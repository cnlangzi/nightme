//go:build !windows

package runtime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/pty"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/registry"
)

// wedgedPTYStarter spawns a PTY-hosted child that ignores SIGTERM.
//
// It is the worst case the shutdown has to survive: the PTY family's
// teardown waits out its own 5s grace before it escalates to
// SIGKILL, so a daemon that does not stay alive for that long
// abandons the kill timer and leaves the child running.
type wedgedPTYStarter struct {
	name string
	pid  int
}

func (s *wedgedPTYStarter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModePTY, "sh", nil, nil)
}
func (s *wedgedPTYStarter) Detect() error { return nil }
func (s *wedgedPTYStarter) Init(string)   {}
func (s *wedgedPTYStarter) Start(ctx context.Context, _ agent.StartConfig) (*agent.Agent, error) {
	transport, err := pty.NewTransport(ctx, os.TempDir(),
		"/bin/sh", []string{"-c", "trap '' TERM; sleep 60"}, nil, 80, 24)
	if err != nil {
		return nil, err
	}
	s.pid = transport.PID()

	events := make(chan agent.AgentEvent)
	go func() {
		defer close(events)
		buf := make([]byte, 256)
		for {
			if _, err := transport.Read(buf); err != nil {
				return
			}
		}
	}()
	return agent.NewAgent(s.Info(), s.pid, events, &shutdownDriver{}), nil
}
func (s *wedgedPTYStarter) RunOnce(context.Context, agent.StartConfig, []agent.ContentBlock, ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunResult{}, errors.New("wedgedPTYStarter: RunOnce not implemented")
}
func (s *wedgedPTYStarter) Review(context.Context, agent.StartConfig, ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunResult{}, errors.New("wedgedPTYStarter: Review not implemented")
}

// TestShutdownRunMulti_TerminatesAWedgedPTYChild is the regression
// test for the shutdown budget: the PTY family escalates to SIGKILL
// only after its own 5s grace, so the daemon has to stay alive that
// long. A shorter budget let the daemon exit first, which killed the
// pending SIGKILL timer and left the child running — an orphan the
// shutdown had promised to prevent.
func TestShutdownRunMulti_TerminatesAWedgedPTYChild(t *testing.T) {
	csFile, asFile := newWireTestStores(t)

	scope, cancelScope := context.WithCancel(context.Background())
	defer cancelScope()

	reg := agent.New()
	st := &wedgedPTYStarter{name: "wedged-pty-agent"}
	reg.Register(st)

	mgr := chatsession.NewManager().
		WithPrimaryAgent("wedged-pty-agent").
		WithSpawner(chatsession.NewRegistrySpawner(reg, scope)).
		WithAgentSessionPool(chatsession.NewAgentSessionPool()).
		WithPersistence(csFile, asFile)

	cs, err := mgr.GetOrCreate("oc_wedged", "wedged-pty-agent")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if err := cs.SetSelectedCwd(t.TempDir()); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if err := cs.SetSelectedAgent("wedged-pty-agent"); err != nil {
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

	// The daemon is about to exit. The child must already be gone —
	// a live child here means the shutdown abandoned its kill.
	//
	// The settle window is deliberately short: the PTY teardown
	// escalates to SIGKILL 5s after the scope was cancelled, so a
	// budget that expires earlier returns while the child is still
	// running, and the pending SIGKILL dies with the daemon.
	deadline := time.Now().Add(500 * time.Millisecond)
	for syscall.Kill(pid, syscall.Signal(0)) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("the PTY child pid %d was still alive when the shutdown returned", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}

	entry, ok := asFile.Get(as.ID)
	if !ok {
		t.Fatal("the agent session row is missing")
	}
	if entry.Status != registry.StatusExited {
		t.Errorf("Status = %q, want %q", entry.Status, registry.StatusExited)
	}
	if entry.ExitCode == nil || *entry.ExitCode != registry.ExitCodeTerminatedByNightme {
		t.Errorf("ExitCode = %v, want %d", entry.ExitCode, registry.ExitCodeTerminatedByNightme)
	}
}
