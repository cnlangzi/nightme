// Package feishu — F-25 MessageReceipt with the rolling-log
// single-card format (v1.1 canonical design; reverted from a brief
// per-event-fresh-message detour in commit dd91e44). Each user
// message gets ONE reply in chat that grows over the agent's
// lifetime:
//
//	⏳ 等待中
//
//	💭 I'll explore the workspace...
//	🔧 codegraph_explore(/repo/api)
//	✅ codegraph_explore done
//	💬 Here's the API handler I found at line 42:
//	    <reply text — may span multiple lines>
//
//	─────────
//	Agent: claude | cwd: ~/code/nightme | tokens: 8.8K / 4.5K
//
//	✅ 已完成 10:11:11
//
// All events (thinking, tool call, tool result, final reply) append
// to the log. The receipt's reply card is posted via the Feishu
// ReplyMessage API so the chat surface shows a "Reply to <user>:
// <preview>" header above the body — Feishu's native pair-the-
// message visual cue. The card body is then edited in place via
// im.message.update as the log grows.
//
// If the log grows past replyMaxBytes, oldest entries are evicted
// from the front (FIFO) and a "…(前 N 条已省略)" prefix is shown.
// The receipt's reaction emoji (⏳ / 🔄 / ✅ / ❌) is APPEND-ONLY
// per state transition — each new state adds an emoji on top of
// the previous trail. We do not delete the old one because a
// failed delete leaves the old reaction in place and Feishu
// rejects the next add with code 99992354. Same-state renders
// (heartbeats) skip the duplicate add.
//
// The footer line ("─────────" + session attribution) is a single
// string supplied by the caller via SetFooter. nightme does NOT
// track tokens or session metadata itself — those come from the
// agent's internal session context (via the gateway's event
// stream) and the caller composes the footer. This keeps the
// receipt a pure renderer and the chat surface free of
// nightme-internal state that has to be kept in sync with the
// agent's own accounting.
// Package feishu — F-25 MessageReceipt with the rolling-log
// single-card format (v1.1 canonical design; reverted from a brief
// per-event-fresh-message detour in commit dd91e44). Each user
// message gets ONE reply in chat that grows over the agent's
// lifetime:
//
//	⏳ 等待中
//
//	💭 I'll explore the workspace...
//	🔧 codegraph_explore(/repo/api)
//	✅ codegraph_explore done
//	💬 Here's the API handler I found at line 42:
//	    <reply text — may span multiple lines>
//
//	─────────
//	Agent: claude | cwd: ~/code/nightme | tokens: 8.8K / 4.5K
//
//	✅ 已完成 10:11:11
//
// All events (thinking, tool call, tool result, final reply) append
// to the log. The receipt's reply card is posted via the Feishu
// ReplyMessage API so the chat surface shows a "Reply to <user>:
// <preview>" header above the body — Feishu's native pair-the-
// message visual cue. The card body is then edited in place via
// im.message.update as the log grows.
//
// If the log grows past replyMaxBytes, oldest entries are evicted
// from the front (FIFO) and a "…(前 N 条已省略)" prefix is shown.
// The receipt's reaction emoji (⏳ / 🔄 / ✅ / ❌) is append-only
// per state transition — each new state adds an emoji on top of
// the previous trail (see note above).
//
// The footer line ("─────────" + session attribution) is a single
// string supplied by the caller via SetFooter. nightme does NOT
// track tokens or session metadata itself — those come from the
// agent's internal session context (via the gateway's event
// stream) and the caller composes the footer. This keeps the
// receipt a pure renderer and the chat surface free of
// nightme-internal state that has to be kept in sync with the
// agent's own accounting.
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
//
// ReplyMessage creates a Feishu reply anchored to userMsgID — Feishu
// renders a "Reply to <user>: <preview>" UI above the body, which
// is the visual cue users rely on to pair a bot reply with the
// triggering user message. NewMessageReceipt posts via ReplyMessage
// (when userMsgID is known, the CreateReceipt path) and falls back
// to SendMessageText only when no userMsgID is available (the
// cold-start synthetic-userMsgID path in Adapter.receiptFor).
type receiptBot interface {
	AddReaction(ctx context.Context, msgID, emoji string) (string, error)
	UpdateMessage(ctx context.Context, messageID, text string) error
	SendMessageText(ctx context.Context, chatID, text string) (string, error)
	ReplyMessage(ctx context.Context, chatID, userMsgID, text string) (string, error)
}

// replyMaxBytes bounds the size of the rolling log message. Feishu's
// CreateMessage / UpdateMessage caps the content body at ~4 KiB; we
// stay under that to avoid implicit truncation by the platform.
const replyMaxBytes = 3500

// perEntryMaxBytes bounds a single log entry's payload so a giant
// 💬 reply line cannot monopolise the budget.
const perEntryMaxBytes = 600

// minBodyUpdateInterval is the per-receipt cooldown between
// UpdateMessage calls. Feishu's per-message update quota (code
// 230001) is 5/sec burst; rapid event bursts (5 events in ~4s)
// would otherwise hit the limit and silently drop the terminal
// state update. Terminal transitions bypass this cooldown.
const minBodyUpdateInterval = 300 * time.Millisecond

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
// spec §6, v1.1). One receipt owns ONE Feishu reply message
// (the replyMsgID, posted via ReplyMessage so the chat shows the
// "Reply to <user>: <preview>" header above the body) and ONE
// reaction emoji on the user message. The reply message is updated
// in place via im.message.update as the log grows.
//
// Two visual tracks update in sync:
//
//  1. The reply text — a multi-line rolling log (header + entries
//     + optional footer). Always ONE message per user message;
//     the message grows over the agent's lifetime. When it
//     exceeds replyMaxBytes, oldest entries are dropped from the
//     front.
//  2. A single reaction emoji on the user message — ⏳ / 🔄 / ✅ /
//     ❌. Swapped on lifecycle transitions (F-25 dual-track).
//
// Visual result on the user message:
//
//	🔄 ← single reaction emoji, swapped on every state change
//	💭 I'll explore the workspace...
//	🔧 codegraph_explore(/repo/api)
//	✅ codegraph_explore done
//	💬 Here's what I found…  ← all appended to one reply message
//	─────────
//	Agent: claude | cwd: ~/code/nightme | tokens: 8.8K / 4.5K
//	✅ 已完成 10:11:11
//
// The footer line is a single string set by the caller via
// SetFooter. The receipt does NOT compose the line — that is the
// adapter's job (the agent's events carry the raw values; the
// adapter formats them). Keeping the receipt ignorant of token
// semantics means nightme doesn't maintain duplicate state for
// values the agent already tracks in its own session context.
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

	// lastBodyUpdate tracks the last successful Feishu
	// UpdateMessage call on the reply card. renderLocked uses
	// this to throttle body updates so we don't exceed the
	// Feishu per-message update quota (code 230001 — "更新消息
	// 频率过快", 5/sec burst). Terminal transitions (Done /
	// Error) bypass the throttle so the user always sees the
	// final state.
	lastBodyUpdate time.Time

	// lastBodyText is the body hash from the last successful
	// UpdateMessage. renderLocked skips the call when formatLocked()
	// hasn't changed since the last write — this is the
	// strongest dedup, since it catches the heartbeat / no-op
	// transitions without per-event bookkeeping.
	lastBodyText string

	// entries is the rolling-log buffer (FIFO). Append grows it;
	// renderLocked evicts from the front when total bytes exceed
	// replyMaxBytes.
	entries []LogEntry

	// evicted tracks how many entries were dropped from the front
	// across the receipt's lifetime. Surfaced via logEvictedMarker.
	evicted int

	// currentReaction tracks the last successfully-added lifecycle
	// reaction emoji so same-state renders (heartbeats) skip a
	// duplicate AddReaction. Reactions are append-only — previous
	// state emojis stay on the user message as history.
	currentReaction string

	// footer is the session-attribution line rendered at the
	// bottom of the reply body (below the rolling log entries,
	// above the terminal state header). The receipt does NOT
	// compose this string — the caller (Feishu adapter) builds
	// the line from agent events (OutInit for static session
	// context, OutUsage for token counts) and stamps the result
	// onto the receipt via SetFooter. This keeps the receipt
	// package ignorant of session/usage semantics: nightme
	// doesn't track tokens itself, it just relays whatever the
	// agent's internal session context reports.
	footer string
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

	// 1. Add the initial lifecycle reaction. Reactions are append-only;
	//    the returned ID is not needed because receipts never delete them.
	_, err := bot.AddReaction(ctx, userMsgID, StateWaiting.Emoji())
	if err != nil {
		r.logger.Warn("feishu receipt: initial reaction failed", "err", err)
	} else {
		r.mu.Lock()
		r.currentReaction = StateWaiting.Emoji()
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
// renderLocked to append reactions and ship reply text.
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
	var lastEntry *LogEntry
	if len(r.entries) > 0 {
		lastEntry = &r.entries[len(r.entries)-1]
	}
	switch ev.Kind {
	case agent.EventDone:
		r.state = StateCompleted
		r.completedAt = time.Now()
		return r.renderLocked(ctx)
	case agent.EventError:
		entry, _ := eventToEntry(ev, time.Now(), lastEntry)
		if entry.Text != "" || entry.Icon != "" {
			r.appendEntryLocked(entry)
		}
		r.state = StateCompleted
		r.completedAt = time.Now()
		return r.renderLocked(ctx)
	}

	entry, ok := eventToEntry(ev, time.Now(), lastEntry)
	if !ok {
		// Unknown / unhandled event kind — keep going but don't
		// touch the log.
		return nil
	}

	// Thinking aggregation: merge consecutive thinking entries
	// into the previous one (separated by a horizontal rule) so
	// Feishu's native code-block auto-collapse kicks in when the
	// merged block crosses the 4-line threshold. The receipt
	// itself doesn't render markdown — formatLocked wraps the
	// merged text in ``` fences at render time.
	if entry.Kind == "thinking" && len(r.entries) > 0 &&
		r.entries[len(r.entries)-1].Kind == "thinking" {
		last := &r.entries[len(r.entries)-1]
		last.Text = last.Text + "\n---\n" + entry.Text
		r.eventCount++
		r.lastEventAt = time.Now()
		return r.renderLocked(ctx)
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
//
// Thinking entries are rendered as markdown ``` code blocks.
// Feishu auto-collapses any code block longer than a few lines
// and shows an "N 行代码 >" expand button — so consecutive
// thinking events Append has merged into a single entry render
// here as a single collapsed block the user can expand on demand.
// All other entries render inline (icon + text on one line).
func (r *MessageReceipt) formatLocked() string {
	var b strings.Builder
	b.WriteString(r.state.headerLine(r))
	b.WriteByte('\n')
	if r.evicted > 0 {
		fmt.Fprintf(&b, "\n"+logEvictedMarker+"\n", r.evicted)
	}
	for _, e := range r.entries {
		if e.Kind == "thinking" {
			b.WriteString(e.Icon)
			b.WriteString("\n```\n")
			b.WriteString(e.Text)
			b.WriteString("\n```\n")
			continue
		}
		b.WriteString(e.Icon)
		b.WriteByte(' ')
		b.WriteString(e.Text)
		b.WriteByte('\n')
	}
	if r.footer != "" {
		fmt.Fprintf(&b, "\n─────────\n%s\n", r.footer)
	}
	return b.String()
}

// SetFooter stores the session-attribution line rendered at the
// bottom of the reply body. The caller (typically the Feishu
// adapter composing the line from OutInit + OutUsage events)
// owns the format. Empty string drops the footer entirely so
// receipts without session attribution don't render a dangling
// separator.
//
// SetFooter is safe to call concurrently; the receipt does NOT
// re-render the chat surface automatically — the next Append /
// applyState triggers the UpdateMessage that paints the new
// footer in place.
func (r *MessageReceipt) SetFooter(footer string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.footer = footer
}

// Footer returns the current footer string (empty when unset).
// Used by tests; not part of the channel.Channel interface.
func (r *MessageReceipt) Footer() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.footer
}

// renderLocked pushes the current state emoji (reaction) and the
// rendered log (reply text) to Feishu. Caller must hold r.mu.
//
// Reaction strategy: append-only. When the lifecycle state changes, the
// new predefined Feishu reaction is added and previous reactions are
// retained as history. Deleting first is intentionally avoided because a
// failed delete can leave the old reaction in place and make Feishu reject
// the new add with code 99992354. Same-state renders (heartbeats) do not
// add duplicate reactions. Reaction failures are best-effort and never
// block shipping the reply text.
//
// Reply text strategy: edit-in-place via im.message.update. Falls
// back to a fresh message if update fails (e.g. message older than
// 48h, or update quota exhausted). One user message → ONE
// Feishu reply card; the card body grows over the agent's
// lifetime as the rolling-log entries append.
func (r *MessageReceipt) renderLocked(ctx context.Context) error {
	// 1. Append a reaction when the lifecycle state changes.
	r.appendReactionLocked(ctx, r.state.Emoji())

	// 2. Update the reply text in place. Throttled + deduped so
	// rapid event bursts don't exceed Feishu's per-message
	// update quota (code 230001 — "更新消息频率过快", 5/sec
	// burst). Terminal transitions (Done / Error) bypass the
	// throttle so the user always sees the final state.
	text := r.formatLocked()
	if text == r.lastBodyText {
		// Body unchanged — nothing to write. Skipping the
		// call entirely is the strongest dedup; it covers
		// every repeat-renderLocked case (heartbeat, no-op
		// transitions, idempotent state writes).
		return nil
	}
	isTerminal := r.state == StateCompleted || r.state == StateError
	if !isTerminal && !r.lastBodyUpdate.IsZero() &&
		time.Since(r.lastBodyUpdate) < minBodyUpdateInterval {
		// Throttle: skip this update; the last write is
		// recent enough that another one would just hit the
		// rate limit. The next renderLocked (after another
		// event arrives past the interval, or at terminal
		// time) writes the full body.
		return nil
	}
	if r.replyMsgID != "" {
		err := r.bot.UpdateMessage(ctx, r.replyMsgID, text)
		if err == nil {
			r.lastBodyText = text
			r.lastBodyUpdate = time.Now()
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
	r.lastBodyText = text
	r.lastBodyUpdate = time.Now()
	return nil
}

// appendReactionLocked adds emoji when it differs from the last
// successful add. Caller must hold r.mu. Failures are logged and
// never returned — currentReaction stays unchanged so a later
// render can retry.
func (r *MessageReceipt) appendReactionLocked(ctx context.Context, emoji string) {
	if emoji == "" || emoji == r.currentReaction {
		return
	}
	if _, err := r.bot.AddReaction(ctx, r.userMsgID, emoji); err != nil {
		r.logger.Warn("feishu receipt: add reaction failed",
			"err", err, "emoji", emoji, "state", r.state)
		return
	}
	r.currentReaction = emoji
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
// ReceiptState + append-only reaction action.
//
// For Pending / Executing / Done the rendered behavior matches
// the legacy SetExecuting / SetCompleted / SetCompleted-equivalent
// paths. For Error (new in v1.1) we flip to StateError and append
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

	// Append reaction (best-effort). Feishu's reaction API errors
	// don't fail the lifecycle transition.
	r.appendReactionLocked(ctx, targetInternal.Emoji())

	r.logger.Debug("feishu receipt: state transition",
		"from", "?", "to", targetInternal, "user_msg_id", r.userMsgID)
	return nil
}

// dispose clears the reply body. Idempotent. Lifecycle reactions are
// left in place as the append-only history trail.
// Called by Adapter.DisposeReceipt after the final state transition.
// v1.1 keeps the receipt in the adapter's indexes until the next
// SendUserMessage call so legacy fallback paths that look up by
// userMsgID keep working until commit 3 migrates them.
func (r *MessageReceipt) dispose(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

// The reply card (rolling log + footer) and the append-only
	// reaction trail are PRESERVED on the Feishu side — they
	// are the user's permanent record of the conversation turn.
	// dispose only tears down our internal handles so the adapter
	// can GC the receipt object.
//
// Previous designs called UpdateMessage("") here to "clean up"
	// the chat surface, but that interacted badly with the
	// rate-limit-throttled body updates: if the terminal
	// UpdateMessage had been rate-limited away, the dispose
	// would still strip the body, leaving the user with a
	// partially-rendered receipt. Keeping the Feishu-side
	// state as the source of truth avoids that divergence.
//
// Reactions are append-only per main's fix(feishu): append-only
// receipt reactions (#4) — there is no currentReactionID to
// clear here; the trail accumulates across the receipt's
// lifetime.
	r.replyMsgID = ""
	return nil
}
