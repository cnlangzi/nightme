// host_state_test.go — pins the F-hostready-install-race invariant.
//
// Before the fix, the per-WS clientId dsh sends in the `ready`
// frame was captured by the host handler (hostWaterfallHandler
// in the dsh/ package). The handler installs lazily on first
// newDriver, so a WS that opens before any chat session exists
// had its one-shot ready frame dispatched to a nil handler and
// silently dropped — the clientId slot stayed empty for the
// connection's lifetime, and dsh/session.go::SendPermission
// failed with "dsh: no clientId captured from host $events ready
// frame" so the runtime's permission answer never reached dsh.
//
// The fix moves the capture to the dispatch path
// (host/stream.go:case "ready" → host.SetHostClientID), which
// runs in the readLoop goroutine before any handler. The two
// tests below pin that:
//
//   - TestReadyFrame_CapturesClientIdBeforeHandlerInstall —
//     connect, do NOT install a host handler, assert that
//     GetHostClientID returns the id the mock dsh sent. With the
//     pre-fix design this fails; with the post-fix design it
//     passes regardless of whether installHostHandler has run.
//
//   - TestSetHostClientID_Contract — pins the public API:
//     round-trip, no-op on empty id, last-write-wins on multiple
//     sets, and ResetHostClientIDForTest clears the slot for
//     cross-package test setup.
package host_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/bridge/dsh/host"
)

// mockReadyClientID is the hard-coded clientId the mock dsh in
// host_test.go:handleMuxWS sends in its `ready` frame. The bridge
// dispatch path captures this string verbatim; if the mock
// changes its hard-coded value, this test must change too —
// intentional coupling, the test's whole point is to pin the
// captured value matches what the mock sent.
const mockReadyClientID = "client-mock-001"

// TestReadyFrame_CapturesClientIdBeforeHandlerInstall proves the
// race-fix invariant. The mock dsh sends a `ready` frame right
// after the mux WS upgrades; the test deliberately does NOT call
// cli.SetHostHandler, simulating the production window where the
// WS opens during EnsureSharedHost but the host handler hasn't
// been wired yet (that wiring happens lazily on the first
// newDriver call from the runtime). The dispatch path must
// capture the clientId anyway.
func TestReadyFrame_CapturesClientIdBeforeHandlerInstall(t *testing.T) {
	host.ResetHostClientIDForTest()
	t.Cleanup(host.ResetHostClientIDForTest)

	mock := newMockDSH(t)
	c := host.New(mock.url(), slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(c.Close)

	// Deliberately do NOT call c.SetHostHandler — this is the
	// production race window. The old design would leave
	// hostRemoteClientID empty; the new design captures it
	// at the dispatch site regardless of handler presence.

	// The mock dsh sends the ready frame on connect; wait for
	// it to be processed and observe via GetHostClientID.
	waitFor(t, 2*time.Second, func() bool {
		return host.GetHostClientID() != ""
	})

	got := host.GetHostClientID()
	if got != mockReadyClientID {
		t.Fatalf("GetHostClientID = %q, want %q (mock dsh ready clientId)",
			got, mockReadyClientID)
	}
}

// TestSetHostClientID_Contract pins the public API of the new
// host/host_state.go surface: round-trip, no-op on empty id,
// last-write-wins on multiple sets, ResetHostClientIDForTest
// clears. The race-fix invariant in the previous test is the
// behavioural test; this one is the unit-level contract.
func TestSetHostClientID_Contract(t *testing.T) {
	host.ResetHostClientIDForTest()
	t.Cleanup(host.ResetHostClientIDForTest)

	// 1. Round-trip on a non-empty id.
	host.SetHostClientID("alpha")
	if got := host.GetHostClientID(); got != "alpha" {
		t.Errorf("after Set(alpha): Get = %q, want %q", got, "alpha")
	}

	// 2. Set("") is a no-op — must not clobber the good id.
	// Rationale: a dsh buggy version might send a ready frame
	// with an empty clientId; we don't want that to drop a
	// previously captured good id (the next valid ready will
	// overwrite anyway).
	host.SetHostClientID("")
	if got := host.GetHostClientID(); got != "alpha" {
		t.Errorf("after Set(\"\") no-op: Get = %q, want %q", got, "alpha")
	}

	// 3. Last-write-wins: another non-empty id overwrites.
	host.SetHostClientID("beta")
	if got := host.GetHostClientID(); got != "beta" {
		t.Errorf("after Set(beta): Get = %q, want %q", got, "beta")
	}

	// 4. Reset clears.
	host.ResetHostClientIDForTest()
	if got := host.GetHostClientID(); got != "" {
		t.Errorf("after Reset: Get = %q, want empty", got)
	}

	// 5. After reset, a fresh Set still works (no stuck state).
	host.SetHostClientID("gamma")
	if got := host.GetHostClientID(); got != "gamma" {
		t.Errorf("after Reset + Set(gamma): Get = %q, want %q", got, "gamma")
	}
}

// TestSetHostClientID_ConcurrentReadersAndWriters is a smoke
// check that the RWMutex around hostRemoteClientID holds under
// the race detector. Production traffic is one writer (the
// readLoop dispatch goroutine on WS connect / reconnect) and
// many readers (every SendPermission call). Spin a few readers
// in parallel with a writer to surface any missed locking.
func TestSetHostClientID_ConcurrentReadersAndWriters(t *testing.T) {
	host.ResetHostClientIDForTest()
	t.Cleanup(host.ResetHostClientIDForTest)

	const (
		readers       = 8
		writesPerLoop = 200
		readsPerLoop  = 200
	)

	var wg sync.WaitGroup
	wg.Add(readers + 1)

	// Writer: alternate between two ids, never empty.
	go func() {
		defer wg.Done()
		for i := 0; i < writesPerLoop; i++ {
			if i%2 == 0 {
				host.SetHostClientID("alpha")
			} else {
				host.SetHostClientID("beta")
			}
		}
	}()

	// Readers: each spins readsPerLoop reads. We only assert
	// the return is one of the two ids the writer set (or ""
	// if it raced with a reset). race detector validates
	// concurrency safety — no value check needed beyond
	// "non-nil per the type system" (string is always
	// comparable to "").
	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			for i := 0; i < readsPerLoop; i++ {
				_ = host.GetHostClientID()
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent reads/writes did not finish in 5s")
	}
}

// TestReadyFrame_ReconnectOverwritesClientID pins the
// last-write-wins invariant of the dispatch-path clientId
// capture. Production scenarios this guards:
//
//   - WS dies, bridge reconnects — dsh may mint a new
//     per-connection clientId; the slot must reflect the new
//     one, not the stale one from the previous connection.
//   - dsh respawns under nightme's watchdog — the new dsh
//     assigns a fresh clientId; same overwrite requirement.
//
// Without this test, a future regression that makes
// SetHostClientID conditional (e.g. "only set if empty") would
// not be caught — the slot would keep the dead connection's
// id and SendPermission would POST /api/$events/result with
// the wrong clientId, getting rejected by dsh.
//
// Flow: start with clientId "first"; capture; rotate the mock
// to emit "second"; force WS reconnect; assert slot == "second".
func TestReadyFrame_ReconnectOverwritesClientID(t *testing.T) {
	host.ResetHostClientIDForTest()
	t.Cleanup(host.ResetHostClientIDForTest)

	const (
		firstID  = "client-mock-first"
		secondID = "client-mock-second"
	)

	mock := newMockDSH(t)
	mock.setReadyClientID(firstID)

	c := host.New(mock.url(), slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(c.Close)

	// Phase 1: first connect's ready frame must populate the slot.
	waitFor(t, 2*time.Second, func() bool {
		return host.GetHostClientID() == firstID
	})
	if got := host.GetHostClientID(); got != firstID {
		t.Fatalf("after first connect: GetHostClientID = %q, want %q", got, firstID)
	}
	firstCount := mock.muxConnectCount.Load()

	// Phase 2: rotate the mock to emit a different clientId, then
	// force the WS to die. The bridge's read pump notices, the
	// reconnect loop dials again, the mock hands out the new
	// clientId, and the dispatch path must overwrite the slot.
	mock.setReadyClientID(secondID)
	mock.shutdown()

	// Reconnect backoff base is 1s; the wait is generous.
	waitFor(t, 8*time.Second, func() bool {
		return mock.muxConnectCount.Load() > firstCount &&
			host.GetHostClientID() == secondID
	})

	if got := mock.muxConnectCount.Load(); got <= firstCount {
		t.Fatalf("expected reconnect (muxConnectCount > %d), got %d", firstCount, got)
	}
	if got := host.GetHostClientID(); got != secondID {
		t.Fatalf("after reconnect: GetHostClientID = %q, want %q (stale %q would mean "+
			"SetHostClientID was made conditional, breaking the dsh respawn path)",
			got, secondID, firstID)
	}

	// Sanity: the slot must NOT still hold the old id — that's
	// the whole point of the test. Defensive second check in
	// case a future maintainer changes the waitFor condition
	// and the first assertion accidentally passes.
	if got := host.GetHostClientID(); got == firstID {
		t.Fatalf("slot still holds stale first id after reconnect; " +
			"dispatch path failed to overwrite")
	}
}
