package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// readFixture loads a JSON fixture from testdata/.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// collectEvents drains `events` until `count` events arrive or timeout.
// Returns the collected events in arrival order.
func collectEvents(t *testing.T, events <-chan agent.AgentEvent, count int, timeout time.Duration) []agent.AgentEvent {
	t.Helper()
	out := make([]agent.AgentEvent, 0, count)
	deadline := time.After(timeout)
	for len(out) < count {
		select {
		case ev, ok := <-events:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("timeout: got %d/%d events", len(out), count)
		}
	}
	return out
}

// streamFromFixture runs pumpStream against a fixture as if it were
// the child's stdout. Captures the events it emits.
func streamFromFixture(t *testing.T, name string, askHandler askHandlerFunc) []agent.AgentEvent {
	t.Helper()
	data := readFixture(t, name)
	events := make(chan agent.AgentEvent, 16)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pumpStream(strings.NewReader(string(data)), events, askHandler, nil)
		close(events)
	}()
	var got []agent.AgentEvent
	for ev := range events {
		got = append(got, ev)
	}
	wg.Wait()
	return got
}

// --- Stream translation tests ---

func TestPumpStream_Init(t *testing.T) {
	evs := streamFromFixture(t, "init.json", nil)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Kind != agent.EventText {
		t.Errorf("event kind = %v, want EventText", evs[0].Kind)
	}
	if !strings.Contains(evs[0].Text, "session initialized") {
		t.Errorf("text = %q, want to contain 'session initialized'", evs[0].Text)
	}
}

func TestPumpStream_TextChunk(t *testing.T) {
	evs := streamFromFixture(t, "text_chunk.json", nil)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Kind != agent.EventText {
		t.Errorf("event kind = %v, want EventText", evs[0].Kind)
	}
	if evs[0].Text != "让我看一下这段代码" {
		t.Errorf("text = %q, want '让我看一下这段代码'", evs[0].Text)
	}
}

func TestPumpStream_ToolUse(t *testing.T) {
	evs := streamFromFixture(t, "tool_use.json", nil)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Kind != agent.EventToolStart {
		t.Errorf("event kind = %v, want EventToolStart", evs[0].Kind)
	}
	if evs[0].ToolStart.Name != "Read" {
		t.Errorf("tool name = %q, want 'Read'", evs[0].ToolStart.Name)
	}
	if evs[0].ToolStart.ID != "toolu_001" {
		t.Errorf("tool id = %q, want 'toolu_001'", evs[0].ToolStart.ID)
	}
	if !strings.Contains(evs[0].ToolStart.Args, "/tmp/foo.py") {
		t.Errorf("args = %q, want to contain '/tmp/foo.py'", evs[0].ToolStart.Args)
	}
}

func TestPumpStream_ToolResult(t *testing.T) {
	evs := streamFromFixture(t, "tool_result.json", nil)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Kind != agent.EventToolEnd {
		t.Errorf("event kind = %v, want EventToolEnd", evs[0].Kind)
	}
}

func TestPumpStream_AskUserQuestion(t *testing.T) {
	handler := defaultAskHandler
	evs := streamFromFixture(t, "ask_question.json", handler)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Kind != agent.EventPermission {
		t.Errorf("event kind = %v, want EventPermission", evs[0].Kind)
	}
	pr := evs[0].Permission
	if pr == nil {
		t.Fatal("permission is nil")
	}
	if pr.Tool != "AskUserQuestion" {
		t.Errorf("tool = %q, want 'AskUserQuestion'", pr.Tool)
	}
	// 3 original options + "Other"
	if len(pr.Options) != 4 {
		t.Errorf("options count = %d, want 4 (3 + Other)", len(pr.Options))
	}
	if pr.Options[0] != "PostgreSQL" {
		t.Errorf("first option = %q, want 'PostgreSQL' (Recommended suffix stripped)", pr.Options[0])
	}
	if pr.Options[3] != "Other" {
		t.Errorf("last option = %q, want 'Other'", pr.Options[3])
	}
	if pr.ResponseCh == nil {
		t.Error("ResponseCh is nil")
	}
	if !strings.Contains(pr.Action, "Which database?") {
		t.Errorf("action = %q, want to contain 'Which database?'", pr.Action)
	}
}

func TestPumpStream_Result(t *testing.T) {
	evs := streamFromFixture(t, "result.json", nil)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Kind != agent.EventDone {
		t.Errorf("event kind = %v, want EventDone", evs[0].Kind)
	}
	if evs[0].Done == nil || evs[0].Done.ExitCode != 0 {
		t.Errorf("done = %+v, want ExitCode 0", evs[0].Done)
	}
}

func TestPumpStream_InvalidJSON_Skipped(t *testing.T) {
	input := "not json\n{\"type\":\"result\",\"subtype\":\"success\"}\n"
	events := make(chan agent.AgentEvent, 4)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pumpStream(strings.NewReader(input), events, nil, nil)
		close(events)
	}()
	var got []agent.AgentEvent
	for ev := range events {
		got = append(got, ev)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1 (invalid line skipped)", len(got))
	}
	if got[0].Kind != agent.EventDone {
		t.Errorf("kind = %v, want EventDone", got[0].Kind)
	}
}

// --- AskUserQuestion answer encoding tests ---

func TestEncodeUserAnswer_SingleSelect(t *testing.T) {
	data, err := encodeUserAnswer("toolu_002", []string{"PostgreSQL"}, false)
	if err != nil {
		t.Fatal(err)
	}

	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string          `json:"type"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if msg.Type != "user" {
		t.Errorf("type = %q, want 'user'", msg.Type)
	}
	if msg.Message.Role != "user" {
		t.Errorf("role = %q, want 'user'", msg.Message.Role)
	}
	if len(msg.Message.Content) != 1 {
		t.Fatalf("content count = %d, want 1", len(msg.Message.Content))
	}
	c := msg.Message.Content[0]
	if c.Type != "tool_result" {
		t.Errorf("content type = %q, want 'tool_result'", c.Type)
	}
	if c.ToolUseID != "toolu_002" {
		t.Errorf("tool_use_id = %q, want 'toolu_002'", c.ToolUseID)
	}
	// Single-select → string form
	if string(c.Content) != `"PostgreSQL"` {
		t.Errorf("content = %s, want \"PostgreSQL\" (string)", c.Content)
	}
}

func TestEncodeUserAnswer_MultiSelect_Array(t *testing.T) {
	data, err := encodeUserAnswer("toolu_002", []string{"PostgreSQL", "Auth"}, true)
	if err != nil {
		t.Fatal(err)
	}

	var msg struct {
		Message struct {
			Content []struct {
				Content json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	if string(msg.Message.Content[0].Content) != `["PostgreSQL","Auth"]` {
		t.Errorf("content = %s, want array form", msg.Message.Content[0].Content)
	}
}

func TestEncodeUserAnswer_MultiSelect_LegacyString(t *testing.T) {
	// Even with multi=true, if only one option was selected, we fall
	// back to the string form (no commas needed).
	data, err := encodeUserAnswer("toolu_002", []string{"PostgreSQL"}, true)
	if err != nil {
		t.Fatal(err)
	}
	var msg struct {
		Message struct {
			Content []struct {
				Content json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	if string(msg.Message.Content[0].Content) != `"PostgreSQL"` {
		t.Errorf("content = %s, want string form for single pick", msg.Message.Content[0].Content)
	}
}

func TestEncodeUserAnswer_Empty_NoOp(t *testing.T) {
	data, err := encodeUserAnswer("toolu_002", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Errorf("data = %q, want empty", data)
	}
}

// --- Agent descriptor tests ---

func TestAgent_Name(t *testing.T) {
	a := New("claude", "claude", nil)
	if a.Name() != "claude" {
		t.Errorf("Name = %q, want 'claude'", a.Name())
	}
}

func TestAgent_Mode(t *testing.T) {
	a := New("claude", "claude", nil)
	if a.Mode() != agent.ModeJSONIO {
		t.Errorf("Mode = %v, want ModeJSONIO", a.Mode())
	}
}

func TestAgent_Detect_MissingBinary(t *testing.T) {
	a := New("claude", "this-binary-does-not-exist-12345", nil)
	if err := a.Detect(); err == nil {
		t.Error("Detect should fail for missing binary")
	}
}

// --- Session tests (no real Claude Code binary needed) ---

func TestSession_SendText_NoProcess(t *testing.T) {
	// newSession requires a real binary; we test the JSON encoding
	// path indirectly via SendText/EncodeUserAnswer.
	a := New("claude", "this-binary-does-not-exist-12345", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := a.Start(ctx, agent.StartConfig{Workspace: t.TempDir()})
	if err == nil {
		t.Fatal("Start should fail for missing binary")
	}
	if !strings.Contains(err.Error(), "this-binary-does-not-exist") {
		t.Errorf("err = %v, want to mention the binary name", err)
	}
}

func TestNewSession_EmptyWorkspace(t *testing.T) {
	_, err := newSession(context.Background(), "echo", nil, nil, "")
	if err == nil {
		t.Fatal("newSession with empty workspace should fail")
	}
	if !strings.Contains(err.Error(), "workspace") {
		t.Errorf("err = %v, want to mention 'workspace'", err)
	}
}
