// Package main — system-tray plumbing shared by every build.
//
// This file holds the parts of the tray feature that do NOT touch
// github.com/getlantern/systray: the debouncer, the options struct
// the daemon lifecycle passes in, and the click error logger. The
// systray-dependent implementation lives in tray_gui.go, and a
// no-op replacement lives in tray_stub.go.
//
// # Why the split exists (the `gui` build tag)
//
// getlantern/systray is CGo. On Linux it resolves to
// systray_linux_ayatana.go, which carries
//
//	#cgo linux pkg-config: ayatana-appindicator3-0.1
//
// so merely importing the package makes the linker record
// libayatana-appindicator3.so.1, libgtk-3.so.0 and ~70 other
// DT_NEEDED entries in the binary. When that import was
// unconditional, EVERY Linux build — including `nightme --version`
// on a headless server — died before main() with
//
//	error while loading shared libraries:
//	libayatana-appindicator3.so.1: cannot open shared object file
//
// unless the GTK3 runtime happened to be installed. That failure
// is emitted by ld.so, so no amount of Go-side guarding can catch
// it; in particular isHeadless() (tray_headless_linux.go) is a
// RUNTIME check and never gets the chance to run.
//
// Note that `CGO_ENABLED=0` alone does not fix this: systray has
// no non-CGo Linux fallback and the package itself fails to
// compile (undefined: nativeLoop, registerSystray, …). The import
// has to be excluded at the source level, which is what the build
// tags below do.
//
// Tag semantics — the default is deliberately per-platform:
//
//	go build                → Linux: no tray. macOS/Windows: tray.
//	go build -tags gui      → Linux: tray.
//
// Linux defaults to OFF because Linux hosts are overwhelmingly
// servers, and a server binary must not carry a GUI toolkit
// dependency. macOS and Windows default to ON because their tray
// backings (Cocoa / Win32) ship with the OS, so there is no
// missing-library failure mode to avoid. This also means a plain
// `go install github.com/cnlangzi/nightme/cmd/nightme` produces a
// working binary on a bare Linux box.
//
// Release artifacts follow the same split — see
// .github/workflows/release.yml:
//
//	nightme_<v>_linux_<arch>.tar.gz       CGO_ENABLED=0, no tray, static
//	nightme_<v>_linux_<arch>-gui.tar.gz   -tags gui, tray, needs GTK3
//
// ci.yml asserts with ldd that the default Linux binary links
// neither GTK nor appindicator, so this regression cannot come
// back unnoticed.

package main

import (
	"log/slog"
	"sync/atomic"
	"time"
)

// trayDebounce is the minimum interval between two clicks being
// honoured on the same primary menu item. 500ms is short enough
// that a deliberate double-click is allowed (a user retrying
// "Restart" because the first click didn't visibly do anything),
// and long enough that a macOS touchpad double-tap from a single
// user gesture does not spawn two REPL windows / send two
// SIGTERMs. (Previously openrepl carried its own debouncer;
// that layer was removed when terminal-spawn moved into proc —
// the tray click-level debounce is the single guard now.)
const trayDebounce = 500 * time.Millisecond

// clickTracker debounces a single menu item. Each menu item gets
// its own clickTracker instance so clicks on different items are
// independent — a single global debouncer (the v1 design) would
// drop the second click on a DIFFERENT item if the user happened
// to click two items in quick succession.
//
// Atomic so the load-store is concurrency-safe across the
// systray event loop goroutine and any future re-entrant caller.
type clickTracker struct {
	lastNS atomic.Int64
}

func (c *clickTracker) allow(now int64) bool {
	prev := c.lastNS.Load()
	if prev != 0 && now-prev < int64(trayDebounce) {
		return false
	}
	c.lastNS.Store(now)
	return true
}

// trayOptions collects the runtime dependencies the tray builder
// needs to wire up menu items. The two callbacks (onStop /
// onRestart) are kept abstract so the test path can substitute
// channels and so the platform-specific spawn recipe (proc.New
// with the right SysProcAttr) lives in daemon_lifecycle_*.go
// where it already lives for `nightme start`.
//
// Declared here rather than in tray_gui.go because the no-tray
// stub's runTrayOwning shares the signature, and
// daemon_lifecycle_{unix,windows}.go construct the struct in
// every build configuration.
type trayOptions struct {
	// reg is the cmdRegistry whose TrayItems() drives the
	// subcommand section below the separator. Must be non-nil.
	reg *cmdRegistry
	// logger receives structured error logs when a click
	// handler fails. May be nil (handler logs to slog.Default).
	logger *slog.Logger
	// onStopRequest is the handler the "Stop" / "Quit" menu
	// items fire. The default production handler sends
	// SIGTERM to self; tests substitute a no-op or a channel
	// notification.
	onStopRequest func()
	// onRestartRequest is the handler the "Restart" menu
	// item fires. The default production handler spawns a
	// new _daemon child then sends SIGTERM to self.
	onRestartRequest func()
}

// logClickErr formats a click error for the daemon's structured
// log. Click errors are operational, not fatal — they are the
// "I clicked Open REPL and nothing happened" symptom — so the
// log line is the user's only signal.
func logClickErr(logger *slog.Logger, label string, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("tray click failed", "item", label, "err", err.Error())
}
