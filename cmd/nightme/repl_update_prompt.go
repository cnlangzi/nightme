// Package main — REPL startup version-check prompt.
//
// Startup order (interactive TTY):
//
//	banner → check (countdown, 5s timeout) → Update now? → download
//	→ Install now? → readline shell
//
// Check is blocking but capped. Timeout / network failure /
// up-to-date is silent and we fall through to the shell.
// "Update now? y" starts the download immediately — there is
// no second Download? prompt. Install stays gated because
// swapping the binary is the destructive step.
//
// The split lets the user:
//
//   - cancel mid-download with Ctrl-C (the download context
//     picks up SIGINT and removes the partial file).
//   - say N to install (skip the swap but keep the staged
//     archive for a later `nightme update` run — the
//     download-cache shortcut reuses it).
//
// This whole prompt runs BEFORE readline takes the TTY, so
// we read y/N from cooked stdin and write to os.Stdout. Using
// rl.Printf / rl.Readline here used to leak ESC[6n replies
// (^[[row;colR) and reprint "nightme> " over the banner.
// After a successful install we re-exec the new binary so
// the shell the user lands in is the version they just
// installed (readline never owned the TTY, so exec is safe).
//
// Design choices, in priority order:
//
//  1. Version check is blocking with a 5s timeout and a
//     visible countdown. Timeout / errors skip silently.
//  2. NEVER prompt when the build is up to date.
//  3. Each stage prompt reads exactly one line. A stray
//     empty input or Ctrl-C ends THAT stage and falls
//     through to the shell. We never re-prompt.
//  4. The PromptDeps.Reader closure is the line source for
//     every y/N. Production wires cooked stdin; tests wire
//     a closure over a bytes.Buffer.
//  5. Each stage's progress / status output goes to
//     PromptDeps.Out so the transcript stays readable.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/reeflective/readline"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/updater"
	"github.com/cnlangzi/nightme/internal/version"
)

// updateCheckTimeout caps the startup version probe. Matches
// internal/version.httpTimeout so the countdown and the HTTP
// client agree on "give up after 5s".
const updateCheckTimeout = 5 * time.Second

// PromptDeps bundles the knobs tests need without dragging in a
// real config file.
//
//   - Reader: line source for every y/N. nil skips the
//     prompt entirely (runREPLWith's scanner path).
//   - Out:    progress + status lines. nil = discard.
//   - Logger: nil = slog.Default().
//   - VersionCheck: already-computed nightme.dev result
//     (production runs the countdown + Check, then passes
//     it in so we don't hit the network twice).
//   - CheckResult: GitHub release + assets (tests inject
//     this to skip the live fetch on the download stage).
//   - ReExecAfterInstall: production-only; after a successful
//     swap, re-exec the new binary so the user lands in the
//     new version's shell. Tests leave this false.
//
// tests inject a Reader closure over a bytes.Buffer so the
// y/N flow is fully reproducible.
type PromptDeps struct {
	Checker            *version.Checker
	VersionCheck       *version.CheckResult
	CheckResult        *updater.CheckResult
	Reader             func() (string, error)
	Out                io.Writer
	Logger             *slog.Logger
	ReExecAfterInstall bool
}

// promptForUpdateIfOutdated is the REPL startup hook. It runs
// AFTER the banner and the (already completed) version check.
// Returns nil on every path — even when the user declines
// install or hits Ctrl-C mid-download — so the caller always
// proceeds to the readline loop (unless ReExecAfterInstall
// execs a new process).
//
// Stages:
//
//	outdated?  → ask Update now? [y/N]
//	  y → download (no extra prompt) → progress / cancel-safe
//	    ok → Install now? → swap + restart daemon [+ re-exec]
//	    fail → fall through to the shell
//
// At most one y/N is read per remaining stage. An EOF / Ctrl-C
// / read error ends the prompt entirely.
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

	// Stage 1: check. Honour deps.CheckResult first (tests
	// inject a fake GitHub release + assets), then
	// deps.VersionCheck (production already ran the
	// countdown probe), then deps.Checker, and finally a
	// fresh live check when nothing is set.
	var latest string
	outdated := false
	var precomputed *updater.CheckResult
	switch {
	case deps.CheckResult != nil:
		precomputed = deps.CheckResult
		latest = precomputed.Latest
		outdated = precomputed.Outdated
	case deps.VersionCheck != nil:
		latest = deps.VersionCheck.Latest
		outdated = deps.VersionCheck.Outdated
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

	current := displayVer(version.Version)
	want := displayVer(latest)
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %s  Update available\n", paintYellow(out, "▲"))
	fmt.Fprintf(out, "     %s %s %s\n",
		paintDim(out, current),
		paintDim(out, "→"),
		paint(out, ansiBold+ansiGreen, want))
	fmt.Fprintln(out)
	if !askYesNo(out, deps.Reader, yesNoPrompt(out, "Update now?"), false) {
		return nil
	}

	// Resolve config up front: the download + install
	// stages both need cfg.Paths.DataDir for staging.
	cfg, err := config.LoadDefault()
	if err != nil || cfg == nil || cfg.Paths.DataDir == "" {
		fmt.Fprintf(out, "  %s  data_dir is empty; cannot stage a download.\n", paintRed(out, "✗"))
		fmt.Fprintln(out, "     Set data_dir in your config and run `nightme update`.")
		return nil
	}

	// Stage 2: download. We need the full Release (with its
	// Assets list) for asset matching. In production we
	// look up the advertised tag on GitHub (nightme.dev
	// reports "0.3.10", GitHub tags "v0.3.10" — Equal
	// treats those as the same release). Tests inject
	// precomputed so we skip the network.
	var checkRes *updater.CheckResult
	if precomputed != nil {
		checkRes = precomputed
	} else {
		tag := version.Tag(latest)
		checkRes, err = updater.Check(ctx, tag)
		if err != nil {
			checkRes, err = updater.Check(ctx, "")
		}
		if err != nil {
			fmt.Fprintf(out, "  %s  download failed (check): %v\n", paintRed(out, "✗"), err)
			return nil
		}
		if !version.Equal(checkRes.Latest, latest) {
			fmt.Fprintf(out, "  %s  release moved during the prompt (%s → %s); aborting\n",
				paintRed(out, "✗"), displayVer(latest), displayVer(checkRes.Latest))
			return nil
		}
	}

	dlCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
	dl, err := runDownloadStage(dlCtx, deps, cfg, checkRes)
	stop()
	if err != nil {
		fmt.Fprintf(out, "  %s  download failed: %v\n", paintRed(out, "✗"), err)
		fmt.Fprintln(out, "     Run `nightme update` from a shell to retry.")
		return nil
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %s  Staged %s  %s\n",
		paintGreen(out, "✓"),
		dl.Asset.Name,
		paintDim(out, updater.FormatBytes(dl.Bytes)+", sha256="+dl.SHA256Hex))
	if !askYesNo(out, deps.Reader, yesNoPrompt(out, "Install now?"), false) {
		fmt.Fprintln(out, "     Run `nightme update` later to install.")
		return nil
	}

	target, err := runInstallStage(ctx, deps, cfg, dl)
	if err != nil {
		fmt.Fprintf(out, "  %s  install failed: %v\n", paintRed(out, "✗"), err)
		return nil
	}
	fmt.Fprintln(out)
	if deps.ReExecAfterInstall {
		fmt.Fprintf(out, "  %s  Installed %s — restarting into the new binary.\n",
			paintGreen(out, "✓"), displayVer(latest))
		_ = execAndExit(out, target, []string{target})
		return nil
	}
	fmt.Fprintf(out, "  %s  Installed %s — exit and re-enter `nightme` to load the new binary.\n",
		paintGreen(out, "✓"), displayVer(latest))
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
	fmt.Fprintf(out, "  %s  %s  %s\n",
		paintCyan(out, "↓"),
		asset.Name,
		paintDim(out, updater.FormatBytes(asset.Size)))
	progress := updater.NewASCIIProgressBar(out, asset.Size)
	res, err := updater.Download(ctx, checkRes.Release, asset, stagingDir, progress)
	if err != nil {
		return nil, err
	}
	if res.Cached {
		fmt.Fprintf(out, "  %s  sha256 verified — skipping download\n", paintGreen(out, "✓"))
		return res, nil
	}
	fmt.Fprintln(out) // newline after the bar
	return res, nil
}

// runInstallStage extracts the staged archive and swaps the
// running binary. It also restarts the daemon (best-effort)
// so a fresh REPL / shell picks up the new daemon.
//
// Returns the path of the binary that Install wrote — i.e.
// the path the REPL was launched from BEFORE Install renamed
// the running inode aside. Callers need this string (NOT a
// fresh os.Executable() — which after Install follows the
// old inode to the .old sidecar) when they want to exec the
// new binary or hand it to a child process.
func runInstallStage(
	_ context.Context,
	deps *PromptDeps,
	cfg *config.Config,
	dl *updater.DownloadResult,
) (string, error) {
	out := deps.Out

	binary, err := updater.ExtractArchive(dl.StagingPath, filepath.Dir(dl.StagingPath))
	if err != nil {
		return "", fmt.Errorf("extract: %w", err)
	}
	target, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate current binary: %w", err)
	}
	_, err = updater.Install(binary, target)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(out, "  %s  installed %s\n", paintGreen(out, "✓"), target)

	running, _ := daemonIsRunning(cfg)
	if running {
		fmt.Fprintf(out, "  %s  restarting daemon…\n", paintDim(out, "→"))
		if err := runRestartInline(out, target); err != nil {
			fmt.Fprintf(out, "  %s  daemon restart failed: %v\n", paintYellow(out, "!"), err)
			fmt.Fprintln(out, "     run `nightme restart` manually.")
		} else {
			fmt.Fprintf(out, "  %s  daemon restarted\n", paintGreen(out, "✓"))
		}
	}
	return target, nil
}

// askYesNo writes prompt + reads one line. Returns true on
// y/yes, false on anything else (n / empty / EOF / Ctrl-C /
// read error). defaultYes flips the default: when true, a
// bare Enter is treated as "yes"; when false, as "no".
//
// We deliberately do NOT loop. A stray "?" ends the prompt
// (returns false) so the user is never trapped at startup.
//
// Cooked stdin already echoes what the user typed, so we
// do not reprint the answer (that doubled the line when
// this ran before readline). Tests assert on the prompt
// text, not on an echoed "y".
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
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer == "" {
		return defaultYes
	}
	return answer == "y" || answer == "yes"
}

// filepathDir was removed: its hand-rolled "/"-only scan broke on
// Windows backslash paths and tripped the Win32 ERROR_SHARING_VIOLATION
// when REPL extraction wrote to cwd/nightme.exe. The fix is to derive
// stagingDir via filepath.Dir(dl.StagingPath) at the use site.

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

// newStdinLineReader returns a one-line Reader over cooked
// os.Stdin. Used by the interactive startup prompt so y/N
// does not go through readline (which would reprint
// "nightme> " and probe the cursor).
func newStdinLineReader() func() (string, error) {
	in := bufio.NewReader(os.Stdin)
	return func() (string, error) {
		return in.ReadString('\n')
	}
}

// checkWithCountdown runs checker.Check while painting a
// "Checking for updates... Ns" line that ticks down once
// per second. The check is bounded by updateCheckTimeout:
// if Check has not returned by then we skip (empty result)
// and let the caller fall through to the shell.
//
// A cache hit returns immediately — the countdown is
// cleared as soon as Check returns, so a warm cache does
// not force a 5s wait.
func checkWithCountdown(
	ctx context.Context,
	out io.Writer,
	checker *version.Checker,
	currentVersion string,
	logf func(string, ...any),
) version.CheckResult {
	if checker == nil {
		return version.CheckResult{}
	}
	cctx, cancel := context.WithTimeout(ctx, updateCheckTimeout)
	defer cancel()

	ch := make(chan version.CheckResult, 1)
	go func() {
		ch <- checker.Check(cctx, currentVersion, logf)
	}()

	return waitCheckCountdown(cctx, out, int(updateCheckTimeout/time.Second), 80*time.Millisecond, ch)
}

// waitCheckCountdown is the ticker loop extracted so tests
// can drive it with a short interval and a pre-filled result
// channel without sleeping 5s.
func waitCheckCountdown(
	ctx context.Context,
	out io.Writer,
	seconds int,
	interval time.Duration,
	ch <-chan version.CheckResult,
) version.CheckResult {
	if seconds < 1 {
		seconds = 1
	}
	remaining := seconds
	writeCheckCountdown(out, remaining, 0)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	start := time.Now()

	for {
		select {
		case res := <-ch:
			clearCheckCountdown(out)
			return res
		case <-ctx.Done():
			clearCheckCountdown(out)
			select {
			case res := <-ch:
				return res
			default:
				return version.CheckResult{}
			}
		case now := <-ticker.C:
			elapsed := now.Sub(start)
			left := seconds - int(elapsed/time.Second)
			if left <= 0 {
				clearCheckCountdown(out)
				select {
				case res := <-ch:
					return res
				default:
					return version.CheckResult{}
				}
			}
			frame := 0
			if interval > 0 {
				frame = int(elapsed/interval) % len(spinnerFrames)
			}
			writeCheckCountdown(out, left, frame)
		}
	}
}

func writeCheckCountdown(out io.Writer, remaining int, frame int) {
	if out == nil {
		return
	}
	spin := ""
	if len(spinnerFrames) > 0 {
		spin = spinnerFrames[frame%len(spinnerFrames)] + " "
	}
	text := fmt.Sprintf("  %sChecking for updates... %ds", spin, remaining)
	fmt.Fprintf(out, "\r%s   ", paintDim(out, text))
}

func clearCheckCountdown(out io.Writer) {
	if out == nil {
		return
	}
	fmt.Fprintf(out, "\r%s\r", strings.Repeat(" ", 48))
}
