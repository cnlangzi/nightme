// deliver_nonblock_test.go — unit test pinning the deadlock-resistance
// invariant for driver.deliver().
//
// The invariant: when the events channel is full AND the bridge
// is alive (closed / exitDone NOT signaled), deliver() must
// return immediately rather than block on the channel send.
// Without the `default:` branch in deliver's select, a slow
// runtime consumer would stall the WS readPump goroutine, which
// would back-pressure dsh web's WS write, which would deadlock
// the entire bridge.
//
// This test fills the events buffer WITHOUT reading from it and
// then calls deliver() many times — each call must return in
// microseconds, not in seconds.

package dsh

import (
	"context"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestDeliver_DoesNotBlockOnFullBuffer pins the deadlock-resistance
// property. We construct a driver whose events chan is buffered
// (default behavior — we never replace it), then call deliver()
// far more times than the buffer can hold. Every call MUST
// return well under 1s; a blocked send would exceed this on a
// healthy machine and the test would hang.
func TestDeliver_DoesNotBlockOnFullBuffer(t *testing.T) {
	// eventBufferSize is 131072 — far too many to actually fill in
	// a unit test (would need 26 MiB of allocations). Instead we
	// use a small standalone driver with a tiny events chan to
	// prove the property at a realistic scale.
	d := &driver{
		sessionID: "test",
		agentName: "dsh",
		workspace: "/tmp",
		events:    make(chan agent.AgentEvent, 4), // intentionally tiny
		closed:    make(chan struct{}),
		exitDone:  make(chan struct{}),
	}
	// Note: we intentionally don't start lifecycle / pumps.
	// deliver() is a pure method on driver state; we only need
	// the channels.

	// Phase 1: fill the buffer with 4 events (no drain).
	for i := 0; i < 4; i++ {
		d.deliver(agent.AgentEvent{Kind: agent.EventAgentText})
	}

	// Phase 2: 1000 more deliveries must all return promptly.
	// If deliver blocks, this loop will hang and the test will
	// time out via -timeout rather than complete.
	const extra = 1000
	start := time.Now()
	for i := 0; i < extra; i++ {
		d.deliver(agent.AgentEvent{Kind: agent.EventAgentText})
	}
	elapsed := time.Since(start)

	// We don't pin an exact upper bound (CI runners vary); we
	// only need "didn't block for seconds". 100 ms is comfortably
	// more than a million non-blocking channel sends cost.
	if elapsed > 100*time.Millisecond {
		t.Errorf("deliver blocked or slow: %d deliveries took %v", extra, elapsed)
	}

	// Phase 3: verify drops landed silently on a closed bridge.
	close(d.closed)
	for i := 0; i < 10; i++ {
		d.deliver(agent.AgentEvent{Kind: agent.EventAgentText})
	}
	// No assertion needed — we just want to ensure no panic
	// (e.g. sending on closed chan) and no hang.
}

// TestDeliver_StampsSessionContext confirms the optimization that
// deliver() fills SessionID / AgentName / Workspace on every event.
// Without this, downstream consumers (gateway, chatsession) can't
// route events back to the originating session.
func TestDeliver_StampsSessionContext(t *testing.T) {
	d := &driver{
		sessionID: "sess-123",
		agentName: "dsh",
		workspace: "/work",
		events:    make(chan agent.AgentEvent, 1),
		closed:    make(chan struct{}),
		exitDone:  make(chan struct{}),
	}

	// Caller doesn't pre-fill — deliver() should.
	ev := agent.AgentEvent{Kind: agent.EventAgentText, Text: "hi"}
	d.deliver(ev)

	select {
	case got := <-d.events:
		if got.SessionID != "sess-123" {
			t.Errorf("SessionID not stamped: got %q, want sess-123", got.SessionID)
		}
		if got.AgentName != "dsh" {
			t.Errorf("AgentName not stamped: got %q, want dsh", got.AgentName)
		}
		if got.Workspace != "/work" {
			t.Errorf("Workspace not stamped: got %q, want /work", got.Workspace)
		}
	case <-time.After(time.Second):
		t.Fatal("deliver did not enqueue within 1s")
	}
}

// TestDeliver_RespectsCallerFilledFields ensures that if the caller
// already populated SessionID (e.g. via a translator that knows
// more than the bridge's deliver()), deliver() does NOT clobber it.
// Preserves the "owner of session metadata" mental model: the
// caller is authoritative when it sets a value.
func TestDeliver_RespectsCallerFilledFields(t *testing.T) {
	d := &driver{
		sessionID: "default-id",
		agentName: "default-name",
		workspace: "/default-work",
		events:    make(chan agent.AgentEvent, 1),
		closed:    make(chan struct{}),
		exitDone:  make(chan struct{}),
	}

	ev := agent.AgentEvent{
		Kind:      agent.EventAgentText,
		SessionID: "caller-id",
		AgentName: "caller-name",
		Workspace: "/caller-work",
	}
	d.deliver(ev)

	select {
	case got := <-d.events:
		if got.SessionID != "caller-id" {
			t.Errorf("SessionID clobbered: got %q, want caller-id", got.SessionID)
		}
		if got.AgentName != "caller-name" {
			t.Errorf("AgentName clobbered: got %q, want caller-name", got.AgentName)
		}
		if got.Workspace != "/caller-work" {
			t.Errorf("Workspace clobbered: got %q, want /caller-work", got.Workspace)
		}
	case <-time.After(time.Second):
		t.Fatal("deliver did not enqueue within 1s")
	}
}

// silence unused import linter if context isn't otherwise used
var _ = context.Background
