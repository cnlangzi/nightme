package cursor

import (
	"encoding/json"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/acp"
	"github.com/cnlangzi/nightme/internal/messages"
)

// mockEmit captures AgentEvents emitted by the handler.
type mockEmit struct {
	events []agent.AgentEvent
	asks   []*agent.AgentPermissionRequest
}

func (m *mockEmit) emit(ev agent.AgentEvent) {
	m.events = append(m.events, ev)
}

func (m *mockEmit) askUser(req *agent.AgentPermissionRequest, _ func(string) (any, error)) bool {
	m.asks = append(m.asks, req)
	m.emit(agent.AgentEvent{Kind: agent.EventAgentPermission, Permission: req})
	return true
}

// newTestHandler creates a handler bound to a mock SessionView.
// Returns the handler and a mock to inspect emitted events.
func newTestHandler() (acp.MethodHandler, *mockEmit) {
	m := &mockEmit{}
	view := &acp.SessionView{
		Emit:    m.emit,
		AskUser: m.askUser,
	}
	return newCursorMethodHandler(view), m
}

// ─── cursor/update_todos ──────────────────────────────────────────

func TestHandleUpdateTodos_EmptyList(t *testing.T) {
	h, m := newTestHandler()
	params := `{"todos":[],"merge":false}`
	handled := h("cursor/update_todos", json.RawMessage(params), nil)
	if !handled {
		t.Fatal("expected handled=true")
	}
	if len(m.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(m.events))
	}
	ev := m.events[0]
	if ev.Kind != agent.EventAgentTaskUpdate {
		t.Fatalf("expected EventAgentTaskUpdate, got %v", ev.Kind)
	}
	if len(ev.TaskList.Items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(ev.TaskList.Items))
	}
}

func TestHandleUpdateTodos_ThreeItems(t *testing.T) {
	h, m := newTestHandler()
	params := `{"todos":[
		{"id":"1","content":"实现 A","status":"pending"},
		{"id":"2","content":"实现 B","status":"in_progress"},
		{"id":"3","content":"实现 C","status":"completed"}
	],"merge":true}`
	handled := h("cursor/update_todos", json.RawMessage(params), nil)
	if !handled {
		t.Fatal("expected handled=true")
	}
	ev := m.events[0]
	if len(ev.TaskList.Items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(ev.TaskList.Items))
	}
	// Verify status mapping
	if ev.TaskList.Items[0].Status != agent.TaskPending {
		t.Errorf("item 0: expected TaskPending, got %v", ev.TaskList.Items[0].Status)
	}
	if ev.TaskList.Items[1].Status != agent.TaskInProgress {
		t.Errorf("item 1: expected TaskInProgress, got %v", ev.TaskList.Items[1].Status)
	}
	if ev.TaskList.Items[2].Status != agent.TaskCompleted {
		t.Errorf("item 2: expected TaskCompleted, got %v", ev.TaskList.Items[2].Status)
	}
	// Verify content mapping
	if ev.TaskList.Items[0].Subject != "实现 A" {
		t.Errorf("item 0: expected '实现 A', got %q", ev.TaskList.Items[0].Subject)
	}
	if ev.TaskList.Items[0].ID != "1" {
		t.Errorf("item 0: expected ID '1', got %q", ev.TaskList.Items[0].ID)
	}
}

func TestHandleUpdateTodos_CancelledStatus(t *testing.T) {
	h, m := newTestHandler()
	params := `{"todos":[{"id":"x","content":"cancelled task","status":"cancelled"}]}`
	h("cursor/update_todos", json.RawMessage(params), nil)
	if len(m.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(m.events))
	}
	if m.events[0].TaskList.Items[0].Status != agent.TaskCancelled {
		t.Errorf("expected TaskCancelled, got %v", m.events[0].TaskList.Items[0].Status)
	}
}

func TestHandleUpdateTodos_MalformedJSON(t *testing.T) {
	h, m := newTestHandler()
	handled := h("cursor/update_todos", json.RawMessage(`{bad json`), nil)
	if !handled {
		t.Fatal("expected handled=true even for malformed JSON")
	}
	if len(m.events) != 0 {
		t.Fatalf("expected 0 events for malformed JSON, got %d", len(m.events))
	}
}

// ─── cursor/create_plan ───────────────────────────────────────────

func TestHandleCreatePlan_EmitsTaskCreateAndResponds(t *testing.T) {
	h, m := newTestHandler()
	params := `{"name":"重构计划","overview":"重构 auth 模块","plan":"## 步骤\n1. 拆分文件","todos":[
		{"id":"p1","content":"拆分 auth.go","status":"pending"},
		{"id":"p2","content":"编写测试","status":"pending"}
	]}`
	var respondCalled bool
	respond := func(id json.RawMessage, result any, err error) bool {
		respondCalled = true
		return true
	}
	handled := h("cursor/create_plan", json.RawMessage(params), respond)
	if !handled {
		t.Fatal("expected handled=true")
	}
	if respondCalled {
		t.Fatal("create_plan must wait for AskUser, not auto-approve")
	}
	if len(m.events) != 2 {
		t.Fatalf("expected task + question, got %d", len(m.events))
	}
	ev := m.events[0]
	if ev.Kind != agent.EventAgentTaskCreate {
		t.Fatalf("expected EventAgentTaskCreate, got %v", ev.Kind)
	}
	if len(ev.TaskList.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(ev.TaskList.Items))
	}
	if ev.TaskList.Items[0].Subject != "拆分 auth.go" {
		t.Errorf("expected '拆分 auth.go', got %q", ev.TaskList.Items[0].Subject)
	}
	q := m.events[1]
	if q.Kind != agent.EventAgentPermission || q.Permission == nil {
		t.Fatalf("expected AskUser permission, got %+v", q)
	}
	if q.Permission.Kind != agent.PermissionKindQuestion {
		t.Errorf("kind = %q, want question", q.Permission.Kind)
	}
	if q.Permission.Tool != "AskUserQuestion" {
		t.Errorf("tool = %q, want AskUserQuestion", q.Permission.Tool)
	}
	if q.Permission.Action != "[重构计划] Accept this plan?" {
		t.Errorf("action = %q", q.Permission.Action)
	}
	if len(q.Permission.Options) != 2 || q.Permission.Options[0] != planAccept || q.Permission.Options[1] != planReject {
		t.Errorf("options = %v", q.Permission.Options)
	}
}

func TestHandleCreatePlan_MalformedJSON(t *testing.T) {
	h, m := newTestHandler()
	handled := h("cursor/create_plan", json.RawMessage(`{bad`), nil)
	if !handled {
		t.Fatal("expected handled=true even for malformed JSON")
	}
	if len(m.events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(m.events))
	}
}

// ─── cursor/task ──────────────────────────────────────────────────

func TestHandleTask_EmitsToolEnd(t *testing.T) {
	h, m := newTestHandler()
	params := `{"description":"subagent explore","prompt":"find auth files","durationMs":1234}`
	handled := h("cursor/task", json.RawMessage(params), nil)
	if !handled {
		t.Fatal("expected handled=true")
	}
	if len(m.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(m.events))
	}
	ev := m.events[0]
	if ev.Kind != agent.EventAgentToolEnd {
		t.Fatalf("expected EventAgentToolEnd, got %v", ev.Kind)
	}
	if ev.ToolEnd.Name != "cursor/task" {
		t.Errorf("expected tool name 'cursor/task', got %q", ev.ToolEnd.Name)
	}
	if ev.ToolEnd.ID != "cursor-task-subagent explore" {
		t.Errorf("expected ID 'cursor-task-subagent explore', got %q", ev.ToolEnd.ID)
	}
}

func TestHandleTask_MalformedJSON(t *testing.T) {
	h, m := newTestHandler()
	handled := h("cursor/task", json.RawMessage(`{bad`), nil)
	if !handled {
		t.Fatal("expected handled=true even for malformed JSON")
	}
	if len(m.events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(m.events))
	}
}

// ─── cursor/ask_question ──────────────────────────────────────────

func TestHandleAskQuestion_EmitsPermission(t *testing.T) {
	h, m := newTestHandler()
	params := `{"title":"选择模型","questions":[{"id":"q1","prompt":"使用哪个模型?","options":[
		{"id":"gpt4","label":"GPT-4 (Recommended)"},
		{"id":"claude","label":"Claude"},
		{"id":"gemini","label":"Gemini"}
	]}]}`
	handled := h("cursor/ask_question", json.RawMessage(params), nil)
	if !handled {
		t.Fatal("expected handled=true")
	}
	if len(m.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(m.events))
	}
	ev := m.events[0]
	if ev.Kind != agent.EventAgentPermission {
		t.Fatalf("expected EventAgentPermission, got %v", ev.Kind)
	}
	if ev.Permission.Tool != "AskUserQuestion" {
		t.Errorf("expected tool 'AskUserQuestion', got %q", ev.Permission.Tool)
	}
	if ev.Permission.Action != "[选择模型] 使用哪个模型?" {
		t.Errorf("action = %q", ev.Permission.Action)
	}
	if ev.Permission.Kind != agent.PermissionKindQuestion {
		t.Errorf("kind = %q, want question", ev.Permission.Kind)
	}
	if len(ev.Permission.Questions) != 1 || ev.Permission.Questions[0].ID != "q1" {
		t.Fatalf("questions = %+v", ev.Permission.Questions)
	}
	opts := ev.Permission.Options
	if len(opts) != 4 || opts[0] != "GPT-4" || opts[1] != "Claude" || opts[2] != "Gemini" || opts[3] != askOther {
		t.Fatalf("options = %v", opts)
	}
}

func TestHandleAskQuestion_SkipsThinQuestions(t *testing.T) {
	h, m := newTestHandler()
	params := `{"questions":[{"id":"q1","prompt":"only one?","options":[{"id":"a","label":"A"}]}]}`
	var got any
	respond := func(id json.RawMessage, result any, err error) bool {
		got = result
		return true
	}
	if !h("cursor/ask_question", json.RawMessage(params), respond) {
		t.Fatal("expected handled=true")
	}
	if len(m.events) != 0 {
		t.Fatalf("expected no card, got %d events", len(m.events))
	}
	out := got.(map[string]any)["outcome"].(map[string]any)
	if out["outcome"] != "skipped" {
		t.Fatalf("outcome = %v", out)
	}
}

func TestHandleAskQuestion_NoQuestions(t *testing.T) {
	h, m := newTestHandler()
	params := `{"title":"empty","questions":[]}`
	handled := h("cursor/ask_question", json.RawMessage(params), nil)
	if !handled {
		t.Fatal("expected handled=true")
	}
	// No questions → no permission event emitted
	if len(m.events) != 0 {
		t.Fatalf("expected 0 events for empty questions, got %d", len(m.events))
	}
}

func TestHandleAskQuestion_MalformedJSON(t *testing.T) {
	h, m := newTestHandler()
	handled := h("cursor/ask_question", json.RawMessage(`{bad`), nil)
	if !handled {
		t.Fatal("expected handled=true even for malformed JSON")
	}
	if len(m.events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(m.events))
	}
}

// ─── cursor/generate_image ────────────────────────────────────────

func TestHandleGenerateImage_WithFilePath(t *testing.T) {
	h, m := newTestHandler()
	params := `{"description":"架构图","filePath":"/tmp/arch.png"}`
	handled := h("cursor/generate_image", json.RawMessage(params), nil)
	if !handled {
		t.Fatal("expected handled=true")
	}
	if len(m.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(m.events))
	}
	ev := m.events[0]
	if ev.Kind != agent.EventAgentText {
		t.Fatalf("expected EventAgentText, got %v", ev.Kind)
	}
	if ev.Text != "[Image generated: 架构图 → /tmp/arch.png]" {
		t.Errorf("unexpected text: %q", ev.Text)
	}
}

func TestHandleGenerateImage_WithoutFilePath(t *testing.T) {
	h, m := newTestHandler()
	params := `{"description":"截图"}`
	h("cursor/generate_image", json.RawMessage(params), nil)
	ev := m.events[0]
	if ev.Text != "[Image generated: 截图]" {
		t.Errorf("unexpected text: %q", ev.Text)
	}
}

func TestHandleGenerateImage_MalformedJSON(t *testing.T) {
	h, m := newTestHandler()
	handled := h("cursor/generate_image", json.RawMessage(`{bad`), nil)
	if !handled {
		t.Fatal("expected handled=true even for malformed JSON")
	}
	if len(m.events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(m.events))
	}
}

// ─── dispatch / unknown method ────────────────────────────────────

func TestHandle_UnknownMethod_ReturnsFalse(t *testing.T) {
	h, _ := newTestHandler()
	handled := h("cursor/unknown_method", json.RawMessage(`{}`), nil)
	if handled {
		t.Fatal("expected handled=false for unknown method")
	}
}

func TestHandle_StandardMethod_ReturnsFalse(t *testing.T) {
	h, _ := newTestHandler()
	// Standard ACP methods should not be intercepted
	handled := h("session/update", json.RawMessage(`{}`), nil)
	if handled {
		t.Fatal("expected handled=false for standard ACP method")
	}
}

// ─── cursorStatusToAgent ──────────────────────────────────────────

func TestEncodeAskAnswer_LabelAndSkip(t *testing.T) {
	qs := []cursorQuestion{{
		ID:     "q1",
		Prompt: "which?",
		Options: []cursorOption{
			{ID: "gpt4", Label: "GPT-4"},
			{ID: "claude", Label: "Claude"},
		},
	}}
	got, err := encodeAskAnswer(qs, "GPT-4")
	if err != nil {
		t.Fatal(err)
	}
	out := got.(map[string]any)["outcome"].(map[string]any)
	if out["outcome"] != "answered" {
		t.Fatalf("outcome = %v", out)
	}
	answers := out["answers"].([]map[string]any)
	ids := answers[0]["selectedOptionIds"].([]string)
	if answers[0]["questionId"] != "q1" || len(ids) != 1 || ids[0] != "gpt4" {
		t.Fatalf("answers = %v", answers)
	}

	skipped, err := encodeAskAnswer(qs, messages.EncodeQuestionPicks([]messages.QuestionPick{{
		ID: "q1", Selected: []string{},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	if skipped.(map[string]any)["outcome"].(map[string]any)["outcome"] != "skipped" {
		t.Fatalf("skip = %v", skipped)
	}

	other, err := encodeAskAnswer(qs, askOther)
	if err != nil {
		t.Fatal(err)
	}
	o := other.(map[string]any)["outcome"].(map[string]any)
	if o["outcome"] != "skipped" || o["reason"] != askOther {
		t.Fatalf("other = %v", o)
	}

	rec, err := encodeAskAnswer([]cursorQuestion{{
		ID: "q1",
		Options: []cursorOption{
			{ID: "gpt4", Label: "GPT-4 (Recommended)"},
			{ID: "claude", Label: "Claude"},
		},
	}}, "GPT-4")
	if err != nil {
		t.Fatal(err)
	}
	ids = rec.(map[string]any)["outcome"].(map[string]any)["answers"].([]map[string]any)[0]["selectedOptionIds"].([]string)
	if len(ids) != 1 || ids[0] != "gpt4" {
		t.Fatalf("recommended label = %v", ids)
	}

	multi, err := encodeAskAnswer([]cursorQuestion{{
		ID:            "q1",
		AllowMultiple: true,
		Options: []cursorOption{
			{ID: "gpt4", Label: "GPT-4"},
			{ID: "claude", Label: "Claude"},
		},
	}}, "GPT-4, Claude")
	if err != nil {
		t.Fatal(err)
	}
	ids = multi.(map[string]any)["outcome"].(map[string]any)["answers"].([]map[string]any)[0]["selectedOptionIds"].([]string)
	if len(ids) != 2 || ids[0] != "gpt4" || ids[1] != "claude" {
		t.Fatalf("multi = %v", ids)
	}
}

func TestEncodePlanAnswer(t *testing.T) {
	acc, err := encodePlanAnswer(planAccept)
	if err != nil {
		t.Fatal(err)
	}
	if acc.(map[string]any)["outcome"].(map[string]any)["outcome"] != "accepted" {
		t.Fatalf("accept = %v", acc)
	}
	rej, err := encodePlanAnswer(planReject)
	if err != nil {
		t.Fatal(err)
	}
	if rej.(map[string]any)["outcome"].(map[string]any)["outcome"] != "rejected" {
		t.Fatalf("reject = %v", rej)
	}
	cancel, err := encodePlanAnswer(messages.EncodeQuestionPicks([]messages.QuestionPick{{
		ID: "plan", Selected: []string{},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	if cancel.(map[string]any)["outcome"].(map[string]any)["outcome"] != "cancelled" {
		t.Fatalf("cancel = %v", cancel)
	}
}

func TestCursorStatusToAgent(t *testing.T) {
	tests := []struct {
		input string
		want  agent.AgentTaskStatus
	}{
		{"pending", agent.TaskPending},
		{"in_progress", agent.TaskInProgress},
		{"completed", agent.TaskCompleted},
		{"cancelled", agent.TaskCancelled},
		{"unknown", agent.TaskPending}, // fallback
		{"", agent.TaskPending},        // empty → pending
	}
	for _, tt := range tests {
		got := cursorStatusToAgent(tt.input)
		if got != tt.want {
			t.Errorf("cursorStatusToAgent(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
