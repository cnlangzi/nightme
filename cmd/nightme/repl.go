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
	if isatty.IsTerminal(os.Stdin.Fd()) {
		return runREPLInteractive(root, reg, logger)
	}
	return runREPLWith(root, reg, logger, os.Stdin, os.Stdout)
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
	// The styled prompt path keeps the console in its original
	// mode on Windows — paint() falls back to visible ASCII
	// ("[33m[1m▲[0m") instead of real ANSI so the prompt
	// stays readable on hosts that don't have VT on by default.
	// VT is enabled later, JUST before readline takes over, so
	// readline's CSI sequences are interpreted by the terminal
	// instead of being rendered as literal escape text.
	if isatty.IsTerminal(os.Stdin.Fd()) {
		ctx := context.Background()
		logf := func(format string, args ...any) {
			if logger != nil {
				logger.Warn(fmt.Sprintf(format, args...))
			}
		}
		checker, _ := version.DefaultChecker(resolveDataDir())
		res := checkWithCountdown(ctx, os.Stdout, checker, version.Version, logf)
		_ = promptForUpdateIfOutdated(ctx, &PromptDeps{
			VersionCheck:       &res,
			Out:                os.Stdout,
			Reader:             newStdinLineReader(),
			Logger:             logger,
			ReExecAfterInstall: true,
		})
	}

	// Enable VT for readline's lifetime. readline sends CSI
	// sequences (cursor positioning, screen clear, hide/show
	// cursor) that need VT processing to be interpreted.
	// We restore the original mode when runREPLInteractive
	// returns — using defer so a panic during readline
	// initialisation still resets the console.
	savedMode, vtOK := saveAndEnableVT(os.Stdout)
	defer func() {
		if vtOK {
			restoreConsoleMode(os.Stdout, savedMode)
		}
	}()

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
	fmt.Fprint(out, "nightme> ")

	// runREPLWith is the test-driven path; passing nil Reader
	// makes the prompt fall through to the "stdin unavailable"
	// branch, which is what the existing TestREPL_* suite
	// expects (no version-check chatter in the transcript).
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

		done, err := dispatchREPLLine(root, logger, scanner.Text(), out, true)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}
