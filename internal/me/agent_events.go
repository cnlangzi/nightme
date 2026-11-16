// agent_events.go — the run's own activity.
//
// The run's tool calls leave the card and travel the outbound path:
// the same path a coding agent's activity travels. The chat renders
// them the way it renders the agent's — a ● call / ⎿ result pair for
// a tool call — so the run reads like the agent it drives.
//
// What the chat model said is not such an event: it is the run's own
// words to the operator, and they go on the run's card. A reply is
// never gated: the run says what it has to say, whatever the chat
// shows of a coding agent's thinking.
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

	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/thinktext"
	"github.com/cnlangzi/nightme/internal/wfe"
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

// emit publishes an outbound message in the chat the run works in.
//
// The message hangs under the run's card: the card is the run's state
// board, and the thread under it is the run's live log. The anchor
// is the card of the iteration the run is in; a run that has no card
// yet publishes without one.
//
// A message that does not reach the chat is reported and the run goes
// on: the run's state lives on the card, not in the chat.
func (s *Service) emit(ctx context.Context, r *run, anchor string, msg messages.OutboundMessage) {
	if s.cfg.Operator == nil {
		return
	}
	msg.ChatID = r.chatID
	msg.ReplyTo = anchor
	if err := s.cfg.Operator.Emit(ctx, r.chatID, msg); err != nil {
		s.logger.Warn("me: the run's event could not be published",
			"run_id", r.id, "kind", msg.Kind.String(), "err", err)
	}
}

// recordReply keeps what the chat model said.
//
// The words are the run's own: they go on the run's card, where the
// operator reads them, and not into the chat as a thinking card of
// their own.
//
// A model that writes its scratchpad into its own text wraps it in
// <think>…</think> tags; the tags come off and the words stay.
func (s *Service) recordReply(r *run, text string) {
	text = strings.TrimSpace(thinktext.Unwrap(text))
	if text == "" {
		return
	}
	r.addReply(replyBlock{Text: clipReply(text), At: s.now()})
}

// countToolCall records a tool call the run made.
//
// The count is taken before the call leaves the run: a chat that
// hides the run's tool calls does not change what the run did.
func (s *Service) countToolCall(r *run) {
	r.countTool(s.now())
}

// refreshCard updates the card, at most once every
// cardRefreshInterval.
//
// The card follows the command the run waits for through the
// snapshots of the tracker. The snapshots are throttled: a run that
// is doing something writes to the chat now and then, not on every
// line the command produces.
//
// A snapshot means the command moved: the run is doing something, and
// the card says when it did it for the last time.
func (s *Service) refreshCard(r *run) {
	now := s.now()
	if !r.takeCardRefresh(now) {
		return
	}
	r.beat(now)
	s.updateCard(r)
}

// clipReply cuts a reply to what a card can show.
//
// The card carries the first lines of the reply and the rest is cut
// short: a card is read at a glance, and the reply is one turn of a
// conversation the operator does not read.
func clipReply(text string) string {
	lines := strings.Split(text, "\n")
	if len(lines) > maxReplyLines {
		lines = lines[:maxReplyLines]
	}
	return clipLine(strings.Join(lines, "\n"), maxReplyChars)
}

// publishToolStart publishes a tool call that is about to run.
//
// The anchor is the card the call hangs under: the call and its
// result belong under the same card, and the run may move to another
// iteration while the call runs.
func (s *Service) publishToolStart(ctx context.Context, r *run, anchor string, call pendingCall) {
	s.emit(ctx, r, anchor, messages.OutboundMessage{
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
func (s *Service) publishToolEnd(ctx context.Context, r *run, anchor string, call pendingCall, res stepResult, cause error) {
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
	s.emit(ctx, r, anchor, messages.OutboundMessage{
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
