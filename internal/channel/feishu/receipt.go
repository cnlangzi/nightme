// Package feishu — F-25 MessageReceipt with the v0.3 rolling-log
// format. Each user message gets one reply in chat that grows over
// the agent's lifetime:
//
//	⏳ 等待中
//
//	💭 I'll explore the workspace...
//	🔧 codegraph_explore(/repo/api)
//	✅ codegraph_explore done
//	💬 Here's the API handler I found at line 42:
//	    <reply text — may span multiple lines>
//
//	✅ 已完成 10:11:11
//
// All events (thinking, tool call, tool result, final reply) append
// to the log. If the log grows past replyMaxBytes, oldest entries
// are evicted from the front (FIFO) and a "…(前 N 条已省略)" prefix
// is shown. The receipt's reaction emoji (⏳ / 🔄 / ✅) still swaps
// per F-25 spec — it shows the lifecycle, the log shows the work.
package feishu

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// receiptBot is the minimal Feishu surface the receipt depends
// on. *Adapter satisfies this via its existing methods. Tests pass
// a mock that records the calls. Keeping the field typed as an
// interface (instead of *Adapter) makes the receipt unit-testable
// without spinning up a real lark client.
type receiptBot interface {
	AddReaction(ctx context.Context, msgID, emoji string) (string, error)
	DeleteReaction(ctx context.Context, msgID, reactionID string) error
	UpdateMessage(ctx context.Context, messageID, text string) error
	SendMessageText(ctx context.Context, chatID, text string) (string, error)
}

// replyMaxBytes bounds the size of the rolling log message. Feishu's
// CreateMessage / UpdateMessage caps the content body at ~4 KiB; we
// stay under that to avoid implicit truncation by the platform.
const replyMaxBytes = 3500

// perEntryMaxBytes bounds a single log entry's payload so a giant
// 💬 reply line cannot monopolise the budget.
const perEntryMaxBytes = 600

// logEvictedMarker is the marker prepended when FIFO eviction has
// trimmed entries from the front of the log.
const logEvictedMarker = "…(前 %d 条已省略)"

// LogEntry is one row of the rolling activity log. The renderer
// stores entries in FIFO order; when total bytes exceed
// replyMaxBytes, the oldest are dropped and the count surfaces via
// logEvictedMarker.
type LogEntry struct {
	// Time is when the event arrived. Used only for sorting /
	// debugging; the rendered message shows eventKind+text, not
	// per-entry timestamps (Feishu's ⏳ header carries the live
	// clock already).
	Time time.Time

	// Icon is the unicode emoji that prefixes the line in chat
	// ("💭", "🔧", "✅", "💬", "❌"). Together with Text it forms
	// one rendered line.
	Icon string

	// Text is the entry body, already truncated to
	// perEntryMaxBytes.
	Text string

	// Kind is one of "thinking" | "tool_start" | "tool_end" |
	// "reply" | "error". Used by tests; not rendered.
	Kind string
}

// MessageReceipt is the per-user-message rolling-log display (F-25
// spec §6, v0.3 update). One receipt owns ONE Feishu reply message
// (the replyMsgID) and ONE reaction emoji on the user message. The
// reply message is updated in place via im.message.update as the log
// grows.
//
// Two visual tracks update in sync:
//
//  1. The reply text — a multi-line rolling log (header + entries).
//     Always ONE message per user message; the message grows over
//     the agent's lifetime. When it exceeds replyMaxBytes, oldest
//     entries are dropped from the front.
//  2. A single reaction emoji on the user message — ⏳ / 🔄 / ✅.
//     Swapped on lifecycle transitions (F-25 dual-track).
//
// Visual result on the user message:
//
//	🔄 ← single reaction emoji, swapped on every state change
//	💭 I'll explore the workspace...
//	🔧 codegraph_explore(/repo/api)
//	✅ codegraph_explore done
//	💬 Here's what I found…  ← all appended to one reply message
//	✅ 已完成 10:11:11
type MessageReceipt struct {
	chatID     string
	userMsgID  string
	replyMsgID string
	bot        receiptBot
	logger     *slog.Logger

	mu          sync.Mutex
	state       ReceiptState
	eventCount  int
	lastEventAt time.Time
	forwardedAt time.Time
	completedAt time.Time
	receivedAt  time.Time

	// entries is the rolling-log buffer (FIFO). Append grows it;
	// renderLocked evicts from the front when total bytes exceed
	// replyMaxBytes.
	entries []LogEntry

	// evicted tracks how many entries were dropped from the front
	// across the receipt's lifetime. Surfaced via logEvictedMarker.
	evicted int

	// currentReaction / currentReactionID implement the F-25
	// swap-on-state-change reaction strategy. Reaction is the
	// lifecycle indicator (lifecycle); the reply log is the work
	// indicator.
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
	// directly or after a buffer flush).
	StateExecuting

	// StateCompleted means Claude's result event has been received.
	// The receipt is terminal and no further updates happen.
	StateCompleted
)

// String renders the state as a short human label. Mostly useful for
// test diagnostics and structured logs.
func (s ReceiptState) String() string {
	switch s {
	case StateWaiting:
		return "waiting"
	case StateExecuting:
		return "executing"
	case StateCompleted:
		return "completed"
	}
	return "unknown"
}

// headerLine returns the single-line header that sits at the top of
// the reply message. It carries the lifecycle state (with the live
// clock for the executing case) and is the only thing users see
// when the log is empty (e.g. right after receipt creation).
func (s ReceiptState) headerLine(r *MessageReceipt) string {
	switch s {
	case StateWaiting:
		return "⏳ 等待中"
	case StateExecuting:
		if r == nil || r.lastEventAt.IsZero() {
			return "🔄 处理中"
		}
		return fmt.Sprintf("🔄 ⏳ %d · %s",
			r.eventCount,
			r.lastEventAt.Format("15:04:05"))
	case StateCompleted:
		if r == nil || r.completedAt.IsZero() {
			return "✅ 已完成"
		}
		return fmt.Sprintf("✅ 已完成 %s",
			r.completedAt.Format("15:04:05"))
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
// PARTY / …).
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
// before NewMessageReceipt returns. The reply starts as just the
// header line; entries are appended as events arrive via Append.
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

	// 2. Post the reply with just the header line. Subsequent
	//    events Append entries and re-render this message in
	//    place. Capture the returned message ID for edits.
	msgID, err := bot.SendMessageText(ctx, chatID, r.state.headerLine(r))
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
// reaction and writes the header with the initial event count.
// First-call only; subsequent SetExecuting calls are idempotent
// no-ops.
func (r *MessageReceipt) SetExecuting(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == StateCompleted {
		return nil
	}
	if r.state == StateExecuting {
		return nil
	}
	r.state = StateExecuting
	r.forwardedAt = time.Now()
	r.eventCount = 1
	r.lastEventAt = r.forwardedAt
	return r.renderLocked(ctx)
}

// Heartbeat refreshes the header timestamp so an inactive agent
// (e.g. mid tool-call, no events for a few seconds) still shows a
// ticking "🔄 ⏳ N · HH:MM:SS" line. No-op when not in Executing
// state.
func (r *MessageReceipt) Heartbeat(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != StateExecuting {
		return nil
	}
	r.lastEventAt = time.Now()
	return r.renderLocked(ctx)
}

// SetCompleted transitions Executing → Completed. Adds the ✅
// reaction and writes the final header. No new entry is appended —
// the work log stays as-is and the header signals completion.
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

// Append ingests one agent event and re-renders the reply message.
// All event kinds (thinking, tool_start, tool_end, text, done, error)
// flow through here — the renderer never calls SendMessage
// directly for these any more. Returns the rendered error (or nil)
// so callers can log per-event failures.
func (r *MessageReceipt) Append(ctx context.Context, ev agent.AgentEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state == StateCompleted {
		// Late event after completion — drop silently. Tool
		// echoes (Claude Code's stream-json user-role events)
		// can arrive after the result event; they're noise from
		// the user's perspective.
		return nil
	}

	// Terminal events are handled first — they don't carry an
	// entry (the header itself signals completion), and we want
	// to short-circuit out before eventToEntry's "skip"
	// branches swallow them.
	switch ev.Kind {
	case agent.EventDone:
		r.state = StateCompleted
		r.completedAt = time.Now()
		return r.renderLocked(ctx)
	case agent.EventError:
		entry, _ := eventToEntry(ev, time.Now())
		if entry.Text != "" || entry.Icon != "" {
			r.appendEntryLocked(entry)
		}
		r.state = StateCompleted
		r.completedAt = time.Now()
		return r.renderLocked(ctx)
	}

	entry, ok := eventToEntry(ev, time.Now())
	if !ok {
		// Unknown / unhandled event kind — keep going but don't
		// touch the log.
		return nil
	}

	if entry.Text != "" || entry.Icon != "" {
		r.appendEntryLocked(entry)
	}

	// State transition: the first non-empty entry promotes the
	// receipt from Waiting → Executing.
	if r.state == StateWaiting && (entry.Text != "" || entry.Icon != "") {
		r.state = StateExecuting
		r.forwardedAt = time.Now()
		r.eventCount = 1
		r.lastEventAt = r.forwardedAt
	}

	return r.renderLocked(ctx)
}

// appendEntryLocked pushes entry onto r.entries and evicts the
// oldest from the front until the rendered byte budget is met.
// Caller must hold r.mu.
func (r *MessageReceipt) appendEntryLocked(entry LogEntry) {
	if entry.Text == "" && entry.Icon == "" {
		return
	}
	r.entries = append(r.entries, entry)
	r.eventCount++
	r.lastEventAt = time.Now()
	r.evictOverflowLocked()
}

// evictOverflowLocked drops oldest entries until the rendered body
// fits under replyMaxBytes. Marks each drop in r.evicted so the
// rendered header can show "…(前 N 条已省略)".
//
// Caller must hold r.mu.
func (r *MessageReceipt) evictOverflowLocked() {
	for totalLogBytesLocked(r) > replyMaxBytes && len(r.entries) > 1 {
		r.entries = r.entries[1:]
		r.evicted++
	}
}

// totalLogBytesLocked returns the rendered byte size of header +
// entries (without the eviction marker — that's only added when
// r.evicted > 0).
//
// Caller must hold r.mu.
func totalLogBytesLocked(r *MessageReceipt) int {
	total := len(r.state.headerLine(r)) + 1 // +1 trailing newline
	for _, e := range r.entries {
		total += len(e.Icon) + 1 + len(e.Text) + 1 // "icon text\n"
	}
	return total
}

// formatLocked produces the full message body. Caller must hold
// r.mu.
func (r *MessageReceipt) formatLocked() string {
	var b strings.Builder
	b.WriteString(r.state.headerLine(r))
	b.WriteByte('\n')
	if r.evicted > 0 {
		fmt.Fprintf(&b, "\n"+logEvictedMarker+"\n", r.evicted)
	}
	for _, e := range r.entries {
		b.WriteString(e.Icon)
		b.WriteByte(' ')
		b.WriteString(e.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

// renderLocked pushes the current state emoji (reaction) and the
// rendered log (reply text) to Feishu. Caller must hold r.mu.
//
// Reaction strategy: SWAP, never accumulate. The previous reaction
// (if any) is deleted and the new state emoji is added in its place.
// Feishu has no UpdateReaction API, so the swap is Delete + Create.
// The user always sees exactly ONE reaction emoji per user message.
//
// Reply text strategy: edit-in-place via im.message.update. Falls
// back to a fresh message if update fails (e.g. message older than
// 48h, or update quota exhausted).
func (r *MessageReceipt) renderLocked(ctx context.Context) error {
	// 1. Swap reaction if the state emoji changed.
	emoji := r.state.Emoji()
	if emoji != r.currentReaction {
		if r.currentReactionID != "" {
			if err := r.bot.DeleteReaction(ctx, r.userMsgID, r.currentReactionID); err != nil {
				r.logger.Warn("feishu receipt: delete old reaction failed",
					"err", err, "old_emoji", r.currentReaction)
			}
		}
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
	text := r.formatLocked()
	if r.replyMsgID != "" {
		err := r.bot.UpdateMessage(ctx, r.replyMsgID, text)
		if err == nil {
			return nil
		}
		r.logger.Warn("feishu receipt: update reply failed, posting fresh",
			"err", err, "state", r.state)
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