// host_waterfall.go — route dsh 0.1.2-rc.1 host $events waterfall
// frames (approval/request, user-questions/request) to the per-session
// driver, and route the runtime's permission card answer back via
// POST /api/$events/result.
//
// Verified 2026-09-11 against dsh 0.1.2-rc.1 by capturing the actual
// wire shapes:
//
//   - waterfall frame on $events stream (host $events):
//     { type:"waterfall", event:"user-questions/request",
//       eventId:"<uuid>", agentId:"<sessionId>",
//       request:{questions:[…], agent:{id:"<sessionId>"}, signal:"…"}}
//
//   - answer RPC:
//     POST /api/$events/result
//     { type:"client-request", rpcId:"<uuid>", method:"$events/result",
//       payload:{ args:{
//         clientId:"<from ready frame>",
//         eventId: "<from waterfall frame>",
//         outcome:{ kind:"result",
//                   value:{ answers:[{id:"…", selected:["…"]}] } } } } }
//
// Architecture:
//
//	StreamHub.onHostFrame → Client.Router.DispatchHost
//	    → hostWaterfallHandler (installed once)
//	    → driver.handleHostFrame(method, rpcID, payload)
//
// Demux key: the $events `ready` frame carries a per-connection
// `clientId`; each remote client (nightme + dashboard) gets its own
// when it subscribes. The bridge echoes it back on every
// /api/$events/result RPC body. Capture happens in the dispatch path
// (host/stream.go:case "ready" → host.SetHostClientID), NOT here —
// the host handler installs lazily on first newDriver, and the
// one-shot ready frame would otherwise race the install and be
// silently dropped. See host/host_state.go for the race-fix
// invariant.
//
// Package-level map (`hostWaterfallBySess`) demuxes host waterfalls
// by sessionId → driver. The shared host has at most one driver
// per sessionId (per-chat-session lifecycle), and the map is
// concurrent-safe under hostWaterfallMu.

package dsh

import (
	"encoding/json"
	"fmt"
	"runtime/debug"
	"sync"

	"github.com/cnlangzi/nightme/internal/bridge/dsh/host"
)

var (
	hostWaterfallMu     sync.RWMutex
	hostWaterfallBySess map[string]*driver // sessionID → driver
)

// installHostHandler wires hostWaterfallHandler onto cli's Router.
// SetHostHandler is per-Client and replaces any prior handler, so
// the call is naturally idempotent on the same Client and safe to
// re-invoke when spawnAndWire constructs a new Client after dsh
// respawns. The clientId slot lives in the host/ subpackage now
// (see host/host_state.go) and is overwritten on every new
// connection by the dispatch path — no reset needed here. The
// package-level state that DOES need clearing across respawns is
// `hostWaterfallBySess` (the driver demux table), cleared below
// before re-registering the new Client's handler.
func installHostHandler(cli *host.Client) {
	if cli == nil {
		return
	}
	hostWaterfallMu.Lock()
	defer hostWaterfallMu.Unlock()
	if hostWaterfallBySess == nil {
		hostWaterfallBySess = make(map[string]*driver)
	}
	cli.SetHostHandler(hostWaterfallHandler)
}

// registerDriverForWaterfall adds d to the package-level demux map
// keyed by d.sessionID. Safe to call repeatedly with the same driver
// (overwrites). Called from newDriver after handshake sets
// d.sessionID, and from Reset after the new sessionId is established.
func registerDriverForWaterfall(d *driver) {
	if d == nil || d.sessionID == "" {
		return
	}
	hostWaterfallMu.Lock()
	defer hostWaterfallMu.Unlock()
	if hostWaterfallBySess == nil {
		hostWaterfallBySess = make(map[string]*driver)
	}
	hostWaterfallBySess[d.sessionID] = d
}

// unregisterDriverForWaterfall removes d from the demux map.
// Idempotent.
func unregisterDriverForWaterfall(d *driver) {
	if d == nil || d.sessionID == "" {
		return
	}
	hostWaterfallMu.Lock()
	defer hostWaterfallMu.Unlock()
	delete(hostWaterfallBySess, d.sessionID)
}

// hostWaterfallHandler is the single cli.SetHostHandler callback.
// The first frame on the host $events stream is `{type:"ready",
// clientId, host:{home}}` — we stash clientId in the package state
// so every later waterfall frame's reply can carry the same
// clientId (the gateway correlates result → pending remote event
// via this key). Subsequent waterfall frames carry `event` (the
// event name) and `eventId` (the per-frame UUID) and are demuxed
// to the right driver by `agentId` (= sessionId for root sessions).
func hostWaterfallHandler(method, rpcID string, payload json.RawMessage) {
	slogDefault().Info("dsh: hostWaterfallHandler invoked", "method", method, "rpc_id", rpcID)
	// clientId capture is in host/stream.go:case "ready" (the
	// dispatch site) so the one-shot ready frame is captured
	// even if installHostHandler hasn't run yet. Nothing to do
	// here — just early-return so the event doesn't fall through
	// to the waterfall demux path, which would log
	// "method=ready (no driver handler)" and bury the original
	// signal under noise.
	if method == "ready" {
		return
	}

	// Gate events we route to the driver:
	//   - approval/request, user-questions/request: register a
	//     pending entry the runtime's permission card answers.
	//   - host/cancel: dsh cancelled the waterfall server-side
	//     (timeout, dashboard answered, NO_PROVIDER). Drop the
	//     pending entry so the runtime's ResponseCh unblocks
	//     immediately instead of waiting for the 5-minute
	//     permissionTimeout watchdog.
	//
	// Other host waterfall events (api-session/status,
	// api-session/activity, etc.) are fire-and-forget Cordis emits
	// with no agent-scoped answer expected; we debug-log and skip.
	if method != "approval/request" && method != "user-questions/request" && method != "host/cancel" {
		dLog("dsh: host waterfall method=%s (no driver handler)", method)
		return
	}

	var env waterfallEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		dLog("dsh: host waterfall decode: %v", err)
		return
	}
	if env.AgentID == "" {
		dLog("dsh: host waterfall missing agentId, dropping method=%s rpc_id=%s",
			method, rpcID)
		return
	}
	hostWaterfallMu.RLock()
	d := hostWaterfallBySess[env.AgentID]
	hostWaterfallMu.RUnlock()
	if d == nil {
		dLog("dsh: host waterfall for unsubscribed session agent_id=%s method=%s",
			env.AgentID, method)
		return
	}
	d.handleHostFrame(method, rpcID, payload)
}

// Register the host-waterfall install hook with the host package at
// import time. spawnAndWire (host/lifecycle.go) calls
// host.OnLifecycleInstall(cli) after constructing the Client but
// before cli.Start(ctx), so the `ready` frame dsh sends on the new
// WS arrives at a handler that is already wired.
func init() {
	host.SetLifecycleInstall(installHostHandler)
}

// handleHostFrame is the driver-side dispatch for host waterfall
// frames. Switches on the dsh event name and emits an
// EventAgentPermission directly (no mux-shape adaptation — the
// approval/question helpers in permissions.go are also reached
// via the legacy mux path, but host waterfall is the primary
// entry in 0.1.2-rc.1).
//
// frameRpcID is the per-waterfall UUID from the frame; it's the
// key dsh uses to correlate the user's answer back to the
// pending waterfall.
func (d *driver) handleHostFrame(method, rpcID string, payload json.RawMessage) {
	defer func() {
		if r := recover(); r != nil {
			warnLogger.Error("dsh: host waterfall handler panic recovered",
				"method", method,
				"rpc_id", rpcID,
				"panic", fmt.Sprintf("%v", r),
				"stack", string(debug.Stack()))
		}
	}()
	switch method {
	case "approval/request":
		var env waterfallEnvelope
		if err := json.Unmarshal(payload, &env); err != nil {
			dLog("dsh: approval/request envelope decode: %v", err)
			return
		}
		var ar waterfallApprovalRequest
		if err := json.Unmarshal(env.Request, &ar); err != nil {
			dLog("dsh: approval/request body decode: %v", err)
			return
		}
		d.handleApprovalRequested(rpcID, muxApprovalRequested{
			SessionID:  d.sessionID,
			ApprovalID: "host-" + rpcID,
			ToolName:   ar.ToolName,
			CallID:     ar.CallID,
			Reason:     ar.Reason,
			Source:     "host",
		})
	case "user-questions/request":
		var env waterfallEnvelope
		if err := json.Unmarshal(payload, &env); err != nil {
			dLog("dsh: user-questions/request envelope decode: %v", err)
			return
		}
		var qr waterfallQuestionRequest
		if err := json.Unmarshal(env.Request, &qr); err != nil {
			dLog("dsh: user-questions/request body decode: %v", err)
			return
		}
		d.handleQuestionRequested(rpcID, muxQuestionRequested{
			SessionID: d.sessionID,
			Questions: qr.Questions,
			Source:    "host",
		})
	case "host/cancel":
		// dsh cancelled this waterfall server-side (timeout,
		// dashboard answered, NO_PROVIDER). Drop the pending
		// entry under rpcID — the runtime's ResponseCh unblocks
		// immediately with "cancelled" so Feishu can PATCH the
		// card.
		if !d.dropPendingByRPCID(rpcID) {
			dLog("dsh: host/cancel for unknown rpc_id=%s (no pending entry)", rpcID)
		}
	default:
		dLog("dsh: unhandled host waterfall method=%s rpc_id=%s", method, rpcID)
	}
}
