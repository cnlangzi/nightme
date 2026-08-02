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
	"github.com/cnlangzi/nightme/internal/channel"
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
//
// StateWaiting / StateExecuting / StateCompleted predate the v1.1
// four-state cross-channel enum (channel.ReceiptState). They remain
// in use because the existing adapter-internal paths
// (SendUserMessage / MarkExecuting / SetExecuting / SetCompleted /
// Append) flow through them; the new v1.1 path
// (channel.CreateReceipt → channel.UpdateReceipt) translates the
// cross-channel ReceiptState into one of these via applyState
// (see bottom of this file).
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

	// StateError means dispatch or processing failed (v1.1). The
	// receipt is terminal and the user should retry.
	StateError
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
	case StateError:
		return "error"
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
	case StateError:
		return "THUMBSUP" // closest Feishu-predefined indicator of "failed"
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

// NewMessageReceiptForReply wraps an already-posted reply message
// (the caller is responsible for sending the initial text and
// adding the ⏳ reaction). The returned receipt is registered in
// the adapter's indexes so subsequent Append calls update the
// same reply. Use this when the adapter owns the full lifecycle
// (Stage 3): the gateway's fallback calls adapter.SendUserMessage
// which posts the reply and constructs the receipt in one place.
//
// bot is the adapter (or any receiptBot implementation) used by
// renderLocked to swap reactions and edit the reply text in place.
// Stage 3 callers pass `a`; passing nil will panic on the first
// Append, since renderLocked calls r.bot.AddReaction directly.
// (Earlier versions left bot=nil under the assumption that "callers
// go through the adapter methods" — but renderLocked does not, and
// the synthetic cold-start path (adapter.receiptFor) constructs a
// receipt via this function before any event has reached the
// adapter's Send switch. See CHANGELOG v0.3 Stage 3 + the
// `recover renderLocked panic` follow-up.)
func NewMessageReceiptForReply(chatID, userMsgID, replyMsgID string, bot receiptBot) *MessageReceipt {
	return &MessageReceipt{
		chatID:     chatID,
		userMsgID:  userMsgID,
		replyMsgID: replyMsgID,
		bot:        bot,
		logger:     slog.Default(),
		receivedAt: time.Now(),
		state:      StateWaiting,
	}
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

// formatEntry produces the body for a single-entry "reply" message.
// Caller must hold r.mu. Used by the per-event update strategy
// where each agent event ships as its own message rather than as
// an in-place update of a rolling log card. The state header is
// omitted because each event carries its own context; the chat
// surface is the sequence of messages, not a single card.
func (r *MessageReceipt) formatEntry(e LogEntry) string {
	if e.Text == "" && e.Icon == "" {
		return ""
	}
	var b strings.Builder
	if e.Icon != "" {
		b.WriteString(e.Icon)
		b.WriteByte(' ')
	}
	b.WriteString(e.Text)
	return b.String()
}

// renderLocked pushes the current state emoji (reaction) and the latest
// entry to Feishu. Caller must hold r.mu.
//
// Reaction strategy: SWAP, never accumulate. The previous reaction
// (if any) is deleted and the new state emoji is added in its place.
// Feishu has no UpdateReaction API, so the swap is Delete + Create.
// The user always sees exactly ONE reaction emoji per user message.
//
// Reply text strategy: PER-EVENT FRESH MESSAGE. Each agent event
// ships as its own im.message.create rather than as an in-place
// update of a rolling-log card. The pre-refactor design collapsed
// every event into a single message that was updated N times; the
// new design emits N messages so the chat surface mirrors the
// event stream one-for-one. The initial ⏳ card (posted by
// SendUserMessage) is preserved as the receipt's "I'm thinking"
// indicator; events after that point arrive as new messages.
//
// The receipt's rolling-log buffer (entries / evicted) is kept for
// audit trails and future replay support but is no longer
// rendered into a single card body. r.replyMsgID is updated to the
// latest shipped message so the receipt can still address its
// surface if a future feature needs to.
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

	// 2. Send the latest entry as a fresh message. We emit one
	// message per agent event rather than updating an existing
	// card in place — the chat surface is now the event stream,
	// not a single rolling-log card. Skip empty entries (header
	// transitions without text, etc.) so we don't post empty
	// messages.
	if len(r.entries) == 0 {
		return nil
	}
	latest := r.entries[len(r.entries)-1]
	text := r.formatEntry(latest)
	if text == "" {
		return nil
	}
	msgID, err := r.bot.SendMessageText(ctx, r.chatID, text)
	if err != nil {
		r.logger.Warn("feishu receipt: ship entry failed",
			"err", err, "state", r.state, "icon", latest.Icon, "text", truncateForLog(latest.Text, 80))
		return fmt.Errorf("feishu receipt: ship entry: %w", err)
	}
	r.replyMsgID = msgID
	return nil
}

// --- v1.1 unified state machine ---
//
// applyState and dispose are the v1.1 receipt lifecycle entry
// points used by Adapter.UpdateReceipt / Adapter.DisposeReceipt.
// Gateway is the only caller; receipts flow as opaque
// channel.Receipt handles.

// applyState transitions the receipt to the cross-channel state
// `target`. Idempotent for the same target. Translates the
// cross-channel ReceiptState enum into the legacy internal
// ReceiptState + reaction-swap action.
//
// For Pending / Executing / Done the rendered behavior matches
// the legacy SetExecuting / SetCompleted / SetCompleted-equivalent
// paths. For Error (new in v1.1) we flip to StateError and swap
// the reaction emoji; no further event updates are accepted.
func (r *MessageReceipt) applyState(ctx context.Context, target channel.ReceiptState) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var targetInternal ReceiptState
	switch target {
	case channel.ReceiptPending:
		targetInternal = StateWaiting
	case channel.ReceiptExecuting:
		targetInternal = StateExecuting
		// Bookkeeping mirrors SetExecuting so the ⌚ timestamp in
		// any subsequent header render is populated.
		if r.state == StateWaiting {
			r.forwardedAt = time.Now()
			r.eventCount = 1
			r.lastEventAt = r.forwardedAt
		}
	case channel.ReceiptDone:
		targetInternal = StateCompleted
		if r.state != StateCompleted {
			r.completedAt = time.Now()
		}
	case channel.ReceiptError:
		targetInternal = StateError
		if r.state != StateCompleted && r.state != StateError {
			r.completedAt = time.Now()
		}
	default:
		return fmt.Errorf("feishu receipt: unknown ReceiptState %d", target)
	}

	// Idempotent: already in this state, no work.
	if r.state == targetInternal {
		return nil
	}

	// StateError is terminal — once entered, ignore further
	// transitions to non-Done states (Done is allowed after Error
	// for partial recovery; ignore everything else).
	r.state = targetInternal

	// Swap reaction (best-effort). Feishu's reaction API errors
	// don't fail the lifecycle transition.
	emoji := targetInternal.Emoji()
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
				"err", err, "emoji", emoji, "state", targetInternal)
		} else {
			r.currentReaction = emoji
			r.currentReactionID = newID
		}
	}

	r.logger.Debug("feishu receipt: state transition",
		"from", "?", "to", targetInternal, "user_msg_id", r.userMsgID)
	return nil
}

// dispose deletes the receipt's reply message + reaction. Idempotent.
// Called by Adapter.DisposeReceipt after the final state transition.
// v1.1 keeps the receipt in the adapter's indexes until the next
// SendUserMessage call so legacy fallback paths that look up by
// userMsgID keep working until commit 3 migrates them.
func (r *MessageReceipt) dispose(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.replyMsgID != "" {
		if err := r.bot.UpdateMessage(ctx, r.replyMsgID, ""); err != nil {
			// Feishu's UpdateMessage may not support empty body; ignore.
			r.logger.Debug("feishu receipt: clear body noop",
				"err", err, "msg_id", r.replyMsgID)
		}
	}
	if r.currentReactionID != "" {
		if err := r.bot.DeleteReaction(ctx, r.userMsgID, r.currentReactionID); err != nil {
			r.logger.Warn("feishu receipt: dispose delete reaction failed",
				"err", err, "msg_id", r.userMsgID)
		}
		r.currentReactionID = ""
	}
	return nil
}
