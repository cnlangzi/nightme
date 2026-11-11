package gtw

// Integration tests for the automatic mode of /gtw fix:
//
//	/gtw fix *
//
// They pair a real git repo (initTempRepo from
// close_integration_test.go) with the in-process fakeGitProvider,
// so the whole flow runs end-to-end without touching github.com /
// gitlab.com.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
)

// TestFixAuto_PicksAssignedIssue pins the automatic mode end to
// end: the oldest assigned issue is claimed, its worktree is
// created, it carries `nightme/wip` and no longer
// `nightme/assigned`, and the issue is dispatched to the agent.
// The open issue that carries no queue label is left alone.
func TestFixAuto_PicksAssignedIssue(t *testing.T) {
	rig := newFixRemoteRig(t)
	assigned := &Issue{
		ID:        42,
		Title:     "feat: the assigned one",
		Body:      "Do the assigned work.",
		State:     "open",
		Labels:    []string{LabelAssigned},
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	unassigned := &Issue{
		ID:        43,
		Title:     "feat: the unassigned one",
		State:     "open",
		CreatedAt: time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC),
	}
	rig.prov.SetListIssues([]*Issue{unassigned, assigned})
	rig.prov.SetIssue(42, assigned)

	res, err := rig.drive(t, "*")
	if err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}

	// The claim: `nightme/wip` goes on the picked issue.
	addCalls := rig.prov.CallsByMethod("AddIssueLabel")
	if len(addCalls) != 1 || addCalls[0].Label != LabelWIP || addCalls[0].ID != 42 {
		t.Errorf("AddIssueLabel calls = %+v, want one call stamping %q on issue 42", addCalls, LabelWIP)
	}
	// The issue leaves the queue: `nightme/assigned` comes off.
	rmCalls := rig.prov.CallsByMethod("RemoveIssueLabel")
	if len(rmCalls) != 1 || rmCalls[0].Label != LabelAssigned || rmCalls[0].ID != 42 {
		t.Errorf("RemoveIssueLabel calls = %+v, want one call removing %q from issue 42", rmCalls, LabelAssigned)
	}
	// The picked issue is fetched like any named issue, so the
	// agent gets its body and attachments.
	getCalls := rig.prov.CallsByMethod("GetIssue")
	if len(getCalls) != 1 || getCalls[0].ID != 42 {
		t.Errorf("GetIssue calls = %+v, want exactly one for issue 42", getCalls)
	}

	// The worktree is real, and the snapshot records the remote
	// mode with the picked issue.
	wtOut, _ := mustGitOut(t, rig.repoRoot, "worktree", "list", "--porcelain")
	realWt := parseSecondWorktree(wtOut, filepath.Dir(rig.repoRoot))
	if realWt == "" {
		t.Fatalf("could not parse created worktree from:\n%s", wtOut)
	}
	if _, err := os.Stat(realWt); err != nil {
		t.Errorf("worktree %s not on disk: %v", realWt, err)
	}
	parsed, err := ReadGTWYml(realWt)
	if err != nil {
		t.Fatalf("ReadGTWYml: %v", err)
	}
	if parsed.Mode != ModeRemote {
		t.Errorf("yml mode = %q, want remote", parsed.Mode)
	}
	if parsed.Issue != 42 {
		t.Errorf("yml issue = %d, want 42", parsed.Issue)
	}

	// The dispatch reached the agent.
	if got := rig.cs.QueueLen(); got != 1 {
		t.Errorf("cs.QueueLen = %d, want 1 (the dispatch message)", got)
	}

	// The reply names the issue it picked and says how.
	last := rig.rec.lastText()
	if !strings.Contains(last, "Fix #42") {
		t.Errorf("success reply missing the picked issue:\n%s", last)
	}
	if !strings.Contains(last, "picked automatically") {
		t.Errorf("success reply must say the issue was picked automatically:\n%s", last)
	}
}

// TestFixAuto_NoCandidate_ReportsNoop pins the empty-queue
// outcome: nothing is claimed, no worktree is created, and the
// reply asks to be woken later.
func TestFixAuto_NoCandidate_ReportsNoop(t *testing.T) {
	rig := newFixRemoteRig(t)
	rig.prov.SetListIssues([]*Issue{
		{ID: 43, Title: "feat: unassigned", State: "open"},
	})

	res, err := rig.drive(t, "*")
	if err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}

	if addCalls := rig.prov.CallsByMethod("AddIssueLabel"); len(addCalls) != 0 {
		t.Errorf("AddIssueLabel calls = %+v, want none (nothing was eligible)", addCalls)
	}
	wtOut, _ := mustGitOut(t, rig.repoRoot, "worktree", "list", "--porcelain")
	if c := strings.Count(wtOut, "worktree "); c != 1 {
		t.Errorf("worktree count = %d, want 1 (no new worktree):\n%s", c, wtOut)
	}
	last := rig.rec.lastText()
	if !strings.Contains(last, "assigned issue") {
		t.Errorf("reply must say nothing was eligible:\n%s", last)
	}
	if !strings.Contains(last, "5m") {
		t.Errorf("reply must say when to come back:\n%s", last)
	}
}

// TestFixAuto_SkipsBlockedCandidate pins the dependency gate in
// the full flow: the blocked candidate is passed over and the
// next one is worked on.
func TestFixAuto_SkipsBlockedCandidate(t *testing.T) {
	rig := newFixRemoteRig(t)
	blocked := &Issue{
		ID:        41,
		Title:     "feat: blocked one",
		State:     "open",
		Labels:    []string{LabelAssigned},
		CreatedAt: time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC),
	}
	free := &Issue{
		ID:        42,
		Title:     "feat: free one",
		Body:      "Do the free work.",
		State:     "open",
		Labels:    []string{LabelAssigned},
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	rig.prov.SetListIssues([]*Issue{blocked, free})
	rig.prov.SetIssue(42, free)
	rig.prov.SetBlockedBy(41, []*Issue{{ID: 30, State: "open"}})

	res, err := rig.drive(t, "*")
	if err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if res == nil || !res.Consumed {
		t.Fatalf("Result = %+v, want Consumed=true", res)
	}

	addCalls := rig.prov.CallsByMethod("AddIssueLabel")
	if len(addCalls) != 1 || addCalls[0].ID != 42 {
		t.Errorf("AddIssueLabel calls = %+v, want one call on issue 42 (the blocked one is passed over)", addCalls)
	}
	last := rig.rec.lastText()
	if !strings.Contains(last, fmt.Sprintf("Fix #%d", 42)) {
		t.Errorf("success reply missing the picked issue:\n%s", last)
	}
}

// TestFixAuto_NoCandidate_OutcomeCarriesWakeUp pins the outcome
// the personal agent reads: a noop that says why nothing was
// eligible and when to come back.
func TestFixAuto_NoCandidate_OutcomeCarriesWakeUp(t *testing.T) {
	rig := newFixRemoteRig(t)
	rig.prov.SetListIssues([]*Issue{
		{ID: 43, Title: "feat: unassigned", State: "open"},
	})

	work := rig.cs.Hold("msg-fix-remote")
	ctx := chatsession.WithWork(context.Background(), work)
	if _, err := RunFix(ctx, ModeRemote, rig.cs, rig.deps, rig.cs.ChatID,
		"msg-fix-remote", []string{"*"}, false); err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	work.Release()

	var settled *messages.Outcome
	for _, m := range rig.rec.sends {
		if m.Kind == messages.OutMessageSettled {
			settled = m.Outcome
		}
	}
	if settled == nil {
		t.Fatalf("no settled outcome: the command reported nothing")
	}
	if settled.Status != messages.OutcomeNoop {
		t.Errorf("outcome status = %q, want noop", settled.Status)
	}
	want := map[string]string{
		"reason":        "no_candidate",
		"wake_after":    "5m",
		"candidates":    "0",
		"blocked":       "0",
		"branch_exists": "0",
		"pr_open":       "0",
	}
	for k, v := range want {
		if settled.Data[k] != v {
			t.Errorf("outcome Data[%q] = %q, want %q", k, settled.Data[k], v)
		}
	}
}
