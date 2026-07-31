package pty

import (
	"io"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// fakeBridge implements Bridge for unit-testing ptySession without
// spawning a real child process. Reads come from a channel so the
// test can drive the read loop deterministically.
type fakeBridge struct {
	mu      sync.Mutex
	reads   chan []byte
	writes  [][]byte
	closed  bool
	closeCh chan struct{}
}

func newFakeBridge() *fakeBridge {
	return &fakeBridge{
		reads:   make(chan []byte, 4),
		closeCh: make(chan struct{}),
	}
}

func (f *fakeBridge) Read(p []byte) (int, error) {
	select {
	case data, ok := <-f.reads:
		if !ok {
			return 0, io.EOF
		}
		n := copy(p, data)
		return n, nil
	case <-f.closeCh:
		return 0, io.ErrClosedPipe
	}
}

func (f *fakeBridge) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, io.ErrClosedPipe
	}
	cp := append([]byte(nil), p...)
	f.writes = append(f.writes, cp)
	return len(p), nil
}

func (f *fakeBridge) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	close(f.closeCh)
	return nil
}

func (f *fakeBridge) PID() int             { return 4242 }
func (f *fakeBridge) Setsize(int, int) error { return nil }

// push feeds bytes into the bridge as if the child had written them.
func (f *fakeBridge) push(data string) {
	f.reads <- []byte(data)
}

// TestPtySessionReadLoop verifies that bytes pushed into the bridge
// become EventText events, followed by an EventDone on EOF.
func TestPtySessionReadLoop(t *testing.T) {
	b := newFakeBridge()
	s := NewPtySession(b)
	s.Start()

	b.push("hello ")
	b.push("world\n")
	b.push("more text")

	// Drain the first three events.
	got := drainEvents(t, s.Events(), 3, 2*time.Second)
	want := []string{"hello ", "world\n", "more text"}
	if !equalTexts(got, want) {
		t.Fatalf("Events() = %+v, want texts %q", got, want)
	}

	// Trigger EOF and verify the terminal EventDone arrives.
	close(b.reads)
	last := <-s.Events()
	if last.Kind != agent.EventDone {
		t.Fatalf("final event Kind = %s, want done", last.Kind)
	}
	if last.Done == nil || last.Done.ExitCode != -1 {
		t.Fatalf("terminal event missing DoneEvent{-1}: %+v", last)
	}

	// Channel must be closed after the terminal event.
	select {
	case _, ok := <-s.Events():
		if ok {
			t.Fatalf("Events() yielded a value after EventDone")
		}
	case <-time.After(time.Second):
		t.Fatalf("Events() not closed after EventDone")
	}
}

// TestPtySessionReadError verifies that a non-EOF read error also
// terminates the session (with EventDone, per the v0.1 contract —
// EventError is reserved for higher-level unrecoverable conditions).
func TestPtySessionReadError(t *testing.T) {
	b := newFakeBridge()
	s := NewPtySession(b)
	s.Start()

	b.push("ok\n")
	_ = <-s.Events()

	// Close the bridge to provoke an error on the next Read.
	_ = b.Close()

	// We expect at least one more event (either a final text event
	// from a Read that returned before the close, or the terminal
	// EventDone). Drain whatever the session produces and verify the
	// last one is EventDone.
	got := drainEvents(t, s.Events(), 1, 2*time.Second)
	if len(got) == 0 {
		t.Fatalf("expected at least one event after Close(), got none")
	}
	if got[len(got)-1].Kind != agent.EventDone {
		t.Fatalf("expected terminal EventDone, got %+v", got)
	}
}

// TestPtySessionSendText verifies that SendText writes bytes to the
// bridge.
func TestPtySessionSendText(t *testing.T) {
	b := newFakeBridge()
	s := NewPtySession(b)
	s.Start()
	t.Cleanup(func() { _ = s.Close() })

	if err := s.SendText("hello\n"); err != nil {
		t.Fatalf("SendText returned error: %v", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.writes) != 1 || string(b.writes[0]) != "hello\n" {
		t.Fatalf("writes = %q, want [\"hello\\n\"]", b.writes)
	}
}

// TestPtySessionSendPermission verifies that SendPermission also
// writes bytes to the bridge (PTY mode has no structured decision).
func TestPtySessionSendPermission(t *testing.T) {
	b := newFakeBridge()
	s := NewPtySession(b)
	s.Start()
	t.Cleanup(func() { _ = s.Close() })

	if err := s.SendPermission("y\n"); err != nil {
		t.Fatalf("SendPermission returned error: %v", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.writes) != 1 || string(b.writes[0]) != "y\n" {
		t.Fatalf("writes = %q, want [\"y\\n\"]", b.writes)
	}
}

// TestPtySessionCloseIdempotent verifies Close can be called more
// than once without error or panic.
func TestPtySessionCloseIdempotent(t *testing.T) {
	b := newFakeBridge()
	s := NewPtySession(b)
	s.Start()

	if err := s.Close(); err != nil {
		t.Fatalf("first Close returned error: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close returned error: %v", err)
	}
}

// drainEvents reads up to n events from ch, returning early on
// timeout. It is used by tests that drive the bridge deterministically.
func drainEvents(t *testing.T, ch <-chan agent.AgentEvent, n int, timeout time.Duration) []agent.AgentEvent {
	t.Helper()
	var got []agent.AgentEvent
	deadline := time.After(timeout)
	for len(got) < n {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			return got
		}
	}
	return got
}

// equalTexts compares a slice of events against the expected text
// payload slice.
func equalTexts(events []agent.AgentEvent, want []string) bool {
	if len(events) != len(want) {
		return false
	}
	for i, ev := range events {
		if ev.Kind != agent.EventText || ev.Text != want[i] {
			return false
		}
	}
	return true
}