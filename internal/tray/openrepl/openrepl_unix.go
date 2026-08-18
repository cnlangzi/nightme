//go:build !windows

// Unix implementation of "Open REPL" from the tray menu.
//
// macOS: AppleScript-driven Terminal.app (or iTerm2 if installed).
// Linux: probe a small set of common terminal emulators in
// preference order, use the first one on $PATH; fall through to
// the next on miss. No X11/Wayland detect — if a terminal is on
// PATH it can almost certainly attach to whatever the user's
// session is. The fallback chain is in order of how common each
// emulator is in nightme's user base (most are Linux desktop
// users with a default Ubuntu/Fedora setup).

package openrepl

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// open is the per-OS implementation behind Open(). Build tag
// keeps it off Windows; see openrepl_windows.go for the Win32
// recipe.
//
// On macOS the first preference is Terminal.app because every
// macOS install has it. iTerm2 is checked second because it is
// the most popular third-party terminal on macOS — and unlike
// Terminal, iTerm supports "create window with default profile
// command" so we can pre-populate the prompt. If neither is
// installed, we fall back to /usr/bin/open which hands the
// command to Launch Services (which routes to whatever the user
// has set as the default terminal via Finder > Get Info).
func open() error {
	if runtime.GOOS == "darwin" {
		return openMac()
	}
	return openLinux()
}

func openMac() error {
	// AppleScript variants in preference order. We try iTerm2
	// first because its "create window with default profile
	// command" form is the cleanest user experience (no extra
	// "press Return to run" prompt that Terminal.app shows).
	candidates := []struct {
		app  string
		snip string
	}{
		{
			app: "iTerm",
			// iTerm2 supports "create window with default
			// profile command" — no need to script the
			// keystroke-return that Terminal requires.
			snip: `tell application "iTerm" to create window with default profile command "nightme"`,
		},
		{
			app: "Terminal",
			// Terminal.app's `do script` opens a new window
			// (or tab in the front window) and runs the
			// command. The "in front window" variant is
			// friendlier when the user already has Terminal
			// open; fall through to a fresh window if the
			// choice is ambiguous.
			snip: `tell application "Terminal" to do script "nightme"`,
		},
	}
	for _, c := range candidates {
		if !appInstalled(c.app) {
			continue
		}
		cmd := exec.Command("osascript", "-e", c.snip)
		// Detach: osascript hands the command to the GUI
		// process and returns; the GUI window is the user-
		// visible artefact, not the osascript subprocess.
		// proc.New is not relevant here because we are not
		// spawning a child of the daemon (no Setsid, no
		// CREATE_NO_WINDOW); we are pinging a GUI helper
		// that runs in the user's session.
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("openrepl: osascript for %s: %w", c.app, err)
		}
		// Reap the osascript helper so it does not become a
		// zombie; we don't need its exit code.
		go func() { _ = cmd.Wait() }()
		return nil
	}
	// Last-resort fallback: `open -a Terminal` via Launch
	// Services. If the user has set a different default
	// terminal, this routes there. Slightly more "magic" but
	// keeps the feature working on machines where neither
	// iTerm2 nor the Apple-bundled Terminal.app is callable
	// via AppleScript (e.g. locked-down MDM profiles).
	cmd := exec.Command("open", "-a", "Terminal", "--args", "nightme")
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("openrepl: 'open -a Terminal': %w", err)
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

// openLinux probes a fixed list of terminal emulators in
// preference order, executing the first one found on $PATH. The
// order reflects the rough popularity in nightme's user base
// (Ubuntu/Fedora defaults first; niche terminals last).
//
// The command string for each emulator is the documented "run a
// command and exit" form; some terminals (e.g. alacritty,
// kitty) accept `-e CMD`, others (gnome-terminal, konsole)
// accept `-- CMD` or `-e CMD`. We standardise on `-e` because it
// is the most common, then add overrides where required.
//
// Why we don't check $DISPLAY: a Linux user with no display
// (headless server) would not have a tray icon to click, so
// openrepl won't be reached in that environment. The daemon
// itself runs in a session that does have a display when the
// tray is up. If a future feature does want to detect a missing
// $DISPLAY for diagnostics, do it here, not in the tray handler.
func openLinux() error {
	// Each entry: {binary, [args..., "--", "nightme"]}
	// The trailing "nightme" is appended literally; some
	// emulators want a separate exec arg (gnome-terminal
	// wants -- arg1 arg2 with arg2 being the command), some
	// treat the rest of argv as the command.
	probes := [][]string{
		{"gnome-terminal", "--", "nightme"},
		{"konsole", "-e", "nightme"},
		{"alacritty", "-e", "nightme"},
		{"kitty", "nightme"},
		{"xfce4-terminal", "-e", "nightme"},
		{"lxterminal", "-e", "nightme"},
		{"mate-terminal", "-e", "nightme"},
		{"xterm", "-e", "nightme"},
		{"foot", "nightme"},
		{"wezterm", "start", "--", "nightme"},
	}
	for _, argv := range probes {
		bin := argv[0]
		if _, err := exec.LookPath(bin); err != nil {
			continue
		}
		cmd := exec.Command(bin, argv[1:]...)
		// Terminal emulators must inherit stdin (some
		// emulators ignore argv and read commands from
		// stdin otherwise) and they must NOT inherit the
		// daemon's stderr (we don't want the terminal's
		// banner or ours to leak into daemon-stderr.log).
		// stdout we leave as /dev/null because the
		// terminal will not produce useful stdout — its
		// window IS the output.
		cmd.Stdin = nil
		cmd.Stdout = nil
		cmd.Stderr = nil
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
