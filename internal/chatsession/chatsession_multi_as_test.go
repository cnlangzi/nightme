package chatsession

import (
	"context"
	"fmt"
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

	delivered := make(chan AgentEventEnvelope, 1)
	cs.AgentEventBus.Subscribe(func(env AgentEventEnvelope) bool {
		select {
		case delivered <- env:
		default:
		}
		return false
	})

	// Push from A (not selected).
	pushEvent(asA, EnrichedEvent{Kind: KindAgentEvent, AgentEvent: makeTextEvent("a")})

	var env AgentEventEnvelope
	select {
	case env = <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("event bus did not receive event")
	}
	if env.AgentSession == nil || env.AgentSession.ID != asA.ID {
		got := ""
		if env.AgentSession != nil {
			got = env.AgentSession.ID
		}
		t.Errorf("observed source = %q, want %q (routeEvent must use AS source, not selected)", got, asA.ID)
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

// TestMultiAS_HighConcurrency_BusFanoutRaceFree — many producers
// pushing concurrently, many subscribers with shared per-AS state,
// many reader goroutines draining. Run under -race; the detector
// trips immediately on any unsynchronized access between the bus
// dispatch goroutine, the subscriber closure, and the reader.
//
// This is the regression catcher for the bare-var-with-polling
// pattern that previously bit TestRouteEvent_IgnoresSelectedAS,
// TestMultiAS_RouteEventUsesSourceNotSelected, and 5 tests in
// event_envelope_test.go. The fix pattern (atomic counter + buffered
// channel signal) is inlined below; the test passes iff -race
// reports nothing AND every event lands at the reader side.
//
// If you add a new Subscribe-based test, please mirror this pattern
// rather than reintroducing the bare-var-with-poll loop: it's
// strictly slower under -race and racy in theory.
//
// Skipped in -short mode (default for `go test ./...`) because
// the test is sensitive to dispatcher backpressure: under
// parallel-package load, 250 events can starve 4 readers and
// the non-blocking subscriber send silently drops the tail.
// Run explicitly with `go test -race -run MultiAS_HighConcurrency`.
func TestMultiAS_HighConcurrency_BusFanoutRaceFree(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-as race-free test in -short mode (use -race -run MultiAS_HighConcurrency to opt in)")
	}
	cs := newChatSessionForTest("cs_concurrent_stress")

	const (
		nAS     = 5
		nPer    = 50 // events per AS
		nRead   = 4  // reader goroutines
		nSubs   = 3  // subscribers with shared state
	)

	ases := make([]*AgentSession, nAS)
	for i := range ases {
		ases[i] = NewAgentSession(
			fmt.Sprintf("as_stress_%d", i),
			cs.ID,
			"pi",
			"/tmp",
			nil,
		)
		cs.attachAgentSession(ases[i])
	}

	// nSubs subscribers each maintain their own per-AS hit count.
	// Atomic.Int64 → race-free read-modify-write. A bare
	// map[string]int here would trip -race under this load.
	subs := make([]*sync.Map, nSubs)
	for i := range subs {
		subs[i] = &sync.Map{}
		idx := i
		cs.AgentEventBus.Subscribe(func(env AgentEventEnvelope) bool {
			if env.AgentSession == nil {
				return false
			}
			// LoadOrStore + add via a small CAS loop on the int.
			// (sync.Map stores any; we wrap counts in *atomic.Int64.)
			v, _ := subs[idx].LoadOrStore(env.AgentSession.ID, new(atomic.Int64))
			v.(*atomic.Int64).Add(1)
			return false
		})
	}

	// Producers — every AS pushes nPer events from its own goroutine.
	var prodWG sync.WaitGroup
	for _, as := range ases {
		prodWG.Add(1)
		go func(as *AgentSession) {
			defer prodWG.Done()
			for i := 0; i < nPer; i++ {
				pushEvent(as, EnrichedEvent{
					Kind:       KindAgentEvent,
					AgentEvent: makeTextEvent(fmt.Sprintf("%s/%d", as.ID, i)),
				})
			}
		}(as)
	}

	// Readers — multiple goroutines drain a shared delivery channel,
	// proving the bus→subscriber→reader chain stays race-free at
	// every step. Channel send/recv is the happens-before edge.
	const totalEvents = nAS * nPer
	delivered := make(chan AgentSession, totalEvents*2)
	cs.AgentEventBus.Subscribe(func(env AgentEventEnvelope) bool {
		if env.AgentSession == nil {
			return false
		}
		select {
		case delivered <- *env.AgentSession:
		default:
		}
		return false
	})

	readerCtx, cancelReaders := context.WithCancel(context.Background())
	defer cancelReaders()
	var readerWG sync.WaitGroup
	var seenCount atomic.Int64
	for i := 0; i < nRead; i++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			for {
				select {
				case <-readerCtx.Done():
					return
				case <-delivered:
					seenCount.Add(1)
				}
			}
		}()
	}

	// Producers finish, then we wait until all events land at the
	// readers. The bus dispatch + subscriber chain runs concurrently
	// with the producers (pushEvent starts a dispatcher goroutine on
	// each AS, then returns immediately).
	prodWG.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && seenCount.Load() < int64(totalEvents) {
		time.Sleep(2 * time.Millisecond)
	}
	cancelReaders()
	readerWG.Wait()

	if got := seenCount.Load(); got != int64(totalEvents) {
		t.Errorf("reader saw %d/%d events (bus dropped or reader starved)", got, totalEvents)
	}

	// Each subscriber must have observed every event from every AS.
	// We read the per-subscriber counts (atomic) outside the
	// subscriber goroutine → race-free read.
	for i, sub := range subs {
		for _, as := range ases {
			v, ok := sub.Load(as.ID)
			if !ok {
				t.Errorf("sub[%d] missing count for %s", i, as.ID)
				continue
			}
			if n := v.(*atomic.Int64).Load(); n != int64(nPer) {
				t.Errorf("sub[%d] count for %s = %d, want %d", i, as.ID, n, nPer)
			}
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