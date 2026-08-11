package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEffectiveName_ConfiguredReturnsConfig: when c.Name is set, the
// helper returns it verbatim — no hostname lookup, no fallback.
func TestEffectiveName_ConfiguredReturnsConfig(t *testing.T) {
	cfg := &Config{Name: "macbook-pro"}
	if got := EffectiveName(cfg); got != "macbook-pro" {
		t.Errorf("EffectiveName = %q, want macbook-pro", got)
	}
}

// TestEffectiveName_EmptyFallsBackToHostname: empty c.Name must fall
// through to os.Hostname(), and the result must be non-empty on any
// real machine (the kernel returns something for every supported
// platform).
func TestEffectiveName_EmptyFallsBackToHostname(t *testing.T) {
	cfg := &Config{Name: ""}
	got := EffectiveName(cfg)
	if got == "" {
		t.Fatal("EffectiveName returned empty string (hostname lookup failed silently?)")
	}

	// Sanity: the returned value matches os.Hostname() when env is
	// clean. We don't pin the exact hostname (it's CI-dependent) —
	// just verify the helper isn't fabricating a different string.
	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("os.Hostname: %v", err)
	}
	if got != host {
		t.Errorf("EffectiveName = %q, want hostname %q", got, host)
	}
}

// TestEffectiveName_NilSafe: passing nil must not panic. The
// fallback path runs and returns hostname (or "unknown" if hostname
// also fails).
func TestEffectiveName_NilSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("EffectiveName(nil) panicked: %v", r)
		}
	}()
	if got := EffectiveName(nil); got == "" {
		t.Error("EffectiveName(nil) returned empty string — the helper must always produce a non-empty value")
	}
}

// TestEnvOverride_Name: NIGHTME_NAME populates cfg.Name just like
// the other top-level env overrides.
func TestEnvOverride_Name(t *testing.T) {
	t.Setenv("NIGHTME_NAME", "env-instance")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Name != "env-instance" {
		t.Errorf("Name = %q, want env-instance", cfg.Name)
	}
}

// TestRoundTrip_Name_OmitEmpty guards a future-proofing guarantee:
// any future code path that does Load + Save (with no name change
// in between, e.g. when another section like Feishu gets updated)
// must not re-introduce an explicit `name: ""` line for a user who
// previously removed it. The `omitempty` yaml tag is what enforces
// this; this test pins the behavior so a refactor can't silently
// regress it.
func TestRoundTrip_Name_OmitEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	seed := &Config{Primary: "claude", Name: "old"}
	if err := Save(seed, path); err != nil {
		t.Fatalf("Save seed: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	loaded.Name = "" // simulate a user deleting the `name:` line
	if err := Save(loaded, path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "name:") {
		t.Errorf("config should not contain `name:` key when Name is empty, got:\n%s", string(data))
	}
}

// TestRoundTrip_Name_SetAndKeep: setting a name persists it AND
// re-loading yields the same value. Sanity check that the
// `omitempty` tag didn't accidentally drop non-empty values.
func TestRoundTrip_Name_SetAndKeep(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	cfg := &Config{Primary: "claude", Name: "office-desktop"}
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Name != "office-desktop" {
		t.Errorf("Name round-trip = %q, want office-desktop", loaded.Name)
	}
}