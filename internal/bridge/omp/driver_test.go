package omp

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
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

// TestKeepalive_ConcurrentCallsRecoverOnce verifies the contract
// that concurrent Keepalive calls are serialised inside the
// bridge: a burst of Submits must not kick off parallel
// recoveries, and the caller does not coordinate.
//
// The recovery callback marks the bridge alive again, so a second
// caller that waits for the first recovery sees a healthy bridge
// and does not recover a second time.
func TestKeepalive_ConcurrentCallsRecoverOnce(t *testing.T) {
	d := &driver{
		pid:    deadPID(t),
		rpc:    newRPCClient(nopWriteCloser{}),
		events: make(chan agent.AgentEvent, 1),
	}

	var mu sync.Mutex
	calls := 0
	recover := func(context.Context) error {
		mu.Lock()
		calls++
		mu.Unlock()
		// The recovery replaces the bridge; mark the new one alive
		// so the next Keepalive sees a healthy session.
		d.pid = os.Getpid()
		return nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.Keepalive(context.Background(), recover); err != nil {
				t.Errorf("Keepalive() = %v, want nil", err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("onRecover called %d times, want 1", calls)
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
