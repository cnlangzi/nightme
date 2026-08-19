// Package openrepl spawns a fresh terminal window that runs a
// `nightme` subcommand, in response to a tray menu click. It is
// platform-agnostic in shape: a single OpenCmd() entry point,
// with the per-OS recipe in openrepl_{unix,windows}.go.
//
// Why this is its own package: the spawn recipe needs to be
// unit-testable in isolation (we mock the *exec.Cmd) without
// dragging in cmd/nightme or the systray library, both of which
// pull CGo and a long dependency chain. The cross-platform shape
// (debounce, error classification) lives in this file; the per-OS
// commands live next to it.
//
// Design (Option B): every tray menu item — primary actions AND
// subcommands — opens a terminal window running `nightme <args>`.
// This gives every command a real TTY (so interactive commands
// like `config` work) and makes the output visible (so non-
// interactive commands like `list` show their result). The old
// in-process `tray.Invoke` path is deleted; there is exactly one
// way to run a command from the tray.
package openrepl

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// debounceWindow is the minimum interval between two OpenCmd()
// invocations for the SAME command from the same process. The
// tray click handler can fire on every press of the icon, and
// macOS touchpads in particular can register 3-4 rapid clicks
// from one user gesture; without this guard, a single click
// would spawn 3-4 terminal windows. 1 second is enough that a
// deliberate second click is always allowed, while accidental
// double-taps are silently dropped.
//
// Debounce is per-command-key: clicking "Open" then immediately
// clicking "list" does NOT suppress each other — only rapid
// repeats of the SAME command are collapsed.
const debounceWindow = 1 * time.Second

// lastCmdNS maps a command key (joined args) to the unix-nanosecond
// timestamp of the last OpenCmd() that was not dropped by the
// debouncer. sync.Map so concurrent LoadOrStore is safe across the
// tray event loop and any future re-entrant caller.
var lastCmdNS sync.Map

// OpenCmd spawns a new terminal window running `nightme <args...>`.
// With no args it enters the REPL; with "logs" it tails the daemon
// log; with "list" / "config" / etc. it runs that subcommand —
// exactly as if the user had typed `nightme <args>` on the shell.
//
// The call is debounced per-command: if OpenCmd(same args) was
// called within debounceWindow, the second call is a no-op and
// returns nil. Different commands do NOT suppress each other.
//
// On no-op returns, the caller (tray click handler) should NOT
// surface any error or log message — dropping the call is the
// intended behaviour, not a failure.
//
// The returned error wraps the underlying spawn failure (missing
// terminal, $DISPLAY unset, osascript refused, etc.) so the tray
// can log it via the daemon's structured logger.
func OpenCmd(args ...string) error {
	key := strings.Join(args, " ")
	v, _ := lastCmdNS.LoadOrStore(key, &atomic.Int64{})
	ts := v.(*atomic.Int64)
	now := time.Now().UnixNano()
	prev := ts.Load()
	if prev != 0 && now-prev < int64(debounceWindow) {
		return nil
	}
	ts.Store(now)
	return openCmd(args...)
}

// ResetDebouncer clears all debounce timestamps. Intended for
// tests; production code should not need this. Exported because
// the only callers are the openrepl_test.go, and the test must
// reset between cases to keep them independent.
func ResetDebouncer() {
	lastCmdNS.Range(func(key, _ any) bool {
		lastCmdNS.Delete(key)
		return true
	})
}
