// F-38 §3.1.3: `/tools on|off` slash command handler.
//
// Toggles ChatSession.ToolsMode for the chat. The actual gate
// (drop OutToolStart / OutToolEnd vs pass-through) lives in the
// runtime's EventHandler closure (cmd/nightme/run.go::newEventHandler),
// which reads cs.ToolsMode() after Translate + ReplyTo stamping
// and before ch.Send. This handler only mutates state and replies.
//
// State semantics:
//
//	/tools on   → ToolsModeShow  (runtime forwards OutToolStart /
//	             OutToolEnd to the Channel; Feishu adapter merges
//	             each pair into a single thread reply via PATCH on
//	             the start message_id — see
//	             internal/channel/feishu/tool_thread_merge.go)
//	/tools off  → ToolsModeHide  (runtime drops OutToolStart and
//	             OutToolEnd at the EventHandler gate; no thread
//	             reply is posted)
//	/tools      → reply with current mode + 1-line usage
//
// Concurrency: SetToolsMode takes the ChatSession mutex, writes
// to disk via persistChatEntry, then releases. We do not hold
// the lock across the channel.Send reply call.
//
// Like /watch and /think, /tools tolerates chats with no
// ChatSession yet (mgr.GetOrCreate lazily creates one). No /cwd
// is required — /tools is a pure state mutation that does not
// depend on activeCwd.
//
// Default direction is OPPOSITE of /think: /think defaults to
// ThinkModeShow (preserve existing F-thread-route behavior);
// /tools defaults to ToolsModeHide (quiet by default; users opt
// in to see tool calls).
//
// globalPrimary is passed through to GetOrCreate so the lazy
// ChatSession gets the same cfg.Primary snapshot as /cwd /use —
// keeping primaryAgent populated even when /tools is the first
// command in a fresh chat.
package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// handleTools toggles ChatSession.ToolsMode for the current chat.
//
//	/tools on           → set ToolsModeShow, persist, reply "tools shown"
//	/tools off          → set ToolsModeHide, persist, reply "tools hidden"
//	/tools              → reply current mode + usage
//	/tools <other>      → reply usage hint (parse failure)
//
// Other OutboundKinds (OutText, OutResult, OutThinking, OutCompaction,
// OutInit, OutUsage) are not affected by /tools off — only
// OutToolStart and OutToolEnd are gated.
func handleTools(ctx context.Context, mgr *chatsession.Manager, channel Channel, msg *InboundMessage, args []string, globalPrimary string) (*CommandResult, error) {
	cs := mgr.GetOrCreate(msg.ChatID, globalPrimary)

	if len(args) < 1 {
		// No-arg form: report current state + brief usage hint.
		return reply(ctx, channel, msg.ChatID, fmt.Sprintf(
			"Current tools mode: %s\nUsage: /tools on | /tools off",
			cs.ToolsMode(),
		)), nil
	}

	mode, ok := chatsession.ParseToolsMode(strings.TrimSpace(args[0]))
	if !ok {
		return reply(ctx, channel, msg.ChatID, fmt.Sprintf(
			"Unknown tools mode %q. Usage: /tools on | /tools off",
			args[0],
		)), nil
	}

	if err := cs.SetToolsMode(mode); err != nil {
		return reply(ctx, channel, msg.ChatID, fmt.Sprintf("SetToolsMode failed: %v", err)), nil
	}

	replyText := fmt.Sprintf("Tools mode set to %s.", mode)
	if mode == chatsession.ToolsModeShow {
		replyText += " Tool calls will appear in the message thread (one reply per tool, call + result merged)."
	} else {
		replyText += " Tool calls will be hidden; only the final answer will be shown."
	}
	return reply(ctx, channel, msg.ChatID, replyText), nil
}
