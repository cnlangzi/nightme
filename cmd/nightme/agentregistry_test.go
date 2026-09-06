package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentregistry"
	"github.com/cnlangzi/nightme/internal/config"
)

// commandHolder is the subset of the bridge *Starter API tests
// use to snapshot and restore Builtins state. Each bridge's
// *Starter implements Command / Init; test fakes don't
// (they never participate in cfg.Agents overrides).
type commandHolder interface {
	Command() string
	Init(string)
}

// snapshotBuiltinCommands saves the current command field on
// every registered built-in and registers a t.Cleanup that
// restores them. Use it at the top of any test that exercises
// cfg.Agents → agentregistry.Build → Init, since Build
// mutates the Builtins singletons in place.
//
// Skips starters that don't implement Command/Init
// (test fakes and any non-bridge starters); for the seven
// built-ins this is always a no-op skip.
func snapshotBuiltinCommands(t *testing.T) {
	t.Helper()
	saved := map[string]string{}
	for _, s := range agent.Builtins.List() {
		name := s.Info().Name
		if c, ok := s.(commandHolder); ok {
			saved[name] = c.Command()
		}
	}
	t.Cleanup(func() {
		for _, s := range agent.Builtins.List() {
			c, ok := s.(commandHolder)
			if !ok {
				continue
			}
			if orig, present := saved[s.Info().Name]; present {
				c.Init(orig)
			}
		}
	})
}

// fakeBinary writes a minimal shell script and returns its
// absolute path.
func fakeBinary(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// TestBuild_BuiltinDrivenIgnoresUnknownNames is the core
// "names outside the whitelist are silently discarded"
// invariant. The loop is built-in driven: we iterate over the
// registered starters and consult cfg.Agents as a side lookup,
// so an entry whose name is not a built-in is never even
// inspected — no warn, no PTY fallback, no alias.
//
// Lives in cmd/nightme because that's where the seven built-ins
// register themselves via init() — agentregistry tests cannot
// run standalone and still see Builtins populated.
func TestBuild_BuiltinDrivenIgnoresUnknownNames(t *testing.T) {
	snapshotBuiltinCommands(t)
	tmp := t.TempDir()
	bin := fakeBinary(t, tmp, "claude-override")
	cfg := &config.Config{
		Agents: []config.AgentEntry{
			{Name: "claude", Command: bin},
			{Name: "not-a-builtin", Command: "/some/path"},
			{Name: "another-unknown", Command: "/another/path"},
		},
	}
	reg := agentregistry.Build(cfg, "")

	// Good entry: registered with override.
	s, err := reg.Get("claude")
	if err != nil {
		t.Fatalf("Get(claude): %v", err)
	}
	if got := s.Info().Command; got != bin {
		t.Errorf("Info.Command = %q, want %q", got, bin)
	}

	// Unknown entries: never read, never registered.
	for _, name := range []string{"not-a-builtin", "another-unknown"} {
		if _, err := reg.Get(name); err == nil {
			t.Errorf("%q leaked into registry", name)
		}
	}

	// Every other built-in is still there with its default
	// (un-overridden) command.
	seen := map[string]bool{}
	for _, s := range reg.List() {
		seen[s.Info().Name] = true
	}
	for _, want := range []string{"codex", "dsh", "opencode", "cursor", "pi", "copilot"} {
		if !seen[want] {
			t.Errorf("built-in %q missing from registry", want)
		}
	}
}

// TestBuild_CfgOverrideBuiltinPath overrides a built-in starter's
// command path and verifies Detect resolves the new path.
func TestBuild_CfgOverrideBuiltinPath(t *testing.T) {
	snapshotBuiltinCommands(t)
	tmp := t.TempDir()
	bin := fakeBinary(t, tmp, "claude-override")
	cfg := &config.Config{
		Agents: []config.AgentEntry{
			{Name: "claude", Command: bin},
		},
	}
	reg := agentregistry.Build(cfg, "")
	s, err := reg.Get("claude")
	if err != nil {
		t.Fatalf("Get(claude): %v", err)
	}
	if got := s.Info().Command; got != bin {
		t.Errorf("Info.Command = %q, want %q", got, bin)
	}
}

// TestBuild_CfgPreservesSpacesInPath locks the Windows-path-with-
// spaces invariant: cfg.Agents.Command is taken verbatim (after
// trim), not whitespace-split. Splitting would turn
// `C:\Program Files\claude\claude.exe` into `C:\Program` and
// silently fail to Detect.
func TestBuild_CfgPreservesSpacesInPath(t *testing.T) {
	snapshotBuiltinCommands(t)
	tmp := t.TempDir()
	parent := tmp + "/Program Files"
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	bin := parent + "/claude-fake"
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := &config.Config{
		Agents: []config.AgentEntry{
			{Name: "claude", Command: bin},
		},
	}
	reg := agentregistry.Build(cfg, "")
	s, err := reg.Get("claude")
	if err != nil {
		t.Fatalf("Get(claude): %v", err)
	}
	if got := s.Info().Command; got != bin {
		t.Errorf("Info.Command = %q, want %q (path with space was mangled)", got, bin)
	}
}

// TestBuild_BarePathAutoRegister verifies that --agent /path/to/bin
// still works: a non-builtin name that exists on disk auto-
// registers as a PTY starter.
func TestBuild_BarePathAutoRegister(t *testing.T) {
	tmp := t.TempDir()
	bin := fakeBinary(t, tmp, "echoish")
	reg := agentregistry.Build(&config.Config{}, bin)
	s, err := reg.Get(bin)
	if err != nil {
		t.Fatalf("Get(%s): %v", bin, err)
	}
	if got := s.Info().Command; !strings.Contains(got, filepath.Base(bin)) {
		t.Errorf("Info.Command = %q, want contains %q", got, filepath.Base(bin))
	}
}

// TestBuild_BarePathTypoNotRegistered verifies that an unknown
// name without an on-disk counterpart is NOT registered, so
// `nightme test --agent /typo` surfaces as "agent not found".
func TestBuild_BarePathTypoNotRegistered(t *testing.T) {
	reg := agentregistry.Build(&config.Config{}, "/nonexistent/typo")
	if _, err := reg.Get("/nonexistent/typo"); err == nil {
		t.Errorf("typo without on-disk counterpart was registered")
	}
}

// silence unused-import warning when this file is built without
// any test referencing agent directly.
var _ = agent.New
