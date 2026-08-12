package agent

import "testing"

// TestMessageState_String covers the human-readable labels emitted by
// MessageState.String(). The labels surface in log lines and test
// diagnostics, so any drift here is observable.
//
// F-XX (slash-command-reactions follow-up): MessageDone is the new
// F-53 §8 follow-up value for the dispatcher-completion indicator.
// It MUST render as "done" — the same label v1.3 used before F-53
// physically deleted the value — so log lines across versions stay
// diff-friendly.
func TestMessageState_String(t *testing.T) {
	cases := []struct {
		state MessageState
		want  string
	}{
		{MessageQueued, "queued"},
		{MessageSubmitted, "submitted"},
		{MessageDropped, "dropped"},
		{MessageDone, "done"},
		{MessageState(99), "unknown"}, // out-of-range value defensively maps to "unknown"
	}
	for _, tc := range cases {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("MessageState(%d).String() = %q; want %q", int(tc.state), got, tc.want)
		}
	}
}

// TestMessageState_DistinctValues is a sanity guard for the const
// block. A future refactor that accidentally collapses two values
// to the same int would silently break the bus subscriber's
// different-state dedup. Locks the values in.
func TestMessageState_DistinctValues(t *testing.T) {
	seen := map[MessageState]string{}
	for _, v := range []MessageState{MessageQueued, MessageSubmitted, MessageDropped, MessageDone} {
		if _, dup := seen[v]; dup {
			t.Errorf("MessageState values collide at int %d", int(v))
		}
		seen[v] = v.String()
	}
	if len(seen) != 4 {
		t.Errorf("expected 4 distinct MessageState values, got %d", len(seen))
	}
}
