// Package openrepl spawns a fresh terminal window that runs the
// nightme REPL, in response to a "Open REPL" click on the system
// tray menu. It is platform-agnostic in shape: a single Open()
// entry point, with the per-OS recipe in openrepl_{unix,windows}.go.
//
// Why this is its own package: the spawn recipe needs to be
// unit-testable in isolation (we mock the *exec.Cmd) without
// dragging in cmd/nightme or the systray library, both of which
// pull CGo and a long dependency chain. The cross-platform shape
// (debounce, no-op when stdin/stdout is not a TTY, error
// classification) lives in this file; the per-OS commands live
// next to it.
//
// The whole module is intentionally tiny: if a future contributor
// is tempted to grow it, that is the signal that some of the
// responsibility (e.g. terminal preference discovery) wants to be
// in internal/config instead.
package openrepl

import (
	"sync/atomic"
	"time"
)

// debounceWindow is the minimum interval between two Open()
// invocations from the same process. The tray click handler can
// fire on every press of the icon, and macOS touchpads in
// particular can register 3-4 rapid clicks from one user gesture;
// without this guard, a single click would spawn 3-4 REPL
// windows. 1 second is enough that a deliberate second click is
// always allowed, while accidental double-taps are silently
// dropped.
const debounceWindow = 1 * time.Second

// lastOpenNS is the unix-nanosecond timestamp of the last Open()
// that was not dropped by the debouncer. atomic.Int64 so the
// load-store is concurrency-safe across the tray event loop and
// any future re-entrant caller.
var lastOpenNS atomic.Int64

// Open spawns a new terminal window running `nightme` (REPL
// mode). The call is debounced: if Open() was called within
// debounceWindow, the second call is a no-op and returns nil.
//
// On no-op returns, the caller (tray click handler) should NOT
// surface any error or log message — dropping the call is the
// intended behaviour, not a failure.
//
// The returned error wraps the underlying spawn failure (missing
// terminal, $DISPLAY unset, osascript refused, etc.) so the tray
// can log it via the daemon's structured logger.
func Open() error {
	now := time.Now().UnixNano()
	prev := lastOpenNS.Load()
	if prev != 0 && now-prev < int64(debounceWindow) {
		return nil
	}
	lastOpenNS.Store(now)
	return open()
}

// ResetDebouncer clears the debounce timestamp. Intended for
// tests; production code should not need this. Exported because
// the only callers are the openrepl_test.go, and the test must
// reset between cases to keep them independent.
func ResetDebouncer() { lastOpenNS.Store(0) }
