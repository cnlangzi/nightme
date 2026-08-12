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
// Selection rules (preserved from the original cmd/nightme/test.go
// doc):
//
//  1. Built-in starters (claudecode / codex / opencode / pi /
//     acp / pty) are always registered first.
//  2. cfg.Agents entries override builtins with the same name
//     (the user's config wins). Each entry's Command is split
//     with strings.Fields; the first token is the binary,
//     the rest are args. The agent is registered as a PTY
//     starter so the long-running daemon still has a TTY for
//     its child CLIs (Claude Code / Codex / OpenCode all
//     require one).
//  3. If `requested` is non-empty AND not already in the
//     registry, auto-register a bare-path agent when the file
//     exists — so a typo surfaces as "agent not found" instead
//     of a confusing exec error. (The CLI's `--agent /some/bin`
//     path uses this.)
package agentregistry

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/pty"
	"github.com/cnlangzi/nightme/internal/config"
)

// Build returns a Registry populated with the built-in
// starters, every cfg.Agents entry, and an optional bare-path
// agent named by `requested`. Pass requested="" to skip the
// auto-register step (the long-running daemon's default —
// `cfg.Primary` selects from the registered set rather than
// auto-registering a bare path).
func Build(cfg *config.Config, requested string) *agent.Registry {
	reg := agent.New()
	for _, a := range agent.Builtins.List() {
		reg.Register(a)
	}
	if cfg != nil && len(cfg.Agents) > 0 {
		for _, entry := range cfg.Agents {
			if entry.Name == "" || entry.Command == "" {
				continue
			}
			// v1.2 schema: Command is the full command line
			// (binary + args), e.g. "claude --dangerously-skip-permissions".
			// Split with strings.Fields; first token is the binary.
			fields := strings.Fields(entry.Command)
			if len(fields) == 0 {
				continue
			}
			a := pty.NewStarter(entry.Name, fields[0], fields[1:], nil, cfg.Session.DefaultPtyCols, cfg.Session.DefaultPtyRows)
			reg.Register(a)
		}
	}
	if _, err := reg.Get(requested); err != nil {
		// Auto-register a bare-path agent when the caller passed
		// a name that wasn't already in the registry. Only do
		// this if the file exists so a typo surfaces as
		// "agent not found" instead of a confusing exec error.
		if requested != "" {
			if _, statErr := os.Stat(requested); statErr == nil {
				reg.Register(pty.NewStarter(requested, filepath.Base(requested), nil, nil, 0, 0))
			}
		}
	}
	return reg
}