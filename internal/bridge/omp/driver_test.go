package omp

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agent/procutil"
)

// TestDriverImplementsAgentDriver asserts against the real
// (unexported) agent.driver interface. agent.NewAgent's type
// parameter already enforces this at compile time for every
// bridge; this is the runtime counterpart, covering the case
// where the driver's dynamic type is what matters.
func TestDriverImplementsAgentDriver(t *testing.T) {
	if !agent.ImplementsDriver(&driver{}) {
		t.Fatal("*driver does not satisfy agent.driver")
	}
}

// TestKeepalive_LiveProcessIsHealthy covers the happy path: a
// bridge whose PID is alive and whose RPC session is open must
// not trigger recovery.
func TestKeepalive_LiveProcessIsHealthy(t *testing.T) {
	d := &driver{
		pid:    os.Getpid(),
		rpc:    newRPCClient(nopWriteCloser{}),
		events: make(chan agent.AgentEvent, 1),
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

// TestKeepalive_DeadProcessRecovers covers the recovery path: a
// dead PID must invoke onRecover exactly once and propagate its
// error.
func TestKeepalive_DeadProcessRecovers(t *testing.T) {
	d := &driver{
		pid:    deadPID(t),
		rpc:    newRPCClient(nopWriteCloser{}),
		events: make(chan agent.AgentEvent, 1),
	}
	called := 0
	wantErr := errors.New("respawn failed")
	err := d.Keepalive(context.Background(), func(context.Context) error {
		called++
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Keepalive() = %v, want %v", err, wantErr)
	}
	if called != 1 {
		t.Fatalf("onRecover called %d times, want 1", called)
	}
}

// TestKeepalive_DeadRPCSessionRecovers covers the "process
// lingered but the RPC session tore down" case. The PID probe
// alone reports the bridge as healthy; the closed RPC client is
// what makes it unusable.
func TestKeepalive_DeadRPCSessionRecovers(t *testing.T) {
	d := &driver{
		pid:    os.Getpid(),
		rpc:    newRPCClient(nopWriteCloser{}),
		events: make(chan agent.AgentEvent, 1),
	}
	d.rpc.failPending(io.EOF)
	called := false
	if err := d.Keepalive(context.Background(), func(context.Context) error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("Keepalive() = %v, want nil", err)
	}
	if !called {
		t.Fatal("Keepalive did not recover a bridge with a closed RPC session")
	}
}

// TestKeepalive_NoRecoveryCallback verifies the error path when
// the caller supplies no recovery callback.
func TestKeepalive_NoRecoveryCallback(t *testing.T) {
	d := &driver{
		pid:    deadPID(t),
		rpc:    newRPCClient(nopWriteCloser{}),
		events: make(chan agent.AgentEvent, 1),
	}
	if err := d.Keepalive(context.Background(), nil); err == nil {
		t.Fatal("Keepalive() = nil, want an error for a dead bridge with no callback")
	}
}

// deadPID returns a PID that is no longer running: a child
// process that has already exited and been reaped.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestNoSuchTestExists")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait helper process: %v", err)
	}
	if err := procutil.AlivePID(pid); err == nil {
		t.Fatalf("pid %d is unexpectedly alive", pid)
	}
	return pid
}

// nopWriteCloser is a stdin sink for the RPC client in tests.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }
