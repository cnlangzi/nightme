package discord

import (
	"testing"
	"time"
)

// TestIsTerminalCloseCode covers the legacy terminal-split
// classifier. Phase 3 widened the decision table with
// decideOnCloseCode; this test pins the Stop subset.
func TestIsTerminalCloseCode(t *testing.T) {
	terminal := []int{
		closeCodeAuthenticationFailed,
		closeCodeInvalidIntents,
		closeCodeDisallowedIntents,
	}
	for _, c := range terminal {
		if !isTerminalCloseCode(c) {
			t.Errorf("isTerminalCloseCode(%d) = false, want true", c)
		}
	}
	transient := []int{4000, 4001, 4003, 4007, 4008, 4009, 1006}
	for _, c := range transient {
		if isTerminalCloseCode(c) {
			t.Errorf("isTerminalCloseCode(%d) = true, want false (transient)", c)
		}
	}
}

func TestDescribeCloseCode(t *testing.T) {
	if got := describeCloseCode(closeCodeAuthenticationFailed); got == "" {
		t.Error("describeCloseCode returned empty for known code")
	}
	if got := describeCloseCode(9999); got != "" {
		t.Errorf("describeCloseCode returned %q for unknown code, want empty", got)
	}
}

// TestDecideOnCloseCode_TableDriven pins the full Phase 3
// close-code → decision table. The (code, expectedAction,
// expectedBackoffBucket) tuples mirror the matrix at
// /docs/topics/opcodes-and-status-codes.
func TestDecideOnCloseCode_TableDriven(t *testing.T) {
	cases := []struct {
		code      int
		want      reconnectAction
		backoffOK func(time.Duration) bool
	}{
		// Terminal — daemon must stop.
		{closeCodeAuthenticationFailed, reconnectActionStop, zeroBackoff},
		{closeCodeInvalidIntents, reconnectActionStop, zeroBackoff},
		{closeCodeDisallowedIntents, reconnectActionStop, zeroBackoff},
		// 4003 / 4007 — server-side state still valid; resume path.
		{4003, reconnectActionResume, nearBackoff(1 * time.Second)},
		{4007, reconnectActionReIdentify, nearBackoff(1 * time.Second)},
		// 4008 — rate-limited; long backoff.
		{4008, reconnectActionReconnect, nearBackoff(30 * time.Second)},
		// 4009 — session timed out; re-identify with short backoff.
		{4009, reconnectActionReIdentify, nearBackoff(5 * time.Second)},
		// Unknown codes — generic reconnect at the strategy floor.
		{4000, reconnectActionReconnect, nearBackoff(1 * time.Second)},
		{4001, reconnectActionReconnect, nearBackoff(1 * time.Second)},
		{4010, reconnectActionReconnect, nearBackoff(1 * time.Second)},
		{4011, reconnectActionReconnect, nearBackoff(1 * time.Second)},
		{9999, reconnectActionReconnect, nearBackoff(1 * time.Second)},
	}
	for _, tc := range cases {
		d := decideOnCloseCode(tc.code)
		if d.Action != tc.want {
			t.Errorf("code %d: Action = %v, want %v", tc.code, d.Action, tc.want)
		}
		if !tc.backoffOK(d.Backoff) {
			t.Errorf("code %d: Backoff = %v, out of bucket", tc.code, d.Backoff)
		}
		if d.Reason == "" {
			t.Errorf("code %d: Reason empty", tc.code)
		}
	}
}

// zeroBackoff returns true for a zero-duration backoff (terminal
// codes don't sleep before returning the error).
func zeroBackoff(d time.Duration) bool { return d == 0 }

// nearBackoff returns a predicate that accepts any backoff within
// ±1 s of the expected value. Avoids brittle second-exact asserts
// when the table deliberately rounds (4008's 30 s, 4009's 5 s).
func nearBackoff(want time.Duration) func(time.Duration) bool {
	delta := 1 * time.Second
	return func(got time.Duration) bool {
		d := got - want
		if d < 0 {
			d = -d
		}
		return d <= delta
	}
}

// TestReconnectAction_String pins the human-readable Action names
// used in log lines and HealthSnapshot diagnostics.
func TestReconnectAction_String(t *testing.T) {
	cases := []struct {
		a    reconnectAction
		want string
	}{
		{reconnectActionReconnect, "reconnect"},
		{reconnectActionResume, "resume"},
		{reconnectActionReIdentify, "reidentify"},
		{reconnectActionStop, "stop"},
		{reconnectAction(0), ""},
	}
	for _, tc := range cases {
		if got := tc.a.String(); got != tc.want {
			t.Errorf("reconnectAction(%d).String() = %q, want %q", tc.a, got, tc.want)
		}
	}
}
