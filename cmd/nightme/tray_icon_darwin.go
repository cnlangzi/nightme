//go:build darwin

// macOS-specific icon wiring. SetTemplateIcon takes the 1x, 2x,
// 3x PNGs we embed (see tray_assets.go) and registers them as a
// "template" image: Cocoa auto-inverts for dark menu bars and
// applies the right alpha mask.

package main

import "github.com/getlantern/systray"

func applyIcon() {
	one, two, three := IconDarwin()
	systray.SetTemplateIcon(one, two, three)
}
