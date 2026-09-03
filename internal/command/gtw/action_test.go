package gtw

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command/services"
	"github.com/cnlangzi/nightme/internal/messages"
)

// TestModeFromDraftPayload pins the Mode inference from a
// draft payload (action handlers call this when writing a
// new Context after a reaction succeeds).
func TestModeFromDraftPayload(t *testing.T) {
	cases := []struct {
		name string
		in   FixDraftPayload
		want Mode
	}{
		{"local -1", FixDraftPayload{IssueID: -1}, ModeLocal},
		{"remote positive", FixDraftPayload{IssueID: 42}, ModeRemote},
		{"legacy zero (treated as Remote)", FixDraftPayload{IssueID: 0}, ModeRemote},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ModeFromDraftPayload(tc.in); got != tc.want {
				t.Errorf("ModeFromDraftPayload(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCancelResultText covers the local/remote branch
// differentiation in the cancel-reply text.
func TestCancelResultText(t *testing.T) {
	if got := cancelResultText(FixDraftPayload{IssueID: -1}); got != "❌ Cancelled local worktree." {
		t.Errorf("local cancel text = %q", got)
	}
	if got := cancelResultText(FixDraftPayload{IssueID: 42}); got != "❌ Cancelled fix #42." {
		t.Errorf("remote cancel text = %q", got)
	}
}

// TestVariantReadyResultText covers the variant-ready reply
// for 🆕 and 🔄 reactions.
func TestVariantReadyResultText(t *testing.T) {
	if got := variantReadyResultText(FixDraftPayload{IssueID: -1}, "login-fix-v2"); got != "✅ Local worktree ready (using `login-fix-v2`)." {
		t.Errorf("local variant text = %q", got)
	}
	if got := variantReadyResultText(FixDraftPayload{IssueID: 42}, "login-fix-v2"); got != "✅ Fix #42 ready (using `login-fix-v2`)." {
		t.Errorf("remote variant text = %q", got)
	}
}

// TestRepoEmptyGuardAllowsLocalMode pins the round-trip
// between FixDraftPayload and ModeFromDraftPayload:
// ModeFromDraftPayload must classify local-mode drafts
// (IssueID == -1) as ModeLocal regardless of whether Repo
// is set, and remote-mode drafts (IssueID > 0) as ModeRemote.
//
// The "Repo == ” for local" shape matters because local
// /gtw fix --name <branch> never populates Repo (no remote
// issue). The defensive check `IssueID != -1 && Repo == ""`
// used to fire inside executeBranchExistsAction (removed by
// F-XX §3.1); the test is kept as a ModeFromDraftPayload
// sanity check.
func TestRepoEmptyGuardAllowsLocalMode(t *testing.T) {
	cases := []struct {
		name       string
		p          FixDraftPayload
		wantMode   Mode
		shouldTrip bool // legacy predicate: remote + empty Repo
	}{
		{"local w/ empty repo (allowed)", FixDraftPayload{IssueID: -1, Repo: ""}, ModeLocal, false},
		{"local w/ non-empty repo (still local)", FixDraftPayload{IssueID: -1, Repo: "o/r"}, ModeLocal, false},
		{"remote w/ empty repo (TRIPS)", FixDraftPayload{IssueID: 42, Repo: ""}, ModeRemote, true},
		{"remote w/ repo (allowed)", FixDraftPayload{IssueID: 42, Repo: "o/r"}, ModeRemote, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ModeFromDraftPayload(tc.p)
			if got != tc.wantMode {
				t.Errorf("ModeFromDraftPayload = %v, want %v", got, tc.wantMode)
			}
			trip := tc.p.IssueID != -1 && tc.p.Repo == ""
			if trip != tc.shouldTrip {
				t.Errorf("guard trip=%v, want %v", trip, tc.shouldTrip)
			}
		})
	}
}

// TestWorktreeFailChoice_LocalMode pins the local-mode render
// of the §5.3.3 decision card: title must NOT include
// "(-1)"; cancel label must NOT mention the nightme/wip
// rollback (local mode never adds a label).
func TestWorktreeFailChoice_LocalMode(t *testing.T) {
	p := FixDraftPayload{IssueID: -1, Branch: "b", Repo: ""}
	card := WorktreeFailChoice(p)
	if strings.Contains(card.Title, "(#-1)") {
		t.Errorf("local-mode title should not include '(-1)', got %q", card.Title)
	}
	if strings.Contains(card.Title, "nightme/wip") {
		t.Errorf("local-mode title should not mention wip label, got %q", card.Title)
	}
	// Cancel label must NOT have the rollback suffix.
	for _, c := range card.Options {
		if c.Emoji == "❌" && strings.Contains(c.Label, "nightme/wip") {
			t.Errorf("local-mode cancel label should not mention nightme/wip, got %q", c.Label)
		}
	}
}

// TestWorktreeFailChoice_RemoteModeWithLabel pins the ID-mode
// render WITH LabelAdded: title includes "(#42)" and the
// cancel label mentions the rollback.
func TestWorktreeFailChoice_RemoteModeWithLabel(t *testing.T) {
	p := FixDraftPayload{IssueID: 42, Branch: "b", Repo: "o/r", LabelAdded: true}
	card := WorktreeFailChoice(p)
	if !strings.Contains(card.Title, "(#42)") {
		t.Errorf("remote-mode title should include '(#42)', got %q", card.Title)
	}
	found := false
	for _, c := range card.Options {
		if c.Emoji == "❌" && strings.Contains(c.Label, "nightme/wip") {
			found = true
		}
	}
	if !found {
		t.Errorf("remote-mode cancel label should mention nightme/wip rollback, got choices %+v", card.Options)
	}
}

// TestWorktreeFailChoice_RemoteModeNoLabel covers the case
// where the worktree failed BEFORE the label was added
// (LabelAdded=false). Cancel label must NOT mention rollback.
func TestWorktreeFailChoice_RemoteModeNoLabel(t *testing.T) {
	p := FixDraftPayload{IssueID: 42, Branch: "b", Repo: "o/r", LabelAdded: false}
	card := WorktreeFailChoice(p)
	for _, c := range card.Options {
		if c.Emoji == "❌" && strings.Contains(c.Label, "nightme/wip") {
			t.Errorf("when LabelAdded=false, cancel label should not mention rollback, got %q", c.Label)
		}
	}
}

func TestEmitFollowUp_SelectedIDIsOptionIDNotEmoji(t *testing.T) {
	rec := &recordingCh{}
	cs := (&chatsession.ChatSession{}).WithEmitter(rec)
	opts := []ChoiceOption{
		{ID: "act:/gtw/branch-newv2", Emoji: "🆕", Label: "用 -v2 新分支"},
		{ID: "act:/gtw/cancel", Emoji: "❌", Label: "取消"},
	}
	emitFollowUp(context.Background(), cs, &Draft{
		ChoicePosted:    true,
		ChoiceTitle:     "⚠️ 分支已存在",
		ChoiceBody:      "branch: fix/42",
		ChoiceOptions:   opts,
		ChoiceRequestID: "gtw-fix-om1",
	}, ReactionEvent{ChatID: "oc_1"}, "🆕", "✅ ready")
	if len(rec.sends) != 1 {
		t.Fatalf("sends = %d, want 1", len(rec.sends))
	}
	got := rec.sends[0]
	if got.Kind != messages.OutChoicePatch {
		t.Errorf("Kind = %v, want OutChoicePatch", got.Kind)
	}
	if got.Choice == nil {
		t.Fatal("Choice is nil")
	}
	if got.Choice.SelectedID != "act:/gtw/branch-newv2" {
		t.Errorf("SelectedID = %q, want act:/gtw/branch-newv2 (option ID, not emoji)", got.Choice.SelectedID)
	}
	if !got.Choice.Settled {
		t.Error("follow-up PATCH should set Settled")
	}
	if got.Choice.Kind != messages.ChoiceKindDecision {
		t.Errorf("Kind = %v, want Decision", got.Choice.Kind)
	}
}

// TestExecuteWorktreeFailRetry_WritesYml pins the v1.5 fix
// for a pre-existing latent bug: the retry path bypasses
// completeFixAndDispatch, so without an explicit WriteGTWYml
// here, /gtw close on the retry'd worktree would say "no active
// fix to close in this chat" because the yml never existed.
//
// Test setup: a real temp git repo, a ChatSession pointing at
// it, a DraftFixWorktreeFail payload stored in the Manager.
// After HandleDraftReaction with the 🔄 emoji, the worktree
// must exist, the yml must exist at <worktree>/.nightme/gtw.yml,
// and ReadGTWYml must return the correct Context fields.
func TestExecuteWorktreeFailRetry_WritesYml(t *testing.T) {
	repoRoot := initTempRepo(t)

	cs, _ := chatsession.New("chat-retry-yml", "test-agent")
	rec := &recordingCh{}
	cs.WithEmitter(rec)

	m := newTestManager()
	deps := HandlerDeps{
		Git: ExecGitRunner{},
		Now: func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) },
	}

	// Mimic what emitWorktreeFailDraft would have stored: a
	// local-mode draft where /gtw fix --name failed at the
	// WorktreeAdd step.
	const slug = "login-bug"
	p := FixDraftPayload{
		IssueID:  -1,
		Title:    "(local branch)",
		Branch:   slug,
		Slug:     slug,
		Repo:     "",
		Worktree: repoRoot, // matches emitWorktreeFailDraft's local-mode field
		ChatID:   cs.ChatID,
	}
	m.StoreDraft(cs.ChatID, "msg-retry-1", &Draft{
		Kind:    DraftFixWorktreeFail,
		Payload: p,
	})

	// Set the chat's SelectedCwd to repoRoot so the retry path's
	// WorktreeAdd runs from inside the repo (the safety helper
	// does not derive repoRoot from cwd — it uses p.Worktree
	// directly, which we set above to repoRoot). This mirrors
	// the typical user state when a fix fails: they're sitting
	// in the main repo, never having reached a worktree.
	if err := cs.SetSelectedCwd(repoRoot); err != nil {
		t.Fatalf("SetSelectedCwd repoRoot: %v", err)
	}

	ev := services.ReactionEvent{
		ChatID:    cs.ChatID,
		RequestID: "msg-retry-1",
		Emoji:     "🔄",
	}

	consumed, err := HandleDraftReaction(context.Background(), m, deps, cs, ev)
	if err != nil {
		t.Fatalf("HandleDraftReaction: %v", err)
	}
	if !consumed {
		t.Fatal("HandleDraftReaction consumed=false; want true (retry should consume the draft)")
	}

	// 1. Worktree must exist on disk.
	wt := WorktreePath(repoRoot, slug)
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree not created at %s: %v\nreply:\n%s", wt, err, rec.lastText())
	}
	// 2. SelectedCwd must point at the new worktree.
	if got := cs.SelectedCwd(); !pathsEqual(got, wt) {
		t.Errorf("SelectedCwd = %q, want %q", got, wt)
	}
	// 3. The yml must exist and round-trip with the right
	//    fields. This is the core assertion of the v1.5 fix:
	//    without WriteGTWYml in the retry path, this ReadGTWYml
	//    would error out and /gtw close would say "no active
	//    fix".
	parsed, err := ReadGTWYml(wt)
	if err != nil {
		t.Fatalf("ReadGTWYml: %v (regression: retry path didn't write yml)", err)
	}
	if parsed.Mode != ModeLocal {
		t.Errorf("yml.Mode = %q, want %q", parsed.Mode, ModeLocal)
	}
	if parsed.Issue != -1 {
		t.Errorf("yml.Issue = %d, want -1", parsed.Issue)
	}
	if parsed.Branch != slug {
		t.Errorf("yml.Branch = %q, want %q", parsed.Branch, slug)
	}
	if !pathsEqual(parsed.Worktree, wt) {
		t.Errorf("yml.Worktree = %q, want %q", parsed.Worktree, wt)
	}
	if !pathsEqual(parsed.RepoRoot, repoRoot) {
		t.Errorf("yml.RepoRoot = %q, want %q", parsed.RepoRoot, repoRoot)
	}
	// 4. The draft was consumed (Take'd) — without this, the
	//    user could re-trigger the same retry emoji and confuse
	//    the handler.
	if m.GetDraft(cs.ChatID, "msg-retry-1") != nil {
		t.Error("draft not consumed after successful retry")
	}
	// 5. The reply card should mention the variant is ready.
	reply := rec.lastText()
	if !strings.Contains(reply, "ready") {
		t.Errorf("reply missing 'ready' marker:\n%s", reply)
	}
}

// _ ensures the package-level chatsession import is used
// when this test file gets trimmed. Prevents `go vet` from
// flagging unused imports if the file shrinks.
var _ = chatsession.MessageKindQueue
var _ context.Context
var _ testing.TB
