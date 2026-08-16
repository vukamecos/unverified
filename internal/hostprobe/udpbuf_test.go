package hostprobe_test

import (
	"errors"
	"runtime"
	"testing"

	"github.com/vukamecos/unverified/internal/hostprobe"
)

// TestRecommendedRmemMax_Stable pins the threshold so
// silent changes show up as a reviewable diff. The value
// is what TODO row 22 documents the operator should aim
// for when running a QUIC bulk sender; lowering it
// without an ADR would regress the recommendation.
func TestRecommendedRmemMax_Stable(t *testing.T) {
	t.Parallel()
	if got, want := hostprobe.RecommendedRmemMax, uint64(4<<20); got != want {
		t.Errorf("RecommendedRmemMax = %d, want %d", got, want)
	}
}

// TestUDPBufRmemMax_RunsWithoutPanic runs the probe and
// asserts the report parses. On Linux the path
// /proc/sys/net/core/rmem_max exists for every kernel
// version in support; a failure here would be a kernel
// regression or a chroot/sandbox that hides procfs.
//
// On non-Linux the test skips, matching the existing
// pattern in capsprobe_test.go.
func TestUDPBufRmemMax_RunsWithoutPanic(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf(
			"UDP buffer probe is Linux-only (running on %s); see udpbuf_other.go",
			runtime.GOOS)
	}
	t.Parallel()

	r, err := hostprobe.UDPBufRmemMax()
	if err != nil {
		t.Fatalf("UDPBufRmemMax() failed: %v", err)
	}
	if r == nil {
		t.Fatal("UDPBufRmemMax() returned nil report with nil err")
	}
	if r.Val == 0 {
		t.Errorf("UDPBufRmemMax().Val = 0; rmem_max was parsed as empty")
	}
	// MeetsRecommendation is Val >= RecommendedRmemMax by
	// construction; the test guards against a refactor
	// that drops the comparison.
	if r.MeetsRecommendation != (r.Val >= hostprobe.RecommendedRmemMax) {
		t.Errorf(
			"MeetsRecommendation = %v, want %v (Val=%d Recommended=%d)",
			r.MeetsRecommendation, r.Val >= hostprobe.RecommendedRmemMax,
			r.Val, hostprobe.RecommendedRmemMax)
	}
	t.Logf(
		"net.core.rmem_max = %d bytes (recommended ≥ %d); meetsRecommendation = %v",
		r.Val, hostprobe.RecommendedRmemMax, r.MeetsRecommendation)
}

// TestUDPBufUnreadableError_Message pins the typed error
// contract so callers can errors.As against it.
func TestUDPBufUnreadableError_Message(t *testing.T) {
	t.Parallel()
	e := &hostprobe.UDPBufUnreadableError{
		Path: "/proc/sys/net/core/rmem_max",
		Err:  errors.New("synthetic"),
	}
	if got, want := e.Error(), "synthetic"; !contains(got, want) {
		t.Errorf("Error() = %q, want it to contain %q", got, want)
	}
	if e.Unwrap() == nil {
		t.Error("Unwrap() = nil, want non-nil underlying error")
	}
	if !errors.Is(e, e.Err) {
		t.Error("errors.Is(e, e.Err) = false, want true")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
