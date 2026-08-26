package gtw

import (
	"strings"
	"testing"
)

// TestRenderFixSuccessCard_PlanMode_HintWording pins the F-XX
// Plan-mode success card: standard "✅ Fix #N ready" header
// (no "(direct execute)" suffix), "agent is analyzing"
// trailing hint, NO Execute-mode phrasing.
func TestRenderFixSuccessCard_PlanMode_HintWording(t *testing.T) {
	issue := &Issue{ID: 42, Title: "x"}
	card := renderFixSuccessCard(issue, "br", "/wt", "o/r", "abc1234abcd", DispatchPlan, false /* reentry */)
	for _, want := range []string{
		"✅ Fix #42 ready",
		"→ branch:   `br`",
		"→ worktree: /wt",
		"→ issue:    o/r#42 [nightme/wip]",
		"→ base:     abc1234abcd",
		"↳ agent is analyzing",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("Plan-mode success card missing %q; got:\n%s", want, card)
		}
	}
	if strings.Contains(card, "(direct execute)") || strings.Contains(card, "agent is fixing now") {
		t.Errorf("Plan-mode success card must NOT carry Execute phrasing; got:\n%s", card)
	}
}

// TestRenderFixSuccessCard_ExecuteMode_HintWording pins the
// F-XX Execute-mode success card: "(direct execute)" suffix
// on header, "agent is fixing now" trailing hint with the
// follow-up commit/push commands.
func TestRenderFixSuccessCard_ExecuteMode_HintWording(t *testing.T) {
	issue := &Issue{ID: 42, Title: "x"}
	card := renderFixSuccessCard(issue, "br", "/wt", "o/r", "abc1234abcd", DispatchExecute, false /* reentry */)
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
		t.Errorf("Execute-mode success card must NOT carry Plan phrasing; got:\n%s", card)
	}
}

// TestRenderFixSuccessCard_PlanMode_EmptyBaseSHA pins the
// empty-baseSHA branch (daemon-recovery re-entry where
// RefreshDefaultBranch was skipped). The "→ base:" line
// must be omitted.
func TestRenderFixSuccessCard_PlanMode_EmptyBaseSHA(t *testing.T) {
	issue := &Issue{ID: 1, Title: "t"}
	card := renderFixSuccessCard(issue, "br", "/wt", "o/r", "" /* no base */, DispatchPlan, false /* reentry */)
	if strings.Contains(card, "→ base:") {
		t.Errorf("Plan-mode success card with empty baseSHA must omit '→ base:' line; got:\n%s", card)
	}
	for _, want := range []string{"✅ Fix #1 ready", "→ branch:   `br`", "↳ agent is analyzing"} {
		if !strings.Contains(card, want) {
			t.Errorf("Plan-mode card missing %q; got:\n%s", want, card)
		}
	}
}

// TestRenderFixSuccessCard_ExecuteMode_EmptyBaseSHA pins the
// empty-baseSHA branch for the Execute-mode card variant.
func TestRenderFixSuccessCard_ExecuteMode_EmptyBaseSHA(t *testing.T) {
	issue := &Issue{ID: 1, Title: "t"}
	card := renderFixSuccessCard(issue, "br", "/wt", "o/r", "" /* no base */, DispatchExecute, false /* reentry */)
	if strings.Contains(card, "→ base:") {
		t.Errorf("Execute-mode success card with empty baseSHA must omit '→ base:' line; got:\n%s", card)
	}
	for _, want := range []string{"✅ Fix #1 ready (direct execute)", "→ branch:   `br`", "↳ agent is fixing now"} {
		if !strings.Contains(card, want) {
			t.Errorf("Execute-mode card missing %q; got:\n%s", want, card)
		}
	}
}

// TestRenderFixSuccessCard_Reentry pins the F-XX re-entry
// path (daemon restart while the worktree + branch still
// exist): the success card must be mode-neutral regardless
// of the original dispatch mode. We don't know whether the
// previous /gtw fix sent a Plan or Execute prompt, and we
// aren't re-prompting — so neither "agent is analyzing" nor
// "agent is fixing now" is honest. The header also drops the
// "(direct execute)" suffix because we don't know which
// mode the previous dispatch used.
func TestRenderFixSuccessCard_Reentry(t *testing.T) {
	issue := &Issue{ID: 42, Title: "x"}

	// Reentry with DispatchPlan (the default we pass when we
	// don't know the original mode).
	card := renderFixSuccessCard(issue, "br", "/wt", "o/r", "abc1234abcd", DispatchPlan, true /* reentry */)
	for _, want := range []string{
		"✅ Fix #42 ready", // no (direct execute) suffix
		"→ branch:   `br`",
		"↳ worktree resumed",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("re-entry Plan card missing %q; got:\n%s", want, card)
		}
	}
	for _, forbid := range []string{
		"agent is analyzing",
		"agent is fixing now",
		"(direct execute)",
	} {
		if strings.Contains(card, forbid) {
			t.Errorf("re-entry Plan card must not contain %q; got:\n%s", forbid, card)
		}
	}

	// Reentry with DispatchExecute — even if the original
	// mode was Execute, the re-entry card stays neutral.
	card2 := renderFixSuccessCard(issue, "br", "/wt", "o/r", "abc1234abcd", DispatchExecute, true /* reentry */)
	if !strings.Contains(card2, "↳ worktree resumed") {
		t.Errorf("re-entry Execute card missing neutral hint; got:\n%s", card2)
	}
	if strings.Contains(card2, "(direct execute)") || strings.Contains(card2, "agent is fixing now") {
		t.Errorf("re-entry Execute card must not claim Execute state; got:\n%s", card2)
	}
}
