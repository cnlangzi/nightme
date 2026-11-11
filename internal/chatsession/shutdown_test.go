package chatsession

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/registry"
)

// countingSpawner counts Spawn calls, so a test can prove that no
// replacement process is created once the daemon is shutting down.
type countingSpawner struct{ calls int32 }

func (s *countingSpawner) Spawn(_ context.Context, _, _ string, _ []string, _ string) (*agent.Agent, error) {
	atomic.AddInt32(&s.calls, 1)
	return nil, errors.New("countingSpawner: not implemented")
}

// TestRouteEvent_ShutdownRecordsTerminatedExitCode pins the exit
// code the operator sees for a session the daemon terminated: -4,
// not 0.
//
// 0 would claim the CLI exited cleanly on its own; -4 says "nightme
// cleaned this up", which is the bit that distinguishes a daemon
// restart from a crash.
func TestRouteEvent_ShutdownRecordsTerminatedExitCode(t *testing.T) {
	cs := newChatSessionForTest("cs_test")
	as, _ := makeSpawnedAS(t, cs, "pi", context.Background())
	defer as.Shutdown()

	cs.MarkShuttingDown()
	cs.routeEvent(as, EnrichedEvent{
		Kind:      KindLifecycle,
		Lifecycle: &LifecycleChange{Status: StatusExited},
	})

	entry := as.Entry()
	if entry.Status != StatusExited {
		t.Errorf("Status = %q, want %q", entry.Status, StatusExited)
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

// TestRouteEvent_OwnDeathRecordsZeroExitCode pins the other side of
// the split: a CLI that died on its own records 0, so the two causes
// stay distinguishable in `nightme list`.
func TestRouteEvent_OwnDeathRecordsZeroExitCode(t *testing.T) {
	cs := newChatSessionForTest("cs_test")
	as, _ := makeSpawnedAS(t, cs, "pi", context.Background())
	defer as.Shutdown()

	cs.routeEvent(as, EnrichedEvent{
		Kind:      KindLifecycle,
		Lifecycle: &LifecycleChange{Status: StatusExited},
	})

	entry := as.Entry()
	if entry.ExitCode == nil {
		t.Fatal("ExitCode = nil, want 0")
	}
	if *entry.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", *entry.ExitCode)
	}
}

// TestRouteEvent_ShutdownDoesNotRespawn pins the shutdown guard: a
// bridge that dies while the daemon is tearing down must not be
// replaced by a new process.
//
// The process scope is already cancelled at that point, so the spawn
// could only fail — and a failed spawn would overwrite the exit code
// the operator needs.
func TestRouteEvent_ShutdownDoesNotRespawn(t *testing.T) {
	cs := newChatSessionForTest("cs_test")
	as, _ := makeSpawnedAS(t, cs, "pi", context.Background())
	defer as.Shutdown()

	spawner := &countingSpawner{}
	cs.spawner = spawner

	cs.MarkShuttingDown()
	cs.routeEvent(as, EnrichedEvent{
		Kind:      KindLifecycle,
		Lifecycle: &LifecycleChange{Status: StatusExited},
	})

	if got := atomic.LoadInt32(&spawner.calls); got != 0 {
		t.Errorf("Spawn was called %d times during shutdown, want 0", got)
	}
}
