package chatsession

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/registry"

	"github.com/cnlangzi/nightme/internal/agent"
)

// fakeAgentSession is a minimal agent.Agent implementation
// used by tests. It does not spawn anything; the caller injects
// events into the channel via PushEvent.
type fakeAgentSession struct {
	mu     sync.Mutex
	pid    int
	events chan agent.AgentEvent
	closed bool
}

func newFakeAgentSession(pid int) *fakeAgentSession {
	return &fakeAgentSession{
		pid:    pid,
		events: make(chan agent.AgentEvent, 32),
	}
}

func (f *fakeAgentSession) Events() <-chan agent.AgentEvent { return f.events }
func (f *fakeAgentSession) PID() int                      { return f.pid }
func (f *fakeAgentSession) Info() agent.Info {
	return agent.NewInfo("fake", agent.ModePTY, "fake", nil, nil)
}
func (f *fakeAgentSession) Detect() error { return nil }
func (f *fakeAgentSession) Start(_ context.Context, _ agent.StartConfig) (*agent.Agent, error) {
	return f.buildLive(), nil
}

// buildLive wraps f in a *agent.Agent with a fake driver that
// forwards Send*/Reset/Close back to f. Used by fakeSpawners to
// return a *Agent from Spawn().
func (f *fakeAgentSession) buildLive() *agent.Agent {
	return agent.NewAgent(
		agent.NewInfo("fake", agent.ModePTY, "fake", nil, nil),
		f.pid, f.events, &fakeDriver{inner: f})
}

// fakeDriver forwards driver calls back to a fakeAgentSession.
type fakeDriver struct{ inner *fakeAgentSession }

func (d *fakeDriver) SendBlocks(ctx context.Context, b []agent.ContentBlock) error {
	return d.inner.SendBlocks(ctx, b)
}
func (d *fakeDriver) SendPermission(resp string) error { return d.inner.SendPermission(resp) }
func (d *fakeDriver) Reset(ctx context.Context) error    { return d.inner.New(ctx) }
func (d *fakeDriver) Stop(ctx context.Context) error    { return d.inner.Stop(ctx) }
func (d *fakeDriver) SetModel(ctx context.Context, providerID, modelID string) error {
	return d.inner.SetModel(ctx, providerID, modelID)
}
func (d *fakeDriver) Close() error                      { return d.inner.Close() }

func (f *fakeAgentSession) SendBlocks(ctx context.Context, b []agent.ContentBlock) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("fake: closed")
	}
	return nil
}

func (f *fakeAgentSession) SendPermission(resp string) error {
	return nil
}

func (f *fakeAgentSession) New(_ context.Context) error { return nil }
func (f *fakeAgentSession) Stop(_ context.Context) error { return agent.ErrNotSupported }
func (f *fakeAgentSession) SetModel(_ context.Context, _, _ string) error {
	return agent.ErrNotSupported
}

func (f *fakeAgentSession) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.events)
	}
	return nil
}

// PushEvent delivers an event to the channel (used by tests to
// simulate agent output).
func (f *fakeAgentSession) PushEvent(ev agent.AgentEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.events <- ev
}

// FinishEvent delivers EventAgentDone and closes the channel.
func (f *fakeAgentSession) FinishEvent() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.events <- agent.AgentEvent{Kind: agent.EventAgentDone}
	f.closed = true
	close(f.events)
}

// fakeSpawner is a Spawner that returns fakeAgentSession instances
// without forking. The test can PushEvent / FinishEvent to drive
// lifecycle transitions.
type fakeSpawner struct {
	mu           sync.Mutex
	fakes        map[spawnKey]*fakeAgentSession
	calls        int
	lastResumeID string
	spawnFn      func(name, cwd string) (*agent.Agent, error) // optional override
}

type spawnKey struct {
	name string
	cwd  string
}

func newFakeSpawner() *fakeSpawner {
	return &fakeSpawner{fakes: make(map[spawnKey]*fakeAgentSession)}
}

func (s *fakeSpawner) Spawn(ctx context.Context, name, cwd string, args []string, sessionID string) (*agent.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastResumeID = sessionID
	if s.spawnFn != nil {
		return s.spawnFn(name, cwd)
	}
	key := spawnKey{name, cwd}
	if f, ok := s.fakes[key]; ok {
		return f.buildLive(), nil
	}
	f := newFakeAgentSession(10000 + s.calls)
	s.fakes[key] = f
	return f.buildLive(), nil
}

func (s *fakeSpawner) Get(name, cwd string) *fakeAgentSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakes[spawnKey{name, cwd}]
}

// --- Tests ---

func TestAgentSession_SpawnViaSpawner(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	cs = cs.WithSpawner(newFakeSpawner())
	cs.SetSelectedCwd("/code/bailing")
	cs.SetSelectedAgent("claude")

	as, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatalf("LookupSelectedAgentSession: %v", err)
	}
	if as.Status() != StatusRunning {
		t.Fatalf("expected Running after spawn, got %q", as.Status())
	}
	if as.PID() == 0 {
		t.Fatalf("expected non-zero PID after spawn")
	}
	if as.Handle() == nil {
		t.Fatalf("Handle() should return the bridge handle after spawn")
	}
}

func TestAgentSession_SpawnIsIdempotent(t *testing.T) {
	spawner := newFakeSpawner()
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	cs = cs.WithSpawner(spawner)
	cs.SetSelectedCwd("/x")
	cs.SetSelectedAgent("claude")

	as1, _ := cs.LookupSelectedAgentSession()
	as2, _ := cs.LookupSelectedAgentSession()

	if as1.ID != as2.ID {
		t.Fatalf("two lookups should resolve to same AgentSession")
	}
	if as1.Handle() != as2.Handle() {
		t.Fatalf("Handle should be the same instance (no respawn)")
	}
}

func TestAgentSession_SpawnFailureLeavesDetached(t *testing.T) {
	spawner := newFakeSpawner()
	spawner.spawnFn = func(name, cwd string) (*agent.Agent, error) {
		return nil, errors.New("spawn boom")
	}
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	cs = cs.WithSpawner(spawner)
	cs.SetSelectedCwd("/x")
	cs.SetSelectedAgent("claude")

	as, err := cs.LookupSelectedAgentSession()
	if err == nil {
		t.Fatalf("expected error from spawn failure")
	}
	if !strings.Contains(err.Error(), "spawn boom") {
		t.Fatalf("error should wrap spawn error: %v", err)
	}
	if as.Status() != StatusDetached {
		t.Fatalf("after spawn failure: got %q, want Detached", as.Status())
	}
}

func TestAgentSession_NoSpawnerLeavesDetached(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	// no WithSpawner

	cs.SetSelectedCwd("/x")
	cs.SetSelectedAgent("claude")

	as, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatalf("LookupSelectedAgentSession: %v", err)
	}
	if as.Status() != StatusDetached {
		t.Fatalf("without Spawner: got %q, want Detached", as.Status())
	}
	if as.Handle() != nil {
		t.Fatalf("Handle should be nil when no Spawner wired")
	}
}

func TestAgentSession_SendBlocksBeforeSpawn(t *testing.T) {
	as := NewAgentSession("as_1", "cs_xxx", "claude", "/x", nil)
	if err := as.SendBlocks(context.Background(), []agent.ContentBlock{{Type: agent.ContentText, Text: "hi"}}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("expected ErrNotRunning, got %v", err)
	}
}

func TestAgentSession_SendBlocksAfterSpawn(t *testing.T) {
	spawner := newFakeSpawner()
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	cs = cs.WithSpawner(spawner)
	cs.SetSelectedCwd("/x")
	cs.SetSelectedAgent("claude")

	as, _ := cs.LookupSelectedAgentSession()
	if err := as.SendBlocks(context.Background(), []agent.ContentBlock{{Type: agent.ContentText, Text: "hello"}}); err != nil {
		t.Fatalf("SendBlocks after spawn: %v", err)
	}
}

func TestAgentSession_ObserveCloseTransitionsToExited(t *testing.T) {
	spawner := newFakeSpawner()
	csFile, asFile := newTestStores(t)
	cs, _ := New("oc_xxx", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	cs = cs.WithSpawner(spawner)
	cs.SetSelectedCwd("/x")
	cs.SetSelectedAgent("claude")
	as, _ := cs.LookupSelectedAgentSession()

	// F-61: AS goroutines (eventDispatchLoop + readpump) must be
	// drained before t.TempDir cleanup, otherwise a late Lifecycle
	// event triggers a persist() against the deleted tempdir.
	// Without Shutdown, t.TempDir's RemoveAll races with pending
	// persist calls → "no such file or directory" or
	// "directory not empty" errors during test cleanup.
	t.Cleanup(func() {
		as.Shutdown()
	})

	// Drain a few events then finish.
	spawner.Get("claude", "/x").PushEvent(agent.AgentEvent{Kind: agent.EventAgentText, Text: "hi"})
	spawner.Get("claude", "/x").PushEvent(agent.AgentEvent{Kind: agent.EventAgentToolStart, ToolStart: &agent.AgentToolStartEvent{ID: "t1", Name: "Bash"}})
	spawner.Get("claude", "/x").FinishEvent()

	// Start ObserveClose; it should transition Running → Exited
	// when the channel drains.
	done := as.ObserveClose()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("ObserveClose did not complete within 2s")
	}

	if as.Status() != StatusExited {
		t.Fatalf("after observe close: got %q, want Exited", as.Status())
	}
	if as.PID() != 0 {
		t.Fatalf("PID should be cleared on exit, got %d", as.PID())
	}
}

// TestAgentSession_ResumeIDRoundTrip asserts that SetSessionID is
// idempotent, accessible via SessionID(), and survives a round-trip
// through the Entry-derived registry form.
func TestAgentSession_ResumeIDRoundTrip(t *testing.T) {
	as := NewAgentSession("as_1", "cs_1", "claude", "/x", nil)

	if got := as.SessionID(); got != "" {
		t.Errorf("initial SessionID = %q, want empty", got)
	}

	as.SetSessionID("sess-abc")
	if got := as.SessionID(); got != "sess-abc" {
		t.Errorf("after set: SessionID = %q, want %q", got, "sess-abc")
	}

	entry := as.Entry()
	if entry.SessionID != "sess-abc" {
		t.Errorf("Entry().SessionID = %q, want %q", entry.SessionID, "sess-abc")
	}

	// Round-trip through FromAgentSessionEntry.
	restored := FromAgentSessionEntry(entry)
	if got := restored.SessionID(); got != "sess-abc" {
		t.Errorf("after FromAgentSessionEntry: SessionID = %q, want %q", got, "sess-abc")
	}
}

// TestAgentSession_RespawnPassesResumeID asserts that after the
// AgentSession captures a resume id (via SetSessionID), a subsequent
// spawn calls the Spawner with that resume id so the bridge can
// resume the previous session.
func TestAgentSession_RespawnPassesResumeID(t *testing.T) {
	csFile, asFile := newTestStores(t)
	spawner := newFakeSpawner()

	cs, _ := New("oc_1", "claude")
	cs = cs.WithPersistence(csFile, asFile)
	cs = cs.WithSpawner(spawner)
	if err := cs.SetSelectedCwd("/x"); err != nil {
		t.Fatalf("SetSelectedCwd: %v", err)
	}
	if err := cs.SetSelectedAgent("claude"); err != nil {
		t.Fatalf("SetSelectedAgent: %v", err)
	}

	as, err := cs.LookupSelectedAgentSession()
	if err != nil {
		t.Fatalf("first Lookup: %v", err)
	}
	if as.SessionID() != "" {
		t.Errorf("fresh AgentSession should not have a resume id")
	}

	// Capture a resume id (simulating EventAgentReady being handled).
	as.SetSessionID("sess-resume-1")

	// Simulate the process exiting; the next spawn should observe
	// the saved resume id.
	as.SetExited(0)

	// Spawn again — but the in-memory AgentSession has its resume
	// id cleared on SetExited? No: SetExited only flips status and
	// pid; the resume id is preserved so a subsequent respawn can
	// forward it. (RestoreFromRegistry does NOT demote resume id.)
	if got := as.SessionID(); got != "sess-resume-1" {
		t.Errorf("resume id should survive SetExited; got %q", got)
	}

	// Clear the in-memory handle so Spawn goes through the spawner.
	as = NewAgentSession(as.ID, as.ChatSessionID, as.Agent, as.Cwd, as.Args())
	as.SetSessionID("sess-resume-1")
	if err := as.Spawn(context.Background(), spawner); err != nil {
		t.Fatalf("respawn: %v", err)
	}

	if got := spawner.lastResumeID; got != "sess-resume-1" {
		t.Errorf("Spawn received sessionID = %q, want %q", got, "sess-resume-1")
	}
}

// TestAgentSession_EmptyResumeIDPassesEmpty asserts that the Spawner
// receives an empty resume id (no `--resume`) when the AgentSession
// has no captured id.
func TestAgentSession_EmptyResumeIDPassesEmpty(t *testing.T) {
	as := NewAgentSession("as_1", "cs_1", "claude", "/x", nil)
	spawner := newFakeSpawner()
	if err := as.Spawn(context.Background(), spawner); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if got := spawner.lastResumeID; got != "" {
		t.Errorf("Spawn received sessionID = %q, want empty", got)
	}
}

// TestAgentSession_ResumeIDRestoreFromRegistry asserts that a
// persisted AgentSessionEntry with a SessionID is correctly restored
// by FromAgentSessionEntry. This is the path that survives a daemon
// restart — the resume id must round-trip so the next respawn can
// replay `--resume <id>`.
func TestAgentSession_ResumeIDRestoreFromRegistry(t *testing.T) {
	now := time.Now()
	e := &registry.AgentSessionEntry{
		ID:            "as_1",
		ChatSessionID: "cs_1",
		Agent:         "claude",
		Cwd:           "/x",
		PID:           0,
		Status:        registry.StatusDetached,
		SessionID:      "sess-round-trip-xyz",
		CreatedAt:     now,
		LastRunAt:     now,
	}
	as := FromAgentSessionEntry(e)
	if as == nil {
		t.Fatal("FromAgentSessionEntry returned nil")
	}
	if got := as.SessionID(); got != "sess-round-trip-xyz" {
		t.Errorf("SessionID = %q, want %q", got, "sess-round-trip-xyz")
	}
	// And the next spawn forwards the resume id to the spawner.
	spawner := newFakeSpawner()
	if err := as.Spawn(context.Background(), spawner); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if got := spawner.lastResumeID; got != "sess-round-trip-xyz" {
		t.Errorf("Spawn received sessionID = %q, want %q", got, "sess-round-trip-xyz")
	}
}