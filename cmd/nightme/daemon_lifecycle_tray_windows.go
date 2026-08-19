//go:build windows

// Windows production defaults for the systray click handlers
// wired up in tray.go.
//
// Windows has no SIGTERM (signal.Notify silently drops it — see
// cmd/nightme/signals_windows.go), and the daemon child runs
// DETACHED_PROCESS so there is no console to send a Ctrl+Break
// event to. The cleanest "stop the daemon" signal we have on
// Windows is os.Kill on self, which terminates the process
// without giving the runtime a graceful-shutdown window.
//
// We accept the abrupt exit on Windows because:
//   1. The daemon child has no console and no interactive
//      client — there is no UI state to save on shutdown.
//   2. agentsession.reapOrphan (the F-61 supervisor) cleans up
//      any leftover agent subprocesses on next nightme run /
//      start, so even if the daemon crashes mid-stop the agents
//      are not leaked.
//   3. signals_windows.go already documents that os.Kill is
//      what task manager / `taskkill /F` use to terminate the
//      daemon — we are doing the same thing from the inside.

package main

import (
	"os"
)

func onStopRequestDefault() {
	os.Exit(0)
}

func onRestartRequestDefault() {
	onStopRequestDefault()
}
