package claudecode

import (
	"context"
	"os"
	"testing"
)

// TestKeepalive_ExitedChildRecovers covers the "child is gone but
// its PID has been recycled" case. The PID probe alone reports the
// bridge as healthy; the closed exitDone channel is what makes it
// unusable.
func TestKeepalive_ExitedChildRecovers(t *testing.T) {
	exitDone := make(chan struct{})
	close(exitDone)
	d := &driver{
		pid:      os.Getpid(),
		exitDone: exitDone,
	}

	called := false
	if err := d.Keepalive(context.Background(), func(context.Context) error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("Keepalive() = %v, want nil", err)
	}
	if !called {
		t.Fatal("Keepalive did not recover a bridge whose child exited")
	}
}

// TestKeepalive_LiveChildIsHealthy covers the happy path: a bridge
// whose PID is alive and whose child has not exited must not
// trigger recovery.
func TestKeepalive_LiveChildIsHealthy(t *testing.T) {
	d := &driver{
		pid:      os.Getpid(),
		exitDone: make(chan struct{}),
	}
	called := false
	if err := d.Keepalive(context.Background(), func(context.Context) error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("Keepalive() = %v, want nil", err)
	}
	if called {
		t.Fatal("Keepalive invoked recovery on a live bridge")
	}
}

// TestKeepalive_NoRecoveryCallback verifies the error path when
// the caller supplies no recovery callback.
func TestKeepalive_NoRecoveryCallback(t *testing.T) {
	exitDone := make(chan struct{})
	close(exitDone)
	d := &driver{
		pid:      os.Getpid(),
		exitDone: exitDone,
	}
	if err := d.Keepalive(context.Background(), nil); err == nil {
		t.Fatal("Keepalive() = nil, want an error for a dead bridge with no callback")
	}
}

// TestKeepalive_NoExitSignalDoesNotPanic pins the not-yet-started
// case: a driver without an exitDone channel has no death signal,
// so the PID probe decides — and it must not panic on the nil
// channel.
func TestKeepalive_NoExitSignalDoesNotPanic(t *testing.T) {
	d := &driver{pid: os.Getpid()}
	called := false
	if err := d.Keepalive(context.Background(), func(context.Context) error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("Keepalive() = %v, want nil", err)
	}
	if called {
		t.Fatal("Keepalive invoked recovery on a bridge with no death signal")
	}
}
