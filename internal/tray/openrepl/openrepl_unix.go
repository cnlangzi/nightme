//go:build !windows

// Unix implementation of tray "open terminal" for all commands.
//
// macOS: AppleScript-driven Terminal.app (or iTerm2 if installed).
// Linux: probe a small set of common terminal emulators in
// preference order, use the first one on $PATH; fall through to
// the next on miss. No X11/Wayland detect — if a terminal is on
// PATH it can almost certainly attach to whatever the user's
// session is. The fallback chain is in order of how common each
// emulator is in nightme's user base (most are Linux desktop
// users with a default Ubuntu/Fedora setup).
//
// All process spawns route through proc.NewVisible so the
// platform-specific SysProcAttr (Setsid on Unix) is applied
// consistently with the rest of the codebase.

package openrepl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/cnlangzi/nightme/internal/proc"
)

// openCmd is the per-OS implementation behind OpenCmd(). Build
// tag keeps it off Windows; see openrepl_windows.go for the Win32
// recipe.
//
// args is the nightme subcommand + flags to run (empty = REPL).
func openCmd(args ...string) error {
	if runtime.GOOS == "darwin" {
		return openCmdMac(args)
	}
	return openCmdLinux(args)
}

func openCmdMac(args []string) error {
	// Build the shell command string: `nightme` or `nightme logs`
	cmdStr := "nightme"
	if len(args) > 0 {
		cmdStr = "nightme " + strings.Join(args, " ")
	}
	// AppleScript variants in preference order. We try iTerm2
	// first because its "create window with default profile
	// command" form is the cleanest user experience (no extra
	// "press Return to run" prompt that Terminal requires).
	candidates := []struct {
		app string
		// %s is the command string to run
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
		cmd := proc.NewVisible(context.Background(), "osascript", "-e", fmt.Sprintf(c.snip, cmdStr))
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("openrepl: osascript for %s: %w", c.app, err)
		}
		go func() { _ = cmd.Wait() }()
		return nil
	}
	// Last-resort fallback: try AppleScript on Terminal.app
	// directly (the bundle could exist without AppleScript
	// being enabled — e.g. an MDM profile that blocks the
	// previous osascript call).
	cmd := proc.NewVisible(context.Background(), "osascript", "-e",
		fmt.Sprintf(`tell application "Terminal" to do script "%s"`, cmdStr))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("openrepl: osascript (fallback) for Terminal: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// appInstalled reports whether the named macOS application is
// callable via AppleScript. The check is "ls of the standard
// install path"; this is the same heuristic the osascript
// runtime uses when it can't find the app, so a match here means
// AppleScript will succeed.
func appInstalled(name string) bool {
	paths := []string{
		"/Applications/" + name + ".app",
		"/System/Applications/" + name + ".app",
		"/Applications/Utilities/" + name + ".app",
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// openCmdLinux probes a fixed list of terminal emulators in
// preference order, executing the first one found on $PATH.
// Each probe appends the nightme command + args after the
// emulator's "run a command" separator.
func openCmdLinux(args []string) error {
	// Build the full argv tail: ["nightme", args...]
	cmdTail := append([]string{"nightme"}, args...)

	// Each entry: {binary, [separator flags...], cmdTail...}
	// The separator is the documented "run a command" form
	// for each emulator (some want -- arg1 arg2, some want
	// -e arg1 arg2).
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
		cmd := proc.NewVisible(context.Background(), bin, argv[1:]...)
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("openrepl: %s: %w", bin, err)
		}
		go func() { _ = cmd.Wait() }()
		return nil
	}
	return fmt.Errorf("openrepl: no supported terminal emulator found on $PATH (tried: %v)", probeNames(probes))
}

func probeNames(probes [][]string) []string {
	out := make([]string, len(probes))
	for i, p := range probes {
		out[i] = p[0]
	}
	return out
}
