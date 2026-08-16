//go:build !linux

// On non-Linux GOOS values, the tun package compiles down to a stub
// that fails Open with a clear error. The rest of the codebase
// (including the data path) compiles fine on darwin / windows for
// cross-platform builds; only a runtime call into Open returns an
// error, which is the right behaviour — there is no /dev/net/tun
// there.

package tun

import (
	"errors"
	"fmt"
	"runtime"

	contracttun "github.com/vukamecos/unverified/internal/contract/tun"
)

// errUnsupported is returned by Open on non-Linux builds.
var errUnsupported = fmt.Errorf("tun: /dev/net/tun is Linux-only (GOOS=%s); see docs/ARCH.md §1", runtime.GOOS)

// Open is unimplemented on non-Linux platforms.
func Open(name string) (contracttun.Device, error) {
	return nil, errors.Join(errors.New("tun: unsupported platform"), errUnsupported)
}