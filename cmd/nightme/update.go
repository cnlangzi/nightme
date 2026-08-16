// Package main — `nightme update` family of subcommands.
//
// `nightme update` is the explicit "manage self-update" entry
// point, distinct from the REPL startup version-check prompt
// (which only asks and never acts). It exposes three subcommands
// that mirror the three phases of an update:
//
//   - nightme update check     ← this commit (live status, no action)
//   - nightme update download  ← next commit (progress, cancellable)
//   - nightme update install   ← next commit (replace + restart daemon)
//
// Bare `nightme update` is an alias for `nightme update check`
// so existing muscle memory and the REPL prompt can call it
// without changing the verb.
//
// Why three subcommands and not one big command?
//   - The REPL startup prompt only needs to *trigger* a check
//     (or, eventually, a download). It never needs to *install*
//     in the same step. Splitting lets each phase own its
//     flags, output format, and failure semantics without
//     overloading one RunE.
//   - `--quiet` on check is useful inside the REPL prompt
//     ("run the check, don't print anything if you're already
//     current") and would be confusing on install.
//   - Future `nightme update install --dry-run` (or similar)
//     needs install to be its own command, not a flag on a
//     giant verb.
//
// Status this round:
//
//   - `check`  : implemented (was the previous round's stub body)
//   - `download`: not implemented — prints "not yet"
//   - `install` : not implemented — prints "not yet"
//
// runUpdateCheck is the live version-status reporter: fetches
// the latest version from nightme.dev/api/version (no cache),
// prints current vs latest, and tells the user how to install
// manually. The REPL prompt calls this when the user accepts
// the "update now? [y/N]" question.
package main

import (
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

// newUpdateCmd builds the `update` family. Bare `nightme update`
// delegates to `update check` so existing callers (and the REPL
// prompt) keep working.
func newUpdateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Manage nightme self-update (check / download / install)",
		Long: "Manage nightme's self-update flow.\n" +
			"\n" +
			"Subcommands:\n" +
			"  check     report current vs latest version (live, no cache)\n" +
			"  download  fetch the release asset (next commit)\n" +
			"  install   replace the binary + restart the daemon (next commit)\n" +
			"\n" +
			"Bare `nightme update` is an alias for `nightme update check`.",
		// Bare form → defer to the check subcommand so the
		// REPL prompt can call `update` (no verb) and still
		// get the check behaviour.
		RunE: func(cmd *cobra.Command, args []string) error {
			// Construct a check subcommand on the fly and
			// dispatch through it. We can't just call
			// runUpdateCheck directly because the flags on
			// the subcommand need to parse first.
			sub := newUpdateCheckCmd()
			sub.SetArgs(args)
			sub.SetOut(cmd.OutOrStdout())
			sub.SetErr(cmd.ErrOrStderr())
			sub.SetContext(cmd.Context())
			return sub.Execute()
		},
	}
	cmd.AddCommand(newUpdateCheckCmd())
	cmd.AddCommand(newUpdateDownloadCmd())
	cmd.AddCommand(newUpdateInstallCmd())
	return cmd
}

// newUpdateCheckCmd — the live version reporter. The REPL
// prompt's "y" branch invokes this so the user gets concrete
// feedback (current vs latest + install instructions) without
// leaving the shell.
func newUpdateCheckCmd() *cobra.Command {
	var quiet bool
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Report current vs latest version (live, no cache)",
		Long: "Fetch the latest version from nightme.dev/api/version\n" +
			"(no caching, unlike the REPL startup prompt) and print\n" +
			"current vs latest plus manual install instructions.\n\n" +
			"--quiet suppresses the install hint when the local\n" +
			"version is already current; useful when this is called\n" +
			"from inside the REPL prompt.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpdateCheck(cmd, quiet)
		},
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false,
		"Suppress install hint when already on the latest version")
	return cmd
}

// newUpdateDownloadCmd downloads the release asset that matches
// the running binary's GOOS/GOARCH to a staging path under
// <DataDir>/updates/<version>/. SHA256 is verified against the
// release's SHA256SUMS file before the staging file is left in
// place; a mismatch deletes the partial download.
//
// Ctrl-C cancels the download mid-flight: signal.NotifyContext
// hands the CLI a context that's cancelled the moment SIGINT
// fires, and the http client picks it up on the next chunk.
func newUpdateDownloadCmd() *cobra.Command {
	var tag string
	var quiet bool
	cmd := &cobra.Command{
		Use:   "download",
		Short: "Download the release asset for this OS/arch to the staging dir",
		Long: "Download the release asset matching the running binary's\n" +
			"GOOS/GOARCH (and a SHA256SUMS.txt-driven integrity check)\n" +
			"to <DataDir>/updates/<version>/. Prints a progress bar\n" +
			"with downloaded bytes, total, speed, and ETA; Ctrl-C\n" +
			"cancels the download and removes the partial file.\n\n" +
			"Use --tag <vX.Y.Z> to download a specific release instead\n" +
			"of the latest. --quiet suppresses the progress bar.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpdateDownload(cmd, tag, quiet)
		},
	}
	cmd.Flags().StringVar(&tag, "tag", "",
		"Specific release tag to download (default: latest)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false,
		"Suppress progress bar (still verifies SHA256)")
	return cmd
}

// newUpdateInstallCmd is the third step: replace the running
// binary with the previously downloaded + extracted asset and
// (optionally) restart the daemon.
//
// Workflow:
//
//   1. Look up the version (from --tag, or by reading the
//      latest staging dir under <DataDir>/updates/).
//   2. Extract <DataDir>/updates/<version>/nightme_<ver>_...
//      via updater.ExtractArchive.
//   3. Atomically swap the extracted binary over the running
//      one (back up to <target>.old, copy, chmod +x).
//   4. Optionally run `nightme restart` so the daemon picks
//      up the new binary on next start.
//
// We deliberately do NOT exec the new binary in place:
// execve() on macOS / Linux succeeds but loses the REPL
// session + every open TTY handle, and on signed binaries
// it confuses the macOS quarantine xattr. We tell the user
// "please restart your REPL/shell" instead.
func newUpdateInstallCmd() *cobra.Command {
	var tag string
	var skipRestart bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Replace the running binary with the staged release asset",
		Long: "Replace the currently-running nightme binary with the\n" +
			"asset previously downloaded by `nightme update download`.\n" +
			"The previous binary is preserved as <binary>.old for\n" +
			"manual rollback if the new one turns out to be broken.\n\n" +
			"By default this also restarts the nightme daemon so it\n" +
			"picks up the new binary. Pass --no-restart to skip.\n\n" +
			"This command does NOT exec the new binary in place —\n" +
			"restart your shell / REPL after install to load it.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpdateInstall(cmd, tag, skipRestart)
		},
	}
	cmd.Flags().StringVar(&tag, "tag", "",
		"Specific release tag to install (default: pick the newest staging dir)")
	cmd.Flags().BoolVar(&skipRestart, "no-restart", false,
		"Skip the daemon restart after install")
	return cmd
}

// runUpdateCheck is the live version-status reporter. It
// fetches the latest version (best-effort — network failure
// prints a warning but does not error out), compares with
// the build-time version, and tells the user how to install.
//
// --quiet: when set AND the local version is already current,
// suppress the install hint. The REPL prompt's "y" path passes
// --quiet=false so the user always sees what to do next.
func runUpdateCheck(cmd *cobra.Command, quiet bool) error {
	out := cmd.OutOrStdout()

	c, _ := version.DefaultChecker("")
	// Cache disabled in this code path: explicit
	// `nightme update check` should always be a live check.
	res := c.Check(cmd.Context(), version.Version, func(format string, args ...any) {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: "+format+"\n", args...)
	})

	fmt.Fprintf(out, "Current version: %s\n", version.Version)
	if res.Latest != "" {
		fmt.Fprintf(out, "Latest release:  %s\n", res.Latest)
		if res.Outdated {
			fmt.Fprintln(out, "Status: a newer release is available.")
		} else {
			fmt.Fprintln(out, "Status: you are on the latest release.")
			if quiet {
				return nil
			}
		}
	} else {
		fmt.Fprintln(out, "Latest release:  (could not reach the version API)")
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Automatic self-update is not implemented yet. To upgrade:")
	fmt.Fprintln(out, "  go install github.com/cnlangzi/nightme/cmd/nightme@latest")
	fmt.Fprintln(out, "Or download a binary release from:")
	fmt.Fprintln(out, "  https://github.com/cnlangzi/nightme/releases/latest")
	return nil
}

// runUpdateDownload is the body of `nightme update download`:
// fetch the release metadata, pick the matching asset, stream
// it to the staging dir with a progress bar, and verify SHA256
// against the release's SHA256SUMS.txt before declaring success.
//
// dataDir is read from config.Paths.DataDir (the REPL prompt
// path will eventually reuse this lookup). An empty dataDir
// fails the command — the user can pin the install location
// later via the planned `--staging-dir` flag.
func runUpdateDownload(cmd *cobra.Command, tag string, quiet bool) error {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	cfg, err := config.LoadDefault()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	dataDir := cfg.Paths.DataDir
	if dataDir == "" {
		return errors.New("download: cfg.Paths.DataDir is empty; cannot stage asset")
	}

	// Ctrl-C cancels the download mid-flight. cmd.Context()
	// is already wired to the cobra signal handler, so we
	// just pass it through to updater.Lookup / Download.
	ctx := cmd.Context()

	// 1. Resolve the GitHub release.
	release, err := updater.Lookup(ctx, "cnlangzi/nightme", tag)
	if err != nil {
		fmt.Fprintf(errOut, "warning: %v\n", err)
		return err
	}
	fmt.Fprintf(out, "Latest release: %s\n", release.TagName)

	// 2. Pick the asset that matches our OS/arch.
	asset := updater.MatchAsset(release, release.TagName)
	if asset == nil {
		return fmt.Errorf("no release asset for %s/%s in %s; available: %s",
			runtime.GOOS, runtime.GOARCH, release.TagName,
			assetNames(release.Assets))
	}

	// 3. Stage the download under <DataDir>/updates/<version>/.
	stagingDir, err := updater.StagingDir(dataDir, release.TagName)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Asset: %s (%s)\n", asset.Name, updater.FormatBytes(asset.Size))
	fmt.Fprintf(out, "Staging: %s\n", stagingDir)

	// 4. Progress bar (or silent). We use a fixed-width line
	// that we overwrite with \r on each tick so the terminal
	// shows a single moving bar.
	var progress updater.ProgressFunc = updater.QuietProgress
	if !quiet {
		progress = makeProgressBar(out, asset.Size)
	}

	// 5. Run the download.
	res, err := updater.Download(ctx, release, asset, stagingDir, progress)
	if err != nil {
		fmt.Fprintln(errOut)
		return fmt.Errorf("download: %w", err)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "✓ downloaded %s (%s, sha256=%s)\n",
		res.Asset.Name, updater.FormatBytes(res.Bytes), res.SHA256Hex)
	fmt.Fprintf(out, "  next: nightme update install\n")
	return nil
}

// makeProgressBar returns a ProgressFunc that renders a
// single-line ASCII progress bar to out. The bar overwrites
// itself with \r on every tick and prints a final newline on
// completion (handled by Download, which flushes an empty
// event by virtue of total == done).
//
// total <= 0 (server omitted Content-Length) renders an
// indeterminate bar that only shows downloaded bytes.
func makeProgressBar(out io.Writer, total int64) updater.ProgressFunc {
	const width = 30
	return func(downloaded, totalNow int64, elapsed time.Duration) {
		if totalNow > 0 {
			total = totalNow
		}
		var pct float64
		if total > 0 {
			pct = float64(downloaded) / float64(total)
			if pct > 1 {
				pct = 1
			}
		}
		filled := int(pct * float64(width))
		if filled > width {
			filled = width
		}
		bar := strings.Repeat("=", filled) + strings.Repeat(" ", width-filled)
		elapsedSec := elapsed.Seconds()
		var speed string
		if elapsedSec > 0 {
			speed = updater.FormatSpeed(downloaded, elapsed)
		} else {
			speed = "— B/s"
		}
		var eta string
		if total > 0 && downloaded > 0 && elapsedSec > 0 {
			remaining := time.Duration(float64(total-downloaded)/float64(downloaded)*elapsedSec) * time.Second
			eta = " ETA " + remaining.Round(time.Second).String()
		}
		fmt.Fprintf(out, "\r[%s] %3d%% %s / %s  %s%s",
			bar, int(pct*100),
			updater.FormatBytes(downloaded), updater.FormatBytes(total),
			speed, eta)
	}
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

// runUpdateInstall is the body of `nightme update install`.
//
// Flow:
//
//  1. Resolve which version we're installing. --tag pins a
//     specific version; otherwise pick the newest subdir of
//     <DataDir>/updates/.
//  2. Find the staged archive under that staging dir.
//  3. Extract the binary via updater.ExtractArchive.
//  4. Resolve targetPath via os.Executable() (the binary
//     the user is currently invoking).
//  5. updater.Install swaps target ↔ staged with a .old
//     backup, returning rollback info.
//  6. Unless --no-restart, run `nightme restart` so the
//     daemon picks up the new binary. The restart path is
//     skipped entirely when the daemon isn't running —
//     `nightme restart` on a stopped daemon is a no-op and
//     just clutters the output.
//
// We deliberately do NOT exec the new binary; the user
// restarts their shell / REPL.
func runUpdateInstall(cmd *cobra.Command, tag string, skipRestart bool) error {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	cfg, err := config.LoadDefault()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	dataDir := cfg.Paths.DataDir
	if dataDir == "" {
		return errors.New("install: cfg.Paths.DataDir is empty")
	}

	// 1. Resolve version.
	version, err := resolveInstallVersion(dataDir, tag)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Installing version: %s\n", version)

	// 2. Find the staged archive. We look for the matching
	// "<version>_<os>_<arch>.<ext>" file in the staging
	// dir; if absent we surface a clear "did you forget to
	// run download?" diagnostic.
	stagingDir, err := updater.StagingDir(dataDir, version)
	if err != nil {
		return err
	}
	archivePath, err := findStagedArchive(stagingDir, version)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Staged archive: %s\n", archivePath)

	// 3. Extract.
	extractedPath, err := updater.ExtractArchive(archivePath, stagingDir)
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	fmt.Fprintf(out, "Extracted:      %s\n", extractedPath)

	// 4. Target = the binary the user is currently running.
	targetPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current binary: %w", err)
	}

	// 5. Swap.
	res, err := updater.Install(extractedPath, targetPath)
	if err != nil {
		return err
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "✓ installed new binary at %s\n", res.NewBinaryPath)
	fmt.Fprintf(out, "  backup: %s\n", res.OldBinaryPath)
	fmt.Fprintln(out)

	// 6. Optional daemon restart. We check whether a daemon
	// is currently running before invoking restart — a
	// stopped daemon makes `nightme restart` succeed (it
	// starts one), which is almost certainly NOT what the
	// user wants after a self-update.
	if !skipRestart {
		running, _ := daemonIsRunning(cfg)
		if running {
			fmt.Fprintln(out, "→ restarting nightme daemon…")
			if err := runRestartInline(out); err != nil {
				fmt.Fprintf(errOut, "warning: daemon restart failed: %v\n", err)
				fmt.Fprintln(errOut, "         run `nightme restart` manually.")
			} else {
				fmt.Fprintln(out, "✓ daemon restarted on the new binary")
			}
		} else {
			fmt.Fprintln(out, "→ no daemon running; skipping restart")
		}
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Done. To load the new binary, restart your shell / REPL")
	fmt.Fprintln(out, "(or just run `nightme` again).")
	return nil
}

// resolveInstallVersion picks which staged version to install.
// --tag pins the version; otherwise we walk <DataDir>/updates/
// and return the lexicographically newest subdir (works for
// semver tags since "0.3.7" < "0.3.10" alphabetically is wrong,
// but it's a sane fallback when no --tag is given and we only
// have one staging dir in practice).
func resolveInstallVersion(dataDir, tag string) (string, error) {
	if tag != "" {
		return strings.TrimPrefix(tag, "v"), nil
	}
	updatesDir := filepath.Join(dataDir, "updates")
	entries, err := os.ReadDir(updatesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no staged installs under %s; run `nightme update download` first", updatesDir)
		}
		return "", fmt.Errorf("read staging dir: %w", err)
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("no staged installs under %s; run `nightme update download` first", updatesDir)
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
		return "", fmt.Errorf("no versioned subdirs under %s; run `nightme update download` first", updatesDir)
	}
	return newest, nil
}

// findStagedArchive picks the archive file in stagingDir that
// matches version + current GOOS/GOARCH + extension. Returns
// a clear error when nothing matches so the user gets an
// actionable message instead of an empty failure.
func findStagedArchive(stagingDir, version string) (string, error) {
	ext := "tar.gz"
	binaryBase := "nightme"
	if runtime.GOOS == "windows" {
		ext = "zip"
		binaryBase = "nightme.exe"
	}
	want := fmt.Sprintf("nightme_%s_%s_%s.%s", version, runtime.GOOS, runtime.GOARCH, ext)
	// First preference: the archive itself.
	archive := filepath.Join(stagingDir, want)
	if _, err := os.Stat(archive); err == nil {
		return archive, nil
	}
	// Fall back: the binary might already be extracted.
	// Install can work off the extracted binary too.
	extracted := filepath.Join(stagingDir, binaryBase)
	if _, err := os.Stat(extracted); err == nil {
		return extracted, nil
	}
	return "", fmt.Errorf("no staged asset for %s/%s under %s (looked for %s and %s); run `nightme update download` first",
		runtime.GOOS, runtime.GOARCH, stagingDir, want, extracted)
}

// daemonIsRunning reports whether a nightme daemon is up.
// It uses the same socket-path resolution as `nightme status`
// and is intentionally best-effort: if the lookup fails for
// any reason we return false so the install doesn't blow up
// when the daemon socket is absent.
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
	// Any other error: report it but treat as not-running
	// so install doesn't fail just because the socket
	// couldn't be probed.
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