// Package main — `nightme auth logout feishu` subcommand.
//
// `logout` removes the stored credentials from config.yaml. It does
// not (cannot) delete the app on Feishu's side — Feishu's app
// management UI is the only place that happens. We tell the user
// that explicitly, then clear every field we wrote.
package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
)

// newAuthLogoutCmd returns the parent `nightme auth logout` command.
func newAuthLogoutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Remove the on-disk credentials for a channel",
		Long: "logout clears the configured app_id and secret from\n" +
			"the on-disk config. The app itself is NOT deleted —\n" +
			"revoke it at https://open.feishu.cn/app.",
	}
	cmd.AddCommand(newAuthLogoutFeishuCmd())
	return cmd
}

// newAuthLogoutFeishuCmd builds `nightme auth logout feishu`.
func newAuthLogoutFeishuCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "feishu",
		Short: "Remove configured Feishu credentials",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAuthLogoutFeishu(cmd)
		},
	}
}

// runAuthLogoutFeishu loads the config, blankes the Feishu fields,
// and saves atomically. It is a no-op (with informational message)
// when there is nothing to clear; that matches the documented
// behaviour of `kubectl config unset`.
func runAuthLogoutFeishu(cmd *cobra.Command) error {
	cfg, err := config.LoadDefault()
	if err != nil {
		return fmt.Errorf("auth logout: load config: %w", err)
	}

	if !hasFeishuCredentials(cfg) {
		fmt.Fprintln(cmd.OutOrStdout(), "Feishu credentials not present; nothing to clear.")
		return nil
	}

	if err := clearFeishu(cfg); err != nil {
		return fmt.Errorf("auth logout: clear: %w", err)
	}
	if err := config.SaveDefault(cfg); err != nil {
		return fmt.Errorf("auth logout: save: %w", err)
	}

	fmt.Fprintln(cmd.OutOrStdout(), "Feishu credentials cleared from config.")
	fmt.Fprintln(cmd.ErrOrStderr(),
		"note: the app is still active on Feishu. Revoke it at https://open.feishu.cn/app.")
	return nil
}

// hasFeishuCredentials returns true when at least one Feishu field
// is populated. We treat app_secret as the truthy flag too — if it
// is set but app_id is empty (unusual but possible) we still want
// to clean it up.
func hasFeishuCredentials(cfg *config.Config) bool {
	return cfg.Feishu.AppID != "" ||
		cfg.Feishu.AppSecret != "" ||
		cfg.Feishu.VerificationToken != "" ||
		cfg.Feishu.EncryptKey != ""
}

// clearFeishu blanks every Feishu field on the provided config. The
// return signature leaves room for future "you need to confirm"
// flags without changing the caller.
func clearFeishu(cfg *config.Config) error {
	cfg.Feishu.AppID = ""
	cfg.Feishu.AppSecret = ""
	cfg.Feishu.VerificationToken = ""
	cfg.Feishu.EncryptKey = ""
	return nil
}
