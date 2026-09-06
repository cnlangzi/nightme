package main

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/config"
)

// TestEnsureAgentAvailable_AutoPickPrimary verifies that when at
// least one built-in agent is detectable, cfg.Primary is set to
// one of them without prompting. The exact name is intentionally
// not asserted: earlier tests in the suite may have mutated
// Builtins via SetBuiltinCommand, and the test only verifies the
// "no prompt" + "Primary is non-empty" contract.
func TestEnsureAgentAvailable_AutoPickPrimary(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	cfgPath := filepath.Join(tmp, ".nightme", "config.yaml")
	t.Setenv("NIGHTME_CONFIG", cfgPath)

	binDir := t.TempDir()
	bin := filepath.Join(binDir, "fake-agent-for-detect")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := &config.Config{}
	var buf bytes.Buffer
	in := bufio.NewReader(strings.NewReader(""))
	if err := EnsureAgentAvailable(cfg, in, &buf); err != nil {
		t.Fatalf("EnsureAgentAvailable: %v", err)
	}

	if cfg.Primary == "" {
		t.Errorf("Primary empty after EnsureAgentAvailable; expected auto-pick")
	}
	cfg2, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}
	if cfg2.Primary != cfg.Primary {
		t.Errorf("persisted Primary = %q, want %q", cfg2.Primary, cfg.Primary)
	}
	if !strings.Contains(buf.String(), "auto-detected") {
		t.Errorf("expected auto-detected confirmation in output: %s", buf.String())
	}
}

// TestEnsureAgentAvailable_AbortOnNoAgents verifies the non-
// interactive bail-out: with no detectable built-ins and stdin
// closed, EnsureAgentAvailable returns errNoAgentConfigured.
func TestEnsureAgentAvailable_AbortOnNoAgents(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	cfgPath := filepath.Join(tmp, ".nightme", "config.yaml")
	t.Setenv("NIGHTME_CONFIG", cfgPath)
	t.Setenv("NIGHTME_NO_PROMPT", "1")
	// PATH set to an empty dir so no built-ins resolve.
	t.Setenv("PATH", t.TempDir())

	cfg := &config.Config{}
	var buf bytes.Buffer
	in := bufio.NewReader(strings.NewReader(""))
	err := EnsureAgentAvailable(cfg, in, &buf)
	if err == nil {
		t.Fatal("expected error when no agent available, got nil")
	}
	if !strings.Contains(err.Error(), "no AI coding agent") {
		t.Errorf("error %q should mention remediation hint", err.Error())
	}
}

// TestEnsureAgentAvailable_FirstrunPromptSucceeds drives the
// interactive path: pick a built-in, provide a fake binary path,
// verify cfg.Agents + cfg.Primary are written.
func TestEnsureAgentAvailable_FirstrunPromptSucceeds(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	cfgPath := filepath.Join(tmp, ".nightme", "config.yaml")
	t.Setenv("NIGHTME_CONFIG", cfgPath)
	t.Setenv("PATH", t.TempDir())

	bin := filepath.Join(tmp, "my-claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := &config.Config{}
	var buf bytes.Buffer
	in := bufio.NewReader(strings.NewReader("1\n" + bin + "\n"))
	if err := EnsureAgentAvailable(cfg, in, &buf); err != nil {
		t.Fatalf("EnsureAgentAvailable: %v", err)
	}

	if cfg.Primary != "claude" {
		t.Errorf("Primary = %q, want claude", cfg.Primary)
	}

	var got string
	for _, e := range cfg.Agents {
		if e.Name == "claude" {
			got = e.Command
			break
		}
	}
	if got != bin {
		t.Errorf("cfg.Agents[claude].Command = %q, want %q", got, bin)
	}

	cfg2, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}
	if cfg2.Primary != "claude" {
		t.Errorf("persisted Primary = %q, want claude", cfg2.Primary)
	}
}
