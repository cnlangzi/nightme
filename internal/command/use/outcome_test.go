package use

// The structured outcome /use reports.
//
// A program that drives nightme reads the outcome, not the card — so
// /use has to say which agent it switched to, or why it could not.

import (
	"context"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/agentsession/agentstest"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
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

// quietDriver is the agent behind the session /use switches to —
// it never gets a prompt in this test.
type quietDriver struct{}

func (d *quietDriver) SendBlocks(context.Context, []agent.ContentBlock) error { return nil }
func (d *quietDriver) SendPermission(string) error                            { return nil }
func (d *quietDriver) Reset(context.Context) error                            { return nil }
func (d *quietDriver) Close() error                                           { return nil }
func (d *quietDriver) Stop(context.Context) error                             { return agent.ErrNotSupported }
func (d *quietDriver) Keepalive(context.Context, func(context.Context) error) error {
	return nil
}

// runUse runs /use with the given argv and returns the outcome it
// reported.
func runUse(t *testing.T, argv ...string) *messages.Outcome {
	t.Helper()
	mgr := chatsession.NewManager()
	cs, err := mgr.GetOrCreate("use-outcome", "claude")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	em := &outcomeEmitter{}
	cs.WithEmitter(em)
	if err := cs.SetSelectedCwd(t.TempDir()); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}

	// The dispatcher holds the message while the command runs and
	// releases it when the command returns.
	work := cs.Hold("msg-1")
	ctx := chatsession.WithWork(context.Background(), work)

	f := NewFactory()
	if _, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    "use-outcome",
		MessageID: "msg-1",
		Args:      append([]string{"use"}, argv...),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	work.Release()
	return em.settled()
}

// TestUseOutcome covers the paths /use can end on.
func TestUseOutcome(t *testing.T) {
	t.Run("no agent name", func(t *testing.T) {
		got := runUse(t)
		if got == nil {
			t.Fatal("no settled outcome: /use reported nothing")
		}
		if got.Status != messages.OutcomeFailed {
			t.Fatalf("outcome = %+v, want failed", got)
		}
		if got.Data["reason"] != "usage" {
			t.Errorf("outcome reason = %q, want usage", got.Data["reason"])
		}
	})

	t.Run("unknown flag", func(t *testing.T) {
		got := runUse(t, "--nope")
		if got == nil {
			t.Fatal("no settled outcome: /use reported nothing")
		}
		if got.Status != messages.OutcomeFailed {
			t.Fatalf("outcome = %+v, want failed", got)
		}
		if got.Data["reason"] != "usage" {
			t.Errorf("outcome reason = %q, want usage", got.Data["reason"])
		}
	})

	t.Run("the agent was switched", func(t *testing.T) {
		mgr := chatsession.NewManager()
		cs, err := mgr.GetOrCreate("use-outcome-ok", "claude")
		if err != nil {
			t.Fatalf("GetOrCreate: %v", err)
		}
		em := &outcomeEmitter{}
		cs.WithEmitter(em)
		dir := t.TempDir()
		if err := cs.SetSelectedCwd(dir); err != nil {
			t.Fatalf("SetSelectedCwd: %v", err)
		}
		// A live agent session, so /use can switch to it.
		as := chatsession.NewAgentSession("as-use", cs.ID, "claude", dir, nil)
		agentstest.SetHandle(as, agent.NewAgent(
			agent.NewInfo("claude", agent.ModeACP, "fake", nil, nil),
			0, make(chan agent.AgentEvent, 4), &quietDriver{}))
		agentstest.SetStatus(as, agentsession.StatusRunning)
		cs.AttachAgentSessionForTest(as)
		cs.SelectedAgentSessionForTest(as)

		work := cs.Hold("msg-1")
		ctx := chatsession.WithWork(context.Background(), work)
		f := NewFactory()
		if _, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
			ChatID:    "use-outcome-ok",
			MessageID: "msg-1",
			Args:      []string{"use", "claude"},
		}); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		work.Release()

		got := em.settled()
		if got == nil {
			t.Fatal("no settled outcome: /use reported nothing")
		}
		if got.Status != messages.OutcomeOK {
			t.Fatalf("outcome = %+v, want ok", got)
		}
		if got.Data["agent"] != "claude" {
			t.Errorf("outcome agent = %q, want claude", got.Data["agent"])
		}
	})
}
