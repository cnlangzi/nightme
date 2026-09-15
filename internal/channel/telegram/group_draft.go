package telegram

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
)

// Batch window parameters for the simulated DraftMessage in
// ChatKind=group. Hard-coded per the design decision (no cfg):
//
//   - 10 events triggers an immediate flush (count-based)
//   - 10s after the first buffered event triggers a flush (time-based)
//   - whichever fires first
//
// 1 flush / 10s per (chat, topic, turn) stays comfortably below
// Telegram's 1/s per-chat and 20/min per-group limits. Both the first
// sendRichMessage (cold-create) and subsequent editMessageText flushes
// follow the same trigger rules — there is no "immediate cold-create
// on first event" fast path.
const (
	groupDraftBatchMaxEvents = 10
	groupDraftBatchInterval  = 10 * time.Second
)

// groupDraftEntry is the per-turn in-memory state for the simulated
// DraftMessage surface used by ChatKind=group. The buffer is a
// windowed slice of richTurnEntry (one paragraph block per entry);
// flushes happen at 10 events or 10s after the first buffered event,
// whichever comes first. After a successful flush the buffer is
// cleared (windowed semantics): Telegram keeps showing the last
// flushed snapshot until the next window replaces it.
//
// messageID is the Telegram message_id returned by the first flush
// (cold-create). 0 means "not yet sent". In-memory only — there is
// no persisted DraftMessageID, so a daemon restart mid-turn loses
// the in-progress DraftMessage (acceptable trade-off).
//
// All mutable fields are guarded by mu. The lock is held across the
// network roundtrip on streamDraftEvent / flushLocked so concurrent
// events on the same turn compose + send in serial order. timer
// callbacks also acquire mu before touching state.
//
// Per-prompt isolation (see docs §11.12.11.3) is enforced by the
// groupDraftKey already containing userMsgID: ensureEntry creates a
// fresh entry per turn, so cross-turn entry reuse cannot happen via
// the production keying.
type groupDraftEntry struct {
	mu sync.Mutex

	// Persistent identity — set by the first flush (cold-create via
	// sendRichMessage); survives the buffer-flush lifecycle so
	// subsequent flushes can PATCH the same Telegram message.
	messageID int

	// Buffer for the current window. Cleared after each successful
	// flush (windowed semantics — Telegram shows the last flushed
	// snapshot, the buffer holds the next).
	entries []richTurnEntry

	// Batch bookkeeping.
	pendingEventCount int  // logical events since last flush (OutToolStart+End = 1)
	pendingToolStart  bool // true while an OutToolStart is awaiting its End
	flushTimer        *time.Timer

	// Captured context for timer callbacks (which fire in their own
	// goroutine and need to re-acquire mu before touching state).
	rawChatID string
	topicID   int
	userMsgID int
}

// composeIntoEntriesLocked applies REPLACE / ACCUMULATE to the
// current window's entries. REPLACE wipes prior content; ACCUMULATE
// appends the new entry. Caller MUST hold e.mu.
func (e *groupDraftEntry) composeIntoEntriesLocked(body string, kind messages.OutboundKind) {
	replace := kind == messages.OutThinking || kind == messages.OutToolStart
	if replace {
		e.entries = e.entries[:0]
	}
	e.entries = append(e.entries, richTurnEntry{
		kind: kindToRichBlockKind(kind),
		body: body,
	})
}

// kindToRichBlockKind maps the OutboundKind to richTurnEntry.kind
// so the same rendering pipeline (richTurn.appendRichTurnAndFlush)
// can consume group_draft entries verbatim.
func kindToRichBlockKind(k messages.OutboundKind) string {
	switch k {
	case messages.OutThinking:
		return "thinking"
	case messages.OutToolStart, messages.OutToolEnd:
		return "tool"
	default:
		return ""
	}
}

// groupDraftManager owns the per-(chat, topic, userMsgID) entries
// for the "group" ChatKind's simulated DraftMessage surface. There
// is at most one active entry per turn; endProcess deletes the
// underlying Telegram message and drops the entry so the next turn
// starts with a clean slate.
//
// Lifecycle per turn:
//
//	OutThinking / OutToolStart / OutToolEnd
//	    → ensureEntry + compose into window buffer; pendingEventCount++
//	    → flush triggered by 10 events or 10s timer
//	    → first flush: sendRichMessage (cold-create)
//	    → subsequent flushes: editMessageText(messageID, …)
//	OutResult / OnPromptEnded
//	    → flush remaining buffered events (final flush)
//	    → deleteMessage + drop entry
//
// Failure semantics:
//
//   - sendRichMessage / editMessageText failure → buffer preserved,
//     return handled=true (issue #391 — fall through to richTurn
//     was leaking tool lines into the final answer message).
//   - No retry / no latch: every event accumulates into the buffer;
//     the next trigger (count or timer, or the next event that
//     pushes past 10) re-attempts the same flush. A sustained
//     outage means the user sees a "stale buffer" until the next
//     successful flush, never a missing event — events are queued
//     in memory regardless.
type groupDraftManager struct {
	api apiClient
	log *slog.Logger
	mu  sync.Mutex
	// entries is keyed by chatID|topicID|userMsgID. Map-level
	// reads/writes are guarded by mu; per-entry mutation is
	// guarded by entry.mu (taken by streamDraftEvent / flushLocked
	// / endProcess for the whole operation, by timer callbacks).
	entries map[string]*groupDraftEntry
}

func newGroupDraftManager(api apiClient, log *slog.Logger) *groupDraftManager {
	return &groupDraftManager{
		api:     api,
		log:     log,
		entries: make(map[string]*groupDraftEntry),
	}
}

func groupDraftKey(chatID string, topicID int, userMsgID int) string {
	return chatID + "|" + stringInt(topicID) + "|" + stringInt(userMsgID)
}

// ensureEntry returns the entry for a turn, allocating one on first
// access. Caller holds the manager mutex.
func (m *groupDraftManager) ensureEntry(chatID string, topicID int, userMsgID int) *groupDraftEntry {
	key := groupDraftKey(chatID, topicID, userMsgID)
	if e, ok := m.entries[key]; ok {
		return e
	}
	e := &groupDraftEntry{
		rawChatID: chatID,
		topicID:   topicID,
		userMsgID: userMsgID,
	}
	m.entries[key] = e
	return e
}

// streamDraftEvent applies one OutThinking / OutToolStart /
// OutToolEnd event to the simulated DraftMessage for this turn:
// composes the in-memory buffer (windowed) and arms the flush
// triggers. The actual Telegram API call happens in flushLocked
// when 10 events accumulate or 10s elapse.
//
// The boolean mirrors streamDraftEvent's DM-side contract — true
// means "consumed, do not fall through to the richTurn path".
//
// userMsgID is BOTH the entry key (one entry per turn) and the
// reply_to_message_id anchor on the cold-create sendRichMessage —
// the DraftMessage visually hangs off the user's message like every
// other turn message. Per-prompt isolation is enforced by the
// groupDraftKey already containing userMsgID; the caller is
// responsible for passing the per-event anchor (msg.ReplyTo), see
// docs/channel/telegram.md §11.12.11.3.
func (m *groupDraftManager) streamDraftEvent(ctx context.Context, rawChatID string, topicID int, userMsgID int, segment string, kind messages.OutboundKind) (bool, error) {
	if userMsgID <= 0 {
		// No user message anchor → can't safely reply_to the user's
		// message. Bail to caller fallthrough (richTurn path renders it).
		m.log.Info("telegram: streamDraftEvent fallthrough (userMsgID<=0)",
			"chat_id", rawChatID,
			"thread_id", topicID,
			"user_msg_id", userMsgID,
			"kind", kind.String(),
		)
		return false, nil
	}
	m.mu.Lock()
	entry := m.ensureEntry(rawChatID, topicID, userMsgID)
	m.mu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()

	// REPLACE wipes prior body (OutThinking / OutToolStart — single
	// visual surface for the latest event). ACCUMULATE stacks the
	// result line under the matching tool start (OutToolEnd).
	replace := kind == messages.OutThinking || kind == messages.OutToolStart
	// OutToolStart+End pair counts as one logical event for the
	// batch threshold; an OutToolEnd with no pending Start (orphan)
	// is a new event on its own.
	isNewLogicalEvent := replace || (kind == messages.OutToolEnd && !entry.pendingToolStart)
	if isNewLogicalEvent {
		entry.pendingEventCount++
	}
	entry.pendingToolStart = (kind == messages.OutToolStart)
	entry.composeIntoEntriesLocked(segment, kind)

	m.log.Info("telegram: streamDraftEvent buffered",
		"chat_id", rawChatID,
		"thread_id", topicID,
		"user_msg_id", userMsgID,
		"kind", kind.String(),
		"pending_event_count", entry.pendingEventCount,
		"buffered_entries", len(entry.entries),
		"message_id", entry.messageID,
	)

	m.startFlushTimerIfIdleLocked(entry)
	if entry.pendingEventCount >= groupDraftBatchMaxEvents {
		return m.flushLocked(ctx, entry, rawChatID, topicID, "count")
	}
	return true, nil
}

// flushLocked serializes the current entries buffer as a rich
// message and PATCHes the Telegram message via editMessageText (or
// sendRichMessage for the first flush). On success the buffer is
// cleared (windowed semantics); on failure the buffer is preserved
// for the next streamDraftEvent to retry on top of (issue #391 —
// fall through would leak tool result lines into the rich turn body).
//
// Caller MUST hold entry.mu.
func (m *groupDraftManager) flushLocked(ctx context.Context, entry *groupDraftEntry, rawChatID string, topicID int, reason string) (bool, error) {
	if len(entry.entries) == 0 {
		return true, nil
	}

	blocksJSON, err := buildParagraphBlocksForEntries(entry.entries)
	if err != nil {
		return true, err
	}

	params := map[string]any{
		"chat_id":      rawChatID,
		"rich_message": json.RawMessage(blocksJSON),
	}
	if topicID > 0 {
		params["message_thread_id"] = topicID
	}

	var method string
	if entry.messageID == 0 {
		// First flush: cold-create via sendRichMessage. The
		// DraftMessage visually hangs off the user's message so it
		// sits in the same chain as the rich turn placeholder.
		method = "sendRichMessage"
		if entry.userMsgID > 0 {
			params["reply_to_message_id"] = entry.userMsgID
		}
		var result SendMessageResult
		if err := m.api.call(ctx, "sendRichMessage", params, &result); err == nil && result.MessageID > 0 {
			entry.messageID = result.MessageID
		} else if err == nil {
			err = &apiError{Message: "telegram: sendRichMessage returned empty message_id"}
		}
	} else {
		// Subsequent flush: PATCH the existing Telegram message in
		// place. The rich_message envelope is the same shape as the
		// cold-create body so the user sees a continuous message.
		method = "editMessageText"
		params["message_id"] = entry.messageID
		err = m.api.call(ctx, "editMessageText", params, nil)
	}

	if err != nil {
		// Send failed: stay on the draft path. The entry (and
		// messageID on subsequent attempts) stays intact so the
		// next trigger retries on top of the prior buffer. Returning
		// false here would let the caller fall through to the rich
		// turn path and leak tool result lines (`⎿ 🔧 tool → N
		// bytes`) into the final answer message — see issue #391.
		m.log.Warn("telegram: group DraftMessage flush failed",
			"chat_id", rawChatID,
			"thread_id", topicID,
			"method", method,
			"message_id", entry.messageID,
			"blocks", len(entry.entries),
			"reason", reason,
			"err", err,
		)
		return true, nil
	}

	m.log.Info("telegram: group DraftMessage flushed",
		"chat_id", rawChatID,
		"thread_id", topicID,
		"method", method,
		"message_id", entry.messageID,
		"blocks", len(entry.entries),
		"reason", reason,
	)
	entry.entries = entry.entries[:0]
	entry.pendingEventCount = 0
	entry.pendingToolStart = false
	if entry.flushTimer != nil {
		entry.flushTimer.Stop()
		entry.flushTimer = nil
	}
	return true, nil
}

// startFlushTimerIfIdleLocked arms the 10s debounce timer if no
// timer is already pending. When it fires the buffered entries are
// flushed via flushLocked. Caller MUST hold entry.mu.
func (m *groupDraftManager) startFlushTimerIfIdleLocked(entry *groupDraftEntry) {
	if entry.flushTimer != nil {
		return
	}
	entry.flushTimer = time.AfterFunc(groupDraftBatchInterval, func() {
		entry.mu.Lock()
		defer entry.mu.Unlock()
		if entry.pendingEventCount == 0 || len(entry.entries) == 0 {
			entry.flushTimer = nil
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// timer fires asynchronously; flushLocked logs + preserves
		// the buffer on failure, so we don't propagate the error.
		_, _ = m.flushLocked(ctx, entry, entry.rawChatID, entry.topicID, "timer")
	})
}

// endProcess finalizes the simulated DraftMessage for a turn:
// flushes any buffered events (so the last window isn't lost),
// deletes the underlying Telegram message, and drops the in-memory
// entry. Mirrors the DM draft's auto-disappear-on-real-message
// behavior (#383).
//
// Safe to call when no DraftMessage exists for the turn (no-op).
// Called from adapter.OnPromptEnded (safety net for turns without
// OutResult) and from the OutResult send path. Idempotent across
// repeated calls.
func (m *groupDraftManager) endProcess(ctx context.Context, rawChatID string, topicID int, userMsgID int) {
	if userMsgID <= 0 {
		return
	}
	m.mu.Lock()
	entry, ok := m.entries[groupDraftKey(rawChatID, topicID, userMsgID)]
	if ok {
		delete(m.entries, groupDraftKey(rawChatID, topicID, userMsgID))
	}
	m.mu.Unlock()

	if !ok {
		// No in-memory entry. The DraftMessage is purely in-memory
		// state (no persisted DraftMessageID); a daemon restart
		// leaves any in-flight DraftMessage orphaned in the chat,
		// which is acceptable — the next turn starts fresh.
		return
	}
	entry.mu.Lock()
	// Stop the timer before flush so a stale timer doesn't fire
	// after endProcess has deleted the message.
	if entry.flushTimer != nil {
		entry.flushTimer.Stop()
		entry.flushTimer = nil
	}
	// Flush any buffered events first so the user sees the final
	// window before we delete the message. Failure is non-fatal —
	// the buffer is about to be discarded with the entry.
	if len(entry.entries) > 0 {
		_, _ = m.flushLocked(ctx, entry, rawChatID, topicID, "endProcess")
	}
	msgID := entry.messageID
	entry.mu.Unlock()

	if msgID > 0 {
		m.deleteOrphan(ctx, rawChatID, topicID, msgID)
	}
}

// deleteOrphan removes a DraftMessage by id. On failure the
// message stays in the chat — acceptable since the next turn's
// cold-create creates a fresh DraftMessage rather than reusing
// the orphan id.
//
// deleteMessage errors are logged not returned — failure to
// delete is a UX nit (orphan in chat) but not a correctness
// issue.
func (m *groupDraftManager) deleteOrphan(ctx context.Context, rawChatID string, topicID int, msgID int) {
	if msgID == 0 {
		return
	}
	if err := m.api.call(ctx, "deleteMessage", map[string]any{
		"chat_id":    rawChatID,
		"message_id": msgID,
	}, nil); err != nil {
		m.log.Warn("telegram: group DraftMessage delete failed",
			"chat_id", rawChatID,
			"thread_id", topicID,
			"message_id", msgID,
			"err", err,
		)
	}
}
