package slack

import (
	"strings"
)

// chatIDPrefix namespaces Slack chat ids so a single daemon running
// Feishu + Telegram + Slack can key one flat chatsession store
// without collisions (docs/CHANNEL.md §5.5).
const chatIDPrefix = "sl_"

// sessionChatID builds the stable nightme chat id for a Slack
// conversation.
//
//	DM / channel top-level:  sl_<team>:<channel>
//	inside a thread:         sl_<team>:<channel>:<threadTS>
//
// The result is a PURE FUNCTION of its inputs — it never consults
// adapter state and never provokes Slack into creating a resource.
// Telegram violated this once by auto-creating a sentinel topic,
// which made the chat id depend on whether nightme had run before;
// the whole path had to be rewritten (see
// internal/channel/telegram/adapter.go:538-543).
func sessionChatID(teamID, channelID, threadTS string) string {
	if teamID == "" || channelID == "" {
		return ""
	}
	base := chatIDPrefix + teamID + ":" + channelID
	if threadTS != "" {
		return base + ":" + threadTS
	}
	return base
}

// splitSessionID is the inverse of sessionChatID. ok is false when
// the string is not a Slack chat id, which is how the adapter
// ignores chat ids belonging to other channels.
func splitSessionID(chatID string) (teamID, channelID, threadTS string, ok bool) {
	if !strings.HasPrefix(chatID, chatIDPrefix) {
		return "", "", "", false
	}
	rest := strings.TrimPrefix(chatID, chatIDPrefix)
	parts := strings.Split(rest, ":")
	switch len(parts) {
	case 2:
		if parts[0] == "" || parts[1] == "" {
			return "", "", "", false
		}
		return parts[0], parts[1], "", true
	case 3:
		if parts[0] == "" || parts[1] == "" || parts[2] == "" {
			return "", "", "", false
		}
		return parts[0], parts[1], parts[2], true
	default:
		return "", "", "", false
	}
}
