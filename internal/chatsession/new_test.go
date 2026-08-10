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

// buildLive wraps c in a *agent.Agent with callRecordingASDriver
// so tests can type-assert Handle().Driver().(*callRecordingASDriver).
func (c *callRecordingAS) buildLive() *agent.Agent {
	return agent.NewAgent(
		agent.NewInfo("fake", agent.ModePTY, "fake", nil, nil),
		c.pid, c.events,
		&callRecordingASDriver{inner: c, calls: &c.calls})
}

func (c *callRecordingAS) New(_ context.Context) error {
	c.calls.Add(1)
	return c.err
}

// callRecordingASDriver wraps a callRecordingAS for the agent.driver
// interface. Tests use Handle().Driver().(*callRecordingASDriver)
// to read c.calls.
type callRecordingASDriver struct {
	inner *callRecordingAS
	calls *atomic.Int32
}

func (d *callRecordingASDriver) SendBlocks(ctx context.Context, b []agent.ContentBlock) error {
	return d.inner.SendBlocks(ctx, b)
}
func (d *callRecordingASDriver) SendPermission(resp string) error {
	return d.inner.SendPermission(resp)
}
func (d *callRecordingASDriver) Reset(ctx context.Context) error { return d.inner.New(ctx) }
func (d *callRecordingASDriver) Stop(ctx context.Context) error { return d.inner.Stop(ctx) }
func (d *callRecordingASDriver) SetModel(ctx context.Context, providerID, modelID string) error {
	return d.inner.SetModel(ctx, providerID, modelID)
}
func (d *callRecordingASDriver) Close() error                   { return d.inner.Close() }

// failingNewAS is a fake whose New always returns errInjected. Used to
// exercise the partial-failure path.
type failingNewAS struct {
	*fakeAgentSession
}

var errInjected = errors.New("injected bridge failure")


func (f *failingNewAS) buildLive() *agent.Agent {
	return agent.NewAgent(
		agent.NewInfo("fake", agent.ModePTY, "fake", nil, nil),
		f.pid, f.events,
		&failingNewASDriver{inner: f})
}

// failingNewASDriver forwards driver calls to a failingNewAS.
type failingNewASDriver struct{ inner *failingNewAS }

func (d *failingNewASDriver) SendBlocks(ctx context.Context, b []agent.ContentBlock) error {
	return d.inner.SendBlocks(ctx, b)
}
func (d *failingNewASDriver) SendPermission(resp string) error {
	return d.inner.SendPermission(resp)
}
func (d *failingNewASDriver) Reset(ctx context.Context) error { return d.inner.New(ctx) }
func (d *failingNewASDriver) Stop(ctx context.Context) error { return d.inner.Stop(ctx) }
func (d *failingNewASDriver) SetModel(ctx context.Context, providerID, modelID string) error {
	return d.inner.SetModel(ctx, providerID, modelID)
}
func (d *failingNewASDriver) Close() error                   { return d.inner.Close() }

func (f *failingNewAS) New(_ context.Context) error { return errInjected }

// injectAS installs an AgentSession directly into cs.pool with the
// provided handle, bypassing Spawner. Mirrors what Spawn would produce
// but skips the start-from-zero state, so tests stay focused on the
// NewActiveAgentSessions filter / counter logic.
func injectAS(t *testing.T, cs *ChatSession, agentName, cwd string, handle *agent.Agent) *AgentSession {
	t.Helper()
	cs.mu.Lock()
	defer cs.mu.Unlock()
	id := newAgentSessionID()
	as := NewAgentSession(id, cs.ID, agentName, cwd, nil)
	as.SetHandleForTest(handle)
	as.SetRunning(1234) // arbitrary pid; needed so Status()==StatusRunning
	cs.pool[agentCwdKey{Agent: agentName, Cwd: cwd}] = as
	return as
}

// TestNewActiveAgentSessions_NoCwd verifies the empty-cwd fast path
// returns (0,0,nil) so the handler can reply "send /cwd first".
func TestNewActiveAgentSessions_NoCwd(t *testing.T) {
	cs, _ := New("chat-nocwd", "cc", newTestChannel())
	matched, reset, _, err := cs.NewActiveAgentSessions(context.Background(), "")
	if err != nil || matched != 0 || reset != 0 {
		t.Fatalf("want (0,0,nil), got (%d,%d,%v)", matched, reset, err)
	}
}

// TestNewActiveAgentSessions_EmptyPool verifies matched==0 when
// activeCwd is set but the pool has no entries.
func TestNewActiveAgentSessions_EmptyPool(t *testing.T) {
	cs, _ := New("chat-empty", "cc", newTestChannel())
	if err := cs.SetSelectedCwd(t.TempDir()); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
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
	cs, _ := New("chat-all", "cc", newTestChannel())
	cwd := t.TempDir()
	if err := cs.SetSelectedCwd(cwd); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	a1 := injectAS(t, cs, "cc", cwd, (&callRecordingAS{fakeAgentSession: newFakeAgentSession(1)}).buildLive())
	a2 := injectAS(t, cs, "codex", cwd, (&callRecordingAS{fakeAgentSession: newFakeAgentSession(2)}).buildLive())

	// Queue a message so we can assert /new does NOT discard it.
	// No selectedAS is installed, so the TryFlush inside
	// QueueUserMessage is a no-op and the message stays queued.
	if err := cs.QueueUserMessage(makeTestMessage(cs,
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "queued"}}, "u1")); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}
	if got := cs.QueueLen(); got == 0 {
		t.Fatalf("queue should be non-empty before reset")
	}

	matched, reset, _, err := cs.NewActiveAgentSessions(context.Background(), "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if matched != 2 || reset != 2 {
		t.Fatalf("want matched=2 reset=2, got matched=%d reset=%d", matched, reset)
	}
	// /new resets the agent's conversation context but does NOT
	// discard queued work — those messages are still owed a reply
	// and flush into the fresh context on the next TryFlush.
	if got := cs.QueueLen(); got != 1 {
		t.Fatalf("queue must survive /new: want 1, got %d", got)
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
	cs, _ := New("chat-named", "cc", newTestChannel())
	cwd := t.TempDir()
	if err := cs.SetSelectedCwd(cwd); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	a1 := injectAS(t, cs, "cc", cwd, (&callRecordingAS{fakeAgentSession: newFakeAgentSession(1)}).buildLive())
	a2 := injectAS(t, cs, "codex", cwd, (&callRecordingAS{fakeAgentSession: newFakeAgentSession(2)}).buildLive())

	matched, reset, _, err := cs.NewActiveAgentSessions(context.Background(), "cc")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if matched != 1 || reset != 1 {
		t.Fatalf("want (1,1), got (%d,%d)", matched, reset)
	}
	// Sanity: a2 was not touched.
	if a2.Handle().Driver().(*callRecordingASDriver).calls.Load() != 0 {
		t.Fatalf("codex AS should not have been New()ed: calls=%d",
			a2.Handle().Driver().(*callRecordingASDriver).calls.Load())
	}
	if a1.Handle() == nil {
		t.Fatalf("a1.Handle() nil unexpectedly")
	}
}

// TestNewActiveAgentSessions_DetachedSkipped verifies that AgentSessions
// that are NOT Running are NOT triggered into a lazy spawn (F-34 §6 Q-N4
// product clarification) AND that their stale SessionID is cleared so the
// next spawn will not resurrect a dead session (F-42 §5.4).
//
// F-42 changes the counting semantics: dead/detached entries now count
// as matched AND reset (with Action="marked-fresh") — the previous
// silently-skip behavior was a bug because the stale SessionID would be
// replayed on the next spawn.
func TestNewActiveAgentSessions_DetachedSkipped(t *testing.T) {
	cs, _ := New("chat-skip", "cc", newTestChannel())
	cwd := t.TempDir()
	if err := cs.SetSelectedCwd(cwd); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	// Detached entry: SetExited clears PID; Status returns StatusExited.
	a := NewAgentSession(newAgentSessionID(), cs.ID, "cc", cwd, nil)
	a.SetSessionID("dead-session-id-456") // populated as if a previous run
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
	if got := a.SessionID(); got != "" {
		t.Fatalf("SessionID should be cleared, got %q", got)
	}
}

// TestNewActiveAgentSessions_CwdFilter verifies only the activeCwd
// entries are touched (other cwd entries are left alone).
func TestNewActiveAgentSessions_CwdFilter(t *testing.T) {
	cs, _ := New("chat-cwd", "cc", newTestChannel())
	cwd1 := t.TempDir()
	cwd2 := t.TempDir()
	if err := cs.SetSelectedCwd(cwd1); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	other := injectAS(t, cs, "cc", cwd2, (&callRecordingAS{fakeAgentSession: newFakeAgentSession(1)}).buildLive())
	here := injectAS(t, cs, "cc", cwd1, (&callRecordingAS{fakeAgentSession: newFakeAgentSession(2)}).buildLive())

	matched, reset, _, err := cs.NewActiveAgentSessions(context.Background(), "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if matched != 1 || reset != 1 {
		t.Fatalf("want (1,1), got (%d,%d)", matched, reset)
	}
	if other.Handle().Driver().(*callRecordingASDriver).calls.Load() != 0 {
		t.Fatalf("other-cwd AS should not have been New()ed: calls=%d",
			other.Handle().Driver().(*callRecordingASDriver).calls.Load())
	}
	if here.Handle().Driver().(*callRecordingASDriver).calls.Load() != 1 {
		t.Fatalf("activeCwd AS should have been New()ed once: calls=%d",
			here.Handle().Driver().(*callRecordingASDriver).calls.Load())
	}
}

// TestNewActiveAgentSessions_PartialFailure verifies that one
// failing New returns matched > reset + non-nil err, and that
// InputBuffer is still cleared.
func TestNewActiveAgentSessions_PartialFailure(t *testing.T) {
	cs, _ := New("chat-fail", "cc", newTestChannel())
	cwd := t.TempDir()
	if err := cs.SetSelectedCwd(cwd); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	injectAS(t, cs, "cc", cwd, (&callRecordingAS{fakeAgentSession: newFakeAgentSession(1)}).buildLive())
	injectAS(t, cs, "codex", cwd, (&failingNewAS{fakeAgentSession: newFakeAgentSession(2)}).buildLive())
	if err := cs.QueueUserMessage(makeTestMessage(cs,
		[]agent.ContentBlock{{Type: agent.ContentText, Text: "x"}}, "u1")); err != nil {
		t.Fatalf("QueueUserMessage: %v", err)
	}

	matched, reset, _, err := cs.NewActiveAgentSessions(context.Background(), "")
	if !errors.Is(err, errInjected) {
		t.Fatalf("want errInjected, got %v", err)
	}
	if matched != 2 || reset != 1 {
		t.Fatalf("want (2,1), got (%d,%d)", matched, reset)
	}
	// Even on partial failure the queue is preserved — /new never
	// discards queued work.
	if got := cs.QueueLen(); got != 1 {
		t.Fatalf("queue must survive a partially-failed /new: want 1, got %d", got)
	}
}

// TestAgentSession_New_Delegate verifies the wrapper passes the call
// through to the bridge handle, and returns ErrNotRunning when the
// AgentSession.New is intentionally thin: it just delegates to
// handle.New(ctx). The PTY fallback (kill+respawn on
// ErrRestartRequired) lives at the chat layer — see
// TestChatSession_RestartAgentSession.
func TestAgentSession_New_Delegate(t *testing.T) {
	t.Run("running delegates", func(t *testing.T) {
		cs, _ := New("chat-running", "cc", newTestChannel())
		cwd := t.TempDir()
		cs.SetSelectedCwd(cwd)
		bh := (&callRecordingAS{fakeAgentSession: newFakeAgentSession(1)}).buildLive()
		as := injectAS(t, cs, "cc", cwd, bh)

		if err := as.New(context.Background()); err != nil {
			t.Fatalf("New: %v", err)
		}
		if bh.Driver().(*callRecordingASDriver).calls.Load() != 1 {
			t.Fatalf("bridge New should be called once, got %d", bh.Driver().(*callRecordingASDriver).calls.Load())
		}
	})

	t.Run("detached returns ErrNotRunning", func(t *testing.T) {
		cs, _ := New("chat-detached", "cc", newTestChannel())
		cwd := t.TempDir()
		cs.SetSelectedCwd(cwd)
		// Construct an AS whose handle is nil: use NewAgentSession
		// (no SetRunning, no handle attached).
		as := NewAgentSession(newAgentSessionID(), cs.ID, "cc", cwd, nil)
		cs.mu.Lock()
		cs.pool[agentCwdKey{Agent: "cc", Cwd: cwd}] = as
		cs.mu.Unlock()

		err := as.New(context.Background())
		if !errors.Is(err, ErrNotRunning) {
			t.Fatalf("want ErrNotRunning, got %v", err)
		}
	})

	t.Run("bridge-returns-ErrRestartRequired-propagates", func(t *testing.T) {
		// AgentSession.New no longer has a fallback — the caller
		// (ChatSession.restartAgentSession) catches
		// ErrRestartRequired and does the kill+respawn itself.
		cs, _ := New("chat-restart", "cc", newTestChannel())
		cwd := t.TempDir()
		cs.SetSelectedCwd(cwd)
		oldSpy := &restartErrAS{fakeAgentSession: newFakeAgentSession(1)}
		old := oldSpy.buildLive()
		as := injectAS(t, cs, "cc", cwd, old)

		err := as.New(context.Background())
		if !errors.Is(err, agent.ErrRestartRequired) {
			t.Fatalf("want ErrRestartRequired, got %v", err)
		}
		// AgentSession.New must NOT have touched the handle — the
		// chat layer owns the kill+respawn.
		if oldSpy.closed {
			t.Fatalf("AgentSession.New must not Close() the handle; that's the chat layer's job")
		}
		if got := as.Handle(); got == nil {
			t.Fatalf("handle should still be attached after a returned ErrRestartRequired")
		}
	})
}

// TestChatSession_RestartAgentSession exercises the chat-layer PTY
// fallback. AgentSession.New returns ErrRestartRequired for bridges
// that can't do in-place reset; ChatSession.restartAgentSession
// catches that and does the kill+respawn itself.
func TestChatSession_RestartAgentSession(t *testing.T) {
	t.Run("PTY fallback closes old + spawns new + clears sessionID", func(t *testing.T) {
		// F-34 + product clarification 2026-08-04: when the bridge
		// (e.g. raw pty) cannot do an in-place reset and returns
		// agent.ErrRestartRequired, the chat layer must close the
		// old handle and spawn a fresh one via the Spawner, with
		// SessionID cleared.
		cs, _ := New("chat-restart", "cc", newTestChannel())
		cwd := t.TempDir()
		cs.SetSelectedCwd(cwd)
		oldSpy := &restartErrAS{fakeAgentSession: newFakeAgentSession(1)}
		old := oldSpy.buildLive()
		as := injectAS(t, cs, "cc", cwd, old)
		as.SetSessionID("stale-id-should-be-cleared")

		newAS := (&callRecordingAS{fakeAgentSession: newFakeAgentSession(2)}).buildLive()
		spawner := &fakeRestartSpawner{handle: newAS}
		cs = cs.WithSpawner(spawner)

		if err := cs.restartAgentSession(context.Background(), as); err != nil {
			t.Fatalf("restartAgentSession: %v", err)
		}
		if !oldSpy.closed {
			t.Fatalf("old handle should have been Close()d")
		}
		if got := as.Handle(); got != newAS {
			t.Fatalf("handle not swapped: got %T", got)
		}
		if got := as.SessionID(); got != "" {
			t.Fatalf("SessionID should be cleared, got %q", got)
		}
		if newAS.Driver().(*callRecordingASDriver).calls.Load() != 0 {
			t.Fatalf("newAS should not have been New()ed (it's the freshly-spawned handle)")
		}
		if got := as.PID(); got != 2 {
			t.Fatalf("PID should be 2 (newFakeAgentSession), got %d", got)
		}
		if spawner.calledWithResumeID != "" {
			t.Fatalf("Spawn should be called with empty sessionID, got %q", spawner.calledWithResumeID)
		}
	})

	t.Run("no spawner configured propagates ErrRestartRequired", func(t *testing.T) {
		// Chat constructed WITHOUT WithSpawner (e.g. unit tests that
		// only exercise the in-place path) — restartAgentSession
		// can't fall back, so it returns the original sentinel for
		// the caller to handle / surface.
		cs, _ := New("chat-restart-no-spawner", "cc", newTestChannel())
		cwd := t.TempDir()
		cs.SetSelectedCwd(cwd)
		oldSpy := &restartErrAS{fakeAgentSession: newFakeAgentSession(1)}
		old := oldSpy.buildLive()
		as := injectAS(t, cs, "cc", cwd, old)

		err := cs.restartAgentSession(context.Background(), as)
		if !errors.Is(err, agent.ErrRestartRequired) {
			t.Fatalf("want ErrRestartRequired, got %v", err)
		}
		if oldSpy.closed {
			t.Fatalf("old handle should NOT have been Close()d when no spawner available")
		}
	})

	t.Run("respawn-spawn-failure-cleans-up-status", func(t *testing.T) {
		// F-34 Phase 3 review #4: when bridge.New returns
		// ErrRestartRequired AND spawner.Spawn fails, the
		// fallback must mark the AS Exited so subsequent
		// LookupSelectedAgentSession lazy-spawns a fresh one.
		// Previously status stayed Running with handle=nil → next
		// SendBlocks returned ErrNotRunning.
		cs, _ := New("chat-respawn-fail", "cc", newTestChannel())
		cwd := t.TempDir()
		cs.SetSelectedCwd(cwd)
		oldSpy := &restartErrAS{fakeAgentSession: newFakeAgentSession(1)}
		old := oldSpy.buildLive()
		as := injectAS(t, cs, "cc", cwd, old)
		as.SetSessionID("stale-id")

		failingSpawner := &fakeFailingSpawner{err: errors.New("spawn blew up")}
		cs = cs.WithSpawner(failingSpawner)
		err := cs.restartAgentSession(context.Background(), as)
		if err == nil {
			t.Fatalf("restartAgentSession should have failed")
		}
		if got := as.Status(); got != StatusExited {
			t.Fatalf("status = %s, want StatusExited", got)
		}
		if got := as.Handle(); got != nil {
			t.Fatalf("handle should be nil after spawn failure, got %T", got)
		}
		if got := as.SessionID(); got != "" {
			t.Fatalf("SessionID should be cleared, got %q", got)
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

// buildLive wraps r in a *agent.Agent so its overridden New
// (which returns ErrRestartRequired) is what driver.Reset dispatches.
func (r *restartErrAS) buildLive() *agent.Agent {
	return agent.NewAgent(
		agent.NewInfo("fake", agent.ModePTY, "fake", nil, nil),
		r.pid, r.events,
		&restartErrASDriver{inner: r})
}

// restartErrASDriver forwards driver calls to a restartErrAS.
type restartErrASDriver struct{ inner *restartErrAS }

func (d *restartErrASDriver) SendBlocks(ctx context.Context, b []agent.ContentBlock) error {
	return d.inner.SendBlocks(ctx, b)
}
func (d *restartErrASDriver) SendPermission(resp string) error {
	return d.inner.SendPermission(resp)
}
func (d *restartErrASDriver) Reset(ctx context.Context) error { return d.inner.New(ctx) }
func (d *restartErrASDriver) Stop(ctx context.Context) error { return d.inner.Stop(ctx) }
func (d *restartErrASDriver) SetModel(ctx context.Context, providerID, modelID string) error {
	return d.inner.SetModel(ctx, providerID, modelID)
}
func (d *restartErrASDriver) Close() error                   { return d.inner.Close() }

// fakeRestartSpawner is a minimal chatsession.Spawner that returns a
// pre-built handle. The wrapper only needs Spawn(ctx, name, cwd, args,
// sessionID); we record the sessionID it was called with so tests can
// assert "no --resume on the fresh spawn".
type fakeRestartSpawner struct {
	handle             *agent.Agent
	calledWithResumeID string
}

func (f *fakeRestartSpawner) Spawn(_ context.Context, _, _ string, _ []string, sessionID string) (*agent.Agent, error) {
	f.calledWithResumeID = sessionID
	return f.handle, nil
}

// fakeFailingSpawner returns a non-nil error from Spawn, used to
// exercise the wrapper's spawn-failure cleanup path (F-34 review #4).
type fakeFailingSpawner struct{ err error }

func (f *fakeFailingSpawner) Spawn(_ context.Context, _, _ string, _ []string, _ string) (*agent.Agent, error) {
	return nil, f.err
}

// F-42 tests for the dead/detached branch of NewActiveAgentSessions.
// These lock the new behavior introduced by F-42 §5.4: dead entries
// are NOT silently skipped — their stale SessionID is cleared
// (in-memory + persisted) so the next spawn will not resurrect a
// dead session via --resume <dead-id>.

// TestNewActiveAgentSessions_DeadEntryClearsResumeIDInMemory verifies
// that a dead entry's SessionID is cleared in-memory after /new.
func TestNewActiveAgentSessions_DeadEntryClearsResumeIDInMemory(t *testing.T) {
	cs, _ := New("chat-dead-mem", "cc", newTestChannel())
	cwd := t.TempDir()
	if err := cs.SetSelectedCwd(cwd); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	// A dead entry with a stale SessionID from a previous run.
	a := NewAgentSession(newAgentSessionID(), cs.ID, "cc", cwd, nil)
	a.SetSessionID("claude-sess-dead-123")
	a.SetExited(0)
	cs.mu.Lock()
	cs.pool[agentCwdKey{Agent: "cc", Cwd: cwd}] = a
	cs.mu.Unlock()

	if got := a.SessionID(); got != "claude-sess-dead-123" {
		t.Fatalf("precondition: want SessionID=%q, got %q", "claude-sess-dead-123", got)
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
	if got := a.SessionID(); got != "" {
		t.Errorf("SessionID should be cleared in-memory: got %q", got)
	}
}

// TestNewActiveAgentSessions_DeadEntryPersistsClearedResumeID verifies
// that the cleared SessionID is persisted to agent_sessions.json so the
// next spawn will not replay the old value.
func TestNewActiveAgentSessions_DeadEntryPersistsClearedResumeID(t *testing.T) {
	cs, _ := New("chat-dead-persist", "cc", newTestChannel())
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
	a.SetSessionID("claude-sess-dead-xyz")
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
		t.Fatalf("entry should still be in the registry (F-42 §5.5: keep entry, clear SessionID)")
	}
	if entry.SessionID != "" {
		t.Errorf("persisted SessionID should be cleared: got %q", entry.SessionID)
	}
}

// TestNewActiveAgentSessions_DeadEntryDoesNotSpawn locks the F-34
// §6 Q-N4 product clarification: dead entries must NOT trigger a
// lazy spawn just to reset their conversation.
func TestNewActiveAgentSessions_DeadEntryDoesNotSpawn(t *testing.T) {
	cs, _ := New("chat-dead-no-spawn", "cc", newTestChannel())
	cwd := t.TempDir()
	if err := cs.SetSelectedCwd(cwd); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	as := newFakeAgentSession(99)
	spy := &fakeRestartSpawner{handle: as.buildLive()}
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
	cs, _ := New("chat-mixed", "cc", newTestChannel())
	cwd := t.TempDir()
	if err := cs.SetSelectedCwd(cwd); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)

	live := injectAS(t, cs, "cc", cwd, (&callRecordingAS{fakeAgentSession: newFakeAgentSession(1)}).buildLive())
	dead := NewAgentSession(newAgentSessionID(), cs.ID, "codex", cwd, nil)
	dead.SetSessionID("codex-sess-dead-789")
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

	// Dead entry's SessionID is cleared.
	if got := dead.SessionID(); got != "" {
		t.Errorf("dead SessionID should be cleared: got %q", got)
	}
	// Live entry's bridge.New was called once.
	if live.Handle().Driver().(*callRecordingASDriver).calls.Load() != 1 {
		t.Errorf("live agent New() should have been called once: got %d",
			live.Handle().Driver().(*callRecordingASDriver).calls.Load())
	}
}

// TestNewActiveAgentSessions_ResultsSliceHasEveryEntry locks that the
// result slice length matches the matched count (1:1 mapping).
func TestNewActiveAgentSessions_ResultsSliceHasEveryEntry(t *testing.T) {
	cs, _ := New("chat-result-len", "cc", newTestChannel())
	cwd := t.TempDir()
	if err := cs.SetSelectedCwd(cwd); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	cs.WithPersistence(nil, nil)
	injectAS(t, cs, "cc", cwd, (&callRecordingAS{fakeAgentSession: newFakeAgentSession(1)}).buildLive())
	injectAS(t, cs, "codex", cwd, (&callRecordingAS{fakeAgentSession: newFakeAgentSession(2)}).buildLive())

	matched, _, results, err := cs.NewActiveAgentSessions(context.Background(), "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(results) != matched {
		t.Errorf("len(results)=%d != matched=%d (1:1 invariant)", len(results), matched)
	}
}