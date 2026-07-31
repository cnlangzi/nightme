// Package acpagent implements agent.Agent for CLIs that speak the Agent
// Client Protocol over their stdio stream. PTY remains the physical carrier;
// ACP supplies the structured request and event layer above it.
package acpagent

import (
	"context"
	"os/exec"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/acp"
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

// New constructs an ACP agent descriptor. args are the command's protocol
// flags, such as the ACP server flag supported by a specific CLI.
func New(name, command string, args []string) *Agent {
	return &Agent{name: name, command: command, args: append([]string(nil), args...)}
}

func (a *Agent) Name() string { return a.name }

func (a *Agent) Mode() agent.Mode { return agent.ModeACP }

func (a *Agent) Detect() error {
	_, err := exec.LookPath(a.command)
	return err
}

func (a *Agent) Start(ctx context.Context, cfg agent.StartConfig) (agent.AgentSession, error) {
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

	bridge, err := pty.New(cfg.Workspace, a.command, args, env, cols, rows)
	if err != nil {
		return nil, err
	}
	session, err := acp.NewSession(ctx, bridge, a.name, acp.WithWorkspace(cfg.Workspace))
	if err != nil {
		_ = bridge.Close()
		return nil, err
	}
	return session, nil
}

var _ agent.Agent = (*Agent)(nil)
