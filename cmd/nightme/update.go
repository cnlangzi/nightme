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
	"fmt"

	"github.com/spf13/cobra"

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

// newUpdateDownloadCmd is a placeholder for the next commit
// (round 2 of the three-step split). It exists today so the
// cobra tree already advertises the verb to `nightme help
// update` users.
func newUpdateDownloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "download",
		Short: "Download the release asset (not implemented yet)",
		Long: "Download the release asset for the current OS / arch.\n" +
			"Will display progress, speed, ETA, and accept Ctrl-C\n" +
			"to cancel. Lands in the next commit.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(),
				"nightme update download: not implemented yet (next commit)")
			return nil
		},
	}
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