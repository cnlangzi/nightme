//go:build linux

// Linux-specific icon wiring. SetIcon takes a single PNG; the
// AppIndicator host re-rasterises it to whatever size the panel
// asks for (typically 22x22 or 24x24, scaled up to 48x48 on
// HiDPI).

package main

import "github.com/getlantern/systray"

func applyIcon() {
	systray.SetIcon(IconLinux())
}
