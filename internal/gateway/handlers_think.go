// F-think §3.1.2: `/think on|off` slash command handler.
//
// Toggles ChatSession.ThinkMode for the chat. The actual gate
// (drop OutThinking vs pass-through) lives in the runtime's
// EventHandler closure (cmd/nightme/run.go::newEventHandler),
// which reads cs.ThinkMode() after Translate + ReplyTo stamping
// and before ch.Send. This handler only mutates state and replies.
//
// State semantics:
//
//	/think on   → ThinkModeShow  (default; runtime forwards every
//	             OutThinking to the Channel, which renders it as
//	             a lark_md card in the user-message thread)
//	/think off  → ThinkModeHide  (runtime drops OutThinking at the
//	             EventHandler gate; no thread reply is posted)
//	/think      → reply with current mode + 1-line usage
//
// Concurrency: SetThinkMode takes the ChatSession mutex, writes
// to disk via persistChatEntry, then releases. We do not hold
// the lock across the channel.Send reply call.
//
// Like /watch, /think tolerates chats with no ChatSession yet
// (mgr.GetOrCreate lazily creates one). No /cwd is required —
// /think is a pure state mutation that does not depend on
// activeCwd.
//
// globalPrimary is passed through to GetOrCreate so the lazy
// ChatSession gets the same cfg.Primary snapshot as /cwd /use —
// keeping primaryAgent populated even when /think is the first
// command in a fresh chat.
package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// handleThink toggles ChatSession.ThinkMode for the current chat.
//
//	/think on           → set ThinkModeShow, persist, reply "thinking shown"
//	/think off          → set ThinkModeHide, persist, reply "thinking hidden"
//	/think              → reply current mode + usage
//	/think <other>      → reply usage hint (parse failure)
//
// Other OutboundKinds (OutReply, OutResult, OutToolStart,
// OutToolEnd, OutCompaction, OutInit, OutUsage) are not affected
// by /think off — only OutThinking is gated.
func handleThink(ctx context.Context, mgr *chatsession.Manager, channel Channel, msg *InboundMessage, args []string, globalPrimary string) (*CommandResult, error) {
	cs := mgr.GetOrCreate(msg.ChatID, globalPrimary)

	if len(args) < 1 {
		// No-arg form: report current state + brief usage hint.
		return reply(ctx, channel, msg.ChatID, fmt.Sprintf(
			"Current think mode: %s\nUsage: /think on | /think off",
			cs.ThinkMode(),
		)), nil
	}

	mode, ok := chatsession.ParseThinkMode(strings.TrimSpace(args[0]))
	if !ok {
		return reply(ctx, channel, msg.ChatID, fmt.Sprintf(
			"Unknown think mode %q. Usage: /think on | /think off",
			args[0],
		)), nil
	}

	if err := cs.SetThinkMode(mode); err != nil {
		return reply(ctx, channel, msg.ChatID, fmt.Sprintf("SetThinkMode failed: %v", err)), nil
	}

	replyText := fmt.Sprintf("Think mode set to %s.", mode)
	if mode == chatsession.ThinkModeShow {
		replyText += " Agent reasoning will appear in the message thread."
	} else {
		replyText += " Agent reasoning will be hidden; only final answers and tool summaries will be shown."
	}
	return reply(ctx, channel, msg.ChatID, replyText), nil
}