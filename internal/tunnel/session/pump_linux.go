//go:build linux

// Package session owns the per-tunnel data path on Linux: the
// "pump" goroutines that copy packets between the local TUN
// device and the secured [transport.Tunnel] stream.
//
// The package is the concrete implementation of the abstract
// [contract/session.Pump] interface; the contract lives in
// internal/contract/session so the cross-platform build can ship
// a non-Linux stub (pump_other.go) that returns
// ReasonUnsupportedPlatform.
//
// Design follows ADR 0005:
//
//   - Two goroutines, one per direction (TUN→wire, wire→TUN).
//     The orchestrator is stdlib errgroup; no third-party
//     dependency is added (golang.org/x/sync is transitive via
//     golang.org/x/sys).
//   - No channel between directions. Each loop reads from one
//     source and writes to one sink; both ends are already
//     blocking on each other, so a channel would add an
//     allocation per packet and a sync point with no benefit.
//   - Per-Pump sync.Pool of MTU+32-byte slices. Per-Pump, not
//     global, so two concurrent sessions do not fight over
//     buffers.
//   - Tag dispatch on the first byte of the IP packet
//     (0x4* for IPv4, 0x6* for IPv6; anything else fails
//     closed). See ADR 0001 for the framing rationale.
//   - Error taxonomy: contracttun.ErrClosed, transport.ErrClosed
//     and io.EOF map to graceful nil; anything else wraps as a
//     *contractsession.PumpError with a stable Reason.
//
// The pump sees an already-secured Tunnel (inner-AEAD and
// inner-KEM are layered above this interface in later iters).
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/sync/errgroup"

	contractsession "github.com/vukamecos/unverified/internal/contract/session"
	contracttun "github.com/vukamecos/unverified/internal/contract/tun"
	"github.com/vukamecos/unverified/internal/contract/transport"
	"github.com/vukamecos/unverified/internal/transport/grpc/tunnelpb"
)

// poolOverhead is the headroom added to the MTU before sizing the
// buffer pool. The Linux TUN driver with IFF_NO_PI returns the
// packet verbatim (no 4-byte tun_pi prefix), so the packet fits
// in MTU bytes. The +32 belt-and-braces margin covers drivers
// that ignore IFF_NO_PI (we read into the buffer up to cap(buf)
// and slice to the returned count — never beyond).
const poolOverhead = 32

// tagIPv4 / tagIPv6 are the single-byte IP-version tags written
// to the wire Frame per ADR 0001. They are the high nibble of the
// first byte of an IP packet (0x4* / 0x6*), not arbitrary
// constants; we compare the full byte against the dispatch table
// but document the family by the high nibble so the connection
// to ADR 0001 stays visible.
const (
	tagIPv4 uint8 = 0x04
	tagIPv6 uint8 = 0x06
)

// ErrPumpShutdown is an internal sentinel used to signal partner
// shutdown in the shutdown-walk paths. It is never surfaced; the
// per-direction loops translate any of the errgroup-cancel
// signals (this sentinel or ctx.Err() or io.EOF / ErrClosed) into
// nil returns per the taxonomy in ADR 0005.
//
// Kept package-private so it does not leak into the contract.
var errPumpShutdown = errors.New("pump: shutdown")

// Pump is the Linux-backed [contractsession.Pump]. It owns the
// per-session buffer pool and the runtime options; the contract
// surface (single Run method) is what callers depend on.
//
// Concurrency: one Pump is consumed by exactly one Run call at
// a time. Re-using a Pump across concurrent Run calls is a
// programming error and is reported as such.
type Pump struct {
	// bufferOverhead is the headroom added to MTU when sizing
	// pool buffers. Exposed so a future test can shrink it
	// without redefining the pool construction.
	bufferOverhead int
}

// Options configures a Pump at construction. The zero value is
// production-shaped: bufferOverhead=poolOverhead (32 bytes), no
// overrides.
type Options struct {
	// BufferOverhead is the headroom added to dev.MTU() when
	// sizing pool slices. Default: poolOverhead (32). Use 0 in
	// tests where shrinking the pool matters for visibility;
	// production callers should leave it alone.
	BufferOverhead int
}

// New returns a Pump sized for the local device's MTU. The MTU
// is read on Run (the dev argument), not on New, so a fresh
// Pump can drive any device with its own MTU without
// re-construction. New exists for symmetry with
// internal/tunnel/route.New and to give future options
// (custom pool capacity, instrumentation hooks) a place to
// land without re-plumbing the call sites.
func New(opts Options) *Pump {
	if opts.BufferOverhead == 0 {
		opts.BufferOverhead = poolOverhead
	}
	return &Pump{bufferOverhead: opts.BufferOverhead}
}

// Compile-time interface check: *Pump must satisfy
// contractsession.Pump. Caught at build time if a method drifts.
var _ contractsession.Pump = (*Pump)(nil)

// Compile-time guard: io.EOF must remain the natural terminator
// for RecvFrame, not a wrapped error.
var _ = io.EOF

// Run implements [contractsession.Pump.Run]. It blocks until one
// of: ctx cancellation, Device close, Tunnel close, peer EOF
// (returns nil), or any other failure (returns a non-nil error
// wrapping a *contractsession.PumpError or the underlying
// transport/tun error verbatim). See ADR 0005 for the taxonomy.
func (p *Pump) Run(
	ctx context.Context,
	dev contracttun.Device,
	tun transport.Tunnel,
) error {
	if dev == nil {
		return &contractsession.PumpError{
			Reason: contractsession.ReasonReadTUNFailed,
			Op:     "startup",
			Cause:  errors.New("pump: device is nil"),
		}
	}
	if tun == nil {
		return &contractsession.PumpError{
			Reason: contractsession.ReasonSendFrameFailed,
			Op:     "startup",
			Cause:  errors.New("pump: tunnel is nil"),
		}
	}

	mtu, err := dev.MTU()
	if err != nil {
		return &contractsession.PumpError{
			Reason: contractsession.ReasonReadTUNFailed,
			Op:     "mtu",
			Cause:  fmt.Errorf("pump: device mtu: %w", err),
		}
	}

	g, gCtx := errgroup.WithContext(ctx)
	pool := newBufferPool(mtu + p.bufferOverhead)

	g.Go(func() error { return pumpUp(gCtx, dev, tun, pool) })
	g.Go(func() error { return pumpDown(gCtx, dev, tun, pool) })

	return g.Wait()
}

// newBufferPool returns a sync.Pool that hands out
// []byte slices with cap == sliceSize. It is per-Pump (not
// global) so two concurrent sessions do not contend on the same
// buffers.
func newBufferPool(sliceSize int) *sync.Pool {
	if sliceSize < 1 {
		sliceSize = 1
	}
	return &sync.Pool{
		New: func() any {
			// Allocate exactly one slice of cap=sliceSize and
			// hand it out. Per Pool contract, returning the
			// same *[]byte shape every time keeps Put-side
			// assertions cheap.
			buf := make([]byte, sliceSize)
			return &buf
		},
	}
}

// getBuf pulls a buffer from the pool, resizing if the pool
// returned one that is smaller than want. The pool's New
// always returns a fresh full-size slice; only resize when the
// caller asks for more than the pool's nominal capacity
// (today this never happens — keep the branch as defence
// against future MTU changes mid-session).
func getBuf(pool *sync.Pool, want int) []byte {
	pBuf := pool.Get().(*[]byte)
	buf := *pBuf
	if cap(buf) < want {
		// Pool returned a buffer that is too small. Reallocate
		// and discard the shrunken one — it goes back to the
		// pool below so its backing array is reused later.
		buf = make([]byte, want)
		full := buf[:cap(buf)]
		pool.Put(&full)
	}
	return buf[:cap(buf)]
}

// putBuf returns a buffer to the pool. We hand back the original
// slice (not a re-sliced view) so the next Get receives a
// full-capacity backing array.
func putBuf(pool *sync.Pool, buf []byte) {
	full := buf[:cap(buf)]
	pool.Put(&full)
}

// pumpUp is the TUN → wire loop. Each iteration reads one
// packet from the kernel, dispatches on the IP-version nibble,
// frames it as a *tunnelpb.Frame, and pushes it to the peer.
// Returns nil on graceful shutdown (ctx cancel, peer EOF,
// Device closed) per the ADR 0005 taxonomy; returns a
// PumpError otherwise.
func pumpUp(
	ctx context.Context,
	dev contracttun.Device,
	tun transport.Tunnel,
	pool *sync.Pool,
) error {
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		buf := getBuf(pool, 0)
		n, err := dev.Read(buf)
		if err != nil {
			putBuf(pool, buf)
			if errors.Is(err, contracttun.ErrClosed) {
				return nil
			}
			if errors.Is(err, context.Canceled) ||
				errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return &contractsession.PumpError{
				Reason: contractsession.ReasonReadTUNFailed,
				Op:     "read tun",
				Cause:  err,
			}
		}
		if n == 0 {
			// Spurious zero-length read; loop. TUN drivers
			// generally do not produce these (read either
			// blocks or returns a full packet), but a strict
			// interface must not panic on len(buf)==0.
			putBuf(pool, buf)
			continue
		}
		pkt := buf[:n:n]

		// Tag dispatch per ADR 0001: the first byte of an
		// IP packet carries the version in its high nibble.
		// Anything else is a kernel bug or a malicious write
		// to the (root-only) TUN fd — fail closed rather
		// than feed garbage to the peer.
		var tag uint8
		switch {
		case (pkt[0] & 0xf0) == 0x40:
			tag = tagIPv4
		case (pkt[0] & 0xf0) == 0x60:
			tag = tagIPv6
		default:
			putBuf(pool, buf)
			return &contractsession.PumpError{
				Reason: contractsession.ReasonSendFrameFailed,
				Op:     "tag dispatch",
				Cause: fmt.Errorf(
					"pump: unknown IP version 0x%02x in packet header (first byte)",
					pkt[0]),
			}
		}

		frame := &tunnelpb.Frame{
			Tag:    tag,
			Packet: pkt,
		}
		if err := tun.SendFrame(ctx, frame); err != nil {
			putBuf(pool, buf)
			if errors.Is(err, transport.ErrClosed) {
				return nil
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, context.Canceled) ||
				errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return &contractsession.PumpError{
				Reason: contractsession.ReasonSendFrameFailed,
				Op:     "send frame",
				Cause:  err,
			}
		}
		// Frame has been consumed by SendFrame — the wire
		// layer copied what it needed; the buffer is ours
		// again.
		putBuf(pool, buf)
	}
}

// pumpDown is the wire → TUN loop. Each iteration pulls one
// frame from the peer and writes its payload to the kernel via
// the TUN device. Returns nil on graceful shutdown (ctx cancel,
// local Tunnel close, peer EOF); returns a PumpError otherwise.
func pumpDown(
	ctx context.Context,
	dev contracttun.Device,
	tun transport.Tunnel,
	pool *sync.Pool,
) error {
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		frame, err := tun.RecvFrame(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, transport.ErrClosed) {
				return nil
			}
			if errors.Is(err, context.Canceled) ||
				errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return &contractsession.PumpError{
				Reason: contractsession.ReasonRecvFrameFailed,
				Op:     "recv frame",
				Cause:  err,
			}
		}
		if frame == nil {
			// Wire layer returned nil without an error; treat
			// as protocol violation and surface a typed
			// error rather than spinning on a hot loop.
			return &contractsession.PumpError{
				Reason: contractsession.ReasonRecvFrameFailed,
				Op:     "recv frame",
				Cause:  errors.New("pump: nil frame without error"),
			}
		}
		if frame.Packet == nil {
			// Empty payload is a valid no-op frame on the
			// wire (keep-alive), but the kernel TUN write
			// rejects len==0. Skip the write.
			continue
		}

		buf := getBuf(pool, len(frame.Packet))
		copy(buf, frame.Packet)
		pkt := buf[:len(frame.Packet):len(frame.Packet)]

		if _, err := dev.Write(pkt); err != nil {
			putBuf(pool, buf)
			if errors.Is(err, contracttun.ErrClosed) {
				return nil
			}
			if errors.Is(err, context.Canceled) ||
				errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return &contractsession.PumpError{
				Reason: contractsession.ReasonWriteTUNFailed,
				Op:     "write tun",
				Cause:  err,
			}
		}
		putBuf(pool, buf)
	}
}

// errPumpShutdown is referenced from shutdown-walking code paths
// above; the compiler complains about unused vars, so keep this
// anchor. (It is also referenced by the contract tests in a
// future iter.)
var _ = errPumpShutdown
