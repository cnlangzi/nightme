//go:build !windows

package runtime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"syscall"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/proc"
	"github.com/cnlangzi/nightme/internal/registry"
)

// shutdownStarter spawns a real child under the context it is
// handed — the same seam the agent bridges use — and closes the
// bridge's events channel when the child exits, which is how a
// bridge reports its death.
type shutdownStarter struct {
	name string
	pid  int
}

func (s *shutdownStarter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModePTY, "sleep", nil, nil)
}
func (s *shutdownStarter) Detect() error { return nil }
func (s *shutdownStarter) Init(string)   {}
func (s *shutdownStarter) Start(ctx context.Context, _ agent.StartConfig) (*agent.Agent, error) {
	cmd := proc.NewWith(ctx, proc.Options{HideWindow: true}, "sleep", "30")
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s.pid = cmd.Process.Pid
	events := make(chan agent.AgentEvent)
	go func() {
		_ = cmd.Wait()
		close(events)
	}()
	return agent.NewAgent(s.Info(), s.pid, events, &shutdownDriver{}), nil
}
func (s *shutdownStarter) RunOnce(context.Context, agent.StartConfig, []agent.ContentBlock, ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunResult{}, errors.New("shutdownStarter: RunOnce not implemented")
}
func (s *shutdownStarter) Review(context.Context, agent.StartConfig, ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunResult{}, errors.New("shutdownStarter: Review not implemented")
}

// shutdownDriver is the minimal agent.driver the wrapper needs.
type shutdownDriver struct{}

func (d *shutdownDriver) SendBlocks(context.Context, []agent.ContentBlock) error       { return nil }
func (d *shutdownDriver) SendPermission(string) error                                  { return nil }
func (d *shutdownDriver) Reset(context.Context) error                                  { return nil }
func (d *shutdownDriver) Close() error                                                 { return nil }
func (d *shutdownDriver) Stop(context.Context) error                                   { return nil }
func (d *shutdownDriver) Keepalive(context.Context, func(context.Context) error) error { return nil }

// TestShutdownRunMulti_TerminatesAgentChildren is the daemon's
// end-to-end shutdown contract: after the shutdown sequence, the
// agent child process is gone and agent_sessions.json records the
// session as exited with the daemon's termination code.
func TestShutdownRunMulti_TerminatesAgentChildren(t *testing.T) {
	csFile, asFile := newWireTestStores(t)

	scope, cancelScope := context.WithCancel(context.Background())
	defer cancelScope()

	reg := agent.New()
	st := &shutdownStarter{name: "sleep-agent"}
	reg.Register(st)

	mgr := chatsession.NewManager().
		WithPrimaryAgent("sleep-agent").
		WithSpawner(chatsession.NewRegistrySpawner(reg, scope)).
		WithAgentSessionPool(chatsession.NewAgentSessionPool()).
		WithPersistence(csFile, asFile)

	cs, err := mgr.GetOrCreate("oc_shutdown", "sleep-agent")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if err := cs.SetSelectedCwd(t.TempDir()); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if err := cs.SetSelectedAgent("sleep-agent"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}

	as, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatalf("LookupSelectedAgentSession: %v", err)
	}
	pid := st.pid
	if pid == 0 {
		t.Fatal("no agent child was spawned")
	}

	registerMgrInAllMgrs(mgr)
	t.Cleanup(func() { unregisterMgrFromAllMgrs(mgr) })

	if err := ShutdownRunMulti(
		io.Discard, nil, csFile, asFile, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		cancelScope,
	); err != nil {
		t.Fatalf("ShutdownRunMulti: %v", err)
	}

	// The agent child must be gone.
	deadline := time.Now().Add(proc.SIGTERMGrace + 5*time.Second)
	for syscall.Kill(pid, syscall.Signal(0)) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("agent child pid %d survived the shutdown", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !cs.IsShuttingDown() {
		t.Error("the chat was not marked as shutting down")
	}

	entry, ok := asFile.Get(as.ID)
	if !ok {
		t.Fatal("the agent session row is missing")
	}
	if entry.Status != registry.StatusExited {
		t.Errorf("Status = %q, want %q", entry.Status, registry.StatusExited)
	}
	if entry.ExitCode == nil {
		t.Fatal("ExitCode = nil, want the terminated code")
	}
	if *entry.ExitCode != registry.ExitCodeTerminatedByNightme {
		t.Errorf("ExitCode = %d, want %d", *entry.ExitCode, registry.ExitCodeTerminatedByNightme)
	}
	if entry.PID != 0 {
		t.Errorf("PID = %d, want 0", entry.PID)
	}
}
