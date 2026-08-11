// Tests for the close workflow via ChatSession's lifecycle accessors.
//
// The /close orchestration lives in internal/command/close (separate
// package). It calls ChatSession.AgentSessionsInCwd + per-entry
// as.Close() in sequence. /close does NOT call DropAgentSession —
// the AgentSession entry is preserved in the pool so the next
// user message can respawn via --resume <sessionID> and continue
// the conversation. To avoid an import cycle (chatsession tests →
// command/close → chatsession), these tests exercise the SAME
// workflow by calling the accessor + per-entry Close steps
// directly. The close package's wrapper (5s timeout, fan-out
// goroutines, error collection) is tested separately in
// internal/command/close/close_test.go.
//
// DropAgentSession itself is still tested below — it's a public
// accessor that callers may invoke directly (e.g. when daemon
// shutdown reaps a stale Detached entry).
package chatsession

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
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

// buildLive wraps c in a *agent.Agent with a closedSpyDriver
// so tests can type-assert Handle().Driver().(*closedSpyDriver)
// to inspect the recording.
func (c *closedSpy) buildLive() *agent.Agent {
	return agent.NewAgent(
		agent.NewInfo("spy", agent.ModePTY, "spy", nil, nil),
		c.pid, c.events,
		&closedSpyDriver{inner: c, closes: &c.closes})
}

// closedSpyDriver forwards driver calls to a closedSpy. Test code
// uses Handle().Driver().(*closedSpyDriver) to reach the recording.
type closedSpyDriver struct {
	inner *closedSpy
	closes *atomic.Int32
}

func (d *closedSpyDriver) SendBlocks(ctx context.Context, b []agent.ContentBlock) error {
	return d.inner.SendBlocks(ctx, b)
}
func (d *closedSpyDriver) SendPermission(resp string) error {
	return d.inner.SendPermission(resp)
}
func (d *closedSpyDriver) Reset(ctx context.Context) error { return d.inner.New(ctx) }
func (d *closedSpyDriver) Stop(ctx context.Context) error { return d.inner.Stop(ctx) }
func (d *closedSpyDriver) SetModel(ctx context.Context, providerID, modelID string) error {
	return d.inner.SetModel(ctx, providerID, modelID)
}
func (d *closedSpyDriver) Close() error                   { return d.inner.Close() }

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
// used by close.CloseAllAgents. Empty cwd → nil; non-matching cwd →
// nil; matching cwd → entries in undefined order.
func TestAgentSessionsInCwd(t *testing.T) {
	cs, _ := New("chat-asic", "cc", newTestChannel())
	cwd1 := t.TempDir()
	cwd2 := t.TempDir()
	cs.WithPersistence(nil, nil)

	a1 := injectAS(t, cs, "cc", cwd1, (&closedSpy{fakeAgentSession: newFakeAgentSession(1)}).buildLive())
	a2 := injectAS(t, cs, "codex", cwd1, (&closedSpy{fakeAgentSession: newFakeAgentSession(2)}).buildLive())
	a3 := injectAS(t, cs, "cc", cwd2, (&closedSpy{fakeAgentSession: newFakeAgentSession(3)}).buildLive())

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
	cs, _ := New("chat-asic-mut", "cc", newTestChannel())
	cwd := t.TempDir()
	cs.WithPersistence(nil, nil)

	injectAS(t, cs, "cc", cwd, (&closedSpy{fakeAgentSession: newFakeAgentSession(1)}).buildLive())
	injectAS(t, cs, "codex", cwd, (&closedSpy{fakeAgentSession: newFakeAgentSession(2)}).buildLive())

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
	cs, _ := New("chat-drop", "cc", newTestChannel())
	cwd := t.TempDir()

	asFile, err := registry.OpenAgentSessionFile(filepath.Join(t.TempDir(), "agent_sessions.json"))
	if err != nil {
		t.Fatalf("OpenAgentSessionFile: %v", err)
	}
	cs.WithPersistence(nil, asFile)

	target := injectAS(t, cs, "cc", cwd, (&closedSpy{fakeAgentSession: newFakeAgentSession(1)}).buildLive())
	sibling := injectAS(t, cs, "codex", cwd, (&closedSpy{fakeAgentSession: newFakeAgentSession(2)}).buildLive())
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
	cs, _ := New("chat-drop-active", "cc", newTestChannel())
	cwd := t.TempDir()
	cs.WithPersistence(nil, nil)

	a := injectAS(t, cs, "cc", cwd, (&closedSpy{fakeAgentSession: newFakeAgentSession(1)}).buildLive())
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
	cs, _ := New("chat-drop-idem", "cc", newTestChannel())
	cwd := t.TempDir()
	cs.WithPersistence(nil, nil)

	a := injectAS(t, cs, "cc", cwd, (&closedSpy{fakeAgentSession: newFakeAgentSession(1)}).buildLive())

	cs.DropAgentSession(a)
	cs.DropAgentSession(a) // must not panic
	if got := cs.AgentSessionsInCwd(cwd); len(got) != 0 {
		t.Errorf("post-double-drop: want pool=[], got %v", got)
	}
}

// TestDropAgentSession_NilSafe — nil entry is a no-op (defense
// against call sites that may pass nil).
func TestDropAgentSession_NilSafe(t *testing.T) {
	cs, _ := New("chat-drop-nil", "cc", newTestChannel())
	cwd := t.TempDir()
	cs.WithPersistence(nil, nil)

	injectAS(t, cs, "cc", cwd, (&closedSpy{fakeAgentSession: newFakeAgentSession(1)}).buildLive())

	cs.DropAgentSession(nil) // must not panic
	if got := cs.AgentSessionsInCwd(cwd); len(got) != 1 {
		t.Errorf("nil drop should be no-op: pool size=%d", len(got))
	}
}

// =========================================================================
// End-to-end close workflow (via accessors, simulating close package)
// =========================================================================

// The close package does: snapshot → fan-out Close only. It does
// NOT call DropAgentSession — the AgentSession entry is preserved.
// These tests reproduce the same workflow directly on ChatSession
// to verify the lifecycle primitives work as advertised.

// TestCloseWorkflow_CloseOne — the /close <agent> path simulated.
// Find (agent, cwd) via LookupInPool, Close. Other entries must
// not be touched. The AgentSession entry stays in the pool.
func TestCloseWorkflow_CloseOne(t *testing.T) {
	cs, _ := New("chat-wf-ka", "cc", newTestChannel())
	cwd := t.TempDir()
	if err := cs.SetSelectedCwd(cwd); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	target := injectAS(t, cs, "cc", cwd, (&closedSpy{fakeAgentSession: newFakeAgentSession(1)}).buildLive())
	otherCwd := t.TempDir()
	sibling := injectAS(t, cs, "cc", otherCwd, (&closedSpy{fakeAgentSession: newFakeAgentSession(2)}).buildLive())
	otherAgent := injectAS(t, cs, "codex", cwd, (&closedSpy{fakeAgentSession: newFakeAgentSession(3)}).buildLive())

	as, err := cs.LookupInPool("cc", cwd)
	if err != nil {
		t.Fatalf("LookupInPool: %v", err)
	}
	if err := as.Close(); err != nil {
		t.Fatalf("target Close: %v", err)
	}
	// No DropAgentSession — entry is preserved.

	if closes := target.Handle().Driver().(*closedSpyDriver).closes.Load(); closes != 1 {
		t.Errorf("target Close(): want 1, got %d", closes)
	}
	if closes := sibling.Handle().Driver().(*closedSpyDriver).closes.Load(); closes != 0 {
		t.Errorf("sibling (other cwd) Close(): want 0, got %d", closes)
	}
	if closes := otherAgent.Handle().Driver().(*closedSpyDriver).closes.Load(); closes != 0 {
		t.Errorf("sibling (other agent) Close(): want 0, got %d", closes)
	}
	// All 3 pool entries survive — /close only kills the bridge process.
	if len(cs.Pool()) != 3 {
		t.Errorf("pool size post-CloseOne: want 3 (sessions preserved), got %d", len(cs.Pool()))
	}
}

// TestCloseWorkflow_CloseOne_ActiveASPreserved — workflow does NOT
// clear selectedAS (only DropAgentSession would do that). The
// next user message would respawn via --resume on the same AS.
func TestCloseWorkflow_CloseOne_ActiveASPreserved(t *testing.T) {
	cs, _ := New("chat-wf-ka-active", "cc", newTestChannel())
	cwd := t.TempDir()
	if err := cs.SetSelectedCwd(cwd); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	a := injectAS(t, cs, "cc", cwd, (&closedSpy{fakeAgentSession: newFakeAgentSession(1)}).buildLive())
	cs.mu.Lock()
	cs.selectedAS = a
	cs.mu.Unlock()

	as, err := cs.LookupInPool("cc", cwd)
	if err != nil {
		t.Fatalf("LookupInPool: %v", err)
	}
	_ = as.Close()
	// No DropAgentSession.

	if got := cs.SelectedAgentSession(); got != a {
		t.Errorf("selectedAS should remain %v post-Close (no Drop), got %v", a, got)
	}
}

// TestCloseWorkflow_CloseOne_NotFound — when (agent, cwd) is not in
// the pool, LookupInPool returns ErrAgentNotFound. The workflow
// must NOT mutate anything.
func TestCloseWorkflow_CloseOne_NotFound(t *testing.T) {
	cs, _ := New("chat-wf-ka-notfound", "cc", newTestChannel())
	cwd := t.TempDir()
	if err := cs.SetSelectedCwd(cwd); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	otherCwd := t.TempDir()
	other := injectAS(t, cs, "cc", otherCwd, (&closedSpy{fakeAgentSession: newFakeAgentSession(1)}).buildLive())

	_, err := cs.LookupInPool("cc", cwd)
	if !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("want ErrAgentNotFound, got %v", err)
	}
	if closes := other.Handle().Driver().(*closedSpyDriver).closes.Load(); closes != 0 {
		t.Errorf("other entry Close() should not have been called: got %d", closes)
	}
	if len(cs.Pool()) != 1 {
		t.Errorf("pool should be untouched: size=%d", len(cs.Pool()))
	}
}

// TestCloseWorkflow_CloseAllAgents — the /close (no args) path
// simulated. AgentSessionsInCwd returns targets; Close each. No
// DropAgentSession — all entries preserved.
func TestCloseWorkflow_CloseAllAgents(t *testing.T) {
	cs, _ := New("chat-wf-kaa", "cc", newTestChannel())
	cwd := t.TempDir()
	if err := cs.SetSelectedCwd(cwd); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	inCwd1 := injectAS(t, cs, "cc", cwd, (&closedSpy{fakeAgentSession: newFakeAgentSession(1)}).buildLive())
	inCwd2 := injectAS(t, cs, "codex", cwd, (&closedSpy{fakeAgentSession: newFakeAgentSession(2)}).buildLive())
	otherCwd := t.TempDir()
	outOfScope := injectAS(t, cs, "cc", otherCwd, (&closedSpy{fakeAgentSession: newFakeAgentSession(3)}).buildLive())

	snapshot := cs.AgentSessionsInCwd(cwd)
	if len(snapshot) != 2 {
		t.Fatalf("snapshot: want 2, got %d", len(snapshot))
	}
	for _, as := range snapshot {
		_ = as.Close()
		// No DropAgentSession.
	}

	if closes := inCwd1.Handle().Driver().(*closedSpyDriver).closes.Load(); closes != 1 {
		t.Errorf("inCwd1 Close(): want 1, got %d", closes)
	}
	if closes := inCwd2.Handle().Driver().(*closedSpyDriver).closes.Load(); closes != 1 {
		t.Errorf("inCwd2 Close(): want 1, got %d", closes)
	}
	if closes := outOfScope.Handle().Driver().(*closedSpyDriver).closes.Load(); closes != 0 {
		t.Errorf("outOfScope Close(): want 0 (preserved), got %d", closes)
	}
	// All 3 pool entries survive.
	if len(cs.Pool()) != 3 {
		t.Errorf("pool size post-CloseAllAgents: want 3 (all preserved), got %d", len(cs.Pool()))
	}
}

// TestCloseWorkflow_CloseAllAgents_EmptyCwd — AgentSessionsInCwd("")
// returns nil → workflow is a no-op.
func TestCloseWorkflow_CloseAllAgents_EmptyCwd(t *testing.T) {
	cs, _ := New("chat-wf-kaa-emptycwd", "cc", newTestChannel())
	cs.WithPersistence(nil, nil)

	spy := &closedSpy{fakeAgentSession: newFakeAgentSession(1)}
	a := injectAS(t, cs, "cc", "/some/other/cwd", spy.buildLive())
	cs.mu.Lock()
	cs.selectedAS = a
	cs.mu.Unlock()

	snapshot := cs.AgentSessionsInCwd("")
	if snapshot != nil {
		t.Errorf("empty cwd: want nil, got %v", snapshot)
	}
	if closes := a.Handle().Driver().(*closedSpyDriver).closes.Load(); closes != 0 {
		t.Errorf("entry in other cwd must NOT be closed: got %d closes", closes)
	}
	if cs.SelectedAgentSession() != a {
		t.Errorf("selectedAS must NOT be cleared when no close happened")
	}
}

// TestCloseWorkflow_StaleCleared — StatusExited entries: the /close
// workflow detects no live handle and reports "stale-cleared" in
// the reply without calling Close. The AgentSession entry itself
// is preserved; this test verifies the liveness-detection branch.
//
// Separately, callers that want to fully reap a stale Detached
// entry (daemon shutdown, manual cleanup) can still call
// DropAgentSession directly — see TestDropAgentSession above.
func TestCloseWorkflow_StaleCleared(t *testing.T) {
	cs, _ := New("chat-wf-stale", "cc", newTestChannel())
	cwd := t.TempDir()
	if err := cs.SetSelectedCwd(cwd); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}

	cs.WithPersistence(nil, nil)

	a := NewAgentSession(newAgentSessionID(), cs.ID, "cc", cwd, nil)
	a.SetExited(0)
	cs.mu.Lock()
	cs.pool[agentCwdKey{Agent: "cc", Cwd: cwd}] = a
	cs.mu.Unlock()

	// Workflow: isAlive=false (StatusExited, no handle) → skip
	// Close, classify as stale-cleared. No DropAgentSession.
	as, err := cs.LookupInPool("cc", cwd)
	if err != nil {
		t.Fatalf("LookupInPool: %v", err)
	}
	isAlive := as.Status() == StatusRunning ||
		(as.Status() == StatusDetached && as.Handle() != nil)
	if isAlive {
		t.Errorf("StatusExited with no handle should NOT be alive")
	}

	// Pool entry survives /close — only the bridge process would
	// have been killed (it was already dead).
	if len(cs.Pool()) != 1 {
		t.Errorf("pool should still have the stale entry, got %d", len(cs.Pool()))
	}
	_ = context.Background() // keep context import
}