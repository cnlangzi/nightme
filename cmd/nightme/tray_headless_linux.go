//go:build linux

// Linux headless detection. On Linux the systray backing
// (getlantern/systray → GTK) requires a reachable X server or
// Wayland compositor; if neither is present it prints
//
//	(nightme:NNNN): Gtk-WARNING **: cannot open display:
//
// and depending on the GTK version either fails to start the
// event loop or hangs in init. The `_daemon` child inherits no
// console, so the warning lands in
// ~/.nightme/daemon-stderr.log and `nightme start` then trips
// the 15s readiness timeout.
//
// isHeadless reports true when there is no plausible GUI session
// so runTrayOwning can skip the systray branch entirely and run
// the runtime loop directly. CLI / REPL control stays intact
// (daemoncontrol IPC runs over the Unix socket, not the tray).
//
// Rule (strict scheme — false positives only cost a tray icon,
// false negatives make the daemon unstartable):
//
//   1. XDG_SESSION_TYPE == "tty"   — explicit "no GUI" signal
//                                    from logind / systemd.
//   2. DISPLAY and WAYLAND_DISPLAY both empty — the failing
//                                    scenario this rule targets.
//
// XDG_SESSION_TYPE=x11 / wayland are treated as trust signals:
// the session manager says a display exists, so we attempt the
// tray even if the corresponding env var is missing. (Wayland
// compositors vary; some don't export WAYLAND_DISPLAY into
// forked children. Trusting XDG_SESSION_TYPE=wayland is the
// safer call.)

package main

import "os"

func isHeadless() bool {
	switch os.Getenv("XDG_SESSION_TYPE") {
	case "tty":
		// Explicit "no GUI session" signal from logind /
		// systemd — trust it over whatever DISPLAY might say.
		return true
	case "x11", "wayland":
		// Session manager claims a display exists. Trust it:
		// some Wayland compositors do not propagate
		// WAYLAND_DISPLAY into forked children, so the env
		// probe alone would (incorrectly) flag them headless.
		return false
	}
	// XDG_SESSION_TYPE unset, "unspecified", "classic",
	// "remote", or anything else is not a positive "I have a
	// display" signal. Fall through to the env-var probe —
	// if both DISPLAY and WAYLAND_DISPLAY are empty the daemon
	// has nothing to attach to.
	return os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == ""
}
