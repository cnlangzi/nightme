// starter.go — the spawn recipe for the acp bridge.
//
// After the Agent → Info/Starter/Agent/driver refactor
// (wip/agent.md), the static metadata (name/command/args/env/
// cols/rows) lives on Starter and is exposed via Info(). The
// runtime state (transport/rpc/ctx/cancel/etc.) lives on driver
// and is exposed via the unexported driver interface. External
// callers never see *Starter or *driver directly — they interact
// via *agent.Agent, which Starter.Start returns.
package acp

import (
	"context"
	"os/exec"

	"github.com/cnlangzi/nightme/internal/agent"
)

// Starter is the acp spawn recipe. Held in agent.Builtins as a
// singleton per agent name.
type Starter struct {
	name    string
	command string
	args    []string
	env     []string
	cols    int
	rows    int
}

// NewStarter constructs the acp spawn recipe. Entry point used at
// registration time (cmd/nightme/agents.go calls it from init()).
//
// args are the command's protocol flags (e.g. the ACP server flag).
// Defensively copied. env is the spawn recipe's default env entries,
// also defensively copied. cols/rows set the initial PTY size; values
// <= 0 are normalized to 80x24 inside newDriver.
func NewStarter(name, command string, args, env []string, cols, rows int) *Starter {
	return &Starter{
		name:    name,
		command: command,
		args:    append([]string(nil), args...),
		env:     append([]string(nil), env...),
		cols:    cols,
		rows:    rows,
	}
}

// Info returns the fixed metadata for this starter. Observable
// at any time; used by `nightme agents` and any other spec-only
// consumer.
func (s *Starter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeACP, s.command, s.args, s.env)
}

// Detect verifies the binary resolves on PATH. Called by Spawner
// before Start; an error aborts session creation with a clear
// "<binary> not found" message.
func (s *Starter) Detect() error {
	_, err := exec.LookPath(s.command)
	return err
}

// Start spawns the CLI under a PTY, runs the ACP initialize +
// session/new handshake, and returns a live *agent.Agent. The
// caller (typically chatsession.AgentSession via the Spawner) must
// Close() the returned handle when done. The Starter is unchanged
// (reusable across many sessions).
//
// cfg.Workspace is the child process's cwd. cfg.Args are appended
// after the starter's defaults (user wins). cfg.Env is merged with
// the starter's defaults (cfg wins). cfg.SessionID, when non-empty,
// is appended as the resume id; ACP does not currently surface it
// over the wire (the bridge synthesizes a fresh one for now).
func (s *Starter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	d, err := newDriver(ctx, s, cfg)
	if err != nil {
		return nil, err
	}
	return agent.NewAgent(s.Info(), d.PID(), d.events, d), nil
}