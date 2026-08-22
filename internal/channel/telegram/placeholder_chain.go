package telegram

import (
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// v9 chain rolling log — pure in-memory placeholder chain. Replaces v8's
// single-placeholder + independent-bubble dual track (see
// docs/channel/telegram.md §11.12). Per-turn chat lifecycle owns one
// chain; chain holds N chunks; active chunk receives new segments; older
// chunks lock in their final render and become historical evidence in
// the chat.
//
// Persist boundary (intentional): chain is NOT persisted to
// telegram_state.json. Daemon restart = chain lost = next event builds a
// fresh chunk. See §11.12.10 for the trade-off rationale.
// ---------------------------------------------------------------------------

// chainKey indexes a chain by (chatID, topicID). DM (thread_id == 0)
// is a distinct chain from a real Forum topic on the same chatID.
type chainKey struct {
	chatID  string
	topicID int
}

// placeholderChain is the per-turn chain container. Holds at most one
// *active* chunk (chain.cursor); older chunks are frozen (chunk.isFull)
// and never re-rendered or re-PATCHed.
type placeholderChain struct {
	mu sync.Mutex

	chunks []*placeholderChunk
	// cursor is the index of the chunk that accepts new segments.
	// -1 means the chain is empty (first event must materialise a chunk).
	cursor int

	// lastFooter is the most recent StatusBar the chain saw. Refreshed
	// by footer-bearing events (OutReply/OutResult/OutTask*); held stable
	// across non-footer events. nil = no footer-bearing event happened
	// yet → render emits no footer panel.
	lastFooter []string

	// dirty is true when the in-memory chunk buffer diverges from the
	// last Telegram-rendered text. flushChainNow consumes it.
	dirty bool

	// debounceTimer is the currently pending debounce. Resets on every
	// new event within the window so bursts coalesce.
	debounceTimer *time.Timer
}

type placeholderChunk struct {
	messageID  int64
	buf        strings.Builder
	charCount  int // buf.Len() snapshot; avoids per-step query
	isFull     bool
	headerLine string // "🤖 Working..." or "💭 N · 🔧 M · ⏱ HH:MM:SS"
}

// ---------------------------------------------------------------------------
// chainLRU is the Adapter-scoped index. Capacity-bounded with simple
// LRU eviction. NOT persisted; reset on daemon restart. See §11.12.2.
// ---------------------------------------------------------------------------

const defaultChainLRUCap = 1000

type chainLRU struct {
	mu     sync.Mutex
	cap    int
	chains map[chainKey]*placeholderChain
	// order holds access-order keys, head = least-recently-used,
	// tail = most-recently-used. Mutating on every get/create.
	order []chainKey
}

func newChainLRU(cap int) *chainLRU {
	if cap <= 0 {
		cap = defaultChainLRUCap
	}
	return &chainLRU{
		cap:    cap,
		chains: make(map[chainKey]*placeholderChain),
	}
}

// getOrCreateChain returns the chain for (chatID, topicID), creating it
// if absent. Updates LRU access order. Evicts the oldest chain when the
// index is full.
//
// Pre-condition: a.mu MUST NOT be held when calling this (the function
// takes its own lock).
func (l *chainLRU) getOrCreate(chatID string, topicID int) *placeholderChain {
	key := chainKey{chatID: chatID, topicID: topicID}

	l.mu.Lock()
	defer l.mu.Unlock()

	if c, ok := l.chains[key]; ok {
		l.touchLocked(key)
		return c
	}

	// Evict LRU if at capacity.
	if len(l.chains) >= l.cap {
		l.evictOldestLocked()
	}

	c := &placeholderChain{cursor: -1}
	l.chains[key] = c
	l.order = append(l.order, key)
	return c
}

// touchLocked moves the key to the tail of the LRU order. Must hold mu.
func (l *chainLRU) touchLocked(key chainKey) {
	// Remove key from its current position.
	for i, k := range l.order {
		if k == key {
			l.order = append(l.order[:i], l.order[i+1:]...)
			break
		}
	}
	l.order = append(l.order, key)
}

// evictOldestLocked removes the head-of-order chain. Must hold mu.
// Aborts silently when chains is empty (defensive — caller must check).
func (l *chainLRU) evictOldestLocked() {
	if len(l.order) == 0 {
		return
	}
	old := l.order[0]
	l.order = l.order[1:]
	delete(l.chains, old)
}

// reset clears all chains. Called on Adapter.Stop to drop in-memory
// state (chains are per-process; don't survive restart in any case).
func (l *chainLRU) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.chains = make(map[chainKey]*placeholderChain)
	l.order = nil
}
