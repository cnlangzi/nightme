// F-think §3.1.2: per-chat thinking-content display toggle.
//
// ThinkMode controls whether the chat renders the agent's
// OutThinking events as Feishu thread cards (Show, default)
// or drops them silently at the outbound EventHandler gate
// (Hide).
//
// Lives in chatsession (next to its reader: ChatSession holds
// it, SetThinkMode mutates it, runtime's EventHandler gate
// consults it). The registry-side ChatSessionEntry persists
// it as a bare int so the registry package doesn't import
// this enum — see the field-level comment on ChatSessionEntry.
// ThinkMode.
//
// See docs/SPEC.md §3.1.2 for the design rationale.
package chatsession

// ThinkMode controls per-chat thinking-content visibility. The
// enum is ordered such that the zero value is the default
// ("show" — preserve the existing F-thread-route behavior);
// this matters because restored ChatSessions whose registry
// entry lacks the thinkMode field (older chat_sessions.json
// files) decode to ThinkModeShow via Go's zero-value
// semantics.
//
// Slash-command syntax:
//
//	/think on   → ThinkModeShow  (also accepts "show")
//	/think off  → ThinkModeHide  (also accepts "hide")
//	/think      → reply current mode + usage hint
//
// Unlike WatchMode (F-watch §3.1.1), ThinkMode is NOT chat-type-
// dependent: thinking is shown or hidden uniformly across DM
// and group chats. The Feishu channel renders OutThinking as a
// lark_md card in both modes; only the gate in the runtime's
// EventHandler closure decides whether to forward the event
// at all.
type ThinkMode int

const (
	// ThinkModeShow is the default: the runtime forwards every
	// OutThinking event to the Channel, which renders it as a
	// markdown card in the user-message thread.
	ThinkModeShow ThinkMode = iota

	// ThinkModeHide: the runtime drops OutThinking events at
	// the EventHandler gate (after Translate + ReplyTo
	// stamping, before ch.Send). Other OutboundKinds — OutReply,
	// OutResult, OutToolStart, OutToolEnd, OutInit, OutUsage —
	// are unaffected. State is persisted so /think off
	// survives daemon restart.
	ThinkModeHide
)

// String implements fmt.Stringer for log lines + reply text.
// Names are kept short and stable so they can appear in /think
// reply messages without ambiguity.
func (m ThinkMode) String() string {
	switch m {
	case ThinkModeHide:
		return "hide"
	case ThinkModeShow:
		return "show"
	default:
		return "unknown"
	}
}

// ParseThinkMode parses the slash-command arg into a ThinkMode.
// Accepts "on" / "show" as Show aliases and "off" / "hide" as
// Hide aliases so users can pick whichever is more memorable.
// Returns false for unknown values so the caller can reply with
// a usage hint without committing a state mutation.
func ParseThinkMode(s string) (ThinkMode, bool) {
	switch s {
	case "on", "show":
		return ThinkModeShow, true
	case "off", "hide":
		return ThinkModeHide, true
	default:
		return ThinkModeShow, false
	}
}
