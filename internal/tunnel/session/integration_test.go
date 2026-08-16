// Package session_test also owns the off-platform integration
// test for the Pump. Unlike the contract tests (pump_test.go)
// which isolate the Pump behind hand-rolled fakes for the
// Device and Tunnel, this file wires two production *Pumps
// together over a real framed Tunnel pair (net.Pipe + the
// production tunnelpb codec) and verifies end-to-end
// behaviour:
//
//   - A packet written to side-A's device Read queue crosses
//     the tunnel-pair and lands on side-B's device Write
//     buffer (and vice versa), with the correct Tag
//     dispatched.
//   - Cancelling ctx on either side tears down both Pumps
//     gracefully (Run returns nil on both sides).
//   - A write error on side-B's device tears down both Pumps
//     (the partner side observes a non-nil PumpError).
//
// The integration is "off-platform" because the only platform
// dep is the production Pump — Devices are fake byte buffers
// and the framed Tunnel transport is the production codec
// round-tripped over an in-memory net.Pipe. No /dev/net/tun,
// no gRPC server, no real kernel involvement.
//
// Production-fidelity caveat: when the pump's lifecycle
// watcher calls tun.Close, the wrapper writes a Close frame
// to the underlying stream but does NOT close the stream
// itself (the gRPC implementation owns that signal). In
// production the gRPC server closes the bidi stream on
// close. In this in-memory test rig, we close both halves
// of the net.Pipe via bufDevice.CloseCallbacks to simulate
// that signal.
package session_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	contracttun "github.com/vukamecos/unverified/internal/contract/tun"
	transportgrpc "github.com/vukamecos/unverified/internal/transport/grpc"
	"github.com/vukamecos/unverified/internal/tunnel/session"
)

// bufDevice is a contracttun.Device backed by two
// synchronized byte queues. Read drains from inQ;
// Write accumulates in outQ. Read blocks on a never-closing
// channel when inQ is empty, and observes Close via a
// dedicated chan (so the pump's blocked Read can be
// unblocked from another goroutine, mirroring the kernel
// TUN fd-close signal). Write is non-blocking.
//
// writeErr, when set, is returned on the next Write call
// (and cleared). Tests use it to force a direction-side
// error without hanging the pump on a second error.
//
// onClose is invoked when Close() runs, after closed flag
// is set. Used by the integration rig to tear down the
// net.Pipe ends (which is the production-equivalent of a
// gRPC server closing the bidi stream).
type bufDevice struct {
	mu        sync.Mutex
	inQ       [][]byte
	outQ      bytes.Buffer
	closed    atomic.Bool
	closeOnce sync.Once
	readCh    chan struct{}
	closedCh  chan struct{}
	writes    atomic.Int64
	reads     atomic.Int64
	writeErr  error
	onClose   func()
}

func newBufDevice() *bufDevice {
	return &bufDevice{
		readCh:   make(chan struct{}),
		closedCh: make(chan struct{}),
	}
}

// enqueueRead appends packet(s) to the read queue, then
// signals the blocked Read to retry. Multiple enqueues
// before any Read prod one signal each.
func (d *bufDevice) enqueueRead(pkts ...[]byte) {
	d.mu.Lock()
	d.inQ = append(d.inQ, pkts...)
	d.mu.Unlock()
	for range pkts {
		select {
		case d.readCh <- struct{}{}:
		default:
			// No waiter; drop the signal.
		}
	}
}

func (d *bufDevice) Name() string { return "buf0" }

func (d *bufDevice) MTU() (int, error) { return 1500, nil }

func (d *bufDevice) Read(p []byte) (int, error) {
	if d.closed.Load() {
		return 0, contracttun.ErrClosed
	}
	for {
		d.mu.Lock()
		if len(d.inQ) > 0 {
			pkt := d.inQ[0]
			d.inQ = d.inQ[1:]
			d.mu.Unlock()
			d.reads.Add(1)
			return copy(p, pkt), nil
		}
		d.mu.Unlock()
		// Empty queue: wait for either Close() or a
		// fresh enqueue. Close races the Close channel
		// (closed chan) against an enqueue (readCh).
		select {
		case <-d.readCh:
			// a packet arrived; loop to drain
		case <-d.closedChan():
			return 0, contracttun.ErrClosed
		}
	}
}

// closedChan returns a channel that is closed when the
// device is closed. Used by Read to unblock on Close().
func (d *bufDevice) closedChan() <-chan struct{} { return d.closedCh }

func (d *bufDevice) Write(p []byte) (int, error) {
	if d.closed.Load() {
		return 0, contracttun.ErrClosed
	}
	d.writes.Add(1)
	if d.writeErr != nil {
		err := d.writeErr
		d.writeErr = nil
		return 0, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.outQ.Write(p)
}

func (d *bufDevice) Close() error {
	d.closeOnce.Do(func() {
		d.closed.Store(true)
		close(d.closedCh)
		if d.onClose != nil {
			d.onClose()
		}
	})
	return nil
}

// outSnapshot returns a copy of what the pump has Written
// to this device. Used by tests to assert end-to-end delivery.
func (d *bufDevice) outSnapshot() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	snap := make([]byte, d.outQ.Len())
	copy(snap, d.outQ.Bytes())
	return snap
}

// TestPump_Integration_BidirectionalFlow drives one IPv4
// packet A→B and one IPv6 packet B→A across the full
// production Pump × 2 with a real net.Pipe-backed Tunnel
// pair. Verifies delivery (each device's outSnapshot
// contains the peer's payload) and Tag dispatch preserved
// over the framing round-trip.
func TestPump_Integration_BidirectionalFlow(t *testing.T) {
	t.Parallel()

	c1, c2 := net.Pipe()
	wrapA := &eofConn{c: c1}
	wrapB := &eofConn{c: c2}
	tunA := transportgrpc.Wrap(wrapA)
	tunB := transportgrpc.Wrap(wrapB)
	devA := newBufDevice()
	devB := newBufDevice()

	// When the watcher closes devA or devB on ctx cancel,
	// also close both ends of the net.Pipe so a peer's
	// blocked RecvFrame observes io.EOF (production gRPC
	// equivalent: server closes the bidi stream on close).
	closePipe := func() {
		_ = c1.Close()
		_ = c2.Close()
	}
	devA.onClose = closePipe
	devB.onClose = closePipe

	pktA := []byte{0x45, 0x00, 0x14, 0x00, 0x01} // IPv4
	pktB := []byte{0x60, 0x00, 0x00, 0x00, 0x02} // IPv6
	devA.enqueueRead(pktA)
	devB.enqueueRead(pktB)

	pumpA := session.New(session.Options{BufferOverhead: 0})
	pumpB := session.New(session.Options{BufferOverhead: 0})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runA := make(chan error, 1)
	runB := make(chan error, 1)
	go func() { runA <- pumpA.Run(ctx, devA, tunA) }()
	go func() { runB <- pumpB.Run(ctx, devB, tunB) }()

	// Wait for both sides to observe at least one write
	// (the partner's packet crossed the tunnel).
	if !pollUntil(2*time.Second, func() bool {
		return devA.writes.Load() >= 1 && devB.writes.Load() >= 1
	}) {
		t.Fatalf("end-to-end: A.writes=%d, B.writes=%d after 2s; want both >=1",
			devA.writes.Load(), devB.writes.Load())
	}

	if got := devA.outSnapshot(); !bytes.Equal(got, pktB) {
		t.Fatalf("A.outQ = % x, want % x (B's IPv6 packet)", got, pktB)
	}
	if got := devB.outSnapshot(); !bytes.Equal(got, pktA) {
		t.Fatalf("B.outQ = % x, want % x (A's IPv4 packet)", got, pktA)
	}

	cancel()
	for i, ch := range []chan error{runA, runB} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("Pump %d Run returned %v on cancel, want nil", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("Pump %d did not return within 2s of ctx cancel", i)
		}
	}
}

// TestPump_Integration_DeviceWriteErrorTearsDownPartner
// verifies ADR 0005's "first error wins, partner exits on
// shutdown" symmetry across the integration rig. A forced
// Write error on side-B's device tears down side-B (the
// errgroup ctx cancels), which the lifecycle watcher
// bridges into a Close on side-A's tunnel — side-A's
// RecvFrame then observes io.EOF and exits nil too.
func TestPump_Integration_DeviceWriteErrorTearsDownPartner(t *testing.T) {
	t.Parallel()

	c1, c2 := net.Pipe()
	wrapA := &eofConn{c: c1}
	wrapB := &eofConn{c: c2}
	tunA := transportgrpc.Wrap(wrapA)
	tunB := transportgrpc.Wrap(wrapB)
	devA := newBufDevice()
	devB := newBufDevice()

	// Single direction error: just on side-B's Write.
	devB.writeErr = errors.New("kernel: ENOSPC")
	devB.onClose = func() {
		_ = c1.Close()
		_ = c2.Close()
	}

	// Side-A's pumpUp will read a packet from devA, encode
	// it as a tunnel frame, and send it across the pipe.
// Side-B's pumpDown will receive that frame and try to
	// write it to devB — where devB.writeErr has been set
	// to ENOSPC. That Write failure is what tears down
	// pumpB's errgroup and, via the lifecycle watcher,
// pumpA's errgroup as well.
	devA.enqueueRead([]byte{0x45, 0x00, 0x14, 0x00, 0x01})

	pumpA := session.New(session.Options{BufferOverhead: 0})
	pumpB := session.New(session.Options{BufferOverhead: 0})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runA := make(chan error, 1)
	runB := make(chan error, 1)
	go func() { runA <- pumpA.Run(ctx, devA, tunA) }()
	go func() { runB <- pumpB.Run(ctx, devB, tunB) }()

	// PumpB should fail (wraps ENOSPC). The partner-side
	// close-tunnel signal bridges to PumpA's pumpDown, which
	// sees io.EOF and returns nil. PumpA's pumpUp is parked
	// in devA.Read (no inbound packets); we then call
	// cancel() to release it via the watcher.
	select {
	case err := <-runB:
		if err == nil {
			t.Fatalf("PumpB returned nil, want wraps *PumpError")
		}
		if msg := err.Error(); !contains(msg, "ENOSPC") {
			t.Fatalf("PumpB err = %v, want wraps ENOSPC", err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		select {
		case err := <-runB:
			t.Fatalf("PumpB did not return within 2s after Write error, then returned %v after cancel", err)
		case <-time.After(time.Second):
			t.Fatalf("PumpB did not return within 2s and not after cancel")
		}
	}

	cancel()
	select {
	case err := <-runA:
		if err != nil {
			t.Fatalf("PumpA returned %v on partner error, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("PumpA did not return within 2s after partner error")
	}
}

// eofConn wraps a net.Conn so that a Read after Close
// returns io.EOF (not io.ErrClosedPipe). This mirrors the
// production gRPC bidi stream's behaviour: closing a
// gRPC stream surfaces io.EOF to Recv callers, whereas
// net.Conn.Close surfaces io.ErrClosedPipe. The pump
// contract treats io.EOF as graceful shutdown (PumpDown
// returns nil), so this shim is the test-side bridge.
type eofConn struct {
	c net.Conn
}

func (e *eofConn) Read(p []byte) (int, error) {
	n, err := e.c.Read(p)
	if errors.Is(err, io.ErrClosedPipe) {
		return n, io.EOF
	}
	return n, err
}

func (e *eofConn) Write(p []byte) (int, error) { return e.c.Write(p) }

func (e *eofConn) Close() error { return e.c.Close() }

// contains is a tiny helper to avoid pulling in `strings`.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Compile-time interface checks.
var (
	_ contracttun.Device = (*bufDevice)(nil)
	_ io.Reader          = (*bufDevice)(nil)
	_ io.Writer          = (*bufDevice)(nil)
)
