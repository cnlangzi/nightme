package chatsession

import (
	"context"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// recordingEmitter captures the outbound messages a ChatSession
// emits.
type recordingEmitter struct {
	mu   sync.Mutex
	sent []messages.OutboundMessage
}

func (e *recordingEmitter) Send(_ context.Context, msg messages.OutboundMessage) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sent = append(e.sent, msg)
	return nil
}

func (e *recordingEmitter) settled() []messages.OutboundMessage {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []messages.OutboundMessage
	for _, m := range e.sent {
		if m.Kind == messages.OutMessageSettled {
			out = append(out, m)
		}
	}
	return out
}

func newSettleSession(t *testing.T) (*ChatSession, *recordingEmitter) {
	t.Helper()
	cs := newChatSessionForTest("cs_settle")
	em := &recordingEmitter{}
	cs.WithEmitter(em)
	return cs, em
}

// TestHold_NestedHoldsEmitOnceAfterLastRelease checks that the
// settled message goes out once, when the last hold is released.
func TestHold_NestedHoldsEmitOnceAfterLastRelease(t *testing.T) {
	cs, em := newSettleSession(t)

	w1 := cs.Hold("m1")
	w2 := cs.Hold("m1")
	if w1 != w2 {
		t.Fatal("Hold returned different Works for the same msgID")
	}

	w1.Report(messages.Outcome{Status: messages.OutcomeOK, Summary: "done"})
	w1.Release()
	if got := em.settled(); len(got) != 0 {
		t.Fatalf("settled emitted %d times before the last release", len(got))
	}

	w2.Release()
	got := em.settled()
	if len(got) != 1 {
		t.Fatalf("settled emitted %d times, want 1", len(got))
	}
	if got[0].ReplyTo != "m1" {
		t.Errorf("settled ReplyTo = %q, want %q", got[0].ReplyTo, "m1")
	}
	if got[0].Outcome == nil || got[0].Outcome.Status != messages.OutcomeOK {
		t.Errorf("settled outcome = %+v, want ok", got[0].Outcome)
	}
}

// TestHold_DoubleReleaseIsSafe checks that releasing more times than
// held does not emit a second settled message.
func TestHold_DoubleReleaseIsSafe(t *testing.T) {
	cs, em := newSettleSession(t)

	w := cs.Hold("m1")
	w.Release()
	w.Release()
	w.Release()
	if got := em.settled(); len(got) != 1 {
		t.Fatalf("settled emitted %d times, want 1", len(got))
	}
}

// TestHold_EmptyMsgIDEmitsNothing checks that a message with no id
// cannot be keyed.
func TestHold_EmptyMsgIDEmitsNothing(t *testing.T) {
	cs, em := newSettleSession(t)

	if w := cs.Hold(""); w != nil {
		t.Fatal("Hold(\"\") returned a Work")
	}
	if got := em.settled(); len(got) != 0 {
		t.Fatalf("settled emitted %d times, want 0", len(got))
	}
}

// TestHold_UnreportedSettlesUnknown checks that a message nobody
// reported on settles as unknown.
func TestHold_UnreportedSettlesUnknown(t *testing.T) {
	cs, em := newSettleSession(t)

	w := cs.Hold("m1")
	w.Release()

	got := em.settled()
	if len(got) != 1 {
		t.Fatalf("settled emitted %d times, want 1", len(got))
	}
	if got[0].Outcome == nil || got[0].Outcome.Status != messages.OutcomeUnknown {
		t.Errorf("settled outcome = %+v, want unknown", got[0].Outcome)
	}
}

// TestHoldUntilPromptEnd_MapsReason checks the reason → status
// mapping and that only the matching msgID settles.
func TestHoldUntilPromptEnd_MapsReason(t *testing.T) {
	cases := []struct {
		name   string
		reason agent.PromptEndReason
		want   messages.OutcomeStatus
	}{
		{"clean", agent.PromptEndClean, messages.OutcomeOK},
		{"user stopped", agent.PromptEndUserStopped, messages.OutcomeCancelled},
		{"user killed", agent.PromptEndUserKilled, messages.OutcomeCancelled},
		{"error", agent.PromptEndError, messages.OutcomeFailed},
		{"process died", agent.PromptEndProcessDied, messages.OutcomeFailed},
		{"stalled", agent.PromptEndStalledKilled, messages.OutcomeFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs, em := newSettleSession(t)
			cs.HoldUntilPromptEnd("m1")

			// A prompt end for another message must not settle it.
			cs.PromptEndBus.Publish(PromptEndedEvent{UserMsgID: "other", Reason: tc.reason})
			if got := em.settled(); len(got) != 0 {
				t.Fatalf("settled emitted for another msgID")
			}

			cs.PromptEndBus.Publish(PromptEndedEvent{UserMsgID: "m1", Reason: tc.reason})
			got := em.settled()
			if len(got) != 1 {
				t.Fatalf("settled emitted %d times, want 1", len(got))
			}
			if got[0].Outcome == nil || got[0].Outcome.Status != tc.want {
				t.Errorf("settled status = %+v, want %s", got[0].Outcome, tc.want)
			}
			if got[0].Outcome.Data["end_reason"] == "" {
				t.Error("settled outcome has no end_reason")
			}
		})
	}
}

// TestQueueUserMessage_HoldsUntilPromptEnd checks that a queued
// message settles when its turn ends.
func TestQueueUserMessage_HoldsUntilPromptEnd(t *testing.T) {
	cs, em := newSettleSession(t)

	if err := cs.QueueUserMessage(Message{ID: "m1", ChatID: cs.ID}); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}
	if got := em.settled(); len(got) != 0 {
		t.Fatalf("settled emitted %d times before the turn ended", len(got))
	}

	cs.PromptEndBus.Publish(PromptEndedEvent{UserMsgID: "m1", Reason: agent.PromptEndClean})
	got := em.settled()
	if len(got) != 1 {
		t.Fatalf("settled emitted %d times, want 1", len(got))
	}
	if got[0].Outcome.Status != messages.OutcomeOK {
		t.Errorf("settled status = %s, want ok", got[0].Outcome.Status)
	}
}

// TestReportOutcome_UsesContextWork checks the ctx-carried report
// path, including the no-op when the context carries no work.
func TestReportOutcome_UsesContextWork(t *testing.T) {
	cs, em := newSettleSession(t)

	w := cs.Hold("m1")
	ctx := WithWork(context.Background(), w)
	ReportOutcome(ctx, messages.Outcome{Status: messages.OutcomeNoop, Summary: "nothing to do"})
	w.Release()

	got := em.settled()
	if len(got) != 1 {
		t.Fatalf("settled emitted %d times, want 1", len(got))
	}
	if got[0].Outcome.Status != messages.OutcomeNoop {
		t.Errorf("settled status = %s, want noop", got[0].Outcome.Status)
	}

	// No work on the context — a no-op, not a panic.
	ReportOutcome(context.Background(), messages.Outcome{Status: messages.OutcomeFailed})
}

// TestReportOutcome_MergesWithCommandOutcome checks that a command's
// verdict survives the agent turn's verdict when the turn is worse,
// and that the command's summary survives a tie.
func TestReportOutcome_MergesWithCommandOutcome(t *testing.T) {
	cs, em := newSettleSession(t)

	w := cs.Hold("m1")
	ctx := WithWork(context.Background(), w)
	ReportOutcome(ctx, messages.Outcome{Status: messages.OutcomeIncomplete, Summary: "1/3 groups unfinished"})
	cs.PromptEndBus.Publish(PromptEndedEvent{UserMsgID: "m1", Reason: agent.PromptEndClean})
	w.Release()

	got := em.settled()
	if len(got) != 1 {
		t.Fatalf("settled emitted %d times, want 1", len(got))
	}
	if got[0].Outcome.Status != messages.OutcomeIncomplete {
		t.Errorf("settled status = %s, want incomplete", got[0].Outcome.Status)
	}
	if got[0].Outcome.Summary != "1/3 groups unfinished" {
		t.Errorf("settled summary = %q, want %q", got[0].Outcome.Summary, "1/3 groups unfinished")
	}
}
