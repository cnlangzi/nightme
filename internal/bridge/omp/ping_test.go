package omp

// The liveness probe.
//
// omp speaks the same RPC envelope as pi. The probe asks for
// get_state — the same command the boot handshake uses, which omp
// answers while a prompt is in flight and which never reaches the
// model's context.

import (
	"context"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/bridgestest"
)

// TestPing_FakeCLIAnswersWhileAPromptIsInFlight — the fake CLI
// ignores the prompt command, so the turn stays in flight; the
// probe's get_state is answered anyway.
func TestPing_FakeCLIAnswersWhileAPromptIsInFlight(t *testing.T) {
	bin := bridgestest.BuildFakeCLIT(t, "fake-omp")
	ag, err := bridgestest.StartFakeAgent(t, NewStarter("omp", "omp", nil), bin, "rpc")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ag.Close()

	// The turn goes out and stays unanswered: the fake CLI does not
	// answer a prompt.
	sent := make(chan error, 1)
	go func() {
		sent <- ag.SendBlocks(context.Background(), []agent.ContentBlock{
			{Type: agent.ContentText, Text: "hello"},
		})
	}()

	// Give the prompt a moment to reach the wire, so the probe
	// really is answered mid-turn.
	time.Sleep(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ag.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	// The turn is still in flight: the prompt was never answered.
	select {
	case err := <-sent:
		t.Fatalf("the prompt settled before the probe: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}
