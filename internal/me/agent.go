// Package me — NightMe's personal agent.
//
// A person drives nightme by typing commands into a chat and reading
// what comes back. `me` does the same thing without the typing: it
// runs a workflow — a YAML file with jobs and steps — and decides
// what to run next.
//
// A run has two layers:
//
//   - The fast path. The workflow says what comes next, so the step
//     runs as it is. No model is asked.
//   - The judgment point. Where the workflow cannot decide — the
//     previous step did not end ok, the plan forks, the plan is
//     exhausted — the agent decides: it picks the next node, repairs
//     the current one, or ends the run.
//
// The agent sits in the chat the run started in. What it says is a
// real message of that chat, posted on the platform like any other
// message, so the receipt, the reaction and the interactive prompts
// anchor to it the way they anchor to a person's message.
//
// The run has no clock of its own. Every command it issues carries
// its own budget, and the run acts on the outcome the agent reports:
// a timeout is the agent's timeout, never the run's. The run watches.
//
// The split:
//
//   - internal/channel — transport. How a message reaches the chat
//     and how a click comes back.
//   - internal/gateway/observe — the side channel that reports what
//     the daemon said about the messages the run sent.
//   - pkg/wfe — the workflow schema, the engine that computes
//     the plan of a run, and the policy that bounds what a run may
//     do.
//   - internal/llm — the two model capabilities: decisions and chat.
//   - internal/me — the run itself.
package me

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/gateway/observe"
	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// Defaults of the personal agent.
const (
	// DefaultMinConfidence is the confidence a System 1 decision
	// must reach before it is acted on without the chat model.
	DefaultMinConfidence = 0.8

	// DefaultMaxJudgments is the number of chances the agent has at
	// one step in one round.
	//
	// A chance is the step's own execution, a nightme command issued
	// for it, or a turn the agent produced no tool call in. One
	// number bounds both what the agent spends and what it causes.
	DefaultMaxJudgments = 50

	// The run has no budget of its own.
	//
	// Every command the run issues carries one: the agent behind it
	// reports its own outcome when its own budget runs out. The run
	// watches that outcome and acts on it — a timeout is the agent's
	// timeout, never the run's.
)

// BotMark is what the run's messages look like in the chat.
//
// The agent speaks with the bot's voice: a person reading the chat
// sees at once that the message came from the run and not from
// somebody else.
const BotMark = "🧑‍💻: "

// Phase is where a run stands.
type Phase string

const (
	// PhaseIdle means the chat has no run.
	PhaseIdle Phase = "idle"
	// PhaseRunning means the run is working.
	PhaseRunning Phase = "running"
	// PhasePaused means the run stopped on purpose: the operator
	// asked for a pause, or a person took the chat back.
	PhasePaused Phase = "paused"
	// PhaseAsking means the run is waiting for the operator to
	// answer a question.
	PhaseAsking Phase = "asking"
	// PhaseDone means the run finished its workflow.
	PhaseDone Phase = "done"
	// PhaseFailed means the run could not go on.
	PhaseFailed Phase = "failed"
	// PhaseStopped means the operator stopped the run.
	PhaseStopped Phase = "stopped"
)

// CommandInfo describes one nightme command the run may issue.
//
// It is the command's own description, carried over from the command
// registry: the run's owner sees the same summary in /help that the
// model reads.
type CommandInfo struct {
	// Name is the command's name without the leading slash.
	Name string
	// Summary is the one-line description of the command.
	Summary string
	// Usage is the usage hint of the command.
	Usage string
	// Subcommands are the command's subcommands.
	Subcommands []CommandInfo
}

// Request is a request to run a workflow.
type Request struct {
	// Name is the workflow's name.
	Name string
	// Args are the arguments of the command line. A token of the
	// form name=value assigns that argument by name; every other
	// token fills the next declared argument, in order.
	Args []string
	// Iterations overrides the round count of every job of the
	// workflow. Nil means the workflow file's own counts stand.
	Iterations *int
	// ChatID is the chat the run works in. The run speaks there and
	// takes its answers from there.
	ChatID string
	// Cwd is the working directory the run starts in. The run follows
	// the chat's directory from there.
	Cwd string
	// Owner is the user the run speaks for.
	Owner string
	// Commands are the nightme commands the run may issue outside the
	// workflow's steps.
	Commands []CommandInfo
	// Agent is the name of the chat's agent. It is the one that
	// thinks for the owner when the daemon names none itself.
	Agent string
	// MessageID is the id of the message that started the run. The
	// run's card hangs under it.
	MessageID string
}

// Status is the state of a run.
type Status struct {
	// RunID is the run's id.
	RunID string
	// Phase is where the run stands.
	Phase Phase
	// Step is the command the run is on.
	Step string
	// Execution is the phase of the running command.
	Execution observe.Phase
	// Progress is the heartbeat view of the running command.
	Progress observe.Progress
	// Actions is the number of actions the run took.
	Actions int
	// Remaining is the budget left in the policy.
	Remaining wfe.Remaining
	// System1Decisions is the number of decisions the decision model
	// made.
	System1Decisions int
	// System2Decisions is the number of turns the chat model took.
	System2Decisions int
	// System1Usage is the token usage of the decision model.
	System1Usage llm.Usage
	// System2Usage is the token usage of the chat model.
	System2Usage llm.Usage
	// Question is the question the run is waiting for.
	Question string
	// Paused reports whether the run is waiting for the operator.
	Paused bool
	// PauseReason says why the run paused.
	PauseReason string
	// Error is why the run cannot go on, when it cannot.
	Error string
}

// Operator drives the chat a run works in.
//
// It is the slice of the channel the personal agent needs: say
// something in the chat, answer a prompt the chat is waiting for,
// publish the run's overview, and watch how a message went.
type Operator interface {
	// Speak says something in the chat. The text is what the daemon
	// dispatches; the display is what the chat shows. It returns the
	// id of the message it became.
	Speak(ctx context.Context, chatID, text string, opts channel.SpeakOptions) (string, error)
	// Ask puts a question in the chat and returns the id of the
	// message it became. The question is visible and the operator
	// can answer it, but the daemon does not take it as a turn of
	// its own.
	Ask(ctx context.Context, chatID, text string, opts channel.SpeakOptions) (string, error)
	// Act answers a prompt the chat is waiting for, as if a person
	// had clicked the button.
	Act(ctx context.Context, chatID string, action messages.ActionPayload) error
	// PostCard publishes the card of a session and returns the id of
	// the message it became. The card is the session's own: the chat
	// patches it in place when the session's events land.
	PostCard(ctx context.Context, chatID string, spec channel.CardSpec) (string, error)
	// AdoptCard tells the chat that the message with the given id is
	// the card of a session: the events that carry it as their anchor
	// are rendered onto that card instead of onto a new one.
	AdoptCard(chatID, cardID string)
	// Emit publishes an outbound message in the chat the run works in.
	// The kind decides how the chat renders it: thinking becomes a
	// 💭 card, a tool call a ● line, a reply a message.
	Emit(ctx context.Context, chatID string, msg messages.OutboundMessage) error
	// OnChoice installs the handler that takes the answer of an
	// interactive choice back into the run. The handler is called with
	// the id of the request the choice was asked under and the option
	// that was picked; it reports whether it took the answer. The
	// returned function removes the handler again.
	OnChoice(chatID string, handler func(requestID, option string) bool) (remove func())
	// Get returns the tracked execution of a message, without
	// consuming it.
	Get(msgID string) (observe.Execution, bool)
	// Watch returns a channel of snapshots of an execution.
	Watch(msgID string) <-chan observe.Execution
}

// GitRunner runs git in a workspace.
type GitRunner interface {
	// Run runs git with the given arguments in dir.
	Run(ctx context.Context, dir string, args ...string) (stdout, stderr string, err error)
}

// CwdProvider reports the directory a chat currently works in.
//
// The chat's directory moves when a command moves it — `/gtw fix`
// moves it into the worktree — so a shell step runs where the chat
// stands.
type CwdProvider interface {
	// Cwd returns the directory the chat currently works in.
	Cwd(chatID string) string
}

// AgentOpener opens the session that thinks for the owner.
//
// The package does not know the agent layer: the daemon injects the
// opener, and the run only ever asks a session a question.
type AgentOpener interface {
	// Open starts a session of the named agent in the workspace. The
	// context is the session's scope: when it ends, the session ends
	// with it.
	Open(ctx context.Context, agent, workspace string) (AgentSession, error)
}

// AgentSession is the thinking of the owner: it investigates a
// question and answers it, and it changes nothing.
type AgentSession interface {
	// Ask asks the session one question and waits for its answer.
	Ask(ctx context.Context, prompt string) (AgentReply, error)
	// Close ends the session.
	Close() error
}

// AgentReply is what a session answered.
type AgentReply struct {
	// Text is the answer.
	Text string
	// Model is the model that produced it, when the session reports
	// one.
	Model string
}

// Config holds the personal agent's configuration.
type Config struct {
	// WorkflowsDir is the directory the workflows live in. The action
	// scripts live in <WorkflowsDir>/actions and the runs in
	// <WorkflowsDir>/runs.
	WorkflowsDir string
	// StateDir is where the runs are persisted. Empty means the
	// runs are not persisted at all.
	StateDir string

	// Provider is the chat model (System 2). Nil turns System 2
	// off.
	Provider llm.Provider
	// Decider is the decision model (System 1). Nil turns System 1
	// off.
	Decider llm.Decider

	// MinConfidence is the confidence a System 1 decision must reach
	// before it is acted on. Zero means DefaultMinConfidence.
	MinConfidence float64
	// MaxJudgments is the number of chances the agent has at one step
	// in one round. Zero means DefaultMaxJudgments.
	MaxJudgments int
	// Sleep waits for a duration.
	//
	// The backoff between two attempts of the chat model uses it: the
	// run waits before it asks again. Nil means the real clock.
	Sleep func(ctx context.Context, d time.Duration) error
	// Agent is the name of the agent that thinks for the owner. It
	// wins over the agent of the chat that started the run. Empty
	// means the chat's agent is used, and a run without one has no
	// thinking to ask.
	Agent string
	// AgentOpener opens the session that thinks for the owner. Nil
	// means the run cannot ask anyone: it has no agent tool.
	AgentOpener AgentOpener
	// Operator drives the chat the run works in. Nil means the
	// personal agent cannot run.
	Operator Operator
	// Shell runs the shell commands of the workflow's steps. Nil
	// turns the shell step kind off.
	Shell ShellRunner
	// Git reads the repository facts of the chat.
	Git GitRunner
	// Cwd reports the directory a chat currently works in. Nil means
	// the shell steps run in the workspace the run started in.
	Cwd CwdProvider

	// Logger is the logger the agent logs through. Nil means
	// slog.Default().
	Logger *slog.Logger
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Service is the personal agent.
//
// It owns the workflow catalogue, the action registry and the runs.
// One run is active per chat: the chat that started a run is the
// chat the run works in and the chat that gets its overview.
type Service struct {
	cfg    Config
	logger *slog.Logger

	// catalog is the daemon's view of the workflows directory. It is
	// read when a workflow is asked for, so an edited file takes
	// effect without a restart.
	catalog *Catalog

	mu sync.Mutex
	// runs holds every run by id.
	runs map[string]*run
	// active maps a chat id to the run it started.
	active map[string]*run
	// started reports whether the service is up.
	started bool

	// actions holds the actions a `use:` step can name.
	actions *ActionRegistry

	// wg tracks the run goroutines, so a shutdown can wait for them.
	wg sync.WaitGroup
}

// New constructs a Service. The workflows are read at Start, not here.
func New(cfg Config) *Service {
	if cfg.MinConfidence <= 0 {
		cfg.MinConfidence = DefaultMinConfidence
	}
	if cfg.MaxJudgments <= 0 {
		cfg.MaxJudgments = DefaultMaxJudgments
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepContext
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		cfg:     cfg,
		logger:  logger,
		catalog: NewCatalog(cfg.WorkflowsDir),
		runs:    map[string]*run{},
		active:  map[string]*run{},
		actions: NewActionRegistry(),
	}
}

// sleepContext waits for a duration, unless the context ends first.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SetLogger installs the logger the service logs through.
func (s *Service) SetLogger(l *slog.Logger) {
	if l == nil {
		return
	}
	s.logger = l
}

// DisabledReason explains why the personal agent cannot run, or
// returns an empty string when it can.
func (s *Service) DisabledReason() string {
	if reason := s.modelsReason(); reason != "" {
		return reason
	}
	if reason := s.operatorReason(); reason != "" {
		return reason
	}
	return s.workflowsReason()
}

// modelsReason reports why the agent has nothing to decide with.
func (s *Service) modelsReason() string {
	if s.cfg.Provider == nil && s.cfg.Decider == nil {
		return "neither me.llm nor me.decider is configured"
	}
	return ""
}

// operatorReason reports why the agent cannot drive a chat.
func (s *Service) operatorReason() string {
	if s.cfg.Operator == nil {
		return "there is no channel the run can work in"
	}
	return ""
}

// workflowsReason reports why the workflows directory cannot be used.
//
// The directory holds no workflow: the failures say why, when there are
// any.
func (s *Service) workflowsReason() string {
	if len(s.catalog.Names()) > 0 {
		return ""
	}
	if failures := s.catalog.Failures(); len(failures) > 0 {
		return strings.Join(failureLines(failures), "; ")
	}
	return fmt.Sprintf("there are no workflows in %s", s.cfg.WorkflowsDir)
}

// failureLines renders the failures of the directory, one per line.
//
// A failure carries every problem its file has, one per line: the
// reason is one line, so the lines of a failure are joined.
func failureLines(failures []error) []string {
	out := make([]string, 0, len(failures))
	for _, err := range failures {
		out = append(out, strings.Join(strings.Split(err.Error(), "\n"), "; "))
	}
	return out
}

// Start reads the workflows, registers the actions and resumes the
// runs that were left behind by a previous daemon.
//
// A file that does not load is not fatal: the daemon comes up and the
// workflows beside it work. The failure is logged and reported by
// `/me`.
//
// A run that cannot go on is reported to its chat; a run that can
// is resumed silently.
func (s *Service) Start(ctx context.Context) error {
	err := s.catalog.Refresh()
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("me: %w", err)
	}
	for _, failure := range s.catalog.Failures() {
		s.logger.Warn("me: a workflow did not load", "dir", s.cfg.WorkflowsDir, "err", failure)
	}

	registerBuiltinActions(s.actions)
	if err := s.actions.ScanUserScripts(s.actionsDir()); err != nil {
		return fmt.Errorf("me: scan action scripts: %w", err)
	}

	s.resumeRuns(ctx)
	return nil
}

// Shutdown stops every run and waits for them to write their final
// state.
func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.started = false
	runs := make([]*run, 0, len(s.runs))
	for _, r := range s.runs {
		runs = append(runs, r)
	}
	s.mu.Unlock()

	for _, r := range runs {
		r.suspend()
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WorkflowsDir returns the directory the workflows live in.
func (s *Service) WorkflowsDir() string {
	return s.cfg.WorkflowsDir
}

// actionsDir is the directory the user-defined action scripts live
// in.
func (s *Service) actionsDir() string {
	return filepath.Join(s.cfg.WorkflowsDir, "actions")
}

// StartWorkflow starts a workflow in a chat.
//
// The workflows directory is read first, so a file that was edited
// since the last start takes effect now.
//
// The chat gets one run at a time: a second start in the same chat
// is refused.
func (s *Service) StartWorkflow(ctx context.Context, req Request) (string, error) {
	if err := s.catalog.Refresh(); err != nil {
		return "", fmt.Errorf("me: %w", err)
	}
	if reason := s.modelsReason(); reason != "" {
		return "", fmt.Errorf("me is disabled: %s", reason)
	}
	if reason := s.operatorReason(); reason != "" {
		return "", fmt.Errorf("me is disabled: %s", reason)
	}
	if strings.TrimSpace(req.ChatID) == "" {
		return "", errors.New("me: the run has no chat")
	}

	wf, ok := s.catalog.Lookup(req.Name)
	if !ok {
		// A file that did not load carries its own failure: the
		// operator asked for that name, and the file is on disk.
		if failure, broken := s.catalog.Failure(req.Name); broken {
			return "", failure
		}
		if reason := s.workflowsReason(); reason != "" {
			return "", fmt.Errorf("me is disabled: %s", reason)
		}
		return "", fmt.Errorf("there is no workflow named %q (known: %s)",
			req.Name, strings.Join(s.catalog.Names(), ", "))
	}

	values, err := mapArgValues(wf, req.Args)
	if err != nil {
		return "", err
	}
	bound, err := wfe.Bind(wf, values)
	if err != nil {
		return "", err
	}
	// The override lands on the bound workflow — the copy the run
	// owns. The file and the catalogue's copy of it keep their own
	// counts, so the override is this run's and not the workflow's.
	if req.Iterations != nil {
		wfe.OverrideIterations(bound, *req.Iterations)
	}
	r := newRun(s, req, bound, values)

	// The check and the insert happen under one lock: two commands
	// that arrive at the same time must not both start a run in the
	// same chat.
	s.mu.Lock()
	if existing, ok := s.active[req.ChatID]; ok {
		s.mu.Unlock()
		return "", fmt.Errorf("this chat already has a run in it (%s)", existing.id)
	}
	s.runs[r.id] = r
	s.active[req.ChatID] = r
	s.mu.Unlock()

	// The run's session goes up before the first step: the chat shows
	// what the run is from the start. Its card hangs under the message
	// that started the run.
	//
	// A chat that cannot show the card cannot show the run: the run
	// is refused before it does any work.
	if !s.openRunSession(ctx, r, req.MessageID) {
		s.finish(r, PhaseFailed, "the chat cannot show the run's card")
		return "", errors.New("me: the chat cannot show the run's card")
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.drive(ctx, r)
	}()
	return r.id, nil
}

// mapArgValues maps the command line's tokens onto the workflow's
// declared arguments.
//
// A token of the form name=value assigns that argument by name; an
// argument the workflow does not declare is an error. Every other
// token fills the next argument that is still unassigned, in
// declaration order. More positional tokens than declared arguments
// is an error: a typo never runs a workflow with a wrong value.
func mapArgValues(wf *wfe.Workflow, tokens []string) (map[string]string, error) {
	declared := wf.ArgNames()
	out := map[string]string{}
	var positional []string
	for _, tok := range tokens {
		name, value, ok := strings.Cut(tok, "=")
		if !ok {
			positional = append(positional, tok)
			continue
		}
		// An argument the workflow does not declare is refused by
		// the bind that follows.
		out[name] = value
	}
	next := 0
	for _, value := range positional {
		for next < len(declared) {
			if _, ok := out[declared[next]]; !ok {
				break
			}
			next++
		}
		if next >= len(declared) {
			return nil, tooManyArgs(wf, len(tokens))
		}
		out[declared[next]] = value
		next++
	}
	return out, nil
}

// tooManyArgs reports that the command line carries more arguments
// than the workflow takes.
func tooManyArgs(wf *wfe.Workflow, given int) error {
	declared := wf.ArgNames()
	return fmt.Errorf("the workflow takes %d argument(s) (%s), %d were given",
		len(declared), strings.Join(declared, ", "), given)
}

// Reply hands a reply to the run that is waiting for one.
//
// A reply to a chat without a waiting run is refused.
func (s *Service) Reply(ctx context.Context, chatID, text string) error {
	s.mu.Lock()
	r, ok := s.active[chatID]
	s.mu.Unlock()
	if !ok {
		return errors.New("this chat has no run in it")
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("the reply is empty")
	}
	return r.deliver(text)
}

// Pause asks the run to stop after the step it is on.
//
// The step that is running is not interrupted: its outcome is
// recorded, and the run waits for the operator to hand the control
// back.
func (s *Service) Pause(ctx context.Context, chatID string) error {
	s.mu.Lock()
	r, ok := s.active[chatID]
	s.mu.Unlock()
	if !ok {
		return errors.New("this chat has no run in it")
	}
	r.requestPause("the operator paused the run")
	s.refreshSessions(r)
	return nil
}

// Continue hands the control back to a paused run.
//
// A note is the operator's instruction: it joins the conversation the
// chat model reads.
func (s *Service) Continue(ctx context.Context, chatID, note string) error {
	s.mu.Lock()
	r, ok := s.active[chatID]
	s.mu.Unlock()
	if !ok {
		return errors.New("this chat has no run in it")
	}
	if err := r.handBack(note); err != nil {
		return err
	}
	// The reset of the step's budget is part of the run's state: a
	// daemon that goes away now comes back with it.
	s.persist(r)
	s.refreshSessions(r)
	return nil
}

// Show puts the run back in front of the operator.
//
// The card the run opened is not always the one the operator wants to
// look at: `/me show` posts the overview of the run as a card of its
// own. The card is static — the live state of the work is on the
// card of the round.
func (s *Service) Show(ctx context.Context, chatID string) error {
	s.mu.Lock()
	r, ok := s.active[chatID]
	s.mu.Unlock()
	if !ok {
		return errors.New("this chat has no run in it")
	}
	if !s.openRunSession(ctx, r, "") {
		return errors.New("me: the chat cannot show the run's state")
	}
	return nil
}

// Stop cancels a run and tells the chat to stop working.
//
// The run's context is cancelled first, so the loop stops at its next
// checkpoint; /stop then reaches the agent that is still working.
//
// The run is marked as stopped by the operator before it is cancelled:
// the mark is what tells an operator's stop from every other way a
// run ends, and the summary of the session is built from it.
func (s *Service) Stop(ctx context.Context, chatID string) error {
	s.mu.Lock()
	r, ok := s.active[chatID]
	s.mu.Unlock()
	if !ok {
		return errors.New("this chat has no run in it")
	}
	r.markStoppedByOperator()
	r.cancel()
	if s.cfg.Operator != nil {
		if _, err := s.speak(ctx, r, "/stop", ""); err != nil {
			s.logger.Warn("me: the chat could not be stopped", "run_id", r.id, "err", err)
		}
	}
	return nil
}

// Status returns the state of the run in a chat.
//
// A chat without a run gets an idle status; the error says why the
// personal agent cannot start one.
func (s *Service) Status(chatID string) Status {
	s.mu.Lock()
	r, ok := s.active[chatID]
	s.mu.Unlock()
	if !ok {
		// The error says why the personal agent cannot start a run;
		// a chat without a run has nothing to report.
		st := Status{Phase: PhaseIdle}
		if reason := s.DisabledReason(); reason != "" {
			st.Error = reason
		}
		return st
	}
	return r.status()
}

// finish records how a run ended and frees its chat.
func (s *Service) finish(r *run, phase Phase, reason string) {
	r.finish(phase, reason)
	s.mu.Lock()
	if s.active[r.chatID] == r {
		delete(s.active, r.chatID)
	}
	delete(s.runs, r.id)
	s.mu.Unlock()
}

// executionOf returns the tracked execution of a message.
func (s *Service) executionOf(msgID string) (observe.Execution, bool) {
	if s.cfg.Operator == nil {
		return observe.Execution{}, false
	}
	return s.cfg.Operator.Get(msgID)
}

// statePath returns the file a run is persisted in.
func (s *Service) statePath(runID string) string {
	return filepath.Join(s.cfg.StateDir, runID+".json")
}
