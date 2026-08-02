package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	larkdispatcher "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
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

func TestSendUserMessage_EvictionDoesNotDeadlock(t *testing.T) {
	// Before the fix: SendUserMessage held a.mu.Lock while calling
	// old.SetCompleted(ctx). SetCompleted → renderLocked →
	// adapter.UpdateMessage → logOutgoing needs a.mu.RLock, and
	// Go's sync.RWMutex is not reentrant — the eviction self-
	// deadlocked the dispatchLoop goroutine, blocking every later
	// inbound message. Reproduce the path here: leave the old
	// receipt in StateExecuting (the prefix that triggers the
	// renderLocked branch) and call SendUserMessage again. The
	// goroutine must return within the deadline.
	a := testAdapter(t)
	chatID := "oc_chat"

	// Replace sendFunc so SendMessageText succeeds without the
	// lark SDK actually hitting Feishu.
	var replies int
	a.sendFunc = func(_ context.Context, _, _, _ string) (string, error) {
		replies++
		return fmt.Sprintf("reply-%d", replies), nil
	}

	// Register an old receipt in StateExecuting. SetCompleted
	// short-circuits when state is already Completed, so the
	// deadlock only repros when the old turn is still in-flight.
	old := NewMessageReceiptForReply(chatID, "msg-old", "reply-old", a)
	old.state = StateExecuting
	a.mu.Lock()
	a.receipts[chatID] = old
	a.receiptsByUserMsgID["msg-old"] = old
	a.mu.Unlock()

	// Kick SendUserMessage for a new message in a goroutine. The
	// pre-fix code locked here for 5+ minutes against itself.
	type result struct {
		receipt *MessageReceipt
		err     error
	}
	done := make(chan result, 1)
	go func() {
		r, err := a.SendUserMessage(context.Background(), chatID, "msg-new", "⏳ 等待中")
		done <- result{r, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("SendUserMessage: %v", r.err)
		}
		if r.receipt == nil {
			t.Fatal("nil receipt returned")
		}
		// The new receipt is the active one for this chat.
		a.mu.RLock()
		got := a.receipts[chatID]
		byUser := a.receiptsByUserMsgID["msg-new"]
		oldStill := a.receiptsByUserMsgID["msg-old"]
		a.mu.RUnlock()
		if got != r.receipt {
			t.Errorf("chat receipt not updated: got %p, want %p", got, r.receipt)
		}
		if byUser != r.receipt {
			t.Errorf("userMsgID index not updated")
		}
		if oldStill != nil {
			t.Errorf("old receipt still in userMsgID index: %p", oldStill)
		}
		// The old receipt was promoted to StateCompleted by the
		// eviction — SetCompleted runs after the lock release.
		old.mu.Lock()
		state := old.state
		old.mu.Unlock()
		if state != StateCompleted {
			t.Errorf("old receipt state = %v, want StateCompleted", state)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SendUserMessage deadlocked: old.SetCompleted → renderLocked → logOutgoing blocked on a.mu.RLock while holding a.mu.Lock")
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

// feishuEventPayload returns a synthetic Feishu im.message.receive_v1
// event body as raw JSON. The shape mirrors what the lark SDK's
// WS client receives over the wire — we feed it to EventDispatcher.Do
// to exercise the SDK's JSON parse + handler-dispatch path without
// needing a real Feishu connection. One event carries one message;
// the tests below loop with different message_id / chat_id / text
// triples to simulate the multi-message scenario.
func feishuEventPayload(t *testing.T, chatID, senderID, messageID, text string) []byte {
	t.Helper()
	body := map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"event_id":    "evt_" + messageID,
			"event_type":  "im.message.receive_v1",
			"app_id":      "cli_test",
			"tenant_key":  "test_tenant",
			"create_time": "1720000000123",
		},
		"event": map[string]any{
			"sender": map[string]any{
				"sender_id":   map[string]any{"open_id": senderID},
				"sender_type": "user",
				"tenant_key":  "test_tenant",
			},
			"message": map[string]any{
				"message_id":   messageID,
				"create_time":  "1720000000123",
				"chat_id":      chatID,
				"chat_type":    "dm",
				"message_type": "text",
				"content":      `{"text":` + strconvQuote(text) + `}`,
			},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal synthetic event: %v", err)
	}
	return raw
}

// strconvQuote wraps s in JSON quotes without dragging in the
// encoding/json import cycle. The synthetic event content is
// `{"text":"..."}`; double quotes inside the text would need
// escaping, but the test inputs are fixed ASCII so a single
// replacement is enough.
func strconvQuote(s string) string {
	out := `"`
	for _, r := range s {
		if r == '"' || r == '\\' {
			out += `\`
		}
		out += string(r)
	}
	return out + `"`
}

// TestFeishuSyntheticEventDispatch_ReachesAdapter simulates the
// Feishu WebSocket delivery path by feeding raw event JSON
// through the lark SDK's EventDispatcher.Do. The test verifies
// the JSON → unmarshal → handler dispatch path produces the
// channel.Message we expect — the same path a real WS frame
// takes, minus the wire transport.
//
// Runs without Feishu credentials because the dispatcher's
// verification_token and encrypt_key are empty here. The
// adapter is wired to the dispatcher so handler call counts as
// the proof of correct wiring.
func TestFeishuSyntheticEventDispatch_ReachesAdapter(t *testing.T) {
	a := testAdapter(t)

	// Build a dispatcher that calls a.handleMessage for the
	// receive_v1 event_type — the same wiring NewAdapter uses,
	// so a successful dispatch proves the synthetic JSON
	// passes the SDK's event parsing untouched.
	dispatcher := larkdispatcher.NewEventDispatcher("", "")
	dispatcher.OnP2MessageReceiveV1(a.handleMessage)

	payload := feishuEventPayload(t, "oc_chat_a", "ou_sender_a", "om_msg_a", "hello")
	if _, err := dispatcher.Do(context.Background(), payload); err != nil {
		t.Fatalf("dispatcher.Do: %v", err)
	}

	select {
	case got := <-a.Incoming():
		if got.ChatID != "oc_chat_a" || got.Text != "hello" || got.UserID != "ou_sender_a" {
			t.Fatalf("message = %+v, want chat/text/sender from synthetic event", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for incoming message after synthetic dispatch")
	}
}

// TestFeishuSyntheticEventDispatch_MultipleMessages exercises the
// eviction path that the B-fix targets, but on the SDK dispatch
// side: feed several synthetic events through the dispatcher in
// quick succession and confirm each one becomes a real
// channel.Message in the incoming channel. Each event triggers
// a fresh SendUserMessage path on the adapter side once the
// runtime picks it up; with the pre-fix eviction code, the
// dispatch goroutine that calls Do() never blocks (it doesn't
// run SendUserMessage itself), so this test stays green either
// way — its job is to assert the *upstream* path can deliver
// multi-message bursts without dropping frames.
//
// For the eviction-deadlock coverage, see
// TestSendUserMessage_EvictionDoesNotDeadlock in this file.
func TestFeishuSyntheticEventDispatch_MultipleMessages(t *testing.T) {
	a := testAdapter(t)
	dispatcher := larkdispatcher.NewEventDispatcher("", "")
	dispatcher.OnP2MessageReceiveV1(a.handleMessage)

	const n = 5
	go func() {
		for i := 0; i < n; i++ {
			chatID := "oc_chat_" + string(rune('a'+i))
			senderID := "ou_sender_" + string(rune('a'+i))
			msgID := "om_msg_" + string(rune('a'+i))
			text := "msg-" + string(rune('a'+i))
			if _, err := dispatcher.Do(context.Background(),
				feishuEventPayload(t, chatID, senderID, msgID, text)); err != nil {
				t.Errorf("dispatcher.Do[%d]: %v", i, err)
				return
			}
		}
	}()

	seen := map[string]bool{}
	deadline := time.After(3 * time.Second)
	for len(seen) < n {
		select {
		case got := <-a.Incoming():
			seen[got.Text] = true
		case <-deadline:
			t.Fatalf("only %d/%d synthetic events delivered before deadline (seen: %v)",
				len(seen), n, seen)
		}
	}
	if len(seen) != n {
		t.Fatalf("delivered %d unique messages, want %d", len(seen), n)
	}
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
