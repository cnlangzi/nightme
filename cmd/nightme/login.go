// Package main — `nightme login` subcommand tree.
//
// `nightme login` is the channel credential onboarding command. v0.1
// ships a single channel (`feishu`) accessed as `nightme login feishu`.
// The verb is promoted to a top-level command (no `nightme auth`
// parent) because login is the only auth verb we ship — status /
// logout are intentionally not provided; users can inspect / clear
// credentials by editing `config.yaml` directly.
//
// The flow is:
//  1. Load config from DefaultPath.
//  2. Always re-bind: re-running login unconditionally overwrites
//     any existing Feishu credentials. The verb IS the rebind — no
//     --force flag, no "already configured" guard. (See F-22 §4
//     for the rationale.)
//  3. Construct the feishu Provider and call Login with a 10-minute
//     deadline (see F-22 §4 for the chosen timeout).
//  4. Persist credentials via config.SaveDefault (atomic write + 0600).
//
// On credential-write failure the in-memory credentials are
// surfaced to stderr (including the app name when present, so the
// user can recognise the registration on the Feishu console) so
// they can re-key them by hand; this matches the edge case in
// F-22 §4 ("preserve in-memory creds on disk write failure").
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/login"
	"github.com/cnlangzi/nightme/internal/login/feishu"
)

// loginCmdFlags captures the few flags the login command accepts.
type loginCmdFlags struct {
	timeout time.Duration
}

// newLoginCmd returns the top-level `nightme login` command.
// `feishu` is added as a sub-command so the verb stays channel-keyed.
//
// Invoking `nightme login` with no subcommand is a silent no-op —
// the verb is meaningless without a channel, and we deliberately
// do not print help so it composes cleanly in shell pipelines.
func newLoginCmd() *cobra.Command {
	var f loginCmdFlags

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Run an interactive channel login flow",
		Long: "login runs an interactive login flow for the\n" +
			"named channel. Today only `feishu` is implemented.\n\n" +
			"Re-running login unconditionally rebinds the channel —\n" +
			"the existing app_id / app_secret are overwritten.",
		RunE: func(_ *cobra.Command, _ []string) error {
			return nil
		},
	}
	cmd.AddCommand(newLoginFeishuCmd(&f))
	return cmd
}

// newLoginFeishuCmd builds `nightme login feishu`. The flags struct
// is shared so tests can introspect it without re-constructing the
// command.
func newLoginFeishuCmd(f *loginCmdFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "feishu",
		Short: "One-click Feishu app registration (QR scan)",
		Long: "login feishu runs Feishu's device-authorization flow:\n" +
			"a QR code is printed in the terminal for scanning with the\n" +
			"Feishu mobile app, then credentials are saved to the\n" +
			"config file (atomic write, chmod 0600).\n\n" +
			"Re-running this command rebinds the channel — any existing\n" +
			"app_id / app_secret in config.yaml is overwritten.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLoginWith(cmd, f, feishu.New(feishu.Options{}))
		},
	}
	cmd.Flags().DurationVar(&f.timeout, "timeout", 10*time.Minute, "abort the flow after this duration")
	return cmd
}

// runLoginWith is the testable core of `nightme login feishu`. The
// provider is injected explicitly so unit tests can swap in a fake
// without hitting Feishu's HTTP endpoints.
func runLoginWith(cmd *cobra.Command, f *loginCmdFlags, provider login.Provider) error {
	out := cmd.OutOrStdout()

	cfg, err := config.LoadDefault()
	if err != nil {
		return fmt.Errorf("login: load config: %w", err)
	}

	// cmd.Context() can be nil if the cobra command was not wired
	// through SetContext (e.g. some test harnesses). context.Background()
	// is the documented zero-value parent for WithTimeout.
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, f.timeout)
	defer cancel()

	creds, err := provider.Login(ctx)
	if err != nil {
		return fmt.Errorf("login: %w", err)
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
		if creds.AppName != "" {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"  app_name:   %s\n", creds.AppName)
		}
		return fmt.Errorf("login: %w", err)
	}

	// Fire the canonical greeting DM at the owner — AFTER the
	// config write has succeeded. Firing before save would mean a
	// save failure followed by a retry re-issues credentials AND a
	// duplicate greeting, leaving the user to clean up DMs.
	// Best-effort: a failed greeting must NOT roll back the
	// successful registration. Each provider implements Greet
	// against its own channel SDK; the orchestrator just hands
	// over the bilingual brand copy and swallows the error after
	// logging.
	if greetErr := provider.Greet(ctx, login.GreetingTexts()); greetErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: greeting DM failed: %v\n", greetErr)
	}

	fmt.Fprintf(out, "✓ App registered successfully!\n")
	fmt.Fprintf(out, "  App ID:    %s\n", creds.AppID)
	if creds.AppName != "" {
		fmt.Fprintf(out, "  App Name:  %s\n", creds.AppName)
	}
	fmt.Fprintf(out, "  Saved to:  %s\n", config.DefaultPath())
	fmt.Fprintf(out, "\nNext: run `nightme start` to launch the gateway daemon.\n")
	return nil
}
