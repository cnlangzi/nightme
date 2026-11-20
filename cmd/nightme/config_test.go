package main

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/config"
)

// writeFakeBinary drops a tiny shell script in dir and returns its
// absolute path. The script is enough for ResolveCommand (which
// only stat's) to succeed; tests don't actually invoke it.
func writeFakeBinary(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", path, err)
	}
	return path
}

// claudeIndex returns the index of the claude built-in in
// Builtins.List(). Tests use it to drive the menu without
// hardcoding registration order.
func claudeIndex(t *testing.T) int {
	t.Helper()
	for i, s := range agent.Builtins.List() {
		if s.Info().Name == "claude" {
			return i
		}
	}
	t.Fatal("claude not in built-in registry")
	return -1
}

// TestConfigAgentsMenu_CancelWithQ verifies that sending "q" at
// the top-level list leaves the config unchanged.
func TestConfigAgentsMenu_CancelWithQ(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	cfgPath := filepath.Join(tmp, ".nightme", "config.yaml")
	t.Setenv("NIGHTME_CONFIG", cfgPath)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var buf bytes.Buffer
	in := bufio.NewReader(strings.NewReader("q\n"))
	if err := configAgentsMenu(cfg, cfgPath, in, &buf); err != nil {
		t.Fatalf("configAgentsMenu: %v", err)
	}

	if cfg.Primary != "" {
		t.Errorf("Primary mutated to %q after cancel", cfg.Primary)
	}
	if _, err := os.Stat(cfgPath); err == nil {
		t.Errorf("config file was created despite cancel: %s", cfgPath)
	}
}

// TestConfigAgentsMenu_ConfiguresPathAndSetsPrimary drives the
// menu twice: first pick claude + "2" (configure path) + path;
// second pick claude + "1" (set as primary). Verifies the
// final cfg.Agents + cfg.Primary are persisted.
func TestConfigAgentsMenu_ConfiguresPathAndSetsPrimary(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	cfgPath := filepath.Join(tmp, ".nightme", "config.yaml")
	t.Setenv("NIGHTME_CONFIG", cfgPath)

	fake := writeFakeBinary(t, tmp, "claude-fake")
	idx := claudeIndex(t)

	// outer-loop pick → sub-menu option → input.
	pick := strconv.Itoa(idx + 1)
	in := bufio.NewReader(strings.NewReader(
		pick + "\n" + // iter 1: pick claude
			"2\n" + // configure path
			fake + "\n" + // path
			pick + "\n" + // iter 2: pick claude again
			"1\n", // set as primary
	))
	var buf bytes.Buffer
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := configAgentsMenu(cfg, cfgPath, in, &buf); err != nil {
		t.Fatalf("configAgentsMenu: %v", err)
	}

	cfg2, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}

	var got string
	for _, e := range cfg2.Agents {
		if e.Name == "claude" {
			got = e.Command
			break
		}
	}
	if got != fake {
		t.Errorf("cfg.Agents[claude].Command = %q, want %q", got, fake)
	}
	if cfg2.Primary != "claude" {
		t.Errorf("Primary = %q, want claude", cfg2.Primary)
	}
}

// TestConfigAgentsMenu_RemovesPathOverride verifies that "3"
// deletes the cfg.Agents entry for the picked built-in.
func TestConfigAgentsMenu_RemovesPathOverride(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	cfgPath := filepath.Join(tmp, ".nightme", "config.yaml")
	t.Setenv("NIGHTME_CONFIG", cfgPath)

	fake := writeFakeBinary(t, tmp, "fake")
	seed := &config.Config{
		Agents: []config.AgentEntry{
			{Name: "claude", Command: fake},
		},
	}
	if err := config.Save(seed, cfgPath); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	idx := claudeIndex(t)
	in := bufio.NewReader(strings.NewReader(
		strconv.Itoa(idx+1) + "\n3\n",
	))
	var buf bytes.Buffer
	if err := configAgentsMenu(cfg, cfgPath, in, &buf); err != nil {
		t.Fatalf("configAgentsMenu: %v", err)
	}

	cfg2, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}
	for _, e := range cfg2.Agents {
		if e.Name == "claude" {
			t.Errorf("cfg.Agents[claude] still present after remove: %+v", e)
		}
	}
}

// TestConfigInteractive_QuitImmediately verifies the top-level "q"
// exits cleanly without touching the config.
func TestConfigInteractive_QuitImmediately(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	cfgPath := filepath.Join(tmp, ".nightme", "config.yaml")

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var buf bytes.Buffer
	in := bufio.NewReader(strings.NewReader("q\n"))
	if err := configInteractive(cfg, cfgPath, in, &buf); err != nil {
		t.Fatalf("configInteractive: %v", err)
	}

	if !strings.Contains(buf.String(), "Bye.") {
		t.Errorf("expected goodbye, got: %s", buf.String())
	}
}

// TestReadLine handles EOF.
func TestReadLine_EOF(t *testing.T) {
	if got := readLine(bufio.NewReader(strings.NewReader(""))); got != "" {
		t.Errorf("EOF should return empty, got %q", got)
	}
}

func TestReadLine_StripsNewline(t *testing.T) {
	if got := readLine(bufio.NewReader(strings.NewReader("hello\n"))); got != "hello" {
		t.Errorf("got %q, want hello", got)
	}
}

func TestConfigNameMenu_Set(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	cfgPath := filepath.Join(tmp, ".nightme", "config.yaml")
	t.Setenv("NIGHTME_CONFIG", cfgPath)

	seed := &config.Config{Primary: "claude"}
	if err := config.Save(seed, cfgPath); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg, _ := config.Load(cfgPath)

	var buf bytes.Buffer
	in := bufio.NewReader(strings.NewReader("my-laptop\n"))
	if err := configNameMenu(cfg, cfgPath, in, &buf); err != nil {
		t.Fatalf("configNameMenu: %v", err)
	}

	cfg2, _ := config.Load(cfgPath)
	if cfg2.Name != "my-laptop" {
		t.Errorf("Name = %q, want my-laptop", cfg2.Name)
	}
	if !strings.Contains(buf.String(), "Name set to") {
		t.Errorf("output missing confirmation: %s", buf.String())
	}
}

func TestConfigNameMenu_EmptyKeepsCurrent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	cfgPath := filepath.Join(tmp, ".nightme", "config.yaml")
	t.Setenv("NIGHTME_CONFIG", cfgPath)

	seed := &config.Config{Primary: "claude", Name: "existing"}
	if err := config.Save(seed, cfgPath); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg, _ := config.Load(cfgPath)

	var buf bytes.Buffer
	in := bufio.NewReader(strings.NewReader("\n"))
	if err := configNameMenu(cfg, cfgPath, in, &buf); err != nil {
		t.Fatalf("empty: %v", err)
	}

	cfg2, _ := config.Load(cfgPath)
	if cfg2.Name != "existing" {
		t.Errorf("Name = %q after empty input, want existing", cfg2.Name)
	}
	if !strings.Contains(buf.String(), "No changes") {
		t.Errorf("output missing no-change message: %s", buf.String())
	}
}

func TestConfigNameMenu_RequiresExistingConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	cfgPath := filepath.Join(tmp, ".nightme", "config.yaml")
	t.Setenv("NIGHTME_CONFIG", cfgPath)

	cfg := &config.Config{Primary: "claude"}

	var buf bytes.Buffer
	in := bufio.NewReader(strings.NewReader("new-name\n"))
	err := configNameMenu(cfg, cfgPath, in, &buf)
	if err == nil {
		t.Fatal("expected error when config file missing, got nil")
	}
	if !strings.Contains(err.Error(), "no config file") {
		t.Errorf("error %q should mention missing config file", err.Error())
	}
}

// TestConfigInteractive_KeepsTheMeBlockChangedOnDisk covers the
// read-modify-write: a me.llm value that changed on disk while the
// menu was open survives the menu's save.
func TestConfigInteractive_KeepsTheMeBlockChangedOnDisk(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	cfgPath := filepath.Join(tmp, ".nightme", "config.yaml")
	t.Setenv("NIGHTME_CONFIG", cfgPath)

	if err := config.Save(&config.Config{Name: "laptop"}, cfgPath); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// The me.llm block changes on disk while the menu is open.
	if err := config.SetMeLLM(cfgPath, config.MeLLMValues{
		Protocol: "chat_completions",
		Model:    "gpt-4.1-mini",
	}); err != nil {
		t.Fatalf("SetMeLLM: %v", err)
	}

	var buf bytes.Buffer
	in := bufio.NewReader(strings.NewReader("1\nmy-laptop\nq\n"))
	if err := configInteractive(cfg, cfgPath, in, &buf); err != nil {
		t.Fatalf("configInteractive: %v", err)
	}

	after, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}
	if after.Name != "my-laptop" {
		t.Errorf("Name = %q, want my-laptop", after.Name)
	}
	if after.Me.LLM.Model != "gpt-4.1-mini" {
		t.Errorf("Me.LLM.Model = %q, want gpt-4.1-mini (the on-disk change survives)", after.Me.LLM.Model)
	}
}

// TestConfigCmd_InsideTheREPL covers the menu inside the REPL: it
// reads its answers from the stream the REPL shares with it, and the
// REPL keeps working afterwards.
func TestConfigCmd_InsideTheREPL(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	cfgPath := filepath.Join(tmp, ".nightme", "config.yaml")
	t.Setenv("NIGHTME_CONFIG", cfgPath)

	if err := config.Save(&config.Config{Primary: "claude"}, cfgPath); err != nil {
		t.Fatalf("seed: %v", err)
	}

	root, reg := newRootCmd()
	var buf bytes.Buffer
	captureREPLIO(root, &buf)

	in := strings.NewReader(strings.Join([]string{
		"config",
		"1",         // the Name menu
		"my-laptop", // the new name
		"q",         // back to the main menu
		"version",
		"exit",
	}, "\n") + "\n")
	if err := runREPLWith(root, reg, nil, in, &buf); err != nil {
		t.Fatalf("runREPLWith: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Name set to") {
		t.Errorf("the menu did not run inside the REPL:\n%s", out)
	}
	// The banner carries a version line of its own, so the command
	// ran only if the line appears twice.
	if got := strings.Count(out, "nightme version"); got != 2 {
		t.Errorf("the line after the menu was not executed (version line x%d):\n%s", got, out)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Name != "my-laptop" {
		t.Errorf("Name = %q, want my-laptop", cfg.Name)
	}
}
