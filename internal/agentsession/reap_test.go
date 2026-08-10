package agentsession

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestReapOrphan_ZeroPID verifies reapOrphan is a no-op for unset /
// zero PIDs (e.g. a fresh AgentSession that has never spawned).
func TestReapOrphan_ZeroPID(t *testing.T) {
	if err := reapOrphan(0); err != nil {
		t.Fatalf("reapOrphan(0) = %v, want nil", err)
	}
	if err := reapOrphan(-1); err != nil {
		t.Fatalf("reapOrphan(-1) = %v, want nil", err)
	}
}

// TestReapOrphan_KillsRunningChild spawns /bin/sleep, hands the PID
// to reapOrphan, and asserts the child is reaped (Wait returns) within
// a short window.
//
// Note: after SIGKILL the child becomes a zombie until the parent
// reaps it via Wait(); during that window `kill(pid, 0)` returns
// success even though the process is dead. So we use Wait() as the
// ground-truth signal that the child actually exited.
func TestReapOrphan_KillsRunningChild(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("/bin/sleep not available: %v", err)
	}
	pid := cmd.Process.Pid

	// Start a goroutine to reap the child once it exits. reapOrphan
	// sends SIGKILL; the Wait should return promptly after that.
	waitErr := make(chan error, 1)
	go func() {
		_, err := cmd.Process.Wait()
		waitErr <- err
	}()

	if err := reapOrphan(pid); err != nil {
		t.Fatalf("reapOrphan(%d) = %v, want nil", pid, err)
	}

	select {
	case err := <-waitErr:
		// Wait returning is proof the child exited. The error
		// itself may be non-nil because the child was killed by
		// signal — that is the expected outcome.
		_ = err
	case <-time.After(2 * time.Second):
		t.Fatalf("child pid %d not reaped within 2s after reapOrphan", pid)
	}
}

// TestReapOrphan_ESRCHIsSuccess verifies that reapOrphan treats
// "no such process" (the normal case once SIGKILL has been
// delivered and the child reaped) as a successful end state.
func TestReapOrphan_ESRCHIsSuccess(t *testing.T) {
	// Pick a PID that almost certainly doesn't exist. PIDs cycle,
	// but at the moment of this call, an arbitrary high number is
	// vanishingly unlikely to be live.
	const unlikelyPID = 1 << 30
	if err := reapOrphan(unlikelyPID); err != nil {
		// On some kernels PIDs may wrap; if we got EPERM that means
		// the pid WAS live (owned by another user). Skip rather
		// than fail spuriously.
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("pid %d unexpectedly live (EPERM); skipping", unlikelyPID)
		}
		t.Fatalf("reapOrphan(unlikely) = %v, want nil", err)
	}
}