//go:build !windows

// Regression tests for the codex bridge's deliver() / events-buffer
// contract. These lock in the "no timeout drop, no default drop"
// producer-side behaviour adopted in commit 67b295ec ("unify
// producer-side buffer contract across all bridges") — the same
// F-54 incident the rest of the diff is fixing.
//
// Pre-fix, codex's Agent.deliver() used `select { case ... default:
// drop }` to silently drop events when the channel was full. The
// remaining bridges (pi, claudecode, pty, acp) had already been
// migrated to block-on-close + exitDone; codex was the last holdout.
// This test package codifies both halves of the contract so a future
// change that either reintroduces a `default:` drop or lowers the
// buffer cap is caught immediately.
//
// Tests are white-box (package codex) and construct a minimal Agent
// + session struct with only the fields deliver() reads. No real
// codex process is spawned — these run in milliseconds and have no
// external dependencies.
package codex

import (
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestEventsBufferSize_PinnedAt40960 locks in the events channel
// cap. 40960 was chosen as generous-but-bounded headroom; bump
// deliberately via this constant, never inline.
func TestEventsBufferSize_PinnedAt40960(t *testing.T) {
	const want = 40960
	if eventBufferSize != want {
		t.Fatalf("eventBufferSize = %d, want %d — regression: cap was lowered, events may drop under load", eventBufferSize, want)
	}
}

// TestDeliver_BlocksWhenConsumerLags_NoDrop is the canonical
// regression test for the `default:` instant-drop. It fills the
// events channel to its full capacity, fires one more deliver()
// in a goroutine, and asserts the goroutine is still blocked after
// 2 s. Under the old behaviour deliver() would have returned
// immediately and the event would have been silently dropped.
func TestDeliver_BlocksWhenConsumerLags_NoDrop(t *testing.T) {
	a := &driver{
		session: &session{
			events:   make(chan agent.AgentEvent, eventBufferSize),
			closed:   make(chan struct{}),
			exitDone: make(chan struct{}),
		},
	}

	// Fill the channel to its full capacity so the next send blocks.
	for i := 0; i < cap(a.session.events); i++ {
		a.session.events <- agent.AgentEvent{Kind: agent.EventAgentText, Text: "filler"}
	}
	if got := len(a.session.events); got != cap(a.session.events) {
		t.Fatalf("setup: channel len = %d, want %d", got, cap(a.session.events))
	}

	// deliver() must block. Wait > 2 s — under the old `default:`
	// behaviour it would have returned instantly (and dropped).
	done := make(chan struct{})
	go func() {
		a.deliver(agent.AgentEvent{Kind: agent.EventAgentText, Text: "real"})
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("deliver returned while channel was full and no close signal was sent — would have dropped the event under the old `default:` instant-drop behaviour")
	case <-time.After(2 * time.Second):
		// Expected: still blocked, no drop.
	}

	// Drain one event — the next send now has room, deliver()
	// must unblock and complete.
	<-a.session.events
	select {
	case <-done:
		// Expected.
	case <-time.After(1 * time.Second):
		t.Fatal("deliver did not unblock after consumer drained one event")
	}
}

// TestDeliver_UnblocksOnClose verifies that closing the session
// (Close() → close(s.closed)) releases a blocked deliver(). The
// bridge must not leak producers after Close().
func TestDeliver_UnblocksOnClose(t *testing.T) {
	a := &driver{
		session: &session{
			events:   make(chan agent.AgentEvent, eventBufferSize),
			closed:   make(chan struct{}),
			exitDone: make(chan struct{}),
		},
	}
	for i := 0; i < cap(a.session.events); i++ {
		a.session.events <- agent.AgentEvent{}
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
	case <-time.After(100 * time.Millisecond):
	}

	// Close the session — deliver() must unblock.
	close(a.session.closed)
	select {
	case <-done:
		// Expected.
	case <-time.After(1 * time.Second):
		t.Fatal("deliver did not unblock after session.closed was closed — Close() would leak producers")
	}
}

// TestDeliver_UnblocksOnExitDone verifies that lifecycle's
// close(s.exitDone) (after cmd.Wait returns) releases a blocked
// deliver() too. Same leak-prevention contract as the closed signal.
func TestDeliver_UnblocksOnExitDone(t *testing.T) {
	a := &driver{
		session: &session{
			events:   make(chan agent.AgentEvent, eventBufferSize),
			closed:   make(chan struct{}),
			exitDone: make(chan struct{}),
		},
	}
	for i := 0; i < cap(a.session.events); i++ {
		a.session.events <- agent.AgentEvent{}
	}

	done := make(chan struct{})
	go func() {
		a.deliver(agent.AgentEvent{})
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("deliver returned with full channel — instant drop regression")
	case <-time.After(100 * time.Millisecond):
	}

	close(a.session.exitDone)
	select {
	case <-done:
		// Expected.
	case <-time.After(1 * time.Second):
		t.Fatal("deliver did not unblock after session.exitDone was closed — lifecycle exit would leak producers")
	}
}
