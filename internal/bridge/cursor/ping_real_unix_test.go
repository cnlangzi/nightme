//go:build !windows

// The liveness probe against the real cursor-agent CLI.
//
// cursor drives the generic acp bridge, so it inherits the acp
// probe: a JSON-RPC request for a method the agent does not
// implement, answered with a "method not found" error.
//
// The test is opt-in (NIGHTME_REAL_CURSOR=1) because it spawns the
// user's CLI and needs it on PATH.

package cursor

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestPing_RealCursor — the probe against the real CLI.
func TestPing_RealCursor(t *testing.T) {
	if os.Getenv("NIGHTME_REAL_CURSOR") == "" {
		t.Skip("NIGHTME_REAL_CURSOR not set; skipping the real-CLI probe")
	}
	if _, err := exec.LookPath("cursor-agent"); err != nil {
		t.Skipf("cursor-agent not on PATH: %v", err)
	}

	s := NewStarter("cursor", "cursor-agent", DefaultACPArgs)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ag, err := s.Start(ctx, agent.StartConfig{Workspace: os.TempDir()})
	if err != nil {
		// The probe needs a live session, and session/new is where
		// cursor-agent asks for a login. An unauthenticated CLI
		// cannot open a session at all.
		if strings.Contains(err.Error(), "Authentication required") {
			t.Skipf("cursor-agent is not logged in: %v", err)
		}
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
