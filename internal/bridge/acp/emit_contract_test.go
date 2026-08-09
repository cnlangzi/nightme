// Regression tests for the acp bridge's emit() / emitConnected()
// contract.
//
// These lock in the "no default: instant drop" producer-side
// behaviour. Pre-alignment, emitConnected() had a `default:` branch
// that silently dropped EventAgentReady when the events channel was
// full — same failure mode as pi's old 1 s timeout drop, just
// triggered instantly rather than after a delay. A consumer that
// was briefly slow (e.g. busy with another AS) would lose the
// session id, and --resume would replay a dead session on daemon
// restart.
//
// emit() already used the correct pattern (ctx.Done fallback only)
// but had no test pinning it; the tests below also lock it down.
//
// The tests are white-box (package acp) and construct a minimal
// Agent with only the fields emit() / emitConnected() read. No real
// ACP bridge is launched — these run in milliseconds.
package acp

import (
	"context"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestEmit_NoInstantDrop verifies that emit() does NOT fall through
// to a `default:` instant-drop when the channel is full. Filling
// the channel to capacity, calling emit() in a goroutine, and
// asserting the goroutine is still blocked after 1 s catches any
// regression that re-adds the instant-drop branch.
func TestEmit_NoInstantDrop(t *testing.T) {
	a := &Agent{
		ctx:    context.Background(),
		events: make(chan agent.AgentEvent, eventBufferSize),
	}

	for i := 0; i < cap(a.events); i++ {
		a.events <- agent.AgentEvent{}
	}

	done := make(chan struct{})
	go func() {
		a.emit(agent.AgentEvent{Kind: agent.EventAgentText, Text: "real"})
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("emit returned while channel was full and ctx was not cancelled — default: instant-drop regression")
	case <-time.After(1 * time.Second):
		// Expected: still blocked.
	}

	// Drain one — emit() must unblock.
	<-a.events
	select {
	case <-done:
		// Expected.
	case <-time.After(1 * time.Second):
		t.Fatal("emit did not unblock after consumer drained one event")
	}
}

// TestEmit_UnblocksOnCtxDone verifies the teardown signal: when the
// bridge context is cancelled, emit() must exit (rather than leak
// the producer goroutine).
func TestEmit_UnblocksOnCtxDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &Agent{
		ctx:    ctx,
		events: make(chan agent.AgentEvent, eventBufferSize),
	}
	for i := 0; i < cap(a.events); i++ {
		a.events <- agent.AgentEvent{}
	}

	done := make(chan struct{})
	go func() {
		a.emit(agent.AgentEvent{})
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("emit returned with full channel and no ctx cancellation")
	case <-time.After(200 * time.Millisecond):
	}

	cancel()

	select {
	case <-done:
		// Expected.
	case <-time.After(1 * time.Second):
		t.Fatal("emit did not unblock after ctx was cancelled")
	}
}

// TestEmitConnected_NoInstantDrop is the specific regression for
// emitConnected(). Pre-alignment this function had a `default:`
// branch that silently dropped EventAgentReady — the canonical
// "session id lost" failure mode. The test asserts the call blocks
// rather than returning immediately when the channel is full.
func TestEmitConnected_NoInstantDrop(t *testing.T) {
	a := &Agent{
		ctx:           context.Background(),
		events:        make(chan agent.AgentEvent, eventBufferSize),
		sessionID:     "test-session",
		agentName:     "test-agent",
		workspace:     "/tmp/test",
		connectedSent: false,
	}

	for i := 0; i < cap(a.events); i++ {
		a.events <- agent.AgentEvent{}
	}

	done := make(chan struct{})
	go func() {
		a.emitConnected()
		close(done)
	}()

	// Give the goroutine a chance to start. If it took the
	// default: branch, it would complete well within 200 ms;
	// otherwise it must be blocked on the full channel.
	select {
	case <-done:
		t.Fatal("emitConnected returned while channel was full and ctx was not cancelled — this is the exact `default:` regression that lost EventAgentReady in production")
	case <-time.After(200 * time.Millisecond):
		// Expected: still blocked.
	}

	// Drain one — emitConnected() must unblock and complete.
	// (Reading a.connectedSent here would race with the goroutine;
	// the post-completion check below is the race-free assertion.)
	<-a.events
	select {
	case <-done:
		// Expected.
	case <-time.After(1 * time.Second):
		t.Fatal("emitConnected did not unblock after consumer drained one event")
	}

	if !a.connectedSent {
		t.Fatal("connectedSent remained false after the send completed")
	}
}

// TestEmitConnected_UnblocksOnCtxDone: the teardown path for
// emitConnected.
func TestEmitConnected_UnblocksOnCtxDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &Agent{
		ctx:           ctx,
		events:        make(chan agent.AgentEvent, eventBufferSize),
		sessionID:     "test-session",
		agentName:     "test-agent",
		workspace:     "/tmp/test",
		connectedSent: false,
	}
	for i := 0; i < cap(a.events); i++ {
		a.events <- agent.AgentEvent{}
	}

	done := make(chan struct{})
	go func() {
		a.emitConnected()
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("emitConnected returned with full channel and no ctx cancellation")
	case <-time.After(200 * time.Millisecond):
	}

	cancel()

	select {
	case <-done:
		// Expected.
	case <-time.After(1 * time.Second):
		t.Fatal("emitConnected did not unblock after ctx was cancelled")
	}
}

// TestEventBufferSize_Pinned locks in the cap, matching the pi
// bridge's TestEventsBufferSize_PinnedAt40960. A regression that
// lowers the cap (or, more likely, drops the constant entirely and
// inlines a smaller literal) is caught here.
func TestEventBufferSize_Pinned(t *testing.T) {
	const want = 40960
	if eventBufferSize != want {
		t.Fatalf("eventBufferSize = %d, want %d — regression: cap was lowered, events may drop under load", eventBufferSize, want)
	}
}
