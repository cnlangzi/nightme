// Package chatsession — HeartbeatTracker (F-63).
//
// HeartbeatTracker accumulates per-turn progress counters
// (ThinkCount / ToolCount / LastBeatAt) keyed by userMsgID and
// exposes a single Observe() choke point that the runtime handler
// calls before the policy chain. The tracker is the canonical
// state for "is the agent still making progress during this turn";
// Feishu adapter (and any future channel) reads it via OutboundMessage
// snapshots delivered through the OutHeartbeat kind.
//
// Why this lives in chatsession (not runtime as the F-63 doc first
// sketched): ChatSession owns the per-chat state and is the only
// place that holds userMsgID-keyed data for the duration of a turn;
// runtime imports chatsession (not the other way around). Moving
// HeartbeatTracker to chatsession keeps the dependency arrow
// pointing the same direction as everything else in the runtime
// package. Architecture is unchanged — only the file location
// differs from the doc.
//
// Memory model: bounded LRU. When more than Cap distinct
// userMsgIDs touch the tracker, the least-recently-used entry is
// evicted. The cap is intentionally generous (1024 by default)
// because active chat sessions rarely exceed a handful of
// in-flight prompts at once; the cap exists as a safety valve
// against unbounded growth from long-lived daemons, not as a
// cache-size tuning knob.
//
// Concurrency: sync.Mutex guards all fields. Reads via Snapshot
// are short (one map lookup). Writes via Observe touch the LRU
// order slice under the same lock; under typical load (a few
// events per turn per chat) lock contention is negligible.
package chatsession

import (
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
)

// DefaultHeartbeatCap is the default LRU capacity for
// HeartbeatTracker. ~32 KB at zero values (32 bytes per
// HeartbeatSnapshot × 1024 entries); production usage typically
// sees < 16 entries (1-3 AgentSessions per chat × a handful of
// in-flight prompts).
const DefaultHeartbeatCap = 1024

// HeartbeatTracker accumulates per-turn progress counters keyed by
// userMsgID. The runtime handler calls Observe() on every
// outbound event (BEFORE the policy chain — see F-63 §3.2 for
// the core invariant); callers that want to render the current
// state call Snapshot() to read a copy.
//
// No explicit Drop: when the LRU evicts an entry, the userMsgID
// simply disappears. Receipts that already hold their own copy of
// the heartbeat snapshot (via Feishu MessageReceipt.heartbeat)
// continue to render correctly; subsequent OutHeartbeat emits for
// the evicted userMsgID produce a zero-valued snapshot that the
// channel adapter's Empty() guard drops.
type HeartbeatTracker struct {
	mu    sync.Mutex
	cap   int
	order []string                          // LRU order: head = most recent
	snaps map[string]messages.HeartbeatSnapshot
}

// NewHeartbeatTracker constructs a tracker with the given LRU
// capacity. Pass DefaultHeartbeatCap for production; tests may
// pass a smaller value to exercise eviction deterministically.
// cap <= 0 falls back to DefaultHeartbeatCap.
func NewHeartbeatTracker(cap int) *HeartbeatTracker {
	if cap <= 0 {
		cap = DefaultHeartbeatCap
	}
	return &HeartbeatTracker{
		cap:   cap,
		snaps: make(map[string]messages.HeartbeatSnapshot, cap),
	}
}

// Observe records an outbound event against the given userMsgID
// and returns whether ThinkCount or ToolCount actually changed.
// LastBeatAt is always refreshed (it's the "agent is alive"
// signal), but a refresh-only Observe returns false — callers
// should skip emitting OutHeartbeat when nothing meaningful
// changed, since that would just burn a Feishu PATCH for an
// identical body.
//
// Counting rules (intentionally narrow, see F-63 §2 非目标):
//
//	OutThinking  → ThinkCount++   (returns true)
//	OutToolStart → ToolCount++    (returns true)
//	everything else → only refresh LastBeatAt (returns false)
//
// OutHeartbeat itself is not in the counting switch — but since
// Observe is only called from the handler with OutboundKinds
// produced by gateway.Translate on raw AgentEvents, OutHeartbeat
// never reaches Observe in practice (the handler emits
// OutHeartbeat via em.Send, not via Observe). Defensive: if it
// somehow does, the default branch only refreshes LastBeatAt and
// returns false — no recursion, no double-counting.
//
// userMsgID == "" is a no-op (returns false) — protects against
// orphan events (EventAgentReady, etc.) that don't have a
// receipt anchor.
func (t *HeartbeatTracker) Observe(userMsgID string, kind messages.OutboundKind) bool {
	if t == nil || userMsgID == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	snap := t.snaps[userMsgID]
	snap.LastBeatAt = time.Now()

	changed := false
	switch kind {
	case messages.OutThinking:
		snap.ThinkCount++
		changed = true
	case messages.OutToolStart:
		snap.ToolCount++
		changed = true
	default:
		// No counter change. We still write back the refreshed
		// snapshot + touch the LRU so this userMsgID stays
		// "recent" (it IS recent activity).
		t.snaps[userMsgID] = snap
		t.touchLocked(userMsgID)
		return false
	}
	t.snaps[userMsgID] = snap
	t.touchLocked(userMsgID)
	return changed
}

// Snapshot returns a copy of the current heartbeat state for
// userMsgID. Zero-value (no entry) is a valid response — callers
// should pass the result to the channel adapter which uses
// HeartbeatSnapshot.Empty() to decide whether to render anything.
func (t *HeartbeatTracker) Snapshot(userMsgID string) messages.HeartbeatSnapshot {
	if t == nil {
		return messages.HeartbeatSnapshot{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snaps[userMsgID]
}

// touchLocked moves userMsgID to the head of the LRU order and
// evicts the tail when the cap is exceeded. Caller must hold
// t.mu.
//
// Implementation: linear search + slice splice. O(n) per touch
// but n <= cap = 1024 in production and touches happen at most
// a few times per turn per chat — well under a microsecond.
// Avoids pulling in container/list or a third-party LRU lib for
// negligible wins at this scale.
func (t *HeartbeatTracker) touchLocked(userMsgID string) {
	// Splice out any existing entry for this userMsgID.
	for i, u := range t.order {
		if u == userMsgID {
			t.order = append(t.order[:i], t.order[i+1:]...)
			break
		}
	}
	// Push to head.
	t.order = append([]string{userMsgID}, t.order...)
	// Evict tail entries until we're under cap.
	for len(t.order) > t.cap {
		evicted := t.order[len(t.order)-1]
		t.order = t.order[:len(t.order)-1]
		delete(t.snaps, evicted)
	}
}

// Len returns the current number of tracked userMsgIDs. Intended
// for tests and observability; production code does not need it.
func (t *HeartbeatTracker) Len() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.snaps)
}

// Cap returns the LRU capacity. Useful for tests that want to
// assert eviction boundaries.
func (t *HeartbeatTracker) Cap() int {
	if t == nil {
		return 0
	}
	return t.cap
}