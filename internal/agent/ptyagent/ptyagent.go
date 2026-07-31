// Package ptyagent implements agent.Agent for CLI tools that expose
// no structured protocol — they are spawned inside a PTY and the
// bytes are forwarded as AgentEvents.
//
// This is the v0.1 fallback for any AI Coding CLI that does not yet
// implement ACP. Claude Code is registered as ModePTY in M2 for the
// same reason; v0.2 introduces a dedicated SDK adapter (see
// docs/feat/F-21-agent-modes.md §5.3).
package ptyagent

import (
	"context"
	"os/exec"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/pty"
)

// Agent is a thin descriptor for one CLI wrapped in PTY mode.
//
// A zero-value Agent is not usable; populate the fields before
// calling Start.
type Agent struct {
	name    string
	Command string
	Args    []string
	Env     []string

	// Cols and Rows set the initial PTY size. Zero values fall back
	// to 80x24, matching config.SessionConfig defaults.
	Cols int
	Rows int
}

// New constructs an Agent. name is the registry key, command is the
// CLI binary (resolved via PATH at Start time).
func New(name, command string) *Agent {
	return &Agent{name: name, Command: command}
}

// Name returns the agent identifier used in the registry and config.
func (a *Agent) Name() string { return a.name }

// Mode reports ModePTY so the SessionManager routes through the PTY
// backend.
func (a *Agent) Mode() agent.Mode { return agent.ModePTY }

// Detect verifies the underlying CLI binary is on PATH. Callers
// should invoke this before Start to produce a friendly "X not found"
// error rather than letting Start fail deep inside the PTY layer.
func (a *Agent) Detect() error {
	_, err := exec.LookPath(a.Command)
	return err
}

// Start spawns the CLI under a PTY and returns an AgentSession that
// streams PTY bytes as EventText. The session is owned by the caller
// and must be Close()d.
//
// Start honors cfg.Workspace as the child's working directory; any
// cfg.Args are appended after the agent's defaults, and cfg.Env is
// merged with the agent's defaults in that order (cfg wins).
func (a *Agent) Start(ctx context.Context, cfg agent.StartConfig) (agent.AgentSession, error) {
	cols := a.Cols
	rows := a.Rows
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}

	// arg order: agent defaults, then user overrides (user wins).
	args := append([]string(nil), a.Args...)
	args = append(args, cfg.Args...)

	// env order: agent defaults, then per-session overrides (cfg wins).
	env := append([]string(nil), a.Env...)
	env = append(env, cfg.Env...)

	// ctx is currently unused — Start blocks synchronously. The
	// context is reserved for a future cancellation hook that
	// propagates to the child process via gopty.CmdContext.
	_ = ctx

	bridge, err := pty.New(cfg.Workspace, a.Command, args, env, cols, rows)
	if err != nil {
		return nil, err
	}

	session := pty.NewPtySession(bridge)
	session.Start()
	return session, nil
}

// Compile-time guarantee that *Agent satisfies agent.Agent.
var _ agent.Agent = (*Agent)(nil)
