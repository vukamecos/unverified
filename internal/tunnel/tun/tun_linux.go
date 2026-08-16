//go:build linux

// Package tun wraps the Linux /dev/net/tun + TUNSETIFF ioctl behind the
// abstract [contract/tun.Device] interface.
//
// The package is Linux-only by build tag. Cross-compile to a non-Linux
// GOOS picks up tun_other.go, which fails Open at runtime with a clear
// error rather than a build failure.
//
// The implementation uses stdlib syscall + golang.org/x/sys/unix for
// the two ioctls (TUNSETIFF, SIOCGIFMTU) and an os.File around the fd.
// We deliberately do not pull in gvisor.dev/gvisor/pkg/tcpip/link/tun
// for the open path; that package is a thin wrapper around the same
// syscall, and its main consumers are gvisor's userspace netstack
// types (buffer.View, stack.Stack, waiter.Queue) which we do not need.
// See ADR 0003 for the original library-choice rationale; the package
// name there referred to the broader dependency surface and the
// library has since been downgraded to stdlib + x/sys/unix after the
// minimum-surface code was found to be ~30 lines.
package tun

import (
	"fmt"
	"os"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"

	contracttun "github.com/vukamecos/unverified/internal/contract/tun"
)

// ifReqSize matches the kernel's struct ifreq on Linux/amd64 and
// Linux/arm64: 16 bytes name + 2 bytes flags (for TUNSETIFF) followed
// by a union of ifru (sized for sockaddr, 16 bytes on most arches).
// We only need name + flags for TUNSETIFF; the union is left zero.
const ifReqSize = 40

// ifReq is the kernel's struct ifreq (Linux) packed to match ABI.
type ifReq struct {
	Name [16]byte
	Flags uint16
	_ [ifReqSize - 16 - 2]byte
}

// Open creates a Linux TUN device.
//
// If name is empty, the kernel assigns the next free "tunN" name. A
// non-empty name with a "%d" suffix (e.g. "unvfd%d") is interpreted by
// the kernel as a name template: the kernel replaces "%d" with the
// next free index. Plain names ("tun0") match exactly and fail if the
// name is taken.
//
// Open requires CAP_NET_ADMIN on the calling process (TUNSETIFF is a
// privileged ioctl). The underlying syscall errors (unix.Errno) are
// surfaced unchanged so the caller can switch on them: EPERM = no caps,
// ENXIO = no /dev/net/tun (kernel module "tun" not loaded), EBUSY =
// name taken.
func Open(name string) (contracttun.Device, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("tun: open /dev/net/tun: %w", err)
	}

	var req ifReq
	copy(req.Name[:], name)
	// IFF_TUN = 0x0001, IFF_NO_PI = 0x1000 (skip 4-byte packet-info
	// header; we want raw IP packets, matching the codec in
	// internal/transport/grpc/tunnelpb).
	req.Flags = unix.IFF_TUN | unix.IFF_NO_PI

	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.TUNSETIFF, uintptr(unsafe.Pointer(&req))); errno != 0 {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("tun: TUNSETIFF(%q): %w", name, errno)
	}

	// Resolve the kernel-assigned name (the kernel may have rewritten
	// "%d" or substituted its own "tunN"). The name is
	// NUL-terminated inside the 16-byte array; trim it.
	resolved := nullTerminatedString(req.Name[:])

	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("tun: set nonblock: %w", err)
	}

	d := &linuxDevice{
		f:          os.NewFile(uintptr(fd), "tun-"+resolved),
		cachedName: resolved,
	}
	return d, nil
}

// linuxDevice is the concrete [contracttun.Device] backed by a single
// /dev/net/tun fd opened by [Open].
//
// The fd is non-blocking; a goroutine doing a blocking Read will
// starve the Go scheduler under no-traffic conditions. The caller is
// expected to drive Read/Write from a select loop or a runtime poller
// (epoll in a future iteration; os.File.SetReadDeadline is not yet
// supported on TUN fds in x/sys/unix).
type linuxDevice struct {
	f          *os.File
	cachedName string
	closed     atomic.Bool
}

// Name returns the kernel-assigned interface name resolved at Open
// time (e.g. "tun0", or the post-template-resolved "unvfd0" form).
// The kernel does not let us rename the device after TUNSETIFF
// without a second ioctl, so the value is cached.
func (d *linuxDevice) Name() string {
	return d.cachedName
}

// MTU returns the device MTU in bytes via SIOCGIFMTU on an AF_INET
// control socket. This is a separate ioctl from TUNSETIFF and reads
// the device's current MTU as the kernel sees it; the default is
// 1500 until the operator overrides it with `ip link set tun0 mtu
// 1420` or we set it programmatically in a future iteration.
func (d *linuxDevice) MTU() (int, error) {
	if d.closed.Load() {
		return 0, contracttun.ErrClosed
	}
	sock, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return 0, fmt.Errorf("tun: mtu socket: %w", err)
	}
	defer unix.Close(sock)

	ifr, err := unix.NewIfreq(d.cachedName)
	if err != nil {
		return 0, fmt.Errorf("tun: mtu ifreq(%q): %w", d.cachedName, err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(sock), unix.SIOCGIFMTU, uintptr(unsafe.Pointer(ifr))); errno != 0 {
		return 0, fmt.Errorf("tun: SIOCGIFMTU(%q): %w", d.cachedName, errno)
	}
	// The kernel writes the MTU as a 32-bit signed integer into the
	// ifru union; unix.Ifreq.Uint32 reads that slot directly.
	return int(ifr.Uint32()), nil
}

// Read copies one IP packet from the kernel into p and returns the
// number of bytes copied. On a closed device, Read returns
// [contracttun.ErrClosed] rather than the OS error from reading a
// closed fd.
func (d *linuxDevice) Read(p []byte) (int, error) {
	if d.closed.Load() {
		return 0, contracttun.ErrClosed
	}
	return d.f.Read(p)
}

// Write hands one IP packet to the kernel for transmission. The
// return is the number of bytes consumed from p and follows the
// usual os.File.Write semantics (may be less than len(p) on partial
// write — callers should loop).
func (d *linuxDevice) Write(p []byte) (int, error) {
	if d.closed.Load() {
		return 0, contracttun.ErrClosed
	}
	return d.f.Write(p)
}

// Close releases the underlying file descriptor. Close is
// idempotent and safe to call from a signal-handler context (it
// does not block). The first call closes the wrapped *os.File;
// subsequent calls return nil without touching the closed fd.
func (d *linuxDevice) Close() error {
	if !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	return d.f.Close()
}

// nullTerminatedString returns the NUL-trimmed contents of a 16-byte
// ifreq name field. The kernel writes the resolved interface name
// into this field as a NUL-terminated C string.
func nullTerminatedString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}