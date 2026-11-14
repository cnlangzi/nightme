package me

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/gateway/observe"
	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// pendingCall is a tool call the model asked for that has not been
// executed yet.
type pendingCall struct {
	// ID ties the call to the tool result that answers it.
	ID string `json:"id"`
	// Name is the tool the model asked for.
	Name string `json:"name"`
	// Args holds the JSON arguments of the call.
	Args json.RawMessage `json:"args"`
}

// askState is the question a run is waiting for.
type askState struct {
	// ToolCallID is the call the question belongs to.
	ToolCallID string `json:"tool_call_id"`
	// Question is the text the operator was asked.
	Question string `json:"question"`
	// Options are the choices the operator may pick, when the
	// question is a choice between them.
	Options []llm.Option `json:"options,omitempty"`
	// MsgID is the id of the message the question was asked in. A
	// reply to that message is an answer, not a takeover.
	MsgID string `json:"msg_id,omitempty"`
}

// inflightState is the command a run is waiting for.
type inflightState struct {
	// MsgID is the chat message the command became.
	MsgID string `json:"msg_id"`
	// Command is the text that was sent.
	Command string `json:"command"`
	// Instruct is true when the command is a free-form instruction
	// rather than a step of the workflow.
	Instruct bool `json:"instruct"`
}

// run is one workflow run.
type run struct {
	svc *Service

	id       string
	chatID   string
	cwd      string
	owner    string
	argNames []string
	argVals  map[string]string

	// workflow is the bound workflow the run works through.
	workflow *wfe.Workflow
	// policy bounds what the run may do.
	policy *wfe.Policy
	// state is the progress of the run: the steps that ran and what
	// they produced. The engine owns it.
	state *wfe.RunState

	// messages is the conversation with the chat model.
	messages []llm.Message
	// pendingCalls are the tool calls that still have to run.
	pendingCalls []pendingCall
	// ask is the question the run is waiting for.
	ask *askState
	// inflight is the command the run is waiting for.
	inflight *inflightState

	phase    Phase
	question string
	errMsg   string

	// paused reports whether the run waits for the operator.
	paused bool
	// pauseRequested reports whether a pause was asked for and the
	// run has not applied it yet.
	pauseRequested bool
	// pauseReason says why the run paused.
	pauseReason string

	// cardMsgID is the id of the run's overview message.
	cardMsgID string

	// system1Decisions and system2Decisions count what each
	// capability decided.
	system1Decisions int
	system2Decisions int
	// system1Usage and system2Usage count the tokens each
	// capability spent.
	system1Usage llm.Usage
	system2Usage llm.Usage
	// nudged records that the chat model answered with text only
	// and was asked to call a tool.
	nudged bool
	// lastTranscript is the tail of the last action's transcript.
	lastTranscript string
	// system1Model is the decision model that answered last.
	system1Model string
	// judgment is why the workflow cannot decide on its own.
	judgment string
	// lastDecision names who decided last: the workflow, the
	// decision model or the chat model.
	lastDecision string
	// lastReason says why that decision was made.
	lastReason string
	// lastNote is what the chat model said last.
	lastNote string
	// summary is how the run ended.
	summary string
	// finished records that the model called the finish tool.
	finished bool
	// suspended records that the daemon went away while the run was
	// going: the run keeps its state and resumes when it comes back.
	suspended bool

	mu sync.Mutex
	// ctx is the run's scope. Cancelling it stops the run.
	ctx context.Context
	// cancel stops the run.
	cancel context.CancelFunc
	// resume carries the signal that a reply arrived.
	resume chan struct{}
	// unpause carries the signal that the control came back.
	unpause chan struct{}
	// done is closed when the run's goroutine is over.
	done chan struct{}
	// reply is the text the operator answered with.
	reply string
	// watch is the channel the running command is watched on. It is
	// kept across the calls that resume a command, so the tracker
	// never grows a watcher nobody reads.
	watch <-chan observe.Execution
}

// newRun builds a run for a bound workflow.
func newRun(svc *Service, req Request, wf *wfe.Workflow, args map[string]string) *run {
	ctx, cancel := context.WithCancel(context.Background())
	id := fmt.Sprintf("%s-%d", req.Name, svc.now().UnixNano())
	now := svc.now()
	names := make([]string, 0, len(args))
	for name := range args {
		names = append(names, name)
	}
	slices.Sort(names)
	return &run{
		svc:      svc,
		id:       id,
		chatID:   req.ChatID,
		cwd:      req.Cwd,
		owner:    req.Owner,
		argNames: names,
		argVals:  args,
		workflow: wf,
		policy:   wfe.NewPolicy(wf, nil),
		state:    newRunState(id, wf.Name, req.Cwd, now),
		phase:    PhaseRunning,
		ctx:      ctx,
		cancel:   cancel,
		resume:   make(chan struct{}, 1),
		unpause:  make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
}

// newRunState builds the progress of a run.
func newRunState(runID, workflow, cwd string, now time.Time) *wfe.RunState {
	return &wfe.RunState{
		RunID:        runID,
		WorkflowName: workflow,
		Workspace:    cwd,
		Status:       wfe.StatusRunning,
		Env:          map[string]string{"WORKSPACE_CWD": cwd},
		StartedAt:    now,
		UpdatedAt:    now,
	}
}

// status returns the run's state.
func (r *run) status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := Status{
		RunID:            r.id,
		Phase:            r.phase,
		Actions:          len(r.policy.History),
		Remaining:        r.policy.Remaining(),
		System1Decisions: r.system1Decisions,
		System2Decisions: r.system2Decisions,
		System1Usage:     r.system1Usage,
		System2Usage:     r.system2Usage,
		Question:         r.question,
		Paused:           r.paused,
		PauseReason:      r.pauseReason,
		Error:            r.errMsg,
	}
	if r.inflight != nil {
		st.Step = r.inflight.Command
		// The execution of the running command carries its phase and
		// its heartbeat.
		if ex, ok := r.svc.executionOf(r.inflight.MsgID); ok {
			st.Execution = ex.Phase
			st.Progress = ex.Progress
		}
	}
	return st
}

// currentPhase returns where the run stands.
func (r *run) currentPhase() Phase {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.phase
}

// setPhase records where the run stands.
func (r *run) setPhase(p Phase) {
	r.mu.Lock()
	r.phase = p
	r.mu.Unlock()
}

// finish records how the run ended.
func (r *run) finish(phase Phase, reason string) {
	r.mu.Lock()
	r.phase = phase
	if reason != "" {
		r.errMsg = reason
	}
	r.mu.Unlock()
	r.cancel()
}

// deliver hands the operator's reply to the waiting run.
//
// A reply that names an option by its number is the answer the run
// is waiting for: the card's buttons carry the number, because a
// chat button has room for a few bytes only.
func (r *run) deliver(text string) error {
	r.mu.Lock()
	if r.ask == nil {
		r.mu.Unlock()
		return errors.New("this run is not waiting for a reply")
	}
	r.reply = resolveChoice(r.ask, text)
	r.ask = nil
	r.question = ""
	r.phase = PhaseRunning
	r.mu.Unlock()

	select {
	case r.resume <- struct{}{}:
	default:
	}
	return nil
}

// awaitReply waits for the operator's reply.
//
// The wait ends when the run is cancelled, when the reply arrives, or
// when ctx ends.
func (r *run) awaitReply(ctx context.Context) (string, error) {
	select {
	case <-r.resume:
	case <-r.ctx.Done():
		return "", r.ctx.Err()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	r.mu.Lock()
	reply := r.reply
	r.reply = ""
	r.mu.Unlock()
	return reply, nil
}

// requestPause records that the run should stop after the step it is
// on.
func (r *run) requestPause(reason string) {
	r.mu.Lock()
	r.pauseRequested = true
	r.pauseReason = reason
	r.mu.Unlock()
}

// applyPause moves the run into the paused state when a pause was
// asked for. It reports whether the run paused.
func (r *run) applyPause() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.pauseRequested {
		return false
	}
	r.pauseRequested = false
	r.paused = true
	r.phase = PhasePaused
	return true
}

// isPaused reports whether the run waits for the operator.
func (r *run) isPaused() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.paused
}

// handBack hands the control back to a paused run.
//
// A pause that has not taken effect yet — the step it was asked for
// is still running — is cancelled: the operator changed their mind
// before the run stopped.
//
// A note is the operator's instruction: it joins the conversation the
// chat model reads.
func (r *run) handBack(note string) error {
	r.mu.Lock()
	if !r.paused && !r.pauseRequested {
		waiting := r.ask != nil
		r.mu.Unlock()
		if waiting {
			return errors.New("this run is waiting for an answer: use /me reply <text>")
		}
		return errors.New("this run is not paused")
	}
	pending := r.pauseRequested
	r.pauseRequested = false
	r.paused = false
	r.pauseReason = ""
	r.phase = PhaseRunning
	if strings.TrimSpace(note) != "" {
		r.messages = append(r.messages, llm.Message{
			Role:  llm.RoleUser,
			Parts: []llm.Part{{Type: llm.PartText, Text: note}},
		})
	}
	r.mu.Unlock()

	if pending {
		// The pause had not taken effect: there is nobody to wake up.
		return nil
	}
	select {
	case r.unpause <- struct{}{}:
	default:
	}
	return nil
}

// resolveChoice turns a reply that names an option by its number into
// the option's value.
//
// A reply that is not a number is the answer itself.
func resolveChoice(ask *askState, text string) string {
	trimmed := strings.TrimSpace(text)
	n, err := strconv.Atoi(trimmed)
	if err != nil || n < 0 {
		return text
	}
	options := askOptions(ask)
	if n >= len(options) {
		return text
	}
	return options[n].Value
}

// awaitUnpause waits until the control comes back.
func (r *run) awaitUnpause(ctx context.Context) error {
	select {
	case <-r.unpause:
		return nil
	case <-r.ctx.Done():
		return r.ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// askMsgID returns the id of the message the run asked its question
// in.
func (r *run) askMsgID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ask == nil {
		return ""
	}
	return r.ask.MsgID
}

// isAnswerTo reports whether the message answers the run's question.
//
// A reply to the question is an answer; anything else is somebody
// else taking the chat back.
func (r *run) isAnswerTo(msg *messages.InboundMessage) bool {
	if msg == nil || msg.ReplyTo == "" {
		return false
	}
	return msg.ReplyTo == r.askMsgID()
}

// appendMessage adds a turn to the conversation.
func (r *run) appendMessage(m llm.Message) {
	r.mu.Lock()
	r.messages = append(r.messages, m)
	r.mu.Unlock()
}

// conversation returns a copy of the conversation.
func (r *run) conversation() []llm.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]llm.Message(nil), r.messages...)
}

// checkRun asks the policy whether a command may run now.
func (r *run) checkRun(command string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.policy.CheckRun(command)
}

// checkInstruct asks the policy whether an instruction is allowed.
func (r *run) checkInstruct() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.policy.CheckInstruct()
}

// checkFinish asks the policy whether the run may end.
func (r *run) checkFinish(status string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.policy.CheckFinish(status)
}

// checkStep asks the policy whether the step may take an action now:
// its own limit for the current round.
func (r *run) checkStep(step wfe.Step) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := r.stepIndex(step.ID)
	if idx < 0 {
		return fmt.Errorf("the step %q is not part of this workflow", step.ID)
	}
	return r.policy.CheckStep(idx)
}

// stepAt returns the workflow's step at the policy's index.
func (r *run) stepAt(idx int) (wfe.Step, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	steps := r.workflow.Steps()
	if idx < 0 || idx >= len(steps) {
		return wfe.Step{}, false
	}
	return steps[idx], true
}

// isStepMessage reports whether the command is the step's own message.
func (r *run) isStepMessage(idx int, command string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.policy.IsStepMessage(idx, command)
}

// judgmentReason returns why the workflow cannot decide on its own.
func (r *run) judgmentReason() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.judgment
}

// setJudgment records why the workflow cannot decide on its own.
func (r *run) setJudgment(reason string) {
	r.mu.Lock()
	r.judgment = reason
	r.mu.Unlock()
}

// setDecision records who decided last and why.
func (r *run) setDecision(who, reason string) {
	r.mu.Lock()
	r.lastDecision = who
	r.lastReason = reason
	r.mu.Unlock()
}

// recordStep records the outcome of a step in the run's progress.
//
// The engine owns the state: the step that ran and its outcome go
// in together.
func (r *run) recordStep(job string, step wfe.Step, out wfe.Outcome) {
	r.mu.Lock()
	wfe.RecordStep(r.state, job, step, out, false)
	r.mu.Unlock()
}

// recordPending records that a step started and has not settled.
func (r *run) recordPending(job string, step wfe.Step) {
	r.mu.Lock()
	wfe.RecordPending(r.state, job, step)
	r.mu.Unlock()
}

// stepIndex returns the index of the step in the workflow's step list,
// or -1.
func (r *run) stepIndex(stepID string) int {
	for i, step := range r.workflow.Steps() {
		if step.ID == stepID {
			return i
		}
	}
	return -1
}

// allowedCommands returns the commands the policy accepts now.
func (r *run) allowedCommands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.policy.AllowedCommands()
}

// renderPolicy returns the workflow as the model reads it.
func (r *run) renderPolicy() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.policy.Render()
}

// record appends an action to the policy's history.
//
// The record carries the round the step's job is in: the per-step
// budgets count the attempts of the current round only.
func (r *run) record(kind wfe.ActionKind, command, stepID string, outcome wfe.Outcome) {
	r.mu.Lock()
	r.policy.Record(wfe.ActionRecord{
		Kind:    kind,
		Command: command,
		StepID:  stepID,
		Round:   r.roundOf(stepID),
		Outcome: outcome,
		At:      r.svc.now(),
	})
	r.mu.Unlock()
}

// roundOf returns the round the step's job is in. The caller holds the
// lock.
func (r *run) roundOf(stepID string) int {
	job := wfe.JobOf(r.workflow, stepID)
	if job == "" {
		return 1
	}
	if round := r.state.Rounds[job]; round > 0 {
		return round
	}
	return 1
}

// exprCtx renders the run's progress as the context an expression
// reads.
func (r *run) exprCtx() wfe.ExprCtx {
	r.mu.Lock()
	defer r.mu.Unlock()
	return wfe.ExprContext(r.state)
}

// lastOutcome returns the outcome of the last step that ran.
func (r *run) lastOutcome() (wfe.Outcome, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return wfe.LastOutcome(r.state)
}

// runContext returns a context that ends when either the caller's
// context or the run's own context ends.
//
// A command the run spawns is bound to it: `/me stop` and the
// daemon's shutdown both stop the child instead of leaving it behind.
func (r *run) runContext(ctx context.Context) (context.Context, context.CancelFunc) {
	merged, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.ctx, cancel)
	return merged, func() {
		stop()
		cancel()
	}
}

// env returns the run's environment.
func (r *run) env() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.Env
}

// hasRun reports whether the step already ran.
func (r *run) hasRun(stepID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return wfe.HasRun(r.state, stepID)
}

// outcomeLine renders an outcome as one human-readable line.
func outcomeLine(o messages.Outcome) string {
	if o.Summary != "" {
		return o.Summary
	}
	return string(o.Status)
}

// persistedRun is the on-disk shape of a run.
//
// One file holds everything: the DAG progress, the conversation and
// the judgment history.
type persistedRun struct {
	RunID            string             `json:"run_id"`
	ChatID           string             `json:"chat_id"`
	Cwd              string             `json:"cwd"`
	Owner            string             `json:"owner"`
	Args             map[string]string  `json:"args,omitempty"`
	Workflow         *wfe.Workflow      `json:"workflow"`
	State            *wfe.RunState      `json:"state"`
	History          []wfe.ActionRecord `json:"history"`
	Messages         []llm.Message      `json:"messages"`
	PendingCalls     []pendingCall      `json:"pending_calls"`
	Ask              *askState          `json:"ask,omitempty"`
	Inflight         *inflightState     `json:"inflight,omitempty"`
	Phase            Phase              `json:"phase"`
	Question         string             `json:"question"`
	Error            string             `json:"error"`
	Paused           bool               `json:"paused"`
	PauseReason      string             `json:"pause_reason,omitempty"`
	RunCardMsgID     string             `json:"run_card_msg_id,omitempty"`
	System1Decisions int                `json:"system1_decisions"`
	System2Decisions int                `json:"system2_decisions"`
	System1Usage     llm.Usage          `json:"system1_usage"`
	System2Usage     llm.Usage          `json:"system2_usage"`
	Nudged           bool               `json:"nudged"`
}

// persist writes the run's state to disk.
//
// The state is written after every tool result, so a daemon that dies
// mid-run comes back with the conversation and the policy intact.
func (s *Service) persist(r *run) {
	if s.cfg.StateDir == "" {
		return
	}
	r.mu.Lock()
	state := persistedRun{
		RunID:            r.id,
		ChatID:           r.chatID,
		Cwd:              r.cwd,
		Owner:            r.owner,
		Args:             r.argVals,
		Workflow:         r.workflow,
		State:            r.state,
		History:          append([]wfe.ActionRecord(nil), r.policy.History...),
		Messages:         append([]llm.Message(nil), r.messages...),
		PendingCalls:     append([]pendingCall(nil), r.pendingCalls...),
		Ask:              r.ask,
		Inflight:         r.inflight,
		Phase:            r.phase,
		Question:         r.question,
		Error:            r.errMsg,
		Paused:           r.paused,
		PauseReason:      r.pauseReason,
		RunCardMsgID:     r.cardMsgID,
		System1Decisions: r.system1Decisions,
		System2Decisions: r.system2Decisions,
		System1Usage:     r.system1Usage,
		System2Usage:     r.system2Usage,
		Nudged:           r.nudged,
	}
	r.mu.Unlock()

	if err := os.MkdirAll(s.cfg.StateDir, 0o755); err != nil {
		s.logger.Warn("me: the state directory could not be created", "err", err)
		return
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		s.logger.Warn("me: the run state could not be encoded", "run_id", r.id, "err", err)
		return
	}
	path := s.statePath(r.id)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		s.logger.Warn("me: the run state could not be written", "run_id", r.id, "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		s.logger.Warn("me: the run state could not be replaced", "run_id", r.id, "err", err)
	}
}

// removeState deletes the persisted state of a run that ended.
func (s *Service) removeState(runID string) {
	if s.cfg.StateDir == "" {
		return
	}
	if err := os.Remove(s.statePath(runID)); err != nil && !os.IsNotExist(err) {
		s.logger.Warn("me: the run state could not be removed", "run_id", runID, "err", err)
	}
}

// resumeRuns brings the runs a previous daemon left behind back to
// life.
//
// A run that was waiting for a reply keeps waiting. A run that was
// working is resumed: the command it was waiting for is resolved
// through the tracker, and a command whose execution is gone comes
// back as interrupted.
//
// A state file left by an older daemon names a chat that no longer
// exists: the run cannot go on there, so it is dropped.
func (s *Service) resumeRuns(ctx context.Context) {
	if s.cfg.StateDir == "" {
		return
	}
	entries, err := os.ReadDir(s.cfg.StateDir)
	if err != nil {
		if !os.IsNotExist(err) {
			s.logger.Warn("me: the state directory could not be read", "err", err)
		}
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.cfg.StateDir, e.Name()))
		if err != nil {
			s.logger.Warn("me: a run state could not be read", "file", e.Name(), "err", err)
			continue
		}
		var state persistedRun
		if err := json.Unmarshal(raw, &state); err != nil {
			s.logger.Warn("me: a run state could not be decoded", "file", e.Name(), "err", err)
			continue
		}
		if state.RunID == "" || state.Workflow == nil {
			continue
		}
		if state.ChatID == "" {
			// The state names a chat the daemon no longer has.
			// The run cannot go on: it is dropped.
			s.logger.Warn("me: a run from an older daemon was dropped",
				"run_id", state.RunID, "file", e.Name())
			s.removeState(state.RunID)
			continue
		}
		r := s.restoreRun(state)
		s.mu.Lock()
		s.runs[r.id] = r
		s.active[r.chatID] = r
		s.mu.Unlock()

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.drive(ctx, r)
		}()
	}
}

// restoreRun rebuilds a run from its persisted state.
func (s *Service) restoreRun(state persistedRun) *run {
	ctx, cancel := context.WithCancel(context.Background())
	r := &run{
		svc:              s,
		id:               state.RunID,
		chatID:           state.ChatID,
		cwd:              state.Cwd,
		owner:            state.Owner,
		argVals:          state.Args,
		workflow:         state.Workflow,
		policy:           wfe.NewPolicy(state.Workflow, state.History),
		state:            state.State,
		messages:         state.Messages,
		pendingCalls:     state.PendingCalls,
		ask:              state.Ask,
		inflight:         state.Inflight,
		phase:            state.Phase,
		question:         state.Question,
		errMsg:           state.Error,
		paused:           state.Paused,
		pauseReason:      state.PauseReason,
		cardMsgID:        state.RunCardMsgID,
		system1Decisions: state.System1Decisions,
		system2Decisions: state.System2Decisions,
		system1Usage:     state.System1Usage,
		system2Usage:     state.System2Usage,
		nudged:           state.Nudged,
		ctx:              ctx,
		cancel:           cancel,
		resume:           make(chan struct{}, 1),
		unpause:          make(chan struct{}, 1),
		done:             make(chan struct{}),
	}
	if r.state == nil {
		r.state = newRunState(r.id, state.Workflow.Name, r.cwd, time.Now())
	}
	if r.phase == "" {
		r.phase = PhaseRunning
	}
	r.policy.SetRounds(r.state.Rounds)
	if len(r.argVals) > 0 {
		names := make([]string, 0, len(r.argVals))
		for name := range r.argVals {
			names = append(names, name)
		}
		slices.Sort(names)
		r.argNames = names
	}
	return r
}

// pendingCallList returns the tool calls that still have to run.
func (r *run) pendingCallList() []pendingCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]pendingCall(nil), r.pendingCalls...)
}

// setPendingCalls records the tool calls that still have to run.
func (r *run) setPendingCalls(calls []pendingCall) {
	r.mu.Lock()
	r.pendingCalls = append([]pendingCall(nil), calls...)
	r.mu.Unlock()
}

// dropPendingCall removes the call that was just executed.
func (r *run) dropPendingCall() {
	r.mu.Lock()
	if len(r.pendingCalls) > 0 {
		r.pendingCalls = r.pendingCalls[1:]
	}
	r.mu.Unlock()
}

// inflightState returns the command the run is waiting for.
func (r *run) inflightState() *inflightState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inflight
}

// setInflight records the command the run is waiting for.
func (r *run) setInflight(st *inflightState) {
	r.mu.Lock()
	r.inflight = st
	r.mu.Unlock()
}

// clearWatch forgets the channel the running command was watched on.
//
// The next attempt watches the execution again.
func (r *run) clearWatch() {
	r.mu.Lock()
	r.watch = nil
	r.mu.Unlock()
}

// clearInflight forgets the command the run was waiting for.
//
// The watch channel goes with it: it belongs to that command, and a
// settled execution closes it.
func (r *run) clearInflight() {
	r.mu.Lock()
	r.inflight = nil
	r.watch = nil
	r.mu.Unlock()
}

// watchChannel returns the channel the running command is watched on.
func (r *run) watchChannel() <-chan observe.Execution {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.watch
}

// setWatchChannel keeps the channel the running command is watched on.
func (r *run) setWatchChannel(ch <-chan observe.Execution) {
	r.mu.Lock()
	r.watch = ch
	r.mu.Unlock()
}

// pendingAsk returns the question the run is waiting for.
func (r *run) pendingAsk() *askState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ask
}

// clearAsk forgets the question the run was waiting for.
func (r *run) clearAsk() {
	r.mu.Lock()
	r.ask = nil
	r.question = ""
	r.mu.Unlock()
}

// cardMessageID returns the id of the run's overview message.
func (r *run) cardMessageID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cardMsgID
}

// setCardMessageID records the id of the run's overview message.
func (r *run) setCardMessageID(id string) {
	r.mu.Lock()
	r.cardMsgID = id
	r.mu.Unlock()
}

// suspend stops the run without ending it.
//
// The daemon is going away, not the run: the state is kept, so the
// run resumes when the daemon comes back.
func (r *run) suspend() {
	r.mu.Lock()
	r.suspended = true
	r.mu.Unlock()
	r.cancel()
}

// isSuspended reports whether the daemon went away while the run was
// going.
func (r *run) isSuspended() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.suspended
}

// markFinished records that the model ended the run.
func (r *run) markFinished() {
	r.mu.Lock()
	r.finished = true
	r.mu.Unlock()
}

// isFinished reports whether the model ended the run.
func (r *run) isFinished() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finished
}

// setTranscript keeps the tail of the last action's transcript.
func (r *run) setTranscript(text string) {
	r.mu.Lock()
	r.lastTranscript = text
	r.mu.Unlock()
}

// setSummary records how the run ended.
func (r *run) setSummary(text string) {
	r.mu.Lock()
	r.summary = text
	r.mu.Unlock()
}

// setNote records what the chat model said last.
func (r *run) setNote(text string) {
	r.mu.Lock()
	r.lastNote = text
	r.mu.Unlock()
}

// askUser asks the operator a question and waits for the answer.
//
// The question is asked in the chat the run works in, as a reply to
// the run's overview: the answer comes back the way a person's reply
// comes back, and the run does not need a channel of its own.
//
// The run is persisted before the question is asked, and the wait
// starts after both. The order matters: the state that says "this
// run is waiting for an answer" has to be durable before the
// operator is told, so a daemon that restarts in the window between
// the two picks the run up as asking.
func (s *Service) askUser(ctx context.Context, r *run, question string, options []llm.Option) (string, error) {
	r.mu.Lock()
	waiting := r.ask != nil
	if !waiting {
		r.ask = &askState{Question: question, Options: options}
		r.question = question
		r.phase = PhaseAsking
	}
	r.mu.Unlock()

	if !waiting {
		s.persist(r)
		msgID, err := s.speakQuestion(ctx, r, question)
		if err != nil {
			s.logger.Warn("me: the question could not be asked", "run_id", r.id, "err", err)
		} else {
			r.mu.Lock()
			if r.ask != nil {
				r.ask.MsgID = msgID
			}
			r.mu.Unlock()
		}
		s.persist(r)
		s.updateCard(r)
	}
	reply, err := r.awaitReply(ctx)
	if err != nil {
		return "", err
	}
	r.clearAsk()
	r.setPhase(PhaseRunning)
	return reply, nil
}
