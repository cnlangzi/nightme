package sdk

import (
	"context"
	"errors"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

func TestAgentName(t *testing.T) {
	a := New("claude", "claude-code")
	if got := a.Name(); got != "claude" {
		t.Fatalf("Name() = %q, want claude", got)
	}
}

func TestAgentMode(t *testing.T) {
	a := New("claude", "claude-code")
	if got := a.Mode(); got != agent.ModeSDK {
		t.Fatalf("Mode() = %s, want sdk", got)
	}
}

func TestAgentDetectIsNoop(t *testing.T) {
	a := New("claude", "claude-code")
	if err := a.Detect(); err != nil {
		t.Fatalf("Detect() returned error: %v", err)
	}
}

func TestAgentStartNotImplemented(t *testing.T) {
	a := New("claude", "claude-code")
	_, err := a.Start(context.Background(), agent.StartConfig{Workspace: t.TempDir()})
	if err == nil {
		t.Fatalf("Start returned nil error, want ErrNotImplemented")
	}
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Start error = %v, want ErrNotImplemented", err)
	}
}