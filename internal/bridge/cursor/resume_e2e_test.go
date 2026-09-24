package cursor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestE2E_ResumeAfterPrompt drives the cursor-agent ACP path the
// bridge uses in production: session/new, one session/prompt (which
// writes store.db), process close, then Start again with that
// sessionId. cursor-agent advertises loadSession, so the second
// Start is session/load of the same id.
func TestE2E_ResumeAfterPrompt(t *testing.T) {
	skipNoAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	s := NewStarter("cursor", "cursor-agent", DefaultACPArgs)
	ws := t.TempDir()
	word := "BANANA-RESUME-E2E"

	first := startAgent(t, ctx, s, agent.StartConfig{Workspace: ws})
	sid := waitReady(t, first, 45*time.Second)
	if err := first.SendBlocks(ctx, []agent.ContentBlock{{
		Type: agent.ContentText,
		Text: "Reply with exactly " + word + " and nothing else.",
	}}); err != nil {
		first.Close()
		t.Fatalf("SendBlocks: %v", err)
	}
	waitTurn(t, first, 2*time.Minute)
	first.Close()

	second := startAgent(t, ctx, s, agent.StartConfig{Workspace: ws, SessionID: sid})
	defer second.Close()
	got := waitReady(t, second, 45*time.Second)
	if got != sid {
		t.Fatalf("resumed session id = %q, want %q", got, sid)
	}
}

// TestE2E_ResumeBeforeStore_ReturnsUnhealthy closes a session before
// any prompt. cursor-agent has only meta.json then, and session/load
// rejects the id. Start must return ErrResumeUnhealthy instead of
// panicking.
func TestE2E_ResumeBeforeStore_ReturnsUnhealthy(t *testing.T) {
	skipNoAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	s := NewStarter("cursor", "cursor-agent", DefaultACPArgs)
	ws := t.TempDir()
	first := startAgent(t, ctx, s, agent.StartConfig{Workspace: ws})
	sid := waitReady(t, first, 45*time.Second)
	first.Close()

	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		_, err = s.Start(ctx, agent.StartConfig{Workspace: ws, SessionID: sid})
		if err == nil || errors.Is(err, agent.ErrResumeUnhealthy) || !strings.Contains(err.Error(), "initialize") {
			break
		}
		t.Logf("initialize attempt %d: %v", attempt, err)
		time.Sleep(2 * time.Second)
	}
	if err == nil {
		t.Fatal("resume before store.db succeeded, want ErrResumeUnhealthy")
	}
	if !errors.Is(err, agent.ErrResumeUnhealthy) {
		t.Fatalf("resume error = %v, want ErrResumeUnhealthy", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("resume error = %v, want session-not-found detail", err)
	}
}

func startAgent(t *testing.T, ctx context.Context, s *Starter, cfg agent.StartConfig) *agent.Agent {
	t.Helper()
	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		a, err := s.Start(ctx, cfg)
		if err == nil {
			return a
		}
		last = err
		if !strings.Contains(err.Error(), "initialize") {
			t.Fatalf("Start: %v", err)
		}
		t.Logf("initialize attempt %d: %v", attempt, err)
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("Start: %v", last)
	return nil
}

func waitReady(t *testing.T, a *agent.Agent, d time.Duration) string {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-a.Events():
			if !ok {
				t.Fatal("events closed before EventAgentReady")
			}
			if ev.Kind == agent.EventAgentReady && ev.SessionID != "" {
				return ev.SessionID
			}
		case <-deadline:
			t.Fatal("timeout waiting for EventAgentReady")
		}
	}
}

func waitTurn(t *testing.T, a *agent.Agent, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-a.Events():
			if !ok {
				t.Fatal("events closed before turn end")
			}
			if ev.Kind == agent.EventAgentResult || ev.Kind == agent.EventAgentDone || ev.Kind == agent.EventAgentError {
				if ev.Kind == agent.EventAgentError {
					t.Fatalf("turn error: %s", ev.Text)
				}
				return
			}
		case <-deadline:
			t.Fatal("timeout waiting for turn end")
		}
	}
}
