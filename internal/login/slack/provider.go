// Package slack implements the `nightme login slack` credential
// flow.
//
// Unlike Feishu — where the Lark SDK can register an app for the
// user over a device-authorization grant — Slack has no third-party
// app-creation API. The bot must be created out of band on
// api.slack.com/apps. What nightme can do is make that as close to
// one step as possible by handing the user an app manifest that
// pre-configures every scope and event subscription, so the flow
// reduces to: paste manifest, copy two tokens back.
package slack

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/cnlangzi/nightme/internal/login"
)

// Options configures the provider. Both tokens may be supplied
// non-interactively for scripted installs.
type Options struct {
	BotToken string
	AppToken string
	// In is the reader for interactive prompts. Defaults to stdin.
	In io.Reader
	// Out is where the walkthrough is printed. Defaults to stdout.
	Out io.Writer
	// authTest is swapped in tests. Production validates the token
	// against Slack's auth.test.
	authTest func(ctx context.Context, botToken, appToken string) (teamName, botName string, err error)
}

// Provider implements login.Provider for Slack.
type Provider struct {
	opts Options
	// SkipGreet suppresses the post-login greeting.
	SkipGreet bool
}

// New builds a Slack login provider.
func New(opts Options) *Provider {
	if opts.authTest == nil {
		opts.authTest = liveAuthTest
	}
	return &Provider{opts: opts}
}

func (p *Provider) Name() string { return "slack" }

// Login collects and validates the two Slack tokens.
func (p *Provider) Login(ctx context.Context) (*login.Credentials, error) {
	out := p.opts.Out
	if out == nil {
		out = io.Discard
	}

	botToken := strings.TrimSpace(p.opts.BotToken)
	appToken := strings.TrimSpace(p.opts.AppToken)

	if botToken == "" || appToken == "" {
		printWalkthrough(out)
	}

	reader := bufio.NewReader(readerOrStdin(p.opts.In))
	var err error
	if botToken == "" {
		botToken, err = prompt(ctx, out, reader, "Paste the Bot User OAuth Token (xoxb-…): ")
		if err != nil {
			return nil, err
		}
	}
	if appToken == "" {
		appToken, err = prompt(ctx, out, reader, "Paste the App-Level Token (xapp-…): ")
		if err != nil {
			return nil, err
		}
	}

	if err := validateTokenShapes(botToken, appToken); err != nil {
		return nil, err
	}

	teamName, botName, err := p.opts.authTest(ctx, botToken, appToken)
	if err != nil {
		return nil, fmt.Errorf("%w: token rejected by Slack: %v", login.ErrLoginFailed, err)
	}

	appName := botName
	if teamName != "" {
		appName = botName + " @ " + teamName
	}

	return &login.Credentials{
		BotToken:  botToken,
		AppToken:  appToken,
		AppName:   appName,
		CreatedAt: time.Now().UTC(),
	}, nil
}

// Greet is a no-op for Slack.
//
// The greeting contract is best-effort and needs a recipient. Feishu
// and Telegram learn one during login (the consenting user; the first
// DM sender). Slack's install flow hands back tokens and nothing
// else — there is no owner id to send to, and guessing one by
// walking users.list would mean DMing an arbitrary workspace member.
// The walkthrough tells the user to DM the bot instead, which
// produces a real turn through the normal inbound path.
func (p *Provider) Greet(context.Context, login.GreetingMessages) error { return nil }

// validateTokenShapes catches the most common paste error — swapping
// the two tokens — before a confusing API rejection.
func validateTokenShapes(botToken, appToken string) error {
	if !strings.HasPrefix(botToken, "xoxb-") {
		return fmt.Errorf("%w: bot token should start with \"xoxb-\" (got %q…); "+
			"it comes from OAuth & Permissions → Bot User OAuth Token",
			login.ErrLoginFailed, safePrefix(botToken))
	}
	if !strings.HasPrefix(appToken, "xapp-") {
		return fmt.Errorf("%w: app-level token should start with \"xapp-\" (got %q…); "+
			"it comes from Basic Information → App-Level Tokens, with the connections:write scope",
			login.ErrLoginFailed, safePrefix(appToken))
	}
	return nil
}

// safePrefix returns a short, non-secret prefix for error messages.
func safePrefix(token string) string {
	if len(token) <= 5 {
		return token
	}
	return token[:5]
}

// liveAuthTest verifies the bot token against Slack.
//
// Only the bot token can be checked here: the app-level token is
// exercised when the Socket Mode connection opens, and there is no
// cheap endpoint to pre-validate it. A wrong xapp- token therefore
// surfaces at `nightme start`, not at login — which is why
// validateTokenShapes checks the prefix.
func liveAuthTest(ctx context.Context, botToken, _ string) (string, string, error) {
	client := slackgo.New(botToken)
	resp, err := client.AuthTestContext(ctx)
	if err != nil {
		return "", "", err
	}
	return resp.Team, resp.User, nil
}

func readerOrStdin(in io.Reader) io.Reader {
	if in != nil {
		return in
	}
	return stdin
}

// prompt reads one non-empty line, honouring ctx cancellation.
func prompt(ctx context.Context, out io.Writer, reader *bufio.Reader, label string) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		for {
			fmt.Fprint(out, label)
			line, err := reader.ReadString('\n')
			trimmed := strings.TrimSpace(line)
			if err != nil && trimmed == "" {
				ch <- result{err: err}
				return
			}
			if trimmed != "" {
				ch <- result{line: trimmed}
				return
			}
			if err != nil {
				ch <- result{err: err}
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", login.ErrLoginTimeout
		}
		return "", ctx.Err()
	case res := <-ch:
		if res.err != nil {
			return "", fmt.Errorf("%w: reading token: %v", login.ErrLoginFailed, res.err)
		}
		return res.line, nil
	}
}

func printWalkthrough(out io.Writer) {
	fmt.Fprint(out, `
Slack app setup (Socket Mode — no public URL required)

  1. Open https://api.slack.com/apps → "Create New App" → "From an app manifest"
  2. Pick your workspace, then on the "Create from manifest" screen
     select the YAML tab and paste the manifest shown below this box.
`)
	// Embed the manifest inline so a user on a network where Slack's
	// new_app=1 deep link is blocked (corporate proxy, sandboxed
	// phone browser, …) does not have to run a second command.
	fmt.Fprintln(out)
	fmt.Fprintln(out, "----- paste this YAML into Slack -----")
	fmt.Fprintln(out)
	fmt.Fprint(out, AppManifest)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "----- end of manifest -----")
	fmt.Fprintln(out)
	fmt.Fprint(out, `  3. After "Your app has been created", left sidebar → "Install App" →
     click "Install to <your workspace>", then Allow.
     Copy the xoxb-… Bot User OAuth Token.

     The manifest already enabled it; you do NOT need to enable
     Socket Mode from the sidebar. It is on by default because
     socket_mode_enabled: true was in the YAML.

  4. Left sidebar → "Basic Information" → "App-Level Tokens" →
     "Generate Token and Enter Socket Mode", enter a name,
     add the connections:write scope, then Generate.
     Copy the xapp-… token now — Slack shows it only once.

  5. Paste both tokens below.

  After login: invite the bot to a channel with /invite @NightMe,
  or just open a DM with it and say hello.

  Note: changing scopes or events later requires reinstalling the
  app to the workspace before the change takes effect.

`)
}
