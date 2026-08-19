//go:build windows

package proc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// CreateNoWindow is the documented Win32 CREATE_NO_WINDOW
// constant
// (https://learn.microsoft.com/en-us/windows/win32/procthread/process-creation-flags).
// It tells CreateProcess not to allocate a console for the
// child. Without this flag, every spawn of a Windows console
// binary (cmd.exe wrapping a .cmd shim, the .exe agent binary
// itself, node.exe, powershell.exe, git.exe, gh.exe, …) opens
// a new console window on the user's desktop — visible as a
// flashing black rectangle in the taskbar, one per spawn.
//
// Exported so callers that build *syscall.SysProcAttr outside
// of proc.New (today: internal/bridge/pty/pty.go via
// gopty.Cmd, which is a sibling type to *exec.Cmd — same
// SysProcAttr field but no embedding, and routes through
// go-pty's own spawn path) can apply the same flag via
// HideWindow and stay consistent with what proc.New does for
// the rest of the codebase.
//
// On non-Windows platforms CreateNoWindow is a build-time
// placeholder; HideWindow is a no-op there.
const CreateNoWindow = 0x08000000

// proc.New is the SOLE spawn recipe in nightme on Windows.
// Every *exec.Cmd that nightme hands to Start / Run / Output
// must come from this helper; direct os/exec.Command[Context]
// calls in production code are forbidden because they bypass
// CREATE_NO_WINDOW and the resulting child pops a visible
// console window (the "flashing black rectangle" symptom).
//
// Routing matrix (mirrors the cmd-launcher rules; full
// rationale follows):
//
//	.exe / .com / no extension   → exec.CommandContext(resolved, args...)
//	.cmd / .bat (batch shim)     → exec.CommandContext(cmd.exe, /d /c, <resolved>, args...)
//	.ps1 (PowerShell script)     → exec.CommandContext(powershell.exe, -NoProfile, -NonInteractive, -ExecutionPolicy Bypass, -File <resolved>, args...)
//	.js  (Node.js script)        → exec.CommandContext(node.exe, <resolved>, args...)
//
// Each returned *exec.Cmd has CREATE_NO_WINDOW baked into its
// SysProcAttr at construction time (see launchOnWindows), so
// the child never allocates a visible console — nightme talks
// to it via stdin / stdout / stderr pipes, no UI surface
// needed.
//
// Why this matters: every Windows install where an agent
// binary is shipped as a Node-style shim (pi-node's
// current\pi.cmd, npm's opencode.cmd, the
// @anthropic-ai/claude-code.cmd npm package, etc.) fails to
// spawn via plain exec.Command — CreateProcess rejects a
// .cmd/.bat path as lpApplicationName with
// ERROR_INVALID_PARAMETER (87) and returns
// "fork/exec <path>.cmd: The parameter is incorrect."
//
// Without wrapping, every Windows install whose binary is a
// batch shim is wedged. The wrap mirrors Microsoft's
// documented workaround:
//
//	lpApplicationName = C:\Windows\System32\cmd.exe
//	lpCommandLine     = cmd /d /c "<resolved>" <args...>
//
// /d skips AutoRun registry commands, matching cmd.exe's
// interactive default; /c runs the command and exits.
//
// We do the LookPath ourselves (mirroring exec.Command's
// internal behaviour) so we can inspect the resolved
// extension before CreateProcess sees it. The original name
// is preserved for the non-shim branches so PATH resolution
// still applies.
//
// Windows has no Setsid equivalent and no controlling-TTY
// inheritance problem (Windows console handles are passed via
// explicit STARTUPINFO handles, not inherited fds). The
// bridge on Windows still works for single-pid Process.Signal
// / Kill, because SignalProcessGroup's Windows fallback
// already collapses to single-pid semantics.
func New(ctx context.Context, name string, args ...string) *exec.Cmd {
	resolved := name
	if lp, err := exec.LookPath(name); err == nil {
		resolved = lp
	}
	return launchOnWindows(ctx, resolved, args...)
}

// launchOnWindows picks the right exec.Cmd shape for the
// resolved target AND applies CREATE_NO_WINDOW before
// returning. Split out from proc.New so the routing is
// table-driven and individually unit-testable. See the file
// doc comment on proc.New for the matrix.
//
// resolved is the absolute path returned by exec.LookPath (or
// the original name if LookPath failed). args are appended
// verbatim after the interpreter / /d /c / -File marker.
//
// Note: when resolved has no extension (or an unknown one) we
// fall through to exec.CommandContext(resolved, args...) —
// the underlying CreateProcess will then either succeed
// (native PE) or fail with the same ERROR_INVALID_PARAMETER
// as before. That matches Linux/macOS semantics where the OS
// figures out the interpreter; on Windows that's not safe
// for .cmd/.bat/.ps1, which is exactly the gap this whole
// file exists to close.
//
// The returned *exec.Cmd ALWAYS has CreateNoWindow on its
// SysProcAttr.CreationFlags — there is no separate "set
// after" step that a caller could forget. PTY users (which
// can't route through exec.CommandContext because go-pty owns
// the cmd) call HideWindow directly with their SysProcAttr to
// apply the same flag with the same merge semantics.
func launchOnWindows(ctx context.Context, resolved string, args ...string) *exec.Cmd {
	var cmd *exec.Cmd
	switch strings.ToLower(filepath.Ext(resolved)) {
	case ".cmd", ".bat":
		cmd = exec.CommandContext(ctx, ComSpecOrDefault(),
			append([]string{"/d", "/c", resolved}, args...)...)
	case ".ps1":
		cmd = exec.CommandContext(ctx, "powershell.exe",
			append([]string{
				"-NoProfile", "-NonInteractive",
				"-ExecutionPolicy", "Bypass",
				"-File", resolved,
			}, args...)...)
	case ".js":
		cmd = exec.CommandContext(ctx, "node.exe",
			append([]string{resolved}, args...)...)
	default:
		// .exe, .com, or no extension — direct invocation. We
		// pass the resolved path (not the original `name`) so
		// the kernel sees an absolute path and skips its own
		// PATH search.
		cmd = exec.CommandContext(ctx, resolved, args...)
	}
	applyHideWindow(cmd)
	return cmd
}

// HideWindow sets CreateNoWindow on the supplied SysProcAttr
// and returns it (a fresh struct is allocated when attr is
// nil, so callers can chain the assignment:
// `cmd.SysProcAttr = proc.HideWindow(cmd.SysProcAttr)`).
// An existing non-nil attr is preserved — CreateNoWindow
// is merged with any caller-provided flags rather than
// replacing them.
//
// The signature takes *syscall.SysProcAttr (the underlying
// field type) rather than *exec.Cmd because the PTY path
// (internal/bridge/pty/pty.go) uses go-pty's gopty.Cmd, which
// is a sibling type to *exec.Cmd — same SysProcAttr field
// shape, but a different concrete type (no embedding). By
// keying on the field type the helper works for both shapes
// and stays platform-agnostic (the Unix build of HideWindow
// is a no-op).
//
// Production code that owns a plain *exec.Cmd should route
// through proc.New instead — proc.New applies this flag at
// construction time. HideWindow exists for the PTY escape
// hatch (gopty.Cmd is built via ptmx.Command, not
// exec.CommandContext, so proc.New can't apply it for them) and
// for tests that need to pin the merge behaviour in isolation.
func HideWindow(attr *syscall.SysProcAttr) *syscall.SysProcAttr {
	if attr == nil {
		attr = &syscall.SysProcAttr{}
	}
	attr.CreationFlags |= CreateNoWindow
	return attr
}

// applyHideWindow is the shared implementation behind proc.New.
// Mirrors HideWindow but applies it to a *exec.Cmd in-place.
// Kept unexported so the public surface is "proc.New" +
// "HideWindow".
func applyHideWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = HideWindow(cmd.SysProcAttr)
}

// ComSpecOrDefault returns %ComSpec% (set on every standard
// Windows install) with an explicit fallback for the rare
// case where the user cleared it. Exported because callers
// outside the proc package (notably internal/tray/openrepl
// which spawns a new console window for the REPL from the
// tray) need the same resolution rule that the daemon
// spawn recipe uses, and a hand-rolled copy in openrepl
// would drift the moment either side changed its mind about
// the fallback path.
func ComSpecOrDefault() string {
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