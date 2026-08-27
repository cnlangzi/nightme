//go:build linux && gui

// Linux-specific icon wiring. SetIcon takes a single PNG; the
// AppIndicator host re-rasterises it to whatever size the panel
// asks for (typically 22x22 or 24x24, scaled up to 48x48 on
// HiDPI).
//
// Tagged `linux && gui` to match tray_gui.go: this file imports
// getlantern/systray, so leaving it on a bare `linux` tag would
// re-introduce the libayatana-appindicator3.so.1 link dependency
// into the default Linux build even though tray_gui.go is excluded.
// applyIcon has no caller in the no-tray build (trayOnReady is the
// only one), so there is no stub counterpart.

package main

import "github.com/getlantern/systray"

func applyIcon() {
	systray.SetIcon(IconLinux())
}
