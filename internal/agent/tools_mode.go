// F-38 §3.1.3: per-chat tool-event display toggle.
//
// ToolsMode is a small enum ChatSession holds to indicate whether
// the chat should render tool calls (OutToolStart / OutToolEnd) as
// thread replies under the user message, or drop them silently at
// the outbound EventHandler gate.
//
// The merge behaviour (a single thread reply carrying both the
// `● Tool(args)` call line and the `⎿  …` result line via PATCH on
// the same Feishu message_id) is a Channel-internal rendering
// choice — see docs/channel/feishu.md §13.14 and the merge helper
// in internal/channel/feishu/tool_thread_merge.go. The toggle is
// a per-chat on/off; the rendering decision is the Feishu
// adapter's.
//
// Unlike ThinkMode (F-think §3.1.2) — whose default is Show to
// preserve the existing F-thread-route behaviour — ToolsMode
// defaults to Hide. Rationale: tool spam is the loudest part of
// the agent progress stream and most users do not want it by
// default. Users who care opt in via /tools on. Mirrors WatchMode's
// "default = safe / quiet" pattern (default = Mention, only @).
//
// Lives in package agent (the core type library — alongside
// agent.MessageState, agent.AgentEvent) so registry.ChatSessionEntry
// can persist it AND chatsession.ChatSession can hold it WITHOUT a
// re-export indirection. Both packages already import agent, so
// moving the enum here removes the registry↔chatsession split that
// existed for ThinkMode / WatchMode / ToolsMode.
//
// See docs/SPEC.md §3.1.3 for the design rationale.
package agent

// ToolsMode controls per-chat tool-event visibility. The enum is
// ordered such that the zero value is the conservative default
// (tool events hidden); this matters because restored
// ChatSessions whose registry entry lacks the toolsMode field
// (older chat_sessions.json files) decode to ToolsModeHide via
// Go's zero-value semantics. Users who want tool-call visibility
// opt in via /tools on.
//
// Slash-command syntax:
//
//	/tools on   → ToolsModeShow  (also accepts "show")
//	/tools off  → ToolsModeHide  (also accepts "hide")
//	/tools      → reply current mode + usage hint
//
// ToolsMode is chat-type-independent: the gate treats DM and
// group chats identically (both /tools off → hide tool events in
// the user-message thread). Mirrors ThinkMode (not WatchMode) on
// this axis.
type ToolsMode int

const (
	// ToolsModeHide is the default: the runtime drops OutToolStart
	// and OutToolEnd events at the EventHandler gate (after
	// Translate + ReplyTo stamping, before ch.Send). The receipt
	// card still carries the final answer; tool activity is just
	// not surfaced in the user-message thread.
	ToolsModeHide ToolsMode = iota

	// ToolsModeShow: the runtime forwards OutToolStart /
	// OutToolEnd to the Channel. Feishu adapter merges each pair
	// into a single thread reply (PATCH on the start message_id
	// when OutToolEnd arrives; falls back to a new thread reply
	// on PATCH failure). The receipt card still carries the
	// final answer as the pinned main-chat view.
	ToolsModeShow
)

// String implements fmt.Stringer for log lines + reply text.
// Names are kept short and stable so they can appear in /tools
// reply messages without ambiguity. Mirrors ThinkMode.String.
func (m ToolsMode) String() string {
	switch m {
	case ToolsModeHide:
		return "hide"
	case ToolsModeShow:
		return "show"
	default:
		return "unknown"
	}
}

// ParseToolsMode parses the slash-command arg into a ToolsMode.
// Accepts "on" / "show" as Show aliases and "off" / "hide" as
// Hide aliases so users can pick whichever is more memorable.
// Returns false for unknown values so the caller can reply with a
// usage hint without committing a state mutation. Mirrors
// ParseThinkMode.
func ParseToolsMode(s string) (ToolsMode, bool) {
	switch s {
	case "on", "show":
		return ToolsModeShow, true
	case "off", "hide":
		return ToolsModeHide, true
	default:
		return ToolsModeHide, false
	}
}