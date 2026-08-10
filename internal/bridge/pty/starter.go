// starter.go — the spawn recipe for the pty bridge.
//
// After the Agent → Info/Starter/Agent/driver refactor
// (wip/agent.md), the static metadata (name/command/args/env/
// cols/rows) lives on Starter and is exposed via Info(). The
// runtime state (transport/events/closed) lives on driver and is
// exposed via the unexported driver interface.
package pty

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/cnlangzi/nightme/internal/agent"
)

// Starter is the pty spawn recipe. Held in agent.Builtins as a
// singleton per agent name (the "bash" / "sh" / etc. fallback
// for unknown user CLIs).
type Starter struct {
	name    string
	command string
	args    []string
	env     []string
	cols    int
	rows    int
}

// Info returns the fixed metadata for this starter. Observable
// at any time; used by `nightme agents` and any other spec-only
// consumer.
func (s *Starter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModePTY, s.command, s.args, s.env)
}

// Detect verifies the binary resolves on PATH. Called by Spawner
// before Start; an error aborts session creation with a clear
// "<binary> not installed" message.
func (s *Starter) Detect() error {
	_, err := exec.LookPath(s.command)
	return err
}

// Start spawns the CLI under a PTY and returns a live
// *agent.Agent that streams events on its Events channel. The
// Starter is unchanged (reusable across many sessions).
//
// cfg.Workspace is the child process's cwd. cfg.Args are appended
// after the starter's defaults. cfg.Env is merged with the
// starter's defaults (cfg wins). cfg.SessionID, when non-empty,
// is forwarded as the resume id (raw PTY bridges don't currently
// surface it over the wire — the bridge synthesizes a fresh one
// for now).
func (s *Starter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	d, err := newDriver(ctx, s, cfg)
	if err != nil {
		return nil, err
	}
	return agent.NewAgent(s.Info(), d.PID(), d.events, d), nil
}

// pidForLive extracts the pid from a pty driver for the Agent.
// This is a placeholder; pty.Transport has its own PID() method
// that we use directly inside the live closure below.
var _ = fmt.Sprintf

// Compile-time guarantee that *driver satisfies the package-private
// agent.driver interface (SendText/SendBlocks/SendPermission/
// Reset/Close). External callers reach driver via *agent.Agent.
var _ agentDriver = (*driver)(nil)

// agentDriver is the local alias for the agent.driver interface so
// this file can compile-time check driver satisfies it without
// importing the unexported name from the agent package.
type agentDriver interface {
	SendText(text string) error
	SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error
	SendPermission(resp string) error
	Reset(ctx context.Context) error
	Close() error
}