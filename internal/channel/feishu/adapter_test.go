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
// StateCompleted) silently dropped inside receipt.Append. This test
// reproduces that path: turn 1 completes, turn 2 starts, and we
// assert turn 2 cold-creates its own receipt.
func TestSend_RoutesByUserMsgID_NotChatID(t *testing.T) {
	a := testAdapter(t)
	chatID := "oc_chat_v13_routing"

	// Mock: SendCard / SendMessageText return synthetic message IDs;
	// PatchMessage is a no-op recorder.
	var cards int
	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
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
		Kind: gateway.OutReply, ChatID: chatID, ReplyTo: "om_msg_1",
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
		Kind: gateway.OutReply, ChatID: chatID, ReplyTo: "om_msg_2",
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
		t.Errorf("expected >=2 SendCard calls (one per turn's lazy receipt), got %d", cards)
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
	// F-42: turn 2's text is shipped inside the lazy ensure's
	// SendCard call (not via a subsequent PATCH). Verify the
	// receipt registered with cardMsgID set and the text already
	// in its entries list, so the user sees "turn 2 hello" in
	// the chat even without a follow-up PATCH.
	if rcpt2.cardMsgID == "" {
		t.Errorf("turn 2 receipt has empty cardMsgID; SendCard did not seed it")
	}
	rcpt2.mu.Lock()
	turn2TextInEntries := len(rcpt2.entries) >= 1 && rcpt2.entries[0].Text == "turn 2 hello"
	rcpt2.mu.Unlock()
	if !turn2TextInEntries {
		t.Errorf("turn 2 receipt entries should contain \"turn 2 hello\"; lazy create did not seed the entry")
	}
}

type patchCall struct {
	MessageID string
}

// TestSend_OutThinking_PostsMarkdownCard — F-34 + F-think.
// OutThinking is routed to a Feishu thread reply (rootID =
// msg.ReplyTo) as an interactive card with a single (or
// multi-div) lark_md body. Plain text thinking would lose all
// markdown formatting in the chat.
//
// The test captures all outgoing sends and asserts on the
// interactive card reply; any receipt cold-start / PATCH side
// effects are accepted as observable side-effects, not tested
// here (covered by TestReceipt_*).
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
	t.Run("ReceiptLazyCreate", func(t *testing.T) {
		// F-42: receiptFor is now a pure cache lookup and never
		// ships a card. The first SendCard happens in
		// ensureReceiptForReply when an actual OutReply lands.
		// This subtest exercises that path: the SendCard that
		// produces the receipt card must post to main chat
		// (reply_in_thread=false), exactly like the old cold-start
		// path did.
		a := testAdapter(t)
		var threadOnly int
		var chatVisible int
		a.sendFunc = func(_ context.Context, _, _, _, rootID string, replyInThread bool) (string, error) {
			if rootID == "om_user_cold" {
				if replyInThread {
					threadOnly++
				} else {
					chatVisible++
				}
			}
			return "om_card_cold", nil
		}

		r, created, err := a.ensureReceiptForReply(t.Context(), "oc_cold", "om_user_cold", "hello world")
		if err != nil {
			t.Fatalf("ensureReceiptForReply: %v", err)
		}
		if r == nil {
			t.Fatalf("ensureReceiptForReply returned nil receipt")
		}
		if !created {
			t.Errorf("ensureReceiptForReply created=false on first call; want true")
		}
		if chatVisible == 0 {
			t.Errorf("lazy-create card reply_in_thread flag was true (threaded), want false — receipt card must be visible in main chat")
		}
		if threadOnly != 0 {
			t.Errorf("lazy-create card was threaded %d times, want 0", threadOnly)
		}
		// Sanity: the entry is already installed; the receipt is
		// StateExecuting (the empty-waiting header would have
		// been the bug we're fixing).
		if r.State() != StateExecuting {
			t.Errorf("lazy-create receipt state = %v, want StateExecuting", r.State())
		}
		r.mu.Lock()
		hasText := len(r.entries) == 1 && r.entries[0].Text == "hello world"
		r.mu.Unlock()
		if !hasText {
			t.Errorf("lazy-create receipt entries should contain \"hello world\" exactly once")
		}
	})

	t.Run("OutCard", func(t *testing.T) {
		a := testAdapter(t)
		var threadOnly int
		var chatVisible int
		a.sendFunc = func(_ context.Context, _, msgTypeRaw, _, rootID string, replyInThread bool) (string, error) {
			if msgTypeRaw == interactiveMessageType && rootID == "om_user_perm" {
				if replyInThread {
					threadOnly++
				} else {
					chatVisible++
				}
			}
			return "om_card_perm", nil
		}

		err := a.Send(t.Context(), gateway.OutboundMessage{
			Kind:    gateway.OutCard,
			ChatID:  "oc_test",
			ReplyTo: "om_user_perm",
			Card: &gateway.Card{
				RequestID: "req_perm",
				Title:     "Permission?",
				Options:   []string{"yes", "no"},
			},
		})
		if err != nil {
			t.Fatalf("Send(OutCard): %v", err)
		}
		if chatVisible == 0 {
			t.Errorf("OutCard reply_in_thread flag was true (threaded), want false — permission card must be visible in main chat")
		}
		if threadOnly != 0 {
			t.Errorf("OutCard was threaded %d times, want 0", threadOnly)
		}
	})

	t.Run("OutCommandReply", func(t *testing.T) {
		a := testAdapter(t)
		var threadOnly int
		var chatVisible int
		a.sendFunc = func(_ context.Context, _, msgTypeRaw, _, rootID string, replyInThread bool) (string, error) {
			if msgTypeRaw == larkim.MsgTypeText && rootID == "om_user_cmd" {
				if replyInThread {
					threadOnly++
				} else {
					chatVisible++
				}
			}
			return "om_text_cmd", nil
		}

		err := a.Send(t.Context(), gateway.OutboundMessage{
			Kind:    gateway.OutCommandReply,
			ChatID:  "oc_test",
			ReplyTo: "om_user_cmd",
			Text:    "agent available",
		})
		if err != nil {
			t.Fatalf("Send(OutCommandReply): %v", err)
		}
		if chatVisible == 0 {
			t.Errorf("OutCommandReply reply_in_thread flag was true (threaded), want false — slash command result must be visible in main chat")
		}
		if threadOnly != 0 {
			t.Errorf("OutCommandReply was threaded %d times, want 0", threadOnly)
		}
	})

	// OutCompaction moved here from
	// TestSend_ThreadOnlyEvents_PassReplyInThreadTrue on
	// 2026-08-04 (ops: a brief "✶ Compacting…" line in main chat
	// is informative, not noise). Same wire shape as
	// OutCommandReply: text body, reply API, reply_in_thread
	// omitted.
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
func TestSend_OutText_FoldsIntoReceipt(t *testing.T) {
	a := testAdapter(t)
	userMsgID := "om_user_out"

	var cards int
	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		cards++
		return fmt.Sprintf("om_card_%d", cards), nil
	}
	a.updateFunc = func(_ context.Context, _, _ string) error { return nil }

	// Warm up the receipt.
	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind:    gateway.OutReply,
		ChatID:  "oc_test",
		ReplyTo: userMsgID,
		Text:    "warmup",
	}); err != nil {
		t.Fatalf("Send(OutReply warmup): %v", err)
	}

	for _, kind := range []gateway.OutboundKind{
		gateway.OutResult,
		gateway.OutUsage,
		gateway.OutInit,
	} {
		msg := gateway.OutboundMessage{
			Kind:    kind,
			ChatID:  "oc_test",
			ReplyTo: userMsgID,
			Text:    "x",
		}
		switch kind {
		case gateway.OutResult:
			msg.Result = &agent.ResultEvent{
				Text:       "x",
				DurationMs: 1234,
				IsError:    false,
				Subtype:    "success",
			}
		case gateway.OutUsage:
			msg.Usage = &gateway.UsageInfo{
				InputTokens:  10,
				OutputTokens: 5,
			}
		case gateway.OutInit:
			msg.Init = &agent.InitEvent{
				SessionID: "s_1",
				Model:     "claude-sonnet-4-5",
				AgentName: "claude",
				Workspace: "/tmp",
				Branch:    "main",
			}
		}
		if err := a.Send(t.Context(), msg); err != nil {
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
		t.Fatalf("receipt has no entries; OutReply/OutResult/OutInit/OutUsage should fold in")
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
	a.sendFunc = func(_ context.Context, chatID, msgType, _, rootID string, _ bool) (string, error) {
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

// ===========================================================================
// F-39: OutResult → independent reply tests
// ===========================================================================

// TestSend_OutResult_GoesToNewReply_NotReceipt — F-39 reverse-section proof.
// OutResult must NOT fold into the rolling-log receipt card. It must be
// delivered as a separate reply anchored at userMsgID via sendResultAsReply.
func TestSend_OutResult_GoesToNewReply_NotReceipt(t *testing.T) {
	a := testAdapter(t)
	userMsgID := "om_user_f39"

	var sends int
	var gotMsgType, gotRootID string
	a.sendFunc = func(_ context.Context, _, msgType, _, rootID string, _ bool) (string, error) {
		sends++
		gotMsgType = msgType
		gotRootID = rootID
		return "om_result_card", nil
	}
	a.updateFunc = func(_ context.Context, _, _ string) error { return nil }

	// Pre-create the receipt via an OutReply warmup so receiptFor lookup
	// has a real receipt to interact with.
	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind:    gateway.OutReply,
		ChatID:  "oc_test",
		ReplyTo: userMsgID,
		Text:    "streaming…",
	}); err != nil {
		t.Fatalf("warmup: %v", err)
	}

	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind:    gateway.OutResult,
		ChatID:  "oc_test",
		ReplyTo: userMsgID,
		Text:    "完成",
		Result: &agent.ResultEvent{
			Text:       "完成",
			DurationMs: 1234,
			IsError:    false,
		},
	}); err != nil {
		t.Fatalf("Send(OutResult): %v", err)
	}

	// Result should arrive as its own msg_type.
	if sends != 2 {
		t.Errorf("expected exactly 2 sends (warmup + result), got %d", sends)
	}
	if gotMsgType != "interactive" && gotMsgType != "post" && gotMsgType != "text" {
		t.Errorf("result msgType should be one of interactive/post/text, got %q", gotMsgType)
	}
	if gotRootID != userMsgID {
		t.Errorf("result should anchor at userMsgID %q, got %q", userMsgID, gotRootID)
	}
	// Result must NOT have been folded into receipt.entries.
	a.mu.RLock()
	rcpt := a.receiptsByUserMsgID[userMsgID]
	a.mu.RUnlock()
	if rcpt == nil {
		t.Fatalf("receipt missing after warmup")
	}
	rcpt.mu.Lock()
	defer rcpt.mu.Unlock()
	hasResult := false
	for _, e := range rcpt.entries {
		if e.Kind == "result" {
			hasResult = true
		}
	}
	if hasResult {
		t.Errorf("receipt.entries should not contain Kind=\"result\" entries (F-39 reverse)")
	}
}

// --- F-40: OutReply fold / overflow routing ---

// TestSend_OutReply_FoldsIntoReceipt_NoTruncate — F-40 default
// fold path. Short replies must reach the receipt LogEntry
// verbatim (no 600-byte truncation) and the receipt must NOT
// trigger sendFunc for the fold path (no stand-alone reply).
func TestSend_OutReply_FoldsIntoReceipt_NoTruncate(t *testing.T) {
	a := testAdapter(t)
	userMsgID := "om_user_fold"

	var sends int
	a.sendFunc = func(context.Context, string, string, string, string, bool) (string, error) {
		sends++
		return "om_xxx", nil
	}
	a.updateFunc = func(context.Context, string, string) error { return nil }

	text1500 := strings.Repeat("y", 1500) // well under 8000 runes but well over old 600B cap
	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind:    gateway.OutReply,
		ChatID:  "oc_test",
		ReplyTo: userMsgID,
		Text:    text1500,
	}); err != nil {
		t.Fatalf("Send(OutReply): %v", err)
	}

	// The fold path itself doesn't call sendFunc after the
	// initial cold-start (which DOES go through sendFunc via
	// SendCard). Total sends = 1 (cold-start only). The fold
	// event itself flows through receipt.Append → updateFunc.
	if sends != 1 {
		t.Errorf("fold path: expected exactly 1 sendFunc call (cold-start only), got %d", sends)
	}

	// Verify the full 1500-char text reached the LogEntry.
	a.mu.RLock()
	rcpt := a.receiptsByUserMsgID[userMsgID]
	a.mu.RUnlock()
	if rcpt == nil {
		t.Fatal("receipt missing after OutReply fold")
	}
	rcpt.mu.Lock()
	defer rcpt.mu.Unlock()
	if len(rcpt.entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(rcpt.entries))
	}
	if rcpt.entries[0].Text != text1500 {
		t.Errorf("reply text truncated: got %d chars, want %d",
			len(rcpt.entries[0].Text), len(text1500))
	}
	if rcpt.entries[0].Kind != "reply" || rcpt.entries[0].Icon != "💬" {
		t.Errorf("entry shape wrong: got (%q, %q), want (💬, reply)",
			rcpt.entries[0].Icon, rcpt.entries[0].Kind)
	}
}

// TestSend_OutReply_OverflowLength_AsReply — F-40 §1.2 length
// rule. text rune count > perEntryMaxRunes (8000) must divert
// to a stand-alone ReplyInThreadAndChat instead of folding into
// the receipt. The receipt must NOT receive a new LogEntry.
func TestSend_OutReply_OverflowLength_AsReply(t *testing.T) {
	a := testAdapter(t)
	userMsgID := "om_user_overflow_len"

	var sends int
	var gotMsgType, gotRootID string
	a.sendFunc = func(_ context.Context, _, msgType, _, rootID string, replyInThread bool) (string, error) {
		sends++
		gotMsgType = msgType
		gotRootID = rootID
		return "om_overflow", nil
	}
	a.updateFunc = func(context.Context, string, string) error { return nil }

	// Prime receipt with one short entry.
	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind: gateway.OutReply, ChatID: "oc_test", ReplyTo: userMsgID, Text: "first chunk",
	}); err != nil {
		t.Fatalf("warmup: %v", err)
	}

	// Now overflow: 9000 runes > perEntryMaxRunes (8000).
	bigText := strings.Repeat("z", 9000)
	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind: gateway.OutReply, ChatID: "oc_test", ReplyTo: userMsgID, Text: bigText,
	}); err != nil {
		t.Fatalf("Send(overflow OutReply): %v", err)
	}

	// Overflow path: warmup triggers cold-start SendCard (1
	// sendFunc), overflow triggers ReplyInThreadAndChat (1 more
	// sendFunc). Total = 2.
	if sends != 2 {
		t.Errorf("expected exactly 2 sendFunc calls (cold-start + overflow), got %d", sends)
	}
	// Anchored at userMsgID via ReplyInThreadAndChat
	// (replyInThread=false ⇒ rootID is set). The closure
	// captures the LAST send's rootID; since overflow is the
	// last call, this is the overflow reply's anchor.
	if gotRootID != userMsgID {
		t.Errorf("overflow reply must anchor at userMsgID %q, got %q", userMsgID, gotRootID)
	}
	// 3-segment dispatch returns interactive / post / text —
	// the long text contains no markdown so it lands in text
	// (no markdown indicators). Sanity-check the msgType family.
	if gotMsgType != "interactive" && gotMsgType != "post" && gotMsgType != "text" {
		t.Errorf("overflow reply msgType should be interactive/post/text, got %q", gotMsgType)
	}

	// Receipt should still hold only the warmup entry; the
	// overflowed reply must NOT have appended a 9000-char entry.
	a.mu.RLock()
	rcpt := a.receiptsByUserMsgID[userMsgID]
	a.mu.RUnlock()
	if rcpt == nil {
		t.Fatal("receipt missing after warmup")
	}
	rcpt.mu.Lock()
	defer rcpt.mu.Unlock()
	if len(rcpt.entries) != 1 {
		t.Errorf("receipt should hold only the warmup entry; got %d entries (overflow must not fold)", len(rcpt.entries))
	}
}

// TestSend_OutReply_OverflowQuantity_AsReply — F-40 §1.2 quantity
// rule. Receipt already full (EntryCount >= replyMaxEntries = 45)
// must divert a new reply to stand-alone ReplyInThreadAndChat
// instead of FIFO-evicting the oldest entries.
func TestSend_OutReply_OverflowQuantity_AsReply(t *testing.T) {
	a := testAdapter(t)
	userMsgID := "om_user_overflow_qty"

	var sends int
	a.sendFunc = func(context.Context, string, string, string, string, bool) (string, error) {
		sends++
		return "om_q", nil
	}
	a.updateFunc = func(context.Context, string, string) error { return nil }

	// Prime receipt with one warmup send (drives cold-start
	// through sendFunc), then inflate its entries directly to
	// replyMaxEntries. Mutating the slice avoids driving 45
	// sequential OutReply sends through the Append → renderLocked
	// pipeline (which would do 45 PATCH storm calls) — we only
	// care about the routing decision, not the appender path.
	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind: gateway.OutReply, ChatID: "oc_test", ReplyTo: userMsgID, Text: "warmup",
	}); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	a.mu.RLock()
	r := a.receiptsByUserMsgID[userMsgID]
	a.mu.RUnlock()
	if r == nil {
		t.Fatal("receipt missing after warmup")
	}
	r.mu.Lock()
	// First entry is the warmup; pad to replyMaxEntries total.
	for len(r.entries) < replyMaxEntries {
		r.entries = append(r.entries, LogEntry{Icon: "💬", Text: "x", Kind: "reply"})
	}
	r.mu.Unlock()

	// A short reply (well below perEntryMaxRunes) but receipt is
	// full → quantity overflow → divert to stand-alone reply.
	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind: gateway.OutReply, ChatID: "oc_test", ReplyTo: userMsgID, Text: "another",
	}); err != nil {
		t.Fatalf("Send(quantity overflow): %v", err)
	}

	if sends != 2 {
		t.Errorf("expected exactly 2 sendFunc calls (warmup cold-start + quantity overflow reply), got %d", sends)
	}

	// The receipt must NOT have FIFO-evicted; all 45 original
	// entries must still be present.
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) != replyMaxEntries {
		t.Errorf("receipt entries changed: got %d, want %d (no FIFO evict on overflow route)",
			len(r.entries), replyMaxEntries)
	}
}

// TestSend_OutReply_AfterCompletion_AsReply — F-40 §1.5 late
// reply. Once the receipt has reached StateCompleted (terminal),
// further OutReply events must be delivered as stand-alone
// replies rather than silently dropped (which is what the old
// Append path did when state == StateCompleted).
func TestSend_OutReply_AfterCompletion_AsReply(t *testing.T) {
	a := testAdapter(t)
	userMsgID := "om_user_late"

	var sends int
	var gotRootID string
	a.sendFunc = func(_ context.Context, _, _, _, rootID string, _ bool) (string, error) {
		sends++
		gotRootID = rootID
		return "om_late", nil
	}
	a.updateFunc = func(context.Context, string, string) error { return nil }

	// Prime receipt then mark it completed via the lifecycle.
	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind: gateway.OutReply, ChatID: "oc_test", ReplyTo: userMsgID, Text: "warmup",
	}); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	a.mu.RLock()
	rcpt := a.receiptsByUserMsgID[userMsgID]
	a.mu.RUnlock()
	if err := rcpt.SetCompleted(t.Context()); err != nil {
		t.Fatalf("SetCompleted: %v", err)
	}

	sendsBeforeLate := sends
	// Late OutReply — must NOT be silently dropped.
	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind: gateway.OutReply, ChatID: "oc_test", ReplyTo: userMsgID, Text: "late reply",
	}); err != nil {
		t.Fatalf("Send(late OutReply): %v", err)
	}

	if sends != sendsBeforeLate+1 {
		t.Errorf("late OutReply must trigger sendFunc; got %d → %d sends",
			sendsBeforeLate, sends)
	}
	if gotRootID != userMsgID {
		t.Errorf("late reply must anchor at userMsgID %q, got %q", userMsgID, gotRootID)
	}
}

// TestSend_OutReply_NoReceiptFallback — when receiptFor fails
// (SendCard cold-start errored), OutReply must degrade to
// sendRawOutText (plain top-level bubble) so the user still sees
// the reply. Mirrors the pre-F-40 fail-safe path.
func TestSend_OutReply_NoReceiptFallback(t *testing.T) {
	a := testAdapter(t)

	// Force receiptFor to fail: pre-populate the map with a
	// poisoned receipt whose SendCard returns an error. Adapter
	// will return this receipt from receiptFor but our new
	// logic treats receipt == nil OR SendCard failure as "fallback".
	// Simpler approach: send with empty ReplyTo so receiptFor
	// itself short-circuits to nil (it returns nil when userMsgID
	// is empty), exercising the fallback path.
	var sends int
	var gotRootID string
	a.sendFunc = func(_ context.Context, _, _, _, rootID string, _ bool) (string, error) {
		sends++
		gotRootID = rootID
		return "om_fb", nil
	}
	a.updateFunc = func(context.Context, string, string) error { return nil }

	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind: gateway.OutReply, ChatID: "oc_test", ReplyTo: "", Text: "orphan",
	}); err != nil {
		t.Fatalf("Send(no-receipt OutReply): %v", err)
	}

	// Orphan path uses sendRawOutText which routes through
	// sendContent with empty rootID (top-level).
	if sends != 1 {
		t.Errorf("expected exactly 1 sendFunc call (fallback), got %d", sends)
	}
	if gotRootID != "" {
		t.Errorf("orphan fallback must use top-level send (rootID=\"\"), got %q", gotRootID)
	}
}

// TestSend_OutReply_NoIconPrefix_OnOverflow — F-40 §1.3 visual:
// the stand-alone reply payload must NOT carry the 💬 icon
// prefix. The icon is reserved for receipt entries; the stand-
// alone path is a continuation of the reply stream and would
// visually clash with the receipt siblings if it carried the
// same prefix.
func TestSend_OutReply_NoIconPrefix_OnOverflow(t *testing.T) {
	a := testAdapter(t)
	userMsgID := "om_user_noicon"

	var capturedBody string
	a.sendFunc = func(_ context.Context, _, _, body, rootID string, _ bool) (string, error) {
		capturedBody = body
		return "om_n", nil
	}
	a.updateFunc = func(context.Context, string, string) error { return nil }

	// Prime receipt, then trigger overflow.
	_ = a.Send(t.Context(), gateway.OutboundMessage{
		Kind: gateway.OutReply, ChatID: "oc_test", ReplyTo: userMsgID, Text: "warmup",
	})
	bigText := strings.Repeat("w", 9000)
	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind: gateway.OutReply, ChatID: "oc_test", ReplyTo: userMsgID, Text: bigText,
	}); err != nil {
		t.Fatalf("Send(overflow): %v", err)
	}

	if strings.Contains(capturedBody, "💬") {
		t.Errorf("overflow reply body must not carry 💬 icon prefix; got: %s",
			truncateForTest(capturedBody, 200))
	}
}

// helper functions removed: newReceiptForTest / buildColdStartCardForTest
// were left over from an earlier draft that inflated the receipt
// via SendCard. The current TestSend_OutReply_OverflowQuantity
// primes via a normal OutReply warmup then mutates r.entries
// directly to avoid driving the SendCard → renderLocked pipeline.

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
// receipt.SetCompleted. The receipt must remain in StateExecuting
// after OutResult so subsequent OutUsage / OutInit / TaskList can
// still update the footer (token counts, agent name, task list).
// EventDone is the terminal signal that flips state to StateCompleted.
func TestSend_OutResult_LeavesReceiptAlive(t *testing.T) {
	a := testAdapter(t)
	userMsgID := "om_complete"

	a.updateFunc = func(_ context.Context, _, _ string) error { return nil }
	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		return "ok", nil
	}

	// Trigger a Text to ensure receipt exists + is Executing.
	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind: gateway.OutReply, ChatID: "oc_test", ReplyTo: userMsgID, Text: "x",
	}); err != nil {
		t.Fatalf("warmup: %v", err)
	}

	// Send OutResult. The answer card goes out as an independent
	// reply. The receipt stays in StateExecuting so EventUsage
	// / EventInit / TaskList can still update the footer after.
	if err := a.Send(t.Context(), gateway.OutboundMessage{
		Kind:    gateway.OutResult,
		ChatID:  "oc_test",
		ReplyTo: userMsgID,
		Text:    "done",
		Result:  &agent.ResultEvent{Text: "done"},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	// Verify receipt is still alive (not StateCompleted).
	a.mu.RLock()
	rcpt := a.receiptsByUserMsgID[userMsgID]
	a.mu.RUnlock()
	if rcpt == nil {
		t.Fatalf("receipt missing")
	}
	if rcpt.State() != StateExecuting {
		t.Errorf("receipt state should remain StateExecuting after OutResult, got %v", rcpt.State())
	}

	// Now simulate EventDone → receipt flips to StateCompleted
	// and clears the rolling-log entries (so the final card
	// collapses to header + footer + task list).
	if err := rcpt.Append(t.Context(), agent.AgentEvent{
		Kind: agent.EventDone,
		Done: &agent.DoneEvent{ExitCode: 0},
	}); err != nil {
		t.Fatalf("EventDone append: %v", err)
	}
	if rcpt.State() != StateCompleted {
		t.Errorf("receipt state should be StateCompleted after EventDone, got %v", rcpt.State())
	}
	rcpt.mu.Lock()
	defer rcpt.mu.Unlock()
	if len(rcpt.entries) != 0 {
		t.Errorf("rolling-log entries should be cleared on SetCompleted, got %d", len(rcpt.entries))
	}
}

// TestSend_OutResult_OrphanTopLevel — when userMsgID is empty,
// sendResultAsReply falls back to sendRawOutText (top-level plain text).
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
func TestEnsureReceiptForReply_ThinkingPrefix_ReturnsError(t *testing.T) {
	a := testAdapter(t)

	// Thinking-prefix text is the only currently-reachable
	// eventToEntry filter (empty text is rejected upstream in
	// Send). The thinking prefix is the gateway's [思考] marker.
	const thinkingText = "[思考] internal note from a misrouted event"

	r, created, err := a.ensureReceiptForReply(t.Context(), "oc_chat", "om_user", thinkingText)
	if err == nil {
		t.Fatalf("ensureReceiptForReply(thinking-prefix) returned nil err; want error (pre-fix returned nil and the caller nil-deref'd)")
	}
	if r != nil {
		t.Errorf("ensureReceiptForReply returned non-nil receipt on error path: %v", r)
	}
	if created {
		t.Errorf("created=true on error path; should be false")
	}

	// And the OutReply Send dispatcher must degrade gracefully
	// via sendRawOutText — no panic, no nil deref.
	a.sendFunc = func(_ context.Context, _, msgType, content, rootID string, _ bool) (string, error) {
		if msgType != larkim.MsgTypeText {
			t.Errorf("thinking-prefix fallback should use MsgTypeText, got %q", msgType)
		}
		if !strings.Contains(content, thinkingText) {
			t.Errorf("fallback body should contain the original text %q, got %q", thinkingText, content)
		}
		return "om_text_fallback", nil
	}
	if sendErr := a.Send(t.Context(), gateway.OutboundMessage{
		Kind:    gateway.OutReply,
		ChatID:  "oc_chat",
		ReplyTo: "om_user",
		Text:    thinkingText,
	}); sendErr != nil {
		t.Fatalf("Send with thinking-prefix text returned err=%v; the fallback path must succeed silently", sendErr)
	}
}

// TestEnsureReceiptForReply_Concurrent_OnlyOneSendCard guards F-42
// review finding #4. Pre-fix the helper used a pre-check + SendCard
// + post-SendCard recheck pattern: two goroutines for the same
// userMsgID could both pass the pre-check, both call SendCard (each
// producing a distinct card in chat), and the loser would discover
// the winner under the post-SendCard lock and discard its receipt —
// leaving its orphan card in chat forever (no future PATCH reaches
// it because PATCHes target the winner's cardMsgID).
//
// The fix uses register-before-SendCard: only the goroutine that
// wins the registration proceeds to SendCard. Concurrent callers
// see the registered placeholder and return early (created=false);
// their renderLocked calls short-circuit on `initializing` so no
// second SendCard ever fires.
func TestEnsureReceiptForReply_Concurrent_OnlyOneSendCard(t *testing.T) {
	a := testAdapter(t)

	// Inject a slow SendCard so both goroutines reach the
	// registration race window before either finishes.
	var (
		sendCardMu     sync.Mutex
		sendCardCalls  int
		releaseSignal  = make(chan struct{})
	)
	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		sendCardMu.Lock()
		sendCardCalls++
		n := sendCardCalls
		sendCardMu.Unlock()
		// Block until the test releases us, so the second
		// goroutine's call lands while we're still mid-SendCard.
		<-releaseSignal
		return fmt.Sprintf("om_card_%d", n), nil
	}

	const userMsgID = "om_user_race"
	const chatID = "oc_race"

	var (
		wg          sync.WaitGroup
		errs        [2]error
		receipts    [2]*MessageReceipt
		createds    [2]bool
	)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			receipts[i], createds[i], errs[i] = a.ensureReceiptForReply(
				t.Context(), chatID, userMsgID,
				fmt.Sprintf("text from goroutine %d", i),
			)
		}(i)
	}

	// Wait briefly so both goroutines reach their respective
	// SendCard calls (both blocked on releaseSignal).
	time.Sleep(50 * time.Millisecond)

	// Release both SendCard calls. They each complete and return
	// a distinct message id.
	close(releaseSignal)
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("goroutine %d: err = %v", i, e)
		}
	}

	// Exactly one goroutine must have won registration (created=true).
	winners := 0
	for _, c := range createds {
		if c {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("created count = %d, want 1 (only one goroutine should win registration)", winners)
	}

	// Exactly one SendCard should have been called (the orphan
	// race pre-fix would produce 2).
	sendCardMu.Lock()
	calls := sendCardCalls
	sendCardMu.Unlock()
	if calls != 1 {
		t.Errorf("SendCard calls = %d, want 1 (F-42 register-before-SendCard prevents the loser from posting an orphan card)", calls)
	}

	// Both goroutines must end up with the SAME receipt pointer
	// (the winner's placeholder).
	if receipts[0] != receipts[1] {
		t.Errorf("receipts[0]=%p != receipts[1]=%p; both should be the same registered placeholder", receipts[0], receipts[1])
	}

	// The receipt must be registered under the canonical map key.
	a.mu.Lock()
	registered := a.receiptsByUserMsgID[userMsgID]
	a.mu.Unlock()
	if registered != receipts[0] {
		t.Errorf("map[userMsgID]=%p != winner receipt=%p", registered, receipts[0])
	}
}

// TestEnsureReceiptForTask_Concurrent_OnlyOneSendCard guards F-42
// review finding #5. Same race as TestEnsureReceiptForReply but
// for the task-list path — two OutTask* events for the same
// userMsgID racing must produce exactly ONE card in chat, with
// the winner's snapshot merged into the canonical receipt.
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
