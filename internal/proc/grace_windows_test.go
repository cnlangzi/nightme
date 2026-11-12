//go:build windows

package proc

import (
	"context"
	"testing"
	"time"
)

// TestGrace_DefaultIsSIGTERMGrace pins the shared default: a spawn
// that does not ask for a specific window gets SIGTERMGrace, i.e.
// cmd.WaitDelay is SIGTERMGrace — stdlib waits that long for the
// child to exit naturally before hard-killing it.
func TestGrace_DefaultIsSIGTERMGrace(t *testing.T) {
	cmd := New(context.Background(), "cmd.exe", "/c", "exit", "0")
	if cmd.WaitDelay != SIGTERMGrace {
		t.Fatalf("cmd.WaitDelay = %v, want %v (SIGTERMGrace)", cmd.WaitDelay, SIGTERMGrace)
	}
}

// TestGrace_ExplicitOverride pins the per-spawn grace: an agent CLI
// child asks for AgentGrace, and the natural-exit window follows
// that request rather than the shared default.
func TestGrace_ExplicitOverride(t *testing.T) {
	const grace = 3 * time.Second

	cmd := NewWith(context.Background(), Options{HideWindow: true, Grace: grace}, "cmd.exe", "/c", "exit", "0")
	if cmd.WaitDelay != grace {
		t.Fatalf("cmd.WaitDelay = %v, want %v", cmd.WaitDelay, grace)
	}
	if cmd.Cancel == nil {
		t.Fatal("cmd.Cancel is nil; armGraceCancel did not run")
	}
}

// TestGrace_AgentGraceIsLongerThanDefault pins the relationship the
// agent bridges rely on: the agent window has to outlast the shared
// default, otherwise the override buys nothing.
func TestGrace_AgentGraceIsLongerThanDefault(t *testing.T) {
	if AgentGrace <= SIGTERMGrace {
		t.Fatalf("AgentGrace = %v, want more than SIGTERMGrace (%v)", AgentGrace, SIGTERMGrace)
	}
}
