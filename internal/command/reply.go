package command

import (
	"context"

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
