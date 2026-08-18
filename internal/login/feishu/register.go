package feishu

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/login"
)

// init wires the Feishu login command into the registry. cmd/nightme
// imports this package so init() runs at process start, exposing
// `nightme login feishu`.
//
// The builder follows the original newLoginFeishuCmd shape:
// top-level cobra command, shared flag bag, one RunE that calls
// login.LoginWith. Adding a new flag is a single-line change here.
func init() {
	login.RegisterProvider("feishu", func(flags *login.ProviderFlags) *cobra.Command {
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
				ctx := loginContext(cmd, flags)
				return login.LoginWith(ctx, New(Options{}),
					cmd.OutOrStdout(), cmd.ErrOrStderr())
			},
		}
		cmd.Flags().DurationVar(&flags.Timeout, "timeout", 10*time.Minute, "abort the flow after this duration")
		return cmd
	})
}

// loginContext derives the timeout context for the provider's
// RunE. Extracted so the feishu / telegram init() functions stay
// symmetric — adding more flag plumbing is a single-point change.
func loginContext(cmd *cobra.Command, flags *login.ProviderFlags) context.Context {
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, flags.Timeout)
	// Caller (login.LoginWith) doesn't see the cancel; rely on
	// provider.Login honouring ctx itself.
	_ = cancel
	return ctx
}
