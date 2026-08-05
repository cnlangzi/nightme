// F-34 tests for ChatSession.NewActiveAgentSessions.
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

// callRecordingAS records every New() invocation. err is returned
// from New; tests that want a failing bridge set err to errInjected.
type callRecordingAS struct {
	*fakeAgentSession
	calls atomic.Int32
	err   error
}

func (c *callRecordingAS) New(_ context.Context) error {
	c.calls.Add(1)
	return c.err
}

// failingNewAS is a fake whose New always returns errInjected. Used to
// exercise the partial-failure path.
type failingNewAS struct {
	*fakeAgentSession
}

var errInjected = errors.New("injected bridge failure")

func (f *failingNewAS) New(_ context.Context) error { return errInjected }

// injectAS installs an AgentSession directly into cs.pool with the
// provided handle, bypassing Spawner. Mirrors what Spawn would produce
// but skips the start-from-zero state, so tests stay focused on the
// NewActiveAgentSessions filter / counter logic.
func injectAS(t *testing.T, cs *ChatSession, agentName, cwd string, handle agent.AgentSession) *AgentSession {
	t.Helper()
	cs.mu.Lock()
	defer cs.mu.Unlock()
	id := newAgentSessionID()
	as := NewAgentSession(id, cs.ID, agentName, cwd, nil)
	as.handle = handle
	as.SetRunning(1234) // arbitrary pid; needed so Status()==StatusRunning
	cs.pool[agentCwdKey{Agent: agentName, Cwd: cwd}] = as
	return as
}

// TestNewActiveAgentSessions_NoCwd verifies the empty-cwd fast path
// returns (0,0,nil) so the handler can reply "send /cwd first".
func TestNewActiveAgentSessions_NoCwd(t *testing.T) {
	cs := New("chat-nocwd", "cc")
	matched, reset, _, err := cs.NewActiveAgentSessions(context.Background(), "")
	if err != nil || matched != 0 || reset != 0 {
		t.Fatalf("want (0,0,nil), got (%d,%d,%v)", matched, reset, err)
	}
}

// TestNewActiveAgentSessions_EmptyPool verifies matched==0 when
// activeCwd is set but the pool has no entries.
func TestNewActiveAgentSessions_EmptyPool(t *testing.T) {
	cs := New("chat-empty", "cc")
	if err := cs.SetActiveCwd(t.TempDir()); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	matched, reset, _, err := cs.NewActiveAgentSessions(context.Background(), "")
	if err != nil || matched != 0 || reset != 0 {
		t.Fatalf("want (0,0,nil), got (%d,%d,%v)", matched, reset, err)
	}
}

// TestNewActiveAgentSessions_AllRunningReset verifies that with N
// RUNNING AgentSessions in activeCwd, all N are reset and InputBuffer
// is cleared.
func TestNewActiveAgentSessions_AllRunningReset(t *testing.T) {
	cs := New("chat-all", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	a1 := injectAS(t, cs, "cc", cwd, &callRecordingAS{fakeAgentSession: newFakeAgentSession(1)})
	a2 := injectAS(t, cs, "codex", cwd, &callRecordingAS{fakeAgentSession: newFakeAgentSession(2)})

	// Force InputBuffer into Busy state and add a queued message so
	// we can assert Clear() actually emptied it (Add otherwise flushes
	// immediately when Idle, bypassing the queue).
	cs.ensureBuffer()
	cs.inputBuffer.SetState(StateBusy)
	if err := cs.inputBuffer.Add([]agent.ContentBlock{{Type: agent.ContentText, Text: "queued"}}, "u1"); err != nil {
		t.Fatalf("inputBuffer.Add: %v", err)
	}
	if got := cs.inputBuffer.Pending(); got == 0 {
		t.Fatalf("inputBuffer pending should be > 0 before reset")
	}

	matched, reset, _, err := cs.NewActiveAgentSessions(context.Background(), "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if matched != 2 || reset != 2 {
		t.Fatalf("want matched=2 reset=2, got matched=%d reset=%d", matched, reset)
	}
	if got := cs.inputBuffer.Pending(); got != 0 {
		t.Fatalf("inputBuffer not cleared: Pending=%d", got)
	}
	// Identity preserved.
	if a1.Cwd != cwd || a2.Cwd != cwd {
		t.Fatalf("AgentSession.Cwd mutated: %q %q", a1.Cwd, a2.Cwd)
	}
	if a1.Agent != "cc" || a2.Agent != "codex" {
		t.Fatalf("AgentSession.Agent mutated: %q %q", a1.Agent, a2.Agent)
	}
}

// TestNewActiveAgentSessions_AgentNameFilter verifies /new <agent>
// only resets the named agent.
func TestNewActiveAgentSessions_AgentNameFilter(t *testing.T) {
	cs := New("chat-named", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	a1 := injectAS(t, cs, "cc", cwd, &callRecordingAS{fakeAgentSession: newFakeAgentSession(1)})
	a2 := injectAS(t, cs, "codex", cwd, &callRecordingAS{fakeAgentSession: newFakeAgentSession(2)})

	matched, reset, _, err := cs.NewActiveAgentSessions(context.Background(), "cc")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if matched != 1 || reset != 1 {
		t.Fatalf("want (1,1), got (%d,%d)", matched, reset)
	}
	// Sanity: a2 was not touched.
	if a2.Handle().(*callRecordingAS).calls.Load() != 0 {
		t.Fatalf("codex AS should not have been New()ed: calls=%d",
			a2.Handle().(*callRecordingAS).calls.Load())
	}
	if a1.Handle() == nil {
		t.Fatalf("a1.Handle() nil unexpectedly")
	}
}

// TestNewActiveAgentSessions_DetachedSkipped verifies that AgentSessions
// that are NOT Running are NOT triggered into a lazy spawn (F-34 §6 Q-N4
// product clarification) AND that their stale ResumeID is cleared so the
// next spawn will not resurrect a dead session (F-42 §5.4).
//
// F-42 changes the counting semantics: dead/detached entries now count
// as matched AND reset (with Action="marked-fresh") — the previous
// silently-skip behavior was a bug because the stale ResumeID would be
// replayed on the next spawn.
func TestNewActiveAgentSessions_DetachedSkipped(t *testing.T) {
	cs := New("chat-skip", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	// Detached entry: SetExited clears PID; Status returns StatusExited.
	a := NewAgentSession(newAgentSessionID(), cs.ID, "cc", cwd, nil)
	a.SetResumeID("dead-session-id-456") // populated as if a previous run
	// captured an init event
	a.SetExited(0)
	cs.mu.Lock()
	cs.pool[agentCwdKey{Agent: "cc", Cwd: cwd}] = a
	cs.mu.Unlock()

	matched, reset, results, err := cs.NewActiveAgentSessions(context.Background(), "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if matched != 1 || reset != 1 {
		t.Fatalf("want (1,1) for Exited under F-42, got (%d,%d)", matched, reset)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].Action != "marked-fresh" {
		t.Fatalf("want Action=marked-fresh, got %q", results[0].Action)
	}
	if got := a.ResumeID(); got != "" {
		t.Fatalf("ResumeID should be cleared, got %q", got)
	}
}

// TestNewActiveAgentSessions_CwdFilter verifies only the activeCwd
// entries are touched (other cwd entries are left alone).
func TestNewActiveAgentSessions_CwdFilter(t *testing.T) {
	cs := New("chat-cwd", "cc")
	cwd1 := t.TempDir()
	cwd2 := t.TempDir()
	if err := cs.SetActiveCwd(cwd1); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	other := injectAS(t, cs, "cc", cwd2, &callRecordingAS{fakeAgentSession: newFakeAgentSession(1)})
	here := injectAS(t, cs, "cc", cwd1, &callRecordingAS{fakeAgentSession: newFakeAgentSession(2)})

	matched, reset, _, err := cs.NewActiveAgentSessions(context.Background(), "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if matched != 1 || reset != 1 {
		t.Fatalf("want (1,1), got (%d,%d)", matched, reset)
	}
	if other.Handle().(*callRecordingAS).calls.Load() != 0 {
		t.Fatalf("other-cwd AS should not have been New()ed: calls=%d",
			other.Handle().(*callRecordingAS).calls.Load())
	}
	if here.Handle().(*callRecordingAS).calls.Load() != 1 {
		t.Fatalf("activeCwd AS should have been New()ed once: calls=%d",
			here.Handle().(*callRecordingAS).calls.Load())
	}
}

// TestNewActiveAgentSessions_PartialFailure verifies that one
// failing New returns matched > reset + non-nil err, and that
// InputBuffer is still cleared.
func TestNewActiveAgentSessions_PartialFailure(t *testing.T) {
	cs := New("chat-fail", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	injectAS(t, cs, "cc", cwd, &callRecordingAS{fakeAgentSession: newFakeAgentSession(1)})
	injectAS(t, cs, "codex", cwd, &failingNewAS{fakeAgentSession: newFakeAgentSession(2)})
	cs.ensureBuffer()
	cs.inputBuffer.SetState(StateBusy)
	if err := cs.inputBuffer.Add([]agent.ContentBlock{{Type: agent.ContentText, Text: "x"}}, "u1"); err != nil {
		t.Fatalf("inputBuffer.Add: %v", err)
	}

	matched, reset, _, err := cs.NewActiveAgentSessions(context.Background(), "")
	if !errors.Is(err, errInjected) {
		t.Fatalf("want errInjected, got %v", err)
	}
	if matched != 2 || reset != 1 {
		t.Fatalf("want (2,1), got (%d,%d)", matched, reset)
	}
	if got := cs.inputBuffer.Pending(); got != 0 {
		t.Fatalf("inputBuffer not cleared on partial failure: %d", got)
	}
}

// TestAgentSession_New_Delegate verifies the wrapper passes the call
// through to the bridge handle, and returns ErrNotRunning when the
// handle is nil (Detached state).
func TestAgentSession_New_Delegate(t *testing.T) {
	t.Run("running delegates", func(t *testing.T) {
		cs := New("chat-running", "cc")
		cwd := t.TempDir()
		cs.SetActiveCwd(cwd)
		bh := &callRecordingAS{fakeAgentSession: newFakeAgentSession(1)}
		as := injectAS(t, cs, "cc", cwd, bh)

		if err := as.New(context.Background(), nil); err != nil {
			t.Fatalf("New: %v", err)
		}
		if bh.calls.Load() != 1 {
			t.Fatalf("bridge New should be called once, got %d", bh.calls.Load())
		}
	})

	t.Run("detached returns ErrNotRunning", func(t *testing.T) {
		cs := New("chat-detached", "cc")
		cwd := t.TempDir()
		cs.SetActiveCwd(cwd)
		// Construct an AS whose handle is nil: use NewAgentSession
		// (no SetRunning, no handle attached).
		as := NewAgentSession(newAgentSessionID(), cs.ID, "cc", cwd, nil)
		cs.mu.Lock()
		cs.pool[agentCwdKey{Agent: "cc", Cwd: cwd}] = as
		cs.mu.Unlock()

		err := as.New(context.Background(), nil)
		if !errors.Is(err, ErrNotRunning) {
			t.Fatalf("want ErrNotRunning, got %v", err)
		}
	})

	t.Run("bridge-returns-ErrRestartRequired-falls-back-to-respawn", func(t *testing.T) {
		// F-34 + product clarification 2026-08-04: when the bridge
		// (e.g. raw pty) cannot do an in-place reset and returns
		// agent.ErrRestartRequired, the wrapper must close the old
		// handle and spawn a fresh one via the Spawner, with
		// ResumeID cleared.
		cs := New("chat-restart", "cc")
		cwd := t.TempDir()
		cs.SetActiveCwd(cwd)
		old := &restartErrAS{fakeAgentSession: newFakeAgentSession(1)}
		as := injectAS(t, cs, "cc", cwd, old)
		as.SetResumeID("stale-id-should-be-cleared")

		newAS := &callRecordingAS{fakeAgentSession: newFakeAgentSession(2)}
		spawner := &fakeRestartSpawner{handle: newAS}

		if err := as.New(context.Background(), spawner); err != nil {
			t.Fatalf("New: %v", err)
		}
		if !old.closed {
			t.Fatalf("old handle should have been Close()d")
		}
		if got := as.Handle(); got != agent.AgentSession(newAS) {
			t.Fatalf("handle not swapped: got %T", got)
		}
		if got := as.ResumeID(); got != "" {
			t.Fatalf("ResumeID should be cleared, got %q", got)
		}
		if newAS.calls.Load() != 0 {
			t.Fatalf("newAS should not have been New()ed (it's the freshly-spawned handle)")
		}
		if got := as.PID(); got != 2 {
			t.Fatalf("PID should be 2 (newFakeAgentSession), got %d", got)
		}
		if spawner.calledWithResumeID != "" {
			t.Fatalf("Spawn should be called with empty resumeID, got %q", spawner.calledWithResumeID)
		}
	})

	t.Run("bridge-ErrRestartRequired-with-nil-spawner-propagates", func(t *testing.T) {
		cs := New("chat-restart-no-spawner", "cc")
		cwd := t.TempDir()
		cs.SetActiveCwd(cwd)
		old := &restartErrAS{fakeAgentSession: newFakeAgentSession(1)}
		as := injectAS(t, cs, "cc", cwd, old)

		err := as.New(context.Background(), nil)
		if !errors.Is(err, agent.ErrRestartRequired) {
			t.Fatalf("want ErrRestartRequired, got %v", err)
		}
		if old.closed {
			t.Fatalf("old handle should NOT have been Close()d when no spawner available")
		}
	})

	t.Run("respawn-spawn-failure-cleans-up-status", func(t *testing.T) {
		// F-34 Phase 3 review #4: when bridge.New returns
		// ErrRestartRequired AND spawner.Spawn fails, the wrapper
		// must mark the AS Exited so subsequent LookupActiveAgentSession
		// lazy-spawns a fresh one. Previously status stayed Running
		// with handle=nil → next SendBlocks returned ErrNotRunning.
		cs := New("chat-respawn-fail", "cc")
		cwd := t.TempDir()
		cs.SetActiveCwd(cwd)
		old := &restartErrAS{fakeAgentSession: newFakeAgentSession(1)}
		as := injectAS(t, cs, "cc", cwd, old)
		as.SetResumeID("stale-id")

		failingSpawner := &fakeFailingSpawner{err: errors.New("spawn blew up")}
		err := as.New(context.Background(), failingSpawner)
		if err == nil {
			t.Fatalf("New should have failed")
		}
		if got := as.Status(); got != StatusExited {
			t.Fatalf("status = %s, want StatusExited", got)
		}
		if got := as.Handle(); got != nil {
			t.Fatalf("handle should be nil after spawn failure, got %T", got)
		}
		if got := as.ResumeID(); got != "" {
			t.Fatalf("ResumeID should be cleared, got %q", got)
		}
	})
}

// restartErrAS is a bridge fake whose New always returns
// agent.ErrRestartRequired. It records whether Close() was called so
// the wrapper's kill+respawn path can be verified.
type restartErrAS struct {
	*fakeAgentSession
	closed bool
}

func (r *restartErrAS) New(_ context.Context) error { return agent.ErrRestartRequired }
func (r *restartErrAS) Close() error {
	r.closed = true
	return r.fakeAgentSession.Close()
}

// fakeRestartSpawner is a minimal chatsession.Spawner that returns a
// pre-built handle. The wrapper only needs Spawn(ctx, name, cwd, args,
// resumeID); we record the resumeID it was called with so tests can
// assert "no --resume on the fresh spawn".
type fakeRestartSpawner struct {
	handle             agent.AgentSession
	calledWithResumeID string
}

func (f *fakeRestartSpawner) Spawn(_ context.Context, _, _ string, _ []string, resumeID string) (agent.AgentSession, error) {
	f.calledWithResumeID = resumeID
	return f.handle, nil
}

// fakeFailingSpawner returns a non-nil error from Spawn, used to
// exercise the wrapper's spawn-failure cleanup path (F-34 review #4).
type fakeFailingSpawner struct{ err error }

func (f *fakeFailingSpawner) Spawn(_ context.Context, _, _ string, _ []string, _ string) (agent.AgentSession, error) {
	return nil, f.err
}

// F-42 tests for the dead/detached branch of NewActiveAgentSessions.
// These lock the new behavior introduced by F-42 §5.4: dead entries
// are NOT silently skipped — their stale ResumeID is cleared
// (in-memory + persisted) so the next spawn will not resurrect a
// dead session via --resume <dead-id>.

// TestNewActiveAgentSessions_DeadEntryClearsResumeIDInMemory verifies
// that a dead entry's ResumeID is cleared in-memory after /new.
func TestNewActiveAgentSessions_DeadEntryClearsResumeIDInMemory(t *testing.T) {
	cs := New("chat-dead-mem", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	// A dead entry with a stale ResumeID from a previous run.
	a := NewAgentSession(newAgentSessionID(), cs.ID, "cc", cwd, nil)
	a.SetResumeID("claude-sess-dead-123")
	a.SetExited(0)
	cs.mu.Lock()
	cs.pool[agentCwdKey{Agent: "cc", Cwd: cwd}] = a
	cs.mu.Unlock()

	if got := a.ResumeID(); got != "claude-sess-dead-123" {
		t.Fatalf("precondition: want ResumeID=%q, got %q", "claude-sess-dead-123", got)
	}

	matched, reset, results, err := cs.NewActiveAgentSessions(context.Background(), "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if matched != 1 || reset != 1 {
		t.Fatalf("Counters: want (1,1), got (%d,%d)", matched, reset)
	}
	if len(results) != 1 || results[0].Action != "marked-fresh" {
		t.Fatalf("result: want one marked-fresh, got %+v", results)
	}
	if got := a.ResumeID(); got != "" {
		t.Errorf("ResumeID should be cleared in-memory: got %q", got)
	}
}

// TestNewActiveAgentSessions_DeadEntryPersistsClearedResumeID verifies
// that the cleared ResumeID is persisted to agent_sessions.json so the
// next spawn will not replay the old value.
func TestNewActiveAgentSessions_DeadEntryPersistsClearedResumeID(t *testing.T) {
	cs := New("chat-dead-persist", "cc")
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
	a.SetResumeID("claude-sess-dead-xyz")
	a.SetExited(0)
	cs.mu.Lock()
	cs.pool[agentCwdKey{Agent: "cc", Cwd: cwd}] = a
	cs.mu.Unlock()
	if err := asFile.Upsert(a.Entry()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if _, _, _, err := cs.NewActiveAgentSessions(context.Background(), ""); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Reload from disk to verify persistence.
	entry, ok := asFile.Get(a.ID)
	if !ok {
		t.Fatalf("entry should still be in the registry (F-42 §5.5: keep entry, clear ResumeID)")
	}
	if entry.ResumeID != "" {
		t.Errorf("persisted ResumeID should be cleared: got %q", entry.ResumeID)
	}
}

// TestNewActiveAgentSessions_DeadEntryDoesNotSpawn locks the F-34
// §6 Q-N4 product clarification: dead entries must NOT trigger a
// lazy spawn just to reset their conversation.
func TestNewActiveAgentSessions_DeadEntryDoesNotSpawn(t *testing.T) {
	cs := New("chat-dead-no-spawn", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	spy := &fakeRestartSpawner{handle: newFakeAgentSession(99)}
	cs.WithSpawner(spy)

	a := NewAgentSession(newAgentSessionID(), cs.ID, "cc", cwd, nil)
	a.SetExited(0)
	cs.mu.Lock()
	cs.pool[agentCwdKey{Agent: "cc", Cwd: cwd}] = a
	cs.mu.Unlock()

	if _, _, _, err := cs.NewActiveAgentSessions(context.Background(), ""); err != nil {
		t.Fatalf("err: %v", err)
	}
	if spy.calledWithResumeID != "" && spy.calledWithResumeID != "<never-called>" {
		t.Errorf("Spawner.Spawn should NOT have been called for dead entry")
	}
}

// TestNewActiveAgentSessions_RunningPlusDeadMixed covers the F-42
// expected behavior when the pool has both running and dead entries.
// The dead one gets marked-fresh; the running one gets in-place-reset.
func TestNewActiveAgentSessions_RunningPlusDeadMixed(t *testing.T) {
	cs := New("chat-mixed", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	live := injectAS(t, cs, "cc", cwd, &callRecordingAS{fakeAgentSession: newFakeAgentSession(1)})
	dead := NewAgentSession(newAgentSessionID(), cs.ID, "codex", cwd, nil)
	dead.SetResumeID("codex-sess-dead-789")
	dead.SetExited(0)
	cs.mu.Lock()
	cs.pool[agentCwdKey{Agent: "codex", Cwd: cwd}] = dead
	cs.mu.Unlock()

	matched, reset, results, err := cs.NewActiveAgentSessions(context.Background(), "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if matched != 2 || reset != 2 {
		t.Fatalf("counters: want (2,2), got (%d,%d)", matched, reset)
	}

	// Verify actions per agent.
	byName := map[string]ResetResult{}
	for _, r := range results {
		byName[r.Agent] = r
	}
	if r := byName["cc"]; r.Action != "in-place-reset" {
		t.Errorf("cc: want in-place-reset, got %q", r.Action)
	}
	if r := byName["codex"]; r.Action != "marked-fresh" {
		t.Errorf("codex: want marked-fresh, got %q", r.Action)
	}

	// Dead entry's ResumeID is cleared.
	if got := dead.ResumeID(); got != "" {
		t.Errorf("dead ResumeID should be cleared: got %q", got)
	}
	// Live entry's bridge.New was called once.
	if live.Handle().(*callRecordingAS).calls.Load() != 1 {
		t.Errorf("live agent New() should have been called once: got %d",
			live.Handle().(*callRecordingAS).calls.Load())
	}
}

// TestNewActiveAgentSessions_ResultsSliceHasEveryEntry locks that the
// result slice length matches the matched count (1:1 mapping).
func TestNewActiveAgentSessions_ResultsSliceHasEveryEntry(t *testing.T) {
	cs := New("chat-result-len", "cc")
	cwd := t.TempDir()
	if err := cs.SetActiveCwd(cwd); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)
	injectAS(t, cs, "cc", cwd, &callRecordingAS{fakeAgentSession: newFakeAgentSession(1)})
	injectAS(t, cs, "codex", cwd, &callRecordingAS{fakeAgentSession: newFakeAgentSession(2)})

	matched, _, results, err := cs.NewActiveAgentSessions(context.Background(), "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(results) != matched {
		t.Errorf("len(results)=%d != matched=%d (1:1 invariant)", len(results), matched)
	}
}