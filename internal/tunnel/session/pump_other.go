//go:build !linux

// Package session on non-Linux GOOS returns a typed
// ReasonUnsupportedPlatform error from Run so cross-compile
// stays green. The runtime invariant — Pump runs on Linux only —
// is documented in ADR 0005 and the TODO §"Read IP packets
// from TUN, hand off to gRPC stream" sub-items.
package session

import (
	"context"

	contractsession "github.com/vukamecos/unverified/internal/contract/session"
	contracttun "github.com/vukamecos/unverified/internal/contract/tun"
	"github.com/vukamecos/unverified/internal/contract/transport"
)

// Options is a placeholder on non-Linux so callers can use the
// same construction sequence cross-platform without build tags
// at the call site.
type Options struct{}

// New returns a Pump that always returns ReasonUnsupportedPlatform
// from Run.
func New(_ Options) *Pump {
	return &Pump{}
}

// Pump is the non-Linux stub. Its Run method always returns a
// *contractsession.PumpError with ReasonUnsupportedPlatform.
type Pump struct{}

// Compile-time interface check on every GOOS — the contract
// shape is the same even if the implementation is a stub.
var _ contractsession.Pump = (*Pump)(nil)

// Run returns ReasonUnsupportedPlatform on every non-Linux GOOS.
// The arguments are accepted but never used.
func (p *Pump) Run(
	_ context.Context,
	_ contracttun.Device,
	_ transport.Tunnel,
) error {
	return &contractsession.PumpError{
		Reason: contractsession.ReasonUnsupportedPlatform,
		Op:     "startup",
	}
}
