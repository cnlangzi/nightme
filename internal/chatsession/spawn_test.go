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

// fakeAgentSession is a minimal agent.AgentSession implementation
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

func (f *fakeAgentSession) SendText(text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("fake: closed")
	}
	return nil
}

func (f *fakeAgentSession) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
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

// FinishEvent delivers EventDone and closes the channel.
func (f *fakeAgentSession) FinishEvent() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.events <- agent.AgentEvent{Kind: agent.EventDone}
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
	spawnFn      func(name, cwd string) (agent.AgentSession, error) // optional override
}

type spawnKey struct {
	name string
	cwd  string
}

func newFakeSpawner() *fakeSpawner {
	return &fakeSpawner{fakes: make(map[spawnKey]*fakeAgentSession)}
}

func (s *fakeSpawner) Spawn(ctx context.Context, name, cwd string, args []string, resumeID string) (agent.AgentSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastResumeID = resumeID
	if s.spawnFn != nil {
		return s.spawnFn(name, cwd)
	}
	key := spawnKey{name, cwd}
	if f, ok := s.fakes[key]; ok {
		return f, nil
	}
	f := newFakeAgentSession(10000 + s.calls)
	s.fakes[key] = f
	return f, nil
}

func (s *fakeSpawner) Get(name, cwd string) *fakeAgentSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakes[spawnKey{name, cwd}]
}

// --- Tests ---

func TestAgentSession_SpawnViaSpawner(t *testing.T) {
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude").
		WithPersistence(csFile, asFile).
		WithSpawner(newFakeSpawner())

	cs.SetActiveCwd("/code/bailing")
	cs.SetActiveAgent("claude")

	as, err := cs.LookupActiveAgentSession()
	if err != nil {
		t.Fatalf("LookupActiveAgentSession: %v", err)
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
	cs := New("oc_xxx", "claude").
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)

	cs.SetActiveCwd("/x")
	cs.SetActiveAgent("claude")

	as1, _ := cs.LookupActiveAgentSession()
	as2, _ := cs.LookupActiveAgentSession()

	if as1.ID != as2.ID {
		t.Fatalf("two lookups should resolve to same AgentSession")
	}
	if as1.Handle() != as2.Handle() {
		t.Fatalf("Handle should be the same instance (no respawn)")
	}
}

func TestAgentSession_SpawnFailureLeavesDetached(t *testing.T) {
	spawner := newFakeSpawner()
	spawner.spawnFn = func(name, cwd string) (agent.AgentSession, error) {
		return nil, errors.New("spawn boom")
	}
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude").
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)

	cs.SetActiveCwd("/x")
	cs.SetActiveAgent("claude")

	as, err := cs.LookupActiveAgentSession()
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
	cs := New("oc_xxx", "claude").
		WithPersistence(csFile, asFile)
	// no WithSpawner

	cs.SetActiveCwd("/x")
	cs.SetActiveAgent("claude")

	as, err := cs.LookupActiveAgentSession()
	if err != nil {
		t.Fatalf("LookupActiveAgentSession: %v", err)
	}
	if as.Status() != StatusDetached {
		t.Fatalf("without Spawner: got %q, want Detached", as.Status())
	}
	if as.Handle() != nil {
		t.Fatalf("Handle should be nil when no Spawner wired")
	}
}

func TestAgentSession_SendTextBeforeSpawn(t *testing.T) {
	as := NewAgentSession("as_1", "cs_xxx", "claude", "/x", nil)
	if err := as.SendText("hi"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("expected ErrNotRunning, got %v", err)
	}
}

func TestAgentSession_SendTextAfterSpawn(t *testing.T) {
	spawner := newFakeSpawner()
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude").
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)

	cs.SetActiveCwd("/x")
	cs.SetActiveAgent("claude")

	as, _ := cs.LookupActiveAgentSession()
	if err := as.SendText("hello"); err != nil {
		t.Fatalf("SendText after spawn: %v", err)
	}
}

func TestAgentSession_ObserveCloseTransitionsToExited(t *testing.T) {
	spawner := newFakeSpawner()
	csFile, asFile := newTestStores(t)
	cs := New("oc_xxx", "claude").
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)

	cs.SetActiveCwd("/x")
	cs.SetActiveAgent("claude")
	as, _ := cs.LookupActiveAgentSession()

	// Drain a few events then finish.
	spawner.Get("claude", "/x").PushEvent(agent.AgentEvent{Kind: agent.EventText, Text: "hi"})
	spawner.Get("claude", "/x").PushEvent(agent.AgentEvent{Kind: agent.EventToolStart, ToolStart: &agent.ToolStartEvent{ID: "t1", Name: "Bash"}})
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

// TestAgentSession_ResumeIDRoundTrip asserts that SetResumeID is
// idempotent, accessible via ResumeID(), and survives a round-trip
// through the Entry-derived registry form.
func TestAgentSession_ResumeIDRoundTrip(t *testing.T) {
	as := NewAgentSession("as_1", "cs_1", "claude", "/x", nil)

	if got := as.ResumeID(); got != "" {
		t.Errorf("initial ResumeID = %q, want empty", got)
	}

	as.SetResumeID("sess-abc")
	if got := as.ResumeID(); got != "sess-abc" {
		t.Errorf("after set: ResumeID = %q, want %q", got, "sess-abc")
	}

	entry := as.Entry()
	if entry.ResumeID != "sess-abc" {
		t.Errorf("Entry().ResumeID = %q, want %q", entry.ResumeID, "sess-abc")
	}

	// Round-trip through FromAgentSessionEntry.
	restored := FromAgentSessionEntry(entry)
	if got := restored.ResumeID(); got != "sess-abc" {
		t.Errorf("after FromAgentSessionEntry: ResumeID = %q, want %q", got, "sess-abc")
	}
}

// TestAgentSession_RespawnPassesResumeID asserts that after the
// AgentSession captures a resume id (via SetResumeID), a subsequent
// spawn calls the Spawner with that resume id so the bridge can
// resume the previous session.
func TestAgentSession_RespawnPassesResumeID(t *testing.T) {
	csFile, asFile := newTestStores(t)
	spawner := newFakeSpawner()

	cs := New("oc_1", "claude").
		WithPersistence(csFile, asFile).
		WithSpawner(spawner)
	if err := cs.SetActiveCwd("/x"); err != nil {
		t.Fatalf("SetActiveCwd: %v", err)
	}
	if err := cs.SetActiveAgent("claude"); err != nil {
		t.Fatalf("SetActiveAgent: %v", err)
	}

	as, err := cs.LookupActiveAgentSession()
	if err != nil {
		t.Fatalf("first Lookup: %v", err)
	}
	if as.ResumeID() != "" {
		t.Errorf("fresh AgentSession should not have a resume id")
	}

	// Capture a resume id (simulating AgentConnected being handled).
	as.SetResumeID("sess-resume-1")

	// Simulate the process exiting; the next spawn should observe
	// the saved resume id.
	as.SetExited(0)

	// Spawn again — but the in-memory AgentSession has its resume
	// id cleared on SetExited? No: SetExited only flips status and
	// pid; the resume id is preserved so a subsequent respawn can
	// forward it. (RestoreFromRegistry does NOT demote resume id.)
	if got := as.ResumeID(); got != "sess-resume-1" {
		t.Errorf("resume id should survive SetExited; got %q", got)
	}

	// Clear the in-memory handle so Spawn goes through the spawner.
	as = NewAgentSession(as.ID, as.ChatSessionID, as.Agent, as.Cwd, as.Args())
	as.SetResumeID("sess-resume-1")
	if err := as.Spawn(context.Background(), spawner); err != nil {
		t.Fatalf("respawn: %v", err)
	}

	if got := spawner.lastResumeID; got != "sess-resume-1" {
		t.Errorf("Spawn received resumeID = %q, want %q", got, "sess-resume-1")
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
		t.Errorf("Spawn received resumeID = %q, want empty", got)
	}
}

// TestAgentSession_ResumeIDRestoreFromRegistry asserts that a
// persisted AgentSessionEntry with a ResumeID is correctly restored
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
		ResumeID:      "sess-round-trip-xyz",
		CreatedAt:     now,
		LastRunAt:     now,
	}
	as := FromAgentSessionEntry(e)
	if as == nil {
		t.Fatal("FromAgentSessionEntry returned nil")
	}
	if got := as.ResumeID(); got != "sess-round-trip-xyz" {
		t.Errorf("ResumeID = %q, want %q", got, "sess-round-trip-xyz")
	}
	// And the next spawn forwards the resume id to the spawner.
	spawner := newFakeSpawner()
	if err := as.Spawn(context.Background(), spawner); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if got := spawner.lastResumeID; got != "sess-round-trip-xyz" {
		t.Errorf("Spawn received resumeID = %q, want %q", got, "sess-round-trip-xyz")
	}
}