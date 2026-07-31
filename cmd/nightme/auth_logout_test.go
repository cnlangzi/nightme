package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/config"
)

// TestAuthLogout_ClearsConfig exercises the happy path: a configured
// Feishu block is replaced with zeroes and the result is persisted.
func TestAuthLogout_ClearsConfig(t *testing.T) {
	_ = withTempConfig(t)

	cfg, _ := config.LoadDefault()
	cfg.Feishu.AppID = "to-remove"
	cfg.Feishu.AppSecret = "to-remove-secret"
	cfg.Feishu.VerificationToken = "vt-rem"
	cfg.Feishu.EncryptKey = "ek-rem"
	if err := config.Save(cfg, os.Getenv("NIGHTME_CONFIG")); err != nil {
		t.Fatal(err)
	}

	cmd := newAuthLogoutFeishuCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	got, _ := config.LoadDefault()
	if got.Feishu.AppID != "" || got.Feishu.AppSecret != "" ||
		got.Feishu.VerificationToken != "" || got.Feishu.EncryptKey != "" {
		t.Errorf("not all fields cleared: %+v", got.Feishu)
	}
}

// TestAuthLogout_NoopOnClean makes sure `logout` on an unconfigured
// install doesn't error out. A failure here would block any
// scripted reinstall workflow.
func TestAuthLogout_NoopOnClean(t *testing.T) {
	_ = withTempConfig(t)

	cmd := newAuthLogoutFeishuCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("logout on clean config: %v", err)
	}
}

// TestHasFeishuCredentials sanity-checks the predicate used by the
// no-op path. If this gets out of sync with the struct definition,
// users would silently lose data on logout.
func TestHasFeishuCredentials(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Config
		want bool
	}{
		{"empty", config.Config{}, false},
		{"only app_id", config.Config{Feishu: config.FeishuConfig{AppID: "x"}}, true},
		{"only app_secret", config.Config{Feishu: config.FeishuConfig{AppSecret: "x"}}, true},
		{"only verification_token", config.Config{Feishu: config.FeishuConfig{VerificationToken: "x"}}, true},
		{"only encrypt_key", config.Config{Feishu: config.FeishuConfig{EncryptKey: "x"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasFeishuCredentials(&tc.cfg); got != tc.want {
				t.Errorf("hasFeishuCredentials = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAuthLogout_RunE_StdoutMentionsRevoke verifies the user-facing
// hint about revoking on the Feishu side is preserved across
// refactors. Failing tests here mean someone tightened the
// operator-facing copy too much.
func TestAuthLogout_RunE_StdoutMentionsRevoke(t *testing.T) {
	_ = withTempConfig(t)
	cfg, _ := config.LoadDefault()
	cfg.Feishu.AppID = "x"
	if err := config.Save(cfg, os.Getenv("NIGHTME_CONFIG")); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	cmd := newAuthLogoutFeishuCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&stderr)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if !strings.Contains(stderr.String(), "open.feishu.cn") {
		t.Errorf("logout stderr missing revoke hint: %s", stderr.String())
	}
}
