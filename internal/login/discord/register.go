// Package discord — registers the discord login command into the
// login provider registry. cmd/nightme imports this package so
// init() runs at process start, exposing `nightme login discord`.
//
// The provider collects a bot token + application id, validates
// the token via GET /users/@me, and (best-effort) waits up to
// 15 seconds for the first MESSAGE_CREATE so the canonical
// greeting can be DM'd to the bot owner.
package discord

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/login"
)

func init() {
	login.RegisterProvider("discord", func(flags *login.ProviderFlags) *cobra.Command {
		var df loginDiscordCmdFlags
		cmd := &cobra.Command{
			Use:   "discord",
			Short: "Save a user-created Discord bot token + application id",
			Long: "login discord walks through the Discord Developer Portal\n" +
				"onboarding: the bot is created out-of-band (Discord does not\n" +
				"let third parties register apps), then the bot token + the\n" +
				"OAuth2 application id are pasted here, the token is\n" +
				"validated against /users/@me, and both are saved to\n" +
				"config.yaml (atomic write, chmod 0600).\n\n" +
				"Interactive (default): prints the Developer Portal\n" +
				"walkthrough, reads both fields from stdin, validates,\n" +
				"then opens a 15-second Gateway window for the owner to\n" +
				"DM the bot so we can send the canonical greeting.\n\n" +
				"Non-interactive:\n" +
				"  --token <token>          skip the bot-token stdin prompt\n" +
				"  --application-id <id>    skip the application-id prompt\n" +
				"                           (required when running scripted)\n\n" +
				"Re-running this command rebinds the channel — any\n" +
				"existing discord.bot_token in config.yaml is overwritten.\n\n" +
				"After login, run `nightme start` (v1.3+ multi-channel —\n" +
				"all channels with valid creds auto-start).",
			RunE: func(cmd *cobra.Command, _ []string) error {
				opts := Options{
					Token:         df.token,
					ApplicationID: df.applicationID,
				}
				provider := New(opts)
				ctx := loginContext(cmd, flags)
				return login.LoginWith(ctx, provider,
					cmd.OutOrStdout(), cmd.ErrOrStderr())
			},
		}
		cmd.Flags().DurationVar(&flags.Timeout, "timeout", 10*time.Minute, "abort the flow after this duration")
		cmd.Flags().StringVar(&df.token, "token", "", "pass the bot token via flag instead of stdin (for non-interactive use)")
		cmd.Flags().StringVar(&df.applicationID, "application-id", "", "pass the OAuth2 application id via flag instead of stdin")
		return cmd
	})
}

// loginDiscordCmdFlags holds the discord-specific flags that
// don't belong on the shared ProviderFlags.
type loginDiscordCmdFlags struct {
	token         string
	applicationID string
}

// loginContext derives the timeout context for the provider's
// RunE. Symmetric with the telegram / feishu packages.
func loginContext(cmd *cobra.Command, flags *login.ProviderFlags) context.Context {
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, flags.Timeout)
	_ = cancel
	return ctx
}
