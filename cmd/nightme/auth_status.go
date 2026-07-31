// Package main — `nightme auth status feishu` subcommand.
//
// `status` is a pure read of on-disk Config. It deliberately does
// NOT print ClientSecret: a leaked secret in a CI log is much more
// damaging than a leaked app_id (the app_id is shown on every
// message your bot sends anyway).
package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
)

// newAuthStatusCmd returns the `nightme auth status` parent command.
func newAuthStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the on-disk credentials for a channel",
		Long: "status shows the currently configured app_id for the\n" +
			"named channel. The secret is never printed.",
	}
	cmd.AddCommand(newAuthStatusFeishuCmd())
	return cmd
}

// newAuthStatusFeishuCmd builds `nightme auth status feishu`. The
// JSON flag is a v0.2 nice-to-have (`--json`), but we keep the
// option open now so command shape does not need to break later.
func newAuthStatusFeishuCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "feishu",
		Short: "Show the configured Feishu app_id",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAuthStatusFeishu(cmd)
		},
	}
}

// runAuthStatusFeishu loads the config and prints the configured
// app_id (or a "not configured" message). The secret is omitted.
func runAuthStatusFeishu(cmd *cobra.Command) error {
	cfg, err := config.LoadDefault()
	if err != nil {
		return fmt.Errorf("auth status: load config: %w", err)
	}
	return printFeishuStatus(cmd.OutOrStdout(), cfg)
}

// printFeishuStatus writes one of two blocks depending on whether
// Feishu.AppID is populated. Splitting the printer from the loader
// makes both pieces testable in isolation.
func printFeishuStatus(w io.Writer, cfg *config.Config) error {
	if cfg.Feishu.AppID == "" {
		fmt.Fprintln(w, "Feishu is not configured. Run `nightme auth login feishu` to onboard.")
		return nil
	}
	fmt.Fprintf(w, "Feishu credentials (%s):\n", config.DefaultPath())
	fmt.Fprintf(w, "  app_id: %s\n", cfg.Feishu.AppID)
	if cfg.Feishu.VerificationToken != "" {
		fmt.Fprintf(w, "  verification_token: configured\n")
	}
	if cfg.Feishu.EncryptKey != "" {
		fmt.Fprintf(w, "  encrypt_key: configured\n")
	}
	// Note: app_secret is deliberately omitted. Leaked secrets in
	// shell history / logs are a common cause of incidents.
	return nil
}
