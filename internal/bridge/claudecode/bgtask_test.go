// Background-task notices: the bridge reports a background subagent
// as two ordinary chat replies — one when it starts, one when it
// finishes — and keeps the background line away from the user's turn.
//
// The frames the tests feed the bridge are the ones a real Claude
// Code emitted (see bgTaskAgentStream): task_started opens a task,
// task_notification closes it, and the result frame of the turn the
// CLI opens for a finished task carries origin.kind
// "task-notification".
package claudecode

import (
	"fmt"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// noticesOnly returns the notices the bridge raised, in order.
func noticesOnly(events []agent.AgentEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		if ev.Kind == agent.EventAgentNotice {
			out = append(out, ev.Text)
		}
	}
	return out
}

// TestPumpStream_BackgroundTask_Notices pins the two notices a
// background subagent produces: one on start, one on finish. The
// foreground task in the same stream (the bash command the subagent
// ran itself) produces none — the CLI reports it with the same frames.
func TestPumpStream_BackgroundTask_Notices(t *testing.T) {
	events := streamFromString(bgTaskAgentStream)
	notices := noticesOnly(events)
	want := []string{
		"🔀 Subagent started: Sleep then reply OK",
		"🔀 Subagent finished: Sleep then reply OK (completed) — OK",
	}
	if len(notices) != len(want) {
		t.Fatalf("got %d notices %q, want %d %q", len(notices), notices, len(want), want)
	}
	for i := range want {
		if notices[i] != want[i] {
			t.Errorf("notice %d = %q, want %q", i, notices[i], want[i])
		}
	}
}

// TestPumpStream_NotificationTurnResult_Dropped pins that the result
// frame of the turn the CLI opens for a finished background task ends
// nothing: no result card, no turn end. The background line never
// meets the user's turn.
func TestPumpStream_NotificationTurnResult_Dropped(t *testing.T) {
	stream := `{"type":"result","subtype":"success","session_id":"s1","duration_ms":11,"is_error":false,"result":"","origin":{"kind":"task-notification","producer":"session-task","runId":"0mwqerlap-6d9f847e"}}` + "\n"
	events := streamFromString(stream)
	if len(events) != 0 {
		t.Errorf("got %d events %+v, want none", len(events), events)
	}
}

// TestPumpStream_NotificationTurnResult_DroppedWithOtherFrames pins
// that dropping the notification turn's result frame leaves the rest
// of the stream alone: the notices of the background task and the
// main turn's own result frame still come through.
func TestPumpStream_NotificationTurnResult_DroppedWithOtherFrames(t *testing.T) {
	events := streamFromString(bgTaskAgentStream)
	var notices, results, dones int
	for _, ev := range events {
		switch ev.Kind {
		case agent.EventAgentNotice:
			notices++
		case agent.EventAgentResult:
			results++
		case agent.EventAgentDone:
			dones++
		}
	}
	if notices != 2 {
		t.Errorf("got %d notices, want 2", notices)
	}
	if results != 1 {
		t.Errorf("got %d result events, want 1 (the main turn's)", results)
	}
	if dones != 1 {
		t.Errorf("got %d turn ends, want 1 (the main turn's)", dones)
	}
}

// TestPumpStream_MainTurnResult_EndsTurn pins that a result frame
// without a notification origin behaves as it always did: the result
// event, then the turn end.
func TestPumpStream_MainTurnResult_EndsTurn(t *testing.T) {
	events := streamFromString(`{"type":"result","subtype":"success","session_id":"s1","duration_ms":3751,"is_error":false,"result":"LAUNCHED","usage":{"input_tokens":11556,"output_tokens":164}}` + "\n")
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (Result + Done): %+v", len(events), events)
	}
	if events[0].Kind != agent.EventAgentResult {
		t.Errorf("events[0].Kind = %v, want EventAgentResult", events[0].Kind)
	}
	if events[1].Kind != agent.EventAgentDone {
		t.Errorf("events[1].Kind = %v, want EventAgentDone", events[1].Kind)
	}
}

// TestPumpStream_ForegroundTask_NoNotice pins that a task the CLI
// ran in the foreground produces no notice — neither on start nor on
// finish.
func TestPumpStream_ForegroundTask_NoNotice(t *testing.T) {
	stream := `{"type":"system","subtype":"task_started","task_id":"bq9x0s2t7","description":"Sleep for 15 seconds","is_backgrounded":false,"task_type":"local_bash"}` + "\n" +
		`{"type":"system","subtype":"task_notification","task_id":"bq9x0s2t7","status":"completed","output_file":"","summary":"Sleep for 15 seconds"}` + "\n"
	events := streamFromString(stream)
	if notices := noticesOnly(events); len(notices) != 0 {
		t.Errorf("got notices %q, want none", notices)
	}
}

// TestPumpStream_TaskStarted_EmptyDescription_UsesTaskID pins the
// fallback name: a task the CLI described with no description is
// named by its id.
func TestPumpStream_TaskStarted_EmptyDescription_UsesTaskID(t *testing.T) {
	stream := `{"type":"system","subtype":"task_started","task_id":"abc123","is_backgrounded":true,"task_type":"local_agent"}` + "\n"
	notices := noticesOnly(streamFromString(stream))
	want := "🔀 Subagent started: abc123"
	if len(notices) != 1 || notices[0] != want {
		t.Errorf("got notices %q, want [%q]", notices, want)
	}
}

// TestPumpStream_TaskNotification_EmptySummary_NoSummarySegment pins
// that a completion frame without a summary yields the notice without
// a summary segment.
func TestPumpStream_TaskNotification_EmptySummary_NoSummarySegment(t *testing.T) {
	stream := `{"type":"system","subtype":"task_started","task_id":"abc123","description":"Sleep then reply OK","is_backgrounded":true}` + "\n" +
		`{"type":"system","subtype":"task_notification","task_id":"abc123","status":"completed","output_file":"/tmp/x.output"}` + "\n"
	notices := noticesOnly(streamFromString(stream))
	want := "🔀 Subagent finished: Sleep then reply OK (completed)"
	if len(notices) != 2 || notices[1] != want {
		t.Errorf("got notices %q, want the second to be %q", notices, want)
	}
}

// TestPumpStream_TaskNotification_UnknownTask_UsesTaskID pins that a
// completion frame for a task the bridge never saw (it joined the
// session late) still produces a notice, named by the task id.
func TestPumpStream_TaskNotification_UnknownTask_UsesTaskID(t *testing.T) {
	stream := `{"type":"system","subtype":"task_notification","task_id":"abc123","status":"failed","summary":"boom"}` + "\n"
	notices := noticesOnly(streamFromString(stream))
	want := "🔀 Subagent finished: abc123 (failed) — boom"
	if len(notices) != 1 || notices[0] != want {
		t.Errorf("got notices %q, want [%q]", notices, want)
	}
}

// TestPumpStream_TaskNotification_MissingStatus_Stopped pins the
// status of a completion frame that names none: the task is over, so
// it is reported as stopped.
func TestPumpStream_TaskNotification_MissingStatus_Stopped(t *testing.T) {
	stream := `{"type":"system","subtype":"task_notification","task_id":"abc123","summary":"boom"}` + "\n"
	notices := noticesOnly(streamFromString(stream))
	want := "🔀 Subagent finished: abc123 (stopped) — boom"
	if len(notices) != 1 || notices[0] != want {
		t.Errorf("got notices %q, want [%q]", notices, want)
	}
}

// TestBgTaskTable_Capacity pins that the table is bounded: it holds
// at most bgTaskTableSize tasks and drops the least recently used one
// to make room.
func TestBgTaskTable_Capacity(t *testing.T) {
	table := newBgTaskTable()
	for i := range bgTaskTableSize + 1 {
		table.put(bgTask{
			ID:          fmt.Sprintf("task-%03d", i),
			Description: fmt.Sprintf("desc-%03d", i),
			Background:  true,
		})
	}
	if got := table.cache.Len(); got > bgTaskTableSize {
		t.Errorf("table holds %d tasks, want at most %d", got, bgTaskTableSize)
	}
	if _, ok := table.get("task-000"); ok {
		t.Error("the least recently used task is still in the table")
	}
	if _, ok := table.get(fmt.Sprintf("task-%03d", bgTaskTableSize)); !ok {
		t.Error("the newest task is missing from the table")
	}
}

// TestBgTaskFinished_EvictedTask_UsesTaskID pins the fallback for a
// task whose table entry was evicted: the notice names the task by
// its id.
func TestBgTaskFinished_EvictedTask_UsesTaskID(t *testing.T) {
	table := newBgTaskTable()
	state := &streamState{bgTasks: table}
	for i := range bgTaskTableSize + 1 {
		table.put(bgTask{
			ID:          fmt.Sprintf("task-%03d", i),
			Description: fmt.Sprintf("desc-%03d", i),
			Background:  true,
		})
	}
	notice, ok := state.bgTaskFinished(streamEvent{
		TaskID:  "task-000",
		Status:  "completed",
		Summary: "OK",
	})
	if !ok {
		t.Fatal("no notice for a task whose entry was evicted")
	}
	want := "🔀 Subagent finished: task-000 (completed) — OK"
	if notice != want {
		t.Errorf("notice = %q, want %q", notice, want)
	}
}

// bgTaskAgentStream is a real Claude Code capture (Claude Code
// 2.1.322, stream-json) trimmed to the frames the bridge acts on: a
// background subagent's start and finish, the foreground bash command
// the subagent ran, the main turn's result frame, and the result
// frame of the turn the CLI opened for the finished subagent.
const bgTaskAgentStream = `{"type":"system","subtype":"task_started","task_id":"a0cf3fbf0a12d45de","run_id":"0mwqerlap-6d9f847e","tool_use_id":"call_H4RKikxrXW8tpLAsTElPTEgU","description":"Sleep then reply OK","subagent_type":"general-purpose","is_backgrounded":true,"spawn_depth":1,"task_type":"local_agent","prompt":"run the bash command ` + "`sleep 15`" + ` and then reply OK","session_id":"e87601c4-1d01-4079-94a8-1fa72b98b7bc"}
{"type":"system","subtype":"task_started","task_id":"bq9x0s2t7","run_id":"0mwqerop5-d9da73ef","owned_by_subagent":true,"tool_use_id":"call_01a1fe2a9e387702bdf3a0c4","description":"Sleep for 15 seconds","is_backgrounded":false,"parent_task_id":"aadb3ece74958f6ff","task_type":"local_bash","session_id":"e87601c4-1d01-4079-94a8-1fa72b98b7bc"}
{"type":"system","subtype":"task_notification","task_id":"bq9x0s2t7","run_id":"0mwqerop5-d9da73ef","tool_use_id":"call_01a1fe2a9e387702bdf3a0c4","status":"completed","output_file":"","summary":"Sleep for 15 seconds","session_id":"e87601c4-1d01-4079-94a8-1fa72b98b7bc"}
{"type":"system","subtype":"task_notification","task_id":"a0cf3fbf0a12d45de","run_id":"0mwqerlap-6d9f847e","tool_use_id":"call_H4RKikxrXW8tpLAsTElPTEgU","status":"completed","output_file":"/tmp/claude-1001/-tmp-cc-two/e87601c4-1d01-4079-94a8-1fa72b98b7bc/tasks/a0cf3fbf0a12d45de.output","summary":"OK","usage":{"total_tokens":6952,"tool_uses":1,"duration_ms":20308},"session_id":"e87601c4-1d01-4079-94a8-1fa72b98b7bc"}
{"type":"result","subtype":"success","session_id":"e87601c4-1d01-4079-94a8-1fa72b98b7bc","duration_ms":3751,"is_error":false,"result":"LAUNCHED","usage":{"input_tokens":11556,"output_tokens":164}}
{"type":"result","subtype":"success","session_id":"e87601c4-1d01-4079-94a8-1fa72b98b7bc","duration_ms":11,"is_error":false,"result":"","origin":{"kind":"task-notification","producer":"session-task","runId":"0mwqerlap-6d9f847e"}}
`
