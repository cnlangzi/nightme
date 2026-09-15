package telegram

import (
	"log/slog"
	"strconv"
	"sync"
)

// draftIndex manages draftStreamer instances keyed by
// (chat_id, thread_id, user_msg_id). One streamer per turn so a
// Bot API < 10.3 failure latch can't poison subsequent turns in
// the same chat, and so cross-turn visual surfaces stay isolated
// even if the runtime is late delivering events (see
// docs/channel/telegram.md §11.12.11.3 — per-prompt isolation).
//
// Streamers are evicted from the index on endProcess (called from
// Send's OutResult branch and OnPromptEnded). The index therefore
// tracks only "currently in-flight" turns — once a turn ends its
// streamer is gone, freeing draft_id namespace and avoiding
// unbounded growth on long-running daemons.
type draftIndex struct {
	mu        sync.Mutex
	streamers map[string]*draftStreamer
}

func newDraftIndex() *draftIndex {
	return &draftIndex{streamers: make(map[string]*draftStreamer)}
}

// draftIndexKey is a pure function. Three-tuple shape mirrors
// group_draft.go's groupDraftKey so the two paths stay
// behaviorally consistent across chat kinds.
func draftIndexKey(chatID int64, threadID int, userMsgID int) string {
	return strconv.FormatInt(chatID, 10) +
		"|" + strconv.Itoa(threadID) +
		"|" + strconv.Itoa(userMsgID)
}

func (i *draftIndex) getOrCreate(api apiClient, log *slog.Logger, chatID int64, threadID int, userMsgID int) *draftStreamer {
	key := draftIndexKey(chatID, threadID, userMsgID)
	i.mu.Lock()
	defer i.mu.Unlock()
	if s, ok := i.streamers[key]; ok {
		return s
	}
	s := newDraftStreamer(api, log, chatID, threadID, userMsgID)
	i.streamers[key] = s
	return s
}

// reset purges the streamer state for one turn (called from
// ensurePlaceholder's prior-turn cleanup, currently a no-op for
// DM but kept symmetric with group_draft.go's API surface so
// future DM-side bookkeeping has a stable hook).
func (i *draftIndex) reset(chatID int64, threadID int, userMsgID int) {
	key := draftIndexKey(chatID, threadID, userMsgID)
	i.mu.Lock()
	defer i.mu.Unlock()
	if s, ok := i.streamers[key]; ok {
		s.resetState()
	}
}

// endProcess signals that the current agent process is finished
// (typically after OutResult / OnPromptEnded). For DM we
// EVICT the streamer — the next turn allocates a fresh one with
// a fresh draft_id, which is the whole point of the per-turn
// isolation. The failed latch is preserved in case eviction is
// short-circuited (see resetProcess) but the typical happy-path
// caller wants the streamer gone so subsequent turns don't see
// stale state.
func (i *draftIndex) endProcess(chatID int64, threadID int, userMsgID int) {
	key := draftIndexKey(chatID, threadID, userMsgID)
	i.mu.Lock()
	defer i.mu.Unlock()
	if s, ok := i.streamers[key]; ok {
		s.resetProcess()
		delete(i.streamers, key)
	}
}
