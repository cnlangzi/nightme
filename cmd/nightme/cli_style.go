package main

import (
	"io"
	"os"

	"github.com/mattn/go-isatty"

	"github.com/cnlangzi/nightme/internal/version"
)

// Tiny ANSI helpers for the REPL update prompt. No colour
// library: the rest of the CLI (banner, etc.) stays
// dependency-free, and we honour NO_COLOR + non-TTY writers
// so tests see plain text.

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

func styleEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fd := f.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
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
