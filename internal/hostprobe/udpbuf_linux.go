//go:build linux

package hostprobe

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// RecommendedRmemMax is the recommended minimum value for
// net.core.rmem_max when QUIC bulk transfer is in use, per
// ARCH §"QUIC transport" and TODO row 22. The value is
// conservative for typical bulk-payload workloads; the
// runtime either shuts down gracefully or surfaces a
// diagnostic — it never auto-raises the ceiling (that
// requires root per ADR 0006's sentinel-operation model).
//
// 4 MiB matches the Linux kernel default since 5.x and
// the threshold the system call itself notes in
// Documentation/sysctl-net.txt as "reasonable for many
// workloads". A QUIC bulk sender that wants larger
// windows than the default needs this raised.
const RecommendedRmemMax uint64 = 4 << 20 // 4 MiB

// UDPBufReport captures a probe of the kernel UDP receive
// buffer ceiling for IPv4. Read-only — never modifies host
// state.
//
// A zero Val means "could not parse the kernel string";
// see ErrUDPBufUnreadable for the cause. A Val that is
// non-zero but below RecommendedRmemMax is the common
// non-root case (the kernel default on this host is 4 MiB
// which already meets RecommendedRmemMax) and is surfaced
// as a non-fatal flag, not an error.
type UDPBufReport struct {
	// Val is the parsed value of /proc/sys/net/core/rmem_max
	// in bytes. Zero means the probe could not parse the
	// underlying string; the caller should consult Err.
	Val uint64

	// MeetsRecommendation is true iff Val >=
	// RecommendedRmemMax. Surfaced for the runtime
	// preflight and for tests so a regression is
	// visible in the diagnostic log.
	MeetsRecommendation bool
}

// UDPBufRmemMax reads the kernel's net.core.rmem_max and
// returns a report. The probe is read-only — it does NOT
// try to raise the value. Raising the ceiling requires
// CAP_NET_ADMIN (per sysctl(2)) and is intentionally
// outside the scope of this package; the production
// preflight surfaces a fix-up instruction instead, and
// the operator runs:
//
//	sudo sysctl -w net.core.rmem_max=8388608
//
// The path used here is /proc/sys/net/core/rmem_max —
// the canonical procfs view of the sysctl. Reading this
// path is unprivileged.
func UDPBufRmemMax() (*UDPBufReport, error) {
	const path = "/proc/sys/net/core/rmem_max"
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("hostprobe: read %s: %w", path, err)
	}
	raw := strings.TrimSpace(string(data))
	if raw == "" {
		return nil, &UDPBufUnreadableError{
			Path: path,
			Err:  errors.New("empty file"),
		}
	}
	val, parseErr := strconv.ParseUint(raw, 10, 64)
	if parseErr != nil {
		return nil, &UDPBufUnreadableError{
			Path: path,
			Err:  parseErr,
		}
	}
	return &UDPBufReport{
		Val:                val,
		MeetsRecommendation: val >= RecommendedRmemMax,
	}, nil
}

// UDPBufUnreadableError signals that the kernel probe
// could not parse /proc/sys/net/core/rmem_max. Callers
// should treat this as fatal for the host-side check —
// a missing procfs path on a "supported" OS indicates the
// kernel ABI is broken in a way the runtime cannot patch.
type UDPBufUnreadableError struct {
	Path string
	Err  error
}

func (e *UDPBufUnreadableError) Error() string {
	return fmt.Sprintf("hostprobe: cannot parse %s: %v", e.Path, e.Err)
}

// Unwrap supports errors.Is/As against the underlying
// parse error.
func (e *UDPBufUnreadableError) Unwrap() error { return e.Err }
