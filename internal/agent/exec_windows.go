//go:build windows

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// CREATE_NO_WINDOW tells CreateProcess not to allocate a console for
// the child. Without this flag, every spawn of a Windows console
// binary (cmd.exe wrapping a .cmd shim, the .exe agent binary
// itself, node.exe, powershell.exe, …) opens a new console window
// on the user's desktop — visible as a flashing black rectangle in
// the taskbar, one per agent spawn. With the flag set, the child
// runs silently and inherits only the stdin / stdout / stderr pipes
// the bridge wires up; no visible window.
//
// Value is the documented Win32 constant
// (https://learn.microsoft.com/en-us/windows/win32/procthread/process-creation-flags).
const createNoWindow = 0x08000000

// NewCmd is the bridge spawn recipe in one place on Windows. It
// returns an *exec.Cmd wired so the child process starts cleanly
// regardless of how the target binary is installed:
//
//	.exe / .com / no extension   → exec.CommandContext(name, args...) directly
//	.cmd / .bat (batch shim)     → exec.CommandContext(cmd.exe, /d /c, <resolved>, args...)
//	.ps1 (PowerShell script)     → exec.CommandContext(powershell.exe, -NoProfile, -NonInteractive, -File, <resolved>, args...)
//	.js  (Node.js script)        → exec.CommandContext(node.exe, <resolved>, args...)
//
// Every returned *exec.Cmd has CREATE_NO_WINDOW set on its
// SysProcAttr so the child never allocates a visible console —
// nightme talks to it via stdin / stdout / stderr pipes, no UI
// surface needed.
//
// Why this matters: every Windows install where an agent binary is
// shipped as a Node-style shim (pi-node's current\pi.cmd,
// npm's opencode.cmd, the @anthropic-ai/claude-code.cmd npm
// package, etc.) fails to spawn via plain exec.Command —
// CreateProcess rejects a .cmd/.bat path as lpApplicationName with
// ERROR_INVALID_PARAMETER (87) and returns
// "fork/exec <path>.cmd: The parameter is incorrect."
//
// Without wrapping, every Windows install whose binary is a batch
// shim is wedged. The wrap mirrors Microsoft's documented
// workaround:
//
//	lpApplicationName = C:\Windows\System32\cmd.exe
//	lpCommandLine     = cmd /d /c "<resolved>" <args...>
//
// /d skips AutoRun registry commands, matching cmd.exe's
// interactive default; /c runs the command and exits.
//
// We do the LookPath ourselves (mirroring exec.Command's
// internal behaviour) so we can inspect the resolved extension
// before CreateProcess sees it. The original name is preserved
// for the non-shim branches so PATH resolution still applies.
//
// Windows has no Setsid equivalent and no controlling-TTY
// inheritance problem (Windows console handles are passed via
// explicit STARTUPINFO handles, not inherited fds). The bridge
// on Windows still works for single-pid Process.Signal / Kill,
// because SignalProcessGroup's Windows fallback already collapses
// to single-pid semantics.
func NewCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	resolved := name
	if lp, err := exec.LookPath(name); err == nil {
		resolved = lp
	}
	cmd := launchOnWindows(ctx, resolved, args...)
	hideWindow(cmd)
	return cmd
}

// launchOnWindows picks the right exec.Cmd shape for the
// resolved target. Split out from NewCmd so the routing is
// table-driven and individually unit-testable. See the file
// doc comment on NewCmd for the matrix.
//
// resolved is the absolute path returned by exec.LookPath (or
// the original name if LookPath failed). args are appended
// verbatim after the interpreter / /d /c / -File marker.
//
// Note: when resolved has no extension (or an unknown one) we
// fall through to exec.CommandContext(resolved, args...) — the
// underlying CreateProcess will then either succeed (native PE)
// or fail with the same ERROR_INVALID_PARAMETER as before. That
// matches Linux/macOS semantics where the OS figures out the
// interpreter; on Windows that's not safe for .cmd/.bat/.ps1,
// which is exactly the gap this whole file exists to close.
func launchOnWindows(ctx context.Context, resolved string, args ...string) *exec.Cmd {
	switch strings.ToLower(filepath.Ext(resolved)) {
	case ".cmd", ".bat":
		return exec.CommandContext(ctx, comspecOrDefault(),
			append([]string{"/d", "/c", resolved}, args...)...)
	case ".ps1":
		return exec.CommandContext(ctx, "powershell.exe",
			append([]string{
				"-NoProfile", "-NonInteractive",
				"-ExecutionPolicy", "Bypass",
				"-File", resolved,
			}, args...)...)
	case ".js":
		return exec.CommandContext(ctx, "node.exe",
			append([]string{resolved}, args...)...)
	}
	// .exe, .com, or no extension — direct invocation. We pass
	// the resolved path (not the original `name`) so the kernel
	// sees an absolute path and skips its own PATH search.
	return exec.CommandContext(ctx, resolved, args...)
}

// hideWindow sets CREATE_NO_WINDOW on cmd so the child runs
// without a console window. Idempotent: a nil SysProcAttr is
// allocated; an existing non-nil SysProcAttr is preserved
// (CREATE_NO_WINDOW is merged with any caller-provided flags).
//
// Split out so tests can pin it without re-checking the launch
// matrix.
func hideWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}

// comspecOrDefault returns %ComSpec% (set on every standard
// Windows install) with an explicit fallback for the rare case
// where the user cleared it.
func comspecOrDefault() string {
	if c := os.Getenv("ComSpec"); c != "" {
		return c
	}
	return `C:\Windows\System32\cmd.exe`
}

// isWindowsBatchExt reports whether ext (the result of
// filepath.Ext, with the leading dot) names a Windows batch
// extension that CreateProcess can't run as lpApplicationName.
// We case-fold because NTFS preserves case but Windows path
// comparisons are case-insensitive.
//
// Retained as a package-level helper because the exec_windows
// unit tests assert on it; the production code uses the
// switch in launchOnWindows directly.
func isWindowsBatchExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".cmd", ".bat":
		return true
	}
	return false
}
