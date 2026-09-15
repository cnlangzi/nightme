package telegram

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
)

// groupDraftEntry is the per-turn in-memory state for the simulated
// DraftMessage surface used by the "group" ChatKind. The textBuf
// follows the same REPLACE / ACCUMULATE semantics as the DM
// draftStreamer (#383) — see draftBuffer.compose — so the two paths
// stay behaviorally consistent across chat kinds.
//
// messageID is the Telegram message_id returned by the cold-create
// sendMessage; 0 means "not yet sent" (the next event triggers
// sendMessage rather than editMessageText). Persisted to
// TopicState.DraftMessageID so a turn that survives a daemon
// restart can resume editing the same Telegram message.
//
// All mutable fields are guarded by mu. The lock is held across
// the network roundtrip on streamDraftEvent so concurrent events
// for the same turn compose + send in serial order — runtime
// normally emits sequentially per ChatSession so this is a no-op
// in practice, but the lock is what stops two concurrent cold-
// creates from double-sending.
//
// Per-prompt isolation (see docs §11.12.11.3) is enforced by the
// groupDraftKey already containing userMsgID: ensureEntry creates
// a fresh entry per turn, so cross-turn entry reuse cannot happen
// via the production keying. The adapter-side replyAnchor
// resolution (msg.ReplyTo first, state.UserMessageID fallback) is
// what guarantees caller correctness.
type groupDraftEntry struct {
	mu        sync.Mutex
	textBuf   strings.Builder
	messageID int
}

// composeLocked is the REPLACE / ACCUMULATE writer. Caller MUST hold
// b.mu (Go mutexes are not re-entrant; streamDraftEvent holds the
// lock across the network roundtrip, so this method body must NOT
// re-acquire it). REPLACE wipes prior content; ACCUMULATE appends
// after a "\n\n" separator (skipping the separator when the buffer
// is empty — the first event always replaces by virtue of being
// first).
func (b *groupDraftEntry) composeLocked(text string, replace bool) string {
	if replace {
		b.textBuf.Reset()
	} else if b.textBuf.Len() > 0 {
		b.textBuf.WriteString("\n\n")
	}
	b.textBuf.WriteString(text)
	return b.textBuf.String()
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
//	    → ensureEntry + compose + cold-create sendMessage + persist message_id
//	subsequent OutThinking / OutToolStart / OutToolEnd
//	    → compose + editMessageText
//	OutResult / OnPromptEnded
//	    → deleteMessage + clear state + drop entry
//
// Unlike the DM sendMessageDraft path, this surface has no failure
// latch: sendMessage / editMessageText / deleteMessage are stable
// across Bot API versions, so a transient error retries through
// apiCall's retry loop rather than dropping events.
type groupDraftManager struct {
	api   apiClient
	log   *slog.Logger
	store *stateStore
	mu    sync.Mutex
	// entries is keyed by chatID|topicID|userMsgID. Map-level
	// reads/writes are guarded by mu; per-entry mutation is
	// guarded by entry.mu (taken by streamDraftEvent for the
	// whole operation, by endProcess after dropping the entry).
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
	e := &groupDraftEntry{}
	m.entries[key] = e
	return e
}

// streamDraftEvent applies one OutThinking / OutToolStart /
// OutToolEnd event to the simulated DraftMessage for this turn:
// composes the in-memory buffer and PATCHes the Telegram message
// (or cold-creates it on the first event). The boolean mirrors
// streamDraftEvent's DM-side contract — true means "consumed, do
// not fall through to the richTurn path".
//
// userMsgID is BOTH the entry key (one entry per turn) and the
// reply_to_message_id anchor on the cold-create sendMessage — the
// DraftMessage visually hangs off the user's message like every
// other turn message. Per-prompt isolation is enforced by the
// groupDraftKey already containing userMsgID; the caller is
// responsible for passing the per-event anchor (msg.ReplyTo),
// see docs/channel/telegram.md §11.12.11.3.
func (m *groupDraftManager) streamDraftEvent(ctx context.Context, rawChatID string, topicID int, userMsgID int, segment string, replace bool) (bool, error) {
	if userMsgID <= 0 {
		// No user message anchor → can't safely cold-create (a
		// floating message would have nothing to chain under).
		// Bail to caller fallthrough (richTurn path renders it).
		return false, nil
	}
	m.mu.Lock()
	entry := m.ensureEntry(rawChatID, topicID, userMsgID)
	m.mu.Unlock()

	// Per-entry lock covers compose + send/edit. Runtime emits
	// events sequentially per ChatSession so contention is
	// bounded by serial events on the same turn; the lock only
	// stops the data race two goroutines would otherwise create
	// (both seeing messageID == 0 → both sendMessage).
	entry.mu.Lock()
	defer entry.mu.Unlock()

	body := entry.composeLocked(segment, replace)

	if entry.messageID == 0 {
		// Cold-create. The first compose result becomes the
		// initial body — there's no temporary "…" placeholder
		// visible to the user; the buffer is already correct.
		// This is a deliberate trade-off vs. draftStreamer's
		// server-managed draft, where the client never sees a
		// transient placeholder.
		params := map[string]any{
			"chat_id": rawChatID,
			"text":    body,
		}
		if topicID > 0 {
			params["message_thread_id"] = topicID
		}
		params["reply_to_message_id"] = userMsgID
		var result SendMessageResult
		if err := m.api.call(ctx, "sendMessage", params, &result); err != nil {
			m.log.Warn("telegram: group DraftMessage cold-create failed",
				"chat_id", rawChatID,
				"thread_id", topicID,
				"user_msg_id", userMsgID,
				"err", err,
			)
			return false, err
		}
		if result.MessageID == 0 {
			return false, errors.New("telegram: group DraftMessage cold-create returned empty message_id")
		}
		entry.messageID = result.MessageID
		// Persist so a daemon restart mid-turn can resume editing
		// the same message. putTopic failure is logged but does
		// not fail the event — the in-memory entry.messageID is
		// already correct for this process; only a restart
		// before this save completes would orphan the message.
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
		return true, nil
	}

	// Subsequent event: editMessageText with the buffer body.
	// Plain text — no parse_mode — so LLM output with literal "&"
	// or "<" survives the round-trip (same rationale as
	// draftStreamer for the DM path).
	params := map[string]any{
		"chat_id":    rawChatID,
		"message_id": entry.messageID,
		"text":       body,
	}
	if err := m.api.call(ctx, "editMessageText", params, nil); err != nil {
		// Transient failures retry inside apiCall; if the retry
		// budget is exhausted, the event is lost but the next
		// event will retry on top of the prior buffer (no latch).
		m.log.Warn("telegram: group DraftMessage edit failed",
			"chat_id", rawChatID,
			"thread_id", topicID,
			"message_id", entry.messageID,
			"err", err,
		)
		return false, err
	}
	return true, nil
}

// endProcess finalizes the simulated DraftMessage for a turn:
// deletes the underlying Telegram message, clears the persisted
// DraftMessageID, and drops the in-memory entry. Mirrors the DM
// draft's auto-disappear-on-real-message behavior (#383) so the
// group chat stays clean between turns.
//
// Safe to call when no DraftMessage exists for the turn (no-op).
// Called from adapter.OnPromptEnded (safety net for turns without
// OutResult) and from the OutResult send path. Idempotent across
// repeated calls (OutResult + OnPromptEnded both fire on the happy
// path; the second call sees an empty map and an already-cleared
// state, returns without a second deleteMessage).
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
		// No in-memory entry — try the persisted state in case
		// the daemon restarted mid-turn (orphan recovery; the
		// persisted message_id is the only handle we have).
		if m.store != nil {
			if state, ok := m.store.topic(rawChatID, topicID); ok && state != nil && state.DraftMessageID > 0 {
				m.deleteOrphan(ctx, rawChatID, topicID, state.DraftMessageID)
			}
		}
		return
	}
	// entry.mu serialises against any in-flight streamDraftEvent
	// so we read the latest messageID rather than racing past a
	// cold-create that's about to set one.
	entry.mu.Lock()
	msgID := entry.messageID
	entry.mu.Unlock()
	if msgID == 0 {
		// Never cold-created (turn had no think/tool events).
		return
	}
	m.deleteOrphan(ctx, rawChatID, topicID, msgID)
}

// deleteOrphan removes a DraftMessage by id and clears the
// persisted DraftMessageID on success. On failure the persisted
// state is left intact so the next ensurePlaceholder (for the
// next turn) sees it and re-attempts the delete. Called from:
//
//   - endProcess (turn-end cleanup)
//   - ensurePlaceholder (orphan recovery for the previous turn's
//     crash or deleteMessage-failed message)
//
// deleteMessage errors are logged not returned — failure to
// delete is a UX nit (orphan in chat) but not a correctness
// issue; the next ensurePlaceholder will retry.
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
// the orphan persists; apiCall already retries transient
// errors so reaching this path means the message is permanently
// stuck (e.g. permissions revoked). The next turn starts fresh
// regardless.
func (m *groupDraftManager) deleteOrphanSync(ctx context.Context, rawChatID string, topicID int, msgID int) {
	if msgID <= 0 || m == nil {
		return
	}
	m.deleteOrphan(ctx, rawChatID, topicID, msgID)
}
