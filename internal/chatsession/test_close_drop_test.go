package chatsession

import (
	"testing"
)

// TestEvictAgentSessionsInCwd_EmptyCwd asserts the
// fast-path: empty cwd arg returns (0, nil) without touching
// the pool.
func TestEvictAgentSessionsInCwd_EmptyCwd(t *testing.T) {
	cs, _ := New("chat-test", "test-agent")

	n, res := cs.EvictAgentSessionsInCwd("")
	if n != 0 || res != nil {
		t.Errorf("EvictAgentSessionsInCwd(\"\") = (%d, %v), want (0, nil)", n, res)
	}
}

// TestEvictAgentSessionsInCwd_EmptyPool asserts the
// no-entries case: pool is empty, helper returns (0, nil).
func TestEvictAgentSessionsInCwd_EmptyPool(t *testing.T) {
	cs, _ := New("chat-test", "test-agent")

	n, res := cs.EvictAgentSessionsInCwd("/nonexistent")
	if n != 0 || res != nil {
		t.Errorf("EvictAgentSessionsInCwd on empty pool = (%d, %v), want (0, nil)", n, res)
	}
}

// TestEvictAgentSessionsInCwd_ClosesAndDropsAllRunning
// seeds two running ASes at the same cwd, calls the helper,
// and asserts both bridge handles observed Close + both pool
// entries removed. Mirrors the contract /gtw close relies on:
//
//   - For every AS whose Cwd matches, run as.Close() (which
//     drives the bridge to terminate).
//   - Drop the AS from cs.pool (and asFile, if present).
//   - Return one DropResult per closed AS so the gtw path can
//     surface a count in the user-facing reply.
func TestEvictAgentSessionsInCwd_ClosesAndDropsAllRunning(t *testing.T) {
	cs, _ := New("chat-test", "test-agent")

	// Two running ASes at the same cwd. Different Agent names
	// so the (Agent, Cwd) pool key keeps them distinct.
	_, spy1 := seedBareRunningAS(t, cs, "first", "/wt-a")
	_, spy2 := seedBareRunningAS(t, cs, "second", "/wt-a")

	// Sanity: pool has both before.
	if n := len(cs.pool); n != 2 {
		t.Fatalf("setup: pool = %d, want 2", n)
	}

	n, res := cs.EvictAgentSessionsInCwd("/wt-a")

	// Total evicted count covers both ASes.
	if n != 2 {
		t.Errorf("evicted count = %d, want 2", n)
	}
	// Helper must also return one DropResult per alive (closed) AS.
	if len(res) != 2 {
		t.Fatalf("DropResult len = %d, want 2", len(res))
	}
	// Pool must be empty after.
	if n := len(cs.pool); n != 0 {
		t.Errorf("pool after = %d, want 0", n)
	}
	// Both bridges must have observed Close.
	assertClosed(t, spy1)
	assertClosed(t, spy2)
}

// TestEvictAgentSessionsInCwd_DropsStaleEntry seeds an AS
// in StatusExited (no live bridge). The helper must still drop
// it from the pool so the chat's view is consistent, but it
// does NOT count the entry in DropResult (no close actually
// fired — there was nothing to close).
func TestEvictAgentSessionsInCwd_DropsStaleEntry(t *testing.T) {
	cs, _ := New("chat-test", "test-agent")
	as := makeBareAgentSession(t, "stale-agent", "/wt-a")
	as.SetStatusForTest(StatusExited)
	// No handle — matches the production state where a
	// crashed-grep AS is left in the pool.
	cs.pool[agentCwdKey{Agent: as.Agent, Cwd: as.Cwd}] = as
	if n := len(cs.pool); n != 1 {
		t.Fatalf("setup: pool = %d, want 1", n)
	}

	n, res := cs.EvictAgentSessionsInCwd("/wt-a")
	// Stale entry was still evicted from the pool, so the
	// user-facing count must include it.
	if n != 1 {
		t.Errorf("evicted count = %d, want 1 (stale entry still counted)", n)
	}
	if len(res) != 0 {
		t.Errorf("DropResult len = %d, want 0 (stale entry didn't fire close)", len(res))
	}
	if n := len(cs.pool); n != 0 {
		t.Errorf("pool after = %d, want 0", n)
	}
}

// TestEvictAgentSessionsInCwd_OnlyMatchingCwd seeds
// ASes at two cwds and asserts only the matching cwd's pool
// is drained.
func TestEvictAgentSessionsInCwd_OnlyMatchingCwd(t *testing.T) {
	cs, _ := New("chat-test", "test-agent")
	asTarget, _ := seedBareRunningAS(t, cs, "target", "/wt-a")
	asOther, _ := seedBareRunningAS(t, cs, "other", "/wt-b")

	n, res := cs.EvictAgentSessionsInCwd("/wt-a")
	if n != 1 {
		t.Errorf("evicted count = %d, want 1", n)
	}
	if len(res) != 1 {
		t.Fatalf("DropResult len = %d, want 1", len(res))
	}
	if got := cs.pool[agentCwdKey{Agent: asTarget.Agent, Cwd: asTarget.Cwd}]; got != nil {
		t.Errorf("target AS still in pool after helper: %v", got)
	}
	if got := cs.pool[agentCwdKey{Agent: asOther.Agent, Cwd: asOther.Cwd}]; got == nil {
		t.Errorf("non-matching AS dropped unexpectedly")
	}
}

// seedBareRunningAS creates a fresh AgentSession at the given
// (name, cwd), wires a fake bridge handle, marks it Running,
// and inserts it into cs.pool. Returns the AS + the underlying
// fakeAgentSession so the test can assert as.Close() was
// invoked. The (Agent, Cwd) pool key keeps distinct names at
// the same cwd as separate pool entries — needed for the
// multi-entry eviction tests.
//
// Inlined after deleting newTestASWithFakeHandle: that helper
// had no callers outside this file (the FSM-transition tests
// referenced in its old docstring were themselves removed)
// and the withASName / withASCwd functional-options machinery
// existed for exactly one consumer.
func seedBareRunningAS(t *testing.T, cs *ChatSession, name, cwd string) (*AgentSession, *fakeAgentSession) {
	t.Helper()
	as := NewAgentSession(newAgentSessionID(), cs.ID, name, cwd, nil)
	spy := newFakeAgentSession(42)
	as.SetHandleForTest(spy.buildLive())
	as.SetStatusForTest(StatusRunning)
	as.SetPIDForTest(42)
	cs.mu.Lock()
	cs.pool[agentCwdKey{Agent: as.Agent, Cwd: as.Cwd}] = as
	cs.mu.Unlock()
	return as, spy
}

// assertClosed reads spy.closed under the fake's mutex. The
// `closed` field is unexported, so this helper lives in the
// chatsession package.
func assertClosed(t *testing.T, spy *fakeAgentSession) {
	t.Helper()
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if !spy.closed {
		t.Errorf("fakeAgentSession: expected closed=true, got closed=false")
	}
}
