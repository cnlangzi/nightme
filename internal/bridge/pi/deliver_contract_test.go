
// Regression tests for the pi bridge's deliver() contract.
//
// These lock in the "no timeout drop, no default drop" producer-side
// behaviour adopted in commit 4e54a02 ("fix(pi+chatsession): unbounded
// producer buffer so PIAgent never blocks") and the post-#82 buffer
// alignment that bumped the cap to 40960 across all four bridges.
//
// Pre-fix, deliver() had a `case <-t.C` branch that fired after 1 s
// and silently dropped the event. That timeout was the root cause of
// the "bridge reset: pi: new_session: context deadline exceeded"
// failure when the runtime was busy with another AS — the per-AS
// readpump blocked on a full eventQueue, the events channel filled
// to 64, deliver dropped the post-/new EventAgentReady, and the
// new_session RPC's 10 s deadline fired before the response could be
// read from stdout. Each test below would have caught that bug.
//
// The tests are white-box (package pi) and construct a minimal Agent
// with only the fields deliver() reads. No real pi process is
// spawned — these run in milliseconds and have no external
// dependencies.
package pi

import (
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestDeliver_BlocksWhenConsumerLags_NoDrop is the canonical
// regression test for the 1 s timeout drop. It fills the events
// channel to its full capacity, fires one more deliver() in a
// goroutine, and asserts the goroutine is still blocked after 2 s.
// Under the old behaviour deliver() would have returned within 1 s
// and the event would have been silently dropped.
func TestDeliver_BlocksWhenConsumerLags_NoDrop(t *testing.T) {
	a := &driver{
		events:   make(chan agent.AgentEvent, eventsBufferSize),
		closed:   make(chan struct{}),
		exitDone: make(chan struct{}),
	}

	// Fill the channel to its full capacity so the next send blocks.
	for i := 0; i < cap(a.events); i++ {
		a.events <- agent.AgentEvent{Kind: agent.EventAgentText, Text: "filler"}
	}
	if got := len(a.events); got != cap(a.events) {
		t.Fatalf("setup: channel len = %d, want %d", got, cap(a.events))
	}

	// deliver() must block. Wait > 2 s — under the old 1 s timeout
	// it would have returned (with a drop log) within 1 s.
	done := make(chan struct{})
	go func() {
		a.deliver(agent.AgentEvent{Kind: agent.EventAgentText, Text: "real"})
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("deliver returned while channel was full and no close signal was sent — would have dropped the event under the old 1 s timeout behaviour")
	case <-time.After(2 * time.Second):
		// Expected: still blocked, no drop.
	}

	// Drain one event — the next send now has room, deliver()
	// must unblock and complete.
	<-a.events
	select {
	case <-done:
		// Expected.
	case <-time.After(1 * time.Second):
		t.Fatal("deliver did not unblock after consumer drained one event")
	}
}

// TestDeliver_UnblocksOnClose verifies that closing the session
// (Close() → close(a.closed)) releases a blocked deliver(). The
// bridge must not leak producers after Close().
func TestDeliver_UnblocksOnClose(t *testing.T) {
	a := &driver{
		events:   make(chan agent.AgentEvent, eventsBufferSize),
		closed:   make(chan struct{}),
		exitDone: make(chan struct{}),
	}
	for i := 0; i < cap(a.events); i++ {
		a.events <- agent.AgentEvent{}
	}

	done := make(chan struct{})
	go func() {
		a.deliver(agent.AgentEvent{})
		close(done)
	}()

	// Sanity: still blocked (would catch a `default:` instant-drop
	// regression even without the close signal).
	select {
	case <-done:
		t.Fatal("deliver returned with full channel and no close signal — instant drop regression")
	case <-time.After(200 * time.Millisecond):
	}

	close(a.closed)

	select {
	case <-done:
		// Expected.
	case <-time.After(1 * time.Second):
		t.Fatal("deliver did not unblock after a.closed was closed")
	}
}

// TestDeliver_UnblocksOnExitDone verifies the second teardown
// signal (lifecycle closing a.exitDone after cmd.Wait returns) also
// unblocks deliver(). A bug that silently ignored exitDone would
// leak the producer when the process exits but Close() has not been
// called explicitly (e.g. crash path).
func TestDeliver_UnblocksOnExitDone(t *testing.T) {
	a := &driver{
		events:   make(chan agent.AgentEvent, eventsBufferSize),
		closed:   make(chan struct{}),
		exitDone: make(chan struct{}),
	}
	for i := 0; i < cap(a.events); i++ {
		a.events <- agent.AgentEvent{}
	}

	done := make(chan struct{})
	go func() {
		a.deliver(agent.AgentEvent{})
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("deliver returned with full channel and no exitDone signal — instant drop regression")
	case <-time.After(200 * time.Millisecond):
	}

	close(a.exitDone)

	select {
	case <-done:
		// Expected.
	case <-time.After(1 * time.Second):
		t.Fatal("deliver did not unblock after a.exitDone was closed")
	}
}

// TestEventsBufferSize_PinnedAt40960 locks in the buffer cap. A
// regression that lowers the cap would surface as dropped events
// under load and is much harder to diagnose than a failing unit
// test. 40960 was chosen as a generous-but-bounded headroom; bump
// deliberately via this constant, never inline.
func TestEventsBufferSize_PinnedAt40960(t *testing.T) {
	const want = 40960
	if eventsBufferSize != want {
		t.Fatalf("eventsBufferSize = %d, want %d — regression: cap was lowered, events may drop under load", eventsBufferSize, want)
	}
}