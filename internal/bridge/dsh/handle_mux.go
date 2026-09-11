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
//   - approval/asked:debug log only (audit-only echo, NOT the
//     respondable gate; dsh 0.1.2-rc.1 puts approval on the host
//     $events waterfall as approval/request — see host_waterfall.go)
//
// Note: dsh 0.1.2-rc.1 folded approval + user-questions onto the
// host $events waterfall stream. The mux top-level
// approval/requested / approval/resolved / question/requested /
// question/resolved methods are no longer emitted; if a frame still
// arrives here it is recorded + counted + warn-logged as a
// regression marker. The respondable path is
// host_waterfall.go::driver.handleHostFrame, which adapts the
// waterfall envelope to the existing handleApprovalRequested /
// handleQuestionRequested helpers in permissions.go.
//
// dsh 0.1.2-rc.1 (new wire) folds the per-event type into the mux
// method itself: after host/stream.go::translateSessionEvent, the
// FrameHandler sees method="assistant/chunk" (not "session/event"
// with a nested envelope), and payload=event.data. handleMuxFrame
// routes these by constructing a synthetic sessionEventEnvelope
// and forwarding to dispatchEvent. The OLD wire method names
// (session/event etc.) are kept for backward-compat reads and the
// legacy session/subscribed / session/projection envelopes still
// arrive on the new wire under their old method names.

package dsh

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strconv"
	"strings"
)

// warnLogger is the package-level slog handle. Cached once at
// init so the dispatch hot path doesn't repeatedly call
// slog.Default() (which has a small atomic-load + global-mutex
// cost). See dispatch.go for the same pattern.
var warnLogger = slog.Default()

// handleMuxFrame is the mux-pump entry. It unmarshals the payload
// and dispatches by method. Extracted from translate.go in
// F-DSH-CHAT-001 so the dispatcher owns the event Type switch
// (registration-driven) instead of an inline switch statement.
//
// Routing:
//   - session/event     → driver.dispatcher.dispatch (registered handlers)
//   - session/projection → driver.wireState.applyProjection (state-only)
//   - approval/asked    → debug log (mux has no top-level approval/asked)
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

	case "approval/requested",
		"approval/resolved",
		"question/requested",
		"question/resolved":
		// dsh 0.1.2-rc.1 no longer emits these as mux top-level
		// methods — both approval and user-questions gate on the
		// host $events waterfall stream
		// (approval/request, user-questions/request). The
		// corresponding reply is `/api/respond` keyed on the
		// waterfall's eventId.
		//
		// The handling lives in host_waterfall.go →
		// driver.handleHostFrame. If a frame still arrives here
		// (older dsh rc, or a future rc reverting to mux),
		// record + count + warn so ops can spot the regression
		// without breaking the bridge.
		unknownTotal := d.wireState.recordAndCountUnknown(method, len(payload))
		warnLogger.Warn("dsh: mux legacy method dropped — dsh 0.1.2-rc.1 sends this as host waterfall",
			"method", method,
			"rpc_id", rpcID,
			"unknown_total", unknownTotal)

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

	case "session/snapshot":
		// dsh 0.1.2-rc.1 sends one snapshot frame as the FIRST
		// item on a session/follow stream (per SessionFollowFrame
		// typert: type="snapshot" with header, cursor, records,
		// hasMore, projections). Records may contain historical
		// events that happened before we subscribed; replay each
		// through dispatchEvent so wireState/translate stay
		// consistent. Update lastSeq to the snapshot cursor so
		// subsequent live events aren't deduped.
		d.wireState.recordWireFrame(method, "", len(payload))
		d.replaySnapshot(payload)

	case "host/cancel":
		// Unreachable today: Host waterfall items route through
		// StreamHub.dispatch → Router.DispatchHost (NOT
		// handleMuxFrame). Kept as a debug-log escape hatch in case
		// dsh starts sending host/* frames on the mux endpoint.
		d.wireState.recordWireFrame(method, "", len(payload))
		var c struct {
			EventID string `json:"eventId"`
		}
		_ = json.Unmarshal(payload, &c)
		dLog("dsh: host waterfall cancel event_id=%s", c.EventID)

	default:
		// dsh 0.1.2-rc.1 wire folds per-event type into the mux
		// method itself: host/stream.go::translateSessionEvent
		// returns method = event.type (e.g. "assistant/chunk",
		// "turn/start", "step/end", "user/message", "session/title",
		// "request/context", "session/title-llm-request", "usage",
		// "agent/inbox/spliced") and payload = event.data with
		// sessionId injected. Route these through dispatchEvent
		// so the registered handlers (assistant/chunk, turn/start,
		// etc.) handle them.
		if isSessionEventType(method) {
			env := sessionEventEnvelope{
				Type: method,
				Seq:  parseSeqFromRPCID(rpcID),
				Data: payload,
			}
			d.dispatchEvent(env, nil)
			return
		}
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

// isSessionEventType reports whether method is one of the
// dsh 0.1.2-rc.1 per-session event.type discriminators that
// arrive on session/follow. Kept as a tight allow-list (NOT a
// slash check) so we never accidentally route a legacy mux-frame
// method (session/subscribed, session/projection, etc.) through
// the per-event dispatch path. Must stay in sync with
// standardRegistry in dispatch.go — a missing entry silently
// demotes the frame to "unknown method" with a Warn.
func isSessionEventType(method string) bool {
	switch method {
	case "assistant/chunk", "assistant/message",
		"tool/call", "tool/result",
		"turn/start", "turn/end",
		"step/start", "step/end",
		"user/message",
		"session/title", "session/title-llm-request",
		"request/context",
		"agent/inbox/spliced",
		"approval/asked",
		"compaction/end",
		"todo/write", "todo/update", "todo/delete":
		return true
	}
	return false
}

// parseSeqFromRPCID extracts the numeric seq from a "seq-N" RPC ID
// minted by host/stream.go::translateSessionEvent. Returns 0 when
// the ID is empty or doesn't carry a seq — dispatchEvent treats
// seq=0 as "no prior watermark to dedupe against", which is
// benign for frames that don't carry one.
func parseSeqFromRPCID(rpcID string) int64 {
	rpcID = strings.TrimPrefix(rpcID, "seq-")
	n, _ := strconv.ParseInt(rpcID, 10, 64)
	return n
}

// replaySnapshot dispatches the records array of one
// SessionFollowFrame snapshot. Records are themselves
// SessionEvent envelopes with their own {type, seq, time, data}
// shape; route each through dispatchEvent exactly as if it had
// arrived live. After the loop, advance lastSeq to the snapshot
// cursor so the live stream doesn't redeliver anything below it.
// bumpLastSeq is idempotent (max of current and new) so the order
// matters only for the gap between "last record seq" and "cursor":
// if dsh's cursor means "the next seq we will deliver", and the
// last record we replayed has seq < cursor, the gap stays open
// for live events to fill.
//
// Records can be either {type:"event", event:{...}} OR
// {type:"chunks", event:{...}} (per typert). We only know how to
// dispatch "event" records today; "chunks" carries precomputed
// text/tool/reasoning chunkrow data which would need its own
// translator. Until F-32/F-52 redo that, log + drop the chunks
// records.
func (d *driver) replaySnapshot(payload json.RawMessage) {
	var snap struct {
		Header  json.RawMessage `json:"header,omitempty"`
		Cursor  int64           `json:"cursor,omitempty"`
		Records []struct {
			Type  string          `json:"type"`
			Event json.RawMessage `json:"event,omitempty"`
		} `json:"records,omitempty"`
	}
	if err := json.Unmarshal(payload, &snap); err != nil {
		dLog("dsh: snapshot decode: %v", err)
		return
	}
	for _, rec := range snap.Records {
		switch rec.Type {
		case "event":
			var env sessionEventEnvelope
			if err := json.Unmarshal(rec.Event, &env); err != nil {
				dLog("dsh: snapshot record decode: %v", err)
				continue
			}
			d.dispatchEvent(env, nil)
		case "chunks":
			// Pre-aggregated chunk rows; not yet handled.
			dLog("dsh: snapshot chunks record skipped (not implemented)")
		}
	}
	if snap.Cursor > 0 {
		d.bumpLastSeq(snap.Cursor)
	}
	dLog("dsh: snapshot replayed cursor=%d records=%d", snap.Cursor, len(snap.Records))
}
