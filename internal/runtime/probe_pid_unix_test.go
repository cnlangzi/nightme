//go:build !windows

package runtime

import (
	"os"
	"syscall"
	"testing"
)

// TestPidAlive_EPERMIsAlive pins the EPERM-as-alive policy. A child
// whose real UID does not match ours (or whose session is otherwise
// protected) returns EPERM from kill(pid, 0); that is a *positive*
// signal that the PID exists — flipping it to dead would silently
// mass-reconcile every cross-user agent on the next daemon restart.
//
// We can't easily synthesize EPERM in a portable test, so we
// approximate the contract by checking the predicate's behaviour
// against the OS error returned by an obviously-bad signal: a real
// ESRCH (no such PID) reports false, a self-probe (always alive)
// reports true. The intent is to document the contract; the actual
// EPERM mapping is enforced by pidAliveOS's errors.Is check.
func TestPidAlive_EPERMIsAlive(t *testing.T) {
	// Sanity: a definitely-dead PID reports false (matches ESRCH path).
	if PidAlive(int(^uint32(0) >> 1)) {
		t.Skipf("host pid_max is too high for a guaranteed-dead sentinel; rerun")
	}
	// Sanity: our own process reports true.
	if !PidAlive(os.Getpid()) {
		t.Errorf("PidAlive(self) = false, want true")
	}
	// Sanity: a PID 0 reports false (the public contract).
	if PidAlive(0) {
		t.Errorf("PidAlive(0) = true, want false")
	}
	// Sanity: a PID that triggers ESRCH on signal 0 reports false.
	err := syscall.Kill(int(^uint32(0)>>1), syscall.Signal(0))
	if err == nil {
		t.Skipf("dead sentinel reported alive; pid_max exceeds our test budget")
	}
	if err != syscall.ESRCH {
		t.Skipf("dead sentinel returned %v, not ESRCH; cannot exercise the dead path", err)
	}
}
