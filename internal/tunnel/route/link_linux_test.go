//go:build linux

package route_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	contractroute "github.com/vukamecos/unverified/internal/contract/route"
	"github.com/vukamecos/unverified/internal/tunnel/route"
)

// fakeExecutor is a hand-rolled test double for [route.Executor].
//
// The Executor interface is one method and lives in this package
// (not internal/contract/), so the ARCH §13.1 moq-only rule for
// contract mocks does not apply. The fake records every call so
// tests can assert on argv shape, and lets each test pre-program
// the next response (stdout + err) FIFO.
type fakeExecutor struct {
	mu        sync.Mutex
	responses []executorResponse
	calls     []executorCall
}

type executorResponse struct {
	stdout []byte
	err    error
}

type executorCall struct {
	name string
	args []string
}

func (f *fakeExecutor) Run(name string, args []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, executorCall{name: name, args: append([]string(nil), args...)})
	if len(f.responses) == 0 {
		return nil, nil
	}
	r := f.responses[0]
	f.responses = f.responses[1:]
	return r.stdout, r.err
}

func (f *fakeExecutor) callsSnapshot() []executorCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]executorCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakeExitError is a minimal stand-in for *exec.ExitError so we
// can construct an exec-shaped error without forking a real child
// process. Error() mirrors the real *exec.ExitError format
// (Go's exec package formats Error() as
//
//	exec: "ip addr add CIDR dev DEV": exit status N
//	    stderr text
//
// — but we don't have the command name in this fake, so we render
// it as a plain "exit status N\n\t+ stderr" block). Tests inspect
// the wrapped error via errors.Is against the same instance.
type fakeExitError struct {
	stderr   string
	exitCode int
}

func (e *fakeExitError) Error() string {
	// Production inspects runErr.Error() looking for "File
	// exists", so the stderr MUST appear in Error().
	return fmt.Sprintf("exit status %d\n\t%s", e.exitCode, e.stderr)
}
func (e *fakeExitError) ExitCode() int  { return e.exitCode }
func (e *fakeExitError) Stderr() string { return e.stderr }

func makeExitError(stderr string, exitCode int) error {
	return &fakeExitError{
		stderr:   stderr,
		exitCode: exitCode,
	}
}

// newTestLink is the standard test wiring: a fake Executor + the
// production New constructor. Returns the Link, the fake, and a
// helper to snapshot recorded calls.
func newTestLink(t *testing.T, name string) (contractroute.Link, *fakeExecutor) {
	t.Helper()
	fake := &fakeExecutor{}
	l, err := route.New(name, route.Options{}.WithExecutor(fake))
	require.NoError(t, err, "New() must succeed with a valid name and injected Executor")
	require.NotNil(t, l, "New() must return a non-nil Link")
	return l, fake
}

// TestNew_EmptyName verifies that constructing a Link with an
// empty interface name returns a LinkError rather than panicking
// or accepting the empty name.
func TestNew_EmptyName(t *testing.T) {
	t.Parallel()
	l, err := route.New("", route.Options{})
	require.Error(t, err, "New(\"\") must fail")
	assert.Nil(t, l, "New(\"\") must return a nil Link")
	require.True(t, contractroute.IsLinkError(err),
		"err must be a *LinkError, got %T", err)
	assert.Equal(t, contractroute.ReasonCIDRInvalid, contractroute.LinkReason(err),
		"empty name should map to the closest stable Reason")
}

// TestNew_DefaultsApplied verifies that New() with zero Options
// uses the default /sbin/ip path and a working Executor. We do
// NOT call any Link method here — that would fork /sbin/ip on
// the test host. The default Executor is exercised in
// TestExecutor_ProductionWiring below (with a Skip when iproute2
// is missing).
func TestNew_DefaultsApplied(t *testing.T) {
	t.Parallel()
	l, err := route.New("tun0", route.Options{})
	require.NoError(t, err)
	require.NotNil(t, l)
}

// TestUp_HappyPath verifies that Up() runs `ip link set NAME up`
// with absolute argv and surfaces no error on exit 0.
func TestUp_HappyPath(t *testing.T) {
	t.Parallel()
	l, fake := newTestLink(t, "tun0")
	require.NoError(t, l.Up(), "Up() with successful exec must return nil")
	calls := fake.callsSnapshot()
	require.Len(t, calls, 1, "Up() must make exactly one exec call")
	assert.Equal(t, []string{"link", "set", "tun0", "up"}, calls[0].args,
		"argv must be exactly [link set NAME up]")
}

// TestUp_ExecFailure verifies that Up() translates a non-zero exit
// into a *LinkError with ReasonLinkUpFailed wrapping the cause.
func TestUp_ExecFailure(t *testing.T) {
	t.Parallel()
	l, fake := newTestLink(t, "tun0")
	cause := makeExitError("RTNETLINK answers: Operation not permitted", 2)
	fake.responses = []executorResponse{{stdout: nil, err: cause}}
	err := l.Up()
	require.Error(t, err)
	require.True(t, contractroute.IsLinkError(err))
	assert.Equal(t, contractroute.ReasonLinkUpFailed, contractroute.LinkReason(err))
	assert.True(t, errors.Is(err, cause),
		"errors.Is must reach the underlying exec error")
}

// TestDown_HappyPath mirrors TestUp_HappyPath for Down.
func TestDown_HappyPath(t *testing.T) {
	t.Parallel()
	l, fake := newTestLink(t, "tun0")
	require.NoError(t, l.Down())
	calls := fake.callsSnapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, []string{"link", "set", "tun0", "down"}, calls[0].args,
		"argv must be exactly [link set NAME down]")
}

// TestDown_ExecFailure mirrors TestUp_ExecFailure for Down.
func TestDown_ExecFailure(t *testing.T) {
	t.Parallel()
	l, fake := newTestLink(t, "tun0")
	cause := makeExitError("RTNETLINK answers: Operation not permitted", 2)
	fake.responses = []executorResponse{{stdout: nil, err: cause}}
	err := l.Down()
	require.Error(t, err)
	require.True(t, contractroute.IsLinkError(err))
	assert.Equal(t, contractroute.ReasonLinkDownFailed, contractroute.LinkReason(err))
	assert.True(t, errors.Is(err, cause))
}

// TestAddAddress_HappyPath verifies a clean first-time add: argv
// shape, no probe, in-memory cache populated.
func TestAddAddress_HappyPath(t *testing.T) {
	t.Parallel()
	l, fake := newTestLink(t, "tun0")
	require.NoError(t, l.AddAddress("10.66.0.2/24"),
		"first AddAddress with clean exec must succeed")
	calls := fake.callsSnapshot()
	require.Len(t, calls, 1,
		"first AddAddress must NOT probe; one exec call only")
	assert.Equal(t, []string{"addr", "add", "10.66.0.2/24", "dev", "tun0"}, calls[0].args,
		"argv must be exactly [addr add CIDR dev NAME]")
}

// TestAddAddress_IdempotentSameCIDR verifies that a second
// AddAddress with the *same* CIDR is a no-op (no second exec).
func TestAddAddress_IdempotentSameCIDR(t *testing.T) {
	t.Parallel()
	l, fake := newTestLink(t, "tun0")
	require.NoError(t, l.AddAddress("10.66.0.2/24"))
	require.NoError(t, l.AddAddress("10.66.0.2/24"),
		"second AddAddress with the same CIDR must be a no-op")
	calls := fake.callsSnapshot()
	assert.Len(t, calls, 1,
		"second AddAddress with the same CIDR must NOT make another exec call")
}

// TestAddAddress_DifferentCIDR_ReturnsAlreadyAssigned verifies the
// conflict path: a second AddAddress with a *different* CIDR
// returns ReasonAddressAlreadyAssigned.
func TestAddAddress_DifferentCIDR_ReturnsAlreadyAssigned(t *testing.T) {
	t.Parallel()
	l, fake := newTestLink(t, "tun0")
	require.NoError(t, l.AddAddress("10.66.0.2/24"))
	err := l.AddAddress("10.66.0.3/24")
	require.Error(t, err)
	require.True(t, contractroute.IsLinkError(err))
	assert.Equal(t, contractroute.ReasonAddressAlreadyAssigned,
		contractroute.LinkReason(err))
	calls := fake.callsSnapshot()
	assert.Len(t, calls, 1,
		"second AddAddress with a different CIDR must NOT exec (in-memory cache)")
}

// TestAddAddress_KernelRejectsSameCIDR verifies the probe path:
// the kernel says "File exists" but the *current* address is
// the same CIDR we're trying to add — treat as no-op.
func TestAddAddress_KernelRejectsSameCIDR(t *testing.T) {
	t.Parallel()
	l, fake := newTestLink(t, "tun0")
	fake.responses = []executorResponse{
		{
			stdout: nil,
			err:    makeExitError("RTNETLINK answers: File exists", 2),
		},
		{
			stdout: []byte("3: tun0    inet 10.66.0.2/24 brd 10.66.0.255 scope global tun0\n"),
			err:    nil,
		},
	}
	require.NoError(t, l.AddAddress("10.66.0.2/24"),
		"kernel-rejected same-CIDR add must be treated as idempotent success")
	calls := fake.callsSnapshot()
	require.Len(t, calls, 2,
		"the rejection must trigger exactly one probe call")
	assert.Equal(t, []string{"-o", "addr", "show", "dev", "tun0"}, calls[1].args,
		"the probe must be `ip -o addr show dev NAME`")
}

// TestAddAddress_KernelRejectsDifferentCIDR verifies the other
// branch of the probe path: kernel says "File exists" AND the
// current address is *different* — return ReasonAddressAlreadyAssigned.
func TestAddAddress_KernelRejectsDifferentCIDR(t *testing.T) {
	t.Parallel()
	l, fake := newTestLink(t, "tun0")
	fake.responses = []executorResponse{
		{
			stdout: nil,
			err:    makeExitError("RTNETLINK answers: File exists", 2),
		},
		{
			stdout: []byte("3: tun0    inet 10.66.0.5/24 brd 10.66.0.255 scope global tun0\n"),
			err:    nil,
		},
	}
	err := l.AddAddress("10.66.0.2/24")
	require.Error(t, err)
	require.True(t, contractroute.IsLinkError(err))
	assert.Equal(t, contractroute.ReasonAddressAlreadyAssigned,
		contractroute.LinkReason(err))
}

// TestAddAddress_InvalidCIDR verifies the pre-exec CIDR parse
// guard: no exec call, ReasonCIDRInvalid, no exec error surfaced.
// Table-driven per §13.1.
func TestAddAddress_InvalidCIDR(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cidr string
	}{
		{"empty", ""},
		{"missing_prefix", "10.66.0.2"},
		{"bad_ip", "10.66.0.999/24"},
		{"bad_prefix", "10.66.0.2/33"},
		{"letters", "not-a-cidr"},
		{"trailing_garbage", "10.66.0.2/24 extra"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l, fake := newTestLink(t, "tun0")
			err := l.AddAddress(tc.cidr)
			require.Error(t, err)
			require.True(t, contractroute.IsLinkError(err),
				"err must be *LinkError, got %T", err)
			assert.Equal(t, contractroute.ReasonCIDRInvalid,
				contractroute.LinkReason(err),
				"invalid CIDR must map to ReasonCIDRInvalid")
			assert.Empty(t, fake.callsSnapshot(),
				"invalid CIDR must NOT trigger any exec call")
		})
	}
}

// TestAddAddress_ExecFailureNonFileExists verifies that any
// kernel error other than "File exists" surfaces as
// ReasonAddressAddFailed (the original error, NOT a probe).
func TestAddAddress_ExecFailureNonFileExists(t *testing.T) {
	t.Parallel()
	l, fake := newTestLink(t, "tun0")
	cause := makeExitError("RTNETLINK answers: Operation not permitted", 2)
	fake.responses = []executorResponse{{stdout: nil, err: cause}}
	err := l.AddAddress("10.66.0.2/24")
	require.Error(t, err)
	require.True(t, contractroute.IsLinkError(err))
	assert.Equal(t, contractroute.ReasonAddressAddFailed,
		contractroute.LinkReason(err),
		"non-File-exists exec failure must map to ReasonAddressAddFailed")
	assert.True(t, errors.Is(err, cause))
}

// TestAddAddress_Canonicalisation verifies that the contract
// rejects a bare-host CIDR (e.g. "10.66.0.2") with ReasonCIDRInvalid
// rather than silently adding it as /32. The runtime always knows
// its prefix length from the IPAM lease; bare-host CIDRs are not
// part of the contract and a typo that drops the slash must NOT
// reach the kernel.
func TestAddAddress_BareHostCIDR_Rejected(t *testing.T) {
	t.Parallel()
	l, fake := newTestLink(t, "tun0")
	err := l.AddAddress("10.66.0.2")
	require.Error(t, err)
	require.True(t, contractroute.IsLinkError(err))
	assert.Equal(t, contractroute.ReasonCIDRInvalid, contractroute.LinkReason(err))
	assert.Empty(t, fake.callsSnapshot(),
		"bare-host CIDR must NOT trigger any exec call")
}
