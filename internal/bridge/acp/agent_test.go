package acp

import (
	"context"
	"runtime"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

func TestAgent_Name(t *testing.T) {
	a := NewStarter("codex", "codex", []string{"--acp"}, nil, 0, 0)
	if got := a.Info().Name; got != "codex" {
		t.Fatalf("Name() = %q, want codex", got)
	}
}

func TestAgent_Mode(t *testing.T) {
	a := NewStarter("codex", "codex", nil, nil, 0, 0)
	if got := a.Info().Mode; got != agent.ModeACP {
		t.Fatalf("Mode() = %s, want acp", got)
	}
}

func TestAgent_Detect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY backend is Unix-only")
	}
	if err := NewStarter("echo", "/bin/echo", nil, nil, 0, 0).Detect(); err != nil {
		t.Fatalf("Detect(/bin/echo) = %v", err)
	}
	if err := NewStarter("missing", "/no/such/acp-agent", nil, nil, 0, 0).Detect(); err == nil {
		t.Fatal("Detect(missing) = nil")
	}
}

func TestAgent_Start_RequiresBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY backend is Unix-only")
	}
	_, err := NewStarter("missing", "/no/such/acp-binary", nil, nil, 0, 0).Start(
		context.Background(), agent.StartConfig{Workspace: t.TempDir()})
	if err == nil {
		t.Fatal("Start on missing binary returned nil")
	}
}
