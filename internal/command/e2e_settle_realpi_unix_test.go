//go:build !windows

// e2e_settle_realpi_unix_test.go — the settled signal against a
// REAL agent.
//
// The stub-based tests in e2e_settle_test.go prove the timing
// contract deterministically. This test proves the same contract
// against a real bridge and a real model turn: the settled message
// must not go out while the agent is still working, and it must go
// out once the turn ends.
//
// Default: SKIP. Opt in with:
//
//	NIGHTME_REAL_PI=1 go test ./internal/command -run RealPi -v -count=1
//
// Cost: ~1 API call to pi's model and a minute of wall clock. Run
// deliberately; never part of CI.

package command_test

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	pibridge "github.com/cnlangzi/nightme/internal/bridge/pi"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command/queue"
	"github.com/cnlangzi/nightme/internal/messages"
)

// requireRealPi skips unless the real-pi smoke is opted into and the
// pi binary is on PATH. Mirrors the guard the gtw package uses.
func requireRealPi(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping real-pi test in -short mode (use -run RealPi to opt in)")
	}
	switch os.Getenv("NIGHTME_REAL_PI") {
	case "", "0", "false", "no", "off":
		t.Skip("set NIGHTME_REAL_PI=1 to enable real-pi e2e tests")
	}
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skipf("pi binary not on PATH: %v", err)
	}
}

// realPiSpawner spawns the real pi bridge.
type realPiSpawner struct {
	starter agent.Starter
}

func (s *realPiSpawner) Spawn(ctx context.Context, _, cwd string, _ []string, _ string) (*agent.Agent, error) {
	return s.starter.Start(ctx, agent.StartConfig{Workspace: cwd})
}

// TestRealPi_SettledWaitsForTheRealTurn drives a real agent turn and
// checks the settled message lands when the turn ends — not before.
//
// The agent's terminal event is observed on the chat session's
// AgentEventBus; the settled message is emitted from the PromptEnd
// subscriber that fires on the prompt-end the same turn produces. If
// the settled went out early, the agent's terminal event would not
// have been observed yet when it arrived.
func TestRealPi_SettledWaitsForTheRealTurn(t *testing.T) {
	requireRealPi(t)

	reg := agent.New()
	reg.Register(pibridge.NewStarter("pi", "pi", nil))
	starter, err := reg.Get("pi")
	if err != nil {
		t.Fatalf("pi not registered: %v", err)
	}

	h := newSettleHarnessWithSpawner(t, &realPiSpawner{starter: starter}, queue.NewFactory())

	// The agent's terminal event, as the chat session sees it.
	var mu sync.Mutex
	agentTurnEnded := false
	h.cs.AgentEventBus.Subscribe(func(e chatsession.AgentEventEnvelope) bool {
		if e.Event == nil {
			return false
		}
		switch e.Event.Kind {
		case agent.EventAgentDone, agent.EventAgentResult, agent.EventAgentError:
			mu.Lock()
			agentTurnEnded = true
			mu.Unlock()
		}
		return false
	})

	h.drive(t, "/queue Reply with exactly one word: pong", "om_settle_realpi")

	got := h.waitSettledFor(t, "om_settle_realpi", 3*time.Minute)
	if got.Outcome == nil || got.Outcome.Status != messages.OutcomeOK {
		t.Fatalf("settled outcome = %+v, want ok", got.Outcome)
	}

	mu.Lock()
	ended := agentTurnEnded
	mu.Unlock()
	if !ended {
		t.Error("the settled message arrived before the agent's turn ended")
	}
}
