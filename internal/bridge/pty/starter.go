// starter.go — the spawn recipe for the pty bridge.
//
// After the Agent → Info/Starter/Agent/driver refactor
// (wip/agent.md), the static metadata (name/command/args/env/
// cols/rows) lives on Starter and is exposed via Info(). The
// runtime state (transport/events/closed) lives on driver and is
// exposed via the unexported driver interface.
package pty

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// Starter is the pty spawn recipe. Held in agent.Builtins as a
// singleton per agent name (the "bash" / "sh" / etc. fallback
// for unknown user CLIs).
type Starter struct {
	name    string
	command string
	args    []string
	env     []string
	cols    int
	rows    int
}

// Info returns the fixed metadata for this starter. Observable
// at any time; used by `nightme agents` and any other spec-only
// consumer.
func (s *Starter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModePTY, s.command, s.args, s.env)
}

// Detect verifies the binary resolves on PATH. Called by Spawner
// before Start; an error aborts session creation with a clear
// "<binary> not installed" message.
func (s *Starter) Detect() error {
	_, err := exec.LookPath(s.command)
	return err
}

// Start spawns the CLI under a PTY and returns a live
// *agent.Agent that streams events on its Events channel. The
// Starter is unchanged (reusable across many sessions).
//
// cfg.Workspace is the child process's cwd. cfg.Args are appended
// after the starter's defaults. cfg.Env is merged with the
// starter's defaults (cfg wins). cfg.SessionID, when non-empty,
// is forwarded as the resume id (raw PTY bridges don't currently
// surface it over the wire — the bridge synthesizes a fresh one
// for now).
func (s *Starter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	d, err := newDriver(ctx, s, cfg)
	if err != nil {
		return nil, err
	}
	return agent.NewAgent(s.Info(), d.PID(), d.events, d), nil
}

// agentDriver is the local alias for the agent.driver interface so
// this file can compile-time check driver satisfies it without
// importing the unexported name from the agent package.
// The matching `var _ agentDriver = (*driver)(nil)` check lives in
// agent.go alongside the type so both files are self-contained.
type agentDriver interface {
	SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error
	SendPermission(resp string) error
	Reset(ctx context.Context) error
	Close() error
}
// ptyIdleTimeout is how long RunOnce waits with no new output
// before declaring the turn done. PTY has no structured result
// event, so the heuristic is "agent went quiet for this long".
// 3s is enough to cover inter-tool-call pauses in shell agents
// without dragging out genuinely-stuck sessions (the parent push
// wraps RunOnce in a 5-min deadline as a backstop).
const ptyIdleTimeout = 3 * time.Second

// RunOnce is the one-shot counterpart to Start for PTY-backed
// agents. It opens a live PTY session, writes blocks to stdin,
// and collects EventAgentText until either EventAgentDone arrives
// or no new bytes have arrived for ptyIdleTimeout. Returns the
// concatenated text.
//
// PTY has no structured "result" event — every byte from the
// child is emitted as EventAgentText, and the only terminal
// signal is EventAgentDone{ExitCode: -1} on transport EOF. The
// idle heuristic is the only practical way to detect "the agent
// finished writing".
//
// The idle timer is "first-byte" — it starts ONLY after the first
// EventAgentText arrives. Without this guard, a slow PTY-wrapped
// CLI whose first byte takes >ptyIdleTimeout to appear (e.g.
// shell wrapper initialization) would be declared done
// prematurely with an empty reply.
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
	a, err := s.Start(ctx, cfg)
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("agent %s: spawn: %w", s.Info().Name, err)
	}
	defer a.Close()

	if err := a.SendBlocks(ctx, blocks); err != nil {
		return agent.RunResult{}, fmt.Errorf("agent %s: send: %w", s.Info().Name, err)
	}

	var sb strings.Builder
	var idle *time.Timer
	resetIdle := func() {
		if idle != nil {
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
		}
		idle = time.NewTimer(ptyIdleTimeout)
	}
	defer func() {
		if idle != nil {
			idle.Stop()
		}
	}()

	for {
		select {
		case ev, ok := <-a.Events():
			if !ok {
				return agent.RunResult{Text: strings.TrimSpace(sb.String())}, nil
			}
			switch ev.Kind {
			case agent.EventAgentText:
				sb.WriteString(ev.Text)
				resetIdle()
			case agent.EventAgentError:
				if ev.Err != nil {
					return agent.RunResult{}, fmt.Errorf("agent %s: %w", s.Info().Name, ev.Err)
				}
				return agent.RunResult{}, fmt.Errorf("agent %s: error event with nil payload", s.Info().Name)
			case agent.EventAgentDone:
				return agent.RunResult{Text: strings.TrimSpace(sb.String())}, nil
			}
		case <-idle.C:
			return agent.RunResult{Text: strings.TrimSpace(sb.String())}, nil
		case <-ctx.Done():
			// On ctx cancellation, drop the partial text — PTY has
			// no structured result event, so any output we
			// collected is "what the agent was in the middle of
			// writing", not a final answer. Returning it alongside
			// the error would mislead the caller (push command)
			// into thinking the agent had produced something
			// usable.
			return agent.RunResult{}, fmt.Errorf("agent %s: canceled: %w", s.Info().Name, ctx.Err())
		}
	}
}

// Review implements /review for pty/bash fallback: bash is not a
// coding agent, can't read git diff, can't output structured
// review. Return ErrReviewNotSupported so the /review dispatcher
// can surface a friendly "agent X 暂不支持 /review" reply.
func (s *Starter) Review(ctx context.Context, rc agent.ReviewContext) error {
	return agent.ErrReviewNotSupported
}
