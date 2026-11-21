// The vocabulary of a background subagent: the statuses a task can
// be in and the line a channel shows for the running ones.
//
// Both live in one place, so the channels that render the running
// count cannot drift apart: the line is the same line in every
// channel.
package agent

import "fmt"

// The statuses the agent CLI reports for a background task.
//
// The CLI's own vocabulary: a task update carries pending / running /
// completed / failed / killed / paused, a task notification carries
// completed / failed / stopped. The names are plain strings — the
// bridge passes the CLI's word through unchanged.
const (
	BgTaskStatusPending   = "pending"
	BgTaskStatusRunning   = "running"
	BgTaskStatusCompleted = "completed"
	BgTaskStatusFailed    = "failed"
	BgTaskStatusKilled    = "killed"
	BgTaskStatusStopped   = "stopped"
	BgTaskStatusPaused    = "paused"
)

// SubagentEvent is the typed payload for EventAgentSubagents. Tasks
// is the whole background-task table of the session (NOT a delta). An
// empty Tasks slice is a valid "nothing is running any more" signal —
// channels drop their running-subagent line when they see it.
type SubagentEvent struct {
	Tasks []SubagentTask
}

// SubagentTask is one background task the agent is running.
type SubagentTask struct {
	// TaskID is the CLI's id of the task. Stable for the task's
	// lifetime; a task that is resumed keeps its id.
	TaskID string
	// Description is what the task is doing, in the CLI's words.
	Description string
	// TaskType is the kind of task (e.g. "local_agent" for a
	// subagent the agent spawned).
	TaskType string
	// Status is the task's state, one of the BgTaskStatus* words.
	Status string
}

// SubagentRunning reports whether a background task is still running.
// A terminal status (completed / failed / killed / stopped) means the
// task is done; every other status means work is still in flight.
func SubagentRunning(status string) bool {
	switch status {
	case BgTaskStatusCompleted, BgTaskStatusFailed, BgTaskStatusKilled, BgTaskStatusStopped:
		return false
	}
	return true
}

// RunningSubagents counts the tasks of a table that are still running.
func RunningSubagents(tasks []SubagentTask) int {
	n := 0
	for _, t := range tasks {
		if SubagentRunning(t.Status) {
			n++
		}
	}
	return n
}

// SubagentRunningLine renders the count of running background
// subagents. Empty when none is running — the caller drops the line
// from the card.
func SubagentRunningLine(tasks []SubagentTask) string {
	n := RunningSubagents(tasks)
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("🔀 %d 个后台 agent 在跑", n)
}
