package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	larkdispatcher "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/cnlangzi/nightme/internal/agent"
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
	a.sendFunc = func(_ context.Context, chatID, msgType, content, rootID string, _ bool) (string, error) {
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
// agent.PromptSucceeded) silently dropped inside receipt.Append. This test
// reproduces that path: turn 1 completes, turn 2 starts, and we
// assert turn 2 cold-creates its own receipt.
func TestSend_OutThinking_PostsMarkdownCard(t *testing.T) {
	a := testAdapter(t)

	type captured struct {
		ChatID  string
		MsgType string
		RootID  string
		Content string
	}
	var sends []captured
	a.sendFunc = func(_ context.Context, chatID, msgType, content, rootID string, _ bool) (string, error) {
		sends = append(sends, captured{chatID, msgType, rootID, content})
		return "om_card_test", nil
	}

	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind:    gateway.OutThinking,
		ChatID:  "oc_test",
		ReplyTo: "om_user_1",
		Text:    "let me think",
	}); err != nil {
		t.Fatalf("Send(OutThinking): %v", err)
	}

	// Find the interactive-card reply (skip the cold-start card
	// created by the receipt).
	var cardReply *captured
	for i := range sends {
		if sends[i].MsgType == larkim.MsgTypeInteractive {
			cardReply = &sends[i]
			break
		}
	}
	if cardReply == nil {
		t.Fatalf("no interactive card reply found in sends: %+v", sends)
	}
	if cardReply.RootID != "om_user_1" {
		t.Errorf("rootID = %q, want om_user_1 (must thread to user message)", cardReply.RootID)
	}

	// Card payload must be Card 2.0 with lark_md content that
	// includes the 💭 prefix and the original thinking text.
	var card struct {
		Config   map[string]any `json:"config"`
		Elements []struct {
			Tag  string `json:"tag"`
			Text struct {
				Tag     string `json:"tag"`
				Content string `json:"content"`
			} `json:"text"`
		} `json:"elements"`
	}
	if err := json.Unmarshal([]byte(cardReply.Content), &card); err != nil {
		t.Fatalf("card payload is not valid JSON: %v\n%s", err, cardReply.Content)
	}
	if len(card.Elements) != 1 {
		t.Fatalf("card.elements len = %d, want 1 (short thinking body stays single-div)", len(card.Elements))
	}
	if card.Elements[0].Text.Tag != "lark_md" {
		t.Errorf("card.elements[0].text.tag = %q, want %q (markdown element)",
			card.Elements[0].Text.Tag, "lark_md")
	}
	if card.Elements[0].Text.Content != "💭 let me think" {
		t.Errorf("card.elements[0].text.content = %q, want %q",
			card.Elements[0].Text.Content, "💭 let me think")
	}

	// Negative assertion: no plain-text reply must be emitted
	// for OutThinking (the pre-F-think behaviour). OutThinking
	// now goes through interactive cards exclusively.
	for _, s := range sends {
		if s.MsgType == larkim.MsgTypeText {
			t.Errorf("OutThinking produced a plain-text reply: %+v (want interactive only)", s)
		}
	}
}

// TestSend_OutToolStart_PostsToThread — F-34. OutToolStart is
// routed to a thread reply with the body "🔧 name(args)".
func TestSend_OutToolStart_PostsToThread(t *testing.T) {
	a := testAdapter(t)

	type captured struct {
		MsgType string
		RootID  string
		Text    string
	}
	var sends []captured
	a.sendFunc = func(_ context.Context, _, msgType, content, rootID string, _ bool) (string, error) {
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
func TestSend_OutToolEnd_PostsToThread(t *testing.T) {
	a := testAdapter(t)

	type captured struct {
		MsgType string
		RootID  string
		Text    string
	}
	var sends []captured
	a.sendFunc = func(_ context.Context, _, msgType, content, rootID string, _ bool) (string, error) {
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
func TestSend_OutCompaction_PostsToThread(t *testing.T) {
	a := testAdapter(t)

	type captured struct {
		MsgType string
		RootID  string
		Text    string
	}
	var sends []captured
	a.sendFunc = func(_ context.Context, _, msgType, content, rootID string, _ bool) (string, error) {
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

// TestSend_ThreadOnlyEvents_PassReplyInThreadTrue — F-37.
// OutThinking / OutToolStart / OutToolEnd are the "agent progress
// stream" kinds that would otherwise flood the user's main chat.
// Each must thread the reply AND set reply_in_thread=true so the
// message body stays out of the main chat (only the thread panel
// collects the 💭/●/⎿ lines; the main chat shows just a
// "X replies" indicator).
//
// Note: OutCompaction was originally in this set but moved to
// ReplyInThreadAndChat on 2026-08-04 (ops decision: a brief
// "✶ Compacting…" line in main chat is informative, not noise).
// It's now covered by TestSend_ChatVisibleEvents_PassReplyInThreadFalse
// → t.Run("OutCompaction", …).
//
// One table-driven test that exercises the three kinds so a future
// regression in any one of them flags here. Each kind produces its
// own cold-start card + PATCH side-effect via the receipt; we
// filter to the text reply (msg_type=text) for OutToolStart /
// OutToolEnd the same way the existing per-kind tests do, then
// assert on the
// captured replyInThread. OutThinking is rendered as an interactive
// lark_md card (F-think §3.1.2), so the assertion counts the
// interactive reply instead and inspects its content for lark_md.
func TestSend_ThreadOnlyEvents_PassReplyInThreadTrue(t *testing.T) {
	type tc struct {
		name string
		msg  gateway.OutboundMessage
		want string // expected thread reply body (for text replies)
	}
	cases := []tc{
		{
			name: "OutThinking",
			msg: gateway.OutboundMessage{
				Kind: gateway.OutThinking, ChatID: "oc_t", ReplyTo: "om_user_t",
				Text: "let me check…",
			},
			want: "💭 let me check…",
		},
		{
			name: "OutToolStart",
			msg: gateway.OutboundMessage{
				Kind: gateway.OutToolStart, ChatID: "oc_t", ReplyTo: "om_user_t",
				Tool: &gateway.ToolInfo{Name: "Read", Args: "/a.go"},
			},
			want: "● Read(/a.go)",
		},
		{
			name: "OutToolEnd",
			msg: gateway.OutboundMessage{
				Kind: gateway.OutToolEnd, ChatID: "oc_t", ReplyTo: "om_user_t",
				Tool: &gateway.ToolInfo{Name: "Read", Output: "x\ny"},
			},
			// summarizeToolResult("Read", "x\ny", nil) → "⎿  📄 Read → 2 lines"
			// We don't hard-code the line — assert via prefix below.
			want: "",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			a := testAdapter(t)

			var threadOnlyText int
			var threadOnlyCard int
			var chatVisible int
			var lastTextBody string
			var cardBodies []string // capture ALL interactive payloads (cold-start may add one)
			a.sendFunc = func(_ context.Context, _, msgType, content, rootID string, replyInThread bool) (string, error) {
				if msgType == larkim.MsgTypeText && rootID == "om_user_t" {
					var payload struct {
						Text string `json:"text"`
					}
					_ = json.Unmarshal([]byte(content), &payload)
					lastTextBody = payload.Text
					if replyInThread {
						threadOnlyText++
					} else {
						chatVisible++
					}
				}
				if msgType == larkim.MsgTypeInteractive && rootID == "om_user_t" {
					cardBodies = append(cardBodies, content)
					if replyInThread {
						threadOnlyCard++
					}
				}
				return "om_text_t", nil
			}

			if err := a.Send(t.Context(), c.msg); err != nil {
				t.Fatalf("Send(%s): %v", c.name, err)
			}
			// OutThinking: thread-routed interactive card (F-think).
			if c.name == "OutThinking" {
				if threadOnlyCard != 1 {
					t.Errorf("%s: threaded card count = %d (reply_in_thread=true), want 1", c.name, threadOnlyCard)
				}
				if threadOnlyText != 0 {
					t.Errorf("%s: plain-text reply count = %d, want 0 (OutThinking uses interactive cards only)", c.name, threadOnlyText)
				}
				if chatVisible != 0 {
					t.Errorf("%s: chat-visible text reply count = %d (reply_in_thread=false), want 0 — OutThinking must NEVER appear in main chat", c.name, chatVisible)
				}
				// The receipt also posts a cold-start card through sendFunc;
				// find the thinking card by its lark_md marker
				// rather than relying on order.
				var thinkingCard string
				for _, body := range cardBodies {
					if strings.Contains(body, "lark_md") {
						thinkingCard = body
						break
					}
				}
				if thinkingCard == "" {
					t.Fatalf("%s: no interactive card contained lark_md; got %d cards", c.name, len(cardBodies))
				}
				// The thinking card structure produced by
				// buildThinkingCard is:
				//   {config, elements: [{tag:"div", text:{tag:"lark_md", content}}]}
				var card struct {
					Config   map[string]any `json:"config"`
					Elements []struct {
						Tag  string `json:"tag"`
						Text struct {
							Tag     string `json:"tag"`
							Content string `json:"content"`
						} `json:"text"`
					} `json:"elements"`
				}
				if err := json.Unmarshal([]byte(thinkingCard), &card); err != nil {
					t.Fatalf("%s: thinking card JSON invalid: %v\n%s", c.name, err, thinkingCard)
				}
				if len(card.Elements) != 1 {
					t.Fatalf("%s: elements len = %d, want 1", c.name, len(card.Elements))
				}
				if card.Elements[0].Text.Tag != "lark_md" {
					t.Errorf("%s: elements[0].text.tag = %q, want %q",
						c.name, card.Elements[0].Text.Tag, "lark_md")
				}
				if card.Elements[0].Text.Content != c.want {
					t.Errorf("%s: elements[0].text.content = %q, want %q",
						c.name, card.Elements[0].Text.Content, c.want)
				}
				return
			}
			if threadOnlyText != 1 {
				t.Errorf("%s: threaded text reply count = %d (reply_in_thread=true), want 1", c.name, threadOnlyText)
			}
			if chatVisible != 0 {
				t.Errorf("%s: chat-visible text reply count = %d (reply_in_thread=false), want 0 — main chat must NOT show the progress line", c.name, chatVisible)
			}
			if c.want != "" && lastTextBody != c.want {
				t.Errorf("%s: body = %q, want %q", c.name, lastTextBody, c.want)
			}
			if c.name == "OutToolEnd" && !strings.HasPrefix(lastTextBody, "⎿  📄 Read") {
				t.Errorf("OutToolEnd: body = %q, want prefix %q", lastTextBody, "⎿  📄 Read")
			}
		})
	}
}

// TestSend_ChatVisibleEvents_PassReplyInThreadFalse — F-37 negative.
// The following paths must NOT set reply_in_thread=true (the
// message must stay visible in the main chat):
//
//   - receipt cold-start card (the pinned answer card)
//   - OutCard (permission card — discoverability > chat cleanliness)
//   - OutCommandReply (slash command — user is waiting at the cursor)
//
// Without this guarantee, a future refactor that decides
// "reply_in_thread=true everywhere" would silently hide the
// receipt card behind a thread indicator, breaking the core UX.
func TestSend_ChatVisibleEvents_PassReplyInThreadFalse(t *testing.T) {
	// F-44: ReceiptLazyCreate subtest removed — ensureReceiptForReply
	// F-44 follow-up: OutCard / OutCommandReply moved off
	// ReplyInBoth to ReplyInChat (top-level Create). They no
	// longer have a thread / anchor concept, so this test
	// (which asserts reply_in_thread=false + anchored to
	// userMsgID) doesn't apply. The dispatch is locked in by
	// TestSendViaLark_TopLevelCreate_Dispatch and the dedicated
	// per-kind emoji-prefix tests
	// (TestSend_OutCard_TopLevelCreate_EmojiPrefixed /
	// TestSend_OutCommandReply_TopLevelCreate_EmojiPrefixed).

	// OutCompaction moved here from
	// TestSend_ThreadOnlyEvents_PassReplyInThreadTrue on
	// 2026-08-04 (ops: a brief "✶ Compacting…" line in main chat
	// is informative, not noise). Still anchored via ReplyInBoth
	// (low-frequency one-shot marker — thread-pull is acceptable
	// since the marker is rare and the user knows what it
	// means).
	t.Run("OutCompaction", func(t *testing.T) {
		a := testAdapter(t)
		var threadOnly int
		var chatVisible int
		a.sendFunc = func(_ context.Context, _, msgTypeRaw, _, rootID string, replyInThread bool) (string, error) {
			if msgTypeRaw == larkim.MsgTypeText && rootID == "om_user_compact" {
				if replyInThread {
					threadOnly++
				} else {
					chatVisible++
				}
			}
			return "om_text_compact", nil
		}

		err := a.Send(t.Context(), gateway.OutboundMessage{
			Kind:    gateway.OutCompaction,
			ChatID:  "oc_test",
			ReplyTo: "om_user_compact",
		})
		if err != nil {
			t.Fatalf("Send(OutCompaction): %v", err)
		}
		if chatVisible == 0 {
			t.Errorf("OutCompaction reply_in_thread flag was true (threaded), want false — compaction marker should be visible in main chat")
		}
		if threadOnly != 0 {
			t.Errorf("OutCompaction was threaded %d times, want 0", threadOnly)
		}
	})
}

// TestSend_OutText_FoldsIntoReceipt — F-34 regression guard.
// OutReply / OutResult / OutInit / OutUsage must still fold into
// the receipt card (unchanged behavior).
// TestSend_OutCard_TopLevelCreate_EmojiPrefixed — F-44 follow-up:
// OutCard is a top-level Create (ReplyInChat, rootID="") with the
// 🔐 emoji prepended to the card title. The card is a blocking UI
// element (user must click Allow/Deny) and must stay visible in
// main chat regardless of whether the parent user message has a
// tool thread. The emoji is the channel's visual decoration (so
// users can scan main chat and immediately see "this is a
// permission request"); the abstract gateway.Card.Title is the
// undecorated plain title.
func TestSend_OutCard_TopLevelCreate_EmojiPrefixed(t *testing.T) {
	a := testAdapter(t)

	var captured struct {
		ChatID  string
		MsgType string
		RootID  string
		Body    string
	}
	a.sendFunc = func(_ context.Context, chatID, msgType, body, rootID string, _ bool) (string, error) {
		captured.ChatID = chatID
		captured.MsgType = msgType
		captured.RootID = rootID
		captured.Body = body
		return "om_card_test", nil
	}

	card := &gateway.Card{
		Title:   "Permission needed",
		Body:    "Allow Bash?",
		Options: []string{"allow", "deny"},
	}
	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind:    gateway.OutCard,
		ChatID:  "oc_test",
		ReplyTo: "om_user_1", // F-44 follow-up: ReplyTo is ignored
		Card:    card,
	}); err != nil {
		t.Fatalf("Send(OutCard): %v", err)
	}

	// F-44 follow-up: top-level Create, no parent/thread anchor.
	if captured.RootID != "" {
		t.Errorf("sendFunc.RootID = %q, want %q (F-44 follow-up: OutCard is top-level Create, no anchor)",
			captured.RootID, "")
	}
	if captured.MsgType != "interactive" {
		t.Errorf("sendFunc.MsgType = %q, want %q", captured.MsgType, "interactive")
	}
	if captured.ChatID != "oc_test" {
		t.Errorf("sendFunc.ChatID = %q, want %q", captured.ChatID, "oc_test")
	}
	// Body must contain the 🔐-prefixed title (channel decoration).
	if !strings.Contains(captured.Body, "🔐 Permission needed") {
		t.Errorf("card body missing 🔐 emoji prefix on title; got body: %s", captured.Body)
	}
}

// TestSend_OutCommandReply_TopLevelCreate_EmojiPrefixed — F-44
// follow-up: OutCommandReply is a top-level Create (ReplyInChat,
// rootID="") with the ❯ emoji prepended to the text body. Same
// rationale as OutCard — slash-command replies are short status
// messages, anchoring them to the user message is unnecessary, and
// a thread-on-parent would pull them into the drawer. The ❯ emoji
// mirrors the 💭 prefix OutThinking uses (visual channel
// decoration for "this is a thinking / command response" — easy
// to scan in main chat).
func TestSend_OutCommandReply_TopLevelCreate_EmojiPrefixed(t *testing.T) {
	a := testAdapter(t)

	var captured struct {
		ChatID string
		Text   string
		RootID string
	}
	a.sendFunc = func(_ context.Context, chatID, _, content, rootID string, _ bool) (string, error) {
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
		ReplyTo: "om_cmd_1", // F-44 follow-up: ReplyTo is ignored
		Text:    "available agents: main, codegraph",
	}); err != nil {
		t.Fatalf("Send(OutCommandReply): %v", err)
	}

	// F-44 follow-up: top-level Create, no parent/thread anchor.
	if captured.RootID != "" {
		t.Errorf("sendFunc.RootID = %q, want %q (F-44 follow-up: OutCommandReply is top-level Create, no anchor)",
			captured.RootID, "")
	}
	// Text must be ❯-prefixed.
	if captured.Text != "❯ available agents: main, codegraph" {
		t.Errorf("sendFunc.Text = %q, want %q (F-44 follow-up: ❯ emoji prefix)",
			captured.Text, "❯ available agents: main, codegraph")
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
	a.sendFunc = func(_ context.Context, _, _, _, rootID string, _ bool) (string, error) {
		gotRoot = rootID
		return "om_msg_test", nil
	}

	if _, err := a.sendContent(t.Context(), "oc_test", "text", `{"text":"hi"}`, "om_user_42", false); err != nil {
		t.Fatalf("sendContent with rootID: %v", err)
	}
	if gotRoot != "om_user_42" {
		t.Errorf("sendFunc received rootID=%q, want %q", gotRoot, "om_user_42")
	}

	// Empty rootID: still flows through sendFunc with "".
	gotRoot = ""
	if _, err := a.sendContent(t.Context(), "oc_test", "text", `{"text":"hi"}`, "", false); err != nil {
		t.Fatalf("sendContent without rootID: %v", err)
	}
	if gotRoot != "" {
		t.Errorf("sendFunc received rootID=%q, want empty", gotRoot)
	}
}

// receiptFor2 was removed: OutThinking test now drives a real
// receipt via the Send dispatcher itself (OutReply warmup primes
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
	a.sendFunc = func(_ context.Context, _, _, _, rootID string, _ bool) (string, error) {
		if rootID != "" {
			replyCalls++
			return "", errors.New("feishu: reply message failed with code 230011")
		}
		createCalls++
		return "om_created", nil
	}

	if _, err := a.sendContent(t.Context(), "oc_test", "text", `{"text":"hi"}`, "om_user_42", false); err != nil {
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
	a.sendFunc = func(_ context.Context, _, _, _, rootID string, _ bool) (string, error) {
		if rootID != "" {
			replyCalls++
			return "", errors.New("feishu: reply message failed with code 231003")
		}
		createCalls++
		return "om_created", nil
	}
	if _, err := a.sendContent(t.Context(), "oc_test", "text", `{"text":"hi"}`, "om_user_42", false); err != nil {
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
	a.sendFunc = func(_ context.Context, _, _, _, rootID string, _ bool) (string, error) {
		if rootID != "" {
			replyCalls++
			return "", errors.New("feishu: reply message failed with code 230020")
		}
		createCalls++
		return "om_created", nil
	}

	_, err := a.sendContent(t.Context(), "oc_test", "text", `{"text":"hi"}`, "om_user_42", false)
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
	a.sendFunc = func(_ context.Context, _, _, _, rootID string, _ bool) (string, error) {
		if rootID != "" {
			replyCalls++
			return "", errors.New("feishu: reply message: connection reset by peer")
		}
		createCalls++
		return "om_created", nil
	}
	_, err = a.sendContent(t.Context(), "oc_test", "text", `{"text":"hi"}`, "om_user_42", false)
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
	a.sendFunc = func(_ context.Context, _, _, _, rootID string, _ bool) (string, error) {
		if rootID != "" {
			replyCalls++
		}
		createCalls++
		return "om_created", nil
	}

	if _, err := a.sendContent(t.Context(), "oc_test", "text", `{"text":"hi"}`, "", false); err != nil {
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
// terminal-code detector that gates the fallback. Covers all three
// accepted code-suffix formats: legacy "code NNNNN" (sendViaLarkReply,
// kept for any in-flight log greps), colon "code:NNNNN", and the PR #47
// "code=NNNNN" produced by ReplyInBoth / ReplyInThread.
func TestIsFeishuTerminalMessageCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		// Legacy sendViaLarkReply format.
		{"230011 recalled (legacy)", errors.New("feishu: reply message failed with code 230011"), true},
		{"231003 deleted (legacy)", errors.New("feishu: reply message failed with code 231003"), true},
		// PR #47 ReplyInBoth / ReplyInThread format.
		{"230011 recalled (PR47 code=)", errors.New("feishu: ReplyInBoth failed: code=230011 msg=msg has been deleted"), true},
		{"231003 deleted (PR47 code=)", errors.New("feishu: ReplyInThread failed: code=231003 msg=message not found"), true},
		// Colon form (defensive).
		{"230011 recalled (colon)", errors.New("upstream wrapper: code:230011"), true},
		// Negative cases.
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

// TestSendViaLark_ReplyInBoth_Dispatch — PR #47 wiring proof.
// sendViaLark must route user-visible kinds through ReplyInBoth,
// i.e. the reply endpoint with reply_in_thread field omitted.
// This is verified by asserting replyInThread=false on the captured
// sendFunc invocation. The wire helpers are exercised via the
// shared sendContent → sendFunc hook so the test exercises the
// production dispatch path end-to-end.
//
// F-44 follow-up: OutReply / OutResult / OutCard / OutCommandReply
// moved OFF ReplyInBoth because of the parent-thread gotcha —
// once OutToolStart/End creates a thread on the user message,
// Feishu pulls all subsequent ReplyInBoth calls into the thread
// panel. They now route through ReplyInChat (top-level Create).
// See TestSendViaLark_TopLevelCreate_Dispatch for those paths.
func TestSendViaLark_ReplyInBoth_Dispatch(t *testing.T) {
	cases := []struct {
		name string
		msg  gateway.OutboundMessage
	}{
		// OutCompaction is one of the few user-visible kinds
		// that still anchors to userMsgID via ReplyInBoth.
		// It's a low-frequency one-shot marker ("✶
		// Compacting…") so a thread-pull is acceptable; plus
		// it's intentionally non-thread so the user sees
		// the marker inline in main chat.
		{
			name: "OutCompaction",
			msg: gateway.OutboundMessage{
				Kind:    gateway.OutCompaction,
				ChatID:  "oc_test",
				ReplyTo: "om_user_5",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := testAdapter(t)
			var capturedReplyInThread bool
			var capturedRootID string
			a.sendFunc = func(_ context.Context, _, _, _, rootID string, replyInThread bool) (string, error) {
				capturedReplyInThread = replyInThread
				capturedRootID = rootID
				return "om_ok", nil
			}
			if err := a.Send(t.Context(), tc.msg); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if capturedRootID != tc.msg.ReplyTo {
				t.Errorf("rootID = %q, want %q (PR #47: ReplyInBoth must anchor on userMsgID)",
					capturedRootID, tc.msg.ReplyTo)
			}
			if capturedReplyInThread {
				t.Errorf("replyInThread = true, want false (PR #47: %s must route via ReplyInBoth, not ReplyInThread)",
					tc.name)
			}
		})
	}
}

// TestSendViaLark_ReplyInThread_Dispatch — PR #47 wiring proof.
// sendViaLark must route the agent-progress kinds (OutThinking /
// OutToolStart / OutToolEnd) through ReplyInThread, i.e. the reply
// endpoint with reply_in_thread=true. OutCompaction stays in main
// chat (replyOnly=false) and therefore routes through ReplyInBoth
// — see TestSendViaLark_ReplyInBoth_Dispatch for that path.
// This is the negation of TestSendViaLark_ReplyInBoth_Dispatch —
// verifies the replyInThread=true wire path is preserved when F-44
// collapses the sendViaLarkReply wrapper into
// ReplyInBoth/ReplyInThread.
func TestSendViaLark_ReplyInThread_Dispatch(t *testing.T) {
	cases := []struct {
		name string
		msg  gateway.OutboundMessage
	}{
		{
			name: "OutThinking",
			msg: gateway.OutboundMessage{
				Kind:    gateway.OutThinking,
				ChatID:  "oc_test",
				ReplyTo: "om_user_1",
				Text:    "considering",
			},
		},
		{
			name: "OutToolStart",
			msg: gateway.OutboundMessage{
				Kind:    gateway.OutToolStart,
				ChatID:  "oc_test",
				ReplyTo: "om_user_2",
				Tool:    &gateway.ToolInfo{Name: "Bash", Args: "ls"},
			},
		},
		{
			name: "OutToolEnd",
			msg: gateway.OutboundMessage{
				Kind:    gateway.OutToolEnd,
				ChatID:  "oc_test",
				ReplyTo: "om_user_3",
				Tool:    &gateway.ToolInfo{Name: "Bash", Output: "file1\nfile2"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := testAdapter(t)
			var capturedReplyInThread bool
			var capturedRootID string
			a.sendFunc = func(_ context.Context, _, _, _, rootID string, replyInThread bool) (string, error) {
				capturedReplyInThread = replyInThread
				capturedRootID = rootID
				return "om_ok", nil
			}
			if err := a.Send(t.Context(), tc.msg); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if capturedRootID != tc.msg.ReplyTo {
				t.Errorf("rootID = %q, want %q (PR #47: ReplyInThread must anchor on userMsgID)",
					capturedRootID, tc.msg.ReplyTo)
			}
			if !capturedReplyInThread {
				t.Errorf("replyInThread = false, want true (PR #47: %s must route via ReplyInThread, not ReplyInBoth)",
					tc.name)
			}
		})
	}
}

// TestSendViaLark_TopLevelCreate_Dispatch — F-44 follow-up wiring proof.
// sendViaLark must route user-visible kinds (OutTaskCreate /
// OutTaskUpdate) through ReplyInChat, i.e. top-level Create with
// rootID="" so the cold-start task card is a standalone bubble in
// main chat. This is verified by asserting rootID="" on the captured
// sendFunc invocation. Without this guard, a future regression that
// re-anchors the task card to the user message would not be caught —
// and the parent-thread gotcha would silently reappear.
//
// Why OutTask* is in this set even though it's "single card" not
// "chunk stream": the gotcha is the same. Once OutToolStart/End
// (ReplyInThread) creates a thread on the user message, Feishu pulls
// the cold-start task card into the thread drawer regardless of how
// many events the card accumulates afterward. Same rule for the
// cold-start card as for individual reply chunks: stay top-level.
//
// (OutReply / OutResult are also ReplyInChat; they're covered by
// TestSend_OutReply_IndependentReply_NotFolded and
// TestSend_OutResult_OrphanTopLevel which assert the same rootID=""
// invariant at the call site. OutTask* is added here because it
// wasn't covered at all before this commit.)
func TestSendViaLark_TopLevelCreate_Dispatch(t *testing.T) {
	cases := []struct {
		name string
		msg  gateway.OutboundMessage
	}{
		{
			name: "OutTaskCreate",
			msg: gateway.OutboundMessage{
				Kind:    gateway.OutTaskCreate,
				ChatID:  "oc_test",
				ReplyTo: "om_user_1",
				TaskList: &agent.TaskListEvent{Items: []agent.TaskItem{
					{Subject: "step 1", Status: agent.TaskPending, ActiveForm: "doing 1"},
				}},
			},
		},
		{
			name: "OutTaskUpdate",
			msg: gateway.OutboundMessage{
				Kind:    gateway.OutTaskUpdate,
				ChatID:  "oc_test",
				ReplyTo: "om_user_2",
				TaskList: &agent.TaskListEvent{Items: []agent.TaskItem{
					{Subject: "step 1", Status: agent.TaskCompleted, ActiveForm: "doing 1"},
				}},
			},
		},
		{
			name: "OutCard (permission)",
			msg: gateway.OutboundMessage{
				Kind:    gateway.OutCard,
				ChatID:  "oc_test",
				ReplyTo: "om_user_3", // F-44 follow-up: ReplyTo is ignored
				Card: &gateway.Card{
					Title:   "Allow Bash?",
					Options: []string{"Allow", "Deny"},
				},
			},
		},
		{
			name: "OutCommandReply",
			msg: gateway.OutboundMessage{
				Kind:    gateway.OutCommandReply,
				ChatID:  "oc_test",
				ReplyTo: "om_user_4", // F-44 follow-up: ReplyTo is ignored
				Text:    "/help result",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := testAdapter(t)
			var capturedRootID string
			var capturedReplyInThread bool
			a.sendFunc = func(_ context.Context, _, _, _, rootID string, replyInThread bool) (string, error) {
				capturedRootID = rootID
				capturedReplyInThread = replyInThread
				return "om_ok", nil
			}
			a.updateFunc = func(_ context.Context, _, _ string) error { return nil }
			if err := a.Send(t.Context(), tc.msg); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if capturedRootID != "" {
				t.Errorf("rootID = %q, want %q (F-44 follow-up: %s must route via ReplyInChat / top-level Create, no anchor)",
					capturedRootID, "", tc.name)
			}
			if capturedReplyInThread {
				t.Errorf("replyInThread = true, want false (F-44 follow-up: %s top-level Create ignores reply_in_thread)",
					tc.name)
			}
		})
	}
}

// ===========================================================================
// F-39: OutResult → independent reply tests
// ===========================================================================

// TestSend_OutResult_GoesToNewReply_NotReceipt — F-39 reverse-section proof.
// OutResult must NOT fold into the rolling-log receipt card. It must be
// delivered as a separate reply anchored at userMsgID via sendResultAsReply.
func TestSend_OutResult_LongMarkdownUsesInteractiveCard(t *testing.T) {
	a := testAdapter(t)
	var gotType string
	a.sendFunc = func(_ context.Context, _, msgType, _, _ string, _ bool) (string, error) {
		gotType = msgType
		return "ok", nil
	}

	longText := "intro paragraph\n\n```go\nfunc x() { return 1 }\n```\n\nbody text after"
	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind:    gateway.OutResult,
		ChatID:  "oc_test",
		ReplyTo: "om_x",
		Text:    longText,
		Result:  &agent.ResultEvent{Text: longText},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotType != "interactive" {
		t.Errorf("markdown content should dispatch to interactive, got %q", gotType)
	}
}

// TestSend_OutResult_NoMarkdownUsesText — F-39 dispatch path 1.
func TestSend_OutResult_NoMarkdownUsesText(t *testing.T) {
	a := testAdapter(t)
	var gotType, gotContent string
	a.sendFunc = func(_ context.Context, _, msgType, content, _ string, _ bool) (string, error) {
		gotType = msgType
		gotContent = content
		return "ok", nil
	}
	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind:    gateway.OutResult,
		ChatID:  "oc_test",
		ReplyTo: "om_x",
		Text:    "plain reply without any markdown markers",
		Result:  &agent.ResultEvent{Text: "plain reply without any markdown markers"},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotType != "text" {
		t.Errorf("plain text should dispatch to text, got %q", gotType)
	}
	if !strings.Contains(gotContent, "plain reply without any markdown markers") {
		t.Errorf("text body should carry the reply, got %q", gotContent)
	}
}

// TestSend_OutResult_LotsOfTablesUsesPost — F-39 dispatch path 2.
func TestSend_OutResult_LotsOfTablesUsesPost(t *testing.T) {
	a := testAdapter(t)
	var gotType string
	a.sendFunc = func(_ context.Context, _, msgType, _, _ string, _ bool) (string, error) {
		gotType = msgType
		return "ok", nil
	}
	var b strings.Builder
	for i := 0; i < 6; i++ {
		b.WriteString("| A | B |\n|---|---|\n| 1 | 2 |\n\n")
	}
	text := strings.TrimRight(b.String(), "\n")
	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind:    gateway.OutResult,
		ChatID:  "oc_test",
		ReplyTo: "om_x",
		Text:    text,
		Result:  &agent.ResultEvent{Text: text},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotType != "post" {
		t.Errorf(">5 tables should dispatch to post, got %q", gotType)
	}
}

// TestSend_OutResult_EmptySkipped — empty result with !IsError is a no-op.
func TestSend_OutResult_EmptySkipped(t *testing.T) {
	a := testAdapter(t)
	var sends int
	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		sends++
		return "ok", nil
	}
	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind:    gateway.OutResult,
		ChatID:  "oc_test",
		ReplyTo: "om_x",
		Text:    "",
		Result:  &agent.ResultEvent{Text: ""},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if sends != 0 {
		t.Errorf("empty result should be skipped, got %d sends", sends)
	}
}

// TestSend_OutResult_IsErrorPrefixedWithIcon — error results get ❌ prefix.
func TestSend_OutResult_IsErrorPrefixedWithIcon(t *testing.T) {
	a := testAdapter(t)
	var gotContent string
	a.sendFunc = func(_ context.Context, _, _, content, _ string, _ bool) (string, error) {
		gotContent = content
		return "ok", nil
	}
	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind:    gateway.OutResult,
		ChatID:  "oc_test",
		ReplyTo: "om_x",
		Text:    "agent run failed",
		Result: &agent.ResultEvent{
			Text:    "agent run failed",
			IsError: true,
		},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	// Body is JSON-encoded for MsgTypeText; decode to extract the text field.
	var envelope struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(gotContent), &envelope); err != nil {
		t.Fatalf("decode body: %v\nraw: %q", err, gotContent)
	}
	if !strings.HasPrefix(envelope.Text, "❌ ") {
		t.Errorf("error result text should be prefixed with ❌, got %q", envelope.Text)
	}
}

// TestSend_OutResult_LeavesReceiptAlive — F-39 follow-up: OutResult
// delivers the answer card independently but does NOT call
// receipt.SetCompleted. The receipt must remain in agent.PromptRunning
// after OutResult so subsequent OutUsage / OutInit / TaskList can
// still update the footer (token counts, agent name, task list).
// EventDone is the terminal signal that flips state to agent.PromptSucceeded.
func TestSend_OutResult_OrphanTopLevel(t *testing.T) {
	a := testAdapter(t)
	var gotType, gotRootID, gotContent string
	a.sendFunc = func(_ context.Context, _, msgType, content, rootID string, _ bool) (string, error) {
		gotType = msgType
		gotContent = content
		gotRootID = rootID
		return "ok", nil
	}
	text := "## Final\n\nbody"
	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind:    gateway.OutResult,
		ChatID:  "oc_test",
		ReplyTo: "", // orphan
		Text:    text,
		Result:  &agent.ResultEvent{Text: text},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotType != "text" {
		t.Errorf("orphan result should use MsgTypeText, got %q", gotType)
	}
	if gotRootID != "" {
		t.Errorf("orphan result should not anchor, got rootID %q", gotRootID)
	}
	if !strings.Contains(gotContent, "Final") {
		t.Errorf("orphan result body missing content, got %q", gotContent)
	}
}

// TestEnsureReceiptForReply_ThinkingPrefix_ReturnsError guards F-42
// review finding #1. Pre-fix the helper returned (nil, false, nil)
// when eventToEntry filtered the text (thinking-prefix or empty),
// and the OutReply caller then dereferenced the nil receipt via
// receipt.IsCompleted() → panic on r.mu.Lock(). The fix returns an
// error so the caller's existing error path (sendRawOutText
// fallback) handles it cleanly.
func TestEnsureReceiptForTask_Concurrent_OnlyOneSendCard(t *testing.T) {
	a := testAdapter(t)

	var (
		sendCardMu    sync.Mutex
		sendCardCalls int
		releaseSignal = make(chan struct{})
	)
	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		sendCardMu.Lock()
		sendCardCalls++
		sendCardMu.Unlock()
		<-releaseSignal
		return "om_task_card", nil
	}

	const userMsgID = "om_user_task_race"
	const chatID = "oc_task_race"

	listA := &agent.TaskListEvent{Items: []agent.TaskItem{
		{ID: "1", Subject: "task A", Status: agent.TaskPending},
	}}
	listB := &agent.TaskListEvent{Items: []agent.TaskItem{
		{ID: "1", Subject: "task B", Status: agent.TaskPending},
	}}

	var (
		wg       sync.WaitGroup
		errs     [2]error
		receipts [2]*MessageReceipt
		createds [2]bool
	)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			list := listA
			if i == 1 {
				list = listB
			}
			receipts[i], createds[i], errs[i] = a.ensureReceiptForTask(
				t.Context(), chatID, userMsgID, list,
			)
		}(i)
	}

	time.Sleep(50 * time.Millisecond)
	close(releaseSignal)
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("goroutine %d: err = %v", i, e)
		}
	}

	winners := 0
	for _, c := range createds {
		if c {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("created count = %d, want 1", winners)
	}

	sendCardMu.Lock()
	calls := sendCardCalls
	sendCardMu.Unlock()
	if calls != 1 {
		t.Errorf("SendCard calls = %d, want 1 (orphan-card race fix)", calls)
	}

	if receipts[0] != receipts[1] {
		t.Errorf("receipts diverge: %p vs %p; both should be the registered placeholder", receipts[0], receipts[1])
	}

	// The loser's caller will SetTaskList on the shared receipt;
	// that PATCH must hit the winner's cardMsgID (set by the
	// register-before-SendCard path), not any orphan card.
	a.mu.Lock()
	registered := a.receiptsByUserMsgID[userMsgID]
	cardMsgID := registered.cardMsgID
	a.mu.Unlock()
	if cardMsgID == "" {
		t.Errorf("registered receipt has empty cardMsgID after the ensure path completed")
	}
}


// ---------------------------------------------------------------------------
// F-44 revert: OutReply folds into the rolling-log receipt card.
// ---------------------------------------------------------------------------

// TestSend_OutReply_FoldsIntoReceipt verifies the F-44 revert
// routing change: the first OutReply chunk cold-starts a new
// rolling-log receipt card via SendCard (top-level Create, rootID="")
// so the card stays visible in main chat regardless of whether
// the parent user message has a tool thread (parent-thread
// gotcha). The chunk is already installed as the first LogEntry.
// The card body is the Card 2.0 envelope; the entry text is
// sanitized + the icon prefix is prepended.
func TestSend_OutReply_FoldsIntoReceipt(t *testing.T) {
	a := testAdapter(t)

	type captured struct {
		ChatID        string
		MsgType       string
		RootID        string
		Body          string
		ReplyInThread bool
	}
	var sends []captured
	a.sendFunc = func(_ context.Context, chatID, msgType, body, rootID string, replyInThread bool) (string, error) {
		sends = append(sends, captured{ChatID: chatID, MsgType: msgType, RootID: rootID, Body: body, ReplyInThread: replyInThread})
		return "om_card_test", nil
	}
	a.updateFunc = func(_ context.Context, _, _ string) error { return nil }

	if err := a.Send(context.Background(), gateway.OutboundMessage{
		Kind:    gateway.OutReply,
		ChatID:  "oc_test",
		ReplyTo: "om_user",
		Text:    "first chunk of the reply",
	}); err != nil {
		t.Fatalf("Send(OutReply): %v", err)
	}

	// Exactly one SendCard call (the cold-start card).
	if len(sends) != 1 {
		t.Fatalf("send count = %d, want 1 (one cold-start card)", len(sends))
	}
	got := sends[0]
	if got.ChatID != "oc_test" {
		t.Errorf("ChatID = %q, want %q", got.ChatID, "oc_test")
	}
	if got.MsgType != "interactive" {
		t.Errorf("MsgType = %q, want %q (Feishu Card 2.0)", got.MsgType, "interactive")
	}
	// Top-level Create (rootID="", reply_in_thread=false) so the
	// card stays in main chat regardless of the parent thread
	// state. The parent user message id is still used as the
	// receipt registry key but does NOT anchor the card itself.
	if got.RootID != "" {
		t.Errorf("RootID = %q, want %q (top-level Create, no anchor)", got.RootID, "")
	}
	if got.ReplyInThread {
		t.Errorf("reply_in_thread = true, want false (top-level Create)")
	}
	// Body must be Card 2.0 JSON with the chunk embedded.
	if !strings.Contains(got.Body, "💬 first chunk of the reply") {
		t.Errorf("body missing entry text (want 💬 prefix + chunk), got %q", got.Body)
	}
	// Receipt IS created — F-44 revert: OutReply folds.
	a.mu.RLock()
	rcpt, ok := a.receiptsByUserMsgID["om_user"]
	a.mu.RUnlock()
	if !ok || rcpt == nil {
		t.Errorf("receipt was NOT created for OutReply; F-44 revert: OutReply folds into receipt")
	}
	if rcpt != nil && len(rcpt.entries) != 1 {
		t.Errorf("receipt entries = %d, want 1", len(rcpt.entries))
	}
}

// TestSend_OutReply_MultipleChunks_PATCHesSameCard verifies that
// the first chunk cold-starts the card and subsequent chunks PATCH
// the same card in place (no new message per chunk).
func TestSend_OutReply_MultipleChunks_PATCHesSameCard(t *testing.T) {
	a := testAdapter(t)
	var sends int
	var patches int
	a.sendFunc = func(_ context.Context, _, msgType, _, _ string, _ bool) (string, error) {
		sends++
		return "om_card_test", nil
	}
	a.updateFunc = func(_ context.Context, _, _ string) error {
		patches++
		return nil
	}

	chunks := []string{"first", "second", "third"}
	for i, chunk := range chunks {
		if err := a.Send(context.Background(), gateway.OutboundMessage{
			Kind:    gateway.OutReply,
			ChatID:  "oc_test",
			ReplyTo: "om_user",
			Text:    chunk,
		}); err != nil {
			t.Fatalf("Send(OutReply chunk %d): %v", i, err)
		}
	}

	// 1 cold-start SendCard + 2 PATCHes (one per additional chunk).
	if sends != 1 {
		t.Errorf("send count = %d, want 1 (one cold-start card)", sends)
	}
	if patches != 2 {
		t.Errorf("patch count = %d, want 2 (one per additional chunk)", patches)
	}
	// Receipt should have all 3 entries.
	a.mu.RLock()
	rcpt := a.receiptsByUserMsgID["om_user"]
	a.mu.RUnlock()
	if rcpt == nil {
		t.Fatalf("receipt missing")
	}
	if len(rcpt.entries) != 3 {
		t.Errorf("receipt entries = %d, want 3 (all chunks folded)", len(rcpt.entries))
	}
}

// TestSend_OutReply_EmptyText_SilentDrop verifies that empty /
// whitespace-only text is silently dropped (no Feishu message sent,
// no error returned). This matches pre-F-40 behavior and avoids
// blank bubbles in main chat.
func TestSend_OutReply_EmptyText_SilentDrop(t *testing.T) {
	a := testAdapter(t)
	var count int
	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		count++
		return "om_msg", nil
	}
	a.updateFunc = func(_ context.Context, _, _ string) error { return nil }

	for _, text := range []string{"", "   ", "\n\n\t"} {
		if err := a.Send(context.Background(), gateway.OutboundMessage{
			Kind:    gateway.OutReply,
			ChatID:  "oc_test",
			ReplyTo: "om_user",
			Text:    text,
		}); err != nil {
			t.Errorf("Send(OutReply empty %q) returned err: %v", text, err)
		}
	}
	if count != 0 {
		t.Errorf("send count = %d, want 0 (all empty texts dropped)", count)
	}
}

// TestSend_OutInit_SilentDrop and TestSend_OutUsage_SilentDrop
// verify the F-44 footer deferral: OutInit / OutUsage events reach
// the Send dispatcher (the path is exercised) but produce no Feishu
// traffic at all. The Translate path still produces
// OutboundMessage{Init/Usage}; only the channel-side render is
// skipped until the footer PR lands.
func TestSend_OutInit_SilentDrop(t *testing.T) {
	a := testAdapter(t)
	var count int
	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		count++
		return "om_msg", nil
	}
	a.updateFunc = func(_ context.Context, _, _ string) error { return nil }

	if err := a.Send(context.Background(), gateway.OutboundMessage{
		Kind:    gateway.OutInit,
		ChatID:  "oc_test",
		ReplyTo: "om_user",
		Init: &agent.InitEvent{
			SessionID: "s_1",
			Model:     "claude-sonnet-4-5",
			AgentName: "claude",
			Workspace: "/tmp",
			Branch:    "main",
		},
	}); err != nil {
		t.Fatalf("Send(OutInit): %v", err)
	}
	if count != 0 {
		t.Errorf("send count = %d, want 0 (OutInit silent drop until footer PR)", count)
	}
}

func TestSend_OutUsage_SilentDrop(t *testing.T) {
	a := testAdapter(t)
	var count int
	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		count++
		return "om_msg", nil
	}
	a.updateFunc = func(_ context.Context, _, _ string) error { return nil }

	if err := a.Send(context.Background(), gateway.OutboundMessage{
		Kind:    gateway.OutUsage,
		ChatID:  "oc_test",
		ReplyTo: "om_user",
		Usage: &gateway.UsageInfo{
			InputTokens:  100,
			OutputTokens: 50,
		},
	}); err != nil {
		t.Fatalf("Send(OutUsage): %v", err)
	}
	if count != 0 {
		t.Errorf("send count = %d, want 0 (OutUsage silent drop until footer PR)", count)
	}
}


// ---------------------------------------------------------------------------
// F-44 lifecycle shift: MessageForwarded → Typing placeholder receipt
//
// These tests call ensureReceiptForTyping directly (the unit under
// test) instead of going through Send(), because Send() also calls
// AddReaction for MessageForwarded which requires a real larkClient
// (no mockable hook today — covered by integration tests).
// ---------------------------------------------------------------------------

// TestEnsureReceiptForTyping_CreatesPlaceholder verifies that
// ensureReceiptForTyping creates a Typing-placeholder card in main
// chat (top-level Create, rootID="") with the "⌨️ Typing..."
// header line that buildReceiptCard prepends when both entries and
// tasks are empty.
//
// F-44 lifecycle shift: receipts used to be lazy-created on the
// first content event (OutReply / OutTaskCreate). With this commit
// the placeholder appears the moment the agent receives the user
// message — the user sees immediate "I heard you, working on it…"
// feedback in main chat, before any stream chunk or task event lands.
// Subsequent events stream updates onto the same card via
// AppendEntry / SetTaskList.
func TestEnsureReceiptForTyping_CreatesPlaceholder(t *testing.T) {
	a := testAdapter(t)

	type captured struct {
		ChatID        string
		MsgType       string
		RootID        string
		Body          string
		ReplyInThread bool
	}
	var sends []captured
	a.sendFunc = func(_ context.Context, chatID, msgType, body, rootID string, replyInThread bool) (string, error) {
		sends = append(sends, captured{ChatID: chatID, MsgType: msgType, RootID: rootID, Body: body, ReplyInThread: replyInThread})
		return "om_typing_placeholder", nil
	}
	a.updateFunc = func(_ context.Context, _, _ string) error { return nil }

	const userMsgID = "om_user"
	rcpt, created, err := a.ensureReceiptForTyping(context.Background(), "oc_test", userMsgID)
	if err != nil {
		t.Fatalf("ensureReceiptForTyping: %v", err)
	}
	if !created {
		t.Errorf("created = false, want true (placeholder was just posted)")
	}

	// Exactly one SendCard call (the Typing placeholder).
	if len(sends) != 1 {
		t.Fatalf("send count = %d, want 1 (Typing placeholder)", len(sends))
	}
	got := sends[0]
	if got.ChatID != "oc_test" {
		t.Errorf("ChatID = %q, want %q", got.ChatID, "oc_test")
	}
	if got.MsgType != "interactive" {
		t.Errorf("MsgType = %q, want %q (Feishu Card 2.0)", got.MsgType, "interactive")
	}
	// Top-level Create (rootID="", reply_in_thread=false) so the
	// placeholder stays in main chat regardless of the parent
	// thread state.
	if got.RootID != "" {
		t.Errorf("RootID = %q, want %q (top-level Create)", got.RootID, "")
	}
	if got.ReplyInThread {
		t.Errorf("reply_in_thread = true, want false")
	}
	// Body must contain the Typing header line.
	if !strings.Contains(got.Body, "⌨️ Typing...") {
		t.Errorf("body missing Typing header line, got %q", got.Body)
	}
	// Receipt registered in receiptsByUserMsgID with the placeholder
	// cardMsgID and no entries / no tasks yet.
	if rcpt == nil {
		t.Fatalf("returned receipt is nil")
	}
	if rcpt.cardMsgID != "om_typing_placeholder" {
		t.Errorf("receipt cardMsgID = %q, want %q", rcpt.cardMsgID, "om_typing_placeholder")
	}
	if len(rcpt.entries) != 0 {
		t.Errorf("receipt entries = %d, want 0", len(rcpt.entries))
	}
	if len(rcpt.tasks) != 0 {
		t.Errorf("receipt tasks = %d, want 0", len(rcpt.tasks))
	}
}

// TestEnsureReceiptForTyping_NoOpWhenReceiptExists verifies that
// calling ensureReceiptForTyping twice on the same userMsgID is
// idempotent — the second call returns the existing receipt with
// created=false, and no second SendCard is issued.
func TestEnsureReceiptForTyping_NoOpWhenReceiptExists(t *testing.T) {
	a := testAdapter(t)

	var sends int
	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		sends++
		return "om_typing_placeholder", nil
	}
	a.updateFunc = func(_ context.Context, _, _ string) error { return nil }

	// First call: cold-start, posts the placeholder.
	rcpt1, created1, err := a.ensureReceiptForTyping(context.Background(), "oc_test", "om_user")
	if err != nil || !created1 {
		t.Fatalf("first ensureReceiptForTyping: err=%v, created=%v", err, created1)
	}
	// Second call: receipt already exists, returns existing.
	rcpt2, created2, err := a.ensureReceiptForTyping(context.Background(), "oc_test", "om_user")
	if err != nil || created2 {
		t.Fatalf("second ensureReceiptForTyping: err=%v, created=%v", err, created2)
	}
	if rcpt1 != rcpt2 {
		t.Errorf("second call returned different receipt pointer")
	}
	if sends != 1 {
		t.Errorf("send count = %d, want 1 (second call is no-op)", sends)
	}
}

// TestEnsureReceiptForReply_ReusesTypingPlaceholder verifies that
// after ensureReceiptForTyping creates the placeholder, the next
// OutReply chunk's ensureReceiptForReply reuses the same receipt
// (created=false), and the caller's AppendEntry PATCHes the
// placeholder with the new entry.
func TestEnsureReceiptForReply_ReusesTypingPlaceholder(t *testing.T) {
	a := testAdapter(t)
	var sends int
	var patches int
	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		sends++
		return "om_typing_placeholder", nil
	}
	a.updateFunc = func(_ context.Context, _, _ string) error {
		patches++
		return nil
	}

	// Step 1: Typing placeholder.
	rcpt, created, err := a.ensureReceiptForTyping(context.Background(), "oc_test", "om_user")
	if err != nil || !created {
		t.Fatalf("ensureReceiptForTyping: err=%v, created=%v", err, created)
	}

	// Step 2: OutReply chunk reuses the same receipt.
	rcpt2, created2, err := a.ensureReceiptForReply(context.Background(), "oc_test", "om_user", "first chunk")
	if err != nil || created2 {
		t.Fatalf("ensureReceiptForReply: err=%v, created=%v (want false)", err, created2)
	}
	if rcpt != rcpt2 {
		t.Errorf("ensureReceiptForReply returned a different receipt (should reuse the Typing placeholder)")
	}
	// Caller (Send) now calls AppendEntry — PATCH the same card.
	if err := rcpt2.AppendEntry(context.Background(), LogEntry{Icon: "💬", Text: "first chunk"}); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}

	if sends != 1 {
		t.Errorf("send count = %d, want 1 (only the placeholder was SendCard'd)", sends)
	}
	if patches != 1 {
		t.Errorf("patch count = %d, want 1 (AppendEntry PATCHes the placeholder)", patches)
	}
	if len(rcpt.entries) != 1 {
		t.Errorf("receipt entries = %d, want 1", len(rcpt.entries))
	}
}

// TestEnsureReceiptForTask_ReusesTypingPlaceholder verifies that
// after ensureReceiptForTyping creates the placeholder, the next
// OutTask* event's ensureReceiptForTask reuses the same receipt
// (created=false), and the caller's SetTaskList PATCHes the
// placeholder with the task checklist.
func TestEnsureReceiptForTask_ReusesTypingPlaceholder(t *testing.T) {
	a := testAdapter(t)
	var sends int
	var patches int
	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		sends++
		return "om_typing_placeholder", nil
	}
	a.updateFunc = func(_ context.Context, _, _ string) error {
		patches++
		return nil
	}

	// Step 1: Typing placeholder.
	rcpt, created, err := a.ensureReceiptForTyping(context.Background(), "oc_test", "om_user")
	if err != nil || !created {
		t.Fatalf("ensureReceiptForTyping: err=%v, created=%v", err, created)
	}

	// Step 2: OutTask event reuses the same receipt.
	list := &agent.TaskListEvent{Items: []agent.TaskItem{
		{Subject: "step 1", Status: agent.TaskPending, ActiveForm: "doing 1"},
	}}
	rcpt2, created2, err := a.ensureReceiptForTask(context.Background(), "oc_test", "om_user", list)
	if err != nil || created2 {
		t.Fatalf("ensureReceiptForTask: err=%v, created=%v (want false)", err, created2)
	}
	if rcpt != rcpt2 {
		t.Errorf("ensureReceiptForTask returned a different receipt (should reuse the Typing placeholder)")
	}
	// Caller (Send) now calls SetTaskList — PATCH the same card.
	if err := rcpt2.SetTaskList(context.Background(), list); err != nil {
		t.Fatalf("SetTaskList: %v", err)
	}

	if sends != 1 {
		t.Errorf("send count = %d, want 1 (only the placeholder was SendCard'd)", sends)
	}
	if patches != 1 {
		t.Errorf("patch count = %d, want 1 (SetTaskList PATCHes the placeholder)", patches)
	}
	if len(rcpt.tasks) != 1 {
		t.Errorf("receipt tasks = %d, want 1", len(rcpt.tasks))
	}
}
