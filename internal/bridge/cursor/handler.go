// handler.go — Cursor-specific ACP extension method handler.
//
// Cursor's ACP server emits 5 extension methods beyond the standard
// ACP protocol. This file implements the MethodHandler callback that
// intercepts these methods and maps them to AgentEvent types:
//
//   - cursor/update_todos  → EventAgentTaskUpdate (full snapshot)
//   - cursor/create_plan   → EventAgentTaskCreate + AskUserQuestion (accept / reject)
//   - cursor/task          → EventAgentToolStart/End (subagent notification)
//   - cursor/ask_question  → AskUserQuestion (blocks until the user answers)
//   - cursor/generate_image → EventAgentText (informational only)
package cursor

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/acp"
	"github.com/cnlangzi/nightme/internal/messages"
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
		return h.handleAskQuestion(params, respond)
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

const (
	planAccept = "Accept"
	planReject = "Reject"
	askOther   = "Other"
)

// ─── cursor/create_plan ───────────────────────────────────────────

// handleCreatePlan processes cursor/create_plan. The agent blocks
// until the client returns accepted / rejected / cancelled, so the
// decision goes out as an AskUserQuestion card. SendPermission
// writes the RPC result.
func (h *cursorMethodHandler) handleCreatePlan(params json.RawMessage, respond func(id json.RawMessage, result any, err error) bool) bool {
	var p struct {
		Name     string       `json:"name"`
		Overview string       `json:"overview"`
		Plan     string       `json:"plan"`
		Todos    []cursorTodo `json:"todos"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		cLog("CreatePlan: decode error", "err", err)
		if respond != nil {
			respond(nil, nil, err)
		}
		return true
	}
	if h.view == nil || h.view.AskUser == nil {
		return false
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
	name := p.Name
	if name == "" {
		name = "Plan"
	}
	body := p.Overview
	if p.Plan != "" {
		if body != "" {
			body += "\n\n"
		}
		body += p.Plan
	}
	question := "Accept this plan?"
	if body != "" {
		question += "\n" + trimRunes(body, 1500)
	}
	cLog("CreatePlan", "name", p.Name, "todos", len(items))
	return h.view.AskUser(&agent.AgentPermissionRequest{
		Tool:    "AskUserQuestion",
		Action:  formatAskAction(name, "Accept this plan?", false),
		Options: []string{planAccept, planReject},
		Questions: []agent.AgentPermissionQuestion{{
			ID:       "plan",
			Header:   name,
			Question: question,
			Options:  []string{planAccept, planReject},
		}},
		Kind: agent.PermissionKindQuestion,
	}, encodePlanAnswer)
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

// cursorQuestion is one cursor/ask_question item. Labels are what
// the AskUserQuestion card shows; ids go back on the RPC.
type cursorQuestion struct {
	ID            string
	Prompt        string
	AllowMultiple bool
	Options       []cursorOption
}

type cursorOption struct {
	ID    string
	Label string
}

// handleAskQuestion processes cursor/ask_question. The agent blocks
// on extMethod until outcome.answered / skipped / cancelled. The
// card is the same AskUserQuestion path dsh and claude use;
// SendPermission encodes the click into that outcome.
func (h *cursorMethodHandler) handleAskQuestion(params json.RawMessage, respond func(id json.RawMessage, result any, err error) bool) bool {
	var p struct {
		Title     string `json:"title"`
		Questions []struct {
			ID            string `json:"id"`
			Prompt        string `json:"prompt"`
			AllowMultiple bool   `json:"allowMultiple"`
			Options       []struct {
				ID    string `json:"id"`
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		cLog("AskQuestion: decode error", "err", err)
		if respond != nil {
			respond(nil, nil, err)
		}
		return true
	}
	qs := make([]cursorQuestion, 0, len(p.Questions))
	for _, q := range p.Questions {
		item := cursorQuestion{ID: q.ID, Prompt: q.Prompt, AllowMultiple: q.AllowMultiple}
		for _, o := range q.Options {
			label := o.Label
			if label == "" {
				label = o.ID
			}
			item.Options = append(item.Options, cursorOption{ID: o.ID, Label: label})
		}
		qs = append(qs, item)
	}
	shown := make([]cursorQuestion, 0, len(qs))
	for _, q := range qs {
		// Claude drops a question with fewer than two options
		// rather than rendering a card the user cannot answer.
		if q.Prompt == "" || len(q.Options) < 2 {
			cLog("AskQuestion: skipped", "id", q.ID, "options", len(q.Options))
			continue
		}
		shown = append(shown, q)
	}
	if len(shown) == 0 {
		cLog("AskQuestion: nothing to ask", "title", p.Title)
		if respond != nil {
			respond(nil, askOutcome("skipped", "No questions", nil), nil)
		}
		return true
	}
	if h.view == nil || h.view.AskUser == nil {
		return false
	}
	permQs := make([]agent.AgentPermissionQuestion, len(shown))
	var action strings.Builder
	for i, q := range shown {
		header := p.Title
		if header == "" || i > 0 {
			header = headerFromPrompt(q.Prompt)
		}
		labels := askLabels(q.Options)
		if i > 0 {
			action.WriteByte('\n')
		}
		action.WriteString(formatAskAction(header, q.Prompt, q.AllowMultiple))
		permQs[i] = agent.AgentPermissionQuestion{
			ID:       q.ID,
			Header:   header,
			Question: q.Prompt,
			Options:  labels,
		}
	}
	cLog("AskQuestion", "title", p.Title, "questions", len(shown))
	return h.view.AskUser(&agent.AgentPermissionRequest{
		Tool:      "AskUserQuestion",
		Action:    action.String(),
		Options:   permQs[0].Options,
		Questions: permQs,
		Kind:      agent.PermissionKindQuestion,
	}, func(answer string) (any, error) {
		return encodeAskAnswer(shown, answer)
	})
}

// askLabels is the button list Claude's AskUserQuestion card uses:
// visible labels, "(Recommended)" removed, plus a synthetic Other
// so the user can type an answer the option list does not contain.
func askLabels(opts []cursorOption) []string {
	labels := make([]string, 0, len(opts)+1)
	hasOther := false
	for _, o := range opts {
		label := cleanAskLabel(o.Label)
		if label == "" {
			label = o.ID
		}
		if label == askOther {
			hasOther = true
		}
		labels = append(labels, label)
	}
	if !hasOther {
		labels = append(labels, askOther)
	}
	return labels
}

func cleanAskLabel(label string) string {
	label = strings.TrimSuffix(label, " (Recommended)")
	label = strings.TrimSuffix(label, "(Recommended)")
	return strings.TrimSpace(label)
}

// formatAskAction matches claudecode.formatQuestionAction:
// "[header]" plus the question, with " (multi-select)" when the
// question accepts more than one option.
func formatAskAction(header, question string, multi bool) string {
	if header == "" {
		header = "Question"
	}
	if multi {
		header += " (multi-select)"
	}
	return "[" + header + "] " + question
}

func headerFromPrompt(prompt string) string {
	prompt = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(prompt), "?"))
	words := strings.Fields(prompt)
	if len(words) > 4 {
		words = words[:4]
	}
	header := strings.Join(words, " ")
	if len([]rune(header)) > 30 {
		header = string([]rune(header)[:30])
	}
	if header == "" {
		return "Question"
	}
	return header
}

func encodeAskAnswer(qs []cursorQuestion, answer string) (any, error) {
	picks, err := messages.DecodeQuestionPicks(answer)
	if err != nil {
		return nil, err
	}
	if picks == nil {
		picks = plainAskPicks(qs, answer)
	}
	byID := make(map[string]messages.QuestionPick, len(picks))
	for _, p := range picks {
		byID[p.ID] = p
	}
	var answers []map[string]any
	var custom string
	for _, q := range qs {
		p := byID[q.ID]
		if p.Custom != "" && custom == "" {
			custom = p.Custom
		}
		ids := optionIDs(q, p.Selected)
		if len(ids) == 0 && p.Custom != "" {
			ids = optionIDs(q, []string{p.Custom})
		}
		if len(ids) == 0 {
			// Cursor's answered schema only carries option ids.
			// "Other" and typed text have nowhere to go except
			// the skipped reason, which is what the model reads.
			if custom == "" {
				custom = freeText(p)
			}
			continue
		}
		answers = append(answers, map[string]any{
			"questionId":        q.ID,
			"selectedOptionIds": ids,
		})
	}
	if len(answers) == 0 {
		reason := "User skipped questions"
		if custom != "" {
			reason = custom
		}
		return askOutcome("skipped", reason, nil), nil
	}
	return askOutcome("answered", "", answers), nil
}

func freeText(p messages.QuestionPick) string {
	if p.Custom != "" {
		return p.Custom
	}
	for _, s := range p.Selected {
		if strings.TrimSpace(s) == askOther {
			return askOther
		}
	}
	return ""
}

func plainAskPicks(qs []cursorQuestion, answer string) []messages.QuestionPick {
	answer = strings.TrimSpace(answer)
	if answer == "" || len(qs) == 0 {
		return nil
	}
	// Claude joins a multi-select answer with commas.
	parts := []string{answer}
	if qs[0].AllowMultiple && strings.Contains(answer, ",") {
		parts = splitComma(answer)
	}
	if len(optionIDs(qs[0], parts)) > 0 {
		return []messages.QuestionPick{{ID: qs[0].ID, Selected: parts}}
	}
	for _, q := range qs[1:] {
		if len(optionIDs(q, []string{answer})) > 0 {
			return []messages.QuestionPick{{ID: q.ID, Selected: []string{answer}}}
		}
	}
	return []messages.QuestionPick{{ID: qs[0].ID, Custom: answer}}
}

func splitComma(s string) []string {
	raw := strings.Split(s, ",")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func optionIDs(q cursorQuestion, selected []string) []string {
	var ids []string
	for _, s := range selected {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		cleaned := cleanAskLabel(s)
		for _, o := range q.Options {
			label := cleanAskLabel(o.Label)
			if label == "" {
				label = o.ID
			}
			if s == o.ID || s == o.Label || cleaned == label {
				if o.ID != "" {
					ids = append(ids, o.ID)
				}
				break
			}
		}
		if !q.AllowMultiple {
			break
		}
	}
	return ids
}

func askOutcome(outcome, reason string, answers []map[string]any) map[string]any {
	o := map[string]any{"outcome": outcome}
	if reason != "" {
		o["reason"] = reason
	}
	if len(answers) > 0 {
		o["answers"] = answers
	}
	return map[string]any{"outcome": o}
}

func encodePlanAnswer(answer string) (any, error) {
	picks, err := messages.DecodeQuestionPicks(answer)
	if err != nil {
		return nil, err
	}
	choice := strings.TrimSpace(answer)
	custom := ""
	if picks != nil {
		choice = ""
		if len(picks) > 0 {
			if picks[0].Custom != "" {
				custom = picks[0].Custom
			} else if len(picks[0].Selected) > 0 {
				choice = picks[0].Selected[0]
			}
		}
	}
	text := choice
	if text == "" {
		text = custom
	}
	switch strings.ToLower(strings.TrimSpace(text)) {
	case strings.ToLower(planAccept):
		return askOutcome("accepted", "", nil), nil
	case "":
		return askOutcome("cancelled", "User cancelled", nil), nil
	case strings.ToLower(planReject):
		return askOutcome("rejected", "User rejected plan", nil), nil
	default:
		return askOutcome("rejected", text, nil), nil
	}
}

func trimRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
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
