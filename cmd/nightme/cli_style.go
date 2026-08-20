package main

import (
	"io"
	"strings"

	"github.com/cnlangzi/nightme/internal/version"
)

// Tiny ANSI helpers for the REPL update prompt. No colour
// library: the rest of the CLI (banner, etc.) stays
// dependency-free.
//
// Two output modes depending on what the host console can
// do:
//
//   - VT-enabled (Windows Terminal, PowerShell 7+,
//     Win10 1607+ cmd.exe, Git Bash MSYS pty, etc.):
//     paint() emits real SGR sequences — `▲` is yellow
//     bold, `✓` is green, etc.
//
//   - No-VT (legacy conhost, redirected to a file that
//     happens to be a *os.File, etc.): paint() emits the
//     SGR parameter as visible ASCII text — `[33m[1m▲[0m`
//     instead of `\x1b[33m\x1b[1m▲\x1b[0m`. The user
//     sees which style would have applied, rather than an
//     invisible ESC byte next to the character.
//
// styleEnabled is split per platform:
//   - cli_style_windows.go: probes ENABLE_VIRTUAL_TERMINAL_PROCESSING
//     via GetConsoleMode only (no SetConsoleMode — that
//     breaks readline's ConPTY setup).
//   - cli_style_unix.go:    isatty.IsTerminal ||
//     IsCygwinTerminal.

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
// styleEnabled returns true we emit the real ANSI sequence;
// when it returns false (no-VT host, NO_COLOR set, or
// non-*os.File writer) we strip the leading "\x1b[" so the
// code renders as visible ASCII text.
func paint(w io.Writer, code, s string) string {
	if s == "" {
		return s
	}
	if styleEnabled(w) {
		return code + s + ansiReset
	}
	return stripCSI(code) + s + stripCSI(ansiReset)
}

// stripCSI replaces every "\x1b[" with "[", turning a
// real CSI sequence into its visible-ASCII equivalent.
// "\x1b[33m" → "[33m"; "\x1b[33m\x1b[1m" → "[33m[1m".
func stripCSI(s string) string {
	return strings.ReplaceAll(s, "\x1b[", "[")
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