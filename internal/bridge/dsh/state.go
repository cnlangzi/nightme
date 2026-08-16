// state.go — dsh bridge 内部真值镜像 (F-DSH-CHAT-001 §4.3)
//
// wireState 是 dsh 包内的私有状态结构,由 applyEvent (raw event 流)
// 和 applyProjection (host 派生的 projection 流) 两路喂数据,产出
// []agent.AgentEvent 序列交给 dispatcher / handle_mux 投出。
//
// 关键设计:
//   - 单源真值:wireState.tasks 按 ID 索引,多次 todo/write 自动覆盖
//     而不是堆叠(对照原 translate.go 每次都发 EventAgentTaskCreate
//     但 ID 为空,runtime dedup 失效)。
//   - graceful 降级:TodoItem 真没 ID 时,fallback 用 Content 作 key,
//     保证 wire 演化 / 字段缺失都不 panic(B3 缓解)。
//   - 不动 agent / channel 包:wireState 完全 dsh 私有,产出仍是
//     现有 AgentEvent kind(EventAgentTaskCreate / Update)。
//   - 锁:wireState.mu 是独立锁,不与 translator.mu 嵌套;
//     调用方(translate.go handlers / handle_mux.go applyProjection
//     入口)负责按"先 wireState 后 translator"或相反的固定顺序
//     避免死锁。Phase 1+2 由 dispatcher 在入口处 acquire 全部锁,
//     handler 内不再加锁(见 dispatch.go)。

package dsh

import (
	"encoding/json"
	"sync"

	"github.com/cnlangzi/nightme/internal/agent"
)

// wireState is the bridge-internal normalized truth mirror of a dsh
// session. It is private to the dsh package (not exported) and is
// consumed by handlers / dispatcher / handle_mux exclusively.
//
// Fields:
//   - tasks: by stable ID (preferred) or Content fallback. Source of
//     truth for To-dos panel rendering.
//   - tools: by CallID. P3: populated from ToolEventView (host-
//     computed). When View is absent, falls back to raw event inference.
//   - inflight: tool_callIDs seen without matching result. P3: lets
//     applyView flag orphan results when the host hasn't reported back.
//   - title: session header title (populated via projection/title
//     handle in wireState.applyProjectionLocked).
//   - wireRing: last 64 raw frames, for debug dump (P4).
//   - unknownCount: cumulative count of unrecognized envelope types,
//     unknown mux methods, AND todo-drift frames (F-DSH-TODO-FIX)
//     — see field comment for details. P4 observability.
//
// Concurrency:
//   - mu guards all fields. Read/write should be brief.
//   - applyEvent / applyProjection / applyView take the lock internally
//     for the full read-modify-write cycle, returning the AgentEvent
//     slice that the caller (dispatcher) will deliver.
//   - ring buffer is also guarded by mu (atomic append-only would be
//     faster but adds complexity for a debug-only path).
type wireState struct {
	mu sync.Mutex

	title    string
	tasks    map[string]todoItem // by ID, Content fallback if ID empty
	tools    map[string]toolItem // by CallID (P3 populates from View)
	inflight map[string]bool     // tool_callIDs seen without matching result

	// wireRing is a fixed-capacity ring of recent raw mux frames
	// (envelope type + raw bytes). Used by DumpWire for ops triage
	// when "bridge isn't producing expected events" — operators
	// can inspect what wire actually arrived vs what bridge
	// translated. P4 observability.
	wireRing *wireRingBuffer

	// unknownCount counts unrecognized events. Three sources bump it:
	//   1. dispatcher registry lookup miss (handler not registered
	//      for env.Type)
	//   2. handleMuxFrame unknown mux method
	//   3. todo drift: a todo/write or session/projection(todo) frame
	//      whose items[] zero-fill (ID="" AND Content=""), which
	//      usually means dsh's real wire field names don't match
	//      the json tags on protocol.go's todoItem. Each affected
	//      frame bumps the counter once, NOT once per item (so the
	//      number is comparable to "how many frames had drift",
	//      not "how many items were lost").
	//
	// Operators read it via DumpWireStats to triage "bridge isn't
	// picking up dsh's todos" — distinct from the prior behaviour
	// where the failure was completely silent (dLog at Debug).
	// P4 + F-DSH-TODO-FIX.
	unknownCount uint64
}

// wireRingBuffer is a fixed-capacity FIFO of wire frames. When
// capacity is reached, oldest is overwritten. Not thread-safe;
// guarded by wireState.mu.
type wireRingBuffer struct {
	cap   int
	buf   []wireFrame
	head  int  // next write position
	count int  // total frames pushed (capped at cap for modulo math)
}

type wireFrame struct {
	Method       string // mux frame method ("session/event", "session/projection", ...)
	EnvelopeType string // envelope Type; only populated when Method == "session/event"
	Bytes        int    // raw payload size, for ops triage
}

// newWireRingBuffer constructs a ring buffer with the given capacity.
// 64 frames covers ~64 tool-call events which is enough for a typical
// multi-tool turn; bump up if production triage needs deeper history.
func newWireRingBuffer(cap int) *wireRingBuffer {
	return &wireRingBuffer{cap: cap, buf: make([]wireFrame, cap)}
}

func (r *wireRingBuffer) push(f wireFrame) {
	r.buf[r.head] = f
	r.head = (r.head + 1) % r.cap
	if r.count < r.cap {
		r.count++
	}
}

// snapshot returns frames in chronological order (oldest first).
// Returned slice is independent of the ring's internal storage.
func (r *wireRingBuffer) snapshot() []wireFrame {
	if r.count == 0 {
		return nil
	}
	out := make([]wireFrame, 0, r.count)
	start := (r.head - r.count + r.cap) % r.cap
	for i := range r.count {
		out = append(out, r.buf[(start+i)%r.cap])
	}
	return out
}

// toolItem is the bridge-internal record of one tool invocation's
// running state. Populated by View (P3); Phase 1+2 keeps this as a
// stub so the data path exists and P3 only needs to fill it.
type toolItem struct {
	CallID  string
	Name    string
	Status  string // "running" | "completed" | "failed"
	Output  string
	UpdatedAt int64
}

// newWireState returns an initialized wireState. Safe to call from
// newDriver; the returned state is empty and grows lazily on the
// first applyEvent / applyProjection.
//
// Lazy allocation: tasks is allocated up-front (always populated
// by todo/write); tools and inflight are reserved for P3 View
// authority and lazily-grown on first access to avoid paying for
// unused maps in the common (no-tool-state) case.
//
// wireRingBuffer is allocated up-front (P4 observability) — its
// 64-frame capacity is a small constant memory cost.
func newWireState() *wireState {
	return &wireState{
		tasks:    map[string]todoItem{},
		wireRing: newWireRingBuffer(64),
	}
}

// applyEvent ingests one session/event envelope (with optional View)
// and returns the AgentEvent sequence the dispatcher should deliver.
//
// Acquires s.mu. Use this from tests and direct callers (e.g.
// handle_mux.go alternative paths). From inside eventDispatcher,
// the dispatcher already holds the lock — call applyEventLocked
// instead to avoid deadlock.
//
// Phase 1+2 only handles `todo/write` here; other event types stay
// in translator (textBuf / pendingTools / F-52 state machine). The
// dispatcher routes by Type and only todo/write reaches applyEvent
// in this phase — other types hit their handler functions which may
// still mutate translator state directly.
//
// view parameter: reserved for P3 View authority. Today every
// handler ignores it — keep the parameter for forward compat but
// expect unused-param vet warnings until P3 wires it in.
func (s *wireState) applyEvent(env sessionEventEnvelope, view json.RawMessage) []agent.AgentEvent {
	_ = view // reserved for P3
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyEventLocked(env)
}

// applyEventLocked is the lock-free variant of applyEvent. The
// caller MUST hold s.mu. eventDispatcher.dispatch uses this version
// after acquiring both translator.mu and wireState.mu at its entry.
func (s *wireState) applyEventLocked(env sessionEventEnvelope) []agent.AgentEvent {
	switch env.Type {
	case "todo/write":
		return s.applyTodoWriteLocked(env)
	default:
		// Other types are handled by registered handlers in dispatch.go
		// which may use translator state directly. applyEventLocked
		// returns nil here because those handlers already produced
		// events.
		return nil
	}
}

// applyTodoWriteLocked assumes the caller holds s.mu.
//
// Decodes a todo/write envelope and updates s.tasks keyed by ID
// (or Content fallback). Returns a single EventAgentTaskCreate
// carrying the full current snapshot.
//
// D1 fix: prefer wire ID; fallback to Content for B3 graceful
// handling when dsh's TodoItem lacks ID. Either way the tasks
// map key is stable, so runtime-side dedup / merge works correctly
// across multiple todo/write events.
//
// F-DSH-TODO-FIX (2026-08-16): wire data arrived but every item
// was zero-valued and got skipped — root cause was dsh wire
// using field names that didn't match the struct tags (the
// WIRE-PROBE-REQUIRED warning on todoItem in protocol.go was
// never validated against a real wire). The skip path used to
// dLog at Debug (invisible in production) and still emit
// EventAgentTaskCreate{Items:[]}, which Feishu renders as
// "clear the checklist". Now the path logs at Warn AND bumps
// wireState.unknownCount so `nightme debug dsh dump-wire` shows
// the failure mode to ops.
func (s *wireState) applyTodoWriteLocked(env sessionEventEnvelope) []agent.AgentEvent {
	var data todoWriteData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		dLog("dsh: wireState.applyTodoWrite decode: %v", err)
		return nil
	}

	// F-DSH-TODO-WIRE-FIX (2026-08-16): wire field is `todos`
	// (verified against @deepseek-ai/dsh-tool-todo source),
	// pre-fix the bridge used `items` and silently dropped every
	// item. todoWriteData.UnmarshalJSON now recovers both shapes.
	items := make([]agent.AgentTaskItem, 0, len(data.Todos))
	skipped := 0

	for _, it := range data.Todos {
		key := it.ID
		if key == "" {
			key = it.Content
		}
		if key == "" {
			// Both ID and Content empty — most likely a wire
			// schema drift (todoItem struct json tags don't match
			// dsh's real wire field names). Don't poison the map
			// with an empty-string key (would collapse all such
			// items into one), but DO surface the failure so ops
			// can see "bridge isn't picking up dsh's todos" via
			// nightme debug dsh dump-wire (unknownCount column).
			skipped++
			continue
		}
		s.tasks[key] = it
		items = append(items, agent.AgentTaskItem{
			ID:         key,
			Subject:    it.Content,
			ActiveForm: it.ActiveForm,
			Status:     todoStatusToTaskStatus(it.Status),
		})
	}

	if skipped > 0 {
		// Bump unknownCount so the failure mode is visible in
		// DumpWireStats alongside other "we don't recognize this
		// wire shape" counters. Caller MUST hold s.mu (we are
		// inside applyEventLocked).
		s.unknownCount++
		warnLogger.Warn("dsh: todo/write items dropped (wire field-name drift?)",
			"skipped", skipped,
			"kept", len(items),
			"hint", "check internal/bridge/dsh/protocol.go todoItem json tags vs dsh wire",
			"unknown_total", s.unknownCount)
	}

	// Even with len(items)==0, emit EventAgentTaskCreate with empty
	// Items. gateway/outbound documents empty Items as the valid
	// "clear the checklist" signal — dropping it freezes the prior
	// checklist for the rest of the session. The Warn log +
	// unknownCount bump above is the only way for ops to tell
	// "intentional clear" apart from "wire drift ate everything".
	return []agent.AgentEvent{{
		Kind:     agent.EventAgentTaskCreate,
		TaskList: &agent.AgentTaskListEvent{Items: items},
	}}
}

// applyProjection ingests one session/projection frame and returns
// the AgentEvent sequence. Phase 1+2 handles only `projection == "todo"`
// (or "tasks"); other projections are graceful no-ops (P3 will add
// title handling).
//
// Acquires s.mu. Use applyProjectionLocked from inside dispatcher.
//
// WIRE-PROBE-REQUIRED: the projection name vocabulary and value
// payload shape are inferred from docs/bridge/dsh.md §4.5. If
// probe reveals different field names, update projectionEnvelope
// in protocol.go only — this method's logic is stable.
func (s *wireState) applyProjection(proj projectionEnvelope) []agent.AgentEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyProjectionLocked(proj)
}

// applyProjectionLocked assumes the caller holds s.mu.
func (s *wireState) applyProjectionLocked(proj projectionEnvelope) []agent.AgentEvent {
	switch proj.Key {
	case "todo", "todos", "tasks":
		// Real dsh wire uses "todos" (plural) per
		// @deepseek-ai/dsh-tool-todo `todos` projection unit
		// (verified 2026-08-16 against captured
		// testdata/projections/todo_snapshot.json). "todo" /
		// "tasks" kept as fallbacks for older / fork builds.
		return s.applyTodoProjectionLocked(proj.Value)
	case "title":
		// Host-pre-computed session title. Stash in s.title for
		// future channel-side rendering; no event emission today
		// (no EventAgentTitleChanged kind in agent package — added
		// only if a channel asks for it).
		//
		// WIRE-PROBE-REQUIRED: payload shape ({"title":"..."} vs
		// {"value":"..."}) — best-guess from typical projection
		// naming. Probe to confirm.
		var t struct {
			Title string `json:"title"`
		}
		if err := json.Unmarshal(proj.Value, &t); err != nil {
			dLog("dsh: applyProjection title decode: %v", err)
			return nil
		}
		s.title = t.Title
		return nil
	default:
		// Unknown projection name — graceful no-op. Don't count
		// as unknown (we don't want to spam logs when dsh adds
		// new projection names).
		return nil
	}
}

// applyTodoProjectionLocked assumes the caller holds s.mu.
//
// Decodes a projection value carrying a todo snapshot (same shape
// as todo/write) and runs the same ID-indexed merge into s.tasks.
//
// Full-snapshot semantics: the bridge treats projection values as
// the authoritative version of the todo list at that moment — any
// wire-id that drops out of the snapshot is considered removed.
// For Phase 1+2 we don't actively prune the map (deletion is rare
// and the next todo/write from raw event will reconcile). P3 can
// add active pruning once we know real-world deletion frequency.
func (s *wireState) applyTodoProjectionLocked(value json.RawMessage) []agent.AgentEvent {
	// F-DSH-TODO-WIRE-FIX (2026-08-16): same fallback as
	// applyTodoWriteLocked — wire uses `todos`, legacy / fork
	// builds may use `items`. Decoding into both keys and
	// preferring `todos` matches the projection-source schema
	// in @deepseek-ai/dsh-tool-todo (the `todos` projection
	// unit: `array({content, status}) | null`).
	var data struct {
		Todos []todoItem `json:"todos"`
		Items []todoItem `json:"items"`
	}
	if err := json.Unmarshal(value, &data); err != nil {
		dLog("dsh: wireState.applyTodoProjection decode: %v", err)
		return nil
	}
	todos := data.Todos
	if len(todos) == 0 {
		todos = data.Items
	}

	items := make([]agent.AgentTaskItem, 0, len(todos))
	skipped := 0

	for _, it := range todos {
		key := it.ID
		if key == "" {
			key = it.Content
		}
		if key == "" {
			// See applyTodoWriteLocked for the rationale on
			// surface-vs-silent: same wire-drift failure mode.
			skipped++
			continue
		}
		s.tasks[key] = it
		items = append(items, agent.AgentTaskItem{
			ID:         key,
			Subject:    it.Content,
			ActiveForm: it.ActiveForm,
			Status:     todoStatusToTaskStatus(it.Status),
		})
	}

	if skipped > 0 {
		// Same field-drift detection as applyTodoWriteLocked —
		// see that comment for the production failure mode this
		// prevents. Caller MUST hold s.mu (we are inside
		// applyProjectionLocked).
		s.unknownCount++
		warnLogger.Warn("dsh: session/projection todo items dropped (wire field-name drift?)",
			"skipped", skipped,
			"kept", len(items),
			"hint", "check internal/bridge/dsh/protocol.go todoItem json tags vs dsh wire",
			"unknown_total", s.unknownCount)
	}

	// Even with len(items)==0, emit EventAgentTaskCreate with empty
	// Items. gateway/outbound documents empty Items as the valid
	// "clear the checklist" signal — dropping it freezes the prior
	// checklist for the rest of the session. The Warn log +
	// unknownCount bump above is the only way for ops to tell
	// "intentional clear" apart from "wire drift ate everything".
	return []agent.AgentEvent{{
		Kind:     agent.EventAgentTaskCreate,
		TaskList: &agent.AgentTaskListEvent{Items: items},
	}}
}
// applyView ingests one ToolEventView payload from dsh's host-
// computed rendering. P3 contract: when a session/event frame
// carries a View, the View is the AUTHORITATIVE source for tool
// status (running / completed / failed) and task snapshot.
//
// Acquires s.mu. Use applyViewLocked from inside dispatcher where
// the lock is already held.
//
// Returns nil — applyView updates state only; the dispatcher will
// have already produced events from the raw envelope Type (e.g.
// tool/call → EventAgentToolStart). applyView just merges the
// host-pre-computed state into wireState.tools / wireState.tasks.
//
// If the View's payload is malformed, returns nil silently (debug
// log only). The raw event translation still ran, so the user
// sees SOMETHING — just without the host's UI polish.
func (s *wireState) applyView(viewBytes json.RawMessage) []agent.AgentEvent {
	if len(viewBytes) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyViewLocked(viewBytes)
}

// applyViewLocked assumes the caller holds s.mu.
func (s *wireState) applyViewLocked(viewBytes json.RawMessage) []agent.AgentEvent {
	var view ToolEventView
	if err := json.Unmarshal(viewBytes, &view); err != nil {
		dLog("dsh: applyView decode: %v", err)
		return nil
	}

	switch view.Kind {
	case "tool_call":
		// Host says this tool is in some status. Bridge accepts
		// whatever host computed (running / completed / failed) —
		// don't second-guess. When raw event also reports tool/
		// result later, host has already merged; we just refresh.
		s.applyToolViewLocked(view)
	case "tool_result":
		// Host-pre-computed tool result view. Same shape as
		// tool_call (the host reuses one View kind envelope for
		// both call and result). The runtime still gets its
		// EventAgentToolEnd from the raw tool/result event —
		// applyView only updates wireState.tools so observers
		// querying internal state see the final status.
		s.applyToolViewLocked(view)
	case "task_list":
		// Host's authoritative task snapshot. Merge into s.tasks
		// using the same ID-keyed rules as applyTodoWriteLocked.
		// This is the "host already did the dedup" path — we just
		// accept its result.
		if len(view.Tasks) == 0 {
			return nil
		}
		for _, it := range view.Tasks {
			key := it.ID
			if key == "" {
				key = it.Content
			}
			if key == "" {
				continue
			}
			s.tasks[key] = it
		}
		// No event emission here — the raw session/event envelope
		// that carried this View already produced events (e.g.
		// todo/write → EventAgentTaskCreate). applyView is purely
		// a state-update sidecar.
	default:
		// Unknown kind (or empty) — graceful no-op. Don't count as
		// unknown (we don't want to spam logs when dsh adds new
		// kind values).
	}

	return nil
}

// recordWireFrame stores one mux frame in the ring buffer for
// later debug triage. Acquires mu (ring buffer is guarded by it).
//
// Called from handle_mux.go's mux-frame switch. Method is the
// mux frame method; for session/event frames, envType is the
// envelope Type (e.g. "tool/call"); otherwise envType is empty.
// bytes is the raw payload size, for ops triage of "is the wire
// payload big enough to matter".
func (s *wireState) recordWireFrame(method, envType string, bytes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wireRing == nil {
		s.wireRing = newWireRingBuffer(64)
	}
	s.wireRing.push(wireFrame{Method: method, EnvelopeType: envType, Bytes: bytes})
}

// incUnknown bumps the unknown-event counter. Called from
// dispatcher's registry-miss path AND handle_mux.go's
// default-method path. Operators read this via DumpWireStats to
// detect "dsh is emitting things we don't handle".
func (s *wireState) incUnknown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unknownCount++
}

// DumpWireStats is a debug snapshot for ops triage. Returns the
// current unknown-event counter and a chronological slice of the
// last 64 raw mux frames.
//
// P4 observability. Called from a future debug command (e.g.
// `nightme debug dsh dump-wire` — to be added separately).
func (s *wireState) DumpWireStats() (unknownCount uint64, frames []wireFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unknownCount, s.wireRing.snapshot()
}

// applyToolViewLocked is the shared body for both tool_call and
// tool_result View kinds (host uses one ToolEventView shape for
// both). Assumes the caller holds s.mu.
//
// Behavior:
//   - Populate st.tools[CallID] with host-pre-computed state
//   - Mark inflight[CallID] = true for "running" / unknown status
//   - Delete inflight[CallID] for "completed" / "failed" status
func (s *wireState) applyToolViewLocked(view ToolEventView) {
	if view.CallID == "" {
		return
	}
	if s.tools == nil {
		s.tools = map[string]toolItem{}
	}
	s.tools[view.CallID] = toolItem{
		CallID:    view.CallID,
		Name:      view.Name,
		Status:    view.Status,
		Output:    view.Output,
		UpdatedAt: view.UpdatedAt,
	}
	if s.inflight == nil {
		s.inflight = map[string]bool{}
	}
	if view.Status == "completed" || view.Status == "failed" {
		delete(s.inflight, view.CallID)
	} else {
		s.inflight[view.CallID] = true
	}
}

// recordAndCountUnknown records a mux frame in the ring buffer AND
// bumps the unknown-count counter, both under a single lock acquire.
// Returns the post-bump count for ops logging convenience.
//
// Used by handle_mux.go's default case (and would be used by any
// future path that records an unrecognized event) so the three
// operations (ring push + count bump + count read) share one lock
// window instead of three sequential acquires.
func (s *wireState) recordAndCountUnknown(method string, bytes int) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wireRing == nil {
		s.wireRing = newWireRingBuffer(64)
	}
	s.wireRing.push(wireFrame{Method: method, EnvelopeType: "", Bytes: bytes})
	s.unknownCount++
	return s.unknownCount
}

// incUnknownLockedRead bumps the unknown-count counter and returns
// the post-bump count. Caller MUST hold s.mu (eventDispatcher.dispatch
// holds it across the lookup + the incUnknown path).
//
// Companion to recordAndCountUnknown — the latter additionally
// pushes a ring buffer entry and is used by handle_mux.go's default
// case. The dispatcher's recordWireFrame call after lock release
// already populates the ring with the envelope Type, so the
// dispatcher uses this counter-only variant under its own lock.
func (s *wireState) incUnknownLockedRead() uint64 {
	s.unknownCount++
	return s.unknownCount
}
