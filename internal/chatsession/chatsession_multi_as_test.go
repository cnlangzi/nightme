package chatsession

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestMultiAS_AllPublishToBus — N AgentSessions in the pool each push
// events; ChatSession's unified receive handler observes all of them.
func TestMultiAS_AllPublishToBus(t *testing.T) {
	cs := newChatSessionForTest("cs_multi_all")

	ases := []*AgentSession{
		NewAgentSession("as_a", cs.ID, "pi", "/code/A", nil),
		NewAgentSession("as_b", cs.ID, "claude", "/code/A", nil),
		NewAgentSession("as_c", cs.ID, "codex", "/code/A", nil),
	}
	for _, as := range ases {
		cs.attachAgentSession(as)
	}

	var hits atomic.Int32
	cs.AgentEventBus.Subscribe(func(env AgentEventEnvelope) bool {
		hits.Add(1)
		return false
	})

	for _, as := range ases {
		pushEvent(as, EnrichedEvent{
			Kind:           KindAgentEvent,
			AgentSessionID: as.ID,
			AgentEvent:     makeTextEvent("from " + as.ID),
		})
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hits.Load() < 3 {
		time.Sleep(5 * time.Millisecond)
	}
	if hits.Load() != 3 {
		t.Errorf("AgentEventBus hits = %d, want 3", hits.Load())
	}
}

// TestMultiAS_InterleavedOrderPreserved — interleaved pushes from
// two ASes: each AS's events arrive at the bus in that AS's push
// order (per-AS FIFO), but cross-AS order is best-effort and depends
// on goroutine scheduling. Phase 1 invariant: ordering within one
// AS is preserved (already covered by
// TestAgentSession_Dispatch_OrderingGuarantees); cross-AS has no
// global ordering guarantee.
func TestMultiAS_InterleavedOrderPreserved(t *testing.T) {
	cs := newChatSessionForTest("cs_multi_interleave")

	asA := NewAgentSession("as_A", cs.ID, "pi", "/tmp", nil)
	asB := NewAgentSession("as_B", cs.ID, "pi", "/tmp", nil)
	cs.attachAgentSession(asA)
	cs.attachAgentSession(asB)

	var (
		mu    sync.Mutex
		fromA []string
		fromB []string
	)
	cs.AgentEventBus.Subscribe(func(env AgentEventEnvelope) bool {
		mu.Lock()
		defer mu.Unlock()
		if env.AgentSession == nil || env.Event == nil {
			return false
		}
		switch env.AgentSession.ID {
		case asA.ID:
			fromA = append(fromA, env.Event.Text)
		case asB.ID:
			fromB = append(fromB, env.Event.Text)
		}
		return false
	})

	// Push A1, A2 from A and B1, B2 from B. Per-AS order is the
	// only thing we can assert.
	pushEvent(asA, EnrichedEvent{Kind:KindAgentEvent, AgentEvent: makeTextEvent("A1")})
	pushEvent(asB, EnrichedEvent{Kind:KindAgentEvent, AgentEvent: makeTextEvent("B1")})
	pushEvent(asB, EnrichedEvent{Kind:KindAgentEvent, AgentEvent: makeTextEvent("B2")})
	pushEvent(asA, EnrichedEvent{Kind:KindAgentEvent, AgentEvent: makeTextEvent("A2")})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(fromA) + len(fromB)
		mu.Unlock()
		if n == 4 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	wantA := []string{"A1", "A2"}
	wantB := []string{"B1", "B2"}
	if !equalSlices(fromA, wantA) {
		t.Errorf("fromA = %v, want %v (per-AS FIFO broken)", fromA, wantA)
	}
	if !equalSlices(fromB, wantB) {
		t.Errorf("fromB = %v, want %v (per-AS FIFO broken)", fromB, wantB)
	}
}

// TestMultiAS_UseSwitchDoesNotUnsubscribe — switching selectedAgent
// does NOT remove the bus subscription on the previously-selected AS.
// Phase 1 invariant #6: switching `selected` does not affect any
// AgentSession's subscription.
func TestMultiAS_UseSwitchDoesNotUnsubscribe(t *testing.T) {
	cs := newChatSessionForTest("cs_use_switch")

	asA := NewAgentSession("as_A", cs.ID, "pi", "/tmp", nil)
	asB := NewAgentSession("as_B", cs.ID, "claude", "/tmp", nil)
	cs.attachAgentSession(asA)
	cs.attachAgentSession(asB)
	cs.selectAgentSessionLocked(asA)

	var hitsA atomic.Int32
	cs.AgentEventBus.Subscribe(func(env AgentEventEnvelope) bool {
		if env.AgentSession != nil && env.AgentSession.ID == asA.ID {
			hitsA.Add(1)
		}
		return false
	})

	// /use B
	cs.SetSelectedAgent("claude")
	cs.selectAgentSessionLocked(asB)

	// A's subscription must still fire.
	pushEvent(asA, EnrichedEvent{
		Kind:       KindAgentEvent,
		AgentEvent: makeTextEvent("from A after /use"),
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hitsA.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if hitsA.Load() == 0 {
		t.Error("A's bus subscription stopped firing after /use B")
	}
}

// TestMultiAS_PromptIDsNotCrossed — two ASes each carry their own
// currentPrompt with distinct IDs. Events pushed to one AS don't
// carry the other's PromptID.
func TestMultiAS_PromptIDsNotCrossed(t *testing.T) {
	cs := newChatSessionForTest("cs_prompt_ids")

	asA := NewAgentSession("as_A", cs.ID, "pi", "/tmp", nil)
	asB := NewAgentSession("as_B", cs.ID, "claude", "/tmp", nil)
	cs.attachAgentSession(asA)
	cs.attachAgentSession(asB)

	var (
		mu       sync.Mutex
		fromA    []string
		fromB    []string
	)
	cs.AgentEventBus.Subscribe(func(env AgentEventEnvelope) bool {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case env.AgentSession != nil && env.AgentSession.ID == asA.ID:
			fromA = append(fromA, env.PromptID)
		case env.AgentSession != nil && env.AgentSession.ID == asB.ID:
			fromB = append(fromB, env.PromptID)
		}
		return false
	})

	pushEvent(asA, EnrichedEvent{Kind:KindAgentEvent, PromptID: "PA-1", AgentEvent: makeTextEvent("a1")})
	pushEvent(asB, EnrichedEvent{Kind:KindAgentEvent, PromptID: "PB-1", AgentEvent: makeTextEvent("b1")})
	pushEvent(asB, EnrichedEvent{Kind:KindAgentEvent, PromptID: "PB-2", AgentEvent: makeTextEvent("b2")})
	pushEvent(asA, EnrichedEvent{Kind:KindAgentEvent, PromptID: "PA-2", AgentEvent: makeTextEvent("a2")})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(fromA) + len(fromB)
		mu.Unlock()
		if n == 4 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	wantA := []string{"PA-1", "PA-2"}
	wantB := []string{"PB-1", "PB-2"}
	if !equalSlices(fromA, wantA) {
		t.Errorf("fromA = %v, want %v", fromA, wantA)
	}
	if !equalSlices(fromB, wantB) {
		t.Errorf("fromB = %v, want %v", fromB, wantB)
	}
}

// TestMultiAS_RouteEventUsesSourceNotSelected — routeEvent receives
// the AS passed by the subscription closure, NOT cs.selectedAS.
// Phase 1 invariant #12: the receiver never reads selectedAS to
// infer event source.
func TestMultiAS_RouteEventUsesSourceNotSelected(t *testing.T) {
	cs := newChatSessionForTest("cs_route_src")

	asA := NewAgentSession("as_A", cs.ID, "pi", "/tmp", nil)
	asB := NewAgentSession("as_B", cs.ID, "claude", "/tmp", nil)
	cs.attachAgentSession(asA)
	cs.attachAgentSession(asB)
	cs.selectAgentSessionLocked(asB) // selected = B

	var observedSource string
	cs.AgentEventBus.Subscribe(func(env AgentEventEnvelope) bool {
		if env.AgentSession != nil {
			observedSource = env.AgentSession.ID
		}
		return false
	})

	// Push from A (not selected).
	pushEvent(asA, EnrichedEvent{Kind:KindAgentEvent, AgentEvent: makeTextEvent("a")})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && observedSource == "" {
		time.Sleep(5 * time.Millisecond)
	}
	if observedSource != asA.ID {
		t.Errorf("observed source = %q, want %q (routeEvent must use AS source, not selected)", observedSource, asA.ID)
	}
}

// TestMultiAS_DetachedKeepsReceiving — an AS in StatusDetached (process
// state unknown) still receives push events via its bus. ChatSession
// subscribers keep firing.
func TestMultiAS_DetachedKeepsReceiving(t *testing.T) {
	cs := newChatSessionForTest("cs_detached_recv")

	as := NewAgentSession("as_det", cs.ID, "pi", "/tmp", nil)
	cs.attachAgentSession(as)
	as.SetDetached()

	var hits atomic.Int32
	cs.AgentEventBus.Subscribe(func(_ AgentEventEnvelope) bool {
		hits.Add(1)
		return false
	})

	pushEvent(as, EnrichedEvent{Kind:KindAgentEvent, AgentEvent: makeTextEvent("from detached")})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hits.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if hits.Load() == 0 {
		t.Error("bus subscriber never fired for detached AS")
	}
}

// TestMultiAS_AllASIndependent — N ASes each run their own
// endPrompt lifecycle independently. Events from one don't bleed
// into another.
func TestMultiAS_AllASIndependent(t *testing.T) {
	cs := newChatSessionForTest("cs_indep")

	ases := []*AgentSession{
		NewAgentSession("as_i1", cs.ID, "pi", "/code/i1", nil),
		NewAgentSession("as_i2", cs.ID, "claude", "/code/i2", nil),
		NewAgentSession("as_i3", cs.ID, "codex", "/code/i3", nil),
	}
	for _, as := range ases {
		cs.attachAgentSession(as)
	}

	var (
		mu    sync.Mutex
		count = make(map[string]int)
	)
	cs.AgentEventBus.Subscribe(func(env AgentEventEnvelope) bool {
		mu.Lock()
		defer mu.Unlock()
		if env.AgentSession != nil {
			count[env.AgentSession.ID]++
		}
		return false
	})

	for _, as := range ases {
		for i := 0; i < 3; i++ {
			pushEvent(as, EnrichedEvent{
				Kind:       KindAgentEvent,
				AgentEvent: makeTextEvent("event"),
			})
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		total := 0
		for _, n := range count {
			total += n
		}
		mu.Unlock()
		if total == 9 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, as := range ases {
		if count[as.ID] != 3 {
			t.Errorf("count[%s] = %d, want 3", as.ID, count[as.ID])
		}
	}
}

// equalSlices — helper for slice equality.
func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ensure the agent package is referenced (some build configurations).
var _ = agent.EventAgentText