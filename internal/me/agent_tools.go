package me

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/channel/loopback"
	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// The tools the chat model may call.
const (
	// toolRunStep runs one of the workflow's steps.
	toolRunStep = "run_step"
	// toolInstruct sends a free-form prompt to the work agent.
	toolInstruct = "instruct"
	// toolAnswerChoice answers a pending prompt of the work chat.
	toolAnswerChoice = "answer_choice"
	// toolAskHuman asks the user.
	toolAskHuman = "ask_human"
	// toolRepoStatus reads the repository facts.
	toolRepoStatus = "repo_status"
	// toolFinish ends the run.
	toolFinish = "finish"
)

// toolError is a failure the model can correct.
//
// It comes back as a tool error result, so the model sees what went
// wrong and tries again. It does not end the run.
type toolError struct{ msg string }

func (e *toolError) Error() string { return e.msg }

// toolErrorf builds a tool error.
func toolErrorf(format string, args ...any) error {
	return &toolError{msg: fmt.Sprintf(format, args...)}
}

// stepResult is the result of a command the run sent to the work
// chat.
type stepResult struct {
	// Status is the machine-readable verdict.
	Status string `json:"status"`
	// Summary is the one-line restatement of what happened.
	Summary string `json:"summary"`
	// Data carries the facts the command reported.
	Data map[string]string `json:"data,omitempty"`
	// Transcript is the tail of what the work agent said.
	Transcript string `json:"transcript,omitempty"`
}

// toolCatalogue returns the tools the chat model may call.
func toolCatalogue() []llm.Tool {
	return []llm.Tool{
		{
			Name:        toolRunStep,
			Description: "Run one of the workflow's steps in the work chat. The command must be a step of the workflow or one its step allows.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"the command to run"}},"required":["command"],"additionalProperties":false}`),
		},
		{
			Name:        toolInstruct,
			Description: "Send a free-form instruction to the work agent, for the cases the workflow's commands do not cover.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"the instruction"}},"required":["text"],"additionalProperties":false}`),
		},
		{
			Name:        toolAnswerChoice,
			Description: "Answer a prompt the work agent is waiting for — a permission request or a question.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"choice_id":{"type":"string","description":"the id of the prompt"},"option_id":{"type":"string","description":"the id of the option to pick"}},"required":["choice_id","option_id"],"additionalProperties":false}`),
		},
		{
			Name:        toolAskHuman,
			Description: "Ask the user a question. The run waits for the answer.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"question":{"type":"string","description":"the question"}},"required":["question"],"additionalProperties":false}`),
		},
		{
			Name:        toolRepoStatus,
			Description: "Read the repository facts of the work chat: the branch, whether the tree is dirty, and how far the branch is ahead of or behind its upstream and its base branch.",
			Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		},
		{
			Name:        toolFinish,
			Description: "End the run and report how it went.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"status":{"type":"string","enum":["done","failed"],"description":"done when the workflow is complete, failed when it cannot go on"},"summary":{"type":"string","description":"what was done"}},"required":["status","summary"],"additionalProperties":false}`),
		},
	}
}

// runPendingCalls executes the tool calls the model asked for.
//
// The calls run one at a time and in order. A call that asks the user
// suspends the run until the answer arrives; the answer becomes the
// call's result.
func (s *Service) runPendingCalls(ctx context.Context, r *run) error {
	for len(r.pendingCallList()) > 0 {
		if err := s.checkAlive(ctx, r); err != nil {
			return err
		}
		call := r.pendingCallList()[0]
		res, err := s.execTool(ctx, r, call)
		if err != nil {
			var te *toolError
			if errors.As(err, &te) {
				s.appendToolResult(r, call, stepResult{Status: "error", Summary: te.Error()}, true)
				r.dropPendingCall()
				s.persist(r)
				continue
			}
			return err
		}
		r.dropPendingCall()
		s.appendToolResult(r, call, res, false)
		s.persist(r)
		if r.isFinished() {
			return errFinished
		}
	}
	return nil
}

// execTool runs one tool call.
func (s *Service) execTool(ctx context.Context, r *run, call pendingCall) (stepResult, error) {
	switch call.Name {
	case toolRunStep:
		return s.toolRunStep(ctx, r, call)
	case toolInstruct:
		return s.toolInstruct(ctx, r, call)
	case toolAnswerChoice:
		return s.toolAnswerChoice(ctx, r, call)
	case toolAskHuman:
		return s.toolAskHuman(ctx, r, call)
	case toolRepoStatus:
		return s.toolRepoStatus(ctx, r, call)
	case toolFinish:
		return s.toolFinish(ctx, r, call)
	default:
		return stepResult{}, toolErrorf("there is no tool called %q", call.Name)
	}
}

// toolRunStep runs a step of the workflow.
func (s *Service) toolRunStep(ctx context.Context, r *run, call pendingCall) (stepResult, error) {
	command, err := callArgString(call, "command")
	if err != nil {
		return stepResult{}, err
	}
	res, err := s.runStep(ctx, r, command)
	if err != nil {
		return stepResult{}, err
	}
	return res, nil
}

// runStep runs a command of the workflow and records it.
//
// The command must be a step's message or one of a step's allow
// entries. A command that is a step's message is that step running:
// the run state records it. A command from an allow list is a repair
// inside the step: only the action history records it.
//
// The action is recorded only when the command settled: a step that
// is still running has not been taken yet.
func (s *Service) runStep(ctx context.Context, r *run, command string) (stepResult, error) {
	idx, err := r.checkRun(command)
	if err != nil {
		return stepResult{}, toolErrorf("%s", err.Error())
	}
	res, err := s.runCommand(ctx, r, command, false)
	if err != nil {
		return stepResult{}, err
	}
	if res.Status == statusWaiting {
		// The command is still running: the step has no verdict yet.
		if step, ok := r.stepAt(idx); ok {
			r.recordPending(wfe.JobOf(r.workflow, step.ID), step)
		}
		return res, nil
	}
	out := wfe.Outcome{
		Status:  outcomeStatus(res.Status),
		Summary: res.Summary,
		Data:    res.Data,
	}
	step, ok := r.stepAt(idx)
	if !ok {
		return res, nil
	}
	r.record(wfe.ActionKindRun, command, step.ID, out)
	if r.isStepMessage(idx, command) || wfe.HasRun(r.state, step.ID) {
		// The step ran, or the command was a repair inside a step
		// that ran: the run state carries the latest outcome.
		r.recordStep(wfe.JobOf(r.workflow, step.ID), step, out)
	}
	return res, nil
}

// toolInstruct sends a free-form prompt to the work agent.
func (s *Service) toolInstruct(ctx context.Context, r *run, call pendingCall) (stepResult, error) {
	text, err := callArgString(call, "text")
	if err != nil {
		return stepResult{}, err
	}
	if err := r.checkInstruct(); err != nil {
		return stepResult{}, toolErrorf("%s", err.Error())
	}
	res, err := s.runCommand(ctx, r, text, true)
	if err != nil {
		return stepResult{}, err
	}
	if res.Status != statusWaiting {
		r.record(wfe.ActionKindInstruct, text, "", wfe.Outcome{
			Status:  outcomeStatus(res.Status),
			Summary: res.Summary,
			Data:    res.Data,
		})
	}
	return res, nil
}

// toolAnswerChoice answers a prompt the work agent is waiting for.
func (s *Service) toolAnswerChoice(ctx context.Context, r *run, call pendingCall) (stepResult, error) {
	choiceID, err := callArgString(call, "choice_id")
	if err != nil {
		return stepResult{}, err
	}
	optionID, err := callArgString(call, "option_id")
	if err != nil {
		return stepResult{}, err
	}
	if s.cfg.Answerer == nil {
		return stepResult{}, toolErrorf("nothing can answer a prompt right now")
	}
	inflight := r.inflightState()
	if inflight == nil {
		return stepResult{}, toolErrorf("no command is running, so there is no prompt to answer")
	}
	ex, ok := s.cfg.Loopback.Get(inflight.MsgID)
	if !ok {
		return stepResult{}, toolErrorf("the command is no longer running")
	}
	label := ""
	found := false
	for _, choice := range ex.Choices {
		if choice.RequestID != choiceID {
			continue
		}
		found = true
		for _, opt := range choice.Options {
			if opt.ID == optionID {
				label = opt.Label
				break
			}
		}
		break
	}
	if !found {
		return stepResult{}, toolErrorf("no prompt %q is waiting", choiceID)
	}
	if label == "" {
		return stepResult{}, toolErrorf("the prompt %q has no option %q", choiceID, optionID)
	}
	if err := s.cfg.Answerer.Answer(r.workChatID, choiceID, optionID); err != nil {
		return stepResult{}, toolErrorf("the prompt could not be answered: %v", err)
	}
	return stepResult{Status: "ok", Summary: "answered: " + label}, nil
}

// toolAskHuman asks the user and waits for the answer.
func (s *Service) toolAskHuman(ctx context.Context, r *run, call pendingCall) (stepResult, error) {
	question, err := callArgString(call, "question")
	if err != nil {
		return stepResult{}, err
	}
	reply, err := s.askUser(ctx, r, question, nil)
	if err != nil {
		return stepResult{}, err
	}
	return stepResult{Status: "ok", Summary: "the user answered: " + reply, Data: map[string]string{"answer": reply}}, nil
}

// toolRepoStatus reads the repository facts of the work chat.
func (s *Service) toolRepoStatus(ctx context.Context, r *run, _ pendingCall) (stepResult, error) {
	if s.cfg.Git == nil {
		return stepResult{}, toolErrorf("the repository facts are not available")
	}
	dir := r.cwd
	if dir == "" {
		return stepResult{}, toolErrorf("the work chat has no working directory")
	}
	facts, err := s.repoFacts(ctx, dir)
	if err != nil {
		return stepResult{}, toolErrorf("the repository facts could not be read: %v", err)
	}
	data := map[string]string{
		"branch": facts.Branch,
		"dirty":  strconv.FormatBool(facts.Dirty),
	}
	if facts.Upstream != "" {
		data["upstream"] = facts.Upstream
		data["ahead_of_upstream"] = strconv.Itoa(facts.AheadOfUpstream)
		data["behind_upstream"] = strconv.Itoa(facts.BehindUpstream)
	}
	if facts.Base != "" {
		data["base"] = facts.Base
		data["ahead_of_base"] = strconv.Itoa(facts.AheadOfBase)
		data["behind_base"] = strconv.Itoa(facts.BehindBase)
	}
	summary := facts.Branch
	if facts.Dirty {
		summary += " (dirty)"
	}
	return stepResult{Status: "ok", Summary: summary, Data: data}, nil
}

// toolFinish ends the run.
func (s *Service) toolFinish(ctx context.Context, r *run, call pendingCall) (stepResult, error) {
	status, err := callArgString(call, "status")
	if err != nil {
		return stepResult{}, err
	}
	summary, err := callArgString(call, "summary")
	if err != nil {
		return stepResult{}, err
	}
	if err := r.checkFinish(status); err != nil {
		return stepResult{}, toolErrorf("%s", err.Error())
	}
	r.markFinished()
	s.finish(r, PhaseDone, "")
	s.notify(r, finishLine(r, status, summary))
	s.persist(r)
	return stepResult{Status: status, Summary: summary}, nil
}

// finishLine renders the summary of a finished run.
func finishLine(r *run, status, summary string) string {
	icon := "✅"
	if status != "done" {
		icon = "❌"
	}
	r.mu.Lock()
	actions := len(r.policy.History)
	s1, s2 := r.system1Decisions, r.system2Decisions
	u1, u2 := r.system1Usage, r.system2Usage
	r.mu.Unlock()
	line := fmt.Sprintf("%s %s · %d actions · system-1: %d decisions, %d tokens · system-2: %d decisions, %d tokens",
		icon, strings.TrimSpace(summary), actions, s1, u1.Total(), s2, u2.Total())
	return line
}

// The statuses a step result can carry beyond the outcome
// vocabulary.
const (
	// statusWaiting means the step is still running.
	statusWaiting = "waiting"
	// statusInterrupted means the execution of the step is gone.
	statusInterrupted = "interrupted"
)

// outcomeStatus maps a step result status onto the outcome
// vocabulary.
//
// An interrupted command reported no verdict, so it is recorded as
// unknown — the vocabulary's way of saying that nobody said anything.
func outcomeStatus(status string) wfe.OutcomeStatus {
	if status == statusInterrupted {
		return wfe.OutcomeUnknown
	}
	return wfe.OutcomeStatus(status)
}

// outcomeSucceeded reports whether a step result status means the
// work was done.
//
// "noop" counts: the request was valid and the world already
// satisfied it, so the step did what it was asked to do.
func outcomeSucceeded(status string) bool {
	return status == string(wfe.OutcomeOK) || status == string(wfe.OutcomeNoop)
}

// runCommand sends a command to the work chat and waits for it.
//
// A command that is already running is picked up where it left off,
// so a step that came back early — waiting for a prompt, or stalled —
// can be resumed instead of run twice.
func (s *Service) runCommand(ctx context.Context, r *run, command string, instruct bool) (stepResult, error) {
	res, err := s.runCommandInner(ctx, r, command, instruct)
	if err != nil {
		return res, err
	}
	s.reportOutcome(r, command, res)
	return res, nil
}

// reportOutcome tells the observer how a command ended.
func (s *Service) reportOutcome(r *run, command string, res stepResult) {
	r.setTranscript(res.Transcript)
	switch res.Status {
	case statusWaiting:
		s.notify(r, "⏳ "+command+" is waiting · "+res.Summary)
	case string(wfe.OutcomeOK), string(wfe.OutcomeNoop):
		s.notify(r, "✅ "+command)
	case string(wfe.OutcomeIncomplete):
		s.notify(r, "⚠️ "+command+" incomplete"+groupsSuffix(res.Data))
	case string(wfe.OutcomeFailed):
		s.notify(r, "❌ "+command+" failed")
	case string(wfe.OutcomeTimeout):
		s.notify(r, "⏱ "+command+" timed out")
	case string(wfe.OutcomeCancelled):
		s.notify(r, "⏹ "+command+" was cancelled")
	default:
		s.notify(r, "❓ "+command+" finished with no verdict")
	}
}

// groupsSuffix renders how many review groups are still open.
func groupsSuffix(data map[string]string) string {
	unfinished, ok := data["unfinished_groups"]
	if !ok {
		return ""
	}
	total, ok := data["groups"]
	if !ok {
		return ""
	}
	return fmt.Sprintf(" · %s/%s groups unfinished", unfinished, total)
}

// runCommandInner sends the command and waits for it.
func (s *Service) runCommandInner(ctx context.Context, r *run, command string, instruct bool) (stepResult, error) {
	if s.cfg.Loopback == nil {
		return stepResult{}, errors.New("me: no loopback client")
	}
	msgID := ""
	if inflight := r.inflightState(); inflight != nil && inflight.Command == command {
		ex, ok := s.cfg.Loopback.Get(inflight.MsgID)
		switch {
		case !ok:
			// The execution is gone: the daemon that carried it is
			// no longer watching it.
			r.clearInflight()
			return stepResult{Status: statusInterrupted, Summary: "the command was interrupted"}, nil
		case ex.Phase == loopback.PhaseSettled:
			r.clearInflight()
			return resultOf(ex), nil
		default:
			msgID = inflight.MsgID
		}
	}
	if msgID == "" {
		s.notify(r, "▶ "+truncate(command, 80))
		id, err := s.cfg.Loopback.Send(ctx, r.workChatID, command)
		if err != nil {
			return stepResult{}, fmt.Errorf("the work chat could not run %q: %w", command, err)
		}
		msgID = id
		r.setInflight(&inflightState{MsgID: msgID, Command: command, Instruct: instruct})
		// The in-flight command is persisted right away: a daemon
		// that dies while it runs comes back and resolves it.
		s.persist(r)
	}
	return s.watchCommand(ctx, r, msgID, command)
}

// watchCommand waits for a command to settle.
//
// It comes back early when the work agent asks something — the model
// can then answer — or when the command goes quiet for too long.
func (s *Service) watchCommand(ctx context.Context, r *run, msgID, command string) (stepResult, error) {
	ch := r.watchChannel()
	if ch == nil {
		ch = s.cfg.Loopback.Watch(msgID)
		r.setWatchChannel(ch)
	}

	deadline := s.now().Add(s.cfg.StepTimeout)
	lastBeat := s.now()
	poll := s.pollInterval()

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return stepResult{}, ctx.Err()
		case <-r.ctx.Done():
			return stepResult{}, r.ctx.Err()
		case ex, ok := <-ch:
			if !ok {
				// The watcher was closed without a final snapshot.
				ex, found := s.cfg.Loopback.Get(msgID)
				if !found {
					r.clearInflight()
					return stepResult{Status: string(wfe.OutcomeUnknown), Summary: "the command was interrupted"}, nil
				}
				if ex.Phase != loopback.PhaseSettled {
					// The watcher closed while the command was
					// still running. The next attempt watches the
					// execution again.
					r.clearWatch()
					return s.waitingResult("the command is no longer running"), nil
				}
				r.clearInflight()
				return resultOf(ex), nil
			}
			if ex.Phase == loopback.PhaseSettled {
				r.clearInflight()
				return resultOf(ex), nil
			}
			if len(ex.Choices) > 0 {
				return s.waitingResult(fmt.Sprintf("the work agent is waiting for an answer: %s", ex.Choices[0].Title)), nil
			}
			if beat := ex.Progress.LastBeatAt; !beat.IsZero() && beat.After(lastBeat) {
				lastBeat = beat
			}
		case <-ticker.C:
			now := s.now()
			if now.After(deadline) {
				r.clearInflight()
				return stepResult{Status: string(wfe.OutcomeTimeout), Summary: "the command timed out"}, nil
			}
			if now.Sub(lastBeat) > s.cfg.StallTimeout {
				return s.waitingResult("the work agent went quiet"), nil
			}
		}
	}
}

// truncate shortens a text to n characters.
func truncate(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[:n] + "…"
}

// waitingResult builds the result of a command that is not finished
// yet.
func (s *Service) waitingResult(reason string) stepResult {
	return stepResult{Status: statusWaiting, Summary: reason}
}

// pollInterval returns how often a running command is checked for a
// timeout or a stall.
func (s *Service) pollInterval() time.Duration {
	interval := time.Second
	if quarter := s.cfg.StallTimeout / 4; quarter > 0 && quarter < interval {
		interval = quarter
	}
	return interval
}

// resultOf turns a settled execution into the result of a command.
func resultOf(ex loopback.Execution) stepResult {
	res := stepResult{
		Status:  string(ex.Outcome.Status),
		Summary: ex.Outcome.Summary,
		Data:    ex.Outcome.Data,
	}
	res.Transcript = transcriptOf(ex)
	return res
}

// transcriptOf renders the transcript of an execution, keeping its
// head and its tail.
//
// A long transcript is cut: the opening lines say what was asked, the
// closing lines say how it ended.
func transcriptOf(ex loopback.Execution) string {
	var b strings.Builder
	for i, entry := range ex.Transcript {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(entry.Text)
	}
	text := b.String()
	if len(text) <= transcriptBytes {
		return text
	}
	half := transcriptBytes / 2
	return text[:half] + "\n…\n" + tailText(text, half)
}

// appendToolResult adds the result of a tool call to the
// conversation.
func (s *Service) appendToolResult(r *run, call pendingCall, res stepResult, isError bool) {
	raw, err := json.Marshal(res)
	if err != nil {
		raw = json.RawMessage(`{"status":"error","summary":"the result could not be encoded"}`)
	}
	r.appendMessage(llm.Message{
		Role: llm.RoleTool,
		Parts: []llm.Part{{
			Type:       llm.PartToolResult,
			ToolCallID: call.ID,
			ToolName:   call.Name,
			Text:       string(raw),
			IsError:    isError,
		}},
	})
}

// callArgString reads a string argument of a tool call.
func callArgString(call pendingCall, name string) (string, error) {
	var args map[string]any
	if len(call.Args) > 0 {
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return "", toolErrorf("the arguments of %s are not valid JSON", call.Name)
		}
	}
	v, ok := args[name]
	if !ok {
		return "", toolErrorf("%s needs the %q argument", call.Name, name)
	}
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", toolErrorf("the %q argument of %s must be a non-empty string", name, call.Name)
	}
	return s, nil
}
