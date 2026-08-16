//go:build linux

// Package route on Linux wraps the iproute2 `ip` command behind the
// abstract [contract/route.Link] interface.
//
// The implementation uses stdlib os/exec with absolute argv — never
// sh -c — so shell-metacharacter injection is structurally impossible
// regardless of input validation. The `ip` binary path is configurable
// (default /sbin/ip) so non-standard installs and the test suite can
// override it without code change.
//
// ADR 0004 selects os/exec over a Go netlink library: iproute2 is
// already a hard runtime dependency per TODO §"Dependencies (Debian
// packages)"; shelling out reuses it instead of pulling
// github.com/vishvananda/netlink for two operations.
//
// Cross-compile to a non-Linux GOOS picks up link_other.go, which
// returns ReasonUnsupportedPlatform from every method so the rest
// of the codebase compiles clean.
package route

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"

	contractroute "github.com/vukamecos/unverified/internal/contract/route"
)

// defaultIPPath is the canonical iproute2 binary path on Debian /
// Ubuntu. Override via the WithIPPath option on New for non-standard
// installs.
const defaultIPPath = "/sbin/ip"

// Executor abstracts the os/exec call so tests can stub the
// `ip` binary's exit code and stderr without forking a real child
// process. Production callers should leave this nil; New() wires
// in execCommandRunner by default.
//
// The contract is intentionally tiny: Run returns the captured
// stdout and a non-nil error on non-zero exit. The error MAY be
// a *exec.ExitError, in which case ExitError.Stderr carries the
// process's diagnostic output; tests may return any error type
// because the production path only inspects the error and (for
// AddAddress's idempotency probe) the stderr string.
type Executor interface {
	Run(name string, args []string) (stdout []byte, err error)
}

// execCommandRunner is the production Executor: it calls
// os/exec.Command with absolute argv (never sh -c) and captures
// stdout + stderr combined so the AddAddress idempotency probe can
// inspect RTNETLINK messages.
type execCommandRunner struct{}

func (execCommandRunner) Run(name string, args []string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		// Wrap ExitError so the original *exec.ExitError is
		// recoverable via errors.As. We do not include the
		// stderr in the wrapped text — LinkError.Error() prints
		// it via %v on the Cause.
		return buf.Bytes(), fmt.Errorf("ip %s: %w", strings.Join(args, " "), err)
	}
	return buf.Bytes(), nil
}

// Options configures a [LinuxLink] at construction. The zero value
// uses /sbin/ip with the real os/exec runner — the production
// default. Tests use WithIPPath / WithExecutor to inject.
type Options struct {
	// IPPath is the absolute path to the iproute2 `ip` binary.
	// Default: /sbin/ip.
	IPPath string
	// Executor runs the binary. Default: real os/exec.
	Executor Executor
}

// WithIPPath returns a copy of opts with IPPath set.
func (opts Options) WithIPPath(p string) Options {
	opts.IPPath = p
	return opts
}

// WithExecutor returns a copy of opts with Executor set.
func (opts Options) WithExecutor(e Executor) Options {
	opts.Executor = e
	return opts
}

// New returns a Linux-backed [contractroute.Link] for the named
// interface. The interface must already exist (typically created
// via [internal/tunnel/tun.Open]); New does not validate its
// presence.
//
// The returned Link is safe to call from a single goroutine; the
// caller is responsible for serialising concurrent calls (the
// tunnel runtime owns one Link per tunnel instance).
func New(name string, opts Options) (contractroute.Link, error) {
	if name == "" {
		return nil, &contractroute.LinkError{
			Reason: contractroute.ReasonCIDRInvalid, // closest stable Reason for "bad name"
			Cause:  errors.New("link: empty interface name"),
		}
	}
	if opts.IPPath == "" {
		opts.IPPath = defaultIPPath
	}
	if opts.Executor == nil {
		opts.Executor = execCommandRunner{}
	}
	return &linuxLink{
		name:     name,
		ipPath:   opts.IPPath,
		executor: opts.Executor,
	}, nil
}

// linuxLink is the production contractroute.Link backed by iproute2.
//
// State (assigned CIDR) is tracked in memory to make idempotency
// cheap; the in-memory record is the source of truth between calls,
// and we probe the kernel via `ip -o addr show` only when the
// in-memory record is empty (e.g. after a restart).
type linuxLink struct {
	name     string
	ipPath   string
	executor Executor
	// assigned is the CIDR currently configured on the link, or
	// "" if AddAddress has not been called (or the link has been
	// re-created externally). Tracked so re-adding the same
	// CIDR is a no-op without a syscall.
	assigned string
}

// Up brings the interface to the UP state. iproute2's
// `ip link set DEV up` is idempotent at the kernel level — calling
// it on an already-up link returns exit 0 — so we do not need a
// pre-probe.
func (l *linuxLink) Up() error {
	_, err := l.executor.Run(l.ipPath, []string{"link", "set", l.name, "up"})
	if err != nil {
		return &contractroute.LinkError{
			Reason: contractroute.ReasonLinkUpFailed,
			Cause:  err,
		}
	}
	return nil
}

// Down brings the interface to the DOWN state. Idempotent for the
// same reason as Up.
func (l *linuxLink) Down() error {
	_, err := l.executor.Run(l.ipPath, []string{"link", "set", l.name, "down"})
	if err != nil {
		return &contractroute.LinkError{
			Reason: contractroute.ReasonLinkDownFailed,
			Cause:  err,
		}
	}
	// Reset the assigned-CIDR cache: the kernel may have cleared
	// it (interface going DOWN preserves addresses on Linux, but
	// a future iter that calls ip addr flush on Down() would
	// need this). Today it is a no-op for correctness; tomorrow
	// it would prevent a stale assignment from masking an
	// AlreadyAssigned error on the next AddAddress.
	l.assigned = ""
	return nil
}

// AddAddress assigns cidr (e.g. "10.66.0.2/24") to the interface.
//
// Idempotency:
//   - If cidr matches the in-memory recorded assignment, return
//     nil without exec.
//   - Otherwise call `ip addr add CIDR dev DEV`. If the kernel
//     rejects with "RTNETLINK answers: File exists" (the standard
//     idempotency / conflict error), probe with `ip -o addr show`
//     to distinguish same-CIDR (no-op) from different-CIDR
//     (ReasonAddressAlreadyAssigned).
//   - Any other failure returns ReasonAddressAddFailed with the
//     underlying ExitError; the probe is NOT run for non-File-exists
//     errors (a permission or syntax failure won't be cured by
//     re-reading the address list).
func (l *linuxLink) AddAddress(cidr string) error {
	// Validate before any exec so a typo never reaches the
	// kernel and produces a non-obvious stderr error.
	if _, _, err := net.ParseCIDR(cidr); err != nil {
		return &contractroute.LinkError{
			Reason: contractroute.ReasonCIDRInvalid,
			Cause:  fmt.Errorf("link: parse CIDR %q: %w", cidr, err),
		}
	}

	// Cheap path: in-memory record matches.
	if l.assigned == cidr {
		return nil
	}

	// Already assigned a different CIDR — no need to exec; it's a
	// definite conflict.
	if l.assigned != "" {
		return &contractroute.LinkError{
			Reason: contractroute.ReasonAddressAlreadyAssigned,
			Cause: fmt.Errorf("link: %s already has CIDR %s; refusing to assign %s",
				l.name, l.assigned, cidr),
		}
	}

	_, runErr := l.executor.Run(l.ipPath, []string{"addr", "add", cidr, "dev", l.name})
	if runErr == nil {
		l.assigned = cidr
		return nil
	}

	// Only the "File exists" error benefits from the probe —
	// it is the kernel's signal for "you tried to add an
	// address that conflicts with one already assigned". Any
	// other failure (Operation not permitted, No such device,
	// etc.) is independent of the current address list, and
	// probing would waste an exec call.
	stderr := runErr.Error()
	if !strings.Contains(stderr, "File exists") {
		return &contractroute.LinkError{
			Reason: contractroute.ReasonAddressAddFailed,
			Cause:  runErr,
		}
	}

	// Probe: which address is on the device? Same CIDR → no-op.
	// Different CIDR → ReasonAddressAlreadyAssigned.
	current, probeErr := l.executor.Run(l.ipPath,
		[]string{"-o", "addr", "show", "dev", l.name})
	if probeErr != nil {
		// Probe failed — surface the original add error; do
		// not mask it with a probe error.
		return &contractroute.LinkError{
			Reason: contractroute.ReasonAddressAddFailed,
			Cause:  runErr,
		}
	}
	if containsCIDR(string(current), cidr) {
		l.assigned = cidr
		return nil
	}
	return &contractroute.LinkError{
		Reason: contractroute.ReasonAddressAlreadyAssigned,
		Cause:  runErr,
	}
}

// containsCIDR reports whether `ip -o addr show dev DEV` output
// contains the literal CIDR string. The one-line format is stable
// since iproute2 3.x:
//
//	2: dummy0    inet 10.99.99.1/24 brd 10.99.99.255 scope global dummy0
//
// The CIDR appears as a token between "inet " and " brd " (for
// IPv4) or "inet6 " and " brd " (for IPv6). The match is a simple
// substring search on the line — false positives are not a
// security concern because the matching prefix+host is unique on
// the line.
func containsCIDR(showOutput, cidr string) bool {
	for _, line := range strings.Split(showOutput, "\n") {
		if strings.Contains(line, " "+cidr+" ") ||
			strings.HasSuffix(strings.TrimSpace(line), " "+cidr) {
			return true
		}
	}
	return false
}

// Compile-time interface check: linuxLink must satisfy
// contractroute.Link. Caught at build time if a method drifts.
var _ contractroute.Link = (*linuxLink)(nil)

// Compile-time interface check: execCommandRunner must satisfy
// Executor.
var _ Executor = execCommandRunner{}
