package agentsession

import (
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestReadPump_AgentErrorEndsThePromptWithError pins the second
// half of the fatal-stderr contract: the bridge reports the CLI's
// own failure as an EventAgentError, and the readpump turns that
// into the prompt-end the chat layer settles the turn on.
//
// The error text travels with the event, so the chat can show the
// CLI's line instead of a generic failure.
func TestReadPump_AgentErrorEndsThePromptWithError(t *testing.T) {
	spawner := newFakeSpawner()
	as := makeBareAgentSession(t, "claudecode", "/tmp")
	if err := as.Spawn(t.Context(), spawner); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { as.Shutdown() })

	p := &Prompt{ID: "p-1", LastMessageID: "m-1"}
	as.asMu.Lock()
	as.currentPrompt = p
	as.isReady.Store(false)
	as.asMu.Unlock()

	ended := make(chan PromptEndedChange, 1)
	as.EventBus.Subscribe(func(ev EnrichedEvent) bool {
		if ev.Kind == KindPromptEnded && ev.PromptEnded != nil {
			select {
			case ended <- *ev.PromptEnded:
			default:
			}
		}
		return false
	})

	const line = `[claude-code:unrecognized_model] {"model":"MiniMax-M3[1m]","query_source":"sdk"}`
	spawner.Get("claudecode", "/tmp").PushEvent(agent.AgentEvent{
		Kind: agent.EventAgentError,
		Err:  agent.FatalStderrError(line),
	})

	select {
	case change := <-ended:
		if change.EndReason != agent.PromptEndError {
			t.Errorf("EndReason = %v, want %v", change.EndReason, agent.PromptEndError)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the prompt never ended on the agent error")
	}

	if p.EndReason != agent.PromptEndError {
		t.Errorf("prompt EndReason = %v, want %v", p.EndReason, agent.PromptEndError)
	}
	if !p.EndReason.IsError() {
		t.Error("the prompt-end reason must count as an error")
	}
}
