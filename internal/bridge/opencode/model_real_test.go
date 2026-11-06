package opencode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/proc"
)

// TestE2E_ConfiguredModelReachesSession drives the real opencode
// binary: a workspace whose opencode.json selects a model must run
// the session on that model, while a workspace without a model
// configuration keeps opencode's own choice.
//
// The gate keeps the test opt-in — it spawns a real opencode and
// needs a model the local opencode knows.
func TestE2E_ConfiguredModelReachesSession(t *testing.T) {
	skipOpencodeE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	s := NewStarter("opencode", "opencode", []string{"acp"})
	ws := t.TempDir()

	// Without a model configuration the session runs on opencode's
	// own choice — the bridge intervenes in neither direction.
	own := startSession(t, ctx, s, ws).Model
	if own == "" {
		t.Fatal("session without a model configuration reported no model")
	}

	model := firstAvailableModel(t, ctx, ws)
	if model == "" {
		t.Skip("opencode lists no model this host can run")
	}
	if normalizeModel(model) == normalizeModel(own) {
		t.Skipf("opencode's own choice is already %q — nothing to prove", own)
	}
	if err := os.WriteFile(filepath.Join(ws, "opencode.json"), []byte(`{"model":"`+model+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got := startSession(t, ctx, s, ws).Model
	if normalizeModel(got) != normalizeModel(model) {
		t.Fatalf("session model = %q, want %q (opencode's own choice is %q)", got, model, own)
	}
}

// normalizeModel makes a model id comparable with the display name
// opencode reports for it — "github-copilot/Claude Fable 5" and
// "github-copilot/claude-fable-5" name the same model.
func normalizeModel(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), " ", "-")
}

// startSession opens an opencode session and returns the model the
// bridge reported for it.
func startSession(t *testing.T, ctx context.Context, s *Starter, ws string) agent.AgentEvent {
	t.Helper()
	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		a, err := s.Start(ctx, agent.StartConfig{Workspace: ws})
		if err != nil {
			last = err
			t.Logf("Start attempt %d: %v", attempt, err)
			time.Sleep(2 * time.Second)
			continue
		}
		defer a.Close()
		select {
		case ev := <-a.Events():
			if ev.Kind != agent.EventAgentReady {
				t.Fatalf("first event = %v, want EventAgentReady", ev.Kind)
			}
			return ev
		case <-time.After(60 * time.Second):
			t.Fatal("timeout waiting for EventAgentReady")
		}
	}
	t.Fatalf("Start: %v", last)
	return agent.AgentEvent{}
}

// firstAvailableModel asks opencode for a model this host can run.
// The test needs a model the session catalog knows — a configured
// model opencode rejects would leave the session on its own choice.
func firstAvailableModel(t *testing.T, ctx context.Context, ws string) string {
	t.Helper()
	cmd := proc.New(ctx, "opencode", "models")
	cmd.Dir = ws
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("opencode models: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && strings.Contains(line, "/") {
			return line
		}
	}
	return ""
}

func skipOpencodeE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("NIGHTME_OPENCODE_E2E") != "1" {
		t.Skip("set NIGHTME_OPENCODE_E2E=1 to run the opencode e2e tests")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skipf("opencode binary not on PATH: %v", err)
	}
}
