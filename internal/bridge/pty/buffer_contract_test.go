// Regression test for the pty bridge's events-buffer cap.
//
// pty's emit path is direct `a.events <- agent.AgentEvent{...}`
// inside readLoop (no select, no default drop) — the "no drop"
// contract is enforced by code structure rather than a wrapper. The
// behavioural regression coverage for that pattern lives in the pi
// and acp bridge tests (which DO use wrapper functions); here we
// pin the channel cap so a regression that lowers it is caught
// immediately.
//
// The send sites are inline in agent.go's readLoop; any future
// change that wraps one of those in a `select { ... default: drop }`
// would have to be caught at review time, since this package has
// no minimal in-process way to invoke the read loop without a real
// PTY child.
package pty

import "testing"

// TestSessionBufferSize_PinnedAt40960 locks in the events channel
// cap. 40960 was chosen as generous-but-bounded headroom; bump
// deliberately via this constant, never inline.
func TestSessionBufferSize_PinnedAt40960(t *testing.T) {
	const want = 40960
	if sessionBufferSize != want {
		t.Fatalf("sessionBufferSize = %d, want %d — regression: cap was lowered, events may drop under load", sessionBufferSize, want)
	}
}
