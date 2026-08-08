// Tests for the package-level kill functions in kill.go.
//
// The /kill surface has exactly two entry points:
//
//	KillAgent(c *KillCmd, agentName)    /kill <agent> path
//	KillAllAgents(c *KillCmd)            /kill (no args) path
//
// These tests replace the historical ChatSession.KillOne /
// ChatSession.KillAll test set. The "orphan disk entry GC"
// invariant (TestKillAll_OrphanDiskEntry_GCd in the old file)
// was KillAll-only defense-in-depth — it does not apply to
// the cwd-scoped /kill surface, so it is intentionally dropped.
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

func (c *closedSpy) Close() error {
	c.closes.Add(1)
	close(c.fakeAgentSession.events)
	return nil
}

// failingCloseAS is a fakeAgentSession whose Close() returns an error.
// Used to verify that per-entry Close failures propagate into
// KillResult.Error (review finding #B2: previously `_ = as.Close()`
// was discarded so any failure was reported as ✓ success).
type failingCloseAS struct {
	*fakeAgentSession
	closeErr error
}

func (f *failingCloseAS) Close() error {
	close(f.fakeAgentSession.events)
	return f.closeErr
}

// killCmd is shorthand for the standard test context.
func killCmd(cs *ChatSession) *KillCmd {
	return &KillCmd{CS: cs, Ctx: context.Background()}
}

// =========================================================================
// KillAgent (was KillOne)
// =========================================================================

// TestKillAgent_NotFound — KillAgent with an (agent, activeCwd) pair
// that isn't in the pool returns ErrAgentNotFound and is a no-op
// for everything else (no other entry's Close is invoked).
func TestKillAgent_NotFound(t *testing.T) {
	cs := New("chat-ka-notfound", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	otherCwd := t.TempDir()
	other := injectAS(t, cs, "cc", otherCwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})

	_, err := KillAgent(killCmd(cs), "cc")
	if !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("want ErrAgentNotFound, got %v", err)
	}

	if len(cs.Pool()) != 1 {
		t.Errorf("pool size: want 1 (untouched), got %d", len(cs.Pool()))
	}
	if closes := other.Handle().(*closedSpy).closes.Load(); closes != 0 {
		t.Errorf("other entry Close() should not have been called: got %d", closes)
	}
}

// TestKillAgent_KillsOneLeavesOthers — only the targeted
// (agent, activeCwd) entry's Close is invoked; sibling entries in
// other cwds / other agents survive. The cwd-scoped invariant /kill
// depends on.
func TestKillAgent_KillsOneLeavesOthers(t *testing.T) {
	cs := New("chat-ka-isolated", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	target := injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})
	otherCwd := t.TempDir()
	sibling := injectAS(t, cs, "cc", otherCwd, &closedSpy{fakeAgentSession: newFakeAgentSession(2)})
	otherAgent := injectAS(t, cs, "codex", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(3)})

	result, err := KillAgent(killCmd(cs), "cc")
	if err != nil {
		t.Fatalf("KillAgent: %v", err)
	}
	if result.Agent != "cc" || result.Cwd != cwd {
		t.Errorf("KillResult.Agent/Cwd: want cc@%s, got %s@%s", cwd, result.Agent, result.Cwd)
	}
	if result.Action != "killed" {
		t.Errorf("Action: want killed, got %q", result.Action)
	}
	if result.BeforeState != StatusRunning {
		t.Errorf("BeforeState: want Running, got %s", result.BeforeState)
	}

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
		t.Errorf("pool size post-KillAgent: want 2, got %d", len(cs.Pool()))
	}
}

// TestKillAgent_ActiveASCleared — when the killed entry IS the
// activeAS, the pointer is cleared so the next inbound message
// goes through LookupActiveAgentSession → fresh spawn.
func TestKillAgent_ActiveASCleared(t *testing.T) {
	cs := New("chat-ka-active", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	a := injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})
	cs.mu.Lock()
	cs.activeAS = a
	cs.mu.Unlock()

	if _, err := KillAgent(killCmd(cs), "cc"); err != nil {
		t.Fatalf("KillAgent: %v", err)
	}
	if got := cs.ActiveAgentSession(); got != nil {
		t.Errorf("activeAS should be nil post-KillAgent of active entry, got %v", got)
	}
}

// TestKillAgent_NotActive_LeavesActiveAlone — when the killed
// entry is NOT the activeAS, activeAS must remain pointed at the
// other entry.
func TestKillAgent_NotActive_LeavesActiveAlone(t *testing.T) {
	cs := New("chat-ka-notactive", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	target := injectAS(t, cs, "codex", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})
	active := injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(2)})
	cs.mu.Lock()
	cs.activeAS = active
	cs.mu.Unlock()

	if _, err := KillAgent(killCmd(cs), "codex"); err != nil {
		t.Fatalf("KillAgent: %v", err)
	}
	if got := cs.ActiveAgentSession(); got != active {
		t.Errorf("activeAS should remain pointing at the surviving entry, got %v", got)
	}
	if len(cs.Pool()) != 1 {
		t.Errorf("pool size post-KillAgent: want 1, got %d", len(cs.Pool()))
	}
	if closes := target.Handle().(*closedSpy).closes.Load(); closes != 1 {
		t.Errorf("target Close(): want 1, got %d", closes)
	}
	if closes := active.Handle().(*closedSpy).closes.Load(); closes != 0 {
		t.Errorf("active Close() should not have been called: got %d", closes)
	}
}

// TestKillAgent_StaleCleared — StatusExited entries produce
// stale-cleared results without invoking Close (no process to
// signal) but are still removed from pool + disk.
func TestKillAgent_StaleCleared(t *testing.T) {
	cs := New("chat-ka-stale", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
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

	result, err := KillAgent(killCmd(cs), "cc")
	if err != nil {
		t.Fatalf("KillAgent: %v", err)
	}
	if result.Action != "stale-cleared" {
		t.Errorf("Action: want stale-cleared, got %q", result.Action)
	}
	if result.BeforeState != StatusExited {
		t.Errorf("BeforeState: want Exited, got %s", result.BeforeState)
	}
	if len(cs.Pool()) != 0 {
		t.Errorf("pool should be empty after stale-clear, got %d", len(cs.Pool()))
	}
	if n := len(asFile.GetByChatPool(cs.ID)); n != 0 {
		t.Errorf("registry row should be deleted, got %d entries", n)
	}
}

// TestKillAgent_RegistryRowDeleted — the killed entry's
// agent_sessions.json row is removed. Other rows (other agents /
// other cwds) are untouched.
func TestKillAgent_RegistryRowDeleted(t *testing.T) {
	cs := New("chat-ka-disk", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}

	asFile, err := registry.OpenAgentSessionFile(filepath.Join(t.TempDir(), "agent_sessions.json"))
	if err != nil {
		t.Fatalf("OpenAgentSessionFile: %v", err)
	}
	cs.WithPersistence(nil, asFile)

	target := injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})
	otherCwd := t.TempDir()
	sibling := injectAS(t, cs, "cc", otherCwd, &closedSpy{fakeAgentSession: newFakeAgentSession(2)})

	if err := asFile.Upsert(target.Entry()); err != nil {
		t.Fatalf("target Upsert: %v", err)
	}
	if err := asFile.Upsert(sibling.Entry()); err != nil {
		t.Fatalf("sibling Upsert: %v", err)
	}
	if n := len(asFile.GetByChatPool(cs.ID)); n != 2 {
		t.Fatalf("pre: want 2 disk rows, got %d", n)
	}

	if _, err := KillAgent(killCmd(cs), "cc"); err != nil {
		t.Fatalf("KillAgent: %v", err)
	}

	remaining := asFile.GetByChatPool(cs.ID)
	if len(remaining) != 1 {
		t.Fatalf("post: want 1 disk row (sibling), got %d", len(remaining))
	}
	if remaining[0].ID != sibling.ID {
		t.Errorf("surviving row should be sibling's, got %s", remaining[0].ID)
	}
	if remaining[0].Agent != "cc" || remaining[0].Cwd != otherCwd {
		t.Errorf("surviving row: want cc@%s, got %s@%s",
			otherCwd, remaining[0].Agent, remaining[0].Cwd)
	}
}

// TestKillAgent_QueuePreserved — InputBuffer / queue are not
// touched by KillAgent (only the target's process is torn down).
func TestKillAgent_QueuePreserved(t *testing.T) {
	cs := New("chat-ka-queue", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})

	if err := cs.QueueUserMessage(makeTestMessage(cs,
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "queued"}}, "u-q")); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}
	if cs.QueueLen() != 1 {
		t.Fatalf("pre: queue should have 1 message")
	}

	if _, err := KillAgent(killCmd(cs), "cc"); err != nil {
		t.Fatalf("KillAgent: %v", err)
	}
	if cs.QueueLen() != 1 {
		t.Errorf("queue should survive KillAgent (only /new discards): got %d", cs.QueueLen())
	}
}

// =========================================================================
// KillAllAgents (was KillAll, but cwd-scoped)
// =========================================================================

// TestKillAllAgents_GracefulShutdown — every running agent in
// activeCwd gets Close() invoked.
func TestKillAllAgents_GracefulShutdown(t *testing.T) {
	cs := New("chat-kaa-graceful", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	a1 := injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})
	a2 := injectAS(t, cs, "codex", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(2)})
	cs.mu.Lock()
	cs.activeAS = a1
	cs.mu.Unlock()

	results, err := KillAllAgents(killCmd(cs))
	if err != nil {
		t.Fatalf("KillAllAgents err: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	if close1 := a1.Handle().(*closedSpy).closes.Load(); close1 != 1 {
		t.Errorf("a1 Close(): want 1, got %d", close1)
	}
	if close2 := a2.Handle().(*closedSpy).closes.Load(); close2 != 1 {
		t.Errorf("a2 Close(): want 1, got %d", close2)
	}
	for _, r := range results {
		if r.BeforeState != StatusRunning {
			t.Errorf("BeforeState: want Running, got %s", r.BeforeState)
		}
		if r.Action != "killed" {
			t.Errorf("Action: want killed, got %s", r.Action)
		}
	}
}

// TestKillAllAgents_QueuePreserved — queued user messages survive.
func TestKillAllAgents_QueuePreserved(t *testing.T) {
	cs := New("chat-kaa-buf", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})

	if err := cs.QueueUserMessage(makeTestMessage(cs,
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "msg"}}, "u1")); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}
	if got := cs.QueueLen(); got == 0 {
		t.Fatalf("queue should have a message pre-KillAllAgents")
	}

	if _, err := KillAllAgents(killCmd(cs)); err != nil {
		t.Fatalf("KillAllAgents: %v", err)
	}
	if got := cs.QueueLen(); got != 1 {
		t.Errorf("queue should be preserved across /kill: want 1, got %d", got)
	}
}

// TestKillAllAgents_AgentSessionEntriesDeleted — registry rows
// for killed entries are deleted.
func TestKillAllAgents_AgentSessionEntriesDeleted(t *testing.T) {
	cs := New("chat-kaa-disk", "cc")
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

	if err := asFile.Upsert(a.Entry()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if got := len(asFile.GetByChatPool(cs.ID)); got != 1 {
		t.Fatalf("want 1 entry pre-KillAllAgents, got %d", got)
	}

	if _, err := KillAllAgents(killCmd(cs)); err != nil {
		t.Fatalf("KillAllAgents: %v", err)
	}

	if got := len(asFile.GetByChatPool(cs.ID)); got != 0 {
		t.Errorf("agent_sessions.json entries should be deleted post-KillAllAgents: got %d", got)
	}
}

// TestKillAllAgents_ActiveASCleared — activeAS pointer is nil
// after /kill so the next inbound goes through
// LookupActiveAgentSession → fresh spawn.
func TestKillAllAgents_ActiveASCleared(t *testing.T) {
	cs := New("chat-kaa-active", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	a := injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})
	cs.mu.Lock()
	cs.activeAS = a
	cs.mu.Unlock()

	if _, err := KillAllAgents(killCmd(cs)); err != nil {
		t.Fatalf("KillAllAgents: %v", err)
	}
	if got := cs.ActiveAgentSession(); got != nil {
		t.Errorf("activeAS should be nil, got %v", got)
	}
}

// TestKillAllAgents_OnlyDeadEntries — StatusExited / Detached
// entries do NOT trigger Close(); reported as "stale-cleared".
func TestKillAllAgents_OnlyDeadEntries(t *testing.T) {
	cs := New("chat-kaa-dead", "cc")
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

	results, err := KillAllAgents(killCmd(cs))
	if err != nil {
		t.Fatalf("KillAllAgents: %v", err)
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

// TestKillAllAgents_MixedStates — running + dead entries each
// take their own path.
func TestKillAllAgents_MixedStates(t *testing.T) {
	cs := New("chat-kaa-mixed", "cc")
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

	results, err := KillAllAgents(killCmd(cs))
	if err != nil {
		t.Fatalf("KillAllAgents: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
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
	} else if r.Action != "stale-cleared" {
		t.Errorf("codex.Action: want stale-cleared, got %q", r.Action)
	}
}

// TestKillAllAgents_EmptyPool — no entries at all → (nil, nil).
func TestKillAllAgents_EmptyPool(t *testing.T) {
	cs := New("chat-kaa-empty", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	results, err := KillAllAgents(killCmd(cs))
	if err != nil {
		t.Fatalf("KillAllAgents: %v", err)
	}
	if results != nil {
		t.Errorf("want nil results for empty pool, got %v", results)
	}
}

// TestKillAllAgents_NoActiveCwd — empty activeCwd → (nil, nil)
// without touching the pool (cwd-scoped invariant: nothing to kill
// in a non-existent cwd).
func TestKillAllAgents_NoActiveCwd(t *testing.T) {
	cs := New("chat-kaa-nocwd", "cc")
	cs.WithPersistence(nil, nil)

	// Inject an entry anyway — it must NOT be killed.
	a := injectAS(t, cs, "cc", "/some/other/cwd",
		&closedSpy{fakeAgentSession: newFakeAgentSession(1)})
	cs.mu.Lock()
	cs.activeAS = a
	cs.mu.Unlock()

	results, err := KillAllAgents(killCmd(cs))
	if err != nil {
		t.Fatalf("KillAllAgents: %v", err)
	}
	if results != nil {
		t.Errorf("want nil results when activeCwd empty, got %v", results)
	}
	if closes := a.Handle().(*closedSpy).closes.Load(); closes != 0 {
		t.Errorf("entry in other cwd must NOT be killed: got %d closes", closes)
	}
	if cs.ActiveAgentSession() != a {
		t.Errorf("activeAS must NOT be cleared when no kill happened")
	}
}

// TestKillAllAgents_CloseErrorPropagates — when a bridge's
// Close() returns an error, the corresponding KillResult records
// Error != nil and Action = "kill-failed".
func TestKillAllAgents_CloseErrorPropagates(t *testing.T) {
	cs := New("chat-kaa-close-err", "cc")
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

	results, err := KillAllAgents(killCmd(cs))
	if err != nil {
		t.Fatalf("KillAllAgents: %v", err)
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

// TestKillAllAgents_StatusDetached_StillCallsClose — review
// finding #B1: SetDetached documents "process alive but nightme
// no longer holds it" — the live CLI must still be closed.
func TestKillAllAgents_StatusDetached_StillCallsClose(t *testing.T) {
	cs := New("chat-kaa-detached", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	a := NewAgentSession(newAgentSessionID(), cs.ID, "cc", cwd, nil)
	a.handle = &closedSpy{fakeAgentSession: newFakeAgentSession(1)}
	a.SetRunning(1234)
	a.SetDetached()
	cs.mu.Lock()
	cs.pool[agentCwdKey{Agent: "cc", Cwd: cwd}] = a
	cs.mu.Unlock()

	results, err := KillAllAgents(killCmd(cs))
	if err != nil {
		t.Fatalf("KillAllAgents: %v", err)
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

// TestKillAllAgents_StatusDetached_NoHandle_StaleCleared — when
// the entry is StatusDetached but the handle is nil (post-restart
// orphan from FromAgentSessionEntry), no process can be signaled.
// The entry is treated as stale-cleared.
func TestKillAllAgents_StatusDetached_NoHandle_StaleCleared(t *testing.T) {
	cs := New("chat-kaa-detached-orphan", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	a := NewAgentSession(newAgentSessionID(), cs.ID, "cc", cwd, nil)
	a.SetDetached()
	cs.mu.Lock()
	cs.pool[agentCwdKey{Agent: "cc", Cwd: cwd}] = a
	cs.mu.Unlock()

	results, err := KillAllAgents(killCmd(cs))
	if err != nil {
		t.Fatalf("KillAllAgents: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].Action != "stale-cleared" {
		t.Errorf("Action: want stale-cleared (no handle), got %q", results[0].Action)
	}
}

// TestKillAllAgents_OnlyActiveCwdEntries — the cwd-scoped
// invariant: entries in OTHER cwds (from prior /cwd switches) are
// preserved. Only entries in activeCwd are killed.
func TestKillAllAgents_OnlyActiveCwdEntries(t *testing.T) {
	cs := New("chat-kaa-cwd-scope", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	// Two in activeCwd, one in a different cwd.
	inCwd1 := injectAS(t, cs, "cc", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(1)})
	inCwd2 := injectAS(t, cs, "codex", cwd, &closedSpy{fakeAgentSession: newFakeAgentSession(2)})
	otherCwd := t.TempDir()
	outOfScope := injectAS(t, cs, "cc", otherCwd, &closedSpy{fakeAgentSession: newFakeAgentSession(3)})

	results, err := KillAllAgents(killCmd(cs))
	if err != nil {
		t.Fatalf("KillAllAgents: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results (only cwd-scoped), got %d", len(results))
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
		t.Errorf("pool size post-KillAllAgents: want 1 (only outOfScope), got %d", len(cs.Pool()))
	}
}