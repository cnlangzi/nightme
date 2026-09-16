package telegram

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
)

// Two FIFO stacks for the simulated DraftMessage surface used
// by every non-channel ChatKind (DM, basic group, forum
// supergroup), each capped at 50:
//
//   - thinkingStack []richTurnEntry  cap 50: holds OutThinking
//     events in arrival order. On overflow, oldest is FIFO-evicted.
//   - toolsStack    []toolSlot       cap 50: each slot is "1
//     OutToolStart + its trailing N OutToolEnd events". New
//     OutToolStart opens a new slot; OutToolEnd appends to the
//     LAST slot's ends. On overflow, oldest slot is FIFO-evicted.
//
// Send window per flush: latest 2 from thinkingStack, latest 5
// from toolsStack (or fewer if the stack has fewer). Each flush
// sends a single rich_message with thinking blocks first, then
// tool blocks, all under Telegram's per-message length cap.
// 2+5 = 7 blocks × ~1KB each ≈ 7KB, well under the limits.
//
// Flush triggers: 10s timer fallback, endProcess / OnPromptEnded.
// No count-based trigger — with cap 50 + send 2/5, dropping
// buffered events on a count threshold would defeat the purpose
// of keeping recent context.
const (
	thinkingStackCap       = 50
	toolsStackCap          = 50
	thinkingStackFlushMax  = 2 // thinking: per-flush send window
	toolsStackFlushMax     = 5 // tools:    per-flush send window
	liveDraftBatchInterval = 10 * time.Second
)

// toolSlot is one entry in toolsStack: a single OutToolStart
// followed by its trailing OutToolEnd events. The slot is created
// when the Start arrives; a subsequent Start implicitly closes
// the prior slot (it can no longer receive Ends) and opens a new
// one. An OutToolEnd with no open slot becomes an orphan slot
// (start empty, ends holds the lone End) — the body shows up as
// a Start line, which the user can see but is semantically odd.
type toolSlot struct {
	start richTurnEntry
	ends  []richTurnEntry
}

// liveDraftEntry is the per-turn in-memory state for the simulated
// DraftMessage surface used by every non-channel ChatKind (DM,
// basic group, forum supergroup). Two FIFO stacks, each capped at
// 50, hold all events since the last flush; the buffer is bounded
// and Telegram only ever sees the latest 5 from each stack per
// flush.
//
// Lock discipline:
//
//   - mu guards: stacks (append / peek / pop), flushTimer
//     (nil / non-nil), messageID (cold-create result).
//   - flushMu serializes concurrent wire calls (timer callback
//     vs endProcess / next event). Held only during the wire
//     roundtrip.
//
// The network roundtrip happens with NEITHER lock held (after mu
// is released for the pop, before flushMu is acquired — and after
// flushMu is released, before mu is re-acquired for restore /
// messageID / timer). Holding mu across the roundtrip was the
// source of the "agent stuck on streamDraftEvent" hang (issue
// observed at 2026-09-15 21:00): a slow Telegram API would block
// every subsequent streamDraftEvent call. The fix: release mu
// before the API call; serialize the API call under flushMu so the
// buffer is still consistent.
//
// messageID is the Telegram message_id returned by the first
// (cold-create) flush. 0 means "not yet sent". In-memory only —
// there is no persisted DraftMessageID in TopicState (orphan
// recovery was retired with the unified path; see §11.12.1 docs).
type liveDraftEntry struct {
	mu      sync.Mutex
	flushMu sync.Mutex

	// Persistent identity — set by the first flush.
	messageID int

	// Two parallel FIFO stacks, each capped at 50.
	thinkingStack []richTurnEntry // cap 50
	toolsStack    []toolSlot      // cap 50 slots

	// 10s timer fallback. Nil when no timer is pending. After
	// every flush we nil this out so the next streamDraftEvent can
	// re-arm; an unstopped, fired timer would otherwise block all
	// subsequent events until endProcess.
	flushTimer *time.Timer

	// Captured context for timer callbacks.
	rawChatID string
	topicID   int
	userMsgID int
}

// liveDraftManager owns the per-(chat, topic, userMsgID) entries
// for the simulated DraftMessage surface used by every
// non-channel ChatKind (DM + group).
type liveDraftManager struct {
	api apiClient
	log *slog.Logger
	mu  sync.Mutex
	// entries is keyed by chatID|topicID|userMsgID. Map-level
	// reads/writes are guarded by mu; per-entry mutation is
	// guarded by entry.mu (buffer) and entry.flushMu (API call).
	entries map[string]*liveDraftEntry
}

func newLiveDraftManager(api apiClient, log *slog.Logger) *liveDraftManager {
	return &liveDraftManager{
		api:     api,
		log:     log,
		entries: make(map[string]*liveDraftEntry),
	}
}

func liveDraftKey(chatID string, topicID int, userMsgID int) string {
	return chatID + "|" + stringInt(topicID) + "|" + stringInt(userMsgID)
}

// ensureEntry returns the entry for a turn, allocating one on first
// access. Caller holds the manager mutex.
func (m *liveDraftManager) ensureEntry(chatID string, topicID int, userMsgID int) *liveDraftEntry {
	key := liveDraftKey(chatID, topicID, userMsgID)
	if e, ok := m.entries[key]; ok {
		return e
	}
	e := &liveDraftEntry{
		rawChatID: chatID,
		topicID:   topicID,
		userMsgID: userMsgID,
	}
	m.entries[key] = e
	return e
}

// streamDraftEvent appends one event to the appropriate stack
// (with FIFO eviction at cap 50) and arms the 10s flush timer if
// no timer is already pending. The actual Telegram API call
// happens in flush when the timer fires (or endProcess).
//
// Returns (handled=true) on success — caller treats it as consumed
// and does NOT fall through to the richTurn path. Returns
// (handled=false) on userMsgID <= 0 or unsupported kind so the
// caller can decide whether to fall through.
func (m *liveDraftManager) streamDraftEvent(_ context.Context, rawChatID string, topicID int, userMsgID int, segment string, kind messages.OutboundKind) (bool, error) {
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
	switch kind {
	case messages.OutThinking:
		entry.thinkingStack = append(entry.thinkingStack, richTurnEntry{
			kind: "thinking",
			body: segment,
		})
		// FIFO evict at cap.
		if len(entry.thinkingStack) > thinkingStackCap {
			entry.thinkingStack = entry.thinkingStack[len(entry.thinkingStack)-thinkingStackCap:]
		}
	case messages.OutToolStart:
		entry.toolsStack = append(entry.toolsStack, toolSlot{
			start: richTurnEntry{kind: "tool", body: segment},
		})
		if len(entry.toolsStack) > toolsStackCap {
			entry.toolsStack = entry.toolsStack[len(entry.toolsStack)-toolsStackCap:]
		}
	case messages.OutToolEnd:
		if len(entry.toolsStack) == 0 {
			// Orphan End: treat as the implicit Start of a new slot.
			entry.toolsStack = append(entry.toolsStack, toolSlot{
				start: richTurnEntry{kind: "tool", body: segment},
			})
		} else {
			slot := &entry.toolsStack[len(entry.toolsStack)-1]
			slot.ends = append(slot.ends, richTurnEntry{kind: "tool", body: segment})
		}
		// toolsStack length may have grown by 1 above; cap-check.
		if len(entry.toolsStack) > toolsStackCap {
			entry.toolsStack = entry.toolsStack[len(entry.toolsStack)-toolsStackCap:]
		}
	}
	thinkLen := len(entry.thinkingStack)
	toolLen := len(entry.toolsStack)
	msgID := entry.messageID
	entry.mu.Unlock()

	m.log.Info("telegram: streamDraftEvent buffered",
		"chat_id", rawChatID,
		"thread_id", topicID,
		"user_msg_id", userMsgID,
		"kind", kind.String(),
		"thinking_stack_len", thinkLen,
		"tools_stack_len", toolLen,
		"message_id", msgID,
	)

	m.startFlushTimerIfIdleLocked(entry)
	return true, nil
}

// flush is the lock-acquiring wrapper around flushInner for the
// timer-callback path. Captures the LATEST N events from each
// stack (thinkingStackFlushMax from thinking, toolsStackFlushMax
// from tools, or all if fewer) as a single rich_message via
// sendRichMessage (first flush) or editMessageText (subsequent
// flushes). Popped entries are removed from the stacks; on API
// failure they are restored.
//
// Lock discipline (per entry):
//  1. mu — peek + pop the to-send slice from each stack, release.
//  2. flushMu — serialize the API call across concurrent flushes
//     (timer callback vs endProcess vs new event arming).
//  3. On failure, mu — restore the popped slices.
//  4. mu — clear flushTimer (always; lets next event re-arm)
//     and update messageID (on cold-create success).
//
// Step 1+2 ensures the buffer mutation happens under mu, the API
// call happens under flushMu (without mu), so a slow API call
// never blocks new streamDraftEvent calls.
func (m *liveDraftManager) flush(ctx context.Context, entry *liveDraftEntry, rawChatID string, topicID int, reason string) (bool, error) {
	entry.flushMu.Lock()
	defer entry.flushMu.Unlock()
	return m.flushInner(ctx, entry, rawChatID, topicID, reason)
}

// flushInner is the flush implementation. The caller MUST hold
// entry.flushMu for the duration of the call (so concurrent flushes
// serialize). See flush for the full lock discipline.
func (m *liveDraftManager) flushInner(ctx context.Context, entry *liveDraftEntry, rawChatID string, topicID int, reason string) (bool, error) {
	// 1. Pop the latest 5 from each stack under mu.
	entry.mu.Lock()
	if len(entry.thinkingStack) == 0 && len(entry.toolsStack) == 0 {
		entry.mu.Unlock()
		return true, nil
	}

	nThink := thinkingStackFlushMax
	if nThink > len(entry.thinkingStack) {
		nThink = len(entry.thinkingStack)
	}
	sendingThink := make([]richTurnEntry, nThink)
	copy(sendingThink, entry.thinkingStack[len(entry.thinkingStack)-nThink:])
	entry.thinkingStack = entry.thinkingStack[:len(entry.thinkingStack)-nThink]

	nTools := toolsStackFlushMax
	if nTools > len(entry.toolsStack) {
		nTools = len(entry.toolsStack)
	}
	sendingTools := make([]toolSlot, nTools)
	for i, slot := range entry.toolsStack[len(entry.toolsStack)-nTools:] {
		sendingTools[i] = slot
	}
	entry.toolsStack = entry.toolsStack[:len(entry.toolsStack)-nTools]

	remainingThink := len(entry.thinkingStack)
	remainingTools := len(entry.toolsStack)
	msgIDBefore := entry.messageID
	entry.mu.Unlock()

	// 2. Marshal + wire call (flushMu is held by the caller).
	blocks := buildBlocksFromStacks(sendingThink, sendingTools)
	blocksJSON, err := json.Marshal(map[string]any{"blocks": blocks})
	if err != nil {
		// Marshal failure: restore the slices and bail. The
		// popped slices hold the NEWEST entries; append them to
		// the end of the remaining buffer to restore original
		// order.
		entry.mu.Lock()
		entry.thinkingStack = append(entry.thinkingStack, sendingThink...)
		entry.toolsStack = append(entry.toolsStack, sendingTools...)
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
	var msgIDAfter int
	if msgIDBefore == 0 {
		method = "sendRichMessage"
		if entry.userMsgID > 0 {
			params["reply_to_message_id"] = entry.userMsgID
		}
		var result SendMessageResult
		err = m.api.call(ctx, "sendRichMessage", params, &result)
		if err == nil && result.MessageID > 0 {
			// Update messageID under mu — multiple readers
			// (streamDraftEvent's log, endProcess's delete) race
			// here.
			entry.mu.Lock()
			entry.messageID = result.MessageID
			msgIDAfter = result.MessageID
			entry.mu.Unlock()
		} else if err == nil {
			err = &apiError{Message: "telegram: sendRichMessage returned empty message_id"}
		}
	} else {
		method = "editMessageText"
		params["message_id"] = msgIDBefore
		err = m.api.call(ctx, "editMessageText", params, nil)
		msgIDAfter = msgIDBefore
	}

	if err != nil {
		m.log.Warn("telegram: liveDraft flush failed; buffer restored",
			"chat_id", rawChatID,
			"thread_id", topicID,
			"method", method,
			"message_id", msgIDBefore,
			"sent_thinking", len(sendingThink),
			"sent_tools", len(sendingTools),
			"remaining_thinking", remainingThink,
			"remaining_tools", remainingTools,
			"reason", reason,
			"err", err,
		)
		// Restore the popped slices back to the end of the
		// buffer (newest at the tail).
		entry.mu.Lock()
		entry.thinkingStack = append(entry.thinkingStack, sendingThink...)
		entry.toolsStack = append(entry.toolsStack, sendingTools...)
		entry.mu.Unlock()
		return true, nil
	}

	m.log.Info("telegram: liveDraft flushed",
		"chat_id", rawChatID,
		"thread_id", topicID,
		"method", method,
		"message_id", msgIDAfter,
		"sent_thinking", len(sendingThink),
		"sent_tools", len(sendingTools),
		"remaining_thinking", remainingThink,
		"remaining_tools", remainingTools,
		"reason", reason,
	)

	// Clear the timer (always — even if buffers are non-empty —
	// so the next streamDraftEvent can arm a fresh one). The
	// timer's callback (if it raced ahead) will see an empty
	// buffer and bail; that's safe.
	entry.mu.Lock()
	if entry.flushTimer != nil {
		entry.flushTimer.Stop()
		entry.flushTimer = nil
	}
	entry.mu.Unlock()
	return true, nil
}

// startFlushTimerIfIdleLocked arms the 10s debounce timer if no
// timer is already pending. Caller MUST NOT hold entry.mu.
func (m *liveDraftManager) startFlushTimerIfIdleLocked(entry *liveDraftEntry) {
	entry.mu.Lock()
	if entry.flushTimer != nil {
		entry.mu.Unlock()
		return
	}
	entry.flushTimer = time.AfterFunc(liveDraftBatchInterval, func() {
		// AfterFunc may run even if Stop() was called between the
		// firing and the callback dispatch — Go's contract allows
		// the callback to run. Be defensive: check buffer state,
		// bail if there's nothing to do.
		entry.mu.Lock()
		empty := len(entry.thinkingStack) == 0 && len(entry.toolsStack) == 0
		// If flushInner already nil'd the timer under mu, this
		// callback is racing with the Stop. Skip.
		timerStopped := entry.flushTimer == nil
		entry.mu.Unlock()
		if empty || timerStopped {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = m.flush(ctx, entry, entry.rawChatID, entry.topicID, "timer")
	})
	entry.mu.Unlock()
}

// endProcess finalizes the simulated DraftMessage for a turn:
// stops the 10s timer (if any), flushes any remaining buffered
// events as a final flush, and deletes the underlying Telegram
// message. The entry is removed from the manager's map first so
// new events for the same userMsgID cannot race in.
//
// flushMu is acquired and held across stop-timer + flush +
// read-messageID + delete so an in-flight timer callback cannot
// cold-create a fresh message between our messageID read and our
// deleteMessage call (which would leave an orphan in chat).
func (m *liveDraftManager) endProcess(ctx context.Context, rawChatID string, topicID int, userMsgID int) {
	if userMsgID <= 0 {
		return
	}
	m.mu.Lock()
	entry, ok := m.entries[liveDraftKey(rawChatID, topicID, userMsgID)]
	if ok {
		delete(m.entries, liveDraftKey(rawChatID, topicID, userMsgID))
	}
	m.mu.Unlock()
	if !ok {
		return
	}

	// Serialize with any in-flight flush (timer callback, new
	// event). Hold flushMu across the entire finalization.
	entry.flushMu.Lock()
	defer entry.flushMu.Unlock()

	// Stop the timer. Any callback blocked on flushMu will see
	// empty buffers / nil timer when we release and bail.
	entry.mu.Lock()
	if entry.flushTimer != nil {
		entry.flushTimer.Stop()
		entry.flushTimer = nil
	}
	hasBuffer := len(entry.thinkingStack) > 0 || len(entry.toolsStack) > 0
	entry.mu.Unlock()

	if hasBuffer {
		// flushInner assumes flushMu held — we already hold it.
		_, _ = m.flushInner(ctx, entry, rawChatID, topicID, "endProcess")
	}

	entry.mu.Lock()
	msgID := entry.messageID
	entry.mu.Unlock()
	if msgID > 0 {
		m.deleteOrphan(ctx, rawChatID, topicID, msgID)
	}
}

// deleteOrphan removes a DraftMessage by id. On failure the
// message stays in the chat — acceptable since the next turn
// creates a fresh DraftMessage.
func (m *liveDraftManager) deleteOrphan(ctx context.Context, rawChatID string, topicID int, msgID int) {
	if msgID == 0 {
		return
	}
	if err := m.api.call(ctx, "deleteMessage", map[string]any{
		"chat_id":    rawChatID,
		"message_id": msgID,
	}, nil); err != nil {
		m.log.Warn("telegram: liveDraft delete failed",
			"chat_id", rawChatID,
			"thread_id", topicID,
			"message_id", msgID,
			"err", err,
		)
	}
}

// buildBlocksFromStacks renders thinking events first, then each
// tool slot as [Start?, End1, End2, ...] paragraph blocks.
func buildBlocksFromStacks(thinking []richTurnEntry, tools []toolSlot) []map[string]any {
	total := len(thinking)
	for _, s := range tools {
		total++
		total += len(s.ends)
	}
	blocks := make([]map[string]any, 0, total)
	for _, t := range thinking {
		blocks = append(blocks, map[string]any{"type": "paragraph", "text": t.body})
	}
	for _, slot := range tools {
		if slot.start.body != "" {
			blocks = append(blocks, map[string]any{"type": "paragraph", "text": slot.start.body})
		}
		for _, e := range slot.ends {
			blocks = append(blocks, map[string]any{"type": "paragraph", "text": e.body})
		}
	}
	return blocks
}
