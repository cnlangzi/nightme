// Background-task table tests.
//
// Claude Code reports the background work of a session in a family of
// `system` frames: background_tasks_changed carries the whole set of
// live background tasks, the per-task frames (task_started /
// task_progress / task_updated / task_notification) describe one task.
// The bridge keeps the table and reports it upward whenever it
// changes.
package claudecode

import (
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// subagentReports returns the EventAgentSubagents payloads the
// stream produced.
func subagentReports(evs []agent.AgentEvent) []*agent.SubagentEvent {
	var out []*agent.SubagentEvent
	for _, ev := range evs {
		if ev.Kind == agent.EventAgentSubagents {
			out = append(out, ev.Subagents)
		}
	}
	return out
}

// TestPumpStream_BackgroundTasksChanged_TableFollowsTheCLI covers the
// level signal: the frame carries the whole set of live background
// tasks with REPLACE semantics, so the table holds exactly what the
// frame listed.
func TestPumpStream_BackgroundTasksChanged_TableFollowsTheCLI(t *testing.T) {
	input := `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"a1","task_type":"local_agent","description":"Run sleep 25 echo one"},{"task_id":"a2","task_type":"local_agent","description":"Run sleep 25 echo two"}]}` + "\n"
	evs := streamFromString(input)

	reports := subagentReports(evs)
	if len(reports) != 1 {
		t.Fatalf("got %d reports, want 1", len(reports))
	}
	if got := len(reports[0].Tasks); got != 2 {
		t.Fatalf("len(Tasks) = %d, want 2", got)
	}
	if reports[0].Tasks[0].TaskID != "a1" || reports[0].Tasks[1].TaskID != "a2" {
		t.Errorf("task ids = %q / %q, want a1 / a2",
			reports[0].Tasks[0].TaskID, reports[0].Tasks[1].TaskID)
	}
	if reports[0].Tasks[0].Description != "Run sleep 25 echo one" {
		t.Errorf("Description = %q, want the frame's description",
			reports[0].Tasks[0].Description)
	}
	if reports[0].Tasks[0].TaskType != "local_agent" {
		t.Errorf("TaskType = %q, want local_agent", reports[0].Tasks[0].TaskType)
	}

	// The next level frame replaces the set: one task left.
	input = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"a2","task_type":"local_agent","description":"Run sleep 25 echo two"}]}` + "\n"
	evs = streamFromString(input)
	reports = subagentReports(evs)
	if len(reports) != 1 {
		t.Fatalf("got %d reports, want 1", len(reports))
	}
	if got := len(reports[0].Tasks); got != 1 {
		t.Fatalf("len(Tasks) = %d, want 1 (the set is replaced, not merged)", got)
	}
	if reports[0].Tasks[0].TaskID != "a2" {
		t.Errorf("TaskID = %q, want a2", reports[0].Tasks[0].TaskID)
	}
}

// TestPumpStream_BackgroundTasksChanged_Empty_ClearsTheTable: the last
// task leaving the set arrives as an empty list. The report says
// nothing is running any more.
func TestPumpStream_BackgroundTasksChanged_Empty_ClearsTheTable(t *testing.T) {
	input := `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"a1","task_type":"local_agent","description":"one"}]}` + "\n" +
		`{"type":"system","subtype":"background_tasks_changed","tasks":[]}` + "\n"
	evs := streamFromString(input)

	reports := subagentReports(evs)
	if len(reports) != 2 {
		t.Fatalf("got %d reports, want 2", len(reports))
	}
	if got := len(reports[1].Tasks); got != 0 {
		t.Errorf("len(Tasks) = %d, want 0", got)
	}
}

// TestPumpStream_TaskStarted_AddsTheTask: a task the CLI registered
// in the background joins the table.
func TestPumpStream_TaskStarted_AddsTheTask(t *testing.T) {
	input := `{"type":"system","subtype":"task_started","task_id":"a1","run_id":"r1","tool_use_id":"call_1","description":"Run sleep 25 echo one","subagent_type":"general-purpose","is_backgrounded":true,"spawn_depth":1,"task_type":"local_agent","prompt":"run the command"}` + "\n"
	evs := streamFromString(input)

	reports := subagentReports(evs)
	if len(reports) != 1 {
		t.Fatalf("got %d reports, want 1", len(reports))
	}
	task := reports[0].Tasks[0]
	if task.TaskID != "a1" {
		t.Errorf("TaskID = %q, want a1", task.TaskID)
	}
	if task.Description != "Run sleep 25 echo one" {
		t.Errorf("Description = %q, want the frame's description", task.Description)
	}
	if task.Status != agent.BgTaskStatusRunning {
		t.Errorf("Status = %q, want %q", task.Status, agent.BgTaskStatusRunning)
	}
}

// TestPumpStream_TaskStarted_Foreground_NotInTheTable: a task the CLI
// registered in the foreground is not a background task.
func TestPumpStream_TaskStarted_Foreground_NotInTheTable(t *testing.T) {
	input := `{"type":"system","subtype":"task_started","task_id":"a1","run_id":"r1","description":"foreground","is_backgrounded":false,"task_type":"local_agent"}` + "\n"
	evs := streamFromString(input)

	if n := len(subagentReports(evs)); n != 0 {
		t.Errorf("got %d reports, want 0 (a foreground task is not a background task)", n)
	}
}

// TestPumpStream_TaskUpdated_TerminalStatusStopsTheCount covers the
// lifecycle: the task's status becomes terminal, so it stops being
// counted as running.
func TestPumpStream_TaskUpdated_TerminalStatusStopsTheCount(t *testing.T) {
	input := `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"a1","task_type":"local_agent","description":"one"}]}` + "\n" +
		`{"type":"system","subtype":"task_updated","task_id":"a1","patch":{"status":"completed","end_time":1795250437677}}` + "\n"
	evs := streamFromString(input)

	reports := subagentReports(evs)
	if len(reports) != 2 {
		t.Fatalf("got %d reports, want 2", len(reports))
	}
	if got := agent.RunningSubagents(reports[1].Tasks); got != 0 {
		t.Errorf("running = %d, want 0 (the task finished)", got)
	}
	if reports[1].Tasks[0].Status != agent.BgTaskStatusCompleted {
		t.Errorf("Status = %q, want %q", reports[1].Tasks[0].Status, agent.BgTaskStatusCompleted)
	}
}

// TestPumpStream_TaskProgress_KeepsTheTerminalStatus: a progress
// frame for a task that already finished must not put it back into
// the running count.
func TestPumpStream_TaskProgress_KeepsTheTerminalStatus(t *testing.T) {
	input := `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"a1","task_type":"local_agent","description":"one"}]}` + "\n" +
		`{"type":"system","subtype":"task_updated","task_id":"a1","patch":{"status":"completed"}}` + "\n" +
		`{"type":"system","subtype":"task_progress","task_id":"a1","tool_use_id":"call_1","description":"Running sleep 25","usage":{"total_tokens":6,"tool_uses":1,"duration_ms":1832},"last_tool_name":"Bash"}` + "\n"
	evs := streamFromString(input)

	reports := subagentReports(evs)
	if len(reports) != 3 {
		t.Fatalf("got %d reports, want 3", len(reports))
	}
	last := reports[2]
	if got := agent.RunningSubagents(last.Tasks); got != 0 {
		t.Errorf("running = %d, want 0 (a progress frame does not restart a task)", got)
	}
	if last.Tasks[0].Status != agent.BgTaskStatusCompleted {
		t.Errorf("Status = %q, want %q", last.Tasks[0].Status, agent.BgTaskStatusCompleted)
	}
	if last.Tasks[0].Description != "Running sleep 25" {
		t.Errorf("Description = %q, want the progress frame's description", last.Tasks[0].Description)
	}
}

// TestPumpStream_TaskNotification_TerminalStatus: the notification
// that closes a task carries its terminal status.
func TestPumpStream_TaskNotification_TerminalStatus(t *testing.T) {
	input := `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"a1","task_type":"local_agent","description":"one"}]}` + "\n" +
		`{"type":"system","subtype":"task_notification","task_id":"a1","tool_use_id":"call_1","status":"completed","output_file":"/tmp/tasks/a1.output","summary":"done","usage":{"total_tokens":53,"tool_uses":1,"duration_ms":31013}}` + "\n"
	evs := streamFromString(input)

	reports := subagentReports(evs)
	if len(reports) != 2 {
		t.Fatalf("got %d reports, want 2", len(reports))
	}
	if reports[1].Tasks[0].Status != agent.BgTaskStatusCompleted {
		t.Errorf("Status = %q, want %q", reports[1].Tasks[0].Status, agent.BgTaskStatusCompleted)
	}
	if got := agent.RunningSubagents(reports[1].Tasks); got != 0 {
		t.Errorf("running = %d, want 0", got)
	}
}

// TestPumpStream_TaskFrame_NoChange_NoReport: a frame that does not
// change the table produces no report.
func TestPumpStream_TaskFrame_NoChange_NoReport(t *testing.T) {
	// The same level frame twice: the second changes nothing.
	level := `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"a1","task_type":"local_agent","description":"one"}]}` + "\n"
	evs := streamFromString(level + level)

	if n := len(subagentReports(evs)); n != 1 {
		t.Errorf("got %d reports, want 1 (the second frame changes nothing)", n)
	}
}

// TestPumpStream_TaskProgress_UnknownTask_NoReport: a progress frame
// for a task the table does not know changes nothing — the level
// frame owns which tasks are background tasks.
func TestPumpStream_TaskProgress_UnknownTask_NoReport(t *testing.T) {
	input := `{"type":"system","subtype":"task_progress","task_id":"a1","tool_use_id":"call_1","description":"Running sleep 25","usage":{"total_tokens":6,"tool_uses":1,"duration_ms":1832}}` + "\n"
	evs := streamFromString(input)

	if n := len(subagentReports(evs)); n != 0 {
		t.Errorf("got %d reports, want 0", n)
	}
}

// TestPumpStream_TaskFrames_DoNotEndTheTurn: a task frame is not a
// turn boundary. The turn ends on the result frame and not before.
func TestPumpStream_TaskFrames_DoNotEndTheTurn(t *testing.T) {
	input := `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"a1","task_type":"local_agent","description":"one"}]}` + "\n" +
		`{"type":"system","subtype":"task_started","task_id":"a1","run_id":"r1","description":"one","is_backgrounded":true,"task_type":"local_agent"}` + "\n" +
		`{"type":"system","subtype":"task_notification","task_id":"a1","run_id":"r1","status":"completed","summary":"done"}` + "\n"
	evs := streamFromString(input)

	if n := countKinds(evs, agent.EventAgentResult); n != 0 {
		t.Errorf("EventAgentResult count = %d, want 0", n)
	}
	if n := countKinds(evs, agent.EventAgentDone); n != 0 {
		t.Errorf("EventAgentDone count = %d, want 0", n)
	}
}

// TestPumpStream_TaskFrames_KeepTheTurnsOwnEvents: the task frames
// add a report and nothing else — the turn's own events keep their
// shape.
func TestPumpStream_TaskFrames_KeepTheTurnsOwnEvents(t *testing.T) {
	input := `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"a1","task_type":"local_agent","description":"one"}]}` + "\n" +
		`{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"working on it"}]},"model":"claude-sonnet-4-5"}` + "\n" +
		`{"type":"result","subtype":"success","result":"done","subagent_stats":{"spawned":0,"completed":0,"failed":0,"killed":{"parent":0,"user":0,"system":0}}}` + "\n"
	evs := streamFromString(input)

	if n := countKinds(evs, agent.EventAgentText); n != 1 {
		t.Errorf("EventAgentText count = %d, want 1", n)
	}
	if n := countKinds(evs, agent.EventAgentResult); n != 1 {
		t.Errorf("EventAgentResult count = %d, want 1", n)
	}
	if n := countKinds(evs, agent.EventAgentDone); n != 1 {
		t.Errorf("EventAgentDone count = %d, want 1", n)
	}
	if n := len(subagentReports(evs)); n != 1 {
		t.Errorf("report count = %d, want 1", n)
	}
}

// TestPumpStream_TaskUpdated_MovesToForeground: a task that moves to
// the foreground leaves the background table.
func TestPumpStream_TaskUpdated_MovesToForeground(t *testing.T) {
	input := `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"a1","task_type":"local_agent","description":"one"}]}` + "\n" +
		`{"type":"system","subtype":"task_updated","task_id":"a1","patch":{"is_backgrounded":false}}` + "\n"
	evs := streamFromString(input)

	reports := subagentReports(evs)
	if len(reports) != 2 {
		t.Fatalf("got %d reports, want 2", len(reports))
	}
	if got := len(reports[1].Tasks); got != 0 {
		t.Errorf("len(Tasks) = %d, want 0 (the task is not a background task any more)", got)
	}
}

// TestPumpStream_TaskUpdated_MovesToBackground: a task that moves to
// the background joins the table.
func TestPumpStream_TaskUpdated_MovesToBackground(t *testing.T) {
	input := `{"type":"system","subtype":"task_updated","task_id":"a1","patch":{"is_backgrounded":true}}` + "\n"
	evs := streamFromString(input)

	reports := subagentReports(evs)
	if len(reports) != 1 {
		t.Fatalf("got %d reports, want 1", len(reports))
	}
	if got := len(reports[0].Tasks); got != 1 {
		t.Fatalf("len(Tasks) = %d, want 1", got)
	}
	if reports[0].Tasks[0].Status != agent.BgTaskStatusRunning {
		t.Errorf("Status = %q, want %q", reports[0].Tasks[0].Status, agent.BgTaskStatusRunning)
	}
}

// TestPumpStream_RealBackgroundAgents_ReportsTheRunningCount replays a
// real capture of a turn that launched two background subagents.
//
// The capture is the CLI's own output with stdin held open — the way
// the bridge drives it. The CLI announces each background agent with a
// task_started frame and closes it with a task_notification frame; the
// background shells the agents ran show up as task frames of their
// own and are not background tasks.
func TestPumpStream_RealBackgroundAgents_ReportsTheRunningCount(t *testing.T) {
	evs := streamFromFixture(t, "subagent_two_agents.jsonl", nil)

	reports := subagentReports(evs)
	if len(reports) == 0 {
		t.Fatal("no report — the capture's task frames did not reach the table")
	}
	// The two background agents open one after the other: the count
	// goes 1 → 2.
	sawOne, sawTwo := false, false
	for _, r := range reports {
		switch agent.RunningSubagents(r.Tasks) {
		case 1:
			sawOne = true
		case 2:
			sawTwo = true
		}
	}
	if !sawOne || !sawTwo {
		t.Errorf("running counts = %v, want the report to show 1 then 2",
			runningCounts(reports))
	}
	// The last report is the empty one: both agents finished.
	last := reports[len(reports)-1]
	if got := agent.RunningSubagents(last.Tasks); got != 0 {
		t.Errorf("running = %d, want 0 (both agents finished)", got)
	}
	// The background shells the agents ran are not background tasks:
	// they never enter the table, and they never count.
	for _, r := range reports {
		for _, task := range r.Tasks {
			if task.TaskType != "local_agent" {
				t.Errorf("a non-agent task entered the background table: %+v", task)
			}
		}
	}
}

// runningCounts lists the running count of every report, for the
// failure message of the test above.
func runningCounts(reports []*agent.SubagentEvent) []int {
	out := make([]int, 0, len(reports))
	for _, r := range reports {
		out = append(out, agent.RunningSubagents(r.Tasks))
	}
	return out
}

// TestPumpStream_ForegroundTask_NotificationKeepsItOutOfTheTable: the
// CLI's background shells (the commands a subagent runs) are tasks of
// their own, but they are not background tasks. Their completion frame
// must not put them in the background table.
func TestPumpStream_ForegroundTask_NotificationKeepsItOutOfTheTable(t *testing.T) {
	input := `{"type":"system","subtype":"task_started","task_id":"s1","run_id":"r1","description":"Sleep 15","is_backgrounded":false,"task_type":"local_bash"}` + "\n" +
		`{"type":"system","subtype":"task_notification","task_id":"s1","run_id":"r1","status":"completed","summary":"done"}` + "\n"
	evs := streamFromString(input)

	if n := len(subagentReports(evs)); n != 0 {
		t.Errorf("got %d reports, want 0 (a foreground task is not a background task)", n)
	}
}

// TestPumpStream_TaskNotification_WithoutStatus_EndsTheTask: a
// completion frame that names no status still means the task ended —
// the task must not stay in the running count.
func TestPumpStream_TaskNotification_WithoutStatus_EndsTheTask(t *testing.T) {
	input := `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"a1","task_type":"local_agent","description":"one"}]}` + "\n" +
		`{"type":"system","subtype":"task_notification","task_id":"a1","tool_use_id":"call_1","summary":"done"}` + "\n"
	evs := streamFromString(input)

	reports := subagentReports(evs)
	if len(reports) != 2 {
		t.Fatalf("got %d reports, want 2", len(reports))
	}
	if got := agent.RunningSubagents(reports[1].Tasks); got != 0 {
		t.Errorf("running = %d, want 0 (the task ended)", got)
	}
}
