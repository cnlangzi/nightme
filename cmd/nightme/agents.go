// Package main — agent registration table.
//
// This file is the single source of truth for which agents ship
// with nightme at compile time. Each line is one entry; the agent
// name is what /run <name> (and `nightme agents`) accepts, the
// bridge package supplies the underlying protocol.
//
// To add a new built-in agent:
//  1. Implement an `agent.Agent` constructor in the relevant
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
	"github.com/cnlangzi/nightme/internal/bridge/pty"
)

func init() {
	// claude — the JSON-IO bridge. Dedicated implementation; user
	// config can override the command path but the override drops
	// back to PTY (loses AskUserQuestion, structured events).
	agent.Builtins.Register(claudecode.New("claude", "claude", nil))

	// bash — example PTY-backed entry. Shows the registration
	// shape for any binary the user might want to launch without
	// an ACP/JSON-IO layer.
	agent.Builtins.Register(pty.NewAgent("bash", "bash", nil, nil))
}