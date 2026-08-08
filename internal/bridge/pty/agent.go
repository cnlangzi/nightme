// Package pty also provides the Agent wrapper that turns a CLI
// command into an agent.AgentSpec backed by the PTY transport defined
// in this package. The wrapper is the safe fallback for any binary
// that does not yet speak ACP / SDK / JSON-IO — bytes flow through
// the PTY as EventAgentText and the session manager drives them.
//
// Lives in bridge/pty/ (not in a separate agent package) so the
// whole PTY story is one tree. See docs/feat/F-21-agent-modes.md §5.3.
package pty

import (
	"context"
	"os/exec"

	"github.com/cnlangzi/nightme/internal/agent"
)

// Agent is a thin descriptor for one CLI wrapped in PTY mode.
//
// A zero-value Agent is not usable; populate the fields before
// calling Start.
type Agent struct {
	name    string
	command string
	args    []string
	env     []string

	// Cols and Rows set the initial PTY size. Zero values fall back
	// to 80x24, matching config.SessionConfig defaults.
	Cols int
	Rows int
}

// NewAgent constructs the Agent descriptor for a CLI driven through
// the PTY bridge. name is the registry key, command is the CLI
// binary (resolved via PATH at Start time). args are appended after
// the binary at Start time; env entries are KEY=VALUE strings merged
// into the child environment.
//
// Both args and env are defensively copied; callers may mutate their
// input slices after the call returns.
func NewAgent(name, command string, args, env []string) *Agent {
	return &Agent{
		name:    name,
		command: command,
		args:    append([]string(nil), args...),
		env:     append([]string(nil), env...),
	}
}

// Name returns the agent identifier used in the registry and config.
func (a *Agent) Name() string { return a.name }

// Mode reports ModePTY so the SessionManager routes through the PTY
// backend.
func (a *Agent) Mode() agent.Mode { return agent.ModePTY }

// Command returns the CLI binary the agent wraps. Surfaced by
// `nightme agents` so users can see what /run would spawn.
func (a *Agent) Command() string { return a.command }

// Args returns a defensive copy of the spawn recipe's default argv.
// Callers may not mutate the returned slice.
func (a *Agent) Args() []string {
	return append([]string(nil), a.args...)
}

// Detect verifies the underlying CLI binary is on PATH. Callers
// should invoke this before Start to produce a friendly "X not found"
// error rather than letting Start fail deep inside the PTY layer.
func (a *Agent) Detect() error {
	_, err := exec.LookPath(a.command)
	return err
}

// Start spawns the CLI under a PTY and returns an Agent that
// streams PTY bytes as EventAgentText. The session is owned by the caller
// and must be Close()d.
//
// Start honors cfg.Workspace as the child's working directory; any
// cfg.Args are appended after the agent's defaults, and cfg.Env is
// merged with the agent's defaults in that order (cfg wins).
func (a *Agent) Start(ctx context.Context, cfg agent.StartConfig) (agent.Agent, error) {
	cols := a.Cols
	rows := a.Rows
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}

	// arg order: agent defaults, then user overrides (user wins).
	args := append([]string(nil), a.args...)
	args = append(args, cfg.Args...)

	// env order: agent defaults, then per-session overrides (cfg wins).
	env := append([]string(nil), a.env...)
	env = append(env, cfg.Env...)

	// ctx is currently unused — Start blocks synchronously. The
	// context is reserved for a future cancellation hook that
	// propagates to the child process via gopty.CmdContext.
	_ = ctx

	bridge, err := NewBridge(cfg.Workspace, a.command, args, env, cols, rows)
	if err != nil {
		return nil, err
	}

	session := NewPtySession(bridge)
	session.Start()
	return session, nil
}

// Compile-time guarantee that *Agent satisfies agent.AgentSpec.
var _ agent.AgentSpec = (*Agent)(nil)