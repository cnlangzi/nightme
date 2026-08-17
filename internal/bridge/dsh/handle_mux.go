// handle_mux.go — mux frame 派发入口 (F-DSH-CHAT-001 §4.5)
//
// 把原本在 translate.go 的 handleMuxFrame 拆出来,理由:
//   1. translate.go 现在只管 sessionEventEnvelope 这一层,不再操心
//      mux frame 顶层 method 分发
//   2. session/projection 现在走 wireState.applyProjection 单独通道
//      (D3 修复,治 To-dos 面板在 projection 路径下渲染缺失)
//   3. session/event 走 dispatcher.dispatch,委派给 eventRegistry 里的
//      注册 handler(治 D4:加新事件不动 switch)
//
// mux frame 顶层 method 一览(从 docs/bridge/dsh.md §1 推断):
//   - session/subscribed:基线 seq 记录(无 event)
//   - session/event:经 dispatcher 路由
//   - session/projection:经 wireState.applyProjection
//   - session/queue / session/jobs:本期不消费,debug log
//   - approval/requested / approval/resolved:permissions 层处理
//   - approval/asked:经 handleInlineApproval
//   - question/requested / question/resolved:handleQuestionRequested

package dsh

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
)

// warnLogger is the package-level slog handle. Cached once at
// init so the dispatch hot path doesn't repeatedly call
// slog.Default() (which has a small atomic-load + global-mutex
// cost). See dispatch.go for the same pattern.
var warnLogger = slog.Default()

// handleMuxFrame is the mux-pump entry. It unmarshals the payload
// and dispatches by method.
// and dispatches by method. Extracted from translate.go in
// F-DSH-CHAT-001 so the dispatcher owns the event Type switch
// (registration-driven) instead of an inline switch statement.
//
// Routing:
//   - session/event     → driver.dispatcher.dispatch (registered handlers)
//   - session/projection → driver.wireState.applyProjection (state-only)
//   - approval/asked    → driver.handleInlineApproval (permissions layer)
//   - other mux methods → debug log only
func (d *driver) handleMuxFrame(method, rpcID string, payload json.RawMessage) {
	// Panic recover: the mux pump goroutine in host.StreamHub
	// dispatches every server-pushed frame through here. A panic
	// in any case would propagate up into the stream.go
	// readUntilClose loop and kill the mux connection — every
	// subsequent reconnect would re-process the same bad frame
	// and panic again. Recover + log so the mux pump survives.
	defer func() {
		if r := recover(); r != nil {
			warnLogger.Error("dsh: mux frame handler panic recovered",
				"method", method,
				"rpc_id", rpcID,
				"panic", fmt.Sprintf("%v", r),
				"stack", string(debug.Stack()))
		}
	}()
	switch method {
	case "session/subscribed":
		// Baseline frame on stream open. Just log it; the
		// SessionID is already known via session.create.
		d.wireState.recordWireFrame(method, "", len(payload))
		var sub muxSessionSubscribed
		if err := json.Unmarshal(payload, &sub); err != nil {
			dLog("dsh: subscribed decode: %v", err)
			return
		}
		dLog("dsh: subscribed", "session_id", sub.SessionID, "last_seq", sub.LastSeq)
		d.bumpLastSeq(sub.LastSeq)

	case "session/event":
		var ev muxSessionEvent
		if err := json.Unmarshal(payload, &ev); err != nil {
			dLog("dsh: session/event decode: %v", err)
			return
		}
		// Decode envelope Type once to drive dispatcher lookup.
		var env sessionEventEnvelope
		if err := json.Unmarshal(ev.Event, &env); err != nil {
			dLog("dsh: session/event envelope decode: %v", err)
			return
		}
		// Ring buffer record happens INSIDE dispatcher.dispatch
		// (per-frame, with the env.Type for triage). Mux-level
		// doesn't double-record here.
		//
		// dispatchEvent dedupes by SessionEvent.seq against the
		// lastSeq the backfill poll is also writing to. Mux
		// session/event is the live path after session.create
		// attach; backfill only fills gaps.
		d.dispatchEvent(env, ev.View)

	case "session/projection":
		// D3 fix: previously dropped on the floor (default dLog).
		// Now routed through wireState.applyProjection so the
		// bridge sees To-dos / Title updates from host-computed
		// projection frames.
		d.wireState.recordWireFrame(method, "", len(payload))
		var proj projectionEnvelope
		if err := json.Unmarshal(payload, &proj); err != nil {
			dLog("dsh: session/projection decode: %v", err)
			return
		}
		events := d.wireState.applyProjection(proj)
		for _, ev := range events {
			d.deliver(ev)
		}

	case "approval/requested":
		d.wireState.recordWireFrame(method, "", len(payload))
		var ar muxApprovalRequested
		if err := json.Unmarshal(payload, &ar); err != nil {
			dLog("dsh: approval/requested decode: %v", err)
			return
		}
		d.handleApprovalRequested(rpcID, ar)

	case "approval/resolved":
		d.wireState.recordWireFrame(method, "", len(payload))
		var ar muxApprovalResolved
		if err := json.Unmarshal(payload, &ar); err != nil {
			dLog("dsh: approval/resolved decode: %v", err)
			return
		}
		d.handleApprovalResolved(ar)

	case "question/requested":
		d.wireState.recordWireFrame(method, "", len(payload))
		var qr muxQuestionRequested
		if err := json.Unmarshal(payload, &qr); err != nil {
			dLog("dsh: question/requested decode: %v", err)
			return
		}
		// Same routing-key constraint as approval/requested above:
		// /api/respond is keyed on the server-frame rpcId, NOT the
		// payload's SessionID+":q". We pass rpcID as the key and let
		// handleQuestionRequested manage the display logic.
		d.handleQuestionRequested(rpcID, qr)

	case "question/resolved":
		d.wireState.recordWireFrame(method, "", len(payload))
		var qr muxQuestionResolved
		if err := json.Unmarshal(payload, &qr); err != nil {
			dLog("dsh: question/resolved decode: %v", err)
			return
		}
		d.handleQuestionResolved(qr.QuestionRPCID, qr.Outcome)

	case "session/queue":
		d.wireState.recordWireFrame(method, "", len(payload))
		// Future: surface queued/steering items in a QueueDock UI
		// (F-38 follow-up). For now, debug log only.
		dLog("dsh: session/queue: %d bytes", len(payload))

	case "session/jobs":
		d.wireState.recordWireFrame(method, "", len(payload))
		dLog("dsh: session/jobs: %d bytes", len(payload))

	case "approval/asked":
		// Mux schema has no top-level approval/asked (only
		// session/event type approval/asked). If a frame still
		// arrives here, skip — the respondable gate is
		// approval/requested keyed on this envelope's rpcId.
		d.wireState.recordWireFrame(method, "", len(payload))
		dLog("dsh: mux approval/asked ignored (use approval/requested)")

	default:
		// Unknown mux method. P4: single lock acquire for ring
		// record + count bump + count read (via
		// recordAndCountUnknown). Warn level surfaces ops that
		// "dsh added a method we don't handle" — operators read
		// DumpWireStats to triage.
		unknownTotal := d.wireState.recordAndCountUnknown(method, len(payload))
		warnLogger.Warn("dsh: mux unknown method",
			"method", method,
			"len", len(payload),
			"unknown_total", unknownTotal)
	}
}
