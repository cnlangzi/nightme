// F-38: TaskCreate / TaskUpdate bridge tests. Each test runs a
// real pumpStream against a synthetic stream of stream-json
// envelopes that mirror the shape of Claude Code 2.1.220.
//
// We exercise the happy path (TaskCreate success → EventTaskCreate
// snapshot), the update paths (in_progress / completed / subject
// / delete → EventTaskUpdate snapshots), the error paths
// (IsError=true → no state mutation, thread fallback), the
// unparseable path (success result that doesn't match our
// expected regex → no state mutation, thread fallback), the
// out-of-order correlation case (two creates whose tool_results
// land in reverse order), and the system/init reset case
// (a new init event must not leak tasks across sessions).
package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// readTaskFixture loads one of the task_*.json fixtures in
// testdata/. Panics on read failure so test setup errors fail fast.
func readTaskFixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("testdata", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

// concatTasks produces a multi-line stream-json input by joining
// the supplied fixture names in order, each on its own line.
func concatTasks(t *testing.T, names ...string) string {
	t.Helper()
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, readTaskFixture(t, n))
	}
	return strings.Join(parts, "\n")
}

// taskEventOnly returns every emitted event with Kind in
// {EventTaskCreate, EventTaskUpdate}, in the order the bridge
// produced them. Dropping the surrounding tool events makes
// the test assertions about task sequence self-contained.
func taskEventOnly(t *testing.T, events []agent.AgentEvent) []agent.AgentEvent {
	t.Helper()
	out := make([]agent.AgentEvent, 0, len(events))
	for _, ev := range events {
		switch ev.Kind {
		case agent.EventTaskCreate, agent.EventTaskUpdate:
			out = append(out, ev)
		}
	}
	return out
}

// TestPumpStream_TaskCreate_Success extracts the assigned task ID
// from the success result and emits an EventTaskCreate carrying
// the single-item snapshot.
func TestPumpStream_TaskCreate_Success(t *testing.T) {
	stream := concatTasks(t,
		"task_create.json",
		"task_create_result.json",
	)
	events := streamFromString(stream)
	only := taskEventOnly(t, events)
	if len(only) != 1 {
		t.Fatalf("got %d task events, want 1: %+v", len(only), only)
	}
	if only[0].Kind != agent.EventTaskCreate {
		t.Errorf("event kind = %v, want EventTaskCreate", only[0].Kind)
	}
	if only[0].TaskList == nil || len(only[0].TaskList.Items) != 1 {
		t.Fatalf("TaskList = %+v, want exactly 1 item", only[0].TaskList)
	}
	item := only[0].TaskList.Items[0]
	if item.ID != "1" {
		t.Errorf("id = %q, want \"1\"", item.ID)
	}
	if item.Subject == "" {
		t.Errorf("subject empty, want non-empty (either result body or input subject)")
	}
	if item.Status != agent.TaskPending {
		t.Errorf("status = %v, want TaskPending", item.Status)
	}
}

// TestPumpStream_TaskUpdate_Transitions walks pending →
// in_progress → completed and asserts the snapshot reflects the
// status at every step.
func TestPumpStream_TaskUpdate_Transitions(t *testing.T) {
	stream := concatTasks(t,
		"task_create.json",
		"task_create_result.json",
		"task_update_inprogress.json",
		"task_update_inprogress_result.json",
		"task_update_completed.json",
		"task_update_completed_result.json",
	)
	events := streamFromString(stream)
	only := taskEventOnly(t, events)
	if len(only) != 3 {
		t.Fatalf("got %d task events, want 3: %+v", len(only), only)
	}
	// event 0: create
	if only[0].Kind != agent.EventTaskCreate || only[0].TaskList.Items[0].Status != agent.TaskPending {
		t.Errorf("event 0 = %+v, want EventTaskCreate with pending status", only[0])
	}
	// event 1: in_progress
	if only[1].Kind != agent.EventTaskUpdate || only[1].TaskList.Items[0].Status != agent.TaskInProgress {
		t.Errorf("event 1 = %+v, want EventTaskUpdate with in_progress status", only[1])
	}
	// event 2: completed
	if only[2].Kind != agent.EventTaskUpdate || only[2].TaskList.Items[0].Status != agent.TaskCompleted {
		t.Errorf("event 2 = %+v, want EventTaskUpdate with completed status", only[2])
	}
}

// TestPumpStream_TaskUpdate_SubjectAndActiveForm asserts the
// subject / activeForm fields on the input are applied to the
// snapshot even when the success result body is generic.
func TestPumpStream_TaskUpdate_SubjectAndActiveForm(t *testing.T) {
	stream := concatTasks(t,
		"task_create.json",
		"task_create_result.json",
		"task_update_subject.json",
		"task_update_subject_result.json",
	)
	events := streamFromString(stream)
	only := taskEventOnly(t, events)
	if len(only) != 2 {
		t.Fatalf("got %d task events, want 2: %+v", len(only), only)
	}
	item := only[1].TaskList.Items[0]
	if item.Subject != "Implement task checklist (renamed)" {
		t.Errorf("subject = %q, want renamed", item.Subject)
	}
	if item.ActiveForm != "Renaming task" {
		t.Errorf("activeForm = %q, want %q", item.ActiveForm, "Renaming task")
	}
}

// TestPumpStream_TaskUpdate_Delete removes the task from the
// snapshot entirely. The deleted id MUST NOT appear in the
// resulting TaskList.
func TestPumpStream_TaskUpdate_Delete(t *testing.T) {
	stream := concatTasks(t,
		"task_create.json",
		"task_create_result.json",
		"task_update_delete.json",
		"task_update_delete_result.json",
	)
	events := streamFromString(stream)
	only := taskEventOnly(t, events)
	if len(only) != 2 {
		t.Fatalf("got %d task events, want 2: %+v", len(only), only)
	}
	if got := only[1].TaskList; got == nil || len(got.Items) != 0 {
		t.Errorf("post-delete snapshot = %+v, want empty items", got)
	}
}

// TestPumpStream_TaskResultError_Fallback ensures an IsError=true
// tool result does not mutate the task state but still emits a
// generic EventToolEnd so the thread line shows the call.
func TestPumpStream_TaskResultError_Fallback(t *testing.T) {
	stream := concatTasks(t,
		"task_create.json",
		"task_error_result.json",
	)
	events := streamFromString(stream)
	// No task event should have been emitted — only the
	// fallback EventToolEnd.
	for _, ev := range events {
		if ev.Kind == agent.EventTaskCreate || ev.Kind == agent.EventTaskUpdate {
			t.Errorf("error result must not emit task event, got %+v", ev)
		}
	}
	var fallback *agent.AgentEvent
	for i, ev := range events {
		if ev.Kind == agent.EventToolEnd && ev.ToolEnd != nil && ev.ToolEnd.Name == "TaskCreate" {
			fallback = &events[i]
		}
	}
	if fallback == nil {
		t.Fatalf("expected a TaskCreate EventToolEnd fallback, got events: %+v", events)
	}
	if fallback.ToolEnd.Err == nil {
		t.Errorf("fallback ToolEnd.Err = nil, want non-nil error wrapping the reason")
	}
}

// TestPumpStream_TaskUnparseableResult_Fallback ensures a
// non-error result that does not match the expected success
// regex still produces a thread fallback rather than silently
// dropping the call.
func TestPumpStream_TaskUnparseableResult_Fallback(t *testing.T) {
	stream := concatTasks(t,
		"task_create.json",
		"task_unparseable_result.json",
	)
	events := streamFromString(stream)
	for _, ev := range events {
		if ev.Kind == agent.EventTaskCreate || ev.Kind == agent.EventTaskUpdate {
			t.Errorf("unparseable result must not emit task event, got %+v", ev)
		}
	}
	var fallback *agent.AgentEvent
	for i, ev := range events {
		if ev.Kind == agent.EventToolEnd && ev.ToolEnd != nil && ev.ToolEnd.Name == "TaskCreate" {
			fallback = &events[i]
		}
	}
	if fallback == nil {
		t.Fatalf("expected a TaskCreate EventToolEnd fallback, got events: %+v", events)
	}
}

// TestPumpStream_TaskOutOfOrderResults verifies the bridge
// correctly correlates tool_use_id across messages even when
// the tool_results land in a different order than the
// tool_uses were issued. We issue TaskCreate tool_use A
// followed by tool_use B, then deliver B's result first and
// A's result second; both tasks must end up in the final
// snapshot.
func TestPumpStream_TaskOutOfOrderResults(t *testing.T) {
	stream := concatTasks(t,
		"task_create.json",          // tool_use id toolu_tc_1, subject: Implement task checklist
		"task_create_2.json",        // tool_use id toolu_tc_2, subject: Write tests
		"task_create_2_result.json", // tool_result for toolu_tc_2 (reverse order)
		"task_create_result.json",   // tool_result for toolu_tc_1
	)
	events := streamFromString(stream)
	only := taskEventOnly(t, events)
	if len(only) != 2 {
		t.Fatalf("got %d task events, want 2: %+v", len(only), only)
	}
	// The most recent snapshot is the final emitted event;
	// it must contain BOTH tasks.
	final := only[len(only)-1]
	if final.TaskList == nil || len(final.TaskList.Items) != 2 {
		t.Fatalf("final snapshot = %+v, want 2 items", final.TaskList)
	}
	ids := map[string]bool{
		final.TaskList.Items[0].ID: true,
		final.TaskList.Items[1].ID: true,
	}
	if !ids["1"] || !ids["2"] {
		t.Errorf("final snapshot ids = %v, want both 1 and 2", ids)
	}
}

// TestPumpStream_TaskNoGenericToolEnd confirms the bridge
// suppresses the generic EventToolStart / EventToolEnd pair for
// confirmed task tools. Without this guard, every TaskCreate
// would produce four events (TaskStart + TaskEnd + taskCreate)
// and double the user's view.
func TestPumpStream_TaskNoGenericToolEnd(t *testing.T) {
	stream := concatTasks(t,
		"task_create.json",
		"task_create_result.json",
	)
	events := streamFromString(stream)
	for i, ev := range events {
		switch ev.Kind {
		case agent.EventToolStart, agent.EventToolEnd:
			t.Errorf("event %d = %+v, want no generic tool events for task tools", i, ev)
		}
	}
}

// TestPumpStream_TaskSystemInitReset verifies the
// system/init event clears the bridge's task state so a resumed
// session does not leak prior tasks into a fresh turn. We
// deliberately issue a task create + update, then a system/init,
// then another task create, and assert the post-init snapshot
// is a fresh single-item list (no leakage from the pre-init
// task). We do NOT assert on the absolute number of task events
// because the bridge re-emits a task snapshot for the post-init
// create only; the pre-init events all came before the reset.
func TestPumpStream_TaskSystemInitReset(t *testing.T) {
	init := readTaskFixture(t, "init.json")
	stream := strings.Join([]string{
		readTaskFixture(t, "task_create.json"),
		readTaskFixture(t, "task_create_result.json"),
		readTaskFixture(t, "task_update_inprogress.json"),
		readTaskFixture(t, "task_update_inprogress_result.json"),
		init, // system/init in the middle — must clear state.tasks
		readTaskFixture(t, "task_create.json"),
		readTaskFixture(t, "task_create_result.json"),
	}, "\n")
	events := streamFromString(stream)
	only := taskEventOnly(t, events)
	// First two events come from the pre-init turn; the
	// remaining event(s) come from the post-init turn.
	if len(only) < 3 {
		t.Fatalf("got %d task events, want at least 3: %+v", len(only), only)
	}
	// The LAST event must be the post-init create, and its
	// snapshot must be exactly 1 item (no leakage from the
	// pre-init task #1 that the bridge just cleared).
	final := only[len(only)-1]
	if final.Kind != agent.EventTaskCreate {
		t.Errorf("final kind = %v, want EventTaskCreate", final.Kind)
	}
	if final.TaskList == nil || len(final.TaskList.Items) != 1 {
		t.Errorf("post-init snapshot = %+v, want exactly 1 item (no leakage)", final.TaskList)
	}
}
