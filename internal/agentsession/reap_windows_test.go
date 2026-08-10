//go:build windows

package agentsession

import (
	"os/exec"
	"testing"
	"time"
)

// TestReapOrphan_ZeroPID_Unix mirrors the Unix test — verify
// reapOrphan is a no-op for unset / zero PIDs.
func TestReapOrphan_ZeroPID_Windows(t *testing.T) {
	if err := reapOrphan(0); err != nil {
		t.Fatalf("reapOrphan(0) = %v, want nil", err)
	}
	if err := reapOrphan(-1); err != nil {
		t.Fatalf("reapOrphan(-1) = %v, want nil", err)
	}
}

// TestReapOrphan_KillsRunningChild_Windows spawns `cmd.exe /c
// timeout ...` (a long sleep equivalent) and verifies that
// reapOrphan kills it within a short window.
//
// `cmd /c timeout` exits quickly when the timeout fires; using
// `ping -n 30 127.0.0.1 > nul` would also work and is more
// universally present on Windows.
func TestReapOrphan_KillsRunningChild_Windows(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "ping", "-n", "30", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Skipf("cmd.exe / ping not available: %v", err)
	}
	pid := cmd.Process.Pid

	waitErr := make(chan error, 1)
	go func() {
		_, err := cmd.Process.Wait()
		waitErr <- err
	}()

	if err := reapOrphan(pid); err != nil {
		t.Fatalf("reapOrphan(%d) = %v, want nil", pid, err)
	}

	select {
	case <-waitErr:
		// Child exited — proof reapOrphan worked. The error
		// itself may be non-nil because the process was
		// terminated externally.
	case <-time.After(5 * time.Second):
		t.Fatalf("child pid %d not reaped within 5s after reapOrphan", pid)
	}
}

// TestReapOrphan_NonExistentIsSuccess verifies the "process gone
// or unreachable" branch. A high PID that's almost certainly not
// in use returns nil from OpenProcess error path.
func TestReapOrphan_NonExistentIsSuccess(t *testing.T) {
	const unlikelyPID = 1 << 30
	if err := reapOrphan(unlikelyPID); err != nil {
		t.Fatalf("reapOrphan(unlikely) = %v, want nil", err)
	}
}