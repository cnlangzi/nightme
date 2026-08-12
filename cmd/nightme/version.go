// Package main — `nightme version` subcommand.
//
// The root command already exposes `--version` via Cobra's built-in
// Version hook. This subcommand is a sibling for the REPL: typing
// `version` at the prompt is more natural than `--version`, and
// Cobra does not register a `version` verb on its own.
//
// Both `nightme --version` and `nightme version` route through
// bannerWithVersion() so they print the ASCII logo header on top of
// the version line — keeping the two output paths guaranteed in
// sync (no risk of one drifting away from the other).
package main

import (
	"github.com/spf13/cobra"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the nightme version and exit",
		Long:  "Print the nightme version metadata (version, commit, build date) and exit.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := cmd.OutOrStdout().Write([]byte(bannerWithVersion() + "\n"))
			return err
		},
	}
}
