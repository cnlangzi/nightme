package gtw

// Tests for the shared "is this issue held?" check and for the two
// fix paths that use it: the automatic pick leaves a held issue
// alone, the named fix refuses it.

import (
	"context"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// TestHeldLabel pins the shared check: a label in the `nightme/`
// namespace other than `nightme/assigned` holds the issue, and the
// match is case-insensitive.
func TestHeldLabel(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   string
	}{
		{"no labels at all", nil, ""},
		{"only the queue marker", []string{LabelAssigned}, ""},
		{"a label outside the namespace", []string{"priority/high"}, ""},
		{"the claim", []string{LabelWIP}, LabelWIP},
		{"ready for review", []string{LabelReady}, LabelReady},
		{"queue marker plus a state", []string{LabelAssigned, LabelReady}, LabelReady},
		{"the claim wins over another state", []string{LabelReady, LabelWIP}, LabelWIP},
		{"a different case", []string{"Nightme/Ready"}, "Nightme/Ready"},
		{"the queue marker in another case", []string{"Nightme/Assigned"}, ""},
		{"a namespace lookalike", []string{"nightmeal"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := heldLabel(&Issue{ID: 42, Labels: tc.labels})
			if got != tc.want {
				t.Errorf("heldLabel(%v) = %q, want %q", tc.labels, got, tc.want)
			}
		})
	}
}

// TestHeldLabel_NilIssue pins the nil-safe answer.
func TestHeldLabel_NilIssue(t *testing.T) {
	if got := heldLabel(nil); got != "" {
		t.Errorf("heldLabel(nil) = %q, want empty", got)
	}
}

// TestSelectNextAssignedIssue_SkipsHeldIssues pins the queue filter:
// an issue that carries the queue marker and another state label is
// held by another agent, so it is not a candidate.
func TestSelectNextAssignedIssue_SkipsHeldIssues(t *testing.T) {
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetListIssues([]*Issue{
		autoIssue(1, "feat: waiting for review", 1, LabelAssigned, LabelReady),
		autoIssue(2, "feat: free", 2, LabelAssigned),
	})

	sel, err := autoSelect(t, prov)
	if err != nil {
		t.Fatalf("selectNextAssignedIssue: %v", err)
	}
	if sel.Issue == nil || sel.Issue.ID != 2 {
		t.Fatalf("picked %+v, want issue #2 (the held issue is out of the queue)", sel.Issue)
	}
	if sel.Counters.Candidates != 1 {
		t.Errorf("candidates = %d, want 1 (the held issue is not a candidate)", sel.Counters.Candidates)
	}
}

// TestRenderAutoNoCandidate_QueueRow pins the wording of the empty
// queue: the candidate must carry the queue marker alone.
func TestRenderAutoNoCandidate_QueueRow(t *testing.T) {
	card := renderAutoNoCandidate(&autoSelection{}, labelCatalogNote(labelCheck{Verified: true}))
	want := "empty — none of the 100 oldest open issues carries `nightme/assigned` on its own"
	if !strings.Contains(card, want) {
		t.Errorf("card does not carry the queue row:\n%s", card)
	}
}

// TestLabelCatalogNote_EndsWithTheQueueRequirement pins that every
// variant of the note tells the reader what the queue needs: the
// queue marker on an issue that carries no other state label, put
// there with `/gtw assign`.
func TestLabelCatalogNote_EndsWithTheQueueRequirement(t *testing.T) {
	want := "run `/gtw assign <issue-id>` on an issue that carries no other `nightme/*` label to fill the queue"
	variants := map[string]labelCheck{
		"just created": {Created: []string{LabelAssigned}, Verified: true},
		"cached":       {},
		"complete":     {Verified: true},
	}
	for name, chk := range variants {
		t.Run(name, func(t *testing.T) {
			note := labelCatalogNote(chk)
			if !strings.HasSuffix(note, want) {
				t.Errorf("note = %q, want it to end with %q", note, want)
			}
		})
	}
}

// --- the named fix path ---

// TestFixRemote_HeldIssue_Rejected pins the refusal of an issue that
// another agent holds: the reply names the label, the outcome
// carries the reason code and the label, and nothing is created.
func TestFixRemote_HeldIssue_Rejected(t *testing.T) {
	rig := newFixRemoteRig(t)
	issueID := 42
	title := "Login state expiration"
	rig.prov.SetIssue(issueID, &Issue{
		ID:     issueID,
		Title:  title,
		State:  "open",
		URL:    "https://github.com/cnlangzi/nightme/issues/42",
		Labels: []string{"priority/high", LabelReady},
	})

	work := rig.cs.Hold("msg-fix")
	ctx := chatsession.WithWork(context.Background(), work)
	if _, err := RunFix(ctx, ModeRemote, rig.cs, rig.deps, rig.cs.ChatID,
		"msg-fix", []string{"42"}, false, false); err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	last := rig.rec.lastText()
	work.Release()

	mustContain(t, last, "Issue #42 is not available for a fix")
	mustContain(t, last, LabelReady)
	mustContain(t, last, "another agent is working on it")
	mustContain(t, last, "remove that label on the issue page")

	got := waitSettled(rig.rec)
	if got == nil {
		t.Fatal("the message did not settle")
	}
	if got.Data["reason"] != "issue_in_state" {
		t.Errorf("outcome reason = %q, want issue_in_state", got.Data["reason"])
	}
	if got.Data["label"] != LabelReady {
		t.Errorf("outcome label = %q, want %q", got.Data["label"], LabelReady)
	}

	// No label write of any kind, and no worktree.
	if addCalls := rig.prov.CallsByMethod("AddIssueLabel"); len(addCalls) != 0 {
		t.Errorf("AddIssueLabel unexpectedly called: %+v", addCalls)
	}
	if rms := rig.prov.CallsByMethod("RemoveIssueLabel"); len(rms) != 0 {
		t.Errorf("RemoveIssueLabel unexpectedly called: %+v", rms)
	}
	branch := DeriveBranchFromTitle(title, issueID)
	if exists, err := BranchExists(context.Background(), rig.repoRoot, branch, rig.deps.Git); err != nil {
		t.Fatalf("BranchExists: %v", err)
	} else if exists {
		t.Errorf("branch %q exists; want no branch", branch)
	}
	if queued := rig.cs.QueueLen(); queued != 0 {
		t.Errorf("cs.QueueLen = %d, want 0 (no dispatch on a held issue)", queued)
	}
}

// TestFixRemote_ClaimedIssue_KeepsItsRefusal pins that the claim
// keeps its own wording and reason code — programs branch on it.
func TestFixRemote_ClaimedIssue_KeepsItsRefusal(t *testing.T) {
	rig := newFixRemoteRig(t)
	issueID := 42
	rig.prov.SetIssue(issueID, &Issue{
		ID:     issueID,
		Title:  "Login state expiration",
		State:  "open",
		URL:    "https://github.com/cnlangzi/nightme/issues/42",
		Labels: []string{LabelWIP},
	})

	work := rig.cs.Hold("msg-fix")
	ctx := chatsession.WithWork(context.Background(), work)
	if _, err := RunFix(ctx, ModeRemote, rig.cs, rig.deps, rig.cs.ChatID,
		"msg-fix", []string{"42"}, false, false); err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	last := rig.rec.lastText()
	work.Release()

	mustContain(t, last, "already claimed")
	mustContain(t, last, "/gtw close")
	mustNotContain(t, last, "not available for a fix")

	got := waitSettled(rig.rec)
	if got == nil {
		t.Fatal("the message did not settle")
	}
	if got.Data["reason"] != "issue_claimed" {
		t.Errorf("outcome reason = %q, want issue_claimed", got.Data["reason"])
	}
	if got.Data["label"] != LabelWIP {
		t.Errorf("outcome label = %q, want %q", got.Data["label"], LabelWIP)
	}
}

// TestFixRemote_ClaimWinsOverAnotherState pins the precedence
// between the two refusals: an issue that carries the claim keeps
// the claim refusal, whatever else it carries.
func TestFixRemote_ClaimWinsOverAnotherState(t *testing.T) {
	rig := newFixRemoteRig(t)
	rig.prov.SetIssue(42, &Issue{
		ID:     42,
		Title:  "Login state expiration",
		State:  "open",
		URL:    "https://github.com/cnlangzi/nightme/issues/42",
		Labels: []string{LabelReady, LabelWIP},
	})

	work := rig.cs.Hold("msg-fix")
	ctx := chatsession.WithWork(context.Background(), work)
	if _, err := RunFix(ctx, ModeRemote, rig.cs, rig.deps, rig.cs.ChatID,
		"msg-fix", []string{"42"}, false, false); err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	last := rig.rec.lastText()
	work.Release()

	mustContain(t, last, "already claimed")
	mustNotContain(t, last, "not available for a fix")
	got := waitSettled(rig.rec)
	if got == nil {
		t.Fatal("the message did not settle")
	}
	if got.Data["reason"] != "issue_claimed" {
		t.Errorf("outcome reason = %q, want issue_claimed", got.Data["reason"])
	}
}

// TestFixRemote_UnlabelledIssue_Proceeds pins that the named fix does
// not need the queue marker: naming the issue is the manual
// assignment that replaces it.
func TestFixRemote_UnlabelledIssue_Proceeds(t *testing.T) {
	rig := newFixRemoteRig(t)
	issueID := 42
	title := "Login state expiration"
	rig.prov.SetIssue(issueID, &Issue{
		ID:    issueID,
		Title: title,
		State: "open",
		URL:   "https://github.com/cnlangzi/nightme/issues/42",
	})

	if _, err := rig.drive(t, "42"); err != nil {
		t.Fatalf("RunFix: %v", err)
	}

	last := rig.rec.lastText()
	mustContain(t, last, "Fix #42 ready")
	addCalls := rig.prov.CallsByMethod("AddIssueLabel")
	if len(addCalls) != 1 || addCalls[0].Label != LabelWIP {
		t.Errorf("AddIssueLabel calls = %+v, want one stamping %q", addCalls, LabelWIP)
	}
	branch := DeriveBranchFromTitle(title, issueID)
	if exists, err := BranchExists(context.Background(), rig.repoRoot, branch, rig.deps.Git); err != nil {
		t.Fatalf("BranchExists: %v", err)
	} else if !exists {
		t.Errorf("branch %q does not exist; the fix must have created it", branch)
	}
}
