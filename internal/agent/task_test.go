package agent

import (
	"strings"
	"testing"
)

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
		{TaskCompleted, "[v]"},
		{TaskFailed, "[!]"},
		{TaskSkipped, "[-]"},
		{TaskCancelled, "[x]"},
		{TaskDeleted, "[x]"},
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

// TestFormatTaskRow pins the row of a checklist: the number of the
// row, the marker of its state, its subject and its detail.
func TestFormatTaskRow(t *testing.T) {
	cases := []struct {
		name  string
		index int
		item  AgentTaskItem
		want  string
	}{
		{
			name:  "a row with a detail",
			index: 1,
			item:  AgentTaskItem{ID: "t1", Subject: "commit", Status: TaskCompleted, ActiveForm: "the commit landed"},
			want:  "1. [v] commit (the commit landed)",
		},
		{
			name:  "a row without a detail",
			index: 2,
			item:  AgentTaskItem{ID: "t2", Subject: "commit", Status: TaskPending},
			want:  "2. [ ] commit",
		},
		{
			name:  "an empty subject falls back to the id",
			index: 3,
			item:  AgentTaskItem{ID: "t3", Status: TaskPending},
			want:  "3. [ ] t3",
		},
		{
			name:  "a whitespace-only subject falls back to the id",
			index: 4,
			item:  AgentTaskItem{ID: "t4", Subject: "   ", Status: TaskPending},
			want:  "4. [ ] t4",
		},
		{
			name:  "the subject and the detail are trimmed",
			index: 5,
			item:  AgentTaskItem{ID: "t5", Subject: "  done  ", Status: TaskCompleted, ActiveForm: "  it landed  "},
			want:  "5. [v] done (it landed)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FormatTaskRow(c.index, c.item); got != c.want {
				t.Errorf("FormatTaskRow(%d, %+v) = %q, want %q", c.index, c.item, got, c.want)
			}
		})
	}
}

// TestFormatTaskRowNeverStartsWithATodoMarker — Feishu's `lark_md`
// parses the leading "[ ]" of a todo row as a checkbox and then
// renders that row by other rules than the rest of the list.
func TestFormatTaskRowNeverStartsWithATodoMarker(t *testing.T) {
	for _, status := range []AgentTaskStatus{
		TaskPending, TaskInProgress, TaskCompleted, TaskFailed,
		TaskSkipped, TaskCancelled, TaskDeleted,
	} {
		row := FormatTaskRow(1, AgentTaskItem{ID: "t1", Subject: "commit", Status: status})
		if strings.HasPrefix(row, "- ") {
			t.Errorf("the row %q starts with a todo marker", row)
		}
	}
}
