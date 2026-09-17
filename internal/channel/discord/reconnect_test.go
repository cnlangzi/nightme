package discord

import "testing"

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
