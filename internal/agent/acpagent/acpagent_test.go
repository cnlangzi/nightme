package acpagent

import (
	"context"
	"runtime"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

func TestAgent_Name(t *testing.T) {
	a := New("codex", "codex", []string{"--acp"})
	if got := a.Name(); got != "codex" {
		t.Fatalf("Name() = %q, want codex", got)
	}
}

func TestAgent_Mode(t *testing.T) {
	a := New("codex", "codex", nil)
	if got := a.Mode(); got != agent.ModeACP {
		t.Fatalf("Mode() = %s, want acp", got)
	}
}

func TestAgent_Detect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY backend is Unix-only")
	}
	if err := New("echo", "/bin/echo", nil).Detect(); err != nil {
		t.Fatalf("Detect(/bin/echo) = %v", err)
	}
	if err := New("missing", "/no/such/acp-agent", nil).Detect(); err == nil {
		t.Fatal("Detect(missing) = nil")
	}
}

func TestAgent_Start_RequiresBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY backend is Unix-only")
	}
	_, err := New("missing", "/no/such/acp-agent", nil).Start(context.Background(), agent.StartConfig{Workspace: t.TempDir()})
	if err == nil {
		t.Fatal("Start() = nil error for missing binary")
	}
}

func TestAgent_Start_CombinesArgs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY backend is Unix-only")
	}
	// /bin/echo exits before an ACP handshake, so this test only verifies
	// that a real PTY spawn is attempted and reports the protocol failure.
	_, err := New("echo", "/bin/echo", []string{"--acp"}).Start(context.Background(), agent.StartConfig{
		Workspace: t.TempDir(),
		Args:      []string{"extra"},
	})
	if err == nil {
		t.Fatal("Start() unexpectedly completed an ACP handshake with echo")
	}
}

var _ agent.Agent = (*Agent)(nil)
