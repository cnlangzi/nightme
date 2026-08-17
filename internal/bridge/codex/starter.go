
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
// RunOnce is the one-shot counterpart to Start. Delegates to
// runPrintMode in print.go, which spawns `codex exec` directly
// (bypassing the long-lived app-server pipeline) — see the file
// doc on print.go for rationale (F-CODEX-PRINT-001, 2026-08-14).
//
// The previous implementation was `Start + defer Close +
// drain Events()` — still the pattern acp uses today (acp has
// no CLI-side print-mode flag), but for codex / opencode the
// 5s closeDrainTimeout + handshake / SSE subscription overhead
// made it the wrong shape for one-shot. That pattern
// paid the full app-server handshake + 5s closeDrainTimeout cost
// per call, which is wasted work for one-shot uses (/gtw commit,
// /gtw pr, buildAgentPrompt).
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
	return runPrintMode(ctx, s, cfg, blocks)
}

// Review implements /review for codex. F-review.md §13
// "codex/claude use native review" rule: codex has a native
// `codex review` subcommand, so we invoke it directly instead of
// running our generic StandardPrompt via `codex exec <prompt>`.
// The native subcommand is structured for the review task —
// reusing it is strictly better than reverse-engineering the same
// review into a generic prompt-mode call.
//
// Output is wrapped in agent.FormatReviewMessage (the canonical
// preamble) by the caller. If body parsing fails (anomalous
// format), FormatReviewMessage falls back to raw output with a
// warning log.
func (s *Starter) Review(ctx context.Context, rc agent.ReviewContext) error {
	result, err := runCodexReview(ctx, s, agent.StartConfig{
		Workspace: rc.Workspace,
	})
	if err != nil {
		return fmt.Errorf("agent %s: review one-shot failed: %w",
			s.Info().Name, err)
	}
	return rc.Inject(ctx, []agent.ContentBlock{{
		Type: agent.ContentText,
		Text: agent.FormatReviewMessage(rc.Workspace, s.Info().Name, result.Text),
	}})
}
