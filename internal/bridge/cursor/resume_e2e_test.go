package cursor

import (
	"context"
	"os"
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
	skipCursorE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	s := NewStarter("cursor", "cursor-agent", DefaultACPArgs)
	ws := t.TempDir()
	word := "BANANA-RESUME-E2E"

	first := startAgent(t, ctx, s, agent.StartConfig{Workspace: ws})
	defer first.Close()
	sid := waitReady(t, first, 45*time.Second)
	if err := first.SendBlocks(ctx, []agent.ContentBlock{{
		Type: agent.ContentText,
		Text: "Reply with exactly " + word + " and nothing else.",
	}}); err != nil {
		t.Fatalf("SendBlocks: %v", err)
	}
	if text := waitTurn(t, first, 2*time.Minute); !strings.Contains(text, word) {
		t.Fatalf("first turn text = %q, want %q", text, word)
	}
	first.Close()

	second := startAgent(t, ctx, s, agent.StartConfig{Workspace: ws, SessionID: sid})
	defer second.Close()
	got := waitReady(t, second, 45*time.Second)
	if got != sid {
		t.Fatalf("resumed session id = %q, want %q", got, sid)
	}
	if err := second.SendBlocks(ctx, []agent.ContentBlock{{
		Type: agent.ContentText,
		Text: "Repeat only the exact token I asked you to reply with in the previous message.",
	}}); err != nil {
		t.Fatalf("recall SendBlocks: %v", err)
	}
	if text := waitTurn(t, second, 2*time.Minute); !strings.Contains(text, word) {
		t.Fatalf("recalled text = %q, want %q", text, word)
	}
}

// TestE2E_ResumeBeforePrompt_StubAllowsLoad closes a session before
// any prompt lands. The bridge pre-writes a stub store.db at
// session/new (see stub.go), so the next session/load on the same id
// must succeed — the resumed session is empty (no prior
// conversation), but the id is reachable. This is the e2e
// counterpart of the timing fix: before the stub was added, the
// same path failed -32602 "Session ... not found" and the runtime
// couldn't safely persist the freshly-assigned id.
func TestE2E_ResumeBeforePrompt_StubAllowsLoad(t *testing.T) {
	skipCursorE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	s := NewStarter("cursor", "cursor-agent", DefaultACPArgs)
	ws := t.TempDir()
	first := startAgent(t, ctx, s, agent.StartConfig{Workspace: ws})
	defer first.Close()
	sid := waitReady(t, first, 45*time.Second)
	first.Close()

	second := startAgent(t, ctx, s, agent.StartConfig{Workspace: ws, SessionID: sid})
	defer second.Close()
	got := waitReady(t, second, 45*time.Second)
	if got != sid {
		t.Fatalf("resumed session id = %q, want %q", got, sid)
	}
}

func skipCursorE2E(t *testing.T) {
	t.Helper()
	skipNoAgent(t)
	if os.Getenv("NIGHTME_CURSOR_E2E") != "1" {
		t.Skip("set NIGHTME_CURSOR_E2E=1 to run cursor-agent resume e2e")
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

func waitTurn(t *testing.T, a *agent.Agent, d time.Duration) string {
	t.Helper()
	var text strings.Builder
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-a.Events():
			if !ok {
				t.Fatal("events closed before turn end")
			}
			if ev.Text != "" {
				text.WriteString(ev.Text)
			}
			if ev.Kind == agent.EventAgentResult || ev.Kind == agent.EventAgentDone || ev.Kind == agent.EventAgentError {
				if ev.Kind == agent.EventAgentError {
					t.Fatalf("turn error: %s", ev.Text)
				}
				return text.String()
			}
		case <-deadline:
			t.Fatal("timeout waiting for turn end")
		}
	}
}
