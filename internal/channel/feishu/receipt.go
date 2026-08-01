package feishu

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// MessageReceipt is the per-user-message dual-track status display
// (F-25 spec). It tracks one user message and surfaces three states:
//
//   - ⏳ 等待中 (StateWaiting)
//   - 🔄 ⏳ N · HH:MM:SS (StateExecuting, with heartbeat event count)
//   - ✅ 已完成 HH:MM:SS (StateCompleted)
//
// Two visual tracks update in sync:
//
//  1. A reply text message (one line, updated via
//     im.message.update). This carries the long-form text
//     including the event count and timestamp. The user always
//     sees exactly ONE reply line per user message — there is no
//     "stack of status messages" mode.
//  2. A single reaction emoji on the user message. Feishu does
//     not expose an "update reaction" API; we swap by deleting
//     the old emoji and creating the new one. The user always
//     sees exactly ONE reaction emoji (⏳ / 🔄 / ✅) per user
//     message — there is no "row of growing emojis" mode.
//
// Visual result on the user message:
//
//	🔄 ← single reaction emoji, swapped on every state change
//	⏳ 等待中 ← reply text, updated in place
//
// The reaction row at the bottom of the Feishu message shows one
// emoji at a time. Heartbeat ticks change the text but not the
// emoji — only state transitions swap the emoji.
//
// The "Other" option for AskUserQuestion is rendered in the reply
// text too — see F-24 §7 for the full card layout. For v0.2 we
// keep the receipt minimal (single line of text); richer card
// layouts land in v0.3.
type MessageReceipt struct {
	chatID     string
	userMsgID  string
	replyMsgID string
	bot        *Adapter
	logger     *slog.Logger

	mu          sync.Mutex
	state       ReceiptState
	eventCount  int
	lastEventAt time.Time
	forwardedAt time.Time
	completedAt time.Time
	receivedAt  time.Time

	// currentReaction is the emoji currently rendered on the user
	// message; currentReactionID is the Feishu-side ID needed to
	// delete it. On every state transition we delete the old
	// reaction and create the new one in the same message row.
	// The user always sees exactly ONE reaction emoji.
	currentReaction   string
	currentReactionID string
}

// ReceiptState is the lifecycle state of a MessageReceipt.
type ReceiptState int

const (
	// StateWaiting means the message has been received and is in
	// the buffer or pending dispatch to Claude.
	StateWaiting ReceiptState = iota

	// StateExecuting means Claude is processing this message (either
	// directly or after a buffer flush). Heartbeat updates the
	// "🔄 ⏳ N · HH:MM:SS" line on every OnEvent.
	StateExecuting

	// StateCompleted means Claude's result event has been received.
	// The receipt is terminal and no further updates happen.
	StateCompleted
)

// String returns the user-facing single-line text for a state.
// Exported because the F-25 spec lists these as the canonical
// renderings.
func (s ReceiptState) String(receipt *MessageReceipt) string {
	switch s {
	case StateWaiting:
		return "⏳ 等待中"
	case StateExecuting:
		if receipt == nil || receipt.lastEventAt.IsZero() {
			return "🔄 处理中"
		}
		return fmt.Sprintf("🔄 ⏳ %d · %s",
			receipt.eventCount,
			receipt.lastEventAt.Format("15:04:05"))
	case StateCompleted:
		if receipt == nil || receipt.completedAt.IsZero() {
			return "✅ 已完成"
		}
		return fmt.Sprintf("✅ 已完成 %s",
			receipt.completedAt.Format("15:04:05"))
	}
	return ""
}

// Emoji returns the Feishu reaction emoji_type for a state. Used by
// the ReactionAdd side of the dual-track.
//
// The identifiers are Feishu predefined emoji_type values — NOT raw
// unicode. Passing unicode like "⏳" to the reaction API fails with
// code 99992354 ("data not found") because the reaction service
// only recognizes the predefined set (THUMBSUP / OK / OnIt /
// PARTY / …, full list at
// https://open.feishu.cn/document/server-docs/im-v1/message-reaction/emojis-introduce).
// The reply-text unicode (in String() above) is unaffected — that
// path sends text, not reactions.
func (s ReceiptState) Emoji() string {
	switch s {
	case StateWaiting:
		return "OK"
	case StateExecuting:
		return "OnIt"
	case StateCompleted:
		return "PARTY"
	}
	return ""
}

// NewMessageReceipt creates a receipt and emits the initial state
// (Waiting). The reply text message and the ⏳ reaction are posted
// before NewMessageReceipt returns. Errors are non-fatal — the
// receipt stays usable even if the initial render fails.
//
// userMsgID is the Feishu message ID of the user's incoming message.
// chatID is the chat where the message lives (open_id / chat_id).
func NewMessageReceipt(ctx context.Context, bot *Adapter, chatID, userMsgID string) (*MessageReceipt, error) {
	r := &MessageReceipt{
		chatID:     chatID,
		userMsgID:  userMsgID,
		bot:        bot,
		logger:     slog.Default(),
		receivedAt: time.Now(),
		state:      StateWaiting,
	}

	// 1. Add the ⏳ reaction to the user message. Capture the
	//    reaction ID so we can delete it on the next state
	//    transition (Feishu has no UpdateReaction API).
	rid, err := bot.AddReaction(ctx, userMsgID, StateWaiting.Emoji())
	if err != nil {
		r.logger.Warn("feishu receipt: initial reaction failed", "err", err)
	} else {
		r.mu.Lock()
		r.currentReaction = StateWaiting.Emoji()
		r.currentReactionID = rid
		r.mu.Unlock()
	}

	// 2. Post the reply text message ("⏳ 等待中") and capture the
	//    returned message ID — subsequent state transitions edit
	//    this message in place via UpdateMessage so the user sees
	//    exactly one reply line per user message (F-25 spec:
	//    "永远只有一行").
	msgID, err := bot.SendMessageText(ctx, chatID, StateWaiting.String(r))
	if err != nil {
		r.logger.Warn("feishu receipt: initial reply failed", "err", err)
		return r, fmt.Errorf("create receipt: %w", err)
	}
	r.mu.Lock()
	r.replyMsgID = msgID
	r.mu.Unlock()

	return r, nil
}

// SetExecuting transitions Waiting → Executing. Adds the 🔄
// reaction and updates the reply text with the initial event count
// (1).
func (r *MessageReceipt) SetExecuting(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == StateCompleted {
		// Already done — ignore late SetExecuting (e.g. racy events).
		return nil
	}
	if r.state == StateExecuting {
		// Idempotent: SetExecuting called twice is a no-op.
		return nil
	}
	r.state = StateExecuting
	r.forwardedAt = time.Now()
	r.eventCount = 1
	r.lastEventAt = r.forwardedAt
	return r.renderLocked(ctx)
}

// Heartbeat increments the event count and updates the executing
// line. Called from F-23 heartbeat.OnEvent on each stream-json event.
// No-op if not in Executing state.
func (r *MessageReceipt) Heartbeat(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != StateExecuting {
		return nil
	}
	r.eventCount++
	r.lastEventAt = time.Now()
	return r.renderLocked(ctx)
}

// SetCompleted transitions Executing → Completed. Adds the ✅
// reaction and writes the final reply text.
func (r *MessageReceipt) SetCompleted(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == StateCompleted {
		return nil // idempotent
	}
	r.state = StateCompleted
	r.completedAt = time.Now()
	return r.renderLocked(ctx)
}

// State returns the current state. Useful for tests + diagnostics.
func (r *MessageReceipt) State() ReceiptState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// renderLocked pushes the current state emoji (reaction) and text
// (reply) to Feishu. Caller must hold r.mu.
//
// Reaction strategy: SWAP, never accumulate. The previous reaction
// (if any) is deleted and the new state emoji (⏳ / 🔄 / ✅) is
// added in its place. Feishu has no UpdateReaction API, so the swap
// is Delete + Create. The user always sees exactly ONE reaction
// emoji per user message.
//
// Heartbeat ticks do NOT swap reactions — only state transitions
// do. Heartbeat updates the reply text in place.
//
// Reply text strategy: edit-in-place via im.message.update. The
// initial reply was posted by NewMessageReceipt and the ID is in
// r.replyMsgID. Heartbeat ticks and state transitions both update
// the same message — the user always sees ONE reply line per user
// message. Falls back to a fresh message if update fails (e.g.
// message older than 48h, or update quota exhausted).
func (r *MessageReceipt) renderLocked(ctx context.Context) error {
	// 1. Swap reaction if the state emoji changed.
	emoji := r.state.Emoji()
	if emoji != r.currentReaction {
		// Delete the old reaction (if any). Best-effort: failure
		// here is logged but does not block — leaving a stale
		// emoji is preferable to surfacing an error to the user.
		if r.currentReactionID != "" {
			if err := r.bot.DeleteReaction(ctx, r.userMsgID, r.currentReactionID); err != nil {
				r.logger.Warn("feishu receipt: delete old reaction failed",
					"err", err, "old_emoji", r.currentReaction)
			}
		}
		// Add the new reaction and remember its ID for the next
		// swap. A failed Create leaves the user without a state
		// indicator on this message; other receipts on the same
		// session still work because they have their own reactions.
		newID, err := r.bot.AddReaction(ctx, r.userMsgID, emoji)
		if err != nil {
			r.logger.Warn("feishu receipt: add new reaction failed",
				"err", err, "emoji", emoji, "state", r.state)
		} else {
			r.currentReaction = emoji
			r.currentReactionID = newID
		}
	}

	// 2. Update the reply text in place.
	text := r.state.String(r)
	if r.replyMsgID != "" {
		if updateErr := r.bot.UpdateMessage(ctx, r.replyMsgID, text); updateErr == nil {
			return nil
		} else {
			r.logger.Warn("feishu receipt: update reply failed, posting fresh",
				"err", updateErr, "state", r.state)
		}
	}

	msgID, sendErr := r.bot.SendMessageText(ctx, r.chatID, text)
	if sendErr != nil {
		r.logger.Warn("feishu receipt: fresh reply failed",
			"err", sendErr, "state", r.state)
		return sendErr
	}
	r.replyMsgID = msgID
	return nil
}

// SendMessageText posts a new text message. Implemented in adapter.go.
