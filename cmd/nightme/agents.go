// Package main — agent registration table.
//
// This file is the single source of truth for which agents ship
// with nightme at compile time. Each line is one entry; the agent
// name is what /run <name> (and `nightme agents`) accepts, the
// bridge package supplies the underlying protocol.
//
// To add a new built-in agent:
//  1. Implement an `agent.AgentSpec` constructor in the relevant
//     `internal/bridge/<protocol>/` package (or extend one).
//  2. Add a Builtins.Register line below.
//
// There is no name-based dispatch table elsewhere in the binary —
// if an agent is not listed here and not in user config, /run
// returns "unknown agent". This is intentional.
package main

import (
	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/claudecode"
	"github.com/cnlangzi/nightme/internal/bridge/codex"
	"github.com/cnlangzi/nightme/internal/bridge/pi"
	"github.com/cnlangzi/nightme/internal/bridge/pty"
)

func init() {
	// claude — the JSON-IO bridge. Dedicated implementation; user
	// config can override the command path but the override drops
	// back to PTY (loses AskUserQuestion, structured events).
	agent.Builtins.Register(claudecode.NewStarter("claude", "claude", nil))

	// codex — the `codex app-server --listen stdio://` JSON-RPC 2.0
	// bridge. Spawns codex in app-server mode and drives it via
	// raw stdio pipes (no PTY). Single backend — see
	// docs/bridge/codex.md §1 for the rationale on not supporting
	// the legacy `codex exec` backend.
	agent.Builtins.LegacyRegister(codex.New("codex", "codex", nil))

	// pi — the long-lived `pi --mode rpc` JSONL bridge. The agent
	// driver is the @earendil-works/pi-coding-agent CLI; see
	// docs/feat/F-32-pi-rpc-bridge.md for wire details and the
	// F-32 MVP scope. User config can override the command path;
	// the structured bridge only works for builtin registration,
	// not for the PTY fallback in buildAgentRegistry.
	agent.Builtins.Register(pi.NewStarter("pi", "pi", nil))

	// bash — example PTY-backed entry. Shows the registration
	// shape for any binary the user might want to launch without
	// an ACP/JSON-IO layer.
	agent.Builtins.LegacyRegister(pty.NewAgent("bash", "bash", nil, nil))
}