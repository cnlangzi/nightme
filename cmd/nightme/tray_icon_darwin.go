//go:build darwin && !notray

// macOS-specific icon wiring. SetTemplateIcon takes a single
// template (alpha-only mask; Cocoa auto-inverts for dark menu
// bars) and a regular fallback (used on hosts that don't honour
// the template flag). We embed both via tray_assets.go.

package main

import "github.com/getlantern/systray"

func applyIcon() {
	template, regular := IconDarwin()
	systray.SetTemplateIcon(template, regular)
}
