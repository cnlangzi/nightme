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
//  2. The subcommand section is driven by cmdRegistry.TrayItems
//     (subcommand.go), the single source of truth shared with
//     the REPL banner and the CLI tree. This file is a consumer,
//     not a re-derivation. Lifecycle commands (start/stop/restart/
//     status) and TTY-bound commands (test/login/logs/config/
//     update/debug) are filtered out by the registry's addNoTray
//     path so they do not appear here.
//
//  3. Click actions must not block the tray event loop. Each
//     click handler runs in systray's event goroutine; a handler
//     that calls into the daemon synchronously (e.g. daemoncontrol.Stop)
//     would freeze the menu until the call returns. The pattern
//     is: handler spawns a goroutine and returns immediately.
//
// Menu layout (all items are top-level — no submenus):
//
//   Running        (disabled info row)
//   ─────────
//   Open            → proc.OpenTerminal() (spawn a terminal with the REPL)
//   Logs            → proc.OpenTerminal("logs") (spawn a terminal tailing the log)
//   Restart         → caller callback (spawn new _daemon child, SIGTERM self)
//   Stop            → caller callback (trigger runtime graceful shutdown)
//   ─────────
//   list / kill / agents / name / clean / version
//                   → proc.OpenTerminal(title) — spawn terminal running `nightme <title>`
//
// The subcommands are top-level items, NOT children of a disabled
// submenu. The v1 design put them under a disabled "Commands"
// parent; on Windows, disabling the parent cascades MF_GRAYED to
// every child, making them all appear unclickable. Top-level items
// are always interactive regardless of platform.

package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/getlantern/systray"
	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/proc"
	"github.com/cnlangzi/nightme/internal/runtime"
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

// runTrayOwning blocks the calling thread (main thread on macOS,
// arbitrary thread on Linux/Windows) until both the daemon
// runtime has returned AND the systray event loop has been
// released. It is the daemon child's "main loop" replacement: it
// owns the goroutine that runs runtime.Run, and it owns the
// systray native loop, and the two are coupled via a single
// channel and systray.Quit().
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

	// Watcher goroutine: when the runtime returns, release
	// the systray native loop. The channel close (implicit
	// when runRunWith returns) synchronises the value
	// delivery with the read below on the calling thread.
	go func() {
		<-runErrCh
		systray.Quit()
	}()

	systray.Run(
		func() { trayOnReady(opts) },
		func() { /* onExit: systray native cleanup is automatic */ },
	)

	// After systray.Run returns, read the runtime's exit
	// code on the same goroutine. The watcher goroutine
	// received it from runErrCh (with channel-close
	// synchronisation); reading it again on this thread is
	// safe because runErrCh has buffer 1 and no other
	// receiver is competing.
	return <-runErrCh
}

// trayOnReady is the systray onReady callback. It runs in
// systray's event goroutine, NOT the main thread — handlers that
// touch the main thread (Cocoa) must marshal themselves.
//
// The menu is rebuilt from scratch on every onReady. systray does
// not support a "menu" object that is mutated; the contract is
// "build the whole tree in onReady".
//
// Layout:
//
//	Running          (disabled info row)
//	─────────
//	Open / Logs / Restart / Stop   (primary actions)
//	─────────
//	list / kill / agents / ...     (subcommands from TrayItems)
//
// The subcommands are top-level items, NOT children of a disabled
// submenu. The v1 design put them under a disabled "Commands"
// parent; on Windows, disabling the parent cascades MF_GRAYED to
// every child, making them all appear unclickable. Top-level items
// are always interactive regardless of platform.
func trayOnReady(opts trayOptions) {
	systray.SetTooltip("NightMe daemon — click to open the menu")
	applyIcon()

	// Read-only status row. Kept minimal — just "Running" —
	// because the daemon now auto-starts every channel with
	// valid creds, so a single channel label would be misleading
	// and listing all of them would overflow the menu width.
	statusItem := systray.AddMenuItem("Running", "nightme daemon is running")
	statusItem.Disable()

	systray.AddSeparator()

	// Primary actions. Each gets its own clickTracker (per-item
	// debouncer) so rapid clicks on DIFFERENT items are still
	// honoured independently.
	openREPL := systray.AddMenuItem("Open", "Spawn a new terminal running the nightme REPL")
	logs := systray.AddMenuItem("Logs", "Open a terminal showing the daemon log (tail -f)")
	restart := systray.AddMenuItem("Restart", "Restart the nightme daemon")
	stop := systray.AddMenuItem("Stop", "Gracefully stop the nightme daemon")

	go handleClick(openREPL, func() {
		if err := proc.OpenTerminal(context.Background(), "nightme"); err != nil {
			logClickErr(opts.logger, "open-repl", err)
		}
	})
	go handleClick(logs, func() {
		if err := proc.OpenTerminal(context.Background(), "nightme", "logs"); err != nil {
			logClickErr(opts.logger, "open-logs", err)
		}
	})
	go handleClick(restart, func() {
		if opts.onRestartRequest != nil {
			opts.onRestartRequest()
		}
	})
	go handleClick(stop, func() {
		if opts.onStopRequest != nil {
			opts.onStopRequest()
		}
	})

	systray.AddSeparator()

	// Subcommands — built from cmdRegistry.TrayItems(), the same
	// source the REPL banner and CLI tree use. Order matches the
	// REPL banner so the tray menu reads the same as the REPL
	// "Common:" list. Each is a top-level item (clickable, not
	// greyed out) that opens a terminal running `nightme <title>`
	// via proc.OpenTerminal — the same path as Open / Logs, giving
	// every command a real TTY and visible output.
	for _, item := range opts.reg.TrayItems() {
		cmdItem := systray.AddMenuItem(item.Title, item.Tooltip)
		ci := item
		go handleClick(cmdItem, func() {
			if err := proc.OpenTerminal(context.Background(), "nightme", ci.Title); err != nil {
				logClickErr(opts.logger, "command:"+ci.Title, err)
			}
		})
	}
}

// handleClick is the shared click-dispatch helper. It drains the
// menu item's ClickedCh (one signal per click) and applies the
// per-item debouncer. Returns immediately on debounce so the
// systray event loop is never blocked.
//
// The wrapped action runs in its own goroutine so the click
// drain is not coupled to action completion — even a hung action
// (e.g. a CLI command that blocks on stdin) does not freeze
// subsequent clicks.
//
// Note: each handleClick invocation creates its OWN clickTracker.
// Sharing one debouncer across all items would drop the second
// click on a different item if the user clicked two items in
// quick succession (an obvious UX bug). The per-item tracker
// here matches what users expect from a native menu: a click on
// each item is evaluated independently.
func handleClick(item *systray.MenuItem, action func()) {
	var tracker clickTracker
	for range item.ClickedCh {
		if !tracker.allow(time.Now().UnixNano()) {
			continue
		}
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
