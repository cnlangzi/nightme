// Package prcache — per-AgentSession cache for the GitHub /
// GitLab PR (or MR) associated with the current head branch,
// refreshed asynchronously on a 60s TTL.
//
// Scope: ONE Cache per AgentSession, owned externally (the
// runtime in cmd/nightme keeps a Registry keyed by
// AgentSession.ID). The stamp path — cmd/nightme/run.go's
// sessionContextInto — calls MaybeRefresh once per stamp and
// reads PR() to populate SessionContext.PullRequest. PR() is
// strict-synchronous and never blocks on network I/O; the
// worst-case stamp cost is the duration of an unlocked mutex
// acquire + a struct field read.
//
// Why this lives in its own package: gtw already imports
// chatsession (for dispatch*), so anything that wants to call
// gtw's CollectPR must NOT be imported by chatsession or its
// dependency chain. AgentSession itself is in that chain —
// moving the cache OUT of AgentSession and into this leaf
// package breaks the cycle cleanly.
//
// Lifecycle:
//
//	Runtime startup
//	  → Registry{} constructed once, lives for daemon lifetime
//	Per AgentSession
//	  → Registry.GetOrCreate(as.ID) returns the same *Cache on
//	    repeat calls (multi-stamp + multi-event hot path)
//	Per stamp
//	  → cache.MaybeRefresh(as.Cwd, deps) (sync, no I/O)
//	  → cache.PR()                          (sync, no I/O)
//	Per AgentSession teardown (graceful path)
//	  → in-flight refresh goroutine, if any, is allowed to
//	    complete on its own — bounded by the 3s prober
//	    timeout and the cache TTL (60s). Refresh hits are
//	    stateless (deps.Git.Run + gtw.CollectPR), so an
//	    orphan goroutine cannot corrupt runtime state; it
//	    just wastes a few HTTP round-trips.
//	Daemon shutdown
//	  → registry.CloseAll() (drops every in-flight refresh
//	    goroutine before the daemon exits)
//	/gtw pr success
//	  → cache.Invalidate() (force the next stamp to refresh)
package prcache

import (
	"context"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/command/gtw"
)

// TTL bounds how long a cached PR / MR reference is trusted
// without a fresh `gh pr list` / `glab mr list` round-trip.
// PR metadata is largely stable over a chat session — once the
// user opened the PR, the URL is fixed for the lifetime of
// that branch — so 60 s is a reasonable ceiling: short enough
// that a user switching branches between long pauses sees the
// new branch's PR within a minute, long enough that the
// typical "agent emits N events over a few seconds" stamp
// burst doesn't fork a refresh per message.
const TTL = 60 * time.Second

// Cache holds the most-recently-known open PR / MR associated
// with one AgentSession's current head branch. Safe for
// concurrent PR() / MaybeRefresh() / Invalidate() / Cancel()
// callers.
//
// Field contract:
//
//	pr        — the most-recently-known PR (or nil when no
//	            PR has ever been successfully resolved for
//	            this branch). The footer render path treats
//	            nil as "omit Line 4".
//	branch    — the head-branch name pr was resolved
//	            against. Refresh drops its result if the
//	            branch on disk no longer matches this value
//	            (a defensive check; in the runtime this is
//	            pure belt-and-suspenders — agentsession.Cwd
//	            is immutable post-construction).
//	expiresAt — wall-clock deadline beyond which PR() treats
//	            the cache as stale and the next stamp's
//	            MaybeRefresh spawns a refresh. Zero value =
//	            "never populated yet" — the first call kicks
//	            the first refresh.
//	inflight  — true while a refresh goroutine is running.
//	            Prevents stamp bursts from forking N
//	            parallel `gh pr list` calls. Set / cleared
//	            under mu.
//	cancel    — context.CancelFunc for the in-flight refresh
//	            (or nil when no refresh is running). Cancel
//	            uses this to abort a session tear-down
//	            without waiting for the goroutine to exit.
type Cache struct {
	mu        sync.Mutex
	pr        *gtw.PR
	branch    string
	expiresAt time.Time
	inflight  bool
	cancel    context.CancelFunc
}

// PR returns the cached PR / MR associated with the current
// head branch (or nil if none is known). Synchronous, never
// blocks on network I/O — at worst it returns a stale value
// while a background refresh is in flight.
//
// Callers (the stamp path) typically pair PR() with
// MaybeRefresh(dir, deps) just before reading; the PR()
// result is whatever value the cache currently holds
// (possibly zero), and the next stamp picks up the refreshed
// value once it lands.
func (c *Cache) PR() *gtw.PR {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pr
}

// MaybeRefresh inspects the cache and, when the entry is
// stale (expiresAt in the past) AND no refresh is currently
// in flight, spawns a background goroutine that re-resolves
// the PR / MR via gtw.CollectPR.
//
// Returns immediately. The caller (the stamp path) proceeds
// to read via PR() — which serves the existing value while
// the refresh runs in parallel; the next stamp picks up the
// refreshed value (when successful) or the next-but-one
// stamp does (when the refresh hits ctx.Canceled during
// session teardown).
//
// Idempotent: a pair of concurrent MaybeRefresh() callers
// may both observe the cache as stale, but only one of them
// wins the `inflight == false` race — the other returns
// immediately. No caller is starved.
//
// `dir` is the workspace root to read branch + remote from.
// `deps` is the gtw-side dependency bundle (Git runner /
// HTTP prober / Detect func) the refresh goroutine needs.
//
// The refresh goroutine uses a child context derived from
// context.Background (not from any caller ctx): the
// runtime's Cancel invokes this context directly,
// decoupling the refresh lifetime from any single stamp's
// context.
func (c *Cache) MaybeRefresh(dir string, deps gtw.HandlerDeps) {
	c.mu.Lock()
	now := time.Now()
	expired := !now.Before(c.expiresAt)
	if !expired || c.inflight {
		c.mu.Unlock()
		return
	}
	var ctx context.Context
	ctx, c.cancel = context.WithCancel(context.Background())
	c.inflight = true
	c.mu.Unlock()

	go c.refresh(ctx, dir, deps)
}

// refresh is the body of the async refresh goroutine.
// Resolve the current branch (one local `git symbolic-ref`)
// then hand off to gtw.CollectPR for the network lookup. The
// result lands back in the cache under mu.
//
// Branch-drift protection: between the moment MaybeRefresh
// decided "inflight = false → spawn" and the moment this
// goroutine acquires mu to write back, the underlying
// cwd could in principle have moved (test scenario; the
// runtime doesn't currently change AgentSession.Cwd, but
// the API surface allows for it). We re-read the
// parsed-branch from the disk state via `git symbolic-ref
// --short HEAD` (synchronously, via gtw.CurrentBranch) and
// compare it against the value cached at spawn time. If
// they diverge we drop the result — the next stamp's
// MaybeRefresh spawns a fresh refresh against the new
// branch.
//
// Cancelled mid-flight (Cache.Cancel): exits silently after
// noticing ctx.Err(). The inflight flag is reset by the
// deferred unlock, so a subsequent stamp that happens to
// interleave with Cancel sees a "fresh" cache state
// (inflight = false) and may spawn another refresh — but
// by then session teardown has already started and the new
// refresh will see a closed runtime.
func (c *Cache) refresh(ctx context.Context, dir string, deps gtw.HandlerDeps) {
	defer func() {
		c.mu.Lock()
		c.inflight = false
		c.mu.Unlock()
	}()
	if ctx.Err() != nil {
		return
	}
	// deps.Git is the only required field — the network
	// round-trip (CollectPR) needs a runner. Other deps fields
	// are detected-on-demand inside Detect (URL hint +
	// Stage B probe) and tolerate zero values via the
	// package-level nil fallbacks. Tests that pass an empty
	// HandlerDeps land here; we exit silently rather than
	// segfaulting on deps.Git.Run. The production runtime
	// always wires gtw.ExecGitRunner{} so this guard never
	// fires there.
	if deps.Git == nil {
		return
	}
	branch, err := gtw.CurrentBranch(ctx, dir, deps.Git)
	if err != nil || branch == "" {
		return
	}
	if ctx.Err() != nil {
		return
	}
	pr, _ := gtw.CollectPR(ctx, dir, branch, deps)
	if ctx.Err() != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Branch-drift guard: drop the result if the cache was
	// scheduled against a different branch than the one
	// currently on disk. Invalidating would be a stronger
	// semantic, but dropping is cheaper and self-healing on
	// the next stamp.
	if c.branch != "" && c.branch != branch {
		return
	}
	c.pr = pr
	c.branch = branch
	c.expiresAt = time.Now().Add(TTL)
}

// Invalidate forces the next MaybeRefresh call to spawn a
// background refresh instead of waiting out the TTL. Used
// by /gtw pr after a successful CreatePR (so the footer
// picks up the new URL on the next stamp rather than
// waiting up to 60 s). Safe to call concurrently — the
// "expiresAt = zero" write is mutex-guarded.
func (c *Cache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expiresAt = time.Time{}
}

// Cancel aborts the in-flight refresh goroutine (if any)
// without waiting for it to complete. Used during session
// teardown so a torn-down runtime doesn't leave a goroutine
// running on a closed handle. The goroutine notices
// ctx.Err() at its next checkpoint and exits silently; the
// deferred mu.Unlock clears inflight.
//
// Safe to call multiple times — if no refresh is in
// flight, c.cancel is nil and the no-op branch runs.
func (c *Cache) Cancel() {
	c.mu.Lock()
	cancel := c.cancel
	c.cancel = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Registry is a per-process lookup table from
// AgentSession.ID to *Cache. Use GetOrCreate from the stamp
// path to fetch (or lazily allocate) the cache for an AS.
//
// Locking: the underlying map is guarded by a single
// sync.RWMutex on Registry. Stamp-path reads (the common
// case) take the read lock; GetOrCreate-on-first-stamp takes
// the write lock once and then never again — allocations
// happen at most once per AgentSession lifetime.
//
// The Registry holds NO strong references to AgentSessions:
// the AgentSession owns its own lifecycle, and the cache
// outlives the AS at most by one StampSessionContextInto call
// (after which no further reads happen because the next stamp
// will GetOrCreate under a new AS.ID). Operators don't have
// to call a "drop AS" hook — the OS reclaims the cache when
// the daemon exits, and within a session the population
// stabilises at the chat's working-set size.
type Registry struct {
	mu     sync.RWMutex
	caches map[string]*Cache
}

// Invalidate marks the cache for asID as stale, so the
// next MaybeRefresh call spawns a background refresh
// instead of waiting out the TTL. No-op when no cache has
// been allocated for asID yet (the next stamp allocates one
// and immediately refreshes).
//
// Used by /gtw pr after a successful CreatePR so the new
// URL surfaces on the next footer stamp (not after the 60s
// TTL). The method exists on Registry (not just Cache)
// because dispatchPR only knows the AgentSession.ID, not
// the *Cache instance.
func (r *Registry) Invalidate(asID string) {
	r.mu.RLock()
	c, ok := r.caches[asID]
	r.mu.RUnlock()
	if !ok {
		return
	}
	c.Invalidate()
}

// GetOrCreate returns the Cache for asID, allocating a fresh
// one on first call. Subsequent calls with the same asID
// return the same pointer.
func (r *Registry) GetOrCreate(asID string) *Cache {
	r.mu.RLock()
	if c, ok := r.caches[asID]; ok {
		r.mu.RUnlock()
		return c
	}
	r.mu.RUnlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	// Re-check under write lock to avoid the classic
	// double-allocation race. The common path is the RLock
	// hit above; the write-path is a one-shot per asID.
	if c, ok := r.caches[asID]; ok {
		return c
	}
	c := &Cache{}
	if r.caches == nil {
		r.caches = make(map[string]*Cache)
	}
	r.caches[asID] = c
	return c
}

// CloseAll cancels every Cache's in-flight refresh goroutine
// (if any) and clears the underlying map. Called once on
// daemon shutdown so the OS doesn't exit with goroutines
// still mid-`gh pr list`. Safe to call concurrently with
// MaybeRefresh / PR — the per-Cache cancel is mutex-guarded.
//
// After CloseAll returns, the Registry is empty: a subsequent
// GetOrCreate allocates a fresh Cache. Existing *Cache pointers
// handed out before CloseAll remain valid (they own their own
// state and won't be touched again by the Registry), but their
// background goroutines have been cancelled.
func (r *Registry) CloseAll() {
	r.mu.Lock()
	caches := r.caches
	r.caches = nil
	r.mu.Unlock()
	for _, c := range caches {
		c.Cancel()
	}
}

// --- Test-only API --------------------------------------------------
//
// These methods exist so cross-package tests (cmd/nightme in
// particular) can drive Cache / Registry internal state without
// running a real Detect cycle. Production code MUST NOT use them
// — the field-write semantics bypass the mutex-guarded production
// paths.
//
// They live in production code (not _test.go) so cross-package
// tests can call them. The "ForTest" suffix is the convention
// (matches agentsession.SetHandleForTest / SetStatusForTest).

// SetCachedForTest installs a known PR / branch / TTL into the
// cache. Used by tests that need a non-nil PR() result without
// standing up a working Detect round-trip.
func (c *Cache) SetCachedForTest(pr *gtw.PR, branch string, expiresAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pr = pr
	c.branch = branch
	c.expiresAt = expiresAt
}

// SetInflightForTest installs a hand-driven inflight state with
// the supplied context.CancelFunc. Used by tests that need to
// observe the cancel propagation without spawning MaybeRefresh.
func (c *Cache) SetInflightForTest(cancel context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inflight = true
	c.cancel = cancel
}
