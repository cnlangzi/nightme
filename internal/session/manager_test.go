package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// fakeAgent is a minimal in-process Agent used by the Manager tests.
// Start produces a fakeAgentSession that emits a configurable list
// of events and then closes.
type fakeAgent struct {
	name    string
	mode    agent.Mode
	events  []agent.AgentEvent
	detectF func() error
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
	return &fakeAgentSession{events: f.events}, nil
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
	mgr := NewMemoryManager(reg, func(s *Session, ev agent.AgentEvent) {
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
	mgr := NewMemoryManager(agent.New(), nil)
	if _, err := mgr.GetByChat("nope"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("GetByChat unknown error = %v, want ErrSessionNotFound", err)
	}
}

// TestGetNotFound covers the by-ID lookup miss path.
func TestGetNotFound(t *testing.T) {
	mgr := NewMemoryManager(agent.New(), nil)
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

	mgr := NewMemoryManager(reg, nil)

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

	mgr := NewMemoryManager(reg, nil)
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
	mgr := NewMemoryManager(agent.New(), nil)
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
	mgr := NewMemoryManager(reg, nil)

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

	mgr := NewMemoryManager(reg, nil)
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
	mgr := NewMemoryManager(agent.New(), nil)
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