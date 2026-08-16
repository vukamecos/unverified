package tun_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	contracttun "github.com/vukamecos/unverified/internal/contract/tun"
)

// fakeDevice is a hand-rolled test double for the [contracttun.Device]
// interface. The interface is small enough (5 methods) that hand-rolling
// it is clearer than wiring moq for this iteration; the future moq
// rule (ARCH §13.1) targets larger interfaces where the hand-rolled
// version would be a maintenance liability. If the interface grows
// past ~7 methods, regenerate this file via moq.
type fakeDevice struct {
	name    string
	mtu     int
	mtuErr  error
	readN   int
	readErr error
	written []byte
	writeN  int
	writeErr error
	closed  bool
	closeErr error
}

func (f *fakeDevice) Name() string { return f.name }

func (f *fakeDevice) MTU() (int, error) {
	if f.closed {
		return 0, contracttun.ErrClosed
	}
	return f.mtu, f.mtuErr
}

func (f *fakeDevice) Read(p []byte) (int, error) {
	if f.closed {
		return 0, contracttun.ErrClosed
	}
	if f.readErr != nil {
		return 0, f.readErr
	}
	n := copy(p, bytes.Repeat([]byte{0xAA}, f.readN))
	return n, nil
}

func (f *fakeDevice) Write(p []byte) (int, error) {
	if f.closed {
		return 0, contracttun.ErrClosed
	}
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	f.written = append(f.written, p...)
	n := f.writeN
	if n <= 0 || n > len(p) {
		n = len(p)
	}
	return n, nil
}

func (f *fakeDevice) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	return f.closeErr
}

// TestDeviceContract_Name verifies the Name() accessor surfaces the
// kernel-assigned interface name (e.g. "tun0") verbatim through the
// contract. Real devices resolve the name at Open time; the contract
// has no knowledge of that and is just a getter.
func TestDeviceContract_Name(t *testing.T) {
	t.Parallel()
	d := &fakeDevice{name: "unvfd0"}
	if got := d.Name(); got != "unvfd0" {
		t.Fatalf("Name() = %q, want %q", got, "unvfd0")
	}
}

// TestDeviceContract_MTU_OK verifies MTU() returns the configured value
// without error on a live device.
func TestDeviceContract_MTU_OK(t *testing.T) {
	t.Parallel()
	d := &fakeDevice{mtu: 1420}
	mtu, err := d.MTU()
	if err != nil {
		t.Fatalf("MTU() unexpected error: %v", err)
	}
	if mtu != 1420 {
		t.Fatalf("MTU() = %d, want 1420", mtu)
	}
}

// TestDeviceContract_MTU_UnderlyingErr verifies the contract surfaces
// arbitrary underlying errors (e.g. ENETDOWN, EINVAL) without
// rewriting them. Only ErrClosed is special.
func TestDeviceContract_MTU_UnderlyingErr(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	d := &fakeDevice{mtuErr: boom}
	if _, err := d.MTU(); !errors.Is(err, boom) {
		t.Fatalf("MTU() = %v, want wraps %v", err, boom)
	}
}

// TestDeviceContract_ReadWrite_Payload covers the data path: Read
// copies the requested number of bytes; Write accepts the payload.
// These are the two methods the tunnel Pump goroutine (§4.2 of ARCH)
// drives in a tight loop.
func TestDeviceContract_ReadWrite_Payload(t *testing.T) {
	t.Parallel()
	d := &fakeDevice{readN: 64, writeN: 64}

	buf := make([]byte, 128)
	n, err := d.Read(buf)
	if err != nil {
		t.Fatalf("Read() unexpected error: %v", err)
	}
	if n != 64 {
		t.Fatalf("Read() = %d, want 64", n)
	}
	for i := 0; i < n; i++ {
		if buf[i] != 0xAA {
			t.Fatalf("Read() payload byte %d = 0x%02x, want 0xAA", i, buf[i])
		}
	}

	payload := []byte("hello")
	n, err = d.Write(payload)
	if err != nil {
		t.Fatalf("Write() unexpected error: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("Write() = %d, want %d", n, len(payload))
	}
	if !bytes.Equal(d.written, payload) {
		t.Fatalf("Write() payload = %x, want %x", d.written, payload)
	}
}

// TestDeviceContract_ClosedSemantics is the central contract test:
// after Close, every method returns [contracttun.ErrClosed] (not a
// raw os.ErrClosed / net.ErrClosed / io.EOF — we own the sentinel).
// This is the property the Linux implementation in
// internal/tunnel/tun/tun_linux.go relies on, so it must hold for
// any future implementation too.
func TestDeviceContract_ClosedSemantics(t *testing.T) {
	t.Parallel()
	d := &fakeDevice{mtu: 1500, readN: 4, writeN: 4}

	if err := d.Close(); err != nil {
		t.Fatalf("Close() unexpected error: %v", err)
	}

	if _, err := d.MTU(); !errors.Is(err, contracttun.ErrClosed) {
		t.Errorf("MTU() after Close = %v, want ErrClosed", err)
	}
	if _, err := d.Read(make([]byte, 16)); !errors.Is(err, contracttun.ErrClosed) {
		t.Errorf("Read() after Close = %v, want ErrClosed", err)
	}
	if _, err := d.Write([]byte("x")); !errors.Is(err, contracttun.ErrClosed) {
		t.Errorf("Write() after Close = %v, want ErrClosed", err)
	}

	// Close must be idempotent.
	if err := d.Close(); err != nil {
		t.Errorf("second Close() = %v, want nil (idempotent)", err)
	}
}

// TestDeviceContract_CloseErr verifies that a non-nil error from
// Close is surfaced to the caller exactly once; subsequent Close
// calls return nil. This mirrors the atomic.Bool pattern in
// internal/tunnel/tun/tun_linux.go.
func TestDeviceContract_CloseErr(t *testing.T) {
	t.Parallel()
	boom := errors.New("close boom")
	d := &fakeDevice{closeErr: boom}
	if err := d.Close(); !errors.Is(err, boom) {
		t.Fatalf("first Close() = %v, want wraps %v", err, boom)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("second Close() = %v, want nil", err)
	}
}

// TestErrClosed_Message is a documentation-style assertion: the
// sentinel's message mentions the package name so log lines are
// greppable.
func TestErrClosed_Message(t *testing.T) {
	t.Parallel()
	if !strings.Contains(contracttun.ErrClosed.Error(), "tun:") {
		t.Fatalf("ErrClosed message = %q, want substring %q", contracttun.ErrClosed.Error(), "tun:")
	}
}