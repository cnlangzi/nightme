package telegram

import (
	"context"
	"encoding/json"
	"fmt"
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
// Telegram's 1/s per-chat and 20/min per-group limits. Cold-create
// failures retry once after 5s; the second failure falls the rest
// of the turn through to richTurn.
const (
	groupDraftBatchMaxEvents = 10
	groupDraftBatchInterval  = 10 * time.Second
	coldCreateRetryDelay     = 5 * time.Second
	coldCreateMaxAttempts    = 2 // initial + 1 retry
)

// groupDraftEntry is the per-turn in-memory state for the simulated
// DraftMessage surface used by ChatKind=group. The buffer is a
// windowed slice of richTurnEntry (one paragraph block per entry);
// flushes happen at 10 events or 10s after the first buffered event,
// whichever comes first. After a successful flush the buffer is
// cleared (windowed semantics): Telegram keeps showing the last
// flushed snapshot until the next window replaces it.
//
// messageID is the Telegram message_id returned by the cold-create
// sendRichMessage; 0 means "not yet sent". state.DraftMessageID still
// mirrors messageID for the orphans-recovery path used by
// ensurePlaceholder; the next commit drops that persistence as a
// breaking change.
//
// All mutable fields are guarded by mu. The lock is held across the
// network roundtrip on streamDraftEvent / coldCreate / flushLocked
// so concurrent events on the same turn compose + send in serial
// order. timer callbacks also acquire mu before touching state.
//
// Per-prompt isolation (see docs §11.12.11.3) is enforced by the
// groupDraftKey already containing userMsgID: ensureEntry creates a
// fresh entry per turn, so cross-turn entry reuse cannot happen via
// the production keying.
type groupDraftEntry struct {
	mu sync.Mutex

	// Persistent identity — set by cold-create, survives the
	// buffer-flush lifecycle.
	messageID int

	// Buffer for the current window. Cleared after each successful
	// flush (windowed semantics — Telegram shows the last flushed
	// snapshot, the buffer holds the next).
	entries []richTurnEntry

	// Batch bookkeeping.
	pendingEventCount int  // logical events since last flush (OutToolStart+End = 1)
	pendingToolStart  bool // true while an OutToolStart is awaiting its End
	flushTimer        *time.Timer

	// Cold-create retry state. Set after a failed sendRichMessage;
	// cleared on the next successful cold-create.
	coldCreateRetries    int
	coldCreateRetryTimer *time.Timer
	hasGivenUpColdCreate bool

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
//	OutThinking / OutToolStart / OutToolEnd (first of turn)
//	    → ensureEntry + compose (window 0)
//	    → coldCreate sendRichMessage + persist message_id
//	subsequent events
//	    → compose into window buffer; pendingEventCount++
//	    → trigger flush at 10 events or 10s timer
//	OutResult / OnPromptEnded
//	    → flush remaining buffered events
//	    → deleteMessage + clear state + drop entry
//
// Failure semantics:
//
//   - editMessageText failure → buffer preserved, return handled=true
//     (issue #391 — fall through to richTurn was leaking tool lines)
//   - cold-create failure → schedule retry after 5s; events during
//     the wait fall through to richTurn. After maxAttempts (2 total)
//     the rest of the turn gives up and falls through too.
type groupDraftManager struct {
	api   apiClient
	log   *slog.Logger
	store *stateStore
	mu    sync.Mutex
	// entries is keyed by chatID|topicID|userMsgID. Map-level
	// reads/writes are guarded by mu; per-entry mutation is
	// guarded by entry.mu (taken by streamDraftEvent / coldCreate /
	// flushLocked / endProcess for the whole operation, by timer
	// callbacks for retry / flush).
	entries map[string]*groupDraftEntry
}

func newGroupDraftManager(api apiClient, log *slog.Logger, store *stateStore) *groupDraftManager {
	return &groupDraftManager{
		api:     api,
		log:     log,
		store:   store,
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
// composes the in-memory buffer (windowed) and either cold-creates
// or flushes via sendRichMessage / editMessageText.
//
// The boolean mirrors streamDraftEvent's DM-side contract — true
// means "consumed, do not fall through to the richTurn path".
//
// userMsgID is BOTH the entry key (one entry per turn) and the
// reply_to_message_id anchor on the cold-create sendRichMessage —
// the DraftMessage visually hangs off the user's message like
// every other turn message. Per-prompt isolation is enforced by the
// groupDraftKey already containing userMsgID; the caller is
// responsible for passing the per-event anchor (msg.ReplyTo), see
// docs/channel/telegram.md §11.12.11.3.
func (m *groupDraftManager) streamDraftEvent(ctx context.Context, rawChatID string, topicID int, userMsgID int, segment string, kind messages.OutboundKind) (bool, error) {
	if userMsgID <= 0 {
		// No user message anchor → can't safely cold-create (a
		// floating message would have nothing to chain under).
		// Bail to caller fallthrough (richTurn path renders it).
		return false, nil
	}
	m.mu.Lock()
	entry := m.ensureEntry(rawChatID, topicID, userMsgID)
	m.mu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()

	if entry.hasGivenUpColdCreate {
		// Already used our cold-create attempts this turn.
		return false, nil
	}
	if entry.coldCreateRetryTimer != nil {
		// Waiting for the cold-create retry timer. Fall this event
		// through to richTurn; the rich surface accumulates it.
		return false, nil
	}

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

	if entry.messageID == 0 {
		return m.coldCreate(ctx, entry, rawChatID, topicID, userMsgID)
	}

	m.startFlushTimerIfIdleLocked(entry)
	if entry.pendingEventCount >= groupDraftBatchMaxEvents {
		return m.flushLocked(ctx, entry, rawChatID, topicID)
	}
	return true, nil
}

// coldCreate sends the first event as a rich message via
// sendRichMessage. On failure it schedules a delayed retry; once
// the retry budget is exhausted the rest of the turn gives up and
// subsequent streamDraftEvent calls return handled=false (events
// flow through to richTurn). Caller MUST hold entry.mu.
func (m *groupDraftManager) coldCreate(ctx context.Context, entry *groupDraftEntry, rawChatID string, topicID int, userMsgID int) (bool, error) {
	blocksJSON, err := buildParagraphBlocksForEntries(entry.entries)
	if err != nil {
		return false, err
	}

	params := map[string]any{
		"chat_id":      rawChatID,
		"rich_message": json.RawMessage(blocksJSON),
	}
	if topicID > 0 {
		params["message_thread_id"] = topicID
	}
	if userMsgID > 0 {
		params["reply_to_message_id"] = userMsgID
	}

	var result SendMessageResult
	if err := m.api.call(ctx, "sendRichMessage", params, &result); err != nil {
		entry.coldCreateRetries++
		if entry.coldCreateRetries >= coldCreateMaxAttempts {
			entry.hasGivenUpColdCreate = true
			m.log.Warn("telegram: group DraftMessage cold-create gave up; rest of turn falls through to richTurn",
				"chat_id", rawChatID,
				"thread_id", topicID,
				"user_msg_id", userMsgID,
				"attempts", entry.coldCreateRetries,
				"err", err,
			)
		} else {
			entry.coldCreateRetryTimer = time.AfterFunc(coldCreateRetryDelay, func() {
				entry.mu.Lock()
				entry.coldCreateRetryTimer = nil
				entry.mu.Unlock()
			})
			m.log.Warn("telegram: group DraftMessage cold-create failed; retrying after delay",
				"chat_id", rawChatID,
				"thread_id", topicID,
				"user_msg_id", userMsgID,
				"attempt", entry.coldCreateRetries,
				"err", err,
			)
		}
		return false, err
	}
	if result.MessageID == 0 {
		entry.coldCreateRetries++
		entry.hasGivenUpColdCreate = entry.coldCreateRetries >= coldCreateMaxAttempts
		return false, fmt.Errorf("telegram: sendRichMessage returned empty message_id")
	}
	entry.messageID = result.MessageID
	// Persist so a daemon restart mid-turn can resume editing the
	// same message. Removed in the next commit (breaking).
	if m.store != nil {
		if state, ok := m.store.topic(rawChatID, topicID); ok && state != nil {
			state.DraftMessageID = result.MessageID
			if err := m.store.putTopic(state); err != nil && m.log != nil {
				m.log.Warn("telegram: failed to persist DraftMessageID",
					"chat_id", rawChatID,
					"thread_id", topicID,
					"err", err,
				)
			}
		}
	}
	// Window 0 flushed: clear the buffer so the next event starts a
	// fresh window (windowed semantics — the buffer is "next flush",
	// not "all of history").
	entry.entries = entry.entries[:0]
	entry.pendingEventCount = 0
	entry.pendingToolStart = false
	return true, nil
}

// flushLocked serializes the current entries buffer as a rich
// message and PATCHes the Telegram message via editMessageText. On
// success the buffer is cleared (windowed semantics); on failure
// the buffer is preserved for the next streamDraftEvent to retry
// on top of (issue #391 — fall through would leak tool result
// lines into the rich turn body). Caller MUST hold entry.mu.
func (m *groupDraftManager) flushLocked(ctx context.Context, entry *groupDraftEntry, rawChatID string, topicID int) (bool, error) {
	if entry.messageID == 0 || len(entry.entries) == 0 {
		return true, nil
	}

	blocksJSON, err := buildParagraphBlocksForEntries(entry.entries)
	if err != nil {
		return true, err
	}

	params := map[string]any{
		"chat_id":      rawChatID,
		"message_id":   entry.messageID,
		"rich_message": json.RawMessage(blocksJSON),
	}
	if topicID > 0 {
		params["message_thread_id"] = topicID
	}

	if err := m.api.call(ctx, "editMessageText", params, nil); err != nil {
		// editMessageText failed: stay on the draft path. The entry
		// (and messageID) stay intact so the next event retries on
		// top of the prior buffer. Returning false here would let
		// the caller fall through to the rich turn path and leak
		// tool result lines (`⎿ 🔧 tool → N bytes`) into the final
		// answer message — see issue #391.
		m.log.Warn("telegram: group DraftMessage edit failed",
			"chat_id", rawChatID,
			"thread_id", topicID,
			"message_id", entry.messageID,
			"err", err,
		)
		return true, nil
	}
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
		if entry.pendingEventCount == 0 || entry.messageID == 0 {
			entry.flushTimer = nil
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// timer fires asynchronously; flushLocked logs + preserves
		// the buffer on failure, so we don't propagate the error.
		_, _ = m.flushLocked(ctx, entry, entry.rawChatID, entry.topicID)
	})
}

// endProcess finalizes the simulated DraftMessage for a turn:
// flushes any buffered events (so the last window isn't lost),
// deletes the underlying Telegram message, clears the persisted
// state, and drops the in-memory entry. Mirrors the DM draft's
// auto-disappear-on-real-message behavior (#383).
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
		// No in-memory entry — try the persisted state in case the
		// daemon restarted mid-turn (orphan recovery). Removed in
		// the next commit when state.DraftMessageID goes away.
		if m.store != nil {
			if state, ok := m.store.topic(rawChatID, topicID); ok && state != nil && state.DraftMessageID > 0 {
				m.deleteOrphan(ctx, rawChatID, topicID, state.DraftMessageID)
			}
		}
		return
	}
	entry.mu.Lock()
	// Stop both timers before flush so a stale timer doesn't fire
	// after endProcess has deleted the message.
	if entry.flushTimer != nil {
		entry.flushTimer.Stop()
		entry.flushTimer = nil
	}
	if entry.coldCreateRetryTimer != nil {
		entry.coldCreateRetryTimer.Stop()
		entry.coldCreateRetryTimer = nil
	}
	// Flush any buffered events first so the user sees the final
	// window before we delete the message. Failure is non-fatal —
	// the buffer is about to be discarded with the entry.
	if entry.messageID > 0 && entry.pendingEventCount > 0 {
		_, _ = m.flushLocked(ctx, entry, rawChatID, topicID)
	}
	msgID := entry.messageID
	entry.mu.Unlock()

	if msgID > 0 {
		m.deleteOrphan(ctx, rawChatID, topicID, msgID)
	}
}

// deleteOrphan removes a DraftMessage by id and clears the
// persisted DraftMessageID on success. On failure the persisted
// state is left intact so the next ensurePlaceholder (for the
// next turn) sees it and re-attempts the delete. Called from
// endProcess (turn-end cleanup) and ensurePlaceholder (orphan
// recovery for the previous turn's crash or deleteMessage-failed
// message).
//
// deleteMessage errors are logged not returned — failure to delete
// is a UX nit (orphan in chat) but not a correctness issue; the
// next ensurePlaceholder will retry.
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
		// Leave state.DraftMessageID set so the next
		// ensurePlaceholder re-attempts. Returning early avoids
		// losing the orphan reference.
		return
	}
	if m.store != nil {
		if state, ok := m.store.topic(rawChatID, topicID); ok && state != nil && state.DraftMessageID == msgID {
			state.DraftMessageID = 0
			if err := m.store.putTopic(state); err != nil && m.log != nil {
				m.log.Warn("telegram: failed to clear DraftMessageID",
					"chat_id", rawChatID,
					"thread_id", topicID,
					"err", err,
				)
			}
		}
	}
}

// deleteOrphanSync is the synchronous variant used by
// ensurePlaceholder for the previous turn's leftover. It blocks
// until the API call completes (success or failure) so the new
// turn's cold-create sees a clean chat. Best-effort: on failure
// the orphan persists.
func (m *groupDraftManager) deleteOrphanSync(ctx context.Context, rawChatID string, topicID int, msgID int) {
	if msgID <= 0 || m == nil {
		return
	}
	m.deleteOrphan(ctx, rawChatID, topicID, msgID)
}
