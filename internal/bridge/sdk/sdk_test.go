package sdk

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

func TestAgent_Name(t *testing.T) {
	a := New("claude", "/bin/echo")
	if got := a.Name(); got != "claude" {
		t.Fatalf("Name() = %q, want claude", got)
	}
}

func TestAgent_Mode(t *testing.T) {
	a := New("claude", "/bin/echo")
	if got := a.Mode(); got != agent.ModeSDK {
		t.Fatalf("Mode() = %s, want sdk", got)
	}
}

func TestAgent_Detect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a Unix binary path")
	}
	if err := New("claude", "/bin/echo").Detect(); err != nil {
		t.Fatalf("Detect(/bin/echo) = %v", err)
	}
	if err := New("claude", "/no/such/claude").Detect(); err == nil {
		t.Fatal("Detect(missing) = nil")
	}
}

func TestAgent_Start_NotImplemented(t *testing.T) {
	a := New("claude", "/bin/echo", []string{"--json"})
	_, err := a.Start(context.Background(), agent.StartConfig{Workspace: t.TempDir()})
	if err == nil {
		t.Fatal("Start() = nil error, want ErrNotImplemented")
	}
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Start() error = %v, want ErrNotImplemented", err)
	}
}

func TestAgent_NewKeepsLegacyConstructor(t *testing.T) {
	if got := New("claude", "claude-code").Name(); got != "claude" {
		t.Fatalf("legacy constructor Name() = %q", got)
	}
}

var _ agent.Agent = (*Agent)(nil)
