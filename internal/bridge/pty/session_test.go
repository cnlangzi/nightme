package pty

import (
	"context"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// fakeTransport implements Transport for unit-testing the merged *driver
// without spawning a real child process. Reads come from a channel so
// the test can drive the read loop deterministically.
type fakeTransport struct {
	mu      sync.Mutex
	reads   chan []byte
	writes  [][]byte
	closed  bool
	closeCh chan struct{}
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		reads:   make(chan []byte, 4),
		closeCh: make(chan struct{}),
	}
}

func (f *fakeTransport) Read(p []byte) (int, error) {
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

func (f *fakeTransport) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, io.ErrClosedPipe
	}
	cp := append([]byte(nil), p...)
	f.writes = append(f.writes, cp)
	return len(p), nil
}

func (f *fakeTransport) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	close(f.closeCh)
	return nil
}

func (f *fakeTransport) PID() int                          { return 4242 }
func (f *fakeTransport) Setsize(int, int) error              { return nil }
func (f *fakeTransport) Signal(os.Signal) error              { return nil }

// push feeds bytes into the bridge as if the child had written them.
func (f *fakeTransport) push(data string) {
	f.reads <- []byte(data)
}

// newAgentForTest constructs an *driver with the given fake Transport and
// a fresh events channel. Skips Start (no real process) and lets the
// caller decide whether to kick off the read loop.
//
// Used by tests that want to drive Events / Send* / Close directly
// without spawning a real PTY child.
func newAgentForTest(b Transport) *driver {
	return &driver{
		transport: b,
		events:    make(chan agent.AgentEvent, sessionBufferSize),
	}
}

// TestAgentReadLoop verifies that bytes pushed into the bridge become
// EventAgentText events, followed by an EventAgentDone on EOF.
func TestAgentReadLoop(t *testing.T) {
	b := newFakeTransport()
	a := newAgentForTest(b)
	go a.readLoop()

	b.push("hello ")
	b.push("world\n")
	b.push("more text")

	// Drain the first three events.
	got := drainEvents(t, a.Events(), 3, 2*time.Second)
	want := []string{"hello ", "world\n", "more text"}
	if !equalTexts(got, want) {
		t.Fatalf("Events() = %+v, want texts %q", got, want)
	}

	// Trigger EOF and verify the terminal EventAgentDone arrives.
	close(b.reads)
	last := <-a.Events()
	if last.Kind != agent.EventAgentDone {
		t.Fatalf("final event Kind = %s, want done", last.Kind)
	}
	if last.Done == nil || last.Done.ExitCode != -1 {
		t.Fatalf("terminal event missing AgentDoneEvent{-1}: %+v", last)
	}

	// Channel must be closed after the terminal event.
	select {
	case _, ok := <-a.Events():
		if ok {
			t.Fatalf("Events() yielded a value after EventAgentDone")
		}
	case <-time.After(time.Second):
		t.Fatalf("Events() not closed after EventAgentDone")
	}
}

// TestAgentReadError verifies that a non-EOF read error also
// terminates the session (with EventAgentDone, per the v0.1 contract —
// EventAgentError is reserved for higher-level unrecoverable conditions).
func TestAgentReadError(t *testing.T) {
	b := newFakeTransport()
	a := newAgentForTest(b)
	go a.readLoop()

	b.push("ok\n")
	_ = <-a.Events()

	// Close the bridge to provoke an error on the next Read.
	_ = b.Close()

	// We expect at least one more event (either a final text event
	// from a Read that returned before the close, or the terminal
	// EventAgentDone). Drain whatever the session produces and verify the
	// last one is EventAgentDone.
	got := drainEvents(t, a.Events(), 1, 2*time.Second)
	if len(got) == 0 {
		t.Fatalf("expected at least one event after Close(), got none")
	}
	if got[len(got)-1].Kind != agent.EventAgentDone {
		t.Fatalf("expected terminal EventAgentDone, got %+v", got)
	}
}

// TestAgentSendBlocks verifies that SendBlocks writes the encoded
// payload to the PTY. ContentText blocks are emitted verbatim
// followed by a newline; Image/File blocks are emitted as
// "@<path>\n".
func TestAgentSendBlocks(t *testing.T) {
	b := newFakeTransport()
	a := newAgentForTest(b)
	go a.readLoop()
	t.Cleanup(func() { _ = a.Close() })

	if err := a.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: "hello"},
	}); err != nil {
		t.Fatalf("SendBlocks returned error: %v", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.writes) != 1 || string(b.writes[0]) != "hello\n" {
		t.Fatalf("writes = %q, want [\"hello\\n\"]", b.writes)
	}
}

// TestAgentSendPermission verifies that SendPermission also writes
// bytes to the bridge (PTY mode has no structured decision).
func TestAgentSendPermission(t *testing.T) {
	b := newFakeTransport()
	a := newAgentForTest(b)
	go a.readLoop()
	t.Cleanup(func() { _ = a.Close() })

	if err := a.SendPermission("y\n"); err != nil {
		t.Fatalf("SendPermission returned error: %v", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.writes) != 1 || string(b.writes[0]) != "y\n" {
		t.Fatalf("writes = %q, want [\"y\\n\"]", b.writes)
	}
}

// TestAgentCloseIdempotent verifies Close can be called more than
// once without error or panic.
func TestAgentCloseIdempotent(t *testing.T) {
	b := newFakeTransport()
	a := newAgentForTest(b)
	go a.readLoop()

	if err := a.Close(); err != nil {
		t.Fatalf("first Close returned error: %v", err)
	}
	if err := a.Close(); err != nil {
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
		if ev.Kind != agent.EventAgentText || ev.Text != want[i] {
			return false
		}
	}
	return true
}
