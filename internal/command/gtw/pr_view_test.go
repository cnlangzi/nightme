package gtw

// Tests for `/gtw pr view [number]`.
//
// The command is read-only: it reports the merge state of a pull
// request and changes nothing. The outcome it settles is the
// contract a workflow branches on — `ok` exactly when the pull
// request can be merged, a normal branch (failed, no error
// description) when it cannot, and an exception when the platform
// cannot tell.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// prViewRig is the /gtw pr view test rig: a git repo with an origin
// remote, a chat session pointed at it, and a fake provider behind
// the factory.
type prViewRig struct {
	repoRoot string
	cs       *chatsession.ChatSession
	prov     *fakeGitProvider
	rec      *recordingCh
	factory  *Factory
	ctx      context.Context
	work     *chatsession.Work

	// sleeps counts the injected waits, so a test can assert the
	// retry loop without spending real seconds.
	sleeps int
}

// newPRViewRig builds the rig.
func newPRViewRig(t *testing.T) *prViewRig {
	t.Helper()
	repoRoot := initTempRepo(t)
	mustGit(t, repoRoot, "remote", "add", "origin",
		"https://github.com/cnlangzi/nightme.git")

	rec := &recordingCh{}
	cs, _ := chatsession.New("chat-prview-"+t.Name(), "test-agent")
	cs.WithEmitter(rec)
	if err := cs.SetSelectedCwd(repoRoot); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}

	prov := newFakeGitProvider(ProviderGitHub, "github.com")

	r := &prViewRig{repoRoot: repoRoot, cs: cs, prov: prov, rec: rec}
	deps := HandlerDeps{
		Git:    ExecGitRunner{},
		Detect: fakeDetect(prov),
		Sleep: func(_ context.Context, _ time.Duration) error {
			r.sleeps++
			return nil
		},
	}
	r.factory = NewFactoryWithDeps(NewManager(), deps)
	work := cs.Hold("msg-1")
	r.ctx = chatsession.WithWork(context.Background(), work)
	r.work = work
	return r
}

// drive runs `/gtw pr view <args...>` through the factory and
// returns the card the user read.
func (r *prViewRig) drive(t *testing.T, args ...string) string {
	t.Helper()
	r.driveOut(t, args...)
	return r.rec.lastText()
}

// driveOut runs the command and returns what the factory handed
// back — the reply of the paths that answer in the command's own
// output instead of the chat.
func (r *prViewRig) driveOut(t *testing.T, args ...string) *command.SlashOutput {
	t.Helper()
	argv := append([]string{"gtw", "pr", "view"}, args...)
	out, err := r.factory.Handle(r.ctx, command.RuntimeServices{}, nil, r.cs, command.SlashInput{
		ChatID:    r.cs.ChatID,
		MessageID: "msg-1",
		Text:      "/gtw pr view " + strings.Join(args, " "),
		Args:      argv,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	return out
}

// settle releases the message's work and returns the outcome the
// command reported.
func (r *prViewRig) settle(t *testing.T) *messages.Outcome {
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

// seedMergeState registers the merge state the provider answers for
// the pull request with the given number, and makes that pull
// request the open one of the branch — so the command finds it
// whether it was given the number or not.
func (r *prViewRig) seedMergeState(number int, verdict PRMergeVerdict, raw string) {
	r.prov.SetFindOpenPRResp(&PR{
		Number: number,
		URL:    fmt.Sprintf("https://github.com/cnlangzi/nightme/pull/%d", number),
		State:  "open",
	})
	r.prov.SetPRMergeState(number, &PRMergeState{
		Number:  number,
		URL:     fmt.Sprintf("https://github.com/cnlangzi/nightme/pull/%d", number),
		Head:    "fix-42",
		Base:    "main",
		State:   "open",
		Verdict: verdict,
		Raw:     raw,
	})
}

// TestPRView_MergeableIsOK: a pull request the platform says merges
// settles as `ok`, and the card says the same thing.
func TestPRView_MergeableIsOK(t *testing.T) {
	r := newPRViewRig(t)
	r.seedMergeState(123, PRMergeMergeable, "CLEAN")

	card := r.drive(t)
	out := r.settle(t)
	if out == nil {
		t.Fatal("the command settled no outcome")
	}
	if out.Status != messages.OutcomeOK {
		t.Fatalf("the status is %q, want ok", out.Status)
	}
	if _, ok := out.Data[messages.OutcomeKeyError]; ok {
		t.Errorf("an ok outcome carries an error description: %q", out.Data[messages.OutcomeKeyError])
	}
	for key, want := range map[string]string{
		"pr_number":   "123",
		"pr_url":      "https://github.com/cnlangzi/nightme/pull/123",
		"pr_state":    "open",
		"base_branch": "main",
		"mergeable":   "1",
		"conflict":    "0",
		"merge_state": "mergeable",
	} {
		if out.Data[key] != want {
			t.Errorf("Data[%q] = %q, want %q", key, out.Data[key], want)
		}
	}
	if !strings.HasPrefix(card, "✅ PR #123 is mergeable") {
		t.Errorf("the card is %q", card)
	}
}

// TestPRView_ConflictIsANormalBranch: a conflicting pull request is
// a conclusion, not a breakdown — the outcome carries no error
// description, so a workflow keeps going and branches on
// Data["conflict"].
func TestPRView_ConflictIsANormalBranch(t *testing.T) {
	r := newPRViewRig(t)
	r.seedMergeState(123, PRMergeConflicting, "DIRTY")

	card := r.drive(t)
	out := r.settle(t)
	if out == nil {
		t.Fatal("the command settled no outcome")
	}
	if out.Status != messages.OutcomeFailed {
		t.Fatalf("the status is %q, want failed", out.Status)
	}
	if _, ok := out.Data[messages.OutcomeKeyError]; ok {
		t.Errorf("a normal branch carries an error description: %q", out.Data[messages.OutcomeKeyError])
	}
	if out.Data["reason"] != "merge_conflict" {
		t.Errorf("the reason is %q, want merge_conflict", out.Data["reason"])
	}
	if out.Data["mergeable"] != "0" {
		t.Errorf("Data[mergeable] = %q, want 0", out.Data["mergeable"])
	}
	if out.Data["conflict"] != "1" {
		t.Errorf("Data[conflict] = %q, want 1", out.Data["conflict"])
	}
	if out.Data["merge_state"] != "conflicting" {
		t.Errorf("Data[merge_state] = %q, want conflicting", out.Data["merge_state"])
	}
	if !strings.HasPrefix(card, "❌ PR #123 has merge conflicts") {
		t.Errorf("the card is %q", card)
	}
	// The card carries the platform's own word and the next step.
	if !strings.Contains(card, "→ state:    DIRTY") {
		t.Errorf("the card does not carry the platform's word: %q", card)
	}
	if !strings.Contains(card, "↳ merge main into the branch, then push") {
		t.Errorf("the card carries no hint: %q", card)
	}
}

// TestPRView_BehindIsANormalBranch: a pull request whose head is
// behind its base is a conclusion too.
func TestPRView_BehindIsANormalBranch(t *testing.T) {
	r := newPRViewRig(t)
	r.seedMergeState(123, PRMergeBehind, "BEHIND")

	card := r.drive(t)
	out := r.settle(t)
	if out == nil {
		t.Fatal("the command settled no outcome")
	}
	if out.Status != messages.OutcomeFailed {
		t.Fatalf("the status is %q, want failed", out.Status)
	}
	if _, ok := out.Data[messages.OutcomeKeyError]; ok {
		t.Errorf("a normal branch carries an error description: %q", out.Data[messages.OutcomeKeyError])
	}
	if out.Data["reason"] != "pr_behind" {
		t.Errorf("the reason is %q, want pr_behind", out.Data["reason"])
	}
	if out.Data["mergeable"] != "0" || out.Data["conflict"] != "0" {
		t.Errorf("the flags are mergeable=%q conflict=%q, want 0/0",
			out.Data["mergeable"], out.Data["conflict"])
	}
	if out.Data["merge_state"] != "behind" {
		t.Errorf("Data[merge_state] = %q, want behind", out.Data["merge_state"])
	}
	if !strings.HasPrefix(card, "❌ PR #123 is behind main") {
		t.Errorf("the card is %q", card)
	}
}

// TestPRView_BlockedIsANormalBranch: the platform stands in the way
// for a reason of its own — still a conclusion, not a breakdown.
func TestPRView_BlockedIsANormalBranch(t *testing.T) {
	r := newPRViewRig(t)
	r.seedMergeState(123, PRMergeBlocked, "BLOCKED")

	card := r.drive(t)
	out := r.settle(t)
	if out == nil {
		t.Fatal("the command settled no outcome")
	}
	if out.Status != messages.OutcomeFailed {
		t.Fatalf("the status is %q, want failed", out.Status)
	}
	if _, ok := out.Data[messages.OutcomeKeyError]; ok {
		t.Errorf("a normal branch carries an error description: %q", out.Data[messages.OutcomeKeyError])
	}
	if out.Data["reason"] != "pr_blocked" {
		t.Errorf("the reason is %q, want pr_blocked", out.Data["reason"])
	}
	if out.Data["merge_state"] != "blocked" {
		t.Errorf("Data[merge_state] = %q, want blocked", out.Data["merge_state"])
	}
	if !strings.HasPrefix(card, "❌ PR #123 is blocked") {
		t.Errorf("the card is %q", card)
	}
}

// TestPRView_RetriesUntilThePlatformComputesTheState: the platform
// computes the merge state asynchronously. The command waits for it
// instead of reporting "not ready yet" as the answer.
func TestPRView_RetriesUntilThePlatformComputesTheState(t *testing.T) {
	r := newPRViewRig(t)
	r.prov.SetFindOpenPRResp(&PR{Number: 123, URL: "u", State: "open"})
	reads := 0
	r.prov.SetPRMergeStateFunc(func(number int) (*PRMergeState, error) {
		reads++
		if reads == 1 {
			return &PRMergeState{Number: number, State: "open", Verdict: PRMergeUnknown, Raw: "UNKNOWN"}, nil
		}
		return &PRMergeState{Number: number, State: "open", Verdict: PRMergeMergeable, Raw: "CLEAN"}, nil
	})

	card := r.drive(t)
	out := r.settle(t)
	if reads != 2 {
		t.Errorf("the state was read %d times, want 2", reads)
	}
	if r.sleeps != 1 {
		t.Errorf("the command waited %d times, want 1", r.sleeps)
	}
	if out == nil {
		t.Fatal("the command settled no outcome")
	}
	if out.Status != messages.OutcomeOK {
		t.Fatalf("the status is %q, want ok", out.Status)
	}
	if out.Data["merge_state"] != "mergeable" {
		t.Errorf("Data[merge_state] = %q, want mergeable", out.Data["merge_state"])
	}
	if !strings.HasPrefix(card, "✅ PR #123 is mergeable") {
		t.Errorf("the card is %q", card)
	}
}

// TestPRView_UnknownAfterTheRetriesIsAnException: a platform that
// never computes the state is a breakdown — the run hands the
// question to the model.
func TestPRView_UnknownAfterTheRetriesIsAnException(t *testing.T) {
	r := newPRViewRig(t)
	r.prov.SetFindOpenPRResp(&PR{Number: 123, URL: "u", State: "open"})
	reads := 0
	r.prov.SetPRMergeStateFunc(func(number int) (*PRMergeState, error) {
		reads++
		return &PRMergeState{Number: number, State: "open", Verdict: PRMergeUnknown, Raw: "UNKNOWN"}, nil
	})

	card := r.drive(t)
	out := r.settle(t)
	if reads != prViewMaxAttempts {
		t.Errorf("the state was read %d times, want %d", reads, prViewMaxAttempts)
	}
	if r.sleeps != prViewMaxAttempts-1 {
		t.Errorf("the command waited %d times, want %d", r.sleeps, prViewMaxAttempts-1)
	}
	if out == nil {
		t.Fatal("the command settled no outcome")
	}
	if out.Status != messages.OutcomeFailed {
		t.Fatalf("the status is %q, want failed", out.Status)
	}
	if out.Data["reason"] != "state_unknown" {
		t.Errorf("the reason is %q, want state_unknown", out.Data["reason"])
	}
	if out.Data[messages.OutcomeKeyError] == "" {
		t.Error("an exception carries no error description")
	}
	if out.Data["merge_state"] != "unknown" {
		t.Errorf("Data[merge_state] = %q, want unknown", out.Data["merge_state"])
	}
	if !strings.HasPrefix(card, "❌ PR #123 state is unknown") {
		t.Errorf("the card is %q", card)
	}
}

// TestPRView_ClosedPRIsANoop: a pull request that is no longer open
// is never going to merge — the platform stopped computing its
// merge state — so there is nothing to do.
func TestPRView_ClosedPRIsANoop(t *testing.T) {
	r := newPRViewRig(t)
	r.prov.SetPRMergeState(123, &PRMergeState{
		Number:  123,
		URL:     "https://github.com/cnlangzi/nightme/pull/123",
		Head:    "fix-42",
		Base:    "main",
		State:   "merged",
		Verdict: PRMergeUnknown,
		Raw:     "UNKNOWN",
	})

	card := r.drive(t, "123")
	out := r.settle(t)
	if out == nil {
		t.Fatal("the command settled no outcome")
	}
	if out.Status != messages.OutcomeNoop {
		t.Fatalf("the status is %q, want noop", out.Status)
	}
	if out.Data["reason"] != "pr_closed" {
		t.Errorf("the reason is %q, want pr_closed", out.Data["reason"])
	}
	if out.Data["pr_state"] != "merged" {
		t.Errorf("Data[pr_state] = %q, want merged", out.Data["pr_state"])
	}
	// The seven keys are there whenever the pull request was read.
	for _, key := range []string{
		"pr_number", "pr_url", "pr_state", "base_branch",
		"mergeable", "conflict", "merge_state",
	} {
		if _, ok := out.Data[key]; !ok {
			t.Errorf("Data[%q] is missing", key)
		}
	}
	if out.Data["mergeable"] != "0" {
		t.Errorf("Data[mergeable] = %q, want 0", out.Data["mergeable"])
	}
	if _, ok := out.Data[messages.OutcomeKeyError]; ok {
		t.Errorf("a noop carries an error description: %q", out.Data[messages.OutcomeKeyError])
	}
	if !strings.HasPrefix(card, "✨ PR #123 is already merged") {
		t.Errorf("the card is %q", card)
	}
}

// TestPRView_NoOpenPRIsANoop: the branch has no open pull request.
func TestPRView_NoOpenPRIsANoop(t *testing.T) {
	r := newPRViewRig(t)
	r.prov.SetFindOpenPRResp(nil)

	card := r.drive(t)
	out := r.settle(t)
	if out == nil {
		t.Fatal("the command settled no outcome")
	}
	if out.Status != messages.OutcomeNoop {
		t.Fatalf("the status is %q, want noop", out.Status)
	}
	if out.Data["reason"] != "no_pr" {
		t.Errorf("the reason is %q, want no_pr", out.Data["reason"])
	}
	if !strings.HasPrefix(card, "✨ no open pull request for ") {
		t.Errorf("the card is %q", card)
	}
	// The provider is asked once — the branch lookup — and never
	// for a merge state that does not exist.
	if calls := r.prov.CallsByMethod("GetPRMergeState"); len(calls) != 0 {
		t.Errorf("the merge state was read %d times, want 0", len(calls))
	}
}

// TestPRView_NotFoundIsAnException: the number names no pull
// request.
func TestPRView_NotFoundIsAnException(t *testing.T) {
	r := newPRViewRig(t)
	r.prov.SetPRMergeStateErr(999, fmt.Errorf("%w: PR #999", ErrPRNotFound))

	card := r.drive(t, "999")
	out := r.settle(t)
	if out == nil {
		t.Fatal("the command settled no outcome")
	}
	if out.Status != messages.OutcomeFailed {
		t.Fatalf("the status is %q, want failed", out.Status)
	}
	if out.Data["reason"] != "pr_not_found" {
		t.Errorf("the reason is %q, want pr_not_found", out.Data["reason"])
	}
	if out.Data[messages.OutcomeKeyError] == "" {
		t.Error("an exception carries no error description")
	}
	if !strings.HasPrefix(card, "❌ no pull request #999") {
		t.Errorf("the card is %q", card)
	}
}

// TestPRView_ProviderFailureIsAnException: the CLI is missing, the
// token expired, the network is down — the command cannot answer.
func TestPRView_ProviderFailureIsAnException(t *testing.T) {
	r := newPRViewRig(t)
	r.prov.SetPRMergeStateErr(123, errors.New("gh: authentication required"))

	card := r.drive(t, "123")
	out := r.settle(t)
	if out == nil {
		t.Fatal("the command settled no outcome")
	}
	if out.Status != messages.OutcomeFailed {
		t.Fatalf("the status is %q, want failed", out.Status)
	}
	if out.Data["reason"] != "provider_error" {
		t.Errorf("the reason is %q, want provider_error", out.Data["reason"])
	}
	if out.Data[messages.OutcomeKeyError] == "" {
		t.Error("an exception carries no error description")
	}
	if !strings.Contains(card, "authentication required") {
		t.Errorf("the card drops the provider's own message: %q", card)
	}
}

// TestPRView_BadNumberIsAUsageError: the command never guesses which
// pull request the user meant.
func TestPRView_BadNumberIsAUsageError(t *testing.T) {
	r := newPRViewRig(t)

	reply := r.driveOut(t, "abc")
	out := r.settle(t)
	if out == nil {
		t.Fatal("the command settled no outcome")
	}
	if out.Status != messages.OutcomeFailed {
		t.Fatalf("the status is %q, want failed", out.Status)
	}
	if out.Data["reason"] != "usage" {
		t.Errorf("the reason is %q, want usage", out.Data["reason"])
	}
	if out.Data[messages.OutcomeKeyError] == "" {
		t.Error("an exception carries no error description")
	}
	if !strings.HasPrefix(reply.Reply, "❌ ") {
		t.Errorf("the reply is %q", reply.Reply)
	}
}

// TestPRView_ZeroIsAUsageError: a pull request number starts at 1.
func TestPRView_ZeroIsAUsageError(t *testing.T) {
	r := newPRViewRig(t)

	r.drive(t, "0")
	out := r.settle(t)
	if out == nil {
		t.Fatal("the command settled no outcome")
	}
	if out.Data["reason"] != "usage" {
		t.Errorf("the reason is %q, want usage", out.Data["reason"])
	}
}

// TestPRView_NoWorkspaceIsAnException: the command needs a
// workspace to know which branch it works on.
func TestPRView_NoWorkspaceIsAnException(t *testing.T) {
	rec := &recordingCh{}
	cs, _ := chatsession.New("chat-prview-nows-"+t.Name(), "test-agent")
	cs.WithEmitter(rec)

	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	deps := HandlerDeps{
		Git:    ExecGitRunner{},
		Detect: fakeDetect(prov),
	}
	factory := NewFactoryWithDeps(NewManager(), deps)
	work := cs.Hold("msg-1")
	ctx := chatsession.WithWork(context.Background(), work)

	if _, err := factory.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    cs.ChatID,
		MessageID: "msg-1",
		Text:      "/gtw pr view",
		Args:      []string{"gtw", "pr", "view"},
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	work.Release()
	var out *messages.Outcome
	for _, m := range rec.sends {
		if m.Kind == messages.OutMessageSettled {
			out = m.Outcome
		}
	}
	if out == nil {
		t.Fatal("the command settled no outcome")
	}
	if out.Status != messages.OutcomeFailed {
		t.Fatalf("the status is %q, want failed", out.Status)
	}
	if out.Data["reason"] != "no_cwd" {
		t.Errorf("the reason is %q, want no_cwd", out.Data["reason"])
	}
}

// TestPRView_WithoutANumberReadsTheBranch: the number is optional —
// the command reads the open pull request of the branch the chat
// works on.
func TestPRView_WithoutANumberReadsTheBranch(t *testing.T) {
	r := newPRViewRig(t)
	r.seedMergeState(55, PRMergeMergeable, "CLEAN")

	card := r.drive(t)
	out := r.settle(t)
	if out == nil {
		t.Fatal("the command settled no outcome")
	}
	if out.Status != messages.OutcomeOK {
		t.Fatalf("the status is %q, want ok", out.Status)
	}
	if out.Data["pr_number"] != "55" {
		t.Errorf("Data[pr_number] = %q, want 55", out.Data["pr_number"])
	}
	if !strings.HasPrefix(card, "✅ PR #55 is mergeable") {
		t.Errorf("the card is %q", card)
	}
}

// TestPRView_WithANumberIgnoresTheBranch: the number names the pull
// request, whichever branch the chat works on.
func TestPRView_WithANumberIgnoresTheBranch(t *testing.T) {
	r := newPRViewRig(t)
	r.seedMergeState(123, PRMergeMergeable, "CLEAN")

	r.drive(t, "123")
	out := r.settle(t)
	if out == nil {
		t.Fatal("the command settled no outcome")
	}
	if out.Data["pr_number"] != "123" {
		t.Errorf("Data[pr_number] = %q, want 123", out.Data["pr_number"])
	}
	if calls := r.prov.CallsByMethod("FindOpenPRForBranch"); len(calls) != 0 {
		t.Errorf("the branch was looked up %d times, want 0", len(calls))
	}
}

// TestPRView_ReadsNothingElse: the command is read-only. It runs no
// hook, opens no pull request and touches no label.
func TestPRView_ReadsNothingElse(t *testing.T) {
	r := newPRViewRig(t)
	r.seedMergeState(123, PRMergeMergeable, "CLEAN")

	r.drive(t, "123")
	for _, method := range []string{"CreatePR", "AddIssueLabel", "RemoveIssueLabel", "CreateLabel"} {
		if calls := r.prov.CallsByMethod(method); len(calls) != 0 {
			t.Errorf("%s was called %d times, want 0", method, len(calls))
		}
	}
}

// TestPRView_IsHiddenFromTheAgentTool: the run's agent tool lists
// the subcommands it may issue. "pr view" sits next to "pr" — one
// letter apart, and picking the wrong one opens a pull request — so
// it stays out of the list the model reads.
func TestPRView_IsHiddenFromTheAgentTool(t *testing.T) {
	f := NewFactoryWithDeps(NewManager(), HandlerDeps{})
	spec := f.Spec()

	var hidden bool
	for _, sub := range spec.Subcommands {
		if sub.Name != "pr view" {
			continue
		}
		hidden = sub.Hidden
		if sub.Usage != "/gtw pr view [number]" {
			t.Errorf("the usage is %q", sub.Usage)
		}
	}
	if !hidden {
		t.Error("the `pr view` subcommand is not hidden")
	}
	if !strings.Contains(spec.Usage, "/gtw pr view [number]") {
		t.Errorf("the usage line does not mention the command: %q", spec.Usage)
	}
}

// TestPRView_DoesNotChangeThePRPath: `/gtw pr` still opens a pull
// request — the read-only path is a separate branch of the dispatch.
func TestPRView_DoesNotChangeThePRPath(t *testing.T) {
	if isPRView([]string{"gtw", "pr"}) {
		t.Error("`/gtw pr` is read as `pr view`")
	}
	if !isPRView([]string{"gtw", "pr", "view"}) {
		t.Error("`/gtw pr view` is not read as `pr view`")
	}
	if !isPRView([]string{"gtw", "pr", "view", "123"}) {
		t.Error("`/gtw pr view 123` is not read as `pr view`")
	}
}
