package slack

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/login"
)

// stdin is a package-level indirection so tests can drive the
// interactive prompt without touching the real terminal.
var stdin = os.Stdin

// init wires the Slack login command into the registry. cmd/nightme
// imports this package so init() runs at process start, exposing
// `nightme login slack`.
func init() {
	login.RegisterProvider("slack", func(flags *login.ProviderFlags) *cobra.Command {
		var sf loginSlackCmdFlags
		cmd := &cobra.Command{
			Use:   "slack",
			Short: "Save Slack bot + app-level tokens (Socket Mode)",
			Long: "login slack stores the two tokens a Socket Mode app needs:\n" +
				"the Bot User OAuth Token (xoxb-) for Web API calls and the\n" +
				"App-Level Token (xapp-, scope connections:write) for the\n" +
				"WebSocket connection.\n\n" +
				"Slack has no third-party app-registration API, so the app\n" +
				"itself is created on api.slack.com/apps. Run with --manifest\n" +
				"to print a ready-made app manifest that pre-configures every\n" +
				"scope and event subscription, then paste the two tokens here.\n\n" +
				"Socket Mode means no public URL, domain, TLS certificate or\n" +
				"reverse proxy is required.\n\n" +
				"Non-interactive:\n" +
				"  --bot-token <xoxb-…>  skip the stdin prompt\n" +
				"  --app-token <xapp-…>  skip the stdin prompt\n\n" +
				"Re-running this command rebinds the channel — any existing\n" +
				"slack tokens in config.yaml are overwritten.",
			RunE: func(cmd *cobra.Command, _ []string) error {
				if sf.manifestURL {
					fmt.Fprintln(cmd.OutOrStdout(), ManifestURL())
					return nil
				}
				if sf.manifest {
					fmt.Fprint(cmd.OutOrStdout(), AppManifest)
					return nil
				}
				provider := New(Options{
					BotToken: sf.botToken,
					AppToken: sf.appToken,
					In:       cmd.InOrStdin(),
					Out:      cmd.OutOrStdout(),
				})
				ctx := loginContext(cmd, flags)
				return login.LoginWith(ctx, provider,
					cmd.OutOrStdout(), cmd.ErrOrStderr())
			},
		}
		cmd.Flags().DurationVar(&flags.Timeout, "timeout", 10*time.Minute, "abort the flow after this duration")
		cmd.Flags().StringVar(&sf.botToken, "bot-token", "", "pass the xoxb- bot token via flag instead of stdin")
		cmd.Flags().StringVar(&sf.appToken, "app-token", "", "pass the xapp- app-level token via flag instead of stdin")
		cmd.Flags().BoolVar(&sf.manifest, "manifest", false, "print the Slack app manifest and exit")
		cmd.Flags().BoolVar(&sf.manifestURL, "manifest-url", false, "print a one-click 'create app from manifest' URL and exit")
		return cmd
	})
}

// loginSlackCmdFlags holds the slack-specific flags that don't
// belong on the shared ProviderFlags.
type loginSlackCmdFlags struct {
	botToken    string
	appToken    string
	manifest    bool
	manifestURL bool
}

// loginContext derives the timeout context for the provider's RunE.
// Symmetric with the feishu and telegram packages.
func loginContext(cmd *cobra.Command, flags *login.ProviderFlags) context.Context {
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, flags.Timeout)
	_ = cancel
	return ctx
}
