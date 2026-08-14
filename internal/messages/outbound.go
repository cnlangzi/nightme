package messages

import (
	"github.com/cnlangzi/nightme/internal/agent"
)

// OutboundKind tags the shape of an OutboundMessage. Channels
// decide whether/how to render each kind; they may drop or
// substitute kinds their UI cannot represent.
type OutboundKind int

const (
	// OutReply is a streaming reply chunk — the agent's reply to
	// the user's current turn. Sourced from agent.EventAgentText
	// (without the [思考] thinking prefix; thinking has its own
	// OutThinking kind). The most common case for both multi-
	// chunk final replies and single-chunk status lines.
	//
	// F-40 rename from OutText: the new name better reflects that
	// this is the agent's reply payload, not a generic "text".
	// See docs/feat/F-40-outreply-overflow.md.
	OutReply OutboundKind = iota
	// OutToolStart announces a tool invocation.
	OutToolStart
	// OutToolEnd announces a tool's completion.
	OutToolEnd
	// OutThinking surfaces the agent's reasoning.
	OutThinking
	// OutMessageState is a MessageState change event for an inbound
	// user message (F-31). Triggered by ChatSession lifecycle;
	// Channel renders it as platform-native progress indicator
	// (Feishu: AddReaction with state-specific emoji_type; Slack:
	// reactions.add; etc.).
	//
	// Distinct from receipt lifecycle: this tracks the MESSAGE's
	// progress through the system, not the response's rendering.
	// See docs/feat/F-31-message-state.md and SPEC §2.5.
	OutMessageState
	// OutMessageStateRemoved removes a message state marker. Not
	// used in v1.3 (append-only); reserved for channels that
	// support mutable state markers (e.g. Web UI).
	OutMessageStateRemoved
	// OutCard sends an interactive card (permission request, etc.).
	OutCard
	// OutResult is the assistant's final reply for the turn. Sourced
	// from agent.EventAgentResult (Claude Code: result.Result). Channels
	// render it with a distinct icon (e.g. 📝) so users can tell
	// "the final answer" from rolling-log entries.
	OutResult
	// OutInit carries session bootstrap data (session_id + model)
	// from the agent's system/init event. Channels use it to render
	// "session <id> · model <name>" in the receipt header.
	OutInit
	// OutCommandReply is a one-shot plain-text message sent in
	// response to a system-level slash command (e.g. /cwd, /run,
	// /help, /kill, /agents), a shell `!cmd` dispatch (see
	// internal/shell/dispatch.go runShell — shell replies use this
	// Kind because shell has no receipt card), or to a runtime
	// error that needs to surface to the user without the
	// rolling-log card path.
	//
	// Distinct from OutReply: OutReply is the agent's stream of
	// intermediate / final replies and goes through the receipt
	// (F-25 rolling-log card → PATCH in place). OutCommandReply
	// bypasses the receipt entirely so the user sees a standalone
	// text message, not a new card. The Feishu adapter implements
	// this with a plain SendMessageText call (no ReplyTo threading,
	// no in-place update). See docs/channel/feishu.md §5 for the
	// full rationale.
	OutCommandReply

	// OutTaskCreate carries the first confirmable task operation
	// (e.g. Claude TaskCreate success). The payload holds the
	// current full task snapshot so the receiving Channel can
	// replace its checklist wholesale. Channels that don't render
	// a checklist (e.g. a future Web / TUI) may drop the kind.
	OutTaskCreate

	// OutTaskUpdate carries subsequent confirmable task mutations
	// (status change, edit, delete). The payload also holds the
	// current full task snapshot, so an empty Items slice is a
	// valid "clear the checklist" signal.
	OutTaskUpdate

	// OutCardPatch (F-46) replaces the body of an existing
	// interactive card message in place (Feishu PATCH). Used by
	// gtw.HandleAction follow-ups to disable the original decision
	// card after the user has picked. ReplyTo carries the bot-side
	// message id to PATCH; Card holds the new payload.
	OutCardPatch
)

// String renders OutboundKind for log lines.
func (k OutboundKind) String() string {
	switch k {
	case OutReply:
		return "reply"
	case OutToolStart:
		return "tool_start"
	case OutToolEnd:
		return "tool_end"
	case OutThinking:
		return "thinking"
	case OutMessageState:
		return "message_state"
	case OutMessageStateRemoved:
		return "message_state_removed"
	case OutCard:
		return "card"
	case OutResult:
		return "result"
	case OutInit:
		return "init"
	case OutCommandReply:
		return "command_reply"
	case OutTaskCreate:
		return "task_create"
	case OutTaskUpdate:
		return "task_update"
	case OutCardPatch:
		return "card_patch"
	}
	return "unknown"
}

// OutboundMessage is the abstract shape of "something the agent
// runtime wants the user to see". Gateway emits these from agent
// events; each Channel formats and sends them in its native UI.
//
// The Gateway does NOT batch, buffer, or retry OutboundMessage
// fire-and-ack semantics; the Channel receives each one in order
// and decides whether to render, drop, or substitute.
type OutboundMessage struct {
	ChatID string
	Kind   OutboundKind
	// Text carries the rendered body for OutReply / OutThinking.
	// Channels are expected to truncate for their own UI limits;
	// Gateway does not pre-truncate. OutToolStart / OutToolEnd
	// carry their content in the Tool field, not Text — see
	// ToolInfo for the rationale (gateway transports the unified
	// tool concept; channel decides how to render it).
	Text string
	// Card carries the interactive card payload for OutCard.
	Card *Card
	// Tool carries the typed payload for OutToolStart / OutToolEnd.
	// nil for other Kinds. Gateway populates this from
	// AgentEvent.ToolStart / ToolEnd in translate(); channels
	// read it directly instead of mining Meta for per-tool
	// fields. The Args string is whatever representation the
	// bridge chose (claudecode emits raw JSON; other bridges
	// may use typed maps serialised to string). Gateway does not
	// parse the Args content — that's channel territory.
	Tool *ToolInfo
	// TaskList carries the typed payload for OutTaskCreate /
	// OutTaskUpdate. Every event carries a full snapshot of the
	// current task list; an Items slice with length 0 is a valid
	// "clear the checklist" signal. nil for other Kinds.
	TaskList *agent.AgentTaskListEvent
	// Result carries the typed payload for OutResult. nil for
	// other Kinds. Gateway populates from AgentEvent.Result; the
	// channel reads directly instead of round-tripping the
	// fields through Meta. Replaces the legacy
	// Meta["duration_ms"] / ["is_error"] / ["subtype"] implicit
	// protocol (removed in §1.4 cleanup).
	Result *agent.AgentResultEvent
	// Usage carries the per-turn token / cost payload co-located
	// with the assistant's final reply (OutResult in practice).
	// Bridges populate this from the same source event they put
	// Result on (Claude Code: result.usage + result.modelUsage;
	// Pi: message_end.usage on the assistant role). The runtime
	// accumulates Usage on receipt before stamping StatusBar,
	// so footer rendering sees this turn's tokens on the first
	// try — the previous EventAgentResult-then-EventUsage pair that
	// required runtime buffering is gone (the data was always
	// co-located on the wire). nil means "no usage reported"
	// (zero-usage turn).
	//
	// Typed as *UsageInfo (per-event shape) because the payload IS
	// a single turn's data, not a running total. The runtime is a
	// passive pass-through — it does NOT aggregate Usage across
	// turns; the channel footer reads out.Usage directly and
	// surfaces it as Line 2 of the footer.
	Usage *agent.UsageInfo
	// MessageState carries the payload for OutMessageState /
	// OutMessageStateRemoved kinds (F-31). Channel reads from this
	// typed field directly. Replaces the legacy
	// Meta["message_id"] / ["state"] / ["reaction_id"] implicit
	// protocol (removed in §1.4 cleanup).
	MessageState *MessageStatePayload

	// SessionID / Model / AgentName / Workspace / Branch carry the
	// session metadata for OutInit (Gateway populates from the
	// top-level AgentEvent fields of the same name). They replace
	// the legacy Ready *AgentReadyEvent field (deleted when event
	// payloads were flattened). Channel reads these directly to
	// render the receipt header / footer.
	SessionID string
	Model     string
	AgentName string
	Workspace string
	Branch    string

	// ReplyTo carries the channel-native root message id when the
	// agent wants to reply in a thread.
	ReplyTo string

	// Err is the unified error indicator on this outbound message,
	// sourced from AgentEvent.Err (see event flattening). Set on:
	//   - OutResult when the turn ended in error
	//   - OutToolEnd when the tool invocation failed
	// nil otherwise. Channels check `msg.Err != nil` to render
	// error UI (📛 icon, ⚠️ prefix, etc.).
	Err error

	// GitStatus (F-CLAUDE-PRINT-002) is the workspace + git + PR
	// context attached to every outbound message that flows to a
	// Channel. Sourced from chatsession (chatsession caches its
	// own GitStatus, refreshed on /gtw commit, /gtw pr, and
	// chatsession startup). The runtime doesn't recompute; the
	// chatsession is the single owner.
	//
	// Per the F-CLAUDE-PRINT-002 refactor: the legacy StatusBar
	// wrapper is gone. Agent identity (Agent / Model / SessionID)
	// and per-turn usage are on this OutboundMessage directly
	// (AgentName, Model, SessionID, Usage). This field carries
	// only the workspace-level snapshot that bridges can't
	// observe.
	//
	// Nil when the chat has no workspace or no AgentSession.
	// Channels omit the workspace footer line entirely in that
	// case.
	GitStatus *GitStatus
}
