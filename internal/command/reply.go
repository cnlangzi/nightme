package command

import (
	"context"

	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/messages"
)

// Reply builds a SlashOutput with the given text, marked as
// Consumed. The runtime shim consumes the SlashOutput and
// routes Reply / Outbound to cs.Emitter().Send.
//
// This is the ONE canonical reply helper for the
// OutCommandReply path — all one-shot system replies
// (/cwd /run /help /kill /agents / runtime errors) use it
// instead of constructing SlashOutput by hand. Keeping
// construction in one place lets us evolve the output shape
// (e.g. add per-reply metadata, drop metadata, log all
// replies) without touching every command.
//
// Returns only *SlashOutput (no error) so callers can do
//
//	return command.Reply(ctx, rt, "..."), nil
//
// matching the (*SlashOutput, error) signature of
// SlashCommandFactory.Handle. Callers that need to surface a
// failure construct the SlashOutput directly with a non-nil
// error.
//
// For per-turn continuations (/steer /queue /stop /use) prefer
// OutReply below — the hint folds into the agent's rolling-log
// card on every channel instead of producing a second bubble.
func Reply(_ context.Context, _ RuntimeServices, text string) *SlashOutput {
	return &SlashOutput{
		Reply:    text,
		Consumed: true,
	}
}

// OutReply builds a SlashOutput that emits `text` as a single
// OutReply message anchored to input.MessageID. Used by per-turn
// continuations (/steer /queue /stop) so the channel adapter
// folds the hint into the same rolling-log card the agent is
// streaming on — Feishu PATCHes the placeholder created at
// MessageQueued time, Slack streams into the same turnStream
// instead of a "❯ " standalone bubble. Mirrors /use's pattern
// (internal/command/use/cmd.go).
//
// ChatID and ReplyTo are pulled from input; the runtime policy
// chain stamps AgentName/Model/SessionID/Usage/GitStatus on
// the OutboundMessage before forwarding to the channel adapter.
func OutReply(input SlashInput, text string) *SlashOutput {
	return &SlashOutput{
		Consumed: true,
		Outbound: []messages.OutboundMessage{{
			ChatID:  input.ChatID,
			Kind:    messages.OutReply,
			Text:    text,
			ReplyTo: input.MessageID,
		}},
	}
}

// WithReplyTo is the command-level re-export of
// agentsession.WithReplyTo. /review uses it to stamp the /review
// slash command's message_id onto the AgentEvents the main agent
// emits in response to the injected review findings, so the chat
// channel folds those replies into the /review placeholder card
// instead of the prior user message's card.
//
// Re-exported (rather than having each command package import
// agentsession directly) to keep the command surface area visible
// in one place. The /review cmd holds the only current caller —
// the option is still variadic-friendly so future commands that
// inject content into a running AS can use the same shape without
// touching this file.
//
// Dependency direction: command → agentsession. agentsession
// only imports command/services (the EventBus helper package), not
// the command top-level package, so this re-export is acyclic.
func WithReplyTo(messageID string) agentsession.SendBlocksOption {
	return agentsession.WithReplyTo(messageID)
}
