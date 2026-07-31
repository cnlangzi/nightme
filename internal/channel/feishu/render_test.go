package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

type renderedMessage struct {
	chatID  string
	msgType string
	content string
}

func captureRenderedMessages(a *Adapter) *[]renderedMessage {
	messages := make([]renderedMessage, 0, 1)
	a.sendFunc = func(_ context.Context, chatID, msgType, content string) error {
		messages = append(messages, renderedMessage{chatID: chatID, msgType: msgType, content: content})
		return nil
	}
	return &messages
}

func textFromRendered(t *testing.T, message renderedMessage) string {
	t.Helper()
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(message.content), &payload); err != nil {
		t.Fatalf("decode text content: %v", err)
	}
	return payload.Text
}

func TestRender_Text(t *testing.T) {
	a := testAdapter(t)
	messages := captureRenderedMessages(a)
	r := NewRenderer(a)

	if err := r.Render(context.Background(), "oc_chat", agent.AgentEvent{Kind: agent.EventText, Text: "hello"}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(*messages) != 1 {
		t.Fatalf("sent %d messages, want 1", len(*messages))
	}
	got := (*messages)[0]
	if got.chatID != "oc_chat" || got.msgType != "text" {
		t.Fatalf("outbound = %+v, want chat/type", got)
	}
	if text := textFromRendered(t, got); text != "hello" {
		t.Errorf("text = %q, want hello", text)
	}
}

func TestRender_Permission(t *testing.T) {
	a := testAdapter(t)
	messages := captureRenderedMessages(a)
	r := NewRenderer(a)

	req := &agent.PermissionRequest{
		Tool:    "Bash",
		Action:  "Run go test ./...",
		Options: []string{"once", "reject"},
	}
	if err := r.Render(context.Background(), "oc_chat", agent.AgentEvent{Kind: agent.EventPermission, Permission: req}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(*messages) != 1 {
		t.Fatalf("sent %d messages, want 1", len(*messages))
	}
	got := (*messages)[0]
	if got.msgType != interactiveMessageType {
		t.Fatalf("msg type = %q, want %q", got.msgType, interactiveMessageType)
	}
	var card struct {
		Header struct {
			Title struct {
				Content string `json:"content"`
			} `json:"title"`
		} `json:"header"`
		Elements []struct {
			Actions []struct {
				Value map[string]string `json:"value"`
			} `json:"actions"`
		} `json:"elements"`
	}
	if err := json.Unmarshal([]byte(got.content), &card); err != nil {
		t.Fatalf("decode permission card: %v", err)
	}
	if card.Header.Title.Content != "Permission required" {
		t.Errorf("card title = %q", card.Header.Title.Content)
	}
	if len(card.Elements) < 2 || len(card.Elements[1].Actions) != 2 {
		t.Fatalf("card actions = %+v, want two buttons", card.Elements)
	}
	if gotOption := card.Elements[1].Actions[0].Value["option"]; gotOption != "once" {
		t.Errorf("first option = %q, want once", gotOption)
	}
}

func TestRender_Done(t *testing.T) {
	a := testAdapter(t)
	messages := captureRenderedMessages(a)
	r := NewRenderer(a)

	if err := r.Render(context.Background(), "oc_chat", agent.AgentEvent{
		Kind: agent.EventDone,
		Done: &agent.DoneEvent{ExitCode: 0},
	}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := textFromRendered(t, (*messages)[0]); got != "Session ended (exit 0)" {
		t.Errorf("done text = %q", got)
	}
}

func TestRender_ToolEnd_Success(t *testing.T) {
	a := testAdapter(t)
	messages := captureRenderedMessages(a)
	r := NewRenderer(a)

	if err := r.Render(context.Background(), "oc_chat", agent.AgentEvent{
		Kind:    agent.EventToolEnd,
		ToolEnd: &agent.ToolEndEvent{Name: "Read"},
	}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := textFromRendered(t, (*messages)[0]); got != "✅ Read done" {
		t.Errorf("tool success text = %q", got)
	}
}

func TestRender_ToolEnd_Failure(t *testing.T) {
	a := testAdapter(t)
	messages := captureRenderedMessages(a)
	r := NewRenderer(a)

	if err := r.Render(context.Background(), "oc_chat", agent.AgentEvent{
		Kind:    agent.EventToolEnd,
		ToolEnd: &agent.ToolEndEvent{Name: "Bash", Err: errors.New("exit status 2")},
	}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := textFromRendered(t, (*messages)[0])
	if !strings.Contains(got, "❌ Bash failed: exit status 2") {
		t.Errorf("tool failure text = %q", got)
	}
}

func TestRender_Error(t *testing.T) {
	a := testAdapter(t)
	messages := captureRenderedMessages(a)
	r := NewRenderer(a)

	if err := r.Render(context.Background(), "oc_chat", agent.AgentEvent{
		Kind:  agent.EventError,
		Error: &agent.ErrorEvent{Err: errors.New("broken")},
	}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := textFromRendered(t, (*messages)[0]); got != "Error: broken" {
		t.Errorf("error text = %q", got)
	}
}
