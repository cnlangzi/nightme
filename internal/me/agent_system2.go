package me

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// The turn of the chat model.
//
// The turn carries its own deadline: the run does not inherit the
// client's own timeout, and a provider that never answers does not
// look like a frozen run.
const (
	// maxAgentTurn bounds one turn of the chat model.
	maxAgentTurn = 5 * time.Minute
	// firstProviderWait is how long the run waits after the chat
	// model failed the first time.
	firstProviderWait = 30 * time.Second
	// maxProviderWait is the longest the run waits between two
	// attempts.
	maxProviderWait = 5 * time.Minute
)

// system2 runs one turn of the chat model.
//
// It reports whether the run is over.
//
// The chat model does not end the run by what it says: a turn that
// produced no tool call costs a chance against the step and the agent
// is asked again. The run ends when the plan is done, when the agent
// calls the finish tool, or when the operator stops it.
func (s *Service) system2(ctx context.Context, r *run) (bool, error) {
	// The chat moves when a command moves it: the run's declared
	// context follows the chat before the model reads the report.
	s.syncWorkDir(r)
	conversation := r.conversation()
	if len(conversation) == 0 {
		// The run talks to the chat model for the first time at a
		// judgment point. A chat model with nothing but a system
		// prompt has no conversation to answer: the API refuses the
		// request. The opening turn says what the run is up
		// against.
		r.appendSituation(s.renderSituation(ctx, r))
		r.takeSituation()
		conversation = r.conversation()
	} else if r.takeSituation() {
		// The workflow ran on by itself while the model was away: the
		// model reads the new situation before it decides again. The
		// report is a user turn, so the system prompt stays the same.
		r.appendSituation(s.renderSituation(ctx, r))
		conversation = r.conversation()
	}
	req := llm.Request{
		System:   s.systemPrompt(r),
		Messages: compact(conversation),
		Tools:    s.toolCatalogue(r),
	}
	resp, err := s.askModel(ctx, r, req)
	if err != nil {
		return false, err
	}
	if resp == nil {
		// The chat model did not answer: the run waits and asks
		// again.
		return false, nil
	}

	r.mu.Lock()
	r.system2Decisions++
	r.system2Usage.Add(resp.Usage)
	r.mu.Unlock()
	r.addUsage(resp.Usage)
	// The turn is counted before the run's events leave it: a chat
	// that hides them does not change what the run did.
	r.countThink(s.now())
	r.recordWake(wakeRecord{
		Who:   decisionChat,
		What:  toolCallsLine(resp.Message.ToolCalls()),
		Why:   r.judgmentReason(),
		Round: r.currentRound(),
		At:    s.now(),
	})
	// The model's own words are the decision's own: they go on the
	// decision's card, where the operator reads them.
	s.recordReply(ctx, r, resp.Message.Text())
	s.refreshRun(r)

	calls := resp.Message.ToolCalls()
	if len(calls) == 0 {
		// The model answered with words. The run does not end: the
		// agent's job is to keep the pipeline moving, and a turn that
		// produced no tool call is a turn it can try again.
		//
		// The words stay in the conversation: a model that is asked
		// again has to see what it just said.
		r.appendMessage(resp.Message)
		s.chargeStall(r, "the chat model answered without a tool call")
		s.refreshRun(r)
		return false, nil
	}

	r.appendMessage(resp.Message)
	pending := make([]pendingCall, 0, len(calls))
	for _, c := range calls {
		pending = append(pending, pendingCall{ID: c.ToolCallID, Name: c.ToolName, Args: c.Args})
	}
	r.setPendingCalls(pending)
	return false, nil
}

// askModel asks the chat model for one turn.
//
// A provider that fails is not fatal: the turn costs one chance
// against the step the judgment is about, and the run waits before it
// asks again. An answer that is not there at all is a provider that
// failed as well.
//
// It reports an error only when the run itself is over.
func (s *Service) askModel(ctx context.Context, r *run, req llm.Request) (*llm.Response, error) {
	execCtx, cancel := r.runContext(ctx)
	defer cancel()
	turnCtx, cancelTurn := context.WithTimeout(execCtx, maxAgentTurn)
	defer cancelTurn()

	s.refreshRun(r)
	defer func() {
		s.refreshRun(r)
	}()

	resp, err := s.cfg.Provider.Complete(turnCtx, req)
	if err == nil && resp == nil {
		err = errors.New("the chat model answered nothing")
	}
	if err == nil {
		r.resetProviderWait()
		return resp, nil
	}
	if endErr := execCtx.Err(); endErr != nil {
		// The run is over: the operator stopped it or the daemon went
		// away.
		return nil, endErr
	}
	s.logger.Warn("me: the chat model did not answer", "run_id", r.id, "err", err)
	s.chargeStall(r, "the chat model did not answer: "+err.Error())
	if err := s.waitBeforeRetry(ctx, r); err != nil {
		return nil, err
	}
	return nil, nil
}

// waitBeforeRetry waits before the run asks the chat model again.
//
// The wait grows: a provider that keeps failing is given more room.
// The wait belongs to the run: stopping the run or shutting the
// daemon ends it at once.
func (s *Service) waitBeforeRetry(ctx context.Context, r *run) error {
	wait := r.nextProviderWait()
	s.logger.Info("me: the chat model is asked again", "run_id", r.id, "wait", wait.String())
	execCtx, cancel := r.runContext(ctx)
	defer cancel()
	return s.cfg.Sleep(execCtx, wait)
}

// chargeStall records a turn that produced no action.
//
// The stall counts against the step the judgment was about: a
// judgment that names no step is charged to the last step that ran
// in the round.
//
// The situation moved on — the agent has one chance less — so the
// model reads the report again before it decides.
func (s *Service) chargeStall(r *run, summary string) {
	r.record(wfe.ActionKindStall, "", r.budgetStep(), wfe.Outcome{
		Status:  wfe.OutcomeUnknown,
		Summary: summary,
	})
	r.markSituation()
	s.persist(r)
}

// situationMark opens the situation report.
//
// The report is the user turn that tells the model why the run needs
// it; the mark is how the run finds it again in the conversation.
const situationMark = "The run needs you:"

// situationMessage builds the user turn that carries a situation
// report.
func situationMessage(text string) llm.Message {
	return llm.Message{
		Role:  llm.RoleUser,
		Parts: []llm.Part{{Type: llm.PartText, Text: text}},
	}
}

// compact shortens the conversation for the model.
//
// Every tool result but the last one keeps its status and its
// summary; the transcript — the long part — is dropped. The latest
// result keeps everything, because that is the one the next decision
// is about.
func compact(msgs []llm.Message) []llm.Message {
	last := -1
	for i, m := range msgs {
		if m.Role == llm.RoleTool {
			last = i
		}
	}
	if last < 0 {
		return msgs
	}
	out := make([]llm.Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		if i >= last || out[i].Role != llm.RoleTool {
			continue
		}
		// The parts are copied before they are rewritten: the
		// messages share their backing array with the run's
		// conversation, and the run keeps it.
		parts := make([]llm.Part, len(out[i].Parts))
		copy(parts, out[i].Parts)
		out[i].Parts = parts
		for j := range parts {
			if parts[j].Type != llm.PartToolResult {
				continue
			}
			parts[j].Text = compactResult(parts[j].Text)
		}
	}
	return out
}

// compactResult keeps the verdict of a tool result and drops the
// rest.
func compactResult(text string) string {
	var res stepResult
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		return text
	}
	short := stepResult{Status: res.Status, Summary: res.Summary}
	raw, err := json.Marshal(short)
	if err != nil {
		return text
	}
	return string(raw)
}
