package main

import (
	"io"

	"github.com/cnlangzi/nightme/internal/version"
)

// Tiny ANSI helpers for the REPL update prompt. No colour
// library: the rest of the CLI (banner, etc.) stays
// dependency-free.
//
// styleEnabled is hard-wired to false. Plan C (the VT-aware
// platform split in cli_style_unix.go / cli_style_windows.go)
// was tried first; the `SetConsoleMode` call that probes for
// `ENABLE_VIRTUAL_TERMINAL_PROCESSING` left the host console
// in a state that broke readline's ConPTY / raw-mode setup on
// the user's machine — the REPL hung at the y/N prompt (the
// prompt painted, but `readline.NewShell().Readline()` never
// produced a `nightme> ` prompt).
//
// Returning false here makes paint() the identity function,
// so every helper (paintRed / Green / Yellow / Cyan / Dim)
// returns its argument verbatim. The REPL update prompt
// falls back to plain Unicode glyphs (▲ ✓ ✗ →) that Windows
// console has rendered natively since NT 5.1. Users keep the
// symbol; they lose the colour. Trade accepted.
//
// To restore colour: re-introduce cli_style_unix.go and
// cli_style_windows.go with the GetConsoleMode probe only
// (drop the SetConsoleMode call), then change styleEnabled to
// dispatch by runtime.GOOS. That variant is in git history at
// commit 9c258f8.

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

// styleEnabled is the fallback path — always plain. The
// plan-C VT-aware split is preserved in git history for a
// future attempt that doesn't touch the host console mode.
func styleEnabled(w io.Writer) bool {
	_ = w
	return false
}

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