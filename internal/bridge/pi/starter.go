
// starter.go — the spawn recipe for the pi bridge.
//
// After the Agent → Info/Starter/Agent/driver refactor
// (wip/agent.md), the static metadata (name/command/args) lives
// on Starter and is exposed via Info(). The runtime state (cmd,
// pipes, RPC client, goroutines) lives on driver and is exposed
// via the unexported driver interface. External callers never
// see *Starter or *driver directly — they interact via
// *agent.Agent, which Starter.Start returns.
package pi

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/cnlangzi/nightme/internal/agent"
)

// Starter is the pi spawn recipe. Held in agent.Builtins as a
// singleton per agent name.
type Starter struct {
	name    string
	command string
	args    []string
}

// NewStarter constructs the pi spawn recipe. Entry point used at
// registration time (cmd/nightme/agents.go calls it from init()).
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

// Detect verifies the `pi` binary resolves on PATH. Called by
// Spawner before Start; an error aborts session creation with a
// clear "pi not installed" message.
func (s *Starter) Detect() error {
	_, err := exec.LookPath(s.command)
	return err
}

// Start spawns Pi in RPC mode and returns a live *agent.Agent
// that streams events on its Events channel. The Starter is
// unchanged (reusable across many sessions).
//
// cfg.Workspace is the child process's cwd. cfg.Args are appended
// after the agent's defaults. cfg.Env is appended to os.Environ()
// for the child. cfg.SessionID is not used by pi (no resume
// semantics).
func (s *Starter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	if cfg.Workspace == "" {
		return nil, fmt.Errorf("pi: workspace is required")
	}
	d, err := newDriver(ctx, s, cfg)
	if err != nil {
		return nil, err
	}
	return agent.NewAgent(s.Info(), d.pid, d.events, d), nil
}
// RunOnce is the one-shot counterpart to Start. Spawns a fresh
// RPC session, sends blocks, and drains Events() until the agent
// produces its final text result. Closes the session before
// returning. pi is a long-lived bridge (single process, many
// turns), but RunOnce only consumes one turn — the EventAgentDone
// that fires at turn end carries Reason:"settled" so the runtime
// distinguishes turn-end from process-end. We don't care about
// that distinction here because we Close the live immediately.
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock) (string, error) {
	a, err := s.Start(ctx, cfg)
	if err != nil {
		return "", fmt.Errorf("agent %s: spawn: %w", s.Info().Name, err)
	}
	defer a.Close()
	return agent.RunOnceDrain(ctx, a, blocks, s.Info().Name)
}
