//go:build !linux

package hostprobe

// RecommendedRmemMax mirrors the Linux constant so the
// runtime can import the symbol without a build tag.
const RecommendedRmemMax uint64 = 4 << 20 // 4 MiB

// UDPBufReport is the no-op stub on non-Linux. UDP buffer
// tuning is Linux-specific; on macOS/Windows the SO_RCVBUF
// default is set elsewhere.
type UDPBufReport struct {
	Val                uint64
	MeetsRecommendation bool
}

// UDPBufRmemMax is a no-op on non-Linux. Returns
// *UnsupportedPlatformError so callers can errors.Is /
// t.Skip against it. The runtime preflight on the
// supported path does not import this stub.
func UDPBufRmemMax() (*UDPBufReport, error) {
	return nil, &UnsupportedPlatformError{}
}
