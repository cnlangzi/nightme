// control_projection.go — host-side store for the dsh
// `session/control` stream's per-session projections.
//
// dsh 0.1.5-rc.1 publishes a baseline of every active session's
// projections on the session/control stream, then emits per-session
// `projection` deltas whenever one changes. The bridge only cares
// about `modelSelection` (the rest ride through undecoded).
//
// The store is keyed by sessionId; each entry holds the latest
// resolved model id. Drivers subscribe to changes via
// WatchSessionModel and read the current value via GetSessionModel.
//
// Concurrency: the per-session entry is guarded by a sync.Mutex on
// the entry struct. WatchSessionModel registers a callback under
// the same lock; callbacks fire under the lock too (so they cannot
// race each other for the same session) but the callback body
// itself must be non-blocking — the dispatch path holds no other
// locks and a slow callback would back up the WS readLoop.

package host

import (
	"sync"
)

// modelWatchCB fires when the session's model selection changes.
// `model` is the resolved model id (next ?? lastUsed), "" when the
// session has not yet recorded a model.
//
// `fresh` is true on the first invocation for a given session —
// the watcher's initial value arrives here, so callers can use the
// callback for both "current state" and "future changes" without
// a separate read-after-register race. The store always invokes the
// callback once on register with the current value (even if "").
type modelWatchCB func(model string, fresh bool)

// controlProjection is the per-Client store for the session/control
// stream's modelSelection projection. There is one instance per
// *Client; the lifecycle matches the Client (created in New, reset
// on every WS reconnect via ApplyBaseline so we never serve stale
// ids from a previous dsh incarnation).
type controlProjection struct {
	mu       sync.Mutex
	bySess   map[string]*modelEntry // sessionID → entry
	baseline bool                   // true once the WS has delivered its first baseline
	closed   bool                   // set by Client.Close; dispatch returns immediately when true
}

func (p *controlProjection) isClosed() bool {
	if p == nil {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

type modelEntry struct {
	resolve  string // resolved next ?? lastUsed, cached
	watchers []*modelWatcher
}

// modelWatcher is the slot pattern used so WatchSessionModel can
// return a stable unsubscribe handle. Without it we can't compare
// two `modelWatchCB` values (Go's func type forbids `==`).
type modelWatcher struct {
	cb modelWatchCB
}

func newControlProjection() *controlProjection {
	return &controlProjection{bySess: make(map[string]*modelEntry)}
}

// dispatch is the ControlFrameHandler wired by Client.New. It
// applies the decoded frame to the projection store. Called from
// the WS readLoop goroutine — must be non-blocking.
func (p *controlProjection) dispatch(frame ControlFrame) {
	if p == nil || p.isClosed() {
		return
	}
	switch frame.Kind {
	case ControlBaseline:
		if frame.Models != nil {
			p.ApplyBaseline(frame.Models)
		}
	case ControlProjection:
		if frame.Key == "modelSelection" {
			p.ApplyProjection(frame.SessionID, frame.Key, frame.Model)
		}
		// Other projection keys are no-ops — forward-compat slot.
	}
}

// close marks the store as closed so any in-flight or future
// dispatch on the WS readLoop drops quietly. Idempotent. Called
// from Client.Close before the Hub closes — between those two
// points a stray control frame could still arrive (goroutine
// scheduling); the closed flag short-circuits it.
func (p *controlProjection) close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
}

// ApplyBaseline seeds the store from a fresh session/control
// baseline. Called once per WS connect (the first frame on the
// control stream). Pre-existing entries for sessions dsh no longer
// reports are dropped — dsh is the authority on "is this session
// still active" and a session that disappeared from the baseline
// either got archived or migrated to another dsh; in both cases
// the driver should re-fetch on resume.
func (p *controlProjection) ApplyBaseline(sessions map[string]string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.baseline = true
	// Collect every watcher fire we owe the caller, then drop the
	// lock before invoking them. Re-entry into the store from a
	// watcher (rare but possible — e.g. a watcher that calls
	// WatchSessionModel again on its own session) would deadlock
	// on sync.Mutex if we held the lock across the fire.
	type fire struct {
		w     *modelWatcher
		model string
		fresh bool
	}
	var fires []fire
	// Vanish fires: sessions dsh no longer reports. fresh=false so
	// the driver treats it as a model change (re-emits EventAgentReady
	// with model="") rather than silently dropping the Ready — the
	// runtime's SetModel is a no-op for "" but the persist side
	// effect is the same, and on next non-empty value the runtime
	// captures it.
	for sid, entry := range p.bySess {
		if _, ok := sessions[sid]; !ok {
			if entry.resolve != "" {
				entry.resolve = ""
				for _, w := range entry.watchers {
					fires = append(fires, fire{w, "", false})
				}
			}
		}
	}
	for sid, m := range sessions {
		entry, existed := p.bySess[sid]
		if !existed {
			entry = &modelEntry{}
			p.bySess[sid] = entry
		}
		if entry.resolve != m {
			entry.resolve = m
			fresh := !existed
			for _, w := range entry.watchers {
				fires = append(fires, fire{w, m, fresh})
			}
		}
	}
	p.mu.Unlock()
	for _, f := range fires {
		if f.w != nil && f.w.cb != nil {
			f.w.cb(f.model, f.fresh)
		}
	}
}

// ApplyProjection is called for every `type:"projection"` frame on
// the session/control stream. Only modelSelection is decoded; other
// keys are no-ops (kept for forward compat — when dsh adds a new
// projection the bridge should add a typed branch here).
func (p *controlProjection) ApplyProjection(sessionID, key string, resolvedModel string) {
	if p == nil || sessionID == "" || key != "modelSelection" {
		return
	}
	p.mu.Lock()
	entry, ok := p.bySess[sessionID]
	if !ok {
		entry = &modelEntry{}
		p.bySess[sessionID] = entry
	}
	if entry.resolve == resolvedModel {
		p.mu.Unlock()
		return
	}
	entry.resolve = resolvedModel
	watchers := make([]*modelWatcher, len(entry.watchers))
	copy(watchers, entry.watchers)
	p.mu.Unlock()
	// Fire AFTER dropping the lock — see re-entrancy note in
	// ApplyBaseline. fresh=false on every projection delta (the
	// driver distinguishes "first sight" from "live change" so it
	// can decide whether to emit Ready).
	for _, w := range watchers {
		if w == nil || w.cb == nil {
			continue
		}
		w.cb(resolvedModel, false)
	}
}

// GetSessionModel returns the latest resolved model id for sessionID,
// or "" if no projection has arrived yet. Cheap; safe for concurrent
// callers.
func (p *controlProjection) GetSessionModel(sessionID string) string {
	if p == nil || sessionID == "" {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.bySess[sessionID]
	if !ok {
		return ""
	}
	return entry.resolve
}

// WatchSessionModel registers cb to fire on every model change for
// sessionID. The callback fires once with the current value (fresh=
// true) before this function returns, even when that value is "" —
// so the caller can use the callback for both initial state and
// future updates without a separate GetSessionModel call.
//
// Returns an unsubscribe function. Idempotent — calling it twice
// is a no-op the second time.
func (p *controlProjection) WatchSessionModel(sessionID string, cb modelWatchCB) func() {
	if p == nil || sessionID == "" || cb == nil {
		return func() {}
	}
	p.mu.Lock()
	entry, ok := p.bySess[sessionID]
	if !ok {
		entry = &modelEntry{}
		p.bySess[sessionID] = entry
	}
	slot := &modelWatcher{cb: cb}
	entry.watchers = append(entry.watchers, slot)
	current := entry.resolve
	p.mu.Unlock()
	// Fire AFTER dropping the lock — the cb may re-enter the store
	// (rare but possible — e.g. another WatchSessionModel call on
	// the same goroutine) and re-entry would deadlock on the
	// non-recursive sync.Mutex.
	cb(current, true)
	return func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		entry, ok := p.bySess[sessionID]
		if !ok {
			return
		}
		for i, w := range entry.watchers {
			if w == slot {
				entry.watchers = append(entry.watchers[:i], entry.watchers[i+1:]...)
				return
			}
		}
	}
}
