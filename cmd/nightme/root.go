// Package main is the nightme daemon entrypoint.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	nmerrors "github.com/cnlangzi/nightme/internal/errors"
)

// version is set at build time via -ldflags (see Makefile / scripts).
var version = "v0.1.0"

// newRootCmd builds the top-level `nightme` command. All subcommands
// attach to it; main.go only adds Execute().
//
// Keeping the construction in a function lets tests build their own
// root without invoking main().
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "nightme",
		Short: "Bridge AI Coding CLIs to IM channels",
		Long: "nightme is a single-process daemon that bridges AI Coding\n" +
			"CLIs (Claude Code / Codex / OpenCode) to IM channels (Feishu /\n" +
			"WhatsApp / Web UI). v0.1 ships the Local Bridge test mode —\n" +
			"use `nightme test` to spawn an agent in a PTY and `nightme list`\n" +
			"to inspect persisted sessions. See docs/SPEC.md and PLAN.md.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.SetVersionTemplate("nightme {{.Version}}\n")
	root.Version = version

	root.AddCommand(newTestCmd())
	root.AddCommand(newListCmd())
	root.AddCommand(newAuthCmd())
	root.AddCommand(newRunCmd())

	return root
}

// Execute runs the CLI and exits with an appropriate code. main.go
// calls this directly; tests can call newRootCmd() for finer control.
//
// The panic guard installed by Recover() converts unexpected
// panics to nmerrors.CodeGenericError so the exit code still
// reflects something scriptable.
func Execute() {
	root := newRootCmd()
	Recover(root)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(nmerrors.ExitCode(err))
	}
}
