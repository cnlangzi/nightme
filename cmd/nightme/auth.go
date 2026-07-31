// Package main — `nightme auth` subcommand tree.
//
// `nightme auth` owns channel credential onboarding. v0.1 ships a
// single child verb (`feishu`) with three sub-verbs: `login`,
// `status`, `logout`. The verb grammar deliberately mirrors the
// spec at docs/feat/F-22 §3 so help text is self-documenting.
//
// We treat each channel as a separate cobra.Command at compile time
// (rather than discovering providers via reflection) so the help
// output and shell completion do not depend on Registry state.
//
//	nightme auth login feishu     -- run QR-coded device-auth flow
//	nightme auth status feishu    -- show currently configured app_id
//	nightme auth logout feishu    -- remove credentials from config
//
// Design notes:
//   - login delegates to an auth.Provider so the CLI shares the
//     interface boundary with any future channels (Lark, etc.).
//   - status / logout work entirely against on-disk Config — no
//     network access. They never expose ClientSecret to stdout.
//   - All commands honour NIGHTME_CONFIG (the env override already
//     implemented by config.LoadDefault).
package main

import "github.com/spf13/cobra"

// newAuthCmd returns the parent `nightme auth` command. Each
// sub-verb (`login`, `status`, `logout`) is added by its own file
// so the package surface scales linearly with new verbs.
func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage channel credentials",
		Long: "auth manages per-channel credentials (e.g. Feishu AppID /\n" +
			"AppSecret). v0.1 supports `login`, `status`, `logout` — all\n" +
			"currently Feishu-only.",
	}

	cmd.AddCommand(newAuthLoginCmd())
	cmd.AddCommand(newAuthStatusCmd())
	cmd.AddCommand(newAuthLogoutCmd())

	return cmd
}
