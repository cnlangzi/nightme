package main

import (
	"io"

	"github.com/cnlangzi/nightme/internal/version"
)

// Tiny ANSI helpers for the REPL update prompt. No colour
// library: the rest of the CLI (banner, etc.) stays
// dependency-free, and we honour NO_COLOR + non-TTY writers
// so tests see plain text.
//
// styleEnabled is split by platform — the implementation in
// this file is the cross-platform surface (constant table,
// paint(), helpers, displayVer, yesNoPrompt). The TTY probe
// lives in:
//   - cli_style_windows.go  (//go:build windows): VT-aware,
//     probes ENABLE_VIRTUAL_TERMINAL_PROCESSING and enables
//     it on demand. Falls back to plain text on legacy
//     conhost that can't be coerced into VT mode.
//   - cli_style_unix.go     (//go:build !windows): keeps the
//     original isatty.IsTerminal || IsCygwinTerminal probe.
//
// If Plan C (the VT-aware split) turns out to still misbehave
// on a particular Windows host, the trivial fallback is to
// inline `func styleEnabled(io.Writer) bool { return false }`
// here and delete the two platform files. paint() already
// treats a false return as "no ANSI", so tests stay green and
// the REPL update prompt falls back to plain Unicode glyphs
// (▲ ✓ ✗ →) which Windows console has rendered natively
// since NT 5.1.

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
