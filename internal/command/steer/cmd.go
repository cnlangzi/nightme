// Package steer implements the `/steer <message>` slash command.
//
// /steer is "stop + redirect": it tries to abort the agent's
// in-flight turn (via the bridge's Stop primitive) and prepends
// the new message to the head of the input queue. The steered
// message becomes the first thing the agent sees on the next
// turn — even if other user messages had piled up in the queue
// while the current turn was running.
//
// Compare to sibling commands:
//
//   - /stop  — signals the bridge to halt the in-flight turn;
//     does NOT enqueue a new message.
//   - /close — forcibly terminates the bridge process (graceful);
//     does NOT enqueue a new message; preserves session
//     identity for respawn.
//   - /new   — invokes the bridge's in-place context reset
//     (claudecode's `/clear`, pi's `new_session`, etc.).
//     Does NOT abort the in-flight turn or enqueue a
//     new message.
//   - /steer — aborts the in-flight turn AND prepends a new
//     message to the queue. The new message takes
//     priority over any already-queued user input.
//
// Implementation:
//
//   - SteerUserMessage on ChatSession does the actual work
//     (Stop + PushFront). This package just builds a Message
//     from the slash command args and calls it.
//   - Stop's outcome is best-effort; PushFront always runs.
//   - The Message.Kind is MessageKindNormal so it merges with
//     any contiguous Normal run in the queue and is dispatched
//     in the same FlushHook batch.
//
// Factory holds *chatsession.Manager directly.
package steer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// Factory is the command.SlashCommandFactory for /steer.
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

// Spec implements command.SlashCommandFactory.
func (f *Factory) Spec() command.Spec {
	return command.Spec{
		Name:     "steer",
		Summary:  "Stop the in-flight turn and prepend <message> to the queue.",
		Usage:    "/steer <message>",
		Category: "session",
	}
}

// Handle implements command.SlashCommandFactory.
//
// Flow:
//  1. Look up the ChatSession for this chat. Reject if absent.
//  2. RequireActiveCwd preflight (every cmd preflights its own).
//  3. Parse trailing args as the steer message. Empty trailing
//     is a usage error (no silent fallback to follow-up).
//  4. Emit MessageQueued BEFORE SteerUserMessage (same timing
//     contract as QueueUserMessage).
//  5. Call cs.SteerUserMessage.
//  6. Reply with a short confirmation that names the steered
//     message (truncated for IM card legibility).
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

	if cs == nil {
		return command.Reply(ctx, rt, "No active chat session."), nil
	}
	if _, failOut := command.RequireActiveCwd(cs); failOut != nil {
		return failOut, nil
	}

	// Trailing args → steer body. Using Args[1:] (rather than
	// re-parsing input.Text) matches the commander flow — both
	// half-width "/" and full-width "／" prefixes are handled
	// upstream, and the whitespace split is the same one the
	// dispatcher used to populate Args.
	//
	// Issue #291 exemption: /steer deliberately does NOT go
	// through command.ParseCmdArgs. Its payload is free-form
	// prose destined for the agent, so a leading "-" is data,
	// not a flag ("/steer --wait for CI to go green" must reach
	// the agent verbatim). The only arity rule that makes sense
	// here is "non-empty", which is the check below. If /steer
	// ever needs a real flag, adopt ParseCmdArgs with
	// MaxArgs: command.UnboundedArgs and require the `--`
	// terminator before the prose.
	body := strings.TrimSpace(strings.Join(input.Args[1:], " "))
	if body == "" {
		return command.Reply(ctx, rt, "Usage: /steer <message>"), nil
	}

	msg := chatsession.Message{
		ID:         input.MessageID,
		ChatID:     input.ChatID,
		Blocks:     []agent.ContentBlock{{Type: agent.ContentText, Text: body}},
		Kind:       chatsession.MessageKindNormal,
		ReceivedAt: time.Now(),
	}

	// /steer used to emit cs.EmitMessageState(input.MessageID,
	// agent.MessageQueued) here. The framework commander layer
	// (internal/command/commander.go Dispatch) now wraps every
	// matched slash command with the MessageQueued → MessageDone
	// pair automatically, so this manual emission is redundant and
	// would be dropped by the feishu adapter's LRU dedup anyway.
	// Removed for clarity — see docs/feat/slash-command-reactions.md.

	if err := cs.SteerUserMessage(msg); err != nil {
		return command.Reply(ctx, rt, fmt.Sprintf("Steer failed: %v", err)), nil
	}

	// Reply with a short preview of the steered body (truncated
	// at rune boundary — see command.PreviewForIM for the
	// multi-byte UTF-8 safety rationale).
	return command.Reply(ctx, rt,
		fmt.Sprintf("🛑 Steering: %s", command.PreviewForIM(body))), nil
}
