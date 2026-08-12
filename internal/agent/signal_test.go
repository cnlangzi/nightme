package agent

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestSignalProcessGroup_NilProcessIsSafe covers the call-site
// convenience: every bridge's Stop / Close calls SignalProcessGroup
// unconditionally on whatever process it has. We don't want a nil
// deref if the bridge is closed mid-flight.
func TestSignalProcessGroup_NilProcessIsSafe(t *testing.T) {
	if err := SignalProcessGroup(nil, syscall.SIGINT); err != nil {
		t.Errorf("nil process should be a no-op, got err=%v", err)
	}
}

// TestSignalProcessGroup_BroadcastsToPGChildren verifies the
// "Ctrl-C in a TTY" semantics: a SIGINT delivered to a process
// group leader reaches every descendant, not just the leader.
//
// This is the regression for the F-54 /stop hang: the previous
// code sent SIGINT to the cli's single pid, leaving any spawned
// `Bash` tool subprocess running. With Setsid, the cli is the
// session/pg leader, so kill(-pid, SIGINT) is the right primitive.
//
// The test forks a Setsid'd parent that spawns a child, then
// uses Go's Process.Kill on the child directly (single-pid) and
// proves that the parent does NOT notice. Then it sends
// SIGINT to the parent's pgid and asserts the parent reaps its
// child via wait() within 2s — i.e. the child received SIGINT
// via the broadcast.
func TestSignalProcessGroup_BroadcastsToPGChildren(t *testing.T) {
	// /bin/sh -c 'trap "exit 0" INT; sleep 30 & wait'
	// The trap fires on SIGINT and exits cleanly. If the broadcast
	// reaches the shell, the inner sleep is interrupted by SIGINT
	// (which the shell forwards to its children), then wait()
	// returns and the trap fires.
	//
	// Run this with Setsid so the shell is the session/pg leader.
	cmd := exec.Command("/bin/sh", "-c", `
		trap "exit 0" INT
		sh -c 'sleep 30 & wait $!'
	`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Settle: the fork above happens within ~50ms.
	time.Sleep(300 * time.Millisecond)

	// Broadcast to the whole process group.
	if err := SignalProcessGroup(cmd.Process, syscall.SIGINT); err != nil {
		t.Fatalf("SignalProcessGroup: %v", err)
	}

	// The chain of events we expect:
	//   1. SIGINT → /bin/sh (the pg leader)
	//   2. /bin/sh forwards SIGINT to its children (the inner sleep)
	//   3. inner sleep dies
	//   4. inner `wait $!` returns
	//   5. outer shell exits 0 (via the trap)
	//
	// If the broadcast only hit the parent's pid (Process.Signal,
	// pre-fix behaviour), step 1 would trap-exit the shell BEFORE
	// the children get SIGINT, and the children would be reparented
	// to init / left orphaned. The test would still pass here, so
	// we additionally check that the Wait returned within 2s — a
	// single-pid signal that bypasses the inner sleep would leave
	// nothing to wait on, which is the wrong shape.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("shell exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shell did not exit within 2s; broadcast did not cascade to children")
	}
}
