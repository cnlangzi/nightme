
// starter.go — the spawn recipe for the codex bridge.
//
// After the Agent → Info/Starter/Agent/driver refactor
// (wip/agent.md), the static metadata (name/command/args) lives
// on Starter and is exposed via Info(). The runtime state
// (session pointer, close machinery) lives on driver and is
// exposed via the unexported driver interface. External callers
// never see *Starter or *driver directly — they interact via
// *agent.Agent, which Starter.Start returns.
package codex

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/cnlangzi/nightme/internal/agent"
)

// Starter is the codex spawn recipe. Held in agent.Builtins as a
// singleton per agent name.
type Starter struct {
	name    string
	command string
	args    []string
}

// NewStarter constructs the codex spawn recipe. Entry point used
// at registration time (cmd/nightme/agents.go calls it from init()).
func NewStarter(name, command string, args []string) *Starter {
	return &Starter{
		name:    name,
		command: command,
		args:    append([]string(nil), args...),
	}
}

// Info returns the fixed metadata for this starter. Observable
// at any time; used by `nightme agents` and any other spec-only
// consumer.
func (s *Starter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeJSONIO, s.command, s.args, nil)
}

// Detect verifies the `codex` binary resolves on PATH. Called by
// Spawner before Start; an error aborts session creation with a
// clear "codex not installed" message.
func (s *Starter) Detect() error {
	_, err := exec.LookPath(s.command)
	return err
}

// Start spawns codex app-server and returns a live *agent.Agent
// that streams events on its Events channel. The Starter is
// unchanged (reusable across many sessions).
//
// cfg.Workspace is the child process's cwd. cfg.Args / cfg.Env are
// forwarded. cfg.SessionID, when non-empty, is forwarded as
// thread/resume. cfg.PermissionMode is ignored (the app-server uses
// approvalPolicy on a per-turn / per-thread basis; not exposed yet).
func (s *Starter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	if cfg.Workspace == "" {
		return nil, fmt.Errorf("codex: workspace is required")
	}
	d, err := newDriver(ctx, s, cfg)
	if err != nil {
		return nil, err
	}
	return agent.NewAgent(s.Info(), d.session.pid, d.session.events, d), nil
}
// RunOnce is the one-shot counterpart to Start. Spawns a fresh
// JSON-RPC session, sends blocks, and drains Events() until the
// agent produces its final text result. Closes the session
// before returning.
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
	a, err := s.Start(ctx, cfg)
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("agent %s: spawn: %w", s.Info().Name, err)
	}
	defer a.Close()
	return agent.RunOnceDrain(ctx, a, blocks, s.Info().Name)
}
