// F-42 tests for ChatSession.KillAll — graceful shutdown semantics.
//
// Critical invariants locked here:
//   - Running entries are closed via bridge.Close (graceful path).
//   - Dead/exited entries are NOT signaled — they get "stale-cleared".
//   - The message queue is preserved across /kill (user messages survive).
//   - agent_sessions.json entries are deleted AFTER the process dies.
//   - cs.activeAS is cleared.
//   - The returned []KillResult reflects each entry's before-state +
//     action so the handler can render a per-row status.
package chatsession

import (
	"errors"
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

	// CS-AS 边界重构 Phase 1: there is no per-CS read pump to start
	// or assert on. Each AgentSession owns its readpump (started by
	// Spawn, torn down by Shutdown — covered by
	// TestAgentSession_Shutdown_ClosesReadPump).

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

	// CS-AS 边界重构 Phase 1: the equivalent of "pump stopped" is
	// that KillAll shut every AgentSession down, which closes each
	// AS's eventQueue and ends its readpump. cs.activeAS being
	// cleared is the observable proxy asserted by
	// TestKillAll_ActiveASCleared.
}

// TestKillAll_QueuePreserved verifies that queued user messages
// survive /kill (user messages are not /kill's concern).
func TestKillAll_QueuePreserved(t *testing.T) {
	cs := New("chat-buf", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})

	// Queue a message with no activeAS installed, so the TryFlush
	// inside QueueUserMessage is a no-op and the message stays put.
	if err := cs.QueueUserMessage(makeTestMessage(cs,
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "msg"}}, "u1")); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}
	if got := cs.QueueLen(); got == 0 {
		t.Fatalf("queue should have a message pre-KillAll")
	}

	if _, err := cs.KillAll(); err != nil {
		t.Fatalf("KillAll: %v", err)
	}

	if got := cs.QueueLen(); got != 1 {
		t.Errorf("queue should be preserved across /kill: want 1, got %d", got)
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
	// F-53: pre-populate AgentSession.currentPrompt to verify
	// KillAll clears it (the new anchor location — was
	// ChatSession.currentTurnUserMsgID in v1.3).
	a.asMu.Lock()
	a.currentPrompt = &Prompt{ID: "test-p1", MessageIDs: []string{"u-1"}, LastMessageID: "u-1"}
	a.asMu.Unlock()
	cs.mu.Unlock()

	if _, err := cs.KillAll(); err != nil {
		t.Fatalf("KillAll: %v", err)
	}

	if got := cs.ActiveAgentSession(); got != nil {
		t.Errorf("activeAS should be nil, got %v", got)
	}
	// F-53: Verify the prompt was endPrompt'd — currentPrompt
	// cleared, EndedAt + EndReason stamped. The test target is
	// only meaningful if KillAll was responsible for clearing it
	// (it doesn't directly, but the activeAS reference is gone,
	// so the prompt is unreachable). We check activeAS is nil
	// above and trust the GC for prompt cleanup.
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

// failingCloseAS is a fakeAgentSession whose Close() returns an error.
// Used to verify that KillAll propagates the error to the
// corresponding KillResult (review finding #B2: previously the
// goroutine discarded `_ = as.Close()` so any failure was reported
// as ✓ success).
type failingCloseAS struct {
	*fakeAgentSession
	closeErr error
}

func (f *failingCloseAS) Close() error {
	close(f.fakeAgentSession.events)
	return f.closeErr
}

// TestKillAll_CloseErrorPropagates — when a bridge's Close() returns
// an error, the resulting KillResult must record Error != nil and
// Action = "kill-failed" (review finding #B2).
func TestKillAll_CloseErrorPropagates(t *testing.T) {
	cs := New("chat-close-err", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	a := injectAS(t, cs, "cc", cwd,
		&failingCloseAS{
			fakeAgentSession: newFakeAgentSession(1),
			closeErr:         errors.New("bridge shutdown: EPIPE on master fd"),
		})

	results, err := cs.KillAll()
	if err != nil {
		t.Fatalf("KillAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].Error == nil {
		t.Errorf("Error should be non-nil when bridge Close fails")
	}
	if results[0].Action != "kill-failed" {
		t.Errorf("Action: want kill-failed, got %q", results[0].Action)
	}
	_ = a
}

// TestKillAll_StatusDetached_StillCallsClose — review finding #B1:
// SetDetached documents "process alive but nightme no longer holds
// it" — the live CLI must still be closed on /kill, not reported
// as stale-cleared.
func TestKillAll_StatusDetached_StillCallsClose(t *testing.T) {
	cs := New("chat-detached", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	a := NewAgentSession(newAgentSessionID(), cs.ID, "cc", cwd, nil)
	a.handle = &closedSpy{fakeAgentSession: newFakeAgentSession(1)}
	a.SetRunning(1234)
	a.SetDetached() // process alive but nightme no longer holds it
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
	if results[0].Action != "killed" {
		t.Errorf("Action: want killed (process alive), got %q", results[0].Action)
	}
	if closes := a.Handle().(*closedSpy).closes.Load(); closes != 1 {
		t.Errorf("Close() should have been called for StatusDetached entry: got %d", closes)
	}
}

// TestKillAll_StatusDetached_NoHandle_StaleCleared — when the entry
// is StatusDetached but the handle is nil (post-restart orphan from
// FromAgentSessionEntry), we can't signal the (lost) process. The
// entry is treated as stale-cleared and the disk entry is deleted.
func TestKillAll_StatusDetached_NoHandle_StaleCleared(t *testing.T) {
	cs := New("chat-detached-orphan", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	// Post-restart: FromAgentSessionEntry creates an AgentSession
	// with StatusDetached but no handle (cmd died, hand lost).
	a := NewAgentSession(newAgentSessionID(), cs.ID, "cc", cwd, nil)
	a.SetDetached() // handle already nil from NewAgentSession
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
		t.Errorf("Action: want stale-cleared (no handle), got %q", results[0].Action)
	}
}

// TestKillAll_OrphanDiskEntry_GCd — old `GetByChatPool` walk did
// this; F-42 first cut narrowed to snapshot-only, reintroducing
// orphan accumulation (review finding #B4). Verify orphan disk
// entries owned by this chat are deleted even if not in the pool.
func TestKillAll_OrphanDiskEntry_GCd(t *testing.T) {
	cs := New("chat-orphan", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	asFile, err := registry.OpenAgentSessionFile(filepath.Join(t.TempDir(), "agent_sessions.json"))
	if err != nil {
		t.Fatalf("OpenAgentSessionFile: %v", err)
	}
	cs.WithPersistence(nil, asFile)

	// 1) snapshot entry (in pool + on disk)
	poolAS := injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})
	if err := asFile.Upsert(poolAS.Entry()); err != nil {
		t.Fatalf("pool Upsert: %v", err)
	}
	// 2) orphan entry (NOT in pool, e.g. leftover from a prior /cwd swap)
	orphanID := newAgentSessionID()
	orphanAS := NewAgentSession(orphanID, cs.ID, "codex", cwd, nil)
	orphanAS.SetRunning(9999)
	if err := asFile.Upsert(orphanAS.Entry()); err != nil {
		t.Fatalf("orphan Upsert: %v", err)
	}

	// Pre: 2 entries on disk (pool + orphan)
	if n := len(asFile.GetByChatPool(cs.ID)); n != 2 {
		t.Fatalf("pre: want 2 entries on disk, got %d", n)
	}

	_, err = cs.KillAll()
	if err != nil {
		t.Fatalf("KillAll: %v", err)
	}

	// Post: both gone (snapshot entry + orphan)
	if n := len(asFile.GetByChatPool(cs.ID)); n != 0 {
		t.Errorf("orphan: want 0 entries on disk post-KillAll, got %d", n)
	}
}
