package telegram

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/login"
)

// init wires the Telegram login command into the registry.
// cmd/nightme imports this package so init() runs at process
// start, exposing `nightme login telegram`.
//
// The builder mirrors the original newLoginTelegramCmd shape:
// shared flag bag, --token for non-interactive ERPL / shell-wrapped
// invocations, RunE that calls login.LoginWith.
func init() {
	login.RegisterProvider("telegram", func(flags *login.ProviderFlags) *cobra.Command {
		var tf loginTelegramCmdFlags
		cmd := &cobra.Command{
			Use:   "telegram",
			Short: "Save a user-created Telegram bot token (via @BotFather)",
			Long: "login telegram walks through the @BotFather onboarding:\n" +
				"the bot is created out-of-band (Telegram does not let\n" +
				"third parties register bots), then the HTTP API token\n" +
				"is pasted here, validated against getMe, and saved to\n" +
				"config.yaml (atomic write, chmod 0600).\n\n" +
				"Interactive (default): prints the @BotFather walkthrough,\n" +
				"reads the token from stdin, validates, then waits up to\n" +
				"2 minutes for the owner to message the bot so we can send\n" +
				"the canonical greeting.\n\n" +
				"Non-interactive:\n" +
				"  --token <token>  skip the stdin prompt, useful for\n" +
				"                  scripts and ERPL / shell-wrapped invocations\n" +
				"                  where stdin is closed.\n\n" +
				"Re-running this command rebinds the channel — any\n" +
				"existing telegram.bot_token in config.yaml is overwritten.\n\n" +
				"After login, add the bot to a Forum Supergroup and run\n" +
				"`nightme start` (v1.3+ multi-channel — all channels with valid creds auto-start).",
			RunE: func(cmd *cobra.Command, _ []string) error {
				opts := Options{Token: tf.token}
				provider := New(opts)
				ctx := loginContext(cmd, flags)
				return login.LoginWith(ctx, provider,
					cmd.OutOrStdout(), cmd.ErrOrStderr())
			},
		}
		cmd.Flags().DurationVar(&flags.Timeout, "timeout", 10*time.Minute, "abort the flow after this duration")
		cmd.Flags().StringVar(&tf.token, "token", "", "pass the bot token via flag instead of stdin (for non-interactive use)")
		return cmd
	})
}

// loginTelegramCmdFlags holds the telegram-specific flags that
// don't belong on the shared ProviderFlags (--token is telegram-only).
type loginTelegramCmdFlags struct {
	token string
}

// loginContext derives the timeout context for the provider's
// RunE. Symmetric with the feishu package — both call into the
// shared login.LoginWith orchestrator.
func loginContext(cmd *cobra.Command, flags *login.ProviderFlags) context.Context {
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, flags.Timeout)
	_ = cancel
	return ctx
}
