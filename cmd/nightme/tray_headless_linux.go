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
//   - XDG_SESSION_TYPE ∈ {x11, wayland} → not headless. Trust
//     the session manager: it claims a display exists. Some
//     Wayland compositors do not propagate WAYLAND_DISPLAY into
//     forked children, so trusting the session type is the safer
//     call.
//
//   - Otherwise (XDG_SESSION_TYPE unset, "tty", "unspecified",
//     "classic", "remote", …): not headless iff either DISPLAY
//     or WAYLAND_DISPLAY is set. This covers:
//
//   - `ssh -X` / `ssh -Y`: XDG_SESSION_TYPE=tty but DISPLAY
//     points to the forwarded socket — we MUST respect the
//     user's intent to forward X, not disable the tray.
//   - plain tty session with no display env: still treated as
//     headless.
//   - unset / unspecified session type with no env: still
//     treated as headless.
//
// The previous version of this rule short-circuited on
// XDG_SESSION_TYPE=tty alone, which incorrectly classified
// ssh -X sessions as headless and silently disabled a tray the
// user had explicitly asked for.

package main

import "os"

func isHeadless() bool {
	switch os.Getenv("XDG_SESSION_TYPE") {
	case "x11", "wayland":
		// Session manager claims a display exists. Trust it
		// (see comment above).
		return false
	}
	// Anything else (unset / tty / unspecified / classic /
	// remote / …): defer to the env-var probe. The daemon is
	// headless only if no display env var is set — that
	// covers plain tty sessions while still honouring
	// X-forwarding where DISPLAY is explicitly set.
	return os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == ""
}
