// Package sdk hosts vendor-specific SDK adapters (e.g. the Claude
// Code Agent SDK). The package exists in v0.1 only to lay down the
// agent.Agent surface and reserve the directory; full implementation
// arrives in v0.2 — see docs/feat/F-21-agent-modes.md §5.2 and
// PLAN.md §3.3 (commit 18).
package sdk

import (
	"context"
	"errors"

	"github.com/cnlangzi/nightme/internal/agent"
)

// ErrNotImplemented is the v0.1 sentinel returned by every Start
// call in this package.
var ErrNotImplemented = errors.New("bridge/sdk: not implemented in v0.1, scheduled for v0.2")

// Agent is the SDK backend descriptor. It implements agent.Agent
// but its Start method always returns ErrNotImplemented in v0.1.
type Agent struct {
	name string
	SDK  string // SDK identifier (e.g. "claude-code") — reserved for v0.2.
}

// New constructs an SDK-backed agent descriptor.
func New(name, sdkID string) *Agent {
	return &Agent{name: name, SDK: sdkID}
}

// Name returns the registry key.
func (a *Agent) Name() string { return a.name }

// Mode reports ModeSDK so the SessionManager routes through the SDK
// backend (when implemented).
func (a *Agent) Mode() agent.Mode { return agent.ModeSDK }

// Detect is a no-op for SDK adapters — the SDK is a Go library, not
// an external binary, so there is nothing to resolve on PATH.
func (a *Agent) Detect() error { return nil }

// Start is unimplemented in v0.1. Future work: construct the
// vendor's SDK client (e.g. claudecode.NewClient), open a session,
// and wrap it in an AgentSession.
func (a *Agent) Start(context.Context, agent.StartConfig) (agent.AgentSession, error) {
	return nil, ErrNotImplemented
}

// Compile-time guarantee that *Agent satisfies agent.Agent.
var _ agent.Agent = (*Agent)(nil)