// todo_e2e_test.go — end-to-end pipeline tests for the dsh
// "todo list → OutTaskCreate" flow.
//
// These tests close the gap that the existing
// TestWireState_ApplyEvent_* and TestDispatcher_AllKnownTypesRoute_*
// unit tests leave open: they prove the whole path
//
//	dh wire JSON (muxSessionEvent)
//	  → handleMuxFrame
//	    → dispatcher.dispatch
//	      → handleTodoWrite
//	        → wireState.applyTodoWriteLocked
//	          → []agent.AgentEvent{Kind: EventAgentTaskCreate, TaskList: ...}
//	            → deliver() (the runtime → gateway.chokepoint)
//
// produces a useful EventAgentTaskCreate with the right TaskList.
// That, transitively, is what downstream
// gateway/outbound/translate.go converts to OutTaskCreate for the
// channel (Feishu) to render.
//
// Why this matters: the dsh wire field names (`id`, `content`,
// `activeForm`, `status`) are WIRE-PROBE-REQUIRED per
// protocol.go L437-455. If the real wire uses different names
// (e.g. `uuid`, `subject`, `task_status`), every item is silently
// dropped (both `it.ID` and `it.Content` end up empty), and
// applyTodoWriteLocked still emits EventAgentTaskCreate with an
// EMPTY Items slice — which Feishu interprets as "clear the
// checklist". The user-visible symptom is "the todo list didn't
// show up", which is exactly the bug report this e2e test pins
// down: the contract is "non-empty wire → non-empty TaskList",
// not just "wire decodes → EventAgentTaskCreate emitted".
//
// Tests below cover three scenarios:
//  1. Happy path: wire matches the inferred field names → full
//     TaskList carried through.
//  2. ID fallback: wire lacks `id` but has `content` → graceful
//     fallback in applyTodoWriteLocked (B3 mitigation).
//  3. Field mismatch simulation: wire uses different field names
//     → all items skipped, but EventAgentTaskCreate STILL emits
//     with empty Items. This is the trap that produces the
//     "todo list not converted" symptom; the test documents
//     the behaviour so future maintainers know what to look for.

package dsh

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestTodoWriteEndToEnd_HappyPath drives the full pipeline with
// a wire shape that matches dsh's inferred `id` / `content` /
// `activeForm` / `status` field names. The output must be a
// non-empty EventAgentTaskCreate with the full TaskList.
func TestTodoWriteEndToEnd_HappyPath(t *testing.T) {
	c := makeMuxCollector(t)

	muxPayload := []byte(`{
		"sessionId": "session-e2e-1",
		"event": {
			"type": "todo/write",
			"data": {
				"items": [
					{"id": "1", "content": "Read docs",  "activeForm": "Reading docs",  "status": "completed"},
					{"id": "2", "content": "Write code", "activeForm": "Writing code",  "status": "in_progress"},
					{"id": "3", "content": "Run tests",  "activeForm": "Running tests", "status": "pending"}
				]
			}
		}
	}`)

	c.d.handleMuxFrame("session/event", "rpc-1", muxPayload)

	// Verify the user-visible promise: 1 event, fully populated.
	if len(c.events) != 1 {
		t.Fatalf("expected 1 delivered event, got %d", len(c.events))
	}
	ev := c.events[0]
	if ev.Kind != agent.EventAgentTaskCreate {
		t.Fatalf("Kind = %v, want EventAgentTaskCreate", ev.Kind)
	}
	if ev.TaskList == nil {
		t.Fatal("TaskList is nil; downstream outbound.Translate drops nil TaskList")
	}
	if len(ev.TaskList.Items) != 3 {
		t.Fatalf("len(Items) = %d, want 3 (broken pipeline = items dropped = silent clear-checklist bug)", len(ev.TaskList.Items))
	}

	// Field-level shape — preserves against future field renames.
	if ev.TaskList.Items[0].ID != "1" || ev.TaskList.Items[0].Subject != "Read docs" {
		t.Errorf("items[0] = %+v", ev.TaskList.Items[0])
	}
	if ev.TaskList.Items[1].Status != agent.TaskInProgress {
		t.Errorf("items[1].Status = %v, want TaskInProgress", ev.TaskList.Items[1].Status)
	}
	if ev.TaskList.Items[2].ActiveForm != "Running tests" {
		t.Errorf("items[2].ActiveForm = %q, want Running tests", ev.TaskList.Items[2].ActiveForm)
	}
}

// TestTodoWriteEndToEnd_ContentFallback — wire omits `id`. The
// graceful fallback in applyTodoWriteLocked uses Content as the
// map key. This is the legacy B3 mitigation; the test pins
// that the contract holds through the full mux pipeline.
func TestTodoWriteEndToEnd_ContentFallback(t *testing.T) {
	c := makeMuxCollector(t)

	muxPayload := []byte(`{
		"sessionId": "session-e2e-2",
		"event": {
			"type": "todo/write",
			"data": {
				"items": [
					{"content": "Task A", "status": "pending"},
					{"content": "Task B", "status": "completed"}
				]
			}
		}
	}`)

	c.d.handleMuxFrame("session/event", "rpc-2", muxPayload)

	if len(c.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(c.events))
	}
	ev := c.events[0]
	if ev.Kind != agent.EventAgentTaskCreate {
		t.Fatalf("Kind = %v, want EventAgentTaskCreate", ev.Kind)
	}
	if len(ev.TaskList.Items) != 2 {
		t.Fatalf("len(Items) = %d, want 2 (fallback path broken)", len(ev.TaskList.Items))
	}
	// ID is the content; Subject is the content.
	if ev.TaskList.Items[0].ID != "Task A" || ev.TaskList.Items[0].Subject != "Task A" {
		t.Errorf("items[0] = %+v, want ID=Subject=Task A", ev.TaskList.Items[0])
	}
}

// TestTodoWriteEndToEnd_FieldMismatch — documents the silent
// failure mode the WIRE-PROBE-REQUIRED comment warns about.
//
// If dsh's real wire uses different field names (e.g. `uuid`
// instead of `id`, `subject` instead of `content`), applyTodoWriteLocked
// json.Unmarshals into zero values for every item. The code
// then skips each item (both ID and Content are empty), but
// STILL emits EventAgentTaskCreate with an empty Items slice.
//
// Downstream: outbound.Translate keeps the empty Items (the
// "clear checklist" signal), Feishu renders an empty card, and
// the user-visible symptom is "the todo list didn't show up".
// This test is a tripwire: if the wire shape diverges, this
// test becomes the broken one, not the production code.
//
// The test PASSES today (because the wire shape matches), but
// it documents the trap so a future wire-probe that finds a
// different shape has a clear next step: change the JSON tags
// on todoItem, then this test reconfigures to match the real
// wire shape.
func TestTodoWriteEndToEnd_FieldMismatch(t *testing.T) {
	c := makeMuxCollector(t)

	// Hypothetical wire that uses {task_id, subject, task_status}
	// instead of the inferred {id, content, status}. Items ShouldBeSkipped.
	muxPayload := []byte(`{
		"sessionId": "session-e2e-3",
		"event": {
			"type": "todo/write",
			"data": {
				"items": [
					{"task_id": "1", "subject": "Different field names", "task_status": "pending"}
				]
			}
		}
	}`)

	c.d.handleMuxFrame("session/event", "rpc-3", muxPayload)

	if len(c.events) != 1 {
		t.Fatalf("expected 1 event (even with empty snapshot), got %d", len(c.events))
	}
	ev := c.events[0]
	if ev.Kind != agent.EventAgentTaskCreate {
		t.Fatalf("Kind = %v, want EventAgentTaskCreate", ev.Kind)
	}

	// The trap: this is where the wire-mismatch bug shows up.
	// Items is empty because every item was dropped due to ID/Content
	// both being empty (the fields the inferred JSON tags actually read).
	// If you see this test fail with len(Items) == 1, the wire shape
	// changed and the JSON tags in protocol.go need to follow.
	if len(ev.TaskList.Items) != 0 {
		t.Fatalf("len(Items) = %d, want 0 — wire shape matches inferred field names; if "+
			"non-zero, protocol.go's todoItem JSON tags need updating", len(ev.TaskList.Items))
	}

	// The empty-Items snapshot is exactly the "clear checklist" signal
	// outbound.go documents. If a future bridge wants to suppress
	// this for "wire I don't understand", add a guard here.
	// Today: emit-and-clear is the documented behavior.
}

// TestTodoWriteEndToEnd_TaskListView — pin the View path
// (kind="task_list"). Per protocol.go L415 + state.go L384-401,
// the View path is state-only: it populates wireState.tasks but
// does NOT emit events. The raw session/event envelope that
// arrived alongside the View is the event source.
//
// This test dispatches a session/event with type="todo/write"
// AND a View with kind="task_list". We expect ONE
// EventAgentTaskCreate (from the raw event), and the View's
// tasks should be merged into wireState.tasks via applyViewLocked.
func TestTodoWriteEndToEnd_TaskListView(t *testing.T) {
	c := makeMuxCollector(t)

	muxPayload := []byte(`{
		"sessionId": "session-e2e-4",
		"event": {
			"type": "todo/write",
			"data": {
				"items": [
					{"id": "v-1", "content": "From raw event", "status": "in_progress"}
				]
			}
		},
		"view": {
			"kind": "task_list",
			"tasks": [
				{"id": "v-2", "content": "From view", "status": "completed"}
			]
		}
	}`)

	c.d.handleMuxFrame("session/event", "rpc-4", muxPayload)

	if len(c.events) != 1 {
		t.Fatalf("expected 1 event (raw event source), got %d", len(c.events))
	}
	ev := c.events[0]
	if ev.Kind != agent.EventAgentTaskCreate {
		t.Fatalf("Kind = %v, want EventAgentTaskCreate", ev.Kind)
	}

	// Raw event emits a snapshot of the raw items (1 item).
	if len(ev.TaskList.Items) != 1 {
		t.Fatalf("len(Items) = %d, want 1 (raw event source)", len(ev.TaskList.Items))
	}

	// View path is state-only: wireState.tasks should now have BOTH
	// v-1 (from raw event) and v-2 (from View).
	c.d.wireState.mu.Lock()
	defer c.d.wireState.mu.Unlock()
	if _, ok := c.d.wireState.tasks["v-1"]; !ok {
		t.Error("v-1 missing from wireState.tasks (raw event merge failed)")
	}
	if _, ok := c.d.wireState.tasks["v-2"]; !ok {
		t.Error("v-2 missing from wireState.tasks (View merge failed)")
	}
}

// TestProjection_TodoSnapshot_EmitsTaskCreate — projection
// frames must also emit EventAgentTaskCreate. Today the only
// emission paths are the raw `todo/write` event AND the
// `session/projection` frame (both wireState.applyTodoWriteLocked
// and wireState.applyProjection feed the same event-out path).
// If dsh drops raw events and only sends projections, this
// test catches the gap.
//
// The mux-collector pattern doesn't capture projection events
// because the projection path in handleMuxFrame calls
// d.deliver(ev) directly (not d.dispatcher.deliver). Wrapping
// d.deliver would require plumbing changes that have a blast
// radius beyond the test. Instead this test verifies the
// contract at the wireState boundary — the same boundary the
// handleMuxFrame integration uses.
//
// For the raw session/projection path integration, the
// handleMuxFrame call is structurally identical to the
// session/event path (record + decode + apply + deliver), so
// the mux-collector already validates the decode + record +
// deliver logic for one event class. The wireState contract
// (applyProjection → events with right shape) is what
// differentiates the projection path; we cover that here.
func TestProjection_TodoSnapshot_EmitsTaskCreate(t *testing.T) {
	st := newWireState()
	proj := projectionEnvelope{
		Projection: "todo",
		Value: []byte(`{
			"items": [
				{"id": "p-1", "content": "From projection", "status": "completed"},
				{"id": "p-2", "content": "Also from projection", "status": "pending"}
			]
		}`),
	}

	events := st.applyProjection(proj)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Kind != agent.EventAgentTaskCreate {
		t.Fatalf("Kind = %v, want EventAgentTaskCreate", ev.Kind)
	}
	if ev.TaskList == nil || len(ev.TaskList.Items) != 2 {
		t.Fatalf("TaskList.Items = %+v, want 2 items", ev.TaskList)
	}
	// Confirm wireState was populated (used by P3 View authority).
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.tasks) != 2 {
		t.Errorf("wireState.tasks = %d, want 2", len(st.tasks))
	}
}

// muxCollector bundles a driver + translator + wireState +
// dispatcher + a deliver closure that captures every event the
// dispatcher produces. Lets the e2e tests feed real
// session/event mux-frame JSON without spawning dsh web.
//
// Driver.deliver (the projection path) is a method on
// *driver and can't be overridden from a test, so the projection
// path is exercised at a lower level (TestProjection_TodoSnapshot_EmitsTaskCreate
// in this file). The dispatcher-bridged path covers
// session/event, which is the primary fault surface for the
// "todo list not converted to OutTaskCreate" symptom.
type muxCollector struct {
	d      *driver
	events []agent.AgentEvent
}

func makeMuxCollector(t *testing.T) *muxCollector {
	t.Helper()
	tr := newTranslator("dsh-e2e", "/tmp/e2e")
	st := newWireState()
	d := &driver{
		sessionID: "session-e2e",
		agentName: "dsh-e2e",
		workspace: "/tmp/e2e",
		wireState: st,
		translate: tr,
		events:    make(chan agent.AgentEvent, 64),
		closed:    make(chan struct{}),
		exitDone:  make(chan struct{}),
	}
	c := &muxCollector{d: d}
	d.dispatcher = newDispatcher(tr, st, nil, func(ev agent.AgentEvent) {
		c.events = append(c.events, ev)
	})
	return c
}
