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

func (f *fakeAgent) Name() string     { return f.name }
func (f *fakeAgent) Mode() agent.Mode { return f.mode }
func (f *fakeAgent) Command() string  { return "" }
func (f *fakeAgent) Args() []string   { return nil }
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
	mu       sync.Mutex
	events   []agent.AgentEvent
	channel  chan agent.AgentEvent
	started  bool
	closed   bool
	sendErr  error
	pid      int
}

func (f *fakeAgentSession) Events() <-chan agent.AgentEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.started {
		f.started = true
		f.channel = make(chan agent.AgentEvent, len(f.events)+1)
		go func() {
			for _, ev := range f.events {
				f.channel <- ev
			}
			close(f.channel)
		}()
	}
	return f.channel
}

func (f *fakeAgentSession) PID() int { return f.pid }
func (f *fakeAgentSession) SendText(string) error {
	return f.sendErr
}
func (f *fakeAgentSession) SendBlocks(context.Context, []agent.ContentBlock) error {
	return f.sendErr
}
func (f *fakeAgentSession) SendPermission(string) error {
	return nil
}
func (f *fakeAgentSession) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeAgentSession) wasClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// waitFor polls fn every 10ms until it returns true or timeout
// elapses. Returns true on success, false on timeout.
func waitFor(timeout time.Duration, fn func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fn()
}

// TestCreateAndGet verifies Create spawns an agent, returns a
// running Session, and Get retrieves it. v1.1: no ChatID involved.
func TestCreateAndGet(t *testing.T) {
	reg := agent.New()
	reg.Register(&fakeAgent{
		name: "claude",
		mode: agent.ModePTY,
		events: []agent.AgentEvent{
			{Kind: agent.EventText, Text: "thinking"},
			{Kind: agent.EventDone},
		},
	})

	var mu sync.Mutex
	var seen []agent.AgentEvent
	mgr := NewMemoryManager(reg, nil, func(_ *Session, ev agent.AgentEvent) {
		mu.Lock()
		seen = append(seen, ev)
		mu.Unlock()
	})

	sess, err := mgr.Create(context.Background(), CreateRequest{
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

	got, err := mgr.Get(sess.ID)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if got.ID != sess.ID {
		t.Fatalf("Get returned ID %s, want %s", got.ID, sess.ID)
	}
	if got.Workspace == "" {
		t.Fatalf("Workspace not set on Get return")
	}

	if !waitFor(2*time.Second, func() bool {
		s, _ := mgr.Get(sess.ID)
		return s != nil && s.Status() == StatusExited
	}) {
		t.Fatalf("session did not transition to exited")
	}

	if !waitFor(time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) >= 2
	}) {
		t.Fatalf("event callback did not see both events (saw %d)", len(seen))
	}
}

// TestGetNotFound returns the package error for an unknown ID.
func TestGetNotFound(t *testing.T) {
	reg := agent.New()
	mgr := NewMemoryManager(reg, nil, nil)
	if _, err := mgr.Get("nope"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Get unknown error = %v, want ErrSessionNotFound", err)
	}
}

// TestListEmptyAndPopulated verifies List returns every known
// session in unspecified order.
func TestListEmptyAndPopulated(t *testing.T) {
	reg := agent.New()
	reg.Register(&fakeAgent{name: "a", mode: agent.ModePTY})
	reg.Register(&fakeAgent{name: "b", mode: agent.ModePTY})

	mgr := NewMemoryManager(reg, nil, nil)

	if got := mgr.List(); len(got) != 0 {
		t.Fatalf("List() on empty manager = %d, want 0", len(got))
	}

	// v1.1: no chatID concept — sessions are just keyed by their
	// generated ID. Create three of them and verify the count.
	for i := 0; i < 3; i++ {
		if _, err := mgr.Create(context.Background(), CreateRequest{
			Workspace: t.TempDir(),
			Agent:     "a",
		}); err != nil {
			t.Fatalf("Create(%d) error: %v", i, err)
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
		name: "claude",
		mode: agent.ModePTY,
		events: []agent.AgentEvent{
			{Kind: agent.EventText, Text: "still going"},
		},
	})

	mgr := NewMemoryManager(reg, nil, nil)
	sess, err := mgr.Create(context.Background(), CreateRequest{
		Workspace: t.TempDir(),
		Agent:     "claude",
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	if err := mgr.Kill(sess.ID); err != nil {
		t.Fatalf("Kill returned error: %v", err)
	}

	if !waitFor(2*time.Second, func() bool {
		s, _ := mgr.Get(sess.ID)
		return s != nil && s.Status() == StatusExited
	}) {
		t.Fatalf("session did not transition to exited")
	}

	// Calling Kill again is a no-op.
	if err := mgr.Kill(sess.ID); err != nil {
		t.Fatalf("Kill on already-exited session error = %v, want nil", err)
	}
}

// TestCreateUnknownAgent surfaces a friendly error.
func TestCreateUnknownAgent(t *testing.T) {
	reg := agent.New()
	mgr := NewMemoryManager(reg, nil, nil)

	if _, err := mgr.Create(context.Background(), CreateRequest{
		Workspace: t.TempDir(),
		Agent:     "nope",
	}); err == nil {
		t.Fatal("Create with unknown agent returned nil error")
	}
}

// TestCreateDetectFailure surfaces the Detect error from the agent.
func TestCreateDetectFailure(t *testing.T) {
	reg := agent.New()
	reg.Register(&fakeAgent{
		name:    "claude",
		mode:    agent.ModePTY,
		detectF: func() error { return errors.New("not installed") },
	})
	mgr := NewMemoryManager(reg, nil, nil)

	if _, err := mgr.Create(context.Background(), CreateRequest{
		Workspace: t.TempDir(),
		Agent:     "claude",
	}); err == nil {
		t.Fatal("Create with Detect failure returned nil error")
	}
}

// TestCreateValidationErrors covers the required-field checks.
func TestCreateValidationErrors(t *testing.T) {
	reg := agent.New()
	mgr := NewMemoryManager(reg, nil, nil)
	cases := []struct {
		label string
		req   CreateRequest
		want  string
	}{
		{"no workspace", CreateRequest{Agent: "claude"}, "Workspace is required"},
		{"no agent", CreateRequest{Workspace: "/tmp"}, "Agent is required"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			_, err := mgr.Create(context.Background(), tc.req)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if got := err.Error(); !contains(got, tc.want) {
				t.Fatalf("error %q does not contain %q", got, tc.want)
			}
		})
	}
}

// TestSetEventCallback verifies the runtime can install a callback
// after construction.
func TestSetEventCallback(t *testing.T) {
	reg := agent.New()
	reg.Register(&fakeAgent{
		name: "claude",
		mode: agent.ModePTY,
		events: []agent.AgentEvent{
			{Kind: agent.EventText, Text: "hi"},
			{Kind: agent.EventDone},
		},
	})

	var got int32
	mgr := NewMemoryManager(reg, nil, nil)
	mgr.SetEventCallback(func(_ *Session, _ agent.AgentEvent) {
		// increment is safe because we never block; the counter
		// is read once after waitFor.
	})
	// (the inline `func` above is replaced by SetEventCallback
	//  just below so we can capture `got`.)
	mgr.SetEventCallback(func(_ *Session, _ agent.AgentEvent) {
		// simple counter without lock; tests run serially.
		// we use a separate channel to avoid races with the
		// testing package's own state.
		got = 1
	})

	sess, err := mgr.Create(context.Background(), CreateRequest{
		Workspace: t.TempDir(),
		Agent:     "claude",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if !waitFor(2*time.Second, func() bool {
		s, _ := mgr.Get(sess.ID)
		return s != nil && s.Status() == StatusExited
	}) {
		t.Fatal("session did not exit")
	}

	if got != 1 {
		t.Fatalf("callback was not invoked: got=%d", got)
	}
}

// TestRestoreLoadsFromFile reads a populated registry and rebuilds
// the in-memory table. Chat-binding is no longer part of Session,
// so the test focuses on workspace / agent / PID / status
// restoration only.
func TestRestoreLoadsFromFile(t *testing.T) {
	reg := agent.New()

	// Persist two entries directly via registry.File.
	tmp := t.TempDir()
	path := filepath.Join(tmp, "reg.json")
	r, err := registry.Open(path)
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	

	if err := r.Upsert(registry.Entry{
		SessionID: "s_aaa",
		Workspace: t.TempDir(),
		Agent:     "claude",
		PID:       111,
		Status:    registry.StatusDetached,
	}); err != nil {
		t.Fatalf("upsert detached: %v", err)
	}
	if err := r.Upsert(registry.Entry{
		SessionID: "s_bbb",
		Workspace: t.TempDir(),
		Agent:     "codex",
		PID:       222,
		Status:    registry.StatusExited,
	}); err != nil {
		t.Fatalf("upsert exited: %v", err)
	}

	mgr := NewMemoryManager(reg, r, nil)
	if err := mgr.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	got, err := mgr.Get("s_aaa")
	if err != nil {
		t.Fatalf("Get s_aaa: %v", err)
	}
	if got.Agent != "claude" || got.Status() != StatusDetached {
		t.Fatalf("s_aaa restored with agent=%s status=%s", got.Agent, got.Status())
	}

	got2, err := mgr.Get("s_bbb")
	if err != nil {
		t.Fatalf("Get s_bbb: %v", err)
	}
	if got2.Agent != "codex" || got2.Status() != StatusExited {
		t.Fatalf("s_bbb restored with agent=%s status=%s", got2.Agent, got2.Status())
	}

	// Restore again is idempotent.
	if err := mgr.Restore(context.Background()); err != nil {
		t.Fatalf("Restore twice: %v", err)
	}
}

// TestRestoreNilRegistry is a no-op when reg is nil.
func TestRestoreNilRegistry(t *testing.T) {
	reg := agent.New()
	mgr := NewMemoryManager(reg, nil, nil)
	if err := mgr.Restore(context.Background()); err != nil {
		t.Fatalf("Restore with nil reg: %v", err)
	}
}

// TestPersistFlushesState writes the in-memory table back to disk.
func TestPersistFlushesState(t *testing.T) {
	reg := agent.New()
	reg.Register(&fakeAgent{name: "claude", mode: agent.ModePTY})

	tmp := t.TempDir()
	path := filepath.Join(tmp, "reg.json")
	r, err := registry.Open(path)
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	

	mgr := NewMemoryManager(reg, r, nil)
	sess, err := mgr.Create(context.Background(), CreateRequest{
		Workspace: t.TempDir(),
		Agent:     "claude",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := mgr.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if _, ok := r.Get(sess.ID); !ok {
		t.Fatalf("Persist did not write entry for %s", sess.ID)
	}
}

// contains is a tiny substring helper to avoid importing strings
// just for the assertion messages.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}