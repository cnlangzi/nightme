// Package prcache — per-AgentSession cache for the GitHub /
// GitLab PR (or MR) associated with the current head branch,
// refreshed asynchronously on a 60s TTL.
//
// Scope: ONE Cache per AgentSession, owned externally (the
// runtime in cmd/nightme keeps a Registry keyed by
// AgentSession.ID). The PR lookup closure the runtime builds
// at startup — internal/runtime/runtime.go's LookupPR, which
// the Emitter's GitStatusLookup reaches through
// ChatSession.GitStatus on every outbound stamp — calls
// cache.MaybeRefresh(cwd, resolver) and then cache.PR() to
// populate StatusBar.PullRequest. PR() is strict-synchronous
// and never blocks on network I/O; the worst-case stamp cost
// is the duration of an unlocked mutex acquire + a struct
// field read. MaybeRefresh is sync no-I/O itself, only
// spawning a goroutine when the 60s TTL has elapsed AND no
// refresh is in flight.
//
// The PR cache also supports direct writes via Cache.WritePR
// / Registry.WritePR — used by /gtw {pr,close} success paths
// where we already know the answer (the new PR number from
// `gh pr create`, or "branch deleted, drop the entry"). No
// goroutine spawn, just a synchronous overwrite. nil pr =
// clear the entry.
//
// Why this lives in its own package: gtw already imports
// chatsession (for dispatch*), so anything that wants to call
// gtw's CollectPR must NOT be imported by chatsession or its
// dependency chain. AgentSession itself is in that chain —
// moving the cache OUT of AgentSession and into this leaf
// package breaks the cycle cleanly. The runtime builds the
// Resolver closure that bridges prcache and gtw at startup
// (in runtime.go, outside both packages' import cycles).
//
// Lifecycle:
//
//	Runtime startup
//	  → Registry{} constructed once, lives for daemon lifetime
//	  → runtime builds a Resolver closure (gtw.CurrentBranch
//	    + gtw.CollectPR composed), injected into MaybeRefresh
//	    via the per-stamp LookupPR closure
//	Per AgentSession
//	  → Registry.GetOrCreate(as.ID) returns the same *Cache on
//	    repeat calls (multi-stamp + multi-event hot path)
//	Per stamp (every outbound message → cs.GitStatus(ctx) →
//	            deps.LookupPR(as.ID, cwd))
//	  → cache.MaybeRefresh(cwd, resolver) (sync, no I/O)
//	  → cache.PR()                          (sync, no I/O)
//	/gtw pr success
//	  → Registry.WritePR(as.ID, newPR) per AS in the chat pool
//	    (we already know the number from `gh pr create`; no
//	    refresh needed; the next stamp will lazy-refresh and
//	    correct any branch mismatch within 60 s)
//	/gtw close success
//	  → Registry.WritePR(as.ID, nil) per AS in the chat pool
//	    (the branch is being deleted; no refresh needed; the
//	    next stamp will lazy-refresh from scratch)
//	Per AgentSession teardown
//	  → Cache.Cancel is defined but NOT currently wired into
//	    chatsession / agentsession teardown — the in-flight
//	    refresh goroutine, if any, is allowed to complete on
//	    its own (bounded by the prober timeout + cache TTL).
//	    This is intentional: refresh work is stateless, so an
//	    orphan goroutine after the AgentSession is gone
//	    cannot corrupt runtime state — it just wastes a few
//	    HTTP round-trips. If a future caller wants strict
//	    cancellation, wire Cancel into the AS reap path; the
//	    method exists for that reason.
//	Daemon shutdown
//	  → Registry.CloseAll cancels every per-AS in-flight
//	    refresh goroutine and clears the registry map (wired
//	    in cmd/nightme/shutdownRun).
package prcache

import (
	"context"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
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

// failureBackoff bounds how long a failed refresh is trusted
// before another attempt is allowed. refresh() pushes this
// deadline onto expiresAt when the Resolver returns an empty
// branch or a non-nil error — without it, a permanently-broken
// state (no git, detached HEAD, missing origin, gh auth
// expired) would fork a fresh goroutine on every single stamp,
// since expiresAt would otherwise stay zero (never updated)
// and MaybeRefresh would consider the cache stale forever.
//
// 5 s is a reasonable compromise: short enough that a
// transient `git` failure (e.g. mid-checkout race) recovers
// within a handful of stamps; long enough that a workspace
// that simply has no git PR / MR stops trying. WritePR(pr)
// resets expiresAt to now+TTL so a freshly created PR surfaces
// on the next stamp even inside the backoff window.
const failureBackoff = 5 * time.Second

// Resolver re-resolves the PR / MR for the given workspace
// dir. Returns (pr, branch, err):
//
//	pr     — the resolved PR, or nil when the workspace has
//	         no open PR / MR. nil PR with err==nil is normal
//	         ("branch has no PR"); the cache accepts nil and
//	         writes it through.
//	branch — the head branch the resolver read from dir.
//	         Empty string signals "couldn't resolve" (no git
//	         / detached HEAD / etc.) — the cache applies the
//	         failureBackoff in that case.
//	err    — non-nil for transient I/O failures (network,
//	         gh auth, etc.); the cache applies failureBackoff.
//
// Production: a closure built in runtime.go that wraps
// gtw.CurrentBranch + gtw.CollectPR. Tests inject stubs.
// Defined as a function type so prcache can stay a leaf
// package (no `gtw` import needed).
type Resolver func(ctx context.Context, dir string) (pr *messages.PR, branch string, err error)

// Cache holds the most-recently-known open PR / MR associated
// with one AgentSession's current head branch. Safe for
// concurrent PR() / MaybeRefresh() / WritePR() / Cancel()
// callers.
//
// Field contract:
//
//	pr        — the most-recently-known PR (or nil when no
//	            PR has ever been successfully resolved for
//	            this branch, OR the branch has no PR). The
//	            footer render path treats nil as "omit the
//	            PR tail segment".
//	branch    — the head-branch name pr was resolved
//	            against. The current refresh writes whatever
//	            branch the Resolver reports; WritePR leaves
//	            this untouched (the caller doesn't know which
//	            branch each AS is pinned to; the next
//	            MaybeRefresh will overwrite).
//	expiresAt — wall-clock deadline beyond which PR() treats
//	            the cache as stale and the next stamp's
//	            MaybeRefresh spawns a refresh. Zero value =
//	            "never populated yet" — the first call kicks
//	            the first refresh. failureBackoff is set on
//	            the same field when the refresh fails (no
//	            git / detached HEAD / etc.) so a permanently-
//	            broken state doesn't fork a refresh goroutine
//	            on every stamp.
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
	pr        *messages.PR
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
// MaybeRefresh(dir, resolver) just before reading; the PR()
// result is whatever value the cache currently holds
// (possibly zero), and the next stamp picks up the refreshed
// value once it lands.
func (c *Cache) PR() *messages.PR {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pr
}

// MaybeRefresh inspects the cache and, when the entry is
// stale (expiresAt in the past) AND no refresh is currently
// in flight, spawns a background goroutine that calls the
// Resolver to re-resolve the PR / MR. Returns immediately.
//
// The caller (the stamp path) proceeds to read via PR() —
// which serves the existing value while the refresh runs in
// parallel; the next stamp picks up the refreshed value
// (when successful) or the next-but-one stamp does (when the
// refresh hits ctx.Canceled during session teardown).
//
// Idempotent: a pair of concurrent MaybeRefresh() callers
// may both observe the cache as stale, but only one of them
// wins the `inflight == false` race — the other returns
// immediately. No caller is starved.
//
// `dir` is the workspace root the Resolver reads branch +
// remote from. `resolver` is the I/O function the refresh
// goroutine calls (production: gtw.CurrentBranch +
// gtw.CollectPR composed in runtime.go).
//
// The refresh goroutine uses a child context derived from
// context.Background (not from any caller ctx): the
// runtime's Cancel invokes this context directly,
// decoupling the refresh lifetime from any single stamp's
// context.
func (c *Cache) MaybeRefresh(dir string, resolver Resolver) {
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

	go c.refresh(ctx, dir, resolver)
}

// refresh is the body of the async refresh goroutine. It
// calls the Resolver and applies the result to the cache
// fields under mu. Empty branch / non-nil error both push
// failureBackoff onto expiresAt so a permanently-broken
// workspace doesn't fork a new refresh on every stamp.
//
// Cancelled mid-flight (Cache.Cancel): exits silently after
// noticing ctx.Err(). The inflight flag is reset by the
// deferred unlock, so a subsequent stamp that happens to
// interleave with Cancel sees a "fresh" cache state
// (inflight = false) and may spawn another refresh — but
// by then session teardown has already started and the new
// refresh will see a closed runtime.
func (c *Cache) refresh(ctx context.Context, dir string, resolver Resolver) {
	defer func() {
		c.mu.Lock()
		c.inflight = false
		c.mu.Unlock()
	}()
	if ctx.Err() != nil {
		return
	}
	if resolver == nil {
		return
	}
	pr, branch, err := resolver(ctx, dir)
	if ctx.Err() != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil || branch == "" {
		// Push a small back-off so a permanently-broken
		// state doesn't fork a fresh refresh on every
		// stamp. Without this, the next stamp's
		// MaybeRefresh sees expiresAt still in the past
		// and spawns again — stamp storm.
		//
		// Also clear c.pr (and c.branch): the previous
		// refresh's PR is for a different branch (or no
		// branch), and surfacing it on the workspace
		// footer now would be misleading. Detached HEAD
		// with a previously-cached PR is the canonical
		// case: without this clear, the footer would
		// render `⎇ ? · [#N](url)` pointing at the old
		// branch's PR until the next successful refresh
		// overwrites it.
		c.pr = nil
		c.branch = branch
		c.expiresAt = time.Now().Add(failureBackoff)
		return
	}
	c.pr = pr
	c.branch = branch
	c.expiresAt = time.Now().Add(TTL)
}

// WritePR overwrites the cache entry with pr. nil pr clears
// the entry (pr=nil, branch="", expiresAt=zero so the next
// MaybeRefresh kicks a fresh refresh from scratch); non-nil
// pr sets pr, leaves branch untouched, resets expiresAt to
// now+TTL. Sync, no I/O, no goroutine spawn.
//
// Used by /gtw dispatchers where we already know the answer:
//   - /gtw pr success: WritePR(&messages.PR{...}) — the new
//     PR number from `gh pr create`.
//   - /gtw close success: WritePR(nil) — the branch is being
//     deleted; the next stamp's lazy MaybeRefresh will fetch
//     fresh (or stay nil if the workspace has no PR).
//
// branch is intentionally NOT touched: the caller (gtw
// dispatch walking the chat pool) doesn't know which branch
// each AS is pinned to, and writing a wrong branch would
// produce a transient inconsistency on the footer that
// self-corrects within 60 s on the next MaybeRefresh.
// Cache.PR() reads only the pr field, so the footer renders
// the new pr immediately.
func (c *Cache) WritePR(pr *messages.PR) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if pr == nil {
		c.pr = nil
		c.branch = ""
		c.expiresAt = time.Time{}
		return
	}
	c.pr = pr
	c.expiresAt = time.Now().Add(TTL)
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
// Use WritePR from /gtw dispatchers to apply a known PR
// (or nil, to clear) to every AS in a chat's pool.
//
// Locking: the underlying map is guarded by a single
// sync.RWMutex on Registry. Stamp-path reads (the common
// case) take the read lock; GetOrCreate-on-first-stamp takes
// the write lock once and then never again — allocations
// happen at most once per AgentSession lifetime.
//
// The Registry holds NO strong references to AgentSessions:
// the AgentSession owns its own lifecycle, and the cache
// outlives the AS at most by one StampStatusBarInto call
// (after which no further reads happen because the next stamp
// will GetOrCreate under a new AS.ID). Operators don't have
// to call a "drop AS" hook — the OS reclaims the cache when
// the daemon exits, and within a session the population
// stabilises at the chat's working-set size.
type Registry struct {
	mu     sync.RWMutex
	caches map[string]*Cache
}

// WritePR writes pr into the cache for asID. nil clears the
// entry. No-op when no cache has been allocated for asID yet
// (the chat either hasn't been stamped enough to allocate
// one, or its ID isn't registered) — we never allocate
// proactively, because the cost of a stale allocation is
// nil and the cost of a fresh one is one map write.
//
// Used by /gtw {pr, close} success paths to apply a known
// PR result without a network round-trip. Wraps Cache.WritePR
// for the asID-keyed lookup.
func (r *Registry) WritePR(asID string, pr *messages.PR) {
	r.mu.RLock()
	c, ok := r.caches[asID]
	r.mu.RUnlock()
	if !ok {
		return
	}
	c.WritePR(pr)
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
