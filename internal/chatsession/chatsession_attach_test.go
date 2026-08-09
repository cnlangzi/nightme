package chatsession

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// makeTextEvent returns a non-nil AgentEvent so routeEvent publishes
// onto AgentEventBus (which it gates on AgentEvent != nil).
func makeTextEvent(text string) *agent.AgentEvent {
	ev := agent.AgentEvent{Kind: agent.EventAgentText, Text: text}
	return &ev
}

// TestAttachAgentSession_AddsToPool — attachAgentSession inserts the
// AS into cs.pool under its (agent, cwd) key, idempotently.
func TestAttachAgentSession_AddsToPool(t *testing.T) {
	cs := newChatSessionForTest("cs_attach")
	as := NewAgentSession("as_x", cs.ID, "pi", "/tmp", nil)

	cs.attachAgentSession(as)

	pool := cs.Pool()
	if len(pool) != 1 {
		t.Fatalf("Pool() len = %d, want 1", len(pool))
	}
	if pool[0].ID != as.ID {
		t.Errorf("Pool()[0].ID = %q, want %q", pool[0].ID, as.ID)
	}
}

// TestAttachAgentSession_InstallsSubscription — after attach, the
// ChatSession has installed an EventBus subscription on as.EventBus.
// Subsequent events pushed to as.eventQueue reach the CS's routeEvent.
func TestAttachAgentSession_InstallsSubscription(t *testing.T) {
	cs := newChatSessionForTest("cs_attach_sub")
	as := NewAgentSession("as_y", cs.ID, "pi", "/tmp", nil)

	var hits atomic.Int32
	cs.AgentEventBus.Subscribe(func(_ AgentEventEnvelope) bool {
		hits.Add(1)
		return false
	})

	cs.attachAgentSession(as)

	pushEvent(as, EnrichedEvent{
		Kind:       KindAgentEvent,
		UserMsgID:  "u-1",
		AgentEvent: makeTextEvent("hello"),
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hits.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if hits.Load() == 0 {
		t.Fatal("AgentEventBus subscriber never fired after attach + push")
	}
}

// TestAttachAgentSession_Idempotent — attaching the same AS twice
// does NOT install a duplicate subscription. The bus handler should
// fire exactly once per event.
func TestAttachAgentSession_Idempotent(t *testing.T) {
	cs := newChatSessionForTest("cs_attach_idem")
	as := NewAgentSession("as_z", cs.ID, "pi", "/tmp", nil)

	var hits atomic.Int32
	cs.AgentEventBus.Subscribe(func(_ AgentEventEnvelope) bool {
		hits.Add(1)
		return false
	})

	cs.attachAgentSession(as)
	cs.attachAgentSession(as) // second attach should be a no-op
	cs.attachAgentSession(as)

	// Verify subs map has exactly one entry.
	cs.mu.RLock()
	nSubs := len(cs.subs)
	cs.mu.RUnlock()
	if nSubs != 1 {
		t.Errorf("subs map has %d entries after 3× attach, want 1", nSubs)
	}

	pushEvent(as, EnrichedEvent{
		Kind:       KindAgentEvent,
		AgentEvent: makeTextEvent("x"),
	})

	// Wait briefly; bus subscriber should fire exactly once.
	time.Sleep(100 * time.Millisecond)
	if hits.Load() != 1 {
		t.Errorf("AgentEventBus fired %d times for single event, want 1", hits.Load())
	}
}

// TestDetachAgentSession_RemovesFromPool — detachAgentSession
// removes the AS from cs.pool. Subsequent Pool() lookups don't
// include it.
func TestDetachAgentSession_RemovesFromPool(t *testing.T) {
	cs := newChatSessionForTest("cs_detach")
	as := NewAgentSession("as_d", cs.ID, "pi", "/tmp", nil)

	cs.attachAgentSession(as)
	if len(cs.Pool()) != 1 {
		t.Fatalf("precondition: pool should have 1 entry, has %d", len(cs.Pool()))
	}

	cs.detachAgentSession(as)
	if len(cs.Pool()) != 0 {
		t.Errorf("pool should be empty after detach, has %d entries", len(cs.Pool()))
	}
}

// TestDetachAgentSession_AllowsReattach — after detach, attach
// again installs a fresh subscription that fires on subsequent
// events.
func TestDetachAgentSession_AllowsReattach(t *testing.T) {
	cs := newChatSessionForTest("cs_detach_reattach")
	as := NewAgentSession("as_d2", cs.ID, "pi", "/tmp", nil)

	var hits atomic.Int32
	cs.AgentEventBus.Subscribe(func(_ AgentEventEnvelope) bool {
		hits.Add(1)
		return false
	})

	cs.attachAgentSession(as)
	cs.detachAgentSession(as)
	cs.attachAgentSession(as)

	pushEvent(as, EnrichedEvent{
		Kind:       KindAgentEvent,
		AgentEvent: makeTextEvent("y"),
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hits.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if hits.Load() == 0 {
		t.Fatal("re-attach did not reinstall subscription")
	}
}

// TestAttachAgentSession_DifferentKeysCoexist — two ASes with the
// same agent but different cwds are distinct pool entries and each
// gets its own subscription.
func TestAttachAgentSession_DifferentKeysCoexist(t *testing.T) {
	cs := newChatSessionForTest("cs_keys")
	asA := NewAgentSession("as_a", cs.ID, "pi", "/code/A", nil)
	asB := NewAgentSession("as_b", cs.ID, "pi", "/code/B", nil)

	cs.attachAgentSession(asA)
	cs.attachAgentSession(asB)

	if len(cs.Pool()) != 2 {
		t.Errorf("Pool() len = %d, want 2", len(cs.Pool()))
	}

	cs.mu.RLock()
	_, hasA := cs.subs[asA.ID]
	_, hasB := cs.subs[asB.ID]
	cs.mu.RUnlock()
	if !hasA || !hasB {
		t.Errorf("subs map missing entries: hasA=%v hasB=%v", hasA, hasB)
	}
}