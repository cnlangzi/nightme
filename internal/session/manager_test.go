package session

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/registry"
)

// fakeAgent is a minimal in-process Agent used by the Manager tests.
// Start produces a fakeAgentSession that emits a configurable list
// of events and then closes.
type fakeAgent struct {
	name    string
	mode    agent.Mode
	events  []agent.AgentEvent
	detectF func() error
	pid     int
}

func (f *fakeAgent) Name() string  { return f.name }
func (f *fakeAgent) Mode() agent.Mode { return f.mode }
func (f *fakeAgent) Detect() error {
	if f.detectF != nil {
		return f.detectF()
	}
	return nil
}
func (f *fakeAgent) Start(context.Context, agent.StartConfig) (agent.AgentSession, error) {
	return &fakeAgentSession{events: f.events, pid: f.pid}, nil
}

// fakeAgentSession is the AgentSession returned by fakeAgent.Start.
// Events are queued from a slice and the channel closes when the
// slice is drained. SendText / SendPermission / Close are no-ops
// sufficient for the tests.
type fakeAgentSession struct {
	mu        sync.Mutex
	events    []agent.AgentEvent
	channel   chan agent.AgentEvent
	started   bool
	closed    bool
	sendErr   error
	closeErr  error
	pid       int
}

func (s *fakeAgentSession) ensureStarted() {
	if s.started {
		return
	}
	s.started = true
	s.channel = make(chan agent.AgentEvent, len(s.events)+1)
	go func() {
		defer close(s.channel)
		for _, ev := range s.events {
			s.channel <- ev
		}
	}()
}

func (s *fakeAgentSession) Events() <-chan agent.AgentEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureStarted()
	return s.channel
}

func (s *fakeAgentSession) SendText(string) error     { return s.sendErr }
func (s *fakeAgentSession) SendPermission(string) error { return s.sendErr }
func (s *fakeAgentSession) PID() int                  { return s.pid }
func (s *fakeAgentSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.started && s.channel != nil {
		// Closing the channel unblocks any consumer; consumers must
		// be tolerant of an early close (the real implementation
		// already handles this).
		close(s.channel)
	}
	return s.closeErr
}

// TestCreateAndGet exercises the happy path: Create registers a
// session, Get and GetByChat both find it, List returns it, and the
// callback observes every event from the underlying agent.
func TestCreateAndGet(t *testing.T) {
	reg := agent.New()
	reg.Register(&fakeAgent{
		name: "claude",
		mode: agent.ModePTY,
		events: []agent.AgentEvent{
			{Kind: agent.EventText, Text: "hello"},
			{Kind: agent.EventDone, Done: &agent.DoneEvent{ExitCode: 0}},
		},
	})

	var seen []agent.AgentEvent
	var mu sync.Mutex
	mgr := NewMemoryManager(reg, nil, func(s *Session, ev agent.AgentEvent) {
		mu.Lock()
		seen = append(seen, ev)
		mu.Unlock()
	})

	sess, err := mgr.Create(context.Background(), CreateRequest{
		ChatID:    "chat-1",
		Workspace: t.TempDir(),
		Agent:     "claude",
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if sess.Status() != StatusRunning {
		t.Fatalf("Status() = %s, want running", sess.Status())
	}
	if sess.ID == "" {
		t.Fatalf("ID is empty")
	}

	got, err := mgr.GetByChat("chat-1")
	if err != nil {
		t.Fatalf("GetByChat returned error: %v", err)
	}
	if got.ID != sess.ID {
		t.Fatalf("GetByChat returned ID %s, want %s", got.ID, sess.ID)
	}

	got, err = mgr.Get(sess.ID)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if got.ChatID != "chat-1" {
		t.Fatalf("Get returned ChatID %s, want chat-1", got.ChatID)
	}

	// Wait for the readPump to drain both events and transition to
	// StatusExited (EventDone triggers the exit).
	if !waitFor(2*time.Second, func() bool {
		s, _ := mgr.Get(sess.ID)
		return s != nil && s.Status() == StatusExited
	}) {
		t.Fatalf("session did not reach StatusExited after EventDone")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("callback saw %d events, want at least 2", len(seen))
	}
	if seen[0].Kind != agent.EventText || seen[0].Text != "hello" {
		t.Fatalf("first event = %+v, want text/hello", seen[0])
	}
	if seen[1].Kind != agent.EventDone {
		t.Fatalf("second event = %+v, want done", seen[1])
	}
}

// TestGetByChatNotFound verifies the not-found error is the package
// sentinel so callers can use errors.Is.
func TestGetByChatNotFound(t *testing.T) {
	mgr := NewMemoryManager(agent.New(), nil, nil)
	if _, err := mgr.GetByChat("nope"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("GetByChat unknown error = %v, want ErrSessionNotFound", err)
	}
}

// TestGetNotFound covers the by-ID lookup miss path.
func TestGetNotFound(t *testing.T) {
	mgr := NewMemoryManager(agent.New(), nil, nil)
	if _, err := mgr.Get("s_xxx"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Get unknown error = %v, want ErrSessionNotFound", err)
	}
}

// TestListEmptyAndPopulated verifies List returns an empty slice for
// a fresh manager and includes every created session afterward.
func TestListEmptyAndPopulated(t *testing.T) {
	reg := agent.New()
	reg.Register(&fakeAgent{name: "a", mode: agent.ModePTY})
	reg.Register(&fakeAgent{name: "b", mode: agent.ModePTY})

	mgr := NewMemoryManager(reg, nil, nil)

	if got := mgr.List(); len(got) != 0 {
		t.Fatalf("List() on empty manager = %d, want 0", len(got))
	}

	for _, chatID := range []string{"c1", "c2", "c3"} {
		if _, err := mgr.Create(context.Background(), CreateRequest{
			ChatID:    chatID,
			Workspace: t.TempDir(),
			Agent:     "a",
		}); err != nil {
			t.Fatalf("Create(%s) error: %v", chatID, err)
		}
	}

	got := mgr.List()
	if len(got) != 3 {
		t.Fatalf("List() returned %d sessions, want 3", len(got))
	}
}

// TestKill transitions a running session to exited. After Kill
// returns the session is still in the table (so /run can respawn) but
// its agent handle has been closed.
func TestKill(t *testing.T) {
	reg := agent.New()
	reg.Register(&fakeAgent{
		name:   "claude",
		mode:   agent.ModePTY,
		events: []agent.AgentEvent{ // keep alive; no EventDone
			{Kind: agent.EventText, Text: "still going"},
		},
	})

	mgr := NewMemoryManager(reg, nil, nil)
	sess, err := mgr.Create(context.Background(), CreateRequest{
		ChatID:    "chat-1",
		Workspace: t.TempDir(),
		Agent:     "claude",
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	// Give the readPump a moment to register on the channel.
	time.Sleep(50 * time.Millisecond)

	if err := mgr.Kill(sess.ID); err != nil {
		t.Fatalf("Kill returned error: %v", err)
	}

	if !waitFor(2*time.Second, func() bool {
		s, _ := mgr.Get(sess.ID)
		return s != nil && s.Status() == StatusExited
	}) {
		t.Fatalf("session status never became StatusExited")
	}

	// Second Kill is a no-op.
	if err := mgr.Kill(sess.ID); err != nil {
		t.Fatalf("second Kill returned error: %v", err)
	}

	// Unknown sid returns ErrSessionNotFound.
	if err := mgr.Kill("missing"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Kill(missing) error = %v, want ErrSessionNotFound", err)
	}
}

// TestCreateUnknownAgent confirms a misspelled agent name bubbles up
// as the agent registry's sentinel error.
func TestCreateUnknownAgent(t *testing.T) {
	mgr := NewMemoryManager(agent.New(), nil, nil)
	_, err := mgr.Create(context.Background(), CreateRequest{
		ChatID:    "chat-1",
		Workspace: t.TempDir(),
		Agent:     "nope",
	})
	if err == nil {
		t.Fatalf("Create with unknown agent returned nil error")
	}
	if !errors.Is(err, agent.ErrUnknownAgent) {
		t.Fatalf("Create unknown-agent error = %v, want ErrUnknownAgent", err)
	}
}

// TestCreateDetectFailure ensures a failing Detect short-circuits
// Create before any session is registered.
func TestCreateDetectFailure(t *testing.T) {
	reg := agent.New()
	reg.Register(&fakeAgent{
		name:    "claude",
		mode:    agent.ModePTY,
		detectF: func() error { return errors.New("binary not found") },
	})
	mgr := NewMemoryManager(reg, nil, nil)

	_, err := mgr.Create(context.Background(), CreateRequest{
		ChatID:    "chat-1",
		Workspace: t.TempDir(),
		Agent:     "claude",
	})
	if err == nil {
		t.Fatalf("Create returned nil error, want Detect failure")
	}

	if got := mgr.List(); len(got) != 0 {
		t.Fatalf("session table size after Detect failure = %d, want 0", len(got))
	}
}

// TestCreateChatAlreadyBound verifies the 1:1 chat → session invariant
// from Q4 / SPEC §9.
func TestCreateChatAlreadyBound(t *testing.T) {
	reg := agent.New()
	reg.Register(&fakeAgent{name: "claude", mode: agent.ModePTY})

	mgr := NewMemoryManager(reg, nil, nil)
	if _, err := mgr.Create(context.Background(), CreateRequest{
		ChatID:    "chat-1",
		Workspace: t.TempDir(),
		Agent:     "claude",
	}); err != nil {
		t.Fatalf("first Create returned error: %v", err)
	}

	_, err := mgr.Create(context.Background(), CreateRequest{
		ChatID:    "chat-1",
		Workspace: t.TempDir(),
		Agent:     "claude",
	})
	if !errors.Is(err, ErrChatAlreadyBound) {
		t.Fatalf("second Create error = %v, want ErrChatAlreadyBound", err)
	}
}

// TestCreateValidationErrors covers the cheap pre-checks: missing
// ChatID / Workspace / Agent / nil registry.
func TestCreateValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		req  CreateRequest
		want string
	}{
		{"no chat", CreateRequest{Workspace: "/tmp", Agent: "claude"}, "ChatID is required"},
		{"no workspace", CreateRequest{ChatID: "c", Agent: "claude"}, "Workspace is required"},
		{"no agent", CreateRequest{ChatID: "c", Workspace: "/tmp"}, "Agent is required"},
	}
	mgr := NewMemoryManager(agent.New(), nil, nil)
	for _, tc := range cases {
		_, err := mgr.Create(context.Background(), tc.req)
		if err == nil {
			t.Fatalf("%s: nil error, want validation failure", tc.name)
		}
		if got := err.Error(); !contains(got, tc.want) {
			t.Fatalf("%s: error = %q, want substring %q", tc.name, got, tc.want)
		}
	}

	if _, err := (&MemoryManager{}).Create(context.Background(), CreateRequest{
		ChatID: "c", Workspace: "/tmp", Agent: "claude",
	}); err == nil {
		t.Fatalf("Create with nil registry returned nil error")
	}
}

// TestRestoreLoadsFromFile seeds a registry.json, opens a fresh
// Manager, calls Restore, and verifies every entry shows up in the
// in-memory table with the right lifecycle mapping.
func TestRestoreLoadsFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	file, err := registry.Open(path)
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	runningCode := 0
	exitedCode := 2
	entries := []registry.Entry{
		{
			SessionID: "s_running",
			ChatID:    "chat-run",
			Workspace: "/tmp/run",
			Agent:     "claude",
			Args:      []string{"--foo"},
			PID:       12345,
			StartedAt: now.Add(-time.Hour),
			LastRunAt: now,
			Status:    registry.StatusRunning,
			ExitCode:  &runningCode,
		},
		{
			SessionID: "s_detached",
			ChatID:    "chat-det",
			Workspace: "/tmp/det",
			Agent:     "codex",
			PID:       67890,
			StartedAt: now.Add(-2 * time.Hour),
			LastRunAt: now.Add(-time.Minute),
			Status:    registry.StatusDetached,
		},
		{
			SessionID: "s_exited",
			ChatID:    "chat-exit",
			Workspace: "/tmp/exit",
			Agent:     "claude",
			PID:       0,
			StartedAt: now.Add(-3 * time.Hour),
			LastRunAt: now.Add(-2 * time.Hour),
			Status:    registry.StatusExited,
			ExitCode:  &exitedCode,
		},
	}
	for _, e := range entries {
		if err := file.Upsert(e); err != nil {
			t.Fatalf("seed Upsert(%s): %v", e.SessionID, err)
		}
	}

	// Re-open the file (simulates nightme restarting) and build a
	// Manager that reads from it.
	file2, err := registry.Open(path)
	if err != nil {
		t.Fatalf("registry.Open(2): %v", err)
	}
	mgr := NewMemoryManager(agent.New(), file2, nil)
	if err := mgr.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Running -> Detached.
	if s, err := mgr.Get("s_running"); err != nil {
		t.Fatalf("Get(s_running): %v", err)
	} else if s.Status() != StatusDetached {
		t.Errorf("running entry status = %s, want detached", s.Status())
	} else if s.Agent != "claude" || s.Workspace != "/tmp/run" {
		t.Errorf("running entry metadata = %+v, want claude@/tmp/run", s.Snapshot())
	}

	// Detached -> Detached.
	if s, err := mgr.Get("s_detached"); err != nil {
		t.Fatalf("Get(s_detached): %v", err)
	} else if s.Status() != StatusDetached {
		t.Errorf("detached entry status = %s, want detached", s.Status())
	}

	// Exited -> Exited, exit code preserved.
	if s, err := mgr.Get("s_exited"); err != nil {
		t.Fatalf("Get(s_exited): %v", err)
	} else if s.Status() != StatusExited {
		t.Errorf("exited entry status = %s, want exited", s.Status())
	} else if s.ExitCode() == nil || *s.ExitCode() != 2 {
		t.Errorf("exited entry exit code = %v, want 2", s.ExitCode())
	}

	if got := len(mgr.List()); got != 3 {
		t.Errorf("List() = %d, want 3", got)
	}
}

// TestRestoreEmptyRegistry is a no-op sanity check: Restore against
// an empty registry should leave the manager empty and return nil.
func TestRestoreEmptyRegistry(t *testing.T) {
	dir := t.TempDir()
	file, err := registry.Open(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	mgr := NewMemoryManager(agent.New(), file, nil)
	if err := mgr.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := len(mgr.List()); got != 0 {
		t.Errorf("List() = %d, want 0", got)
	}
}

// TestRestoreNilRegistry documents the no-op behavior when reg is
// nil (callers that opt out of persistence).
func TestRestoreNilRegistry(t *testing.T) {
	mgr := NewMemoryManager(agent.New(), nil, nil)
	if err := mgr.Restore(context.Background()); err != nil {
		t.Fatalf("Restore(nil): %v", err)
	}
}

// TestCreatePersists exercises the round-trip: Create writes to disk,
// Kill writes again, a fresh Manager + Restore sees the terminal
// state.
func TestCreatePersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	file, err := registry.Open(path)
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}

	reg := agent.New()
	reg.Register(&fakeAgent{
		name: "claude",
		mode: agent.ModePTY,
		events: []agent.AgentEvent{
			{Kind: agent.EventText, Text: "hello"},
			// No EventDone — let Kill do the transition.
		},
	})
	mgr := NewMemoryManager(reg, file, nil)

	sess, err := mgr.Create(context.Background(), CreateRequest{
		ChatID:    "chat-persist",
		Workspace: t.TempDir(),
		Agent:     "claude",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Registry should now have the running entry.
	if got, ok := file.Get(sess.ID); !ok {
		t.Fatalf("registry missing entry after Create")
	} else if got.Status != registry.StatusRunning {
		t.Errorf("after Create, registry status = %s, want running", got.Status)
	}

	// Kill it.
	if err := mgr.Kill(sess.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if got, ok := file.Get(sess.ID); !ok {
		t.Fatalf("registry missing entry after Kill")
	} else if got.Status != registry.StatusExited {
		t.Errorf("after Kill, registry status = %s, want exited", got.Status)
	}

	// Re-open registry + Manager, Restore, verify state.
	file2, err := registry.Open(path)
	if err != nil {
		t.Fatalf("registry.Open(2): %v", err)
	}
	mgr2 := NewMemoryManager(agent.New(), file2, nil)
	if err := mgr2.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err := mgr2.Get(sess.ID)
	if err != nil {
		t.Fatalf("Get after Restore: %v", err)
	}
	if got.Status() != StatusExited {
		t.Errorf("after Restore, session status = %s, want exited", got.Status())
	}
	if got.Workspace == "" || got.Agent != "claude" {
		t.Errorf("after Restore, session metadata = %+v, want non-empty workspace + claude", got.Snapshot())
	}
}

// TestChatIndexRestored verifies the secondary chat → session index
// is rebuilt by Restore, so GetByChat works without any in-memory
// priming.
func TestChatIndexRestored(t *testing.T) {
	dir := t.TempDir()
	file, err := registry.Open(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	if err := file.Upsert(registry.Entry{
		SessionID: "s_idx",
		ChatID:    "oc_chat_42",
		Workspace: "/tmp/idx",
		Agent:     "claude",
		StartedAt: now,
		LastRunAt: now,
		Status:    registry.StatusDetached,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	mgr := NewMemoryManager(agent.New(), file, nil)
	if err := mgr.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	got, err := mgr.GetByChat("oc_chat_42")
	if err != nil {
		t.Fatalf("GetByChat after Restore: %v", err)
	}
	if got.ID != "s_idx" {
		t.Errorf("GetByChat returned ID %q, want s_idx", got.ID)
	}

	// Unknown chat still returns the sentinel.
	if _, err := mgr.GetByChat("nope"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("GetByChat(unknown) error = %v, want ErrSessionNotFound", err)
	}
}

// TestPersistFlushesState covers the explicit Persist() hook used by
// callers that want to force a write without going through Kill.
func TestPersistFlushesState(t *testing.T) {
	dir := t.TempDir()
	file, err := registry.Open(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}

	reg := agent.New()
	reg.Register(&fakeAgent{
		name:   "claude",
		mode:   agent.ModePTY,
		events: []agent.AgentEvent{{Kind: agent.EventText, Text: "hi"}},
	})
	mgr := NewMemoryManager(reg, file, nil)

	sess, err := mgr.Create(context.Background(), CreateRequest{
		ChatID:    "c",
		Workspace: t.TempDir(),
		Agent:     "claude",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Manually flip the session to detached (no agent involved).
	sess.setLifecycle(StatusDetached, nil, 0, nil)

	if err := mgr.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	got, ok := file.Get(sess.ID)
	if !ok {
		t.Fatalf("registry missing entry after Persist")
	}
	if got.Status != registry.StatusDetached {
		t.Errorf("after Persist, status = %s, want detached", got.Status)
	}
}

// waitFor polls cond every 10ms until it returns true or timeout
// elapses. Returns true on success, false on timeout.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// contains is a tiny substring helper so the test file does not need
// to import strings.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}