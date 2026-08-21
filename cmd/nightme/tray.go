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
//  2. The subcommand section is hand-grouped into Sessions /
//     Setup submenus (with Logs and Doctor sitting at the top
//     as their own rows, since they're the commands users reach
//     for most often after Console). The cliTitles used as
//     argv to proc.OpenTerminal match the cobra subcommand
//     names that cmdRegistry registers, so adding a new
//     reg.add() cobra command is a prerequisite for surfacing
//     it in the tray — but the grouping itself is owned here.
//     Lifecycle commands (start/stop/restart/status) and
//     TTY-bound commands (test/login/logs) remain
//     addNoTray so they don't
//     drift into the submenu list. (config and update were
//     briefly addNoTray too, but they're now reg.add() so the
//     REPL banner reflects the full user-facing surface.)
//
//  3. Click actions must not block the tray event loop. Each
//     click handler runs in systray's event goroutine; a handler
//     that calls into the daemon synchronously (e.g. daemoncontrol.Stop)
//     would freeze the menu until the call returns. The pattern
//     is: handler spawns a goroutine and returns immediately.
//
//  4. On headless systems the systray native init is unreliable
//     (GTK cannot open DISPLAY). runTrayOwning detects this via
//     isHeadless() (tray_headless_*.go) and skips the systray
//     branch entirely, falling through to the plain runtime
//     loop. recover() around systray.Run catches the
//     not-caught-by-detection cases (X server dies mid-run,
//     partial init panic) so the daemon survives a tray crash.
//
// Menu layout:
//
//   🟢 NightMe v<ver> is running (disabled info row)
//   ─────────
//   >_  Console                  → proc.OpenTerminal() (spawn REPL)
//   ─────────
//   Logs >         view           → proc.OpenTerminal("logs")
//                  clean          → proc.OpenTerminal("clean")
//   Sessions >     list           → proc.OpenTerminal("list")
//                  kill           → proc.OpenTerminal("kill")
//   Config                       → proc.OpenTerminal("config")
//   About >       agents          → proc.OpenTerminal("agents")
//                  name           → proc.OpenTerminal("name")
//                  doctor         → proc.OpenTerminal("doctor")
//   ─────────
//   ⓘ  Download Update           (hidden unless a newer release exists)
//   ↻  Restart                   → caller callback (respawn _daemon)
//   ⏻  Quit                      → caller callback (graceful daemon stop)
//
// The terminal-bound one-shots (Logs, Doctor) sit at the top of
// the second cluster; the deeper "configure / manage" actions
// live under Sessions / Setup submenus. The earlier flat
// layout exposed them all under a disabled "Commands" parent, which
// on Windows cascades MF_GRAYED to every child and made them appear
// unclickable. Submenus are clickable regardless of platform.

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/getlantern/systray"
	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/proc"
	"github.com/cnlangzi/nightme/internal/runtime"
	"github.com/cnlangzi/nightme/internal/version"
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
//
// Headless short-circuit (Linux only — see tray_headless_*.go):
// when no GUI session is detected, runTrayOwning skips the
// systray branch entirely and just runs the runtime loop on
// this thread. CLI / REPL control of the daemon still works
// (daemoncontrol IPC runs over the Unix socket).
func runTrayOwning(cmd *cobra.Command, runDeps runtime.Deps, opts trayOptions) error {
	// Strict scheme: when we can't be sure a GUI session is
	// reachable, skip the tray rather than crash inside GTK.
	if isHeadless() {
		if opts.logger != nil {
			opts.logger.Info("system tray disabled (no GUI session detected); running runtime loop directly",
				"xdg_session_type", os.Getenv("XDG_SESSION_TYPE"),
				"display", os.Getenv("DISPLAY"),
				"wayland_display", os.Getenv("WAYLAND_DISPLAY"))
		}
		return runRunWith(cmd, runDeps)
	}

	runErrCh := make(chan error, 1)
	runtimeDone := make(chan struct{})
	go func() {
		runErrCh <- runRunWith(cmd, runDeps)
		close(runtimeDone)
	}()

	// Watcher goroutine: when the runtime returns, release
	// the systray native loop. We watch a SEPARATE
	// runtimeDone signal (closed after the runErrCh send) so
	// the watcher does NOT consume the runErrCh value — the
	// calling thread reads it via `return <-runErrCh` below.
	// The previous shape read runErrCh directly here, which
	// raced the main return: on platforms where systray.Run
	// blocks (Windows / macOS), the watcher always won the
	// single buffered value, leaving `return <-runErrCh`
	// blocked forever — the daemon shutdown completed but
	// the process never exited, so `nightme stop` timed out
	// at 15s with the lock still held.
	go func() {
		<-runtimeDone
		systray.Quit()
	}()

	// Recover fallback for the not-caught-by-detection cases:
	// getlantern/systray's native init can panic in odd
	// situations (X server dies mid-run, partial GTK init,
	// Wayland compositor protocol mismatch). The detection
	// above is conservative but cannot cover every runtime
	// failure mode. If a panic does escape, log it and let
	// the daemon continue running without the tray UI —
	// runtime lives in its own goroutine, so status / stop /
	// restart still work over daemoncontrol's socket.
	//
	// Scope: recover only catches panics on the calling
	// goroutine's call stack. systray's CGo callbacks run in
	// goroutines the library itself spawns; those panics
	// cannot be intercepted here. The native-init panic that
	// is the original failure mode DOES propagate through
	// this defer.
	defer func() {
		if r := recover(); r != nil {
			// debug.Stack preserves the goroutine's full
			// stack at the panic site. systray/GTK/Wayland
			// crashes are often CGo errors that surface
			// as opaque panic values; the stack is the
			// only signal telling us which native frame
			// actually blew up. Includes all goroutines
			// (debug.Stack docs), so any helper goroutine
			// state is captured too.
			stack := string(debug.Stack())
			if opts.logger != nil {
				opts.logger.Warn("system tray crashed; daemon continues without tray UI",
					"panic", fmt.Sprint(r),
					"stack", stack)
			}
			// systray.Run has returned (the panic reached
			// this defer). Runtime is still alive in its
			// own goroutine; fall through to the wait
			// below for the runtime's exit code.
		}
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
// Layout (matches the file-level Menu layout block):
//
//	🟢 NightMe is running        (disabled info row)
//	─────────
//	>_  Console                  (primary action)
//	─────────
//	Logs / Sessions > / Setup > / About >
//	─────────
//	ⓘ  Download Update (hidden until newer release exists)
//	↻  Restart / ⏻  Quit
//
// The subcommand section is grouped into Sessions / Setup / About
// submenus rather than rendered as a flat list — see the file
// header for the rationale (Windows MF_GRAYED cascade).
func trayOnReady(opts trayOptions) {
	systray.SetTooltip("NightMe daemon — click to open the menu")
	applyIcon()

	// Status row. \U0001F7E2 is the green-circle emoji — the
	// row stays disabled so it doesn't fire a handler, but
	// the emoji glyph itself renders green regardless of the
	// row's enabled state because it's a coloured codepoint.
	//
	// Format: 🟢 NightMe[<name>] v<version> is running.
	// - `<name>` is the daemon's configured instance name
	//   (cfg.Name) with hostname fallback via
	//   config.EffectiveName, so users with multiple daemons
	//   can tell them apart at a glance.
	// - `<version>` is the compiled-in version.Version,
	//   same string the REPL banner and `nightme version`
	//   print. Showing it here means the user never needs to
	//   dig into a submenu to learn which build is live.
	instanceName := "unknown"
	if cfg, err := config.LoadDefault(); err == nil && cfg != nil {
		instanceName = config.EffectiveName(cfg)
	}
	statusItem := systray.AddMenuItem(
		"\U0001F7E2  NightMe[" + instanceName + "] v" + version.Version + " is running",
		"nightme daemon is running",
	)
	statusItem.Disable()

	systray.AddSeparator()

	// Primary actions — no icons. Plain text keeps them
	// visually quieter than the lifecycle cluster below, so
	// the eye lands on "Open Terminal" / "View Logs" first
	// without competing with the icon-heavy bottom group.
	openREPL := systray.AddMenuItem(">_  Console", "Spawn a new terminal running the nightme REPL")

	go handleClick(openREPL, func() {
		if err := proc.OpenTerminal(context.Background(), "nightme"); err != nil {
			logClickErr(opts.logger, "open-repl", err)
		}
	})

	systray.AddSeparator()

	// Submenus + Config. Three of the four items in this cluster
	// are submenus so the parent item carries the verb and each
	// sub-item carries the noun (Logs > view / clean); Config
	// stays a flat top-level item because it has no peer actions
	// to group with. Items inside each submenu spawn a maximized,
		// focused Terminal.app tab via proc.OpenTerminal — the
	// same path as Console above, so every command gets a real
		// TTY and visible output.
	logsMenu := systray.AddMenuItem("Logs", "")
	addTerminalSubItem(logsMenu, opts, "▸ logs: \ttail daemon log", "logs", "")
	addTerminalSubItem(logsMenu, opts, "▸ clean: \ttruncate logs + attachments", "clean", "")

	sessions := systray.AddMenuItem("Sessions", "")
	addTerminalSubItem(sessions, opts, "▸ list: \tshow active sessions", "list", "")
	addTerminalSubItem(sessions, opts, "▸ kill: \tterminate agent procs", "kill", "")

	configItem := systray.AddMenuItem("Config…", "")
	go handleClick(configItem, func() {
		if err := proc.OpenTerminal(context.Background(), "nightme", "config"); err != nil {
			logClickErr(opts.logger, "config", err)
		}
	})

	about := systray.AddMenuItem("About", "")
	addTerminalSubItem(about, opts, "▸ agents: \tmanage configured agents", "agents", "")
	addTerminalSubItem(about, opts, "▸ doctor: \tdiagnose the daemon", "doctor", "")

	systray.AddSeparator()

	// Icon-only cluster. These three carry icons because
	// they're the highest-stakes actions in the menu:
	// update (network), restart (daemon-lifecycle), quit
	// (terminating this very icon). Icons reinforce that the
	// user is in the "system control" zone.
	//
	// Docker Desktop renders SF Symbol-style monochrome
	// glyphs. getlantern/systray doesn't expose per-item
	// images, so we approximate with single-character
	// Unicode symbols — all the same nominal width as ⏻ so
	// the text column stays aligned across the three rows.
	//
	// Download Update starts hidden: the background version
	// check only shows it (along with the separator above)
	// when a newer version actually exists. Up-to-date
	// installations see just Restart + Quit in this cluster.
	updateItem := systray.AddMenuItem("ⓘ  Download Update", "Check for and install a newer nightme release")
	updateItem.Hide()
	go handleClick(updateItem, func() {
		if err := proc.OpenTerminal(context.Background(), "nightme", "update"); err != nil {
			logClickErr(opts.logger, "update", err)
		}
	})

	// Background version check. Hits nightme.dev (or the cache
	// populated by an earlier check within the last 24h), then
	// patches the menu item's title in place. Done off the
	// systray event loop thread so the click handler stays
	// snappy and a slow network never blocks trayOnReady.
	//
	// Title format mirrors Docker Desktop's badge style:
	//   - up-to-date:    ⓘ  Download Update    v0.1.0
	//   - newer exists:  ⓘ  Download Update    v0.1.0 → v0.2.0
	// The arrow uses U+2192 (rightwards arrow), not "->", so
	// the suffix reads as "version progression" rather than
	// shell syntax. We re-use the same Checker as the REPL
	// startup prompt so the 24h throttle cache is shared
	// between the two surfaces — opening the tray menu after a
	// REPL start shouldn't trigger a second second API hit.
	go decorateUpdateItem(updateItem)

	restart := systray.AddMenuItem("↻  Restart", "Restart the nightme daemon")
	go handleClick(restart, func() {
		if opts.onRestartRequest != nil {
			opts.onRestartRequest()
		}
	})

	quit := systray.AddMenuItem("⏻  Quit", "Stop the daemon and remove the tray icon")
	go handleClick(quit, func() {
		if opts.onStopRequest != nil {
			opts.onStopRequest()
		}
	})
}

// decorateUpdateItem runs a background version check and
// shows item only when a newer version exists, with title
// "<current> → <latest>". Up-to-date installations leave
// the item hidden — the menu simply omits the row. It is
// fire-and-forget: a network failure or unparseable
// response leaves the item hidden rather than failing
// the tray.
//
// Log level for the version-check error path is DEBUG,
// not WARN. A tray that logs a "version check: ..." WARN
// every time the user opens the menu while offline / behind
// a captive portal / during a CDN hiccup would be noisy
// without being actionable — the UI is already correct
// (item hidden, click still routes to the manual upgrade
// path). The REPL startup prompt uses a separate callback
// at WARN because that's a user-facing decision point;
// the tray is not.
//
// The check shares the 24h on-disk cache with the REPL
// startup prompt (internal/version.DefaultChecker reads
// cfg.Paths.DataDir/version-check.json), so a user who
// already answered the REPL prompt doesn't see the tray
// re-fetch the same data within the throttle window.
func decorateUpdateItem(item *systray.MenuItem) {
	dataDir := ""
	if cfg, err := config.LoadDefault(); err == nil && cfg != nil {
		dataDir = cfg.Paths.DataDir
	}
	checker, _ := version.DefaultChecker(dataDir)
	if checker == nil {
		// Misconfig (no data dir resolution path at all) is
		// silent: the tray surface is decorative, and the
		// user has bigger problems if the daemon can't
		// locate its own config. Don't add log noise.
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	res := checker.Check(ctx, version.Version, func(format string, args ...any) {
		// Transient network / parsing errors are DEBUG.
		// See the function comment for why this isn't
		// WARN — the UI is correct without the log.
		slog.Default().Debug(fmt.Sprintf("version check: "+format, args...))
	})

	if res.Latest == "" || !res.Outdated {
		// Up to date, or no usable result — leave the
		// item hidden. Earlier separator still renders;
		// that mirrors Docker Desktop, which also keeps
		// the bottom separator even when the cluster
		// above it has only one item.
		return
	}

	current := displayVer(version.Version)
	latest := displayVer(res.Latest)
	item.SetTitle("ⓘ  Download Update    " + current + "  →  " + latest)
	item.Show()
}

// addTerminalSubItem adds a child item under parent that opens a
// maximized, focused Terminal.app tab running `nightme <cliTitle>`
// when clicked. parent is a sub-menu item created via
// systray.AddMenuItem / MenuItem.AddSubMenuItem; cliTitle is the
// lowercase cobra subcommand name and must match exactly (used as
// argv[1] to proc.OpenTerminal). displayTitle is what the user sees
// in the menu — independent from cliTitle so we can capitalise here
// without breaking the CLI.
func addTerminalSubItem(parent *systray.MenuItem, opts trayOptions, displayTitle, cliTitle, tooltip string) {
	item := parent.AddSubMenuItem(displayTitle, tooltip)
	go handleClick(item, func() {
		if err := proc.OpenTerminal(context.Background(), "nightme", cliTitle); err != nil {
			logClickErr(opts.logger, "command:"+cliTitle, err)
		}
	})
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
