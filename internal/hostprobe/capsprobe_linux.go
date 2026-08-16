//go:build linux

package hostprobe

import (
	"fmt"
	"sort"

	"golang.org/x/sys/unix"
)

// CapsRequired is the canonical set of Linux capabilities the
// runtime needs to load eBPF programs and operate the TUN
// device, per ARCH §11 / TODO row 11:
//
//   - CAP_NET_ADMIN — TUN ioctl + ip route manipulation
//     (ADR 0003, ADR 0006).
//   - CAP_BPF       — load BPF programs on Linux ≥ 5.8
//     (split out of CAP_SYS_ADMIN in 5.8; the runtime
//     probes and selects the right set per the
//     TODO row 11 carve-out).
//   - CAP_PERFMON   — required alongside CAP_BPF for
//     tracing BPF programs (perf event readers); not
//     strictly needed for the network-programming path
//     but listed here so the probe surface is uniform.
//
// On kernels < 5.8 the eBPF path needs CAP_SYS_ADMIN
// instead of CAP_BPF + CAP_PERFMON; the runtime's
// preflight selects the right set at startup (TODO
// row 11 + ARCH §11). The probe here reports whichever
// caps the caller asked about — the host-side check
// is independent of the runtime's version-aware
// selection.
var CapsRequired = []Cap{
	{Name: "CAP_NET_ADMIN", Bit: unix.CAP_NET_ADMIN},
	{Name: "CAP_BPF", Bit: unix.CAP_BPF},
	{Name: "CAP_PERFMON", Bit: unix.CAP_PERFMON},
}

// Cap is one Linux capability the host probe can check.
type Cap struct {
	// Name is the canonical capability constant name
	// (e.g. "CAP_NET_ADMIN"). Surfaced in errors.
	Name string
	// Bit is the kernel capability bit position
	// (e.g. unix.CAP_NET_ADMIN == 12). Surfaced in
	// errors and used to mask the right word of the
	// capget data array.
	Bit int
}

// CapsReport captures the result of a CapsEffective probe
// run. Present[i] tells whether CapsRequired[i] is in the
// Effective set of the current process; Effective[0..1]
// is the raw low/high word values for diagnostics.
//
// The runtime probe ([internal/tunnel/tun.defaultCapProbe])
// uses Effective&(1<<bit)==0 to fail closed; this host-side
// probe mirrors that surface so a future iter that runs the
// production daemon non-root can compare both reports.
type CapsReport struct {
	// Present[i] is true iff CapsRequired[i] is set in the
	// Effective set of the current process.
	Present []bool
	// Effective is the raw low/high word values returned
	// by capget, surfaced for the diagnostic log line.
	Effective [2]uint32
}

// CapsEffective runs capget(pid=0) on the current process
// and reports which of CapsRequired are set in the
// Effective set. The probe is read-only — it never modifies
// the process's capability state.
//
// On a non-root, capabilities-dropped process the report
// shows every cap as absent; the caller (a test or a
// preflight surface) decides whether that is acceptable.
// The runtime production daemon has not yet been run on
// this host so the present call is "everything absent
// except those the user inherits from the login session",
// which on a normal Ubuntu desktop is typically none.
func CapsEffective() (*CapsReport, error) {
	hdr := unix.CapUserHeader{
		Version: unix.LINUX_CAPABILITY_VERSION_3,
		Pid:     0, // 0 = current process
	}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return nil, fmt.Errorf("hostprobe: capget: %w", err)
	}
	report := &CapsReport{
		Present:   make([]bool, len(CapsRequired)),
		Effective: [2]uint32{data[0].Effective, data[1].Effective},
	}
	for i, c := range CapsRequired {
		word := c.Bit / 32
		if word > 1 {
			return nil, fmt.Errorf(
				"hostprobe: capability %s has bit %d, beyond 64",
				c.Name, c.Bit)
		}
		bit := uint32(c.Bit % 32)
		report.Present[i] = report.Effective[word]&(1<<bit) != 0
	}
	return report, nil
}

// Missing returns the canonical Names of every cap in the
// report that is *not* in the Effective set, sorted by the
// canonical CapsRequired ordering. Used by callers that
// want a stable diagnostic ("missing: CAP_BPF, CAP_PERFMON").
func (r *CapsReport) Missing() []string {
	var missing []string
	for i, c := range CapsRequired {
		if !r.Present[i] {
			missing = append(missing, c.Name)
		}
	}
	sort.Strings(missing)
	return missing
}

// Present returns the canonical Names of every cap in the
// report that *is* in the Effective set. Mirror of Missing
// for symmetry.
func (r *CapsReport) PresentNames() []string {
	var present []string
	for i, c := range CapsRequired {
		if r.Present[i] {
			present = append(present, c.Name)
		}
	}
	sort.Strings(present)
	return present
}