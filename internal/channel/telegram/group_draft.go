package telegram

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
)

// Two FIFO stacks for the simulated DraftMessage in
// ChatKind=group, each capped at 50:
//
//   - thinkingStack []richTurnEntry  cap 50: holds OutThinking
//     events in arrival order. On overflow, oldest is FIFO-evicted.
//   - toolsStack    []toolSlot       cap 50: each slot is "1
//     OutToolStart + its trailing N OutToolEnd events". New
//     OutToolStart opens a new slot; OutToolEnd appends to the
//     LAST slot's ends. On overflow, oldest slot is FIFO-evicted.
//
// Send window per flush: latest 5 from each stack (or fewer if
// the stack has fewer). Each flush sends a single rich_message
// with thinking blocks first, then tool blocks, all under
// Telegram's per-message length cap. 5+5 = 10 blocks × ~1KB
// each ≈ 10KB, well under the limits.
//
// Flush triggers: 10s timer fallback, endProcess / OnPromptEnded.
// No count-based trigger — with cap 50 + send 5, dropping
// buffered events on a count threshold would defeat the purpose
// of keeping recent context.
const (
	thinkingStackCap  = 50
	toolsStackCap     = 50
	groupDraftSendMax = 5 // per-stack, per flush

	groupDraftBatchInterval = 10 * time.Second
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

// groupDraftEntry is the per-turn in-memory state for the simulated
// DraftMessage surface used by ChatKind=group. Two FIFO stacks,
// each capped at 50, hold all events since the last flush; the
// buffer is bounded and Telegram only ever sees the latest 5 from
// each stack per flush.
//
// Two locks:
//   - mu protects the two stacks (append / peek / pop). Held
//     briefly during the mutation only.
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

	// Two parallel FIFO stacks, each capped at 50.
	thinkingStack []richTurnEntry // cap 50
	toolsStack    []toolSlot      // cap 50 slots

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

// streamDraftEvent appends one event to the appropriate stack
// (with FIFO eviction at cap 50) and arms the 10s flush timer if
// no timer is already pending. The actual Telegram API call
// happens in flushLocked when the timer fires (or endProcess).
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
	entry.mu.Unlock()

	m.log.Info("telegram: streamDraftEvent buffered",
		"chat_id", rawChatID,
		"thread_id", topicID,
		"user_msg_id", userMsgID,
		"kind", kind.String(),
		"thinking_stack_len", thinkLen,
		"tools_stack_len", toolLen,
		"message_id", entry.messageID,
	)

	m.startFlushTimerIfIdleLocked(entry)
	return true, nil
}

// flushLocked sends the LATEST N events from each stack (N =
// groupDraftSendMax, or all if fewer) as a single rich_message
// via sendRichMessage (first flush) or editMessageText
// (subsequent flushes). Popped entries are removed from the
// stacks; on API failure they are restored (issue #391 contract).
//
// Lock discipline:
//  1. mu — peek + pop the to-send slice from each stack, release.
//  2. flushMu — serialize the API call.
//  3. On failure, mu — restore the popped slices.
//  4. flushMu release.
//
// Step 1+2 ensures the buffer mutation happens under mu, the API
// call happens under flushMu (without mu), so a slow API call
// never blocks new streamDraftEvent calls.
func (m *groupDraftManager) flushLocked(ctx context.Context, entry *groupDraftEntry, rawChatID string, topicID int, reason string) (bool, error) {
	// 1. Pop the latest 5 from each stack under mu.
	entry.mu.Lock()
	if len(entry.thinkingStack) == 0 && len(entry.toolsStack) == 0 {
		entry.mu.Unlock()
		return true, nil
	}

	nThink := groupDraftSendMax
	if nThink > len(entry.thinkingStack) {
		nThink = len(entry.thinkingStack)
	}
	sendingThink := make([]richTurnEntry, nThink)
	copy(sendingThink, entry.thinkingStack[len(entry.thinkingStack)-nThink:])
	entry.thinkingStack = entry.thinkingStack[:len(entry.thinkingStack)-nThink]

	nTools := groupDraftSendMax
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
	entry.mu.Unlock()

	// 2. Serialize the API call.
	entry.flushMu.Lock()
	defer entry.flushMu.Unlock()

	blocks := buildBlocksFromStacks(sendingThink, sendingTools)
	blocksJSON, err := json.Marshal(map[string]any{"blocks": blocks})
	if err != nil {
		// Marshal failure: restore the slices and bail.
		entry.mu.Lock()
		entry.thinkingStack = append(sendingThink, entry.thinkingStack...)
		entry.toolsStack = append(sendingTools, entry.toolsStack...)
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
			"sent_thinking", len(sendingThink),
			"sent_tools", len(sendingTools),
			"remaining_thinking", remainingThink,
			"remaining_tools", remainingTools,
			"reason", reason,
			"err", err,
		)
		// Restore the popped slices back to the buffers.
		entry.mu.Lock()
		entry.thinkingStack = append(sendingThink, entry.thinkingStack...)
		entry.toolsStack = append(sendingTools, entry.toolsStack...)
		entry.mu.Unlock()
		return true, nil
	}

	m.log.Info("telegram: group DraftMessage flushed",
		"chat_id", rawChatID,
		"thread_id", topicID,
		"method", method,
		"message_id", entry.messageID,
		"sent_thinking", len(sendingThink),
		"sent_tools", len(sendingTools),
		"remaining_thinking", remainingThink,
		"remaining_tools", remainingTools,
		"reason", reason,
	)
	if remainingThink == 0 && remainingTools == 0 && entry.flushTimer != nil {
		// Both buffers empty; no point keeping a timer alive.
		entry.flushTimer.Stop()
		entry.flushTimer = nil
	}
	return true, nil
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

// startFlushTimerIfIdleLocked arms the 10s debounce timer if no
// timer is already pending. Caller MUST NOT hold entry.mu.
func (m *groupDraftManager) startFlushTimerIfIdleLocked(entry *groupDraftEntry) {
	entry.mu.Lock()
	if entry.flushTimer != nil {
		entry.mu.Unlock()
		return
	}
	entry.flushTimer = time.AfterFunc(groupDraftBatchInterval, func() {
		entry.mu.Lock()
		if len(entry.thinkingStack) == 0 && len(entry.toolsStack) == 0 {
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
	hasBuffer := len(entry.thinkingStack) > 0 || len(entry.toolsStack) > 0
	entry.mu.Unlock()

	if hasBuffer {
		_, _ = m.flushLocked(ctx, entry, rawChatID, topicID, "endProcess")
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
