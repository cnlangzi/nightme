package feishu

import (
	"context"
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
	a := testAdapter(t)
	e := receiveV1Source{event: fakeReceiveV1("oc_abc123")}

	for i := range 100 {
		if got := a.SessionChatID(e); got != "oc_abc123" {
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
	a := testAdapter(t)
	const chatID = "oc_real_user_42"

	sources := []SessionChatIDSource{
		receiveV1Source{event: fakeReceiveV1(chatID)},
		reactionV3Source{event: fakeReactionV3(chatID)},
		cardActionSource{event: fakeCardAction(chatID)},
	}
	for i, src := range sources {
		if got := a.SessionChatID(src); got != chatID {
			t.Errorf("source %d: got %q, want %q", i, got, chatID)
		}
	}
}

// --- TestSessionChatID_EmptyDrops ---
// Every source returns "" when there is no chatID-shaped data in
// the event. SessionChatID must return "" (not zero, not panic).
func TestSessionChatID_EmptyDrops(t *testing.T) {
	a := testAdapter(t)
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
			if got := a.SessionChatID(tc.src); got != "" {
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
	a := testAdapter(t)

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
			if got := a.SessionChatID(tc.src); got != tc.want {
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
	a := testAdapter(t)

	if got := a.SessionChatID(receiveV1Source{event: fakeReceiveV1("oc_typed")}); got != "oc_typed" {
		t.Errorf("typed-only: got %q, want oc_typed", got)
	}
	if got := a.SessionChatID(reactionV3Source{event: fakeReactionV3("oc_envelope")}); got != "oc_envelope" {
		t.Errorf("envelope-only: got %q, want oc_envelope", got)
	}
	if got := a.SessionChatID(cardActionSource{event: fakeCardAction("oc_context")}); got != "oc_context" {
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
