// Package main — `nightme update`.
//
// Self-update entry point. The command is one verb (no
// subcommands): a single invocation walks the user through
// the three internal stages — check, download, install —
// in order, with explicit y/N gates between them.
//
// Internal layering (not user-visible):
//
//	internal/updater/check.go      — Lookup / MatchAsset / compare
//	internal/updater/download.go   — Download / SHA256SUMS / ExtractArchive
//	internal/updater/install.go    — Install / swap binary / restart daemon
//
// The three files mirror the three stages but the CLI
// surface stays single-verb. We picked this layout because
// the REPL prompt wants to drive the same three stages
// interactively (one y/N per stage), and re-using the same
// internal functions from both the CLI shell and the REPL
// keeps the two paths in lock-step.
//
// # Stage gating
//
// CLI (bare `nightme update`):
//   - check fails / up-to-date     → exit 0
//   - check OK                     → unconditional download
//   - download OK                  → unconditional install
//   - --no-install                 → stop after download (CI / pre-stage)
//
// REPL prompt (PromptDeps.OnUpdate):
//   - check fails / up-to-date     → silent, return to REPL
//   - check OK                     → ask y/N
//   - y            → ask download vs skip
//   - download OK  → ask install vs skip (matches the
//     "可中途取消,跳过下载/安装" requirement:
//     Ctrl-C during download AND a y/N gate
//     before install)
//   - y            → install + restart + exit
//
// # Restart policy
//
// Bare CLI: after install the binary is re-exec'd (with the
// user's original argv) so the running process is replaced
// with the new version. Daemon is restarted first so a fresh
// REPL / shell sees the new daemon.
//
// REPL: the readline instance owns TTY state that doesn't
// survive an exec — we'd lose history, key bindings, etc.
// So in the REPL path we DO NOT exec; we print "the new
// binary is installed; restart your REPL to use it" and let
// the user re-enter the shell. The daemon still restarts.
package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/cnlangzi/nightme/internal/proc"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/daemoncontrol"
	"github.com/cnlangzi/nightme/internal/pathutil"
	"github.com/cnlangzi/nightme/internal/updater"
	"github.com/cnlangzi/nightme/internal/version"
)

// newUpdateCmd builds the single-verb `nightme update`.
//
// Flags:
//
//	--tag vX.Y.Z      pin a specific release (default: latest)
//	--quiet / -q      suppress progress bar (still verifies SHA256)
//	--no-install      download + verify only; do NOT swap the binary.
//	                  Useful in CI: pre-warm the staging dir, then run
//	                  `nightme update` again later with --no-install
//	                  removed to actually install.
//	--no-restart      skip the daemon restart after install
//	--yes / -y        accept every stage without y/N prompts (CI mode)
func newUpdateCmd() *cobra.Command {
	var (
		tag       string
		quiet     bool
		noInstall bool
		noRestart bool
		yes       bool
		fromRepl  bool
	)
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update nightme to the latest release (check + download + install)",
		Long: "Walk the self-update flow end to end:\n" +
			"\n" +
			"  1. check     resolve the latest release; bail if up-to-date\n" +
			"  2. download  fetch the matching asset + SHA256SUMS verify\n" +
			"  3. install   extract, swap the binary, restart the daemon\n" +
			"\n" +
			"All three stages run in a single invocation. Use --no-install\n" +
			"to stop after download (CI pre-warm). Use --yes / -y to skip\n" +
			"the y/N confirmations (also CI). --repl is set automatically\n" +
			"when the REPL prompt drives the flow; it skips the post-install\n" +
			"exec so the REPL's readline state survives.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpdate(cmd, updateOpts{
				tag:       tag,
				quiet:     quiet,
				noInstall: noInstall,
				noRestart: noRestart,
				yes:       yes,
				fromRepl:  fromRepl,
			})
		},
	}
	cmd.Flags().StringVar(&tag, "tag", "",
		"Specific release tag to install (default: latest)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false,
		"Suppress progress bar (still verifies SHA256)")
	cmd.Flags().BoolVar(&noInstall, "no-install", false,
		"Stop after download; do not swap the binary")
	cmd.Flags().BoolVar(&noRestart, "no-restart", false,
		"Skip the daemon restart after install")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false,
		"Skip y/N confirmations (CI / scripted runs)")
	cmd.Flags().BoolVar(&fromRepl, "repl", false,
		"REPL driver: skip the post-install exec (preserve readline state)")
	return cmd
}

// updateOpts is the parsed-flag bundle runUpdate consumes.
// Bundling keeps the function signature stable as we add
// flags without touching every call site.
type updateOpts struct {
	tag       string
	quiet     bool
	noInstall bool
	noRestart bool
	yes       bool
	fromRepl  bool
}

// updateStage is the per-stage result the CLI prints.
//
// Stages 1 (check) and 2 (download) report Outdated / Skipped;
// stage 3 (install) reports NewVersion. The struct doubles as
// the "what got done" tally for the REPL prompt's transcript.
type updateStage struct {
	Name       string
	Outdated   bool
	Skipped    bool
	NewVersion string // install-only
}

// runUpdate is the bare-CLI entry point. It walks the three
// stages unconditionally, prints what each did, and (on
// success) re-execs the binary so the user immediately sees
// the new version.
//
// The CLI variant doesn't ask y/N questions (other than
// optionally via --yes for stages that would normally
// prompt in the REPL path). The REPL path uses its own
// helper (promptForUpdateIfOutdated → OnUpdate) which drives
// the same internal functions but with per-stage prompts.
func runUpdate(cmd *cobra.Command, opts updateOpts) error {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	cfg, err := config.LoadDefault()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	dataDir := cfg.Paths.DataDir
	if dataDir == "" {
		return errors.New("update: cfg.Paths.DataDir is empty")
	}

	ctx := cmd.Context()

	res, err := updater.Check(ctx, opts.tag)
	if err != nil {
		fmt.Fprintf(errOut, "  %s  check failed: %v\n", paintRed(out, "✗"), err)
		return err
	}

	current := displayVer(version.Version)
	latest := displayVer(res.Latest)
	fmt.Fprintln(out)
	if !res.Outdated {
		fmt.Fprintf(out, "  %s  Already up to date\n", paintGreen(out, "✓"))
		fmt.Fprintf(out, "     %s\n", paintDim(out, current))
		return nil
	}
	fmt.Fprintf(out, "  %s  Update available\n", paintYellow(out, "▲"))
	fmt.Fprintf(out, "     %s %s %s\n",
		paintDim(out, current),
		paintDim(out, "→"),
		paint(out, ansiBold+ansiGreen, latest))

	asset := updater.MatchAsset(res.Release, res.Latest)
	if asset == nil {
		return fmt.Errorf("no release asset for %s/%s in %s; available: %s",
			runtime.GOOS, runtime.GOARCH, res.Latest, assetNames(res.Release.Assets))
	}

	stagingDir, err := updater.StagingDir(dataDir, res.Latest)
	if err != nil {
		return err
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %s  %s  %s\n",
		paintCyan(out, "↓"),
		asset.Name,
		paintDim(out, updater.FormatBytes(asset.Size)))
	progress := updater.QuietProgress
	if !opts.quiet {
		progress = updater.NewASCIIProgressBar(out, asset.Size)
	}
	dlRes, err := updater.Download(ctx, res.Release, asset, stagingDir, progress)
	if err != nil {
		fmt.Fprintf(errOut, "  %s  download failed: %v\n", paintRed(out, "✗"), err)
		return err
	}
	if dlRes.Cached {
		fmt.Fprintf(out, "  %s  sha256 verified — skipping download\n", paintGreen(out, "✓"))
	} else if !opts.quiet {
		fmt.Fprintln(out)
	}
	fmt.Fprintf(out, "  %s  Staged %s  %s\n",
		paintGreen(out, "✓"),
		dlRes.Asset.Name,
		paintDim(out, updater.FormatBytes(dlRes.Bytes)+", sha256="+dlRes.SHA256Hex))

	if opts.noInstall {
		fmt.Fprintf(out, "  %s  --no-install; stopping before swap\n", paintDim(out, "→"))
		return nil
	}

	fmt.Fprintln(out)
	binary, err := updater.ExtractArchive(dlRes.StagingPath, stagingDir)
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}

	targetPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current binary: %w", err)
	}
	installRes, err := updater.Install(binary, targetPath)
	if err != nil {
		return fmt.Errorf("install: %w", err)
	}
	fmt.Fprintf(out, "  %s  installed %s\n", paintGreen(out, "✓"), installRes.NewBinaryPath)
	fmt.Fprintf(out, "     %s %s\n", paintDim(out, "backup"), paintDim(out, installRes.OldBinaryPath))

	if !opts.noRestart {
		running, _ := daemonIsRunning(cfg)
		if running {
			fmt.Fprintf(out, "  %s  restarting daemon…\n", paintDim(out, "→"))
			if err := runRestartInline(out, targetPath); err != nil {
				fmt.Fprintf(errOut, "  %s  daemon restart failed: %v\n", paintYellow(out, "!"), err)
				fmt.Fprintln(errOut, "     run `nightme restart` manually.")
			} else {
				fmt.Fprintf(out, "  %s  daemon restarted\n", paintGreen(out, "✓"))
			}
		} else {
			fmt.Fprintf(out, "  %s  no daemon running\n", paintDim(out, "→"))
		}
	}

	fmt.Fprintln(out)
	if opts.fromRepl {
		fmt.Fprintf(out, "  %s  Installed %s — exit and re-enter `nightme` to load the new binary.\n",
			paintGreen(out, "✓"), latest)
		return nil
	}
	fmt.Fprintf(out, "  %s  Installed %s — restarting into the new binary.\n",
		paintGreen(out, "✓"), latest)
	return execAndExit(out, targetPath, os.Args)
}

// execAndExit replaces the current process with the new
// binary, preserving the user's original argv. We use
// fork+exec + os.Exit so buffered output is flushed first;
// a raw syscall.Exec would skip that.
//
// On a fork/exec failure (rare — usually ENOENT after a
// bad install) we surface the error so the operator can
// recover manually by re-running.
func execAndExit(out io.Writer, binary string, argv []string) error {
	cmd := proc.New(context.Background(), binary, argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// Child failed — print the error and stay alive
		// so the user can react (rather than disappearing).
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintf(out, "re-exec failed: %v\n", err)
		return err
	}
	// Child succeeded — match its exit code.
	os.Exit(cmd.ProcessState.ExitCode())
	return nil
}

// assetNames joins asset basenames into a comma-separated
// string for the "no asset for our OS/arch" diagnostic.
func assetNames(assets []updater.Asset) string {
	names := make([]string, 0, len(assets))
	for _, a := range assets {
		names = append(names, a.Name)
	}
	return strings.Join(names, ", ")
}

// daemonIsRunning reports whether a nightme daemon is up.
// It uses the same socket-path resolution as `nightme status`
// and is intentionally best-effort: any lookup error is
// treated as "not running" so install doesn't blow up just
// because the daemon socket is absent.
func daemonIsRunning(cfg *config.Config) (bool, error) {
	paths, err := daemoncontrol.ResolvePaths(cfg.Paths.DataDir)
	if err != nil {
		return false, err
	}
	_, err = daemoncontrol.GetStatus(paths.Socket, time.Second)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, daemoncontrol.ErrNotRunning) {
		return false, nil
	}
	return false, err
}

// runRestartInline invokes `nightme restart` against the
// freshly-installed binary. We spawn the path that was
// captured BEFORE Install (targetPath), not os.Executable()
// — re-reading os.Executable() here would follow the running
// process's inode to targetPath + ".old" (Install renames
// the running binary aside before writing the new one) and
// the spawned restart would be the OLD binary restarting
// the daemon with the OLD binary. See update.go runUpdate
// for the Install call site.
func runRestartInline(out io.Writer, targetPath string) error {
	cmd := proc.New(context.Background(), targetPath, "restart")
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

// resolveInstallVersion is kept for the REPL path's
// "no --tag" fallback (it picks the newest staging dir).
// Not used by the CLI shell, which always re-checks
// against the live version feed.
func resolveInstallVersion(dataDir, tag string) (string, error) {
	if tag != "" {
		return strings.TrimPrefix(tag, "v"), nil
	}
	// F-PATHUTIL-001: cfg.Paths.DataDir is user-supplied via YAML
	// and on Windows is commonly written with forward slashes
	// (Git Bash / WSL habits). Normalize before joining so the
	// staging directory comes out as "F:\nightme\updates" not
	// "F:/nightme\updates" (which os.ReadDir on Windows rejects).
	if n, err := pathutil.NormalizeForOS(dataDir); err == nil {
		dataDir = n
	}
	updatesDir := pathutil.Join(dataDir, "updates")
	entries, err := os.ReadDir(updatesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no staged installs under %s; run `nightme update` first", updatesDir)
		}
		return "", fmt.Errorf("read staging dir: %w", err)
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("no staged installs under %s; run `nightme update` first", updatesDir)
	}
	var newest string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if newest == "" || e.Name() > newest {
			newest = e.Name()
		}
	}
	if newest == "" {
		return "", fmt.Errorf("no versioned subdirs under %s; run `nightme update` first", updatesDir)
	}
	return newest, nil
}
