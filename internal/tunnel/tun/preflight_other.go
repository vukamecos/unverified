//go:build !linux

package tun

import (
	"fmt"
	"runtime"

	contracttun "github.com/vukamecos/unverified/internal/contract/tun"
)

// DefaultPreflight returns a Preflight that always fails with
// ReasonUnsupportedPlatform. There is no /dev/net/tun to probe and
// no CAP_NET_ADMIN to check on non-Linux GOOS values.
func DefaultPreflight() contracttun.Preflight {
	return unsupportedPreflight{}
}

type unsupportedPreflight struct{}

func (unsupportedPreflight) Run() error {
	return &contracttun.PreflightError{
		Reason: contracttun.ReasonUnsupportedPlatform,
		Cause:  fmt.Errorf("tun preflight: /dev/net/tun is Linux-only (GOOS=%s)", runtime.GOOS),
	}
}