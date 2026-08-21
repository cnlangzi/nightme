package feishu

import (
	"context"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkcallback "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// --- fake event builders ---

// fakeReceiveV1 builds a minimal P2MessageReceiveV1 with the given
// chatID on Message.ChatId. Used by SessionChatID tests to verify
// the receiveV1Source.TypedChatID path.
func fakeReceiveV1(chatID string) *larkim.P2MessageReceiveV1 {
	cid := chatID
	return &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Message: &larkim.EventMessage{
				ChatId: &cid,
			},
		},
	}
}

// fakeReactionV3 builds a minimal P2MessageReactionCreatedV1 whose
// raw envelope body has a chat_id field. The SDK typed struct
// doesn't expose chat_id, so the adapter reads from the envelope.
// Used by SessionChatID tests to verify the
// reactionV3Source.EnvelopeChatID path.
func fakeReactionV3(chatID string) *larkim.P2MessageReactionCreatedV1 {
	body := []byte(`{"event":{},"chat_id":"` + chatID + `"}`)
	return &larkim.P2MessageReactionCreatedV1{
		EventReq: &larkevent.EventReq{Body: body},
	}
}

// fakeCardAction builds a minimal CardActionTriggerEvent whose
// Context.OpenChatID is set. Used by SessionChatID tests to verify
// the cardActionSource.ContextOpenChatID path.
func fakeCardAction(chatID string) *larkcallback.CardActionTriggerEvent {
	return &larkcallback.CardActionTriggerEvent{
		Event: &larkcallback.CardActionTriggerRequest{
			Context: &larkcallback.Context{
				OpenChatID:    chatID,
				OpenMessageID: "om_test",
			},
		},
	}
}

// fakeReceiveV1Empty builds a receive_v1 event with no ChatId at
// all (TypedChatID path returns ""). Used to test the empty drop
// path — SessionChatID must return "" rather than blow up.
func fakeReceiveV1Empty() *larkim.P2MessageReceiveV1 {
	return &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Message: &larkim.EventMessage{},
		},
	}
}

// fakeReactionV3Empty builds a reaction event with no body
// (EnvelopeChatID path returns ""). Used to test the empty drop
// path.
func fakeReactionV3Empty() *larkim.P2MessageReactionCreatedV1 {
	return &larkim.P2MessageReactionCreatedV1{}
}

// fakeCardActionEmpty builds a card action event with no Context
// (ContextOpenChatID path returns ""). Used to test the empty
// drop path.
func fakeCardActionEmpty() *larkcallback.CardActionTriggerEvent {
	return &larkcallback.CardActionTriggerEvent{}
}

// --- TestSessionChatID_PureFunction ---
// Same input → same output across many invocations. Confirms
// SessionChatID has no hidden state and no side effects.
func TestSessionChatID_PureFunction(t *testing.T) {
	e := receiveV1Source{event: fakeReceiveV1("oc_abc123")}

	for i := range 100 {
		if got := SessionChatID(e); got != "oc_abc123" {
			t.Fatalf("iteration %d: got %q, want %q", i, got, "oc_abc123")
		}
	}
}

// --- TestSessionChatID_AllSourcesAgree ---
// Same chatID must produce the same string from any of the three
// sources. This is the cross-source invariant that protects
// against chatID drift (which is what made /new look like it
// wiped cwd).
func TestSessionChatID_AllSourcesAgree(t *testing.T) {
	const chatID = "oc_real_user_42"

	sources := []SessionChatIDSource{
		receiveV1Source{event: fakeReceiveV1(chatID)},
		reactionV3Source{event: fakeReactionV3(chatID)},
		cardActionSource{event: fakeCardAction(chatID)},
	}
	for i, src := range sources {
		if got := SessionChatID(src); got != chatID {
			t.Errorf("source %d: got %q, want %q", i, got, chatID)
		}
	}
}

// --- TestSessionChatID_EmptyDrops ---
// Every source returns "" when there is no chatID-shaped data in
// the event. SessionChatID must return "" (not zero, not panic).
func TestSessionChatID_EmptyDrops(t *testing.T) {
	cases := []struct {
		name string
		src  SessionChatIDSource
	}{
		{"nil receive", receiveV1Source{event: nil}},
		{"empty receive", receiveV1Source{event: fakeReceiveV1Empty()}},
		{"nil reaction", reactionV3Source{event: nil}},
		{"empty reaction", reactionV3Source{event: fakeReactionV3Empty()}},
		{"nil card action", cardActionSource{event: nil}},
		{"empty card action", cardActionSource{event: fakeCardActionEmpty()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SessionChatID(tc.src); got != "" {
				t.Errorf("got %q, want empty", got)
			}
		})
	}
}

// --- TestSessionChatID_NoFormatValidation ---
// Confirms the documented contract: SessionChatID does not check
// the "oc_" prefix. Whatever the SDK returns, SessionChatID
// passes through. We trust the SDK for modern Feishu; legacy
// chats get through unchanged and the caller (Manager.GetOrCreate)
// routes them as-is.
func TestSessionChatID_NoFormatValidation(t *testing.T) {
	cases := []struct {
		name string
		src  SessionChatIDSource
		want string
	}{
		{"oc_prefix", receiveV1Source{event: fakeReceiveV1("oc_modern_42")}, "oc_modern_42"},
		// No rejection of unusual prefixes — by design. See
		// docs/channel/feishu.md "no migration, no compatibility".
		{"arbitrary_string", receiveV1Source{event: fakeReceiveV1("anything")}, "anything"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SessionChatID(tc.src); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// --- TestSessionChatID_RouteOrder ---
// When multiple sources could theoretically produce a value, the
// first one wins. Verifies the fallback order: TypedChatID →
// EnvelopeChatID → ContextOpenChatID.
func TestSessionChatID_RouteOrder(t *testing.T) {
	if got := SessionChatID(receiveV1Source{event: fakeReceiveV1("oc_typed")}); got != "oc_typed" {
		t.Errorf("typed-only: got %q, want oc_typed", got)
	}
	if got := SessionChatID(reactionV3Source{event: fakeReactionV3("oc_envelope")}); got != "oc_envelope" {
		t.Errorf("envelope-only: got %q, want oc_envelope", got)
	}
	if got := SessionChatID(cardActionSource{event: fakeCardAction("oc_context")}); got != "oc_context" {
		t.Errorf("context-only: got %q, want oc_context", got)
	}
}

// --- TestSessionChatID_DispatchesOverWire ---
// End-to-end: the receive_v1 dispatch path through the SDK
// dispatcher must produce the same chatID as the manual
// SessionChatID call. Locks the contract that handleMessage →
// SessionChatID yields the same string the test would compute.
func TestSessionChatID_DispatchesOverWire(t *testing.T) {
	a := testAdapter(t)
	const chatID = "oc_dispatch_test"

	event := fakeReceiveV1(chatID)
	if err := a.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}
	select {
	case got := <-a.Incoming():
		if got.ChatID != chatID {
			t.Fatalf("incoming.ChatID = %q, want %q", got.ChatID, chatID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for incoming message")
	}
}

// --- TestSessionChatID_EmptyHandleDrops ---
// When SessionChatID returns "", the handler must drop the event
// — no message is published on the incoming channel.
func TestSessionChatID_EmptyHandleDrops(t *testing.T) {
	a := testAdapter(t)
	event := fakeReceiveV1Empty()
	if err := a.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}
	select {
	case got := <-a.Incoming():
		t.Fatalf("expected drop, got incoming message: %+v", got)
	case <-time.After(500 * time.Millisecond):
		// good — handler dropped
	}
}

// --- TestExtractReactionChatID_NilEventReq ---
// Lock the nil-pointer fix at adaptation.go: extractReactionChatID
// must return "" (not panic) when the SDK event has a nil
// EventReq. This is what happens when the Feishu SDK delivers an
// event with the body already parsed away (e.g. when the WS
// dispatcher short-circuits). Without the EventReq nil check, the
// function used to dereference event.Body and panic.
func TestExtractReactionChatID_NilEventReq(t *testing.T) {
	// (1) Direct nil-event: extractReactionChatID returns "".
	if got := extractReactionChatID(nil); got != "" {
		t.Errorf("nil event: got %q, want empty", got)
	}

	// (2) Event present but EventReq nil: extractReactionChatID
	// returns "" without panic. This is the case that used to
	// crash via the implicit Body access on a nil embedded
	// pointer.
	ev := &larkim.P2MessageReactionCreatedV1{}
	if got := extractReactionChatID(ev); got != "" {
		t.Errorf("non-nil event with nil EventReq: got %q, want empty", got)
	}

	// (3) EventReq present but Body empty: still returns "".
	ev2 := &larkim.P2MessageReactionCreatedV1{
		EventReq: &larkevent.EventReq{},
	}
	if got := extractReactionChatID(ev2); got != "" {
		t.Errorf("nil Body: got %q, want empty", got)
	}

	// (4) EventReq present with malformed body: returns "".
	ev3 := &larkim.P2MessageReactionCreatedV1{
		EventReq: &larkevent.EventReq{Body: []byte("not json")},
	}
	if got := extractReactionChatID(ev3); got != "" {
		t.Errorf("malformed body: got %q, want empty", got)
	}
}

// --- TestSessionChatID_Concurrent ---
// 50 goroutines call SessionChatID concurrently with the same
// source. All must observe the same string. Also serves as a
// -race smoke test: source adapters read only the input event
// payload, no shared writable state.
func TestSessionChatID_Concurrent(t *testing.T) {
	const chatID = "oc_concurrent_test"
	const goroutines = 50
	e := receiveV1Source{event: fakeReceiveV1(chatID)}

	var wg sync.WaitGroup
	var mu sync.Mutex
	failures := 0
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := SessionChatID(e)
			mu.Lock()
			if got != chatID {
				failures++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if failures > 0 {
		t.Errorf("%d goroutines saw wrong chatID", failures)
	}
}

// --- TestHandleCardAction_DispatchesChatID ---
// E2E: when a card action click lands via handleCardAction with
// the act: prefix → handleActCardAction, the synthetic inbound
// message must carry the chatID from Context.OpenChatID. This is
// the path that previously drifted if anyone reverted the
// SessionChatID refactor back to direct OpenChatID access.
func TestHandleCardAction_DispatchesChatID(t *testing.T) {
	a := testAdapter(t)
	const chatID = "oc_card_action_test"
	const botMsgID = "om_card_action_test"

	event := &larkcallback.CardActionTriggerEvent{
		Event: &larkcallback.CardActionTriggerRequest{
			Operator: &larkcallback.Operator{OpenID: "ou_user"},
			Action: &larkcallback.CallBackAction{
				Value: map[string]any{
					"action":     "act:/gtw/branch-newv2",
					"request_id": "req-card-test",
				},
			},
			Context: &larkcallback.Context{
				OpenChatID:    chatID,
				OpenMessageID: botMsgID,
			},
		},
	}
	if _, err := a.handleCardAction(context.Background(), event); err != nil {
		t.Fatalf("handleCardAction: %v", err)
	}
	select {
	case got := <-a.Incoming():
		if got.ChatID != chatID {
			t.Errorf("incoming.ChatID = %q, want %q", got.ChatID, chatID)
		}
		if got.Reaction == nil {
			t.Fatal("incoming.Reaction is nil")
		}
		if got.Reaction.ChatID != chatID {
			t.Errorf("incoming.Reaction.ChatID = %q, want %q", got.Reaction.ChatID, chatID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for incoming message")
	}
}

// --- TestHandleMessage_DispatchesChatID ---
// Mirror version of TestSessionChatID_DispatchesOverWire — kept
// here so all dispatch-side tests live next to the SessionChatID
// builder. Verifies the receive_v1 wire path lands `chatID` on
// the inbound channel.
func TestHandleMessage_DispatchesChatID(t *testing.T) {
	a := testAdapter(t)
	const chatID = "oc_recv_dispatch_test"

	event := fakeReceiveV1(chatID)
	if err := a.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}
	select {
	case got := <-a.Incoming():
		if got.ChatID != chatID {
			t.Errorf("incoming.ChatID = %q, want %q", got.ChatID, chatID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for incoming message")
	}
}

// --- TestHandleCardAction_SkipsLogOnEmptyChatID ---
// When the card action event has no Context (and therefore no
// chatID), the unknown-action fallback log.Printf must NOT
// produce "chat=" noise. We capture stdout and assert it doesn't
// contain the F-46 prototype log prefix.
func TestHandleCardAction_SkipsLogOnEmptyChatID(t *testing.T) {
	a := testAdapter(t)

	// Capture stdout to inspect the log.Printf.
	origStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = origStdout
		_ = w.Close()
		_, _ = io.ReadAll(r)
	})
	log.SetOutput(w)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	// Event with no Event at all → ContextOpenChatID returns "".
	event := &larkcallback.CardActionTriggerEvent{}
	_, _ = a.handleCardAction(context.Background(), event)

	_ = w.Close()
	out, _ := io.ReadAll(r)
	if strings.Contains(string(out), "feishu: card action received chat=") {
		t.Errorf("unexpected log output on empty chatID: %s", string(out))
	}
}

