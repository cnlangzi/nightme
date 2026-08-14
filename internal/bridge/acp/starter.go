// starter.go — the spawn recipe for the acp bridge.
//
// After the Agent → Info/Starter/Agent/driver refactor
// (wip/agent.md), the static metadata (name/command/args/env/
// cols/rows) lives on Starter and is exposed via Info(). The
// runtime state (transport/rpc/ctx/cancel/etc.) lives on driver
// and is exposed via the unexported driver interface. External
// callers never see *Starter or *driver directly — they interact
// via *agent.Agent, which Starter.Start returns.
package acp

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
)

// Starter is the acp spawn recipe. Held in agent.Builtins as a
// singleton per agent name.
type Starter struct {
	name    string
	command string
	args    []string
	env     []string
	cols    int
	rows    int
}

// NewStarter constructs the acp spawn recipe. Entry point used at
// registration time (cmd/nightme/agents.go calls it from init()).
//
// args are the command's protocol flags (e.g. the ACP server flag).
// Defensively copied. env is the spawn recipe's default env entries,
// also defensively copied. cols/rows set the initial PTY size; values
// <= 0 are normalized to 80x24 inside newDriver.
func NewStarter(name, command string, args, env []string, cols, rows int) *Starter {
	return &Starter{
		name:    name,
		command: command,
		args:    append([]string(nil), args...),
		env:     append([]string(nil), env...),
		cols:    cols,
		rows:    rows,
	}
}

// Info returns the fixed metadata for this starter. Observable
// at any time; used by `nightme agents` and any other spec-only
// consumer.
func (s *Starter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeACP, s.command, s.args, s.env)
}

// Detect verifies the binary resolves on PATH. Called by Spawner
// before Start; an error aborts session creation with a clear
// "<binary> not found" message.
func (s *Starter) Detect() error {
	_, err := exec.LookPath(s.command)
	return err
}

// Start spawns the CLI under a PTY, runs the ACP initialize +
// session/new handshake, and returns a live *agent.Agent. The
// caller (typically agentsession.AgentSession via the Spawner) must
// Close() the returned handle when done. The Starter is unchanged
// (reusable across many sessions).
//
// cfg.Workspace is the child process's cwd. cfg.Args are appended
// after the starter's defaults (user wins). cfg.Env is merged with
// the starter's defaults (cfg wins). cfg.SessionID, when non-empty,
// is appended as the resume id; ACP does not currently surface it
// over the wire (the bridge synthesizes a fresh one for now).
func (s *Starter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	d, err := newDriver(ctx, s, cfg)
	if err != nil {
		return nil, err
	}
	return agent.NewAgent(s.Info(), d.PID(), d.events, d), nil
}

// RunOnce is the one-shot counterpart to Start. Spawns the acp
// server, runs the initialize + session/new handshake, sends
// blocks, and collects the resulting EventAgentResult. Closes
// the session before returning.
//
// ACP has no CLI-side print-mode flag (no `claude -p` equivalent
// on the protocol layer), so the one-shot path reuses the
// long-lived Start recipe and delegates event collection to
// collectResult. Every other bridge that has a CLI-side one-shot
// flag (claudecode `claude -p`, codex `codex exec`, pi `pi -p`,
// opencode `opencode run`) routes RunOnce through its own
// print-mode spawn in print.go instead.
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
	a, err := s.Start(ctx, cfg)
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("agent %s: spawn: %w", s.Info().Name, err)
	}
	defer a.Close()
	return s.collectResult(ctx, a, blocks)
}

// collectResult sends blocks to a live *agent.Agent and drains
// its Events channel until the turn ends. Extracted from
// RunOnce so the dispatch logic can be unit-tested with a
// fakeDriver-backed *Agent instead of requiring a real acp
// server process. Caller is responsible for closing the live
// *Agent; RunOnce handles this via `defer a.Close()`.
//
// Return semantics:
//   - EventAgentResult: returns RunResult with Text, Usage,
//     SessionID (from the first EventAgentReady), Model (same),
//     DurationMs, Subtype populated.
//   - EventAgentDone with exit 0 but no preceding Result: error
//     "turn ended without result event" — captured session /
//     model are appended as audit fields so the failure is
//     still inspectable.
//   - EventAgentDone with non-zero exit: error mentioning exit
//     code, with audit fields appended.
//   - EventAgentError: wrapped error.
//   - ctx canceled: ctx.Err().
//   - events channel closed without result: error.
func (s *Starter) collectResult(ctx context.Context, live *agent.Agent, blocks []agent.ContentBlock) (agent.RunResult, error) {
	name := live.Info.Name

	// Track per-session identity (model + session id) from
	// EventAgentReady so the returned RunResult carries both.
	// First Ready in this turn wins (subsequent Ready events
	// after resume / re-handshake overwrite — those don't
	// belong to this turn's audit trail). If the bridge never
	// emits Ready the fields stay empty.
	var readySeen bool
	var sessionID, model string

	if err := live.SendBlocks(ctx, blocks); err != nil {
		return agent.RunResult{}, fmt.Errorf("agent %s: send: %w", name, err)
	}
	for {
		select {
		case ev, ok := <-live.Events():
			if !ok {
				return agent.RunResult{}, fmt.Errorf("agent %s: event stream closed without result%s", name, appendAuditFields(sessionID, model))
			}
			switch ev.Kind {
			case agent.EventAgentReady:
				if !readySeen {
					readySeen = true
					sessionID = ev.SessionID
					model = ev.Model
				}
			case agent.EventAgentResult:
				if ev.Result == nil {
					return agent.RunResult{}, fmt.Errorf("agent %s: result event with nil payload", name)
				}
				// TrimSpace so the dispatcher's success card has
				// consistent whitespace across bridges (PTY already
				// trims; structured bridges historically did not).
				return agent.RunResult{
					Text:       strings.TrimSpace(ev.Result.Text),
					Usage:      ev.Result.Usage,
					SessionID:  sessionID,
					Model:      model,
					DurationMs: ev.Result.DurationMs,
					Subtype:    ev.Result.Subtype,
				}, nil
			case agent.EventAgentDone:
				exit := 0
				if ev.Done != nil {
					exit = ev.Done.ExitCode
				}
				// Session identity (model / session_id) is the
				// only audit data we have at this point — Result
				// is terminal, so a "Done without Result" path
				// has no captured text / usage to surface.
				audit := appendAuditFields(sessionID, model)
				if exit != 0 {
					return agent.RunResult{}, fmt.Errorf("agent %s: turn ended with exit %d (no result text)%s", name, exit, audit)
				}
				return agent.RunResult{}, fmt.Errorf("agent %s: turn ended without result event%s", name, audit)
			case agent.EventAgentError:
				if ev.Err != nil {
					return agent.RunResult{}, fmt.Errorf("agent %s: %w", name, ev.Err)
				}
				return agent.RunResult{}, fmt.Errorf("agent %s: agent error event with nil payload", name)
			}
			// EventAgentText / EventAgentToolStart / etc. — keep draining.
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return agent.RunResult{}, fmt.Errorf("agent %s: canceled: %w", name, ctx.Err())
			}
			return agent.RunResult{}, fmt.Errorf("agent %s: %w", name, ctx.Err())
		}
	}
}

// appendAuditFields builds the audit-suffix string for the
// non-Result failure paths in collectResult. Format is
// symmetric with the claudecode / pi bridges' auditFields:
// [session_id=…] [model=…]. Empty when neither is captured.
func appendAuditFields(sessionID, model string) string {
	return agent.FormatSessionID(sessionID) + agent.FormatModel(model)
}
