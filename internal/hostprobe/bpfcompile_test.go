package hostprobe_test

import (
	"errors"
	"os"
	"testing"

	"github.com/vukamecos/unverified/internal/hostprobe"
)

// TestCOReProbe_HostToolchain runs the scriptable CO-RE
// compile probe for TODO row 10b. On hosts without clang +
// llvm-objdump the probe returns a *hostprobe.ClangNotFoundError
// and the test t.Skip()s — the absence of the toolchain
// does not break the gate, it just surfaces the dep as
// missing in TODO row 10a (operator action). On hosts with
// the toolchain the probe must succeed end-to-end and the
// compiled `.o` must contain a `.BTF` section.
func TestCOReProbe_HostToolchain(t *testing.T) {
	t.Parallel()

	res, err := hostprobe.COReProbe()
	if err != nil {
		var cfne *hostprobe.ClangNotFoundError
		if errors.As(err, &cfne) {
			t.Skipf(
				"host lacks clang/llvm-objdump (%v); install via "+
					"TODO row 10a (sudo apt install clang "+
					"libbpf-dev llvm)",
				err)
		}
		t.Fatalf("COReProbe failed: %v", err)
	}
	defer func() { _ = os.Remove(res.ObjectPath) }()

	if res.ObjectPath == "" {
		t.Fatal("ProbeResult.ObjectPath is empty after successful probe")
	}
	if res.ClangVersion == "" {
		t.Error("ProbeResult.ClangVersion is empty (clang --version emitted no output)")
	}
	if res.ObjDumpBTFLine == "" {
		t.Fatal("ProbeResult.ObjDumpBTFLine is empty after .BTF section check passed")
	}

	// The .BTF line must start with `.BTF` (with a trailing
	// flag column — size / VMA / LMA / file off / aligment,
	// but the column separator is whitespace-agnostic so
	// `strings.HasPrefix` on the trimmed line is sufficient).
	if len(res.ObjDumpBTFLine) < 4 || res.ObjDumpBTFLine[:4] != ".BTF" {
		t.Errorf(
			"ObjDumpBTFLine = %q, want prefix .BTF",
			res.ObjDumpBTFLine)
	}
}

// TestClangNotFoundError_Message confirms the typed error's
// Error() / Unwrap() contract — Unwrap must surface the
// underlying exec.LookPath error so callers can errors.Is
// against exec.ErrNotFound, and the message must reference
// the TODO row 10a install path so the diagnostic is
// operator-actionable.
func TestClangNotFoundError_Message(t *testing.T) {
	t.Parallel()

	base := &hostprobe.ClangNotFoundError{Path: "clang-fake"}
	if got := base.Error(); got == "" {
		t.Fatal("ClangNotFoundError.Error() returned empty string")
	}

	baseNoPath := &hostprobe.ClangNotFoundError{}
	if got := baseNoPath.Error(); got == "" {
		t.Fatal("ClangNotFoundError.Error() returned empty string (no Path)")
	}

	// Unwrap with nil Err returns nil — that's a valid
	// expression of "no underlying cause".
	if err := base.Unwrap(); err != nil {
		t.Errorf(
			"Unwrap() = %v, want nil when Err is nil",
			err)
	}
}