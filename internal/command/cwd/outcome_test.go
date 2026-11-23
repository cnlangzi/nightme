package cwd

// The structured outcome /cwd reports.
//
// A program that drives nightme reads the outcome, not the card — so
// /cwd has to say what it did: the workspace it set, or why it
// could not.

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/outcome"
)

// outcomeEmitter records the outbound messages of a chat session —
// among them the settled message that carries the outcome.
type outcomeEmitter struct {
	mu   sync.Mutex
	sent []messages.OutboundMessage
}

func (e *outcomeEmitter) Send(_ context.Context, m messages.OutboundMessage) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sent = append(e.sent, m)
	return "", nil
}

func (e *outcomeEmitter) settled() *outcome.Outcome {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, m := range e.sent {
		if m.Kind == messages.OutMessageSettled {
			return m.Outcome
		}
	}
	return nil
}

// runCwd runs /cwd with the given argv and returns the outcome it
// reported.
func runCwd(t *testing.T, argv ...string) *outcome.Outcome {
	t.Helper()
	mgr := chatsession.NewManager()
	cs, err := mgr.GetOrCreate("cwd-outcome", "test")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	em := &outcomeEmitter{}
	cs.WithEmitter(em)

	// The dispatcher holds the message while the command runs and
	// releases it when the command returns.
	work := cs.Hold("msg-1")
	ctx := chatsession.WithWork(context.Background(), work)

	f := NewFactory()
	if _, err := f.Handle(ctx, command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    "cwd-outcome",
		MessageID: "msg-1",
		Args:      append([]string{"cwd"}, argv...),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	work.Release()
	return em.settled()
}

// TestCwdOutcome covers the paths /cwd can end on.
func TestCwdOutcome(t *testing.T) {
	t.Run("the workspace was set", func(t *testing.T) {
		dir := t.TempDir()
		got := runCwd(t, dir)
		if got == nil {
			t.Fatal("no settled outcome: /cwd reported nothing")
		}
		if got.Status != http.StatusOK {
			t.Fatalf("outcome = %+v, want ok", got)
		}
		if got.Data["cwd"] == "" {
			t.Errorf("outcome data = %v, want a cwd", got.Data)
		}
	})

	t.Run("no path", func(t *testing.T) {
		got := runCwd(t)
		if got == nil {
			t.Fatal("no settled outcome: /cwd reported nothing")
		}
		if got.Status != http.StatusBadRequest {
			t.Errorf("outcome status = %d, want %d", got.Status, http.StatusBadRequest)
		}
	})

	t.Run("unknown flag", func(t *testing.T) {
		got := runCwd(t, "--nope")
		if got == nil {
			t.Fatal("no settled outcome: /cwd reported nothing")
		}
		if got.Status != http.StatusBadRequest {
			t.Errorf("outcome status = %d, want %d", got.Status, http.StatusBadRequest)
		}
	})

	t.Run("the path does not exist", func(t *testing.T) {
		got := runCwd(t, "/definitely/not/a/directory")
		if got == nil {
			t.Fatal("no settled outcome: /cwd reported nothing")
		}
		if got.Status != http.StatusInternalServerError {
			t.Errorf("outcome status = %d, want %d", got.Status, http.StatusInternalServerError)
		}
	})
}
