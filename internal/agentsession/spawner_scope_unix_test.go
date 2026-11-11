//go:build !windows

package agentsession

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/proc"
)

// scopeStarter spawns a real, long-lived child through the context it
// is handed — the same seam a real bridge uses.
type scopeStarter struct {
	pid int
}

func (s *scopeStarter) Info() agent.Info {
	return agent.NewInfo("scope-test", agent.ModePTY, "sleep", nil, nil)
}
func (s *scopeStarter) Detect() error { return nil }
func (s *scopeStarter) Init(string)   {}
func (s *scopeStarter) Start(ctx context.Context, _ agent.StartConfig) (*agent.Agent, error) {
	cmd := proc.NewWith(ctx, proc.Options{HideWindow: true}, "sleep", "30")
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s.pid = cmd.Process.Pid
	// Reap the child so the test observes a fully gone pid, not a
	// zombie that still answers signal 0.
	go func() { _ = cmd.Wait() }()
	return agent.NewAgent(s.Info(), s.pid, make(chan agent.AgentEvent), &scopeDriver{}), nil
}
func (s *scopeStarter) RunOnce(context.Context, agent.StartConfig, []agent.ContentBlock, ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunResult{}, errors.New("scopeStarter: RunOnce not implemented")
}
func (s *scopeStarter) Review(context.Context, agent.StartConfig, ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunResult{}, errors.New("scopeStarter: Review not implemented")
}

// scopeDriver is the minimal agent.driver the wrapper needs.
type scopeDriver struct{}

func (d *scopeDriver) SendBlocks(context.Context, []agent.ContentBlock) error       { return nil }
func (d *scopeDriver) SendPermission(string) error                                  { return nil }
func (d *scopeDriver) Reset(context.Context) error                                  { return nil }
func (d *scopeDriver) Close() error                                                 { return nil }
func (d *scopeDriver) Stop(context.Context) error                                   { return nil }
func (d *scopeDriver) Keepalive(context.Context, func(context.Context) error) error { return nil }

// waitForPIDGone waits until pid can no longer be signalled.
func waitForPIDGone(t *testing.T, pid int, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		if syscall.Kill(pid, syscall.Signal(0)) != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child pid %d survived the cancellation", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRegistrySpawner_ScopeCancelTerminatesChild is the core of the
// daemon's process scope: cancelling the scope terminates the agent
// child process the spawner started.
//
// Without it, every agent CLI the daemon spawns outlives the daemon.
func TestRegistrySpawner_ScopeCancelTerminatesChild(t *testing.T) {
	scope, cancelScope := context.WithCancel(context.Background())
	reg := agent.New()
	st := &scopeStarter{}
	reg.Register(st)

	handle, err := NewRegistrySpawner(reg, scope).
		Spawn(context.Background(), "scope-test", t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if handle == nil {
		t.Fatal("Spawn returned a nil handle")
	}
	pid := st.pid
	if pid == 0 {
		t.Fatal("no child pid recorded")
	}

	cancelScope()
	waitForPIDGone(t, pid, proc.SIGTERMGrace+5*time.Second)
}

// TestRegistrySpawner_CallerCancelTerminatesChild pins the other half
// of the "either one cancels" contract: the caller's context
// terminates the child too.
func TestRegistrySpawner_CallerCancelTerminatesChild(t *testing.T) {
	scope, cancelScope := context.WithCancel(context.Background())
	defer cancelScope()

	caller, cancelCaller := context.WithCancel(context.Background())
	reg := agent.New()
	st := &scopeStarter{}
	reg.Register(st)

	handle, err := NewRegistrySpawner(reg, scope).
		Spawn(caller, "scope-test", t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if handle == nil {
		t.Fatal("Spawn returned a nil handle")
	}
	pid := st.pid
	if pid == 0 {
		t.Fatal("no child pid recorded")
	}

	cancelCaller()
	waitForPIDGone(t, pid, proc.SIGTERMGrace+5*time.Second)
}

// TestRegistrySpawner_CancelledScopeRefusesToSpawn pins the guard
// that keeps the daemon's shutdown from producing a new agent
// process: with the scope already cancelled, the spawn must fail
// instead of starting a child.
func TestRegistrySpawner_CancelledScopeRefusesToSpawn(t *testing.T) {
	scope, cancelScope := context.WithCancel(context.Background())
	cancelScope()

	reg := agent.New()
	st := &scopeStarter{}
	reg.Register(st)

	handle, err := NewRegistrySpawner(reg, scope).
		Spawn(context.Background(), "scope-test", t.TempDir(), nil, "")
	if err == nil {
		t.Fatalf("Spawn with a cancelled scope = %v, want an error", handle)
	}
	if st.pid != 0 {
		t.Fatalf("a child process was started (pid %d) despite the cancelled scope", st.pid)
	}
}
