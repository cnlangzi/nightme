package gtw

// Tests for the PR half of /gtw close: the pull request the branch
// opened gets its `nightme/*` state labels taken off before the
// branch is deleted, a failure there only warns, and a branch
// without a pull request says nothing at all.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
)

// seedPR seeds the pull request the branch opened, together with
// the labels it carries. The state is "merged": the close runs after
// the merge, which is exactly why the lookup is not limited to the
// open pull requests.
func seedPR(t *testing.T, prov *fakeGitProvider, head string, number int, labels ...string) {
	t.Helper()
	prov.SetPRForBranch(head, &PR{Number: number, State: "merged"})
	prov.SetPRLabels(number, labels)
}

// settledOutcomeOfClose returns the outcome the close settled with,
// read from the rig's recorder.
func settledOutcomeOfClose(t *testing.T, rec *closeTestRecCh) *messages.Outcome {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, m := range rec.sends {
		if m.Kind == messages.OutMessageSettled {
			return m.Outcome
		}
	}
	return nil
}

// assertPRLabels fails unless the PR carries exactly `want`.
func assertPRLabels(t *testing.T, prov *fakeGitProvider, number int, want ...string) {
	t.Helper()
	got := prov.PRLabels(number)
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("PR labels = %v, want %q to be present", got, w)
		}
	}
	for _, g := range got {
		if !slices.Contains(want, g) {
			t.Errorf("PR labels = %v, want %q to be absent", got, g)
		}
	}
}

// TestRunClose_FinalizesPRLabels pins the PR side of the close: the
// `nightme/*` state labels come off the pull request, the queue
// marker stays (it belongs to the queue, and the queue marker is
// not put on a PR), and the labels outside the namespace are
// untouched.
func TestRunClose_FinalizesPRLabels(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	seedPR(t, prov, "fix/42-test", 7, LabelReady, LabelAssigned, "bug")

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", false); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	assertPRLabels(t, prov, 7, LabelAssigned, "bug")

	// The card names the PR and the label that came off it.
	card := rig.rec.lastText()
	want := "→ pr: cnlangzi/nightme#7 -" + LabelReady
	if !strings.Contains(card, want) {
		t.Errorf("card missing %q:\n%s", want, card)
	}
}

// TestRunClose_PRFinalizeRunsBeforeTheBranchIsDeleted pins the
// order: the pull request is looked up by head branch, and the
// platform only maps a branch to its pull request while the branch
// is still there. The lookup answers an error when it finds the
// branch already deleted — so the close must have asked before the
// branch went away.
func TestRunClose_PRFinalizeRunsBeforeTheBranchIsDeleted(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	prov.SetPRLabels(7, []string{LabelReady})
	prov.SetFindPRForBranchFunc(func(head string) (*PR, error) {
		for _, args := range rig.git.calls {
			if len(args) >= 3 && args[0] == "branch" && args[1] == "-D" {
				return nil, errors.New("the branch is gone")
			}
		}
		return &PR{Number: 7, State: "merged"}, nil
	})

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", false); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	// The lookup ran before the branch was deleted, so the PR was
	// finalized.
	assertPRLabels(t, prov, 7)
	card := rig.rec.lastText()
	if strings.Contains(card, "⚠️") {
		t.Errorf("card must not warn:\n%s", card)
	}
}

// TestRunClose_PRFinalizeFailureWarnsButDoesNotAbort pins the
// deliberate difference to the issue side: the PR's labels are
// bookkeeping, so a failure there only warns. The local cleanup
// runs to the end and the outcome stays a normal one.
func TestRunClose_PRFinalizeFailureWarnsButDoesNotAbort(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	seedPR(t, prov, "fix/42-test", 7, LabelReady)
	prov.SetRemovePRLabelErr(errors.New("403 Forbidden: missing write scope"))

	work := rig.cs.Hold("msg-1")
	ctx := chatsession.WithWork(context.Background(), work)
	if _, err := RunClose(ctx, rig.cs, rig.deps, rig.cs.ChatID, "msg-1", false); err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	card := rig.rec.lastText()
	work.Release()

	// The local cleanup ran: the worktree is gone, the branch is
	// deleted, and the cwd is back at the repo root.
	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd = %q, want %q", got, repoRoot)
	}
	sawRemove, sawBranchDel := false, false
	for _, args := range rig.git.calls {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
			sawRemove = true
		}
		if len(args) >= 3 && args[0] == "branch" && args[1] == "-D" {
			sawBranchDel = true
		}
	}
	if !sawRemove {
		t.Errorf("git worktree remove not called; calls=%v", rig.git.calls)
	}
	if !sawBranchDel {
		t.Errorf("git branch -D not called; calls=%v", rig.git.calls)
	}

	// The warning names the label that stayed on.
	for _, want := range []string{"⚠️", LabelReady, "403 Forbidden"} {
		if !strings.Contains(card, want) {
			t.Errorf("card missing %q:\n%s", want, card)
		}
	}

	// The outcome is a normal one: the close did its work.
	got := settledOutcomeOfClose(t, rig.rec)
	if got == nil {
		t.Fatal("no settled outcome")
	}
	if got.Status != messages.OutcomeOK {
		t.Errorf("outcome status = %s, want ok — the local cleanup ran", got.Status)
	}
}

// TestRunClose_NoPR_SaysNothing pins that a branch without a pull
// request is not worth a word: the close completes and the reply
// carries no PR row and no warning.
func TestRunClose_NoPR_SaysNothing(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", false); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	card := rig.rec.lastText()
	if strings.Contains(card, "→ pr:") {
		t.Errorf("card must not carry a PR row:\n%s", card)
	}
	if strings.Contains(card, "⚠️") {
		t.Errorf("card must not warn about a pull request that is not there:\n%s", card)
	}
	if calls := prov.CallsByMethod("RemovePRLabel"); len(calls) != 0 {
		t.Errorf("RemovePRLabel calls = %+v, want 0", calls)
	}
}

// TestRunClose_ModeLocal_FinalizesPRLabels pins that the PR side
// does not depend on an issue: a local fix has none, and the pull
// request it opened is finalized all the same.
func TestRunClose_ModeLocal_FinalizesPRLabels(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	rig.deps.Detect = fakeDetect(prov)
	seedFix(t, rig, wt, repoRoot)
	rig.git.remoteURLResp = "https://github.com/cnlangzi/nightme.git"
	seedPR(t, prov, "fix/42-test", 7, LabelReady)

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", false); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	assertPRLabels(t, prov, 7)
	card := rig.rec.lastText()
	want := "→ pr: cnlangzi/nightme#7 -" + LabelReady
	if !strings.Contains(card, want) {
		t.Errorf("card missing %q:\n%s", want, card)
	}
}

// TestRunClose_PRFinalize_ResetOnlyRemoves pins that `--reset` on
// the PR side only takes labels off: the close never puts a label
// on a pull request, not even the `nightme/done` the issue side
// attaches.
func TestRunClose_PRFinalize_ResetOnlyRemoves(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	seedPR(t, prov, "fix/42-test", 7, LabelReady)

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", true); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	assertPRLabels(t, prov, 7)
	card := rig.rec.lastText()
	if strings.Contains(card, "+"+LabelDone) {
		t.Errorf("the PR must not report an added label:\n%s", card)
	}
}
