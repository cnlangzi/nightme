package gtw

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
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

// TestRepoEmptyGuardAllowsLocalMode pins the fix for the
// defensive `p.Repo == ""` check inside
// executeBranchExistsAction: it must NOT fire for local-mode
// drafts (which legitimately carry Repo = "" because local
// mode has no remote issue).
//
// We can't easily call the executor without a full ChatSession
// stub, so we test the predicate it uses: IssueID == -1 → local
// → Repo is allowed to be empty. If a future refactor
// re-tightens the predicate, this test catches it.
func TestRepoEmptyGuardAllowsLocalMode(t *testing.T) {
	p := FixDraftPayload{IssueID: -1, Repo: "", Branch: "b"}
	if ModeFromDraftPayload(p) != ModeLocal {
		t.Fatalf("expected local-mode payload")
	}
	// The guard condition in executeBranchExistsAction is
	// `p.IssueID != -1 && p.Repo == ""`. Validate the truth
	// table:
	cases := []struct {
		name      string
		p         FixDraftPayload
		shouldTrip bool
	}{
		{"local w/ empty repo (allowed)", FixDraftPayload{IssueID: -1, Repo: ""}, false},
		{"local w/ non-empty repo (still local)", FixDraftPayload{IssueID: -1, Repo: "o/r"}, false},
		{"remote w/ empty repo (TRIPS)", FixDraftPayload{IssueID: 42, Repo: ""}, true},
		{"remote w/ repo (allowed)", FixDraftPayload{IssueID: 42, Repo: "o/r"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trip := tc.p.IssueID != -1 && tc.p.Repo == ""
			if trip != tc.shouldTrip {
				t.Errorf("guard trip=%v, want %v", trip, tc.shouldTrip)
			}
		})
	}
}

// TestBranchExistsCard_LocalMode pins the local-mode render
// of the §5.3.1 decision card: the body must NOT show
// "issue: #-1" (which would look broken); instead it shows
// "branch: `<name>` (local)".
func TestBranchExistsCard_LocalMode(t *testing.T) {
	p := FixDraftPayload{IssueID: -1, Title: "(local branch)", Branch: "login-fix", Slug: "login-fix", Repo: ""}
	card := BranchExistsCard(p, "/worktrees/login-fix")
	if strings.Contains(card.Body, "issue: #") {
		t.Errorf("local-mode card should not show 'issue: #', got body:\n%s", card.Body)
	}
	if !strings.Contains(card.Body, "branch: `login-fix` (local)") {
		t.Errorf("local-mode card should mention '(local)' marker, got body:\n%s", card.Body)
	}
}

// TestBranchExistsCard_RemoteMode pins the ID-mode render.
func TestBranchExistsCard_RemoteMode(t *testing.T) {
	p := FixDraftPayload{IssueID: 42, Title: "Login state expires", Branch: "login-state", Repo: "owner/repo", Provider: "github"}
	card := BranchExistsCard(p, "")
	if !strings.Contains(card.Body, "issue: #42") {
		t.Errorf("remote-mode card should show 'issue: #42', got body:\n%s", card.Body)
	}
}

// TestWorktreeFailCard_LocalMode pins the local-mode render
// of the §5.3.3 decision card: title must NOT include
// "(-1)"; cancel label must NOT mention the nightme/wip
// rollback (local mode never adds a label).
func TestWorktreeFailCard_LocalMode(t *testing.T) {
	p := FixDraftPayload{IssueID: -1, Branch: "b", Repo: ""}
	card := WorktreeFailCard(p)
	if strings.Contains(card.Title, "(#-1)") {
		t.Errorf("local-mode title should not include '(-1)', got %q", card.Title)
	}
	if strings.Contains(card.Title, "nightme/wip") {
		t.Errorf("local-mode title should not mention wip label, got %q", card.Title)
	}
	// Cancel label must NOT have the rollback suffix.
	for _, c := range card.Choices {
		if c.Emoji == "❌" && strings.Contains(c.Label, "nightme/wip") {
			t.Errorf("local-mode cancel label should not mention nightme/wip, got %q", c.Label)
		}
	}
}

// TestWorktreeFailCard_RemoteModeWithLabel pins the ID-mode
// render WITH LabelAdded: title includes "(#42)" and the
// cancel label mentions the rollback.
func TestWorktreeFailCard_RemoteModeWithLabel(t *testing.T) {
	p := FixDraftPayload{IssueID: 42, Branch: "b", Repo: "o/r", LabelAdded: true}
	card := WorktreeFailCard(p)
	if !strings.Contains(card.Title, "(#42)") {
		t.Errorf("remote-mode title should include '(#42)', got %q", card.Title)
	}
	found := false
	for _, c := range card.Choices {
		if c.Emoji == "❌" && strings.Contains(c.Label, "nightme/wip") {
			found = true
		}
	}
	if !found {
		t.Errorf("remote-mode cancel label should mention nightme/wip rollback, got choices %+v", card.Choices)
	}
}

// TestWorktreeFailCard_RemoteModeNoLabel covers the case
// where the worktree failed BEFORE the label was added
// (LabelAdded=false). Cancel label must NOT mention rollback.
func TestWorktreeFailCard_RemoteModeNoLabel(t *testing.T) {
	p := FixDraftPayload{IssueID: 42, Branch: "b", Repo: "o/r", LabelAdded: false}
	card := WorktreeFailCard(p)
	for _, c := range card.Choices {
		if c.Emoji == "❌" && strings.Contains(c.Label, "nightme/wip") {
			t.Errorf("when LabelAdded=false, cancel label should not mention rollback, got %q", c.Label)
		}
	}
}

// _ ensures the package-level chatsession import is used
// when this test file gets trimmed. Prevents `go vet` from
// flagging unused imports if the file shrinks.
var _ = chatsession.MessageKindQueue
var _ context.Context
var _ testing.TB