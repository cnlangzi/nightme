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
// onRestartRequestDefault spawns a detached `nightme restart`
// CLI child, which performs the full stop+start protocol via the
// daemon IPC socket. We CANNOT spawn a bare `_daemon` child
// directly: runDaemonChild requires NIGHTME_DAEMON_LOCK_FD and
// NIGHTME_READY_FD env vars plus the matching ExtraFiles wiring
// that only startDaemon sets up — see daemon_lifecycle_unix.go.
// Without them the child exits immediately with
// `_daemon must be launched by nightme start or nightme restart`,
// the new daemon never starts, and the tray restart silently
// degrades to a plain stop. (Pre fd-inheritance this happened to
// work because runDaemonChild had no env-var gate.)
//
// Why we delegate to `nightme restart` rather than call runRestart
// in-process: this code runs *inside* the daemon. runRestart
// would acquire lifecycleLock (fine — different lock) then call
// daemoncontrol.Stop on our own socket. The daemon's server
// receives that stop RPC and cancels its context (s.cancel() in
// server_unix.go's "stop" branch), which trips the runtime's
// shutdown path before startDaemon's fork-exec finishes — our
// goroutine gets reaped alongside the process. Spawning a
// sibling process keeps the restart work independent of our own
// lifecycle. proc.New applies Setsid to that spawn, which
// detaches the sibling from our controlling TTY (and is a
// belt-and-braces guarantee for the case of a signal sent to our
// process group, though the normal shutdown path doesn't send
// signals — it cancels our context).
//
// We do NOT call onStopRequestDefault afterwards: the spawned
// restart process handles the stop itself via daemoncontrol.Stop.
// Sending SIGTERM here too would race with the spawn (we might
// exit before the restart process can connect to the socket),
// and the daemoncontrol.Stop path triggers the same shutdown
// path anyway.

package main

import (
	"context"
	"os"
	"os/exec"
	"syscall"

	"github.com/cnlangzi/nightme/internal/proc"
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
	cmd := buildRestartCmd(context.Background(), exe)
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
	// shutdown waiting on its lifecycle. The spawned
	// `nightme restart` process is responsible for
	// daemoncontrol.Stop (which sends a stop RPC to our
	// unix socket; the server cancels our context, which
	// triggers the same graceful-shutdown path SIGTERM
	// would) and for the full startDaemon fork-exec.
	// See the file header for why we don't also call
	// onStopRequestDefault here.
	_ = cmd.Process.Release()
}

// buildRestartCmd constructs the `nightme restart` spawn that
// onRestartRequestDefault launches. Factored out so a unit test
// can assert the argv shape without actually fork-execing — the
// spawn point's first argument is the load-bearing detail
// (see file header for why a bare `_daemon` argv would silently
// regress to a no-op start).
func buildRestartCmd(ctx context.Context, exe string) *exec.Cmd {
	// Spawn through proc.New so the child gets the same
	// platform-specific spawn recipe (Setsid on Unix,
	// CREATE_NO_WINDOW on Windows) as the rest of the
	// codebase — no inconsistent detach / TTY inheritance
	// vs the daemon lifecycle commands.
	return proc.New(ctx, exe, "restart")
}
