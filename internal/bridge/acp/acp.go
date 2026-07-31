// Package acp hosts the Agent Client Protocol (ACP) backend. The
// baseline implementation is stubbed in v0.1 and will be filled in
// for v0.2 — see docs/feat/F-21-agent-modes.md §5.1 and PLAN.md §3.3
// (commit 16).
//
// Stubs compile against agent.Agent so other packages can reference
// the symbols without dragging in a half-baked protocol stack.
package acp

import (
	"context"
	"errors"

	"github.com/cnlangzi/nightme/internal/agent"
)

// ErrNotImplemented is returned by every v0.1 stub in this package.
// The error message explicitly cites F-21 so future readers can find
// the design doc.
var ErrNotImplemented = errors.New("bridge/acp: not implemented in v0.1, see F-21")

// Agent is the ACP backend descriptor. It implements agent.Agent
// but its Start method always returns ErrNotImplemented.
type Agent struct {
	name    string
	Command string
	Args    []string
}

// New constructs an ACP-backed agent descriptor. The actual JSON-RPC
// client is not implemented in v0.1.
func New(name, command string) *Agent {
	return &Agent{name: name, Command: command}
}

// Name returns the registry key.
func (a *Agent) Name() string { return a.name }

// Mode reports ModeACP so the SessionManager routes through the ACP
// backend (when implemented).
func (a *Agent) Mode() agent.Mode { return agent.ModeACP }

// Detect verifies the binary is on PATH. v0.1 returns nil when the
// binary resolves so config validation can pass; once Start is
// implemented this can grow into a deeper capability handshake.
func (a *Agent) Detect() error {
	return nil
}

// Start is unimplemented in v0.1. Future work: spawn the ACP server,
// open a JSON-RPC client over stdio (typically piped through the
// PTY bridge), perform the Initialize / NewSession handshake, and
// return an AgentSession that emits structured events.
func (a *Agent) Start(context.Context, agent.StartConfig) (agent.AgentSession, error) {
	return nil, ErrNotImplemented
}

// Compile-time guarantee that *Agent satisfies agent.Agent.
var _ agent.Agent = (*Agent)(nil)