// Package gateway — message types shared between Channel adapters
// and the Gateway itself. Defined here so that:
//
//   - Adding a new Channel doesn't require touching agent code.
//   - The Gateway never sees a channel-native event format.
//   - The agent runtime doesn't know which channel is on the wire.
//
// See docs/feat/F-26-gateway-hub.md for the design rationale and
// the full hub-and-spoke architecture diagram.
package gateway

import (
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
)

// ChatType was removed in F-33. The Gateway never sees chat types;
// ChatSession / BindingEntry / registry schema carry no ChatType
// field. Channel adapters may classify chats internally (see
// internal/channel.ChatTypeP2P/ChatTypeGroup) but only their
// rendering decisions are exposed to the wider system, never the
// classification itself. See docs/SPEC.md section 3.1 and docs/feat/F-33.

// InboundMessage is the abstract shape of "a message arriving from
// some Channel". Channels parse their native event into this shape
// before publishing on Channel.Incoming(); Gateway reads this and
// never sees the channel-native payload (which lives in Raw).
type InboundMessage struct {
	ChatID string
	UserID string
	// Text is the caption / message body.
	Text string
	// Attachments is the unified attachment shape. Channels are
	// responsible for downloading their native blob into a local
	// file and exposing it here.
	//
	// Invariant (F-14 v1.4a): Attachments[i].LocalPath must be
	// populated before this struct is published on ch.Incoming().
	// A Channel that emits LocalPath == "" for non-failed downloads
	// is a bug — the dispatcher silently drops those attachments at
	// BuildBlocks. See WARN log "feishu: inbound attachments decoded
	// with empty LocalPath" for the diagnostic.
	Attachments []Attachment
	// Blocks is the ordered user-visible turn shape, populated by
	// Channel adapters for rich-text messages (Feishu msg_type=post)
	// whose paragraph-internal ordering must be preserved through to
	// the Agent. Non-post msg_types leave Blocks == nil; the
	// dispatcher falls back to BuildBlocks(msg.Text, msg.Attachments)
	// for those.
	//
	// For post messages: each Feishu paragraph node (`tag:"text"` /
	// `tag:"img"` / `tag:"a"`) maps to one ContentBlock. Image blocks
	// carry FileKey in their Path field as a pre-download placeholder;
	// the post-download resolveBlocks step back-fills LocalPath.
	// The order of blocks is the order the user saw in Feishu.
	//
	// Note: Blocks is the SOURCE OF TRUTH for post msg_types; Text
	// is "" for those (extractAttachments folds text into Blocks).
	// Attachments and Blocks are not redundant — Attachments carries
	// the download candidates (binary sources keyed by FileKey);
	// Blocks carries the user-visible turn shape (text + images
	// interleaved).
	Blocks []agent.ContentBlock
	// ReplyTo is the channel-native message id that this inbound
	// message is a reply to. For Feishu, this is the SDK's
	// `parent_id` field (F-33 D3) -- the message the user directly
	// replied to. Thread-top-level `RootId` is intentionally not
	// surfaced: nightme only tracks point-to-point reply
	// relationships. Empty when the inbound message is not a reply.
	//
	// Other channels map their native reply-target identifier onto
	// this same field (Slack message_ts, Telegram message_id, ...);
	// the field name stays stable but the semantics are
	// channel-specific. No Channel introduces a thread concept into
	// nightme data model (F-33 D4).
	ReplyTo string
	// MessageID is the channel-native message id of this inbound
	// message itself.
	MessageID string
	// Time is when the message arrived at the channel.
	Time time.Time
	// Action is non-nil when this inbound message represents a user
	// interaction with a previously-sent interactive card.
	Action *ActionPayload
	// Reaction is non-nil when this inbound message represents a
	// user-emoji reaction on a previously-sent message. Channels
	// translate their native reaction-created event into this
	// shape; the gateway routes to ChatSession.HandleReaction,
	// which checks gtwDrafts first (F-45 §3.5) and may fall
	// through to the F-31 MessageState FSM for non-gtw reactions.
	//
	// Action and Reaction are mutually exclusive in practice: a
	// reaction is an emoji click on a message; an Action is a
	// card-button click. Both share the same inbound pipeline.
	Reaction *ReactionEvent
	// Raw carries the channel-native payload for handlers that
	// genuinely need it.
	Raw any
	// HasMention is set by the channel adapter during decode.
	// Semantics: "the original message addressed bot or @_all".
	//
	//   - DM (chat_type=p2p): always true (every DM message is
	//     implicitly addressed to the bot).
	//   - group/topic_group: true iff Mentions contains bot's own
	//     open_id or @_all.
	//   - unknown/empty chat_type: defaults to true (safe fallback;
	//     dropping is a worse failure than over-processing).
	//
	// The Gateway dispatcher uses this with ChatSession.WatchMode
	// to drop non-mention group messages when WatchMode ==
	// WatchModeMention (default). See docs/SPEC.md §3.1.1.
	HasMention bool
}

// Attachment is the unified inbound attachment shape. Channels are
// expected to download (or copy) the remote blob to a local path
// before constructing this struct.
//
// Type and FileKey are channel-native metadata that some channels
// (Feishu in particular) populate before download — they let the
// channel driver know which API to call for the actual binary
// fetch. Channels without an asynchronous-resource API (web UI,
// echo) leave them empty; the abstract Type-aware fields below
// are what downstream code actually depends on.
type Attachment struct {
	// LocalPath is the absolute filesystem path after successful
	// download. Empty if the download failed (see Error) or has
	// not been attempted yet (Feishu leaves it empty until
	// DownloadAttachments fills it in).
	LocalPath string
	// MimeType is the RFC 6838 media type. Defaults to
	// "application/octet-stream" if the channel can't determine it.
	MimeType string
	// Size is the byte length of the downloaded file. Zero if the
	// download failed or has not been attempted.
	Size int64
	// Name is the original filename when the channel exposes one.
	// Empty for bare image messages; the download path synthesises
	// a fallback name when needed.
	Name string
	// Type is the channel-native msg_type — Feishu values: "image",
	// "file", "audio", "media" (video). Other backends use their own
	// vocabulary. Downstream display code (e.g. agent.ContentBlock
	// selector) branches on this to pick the right renderer.
	Type string
	// FileKey is the channel-side resource identifier (Feishu
	// image_key / file_key). Preserved so the download path can
	// re-attempt without re-extracting from the message envelope.
	FileKey string
	// FileName is the original filename when the channel exposes
	// one. Distinct from Name because Feishu carries file_name on
	// the message envelope; other channels synthesise it from
	// Content-Disposition or equivalent. Download uses whichever
	// is set.
	FileName string
	// Error is non-nil if the download failed after all retries.
	// Empty on success. Best-effort callers should treat Error as
	// a hard skip.
	Error error
}

// ActionPayload represents a user's interaction with a previously-sent
// interactive card. Channels build this from their native click /
// submit / select payloads.
type ActionPayload struct {
	// RequestID is the Gateway-assigned correlation token that ties
	// the click back to the original EventPermission.
	RequestID string
	// Option is the user's choice (Feishu button label, Slack action
	// value, etc.). Empty when the action is a form submission.
	Option string
	// Form is the user's form input when the action carries form
	// data (Slack modal submit, etc.). Nil when the action is a
	// button click.
	Form map[string]string
	// Raw carries the channel-native action payload for handlers
	// that need it.
	Raw any
}

// ReactionEvent is the abstract shape of a user-emoji reaction on
// a previously-sent message. Channels build this from their
// native reaction-created event; the gateway forwards to
// ChatSession.HandleReaction which consults the per-chat
// ReactionHandler installed at runtime.
//
// F-45 §3.2: this is the inbound counterpart of the bot's
// AddReaction outbound API. Emoji is the raw unicode form
// ("✅", "🆕", "🔗", "❌", "🔄", "🤝"); semantic interpretation
// is the per-draft handler's job.
//
// The type is declared in internal/chatsession (which owns the
// per-chat handler machinery); the alias below keeps the
// gateway-package import path working without creating an
// import cycle (gateway → chatsession is the only direction).
type ReactionEvent = chatsession.ReactionEvent

// OutboundKind tags the shape of an OutboundMessage. Channels
// decide whether/how to render each kind; they may drop or
// substitute kinds their UI cannot represent.
type OutboundKind int

const (
	// OutReply is a streaming reply chunk — the agent's reply to
	// the user's current turn. Sourced from agent.EventText
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
	// OutTyping sends a transient typing indicator.
	OutTyping
	// OutResult is the assistant's final reply for the turn. Sourced
	// from agent.EventResult (Claude Code: result.Result). Channels
	// render it with a distinct icon (e.g. 📝) so users can tell
	// "the final answer" from rolling-log entries.
	OutResult
	// OutUsage carries the turn's token usage / cost. Sourced from
	// agent.EventUsage (Claude Code: result.usage /
	// result.modelUsage). Channels typically render it as a footer
	// line ("1.2k tokens · $0.012") or append it to the receipt's
	// terminal header.
	OutUsage
	// OutCompaction signals a mid-turn context compaction. NOT a
	// turn end — the agent continues. Channels briefly surface
	// "Compacting…" so users know why the agent paused.
	OutCompaction
	// OutInit carries session bootstrap data (session_id + model)
	// from the agent's system/init event. Channels use it to render
	// "session <id> · model <name>" in the receipt header.
	OutInit
	// OutCommandReply is a one-shot plain-text message sent in
	// response to a system-level slash command (e.g. /cwd, /run,
	// /help, /kill, /agents) or to a runtime error that needs to
	// surface to the user without the rolling-log card path.
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
	case OutTyping:
		return "typing"
	case OutResult:
		return "result"
	case OutUsage:
		return "usage"
	case OutCompaction:
		return "compaction"
	case OutInit:
		return "init"
	case OutCommandReply:
		return "command_reply"
	case OutTaskCreate:
		return "task_create"
	case OutTaskUpdate:
		return "task_update"
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
	TaskList *agent.TaskListEvent
	// Result carries the typed payload for OutResult. nil for
	// other Kinds. Gateway populates from AgentEvent.Result; the
	// channel reads directly instead of round-tripping the
	// fields through Meta. Replaces the legacy
	// Meta["duration_ms"] / ["is_error"] / ["subtype"] implicit
	// protocol (removed in §1.4 cleanup).
	Result *agent.ResultEvent
	// Usage carries the typed payload for OutUsage. nil for other
	// Kinds. Gateway populates from AgentEvent.Usage. Replaces
	// the legacy Meta["input_tokens"] / ["output_tokens"] /
	// ["cache_creation_input_tokens"] / ["cache_read_input_tokens"]
	// / ["cost_usd"] implicit protocol (removed in §1.4 cleanup).
	Usage *UsageInfo
	// MessageState carries the payload for OutMessageState /
	// OutMessageStateRemoved kinds (F-31). Channel reads from this
	// typed field directly. Replaces the legacy
	// Meta["message_id"] / ["state"] / ["reaction_id"] implicit
	// protocol (removed in §1.4 cleanup).
	MessageState *MessageStatePayload
	// Init carries the typed payload for OutInit. nil for other
	// Kinds. Gateway populates from AgentEvent.Init. Replaces the
	// legacy Meta["session_id"] / ["model"] / ["agent_name"] /
	// ["workspace"] / ["branch"] implicit protocol (removed in
	// §1.4 cleanup).
	Init *agent.InitEvent
	// ReplyTo carries the channel-native root message id when the
	// agent wants to reply in a thread.
	ReplyTo string

	// SessionContext (F-45) is the runtime-stamped snapshot of the
	// AgentSession that produced this outbound event. It carries
	// everything the main-chat footer needs (Agent / Model /
	// CumulativeUsage) as a single atomic value — not three
	// scattered fields — so Channel render paths see one typed
	// payload and future metadata additions don't break the
	// Channel interface.
	//
	// Stamped ONLY on OutReply / OutResult / OutTaskCreate /
	// OutTaskUpdate by the runtime's newEventHandler closure. nil
	// on every other Kind (thread-only, lifecycle, init/usage
	// payloads themselves) — Channel skips footer rendering when
	// nil.
	//
	// Bridges never populate this field directly; runtime is the
	// single owner. See docs/feat/F-45-session-footer.md §1.3.
	SessionContext *SessionContext
}

// SessionContext is the runtime-stamped AgentSession snapshot
// delivered alongside main-chat OutboundMessages for footer
// rendering. Populated by the runtime's newEventHandler closure
// (not by bridges) and read by Channel adapters to compose the
// footer line on each reply / result / task receipt.
//
// Field semantics:
//
//	Agent           — registry name of the agent that produced this
//	                  event (e.g. "claude", "codex"). Sourced from
//	                  AgentSession.Agent (immutable, no lock).
//	Model           — model the agent selected (e.g.
//	                  "claude-opus-4-5-20250929"). Sourced from
//	                  AgentSession.Model which the runtime caches
//	                  on first EventInit. Empty before EventInit
//	                  lands; footer omits the segment when "".
//	CumulativeUsage — per-AgentSession running total of token /
//	                  cost stats as of this event's emission.
//	                  Sourced from AgentSession.CumulativeUsage;
//	                  captured by VALUE (struct copy under RWMutex)
//	                  so Channel can render at leisure. All 4
//	                  fields are zero on a fresh /new'd session.
//	                  Total = In + CacheCreate + CacheRead + Out
//	                  is derived at render time; no Total field
//	                  on the wire (avoids redundancy).
type SessionContext struct {
	Agent           string
	Model           string
	CumulativeUsage UsageInfo
}

// ToolInfo is the typed payload for OutboundMessage.Tool,
// representing a tool call (start or end). It captures the
// generic concepts that any tool has — name, args, output, error
// — without prescribing how each bridge represents them. Fields:
//
//	Name    — the tool's registered name (e.g. "Read", "Bash").
//	          Set on both Start and End.
//	Args    — the tool's input, in whatever representation the
//	          bridge chose. Set on both Start and End. Gateway
//	          does NOT parse this string; channels that want
//	          type-aware rendering (e.g. summarising tool output)
//	          parse it themselves.
//	Output  — the tool's result text. Only set on End; empty on
//	          Start.
//	Err     — the tool's error (if any). Only set on End; nil on
//	          Start.
//
// ToolInfo deliberately avoids naming fields after any specific
// bridge's schema (no `file_path`, `command`, `content`, etc.) —
// those are tool-specific details that the channel layer
// (with its own per-tool heuristics) handles.
type ToolInfo struct {
	Name   string
	Args   string
	Output string
	Err    error
}

// Card is an interactive permission card or any other card that
// requires the user's choice.
type Card struct {
	// Title is the short headline (e.g., "Permission needed").
	Title string
	// Body is the question or instructions.
	Body string
	// Options enumerates the user-selectable choices. The first
	// option is the default / safe choice. The Gateway maps
	// the user's selection back via SendPermission(choice).
	Options []string
	// RequestID is the correlation token.
	RequestID string
}

// MessageStatePayload is the OutboundMessage payload for
// OutMessageState / OutMessageStateRemoved kinds (F-31). It is
// the typed transport for the same data that v0.2 carried in
// Meta["message_id"] / ["state"] / ["reaction_id"]; channels
// read from this typed field directly. Replaces the legacy
// Reaction struct + implicit Meta keys (removed in §1.4
// cleanup).
type MessageStatePayload struct {
	// State is the abstract MessageState value (received /
	// forwarded / done / error).
	State agent.MessageState
	// MessageID is the channel-native id of the message being
	// reacted on (typically the user message that triggered the
	// assistant turn). Required for both OutMessageState (target
	// of AddReaction) and OutMessageStateRemoved (target of
	// DeleteReaction).
	MessageID string
	// ReactionID is the channel-native reaction id returned by a
	// prior AddReaction call. Required for OutMessageStateRemoved
	// so the channel can target the right reaction row (Feishu
	// has no UpdateReaction API). Empty for OutMessageState (the
	// reaction has not been created yet at that point).
	ReactionID string
	// Emoji is an optional channel-native emoji override. Most
	// channels ignore this and map State → emoji via their own
	// table (e.g. Feishu: StateReceived → "OneSecond").
	Emoji string
}

// UsageInfo is the typed payload for OutUsage and the
// SessionContext.CumulativeUsage field. See agent.UsageInfo for
// field semantics. Re-exported as a type alias here so existing
// gateway code (translate.go:158) keeps the same symbol name; the
// canonical definition lives in internal/agent (F-45 §2.1).
//
// (F-45): the comment block that used to live here was removed
// when the type moved to agent.UsageInfo. Old "InputTokens is the
// total input tokens ... (prompt + cache reads + tool input)" was
// misleading — InputTokens is the non-cached input count, NOT the
// sum with cache reads. Cache hits live in CacheReadInputTokens.
type UsageInfo = agent.UsageInfo

// AgentEventEnvelope carries the agent-side metadata alongside an
// OutboundMessage so Channels can correlate updates (e.g., Feishu's
// "swap reaction on the user message" needs to know which userMsgID
// the receipt is bound to).
type AgentEventEnvelope struct {
	SessionID string
	UserMsgID string
	ChatID    string
	Event     agent.AgentEvent
}
