// todo_realwire_test.go — wire-shape regression tests against
// the REAL dsh wire payload (verified against
// @deepseek-ai/dsh-tool-todo source on 2026-08-16).
//
// These tests pin the field-name contract that was the root
// cause of the "todo list not converted to OutTaskCreate"
// production symptom:
//
//   - Top-level container is `todos`, NOT `items` (verified:
//     `agent.session.append('todo/write', { todos })`).
//   - Each entry is `{content, status}` — dsh's tool
//     registration forbids additionalProperties (the schema
//     uses `additionalProperties: false`).
//   - status values are exactly: pending | in_progress | completed
//   - No `id` or `activeForm` field on the wire (dsh doesn't
//     carry those on its schema).
//
// F-DSH-TODO-WIRE-FIX (2026-08-16): pre-fix the bridge's
// `todoWriteData` used `Items []todoItem json:"items"`, which
// silently dropped every real dsh frame. The fix adds `Todos`
// as the primary tag with `Items` as a legacy fallback
// (handled by custom UnmarshalJSON).
//
// These tests are the tripwire: if dsh changes the wire shape
// again, these fail and the fix lands in protocol.go, not in
// channel / runtime.

package dsh

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// muxCollector bundles a driver + translator + wireState +
// dispatcher + a deliver closure that captures every event the
// dispatcher produces. Lets the tests feed real session/event
// mux-frame JSON without spawning dsh web.
type muxCollector struct {
	d      *driver
	events []agent.AgentEvent
}

func makeMuxCollector(t *testing.T) *muxCollector {
	t.Helper()
	tr := newTranslator("dsh-realwire", "/tmp/realwire")
	st := newWireState()
	d := &driver{
		sessionID:    "session-realwire",
		agentName:    "dsh-realwire",
		workspace:    "/tmp/realwire",
		wireState:    st,
		translate:    tr,
		events:       make(chan agent.AgentEvent, 64),
		closed:       make(chan struct{}),
	}
	c := &muxCollector{d: d}
	d.dispatcher = newDispatcher(tr, st, nil, func(ev agent.AgentEvent) {
		c.events = append(c.events, ev)
	})
	return c
}

// TestTodoWriteRealWireShape drives the exact payload shape
// dsh-tool-todo emits (verified by reading the source on
// 2026-08-16). Wire shape:
//
//	{"type":"todo/write","data":{"todos":[
//	  {"content":"Read docs","status":"completed"},
//	  {"content":"Write code","status":"in_progress"}
//	]}}
//
// This is the regression test for the production symptom:
// bridge previously used `items` (silent drop), now uses
// `todos` (real wire). Without this fix, real-world dsh
// sessions render an empty checklist in Feishu even though
// dsh's own UI displays the full todo list (the same wire
// data that the bridge was dropping).
func TestTodoWriteRealWireShape(t *testing.T) {
	c := makeMuxCollector(t)

	// Exact wire shape from dsh-tool-todo/lib/types/index.js:
	// agent.session.append('todo/write', { todos: [...] })
	muxPayload := []byte(`{
		"sessionId": "session-real-1",
		"event": {
			"type": "todo/write",
			"data": {
				"todos": [
					{"content": "Read docs/SPEC.md", "status": "completed"},
					{"content": "Read docs/bridge/dsh.md", "status": "completed"},
					{"content": "Read docs/feat/F-dsh-bridge.md", "status": "in_progress"},
					{"content": "Read all bridge/dsh/*.go", "status": "pending"},
					{"content": "Cross-check spec vs implementation", "status": "pending"}
				]
			}
		}
	}`)

	c.d.handleMuxFrame("session/event", "rpc-real-1", muxPayload)

	// Real wire = 5 todos → EventAgentTaskCreate must carry 5
	// items. Pre-fix: 0 items (silent "clear checklist" bug).
	if len(c.events) != 1 {
		t.Fatalf("expected 1 delivered event, got %d", len(c.events))
	}
	ev := c.events[0]
	if ev.Kind != agent.EventAgentTaskCreate {
		t.Fatalf("Kind = %v, want EventAgentTaskCreate", ev.Kind)
	}
	if len(ev.TaskList.Items) != 5 {
		t.Fatalf("len(Items) = %d, want 5 — this is the production bug shape (silently dropped); "+
			"if non-zero here, the wire shape changed and protocol.go's tags need updating",
			len(ev.TaskList.Items))
	}

	// Verify field mapping for the real wire (content-only, no id/activeForm).
	items := ev.TaskList.Items
	if items[0].Subject != "Read docs/SPEC.md" {
		t.Errorf("items[0].Subject = %q, want Read docs/SPEC.md", items[0].Subject)
	}
	// ID is content (bridge fallback when wire omits id).
	if items[0].ID != "Read docs/SPEC.md" {
		t.Errorf("items[0].ID = %q, want content (bridge fallback)", items[0].ID)
	}
	if items[0].Status != agent.TaskCompleted {
		t.Errorf("items[0].Status = %v, want TaskCompleted", items[0].Status)
	}
	if items[2].Status != agent.TaskInProgress {
		t.Errorf("items[2].Status = %v, want TaskInProgress", items[2].Status)
	}
	if items[3].Status != agent.TaskPending {
		t.Errorf("items[3].Status = %v, want TaskPending", items[3].Status)
	}

	// Confirm unknownCount was NOT bumped — this is the happy
	// path (no wire drift). If unknownCount > 0 here, the
	// F-DSH-TODO-FIX-LOG instrumentation is firing on healthy
	// traffic (false positive).
	unknown, _ := c.d.wireState.DumpWireStats()
	if unknown != 0 {
		t.Errorf("unknownCount = %d, want 0 (real-wire happy path should not trigger drift detection)",
			unknown)
	}
}

// TestTodoWriteLegacyItemsField_Fallback covers the case where
// some fork / older dsh build uses `items` instead of `todos`
// (pre-fix dsh bridge used this tag). The bridge's custom
// UnmarshalJSON should recover the payload via the legacy
// Items field. If dsh ever renames `todos` → something else,
// this test becomes a guide for adding the next fallback.
func TestTodoWriteLegacyItemsField_Fallback(t *testing.T) {
	c := makeMuxCollector(t)

	muxPayload := []byte(`{
		"sessionId": "session-legacy",
		"event": {
			"type": "todo/write",
			"data": {
				"items": [
					{"content": "Legacy item A", "status": "pending"},
					{"content": "Legacy item B", "status": "completed"}
				]
			}
		}
	}`)

	c.d.handleMuxFrame("session/event", "rpc-legacy", muxPayload)

	if len(c.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(c.events))
	}
	ev := c.events[0]
	if len(ev.TaskList.Items) != 2 {
		t.Fatalf("len(Items) = %d, want 2 (legacy `items` field fallback broken)", len(ev.TaskList.Items))
	}
}

// TestTodoWriteBothFieldsPrefersTodos — if a malformed /
// non-canonical wire somehow carries both `todos` and `items`,
// `todos` wins (it's the canonical name per dsh-tool-todo source).
// This prevents ambiguity regressions in UnmarshalJSON.
func TestTodoWriteBothFieldsPrefersTodos(t *testing.T) {
	c := makeMuxCollector(t)

	muxPayload := []byte(`{
		"sessionId": "session-both",
		"event": {
			"type": "todo/write",
			"data": {
				"todos": [{"content": "Canonical todos field", "status": "pending"}],
				"items": [{"content": "Should be ignored", "status": "completed"}]
			}
		}
	}`)

	c.d.handleMuxFrame("session/event", "rpc-both", muxPayload)

	if len(c.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(c.events))
	}
	ev := c.events[0]
	if len(ev.TaskList.Items) != 1 {
		t.Fatalf("len(Items) = %d, want 1 (todos field should win)", len(ev.TaskList.Items))
	}
	if ev.TaskList.Items[0].Subject != "Canonical todos field" {
		t.Errorf("Subject = %q, want Canonical todos field", ev.TaskList.Items[0].Subject)
	}
}

// TestTodoProjectionRealWireShape — same wire shape check for
// the projection path. The discriminator on the real dsh wire
// is "todos" (plural), per the captured testdata
// testdata/projections/todo_snapshot.json. The projection
// value shape matches the raw event shape per
// dsh-tool-todo's `todos` projection unit.
func TestTodoProjectionRealWireShape(t *testing.T) {
	st := newWireState()
	proj := projectionEnvelope{
		Key: "todos",
		Value: []byte(`{
			"todos": [
				{"content": "From projection (real wire)", "status": "completed"}
			]
		}`),
	}

	events := st.applyProjection(proj)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if len(ev.TaskList.Items) != 1 {
		t.Fatalf("len(Items) = %d, want 1 (projection path broken for real wire)",
			len(ev.TaskList.Items))
	}
	if ev.TaskList.Items[0].Subject != "From projection (real wire)" {
		t.Errorf("Subject = %q", ev.TaskList.Items[0].Subject)
	}
}