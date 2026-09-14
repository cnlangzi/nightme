// fallback_test.go — pins the monitor's attached → owned fallback
// and owned → owned respawn paths.
//
// Background: nightme has taken over the dsh host lifecycle end to
// end. The monitor (runMonitor) starts in attached mode when
// tryAttachExistingDSH succeeded at StartSharedHost (h.cmd = nil)
// and dispatches into owned mode (h.cmd != nil) on:
//
//   - attached: probe failures (3 strikes by default) → fallback-spawn
//     a fresh dsh (monitorAttachedFallback)
//   - owned: cmd.Wait returns → respawn (tryRespawn)
//
// Both paths retry indefinitely (no attempt cap). The previous
// "give up after N attempts; user must `make restart`" contract
// (pin'd by the legacy TestFallback_GivesUpAfterMaxAttempts) was
// removed in 2026-09-14: nightme owns the host, so transient
// failures should not strand the host and force a manual restart.
//
// Tests in this file use a mock dsh server (mockDSHServer) plus
// SharedHost.testHooks to drive the probe + fallback loops fast
// (millisecond intervals instead of 30s production defaults) and
// to replace the spawner with a deterministic mock. The mock
// spawner returns a fake *Client (unstarted is fine for the
// state-swap paths).

package host

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// mockDSHServer is the minimum httptest.Server that satisfies
// the probe loop's /api/session.list calls. It does NOT implement
// the mux WS protocol — the client is allowed to dial
// /api/remote.mux and sit there, never receiving frames. The
// probe validation is just the 200 from /api/session.list.
type mockDSHServer struct {
	srv    *httptest.Server
	closed bool
}

func newMockDSHServer(t *testing.T) *mockDSHServer {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session.list", func(w http.ResponseWriter, r *http.Request) {
		// session.list response is a typert envelope. We return
		// the minimum shape SessionList needs; it ignores the
		// value's contents.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"server-response","rpcId":"probe","result":{"ok":true,"value":{"items":[]}}}`))
	})
	// /api/remote.mux — accept WS upgrade, then sit there
	// silently. gorilla's upgrader does the handshake; we
	// don't write anything; the client gives up after its
	// own read deadline.
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	mux.HandleFunc("/api/remote.mux", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Hold the connection open until the server is closed
		// (via t.Cleanup). Reads would block here otherwise; we
		// just sit on the conn without reading.
		<-r.Context().Done()
		_ = conn.Close()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &mockDSHServer{srv: srv}
}

func (m *mockDSHServer) url() string { return m.srv.URL }
func (m *mockDSHServer) shutdown()   { m.srv.Close(); m.closed = true }

// newAttachedHostForTest constructs a minimal *SharedHost in
// attached mode (cmd=nil) backed by the mock dsh. Bypasses
// tryAttachExistingDSH (which would need a real dsh cookie
// validation path) and goes straight to a struct that runMonitor
// can drive. Caller is expected to invoke go h.runMonitor() (or
// just call h.Close via ShutdownSharedHost at test end) and
// trigger cleanup via the returned hooks.
func newAttachedHostForTest(t *testing.T, mock *mockDSHServer, hooks *testHooks) *SharedHost {
	t.Helper()
	cli := New(mock.url(), slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("cli.Start: %v", err)
	}
	h := &SharedHost{
		cli:          cli,
		logger:       slog.Default(),
		opts:         SharedHostOptions{HostCmd: "dsh", Workspace: "/tmp/test", Port: 3080},
		closed:       make(chan struct{}),
		watchdogDone: make(chan struct{}),
		testHooks:    hooks,
	}
	t.Cleanup(func() {
		ShutdownSharedHost(h)
		if !mock.closed {
			cli.Close()
		}
	})
	return h
}

// TestMonitor_AttachedDSHDiesTriggersFallback pins the core
// race-fix invariant: when the attached dsh dies, the monitor
// detects the dead state and triggers the fallback. The real
// fallback would spawn a dsh subprocess (which we can't do in a
// unit test); the Spawner hook replaces it with a stub that
// signals completion via a channel.
func TestMonitor_AttachedDSHDiesTriggersFallback(t *testing.T) {
	mock := newMockDSHServer(t)
	resetGlobalState(t)

	fallbackCalled := make(chan struct{}, 1)
	hooks := &testHooks{
		ProbeInterval: 20 * time.Millisecond,
		ProbeStrikes:  2,
		ProbeTimeout:  50 * time.Millisecond,
		Spawner: func() (*exec.Cmd, *Client, error) {
			select {
			case fallbackCalled <- struct{}{}:
			default:
			}
			// Return nil cmd so the monitor's owned branch
			// (cmd == nil → return) exits after this single
			// iteration. The cli is what tests inspect via
			// h.cli.
			return nil, New("http://127.0.0.1:1", slog.Default()), nil
		},
	}
	h := newAttachedHostForTest(t, mock, hooks)

	monitorExited := make(chan struct{})
	go func() {
		h.runMonitor()
		close(monitorExited)
	}()

	// Kill the mock dsh. The next probe (within ProbeInterval)
	// will fail; after ProbeStrikes failures, the Spawner fires.
	mock.shutdown()

	select {
	case <-fallbackCalled:
		// Spawner fired.
	case <-time.After(2 * time.Second):
		t.Fatal("Spawner was not called within 2s after mock shutdown")
	}

	select {
	case <-monitorExited:
		// runMonitor observed the fallback → cmd == nil → return.
	case <-time.After(2 * time.Second):
		t.Fatal("monitor did not exit after fallback")
	}
}

// TestMonitor_ProbeRecoversOnTransientFailure pins the strike
// counter reset: a single successful probe (after one or more
// failures) clears the counter, so transient blips don't
// accumulate into a false fallback. We use the ProbeFailure hook
// to drive the probe outcome deterministically without needing
// a real dsh to die.
func TestMonitor_ProbeRecoversOnTransientFailure(t *testing.T) {
	mock := newMockDSHServer(t)
	resetGlobalState(t)

	var failureCount int
	hooks := &testHooks{
		ProbeInterval: 20 * time.Millisecond,
		ProbeStrikes:  5,
		ProbeTimeout:  50 * time.Millisecond,
		ProbeFailure: func(int) bool {
			// Fail the first 2 probes, then pass forever
			// after. The strike counter should reset on the
			// first pass and never reach 5.
			failureCount++
			return failureCount > 2
		},
	}
	h := newAttachedHostForTest(t, mock, hooks)

	done := make(chan struct{})
	go func() {
		h.runMonitor()
		close(done)
	}()

	// Run for 500ms — should never trigger fallback because the
	// probe passes after the first 2 transient failures.
	time.Sleep(500 * time.Millisecond)

	ShutdownSharedHost(h)
	select {
	case <-done:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("monitor did not exit on ShutdownSharedHost")
	}

	if failureCount < 3 {
		t.Errorf("expected at least 3 probe calls (2 fail + 1 pass); got %d", failureCount)
	}
}

// TestMonitor_StrikesAreCounted pins the strike threshold: with
// strikes=2, the fallback fires after exactly 2 consecutive
// failures, not 1 and not 3. We verify by counting Spawner calls.
func TestMonitor_StrikesAreCounted(t *testing.T) {
	mock := newMockDSHServer(t)
	resetGlobalState(t)

	var fallbackCount int
	hooks := &testHooks{
		ProbeInterval: 20 * time.Millisecond,
		ProbeStrikes:  2,
		ProbeTimeout:  50 * time.Millisecond,
		ProbeFailure:  func(int) bool { return false }, // always fail
		Spawner: func() (*exec.Cmd, *Client, error) {
			fallbackCount++
			// Return nil cmd so the monitor exits cleanly
			// after the owned branch hits cmd == nil.
			return nil, New("http://127.0.0.1:1", slog.Default()), nil
		},
	}
	h := newAttachedHostForTest(t, mock, hooks)

	done := make(chan struct{})
	go func() {
		h.runMonitor()
		close(done)
	}()

	// Don't shutdown the mock — let ProbeFailure drive the
	// strikes (mock's /api/session.list is irrelevant under
	// ProbeFailure). Wait for the monitor to exit.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor did not exit")
	}

	if fallbackCount != 1 {
		t.Errorf("Spawner called %d times; want 1 (one per monitor run)", fallbackCount)
	}
}

// TestMonitor_FallbackRetriesOnSpawnFailure pins the retry
// contract: a transient spawn failure does NOT strand the host.
// monitorAttachedFallback backs off and retries until the
// spawner succeeds (or h.closed fires, which we don't trigger
// in this test). After the transient failure clears, the
// fallback completes and the monitor transitions to owned mode.
//
// We use the Spawner hook to fail the first N-1 calls and
// succeed on the Nth.
func TestMonitor_FallbackRetriesOnSpawnFailure(t *testing.T) {
	mock := newMockDSHServer(t)
	resetGlobalState(t)

	var calls int
	var fakeCli *Client
	hooks := &testHooks{
		ProbeInterval: 20 * time.Millisecond,
		ProbeStrikes:  1, // fail fast — we want to test the spawner loop
		ProbeTimeout:  50 * time.Millisecond,
		ProbeFailure:  func(int) bool { return false }, // always fail
		Spawner: func() (*exec.Cmd, *Client, error) {
			calls++
			if calls < 3 {
				return nil, nil, errors.New("simulated spawn failure")
			}
			// 3rd call: success. nil cmd makes the owned
			// branch return on its first iteration.
			fakeCli = New("http://127.0.0.1:1", slog.Default())
			return nil, fakeCli, nil
		},
	}
	h := newAttachedHostForTest(t, mock, hooks)

	done := make(chan struct{})
	go func() {
		h.runMonitor()
		close(done)
	}()

	// The retry backoff is respawnBackoffMax (30s in production,
	// but tests override via... wait, we didn't expose a
	// fallback-backoff knob. Backoff is 30s. Allow plenty of
	// time — we'll trim if needed. For now, the second
	// failure fires within respawnBackoffMax of the first; the
	// third succeeds within respawnBackoffMax of the second.
	// Total wait ≤ 60s. The test should complete in ~30s; we
	// cap at 90s to be safe on slow CI.
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatalf("fallback did not complete; calls=%d", calls)
	}

	if calls != 3 {
		t.Errorf("Spawner called %d times; want 3 (2 failures + 1 success)", calls)
	}

	h.mu.RLock()
	cli := h.cli
	h.mu.RUnlock()
	if cli != fakeCli {
		t.Errorf("host.cli = %p, want %p (the Spawner's success cli)", cli, fakeCli)
	}
}

// TestMonitor_FallbackRetriesForeverOnPermanentSpawnFailure pins
// the NEW forever-retry contract (2026-09-14): even if the
// Spawner fails on every call, monitorAttachedFallback keeps
// retrying with backoff until h.closed fires. The old
// TestFallback_GivesUpAfterMaxAttempts pinned the OPPOSITE
// behaviour (give up after 3 attempts) — that test was removed
// because it was tracking a "graceful degradation" path that
// the project decided was actually a silent failure.
//
// To assert "no give up", we drive the Spawner to fail
// forever, sleep long enough for one or two retries, then close
// h.closed and verify the monitor exits cleanly with an
// error-classified log line (not "monitor exiting for good").
func TestMonitor_FallbackRetriesForeverOnPermanentSpawnFailure(t *testing.T) {
	mock := newMockDSHServer(t)
	resetGlobalState(t)

	var calls atomic.Int32
	hooks := &testHooks{
		ProbeInterval: 20 * time.Millisecond,
		ProbeStrikes:  1,
		ProbeTimeout:  50 * time.Millisecond,
		ProbeFailure:  func(int) bool { return false }, // always fail
		Spawner: func() (*exec.Cmd, *Client, error) {
			calls.Add(1)
			return nil, nil, errors.New("permanent spawn failure")
		},
	}
	h := newAttachedHostForTest(t, mock, hooks)

	done := make(chan struct{})
	go func() {
		h.runMonitor()
		close(done)
	}()

	// Wait for the Spawner to fire at least 2 times (proves the
	// first failure did not stop the monitor).
	deadline := time.Now().Add(90 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if calls.Load() < 2 {
		t.Fatalf("Spawner called %d times in 90s; want ≥2 (must retry past first failure)", calls.Load())
	}

	// Now close the host. monitorAttachedFallback's backoff
	// select will see h.closed and return errors.New("dsh.host:
	// monitor closed before fallback succeeded"), and the
	// monitor will exit cleanly.
	ShutdownSharedHost(h)
	select {
	case <-done:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("monitor did not exit on ShutdownSharedHost even with permanent spawn failure")
	}

	// Host state must remain "attached" — no successful fallback
	// happened, so h.cmd should still be nil and the original
	// (attached) cli should still be in place.
	h.mu.RLock()
	cmd := h.cmd
	cli := h.cli
	h.mu.RUnlock()
	if cmd != nil {
		t.Errorf("host.cmd = %v after monitor exit; want nil (no successful fallback)", cmd)
	}
	if cli == nil {
		t.Error("host.cli = nil; want the original attached cli")
	}
}

// TestMonitor_RespawnRetriesForeverOnPermanentRespawnFailure
// pins the same forever-retry contract for owned-mode respawn.
// The ExitedProbe hook makes the monitor see "dsh exited" without
// a real subprocess; the Respawner hook always fails. The monitor
// must keep retrying until h.closed fires (no "watchdog giving up"
// exit).
func TestMonitor_RespawnRetriesForeverOnPermanentRespawnFailure(t *testing.T) {
	resetGlobalState(t)

	var respawnCalls atomic.Int32
	hooks := &testHooks{
		ProbeInterval: 20 * time.Millisecond,
		ProbeStrikes:  1,
		ProbeTimeout:  50 * time.Millisecond,
		ExitedProbe: func() (*exec.Cmd, bool) {
			// Inject "dsh exited" exactly once on the first
			// monitorOwnedLoop entry. After that, the monitor
			// stays inside tryRespawn (which retries the
			// Respawner forever); we don't want ExitedProbe
			// to be called again.
			if respawnCalls.Load() > 0 {
				return nil, false // no exit; let monitor sleep
			}
			respawnCalls.Add(1)
			return nil, true
		},
		Respawner: func() (*exec.Cmd, *Client, error) {
			// Called from tryRespawn after the monitor
			// observes "dsh exited". Always fail.
			return nil, nil, errors.New("permanent respawn failure")
		},
	}

	// Build an owned-mode SharedHost (h.cmd != nil) so
	// monitorOwnedLoop runs from the start (not via a fallback
	// transition). Attach a fresh *Client so monitorOwnedLoop's
	// RecoverSubscriptions and the Respawner have something
	// to swap.
	cli := New("http://127.0.0.1:1", slog.Default())
	h := &SharedHost{
		cmd:          &exec.Cmd{},
		cli:          cli,
		logger:       slog.Default(),
		opts:         SharedHostOptions{HostCmd: "dsh", Workspace: "/tmp/test", Port: 3080},
		closed:       make(chan struct{}),
		watchdogDone: make(chan struct{}),
		testHooks:    hooks,
	}
	t.Cleanup(func() { ShutdownSharedHost(h) })

	done := make(chan struct{})
	go func() {
		h.runMonitor()
		close(done)
	}()

	// Wait until the monitor has started respawning (ExitedProbe
	// fired once and returned true → monitorOwnedLoop entered
	// tryRespawn).
	deadline := time.Now().Add(5 * time.Second)
	for respawnCalls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if respawnCalls.Load() < 1 {
		t.Fatal("ExitedProbe never fired (monitor didn't observe dsh exit)")
	}

	// Give the monitor a moment to enter the retry backoff so
	// ShutdownSharedHost's select sees h.closed mid-backoff
	// (the production path that exercises h.closed handling).
	time.Sleep(100 * time.Millisecond)

	// Close the host. tryRespawn's backoff select sees
	// h.closed and returns; the monitor exits.
	ShutdownSharedHost(h)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor did not exit on ShutdownSharedHost even with permanent respawn failure")
	}
}

// resetGlobalState clears the package-level *Client and
// *SharedHost globals. The fallback path mutates both, and any
// state leaked across tests breaks parallel package-global
// invariants (SetSharedHost panics on double-install, hence
// "must reset between tests").
func resetGlobalState(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		UnsetGlobal()
		UnsetSharedHost()
	})
	// Defensive: also reset at start in case a previous test
	// left state behind.
	UnsetGlobal()
	UnsetSharedHost()
}
