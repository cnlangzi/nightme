package telegram

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
)

// Stack capacity for the simulated DraftMessage in ChatKind=group.
//
// Two parallel stacks, each capped at 5:
//
//   - thinkingStack: holds up to 5 OutThinking events (latest 5).
//   - toolsStack: holds up to 5 tool-call slots. Each slot is
//     "1 OutToolStart + its trailing N consecutive OutToolEnd
//     events". When the 6th slot would arrive, the buffer must
//     flush first so the new slot has room.
//
// When either stack reaches its cap, an immediate flush is
// triggered before appending the new event. The new event then
// enters the (now empty) stack. The 10s timer is the fallback
// trigger when neither stack fills.
//
// With both caps at 5, the max buffer size is roughly 10 events
// (5 thinking + 5 tool slots) — comfortably under Telegram's
// per-chat 1/s and per-group 20/min limits.
const (
	thinkingStackCap = 5
	toolsStackCap    = 5

	// 10s timer fallback when neither stack fills. Without this
	// a turn that emits 1 OutThinking + 1 OutToolStart would never
	// flush — both stacks are well below cap.
	groupDraftBatchInterval = 10 * time.Second
)

// toolSlot is one entry in toolsStack: a single OutToolStart
// followed by its trailing OutToolEnd events. The slot is created
// when the Start arrives and stays open until either the next
// Start closes it implicitly, or the buffer flushes.
//
// An OutToolEnd arriving with no open slot becomes an orphan
// slot (start is empty, ends holds the lone End) so the result
// line is still visible after flush.
type toolSlot struct {
	start richTurnEntry // zero-value richTurnEntry if the slot has no Start
	ends  []richTurnEntry
}

// groupDraftEntry is the per-turn in-memory state for the simulated
// DraftMessage surface used by ChatKind=group. The buffer is two
// parallel stacks (thinkingStack + toolsStack), each capped at 5.
// Both stacks clear on every successful flush (windowed semantics):
// the Telegram message shows the last flushed snapshot until the
// next flush replaces it.
//
// messageID is the Telegram message_id returned by the first
// (cold-create) flush. 0 means "not yet sent". In-memory only —
// no persisted DraftMessageID.
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

	// Two parallel stacks, each capped. Cleared after every
	// successful flush.
	thinkingStack []richTurnEntry // cap 5
	toolsStack    []toolSlot      // cap 5 slots

	// 10s timer fallback.
	flushTimer *time.Timer

	// Captured context for timer callbacks (which fire in their own
	// goroutine and need to re-acquire mu before touching state).
	rawChatID string
	topicID   int
	userMsgID int
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
//	    → ensureEntry + append to thinkingStack / toolsStack
//	    → flush triggered by stack-cap (5), 10s timer, or endProcess
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
// appends to the appropriate stack and triggers a flush if the
// new event would overflow that stack's cap. The 10s timer is
// armed/refreshed to cover the case where neither stack fills.
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

	// Decide whether appending this event would overflow its
	// stack's cap. If so, flush first so the new event has room.
	// The flush is synchronous — we hold entry.mu through the API
	// call — so by the time we append, the stacks are empty.
	needsFlush := false
	switch kind {
	case messages.OutThinking:
		needsFlush = len(entry.thinkingStack) >= thinkingStackCap
	case messages.OutToolStart:
		needsFlush = len(entry.toolsStack) >= toolsStackCap
	case messages.OutToolEnd:
		// End appends to the last open slot; only overflows when
		// the End is orphan (no open slot) AND toolsStack is at
		// cap — in that case it would create a 6th slot, so flush.
		if len(entry.toolsStack) == 0 {
			needsFlush = len(entry.toolsStack) >= toolsStackCap
		}
	}

	if needsFlush {
		if _, err := m.flushLocked(ctx, entry, rawChatID, topicID, "capacity"); err != nil {
			return false, err
		}
	}

	// Append to the appropriate stack.
	switch kind {
	case messages.OutThinking:
		entry.thinkingStack = append(entry.thinkingStack, richTurnEntry{
			kind: "thinking",
			body: segment,
		})
	case messages.OutToolStart:
		// New Start closes the implicit "open" state of the prior
		// slot (it can no longer receive Ends). Subsequent Ends
		// attach to THIS slot.
		entry.toolsStack = append(entry.toolsStack, toolSlot{
			start: richTurnEntry{kind: "tool", body: segment},
		})
	case messages.OutToolEnd:
		if len(entry.toolsStack) == 0 {
			// Orphan End (no preceding OutToolStart): treat as
			// the implicit Start of a new slot. Subsequent Ends
			// (or a fresh Start) will close this slot naturally.
			entry.toolsStack = append(entry.toolsStack, toolSlot{
				start: richTurnEntry{kind: "tool", body: segment},
			})
		} else {
			slot := &entry.toolsStack[len(entry.toolsStack)-1]
			slot.ends = append(slot.ends, richTurnEntry{kind: "tool", body: segment})
		}
	}

	m.log.Info("telegram: streamDraftEvent buffered",
		"chat_id", rawChatID,
		"thread_id", topicID,
		"user_msg_id", userMsgID,
		"kind", kind.String(),
		"thinking_stack_len", len(entry.thinkingStack),
		"tools_stack_len", len(entry.toolsStack),
		"message_id", entry.messageID,
	)

	m.startFlushTimerIfIdleLocked(entry)
	return true, nil
}

// flushLocked serializes the current thinkingStack + toolsStack as a
// rich message and PATCHes the Telegram message via editMessageText
// (or sendRichMessage for the first flush). On success both stacks
// clear (windowed semantics); on failure they are preserved for the
// next trigger to retry on top of (issue #391 — fall through would
// leak tool result lines into the rich turn body).
//
// Caller MUST hold entry.mu.
func (m *groupDraftManager) flushLocked(ctx context.Context, entry *groupDraftEntry, rawChatID string, topicID int, reason string) (bool, error) {
	if len(entry.thinkingStack) == 0 && len(entry.toolsStack) == 0 {
		return true, nil
	}

	blocks := renderStacksAsBlocks(entry.thinkingStack, entry.toolsStack)
	blocksJSON, err := json.Marshal(blocks)
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
		// place.
		method = "editMessageText"
		params["message_id"] = entry.messageID
		err = m.api.call(ctx, "editMessageText", params, nil)
	}

	if err != nil {
		m.log.Warn("telegram: group DraftMessage flush failed",
			"chat_id", rawChatID,
			"thread_id", topicID,
			"method", method,
			"message_id", entry.messageID,
			"thinking_blocks", len(entry.thinkingStack),
			"tool_blocks", toolSlotBlockCount(entry.toolsStack),
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
		"thinking_blocks", len(entry.thinkingStack),
		"tool_blocks", toolSlotBlockCount(entry.toolsStack),
		"reason", reason,
	)
	entry.thinkingStack = entry.thinkingStack[:0]
	entry.toolsStack = entry.toolsStack[:0]
	if entry.flushTimer != nil {
		entry.flushTimer.Stop()
		entry.flushTimer = nil
	}
	return true, nil
}

// renderStacksAsBlocks flattens thinkingStack + toolsStack into the
// ordered list of paragraph blocks sent to Telegram. Layout:
// thinking events in stack order (oldest first), then each tool
// slot rendered as [Start, End1, End2, ...].
func renderStacksAsBlocks(thinkingStack []richTurnEntry, toolsStack []toolSlot) []map[string]any {
	total := len(thinkingStack)
	for _, s := range toolsStack {
		total++
		total += len(s.ends)
	}
	blocks := make([]map[string]any, 0, total)
	for _, t := range thinkingStack {
		blocks = append(blocks, map[string]any{"type": "paragraph", "text": t.body})
	}
	for _, slot := range toolsStack {
		if slot.start.body != "" {
			blocks = append(blocks, map[string]any{"type": "paragraph", "text": slot.start.body})
		}
		for _, e := range slot.ends {
			blocks = append(blocks, map[string]any{"type": "paragraph", "text": e.body})
		}
	}
	return blocks
}

// toolSlotBlockCount returns the total number of paragraph blocks
// a tool slot list would render (Start + each End).
func toolSlotBlockCount(slots []toolSlot) int {
	n := 0
	for _, s := range slots {
		if s.start.body != "" {
			n++
		}
		n += len(s.ends)
	}
	return n
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
		if len(entry.thinkingStack) == 0 && len(entry.toolsStack) == 0 {
			entry.flushTimer = nil
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
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
	if len(entry.thinkingStack) > 0 || len(entry.toolsStack) > 0 {
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
