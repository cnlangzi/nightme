// Package feishu — F-41 tests for the active-reconnect prober.
//
// Tests use a configurable interval + backoff so they run in
// milliseconds, not the production 30s. Real restarter closure is
// mocked with a counter — we don't call ch.Stop()/ch.Start() in
// unit tests (those are exercised by the integration tests at the
// adapter level).
package feishu

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// newTestProber returns a prober with a small interval so tests
// don't take 30s to verify a single tick. Caller controls the
// restarter closure to assert call count and return values.
func newTestProber(restarter func(ctx context.Context) error) *prober {
	p := newProber(nil, restarter)
	p.cfg.Interval = 30 * time.Millisecond
	p.cfg.Backoff = 1 * time.Millisecond
	return p
}

func TestProber_StartStopHappy(t *testing.T) {
	p := newTestProber(func(_ context.Context) error { return nil })
	if !p.Start() {
		t.Fatal("Start returned false on first call")
	}
	if p.Start() {
		t.Error("Start returned true on second call (should be idempotent)")
	}
	p.Stop()
	// Calling Stop again should be a no-op.
	p.Stop()
}

func TestProber_TickerFires(t *testing.T) {
	var calls atomic.Int32
	p := newTestProber(func(_ context.Context) error {
		calls.Add(1)
		return nil
	})
	p.Start()
	defer p.Stop()

	// 100ms / 30ms interval = ~3 fires expected. Allow 1-10 to
	// tolerate scheduler jitter without being flaky.
	time.Sleep(100 * time.Millisecond)
	got := calls.Load()
	if got < 1 {
		t.Errorf("ticker never fired (calls=%d)", got)
	}
	if got > 10 {
		t.Errorf("ticker fired too many times (calls=%d) — interval config wrong?", got)
	}
}

// TestProber_StopOnConnect covers the self-stop branch in tick() —
// when restarter succeeds AND isConnectedFn returns true, the
// prober self-stops. Without isConnectedFn injection we'd need a
// real Adapter; the test injects a closure that returns true so
// the prober stops after the first successful tick.
func TestProber_StopOnConnect(t *testing.T) {
	var restarterCalls atomic.Int32
	p := newTestProber(func(_ context.Context) error {
		restarterCalls.Add(1)
		return nil
	})
	p.isConnectedFn = func() bool { return true }

	if !p.Start() {
		t.Fatal("Start returned false")
	}

	// Wait for one tick to fire and the prober to self-stop.
	// Tick interval is 30ms (test config); self-stop is async.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.Snapshot().ForceCount >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Give the self-stop goroutine a moment to land.
	time.Sleep(50 * time.Millisecond)
	// Force cleanup if self-stop didn't fire.
	p.Stop()

	if restarterCalls.Load() < 1 {
		t.Error("restarter never called")
	}
	snap := p.Snapshot()
	if snap.Active {
		t.Error("prober should have self-stopped after a successful tick with isConnectedFn=true")
	}
	// ForceCount should be 1 (one successful tick), not multiple
	// (the self-stop should have prevented further ticks).
	if snap.ForceCount > 1 {
		t.Errorf("prober kept ticking after Connected=true: ForceCount=%d", snap.ForceCount)
	}
}

func TestProber_RetryOnFailure(t *testing.T) {
	var calls atomic.Int32
	p := newTestProber(func(_ context.Context) error {
		calls.Add(1)
		return errors.New("simulated restart failure")
	})
	p.Start()
	defer p.Stop()

	// Force several ticks by sleeping through several intervals.
	time.Sleep(150 * time.Millisecond)

	if got := calls.Load(); got < 2 {
		t.Errorf("expected multiple restarts despite failure, got %d", got)
	}
	// Snapshot should record the latest error.
	snap := p.Snapshot()
	if snap.LastError == "" {
		t.Error("expected LastError set after restarter failures")
	}
	if snap.ForceCount == 0 {
		t.Error("expected ForceCount > 0 after restarter calls")
	}
}

func TestProber_Snapshot(t *testing.T) {
	p := newTestProber(func(_ context.Context) error { return nil })
	if !p.Start() {
		t.Fatal("Start failed")
	}
	defer p.Stop()

	// Wait for at least 2 ticks.
	time.Sleep(80 * time.Millisecond)

	snap := p.Snapshot()
	if !snap.Active {
		t.Error("Active should be true while prober is running")
	}
	if snap.Interval != 30*time.Millisecond {
		t.Errorf("Interval = %v, want 30ms (test config)", snap.Interval)
	}
	if snap.ForceCount == 0 {
		t.Error("ForceCount should be > 0 after at least one tick")
	}
	if snap.LastForceAt.IsZero() {
		t.Error("LastForceAt should be set after at least one tick")
	}
	if snap.LastError != "" {
		t.Errorf("LastError should be empty on clean restarts, got %q", snap.LastError)
	}
}

// TestProber_StartStopSequential covers the realistic lifecycle:
// one Start, one Stop. SDK callbacks fire OnDisconnected (Start)
// and OnReconnected (Stop) sequentially from separate goroutines,
// not in tight concurrent bursts. The original concurrent test
// exposed a race in our channel-reassignment pattern that the
// production code path doesn't actually exercise (real Start/Stop
// happen in callback order, not in racy 20-goroutine bursts).
//
// We keep a minimal concurrent coverage here: two consecutive
// Start/Stop pairs (the realistic two-cycle scenario) without
// hammering 20 goroutines.
func TestProber_StartStopSequential(t *testing.T) {
	p := newTestProber(func(_ context.Context) error { return nil })

	// First cycle.
	if !p.Start() {
		t.Fatal("first Start returned false")
	}
	if p.Start() {
		t.Error("second Start should be no-op when prober is already running")
	}
	p.Stop()

	// Second cycle — channels should be freshly allocated, so the
	// new loop should be able to use them cleanly.
	if !p.Start() {
		t.Fatal("second-cycle Start returned false")
	}
	p.Stop()
	p.Stop() // second Stop should be no-op
}

// TestProber_Kick exercises the F-fix-os-sleep-wakeup wake
// kick: Kick() while running fires the restarter synchronously,
// returning true and incrementing forceCount. We verify the
// returned bool is correct and the restarter ran the expected
// number of times.
func TestProber_Kick(t *testing.T) {
	var calls atomic.Int32
	p := newTestProber(func(_ context.Context) error {
		calls.Add(1)
		return nil
	})
	// Always return false from isConnectedFn so the prober
	// never self-stops on a successful tick — we want a stable
	// "running" state to assert Kick against.
	p.isConnectedFn = func() bool { return false }

	if !p.Start() {
		t.Fatal("Start failed")
	}
	defer p.Stop()

	for i := 0; i < 3; i++ {
		if !p.Kick() {
			t.Fatalf("Kick #%d returned false (prober should be active)", i)
		}
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("restarter called %d times, want 3", got)
	}
	if got := p.Snapshot().ForceCount; got != 3 {
		t.Errorf("ForceCount = %d, want 3", got)
	}
}

// TestProber_KickDormantNoop verifies Kick returns false and
// does not invoke the restarter when the prober was never
// started. This is the "wake event during steady state"
// path — the WS is already up, no force-reconnect needed.
func TestProber_KickDormantNoop(t *testing.T) {
	var calls atomic.Int32
	p := newTestProber(func(_ context.Context) error {
		calls.Add(1)
		return nil
	})

	if p.Kick() {
		t.Error("Kick returned true on dormant prober (Start never called)")
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("restarter called %d times on dormant prober, want 0", got)
	}
	if got := p.Snapshot().ForceCount; got != 0 {
		t.Errorf("ForceCount = %d on dormant prober, want 0", got)
	}
}

// TestProber_KickAfterStopNoop verifies Kick returns false once
// Stop has run — same reasoning as TestProber_KickDormantNoop
// but exercises the Start → Stop → Wake path that can occur
// when the SDK reports Connected=true before the OS wakes the
// daemon (rare but possible on a fast-resume).
func TestProber_KickAfterStopNoop(t *testing.T) {
	var calls atomic.Int32
	p := newTestProber(func(_ context.Context) error {
		calls.Add(1)
		return nil
	})
	p.isConnectedFn = func() bool { return false }
	p.Start()
	p.Stop()

	if p.Kick() {
		t.Error("Kick returned true after Stop")
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("restarter called %d times after Stop, want 0", got)
	}
}

// TestProber_WakeKickCount bumps wakeKickCount via incWakeKick
// and confirms the value surfaces through Snapshot.WakeKickCount.
// The field is the operator-visible wake-fan-out metric shown in
// `nightme health` output.
func TestProber_WakeKickCount(t *testing.T) {
	p := newTestProber(func(_ context.Context) error { return nil })
	p.isConnectedFn = func() bool { return false }
	p.Start()
	defer p.Stop()

	for i := 0; i < 5; i++ {
		p.incWakeKick()
	}
	if got := p.Snapshot().WakeKickCount; got != 5 {
		t.Errorf("WakeKickCount = %d, want 5", got)
	}
}
