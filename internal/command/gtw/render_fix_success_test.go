package gtw

import (
	"strings"
	"testing"
)

// TestRenderFixSuccessCard_AssessMode_HintWording pins the F-XX
// Assess-mode success card: standard "✅ Fix #N ready" header
// (no "(direct execute)" suffix), "agent is analyzing"
// trailing hint, NO Execute-mode phrasing.
func TestRenderFixSuccessCard_AssessMode_HintWording(t *testing.T) {
	issue := &Issue{ID: 42, Title: "x"}
	card := renderFixSuccessCard(issue, "br", "/wt", "o/r", "abc1234abcd", DispatchAssess)
	for _, want := range []string{
		"✅ Fix #42 ready",
		"→ branch:   `br`",
		"→ worktree: /wt",
		"→ issue:    o/r#42 [nightme/wip]",
		"→ base:     abc1234abcd",
		"↳ agent is analyzing",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("Assess-mode success card missing %q; got:\n%s", want, card)
		}
	}
	if strings.Contains(card, "(direct execute)") || strings.Contains(card, "agent is fixing now") {
		t.Errorf("Assess-mode success card must NOT carry Execute phrasing; got:\n%s", card)
	}
}

// TestRenderFixSuccessCard_ExecuteMode_HintWording pins the
// F-XX Execute-mode success card: "(direct execute)" suffix
// on header, "agent is fixing now" trailing hint with the
// follow-up commit/push commands.
func TestRenderFixSuccessCard_ExecuteMode_HintWording(t *testing.T) {
	issue := &Issue{ID: 42, Title: "x"}
	card := renderFixSuccessCard(issue, "br", "/wt", "o/r", "abc1234abcd", DispatchExecute)
	for _, want := range []string{
		"✅ Fix #42 ready (direct execute)",
		"↳ agent is fixing now",
		"/gtw commit",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("Execute-mode success card missing %q; got:\n%s", want, card)
		}
	}
	if strings.Contains(card, "agent is analyzing") || strings.Contains(card, "review the plan in chat") {
		t.Errorf("Execute-mode success card must NOT carry Assess phrasing; got:\n%s", card)
	}
}

// TestRenderFixSuccessCard_AssessMode_EmptyBaseSHA pins the
// empty-baseSHA branch (F-59 / F-45 daemon-recovery scenario
// where RefreshDefaultBranch was skipped). The "→ base:" line
// must be omitted.
func TestRenderFixSuccessCard_AssessMode_EmptyBaseSHA(t *testing.T) {
	issue := &Issue{ID: 1, Title: "t"}
	card := renderFixSuccessCard(issue, "br", "/wt", "o/r", "" /* no base */, DispatchAssess)
	if strings.Contains(card, "→ base:") {
		t.Errorf("Assess-mode success card with empty baseSHA must omit '→ base:' line; got:\n%s", card)
	}
	for _, want := range []string{"✅ Fix #1 ready", "→ branch:   `br`", "↳ agent is analyzing"} {
		if !strings.Contains(card, want) {
			t.Errorf("Assess-mode card missing %q; got:\n%s", want, card)
		}
	}
}

// TestRenderFixSuccessCard_ExecuteMode_EmptyBaseSHA pins the
// empty-baseSHA branch for the Execute-mode card variant.
func TestRenderFixSuccessCard_ExecuteMode_EmptyBaseSHA(t *testing.T) {
	issue := &Issue{ID: 1, Title: "t"}
	card := renderFixSuccessCard(issue, "br", "/wt", "o/r", "" /* no base */, DispatchExecute)
	if strings.Contains(card, "→ base:") {
		t.Errorf("Execute-mode success card with empty baseSHA must omit '→ base:' line; got:\n%s", card)
	}
	for _, want := range []string{"✅ Fix #1 ready (direct execute)", "→ branch:   `br`", "↳ agent is fixing now"} {
		if !strings.Contains(card, want) {
			t.Errorf("Execute-mode card missing %q; got:\n%s", want, card)
		}
	}
}
