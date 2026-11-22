// The CLI's background-task table.
//
// Claude Code reports the background work of a session in a family of
// `system` frames: one frame carries the whole set of live background
// tasks (background_tasks_changed), the others describe a single task
// (task_started / task_progress / task_updated / task_notification).
//
// The table lives in the bridge because the CLI reports the
// authoritative list as a whole: reconstructing it in the runtime would
// mean every consumer of the event stream keeps its own copy of a table
// the CLI already publishes. Here the report upward is a pure
// projection of the CLI's own state.
//
// The frames also carry the subagents' prompts and the paths of their
// output files. Neither leaves the bridge: only the identity, the
// description and the status of a task are reported.
package claudecode

import (
	"github.com/cnlangzi/nightme/internal/agent"
)

// bgTask is one entry of the session's background-task table.
type bgTask struct {
	ID          string
	Description string
	TaskType    string
	Status      string
}

// backgroundTask is one entry of the background_tasks_changed frame's
// task list. The CLI sends the whole set of live background tasks with
// REPLACE semantics: the consumer swaps its set for the payload.
//
// Only the fields the report needs are modeled; the frame's other
// fields (run id, subagent type, parent task) are dropped by the
// decoder.
type backgroundTask struct {
	TaskID      string `json:"task_id"`
	TaskType    string `json:"task_type,omitempty"`
	Description string `json:"description"`
}

// taskPatch is the changed-field subset of a task's state, carried by
// the task_updated frame. IsBackgrounded is a pointer so that "absent"
// and "false" stay apart: a task that moves to the foreground is a
// different event from a task whose state merely changed.
type taskPatch struct {
	Status         string `json:"status,omitempty"`
	Description    string `json:"description,omitempty"`
	IsBackgrounded *bool  `json:"is_backgrounded,omitempty"`
}

// applyBackgroundTasksChanged replaces the table with the frame's list.
//
// Every listed task is live — the CLI sends the set of running
// background tasks — so each entry's status is running. A task that
// left the set is gone from the table; the frames that follow it
// (task_updated, task_notification) carry its terminal status.
func (s *streamState) applyBackgroundTasksChanged(tasks []backgroundTask) bool {
	if s == nil {
		return false
	}
	next := make(map[string]bgTask, len(tasks))
	order := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if t.TaskID == "" {
			continue
		}
		if _, seen := next[t.TaskID]; seen {
			continue
		}
		next[t.TaskID] = bgTask{
			ID:          t.TaskID,
			Description: t.Description,
			TaskType:    t.TaskType,
			Status:      agent.BgTaskStatusRunning,
		}
		order = append(order, t.TaskID)
	}
	if sameBgTable(s.bgTasks, s.bgTaskOrder, next, order) {
		return false
	}
	s.bgTasks = next
	s.bgTaskOrder = order
	return true
}

// applyTaskStarted adds or updates the entry the frame names.
//
// The frame describes a task that just started, so the entry's status
// is running. A task the CLI registered in the foreground is not part
// of the background table — it is remembered as foreground, so its
// completion frame does not put it there either.
func (s *streamState) applyTaskStarted(ev streamEvent) bool {
	if s == nil {
		return false
	}
	if !ev.IsBackgrounded {
		s.noteForegroundTask(ev.TaskID)
		return false
	}
	return s.upsertBgTask(ev.TaskID, func(t *bgTask) {
		if ev.Description != "" {
			t.Description = ev.Description
		}
		if ev.TaskType != "" {
			t.TaskType = ev.TaskType
		}
		t.Status = agent.BgTaskStatusRunning
	})
}

// applyTaskProgress updates the description of a task the table knows.
//
// A progress frame carries no status, and a progress frame for a task
// that already finished must not put it back into the running count —
// so the status is left alone. A task the table does not know is left
// alone too: the level frame owns which tasks are background tasks.
func (s *streamState) applyTaskProgress(ev streamEvent) bool {
	if s == nil {
		return false
	}
	return s.updateBgTask(ev.TaskID, func(t *bgTask) {
		if ev.Description != "" {
			t.Description = ev.Description
		}
	})
}

// applyTaskUpdated applies the frame's patch to the entry it names.
//
// A task that moves to the background joins the table; one that moves
// to the foreground leaves it. A patch without that flag updates the
// task the table already knows — the CLI's level frame owns the
// membership, so a task the table does not know is not added here.
func (s *streamState) applyTaskUpdated(ev streamEvent) bool {
	if s == nil || ev.TaskID == "" || ev.Patch == nil {
		return false
	}
	p := ev.Patch
	if p.IsBackgrounded != nil && !*p.IsBackgrounded {
		delete(s.bgForeground, ev.TaskID)
		return s.removeBgTask(ev.TaskID)
	}
	if _, known := s.bgTasks[ev.TaskID]; !known {
		if p.IsBackgrounded == nil {
			return false
		}
		// The task moved to the background: it is one now.
		delete(s.bgForeground, ev.TaskID)
		return s.upsertBgTask(ev.TaskID, func(t *bgTask) {
			applyTaskPatch(t, p)
		})
	}
	return s.updateBgTask(ev.TaskID, func(t *bgTask) {
		applyTaskPatch(t, p)
	})
}

// applyTaskPatch applies the changed fields of a task update.
func applyTaskPatch(t *bgTask, p *taskPatch) {
	if p.Status != "" {
		t.Status = p.Status
	}
	if p.Description != "" {
		t.Description = p.Description
	}
}

// applyTaskNotification records the terminal state of a task.
//
// The frame's status is one of completed / failed / stopped — all
// terminal, so the task stops being counted as running. A frame
// without a status still means the task ended: it is recorded as
// stopped, so a task can never stay in the running count.
//
// A task the CLI registered in the foreground is not a background
// task: its completion frame leaves the table alone.
func (s *streamState) applyTaskNotification(ev streamEvent) bool {
	if s == nil || ev.TaskID == "" {
		return false
	}
	if s.bgForeground[ev.TaskID] {
		delete(s.bgForeground, ev.TaskID)
		return false
	}
	status := ev.Status
	if status == "" {
		status = agent.BgTaskStatusStopped
	}
	return s.upsertBgTask(ev.TaskID, func(t *bgTask) {
		t.Status = status
	})
}

// noteForegroundTask records a task the CLI registered in the
// foreground, so its completion frame does not put it in the
// background table.
func (s *streamState) noteForegroundTask(id string) {
	if s == nil || id == "" {
		return
	}
	if s.bgForeground == nil {
		s.bgForeground = make(map[string]bool)
	}
	s.bgForeground[id] = true
}

// upsertBgTask applies fn to the task's entry, creating it when the
// table does not know the task yet. Reports whether the table changed.
//
// A task that joins the table is running unless the frame says
// otherwise, so the new entry starts out running.
func (s *streamState) upsertBgTask(id string, fn func(*bgTask)) bool {
	if s == nil || id == "" {
		return false
	}
	t, known := s.bgTasks[id]
	before := t
	if !known {
		t = bgTask{ID: id, Status: agent.BgTaskStatusRunning}
	}
	fn(&t)
	if known && t == before {
		return false
	}
	if s.bgTasks == nil {
		s.bgTasks = make(map[string]bgTask)
	}
	s.bgTasks[id] = t
	if !known {
		s.bgTaskOrder = append(s.bgTaskOrder, id)
	}
	return true
}

// updateBgTask applies fn to a task the table already knows. Reports
// whether the table changed.
func (s *streamState) updateBgTask(id string, fn func(*bgTask)) bool {
	if s == nil || id == "" {
		return false
	}
	t, known := s.bgTasks[id]
	if !known {
		return false
	}
	before := t
	fn(&t)
	if t == before {
		return false
	}
	s.bgTasks[id] = t
	return true
}

// removeBgTask drops a task from the table. Reports whether the table
// changed.
func (s *streamState) removeBgTask(id string) bool {
	if s == nil || id == "" {
		return false
	}
	if _, known := s.bgTasks[id]; !known {
		return false
	}
	delete(s.bgTasks, id)
	s.bgTaskOrder = removeTaskOrder(s.bgTaskOrder, id)
	return true
}

// bgTaskSnapshot copies the table into the report payload. The
// payload carries the whole table, so a consumer replaces its view
// wholesale.
func (s *streamState) bgTaskSnapshot() *agent.SubagentEvent {
	if s == nil {
		return nil
	}
	out := &agent.SubagentEvent{Tasks: make([]agent.SubagentTask, 0, len(s.bgTaskOrder))}
	for _, id := range s.bgTaskOrder {
		t, ok := s.bgTasks[id]
		if !ok {
			continue
		}
		out.Tasks = append(out.Tasks, agent.SubagentTask{
			TaskID:      t.ID,
			Description: t.Description,
			TaskType:    t.TaskType,
			Status:      t.Status,
		})
	}
	return out
}

// sameBgTable reports whether two tables hold the same entries in the
// same order.
func sameBgTable(a map[string]bgTask, aOrder []string, b map[string]bgTask, bOrder []string) bool {
	if len(a) != len(b) || len(aOrder) != len(bOrder) {
		return false
	}
	for i, id := range aOrder {
		if bOrder[i] != id {
			return false
		}
	}
	for id, t := range a {
		if b[id] != t {
			return false
		}
	}
	return true
}
