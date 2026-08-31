package slack

import (
	"container/list"
	"sync"
	"time"
)

// defaultDedupCap bounds the dedup index. Each entry is two short
// strings plus a timestamp, so a thousand of them is negligible, and
// the window only needs to outlive the gap between Slack's two
// deliveries of the same message (milliseconds in practice).
const defaultDedupCap = 1024

// defaultDedupTTL is how long a seen key stays remembered.
const defaultDedupTTL = 5 * time.Minute

// dedupIndex suppresses double-processing of a single Slack message.
//
// Why this exists: nightme subscribes to BOTH app_mention AND
// message.channels (docs/channel/slack.md §5.4 — the "permissions
// wide open" decision is what makes /watch all possible at all).
// Slack delivers an @-mention in a channel through both
// subscriptions, so without this the agent would answer twice.
// cc-connect never hit this because it only subscribed to
// app_mention.
//
// Keyed by (channel, ts) because that pair identifies a Slack
// message uniquely, and both deliveries carry the same values.
type dedupIndex struct {
	mu    sync.Mutex
	cap   int
	ttl   time.Duration
	now   func() time.Time
	order *list.List               // front = oldest
	items map[string]*list.Element // key -> element holding dedupEntry
}

type dedupEntry struct {
	key  string
	seen time.Time
}

func newDedupIndex(capacity int, ttl time.Duration) *dedupIndex {
	if capacity <= 0 {
		capacity = defaultDedupCap
	}
	if ttl <= 0 {
		ttl = defaultDedupTTL
	}
	return &dedupIndex{
		cap:   capacity,
		ttl:   ttl,
		now:   time.Now,
		order: list.New(),
		items: make(map[string]*list.Element),
	}
}

// seen records the (channel, ts) pair and reports whether it had
// already been recorded. First call returns false ("process it");
// every later call within the TTL returns true ("drop it").
func (d *dedupIndex) seen(channelID, ts string) bool {
	if d == nil || channelID == "" || ts == "" {
		return false
	}
	key := channelID + "|" + ts

	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.now()
	d.pruneLocked(now)

	if el, ok := d.items[key]; ok {
		entry := el.Value.(*dedupEntry)
		entry.seen = now
		d.order.MoveToBack(el)
		return true
	}

	el := d.order.PushBack(&dedupEntry{key: key, seen: now})
	d.items[key] = el
	for d.order.Len() > d.cap {
		d.evictOldestLocked()
	}
	return false
}

func (d *dedupIndex) pruneLocked(now time.Time) {
	for {
		front := d.order.Front()
		if front == nil {
			return
		}
		if now.Sub(front.Value.(*dedupEntry).seen) < d.ttl {
			return
		}
		d.removeLocked(front)
	}
}

func (d *dedupIndex) evictOldestLocked() {
	if front := d.order.Front(); front != nil {
		d.removeLocked(front)
	}
}

func (d *dedupIndex) removeLocked(el *list.Element) {
	entry := el.Value.(*dedupEntry)
	delete(d.items, entry.key)
	d.order.Remove(el)
}

// len reports the number of tracked keys (test hook).
func (d *dedupIndex) len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.order.Len()
}
