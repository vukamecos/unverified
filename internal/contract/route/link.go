// Package route defines the abstract Link-control seam.
//
// A Link represents a single OS network interface that the daemon
// has authority to bring up, take down, and configure addresses
// on. Concrete implementations live outside this package (see
// internal/tunnel/route on Linux); the rest of the codebase
// depends on the [Link] interface here so the data path can be
// exercised in tests without root and without iproute2, and so a
// future cross-platform port can drop in a different
// implementation behind the same contract.
//
// ADR 0004 selects the mechanism: this package wraps an os/exec
// shell-out to the `ip` binary (iproute2). The interface here is
// deliberately small — three operations — because that is all the
// kill switch / tunnel bring-up flow needs.
//
// # Mock generation
//
// Per ARCH §13.1 ("mocks are generated, never hand-written"), the
// test mock for this package's [Link] interface is produced by
// github.com/matryer/moq and committed next to the contract:
//
//	//go:generate go run github.com/matryer/moq@latest -out link_mock.go . Link
//
// Regenerate after any interface change with `go generate ./...`.
// Do not edit link_mock.go by hand; the file header carries a
// "regenerate" marker and a CI check (future) will reject drift
// from `go generate`.
package route

import (
	"errors"
	"fmt"
)

//go:generate go run github.com/matryer/moq@latest -out link_mock.go . Link

// Link is a single network interface the daemon can configure.
//
// Implementations are safe for use by a single goroutine; the
// daemon owns one Link per tunnel instance and serialises calls
// itself. Concurrency is not part of the contract.
type Link interface {
	// Up brings the interface to the UP state. Idempotent: calling
	// Up on an already-up interface returns nil and performs no
	// side effect.
	Up() error

	// Down brings the interface to the DOWN state. Idempotent:
	// calling Down on an already-down interface returns nil.
	Down() error

	// AddAddress assigns the given CIDR (e.g. "10.66.0.2/24") to
	// the interface. Idempotent: re-adding the same CIDR is a
	// no-op and returns nil. Adding a *different* CIDR while one
	// is already assigned returns *LinkError with
	// ReasonAddressAlreadyAssigned wrapping the underlying
	// error — the operator must Down() / re-configure explicitly
	// to switch subnets; silent replacement is the kind of "two
	// addresses on the same interface" footgun the contract is
	// designed to prevent.
	AddAddress(cidr string) error
}

// LinkError is the typed error returned by every [Link] method on
// failure. The Reason string is a stable identifier callers can
// switch on without parsing the message; Cause is the underlying
// error from the OS layer (typically an *exec.ExitError or an
// *fs.PathError from the `ip` binary) and is wrapped via
// [errors.Unwrap] / [errors.Is] / [errors.As].
//
// The Reason constants are part of the public contract — renaming
// any of them is a breaking change. They are pinned by the test
// TestLinkReason_Stability in link_test.go.
type LinkError struct {
	// Reason is one of the Reason* constants below.
	Reason string
	// Cause is the underlying error; may be nil for errors that
	// originate in this package (e.g. CIDR parse failure on a
	// syntactically malformed input that the kernel never sees).
	Cause error
}

// Error renders Reason + Cause as a single human-readable string.
// The format is deliberately stable so log scrapers can match on
// the Reason token without parsing free-form text.
func (e *LinkError) Error() string {
	if e.Cause == nil {
		return fmt.Sprintf("link: %s", e.Reason)
	}
	return fmt.Sprintf("link: %s: %v", e.Reason, e.Cause)
}

// Unwrap returns the underlying cause for [errors.Is] /
// [errors.As] / [errors.Unwrap]. Returns nil when Cause is nil.
func (e *LinkError) Unwrap() error {
	return e.Cause
}

// IsLinkError reports whether err is or wraps a *LinkError.
// Useful in error-handling paths that need to branch on the
// Reason without doing a typed unwrap themselves.
func IsLinkError(err error) bool {
	var le *LinkError
	return errors.As(err, &le)
}

// LinkReason extracts the stable Reason from err. Returns "" for
// nil or for any error that is not (or does not wrap) a
// *LinkError. Callers that need to switch on the Reason should
// always call LinkReason rather than parsing err.Error().
func LinkReason(err error) string {
	if err == nil {
		return ""
	}
	var le *LinkError
	if errors.As(err, &le) {
		return le.Reason
	}
	return ""
}

// Reason constants — stable, part of the public contract.
//
// The naming mirrors contracttun.PreflightError so the caller-facing
// vocabulary is consistent: ReasonUnsupportedPlatform, Reason*Missing,
// ReasonProbeFailed, plus operation-specific names where the kernel's
// failure modes differ between operations.
const (
	// ReasonUnsupportedPlatform is returned by every Link method on
	// a non-Linux GOOS. There is no `ip` binary to shell out to
	// and no iproute2-equivalent userspace tool on the supported
	// platforms.
	ReasonUnsupportedPlatform = "unsupported_platform"

	// ReasonBinaryNotFound is returned when the configured `ip`
	// binary path does not exist or is not executable. The most
	// common cause is a stripped image without iproute2 installed
	// (TODO §"Dependencies (Debian packages)" lists iproute2 as a
	// hard dependency, but a misconfigured runtime may still hit
	// this).
	ReasonBinaryNotFound = "ip_binary_not_found"

	// ReasonCIDRInvalid is returned when AddAddress is called
	// with a string that fails [net.ParseCIDR]. We validate before
	// exec so a typo never reaches the kernel and produces a
	// confusing stderr error.
	ReasonCIDRInvalid = "cidr_invalid"

	// ReasonLinkUpFailed is returned by Up when the kernel
	// rejected the state change. The Cause carries the
	// *exec.ExitError and its stderr for diagnosis (typically
	// "RTNETLINK answers: Operation not permitted" when
	// CAP_NET_ADMIN is missing, or "Cannot assign requested
	// address" when the interface name is wrong).
	ReasonLinkUpFailed = "link_up_failed"

	// ReasonLinkDownFailed is returned by Down when the kernel
	// rejected the state change.
	ReasonLinkDownFailed = "link_down_failed"

	// ReasonAddressAlreadyAssigned is returned by AddAddress when
	// the interface already has a different address assigned.
	// Re-adding the *same* CIDR is a no-op (idempotency); only a
	// conflicting CIDR triggers this error.
	ReasonAddressAlreadyAssigned = "address_already_assigned"

	// ReasonAddressAddFailed is returned by AddAddress for any
	// other kernel-side failure (permission denied, address
	// family mismatch, etc.).
	ReasonAddressAddFailed = "address_add_failed"
)
