// Package tun defines the abstract TUN-device seam.
//
// Concrete implementations live outside this package
// (see internal/tunnel/tun on Linux); the rest of the codebase depends
// on the [Device] interface here, so the data path can be exercised in
// tests without a real /dev/net/tun and so a future cross-platform port
// can drop in a different implementation behind the same contract.
//
// The interface is deliberately small: open-by-name, raw packet
// Read/Write, Close, plus Name() and MTU() accessors. Anything more
// (packet-info headers, event readiness, persistent state) belongs to
// the kernel TUN device and is not exposed here — kernels do not all
// behave the same way, and we want the contract to be portable.
package tun

import "errors"

// ErrClosed is returned by Read and Write after the device has been
// Closed. It is the only error the contract guarantees callers will see
// from this package; all other errors come from the OS layer and must
// be inspected by the caller (typically: ENETDOWN, EAGAIN, EINVAL).
var ErrClosed = errors.New("tun: device is closed")

// Device is a single TUN interface. Implementations are safe for use by
// a single goroutine; concurrent Read/Write is not part of the
// contract.
type Device interface {
	// Name returns the kernel-assigned interface name (e.g. "tun0").
	// On Linux the name is whatever the kernel chose when the
	// TUNSETIFF ioctl ran with a zero-prefixed name; pass an empty
	// string to let the kernel assign.
	Name() string

	// MTU returns the device MTU in bytes, or an error if the
	// underlying ioctl failed.
	MTU() (int, error)

	// Read blocks until one IP packet is available from the kernel
	// and copies it into p, returning the number of bytes copied.
	// On a closed device, Read returns [ErrClosed].
	Read(p []byte) (int, error)

	// Write hands one IP packet to the kernel for transmission via
	// the device. The return is the number of bytes consumed from p
	// (may be less than len(p) on a partial write — callers should
	// loop). On a closed device, Write returns [ErrClosed].
	Write(p []byte) (int, error)

	// Close releases the underlying file descriptor. Close is
	// idempotent and safe to call from a signal-handler context.
	Close() error
}