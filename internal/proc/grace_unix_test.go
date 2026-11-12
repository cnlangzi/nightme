//go:build !windows

package proc

import (
	"context"
	"testing"
	"time"
)

// TestGrace_DefaultIsSIGTERMGrace pins the shared default: a spawn
// that does not ask for a specific window gets SIGTERMGrace. The
// short-lived helper commands (the git probes, `copilot --help`,
// `opencode debug config`) all rely on this.
//
// The child ignores SIGTERM, so the only thing that can end it is
// the SIGKILL that fires after the grace window — which makes the
// elapsed time a direct readout of the effective grace.
func TestGrace_DefaultIsSIGTERMGrace(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cmd := New(ctx, "/bin/sh", "-c", "trap '' TERM; sleep 600")
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run() = nil; want non-nil (child ignored SIGTERM, SIGKILL fired)")
		}
		elapsed := time.Since(start)
		if elapsed < SIGTERMGrace {
			t.Fatalf("Run() returned in %v, before the default grace %v elapsed", elapsed, SIGTERMGrace)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() didn't return within 5s of cancel")
	}
}

// TestGrace_ExplicitOverride pins the per-spawn grace: an agent CLI
// child asks for AgentGrace, and the SIGKILL lands after that
// window rather than after the shared default.
//
// The agent bridges use this so the CLI gets long enough to flush its
// own session state on SIGTERM — an outright kill loses the resume
// id the CLI persists.
func TestGrace_ExplicitOverride(t *testing.T) {
	const grace = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	cmd := NewWith(ctx, Options{HideWindow: true, Grace: grace}, "/bin/sh", "-c", "trap '' TERM; sleep 600")
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run() = nil; want non-nil (child ignored SIGTERM, SIGKILL fired)")
		}
		elapsed := time.Since(start)
		if elapsed < grace {
			t.Fatalf("Run() returned in %v, before the requested grace %v elapsed", elapsed, grace)
		}
		if elapsed > grace+2*time.Second {
			t.Fatalf("Run() returned in %v, way past the requested grace %v", elapsed, grace)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() didn't return within 5s of cancel")
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
