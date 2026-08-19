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
// `name args...`. Unlike New/NewWith — which build a child that
// inherits the daemon's (absent) TTY — OpenTerminal launches a
// GUI terminal emulator (macOS: Terminal.app / iTerm2 via
// AppleScript; Linux: gnome-terminal / konsole / xterm / …) so
// the user always sees a window.
//
// Fire-and-forget: the helper reaps the short-lived launcher
// (osascript / the terminal emulator) in a goroutine and
// returns nil on successful launch; the visible window is its
// own process whose lifetime is independent of the caller. Use
// this for tray menu clicks that need to show the user a
// terminal (REPL, logs tail, interactive subcommands).
//
// name is resolved via exec.LookPath so a bare "nightme" works
// regardless of install location.
func OpenTerminal(ctx context.Context, name string, args ...string) error {
	if runtime.GOOS == "darwin" {
		return openTerminalMac(ctx, name, args)
	}
	return openTerminalLinux(ctx, name, args)
}

// openTerminalMac drives Terminal.app or iTerm2 via AppleScript.
// iTerm2 is tried first because its "create window with default
// profile command" form is the cleanest UX (no "press Return to
// run" prompt that Terminal.app shows).
func openTerminalMac(ctx context.Context, name string, args []string) error {
	cmdStr := name
	if len(args) > 0 {
		cmdStr = name + " " + strings.Join(args, " ")
	}
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
// Each probe appends the command + args after the emulator's
// "run a command" separator (some want -- arg1 arg2, some want
// -e arg1 arg2).
func openTerminalLinux(ctx context.Context, name string, args []string) error {
	cmdTail := append([]string{name}, args...)
	probes := [][]string{
		append([]string{"gnome-terminal", "--"}, cmdTail...),
		append([]string{"konsole", "-e"}, cmdTail...),
		append([]string{"alacritty", "-e"}, cmdTail...),
		append([]string{"kitty"}, cmdTail...),
		append([]string{"xfce4-terminal", "-e"}, cmdTail...),
		append([]string{"lxterminal", "-e"}, cmdTail...),
		append([]string{"mate-terminal", "-e"}, cmdTail...),
		append([]string{"xterm", "-e"}, cmdTail...),
		append([]string{"foot"}, cmdTail...),
		append([]string{"wezterm", "start", "--"}, cmdTail...),
	}
	for _, argv := range probes {
		bin := argv[0]
		if _, err := exec.LookPath(bin); err != nil {
			continue
		}
		cmd := NewWith(ctx, Options{}, bin, argv[1:]...)
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("proc: %s: %w", bin, err)
		}
		go func() { _ = cmd.Wait() }()
		return nil
	}
	return fmt.Errorf("proc: no supported terminal emulator found on $PATH (tried: %v)", probeNames(probes))
}

func probeNames(probes [][]string) []string {
	out := make([]string, len(probes))
	for i, p := range probes {
		out[i] = p[0]
	}
	return out
}
