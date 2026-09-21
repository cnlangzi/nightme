// host_state.go — package-level state for the host $events stream.
//
// dsh delivers the $events `ready` handshake as a host stream item:
// {type:"item", streamId:"host-$events",
//
//	value:{type:"ready", clientId, host:{home}}}. The per-connection
//
// clientId is echoed back on every /api/$events/result RPC;
// hostRemoteClientID is the single slot for it.
//
// Capture happens in host/stream.go::dispatch on the host-stream
// item path (the readLoop goroutine, before any host handler runs).
// dsh/session.go::SendPermission reads this slot for the
// /api/$events/result clientId field; if the slot is empty (no ready
// captured), SendPermission errors and the runtime's permission
// answer never reaches dsh.
//
// SetHostClientID is the single source of truth; hostWaterfallHandler
// no longer writes this var.
//
// The RWMutex here is independent of hostWaterfallMu in the dsh
// package (which guards the driver demux table hostWaterfallBySess).
// The two locks never contend on the same data, so they remain
// separate.
package host

import "sync"

var (
	hostClientIDMu     sync.RWMutex
	hostRemoteClientID string
)

// SetHostClientID sets the per-WS clientId captured from the most
// recent host $events ready handshake. No-op when id is empty (a
// malformed ready must not clobber a previously captured good id;
// the next valid ready will overwrite anyway).
//
// Called from host/stream.go::dispatch on the host-stream item
// path (the readLoop goroutine), so the capture happens regardless
// of whether the dsh-level host handler (hostWaterfallHandler) has
// been installed yet. dispatch is the single source of truth.
func SetHostClientID(id string) {
	if id == "" {
		return
	}
	hostClientIDMu.Lock()
	hostRemoteClientID = id
	hostClientIDMu.Unlock()
}

// GetHostClientID returns the per-WS clientId, or "" if no ready
// frame has been captured yet. Used by dsh/session.go::SendPermission
// to fill the /api/$events/result clientId field, and by tests
// that need to assert on capture state without depending on a
// live dsh subprocess.
func GetHostClientID() string {
	hostClientIDMu.RLock()
	defer hostClientIDMu.RUnlock()
	return hostRemoteClientID
}

// ResetHostClientIDForTest clears the captured clientId. ONLY for
// test setup — production code never needs this (a fresh
// dispatch ready overwrites with the new id; a stale id on a
// dead stream is harmless because the old stream's pending
// waterfalls are dead anyway per dsh's disconnect semantics).
//
// The "_ForTest" suffix is the project's convention for
// cross-package test helpers; callers are expected to invoke
// this from `go test` runs only. Lives in the non-test file so
// the dsh/ test package (which can't see host/'s _test.go
// exports) can reach it.
func ResetHostClientIDForTest() {
	hostClientIDMu.Lock()
	hostRemoteClientID = ""
	hostClientIDMu.Unlock()
}
