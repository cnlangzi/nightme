package telegram

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
)

// Single FIFO buffer for the simulated DraftMessage in ChatKind=group.
//
// Capacity: 50 events. On overflow, the OLDEST event is evicted
// (FIFO). 50 is large enough to swallow a long-running agent turn
// without losing any meaningful history, while staying well under
// any reasonable in-memory ceiling.
//
// Send window: 5 events per flush. When a flush fires, the LATEST
// 5 events in the buffer are popped and sent to Telegram as
// `{"blocks":[…]}`; the rest stay buffered for the next flush.
// 5 blocks × ~1KB each = ~5KB, well under Telegram's 4096-char
// text limit and the rich_message block cap.
//
// Flush triggers:
//   - 10s timer after the first buffered event (whichever comes
//     first per-window).
//   - endProcess / OnPromptEnded (final flush + deleteMessage).
//
// No count-based trigger — we don't want to drop buffered events
// just because N reached some threshold; the 50-cap + 5-per-send
// design already keeps each flush bounded.
const (
	groupDraftStackCap    = 50
	groupDraftSendPerFlush = 5

	// 10s timer fallback when events keep trickling in below
	// the natural 5-per-send cadence. Without this a turn that
	// emits 1–2 events per second would never flush.
	groupDraftBatchInterval = 10 * time.Second
)

// groupDraftEntry is the per-turn in-memory state for the simulated
// DraftMessage surface used by ChatKind=group. A single FIFO buffer
// of richTurnEntry holds all events since the last flush; the
// buffer is bounded at 50 (FIFO eviction) and Telegram only ever
// sees the latest 5 events per flush.
//
// Two locks:
//   - mu protects the buffer (entries). Held briefly during
//     append / peek / pop. Released before the API call.
//   - flushMu serializes concurrent flushes (the API call + the
//     post-call buffer mutation). Held during the API call only.
//
// Holding mu through the network roundtrip was the source of the
// "agent stuck on streamDraftEvent" hang (issue observed at
// 2026-09-15 21:00): a slow Telegram API would block every
// subsequent streamDraftEvent call. The fix: release mu before
// the API call; serialize the API call + buffer mutation under
// flushMu so the buffer is still consistent.
//
// messageID is the Telegram message_id returned by the first
// (cold-create) flush. 0 means "not yet sent". In-memory only —
// no persisted DraftMessageID.
type groupDraftEntry struct {
	mu      sync.Mutex
	flushMu sync.Mutex

	// Persistent identity — set by the first flush.
	messageID int

	// FIFO buffer. cap 50 (FIFO eviction at overflow). Cleared
	// at the latest 5 events on every successful flush.
	entries []richTurnEntry

	// 10s timer fallback.
	flushTimer *time.Timer

	// Captured context for timer callbacks.
	rawChatID string
	topicID   int
	userMsgID int
}

// groupDraftManager owns the per-(chat, topic, userMsgID) entries
// for the "group" ChatKind's simulated DraftMessage surface.
type groupDraftManager struct {
	api apiClient
	log *slog.Logger
	mu  sync.Mutex
	// entries is keyed by chatID|topicID|userMsgID. Map-level
	// reads/writes are guarded by mu; per-entry mutation is
	// guarded by entry.mu (buffer) and entry.flushMu (API call).
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

// streamDraftEvent appends one event to the FIFO buffer (with
// FIFO eviction at cap 50) and arms the 10s flush timer if no
// timer is already pending. The actual Telegram API call happens
// in flushLocked when the timer fires (or endProcess).
//
// The boolean mirrors streamDraftEvent's DM-side contract — true
// means "consumed, do not fall through to the richTurn path".
func (m *groupDraftManager) streamDraftEvent(_ context.Context, rawChatID string, topicID int, userMsgID int, segment string, kind messages.OutboundKind) (bool, error) {
	if userMsgID <= 0 {
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
	entry.entries = append(entry.entries, richTurnEntry{
		kind: kindToRichBlockKind(kind),
		body: segment,
	})
	// FIFO eviction at cap: drop oldest until len == cap.
	if len(entry.entries) > groupDraftStackCap {
		entry.entries = entry.entries[len(entry.entries)-groupDraftStackCap:]
	}
	stackLen := len(entry.entries)
	entry.mu.Unlock()

	m.log.Info("telegram: streamDraftEvent buffered",
		"chat_id", rawChatID,
		"thread_id", topicID,
		"user_msg_id", userMsgID,
		"kind", kind.String(),
		"stack_len", stackLen,
		"message_id", entry.messageID,
	)

	m.startFlushTimerIfIdleLocked(entry)
	return true, nil
}

// kindToRichBlockKind maps the OutboundKind to richTurnEntry.kind
// for the rich_message block renderer.
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

// flushLocked sends the LATEST 5 entries (or all of them if fewer
// than 5) as a rich message via sendRichMessage (first flush) or
// editMessageText (subsequent flushes). buffer is mutated to remove
// the sent entries. On API failure the sent entries are restored
// to the buffer (issue #391 contract).
//
// Lock discipline:
//   1. Acquire mu, peek + pop the sent slice, release mu.
//   2. Acquire flushMu (serializes concurrent flushes).
//   3. Call API.
//   4. On failure, re-acquire mu and prepend the popped slice back.
//   5. Release flushMu.
//
// Step 1+2 ensures the buffer mutation happens under mu, the API
// call happens under flushMu (without mu), so a slow API call
// never blocks new streamDraftEvent calls.
func (m *groupDraftManager) flushLocked(ctx context.Context, entry *groupDraftEntry, rawChatID string, topicID int, reason string) (bool, error) {
	// 1. Pop the latest 5 entries under mu.
	entry.mu.Lock()
	if len(entry.entries) == 0 {
		entry.mu.Unlock()
		return true, nil
	}
	n := groupDraftSendPerFlush
	if n > len(entry.entries) {
		n = len(entry.entries)
	}
	sending := make([]richTurnEntry, n)
	copy(sending, entry.entries[len(entry.entries)-n:])
	// Remove the sent slice from the buffer.
	entry.entries = entry.entries[:len(entry.entries)-n]
	remainingAfterPop := len(entry.entries)
	entry.mu.Unlock()

	// 2. Serialize the API call.
	entry.flushMu.Lock()
	defer entry.flushMu.Unlock()

	blocks := make([]map[string]any, 0, len(sending))
	for _, e := range sending {
		blocks = append(blocks, map[string]any{"type": "paragraph", "text": e.body})
	}
	blocksJSON, err := json.Marshal(map[string]any{"blocks": blocks})
	if err != nil {
		// On marshal failure, restore the slice and bail.
		entry.mu.Lock()
		entry.entries = append(sending, entry.entries...)
		entry.mu.Unlock()
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
		method = "editMessageText"
		params["message_id"] = entry.messageID
		err = m.api.call(ctx, "editMessageText", params, nil)
	}

	if err != nil {
		m.log.Warn("telegram: group DraftMessage flush failed; buffer restored",
			"chat_id", rawChatID,
			"thread_id", topicID,
			"method", method,
			"message_id", entry.messageID,
			"sent_blocks", len(sending),
			"remaining_after_pop", remainingAfterPop,
			"reason", reason,
			"err", err,
		)
		// Restore the popped slice back to the buffer.
		entry.mu.Lock()
		entry.entries = append(sending, entry.entries...)
		entry.mu.Unlock()
		return true, nil
	}

	m.log.Info("telegram: group DraftMessage flushed",
		"chat_id", rawChatID,
		"thread_id", topicID,
		"method", method,
		"message_id", entry.messageID,
		"sent_blocks", len(sending),
		"remaining_after_pop", remainingAfterPop,
		"reason", reason,
	)
	if remainingAfterPop == 0 && entry.flushTimer != nil {
		// Buffer empty after this flush; no point keeping a timer alive.
		entry.flushTimer.Stop()
		entry.flushTimer = nil
	}
	return true, nil
}

// startFlushTimerIfIdleLocked arms the 10s debounce timer if no
// timer is already pending. Caller MUST NOT hold entry.mu; this
// function acquires it.
func (m *groupDraftManager) startFlushTimerIfIdleLocked(entry *groupDraftEntry) {
	entry.mu.Lock()
	if entry.flushTimer != nil {
		entry.mu.Unlock()
		return
	}
	entry.flushTimer = time.AfterFunc(groupDraftBatchInterval, func() {
		// Timer callback runs in its own goroutine; acquire mu to
		// check the buffer state.
		entry.mu.Lock()
		if len(entry.entries) == 0 {
			entry.flushTimer = nil
			entry.mu.Unlock()
			return
		}
		entry.mu.Unlock()
		// No mu held during the API call.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = m.flushLocked(ctx, entry, entry.rawChatID, entry.topicID, "timer")
	})
	entry.mu.Unlock()
}

// endProcess finalizes the simulated DraftMessage for a turn:
// flushes any remaining buffered events, deletes the underlying
// Telegram message, and drops the in-memory entry.
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
		return
	}
	entry.mu.Lock()
	if entry.flushTimer != nil {
		entry.flushTimer.Stop()
		entry.flushTimer = nil
	}
	hasBuffer := len(entry.entries) > 0
	msgID := entry.messageID
	entry.mu.Unlock()

	if hasBuffer {
		// Final flush. Will still call API even if no more events
		// arrive — buffer may have leftovers.
		_, _ = m.flushLocked(ctx, entry, rawChatID, topicID, "endProcess")
		// After the final flush, look up the actual messageID
		// (sendRichMessage may have just set it).
		entry.mu.Lock()
		msgID = entry.messageID
		entry.mu.Unlock()
	}
	if msgID > 0 {
		m.deleteOrphan(ctx, rawChatID, topicID, msgID)
	}
}

// deleteOrphan removes a DraftMessage by id. On failure the
// message stays in the chat — acceptable since the next turn
// creates a fresh DraftMessage.
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