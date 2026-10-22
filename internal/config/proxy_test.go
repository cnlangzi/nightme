// proxy_test.go — the `proxy:` / NIGHTME_PROXY setting.
//
// The rule is the same one every other nightme setting follows:
// configured means honoured, absent means the setting does not exist
// and outbound behaviour is exactly what it was before.

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProxy_UnsetIsEmpty(t *testing.T) {
	t.Setenv("NIGHTME_PROXY", "")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Proxy != "" {
		t.Errorf("Proxy = %q, want empty when nothing configured it", cfg.Proxy)
	}
}

func TestProxy_FromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("proxy: http://127.0.0.1:7890\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NIGHTME_PROXY", "")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Proxy != "http://127.0.0.1:7890" {
		t.Errorf("Proxy = %q, want http://127.0.0.1:7890", cfg.Proxy)
	}
}

func TestProxy_EnvBeatsYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("proxy: http://from-file:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NIGHTME_PROXY", "socks5://from-env:2")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Proxy != "socks5://from-env:2" {
		t.Errorf("Proxy = %q, want the environment to win over the file", cfg.Proxy)
	}
}

// TestProxy_OmittedFromSavedConfig: proxy is opt-in, so a config the
// user never touched must not grow a `proxy:` line every time nightme
// rewrites it.
func TestProxy_OmittedFromSavedConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := Save(&Config{Primary: "claude"}, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); strings.Contains(got, "proxy:") {
		t.Errorf("saved config grew a proxy line:\n%s", got)
	}
}
