package chatsession

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// makeBareAgentSession constructs an AgentSession without going
// through Spawn, so the tests can drive the EventBus directly.
func makeBareAgentSession(t *testing.T, agentName, cwd string) *AgentSession {
	t.Helper()
	return NewAgentSession("as_test_"+agentName+"_"+cwd, "cs_test", agentName, cwd, nil)
}

// pushEvent is the test-side equivalent of the readpump's push:
// it ensures the dispatcher is running before pushing to eventQueue.
// Production pushes always come from readpump which calls
// ensureDispatcher(); bare-AS tests must do it explicitly.
func pushEvent(as *AgentSession, ev EnrichedEvent) {
	as.ensureDispatcher()
	as.eventQueue <- ev
}

// TestAgentSession_Dispatch_PublishesViaEventBus — the dispatcher
// drains eventQueue (filled via the read pump or directly here) and
// publishes each EnrichedEvent onto as.EventBus. Subscribers
// receive events as a fan-out of the bus.
func TestAgentSession_Dispatch_PublishesViaEventBus(t *testing.T) {
	as := makeBareAgentSession(t, "pi", "/tmp")

	var got EnrichedEvent
	as.EventBus.Subscribe(func(ev EnrichedEvent) bool {
		got = ev
		return false
	})

	want := EnrichedEvent{
		Kind:           KindAgentEvent,
		AgentSessionID: as.ID,
		PromptID:       "p_1",
		UserMsgID:      "u_1",
	}
	pushEvent(as, want)

	// Poll briefly for bus delivery (dispatcher runs in a goroutine).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got.UserMsgID == "u_1" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got.UserMsgID != "u_1" {
		t.Fatalf("event bus did not receive event (got %+v)", got)
	}
	if got.AgentSessionID != as.ID {
		t.Errorf("AgentSessionID: got %q, want %q", got.AgentSessionID, as.ID)
	}
	if got.PromptID != "p_1" {
		t.Errorf("PromptID: got %q, want %q", got.PromptID, "p_1")
	}
}

// TestAgentSession_Dispatch_LazyStart — the dispatcher goroutine
// does NOT start at construction. It's lazily spawned on the first
// push to eventQueue (via ensureDispatcher). Before the first push,
// the bus exists and can accept Subscribe calls; the flag is false.
func TestAgentSession_Dispatch_LazyStart(t *testing.T) {
	as := makeBareAgentSession(t, "pi", "/tmp")

	as.asMu.RLock()
	started := as.dispatchStarted
	as.asMu.RUnlock()
	if started {
		t.Fatal("dispatcher should not be started before first push")
	}

	// Subscribe (does not start dispatcher) and trigger the lazy start.
	as.EventBus.Subscribe(func(_ EnrichedEvent) bool { return false })
	pushEvent(as, EnrichedEvent{Kind:KindLifecycle})

	// Wait for ensureDispatcher to flip the flag.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		as.asMu.RLock()
		started = as.dispatchStarted
		as.asMu.RUnlock()
		if started {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !started {
		t.Fatal("dispatcher should be started after first push")
	}
}

// TestAgentSession_Shutdown_ClosesEventBus — Shutdown tears down the
// dispatcher and closes the EventBus. After Shutdown, Publish is a
// no-op (subscribers don't fire).
func TestAgentSession_Shutdown_ClosesEventBus(t *testing.T) {
	as := makeBareAgentSession(t, "pi", "/tmp")

	var hits atomic.Int32
	as.EventBus.Subscribe(func(_ EnrichedEvent) bool {
		hits.Add(1)
		return false
	})

	// Trigger first event to land.
	pushEvent(as, EnrichedEvent{Kind:KindAgentEvent})

	// Wait for first event to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hits.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}

	as.Shutdown()

	// After Shutdown, EventBus.Publish should be a no-op (subscribers
	// don't fire on a closed bus).
	preHits := hits.Load()
	as.EventBus.Publish(EnrichedEvent{Kind:KindAgentEvent})
	time.Sleep(50 * time.Millisecond)
	if hits.Load() != preHits {
		t.Errorf("subscriber fired after Shutdown: pre=%d post=%d", preHits, hits.Load())
	}
}

// TestAgentSession_Dispatch_OrderingGuarantees — events are
// delivered to the bus in the order they were pushed to eventQueue
// (FIFO). This is the multi-as Phase 1 invariant for per-AS event
// streams: ordering within one AS is preserved.
func TestAgentSession_Dispatch_OrderingGuarantees(t *testing.T) {
	as := makeBareAgentSession(t, "pi", "/tmp")

	var (
		mu    sync.Mutex
		order []int
	)
	as.EventBus.Subscribe(func(ev EnrichedEvent) bool {
		mu.Lock()
		defer mu.Unlock()
		if len(ev.PromptID) > 0 {
			order = append(order, int(ev.PromptID[0]-'0'))
		}
		return false
	})

	as.ensureDispatcher()
	for i := 0; i < 10; i++ {
		as.eventQueue <- EnrichedEvent{
			Kind:     KindAgentEvent,
			PromptID: string(rune('0' + i)),
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(order)
		mu.Unlock()
		if n == 10 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 10 {
		t.Fatalf("expected 10 events, got %d", len(order))
	}
	for i, v := range order {
		if v != i {
			t.Errorf("order[%d] = %d, want %d", i, v, i)
		}
	}
}

// TestAgentSession_Shutdown_NoEventPushed_ReturnsImmediately —
// F2 regression: Shutdown on an AS whose dispatcher was never
// started (no eventQueue push) must not deadlock waiting for
// dispatchDone.
func TestAgentSession_Shutdown_NoEventPushed_ReturnsImmediately(t *testing.T) {
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

// TestAgentSession_Respawn_PreservesEventBus — Spawn replaces the
// bridge handle but the EventBus pointer is preserved across
// respawns (Phase 1 invariant #8). ChatSession subscribers keep
// firing without re-subscribing.
//
// Invariant #9: bridge process exit does NOT close the EventBus. Only
// /kill or ChatSession shutdown closes it. We exercise this by
// marking the AS Exited (simulating bridge exit) and verifying the
// bus remains open + subscribers still fire.
func TestAgentSession_Respawn_PreservesEventBus(t *testing.T) {
	spawner := newFakeSpawner()
	as := makeBareAgentSession(t, "pi", "/tmp")

	var hits atomic.Int32
	as.EventBus.Subscribe(func(_ EnrichedEvent) bool {
		hits.Add(1)
		return false
	})

	ctx := context.Background()
	if err := as.Spawn(ctx, spawner); err != nil {
		t.Fatalf("first Spawn: %v", err)
	}
	firstBus := as.EventBus

	// Simulate bridge exit: mark Exited but DO NOT call Shutdown.
	// The EventBus must remain open (invariant #9).
	as.SetExited(0)
	if as.Status() != StatusExited {
		t.Fatalf("expected StatusExited, got %q", as.Status())
	}

	// Respawn (the existing entry's ID is preserved — pool identity).
	if err := as.Spawn(ctx, spawner); err != nil {
		t.Fatalf("respawn Spawn: %v", err)
	}

	if as.EventBus != firstBus {
		t.Errorf("EventBus pointer changed across respawn: %p → %p", firstBus, as.EventBus)
	}

	// Push an event; the same subscriber (registered before respawn)
	// should still fire.
	pushEvent(as, EnrichedEvent{Kind:KindAgentEvent})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hits.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if hits.Load() == 0 {
		t.Error("subscriber stopped firing across respawn")
	}
}

// TestAgentSession_Dispatch_FIFOAcrossMany — stress: 200 events
// pushed in quick succession all arrive at the bus in order.
func TestAgentSession_Dispatch_FIFOAcrossMany(t *testing.T) {
	as := makeBareAgentSession(t, "pi", "/tmp")

	const N = 200
	var (
		mu    sync.Mutex
		order []int
	)
	as.EventBus.Subscribe(func(ev EnrichedEvent) bool {
		mu.Lock()
		defer mu.Unlock()
		if len(ev.PromptID) > 0 {
			n, _ := strconvAtoi(ev.PromptID)
			if len(order) < N {
				order = append(order, n)
			}
		}
		return false
	})

	as.ensureDispatcher()
	for i := 0; i < N; i++ {
		as.eventQueue <- EnrichedEvent{
			Kind:     KindAgentEvent,
			PromptID: strconvItoa(i),
		}
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(order)
		mu.Unlock()
		if n == N {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != N {
		t.Fatalf("expected %d events, got %d", N, len(order))
	}
	for i, v := range order {
		if v != i {
			t.Fatalf("order[%d] = %d, want %d", i, v, i)
		}
	}
}

// strconvItoa / strconvAtoi — local helpers to avoid pulling in
// strconv just for these tests (they're not perf-critical).
func strconvItoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func strconvAtoi(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}