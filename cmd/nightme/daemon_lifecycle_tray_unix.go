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
// onRestartRequestDefault spawns a fresh `_daemon` child from
// the same binary, then signals itself to terminate. The child
//// picks up the same config and the new daemon inherits the
//// existing daemon.sock after we release daemon.lock on
// shutdown. We can't use the lifecycle-lock-protected
// runRestart path here (this code runs *inside* the daemon
// and can't acquire the same lock without a separate process),
// so the race is bounded by the SIGTERM -> graceful-shutdown
// delay (a few hundred ms in practice) — the new child
// blocks on daemon.lock until then.

package main

import (
	"os"
	"os/exec"
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
	exe, err := os.Executable()
	if err != nil {
		// Couldn't find our own binary — fall back to a
		// plain stop so the tray never silently no-ops.
		onStopRequestDefault()
		return
	}
	cmd := exec.Command(exe, "_daemon")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		// Spawn failed (no exec permission, etc.) — same
		// fallback: stop, don't leave the user clicking
		// into a void.
		onStopRequestDefault()
		return
	}
	// Detach from the child so we don't block our own
	// shutdown waiting on its lifecycle. The child reads
	// the same config and re-acquires daemon.lock after
	// we release it on SIGTERM exit.
	_ = cmd.Process.Release()
	onStopRequestDefault()
}
