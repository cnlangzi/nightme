// Package main — REPL startup version-check prompt.
//
// runREPLInteractive and runREPLWith call
// promptForUpdateIfOutdated once, right after the banner prints.
// The function does the full "check + prompt + maybe-act"
// cycle in one shot so the caller doesn't have to thread a
// 4-return-value result through its loop.
//
// Design choices, in priority order:
//
//  1. NEVER block the REPL on a slow or unreachable
//     nightme.dev. The Checker swallows network errors and
//     falls back to the on-disk cache (see
//     internal/version/check.go). If that also fails we return
//     early — no prompt, no log spam.
//  2. NEVER prompt when the build is up to date.
//  3. NEVER prompt more than once per startup, even if the user
//     mistypes. One y/N read, then back to the main loop.
//  4. The "y" branch delegates to deps.OnYes. Production wires
//     this to "execute \`nightme update check\` against the
//     cobra tree"; tests wire it to a counter closure.
//  5. Read prompt uses the same writer as the banner so the
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
// real config file.
//
// Production callers pass nil for the optional fields and get
// defaults (Checker from version.DefaultChecker, OnYes runs
// `nightme update check`, Reader=nil so the prompt is silent
// for the scanner-based REPL path, etc.).
//
// tests pass:
//   - Checker: a Checker wired to an httptest server so we
//     can pin "latest = 9.9.9" without hitting nightme.dev.
//   - Reader: a function that returns the next y/N line.
//     Production code passes rl.Readline; tests pass a closure
//     over a bytes.Buffer so we don't need a TTY.
//   - OnYes: a closure that runs whatever the "user accepted"
//     should trigger. Production wires it to execute the
//     cobra `update` subcommand; tests wire it to a counter
//     so they can assert "y was accepted" without spinning up
//     a full cobra tree.
//   - Out: a bytes.Buffer so we can assert on the prompt text.
//   - Logger: nil is fine — we fall back to slog.Default().
type PromptDeps struct {
	Checker *version.Checker
	// Reader is the line-source used for the y/N reply. When
	// nil, promptForUpdateIfOutdated skips the prompt entirely
	// (the "no stdin" branch). Production callers wire this
	// to readline.Instance.Readline.
	Reader func() (string, error)
	// OnYes runs when the user accepts the prompt with y/yes.
	// Production wires it to dispatch the cobra `update`
	// subcommand; tests pass a counter closure. When nil and
	// the user says y, we fall back to printing the manual
	// install hint (the legacy pre-cobra behaviour) so the
	// prompt is never a no-op.
	OnYes func() error
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
// echoed answer, and (c) either (i) the result of deps.OnYes
// when the user accepts, or (ii) a "Run \`nightme update\`…"
// hint when they decline / EOF / error.
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

	// 6. Yes — invoke OnYes. Production wires this to the
	// cobra `update` subcommand so the user gets the same
	// "current vs latest + install instructions" output they
	// would get from `nightme update` in a fresh terminal.
	// When OnYes is nil (legacy / safety net) we fall back
	// to printing the manual install hint inline.
	fmt.Fprintln(out, "→ running `nightme update check`…")
	if deps.OnYes != nil {
		return deps.OnYes()
	}
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
// cache, which is fine (every startup just hits nightme.dev
// once).
func resolveDataDir() string {
	cfg, err := config.LoadDefault()
	if err != nil || cfg == nil {
		return ""
	}
	return cfg.Paths.DataDir
}