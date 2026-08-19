package cursor

import (
	"encoding/json"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/acp"
)

// mockEmit captures AgentEvents emitted by the handler.
type mockEmit struct {
	events []agent.AgentEvent
}

func (m *mockEmit) emit(ev agent.AgentEvent) {
	m.events = append(m.events, ev)
}

// newTestHandler creates a handler bound to a mock SessionView.
// Returns the handler and a mock to inspect emitted events.
func newTestHandler() (acp.MethodHandler, *mockEmit) {
	m := &mockEmit{}
	view := &acp.SessionView{
		Emit: m.emit,
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
	var respondResult any
	respond := func(id json.RawMessage, result any, err error) bool {
		respondCalled = true
		respondResult = result
		return true
	}
	handled := h("cursor/create_plan", json.RawMessage(params), respond)
	if !handled {
		t.Fatal("expected handled=true")
	}
	// Verify event
	if len(m.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(m.events))
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
	// Verify respond was called with approval
	if !respondCalled {
		t.Fatal("expected respond to be called")
	}
	result, ok := respondResult.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", respondResult)
	}
	if result["approved"] != true {
		t.Errorf("expected approved=true, got %v", result["approved"])
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
		{"id":"gpt4","label":"GPT-4"},
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
	if ev.Permission.Tool != "cursor/ask_question" {
		t.Errorf("expected tool 'cursor/ask_question', got %q", ev.Permission.Tool)
	}
	if ev.Permission.Action != "使用哪个模型?" {
		t.Errorf("expected action '使用哪个模型?', got %q", ev.Permission.Action)
	}
	if len(ev.Permission.Options) != 3 {
		t.Fatalf("expected 3 options, got %d", len(ev.Permission.Options))
	}
	if ev.Permission.Options[0] != "gpt4" || ev.Permission.Options[1] != "claude" || ev.Permission.Options[2] != "gemini" {
		t.Errorf("unexpected options: %v", ev.Permission.Options)
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
