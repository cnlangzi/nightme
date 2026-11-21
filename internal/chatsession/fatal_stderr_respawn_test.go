package chatsession

import (
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession/agentstest"
)

// TestChatSession_BridgeFailure_RespawnsTheSameSession pins the
// recovery half of the fatal-stderr contract: when the CLI reports
// its own failure and goes away, the session comes back on a fresh
// CLI under the same resume id, so the next user message lands on a
// CLI that re-read its configuration.
//
// The bridge closes the CLI itself after reporting the failure; the
// chat layer's job is to notice the death and rebuild the session.
func TestChatSession_BridgeFailure_RespawnsTheSameSession(t *testing.T) {
	spawner := newFakeSpawner()
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	cs = cs.WithSpawner(spawner)
	cs.SetSelectedCwd("/x")
	cs.SetSelectedAgent("claude")

	as, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatalf("LookupSelectedAgentSession: %v", err)
	}
	t.Cleanup(func() { as.Shutdown() })
	as.SetSessionID("sess-1")
	// The spawner keys the fake by the agent and the cwd the AS
	// was spawned with — not by the cwd this test asked for, which
	// the platform rewrites (filepath.Clean turns "/x" into "\x"
	// on Windows).
	first := spawner.Get(as.Agent, as.Cwd)
	if first == nil {
		t.Fatal("no fake bridge for the spawned session")
	}

	replies := make(chan string, 4)
	cs.AgentEventBus.Subscribe(func(env AgentEventEnvelope) bool {
		if env.Event != nil && env.Event.Kind == agent.EventAgentText {
			select {
			case replies <- env.Event.Text:
			default:
			}
		}
		return false
	})

	// The CLI reports the failure it hit and then goes away.
	const line = `[claude-code:unrecognized_model] {"model":"MiniMax-M3[1m]"}`
	first.PushEvent(agent.AgentEvent{
		Kind: agent.EventAgentError,
		Err:  agent.FatalStderrError(line),
		Diagnostic: &agent.BridgeDiagnostic{
			StderrTail: line,
			AgentName:  "claude",
		},
	})
	first.Close()

	if !waitFor(func() bool { return spawnerCalls(spawner) >= 2 }, 3*time.Second) {
		t.Fatalf("session was not respawned (spawn calls = %d, want >= 2)", spawnerCalls(spawner))
	}
	if id := spawnerResumeID(spawner); id != "sess-1" {
		t.Errorf("respawn resume id = %q, want %q", id, "sess-1")
	}
	if got := as.SessionID(); got != "sess-1" {
		t.Errorf("SessionID() = %q, want %q (the respawn must not change it)", got, "sess-1")
	}
	second := spawner.Get(as.Agent, as.Cwd)
	if second == first {
		t.Fatal("the respawn reused the dead bridge; the next message must land on a new CLI")
	}

	// The replacement has to be usable: its events reach the chat.
	// The probe needs a turn in flight: a turn-scoped event that
	// arrives with no prompt is dropped.
	agentstest.SetCurrentPrompt(as, &Prompt{ID: "p_probe", LastMessageID: "m_probe"})
	second.PushEvent(agent.AgentEvent{Kind: agent.EventAgentText, Text: "from the new CLI"})
	select {
	case got := <-replies:
		if got != "from the new CLI" {
			t.Errorf("reply = %q, want %q", got, "from the new CLI")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the replacement CLI's events never reached the chat")
	}
}

// spawnerCalls reads the spawn counter under the spawner's lock —
// the respawn runs on the event dispatcher goroutine.
func spawnerCalls(s *fakeSpawner) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// spawnerResumeID reads the resume id of the last spawn under the
// spawner's lock.
func spawnerResumeID(s *fakeSpawner) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastResumeID
}
