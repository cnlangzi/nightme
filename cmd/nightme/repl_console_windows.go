//go:build windows

// Package main — Windows console gating for the interactive REPL.
//
// reeflective/readline drives the console by emitting VT/ANSI CSI
// sequences (cursor hide/show, line clears, cursor moves) to
// os.Stdout on every refresh. The console output buffer must have
// ENABLE_VIRTUAL_TERMINAL_PROCESSING set for those to be
// interpreted rather than rendered as literal text.
//
// On Windows both states of a classic cmd.exe console are broken
// for reeflective:
//
//   - VT off (the cmd.exe default): the leading ESC (0x1B) is
//     stripped and the rest prints literally —
//     "nightme> [1 q[?25l[120Dnightme> [0m[0K[49m...". The line
//     editor floods the screen with garbage and is unusable.
//   - VT on (force-enabled via SetConsoleMode): readline hangs at
//     startup. It emits a cursor-style sequence (ESC[1 q →
//     blinking block) and a mis-targeted cursor move, lands the
//     blinking cursor on a banner line ("...at prompt cancels..."
//     or the y/N "Update now?" line), and blocks in its input loop
//     before "nightme> " is ever drawn. This is the "Plan C"
//     regression documented in commit 6d29c03 — re-confirmed here
//     by enabling VT once, just-in-time, with no paint-side
//     cadence: the hang reproduces regardless.
//
// So we DETECT VT support but never try to ENABLE it. Hosts that
// already speak VT (Windows Terminal, ConPTY, modern conhost with
// the bit on by default) get the full readline experience (line
// editing + ↑/↓ history). Classic cmd.exe (VT off) is routed to
// the scanner-based runREPLWith, which is reliable — at the cost
// of line editing on that one host. Restoring line editing on
// cmd.exe requires a Win32-native editor (ReadConsoleInputW +
// SetConsoleCursorPosition, no ANSI), tracked as a follow-up.
package main

import (
	"os"

	"github.com/mattn/go-isatty"
	"golang.org/x/sys/windows"
)

// readlineUsable reports whether reeflective/readline can safely
// drive this host's console. True only when stdin is a real
// terminal AND stdout's output console already has
// ENABLE_VIRTUAL_TERMINAL_PROCESSING set (we never set the bit
// ourselves — see the file doc for why). Non-console stdout
// (file/pipe/closed) → false → scanner path.
func readlineUsable() bool {
	if !isatty.IsTerminal(os.Stdin.Fd()) {
		return false
	}
	var mode uint32
	if err := windows.GetConsoleMode(windows.Handle(os.Stdout.Fd()), &mode); err != nil {
		// Not a console handle (file/pipe/closed) — scanner path.
		return false
	}
	return mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0
}
