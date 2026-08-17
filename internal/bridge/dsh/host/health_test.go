// health_test.go — HealthProbe unit tests using mock HTTP server.
//
// The probe's contract is small (GET /health + strike counter +
// forceKill callback), so the tests focus on the boundary
// conditions:
//   - success resets the strike counter
//   - 3 consecutive failures trigger onFailure exactly once
//   - the probe follows URL changes (respawn scenario)
//   - Stop blocks until the goroutine exits
//
// We use httptest.Server for the dsh health endpoint — fast,
// deterministic, no external dependency. We construct a real
// *Client rooted at the test server URL (via New + BaseURL only —
// no need to actually dial mux/host WS for these tests) so the
// probe's clientGetter returns a non-nil value.

package host

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testProbe creates a HealthProbe with shorter intervals so tests
// don't have to wait the full 30s for a tick. Returns the probe
// plus a function that triggers one tick synchronously.
func testProbe(t *testing.T, ts *httptest.Server, onFailure func()) (*HealthProbe, *Client) {
	t.Helper()
	cli := New(ts.URL, nil)
	probe := NewHealthProbe(
		func() *Client { return cli },
		onFailure,
		nil,
	)
	// Override the interval / strikesMax for fast tests.
	probe.interval = 10 * time.Millisecond
	probe.timeout = 200 * time.Millisecond
	probe.strikesMax = 3
	probe.path = "/health"
	return probe, cli
}

// TestHealthProbe_SuccessResetsStrikes verifies that a successful
// probe clears any accumulated failure count.
func TestHealthProbe_SuccessResetsStrikes(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	probe, _ := testProbe(t, ts, nil)

	// Drive several ticks manually — all succeed, no strikes.
	for i := 0; i < 5; i++ {
		probe.tick()
	}
	if got := probe.Strikes(); got != 0 {
		t.Errorf("expected 0 strikes after all-success ticks, got %d", got)
	}
}

// TestHealthProbe_FailuresAccumulateThenTrigger verifies that
// strikesMax consecutive failures invoke onFailure exactly once.
func TestHealthProbe_FailuresAccumulateThenTrigger(t *testing.T) {
	// Server always returns 500 — every probe is a failure.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	var (
		triggered atomic.Int32
		probe      *HealthProbe
	)
	probe, _ = testProbe(t, ts, func() { triggered.Add(1) })

	// First two failures: count up but don't trigger.
	probe.tick()
	probe.tick()
	if got := probe.Strikes(); got != 2 {
		t.Errorf("after 2 failures: expected strikes=2, got %d", got)
	}
	if triggered.Load() != 0 {
		t.Errorf("expected no trigger yet, got %d", triggered.Load())
	}

	// Third failure: trigger fires.
	probe.tick()
	if got := probe.Strikes(); got != 0 {
		t.Errorf("after trigger: expected strikes reset to 0, got %d", got)
	}
	if triggered.Load() != 1 {
		t.Errorf("expected onFailure to fire once, got %d", triggered.Load())
	}

	// Fourth failure starts a new strike cycle.
	probe.tick()
	if got := probe.Strikes(); got != 1 {
		t.Errorf("after 4th failure: expected strikes=1, got %d", got)
	}
}

// TestHealthProbe_NetworkErrorCounts verifies that transport-level
// failures (not just HTTP 500s) are counted as strikes.
func TestHealthProbe_NetworkErrorCounts(t *testing.T) {
	// Server is closed immediately — probes get connection refused.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ts.Close()

	var triggered atomic.Int32
	probe, _ := testProbe(t, ts, func() { triggered.Add(1) })

	// Each tick should hit "connection refused" and count as a strike.
	for i := 0; i < 3; i++ {
		probe.tick()
	}
	if triggered.Load() != 1 {
		t.Errorf("expected 1 trigger from 3 network failures, got %d", triggered.Load())
	}
	if probe.Strikes() != 0 {
		t.Errorf("expected strikes reset after trigger, got %d", probe.Strikes())
	}
}

// TestHealthProbe_RecoversAfterTransientFailure verifies that a
// single success between failures clears the strike count and
// prevents an unnecessary trigger.
//
// Sequence: fail, fail, success, fail, fail → strikes should never
// reach 3, so onFailure must NOT fire.
func TestHealthProbe_RecoversAfterTransientFailure(t *testing.T) {
	// The handler fails exactly 2 requests, then succeeds, then
	// fails 2 more. We use a small "responses" script so the
	// failure pattern is deterministic regardless of test
	// ordering.
	script := []bool{
		false, // probe.tick() → fail
		false, // fail
		true,  // success (resets strikes)
		false, // fail
		false, // fail
	}
	var i atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := i.Add(1) - 1
		if int(idx) >= len(script) {
			// No script entry — default to success.
			w.WriteHeader(http.StatusOK)
			return
		}
		if script[idx] {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	var triggered atomic.Int32
	probe, _ := testProbe(t, ts, func() { triggered.Add(1) })

	probe.tick() // fail → strikes=1
	if probe.Strikes() != 1 {
		t.Fatalf("after fail #1: expected strikes=1, got %d", probe.Strikes())
	}
	probe.tick() // fail → strikes=2
	if probe.Strikes() != 2 {
		t.Fatalf("after fail #2: expected strikes=2, got %d", probe.Strikes())
	}
	probe.tick() // success → strikes reset to 0
	if probe.Strikes() != 0 {
		t.Fatalf("after success: expected strikes=0, got %d", probe.Strikes())
	}
	probe.tick() // fail → strikes=1
	probe.tick() // fail → strikes=2

	if triggered.Load() != 0 {
		t.Errorf("expected no trigger (strikes never hit 3), got %d", triggered.Load())
	}
	if probe.Strikes() != 2 {
		t.Errorf("expected strikes=2 at end, got %d", probe.Strikes())
	}
}

// TestHealthProbe_FollowsURLChange verifies that the probe picks
// up URL changes (e.g. across a dsh respawn). We swap the
// clientGetter's return value via a shared holder and verify
// the second URL is hit on the next tick.
func TestHealthProbe_FollowsURLChange(t *testing.T) {
	var (
		firstHit  atomic.Int32
		secondHit atomic.Int32
	)

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstHit.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer first.Close()

	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHit.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()

	var current atomic.Pointer[Client]
	firstClient := New(first.URL, nil)
	current.Store(firstClient)
	probe := NewHealthProbe(func() *Client { return current.Load() }, nil, nil)
	probe.interval = 10 * time.Millisecond
	probe.timeout = 200 * time.Millisecond

	probe.tick()
	if firstHit.Load() != 1 || secondHit.Load() != 0 {
		t.Fatalf("after tick 1: firstHit=%d secondHit=%d (want 1,0)",
			firstHit.Load(), secondHit.Load())
	}

	// Simulate a respawn — new dsh at a different URL.
	secondClient := New(second.URL, nil)
	current.Store(secondClient)

	probe.tick()
	if secondHit.Load() != 1 {
		t.Errorf("after tick 2 with swapped client: secondHit=%d (want 1)",
			secondHit.Load())
	}
	if firstHit.Load() != 1 {
		t.Errorf("firstHit should not increase after swap, got %d", firstHit.Load())
	}
}

// TestHealthProbe_NilClientNoPanic verifies the probe tolerates a
// nil client (e.g. brief window during Close when h.Client has
// been cleared). The probe should count it as a strike and log,
// not panic.
func TestHealthProbe_NilClientNoPanic(t *testing.T) {
	var triggered atomic.Int32
	probe := NewHealthProbe(func() *Client { return nil }, func() { triggered.Add(1) }, nil)
	probe.interval = 10 * time.Millisecond
	probe.timeout = 200 * time.Millisecond
	probe.strikesMax = 2

	// Should not panic.
	probe.tick()
	probe.tick()
	if triggered.Load() != 1 {
		t.Errorf("expected 1 trigger from 2 nil-client failures, got %d", triggered.Load())
	}
}

// TestHealthProbe_StopBlocksUntilExit verifies the Start/Stop
// contract: Start launches a goroutine, Stop signals it and
// waits for Done to close. Run the goroutine for a couple of
// ticks to make sure it's actually doing work.
func TestHealthProbe_StopBlocksUntilExit(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	probe, _ := testProbe(t, ts, nil)
	probe.interval = 5 * time.Millisecond
	probe.timeout = 200 * time.Millisecond

	probe.Start()
	// Let the goroutine run a few ticks.
	time.Sleep(50 * time.Millisecond)
	if hits.Load() == 0 {
		t.Fatalf("expected goroutine to have ticked at least once, hits=%d", hits.Load())
	}

	// Stop should close Done.
	done := probe.Done()
	select {
	case <-done:
		// Already exited — shouldn't happen during a healthy run.
		t.Fatal("Done was already closed before Stop was called")
	default:
	}
	probe.Stop()

	select {
	case <-done:
		// Good — Stop blocked until goroutine exited.
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not unblock Done within 2s")
	}

	// Stop is idempotent — calling twice should not deadlock or panic.
	probe.Stop()
}

// TestHealthProbe_ConcurrentTicksSafe exercises the strike counter
// under concurrent tick invocations (the run loop calls tick()
// on the goroutine; tests do the same). The mutex around strikes
// should make this race-free per `go test -race`.
func TestHealthProbe_ConcurrentTicksSafe(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	var (
		triggered atomic.Int32
		wg        sync.WaitGroup
	)
	probe, _ := testProbe(t, ts, func() { triggered.Add(1) })

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			probe.tick()
		}()
	}
	wg.Wait()

	// Don't assert exact strike count (race-prone), just that
	// the trigger fired (or didn't) without a panic.
	_ = triggered.Load()
}