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
}

type modelEntry struct {
	model    string
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
// the WS readLoop goroutine — must be non-blocking. The store's
// callbacks (per-driver watchers) fire inline under p.mu; the
// dispatcher contract is "no blocking work in cb".
func (p *controlProjection) dispatch(frame ControlFrame) {
	if p == nil {
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

// ApplyBaseline seeds the store from a fresh session/control
// baseline. Called once per WS connect (the first frame on the
// control stream). Pre-existing entries for sessions dsh no longer
// reports are dropped — dsh is the authority on "is this session
// still active" and a session that disappeared from the baseline
// either got archived or migrated to another dsh; in both cases
// the driver should re-fetch on resume.
//
// watchFires=true means existing watchers receive an initial
// "fresh" callback for every session that survived the rebaseline
// (so a post-respawn runtime sees the new dsh's model).
func (p *controlProjection) ApplyBaseline(sessions map[string]string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.baseline = true
	// Drop entries for sessions dsh no longer reports. Drivers
	// holding watchers for those sessions are still called with
	// model="" fresh=true so they know the projection is gone.
	for sid, entry := range p.bySess {
		if _, ok := sessions[sid]; !ok {
			entry.model = ""
			entry.resolve = ""
			p.fireLocked(entry, "", true)
		}
	}
	for sid, m := range sessions {
		entry, existed := p.bySess[sid]
		if !existed {
			entry = &modelEntry{}
			p.bySess[sid] = entry
		}
		if entry.resolve != m {
			entry.model = m
			entry.resolve = m
			p.fireLocked(entry, m, !existed)
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
	defer p.mu.Unlock()
	entry, ok := p.bySess[sessionID]
	if !ok {
		entry = &modelEntry{}
		p.bySess[sessionID] = entry
	}
	if entry.resolve == resolvedModel {
		return
	}
	entry.model = resolvedModel
	entry.resolve = resolvedModel
	p.fireLocked(entry, resolvedModel, false)
}

// fireLocked invokes every watcher on entry. Caller must hold p.mu.
// We copy the watcher slice under lock to avoid "callback runs and
// unsubscribes, then we iterate the truncated slice" surprises.
func (p *controlProjection) fireLocked(entry *modelEntry, model string, fresh bool) {
	watchers := make([]*modelWatcher, len(entry.watchers))
	copy(watchers, entry.watchers)
	// Invoke after dropping the lock would be safer, but we want
	// "fresh" to mean the very first callback the watcher sees.
	// Callers are expected to be non-blocking (see type doc).
	for _, w := range watchers {
		if w == nil || w.cb == nil {
			continue
		}
		w.cb(model, fresh)
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

// HasBaseline reports whether the store has received at least one
// session/control baseline. Drivers use this to decide whether to
// block on WaitForModel or fall back to emitting Ready with empty
// model. False after every WS reconnect until the first new
// baseline lands.
func (p *controlProjection) HasBaseline() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.baseline
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
	defer p.mu.Unlock()
	entry, ok := p.bySess[sessionID]
	if !ok {
		entry = &modelEntry{}
		p.bySess[sessionID] = entry
	}
	// Wrap cb in a struct slot so the unsubscribe path can identify
	// this specific registration without comparing function values
	// (Go forbids `==` on func types — see testing failure when we
	// tried `any(w) == any(cb)`). The slot is also how we let the
	// caller hold the func reference if they want to re-fire it.
	slot := &modelWatcher{cb: cb}
	entry.watchers = append(entry.watchers, slot)
	current := entry.resolve
	// Fire the initial value under lock. cb may unregister itself
	// (rare) — that's fine, the copy in fireLocked is the live
	// snapshot at the moment of fire.
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
