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
// Both drive the daemon through the loopback channel, the way a
// person drives it through Feishu: every command is a real message
// with a real id, and every answer comes back as a structured
// outcome. The workflow's policy (internal/wfe.Policy) is the
// boundary — a tool call the workflow does not allow comes back as
// an error the model can correct.
//
// The split:
//
//   - internal/channel/loopback — transport. How a message gets into
//     the daemon, and how its outcome comes back.
//   - internal/wfe — the workflow schema, the engine that computes
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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/channel/loopback"
	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// Defaults of the personal agent.
const (
	// DefaultMinConfidence is the confidence a System 1 decision
	// must reach before it is acted on without the chat model.
	DefaultMinConfidence = 0.8

	// DefaultTokenBudget caps the tokens one run may spend on the
	// chat model.
	DefaultTokenBudget = 200_000

	// DefaultStepTimeout caps one step of a run.
	DefaultStepTimeout = 30 * time.Minute

	// DefaultStallTimeout is how long a step may go without a
	// heartbeat before the run stops waiting for it.
	DefaultStallTimeout = 10 * time.Minute
)

// Phase is where a run stands.
type Phase string

const (
	// PhaseIdle means the chat has no run.
	PhaseIdle Phase = "idle"
	// PhaseRunning means the run is working.
	PhaseRunning Phase = "running"
	// PhaseWaiting means the run is waiting for a step to settle.
	PhaseWaiting Phase = "waiting"
	// PhasePaused means the run is waiting for the user's reply.
	PhasePaused Phase = "paused"
	// PhaseDone means the run finished its workflow.
	PhaseDone Phase = "done"
	// PhaseFailed means the run could not go on.
	PhaseFailed Phase = "failed"
	// PhaseStopped means the user stopped the run.
	PhaseStopped Phase = "stopped"
)

// Request is a request to run a workflow.
type Request struct {
	// Name is the workflow's name.
	Name string
	// Args are the arguments of the command line. A token of the
	// form name=value assigns that argument by name; every other
	// token fills the next declared argument, in order.
	Args []string
	// ObserverChatID is the chat the run reports to and takes
	// answers from.
	ObserverChatID string
	// Cwd is the working directory the run works in.
	Cwd string
	// Agent is the agent the work chat uses.
	Agent string
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
	Execution loopback.Phase
	// Progress is the heartbeat view of the running command.
	Progress loopback.Progress
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
	// Error is why the run cannot go on, when it cannot.
	Error string
}

// ChoiceAnswerer answers a pending interactive prompt on a work chat.
//
// The loopback channel reports the prompts a run still has to answer;
// answering one is the daemon's business, not the agent's.
type ChoiceAnswerer interface {
	// Answer delivers the option to the prompt with the given
	// request id.
	Answer(chatID, requestID, optionID string) error
}

// Loopback drives the work chat.
//
// It is the slice of the loopback client the personal agent needs:
// send a command into a chat, watch how it goes, and read the
// execution back. The agent depends on the interface rather than on
// the concrete client, so a run can be driven without a daemon behind
// it.
type Loopback interface {
	// Send pushes text into the chat and returns the id of the
	// message it became.
	Send(ctx context.Context, chatID, text string) (string, error)
	// Do sends text and waits for the command to settle.
	Do(ctx context.Context, chatID, text string) (loopback.Execution, error)
	// Get returns the execution of a message, without consuming it.
	Get(msgID string) (loopback.Execution, bool)
	// Watch returns a channel of snapshots of an execution.
	Watch(msgID string) <-chan loopback.Execution
}

// GitRunner runs git in a workspace.
type GitRunner interface {
	// Run runs git with the given arguments in dir.
	Run(ctx context.Context, dir string, args ...string) (stdout, stderr string, err error)
}

// CwdProvider reports the directory a chat currently works in.
//
// The work chat's directory moves when a command moves it — `/gtw fix`
// moves it into the worktree — so a shell step runs where the chat
// stands.
type CwdProvider interface {
	// Cwd returns the directory the chat currently works in.
	Cwd(chatID string) string
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
	// TokenBudget caps the tokens one run may spend on the chat
	// model. Zero means DefaultTokenBudget.
	TokenBudget int
	// StepTimeout caps one step. Zero means DefaultStepTimeout.
	StepTimeout time.Duration
	// StallTimeout is how long a step may go without a heartbeat.
	// Zero means DefaultStallTimeout.
	StallTimeout time.Duration

	// Loopback drives the work chat.
	Loopback Loopback
	// Shell runs the shell commands of the workflow's steps. Nil
	// turns the shell step kind off.
	Shell ShellRunner
	// Sender posts the progress to the observer chat.
	Sender messages.ChatSender
	// Answerer answers the pending prompts of the work chat.
	Answerer ChoiceAnswerer
	// Git reads the repository facts of the work chat.
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
// One run is active per observer chat: the chat that started a run is
// the chat that gets the progress, the questions and the summary.
type Service struct {
	cfg    Config
	logger *slog.Logger

	mu sync.Mutex
	// workflows holds the loaded workflows by name.
	workflows map[string]*wfe.Workflow
	// runs holds every run by id.
	runs map[string]*run
	// active maps an observer chat id to the run it started.
	active map[string]*run
	// started reports whether the service is up.
	started bool
	// loadErr is why the workflows could not be loaded.
	loadErr error

	// actions holds the actions a `use:` step can name.
	actions *ActionRegistry

	// wg tracks the run goroutines, so a shutdown can wait for them.
	wg sync.WaitGroup
}

// New constructs a Service. The workflows are loaded at Start, not
// here.
func New(cfg Config) *Service {
	if cfg.MinConfidence <= 0 {
		cfg.MinConfidence = DefaultMinConfidence
	}
	if cfg.TokenBudget <= 0 {
		cfg.TokenBudget = DefaultTokenBudget
	}
	if cfg.StepTimeout <= 0 {
		cfg.StepTimeout = DefaultStepTimeout
	}
	if cfg.StallTimeout <= 0 {
		cfg.StallTimeout = DefaultStallTimeout
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		cfg:       cfg,
		logger:    logger,
		workflows: map[string]*wfe.Workflow{},
		runs:      map[string]*run{},
		active:    map[string]*run{},
		actions:   NewActionRegistry(),
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
	if s.cfg.Provider == nil && s.cfg.Decider == nil {
		return "neither me.llm nor me.decider is configured"
	}
	s.mu.Lock()
	n := len(s.workflows)
	loadErr := s.loadErr
	s.mu.Unlock()
	if loadErr != nil {
		return loadErr.Error()
	}
	if n == 0 {
		return fmt.Sprintf("there are no workflows in %s", s.cfg.WorkflowsDir)
	}
	return ""
}

// Start loads the workflows, registers the actions and resumes the
// runs that were left behind by a previous daemon.
//
// A run that cannot go on is reported to its observer; a run that can
// is resumed silently.
func (s *Service) Start(ctx context.Context) error {
	workflows, err := loadWorkflows(s.cfg.WorkflowsDir)
	s.mu.Lock()
	s.loadErr = err
	s.workflows = workflows
	s.started = true
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("me: %w", err)
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

// loadWorkflows reads the workflows of the workflows directory.
//
// A missing directory is not an error: a daemon without workflows is
// simply idle.
func loadWorkflows(dir string) (map[string]*wfe.Workflow, error) {
	out := map[string]*wfe.Workflow{}
	if strings.TrimSpace(dir) == "" {
		return out, nil
	}
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("the workflows directory %s cannot be read: %w", dir, err)
	}
	wfs, err := wfe.LoadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("the workflows in %s cannot be loaded: %w", dir, err)
	}
	for _, wf := range wfs {
		out[wf.Name] = wf
	}
	return out, nil
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
// The chat gets one run at a time: a second start in the same chat
// is refused.
func (s *Service) StartWorkflow(ctx context.Context, req Request) (string, error) {
	if reason := s.DisabledReason(); reason != "" {
		return "", fmt.Errorf("me is disabled: %s", reason)
	}
	if strings.TrimSpace(req.ObserverChatID) == "" {
		return "", errors.New("me: the run has no observer chat")
	}

	s.mu.Lock()
	wf, ok := s.workflows[req.Name]
	if !ok {
		known := strings.Join(s.workflowNames(), ", ")
		s.mu.Unlock()
		return "", fmt.Errorf("there is no workflow named %q (known: %s)", req.Name, known)
	}
	s.mu.Unlock()

	values, err := mapArgValues(wf, req.Args)
	if err != nil {
		return "", err
	}
	bound, err := wfe.Bind(wf, values)
	if err != nil {
		return "", err
	}
	r := newRun(s, req, bound)

	// The check and the insert happen under one lock: two commands
	// that arrive at the same time must not both start a run in the
	// same chat.
	s.mu.Lock()
	if existing, ok := s.active[req.ObserverChatID]; ok {
		s.mu.Unlock()
		return "", fmt.Errorf("this chat already has a run in it (%s)", existing.id)
	}
	s.runs[r.id] = r
	s.active[req.ObserverChatID] = r
	s.mu.Unlock()

	if err := s.setupWorkChat(ctx, r); err != nil {
		s.finish(r, PhaseFailed, err.Error())
		return "", err
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.drive(ctx, r)
	}()
	return r.id, nil
}

// workflowNames returns the loaded workflow names, sorted. The caller
// holds the lock.
func (s *Service) workflowNames() []string {
	out := make([]string, 0, len(s.workflows))
	for name := range s.workflows {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
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

// setupWorkChat points the work chat at the run's workspace and
// agent.
//
// The two commands are sent one at a time and each one is awaited:
// /use must not run against a chat that has no workspace yet.
func (s *Service) setupWorkChat(ctx context.Context, r *run) error {
	if s.cfg.Loopback == nil {
		return errors.New("me: no loopback client: the work chat cannot be set up")
	}
	if r.cwd != "" {
		if err := s.setupStep(ctx, r, "/cwd "+r.cwd); err != nil {
			return err
		}
	}
	if r.agent != "" {
		if err := s.setupStep(ctx, r, "/use "+r.agent); err != nil {
			return err
		}
	}
	return nil
}

// setupStep sends one setup command and checks the outcome it
// produced.
func (s *Service) setupStep(ctx context.Context, r *run, text string) error {
	ex, err := s.cfg.Loopback.Do(ctx, r.workChatID, text)
	if err != nil {
		return fmt.Errorf("the work chat could not run %q: %w", text, err)
	}
	if ex.Outcome.Status != messages.OutcomeOK {
		return fmt.Errorf("the work chat could not run %q: %s", text, outcomeLine(ex.Outcome))
	}
	return nil
}

// Reply hands a reply to the run that is waiting for one.
//
// A reply to a chat without a waiting run is refused.
func (s *Service) Reply(ctx context.Context, observerChatID, text string) error {
	s.mu.Lock()
	r, ok := s.active[observerChatID]
	s.mu.Unlock()
	if !ok {
		return errors.New("this chat has no run in it")
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("the reply is empty")
	}
	return r.deliver(text)
}

// Stop cancels a run and stops the work chat.
//
// The run's context is cancelled first, so the loop stops at its next
// checkpoint; /stop then reaches the agent that is still working.
func (s *Service) Stop(ctx context.Context, observerChatID string) error {
	s.mu.Lock()
	r, ok := s.active[observerChatID]
	s.mu.Unlock()
	if !ok {
		return errors.New("this chat has no run in it")
	}
	r.cancel()
	if s.cfg.Loopback != nil {
		if _, err := s.cfg.Loopback.Send(ctx, r.workChatID, "/stop"); err != nil {
			s.logger.Warn("me: the work chat could not be stopped", "run_id", r.id, "err", err)
		}
	}
	return nil
}

// Status returns the state of the run in a chat.
//
// A chat without a run gets an idle status; the error says why the
// personal agent cannot start one.
func (s *Service) Status(observerChatID string) Status {
	s.mu.Lock()
	r, ok := s.active[observerChatID]
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

// finish records how a run ended and tells its observer.
func (s *Service) finish(r *run, phase Phase, reason string) {
	r.finish(phase, reason)
	s.mu.Lock()
	if s.active[r.observerChatID] == r {
		delete(s.active, r.observerChatID)
	}
	delete(s.runs, r.id)
	s.mu.Unlock()
}

// executionOf returns the tracked execution of a message.
func (s *Service) executionOf(msgID string) (loopback.Execution, bool) {
	if s.cfg.Loopback == nil {
		return loopback.Execution{}, false
	}
	return s.cfg.Loopback.Get(msgID)
}

// statePath returns the file a run is persisted in.
func (s *Service) statePath(runID string) string {
	return filepath.Join(s.cfg.StateDir, runID+".json")
}
