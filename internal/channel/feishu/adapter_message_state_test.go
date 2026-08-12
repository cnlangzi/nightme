package feishu

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
)

// TestMapStateToFeishuEmoji locks in the mapping from agent.MessageState
// to Feishu's emoji_type strings (larkim NewEmoji builder inputs).
//
//   - Queued    → "OneSecond"  (⏳)
//   - Submitted → "OnIt"       (🔄)
//   - Done      → "DONE"       (✅)
//
// The "DONE" string is also reused by mapPromptStateToFeishuEmoji
// (PromptDone → "DONE") — same emoji, but the reaction target
// differs (Done on the user message vs PromptDone on the receipt
// card). Both are exercised by the same LRU dedup + append-only
// reactions contract at the MessageStateBus / PromptEndBus
// subscriber layer.
//
// F-XX (slash-command-reactions follow-up): MessageDone is the
// F-53 §8 follow-up value. It's the newest entry in this table
// and the one most likely to regress if someone deletes the case
// during a future F-53 cleanup pass. This test exists to catch
// that.
func TestMapStateToFeishuEmoji(t *testing.T) {
	cases := []struct {
		state agent.MessageState
		want  string
	}{
		{agent.MessageQueued, "OneSecond"},
		{agent.MessageSubmitted, "OnIt"},
		{agent.MessageDone, "DONE"},
		{agent.MessageDropped, ""},   // not mapped — silent drop is the contract
		{agent.MessageState(99), ""}, // unknown value → silent drop (forward-compatible)
	}
	for _, tc := range cases {
		if got := mapStateToFeishuEmoji(tc.state); got != tc.want {
			t.Errorf("mapStateToFeishuEmoji(%v) = %q; want %q", tc.state, got, tc.want)
		}
	}
}

// TestMapPromptStateToFeishuEmoji locks in the receipt-card-side
// mapping (chatsession.PromptState → Feishu emoji_type). Kept
// alongside TestMapStateToFeishuEmoji so both mappings regress in
// lock-step if anyone touches the emoji strings.
//
// The Done↔Done cross-mapping assertion below depends on both
// functions returning "DONE" for their respective Done values.
// If either side drifts, the user-message ✅ and the receipt-card
// ✅ would render as different emojis — the regression this test
// catches.
func TestMapPromptStateToFeishuEmoji(t *testing.T) {
	cases := []struct {
		state agentsession.PromptState
		want  string
	}{
		{agentsession.PromptRunning, "OnIt"},
		{agentsession.PromptDone, "DONE"},
	}
	for _, tc := range cases {
		if got := mapPromptStateToFeishuEmoji(tc.state); got != tc.want {
			t.Errorf("mapPromptStateToFeishuEmoji(%v) = %q; want %q", tc.state, got, tc.want)
		}
	}
}

// TestMapStateAndPrompt_DoneUseSameEmoji guards the visual
// invariant that user-message ✅ (MessageDone) and receipt-card ✅
// (PromptDone) use the same emoji_type string. The runtime side
// routes them to different reaction targets (user message vs
// receipt card), but visually they should look identical to the
// user. If the strings drift apart, the reaction would render
// differently across the two surfaces.
func TestMapStateAndPrompt_DoneUseSameEmoji(t *testing.T) {
	if fromMsg := mapStateToFeishuEmoji(agent.MessageDone); fromMsg != mapPromptStateToFeishuEmoji(agentsession.PromptDone) {
		t.Errorf("MessageDone emoji %q must equal PromptDone emoji; check both mapXToFeishuEmoji funcs",
			fromMsg)
	}
}
