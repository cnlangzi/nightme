//go:build windows

// Windows-specific icon wiring. SetIcon takes an .ico (or PNG,
// but the existing logo-32.ico we embed is the natural fit and
// matches what the taskbar already shows via the PE .rsrc
// section).

package main

import "github.com/getlantern/systray"

func applyIcon() {
	systray.SetIcon(IconWindows())
}
