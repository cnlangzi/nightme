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

	"github.com/cnlangzi/nightme/internal/channel/loopback"
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
	// Question is the text the observer was asked.
	Question string `json:"question"`
	// Options are the choices the user may pick, when the question
	// is a choice between them.
	Options []llm.Option `json:"options,omitempty"`
}

// inflightState is the command a run is waiting for.
type inflightState struct {
	// MsgID is the loopback message the command became.
	MsgID string `json:"msg_id"`
	// Command is the text that was sent.
	Command string `json:"command"`
	// Instruct is true when the command is a free-form instruction
	// rather than a playbook step.
	Instruct bool `json:"instruct"`
}

// run is one playbook run.
type run struct {
	svc *Service

	id             string
	observerChatID string
	workChatID     string
	cwd            string
	agent          string

	// playbook is the bound playbook the run works through.
	playbook *wfe.Playbook
	// policy bounds what the run may do.
	policy *wfe.Policy

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
	// done is closed when the run's goroutine is over.
	done chan struct{}
	// reply is the text the user answered with.
	reply string
	// watch is the channel the running command is watched on. It is
	// kept across the calls that resume a command, so the loopback
	// channel never grows a watcher nobody reads.
	watch <-chan loopback.Execution
}

// newRun builds a run for a bound playbook.
func newRun(svc *Service, req Request, pb *wfe.Playbook) *run {
	ctx, cancel := context.WithCancel(context.Background())
	id := fmt.Sprintf("%s-%d", req.Name, svc.now().UnixNano())
	return &run{
		svc:            svc,
		id:             id,
		observerChatID: req.ObserverChatID,
		workChatID:     loopback.ChatIDPrefix + id,
		cwd:            req.Cwd,
		agent:          req.Agent,
		playbook:       pb,
		policy:         wfe.NewPolicy(pb, nil),
		phase:          PhaseRunning,
		ctx:            ctx,
		cancel:         cancel,
		resume:         make(chan struct{}, 1),
		done:           make(chan struct{}),
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

// deliver hands the user's reply to the waiting run.
func (r *run) deliver(text string) error {
	r.mu.Lock()
	if r.ask == nil {
		r.mu.Unlock()
		return errors.New("this run is not waiting for a reply")
	}
	r.reply = text
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

// awaitReply waits for the user's reply.
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

// allowedCommands returns the commands the policy accepts now.
func (r *run) allowedCommands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.policy.AllowedCommands()
}

// renderPolicy returns the playbook as the model reads it.
func (r *run) renderPolicy() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.policy.Render()
}

// record appends an action to the policy's history.
func (r *run) record(kind wfe.ActionKind, command string, stepIndex int, outcome wfe.Outcome) {
	r.mu.Lock()
	r.policy.Record(wfe.ActionRecord{
		Kind:      kind,
		Command:   command,
		StepIndex: stepIndex,
		Outcome:   outcome,
		At:        r.svc.now(),
	})
	r.mu.Unlock()
}

// outcomeLine renders an outcome as one human-readable line.
func outcomeLine(o messages.Outcome) string {
	if o.Summary != "" {
		return o.Summary
	}
	return string(o.Status)
}

// persistedRun is the on-disk shape of a run.
type persistedRun struct {
	RunID            string             `json:"run_id"`
	ObserverChatID   string             `json:"observer_chat_id"`
	WorkChatID       string             `json:"work_chat_id"`
	Cwd              string             `json:"cwd"`
	Agent            string             `json:"agent"`
	Playbook         *wfe.Playbook      `json:"playbook"`
	History          []wfe.ActionRecord `json:"history"`
	Messages         []llm.Message      `json:"messages"`
	PendingCalls     []pendingCall      `json:"pending_calls"`
	Ask              *askState          `json:"ask,omitempty"`
	Inflight         *inflightState     `json:"inflight,omitempty"`
	Phase            Phase              `json:"phase"`
	Question         string             `json:"question"`
	Error            string             `json:"error"`
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
		ObserverChatID:   r.observerChatID,
		WorkChatID:       r.workChatID,
		Cwd:              r.cwd,
		Agent:            r.agent,
		Playbook:         r.playbook,
		History:          append([]wfe.ActionRecord(nil), r.policy.History...),
		Messages:         append([]llm.Message(nil), r.messages...),
		PendingCalls:     append([]pendingCall(nil), r.pendingCalls...),
		Ask:              r.ask,
		Inflight:         r.inflight,
		Phase:            r.phase,
		Question:         r.question,
		Error:            r.errMsg,
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
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
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
// through the loopback, and a command whose execution is gone comes
// back as interrupted.
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
		if state.RunID == "" || state.Playbook == nil {
			continue
		}
		r := s.restoreRun(state)
		s.mu.Lock()
		s.runs[r.id] = r
		s.active[r.observerChatID] = r
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
		observerChatID:   state.ObserverChatID,
		workChatID:       state.WorkChatID,
		cwd:              state.Cwd,
		agent:            state.Agent,
		playbook:         state.Playbook,
		policy:           wfe.NewPolicy(state.Playbook, state.History),
		messages:         state.Messages,
		pendingCalls:     state.PendingCalls,
		ask:              state.Ask,
		inflight:         state.Inflight,
		phase:            state.Phase,
		question:         state.Question,
		errMsg:           state.Error,
		system1Decisions: state.System1Decisions,
		system2Decisions: state.System2Decisions,
		system1Usage:     state.System1Usage,
		system2Usage:     state.System2Usage,
		nudged:           state.Nudged,
		ctx:              ctx,
		cancel:           cancel,
		resume:           make(chan struct{}, 1),
		done:             make(chan struct{}),
	}
	if r.phase == "" {
		r.phase = PhaseRunning
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
func (r *run) watchChannel() <-chan loopback.Execution {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.watch
}

// setWatchChannel keeps the channel the running command is watched on.
func (r *run) setWatchChannel(ch <-chan loopback.Execution) {
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

// askUser asks the user a question and waits for the answer.
//
// The question is posted to the observer chat and the run is
// persisted before the wait, so a daemon that restarts keeps the
// question open.
func (s *Service) askUser(ctx context.Context, r *run, question string, options []llm.Option) (string, error) {
	r.mu.Lock()
	waiting := r.ask != nil
	if !waiting {
		r.ask = &askState{Question: question, Options: options}
		r.question = question
		r.phase = PhasePaused
	}
	r.mu.Unlock()

	if !waiting {
		s.notify(r, "❓ "+question)
		s.persist(r)
	}
	reply, err := r.awaitReply(ctx)
	if err != nil {
		return "", err
	}
	r.clearAsk()
	r.setPhase(PhaseRunning)
	return reply, nil
}
