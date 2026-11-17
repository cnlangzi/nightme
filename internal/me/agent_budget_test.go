package me

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// budgetWorkflow is a workflow with one step: the step the run
// spends its budget on.
const budgetWorkflow = `title: Do the one thing.
jobs:
  main:
    steps:
      - id: commit
        message: /gtw commit
        note: the commit comes first
`

// waitPaused waits until the run of a chat waits for the operator.
func waitPaused(t *testing.T, svc *Service, chatID string) *run {
	t.Helper()
	svc.mu.Lock()
	r := svc.active[chatID]
	svc.mu.Unlock()
	if r == nil {
		t.Fatal("the chat has no run in it")
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if r.isPaused() {
			return r
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the run did not pause")
	return r
}

// TestBudget_TheRunPausesWhenTheStepSpentItsBudget — the budget of a
// step is spent: the run waits for a human and keeps its place.
func TestBudget_TheRunPausesWhenTheStepSpentItsBudget(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", budgetWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: textMsg("I think I should commit.")},
	}}
	svc, op := testService(t, dir, Config{Provider: provider, MaxJudgments: 2})
	op.steps = []fakeStep{pendingStep("still going")}

	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	svc.mu.Lock()
	r := svc.runs[runID]
	svc.mu.Unlock()
	waitPaused(t, svc, "obs-1")

	if phase := r.currentPhase(); phase != PhasePaused {
		t.Fatalf("the run stands at %q, want %q", phase, PhasePaused)
	}
	waitEntry(t, op, "paused:")
	// The pause notice rides on the card of the round: the card of
	// the run is the static overview of the run.
	var named bool
	for _, entry := range op.Entries(op.Cards()[1].ID) {
		if strings.Contains(entry, `the step "commit" used up its budget (2 of 2 chances)`) {
			named = true
		}
	}
	if !named {
		t.Errorf("the card does not name the step and its chances: %v", op.Entries(op.Cards()[1].ID))
	}

	// The chat is told once that the run waits for a human.
	waitSpoken(t, op, "The run paused")
	var notices int
	for _, msg := range op.Spoken() {
		if strings.Contains(msg.Text, "The run paused") {
			notices++
		}
	}
	if notices != 1 {
		t.Errorf("the chat got %d notices about the pause, want 1: %v", notices, op.Spoken())
	}

	// The state of the run is kept: it is resumable.
	if _, err := os.Stat(filepath.Join(dir, "runs", runID+".json")); err != nil {
		t.Errorf("the state of the run is gone: %v", err)
	}
}

// TestBudget_ContinueHandsTheControlBackWithAFreshBudget — the
// operator takes the chat back and hands the control to the run: the
// note joins the conversation and the step's budget starts over.
func TestBudget_ContinueHandsTheControlBackWithAFreshBudget(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", budgetWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: textMsg("I think I should commit.")},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "the operator took over"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider, MaxJudgments: 2})
	op.steps = []fakeStep{pendingStep("still going")}

	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	svc.mu.Lock()
	r := svc.runs[runID]
	svc.mu.Unlock()
	waitPaused(t, svc, "obs-1")

	if err := svc.Continue(context.Background(), "obs-1", "the branch was reset, try again"); err != nil {
		t.Fatalf("continue: %v", err)
	}
	waitRun(t, svc, "obs-1")

	// The agent was asked again, and it read the note of the operator.
	if provider.Calls() < 2 {
		t.Fatalf("the chat model was called %d times, want the agent to be asked again", provider.Calls())
	}
	req := provider.Request(1)
	var carried bool
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if strings.Contains(p.Text, "the branch was reset, try again") {
				carried = true
			}
		}
	}
	if !carried {
		t.Fatalf("the agent did not read the note of the operator: %v", req.Messages)
	}

	// The step's chances are back to zero.
	r.mu.Lock()
	idx := r.stepIndex("commit")
	left := r.policy.Remaining().Steps[idx]
	r.mu.Unlock()
	if idx < 0 {
		t.Fatal("the workflow has no commit step")
	}
	if left != 2 {
		t.Errorf("the step has %d chances left, want the whole budget of 2", left)
	}
}

// TestBudget_TheStepRefusesAnotherCommandWhenItsBudgetIsSpent — the
// budget bounds what the agent may do at the step.
func TestBudget_TheStepRefusesAnotherCommandWhenItsBudgetIsSpent(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", budgetWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{
			{Type: llm.PartToolCall, ToolCallID: "c1", ToolName: "ntm_gtw", Args: json.RawMessage(`{"args":""}`)},
			{Type: llm.PartToolCall, ToolCallID: "c2", ToolName: "ntm_gtw", Args: json.RawMessage(`{"args":""}`)},
		}}},
	}}
	svc, op := testService(t, dir, Config{
		Provider:     provider,
		MaxJudgments: 2,
	})
	op.steps = []fakeStep{pendingStep("still going"), okStep("the first fix"), okStep("the second fix")}

	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
		Commands: []CommandInfo{{Name: "gtw", Summary: "the gtw command"}},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	svc.mu.Lock()
	r := svc.runs[runID]
	svc.mu.Unlock()
	waitPaused(t, svc, "obs-1")

	// The second repair was refused: the step had no chance left.
	want := "already ran 2 times, its limit is 2"
	var found bool
	for _, m := range r.conversation() {
		for _, p := range m.Parts {
			if strings.Contains(p.Text, want) {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("the agent was not told that the budget is spent: %v", r.conversation())
	}
}

// proseProvider answers with words once and then holds the turn: the
// run stays in the turn of the chat model, so the state it left
// behind does not move.
type proseProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *proseProvider) Complete(ctx context.Context, _ llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	if n == 1 {
		return &llm.Response{Message: textMsg("I think I should commit.")}, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestBudget_AStallCostsAChance — a turn of the chat model that
// produced no tool call costs a chance of the step.
func TestBudget_AStallCostsAChance(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", budgetWorkflow)

	provider := &proseProvider{}
	svc, op := testService(t, dir, Config{Provider: provider, MaxJudgments: 50})
	op.steps = []fakeStep{pendingStep("still going")}

	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	svc.mu.Lock()
	r := svc.runs[runID]
	svc.mu.Unlock()
	waitStalls(t, svc, "obs-1", 1)

	r.mu.Lock()
	stalls := r.policy.ConsecutiveStalls()
	idx := r.stepIndex("commit")
	left := r.policy.Remaining().Steps[idx]
	r.mu.Unlock()
	if stalls != 1 {
		t.Errorf("the history holds %d stalls, want 1", stalls)
	}
	if left != 48 {
		t.Errorf("the step has %d chances left, want 48 (the execution and the stall took two)", left)
	}
}

// TestProviderFailure_TheRunKeepsAsking — a provider that fails is
// not the end of the run: the run waits and asks again.
func TestProviderFailure_TheRunKeepsAsking(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", budgetWorkflow)

	provider := &scriptedProvider{err: errors.New("the chat model is broken")}
	svc, op := testService(t, dir, Config{Provider: provider, MaxJudgments: 3})
	op.steps = []fakeStep{pendingStep("still going")}

	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	svc.mu.Lock()
	r := svc.runs[runID]
	svc.mu.Unlock()

	// The run asks again, and again: the budget is the only thing that
	// stops it.
	waitCalls(t, provider, 2)
	if phase := r.currentPhase(); phase == PhaseFailed {
		t.Fatalf("the run ended as %q, want it to keep going", phase)
	}
	waitPaused(t, svc, "obs-1")
	if phase := r.currentPhase(); phase != PhasePaused {
		t.Fatalf("the run stands at %q, want %q", phase, PhasePaused)
	}
}

// TestProviderFailure_TheAttemptsAreSpacedByTheBackoff — the run
// waits longer after every failed attempt.
func TestProviderFailure_TheAttemptsAreSpacedByTheBackoff(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", budgetWorkflow)

	provider := &scriptedProvider{err: errors.New("the chat model is broken")}
	var mu sync.Mutex
	var waits []time.Duration
	svc, op := testService(t, dir, Config{
		Provider: provider,
		Sleep: func(_ context.Context, d time.Duration) error {
			mu.Lock()
			waits = append(waits, d)
			mu.Unlock()
			return nil
		},
	})
	op.steps = []fakeStep{pendingStep("still going")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitCalls(t, provider, 3)

	mu.Lock()
	defer mu.Unlock()
	if len(waits) < 2 {
		t.Fatalf("the run waited %d times, want at least 2", len(waits))
	}
	if waits[0] != firstProviderWait {
		t.Errorf("the first wait = %v, want %v", waits[0], firstProviderWait)
	}
	if waits[1] != 2*firstProviderWait {
		t.Errorf("the second wait = %v, want %v", waits[1], 2*firstProviderWait)
	}
}

// deadlineProvider records the deadline of the turn it was given and
// holds it.
//
// The turn stays in flight, so the deadline the run set is observable
// while the run is still asking.
type deadlineProvider struct {
	mu       sync.Mutex
	calls    int
	calledAt time.Time
	deadline time.Time
	has      bool
}

func (p *deadlineProvider) Complete(ctx context.Context, _ llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	p.calls++
	deadline, ok := ctx.Deadline()
	if !p.has {
		p.has = ok
		p.deadline = deadline
		p.calledAt = time.Now()
	}
	p.mu.Unlock()
	if !ok {
		return &llm.Response{Message: textMsg("thinking")}, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestProviderFailure_TheTurnIsBounded — one turn of the chat model
// does not last forever: it carries its own deadline.
//
// The run does not inherit the client's own timeout, and a provider
// that never answers does not look like a frozen run.
func TestProviderFailure_TheTurnIsBounded(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", budgetWorkflow)

	provider := &deadlineProvider{}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		provider.mu.Lock()
		has := provider.has
		until := provider.deadline
		at := provider.calledAt
		provider.mu.Unlock()
		if has {
			// The span of the turn is measured against the moment the
			// call was made: a slow host cannot make it look shorter.
			span := until.Sub(at)
			if span > maxAgentTurn {
				t.Errorf("the turn lasts %v, want at most %v", span, maxAgentTurn)
			}
			if span < maxAgentTurn-time.Minute {
				t.Errorf("the turn lasts %v, want it to be about %v", span, maxAgentTurn)
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the chat model was never called")
}

// TestTheSituationReport_CarriesTheRepair — the report the agent
// reads says what it has to repair and what it may do.
func TestTheSituationReport_CarriesTheRepair(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", budgetWorkflow)

	repo := repoWithOrigin(t)
	svc, _ := testService(t, dir, Config{Git: realGitRunner{}})
	wf, ok := svc.catalog.Lookup("one")
	if !ok {
		t.Fatal("the workflow is not in the catalogue")
	}
	bound, err := wfe.Bind(wf, map[string]string{})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	r := newRun(svc, Request{
		Name: "one", ChatID: "obs-1", Cwd: repo,
		Commands: []CommandInfo{{Name: "gtw", Summary: "the gtw command"}},
	}, bound, map[string]string{})

	r.setJudgment(`the previous step "commit" ended as "failed"`)
	r.setJudgmentStep("commit")
	r.record(wfe.ActionKindStall, "", "commit", wfe.Outcome{Status: wfe.OutcomeUnknown})

	report := svc.renderSituation(context.Background(), r)
	for _, want := range []string{
		"repository:",
		"branch:",
		"dirty:",
		"base: origin/main",
		"ahead of base:",
		"repair:",
		"step: commit",
		"note: the commit comes first",
		"chances left:",
		"turns without an action: 1",
		"step commands now: /gtw commit",
		"directives now:",
		"resume",
		"next",
		"ntm_gtw",
		"end_round",
		"ask_human",
		"finish",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the situation does not carry %q:\n%s", want, report)
		}
	}
}

// TestTheCard_ShowsTheWaitingLineAndTheChancesLeft — the card says
// what the run waits for and what the step has left.
func TestTheCard_ShowsTheWaitingLineAndTheChancesLeft(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", budgetWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolFinish, map[string]any{"status": "failed", "summary": "the step did not work"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider, MaxJudgments: 7})
	op.steps = []fakeStep{pendingStep("still going")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	var chances bool
	for _, item := range op.Checklist(op.Cards()[1].ID) {
		if strings.Contains(item.ActiveForm, "chances left") {
			chances = true
		}
	}
	if !chances {
		t.Errorf("the card does not carry the chances left: %+v", op.Checklist(op.Cards()[1].ID))
	}
}

// TestTheRunDoesNotFailOnAProseReply — no path in the personal agent
// ends a run because the chat model did not call a tool.
func TestTheRunDoesNotFailOnAProseReply(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", budgetWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: textMsg("I think I should commit.")},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going")}

	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	svc.mu.Lock()
	r := svc.runs[runID]
	svc.mu.Unlock()
	waitCalls(t, provider, 1)

	if phase := r.currentPhase(); phase == PhaseFailed {
		t.Fatalf("the run ended as %q, want it to keep going", phase)
	}
	if op.Has("did not call a tool") {
		t.Errorf("the card says the run failed for a prose reply: %v", op.Lines())
	}
}

// waitStalls waits until the history of the run holds that many
// stalls.
func waitStalls(t *testing.T, svc *Service, chatID string, n int) {
	t.Helper()
	svc.mu.Lock()
	r := svc.active[chatID]
	svc.mu.Unlock()
	if r == nil {
		t.Fatal("the chat has no run in it")
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		stalls := r.policy.ConsecutiveStalls()
		r.mu.Unlock()
		if stalls >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the history holds no stall")
}

// waitCard waits until the run's overview carries the given text.
func waitCard(t *testing.T, op *fakeOperator, text string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if op.Has(text) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the card never carried %q: %v", text, op.Lines())
}

// waitEntry waits until the run published an entry with the given
// text.
func waitEntry(t *testing.T, op *fakeOperator, text string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range op.EmittedOf(messages.OutReply) {
			if strings.Contains(m.Text, text) {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the run published no entry with %q: %v", text, op.EmittedOf(messages.OutReply))
}

// TestTheRunReplansAfterEveryTurn — the plan decides whether the
// agent is needed at all.
//
// A turn of the chat model that produced no tool call does not change
// the plan: the run asks again. Once the plan can go on by itself, the
// step runs on the fast path and no model is asked.
func TestTheRunReplansAfterEveryTurn(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "two", `title: two
jobs:
  main:
    steps:
      - id: commit
        message: /gtw commit
      - id: push
        message: /gtw push
`)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: textMsg("I think I should commit.")},
		{Message: toolCallMsg("c2", toolResume, map[string]any{})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{pendingStep("still going"), okStep("committed"), okStep("pushed")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "two", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	// The agent was asked twice: the prose turn and the turn that ran
	// the step again. The second step ran on the fast path.
	if provider.Calls() != 2 {
		t.Fatalf("the chat model was called %d times, want 2", provider.Calls())
	}
	sent := op.Sent()
	var pushed bool
	for _, cmd := range sent {
		if cmd == "/gtw push" {
			pushed = true
		}
	}
	if !pushed {
		t.Fatalf("the second step never ran on the fast path: %v", sent)
	}
}

// alwaysProse answers with words every time.
type alwaysProse struct {
	mu    sync.Mutex
	calls int
}

func (p *alwaysProse) Complete(_ context.Context, _ llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return &llm.Response{Message: textMsg("I think I should commit.")}, nil
}

// alwaysInstruct calls the instruction tool every time.
type alwaysInstruct struct {
	mu    sync.Mutex
	calls int
}

func (p *alwaysInstruct) Complete(_ context.Context, _ llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return &llm.Response{Message: toolCallMsg("c1", toolInstruct, map[string]any{"text": "fix it"})}, nil
}

// TestBudget_AJudgmentWithoutAStepIsBounded — a judgment that names
// no step — the plan forks — still costs chances.
//
// The stall has to land on a step the budget can see, or nothing
// bounds the run.
func TestBudget_AJudgmentWithoutAStepIsBounded(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fork", `title: fork
jobs:
  a:
    steps:
      - id: a1
        message: /gtw a
  b:
    steps:
      - id: b1
        message: /gtw b
`)

	provider := &alwaysProse{}
	svc, op := testService(t, dir, Config{Provider: provider, MaxJudgments: 3})
	op.steps = []fakeStep{okStep("a"), okStep("b")}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fork", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitPaused(t, svc, "obs-1")

	provider.mu.Lock()
	calls := provider.calls
	provider.mu.Unlock()
	if calls > 4 {
		t.Errorf("the chat model was called %d times, want the budget to stop the run", calls)
	}
}

// TestBudget_AnInstructionCostsAChance — an instruction is a repair:
// it is work the agent causes, so it costs a chance of the step.
func TestBudget_AnInstructionCostsAChance(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", budgetWorkflow)

	provider := &alwaysInstruct{}
	svc, op := testService(t, dir, Config{Provider: provider, MaxJudgments: 3})
	op.steps = []fakeStep{{outcome: messages.Outcome{
		Status:  messages.OutcomeFailed,
		Summary: "the command failed",
	}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitPaused(t, svc, "obs-1")

	provider.mu.Lock()
	calls := provider.calls
	provider.mu.Unlock()
	if calls > 3 {
		t.Errorf("the chat model was called %d times, want the budget to stop the run", calls)
	}
}

// TestBudget_TheConversationKeepsOneSituationReport — the report is
// rendered again on every turn, and only the latest one is kept: a
// conversation that holds one report per turn grows past what the
// provider accepts.
func TestBudget_TheConversationKeepsOneSituationReport(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", budgetWorkflow)

	provider := &alwaysProse{}
	svc, op := testService(t, dir, Config{Provider: provider, MaxJudgments: 6})
	op.steps = []fakeStep{{outcome: messages.Outcome{
		Status:  messages.OutcomeFailed,
		Summary: "the command failed",
	}}}

	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	svc.mu.Lock()
	r := svc.runs[runID]
	svc.mu.Unlock()
	waitPaused(t, svc, "obs-1")

	var reports int
	for _, m := range r.conversation() {
		if m.Role == llm.RoleUser && strings.HasPrefix(m.Text(), situationMark) {
			reports++
		}
	}
	if reports != 1 {
		t.Errorf("the conversation holds %d situation reports, want 1", reports)
	}
}
