package telegram

import (
	"log/slog"
	"strconv"
	"sync"
)

// draftIndex manages draftStreamer instances keyed by
// (chat_id, thread_id). Streamers persist across the daemon's
// lifetime AND across turns — draft state is global per
// (chat, thread). The reset method exists for tests and future
// use (e.g. an admin command to clear draft state) but is not
// called from the production ensurePlaceholder path.
type draftIndex struct {
	mu        sync.Mutex
	streamers map[string]*draftStreamer
}

func newDraftIndex() *draftIndex {
	return &draftIndex{streamers: make(map[string]*draftStreamer)}
}

// draftIndexKey is a pure function — same shape as stateStore.topicKey
// so the two indexes use consistent keying.
func draftIndexKey(chatID int64, threadID int) string {
	return strconv.FormatInt(chatID, 10) + "|" + strconv.Itoa(threadID)
}

func (i *draftIndex) getOrCreate(api apiClient, log *slog.Logger, chatID int64, threadID int) *draftStreamer {
	key := draftIndexKey(chatID, threadID)
	i.mu.Lock()
	defer i.mu.Unlock()
	if s, ok := i.streamers[key]; ok {
		return s
	}
	s := newDraftStreamer(api, log, chatID, threadID)
	i.streamers[key] = s
	return s
}

// reset purges the streamer state at turn boundary (called from
// ensurePlaceholder). The streamer object stays in the index — the
// next turn reuses it but starts with a fresh draft_id and cleared
// failure latch.
func (i *draftIndex) reset(chatID int64, threadID int) {
	key := draftIndexKey(chatID, threadID)
	i.mu.Lock()
	defer i.mu.Unlock()
	if s, ok := i.streamers[key]; ok {
		s.resetState()
	}
}
