// Package main — REPL mode for bare `nightme` invocation.
//
// Triggered when os.Args has length 1 (no subcommand). Reads stdin
// line-by-line and dispatches each line as if it were a normal cobra
// command invocation. Designed for the dev workflow where the user
// wants to poke at nightme without typing the binary name over and
// over.
//
// Design notes (per the doc-only v0.2 spec we sketched with Devin):
//   - Production uses github.com/reeflective/readline for line
//     editing and history (↑/↓ navigate, in-memory only — no
//     on-disk persistence). Swapped from chzyer/readline in
//     fix-erpl-on-windows because chzyer drops printable characters
//     on Windows when its ANSI/VT escape handling mis-fires (the
//     user-visible symptom was 'f' character going missing on
//     Windows consoles). reeflective/readline is a more mature
//     pure-Go library with proper Windows console VT support and
//     has stable tagged releases (we pin v1.3.0).
//   - Tests use a scanner-based path (runREPLWith) that injects an
//     io.Reader so the existing 9 unit tests stay simple and
//     independent of TTY.
//   - runREPL is the production entry; runREPLWith is the testable
//     core. Both share dispatchREPLLine for command execution.
//
// The REPL banner's "Common:" section is no longer hard-coded here.
// It is rendered from registry.entries by buildBanner, so adding a
// new subcommand is a single reg.add() call (see subcommand.go and
// root.go) — the cobra tree and the banner cannot drift apart.
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

	"github.com/mattn/go-isatty"
	"github.com/reeflective/readline"
	"github.com/spf13/cobra"

	nmerrors "github.com/cnlangzi/nightme/internal/errors"
)

// replBannerHeader is the part of the banner that does NOT depend on
// the registry. The "Common:" list itself comes from reg.banner(),
// spliced in between the header and the "Shell:" footer.
const replBannerHeader = `%s
Interactive shell. Type a command and press Enter.

Common:
`

const replBannerFooter = `
Shell:
  exit / quit     leave shell
  Ctrl-D          leave shell
  Ctrl-C at prompt cancels the current line

nightme> `

// buildBanner renders the full REPL banner: version line + intro +
// "Common:" entries from the registry + shell hints + trailing
// prompt. versionLine is the ASCII logo + version metadata returned
// by bannerWithVersion(). reg supplies the "Common:" section.
//
// Tests pin substrings of this output (see TestREPL_EOF) — the
// "Common:" header is preserved exactly, so adding new commands
// does not break the substring contract.
func buildBanner(versionLine string, reg *cmdRegistry) string {
	var b strings.Builder
	fmt.Fprintf(&b, replBannerHeader, versionLine)
	b.WriteString(reg.banner())
	b.WriteString(replBannerFooter)
	return b.String()
}

// shellWriter adapts reeflective/readline's Shell.Printf into an
// io.Writer so dispatchREPLLine (which is shared with the
// scanner-based test path that uses a bytes.Buffer) can stay
// io.Writer-agnostic.
//
// We use Printf("%s", string(p)) rather than Printf(string(p)) so
// that literal '%' bytes in the payload are not interpreted as
// fmt verbs. reeflective's Printf uses fmt.Sprintf under the hood,
// so this is correct: the format string is fixed ("%s"), the only
// variadic argument is the data.
type shellWriter struct{ rl *readline.Shell }

func (w shellWriter) Write(p []byte) (int, error) {
	return w.rl.Printf("%s", string(p))
}

// runREPL is the no-args entry point invoked from Execute(). It
// blocks until the user exits via EOF, "exit"/"quit", or a fatal
// read error. Production path uses reeflective/readline for line
// editing and in-memory history.
func runREPL(root *cobra.Command, reg *cmdRegistry, logger *slog.Logger) error {
	return runREPLInteractive(root, reg, logger)
}

// runREPLInteractive drives the REPL with reeflective/readline so
// the user gets ↑/↓ history navigation, in-line editing, and
// Ctrl-C / Ctrl-D handling. History is held in memory only — no
// on-disk persistence (per Devin's explicit ask: "history in
// memory is enough").
//
// Errors from readline (other than user-initiated EOF / interrupt)
// bubble up; the caller (Execute) prints them and exits with the
// appropriate error code.
//
// Note on reeflective specifics:
//   - NewShell() returns a fully configured *Shell with sensible
//     defaults; we only override the primary prompt.
//   - Shell.Readline() restores the terminal to cooked mode on
//     every return (Ctrl-C, Ctrl-D, or accepted line), so writes
//     through shellWriter in dispatchREPLLine happen in cooked mode
//     and the terminal stays sane across REPL turns.
//   - reeflective auto-restores terminal state on exit — no
//     Close() method to defer (unlike chzyer).
func runREPLInteractive(root *cobra.Command, reg *cmdRegistry, logger *slog.Logger) error {
	if logger != nil {
		logger.Info("repl started")
	}

	rl := readline.NewShell()
	rl.Prompt.Primary(func() string { return "nightme> " })

	out := shellWriter{rl: rl}

	// Banner goes below the (still-cooked) terminal before the
	// first Readline() takes over. shellWriter.Printf is designed
	// for inter-prompt messaging; calling it here with no prior
	// prompt is the same as fmt.Fprint to the raw terminal.
	if _, err := out.Write([]byte(buildBanner(bannerWithVersion(), reg))); err != nil {
		return fmt.Errorf("write banner: %w", err)
	}

	// Version-check prompt: once per REPL startup. The prompt
	// drives the three internal stages (check / download / install)
	// interactively, gating each on its own y/N. Ctrl-C mid-download
	// aborts cleanly; declining install keeps the staged archive
	// for a later `nightme update`. The REPL prompt never exec's
	// the new binary — we restart the daemon and tell the user
	// to exit + re-enter `nightme`.
	//
	// Non-TTY stdin (scripting, CI smoke tests) passes a nil
	// Reader so promptForUpdateIfOutdated falls through its
	// "no line source → be silent" branch. This is also the
	// correct behavior for piping — the prompt would deadlock
	// otherwise, since it blocks on a y/N that the pipe never
	// sends in any meaningful order. Without this guard,
	// reeflective/readline v1.3.0 raises "Incorrect function"
	// (Win32 ERROR_INVALID_FUNCTION) on Windows when stdin is a
	// pipe: it assumes a real console handle for ReadConsoleInput
	// and fails fast. The TTY check (vs hardcoding the env var)
	// means interactive users still get the prompt, and piped
	// users on every OS skip it cleanly.
	var reader func() (string, error)
	if isatty.IsTerminal(os.Stdin.Fd()) {
		reader = rl.Readline
	}
	_ = promptForUpdateIfOutdated(context.Background(), &PromptDeps{
		Out:    out,
		Reader: reader,
		Logger: logger,
	})

	for {
		line, err := rl.Readline()
		switch {
		case errors.Is(err, readline.ErrInterrupt):
			// Ctrl-C at the prompt cancels the current line and
			// stays in the REPL. We don't print anything — the
			// terminal already echoed ^C (reeflective handles this
			// natively, unlike chzyer which needed InterruptPrompt).
			continue
		case errors.Is(err, io.EOF):
			// Ctrl-D — clean exit. Trailing newline so the
			// host shell prompt starts on its own line (raw
			// mode leaves the cursor at end-of-line, not
			// beginning-of-new-line).
			_, _ = out.Write([]byte("\n"))
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
	// the same destination. Tests inject a bytes.Buffer here;
	// production passes a shellWriter that routes through readline.
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
func runREPLWith(root *cobra.Command, reg *cmdRegistry, logger *slog.Logger, in io.Reader, out io.Writer) error {
	if logger != nil {
		logger.Info("repl started")
	}

	fmt.Fprint(out, buildBanner(bannerWithVersion(), reg))

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