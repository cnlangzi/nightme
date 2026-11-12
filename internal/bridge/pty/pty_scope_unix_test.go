//go:build !windows

package pty

import (
	"context"
	"syscall"
	"testing"
	"time"
)

// pidAlive reports whether pid can still be signalled. Signal 0
// performs the existence + permission check without delivering
// anything.
//
// A child whose parent has not reaped it yet still answers signal 0
// as a zombie — so a "gone" answer here means the child was both
// killed AND reaped, which is exactly what the scope cancellation
// has to achieve.
func pidAlive(pid int) bool {
	return syscall.Kill(pid, syscall.Signal(0)) == nil
}

// TestTransport_ContextCancelTerminatesChild is the PTY half of the
// daemon's process scope: cancelling the context the transport was
// started with terminates the child process group and reaps it.
//
// Without this, a PTY-hosted agent (opencode / copilot / cursor)
// would outlive the daemon.
func TestTransport_ContextCancelTerminatesChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	b, err := NewTransport(ctx, t.TempDir(), "/bin/sleep", []string{"30"}, nil, 80, 24)
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	pid := b.PID()
	if pid == 0 {
		t.Fatal("PID() = 0, want the child's pid")
	}

	cancel()

	deadline := time.Now().Add(childExitGrace + 5*time.Second)
	for pidAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("child pid %d survived the context cancellation", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The watcher already tore the transport down; Close must be a
	// harmless no-op rather than an error.
	if err := b.Close(); err != nil {
		t.Errorf("Close() after the context was cancelled = %v, want nil", err)
	}
}

// TestTransport_CancelledContextRefusesToStart pins the guard that
// keeps the daemon's shutdown from producing a new agent process: a
// context that is already cancelled must not spawn a child.
func TestTransport_CancelledContextRefusesToStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := NewTransport(ctx, t.TempDir(), "/bin/sleep", []string{"30"}, nil, 80, 24); err == nil {
		t.Fatal("NewTransport with a cancelled context = nil error, want a refusal")
	}
}
