// dispatch.go — 声明式事件派发 (F-DSH-CHAT-001 §4.4)
//
// 把原来 translate.go 里的 `switch env.Type` 封闭派发改成注册表驱动:
//
//   type eventHandler func(env sessionEventEnvelope, view json.RawMessage,
//                          tr *translator, st *wireState) []agent.AgentEvent
//
//   var eventRegistry = newRegistry(map[string]eventHandler{
//       "assistant/chunk":   handleAssistantChunk,
//       ...
//       "todo/update":       handleTodoUpdate,  // 提前注册,handler 现在返回 nil
//       "todo/delete":       handleTodoDelete,  // 同上
//   })
//
// 加新 dsh 事件 = 加一行注册 + 写一个 handler,**不动 switch 不动 default**。
// 提前注册尚未实现的 type(todo/update、todo/delete),让 dsh web 加这种事件
// 时 bridge 自动开始处理(即使 handler 返回 nil 也比静默 dLog 强)。
//
// Lock 契约:
//   - dispatcher.dispatch() 入口处 acquire translator.mu 和 wireState.mu
//     (固定顺序:translator 先,wireState 后)
//   - handler 内部**禁止**再次加锁(避免死锁)
//   - deliver 回调在锁外调用(避免 channel 写阻塞锁)
//
// 这是治本 D4:未知 wire frame 不再静默 dLog,而是走"registry lookup miss"
// 路径,Phase 4 可以加 ring buffer 计数 / debug dump。

package dsh

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
)

// eventHandler is the registered callback for one envelope Type.
// Receives the decoded envelope, optional View bytes, and access to
// the translator (for F-52 textBuf/pendingTools), wireState (for
// tasks/tools), and driver (for handlers that need driver methods
// like handleApprovalAsked → handleInlineApproval). Returns the
// AgentEvent sequence to deliver; nil for graceful no-op handlers
// (e.g. todo/update before wire emits it).
//
// Driver is passed even to handlers that don't need it (e.g.
// handleAssistantChunk) so the signature stays uniform — handlers
// that don't need d can simply ignore the parameter (Go's unnamed
// parameter convention). Passing d as nil is the test-friendly path
// for handlers that DO need it: handleApprovalAsked returns nil when
// d is nil.
type eventHandler func(env sessionEventEnvelope, view json.RawMessage, tr *translator, st *wireState, d *driver) []agent.AgentEvent

// registry maps envelope Type → handler. The map is read-only after
// init; handlers are pure top-level funcs. Lookup is O(1) — fine for
// the ~11 entries Phase 1+2 cares about (will stay fine even if
// dsh grows to 50+).
type registry struct {
	handlers map[string]eventHandler
}

func newRegistry(handlers map[string]eventHandler) *registry {
	return &registry{handlers: handlers}
}

func (r *registry) lookup(envType string) (eventHandler, bool) {
	h, ok := r.handlers[envType]
	return h, ok
}

// eventDispatcher ties together the translator (F-52 streaming state),
// wireState (multi-source truth), the driver (for handlers that need
// to call driver methods like handleInlineApproval), and the
// deliver callback. One instance per driver.
type eventDispatcher struct {
	registry *registry
	tr       *translator
	st       *wireState
	d        *driver // optional; nil-safe for tests that don't need driver methods
	deliver  func(agent.AgentEvent)
}

// newDispatcher constructs a dispatcher with the standard event
// registry. The registry is package-level (init once); per-driver
// state is the translator + wireState + deliver closure.
//
// The driver argument is optional: pass nil for tests that don't
// exercise handler paths that need driver methods (e.g.
// handleApprovalAsked → handleInlineApproval).
func newDispatcher(tr *translator, st *wireState, d *driver, deliver func(agent.AgentEvent)) *eventDispatcher {
	return &eventDispatcher{
		registry: standardRegistry,
		tr:       tr,
		st:       st,
		d:        d,
		deliver:  deliver,
	}
}

// dispatch routes one envelope to its registered handler. Returns
// silently on lookup miss (caller has already logged the unknown
// type).
//
// Lock discipline (matters for the race detector):
//   - translator.mu + wireState.mu acquired at entry (fixed order:
//     translator first, wireState second — prevents reentrancy cycles)
//   - handlers run with BOTH locks held; they MUST NOT re-acquire
//     either lock (would deadlock with the dispatcher)
//   - deliver is invoked AFTER the locks are released (C1 fix):
//     deliver currently uses a non-blocking select-default, so
//     this is a correctness/clarity win rather than a deadlock
//     avoidance — but if a future deliver ever blocks, the
//     lock window stays short and the runtime stays responsive
//
// P3 View authority: when view bytes are present, applyViewLocked
// runs alongside the registered handler so wireState.tools gets
// the host-pre-computed state merged in BEFORE the handler reads
// it (for tool/call and tool/result handlers that consult
// wireState.tools). For event types that don't consult View (text
// delta, turn boundaries), applyView is a no-op — its side effects
// are state-only.
//
// P4 observability: every dispatch records the wire frame and
// counts unknown types so DumpWireStats can surface them.
func (d *eventDispatcher) dispatch(env sessionEventEnvelope, view json.RawMessage) {
	d.tr.mu.Lock()
	d.st.mu.Lock()
	h, ok := d.registry.lookup(env.Type)

	// P3: apply View state under the same lock as the handler.
	// Both read/write wireState; doing it serially avoids the
	// "tool_end fires before view lands" race that would happen
	// if we unlocked between them.
	if len(view) > 0 {
		d.st.applyViewLocked(view)
	}

	var events []agent.AgentEvent
	var unknownCount uint64
	if ok {
		events = h(env, view, d.tr, d.st, d.d)
	} else {
		// Lookup miss — count it for ops triage. Goes through the
		// helper so handle_mux.go's unknown-method path and the
		// dispatcher's unknown-envelope-type path share a single
		// accounting point (any future logging / threshold alerts
		// land in one place). _Locked variant because dispatcher
		// already holds s.mu at this point.
		unknownCount = d.st.incUnknownLockedRead()
	}
	d.st.mu.Unlock()
	d.tr.mu.Unlock()

	// P4: ring buffer record is outside the lock so DumpWireStats
	// can be called from a debug command without contending with
	// the mux readPump.
	d.st.recordWireFrame("session/event", env.Type, len(view))

	if !ok {
		// Lookup miss: dsh emitted an event type we don't recognize.
		// Warn (not Debug) so production logs surface this — if
		// count is non-zero, operators know dsh added something
		// we don't handle. Crucially, do NOT flip t.active
		// (preserves F-52's "phantom Done" guard invariant from
		// the old code).
		warnLogger.Warn("dsh: dispatch unknown envelope type",
			"type", env.Type,
			"view_bytes", len(view),
			"unknown_total", unknownCount)
		return
	}
	for _, ev := range events {
		d.deliver(ev)
	}
}

// standardRegistry is the package-level event handler map. New types
// = new line here + new handler function. handlers are top-level funcs
// (no closures over per-driver state — that's what *translator /
// *wireState parameters are for).
//
// Pre-registered future types (todo/update, todo/delete) have
// handlers that return nil today. When dsh starts emitting them, the
// handler bodies get filled in — no switch default change needed.
var standardRegistry = newRegistry(map[string]eventHandler{
	"assistant/chunk":   handleAssistantChunk,
	"assistant/message": handleAssistantMessage,
	"tool/call":         handleToolCall,
	"tool/result":       handleToolResult,
	"turn/start":        handleTurnStart,
	"turn/end":          handleTurnEnd,
	"compaction/end":    handleCompactionEnd,
	"todo/write":        handleTodoWrite,
	"todo/update":       handleTodoUpdate, // P3+: dsh will emit; handler is no-op now
	"todo/delete":       handleTodoDelete, // same
	"approval/asked":    handleApprovalAsked,
})

// ─── handlers ────────────────────────────────────────────────────
//
// All handlers run with translator.mu AND wireState.mu held. They
// MUST NOT acquire locks or call d.deliver (the dispatcher delivers
// after the lock release). They MUST NOT call each other.
//
// Returning nil means "no event" (e.g. turn/start just clears state).
// Returning []agent.AgentEvent{...} delivers each event through the
// dispatcher's deliver callback after both locks are released.

// handleAssistantChunk buffers streaming text into translator.textBuf.
// Per F-52: chunks don't emit AgentEvents directly — flush happens
// at tool-boundary (tool/call) or turn-end (assistant/message).
func handleAssistantChunk(env sessionEventEnvelope, view json.RawMessage, tr *translator, st *wireState, d *driver) []agent.AgentEvent {
	var data assistantChunkData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		dLog("dsh: handler data envelope decode: %v", err)
		return nil
	}
	tr.active = true
	idx := data.Chunk.Index
	if idx < 0 || idx >= maxTextStreams {
		dLog("dsh: assistant/chunk contentIndex out of range",
			"content_index", idx,
			"max", maxTextStreams)
		return nil
	}
	b, ok := tr.textBuf[idx]
	if !ok {
		b = &strings.Builder{}
		tr.textBuf[idx] = b
	}
	b.Grow(256)
	b.WriteString(data.Chunk.Text)
	return nil
}

// handleAssistantMessage captures the last full text block from an
// assistant message into pendingText for tool-boundary flush.
// F-52 invariant: never emit EventAgentText here — flush is the
// tool/call path's job.
func handleAssistantMessage(env sessionEventEnvelope, view json.RawMessage, tr *translator, st *wireState, d *driver) []agent.AgentEvent {
	var data assistantMessageData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		dLog("dsh: handler data envelope decode: %v", err)
		return nil
	}
	tr.active = true
	tr.pendingText = pickText(data.Message.Content)
	tr.lastText = tr.pendingText
	return nil
}

// handleToolCall flushes pendingText at the tool boundary (F-52) and
// emits EventAgentToolStart. Name + Args are stashed into
// translator.pendingTools for handleToolResult to backfill.
func handleToolCall(env sessionEventEnvelope, view json.RawMessage, tr *translator, st *wireState, d *driver) []agent.AgentEvent {
	var data toolCallData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		dLog("dsh: handler data envelope decode: %v", err)
		return nil
	}
	tr.active = true

	events := make([]agent.AgentEvent, 0, 2)
	if tr.pendingText != "" {
		text := tr.pendingText
		events = append(events, agent.AgentEvent{
			Kind: agent.EventAgentText,
			Text: text,
		})
		tr.textDelivered = true
		tr.pendingText = ""
	}

	tr.pendingTools[data.CallID] = pendingTool{
		Name: data.Name,
		Args: data.Arguments,
	}
	events = append(events, agent.AgentEvent{
		Kind: agent.EventAgentToolStart,
		ToolStart: &agent.AgentToolStartEvent{
			ID:   data.CallID,
			Name: data.Name,
			Args: data.Arguments,
		},
	})
	return events
}

// handleToolResult reads the matching pendingTool (Name+Args), emits
// EventAgentToolEnd. Orphan results (no matching call) get empty Name
// per pre-existing behavior — preserves diagnostic visibility.
func handleToolResult(env sessionEventEnvelope, view json.RawMessage, tr *translator, st *wireState, d *driver) []agent.AgentEvent {
	var data toolResultData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		dLog("dsh: handleToolResult decode: %v", err)
		return nil
	}
	tr.active = true

	block := data.Message.pickToolResultBlock()
	if block == nil {
		// Defensive: malformed user envelope (no tool-result block).
		// Emitting EventAgentToolEnd here would carry a stale
		// empty-key entry from tr.pendingTools[""] and confuse
		// the runtime. Drop silently rather than misattribute.
		return nil
	}
	toolCallID := block.ToolCallID
	if toolCallID == "" {
		// Same as above — empty CallID would match a stale "" entry
		// in tr.pendingTools. Drop and let the runtime continue
		// (a follow-up tool/result with a real CallID will surface).
		return nil
	}
	isError := block.IsError
	var sb strings.Builder
	for _, inner := range block.Content {
		if inner.Type == "text" && inner.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(inner.Text)
		}
	}
	resultText := sb.String()
	pt, hasPending := tr.pendingTools[toolCallID]
	if hasPending {
		delete(tr.pendingTools, toolCallID)
	}
	name := pt.Name
	args := pt.Args
	if !hasPending {
		// Orphan result (no matching call). Use fallback name so
		// the renderer doesn't print "tool" — better than silently
		// dropping args.
		name = ""
	}
	ev := agent.AgentEvent{
		Kind: agent.EventAgentToolEnd,
		ToolEnd: &agent.AgentToolEndEvent{
			ID:     toolCallID,
			Name:   name,
			Args:   args,
			Output: resultText,
		},
	}
	if isError {
		ev.Err = fmt.Errorf("dsh: tool %s failed: %s", name, resultText)
	}
	return []agent.AgentEvent{ev}
}

// handleTurnStart clears per-turn buffering AND pendingTools.
// pendingTools is cleared because dsh recycles numeric toolCallIDs
// across turns (a stale entry from turn N would corrupt turn N+1's
// matching tool/result).
func handleTurnStart(env sessionEventEnvelope, view json.RawMessage, tr *translator, st *wireState, d *driver) []agent.AgentEvent {
	var data turnStartData
	_ = json.Unmarshal(env.Data, &data)
	tr.active = true

	clear(tr.textBuf)
	tr.thinkBuf.Reset()
	tr.pendingText = ""
	tr.lastText = ""
	tr.lastUsage = nil
	tr.textDelivered = false
	tr.pendingTools = map[string]pendingTool{}

	// wireState: drop the per-turn inflight set. If a tool View
	// arrived with Status="running" but the host never reported
	// completion (crash / disconnect / dropped frame), its
	// CallID would otherwise stay in inflight forever, growing
	// across many turns. A fresh turn starts with an empty
	// inflight set; tools carried over are reconciled on the
	// next View. tasks are session-scoped, not turn-scoped —
	// they survive via the next todo/write or projection.
	st.inflight = nil
	return nil
}

// handleTurnEnd emits EventAgentResult (if active) + EventAgentDone.
// F-52 guards:
//   - active: empty turn → no phantom "Done." card
//   - textDelivered + pendingText: don't re-emit already-shown prose
//   - empty text fallback: "Done." placeholder if no content at all
func handleTurnEnd(env sessionEventEnvelope, view json.RawMessage, tr *translator, st *wireState, d *driver) []agent.AgentEvent {
	var data turnEndData
	_ = json.Unmarshal(env.Data, &data)

	events := make([]agent.AgentEvent, 0, 2)
	if tr.active {
		text := ""
		if tr.pendingText != "" {
			text = tr.pendingText
		} else if !tr.textDelivered && tr.lastText != "" {
			text = tr.lastText
		}
		if text == "" {
			text = "Done."
		}
		usage := tr.lastUsage
		events = append(events, agent.AgentEvent{
			Kind: agent.EventAgentResult,
			Result: &agent.AgentResultEvent{
				Text:       text,
				DurationMs: 0,
				Subtype:    stopReasonToSubtype(data.StopReason),
				Usage:      usage,
			},
		})
	}
	events = append(events, agent.AgentEvent{
		Kind: agent.EventAgentDone,
		Done: &agent.AgentDoneEvent{
			ExitCode: 0,
			Reason:   "settled",
			Usage:    tr.lastUsage,
		},
	})

	// Reset per-turn state.
	tr.active = false
	tr.textDelivered = false
	tr.pendingText = ""
	tr.lastText = ""
	tr.lastUsage = nil
	return events
}

// handleCompactionEnd is debug-log only today (no AgentEvent kind
// exists for it). Pre-existing behavior preserved.
func handleCompactionEnd(env sessionEventEnvelope, view json.RawMessage, tr *translator, st *wireState, d *driver) []agent.AgentEvent {
	var data compactionEndData
	_ = json.Unmarshal(env.Data, &data)
	if !data.Aborted {
		dLog("dsh: compaction completed", "reason", data.Reason)
	}
	return nil
}

// handleTodoWrite delegates to wireState.applyEventLocked, which keeps
// ID-indexed tasks map and emits EventAgentTaskCreate. The dispatcher
// already holds st.mu at this point, so we use the _Locked variant
// to avoid re-acquiring (deadlock).
func handleTodoWrite(env sessionEventEnvelope, view json.RawMessage, tr *translator, st *wireState, d *driver) []agent.AgentEvent {
	tr.active = true
	_ = view // reserved for P3 View authority
	return st.applyEventLocked(env)
}

// handleTodoUpdate is pre-registered for forward compatibility. When
// dsh starts emitting `todo/update` (single-item status delta), this
// handler body gets filled in. Today it returns nil — bridge is
// already receiving these events without panic (registry hit, not
// unknown-type dLog).
func handleTodoUpdate(env sessionEventEnvelope, view json.RawMessage, tr *translator, st *wireState, d *driver) []agent.AgentEvent {
	// Set tr.active defensively even though handleTurnStart has
	// typically already set it for this turn: a future dsh version
	// may ship todo/update before turn/start (session.fork
	// resume edge case), and the F-52 phantom-Done guard depends
	// on tr.active being true at turn/end. Setting it here is
	// safe — the only way it gets cleared is at turn/start or
	// turn/end.
	tr.active = true
	// P3+: decode single-item update, find existing key in st.tasks,
	// update status, emit EventAgentTaskUpdate.
	return nil
}

// handleTodoDelete is the pre-registered counterpart to handleTodoUpdate.
// P3+: remove key from st.tasks, emit EventAgentTaskUpdate with the
// remaining snapshot.
func handleTodoDelete(env sessionEventEnvelope, view json.RawMessage, tr *translator, st *wireState, d *driver) []agent.AgentEvent {
	tr.active = true // see handleTodoUpdate comment
	return nil
}

// handleApprovalAsked is the session/event approval/asked entry.
// Previously a no-op (returned nil) because the mux-frame `case
// "approval/asked"` in handle_mux.go drove the actual approval
// flow via driver.handleInlineApproval. But dsh ships approval/
// asked as a session/event envelope in some versions (per
// permissions.go's own comment on handleInlineApproval: "the
// session/event approval/asked entry"). The old "registry hit +
// return nil" silently dropped those approvals.
//
// This handler now mirrors the mux-frame path: decodes the same
// approval/asked payload (approvalAskedData) and routes through
// driver.handleInlineApproval. If dispatcher.d is nil (test-only),
// the handler returns nil silently rather than panicking.
func handleApprovalAsked(env sessionEventEnvelope, view json.RawMessage, tr *translator, st *wireState, d *driver) []agent.AgentEvent {
	if d == nil {
		return nil
	}
	var aa approvalAskedData
	if err := json.Unmarshal(env.Data, &aa); err != nil {
		dLog("dsh: handleApprovalAsked decode: %v", err)
		return nil
	}
	d.handleInlineApproval(aa.ToolCallID, aa.ToolName, aa.Action, aa.Options)
	return nil
}