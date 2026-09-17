package discord

import (
	"testing"
	"time"
)

// TestNextBackoff_DoublesUntilCap verifies the backoff doubling
// curve. The run loop resets to 500ms on every clean disconnect —
// the cap behaviour is what's worth locking in here, since a
// runaway backoff that never resets is the worst-case failure
// mode for a long-lived daemon.
func TestNextBackoff_DoublesUntilCap(t *testing.T) {
	const max = 30 * time.Second
	got := 500 * time.Millisecond
	want := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 16 * time.Second, 30 * time.Second, // cap
		30 * time.Second, 30 * time.Second,
	}
	for i, w := range want {
		got = nextBackoff(got, max)
		if got != w {
			t.Errorf("step %d: got %v, want %v", i, got, w)
		}
	}
}

// TestNextBackoff_ZeroCap covers the (mis-)configured case
// where max is zero — we want a sane ceiling rather than an
// overflow. The policy is "cap at max, even if max is 0".
func TestNextBackoff_ZeroCap(t *testing.T) {
	got := nextBackoff(500*time.Millisecond, 0)
	if got != 0 {
		t.Errorf("got %v, want 0 (zero cap)", got)
	}
}
