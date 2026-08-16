//go:build linux

package tun

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	contracttun "github.com/vukamecos/unverified/internal/contract/tun"
)

// tunDevicePath is the kernel TUN device node. Constant so tests and
// callers can refer to the same path without typos.
const tunDevicePath = "/dev/net/tun"

// capNetAdminBit is the bit position of CAP_NET_ADMIN in the low
// capability word. It is 12 on every Linux architecture we target
// (the kernel exposes capability numbers, not arch-dependent bit
// positions, on this side of the syscall).
const capNetAdminBit = unix.CAP_NET_ADMIN

// DefaultPreflight returns the production [contracttun.Preflight] for
// the current process. It probes /dev/net/tun accessibility and the
// process's CAP_NET_ADMIN effective set, in that order. Either
// failure is a hard error.
//
// Callers should invoke DefaultPreflight() at process start, BEFORE
// any privileged operation (TUN open, nftables rules install,
// capability drop). The check is cheap (one stat + one open + one
// capget) and runs synchronously; it does not belong in the daemon's
// steady-state hot path.
func DefaultPreflight() contracttun.Preflight {
	return &linuxPreflight{
		probeDevice: defaultDeviceProbe,
		probeCaps:   defaultCapProbe,
	}
}

// linuxPreflight is the production [contracttun.Preflight]
// implementation. The two probe fields are exported via the
// constructor's default helpers but can be replaced in tests with
// fakes (the contract test in preflight_test.go does exactly that).
type linuxPreflight struct {
	probeDevice contracttun.DeviceAvailabilityProbe
	probeCaps   contracttun.CapabilityProbe
}

// Run executes every check and returns the first failure as a
// *contracttun.PreflightError. Returns nil only when every check
// passed. The order of checks is deliberate: device first, then
// capabilities — a missing device is the more common container /
// minimal-image failure mode and the operator-actionable error.
func (p *linuxPreflight) Run() error {
	if err := p.probeDevice(); err != nil {
		return err
	}
	if err := p.probeCaps(); err != nil {
		return err
	}
	return nil
}

// defaultDeviceProbe is the production
// [contracttun.DeviceAvailabilityProbe]. It performs two checks:
//
//  1. The /dev/net/tun path must stat to a character device file
//     owned by root (or, in containers, the device node must at
//     least exist). stat-ing catches the common "kernel module tun
//     not loaded" failure where modprobe has not run and the
//     device node is absent.
//
//  2. The process must be able to open /dev/net/tun O_RDWR. The
//     probe opens and immediately closes; if the kernel denies
//     open (EACCES from a too-restrictive DeviceAllow=, or ENXIO
//     from a missing module), the probe returns a PreflightError
//     with the appropriate stable Reason.
func defaultDeviceProbe() error {
	// (1) stat the path. A non-existent path returns ENOENT;
	// anything else is treated as "exists" for the next step.
	if _, err := os.Stat(tunDevicePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &contracttun.PreflightError{
				Reason: contracttun.ReasonTUNDeviceMissing,
				Cause:  err,
			}
		}
		// Some other stat error — surface as missing rather than
		// guessing. The operator will recognise the underlying
		// errno from the wrapped cause.
		return &contracttun.PreflightError{
			Reason: contracttun.ReasonTUNDeviceMissing,
			Cause:  err,
		}
	}

	// (2) probe open + immediate close. The kernel's open(2) on
	// /dev/net/tun does not require CAP_NET_ADMIN (only the
	// subsequent TUNSETIFF ioctl does), so this catches
	// device-node permission problems independently of the
	// capability check below.
	fd, err := unix.Open(tunDevicePath, unix.O_RDWR, 0)
	if err != nil {
		return &contracttun.PreflightError{
			Reason: contracttun.ReasonTUNDeviceNotReadWrite,
			Cause:  fmt.Errorf("open %s O_RDWR: %w", tunDevicePath, err),
		}
	}
	// Best-effort close; ignore the error (the probe fd is
	// short-lived and we are about to return regardless).
	_ = unix.Close(fd)
	return nil
}

// defaultCapProbe is the production
// [contracttun.CapabilityProbe]. It issues a capget syscall for the
// current process (pid 0 = "self" in the Linux capability API) and
// checks that CAP_NET_ADMIN is set in the Effective set.
//
// The Effective set is the right one to check: it is what the kernel
// uses for capability checks on every syscall, including TUNSETIFF.
// Permitted / Inheritable are bookkeeping sets used for transitions
// across execve and fork; the kernel never grants access based on
// them alone.
//
// LINUX_CAPABILITY_VERSION_3 is the modern (post-2.6.25) format.
// The data array has two 32-bit words (low / high) covering caps
// 0..63. CAP_NET_ADMIN = 12 fits in the low word on every supported
// architecture. The high word is reserved for future expansion and
// is read for completeness so the kernel does not return EINVAL.
func defaultCapProbe() error {
	hdr := unix.CapUserHeader{
		Version: unix.LINUX_CAPABILITY_VERSION_3,
		Pid:     0, // 0 = current process
	}
	// Two words: caps 0..31 and caps 32..63.
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return &contracttun.PreflightError{
			Reason: contracttun.ReasonCapProbeFailed,
			Cause:  fmt.Errorf("capget: %w", err),
		}
	}
	if data[0].Effective&(1<<capNetAdminBit) == 0 {
		return &contracttun.PreflightError{
			Reason: contracttun.ReasonCAPNetAdminMissing,
			Cause:  fmt.Errorf("CAP_NET_ADMIN (bit %d) not in effective set; effective=0x%x", capNetAdminBit, data[0].Effective),
		}
	}
	return nil
}