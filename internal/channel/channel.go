// Package channel defines the protocol-neutral boundary between nightme and
// an instant-messaging backend.
package channel

import (
	"context"
	"time"
)

// Message is a normalized incoming message from a channel.
//
// Channel adapters strip protocol-specific markup before publishing a Message;
// non-text payloads (image/file/audio/video) are downloaded into
// Attachments before the message reaches the gateway. Raw payload
// support can be added without changing the daemon contract.
//
// ChatType discriminates DM ("p2p") from group chat ("group"). Each
// IM backend exposes this concept differently:
//   - Feishu: p2p | group | topic_group (P2MessageReceiveV1.event.message.chat_type)
//   - Telegram: "private" | "group" | "supergroup"
//   - Slack: "im" | "mpim" | "channel"
//
// We normalize to a small set of stable names so downstream code
// (Session Manager, Gateway) does not have to special-case every
// backend. Unknown values pass through as-is; the Session Manager
// only branches on the two well-known values.
type Message struct {
	ChatID   string
	Text     string
	SenderID string
	Time     time.Time

	// ChatType is one of "p2p" (DM), "group", "topic_group", or
	// the channel-native string for backends that do not map
	// cleanly. Empty string means "unknown" (e.g. legacy callers
	// that pre-date this field).
	ChatType string

	// MessageID is the channel-native identifier for this message
	// (e.g. Feishu's om_xxx open_message_id). Required by the
	// adapter download path — resource lookups are scoped to a
	// specific message ID. Empty for backends that do not expose
	// one.
	MessageID string

	// Attachments carries any non-text payloads (image, file, audio,
	// video, post-embedded images). For text-only messages this is
	// nil/empty. Each Attachment's LocalPath is populated by the
	// channel adapter after download; Error holds the failure
	// reason if the download was unsuccessful. See Attachment.
	Attachments []Attachment
}

// Attachment is a single file/image/audio/video resource attached to
// an incoming channel.Message.
//
// Lifecycle:
//  1. Channel adapter extracts {Type, FileKey, FileName} from the
//     raw message envelope (e.g. Feishu image/file/audio/media
//     msg_type payloads).
//  2. Channel adapter (or its delegate) downloads the binary to a
//     local path under nightme's per-session inbox and sets
//     LocalPath + Size. On failure it sets Error instead — the
//     downstream code decides whether to surface that to the user.
//
// LocalPath is an absolute filesystem path. Callers must NOT assume
// the file lives under the workspace — for v0.2 attachments live
// under ~/.nightme/inbox/<session_id>/.
//
// Error is non-empty iff the download failed after all retries.
// Best-effort semantics: the caller can choose to drop, surface, or
// retry further.
type Attachment struct {
	// Type is the channel-native msg_type — Feishu values:
	// "image", "file", "audio", "media" (video). Other backends
	// may use their own vocabulary; downstream code only branches
	// on Type for display purposes (e.g. "image" vs "file").
	Type string

	// FileKey is the channel-side resource identifier (Feishu
	// image_key / file_key). Preserved for diagnostics and to allow
	// future re-download attempts without re-extracting from the
	// message envelope.
	FileKey string

	// FileName is the original filename when the channel exposes
	// one (Feishu's file/audio/media msg_types carry file_name).
	// Empty for bare image messages. Used as the on-disk filename;
	// a synthesized fallback ("<type>_<filekey>") is used when
	// empty.
	FileName string

	// LocalPath is the absolute filesystem path after successful
	// download. Empty if the download failed (see Error) or has
	// not been attempted yet.
	LocalPath string

	// Size is the byte length of the downloaded file. Zero if the
	// download failed or has not been attempted.
	Size int64

	// Error is the failure reason after the download exhausted
	// its retries. Empty on success. Best-effort callers should
	// treat Error as a hard skip.
	Error string
}

// Normalized chat type constants. Channel adapters should map their
// native values onto these.
const (
	ChatTypeP2P    = "p2p"         // 1-on-1 DM (Feishu) / private (Telegram) / im (Slack)
	ChatTypeGroup  = "group"       // group chat (Feishu / Telegram / Slack channel)
	ChatTypeThread = "topic_group" // Feishu topic group; mapped to "thread" elsewhere
)

// IsDM returns true for private / 1-on-1 chats. Session Manager
// uses this to decide whether the chat should host a workspace
// (DMs are treated as a single auxiliary "control plane" — see
// docs/SPEC.md §3 for the full rationale).
func (m Message) IsDM() bool {
	switch m.ChatType {
	case ChatTypeP2P:
		return true
	case "":
		// Legacy callers (tests, mocks) may leave ChatType empty.
		// We treat empty as "unknown, assume not DM" to keep the
		// conservative behaviour until proven otherwise.
		return false
	}
	return false
}

// Channel is the lifecycle and messaging contract implemented by each IM
// adapter.
type Channel interface {
	// Start starts the adapter's long-lived receive loop.
	Start(ctx context.Context) error

	// Stop closes the receive loop and releases adapter resources.
	Stop(ctx context.Context) error

	// SendMessage sends one text message to chatID.
	SendMessage(ctx context.Context, chatID, text string) error

	// SendLongMessage sends text in channel-safe chunks.
	SendLongMessage(ctx context.Context, chatID, text string) error

	// Incoming returns normalized messages received from the channel.
	Incoming() <-chan Message
}
