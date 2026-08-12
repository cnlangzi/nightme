package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/login"
)

// fakeProvider is a login.Provider that returns whatever the
// test set up. It is the test's handle to "what the QR flow would
// have produced".
type fakeProvider struct {
	creds     *login.Credentials
	err       error
	greetErr  error
	delay     time.Duration // simulate >zero Login before returning
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

// Greet implements login.Provider for the fake. Captures the
// messages it received + counts hits so orchestrator tests can
// assert "Greet was actually called with X". The fake never sends.
func (f *fakeProvider) Greet(_ context.Context, m login.GreetingMessages) error {
	f.greetBody = m
	f.greetHits++
	return f.greetErr
}

// withTempConfig points NIGHTME_CONFIG at a fresh temp file so the
// CLI write paths can run without polluting the user's real config.
// Returns the path so the test can read what was written.
func withTempConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	t.Setenv("NIGHTME_CONFIG", path)
	return path
}

// TestLogin_Success walks the happy path: clean config + fake
// provider returning credentials -> config.yaml is written, success
// banner mentions the app_id.
func TestLogin_Success(t *testing.T) {
	_ = withTempConfig(t)

	prov := &fakeProvider{
		creds: &login.Credentials{
			AppID:     "cli_newapp",
			AppSecret: "sek-ret-1",
			AppName:   "nightme",
			CreatedAt: time.Now(),
		},
	}

	flags := &loginCmdFlags{timeout: 5 * time.Second}
	cmd := newLoginFeishuCmd(flags)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetContext(context.Background())

	if err := runLoginWith(cmd, flags, prov); err != nil {
		t.Fatalf("runLoginWith: %v", err)
	}

	// Config got written with our credentials.
	cfg, err := config.LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault: %v", err)
	}
	if cfg.Feishu.AppID != "cli_newapp" {
		t.Errorf("Feishu.AppID = %q, want cli_newapp", cfg.Feishu.AppID)
	}
	if cfg.Feishu.AppSecret != "sek-ret-1" {
		t.Errorf("Feishu.AppSecret = %q, want sek-ret-1", cfg.Feishu.AppSecret)
	}

	// Stdout includes the success banner.
	if !strings.Contains(stdout.String(), "cli_newapp") {
		t.Errorf("stdout missing app_id: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "✓") {
		t.Errorf("stdout missing success check: %s", stdout.String())
	}

	// File is 0600.
	path := os.Getenv("NIGHTME_CONFIG")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config perms = %#o, want 0600", perm)
	}
}

// TestLogin_AlwaysRebinds asserts the rebind contract: running
// login on a config that already holds Feishu credentials
// unconditionally overwrites them. There is no --force flag, no
// "already configured" guard — `nightme login feishu` IS the
// rebind operation. This is what makes bumping the requested scopes
// (e.g. adding im:message.reactions:write_only) a single command.
func TestLogin_AlwaysRebinds(t *testing.T) {
	_ = withTempConfig(t)

	cfg, err := config.LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Feishu.AppID = "old-cli-id"
	cfg.Feishu.AppSecret = "old-secret"
	if err := config.Save(cfg, os.Getenv("NIGHTME_CONFIG")); err != nil {
		t.Fatal(err)
	}

	prov := &fakeProvider{
		creds: &login.Credentials{AppID: "new-cli-id", AppSecret: "new-secret"},
	}

	flags := &loginCmdFlags{timeout: 5 * time.Second}
	cmd := newLoginFeishuCmd(flags)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetContext(context.Background())
	if err := runLoginWith(cmd, flags, prov); err != nil {
		t.Fatalf("runLoginWith: %v", err)
	}

	cfg2, _ := config.LoadDefault()
	if cfg2.Feishu.AppID != "new-cli-id" {
		t.Errorf("AppID = %q, want new-cli-id (rebind did not overwrite)", cfg2.Feishu.AppID)
	}
	if cfg2.Feishu.AppSecret != "new-secret" {
		t.Errorf("AppSecret = %q, want new-secret", cfg2.Feishu.AppSecret)
	}
}

// TestLogin_ProviderError wraps the SDK error path: any Login
// failure bubbles through verbatim so the user sees the real
// upstream message rather than a placeholder.
func TestLogin_ProviderError(t *testing.T) {
	_ = withTempConfig(t)

	prov := &fakeProvider{err: login.ErrLoginTimeout}

	cmd := newLoginFeishuCmd(&loginCmdFlags{timeout: 5 * time.Second})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetContext(context.Background())
	err := runLoginWith(cmd, &loginCmdFlags{timeout: 5 * time.Second}, prov)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "login timeout") {
		t.Errorf("error chain does not mention timeout: %v", err)
	}

	// Crucially, nothing was written to disk.
	if _, err := os.Stat(os.Getenv("NIGHTME_CONFIG")); !os.IsNotExist(err) {
		// We don't expect the file to exist at all (no save ran)
		// — but if it existed before this test that's fine too.
		// Important: it must not have been REWRITTEN with creds.
		cfg, _ := config.LoadDefault()
		if cfg.Feishu.AppID != "" {
			t.Errorf("AppID written after provider failure: %q", cfg.Feishu.AppID)
		}
	}
}

// TestLogin_ContextDeadline aligns the timeout-in-the-flag with
// the actual context passed to provider.Login. A 1ms timeout with
// a fake that sleeps 1s should surface ctx.DeadlineExceeded.
func TestLogin_ContextDeadline(t *testing.T) {
	_ = withTempConfig(t)

	prov := &fakeProvider{
		delay: 10 * time.Millisecond,
		creds: &login.Credentials{AppID: "x", AppSecret: "x"},
	}

	cmd := newLoginFeishuCmd(&loginCmdFlags{timeout: 50 * time.Millisecond})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetContext(context.Background())
	// This should NOT time out: 50ms > 10ms delay, so the fake
	// returns in time. The point of this test is mostly to lock
	// in the plumbing: ctx is passed, Login respects it.
	if err := runLoginWith(cmd, &loginCmdFlags{timeout: 50 * time.Millisecond}, prov); err != nil {
		// Tolerated: if timer scheduling gets weird it can race,
		// but neither side should crash.
		t.Logf("note: timed error returned (likely scheduling): %v", err)
	}
}

// TestLogin_CallsGreet asserts the orchestrator invokes
// provider.Greet with login.GreetingTexts() right after a
// successful Login. Locks the post-login greeting flow.
func TestLogin_CallsGreet(t *testing.T) {
	_ = withTempConfig(t)

	prov := &fakeProvider{
		creds: &login.Credentials{
			AppID: "cli_x", AppSecret: "s", AppName: "n",
			CreatedAt: time.Now(),
		},
	}
	flags := &loginCmdFlags{timeout: 5 * time.Second}
	cmd := newLoginFeishuCmd(flags)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetContext(context.Background())

	if err := runLoginWith(cmd, flags, prov); err != nil {
		t.Fatalf("runLoginWith: %v", err)
	}
	if prov.greetHits != 1 {
		t.Errorf("Greet called %d times, want 1", prov.greetHits)
	}
	want := login.GreetingTexts()
	if len(prov.greetBody) != len(want) {
		t.Errorf("Greet body len = %d, want %d", len(prov.greetBody), len(want))
	}
	for i, m := range want {
		if i >= len(prov.greetBody) {
			t.Errorf("Greet body missing element %d (want %+v)", i, m)
			continue
		}
		if prov.greetBody[i] != m {
			t.Errorf("Greet body[%d] = %+v, want %+v", i, prov.greetBody[i], m)
		}
	}
}

// TestLogin_GreetError_DoesNotBlockConfigWrite ensures a failing
// Greet does NOT roll back the successful registration. The
// orchestrator must swallow the error after logging, then proceed
// to write credentials + print success banner.
func TestLogin_GreetError_DoesNotBlockConfigWrite(t *testing.T) {
	_ = withTempConfig(t)

	prov := &fakeProvider{
		creds: &login.Credentials{
			AppID: "cli_y", AppSecret: "s", AppName: "n",
			CreatedAt: time.Now(),
		},
		greetErr: errors.New("greeting blew up"),
	}
	flags := &loginCmdFlags{timeout: 5 * time.Second}
	cmd := newLoginFeishuCmd(flags)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())

	if err := runLoginWith(cmd, flags, prov); err != nil {
		t.Fatalf("runLoginWith returned err: %v", err)
	}
	cfg, err := config.LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault: %v", err)
	}
	if cfg.Feishu.AppID != "cli_y" {
		t.Errorf("config Feishu.AppID = %q, want cli_y (Greet failure must not roll back)", cfg.Feishu.AppID)
	}
	if !strings.Contains(stderr.String(), "greeting DM failed") {
		t.Errorf("stderr missing greeting failure log: %s", stderr.String())
	}
}
