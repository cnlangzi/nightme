//go:build !windows

// The liveness probe against the real omp CLI.
//
// The unit test pins the wire shape; this test pins the whole path —
// the real binary, the real RPC envelope, the bridge's own probe.
//
// The test is opt-in (NIGHTME_REAL_OMP=1) because it spawns the
// user's CLI and needs it on PATH.

package omp

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestPing_RealOmp — the probe against the real CLI.
func TestPing_RealOmp(t *testing.T) {
	if os.Getenv("NIGHTME_REAL_OMP") == "" {
		t.Skip("NIGHTME_REAL_OMP not set; skipping the real-CLI probe")
	}
	ompPath, err := exec.LookPath("omp")
	if err != nil {
		t.Skipf("omp not on PATH: %v", err)
	}

	s := NewStarter("omp", ompPath, nil)
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
	t.Logf("probe frame: {\"id\":\"req-<n>\",\"type\":\"get_state\"}")
	t.Logf("probe round trip: %s", time.Since(start))
}
