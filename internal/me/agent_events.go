// agent_events.go — the run's own activity.
//
// The run speaks the standard outbound vocabulary: the same events a
// coding agent's turn speaks. The chat renders them the way it
// renders the agent's — a ● call / ⎿ result pair for a tool call, a
// 💭 line for the thinking, entries for the words — so a run reads
// like the agent it drives.
//
// Every event carries the anchor of the session it belongs to: the
// message the session's events hang under. The chat patches the card
// of that message in place, so a session owns exactly one card.
//
// The counts of what the run did are taken here, before the events
// leave the run: a chat that hides them does not change what the run
// did.
//
// The package speaks the abstract outbound vocabulary
// (internal/messages) and not the agent protocol: it does not depend
// on the agent layer.
package me

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/thinktext"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// maxEventLine is how much of an argument line the chat sees. The
// line is for a person: the model's raw JSON is a wall of escaped
// text.
const maxEventLine = 300

// cardRefreshInterval is how long the run waits between two card
// updates of its own.
//
// The snapshots of the command the run waits for come at the pace of
// that command: without a throttle the card would be written on
// every line the command produces.
const cardRefreshInterval = 5 * time.Second

// emit publishes an outbound message for a session.
//
// The message hangs under the session's anchor: the anchor is the
// session's state board, and the thread under it is the session's
// live log. A session without an anchor publishes without one, and
// the chat falls back to its orphan handling.
//
// A message that does not reach the chat is reported and the run goes
// on: the run's state lives in the chat, not in the run.
func (s *Service) emit(ctx context.Context, r *run, sess *session, msg messages.OutboundMessage) {
	if s.cfg.Operator == nil {
		return
	}
	msg.ChatID = r.chatID
	if sess != nil {
		msg.ReplyTo = sess.anchor
		msg.SessionID = sess.id
		msg.AgentName = meAgentName
		msg.Model = r.modelName()
		msg.Usage = usageInfo(r.sessionUsage(sess))
		msg.GitStatus = r.gitStatusValue()
	}
	if err := s.cfg.Operator.Emit(ctx, r.chatID, msg); err != nil {
		s.logger.Warn("me: the run's event could not be published",
			"run_id", r.id, "kind", msg.Kind.String(), "err", err)
	}
}

// usageInfo renders what a session spent as the footer's usage line.
//
// A session that spent nothing has no usage line.
func usageInfo(usage llm.Usage) *messages.UsageInfo {
	if usage.Total() == 0 {
		return nil
	}
	return &messages.UsageInfo{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
	}
}

// recordReply publishes what the chat model said.
//
// The words are the decision's own: they go on the decision's card,
// where the operator reads them. Every reply is shown: the card is
// the record of the decision.
//
// A model that writes its scratchpad into its own text wraps it in
// <think>…</think> tags: the thinking comes off and becomes a
// thinking event of its own, and the reply carries the words alone.
func (s *Service) recordReply(ctx context.Context, r *run, text string) {
	sess := r.decisionSessionRef()
	if sess == nil {
		return
	}
	thinking, reply := thinktext.SplitText(text)
	if t := strings.TrimSpace(thinking); t != "" {
		s.emit(ctx, r, sess, messages.OutboundMessage{
			Kind: messages.OutThinking,
			Text: t,
		})
	}
	if strings.TrimSpace(reply) == "" {
		return
	}
	s.publishReply(ctx, r, sess, strings.TrimSpace(reply))
}

// countToolCall records a tool call the run made.
//
// The count is taken before the call leaves the run: a chat that
// hides the run's tool calls does not change what the run did.
func (s *Service) countToolCall(ctx context.Context, r *run) {
	r.countTool(s.now())
	s.publishHeartbeat(ctx, r)
}

// refreshSessions republishes the state of the round the run is in.
//
// The checklist of the round follows the run's progress: a step that
// ran shows up on the card of the round it belongs to.
func (s *Service) refreshSessions(r *run) {
	now := s.now()
	if !r.takeCardRefresh(now) {
		return
	}
	r.beat(now)
	ctx := context.WithoutCancel(r.ctx)
	r.setGitStatus(s.gitStatus(r))
	s.publishHeartbeat(ctx, r)
	sess := r.roundSessionRef()
	if sess != nil {
		s.publishTasks(ctx, r, sess, r.roundTasks(sess.job, sess.round))
	}
}

// publishToolStart publishes a tool call that is about to run.
//
// The anchor is the card the call hangs under: the call and its
// result belong under the same card, and the run may move on while
// the call runs.
func (s *Service) publishToolStart(ctx context.Context, r *run, sess *session, call pendingCall) {
	s.emit(ctx, r, sess, messages.OutboundMessage{
		Kind: messages.OutToolStart,
		Tool: &messages.ToolInfo{
			Name: call.Name,
			Args: toolArgLine(r, call),
		},
	})
}

// publishToolEnd publishes a tool call that settled.
//
// The result is the call's verdict: the chat shows it as it is. A
// call that failed carries its failure as the error — the chat
// renders the error, and a failed call must not read as one that
// went through.
func (s *Service) publishToolEnd(ctx context.Context, r *run, sess *session, call pendingCall, res stepResult, cause error) {
	tool := &messages.ToolInfo{
		Name: call.Name,
		Args: toolArgLine(r, call),
	}
	switch {
	case cause != nil:
		tool.Err = cause
	case outcomeFailed(res.Status):
		tool.Err = errors.New(res.Summary)
	default:
		tool.Summary = res.Summary
	}
	s.emit(ctx, r, sess, messages.OutboundMessage{
		Kind: messages.OutToolEnd,
		Tool: tool,
	})
}

// outcomeFailed reports whether a tool result is a failure.
//
// The verdict of a call that did not go through is an error, not a
// summary: the chat renders the two differently.
func outcomeFailed(status string) bool {
	switch status {
	case string(wfe.OutcomeFailed), string(wfe.OutcomeCancelled), string(wfe.OutcomeTimeout), "error":
		return true
	}
	return false
}

// toolArgLine renders the one-line summary of a tool call's
// arguments.
//
// The line is for a person: the chat shows the argument as it is,
// and the model's raw JSON is unreadable there. A tool the run does
// not know carries no line at all.
func toolArgLine(r *run, call pendingCall) string {
	switch call.Name {
	case toolRunStep:
		return clipLine(argText(call, "step_id"), maxEventLine)
	case toolInstruct:
		return clipLine(argText(call, "text"), maxEventLine)
	case toolAskHuman:
		return clipLine(argText(call, "question"), maxEventLine)
	case toolAgent:
		return clipLine(argText(call, "task"), maxEventLine)
	case toolFinish:
		return clipLine(finishArgLine(call), maxEventLine)
	case toolAnswerChoice:
		return clipLine(strings.TrimSpace(argText(call, "choice_id")+" "+argText(call, "option_id")), maxEventLine)
	case toolRepoStatus:
		return ""
	}
	cmd, ok := r.commandFor(call.Name)
	if !ok {
		return ""
	}
	line := cmd.Name
	if sub, _ := callArgOptional(call, "subcommand"); sub != "" {
		line += " " + sub
	}
	if args, _ := callArgOptional(call, "args"); args != "" {
		line += " " + args
	}
	return clipLine(line, maxEventLine)
}

// finishArgLine renders the arguments of the finish tool: how the run
// ended and what it did.
func finishArgLine(call pendingCall) string {
	status := argText(call, "status")
	summary := argText(call, "summary")
	switch {
	case summary == "":
		return status
	case status == "":
		return summary
	}
	return status + ": " + summary
}

// argText reads a string argument of a tool call.
//
// An argument that is not there reads as empty: the line says what
// the call carried, and a call without the argument has nothing to
// say about it.
func argText(call pendingCall, name string) string {
	text, _ := callArgOptional(call, name)
	return text
}

// clipLine shortens a line to n characters, the ellipsis included.
//
// The line is read in a chat: it must not run away with the message
// that carries it.
func clipLine(text string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n-1]) + "…"
}
