package gtw

// Tests for `/gtw pr --ready`: the flag's own grammar, the label
// transition it performs on the linked issue, and the warnings the
// transition produces.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/messages"
)

// --- the flag ---

// TestParsePRArgs_Ready pins the boolean flag: `--ready` parses on
// its own and reports as set.
func TestParsePRArgs_Ready(t *testing.T) {
	out, err := parsePRArgs([]string{"--ready"})
	if err != nil {
		t.Fatalf("parsePRArgs: %v", err)
	}
	if !out.Ready {
		t.Fatalf("Ready = false, want true")
	}
	if out.Agent != "" {
		t.Errorf("Agent = %q, want empty", out.Agent)
	}
}

// TestParsePRArgs_ReadyWithAgent pins that the two flags combine:
// the agent override and the boolean flag are independent.
func TestParsePRArgs_ReadyWithAgent(t *testing.T) {
	out, err := parsePRArgs([]string{"--ready", "-a", "codex"})
	if err != nil {
		t.Fatalf("parsePRArgs: %v", err)
	}
	if !out.Ready {
		t.Errorf("Ready = false, want true")
	}
	if out.Agent != "codex" {
		t.Errorf("Agent = %q, want codex", out.Agent)
	}
}

// TestParsePRArgs_WithoutReady pins the default: no flag, no
// transition.
func TestParsePRArgs_WithoutReady(t *testing.T) {
	out, err := parsePRArgs(nil)
	if err != nil {
		t.Fatalf("parsePRArgs: %v", err)
	}
	if out.Ready {
		t.Errorf("Ready = true, want false")
	}
}

// TestReadyIsRejectedByPushAndCommit pins that the flag belongs to
// /gtw pr alone: the shared agent-only grammar of /gtw push and
// /gtw commit rejects it with the lexer's unknown-flag error.
func TestReadyIsRejectedByPushAndCommit(t *testing.T) {
	if _, err := parsePushArgs([]string{"--ready"}); err == nil {
		t.Fatalf("parsePushArgs accepted --ready")
	} else if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("push error = %q, want the unknown-flag error", err.Error())
	}
	if _, err := parseCommitArgs([]string{"--ready"}); err == nil {
		t.Fatalf("parseCommitArgs accepted --ready")
	} else if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("commit error = %q, want the unknown-flag error", err.Error())
	}
}

// TestSpecUsage_NamesTheReadyFlag pins the usage strings: the /gtw
// help text and the subcommand list both name the flag.
func TestSpecUsage_NamesTheReadyFlag(t *testing.T) {
	f := NewFactoryWithDeps(NewManager(), HandlerDeps{})
	spec := f.Spec()
	if !strings.Contains(spec.Usage, "/gtw pr [-a <agent>] [--ready]") {
		t.Errorf("usage does not name the flag:\n%s", spec.Usage)
	}
	for _, sub := range spec.Subcommands {
		if sub.Name != "pr" {
			continue
		}
		if sub.Usage != "/gtw pr [-a <agent>] [--ready]" {
			t.Errorf("pr subcommand usage = %q, want it to name the flag", sub.Usage)
		}
		return
	}
	t.Fatalf("no pr subcommand in the spec")
}

// --- the transition ---

// labelCallIndex returns the position of the first recorded call to
// `method` with `label`, or -1 when the call was never made.
func labelCallIndex(calls []fakeProviderCall, method, label string) int {
	for i, c := range calls {
		if c.Method == method && c.Label == label {
			return i
		}
	}
	return -1
}

// readyRig wires a /gtw pr rig whose worktree links issue 42 and
// whose provider can open a pull request.
func readyRig(t *testing.T) *prTestRig {
	t.Helper()
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{Branch: "wt", Issue: 42})
	setupPRGit(rig, "wt", 0)
	rig.git.on("remote", "git@github.com:octocat/hello.git", "", nil)
	rig.prov.SetCreatePRResp("https://github.com/octocat/hello/pull/7")
	rig.prov.SetIssue(42, &Issue{ID: 42, Title: "t", URL: "u", Body: "b"})
	rig.installDeps()
	return rig
}

// TestDispatchPR_Ready_MovesTheIssueToReady pins the transition:
// the issue gains `nightme/ready` and loses `nightme/wip`, and the
// add comes before the remove.
func TestDispatchPR_Ready_MovesTheIssueToReady(t *testing.T) {
	rig := readyRig(t)
	cs := rig.cs
	captureCh(t, cs)
	if _, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg",
		prArgs{Ready: true}, Config{}); err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}

	calls := rig.prov.Calls()
	addAt := labelCallIndex(calls, "AddIssueLabel", LabelReady)
	rmAt := labelCallIndex(calls, "RemoveIssueLabel", LabelWIP)
	if addAt < 0 {
		t.Fatalf("the issue did not gain %q; calls = %+v", LabelReady, calls)
	}
	if rmAt < 0 {
		t.Fatalf("the issue did not lose %q; calls = %+v", LabelWIP, calls)
	}
	if addAt > rmAt {
		t.Errorf("%q must be added before %q is removed; calls = %+v", LabelReady, LabelWIP, calls)
	}
	// The transition touches the linked issue only.
	for _, c := range calls {
		if (c.Method == "AddIssueLabel" || c.Method == "RemoveIssueLabel") && c.ID != 42 {
			t.Errorf("label call on issue %d, want 42", c.ID)
		}
	}
}

// TestDispatchPR_Ready_AFailedRemovalKeepsTheClaim covers the
// failure between the two steps: the removal fails, so the issue
// keeps its claim and still carries the ready label.
func TestDispatchPR_Ready_AFailedRemovalKeepsTheClaim(t *testing.T) {
	rig := readyRig(t)
	rig.prov.SetRemoveIssueLabelErr(errors.New("403 forbidden"))
	cs := rig.cs
	s := captureCh(t, cs)
	if _, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg",
		prArgs{Ready: true}, Config{}); err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}

	calls := rig.prov.Calls()
	if labelCallIndex(calls, "AddIssueLabel", LabelReady) < 0 {
		t.Errorf("the issue did not gain %q; calls = %+v", LabelReady, calls)
	}
	r := s.lastText()
	mustContain(t, r, "⚠️")
	mustContain(t, r, LabelWIP)
}

// TestDispatchPR_Ready_AFailedAddKeepsTheClaim covers the other
// half of the ordering: the add fails, so the removal does not run
// and the issue stays claimed.
func TestDispatchPR_Ready_AFailedAddKeepsTheClaim(t *testing.T) {
	rig := readyRig(t)
	rig.prov.SetAddIssueLabelErr(errors.New("404 label not found"))
	cs := rig.cs
	s := captureCh(t, cs)
	if _, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg",
		prArgs{Ready: true}, Config{}); err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}

	calls := rig.prov.Calls()
	if labelCallIndex(calls, "RemoveIssueLabel", LabelWIP) >= 0 {
		t.Errorf("the claim was dropped although %q was never added; calls = %+v", LabelReady, calls)
	}
	r := s.lastText()
	mustContain(t, r, "⚠️")
	mustContain(t, r, LabelReady)
}

// TestDispatchPR_Ready_LabelFailureKeepsTheOutcome pins that a
// failing label call does not fail the command: the outcome stays
// `ok` and the reply names the failed call.
func TestDispatchPR_Ready_LabelFailureKeepsTheOutcome(t *testing.T) {
	rig := readyRig(t)
	rig.prov.SetRemoveIssueLabelErr(errors.New("403 forbidden"))
	work := rig.cs.Hold("msg")
	ctx := chatsession.WithWork(context.Background(), work)
	ch := captureCh(t, rig.cs)
	if _, err := dispatchPR(ctx, rig.cs, rig.deps, "chat", "msg",
		prArgs{Ready: true}, Config{}); err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	reply := ch.lastText()
	work.Release()

	got := settledOutcomeOf(ch)
	if got == nil {
		t.Fatal("no settled outcome")
	}
	if got.Status != messages.OutcomeOK {
		t.Errorf("outcome status = %s, want ok — the pull request is open", got.Status)
	}
	mustContain(t, reply, "⚠️")
}

// TestDispatchPR_Ready_ExistingPRStillTransitions pins that the
// transition also runs on the path where a pull request for the
// branch was already open.
func TestDispatchPR_Ready_ExistingPRStillTransitions(t *testing.T) {
	rig := readyRig(t)
	rig.prov.SetFindOpenPRResp(&PR{
		Number: 42,
		URL:    "https://github.com/octocat/hello/pull/42",
		State:  "open",
	})
	cs := rig.cs
	s := captureCh(t, cs)
	if _, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg",
		prArgs{Ready: true}, Config{}); err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}

	calls := rig.prov.Calls()
	if labelCallIndex(calls, "AddIssueLabel", LabelReady) < 0 {
		t.Errorf("the issue did not gain %q; calls = %+v", LabelReady, calls)
	}
	if labelCallIndex(calls, "RemoveIssueLabel", LabelWIP) < 0 {
		t.Errorf("the issue did not lose %q; calls = %+v", LabelWIP, calls)
	}
	r := s.lastText()
	mustContain(t, r, "already has an open PR")
}

// TestDispatchPR_Ready_PRExistsRaceStillTransitions pins that the
// transition also runs on the race window where a pull request
// appeared between the gate and CreatePR: a pull request is open for
// the branch, so the issue is waiting for review.
func TestDispatchPR_Ready_PRExistsRaceStillTransitions(t *testing.T) {
	rig := readyRig(t)
	rig.prov.SetCreatePRErr(fmt.Errorf("%w: a pull request for wt already exists", ErrPRExists))
	cs := rig.cs
	s := captureCh(t, cs)
	if _, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg",
		prArgs{Ready: true}, Config{}); err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}

	calls := rig.prov.Calls()
	if labelCallIndex(calls, "AddIssueLabel", LabelReady) < 0 {
		t.Errorf("the issue did not gain %q; calls = %+v", LabelReady, calls)
	}
	if labelCallIndex(calls, "RemoveIssueLabel", LabelWIP) < 0 {
		t.Errorf("the issue did not lose %q; calls = %+v", LabelWIP, calls)
	}
	mustContain(t, s.lastText(), "already exists")
}

// TestDispatchPR_Ready_NoPRNoLabelCall pins that the transition
// does not run when no pull request is open for the branch: the
// pull request could not be created, so the labels stay as they
// were.
func TestDispatchPR_Ready_NoPRNoLabelCall(t *testing.T) {
	rig := readyRig(t)
	rig.prov.SetCreatePRErr(errors.New("create PR failed: boom"))
	cs := rig.cs
	s := captureCh(t, cs)
	if _, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg",
		prArgs{Ready: true}, Config{}); err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}

	calls := rig.prov.Calls()
	if labelCallIndex(calls, "AddIssueLabel", LabelReady) >= 0 {
		t.Errorf("the labels changed although no pull request was opened; calls = %+v", calls)
	}
	if labelCallIndex(calls, "RemoveIssueLabel", LabelWIP) >= 0 {
		t.Errorf("the labels changed although no pull request was opened; calls = %+v", calls)
	}
	if labelCallIndex(calls, "AddPRLabel", LabelReady) >= 0 {
		t.Errorf("the pull request was labelled although no pull request was opened; calls = %+v", calls)
	}
	mustContain(t, s.lastText(), "create PR failed")
}

// TestDispatchPR_Ready_NoLinkedIssueWarns pins the local-mode case:
// the worktree links no issue, so the issue side has nothing to
// change and the reply says so.
func TestDispatchPR_Ready_NoLinkedIssueWarns(t *testing.T) {
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{Branch: "wt"})
	setupPRGit(rig, "wt", 0)
	rig.git.on("remote", "git@github.com:octocat/hello.git", "", nil)
	rig.prov.SetCreatePRResp("https://github.com/octocat/hello/pull/7")
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	if _, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg",
		prArgs{Ready: true}, Config{}); err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}

	if calls := rig.prov.CallsByMethod("CreatePR"); len(calls) != 1 {
		t.Fatalf("CreatePR calls = %d, want 1", len(calls))
	}
	r := s.lastText()
	mustContain(t, r, "the issue labels were not changed")
	mustContain(t, r, "✅ PR opened")
}

// TestDispatchPR_Ready_CardGainsNoRow pins that the transition does
// not add a row to the success card: the card is about the pull
// request, the state lives on the issue.
func TestDispatchPR_Ready_CardGainsNoRow(t *testing.T) {
	withReady := readyRig(t)
	cs := withReady.cs
	ch := captureCh(t, cs)
	if _, err := dispatchPR(context.Background(), cs, withReady.deps, "chat", "msg",
		prArgs{Ready: true}, Config{}); err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	readyRows := cardRowKeys(ch.lastText())

	without := readyRig(t)
	cs2 := without.cs
	ch2 := captureCh(t, cs2)
	if _, err := dispatchPR(context.Background(), cs2, without.deps, "chat", "msg",
		prArgs{}, Config{}); err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	plainRows := cardRowKeys(ch2.lastText())

	if strings.Join(readyRows, ",") != strings.Join(plainRows, ",") {
		t.Errorf("the card rows changed with --ready: %v, want %v", readyRows, plainRows)
	}
}

// --- the pull request side ---

// prLabelCall returns the single AddPRLabel call the provider
// recorded, failing the test when there is not exactly one.
func prLabelCall(t *testing.T, rig *prTestRig) fakeProviderCall {
	t.Helper()
	calls := rig.prov.CallsByMethod("AddPRLabel")
	if len(calls) != 1 {
		t.Fatalf("AddPRLabel calls = %d, want 1; calls = %+v", len(calls), rig.prov.Calls())
	}
	return calls[0]
}

// TestDispatchPR_Ready_LabelsTheOpenedPullRequest pins the pull
// request side on the path where this run opened the pull request:
// the pull request gains `nightme/ready` — the reviewer sees the
// state on the pull request, not only on the issue.
func TestDispatchPR_Ready_LabelsTheOpenedPullRequest(t *testing.T) {
	rig := readyRig(t)
	cs := rig.cs
	captureCh(t, cs)
	if _, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg",
		prArgs{Ready: true}, Config{}); err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}

	call := prLabelCall(t, rig)
	if call.ID != 7 {
		t.Errorf("AddPRLabel number = %d, want 7 (the number of the PR this run opened)", call.ID)
	}
	if call.Label != LabelReady {
		t.Errorf("AddPRLabel label = %q, want %q", call.Label, LabelReady)
	}
}

// TestDispatchPR_Ready_LabelsTheExistingPullRequest pins the pull
// request side on the path where the branch already had an open pull
// request: the pull request that was already open gains the label.
func TestDispatchPR_Ready_LabelsTheExistingPullRequest(t *testing.T) {
	rig := readyRig(t)
	rig.prov.SetFindOpenPRResp(&PR{
		Number: 42,
		URL:    "https://github.com/octocat/hello/pull/42",
		State:  "open",
	})
	cs := rig.cs
	captureCh(t, cs)
	if _, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg",
		prArgs{Ready: true}, Config{}); err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}

	call := prLabelCall(t, rig)
	if call.ID != 42 {
		t.Errorf("AddPRLabel number = %d, want 42 (the PR already open for the branch)", call.ID)
	}
	if call.Label != LabelReady {
		t.Errorf("AddPRLabel label = %q, want %q", call.Label, LabelReady)
	}
}

// TestDispatchPR_Ready_LabelsTheRacedPullRequest pins the pull
// request side on the race window: the pull request appeared between
// the gate and CreatePR, so its number is resolved from the branch
// and the label goes there.
func TestDispatchPR_Ready_LabelsTheRacedPullRequest(t *testing.T) {
	rig := readyRig(t)
	rig.prov.SetCreatePRErr(fmt.Errorf("%w: a pull request for wt already exists", ErrPRExists))
	// The gate must see no pull request; the lookup after the race
	// must find the one that appeared in between.
	var lookups atomic.Int32
	rig.prov.SetFindOpenPRFunc(func(head string) (*PR, error) {
		if lookups.Add(1) == 1 {
			return nil, nil
		}
		return &PR{
			Number: 42,
			URL:    "https://github.com/octocat/hello/pull/42",
			State:  "open",
		}, nil
	})
	cs := rig.cs
	captureCh(t, cs)
	if _, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg",
		prArgs{Ready: true}, Config{}); err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}

	call := prLabelCall(t, rig)
	if call.ID != 42 {
		t.Errorf("AddPRLabel number = %d, want 42 (the PR found on the branch)", call.ID)
	}
	if call.Label != LabelReady {
		t.Errorf("AddPRLabel label = %q, want %q", call.Label, LabelReady)
	}
}

// TestDispatchPR_Ready_NoLinkedIssueStillLabelsThePullRequest pins
// the local-mode case: the worktree links no issue, so the issue
// side has nothing to change — but the pull request still gains the
// label, because the pull request is the review unit.
func TestDispatchPR_Ready_NoLinkedIssueStillLabelsThePullRequest(t *testing.T) {
	rig := newPRTestRig(t)
	setupPRWorktree(t, rig, Context{Branch: "wt"})
	setupPRGit(rig, "wt", 0)
	rig.git.on("remote", "git@github.com:octocat/hello.git", "", nil)
	rig.prov.SetCreatePRResp("https://github.com/octocat/hello/pull/7")
	rig.installDeps()

	cs := rig.cs
	s := captureCh(t, cs)
	if _, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg",
		prArgs{Ready: true}, Config{}); err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}

	call := prLabelCall(t, rig)
	if call.ID != 7 {
		t.Errorf("AddPRLabel number = %d, want 7", call.ID)
	}
	if call.Label != LabelReady {
		t.Errorf("AddPRLabel label = %q, want %q", call.Label, LabelReady)
	}
	mustContain(t, s.lastText(), "the issue labels were not changed")
}

// TestDispatchPR_Ready_UnknownPRNumberWarns pins the fallback that
// fails: the pull request URL carries no number and the branch has
// no open pull request to look up, so the reply says the number
// could not be determined.
func TestDispatchPR_Ready_UnknownPRNumberWarns(t *testing.T) {
	rig := readyRig(t)
	// A URL whose pull request path carries no number.
	rig.prov.SetCreatePRResp("https://github.com/octocat/hello/pull/")
	cs := rig.cs
	s := captureCh(t, cs)
	if _, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg",
		prArgs{Ready: true}, Config{}); err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}

	if calls := rig.prov.CallsByMethod("AddPRLabel"); len(calls) != 0 {
		t.Errorf("AddPRLabel calls = %d, want 0 — the number is unknown", len(calls))
	}
	mustContain(t, s.lastText(), "could not determine the pull request number")
	mustContain(t, s.lastText(), LabelReady)
}

// TestDispatchPR_Ready_PRLabelFailureKeepsTheOutcome pins that a
// failing pull request label call does not fail the command: the
// outcome stays `ok` and the reply names the failed call.
func TestDispatchPR_Ready_PRLabelFailureKeepsTheOutcome(t *testing.T) {
	rig := readyRig(t)
	rig.prov.SetAddPRLabelErr(errors.New("403 forbidden"))
	work := rig.cs.Hold("msg")
	ctx := chatsession.WithWork(context.Background(), work)
	ch := captureCh(t, rig.cs)
	if _, err := dispatchPR(ctx, rig.cs, rig.deps, "chat", "msg",
		prArgs{Ready: true}, Config{}); err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}
	reply := ch.lastText()
	work.Release()

	got := settledOutcomeOf(ch)
	if got == nil {
		t.Fatal("no settled outcome")
	}
	if got.Status != messages.OutcomeOK {
		t.Errorf("outcome status = %s, want ok — the pull request is open", got.Status)
	}
	mustContain(t, reply, "⚠️")
	mustContain(t, reply, "could not add the `nightme/ready` label to PR #7")
}

// TestDispatchPR_Ready_IssueFailureDoesNotBlockThePullRequest pins
// that the two sides are independent: a failing issue-side label call
// does not stop the pull request from gaining the label.
func TestDispatchPR_Ready_IssueFailureDoesNotBlockThePullRequest(t *testing.T) {
	rig := readyRig(t)
	rig.prov.SetAddIssueLabelErr(errors.New("404 label not found"))
	cs := rig.cs
	s := captureCh(t, cs)
	if _, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg",
		prArgs{Ready: true}, Config{}); err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}

	call := prLabelCall(t, rig)
	if call.ID != 7 {
		t.Errorf("AddPRLabel number = %d, want 7", call.ID)
	}
	if call.Label != LabelReady {
		t.Errorf("AddPRLabel label = %q, want %q", call.Label, LabelReady)
	}
	mustContain(t, s.lastText(), "⚠️")
}

// TestDispatchPR_Ready_PRFailureDoesNotBlockTheIssue pins the other
// half of the independence: a failing pull request label call leaves
// the issue transition complete — `nightme/ready` is on, `nightme/wip`
// is off.
func TestDispatchPR_Ready_PRFailureDoesNotBlockTheIssue(t *testing.T) {
	rig := readyRig(t)
	rig.prov.SetAddPRLabelErr(errors.New("403 forbidden"))
	cs := rig.cs
	s := captureCh(t, cs)
	if _, err := dispatchPR(context.Background(), cs, rig.deps, "chat", "msg",
		prArgs{Ready: true}, Config{}); err != nil {
		t.Fatalf("dispatchPR err: %v", err)
	}

	calls := rig.prov.Calls()
	if labelCallIndex(calls, "AddIssueLabel", LabelReady) < 0 {
		t.Errorf("the issue did not gain %q; calls = %+v", LabelReady, calls)
	}
	if labelCallIndex(calls, "RemoveIssueLabel", LabelWIP) < 0 {
		t.Errorf("the issue did not lose %q; calls = %+v", LabelWIP, calls)
	}
	mustContain(t, s.lastText(), "⚠️")
}

// cardRowKeys returns the field names of the card's `→ field: value`
// rows, in order.
func cardRowKeys(card string) []string {
	var keys []string
	for _, line := range strings.Split(card, "\n") {
		rest, ok := strings.CutPrefix(line, "→ ")
		if !ok {
			continue
		}
		key, _, _ := strings.Cut(rest, ":")
		keys = append(keys, strings.TrimSpace(key))
	}
	return keys
}
