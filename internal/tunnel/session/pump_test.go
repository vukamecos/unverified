// Package session_test exercises the per-session data path Pump
// against the ADR 0005 error taxonomy. Tests are off-platform:
// they drive the production *Pump through every branch of the
// taxonomy using a hand-rolled [fakeDevice] (a moq-generated
// mock would be larger than the test logic). Each test cancels
// the ctx as the shutdown trigger; the pump observes ctx.Err()
// and returns nil. The one failure-path test arranges the
// partner side to error first.
package session_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	contractsession "github.com/vukamecos/unverified/internal/contract/session"
	contracttun "github.com/vukamecos/unverified/internal/contract/tun"
	"github.com/vukamecos/unverified/internal/contract/transport"
	"github.com/vukamecos/unverified/internal/transport/grpc/tunnelpb"
	"github.com/vukamecos/unverified/internal/tunnel/session"
)

// fakeDevice is a hand-rolled tun.Device. Read/Write block on
// the corresponding channel until it is closed (or the device
// is Closed). Test workflow:
//
//  1. Configure readN/readPayload/readErr and start the pump.
//  2. releaseRead() OR Close() to let the pump proceed.
//
// Each Read consumes one "release" from readBlock; after the
// configured number of reads, Read returns (0, contracttun.ErrClosed)
// so subsequent loops in the pump exit cleanly. To make the
// pump loop indefinitely (e.g. for the multi-iteration pool
// test), set infiniteReads=true and release once: every Read
// after that returns the configured payload.
//
// The interface is small (5 methods) so hand-rolling it is
// cheaper than wiring moq for this test set; the future moq
// rule targets larger interfaces. See
// internal/contract/tun/tun_test.go for the same pattern.
type fakeDevice struct {
	mu        sync.Mutex
	mtu       int
	mtuErr    error
	closed    atomic.Bool
	closeOnce sync.Once

	readBlock    chan struct{}
	readN        int
	readPayload  []byte
	readErr      error
	infiniteReads bool
	exhausted     atomic.Bool
	reads         atomic.Int64

	writeBlock chan struct{}
	writeN     int
	writeErr   error
	writes     atomic.Int64
	lastWrite  []byte
}

func newFakeDevice(mtu int) *fakeDevice {
	return &fakeDevice{
		mtu:        mtu,
		readBlock:  make(chan struct{}),
		writeBlock: make(chan struct{}),
	}
}

func (f *fakeDevice) Name() string { return "fake0" }

func (f *fakeDevice) MTU() (int, error) {
	if f.closed.Load() {
		return 0, contracttun.ErrClosed
	}
	if f.mtuErr != nil {
		return 0, f.mtuErr
	}
	return f.mtu, nil
}

func (f *fakeDevice) Read(p []byte) (int, error) {
	if f.closed.Load() {
		return 0, contracttun.ErrClosed
	}
	f.reads.Add(1)
	f.mu.Lock()
	block := f.readBlock
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	if f.readErr != nil {
		return 0, f.readErr
	}
	if f.infiniteReads {
		// Loop forever returning the configured payload.
		if len(f.readPayload) > 0 {
			n := copy(p, f.readPayload)
			return n, nil
		}
		n := f.readN
		if n > len(p) {
			n = len(p)
		}
		for i := 0; i < n; i++ {
			p[i] = 0xAA
		}
		return n, nil
	}
	if f.exhausted.Swap(true) {
		// Signal the per-pump lifetime: after the first read,
		// subsequent reads return ErrClosed so the pump can
		// observe dev close and exit.
		return 0, contracttun.ErrClosed
	}
	if len(f.readPayload) > 0 {
		n := copy(p, f.readPayload)
		return n, nil
	}
	n := f.readN
	if n > len(p) {
		n = len(p)
	}
	for i := 0; i < n; i++ {
		p[i] = 0xAA
	}
	return n, nil
}

func (f *fakeDevice) Write(p []byte) (int, error) {
	if f.closed.Load() {
		return 0, contracttun.ErrClosed
	}
	f.writes.Add(1)
	f.mu.Lock()
	block := f.writeBlock
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	f.mu.Lock()
	f.lastWrite = append([]byte(nil), p...)
	f.mu.Unlock()
	n := f.writeN
	if n <= 0 || n > len(p) {
		n = len(p)
	}
	return n, nil
}

func (f *fakeDevice) Close() error {
	f.closeOnce.Do(func() { f.closed.Store(true) })
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readBlock != nil {
		select {
		case <-f.readBlock:
		default:
			close(f.readBlock)
		}
	}
	if f.writeBlock != nil {
		select {
		case <-f.writeBlock:
		default:
			close(f.writeBlock)
		}
	}
	return nil
}

// releaseRead unblocks any pending Read. Idempotent: a second
// call after the channel is already closed is a no-op.
//
// NOTE: the channel reference is kept (set to a never-closing
// channel after first close, to keep the field readable but
// uninstrumented). See the constructor.
func (f *fakeDevice) releaseRead() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readBlock != nil {
		select {
		case <-f.readBlock:
		default:
			close(f.readBlock)
		}
	}
}

func (f *fakeDevice) releaseWrite() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeBlock != nil {
		select {
		case <-f.writeBlock:
		default:
			close(f.writeBlock)
		}
	}
}

// fakeTunnel is a hand-rolled transport.Tunnel. RecvFrame blocks
// on recvBlock (or returns recvErr / io.EOF when configured).
// SendFrame either succeeds, sends the configured number of
// payloads and then errors, or returns sendErr on the first
// call. Close is idempotent.
type fakeTunnel struct {
	mu           sync.Mutex
	closeOnce    sync.Once
	closed       atomic.Bool
	recvBlock    chan struct{}
	recvFrame    *tunnelpb.Frame
	recvErr      error
	recvCallback func() (*tunnelpb.Frame, error)
	sendErr      error
	sendConsumed atomic.Int64
	sendFramesMu sync.Mutex
	sentFrames   []*tunnelpb.Frame
}

func newFakeTunnel() *fakeTunnel {
	return &fakeTunnel{recvBlock: make(chan struct{})}
}

func (f *fakeTunnel) SendFrame(_ context.Context, fr *tunnelpb.Frame) error {
	if f.closed.Load() {
		return transport.ErrClosed
	}
	f.sendConsumed.Add(1)
	f.sendFramesMu.Lock()
	f.sentFrames = append(f.sentFrames, fr)
	f.sendFramesMu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	return nil
}

func (f *fakeTunnel) RecvFrame(_ context.Context) (*tunnelpb.Frame, error) {
	if f.closed.Load() {
		return nil, transport.ErrClosed
	}
	if f.recvCallback != nil {
		return f.recvCallback()
	}
	f.mu.Lock()
	block := f.recvBlock
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	if f.recvErr != nil {
		return nil, f.recvErr
	}
	if f.recvFrame != nil {
		return f.recvFrame, nil
	}
	return nil, io.EOF
}

func (f *fakeTunnel) Close(_ uint32, _ string) error {
	f.closeOnce.Do(func() {
		f.closed.Store(true)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.recvBlock != nil {
			select {
			case <-f.recvBlock:
			default:
				close(f.recvBlock)
			}
		}
	})
	return nil
}

func (f *fakeTunnel) releaseRecv() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recvBlock != nil {
		select {
		case <-f.recvBlock:
		default:
			close(f.recvBlock)
		}
	}
}

// ipv4Packet returns a 1-byte payload that the pump's tag
// dispatch classifies as IPv4 (high nibble 0x4).
func ipv4Packet() []byte { return []byte{0x45} }

// ipv6Packet returns a 1-byte payload classified as IPv6 (high
// nibble 0x6).
func ipv6Packet() []byte { return []byte{0x60} }

// pollUntil calls cond every 1ms until it returns true or
// timeout elapses. Used to wait for pump goroutines (which run
// asynchronously) to observe the configured test state.
func pollUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

func shutdownOnCtx(ctx context.Context, dev *fakeDevice, tun *fakeTunnel) func() {
	cancelHolder := func() {}
	ctx, cancelHolder = context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		if dev != nil {
			dev.releaseRead()
			dev.releaseWrite()
		}
		if tun != nil {
			tun.releaseRecv()
		}
		close(done)
	}()
	return func() {
		cancelHolder()
		<-done
	}
}

// runUntilCancel starts a pump with the given device and tunnel,
// cancels the ctx after a short grace period, and returns the
// final error from Run. The grace period is configurable via
// grace; tests with partner-side errors arrange the partner to
// fail before grace expires.
func runUntilCancel(
	t *testing.T,
	dev *fakeDevice,
	tun *fakeTunnel,
	grace time.Duration,
) error {
	t.Helper()
	p := session.New(session.Options{BufferOverhead: 0})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitShutdown := shutdownOnCtx(ctx, dev, tun)
	done := make(chan error, 1)
	go func() {
		done <- p.Run(ctx, dev, tun)
	}()
	time.Sleep(grace)
	cancel()
	waitShutdown()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatalf("pump did not return within 2s of ctx cancel")
		return nil
	}
}

// TestPump_Run_NilDevice wraps nil-device startup-time validation.
// Run returns ReasonReadTUNFailed because the input-validation
// branch for "dev == nil" maps to that reason (the pump never
// reaches a read).
func TestPump_Run_NilDevice(t *testing.T) {
	t.Parallel()
	p := session.New(session.Options{})
	err := p.Run(context.Background(), nil, newFakeTunnel())
	if err == nil {
		t.Fatalf("Run(nil dev) = nil, want *PumpError")
	}
	if !contractsession.IsPumpError(err) {
		t.Fatalf("Run(nil dev) = %v, want wraps *PumpError", err)
	}
	if got := contractsession.PumpReason(err); got != contractsession.ReasonReadTUNFailed {
		t.Fatalf("PumpReason = %q, want %q", got, contractsession.ReasonReadTUNFailed)
	}
}

// TestPump_Run_NilTunnel mirrors TestPump_Run_NilDevice for the
// tunnel side: Run validates both inputs before starting any
// goroutines and surfaces ReasonSendFrameFailed for nil tunnel.
func TestPump_Run_NilTunnel(t *testing.T) {
	t.Parallel()
	p := session.New(session.Options{})
	err := p.Run(context.Background(), newFakeDevice(1500), nil)
	if err == nil {
		t.Fatalf("Run(nil tun) = nil, want *PumpError")
	}
	if !contractsession.IsPumpError(err) {
		t.Fatalf("Run(nil tun) = %v, want wraps *PumpError", err)
	}
	if got := contractsession.PumpReason(err); got != contractsession.ReasonSendFrameFailed {
		t.Fatalf("PumpReason = %q, want %q", got, contractsession.ReasonSendFrameFailed)
	}
}

// TestPump_Run_DevMTUError verifies Run propagates a Device.MTU
// error as a *PumpError with ReasonReadTUNFailed and the
// underlying cause preserved through Unwrap.
func TestPump_Run_DevMTUError(t *testing.T) {
	t.Parallel()
	boom := errors.New("mtu probe: ENETDOWN")
	dev := newFakeDevice(0)
	dev.mtuErr = boom
	p := session.New(session.Options{})
	err := p.Run(context.Background(), dev, newFakeTunnel())
	if err == nil {
		t.Fatalf("Run = nil, want *PumpError wrapping %v", boom)
	}
	if !contractsession.IsPumpError(err) {
		t.Fatalf("Run = %v, want wraps *PumpError", err)
	}
	if got := contractsession.PumpReason(err); got != contractsession.ReasonReadTUNFailed {
		t.Fatalf("PumpReason = %q, want %q", got, contractsession.ReasonReadTUNFailed)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wraps %v", err, boom)
	}
}

// TestPump_Run_GracefulCtxCancel verifies the central contract:
// on ctx cancel the pump returns nil (clean shutdown), not a
// *PumpError. Both directions block on their Read/RecvFrame and
// observe ctx cancellation.
func TestPump_Run_GracefulCtxCancel(t *testing.T) {
	t.Parallel()
	dev := newFakeDevice(1500)
	tun := newFakeTunnel()
	err := runUntilCancel(t, dev, tun, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("Run returned %v on ctx cancel, want nil", err)
	}
}

// TestPump_Run_DeviceClosed_Nil verifies that when Device.Close
// happens during operation, the read direction observes
// contracttun.ErrClosed and returns nil (graceful shutdown).
// The recveive side is unblocked via the ctx cancel and also
// returns nil. Run surfaces nil overall.
func TestPump_Run_DeviceClosed_Nil(t *testing.T) {
	t.Parallel()
	dev := newFakeDevice(1500)
	tun := newFakeTunnel()
	p := session.New(session.Options{BufferOverhead: 0})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	waitShutdown := shutdownOnCtx(ctx, dev, tun)
	defer waitShutdown()
	go func() { done <- p.Run(ctx, dev, tun) }()

	// Give the pump a moment to enter the read loop.
	time.Sleep(20 * time.Millisecond)

	// Close the device while Read/RecvFrame are blocked.
	if err := dev.Close(); err != nil {
		t.Fatalf("dev.Close = %v", err)
	}
	// Drop the cancel too — releases RecvFrame so the partner
	// can exit; Run has already produced its error by then.
	cancel()
	waitShutdown()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v after Device.Close, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return within 2s after Device.Close")
	}

	// The Read must have been observed at least once.
	if got := dev.reads.Load(); got < 1 {
		t.Fatalf("Device.Read invocations = %d, want >= 1", got)
	}
}

// TestPump_Run_DeviceReadError verifies that a non-ErrClosed,
// non-context error from Device.Read is wrapped as ReasonReadTUNFailed.
// The pump does NOT recover from a read error; the partner
// goroutine receives a ctx cancel and exits nil.
func TestPump_Run_DeviceReadError(t *testing.T) {
	t.Parallel()
	dev := newFakeDevice(1500)
	tun := newFakeTunnel()
	boom := errors.New("kernel: EIO")
	dev.readErr = boom

	p := session.New(session.Options{BufferOverhead: 0})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	waitShutdown := shutdownOnCtx(ctx, dev, tun)
	defer waitShutdown()
	go func() { done <- p.Run(ctx, dev, tun) }()

	// Release the Read side so it observes readErr.
	time.Sleep(20 * time.Millisecond)
	dev.releaseRead()
	waitShutdown()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("Run returned nil, want wraps *PumpError")
		}
		if !contractsession.IsPumpError(err) {
			t.Fatalf("Run returned %v, want wraps *PumpError", err)
		}
		if got := contractsession.PumpReason(err); got != contractsession.ReasonReadTUNFailed {
			t.Fatalf("PumpReason = %q, want %q", got, contractsession.ReasonReadTUNFailed)
		}
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want wraps %v", err, boom)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return within 2s after Read error")
	}
}

// TestPump_Run_PeerEOF_Nil verifies the wire→TUN direction
// returns nil when RecvFrame returns io.EOF (clean peer
// shutdown per ADR 0005). The TUN→wire direction is unblocked
// by ctx cancel and also returns nil; Run surfaces nil overall.
func TestPump_Run_PeerEOF_Nil(t *testing.T) {
	t.Parallel()
	dev := newFakeDevice(1500)
	tun := newFakeTunnel()
	tun.recvErr = io.EOF

	p := session.New(session.Options{BufferOverhead: 0})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	waitShutdown := shutdownOnCtx(ctx, dev, tun)
	defer waitShutdown()
	go func() { done <- p.Run(ctx, dev, tun) }()

	// Release the RecvFrame side so it observes io.EOF.
	time.Sleep(20 * time.Millisecond)
	tun.releaseRecv()
	waitShutdown()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on peer EOF, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return within 2s after peer EOF")
	}
}

// TestPump_Run_RecvError verifies that a non-ErrClosed,
// non-io.EOF error from RecvFrame is wrapped as
// ReasonRecvFrameFailed.
func TestPump_Run_RecvError(t *testing.T) {
	t.Parallel()
	dev := newFakeDevice(1500)
	tun := newFakeTunnel()
	boom := errors.New("decode: bad tag")
	tun.recvErr = boom

	p := session.New(session.Options{BufferOverhead: 0})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	waitShutdown := shutdownOnCtx(ctx, dev, tun)
	defer waitShutdown()
	go func() { done <- p.Run(ctx, dev, tun) }()

	time.Sleep(20 * time.Millisecond)
	tun.releaseRecv()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("Run returned nil, want wraps *PumpError")
		}
		if !contractsession.IsPumpError(err) {
			t.Fatalf("Run returned %v, want wraps *PumpError", err)
		}
		if got := contractsession.PumpReason(err); got != contractsession.ReasonRecvFrameFailed {
			t.Fatalf("PumpReason = %q, want %q", got, contractsession.ReasonRecvFrameFailed)
		}
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want wraps %v", err, boom)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return within 2s after Recv error")
	}
}

// TestPump_Run_SendFrameError verifies a SendFrame failure
// surfaces as ReasonSendFrameFailed. The pump observes it after
// a successful Read (the partner goroutine), then Run returns
// the error from g.Wait().
func TestPump_Run_SendFrameError(t *testing.T) {
	t.Parallel()
	dev := newFakeDevice(1500)
	tun := newFakeTunnel()
	boom := errors.New("tls: write half closed")
	tun.sendErr = boom

	dev.readN = 1
	dev.readPayload = ipv4Packet() // 0x45 — must dispatch as IPv4

	p := session.New(session.Options{BufferOverhead: 0})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	waitShutdown := shutdownOnCtx(ctx, dev, tun)
	defer waitShutdown()
	go func() { done <- p.Run(ctx, dev, tun) }()

	// Release the Read so the pump reads 1 byte and tries to
	// SendFrame (which returns boom).
	time.Sleep(20 * time.Millisecond)
	dev.releaseRead()
	waitShutdown()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("Run returned nil, want wraps *PumpError")
		}
		if !contractsession.IsPumpError(err) {
			t.Fatalf("Run returned %v, want wraps *PumpError", err)
		}
		if got := contractsession.PumpReason(err); got != contractsession.ReasonSendFrameFailed {
			t.Fatalf("PumpReason = %q, want %q", got, contractsession.ReasonSendFrameFailed)
		}
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want wraps %v", err, boom)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return within 2s after SendFrame error")
	}
}

// TestPump_Run_TagDispatchFailClosed verifies that a packet
// whose first byte does not match 0x4x or 0x6x fails closed
// with ReasonSendFrameFailed. The pump does NOT push garbage
// to the peer.
func TestPump_Run_TagDispatchFailClosed(t *testing.T) {
	t.Parallel()
	dev := newFakeDevice(1500)
	tun := newFakeTunnel()

	// 0x99 — neither IPv4 (0x4x) nor IPv6 (0x6x). The kernel
	// should never produce this; the pump treats it as a
	// defensive kernel bug / malicious write.
	dev.readN = 1
	dev.readPayload = []byte{0x99}

	p := session.New(session.Options{BufferOverhead: 0})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	waitShutdown := shutdownOnCtx(ctx, dev, tun)
	defer waitShutdown()
	go func() { done <- p.Run(ctx, dev, tun) }()

	time.Sleep(20 * time.Millisecond)
	dev.releaseRead()
	waitShutdown()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("Run returned nil on bad tag, want *PumpError")
		}
		if !contractsession.IsPumpError(err) {
			t.Fatalf("Run = %v, want wraps *PumpError", err)
		}
		if got := contractsession.PumpReason(err); got != contractsession.ReasonSendFrameFailed {
			t.Fatalf("PumpReason = %q, want %q", got, contractsession.ReasonSendFrameFailed)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return within 2s")
	}
	// The SendFrame mock should never have been called — the
	// pump fails the packet before framing.
	if got := tun.sendConsumed.Load(); got != 0 {
		t.Fatalf("SendFrame called %d times after tag fail-closed, want 0", got)
	}
}

// TestPump_Run_IPv4_TagDispatchedAs4 verifies the happy path
// for an IPv4 packet: the first byte (0x4x) is dispatched as
// tag=0x04 and the frame is sent to the peer with the same
// payload bytes.
func TestPump_Run_IPv4_TagDispatchedAs4(t *testing.T) {
	t.Parallel()
	dev := newFakeDevice(1500)
	tun := newFakeTunnel()
	pkt := ipv4Packet()

	dev.readN = len(pkt)
	dev.readPayload = pkt

	p := session.New(session.Options{BufferOverhead: 0})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	waitShutdown := shutdownOnCtx(ctx, dev, tun)
	defer waitShutdown()
	go func() { done <- p.Run(ctx, dev, tun) }()

	time.Sleep(20 * time.Millisecond)
	dev.releaseRead()

	// One SendFrame and one Read should have happened. The pump
// runs in a goroutine, so we poll for the SendFrame call to
// land within a bounded window.
	if !pollUntil(2*time.Second, func() bool {
		return dev.reads.Load() >= 1 && tun.sendConsumed.Load() >= 1
	}) {
		t.Fatalf("Device.Read=%d, SendFrame=%d after 2s; want both >=1",
			dev.reads.Load(), tun.sendConsumed.Load())
	}

	// Stop the pump.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on cancel after happy path, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return within 2s after ctx cancel")
	}

	// Verify the sent frame's tag and payload.
	tun.sendFramesMu.Lock()
	defer tun.sendFramesMu.Unlock()
	if len(tun.sentFrames) != 1 {
		t.Fatalf("sent %d frames, want 1", len(tun.sentFrames))
	}
	fr := tun.sentFrames[0]
	if fr.Tag != 0x04 {
		t.Fatalf("sent frame Tag = 0x%02x, want 0x04", fr.Tag)
	}
	if !bytes.Equal(fr.Packet, pkt) {
		t.Fatalf("sent frame Packet = % x, want % x", fr.Packet, pkt)
	}
}

// TestPump_Run_IPv6_TagDispatchedAs6 mirrors
// TestPump_Run_IPv4_TagDispatchedAs4 for an IPv6 packet
// (tag=0x06).
func TestPump_Run_IPv6_TagDispatchedAs6(t *testing.T) {
	t.Parallel()
	dev := newFakeDevice(1500)
	tun := newFakeTunnel()
	pkt := ipv6Packet()

	dev.readN = len(pkt)
	dev.readPayload = pkt

	p := session.New(session.Options{BufferOverhead: 0})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	waitShutdown := shutdownOnCtx(ctx, dev, tun)
	defer waitShutdown()
	go func() { done <- p.Run(ctx, dev, tun) }()

	time.Sleep(20 * time.Millisecond)
	dev.releaseRead()

	// Wait for the pump goroutine to read the packet and call
	// SendFrame.
	if !pollUntil(2*time.Second, func() bool {
		return tun.sendConsumed.Load() >= 1
	}) {
		t.Fatalf("SendFrame invocations = %d after 2s, want >= 1",
			tun.sendConsumed.Load())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on cancel, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return within 2s")
	}

	tun.sendFramesMu.Lock()
	defer tun.sendFramesMu.Unlock()
	fr := tun.sentFrames[0]
	if fr.Tag != 0x06 {
		t.Fatalf("sent frame Tag = 0x%02x, want 0x06", fr.Tag)
	}
	if !bytes.Equal(fr.Packet, pkt) {
		t.Fatalf("sent frame Packet = % x, want % x", fr.Packet, pkt)
	}
}

// TestPump_Run_PartnerTearDownOnError verifies that an error in
// one direction cancels the partner goroutine (per ADR 0005's
// shutdown symmetry). A read-side failure tears down the
// receive goroutine, which is observed by the partner exiting
// via ctx (not via another independent error).
func TestPump_Run_PartnerTearDownOnError(t *testing.T) {
	t.Parallel()
	dev := newFakeDevice(1500)
	tun := newFakeTunnel()
	dev.readErr = errors.New("read boom")
	dev.infiniteReads = true

	p := session.New(session.Options{BufferOverhead: 0})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	waitShutdown := shutdownOnCtx(ctx, dev, tun)
	defer waitShutdown()
	go func() { done <- p.Run(ctx, dev, tun) }()

	time.Sleep(20 * time.Millisecond)
	dev.releaseRead()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("Run returned nil, want wraps *PumpError")
		}
		// The orchestrator returns the read-side error (the
		// first non-nil from any goroutine), NOT a
		// context.Canceled from the partner's exit. ADR
		// 0005's "first error wins, ctx-cancel is just a
		// shutdown signal for the partner".
		if !contractsession.IsPumpError(err) {
			t.Fatalf("Run = %v, want wraps *PumpError", err)
		}
		if got := contractsession.PumpReason(err); got != contractsession.ReasonReadTUNFailed {
			t.Fatalf("PumpReason = %q, want %q (the root cause, not ctx-cancel)",
				got, contractsession.ReasonReadTUNFailed)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return within 2s")
	}
}

// TestPump_Run_PoolCleanupOnCancel is a smoke test that the
// sync.Pool does not deadlock or leak goroutines when ctx is
// cancelled. We assert by completing in a bounded time (no
// goroutine leak); the exact pool internals are an
// implementation detail.
func TestPump_Run_PoolCleanupOnCancel(t *testing.T) {
	t.Parallel()
	dev := newFakeDevice(1500)
	tun := newFakeTunnel()

	// Multi-iteration IPv4 happy path: read -> send -> read ->
	// send. Confirms the pool can be reused across iterations.
	dev.readN = 1
	dev.readPayload = ipv4Packet()
	dev.infiniteReads = true

	p := session.New(session.Options{BufferOverhead: 0})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	waitShutdown := shutdownOnCtx(ctx, dev, tun)
	defer waitShutdown()
	go func() { done <- p.Run(ctx, dev, tun) }()

	// Release the first Read to let the pump complete an
	// iteration. With infiniteReads=true, subsequent Reads
	// short-circuit past the block and the pump loops.
	time.Sleep(20 * time.Millisecond)
	dev.releaseRead()

	// Wait for the pump to complete at least one iteration
	// (one Read + one SendFrame) before we cancel. The pump
	// goroutine runs asynchronously; a fixed sleep cannot
	// reliably observe the round-trip under -race.
	if !pollUntil(2*time.Second, func() bool {
		return dev.reads.Load() >= 1 && tun.sendConsumed.Load() >= 1
	}) {
		t.Fatalf("pump did not complete an iteration within 2s (read=%d, send=%d)",
			dev.reads.Load(), tun.sendConsumed.Load())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on ctx cancel, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return within 2s")
	}
}

// Compile-time check: Pump satisfies contractsession.Pump.
var _ contractsession.Pump = (*session.Pump)(nil)

// Compile-time check: fakeDevice satisfies contracttun.Device.
// Keeps the test binary honest if the contract grows.
var _ contracttun.Device = (*fakeDevice)(nil)

// Compile-time check: fakeTunnel satisfies transport.Tunnel.
var _ transport.Tunnel = (*fakeTunnel)(nil)
