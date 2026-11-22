//go:build !windows

// The liveness probe against the real codex app-server.
//
// The unit test pins the JSON-RPC shape; this test pins the whole
// path — the real app-server, the real request, the bridge's own
// probe.
//
// The test is opt-in (NIGHTME_REAL_CODEX=1) because it spawns the
// user's CLI and needs it on PATH.

package codex

import (
	"context"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestPing_RealCodex — the probe against the real app-server.
func TestPing_RealCodex(t *testing.T) {
	requireRealCodex(t)

	s := NewStarter("codex", "codex", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
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
	t.Logf("probe frame: {\"jsonrpc\":\"2.0\",\"id\":<n>,\"method\":\"getAuthStatus\",\"params\":{}}")
	t.Logf("probe round trip: %s", time.Since(start))
}
