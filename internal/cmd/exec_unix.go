//go:build !windows

package agent

import (
	"context"
	"os/exec"
	"syscall"
)

// CreateNoWindow is a build-time placeholder on non-Windows
// platforms. The Windows version (exec_windows.go) is the
// real Win32 CREATE_NO_WINDOW constant; on Unix there is no
// "console window" concept so this is just a typed 0 — code
// that references it cross-builds without #ifdef noise, and
// any platform-specific SysProcAttr bit it would have been
// OR'd into is irrelevant on Unix.
const CreateNoWindow = 0

// NewCmd is the bridge spawn recipe in one place. It returns an
// exec.Cmd wired with the platform-specific SysProcAttr that
// detaches the child from the daemon's controlling TTY (Setsid)
// so /dev/tty inheritance cannot wedge the cli event loop on
// macOS / Linux (F-54 / stop hang investigation: pi / claude /
// codex / opencode all block on a TTY-driven kqueue if the TTY
// fd is inherited).
//
// With Setsid, the child becomes the leader of its own session
// AND process group. That makes the cli the pgid, so callers
// can later broadcast SIGINT to the whole subtree via
// SignalProcessGroup(d.cmd.Process, syscall.SIGINT) — the same
// effect Ctrl-C in a TTY has on the foreground process group.
//
// All four bridge spawn sites (claudecode / pi / codex /
// opencode) call this helper so the platform-specific knob stays
// in one place; any future spawn hardening (e.g. CLOSE_ON_EXEC
// on inherited fds) lands here, not in the per-bridge newSession
// functions.
func NewCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}

// HideWindow is a no-op on non-Windows platforms. The Windows
// version (exec_windows.go) sets CREATE_NO_WINDOW on the
// supplied SysProcAttr so the child runs without a console
// window. PTY-backed spawners (internal/bridge/pty/pty.go)
// call this unconditionally so they don't need per-platform
// build tags for the hide-window step — the platform-
// specific logic is in agent.HideWindow.
func HideWindow(attr *syscall.SysProcAttr) *syscall.SysProcAttr {
	return attr
}
