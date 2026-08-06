package gtw

import "testing"

func TestActionLookup_LiveActions(t *testing.T) {
	cases := []struct {
		action string
		want   ReactionKind
	}{
		{"act:/gtw/branch-newv2", ReactionNewV2},
		{"act:/gtw/branch-join", ReactionJoin},
		{"act:/gtw/worktree-retry", ReactionRetry},
		{"act:/gtw/cancel", ReactionCancel},
	}
	for _, tc := range cases {
		got, ok := ActionLookup(tc.action)
		if !ok {
			t.Errorf("ActionLookup(%q): want ok, got !ok", tc.action)
			continue
		}
		if got != tc.want {
			t.Errorf("ActionLookup(%q)=%q, want %q", tc.action, got, tc.want)
		}
	}
}

func TestActionLookup_RetiredOrUnknown(t *testing.T) {
	// label-force / worktree-cancel were placeholders/aliases and must
	// not resolve — keep the map equal to live card buttons only.
	retired := []string{
		"",
		"act:/gtw/label-force",
		"act:/gtw/worktree-cancel",
		"act:/gtw/ok", // scenario name ≠ button action
		"nav:/somewhere",
		"cmd:/gtw/fix",
	}
	for _, action := range retired {
		if _, ok := ActionLookup(action); ok {
			t.Errorf("ActionLookup(%q): want !ok for retired/unknown action", action)
		}
	}
}
