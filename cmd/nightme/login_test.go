package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/login"
	_ "github.com/cnlangzi/nightme/internal/channel/feishu"
	_ "github.com/cnlangzi/nightme/internal/login/telegram"
)

// fakeProvider is a login.Provider that returns whatever the
// test set up. It is the test's handle to "what the QR flow
// would have produced".
type fakeProvider struct {
	creds     *login.Credentials
	err       error
	greetErr  error
	delay     time.Duration
	greetBody login.GreetingMessages
	greetHits int
}

func (f *fakeProvider) Name() string { return "feishu" }
func (f *fakeProvider) Login(ctx context.Context) (*login.Credentials, error) {
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(f.delay):
		}
	}
	return f.creds, f.err
}

// Greet implements login.Provider for the fake.
func (f *fakeProvider) Greet(_ context.Context, m login.GreetingMessages) error {
	f.greetBody = m
	f.greetHits++
	return f.greetErr
}

// withTempConfig points NIGHTME_CONFIG at a fresh temp file so
// the CLI write paths can run without polluting the user's real
// config. Returns the path so the test can read what was
// written.
func withTempConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	t.Setenv("NIGHTME_CONFIG", path)
	return path
}

// TestLogin_Success verifies the happy path: provider returns
// valid creds, login.LoginWith persists them and prints the
// success summary.
func TestLogin_Success(t *testing.T) {
	_ = withTempConfig(t)

	prov := &fakeProvider{
		creds: &login.Credentials{
			AppID:     "cli_test_app",
			AppSecret: "secret",
			AppName:   "Test App",
			CreatedAt: time.Now(),
		},
	}
	out := &bytes.Buffer{}
	if err := login.LoginWith(context.Background(), prov, out, &bytes.Buffer{}); err != nil {
		t.Fatalf("LoginWith: %v", err)
	}
	cfg, err := config.LoadDefault()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Feishu.AppID != "cli_test_app" {
		t.Errorf("AppID = %q, want cli_test_app", cfg.Feishu.AppID)
	}
	if cfg.Feishu.AppSecret != "secret" {
		t.Errorf("AppSecret = %q, want secret", cfg.Feishu.AppSecret)
	}
	if !strings.Contains(out.String(), "registered successfully") {
		t.Errorf("output missing success line: %q", out.String())
	}
}

// TestLogin_AlwaysRebinds verifies that re-running login
// overwrites any prior credentials without prompting.
func TestLogin_AlwaysRebinds(t *testing.T) {
	_ = withTempConfig(t)

	// Seed a prior credential so we can verify overwrite.
	prior := &login.Credentials{AppID: "prior", AppSecret: "prior-secret"}
	login.LoginWith(context.Background(), &fakeProvider{creds: prior}, &bytes.Buffer{}, &bytes.Buffer{})

	cfg, _ := config.LoadDefault()
	if cfg.Feishu.AppID != "prior" {
		t.Fatalf("seed failed: AppID = %q, want prior", cfg.Feishu.AppID)
	}

	// Re-bind.
	fresh := &login.Credentials{AppID: "fresh", AppSecret: "fresh-secret"}
	if err := login.LoginWith(context.Background(), &fakeProvider{creds: fresh}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	cfg, _ = config.LoadDefault()
	if cfg.Feishu.AppID != "fresh" {
		t.Errorf("AppID = %q, want fresh (overwrite)", cfg.Feishu.AppID)
	}
	if cfg.Feishu.AppSecret != "fresh-secret" {
		t.Errorf("AppSecret = %q, want fresh-secret", cfg.Feishu.AppSecret)
	}
}

// TestLogin_ProviderError wraps the SDK error path: any Login
// failure bubbles through verbatim so the user sees the real
// upstream message rather than a placeholder. Crucially, nothing
// is written to disk.
func TestLogin_ProviderError(t *testing.T) {
	_ = withTempConfig(t)

	prov := &fakeProvider{err: login.ErrLoginTimeout}

	err := login.LoginWith(context.Background(), prov, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "login timeout") {
		t.Errorf("error chain does not mention timeout: %v", err)
	}
	cfg, _ := config.LoadDefault()
	if cfg.Feishu.AppID != "" {
		t.Errorf("AppID written after provider failure: %q", cfg.Feishu.AppID)
	}
}

// TestLogin_ContextDeadline verifies that a slow provider is
// interrupted by the login context.
func TestLogin_ContextDeadline(t *testing.T) {
	_ = withTempConfig(t)

	prov := &fakeProvider{
		delay: 200 * time.Millisecond,
		creds: &login.Credentials{AppID: "x"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := login.LoginWith(ctx, prov, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error from ctx deadline")
	}
}

// TestLogin_CallsGreet verifies that login.LoginWith calls
// Greet after the save succeeds, and forwards GreetingTexts.
func TestLogin_CallsGreet(t *testing.T) {
	_ = withTempConfig(t)

	prov := &fakeProvider{
		creds: &login.Credentials{
			AppID:     "x",
			AppSecret: "y",
		},
	}
	if err := login.LoginWith(context.Background(), prov, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("LoginWith: %v", err)
	}
	if prov.greetHits != 1 {
		t.Fatalf("Greet hits = %d, want 1", prov.greetHits)
	}
	if len(prov.greetBody) == 0 {
		t.Fatal("Greet received empty greeting messages")
	}
}

// TestLogin_GreetError_DoesNotBlockConfigWrite verifies that a
// greeting failure is best-effort: the credentials are still
// persisted and the CLI exits nil.
func TestLogin_GreetError_DoesNotBlockConfigWrite(t *testing.T) {
	_ = withTempConfig(t)

	prov := &fakeProvider{
		creds:    &login.Credentials{AppID: "x", AppSecret: "y"},
		greetErr: errors.New("greeting API down"),
	}
	errOut := &bytes.Buffer{}
	if err := login.LoginWith(context.Background(), prov, &bytes.Buffer{}, errOut); err != nil {
		t.Fatalf("LoginWith must not propagate Greet errors: %v", err)
	}
	if !strings.Contains(errOut.String(), "greeting DM failed") {
		t.Errorf("errOut should mention greeting failure, got %q", errOut.String())
	}
	cfg, _ := config.LoadDefault()
	if cfg.Feishu.AppID != "x" {
		t.Errorf("AppID not persisted: %q", cfg.Feishu.AppID)
	}
}

// TestLogin_NilProvider verifies the helper rejects nil
// providers up-front rather than panicking.
func TestLogin_NilProvider(t *testing.T) {
	err := login.LoginWith(context.Background(), nil, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("nil provider must error")
	}
}

// TestNightmeLogin_RegistryContainsFeishuTelegram verifies the
// provider registry is populated by both packages' init() funcs.
func TestNightmeLogin_RegistryContainsFeishuTelegram(t *testing.T) {
	channels := login.AvailableChannels()
	want := map[string]bool{"feishu": false, "telegram": false}
	for _, ch := range channels {
		if _, ok := want[ch]; ok {
			want[ch] = true
		}
	}
	for ch, found := range want {
		if !found {
			t.Errorf("registry missing channel %q (have: %v)", ch, channels)
		}
	}
}

// TestNightmeLogin_BuildsCobraCommandTree verifies that the CLI
// orchestrator produces a working subcommand tree from the
// registry. The parent command must:
//   - list all available channels in --help
//   - return an error (not nil) when invoked without a subcommand
//   - let cobra report "unknown command" when invoked with an
//     invalid channel
func TestNightmeLogin_BuildsCobraCommandTree(t *testing.T) {
	cmd := newLoginCmd()

	// --help should list every registered channel.
	helpOutput, err := executeCmd(cmd, "--help")
	if err != nil {
		t.Fatalf("--help: %v", err)
	}
	for _, ch := range login.AvailableChannels() {
		if !strings.Contains(helpOutput, ch) {
			t.Errorf("--help output missing channel %q: %s", ch, helpOutput)
		}
	}

	// No subcommand: should error and exit non-zero.
	out, err := executeCmdBare(cmd)
	if err == nil {
		t.Fatal("`nightme login` with no args must error")
	}
	if !strings.Contains(err.Error(), "no channel specified") {
		t.Errorf("error should mention no channel specified, got %v", err)
	}
	if !strings.Contains(out, "Available channels") {
		t.Errorf("error output should list available channels, got %q", out)
	}

	// Unknown subcommand: cobra should print its own error.
	_, err = executeCmd(cmd, "no-such-channel")
	if err == nil {
		t.Fatal("`nightme login no-such-channel` must error")
	}
	if !strings.Contains(err.Error(), "no-such-channel") {
		t.Errorf("error should mention the bad channel, got %v", err)
	}
}

// executeCmd runs cmd with the given args and returns (stdout,
// err). Used by tests that need to exercise the cobra pipeline
// end-to-end.
func executeCmd(cmd *cobra.Command, args ...string) (string, error) {
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(append([]string{cmd.Name()}, args...))
	// ResetFlags clears parsed flag values between invocations so
	// multiple executions in one test are independent. We do NOT
	// call ResetCommands — that would strip the subcommands the
	// parent walked at construction time.
	cmd.ResetFlags()
	err := cmd.Execute()
	return out.String(), err
}

// executeCmdBare invokes cmd without any extra args. Use this
// when testing the parent command no-arg behaviour —
// executeCmd always prepends cmd.Name(), which cobra treats as
// an arg under ArbitraryArgs.
func executeCmdBare(cmd *cobra.Command) (string, error) {
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{})
	cmd.ResetFlags()
	err := cmd.Execute()
	return out.String(), err
}
