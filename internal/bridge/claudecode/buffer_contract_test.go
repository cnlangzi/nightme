//go:build !windows

// Regression test for the claudecode bridge's events-buffer cap.
//
// claudecode's emit path is direct `events <- ev` (no select, no
// default drop) — the "no drop" contract is enforced by code
// structure rather than a wrapper. The behavioural regression
// coverage for that pattern lives in the pi and acp bridge tests
// (which DO use wrapper functions); here we pin the channel cap so
// a regression that lowers it (or, more likely, drops the constant
// and inlines a smaller literal) is caught immediately.
//
// The send sites are in stream.go's translate() and a handful of
// inline emit points; any future change that wraps one of those in
// a `select { ... default: drop }` would have to be caught at
// review time, since this package has no minimal in-process way to
// invoke translate() without a real claude process.
package claudecode

import "testing"

// TestEventsBufferSize_PinnedAt40960 locks in the events channel
// cap. 40960 was chosen as generous-but-bounded headroom; bump
// deliberately via this constant, never inline.
func TestEventsBufferSize_PinnedAt40960(t *testing.T) {
	const want = 40960
	if eventsBufferSize != want {
		t.Fatalf("eventsBufferSize = %d, want %d — regression: cap was lowered, events may drop under load", eventsBufferSize, want)
	}
}