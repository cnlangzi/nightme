// Package main — REPL mode for bare `nightme` invocation.
//
// Triggered when os.Args has length 1 (no subcommand). Reads stdin
// line-by-line and dispatches each line as if it were a normal cobra
// command invocation. Designed for the dev workflow where the user
// wants to poke at nightme without typing the binary name over and
// over.
//
// Design notes (per the doc-only v0.2 spec we sketched with Devin):
//   - 0 deps: bufio.Scanner is enough; we trade fancy readline
//     features (history, tab completion) for a hot-fix-friendly
//     patch. Add chzyer/readline later when REPL becomes a daily
//     driver.
//   - blocking commands (run, test) work; first Ctrl-C ends the
//     session and returns to the prompt. Hitting Ctrl-C a second
//     time exits the REPL (test.go has a force-exit on second
//     signal).
//   - Ctrl-D (EOF on stdin) cleanly exits. Same for "exit" / "quit".
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	nmerrors "github.com/cnlangzi/nightme/internal/errors"
	"github.com/cnlangzi/nightme/internal/version"
)

// replBanner explains the available surface. Kept short so the
// first-time user can scan it in a glance and the returning user
// can ignore it. Updated to mention every registered subcommand
// name so /help is not needed for the common path.
const replBanner = `nightme %s (commit: %s, built: %s)
Interactive shell. Type a command and press Enter.

Common:
  list            list sessions
  agents          list registered agents
  auth status     show channel credentials
  auth login      QR Feishu registration
  test ...        spawn CLI in PTY (Ctrl-C to end)
  run             start daemon (Ctrl-C to end)
  help            full command list
  version         version info

Shell:
  exit / quit     leave shell
  Ctrl-D          leave shell
  Ctrl-C at prompt exits the REPL

nightme> `

// runREPL is the no-args entry point invoked from Execute(). It
// blocks until the user exits via EOF, "exit"/"quit", or a fatal
// read error.
//
// root is the already-constructed cobra root command. logger is the
// global slog logger (for the "command started" line and any
// internal errors); all user-visible output (banner, prompt,
// dispatched command stdout) goes to root.OutOrStdout() so tests
// can capture it via root.SetOut(&buf).
//
// runREPL never returns a non-nil error for ordinary user mistakes
// (unknown command, syntax error). It only returns an error when
// stdin itself becomes unreadable, so the caller can propagate.
func runREPL(root *cobra.Command, logger *slog.Logger) error {
	return runREPLWith(root, logger, os.Stdin)
}

// runREPLWith is the testable core. It separates I/O from the loop
// so unit tests can drive the REPL with a strings.Reader and
// capture output via root.SetOut(&buf).
//
// Output always flows through root.OutOrStdout(): banner, prompt,
// and dispatched command stdout all share the same writer. This is
// the same writer a leaf command would use (Cobra walks up to the
// root when the leaf has no own outWriter), so the REPL and a
// dispatched command never produce split output.
func runREPLWith(root *cobra.Command, logger *slog.Logger, in io.Reader) error {
	if logger != nil {
		logger.Info("repl started")
	}

	out := root.OutOrStdout()
	if out == nil {
		out = os.Stdout
	}

	fmt.Fprintf(out, replBanner, version.Version, version.GitCommit, version.BuildDate)

	scanner := bufio.NewScanner(in)
	// Allow long lines (e.g. pasting a big command). 64 KiB is a
	// conservative ceiling; anything larger is almost certainly
	// accidental.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for {
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
				fmt.Fprintln(out, "Error:", err)
				return err
			}
			// Clean EOF — print a newline so the shell prompt
			// starts on its own line.
			fmt.Fprintln(out)
			return nil
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			fmt.Fprint(out, "nightme> ")
			continue
		}
		if line == "exit" || line == "quit" {
			fmt.Fprintln(out, "bye")
			return nil
		}

		tokens := strings.Fields(line)
		// Reset args and context for every iteration so a previous
		// dispatch does not leak state.
		root.SetArgs(tokens)
		root.SetContext(withLogger(context.Background(), logger))

		if err := root.Execute(); err != nil {
			// Root has SilenceErrors:true so cobra itself is quiet.
			// Surface the error inline so the user can correct
			// the typo without leaving the REPL.
			code := nmerrors.ExitCode(err)
			fmt.Fprintf(out, "Error: %v (exit %d)\n", err, code)
		}
		// Re-print the prompt after each command. Using a fresh
		// line keeps the output readable when a command prints
		// without a trailing newline.
		fmt.Fprint(out, "nightme> ")
	}
}