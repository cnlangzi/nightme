package gtw

// Flow tests for `/gtw assign <issue-id> [--remove|-r]`.
//
// The command is the user's move on the queue marker: it attaches
// `nightme/assigned` to an issue, or takes it off. It touches the
// tracker and nothing else — no worktree, no branch, no agent, no
// state file.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// assignRig is the /gtw assign test rig: a git repo with an origin
// remote, a chat session pointed at it, and a fake provider behind
// the factory.
type assignRig struct {
	repoRoot string
	cs       *chatsession.ChatSession
	prov     *fakeGitProvider
	rec      *recordingCh
	factory  *Factory
	ctx      context.Context
	work     *chatsession.Work
}

// newAssignRig builds the rig. The label catalog is complete by
// default, so the catalog check passes and the tests that care about
// it override the fake's response.
func newAssignRig(t *testing.T) *assignRig {
	t.Helper()
	repoRoot := initTempRepo(t)
	mustGit(t, repoRoot, "remote", "add", "origin",
		"https://github.com/cnlangzi/nightme.git")

	rec := &recordingCh{}
	cs, _ := chatsession.New("chat-assign-"+t.Name(), "test-agent")
	cs.WithEmitter(rec)
	if err := cs.SetSelectedCwd(repoRoot); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}

	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetListLabels(AllLabels)

	deps := HandlerDeps{
		Git:    ExecGitRunner{},
		Detect: fakeDetect(prov),
	}
	work := cs.Hold("msg-1")
	return &assignRig{
		repoRoot: repoRoot,
		cs:       cs,
		prov:     prov,
		rec:      rec,
		factory:  NewFactoryWithDeps(NewManager(), deps),
		ctx:      chatsession.WithWork(context.Background(), work),
		work:     work,
	}
}

// drive runs `/gtw assign <args...>` through the factory and returns
// the card the user read.
func (r *assignRig) drive(t *testing.T, args ...string) string {
	t.Helper()
	argv := append([]string{"gtw", "assign"}, args...)
	if _, err := r.factory.Handle(r.ctx, command.RuntimeServices{}, nil, r.cs, command.SlashInput{
		ChatID:    r.cs.ChatID,
		MessageID: "msg-1",
		Text:      "/gtw assign " + strings.Join(args, " "),
		Args:      argv,
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	return r.rec.lastText()
}

// settle releases the message's work and returns the outcome the
// command reported.
func (r *assignRig) settle(t *testing.T) *messages.Outcome {
	t.Helper()
	r.work.Release()
	r.rec.mu.Lock()
	defer r.rec.mu.Unlock()
	var out *messages.Outcome
	for _, m := range r.rec.sends {
		if m.Kind != messages.OutMessageSettled {
			continue
		}
		out = m.Outcome
	}
	return out
}

// seedIssue registers an open issue the provider will answer.
func (r *assignRig) seedIssue(id int, title string, labels ...string) {
	r.prov.SetIssue(id, &Issue{
		ID:     id,
		Title:  title,
		State:  "open",
		Labels: labels,
		URL:    "https://github.com/cnlangzi/nightme/issues/" + strconv.Itoa(id),
	})
}

// TestAssign_AddsTheQueueMarker pins the add path: the label goes
// on the issue the user named, in the repository of the chat's cwd,
// and the reply names the issue and the label.
func TestAssign_AddsTheQueueMarker(t *testing.T) {
	rig := newAssignRig(t)
	rig.seedIssue(42, "Login state expiration")

	last := rig.drive(t, "42")

	calls := rig.prov.CallsByMethod("AddIssueLabel")
	if len(calls) != 1 {
		t.Fatalf("AddIssueLabel calls = %+v, want exactly one", calls)
	}
	if calls[0].Owner != "cnlangzi" || calls[0].Repo != "nightme" || calls[0].ID != 42 {
		t.Errorf("AddIssueLabel called with %+v, want cnlangzi/nightme#42", calls[0])
	}
	if calls[0].Label != LabelAssigned {
		t.Errorf("AddIssueLabel label = %q, want %q", calls[0].Label, LabelAssigned)
	}
	if rms := rig.prov.CallsByMethod("RemoveIssueLabel"); len(rms) != 0 {
		t.Errorf("RemoveIssueLabel unexpectedly called: %+v", rms)
	}

	for _, want := range []string{"#42", "Login state expiration", LabelAssigned} {
		if !strings.Contains(last, want) {
			t.Errorf("reply missing %q:\n%s", want, last)
		}
	}
}

// TestAssign_RemoveTakesTheQueueMarkerOff pins the remove path, in
// both flag spellings.
func TestAssign_RemoveTakesTheQueueMarkerOff(t *testing.T) {
	for _, flag := range []string{"--remove", "-r"} {
		t.Run(flag, func(t *testing.T) {
			rig := newAssignRig(t)
			rig.seedIssue(42, "Login state expiration", LabelAssigned)

			last := rig.drive(t, "42", flag)

			calls := rig.prov.CallsByMethod("RemoveIssueLabel")
			if len(calls) != 1 {
				t.Fatalf("RemoveIssueLabel calls = %+v, want exactly one", calls)
			}
			if calls[0].Owner != "cnlangzi" || calls[0].Repo != "nightme" || calls[0].ID != 42 {
				t.Errorf("RemoveIssueLabel called with %+v, want cnlangzi/nightme#42", calls[0])
			}
			if calls[0].Label != LabelAssigned {
				t.Errorf("RemoveIssueLabel label = %q, want %q", calls[0].Label, LabelAssigned)
			}
			if adds := rig.prov.CallsByMethod("AddIssueLabel"); len(adds) != 0 {
				t.Errorf("AddIssueLabel unexpectedly called: %+v", adds)
			}
			if !strings.Contains(last, "removed") {
				t.Errorf("reply must report the removal:\n%s", last)
			}
		})
	}
}

// TestAssign_NoIssueStateGuard pins that the command flips the label
// whatever state the issue is in: a closed issue is labelled, and an
// issue another agent holds is labelled too — the queue filter is the
// pick's job, not the assigner's.
func TestAssign_NoIssueStateGuard(t *testing.T) {
	cases := []struct {
		name   string
		state  string
		labels []string
	}{
		{"closed issue", "closed", nil},
		{"held issue", "open", []string{LabelWIP}},
		{"done issue", "closed", []string{LabelDone}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newAssignRig(t)
			rig.prov.SetIssue(42, &Issue{
				ID:     42,
				Title:  "Login state expiration",
				State:  tc.state,
				Labels: tc.labels,
				URL:    "https://github.com/cnlangzi/nightme/issues/42",
			})

			rig.drive(t, "42")

			calls := rig.prov.CallsByMethod("AddIssueLabel")
			if len(calls) != 1 || calls[0].Label != LabelAssigned {
				t.Fatalf("AddIssueLabel calls = %+v, want one stamping %q", calls, LabelAssigned)
			}
			got := rig.prov.IssueLabels(42)
			if !slices.Contains(got, LabelAssigned) {
				t.Errorf("issue labels = %v, want %q to be present", got, LabelAssigned)
			}
			// The other labels are untouched.
			for _, l := range tc.labels {
				if !slices.Contains(got, l) {
					t.Errorf("issue labels = %v, want %q to be kept", got, l)
				}
			}
		})
	}
}

// TestAssign_IdempotentAddSaysNothingChanged pins that a repeated
// add does not claim a change it did not make — while the provider
// call still happens, because the call is idempotent.
func TestAssign_IdempotentAddSaysNothingChanged(t *testing.T) {
	rig := newAssignRig(t)
	rig.seedIssue(42, "Login state expiration", LabelAssigned)

	last := rig.drive(t, "42")

	if calls := rig.prov.CallsByMethod("AddIssueLabel"); len(calls) != 1 {
		t.Errorf("AddIssueLabel calls = %+v, want one (the call is idempotent)", calls)
	}
	if !strings.Contains(last, "nothing changed") {
		t.Errorf("reply must say nothing changed:\n%s", last)
	}
	if strings.Contains(last, "added to #42") {
		t.Errorf("reply must not claim the label was added:\n%s", last)
	}
}

// TestAssign_IdempotentRemoveSaysNothingChanged is the remove twin:
// the label was not there, so the reply says so.
func TestAssign_IdempotentRemoveSaysNothingChanged(t *testing.T) {
	rig := newAssignRig(t)
	rig.seedIssue(42, "Login state expiration")

	last := rig.drive(t, "42", "--remove")

	if calls := rig.prov.CallsByMethod("RemoveIssueLabel"); len(calls) != 1 {
		t.Errorf("RemoveIssueLabel calls = %+v, want one (the call is idempotent)", calls)
	}
	if !strings.Contains(last, "nothing changed") {
		t.Errorf("reply must say nothing changed:\n%s", last)
	}
	if strings.Contains(last, "removed from #42") {
		t.Errorf("reply must not claim the label was removed:\n%s", last)
	}
}

// TestAssign_CatalogCheckFailsOnAdd pins that a failing catalog
// check stops the add before anything is attached: the label cannot
// go on an issue in a repository whose catalog does not hold it.
func TestAssign_CatalogCheckFailsOnAdd(t *testing.T) {
	rig := newAssignRig(t)
	rig.seedIssue(42, "Login state expiration")
	rig.prov.SetListLabelsErr(errors.New("403 Forbidden: missing Labels read scope"))

	last := rig.drive(t, "42")

	if !strings.Contains(last, "Could not ensure gtw labels on cnlangzi/nightme") {
		t.Errorf("reply must name the failed check:\n%s", last)
	}
	if !strings.Contains(last, "403 Forbidden: missing Labels read scope") {
		t.Errorf("reply must echo the provider error verbatim:\n%s", last)
	}
	if calls := rig.prov.CallsByMethod("AddIssueLabel"); len(calls) != 0 {
		t.Errorf("AddIssueLabel calls = %+v, want 0 (the check failed first)", calls)
	}
	got := rig.settle(t)
	assertOutcome(t, got, messages.OutcomeFailed, map[string]string{"reason": "label_error"})
}

// TestAssign_CatalogCheckWarnsOnRemove pins that a failing catalog
// check does not stop the removal: the removal proceeds and the
// warning rides along on the card.
func TestAssign_CatalogCheckWarnsOnRemove(t *testing.T) {
	rig := newAssignRig(t)
	rig.seedIssue(42, "Login state expiration", LabelAssigned)
	rig.prov.SetListLabelsErr(errors.New("403 Forbidden: missing Labels read scope"))

	last := rig.drive(t, "42", "--remove")

	if calls := rig.prov.CallsByMethod("RemoveIssueLabel"); len(calls) != 1 {
		t.Fatalf("RemoveIssueLabel calls = %+v, want one (the warning does not stop it)", calls)
	}
	if !strings.HasPrefix(last, "⚠️") {
		t.Errorf("reply must start with the warning line:\n%s", last)
	}
	if !strings.Contains(last, "could not ensure gtw labels on cnlangzi/nightme") {
		t.Errorf("reply must name the failed check:\n%s", last)
	}
	got := rig.settle(t)
	assertOutcome(t, got, messages.OutcomeOK, map[string]string{"action": "removed"})
}

// TestAssign_LabelWriteFailure_ReportsLabelError pins the reason
// code of a failed label write: the label did not land, which is
// what `label_error` means — the same code /gtw fix reports.
func TestAssign_LabelWriteFailure_ReportsLabelError(t *testing.T) {
	cases := []struct {
		name string
		args []string
		err  error
	}{
		{"add", []string{"42"}, errors.New("HTTP 403: missing label write scope")},
		{"remove", []string{"42", "--remove"}, errors.New("HTTP 403: missing label write scope")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newAssignRig(t)
			rig.seedIssue(42, "Login state expiration")
			rig.prov.SetAddIssueLabelErr(tc.err)
			rig.prov.SetRemoveIssueLabelErr(tc.err)

			rig.drive(t, tc.args...)

			got := rig.settle(t)
			assertOutcome(t, got, messages.OutcomeFailed, map[string]string{"reason": "label_error"})
		})
	}
}

// TestAssign_ClosedIssue_MakesNoPickPromise pins that the card does
// not promise a pick the queue cannot deliver: the queue holds open
// issues only.
func TestAssign_ClosedIssue_MakesNoPickPromise(t *testing.T) {
	rig := newAssignRig(t)
	rig.prov.SetIssue(42, &Issue{
		ID:    42,
		Title: "Login state expiration",
		State: "closed",
		URL:   "https://github.com/cnlangzi/nightme/issues/42",
	})

	last := rig.drive(t, "42")

	if strings.Contains(last, "picks it up on the next run") {
		t.Errorf("reply must not promise a pick of a closed issue:\n%s", last)
	}
	if !strings.Contains(last, "queue holds open issues") {
		t.Errorf("reply must say the queue holds open issues:\n%s", last)
	}
}

// TestAssign_HeldIssue_MakesNoPickPromise is the same promise, for
// an issue another agent holds.
func TestAssign_HeldIssue_MakesNoPickPromise(t *testing.T) {
	rig := newAssignRig(t)
	rig.seedIssue(42, "Login state expiration", LabelWIP)

	last := rig.drive(t, "42")

	if strings.Contains(last, "picks it up on the next run") {
		t.Errorf("reply must not promise a pick of a held issue:\n%s", last)
	}
	if !strings.Contains(last, LabelWIP) {
		t.Errorf("reply must name the label that holds the issue:\n%s", last)
	}
}

// TestAssign_UnknownIssue pins the error card of a missing issue:
// the same shape as /gtw fix's.
func TestAssign_UnknownIssue(t *testing.T) {
	rig := newAssignRig(t)
	rig.prov.SetGetIssueErr(42, ErrIssueNotFound)

	last := rig.drive(t, "42")

	want := "❌ Issue #42 not found in cnlangzi/nightme."
	if !strings.Contains(last, want) {
		t.Errorf("reply = %q, want it to contain %q", last, want)
	}
	if calls := rig.prov.CallsByMethod("AddIssueLabel"); len(calls) != 0 {
		t.Errorf("AddIssueLabel unexpectedly called: %+v", calls)
	}
	got := rig.settle(t)
	assertOutcome(t, got, messages.OutcomeFailed, map[string]string{"reason": "issue_not_found"})
}

// TestAssign_OutcomeData pins the facts a program reads: the issue,
// its URL, and the operation that was carried out.
func TestAssign_OutcomeData(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		action string
	}{
		{"added", []string{"42"}, "added"},
		{"removed", []string{"42", "--remove"}, "removed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newAssignRig(t)
			rig.seedIssue(42, "Login state expiration")

			rig.drive(t, tc.args...)

			got := rig.settle(t)
			assertOutcome(t, got, messages.OutcomeOK, map[string]string{
				"issue":  "42",
				"url":    "https://github.com/cnlangzi/nightme/issues/42",
				"action": tc.action,
			})
		})
	}
}

// TestAssign_UsageError pins the parse gate: a bad command line is
// refused before any provider call.
func TestAssign_UsageError(t *testing.T) {
	cases := [][]string{
		{"42", "--dry-run"},
		{"1", "2"},
		{"abc"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			rig := newAssignRig(t)
			rig.seedIssue(42, "Login state expiration")

			rig.drive(t, args...)

			if calls := rig.prov.Calls(); len(calls) != 0 {
				t.Errorf("provider called for an invalid command line: %+v", calls)
			}
			got := rig.settle(t)
			assertOutcome(t, got, messages.OutcomeFailed, map[string]string{"reason": "usage"})
		})
	}
}

// TestAssign_TouchesNothingElse pins that the command is a label
// flip and nothing more: no worktree, no branch, no state file.
func TestAssign_TouchesNothingElse(t *testing.T) {
	rig := newAssignRig(t)
	rig.seedIssue(42, "Login state expiration")

	rig.drive(t, "42")

	wtOut, _ := mustGitOut(t, rig.repoRoot, "worktree", "list", "--porcelain")
	if c := strings.Count(wtOut, "worktree "); c != 1 {
		t.Errorf("worktree count = %d, want 1 (the command creates none):\n%s", c, wtOut)
	}
	if _, err := os.Stat(filepath.Join(rig.repoRoot, ".nightme", "gtw.yml")); !os.IsNotExist(err) {
		t.Errorf("the command must not write .nightme/gtw.yml (stat err = %v)", err)
	}
	if rig.cs.QueueLen() != 0 {
		t.Errorf("cs.QueueLen = %d, want 0 (the command dispatches no agent)", rig.cs.QueueLen())
	}
}

// TestAssign_Spec pins the /help surface: the usage line and the
// subcommand entry.
func TestAssign_Spec(t *testing.T) {
	spec := NewFactory(NewManager()).Spec()
	if !strings.Contains(spec.Usage, "/gtw assign <issue-id> [--remove|-r]") {
		t.Errorf("Spec().Usage does not list the assign command:\n%s", spec.Usage)
	}
	var found bool
	for _, sub := range spec.Subcommands {
		if sub.Name != "assign" {
			continue
		}
		found = true
		if sub.Summary == "" {
			t.Errorf("the assign subcommand has no summary")
		}
		if !strings.Contains(sub.Usage, "/gtw assign <issue-id>") {
			t.Errorf("the assign subcommand usage = %q, want the assign usage", sub.Usage)
		}
	}
	if !found {
		t.Errorf("Spec().Subcommands has no assign entry")
	}
}
