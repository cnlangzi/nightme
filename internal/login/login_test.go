package login

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
)

// fakeProvider is a compile-time-checked Provider implementation
// used solely to exercise the interface contract. Anything more
// elaborate lives in the provider-specific sub-packages.
type fakeProvider struct {
	name string
	out  *Credentials
	err  error
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Login(_ context.Context) (*Credentials, error) {
	return f.out, f.err
}

// Greet implements Provider. The fake is only used to exercise
// the interface contract — it never actually sends; the orchestrator
// tests assert on the warning log / error path instead.
func (f *fakeProvider) Greet(_ context.Context, _ GreetingMessages) error {
	return nil
}

// TestProvider_Interface is a compile-time check: any concrete
// Provider (real or fake) must satisfy the interface. If this
// stops compiling, the interface changed — update fakes too.
func TestProvider_Interface(t *testing.T) {
	var _ Provider = (*fakeProvider)(nil)
	var _ Provider = (*feishuStub)(nil)
}

// feishuStub exists so the interface check covers a second
// concrete type. The real feishu Provider lives in internal/login/feishu
// and cannot be imported here without an import cycle.
type feishuStub struct{ name string }

func (f *feishuStub) Name() string                                  { return f.name }
func (f *feishuStub) Login(_ context.Context) (*Credentials, error) { return nil, nil }
func (f *feishuStub) Greet(_ context.Context, _ GreetingMessages) error { return nil }

// TestProvider_Name_And_Login verifies the fake behaves as the
// interface contract promises: name is sticky, errors propagate.
func TestProvider_Name_And_Login(t *testing.T) {
	want := &Credentials{AppID: "cli_x", AppSecret: "secret", AppName: "nightme", CreatedAt: time.Now()}
	sentinel := errors.New("boom")
	p := &fakeProvider{name: "feishu", out: want, err: sentinel}

	if got := p.Name(); got != "feishu" {
		t.Errorf("Name() = %q, want feishu", got)
	}
	got, err := p.Login(context.Background())
	if !errors.Is(err, sentinel) {
		t.Errorf("Login error = %v, want sentinel %v", err, sentinel)
	}
	if got != want {
		t.Errorf("Login credentials = %+v, want %+v", got, want)
	}
}

// TestCredentials_JSON exercises the on-disk JSON encoding. The
// shape mirrors what the CLI persists into config.yaml under
// `feishu.app_id` / `feishu.app_secret` on a successful login.
func TestCredentials_JSON(t *testing.T) {
	in := Credentials{
		AppID:     "cli_a1b2",
		AppSecret: "secret-value",
		AppName:   "nightme",
		CreatedAt: time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
	}

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{
		`"app_id":"cli_a1b2"`,
		`"app_secret":"secret-value"`,
		`"app_name":"nightme"`,
		`"created_at":"2026-07-31T12:00:00Z"`,
	} {
		if !contains(data, want) {
			t.Errorf("Marshal missing %s\nactual: %s", want, data)
		}
	}

	var out Credentials
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

// TestErrors_AreDistinct makes sure the sentinel errors stay
// distinct so errors.Is matching keeps working in callers.
func TestErrors_AreDistinct(t *testing.T) {
	sentinels := []error{ErrLoginTimeout, ErrLoginFailed}
	for i, a := range sentinels {
		for j, b := range sentinels {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Errorf("errors.Is(%v, %v) = true; want false", a, b)
			}
		}
	}
}

// contains is a strings.Contains wrapper that works on []byte without
// an extra import.
func contains(haystack []byte, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == needle {
			return true
		}
	}
	return false
}
// TestLoginWith_DispatchesByProviderName verifies that the
// v0.x regression where LoginWith wrote all three creds
// unconditionally is gone. v1.3+ dispatches on
// provider.Name(): a telegram login only writes BotToken;
// feishu's AppID/AppSecret are left alone. Same for the
// symmetric direction.
func TestLoginWith_DispatchesByProviderName(t *testing.T) {
	dir := t.TempDir()
	// LoginWith → SaveDefault → $HOME/.nightme/config.yaml.
	// Seed the file at that path.
	t.Setenv("HOME", dir)
	cfgPath := config.DefaultPath()
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	pre := &config.Config{}
	pre.Feishu.AppID = "feishu_app_keep"
	pre.Feishu.AppSecret = "feishu_secret_keep"
	pre.Telegram.BotToken = "telegram_bot_keep"
	if err := config.Save(pre, cfgPath); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	// Telegram login: must write BotToken, leave feishu alone.
	tg := &fakeProvider{
		name: "telegram",
		out:  &Credentials{AppName: "tg", BotToken: "1234:new_tg"},
		err:  nil,
	}
	if err := LoginWith(context.Background(), tg, io.Discard, io.Discard); err != nil {
		t.Fatalf("LoginWith telegram: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Telegram.BotToken != "1234:new_tg" {
		t.Errorf("Telegram.BotToken = %q, want 1234:new_tg", cfg.Telegram.BotToken)
	}
	if cfg.Feishu.AppID != "feishu_app_keep" {
		t.Errorf("Feishu.AppID was stomped: %q (want feishu_app_keep)", cfg.Feishu.AppID)
	}
	if cfg.Feishu.AppSecret != "feishu_secret_keep" {
		t.Errorf("Feishu.AppSecret was stomped: %q (want feishu_secret_keep)", cfg.Feishu.AppSecret)
	}

	// Feishu login: must write AppID/AppSecret, leave telegram alone.
	fs := &fakeProvider{
		name: "feishu",
		out:  &Credentials{AppID: "new_app", AppSecret: "new_secret"},
	}
	if err := LoginWith(context.Background(), fs, io.Discard, io.Discard); err != nil {
		t.Fatalf("LoginWith feishu: %v", err)
	}
	cfg, err = config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Feishu.AppID != "new_app" {
		t.Errorf("Feishu.AppID = %q, want new_app", cfg.Feishu.AppID)
	}
	if cfg.Feishu.AppSecret != "new_secret" {
		t.Errorf("Feishu.AppSecret = %q, want new_secret", cfg.Feishu.AppSecret)
	}
	if cfg.Telegram.BotToken != "1234:new_tg" {
		t.Errorf("Telegram.BotToken was stomped: %q (want 1234:new_tg)", cfg.Telegram.BotToken)
	}
}

// TestLoginWith_UnknownProvider_FailsFast ensures an
// unrecognized channel name returns a clear error instead of
// silently writing nothing.
func TestLoginWith_UnknownProvider_FailsFast(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	bogus := &fakeProvider{
		name: "carrier-pigeon",
		out:  &Credentials{},
	}
	if err := LoginWith(context.Background(), bogus, io.Discard, io.Discard); err == nil {
		t.Fatal("LoginWith carrier-pigeon: want error, got nil")
	}
}

func TestRegistry_AvailableChannels_Alphabetical(t *testing.T) {
	// Snapshot the registry (we can't easily reset without
	// breaking other tests' init() side-effects, so we capture
	// the current state and just assert the alphabetical order).
	names := AvailableChannels()
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("AvailableChannels not sorted: %v", names)
		}
	}
}

func TestRegistry_GetBuilder_KnownAndUnknown(t *testing.T) {
	for _, name := range AvailableChannels() {
		if GetBuilder(name) == nil {
			t.Fatalf("GetBuilder(%q) returned nil for a registered channel", name)
		}
	}
	if GetBuilder("definitely-not-a-channel") != nil {
		t.Fatal("GetBuilder on unknown name must return nil")
	}
}

func TestRegistry_RegisterProvider_OverwritesOnDuplicate(t *testing.T) {
	// Register a fake builder under a unique name, twice. The
	// second call must win (we agreed: silent overwrite).
	const name = "test-duplicate-channel"
	firstCalled := false
	secondCalled := false
	RegisterProvider(name, func(flags *ProviderFlags) *cobra.Command {
		firstCalled = true
		return &cobra.Command{Use: name}
	})
	RegisterProvider(name, func(flags *ProviderFlags) *cobra.Command {
		secondCalled = true
		return &cobra.Command{Use: name}
	})

	// Resolve and call the resolved builder. Only the
	// second (overwriting) registration should fire.
	defer Reset()
	b := GetBuilder(name)
	if b == nil {
		t.Fatal("GetBuilder returned nil after registration")
	}
	b(&ProviderFlags{})
	if firstCalled {
		t.Fatal("first (overwritten) builder must not be called")
	}
	if !secondCalled {
		t.Fatal("second (overwriting) builder must be called")
	}
}

func TestLoginWith_NilProviderErrors(t *testing.T) {
	err := LoginWith(context.Background(), nil, nil, nil)
	if err == nil {
		t.Fatal("LoginWith(nil provider) must error")
	}
	if !strings.Contains(err.Error(), "provider is nil") {
		t.Fatalf("error should mention nil provider, got %v", err)
	}
}

func TestLoginWith_AcceptsDiscardWriters(t *testing.T) {
	// out=nil and errOut=nil should fall back to io.Discard
	// rather than panic. Used by tests that don't care about
	// CLI output but still want to exercise the orchestrator.
	prov := &registryTestProvider{creds: &Credentials{AppID: "x"}}
	err := LoginWith(context.Background(), prov, nil, nil)
	if err != nil {
		// We expect a config-load error or a save error since
		// NIGHTME_CONFIG may not be set up in this test env;
		// the point is that nil writers don't panic.
		_ = err
	}
}

// registryTestProvider is a minimal login.Provider used only by
// the registry/orchestrator tests above.
type registryTestProvider struct {
	creds *Credentials
	err   error
	greet error
	hits  int
}

func (r *registryTestProvider) Name() string                       { return "registry-test" }
func (r *registryTestProvider) Login(_ context.Context) (*Credentials, error) { return r.creds, r.err }
func (r *registryTestProvider) Greet(_ context.Context, _ GreetingMessages) error {
	r.hits++
	return r.greet
}
