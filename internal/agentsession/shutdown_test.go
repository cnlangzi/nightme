package agentsession

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestShutdown_ClosesEventBus — after AgentSession.Shutdown, the
// EventBus stops delivering events.
func TestShutdown_ClosesEventBus(t *testing.T) {
	as := makeBareAgentSession(t, "pi", "/tmp")

	var hits atomic.Int32
	as.EventBus.Subscribe(func(_ EnrichedEvent) bool {
		hits.Add(1)
		return false
	})

	// Trigger first event to land so the dispatcher is started.
	pushEvent(as, EnrichedEvent{Kind:KindAgentEvent, AgentEvent: makeTextEvent("pre")})

	// Wait for first event.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hits.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}

	as.Shutdown()

	// After Shutdown, EventBus.Publish is a no-op.
	preHits := hits.Load()
	as.EventBus.Publish(EnrichedEvent{Kind:KindAgentEvent})
	time.Sleep(50 * time.Millisecond)
	if hits.Load() != preHits {
		t.Errorf("subscriber fired after Shutdown: pre=%d post=%d", preHits, hits.Load())
	}
}

// TestShutdown_DispatcherDrainsQueue — pushing N events followed by
// Shutdown: the dispatcher processes all queued events before
// exiting. Each event must reach a bus subscriber.
func TestShutdown_DispatcherDrainsQueue(t *testing.T) {
	as := makeBareAgentSession(t, "pi", "/tmp")

	var hits atomic.Int32
	as.EventBus.Subscribe(func(_ EnrichedEvent) bool {
		hits.Add(1)
		return false
	})

	as.ensureDispatcher()
	const N = 5
	for i := 0; i < N; i++ {
		as.eventQueue <- EnrichedEvent{Kind:KindAgentEvent}
	}

	// Wait for all events to drain before shutdown.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hits.Load() < N {
		time.Sleep(5 * time.Millisecond)
	}
	if hits.Load() != N {
		t.Fatalf("precondition: hits = %d, want %d", hits.Load(), N)
	}

	as.Shutdown()
	// After Shutdown, no new events fire.
	as.EventBus.Publish(EnrichedEvent{Kind:KindAgentEvent})
	time.Sleep(50 * time.Millisecond)
	if hits.Load() != N {
		t.Errorf("post-Shutdown hits = %d, want %d", hits.Load(), N)
	}
}

// TestShutdown_NoEventPushed_ReturnsImmediately — F2 regression:
// Shutdown on an AS whose dispatcher was never started returns
// promptly without waiting on dispatchDone.
func TestShutdown_NoEventPushed_ReturnsImmediately(t *testing.T) {
	as := makeBareAgentSession(t, "pi", "/tmp")

	// Sanity: dispatcher not started.
	as.asMu.RLock()
	if as.dispatchStarted {
		t.Fatal("dispatcher should not be started yet")
	}
	as.asMu.RUnlock()

	done := make(chan struct{})
	go func() {
		as.Shutdown()
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown deadlocked when dispatcher was never started")
	}
}

// TestShutdown_BridgeExit_DoesNotCloseEventBus — Phase 1 invariant
// #9: bridge process exit (SetExited) does NOT close the EventBus
// or remove subscriptions. Only /close or ChatSession shutdown does.
func TestShutdown_BridgeExit_DoesNotCloseEventBus(t *testing.T) {
	as := makeBareAgentSession(t, "pi", "/tmp")

	var hits atomic.Int32
	as.EventBus.Subscribe(func(_ EnrichedEvent) bool {
		hits.Add(1)
		return false
	})

	// Simulate bridge exit (no Shutdown).
	as.SetExited(0)
	if as.Status() != StatusExited {
		t.Fatalf("Status = %q, want StatusExited", as.Status())
	}

	// Subsequent pushes still reach the bus.
	pushEvent(as, EnrichedEvent{Kind:KindAgentEvent, AgentEvent: makeTextEvent("post-exit")})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hits.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if hits.Load() == 0 {
		t.Error("bus subscriber stopped firing after bridge exit (should NOT close bus)")
	}
}

// TestShutdown_DoubleShutdownIsIdempotent — calling Shutdown twice
// is a no-op (no panic, no leaked goroutines).
func TestShutdown_DoubleShutdownIsIdempotent(t *testing.T) {
	as := makeBareAgentSession(t, "pi", "/tmp")

	// First Shutdown.
	as.Shutdown()

	// Second Shutdown must not panic and must return promptly.
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("Shutdown panicked on second call: %v", r)
			}
			close(done)
		}()
		as.Shutdown()
	}()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("second Shutdown deadlocked")
	}
}

// silence unused import warnings.
var _ = atomic.Int32{}
var _ = agent.EventAgentText
// makeTextEvent returns a non-nil AgentEvent so the dispatcher
// publishes onto EventBus (gated on AgentEvent != nil).
func makeTextEvent(text string) *agent.AgentEvent {
	ev := agent.AgentEvent{Kind: agent.EventAgentText, Text: text}
	return &ev
}
