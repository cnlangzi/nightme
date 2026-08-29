// Package main — `nightme login` subcommand tree.
//
// `nightme login` is the channel credential onboarding command.
// Each channel ships a provider package (internal/login/<name>)
// that registers its own subcommand via init(). Adding a new
// channel = add a new package + import it from this file (or
// transitively via internal/login's registry consumer).
//
// The verb is promoted to a top-level command (no `nightme auth`
// parent) because login is the only auth verb we ship — status /
// logout are intentionally not provided; users can inspect /
// clear credentials by editing `config.yaml` directly.
//
// Flow (delegated to login.LoginWith):
//  1. Load config from DefaultPath.
//  2. Always re-bind: re-running login unconditionally overwrites
//     any existing credentials. The verb IS the rebind — no
//     --force flag, no "already configured" guard.
//  3. Construct the channel-specific Provider and call Login
//     with a 10-minute deadline.
//  4. Persist credentials via config.SaveDefault (atomic write
//     + 0600).
//  5. Fire the canonical greeting DM (best-effort).
package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/login"
	// Register built-in channel providers via their init().
	// The blank import keeps the registry populated when the
	// CLI binary starts; new channels are added by appending to
	// this list (or via a build-tag-gated package).
	_ "github.com/cnlangzi/nightme/internal/login/feishu"
	_ "github.com/cnlangzi/nightme/internal/login/slack"
	_ "github.com/cnlangzi/nightme/internal/login/telegram"
)

// newLoginCmd returns the top-level `nightme login` command.
//
// Available subcommands come from the login provider registry
// (login.AvailableChannels). Each provider's init() registers
// its own subcommand at process start.
//
// Invoking `nightme login` with no subcommand prints a friendly
// error listing the available channels and exits non-zero.
// Invoking `nightme login <unknown>` is caught by cobra's built-in
// "unknown command" handler — we don't need to override it.
func newLoginCmd() *cobra.Command {
	flags := &login.ProviderFlags{}

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Run an interactive channel login flow",
		Long: "login runs an interactive login flow for the\n" +
			"named channel. Available channels: " +
			strings.Join(login.AvailableChannels(), ", ") + ".\n\n" +
			"Re-running login unconditionally rebinds the channel —\n" +
			"the existing credentials are overwritten.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf(
					"no channel specified\n\nAvailable channels: %s",
					strings.Join(login.AvailableChannels(), ", "))
			}
			// args is non-empty but cobra didn't match a subcommand.
			// This is unreachable in practice — cobra reports
			// "unknown command" before RunE fires — but the
			// explicit return keeps the function well-defined if
			// a future caller wires RunE differently.
			return fmt.Errorf(
				"unknown channel %q\n\nAvailable channels: %s",
				strings.Join(args, " "),
				strings.Join(login.AvailableChannels(), ", "))
		},
	}

	// Walk the registry to attach each provider's cobra command.
	// Order doesn't matter for behaviour — cobra sorts the
	// available subcommands in --help output automatically.
	for _, name := range login.AvailableChannels() {
		builder := login.GetBuilder(name)
		if builder == nil {
			continue
		}
		sub := builder(flags)
		if sub == nil {
			continue
		}
		cmd.AddCommand(sub)
	}
	return cmd
}
