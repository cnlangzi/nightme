package agentsession

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/registry"
)

// F-62 test coverage — AgentSession.ClearInFlight() contract:
//
//   - Idempotent: empty slice is a no-op, no persist call.
//   - Non-empty: in-memory slice becomes nil, persist fires once.
//   - Differs from endPrompt(reason): does not touch currentPrompt
//     or isReady (the AS is detached here, so the readPump
//     subscribers are gone).
//
// Run with: go test ./internal/agentsession/... -run TestF62

// f62Persist is a thread-safe in-memory persist recorder. Same
// shape as f61Persist but kept separate so each F-XX test owns
// its recorder state.
type f62Persist struct {
	mu      sync.Mutex
	calls   int32
	lastErr error
	last    *registry.AgentSessionEntry
}

func (p *f62Persist) save(e *registry.AgentSessionEntry) error {
	atomic.AddInt32(&p.calls, 1)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last = e
	return p.lastErr
}

func (p *f62Persist) Calls() int32 {
	return atomic.LoadInt32(&p.calls)
}

// f62SeedInFlight mirrors the shape FromAgentSessionEntry uses
// to re-hydrate an AS after restore. Tests use this to put the AS
// into the "in-memory mirror has entries" state.
func f62SeedInFlight(as *AgentSession, msgs []registry.InFlightMessageRef) {
	as.asMu.Lock()
	as.inFlightMessages = msgs
	as.asMu.Unlock()
}

// TestF62_ClearInFlightEmptyIsNoop verifies that ClearInFlight on
// an AS with no in-flight does NOT persist (no churn on the
// agent_sessions.json file when nothing changes).
func TestF62_ClearInFlightEmptyIsNoop(t *testing.T) {
	as := newAgentSessionRuntime("as_f62_empty", "cs_x", "claude", "/tmp", nil)
	persist := &f62Persist{}
	as.SetPersist(persist.save)

	as.ClearInFlight()

	if got := persist.Calls(); got != 0 {
		t.Errorf("persist calls = %d, want 0 (no-op on empty)", got)
	}
}

// TestF62_ClearInFlightDropsInMemoryAndPersists verifies the
// happy path: a hydrated in-flight slice is cleared in memory and
// the empty state is persisted to disk.
func TestF62_ClearInFlightDropsInMemoryAndPersists(t *testing.T) {
	as := newAgentSessionRuntime("as_f62_drop", "cs_x", "claude", "/tmp", nil)
	persist := &f62Persist{}
	as.SetPersist(persist.save)

	now := time.Date(2026, 8, 14, 18, 0, 0, 0, time.UTC)
	f62SeedInFlight(as, []registry.InFlightMessageRef{
		{
			ID: "m_drop_1",
			Blocks: []agent.ContentBlock{
				{Type: agent.ContentText, Text: "before crash"},
			},
			ReceivedAt: now,
		},
	})

	as.ClearInFlight()

	// In-memory mirror is nil after the call.
	if got := as.Entry().InFlightMessages; len(got) != 0 {
		t.Errorf("after ClearInFlight, Entry().InFlightMessages = %v, want empty", got)
	}

	// Persist fires exactly once with the empty state.
	if got := persist.Calls(); got != 1 {
		t.Errorf("persist calls = %d, want 1", got)
	}
	if persist.last == nil {
		t.Fatal("persist.last is nil")
	}
	if len(persist.last.InFlightMessages) != 0 {
		t.Errorf("persisted InFlightMessages = %v, want empty", persist.last.InFlightMessages)
	}
}

// TestF62_ClearInFlightIdempotent makes sure calling ClearInFlight
// twice in a row is safe: the second call is a no-op (no persist
// churn).
func TestF62_ClearInFlightIdempotent(t *testing.T) {
	as := newAgentSessionRuntime("as_f62_idem", "cs_x", "claude", "/tmp", nil)
	persist := &f62Persist{}
	as.SetPersist(persist.save)

	f62SeedInFlight(as, []registry.InFlightMessageRef{
		{ID: "m_idem_1", Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "x"}}, ReceivedAt: time.Now()},
	})

	as.ClearInFlight()
	as.ClearInFlight()

	if got := persist.Calls(); got != 1 {
		t.Errorf("persist calls = %d, want 1 (second call must be no-op)", got)
	}
}

// TestF62_ClearInFlightLeavesOtherStateAlone verifies that the
// method does NOT touch currentPrompt / isReady / pid / status —
// the in-flight drop is the only side effect. Differs from
// endPrompt(reason) which also flips those.
func TestF62_ClearInFlightLeavesOtherStateAlone(t *testing.T) {
	as := newAgentSessionRuntime("as_f62_state", "cs_x", "claude", "/tmp", nil)
	persist := &f62Persist{}
	as.SetPersist(persist.save)

	as.SetRunning(12345)
	f62SeedInFlight(as, []registry.InFlightMessageRef{
		{ID: "m_state_1", Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "y"}}, ReceivedAt: time.Now()},
	})

	as.ClearInFlight()

	if as.Status() != StatusRunning {
		t.Errorf("status after ClearInFlight = %s, want Running", as.Status())
	}
	if as.PID() != 12345 {
		t.Errorf("pid after ClearInFlight = %d, want 12345", as.PID())
	}
	if as.CurrentPrompt() != nil {
		t.Errorf("currentPrompt after ClearInFlight = %v, want nil", as.CurrentPrompt())
	}
}
