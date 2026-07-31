package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/config"
)

// TestAuthStatus_Unconfigured covers the cold-start case: status
// must say "not configured" rather than failing or printing empty
// lines. Without this guard, `nightme auth status feishu` on a
// fresh install looks broken.
func TestAuthStatus_Unconfigured(t *testing.T) {
	_ = withTempConfig(t)

	var buf bytes.Buffer
	if err := printFeishuStatus(&buf, &config.Config{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "not configured") {
		t.Errorf("status missing 'not configured' message: %s", buf.String())
	}
	if strings.Contains(buf.String(), "app_id:") {
		t.Errorf("status unexpectedly prints app_id in cold-start case: %s", buf.String())
	}
}

// TestAuthStatus_ShowsAppId confirms the configured path: app_id is
// echoed, secret is NOT (the spec at F-22 §3 explicitly says
// "status 显示当前凭证 ... 不显示 secret").
func TestAuthStatus_ShowsAppId(t *testing.T) {
	_ = withTempConfig(t)

	cfg, _ := config.LoadDefault()
	cfg.Feishu.AppID = "cli_status_app"
	cfg.Feishu.AppSecret = "do-not-leak-me-2"
	cfg.Feishu.VerificationToken = "vt"
	cfg.Feishu.EncryptKey = "ek"
	if err := config.Save(cfg, os.Getenv("NIGHTME_CONFIG")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	cfg2, _ := config.LoadDefault()
	if err := printFeishuStatus(&buf, cfg2); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"cli_status_app",
		"verification_token: configured",
		"encrypt_key: configured",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "do-not-leak-me-2") {
		t.Errorf("status leaks app_secret: %s", out)
	}
}

// TestAuthStatus_RunE drives the cobra wiring without going through
// the cobra parser, mirroring how the list command is tested.
func TestAuthStatus_RunE(t *testing.T) {
	_ = withTempConfig(t)

	cmd := newAuthStatusFeishuCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}
}
