// F-42 tests for ChatSession.KillAll — graceful shutdown semantics.
//
// Critical invariants locked here:
//   - Running entries are closed via bridge.Close (graceful path).
//   - Dead/exited entries are NOT signaled — they get "stale-cleared".
//   - InputBuffer is preserved across /kill (user messages survive).
//   - agent_sessions.json entries are deleted AFTER the process dies.
//   - cs.activeAS is cleared.
//   - The returned []KillResult reflects each entry's before-state +
//     action so the handler can render a per-row status.
package chatsession

import (
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/registry"
)

// closedSpy is a fakeAgentSession that records every Close() call and
// closes its events channel on Close so the bridge shutdown sequence
// resembles a real bridge.
type closedSpy struct {
	*fakeAgentSession
	closes atomic.Int32
}

func (c *closedSpy) Close() error {
	c.closes.Add(1)
	// Drain and close the events channel so ObserveClose would flip
	// the agent to StatusExited if it were running.
	close(c.fakeAgentSession.events)
	return nil
}

// TestKillAll_GracefulShutdown verifies that every running agent's
// Close() is invoked (the bridge then runs its own graceful path:
// stdin EOF + SIGINT + 2s + SIGKILL fallback).
func TestKillAll_GracefulShutdown(t *testing.T) {
	cs := New("chat-graceful", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}

	cs.WithPersistence(nil, nil) // no disk; testing in-memory only

	a1 := injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})
	a2 := injectAS(t, cs, "codex", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(2)})
	cs.mu.Lock()
	cs.activeAS = a1
	cs.mu.Unlock()

	results, err := cs.KillAll()
	if err != nil {
		t.Fatalf("KillAll err: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}

	// Each Running entry should have Close() called exactly once.
	close1 := a1.Handle().(*closedSpy).closes.Load()
	close2 := a2.Handle().(*closedSpy).closes.Load()
	if close1 != 1 || close2 != 1 {
		t.Errorf("each running agent should have Close() called once: a1=%d a2=%d", close1, close2)
	}

	// Each result should be "killed" with BeforeState Running.
	for _, r := range results {
		if r.BeforeState != StatusRunning {
			t.Errorf("BeforeState: want Running, got %s", r.BeforeState)
		}
		if r.Action != "killed" {
			t.Errorf("Action: want killed, got %s", r.Action)
		}
	}
}

// TestKillAll_InputBufferPreserved verifies that queued user messages
// survive /kill (user messages are not /kill's concern).
func TestKillAll_InputBufferPreserved(t *testing.T) {
	cs := New("chat-buf", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})

	cs.ensureBuffer()
	cs.inputBuffer.SetState(StateBusy)
	if err := cs.inputBuffer.Add([]agent.ContentBlock{{Type: agent.ContentText, Text: "msg"}}, "u1"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := cs.inputBuffer.Pending(); got == 0 {
		t.Fatalf("buffer should have a queued message pre-KillAll")
	}

	if _, err := cs.KillAll(); err != nil {
		t.Fatalf("KillAll: %v", err)
	}

	if got := cs.inputBuffer.Pending(); got != 1 {
		t.Errorf("InputBuffer should be preserved across /kill: want 1, got %d", got)
	}
}

// TestKillAll_AgentSessionEntriesDeleted verifies that the
// agent_sessions.json entries owned by this ChatSession are deleted
// after KillAll.
func TestKillAll_AgentSessionEntriesDeleted(t *testing.T) {
	cs := New("chat-disk", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}

	asFile, err := registry.OpenAgentSessionFile(filepath.Join(t.TempDir(), "agent_sessions.json"))
	if err != nil {
		t.Fatalf("OpenAgentSessionFile: %v", err)
	}
	cs.WithPersistence(nil, asFile)

	cs.mu.Lock()
	a := NewAgentSession(newAgentSessionID(), cs.ID, "cc", cwd, nil)
	a.handle = &closedSpy{fakeAgentSession: newFakeAgentSession(1)}
	a.SetRunning(1234)
	cs.pool[agentCwdKey{Agent: "cc", Cwd: cwd}] = a
	cs.activeAS = a
	cs.mu.Unlock()

	// Persist the entry so KillAll has something to delete.
	if err := asFile.Upsert(a.Entry()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if got := len(asFile.GetByChatPool(cs.ID)); got != 1 {
		t.Fatalf("want 1 entry pre-KillAll, got %d", got)
	}

	if _, err := cs.KillAll(); err != nil {
		t.Fatalf("KillAll: %v", err)
	}

	if got := len(asFile.GetByChatPool(cs.ID)); got != 0 {
		t.Errorf("agent_sessions.json entries should be deleted post-KillAll: got %d", got)
	}
}

// TestKillAll_ActiveASCleared verifies that the ChatSession's activeAS
// pointer is nil after /kill so the next inbound message goes through
// LookupActiveAgentSession → fresh spawn.
func TestKillAll_ActiveASCleared(t *testing.T) {
	cs := New("chat-active", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	a := injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})
	cs.mu.Lock()
	cs.activeAS = a
	cs.currentTurnUserMsgID = "u-1"
	cs.mu.Unlock()

	if _, err := cs.KillAll(); err != nil {
		t.Fatalf("KillAll: %v", err)
	}

	if got := cs.ActiveAgentSession(); got != nil {
		t.Errorf("activeAS should be nil, got %v", got)
	}
	if got := cs.currentTurnUserMsgID; got != "" {
		t.Errorf("currentTurnUserMsgID should be cleared, got %q", got)
	}
}

// TestKillAll_OnlyDeadEntries verifies that entries already in
// StatusExited/StatusDetached do NOT trigger Close() — they're
// reported as "stale-cleared" and the disk entry is deleted.
func TestKillAll_OnlyDeadEntries(t *testing.T) {
	cs := New("chat-dead", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	a := NewAgentSession(newAgentSessionID(), cs.ID, "cc", cwd, nil)
	a.SetExited(0)
	cs.mu.Lock()
	cs.pool[agentCwdKey{Agent: "cc", Cwd: cwd}] = a
	cs.mu.Unlock()

	results, err := cs.KillAll()
	if err != nil {
		t.Fatalf("KillAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].Action != "stale-cleared" {
		t.Errorf("Action: want stale-cleared, got %q", results[0].Action)
	}
	if results[0].BeforeState != StatusExited {
		t.Errorf("BeforeState: want StatusExited, got %s", results[0].BeforeState)
	}
}

// TestKillAll_MixedStates verifies that running + dead entries are
// each handled by their own path: running gets Close(), dead gets
// stale-cleared; the result slice has both rows.
func TestKillAll_MixedStates(t *testing.T) {
	cs := New("chat-mixed", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	live := injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})
	dead := NewAgentSession(newAgentSessionID(), cs.ID, "codex", cwd, nil)
	dead.SetExited(0)
	cs.mu.Lock()
	cs.pool[agentCwdKey{Agent: "codex", Cwd: cwd}] = dead
	cs.activeAS = live
	cs.mu.Unlock()

	results, err := cs.KillAll()
	if err != nil {
		t.Fatalf("KillAll: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}

	// Build a quick map by agent name for stable assertions.
	byName := map[string]KillResult{}
	for _, r := range results {
		byName[r.Agent] = r
	}

	if r, ok := byName["cc"]; !ok {
		t.Errorf("missing cc result")
	} else {
		if r.Action != "killed" {
			t.Errorf("cc.Action: want killed, got %q", r.Action)
		}
		if live.Handle().(*closedSpy).closes.Load() != 1 {
			t.Errorf("live agent Close() should have been called")
		}
	}

	if r, ok := byName["codex"]; !ok {
		t.Errorf("missing codex result")
	} else {
		if r.Action != "stale-cleared" {
			t.Errorf("codex.Action: want stale-cleared, got %q", r.Action)
		}
	}
}

// TestKillAll_EmptyPool is the simplest case: no entries at all.
// Result is empty, no error.
func TestKillAll_EmptyPool(t *testing.T) {
	cs := New("chat-empty", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	results, err := cs.KillAll()
	if err != nil {
		t.Fatalf("KillAll: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("want empty results, got %d", len(results))
	}
}
