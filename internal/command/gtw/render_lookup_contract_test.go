package gtw_test

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/command/gtw"
	"github.com/cnlangzi/nightme/internal/messages"
)

// TestRenderActionLookupContract locks the contract between
// BranchExistsChoice / WorktreeFailChoice (the gtw card renderer) and
// messages.ActionLookup (the channel-side translator that turns
// Feishu card.action.trigger events into ReactionKind values).
//
// Each card emits one or more (Emoji, Action) pairs; every emitted
// Action must round-trip through ActionLookup to the ReactionKind
// whose string value equals the rendered Emoji. If a renderer
// change introduces a new button without updating ActionLookup
// (or vice versa), this test fails before the user can see
// "未知操作: ..." toasts.
//
// Lives in gtw_test (not messages_test) because:
//   - The renderer is gtw's responsibility; the contract drift
//     we're guarding against is renderer-driven.
//   - Putting the test in messages would force messages to import
//     gtw, recreating the dependency cycle this refactor removed.
func TestRenderActionLookupContract(t *testing.T) {
	cases := []struct {
		name string
		card gtw.Choice
	}{
		{
			name: "BranchExistsChoice",
			card: gtw.BranchExistsChoice(gtw.FixDraftPayload{
				Branch:  "feat/foo",
				IssueID: 42,
				Title:   "example issue",
			}, ""),
		},
		{
			name: "WorktreeFailChoice",
			card: gtw.WorktreeFailChoice(gtw.FixDraftPayload{
				Branch:   "feat/foo",
				IssueID:  -1, // local-mode
				GitError: "fatal: not a git repository",
			}),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.card.Options) == 0 {
				t.Fatalf("%s has no Options — choice rendered nothing to route", tc.name)
			}
			for _, choice := range tc.card.Options {
				kind, ok := messages.ActionLookup(choice.ID)
				if !ok {
					t.Errorf("%s emitted ID %q (Emoji %q) — ActionLookup has no mapping; user would see '未知操作' toast",
						tc.name, choice.ID, choice.Emoji)
					continue
				}
				if string(kind) != choice.Emoji {
					t.Errorf("%s emitted ID %q → kind %q, but the rendered Emoji is %q (must match so the reaction handler sees the same emoji path)",
						tc.name, choice.ID, kind, choice.Emoji)
				}
			}
		})
	}
}

// TestActionLookupUnknown ensures ActionLookup returns ok=false for
// arbitrary / hostile input. The channel adapter renders an
// "unknown action" toast when this happens, so an over-permissive
// lookup would silently misroute user clicks to the wrong draft
// handler instead of surfacing the error.
func TestActionLookupUnknown(t *testing.T) {
	for _, tag := range []string{
		"",
		"act:/gtw/",
		"act:/unknown",
		"random string",
		"ACT:/GTW/CANCEL",  // case-sensitive
		"act:/gtw/cancel ", // trailing space
	} {
		if _, ok := messages.ActionLookup(tag); ok {
			t.Errorf("ActionLookup(%q) returned ok=true; expected ok=false (whitelist is exact)", tag)
		}
	}
}
