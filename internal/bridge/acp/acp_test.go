package acp

import (
	"context"
	"errors"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

func TestAgentName(t *testing.T) {
	a := New("codex", "codex")
	if got := a.Name(); got != "codex" {
		t.Fatalf("Name() = %q, want codex", got)
	}
}

func TestAgentMode(t *testing.T) {
	a := New("codex", "codex")
	if got := a.Mode(); got != agent.ModeACP {
		t.Fatalf("Mode() = %s, want acp", got)
	}
}

// TestAgentDetectIsCheap confirms Detect does not spawn anything in
// v0.1 — it only reports ModeACP, the actual capability handshake
// arrives in v0.2.
func TestAgentDetectIsCheap(t *testing.T) {
	a := New("codex", "codex")
	if err := a.Detect(); err != nil {
		t.Fatalf("Detect() returned error: %v", err)
	}
}

// TestAgentStartNotImplemented confirms the v0.1 contract: Start
// returns ErrNotImplemented so the SessionManager surfaces a clear
// message to the user instead of half-working.
func TestAgentStartNotImplemented(t *testing.T) {
	a := New("codex", "codex")
	_, err := a.Start(context.Background(), agent.StartConfig{Workspace: t.TempDir()})
	if err == nil {
		t.Fatalf("Start returned nil error, want ErrNotImplemented")
	}
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Start error = %v, want ErrNotImplemented", err)
	}
}