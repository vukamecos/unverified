//go:build !linux

// On non-Linux GOOS values, the route package compiles down to a
// stub that fails every [contractroute.Link] method with
// ReasonUnsupportedPlatform. There is no iproute2 binary to shell
// out to and no netlink-equivalent userspace tool on the supported
// platforms.
//
// Cross-compile stays green (the contract tests in
// internal/contract/route pass on every GOOS); only a runtime
// call into New / Up / Down / AddAddress returns an error, which
// is the right behaviour — there is nothing to configure on
// non-Linux.

package route

import (
	"fmt"
	"runtime"

	contractroute "github.com/vukamecos/unverified/internal/contract/route"
)

// New is unimplemented on non-Linux platforms. It returns a
// [contractroute.LinkError] with ReasonUnsupportedPlatform so the
// caller can branch on the stable Reason without parsing the
// message.
func New(_ string, _ Options) (contractroute.Link, error) {
	return nil, &contractroute.LinkError{
		Reason: contractroute.ReasonUnsupportedPlatform,
		Cause:  fmt.Errorf("route: iproute2 is Linux-only (GOOS=%s); see docs/ARCH.md §1", runtime.GOOS),
	}
}
