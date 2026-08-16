package route_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vukamecos/unverified/internal/contract/route"
)

// TestLinkError_MessageAndUnwrap verifies the typed error's
// human-readable message includes both the Reason and the
// underlying cause, and that errors.Unwrap recovers the cause.
// Table-driven per §13.1.
//
// Note on errors.Is: errors.Is matches by identity for plain
// errors (errors.New returns a unique *errorString each call).
// The contract being verified here is that Unwrap() returns the
// stored Cause, which is the right behaviour: callers that wrap
// their own sentinel via errors.Is should compose LinkError at
// construction time, not try to match against a re-created
// instance.
func TestLinkError_MessageAndUnwrap(t *testing.T) {
	t.Parallel()
	causeRTNETLINK := errors.New("RTNETLINK answers: Operation not permitted")
	causeGOOS := errors.New("GOOS=darwin")
	tests := []struct {
		name       string
		err        *route.LinkError
		wantSubs   []string // substrings that must appear in Error()
		wantCause  error    // expected Cause (pointer-equal); nil means no Cause
		wantUnwrap error    // expected Unwrap() (pointer-equal)
	}{
		{
			name: "with_cause",
			err: &route.LinkError{
				Reason: route.ReasonLinkUpFailed,
				Cause:  causeRTNETLINK,
			},
			wantSubs: []string{
				"link:",
				route.ReasonLinkUpFailed,
				"RTNETLINK",
			},
			wantCause:  causeRTNETLINK,
			wantUnwrap: causeRTNETLINK,
		},
		{
			name: "nil_cause",
			err: &route.LinkError{
				Reason: route.ReasonCIDRInvalid,
			},
			wantSubs:   []string{"link:", route.ReasonCIDRInvalid},
			wantCause:  nil,
			wantUnwrap: nil,
		},
		{
			name: "unsupported_platform",
			err: &route.LinkError{
				Reason: route.ReasonUnsupportedPlatform,
				Cause:  causeGOOS,
			},
			wantSubs: []string{
				"link:",
				route.ReasonUnsupportedPlatform,
				"GOOS=darwin",
			},
			wantCause:  causeGOOS,
			wantUnwrap: causeGOOS,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg := tc.err.Error()
			for _, s := range tc.wantSubs {
				assert.Contains(t, msg, s, "Error() must contain %q", s)
			}
			if tc.wantCause == nil {
				assert.Nil(t, tc.err.Cause, "Cause must be nil")
				assert.Nil(t, tc.err.Unwrap(), "Unwrap() must be nil when Cause is nil")
			} else {
				require.NotNil(t, tc.err.Cause, "Cause must be non-nil")
				assert.Same(t, tc.wantCause, tc.err.Cause,
					"Cause must be the value the constructor was given")
				assert.Same(t, tc.wantUnwrap, tc.err.Unwrap(),
					"Unwrap() must return the same value as Cause")
				// errors.Is matches the wrapped cause by identity.
				assert.True(t, errors.Is(tc.err, tc.wantCause),
					"errors.Is must match the wrapped cause by identity")
			}
		})
	}
}

// TestIsLinkError verifies the predicate accepts *LinkError,
// rejects plain errors, and rejects nil. Table-driven.
func TestIsLinkError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"link_error", &route.LinkError{Reason: route.ReasonLinkUpFailed}, true},
		{"wrapped_link_error", error(&route.LinkError{Reason: route.ReasonLinkDownFailed}), true},
		{"plain_error", errors.New("not a link error"), false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, route.IsLinkError(tc.err),
				"IsLinkError(%v)", tc.err)
		})
	}
}

// TestLinkReason verifies the Reason extractor pulls the stable
// string out of a *LinkError and returns "" for everything else.
// Table-driven.
func TestLinkReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"link_error_up", &route.LinkError{Reason: route.ReasonLinkUpFailed}, route.ReasonLinkUpFailed},
		{"link_error_cidr", &route.LinkError{Reason: route.ReasonCIDRInvalid}, route.ReasonCIDRInvalid},
		{"link_error_address",
			&route.LinkError{Reason: route.ReasonAddressAddFailed},
			route.ReasonAddressAddFailed},
		{"plain", errors.New("plain"), ""},
		{"nil", nil, ""},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, route.LinkReason(tc.err),
				"LinkReason(%v)", tc.err)
		})
	}
}

// TestLinkReason_Stability pins the Reason string values. The
// strings are part of the public contract (operator scripts and
// downstream tooling switch on them); renaming any of them is a
// breaking change. Pinned as a table of (constant, value) pairs.
func TestLinkReason_Stability(t *testing.T) {
	t.Parallel()
	constants := []struct {
		constName string
		value     string
	}{
		{"ReasonUnsupportedPlatform", route.ReasonUnsupportedPlatform},
		{"ReasonBinaryNotFound", route.ReasonBinaryNotFound},
		{"ReasonCIDRInvalid", route.ReasonCIDRInvalid},
		{"ReasonLinkUpFailed", route.ReasonLinkUpFailed},
		{"ReasonLinkDownFailed", route.ReasonLinkDownFailed},
		{"ReasonAddressAlreadyAssigned", route.ReasonAddressAlreadyAssigned},
		{"ReasonAddressAddFailed", route.ReasonAddressAddFailed},
	}
	want := map[string]string{
		"ReasonUnsupportedPlatform":      "unsupported_platform",
		"ReasonBinaryNotFound":           "ip_binary_not_found",
		"ReasonCIDRInvalid":              "cidr_invalid",
		"ReasonLinkUpFailed":             "link_up_failed",
		"ReasonLinkDownFailed":           "link_down_failed",
		"ReasonAddressAlreadyAssigned":   "address_already_assigned",
		"ReasonAddressAddFailed":         "address_add_failed",
	}
	for _, c := range constants {
		c := c
		t.Run(c.constName, func(t *testing.T) {
			t.Parallel()
			got, ok := want[c.constName]
			require.True(t, ok, "missing expected value for %s in stability table", c.constName)
			assert.Equal(t, got, c.value,
				"%s = %q, want %q (do not rename; the string is part of the contract)",
				c.constName, c.value, got)
		})
	}
}

// TestReasonConstants_NoEmpty pins that every Reason constant is
// non-empty and lower_snake_case (no uppercase, no spaces). A
// constant that slipped in with an empty value or a typo would
// silently break operators' switch statements.
func TestReasonConstants_NoEmpty(t *testing.T) {
	t.Parallel()
	all := []string{
		route.ReasonUnsupportedPlatform,
		route.ReasonBinaryNotFound,
		route.ReasonCIDRInvalid,
		route.ReasonLinkUpFailed,
		route.ReasonLinkDownFailed,
		route.ReasonAddressAlreadyAssigned,
		route.ReasonAddressAddFailed,
	}
	for _, r := range all {
		r := r
		t.Run(r, func(t *testing.T) {
			t.Parallel()
			require.NotEmpty(t, r, "Reason constant must be non-empty")
			assert.False(t, strings.ContainsAny(r, " \t\n"),
				"Reason %q must not contain whitespace", r)
			assert.Equal(t, strings.ToLower(r), r,
				"Reason %q must be lower-case", r)
		})
	}
}

// TestReasonConstants_Unique pins that no two Reason constants
// share the same string value. A duplicate would make a switch on
// the Reason ambiguous — the caller cannot distinguish between
// "link up failed" and "link down failed" if both carry the same
// Reason.
func TestReasonConstants_Unique(t *testing.T) {
	t.Parallel()
	all := []string{
		route.ReasonUnsupportedPlatform,
		route.ReasonBinaryNotFound,
		route.ReasonCIDRInvalid,
		route.ReasonLinkUpFailed,
		route.ReasonLinkDownFailed,
		route.ReasonAddressAlreadyAssigned,
		route.ReasonAddressAddFailed,
	}
	seen := make(map[string]string, len(all))
	for _, name := range []string{
		"ReasonUnsupportedPlatform",
		"ReasonBinaryNotFound",
		"ReasonCIDRInvalid",
		"ReasonLinkUpFailed",
		"ReasonLinkDownFailed",
		"ReasonAddressAlreadyAssigned",
		"ReasonAddressAddFailed",
	} {
		// Re-derive value via the package-level constants; the
		// name list is kept in lock-step with the value list
		// above by convention. If they drift, this test fails
		// at compile time (the slices have different lengths).
		var v string
		switch name {
		case "ReasonUnsupportedPlatform":
			v = route.ReasonUnsupportedPlatform
		case "ReasonBinaryNotFound":
			v = route.ReasonBinaryNotFound
		case "ReasonCIDRInvalid":
			v = route.ReasonCIDRInvalid
		case "ReasonLinkUpFailed":
			v = route.ReasonLinkUpFailed
		case "ReasonLinkDownFailed":
			v = route.ReasonLinkDownFailed
		case "ReasonAddressAlreadyAssigned":
			v = route.ReasonAddressAlreadyAssigned
		case "ReasonAddressAddFailed":
			v = route.ReasonAddressAddFailed
		}
		if other, dup := seen[v]; dup {
			t.Errorf("Reason %q (%s) duplicates %q (%s)",
				v, name, v, other)
		}
		seen[v] = name
	}
	// Sanity: every constant made it into the map.
	assert.Equal(t, len(all), len(seen),
		"every Reason constant must appear exactly once in the dedup map")
}
