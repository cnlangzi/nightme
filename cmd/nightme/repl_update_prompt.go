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
//   - VersionCheck: pre-computed version-check result. Production
//     runs the countdown + Check, then passes it in so the
//     prompt doesn't hit the network twice. Tests inject a
//     manually-built result (call *version.Checker.Check
//     yourself and pass the result here). nil means "do a
//     fresh live check via the production checker" — only
//     useful for callers that don't already have a result.
//   - Reader: line source for every y/N. nil skips the
//     prompt entirely (runREPLWith's scanner path).
//   - Out:    progress + status lines. nil = discard.
//   - Logger: nil = slog.Default().
//   - ReExecAfterInstall: production-only; after a successful
//     swap, re-exec the new binary so the user lands in the
//     new version's shell. Tests leave this false.
//
// tests inject a Reader closure over a bytes.Buffer so the
// y/N flow is fully reproducible.
type PromptDeps struct {
	VersionCheck       *version.CheckResult
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

	// Stage 1: check. Use the pre-computed VersionCheck when
	// present (production); fall through to a fresh live
	// check otherwise. Tests build the CheckResult themselves
	// and inject it via VersionCheck.
	logf := func(format string, args ...any) {
		logger.Warn(fmt.Sprintf(format, args...))
	}
	var (
		latest   string
		outdated bool
	)
	if deps.VersionCheck != nil {
		latest = deps.VersionCheck.Latest
		outdated = deps.VersionCheck.Outdated
	} else {
		c, _ := wiredChecker(resolveDataDir())
		if c != nil {
			res := c.Check(ctx, version.Version, logf)
			latest = res.Latest
			outdated = res.Outdated
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
	// Stage 2: download + verify + extract in one call.
	// updater.DownloadTag composes the URL itself (no API
	// call) and tries GitHub first, mirror fallback. Pass a
	// progress bar to deps.Out so the user sees download
	// activity (it can take minutes on a 100 MB binary).
	targetTag := version.Tag(latest)
	progress := updater.NewASCIIProgressBar(out, 0)
	dlCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
	dl, err := updater.DownloadTag(dlCtx, targetTag, cfg.Paths.DataDir, progress)
	stop()
	if err != nil {
		fmt.Fprintf(out, "  %s  download failed: %v\n", paintRed(out, "✗"), err)
		fmt.Fprintln(out, "     Run `nightme update` from a shell to retry.")
		return nil
	}
	if dl.Source == "mirror" {
		fmt.Fprintln(out, "  ·  using mirror (github was unreachable)")
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %s  Staged %s  %s\n",
		paintGreen(out, "✓"),
		dl.AssetName,
		paintDim(out, "sha256="+dl.SHA256Hex))
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

// runInstallStage swaps the running binary with the
// downloaded one and restarts the daemon (best-effort).
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

	target, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate current binary: %w", err)
	}
	installRes, err := updater.Install(dl.BinaryPath, target)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(out, "  %s  installed %s\n", paintGreen(out, "✓"), installRes.NewBinaryPath)
	fmt.Fprintf(out, "     %s %s\n", paintDim(out, "backup"), paintDim(out, installRes.OldBinaryPath))

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
