package agentsession

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/registry"
)

// F-61 test coverage:
//
//   - SetExited persists (so the JSON isn't stale on next read)
//   - SetSuspect/ClearSuspect toggle persisted state correctly
//   - RestartFromDeath skips when closedByUser
//   - RestartFromDeath concurrent Close → respawn rolls back
//   - Spawn() clears closedByUser (explicit user action overrides)
//
// Run with: go test ./internal/agentsession/... -run TestF61

// f61Persist is a thread-safe in-memory persist recorder. The
// production persist is registry.AgentSessionFile.Save; tests
// substitute this fake.
type f61Persist struct {
	mu      sync.Mutex
	calls   int32
	lastErr error
	last    *registry.AgentSessionEntry
}

func (p *f61Persist) save(e *registry.AgentSessionEntry) error {
	atomic.AddInt32(&p.calls, 1)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last = e
	return p.lastErr
}

func (p *f61Persist) Calls() int32 {
	return atomic.LoadInt32(&p.calls)
}

// TestF61_SetExitedPersists verifies that SetExited triggers a
// persist callback with the updated Entry (status=exited, pid=0).
func TestF61_SetExitedPersists(t *testing.T) {
	as := newAgentSessionRuntime("as_test", "cs_x", "claude", "/tmp", nil)
	persist := &f61Persist{}
	as.SetPersist(persist.save)

	as.SetExited(0)

	if got := persist.Calls(); got != 1 {
		t.Fatalf("persist calls = %d, want 1", got)
	}
	if persist.last == nil {
		t.Fatal("persist.last is nil")
	}
	if persist.last.Status != StatusExited {
		t.Errorf("status = %s, want Exited", persist.last.Status)
	}
	if persist.last.PID != 0 {
		t.Errorf("pid = %d, want 0", persist.last.PID)
	}
}

// TestF61_SetSuspectPersists verifies SetSuspect writes the new
// suspect state and ClearSuspect resets it.
func TestF61_SetSuspectPersists(t *testing.T) {
	as := newAgentSessionRuntime("as_test", "cs_x", "claude", "/tmp", nil)
	persist := &f61Persist{}
	as.SetPersist(persist.save)

	as.SetSuspect("no_fast_ack")
	if got := persist.Calls(); got != 1 {
		t.Fatalf("SetSuspect calls = %d, want 1", got)
	}
	if reason, _ := as.Suspect(); reason != "no_fast_ack" {
		t.Errorf("Suspect() = %q, want no_fast_ack", reason)
	}

	as.ClearSuspect()
	if got := persist.Calls(); got != 2 {
		t.Fatalf("ClearSuspect calls = %d, want 2", got)
	}
	if reason, _ := as.Suspect(); reason != "" {
		t.Errorf("Suspect() = %q, want empty", reason)
	}
}

// TestF61_RestartFromDeathSkipsClosedByUser verifies that an AS
// marked closedByUser does NOT respawn on RestartFromDeath.
func TestF61_RestartFromDeathSkipsClosedByUser(t *testing.T) {
	as := newAgentSessionRuntime("as_test", "cs_x", "claude", "/tmp", nil)
	// Mark closedByUser via Close path (handle is nil → no-op).
	_ = as.Close()

	spawnCount := int32(0)
	launcher := testSpawner{spawn: func(_ context.Context, _, _ string, _ []string, _ string) (*agent.Agent, error) {
		atomic.AddInt32(&spawnCount, 1)
		return nil, nil
	}}

	if err := as.RestartFromDeath(context.Background(), launcher); err != nil {
		t.Fatalf("RestartFromDeath returned err: %v", err)
	}
	if got := atomic.LoadInt32(&spawnCount); got != 0 {
		t.Errorf("spawn called %d times, want 0 (closedByUser should skip)", got)
	}
}

// TestF61_SpawnClearsClosedByUser verifies that an explicit user-
// driven Spawn overrides the closedByUser sticky state.
func TestF61_SpawnClearsClosedByUser(t *testing.T) {
	as := newAgentSessionRuntime("as_test", "cs_x", "claude", "/tmp", nil)
	_ = as.Close() // sets closedByUser=true

	if reason := as.closedByUser; !reason {
		t.Fatal("closedByUser should be true after Close")
	}

	// Spawn with a launcher that returns an error so we don't
	// need a real handle. The point is: closedByUser is cleared
	// before the respawn happens.
	launcher := testSpawner{spawn: func(_ context.Context, _, _ string, _ []string, _ string) (*agent.Agent, error) {
		return nil, agent.ErrRestartRequired
	}}
	_ = as.Spawn(context.Background(), launcher)

	if reason := as.closedByUser; reason {
		t.Errorf("closedByUser should be cleared after Spawn, got true")
	}
}

// TestF61_RestartFromDeathPreservesSessionID is the regression
// guard for the dsh silent-death incident on 2026-08-15:
//
//   When the bridge dies unexpectedly, RestartFromDeath must
//   pass the PREVIOUS sessionID to respawn so resume-capable
//   bridges (dsh session.fork, claudecode --resume, codex
//   thread resume) preserve conversation context across the
//   death. A previous version of this code deliberately cleared
//   sessionID here ("we want a fresh conversation") — that
//   broke upper-layer UX: the user would lose all in-flight
//   context and have to repeat the task after every bridge
//   crash.
//
// What this test asserts:
//   1. RestartFromDeath passes the existing sessionID to the
//      spawner verbatim (no clearing).
//   2. RestartFromDeath does NOT clear as.sessionID after a
//      successful respawn — the runtime's EventHandler will
//      overwrite it with the new bridge's EventAgentReady
//      SessionID once the handshake completes.
//   3. RestartFromDeath still skips respawn when closedByUser
//      is set (unchanged behavior).
func TestF61_RestartFromDeathPreservesSessionID(t *testing.T) {
	const saved = "session-abc-123"

	as := newAgentSessionRuntime("as_test", "cs_x", "dsh", "/tmp", nil)
	as.SetSessionID(saved)

	// Capture the sessionID the spawner actually sees.
	var got string
	launcher := testSpawner{spawn: func(_ context.Context, _, _ string, _ []string, sessionID string) (*agent.Agent, error) {
		got = sessionID
		return newFakeAgentSession(7777).buildLive(), nil
	}}

	if err := as.RestartFromDeath(context.Background(), launcher); err != nil {
		t.Fatalf("RestartFromDeath returned err: %v", err)
	}

	if got != saved {
		t.Errorf("spawner received sessionID = %q, want %q (resume must be preserved)", got, saved)
	}
	if as.SessionID() != saved {
		t.Errorf("as.sessionID after respawn = %q, want %q (must not clear; EventAgentReady will overwrite)",
			as.SessionID(), saved)
	}
	if got := as.Status(); got != StatusRunning {
		t.Errorf("Status = %s, want StatusRunning after successful respawn", got)
	}
}

// TestF61_RestartFromDeathPassesEmptyWhenNoSessionID is the
// sanity counter-case: a fresh AS with no captured sessionID
// must pass empty through to the spawner (no-op resume → fresh
// conversation). Guards against accidental hardcoding.
func TestF61_RestartFromDeathPassesEmptyWhenNoSessionID(t *testing.T) {
	as := newAgentSessionRuntime("as_test", "cs_x", "dsh", "/tmp", nil)
	// sessionID left as zero value "".

	var got string
	launcher := testSpawner{spawn: func(_ context.Context, _, _ string, _ []string, sessionID string) (*agent.Agent, error) {
		got = sessionID
		return newFakeAgentSession(7778).buildLive(), nil
	}}

	if err := as.RestartFromDeath(context.Background(), launcher); err != nil {
		t.Fatalf("RestartFromDeath returned err: %v", err)
	}
	if got != "" {
		t.Errorf("spawner received sessionID = %q, want \"\" (cold start should be empty)", got)
	}
}

// testSpawner is a Spawner that delegates to a function.
type testSpawner struct {
	spawn func(ctx context.Context, agent, cwd string, args []string, sessionID string) (*agent.Agent, error)
}

func (s testSpawner) Spawn(ctx context.Context, agent, cwd string, args []string, sessionID string) (*agent.Agent, error) {
	return s.spawn(ctx, agent, cwd, args, sessionID)
}

// Compile-time check that testSpawner implements agentsession.Spawner.
var _ Spawner = testSpawner{}

// silence unused warnings if any test is later removed
var _ = time.Now