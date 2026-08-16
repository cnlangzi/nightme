// todo_testdata_test.go — drives the real dsh todo_write wire
// payload (captured in testdata/envelopes/todo_write.json)
// through the bridge's full mux → dispatcher → wireState →
// deliver pipeline.
//
// This test exists for two reasons:
//
//  1. Pin the wire shape against future drift. The captured
//     JSON is the source of truth for what dsh-tool-todo emits;
//     if dsh changes the schema, the test fails and the fix
//     lands in protocol.go (the only place field names live).
//
//  2. Pin the bridge's behavior against the captured shape —
//     the deliver closure must see exactly 1
//     EventAgentTaskCreate carrying 7 items, each with the
//     correct Subject / Status mapping.

package dsh

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// loadTestdataEnvelope reads a JSON file from
// internal/bridge/dsh/testdata/envelopes/. Fails the test on
// read error so the test pins both the file's existence and
// its parseability.
func loadTestdataEnvelope(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", "envelopes", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// TestTodoWriteFromCapturedWirePayload drives the real dsh
// wire payload (captured against @deepseek-ai/dsh-tool-todo
// source on 2026-08-16) through the bridge's full mux pipeline.
// The shape verified by this test is what dsh actually emits,
// not a synthetic shape.
func TestTodoWriteFromCapturedWirePayload(t *testing.T) {
	c := makeMuxCollector(t)

	muxPayload := loadTestdataEnvelope(t, "todo_write.json")
	c.d.handleMuxFrame("session/event", "rpc-captured", muxPayload)

	if len(c.events) != 1 {
		t.Fatalf("expected 1 delivered event, got %d", len(c.events))
	}
	ev := c.events[0]
	if ev.Kind != agent.EventAgentTaskCreate {
		t.Fatalf("Kind = %v, want EventAgentTaskCreate", ev.Kind)
	}

	// The captured payload has 7 todos. Pre-fix this would be
	// 0 (silent drop) — that's the production symptom we're
	// guarding against.
	if len(ev.TaskList.Items) != 7 {
		t.Fatalf("len(Items) = %d, want 7 — captured wire payload did NOT "+
			"flow through the bridge. Production symptom: empty "+
			"Feishu checklist even though dsh's own UI shows the "+
			"todos. Check protocol.go todoWriteData json tags.",
			len(ev.TaskList.Items))
	}

	// Spot-check status mapping (the most likely drift target).
	wantStatuses := []agent.AgentTaskStatus{
		agent.TaskCompleted,
		agent.TaskCompleted,
		agent.TaskInProgress,
		agent.TaskPending,
		agent.TaskPending,
		agent.TaskPending,
		agent.TaskPending,
	}
	for i, want := range wantStatuses {
		if ev.TaskList.Items[i].Status != want {
			t.Errorf("items[%d].Status = %v, want %v",
				i, ev.TaskList.Items[i].Status, want)
		}
	}

	// Spot-check the bridge's ID fallback (wire omits id, so
	// bridge uses Content as key).
	if ev.TaskList.Items[0].ID != "Read docs/SPEC.md" {
		t.Errorf("items[0].ID = %q, want content fallback", ev.TaskList.Items[0].ID)
	}
	if ev.TaskList.Items[0].Subject != "Read docs/SPEC.md" {
		t.Errorf("items[0].Subject = %q, want Read docs/SPEC.md",
			ev.TaskList.Items[0].Subject)
	}

	// Confirm wireState populated (P3 View authority + dedup).
	c.d.wireState.mu.Lock()
	defer c.d.wireState.mu.Unlock()
	if len(c.d.wireState.tasks) != 7 {
		t.Errorf("wireState.tasks = %d, want 7", len(c.d.wireState.tasks))
	}
}