package me

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/gateway/observe"
	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// The tools the chat model may call.
const (
	// toolResume runs the step that broke again.
	toolResume = "resume"
	// toolNext says the step is already done.
	toolNext = "next"
	// toolRunStep names the step the decision model ran.
	//
	// The chat model has no such tool: it repairs a gap and says which
	// way to go. The decision model's action is recorded under this
	// name, so the conversation the chat model reads stays consistent.
	toolRunStep = "run_step"
	// toolInstruct sends a free-form prompt to the coding agent.
	toolInstruct = "instruct"
	// toolAgent asks the thinking of the owner.
	toolAgent = "agent"
	// toolAnswerChoice answers a pending prompt of the chat.
	toolAnswerChoice = "answer_choice"
	// toolAskHuman asks the user.
	toolAskHuman = "ask_human"
	// toolRepoStatus reads the repository facts.
	toolRepoStatus = "repo_status"
	// toolWait waits for a duration before the model looks again.
	toolWait = "wait"
	// toolEndRound ends the round of the job the run is on.
	toolEndRound = "end_round"
	// toolFinish ends the run.
	toolFinish = "finish"
)

// maxWait is how long one wait may last.
//
// The wait happens inside the agent's turn: the model waits, looks
// again and decides. A wait longer than this would hold the turn for
// too long, so it is cut short.
const maxWait = 10 * time.Minute

// toolError is a failure the model can correct.
//
// It comes back as a tool error result, so the model sees what went
// wrong and tries again. It does not end the run.
type toolError struct{ msg string }

func (e *toolError) Error() string { return e.msg }

// isToolError reports whether the error is one the agent can correct.
//
// A tool error does not end the run: it comes back to the agent, which
// tries again.
func isToolError(err error) bool {
	var te *toolError
	return errors.As(err, &te)
}

// toolErrorf builds a tool error.
func toolErrorf(format string, args ...any) error {
	return &toolError{msg: fmt.Sprintf(format, args...)}
}

// stepResult is the result of a command the run sent to the the
// chat.
type stepResult struct {
	// Status is the machine-readable verdict.
	Status string `json:"status"`
	// Summary is the one-line restatement of what happened.
	Summary string `json:"summary"`
	// Data carries the facts the command reported.
	Data map[string]string `json:"data,omitempty"`
	// Transcript is the tail of what the coding agent said.
	Transcript string `json:"transcript,omitempty"`
	// entries is the whole transcript of the execution. It is not
	// part of the tool result — the model reads the truncated
	// transcript above — but the run scans every line of it.
	entries []observe.Entry
}

// toolCatalogue returns the tools the chat model may call.
//
// The nightme commands are tools of their own: they are the only way
// the model runs a command, and each carries the command's own
// description.
func (s *Service) toolCatalogue(r *run) []llm.Tool {
	tools := []llm.Tool{
		{
			Name:        toolResume,
			Description: "Run the step that broke again, after you repaired the gap.",
			Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		},
		{
			Name:        toolNext,
			Description: "Say the step is already done and let the engine continue with the workflow.",
			Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		},
	}
	tools = append(tools, commandTools(r)...)
	tools = append(tools,
		llm.Tool{
			Name:        toolInstruct,
			Description: "Send a free-form instruction to the coding agent, for the cases the workflow's commands do not cover.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"the instruction"}},"required":["text"],"additionalProperties":false}`),
		},
		llm.Tool{
			Name:        toolAnswerChoice,
			Description: "Answer a prompt the coding agent is waiting for — a permission request or a question.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"choice_id":{"type":"string","description":"the id of the prompt"},"option_id":{"type":"string","description":"the id of the option to pick"}},"required":["choice_id","option_id"],"additionalProperties":false}`),
		},
		llm.Tool{
			Name:        toolAskHuman,
			Description: "Ask the user a question. The run waits for the answer. The operator answers by quoting the question or by picking a button on the run card.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"question":{"type":"string","description":"the question"}},"required":["question"],"additionalProperties":false}`),
		},
		llm.Tool{
			Name:        toolRepoStatus,
			Description: "Read the repository facts of the chat: the branch, whether the tree is dirty, how far the branch is ahead of or behind its upstream and its base branch, and the directory they were read in.",
			Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		},
		llm.Tool{
			Name:        toolWait,
			Description: "Wait before you look again — for work that is still running in the background. The duration is a Go duration (\"30s\", \"5m\") and the wait lasts at most 10 minutes.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"duration":{"type":"string","description":"how long to wait, e.g. 30s or 5m"}},"required":["duration"],"additionalProperties":false}`),
		},
		llm.Tool{
			Name:        toolEndRound,
			Description: "End the round of the job the run is on, because there is genuinely nothing left to do in it. The steps of the round that did not run are skipped, the job's final step runs, and the job starts its next round.",
			Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		},
		llm.Tool{
			Name:        toolFinish,
			Description: "End the run and report how it went.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"status":{"type":"string","enum":["done","failed"],"description":"done when the workflow is complete, failed when it cannot go on"},"summary":{"type":"string","description":"what was done"}},"required":["status","summary"],"additionalProperties":false}`),
		},
	)
	if s.hasBrain(r) {
		tools = append(tools, llm.Tool{
			Name:        toolAgent,
			Description: "Ask the owner's thinking about a task. It investigates and comes back with the cause and a recommendation. It changes nothing.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"task":{"type":"string","description":"the question to investigate"}},"required":["task"],"additionalProperties":false}`),
		})
	}
	return tools
}

// commandTools returns the tools of the nightme commands the run may
// issue.
func commandTools(r *run) []llm.Tool {
	out := make([]llm.Tool, 0, len(r.commands))
	for _, c := range r.commands {
		name := toolName(c.Name)
		if name == "" {
			continue
		}
		out = append(out, llm.Tool{
			Name:        name,
			Description: commandDescription(c),
			Schema:      commandSchema(c),
		})
	}
	return out
}

// commandDescription renders what the model reads about a command.
func commandDescription(c CommandInfo) string {
	var b strings.Builder
	if summary := strings.TrimSpace(c.Summary); summary != "" {
		b.WriteString(summary)
	}
	if usage := strings.TrimSpace(c.Usage); usage != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("usage: ")
		b.WriteString(usage)
	}
	if len(c.Subcommands) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("subcommands:")
		for _, s := range c.Subcommands {
			b.WriteString("\n- ")
			b.WriteString(s.Name)
			if summary := strings.TrimSpace(s.Summary); summary != "" {
				b.WriteString(": ")
				b.WriteString(summary)
			}
		}
	}
	return b.String()
}

// commandSchema renders the arguments of a command.
//
// A command with subcommands takes one of them; a command without
// them takes its arguments only. The arguments are always optional.
func commandSchema(c CommandInfo) json.RawMessage {
	props := map[string]any{}
	if len(c.Subcommands) > 0 {
		values := make([]string, 0, len(c.Subcommands))
		for _, s := range c.Subcommands {
			values = append(values, s.Name)
		}
		props["subcommand"] = map[string]any{
			"type":        "string",
			"description": "the subcommand to run",
			"enum":        values,
		}
	}
	props["args"] = map[string]any{
		"type":        "string",
		"description": "the arguments of the command",
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(c.Subcommands) > 0 {
		schema["required"] = []string{"subcommand"}
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	}
	return raw
}

// runPendingCalls executes the tool calls the model asked for.
//
// The calls run one at a time and in order. A call that asks the user
// suspends the run until the answer arrives; the answer becomes the
// call's result.
//
// Every call is published to the chat: the call before it runs and
// the result after it settles. Both hang under the card the call
// started under — the run may move to another iteration while the
// call runs.
func (s *Service) runPendingCalls(ctx context.Context, r *run) error {
	for len(r.pendingCallList()) > 0 {
		if err := s.checkAlive(ctx, r); err != nil {
			return err
		}
		call := r.pendingCallList()[0]
		// The call hangs under the decision that made it: the call and
		// its result belong under the same card.
		sess := r.decisionSessionRef()
		// The call is counted before it leaves the run: a chat that
		// hides the run's tool calls does not change what the run did.
		s.countToolCall(ctx, r)
		s.publishToolStart(ctx, r, sess, call)
		// The run moved: the card counts the call it is making.
		s.refreshSessions(r)
		res, err := s.execTool(ctx, r, call)
		if err != nil {
			var te *toolError
			if errors.As(err, &te) {
				s.publishToolEnd(ctx, r, sess, call, stepResult{Status: "error", Summary: te.Error()}, nil)
				s.refreshSessions(r)
				s.appendToolResult(r, call, stepResult{Status: "error", Summary: te.Error()}, true)
				r.dropPendingCall()
				s.persist(r)
				continue
			}
			s.publishToolEnd(ctx, r, sess, call, stepResult{}, err)
			return err
		}
		if res.Status != statusWaiting {
			// The call settled. A call whose command is still running
			// has no verdict yet: the result comes when the run picks
			// the command up again.
			s.publishToolEnd(ctx, r, sess, call, res, nil)
			s.refreshSessions(r)
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
	case toolResume:
		return s.toolResume(ctx, r, call)
	case toolNext:
		return s.toolNext(ctx, r, call)
	case toolInstruct:
		return s.toolInstruct(ctx, r, call)
	case toolAgent:
		return s.toolAgent(ctx, r, call)
	case toolAnswerChoice:
		return s.toolAnswerChoice(ctx, r, call)
	case toolAskHuman:
		return s.toolAskHuman(ctx, r, call)
	case toolRepoStatus:
		return s.toolRepoStatus(ctx, r, call)
	case toolWait:
		return s.toolWait(ctx, r, call)
	case toolEndRound:
		return s.toolEndRound(ctx, r, call)
	case toolFinish:
		return s.toolFinish(ctx, r, call)
	default:
		if cmd, ok := r.commandFor(call.Name); ok {
			return s.toolCommand(ctx, r, call, cmd)
		}
		return stepResult{}, toolErrorf("there is no tool called %q", call.Name)
	}
}

// judgmentStep returns the step the run is on: the one the judgment
// is about.
//
// The agent names no step: the run knows it, and the engine decides
// what runs next.
func judgmentStep(r *run) (wfe.Step, error) {
	stepID := r.budgetStep()
	if stepID == "" {
		return wfe.Step{}, toolErrorf("the run is not on a step")
	}
	step, ok := r.stepByID(stepID)
	if !ok {
		return wfe.Step{}, toolErrorf("the step %q is not part of this workflow", stepID)
	}
	return step, nil
}

// toolResume runs the step that broke again.
//
// The step is the one the judgment is about: the run knows it, so the
// agent names nothing. Its budget is checked like the budget of any
// other action, and the step's verdict comes back.
func (s *Service) toolResume(ctx context.Context, r *run, _ pendingCall) (stepResult, error) {
	step, err := judgmentStep(r)
	if err != nil {
		return stepResult{}, err
	}
	st, ok := r.stepState(step.ID)
	if !ok {
		// The step never ran: there is nothing to run again.
		return stepResult{}, toolErrorf("the step %q has not run: there is nothing to resume", step.ID)
	}
	if st.Outcome.Succeeded() {
		// The step did its job: there is nothing to repair.
		return stepResult{}, toolErrorf("the step %q did not break: there is nothing to repair", step.ID)
	}
	if !r.runnableStep(step.ID) {
		return stepResult{}, toolErrorf("the step %q cannot run now: it did not run this round and it is not ready", step.ID)
	}
	if err := r.checkStep(step); err != nil {
		return stepResult{}, toolErrorf("%s", err.Error())
	}
	idx := r.stepIndex(step.ID)
	// The card follows the step: the step runs in the job and the
	// round of that step.
	s.switchRoundSession(ctx, r, r.iterationOfStep(idx))
	job := wfe.JobOf(r.workflow, step.ID)
	verdict, err := s.executeStep(ctx, r, job, step)
	if err != nil {
		return stepResult{}, err
	}
	if !verdict.Settled {
		// The step is still running: it has no verdict yet.
		return stepResult{Status: statusWaiting, Summary: verdict.Outcome.Summary}, nil
	}
	r.recordStep(job, step, verdict.Outcome)
	s.recordStepOutcome(ctx, r)
	s.persist(r)
	s.refreshSessions(r)
	return stepResult{
		Status:     string(verdict.Outcome.Status),
		Summary:    verdict.Outcome.Summary,
		Data:       verdict.Outcome.Data,
		Transcript: verdict.Transcript,
	}, nil
}

// toolNext records the step as done and hands the control back.
//
// The step is the one the run is on: the judgment names it, and the
// run knows it. The facts the step produced are kept, the summary says
// the agent decided the step was done, and the engine plans the next
// step.
func (s *Service) toolNext(ctx context.Context, r *run, _ pendingCall) (stepResult, error) {
	step, err := judgmentStep(r)
	if err != nil {
		return stepResult{}, err
	}
	st, ok := r.stepState(step.ID)
	if !ok {
		return stepResult{}, toolErrorf("the step %q has not run: there is nothing to declare done", step.ID)
	}
	if st.Pending {
		// The step is still running: the work is in flight.
		return stepResult{}, toolErrorf("the step %q is still running: wait for it", step.ID)
	}
	if st.Outcome.Succeeded() {
		// The step did its job: there is nothing to repair.
		return stepResult{}, toolErrorf("the step %q is already done: there is nothing to repair", step.ID)
	}
	job := wfe.JobOf(r.workflow, step.ID)
	done := doneOutcome(st.Outcome)
	r.recordStep(job, step, done)
	s.recordStepOutcome(ctx, r)
	// The history has to agree with the run state: the step is done,
	// and the run says so.
	r.record(wfe.ActionKindRun, s.stepCommand(r, step), step.ID, done)
	s.persist(r)
	s.refreshSessions(r)
	return stepResult{
		Status:  string(done.Status),
		Summary: done.Summary,
		Data:    done.Data,
	}, nil
}

// stepCommand renders the command a step runs, the way the run records
// it.
func (s *Service) stepCommand(r *run, step wfe.Step) string {
	if step.Kind() == wfe.StepKindWait {
		return "wait " + step.Wait
	}
	if command := s.evalText(r, stepText(step)); command != "" {
		return command
	}
	return step.ID
}

// toolCommand runs a nightme command as the owner.
func (s *Service) toolCommand(ctx context.Context, r *run, call pendingCall, cmd CommandInfo) (stepResult, error) {
	sub, _ := callArgOptional(call, "subcommand")
	if len(cmd.Subcommands) > 0 {
		if sub == "" {
			return stepResult{}, toolErrorf("%s needs a subcommand", toolName(cmd.Name))
		}
		if !hasSubcommand(cmd, sub) {
			return stepResult{}, toolErrorf("%s has no subcommand %q", toolName(cmd.Name), sub)
		}
	}
	args, _ := callArgOptional(call, "args")
	command := "/" + cmd.Name
	if sub != "" {
		command += " " + sub
	}
	if args != "" {
		command += " " + args
	}
	return s.runStep(ctx, r, command)
}

// hasSubcommand reports whether the command has the subcommand.
func hasSubcommand(cmd CommandInfo, name string) bool {
	for _, s := range cmd.Subcommands {
		if s.Name == name {
			return true
		}
	}
	return false
}

// toolAgent asks the thinking of the owner.
func (s *Service) toolAgent(ctx context.Context, r *run, call pendingCall) (stepResult, error) {
	task, err := callArgString(call, "task")
	if err != nil {
		return stepResult{}, err
	}
	if err := r.checkCommandText(task); err != nil {
		return stepResult{}, toolErrorf("%s", err.Error())
	}
	reply, err := s.askBrain(ctx, r, task)
	if err != nil {
		return stepResult{}, toolErrorf("%s", err.Error())
	}
	summary, transcript := brainSummary(reply)
	data := map[string]string{}
	if reply.Model != "" {
		data["model"] = reply.Model
	}
	return stepResult{
		Status:     string(wfe.OutcomeOK),
		Summary:    summary,
		Data:       data,
		Transcript: transcript,
	}, nil
}

// runStep runs a command of the workflow and records it.
//
// A command that is a step's message is that step running: the run
// state records it, and it is the step's verdict.
//
// A command of the registry that belongs to no step is a repair: it
// counts against the step the run is on, and it never becomes that
// step's verdict.
//
// A command that could not run at all comes back as a tool error: the
// agent reads what went wrong and tries again.
//
// The action is recorded only when the command settled: a step that
// is still running has not been taken yet.
func (s *Service) runStep(ctx context.Context, r *run, command string) (stepResult, error) {
	idx, err := r.checkRun(command)
	if err != nil {
		return stepResult{}, toolErrorf("%s", err.Error())
	}
	// The step's own message is the step running. Anything else is a
	// repair: it counts against the step the run is on and leaves the
	// step's verdict alone.
	owned := r.isStepMessage(idx, command)
	// The card follows the step the command belongs to: the command
	// runs in the job and the round of that step, and the card records
	// where the run is working.
	s.switchRoundSession(ctx, r, r.iterationOfStep(idx))
	res, err := s.runCommand(ctx, r, command, false)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return stepResult{}, err
		}
		out := failedOutcome(err.Error(), nil)
		step, ok := r.stepAt(idx)
		if !ok {
			r.record(actionKind(owned), command, "", out)
			return stepResult{}, toolErrorf("%s", err.Error())
		}
		r.record(actionKind(owned), command, step.ID, out)
		if owned {
			// The step ran: the run state carries the outcome.
			r.recordStep(wfe.JobOf(r.workflow, step.ID), step, out)
			s.recordStepOutcome(ctx, r)
		}
		return stepResult{}, toolErrorf("%s", err.Error())
	}
	if res.Status == statusWaiting {
		// The command is still running: the step has no verdict yet.
		// A repair changes nothing: the command it started is picked
		// up where it left off.
		if owned {
			if step, ok := r.stepAt(idx); ok {
				r.recordPending(wfe.JobOf(r.workflow, step.ID), step)
			}
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
		// The action still happened, so it is recorded — the budget
		// has to know about it.
		r.record(actionKind(owned), command, "", out)
		return res, nil
	}
	r.record(actionKind(owned), command, step.ID, out)
	if owned {
		// The step ran: the run state carries the outcome.
		r.recordStep(wfe.JobOf(r.workflow, step.ID), step, out)
		s.recordStepOutcome(ctx, r)
	}
	return res, nil
}

// actionKind returns the kind an action is recorded as: a command
// that is no step's own message is a repair.
func actionKind(owned bool) wfe.ActionKind {
	if owned {
		return wfe.ActionKindRun
	}
	return wfe.ActionKindCommand
}

// toolInstruct sends a free-form prompt to the coding agent.
func (s *Service) toolInstruct(ctx context.Context, r *run, call pendingCall) (stepResult, error) {
	text, err := callArgString(call, "text")
	if err != nil {
		return stepResult{}, err
	}
	if err := r.checkInstruct(); err != nil {
		return stepResult{}, toolErrorf("%s", err.Error())
	}
	if err := r.checkCommandText(text); err != nil {
		return stepResult{}, toolErrorf("%s", err.Error())
	}
	res, err := s.runCommand(ctx, r, text, true)
	if err != nil {
		return stepResult{}, err
	}
	if res.Status != statusWaiting {
		// The instruction is a repair: it is work the agent causes, so
		// it costs a chance of the step the run is on.
		r.record(wfe.ActionKindInstruct, text, r.budgetStep(), wfe.Outcome{
			Status:  outcomeStatus(res.Status),
			Summary: res.Summary,
			Data:    res.Data,
		})
	}
	return res, nil
}

// toolAnswerChoice answers a prompt the chat is waiting for.
//
// The answer is a click: the prompt is patched to show the choice,
// and the click reaches the agent the way a person's click reaches
// it.
func (s *Service) toolAnswerChoice(ctx context.Context, r *run, call pendingCall) (stepResult, error) {
	choiceID, err := callArgString(call, "choice_id")
	if err != nil {
		return stepResult{}, err
	}
	optionID, err := callArgString(call, "option_id")
	if err != nil {
		return stepResult{}, err
	}
	if s.cfg.Operator == nil {
		return stepResult{}, toolErrorf("nothing can answer a prompt right now")
	}
	inflight := r.inflightState()
	if inflight == nil {
		return stepResult{}, toolErrorf("no command is running, so there is no prompt to answer")
	}
	ex, ok := s.cfg.Operator.Get(inflight.MsgID)
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
	if err := s.cfg.Operator.Act(ctx, r.chatID, messages.ActionPayload{
		RequestID: choiceID,
		Option:    optionID,
	}); err != nil {
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

// toolRepoStatus reads the repository facts of the chat.
func (s *Service) toolRepoStatus(ctx context.Context, r *run, _ pendingCall) (stepResult, error) {
	if s.cfg.Git == nil {
		return stepResult{}, toolErrorf("the repository facts are not available")
	}
	dir := s.workDir(r)
	if dir == "" {
		return stepResult{}, toolErrorf("the chat has no working directory")
	}
	facts, err := s.repoFacts(ctx, dir)
	if err != nil {
		return stepResult{}, toolErrorf("the repository facts could not be read: %v", err)
	}
	data := map[string]string{
		"branch": facts.Branch,
		"dirty":  strconv.FormatBool(facts.Dirty),
		"cwd":    dir,
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

// toolWait waits for a duration before the model looks again.
//
// The wait happens inside the agent's turn, so the model can wait and
// then look again without the run going on in the meantime. It
// belongs to the run: `/me stop` and the daemon's shutdown end it at
// once.
func (s *Service) toolWait(ctx context.Context, r *run, call pendingCall) (stepResult, error) {
	text, err := callArgString(call, "duration")
	if err != nil {
		return stepResult{}, err
	}
	d, err := time.ParseDuration(strings.TrimSpace(text))
	if err != nil {
		return stepResult{}, toolErrorf("%q is not a duration: use \"30s\" or \"5m\"", text)
	}
	if d <= 0 {
		return stepResult{}, toolErrorf("the duration must be positive")
	}
	capped := d > maxWait
	if capped {
		d = maxWait
	}

	s.refreshSessions(r)
	defer func() {
		s.refreshSessions(r)
	}()

	execCtx, cancel := r.runContext(ctx)
	defer cancel()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-execCtx.Done():
		return stepResult{}, execCtx.Err()
	}
	summary := "waited " + d.String()
	if capped {
		summary += " (a wait lasts at most 10 minutes)"
	}
	return stepResult{
		Status:  string(wfe.OutcomeOK),
		Summary: summary,
	}, nil
}

// toolEndRound ends the round of the job the run is on.
//
// It is the generic signal for "there is nothing left to do here":
// the steps of the round that did not run are skipped, the final step
// of the job runs, and the job starts its next round.
func (s *Service) toolEndRound(ctx context.Context, r *run, _ pendingCall) (stepResult, error) {
	job, round := r.brainIteration()
	if job == "" {
		return stepResult{}, toolErrorf("the run is not on a job yet")
	}
	r.mu.Lock()
	err := wfe.EndRound(r.state, r.workflow, job)
	r.mu.Unlock()
	if err != nil {
		return stepResult{}, toolErrorf("%s", err.Error())
	}
	// The round moved on: the model reads the new situation before it
	// decides again.
	r.markSituation()
	s.persist(r)
	s.refreshSessions(r)
	return stepResult{
		Status:  string(wfe.OutcomeOK),
		Summary: fmt.Sprintf("the round %d of %s is over", round, job),
	}, nil
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
	if status == "failed" {
		// The model says the run did not work: the reason is the
		// error, and the summary says how it ended.
		r.setSummary("the run failed")
		s.finish(r, PhaseFailed, summary)
	} else {
		r.setSummary(summary)
		s.finish(r, PhaseDone, "")
	}
	s.refreshSessions(r)
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

// runCommand sends a command to the chat and waits for it.
//
// A command that is already running is picked up where it left off,
// so a step that came back early — waiting for a prompt, or stalled —
// can be resumed instead of run twice.
//
// A command the agent could not take because it is rate limited pauses
// the run: the agent is throttled, and running the command again
// makes it worse.
func (s *Service) runCommand(ctx context.Context, r *run, command string, instruct bool) (stepResult, error) {
	res, err := s.runCommandInner(ctx, r, command, instruct)
	if err != nil {
		return res, err
	}
	s.reportOutcome(r, command, res)
	if throttled(res) {
		r.requestPause(rateLimitReason)
	}
	return res, nil
}

// reportOutcome records how a command ended and refreshes the run's
// overview.
func (s *Service) reportOutcome(r *run, command string, res stepResult) {
	r.setTranscript(command, res.Transcript)
	s.refreshSessions(r)
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
	if s.cfg.Operator == nil {
		return stepResult{}, errors.New("me: no operator: the chat cannot be driven")
	}
	msgID := ""
	if inflight := r.inflightState(); inflight != nil && inflight.Command == command {
		ex, ok := s.cfg.Operator.Get(inflight.MsgID)
		switch {
		case !ok:
			// The execution is gone: the daemon that carried it is
			// no longer watching it.
			r.clearInflight()
			return stepResult{Status: statusInterrupted, Summary: "the command was interrupted"}, nil
		case ex.Phase == observe.PhaseSettled:
			r.clearInflight()
			return resultOf(ex), nil
		default:
			msgID = inflight.MsgID
		}
	}
	if msgID == "" {
		id, err := s.speak(ctx, r, command, "")
		if err != nil {
			return stepResult{}, fmt.Errorf("the chat could not run %q: %w", command, err)
		}
		msgID = id
		r.setLastCommandMsgID(msgID)
		r.setInflight(&inflightState{MsgID: msgID, Command: command, Instruct: instruct})
		// The in-flight command is persisted right away: a daemon
		// that dies while it runs comes back and resolves it.
		s.persist(r)
		s.refreshSessions(r)
	}
	return s.watchCommand(ctx, r, msgID, command)
}

// watchCommand waits for a command to settle.
//
// The run has no clock of its own here: the agent behind the command
// decides when it is done, and its verdict is the verdict. The run
// only watches.
//
// It comes back early when the chat asks something — the agent is
// waiting for an answer, and the model can answer it.
func (s *Service) watchCommand(ctx context.Context, r *run, msgID, command string) (stepResult, error) {
	ch := r.watchChannel()
	if ch == nil {
		ch = s.cfg.Operator.Watch(msgID)
		r.setWatchChannel(ch)
	}

	for {
		select {
		case <-ctx.Done():
			return stepResult{}, ctx.Err()
		case <-r.ctx.Done():
			return stepResult{}, r.ctx.Err()
		case ex, ok := <-ch:
			if !ok {
				// The watcher was closed without a final snapshot.
				ex, found := s.cfg.Operator.Get(msgID)
				if !found {
					r.clearInflight()
					return stepResult{Status: string(wfe.OutcomeUnknown), Summary: "the command was interrupted"}, nil
				}
				if ex.Phase != observe.PhaseSettled {
					// The watcher closed while the command was
					// still running. The next attempt watches the
					// execution again.
					r.clearWatch()
					return s.waitingResult("the command is no longer running"), nil
				}
				r.clearInflight()
				return resultOf(ex), nil
			}
			if ex.Phase == observe.PhaseSettled {
				r.clearInflight()
				return resultOf(ex), nil
			}
			if len(ex.Choices) > 0 {
				return s.waitingResult(fmt.Sprintf("the chat is waiting for an answer: %s", ex.Choices[0].Title)), nil
			}
			// The command is still running: the card follows it
			// through the snapshots of the tracker. The refresh is
			// throttled, so a chatty command does not write to the
			// chat on every line it produces.
			s.refreshSessions(r)
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

// resultOf turns a settled execution into the result of a command.
func resultOf(ex observe.Execution) stepResult {
	res := stepResult{
		Status:  string(ex.Outcome.Status),
		Summary: ex.Outcome.Summary,
		Data:    ex.Outcome.Data,
	}
	res.Transcript = transcriptOf(ex)
	res.entries = ex.Transcript
	return res
}

// transcriptOf renders the transcript of an execution, keeping its
// head and its tail.
//
// A long transcript is cut: the opening lines say what was asked, the
// closing lines say how it ended.
func transcriptOf(ex observe.Execution) string {
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

// callArgOptional reads a string argument of a tool call that may be
// absent.
//
// An argument that is there but empty reads as absent.
func callArgOptional(call pendingCall, name string) (string, bool) {
	var args map[string]any
	if len(call.Args) > 0 {
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return "", false
		}
	}
	v, ok := args[name]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(s), s != ""
}
