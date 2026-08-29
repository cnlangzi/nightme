//go:build windows

package proc

import (
	"context"
	"fmt"
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

// Options controls spawn behaviour. The zero value is NOT the
// safe default — HideWindow=false means "don't hide". Use
// New() (which passes HideWindow=true) for the default hide-
// window behaviour. Set to false only when you explicitly need
// the child to inherit a visible console — note that on a
// detached daemon (no parent console) this still produces NO
// visible window; for a guaranteed-visible terminal window use
// OpenTerminal instead.
type Options struct {
	// HideWindow, when true, sets CREATE_NO_WINDOW on the
	// child's SysProcAttr so it never allocates a visible
	// console. The default for daemon-internal command
	// execution is true (hide) — every proc.New() caller gets
	// this for free.
	HideWindow bool
}

// New is the backward-compatible spawn recipe: always hides the
// window. Every existing caller (bridges, update, lifecycle,
// shell dispatch, gtw exec) routes through here and gets the
// same behaviour as before this Options refactor.
func New(ctx context.Context, name string, args ...string) *exec.Cmd {
	return NewWith(ctx, Options{HideWindow: true}, name, args...)
}

// NewWith is the configurable spawn recipe. See Options for the
// available knobs. proc.New is the SOLE spawn recipe in
// nightme on Windows — every *exec.Cmd that nightme hands to
// Start / Run / Output must come from here or its wrappers;
// direct os/exec.Command[Context] calls in production code are
// forbidden because they bypass CREATE_NO_WINDOW and the
// resulting child pops a visible console window (the "flashing
// black rectangle" symptom).
//
// Routing matrix (mirrors the cmd-launcher rules; full
// rationale follows):
//
//	.exe / .com / no extension   → exec.CommandContext(resolved, args...)
//	.cmd / .bat (batch shim)     → exec.CommandContext(cmd.exe, /d /c, <resolved>, args...)
//	.ps1 (PowerShell script)     → exec.CommandContext(powershell.exe, -NoProfile, -NonInteractive, -ExecutionPolicy Bypass, -File <resolved>, args...)
//	.js  (Node.js script)        → exec.CommandContext(node.exe, <resolved>, args...)
//
// When opts.HideWindow is true (the default via New), each
// returned *exec.Cmd has CREATE_NO_WINDOW baked into its
// SysProcAttr at construction time (see launchOnWindowsWith),
// so the child never allocates a visible console. When false,
// CREATE_NO_WINDOW is skipped — but note the daemon itself is
// a detached background process with no console, so a child
// built with HideWindow=false will NOT get a visible window
// either (there is no parent console to inherit). For a
// guaranteed-visible terminal window use OpenTerminal.
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
func NewWith(ctx context.Context, opts Options, name string, args ...string) *exec.Cmd {
	resolved := name
	if lp, err := exec.LookPath(name); err == nil {
		resolved = lp
	}
	cmd := launchOnWindowsWith(ctx, opts, resolved, args...)
	armGraceCancel(cmd)
	return cmd
}

// armGraceCancel is the Windows counterpart to
// exec_unix.go:armGraceCancel. It exists so that cmd.Cancel
// has the same shape on every platform — callers (and tests)
// can rely on it being non-nil after proc.NewWith — but the
// implementation is fundamentally different.
//
// Why we can't fix the .git/index.lock stale-lock issue the
// way Unix does:
//
//	Unix    — child receives SIGTERM, runs its signal handler,
//	          unlinks `.git/index.lock`, exits 0.
//	Windows — there is no polite-signal mechanism for console-
//	          less children. The only kill primitives are
//	          TerminateProcess (no cleanup) and Job Object
//	          + GenerateConsoleCtrlEvent (requires a console
//	          attached to the child, which nightme's
//	          CREATE_NO_WINDOW children don't have).
//
// What armGraceCancel can still do on Windows — a marginal
// improvement for git children that are *almost done* when
// cancel fires:
//
//   - Set cmd.WaitDelay = SIGTERMGrace. After ctx-fire, stdlib
//     waits up to SIGTERMGrace (1 s) for the child to exit
//     naturally before hard-killing it. A git child that's
//     finishing its `git status` at the moment of cancel can
//     complete + drop .git/index.lock in that window.
//   - Override cmd.Cancel to return os.ErrProcessDone so
//     stdlib doesn't replace Run()'s real exit status with
//     ctx.Err().
//
// For a git child that's actively mid-`git add`/`git commit`
// when cancel fires, this does NOT help — git on Windows in
// console-less mode has no signal handler, so TerminateProcess
// still leaves `.git/index.lock` behind. Real Windows grace
// needs Job Object + CREATE_NEW_CONSOLE +
// GenerateConsoleCtrlEvent — substantially more code; see
// docs/feat/F-CLAUDE-PRINT-002 §Windows caveat for the full
// design and the v4 follow-up plan.
func armGraceCancel(cmd *exec.Cmd) {
	cmd.WaitDelay = SIGTERMGrace
	cmd.Cancel = func() error { return os.ErrProcessDone }
}

// launchOnWindows is the backward-compatible wrapper around
// launchOnWindowsWith that always hides the window. Retained
// so existing unit tests (which call launchOnWindows directly
// and assert CREATE_NO_WINDOW is set) don't need a signature
// change.
func launchOnWindows(ctx context.Context, resolved string, args ...string) *exec.Cmd {
	return launchOnWindowsWith(ctx, Options{HideWindow: true}, resolved, args...)
}

// launchOnWindowsWith picks the right exec.Cmd shape for the
// resolved target AND conditionally applies CREATE_NO_WINDOW
// before returning. Split out from NewWith so the routing is
// table-driven and individually unit-testable. See the file
// doc comment on NewWith for the matrix.
//
// resolved is the absolute path returned by exec.LookPath (or
// the original name if LookPath failed). args are appended
// verbatim after the interpreter / /d /c / -File marker.
//
// When opts.HideWindow is true, the returned *exec.Cmd has
// CreateNoWindow on its SysProcAttr.CreationFlags. When false,
// SysProcAttr is left nil so the child gets a visible console.
//
// Note: when resolved has no extension (or an unknown one) we
// fall through to exec.CommandContext(resolved, args...) —
// the underlying CreateProcess will then either succeed
// (native PE) or fail with the same ERROR_INVALID_PARAMETER
// as before. That matches Linux/macOS semantics where the OS
// figures out the interpreter; on Windows that's not safe
// for .cmd/.bat/.ps1, which is exactly the gap this whole
// file exists to close.
func launchOnWindowsWith(ctx context.Context, opts Options, resolved string, args ...string) *exec.Cmd {
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
	if opts.HideWindow {
		applyHideWindow(cmd)
	}
	return cmd
}

// SetCloseOnExec is a no-op on Windows. Handle inheritance is
// opt-in there — CreateProcess only passes the handles Go
// explicitly lists — so there is no inherited-descriptor leak
// to disarm. See the Unix implementation (exec_unix.go) for the
// flock-lifetime problem this solves on POSIX.
func SetCloseOnExec(_ *os.File) error {
	return nil
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
// outside the proc package need the same resolution rule that
// the daemon spawn recipe uses, and a hand-rolled copy would
// drift the moment either side changed its mind about the
// fallback path.
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

// OpenTerminal spawns a NEW visible console window that runs
// `name args...` and keeps it open after the command exits
// (cmd /k). Unlike New/NewWith — which build a child that
// inherits (or not) the daemon's absent console — OpenTerminal
// uses `cmd /c start "title" cmd /k <name> [args]` to
// allocate a fresh console for the child, so the user
// always sees a window regardless of whether the daemon has
// one.
//
// Fire-and-forget: the helper reaps the short-lived `cmd /c
// start` helper in a goroutine and returns nil on successful
// launch; the visible window is its own process whose lifetime
// is independent of the caller. Use this for tray menu clicks
// that need to show the user a terminal (REPL, logs tail,
// interactive subcommands).
//
// Path resolution: bin is taken from os.Executable() (the
// absolute path of the running nightme binary), NOT from
// exec.LookPath(name). The tray is only ever clicked by the
// daemon child, which IS nightme — so the running binary IS
// the one the user wants the new window to invoke. This
// sidesteps the `go install` / scoop / chocolatey PATH
// ambiguity that LookPath cannot resolve when the binary
// isn't on %PATH%.
//
// Keep-open: `cmd /k` keeps the spawned cmd window open
// after nightme exits, so any error output stays visible
// (analogous to the macOS / Linux `read` suffix). No extra
// suffix needed.
//
// The window title is derived from name + args for at-a-glance
// identification in the taskbar; the bare name (e.g. "nightme")
// reads better as a title than the full resolved path.
//
// name is currently IGNORED for path resolution but still used
// for the window title — see the matching note on the Unix
// version.
func OpenTerminal(ctx context.Context, name string, args ...string) error {
	bin, err := currentExePath()
	if err != nil {
		return err
	}
	cmdExe := ComSpecOrDefault()
	title := name
	if len(args) > 0 {
		title = name + " " + strings.Join(args, " ")
	}
	fullArgs := append([]string{"/c", "start", title, "cmd", "/k", bin}, args...)
	cmd := NewWith(ctx, Options{HideWindow: false}, cmdExe, fullArgs...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("proc: start terminal: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
