//go:build !windows

// starter.go — the spawn recipe for the claudecode bridge.
//
// After the Agent → Info/Starter/Agent/driver refactor
// (wip/agent.md), the static metadata (name/command/args) lives
// on starter and is exposed via Info(). The runtime state (cmd,
// pipes, goroutines) lives on driver and is exposed via the
// unexported driver interface. External callers never see
// *starter or *driver directly — they interact via
// *agent.Agent, which starter.Start returns.
//
// starter is reusable: agent.Builtins holds ONE *starter per
// agent name; every Spawn call invokes starter.Start and gets
// back an independent *agent.Agent wrapping a fresh driver.
package claudecode

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"

	"github.com/cnlangzi/nightme/internal/agent"
)

// starter is the claudecode spawn recipe. Held in agent.Builtins
// as a singleton per agent name. Lifecycle:
//
//	Build-time:    NewStarter → Register(Starter) → Builtins holds *Starter
//	Spawn-time:    Builtins.Get → Starter.Info/Detect/Start → *driver → *agent.Agent
//	Teardown:      starter itself is never mutated or freed
type Starter struct {
	name    string
	command string
	args    []string
}

// NewStarter constructs the claudecode spawn recipe. This is the
// entry point used at registration time (cmd/nightme/agents.go
// calls it from init()); the returned *Starter is held by
// agent.Builtins as the singleton for `name`.
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

// Detect verifies the `claude` binary resolves on PATH. Called
// by Spawner before Start; an error aborts session creation with
// a clear "claude not installed" message.
func (s *Starter) Detect() error {
	_, err := exec.LookPath(s.command)
	return err
}

// Start spawns Claude Code in stream-json mode and returns a live
// *agent.Agent that streams parsed events on its Events
// channel. The starter is unchanged (reusable across many
// sessions).
//
// cfg.Workspace is the child process's cwd. cfg.Args are appended
// after the agent's defaults. cfg.Env is appended to os.Environ()
// for the child. cfg.PermissionMode overrides the
// --permission-mode placeholder baked into DefaultArgs (empty
// falls back to PermissionBypass). cfg.SessionID, when non-empty,
// is appended as `--resume <id>` so the child resumes the
// previous Claude Code session.
//
// Resume-preservation (T-alive, 2026-08-07): when cfg.SessionID
// is set, the spawn is probed for stderr-detected rejection
// signals. On detection, the bridge returns ErrResumeUnhealthy
// instead of silently falling back to a fresh session.
func (s *Starter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	if cfg.Workspace == "" {
		return nil, fmt.Errorf("claudecode: workspace is required")
	}

	d, err := newDriver(ctx, s, cfg)
	if err != nil {
		return nil, err
	}
	if cfg.SessionID != "" {
		if reason, unhealthy := probeResume(ctx, d); unhealthy {
			slog.Warn("claudecode: --resume spawn unhealthy; refusing fallback to preserve resume context",
				"resume_id", cfg.SessionID, "reason", reason)
			_ = d.Close()
			return nil, fmt.Errorf("%w: %s (session_id=%s); check workspace path and resume id",
				ErrResumeUnhealthy, reason, cfg.SessionID)
		}
	}

	return agent.NewAgent(s.Info(), d.pid, d.events, d), nil
}
// RunOnce is the one-shot counterpart to Start. Spawns a fresh
// stream-json session, sends blocks, and drains Events() until
// the agent produces its final text result. Closes the session
// before returning.
//
// We bypass Start's resume-preservation probe (cfg.SessionID is
// always empty for one-shot), so we call newDriver directly via
// the Start path. The shared agent.RunOnceDrain helper handles
// send + drain.
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock) (string, error) {
	a, err := s.Start(ctx, cfg)
	if err != nil {
		return "", fmt.Errorf("agent %s: spawn: %w", s.Info().Name, err)
	}
	defer a.Close()
	return agent.RunOnceDrain(ctx, a, blocks, s.Info().Name)
}
