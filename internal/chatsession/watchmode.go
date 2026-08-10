// F-watch §3.1.1: per-chat message-watch mode.
//
// WatchMode controls whether the chat processes every incoming
// message or only those that mention the bot (or @_all). The
// decision is consulted by chatsession.Manager.AcceptInbound
// when a non-mention group message arrives (see docs/SPEC.md
// §3.1.1 + docs/channel/feishu.md §6.11).
//
// DM chats ignore this value entirely at the gate: the channel
// adapter sets Message.HasMention=true for every DM message
// (every DM is implicitly "addressed to bot"), so the
// `!HasMention && WatchMode != WatchModeAll` check never
// triggers for DM. The mode value still persists across
// chat-type changes so switching from group to DM and back
// preserves the user's last-set preference.
//
// Lives in chatsession (next to its reader: ChatSession holds
// it, SetWatchMode mutates it, Manager.AcceptInbound gates on
// it). Previously the gate lived in gateway via a callback
// injection (gateway.WithWatchModeResolver) — relocated here for
// functional cohesion so the policy sits next to its state. The
// registry-side ChatSessionEntry persists this as a bare int so
// the registry package doesn't import this enum — see the
// field-level comment on ChatSessionEntry.WatchMode.
package chatsession

// WatchMode controls per-chat message-watch behavior. The enum
// is ordered such that the zero value is the conservative default
// (only @ bot / @_all messages processed); this matters because
// restored ChatSessions whose registry entry lacks the watchMode
// field (older chat_sessions.json files) default to this safe
// behavior via Go's zero-value semantics.
type WatchMode int

const (
	// WatchModeMention is the default: the chat only processes
	// messages that @ the bot or @_all. Other group members'
	// messages are dropped at the gateway dispatcher gate.
	WatchModeMention WatchMode = iota

	// WatchModeAll: process every message in the chat regardless
	// of @-mention. Toggled on per-chat by the `/watch on` slash
	// command.
	WatchModeAll
)

// String implements fmt.Stringer for log lines + reply text.
// Names are kept short and stable so they can appear in
// `/watch` reply messages without ambiguity.
func (m WatchMode) String() string {
	switch m {
	case WatchModeAll:
		return "all"
	case WatchModeMention:
		return "mention"
	default:
		return "unknown"
	}
}

// ParseWatchMode parses the slash-command arg into a WatchMode.
// Returns false for unknown values so the caller can reply with
// a usage hint.
func ParseWatchMode(s string) (WatchMode, bool) {
	switch s {
	case "on", "all":
		return WatchModeAll, true
	case "off", "mention":
		return WatchModeMention, true
	default:
		return WatchModeMention, false
	}
}
