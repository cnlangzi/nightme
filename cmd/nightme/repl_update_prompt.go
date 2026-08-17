// Package main — REPL startup version-check prompt.
//
// The REPL drives the same three internal stages as the bare
// `nightme update` CLI, but interactively: each stage gates
// on its own y/N. So:
//
//   check     → outdated?           → ask y/N
//   download  → ask y/N             → with progress + speed + ETA
//   install   → ask y/N             → swap binary + restart daemon
//
// The split lets the user:
//
//   - cancel mid-download with Ctrl-C (the download context
//     picks up SIGINT and removes the partial file).
//   - say N to install (skip the swap but keep the staged
//     archive for a later `nightme update` run — the
//     download-cache shortcut reuses it).
//
// We deliberately do NOT exec the new binary from inside the
// REPL. The chzyer/readline instance owns TTY state that
// doesn't survive exec; we restart the daemon (so a fresh
// shell sees the new daemon) and tell the user to exit +
// re-enter `nightme` to load the new binary.
//
// Design choices, in priority order:
//
//  1. NEVER block the REPL on a slow or unreachable
//     nightme.dev. The Checker swallows network errors and
//     falls back to the on-disk cache (see
//     internal/version/check.go).
//  2. NEVER prompt when the build is up to date.
//  3. Each stage prompt reads exactly one line. A stray
//     empty input or Ctrl-C ends THAT stage and falls
//     through to the next / back to the REPL. We never
//     re-prompt — the user gets one shot per stage.
//  4. The PromptDeps.Reader closure is the line source for
//     every y/N. Production wires it to rl.Readline; tests
//     wire it to a closure over a bytes.Buffer.
//  5. Each stage's progress / status output goes to
//     PromptDeps.Out so the REPL transcript stays
//     self-explanatory.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"

	"github.com/chzyer/readline"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/updater"
	"github.com/cnlangzi/nightme/internal/version"
)

// PromptDeps bundles the knobs tests need without dragging in a
// real config file.
//
//   - Reader: line source for every y/N. nil skips the
//     prompt entirely (runREPLWith's scanner path).
//   - Out:    progress + status lines. nil = discard.
//   - Logger: nil = slog.Default().
//
// tests inject a Reader closure over a bytes.Buffer so the
// y/N flow is fully reproducible.
type PromptDeps struct {
	Checker     *version.Checker
	CheckResult *updater.CheckResult
	Reader      func() (string, error)
	Out         io.Writer
	Logger      *slog.Logger
}

// promptForUpdateIfOutdated is the REPL startup hook. It runs
// the three stages interactively, gated by y/N between each.
// Returns nil on every path — even when the user declines
// install or hits Ctrl-C mid-download — so the REPL caller
// always ends up back at its main loop.
//
// Stages:
//
//	check → outdated?  → ask y/N
//	  y → download → progress / cancel-safe
//	    ok → install? → swap + restart daemon
//	    fail → fall through silently (user can re-run later)
//
// At most one y/N is read per stage. An EOF / Ctrl-C / read
// error ends the prompt entirely and returns to the REPL.
func promptForUpdateIfOutdated(ctx context.Context, deps *PromptDeps) error {
	if deps == nil {
		deps = &PromptDeps{}
	}

	out := deps.Out
	if out == nil {
		out = io.Discard
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Stage 1: check. We honour deps.CheckResult first (tests
	// inject it to pin a fake release + assets without
	// touching the network), then deps.Checker (production's
	// cached 24h check), and finally fall back to a fresh
	// live check against nightme.dev when neither is set.
	var latest string
	outdated := false
	var precomputed *updater.CheckResult
	switch {
	case deps.CheckResult != nil:
		precomputed = deps.CheckResult
		latest = precomputed.Latest
		outdated = precomputed.Outdated
	case deps.Checker != nil:
		res := deps.Checker.Check(ctx, version.Version, func(format string, args ...any) {
			logger.Warn(fmt.Sprintf(format, args...))
		})
		if res.Latest != "" {
			latest = res.Latest
			outdated = res.Outdated
		}
	default:
		c, _ := version.DefaultChecker(resolveDataDir())
		if c != nil {
			res := c.Check(ctx, version.Version, func(format string, args ...any) {
				logger.Warn(fmt.Sprintf(format, args...))
			})
			if res.Latest != "" {
				latest = res.Latest
				outdated = res.Outdated
			}
		}
	}
	if latest == "" || !outdated {
		return nil
	}

	// The scanner-based REPL path (runREPLWith) passes a
	// nil Reader so we don't leak prompt text into its
	// banner-substring contract. Honour that: bail out
	// silently when there's no line source.
	if deps.Reader == nil {
		return nil
	}

	fmt.Fprintf(out, "\n[!] nightme %s is available (you have %s).\n",
		latest, version.Version)
	if !askYesNo(out, deps.Reader, "Update now? [y/N]: ", false) {
		return nil
	}

	// Resolve config up front: the download + install
	// stages both need cfg.Paths.DataDir for staging.
	cfg, err := config.LoadDefault()
	if err != nil || cfg == nil || cfg.Paths.DataDir == "" {
		fmt.Fprintln(out,
			"  cfg.Paths.DataDir is empty; cannot stage a download.")
		fmt.Fprintln(out, "  Set data_dir in your config and run `nightme update`.")
		return nil
	}

	// Stage 2: download. We need the full Release (with its
	// Assets list) for asset matching. In production we
	// re-fetch from GitHub (one round trip; the version
	// string came from the cached Check earlier). Tests
	// inject precomputed so we skip the network.
	var checkRes *updater.CheckResult
	if precomputed != nil {
		checkRes = precomputed
	} else {
		checkRes, err = updater.Check(ctx, "")
		if err != nil {
			fmt.Fprintf(out, "  download failed (check): %v\n", err)
			return nil
		}
		if checkRes.Latest != latest {
			fmt.Fprintf(out, "  release moved during the prompt (%s → %s); aborting\n",
				latest, checkRes.Latest)
			return nil
		}
	}

	dl, err := runDownloadStage(ctx, deps, cfg, checkRes)
	if err != nil {
		fmt.Fprintf(out, "  download failed: %v\n", err)
		fmt.Fprintln(out, "  Run `nightme update` from a shell to retry.")
		return nil
	}

	// Stage 3: install — gated by another y/N.
	fmt.Fprintf(out, "\n[!] staged %s (%s, sha256=%s).\n",
		dl.Asset.Name, updater.FormatBytes(dl.Bytes), dl.SHA256Hex)
	if !askYesNo(out, deps.Reader, "Install now? [y/N]: ", false) {
		fmt.Fprintln(out, "  Run `nightme update` later to install.")
		return nil
	}

	if err := runInstallStage(ctx, deps, cfg, dl); err != nil {
		fmt.Fprintf(out, "  install failed: %v\n", err)
		return nil
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "✓ installed %s — exit this REPL and re-enter `nightme` to load the new binary.\n",
		latest)
	return nil
}

// promptCheckOnly has been removed: the prompt now handles
// the "DataDir is empty" case inline (degrades to a hint
// after the y/N answer). Keeping a separate helper would
// duplicate the check logic and risk the two paths drifting.

// runDownloadStage does the actual download with a
// cancellable context (Ctrl-C = ctx cancel). On success it
// returns the staged archive info so the install stage can
// reuse it. On failure it prints a one-line error and
// returns the error so the prompt falls through cleanly.
func runDownloadStage(
	ctx context.Context,
	deps *PromptDeps,
	cfg *config.Config,
	checkRes *updater.CheckResult,
) (*updater.DownloadResult, error) {
	out := deps.Out

	asset := updater.MatchAsset(checkRes.Release, checkRes.Latest)
	if asset == nil {
		return nil, fmt.Errorf("no release asset for %s/%s",
			runtime.GOOS, runtime.GOARCH)
	}

	stagingDir, err := updater.StagingDir(cfg.Paths.DataDir, checkRes.Latest)
	if err != nil {
		return nil, err
	}
	// "what / where from / where to" before the progress bar
	// so the user knows what's about to download. The bar
	// overwrites itself with \r; these lines stay put.
	fmt.Fprintln(out)
	fmt.Fprintf(out, "[→] downloading %s (%s)\n",
		asset.Name, updater.FormatBytes(asset.Size))
	fmt.Fprintf(out, "    from: %s\n", asset.BrowserDownloadURL)
	fmt.Fprintf(out, "    to:   %s\n", stagingDir)
	progress := updater.NewASCIIProgressBar(out, asset.Size)
	res, err := updater.Download(ctx, checkRes.Release, asset, stagingDir, progress)
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(out) // newline after the bar
	return res, nil
}

// runInstallStage extracts the staged archive and swaps the
// running binary. It also restarts the daemon (best-effort)
// so a fresh REPL / shell picks up the new daemon.
func runInstallStage(
	_ context.Context,
	deps *PromptDeps,
	cfg *config.Config,
	dl *updater.DownloadResult,
) error {
	out := deps.Out

	binary, err := updater.ExtractArchive(dl.StagingPath, filepathDir(dl.StagingPath))
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	target, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current binary: %w", err)
	}
	_, err = updater.Install(binary, target)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  ✓ installed %s\n", target)

	// Restart the daemon if one is running. We don't fail
	// the install if the daemon restart fails — the user
	// can `nightme restart` by hand.
	running, _ := daemonIsRunning(cfg)
	if running {
		fmt.Fprintln(out, "  → restarting daemon…")
		if err := runRestartInline(out); err != nil {
			fmt.Fprintf(out, "  warning: daemon restart failed: %v\n", err)
			fmt.Fprintln(out, "           run `nightme restart` manually.")
		} else {
			fmt.Fprintln(out, "  ✓ daemon restarted")
		}
	}
	return nil
}

// askYesNo writes prompt + reads one line. Returns true on
// y/yes, false on anything else (n / empty / EOF / Ctrl-C /
// read error). defaultYes flips the default: when true, a
// bare Enter is treated as "yes"; when false, as "no".
//
// We deliberately do NOT loop. A stray "?" ends the prompt
// (returns false) so the user is never trapped at startup.
//
// The line is echoed to out so the REPL transcript shows
// what the user typed — readline already echoes in raw
// mode, but the scanner-based path doesn't, so we echo
// defensively in both cases.
func askYesNo(out io.Writer, reader func() (string, error), prompt string, defaultYes bool) bool {
	if reader == nil {
		// No line source (test-only path) — return the
		// default so the prompt is fully silent.
		return defaultYes
	}
	fmt.Fprint(out, prompt)
	answer, err := reader()
	if err != nil {
		if errors.Is(err, readline.ErrInterrupt) {
			fmt.Fprintln(out, "^C")
			return false
		}
		if errors.Is(err, io.EOF) {
			// Ctrl-D without a reply — same as N, but
			// hint so the user knows how to retry later.
			fmt.Fprintln(out)
			fmt.Fprintln(out, "  Run `nightme update` whenever you're ready.")
			return false
		}
		fmt.Fprintf(out, "(read error: %v)\n", err)
		return false
	}
	if answer != "" {
		fmt.Fprintln(out, answer)
	}
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer == "" {
		return defaultYes
	}
	return answer == "y" || answer == "yes"
}

// filepathDir is a tiny shim that lets the install stage
// pass the archive's parent dir to ExtractArchive without
// importing path/filepath at the top level (avoids pulling
// in os.Stat twice in the same file).
func filepathDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}

// resolveDataDir returns cfg.Paths.DataDir or "" if config
// can't be loaded. Used by the live-check fallback in the
// prompt to wire the 24h cache. We don't surface the
// error because the prompt path is best-effort: a missing
// data dir means no cache, which is fine (every startup
// just hits nightme.dev once).
func resolveDataDir() string {
	cfg, err := config.LoadDefault()
	if err != nil || cfg == nil {
		return ""
	}
	return cfg.Paths.DataDir
}