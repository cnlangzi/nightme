package slack

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/login"
	"gopkg.in/yaml.v3"
)

func stubAuth(team, bot string, err error) func(context.Context, string, string) (string, string, error) {
	return func(context.Context, string, string) (string, string, error) {
		return team, bot, err
	}
}

func TestProvider_Name(t *testing.T) {
	if got := New(Options{}).Name(); got != "slack" {
		t.Fatalf("Name = %q", got)
	}
}

func TestLogin_NonInteractiveWithBothFlags(t *testing.T) {
	p := New(Options{
		BotToken: "xoxb-abc",
		AppToken: "xapp-def",
		Out:      io.Discard,
		authTest: stubAuth("Acme", "nightme", nil),
	})

	creds, err := p.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if creds.BotToken != "xoxb-abc" || creds.AppToken != "xapp-def" {
		t.Fatalf("credentials = %+v", creds)
	}
	if creds.AppName != "nightme @ Acme" {
		t.Fatalf("AppName = %q", creds.AppName)
	}
	if creds.CreatedAt.IsZero() {
		t.Fatal("CreatedAt should be stamped locally")
	}
}

func TestLogin_ReadsBothTokensFromStdin(t *testing.T) {
	p := New(Options{
		In:       strings.NewReader("xoxb-from-stdin\nxapp-from-stdin\n"),
		Out:      io.Discard,
		authTest: stubAuth("Acme", "nightme", nil),
	})

	creds, err := p.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if creds.BotToken != "xoxb-from-stdin" || creds.AppToken != "xapp-from-stdin" {
		t.Fatalf("credentials = %+v", creds)
	}
}

func TestLogin_SkipsBlankLines(t *testing.T) {
	p := New(Options{
		In:       strings.NewReader("\n   \nxoxb-a\n\nxapp-b\n"),
		Out:      io.Discard,
		authTest: stubAuth("", "nightme", nil),
	})

	creds, err := p.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if creds.BotToken != "xoxb-a" || creds.AppToken != "xapp-b" {
		t.Fatalf("credentials = %+v", creds)
	}
}

// Swapping the two tokens is the most likely paste error; catching
// it here beats a confusing API rejection later.
func TestLogin_RejectsSwappedTokens(t *testing.T) {
	p := New(Options{
		BotToken: "xapp-oops",
		AppToken: "xoxb-oops",
		Out:      io.Discard,
		authTest: stubAuth("Acme", "nightme", nil),
	})

	_, err := p.Login(context.Background())
	if err == nil {
		t.Fatal("swapped tokens should be rejected")
	}
	if !errors.Is(err, login.ErrLoginFailed) {
		t.Fatalf("error should wrap ErrLoginFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "xoxb-") {
		t.Fatalf("the message should say which prefix was expected, got %q", err)
	}
}

func TestLogin_RejectsBadAppTokenPrefix(t *testing.T) {
	p := New(Options{
		BotToken: "xoxb-fine",
		AppToken: "not-a-token",
		Out:      io.Discard,
		authTest: stubAuth("Acme", "nightme", nil),
	})

	_, err := p.Login(context.Background())
	if err == nil || !strings.Contains(err.Error(), "connections:write") {
		t.Fatalf("error should point at the app-level token, got %v", err)
	}
}

// The error must not echo the whole secret back into logs.
func TestLogin_ErrorDoesNotLeakFullToken(t *testing.T) {
	secret := "xapp-super-secret-value-do-not-print"
	p := New(Options{
		BotToken: secret,
		AppToken: "xapp-fine",
		Out:      io.Discard,
		authTest: stubAuth("Acme", "nightme", nil),
	})

	_, err := p.Login(context.Background())
	if err == nil {
		t.Fatal("expected rejection")
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("error leaked the token body: %q", err)
	}
}

func TestLogin_SurfacesSlackRejection(t *testing.T) {
	p := New(Options{
		BotToken: "xoxb-a",
		AppToken: "xapp-b",
		Out:      io.Discard,
		authTest: stubAuth("", "", errors.New("invalid_auth")),
	})

	_, err := p.Login(context.Background())
	if err == nil || !errors.Is(err, login.ErrLoginFailed) {
		t.Fatalf("expected a wrapped ErrLoginFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "invalid_auth") {
		t.Fatalf("the underlying Slack error should be visible, got %q", err)
	}
}

func TestLogin_TimesOutWaitingForInput(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()

	p := New(Options{In: pr, Out: io.Discard, authTest: stubAuth("", "", nil)})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := p.Login(ctx)
	if !errors.Is(err, login.ErrLoginTimeout) {
		t.Fatalf("expected ErrLoginTimeout, got %v", err)
	}
}

func TestLogin_PrintsWalkthroughWhenInteractive(t *testing.T) {
	var out strings.Builder
	p := New(Options{
		In:       strings.NewReader("xoxb-a\nxapp-b\n"),
		Out:      &out,
		authTest: stubAuth("Acme", "nightme", nil),
	})
	if _, err := p.Login(context.Background()); err != nil {
		t.Fatalf("Login: %v", err)
	}

	text := out.String()
	for _, want := range []string{
		"api.slack.com/apps",
		"socket_mode_enabled",
		"Install to <your workspace>",
		// The walkthrough now carries the manifest itself so users
		// on networks that block Slack's deep link do not have to
		// run a second command to fetch it.
		AppManifest,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("walkthrough is missing %q:\n%s", want, text)
		}
	}
}

func TestLogin_SkipsWalkthroughWhenTokensSupplied(t *testing.T) {
	var out strings.Builder
	p := New(Options{
		BotToken: "xoxb-a",
		AppToken: "xapp-b",
		Out:      &out,
		authTest: stubAuth("Acme", "nightme", nil),
	})
	if _, err := p.Login(context.Background()); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if strings.Contains(out.String(), "api.slack.com/apps") {
		t.Fatal("a fully non-interactive run should print nothing")
	}
}

func TestGreet_IsANoOp(t *testing.T) {
	// Slack's install flow yields tokens and no recipient, so there
	// is nobody to greet. Documented, not forgotten.
	if err := New(Options{}).Greet(context.Background(), login.GreetingTexts()); err != nil {
		t.Fatalf("Greet should be a silent no-op, got %v", err)
	}
}

// manifestView is the subset of AppManifest that tests actually
// assert on. yaml.v3 maps field names case-insensitively so a single
// struct covers both the YAML body and the URL-encoded copy.
type manifestView struct {
	OAuthConfig struct {
		Scopes struct {
			Bot []string `yaml:"bot"`
		} `yaml:"scopes"`
	} `yaml:"oauth_config"`
	Settings struct {
		SocketModeEnabled  bool `yaml:"socket_mode_enabled"`
		EventSubscriptions struct {
			BotEvents []string `yaml:"bot_events"`
		} `yaml:"event_subscriptions"`
	} `yaml:"settings"`
}

func parseManifest(raw string) (*manifestView, error) {
	var v manifestView
	if err := yaml.Unmarshal([]byte(raw), &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func TestAppManifest_IsValidYAML(t *testing.T) {
	if _, err := parseManifest(AppManifest); err != nil {
		t.Fatalf("the manifest users paste into Slack must be valid YAML: %v", err)
	}
}

// The manifest is the contract for what the adapter is allowed to do
// at runtime. These assertions pin the decisions in
// docs/channel/slack.md §6.
func TestAppManifest_CarriesRequiredScopesAndEvents(t *testing.T) {
	parsed, err := parseManifest(AppManifest)
	if err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}

	if !parsed.Settings.SocketModeEnabled {
		t.Fatal("Socket Mode must be enabled — it is the only transport the adapter implements")
	}

	scopes := make(map[string]bool)
	for _, s := range parsed.OAuthConfig.Scopes.Bot {
		scopes[s] = true
	}
	for _, want := range []string{
		"chat:write",        // posting at all
		"assistant:write",   // assistant.threads.setStatus (heartbeat)
		"reactions:write",   // message-state track
		"reactions:read",    // needed to replace rather than stack
		"files:read",        // downloading attachments
		"channels:history",  // /watch all in public channels
		"groups:history",    // ... private channels
		"im:history",        // ... DMs
		"mpim:history",      // ... group DMs
		"app_mentions:read", // mention-only default
	} {
		if !scopes[want] {
			t.Fatalf("manifest is missing the %q scope", want)
		}
	}

	events := make(map[string]bool)
	for _, e := range parsed.Settings.EventSubscriptions.BotEvents {
		events[e] = true
	}
	// Without the message.* family the bot only ever sees mentions,
	// which makes /watch all impossible to implement.
	for _, want := range []string{
		"app_mention", "message.im", "message.channels",
		"message.groups", "message.mpim",
	} {
		if !events[want] {
			t.Fatalf("manifest is missing the %q event subscription", want)
		}
	}
}

func TestManifestURL_IsAWellFormedDeepLink(t *testing.T) {
	raw := ManifestURL()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("manifest URL does not parse: %v", err)
	}
	if u.Host != "api.slack.com" || u.Path != "/apps" {
		t.Fatalf("unexpected target %s%s", u.Host, u.Path)
	}
	q := u.Query()
	if q.Get("new_app") != "1" {
		t.Fatal("new_app=1 is what opens the creation flow")
	}
	// Slack's deep link accepts the manifest as YAML (manifest_yaml=…)
	// today, and as JSON (manifest_json=…) is kept as a fallback in
	// case the YAML form ever fails. The manifest must survive
	// URL-encoding intact either way or Slack shows an empty form
	// and the whole point is lost.
	encoded := q.Get("manifest_yaml")
	if encoded == "" {
		encoded = q.Get("manifest_json")
	}
	if encoded == "" {
		t.Fatal("neither manifest_yaml nor manifest_json is present")
	}
	parsed, err := parseManifest(encoded)
	if err != nil {
		t.Fatalf("embedded manifest is not valid after encoding: %v", err)
	}
	if !parsed.Settings.SocketModeEnabled {
		t.Fatal("embedded manifest lost its contents")
	}
}
