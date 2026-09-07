// Package agentregistry — build an agent.Registry from a
// config.Config.
//
// Lives outside internal/agent (which already imports bridge/pty)
// and outside internal/runtime (which would force cmd/nightme to
// import the daemon). Holds the Builtins + cfg.Agents + bare-path
// logic shared by `nightme test` and the daemon runtime.
//
// Build applies cfg.Agents path overrides to the seven built-in
// singletons via Starter.Init. cfg.Agents entries whose name is
// not a built-in are silently ignored — no PTY fallback, no
// alias. When `requested` is non-empty and not already in the
// registry, Build auto-registers a bare-path PTY starter when the
// file exists on disk (one-shot `nightme test --agent /some/bin`).
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
