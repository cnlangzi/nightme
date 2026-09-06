// Package agentregistry — build an agent.Registry from a
// config.Config.
//
// This package exists as a neutral home for the
// Builtins + cfg.Agents + bare-path-auto-register logic that
// both the CLI's `nightme test` subcommand and the long-running
// daemon (internal/runtime) need. It cannot live in
// internal/agent (the package imports bridge/pty, but
// bridge/pty already imports internal/agent — cycle) and it
// cannot live in internal/runtime (cmd/nightme would have to
// import runtime, but runtime is the daemon — moving it across
// the CLI/daemon boundary is wrong).
//
// Selection rules:
//
//  1. Built-in starters (claudecode / codex / opencode / cursor /
//     pi / copilot / dsh) are always registered first, in their
//     Builtins order. They are the whitelist — no other names can
//     become a primary agent.
//
//  2. cfg.Agents is consulted as a side lookup: for each
//     registered built-in, look up `cfg.Agents[name]` and apply
//     the override if present. Iteration is built-in-driven
//     rather than config-driven so names outside the whitelist
//     are never read — no warn-log, no silent PTY fallback. The
//     user only configures the executable path; bridge / args /
//     mode stay fixed by nightme.
//
//  3. If `requested` is non-empty AND not already in the
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

// Build returns a Registry populated with the built-in starters,
// every applicable cfg.Agents path override, and an optional
// bare-path agent named by `requested`. Pass requested="" to
// skip the auto-register step (the long-running daemon's default
// — `cfg.Primary` selects from the registered set rather than
// auto-registering a bare path).
func Build(cfg *config.Config, requested string) *agent.Registry {
	reg := agent.New()
	for _, a := range agent.Builtins.List() {
		reg.Register(a)
	}

	if cfg != nil && len(cfg.Agents) > 0 {
		// Built-in driven: iterate over the registered starters,
		// not over cfg.Agents. Names outside the whitelist are
		// never even inspected — they simply have no matching
		// built-in to apply to.
		overrides := cfgPathMap(cfg.Agents)
		for _, s := range reg.List() {
			name := s.Info().Name
			if path, ok := overrides[name]; ok {
				agent.SetBuiltinCommand(reg, name, path)
			}
		}
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

// cfgPathMap flattens cfg.Agents into a name → path lookup.
// Command is treated verbatim (after trimming) — the schema is
// "absolute path to one binary", not "command line". This
// matters on Windows where paths routinely contain spaces
// (`C:\Program Files\claude\claude.exe`); whitespace-splitting
// would truncate the path at the first space.
func cfgPathMap(entries []config.AgentEntry) map[string]string {
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.Name == "" || e.Command == "" {
			continue
		}
		path := strings.TrimSpace(e.Command)
		if path == "" {
			continue
		}
		out[e.Name] = path
	}
	return out
}
