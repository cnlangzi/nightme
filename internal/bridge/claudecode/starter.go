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
			// Wrap with both sentinels (bridge-level for legacy
			// callers, agent-level for chatsession auto-recovery).
			// fmt.Errorf's %w only retains the last wrap, so the
			// error type below exposes Is() to match both.
			return nil, resumeUnhealthyError{
				reason:  reason,
				session: cfg.SessionID,
			}
		}
	}

	return agent.NewAgent(s.Info(), d.pid, d.events, d), nil
}

// resumeUnhealthyError is returned by Start when probeResume
// detects a stderr rejection of the requested --resume session
// id. It satisfies errors.Is for BOTH claudecode.ErrResumeUnhealthy
// (the legacy bridge-level sentinel callers may have imported)
// AND agent.ErrResumeUnhealthy (the cross-package sentinel the
// chat layer uses to drive auto-recovery). fmt.Errorf's %w only
// retains the last wrap, so we expose Is() instead.
type resumeUnhealthyError struct {
	reason  string
	session string
}

func (e resumeUnhealthyError) Error() string {
	return fmt.Sprintf("%s: %s (session_id=%s); check workspace path and resume id",
		ErrResumeUnhealthy.Error(), e.reason, e.session)
}

func (e resumeUnhealthyError) Is(target error) bool {
	return target == ErrResumeUnhealthy || target == agent.ErrResumeUnhealthy
}

// RunOnce is the one-shot counterpart to Start. Spawns a fresh
// `claude -p <prompt>` print-mode session (process exits after
// the turn), streams events through the shared stream.go
// translator, and returns the agent's final text on a clean run.
//
// As of F-CLAUDE-PRINT-001 (2026-08-14), RunOnce routes through
// the print-mode spawn (print_unix.go) rather than the long-
// lived stream-json session that Start uses. Rationale:
//
//   - One-shot invocations (/gtw commit, buildAgentPrompt)
//     don't need a multi-turn session; they spawn, do the
//     work, and exit.
//   - The Start path was observed to carry unnecessary
//     surface for one-shot: stream-json stdin correlation,
//     resume-preservation probe, busy guard, held-open pipe.
//     Mirrors pi's F-PI-PRINT-001 (2026-08-13) finding that
//     the long-lived path is unreliable for single-turn use
//     while print-mode is deterministic.
//   - Print-mode uses positional `-p <prompt>` (no stdin reads),
//     runs the turn, emits the result event, and exits. The
//     shared stream.go translator consumes the wire events
//     without modification.
//
// Start (above) is unchanged: it still opens the stream-json
// held-stdin session for the chat-session use case where many
// turns ride one bridge. RunOnce and Start share the same
// Starter; only the spawn path differs.
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
	result, err := runPrintMode(ctx, s, cfg, blocks)
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("agent %s: %w", s.Info().Name, err)
	}
	return result, nil
}

// Review implements the /review slash command for the claudecode
// bridge. Delegates to the canonical agent.DefaultReview, which
// injects the shared StandardPrompt into the current chat session
// via rc.Inject. The chat agent then reads the prompt, runs git
// itself, and produces a structured review.
//
// (Future v2: claude could override Review to inject the
// "/code-review" built-in slash trigger instead of StandardPrompt,
// if its multi-agent review pipeline produces higher-quality
// findings than our shared prompt.)
func (s *Starter) Review(ctx context.Context, rc agent.ReviewContext) error {
	return agent.Review(ctx, s, rc)
}
