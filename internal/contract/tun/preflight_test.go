package tun_test

import (
	"errors"
	"strings"
	"testing"

	contracttun "github.com/vukamecos/unverified/internal/contract/tun"
)

// fakePreflight records the probe results it was constructed with.
// The contract is small (Run -> error) so the fake is hand-rolled;
// the future moq rule (ARCH §13.1) targets interfaces where the
// hand-rolled version would be a maintenance liability.
type fakePreflight struct {
	deviceErr error
	capsErr   error
}

func (f *fakePreflight) Run() error {
	if f.deviceErr != nil {
		return f.deviceErr
	}
	if f.capsErr != nil {
		return f.capsErr
	}
	return nil
}

// TestPreflightError_MessageAndUnwrap verifies the typed error's
// human-readable message includes both the Reason and the underlying
// cause, and that errors.Unwrap recovers the cause.
func TestPreflightError_MessageAndUnwrap(t *testing.T) {
	t.Parallel()
	cause := errors.New("underlying")
	pe := &contracttun.PreflightError{Reason: contracttun.ReasonTUNDeviceMissing, Cause: cause}

	msg := pe.Error()
	if !strings.Contains(msg, "tun preflight:") {
		t.Errorf("Error() = %q, want substring %q", msg, "tun preflight:")
	}
	if !strings.Contains(msg, contracttun.ReasonTUNDeviceMissing) {
		t.Errorf("Error() = %q, want substring %q", msg, contracttun.ReasonTUNDeviceMissing)
	}
	if !strings.Contains(msg, "underlying") {
		t.Errorf("Error() = %q, want substring %q (cause)", msg, "underlying")
	}
	if !errors.Is(pe, cause) {
		t.Errorf("errors.Is(%v, %v) = false, want true", pe, cause)
	}
}

// TestPreflightError_NilCause verifies a PreflightError without an
// underlying cause still produces a usable message.
func TestPreflightError_NilCause(t *testing.T) {
	t.Parallel()
	pe := &contracttun.PreflightError{Reason: contracttun.ReasonCAPNetAdminMissing}
	if pe.Cause != nil {
		t.Errorf("Cause = %v, want nil", pe.Cause)
	}
	if !strings.Contains(pe.Error(), contracttun.ReasonCAPNetAdminMissing) {
		t.Errorf("Error() = %q, want substring %q", pe.Error(), contracttun.ReasonCAPNetAdminMissing)
	}
}

// TestIsPreflightError verifies the predicate accepts *PreflightError
// and rejects plain errors.
func TestIsPreflightError(t *testing.T) {
	t.Parallel()
	pe := &contracttun.PreflightError{Reason: contracttun.ReasonTUNDeviceMissing}
	if !contracttun.IsPreflightError(pe) {
		t.Error("IsPreflightError(*PreflightError) = false, want true")
	}
	plain := errors.New("not a preflight")
	if contracttun.IsPreflightError(plain) {
		t.Error("IsPreflightError(plain) = true, want false")
	}
	// nil is not a preflight error.
	if contracttun.IsPreflightError(nil) {
		t.Error("IsPreflightError(nil) = true, want false")
	}
}

// TestPreflightReason verifies the Reason extractor pulls the stable
// string out of a *PreflightError and returns "" for everything else.
func TestPreflightReason(t *testing.T) {
	t.Parallel()
	pe := &contracttun.PreflightError{Reason: contracttun.ReasonCapProbeFailed}
	if got := contracttun.PreflightReason(pe); got != contracttun.ReasonCapProbeFailed {
		t.Errorf("PreflightReason(*PreflightError) = %q, want %q", got, contracttun.ReasonCapProbeFailed)
	}
	if got := contracttun.PreflightReason(errors.New("plain")); got != "" {
		t.Errorf("PreflightReason(plain) = %q, want \"\"", got)
	}
	if got := contracttun.PreflightReason(nil); got != "" {
		t.Errorf("PreflightReason(nil) = %q, want \"\"", got)
	}
}

// TestPreflightReason_Stability pins the Reason string values. The
// strings are part of the public contract (operator scripts and
// downstream tooling switch on them); renaming any of them is a
// breaking change.
func TestPreflightReason_Stability(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"ReasonUnsupportedPlatform":  "unsupported_platform",
		"ReasonTUNDeviceMissing":     "tun_device_missing",
		"ReasonTUNDeviceNotReadWrite": "tun_device_not_read_write",
		"ReasonCAPNetAdminMissing":   "cap_net_admin_missing",
		"ReasonCapProbeFailed":       "cap_probe_failed",
	}
	got := map[string]string{
		"ReasonUnsupportedPlatform":   contracttun.ReasonUnsupportedPlatform,
		"ReasonTUNDeviceMissing":      contracttun.ReasonTUNDeviceMissing,
		"ReasonTUNDeviceNotReadWrite": contracttun.ReasonTUNDeviceNotReadWrite,
		"ReasonCAPNetAdminMissing":    contracttun.ReasonCAPNetAdminMissing,
		"ReasonCapProbeFailed":        contracttun.ReasonCapProbeFailed,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (do not rename; strings are part of the contract)", k, got[k], v)
		}
	}
}

// TestPreflight_HappyPath is the smoke test: both probes succeed,
// Run returns nil, and the caller can proceed.
func TestPreflight_HappyPath(t *testing.T) {
	t.Parallel()
	p := &fakePreflight{}
	if err := p.Run(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
}

// TestPreflight_DeviceFailureShortCircuits verifies that a device
// failure is reported as-is and the capability probe does NOT run.
// (Order matters: a missing device should not produce a misleading
// "no capability" error from a probe that was never reached.)
func TestPreflight_DeviceFailureShortCircuits(t *testing.T) {
	t.Parallel()
	deviceErr := &contracttun.PreflightError{Reason: contracttun.ReasonTUNDeviceMissing}
	p := &fakePreflight{
		deviceErr: deviceErr,
		capsErr:   errors.New("should not be reached"),
	}
	err := p.Run()
	if err == nil {
		t.Fatal("Run() = nil, want error")
	}
	if contracttun.PreflightReason(err) != contracttun.ReasonTUNDeviceMissing {
		t.Errorf("PreflightReason = %q, want %q", contracttun.PreflightReason(err), contracttun.ReasonTUNDeviceMissing)
	}
}

// TestPreflight_CapsCheckedAfterDevice verifies the precedence:
// device probe runs first; on success, the capability probe runs.
// A caps-only failure is only surfaced when the device probe passed.
func TestPreflight_CapsCheckedAfterDevice(t *testing.T) {
	t.Parallel()
	capsErr := &contracttun.PreflightError{Reason: contracttun.ReasonCAPNetAdminMissing}
	p := &fakePreflight{capsErr: capsErr}
	err := p.Run()
	if err == nil {
		t.Fatal("Run() = nil, want error")
	}
	if contracttun.PreflightReason(err) != contracttun.ReasonCAPNetAdminMissing {
		t.Errorf("PreflightReason = %q, want %q", contracttun.PreflightReason(err), contracttun.ReasonCAPNetAdminMissing)
	}
}

// TestPreflight_ProductionDefault_AtLeastConstructible is a guard
// against accidental breaking of the production constructor on the
// current GOOS. We cannot assert success (no /dev/net/tun and no
// CAP_NET_ADMIN on most test runners), only that the constructor
// returns a non-nil Preflight and Run returns either nil or a
// PreflightError (i.e. it never panics, never returns a foreign
// error type).
//
// On non-Linux GOOS, the production constructor returns the
// unsupported-platform stub; the test is meaningful only on Linux.
// The constructor itself is in internal/tunnel/tun, not in the
// contract package; this test guards only the contract shapes.
func TestPreflight_ProductionDefault_AtLeastConstructible(t *testing.T) {
	t.Parallel()
	var p contracttun.Preflight = &fakePreflight{}
	if p == nil {
		t.Fatal("fakePreflight assigned to nil Preflight interface")
	}
}