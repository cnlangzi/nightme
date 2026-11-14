package me

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/channel/loopback"
	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// ── fakes ─────────────────────────────────────────────────────────

// fakeSender records the lines the run posts to the observer.
type fakeSender struct {
	mu    sync.Mutex
	lines []string
}

func (s *fakeSender) Send(_ context.Context, _ string, text string) error {
	s.mu.Lock()
	s.lines = append(s.lines, text)
	s.mu.Unlock()
	return nil
}

// Lines returns the lines the observer received.
func (s *fakeSender) Lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

// Has reports whether the observer received a line that contains the
// given text.
func (s *fakeSender) Has(text string) bool {
	for _, line := range s.Lines() {
		if strings.Contains(line, text) {
			return true
		}
	}
	return false
}

// fakeStep is one scripted answer of the work chat.
type fakeStep struct {
	// outcome is the verdict the command reports.
	outcome messages.Outcome
	// transcript is what the work agent said.
	transcript []loopback.Entry
	// choices are the prompts the command is waiting for.
	choices []messages.Choice
	// open keeps the command running: it never settles on its own.
	open bool
}

// fakeExec is one tracked execution of the fake loopback.
type fakeExec struct {
	ex       loopback.Execution
	watchers []chan loopback.Execution
}

// fakeLoopback is a scripted loopback channel.
//
// The commands settle on their own unless a step says otherwise.
//
// The setup commands — /cwd and /use — always succeed and never
// consume a scripted step, so a test only scripts the workflow's own
// commands.
type fakeLoopback struct {
	mu      sync.Mutex
	steps   []fakeStep
	sent    []string
	execs   map[string]*fakeExec
	seq     int
	watched []string
	// setupOutcome is the outcome the setup commands report. Nil
	// means they succeed.
	setupOutcome *messages.Outcome
	// failCommand is the command that cannot be sent at all, the way
	// a dead work chat cannot take it.
	failCommand string
}

func newFakeLoopback(steps ...fakeStep) *fakeLoopback {
	return &fakeLoopback{steps: steps, execs: map[string]*fakeExec{}}
}

func (f *fakeLoopback) Send(_ context.Context, chatID, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, text)
	if f.failCommand != "" && text == f.failCommand {
		return "", errors.New("the work chat is gone")
	}
	f.seq++
	id := fmt.Sprintf("msg-%d", f.seq)

	step := fakeStep{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "ok"}}
	if !isSetupCommand(text) {
		if len(f.steps) > 0 {
			step = f.steps[0]
			f.steps = f.steps[1:]
		}
	} else if f.setupOutcome != nil {
		step = fakeStep{outcome: *f.setupOutcome}
	}
	ex := &fakeExec{ex: loopback.Execution{
		MsgID:      id,
		ChatID:     chatID,
		Text:       text,
		Phase:      loopback.PhaseSettled,
		Outcome:    step.outcome,
		Transcript: step.transcript,
		Choices:    step.choices,
	}}
	if step.open {
		ex.ex.Phase = loopback.PhaseRunning
	}
	f.execs[id] = ex
	return id, nil
}

func (f *fakeLoopback) Do(ctx context.Context, chatID, text string) (loopback.Execution, error) {
	id, err := f.Send(ctx, chatID, text)
	if err != nil {
		return loopback.Execution{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.execs[id].ex, nil
}

func (f *fakeLoopback) Get(msgID string) (loopback.Execution, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ex, ok := f.execs[msgID]
	if !ok {
		return loopback.Execution{}, false
	}
	return ex.ex, true
}

func (f *fakeLoopback) Watch(msgID string) <-chan loopback.Execution {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watched = append(f.watched, msgID)
	ch := make(chan loopback.Execution, 4)
	ex, ok := f.execs[msgID]
	if !ok {
		close(ch)
		return ch
	}
	ch <- ex.ex
	if ex.ex.Phase == loopback.PhaseSettled {
		close(ch)
		return ch
	}
	ex.watchers = append(ex.watchers, ch)
	return ch
}

// Answer settles the open execution of a chat, the way answering a
// prompt lets the work agent go on.
func (f *fakeLoopback) Answer(chatID, requestID, optionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ex := range f.execs {
		if ex.ex.ChatID != chatID || ex.ex.Phase == loopback.PhaseSettled {
			continue
		}
		ex.ex.Phase = loopback.PhaseSettled
		ex.ex.Outcome = messages.Outcome{Status: messages.OutcomeOK, Summary: "answered " + optionID}
		for _, w := range ex.watchers {
			w <- ex.ex
			close(w)
		}
		ex.watchers = nil
		return nil
	}
	return errors.New("no execution is waiting")
}

// Watched returns the messages the run watched.
func (f *fakeLoopback) Watched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.watched...)
}

// Sent returns the commands the run sent.
func (f *fakeLoopback) Sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

// isSetupCommand reports whether a command sets the work chat up.
func isSetupCommand(text string) bool {
	return strings.HasPrefix(text, "/cwd ") || strings.HasPrefix(text, "/use ")
}

// fakeShell records the shell commands a run made.
type fakeShell struct {
	mu    sync.Mutex
	calls []wfe.ShellSpec
	ret   *wfe.ShellResult
	err   error
}

func (f *fakeShell) RunShell(_ context.Context, spec wfe.ShellSpec) (*wfe.ShellResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, spec)
	if f.err != nil {
		return nil, f.err
	}
	if f.ret != nil {
		return f.ret, nil
	}
	return &wfe.ShellResult{ExitCode: 0}, nil
}

// blockingShell blocks until its context ends, the way a long command
// does.
type blockingShell struct {
	started   chan struct{}
	mu        sync.Mutex
	cancelled bool
}

func newBlockingShell() *blockingShell {
	return &blockingShell{started: make(chan struct{})}
}

func (b *blockingShell) RunShell(ctx context.Context, _ wfe.ShellSpec) (*wfe.ShellResult, error) {
	close(b.started)
	<-ctx.Done()
	b.mu.Lock()
	b.cancelled = true
	b.mu.Unlock()
	return nil, ctx.Err()
}

// Cancelled reports whether the command was stopped.
func (b *blockingShell) Cancelled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cancelled
}

// fakeAnswerer answers the prompts of the fake loopback.
type fakeAnswerer struct {
	lb *fakeLoopback
}

func (a fakeAnswerer) Answer(chatID, requestID, optionID string) error {
	return a.lb.Answer(chatID, requestID, optionID)
}

// scriptedProvider answers with a scripted list of responses.
type scriptedProvider struct {
	mu        sync.Mutex
	responses []llm.Response
	err       error
	requests  []llm.Request
	// block holds a turn open: the provider waits for the channel to
	// be closed before it answers.
	block chan struct{}
}

func (p *scriptedProvider) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if p.block != nil {
		<-p.block
	}
	if p.err != nil {
		return nil, p.err
	}
	if len(p.responses) == 0 {
		return nil, errors.New("the script is exhausted")
	}
	resp := p.responses[0]
	p.responses = p.responses[1:]
	return &resp, nil
}

// Calls returns the number of calls the provider served.
func (p *scriptedProvider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

// Request returns the request of the n-th call.
func (p *scriptedProvider) Request(n int) llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[n]
}

// scriptedDecider answers with a scripted list of responses.
type scriptedDecider struct {
	mu       sync.Mutex
	answers  []llm.DecisionResponse
	err      error
	requests []llm.DecisionRequest
}

func (d *scriptedDecider) Decide(_ context.Context, req llm.DecisionRequest) (*llm.DecisionResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests = append(d.requests, req)
	if d.err != nil {
		return nil, d.err
	}
	if len(d.answers) == 0 {
		// An exhausted script escalates: the chat model takes over.
		resp := decisionResp("test", optionEscalate, 0, 0)
		return &resp, nil
	}
	resp := d.answers[0]
	d.answers = d.answers[1:]
	return &resp, nil
}

// Calls returns the number of calls the decider served.
func (d *scriptedDecider) Calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.requests)
}

// Request returns the request of the n-th call.
func (d *scriptedDecider) Request(n int) llm.DecisionRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.requests[n]
}

// ── helpers ───────────────────────────────────────────────────────

// oneStepWorkflow is a workflow with a single step.
const oneStepWorkflow = `title: Do the one thing.
jobs:
  main:
    steps:
      - id: commit
        message: /gtw commit
`

// fixIssueWorkflow is the workflow of the integration test.
const fixIssueWorkflow = `title: Fix issue #${{ args.id }} and ship it as a PR.
args:
  - name: id
    default: "1"
jobs:
  main:
    steps:
      - id: fix
        message: /gtw fix ${{ args.id }} -y
      - id: commit
        message: /gtw commit
      - id: review
        message: /review
        note: If it fails or does not finish, continue with /review -c.
        allow: [/review -c, /gtw commit]
        attempts: 5
      - id: push
        message: /gtw push
      - id: pr
        message: /gtw pr
`

// writeWorkflow writes a workflow into a workflows directory.
func writeWorkflow(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
}

// testService builds a personal agent with fakes.
//
// The service is shut down when the test ends. A run that is still
// working keeps writing its state file, and Windows refuses to
// remove a directory that holds a file being written — so a test
// that leaves the service running fails in its own cleanup rather
// than in an assertion. The cleanup runs before the temp dir's own:
// t.TempDir registers its cleanup first, and cleanups run last-in
// first-out.
func testService(t *testing.T, dir string, cfg Config) (*Service, *fakeLoopback, *fakeSender) {
	t.Helper()
	lb := newFakeLoopback()
	sender := &fakeSender{}
	cfg.WorkflowsDir = dir
	cfg.StateDir = filepath.Join(dir, "runs")
	cfg.Loopback = lb
	cfg.Sender = sender
	cfg.Answerer = fakeAnswerer{lb: lb}
	cfg.StepTimeout = 5 * time.Second
	cfg.StallTimeout = 5 * time.Second
	svc := New(cfg)
	t.Cleanup(func() { _ = svc.Shutdown(context.Background()) })
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	return svc, lb, sender
}

// toolCallMsg builds an assistant message that asks for one tool.
func toolCallMsg(id, name string, args map[string]any) llm.Message {
	raw, err := json.Marshal(args)
	if err != nil {
		raw = json.RawMessage("{}")
	}
	return llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{
		Type:       llm.PartToolCall,
		ToolCallID: id,
		ToolName:   name,
		Args:       raw,
	}}}
}

// textMsg builds an assistant message that carries words only.
func textMsg(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: text}}}
}

// decisionResp builds a decision answer.
func decisionResp(model, next string, confidence, needsHuman float64) llm.DecisionResponse {
	return llm.DecisionResponse{
		Model: model,
		Answers: map[string]llm.Answer{
			questionNext: {
				Type:       llm.QuestionChoice,
				Choice:     next,
				Confidence: confidence,
				Probabilities: map[string]float64{
					next: confidence,
				},
			},
			questionNeedsHuman: {
				Type:        llm.QuestionYesNo,
				Probability: needsHuman,
			},
		},
	}
}

// waitRun waits until the run of a chat is over.
func waitRun(t *testing.T, svc *Service, chatID string) {
	t.Helper()
	svc.mu.Lock()
	r := svc.active[chatID]
	svc.mu.Unlock()
	if r == nil {
		return
	}
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not finish")
	}
}

// waitSent waits until the work chat received a command.
func waitSent(t *testing.T, lb *fakeLoopback, command string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, cmd := range lb.Sent() {
			if cmd == command {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the work chat never received %q: %v", command, lb.Sent())
}

// ── starting a run ────────────────────────────────────────────────

func TestStartWorkflow_RefusesASecondRunInTheSameChat(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	svc, _, _ := testService(t, dir, Config{Provider: provider, Decider: decider})

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err == nil {
		t.Fatal("a second run in the same chat was accepted")
	}
}

func TestStartWorkflow_ReportsAnUnknownWorkflow(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, _, _ := testService(t, dir, Config{Provider: &scriptedProvider{}})
	_, err := svc.StartWorkflow(context.Background(), Request{
		Name: "nope", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	})
	if err == nil {
		t.Fatal("an unknown workflow was accepted")
	}
	if !strings.Contains(err.Error(), "no workflow named") {
		t.Fatalf("error = %q, want the unknown workflow", err)
	}
}

func TestStartWorkflow_RefusesExtraArguments(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", `title: one
args:
  - name: id
jobs:
  main:
    steps:
      - message: /gtw commit
`)

	svc, _, _ := testService(t, dir, Config{Provider: &scriptedProvider{}})
	_, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", Args: []string{"42", "43"}, ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	})
	if err == nil {
		t.Fatal("an extra argument was accepted")
	}
	if !strings.Contains(err.Error(), "argument") {
		t.Fatalf("error = %q, want the argument count", err)
	}
}

func TestStartWorkflow_BindsANamedArgument(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", `title: one
args:
  - name: issue
    default: "*"
jobs:
  main:
    steps:
      - id: fix
        message: /gtw fix ${{ args.issue }} -y
`)

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	svc, lb, _ := testService(t, dir, Config{Provider: provider, Decider: decider})

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", Args: []string{"issue=42"}, ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	waitSent(t, lb, "/gtw fix 42 -y")
}

func TestStartWorkflow_RefusesAnUnknownNamedArgument(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, _, _ := testService(t, dir, Config{Provider: &scriptedProvider{}})
	_, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", Args: []string{"nope=1"}, ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	})
	if err == nil {
		t.Fatal("an unknown argument was accepted")
	}
	if !strings.Contains(err.Error(), `unknown argument "nope"`) {
		t.Fatalf("error = %q, want the unknown argument", err)
	}
}

func TestStartWorkflow_ReportsThatTheAgentIsOff(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, _, _ := testService(t, dir, Config{})
	_, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	})
	if err == nil {
		t.Fatal("the run started without a model")
	}
	if !strings.Contains(err.Error(), "me.llm") {
		t.Fatalf("error = %q, want the reason", err)
	}
}

// ── the fast path ──────────────────────────────────────────────────

// TestFastPath_RunsEveryStepWithoutAModel — a workflow whose steps
// all end ok runs on the fast path alone: neither model is asked.
func TestFastPath_RunsEveryStepWithoutAModel(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix-issue", fixIssueWorkflow)

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	svc, lb, sender := testService(t, dir, Config{Provider: provider, Decider: decider})

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name:           "fix-issue",
		Args:           []string{"42"},
		ObserverChatID: "obs-1",
		Cwd:            "/tmp/work",
		Agent:          "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if provider.Calls() != 0 {
		t.Fatalf("the chat model was called %d times, the fast path should not need it", provider.Calls())
	}
	if decider.Calls() != 0 {
		t.Fatalf("the decision model was called %d times, the fast path should not need it", decider.Calls())
	}
	sent := lb.Sent()
	want := []string{"/gtw fix 42 -y", "/gtw commit", "/review", "/gtw push", "/gtw pr"}
	if len(sent) != len(want)+2 {
		t.Fatalf("the work chat got %v, want the setup and %v", sent, want)
	}
	for i, cmd := range want {
		if sent[i+2] != cmd {
			t.Fatalf("command %d was %q, want %q", i, sent[i+2], cmd)
		}
	}
	if !sender.Has("system-1: 0 decisions") {
		t.Fatalf("the summary does not count the decisions: %v", sender.Lines())
	}
}

// TestFastPath_RecordsTheStepOutcomes — the run state holds the
// outcome of every step that ran.
func TestFastPath_RecordsTheStepOutcomes(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", `title: one
jobs:
  main:
    steps:
      - id: fix
        message: /gtw fix 1 -y
      - id: commit
        message: /gtw commit
      - id: push
        message: /gtw push
`)

	// The decision model escalates, so the run waits for the user and
	// stays alive while the test reads its state.
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", optionEscalate, 0.1, 0),
	}}
	svc, lb, sender := testService(t, dir, Config{Decider: decider})
	lb.steps = []fakeStep{
		{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "fixed"}},
		{outcome: messages.Outcome{Status: messages.OutcomeNoop, Summary: "nothing to commit"}},
		{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}},
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, sender, "Which step should run next?")

	svc.mu.Lock()
	r := svc.active["obs-1"]
	svc.mu.Unlock()
	if r == nil {
		t.Fatal("the run is not registered")
	}
	r.mu.Lock()
	steps := append([]wfe.StepState(nil), r.state.Steps...)
	r.mu.Unlock()
	if len(steps) != 3 {
		t.Fatalf("the run recorded %d steps, want 3", len(steps))
	}
	if steps[0].ID != "fix" || steps[0].Outcome.Status != wfe.OutcomeOK {
		t.Errorf("the first step = %+v, want fix with an ok outcome", steps[0])
	}
	if steps[1].ID != "commit" || steps[1].Outcome.Status != wfe.OutcomeNoop {
		t.Errorf("the second step = %+v, want commit with a noop outcome", steps[1])
	}
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")
}

// TestFastPath_CarriesTheOutcomeData — the facts a step reported reach
// the run state.
func TestFastPath_CarriesTheOutcomeData(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", `title: one
jobs:
  main:
    steps:
      - id: fix
        message: /gtw fix 1 -y
`)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", optionEscalate, 0.1, 0),
	}}
	svc, lb, sender := testService(t, dir, Config{Decider: decider})
	lb.steps = []fakeStep{{outcome: messages.Outcome{
		Status:  messages.OutcomeIncomplete,
		Summary: "1 of 3 groups unfinished",
		Data:    map[string]string{"issue": "42", "groups": "3"},
	}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, sender, "Which step should run next?")

	svc.mu.Lock()
	r := svc.active["obs-1"]
	svc.mu.Unlock()
	if r == nil {
		t.Fatal("the run is not registered")
	}
	r.mu.Lock()
	steps := append([]wfe.StepState(nil), r.state.Steps...)
	r.mu.Unlock()
	if len(steps) != 1 {
		t.Fatalf("the run recorded %d steps, want 1", len(steps))
	}
	if steps[0].Outcome.Status != wfe.OutcomeIncomplete {
		t.Errorf("the status = %q, want incomplete", steps[0].Outcome.Status)
	}
	if steps[0].Outcome.Summary != "1 of 3 groups unfinished" {
		t.Errorf("the summary = %q", steps[0].Outcome.Summary)
	}
	if steps[0].Outcome.Data["issue"] != "42" {
		t.Errorf("the data = %v, want the issue", steps[0].Outcome.Data)
	}
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")
}

// TestJudgmentPoint_ARepairIsRecordedAgainstItsStep — a command the
// deterministic path would not have run is recorded against the step
// that owns it.
func TestJudgmentPoint_ARepairIsRecordedAgainstItsStep(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "repair", `title: repair
jobs:
  main:
    steps:
      - id: review
        message: /review
        allow: ["/review -c"]
        attempts: 5
      - id: push
        message: /gtw push
`)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", "/review -c", 0.95, 0),
	}}
	svc, lb, sender := testService(t, dir, Config{Decider: decider})
	lb.steps = []fakeStep{
		{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}},
		{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "review finished"}},
		{outcome: messages.Outcome{Status: messages.OutcomeFailed, Summary: "the push failed"}},
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "repair", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, sender, "Which step should run next?")

	svc.mu.Lock()
	r := svc.active["obs-1"]
	svc.mu.Unlock()
	if r == nil {
		t.Fatal("the run is not registered")
	}
	r.mu.Lock()
	history := append([]wfe.ActionRecord(nil), r.policy.History...)
	r.mu.Unlock()

	found := false
	for _, rec := range history {
		if rec.Command != "/review -c" {
			continue
		}
		found = true
		if rec.StepID != "review" {
			t.Errorf("the repair was recorded against %q, want the review step", rec.StepID)
		}
	}
	if !found {
		t.Fatalf("the repair is not in the history: %v", history)
	}
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")
}

// TestPersist_HoldsTheProgressTheConversationAndTheHistory — one file
// holds everything a resumed run needs.
func TestPersist_HoldsTheProgressTheConversationAndTheHistory(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", optionEscalate, 0.1, 0),
	}}
	svc, lb, sender := testService(t, dir, Config{Decider: decider})
	lb.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, sender, "Which step should run next?")
	waitStateFile(t, dir)

	svc.mu.Lock()
	runID := svc.active["obs-1"].id
	svc.mu.Unlock()
	raw, err := os.ReadFile(filepath.Join(dir, "runs", runID+".json"))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	for _, key := range []string{"state", "workflow", "history", "messages"} {
		if _, ok := state[key]; !ok {
			t.Errorf("the state file has no %q key: %s", key, raw)
		}
	}
	run, ok := state["state"].(map[string]any)
	if !ok {
		t.Fatalf("the state file has no run state: %s", raw)
	}
	steps, ok := run["steps"].([]any)
	if !ok || len(steps) == 0 {
		t.Errorf("the state file holds no step progress: %s", raw)
	}
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")
}

// TestSetup_FailureEndsTheRunBeforeAnyStep — a work chat that does
// not come up ends the run before a step executes.
func TestSetup_FailureEndsTheRunBeforeAnyStep(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, lb, _ := testService(t, dir, Config{Provider: &scriptedProvider{}})
	lb.setupOutcome = &messages.Outcome{Status: messages.OutcomeFailed, Summary: "no such directory"}

	_, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	})
	if err == nil {
		t.Fatal("the run started with a broken work chat")
	}
	if !strings.Contains(err.Error(), "/cwd") {
		t.Errorf("error = %q, want it to name the command", err)
	}
	for _, cmd := range lb.Sent() {
		if cmd == "/gtw commit" {
			t.Fatalf("a step ran although the setup failed: %v", lb.Sent())
		}
	}
}

// TestStepKind_ShellRunsInTheWorkspace — a shell step runs its
// command in the workspace of the run.
func TestStepKind_ShellRunsInTheWorkspace(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "sh", `title: sh
jobs:
  main:
    steps:
      - id: build
        shell: make build
        env:
          GOFLAGS: -mod=vendor
`)

	shell := &fakeShell{}
	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	svc, _, _ := testService(t, dir, Config{Provider: provider, Decider: decider, Shell: shell})

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "sh", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	shell.mu.Lock()
	calls := append([]wfe.ShellSpec(nil), shell.calls...)
	shell.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("the shell ran %d commands, want 1", len(calls))
	}
	if calls[0].Command != "make build" {
		t.Errorf("the command = %q, want make build", calls[0].Command)
	}
	if calls[0].Cwd != "/tmp/work" {
		t.Errorf("the cwd = %q, want the workspace of the chat", calls[0].Cwd)
	}
	if calls[0].Env["GOFLAGS"] != "-mod=vendor" {
		t.Errorf("the env = %v, want the step's env", calls[0].Env)
	}
}

// recordingAction records the arguments it ran with.
type recordingAction struct {
	name string
	mu   sync.Mutex
	args map[string]any
	ran  bool
}

func (a *recordingAction) Name() string { return a.name }

func (a *recordingAction) Execute(_ context.Context, args map[string]any, _ map[string]string) (*wfe.ActionResult, error) {
	a.mu.Lock()
	a.args = args
	a.ran = true
	a.mu.Unlock()
	return &wfe.ActionResult{Outputs: map[string]any{"sent": "true"}}, nil
}

// Called reports whether the action ran.
func (a *recordingAction) Called() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ran
}

// TestStop_CancelsARunningShellStep — `/me stop` stops the command a
// shell step is running, and the run ends.
func TestStop_CancelsARunningShellStep(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "sh", `title: sh
jobs:
  main:
    steps:
      - id: build
        shell: make build
`)

	shell := newBlockingShell()
	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	svc, _, _ := testService(t, dir, Config{Provider: provider, Decider: decider, Shell: shell})

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "sh", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-shell.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the shell step never ran")
	}
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if !shell.Cancelled() {
		t.Fatal("the shell command was not stopped with the run")
	}
}

// TestStepKind_UseRunsTheAction — a `use:` step runs the action it
// names, with the arguments it declares.
func TestStepKind_UseRunsTheAction(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "act", `title: act
jobs:
  main:
    steps:
      - id: notify
        use: hello
        with:
          who: world
`)

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	svc, _, _ := testService(t, dir, Config{Provider: provider, Decider: decider})
	action := &recordingAction{name: "hello"}
	svc.actions.Register(action)

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "act", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	action.mu.Lock()
	args := action.args
	action.mu.Unlock()
	if args == nil {
		t.Fatal("the action did not run")
	}
	if args["who"] != "world" {
		t.Errorf("the action ran with %v, want the step's arguments", args)
	}
}

// TestStepKind_AnUnknownActionFailsTheStep — a `use:` step that names
// an action nobody registered fails with the name of the action.
func TestStepKind_AnUnknownActionFailsTheStep(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "act", `title: act
jobs:
  main:
    steps:
      - id: notify
        use: nope
`)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", optionEscalate, 0.1, 0),
	}}
	svc, _, sender := testService(t, dir, Config{Decider: decider})

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "act", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, sender, "Which step should run next?")

	svc.mu.Lock()
	r := svc.active["obs-1"]
	svc.mu.Unlock()
	if r == nil {
		t.Fatal("the run is not registered")
	}
	r.mu.Lock()
	history := append([]wfe.ActionRecord(nil), r.policy.History...)
	r.mu.Unlock()
	if len(history) == 0 {
		t.Fatal("the failed action is not in the history")
	}
	if history[0].Outcome.Status != wfe.OutcomeFailed {
		t.Errorf("the action = %q, want failed", history[0].Outcome.Status)
	}
	if !strings.Contains(history[0].Outcome.Summary, `unknown action: "nope"`) {
		t.Errorf("the summary = %q, want the unknown action", history[0].Outcome.Summary)
	}
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")
}

// ── the judgment point ─────────────────────────────────────────────

func TestJudgmentPoint_ANonOkOutcomeAsksTheAgent(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix-issue", fixIssueWorkflow)

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", "/gtw fix 42 -y", 0.95, 0),
	}}
	svc, lb, _ := testService(t, dir, Config{Provider: provider, Decider: decider})
	// The first step does not finish.
	lb.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix-issue", Args: []string{"42"}, ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if decider.Calls() == 0 {
		t.Fatal("a step that did not end ok must reach a judgment point")
	}
	if provider.Calls() != 0 {
		t.Fatalf("the chat model was called %d times, a confident decision does not need it", provider.Calls())
	}
}

func TestJudgmentPoint_LowConfidenceEscalatesToTheChatModel(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix-issue", fixIssueWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"command": "/gtw fix 42 -y"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "stopped here"})},
	}}
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", "/gtw fix 42 -y", 0.2, 0),
	}}
	svc, lb, _ := testService(t, dir, Config{Provider: provider, Decider: decider})
	lb.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix-issue", Args: []string{"42"}, ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if provider.Calls() == 0 {
		t.Fatal("the chat model was not called, a low-confidence decision must escalate")
	}
	sent := lb.Sent()
	if len(sent) < 3 || sent[2] != "/gtw fix 42 -y" {
		t.Fatalf("the work chat got %v, want the escalated command", sent)
	}
}

func TestJudgmentPoint_ARefusedDecisionEscalatesToTheChatModel(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix-issue", fixIssueWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"command": "/gtw fix 42 -y"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "stopped here"})},
	}}
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		{
			Model: "jev-1.14",
			Answers: map[string]llm.Answer{
				questionNext:       {Type: llm.QuestionChoice, Refused: true},
				questionNeedsHuman: {Type: llm.QuestionYesNo, Refused: true},
			},
		},
	}}
	svc, lb, _ := testService(t, dir, Config{Provider: provider, Decider: decider})
	lb.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix-issue", Args: []string{"42"}, ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if provider.Calls() == 0 {
		t.Fatal("a refused decision must escalate to the chat model")
	}
}

func TestJudgmentPoint_NeedsHumanAsksTheUserWhenThereIsNoChatModel(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix-issue", fixIssueWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", "/gtw fix 42 -y", 0.95, 0.9),
	}}
	svc, lb, sender := testService(t, dir, Config{Decider: decider})
	lb.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix-issue", Args: []string{"42"}, ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, sender, "Which step should run next?")

	if err := svc.Reply(context.Background(), "obs-1", "/gtw fix 42 -y"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	waitSent(t, lb, "/gtw fix 42 -y")
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")
}

func TestJudgmentPoint_PolicyRejectedChoiceEscalates(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix-issue", fixIssueWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"command": "/gtw fix 42 -y"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", "/gtw sync", 0.99, 0),
	}}
	svc, lb, _ := testService(t, dir, Config{Provider: provider, Decider: decider})
	lb.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix-issue", Args: []string{"42"}, ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	sent := lb.Sent()
	for _, cmd := range sent {
		if cmd == "/gtw sync" {
			t.Fatalf("the policy rejected /gtw sync but it ran anyway: %v", sent)
		}
	}
	if provider.Calls() == 0 {
		t.Fatal("a rejected choice must escalate to the chat model")
	}
}

// ── the System 1 decision ──────────────────────────────────────────

// TestSystem1_UnknownOutcomeAsksWhetherTheStepSucceeded — the step
// reported nothing, so the decision model is asked whether it worked.
func TestSystem1_UnknownOutcomeAsksWhetherTheStepSucceeded(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix-issue", fixIssueWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", optionEscalate, 0.1, 0),
	}}
	svc, lb, _ := testService(t, dir, Config{Provider: provider, Decider: decider})
	// The first step reports nothing.
	lb.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeUnknown}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix-issue", Args: []string{"42"}, ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if decider.Calls() == 0 {
		t.Fatal("the decision model was not called")
	}
	questions := decider.Request(0).Questions
	names := map[string]bool{}
	for _, q := range questions {
		names[q.Name] = true
	}
	if !names[questionNext] || !names[questionNeedsHuman] {
		t.Fatalf("the decision request is missing a question: %v", names)
	}
	if !names[questionStepSucceeded] {
		t.Fatalf("the step_succeeded question is missing: %v", names)
	}
}

// TestSystem1_KnownOutcomeDoesNotAskWhetherTheStepSucceeded — the
// step reported a verdict, so nobody has to guess whether it worked.
func TestSystem1_KnownOutcomeDoesNotAskWhetherTheStepSucceeded(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix-issue", fixIssueWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", optionEscalate, 0.1, 0),
	}}
	svc, lb, _ := testService(t, dir, Config{Provider: provider, Decider: decider})
	lb.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix-issue", Args: []string{"42"}, ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if decider.Calls() == 0 {
		t.Fatal("the decision model was not called")
	}
	for _, q := range decider.Request(0).Questions {
		if q.Name == questionStepSucceeded {
			t.Fatal("the step_succeeded question must only be asked when the last outcome is unknown")
		}
	}
}

func TestSystem1_DecisionIsRecordedAsASyntheticToolCall(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix-issue", fixIssueWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", "/gtw fix 42 -y", 0.93, 0),
	}}
	svc, lb, _ := testService(t, dir, Config{Provider: provider, Decider: decider})
	lb.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}}}

	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix-issue", Args: []string{"42"}, ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	svc.mu.Lock()
	r := svc.runs[runID]
	svc.mu.Unlock()
	if r == nil {
		t.Fatal("the run is not registered")
	}
	waitRun(t, svc, "obs-1")

	found := false
	for _, m := range r.conversation() {
		for _, p := range m.Parts {
			if p.Type != llm.PartToolCall {
				continue
			}
			if strings.Contains(p.Text, "decided by jev-1.14, p=0.93") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the decision was not recorded in the conversation")
	}
}

func TestSystem1_ANoopStepIsNotAToolError(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", "/gtw commit", 0.95, 0),
	}}
	svc, lb, _ := testService(t, dir, Config{Provider: provider, Decider: decider})
	// The first attempt does not finish, so the run reaches a
	// judgment point; the command the agent runs then reports a noop.
	lb.steps = []fakeStep{
		{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}},
		{outcome: messages.Outcome{Status: messages.OutcomeNoop, Summary: "nothing to commit"}},
	}

	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	svc.mu.Lock()
	r := svc.runs[runID]
	svc.mu.Unlock()
	waitRun(t, svc, "obs-1")

	if decider.Calls() == 0 {
		t.Fatal("the decision model never took a turn")
	}
	part := toolResultPart(t, r.conversation(), toolRunStep)
	if part.IsError {
		t.Fatalf("a noop outcome came back as a tool error: %s", part.Text)
	}
	if !strings.Contains(part.Text, "noop") {
		t.Fatalf("the tool result does not carry the status: %s", part.Text)
	}
}

func TestSystem1_AFailedStepIsAToolError(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", "/gtw commit", 0.95, 0),
	}}
	svc, lb, _ := testService(t, dir, Config{Provider: provider, Decider: decider})
	lb.steps = []fakeStep{
		{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}},
		{outcome: messages.Outcome{Status: messages.OutcomeFailed, Summary: "the commit failed"}},
	}

	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	svc.mu.Lock()
	r := svc.runs[runID]
	svc.mu.Unlock()
	waitRun(t, svc, "obs-1")

	if provider.Calls() == 0 {
		t.Fatal("the chat model never took a turn")
	}
	part := toolResultPart(t, r.conversation(), toolRunStep)
	if !part.IsError {
		t.Fatalf("a failed outcome came back as a tool result that is not an error: %s", part.Text)
	}
}

// toolResultPart returns the tool result the messages carry for a
// tool.
func toolResultPart(t *testing.T, msgs []llm.Message, tool string) llm.Part {
	t.Helper()
	for _, m := range msgs {
		if m.Role != llm.RoleTool {
			continue
		}
		for _, p := range m.Parts {
			if p.Type == llm.PartToolResult && p.ToolName == tool {
				return p
			}
		}
	}
	t.Fatalf("the messages carry no tool result for %q", tool)
	return llm.Part{}
}

// waitForLine waits until the observer received a line that contains
// the given text.
func waitForLine(t *testing.T, sender *fakeSender, text string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if sender.Has(text) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the observer never received %q: %v", text, sender.Lines())
}
