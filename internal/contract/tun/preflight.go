package tun

import "errors"

// PreflightError is the typed error returned by Preflight.Run when the
// caller is missing one or more preconditions for opening a /dev/net/tun
// device. The Reason field is a stable string callers can switch on
// (without parsing the message) and map to user-facing log lines /
// systemd exit codes.
//
// All PreflightError returns are terminal: the daemon refuses to start
// (per ARCH §2.5 "fail closed"). The caller MUST NOT retry.
type PreflightError struct {
	// Reason is one of the ReasonXxx constants below. Stable across
	// releases; do not introduce new values without adding a new
	// constant.
	Reason string

	// Cause is the underlying OS / syscall error, wrapped for
	// errors.Is / errors.As inspection. May be nil if no underlying
	// error is available (e.g. a logic-only failure).
	Cause error
}

// Error implements the error interface. The message is human-readable
// and includes the Reason for grep-ability.
func (e *PreflightError) Error() string {
	if e.Cause != nil {
		return "tun preflight: " + e.Reason + ": " + e.Cause.Error()
	}
	return "tun preflight: " + e.Reason
}

// Unwrap returns the underlying Cause so errors.Is / errors.As can
// inspect the OS error.
func (e *PreflightError) Unwrap() error {
	return e.Cause
}

// Stable Reason values. Treat these as part of the public contract;
// bumping a value is a breaking change for callers that switch on it.
const (
	// ReasonUnsupportedPlatform — the build was for a non-Linux
	// GOOS; /dev/net/tun does not exist there. This is a build
	// configuration error, not a runtime one.
	ReasonUnsupportedPlatform = "unsupported_platform"

	// ReasonTUNDeviceMissing — /dev/net/tun could not be stat'd
	// or accessed. The kernel module `tun` is almost certainly
	// not loaded (or the device node is missing — some minimal
	// containers strip it).
	ReasonTUNDeviceMissing = "tun_device_missing"

	// ReasonTUNDeviceNotReadWrite — /dev/net/tun exists but the
	// process cannot open it O_RDWR. Most likely cause: the
	// process does not have write permission on the device node
	// (mode 0666 root:root typically; a container with a
	// restricted DeviceAllow list).
	ReasonTUNDeviceNotReadWrite = "tun_device_not_read_write"

	// ReasonCAPNetAdminMissing — the calling process lacks
	// CAP_NET_ADMIN in its effective set. TUNSETIFF is a
	// privileged ioctl; without this capability the kernel will
	// silently reject the ioctl with EPERM and the open will
	// appear to fail. ARCH §11 forbids running the daemon with
	// the capability set after setup; the preflight check exists
	// precisely to fail closed before setup begins.
	ReasonCAPNetAdminMissing = "cap_net_admin_missing"

	// ReasonCapProbeFailed — the capability query syscall itself
	// failed (very rare; indicates a non-Linux kernel or a
	// broken container seccomp filter). Reported as a distinct
	// reason so the operator can diagnose.
	ReasonCapProbeFailed = "cap_probe_failed"
)

// IsPreflightError reports whether err is a *PreflightError.
func IsPreflightError(err error) bool {
	var pe *PreflightError
	return errors.As(err, &pe)
}

// PreflightReason extracts the stable Reason string from err if it is
// a *PreflightError. Returns "" otherwise.
func PreflightReason(err error) string {
	var pe *PreflightError
	if errors.As(err, &pe) {
		return pe.Reason
	}
	return ""
}

// DeviceAvailabilityProbe returns nil if /dev/net/tun is openable in
// read-write mode by the current process, or a *PreflightError with
// ReasonTUNDeviceMissing / ReasonTUNDeviceNotReadWrite otherwise.
//
// Implementations MUST NOT actually open the device for an extended
// period — this is a probe, not the real open. Probing is done by
// stat'ing the path (for the missing check) and by attempting a brief
// O_RDWR open and immediately closing (for the read-write check). The
// probe fd is closed before Run returns.
//
// CapabilityProbe returns nil if the process holds CAP_NET_ADMIN in
// its effective set, or a *PreflightError with
// ReasonCAPNetAdminMissing otherwise. A failure of the capget syscall
// itself returns ReasonCapProbeFailed.
//
// The split into two probes lets the caller test each independently
// and lets unit tests inject fakes for each surface separately.
type (
	DeviceAvailabilityProbe func() error
	CapabilityProbe         func() error
)

// Preflight is the abstract pre-flight check contract. Implementations
// live outside this package; the production implementation lives in
// internal/tunnel/tun/preflight_linux.go.
//
// Run performs every check the open path needs to succeed. On any
// failure it returns a *PreflightError; on success it returns nil.
//
// Preflight MUST be fail-closed: a probe that cannot positively
// confirm its precondition returns an error, never nil with a caveat.
type Preflight interface {
	Run() error
}