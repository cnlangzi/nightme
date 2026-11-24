package gtw

// Tests for the label finalization step of /gtw close: the default
// close leaves the issue's `nightme/*` labels alone, the `--done`
// close finalises them, the queue marker is never touched, a
// platform failure leaves the local state intact, and a failed local
// step puts the labels back.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/command"
)

// seedRemoteLabels rewrites the label set of the seeded issue (42)
// so a test can start from a specific tracker state. The issue is
// also registered as the repository's open issue, so the queue
// filter sees it.
func seedRemoteLabels(t *testing.T, prov *fakeGitProvider, labels ...string) {
	t.Helper()
	issue := &Issue{
		ID:     42,
		Title:  "feat: the claimed one",
		State:  "open",
		Labels: labels,
	}
	prov.SetIssue(42, issue)
	prov.SetListIssues([]*Issue{issue})
}

// assertLabels fails unless the issue carries exactly `want`.
func assertLabels(t *testing.T, prov *fakeGitProvider, want ...string) {
	t.Helper()
	got := prov.IssueLabels(42)
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("issue labels = %v, want %q to be present", got, w)
		}
	}
	for _, g := range got {
		if !slices.Contains(want, g) {
			t.Errorf("issue labels = %v, want %q to be absent", got, g)
		}
	}
}

// TestRunClose_ModeRemote_DefaultTouchesNoLabel pins the default
// close: the issue's labels are exactly what they were — the claim
// stays, a `nightme/done` from an earlier cycle stays — and the
// close asks the platform for nothing at all. Ending the local
// session says nothing about the work being finished, so the tracker
// state is left to the human.
func TestRunClose_ModeRemote_DefaultTouchesNoLabel(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	seedRemoteLabels(t, prov, LabelAssigned, LabelWIP, LabelDone)

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", false); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	// The labels are untouched.
	assertLabels(t, prov, LabelAssigned, LabelWIP, LabelDone)

	// Not one provider method ran.
	if calls := prov.Calls(); len(calls) != 0 {
		t.Errorf("provider calls = %+v, want none (the default close does not reach the tracker)", calls)
	}

	// The local teardown still happened.
	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd = %q, want %q", got, repoRoot)
	}
}

// TestRunClose_ModeRemote_DefaultSurvivesAnUnreachablePlatform pins
// the offline promise of the default close: with the tracker
// unreachable (expired auth, no network) the local session is torn
// down all the same, and no platform failure surfaces.
func TestRunClose_ModeRemote_DefaultSurvivesAnUnreachablePlatform(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	seedRemoteLabels(t, prov, LabelAssigned, LabelWIP)

	// The tracker is unreachable: the provider detection fails and
	// the repository has no origin remote.
	rig.deps.Detect = func(context.Context, string, HTTPProber, string) (GitProvider, error) {
		return nil, errors.New("git host unreachable")
	}
	rig.git.remoteURLResp = ""

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", false); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd = %q, want %q (the local session is torn down)", got, repoRoot)
	}
	card := rig.rec.lastText()
	if !strings.Contains(card, "closed") {
		t.Errorf("reply must report the close:\n%s", card)
	}
	if strings.Contains(card, "⚠️") {
		t.Errorf("reply must not warn about a platform the close never called:\n%s", card)
	}
	assertLabels(t, prov, LabelAssigned, LabelWIP)
}

// TestRunClose_ModeRemote_FinalizesLabels pins the `--done` close:
// every `nightme/*` label but the queue marker comes off the issue
// and `nightme/done` goes on. The removal set comes from the
// issue's own labels — a hand-made `Nightme/Custom` is a nightme
// label too — and the labels outside the namespace are untouched.
func TestRunClose_ModeRemote_FinalizesLabels(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	seedRemoteLabels(t, prov, LabelWIP, LabelReady, "Nightme/Custom", "bug")

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", true); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	assertLabels(t, prov, LabelDone, "bug")

	// The card names the issue and every label that moved.
	card := rig.rec.lastText()
	want := "→ issue: cnlangzi/nightme#42 -" + LabelWIP + " -" + LabelReady + " -Nightme/Custom +" + LabelDone
	if !strings.Contains(card, want) {
		t.Errorf("card missing %q:\n%s", want, card)
	}
}

// TestRunClose_ModeRemote_QueueMarkerUntouched pins that the queue
// marker survives the close in both modes — it is the user's label,
// not the close's.
func TestRunClose_ModeRemote_QueueMarkerUntouched(t *testing.T) {
	for _, tc := range []struct {
		name string
		done bool
		want []string
	}{
		{"default", false, []string{LabelAssigned, LabelWIP}},
		{"done", true, []string{LabelAssigned, LabelDone}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wt := t.TempDir()
			repoRoot := t.TempDir()

			rig := newCloseRig(t)
			prov := newFakeGitProvider(ProviderGitHub, "github.com")
			seedFixRemote(t, rig, prov, wt, repoRoot)
			seedRemoteLabels(t, prov, LabelAssigned, LabelWIP)

			if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", tc.done); err != nil {
				t.Fatalf("RunClose: %v", err)
			}

			assertLabels(t, prov, tc.want...)
		})
	}
}

// TestRunClose_ModeRemote_AddFailureAbortsBeforeRemoval pins the
// add-before-remove order: a failed attach leaves the issue with
// the state labels it had, and no label comes off.
func TestRunClose_ModeRemote_AddFailureAbortsBeforeRemoval(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	seedRemoteLabels(t, prov, LabelAssigned, LabelWIP)
	prov.SetAddIssueLabelErr(errors.New("403 Forbidden: missing write scope"))

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", true); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	// No label came off, and the one that was to be added is not on.
	if n := len(prov.CallsByMethod("RemoveIssueLabel")); n != 0 {
		t.Errorf("RemoveIssueLabel calls = %d, want 0 (the add failed first)", n)
	}
	assertLabels(t, prov, LabelAssigned, LabelWIP)

	// The local state is untouched — the user can retry.
	if got := rig.cs.SelectedCwd(); got != wt {
		t.Errorf("SelectedCwd = %q, want %q (unchanged for retry)", got, wt)
	}
	for _, args := range rig.git.calls {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
			t.Errorf("git worktree remove called despite the label failure: %v", args)
		}
	}

	reply := rig.rec.lastText()
	for _, want := range []string{"could not add label", LabelDone, "403 Forbidden", "retry /gtw close"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing %q:\n%s", want, reply)
		}
	}
}

// TestRunClose_ModeRemote_RemoveFailureAbortsClose pins that a
// failed removal gates the local cleanup: the worktree, the branch
// and the yml stay where they are.
func TestRunClose_ModeRemote_RemoveFailureAbortsClose(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	seedRemoteLabels(t, prov, LabelAssigned, LabelWIP)
	prov.SetRemoveIssueLabelErr(errors.New("403 Forbidden: missing write scope"))

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", true); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	if got := rig.cs.SelectedCwd(); got != wt {
		t.Errorf("SelectedCwd = %q, want %q (unchanged for retry)", got, wt)
	}
	for _, args := range rig.git.calls {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
			t.Errorf("git worktree remove called despite the label failure: %v", args)
		}
	}

	reply := rig.rec.lastText()
	for _, want := range []string{"could not remove label", LabelWIP, "403 Forbidden", "retry /gtw close"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing %q:\n%s", want, reply)
		}
	}
}

// TestRunClose_ModeRemote_IssueNotFound_WarnsAndContinues pins the
// 404 path: a deleted issue can never be labelled, so the close
// warns and carries on with the local cleanup.
func TestRunClose_ModeRemote_IssueNotFound_WarnsAndContinues(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	prov.SetGetIssueErr(42, fmt.Errorf("%w: issue 42 is gone", ErrIssueNotFound))

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", true); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	// No label was touched.
	for _, method := range []string{"AddIssueLabel", "RemoveIssueLabel"} {
		if calls := prov.CallsByMethod(method); len(calls) != 0 {
			t.Errorf("%s calls = %+v, want 0", method, calls)
		}
	}

	// The local cleanup ran: the worktree is gone, the branch is
	// deleted, and the cwd is back at the repo root.
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
	if got := rig.cs.SelectedCwd(); got != repoRoot {
		t.Errorf("SelectedCwd = %q, want %q", got, repoRoot)
	}

	// The card carries the warning.
	card := rig.rec.lastText()
	if !strings.Contains(card, "⚠️") {
		t.Errorf("card must carry the warning:\n%s", card)
	}
	if !strings.Contains(card, "not found") {
		t.Errorf("card must name the missing issue:\n%s", card)
	}
}

// TestRunClose_ModeRemote_LocalFailureRestoresLabels pins the
// compensation: a local step that fails after the labels changed
// puts the issue back the way it was, so a retry converges.
func TestRunClose_ModeRemote_LocalFailureRestoresLabels(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	seedRemoteLabels(t, prov, LabelAssigned, LabelWIP)
	rig.git.worktreeRemoveErr = &fakeExitError{code: 128, msg: "worktree remove failed"}

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", true); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	// The issue carries the pre-close labels again.
	assertLabels(t, prov, LabelAssigned, LabelWIP)

	// The reply names the restore.
	reply := rig.rec.lastText()
	if !strings.Contains(reply, "restored") {
		t.Errorf("reply must carry the restore status line:\n%s", reply)
	}
}

// TestRunClose_ModeRemote_CompensationFailureIsReported pins that a
// failed restore is named on the reply and does not turn into a
// second top-level error.
func TestRunClose_ModeRemote_CompensationFailureIsReported(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	seedRemoteLabels(t, prov, LabelAssigned, LabelWIP)
	rig.git.worktreeRemoveErr = &fakeExitError{code: 128, msg: "worktree remove failed"}
	// The restore of the removed label fails; the removal of the
	// added label still runs.
	prov.SetAddIssueLabelFunc(func(id int, label string) error {
		if label == LabelWIP {
			return errors.New("403 Forbidden: missing write scope")
		}
		return nil
	})

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", true); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	reply := rig.rec.lastText()
	if !strings.Contains(reply, "could not restore") {
		t.Errorf("reply must name the failed restore:\n%s", reply)
	}
	if !strings.Contains(reply, "403 Forbidden") {
		t.Errorf("reply must echo the restore error:\n%s", reply)
	}
	// The original error is still the headline — the restore
	// failure did not replace it.
	if !strings.Contains(reply, "git worktree remove failed") {
		t.Errorf("reply must keep the original failure:\n%s", reply)
	}
}

// TestRunClose_ModeLocal_NoLabelCall pins that a local fix has no
// issue to label: the close makes no platform call at all, in either
// mode.
func TestRunClose_ModeLocal_NoLabelCall(t *testing.T) {
	for _, done := range []bool{false, true} {
		wt := t.TempDir()
		repoRoot := t.TempDir()

		rig := newCloseRig(t)
		prov := newFakeGitProvider(ProviderGitHub, "github.com")
		rig.deps.Detect = fakeDetect(prov)
		seedFix(t, rig, wt, repoRoot)

		if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", done); err != nil {
			t.Fatalf("RunClose: %v", err)
		}

		if calls := prov.Calls(); len(calls) != 0 {
			t.Errorf("provider calls = %+v, want none (a local fix has no issue)", calls)
		}
		card := rig.rec.lastText()
		if strings.Contains(card, "→ issue:") {
			t.Errorf("card must not carry an issue row:\n%s", card)
		}
	}
}

// TestRunClose_ModeRemote_NoNightmeLabels_DoneStillGoesOn pins that
// the `--done` close marks the work done even when the issue carried
// no nightme label at all: there is nothing to take off, and the
// card says what went on.
func TestRunClose_ModeRemote_NoNightmeLabels_DoneStillGoesOn(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	seedRemoteLabels(t, prov, "bug")

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", true); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	assertLabels(t, prov, LabelDone, "bug")
	card := rig.rec.lastText()
	if !strings.Contains(card, "→ issue: cnlangzi/nightme#42 +"+LabelDone) {
		t.Errorf("card must report the label that went on:\n%s", card)
	}
}

// TestRunClose_ModeRemote_StaleDoneStays pins that a stale
// `nightme/done` (left by an earlier cycle) survives the `--done`
// close: the close puts the label on, so taking it off again would
// undo the finalization.
func TestRunClose_ModeRemote_StaleDoneStays(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	seedRemoteLabels(t, prov, LabelAssigned, LabelWIP, LabelDone)

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", true); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	assertLabels(t, prov, LabelAssigned, LabelDone)
}

// TestRunClose_ModeRemote_FinalizedIssueLeavesTheQueue pins the
// point of the finalization: the closed issue is not offered again
// by the automatic pick — it is finished work, not a claim.
func TestRunClose_ModeRemote_FinalizedIssueLeavesTheQueue(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	seedRemoteLabels(t, prov, LabelAssigned, LabelWIP)

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", true); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	issues, err := prov.ListIssues(context.Background(), "cnlangzi", "nightme")
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	cands, held := assignedCandidates(issues)
	if len(cands) != 0 {
		t.Errorf("candidates = %+v, want none (the closed issue is done)", cands)
	}
	if held != 1 {
		t.Errorf("held = %d, want 1 (the closed issue carries the done marker)", held)
	}
}

// TestRunClose_ModeRemote_RestoreKeepsAPreexistingDone pins that
// the restore puts the issue back exactly as it was: a `nightme/done`
// the issue already carried (left by an earlier cycle) is not a
// label this close added, so the restore must leave it on.
func TestRunClose_ModeRemote_RestoreKeepsAPreexistingDone(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	seedRemoteLabels(t, prov, LabelAssigned, LabelWIP, LabelDone)
	rig.git.worktreeRemoveErr = &fakeExitError{code: 128, msg: "worktree remove failed"}

	if _, err := RunClose(context.Background(), rig.cs, rig.deps, rig.cs.ChatID, "msg-1", true); err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	// The pre-close label set, exactly.
	assertLabels(t, prov, LabelAssigned, LabelWIP, LabelDone)
}

// --- the `--done` flag on the command line ---

// TestCloseCmdSpec pins the argv grammar: `--done` is a boolean
// flag, and anything else in the tail is rejected.
func TestCloseCmdSpec(t *testing.T) {
	parsed, err := command.ParseCmdArgs([]string{"--done"}, closeCmdSpec)
	if err != nil {
		t.Fatalf("ParseCmdArgs(--done): %v", err)
	}
	if !parsed.Bool("done") {
		t.Error("done = false, want true")
	}

	// `--done` is the only flag the close knows.
	if len(closeCmdSpec.Flags) != 1 {
		t.Errorf("closeCmdSpec flags = %+v, want only --done", closeCmdSpec.Flags)
	}
	if _, ok := closeCmdSpec.Flags["--reset"]; ok {
		t.Error("closeCmdSpec still knows --reset")
	}

	for _, argv := range [][]string{
		{"--force"},
		{"-r"},
		{"extra"},
		{"--done", "extra"},
	} {
		if _, err := command.ParseCmdArgs(argv, closeCmdSpec); err == nil {
			t.Errorf("ParseCmdArgs(%q) = nil, want an error", argv)
		} else if !strings.Contains(err.Error(), "Usage: /gtw close [--done]") {
			t.Errorf("error lacks the usage tail: %q", err)
		}
	}
}

// TestFactory_Handle_Close_DoneReachesRunClose pins that the flag
// survives the dispatch: `/gtw close --done` finalises the issue's
// state labels and marks the work done.
func TestFactory_Handle_Close_DoneReachesRunClose(t *testing.T) {
	wt := t.TempDir()
	repoRoot := t.TempDir()

	rig := newCloseRig(t)
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	seedFixRemote(t, rig, prov, wt, repoRoot)
	seedRemoteLabels(t, prov, LabelAssigned, LabelWIP)

	f := NewFactoryWithDeps(NewManager(), rig.deps)
	got, err := f.Handle(context.Background(), command.RuntimeServices{},
		nil, rig.cs,
		command.SlashInput{
			Text:      "/gtw close --done",
			Args:      []string{"gtw", "close", "--done"},
			ChatID:    rig.cs.ChatID,
			MessageID: "msg-1",
		})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !got.Consumed {
		t.Errorf("expected Consumed, got %+v", got)
	}

	assertLabels(t, prov, LabelAssigned, LabelDone)
}

// TestFactory_Handle_Close_ResetIsRejected pins that the removed
// `--reset` gets its own answer: the reply names what to write
// instead, and nothing is torn down — no worktree, no branch, no
// label.
//
// The dash family counts too: a phone keyboard types an em dash for
// the ASCII hyphen, and the lexer folds it onto the same flag — the
// spelling worked before the flag was removed, so it has to reach the
// migration hint as well.
func TestFactory_Handle_Close_ResetIsRejected(t *testing.T) {
	for _, spelling := range []string{"--reset", "—reset"} {
		t.Run(spelling, func(t *testing.T) {
			wt := t.TempDir()
			repoRoot := t.TempDir()

			rig := newCloseRig(t)
			prov := newFakeGitProvider(ProviderGitHub, "github.com")
			seedFixRemote(t, rig, prov, wt, repoRoot)
			seedRemoteLabels(t, prov, LabelAssigned, LabelWIP)

			f := NewFactoryWithDeps(NewManager(), rig.deps)
			got, err := f.Handle(context.Background(), command.RuntimeServices{},
				nil, rig.cs,
				command.SlashInput{
					Text:      "/gtw close " + spelling,
					Args:      []string{"gtw", "close", spelling},
					ChatID:    rig.cs.ChatID,
					MessageID: "msg-1",
				})
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if !got.Consumed {
				t.Errorf("expected Consumed, got %+v", got)
			}

			if !strings.HasPrefix(got.Reply, "❌ ") {
				t.Errorf("reply must start with the failure marker:\n%s", got.Reply)
			}
			for _, want := range []string{"--reset", "no longer exists", "--done"} {
				if !strings.Contains(got.Reply, want) {
					t.Errorf("reply missing %q:\n%s", want, got.Reply)
				}
			}

			// Nothing was torn down: the worktree, the branch, the yml
			// and the issue's labels are all where they were.
			if cwd := rig.cs.SelectedCwd(); cwd != wt {
				t.Errorf("SelectedCwd = %q, want %q (nothing torn down)", cwd, wt)
			}
			for _, args := range rig.git.calls {
				if len(args) >= 2 && args[0] == "worktree" && args[1] == "remove" {
					t.Errorf("git worktree remove called despite the rejected flag: %v", args)
				}
				if len(args) >= 3 && args[0] == "branch" && args[1] == "-D" {
					t.Errorf("git branch -D called despite the rejected flag: %v", args)
				}
			}
			assertLabels(t, prov, LabelAssigned, LabelWIP)
		})
	}
}
