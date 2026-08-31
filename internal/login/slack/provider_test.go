package slack

import (
	"bytes"
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

func TestGreet_SkipsWhenNoBotToken(t *testing.T) {
	// Login was bypassed (no bot token), so Greet must be a silent
	// no-op — it must not even touch the seams. Mirrors feishu's
	// nil-sendDM guard.
	p := &Provider{
		listUsers: func(context.Context) ([]userView, error) {
			t.Fatal("listUsers must not be called when botToken is empty")
			return nil, nil
		},
		postDM: func(context.Context, string) error {
			t.Fatal("postDM must not be called when botToken is empty")
			return nil
		},
	}
	if err := p.Greet(context.Background(), login.GreetingTexts()); err != nil {
		t.Fatalf("Greet: %v", err)
	}
}

func TestGreet_SkipsWhenNoOwnerDiscovered(t *testing.T) {
	// Login ran (botToken set) and the slackgo client was wired,
	// but the listUsers seam returned an error → discovery fails →
	// postDM stays nil → Greet must print the skip hint and return
	// nil. Replaces the old polling "TimesOut / Stale" tests; there
	// is no poll anymore.
	out := &bytes.Buffer{}
	p := &Provider{
		botToken: "xoxb-x",
		out:      out,
		listUsers: func(context.Context) ([]userView, error) {
			return nil, errors.New("transient slack outage")
		},
		// postDM deliberately left nil: discovery is now lazy and
		// will fail because listUsers errors above.
	}

	if err := p.Greet(context.Background(), login.GreetingTexts()); err != nil {
		t.Fatalf("Greet with nil postDM should be a no-op, got %v", err)
	}
	if !strings.Contains(out.String(), "no owner discovered") {
		t.Fatalf("output missing skip hint: %q", out.String())
	}
}

// Login bypassed in tests means botToken == "" (the canonical
// short-circuit). Greet must not even consult the listUsers seam.
func TestGreet_LoginBypassed_NoClient(t *testing.T) {
	p := &Provider{
		out: io.Discard,
		listUsers: func(context.Context) ([]userView, error) {
			t.Fatal("listUsers must not be called when botToken is empty")
			return nil, nil
		},
	}
	if err := p.Greet(context.Background(), login.GreetingTexts()); err != nil {
		t.Fatalf("Greet: %v", err)
	}
}

// discoverOwnerAndPostDM honors opts.Owner without calling users.list.
func TestDiscoverOwner_OwnerFlagOverrides(t *testing.T) {
	p := &Provider{
		opts: Options{Owner: "U_OVERRIDE"},
		out:  io.Discard,
		listUsers: func(context.Context) ([]userView, error) {
			t.Fatal("listUsers must not be called when opts.Owner is set")
			return nil, nil
		},
	}

	p.discoverOwnerAndPostDM(context.Background())
	if p.ownerUserID != "U_OVERRIDE" {
		t.Fatalf("owner = %q, want %q", p.ownerUserID, "U_OVERRIDE")
	}
}

// discoverOwnerAndPostDM calls listUsers and picks is_primary_owner.
func TestDiscoverOwner_PicksPrimaryOwner(t *testing.T) {
	p := &Provider{
		out: io.Discard,
		listUsers: func(context.Context) ([]userView, error) {
			return []userView{
				{ID: "U_BOT", IsPrimaryOwner: false, IsBot: true},
				{ID: "U_HUMAN1", IsPrimaryOwner: false, IsBot: false},
				{ID: "U_OWNER", IsPrimaryOwner: true, IsBot: false},
				{ID: "U_HUMAN2", IsPrimaryOwner: false, IsBot: false},
			}, nil
		},
	}

	p.discoverOwnerAndPostDM(context.Background())
	if p.ownerUserID != "U_OWNER" {
		t.Fatalf("owner = %q, want %q", p.ownerUserID, "U_OWNER")
	}
}

// discoverOwnerAndPostDM skips bots and deleted accounts even when
// they carry is_primary_owner.
func TestDiscoverOwner_SkipsBotsAndDeleted(t *testing.T) {
	p := &Provider{
		out: io.Discard,
		listUsers: func(context.Context) ([]userView, error) {
			return []userView{
				{ID: "U_BOT_OWNER", IsPrimaryOwner: true, IsBot: true},
				{ID: "U_GHOST", IsPrimaryOwner: true, IsBot: false, Deleted: true},
				{ID: "U_OWNER", IsPrimaryOwner: true, IsBot: false},
			}, nil
		},
	}

	p.discoverOwnerAndPostDM(context.Background())
	if p.ownerUserID != "U_OWNER" {
		t.Fatalf("owner = %q, want %q (skipped bot + deleted)", p.ownerUserID, "U_OWNER")
	}
}

// discoverOwnerAndPostDM leaves ownerUserID empty when listUsers errors
// so Greet can skip without failing.
func TestDiscoverOwner_ListUsersErrorLeavesOwnerEmpty(t *testing.T) {
	out := &bytes.Buffer{}
	p := &Provider{
		out: out,
		listUsers: func(context.Context) ([]userView, error) {
			return nil, errors.New("transient slack outage")
		},
	}

	p.discoverOwnerAndPostDM(context.Background())
	if p.ownerUserID != "" {
		t.Fatalf("ownerUserID = %q, want empty", p.ownerUserID)
	}
	if !strings.Contains(out.String(), "users.list failed") {
		t.Fatalf("warning should mention users.list, got %q", out.String())
	}
}

func TestGreet_SendsEnglishOnlyToOwner(t *testing.T) {
	// Happy path: ownerUserID + postDM are wired. GreetingTexts()
	// ships 2 bodies, each with both Chinese and English. They are
	// combined into a SINGLE postDM call (Slack would otherwise
	// group consecutive bot messages and hide the first body) joined
	// by "\n\n" — Slack's mrkdwn paragraph break. Chinese halves
	// are dropped (Slack has no Feishu-style bilingual block).
	out := &bytes.Buffer{}
	p := &Provider{
		botToken:    "xoxb-x",
		botName:     "nightme",
		ownerUserID: "U_OWNER",
		out:         out,
	}
	var posted []string
	p.postDM = func(_ context.Context, text string) error {
		posted = append(posted, text)
		return nil
	}

	if err := p.Greet(context.Background(), login.GreetingTexts()); err != nil {
		t.Fatalf("Greet: %v", err)
	}

	// Exactly one postDM call — the two English bodies are joined
	// into one message so Slack's consecutive-message grouping can't
	// hide the first half.
	if len(posted) != 1 {
		t.Fatalf("expected 1 combined post, got %d (%q)", len(posted), posted)
	}
	body := posted[0]
	if containsCJK(body) {
		t.Fatalf("Chinese body leaked into Slack greeting: %q", body)
	}
	want := login.GreetingMessageEnglish1 + "\n\n" + login.GreetingMessageEnglish2
	if body != want {
		t.Fatalf("posted body mismatch\n got: %q\nwant: %q", body, want)
	}
	// Both halves must be visible as separate paragraphs in the
	// joined text (defends the "\n\n" separator choice — a single
	// "\n" would render as a soft line break in Slack, which a
	// regression could quietly change without breaking len==1).
	for _, want := range []string{login.GreetingMessageEnglish1, login.GreetingMessageEnglish2} {
		if !strings.Contains(body, want) {
			t.Fatalf("combined body missing %q: %q", want, body)
		}
	}
	if !strings.Contains(out.String(), "U_OWNER") {
		t.Fatalf("Greet should print the recipient owner ID, got %q", out.String())
	}
}

// manifestView is the subset of AppManifest that tests actually
// assert on. yaml.v3 maps field names case-insensitively so a single
// struct covers both the YAML body and the URL-encoded copy.
type manifestView struct {
	Features struct {
		AppHome struct {
			MessagesTabEnabled         bool `yaml:"messages_tab_enabled"`
			MessagesTabReadOnlyEnabled bool `yaml:"messages_tab_read_only_enabled"`
		} `yaml:"app_home"`
	} `yaml:"features"`
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

	// messages_tab_enabled MUST be true: without it Slack shows the
	// app's DM as "sending messages to this app has been turned
	// off", users can't DM the bot, and the message.im event the
	// adapter subscribes to never fires (docs.slack.dev/surfaces/
	// app-home#messages-tab). It defaults to false when omitted,
	// so we set it explicitly and pin the assertion here.
	if !parsed.Features.AppHome.MessagesTabEnabled {
		t.Fatal("features.app_home.messages_tab_enabled must be true — see manifest.go comment for why")
	}

	// messages_tab_read_only_enabled is the inverse-named "users can
	// send messages" toggle: false means "not read-only" → users
	// CAN DM the bot and message.im events fire. The Slack default
	// when omitted is true (read-only), which silently breaks the
	// entire adapter. Pin it to false here.
	if parsed.Features.AppHome.MessagesTabReadOnlyEnabled {
		t.Fatal("features.app_home.messages_tab_read_only_enabled must be false — Slack's name is the inverse of its effect; true means read-only")
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

// containsCJK reports whether s carries any CJK Unified Ideograph
// (U+4E00–U+9FFF). The canonical English greetings contain emoji
// (👋 / 🚀) — non-ASCII but NOT CJK — so a plain ASCII check would
// false-positive on them. CJK ideographs only appear in the Chinese
// halves, which is exactly what we must detect here.
func containsCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}
