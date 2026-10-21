// Package omp — starter (spawn recipe + lifecycle entry
// points).
//
// Starter is the spawn recipe; held in agent.Builtins as a
// singleton per agent name. The runtime state (cmd, pipes,
// RPC client, goroutines) lives on driver and is exposed via
// the unexported driver interface. External callers never see
// *Starter or *driver directly — they interact via
// *agent.Agent, which Starter.Start returns.
package omp

import (
	"context"
	"fmt"

	"github.com/cnlangzi/nightme/internal/agent"
)

// Starter is the omp spawn recipe.
type Starter struct {
	name           string
	command        string
	defaultCommand string
	args           []string
}

// NewStarter constructs the omp spawn recipe. Entry point
// used at registration time (cmd/nightme/agents.go calls it
// from init()).
func NewStarter(name, command string, args []string) *Starter {
	return &Starter{
		name:           name,
		command:        command,
		defaultCommand: command,
		args:           append([]string(nil), args...),
	}
}

// Info returns the fixed metadata for this starter.
func (s *Starter) Info() agent.Info {
	args := make([]string, 0, len(DefaultArgs)+len(s.args))
	args = append(args, DefaultArgs...)
	args = append(args, s.args...)
	// ModeJSONIO: omp --mode rpc is a NDJSON / JSON-event
	// stream over stdio, matching the pi bridge's surface.
	return agent.NewInfo(s.name, agent.ModeJSONIO, s.command, args, ompHeadlessEnv)
}

// Detect verifies the configured command resolves to an
// invokable binary. Called by Spawner before Start.
func (s *Starter) Detect() error {
	return agent.ResolveCommand(s.command)
}

// Init overrides the executable path. "" resets to the
// default baked in by NewStarter.
func (s *Starter) Init(command string) {
	if command == "" {
		s.command = s.defaultCommand
		return
	}
	s.command = command
}

// Command returns the current executable path.
func (s *Starter) Command() string { return s.command }

// Start spawns omp in RPC mode and returns a live
// *agent.Agent.
func (s *Starter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	if cfg.Workspace == "" {
		return nil, fmt.Errorf("omp: workspace is required")
	}
	d, err := newDriver(ctx, s, cfg)
	if err != nil {
		return nil, err
	}
	return agent.NewAgent(s.Info(), d.pid, d.events, d), nil
}

// RunOnce is the one-shot counterpart to Start. Drives the
// configured agent through a single prompt via print mode
// (`omp -p --mode json --approval-mode yolo`) and returns
// the final text + per-turn usage.
//
// As with the other print-mode bridges (pi / claudecode /
// codex / opencode / copilot), the rationale is to bypass the
// long-lived RPC handshake for callers that only need a
// single turn (/gtw commit, buildAgentPrompt, /review via
// RunOnce). RunOnce spawns, drains the NDJSON event stream
// through the same translator the RPC path uses, and exits.
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	result, err := runPrintMode(ctx, s, cfg, blocks, opts...)
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("agent %s: %w", s.Info().Name, err)
	}
	return result, nil
}

// Review implements /review for omp: delegate to the shared
// agent.ReviewDispatch. omp has no native review subcommand,
// so /review runs the Go-precompute-enhanced prompt via
// RunOnce.
func (s *Starter) Review(ctx context.Context, cfg agent.StartConfig, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.ReviewDispatch(ctx, s, cfg, opts...)
}
