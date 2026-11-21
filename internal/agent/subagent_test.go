package agent

import "testing"

// TestSubagentRunning_OnlyLiveWordsCount pins the default of the
// predicate the channels' running count is built from: a word the
// bridge does not know must not pin a task in the count. A count that
// over-reports keeps the line on the card for the rest of the session.
func TestSubagentRunning_OnlyLiveWordsCount(t *testing.T) {
	live := []string{BgTaskStatusRunning, BgTaskStatusPending, BgTaskStatusPaused}
	for _, s := range live {
		if !SubagentRunning(s) {
			t.Errorf("SubagentRunning(%q) = false, want true", s)
		}
	}
	over := []string{
		BgTaskStatusCompleted,
		BgTaskStatusFailed,
		BgTaskStatusKilled,
		BgTaskStatusStopped,
		"",
		"cancelled",
		"aborted",
		"timed_out",
	}
	for _, s := range over {
		if SubagentRunning(s) {
			t.Errorf("SubagentRunning(%q) = true, want false", s)
		}
	}
}

// TestRunningSubagents_CountsOnlyTheLiveOnes: the count is what the
// channel's line shows.
func TestRunningSubagents_CountsOnlyTheLiveOnes(t *testing.T) {
	tasks := []SubagentTask{
		{TaskID: "a1", Status: BgTaskStatusRunning},
		{TaskID: "a2", Status: BgTaskStatusRunning},
		{TaskID: "a3", Status: BgTaskStatusCompleted},
		{TaskID: "a4", Status: BgTaskStatusFailed},
	}
	if got := RunningSubagents(tasks); got != 2 {
		t.Errorf("running = %d, want 2", got)
	}
	if got := RunningSubagents(nil); got != 0 {
		t.Errorf("running = %d, want 0", got)
	}
}

// TestSubagentRunningLine_EmptyWhenNoneRuns: the line disappears with
// the last running subagent.
func TestSubagentRunningLine_EmptyWhenNoneRuns(t *testing.T) {
	if got := SubagentRunningLine([]SubagentTask{{TaskID: "a1", Status: BgTaskStatusCompleted}}); got != "" {
		t.Errorf("line = %q, want empty", got)
	}
	got := SubagentRunningLine([]SubagentTask{
		{TaskID: "a1", Status: BgTaskStatusRunning},
		{TaskID: "a2", Status: BgTaskStatusRunning},
	})
	if got == "" {
		t.Error("line is empty, want the running count")
	}
}
