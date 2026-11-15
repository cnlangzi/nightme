// agent_events.go — the run's own activity in the chat.
//
// The run's thinking and the run's tool calls leave the card and
// travel the outbound path: the same path a coding agent's activity
// travels. The chat renders them the way it renders the agent's —
// a 💭 card for the thinking, a ● call / ⎿ result pair for a tool
// call — so the run reads like the agent it drives.
//
// The card keeps the state. What the model said is not state: it is
// the run's reasoning, and it belongs next to the work, not on the
// board that carries the state.
//
// The package speaks the abstract outbound vocabulary
// (internal/messages) and not the agent protocol: it does not depend
// on the agent layer.
package me

import (
	"context"
	"strings"

	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/thinktext"
)

// maxEventLine is how much of an argument line or a summary the chat
// sees. The line is for a person: the model's raw JSON is a wall of
// escaped text, and the full text is on the card.
const maxEventLine = 300

// emit publishes an outbound message in the chat the run works in.
//
// The message hangs under the run's card: the card is the run's state
// board, and the thread under it is the run's live log. A run that
// has no card yet publishes without an anchor.
//
// A message that does not reach the chat is reported and the run goes
// on: the run's state lives on the card, not in the chat.
func (s *Service) emit(ctx context.Context, r *run, msg messages.OutboundMessage) {
	if s.cfg.Operator == nil {
		return
	}
	msg.ChatID = r.chatID
	msg.ReplyTo = r.cardMessageID()
	if err := s.cfg.Operator.Emit(ctx, r.chatID, msg); err != nil {
		s.logger.Warn("me: the run's event could not be published",
			"run_id", r.id, "kind", msg.Kind.String(), "err", err)
	}
}

// publishThinking publishes what the chat model said.
//
// A model that writes its scratchpad into its own text wraps it in
// <think>…</think> tags; the tags come off and the reasoning stays.
func (s *Service) publishThinking(ctx context.Context, r *run, text string) {
	text = strings.TrimSpace(thinktext.Unwrap(text))
	if text == "" {
		return
	}
	s.emit(ctx, r, messages.OutboundMessage{
		Kind: messages.OutThinking,
		Text: text,
	})
}

// publishToolStart publishes a tool call that is about to run.
func (s *Service) publishToolStart(ctx context.Context, r *run, call pendingCall) {
	s.emit(ctx, r, messages.OutboundMessage{
		Kind: messages.OutToolStart,
		Tool: &messages.ToolInfo{
			Name: call.Name,
			Args: toolArgLine(r, call),
		},
	})
}

// publishToolEnd publishes a tool call that settled.
//
// A call that failed carries its failure in the summary: the chat
// renders the summary, not the error.
func (s *Service) publishToolEnd(ctx context.Context, r *run, call pendingCall, summary string) {
	s.emit(ctx, r, messages.OutboundMessage{
		Kind: messages.OutToolEnd,
		Tool: &messages.ToolInfo{
			Name:   call.Name,
			Args:   toolArgLine(r, call),
			Output: summary,
		},
	})
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
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	if n <= 1 {
		return "…"
	}
	return string(runes[:n-1]) + "…"
}
