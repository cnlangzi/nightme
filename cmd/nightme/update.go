// Package main — `nightme update`.
//
// Self-update entry point. The command is one verb (no
// subcommands): a single invocation walks the user through
// the three internal stages — check, download, install —
// in order, with explicit y/N gates between them.
//
// Internal layering (not user-visible):
//
//   internal/updater/check.go      — Lookup / MatchAsset / compare
//   internal/updater/download.go   — Download / SHA256SUMS / ExtractArchive
//   internal/updater/install.go    — Install / swap binary / restart daemon
//
// The three files mirror the three stages but the CLI
// surface stays single-verb. We picked this layout because
// the REPL prompt wants to drive the same three stages
// interactively (one y/N per stage), and re-using the same
// internal functions from both the CLI shell and the REPL
// keeps the two paths in lock-step.
//
// Stage gating
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
//                    "可中途取消,跳过下载/安装" requirement:
//                    Ctrl-C during download AND a y/N gate
//                    before install)
//   - y            → install + restart + exit
//
// Restart policy
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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/daemoncontrol"
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
		tag        string
		quiet      bool
		noInstall  bool
		noRestart  bool
		yes        bool
		fromRepl   bool
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

	// Banner so the user can read what kind of operation
	// is about to run BEFORE the first stage header
	// appears. The "[1/3] check" line by itself doesn't say
	// what we're checking, and a user pasting this into a
	// bug report needs the verb visible at the top.
	fmt.Fprintln(out, "nightme update — installing the latest release")
	fmt.Fprintln(out, "  stages: 1/3 check → 2/3 download → 3/3 install")
	fmt.Fprintln(out)

	// Stage 1: check.
	fmt.Fprintln(out, "[1/3] check")
	res, err := updater.Check(ctx, opts.tag)
	if err != nil {
		fmt.Fprintf(errOut, "check failed: %v\n", err)
		return err
	}
	fmt.Fprintf(out, "  current: %s\n", version.Version)
	fmt.Fprintf(out, "  latest:  %s\n", res.Latest)
	if !res.Outdated {
		fmt.Fprintln(out, "  status:  up-to-date; nothing to do")
		return nil
	}
	fmt.Fprintf(out, "  status:  newer release available (%s)\n", res.Latest)

	// Resolve the matching asset (we already know our
	// OS/arch from runtime; MatchAsset filters on it).
	asset := updater.MatchAsset(res.Release, res.Latest)
	if asset == nil {
		return fmt.Errorf("no release asset for %s/%s in %s; available: %s",
			runtime.GOOS, runtime.GOARCH, res.Latest, assetNames(res.Release.Assets))
	}

	// Stage 2: download. If the staging dir already holds the
	// archive + SHA256SUMS for this exact release + asset, we
	// skip the re-fetch and reuse the staged file. This is what
	// lets the REPL prompt do "check + download" → "ask y/N" →
	// "install" in two separate cobra invocations: the first
	// invocation leaves the staging dir in place; the second
	// sees it and goes straight to install.
	fmt.Fprintln(out, "[2/3] download")
	stagingDir, err := updater.StagingDir(dataDir, res.Latest)
	if err != nil {
		return err
	}
	// Print "what / where from / where to" BEFORE the progress
	// bar so the user knows what's about to download. These lines
	// stay put; the bar overwrites itself with \r on every tick.
	fmt.Fprintf(out, "  asset:    %s\n", asset.Name)
	fmt.Fprintf(out, "  from:     %s\n", asset.BrowserDownloadURL)
	fmt.Fprintf(out, "  to:       %s\n", stagingDir)
	wantExt := "tar.gz"
	if runtime.GOOS == "windows" {
		wantExt = "zip"
	}
	wantArchive := filepath.Join(stagingDir, fmt.Sprintf("nightme_%s_%s_%s.%s",
		strings.TrimPrefix(res.Latest, "v"), runtime.GOOS, runtime.GOARCH, wantExt))
	var dlRes *updater.DownloadResult
	if info, err := os.Stat(wantArchive); err == nil && info.Size() == asset.Size {
		// Cache hit. Compute SHA256 for the trailer so the
		// output is identical to a fresh download's; install
		// doesn't use the value (extract just opens the
		// archive) but the operator transcript looks
		// consistent.
		sum, err := sha256OfFile(wantArchive)
		if err != nil {
			return fmt.Errorf("compute cached sha256: %w", err)
		}
		fmt.Fprintf(out, "  cache hit: %s (size %d) — skipping download\n",
			wantArchive, info.Size())
		dlRes = &updater.DownloadResult{
			Asset:       *asset,
			StagingPath: wantArchive,
			Bytes:        info.Size(),
			SHA256Hex:   sum,
		}
	} else {
		progress := updater.QuietProgress
		if !opts.quiet {
			progress = updater.NewASCIIProgressBar(out, asset.Size)
		}
		dlRes, err = updater.Download(ctx, res.Release, asset, stagingDir, progress)
		if err != nil {
			fmt.Fprintf(errOut, "download failed: %v\n", err)
			return err
		}
		fmt.Fprintln(out) // newline after the bar
	}
	fmt.Fprintf(out, "  size:     %s\n", updater.FormatBytes(dlRes.Bytes))
	fmt.Fprintf(out, "  sha256:   %s\n", dlRes.SHA256Hex)

	// Optional: --no-install stops here. We print the stage
	// header even on the skip path so the user sees the
	// three-stage progress; the trailer makes the skip
	// explicit.
	if opts.noInstall {
		fmt.Fprintln(out, "[3/3] install (skipped)")
		fmt.Fprintln(out, "  --no-install set; stopping before swap")
		return nil
	}

	// Stage 3: install.
	fmt.Fprintln(out, "[3/3] install")
	binary, err := updater.ExtractArchive(dlRes.StagingPath, stagingDir)
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	fmt.Fprintf(out, "  extracted: %s\n", binary)

	targetPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current binary: %w", err)
	}
	installRes, err := updater.Install(binary, targetPath)
	if err != nil {
		return fmt.Errorf("install: %w", err)
	}
	fmt.Fprintf(out, "  installed: %s\n", installRes.NewBinaryPath)
	fmt.Fprintf(out, "  backup:    %s\n", installRes.OldBinaryPath)

	// Optional: --no-restart skips the daemon restart but
	// still prints the "now restart your shell" hint.
	if !opts.noRestart {
		running, _ := daemonIsRunning(cfg)
		if running {
			fmt.Fprintln(out, "  → restarting nightme daemon…")
			if err := runRestartInline(out); err != nil {
				fmt.Fprintf(errOut, "  warning: daemon restart failed: %v\n", err)
				fmt.Fprintln(errOut, "           run `nightme restart` manually.")
			} else {
				fmt.Fprintln(out, "  ✓ daemon restarted on the new binary")
			}
		} else {
			fmt.Fprintln(out, "  no daemon running; skipping restart")
		}
	}

	// Success trailer. Bare CLI re-execs the new binary;
	// the REPL driver stops here so readline state survives
	// (the user restarts the REPL manually to load the
	// new binary).
	fmt.Fprintln(out)
	fmt.Fprintln(out, "✓ updated to", res.Latest)
	if opts.fromRepl {
		fmt.Fprintln(out, "  exit this REPL and re-enter `nightme` to load the new binary.")
		return nil
	}
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
	cmd := exec.Command(binary, argv[1:]...)
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

// sha256OfFile hashes a single file and returns the hex
// digest. Used by the download-cache-hit path so the
// transcript's "sha256:" line is populated even when we
// skipped the live fetch.
func sha256OfFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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
// freshly-installed binary. We shell out via os.Executable()
// (which is now the new binary) so the restart is performed
// by the version we just installed.
func runRestartInline(out io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "restart")
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
	updatesDir := filepath.Join(dataDir, "updates")
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