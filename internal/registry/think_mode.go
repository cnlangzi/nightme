// F-think §3.1.2: per-chat thinking-content display toggle.
//
// ThinkMode is a small enum ChatSession holds to indicate whether
// the chat should render the agent's OutThinking events as Feishu
// thread cards (Show, default) or drop them silently at the
// outbound EventHandler gate (Hide).
//
// Defined in package registry (not chatsession) so that
// registry.ChatSessionEntry can carry it without an import cycle.
// The chatsession package uses this type directly via the thin
// alias in internal/chatsession/thinkmode.go.
//
// See docs/SPEC.md §3.1.2 for the design rationale.
package registry

// ThinkMode controls per-chat thinking-content visibility. The enum
// is ordered such that the zero value is the default ("show"
// — preserve the existing F-thread-route behavior); this matters
// because restored ChatSessions whose registry entry lacks the
// thinkMode field (older chat_sessions.json files) decode to
// ThinkModeShow via Go's zero-value semantics.
//
// Slash-command syntax:
//
//	/think on   → ThinkModeShow  (also accepts "show")
//	/think off  → ThinkModeHide  (also accepts "hide")
//	/think      → reply current mode + usage hint
//
// Unlike WatchMode (F-watch §3.1.1), ThinkMode is NOT chat-type-
// dependent: thinking is shown or hidden uniformly across DM and
// group chats. The Feishu channel renders OutThinking as a
// lark_md card in both modes; only the gate in the runtime's
// EventHandler closure decides whether to forward the event at
// all.
type ThinkMode int

const (
	// ThinkModeShow is the default: the runtime forwards every
	// OutThinking event to the Channel, which renders it as a
	// markdown card in the user-message thread.
	ThinkModeShow ThinkMode = iota

	// ThinkModeHide: the runtime drops OutThinking events at the
	// EventHandler gate (after Translate + ReplyTo stamping,
	// before ch.Send). Other OutboundKinds — OutReply, OutResult,
	// OutToolStart, OutToolEnd, OutCompaction, OutInit, OutUsage —
	// are unaffected. State is persisted so /think off survives
	// daemon restart.
	ThinkModeHide
)

// String implements fmt.Stringer for log lines + reply text.
// Names are kept short and stable so they can appear in /think
// reply messages without ambiguity.
func (m ThinkMode) String() string {
	switch m {
	case ThinkModeShow:
		return "show"
	case ThinkModeHide:
		return "hide"
	default:
		return "unknown"
	}
}

// ParseThinkMode parses the slash-command arg into a ThinkMode.
// Accepts "on" / "show" as Show aliases and "off" / "hide" as Hide
// aliases so users can pick whichever is more memorable. Returns
// false for unknown values so the caller can reply with a usage
// hint without committing a state mutation.
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