package main

import (
	"io"

	"github.com/cnlangzi/nightme/internal/version"
)

// Tiny ANSI helpers for the REPL update prompt. No colour
// library: the rest of the CLI (banner, etc.) stays
// dependency-free.
//
// paint() either wraps s with an SGR sequence (when
// styleEnabled returns true) or returns s verbatim (when
// it returns false). On Windows styleEnabled is hard-wired
// to false — no console mode probing, no SetConsoleMode,
// no visible "[33m" codes either. We emit pure plain text
// and let readline do whatever it does next; if readline's
// CSI sequences render as literal text on a particular
// host (because that host doesn't have
// ENABLE_VIRTUAL_TERMINAL_PROCESSING), the fix has to move
// to the REPL mechanism (scanner-based runREPLWith instead
// of readline-driven runREPLInteractive), not to the
// prompt's output format.

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// paint wraps s with the given SGR code + reset. When
// styleEnabled returns false (Windows hosts, NO_COLOR set,
// non-*os.File writer) it returns s verbatim — pure plain
// text, no codes at all.
func paint(w io.Writer, code, s string) string {
	if s == "" || !styleEnabled(w) {
		return s
	}
	return code + s + ansiReset
}

func paintDim(w io.Writer, s string) string    { return paint(w, ansiDim, s) }
func paintRed(w io.Writer, s string) string    { return paint(w, ansiRed, s) }
func paintGreen(w io.Writer, s string) string  { return paint(w, ansiGreen, s) }
func paintYellow(w io.Writer, s string) string { return paint(w, ansiYellow+ansiBold, s) }
func paintCyan(w io.Writer, s string) string   { return paint(w, ansiCyan, s) }

// displayVer strips a leading "v" so 0.3.10 and v0.3.10
// render the same in the UI.
func displayVer(v string) string {
	return version.Normalize(v)
}

func yesNoPrompt(w io.Writer, question string) string {
	return "  " + paintCyan(w, "?") + "  " + question + " [y/N] "
}