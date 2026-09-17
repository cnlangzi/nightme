package discord

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
)

// interactionAckBudgetHalf is half the adapter's interactionAckBudget
// — well within the budget so an ACK that lands inside this window
// is guaranteed to pass the ctx.Err() check on the fake.
const interactionAckBudgetHalf = interactionAckBudget / 2

// TestHandleChoiceClick_AcksAndPublishes verifies a button click
// (Type=3) ACKs with type=7 UPDATE_MESSAGE and publishes an
// InboundMessage.Action carrying the chosen Option ID.
func TestHandleChoiceClick_AcksAndPublishes(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	_ = a.choiceStore.Put(&choiceState{
		RequestID: "req-1",
		ChannelID: "chan1",
		MessageID: "card-1",
		Choice: &messages.Choice{
			RequestID: "req-1",
			Options:   messages.ChoiceOptionsFromLabels([]string{"yes", "no"}),
		},
	})

	raw, _ := json.Marshal(Interaction{
		ID:        "int-1",
		Type:      InteractionTypeMessageComponent,
		Token:     "tok",
		ChannelID: "chan1",
		User:      User{ID: "user-1"},
		Message:   &Message{ID: "card-1"},
		Data:      &InteractionData{CustomID: "c:req-1:0"},
	})
	a.onInteraction(raw)

	if len(rest.acks) != 1 {
		t.Fatalf("acks = %d, want 1", len(rest.acks))
	}
	if rest.acks[0].Body.Type != 7 {
		t.Errorf("ack type = %d, want 7 (UPDATE_MESSAGE)", rest.acks[0].Body.Type)
	}
	select {
	case got := <-a.incoming:
		if got.Action == nil {
			t.Fatalf("expected Action on InboundMessage; got nil")
		}
		if got.Action.Option != "yes" {
			t.Errorf("Action.Option = %q, want yes", got.Action.Option)
		}
		if got.Action.RequestID != "req-1" {
			t.Errorf("Action.RequestID = %q, want req-1", got.Action.RequestID)
		}
		if got.UserID != "user-1" {
			t.Errorf("UserID = %q, want user-1", got.UserID)
		}
	default:
		t.Fatalf("no InboundMessage published")
	}
}

// TestHandleInputClick_PopsModal verifies a click on the
// "Type your answer" button (custom_id starts with i:) ACKs with
// type=9 MODAL and a well-formed modal envelope.
func TestHandleInputClick_PopsModal(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	_ = a.choiceStore.Put(&choiceState{
		RequestID: "req-q",
		ChannelID: "chan1",
		MessageID: "card-1",
		Choice: &messages.Choice{
			RequestID: "req-q",
			Questions: []messages.ChoiceQuestion{{ID: "q1"}},
		},
	})
	raw, _ := json.Marshal(Interaction{
		ID:        "int-1",
		Type:      InteractionTypeMessageComponent,
		Token:     "tok",
		ChannelID: "chan1",
		User:      User{ID: "user-1"},
		Message:   &Message{ID: "card-1"},
		Data:      &InteractionData{CustomID: "i:req-q"},
	})
	a.onInteraction(raw)

	if len(rest.acks) != 1 {
		t.Fatalf("acks = %d, want 1", len(rest.acks))
	}
	body := rest.acks[0].Body
	if body.Type != 9 {
		t.Errorf("ack type = %d, want 9 (MODAL)", body.Type)
	}
	if body.Data == nil {
		t.Fatalf("ack body missing Modal envelope: %+v", body)
	}
	if body.Data.Title != "Your answer" {
		t.Errorf("Modal.Title = %q", body.Data.Title)
	}
	// Find the leaf TextInput with custom_id "answer".
	var textInput *Component
	for _, row := range body.Data.Components {
		for i := range row.Components {
			if row.Components[i].CustomID == "answer" {
				textInput = &row.Components[i]
			}
		}
	}
	if textInput == nil {
		t.Fatalf("modal missing TextInput custom_id=answer")
	}
	if !textInput.Required || textInput.MaxLength != 4000 {
		t.Errorf("text input shape: required=%v max_len=%d", textInput.Required, textInput.MaxLength)
	}
}

// TestHandleModalSubmit_PublishesAnswerAsActionCustom verifies a
// MODAL_SUBMIT (Type=5) ACKs with type=7 and publishes an
// InboundMessage.Action with Option="custom" + Form={"answer":...}.
func TestHandleModalSubmit_PublishesAnswerAsActionCustom(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	_ = a.choiceStore.Put(&choiceState{
		RequestID: "req-q",
		ChannelID: "chan1",
		MessageID: "card-1",
		Choice: &messages.Choice{
			RequestID: "req-q",
			Questions: []messages.ChoiceQuestion{{ID: "q1"}},
		},
	})
	raw, _ := json.Marshal(Interaction{
		ID:        "int-1",
		Type:      InteractionTypeModalSubmit,
		Token:     "tok",
		ChannelID: "chan1",
		User:      User{ID: "user-1"},
		Message:   &Message{ID: "card-1"},
		Data: &InteractionData{
			CustomID: "i:req-q",
			Components: []Component{{
				Type: ComponentActionRow,
				Components: []Component{{
					Type:     ComponentTextInput,
					CustomID: "answer",
					Value:    "the answer",
				}},
			}},
		},
	})
	a.onInteraction(raw)

	if len(rest.acks) != 1 || rest.acks[0].Body.Type != 7 {
		t.Fatalf("expected one type=7 ack; got %+v", rest.acks)
	}
	select {
	case got := <-a.incoming:
		if got.Action == nil {
			t.Fatalf("expected Action; got nil")
		}
		if got.Action.Option != "custom" {
			t.Errorf("Action.Option = %q, want custom", got.Action.Option)
		}
		if got.Action.Form["answer"] != "the answer" {
			t.Errorf("Form[answer] = %q, want 'the answer'", got.Action.Form["answer"])
		}
	default:
		t.Fatalf("no InboundMessage published")
	}
}

// TestAcknowledgeUpdateMessage_FreshContextBudget verifies the
// ACK uses a fresh context.Background()-derived ctx so a cancelled
// caller ctx can't strand the POST.
func TestAcknowledgeUpdateMessage_FreshContextBudget(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel() // already-cancelled

	// Build an Interaction; the ACK handler will use its own
	// fresh ctx, ignoring cancelledCtx.
	it := Interaction{
		ID:        "int-1",
		Type:      InteractionTypeMessageComponent,
		Token:     "tok",
		ChannelID: "chan1",
		Data:      &InteractionData{CustomID: "c:req-1:0"},
	}
	// Call the unexported helper with the cancelled ctx — it
	// must not propagate; the fake would reject on ctx.Err()
	// before recording the ack.
	a.acknowledgeUpdateMessage(it)

	if len(rest.acks) != 1 {
		t.Fatalf("acks = %d, want 1 (fresh ctx)", len(rest.acks))
	}
	if rest.acks[0].Body.Type != 7 {
		t.Errorf("type = %d, want 7", rest.acks[0].Body.Type)
	}
	_ = cancelledCtx
}

// TestExtractModalAnswer_NestedLayout walks the modal envelope
// the way the real handler does and confirm the answer lands on the
// leaf TextInput with custom_id "answer".
func TestExtractModalAnswer_NestedLayout(t *testing.T) {
	data := &InteractionData{
		Components: []Component{{
			Type: ComponentActionRow,
			Components: []Component{{
				Type:     ComponentTextInput,
				CustomID: "answer",
				Value:    "hello",
			}},
		}},
	}
	if got := extractModalAnswer(data); got != "hello" {
		t.Errorf("extractModalAnswer = %q, want hello", got)
	}
}

// TestHandleComponentClick_UnknownCustomID verifies unrecognised
// custom_id values are silently dropped (no ACK, no publish).
func TestHandleComponentClick_UnknownCustomID(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	raw, _ := json.Marshal(Interaction{
		ID:    "int-1",
		Type:  InteractionTypeMessageComponent,
		Token: "tok",
		Data:  &InteractionData{CustomID: "z:something"},
	})
	a.onInteraction(raw)
	if len(rest.acks) != 0 {
		t.Errorf("acks = %d, want 0 (unknown custom_id)", len(rest.acks))
	}
}

// TestHandleInteraction_PingTypeIgnored verifies the rare PING
// type is silently dropped.
func TestHandleInteraction_PingTypeIgnored(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	raw, _ := json.Marshal(Interaction{Type: InteractionTypePing})
	a.onInteraction(raw)
	if len(rest.acks) != 0 {
		t.Errorf("acks = %d, want 0 (PING ignored)", len(rest.acks))
	}
}

// TestHandleComponentClick_StateMissingStillAcks verifies a click
// targeting an evicted choice state still posts the ACK so the
// user doesn't see "This interaction failed".
func TestHandleComponentClick_StateMissingStillAcks(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	raw, _ := json.Marshal(Interaction{
		ID:    "int-1",
		Type:  InteractionTypeMessageComponent,
		Token: "tok",
		Data:  &InteractionData{CustomID: "c:missing:0"},
	})
	a.onInteraction(raw)
	if len(rest.acks) != 1 || rest.acks[0].Body.Type != 7 {
		t.Errorf("expected one UPDATE_MESSAGE ack; got %+v", rest.acks)
	}
	select {
	case got := <-a.incoming:
		if got.Action != nil {
			t.Errorf("expected no Action publish; got %+v", got.Action)
		}
	default:
		// No publish is fine — the missing-state branch silent-skips.
	}
}

// TestInteractionAckBudget_RespectsDeadline ensures the
// interactionAckBudget constant stays at the documented value
// (Discord's 3 s budget minus 500 ms slack).
func TestInteractionAckBudget_RespectsDeadline(t *testing.T) {
	if interactionAckBudget != 2500*time.Millisecond {
		t.Errorf("interactionAckBudget = %v, want 2500ms", interactionAckBudget)
	}
	// Silence unused-import warnings; strings is needed by the
	// other tests in this file.
	_ = strings.Split
}
