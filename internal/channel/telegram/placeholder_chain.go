package telegram

import (
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

// chainKey indexes a chain by (chatID, topicID, userMessageID). DM
// (thread_id == 0) is a distinct chain from a real Forum topic on the
// same chatID; user messages with different reply anchors live on
// separate chains so back-to-back user inputs don't bleed Out* events
// across turns (see docs/channel/telegram.md §11.12.2).
//
// userMessageID == 0 is the race-window sentinel: handleMessage hasn't
// run yet for this turn, ensurePlaceholderForHeartbeat returns (0,nil),
// and OutHeartbeat/OutError that race ahead of handleMessage get a
// scratch chain keyed on 0. ensurePlaceholder purges that scratch chain
// when handleMessage finally lands, replacing it with the real key.
type chainKey struct {
	chatID        string
	topicID       int
	userMessageID int
}

// placeholderChain is the per-turn chain container. Holds at most one
// *active* chunk (chain.cursor); older chunks are frozen (chunk.isFull)
// and never re-rendered or re-PATCHed.
//
// chunks now hold the OOP chunkBody type (see chunk_body.go) —
// business code mutates fields through chunkBody methods; rendering
// is encapsulated in chunkBody.Compose().
type placeholderChain struct {
	mu sync.Mutex

	chunks []*chunkBody
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

	// toolPending is the FIFO of in-flight OutToolStart events for
	// this turn. Each entry remembers where the start body landed so
	// the matching OutToolEnd can rewrite that chunkEntry in place
	// (`● Tool(args)` → `● Tool(args)\n⎿  result`), keeping both lines
	// in a single Telegram message. FIFO ordering (push back, pop
	// front) matches feishu's tool_thread_merge.go — most agents
	// pair Start/End in order, and parallel tool_use pops the oldest
	// first.
	//
	// Scoped per-chain so LRU eviction of an unrelated turn's chain
	// drops its toolPending along with the chain. OutToolEnd that
	// arrives after eviction falls back to a fresh appendSegment for
	// the result line (start, if it landed in a still-live chunk,
	// stays as a lone `●` — visible but not silently lost).
	toolPending []toolPendingEntry
}

// toolPendingEntry is one in-flight OutToolStart. The matching
// OutToolEnd rewrites chunks[chunkIdx].entries[entryIdx].text from
// the original `● Tool(args)\n` to `startBody + "\n" + resultBody\n`,
// so the call line and the result line render as one chunkEntry —
// one Telegram message, exactly like feishu's PATCH-merge UX but
// staying inside the chunked chain so the StatusBar / header /
// footer render naturally around it.
type toolPendingEntry struct {
	chunkIdx  int
	entryIdx  int
	startBody string
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

// getOrCreateChain returns the chain for (chatID, topicID, userMessageID),
// creating it if absent. Updates LRU access order. Evicts the oldest chain
// when the index is full.
//
// Pre-condition: a.mu MUST NOT be held when calling this (the function
// takes its own lock).
func (l *chainLRU) getOrCreate(chatID string, topicID int, userMessageID int) *placeholderChain {
	key := chainKey{chatID: chatID, topicID: topicID, userMessageID: userMessageID}

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

// lookup returns the chain for (chatID, topicID, userMessageID) without
// modifying LRU access order. Adapters call this when they need to
// inspect a chain (e.g. ensuring its debounce timer is stopped before
// a purge) without advancing it to the tail.
func (l *chainLRU) lookup(chatID string, topicID int, userMessageID int) (*placeholderChain, bool) {
	key := chainKey{chatID: chatID, topicID: topicID, userMessageID: userMessageID}
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.chains[key]
	return c, ok
}

// evictOldestLocked removes the head-of-order chain. Must hold mu.
// Aborts silently when chains is empty (defensive — caller must check).
//
// P2 fix (2026-08-23): also stop the evicted chain's pending
// debounce timer. Without this, an orphan timer fires 250 ms
// later and ghost-edits the evicted chain's chunk messageID —
// visible to the user as text from a now-untracked chain.
func (l *chainLRU) evictOldestLocked() {
	if len(l.order) == 0 {
		return
	}
	old := l.order[0]
	l.order = l.order[1:]
	chain, ok := l.chains[old]
	delete(l.chains, old)
	if ok && chain != nil {
		chain.mu.Lock()
		stopDebounceTimer(chain)
		// Drop any in-flight OutToolStart bookkeeping. The chain
		// itself is being evicted; the chunks it owns stay on
		// Telegram as historical evidence, but no future
		// OutToolEnd will arrive at this adapter instance with
		// a matching chain to consume it from. Clearing the
		// FIFO here is hygienic — defensive against a stale
		// reference lingering in some future caller.
		chain.clearToolPending()
		chain.mu.Unlock()
	}
}

// reset clears all chains. Called on Adapter.Stop to drop in-memory
// state (chains are per-process; don't survive restart in any case).
func (l *chainLRU) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.chains = make(map[chainKey]*placeholderChain)
	l.order = nil
}

// purge removes the chain for (chatID, topicID, userMessageID) if present.
// Called by ensurePlaceholder when a new user message arrives so the
// previous turn's chain is forgotten (its frozen chunks remain in chat
// as historical evidence).
func (l *chainLRU) purge(chatID string, topicID int, userMessageID int) {
	key := chainKey{chatID: chatID, topicID: topicID, userMessageID: userMessageID}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.chains, key)
	for i, k := range l.order {
		if k == key {
			l.order = append(l.order[:i], l.order[i+1:]...)
			return
		}
	}
}

// --- toolPending FIFO ------------------------------------------------
//
// pushToolStartEntry / popToolStartEntry / clearToolPending implement
// the OutToolStart → OutToolEnd rewrite bookkeeping. Callers MUST
// hold chain.mu. The Adapter layer takes the lock around both the
// appendSegmentLocked (start path) and the pop+rewrite+flush
// sequence (end path), so no extra locking is needed here.

// pushToolStartEntry records where the start body landed so the
// matching OutToolEnd can rewrite that chunkEntry in place. The
// FIFO order matches feishu/tool_thread_merge.go — most agents
// pair Start/End in order, and parallel tool_use pops the oldest
// in-flight start first.
func (c *placeholderChain) pushToolStartEntry(chunkIdx, entryIdx int, startBody string) {
	c.toolPending = append(c.toolPending, toolPendingEntry{
		chunkIdx:  chunkIdx,
		entryIdx:  entryIdx,
		startBody: startBody,
	})
}

// popToolStartEntry returns and removes the front in-flight start.
// Returns (_, false) when nothing is pending — caller should fall
// back to a fresh appendSegment for the result body.
func (c *placeholderChain) popToolStartEntry() (toolPendingEntry, bool) {
	if len(c.toolPending) == 0 {
		return toolPendingEntry{}, false
	}
	front := c.toolPending[0]
	// Drop the front without retaining the backing array (long
	// turns could otherwise pin a growing slice).
	if len(c.toolPending) == 1 {
		c.toolPending = nil
	} else {
		next := make([]toolPendingEntry, len(c.toolPending)-1)
		copy(next, c.toolPending[1:])
		c.toolPending = next
	}
	return front, true
}

// clearToolPending empties the FIFO. Called on chain.reset() /
// LRU eviction so a future OutToolEnd on a recycled chain can't
// accidentally rewrite a stale entry. Safe under mu; no-op when
// the FIFO is already empty.
func (c *placeholderChain) clearToolPending() {
	c.toolPending = nil
}
