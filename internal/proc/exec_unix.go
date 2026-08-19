//go:build !windows

package proc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
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

// openTerminalMac drives Terminal.app or iTerm2 via AppleScript.
// iTerm2 is tried first because its "create window with default
// profile command" form is the cleanest UX (no "press Return to
// run" prompt that Terminal.app shows).
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
	cmdStr := escapeAppleScriptString(buildTerminalShellCommand(exe, args))

	candidates := []struct {
		app  string
		snip string
	}{
		{
			app:  "iTerm",
			snip: `tell application "iTerm" to create window with default profile command "%s"`,
		},
		{
			app:  "Terminal",
			snip: `tell application "Terminal" to do script "%s"`,
		},
	}
	for _, c := range candidates {
		if !appInstalled(c.app) {
			continue
		}
		cmd := NewWith(ctx, Options{}, "osascript", "-e", fmt.Sprintf(c.snip, cmdStr))
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("proc: osascript for %s: %w", c.app, err)
		}
		go func() { _ = cmd.Wait() }()
		return nil
	}
	// Last-resort fallback: try AppleScript on Terminal.app
	// directly (the bundle could exist without AppleScript
	// being enabled — e.g. an MDM profile that blocks the
	// previous osascript call).
	cmd := NewWith(ctx, Options{}, "osascript", "-e",
		fmt.Sprintf(`tell application "Terminal" to do script "%s"`, cmdStr))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("proc: osascript (fallback) for Terminal: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// keepOpenShellSuffix is the POSIX-compatible keep-open suffix
// appended to the spawned shell command. Read together with the
// command by `sh -c` (Linux) or the shell that Terminal.app
// hands the string to (macOS):
//
//	<command> ; echo ; printf 'press enter to close\n' ; read dummy
//
// The trailing `read` blocks until the user presses Enter, so
// the terminal window stays around long enough for the user to
// read whatever output nightme produced (including error
// messages) instead of vanishing the instant the command exits.
//
// `read -p` is a bash-ism; it isn't supported by dash (the
// default /bin/sh on Debian / Ubuntu) or other minimal POSIX
// shells. `printf '…\n'; read dummy` is the portable equivalent
// — every shell implementing POSIX `read` accepts an unnamed
// variable to read into. Windows uses `cmd /k` for the same
// effect and doesn't touch this constant.
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
func buildTerminalShellCommand(exe string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, shellQuote(exe))
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ") + keepOpenShellSuffix
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
func appInstalled(appName string) bool {
	paths := []string{
		"/Applications/" + appName + ".app",
		"/System/Applications/" + appName + ".app",
		"/Applications/Utilities/" + appName + ".app",
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

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
	shellCmd := buildTerminalShellCommand(exe, args)

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
