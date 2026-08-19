//go:build !windows

// Unix production defaults for the systray click handlers wired
// up in tray.go.
//
// onStopRequestDefault sends SIGTERM to the current process; the
// runtime (runRunWith) registers SIGTERM in its signal channel
// and runs the graceful shutdown path. We use SIGTERM (not
// os.Interrupt) because the tray click is not a console event —
// SIGTERM is the "supervisor / programmatic shutdown" signal.
//
// onRestartRequestDefault is identical in v1: SIGTERM to self.
// The tooltip on the menu item tells the user to re-run `nightme
// start` to bring a fresh daemon up. The supervisor-aware restart
// is deferred to a follow-up.

package main

import (
	"os"
	"syscall"
)

func onStopRequestDefault() {
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		// Last-resort: signal failed (PID is wrong, or we're
		// on a POSIX where the syscall returned an error we
		// don't recognise). Fall through to direct os.Exit
		// so we don't leave a tray that the user can click
		// but cannot stop.
		os.Exit(0)
	}
}

func onRestartRequestDefault() {
	onStopRequestDefault()
}
