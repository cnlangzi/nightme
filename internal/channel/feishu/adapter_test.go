package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	a.sendFunc = func(_ context.Context, chatID, msgType, content, rootID string) (string, error) {
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

// TestHandleMessage_ReplyToFromParentId verifies the F-33 D3
// invariant: InboundMessage.ReplyTo is wired from
// event.Message.ParentId. When the user replies in a thread the
// SDK surfaces ParentId pointing at the directly replied-to
// message; nightme's channel.Message.ReplyTo must carry that same
// id. Top-level messages (ParentId == "") must produce an empty
// ReplyTo so dispatch treats them as fresh turns.
//
// Thread-top-level RootId is intentionally not surfaced (F-33 D3):
// even if the SDK populates RootId, nightme data model does not
// see it.
func TestHandleMessage_ReplyToFromParentId(t *testing.T) {
	cases := []struct {
		name        string
		parentID    string // event.Message.ParentId (empty for top-level)
		rootID      string // event.Message.RootId (must NOT surface)
		wantReplyTo string // expected InboundMessage.ReplyTo
	}{
		{
			name:        "reply in thread carries ParentId",
			parentID:    "om_target_message",
			rootID:      "om_thread_root",
			wantReplyTo: "om_target_message",
		},
		{
			name:        "top-level message has empty ReplyTo",
			parentID:    "",
			rootID:      "",
			wantReplyTo: "",
		},
		{
			name:        "thread-root message has empty ReplyTo",
			parentID:    "",
			rootID:      "om_thread_root",
			wantReplyTo: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := testAdapter(t)

			chatID := "oc_chat"
			senderID := "ou_sender"
			content := `{"text":"hello"}`
			messageType := larkim.MsgTypeText
			created := "1720000000123"
			messageID := "om_new"
			event := &larkim.P2MessageReceiveV1{
				Event: &larkim.P2MessageReceiveV1Data{
					Sender: &larkim.EventSender{SenderId: &larkim.UserId{OpenId: &senderID}},
					Message: &larkim.EventMessage{
						ChatId:      &chatID,
						ParentId:    &tc.parentID,
						RootId:      &tc.rootID,
						Content:     &content,
						MessageType: &messageType,
						CreateTime:  &created,
						MessageId:   &messageID,
					},
				},
			}

			if err := a.handleMessage(context.Background(), event); err != nil {
				t.Fatalf("handleMessage: %v", err)
			}

			select {
			case got := <-a.Incoming():
				if got.ReplyTo != tc.wantReplyTo {
					t.Errorf("ReplyTo = %q, want %q", got.ReplyTo, tc.wantReplyTo)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for incoming message")
			}
		})
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

// TestSend_RoutesByUserMsgID_NotChatID is the v1.3 regression test
// for the per-chat-active-receipt bug. Per SPEC §2.2, Send must
// route OutboundMessage{ReplyTo: userMsgID} to the per-userMsgID
// receipt; if no receipt exists for that userMsgID, cold-create one.
//
// The pre-fix implementation routed by chatID via a.receipts[chatID],
// which meant turn 2's events (after turn 1's receipt hit
// StateCompleted) silently dropped inside receipt.Append. This test
// reproduces that path: turn 1 completes, turn 2 starts, and we
// assert turn 2 cold-creates its own receipt.
func TestSend_RoutesByUserMsgID_NotChatID(t *testing.T) {
	a := testAdapter(t)
	chatID := "oc_chat_v13_routing"

	// Mock: SendCard / SendMessageText return synthetic message IDs;
	// PatchMessage is a no-op recorder.
	var cards int
	a.sendFunc = func(_ context.Context, _, _, _, _ string) (string, error) {
		cards++
		return fmt.Sprintf("om_card_%d", cards), nil
	}
	var patches []patchCall
	a.updateFunc = func(_ context.Context, msgID, _ string) error {
		patches = append(patches, patchCall{MessageID: msgID})
		return nil
	}

	// ---- Turn 1 ----
	if err := a.Send(context.Background(), gateway.OutboundMessage{
		Kind: gateway.OutText, ChatID: chatID, ReplyTo: "om_msg_1",
		Text: "turn 1 hello",
	}); err != nil {
		t.Fatalf("turn 1 Send: %v", err)
	}

	// Receipt 1 should exist keyed by om_msg_1.
	a.mu.RLock()
	rcpt1, ok1 := a.receiptsByUserMsgID["om_msg_1"]
	a.mu.RUnlock()
	if !ok1 || rcpt1 == nil {
		t.Fatalf("turn 1: receipt not registered for om_msg_1")
	}
	// Simulate turn 1 ending: receipt reaches terminal state.
	rcpt1.mu.Lock()
	rcpt1.state = StateCompleted
	rcpt1.mu.Unlock()

	// Reset PATCH counter so we only assert on turn 2's outgoing
	// writes. (Turn 1 legitimately PATCHed its own cardMsgID via
	// the Append path before we marked it Completed.)
	patchesBeforeTurn2 := len(patches)

	// ---- Turn 2 ----
	if err := a.Send(context.Background(), gateway.OutboundMessage{
		Kind: gateway.OutText, ChatID: chatID, ReplyTo: "om_msg_2",
		Text: "turn 2 hello",
	}); err != nil {
		t.Fatalf("turn 2 Send: %v", err)
	}

	// Turn 2 must have cold-created a SEPARATE receipt.
	a.mu.RLock()
	rcpt2, ok2 := a.receiptsByUserMsgID["om_msg_2"]
	rcpt1ByMsg, ok1ByMsg := a.receiptsByUserMsgID["om_msg_1"]
	a.mu.RUnlock()
	if !ok2 || rcpt2 == nil {
		t.Fatalf("turn 2: receipt not registered for om_msg_2 (cold-create failed)")
	}
	if rcpt1 == rcpt2 {
		t.Fatalf("turn 2 must cold-create new receipt; got same instance as turn 1 (per-chat routing bug)")
	}
	if !ok1ByMsg || rcpt1ByMsg != rcpt1 {
		t.Fatalf("turn 1 receipt lost after turn 2 cold-create")
	}

	// Both receipts should have been cold-created via SendCard (one per turn).
	if cards < 2 {
		t.Errorf("expected >=2 cold-start SendCard calls, got %d", cards)
	}

	// After turn 1 was Completed, NO further PATCH may target
	// rcpt1.cardMsgID. The pre-fix per-chat routing bug routed
	// turn 2's events to rcpt1 (which had StateCompleted, so the
	// PATCH was silently dropped inside receipt.Append — but the
	// user would see nothing in turn 2's reply, only rcpt2 would
	// show). Asserting "no PATCH on rcpt1.cardMsgID after turn 2
	// started" is the smoke test.
	for _, p := range patches[patchesBeforeTurn2:] {
		if p.MessageID == rcpt1.cardMsgID {
			t.Errorf("turn 2 wrote into turn 1's completed receipt (msgID=%s)", p.MessageID)
		}
	}
	// And turn 2's content must actually land somewhere: at least
	// one PATCH on rcpt2.cardMsgID since turn 2 started.
	var rcpt2Patched bool
	for _, p := range patches[patchesBeforeTurn2:] {
		if p.MessageID == rcpt2.cardMsgID {
			rcpt2Patched = true
			break
		}
	}
	if !rcpt2Patched {
		t.Errorf("turn 2 cold-create produced no PATCH on rcpt2 (cardMsgID=%s); user would see nothing", rcpt2.cardMsgID)
	}
}

type patchCall struct {
	MessageID string
}

// TestSend_OutThinking_PostsToThread — F-34. OutThinking is
// routed to a Feishu thread reply (rootID = msg.ReplyTo) with
// the body "💭 <text>". The receipt card no longer carries the
// thinking entry.
//
// F-34 review P1-3: Adapter.Send also Touch()es the receipt so
// the main chat card header keeps ticking. This triggers the
// cold-start card creation (one send_card) followed by a silent
// PATCH. The test captures all outgoing sends and asserts on the
// text reply; the card + PATCH are accepted as observable
// side-effects, not tested here (covered by TestReceipt_*).
func TestSend_OutThinking_PostsToThread(t *testing.T) {
	a := testAdapter(t)

	type captured struct {
		ChatID  string
		MsgType string
		RootID  string
		Text    string
	}
	var sends []captured
	a.sendFunc = func(_ context.Context, chatID, msgType, content, rootID string) (string, error) {
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal([]byte(content), &payload)
		sends = append(sends, captured{chatID, msgType, rootID, payload.Text})
		return "om_text_test", nil
	}

	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind:    gateway.OutThinking,
		ChatID:  "oc_test",
		ReplyTo: "om_user_1",
		Text:    "let me think",
	}); err != nil {
		t.Fatalf("Send(OutThinking): %v", err)
	}

	// Find the text reply (skip the cold-start card created by Touch).
	var textReply *captured
	for i := range sends {
		if sends[i].MsgType == larkim.MsgTypeText {
			textReply = &sends[i]
			break
		}
	}
	if textReply == nil {
		t.Fatalf("no text reply found in sends: %+v", sends)
	}
	if textReply.RootID != "om_user_1" {
		t.Errorf("rootID = %q, want om_user_1 (must thread to user message)", textReply.RootID)
	}
	if textReply.Text != "💭 let me think" {
		t.Errorf("body = %q, want %q", textReply.Text, "💭 let me think")
	}
}

// TestSend_OutToolStart_PostsToThread — F-34. OutToolStart is
// routed to a thread reply with the body "🔧 name(args)".
// F-34 review P1-3: also Touch()es the receipt (cold-start
// card + silent PATCH side-effect).
func TestSend_OutToolStart_PostsToThread(t *testing.T) {
	a := testAdapter(t)

	type captured struct {
		MsgType string
		RootID  string
		Text    string
	}
	var sends []captured
	a.sendFunc = func(_ context.Context, _, msgType, content, rootID string) (string, error) {
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal([]byte(content), &payload)
		sends = append(sends, captured{msgType, rootID, payload.Text})
		return "om_text_test", nil
	}

	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind:    gateway.OutToolStart,
		ChatID:  "oc_test",
		ReplyTo: "om_user_1",
		Tool: &gateway.ToolInfo{
			Name: "Read",
			Args: "/foo.go",
		},
	}); err != nil {
		t.Fatalf("Send(OutToolStart): %v", err)
	}
	var textReply *captured
	for i := range sends {
		if sends[i].MsgType == larkim.MsgTypeText {
			textReply = &sends[i]
			break
		}
	}
	if textReply == nil {
		t.Fatalf("no text reply in sends: %+v", sends)
	}
	if textReply.RootID != "om_user_1" {
		t.Errorf("rootID = %q, want om_user_1", textReply.RootID)
	}
	if textReply.Text != "● Read(/foo.go)" {
		t.Errorf("body = %q, want %q (Claude Code-style call line)", textReply.Text, "● Read(/foo.go)")
	}
}

// TestSend_OutToolEnd_PostsToThread — F-34. OutToolEnd is routed
// to a thread reply with a type-aware one-line summary.
// F-34 review P1-3: also Touch()es the receipt (cold-start
// card + silent PATCH side-effect).
func TestSend_OutToolEnd_PostsToThread(t *testing.T) {
	a := testAdapter(t)

	type captured struct {
		MsgType string
		RootID  string
		Text    string
	}
	var sends []captured
	a.sendFunc = func(_ context.Context, _, msgType, content, rootID string) (string, error) {
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal([]byte(content), &payload)
		sends = append(sends, captured{msgType, rootID, payload.Text})
		return "om_text_test", nil
	}

	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind:    gateway.OutToolEnd,
		ChatID:  "oc_test",
		ReplyTo: "om_user_1",
		Tool: &gateway.ToolInfo{
			Name:   "Read",
			Args:   "/foo.go",
			Output: "line1\nline2",
		},
	}); err != nil {
		t.Fatalf("Send(OutToolEnd): %v", err)
	}
	var textReply *captured
	for i := range sends {
		if sends[i].MsgType == larkim.MsgTypeText {
			textReply = &sends[i]
			break
		}
	}
	if textReply == nil {
		t.Fatalf("no text reply in sends: %+v", sends)
	}
	if textReply.RootID != "om_user_1" {
		t.Errorf("rootID = %q, want om_user_1", textReply.RootID)
	}
	want := summarizeToolResult("Read", "line1\nline2", nil)
	if textReply.Text != want {
		t.Errorf("body = %q, want %q (from summarizeToolResult)", textReply.Text, want)
	}
	if !strings.Contains(textReply.Text, "⎿  📄 Read") {
		t.Errorf("body = %q, want it to start with ⎿  📄 Read (Claude Code-style result line)", textReply.Text)
	}
}

// TestSend_OutCompaction_PostsToThread — F-34. OutCompaction
// is routed to a thread reply with "✶ Compacting conversation…".
// F-34 review P1-3: also Touch()es the receipt.
func TestSend_OutCompaction_PostsToThread(t *testing.T) {
	a := testAdapter(t)

	type captured struct {
		MsgType string
		RootID  string
		Text    string
	}
	var sends []captured
	a.sendFunc = func(_ context.Context, _, msgType, content, rootID string) (string, error) {
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal([]byte(content), &payload)
		sends = append(sends, captured{msgType, rootID, payload.Text})
		return "om_text_test", nil
	}

	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind:    gateway.OutCompaction,
		ChatID:  "oc_test",
		ReplyTo: "om_user_1",
	}); err != nil {
		t.Fatalf("Send(OutCompaction): %v", err)
	}
	var textReply *captured
	for i := range sends {
		if sends[i].MsgType == larkim.MsgTypeText {
			textReply = &sends[i]
			break
		}
	}
	if textReply == nil {
		t.Fatalf("no text reply in sends: %+v", sends)
	}
	if textReply.RootID != "om_user_1" {
		t.Errorf("rootID = %q, want om_user_1", textReply.RootID)
	}
	if textReply.Text != "✶ Compacting conversation…" {
		t.Errorf("body = %q, want %q", textReply.Text, "✶ Compacting conversation…")
	}
}

// TestSend_OutText_FoldsIntoReceipt — F-34 regression guard.
// OutText / OutResult / OutInit / OutUsage must still fold into
// the receipt card (unchanged behavior).
func TestSend_OutText_FoldsIntoReceipt(t *testing.T) {
	a := testAdapter(t)
	userMsgID := "om_user_out"

	var cards int
	a.sendFunc = func(_ context.Context, _, _, _, _ string) (string, error) {
		cards++
		return fmt.Sprintf("om_card_%d", cards), nil
	}
	a.updateFunc = func(_ context.Context, _, _ string) error { return nil }

	// Warm up the receipt.
	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind:    gateway.OutText,
		ChatID:  "oc_test",
		ReplyTo: userMsgID,
		Text:    "warmup",
	}); err != nil {
		t.Fatalf("Send(OutText warmup): %v", err)
	}

	for _, kind := range []gateway.OutboundKind{
		gateway.OutResult,
		gateway.OutUsage,
		gateway.OutInit,
	} {
		if err := a.Send(t.Context(), gateway.OutboundMessage{
			Kind:    kind,
			ChatID:  "oc_test",
			ReplyTo: userMsgID,
			Text:    "x",
			Meta: map[string]any{
				"session_id":    "s_1",
				"model":         "claude-sonnet-4-5",
				"agent_name":    "claude",
				"workspace":     "/tmp",
				"branch":        "main",
				"input_tokens":  10,
				"output_tokens": 5,
			},
		}); err != nil {
			t.Fatalf("Send(%v): %v", kind, err)
		}
	}

	a.mu.RLock()
	rcpt := a.receiptsByUserMsgID[userMsgID]
	a.mu.RUnlock()
	if rcpt == nil {
		t.Fatalf("receipt not registered for %s", userMsgID)
	}
	rcpt.mu.Lock()
	defer rcpt.mu.Unlock()
	if len(rcpt.entries) == 0 {
		t.Fatalf("receipt has no entries; OutText/OutResult/OutInit/OutUsage should fold in")
	}
}

// TestSend_OutCard_PassesReplyTo — v1.3.x (§13.10). OutCard (permission
// request) must thread to the user's message via Feishu root_id.
func TestSend_OutCard_PassesReplyTo(t *testing.T) {
	a := testAdapter(t)

	var captured struct {
		ChatID  string
		MsgType string
		RootID  string
	}
	a.sendFunc = func(_ context.Context, chatID, msgType, _, rootID string) (string, error) {
		captured.ChatID = chatID
		captured.MsgType = msgType
		captured.RootID = rootID
		return "om_card_test", nil
	}

	card := &gateway.Card{
		Title: "Permission needed",
		Body:  "Allow Bash?",
		Options: []string{
			"allow", "deny",
		},
	}
	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind:    gateway.OutCard,
		ChatID:  "oc_test",
		ReplyTo: "om_user_1",
		Card:    card,
	}); err != nil {
		t.Fatalf("Send(OutCard): %v", err)
	}

	if captured.RootID != "om_user_1" {
		t.Errorf("sendFunc.RootID = %q, want %q", captured.RootID, "om_user_1")
	}
	if captured.MsgType != "interactive" {
		t.Errorf("sendFunc.MsgType = %q, want %q", captured.MsgType, "interactive")
	}
	if captured.ChatID != "oc_test" {
		t.Errorf("sendFunc.ChatID = %q, want %q", captured.ChatID, "oc_test")
	}
}

// TestSend_OutCommandReply_PassesReplyTo — v1.3.x (§13.10). Slash
// command replies must thread to the user's /command message.
func TestSend_OutCommandReply_PassesReplyTo(t *testing.T) {
	a := testAdapter(t)

	var captured struct {
		ChatID string
		Text   string
		RootID string
	}
	a.sendFunc = func(_ context.Context, chatID, _, content, rootID string) (string, error) {
		captured.ChatID = chatID
		// content is JSON-encoded text payload; extract for assertion
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal([]byte(content), &payload)
		captured.Text = payload.Text
		captured.RootID = rootID
		return "om_text_test", nil
	}

	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind:    gateway.OutCommandReply,
		ChatID:  "oc_test",
		ReplyTo: "om_cmd_1",
		Text:    "available agents: main, codegraph",
	}); err != nil {
		t.Fatalf("Send(OutCommandReply): %v", err)
	}

	if captured.RootID != "om_cmd_1" {
		t.Errorf("sendFunc.RootID = %q, want %q (slash command reply must thread)", captured.RootID, "om_cmd_1")
	}
	if captured.Text != "available agents: main, codegraph" {
		t.Errorf("sendFunc.Text = %q, want %q", captured.Text, "available agents: main, codegraph")
	}
}

// TestSendViaLark_RootIdSet — v1.3.x (§13.10). When rootID is non-empty,
// sendViaLark must dispatch to Message.Reply (which uses path :message_id
// as the Feishu root_id) instead of Message.Create. PatchMessage preserves
// the thread across subsequent in-place updates.
func TestSendViaLark_RootIdSet(t *testing.T) {
	a := testAdapter(t)
	// Minimal larkClient stand-in: assert we don't crash when rootID is
	// non-empty; the full integration is exercised by E2E tests against
	// a real Feishu bot. Here we just verify the sendContent → sendFunc
	// plumbing passes rootID end-to-end.

	var gotRoot string
	a.sendFunc = func(_ context.Context, _, _, _, rootID string) (string, error) {
		gotRoot = rootID
		return "om_msg_test", nil
	}

	if _, err := a.sendContent(t.Context(), "oc_test", "text", `{"text":"hi"}`, "om_user_42"); err != nil {
		t.Fatalf("sendContent with rootID: %v", err)
	}
	if gotRoot != "om_user_42" {
		t.Errorf("sendFunc received rootID=%q, want %q", gotRoot, "om_user_42")
	}

	// Empty rootID: still flows through sendFunc with "".
	gotRoot = ""
	if _, err := a.sendContent(t.Context(), "oc_test", "text", `{"text":"hi"}`, ""); err != nil {
		t.Fatalf("sendContent without rootID: %v", err)
	}
	if gotRoot != "" {
		t.Errorf("sendFunc received rootID=%q, want empty", gotRoot)
	}
}

// receiptFor2 was removed: OutThinking test now drives a real
// receipt via the Send dispatcher itself (OutText warmup primes
// receiptsByUserMsgID).

// TestSendViaLark_TerminalCodeFallsBackToCreate — v1.3.x
// (openclaw-lark pattern from src/core/message-unavailable.ts).
// When the Reply API returns 230011 (recalled) or 231003
// (deleted), the target user message is permanently invalid;
// we fall back to the Create path so the user still sees a
// top-level message instead of a hard drop.
func TestSendViaLark_TerminalCodeFallsBackToCreate(t *testing.T) {
	a := testAdapter(t)

	var replyCalls, createCalls int
	a.sendFunc = func(_ context.Context, _, _, _, rootID string) (string, error) {
		if rootID != "" {
			replyCalls++
			return "", errors.New("feishu: reply message failed with code 230011")
		}
		createCalls++
		return "om_created", nil
	}

	if _, err := a.sendContent(t.Context(), "oc_test", "text", `{"text":"hi"}`, "om_user_42"); err != nil {
		t.Fatalf("sendContent: %v", err)
	}
	if replyCalls != 1 {
		t.Errorf("Reply attempted = %d, want 1", replyCalls)
	}
	if createCalls != 1 {
		t.Errorf("Create fallback = %d, want 1 (fallback must fire on 230011)", createCalls)
	}

	// 231003 (deleted) must also trigger the fallback.
	replyCalls, createCalls = 0, 0
	a.sendFunc = func(_ context.Context, _, _, _, rootID string) (string, error) {
		if rootID != "" {
			replyCalls++
			return "", errors.New("feishu: reply message failed with code 231003")
		}
		createCalls++
		return "om_created", nil
	}
	if _, err := a.sendContent(t.Context(), "oc_test", "text", `{"text":"hi"}`, "om_user_42"); err != nil {
		t.Fatalf("sendContent: %v", err)
	}
	if replyCalls != 1 || createCalls != 1 {
		t.Errorf("230011 path: reply=%d create=%d, want 1/1", replyCalls, createCalls)
	}
}

// TestSendViaLark_NonTerminalErrorPropagates — when Reply fails
// with a non-terminal code (e.g., 230020 invalid-param, or a
// transport error), we MUST propagate the error so the agent
// can react. Falling back to Create on every error would silently
// degrade threading for transient issues.
func TestSendViaLark_NonTerminalErrorPropagates(t *testing.T) {
	a := testAdapter(t)

	var replyCalls, createCalls int
	a.sendFunc = func(_ context.Context, _, _, _, rootID string) (string, error) {
		if rootID != "" {
			replyCalls++
			return "", errors.New("feishu: reply message failed with code 230020")
		}
		createCalls++
		return "om_created", nil
	}

	_, err := a.sendContent(t.Context(), "oc_test", "text", `{"text":"hi"}`, "om_user_42")
	if err == nil {
		t.Fatalf("sendContent: want error (230020 must propagate), got nil")
	}
	if replyCalls != 1 {
		t.Errorf("Reply attempted = %d, want 1", replyCalls)
	}
	if createCalls != 0 {
		t.Errorf("Create fallback fired on non-terminal error: createCalls=%d, want 0", createCalls)
	}

	// Transport errors also must propagate, not silently fall back.
	replyCalls, createCalls = 0, 0
	a.sendFunc = func(_ context.Context, _, _, _, rootID string) (string, error) {
		if rootID != "" {
			replyCalls++
			return "", errors.New("feishu: reply message: connection reset by peer")
		}
		createCalls++
		return "om_created", nil
	}
	_, err = a.sendContent(t.Context(), "oc_test", "text", `{"text":"hi"}`, "om_user_42")
	if err == nil {
		t.Fatalf("transport error: want error, got nil")
	}
	if createCalls != 0 {
		t.Errorf("transport error triggered Create fallback: createCalls=%d", createCalls)
	}
}

// TestSendViaLark_NoRootIDSkipsReply — the no-rootID (top-level)
// path must never attempt Reply, even if Reply would have
// succeeded (which is a degenerate case — Reply without a real
// rootID would fail server-side, but we should never call it).
func TestSendViaLark_NoRootIDSkipsReply(t *testing.T) {
	a := testAdapter(t)

	var replyCalls, createCalls int
	a.sendFunc = func(_ context.Context, _, _, _, rootID string) (string, error) {
		if rootID != "" {
			replyCalls++
		}
		createCalls++
		return "om_created", nil
	}

	if _, err := a.sendContent(t.Context(), "oc_test", "text", `{"text":"hi"}`, ""); err != nil {
		t.Fatalf("sendContent: %v", err)
	}
	if replyCalls != 0 {
		t.Errorf("Reply invoked with empty rootID: replyCalls=%d, want 0", replyCalls)
	}
	if createCalls != 1 {
		t.Errorf("Create invoked = %d, want 1", createCalls)
	}
}

// TestIsFeishuTerminalMessageCode — direct unit test for the
// terminal-code detector that gates the fallback.
func TestIsFeishuTerminalMessageCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"230011 recalled", errors.New("feishu: reply message failed with code 230011"), true},
		{"231003 deleted", errors.New("feishu: reply message failed with code 231003"), true},
		{"230020 invalid-param (NOT terminal)", errors.New("feishu: reply message failed with code 230020"), false},
		{"transport error (NOT terminal)", errors.New("feishu: reply message: connection reset"), false},
		{"unrelated", errors.New("some other error"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFeishuTerminalMessageCode(tc.err); got != tc.want {
				t.Errorf("isFeishuTerminalMessageCode(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
