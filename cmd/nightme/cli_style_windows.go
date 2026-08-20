//go:build windows

package main

import "io"

// styleEnabled on Windows is hard-wired to false: the
// paint helpers always return their argument verbatim,
// producing no ANSI codes at all in the prompt output.
//
// This is the cleanest possible "don't touch the terminal"
// path. No GetConsoleMode, no SetConsoleMode, no platform
// probing — paint just emits plain text and we let
// readline do whatever it does next.
//
// If readline's CSI sequences get rendered as literal
// text on a particular host (because that host doesn't
// have ENABLE_VIRTUAL_TERMINAL_PROCESSING), the
// investigation moves to the REPL mechanism itself
// (scanner-based runREPLWith instead of readline-driven
// runREPLInteractive) rather than to the prompt's output
// format.
func styleEnabled(_ io.Writer) bool {
	return false
}