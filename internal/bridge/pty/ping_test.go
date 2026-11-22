package pty

// The liveness probe.
//
// A PTY carries raw bytes with no protocol to ask, so the bridge
// answers with the most precise signal it has: the child's process
// liveness. It is a weaker answer than the protocol bridges give —
// a live process whose CLI stopped reading its stdin still passes
// the probe — but it is the only one a protocol-less transport can
// give, and it settles the case the daemon cannot see otherwise:
// the child is gone.

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent/procutil"
)

// pidTransport is a Transport whose only interesting answer is the
// child's PID: the probe reads the process liveness.
type pidTransport struct {
	fakeTransport
	pid int
}

func (t *pidTransport) PID() int { return t.pid }

// TestPing_ALiveChildAnswers — a child that is still running
// answers the probe.
func TestPing_ALiveChildAnswers(t *testing.T) {
	d := &driver{transport: &pidTransport{pid: os.Getpid()}}
	if err := d.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestPing_ReportsTheProcessAsGone — the probe reports the child as
// gone once it has exited.
func TestPing_ReportsTheProcessAsGone(t *testing.T) {
	d := &driver{transport: &pidTransport{pid: deadPID(t)}}
	if err := d.Ping(context.Background()); err == nil {
		t.Fatal("Ping() = nil, want an error for a child that has exited")
	}
}

// TestPing_WithoutATransportFails — a driver that was never started
// has nothing to probe.
func TestPing_WithoutATransportFails(t *testing.T) {
	d := &driver{}
	if err := d.Ping(context.Background()); err == nil {
		t.Fatal("Ping() = nil, want an error without a transport")
	}
}

// deadPID returns the pid of a child that has already exited.
//
// The reaped pid is not reserved. On Windows the process handle is
// closed inside cmd.Wait, which frees the pid for immediate reuse
// by any other process on the machine; a concurrent spawn can
// claim it before the probe below runs, and OpenProcess then
// succeeds on the new process. The liveness probe is right — the
// pid just no longer refers to our child. Retry the whole cycle
// when that happens.
func deadPID(t *testing.T) int {
	t.Helper()
	for range 10 {
		cmd := exec.Command(os.Args[0], "-test.run=TestNoSuchTestExists")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start helper process: %v", err)
		}
		pid := cmd.Process.Pid
		if err := cmd.Wait(); err != nil {
			t.Fatalf("wait helper process: %v", err)
		}
		if procutil.AlivePID(pid) == nil {
			continue
		}
		return pid
	}
	t.Fatalf("could not obtain a dead pid after 10 attempts")
	return 0
}
