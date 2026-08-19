// Package queue implements the `/queue <message>` slash command.
//
// /queue appends <message> to the tail of the input queue as a
// MessageKindQueue barrier. Compared to sibling commands:
//
//   - plain text — appended as MessageKindNormal; merged with
//     adjacent Normal runs into one Prompt batch by MessageQueue.Peek.
//   - /steer     — stops the in-flight turn (via the bridge's
//     Stop primitive) AND prepends to the head of the queue;
//     Kind=Normal (merges with the next drained batch).
//   - /queue     — appends to the tail; Kind=Queue (its own
//     discrete Prompt batch — Peek returns it alone, and it
//     terminates any Normal run still in the pending region).
//     Does NOT abort the in-flight turn.
//
// Use /queue when you explicitly want a standalone Prompt —
// scheduled / cron deliveries, "execute this now" instructions,
// any message that should never be batched with whatever came
// before it.
//
// Implementation:
//
//   - ChatSession.QueueUserMessage is a thin Push wrapper that
//     does NOT inspect msg.Kind; BuildMessage (below) is this
//     package's sole construction site for Kind=MessageKindQueue
//     messages. Mirrors /steer's contract: SteerUserMessage also
//     does not inspect Kind, so /steer keeps its Message
//     construction inline in Handle. /queue extracts to a
//     package-level function so tests can assert on the
//     constructed Message directly (see cmd_test.go's
//     TestBuildMessage_SetsBarrierKind).
//   - No Stop — /queue is non-interrupting by design.
//   - The framework commander layer
//     (internal/command/commander.go Dispatch) wraps every
//     matched slash command with the MessageQueued → MessageDone
//     pair, so this factory does NOT emit those states manually.
//   - Reply preview uses command.PreviewForIM (shared with
//     /steer) so the IM-card truncation contract stays in one
//     place — Feishu truncates around 4 KB but the visible
//     header is much shorter; ~80 runes keeps one short
//     sentence visible while leaving room for the emoji +
//     label + ellipsis.
//
// Factory holds *chatsession.Manager directly.
package queue

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// Factory is the command.SlashCommandFactory for /queue.
type Factory struct {}

// NewFactory constructs a Factory. command/* factories do not
// receive a *chatsession.Manager — cs comes from the dispatcher
// parameter at Handle time.
func init() {
	command.RegisterBuilder(func(d command.Deps) command.SlashCommandFactory {
		return NewFactory()
	})
}

func NewFactory() *Factory {
	return &Factory{}
}

// BuildMessage constructs the Message that Handle will enqueue
// for the given SlashInput + body. Exposed as a package-level
// function so tests can assert on the constructed Message
// (Kind, ID, Blocks) directly, without reaching into ChatSession's
// private queue linked list. The /queue factory is the only
// caller; ChatSession's own /queue entrypoint is intentional —
// Kind is the whole point of the command.
//
// ReceivedAt is set to time.Now(); tests should not pin its exact
// value (use IsZero / non-zero assertions, or compare within a
// tolerance window).
func BuildMessage(input command.SlashInput, body string) chatsession.Message {
	return chatsession.Message{
		ID:         input.MessageID,
		ChatID:     input.ChatID,
		Blocks:     []agent.ContentBlock{{Type: agent.ContentText, Text: body}},
		Kind:       chatsession.MessageKindQueue, // ← barrier
		ReceivedAt: time.Now(),
	}
}

// Spec implements command.SlashCommandFactory.
func (f *Factory) Spec() command.Spec {
	return command.Spec{
		Name:     "queue",
		Summary:  "Append <message> to the queue as a standalone Prompt.",
		Usage:    "/queue <message>",
		Category: "session",
	}
}

// Handle implements command.SlashCommandFactory.
//
// Flow:
//  1. Look up the ChatSession for this chat. Reject if absent.
//  2. RequireActiveCwd preflight (every cmd preflights its own).
//  3. Empty MessageID guard (QueueUserMessage silently no-ops
//     on msg.ID == "" — surface that as a non-success reply so
//     the user doesn't get a "Queued:" confirmation for a
//     nothing-pushed).
//  4. Parse trailing args as the queue body. Empty trailing is a
//     usage error (no silent fallback to follow-up).
//  5. Build the Message via BuildMessage (Kind=MessageKindQueue).
//  6. Call cs.QueueUserMessage (Kind is preserved through Push).
//  7. Reply with a short preview, emoji-prefixed.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

	if cs == nil {
		return command.Reply(ctx, rt, "No active chat session."), nil
	}
	if _, failOut := command.RequireActiveCwd(cs); failOut != nil {
		return failOut, nil
	}

	// Empty MessageID guard. ChatSession.QueueUserMessage silently
	// no-ops on msg.ID == "" (see internal/chatsession/chatsession.go
	// QueueUserMessage), and the framework commander guards its
	// ⏳→✅ emission against an empty MessageID — so without this
	// guard, a synthetic inbound with no MessageID would receive a
	// success reply while enqueuing nothing. /steer has the same
	// exposure but additionally fires an async Stop, which surfaces
	// the wiring; /queue is silent in that case. Surface it here.
	if input.MessageID == "" {
		return command.Reply(ctx, rt,
			"Internal: missing message id; /queue did not enqueue."), nil
	}

	// Trailing args → queue body. Using Args[1:] (rather than
	// re-parsing input.Text) matches the commander flow — both
	// half-width "/" and full-width "／" prefixes are handled
	// upstream, and the whitespace split is the same one the
	// dispatcher used to populate Args.
	body := strings.TrimSpace(strings.Join(input.Args[1:], " "))
	if body == "" {
		return command.Reply(ctx, rt, "Usage: /queue <message>"), nil
	}

	msg := BuildMessage(input, body)

	// /queue does NOT emit MessageQueued here. The framework
	// commander layer (internal/command/commander.go Dispatch)
	// wraps every matched slash command with the MessageQueued →
	// MessageDone pair automatically, so this manual emission is
	// redundant and would be dropped by the feishu adapter's LRU
	// dedup anyway. See internal/command/commander.go Dispatch
	// (the framework ⏳→✅ wrapping contract) and
	// docs/feat/slash-command-reactions.md for the full design.

	if err := cs.QueueUserMessage(msg); err != nil {
		return command.Reply(ctx, rt, fmt.Sprintf("Queue failed: %v", err)), nil
	}

	// Reply with a short preview of the queued body (truncated
	// at rune boundary — see command.PreviewForIM for the
	// multi-byte UTF-8 safety rationale).
	return command.Reply(ctx, rt,
		fmt.Sprintf("📥 Queued: %s", command.PreviewForIM(body))), nil
}