package me

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/gateway/observe"
	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// ── fakes ─────────────────────────────────────────────────────────

// fakeStep is one scripted answer of the chat.
type fakeStep struct {
	// outcome is the verdict the command reports.
	outcome messages.Outcome
	// transcript is what the coding agent said.
	transcript []observe.Entry
	// choices are the prompts the command is waiting for.
	choices []messages.Choice
	// open keeps the command running: it never settles on its own.
	open bool
}

// fakeExec is one tracked execution of the fake operator.
type fakeExec struct {
	ex       observe.Execution
	watchers []chan observe.Execution
}

// cardRecord is one card the run published or updated.
type cardRecord struct {
	// ID is the id of the message the card became.
	ID string
	// Title is the card's title.
	Title string
	// Body is the card's body.
	Body string
	// Posted reports whether the card was published as a card of its
	// own or updated in place.
	Posted bool
}

// emittedMessage is one outbound message the run published.
type emittedMessage struct {
	// Kind is the kind of the message.
	Kind messages.OutboundKind
	// Text is the body of the message.
	Text string
	// Tool is the tool payload, when the message carries one.
	Tool *messages.ToolInfo
	// ReplyTo names the message the event hangs under.
	ReplyTo string
}

// fakeOperator is a scripted chat.
//
// The commands settle on their own unless a step says otherwise. The
// questions the run asks and the /stop it speaks do not consume a
// scripted step: they are not steps of the workflow.
type fakeOperator struct {
	mu      sync.Mutex
	steps   []fakeStep
	sent    []string
	spoken  []spokenMessage
	asked   []askedMessage
	cards   []cardRecord
	log     []string
	buttons [][]channel.CardButton
	execs   map[string]*fakeExec
	seq     int
	cardSeq int
	watched []string
	// emitted are the outbound messages the run published.
	emitted []emittedMessage
	// failCommand is the command that cannot be sent at all, the way
	// a dead chat cannot take it.
	failCommand string
	// failCard makes the chat refuse the run's overview.
	failCard bool
	// failCardAfter makes the chat refuse the cards after it has
	// published that many of them.
	failCardAfter int
	// refuseAttempt makes the chat refuse that attempt to publish a
	// card. Zero refuses none.
	refuseAttempt int
	// cardAttempts counts the attempts to publish a card.
	cardAttempts int
	// dropOutbound makes the chat hide every outbound message: the
	// run's events do not reach it.
	dropOutbound bool
}

func newFakeOperator(steps ...fakeStep) *fakeOperator {
	return &fakeOperator{steps: steps, execs: map[string]*fakeExec{}}
}

// spokenMessage is one thing the run said in the chat.
type spokenMessage struct {
	// Text is what the daemon was given.
	Text string
	// Display is what the chat shows.
	Display string
	// ReplyTo names the message the text quotes.
	ReplyTo string
}

// Speak says something in the chat.
func (f *fakeOperator) Speak(_ context.Context, chatID, text string, opts channel.SpeakOptions) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, text)
	f.spoken = append(f.spoken, spokenMessage{
		Text:    text,
		Display: opts.Display,
		ReplyTo: opts.ReplyTo,
	})
	f.log = append(f.log, "speak:"+text)
	if f.failCommand != "" && text == f.failCommand {
		return "", errors.New("the chat is gone")
	}
	f.seq++
	id := fmt.Sprintf("msg-%d", f.seq)

	if !isControlText(text) {
		step := fakeStep{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "ok"}}
		if len(f.steps) > 0 {
			step = f.steps[0]
			f.steps = f.steps[1:]
		}
		ex := &fakeExec{ex: observe.Execution{
			MsgID:      id,
			ChatID:     chatID,
			Text:       text,
			Phase:      observe.PhaseSettled,
			Outcome:    step.outcome,
			Transcript: step.transcript,
			Choices:    step.choices,
		}}
		if step.open {
			ex.ex.Phase = observe.PhaseRunning
		}
		f.execs[id] = ex
	}
	return id, nil
}

// askedMessage is one question the run asked in the chat.
type askedMessage struct {
	// Text is what was asked.
	Text string
	// Display is what the chat shows.
	Display string
	// ReplyTo names the message the question quotes.
	ReplyTo string
	// MsgID is the id of the message the question became.
	MsgID string
}

// Ask puts a question in the chat.
//
// The question is not dispatched: it belongs to the run, not to the
// agent.
func (f *fakeOperator) Ask(_ context.Context, _ string, text string, opts channel.SpeakOptions) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := fmt.Sprintf("msg-%d", f.seq)
	f.asked = append(f.asked, askedMessage{
		Text:    text,
		Display: opts.Display,
		ReplyTo: opts.ReplyTo,
		MsgID:   id,
	})
	return id, nil
}

// Asked returns the questions the run asked.
func (f *fakeOperator) Asked() []askedMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]askedMessage(nil), f.asked...)
}

// Act answers a prompt the chat is waiting for.
func (f *fakeOperator) Act(_ context.Context, chatID string, action messages.ActionPayload) error {
	return f.answer(chatID, action.RequestID, action.Option)
}

// PostCard publishes the run's overview.
func (f *fakeOperator) PostCard(_ context.Context, _ string, spec channel.CardSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cardAttempts++
	if f.failCard ||
		f.refuseAttempt == f.cardAttempts ||
		(f.failCardAfter > 0 && f.postedCount() >= f.failCardAfter) {
		return "", errors.New("the chat cannot show a card")
	}
	f.cardSeq++
	id := fmt.Sprintf("card-%d", f.cardSeq)
	f.cards = append(f.cards, cardRecord{ID: id, Title: spec.Title, Body: spec.Body, Posted: true})
	f.buttons = append(f.buttons, spec.Buttons)
	f.log = append(f.log, "card:"+id)
	return id, nil
}

// postedCount returns the number of cards the run published. The
// caller holds the lock.
func (f *fakeOperator) postedCount() int {
	n := 0
	for _, c := range f.cards {
		if c.Posted {
			n++
		}
	}
	return n
}

// Posted returns the number of cards the run published.
func (f *fakeOperator) Posted() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.postedCount()
}

// UpdateCard replaces the run's overview.
func (f *fakeOperator) UpdateCard(_ context.Context, _, cardID string, spec channel.CardSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cards = append(f.cards, cardRecord{ID: cardID, Title: spec.Title, Body: spec.Body})
	f.buttons = append(f.buttons, spec.Buttons)
	return nil
}

// Emit publishes an outbound message in the chat.
func (f *fakeOperator) Emit(_ context.Context, _ string, msg messages.OutboundMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dropOutbound {
		// The chat hides the run's events.
		return nil
	}
	f.emitted = append(f.emitted, emittedMessage{
		Kind:    msg.Kind,
		Text:    msg.Text,
		Tool:    msg.Tool,
		ReplyTo: msg.ReplyTo,
	})
	return nil
}

// Emitted returns the outbound messages the run published, in the
// order it published them.
func (f *fakeOperator) Emitted() []emittedMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]emittedMessage(nil), f.emitted...)
}

// EmittedOf returns the outbound messages of one kind.
func (f *fakeOperator) EmittedOf(kind messages.OutboundKind) []emittedMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]emittedMessage, 0, len(f.emitted))
	for _, m := range f.emitted {
		if m.Kind == kind {
			out = append(out, m)
		}
	}
	return out
}

// Cards returns the cards the run published and updated, in the order
// it did it.
func (f *fakeOperator) Cards() []cardRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cardRecord(nil), f.cards...)
}

// Log returns what the run did in the chat, in the order it did it:
// "card:<id>" for a card it published, "speak:<text>" for a command.
func (f *fakeOperator) Log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

// Buttons returns the buttons of the run's overview.
func (f *fakeOperator) Buttons() []channel.CardButton {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.buttons) == 0 {
		return nil
	}
	return append([]channel.CardButton(nil), f.buttons[len(f.buttons)-1]...)
}

// CardButtons returns the buttons of every card the run published and
// updated, in the order it did it.
func (f *fakeOperator) CardButtons() [][]channel.CardButton {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]channel.CardButton, 0, len(f.buttons))
	for _, b := range f.buttons {
		out = append(out, append([]channel.CardButton(nil), b...))
	}
	return out
}

// Get returns the tracked execution of a message.
func (f *fakeOperator) Get(msgID string) (observe.Execution, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ex, ok := f.execs[msgID]
	if !ok {
		return observe.Execution{}, false
	}
	return ex.ex, true
}

// Watch returns a channel of snapshots of an execution.
func (f *fakeOperator) Watch(msgID string) <-chan observe.Execution {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watched = append(f.watched, msgID)
	ch := make(chan observe.Execution, 4)
	ex, ok := f.execs[msgID]
	if !ok {
		close(ch)
		return ch
	}
	ch <- ex.ex
	if ex.ex.Phase == observe.PhaseSettled {
		close(ch)
		return ch
	}
	ex.watchers = append(ex.watchers, ch)
	return ch
}

// answer settles the open execution of a chat, the way answering a
// prompt lets the coding agent go on.
func (f *fakeOperator) answer(chatID, requestID, optionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ex := range f.execs {
		if ex.ex.ChatID != chatID || ex.ex.Phase == observe.PhaseSettled {
			continue
		}
		ex.ex.Phase = observe.PhaseSettled
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
func (f *fakeOperator) Watched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.watched...)
}

// Sent returns the commands the run sent.
func (f *fakeOperator) Sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

// Spoken returns what the run said in the chat.
func (f *fakeOperator) Spoken() []spokenMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]spokenMessage(nil), f.spoken...)
}

// Lines returns the bodies the run's overview showed.
func (f *fakeOperator) Lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.cards))
	for _, c := range f.cards {
		out = append(out, c.Body)
	}
	return out
}

// Has reports whether the run's overview carried the given text.
func (f *fakeOperator) Has(text string) bool {
	for _, line := range f.Lines() {
		if strings.Contains(line, text) {
			return true
		}
	}
	return false
}

// isControlText reports whether the text is something the run says
// outside a workflow step: a question, the /stop or the pause.
func isControlText(text string) bool {
	return strings.HasPrefix(text, "❓") ||
		strings.HasPrefix(text, "/stop") ||
		strings.HasPrefix(text, pauseMark)
}

// recordingLogger keeps the messages the service logged.
type recordingLogger struct {
	mu    sync.Mutex
	lines []string
}

// Enabled reports that every level is logged.
func (l *recordingLogger) Enabled(context.Context, slog.Level) bool { return true }

// Handle keeps the message of the record.
func (l *recordingLogger) Handle(_ context.Context, rec slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, rec.Message)
	return nil
}

// WithAttrs returns the logger itself: the test reads the messages.
func (l *recordingLogger) WithAttrs([]slog.Attr) slog.Handler { return l }

// WithGroup returns the logger itself: the test reads the messages.
func (l *recordingLogger) WithGroup(string) slog.Handler { return l }

// Lines returns the messages that were logged.
func (l *recordingLogger) Lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// logged reports whether the service logged the given text.
func logged(l *recordingLogger, text string) bool {
	for _, line := range l.Lines() {
		if strings.Contains(line, text) {
			return true
		}
	}
	return false
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
func testService(t *testing.T, dir string, cfg Config) (*Service, *fakeOperator) {
	t.Helper()
	op := newFakeOperator()
	cfg.WorkflowsDir = dir
	cfg.StateDir = filepath.Join(dir, "runs")
	cfg.Operator = op
	if cfg.Sleep == nil {
		// The backoff between two attempts of the chat model is real
		// time; a test does not wait for it.
		cfg.Sleep = func(context.Context, time.Duration) error { return nil }
	}
	svc := New(cfg)
	t.Cleanup(func() { _ = svc.Shutdown(context.Background()) })
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	return svc, op
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
//
// A run that is over has left the chat, and its goroutine may still
// be unwinding — posting the summary of the session, closing the
// session that thinks for the owner. The wait is for the goroutine,
// not for the chat.
func waitRun(t *testing.T, svc *Service, chatID string) {
	t.Helper()
	svc.mu.Lock()
	r := svc.active[chatID]
	svc.mu.Unlock()
	if r != nil {
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
			t.Fatal("the run did not finish")
		}
		return
	}
	done := make(chan struct{})
	go func() {
		svc.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not finish")
	}
}

// waitSent waits until the chat received a command.
func waitSent(t *testing.T, op *fakeOperator, command string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, cmd := range op.Sent() {
			if cmd == command {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the chat never received %q: %v", command, op.Sent())
}

// stepOutcome returns the outcome the run state holds for a step.
func stepOutcome(r *run, stepID string) (wfe.Outcome, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, st := range r.state.Steps {
		if st.ID == stepID {
			return st.Outcome, true
		}
	}
	return wfe.Outcome{}, false
}

// runHistory returns the actions the run took.
func runHistory(r *run) []wfe.ActionRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]wfe.ActionRecord(nil), r.policy.History...)
}

// activeRun returns the run of a chat.
func activeRun(t *testing.T, svc *Service, chatID string) *run {
	t.Helper()
	svc.mu.Lock()
	r := svc.active[chatID]
	svc.mu.Unlock()
	if r == nil {
		t.Fatal("the run is not registered")
	}
	return r
}

// ── starting a run ────────────────────────────────────────────────

func TestStartWorkflow_RefusesASecondRunInTheSameChat(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	provider := &scriptedProvider{err: errors.New("the chat model must not be called")}
	decider := &scriptedDecider{err: errors.New("the decision model must not be called")}
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})
	// The run keeps working: a run that is over would free the chat
	// for a second one, and the test would prove nothing.
	op.steps = []fakeStep{{open: true}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err == nil {
		t.Fatal("a second run in the same chat was accepted")
	}
}

func TestStartWorkflow_ReportsAnUnknownWorkflow(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, _ := testService(t, dir, Config{Provider: &scriptedProvider{}})
	_, err := svc.StartWorkflow(context.Background(), Request{
		Name: "nope", ChatID: "obs-1", Cwd: "/tmp/work",
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

	svc, _ := testService(t, dir, Config{Provider: &scriptedProvider{}})
	_, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", Args: []string{"42", "43"}, ChatID: "obs-1", Cwd: "/tmp/work",
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
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", Args: []string{"issue=42"}, ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	waitSent(t, op, "/gtw fix 42 -y")
}

func TestStartWorkflow_RefusesAnUnknownNamedArgument(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, _ := testService(t, dir, Config{Provider: &scriptedProvider{}})
	_, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", Args: []string{"nope=1"}, ChatID: "obs-1", Cwd: "/tmp/work",
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

	svc, _ := testService(t, dir, Config{})
	_, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
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
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name:   "fix-issue",
		Args:   []string{"42"},
		ChatID: "obs-1",
		Cwd:    "/tmp/work",
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
	sent := op.Sent()
	want := []string{"/gtw fix 42 -y", "/gtw commit", "/review", "/gtw push", "/gtw pr"}
	if len(sent) != len(want) {
		t.Fatalf("the chat got %v, want %v", sent, want)
	}
	for i, cmd := range want {
		if sent[i] != cmd {
			t.Fatalf("command %d was %q, want %q", i, sent[i], cmd)
		}
	}
	if !op.Has("system-1: 0 decisions") {
		t.Fatalf("the summary does not count the decisions: %v", op.Lines())
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
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{
		{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "fixed"}},
		{outcome: messages.Outcome{Status: messages.OutcomeNoop, Summary: "nothing to commit"}},
		{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}},
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, op, "Which step should run next?")

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
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{{outcome: messages.Outcome{
		Status:  messages.OutcomeIncomplete,
		Summary: "1 of 3 groups unfinished",
		Data:    map[string]string{"issue": "42", "groups": "3"},
	}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, op, "Which step should run next?")

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
// the run is on.
func TestJudgmentPoint_ARepairIsRecordedAgainstItsStep(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "repair", `title: repair
jobs:
  main:
    steps:
      - id: review
        message: /review
      - id: push
        message: /gtw push
`)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", "ntm_review", map[string]any{"args": "-c"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "the review did not pass"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{
		{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}},
		{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "review finished"}},
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "repair", ChatID: "obs-1", Cwd: "/tmp/work",
		Commands: []CommandInfo{{Name: "review"}},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	r := activeRun(t, svc, "obs-1")
	waitRun(t, svc, "obs-1")

	history := runHistory(r)
	found := false
	for _, rec := range history {
		if rec.Command != "/review -c" {
			continue
		}
		found = true
		if rec.Kind != wfe.ActionKindCommand {
			t.Errorf("the repair was recorded as %q, want it recorded as a command", rec.Kind)
		}
		if rec.StepID != "review" {
			t.Errorf("the repair was recorded against %q, want the review step", rec.StepID)
		}
	}
	if !found {
		t.Fatalf("the repair is not in the history: %v", history)
	}
}

// TestJudgmentPoint_ARepairDoesNotReplaceTheVerdict — a repair is
// recorded against the step the run is on, but the step's verdict is
// the one its own execution wrote.
func TestJudgmentPoint_ARepairDoesNotReplaceTheVerdict(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "repair", `title: repair
jobs:
  main:
    steps:
      - id: review
        message: /review
      - id: push
        message: /gtw push
`)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", "ntm_review", map[string]any{"args": "-c"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "the review did not pass"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{
		{outcome: messages.Outcome{Status: messages.OutcomeFailed, Summary: "the review failed"}},
		{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "the repair worked"}},
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "repair", ChatID: "obs-1", Cwd: "/tmp/work",
		Commands: []CommandInfo{{Name: "review"}},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	r := activeRun(t, svc, "obs-1")
	waitRun(t, svc, "obs-1")

	out, ok := stepOutcome(r, "review")
	if !ok {
		t.Fatal("the review step has no outcome")
	}
	if out.Status != wfe.OutcomeFailed {
		t.Errorf("the review step ended as %q, want the failed one the step produced", out.Status)
	}
}

// TestJudgmentPoint_AStepRunsAgainReplacesTheVerdict — the step's own
// execution writes the verdict, and a later run of it replaces the
// verdict it had.
func TestJudgmentPoint_AStepRunsAgainReplacesTheVerdict(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "repair", `title: repair
jobs:
  main:
    steps:
      - id: review
        message: /review
      - id: push
        message: /gtw push
`)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", "ntm_review", map[string]any{"args": "-c"})},
		{Message: toolCallMsg("c2", toolRunStep, map[string]any{"step_id": "review"})},
	}}
	svc, op := testService(t, dir, Config{Provider: provider})
	op.steps = []fakeStep{
		{outcome: messages.Outcome{Status: messages.OutcomeFailed, Summary: "the review failed"}},
		{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "the repair worked"}},
		{outcome: messages.Outcome{Status: messages.OutcomeOK, Summary: "the review worked"}},
	}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "repair", ChatID: "obs-1", Cwd: "/tmp/work",
		Commands: []CommandInfo{{Name: "review"}},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	r := activeRun(t, svc, "obs-1")
	waitRun(t, svc, "obs-1")

	out, ok := stepOutcome(r, "review")
	if !ok {
		t.Fatal("the review step has no outcome")
	}
	if out.Status != wfe.OutcomeOK {
		t.Errorf("the review step ended as %q, want the ok one its own run wrote", out.Status)
	}
	found := false
	for _, rec := range runHistory(r) {
		if rec.Command != "/review" {
			continue
		}
		found = true
		if rec.Kind != wfe.ActionKindRun {
			t.Errorf("the step's own run was recorded as %q, want it recorded as a run", rec.Kind)
		}
	}
	if !found {
		t.Fatal("the step's own run is not in the history")
	}
}

// TestPersist_HoldsTheProgressTheConversationAndTheHistory — one file
// holds everything a resumed run needs.
func TestPersist_HoldsTheProgressTheConversationAndTheHistory(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", optionEscalate, 0.1, 0),
	}}
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, op, "Which step should run next?")
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

// TestSetup_FailureEndsTheRunBeforeAnyStep — a chat that cannot
// take the run's overview ends the run before a step executes.
func TestSetup_FailureEndsTheRunBeforeAnyStep(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "one", oneStepWorkflow)

	svc, op := testService(t, dir, Config{Provider: &scriptedProvider{}})
	op.failCard = true

	_, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
	})
	if err == nil {
		t.Fatal("the run started with a chat that cannot show its overview")
	}
	for _, cmd := range op.Sent() {
		if cmd == "/gtw commit" {
			t.Fatalf("a step ran although the run could not start: %v", op.Sent())
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
	svc, _ := testService(t, dir, Config{Provider: provider, Decider: decider, Shell: shell})

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "sh", ChatID: "obs-1", Cwd: "/tmp/work",
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
	svc, _ := testService(t, dir, Config{Provider: provider, Decider: decider, Shell: shell})

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "sh", ChatID: "obs-1", Cwd: "/tmp/work",
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
	svc, _ := testService(t, dir, Config{Provider: provider, Decider: decider})
	action := &recordingAction{name: "hello"}
	svc.actions.Register(action)

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "act", ChatID: "obs-1", Cwd: "/tmp/work",
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
	svc, op := testService(t, dir, Config{Decider: decider})

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "act", ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, op, "Which step should run next?")

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
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})
	// The first step does not finish.
	op.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix-issue", Args: []string{"42"}, ChatID: "obs-1", Cwd: "/tmp/work",
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
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"step_id": "fix"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "stopped here"})},
	}}
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", "/gtw fix 42 -y", 0.2, 0),
	}}
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})
	op.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix-issue", Args: []string{"42"}, ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	if provider.Calls() == 0 {
		t.Fatal("the chat model was not called, a low-confidence decision must escalate")
	}
	sent := op.Sent()
	if len(sent) < 2 || sent[1] != "/gtw fix 42 -y" {
		t.Fatalf("the chat got %v, want the escalated command", sent)
	}
}

func TestJudgmentPoint_ARefusedDecisionEscalatesToTheChatModel(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix-issue", fixIssueWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"step_id": "fix"})},
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
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})
	op.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix-issue", Args: []string{"42"}, ChatID: "obs-1", Cwd: "/tmp/work",
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
	svc, op := testService(t, dir, Config{Decider: decider})
	op.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix-issue", Args: []string{"42"}, ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLine(t, op, "Which step should run next?")

	if err := svc.Reply(context.Background(), "obs-1", "/gtw fix 42 -y"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	waitSent(t, op, "/gtw fix 42 -y")
	if err := svc.Stop(context.Background(), "obs-1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitRun(t, svc, "obs-1")
}

func TestJudgmentPoint_PolicyRejectedChoiceEscalates(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix-issue", fixIssueWorkflow)

	provider := &scriptedProvider{responses: []llm.Response{
		{Message: toolCallMsg("c1", toolRunStep, map[string]any{"step_id": "fix"})},
		{Message: toolCallMsg("c2", toolFinish, map[string]any{"status": "failed", "summary": "stopped"})},
	}}
	decider := &scriptedDecider{answers: []llm.DecisionResponse{
		decisionResp("jev-1.14", "/gtw sync", 0.99, 0),
	}}
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})
	op.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix-issue", Args: []string{"42"}, ChatID: "obs-1", Cwd: "/tmp/work",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRun(t, svc, "obs-1")

	sent := op.Sent()
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
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})
	// The first step reports nothing.
	op.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeUnknown}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix-issue", Args: []string{"42"}, ChatID: "obs-1", Cwd: "/tmp/work",
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
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})
	op.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}}}

	if _, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix-issue", Args: []string{"42"}, ChatID: "obs-1", Cwd: "/tmp/work",
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
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})
	op.steps = []fakeStep{{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}}}

	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "fix-issue", Args: []string{"42"}, ChatID: "obs-1", Cwd: "/tmp/work",
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
			if p.Type != llm.PartToolCall || p.ToolName != toolRunStep {
				continue
			}
			if !strings.Contains(string(p.Args), `"step_id":"fix"`) {
				continue
			}
			found = true
			if strings.Contains(p.Text, "decided by") || strings.Contains(p.Text, "p=") {
				t.Errorf("the synthetic call carries the decision model: %q", p.Text)
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
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})
	// The first attempt does not finish, so the run reaches a
	// judgment point; the command the agent runs then reports a noop.
	op.steps = []fakeStep{
		{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}},
		{outcome: messages.Outcome{Status: messages.OutcomeNoop, Summary: "nothing to commit"}},
	}

	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
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
	svc, op := testService(t, dir, Config{Provider: provider, Decider: decider})
	op.steps = []fakeStep{
		{outcome: messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "still going"}},
		{outcome: messages.Outcome{Status: messages.OutcomeFailed, Summary: "the commit failed"}},
	}

	runID, err := svc.StartWorkflow(context.Background(), Request{
		Name: "one", ChatID: "obs-1", Cwd: "/tmp/work",
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

// waitForLine waits until the run's overview carried the given text.
func waitForLine(t *testing.T, op *fakeOperator, text string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if op.Has(text) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the run's overview never carried %q: %v", text, op.Lines())
}
