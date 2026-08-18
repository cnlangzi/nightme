// Package main — system-tray icon for the nightme daemon.
//
// This file is the daemon-child side of the tray feature. It is
// built on top of github.com/getlantern/systray, which is CGo and
// has a per-OS native event loop. The structure here reflects
// three constraints:
//
//  1. systray.Run blocks the calling thread (it is the main
//     thread on macOS where Cocoa insists). The daemon's runtime
//     is also blocking, so we have to flip the threading model:
//     the runtime runs in a goroutine, systray.Run occupies the
//     main thread. The two exit paths are wired so the other
//     learns about the first's completion via systray.Quit() and
//     a shared error variable.
//
//  2. The set of menu items must stay in sync with the cobra
//     subcommand tree and the REPL banner. cmdRegistry.TrayItems
//     (subcommand.go) is the single source of truth; this file
//     is a consumer, not a re-derivation. The lifecycle commands
//     (start/stop/restart/status) and the TTY-bound commands
//     (test/login/logs/config/update/debug) are filtered out by
//     the registry's addNoTray path so they do not appear here.
//
//  3. Click actions must not block the tray event loop. Each
//     click handler runs in systray's event goroutine; a handler
//     that calls into the daemon synchronously (e.g. daemoncontrol.Stop)
//     would freeze the menu until the call returns. The pattern
//     is: handler spawns a goroutine and returns immediately.
//
// Three primary menu items live outside the dynamic "Commands"
// submenu because they need extra logic, not just a cobra
// re-dispatch:
//
//   - Open REPL  → internal/tray/openrepl.Open() (spawns a new
//                  terminal window with `nightme` REPL mode)
//   - Restart    → caller-supplied callback that spawns a new
//                  _daemon child and signals the current one
//                  to exit (default impl in daemon_lifecycle_*.go)
//   - Stop       → caller-supplied callback that triggers the
//                  runtime's graceful shutdown (default sends
//                  SIGTERM to self)
//
// Quit is the systray.Quit-only release. In v1 Quit is
// equivalent to Stop — "release tray but keep daemon running" is
// a footgun (the user expects the daemon to stop when they close
// the menu-bar icon) and we have no clean way to drop only the
// systray handle. If a future feature wants that, the cleanest
// split is to make the daemon outlive multiple systray lifetimes
// and expose that as a separate "Hide" item.

package main

import (
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/getlantern/systray"
	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/runtime"
	"github.com/cnlangzi/nightme/internal/tray"
	"github.com/cnlangzi/nightme/internal/tray/openrepl"
)

// trayDebounce is the minimum interval between two clicks being
// honoured on the same primary menu item. 500ms is short enough
// that a deliberate double-click is allowed (a user retrying
// "Restart" because the first click didn't visibly do anything),
// and long enough that a macOS touchpad double-tap from a single
// user gesture does not spawn two REPL windows / send two
// SIGTERMs. The openrepl package has its own debouncer too, but
// having one at the click-dispatch level catches the other
// primaries (Stop / Restart / Commands) which don't go through
// openrepl.
const trayDebounce = 500 * time.Millisecond

// lastClickNS is the unix-nanosecond timestamp of the most recent
// primary-item click that was honoured. atomic.Int64 so the
// load-store is concurrency-safe across the systray event loop
// goroutine and any future re-entrant caller.
var lastClickNS atomic.Int64

// trayOptions collects the runtime dependencies the tray builder
// needs to wire up menu items. The two callbacks (onStop /
// onRestart) are kept abstract so the test path can substitute
// channels and so the platform-specific spawn recipe (proc.New
// with the right SysProcAttr) lives in daemon_lifecycle_*.go
// where it already lives for `nightme start`.
type trayOptions struct {
	// reg is the cmdRegistry whose TrayItems() drives the
	// "Commands" submenu. Must be non-nil.
	reg *cmdRegistry
	// channelName is the channel the daemon was started with
	// (e.g. "feishu", "echo"). Surfaced in the Status info
	// row so the user can see at a glance which mode the
	// running daemon is in.
	channelName string
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

// runTrayOwning blocks the calling thread (main thread on macOS,
// arbitrary thread on Linux/Windows) until both the daemon
// runtime has returned AND the systray event loop has been
// released. It is the daemon child's "main loop" replacement: it
// owns the goroutine that runs runtime.Run, and it owns the
// systray native loop, and the two are coupled by a single
// shared error variable and systray.Quit().
//
// Returns whatever error the daemon runtime produced (nil for
// graceful shutdown). Click errors are logged but never
// returned.
//
// runDeps is the runtime.Deps the daemon should run with. Passed
// in (rather than constructed here) so the call site can apply
// the same dependency-injection rules the existing runRunWith
// path already enforces.
func runTrayOwning(cmd *cobra.Command, runDeps runtime.Deps, opts trayOptions) error {
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- runRunWith(cmd, runDeps)
	}()

	// systray.Run blocks until systray.Quit() is called. We
	// call Quit from a watcher goroutine that observes the
	// runtime's exit. When Quit fires, systray runs the
	// onExit callback and Run returns; we then read the
	// runtime's exit code and return it to the caller.
	var runErr error
	go func() {
		runErr = <-runErrCh
		systray.Quit()
	}()

	systray.Run(
		func() { trayOnReady(opts) },
		func() { /* onExit: systray native cleanup is automatic */ },
	)

	return runErr
}

// trayOnReady is the systray onReady callback. It runs in
// systray's event goroutine, NOT the main thread — handlers that
// touch the main thread (Cocoa) must marshal themselves.
//
// The menu is rebuilt from scratch on every onReady. systray does
// not support a "menu" object that is mutated; the contract is
// "build the whole tree in onReady". For our 13 items this is
// instantaneous and gives a clean state on every daemon start.
func trayOnReady(opts trayOptions) {
	systray.SetTitle("NightMe")
	systray.SetTooltip("NightMe daemon — click to open the menu")
	applyIcon()

	// Disabled info row at the top so the user can see at a
	// glance whether the daemon is the one they expect.
	statusItem := systray.AddMenuItem(
		fmt.Sprintf("Status: running · %s", opts.channelName),
		"Read-only status line — the running channel is shown for at-a-glance confirmation",
	)
	statusItem.Disable()

	systray.AddSeparator()

	// Primary actions. Each handler is debounced to absorb
	// accidental double-clicks.
	openREPL := systray.AddMenuItem("Open REPL", "Spawn a new terminal running the nightme REPL")
	restart := systray.AddMenuItem("Restart", "Stop this daemon and start a fresh one in the background")
	stop := systray.AddMenuItem("Stop", "Gracefully stop the nightme daemon")
	go handleClick(openREPL, "open-repl", func() {
		if err := openrepl.Open(); err != nil {
			logClickErr(opts.logger, "open-repl", err)
		}
	})
	go handleClick(restart, "restart", func() {
		if opts.onRestartRequest != nil {
			opts.onRestartRequest()
		}
	})
	go handleClick(stop, "stop", func() {
		if opts.onStopRequest != nil {
			opts.onStopRequest()
		}
	})

	systray.AddSeparator()

	// "Commands" submenu — built from cmdRegistry.TrayItems.
	// Order matches the REPL banner order so a user who has
	// the REPL banner memorised finds the same verbs in the
	// same sequence in the tray. The parent itself is
	// informational (disabled) — only the children are
	// clickable.
	cmds := systray.AddMenuItem("Commands", "Submenu mirroring the REPL 'Common:' list (lifecycle + TTY commands are filtered out)")
	cmds.Disable()
	for _, item := range opts.reg.TrayItems() {
		cmdItem := cmds.AddSubMenuItem(item.Title, item.Tooltip)
		// Capture loop variable for the goroutine.
		captured := item
		go handleClick(cmdItem, captured.Title, func() {
			if err := tray.Invoke(captured.Command); err != nil {
				logClickErr(opts.logger, "command:"+captured.Title, err)
			}
		})
	}

	systray.AddSeparator()

	// Quit = stop + release tray. See file doc comment for
	// the rationale on merging Quit with Stop.
	quit := systray.AddMenuItem("Quit", "Stop the daemon and release the tray icon")
	go handleClick(quit, "quit", func() {
		if opts.onStopRequest != nil {
			opts.onStopRequest()
		}
	})
}

// handleClick is the shared click-dispatch helper. It drains the
// menu item's ClickedCh (one signal per click) and applies the
// trayDebounce. Returns immediately on debounce so the systray
// event loop is never blocked.
//
// The wrapped action runs in its own goroutine so the click
// drain is not coupled to action completion — even a hung action
// (e.g. a CLI command that blocks on stdin) does not freeze
// subsequent clicks.
func handleClick(item *systray.MenuItem, label string, action func()) {
	for range item.ClickedCh {
		now := time.Now().UnixNano()
		prev := lastClickNS.Load()
		if prev != 0 && now-prev < int64(trayDebounce) {
			continue
		}
		lastClickNS.Store(now)
		go action()
	}
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
