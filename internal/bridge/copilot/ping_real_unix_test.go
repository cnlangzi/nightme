//go:build !windows

// The liveness probe against the real copilot CLI.
//
// copilot drives the generic acp bridge, so it inherits the acp
// probe: a JSON-RPC request for a method the agent does not
// implement, answered with a "method not found" error.
//
// The test is opt-in (NIGHTME_REAL_COPILOT=1) because it spawns the
// user's CLI and needs it on PATH.

package copilot

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestPing_RealCopilot — the probe against the real CLI.
func TestPing_RealCopilot(t *testing.T) {
	if os.Getenv("NIGHTME_REAL_COPILOT") == "" {
		t.Skip("NIGHTME_REAL_COPILOT not set; skipping the real-CLI probe")
	}
	if _, err := exec.LookPath("copilot"); err != nil {
		t.Skipf("copilot not on PATH: %v", err)
	}

	s := NewStarter("copilot", "copilot", DefaultACPArgs)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ag, err := s.Start(ctx, agent.StartConfig{Workspace: t.TempDir()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = ag.Close() })

	start := time.Now()
	if err := ag.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	t.Logf("probe frame: {\"jsonrpc\":\"2.0\",\"id\":<n>,\"method\":\"__nightme_probe__\",\"params\":{}}")
	t.Logf("probe round trip: %s", time.Since(start))
}
