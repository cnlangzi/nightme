// F-38: provider-native task tool parsing and snapshot management.
//
// This file is the ONLY place in the claudecode bridge where the
// provider-specific names "TaskCreate", "TaskUpdate", "subject",
// "activeForm", "taskId", "status" (and the human-rendered result
// text starting with "Task #N ...") appear. They are normalised
// into the generic agent.AgentTaskListEvent shape so the Gateway and
// Feishu channels can render the checklist without knowing about
// the Claude Code protocol.
package claudecode

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
)

// jsonRaw is an alias for json.RawMessage used in the task tool
// helper to keep function signatures compact and idiomatic.
type jsonRaw = json.RawMessage

// decodeJSONBestEffort decodes raw into target, ignoring errors.
// Used to parse provider-native input fields that may be
// malformed or absent; the caller falls back to defaults.
func decodeJSONBestEffort(raw []byte, target any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, target)
}

// taskCreatedPattern captures the success line Claude Code emits
// after a TaskCreate call. The captured group is the assigned
// task ID; the rest of the line is the human-friendly subject
// reminder. We anchor on `^` + the literal "Task #" so protocol
// drift (additional text, multi-line) does not silently match a
// different shape.
var taskCreatedPattern = regexp.MustCompile(`^Task #(\S+) created successfully:`)

// taskUpdatedPattern matches the success line emitted by
// TaskUpdate. The body after the id is not parsed (it is a
// human reminder of what changed; status / subject deltas are
// already in the input).
var taskUpdatedPattern = regexp.MustCompile(`^Updated task #(\S+)`)

// isTaskToolName reports whether the given tool name is one of
// the provider-native task tools we intercept. Used by both
// handleToolUse (suppress generic ToolStart) and the tool_result
// branch (route through the task parser). The body is defined
// next to applyTaskToolResult in task.go; resetTasksForNewTurn
// lives in stream.go next to streamState.
func isTaskToolName(name string) bool {
	switch name {
	case "TaskCreate", "TaskUpdate":
		return true
	}
	return false
}

// applyTaskToolResult inspects a tool_result block and, if the
// underlying tool is a task tool, either:
//
//   - emits a fully-populated agent.AgentEvent (EventAgentTaskCreate /
//     EventAgentTaskUpdate) on a confirmed success result;
//   - emits a generic EventAgentToolEnd with a degraded note so the
//     user still sees the operation in the thread on parse
//     failure or confirmed error;
//   - returns (false, nil) for a non-task tool so the caller
//     falls through to the existing generic ToolEnd emission.
//
// The bridge's per-session state (state.tasks, state.taskOrder,
// state.pendingTools, state.toolUseArgs) is mutated only on a
// confirmed success result; the corresponding pendingTool entry
// is removed on every code path (success, failure, unknown).
//
// The first successful TaskCreate of a session is emitted as
// EventAgentTaskCreate; subsequent TaskCreate / TaskUpdate / delete
// operations are emitted as EventAgentTaskUpdate, all carrying the
// current full snapshot.
func applyTaskToolResult(
	state *streamState,
	block contentBlock,
	logger *slog.Logger,
) (bool, *agent.AgentEvent) {
	if state == nil {
		return false, nil
	}
	pending, ok := state.pendingTools[block.ToolUseID]
	if !ok || !isTaskToolName(pending.Name) {
		return false, nil
	}
	// Always evict the pending record. A second matching result
	// would be a protocol anomaly; logging once is enough.
	delete(state.pendingTools, block.ToolUseID)
	delete(state.toolUseArgs, block.ToolUseID)

	if block.IsError {
		logTaskWarn(logger, "task tool result errored", pending.Name, block)
		return true, taskToolEndFallback(pending, block, "failed")
	}

	output := stringifyToolResult(block.Content)
	switch pending.Name {
	case "TaskCreate":
		id, subject, ok := parseTaskCreated(output)
		if !ok {
			logTaskWarn(logger, "TaskCreate result did not match expected success pattern", pending.Name, block)
			return true, taskToolEndFallback(pending, block, "unparseable result")
		}
		input := parseTaskCreateInput(pending.Input)
		state.tasks[id] = agent.AgentTaskItem{
			ID:         id,
			Subject:    coalesceNonEmpty(subject, input.Subject, "Task #"+id),
			ActiveForm: input.ActiveForm,
			Status:     agent.TaskPending,
		}
		state.taskOrder = upsertTaskOrder(state.taskOrder, id)
		ev := agent.AgentEvent{
			Kind:     agent.EventAgentTaskCreate,
			TaskList: snapshotTasks(state),
		}
		return true, &ev
	case "TaskUpdate":
		id, ok := parseTaskUpdatedID(output, pending.Input)
		if !ok {
			logTaskWarn(logger, "TaskUpdate result did not match expected success pattern", pending.Name, block)
			return true, taskToolEndFallback(pending, block, "unparseable result")
		}
		input := parseTaskUpdateInput(pending.Input)
		existing, exists := state.tasks[id]
		if !exists {
			existing = agent.AgentTaskItem{
				ID:      id,
				Subject: "Task #" + id,
				Status:  agent.TaskPending,
			}
		}
		updated := applyTaskUpdateFields(existing, input)
		if updated.Status == agent.TaskDeleted {
			delete(state.tasks, id)
			state.taskOrder = removeTaskOrder(state.taskOrder, id)
		} else {
			state.tasks[id] = updated
			state.taskOrder = upsertTaskOrder(state.taskOrder, id)
		}
		ev := agent.AgentEvent{
			Kind:     agent.EventAgentTaskUpdate,
			TaskList: snapshotTasks(state),
		}
		return true, &ev
	}
	return false, nil
}

func parseTaskCreated(output string) (id, subject string, ok bool) {
	m := taskCreatedPattern.FindStringSubmatch(strings.TrimSpace(output))
	if m == nil {
		return "", "", false
	}
	id = m[1]
	// The remainder of the line is the subject reminder; trim
	// trailing whitespace so the resulting snapshot subject
	// is clean even if Claude Code pads the line.
	subject = strings.TrimSpace(strings.TrimPrefix(output, m[0]))
	return id, subject, true
}

func parseTaskUpdatedID(output string, input []byte) (string, bool) {
	if m := taskUpdatedPattern.FindStringSubmatch(strings.TrimSpace(output)); m != nil {
		return m[1], true
	}
	// Fall back to the input's taskId when the success line shape
	// drifted but the operation completed. We still log elsewhere
	// on total parse failure.
	if in := parseTaskUpdateInput(input); in.TaskID != "" {
		return in.TaskID, true
	}
	return "", false
}

// taskCreateInput is the provider-native input shape for the
// TaskCreate tool. Only fields we need for the snapshot.
type taskCreateInput struct {
	Subject    string `json:"subject"`
	ActiveForm string `json:"activeForm"`
}

func parseTaskCreateInput(raw []byte) taskCreateInput {
	var in taskCreateInput
	if len(raw) == 0 {
		return in
	}
	// Best-effort decode; malformed input falls back to empty
	// fields and the snapshot row uses the ID-derived fallback.
	_ = decodeJSONBestEffort(raw, &in)
	return in
}

// taskUpdateInput is the provider-native input shape for the
// TaskUpdate tool. We accept pointers so we can distinguish
// "field absent" from "field present with zero value".
type taskUpdateInput struct {
	TaskID     string  `json:"taskId"`
	Subject    *string `json:"subject,omitempty"`
	ActiveForm *string `json:"activeForm,omitempty"`
	Status     *string `json:"status,omitempty"`
}

func parseTaskUpdateInput(raw []byte) taskUpdateInput {
	var in taskUpdateInput
	if len(raw) == 0 {
		return in
	}
	_ = decodeJSONBestEffort(raw, &in)
	return in
}

func applyTaskUpdateFields(prev agent.AgentTaskItem, in taskUpdateInput) agent.AgentTaskItem {
	if in.Subject != nil {
		prev.Subject = *in.Subject
	}
	if in.ActiveForm != nil {
		prev.ActiveForm = *in.ActiveForm
	}
	if in.Status != nil {
		prev.Status = taskStatusFromString(*in.Status)
	}
	return prev
}

func taskStatusFromString(s string) agent.AgentTaskStatus {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "in_progress", "inprogress", "in-progress":
		return agent.TaskInProgress
	case "completed", "done":
		return agent.TaskCompleted
	case "deleted", "removed":
		return agent.TaskDeleted
	case "pending", "":
		return agent.TaskPending
	}
	return agent.TaskPending
}

// taskToolEndFallback returns a synthetic EventAgentToolEnd so the
// task tool call still shows up in the user message thread when
// the result cannot drive a typed task event (parse failure or
// IsError=true). The bridge's existing thread reply path renders
// the result as `● TaskCreate(unparseable result)` so the user
// sees the operation happened even when the snapshot can't be
// applied. No TaskList payload is set, so the receipt card is
// not re-rendered.
func taskToolEndFallback(pending pendingTool, block contentBlock, reason string) *agent.AgentEvent {
	args := string(pending.Input)
	if len(args) > 500 {
		args = args[:500] + "…"
	}
	return &agent.AgentEvent{
		Kind: agent.EventAgentToolEnd,
		ToolEnd: &agent.AgentToolEndEvent{
			ID:     block.ToolUseID,
			Name:   pending.Name,
			Args:   args,
			Output: snippetToolResult(block.Content),
		},
		Err: fmt.Errorf("claudecode: task tool result %s", reason),
	}
}

func snapshotTasks(state *streamState) *agent.AgentTaskListEvent {
	items := make([]agent.AgentTaskItem, 0, len(state.taskOrder))
	for _, id := range state.taskOrder {
		if item, ok := state.tasks[id]; ok {
			items = append(items, item)
		}
	}
	return &agent.AgentTaskListEvent{Items: items}
}

func upsertTaskOrder(order []string, id string) []string {
	for _, existing := range order {
		if existing == id {
			return order
		}
	}
	return append(order, id)
}

func removeTaskOrder(order []string, id string) []string {
	for i, existing := range order {
		if existing == id {
			return append(order[:i], order[i+1:]...)
		}
	}
	return order
}

func coalesceNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func logTaskWarn(logger *slog.Logger, msg, toolName string, block contentBlock) {
	if logger == nil {
		return
	}
	logger.Warn("claudecode: "+msg,
		"tool_name", toolName,
		"tool_use_id", block.ToolUseID,
		"is_error", block.IsError,
		"output", snippetToolResult(block.Content),
	)
}

// snippetToolResult produces a short single-line preview of a
// tool_result's content for warn logs. Full content is
// intentionally not logged.
func snippetToolResult(content jsonRaw) string {
	s := stringifyToolResult(content)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
