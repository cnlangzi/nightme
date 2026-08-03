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
	"github.com/cnlangzi/nightme/internal/receipt"
)

// ChatType discriminates inbound message origins. The exact
// semantics (DM vs group vs thread) are channel-specific; we keep
// the set small and let Channel translate its native taxonomy.
type ChatType string

const (
	ChatTypeP2P    ChatType = "p2p"     // 1:1 DM
	ChatTypeGroup  ChatType = "group"   // group chat
	ChatTypeThread ChatType = "thread"  // Feishu topic_group / Slack thread
	ChatTypeOther  ChatType = "other"   // channel-private; Gateway doesn't branch on this
)

// InboundMessage is the abstract shape of "a message arriving from
// some Channel". Channels parse their native event into this shape
// before publishing on Channel.Incoming(); Gateway reads this and
// never sees the channel-native payload (which lives in Raw).
type InboundMessage struct {
	ChatID string
	UserID string
	// ChatType drives Gateway's buffer / threading policy.
	ChatType ChatType
	// Text is the caption / message body.
	Text string
	// Attachments is the unified attachment shape. Channels are
	// responsible for downloading their native blob into a local
	// file and exposing it here.
	Attachments []Attachment
	// ReplyTo is the channel-native message id that this inbound
	// message is a reply to (Feishu root_id, Slack thread_ts, ...).
	// Empty when not a reply.
	ReplyTo string
	// MessageID is the channel-native message id of this inbound
	// message itself.
	MessageID string
	// Time is when the message arrived at the channel.
	Time time.Time
	// Action is non-nil when this inbound message represents a user
	// interaction with a previously-sent interactive card.
	Action *ActionPayload
	// Raw carries the channel-native payload for handlers that
	// genuinely need it.
	Raw any
}

// IsDM returns true for private / 1-on-1 chats. Session Manager
// uses this to decide whether the chat should host a workspace
// (DMs are treated as a single auxiliary "control plane" — see
// docs/SPEC.md §3 for the full rationale).
func (m InboundMessage) IsDM() bool {
	switch m.ChatType {
	case ChatTypeP2P:
		return true
	case "":
		return false
	}
	return false
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

// OutboundKind tags the shape of an OutboundMessage. Channels
// decide whether/how to render each kind; they may drop or
// substitute kinds their UI cannot represent.
type OutboundKind int

const (
	// OutText is a plain-text message — the most common case for
	// both final agent replies and intermediate status lines.
	OutText OutboundKind = iota
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
	// Distinct from OutText: OutText is the agent's stream of
	// intermediate / final replies and goes through the receipt
	// (F-25 rolling-log card → PATCH in place). OutCommandReply
	// bypasses the receipt entirely so the user sees a standalone
	// text message, not a new card. The Feishu adapter implements
	// this with a plain SendMessageText call (no ReplyTo threading,
	// no in-place update). See docs/channel/feishu.md §5 for the
	// full rationale.
	OutCommandReply
)

// String renders OutboundKind for log lines.
func (k OutboundKind) String() string {
	switch k {
	case OutText:
		return "text"
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
	// Text carries the rendered body for OutText / OutToolStart /
	// OutToolEnd / OutThinking. Channels are expected to truncate
	// for their own UI limits; Gateway does not pre-truncate.
	Text string
	// Card carries the interactive card payload for OutCard.
	Card *Card
	// Reaction carries the emoji + target for the legacy reaction
	// events. v1.3 (F-31) introduces MessageState (preferred path);
	// Reaction is retained temporarily for backward compatibility
	// and will be removed once all channel adapters migrate to
	// MessageState.
	Reaction *Reaction
	// MessageState carries the payload for OutMessageState /
	// OutMessageStateRemoved kinds (F-31). Channel reads from this
	// field directly OR from Meta["message_id"] + Meta["state"].
	MessageState *MessageStatePayload
	// ReplyTo carries the channel-native root message id when the
	// agent wants to reply in a thread.
	ReplyTo string
	// Meta carries opaque per-kind payload the Channel may need:
	//   OutMessageState: Meta.MessageID is the target user message;
	//     Meta.State is the receipt.MessageState value.
	//   (legacy) Reaction / ReactionRemoved: see Reaction struct.
	//   OutCard: Meta.RequestID is the correlation token the user
	//     click carries back in InboundMessage.Action.RequestID.
	//   OutToolStart: Meta.ToolName / Meta.Args.
	//   OutToolEnd: Meta.ToolName / Meta.Output / Meta.Err.
	//   OutTyping: Channel-specific (usually empty).
	Meta map[string]any
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
// OutMessageState / OutMessageStateRemoved kinds (F-31). It is a
// redundant carrier for the same data available in
// Meta["message_id"] + Meta["state"]; channels can read from
// either location based on preference.

// Reaction is the legacy payload for the (deprecated)
// OutReaction / OutReactionRemoved kinds. v1.3 channels should
// migrate to MessageStatePayload instead. This type remains
// temporarily for backward compatibility and will be removed
// once all channel adapters use MessageState.
type Reaction struct {
	// EmojiType is the channel-native identifier of the emoji.
	EmojiType string
	// ReactionID is the channel-native reaction id returned by a
	// previous AddReaction call. Required for OutReactionRemoved.
	// Empty for OutReaction.
	ReactionID string
}
//
// v1.3: State is the canonical abstract value; Emoji is optional
// channel-specific override (most channels ignore it and map
// State → emoji internally).
type MessageStatePayload struct {
	// State is the abstract MessageState value (received /
	// forwarded / done / error).
	State receipt.MessageState
	// Emoji is an optional channel-native emoji override. Most
	// channels ignore this and map State → emoji via their own
	// table (e.g. Feishu: StateReceived → "OneSecond").
	Emoji string
}

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
