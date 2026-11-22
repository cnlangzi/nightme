package agentsession

import (
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestRespawn_StartsAReaderForTheReplacementBridge pins the readpump
// slot contract the respawn path depends on: the readpump of a dead
// bridge frees the slot when it sees that bridge's events channel
// close, so the bridge that replaces it gets a reader of its own.
//
// Without the reader the replacement is mute: its events reach a
// channel nobody drains and the next turn never completes.
func TestRespawn_StartsAReaderForTheReplacementBridge(t *testing.T) {
	spawner := newFakeSpawner()
	// Each spawn gets its own fake: a respawn that handed back the
	// dead bridge's closed channel would prove nothing.
	var bridge *fakeAgentSession
	spawner.spawnFn = func(name, cwd string) (*agent.Agent, error) {
		f := newFakeAgentSession(20000 + spawner.calls)
		spawner.fakes[spawnKey{name, cwd}] = f
		bridge = f
		return f.buildLive(), nil
	}
	as := makeBareAgentSession(t, "claudecode", "/tmp")
	if err := as.Spawn(t.Context(), spawner); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { as.Shutdown() })
	first := bridge

	replies := make(chan string, 4)
	as.EventBus.Subscribe(func(ev EnrichedEvent) bool {
		if ev.Kind == KindAgentEvent && ev.AgentEvent != nil && ev.AgentEvent.Text != "" {
			select {
			case replies <- ev.AgentEvent.Text:
			default:
			}
		}
		return false
	})

	// The bridge dies: its events channel closes, the readpump exits
	// and reports the death.
	deaths := make(chan struct{}, 1)
	as.EventBus.Subscribe(func(ev EnrichedEvent) bool {
		if ev.Kind == KindLifecycle && ev.Lifecycle != nil && ev.Lifecycle.Status == StatusExited {
			select {
			case deaths <- struct{}{}:
			default:
			}
		}
		return false
	})
	first.Close()
	select {
	case <-deaths:
	case <-time.After(3 * time.Second):
		t.Fatal("the bridge death never reached the bus")
	}

	// The chat layer rebuilds the session on the death.
	if err := as.RestartFromDeath(t.Context(), spawner); err != nil {
		t.Fatalf("RestartFromDeath: %v", err)
	}
	if bridge == first {
		t.Fatal("the respawn reused the dead bridge")
	}

	bridge.PushEvent(agent.AgentEvent{Kind: agent.EventAgentText, Text: "from the new CLI"})

	select {
	case got := <-replies:
		if got != "from the new CLI" {
			t.Errorf("event text = %q, want %q", got, "from the new CLI")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the replacement bridge's events never reached the bus")
	}
}

// TestShutdown_LeavesNoReaderForALateRespawn pins the invariant
// Shutdown rests on: the eventQueue is closed there, and the
// readpump is its only writer — so no readpump may start once the
// shutdown has run, and a respawn that lands after it must not
// install a bridge.
func TestShutdown_LeavesNoReaderForALateRespawn(t *testing.T) {
	spawner := newFakeSpawner()
	as := makeBareAgentSession(t, "claudecode", "/tmp")
	if err := as.Spawn(t.Context(), spawner); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	as.Shutdown()

	// No readpump may take the slot of the one that just exited.
	as.startReadPump()
	as.asMu.RLock()
	started := as.readpumpStarted
	as.asMu.RUnlock()
	if started {
		t.Error("a readpump started after Shutdown; it would push into the closed eventQueue")
	}

	// A respawn that lands after the shutdown must not spawn either.
	before := spawnerCalls(spawner)
	if err := as.RestartFromDeath(t.Context(), spawner); err != nil {
		t.Fatalf("RestartFromDeath: %v", err)
	}
	if got := spawnerCalls(spawner); got != before {
		t.Errorf("spawn calls = %d, want %d (no bridge after Shutdown)", got, before)
	}
}

// spawnerCalls reads the spawn counter under the spawner's lock —
// the respawn runs on the event dispatcher goroutine.
func spawnerCalls(s *fakeSpawner) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}
