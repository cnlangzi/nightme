//go:build windows

package main

import "io"

// styleEnabled on Windows is hard-wired to false: the
// paint helpers always return their argument verbatim,
// producing no ANSI codes at all in the prompt output.
//
// This keeps the banner / y-N update prompt as plain
// Unicode glyphs (▲ ✓ ✗ →) that the Windows console
// renders natively without VT, so the prompt is legible
// on a classic cmd.exe whose output buffer lacks
// ENABLE_VIRTUAL_TERMINAL_PROCESSING. paint() never calls
// GetConsoleMode / SetConsoleMode: the abandoned Plan C
// did that from inside the paint helpers (many times per
// prompt) and the cadence of mode flips collided with
// readline's ConPTY / raw-mode setup, hanging the REPL.
// The VT decision now lives in readlineUsable() in
// repl_console_windows.go, which DETECTS (never enables)
// the VT bit and routes no-VT hosts to the scanner path —
// so this paint path stays trivial and never touches the
// console mode.
func styleEnabled(_ io.Writer) bool {
	return false
}
