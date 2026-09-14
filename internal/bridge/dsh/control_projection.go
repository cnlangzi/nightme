// control_projection.go — driver-side bridge between the host's
// session/control projection store and each driver's per-session
// model callback.
//
// Lifecycle:
//
//   - installControlHandler is registered as a host lifecycle hook
//     (see init() at the bottom). On every new Client construction
//     the hook is a no-op — the Client.Control.dispatch wiring is
//     already in host.go::New; what we need here is just a noop
//     hook so future per-Client setup has a place to land.
//   - Each driver registers itself via registerControlProjection
//     after handshakeSession sets d.sessionID. The registration
//     installs a modelWatchCB on cli.Control.WatchSessionModel.
//   - When the host projection store fires the callback, we look
//     up the driver by sessionId and call d.onSessionModelChanged.
//     That updates d.model and re-emits EventAgentReady so the
//     runtime's captured model stays current when the user switches
//     models via the dashboard picker mid-session.
//   - On Close the driver calls unregisterControlProjection, which
//     invokes the WatchSessionModel unsubscribe function.
//
// Concurrency: modelBySess is read on every projection callback
// (WS readLoop goroutine) and written on driver register / unregister
// (chat session goroutine). RWMutex guards both paths; reads
// dominate.

package dsh

import (
	"sync"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/dsh/host"
)

var (
	modelProjectionMu sync.RWMutex
	modelBySess       map[string]*driver // sessionID → driver
)

// installControlHandler is the lifecycle hook the parent dsh package
// registers with the host package at import time. It currently does
// nothing per-Client (the projection dispatch is wired in
// host.Client.New itself) — kept as a hook so future per-Client
// setup (e.g. wiping the demux table on respawn) has a stable
// extension point.
//
// Idempotent across respawns: a fresh Client comes with a fresh
// Control store, so nothing leaks across spawn/attach boundaries.
func installControlHandler(_ *host.Client) {}

// registerControlProjection installs a WatchSessionModel callback
// for d.sessionID. The callback updates d.model and (when the value
// changes after the initial fresh=true fire) re-emits an
// EventAgentReady so the runtime's SetModel sees the new model.
//
// Returns the unsubscribe function — caller must invoke on Close so
// the projection store doesn't fire into a stale driver. The fresh=
// true initial fire updates d.model synchronously inside this call;
// the caller can read d.model immediately after registering.
func registerControlProjection(d *driver) (unsub func()) {
	if d == nil || d.cli == nil || d.sessionID == "" {
		return func() {}
	}
	modelProjectionMu.Lock()
	if modelBySess == nil {
		modelBySess = make(map[string]*driver)
	}
	modelBySess[d.sessionID] = d
	modelProjectionMu.Unlock()

	unsub = d.cli.WatchSessionModel(d.sessionID, d.onSessionModelChanged)
	return unsub
}

// unregisterControlProjection removes d from the projection demux
// map and invokes the unsubscribe func returned by
// registerControlProjection. Idempotent.
func unregisterControlProjection(d *driver, unsub func()) {
	if d == nil || d.sessionID == "" {
		if unsub != nil {
			unsub()
		}
		return
	}
	modelProjectionMu.Lock()
	if cur, ok := modelBySess[d.sessionID]; ok && cur == d {
		delete(modelBySess, d.sessionID)
	}
	modelProjectionMu.Unlock()
	if unsub != nil {
		unsub()
	}
}

// onSessionModelChanged is invoked by the host projection store
// every time the dsh session/control stream publishes a model
// change (or once with fresh=true when the driver registers).
//
// fresh=true: the very first fire for this driver. We just stamp
// d.model — Ready will be emitted by the caller once it has the
// SessionID + Workspace + Branch context.
//
// fresh=false: a live change. Update d.model and re-emit an
// EventAgentReady so the runtime's SetModel sees the new value.
// Only emit if the value actually changed (the projection store
// already dedupes, but defensive check guards against a same-value
// fire slipping through if the store's hashing is ever loosened).
func (d *driver) onSessionModelChanged(model string, fresh bool) {
	if d == nil {
		return
	}
	d.modelMu.Lock()
	prev := d.model
	d.model = model
	d.modelMu.Unlock()
	if fresh || model == prev {
		return
	}
	dLog("dsh: session model changed",
		"session_id", d.sessionID,
		"prev", prev,
		"next", model)
	// Re-emit Ready with the new model. Runtime picks this up via
	// SetModel(ev.Model) (any non-empty ev.Model wins); the runtime
	// also persists the AgentSession on every Ready, which is a
	// cheap no-op when SessionID is unchanged.
	d.deliver(agent.AgentEvent{
		Kind:      agent.EventAgentReady,
		SessionID: d.sessionID,
		AgentName: d.agentName,
		Workspace: d.workspace,
		Branch:    detectBranch(d.workspace),
		Model:     model,
	})
}

// Note: the host lifecycle install hook is owned by host_waterfall.go
// (host.SetLifecycleInstall(installHostHandler)). No second init is
// registered here — host.Client.New wires Control.dispatch into the
// StreamHub on every construction, which is the only per-Client
// setup the projection store needs.
