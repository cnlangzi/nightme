// Package main — REPL startup version-check prompt.
//
// runREPLInteractive and runREPLWith call
// promptForUpdateIfOutdated once, right after the banner prints.
// The function does the full "check + prompt + maybe-print-
// instructions" cycle in one shot so the caller doesn't have to
// thread a 4-return-value result through its loop.
//
// Design choices, in priority order:
//
//  1. NEVER block the REPL on a slow or unreachable GitHub.
//     The Checker swallows network errors and falls back to the
//     on-disk cache (see internal/version/check.go). If that
//     also fails we return early — no prompt, no log spam.
//  2. NEVER prompt when the build is up to date.
//  3. NEVER prompt more than once per startup, even if the user
//     mistypes. One y/N read, then back to the main loop. We
//     considered looping until the user typed y/n explicitly,
//     but a stray space or "?" shouldn't trap them at the
//     startup prompt.
//  4. Read prompt uses the same writer as the banner so the
//     "nightme> " prompt and the prompt text stay aligned in
//     the same terminal session.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/chzyer/readline"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/version"
)

// PromptDeps bundles the knobs tests need without dragging in a
// real config file. Production callers pass nil and get
// promptForUpdateIfOutdated's default (which loads the
// DataDir and constructs a version.DefaultChecker).
//
// tests pass:
//   - Checker: a Checker wired to an httptest server so we
//     can pin "latest = 9.9.9" without hitting GitHub.
//   - Reader: a function that returns the next y/N line.
//     Production code passes rl.Readline; tests pass a
//     closure over a bytes.Buffer so we don't need a TTY.
//   - Out: a bytes.Buffer so we can assert on the prompt text.
//   - Logger: nil is fine — we fall back to slog.Default().
type PromptDeps struct {
	Checker *version.Checker
	// Reader is the line-source used for the y/N reply. When
	// nil, promptForUpdateIfOutdated skips the prompt entirely
	// (the "no stdin" branch). Production callers must wire
	// this to readline.Instance.Readline; tests wire it to
	// bufio.Reader.ReadString('\n') over an in-memory buffer.
	Reader func() (string, error)
	Out    io.Writer
	Logger *slog.Logger
}

// promptForUpdateIfOutdated is the single-shot startup hook.
// Returns nil on any failure path (network down, empty cache,
// user typed anything other than y/Y, EOF, etc.) so the REPL
// caller can safely ignore the error and proceed to the
// interactive loop.
//
// On success it has printed (a) the prompt, (b) the user's
// echoed answer (so the REPL transcript shows what they
// typed), and (c) either a "Run `nightme update`…" hint or
// the manual-install instructions.
func promptForUpdateIfOutdated(ctx context.Context, deps *PromptDeps) error {
	if deps == nil {
		deps = &PromptDeps{}
	}

	// 1. Resolve the checker.
	checker := deps.Checker
	if checker == nil {
		c, _ := version.DefaultChecker(resolveDataDir())
		checker = c
	}
	if checker == nil {
		return nil // defensive only
	}

	out := deps.Out
	if out == nil {
		out = io.Discard
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// 2. Run the check. logf swallows the noise — we only log
	// on errors, never echo to the user.
	res := checker.Check(ctx, version.Version, func(format string, args ...any) {
		logger.Warn(fmt.Sprintf(format, args...))
	})

	// 3. Up to date or unknown → silently proceed.
	if !res.Outdated || res.Latest == "" {
		return nil
	}

	// 4. Read one y/N reply. We deliberately check
	// deps.Reader == nil BEFORE printing the prompt so the
	// "no reader" branch is fully silent (the scanner-based
	// REPL path uses a nil reader; we don't want extra text
	// to leak into its transcript and break the existing
	// TestREPL_* banner-substring assertions).
	if deps.Reader == nil {
		// Production callers (runREPLInteractive) always
		// wire a Reader via rl.Readline, so this branch is
		// only hit by runREPLWith's scanner path. Honour
		// "never block, never surprise the transcript":
		// return silently.
		return nil
	}

	// 5. Outdated → print the prompt.
	fmt.Fprintf(out,
		"\n[!] nightme %s is available (you have %s). Update now? [y/N]: ",
		res.Latest, version.Version)
	answer, err := deps.Reader()
	if err != nil && !errors.Is(err, io.EOF) && answer == "" {
		// readline.ErrInterrupt (Ctrl-C) is reported as an
		// error with empty answer; treat it as "skip the
		// prompt, get me back to the shell".
		if errors.Is(err, readline.ErrInterrupt) {
			fmt.Fprintln(out, "^C")
			return nil
		}
		fmt.Fprintf(out, "n (read error: %v)\n", err)
		fmt.Fprintln(out, "  Run `nightme update` whenever you're ready.")
		return nil
	}
	// Echo what the user typed so the REPL transcript stays
	// self-explanatory. Readline already echoes in raw mode;
	// the scanner path does not, so we echo defensively.
	if answer != "" {
		fmt.Fprintln(out, answer)
	}
	if answer == "" {
		// EOF / Ctrl-D without a reply — same as N.
		fmt.Fprintln(out, "  Run `nightme update` whenever you're ready.")
		return nil
	}
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "y" && answer != "yes" {
		return nil
	}

	// 6. Yes — print the manual install hint. The actual
	// download / replace flow lands in a follow-up commit; for
	// now we point the user at the same instructions
	// `nightme update` would print.
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Automatic self-update is not implemented yet. To upgrade:")
	fmt.Fprintln(out, "  go install github.com/cnlangzi/nightme/cmd/nightme@latest")
	fmt.Fprintln(out, "Or download a binary release from:")
	fmt.Fprintln(out, "  https://github.com/cnlangzi/nightme/releases/latest")
	return nil
}

// resolveDataDir returns cfg.Paths.DataDir or "" if config
// can't be loaded. We don't surface the error because the
// prompt path is best-effort: a missing data dir means no
// cache, which is fine (every startup just hits GitHub once).
func resolveDataDir() string {
	cfg, err := config.LoadDefault()
	if err != nil || cfg == nil {
		return ""
	}
	return cfg.Paths.DataDir
}