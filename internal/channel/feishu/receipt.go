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

	// 2. Post the reply text message ("⏳ 等待中").
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
// Reply text strategy: best-effort SendMessageText. We don't yet
// have Feishu MessageUpdate wired up (lark-oapi exposes it; deferred
// to a follow-up because each SendMessageText creates a new message).
// For v0.2 the reply text grows as a stack of messages, which is
// ugly. The honest fix is MessageUpdate. We log a TODO when this
// branch fires.
func (r *MessageReceipt) renderLocked(ctx context.Context) error {
	emoji := r.state.Emoji()
	if err := r.addReaction(ctx, emoji); err != nil {
		r.logger.Warn("feishu receipt: reaction update failed",
			"err", err, "state", r.state)
	}

	text := r.state.String(r)
	// TODO(F-25): wire im.message.update for in-place reply edits.
	// For now, post a new message each time. Users see a thread of
	// status lines which is ugly but functional. Follow-up commit
	// will replace this with a single card that we patch.
	if _, err := r.bot.SendMessageText(ctx, r.chatID, text); err != nil {
		r.logger.Warn("feishu receipt: reply text update failed",
			"err", err, "state", r.state)
		return err
	}
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

// SendMessageText posts a new text message. We use the public
// SendMessage (not the internal sendContent) so the test surface
// stays consistent.
func (a *Adapter) SendMessageText(ctx context.Context, chatID, text string) (string, error) {
	if err := a.SendMessage(ctx, chatID, text); err != nil {
		return "", err
	}
	// SendMessage does not currently expose the returned message ID
	// through the public Channel API. We return "" so callers can
	// detect the gap (and the receipt layer can fall back to
	// "no-edit-available" mode if needed).
	return "", nil
}
