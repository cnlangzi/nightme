// Package agentregistry — build an agent.Registry from a
// config.Config.
//
// This package exists as a neutral home for the Builtins +
// bare-path-auto-register logic that both the CLI's `nightme
// test` subcommand and the long-running daemon
// (internal/runtime) need. It cannot live in internal/agent
// (the package imports bridge/pty, but bridge/pty already
// imports internal/agent — cycle) and it cannot live in
// internal/runtime (cmd/nightme would have to import runtime,
// but runtime is the daemon — moving it across the CLI/daemon
// boundary is wrong).
//
// Selection rules:
//
//  1. For each registered built-in starter (claudecode / codex /
//     dsh / opencode / cursor / pi / copilot), look up
//     cfg.Agents[name]. If present, apply the override to the
//     singleton (Init) before Register. Names outside the
//     whitelist are never read — they have no matching
//     built-in to apply to, so the merge is implicitly
//     restricted to the seven shipped bridges.
//
//  2. If `requested` is non-empty AND not already in the
//     registry, auto-register a bare-path agent when the file
//     exists — so a one-shot `nightme test --agent /some/bin`
//     still works without polluting the production daemon's
//     registry with user-defined agents.
package agentregistry

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/pty"
	"github.com/cnlangzi/nightme/internal/config"
)

// Build returns a Registry populated with the built-in starters
// (cfg.Agents path overrides applied inline before Register)
// and an optional bare-path agent named by `requested`. Pass
// requested="" to skip the auto-register step (the long-running
// daemon's default — `cfg.Primary` selects from the registered
// set rather than auto-registering a bare path).
//
// Single-pass loop: for each built-in, resolve the cfg.Agents
// override, apply Init to the singleton, then Register.
// The mutation hits agent.Builtins in place — production calls
// Build once at startup so the in-place mutation is fine;
// tests that exercise cfg.Agents overrides should snapshot
// and restore via each Starter's Command() / Init.
func Build(cfg *config.Config, requested string) *agent.Registry {
	reg := agent.New()
	for _, a := range agent.Builtins.List() {
		// Always reset to the starter's baked-in default first so
		// a previous cfg.Agents entry that has since been removed
		// does not leave stale state on the singleton.
		a.Init("")
		if cfg != nil {
			if path := cfgAgentPath(cfg.Agents, a.Info().Name); path != "" {
				a.Init(path)
			}
		}
		reg.Register(a)
	}

	if _, err := reg.Get(requested); err != nil {
		if requested != "" {
			if _, statErr := os.Stat(requested); statErr == nil {
				reg.Register(pty.NewStarter(requested, filepath.Base(requested), nil, nil, 0, 0))
			}
		}
	}
	return reg
}

// cfgAgentPath returns the override path for `name` in entries,
// or "" if no override is set. The Command string is taken
// verbatim after trim — the schema is "absolute path to one
// binary", not "command line" (Windows paths with spaces
// (`C:\Program Files\claude\claude.exe`) would be truncated by
// whitespace splitting).
func cfgAgentPath(entries []config.AgentEntry, name string) string {
	for _, e := range entries {
		if e.Name != name || e.Command == "" {
			continue
		}
		if path := strings.TrimSpace(e.Command); path != "" {
			return path
		}
	}
	return ""
}
