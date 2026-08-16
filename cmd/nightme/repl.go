// Package main — REPL mode for bare `nightme` invocation.
//
// Triggered when os.Args has length 1 (no subcommand). Reads stdin
// line-by-line and dispatches each line as if it were a normal cobra
// command invocation. Designed for the dev workflow where the user
// wants to poke at nightme without typing the binary name over and
// over.
//
// Design notes (per the doc-only v0.2 spec we sketched with Devin):
//   - Production uses chzyer/readline for line editing and history
//     (↑/↓ navigate, in-memory only — no on-disk persistence).
//   - Tests use a scanner-based path (runREPLWith) that injects an
//     io.Reader so the existing 9 unit tests stay simple and
//     independent of TTY.
//   - runREPL is the production entry; runREPLWith is the testable
//     core. Both share dispatchREPLLine for command execution.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/chzyer/readline"
	"github.com/spf13/cobra"

	nmerrors "github.com/cnlangzi/nightme/internal/errors"
)

// replBanner explains the available surface. Kept short so the
// first-time user can scan it in a glance and the returning user
// can ignore it. Updated to mention every registered subcommand
// name so /help is not needed for the common path.
//
// The banner is printed via the %s placeholder, which runREPLInteractive
// and runREPLWith fill with bannerWithVersion() (ASCII logo + the
// shared version.String() line). Tests pin substrings — see
// TestREPL_EOF for the substring contract.
const replBanner = `%s
Interactive shell. Type a command and press Enter.

Common:
  list            list sessions
  agents          list registered agents
  login feishu    QR Feishu registration
  test ...        spawn CLI in PTY (Ctrl-C to end)
  start           start daemon in the background
  status          show daemon status
  logs [--lines N] tail daemon log (Ctrl-C to exit follow)
  restart         gracefully replace daemon
  stop            gracefully stop daemon
  help            full command list
  version         version info
  update          check for a newer release

Shell:
  exit / quit     leave shell
  Ctrl-D          leave shell
  Ctrl-C at prompt cancels the current line

nightme> `

// runREPL is the no-args entry point invoked from Execute(). It
// blocks until the user exits via EOF, "exit"/"quit", or a fatal
// read error. Production path uses chzyer/readline for line editing
// and persistent history.
func runREPL(root *cobra.Command, logger *slog.Logger) error {
	return runREPLInteractive(root, logger)
}

// runREPLInteractive drives the REPL with chzyer/readline so the user
// gets ↑/↓ history navigation, in-line editing, and Ctrl-C handling.
// History is held in memory only — no on-disk persistence (per
// Devin's explicit ask: "history in memory is enough").
//
// Errors from readline (other than user-initiated EOF / interrupt)
// bubble up; the caller (Execute) prints them and exits with the
// appropriate error code.
func runREPLInteractive(root *cobra.Command, logger *slog.Logger) error {
	if logger != nil {
		logger.Info("repl started")
	}

	rl, err := readline.NewEx(&readline.Config{
		Prompt:            "nightme> ",
		InterruptPrompt:   "^C",
		HistorySearchFold: true,
		// No FuncFilterInputRune: chzyer/readline handles arrow
		// keys (\x1b[A / \x1b[B) and Ctrl-C at its own level; an
		// overzealous filter on our side would drop the leading
		// ESC of those sequences and break navigation.
	})
	if err != nil {
		return fmt.Errorf("readline init: %w", err)
	}
	defer func() { _ = rl.Close() }()

	out := rl.Stdout()
	fmt.Fprintf(out, replBanner, bannerWithVersion())

	// Version-check prompt: once per REPL startup. Best-effort;
	// any failure (network, missing cache, malformed input)
	// falls through silently so the user always ends up at the
	// interactive prompt.
	_ = promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Out:    out,
		Reader: rl.Readline,
		Logger: logger,
	})

	for {
		line, err := rl.Readline()
		switch {
		case errors.Is(err, readline.ErrInterrupt):
			// Ctrl-C at the prompt cancels the current line and
			// stays in the REPL. We don't print anything — the
			// terminal already echoed ^C.
			continue
		case errors.Is(err, io.EOF):
			// Ctrl-D — clean exit.
			fmt.Fprintln(out)
			return nil
		case err != nil:
			return err
		}

		done, err := dispatchREPLLine(root, logger, line, out)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// dispatchREPLLine is the per-line core shared between the
// production (readline) path and the test (scanner) path. Returns
// done=true when the user typed exit/quit; otherwise dispatches and
// returns done=false so the outer loop can read another line.
func dispatchREPLLine(root *cobra.Command, logger *slog.Logger, line string, out io.Writer) (bool, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		fmt.Fprint(out, "nightme> ")
		return false, nil
	}
	if line == "exit" || line == "quit" {
		fmt.Fprintln(out, "bye")
		return true, nil
	}

	tokens := strings.Fields(line)
	root.SetArgs(tokens)
	root.SetContext(withLogger(context.Background(), logger))
	// Route command output (cobra leaf's OutOrStdout) through the
	// REPL's writer so the banner / prompt / dispatch all share
	// the same destination. Tests inject a bytes.Buffer here.
	root.SetOut(out)
	root.SetErr(out)

	if err := root.Execute(); err != nil {
		code := nmerrors.ExitCode(err)
		fmt.Fprintf(out, "Error: %v (exit %d)\n", err, code)
	}
	fmt.Fprint(out, "nightme> ")
	return false, nil
}

// runREPLWith is the testable core. It uses bufio.Scanner on the
// supplied io.Reader so unit tests can drive the REPL without a TTY.
// Behavior matches runREPLInteractive for the cases the tests cover:
// EOF exits, exit/quit says "bye", unknown commands print "Error:".
//
// readline-specific behavior (↑/↓ history, line editing) is
// exercised in interactive testing; this path is the contract.
func runREPLWith(root *cobra.Command, logger *slog.Logger, in io.Reader, out io.Writer) error {
	if logger != nil {
		logger.Info("repl started")
	}

	fmt.Fprintf(out, replBanner, bannerWithVersion())

	// runREPLWith is the test-driven path; passing nil Reader
	// makes the prompt fall through to the "stdin unavailable"
	// branch, which is what the existing TestREPL_* suite
	// expects (no version-check chatter in the transcript).
	// The dedicated TestREPL_*VersionPrompt exercises the
	// real prompt path through the dedicated helper.
	_ = promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Out:    out,
		Logger: logger,
	})

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for {
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
				fmt.Fprintln(out, "Error:", err)
				return err
			}
			// Clean EOF — newline so the host prompt starts on
			// its own line.
			fmt.Fprintln(out)
			return nil
		}

		done, err := dispatchREPLLine(root, logger, scanner.Text(), out)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}
