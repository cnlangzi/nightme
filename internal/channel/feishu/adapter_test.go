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
	"github.com/cnlangzi/nightme/internal/gateway"
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
		// Receipt / standalone text now posts as `post` type so
		// the markdown renders natively (collapsible code
		// blocks). See wrapAsPostContent for the schema.
		if msgType != receiptMessageType {
			t.Errorf("msgType = %q, want %q", msgType, receiptMessageType)
		}
		var payload struct {
			ContentV2 [][]map[string]any `json:"content_v2"`
		}
		if err := json.Unmarshal([]byte(content), &payload); err != nil {
			t.Fatalf("decode content: %v", err)
		}
		if len(payload.ContentV2) == 0 {
			t.Fatal("content_v2 is empty")
		}
		// Each chunk is one outer paragraph containing a single
		// `md` tag; pull its text back out for content equality.
		var got string
		for _, in := range payload.ContentV2[0] {
			if tag, _ := in["tag"].(string); tag == "md" {
				if t, ok := in["text"].(string); ok {
					got += t
				}
			}
		}
		sent = append(sent, got)
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

// TestSendUserMessage_MultipleReceiptsPerChat covers the
// per-userMsgID receipt model: each user message in a chat gets
// its own receipt (its own rolling-log card anchored via
// ReplyMessage). The receipts coexist in receiptsByUserMsgID so
// buffered-batch flushes don't collapse them into one shared
// card. This replaces the old single-receipt-per-chat eviction
// test (commit a9f73d3 v1.1 commit 4 moved eviction into the
// terminal lifecycle of each individual receipt).
func TestSendUserMessage_MultipleReceiptsPerChat(t *testing.T) {
	a := testAdapter(t)
	chatID := "oc_chat"

	// Mock replyFunc so ReplyMessage succeeds without the lark
	// SDK hitting Feishu. Each call returns a distinct id so we
	// can tell the receipts apart. sendFunc is also mocked so
	// SetCompleted (which calls UpdateMessage, falling back to
	// SendMessageText on failure) has a working path.
	var replies int
	a.replyFunc = func(_ context.Context, _, _, _ string) (string, error) {
		replies++
		return fmt.Sprintf("reply-%d", replies), nil
	}
	a.sendFunc = func(_ context.Context, _, _, _ string) (string, error) {
		replies++
		return fmt.Sprintf("reply-%d", replies), nil
	}

	r1, err := a.SendUserMessage(context.Background(), chatID, "msg-1", "⏳")
	if err != nil {
		t.Fatalf("SendUserMessage msg-1: %v", err)
	}
	r2, err := a.SendUserMessage(context.Background(), chatID, "msg-2", "⏳")
	if err != nil {
		t.Fatalf("SendUserMessage msg-2: %v", err)
	}
	r3, err := a.SendUserMessage(context.Background(), chatID, "msg-3", "⏳")
	if err != nil {
		t.Fatalf("SendUserMessage msg-3: %v", err)
	}

	// All three receipts are distinct and registered under
	// their own userMsgID. None of them evicts another.
	a.mu.RLock()
	got1 := a.receiptsByUserMsgID["msg-1"]
	got2 := a.receiptsByUserMsgID["msg-2"]
	got3 := a.receiptsByUserMsgID["msg-3"]
	a.mu.RUnlock()
	if got1 != r1 || got2 != r2 || got3 != r3 {
		t.Errorf("receipts not registered per userMsgID: got1=%p want=%p got2=%p want=%p got3=%p want=%p",
			got1, r1, got2, r2, got3, r3)
	}
	if r1 == r2 || r2 == r3 || r1 == r3 {
		t.Errorf("receipts are not distinct pointers: %p %p %p", r1, r2, r3)
	}

	// Completing one receipt does NOT touch the others.
	if err := r1.SetCompleted(context.Background()); err != nil {
		t.Fatalf("SetCompleted r1: %v", err)
	}
	a.mu.RLock()
	got1After := a.receiptsByUserMsgID["msg-1"]
	a.mu.RUnlock()
	if got1After == nil {
		t.Errorf("completed receipt removed from index prematurely")
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

// TestComposeFooterPrefix covers the static-session-attribution
// composer the adapter uses on OutInit to build the cached
// footer prefix. Empty segments drop themselves so a partial
// set ("claude" arrived but cwd not yet) still renders cleanly.
// Tokens are intentionally NOT included — they're agent-context
// and only surface when an OutUsage arrives.
func TestComposeFooterPrefix(t *testing.T) {
	cases := []struct {
		name     string
		agent    string
		cwd      string
		provider string
		want     string
	}{
		{"all three (provider dropped)", "claude", "/code/nightme", "minimax", "claude | /code/nightme"},
		{"just agent", "claude", "", "", "claude"},
		{"agent and cwd", "codex", "/code/pangolin", "", "codex | /code/pangolin"},
		{"cwd only", "", "/code/nightme", "", "/code/nightme"},
		{"all empty", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := composeFooterPrefix(tc.agent, tc.cwd, tc.provider)
			if got != tc.want {
				t.Errorf("composeFooterPrefix(%q, %q, %q) = %q, want %q",
					tc.agent, tc.cwd, tc.provider, got, tc.want)
			}
		})
	}
}

// TestAdapter_FooterCache_PerChat verifies that the cached
// footer prefix is keyed by chatID: a prefix set for chat-A
// does NOT leak into chat-B's receipt rendering, and vice versa.
// The cache is a simple per-chat memoization that the adapter
// uses to rebuild the full footer on OutUsage without re-reading
// session metadata each time.
func TestAdapter_FooterCache_PerChat(t *testing.T) {
	a := testAdapter(t)

	// Prime two chats' caches.
	a.cacheFooterPrefix("chat-A", "Agent: claude | cwd: /chatA")
	a.cacheFooterPrefix("chat-B", "Agent: codex | cwd: /chatB")

	if got := a.cachedFooterPrefix("chat-A"); got != "Agent: claude | cwd: /chatA" {
		t.Errorf("chat-A prefix = %q", got)
	}
	if got := a.cachedFooterPrefix("chat-B"); got != "Agent: codex | cwd: /chatB" {
		t.Errorf("chat-B prefix = %q", got)
	}
	if got := a.cachedFooterPrefix("chat-C"); got != "" {
		t.Errorf("unset chat-C prefix = %q, want empty", got)
	}

	// Clearing one chat doesn't disturb the other.
	a.cacheFooterPrefix("chat-A", "")
	if got := a.cachedFooterPrefix("chat-A"); got != "" {
		t.Errorf("chat-A prefix after clear = %q, want empty", got)
	}
	if got := a.cachedFooterPrefix("chat-B"); got != "Agent: codex | cwd: /chatB" {
		t.Errorf("chat-B prefix disturbed by chat-A clear: %q", got)
	}
}

// TestSend_ReplyToEmpty_PlainText covers the no-anchor path:
// OutboundMessage without a ReplyTo posts as a plain text message
// (no receipt, no ReplyMessage). This is the path used for
// genuinely unsolicited output (startup notices, internal logs,
// etc.) and the fallback messages where the channel should not
// try to be clever about cards.
func TestSend_ReplyToEmpty_PlainText(t *testing.T) {
	a := testAdapter(t)
	var sent []string
	a.sendFunc = func(_ context.Context, _, _, content string) (string, error) {
		// content is the JSON-encoded {text: ...} envelope;
		// the message body is the unquoted text field. We just
		// record it as-is for assertion simplicity — the
		// adapter's behavior we care about is that sendFunc is
		// called exactly once, NOT that the encoding matches.
		sent = append(sent, content)
		return "msg-x", nil
	}

	if err := a.Send(context.Background(), gateway.OutboundMessage{
		ChatID:  "oc_test",
		Kind:    gateway.OutText,
		Text:    "no anchor",
		ReplyTo: "",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(sent) != 1 {
		t.Errorf("sendFunc calls = %d, want exactly 1 (one plain text send)", len(sent))
	}
	if got := a.receiptFor("any"); got != nil {
		t.Errorf("ReplyTo=\"\" must NOT create a receipt, got %p", got)
	}
}

// TestSend_ReplyToColdStart covers the cold-start path: first
// OutboundMessage for a userMsgID posts a fresh reply card via
// the ReplyMessage API (anchored to the user message) and
// registers the receipt. Subsequent calls with the same
// ReplyTo Append in place.
func TestSend_ReplyToColdStart(t *testing.T) {
	a := testAdapter(t)
	var replies []string
	var replyAnchors []string
	a.replyFunc = func(_ context.Context, userMsgID, _, text string) (string, error) {
		replyAnchors = append(replyAnchors, userMsgID)
		replies = append(replies, text)
		return "msg-cold", nil
	}
	a.sendFunc = func(_ context.Context, _, _, _ string) (string, error) {
		return "msg-cold", nil
	}

	const userMsgID = "om_user_cold"
	if err := a.Send(context.Background(), gateway.OutboundMessage{
		ChatID:  "oc_test",
		Kind:    gateway.OutText,
		Text:    "first event",
		ReplyTo: userMsgID,
	}); err != nil {
		t.Fatalf("Send first: %v", err)
	}
	if len(replyAnchors) != 1 || replyAnchors[0] != userMsgID {
		t.Errorf("first Send should anchor via ReplyMessage to %q, got anchors=%v", userMsgID, replyAnchors)
	}
	r := a.receiptFor(userMsgID)
	if r == nil {
		t.Fatal("first Send should register a receipt for the anchored userMsgID")
	}

	// Second call: same userMsgID, existing receipt → Append
	// (UpdateMessage in place, no ReplyMessage).
	if err := a.Send(context.Background(), gateway.OutboundMessage{
		ChatID:  "oc_test",
		Kind:    gateway.OutText,
		Text:    "second event",
		ReplyTo: userMsgID,
	}); err != nil {
		t.Fatalf("Send second: %v", err)
	}
	if len(replyAnchors) != 1 {
		t.Errorf("second Send must NOT call ReplyMessage again; got %d ReplyMessage calls", len(replyAnchors))
	}
}

// TestSend_MultipleReceiptsCoexist covers the buffered-batch
// scenario: events for different userMsgIDs in the same chat
// create / append to separate receipts. No event for one
// userMsgID folds into another userMsgID's receipt.
func TestSend_MultipleReceiptsCoexist(t *testing.T) {
	a := testAdapter(t)
	a.replyFunc = func(_ context.Context, userMsgID, _, text string) (string, error) {
		return "reply-" + userMsgID, nil
	}
	a.sendFunc = func(_ context.Context, _, _, _ string) (string, error) {
		return "msg", nil
	}

	// Two user messages buffered together; agent emits events
	// for both, each anchored to its own userMsgID.
	for _, userMsgID := range []string{"om_a", "om_b"} {
		if err := a.Send(context.Background(), gateway.OutboundMessage{
			ChatID:  "oc_test",
			Kind:    gateway.OutText,
			Text:    "event for " + userMsgID,
			ReplyTo: userMsgID,
		}); err != nil {
			t.Fatalf("Send %s: %v", userMsgID, err)
		}
	}

	rA := a.receiptFor("om_a")
	rB := a.receiptFor("om_b")
	if rA == nil || rB == nil {
		t.Fatalf("both receipts should exist: rA=%p rB=%p", rA, rB)
	}
	if rA == rB {
		t.Fatal("receipts must be distinct objects per userMsgID")
	}
}
