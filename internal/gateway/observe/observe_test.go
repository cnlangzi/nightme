package observe

import (
	"context"
	"net/http"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/outcome"
)

// TestTracker_OnlyRegisteredMessagesAreTracked — the tracker holds
// the messages a program registered. What the daemon says about a
// message nobody registered is nobody's business.
func TestTracker_OnlyRegisteredMessagesAreTracked(t *testing.T) {
	tr := NewTracker()
	tr.Register("om_1", "oc_test", "/gtw commit")

	if _, ok := tr.Get("om_1"); !ok {
		t.Fatal("the registered message is not tracked")
	}
	if _, ok := tr.Get("om_human"); ok {
		t.Fatal("a message nobody registered is tracked")
	}
}

// TestTracker_SeenRegistersTheProgramsMessages — a message the
// personal agent sent is registered from the inbound side, so the
// tracker knows about it before the daemon says anything about it.
func TestTracker_SeenRegistersTheProgramsMessages(t *testing.T) {
	tr := NewTracker()
	tr.Seen(&messages.InboundMessage{
		ChatID:    "oc_test",
		MessageID: "om_1",
		Text:      "/gtw commit",
		Origin:    messages.OriginMe,
	})
	if _, ok := tr.Get("om_1"); !ok {
		t.Fatal("the program's message is not tracked")
	}

	// A person's message is not the program's.
	tr.Seen(&messages.InboundMessage{
		ChatID:    "oc_test",
		MessageID: "om_human",
		Text:      "hello",
		Origin:    messages.OriginHuman,
	})
	if _, ok := tr.Get("om_human"); ok {
		t.Fatal("a person's message was tracked")
	}
}

// TestTracker_RegisterIsIdempotent — a message that is already tracked
// keeps the state it has: the answer the daemon already gave is not
// thrown away.
func TestTracker_RegisterIsIdempotent(t *testing.T) {
	tr := NewTracker()
	tr.Register("om_1", "oc_test", "/gtw commit")
	tr.Observe(messages.OutboundMessage{
		ChatID:  "oc_test",
		Kind:    messages.OutReply,
		Text:    "working",
		ReplyTo: "om_1",
	})
	tr.Register("om_1", "oc_test", "/gtw commit")

	ex, ok := tr.Get("om_1")
	if !ok {
		t.Fatal("the message is no longer tracked")
	}
	if len(ex.Transcript) != 1 {
		t.Fatalf("the transcript has %d lines, want 1", len(ex.Transcript))
	}
}

// TestTracker_AnUnrelatedMessageIsNotAttributed — what the daemon
// says about another chat, or about a message of this chat nobody
// registered, does not land in the execution.
func TestTracker_AnUnrelatedMessageIsNotAttributed(t *testing.T) {
	tr := NewTracker()
	tr.Register("om_1", "oc_test", "/gtw commit")

	// Another chat's message.
	tr.Observe(messages.OutboundMessage{
		ChatID:  "oc_other",
		Kind:    messages.OutReply,
		Text:    "not mine",
		ReplyTo: "om_1",
	})
	// This chat's message that nobody registered.
	tr.Observe(messages.OutboundMessage{
		ChatID:  "oc_test",
		Kind:    messages.OutReply,
		Text:    "not mine",
		ReplyTo: "om_unknown",
	})

	ex, ok := tr.Get("om_1")
	if !ok {
		t.Fatal("the message is no longer tracked")
	}
	if len(ex.Transcript) != 0 {
		t.Fatalf("the transcript has %d lines, want 0", len(ex.Transcript))
	}
}

// TestTracker_ThePhaseOnlyMovesForward — a late answer cannot pull a
// settled execution back.
func TestTracker_ThePhaseOnlyMovesForward(t *testing.T) {
	tr := NewTracker()
	tr.Register("om_1", "oc_test", "/gtw commit")

	tr.Observe(messages.OutboundMessage{
		ChatID: "oc_test",
		Kind:   messages.OutMessageState,
		MessageState: &messages.MessageStatePayload{
			MessageID: "om_1",
			State:     agent.MessageSubmitted,
		},
		ReplyTo: "om_1",
	})
	ex, _ := tr.Get("om_1")
	if ex.Phase != PhaseRunning {
		t.Fatalf("phase = %q, want %q", ex.Phase, PhaseRunning)
	}

	tr.Observe(messages.OutboundMessage{
		ChatID: "oc_test",
		Kind:   messages.OutMessageState,
		MessageState: &messages.MessageStatePayload{
			MessageID: "om_1",
			State:     agent.MessageQueued,
		},
		ReplyTo: "om_1",
	})
	ex, _ = tr.Get("om_1")
	if ex.Phase != PhaseRunning {
		t.Fatalf("phase = %q, want it to stay %q", ex.Phase, PhaseRunning)
	}
}

// TestTracker_ASettleClosesTheWatch — a watcher sees the final
// snapshot and then the channel closes.
func TestTracker_ASettleClosesTheWatch(t *testing.T) {
	tr := NewTracker()
	tr.Register("om_1", "oc_test", "/gtw commit")

	ch := tr.Watch("om_1")
	tr.Observe(messages.OutboundMessage{
		ChatID:  "oc_test",
		Kind:    messages.OutMessageSettled,
		ReplyTo: "om_1",
		Outcome: &outcome.Outcome{
			Success:    true,
			Status:     http.StatusOK,
			StatusText: "committed",
		},
	})

	var last Execution
	closed := false
	for snap := range ch {
		last = snap
	}
	closed = true
	if !closed {
		t.Fatal("the watch did not close")
	}
	if last.Phase != PhaseSettled {
		t.Fatalf("phase = %q, want %q", last.Phase, PhaseSettled)
	}
	if last.Outcome.StatusText != "committed" {
		t.Fatalf("outcome = %q, want committed", last.Outcome.StatusText)
	}
}

// TestTracker_APendingPromptIsVisible — the prompts the chat waits
// for show up on the execution.
func TestTracker_APendingPromptIsVisible(t *testing.T) {
	tr := NewTracker()
	tr.Register("om_1", "oc_test", "/gtw commit")

	tr.Observe(messages.OutboundMessage{
		ChatID:  "oc_test",
		Kind:    messages.OutChoice,
		ReplyTo: "om_1",
		Choice: &messages.Choice{
			RequestID: "perm:oc_test:1",
			Title:     "Waiting for approval",
			Options:   []messages.ChoiceOption{{ID: "yes", Label: "yes"}},
		},
	})

	ex, _ := tr.Get("om_1")
	if len(ex.Choices) != 1 {
		t.Fatalf("the execution carries %d prompts, want 1", len(ex.Choices))
	}
	if ex.Choices[0].RequestID != "perm:oc_test:1" {
		t.Fatalf("request id = %q, want the prompt's", ex.Choices[0].RequestID)
	}
}

// TestTap_ForwardsUnchangedAndObserves — the tap hands the message on
// as it is and folds it into the tracker.
func TestTap_ForwardsUnchangedAndObserves(t *testing.T) {
	tr := NewTracker()
	tr.Register("om_1", "oc_test", "/gtw commit")

	var forwarded []messages.OutboundMessage
	inner := emitterFunc(func(_ context.Context, msg messages.OutboundMessage) (string, error) {
		forwarded = append(forwarded, msg)
		return "", nil
	})

	em := Tap(inner, tr)
	msg := messages.OutboundMessage{
		ChatID:  "oc_test",
		Kind:    messages.OutReply,
		Text:    "working",
		ReplyTo: "om_1",
	}
	if _, err := em.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(forwarded) != 1 {
		t.Fatalf("%d messages were forwarded, want 1", len(forwarded))
	}
	if forwarded[0].Text != "working" || forwarded[0].ReplyTo != "om_1" {
		t.Fatalf("the message was changed on the way: %+v", forwarded[0])
	}
	ex, _ := tr.Get("om_1")
	if len(ex.Transcript) != 1 {
		t.Fatalf("the execution carries %d lines, want 1", len(ex.Transcript))
	}
}

// TestTap_WithoutATrackerForwardsUnchanged — a tap with nothing to
// observe is the emitter itself.
func TestTap_WithoutATrackerForwardsUnchanged(t *testing.T) {
	inner := emitterFunc(func(context.Context, messages.OutboundMessage) (string, error) { return "", nil })
	if got := Tap(inner, nil); got == nil {
		t.Fatal("the tap dropped the emitter")
	}
}

// emitterFunc is a messages.Emitter backed by a function.
type emitterFunc func(ctx context.Context, msg messages.OutboundMessage) (string, error)

func (f emitterFunc) Send(ctx context.Context, msg messages.OutboundMessage) (string, error) {
	return f(ctx, msg)
}

// TestObservers_SkipNil — an observer that is not there is skipped.
func TestObservers_SkipNil(t *testing.T) {
	seen := 0
	obs := Observers(nil, observerFunc(func(*messages.InboundMessage) { seen++ }))
	obs.Seen(&messages.InboundMessage{MessageID: "om_1"})
	if seen != 1 {
		t.Fatalf("the observer was called %d times, want 1", seen)
	}
}

// observerFunc is an Observer backed by a function.
type observerFunc func(msg *messages.InboundMessage)

func (f observerFunc) Seen(msg *messages.InboundMessage) { f(msg) }
