package main

import (
	"runtime"
	"testing"
)

// TestIsHeadless_NonLinuxAlwaysFalse asserts the !linux stub
// returns false on every non-Linux build. On a Linux host this
// test is skipped — the linux-specific cases below cover that
// platform.
func TestIsHeadless_NonLinuxAlwaysFalse(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("non-linux stub: covered by TestIsHeadless_LinuxStrictRules on Linux hosts")
	}
	if isHeadless() {
		t.Errorf("isHeadless() = true on %s; non-linux stub must always return false", runtime.GOOS)
	}
}

// TestIsHeadless_LinuxStrictRules exercises every cell of the
// Linux detection truth table:
//
//	XDG_SESSION_TYPE  DISPLAY  WAYLAND_DISPLAY  →  isHeadless
//	------------------------------------------------------------
//	empty             empty    empty            →  true
//	"tty"             empty    empty            →  true
//	"tty"             ":0"     empty            →  true (tty wins)
//	"unspecified"     empty    empty            →  true (unspecified != GUI)
//	"x11"             empty    empty            →  false (trust session type)
//	"wayland"         empty    empty            →  false (trust session type)
//	empty             ":0"     empty            →  false
//	empty             empty    "wayland-0"      →  false
//
// t.Setenv is used so each case restores the process env on
// subtest exit — no manual t.Cleanup needed and no risk of
// leakage into sibling tests.
func TestIsHeadless_LinuxStrictRules(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only detection rules")
	}

	cases := []struct {
		name        string
		xdgType     string // "" means leave unset
		display     string
		waylandDisp string
		want        bool
	}{
		{"all empty", "", "", "", true},
		{"tty overrides empty", "tty", "", "", true},
		{"tty overrides display", "tty", ":0", "", true},
		{"unspecified with empty vars", "unspecified", "", "", true},
		{"x11 without display", "x11", "", "", false},
		{"wayland without wayland_display", "wayland", "", "", false},
		{"display only", "", ":0", "", false},
		{"wayland_display only", "", "", "wayland-0", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// t.Setenv restores on exit. An empty value
			// leaves the var set-but-empty in the
			// environment; os.Getenv returns "" for both
			// "set to empty" and "unset", so isHeadless
			// behaves the same either way.
			t.Setenv("XDG_SESSION_TYPE", tc.xdgType)
			t.Setenv("DISPLAY", tc.display)
			t.Setenv("WAYLAND_DISPLAY", tc.waylandDisp)

			if got := isHeadless(); got != tc.want {
				t.Errorf("isHeadless() = %v, want %v (xdg=%q display=%q wayland=%q)",
					got, tc.want, tc.xdgType, tc.display, tc.waylandDisp)
			}
		})
	}
}
