package chatsession

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
)

// countSpawner wraps a Spawner and counts how many times Spawn
// was invoked. Used by the AS-reuse tests to assert the
// runtime doesn't respawn the bridge on every message.
type countSpawner struct {
	inner Spawner
	calls atomic.Int64
}

func (c *countSpawner) Spawn(ctx context.Context, agentName, cwd string, args []string, sessionID string) (*agent.Agent, error) {
	c.calls.Add(1)
	return c.inner.Spawn(ctx, agentName, cwd, args, sessionID)
}

// TestAS_ReuseAcrossMessages_SuccessPath is the **positive**
// contract: once an AS is successfully spawned, subsequent
// LookupSelectedAgentSession calls for the same (agent, cwd)
// tuple MUST NOT invoke the Spawner again. This is the property
// the design promises — "1 AS = 1 CLI process, many turns" —
// and a regression that breaks it would cause the per-message
// cmd window flash the user observed.
//
// We use a fakeSuccessfulSpawner that returns the same handle
// on every call. The first call wires the AS as Running;
// subsequent calls must short-circuit on the Running check.
func TestAS_ReuseAcrossMessages_SuccessPath(t *testing.T) {
	mgr := NewManager()
	cs, _ := mgr.GetOrCreate("oc_reuse_ok", "claude")
	cs.SetSelectedCwd("/tmp")
	cs.SetWatchMode(WatchModeAll)

	spy := &countSpawner{inner: &fakeSuccessfulSpawner{pid: 4242}}
	mgr.WithSpawner(spy)
	cs.WithSpawner(spy)

	// Drive N messages through the manager.
	const N = 5
	for i := 0; i < N; i++ {
		mgr.HandleInbound(context.Background(), &messages.InboundMessage{
			ChatID:    "oc_reuse_ok",
			MessageID: "om_reuse_ok_" + itoa(i),
			UserID:    "u_reuse_ok",
			Text:      "hi",
		})
		// HandleInbound queues the message; the AS lookup
		// happens synchronously inside HandleInbound before
		// the message is queued. Yield once to let the
		// queue+dispatcher settle (the test only asserts
		// counts/pid/status, not actual flush).
		time.Sleep(2 * time.Millisecond)
	}

	// Contract: only the FIRST message's lookup should have
	// called Spawn. Subsequent calls see the (Running,
	// handle != nil) AS in the pool and short-circuit.
	got := spy.calls.Load()
	if got != 1 {
		t.Errorf("Spawn should be called exactly once across %d messages, got %d (AS not reused)", N, got)
	}

	// The AS we end up with must be the one the spawner
	// returned — and it must stay Running.
	as := cs.SelectedAgentSession()
	if as == nil {
		t.Fatalf("expected a selected AS, got nil")
	}
	if as.PID() != 4242 {
		t.Errorf("AS pid=%d, want 4242 (single CLI process reused)", as.PID())
	}
	if as.Status() != StatusRunning {
		t.Errorf("AS status=%s, want %s (process must stay Running across messages)",
			as.Status(), StatusRunning)
	}
}

// TestAS_ReuseAcrossMessages_FailedSpawnPath documents the
// current behavior for a *failed* spawn: the AS stays Detached
// (never reached Running), so LookupSelectedAgentSession re-
// attempts Spawn on every subsequent message. This is intentional
// — a transient "binary missing" should not block the user
// forever — but we pin it so a future refactor that breaks the
// retry path gets caught.
func TestAS_ReuseAcrossMessages_FailedSpawnPath(t *testing.T) {
	mgr := NewManager()
	cs, _ := mgr.GetOrCreate("oc_reuse_fail", "claude")
	cs.SetSelectedCwd("/tmp")
	cs.SetWatchMode(WatchModeAll)

	spy := &countSpawner{inner: &fakeFailingSpawner{err: errors.New("test: spawn fail")}}
	mgr.WithSpawner(spy)
	cs.WithSpawner(spy)

	const N = 3
	for i := 0; i < N; i++ {
		mgr.HandleInbound(context.Background(), &messages.InboundMessage{
			ChatID:    "oc_reuse_fail",
			MessageID: "om_reuse_fail_" + itoa(i),
			UserID:    "u_reuse_fail",
			Text:      "hi",
		})
		time.Sleep(2 * time.Millisecond)
	}

	if got := spy.calls.Load(); got != int64(N) {
		t.Errorf("failed-spawn AS should be re-attempted per message: calls=%d, want=%d", got, N)
	}
}

// fakeSuccessfulSpawner is a test double that returns a stable
// agent.Agent handle. The handle's PID is fixed so the test
// can assert identity across the reuse path.
type fakeSuccessfulSpawner struct {
	pid int
	mu  sync.Mutex
	ag  *agent.Agent
}

func (f *fakeSuccessfulSpawner) Spawn(ctx context.Context, agentName, cwd string, args []string, sessionID string) (*agent.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ag != nil {
		// Return the SAME handle — proves the runtime reuses
		// the AS across lookups without us re-spawning.
		return f.ag, nil
	}
	f.ag = newReuseTestAgent(f.pid)
	return f.ag, nil
}

// newReuseTestAgent builds a minimal *agent.Agent that exposes
// a fixed PID and a never-closing Events channel. The runtime
// only reads PID() and (transitively) Status(); the bridge
// interface methods are stubbed because the test never submits.
func newReuseTestAgent(pid int) *agent.Agent {
	return agent.NewAgent(
		agent.NewInfo("fake", agent.ModePTY, "fake", nil, nil),
		pid,
		make(chan agent.AgentEvent, 32), // never closed → readpump stays alive
		&reuseTestDriver{},
	)
}

// reuseTestDriver is a no-op implementation of the package-
// private agent.driver interface. The reuse tests never submit
// blocks, so Send/Reset/Close are no-ops; they exist only to
// satisfy the constructor's driver interface assertion.
type reuseTestDriver struct{}

func (*reuseTestDriver) SendBlocks(_ context.Context, _ []agent.ContentBlock) error { return nil }
func (*reuseTestDriver) SendPermission(_ string) error                              { return nil }
func (*reuseTestDriver) Reset(_ context.Context) error                              { return nil }
func (*reuseTestDriver) Close() error                                               { return nil }
func (*reuseTestDriver) Stop(_ context.Context) error                              { return nil }
func (*reuseTestDriver) SetModel(_ context.Context, _, _ string) error              { return nil }

// itoa is a local strconv-free integer → string helper.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
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
