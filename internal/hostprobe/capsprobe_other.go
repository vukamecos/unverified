//go:build !linux

package hostprobe

// CapsRequired is the canonical capability set per ARCH §11.
// On non-Linux platforms (cross-compile to darwin/windows/etc.)
// the Bit field is not populated — the probe is a no-op there.
// The probe package keeps the surface uniform so contract
// tests on the runtime can import the slice without a build
// tag.
var CapsRequired = []Cap{
	{Name: "CAP_NET_ADMIN"},
	{Name: "CAP_BPF"},
	{Name: "CAP_PERFMON"},
}

// Cap is one Linux capability the host probe can check.
// The Bit field is 0 on non-Linux platforms — the
// capsprobe_other.go surface is here only to keep the
// `go build` green on darwin/windows for cross-compile.
type Cap struct {
	Name string
	Bit  int
}

// CapsReport is the no-op stub on non-Linux. The probe
// runtime path returns UnsupportedPlatformError so callers
// can errors.Is against it.
type CapsReport struct {
	Present []bool
}

// CapsEffective is a no-op on non-Linux. Returns an
// *UnsupportedPlatformError so callers can `t.Skip` or
// fail-closed depending on their semantics.
func CapsEffective() (*CapsReport, error) {
	return nil, &UnsupportedPlatformError{}
}

// Missing on non-Linux is a no-op (returns empty slice).
func (r *CapsReport) Missing() []string { return nil }

// PresentNames on non-Linux is a no-op (returns empty slice).
func (r *CapsReport) PresentNames() []string { return nil }

// UnsupportedPlatformError signals that the host probe
// does not apply on the current OS. Cross-compile uses
// this to keep the package surface uniform without
// runtime support.
type UnsupportedPlatformError struct{}

func (e *UnsupportedPlatformError) Error() string {
	return "hostprobe: caps probe is Linux-only (cross-compile stub)"
}