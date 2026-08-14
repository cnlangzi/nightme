// starter.go — the spawn recipe for the dsh bridge.
//
// Starter is held in agent.Builtins as a singleton per agent name.
// Lifecycle:
//
//	Build-time:    NewStarter → Register(Starter) → Builtins holds *Starter
//	Spawn-time:    Builtins.Get → Starter.Info/Detect/RunOnce → RunResult
//	Chat session:  Builtins.Get → Starter.Start returns error (not implemented)
//
// The runtime state for print-mode lives in runPrintMode (print.go)
// and is re-created per RunOnce call. There is no long-lived child
// process — dsh --profile headless is one-shot, and the bridge
// mirrors that lifecycle.
package dsh

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/cnlangzi/nightme/internal/agent"
)

// Starter is the dsh spawn recipe. Held in agent.Builtins as a
// singleton for the registered name (typically "dsh").
//
// command is the executable name resolved via PATH at RunOnce
// time (exec.LookPath in runPrintMode). The Starter itself does
// not cache the absolute path so user-side PATH edits take effect
// without restarting the daemon.
type Starter struct {
	name    string
	command string
	args    []string
}

// NewStarter constructs the dsh spawn recipe. Entry point used at
// registration time (cmd/nightme/agents.go calls it from init()).
//
// args are the agent's static argv defaults (["--profile",
// "headless"]). Defensively copied.
func NewStarter(name string) *Starter {
	return &Starter{
		name:    name,
		command: name,
		args:    []string{"--profile", "headless"},
	}
}

// Info returns the fixed metadata for this starter. Observable at
// any time; used by `nightme agents` and any other spec-only
// consumer.
//
// Mode is reported as ModeJSONIO because dsh's `--profile headless`
// returns its result on stdout in a structured (single-message)
// form — closest match to the existing mode taxonomy. The bridge
// does not stream events, but Mode is metadata-only and does not
// gate capability checks elsewhere.
func (s *Starter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeJSONIO, s.command, s.args, nil)
}

// Detect verifies the dsh binary resolves on PATH. Called by
// Spawner before RunOnce (or before any future Start path); an
// error aborts invocation with a clear install hint.
//
// PATH check only — we deliberately do NOT probe dsh's internal
// config (`~/.dsh/settings.yaml`, `.credentials.yaml`) because
// per the agent-no-config-tampering principle, nightme does not
// own dsh's configuration lifecycle. If dsh is on PATH but its
// config is broken, the user will see dsh's own error message
// when runPrintMode spawns it.
//
// Note: this only verifies existence — we discard the resolved
// path. PATH resolution at spawn time happens implicitly inside
// exec.CommandContext at cmd.Start(); runPrintMode passes the
// unresolved name to agent.NewCmd and the kernel + Go stdlib do
// the rest. We don't cache the LookPath result because user-side
// PATH edits should take effect without restarting the daemon.
func (s *Starter) Detect() error {
	if _, err := exec.LookPath(s.command); err != nil {
		return fmt.Errorf("dsh: %q not found in PATH. Install via `npm install -g @deepseek-ai/dsh`", s.command)
	}
	return nil
}

// Start spawns dsh --profile web as a long-lived process, dials
// the two WebSocket downlinks (mux + host), performs session.create,
// and returns a live *agent.Agent that streams events on its
// Events channel. The starter is unchanged (reusable across many
// sessions).
//
// cfg.Workspace is the dsh web process's cwd (dsh's bash / fs
// plugins read process.cwd() set via cmd.Dir). cfg.SessionID is
// ignored — dsh web's session.resume wire isn't wired; new sessions
// are always created (server-side session-persistence-jsonl keeps
// the JSONL log, but resume would need a separate RPC).
func (s *Starter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	d, err := newDriver(ctx, s, cfg)
	if err != nil {
		return nil, err
	}
	return agent.NewAgent(s.Info(), d.cmd.Process.Pid, d.events, d), nil
}

// RunOnce is the one-shot counterpart to Start. Spawns a fresh
// `dsh --profile headless -- "<prompt>"` process, captures the
// final assistant text from stdout, and returns it as RunResult.
//
// dsh headless does NOT support stream-json / structured events
// (unlike codex exec, claude -p, or pi --print). It writes the
// final answer verbatim to stdout and exits. So the body here is
// leaner than the other print-mode bridges: no NDJSON parser, no
// tmpfile, no events chan. Plain spawn + read stdout.
//
// cmd.Dir is set to cfg.Workspace so the agent operates in the
// user's chat workspace (per /cwd). dsh's bash / fs plugins read
// `process.cwd()` (set by cmd.Dir) — we deliberately do NOT set
// DSH_CWD env var because it would be redundant with cmd.Dir and
// would conflict with the agent-no-config-tampering principle
// (which says: don't override agent configuration knobs).
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
	result, err := runPrintMode(ctx, s, cfg, blocks)
	if err != nil {
		return agent.RunResult{}, fmt.Errorf("agent %s: %w", s.Info().Name, err)
	}
	return result, nil
}
