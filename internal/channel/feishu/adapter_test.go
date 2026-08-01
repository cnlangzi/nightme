package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/cnlangzi/nightme/internal/config"
)

func testAdapter(t *testing.T) *Adapter {
	t.Helper()
	a, err := NewAdapter(&config.Config{Feishu: config.FeishuConfig{
		AppID:     "cli_test",
		AppSecret: "secret_test",
	}})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	return a
}

func TestAdapter_Name(t *testing.T) {
	if got := testAdapter(t).Name(); got != "feishu" {
		t.Fatalf("Name() = %q, want feishu", got)
	}
}

func TestNewAdapter_RequiresCredentials(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
	}{
		{name: "nil config"},
		{name: "missing app id", cfg: &config.Config{Feishu: config.FeishuConfig{AppSecret: "secret"}}},
		{name: "missing app secret", cfg: &config.Config{Feishu: config.FeishuConfig{AppID: "app"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewAdapter(tc.cfg); err == nil {
				t.Fatal("NewAdapter returned nil error")
			}
		})
	}
}

func TestSendLongMessage_SplitsAtNewline(t *testing.T) {
	a := testAdapter(t)
	text := strings.Repeat("a", 2000) + "\n" + strings.Repeat("b", 2000)

	var sent []string
	a.sendFunc = func(_ context.Context, chatID, msgType, content string) (string, error) {
		if chatID != "oc_test" {
			t.Errorf("chatID = %q, want oc_test", chatID)
		}
		if msgType != larkim.MsgTypeText {
			t.Errorf("msgType = %q, want %q", msgType, larkim.MsgTypeText)
		}
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(content), &payload); err != nil {
			t.Fatalf("decode content: %v", err)
		}
		sent = append(sent, payload.Text)
		return "", nil
	}

	if err := a.SendLongMessage(context.Background(), "oc_test", text); err != nil {
		t.Fatalf("SendLongMessage: %v", err)
	}
	if len(sent) != 2 {
		t.Fatalf("sent %d chunks, want 2", len(sent))
	}
	for i, part := range sent {
		if len(part) > maxMessageBytes {
			t.Errorf("chunk %d is %d bytes, want <= %d", i, len(part), maxMessageBytes)
		}
	}
	if got := strings.Join(sent, ""); got != text {
		t.Fatal("chunks changed the message contents")
	}
	if !strings.HasSuffix(sent[0], "\n") {
		t.Error("first chunk did not end at the newline boundary")
	}
}

func TestIncoming_ReceivesEvent(t *testing.T) {
	a := testAdapter(t)
	chatID := "oc_chat"
	senderID := "ou_sender"
	content := `{"text":"hello"}`
	messageType := larkim.MsgTypeText
	created := "1720000000123"
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderId: &larkim.UserId{OpenId: &senderID}},
			Message: &larkim.EventMessage{
				ChatId:      &chatID,
				Content:     &content,
				MessageType: &messageType,
				CreateTime:  &created,
			},
		},
	}

	if err := a.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}
	select {
	case got := <-a.Incoming():
		if got.ChatID != chatID || got.Text != "hello" || got.UserID != senderID {
			t.Fatalf("message = %+v, want chat/text/sender", got)
		}
		if !got.Time.Equal(time.UnixMilli(1720000000123)) {
			t.Errorf("message time = %v, want parsed event time", got.Time)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for incoming message")
	}
}

func TestHandleMessage_LogsInbound(t *testing.T) {
	// Capture slog.Default output so the test sees what the
	// adapter writes via logInbound. logInbound reads
	// a.logger (defaulted to slog.Default in NewAdapter).
	var (
		mu  sync.Mutex
		buf bytes.Buffer
	)
	prev := slog.Default()
	both := io.MultiWriter(&safeWriter{w: &buf, mu: &mu}, io.Discard)
	slog.SetDefault(slog.New(slog.NewJSONHandler(both, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	a := testAdapter(t)
	chatID := "oc_chat"
	senderID := "ou_sender"
	content := `{"text":"trace me"}`
	messageType := larkim.MsgTypeText
	created := "1720000000123"
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderId: &larkim.UserId{OpenId: &senderID}},
			Message: &larkim.EventMessage{
				ChatId:      &chatID,
				Content:     &content,
				MessageType: &messageType,
				CreateTime:  &created,
			},
		},
	}

	if err := a.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}

	// Drain the incoming channel so the publish goroutine — if
	// any — completes and flushes the log line.
	select {
	case <-a.Incoming():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for incoming message")
	}

	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if !strings.Contains(out, "feishu: incoming") {
		t.Errorf("expected feishu: incoming log line, got %q", out)
	}
	if !strings.Contains(out, "trace me") {
		t.Errorf("expected log line to carry the message text, got %q", out)
	}
}

// safeWriter serializes writes so the test goroutine and the
// adapter's logger do not race on the shared buffer.
type safeWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (s *safeWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func TestAdapter_StopClosesIncoming(t *testing.T) {
	a := testAdapter(t)
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case _, ok := <-a.Incoming():
		if ok {
			t.Fatal("Incoming channel remains open after Stop")
		}
	case <-time.After(time.Second):
		t.Fatal("Incoming channel did not close")
	}
}
