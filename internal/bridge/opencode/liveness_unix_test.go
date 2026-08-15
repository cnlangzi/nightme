// Tests for the opencode bridge liveness probe and SSE-survives-
// silence behavior.
//
// These tests cover two historical bugs:
//
//  1. The old http.Client.Timeout: 30 * time.Second cut off the
//     SSE stream any time opencode went silent for >30s (e.g.
//     during model load), causing "Working... → Done" with no
//     content. We split the client into http (short requests,
//     bounded by per-call context) and httpSSE (no Timeout —
//     lifetime governed by sseCancel + the liveness probe).
//
//  2. The liveness probe runs on a SEPARATE HTTP connection
//     (/api/health) so it never interferes with the SSE wire. We
//     verify it kills the session after livenessFailThreshold
//     consecutive failures and emits EventAgentError.
package opencode

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestClient_NoOverallTimeoutOnSSE verifies the dedicated SSE
// client has no http.Client.Timeout. Regression guard for the
// "SSE dies after 30 s of silence" bug. Uses an inline
// httptest.Server (not the package's fakeServer, which lives in
// agent_e2e_unix_test.go and is gated by //go:build !windows)
// so this regression guard runs on every platform.
func TestClient_NoOverallTimeoutOnSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Any 2xx is enough — we only care about the client's
		// timeout configuration here.
		w.WriteHeader(200)
	}))
	defer srv.Close()

	proc := &serverProc{baseURL: srv.URL}
	c := newClient(proc, "/tmp")

	if c.http.Timeout != 0 {
		t.Errorf("http.Timeout = %s, want 0 (per-request context handles it)", c.http.Timeout)
	}
	if c.httpSSE.Timeout != 0 {
		t.Errorf("httpSSE.Timeout = %s, want 0 (sseCancel handles it)", c.httpSSE.Timeout)
	}
	// httpSSE should still bound the response-header phase so a
	// dead server fails fast on connect — not on first byte read.
	if c.httpSSE.Transport == nil {
		t.Fatal("httpSSE.Transport is nil; ResponseHeaderTimeout not configured")
	}
	tr, ok := c.httpSSE.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("httpSSE.Transport type = %T, want *http.Transport", c.httpSSE.Transport)
	}
	if tr.ResponseHeaderTimeout == 0 {
		t.Error("httpSSE.Transport.ResponseHeaderTimeout = 0; SSE handshake can hang on dead server")
	}
	if tr.ResponseHeaderTimeout > 30*time.Second {
		t.Errorf("ResponseHeaderTimeout = %s, want <= 30s (response header phase only)", tr.ResponseHeaderTimeout)
	}
}

// TestSubscribe_SurvivesIdleSilence verifies that the SSE reader
// does NOT terminate just because the server has gone silent for
// longer than the old 30 s cut-off. We start a subscribe, let the
// connection sit silent for 2x the old timeout (60s would be too
// slow for a unit test, so we shrink the test by reading for a
// shorter interval that still proves the body is being held open
// past any fixed timeout).
//
// We can't easily wait 60s in a unit test, so we instead verify
// the structural property: the response body is held open by the
// subscribeGlobal path until the caller's context cancels. To
// prove this we use a fake server whose handler records the
// moment the SSE connection closes — and we assert it stays open
// for at least 1 second past where the old code would have
// cancelled. We then cancel the context and verify the connection
// closes within a short window.
func TestSubscribe_SurvivesIdleSilence(t *testing.T) {
	var (
		connected    atomic.Bool
		closedAt     atomic.Int64 // unix nano when handler returned
		clientCancel atomic.Bool
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/event", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		connected.Store(true)
		// Block until the client cancels. We do NOT push any
		// events — this is the "model thinking, no events for
		// ages" scenario the old 30 s Timeout used to mis-handle.
		<-r.Context().Done()
		clientCancel.Store(true)
		closedAt.Store(time.Now().UnixNano())
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	proc := &serverProc{baseURL: srv.URL}
	c := newClient(proc, "/tmp")

	// Open a session id first so the client doesn't reject. (We
	// don't care about session id semantics here.)
	ctx, cancel := context.WithCancel(context.Background())
	body, err := c.Subscribe(ctx, "ses_idle")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer body.Close()

	// Wait for the SSE handler to confirm it's connected.
	deadline := time.Now().Add(2 * time.Second)
	for !connected.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !connected.Load() {
		t.Fatal("SSE handler never registered connection")
	}

	// The OLD bug would have cancelled the body read at
	// ~30 s due to http.Client.Timeout. We can't wait 30 s in a
	// unit test, but we can read for 2 s (twice the historical
	// minimum we can prove here without slowing the suite) and
	// verify the connection is STILL alive (closedAt == 0).
	time.Sleep(2 * time.Second)
	if got := closedAt.Load(); got != 0 {
		t.Fatalf("SSE connection closed at %d — server hung up while client thought it was idle", got)
	}
	if clientCancel.Load() {
		t.Fatal("SSE handler saw ctx done; the bridge must have given up the body too early")
	}

	// Now cancel the caller's ctx — the SSE handler should see
	// r.Context().Done() fire and we should see a closedAt.
	cancel()
	deadline = time.Now().Add(2 * time.Second)
	for closedAt.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if closedAt.Load() == 0 {
		t.Fatal("SSE handler did not observe ctx cancellation within 2s of caller cancel")
	}
}

// TestLivenessLoop_KillsOnHealthFailures verifies that the
// liveness probe goroutine tears the session down after
// livenessFailThreshold consecutive /api/health failures.
//
// We shrink the probe interval/timeout via env vars so the test
// does not have to wait the production defaults (5s / 2s). The
// livenessProbeConfig() helper is what the goroutine reads.
func TestLivenessLoop_KillsOnHealthFailures(t *testing.T) {
	t.Setenv("NIGHTME_OPENCODE_LIVENESS_INTERVAL", "100ms")
	t.Setenv("NIGHTME_OPENCODE_LIVENESS_TIMEOUT", "50ms")
	t.Setenv("NIGHTME_OPENCODE_LIVENESS_THRESHOLD", "3")
	// A /api/health handler we can flip on/off from the test.
	var healthOK atomic.Bool
	healthOK.Store(true)
	var healthHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		healthHits.Add(1)
		if healthOK.Load() {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(503)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	d := &driver{
		name:        "opencode",
		command:     "opencode",
		workspace:   "/tmp",
		branch:      "main",
		events:      make(chan agent.AgentEvent, eventBufferSize),
		closed:      make(chan struct{}),
		stopDeliver: make(chan struct{}),
		exitDone:    make(chan struct{}),
		server:      &serverProc{baseURL: srv.URL, cmd: nil},
		client:      newClient(&serverProc{baseURL: srv.URL}, "/tmp"),
		sessionID:   "ses_test",
	}
	defer d.Close()

	// Speed the test up: shrink the probe interval & fail
	// threshold to small values. We cannot redefine the consts,
	// so we instead simulate by counting hits in a tight loop
	// after flipping healthOK off and confirming we get the
	// expected number of probes in a bounded window.

	// First confirm the probe runs at all and that health=ok
	// does not tear down.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		d.livenessLoop(ctx)
		close(done)
	}()

	// Wait until we've seen at least one successful probe. With
	// the env-overridden interval of 100ms this lands within ~200ms.
	deadline := time.Now().Add(2 * time.Second)
	for healthHits.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if healthHits.Load() < 1 {
		t.Fatal("liveness probe never hit /api/health")
	}

	// Flip health off. After livenessFailThreshold consecutive
	// failures the loop must return (signalling teardown).
	healthOK.Store(false)

	select {
	case <-done:
		// Good — loop exited.
	case <-time.After(8 * time.Second):
		t.Fatal("livenessLoop did not exit after livenessFailThreshold consecutive failures")
	}

	// We should have at least livenessFailThreshold failed probes.
	if got := healthHits.Load(); int(got) < livenessFailThreshold {
		t.Errorf("healthHits = %d, want >= %d (consecutive failed probes)", got, livenessFailThreshold)
	}

	// And an EventAgentError should have been emitted on the
	// events channel.
	deadline = time.Now().Add(1 * time.Second)
	var sawErr bool
	for time.Now().Before(deadline) {
		select {
		case ev := <-d.events:
			if ev.Kind == agent.EventAgentError {
				if ev.Err == nil {
					t.Error("EventAgentError emitted with nil Err")
				} else if !strings.Contains(ev.Err.Error(), "liveness probe") {
					t.Errorf("EventAgentError.Err = %v, want substring 'liveness probe'", ev.Err)
				}
				sawErr = true
			}
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !sawErr {
		t.Error("no EventAgentError on events channel after liveness exhaustion")
	}
}

// TestLivenessLoop_StopsOnContextCancel verifies the goroutine
// terminates when the parent ctx is cancelled (no leaked
// goroutines after Start/Close).
func TestLivenessLoop_StopsOnContextCancel(t *testing.T) {
	t.Setenv("NIGHTME_OPENCODE_LIVENESS_INTERVAL", "100ms")
	t.Setenv("NIGHTME_OPENCODE_LIVENESS_TIMEOUT", "50ms")
	t.Setenv("NIGHTME_OPENCODE_LIVENESS_THRESHOLD", "3")

	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	d := &driver{
		name:        "opencode",
		command:     "opencode",
		workspace:   "/tmp",
		branch:      "main",
		events:      make(chan agent.AgentEvent, eventBufferSize),
		closed:      make(chan struct{}),
		stopDeliver: make(chan struct{}),
		exitDone:    make(chan struct{}),
		server:      &serverProc{baseURL: srv.URL, cmd: nil},
		client:      newClient(&serverProc{baseURL: srv.URL}, "/tmp"),
		sessionID:   "ses_test",
	}
	defer d.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.livenessLoop(ctx)
		close(done)
	}()

	// Let it run a couple ticks.
	time.Sleep(livenessProbeInterval + 200*time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("livenessLoop did not exit after ctx cancel")
	}
}

// TestLivenessLoop_StopsOnClosed verifies the goroutine observes
// d.closed (set by Close()).
func TestLivenessLoop_StopsOnClosed(t *testing.T) {
	t.Setenv("NIGHTME_OPENCODE_LIVENESS_INTERVAL", "100ms")
	t.Setenv("NIGHTME_OPENCODE_LIVENESS_TIMEOUT", "50ms")
	t.Setenv("NIGHTME_OPENCODE_LIVENESS_THRESHOLD", "3")

	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	d := &driver{
		name:        "opencode",
		command:     "opencode",
		workspace:   "/tmp",
		branch:      "main",
		events:      make(chan agent.AgentEvent, eventBufferSize),
		closed:      make(chan struct{}),
		stopDeliver: make(chan struct{}),
		exitDone:    make(chan struct{}),
		server:      &serverProc{baseURL: srv.URL, cmd: nil},
		client:      newClient(&serverProc{baseURL: srv.URL}, "/tmp"),
		sessionID:   "ses_test",
	}
	defer d.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		d.livenessLoop(ctx)
		close(done)
	}()

	time.Sleep(livenessProbeInterval + 200*time.Millisecond)
	// Trigger Close() (which closes d.closed internally via
	// closeOnce). We avoid a manual close(d.closed) so the
	// test does not race with Close()'s own close call.
	d.Close()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("livenessLoop did not exit after Close()")
	}
}

// ─── SSE reconnect ───────────────────────────────────────────────

// TestSSELoop_ReconnectsAfterDisconnect simulates the opencode
// server briefly dropping the SSE connection (process restart,
// proxy idle, kernel blip). The bridge must reconnect on its own
// and resume delivering events on the second connection.
func TestSSELoop_ReconnectsAfterDisconnect(t *testing.T) {
	var (
		connMu       sync.Mutex
		connIndex    int           // how many SSE connections the test has served
		dropAfterOne atomic.Bool  // when true, the SSE handler exits after the first event
		servedEvents atomic.Int32  // total events written across all connections
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/event", func(w http.ResponseWriter, r *http.Request) {
		connMu.Lock()
		idx := connIndex
		connIndex++
		connMu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		// Push one event on the first connection; drop immediately.
		// On subsequent connections (post-reconnect), push one
		// event and stay open — the bridge should observe both.
		_, _ = io.WriteString(w, `data: {"type":"server.connected","properties":{}}`+"\n\n")
		servedEvents.Add(1)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if idx == 0 && dropAfterOne.Load() {
			// Hijack and close the TCP conn so the client sees
			// EOF and triggers reconnect.
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		// Stay open until the client cancels.
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Shrink backoff so the test runs fast.
	t.Setenv("NIGHTME_OPENCODE_SSE_RECONNECT_MIN", "20ms")

	dropAfterOne.Store(true)

	d := newDriverForSSE(t, srv.URL)
	defer d.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Stamp initial watchdog timer so the watchdog doesn't fire
	// (this test is about reconnect, not watchdog).
	d.lastEventAtUnixNano.Store(time.Now().UnixNano())

	done := make(chan struct{})
	go func() {
		d.sseLoop(ctx)
		close(done)
	}()

	// Wait for the test server to see at least 2 successful
	// connections AND at least 2 events served.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		connMu.Lock()
		ci := connIndex
		connMu.Unlock()
		if ci >= 2 && servedEvents.Load() >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	connMu.Lock()
	ci := connIndex
	connMu.Unlock()
	if ci < 2 {
		t.Fatalf("sseLoop never reconnected: connIndex=%d (want >=2)", ci)
	}
	if servedEvents.Load() < 2 {
		t.Fatalf("only %d events served across reconnects (want >=2)", servedEvents.Load())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sseLoop did not exit after ctx cancel")
	}
}

// TestSSELoop_NonRetryableStopsLoop verifies that a 4xx subscribe
// error (e.g. auth failure) does NOT trigger an infinite retry
// loop — the bridge gives up after one attempt.
func TestSSELoop_NonRetryableStopsLoop(t *testing.T) {
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/event", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "auth failed", http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("NIGHTME_OPENCODE_SSE_RECONNECT_MIN", "20ms")

	d := newDriverForSSE(t, srv.URL)
	defer d.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		d.sseLoop(ctx)
		close(done)
	}()

	// We expect exactly one Subscribe attempt (no retry).
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if hits.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1 (no retry on 4xx)", hits.Load())
	}

	select {
	case <-done:
		// Good: loop exited on its own after the non-retryable err.
	case <-time.After(500 * time.Millisecond):
		// Allow a brief moment for the goroutine to notice; if
		// it's still running it's in the wrong state.
		select {
		case <-done:
		default:
			t.Fatal("sseLoop kept running after non-retryable 4xx")
		}
	}

	if got := hits.Load(); got != 1 {
		t.Fatalf("hits after loop exit = %d, want 1", got)
	}
}

// TestSSELoop_StopsOnClosed verifies that d.Close() terminates
// an sseLoop that is currently in the backoff-and-retry path
// (Subscribe returning 5xx). Close() cancels sseCtx and signals
// d.closed; the loop must observe at least one of them and exit
// within the backoff window.
func TestSSELoop_StopsOnClosed(t *testing.T) {
	// Subscribe handler returns 503 forever so sseLoop is in the
	// backoff-and-retry path.
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/event", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("NIGHTME_OPENCODE_SSE_RECONNECT_MIN", "20ms")
	t.Setenv("NIGHTME_OPENCODE_SSE_RECONNECT_MAX", "100ms")

	d := newDriverForSSE(t, srv.URL)
	defer d.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		d.sseLoop(ctx)
		close(done)
	}()

	// Let it retry a few times.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if hits.Load() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if hits.Load() < 2 {
		t.Fatalf("hits = %d, want >=2 (reconnect should be active)", hits.Load())
	}

	// Signal close and expect sseLoop to exit within the backoff window.
	// Use Close() (which closes d.closed via closeOnce) instead
	// of a manual close(d.closed), to avoid the double-close
	// panic in closeOnce.Do.
	d.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sseLoop did not exit after Close()")
	}
}

// newDriverForSSE wires a minimal *driver suitable for unit-testing
// sseLoop without going through newDriver (which spawns a real
// subprocess). It mirrors testAgentFromServer but with everything
// sseLoop needs.
func newDriverForSSE(t *testing.T, baseURL string) *driver {
	t.Helper()
	proc := &serverProc{baseURL: baseURL}
	d := &driver{
		name:        "opencode",
		command:     "opencode",
		workspace:   "/tmp",
		branch:      "main",
		events:      make(chan agent.AgentEvent, eventBufferSize),
		closed:      make(chan struct{}),
		stopDeliver: make(chan struct{}),
		exitDone:    make(chan struct{}),
		server:      proc,
		client:      newClient(proc, "/tmp"),
		sessionID:   "ses_test",
	}
	d.trans = newTranslator(d.deliver, "opencode", "/tmp", "main", "ses_test", "")
	return d
}

// TestSSELoop_BackoffGrowsOnRapidDisconnect verifies that when the
// server keeps closing the connection within sseStableGrace, the
// reconnect loop GROWS its backoff rather than hammering at
// sseReconnectMin. We measure the gap between successive Subscribe
// attempts and assert it trends upward.
func TestSSELoop_BackoffGrowsOnRapidDisconnect(t *testing.T) {
	var hits atomic.Int32
	var hitTimes []time.Time
	var mu sync.Mutex

	mux := http.NewServeMux()
	mux.HandleFunc("/api/event", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hitTimes = append(hitTimes, time.Now())
		mu.Unlock()
		hits.Add(1)
		// Hijack and close immediately so each connection
		// lasts ~0 ms — well below sseStableGrace (2 s).
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Shrink the backoff so the test finishes in < 2 s.
	t.Setenv("NIGHTME_OPENCODE_SSE_RECONNECT_MIN", "20ms")
	t.Setenv("NIGHTME_OPENCODE_SSE_RECONNECT_MAX", "200ms")

	d := newDriverForSSE(t, srv.URL)
	defer d.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		d.sseLoop(ctx)
		close(done)
	}()

	// Let it accumulate a handful of attempts.
	time.Sleep(700 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sseLoop did not exit")
	}

	if hits.Load() < 4 {
		t.Fatalf("hits = %d, want >=4 (need several rapid attempts to measure backoff growth)", hits.Load())
	}

	mu.Lock()
	times := append([]time.Time(nil), hitTimes...)
	mu.Unlock()

	// Compute gaps and assert they grow (allowing jitter variance).
	gaps := make([]time.Duration, 0, len(times)-1)
	for i := 1; i < len(times); i++ {
		gaps = append(gaps, times[i].Sub(times[i-1]))
	}
	// First gap should be near the min (20 ms), later gaps should
	// be larger (backoff grows toward max=200 ms).
	if len(gaps) < 3 {
		t.Fatalf("only %d gaps, need >=3 to assert trend", len(gaps))
	}
	earlyAvg := (gaps[0] + gaps[1]) / 2
	lateAvg := (gaps[len(gaps)-2] + gaps[len(gaps)-1]) / 2
	if lateAvg <= earlyAvg {
		t.Errorf("backoff did not grow: early=%s, late=%s, gaps=%v",
			earlyAvg, lateAvg, gaps)
	}
}

// TestSSELoop_ResetsPendingTurnActiveOnReconnect verifies that a
// reconnect drops the busy guard. Scenario: a turn is in flight
// (pendingTurnActive=true); SSE drops mid-turn before session.idle
// arrives; on reconnect, the next SendBlocks must succeed (not
// hit ErrTurnBusy).
func TestSSELoop_ResetsPendingTurnActiveOnReconnect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/event", func(w http.ResponseWriter, r *http.Request) {
		// Send headers first so Subscribe() on the client
		// returns a body successfully; THEN hijack and close
		// so the client sees EOF and triggers reconnect. Without
		// this, Subscribe itself fails and we never reach the
		// "successful Subscribe" branch that resets
		// pendingTurnActive.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("NIGHTME_OPENCODE_SSE_RECONNECT_MIN", "20ms")

	d := newDriverForSSE(t, srv.URL)
	defer d.Close()

	// Pretend a turn is in flight.
	d.pendingMu.Lock()
	d.pendingTurnActive = true
	d.pendingMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.sseLoop(ctx)

	// Wait for at least one reconnect to happen.
	time.Sleep(150 * time.Millisecond)

	// pendingTurnActive should have been reset on the
	// successful Subscribe that came AFTER our stuck-true.
	d.pendingMu.Lock()
	got := d.pendingTurnActive
	d.pendingMu.Unlock()
	if got {
		t.Errorf("pendingTurnActive still true after reconnect; want false (user would be stranded on ErrTurnBusy)")
	}
}
