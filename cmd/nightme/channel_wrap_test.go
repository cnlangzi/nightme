// Tests for channelWrap: the adapter that bridges the
// *feishu.Adapter (gateway.Channel) into the chatsession.Channel
// abstract surface used by the runtime shim after the
// Commander refactor.
package main

import (
	"context"
	"errors"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/gateway"
)

// fakeGatewayChannel is a minimal in-test gateway.Channel
// implementation that records its received messages. It is
// passed to channelWrap (which only depends on the gateway.Channel
// interface).
type fakeGatewayChannel struct {
	sent          []gateway.OutboundMessage
	sentCard      []gateway.OutboundMessage
	sentCardID    string
	sentCardErr   error
	patched       []gateway.OutboundMessage
	sendErr       error
	defaultReturn string
}

func (f *fakeGatewayChannel) Name() string { return "fake" }
func (f *fakeGatewayChannel) Start(_ context.Context) error { return nil }
func (f *fakeGatewayChannel) Stop(_ context.Context) error  { return nil }
func (f *fakeGatewayChannel) Incoming() <-chan gateway.InboundMessage {
	ch := make(chan gateway.InboundMessage)
	close(ch)
	return ch
}

func (f *fakeGatewayChannel) Send(_ context.Context, m gateway.OutboundMessage) error {
	f.sent = append(f.sent, m)
	return f.sendErr
}

func (f *fakeGatewayChannel) SendCard(_ context.Context, m gateway.OutboundMessage) (string, error) {
	f.sentCard = append(f.sentCard, m)
	return f.sentCardID, f.sentCardErr
}

func (f *fakeGatewayChannel) Patch(_ context.Context, m gateway.OutboundMessage) error {
	f.patched = append(f.patched, m)
	return nil
}

// Test channelWrap sets OutboundKind for the gateway channel.
func TestChannelWrap_SendSetsCommandReply(t *testing.T) {
	gw := &fakeGatewayChannel{}
	wrap := newChannelWrap(gw)

	if err := wrap.Send(context.Background(), chatsession.OutboundMessage{
		ChatID:  "c1",
		Text:    "hello",
		ReplyTo: "msg-1",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(gw.sent) != 1 {
		t.Fatalf("expected 1 sent, got %d", len(gw.sent))
	}
	got := gw.sent[0]
	if got.Kind != gateway.OutCommandReply {
		t.Errorf("Kind = %v, want OutCommandReply", got.Kind)
	}
	if got.ChatID != "c1" || got.Text != "hello" || got.ReplyTo != "msg-1" {
		t.Errorf("Send payload wrong: %+v", got)
	}
}

// Test channelWrap.SendCard sets OutCard kind and translates the
// gtw-friendly chatsession.Card (Title/Body/Choices) into the
// gateway OutboundMessage's Card field.
func TestChannelWrap_SendCardTranslatesCardFields(t *testing.T) {
	gw := &fakeGatewayChannel{sentCardID: "bot-42"}
	wrap := newChannelWrap(gw)

	got, err := wrap.SendCard(context.Background(), chatsession.OutboundMessage{
		ChatID: "c2",
		Card: &chatsession.Card{
			Kind:      "decision",
			Title:     "T",
			Body:      "B",
			RequestID: "req-1",
			Choices: []chatsession.CardChoice{
				{Emoji: "🆕", Label: "Use new variant", Action: "act:/x"},
			},
		},
	})
	if err != nil {
		t.Fatalf("SendCard: %v", err)
	}
	if got != "bot-42" {
		t.Errorf("returned id = %q, want bot-42", got)
	}
	if len(gw.sentCard) != 1 {
		t.Fatalf("expected 1 sentCard, got %d", len(gw.sentCard))
	}
	m := gw.sentCard[0]
	if m.Kind != gateway.OutCard {
		t.Errorf("Kind = %v, want OutCard", m.Kind)
	}
	if m.Card == nil {
		t.Fatal("Card nil")
	}
	if m.Card.Title != "T" || m.Card.Body != "B" || m.Card.RequestID != "req-1" {
		t.Errorf("Card fields wrong: %+v", m.Card)
	}
	if got := m.Card.Kind; got != gateway.CardKindDecision {
		t.Errorf("CardKind = %v, want Decision (string=\"decision\")", got)
	}
	if len(m.Card.Choices) != 1 || m.Card.Choices[0].Emoji != "🆕" {
		t.Errorf("Choices translation wrong: %+v", m.Card.Choices)
	}
}

// Test channelWrap.SendCardError returns the SendCard error and
// does NOT fall back to a Send automatically — caller decides.
func TestChannelWrap_SendCardErrorPropagates(t *testing.T) {
	gw := &fakeGatewayChannel{sentCardErr: errors.New("card quota exceeded")}
	wrap := newChannelWrap(gw)
	_, err := wrap.SendCard(context.Background(), chatsession.OutboundMessage{
		Card: &chatsession.Card{Title: "T"},
	})
	if err == nil || err.Error() != "card quota exceeded" {
		t.Fatalf("err = %v, want \"card quota exceeded\"", err)
	}
}

// Test channelWrap.Patch sets OutCardPatch kind and copies the
// PATCH body fields into the gateway OutboundMessage's Card
// field (so feishu picks them up when the kind matches).
func TestChannelWrap_PatchBuildsOutCardPatch(t *testing.T) {
	gw := &fakeGatewayChannel{}
	wrap := newChannelWrap(gw)

	err := wrap.Patch(context.Background(), chatsession.OutboundMessage{
		ChatID:             "c3",
		ReplyTo:           "msg-42",
		PatchBotMsgID:      "msg-42",
		PatchChosenEmoji:   "✅",
		PatchResult:       "card updated",
		CardTitle:         "Updated",
		CardBody:          "new body",
		CardRequestID:     "req-2",
		ChosenChoiceEmoji: "✅",
		CardChoices: []chatsession.CardChoice{
			{Emoji: "✅", Label: "Use new variant", Action: "act:/y"},
		},
	})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if len(gw.sent) != 1 {
		t.Fatalf("expected 1 sent (PATCH uses Send), got %d", len(gw.sent))
	}
	m := gw.sent[0]
	if m.Kind != gateway.OutCardPatch {
		t.Errorf("Kind = %v, want OutCardPatch", m.Kind)
	}
	if m.ReplyTo != "msg-42" {
		t.Errorf("ReplyTo = %q, want msg-42", m.ReplyTo)
	}
	if m.Card == nil {
		t.Fatal("Card nil — PATCH fields should populate Card")
	}
	if m.Card.Title != "Updated" || m.Card.Body != "new body" {
		t.Errorf("Card body fields wrong: %+v", m.Card)
	}
	if len(m.Card.Choices) != 1 || m.Card.Choices[0].Emoji != "✅" {
		t.Errorf("Choices translation wrong: %+v", m.Card.Choices)
	}
}

// Test channelWrap.Patch with only PatchResult (no Card fields)
// still sets OutCardPatch so the gateway layer triggers a PATCH
// with the result text.
func TestChannelWrap_PatchResultOnly(t *testing.T) {
	gw := &fakeGatewayChannel{}
	wrap := newChannelWrap(gw)

	err := wrap.Patch(context.Background(), chatsession.OutboundMessage{
		ChatID:        "c4",
		ReplyTo:      "msg-99",
		PatchBotMsgID: "msg-99",
		PatchResult:   "card updated",
	})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	m := gw.sent[0]
	if m.Kind != gateway.OutCardPatch {
		t.Errorf("Kind = %v, want OutCardPatch", m.Kind)
	}
	if m.ReplyTo != "msg-99" || m.Text != "card updated" {
		t.Errorf("ReplyTo/Text wrong: %+v", m)
	}
	if m.Card != nil {
		t.Errorf("Card should be nil when only PatchResult set: %+v", m.Card)
	}
}

// Test cardKindFromString: each canonical kind maps to its
// enum, empty/unknown defaults to Decision.
func TestCardKindFromString(t *testing.T) {
	cases := []struct {
		in   string
		want gateway.CardKind
	}{
		{"permission", gateway.CardKindPermission},
		{"preview", gateway.CardKindPreview},
		{"decision", gateway.CardKindDecision},
		{"", gateway.CardKindDecision},
		{"unknown", gateway.CardKindDecision},
	}
	for _, tc := range cases {
		if got := cardKindFromString(tc.in); got != tc.want {
			t.Errorf("cardKindFromString(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
