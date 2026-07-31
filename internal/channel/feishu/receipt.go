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
//  1. A reply text message (one line, updated via SendMessage /
//     a future MessageUpdate API). This carries the long-form text
//     including the event count and timestamp.
//  2. Reaction emojis added to the user message. We accumulate
//     rather than switch — each state adds a new emoji. This is
//     simpler than delete-and-re-add and survives Feishu's
//     reaction_id bookkeeping.
//
// Visual result on the user message:
//
//	👀 ← typing indicator (added once at creation)
//	⏳ 🔄 ✅ ← reaction row, growing as the message progresses
//	⏳ 等待中 ← reply text (updates in place)
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

	mu             sync.Mutex
	state          ReceiptState
	eventCount     int
	lastEventAt    time.Time
	forwardedAt    time.Time
	completedAt    time.Time
	receivedAt     time.Time
	reactionsAdded map[string]bool // emoji -> already added (idempotent)
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

// Emoji returns the reaction emoji for a state. Used by the
// ReactionAdd side of the dual-track.
func (s ReceiptState) Emoji() string {
	switch s {
	case StateWaiting:
		return "⏳"
	case StateExecuting:
		return "🔄"
	case StateCompleted:
		return "✅"
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
		chatID:         chatID,
		userMsgID:      userMsgID,
		bot:            bot,
		logger:         slog.Default(),
		receivedAt:     time.Now(),
		reactionsAdded: make(map[string]bool),
		state:          StateWaiting,
	}

	// 1. Add the ⏳ reaction to the user message.
	if err := r.addReaction(ctx, StateWaiting.Emoji()); err != nil {
		r.logger.Warn("feishu receipt: initial reaction failed", "err", err)
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
	r.replyMsgID = msgID

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
// Reaction strategy: ACCUMULATE, never delete. Each state adds its
// emoji (⏳ → 🔄 → ✅) and the user sees the progression in the
// reaction row. This avoids the reaction_id bookkeeping that
// delete-then-add would require.
//
// Reply text strategy: edit-in-place via im.message.update. The
// initial reply was posted by NewMessageReceipt and the ID is in
// r.replyMsgID. Each subsequent state transitions updates the same
// message — the user always sees ONE reply line per user message.
// Falls back to a fresh message if update fails (e.g. message older
// than 48h, or update quota exhausted).
func (r *MessageReceipt) renderLocked(ctx context.Context) error {
	emoji := r.state.Emoji()
	if err := r.addReaction(ctx, emoji); err != nil {
		r.logger.Warn("feishu receipt: reaction update failed",
			"err", err, "state", r.state)
	}

	text := r.state.String(r)

	// Try in-place update first; on failure, post a fresh message
	// and adopt its ID as the new reply target. This means the
	// fallback degrades gracefully without losing the receipt.
	if r.replyMsgID != "" {
		if err := r.bot.UpdateMessage(ctx, r.replyMsgID, text); err == nil {
			return nil
		} else {
			r.logger.Warn("feishu receipt: update reply failed, posting fresh",
				"err", err, "state", r.state)
		}
	}

	msgID, err := r.bot.SendMessageText(ctx, r.chatID, text)
	if err != nil {
		r.logger.Warn("feishu receipt: fresh reply failed",
			"err", err, "state", r.state)
		return err
	}
	r.replyMsgID = msgID
	return nil
}

// addReaction is idempotent: we track which emojis have been added
// and skip duplicate calls. This protects against repeated
// Heartbeat() invocations adding the same 🔄 multiple times.
func (r *MessageReceipt) addReaction(ctx context.Context, emoji string) error {
	if emoji == "" {
		return nil
	}
	if r.reactionsAdded[emoji] {
		return nil
	}
	if err := r.bot.AddReaction(ctx, r.userMsgID, emoji); err != nil {
		return err
	}
	r.reactionsAdded[emoji] = true
	return nil
}

// SendMessageText posts a new text message. Implemented in adapter.go.
