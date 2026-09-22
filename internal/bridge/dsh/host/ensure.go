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
// are protected by their own mutexes. Concurrent first-touch callers
// share one in-flight StartSharedHost. A failure is not remembered:
// the next call starts again. SetSharedHost panics on double-set, so
// only the in-flight owner installs the singleton, and only after
// StartSharedHost succeeds.

package host

import (
	"context"
	"sync"
)

// ensureCall is one in-flight StartSharedHost. Waiters block on done
// and then read cli/err. The call is dropped when it finishes, so a
// failed start does not stick for the process lifetime.
type ensureCall struct {
	done chan struct{}
	cli  *Client
	err  error
}

var (
	ensureMu       sync.Mutex
	ensureInflight *ensureCall
)

// EnsureSharedHost returns the shared dsh *Client, starting it on
// first call. Subsequent calls return the cached client.
//
// Behaviour:
//   - If GetGlobal() already returns a non-nil client (set by a
//     previous StartSharedHost in this process), it's returned
//     as-is.
//   - Otherwise calls StartSharedHost(ctx, opts). Concurrent
//     callers share that attempt.
//   - On error, returns the error verbatim and leaves no singleton.
//     The next call tries again. A missing dsh binary surfaces here
//     with the underlying exec.LookPath error.
//
// The runtime never calls this; the dsh bridge does (see
// internal/bridge/dsh/session.go newDriver). Daemon boot succeeds
// even if dsh is not installed.
func EnsureSharedHost(ctx context.Context, opts SharedHostOptions) (*Client, error) {
	if cli := GetGlobal(); cli != nil {
		return cli, nil
	}

	ensureMu.Lock()
	if cli := GetGlobal(); cli != nil {
		ensureMu.Unlock()
		return cli, nil
	}
	if ensureInflight != nil {
		call := ensureInflight
		ensureMu.Unlock()
		<-call.done
		return call.cli, call.err
	}
	call := &ensureCall{done: make(chan struct{})}
	ensureInflight = call
	ensureMu.Unlock()

	defer func() {
		ensureMu.Lock()
		if ensureInflight == call {
			ensureInflight = nil
		}
		ensureMu.Unlock()
		close(call.done)
	}()

	h, err := StartSharedHost(ctx, opts)
	if err != nil {
		call.err = err
		return nil, err
	}
	SetSharedHost(h)
	// StartSharedHost already populates the global Client via
	// SetGlobal internally (spawn path).
	call.cli = h.Client()
	return call.cli, nil
}

// ResetEnsureForTest re-initializes the lazy-start state so a
// subsequent EnsureSharedHost call behaves like a first call. Test
// helpers only — production code never invokes this. Pair with
// UnsetGlobal + UnsetSharedHost to fully reset the host package
// between tests.
func ResetEnsureForTest() {
	ensureMu.Lock()
	ensureInflight = nil
	ensureMu.Unlock()
}
