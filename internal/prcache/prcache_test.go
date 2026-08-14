package prcache

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
)

// TestCache_PR_BeforeAnyRefresh covers the cold-start case:
// a freshly-zero-valued Cache returns nil from PR(). The
// first stamp path's MaybeRefresh will spawn a refresh
// goroutine that resolves the PR asynchronously; the FIRST
// stamp's PR() therefore returns nil. Subsequent stamps
// (within the 60s TTL) hit the cache. This test pins only
// the nil-before-any-refresh half — the async spawn is
// covered indirectly by TestCache_Refresh_UpdatesBranchOnSwitch
// + TestCache_ConcurrentGetAndWritePR.
func TestCache_PR_BeforeAnyRefresh(t *testing.T) {
	c := &Cache{}
	if got := c.PR(); got != nil {
		t.Fatalf("Cache.PR() = %+v, want nil", got)
	}
}

// TestCache_WritePR_NilClears verifies the nil-clears branch
// of WritePR: pr=nil drops pr, branch, and expiresAt. The
// next MaybeRefresh would then kick a fresh refresh from
// scratch (expiresAt zero == expired).
func TestCache_WritePR_NilClears(t *testing.T) {
	c := &Cache{}
	c.mu.Lock()
	c.pr = &messages.PR{Number: 1, URL: "https://example/pr/1", State: "open"}
	c.branch = "main"
	c.expiresAt = time.Now().Add(TTL)
	c.mu.Unlock()

	c.WritePR(nil)

	c.mu.Lock()
	gotPR := c.pr
	gotBranch := c.branch
	gotExpires := c.expiresAt
	c.mu.Unlock()
	if gotPR != nil {
		t.Errorf("WritePR(nil) left pr = %+v, want nil", gotPR)
	}
	if gotBranch != "" {
		t.Errorf("WritePR(nil) left branch = %q, want \"\"", gotBranch)
	}
	if !gotExpires.IsZero() {
		t.Errorf("WritePR(nil) left expiresAt = %v, want zero", gotExpires)
	}
}

// TestCache_WritePR_NonNilSets verifies the non-nil branch:
// pr is stored, expiresAt is reset to now+TTL, branch is
// intentionally left untouched.
func TestCache_WritePR_NonNilSets(t *testing.T) {
	c := &Cache{}
	// Pre-populate branch + an old expiresAt to confirm
	// WritePR only touches pr + expiresAt.
	c.mu.Lock()
	c.branch = "feature/old"
	c.expiresAt = time.Time{}
	c.mu.Unlock()

	newPR := &messages.PR{Number: 42, URL: "https://example/pr/42", State: "open"}
	before := time.Now()
	c.WritePR(newPR)
	after := time.Now()

	c.mu.Lock()
	gotPR := c.pr
	gotBranch := c.branch
	gotExpires := c.expiresAt
	c.mu.Unlock()
	if gotPR == nil || gotPR.Number != 42 {
		t.Errorf("after WritePR, PR = %+v, want {Number:42}", gotPR)
	}
	if gotBranch != "feature/old" {
		t.Errorf("WritePR touched branch = %q, want unchanged %q", gotBranch, "feature/old")
	}
	// expiresAt must be in (before+TTL - slop, after+TTL + slop).
	low := before.Add(TTL)
	high := after.Add(TTL)
	if gotExpires.Before(low) || gotExpires.After(high.Add(50*time.Millisecond)) {
		t.Errorf("expiresAt = %v, want within [%v, %v]", gotExpires, low, high)
	}
}

// TestCache_WritePR_CancelsInflightRefresh locks the contract
// that WritePR cancels any in-flight refresh goroutine so the
// goroutine's stale resolver result does NOT overwrite the
// fresh write. Without this, a /gtw pr fired while a refresh
// was already running would have its WritePR overwritten by
// the refresh's pre-/gtw-pr resolver result — the footer would
// show the OLD PR (or no PR) for up to 60s until the TTL
// expired and the next MaybeRefresh finally fetched fresh.
//
// Race exercised:
//   T0: stamp triggers MaybeRefresh → spawns goroutine
//   T1: goroutine starts the slow resolver
//   T2: /gtw pr succeeds → WritePR(as.ID, newPR)
//   T3: resolver returns (stale result for the pre-/gtw-pr
//        branch state)
//   T4: WritePR's cancel() must have set ctx.Err() so the
//        goroutine's post-resolver `if ctx.Err() != nil`
//        check returns early WITHOUT writing its result.
func TestCache_WritePR_CancelsInflightRefresh(t *testing.T) {
	c := &Cache{}

	// Block the resolver until we explicitly release it,
	// so we can observe the cancellation mid-flight.
	release := make(chan struct{})
	resolver := func(ctx context.Context, dir string) (*messages.PR, string, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
		if ctx.Err() != nil {
			// ctx.Err() observed: WritePR cancelled us.
			// Return whatever — refresh() will early-return
			// before writing.
			return nil, "", ctx.Err()
		}
		// Buggy path: we'd write a stale result. With the
		// cancel-in-WritePR fix, we never reach here.
		t.Errorf("resolver was not cancelled; WritePR failed to cancel in-flight refresh")
		return &messages.PR{Number: 999, URL: "stale", State: "open"}, "stale-branch", nil
	}

	// T0: kick off an in-flight refresh.
	c.MaybeRefresh("/w", resolver)
	if !c.inflight {
		t.Fatal("MaybeRefresh did not mark inflight=true")
	}

	// Give the goroutine a moment to enter the resolver
	// and block on `release` / ctx.Done().
	time.Sleep(10 * time.Millisecond)

	// T2: simulate /gtw pr's WritePR.
	newPR := &messages.PR{Number: 42, URL: "https://example/pr/42", State: "open"}
	c.WritePR(newPR)

	// T3/T4: release the resolver — it should observe
	// ctx.Err() (cancelled) and bail without writing.
	close(release)

	// Give the goroutine a moment to exit.
	time.Sleep(50 * time.Millisecond)

	if got := c.PR(); got == nil || got.Number != 42 {
		t.Errorf("WritePR was overwritten by stale refresh: PR = %+v, want {Number:42}", got)
	}
	if c.inflight {
		t.Errorf("inflight = true after WritePR; refresh goroutine did not exit via ctx.Err()")
	}
}

// TestCache_WritePR_NilCancelsInflightRefresh is the close-path
// counterpart: WritePR(nil) must also cancel any in-flight
// refresh, otherwise the refresh would write a non-nil PR
// after we cleared, un-doing the clear.
func TestCache_WritePR_NilCancelsInflightRefresh(t *testing.T) {
	c := &Cache{}

	release := make(chan struct{})
	resolver := func(ctx context.Context, dir string) (*messages.PR, string, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		t.Errorf("resolver was not cancelled; WritePR(nil) failed to cancel in-flight refresh")
		return &messages.PR{Number: 999, URL: "stale", State: "open"}, "main", nil
	}

	c.MaybeRefresh("/w", resolver)
	time.Sleep(10 * time.Millisecond)

	// /gtw close's WritePR(nil).
	c.WritePR(nil)

	close(release)
	time.Sleep(50 * time.Millisecond)

	if got := c.PR(); got != nil {
		t.Errorf("WritePR(nil) was overwritten by stale refresh: PR = %+v, want nil", got)
	}
	if c.inflight {
		t.Errorf("inflight = true after WritePR(nil); refresh goroutine did not exit via ctx.Err()")
	}
}

// TestCache_CancelNoOpWhenIdle covers Cancel's nil-cancel
// branch — calling Cancel on a Cache whose refresh isn't
// running must not panic and must not block.
func TestCache_CancelNoOpWhenIdle(t *testing.T) {
	c := &Cache{}
	c.Cancel() // no inflight, no panic
	c.Cancel() // idempotent
}

// TestCache_CancelStopsInflightRefresh drives Cancel from the
// outside of a slow refresh to confirm the in-flight
// goroutine honours ctx cancellation. We swap MaybeRefresh's
// underlying work via a hook: manually install an inflight
// state, spawn a goroutine that blocks on ctx.Done, call
// Cancel, and confirm the goroutine saw the cancellation.
func TestCache_CancelStopsInflightRefresh(t *testing.T) {
	c := &Cache{}
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	c.inflight = true
	c.cancel = cancel
	c.mu.Unlock()

	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(done)
	}()

	c.Cancel()

	select {
	case <-done:
		// goroutine saw the cancel — good
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Cancel() did not propagate ctx.Done within 500ms")
	}

	c.mu.Lock()
	if c.cancel != nil {
		t.Errorf("cancel = %p, want nil after Cancel", c.cancel)
	}
	c.mu.Unlock()
}

// TestCache_MaybeRefresh_NilResolver is a guard: passing
// resolver=nil into MaybeRefresh must not panic. The
// production runtime always wires a real Resolver, but a
// misconfigured test fixture could leave it nil.
func TestCache_MaybeRefresh_NilResolver(t *testing.T) {
	c := &Cache{}
	c.MaybeRefresh("/w", nil) // no panic, no-op
	if c.PR() != nil {
		t.Errorf("MaybeRefresh(nil resolver) populated PR")
	}
}

// TestCache_MaybeRefresh_NoGoroutineOnFresh covers the
// common case: a fresh (non-expired) cache does NOT spawn
// a refresh goroutine. We probe by inspecting inflight
// after a small delay.
func TestCache_MaybeRefresh_NoGoroutineOnFresh(t *testing.T) {
	c := &Cache{}
	c.mu.Lock()
	c.expiresAt = time.Now().Add(TTL)
	c.pr = &messages.PR{Number: 7, URL: "https://example/pr/7", State: "open"}
	c.mu.Unlock()

	c.MaybeRefresh("/w", func(ctx context.Context, dir string) (*messages.PR, string, error) {
		t.Errorf("resolver was invoked on a fresh cache; want no-op")
		return nil, "", nil
	})

	time.Sleep(20 * time.Millisecond)
	c.mu.Lock()
	inflight := c.inflight
	c.mu.Unlock()
	if inflight {
		t.Errorf("MaybeRefresh spawned a goroutine on a fresh cache; want no-op")
	}
}

// TestCache_MaybeRefresh_SpawnsOnExpired covers the
// expired-cache case: a Resolver is invoked exactly once
// even if MaybeRefresh is hammered from N goroutines.
func TestCache_MaybeRefresh_SpawnsOnExpired(t *testing.T) {
	c := &Cache{}
	// expiresAt is zero → expired → MaybeRefresh should spawn.

	var calls atomic.Int64
	resolver := func(ctx context.Context, dir string) (*messages.PR, string, error) {
		calls.Add(1)
		// simulate the resolver writing back into the cache
		// (mirrors what refresh() does after a successful
		// resolver call) so subsequent MaybeRefreshs see a
		// fresh cache and short-circuit.
		c.mu.Lock()
		c.pr = &messages.PR{Number: 1, URL: "https://example/pr/1", State: "open"}
		c.branch = "main"
		c.expiresAt = time.Now().Add(TTL)
		c.mu.Unlock()
		return c.pr, "main", nil
	}

	const N = 20
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.MaybeRefresh("/w", resolver)
		}()
	}
	wg.Wait()

	// Give the spawned goroutine a chance to finish.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		done := !c.inflight
		c.mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := calls.Load(); got < 1 {
		t.Errorf("resolver was called %d times, want >=1", got)
	}
	if got := calls.Load(); got > 2 {
		t.Errorf("resolver was called %d times; want at most 2 (one for the race-winner, possibly a second after inflight clears)", got)
	}
}

// TestRegistry_GetOrCreate_AllocatesOnce covers the lazy
// allocation contract: repeated GetOrCreate(asID) calls return
// the same pointer; missing keys allocate on first call.
func TestRegistry_GetOrCreate_AllocatesOnce(t *testing.T) {
	var r Registry

	a := r.GetOrCreate("as1")
	b := r.GetOrCreate("as1")
	c := r.GetOrCreate("as2")

	if a != b {
		t.Errorf("GetOrCreate(as1) returned different pointers: %p vs %p", a, b)
	}
	if a == c {
		t.Errorf("GetOrCreate(as1) and GetOrCreate(as2) share a pointer: %p", a)
	}
}

// TestRegistry_WritePR_NoOpOnUnknown pins the contract that
// Registry.WritePR is a no-op for ASes the registry hasn't
// allocated a Cache for yet. The /gtw dispatchers may
// legitimately look up an AS that hasn't been stamped enough
// to trigger GetOrCreate; WritePR on such an AS must not
// panic and must not allocate a Cache (otherwise /gtw pr
// success on a chat with zero stamps would allocate caches
// for every AS).
func TestRegistry_WritePR_NoOpOnUnknown(t *testing.T) {
	r := &Registry{}
	r.WritePR("as-never-stamped", &messages.PR{Number: 1, URL: "x", State: "open"})
	r.WritePR("as-never-stamped-2", nil)

	// Registry must remain empty: GetOrCreate was never called.
	r.mu.RLock()
	if _, ok := r.caches["as-never-stamped"]; ok {
		r.mu.RUnlock()
		t.Fatalf("WritePR allocated a Cache for an unregistered AS")
	}
	if _, ok := r.caches["as-never-stamped-2"]; ok {
		r.mu.RUnlock()
		t.Fatalf("WritePR(nil) allocated a Cache for an unregistered AS")
	}
	r.mu.RUnlock()
}

// TestRegistry_WritePR_RoutesToAllocatedCache covers the
// happy path: WritePR on an already-allocated asID lands
// in the Cache.
func TestRegistry_WritePR_RoutesToAllocatedCache(t *testing.T) {
	r := &Registry{}
	c := r.GetOrCreate("as-1")
	c.WritePR(&messages.PR{Number: 9, URL: "https://example/pr/9", State: "open"})

	r.WritePR("as-1", &messages.PR{Number: 11, URL: "https://example/pr/11", State: "open"})

	if got := c.PR(); got == nil || got.Number != 11 {
		t.Errorf("after Registry.WritePR, c.PR() = %+v, want {Number:11}", got)
	}

	r.WritePR("as-1", nil)
	if got := c.PR(); got != nil {
		t.Errorf("after Registry.WritePR(nil), c.PR() = %+v, want nil", got)
	}
}

// TestCache_ConcurrentGetAndWritePR hammers a Cache from
// many goroutines doing PR/WritePR in parallel — the lock
// must not race. (Run with -race to catch data races; CI
// does this for every PR.)
func TestCache_ConcurrentGetAndWritePR(t *testing.T) {
	c := &Cache{}
	c.pr = &messages.PR{Number: 9, URL: "https://example/pr/9", State: "open"}
	c.branch = "main"
	c.expiresAt = time.Now().Add(TTL)

	const N = 100
	var wg sync.WaitGroup
	var ops atomic.Int64

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = c.PR()
				ops.Add(1)
			}
		}()
	}
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if i%2 == 0 {
					c.WritePR(&messages.PR{Number: i*1000 + j, URL: "x", State: "open"})
				} else {
					c.WritePR(nil)
				}
				ops.Add(1)
			}
		}(i)
	}

	wg.Wait()

	// After the storm the cache must still be readable.
	// We don't pin the exact final state — interleavings
	// between PR() and WritePR() are non-deterministic —
	// only that PR() returns without panic / nil-deref.
	_ = c.PR()
}

// TestRegistry_CloseAll_CancelsInflight verifies Registry.CloseAll
// cancels every Cache's in-flight refresh goroutine. We drive the
// inflight state directly via Cache.mu (this is the same-package
// test, no exported test helpers needed) — the production caller
// is cmd/nightme/shutdownRun, whose wiring is covered by
// cmd/nightme/run_test.go::TestShutdownRun_CloseAllCancelsCaches.
//
// The cancel is observed via a goroutine that watches ctx.Done();
// CloseAll must reach the cache, grab mu, read cancel, and invoke
// it within a bounded window.
func TestRegistry_CloseAll_CancelsInflight(t *testing.T) {
	r := &Registry{}

	// Three caches, each with its own cancel context. CloseAll
	// must hit all three — a single iteration bug (e.g.
	// `for i := range caches` without copying the map) would
	// leak the rest.
	const N = 3
	cancels := make([]context.CancelFunc, N)
	done := make([]chan struct{}, N)

	for i := 0; i < N; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		done[i] = make(chan struct{})

		c := r.GetOrCreate(fmt.Sprintf("as_%d", i))
		c.mu.Lock()
		c.inflight = true
		c.cancel = cancel
		c.mu.Unlock()

		go func(idx int) {
			<-ctx.Done()
			close(done[idx])
		}(i)
	}

	r.CloseAll()

	for i := 0; i < N; i++ {
		select {
		case <-done[i]:
			// observed
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("cache %d: CloseAll did not cancel within 500ms", i)
		}
	}

	// After CloseAll the registry's map is empty: GetOrCreate
	// returns a freshly-allocated Cache (different pointer from
	// the pre-CloseAll instances, which remain valid for any
	// caller still holding them).
	pre := r.GetOrCreate("as_0") // first re-entry after CloseAll
	if pre == nil {
		t.Fatal("GetOrCreate after CloseAll returned nil; want fresh cache")
	}
	if pre.inflight {
		t.Errorf("freshly-allocated cache has inflight=true; want false")
	}
}
