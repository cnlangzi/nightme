//go:build !windows

// The liveness probe against the real opencode CLI.
//
// opencode drives the generic acp bridge, so it inherits the acp
// probe: a JSON-RPC request for a method the agent does not
// implement, answered with a "method not found" error.
//
// The test is opt-in (NIGHTME_REAL_OPENCODE=1) because it spawns the
// user's CLI and needs it on PATH.

package opencode

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestPing_RealOpencode — the probe against the real CLI.
func TestPing_RealOpencode(t *testing.T) {
	if os.Getenv("NIGHTME_REAL_OPENCODE") == "" {
		t.Skip("NIGHTME_REAL_OPENCODE not set; skipping the real-CLI probe")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skipf("opencode not on PATH: %v", err)
	}

	s := NewStarter("opencode", "opencode", []string{"acp"})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	// opencode's session/new rejects a fresh, empty workspace, so
	// the probe runs against the shared temp directory.
	ag, err := s.Start(ctx, agent.StartConfig{Workspace: os.TempDir()})
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
