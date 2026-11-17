package me

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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

// actionTranscript is the transcript of one action of a run.
//
// The evidence that explains a failure is often in the action before
// the one that failed, so a run keeps the last two.
type actionTranscript struct {
	// Command is the action the transcript belongs to.
	Command string `json:"command"`
	// Text is the transcript itself.
	Text string `json:"text"`
}

// maxActionTranscripts is how many action transcripts a run keeps.
//
// The one before the last one is what explains the last one: the
// agent that did the work said why it could not.
const maxActionTranscripts = 2

// The caps of the run's own words.
//
// The card is read at a glance: it carries the last few replies, and
// every one of them is short.
const (
	// maxCardReplies is how many replies the run keeps.
	maxCardReplies = 3
	// maxReplyChars is how many characters a reply has.
	maxReplyChars = 400
	// maxReplyLines is how many lines a reply has.
	maxReplyLines = 6
)

// replyBlock is one turn of the chat model's message.
//
// The block holds the text already cut: the card renders it as it
// is, and nothing has to be cut again.
type replyBlock struct {
	// Text is what the model said.
	Text string `json:"text"`
	// At is when the model said it.
	At time.Time `json:"at"`
}

// wakeRecord is one decision a model made after the run woke it up.
type wakeRecord struct {
	// Who decided: the decision model or the chat model.
	Who string `json:"who"`
	// What the model decided.
	What string `json:"what"`
	// Why the run woke the model: the judgment the workflow could not
	// decide.
	Why string `json:"why,omitempty"`
	// Round is the round of the job the run was on.
	Round int `json:"round,omitempty"`
	// At is when the decision was made.
	At time.Time `json:"at"`
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
	// registry holds the nightme commands the chat knows, as they came
	// from the command registry.
	registry []CommandInfo
	// commands are the nightme commands the run may issue outside the
	// workflow's steps.
	commands []CommandInfo
	// agentName is the agent that thinks for the owner. Empty when
	// there is none: the run then has no agent tool.
	agentName string
	// brain is the session that thinks for the owner. It is opened
	// when the model asks for it and closed when the round it belongs
	// to is over.
	brain *roundBrain

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
	// cardJob is the job the card records; cardRound is the round of
	// that job the card belongs to. An empty cardJob means the card
	// has no iteration yet: it is the one the run started with.
	cardJob   string
	cardRound int
	// cards holds the message id of the card of every iteration the
	// run has entered. A run that comes back to an iteration goes on
	// with the card that iteration has.
	cards map[iteration]string

	// system1Decisions and system2Decisions count what each
	// capability decided.
	system1Decisions int
	system2Decisions int
	// system1Usage and system2Usage count the tokens each
	// capability spent.
	system1Usage llm.Usage
	system2Usage llm.Usage
	// situationStale records that the workflow moved on since the
	// model last read a situation report.
	situationStale bool
	// situationIdx is where the situation report sits in the
	// conversation. The report is rendered again on every turn, and
	// only the latest one is kept: the conversation would grow with
	// every turn of the agent.
	situationIdx int
	// transcripts holds the transcripts of the last actions, oldest
	// first. The evidence that explains a failure is often in the
	// action before the one that failed.
	transcripts []actionTranscript
	// waiting is how long the run is waiting, when it waits inside
	// the agent's turn.
	waiting string
	// system1Model is the decision model that answered last.
	system1Model string
	// judgment is why the workflow cannot decide on its own.
	judgment string
	// judgmentStep is the step the judgment is about: the step that
	// failed, the step that is still running. Empty when the judgment
	// names no step.
	judgmentStep string
	// base is the base branch of the workspace, and baseDir the
	// directory it was read for.
	//
	// Resolving the base branch may reach the remote: the situation
	// report is rendered on every turn of the agent, so the branch is
	// read once per workspace.
	base    string
	baseDir string
	// providerWait is how long the run waits before it asks the chat
	// model again. It grows with every failed turn and starts over
	// when the model answers.
	//
	// It is not persisted: a daemon that comes back starts the wait
	// over. The chances the agent spent are what the history keeps.
	providerWait time.Duration
	// lastDecision names who decided last: the workflow, the
	// decision model or the chat model.
	lastDecision string
	// lastReason says why that decision was made.
	lastReason string
	// thinkCount counts the turns the chat model took, and toolCount
	// the tool calls the run made.
	//
	// The counts are taken before the run's events leave it: a chat
	// that hides them does not change what the run did.
	thinkCount int
	toolCount  int
	// lastBeatAt is when the run last did something. The card shows
	// it as the time of day.
	lastBeatAt time.Time
	// replies holds the run's own words, oldest first. The card
	// shows the last few of them.
	replies []replyBlock
	// cardRefreshedAt is when the card was refreshed last. The
	// snapshots of the command the run waits for refresh the card,
	// and they are throttled.
	cardRefreshedAt time.Time
	// summary is how the run ended.
	summary string
	// finished records that the model called the finish tool.
	finished bool
	// suspended records that the daemon went away while the run was
	// going: the run keeps its state and resumes when it comes back.
	suspended bool
	// wakes holds the decisions the models made at the judgment points.
	wakes []wakeRecord
	// stoppedByOperator reports that the operator stopped the run.
	stoppedByOperator bool

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
	commands := usableCommands(req.Commands)
	policy := svc.newPolicy(wf, nil, commands)
	return &run{
		svc:          svc,
		id:           id,
		chatID:       req.ChatID,
		cwd:          req.Cwd,
		owner:        req.Owner,
		argNames:     names,
		argVals:      args,
		workflow:     wf,
		policy:       policy,
		state:        newRunState(id, wf.Name, req.Cwd, now),
		registry:     req.Commands,
		commands:     commands,
		agentName:    svc.agentName(req.Agent),
		phase:        PhaseRunning,
		situationIdx: -1,
		ctx:          ctx,
		cancel:       cancel,
		resume:       make(chan struct{}, 1),
		unpause:      make(chan struct{}, 1),
		done:         make(chan struct{}),
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

// pauseNow stops the run and waits for the operator.
//
// The run keeps its state and its place: the operator hands the
// control back, and the run goes on where it stopped.
func (r *run) pauseNow(reason string) {
	r.mu.Lock()
	r.pauseRequested = false
	r.paused = true
	r.pauseReason = reason
	r.phase = PhasePaused
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
//
// The step the run was on gets a fresh budget: the operator took the
// chat back, so the chances the agent spent there are spent no more.
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

	// The operator took over: the step the run was on starts over.
	if stepID := r.budgetStep(); stepID != "" {
		r.record(wfe.ActionKindReset, "", stepID, wfe.Outcome{})
	}

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

// appendSituation adds the situation report to the conversation.
//
// Only the latest report is kept. The report is rendered again on
// every turn of the agent, and a conversation that holds one report
// per turn grows past what the provider accepts.
func (r *run) appendSituation(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.situationIdx >= 0 && r.situationIdx < len(r.messages) {
		// A fresh slice: the conversation is copied elsewhere, and the
		// copy shares the array the messages live in.
		msgs := make([]llm.Message, 0, len(r.messages))
		msgs = append(msgs, r.messages[:r.situationIdx]...)
		msgs = append(msgs, r.messages[r.situationIdx+1:]...)
		r.messages = msgs
	}
	r.messages = append(r.messages, situationMessage(text))
	r.situationIdx = len(r.messages) - 1
}

// situationIndex returns where the last situation report sits in a
// conversation, or -1 when it holds none.
//
// The report is the user turn that opens with the reason the run
// needs the model.
func situationIndex(msgs []llm.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != llm.RoleUser {
			continue
		}
		if strings.HasPrefix(msgs[i].Text(), situationMark) {
			return i
		}
	}
	return -1
}

// markSituation records that the workflow moved on by itself.
func (r *run) markSituation() {
	r.mu.Lock()
	r.situationStale = true
	r.mu.Unlock()
}

// takeSituation reports whether a new situation report is due, and
// clears the mark.
func (r *run) takeSituation() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	stale := r.situationStale
	r.situationStale = false
	return stale
}

// conversation returns a copy of the conversation.
func (r *run) conversation() []llm.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]llm.Message(nil), r.messages...)
}

// checkRun asks the policy whether a command may run now.
//
// It reports the step the command counts against.
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

// stepByID returns the workflow's step with the given id.
func (r *run) stepByID(stepID string) (wfe.Step, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := r.stepIndex(stepID)
	if idx < 0 {
		return wfe.Step{}, false
	}
	steps := r.workflow.Steps()
	return steps[idx], true
}

// runnableStep reports whether the step may run now: it is the ready
// step, or it already ran in this round.
//
// A step further down the line is not runnable: the steps of a job
// run in order, and a skipped one would come back out of order.
func (r *run) runnableStep(stepID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	ready, err := wfe.ReadySet(r.state, r.workflow)
	if err == nil {
		for _, step := range ready {
			if step.ID == stepID {
				return true
			}
		}
	}
	for _, st := range r.state.Steps {
		if st.ID != stepID || st.Skipped {
			continue
		}
		if st.Round == r.state.RoundOf(st.Job) {
			return true
		}
	}
	return false
}

// commandFor returns the command a tool name stands for.
func (r *run) commandFor(tool string) (CommandInfo, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.commands {
		if toolName(c.Name) == tool {
			return c, true
		}
	}
	return CommandInfo{}, false
}

// checkCommandText refuses a text that names a nightme command.
//
// The commands have tools of their own: a command in a free-form text
// would run as a turn of the chat, not as a command of the run.
func (r *run) checkCommandText(text string) error {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return nil
	}
	first := fields[0]
	if !strings.HasPrefix(first, "/") {
		return nil
	}
	name := strings.TrimPrefix(first, "/")
	for _, c := range r.registry {
		if c.Name != name {
			continue
		}
		tool := toolName(name)
		for _, usable := range r.commands {
			if usable.Name == name {
				return fmt.Errorf("%q is a nightme command: run it with the %s tool", first, tool)
			}
		}
		return fmt.Errorf("%q is a nightme command, and the run may not use it", first)
	}
	return nil
}

// workflowTitle returns the workflow's title.
func (r *run) workflowTitle() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.workflow.Title
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

// setJudgmentStep records the step the judgment is about.
func (r *run) setJudgmentStep(stepID string) {
	r.mu.Lock()
	r.judgmentStep = stepID
	r.mu.Unlock()
}

// newPolicy builds the policy of a run.
//
// The budget is the agent's: one number for every step and every
// round. It is set here, so a run cannot go without one.
func (s *Service) newPolicy(wf *wfe.Workflow, history []wfe.ActionRecord, commands []CommandInfo) *wfe.Policy {
	p := wfe.NewPolicy(wf, history)
	p.Budget = s.cfg.MaxJudgments
	p.Commands = commandNames(commands)
	return p
}

// baseBranchOf returns the base branch of a workspace.
//
// The resolution may reach the remote, so it is done once per
// workspace: the situation report is rendered on every turn of the
// agent, and the base branch of a workspace does not change.
func (r *run) baseBranchOf(ctx context.Context, s *Service, dir string) string {
	r.mu.Lock()
	cached, cachedDir := r.base, r.baseDir
	r.mu.Unlock()
	if cachedDir == dir {
		return cached
	}
	base := s.baseBranch(ctx, dir)
	r.mu.Lock()
	r.base, r.baseDir = base, dir
	r.mu.Unlock()
	return base
}

// budgetStep returns the step whose budget bounds the run now.
//
// The judgment names the step it is about. A judgment that names no
// step — the plan forks, a job starts a new round — belongs to the
// last step that ran in the round. A run that has done no work yet
// belongs to the step the plan may run now, and a run whose plan is
// spent to the last step of the round.
//
// Every judgment ends up at a step: a turn the agent spent without
// acting has to cost a chance somewhere, or nothing bounds the run.
func (r *run) budgetStep() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.budgetStepLocked()
}

// budgetStepLocked returns the step whose budget bounds the run now.
// The caller holds the lock.
func (r *run) budgetStepLocked() string {
	if r.judgmentStep != "" {
		return r.judgmentStep
	}
	if id := r.policy.CurrentStepID(); id != "" {
		return id
	}
	if ids := r.readyStepIDsLocked(); len(ids) > 0 {
		return ids[0]
	}
	return r.lastStepLocked()
}

// lastStepLocked returns the id of the last step the run state holds.
// The caller holds the lock.
func (r *run) lastStepLocked() string {
	for i := len(r.state.Steps) - 1; i >= 0; i-- {
		if r.state.Steps[i].ID != "" {
			return r.state.Steps[i].ID
		}
	}
	return ""
}

// stepUsage returns the chances the step spent in the current round
// and the budget it has.
func (r *run) stepUsage(stepID string) (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := r.stepIndex(stepID)
	if idx < 0 {
		return 0, 0
	}
	return r.policy.StepUsage(idx), r.policy.Budget
}

// nextProviderWait returns how long the run waits before it asks the
// chat model again, and grows the wait for the next time.
func (r *run) nextProviderWait() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.providerWait == 0 {
		r.providerWait = firstProviderWait
	} else {
		r.providerWait *= 2
		if r.providerWait > maxProviderWait {
			r.providerWait = maxProviderWait
		}
	}
	return r.providerWait
}

// resetProviderWait forgets the wait: the chat model answered.
func (r *run) resetProviderWait() {
	r.mu.Lock()
	r.providerWait = 0
	r.mu.Unlock()
}

// setDecision records who decided last and why.
func (r *run) setDecision(who, reason string) {
	r.mu.Lock()
	r.lastDecision = who
	r.lastReason = reason
	r.mu.Unlock()
}

// countThink records a turn of the chat model.
func (r *run) countThink(now time.Time) {
	r.mu.Lock()
	r.thinkCount++
	r.lastBeatAt = now
	r.mu.Unlock()
}

// countTool records a tool call the run made.
func (r *run) countTool(now time.Time) {
	r.mu.Lock()
	r.toolCount++
	r.lastBeatAt = now
	r.mu.Unlock()
}

// beat records that the run did something.
//
// The command the run waits for moved: the run is doing something,
// and the card says when it did it for the last time.
func (r *run) beat(now time.Time) {
	r.mu.Lock()
	r.lastBeatAt = now
	r.mu.Unlock()
}

// addReply keeps what the chat model said.
//
// The run's own words are on its card: the run keeps the last few of
// them and forgets the rest.
func (r *run) addReply(block replyBlock) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replies = append(r.replies, block)
	if len(r.replies) > maxCardReplies {
		r.replies = r.replies[len(r.replies)-maxCardReplies:]
	}
}

// replyList returns the run's own words, oldest first.
func (r *run) replyList() []replyBlock {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.replyListLocked()
}

// replyListLocked returns the run's own words. The caller holds the
// lock.
func (r *run) replyListLocked() []replyBlock {
	return append([]replyBlock(nil), r.replies...)
}

// takeCardRefresh reports whether the card may be refreshed now, and
// remembers when it was.
//
// The throttle keeps a run from writing to the chat on every snapshot
// of the command it waits for.
func (r *run) takeCardRefresh(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.cardRefreshedAt.IsZero() && now.Sub(r.cardRefreshedAt) < cardRefreshInterval {
		return false
	}
	r.cardRefreshedAt = now
	return true
}

// recordStep records the outcome of a step in the run's progress.
//
// The engine owns the state: the step that ran and its outcome go
// in together. The run's situation moved on, so the model reads it
// again before it decides.
func (r *run) recordStep(job string, step wfe.Step, out wfe.Outcome) {
	r.mu.Lock()
	wfe.RecordStep(r.state, job, step, out, false)
	r.mu.Unlock()
	r.markSituation()
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

// stepCommands returns the step messages the policy accepts now.
func (r *run) stepCommands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.policy.StepCommands()
}

// consecutiveStalls returns the number of turns the agent produced no
// tool call in.
func (r *run) consecutiveStalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.policy.ConsecutiveStalls()
}

// readyStepIDs returns the ids of the steps the plan may run now.
func (r *run) readyStepIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readyStepIDsLocked()
}

// readyStepIDsLocked returns the ids of the steps the plan may run
// now. The caller holds the lock.
func (r *run) readyStepIDsLocked() []string {
	ready, err := wfe.ReadySet(r.state, r.workflow)
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(ready))
	for _, step := range ready {
		ids = append(ids, step.ID)
	}
	return ids
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
// budgets count the actions of the current round only.
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
	return r.state.RoundOf(job)
}

// exprCtx renders the run's progress as the context an expression
// reads.
func (r *run) exprCtx() wfe.ExprCtx {
	r.mu.Lock()
	defer r.mu.Unlock()
	return wfe.ExprContext(r.state)
}

// lastVerdict returns the last action that produced a verdict.
//
// A stall and a reset produce none: they are the run's own
// bookkeeping, not the work's.
func (r *run) lastVerdict() (wfe.ActionRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.policy.History) - 1; i >= 0; i-- {
		rec := r.policy.History[i]
		if rec.Kind == wfe.ActionKindStall || rec.Kind == wfe.ActionKindReset {
			continue
		}
		return rec, true
	}
	return wfe.ActionRecord{}, false
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

// workCwd returns the directory the run last knew.
//
// The chat moves when a command moves it, so this is the fallback for
// the moments the daemon cannot tell where the chat stands.
func (r *run) workCwd() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cwd
}

// setWorkCwd records the directory the run works in: the run's own
// field, the workspace of its state and the environment its steps
// run with.
//
// It reports whether the directory changed.
//
// The environment is replaced rather than written into: a map that
// was handed out to a step is read outside the lock.
func (r *run) setWorkCwd(dir string) bool {
	if dir == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := r.cwd != dir
	r.cwd = dir
	if r.state == nil {
		return changed
	}
	r.state.Workspace = dir
	env := maps.Clone(r.state.Env)
	if env == nil {
		env = map[string]string{}
	}
	env["WORKSPACE_CWD"] = dir
	r.state.Env = env
	return changed
}

// outcomeLine renders an outcome as one human-readable line.
func outcomeLine(o messages.Outcome) string {
	if o.Summary != "" {
		return o.Summary
	}
	return string(o.Status)
}

// cardEntry is the card of one iteration of a run.
type cardEntry struct {
	// Job is the job the card records.
	Job string `json:"job"`
	// Round is the round of that job the card belongs to.
	Round int `json:"round"`
	// ID is the id of the message the card became.
	ID string `json:"id"`
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
	Transcripts      []actionTranscript `json:"transcripts,omitempty"`
	Messages         []llm.Message      `json:"messages"`
	PendingCalls     []pendingCall      `json:"pending_calls"`
	Ask              *askState          `json:"ask,omitempty"`
	Inflight         *inflightState     `json:"inflight,omitempty"`
	Phase            Phase              `json:"phase"`
	JudgmentStep     string             `json:"judgment_step,omitempty"`
	Question         string             `json:"question"`
	Error            string             `json:"error"`
	Paused           bool               `json:"paused"`
	PauseReason      string             `json:"pause_reason,omitempty"`
	RunCardMsgID     string             `json:"run_card_msg_id,omitempty"`
	CardJob          string             `json:"card_job"`
	CardRound        int                `json:"card_round"`
	Cards            []cardEntry        `json:"cards,omitempty"`
	System1Decisions int                `json:"system1_decisions"`
	System2Decisions int                `json:"system2_decisions"`
	System1Usage     llm.Usage          `json:"system1_usage"`
	System2Usage     llm.Usage          `json:"system2_usage"`
	Commands         []CommandInfo      `json:"commands,omitempty"`
	Agent            string             `json:"agent,omitempty"`
	Wakes            []wakeRecord       `json:"wakes,omitempty"`
	ThinkCount       int                `json:"think_count"`
	ToolCount        int                `json:"tool_count"`
	LastBeatAt       time.Time          `json:"last_beat_at"`
	Replies          []replyBlock       `json:"replies,omitempty"`
}

// persist writes the run's state to disk.
//
// The state is written after every tool result, so a daemon that dies
// mid-run comes back with the conversation and the policy intact.
func (s *Service) persist(r *run) {
	if s.cfg.StateDir == "" {
		return
	}
	// The cards are read before the lock: the run holds it while it
	// writes the state.
	cards := r.cardEntries()
	r.mu.Lock()
	state := persistedRun{
		RunID:            r.id,
		ChatID:           r.chatID,
		Cwd:              r.cwd,
		Owner:            r.owner,
		Args:             r.argVals,
		Workflow:         r.workflow,
		State:            snapshotState(r.state),
		History:          append([]wfe.ActionRecord(nil), r.policy.History...),
		Transcripts:      append([]actionTranscript(nil), r.transcripts...),
		Messages:         append([]llm.Message(nil), r.messages...),
		PendingCalls:     append([]pendingCall(nil), r.pendingCalls...),
		Ask:              r.ask,
		Inflight:         r.inflight,
		Phase:            r.phase,
		JudgmentStep:     r.judgmentStep,
		Question:         r.question,
		Error:            r.errMsg,
		Paused:           r.paused,
		PauseReason:      r.pauseReason,
		RunCardMsgID:     r.cardMsgID,
		CardJob:          r.cardJob,
		CardRound:        r.cardRound,
		Cards:            cards,
		System1Decisions: r.system1Decisions,
		System2Decisions: r.system2Decisions,
		System1Usage:     r.system1Usage,
		System2Usage:     r.system2Usage,
		Commands:         r.registry,
		Agent:            r.agentName,
		Wakes:            append([]wakeRecord(nil), r.wakes...),
		ThinkCount:       r.thinkCount,
		ToolCount:        r.toolCount,
		LastBeatAt:       r.lastBeatAt,
		Replies:          append([]replyBlock(nil), r.replies...),
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

// snapshotState copies the progress of a run.
//
// The state is written to disk outside the run's lock, and the run
// moves on while the file is written: the state is copied first.
func snapshotState(state *wfe.RunState) *wfe.RunState {
	if state == nil {
		return nil
	}
	out := *state
	out.Env = maps.Clone(state.Env)
	out.Rounds = maps.Clone(state.Rounds)
	if state.EndedRounds != nil {
		out.EndedRounds = make(map[string]wfe.EndedRound, len(state.EndedRounds))
		for job, ended := range state.EndedRounds {
			out.EndedRounds[job] = ended
		}
	}
	out.Steps = make([]wfe.StepState, len(state.Steps))
	for i, st := range state.Steps {
		out.Steps[i] = st
		out.Steps[i].Outcome.Data = maps.Clone(st.Outcome.Data)
	}
	return &out
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
	commands := usableCommands(state.Commands)
	policy := s.newPolicy(state.Workflow, state.History, commands)
	r := &run{
		svc:              s,
		id:               state.RunID,
		chatID:           state.ChatID,
		cwd:              state.Cwd,
		owner:            state.Owner,
		argVals:          state.Args,
		workflow:         state.Workflow,
		policy:           policy,
		state:            state.State,
		transcripts:      state.Transcripts,
		messages:         state.Messages,
		pendingCalls:     state.PendingCalls,
		ask:              state.Ask,
		inflight:         state.Inflight,
		phase:            state.Phase,
		judgmentStep:     state.JudgmentStep,
		question:         state.Question,
		errMsg:           state.Error,
		paused:           state.Paused,
		pauseReason:      state.PauseReason,
		cardMsgID:        state.RunCardMsgID,
		cardJob:          state.CardJob,
		cardRound:        state.CardRound,
		system1Decisions: state.System1Decisions,
		system2Decisions: state.System2Decisions,
		system1Usage:     state.System1Usage,
		system2Usage:     state.System2Usage,
		registry:         state.Commands,
		commands:         commands,
		agentName:        s.agentName(state.Agent),
		wakes:            state.Wakes,
		thinkCount:       state.ThinkCount,
		toolCount:        state.ToolCount,
		lastBeatAt:       state.LastBeatAt,
		replies:          state.Replies,
		situationIdx:     situationIndex(state.Messages),
		ctx:              ctx,
		cancel:           cancel,
		resume:           make(chan struct{}, 1),
		unpause:          make(chan struct{}, 1),
		done:             make(chan struct{}),
	}
	if r.state == nil {
		r.state = newRunState(r.id, state.Workflow.Name, state.Cwd, time.Now())
	}
	if r.phase == "" {
		r.phase = PhaseRunning
	}
	if oldConversation(r.messages) {
		// The conversation was written by an older daemon, in a shape
		// the tools no longer have. The run starts over from the
		// situation report instead. The pending calls go with it: a
		// call the model never saw answered would come back as a
		// result without a question.
		r.messages = nil
		r.pendingCalls = nil
	}
	r.cards = map[iteration]string{}
	for _, entry := range state.Cards {
		r.cards[iteration{job: entry.Job, round: entry.Round}] = entry.ID
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

// oldConversation reports whether the conversation was written in a
// shape the tools no longer have.
//
// The old run_step took a command; the new one takes a step's id. A
// conversation that carries the old call cannot be read by the model
// the way it was written, so it is dropped.
func oldConversation(msgs []llm.Message) bool {
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type != llm.PartToolCall || p.ToolName != toolRunStep {
				continue
			}
			var args map[string]any
			if err := json.Unmarshal(p.Args, &args); err != nil {
				continue
			}
			if _, ok := args["command"]; ok {
				return true
			}
		}
	}
	return false
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

// setCard makes the card of an iteration the one the run updates.
func (r *run) setCard(id string, it iteration) {
	r.mu.Lock()
	r.cardMsgID = id
	r.cardJob = it.job
	r.cardRound = it.round
	r.mu.Unlock()
}

// cardIteration returns the iteration the run's card records.
func (r *run) cardIteration() iteration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return iteration{job: r.cardJob, round: r.cardRound}
}

// cardOf returns the message id of the card of an iteration.
func (r *run) cardOf(it iteration) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.cards[it]
	return id, ok
}

// rememberCard records the card of an iteration.
func (r *run) rememberCard(it iteration, id string) {
	r.mu.Lock()
	if r.cards == nil {
		r.cards = map[iteration]string{}
	}
	r.cards[it] = id
	r.mu.Unlock()
}

// cardEntries returns the card of every iteration the run has entered.
func (r *run) cardEntries() []cardEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]cardEntry, 0, len(r.cards))
	for it, id := range r.cards {
		out = append(out, cardEntry{Job: it.job, Round: it.round, ID: id})
	}
	// The map has no order of its own: the file is written in the
	// order of the iterations.
	slices.SortFunc(out, func(a, b cardEntry) int {
		if a.Job != b.Job {
			return strings.Compare(a.Job, b.Job)
		}
		return a.Round - b.Round
	})
	return out
}

// iterationOfStep returns the iteration a step of the workflow runs
// in: the job it belongs to and the round of that job.
func (r *run) iterationOfStep(idx int) iteration {
	step, ok := r.stepAt(idx)
	if !ok {
		return iteration{}
	}
	job := wfe.JobOf(r.workflow, step.ID)
	if job == "" {
		return iteration{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return iteration{job: job, round: r.state.RoundOf(job)}
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

// recordWake appends a decision to the wake log.
func (r *run) recordWake(rec wakeRecord) {
	r.mu.Lock()
	r.wakes = append(r.wakes, rec)
	r.mu.Unlock()
}

// wakeRecords returns the decisions the models made, oldest first.
func (r *run) wakeRecords() []wakeRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]wakeRecord(nil), r.wakes...)
}

// markStoppedByOperator records that the operator stopped the run.
func (r *run) markStoppedByOperator() {
	r.mu.Lock()
	r.stoppedByOperator = true
	r.mu.Unlock()
}

// wasStoppedByOperator reports that the operator stopped the run.
func (r *run) wasStoppedByOperator() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stoppedByOperator
}

// currentRound returns the round of the job the run is on.
//
// The run's iteration is the job it entered and the round of that job;
// a run that has entered no job yet is in the first round.
func (r *run) currentRound() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cardJob == "" || r.cardRound <= 0 {
		return 1
	}
	return r.cardRound
}

// setTranscript keeps the transcript of an action.
//
// The run keeps the last two: the evidence that explains a failure is
// often in the action before the one that failed.
func (r *run) setTranscript(command, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	r.mu.Lock()
	r.transcripts = append(r.transcripts, actionTranscript{Command: command, Text: text})
	if len(r.transcripts) > maxActionTranscripts {
		r.transcripts = r.transcripts[len(r.transcripts)-maxActionTranscripts:]
	}
	r.mu.Unlock()
}

// actionTranscripts returns the transcripts of the last actions,
// oldest first.
func (r *run) actionTranscripts() []actionTranscript {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]actionTranscript(nil), r.transcripts...)
}

// lastTranscriptText returns the transcript of the last action.
func (r *run) lastTranscriptText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.transcripts) == 0 {
		return ""
	}
	return r.transcripts[len(r.transcripts)-1].Text
}

// setWaiting records how long the run is waiting.
func (r *run) setWaiting(text string) {
	r.mu.Lock()
	r.waiting = text
	r.mu.Unlock()
}

// setSummary records how the run ended.
func (r *run) setSummary(text string) {
	r.mu.Lock()
	r.summary = text
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
	// The card does not offer the answer any more: the run is not
	// waiting for one. A card that is left behind when the run moves
	// on must not keep the choices of a question that is answered.
	s.updateCard(r)
	return reply, nil
}
