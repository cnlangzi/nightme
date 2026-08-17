// ensure.go — lazy materialization of the shared dsh host.
//
// The daemon no longer starts dsh at boot. Instead, the dsh bridge's
// newDriver calls EnsureSharedHost the first time it needs a shared
// *Client: the very first dsh session-start pays the spawn cost,
// every subsequent one reuses the cached result.
//
// This is the only place the dsh lifecycle is owned now. The runtime
// never calls it; `SetSharedHost` is fire-and-forget here purely so
// the singleton pointer matches the legacy contract (tests and
// diagnostic tools that read GetSharedHost). Closing the host is
// delegated to the OS — when the daemon exits, the spawned dsh
// stays alive (intended; it's a persistent service on port 3080).
//
// Concurrency: the package's `globalClient` and `sharedHostGlobal`
// are protected by their own mutexes; this helper layers a
// sync.Once on top so concurrent first-touch callers serialize
// through one StartSharedHost invocation. SetSharedHost panics on
// double-set, so the Once is load-bearing — without it two chats
// could race past the nil check and both call SetSharedHost.

package host

import (
	"context"
	"sync"
)

var (
	ensureOnce sync.Once
	ensureErr  error
)

// EnsureSharedHost returns the shared dsh *Client, starting it on
// first call. Subsequent calls return the cached client.
//
// Behaviour:
//   - If GetGlobal() already returns a non-nil client (e.g. a
//     user-launched dsh on port 3080), it's returned as-is.
//   - Otherwise calls StartSharedHost(ctx, opts):
//       * discovers an existing dsh on port 3080 via
//         DiscoverExisting (attaches, ownsProcess=false, no
//         watchdog, no Close — daemon never closes a host it
//         didn't spawn)
//       * spawns a fresh dsh --profile web (ownsProcess=true,
//         watchdog runs and respawns on crash)
//   - On error, returns the error verbatim. A missing dsh binary
//     surfaces here with the underlying exec.LookPath error.
//
// The runtime never calls this; the dsh bridge does (see
// internal/bridge/dsh/session.go newDriver). Daemon boot succeeds
// even if dsh is not installed.
func EnsureSharedHost(ctx context.Context, opts SharedHostOptions) (*Client, error) {
	if cli := GetGlobal(); cli != nil {
		return cli, nil
	}
	ensureOnce.Do(func() {
		// Re-check after acquiring the once: another goroutine may
		// have raced past the first GetGlobal() check and installed
		// the host before us. Without this re-check the second
		// caller would still call SetSharedHost and panic.
		if cli := GetGlobal(); cli != nil {
			return
		}
		h, err := StartSharedHost(ctx, opts)
		if err != nil {
			ensureErr = err
			return
		}
		SetSharedHost(h)
		// StartSharedHost already populates the global Client via
		// SetGlobal internally (both the reuse path and the spawn
		// path). No-op here.
	})
	if ensureErr != nil {
		return nil, ensureErr
	}
	return GetGlobal(), nil
}

// ResetEnsureForTest re-initializes the lazy-start state so a
// subsequent EnsureSharedHost call behaves like a first call. Test
// helpers only — production code never invokes this. Pair with
// UnsetGlobal + UnsetSharedHost to fully reset the host package
// between tests.
//
// We need this because sync.Once has no reset method, and the
// once-fired state would otherwise carry across tests in the same
// binary, masking per-test regressions.
func ResetEnsureForTest() {
	ensureOnce = sync.Once{}
	ensureErr = nil
}
