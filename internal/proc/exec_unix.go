//go:build !windows

package proc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
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
// safe default — HideWindow=false means "don't hide". Use
// New() (which passes HideWindow=true) for the default hide-
// window behaviour. HideWindow is a no-op on Unix (there is no
// "console window" concept), but the field exists so callers
// can write platform-agnostic code that threads Options
// through to NewWith.
type Options struct {
	// HideWindow, when true, suppresses the child's console
	// window. No-op on Unix. The default for daemon-internal
	// command execution is true (hide) — every proc.New()
	// caller gets this for free.
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
// available knobs. New() is the convenience wrapper.
//
// On Unix, HideWindow is a no-op — the child always gets
// Setsid: true so it detaches from the daemon's controlling TTY
// (F-54 / stop hang investigation: pi / claude / codex / opencode
// / dsh all block on a TTY-driven kqueue if the TTY fd is
// inherited). With Setsid, the child becomes the leader of its
// own session AND process group, so callers can later broadcast
// SIGINT to the whole subtree via SignalProcessGroup.
//
// SIGTERMGrace is declared in exec_common.go (cross-platform).
// On Unix it's a SIGTERM-flush window before SIGKILL; on
// Windows it's a WaitDelay (natural-exit window before hard
// kill — see exec_windows.go for why we can't fix the
// .git/index.lock issue the same way there).

// fix-git-lock-file 2026-08-29: every child spawned via
// proc.NewWith now leaves a clean `.git/index.lock` when
// cancelled, by overriding cmd.Cancel to a SIGTERM-then-SIGKILL
// path that stdlib's existing watchCtx goroutine invokes on
// ctx-fire. Replaces the earlier WithGrace + graced-map design
// (which leaked one goroutine + map entry per successful
// proc.New whose ctx never fired — see /review finding #1).
func NewWith(ctx context.Context, _ Options, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	armGraceCancel(cmd)
	return cmd
}

// armGraceCancel rewrites cmd.Cancel from exec.CommandContext's
// default `func() error { return c.Process.Kill() }` (raw
// SIGKILL) to SIGTERM → SIGTERMGrace → SIGKILL, and returns
// os.ErrProcessDone so stdlib's watchCtx doesn't wrap Run()'s
// result with ctx.Err().
//
// Mechanics:
//
//   - cmd.Cancel is invoked by stdlib's watchCtx goroutine —
//     one already started by exec.CommandContext because Cancel
//     != nil at cmd.Start time. We reuse that goroutine; no new
//     goroutine, no map, no mutex.
//   - cmd.Cancel may be called BEFORE cmd.Start (ctx fires
//     before we ever spawned the child). In that case
//     cmd.Process is nil; we return os.ErrProcessDone and
//     stdlib's Start preflight handles the ctx error separately.
//   - Once Start has set cmd.Process, Cancel sends SIGTERM to
//     the entire process group (Setsid armed in NewWith
//     ensures pid == pgid). On pgid-kill non-ESRCH (EPERM on
//     a foreign pgid), we silently fall through: the
//     single-pid attempt would fail for the same reason, and
//     the SIGKILL AfterFunc below is the real upper bound.
//     Surfacing the error would replace the child's real exit
//     status in Run()'s return value, which is worse than the
//     diagnostic loss.
//   - After SIGTERMGrace, an AfterFunc fires SIGKILL on the
//     same pgid. The closure captures cmd by reference for
//     the full grace window — this is a bounded retention cost
//     (timer-heap slot + ~1 KB cmd struct), not a leak.
//     Timer.Stop exists but doesn't help: the closure's own
//     capture keeps cmd alive regardless. The common case
//     (child reaped within grace) gets ESRCH from the kill
//     syscall and returns immediately. We deliberately do NOT
//     read cmd.ProcessState here (race against Wait's write,
//     flagged by `go test -race` on CI).
//
// armGraceCancel is idempotent: it just assigns cmd.Cancel. Two
// proc.NewWith calls on the same *exec.Cmd are not possible
// (each New returns a fresh cmd).
func armGraceCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		proc := cmd.Process
		if proc == nil {
			return os.ErrProcessDone
		}
		pid := proc.Pid
		if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			// pgid-kill non-ESRCH — see contract above.
			_ = err
		}
		// Grace timer. Returns *time.Timer but we discard it:
		// even though Timer.Stop exists, the closure's `cmd`
		// reference keeps cmd alive in the timer heap for the
		// full grace window regardless. We accept this bounded
		// retention (timer-heap slot + ~1 KB cmd struct) in
		// exchange for not needing a Wait()-side stop hook —
		// stdlib's exec.Cmd doesn't expose one, and adding a
		// parallel finalizer / runtime.SetFinalizer would be
		// more code than the cost it saves.
		time.AfterFunc(SIGTERMGrace, func() {
			// Race fix: do NOT read cmd.ProcessState here.
			// stdlib's cmd.Wait() writes it concurrently from
			// the main test goroutine, which Go's race
			// detector flags (verified in CI on Ubuntu, the
			// fix-git-lock-file branch). The previous design
			// tried to short-circuit on ProcessState != nil
			// but that's a data race; we instead always run
			// the syscall path and rely on ESRCH for the
			// common "child reaped within grace" case. Cost:
			// one extra syscall per clean exit (negligible).
			p := cmd.Process
			if p == nil {
				return
			}
			if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil {
				if errors.Is(err, syscall.ESRCH) {
					return
				}
				// Non-ESRCH pgid-kill failure (e.g. EPERM
				// on a foreign pgid). Best-effort single-pid
				// fallback; if that also fails the child
				// survives until the wait goroutine reaps it.
				// The error is discarded: surfacing it would
				// replace the child's real exit status in
				// Run()'s return value. Documented as
				// "best-effort" in the contract above.
				_ = p.Kill()
			}
		})
		return os.ErrProcessDone
	}
}

// SetCloseOnExec arms FD_CLOEXEC on f's descriptor so it is NOT
// inherited by processes this one later execs.
//
// The caller that needs this is anyone adopting a descriptor
// handed over via exec.Cmd.ExtraFiles: forkExec deliberately
// clears FD_CLOEXEC on those so the child can see them, and
// os.NewFile does not re-arm it. A long-lived daemon that spawns
// subprocesses (shell !cmd, gtw hooks, agent bridges) would
// otherwise leak the descriptor into every one of them — which
// matters most for flock, whose lock is bound to the open file
// description and therefore outlives the daemon as long as any
// descendant still holds a copy.
//
// No-op on Windows (see exec_windows.go): CreateProcess only
// passes explicitly-listed handles, so there is nothing to
// disarm.
func SetCloseOnExec(f *os.File) error {
	if f == nil {
		return nil
	}
	if _, err := unix.FcntlInt(f.Fd(), unix.F_SETFD, unix.FD_CLOEXEC); err != nil {
		return fmt.Errorf("set FD_CLOEXEC: %w", err)
	}
	return nil
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

// OpenTerminal spawns a NEW visible terminal window that runs
// the nightme binary (resolved via os.Executable, NOT
// exec.LookPath) with the given args. Unlike New/NewWith —
// which build a child that inherits the daemon's (absent) TTY
// — OpenTerminal launches a GUI terminal emulator (macOS:
// Terminal.app / iTerm2 via AppleScript; Linux: gnome-terminal
// / konsole / xterm / …; Windows: cmd /k) so the user always
// sees a window.
//
// The `name` argument is currently IGNORED on every platform:
// the tray only fires for the daemon child, which IS nightme,
// so re-spawning with the running binary's own path is always
// the right answer. The argument stays on the signature so
// cross-platform call sites stay platform-agnostic — drop it
// only if the platform ever needs to invoke a different
// binary.
//
// Fire-and-forget: the helper reaps the short-lived launcher
// (osascript / the terminal emulator) in a goroutine and
// returns nil on successful launch; the visible window is its
// own process whose lifetime is independent of the caller. Use
// this for tray menu clicks that need to show the user a
// terminal (REPL, logs tail, interactive subcommands).
func OpenTerminal(ctx context.Context, name string, args ...string) error {
	_ = name // see doc above; reserved for future per-platform override.
	if runtime.GOOS == "darwin" {
		return openTerminalMac(ctx, args)
	}
	return openTerminalLinux(ctx, args)
}

// openTerminalMac drives Terminal.app via AppleScript.
//
// Why Terminal.app only — no iTerm2. We previously preferred iTerm2
// because its "create window with default profile command" form
// was claimed to be cleaner UX. It isn't: that AppleScript
// command does NOT hand the string to a shell — iTerm2
// tokenizes the command string on whitespace and execve's the
// first token with the rest as argv. So a command like
// `'/path/to/nightme' 'kill'; echo ; printf '...'; read dummy`
// is split into ["/path/to/nightme", "kill;", "echo", ";" ...],
// and nightme receives `kill;` (or `kill ;` after iTerm's quote
// stripping) as its subcommand name. cobra then errors with
// `Error: unknown command "kill ;" for "nightme"` — the exact
// bug the user reported.
//
// Terminal.app's `do script`, by contrast, hands the string to
// the user's default shell (zsh on stock macOS), which parses
// the `;` separators and quotes correctly. So we drive
// Terminal.app only; iTerm2 users still get the same UX (a
// fresh terminal running their nightme command), just driven
// by Terminal.app instead.
//
// Path resolution: we use os.Executable() (the absolute path of
// the currently-running nightme binary) instead of exec.LookPath,
// because AppleScript's `do script` hands the command string to
// the Terminal-app default shell. That shell is /bin/zsh on a
// stock macOS install — a NON-login zsh that does NOT source the
// user's ~/.zprofile / ~/.zshenv. exec.LookPath("nightme") then
// returns an error for any binary installed via `go install`
// (~/go/bin) or any other path outside the system PATH, and the
// shell prints "command not found" before closing the window
// before the user can read anything.
//
// os.Executable() returns the exact path the user invoked the
// daemon with — matching the running daemon's binary 1:1 —
// regardless of $PATH. This survives every install location we
// know about (Homebrew, go install, scoop-like manual cp, even
// paths containing spaces).
//
// Keep-open suffix: the spawned shell command is suffixed with
// `; echo ; printf 'press enter to close\n' ; read dummy` (see
// keepOpenShellSuffix). Without this, any
// subcommand that exits (nightme logs when the user Ctrl-Cs it,
// nightme list, etc.) closes the Terminal window immediately,
// taking any error output with it. The trailing `read` waits for
// the user to dismiss the window. Cost: REPL users hit one extra
// Enter after they exit the REPL — acceptable trade-off for
// diagnosability on every other code path.
func openTerminalMac(ctx context.Context, args []string) error {
	exe, err := currentExePath()
	if err != nil {
		return err
	}
	cmdStr := escapeAppleScriptString(buildTerminalShellCommand(exe, args, ""))

	// Terminal.app only. See function doc for why iTerm2 is
	// intentionally not tried. Three things have to happen for
	// a tray click to be visibly responsive:
	//
	//   1. The new tab has to land in a window that's maximized
	//      to the desktop — `set zoomed of front window to true`
	//      is Terminal's native maximize (it's the green button
	//      click). `set bounds to screen size` is too crude: it
	//      overlaps the menu bar and looks wrong on Retina where
	//      point vs pixel bounds diverge. zoomed respects both.
	//   2. Terminal.app has to come to the front. `activate`
	//      alone works on a single-space setup, but on a
	//      multi-Space setup the window stays on whichever
	//      Space Terminal.app was last active on — i.e. the
	//      user's empty Desktop, hidden behind whatever they
	//      were actually looking at. The followup System Events
	//      `set frontmost of process "Terminal" to true` is the
	//      only thing that reliably migrates the window onto
	//      the current Space.
	//   3. The tab must exist before we maximise — `do script`
	//      is synchronous in AppleScript (the tab is created
	//      before the next line runs), so the ordering below is
	//      already correct.
	const snip = `tell application "Terminal"
    do script "%s"
    set zoomed of front window to true
    activate
end tell
tell application "System Events"
    set frontmost of process "Terminal" to true
end tell`
	cmd := NewWith(ctx, Options{}, "osascript", "-e", fmt.Sprintf(snip, cmdStr))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("proc: osascript for Terminal: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// keepOpenShellSuffix is the POSIX keep-open suffix appended
// to the spawned shell command on Linux. The trailing `read`
// blocks until the user presses Enter, so the terminal window
// stays around long enough for the user to read whatever
// output nightme produced (including error messages) instead
// of vanishing the instant the command exits.
//
// On Linux this is necessary because most terminal emulators
// (gnome-terminal, konsole, xterm, …) close the window when
// the spawned shell exits. On macOS Terminal.app's
// `do script` already leaves the new window / tab open after
// the spawned shell exits (the shell session persists and
// returns to its prompt), so this suffix is unnecessary
// there and only adds a `press enter to close` noise line to
// every tray-spawned terminal. macOS therefore omits the suffix
// at build time (see buildTerminalShellCommand).
//
// `read -p` is a bash-ism; it isn't supported by dash (the
// default /bin/sh on Debian / Ubuntu) or other minimal POSIX
// shells. `printf '…\n'; read dummy` is the portable equivalent
// — every shell implementing POSIX `read` accepts an unnamed
// variable to read into.
//
// Exposed at package scope so the keep-open pattern can't drift
// between the helper that emits it and the tests that pin it.
const keepOpenShellSuffix = `; echo ; printf 'press enter to close\n' ; read dummy`

// buildTerminalShellCommand assembles the inner shell command
// string that drives the spawned terminal window. exe is the
// absolute path of the nightme binary (typically from
// os.Executable()); args are the CLI subcommand arguments. The
// result is wrapped in keepOpenShellSuffix so the terminal stays
// open after nightme exits and the user can read any error
// output.
//
// Each component is shell-quoted via shellQuote (single-quoted
// with embedded ' escaped as '\”), so the result is shell-safe
// regardless of the contents of exe or args — spaces, quotes,
// backslashes, and other metacharacters. The suffix itself uses
// single quotes too: a non-login shell (the default on both
// macOS and fresh Linux DE sessions) treats single-quoted
// strings literally. macOS AppleScript layers add another
// quoting layer; see escapeAppleScriptString for that side.
// Windows uses cmd /k which doesn't need this helper at all.
//
// Exposed at package scope so it can be unit-tested without
// invoking osascript / Terminal.app / gnome-terminal.
// buildTerminalShellCommand assembles the inner shell command
// string that drives the spawned terminal window. exe is the
// absolute path of the nightme binary (typically from
// os.Executable()); args are the CLI subcommand arguments.
//
// suffix is appended verbatim after a single space — the
// default is keepOpenShellSuffix, which most terminal
// emulators on Linux need to keep their window open. macOS
// passes the empty string because Terminal.app's
// `do script` already keeps the window open after the shell
// exits.
//
// Each component is shell-quoted via shellQuote (single-quoted
// with embedded ' escaped as '\”), so the result is shell-safe
// regardless of the contents of exe or args — spaces, quotes,
// backslashes, and other metacharacters. The suffix itself
// uses single quotes too: a non-login shell (the default on
// both macOS and fresh Linux DE sessions) treats single-quoted
// strings literally. macOS AppleScript layers add another
// quoting layer; see escapeAppleScriptString for that side.
//
// Exposed at package scope so it can be unit-tested without
// invoking osascript / Terminal.app / gnome-terminal.
func buildTerminalShellCommand(exe string, args []string, suffix string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, shellQuote(exe))
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ") + suffix
}

// shellQuote wraps s in single quotes for safe inclusion in a
// shell command. Single quotes inside s are escaped with the
// standard `'\”` idiom (close, literal quote, reopen) so the
// result is shell-safe regardless of s's content — including
// paths containing an apostrophe (e.g. /Users/O'Brien/nightme
// on macOS, which HFS+/APFS allows).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// escapeAppleScriptString escapes the backslash and double-quote
// in s so the result can be embedded inside an AppleScript
// double-quoted string ("..."). AppleScript treats both as
// escape introducers within `"..."`; without this, a path
// containing a literal `"` or `\` would break the AppleScript
// string and the do-script / create-window call would either
// syntax-error or interpret arbitrary characters past the break.
func escapeAppleScriptString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// appInstalled reports whether the named macOS application is
// callable via AppleScript. The check is "ls of the standard
// install path"; this is the same heuristic the osascript
// runtime uses when it can't find the app, so a match here means
// AppleScript will succeed.


// openTerminalLinux probes a fixed list of terminal emulators in
// preference order, executing the first one found on $PATH.
//
// Each probe wraps the nightme binary (resolved via
// os.Executable()) in `sh -c "<cmd>"` and hands that to the
// emulator. The `sh -c` wrapper does two things at once:
//
//   - Path resolution: the shell receives the absolute path
//     directly, so a `go install`-style install to ~/go/bin
//     (or any other path missing from $PATH) works without
//     depending on the emulator's own $PATH lookup. The
//     previous design passed the bare name `nightme` and
//     relied on each emulator's default shell sourcing the
//     user's profile — usually true, occasionally not.
//   - Keep-open suffix: `; echo ; printf 'press enter to close\n' ; read dummy`
//     keeps the window around after nightme exits so the user
//     can read any error output (see buildTerminalShellCommand).
//
// The wrapper is uniform across all emulators in the list
// because every modern emulator honours `-- COMMAND ARGS…` or
// `-e COMMAND ARGS…` by passing the rest as argv to execvp,
// which means sh receives its `-c` argument as a single
// string and parses it internally. The emulators we list do
// NOT need to know about shell syntax.
func openTerminalLinux(ctx context.Context, args []string) error {
	exe, err := currentExePath()
	if err != nil {
		return err
	}
	shellCmd := buildTerminalShellCommand(exe, args, keepOpenShellSuffix)

	// Each probe lists the emulator binary plus its "run a
	// command" prefix (some want --, some want -e, some want
	// neither). The sh -c wrapper is appended uniformly below.
	for _, prefix := range LinuxTerminalProbes {
		bin := prefix[0]
		if _, err := exec.LookPath(bin); err != nil {
			continue
		}
		argv := linuxProbeArgv(prefix, shellCmd)
		cmd := NewWith(ctx, Options{}, bin, argv[1:]...)
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("proc: %s: %w", bin, err)
		}
		go func() { _ = cmd.Wait() }()
		return nil
	}
	return fmt.Errorf("proc: no supported terminal emulator found on $PATH (tried: %v)", probeNames(LinuxTerminalProbes))
}

// LinuxTerminalProbes is the ordered list of terminal emulators
// openTerminalLinux probes, in preference order. Each entry is
// the emulator binary plus its "run a command" prefix (some
// want --, some want -e, some want neither); openTerminalLinux
// appends `sh -c <shellCmd>` uniformly (see linuxProbeArgv).
//
// Exposed at package scope so the probe list and argv
// construction can be unit-tested without standing up a real
// emulator. A probe is a configuration choice, not test-only
// state — see e.g. the CLI surface area for future per-emulator
// overrides.
var LinuxTerminalProbes = [][]string{
	{"gnome-terminal", "--"},
	{"konsole", "-e"},
	{"alacritty", "-e"},
	{"kitty"},
	{"xfce4-terminal", "-e"},
	{"lxterminal", "-e"},
	{"mate-terminal", "-e"},
	{"xterm", "-e"},
	{"foot"},
	{"wezterm", "start", "--"},
}

// linuxProbeArgv builds the argv that openTerminalLinux hands to
// a terminal emulator for the given probe prefix. The
// `sh -c <shellCmd>` suffix is appended uniformly so every
// emulator — regardless of whether it accepts `--`, `-e`, or
// neither — receives a shell-interpretable command line. See
// buildTerminalShellCommand for the shell-side rationale.
func linuxProbeArgv(prefix []string, shellCmd string) []string {
	return append(append([]string{}, prefix...), "sh", "-c", shellCmd)
}

func probeNames(probes [][]string) []string {
	out := make([]string, len(probes))
	for i, p := range probes {
		out[i] = p[0]
	}
	return out
}
