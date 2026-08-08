// Package acp also provides the Agent wrapper that turns a CLI
// command into an agent.AgentSpec backed by the Agent Client Protocol
// defined in this package. PTY remains the physical carrier;
// ACP supplies the structured request and event layer above it.
//
// Lives in bridge/acp/ (not in a separate agent package) so the
// whole ACP story is one tree. See docs/feat/F-21-agent-modes.md §5.3.
package acp

import (
	"context"
	"os/exec"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/pty"
)

// Agent describes one ACP-speaking CLI.
type Agent struct {
	name    string
	command string
	args    []string
	env     []string
	cols    int
	rows    int
}

// NewAgent constructs an ACP agent descriptor. args are the
// command's protocol flags, such as the ACP server flag supported by
// a specific CLI.
//
// The agent stores args defensively; callers may mutate their input
// slice after the call returns.
func NewAgent(name, command string, args []string) *Agent {
	return &Agent{name: name, command: command, args: append([]string(nil), args...)}
}

func (a *Agent) Name() string { return a.name }

func (a *Agent) Mode() agent.Mode { return agent.ModeACP }

// Command returns the CLI binary the agent wraps. Surfaced by
// `nightme agents` so users can see what /run would spawn.
func (a *Agent) Command() string { return a.command }

// Args returns a defensive copy of the spawn recipe's default argv.
// Callers may not mutate the returned slice.
func (a *Agent) Args() []string {
	return append([]string(nil), a.args...)
}

func (a *Agent) Detect() error {
	_, err := exec.LookPath(a.command)
	return err
}

func (a *Agent) Start(ctx context.Context, cfg agent.StartConfig) (agent.Agent, error) {
	args := append([]string(nil), a.args...)
	args = append(args, cfg.Args...)
	env := append([]string(nil), a.env...)
	env = append(env, cfg.Env...)
	cols, rows := a.cols, a.rows
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}

	bridge, err := pty.NewBridge(cfg.Workspace, a.command, args, env, cols, rows)
	if err != nil {
		return nil, err
	}
	session, err := NewSession(ctx, bridge, a.name, WithWorkspace(cfg.Workspace))
	if err != nil {
		_ = bridge.Close()
		return nil, err
	}
	return session, nil
}

var _ agent.AgentSpec = (*Agent)(nil)