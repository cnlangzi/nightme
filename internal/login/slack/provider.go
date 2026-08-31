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
//
// Greeting: after setup, the canonical greeting is DM'd directly to
// the owner — mirroring feishu.Greet, not telegram.Greet's poll.
// Slack's bot token can't identify the installer (auth.test returns
// only the bot's own identity; Slack fires no install event), so
// unlike Feishu — whose consent flow hands back the owner open_id —
// Slack discovers the owner from users.list: the one user with
// is_primary_owner == true (needs the users:read scope the manifest
// already requests). --owner overrides that for workspaces where the
// primary owner ≠ the app installer. chat.postMessage(channel=
// <userID>) opens the DM and posts in one call; the owner never has
// to message the bot first.
package slack

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
	// Owner optionally overrides the greeting recipient. Empty → the
	// owner is auto-discovered via users.list (is_primary_owner).
	// Set this only for workspaces where the primary owner is not the
	// app installer (e.g. an admin installed it on the owner's behalf).
	Owner string
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
	// out is where Greet prints its instructions + status. Login
	// keeps reading opts.Out directly for its own walkthrough so
	// the nil → io.Discard silence contract there is untouched;
	// only Greet (user-facing) defaults to os.Stdout when unset.
	out io.Writer

	// botToken is the validated xoxb- token captured during Login.
	// Greet guards on it ("Login bypassed in tests") and wireGreet
	// builds the live Slack client from it. Empty before Login.
	botToken string
	// botName is the bot username from auth.test, shown in Greet's
	// "open a DM with <name>" instruction.
	botName string

	// ownerUserID is the Slack user ID the greeting is DM'd to,
	// auto-discovered during Login via users.list (is_primary_owner),
	// or taken from opts.Owner. Empty → Greet is a no-op. Mirrors
	// feishu.ownerOpenID, which comes from the consent flow instead.
	ownerUserID string

	// listUsers is the testable seam for owner discovery: returns
	// the workspace users so discoverOwner can pick is_primary_owner.
	// Wired by wireGreet() at the end of a successful Login; nil before
	// Login. Mirrors feishu.sendDMFunc (kept off the slack-go wire types
	// so tests don't import the SDK).
	listUsers listUsersFunc

	// postDM is the testable seam for Greet's per-message send,
	// wired by wireGreet() at the end of a successful Login; nil
	// means Greet is a no-op (Login bypassed or no owner captured).
	// Mirrors feishu.sendDM (owner baked into the closure).
	postDM postDMFunc

	// SkipGreet suppresses the post-login greeting.
	SkipGreet bool
}

// New builds a Slack login provider.
func New(opts Options) *Provider {
	if opts.authTest == nil {
		opts.authTest = liveAuthTest
	}
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	return &Provider{opts: opts, out: out}
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

	// Capture the validated token + bot name and auto-discover the
	// owner (for the post-login greeting), then wire the Greet seam.
	// Mirrors feishu wiring sendDM at the end of Login — feishu gets
	// ownerOpenID from its consent flow; Slack has none, so we read
	// the owner from users.list (is_primary_owner). No stdin prompt:
	// the owner is identifiable from the bot token + users:read scope.
	p.botToken = botToken
	p.botName = botName
	p.wireGreet(ctx)

	return &login.Credentials{
		BotToken:  botToken,
		AppToken:  appToken,
		AppName:   appName,
		CreatedAt: time.Now().UTC(),
	}, nil
}

// userView is the minimal projection of a Slack user that owner
// discovery needs: its ID and the flags is_primary_owner / is_bot /
// deleted. The live implementation pulls these from users.list; tests
// feed canned values via the listUsers seam (kept off the slack-go
// types so tests don't import the SDK).
type userView struct {
	ID             string
	IsPrimaryOwner bool
	IsBot          bool
	Deleted        bool
}

// listUsersFunc is the testable seam for owner discovery. The real
// implementation hits Slack's users.list; tests return canned users
// without touching the network. Mirrors feishu.sendDMFunc.
type listUsersFunc func(ctx context.Context) ([]userView, error)

// postDMFunc is the testable seam for Greet's per-message send. The
// real implementation hits Slack's chat.postMessage with the owner's
// user ID as the channel — Slack opens the DM implicitly, so no prior
// message and no conversations.open are required. The owner user ID
// is baked into the closure at wireGreet time (mirrors feishu.sendDM
// baking ownerOpenID), so the seam takes only the text to send.
type postDMFunc func(ctx context.Context, text string) error

// Greet dispatches the canonical NightMe greeting directly to the
// owner's DM right after setup — mirroring feishu.Greet's "send to
// the consenting owner" shape, NOT telegram.Greet's "wait for the
// user to message first" poll.
//
// The owner's Slack user ID is auto-discovered during Login (see
// discoverOwner): the one user in users.list with is_primary_owner ==
// true. Slack's bot token cannot identify the installer (auth.test
// returns only the bot's own identity; Slack fires no install event),
// so unlike Feishu — whose consent flow hands back the owner open_id —
// Slack reads the owner from the workspace roster. This needs the
// users:read scope the manifest already requests. chat.postMessage
// (channel=<ownerUserID>) opens the DM and posts in one call; the
// owner never has to message the bot first.
//
// Slack only sends the English copy of each body. GreetingTexts
// still exposes a Chinese field for Feishu's post envelope (which
// renders both locales natively), but Slack has no equivalent
// bilingual block — two consecutive messages would just double the
// noise for an English-only workspace. Same decision
// telegram.sendGreeting made; the policy is enforced line-by-line in
// sendGreeting below.
//
// Best-effort throughout: a failed send never rolls back the
// credential save (the orchestrator calls Greet AFTER SaveDefault).
// No owner captured → silent skip (the daemon will still answer the
// owner's first runtime message; it never replays the login greeting).
func (p *Provider) Greet(ctx context.Context, messages login.GreetingMessages) error {
	if p.botToken == "" || p.SkipGreet {
		// Login was bypassed in tests, OR the caller opted out of
		// the post-login greeting (e.g. --no-greet).
		return nil
	}
	if p.postDM == nil {
		// Login ran but discovered no owner (users.list failed or no
		// primary owner), OR Login was bypassed in tests. Don't
		// pretend to greet — surface and exit, mirroring feishu's
		// nil-sendDM guard rather than sending to nobody.
		fmt.Fprintln(p.out, "greeting skip: no owner discovered (Login bypassed, --no-greet, or users.list failed)")
		return nil
	}

	fmt.Fprintln(p.out)
	fmt.Fprintln(p.out, "📨 Sending greeting DM")
	fmt.Fprintln(p.out, "---------------------")

	if err := p.sendGreeting(ctx, messages); err != nil {
		return fmt.Errorf("slack: send greeting: %w", err)
	}

	fmt.Fprintf(p.out, "  ✓ Greeting sent to %s\n", p.ownerUserID)
	return nil
}

// sendGreeting fires each English greeting body to the owner's DM as
// a Slack chat.postMessage (the owner user ID is baked into postDM at
// wireGreet time). A failure on body N aborts the rest (mirrors
// feishu's per-post abort).
//
// This is where the "ignore Chinese" policy is enforced line-by-line:
// only body.English is posted, body.Chinese is skipped. See Greet's
// doc for the rationale (Slack has no Feishu-style bilingual block).
func (p *Provider) sendGreeting(ctx context.Context, messages login.GreetingMessages) error {
	for index, body := range messages {
		if body.English == "" {
			continue
		}
		if err := p.postDM(ctx, body.English); err != nil {
			return fmt.Errorf("body %d english: %w", index, err)
		}
	}
	return nil
}

// discoverOwner resolves the Slack user ID to DM the greeting to:
//
//   - If opts.Owner is set (--owner), use it verbatim — an explicit
//     override for workspaces where the primary owner is not the app
//     installer (e.g. an admin installed it).
//   - Otherwise call the listUsers seam (users.list in production)
//     and return the first user with IsPrimaryOwner && !IsBot &&
//     !Deleted. Every workspace has exactly one primary owner.
//
// Returns "" (→ Greet no-op) when the override is blank, listUsers
// is unwired (Login bypassed in tests), the call fails, or no primary
// owner is found. Errors are soft (warning + skip) so a flapping
// users.list never fails the login.
func (p *Provider) discoverOwner(ctx context.Context) string {
	if owner := strings.TrimSpace(p.opts.Owner); owner != "" {
		return owner
	}
	if p.listUsers == nil {
		return ""
	}
	users, err := p.listUsers(ctx)
	if err != nil {
		fmt.Fprintf(p.out, "warning: users.list failed: %v; greeting skipped\n", err)
		return ""
	}
	for _, u := range users {
		if u.IsPrimaryOwner && !u.IsBot && !u.Deleted {
			return u.ID
		}
	}
	return ""
}

// wireGreet builds the live listUsers / postDM seams from the
// validated bot token, auto-discovers the owner, and — if one was
// found — wires postDM with the owner baked in. Called at the end of
// a successful Login so Greet is ready when the orchestrator invokes
// it. Tests that bypass Login assign the seams directly.
//
// No owner → postDM stays nil → Greet is a no-op. This mirrors
// feishu: feishu wires sendDM only when an owner open_id was
// captured; without one, Greet is a no-op and a client would be
// wasted.
func (p *Provider) wireGreet(ctx context.Context) {
	client := slackgo.New(p.botToken)
	p.listUsers = func(ctx context.Context) ([]userView, error) {
		users, err := client.GetUsersContext(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]userView, 0, len(users))
		for _, u := range users {
			out = append(out, userView{
				ID:             u.ID,
				IsPrimaryOwner: u.IsPrimaryOwner,
				IsBot:          u.IsBot,
				Deleted:        u.Deleted,
			})
		}
		return out, nil
	}
	if owner := p.discoverOwner(ctx); owner != "" {
		p.ownerUserID = owner
		p.postDM = func(ctx context.Context, text string) error {
			// channel=<user ID> opens the DM implicitly; no
			// conversations.open needed (verified against
			// docs.slack.dev/reference/methods/chat.postMessage).
			_, _, err := client.PostMessageContext(ctx, owner, slackgo.MsgOptionText(text, false))
			return err
		}
	}
}

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
