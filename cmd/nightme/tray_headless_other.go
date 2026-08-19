//go:build !linux

// Non-Linux headless stub: tray is unconditionally available.
//
//   - macOS: the systray backing (Cocoa / NSStatusBar) talks to
//     WindowServer. WindowServer runs for any logged-in user
//     session, and `nightme start` is invoked by an interactive
//     user — there is no "headless macOS" path this code can
//     hit.
//   - Windows: Shell_NotifyIcon is permissive; it does not
//     require an interactive desktop. Registering from a
//     service or non-interactive session succeeds, the icon
//     just won't be visible. We never get the GTK "cannot
//     open display" failure mode.
//
// Keeping the stub always-false matches the strict-scheme rule
// "rather skip than crash" extended across platforms: these
// platforms don't crash, so no skip is needed.

package main

func isHeadless() bool { return false }
