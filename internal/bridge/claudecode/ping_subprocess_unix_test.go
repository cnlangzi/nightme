//go:build !windows

// The liveness probe against a fake CLI.
//
// The mock in internal/testdata answers control_request frames with
// a control_response that echoes the request_id — the same contract
// the real Claude Code CLI honours. The test drives the bridge's
// whole spawn path (pipes, handshake, stream pump) and asks the
// probe the question the daemon asks: is the CLI still answering?

package claudecode

import (
	"context"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestPing_FakeCLIAnswersWhileATurnIsInFlight — the probe is
// answered while the turn is still running: the control channel
// answers during a turn, and the answer never reaches the model's
// context.
func TestPing_FakeCLIAnswersWhileATurnIsInFlight(t *testing.T) {
	cmd, args := claudeMockCommand(t)

	a := NewStarter("mock-claude", cmd, args)
	sess, err := a.Start(context.Background(), agent.StartConfig{
		Workspace:      t.TempDir(),
		PermissionMode: PermissionBypass,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	// A turn is in flight: the mock answers the prompt, and the
	// bridge is still streaming the turn's events.
	if err := sess.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: "hello"},
	}); err != nil {
		t.Fatalf("SendBlocks: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sess.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}
