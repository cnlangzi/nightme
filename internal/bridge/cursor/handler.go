// handler.go — Cursor-specific ACP extension method handler.
//
// Cursor's ACP server emits 5 extension methods beyond the standard
// ACP protocol. This file implements the MethodHandler callback that
// intercepts these methods and maps them to AgentEvent types:
//
//   - cursor/update_todos  → EventAgentTaskUpdate (full snapshot)
//   - cursor/create_plan   → EventAgentTaskCreate (blocking, approval required)
//   - cursor/task          → EventAgentToolStart/End (subagent notification)
//   - cursor/ask_question  → EventAgentPermission (multi-choice question)
//   - cursor/generate_image → EventAgentText (informational only)
package cursor

import (
	"encoding/json"
	"fmt"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/acp"
)

// NewMethodHandler returns an acp.MethodHandler that intercepts
// Cursor-specific extension methods and emits the corresponding
// AgentEvents via the SessionView.
func NewMethodHandler(view *acp.SessionView) acp.MethodHandler {
	return newCursorMethodHandler(view)
}

// cursorMethodHandler is the concrete implementation that intercepts
// Cursor extension methods. Created by newCursorMethodHandler.
type cursorMethodHandler struct {
	view *acp.SessionView
}

// newCursorMethodHandler returns an acp.MethodHandler closure bound
// to the given SessionView. The closure intercepts cursor/* methods
// and emits AgentEvents via view.Emit.
func newCursorMethodHandler(view *acp.SessionView) acp.MethodHandler {
	h := &cursorMethodHandler{view: view}
	return h.handle
}

// handle is the dispatch entry point for Cursor extension methods.
// Returns true if the method was handled, false otherwise.
func (h *cursorMethodHandler) handle(method string, params json.RawMessage, respond func(id json.RawMessage, result any, err error) bool) bool {
	cLog("MethodHandler", "method", method)
	switch method {
	case "cursor/update_todos":
		return h.handleUpdateTodos(params)
	case "cursor/create_plan":
		return h.handleCreatePlan(params, respond)
	case "cursor/task":
		return h.handleTask(params)
	case "cursor/ask_question":
		return h.handleAskQuestion(params)
	case "cursor/generate_image":
		return h.handleGenerateImage(params)
	default:
		return false
	}
}

// ─── cursor/update_todos ──────────────────────────────────────────

// cursorTodo is the JSON structure for a single todo item in
// cursor/update_todos and cursor/create_plan.
type cursorTodo struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"` // pending | in_progress | completed | cancelled
}

// handleUpdateTodos processes cursor/update_todos notifications.
// The todos array is the full current snapshot (or a merge if
// merge=true). We emit EventAgentTaskUpdate with the complete
// converted list.
func (h *cursorMethodHandler) handleUpdateTodos(params json.RawMessage) bool {
	var p struct {
		Todos []cursorTodo `json:"todos"`
		Merge bool         `json:"merge"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		cLog("UpdateTodos: decode error", "err", err)
		return true // handled but malformed — don't fall through
	}
	items := make([]agent.AgentTaskItem, 0, len(p.Todos))
	for _, t := range p.Todos {
		items = append(items, agent.AgentTaskItem{
			ID:      t.ID,
			Subject: t.Content,
			Status:  cursorStatusToAgent(t.Status),
		})
	}
	h.view.Emit(agent.AgentEvent{
		Kind: agent.EventAgentTaskUpdate,
		TaskList: &agent.AgentTaskListEvent{
			Items: items,
		},
	})
	cLog("UpdateTodos", "count", len(items), "merge", p.Merge)
	return true
}

// ─── cursor/create_plan ───────────────────────────────────────────

// handleCreatePlan processes cursor/create_plan blocking methods.
// This emits EventAgentTaskCreate with the plan's todos, then
// responds with approval (the plan is accepted).
func (h *cursorMethodHandler) handleCreatePlan(params json.RawMessage, respond func(id json.RawMessage, result any, err error) bool) bool {
	var p struct {
		Name     string       `json:"name"`
		Overview string       `json:"overview"`
		Plan     string       `json:"plan"`
		Todos    []cursorTodo `json:"todos"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		cLog("CreatePlan: decode error", "err", err)
		return true
	}
	items := make([]agent.AgentTaskItem, 0, len(p.Todos))
	for _, t := range p.Todos {
		items = append(items, agent.AgentTaskItem{
			ID:      t.ID,
			Subject: t.Content,
			Status:  cursorStatusToAgent(t.Status),
		})
	}
	h.view.Emit(agent.AgentEvent{
		Kind: agent.EventAgentTaskCreate,
		TaskList: &agent.AgentTaskListEvent{
			Items: items,
		},
	})
	cLog("CreatePlan", "name", p.Name, "todos", len(items))
	// Respond with approval — the plan is accepted.
	respond(nil, map[string]any{"approved": true}, nil)
	return true
}

// ─── cursor/task ──────────────────────────────────────────────────

// handleTask processes cursor/task notifications. This is a subagent
// completion notification. We emit EventAgentToolEnd to indicate
// the subagent task finished.
func (h *cursorMethodHandler) handleTask(params json.RawMessage) bool {
	var p struct {
		Description string `json:"description"`
		Prompt      string `json:"prompt"`
		DurationMs  int64  `json:"durationMs"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		cLog("Task: decode error", "err", err)
		return true
	}
	h.view.Emit(agent.AgentEvent{
		Kind: agent.EventAgentToolEnd,
		ToolEnd: &agent.AgentToolEndEvent{
			ID:   fmt.Sprintf("cursor-task-%s", p.Description),
			Name: "cursor/task",
		},
	})
	cLog("Task", "description", p.Description, "duration_ms", p.DurationMs)
	return true
}

// ─── cursor/ask_question ──────────────────────────────────────────

// handleAskQuestion processes cursor/ask_question blocking methods.
// Maps to EventAgentPermission so the runtime can render a
// permission card with the question options.
func (h *cursorMethodHandler) handleAskQuestion(params json.RawMessage) bool {
	var p struct {
		Title     string `json:"title"`
		Questions []struct {
			ID      string `json:"id"`
			Prompt  string `json:"prompt"`
			Options []struct {
				ID    string `json:"id"`
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		cLog("AskQuestion: decode error", "err", err)
		return true
	}
	// Emit permission request for the first question (most common
	// pattern is a single question with multiple options).
	if len(p.Questions) > 0 {
		q := p.Questions[0]
		options := make([]string, 0, len(q.Options))
		for _, o := range q.Options {
			options = append(options, o.ID)
		}
		h.view.Emit(agent.AgentEvent{
			Kind: agent.EventAgentPermission,
			Permission: &agent.AgentPermissionRequest{
				Tool:    "cursor/ask_question",
				Action:  q.Prompt,
				Options: options,
			},
		})
	}
	cLog("AskQuestion", "title", p.Title, "questions", len(p.Questions))
	return true
}

// ─── cursor/generate_image ────────────────────────────────────────

// handleGenerateImage processes cursor/generate_image notifications.
// We emit a text event with the description since the runtime
// doesn't have native image rendering support yet.
func (h *cursorMethodHandler) handleGenerateImage(params json.RawMessage) bool {
	var p struct {
		Description string `json:"description"`
		FilePath    string `json:"filePath"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		cLog("GenerateImage: decode error", "err", err)
		return true
	}
	text := fmt.Sprintf("[Image generated: %s]", p.Description)
	if p.FilePath != "" {
		text = fmt.Sprintf("[Image generated: %s → %s]", p.Description, p.FilePath)
	}
	h.view.Emit(agent.AgentEvent{
		Kind: agent.EventAgentText,
		Text: text,
	})
	cLog("GenerateImage", "description", p.Description, "path", p.FilePath)
	return true
}

// ─── helpers ──────────────────────────────────────────────────────

// cursorStatusToAgent maps Cursor's todo status strings to the
// internal AgentTaskStatus constants.
func cursorStatusToAgent(status string) agent.AgentTaskStatus {
	switch status {
	case "in_progress":
		return agent.TaskInProgress
	case "completed":
		return agent.TaskCompleted
	case "cancelled":
		return agent.TaskCancelled
	default:
		return agent.TaskPending
	}
}
