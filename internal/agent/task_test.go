package agent

import "testing"

// TestTaskStatusString pins the names of the task states: a state
// that reads "failed" is not a state that reads "pending".
func TestTaskStatusString(t *testing.T) {
	cases := []struct {
		status AgentTaskStatus
		want   string
	}{
		{TaskPending, "pending"},
		{TaskInProgress, "in_progress"},
		{TaskCompleted, "completed"},
		{TaskDeleted, "deleted"},
		{TaskCancelled, "cancelled"},
		{TaskFailed, "failed"},
		{TaskSkipped, "skipped"},
	}
	for _, c := range cases {
		if got := c.status.String(); got != c.want {
			t.Errorf("%d.String() = %q, want %q", c.status, got, c.want)
		}
	}
}

// TestTaskStatusMarker pins the marker of every state. Every marker
// is one character between the brackets, so the status column of a
// checklist has the same width on every row.
func TestTaskStatusMarker(t *testing.T) {
	cases := []struct {
		status AgentTaskStatus
		want   string
	}{
		{TaskPending, "[ ]"},
		{TaskInProgress, "[>]"},
		{TaskCompleted, "[√]"},
		{TaskFailed, "[!]"},
		{TaskCancelled, "[-]"},
		{TaskSkipped, "[/]"},
	}
	for _, c := range cases {
		got := TaskStatusMarker(c.status)
		if got != c.want {
			t.Errorf("TaskStatusMarker(%d) = %q, want %q", c.status, got, c.want)
		}
		if len([]rune(got)) != 3 {
			t.Errorf("TaskStatusMarker(%d) = %q, want one character between the brackets", c.status, got)
		}
	}
}
