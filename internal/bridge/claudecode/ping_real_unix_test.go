//go:build !windows

// The liveness probe against the real Claude Code CLI.
//
// The unit tests pin the wire shape and the correlation; this test
// pins the whole path — the real binary, the real control channel,
// the bridge's own probe.
//
// The test is opt-in (NIGHTME_REAL_CLAUDE=1) because it spawns the
// user's CLI and needs it on PATH. It logs the round trip so the
// evidence travels with the test run.

package claudecode

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestPing_RealClaude — the probe against the real CLI.
func TestPing_RealClaude(t *testing.T) {
	if os.Getenv("NIGHTME_REAL_CLAUDE") == "" {
		t.Skip("NIGHTME_REAL_CLAUDE not set; skipping the real-CLI probe")
	}
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude not on PATH: %v", err)
	}

	s := NewStarter("claude", claudePath, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ag, err := s.Start(ctx, agent.StartConfig{
		Workspace:      t.TempDir(),
		PermissionMode: PermissionBypass,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = ag.Close() })

	start := time.Now()
	if err := ag.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	t.Logf("probe frame: {\"type\":\"control_request\",\"request\":{\"subtype\":\"get_usage\"},\"request_id\":\"nightme-probe-1\"}")
	t.Logf("probe round trip (cold CLI): %s", time.Since(start))

	// The second probe measures the control channel itself: the CLI
	// is booted by now, and the answer is the control-channel
	// latency the daemon's probe deadline is sized against.
	start = time.Now()
	if err := ag.Ping(ctx); err != nil {
		t.Fatalf("Ping (warm): %v", err)
	}
	t.Logf("probe round trip (warm CLI): %s", time.Since(start))
}
