// Package main — `nightme version` subcommand.
//
// The root command already exposes `--version` via Cobra's built-in
// Version hook. This subcommand is a sibling for the REPL: typing
// `version` at the prompt is more natural than `--version`, and
// Cobra does not register a `version` verb on its own.
//
// The output matches `nightme --version` (both go through
// version.String()) so the two are guaranteed to stay in sync.
package main

import (
	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/version"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the nightme version and exit",
		Long:  "Print the nightme version metadata (version, commit, build date) and exit.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := cmd.OutOrStdout().Write([]byte(version.String() + "\n"))
			return err
		},
	}
}
