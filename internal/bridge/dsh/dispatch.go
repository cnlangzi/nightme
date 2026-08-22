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
// like handleApprovalAsked). Returns the
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
// to call driver methods), and the
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
// handleApprovalAsked).
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
	"todo/update":       handleTodoUpdate,    // P3+: dsh will emit; handler is no-op now
	"todo/delete":       handleTodoDelete,    // same
	"approval/asked":    handleApprovalAsked, // session/event echo; mux approval/requested is the respondable gate

	// F-dsh-shared-host §5: 9 new event types discovered during
	// the 2026-08-16 mux-demux probe against dsh 0.1.0-rc.6.
	// Most are debug-only — they describe internal dsh state that
	// the runtime doesn't surface but operators want to see in
	// /diagnose output. Adding them here (rather than relying on
	// the "unknown method" warn branch) means a future dsh upgrade
	// that adds new types no longer flips the count.
	"permission/preset":         handleDebugOnly,
	"sandbox/mode":              handleDebugOnly,
	"approval/policy":           handleDebugOnly,
	"agent/inbox/spliced":       handleDebugOnly,       // queue spliced by server
	"user/message":              handleUserMessageEcho, // ⚠ do NOT emit; see handler doc
	"request/header":            handleDebugOnly,
	"request/context":           handleDebugOnly,
	"step/start":                handleStepBoundary,
	"step/end":                  handleStepBoundary,
	"session/title":             handleSessionTitle,
	"session/title-llm-request": handleDebugOnly,
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
// handleAssistantChunk ingests one assistant/chunk envelope and folds
// its delta into the right per-block accumulator. Behavior mirrors
// the dashboard's PartialAccumulator (dsh-client-runtime/.../partial.d.ts):
// each StreamChunk's Type discriminator routes the delta to textBuf
// (text-delta), reasoningBuf (reasoning-delta), or is dropped
// (block-start / block-end / tool-call-delta / usage / finish).
//
// F-DSH-DASHBOARD-PARITY (2026-08-16): reasoning-delta MUST NOT land
// in textBuf — pre-fix it did, and the model's thinking leaked into
// the reply stream. block-end{type:"reasoning"} is the only emit
// point for thinking text (whole-block, not per-delta, to match the
// dashboard's collapsed-thinking view).
// handleAssistantChunk ingests one assistant/chunk envelope and folds
// its delta into the right per-block accumulator. Behavior mirrors
// the dashboard's PartialAccumulator (dsh-client-runtime/.../partial.d.ts):
// each StreamChunk's Type discriminator routes the delta to textBuf
// (text-delta), reasoningBuf (reasoning-delta), or is dropped
// (block-start / block-end / tool-call-delta / usage / finish).
//
// F-DSH-DASHBOARD-PARITY (2026-08-16): reasoning-delta MUST NOT land
// in textBuf — pre-fix it did, and the model's thinking leaked into
// the reply stream. block-end{type:"reasoning"} is the only emit
// point for thinking text (whole-block, not per-delta, to match the
// dashboard's collapsed-thinking view).
//
// F-52 invariant: tr.active is set PER-TYPE (not at function top),
// so unknown / future chunk types (which fall through to the
// post-switch dLog) don't flip active=true and synthesize a phantom
// Done card. See translate.go's "Set PER-TYPE" comment for context.
func handleAssistantChunk(env sessionEventEnvelope, view json.RawMessage, tr *translator, st *wireState, d *driver) []agent.AgentEvent {
	var data assistantChunkData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		dLog("dsh: handler data envelope decode: %v", err)
		return nil
	}

	switch data.Chunk.Type {
	case "text-delta":
		// Same accumulation path as pre-fix; flushed at the next
		// assistant/message, tool/call, or turn/end boundary per
		// F-52 granularity contract. Max-contentIndex guard
		// preserved verbatim — mirrors the pre-fix behaviour and
		// prevents a malicious dsh from OOM-ing the bridge via a
		// huge Index field.
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

	case "reasoning-delta":
		// Dashboard parity: reasoning goes to its OWN buffer, never
		// to textBuf. We do not emit per-delta (would split the
		// folded view into one OutThinking per token); instead we
		// wait for block-end{type:"reasoning"} which carries the
		// whole assembled reasoning block.
		// active=true: a reasoning-only step should still produce
		// EventAgentDone at turn/end (so receipt finalises).
		tr.active = true
		idx := data.Chunk.Index
		if idx < 0 || idx >= maxTextStreams {
			dLog("dsh: assistant/chunk reasoning contentIndex out of range",
				"content_index", idx,
				"max", maxTextStreams)
			return nil
		}
		b, ok := tr.reasoningBuf[idx]
		if !ok {
			b = &strings.Builder{}
			tr.reasoningBuf[idx] = b
		}
		b.WriteString(data.Chunk.Text)
		return nil

	case "block-end":
		// Block-end is the authoritative carrier of a reasoning
		// block's full text. Pre-fix this branch didn't exist; the
		// whole reasoning-delta → block-end sequence was lost.
		// We emit "[思考] ..." with the local thinkingPrefix
		// constant (mirrors pi / codex) so the downstream
		// gateway/outbound/translate.go routes the resulting
		// OutboundMessage to OutThinking rather than OutReply.
		tr.active = true
		if data.Chunk.Block == nil {
			return nil
		}
		if data.Chunk.Block.Type == "reasoning" && data.Chunk.Block.Text != "" {
			idx := data.Chunk.Index
			if tr.reasoningEmitted[idx] {
				// Already emitted via assistant/message path —
				// suppress duplicate.
				return nil
			}
			tr.reasoningEmitted[idx] = true
			// Clear the per-block accumulator once consumed so
			// memory doesn't grow over a long turn.
			delete(tr.reasoningBuf, idx)
			return []agent.AgentEvent{{
				Kind: agent.EventAgentText,
				Text: thinkingPrefix + data.Chunk.Block.Text,
			}}
		}
		// block-end for text: text-delta already accumulated into
		// textBuf; boundary flush at assistant/message / tool/call /
		// turn/end handles it. block-end for tool-call: the
		// independent tool/call path handles it. No-op here.
		return nil

	case "block-start", "tool-call-delta", "usage", "finish":
		// Dashboard feeds these to PartialAccumulator for projection
		// state only — they don't render. Same posture here.
		// usage arrives on assistant/message as data.Usage, which
		// we already handle. finish / stopReason flows via turn/end.
		// tr.active NOT set: none of these types are observed
		// content from the model — a turn composed solely of
		// usage chunks should not synthesize EventAgentResult.
		return nil
	}

	// Unknown / future chunk types — debug-log only, no event, and
	// critically NO tr.active flip (preserves the F-52 phantom-Done
	// guard invariant).
	dLog("dsh: assistant/chunk unknown type: %q", data.Chunk.Type)
	return nil
}

// handleAssistantMessage captures the last full text block from an
// assistant message into pendingText for tool-boundary flush.
// F-52 invariant: never emit EventAgentText here — flush is the
// tool/call path's job.
// handleAssistantMessage ingests one assistant/message envelope and
// folds its content blocks into AgentEvents. Per-block-type routing
// mirrors the dashboard's toAssistantBlocks (dsh-client-runtime/
// .../conversation.d.ts): text blocks → reply, reasoning blocks
// → thinking (via local thinkingPrefix → gateway OutThinking),
// tool-call blocks → suppressed (handled by the independent
// tool/call path to avoid double-emit).
//
// F-DSH-DASHBOARD-PARITY (2026-08-16): pre-fix this used
// pickText(content) which filtered content to type=="text" only,
// dropping reasoning entirely. Splitting the switch on b.Type is
// the fix.
// handleAssistantMessage ingests one assistant/message envelope and
// folds its content blocks into AgentEvents. Per-block-type routing
// mirrors the dashboard's toAssistantBlocks (dsh-client-runtime/
// .../conversation.d.ts): text blocks → reply; reasoning blocks →
// suppressed (already emitted via block-end{type:"reasoning"} in
// handleAssistantChunk); tool-call blocks → suppressed (handled by
// the independent tool/call path to avoid double-emit).
//
// F-DSH-DASHBOARD-PARITY (2026-08-16): pre-fix this used
// pickText(content) which filtered content to type=="text" only,
// dropping reasoning entirely. Splitting the switch on b.Type is
// the fix.
//
// Reasoning-emission invariant: reasoning content blocks are NOT
// emitted here. block-end{type:"reasoning"} in handleAssistantChunk
// is the single source of truth for thinking text. The content
// block in assistant/message is the same data shipped again for
// durable-log purposes — emitting from both paths would double
// the user's thinking card. This is why reasoningEmitted in the
// translator is keyed by blockIndex (only block-end knows it).
// handleAssistantMessage ingests one assistant/message envelope and
// folds its content blocks into AgentEvents. Per-block-type routing
// mirrors the dashboard's toAssistantBlocks (dsh-client-runtime/
// .../conversation.d.ts): text blocks → reply; reasoning blocks →
// suppressed (already emitted via block-end{type:"reasoning"} in
// handleAssistantChunk); tool-call blocks → suppressed (handled by
// the independent tool/call path to avoid double-emit).
//
// F-DSH-DASHBOARD-PARITY (2026-08-16): pre-fix this used
// pickText(content) which filtered content to type=="text" only,
// dropping reasoning entirely. Splitting the switch on b.Type is
// the fix.
//
// Reasoning-emission invariant: reasoning content blocks are NOT
// emitted here. block-end{type:"reasoning"} in handleAssistantChunk
// is the single source of truth for thinking text. The content
// block in assistant/message is the same data shipped again for
// durable-log purposes — emitting from both paths would double
// the user's thinking card. This is why reasoningEmitted in the
// translator is keyed by blockIndex (only block-end knows it).
//
// F-52 state-machine contract (preserved from pre-fix): after
// emitting text blocks, stash the joined text on tr.pendingText
// AND tr.lastText so handleTurnEnd's Result.Text fallback carries
// the reply (not "Done."), and mark textDelivered=true so
// handleToolCall won't re-flush the same text. Pre-fix this was
// the only emit path; my refactor moved to per-block emit but
// must keep the state-machine side effects or the result card
// degenerates.
func handleAssistantMessage(env sessionEventEnvelope, view json.RawMessage, tr *translator, st *wireState, d *driver) []agent.AgentEvent {
	var data assistantMessageData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		dLog("dsh: handler data envelope decode: %v", err)
		return nil
	}
	tr.active = true

	// Per-turn usage block → flows into EventAgentResult.Usage at
	// turn/end (which the runtime's receipt-footer Line 2 already
	// renders via gateway/outbound/translate.go → feishu
	// internal/statusbar/statusbar.go). Pre-fix this was dropped on the floor.
	if u := usageToAgent(data.Usage); u != nil {
		tr.lastUsage = u
	}

	var evs []agent.AgentEvent
	var joined strings.Builder
	for _, b := range data.Message.Content {
		switch b.Type {
		case "text":
			// Dashboard parity: finalized text becomes one reply
			// block. We emit per-block (not via pendingText) so
			// the text lands on OutReply even when no tool call
			// follows in the turn. Pre-fix relied on tool/call or
			// turn/end boundary to flush, which delayed the reply
			// (and dropped it entirely on tool-less turns).
			if b.Text != "" {
				evs = append(evs, agent.AgentEvent{
					Kind: agent.EventAgentText,
					Text: b.Text,
				})
				// Mirror into the F-52 state machine so
				// handleTurnEnd's Result.Text fallback carries
				// the reply (not "Done."). Joined text from all
				// text blocks in this message.
				if joined.Len() > 0 {
					joined.WriteByte('\n')
				}
				joined.WriteString(b.Text)
			}
		case "reasoning":
			// Suppressed — see reasoning-emission invariant above.
			// block-end already covered this block; we drop the
			// durable-log duplicate. If dsh ever omits the
			// block-end frame (regression), reasoning leaks will
			// be visible via the unknownCount in DumpWireStats.
		case "tool-call":
			// Suppressed: handled by the independent tool/call
			// path (handleToolCall) which is the canonical
			// source of EventAgentToolStart. Avoid double-emit.
		case "image":
			// dsh assistant-side image output is baseline-only;
			// not produced in practice today. No-op.
		case "tool-result":
			// tool-result shouldn't appear in an assistant
			// message's content[] — it has its own event
			// (tool/result) handled by handleToolResult.
			// Defensive no-op.
		default:
			// Unknown block type — dashboard treats it as
			// AssistantBlock{kind:"other"} and renders generically.
			// We no-op rather than guess a render. dLog the type
			// so ops can see if dsh adds new block types we
			// haven't taught the bridge.
			dLog("dsh: assistant/message unknown content block type: %q", b.Type)
		}
	}

	// F-52 sync: stash joined text + mark delivered so handleToolCall
	// won't re-flush and handleTurnEnd's fallback can read it.
	// textDelivered=true matters: handleToolCall flushes
	// tr.pendingText as EventAgentText unconditionally (it can't
	// tell whether the flush is a duplicate).
	if joined.Len() > 0 {
		joinedText := joined.String()
		tr.pendingText = joinedText
		tr.lastText = joinedText
		tr.textDelivered = true
	}
	return evs
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
	// Flush pendingText only if it hasn't been delivered yet. Pre-fix
	// the only emit path was this flush, so textDelivered was always
	// false here; after F-DSH-DASHBOARD-PARITY (2026-08-16),
	// handleAssistantMessage emits per-block and sets
	// textDelivered=true — without this guard, every tool boundary
	// after a finalized message would duplicate the text.
	if tr.pendingText != "" && !tr.textDelivered {
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
	clear(tr.reasoningBuf)
	clear(tr.reasoningEmitted)
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
	// next View. tasks (todo/write / todos projection) are the
	// dashboard TodoPanel snapshot; a later todo/write or
	// session/projection{key:"todos"} reconciles them. Dashboard
	// also clears the panel on turn/start when no later write
	// stands — we leave the last snapshot until that write so
	// Feishu does not flicker an empty checklist mid-turn.
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
			if data.StopReason == "abort" {
				text = "Stopped."
			} else {
				text = "Done."
			}
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
	doneReason := "settled"
	if data.StopReason == "abort" {
		doneReason = "interrupted"
	}
	events = append(events, agent.AgentEvent{
		Kind: agent.EventAgentDone,
		Done: &agent.AgentDoneEvent{
			ExitCode: 0,
			Reason:   doneReason,
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
	// may ship todo/update before turn/start (session.fork is
	// no longer used; this comment kept for context on why
	// we defensively set active=true here)
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

// handleApprovalAsked is the session/event approval/asked echo.
// The dashboard ApprovalPanel and /api/respond are keyed on mux
// approval/requested (frame rpcId). Emitting a second
// EventAgentPermission here would duplicate the Feishu card.
func handleApprovalAsked(env sessionEventEnvelope, view json.RawMessage, tr *translator, st *wireState, d *driver) []agent.AgentEvent {
	// session/event approval/asked is an audit/echo of the tool
	// call. The dashboard ApprovalPanel and /api/respond are keyed
	// on mux approval/requested (frame rpcId). Emitting a second
	// EventAgentPermission here duplicated the Feishu card and
	// registered a fake "evt-" key that cannot POST /api/respond.
	dLog("dsh: session/event approval/asked (echo; mux approval/requested is the gate)")
	return nil
}

// ─── F-dsh-shared-host §5 new event handlers ────────────────────────
//
// Most of these types dsh emits as part of its own bookkeeping and
// are interesting to /diagnose but should NOT bubble up to the
// runtime's AgentEvent stream. The default handler
// `handleDebugOnly` records the wire frame + decodes the envelope
// for log inspection, then returns nil (no AgentEvent to deliver).
//
// `user/message` is special: it's the server's echo of the user's
// message (dsh-api.md §3.4.2), distinct from request/header which
// marks the actual LLM turn boundary. We must NOT emit this to the
// runtime — the runtime's readpump already has the user's message
// from the inbound path; re-emitting would double the bubble in
// the feishu chat. See claudecode bridge §1 `--replay-user-messages`
// don't for the same reason.

// handleDebugOnly records the envelope for /diagnose + ops triage
// but emits no AgentEvent. The wire frame is counted in
// wireState's recordWireFrame (called by dispatcher.dispatch
// before the handler runs), so observability is covered.
func handleDebugOnly(env sessionEventEnvelope, view json.RawMessage, tr *translator, st *wireState, d *driver) []agent.AgentEvent {
	dLog("dsh: %s", env.Type)
	return nil
}

// thinkingPrefix is the sentinel that gets prepended to every
// thinking block before emit. The downstream gateway translate
// (internal/gateway/outbound/translate.go) recognises this prefix
// and routes the resulting OutboundMessage to OutThinking rather
// than OutReply. Must stay in sync with the gateway constant.
// Mirrors the same pattern in pi/translate.go and codex/translate.go.
const thinkingPrefix = "[思考] "

// handleUserMessageEcho is the same as handleDebugOnly but with a
// stronger comment to flag the "do not emit" invariant. If a
// future change needs to expose this event (e.g. for an IM channel
// that does want the echo), the suppression lives here.
func handleUserMessageEcho(env sessionEventEnvelope, view json.RawMessage, tr *translator, st *wireState, d *driver) []agent.AgentEvent {
	dLog("dsh: user/message echo (suppressed; runtime has it via inbound)")
	return nil
}

// handleStepBoundary is a no-op for AgentEvent emission.
//
// Dashboard alignment (packages/client/ui-conversation TodoDock):
// the TODO LIST strip reads the host `todos` projection, which is
// folded from `todo/write` {todos:[{content,status}]} and cleared
// on a later `turn/start`. step/start and step/end are inference
// cycle bounds ({turn, step} only — one model call plus its tools)
// used for sessionStats (TTFT / tok/s), not the plan panel.
// Mapping them onto OutTask* synthesised "Step N" rows was wrong.
func handleStepBoundary(env sessionEventEnvelope, view json.RawMessage, tr *translator, st *wireState, d *driver) []agent.AgentEvent {
	return nil
}

// handleSessionTitle emits an EventAgentTitle (or equivalent) so
// the runtime can surface the auto-generated session title in the
// chat header / receipt card. dsh 0.1.0-rc.6 generates titles
// during the first turn via session/title-llm-request, then commits
// them via session/title once the model finishes.
func handleSessionTitle(env sessionEventEnvelope, view json.RawMessage, tr *translator, st *wireState, d *driver) []agent.AgentEvent {
	var p struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(env.Data, &p); err != nil {
		dLog("dsh: session/title decode: %v", err)
		return nil
	}
	if p.Title == "" {
		return nil
	}
	// Dispatcher already holds st.mu (see eventDispatcher.dispatch).
	// Re-locking here deadlocks the mux pump and the history
	// backfill — both call dispatch — which is the
	// "events=14 then silence / Feishu card never patches"
	// failure from 2026-08-16 (test-resume-01 / test-new-01).
	if st.title == "" {
		st.title = p.Title
	}
	dLog("dsh: title=%q", p.Title)
	return nil
}
