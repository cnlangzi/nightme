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
	"github.com/cnlangzi/nightme/internal/version"
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
`

// buildBanner renders the full REPL banner: version line + intro +
// "Common:" entries from the registry + shell hints. The trailing
// "nightme> " prompt is NOT part of the banner — the interactive
// path lets readline print it after the version-check prompt, and
// the scanner path (runREPLWith) prints it itself so tests keep a
// stable "banner then prompt" transcript.
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

// runREPL is the no-args entry point invoked from Execute(). It
// blocks until the user exits via EOF, "exit"/"quit", or a fatal
// read error. Production path uses reeflective/readline for line
// editing and in-memory history.
func runREPL(root *cobra.Command, reg *cmdRegistry, logger *slog.Logger) error {
	switch {
	case readlineUsable():
		// tty + VT (Windows Terminal / ConPTY / POSIX tty):
		// full readline — line editing, ↑/↓ history, Ctrl-C/D.
		return runREPLInteractive(root, reg, logger)
	case isatty.IsTerminal(os.Stdin.Fd()):
		// tty but no VT (classic cmd.exe): reeflective/readline
		// is unusable here (VT-off floods literal CSI; VT-on
		// hangs at startup), so fall back to the scanner-based
		// REPL — still interactive, still gets the startup
		// version check + "Update now? [y/N]" prompt. No
		// inline editing / history on this one host.
		return runREPLScanner(root, reg, logger)
	default:
		// non-tty (piped / redirected stdin): scanner, and skip
		// the version check so the first piped line is not eaten
		// as a y/N answer.
		return runREPLWith(root, reg, logger, os.Stdin, os.Stdout)
	}
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
//     every return. Command output and "bye" therefore go to
//     os.Stdout with fmt, NOT rl.Printf — Printf redisplays
//     "nightme> " and sends ESC[6n, which leaks as ^[[row;colR
//     once the process exits (the DSR reply is echoed by the
//     cooked tty).
//   - reeflective auto-restores terminal state on exit — no
//     Close() method to defer (unlike chzyer).
func runREPLInteractive(root *cobra.Command, reg *cmdRegistry, logger *slog.Logger) error {
	if logger != nil {
		logger.Info("repl started")
	}

	// Banner + version prompt happen on the still-cooked
	// terminal, BEFORE readline takes over. rl.Printf /
	// rl.Readline send an ESC[6n cursor-position probe and
	// redraw "nightme> " — using them here leaked ^[[row;colR
	// and tore the banner apart.
	if _, err := os.Stdout.Write([]byte(buildBanner(bannerWithVersion(), reg))); err != nil {
		return fmt.Errorf("write banner: %w", err)
	}

	// Blocking version check (HTTP timeout 5s) with a visible
	// countdown. Timeout / network failure / up-to-date → skip
	// silently. Outdated → Update now? [y/N] then download then
	// Install now? [y/N], all via plain stdin/stdout. Then we
	// construct the readline shell.
	//
	// Startup version check + "Update now? [y/N]" runs on the
	// still-cooked terminal, BEFORE readline takes over. Shared
	// with runREPLScanner (the no-VT tty path) so classic cmd.exe
	// gets the same update flow as Windows Terminal. The helper
	// writes a 5s countdown line then, if a newer release is
	// cached/found, prompts y/N and reads one cooked-stdin line.
	// Neither step needs VT: the glyphs (▲ ✓ ✗) render natively
	// and the y/N reads cooked stdin. We do NOT call
	// SetConsoleMode here — the host either already has VT
	// (readline works) or runREPL routed it to the scanner path.
	runStartupUpdateCheck(os.Stdout, logger, newStdinLineReader())

	rl := readline.NewShell()
	rl.Prompt.Primary(func() string { return "nightme> " })

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
			fmt.Fprintln(os.Stdout)
			return nil
		case err != nil:
			return err
		}

		// printPrompt=false: the next Readline() paints
		// "nightme> ". Printing it here via rl.Printf would
		// probe the cursor and leak ^[[row;colR on exit.
		done, err := dispatchREPLLine(root, logger, line, os.Stdout, false)
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
func dispatchREPLLine(root *cobra.Command, logger *slog.Logger, line string, out io.Writer, printPrompt bool) (bool, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		if printPrompt {
			fmt.Fprint(out, "nightme> ")
		}
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
	// the interactive path uses os.Stdout (cooked, after
	// Readline returns).
	root.SetOut(out)
	root.SetErr(out)

	if err := root.Execute(); err != nil {
		code := nmerrors.ExitCode(err)
		fmt.Fprintf(out, "Error: %v (exit %d)\n", err, code)
	}
	if printPrompt {
		fmt.Fprint(out, "nightme> ")
	}
	return false, nil
}

// runREPLWith is the test / non-tty core: bufio.Scanner on the
// supplied io.Reader so unit tests can drive the REPL without a TTY
// and so piped-stdin invocations (echo "exit" | nightme) work. It
// deliberately skips the startup version check — test transcripts
// stay free of countdown + y-N chatter, and a piped first line is
// not eaten as a y/N answer. Interactive no-VT ttys (classic cmd.exe)
// go through runREPLScanner instead, which runs the version check
// between the banner and the first prompt. Behavior matches
// runREPLInteractive for the cases the tests cover: EOF exits,
// exit/quit says "bye", unknown commands print "Error:".
//
// readline-specific behavior (↑/↓ history, line editing) is
// exercised in interactive testing; this path is the contract.
func runREPLWith(root *cobra.Command, reg *cmdRegistry, logger *slog.Logger, in io.Reader, out io.Writer) error {
	if logger != nil {
		logger.Info("repl started")
	}

	fmt.Fprint(out, buildBanner(bannerWithVersion(), reg))
	fmt.Fprint(out, "nightme> ")

	return scanREPLLoop(root, logger, in, out)
}

// runStartupUpdateCheck runs the blocking version probe (5s
// countdown written to out) and, if a newer release is cached or
// found, the "Update now? [y/N]" prompt + download/install flow.
// Shared by runREPLInteractive (readline path) and runREPLScanner
// (no-VT tty path) so both get the same startup update experience.
//
// reader is the cooked-stdin line source for the y/N answer
// (newStdinLineReader in production); pass nil to skip the y/N.
//
// This must run on a COOKED terminal: it writes a countdown line
// and reads a y/N line via plain stdin/stdout, neither of which
// tolerates raw mode. On the readline path it is called before
// readline.NewShell(); on the scanner path stdin is always cooked.
func runStartupUpdateCheck(out io.Writer, logger *slog.Logger, reader func() (string, error)) {
	ctx := context.Background()
	logf := func(format string, args ...any) {
		if logger != nil {
			logger.Warn(fmt.Sprintf(format, args...))
		}
	}
	checker, _ := version.DefaultChecker(resolveDataDir())
	res := checkWithCountdown(ctx, out, checker, version.Version, logf)
	_ = promptForUpdateIfOutdated(ctx, &PromptDeps{
		VersionCheck:       &res,
		Out:                out,
		Reader:             reader,
		Logger:             logger,
		ReExecAfterInstall: true,
	})
}

// scanREPLLoop is the shared scanner core: read lines from in,
// dispatch each via dispatchREPLLine (which reprints "nightme> "
// after every command), and return on clean EOF, exit/quit, or a
// read/dispatch error. Used by both the test / non-tty path
// (runREPLWith) and the production no-VT tty path (runREPLScanner).
func scanREPLLoop(root *cobra.Command, logger *slog.Logger, in io.Reader, out io.Writer) error {
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

		done, err := dispatchREPLLine(root, logger, scanner.Text(), out, true)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// runREPLScanner is the production REPL for a real tty that
// reeflective/readline can't drive — i.e. a classic cmd.exe whose
// output console lacks ENABLE_VIRTUAL_TERMINAL_PROCESSING (VT-off
// floods literal CSI; VT-on hangs the library — see
// repl_console_windows.go). It reuses the scanner core (scanREPLLoop)
// but inserts the startup version check + "Update now? [y/N]" flow
// between the banner and the first "nightme> " prompt, so cmd.exe
// users get the same update experience as Windows Terminal users.
//
// History / inline line-editing are NOT available on this path —
// only readline (runREPLInteractive) provides those, and only on
// VT-capable hosts. Restoring them on cmd.exe needs a Win32-native
// editor (ReadConsoleInputW + SetConsoleCursorPosition, no ANSI).
func runREPLScanner(root *cobra.Command, reg *cmdRegistry, logger *slog.Logger) error {
	if logger != nil {
		logger.Info("repl started")
	}

	out := os.Stdout
	fmt.Fprint(out, buildBanner(bannerWithVersion(), reg))
	runStartupUpdateCheck(out, logger, newStdinLineReader())
	fmt.Fprint(out, "nightme> ")

	return scanREPLLoop(root, logger, os.Stdin, out)
}
