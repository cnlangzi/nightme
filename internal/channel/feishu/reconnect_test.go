// Package feishu — F-41 tests for the active-reconnect prober.
//
// Tests use a configurable interval + backoff so they run in
// milliseconds, not the production 30s. Real restarter closure is
// mocked with a counter — we don't call ch.Stop()/ch.Start() in
// unit tests (those are exercised by the integration tests at the
// adapter level).
package feishu

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestProber returns a prober with a small interval so tests
// don't take 30s to verify a single tick. Caller controls the
// restarter closure to assert call count and return values.
func newTestProber(restarter func() error) *prober {
	p := newProber(nil, restarter)
	p.cfg.Interval = 30 * time.Millisecond
	p.cfg.Backoff = 1 * time.Millisecond
	return p
}

func TestProber_StartStopHappy(t *testing.T) {
	p := newTestProber(func() error { return nil })
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
	p := newTestProber(func() error {
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

func TestProber_StopOnConnect(t *testing.T) {
	// prober.tick() should self-stop when restarter succeeds AND
	// adapter is connected. We simulate this by:
	//   1. building a prober with a non-nil adapter pointing to a
	//      mock that returns Connected=true
	//   2. feeding one tick
	//   3. verifying prober.Active becomes false
	//
	// Since we don't want to spin up a real Adapter, we observe
	// the equivalent: prober.tick() calls p.Stop() in a goroutine
	// when restarter succeeds. We confirm the goroutine is alive
	// briefly and then exit.
	var restarterCalls atomic.Int32
	p := newTestProber(func() error {
		restarterCalls.Add(1)
		return nil
	})
	// nil adapter means prober.tick() won't try to check Connected;
	// instead, the test verifies the restarter was called repeatedly.
	// The self-stop path requires a non-nil adapter; covered by
	// integration test TestAdapter_OnReconnected_StopsProber.
	p.Start()
	defer p.Stop()

	time.Sleep(80 * time.Millisecond)
	if restarterCalls.Load() < 1 {
		t.Errorf("restarter never called")
	}
}

func TestProber_RetryOnFailure(t *testing.T) {
	var calls atomic.Int32
	p := newTestProber(func() error {
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
	p := newTestProber(func() error { return nil })
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

func TestProber_ConcurrentStartStop(t *testing.T) {
	// Race: 10 goroutines call Start/Stop in parallel. Only one
	// Start should win; subsequent Stops should be no-ops.
	p := newTestProber(func() error { return nil })

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); p.Start() }()
		go func() { defer wg.Done(); p.Stop() }()
	}
	wg.Wait()

	// Final state: prober may or may not be running depending on
	// the last operation; either is valid. Just ensure the snapshot
	// doesn't panic.
	_ = p.Snapshot()
}