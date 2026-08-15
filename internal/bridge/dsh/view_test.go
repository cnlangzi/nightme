// view_test.go — P3 (View authority) + P4 (ring buffer / dump) tests.
//
// P3 covers:
//   - ToolEventView with kind="tool_call" populates wireState.tools
//   - ToolEventView with kind="task_list" merges into wireState.tasks
//   - applyView is state-only (no events emitted)
//   - inflight tracking drops on completion
//
// P4 covers:
//   - recordWireFrame accumulates in ring buffer (chronological order)
//   - incUnknown bumps the counter
//   - DumpWireStats returns both correctly
//   - Ring buffer wraps around (FIFO over oldest)
//   - Apply view inside dispatcher (via eventDispatcher.dispatch)
//     populates state without double-counting unknown for known
//     envelope types.

package dsh

import (
	"encoding/json"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestWireState_ApplyView_ToolCallPopulatesTools
// P3 contract: a ToolEventView with kind="tool_call" populates
// wireState.tools[CallID] with host-pre-computed state.
func TestWireState_ApplyView_ToolCallPopulatesTools(t *testing.T) {
	st := newWireState()
	viewBytes := []byte(`{
		"kind":"tool_call",
		"callId":"c-1",
		"name":"Bash",
		"status":"running",
		"output":"",
		"updatedAt":1234567890
	}`)
	events := st.applyView(viewBytes)
	if events != nil {
		t.Errorf("applyView should return nil (state-only), got %d events", len(events))
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.tools) != 1 {
		t.Fatalf("st.tools size = %d, want 1", len(st.tools))
	}
	got := st.tools["c-1"]
	if got.CallID != "c-1" || got.Name != "Bash" || got.Status != "running" {
		t.Errorf("st.tools[c-1] = %+v", got)
	}
}

// TestWireState_ApplyView_ToolCompletedRemovesInflight
// P3 contract: when host says a tool is completed/failed, the
// inflight tracking should clear that CallID.
func TestWireState_ApplyView_ToolCompletedRemovesInflight(t *testing.T) {
	st := newWireState()
	// First: mark tool as running (inflight = true).
	runningView := []byte(`{"kind":"tool_call","callId":"c-2","name":"Read","status":"running"}`)
	st.applyView(runningView)
	st.mu.Lock()
	if !st.inflight["c-2"] {
		st.mu.Unlock()
		t.Fatal("inflight[c-2] should be true after running view")
	}
	st.mu.Unlock()

	// Now: host says completed. Inflight should clear.
	completedView := []byte(`{
		"kind":"tool_call",
		"callId":"c-2",
		"name":"Read",
		"status":"completed",
		"output":"file contents"
	}`)
	st.applyView(completedView)
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, present := st.inflight["c-2"]; present {
		t.Errorf("inflight[c-2] should be removed after completed view")
	}
	if st.tools["c-2"].Status != "completed" {
		t.Errorf("st.tools[c-2].Status = %q, want completed", st.tools["c-2"].Status)
	}
}

// TestWireState_ApplyView_TaskListMergesIntoTasks
// P3 contract: a ToolEventView with kind="task_list" merges its
// tasks into wireState.tasks using the same ID-keyed rules.
func TestWireState_ApplyView_TaskListMergesIntoTasks(t *testing.T) {
	st := newWireState()
	viewBytes := []byte(`{
		"kind":"task_list",
		"tasks":[
			{"id":"v-1","content":"View Task A","status":"in_progress"},
			{"id":"v-2","content":"View Task B","status":"completed"}
		]
	}`)
	st.applyView(viewBytes)

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.tasks) != 2 {
		t.Fatalf("st.tasks size = %d, want 2", len(st.tasks))
	}
	if st.tasks["v-1"].Content != "View Task A" {
		t.Errorf("st.tasks[v-1].Content = %q", st.tasks["v-1"].Content)
	}
	if st.tasks["v-2"].Status != "completed" {
		t.Errorf("st.tasks[v-2].Status = %q", st.tasks["v-2"].Status)
	}
}

// TestWireState_ApplyProjection_TitleUpdatesState
// W6: projection "title" payload sets wireState.title.
func TestWireState_ApplyProjection_TitleUpdatesState(t *testing.T) {
	st := newWireState()
	proj := projectionEnvelope{
		Projection: "title",
		Value:      []byte(`{"title":"My Session"}`),
	}
	events := st.applyProjection(proj)
	if events != nil {
		t.Errorf("title projection should produce no events, got %d", len(events))
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.title != "My Session" {
		t.Errorf("st.title = %q, want %q", st.title, "My Session")
	}
}
// P3 contract: empty View bytes or unknown kind = graceful no-op.
func TestWireState_ApplyView_EmptyOrUnknownKind_NoOp(t *testing.T) {
	st := newWireState()
	if events := st.applyView(nil); events != nil {
		t.Errorf("nil view should produce no events, got %d", len(events))
	}
	if events := st.applyView([]byte(`{"kind":"future_unknown_kind"}`)); events != nil {
		t.Errorf("unknown kind should produce no events, got %d", len(events))
	}
	if events := st.applyView([]byte(`not-valid-json`)); events != nil {
		t.Errorf("invalid JSON should produce no events (state-only), got %d", len(events))
	}
	st.mu.Lock()
	if len(st.tools) != 0 || len(st.tasks) != 0 {
		t.Errorf("no-ops should not populate state")
	}
	st.mu.Unlock()
}

// TestWireState_RecordWireFrame_ChronologicalOrder
// P4 contract: ring buffer snapshot is in chronological order.
func TestWireState_RecordWireFrame_ChronologicalOrder(t *testing.T) {
	st := newWireState()
	for i := range 5 {
		st.recordWireFrame("session/event", "tool/call", 100+i)
	}
	unknownTotal, frames := st.DumpWireStats()
	if unknownTotal != 0 {
		t.Errorf("unknownTotal = %d, want 0", unknownTotal)
	}
	if len(frames) != 5 {
		t.Fatalf("len(frames) = %d, want 5", len(frames))
	}
	// First frame should be the very first push.
	if frames[0].Bytes != 100 {
		t.Errorf("frames[0].Bytes = %d, want 100", frames[0].Bytes)
	}
	// Last frame should be the last push.
	if frames[4].Bytes != 104 {
		t.Errorf("frames[4].Bytes = %d, want 104", frames[4].Bytes)
	}
}

// TestWireState_RecordWireFrame_WrapAround
// P4 contract: ring buffer wraps around when capacity is reached,
// oldest frames get overwritten (FIFO).
func TestWireState_RecordWireFrame_WrapAround(t *testing.T) {
	st := newWireState()
	// Capacity is 64. Push 100 frames, expect last 64 in chronological order.
	for i := range 100 {
		st.recordWireFrame("session/event", "tool/call", i)
	}
	_, frames := st.DumpWireStats()
	if len(frames) != 64 {
		t.Fatalf("len(frames) = %d, want 64 (capacity)", len(frames))
	}
	// First frame should be the (100-64)th = 36th push, Bytes=36.
	if frames[0].Bytes != 36 {
		t.Errorf("frames[0].Bytes = %d, want 36 (after wrap)", frames[0].Bytes)
	}
	// Last frame should be the 99th push.
	if frames[63].Bytes != 99 {
		t.Errorf("frames[63].Bytes = %d, want 99 (last)", frames[63].Bytes)
	}
}

// TestWireState_IncUnknown_Counts
// P4 contract: incUnknown bumps the counter.
func TestWireState_IncUnknown_Counts(t *testing.T) {
	st := newWireState()
	for range 7 {
		st.incUnknown()
	}
	unknownTotal, _ := st.DumpWireStats()
	if unknownTotal != 7 {
		t.Errorf("unknownTotal = %d, want 7", unknownTotal)
	}
}

// TestDispatcher_UnknownType_CountsAndLogsAtWarn
// P4 contract: dispatcher lookup-miss bumps unknownCount (was
// silently dLog'd before P4).
func TestDispatcher_UnknownType_CountsAndLogsAtWarn(t *testing.T) {
	tr := newTranslator("a", "/tmp")
	st := newWireState()
	d := newDispatcher(tr, st, nil, func(agent.AgentEvent) {})
	muxBytes := makeMuxEvent(t, "dsh/will/invent/this", `{"foo":"bar"}`)
	env, view := decodeMuxEvent(t, muxBytes)
	d.dispatch(env, view)

	unknownTotal, frames := st.DumpWireStats()
	if unknownTotal != 1 {
		t.Errorf("unknownTotal = %d, want 1", unknownTotal)
	}
	if len(frames) != 1 {
		t.Errorf("frames len = %d, want 1", len(frames))
	}
	if frames[0].EnvelopeType != "dsh/will/invent/this" {
		t.Errorf("frames[0].EnvelopeType = %q, want %q", frames[0].EnvelopeType, "dsh/will/invent/this")
	}
}

// TestDispatcher_ViewAttached_PopulatesStateButDoesNotCount
// P3 + P4 combined: a session/event with View attaches state to
// wireState.tools/tasks AND records the wire frame, but does NOT
// increment unknownCount (the envelope Type is known).
func TestDispatcher_ViewAttached_PopulatesStateButDoesNotCount(t *testing.T) {
	tr := newTranslator("a", "/tmp")
	st := newWireState()
	d := newDispatcher(tr, st, nil, func(agent.AgentEvent) {})

	// Wrap session/event with tool/call envelope AND a tool_call View.
	viewJSON := `{"kind":"tool_call","callId":"c-v","name":"Bash","status":"running"}`
	muxJSON := `{
		"sessionId":"s-1",
		"event":{"type":"tool/call","data":{"callId":"c-v","name":"Bash","arguments":"{}"}},
		"view":` + viewJSON + `
	}`
	var muxEv muxSessionEvent
	if err := json.Unmarshal([]byte(muxJSON), &muxEv); err != nil {
		t.Fatalf("decode mux: %v", err)
	}
	var env sessionEventEnvelope
	if err := json.Unmarshal(muxEv.Event, &env); err != nil {
		t.Fatalf("decode env: %v", err)
	}
	d.dispatch(env, muxEv.View)

	// View should have populated st.tools.
	st.mu.Lock()
	tool, ok := st.tools["c-v"]
	st.mu.Unlock()
	if !ok {
		t.Fatal("st.tools[c-v] should be populated by View")
	}
	if tool.Status != "running" {
		t.Errorf("st.tools[c-v].Status = %q, want running", tool.Status)
	}

	// unknownCount should be 0 (envelope Type is known).
	unknownTotal, frames := st.DumpWireStats()
	if unknownTotal != 0 {
		t.Errorf("unknownTotal = %d, want 0 (tool/call is known)", unknownTotal)
	}
	if len(frames) != 1 {
		t.Errorf("frames len = %d, want 1", len(frames))
	}
	if frames[0].EnvelopeType != "tool/call" {
		t.Errorf("frames[0].EnvelopeType = %q, want tool/call", frames[0].EnvelopeType)
	}
}
// TestWireState_ApplyView_EmptyOrUnknownKind_NoOp