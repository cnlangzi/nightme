// F-watch §3.1.1: `/watch on|off` slash command handler.
//
// Toggles ChatSession.WatchMode for the chat. The actual gate
// (drop non-mention messages vs pass-through) is in
// gateway.Handle — this handler only mutates state and replies.
//
// State semantics:
//   - /watch on   → WatchModeAll (process every chat message)
//   - /watch off  → WatchModeMention (only @ bot / @_all, default)
//   - /watch      → reply with current mode + 1-line usage
//
// DM behavior: the gate itself is a no-op in DM (Message.HasMention
// is always true), so /watch on/off has no observable effect in
// DM chats. The state is still written and persisted so that
// switching from DM to group preserves the user's last-set
// preference. Reply text mentions this so users aren't surprised.
//
// Concurrency: SetWatchMode takes the ChatSession mutex, writes
// to disk via persistChatEntry, then releases. We do not hold
// the lock across the channel.Send reply call.
package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
)

// handleWatch toggles ChatSession.WatchMode for the current chat.
//
//	/watch on           → set WatchModeAll, persist, reply "watching all"
//	/watch off          → set WatchModeMention, persist, reply "watching mentions only"
//	/watch              → reply current mode + usage
//	/watch <other>      → reply usage hint (parse failure)
//
// The handler tolerates chats with no ChatSession yet
// (mgr.GetOrCreate lazily creates one). No /cwd is required —
// /watch is a pure state mutation that does not depend on
// activeCwd (it's about message-watching, not agent dispatch).
//
// globalPrimary is passed through to GetOrCreate so the lazy
// ChatSession gets the same cfg.Primary snapshot as /cwd /use —
// keeping primaryAgent populated even when /watch is the first
// command in a fresh chat.
func handleWatch(ctx context.Context, mgr *chatsession.Manager, channel Channel, msg *InboundMessage, args []string, globalPrimary string) (*CommandResult, error) {
	cs := mgr.GetOrCreate(msg.ChatID, globalPrimary)

	if len(args) < 1 {
		// No-arg form: report current state + brief usage hint.
		return reply(ctx, channel, msg.ChatID, fmt.Sprintf(
			"Current watch mode: %s\nUsage: /watch on | /watch off\n"+
				"Note: in DMs this is a no-op — every DM message is processed regardless.",
			cs.WatchMode(),
		)), nil
	}

	mode, ok := chatsession.ParseWatchMode(strings.TrimSpace(args[0]))
	if !ok {
		return reply(ctx, channel, msg.ChatID, fmt.Sprintf(
			"Unknown watch mode %q. Usage: /watch on | /watch off",
			args[0],
		)), nil
	}

	if err := cs.SetWatchMode(mode); err != nil {
		return reply(ctx, channel, msg.ChatID, fmt.Sprintf("SetWatchMode failed: %v", err)), nil
	}

	replyText := fmt.Sprintf("Watch mode set to %s.", mode)
	if mode == chatsession.WatchModeAll {
		replyText += " I will now process every message in this chat."
	} else {
		replyText += " I will only process messages that @ me or @_all."
	}
	return reply(ctx, channel, msg.ChatID, replyText), nil
}
