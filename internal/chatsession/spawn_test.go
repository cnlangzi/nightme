package chatsession

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

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
	mu      sync.Mutex
	fakes   map[spawnKey]*fakeAgentSession
	calls   int
	spawnFn func(name, cwd string) (agent.AgentSession, error) // optional override
}

type spawnKey struct {
	name string
	cwd  string
}

func newFakeSpawner() *fakeSpawner {
	return &fakeSpawner{fakes: make(map[spawnKey]*fakeAgentSession)}
}

func (s *fakeSpawner) Spawn(ctx context.Context, name, cwd string, args []string) (agent.AgentSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
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