// session_smoke_test.go — quick real dsh web smoke for chat session.
//
//go:build unix

package dsh

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestE2E_ChatSession_SpawnAndReady spawns a real dsh web process,
// waits for EventAgentReady, then closes. This is the canonical
// "does Start work?" smoke test — exercises spawn → URL parse →
// WS dial → session.create → EventAgentReady pipeline end-to-end.
//
// Gated by NIGHTME_REAL_DSH (same gate as the existing print-mode
// e2e tests). Requires `dsh` on PATH (npm install -g @deepseek-ai/dsh).
func TestE2E_ChatSession_SpawnAndReady(t *testing.T) {
	if _, err := exec.LookPath("dsh"); err != nil {
		t.Skipf("dsh not in PATH; skipping: %v", err)
	}

	s := NewStarter("dsh")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a, err := s.Start(ctx, agent.StartConfig{
		Workspace: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if a.PID() <= 0 {
		t.Errorf("PID = %d, want > 0", a.PID())
	}
	defer a.Close()

	// Drain events until we see EventAgentReady or timeout
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ready := false
	for {
		select {
		case ev, ok := <-a.Events():
			if !ok {
				t.Fatal("events channel closed before EventAgentReady")
			}
			t.Logf("event: kind=%s sessionID=%q model=%q", ev.Kind, ev.SessionID, ev.Model)
			if ev.Kind == agent.EventAgentReady {
				if ev.SessionID == "" {
					t.Error("EventAgentReady.SessionID is empty")
				}
				if ev.AgentName != "dsh" {
					t.Errorf("EventAgentReady.AgentName = %q, want dsh", ev.AgentName)
				}
				ready = true
				goto done
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for EventAgentReady")
		}
	}
done:
	if !ready {
		t.Fatal("never received EventAgentReady")
	}
}
