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
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
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

// newUpdateInstallCmd is a placeholder for round 3 of the
// three-step split. It will use selfupdate to swap the binary
// in place and then exec + restart the daemon.
func newUpdateInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Replace the binary + restart the daemon (not implemented yet)",
		Long: "Replace the running binary with the previously downloaded\n" +
			"asset, then restart the nightme daemon. Will land in\n" +
			"the third commit of the three-step split.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(),
				"nightme update install: not implemented yet (next-next commit)")
			return nil
		},
	}
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