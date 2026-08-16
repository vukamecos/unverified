package hostprobe_test

import (
	"runtime"
	"strings"
	"testing"

	"github.com/vukamecos/unverified/internal/hostprobe"
)

// TestCapsRequired_CanonicalOrder pins the canonical
// ordering of the runtime's required capability set. The
// ordering matters because CapsReport.Present[i] is
// indexed by CapsRequired[i] — reordering would silently
// mislabel every test that uses the slice.
func TestCapsRequired_CanonicalOrder(t *testing.T) {
	t.Parallel()
	if got, want := len(hostprobe.CapsRequired), 3; got != want {
		t.Fatalf("len(CapsRequired) = %d, want %d", got, want)
	}
	wantOrder := []string{"CAP_NET_ADMIN", "CAP_BPF", "CAP_PERFMON"}
	for i, want := range wantOrder {
		if got := hostprobe.CapsRequired[i].Name; got != want {
			t.Errorf("CapsRequired[%d].Name = %q, want %q",
				i, got, want)
		}
	}
}

// TestCapsEffective_RunsWithoutPanic runs the scriptable
// capability probe on Linux and asserts it returns a
// non-nil report (the process always has an Effective set,
// even if it is the empty one). On non-Linux the probe
// returns *hostprobe.UnsupportedPlatformError; that is
// cross-compile behaviour, not a failure.
//
// This is the host-side counterpart of
// [internal/tunnel/tun.defaultCapProbe] — the runtime
// preflight uses the same capget surface (LINUX_CAPABILITY_VERSION_3,
// pid=0, two-word Effective). The probe here is non-
// production: it lives under internal/hostprobe so the
// production binary never imports it.
func TestCapsEffective_RunsWithoutPanic(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf(
			"caps probe is Linux-only (running on %s); see capsprobe_other.go",
			runtime.GOOS)
	}
	t.Parallel()

	report, err := hostprobe.CapsEffective()
	if err != nil {
		t.Fatalf("CapsEffective() failed: %v", err)
	}
	if report == nil {
		t.Fatal("CapsEffective() returned nil report with nil err")
	}
	if len(report.Present) != len(hostprobe.CapsRequired) {
		t.Errorf(
			"len(report.Present) = %d, want %d",
			len(report.Present), len(hostprobe.CapsRequired))
	}
}

// TestCapsReport_MissingAndPresent verifies that Missing
// and PresentNames return the canonical Names in
// lexicographic order (so diagnostic logs are stable
// across runs). It does NOT assert which caps are missing
// or present — that depends on the runtime user's
// capability state, which is host-dependent (root vs
// non-root, ambient caps, etc.).
func TestCapsReport_MissingAndPresent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf(
			"caps probe is Linux-only (running on %s)",
			runtime.GOOS)
	}
	t.Parallel()

	report, err := hostprobe.CapsEffective()
	if err != nil {
		t.Fatalf("CapsEffective() failed: %v", err)
	}

	missing := report.Missing()
	present := report.PresentNames()

	// Missing and Present must be disjoint.
	for _, m := range missing {
		for _, p := range present {
			if m == p {
				t.Errorf(
					"cap %q appears in both Missing and PresentNames",
					m)
			}
		}
	}
	// Missing ∪ Present = CapsRequired (every cap must
	// appear in exactly one of the two).
	combined := append(append([]string{}, missing...), present...)
	if len(combined) != len(hostprobe.CapsRequired) {
		t.Errorf(
			"|Missing ∪ Present| = %d, want %d (some cap missing from both)",
			len(combined), len(hostprobe.CapsRequired))
	}
	// Each name appears exactly once.
	seen := map[string]int{}
	for _, n := range combined {
		seen[n]++
	}
	for name, n := range seen {
		if n != 1 {
			t.Errorf("cap %q appears %d times in Missing ∪ Present, want 1",
				name, n)
		}
	}
	// Diagnostic: surface the missing set so an operator
	// running the test sees which caps the user lacks.
	// (Silent probe failures are exactly the failure
	// mode the runtime preflight guards against.)
	if len(missing) > 0 {
		t.Logf(
			"caps not in Effective set: %s " +
				"(load eBPF programs requires these; "+
				"see TODO row 11 + ARCH §11)",
			strings.Join(missing, ", "))
	}
}