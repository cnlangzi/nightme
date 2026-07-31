// Package main — `nightme auth login feishu` subcommand.
//
// The flow is:
//  1. Load config from DefaultPath.
//  2. If Feishu.AppID is already set, abort with auth.ErrAlreadyConfigured
//     unless --force is passed.
//  3. Construct a FeishuAuth Provider and call Login with a 10-minute
//     deadline (see F-22 §4 for the chosen timeout).
//  4. Persist credentials via config.SaveDefault (atomic write + 0600).
//
// On credential-write failure the in-memory credentials are
// surfaced to stderr so the user can re-key them by hand; this
// matches the edge case in F-22 §4 ("preserve in-memory creds on
// disk write failure").
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/auth"
	"github.com/cnlangzi/nightme/internal/auth/feishu"
	"github.com/cnlangzi/nightme/internal/config"
)

// loginCmdFlags captures the few flags the login command accepts.
type loginCmdFlags struct {
	timeout time.Duration
	force   bool
}

// newAuthLoginCmd returns the parent `nightme auth login` command.
// `feishu` is added as a sub-command so the verb stays channel-keyed.
func newAuthLoginCmd() *cobra.Command {
	var f loginCmdFlags

	parent := &cobra.Command{
		Use:   "login",
		Short: "Run an interactive channel auth flow",
		Long: "login runs an interactive authentication flow for the\n" +
			"named channel. Today only `feishu` is implemented.",
	}
	parent.AddCommand(newAuthLoginFeishuCmd(&f))
	return parent
}

// newAuthLoginFeishuCmd builds `nightme auth login feishu`. The
// flags struct is shared so tests can introspect it without
// re-constructing the command.
func newAuthLoginFeishuCmd(f *loginCmdFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "feishu",
		Short: "One-click Feishu app registration (QR scan)",
		Long: "login feishu runs Feishu's device-authorization flow:\n" +
			"a QR code is printed in the terminal for scanning with the\n" +
			"Feishu mobile app, then credentials are saved to the\n" +
			"config file (atomic write, chmod 0600).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAuthLoginFeishu(cmd, f)
		},
	}
	cmd.Flags().DurationVar(&f.timeout, "timeout", 10*time.Minute, "abort the flow after this duration")
	cmd.Flags().BoolVar(&f.force, "force", false, "overwrite existing Feishu credentials")
	return cmd
}

// runAuthLoginFeishu is the body of `nightme auth login feishu`. It
// is split from the cobra wiring so tests can drive it with custom
// flags without re-creating the command tree.
//
// `provider` is injected by the production path via
// defaultFeishuProvider(); tests substitute their own.
func runAuthLoginFeishu(cmd *cobra.Command, f *loginCmdFlags) (err error) {
	return runAuthLoginWith(cmd, f, defaultFeishuProvider(f))
}

// runAuthLoginWith is the testable core. It accepts the provider
// explicitly so unit tests can swap in a fake without hitting
// Feishu's HTTP endpoints.
func runAuthLoginWith(cmd *cobra.Command, f *loginCmdFlags, provider auth.Provider) (err error) {
	out := cmd.OutOrStdout()

	cfg, err := config.LoadDefault()
	if err != nil {
		return fmt.Errorf("auth login: load config: %w", err)
	}

	if cfg.Feishu.AppID != "" && !f.force {
		return fmt.Errorf("auth login: %w (--force to overwrite)", auth.ErrAlreadyConfigured)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
	defer cancel()

	creds, err := provider.Login(ctx)
	if err != nil {
		return fmt.Errorf("auth login: %w", err)
	}

	cfg.Feishu.AppID = creds.AppID
	cfg.Feishu.AppSecret = creds.AppSecret

	if err := config.SaveDefault(cfg); err != nil {
		// Edge case from F-22 §4: surface the credentials verbatim
		// so the user can paste them by hand if the disk write
		// failed (permission denied, disk full, ...).
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: failed to persist credentials: %v\n"+
				"in-memory credentials (please paste into config.yaml):\n"+
				"  app_id:     %s\n"+
				"  app_secret: %s\n",
			err, creds.AppID, creds.AppSecret)
		return fmt.Errorf("auth login: %w", err)
	}

	fmt.Fprintf(out, "✓ App registered successfully!\n")
	fmt.Fprintf(out, "  App ID:    %s\n", creds.AppID)
	if creds.AppName != "" {
		fmt.Fprintf(out, "  App Name:  %s\n", creds.AppName)
	}
	fmt.Fprintf(out, "  Saved to:  %s\n", config.DefaultPath())
	fmt.Fprintf(out, "\nNext: run `nightme run` to start the gateway.\n")
	return nil
}

// defaultFeishuProvider returns the production provider. It is a
// function so tests can leave it untouched and overwrite the call
// site in runAuthLoginFeishu without conditional compilation.
func defaultFeishuProvider(_ *loginCmdFlags) auth.Provider {
	return feishu.NewFeishuAuth(feishu.FeishuAuthOptions{})
}

// isAlreadyConfigured returns true if err wraps auth.ErrAlreadyConfigured.
// Used by tests; uses errors.Is so wrapped errors still match.
func isAlreadyConfigured(err error) bool {
	return errors.Is(err, auth.ErrAlreadyConfigured)
}
