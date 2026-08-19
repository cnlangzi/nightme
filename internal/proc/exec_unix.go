//go:build !windows

package proc

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

// Options controls spawn behaviour. The zero value is NOT the
// safe default — HideWindow=false means "visible window". Use
// New() (which passes HideWindow=true) for the default hide-
// window behaviour; use NewVisible() when the child needs a
// visible console (the tray's terminal spawn path).
type Options struct {
	// HideWindow, when true, suppresses the child's console
	// window. On Unix this is a no-op (there is no "console
	// window" concept); on Windows it sets CREATE_NO_WINDOW.
	// The default for daemon-internal command execution is
	// true (hide) — every proc.New() caller gets this for
	// free. Set to false only for tray-spawned terminals that
	// the user needs to see.
	HideWindow bool
}

// New is the backward-compatible spawn recipe: always hides the
// window. Every existing caller (bridges, update, lifecycle,
// shell dispatch, gtw exec) routes through here and gets the
// same behaviour as before this Options refactor.
func New(ctx context.Context, name string, args ...string) *exec.Cmd {
	return NewWith(ctx, Options{HideWindow: true}, name, args...)
}

// NewVisible is the convenience wrapper for NewWith with
// HideWindow=false — the child gets a visible console window.
// Used by the tray (internal/tray/openrepl) to spawn terminal
// windows that the user needs to interact with.
func NewVisible(ctx context.Context, name string, args ...string) *exec.Cmd {
	return NewWith(ctx, Options{HideWindow: false}, name, args...)
}

// NewWith is the configurable spawn recipe. See Options for the
// available knobs. New() and NewVisible() are convenience
// wrappers; prefer them unless you need per-call control.
//
// On Unix, HideWindow is a no-op — the child always gets
// Setsid: true so it detaches from the daemon's controlling TTY
// (F-54 / stop hang investigation: pi / claude / codex / opencode
// / dsh all block on a TTY-driven kqueue if the TTY fd is
// inherited). With Setsid, the child becomes the leader of its
// own session AND process group, so callers can later broadcast
// SIGINT to the whole subtree via SignalProcessGroup.
func NewWith(ctx context.Context, _ Options, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}

// HideWindow is a no-op on non-Windows platforms. The Windows
// version (exec_windows.go) sets CreateNoWindow on the supplied
// SysProcAttr so the child runs without a console window.
// PTY-backed spawners (internal/bridge/pty/pty.go) call this
// unconditionally so they don't need per-platform build tags
// for the hide-window step — the platform-specific logic lives
// in proc.HideWindow.
func HideWindow(attr *syscall.SysProcAttr) *syscall.SysProcAttr {
	return attr
}
