package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/config"
)

// TestLogger_RedactsSecrets ensures any attribute whose key
// contains "secret", "token", or "password" is rewritten to
// "***REDACTED***". Case-insensitive.
func TestLogger_RedactsSecrets(t *testing.T) {
	var buf bytes.Buffer
	handler := &redactingHandler{inner: slog.NewJSONHandler(&buf, nil)}
	lg := slog.New(handler)

	lg.Info("starting", "AppSecret", "shhh", "access_token", "tok-1", "PASSWORD", "pw", "Safe", "ok")

	line := buf.String()
	if !strings.Contains(line, "\"AppSecret\":\"***REDACTED***\"") {
		t.Errorf("expected AppSecret to be redacted; got: %s", line)
	}
	if !strings.Contains(line, "\"access_token\":\"***REDACTED***\"") {
		t.Errorf("expected access_token to be redacted; got: %s", line)
	}
	if !strings.Contains(line, "\"PASSWORD\":\"***REDACTED***\"") {
		t.Errorf("expected PASSWORD (uppercase) to be redacted; got: %s", line)
	}
	if !strings.Contains(line, "\"Safe\":\"ok\"") {
		t.Errorf("expected Safe to be preserved; got: %s", line)
	}
}

// TestLogger_WritesToFile checks that New() appends JSON lines to
// the configured log file (or its default fallback) and that the
// JSON is well-formed.
func TestLogger_WritesToFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "sub", "nightme.log")

	cfg := &config.Config{
		Logging: config.LoggingConfig{File: logPath, Level: "info"},
	}
	lg, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lg.Info("hello", "key", "value")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	line := strings.TrimSpace(string(data))
	if line == "" {
		t.Fatal("expected log line, got empty")
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, line)
	}
	if record["msg"] != "hello" {
		t.Errorf("expected msg=hello, got %v", record["msg"])
	}
	if record["key"] != "value" {
		t.Errorf("expected key=value, got %v", record["key"])
	}
}

// TestLogger_FilePermissions verifies the created log file is mode
// 0600 (NFR N-7). Skipped on Windows where chmod semantics differ.
func TestLogger_FilePermissions(t *testing.T) {
	if os.Getenv("GOOS_OVERRIDE_WIN") == "windows" {
		t.Skip("skipping on windows")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "perms.log")

	cfg := &config.Config{Logging: config.LoggingConfig{File: logPath}}
	lg, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = lg

	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("expected 0600, got %o", mode)
	}
}

// TestLogger_DefaultPath checks that an empty cfg.Logging.File
// falls back to ~/.local/share/nightme/nightme.log.
func TestLogger_DefaultPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	lg, err := New(&config.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = lg

	want := filepath.Join(dir, ".local", "share", "nightme", "nightme.log")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected default log at %s: %v", want, err)
	}
}

// TestLogger_LevelParsing pins the level-to-string mapping so
// future expansion doesn't regress a config knob.
func TestLogger_LevelParsing(t *testing.T) {
	cases := map[string]slog.Level{
		"":       slog.LevelInfo,
		"debug":  slog.LevelDebug,
		"info":   slog.LevelInfo,
		"warn":   slog.LevelWarn,
		"error":  slog.LevelError,
		"TRACE":  slog.LevelInfo, // unknown -> info
		" banana ": slog.LevelInfo,
	}
	for in, want := range cases {
		if got := levelFor(in); got != want {
			t.Errorf("levelFor(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestShouldRedact covers the substring matcher exhaustively.
func TestShouldRedact(t *testing.T) {
	cases := map[string]bool{
		"app_secret":      true,
		"AppSecret":       true,
		"access_token":    true,
		"TOKEN":           true,
		"db_password":     true,
		"pass":            false,
		"username":        false,
		"safe_field":      false,
		"":                false,
	}
	for k, want := range cases {
		if got := shouldRedact(k); got != want {
			t.Errorf("shouldRedact(%q) = %v, want %v", k, got, want)
		}
	}
}
