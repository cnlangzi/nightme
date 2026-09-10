// Package chatsession — HeartbeatTracker (F-63).
//
// HeartbeatTracker is the per-turn state machine for "what is the
// agent doing right now". It accumulates activity counters
// (ThinkCount / ToolCount / LastBeatAt) and a terminal verdict
// (Status) keyed by userMsgID. The runtime handler, the GTW
// sink, and the CS pump's PromptEndBus subscriber all funnel
// through Observe() — a single choke point that decides whether
// the event is a counter bump or a terminal flip based on the
// OutboundMessage kind and payload. The tracker is the canonical
// state for "is the agent still making progress, and is this
// turn over yet"; Feishu adapter (and any future channel) reads
// it via OutboundMessage snapshots delivered through the
// OutHeartbeat kind.
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
// userMsgID. Callers route every outbound event through Observe
// (BEFORE the policy chain — see F-63 §3.2 for the core
// invariant); callers that want to render the current state call
// Snapshot() to read a copy.
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
	order []string // LRU order: head = most recent
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

// Observe is the single choke point for all heartbeat state
// transitions. The msg.Kind determines the action; the payload
// (msg.Err, msg.PromptEndReason) drives the verdict when the
// kind is terminal:
//
//	OutThinking     → ThinkCount++                      (returns true)
//	OutToolStart    → ToolCount++                       (returns true)
//	OutResult       → flipTerminal:
//	                   msg.Err == nil → HeartbeatDone
//	                   msg.Err != nil → HeartbeatError  (returns true)
//	OutPromptEnded  → flipTerminal:
//	                   msg.PromptEndReason == nil ||
//	                   !msg.PromptEndReason.IsError()
//	                   → HeartbeatDone
//	                   else → HeartbeatError           (returns true)
//	everything else → refresh LastBeatAt only          (returns false)
//
// Returns true when anything visible changed (counter or
// Status). Callers should use this to decide whether to emit a
// follow-up OutHeartbeat; a refresh-only Observe should NOT
// produce a redundant PATCH.
//
// All terminal flips are idempotent and verdict-agnostic: the
// second Observe on the same userMsgID with any terminal kind
// (or a different verdict payload) is a no-op. The first caller
// wins; racing triggers (e.g. the runtime's OutResult branch
// racing the CS pump's PromptEndBus subscriber) converge on a
// single transition.
//
// OutHeartbeat itself is not in the kind switch — Observe is
// only called with OutboundKinds produced by gateway.Translate
// on raw AgentEvents or by the PromptEndBus subscriber; the
// runtime emits OutHeartbeat via em.Send, so the kind never
// recurses. Defensive default branch treats it as a refresh.
//
// userMsgID == "" is a no-op (returns false) — protects against
// orphan events (EventAgentReady, etc.) that don't have a
// receipt anchor.
func (t *HeartbeatTracker) Observe(userMsgID string, msg messages.OutboundMessage) bool {
	if t == nil || userMsgID == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	snap := t.snaps[userMsgID]
	snap.LastBeatAt = time.Now()

	changed := false
	switch msg.Kind {
	case messages.OutThinking:
		snap.ThinkCount++
		changed = true
	case messages.OutToolStart:
		snap.ToolCount++
		changed = true
	case messages.OutResult:
		// Verdict derived from msg.Err: a populated Err signals
		// the bridge reported an errored result (Claude Code
		// result.is_error / pi error turn). nil → clean.
		status := messages.HeartbeatDone
		if msg.Err != nil {
			status = messages.HeartbeatError
		}
		if t.flipTerminalLocked(&snap, status) {
			changed = true
		}
	case messages.OutPromptEnded:
		// Verdict derived from msg.PromptEndReason. nil reason
		// (defensive — production always populates it) and
		// non-error reasons → Done; IsError() → Error. Mirrors
		// the previous eventbus.go IsError() collapse.
		status := messages.HeartbeatDone
		if msg.PromptEndReason != nil && msg.PromptEndReason.IsError() {
			status = messages.HeartbeatError
		}
		if t.flipTerminalLocked(&snap, status) {
			changed = true
		}
	default:
		// Refresh-only — no counter / no terminal flip. Fall
		// through to the tail which writes back the
		// LastBeatAt refresh and touches the LRU so this
		// userMsgID stays recent for eviction ordering.
	}
	t.snaps[userMsgID] = snap
	t.touchLocked(userMsgID)
	return changed
}

// flipTerminalLocked transitions the snapshot to a terminal
// verdict. Returns true on the false→true transition (running →
// terminal, regardless of which terminal), false otherwise
// (no-op when already terminal). The first caller wins;
// subsequent calls with a different verdict are still a no-op
// (the tracker is verdict-agnostic on transition).
//
// Caller must hold t.mu. Does NOT touch the LRU — the
// caller's Observe tail handles LRU unconditionally so terminal
// and non-terminal paths share one touch point (no double-touch
// on transitions). Does NOT touch ThinkCount / ToolCount /
// LastBeatAt for the same reason; those live in Observe. Caller
// writes the modified snap back via t.snaps[userMsgID] = snap
// after this returns.
func (t *HeartbeatTracker) flipTerminalLocked(snap *messages.HeartbeatSnapshot, status messages.HeartbeatStatus) bool {
	if snap.Status != messages.HeartbeatRunning {
		return false
	}
	snap.Status = status
	return true
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
