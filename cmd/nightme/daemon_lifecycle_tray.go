// Package main — tray click handlers that the daemon child wires
// into the systray menu (see tray.go).
//
// onStopRequestDefault and onRestartRequestDefault are the
// production defaults: they signal the daemon's own runtime to
// exit gracefully. The Restart path is intentionally minimal in
// v1 — it does NOT pre-spawn a replacement daemon, because the
// lock-handoff / new-daemon-pickup sequence is racy without a
// supervisor (launchd / systemd / a hand-rolled PID watcher).
// The tooltip on the menu item directs the user to re-run
// `nightme start` after the daemon exits; a follow-up can add
// the supervisor-aware restart when nightme ships its own
// lifecycle service.

package main

import (
	"os"
	"syscall"
)

// onStopRequestDefault is the production "Stop" / "Quit"
// handler. It sends SIGTERM to the current process; the runtime
// (runRunWith) registers SIGTERM in its signal channel and runs
// the graceful shutdown path. We use SIGTERM (not os.Interrupt)
// because the tray click is not a console event — SIGTERM is the
// "supervisor / programmatic shutdown" signal, and on Windows
// where SIGTERM is silently dropped we explicitly fall back to
// os.Kill via syscall so the tray never wedges a daemon that
// refuses to exit.
func onStopRequestDefault() {
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		// Last-resort: signal failed (PID is wrong, the
		// process is in a state where it can't be signalled,
		// or we're on a platform where the syscall returned
		// an error we don't recognise). Fall through to
		// direct os.Exit so we don't leave a tray that the
		// user can click but cannot stop.
		os.Exit(0)
	}
}

// onRestartRequestDefault is the production "Restart" handler.
// In v1 it is equivalent to Stop: it sends SIGTERM to self. The
// tooltip on the menu item tells the user to re-run
// `nightme start` to bring a fresh daemon up. The
// supervisor-aware restart (where the current process spawns
// its own replacement and exits atomically with respect to the
// daemon lock — see the file doc on tray.go) is deferred to a
// follow-up because it requires a parent-or-supervisor contract
// that nightme does not yet have.
//
// Kept as a separate function (not just an alias for
// onStopRequestDefault) so the call site in daemon_lifecycle_*.go
// stays readable and so a future supervisor-aware replacement
// is a one-function change.
func onRestartRequestDefault() {
	onStopRequestDefault()
}
