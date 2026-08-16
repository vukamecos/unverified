// Package session defines the abstract Pump seam between the TUN
// device and the wire-format [transport.Tunnel].
//
// A Pump is the per-session data path: it owns two goroutines
// (TUN→wire, wire→TUN) and runs them until either side closes,
// the context is cancelled, or an unrecoverable error occurs.
// Per ADR 0005 the pump is the orchestration of the two read
// loops; it does NOT implement inner-AEAD or inner-KEM — those
// are layered on top of the [transport.Tunnel] interface in a
// future iteration.
//
// # Package layout
//
// The concrete implementation lives in `internal/tunnel/session`
// (cross-platform: `pump_linux.go` for Linux, `pump_other.go` for
// every other GOOS). The interface in this package depends only
// on the [contract/tun.Device] and [transport.Tunnel] interfaces,
// never on the iproute2 binary, the kernel TUN driver, or any
// concrete gRPC stack — so the data path is exercisable in tests
// without root and without iproute2.
//
// # Mock generation
//
// Per ARCH §13.1 ("mocks are generated, never hand-written"), the
// test mock for this package's [Pump] interface is produced by
// github.com/matryer/moq and committed next to the contract:
//
//	//go:generate go run github.com/matryer/moq@latest -out pump_mock.go . Pump
//
// Regenerate after any interface change with `go generate ./...`.
// Do not edit pump_mock.go by hand.
package session

import (
	"context"
	"errors"
	"fmt"

	contracttun "github.com/vukamecos/unverified/internal/contract/tun"
	"github.com/vukamecos/unverified/internal/contract/transport"
)

//go:generate go run github.com/matryer/moq@latest -out pump_mock.go . Pump

// Pump is the abstract per-session data path. One Pump = one
// authenticated Tunnel session; the runtime creates a Pump after
// the inner-KEM handshake completes and discards it when the
// stream tears down.
//
// Implementations MUST:
//   - Run both directions (TUN→wire and wire→TUN) concurrently.
//   - Return nil on graceful shutdown (peer closed, ctx cancelled,
//     local Device or Tunnel closed). See [PumpError] for the
//     classification.
//   - Return a non-nil error wrapping a [PumpError] (or the
//     underlying transport/tun error verbatim) on any other
//     failure. The root cause is preserved via [errors.Unwrap].
//
// Implementations MUST NOT:
//   - Touch inner-AEAD or inner-KEM. The Pump sees an already-
//     secured [transport.Tunnel]; encryption is a layer above.
//   - Spawn more than two goroutines per Pump instance. The
//     two-direction rule keeps the data path inspectable and the
//     shutdown symmetry trivial.
type Pump interface {
	// Run starts both pump directions and blocks until one of:
	//   - ctx is cancelled (returns nil),
	//   - the local Device or Tunnel closes (returns nil),
	//   - the peer closes the stream cleanly, surfaced via
	//     transport.Tunnel.RecvFrame returning io.EOF (returns nil),
	//   - any other error occurs (returns a non-nil error wrapping
	//     a PumpError or the underlying transport/tun error).
	//
	// Run is single-shot: calling it twice on the same Pump
	// returns an error. Construction (option wiring, buffer pool
	// sizing) happens in the concrete package's constructor; this
	// interface carries only the runtime entry point.
	Run(ctx context.Context, dev contracttun.Device, tun transport.Tunnel) error
}

// PumpError is the typed error returned by [Pump.Run] when a
// direction-specific operation fails. Mirrors the shape of
// [contract/tun.PreflightError] and [contract/route.LinkError]:
// a stable [Reason] string callers can switch on without parsing
// the message; the underlying cause is wrapped via [errors.Unwrap]
// for [errors.Is] / [errors.As].
//
// Reason constants are part of the public contract; renaming any
// of them is a breaking change. They are pinned by
// TestPumpReason_Stability in pump_test.go.
type PumpError struct {
	// Reason is one of the Reason* constants below.
	Reason string
	// Op is the failing operation in human-readable form
	// ("pump: read tun", "pump: send frame", "pump: recv frame",
	// "pump: write tun"). It appears in Error() output but is
	// not part of the contract — callers switch on Reason, not Op.
	Op string
	// Cause is the underlying error; may be nil for errors that
	// originate in the pump (none today, but kept for parity
	// with the other typed errors in the codebase).
	Cause error
}

// Error renders Op + Reason + Cause as a single human-readable
// string. Format is stable so log scrapers can match on the
// Reason token without parsing free-form text.
func (e *PumpError) Error() string {
	if e.Cause == nil {
		return fmt.Sprintf("pump: %s: %s", e.Op, e.Reason)
	}
	return fmt.Sprintf("pump: %s: %s: %v", e.Op, e.Reason, e.Cause)
}

// Unwrap returns the underlying cause for [errors.Is] /
// [errors.As] / [errors.Unwrap]. Returns nil when Cause is nil.
func (e *PumpError) Unwrap() error {
	return e.Cause
}

// IsPumpError reports whether err is or wraps a *PumpError.
// Useful in error-handling paths that need to branch on the
// Reason without doing a typed unwrap themselves.
func IsPumpError(err error) bool {
	var pe *PumpError
	return errors.As(err, &pe)
}

// PumpReason extracts the stable Reason from err. Returns "" for
// nil or for any error that is not (or does not wrap) a
// *PumpError. Callers that need to switch on the Reason should
// always call PumpReason rather than parsing err.Error().
func PumpReason(err error) string {
	if err == nil {
		return ""
	}
	var pe *PumpError
	if errors.As(err, &pe) {
		return pe.Reason
	}
	return ""
}

// Reason constants — stable, part of the public contract.
//
// The naming mirrors [contract/tun.PreflightError] and
// [contract/route.LinkError] so the caller-facing vocabulary is
// consistent: ReasonUnsupportedPlatform for non-Linux, then
// operation-specific names. Pump has fewer failure modes than
// the link-up or preflight paths (the pump's operations are
// `read tun`, `send frame`, `recv frame`, `write tun` — each
// has one canonical failure reason).
const (
	// ReasonUnsupportedPlatform is returned by every Pump method
	// on a non-Linux GOOS. There is no `ip` binary, no TUN
	// driver, and no kernel data path on the supported
	// platforms.
	ReasonUnsupportedPlatform = "unsupported_platform"

	// ReasonReadTUNFailed is returned when the local
	// [contract/tun.Device] read returns a non-ErrClosed error.
	// The kernel rejected the read (e.g. interface went down
	// mid-pump, ENXIO, EIO). The Cause carries the underlying
	// error from the runtime (typically an *os.PathError or
	// syscall.Errno).
	ReasonReadTUNFailed = "read_tun_failed"

	// ReasonWriteTUNFailed is returned when the local
	// [contract/tun.Device] write returns a non-ErrClosed error.
	// The kernel rejected the write (typically ENXIO if the
	// interface went down, or EIO if the buffer was malformed).
	ReasonWriteTUNFailed = "write_tun_failed"

	// ReasonSendFrameFailed is returned when
	// [transport.Tunnel.SendFrame] returns a non-ErrClosed,
	// non-io.EOF error. The wire-side send failed (TLS shutdown,
	// gRPC stream error, etc.). The Cause carries the underlying
	// transport error.
	ReasonSendFrameFailed = "send_frame_failed"

	// ReasonRecvFrameFailed is returned when
	// [transport.Tunnel.RecvFrame] returns a non-ErrClosed,
	// non-io.EOF error. The wire-side receive failed (decode
	// error, protocol violation, gRPC stream error). The Cause
	// carries the underlying transport error.
	ReasonRecvFrameFailed = "recv_frame_failed"
)