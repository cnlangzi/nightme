package telegram

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
)

// draftIndex manages draftStreamer instances keyed by
// (chat_id, thread_id, user_msg_id). One streamer per turn so a
// transient wire failure in turn N doesn't poison subsequent turns
// in the same chat, and so cross-turn visual surfaces stay isolated
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

// reset was the prior-DM-turn cleanup hook. Kept for the parallel
// API surface with group_draft.go's ensureEntry / endProcess
// pattern; current DM callers have no use for it but the index
// stays symmetric so future per-turn bookkeeping has a stable hook.
func (i *draftIndex) reset(chatID int64, threadID int, userMsgID int) {
	key := draftIndexKey(chatID, threadID, userMsgID)
	i.mu.Lock()
	defer i.mu.Unlock()
	if s, ok := i.streamers[key]; ok {
		s.mu.Lock()
		s.draftID = 0
		s.thinkingStack = s.thinkingStack[:0]
		s.toolsStack = s.toolsStack[:0]
		s.mu.Unlock()
	}
}

// endProcess signals that the current agent process is finished
// (typically after OutResult / OnPromptEnded). For DM we
// EVICT the streamer — the next turn allocates a fresh one with
// a fresh draft_id, which is the whole point of the per-turn
// isolation. The streamer first runs its own endProcess to stop
// the timer and flush any remaining buffered events as the final
// surface before the real message (OutResult) lands.
func (i *draftIndex) endProcess(ctx context.Context, chatID int64, threadID int, userMsgID int) {
	key := draftIndexKey(chatID, threadID, userMsgID)
	i.mu.Lock()
	s, ok := i.streamers[key]
	delete(i.streamers, key)
	i.mu.Unlock()
	if !ok {
		return
	}
	s.endProcess(ctx)
}
