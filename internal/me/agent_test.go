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
// consume a scripted step, so a test only scripts the playbook's own
// commands.
type fakeLoopback struct {
	mu      sync.Mutex
	steps   []fakeStep
	sent    []string
	execs   map[string]*fakeExec
	seq     int
	watched []string
}

func newFakeLoopback(steps ...fakeStep) *fakeLoopback {
	return &fakeLoopback{steps: steps, execs: map[string]*fakeExec{}}
}

func (f *fakeLoopback) Send(_ context.Context, chatID, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, text)
	f.seq++
	id := fmt.Sprintf("msg-%d", f.seq)

	step := fakeStep{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "ok"}}
	if !isSetupCommand(text) {
		if len(f.steps) > 0 {
			step = f.steps[0]
			f.steps = f.steps[1:]
		}
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

// fixIssuePlaybook is the playbook of the integration test.
const fixIssuePlaybook = `name: fix-issue
args: [id]
goal: Fix issue #${{ args.id }} and ship it as a PR.
steps:
  - /gtw fix ${{ args.id }} -y
  - /gtw commit
  - run: /review
    note: If it fails or does not finish, continue with /review -c.
    allow: [/review -c, /gtw commit]
    max: 5
  - /gtw push
  - /gtw pr
`

// writePlaybook writes a playbook into a workflows directory.
func writePlaybook(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write playbook: %v", err)
	}
}

// testService builds a personal agent with fakes.
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

// ── the System 1 fast path ────────────────────────────────────────

func TestStartPlaybook_RefusesASecondRunInTheSameChat(t *testing.T) {
	dir := t.TempDir()
	writePlaybook(t, dir, "one", oneStepPlaybook)

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	svc, _, _ := testService(t, dir, Config{Provider: provider, Decider: decider})

	if _, err := svc.StartPlaybook(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := svc.StartPlaybook(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err == nil {
		t.Fatal("a second run in the same chat was accepted")
	}
}

func TestStartPlaybook_ReportsAnUnknownPlaybook(t *testing.T) {
	dir := t.TempDir()
	writePlaybook(t, dir, "one", oneStepPlaybook)

	svc, _, _ := testService(t, dir, Config{Provider: &scriptedProvider{}})
	_, err := svc.StartPlaybook(context.Background(), Request{
		Name: "nope", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	})
	if err == nil {
		t.Fatal("an unknown playbook was accepted")
	}
	if !strings.Contains(err.Error(), "no playbook named") {
		t.Fatalf("error = %q, want the unknown playbook", err)
	}
}

func TestStartPlaybook_ReportsThatTheAgentIsOff(t *testing.T) {
	dir := t.TempDir()
	writePlaybook(t, dir, "one", oneStepPlaybook)

	svc, _, _ := testService(t, dir, Config{})
	_, err := svc.StartPlaybook(context.Background(), Request{
		Name: "one", ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	})
	if err == nil {
		t.Fatal("the run started without a model")
	}
	if !strings.Contains(err.Error(), "me.llm") {
		t.Fatalf("error = %q, want the reason", err)
	}
}

func TestSystem1_ConfidentChoiceRunsWithoutTheChatModel(t *testing.T) {
	dir := t.TempDir()
	writePlaybook(t, dir, "fix-issue", fixIssuePlaybook)

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", "/gtw fix 42 -y", 0.95, 0),
		decisionResp("jev-1.14", "/gtw commit", 0.93, 0),
		decisionResp("jev-1.14", "/review", 0.9, 0),
		decisionResp("jev-1.14", "/gtw push", 0.9, 0),
		decisionResp("jev-1.14", "/gtw pr", 0.9, 0),
		decisionResp("jev-1.14", optionDone, 0.99, 0),
	}}
	svc, lb, sender := testService(t, dir, Config{Provider: provider, Decider: decider})

	runID, err := svc.StartPlaybook(context.Background(), Request{
		Name:           "fix-issue",
		Args:           []string{"42"},
		ObserverChatID: "obs-1",
		Cwd:            "/tmp/work",
		Agent:          "claude",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if provider.Calls() != 0 {
		t.Fatalf("the chat model was called %d times, the fast path should not need it", provider.Calls())
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
	if !sender.Has("system-1: 6 decisions") {
		t.Fatalf("the summary does not count the decisions: %v", sender.Lines())
	}
	if runID == "" {
		t.Fatal("the run has no id")
	}
}

func TestSystem1_LowConfidenceEscalatesToTheChatModel(t *testing.T) {
	dir := t.TempDir()
	writePlaybook(t, dir, "fix-issue", fixIssuePlaybook)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"command": "/gtw fix 42 -y"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "stopped here"})},
	}}
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", "/gtw fix 42 -y", 0.2, 0),
	}}
	svc, lb, _ := testService(t, dir, Config{Provider: provider, Decider: decider})

	if _, err := svc.StartPlaybook(context.Background(), Request{
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

func TestSystem1_NeedsHumanAsksTheUserWhenThereIsNoChatModel(t *testing.T) {
	dir := t.TempDir()
	writePlaybook(t, dir, "fix-issue", fixIssuePlaybook)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", "/gtw fix 42 -y", 0.95, 0.9),
	}}
	svc, lb, sender := testService(t, dir, Config{Decider: decider})

	if _, err := svc.StartPlaybook(context.Background(), Request{
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

func TestSystem1_PolicyRejectedChoiceEscalates(t *testing.T) {
	dir := t.TempDir()
	writePlaybook(t, dir, "fix-issue", fixIssuePlaybook)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"command": "/gtw fix 42 -y"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", "/gtw sync", 0.99, 0),
	}}
	svc, lb, _ := testService(t, dir, Config{Provider: provider, Decider: decider})

	if _, err := svc.StartPlaybook(context.Background(), Request{
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

func TestSystem1_UnknownOutcomeAsksWhetherTheStepSucceeded(t *testing.T) {
	dir := t.TempDir()
	writePlaybook(t, dir, "fix-issue", fixIssuePlaybook)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"command": "/gtw fix 42 -y"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", optionEscalate, 0.1, 0),
	}}
	svc, lb, _ := testService(t, dir, Config{Provider: provider, Decider: decider})
	// The first step reports nothing.
	lb.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeUnknown}}}

	if _, err := svc.StartPlaybook(context.Background(), Request{
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
	if names[questionStepSucceeded] {
		t.Fatal("the step_succeeded question must only be asked when the last outcome is unknown")
	}
}

func TestSystem1_UnknownOutcomeAddsTheStepSucceededQuestion(t *testing.T) {
	dir := t.TempDir()
	writePlaybook(t, dir, "fix-issue", fixIssuePlaybook)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"command": "/gtw fix 42 -y"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", optionEscalate, 0.1, 0),
	}}
	svc, lb, _ := testService(t, dir, Config{Provider: provider, Decider: decider})
	lb.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeUnknown}}}

	if _, err := svc.StartPlaybook(context.Background(), Request{
		Name: "fix-issue", Args: []string{"42"}, ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	// The second decision request follows the first step, whose
	// outcome was unknown.
	if decider.Calls() < 2 {
		t.Fatalf("the decision model was called %d times, want at least 2", decider.Calls())
	}
	questions := decider.Request(decider.Calls() - 1).Questions
	found := false
	for _, q := range questions {
		if q.Name == questionStepSucceeded {
			found = true
		}
	}
	if !found {
		t.Fatalf("the step_succeeded question is missing: %v", questions)
	}
}

func TestSystem1_DecisionIsRecordedAsASyntheticToolCall(t *testing.T) {
	dir := t.TempDir()
	writePlaybook(t, dir, "fix-issue", fixIssuePlaybook)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", "/gtw fix 42 -y", 0.93, 0),
	}}
	svc, _, _ := testService(t, dir, Config{Provider: provider, Decider: decider})

	if _, err := svc.StartPlaybook(context.Background(), Request{
		Name: "fix-issue", Args: []string{"42"}, ObserverChatID: "obs-1", Cwd: "/tmp/work", Agent: "claude",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	svc.mu.Lock()
	r := svc.runs[firstRunID(svc)]
	svc.mu.Unlock()
	if r == nil {
		t.Fatal("the run is gone")
	}
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

// firstRunID returns the id of the first run of a service.
func firstRunID(svc *Service) string {
	for id := range svc.runs {
		return id
	}
	return ""
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
