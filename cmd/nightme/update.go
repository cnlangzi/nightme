// Package main — `nightme update` subcommand.
//
// This is the explicit "do an update now" path, distinct from
// the REPL startup version-check prompt. It exists today so:
//
//   - The REPL startup Y/n prompt has something concrete to
//     point the user at once the auto-update flow lands.
//   - Power users can trigger an update without restarting the
//     REPL (`nightme update` from another terminal).
//
// It is intentionally a STUB this round: we don't yet replace
// the running binary. The user-visible message explains how to
// reinstall manually, which matches the safe behaviour the
// REPL prompt already documents (see maybeCheckAndPrompt in
// repl.go). When the real download / replace logic lands it
// will replace runUpdate's body, not its signature.
package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/version"
)

func newUpdateCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update nightme to the latest release",
		Long: "Check nightme.dev for a newer release and (when implemented)\n" +
			"replace the running binary. The version-check is throttled:\n" +
			"results are cached under <DataDir>/version-check.json for\n" +
			"24h so repeated invocations don't hammer the version API.\n\n" +
			"This round the command only REPORTS the latest available\n" +
			"version and prints the manual install instructions. The\n" +
			"download + replace flow will replace this body in a follow-up.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpdate(cmd, yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false,
		"Accept the manual reinstall instructions without prompting (no-op this round)")
	return cmd
}

// runUpdate is the placeholder body. It fetches the latest
// release (best-effort — network failure prints a warning but
// does not error out), compares with the build-time version,
// and tells the user how to install.
//
// Why no error return when the network is down?
// `nightme update` is the kind of command a user runs when
// they're already mid-debug. We should not gate them on a
// reachable GitHub; if we can't check, we still tell them
// what we DO know (current version, install hint).
func runUpdate(cmd *cobra.Command, _ bool) error {
	out := cmd.OutOrStdout()

	c, _ := version.DefaultChecker("")
	// Cache disabled in this code path: explicit "nightme update"
	// should always be a live check. (We could reuse the
	// DataDir-resolved path, but root.go already calls
	// config.LoadDefault so we'd need to thread that in.)
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
		}
	} else {
		fmt.Fprintln(out, "Latest release:  (could not reach GitHub)")
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Automatic self-update is not implemented yet. To upgrade:")
	fmt.Fprintln(out, "  go install github.com/cnlangzi/nightme/cmd/nightme@latest")
	fmt.Fprintln(out, "Or download a binary release from:")
	fmt.Fprintln(out, "  https://github.com/cnlangzi/nightme/releases/latest")
	return nil
}