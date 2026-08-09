// Tests for the kill workflow via ChatSession's lifecycle accessors.
//
// The /kill orchestration lives in internal/command/kill (separate
// package). It calls ChatSession.AgentSessionsInCwd + per-entry
// as.Close() + ChatSession.DropAgentSession in sequence. To avoid an
// import cycle (chatsession tests → command/kill → chatsession),
// these tests exercise the SAME workflow by calling the accessor +
// per-entry Close steps directly. The kill package's wrapper (5s
// timeout, fan-out goroutines, error collection) is tested
// separately in internal/command/kill/kill_test.go.
package chatsession

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cnlangzi/nightme/internal/registry"
)

// --- shared helpers ---------------------------------------------------

// closedSpy is a fakeAgentSession that records every Close() call and
// closes its events channel on Close so the bridge shutdown sequence
// resembles a real bridge.
type closedSpy struct {
	*fakeAgentSession
	closes atomic.Int32
}

func (c *closedSpy) Close() error {
	c.closes.Add(1)
	close(c.fakeAgentSession.events)
	return nil
}

// failingCloseAS is a fakeAgentSession whose Close() returns an error.
// Used to verify that per-entry Close failures propagate.
type failingCloseAS struct {
	*fakeAgentSession
	closeErr error
}

func (f *failingCloseAS) Close() error {
	close(f.fakeAgentSession.events)
	return f.closeErr
}

// =========================================================================
// Accessor: AgentSessionsInCwd
// =========================================================================

// TestAgentSessionsInCwd verifies the read-only snapshot accessor
// used by kill.KillAllAgents. Empty cwd → nil; non-matching cwd →
// nil; matching cwd → entries in undefined order.
func TestAgentSessionsInCwd(t *testing.T) {
	cs := New("chat-asic", "cc")
	cwd1 := t.TempDir()
	cwd2 := t.TempDir()
	cs.WithPersistence(nil, nil)

	a1 := injectAS(t, cs, "cc", cwd1, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})
	a2 := injectAS(t, cs, "codex", cwd1, &closedSpy{fakeAgentSession: newFakeAgentSession(2)})
	a3 := injectAS(t, cs, "cc", cwd2, &closedSpy{fakeAgentSession: newFakeAgentSession(3)})

	// Empty cwd → nil.
	if got := cs.AgentSessionsInCwd(""); got != nil {
		t.Errorf("empty cwd: want nil, got %v", got)
	}
	// Matching cwd → both entries (order unspecified).
	got := cs.AgentSessionsInCwd(cwd1)
	if len(got) != 2 {
		t.Fatalf("cwd1: want 2 entries, got %d", len(got))
	}
	seen := map[*AgentSession]bool{}
	for _, as := range got {
		seen[as] = true
	}
	if !seen[a1] || !seen[a2] {
		t.Errorf("cwd1: want a1+a2 in snapshot, got %v", got)
	}
	if seen[a3] {
		t.Errorf("cwd1: a3 (different cwd) must not appear")
	}
	// Non-matching cwd → empty (length 0; nil or [] both fine).
	if got := cs.AgentSessionsInCwd("/no/such/cwd"); len(got) != 0 {
		t.Errorf("non-matching cwd: want len=0, got %d entries", len(got))
	}
}

// TestAgentSessionsInCwd_DoesNotMutate — the snapshot accessor
// must NOT remove anything from the pool.
func TestAgentSessionsInCwd_DoesNotMutate(t *testing.T) {
	cs := New("chat-asic-mut", "cc")
	cwd := t.TempDir()
	cs.WithPersistence(nil, nil)

	injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})
	injectAS(t, cs, "codex", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(2)})

	pre := len(cs.Pool())
	_ = cs.AgentSessionsInCwd(cwd)
	_ = cs.AgentSessionsInCwd(cwd) // call twice
	if post := len(cs.Pool()); post != pre {
		t.Errorf("AgentSessionsInCwd mutated pool: pre=%d post=%d", pre, post)
	}
}

// =========================================================================
// Accessor: DropAgentSession
// =========================================================================

// TestDropAgentSession verifies the post-close cleanup primitive.
//   - Removes from pool
//   - Clears selectedAS iff it pointed to the entry
//   - Deletes agent_sessions.json row
//   - Preserves selectedAS when it pointed elsewhere
func TestDropAgentSession(t *testing.T) {
	cs := New("chat-drop", "cc")
	cwd := t.TempDir()

	asFile, err := registry.OpenAgentSessionFile(filepath.Join(t.TempDir(), "agent_sessions.json"))
	if err != nil {
		t.Fatalf("OpenAgentSessionFile: %v", err)
	}
	cs.WithPersistence(nil, asFile)

	target := injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})
	sibling := injectAS(t, cs, "codex", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(2)})
	if err := asFile.Upsert(target.Entry()); err != nil {
		t.Fatalf("target Upsert: %v", err)
	}
	if err := asFile.Upsert(sibling.Entry()); err != nil {
		t.Fatalf("sibling Upsert: %v", err)
	}

	cs.mu.Lock()
	cs.selectedAS = sibling
	cs.mu.Unlock()

	cs.DropAgentSession(target)

	if got := cs.AgentSessionsInCwd(cwd); len(got) != 1 || got[0] != sibling {
		t.Errorf("post-drop: want pool=[sibling], got %v entries", got)
	}
	if cs.SelectedAgentSession() != sibling {
		t.Errorf("selectedAS should remain sibling (target was not active)")
	}
	if n := len(asFile.GetByChatPool(cs.ID)); n != 1 {
		t.Errorf("registry: want 1 row (sibling), got %d", n)
	}
}

// TestDropAgentSession_ActiveASCleared — DropAgentSession clears
// selectedAS iff it pointed to the dropped entry.
func TestDropAgentSession_ActiveASCleared(t *testing.T) {
	cs := New("chat-drop-active", "cc")
	cwd := t.TempDir()
	cs.WithPersistence(nil, nil)

	a := injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})
	cs.mu.Lock()
	cs.selectedAS = a
	cs.mu.Unlock()

	cs.DropAgentSession(a)

	if cs.SelectedAgentSession() != nil {
		t.Errorf("selectedAS should be nil post-Drop of active entry")
	}
}

// TestDropAgentSession_Idempotent — calling twice is safe (no
// panic, no error). The second call is a no-op.
func TestDropAgentSession_Idempotent(t *testing.T) {
	cs := New("chat-drop-idem", "cc")
	cwd := t.TempDir()
	cs.WithPersistence(nil, nil)

	a := injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})

	cs.DropAgentSession(a)
	cs.DropAgentSession(a) // must not panic
	if got := cs.AgentSessionsInCwd(cwd); len(got) != 0 {
		t.Errorf("post-double-drop: want pool=[], got %v", got)
	}
}

// TestDropAgentSession_NilSafe — nil entry is a no-op (defense
// against call sites that may pass nil).
func TestDropAgentSession_NilSafe(t *testing.T) {
	cs := New("chat-drop-nil", "cc")
	cwd := t.TempDir()
	cs.WithPersistence(nil, nil)

	injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})

	cs.DropAgentSession(nil) // must not panic
	if got := cs.AgentSessionsInCwd(cwd); len(got) != 1 {
		t.Errorf("nil drop should be no-op: pool size=%d", len(got))
	}
}

// =========================================================================
// End-to-end kill workflow (via accessors, simulating kill package)
// =========================================================================

// The kill package does: snapshot → fan-out Close → per-entry Drop.
// These tests reproduce the same workflow directly on ChatSession
// to verify the lifecycle primitives work as advertised.

// TestKillWorkflow_KillOne — the /kill <agent> path simulated.
// Find (agent, cwd) via LookupInPool, Close, Drop. Other entries
// must not be touched.
func TestKillWorkflow_KillOne(t *testing.T) {
	cs := New("chat-wf-ka", "cc")
	cwd := t.TempDir()
	if err := cs.SetSelectedCwd(cwd); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	target := injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})
	otherCwd := t.TempDir()
	sibling := injectAS(t, cs, "cc", otherCwd, &closedSpy{fakeAgentSession: newFakeAgentSession(2)})
	otherAgent := injectAS(t, cs, "codex", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(3)})

	as, err := cs.LookupInPool("cc", cwd)
	if err != nil {
		t.Fatalf("LookupInPool: %v", err)
	}
	if err := as.Close(); err != nil {
		t.Fatalf("target Close: %v", err)
	}
	cs.DropAgentSession(as)

	if closes := target.Handle().(*closedSpy).closes.Load(); closes != 1 {
		t.Errorf("target Close(): want 1, got %d", closes)
	}
	if closes := sibling.Handle().(*closedSpy).closes.Load(); closes != 0 {
		t.Errorf("sibling (other cwd) Close(): want 0, got %d", closes)
	}
	if closes := otherAgent.Handle().(*closedSpy).closes.Load(); closes != 0 {
		t.Errorf("sibling (other agent) Close(): want 0, got %d", closes)
	}
	if len(cs.Pool()) != 2 {
		t.Errorf("pool size post-KillOne: want 2, got %d", len(cs.Pool()))
	}
}

// TestKillWorkflow_KillOne_ActiveASCleared — workflow handles
// selectedAS correctly: cleared iff killed entry was active.
func TestKillWorkflow_KillOne_ActiveASCleared(t *testing.T) {
	cs := New("chat-wf-ka-active", "cc")
	cwd := t.TempDir()
	if err := cs.SetSelectedCwd(cwd); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	a := injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})
	cs.mu.Lock()
	cs.selectedAS = a
	cs.mu.Unlock()

	as, err := cs.LookupInPool("cc", cwd)
	if err != nil {
		t.Fatalf("LookupInPool: %v", err)
	}
	_ = as.Close()
	cs.DropAgentSession(as)

	if got := cs.SelectedAgentSession(); got != nil {
		t.Errorf("selectedAS should be nil post-Drop of active entry, got %v", got)
	}
}

// TestKillWorkflow_KillOne_NotFound — when (agent, cwd) is not in
// the pool, LookupInPool returns ErrAgentNotFound. The workflow
// must NOT mutate anything.
func TestKillWorkflow_KillOne_NotFound(t *testing.T) {
	cs := New("chat-wf-ka-notfound", "cc")
	cwd := t.TempDir()
	if err := cs.SetSelectedCwd(cwd); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	otherCwd := t.TempDir()
	other := injectAS(t, cs, "cc", otherCwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})

	_, err := cs.LookupInPool("cc", cwd)
	if !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("want ErrAgentNotFound, got %v", err)
	}
	if closes := other.Handle().(*closedSpy).closes.Load(); closes != 0 {
		t.Errorf("other entry Close() should not have been called: got %d", closes)
	}
	if len(cs.Pool()) != 1 {
		t.Errorf("pool should be untouched: size=%d", len(cs.Pool()))
	}
}

// TestKillWorkflow_KillAllAgents — the /kill (no args) path
// simulated. AgentSessionsInCwd returns targets; Close each; Drop each.
func TestKillWorkflow_KillAllAgents(t *testing.T) {
	cs := New("chat-wf-kaa", "cc")
	cwd := t.TempDir()
	if err := cs.SetSelectedCwd(cwd); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	inCwd1 := injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})
	inCwd2 := injectAS(t, cs, "codex", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(2)})
	otherCwd := t.TempDir()
	outOfScope := injectAS(t, cs, "cc", otherCwd, &closedSpy{fakeAgentSession: newFakeAgentSession(3)})

	snapshot := cs.AgentSessionsInCwd(cwd)
	if len(snapshot) != 2 {
		t.Fatalf("snapshot: want 2, got %d", len(snapshot))
	}
	for _, as := range snapshot {
		_ = as.Close()
		cs.DropAgentSession(as)
	}

	if closes := inCwd1.Handle().(*closedSpy).closes.Load(); closes != 1 {
		t.Errorf("inCwd1 Close(): want 1, got %d", closes)
	}
	if closes := inCwd2.Handle().(*closedSpy).closes.Load(); closes != 1 {
		t.Errorf("inCwd2 Close(): want 1, got %d", closes)
	}
	if closes := outOfScope.Handle().(*closedSpy).closes.Load(); closes != 0 {
		t.Errorf("outOfScope Close(): want 0 (preserved), got %d", closes)
	}
	if len(cs.Pool()) != 1 {
		t.Errorf("pool size post-KillAllAgents: want 1, got %d", len(cs.Pool()))
	}
}

// TestKillWorkflow_KillAllAgents_EmptyCwd — AgentSessionsInCwd("")
// returns nil → workflow is a no-op.
func TestKillWorkflow_KillAllAgents_EmptyCwd(t *testing.T) {
	cs := New("chat-wf-kaa-emptycwd", "cc")
	cs.WithPersistence(nil, nil)

	a := injectAS(t, cs, "cc", "/some/other/cwd",
		&closedSpy{fakeAgentSession: newFakeAgentSession(1)})
	cs.mu.Lock()
	cs.selectedAS = a
	cs.mu.Unlock()

	snapshot := cs.AgentSessionsInCwd("")
	if snapshot != nil {
		t.Errorf("empty cwd: want nil, got %v", snapshot)
	}
	if closes := a.Handle().(*closedSpy).closes.Load(); closes != 0 {
		t.Errorf("entry in other cwd must NOT be killed: got %d closes", closes)
	}
	if cs.SelectedAgentSession() != a {
		t.Errorf("selectedAS must NOT be cleared when no kill happened")
	}
}

// TestKillWorkflow_StaleCleared — StatusExited entries: workflow
// detects no live handle → skips Close, just Drop.
func TestKillWorkflow_StaleCleared(t *testing.T) {
	cs := New("chat-wf-stale", "cc")
	cwd := t.TempDir()
	if err := cs.SetSelectedCwd(cwd); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}

	asFile, err := registry.OpenAgentSessionFile(filepath.Join(t.TempDir(), "agent_sessions.json"))
	if err != nil {
		t.Fatalf("OpenAgentSessionFile: %v", err)
	}
	cs.WithPersistence(nil, asFile)

	a := NewAgentSession(newAgentSessionID(), cs.ID, "cc", cwd, nil)
	a.SetExited(0)
	cs.mu.Lock()
	cs.pool[agentCwdKey{Agent: "cc", Cwd: cwd}] = a
	cs.mu.Unlock()
	if err := asFile.Upsert(a.Entry()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Workflow: isAlive=false (StatusExited, no handle) → skip
	// Close, just Drop.
	as, err := cs.LookupInPool("cc", cwd)
	if err != nil {
		t.Fatalf("LookupInPool: %v", err)
	}
	isAlive := as.Status() == StatusRunning ||
		(as.Status() == StatusDetached && as.Handle() != nil)
	if isAlive {
		t.Errorf("StatusExited with no handle should NOT be alive")
	}
	cs.DropAgentSession(as)

	if len(cs.Pool()) != 0 {
		t.Errorf("pool should be empty after stale-clear, got %d", len(cs.Pool()))
	}
	if n := len(asFile.GetByChatPool(cs.ID)); n != 0 {
		t.Errorf("registry row should be deleted, got %d entries", n)
	}
	_ = context.Background() // keep context import
}