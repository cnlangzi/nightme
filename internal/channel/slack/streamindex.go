package slack

import (
	"container/list"
	"sync"
)

// defaultStreamIndexCap bounds how many turns the adapter tracks at
// once. Each entry is one open (or recently closed) placeholder; the
// LRU exists so a long-lived daemon in a busy workspace cannot grow
// this map without bound.
const defaultStreamIndexCap = 512

// streamKeyOf identifies a turn: one user message in one chat.
func streamKeyOf(chatID, userMsgID string) string {
	return chatID + "|" + userMsgID
}

// streamIndex is an LRU of live turnStreams.
type streamIndex struct {
	mu    sync.Mutex
	cap   int
	order *list.List
	items map[string]*list.Element
}

type streamEntry struct {
	key    string
	stream *turnStream
}

func newStreamIndex(capacity int) *streamIndex {
	if capacity <= 0 {
		capacity = defaultStreamIndexCap
	}
	return &streamIndex{
		cap:   capacity,
		order: list.New(),
		items: make(map[string]*list.Element),
	}
}

// getOrCreate returns the turn's stream, minting one via build if it
// does not exist yet. created reports which happened, so callers can
// tell a cold start from a continuation.
func (idx *streamIndex) getOrCreate(chatID, userMsgID string, build func() *turnStream) (stream *turnStream, created bool) {
	key := streamKeyOf(chatID, userMsgID)

	idx.mu.Lock()
	defer idx.mu.Unlock()

	if el, ok := idx.items[key]; ok {
		idx.order.MoveToBack(el)
		return el.Value.(*streamEntry).stream, false
	}
	s := build()
	el := idx.order.PushBack(&streamEntry{key: key, stream: s})
	idx.items[key] = el
	for idx.order.Len() > idx.cap {
		idx.evictOldestLocked()
	}
	return s, true
}

// lookup returns the turn's stream without creating one.
func (idx *streamIndex) lookup(chatID, userMsgID string) (*turnStream, bool) {
	key := streamKeyOf(chatID, userMsgID)
	idx.mu.Lock()
	defer idx.mu.Unlock()
	el, ok := idx.items[key]
	if !ok {
		return nil, false
	}
	idx.order.MoveToBack(el)
	return el.Value.(*streamEntry).stream, true
}

// purge forgets a turn. The Slack message stays where it is; only
// the adapter's in-memory handle goes away.
func (idx *streamIndex) purge(chatID, userMsgID string) {
	key := streamKeyOf(chatID, userMsgID)
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if el, ok := idx.items[key]; ok {
		delete(idx.items, key)
		idx.order.Remove(el)
	}
}

// evictOldestLocked drops the least recently used entry. The evicted
// stream's timer is stopped so it cannot fire against a turn nobody
// is tracking any more.
func (idx *streamIndex) evictOldestLocked() {
	front := idx.order.Front()
	if front == nil {
		return
	}
	entry := front.Value.(*streamEntry)
	entry.stream.mu.Lock()
	entry.stream.stopTimerLocked()
	entry.stream.mu.Unlock()
	delete(idx.items, entry.key)
	idx.order.Remove(front)
}

// all returns every tracked stream (used on Stop to drain).
func (idx *streamIndex) all() []*turnStream {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	out := make([]*turnStream, 0, idx.order.Len())
	for el := idx.order.Front(); el != nil; el = el.Next() {
		out = append(out, el.Value.(*streamEntry).stream)
	}
	return out
}

func (idx *streamIndex) len() int {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.order.Len()
}
