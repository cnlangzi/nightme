package stop_test

// The structured outcome /stop reports.
//
// A program that drives nightme reads the outcome, not the card — so
// /stop has to say what the stop did: it stopped the turn, there was
// nothing in flight, the agent does not support it, or it failed.
//
// The turn /stop ends belongs to an EARLIER inbound message — that
// one settles as cancelled. This command's own message reports the
// stop itself.

import (
	"context"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	stoppkg "github.com/cnlangzi/nightme/internal/command/stop"
	"github.com/cnlangzi/nightme/internal/messages"
)

// outcomeEmitter records the outbound messages of a chat session —
// among them the settled message that carries the outcome.
type outcomeEmitter struct {
	mu   sync.Mutex
	sent []messages.OutboundMessage
}

func (e *outcomeEmitter) Send(_ context.Context, m messages.OutboundMessage) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sent = append(e.sent, m)
	return nil
}

func (e *outcomeEmitter) settled() *messages.Outcome {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, m := range e.sent {
		if m.Kind == messages.OutMessageSettled {
			return m.Outcome
		}
	}
	return nil
}

// runStop runs /stop against the given chat and returns the outcome
// it reported.
func runStop(t *testing.T, cs *chatsession.ChatSession) *messages.Outcome {
	t.Helper()
	em := &outcomeEmitter{}
	cs.WithEmitter(em)

	// The dispatcher holds the message while the command runs and
	// releases it when the command returns.
	work := cs.Hold("msg-1")
	ctx := chatsession.WithWork(context.Background(), work)

	f := stoppkg.NewFactory()
	if _, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    cs.ChatID,
		MessageID: "msg-1",
		Args:      []string{"stop"},
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	work.Release()
	return em.settled()
}

// TestStopOutcome covers the verdicts /stop can report.
func TestStopOutcome(t *testing.T) {
	t.Run("the turn was stopped", func(t *testing.T) {
		cs, _, _ := setupSelectedAS(t, "claude", "/tmp", false)
		got := runStop(t, cs)
		if got == nil {
			t.Fatal("no settled outcome: /stop reported nothing")
		}
		if got.Status != messages.OutcomeOK {
			t.Fatalf("outcome = %+v, want ok", got)
		}
		if got.Data["agent"] != "claude" {
			t.Errorf("outcome agent = %q, want claude", got.Data["agent"])
		}
	})

	t.Run("no turn in flight", func(t *testing.T) {
		cs, _, _ := setupSelectedAS(t, "claude", "/tmp", true)
		got := runStop(t, cs)
		if got == nil {
			t.Fatal("no settled outcome: /stop reported nothing")
		}
		if got.Status != messages.OutcomeNoop {
			t.Fatalf("outcome = %+v, want noop", got)
		}
	})

	t.Run("no agent selected", func(t *testing.T) {
		mgr := chatsession.NewManager()
		cs, err := mgr.GetOrCreate("stop-outcome-noagent", "claude")
		if err != nil {
			t.Fatalf("GetOrCreate: %v", err)
		}
		if err := cs.SetSelectedCwd("/tmp"); err != nil {
			t.Fatalf("SetSelectedCwd: %v", err)
		}
		// No agent session attached — there is nothing to stop.
		got := runStop(t, cs)
		if got == nil {
			t.Fatal("no settled outcome: /stop reported nothing")
		}
		if got.Status != messages.OutcomeFailed {
			t.Fatalf("outcome = %+v, want failed", got)
		}
		if got.Data["reason"] != "no_agent" {
			t.Errorf("outcome reason = %q, want no_agent", got.Data["reason"])
		}
	})

	t.Run("the agent does not support stop", func(t *testing.T) {
		cs, _, stub := setupSelectedAS(t, "claude", "/tmp", false)
		stub.stopErr = agent.ErrNotSupported
		got := runStop(t, cs)
		if got == nil {
			t.Fatal("no settled outcome: /stop reported nothing")
		}
		if got.Status != messages.OutcomeFailed {
			t.Fatalf("outcome = %+v, want failed", got)
		}
		if got.Data["reason"] != "not_supported" {
			t.Errorf("outcome reason = %q, want not_supported", got.Data["reason"])
		}
	})
}
