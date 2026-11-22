// The bridge's table of the tasks the CLI reported.
//
// Claude Code describes the work it delegates in a family of `system`
// frames: task_started opens a task, task_notification closes it, and
// the pair carries the task's identity (task_id) plus a human-readable
// description of what the task is doing.
//
// The table exists for one reason: the notification frame carries no
// description, so the notice that a task finished cannot name the task
// without the table.
//
// The table is bounded — a long session keeps reporting tasks — so the
// least recently used entry is evicted once the table is full. A task
// whose entry was evicted is named by its id instead.
package claudecode

import (
	lru "github.com/hashicorp/golang-lru/v2"
)

// bgTaskTableSize is how many tasks the bridge remembers per session.
const bgTaskTableSize = 500

// bgTaskStatusRunning is the status of a task the CLI reported as
// started.
const bgTaskStatusRunning = "running"

// bgTaskStatusStopped is the status a task that ended without one gets.
// The CLI's notification frame names the outcome it saw ("completed",
// "failed", ...); a frame without one still means the task is over.
const bgTaskStatusStopped = "stopped"

// bgTask is one entry of the session's task table. It records what
// the CLI reported about the task.
type bgTask struct {
	ID          string
	Description string
	TaskType    string
	Status      string
	RunID       string

	// Background tells whether the CLI ran the task in the
	// background. A foreground task — a bash command the agent ran
	// itself — reports the same frames, and its frames must not
	// produce notices.
	Background bool
}

// bgTaskTable is the session's table of the tasks the CLI reported.
type bgTaskTable struct {
	cache *lru.Cache[string, bgTask]
}

// newBgTaskTable returns the session's task table.
func newBgTaskTable() (*bgTaskTable, error) {
	cache, err := lru.New[string, bgTask](bgTaskTableSize)
	if err != nil {
		return nil, err
	}
	return &bgTaskTable{cache: cache}, nil
}

// put records a task, evicting the least recently used entry when the
// table is full.
func (t *bgTaskTable) put(task bgTask) {
	if t == nil || t.cache == nil {
		return
	}
	t.cache.Add(task.ID, task)
}

// get returns the task the table knows under id. The lookup marks the
// entry as recently used.
func (t *bgTaskTable) get(id string) (bgTask, bool) {
	if t == nil || t.cache == nil {
		return bgTask{}, false
	}
	return t.cache.Get(id)
}

// bgTaskStarted records the task the frame opens and returns the
// notice for it. A foreground task gets no notice — the CLI reports
// the frames of the agent's own background shell the same way.
func (s *streamState) bgTaskStarted(ev streamEvent) (string, bool) {
	if s == nil || ev.TaskID == "" {
		return "", false
	}
	task := bgTask{
		ID:          ev.TaskID,
		Description: ev.Description,
		TaskType:    ev.TaskType,
		Status:      bgTaskStatusRunning,
		RunID:       ev.RunID,
		Background:  ev.IsBackgrounded,
	}
	s.bgTasks.put(task)
	if !task.Background {
		return "", false
	}
	return bgStartedLine(bgTaskName(task)), true
}

// bgTaskFinished returns the notice for the task the frame closes.
//
// A task the table knows as foreground gets no notice. A task the
// table does not know — the bridge joined the session late, or the
// entry was evicted — is named by its id.
func (s *streamState) bgTaskFinished(ev streamEvent) (string, bool) {
	if s == nil || ev.TaskID == "" {
		return "", false
	}
	task, known := s.bgTasks.get(ev.TaskID)
	if known && !task.Background {
		return "", false
	}
	status := ev.Status
	if status == "" {
		status = bgTaskStatusStopped
	}
	if known {
		task.Status = status
		s.bgTasks.put(task)
	}
	name := ev.TaskID
	if known && task.Description != "" {
		name = task.Description
	}
	return bgFinishedLine(name, status, ev.Summary), true
}

// bgTaskName is the name of a task in the notices: its description
// when the CLI gave one, its id otherwise.
func bgTaskName(task bgTask) string {
	if task.Description != "" {
		return task.Description
	}
	return task.ID
}

// bgStartedLine renders the notice for a task that started.
func bgStartedLine(name string) string {
	return "🔀 Subagent started: " + name
}

// bgFinishedLine renders the notice for a task that finished. The
// summary is the CLI's own one-line report of what the task did; it is
// left out when the CLI sent none.
func bgFinishedLine(name, status, summary string) string {
	line := "🔀 Subagent finished: " + name + " (" + status + ")"
	if summary != "" {
		line += " — " + summary
	}
	return line
}
