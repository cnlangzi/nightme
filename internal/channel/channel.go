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
// Raw payload support can be added without changing the daemon contract.
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
