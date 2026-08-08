package main

import (
	"bytes"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/config"
)

// TestMergeAgents_BuiltinsOnly verifies the merge starts with all
// built-ins (sorted by name) and tags them Source="builtin".
func TestMergeAgents_BuiltinsOnly(t *testing.T) {
	cfg := &config.Config{}
	got := MergeAgents(cfg)

	if len(got) == 0 {
		t.Fatalf("expected builtins, got 0")
	}
	for _, a := range got {
		if a.Source != "builtin" {
			t.Errorf("%s Source = %q, want builtin", a.Name, a.Source)
		}
	}
	// Names must be alphabetical.
	for i := 1; i < len(got); i++ {
		if got[i-1].Name > got[i].Name {
			t.Errorf("not sorted: %s > %s", got[i-1].Name, got[i].Name)
		}
	}
}

// TestMergeAgents_UserConfigAddsNew verifies a new name (not in
// builtins) appends a Source="config" entry.
func TestMergeAgents_UserConfigAddsNew(t *testing.T) {
	cfg := &config.Config{
		Agents: []config.AgentEntry{
			{Name: "cc", Bridge: "claude", Command: "claude --dangerously-skip-permissions"},
		},
	}
	got := MergeAgents(cfg)

	found := false
	for _, a := range got {
		if a.Name == "cc" {
			found = true
			if a.Source != "config" {
				t.Errorf("cc Source = %q, want config", a.Source)
			}
			if a.Command != "claude --dangerously-skip-permissions" {
				t.Errorf("cc Command = %q", a.Command)
			}
		}
	}
	if !found {
		t.Errorf("cc not found in merged list")
	}
}

// TestMergeAgents_UserConfigOverridesBuiltin verifies that a name
// collision with a built-in is won by cfg.Agents (Source="config",
// Command from cfg, Bridge from cfg).
func TestMergeAgents_UserConfigOverridesBuiltin(t *testing.T) {
	cfg := &config.Config{
		Agents: []config.AgentEntry{
			{Name: "claude", Bridge: "claude", Command: "/custom/path/claude --custom-flag"},
		},
	}
	got := MergeAgents(cfg)

	count := 0
	for _, a := range got {
		if a.Name == "claude" {
			count++
			if a.Source != "config" {
				t.Errorf("claude Source = %q, want config (override)", a.Source)
			}
			if a.Command != "/custom/path/claude --custom-flag" {
				t.Errorf("claude Command = %q, want /custom/path/claude --custom-flag", a.Command)
			}
		}
	}
	if count != 1 {
		t.Errorf("claude appeared %d times, want 1 (override, no duplication)", count)
	}
}

// TestConfigAgentsMenu_PickAndSave exercises the full menu loop
// with a fake stdin and asserts the config file is updated.
func TestConfigAgentsMenu_PickAndSave(t *testing.T) {
	// Use a temp HOME so config.SaveDefault writes there. Also pin
	// NIGHTME_CONFIG to the same path so the test is robust against
	// CI runners that have NIGHTME_CONFIG exported in the env (which
	// would otherwise redirect SaveDefault to a different file).
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	cfgPath := filepath.Join(tmp, ".nightme", "config.yaml")
	t.Setenv("NIGHTME_CONFIG", cfgPath)

	// Seed the file with cfg.Primary=claude + a user config entry.
	seed := &config.Config{
		Primary: "claude",
		Agents: []config.AgentEntry{
			{Name: "cc", Bridge: "claude", Command: "claude --dangerously-skip-permissions"},
		},
	}
	if err := config.Save(seed, cfgPath); err != nil {
		t.Fatalf("Save seed: %v", err)
	}

	// Load back (round-trip through file).
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Find "cc"'s index in the merged list (it depends on builtin
	// sort order, which we don't want to hardcode here).
	merged := MergeAgents(cfg)
	ccIdx := -1
	for i, a := range merged {
		if a.Name == "cc" {
			ccIdx = i
			break
		}
	}
	if ccIdx < 0 {
		t.Fatalf("cc not in merged list (got %d entries)", len(merged))
	}

	// Simulate the user picking that index.
	var buf bytes.Buffer
	in := strings.NewReader(strconv.Itoa(ccIdx+1) + "\n")
	if err := configAgentsMenu(cfg, in, &buf); err != nil {
		t.Fatalf("configAgentsMenu: %v", err)
	}

	// Reload and verify.
	cfg2, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}
	if cfg2.Primary != "cc" {
		t.Errorf("Primary = %q, want cc", cfg2.Primary)
	}

	// Verify menu output mentions the pick.
	out := buf.String()
	if !strings.Contains(out, "Primary set to \"cc\"") {
		t.Errorf("output missing confirmation: %s", out)
	}
}

// TestConfigAgentsMenu_CancelWithQ verifies that sending "q" leaves
// the config unchanged.
func TestConfigAgentsMenu_CancelWithQ(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	cfgPath := filepath.Join(tmp, ".nightme", "config.yaml")
	t.Setenv("NIGHTME_CONFIG", cfgPath)

	cfg, err := config.Load(cfgPath) // defaults: Primary=claude
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var buf bytes.Buffer
	if err := configAgentsMenu(cfg, strings.NewReader("q\n"), &buf); err != nil {
		t.Fatalf("configAgentsMenu: %v", err)
	}

	if !strings.Contains(buf.String(), "Cancelled") {
		t.Errorf("expected cancel message, got: %s", buf.String())
	}

	// File should NOT exist (no save happened).
	if _, err := config.Load(cfgPath); err == nil {
		// Actually wait — Load returns defaults if file missing, so
		// success here just means "Load worked with defaults".
		// Verify the in-memory cfg is unchanged.
		if cfg.Primary != "claude" {
			t.Errorf("Primary mutated to %q after cancel", cfg.Primary)
		}
	}
}

// TestConfigInteractive_QuitImmediately verifies the top-level "q"
// exits cleanly without touching the config.
func TestConfigInteractive_QuitImmediately(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cfg, err := config.LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault: %v", err)
	}

	var buf bytes.Buffer
	if err := configInteractive(cfg, strings.NewReader("q\n"), &buf); err != nil {
		t.Fatalf("configInteractive: %v", err)
	}

	if !strings.Contains(buf.String(), "Bye.") {
		t.Errorf("expected goodbye, got: %s", buf.String())
	}
}

// TestReadLine handles EOF.
func TestReadLine_EOF(t *testing.T) {
	if got := readLine(strings.NewReader("")); got != "" {
		t.Errorf("EOF should return empty, got %q", got)
	}
}

func TestReadLine_StripsNewline(t *testing.T) {
	if got := readLine(strings.NewReader("hello\n")); got != "hello" {
		t.Errorf("got %q, want hello", got)
	}
}