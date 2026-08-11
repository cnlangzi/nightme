// Package messages — wire-protocol message types shared by every
// component in the system: Channel adapters, the Gateway, the runtime
// event pump, and the outbound chokepoint. Defined here (rather than
// alongside any of those callers) so that:
//
//   - Adding a new Channel doesn't require touching agent code.
//   - The Gateway never sees a channel-native event format.
//   - The agent runtime doesn't know which channel is on the wire.
//
// See docs/feat/F-26-gateway-hub.md for the design rationale and
// the full hub-and-spoke architecture diagram.
package messages

import (
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	commandServices "github.com/cnlangzi/nightme/internal/command/services"
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
	// shape; the gateway routes to ChatSession.HandleAction,
	// which checks gtwDrafts first (F-50 §6.1 reaction routing) and may fall
	// through to the F-31 MessageState FSM for non-gtw reactions.
	//
	// Action and Reaction are mutually exclusive in practice: a
	// reaction is an emoji click on a message; an Action is a
	// card-button click. Both share the same inbound pipeline.
	//
	// F-51: Reaction is the canonical command.ReactionEvent
	// (defined in internal/command/services/reaction.go). The
	// gateway no longer defines its own type; channel adapters
	// construct this directly.
	Reaction *commandServices.ReactionEvent
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
	// The runtime messageDispatcher (cmd/nightme/run.go) passes
	// this to chatsession.Manager.AcceptInbound, which combines it
	// with ChatSession.WatchMode() to drop non-mention group
	// messages when WatchMode == WatchModeMention (default). The
	// gate used to live in gateway.applyWatchModeGate; it moved
	// to chatsession so the policy sits next to its state. See
	// docs/SPEC.md §3.1.1.
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
	// the click back to the original EventAgentPermission.
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