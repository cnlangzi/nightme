package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

func TestLoadExample(t *testing.T) {
	// Repo-relative path — tests run with the package directory as cwd.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	example := filepath.Join(cwd, "..", "..", "configs", "nightme.example.yaml")

	cfg, err := Load(example)
	if err != nil {
		t.Fatalf("Load(example) error: %v", err)
	}

	if cfg.Primary != "claude" {
		t.Errorf("Primary = %q, want claude", cfg.Primary)
	}
	// v1.2: cfg.Agents is a list. Lookup by name.
	var claude, codex, opencode *AgentEntry
	for i := range cfg.Agents {
		switch cfg.Agents[i].Name {
		case "claude":
			claude = &cfg.Agents[i]
		case "codex":
			codex = &cfg.Agents[i]
		case "opencode":
			opencode = &cfg.Agents[i]
		}
	}
	if claude == nil || claude.Command != "claude --dangerously-skip-permissions" {
		t.Errorf("claude entry: %+v, want Command=\"claude --dangerously-skip-permissions\"", claude)
	}
	if codex == nil || codex.Command != "codex" {
		t.Errorf("codex entry: %+v, want Command=\"codex\"", codex)
	}
	if opencode == nil || opencode.Command != "opencode" {
		t.Errorf("opencode entry: %+v, want Command=\"opencode\" (the HTTP bridge appends `serve` itself)", opencode)
	}
	if cfg.Session.DefaultPtyCols != 80 {
		t.Errorf("Session.DefaultPtyCols = %d, want 80", cfg.Session.DefaultPtyCols)
	}
	if cfg.Session.DefaultPtyRows != 24 {
		t.Errorf("Session.DefaultPtyRows = %d, want 24", cfg.Session.DefaultPtyRows)
	}
	if cfg.Session.OutputChunkSize != 4096 {
		t.Errorf("Session.OutputChunkSize = %d, want 4096", cfg.Session.OutputChunkSize)
	}
	if cfg.Session.OutputFlushIntervalMs != 200 {
		t.Errorf("Session.OutputFlushIntervalMs = %d, want 200", cfg.Session.OutputFlushIntervalMs)
	}
	if cfg.Logging.Level != "info" {
		t.Errorf("Logging.Level = %q, want info", cfg.Logging.Level)
	}
	if cfg.Paths.DataDir == "" {
		t.Errorf("Paths.DataDir should not be empty")
	}
}

func TestDefaults(t *testing.T) {
	// Load with an empty path -> defaults only.
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") error: %v", err)
	}

	// Primary has no hardcoded default — applyDefaults leaves it
	// empty and the auto-detect layer (LoadDefault) picks it up
	// from agent.Builtins on a real startup. Documenting the
	// behaviour here so a future reader doesn't "fix" the
	// emptiness back to a hardcoded name.
	if cfg.Primary != "" {
		t.Errorf("default Primary = %q, want \"\" (resolved by LoadDefault auto-detect)", cfg.Primary)
	}
	if cfg.Session.DefaultPtyCols != 80 {
		t.Errorf("default Session.DefaultPtyCols = %d, want 80", cfg.Session.DefaultPtyCols)
	}
	if cfg.Session.DefaultPtyRows != 24 {
		t.Errorf("default Session.DefaultPtyRows = %d, want 24", cfg.Session.DefaultPtyRows)
	}
	if cfg.Session.OutputChunkSize != 4096 {
		t.Errorf("default Session.OutputChunkSize = %d, want 4096", cfg.Session.OutputChunkSize)
	}
	if cfg.Session.OutputFlushIntervalMs != 200 {
		t.Errorf("default Session.OutputFlushIntervalMs = %d, want 200", cfg.Session.OutputFlushIntervalMs)
	}
	// Logging.Level is intentionally left empty when unset so
	// internal/logging.levelFor can apply the WARN default.
	// Loading config must not override that — otherwise the
	// interactive REPL / tray would leak INFO chatter despite
	// the logger's own default. Tests for the WARN default
	// live in internal/logging.
	if cfg.Logging.Level != "" {
		t.Errorf("default Logging.Level = %q, want \"\" (resolved by logging package)", cfg.Logging.Level)
	}

	// DataDir should have ~ expanded to the real home directory.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".nightme")
	if cfg.Paths.DataDir != want {
		t.Errorf("default Paths.DataDir = %q, want %q", cfg.Paths.DataDir, want)
	}
}

func TestEnvOverride(t *testing.T) {
	// Save and restore the environment around this test.
	t.Setenv("NIGHTME_PRIMARY", "codex")
	t.Setenv("NIGHTME_FEISHU_APP_ID", "cli_override")
	t.Setenv("NIGHTME_LOGGING_LEVEL", "debug")
	t.Setenv("NIGHTME_SESSION_DEFAULT_PTY_COLS", "120")
	t.Setenv("NIGHTME_SESSION_OUTPUT_FLUSH_INTERVAL_MS", "500")
	t.Setenv("NIGHTME_PATHS_DATA_DIR", "/tmp/nightme-data")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") error: %v", err)
	}

	if cfg.Primary != "codex" {
		t.Errorf("Primary = %q, want codex", cfg.Primary)
	}
	if cfg.Feishu.AppID != "cli_override" {
		t.Errorf("Feishu.AppID = %q, want cli_override", cfg.Feishu.AppID)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("Logging.Level = %q, want debug", cfg.Logging.Level)
	}
	if cfg.Session.DefaultPtyCols != 120 {
		t.Errorf("Session.DefaultPtyCols = %d, want 120", cfg.Session.DefaultPtyCols)
	}
	if cfg.Session.OutputFlushIntervalMs != 500 {
		t.Errorf("Session.OutputFlushIntervalMs = %d, want 500", cfg.Session.OutputFlushIntervalMs)
	}
	if cfg.Paths.DataDir != "/tmp/nightme-data" {
		t.Errorf("Paths.DataDir = %q, want /tmp/nightme-data", cfg.Paths.DataDir)
	}

	// Verify defaults are still applied for fields not overridden.
	if cfg.Session.DefaultPtyRows != 24 {
		t.Errorf("Session.DefaultPtyRows = %d, want 24 (default)", cfg.Session.DefaultPtyRows)
	}
}

func TestMissingFile(t *testing.T) {
	cfg, err := Load("/nonexistent/path/to/config.yaml")
	if err != nil {
		t.Fatalf("Load(missing) returned error: %v (want nil — defaults only)", err)
	}
	// No hardcoded Primary default — see TestDefaults comment.
	if cfg.Primary != "" {
		t.Errorf("Primary = %q, want \"\" (no hardcoded default; auto-detect is LoadDefault's job)", cfg.Primary)
	}
}

func TestMalformedFile(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("feishu: [unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil {
		t.Fatal("Load(malformed) expected error, got nil")
	}
}

func TestEnvOverrideInvalidNumber(t *testing.T) {
	// Set a non-numeric value for an int override — the loader should
	// silently fall back to the default rather than crash.
	t.Setenv("NIGHTME_SESSION_DEFAULT_PTY_COLS", "not-a-number")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") error: %v", err)
	}
	if cfg.Session.DefaultPtyCols != 80 {
		t.Errorf("Session.DefaultPtyCols = %d, want 80 (default after invalid override)", cfg.Session.DefaultPtyCols)
	}
}

// TestLoadDefault_AutoDetectsPersistsAndIsIdempotent verifies the
// full LoadDefault auto-detect path: a fresh temp config file with
// no `primary:` line should be filled in by probing agent.Builtins
// and the choice should be persisted to disk so a second LoadDefault
// is a no-op.
//
// IMPORTANT: agent.Builtins is empty in this test binary because
// the only place that calls Builtins.Register is cmd/nightme/agents.go
// (package main), which is not transitively imported here. So the
// test must register its own starter — otherwise detection always
// returns "" and the persist branch is never exercised. Mirror the
// pattern from internal/command/gtw/hooks_test.go:512-521.
func TestLoadDefault_AutoDetectsPersistsAndIsIdempotent(t *testing.T) {
	// Seed a uniquely-named starter. Detect() succeeds unconditionally
	// so this starter is guaranteed to win the Builtins.List() probe.
	const testName = "detect-persist-test"
	prev, _ := agent.Builtins.Get(testName)
	t.Cleanup(func() {
		if prev != nil {
			// Re-register any previously-registered entry under the
			// same name to keep order stable across runs of this test
			// within the same binary. Builtins has no Unregister; if
			// nothing was previously registered, the entry stays —
			// that's fine, the name is unique and never collides.
			_ = agent.Builtins.Register(prev)
		}
	})
	_ = agent.Builtins.Register(&detectPersistTestStarter{name: testName})

	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("NIGHTME_CONFIG", cfgFile)
	t.Setenv("NIGHTME_PRIMARY", "")

	// First call: triggers detect + persist.
	first, err := LoadDefault()
	if err != nil {
		t.Fatalf("first LoadDefault: %v", err)
	}
	if first.Primary != testName {
		t.Fatalf("first LoadDefault Primary = %q, want %q — detect branch did not fire", first.Primary, testName)
	}

	// Verify the choice landed on disk.
	if _, err := os.Stat(cfgFile); err != nil {
		t.Fatalf("config file %s was not written after auto-detect: %v", cfgFile, err)
	}
	onDisk, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("re-read config after auto-detect: %v", err)
	}
	if onDisk.Primary != testName {
		t.Errorf("persisted Primary = %q, in-memory = %q — SaveDefault did not run",
			onDisk.Primary, testName)
	}

	// Second call: Primary already set, must be a no-op (still
	// the same value, file not rewritten — we can't easily detect
	// "file not rewritten" without fs stat timestamps, so we just
	// re-check the value).
	second, err := LoadDefault()
	if err != nil {
		t.Fatalf("second LoadDefault: %v", err)
	}
	if second.Primary != testName {
		t.Errorf("second LoadDefault Primary = %q, want %q — non-idempotent",
			second.Primary, testName)
	}
}

// detectPersistTestStarter is a minimal agent.Starter that always
// reports its binary as "available" via Detect(). The other Starter
// methods are stubbed because LoadDefault's auto-detect path only
// calls Detect(); Start/RunOnce/Review never run from this test.
//
// ModePTY is irrelevant to this test — detection doesn't inspect
// the mode — but matches the convention in internal/command/gtw/hooks_test.go
// (testStarter) so future readers see a familiar shape.
type detectPersistTestStarter struct{ name string }

func (s *detectPersistTestStarter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModePTY, "", nil, nil)
}
func (s *detectPersistTestStarter) Detect() error { return nil }
func (s *detectPersistTestStarter) Start(context.Context, agent.StartConfig) (*agent.Agent, error) {
	return nil, errors.New("detectPersistTestStarter: Start not implemented")
}
func (s *detectPersistTestStarter) RunOnce(context.Context, agent.StartConfig, []agent.ContentBlock) (agent.RunResult, error) {
	return agent.RunResult{}, errors.New("detectPersistTestStarter: RunOnce not implemented")
}
func (s *detectPersistTestStarter) Review(context.Context, agent.StartConfig) (agent.RunResult, error) {
	return agent.RunResult{}, errors.New("detectPersistTestStarter: Review not implemented")
}
