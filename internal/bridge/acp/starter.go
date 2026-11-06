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

	"github.com/cnlangzi/nightme/internal/agent"
)

// OnSessionID is invoked synchronously after the acp server returns
// a sessionId from session/new and before the synthesized
// EventAgentReady is emitted. Returning a non-nil error aborts the
// spawn. The hook is generic — bridges that need pre-Ready prep
// (e.g. cursor pre-creates store.db to bridge the
// session/new→store.db timing gap) register one via
// WithSessionIDHook. Opencode / copilot / others don't register.
type OnSessionID func(sessionID string) error

// Starter is the acp spawn recipe. Held in agent.Builtins as a
// singleton per agent name.
type Starter struct {
	name           string
	command        string
	defaultCommand string
	args           []string
	env            []string
	cols           int
	rows           int
	onSessionID    OnSessionID
}

// StarterOpt customizes a Starter at construction time. Use
// WithSessionIDHook to register a pre-EventAgentReady hook.
type StarterOpt func(*Starter)

// WithSessionIDHook registers a callback invoked synchronously
// after session/new returns its id and before EventAgentReady is
// emitted. Returns the modified Starter for chaining.
func WithSessionIDHook(fn OnSessionID) StarterOpt {
	return func(s *Starter) { s.onSessionID = fn }
}

// NewStarter constructs the acp spawn recipe. Entry point used at
// registration time (cmd/nightme/agents.go calls it from init()).
//
// args are the command's protocol flags (e.g. the ACP server flag).
// Defensively copied. env is the spawn recipe's default env entries,
// also defensively copied. cols/rows set the initial PTY size; values
// <= 0 are normalized to 80x24 inside newDriver. Optional opts
// customize behavior — see WithSessionIDHook.
func NewStarter(name, command string, args, env []string, cols, rows int, opts ...StarterOpt) *Starter {
	s := &Starter{
		name:           name,
		command:        command,
		defaultCommand: command,
		args:           append([]string(nil), args...),
		env:            append([]string(nil), env...),
		cols:           cols,
		rows:           rows,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Info returns the fixed metadata for this starter. Observable
// at any time; used by `nightme agents` and any other spec-only
// consumer.
func (s *Starter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeACP, s.command, s.args, s.env)
}

// Detect verifies the configured command resolves to an invokable
// binary (absolute path or PATH-relative name). Called by Spawner
// before Start; an error aborts session creation.
func (s *Starter) Detect() error {
	return agent.ResolveCommand(s.command)
}

// Init overrides the executable path used by Detect and
// Info. cfg.Agents path overrides flow through
// agentregistry.Build. The mutation hits the singleton
// held in agent.Builtins; tests that exercise cfg.Agents
// overrides should snapshot and restore via Command().
func (s *Starter) Init(command string) {
	if command == "" {
		s.command = s.defaultCommand
		return
	}
	s.command = command
}

// Command returns the current executable path. Used by tests
// to snapshot Builtins state before mutations from
// agentregistry.Build.
func (s *Starter) Command() string { return s.command }

// Start spawns the CLI under a PTY, runs the ACP initialize +
// session/new handshake, and returns a live *agent.Agent. The
// caller (typically agentsession.AgentSession via the Spawner) must
// Close() the returned handle when done. The Starter is unchanged
// (reusable across many sessions).
//
// cfg.Workspace is the child process's cwd. cfg.Args are appended
// after the starter's defaults (user wins). cfg.Env is merged with
// the starter's defaults (cfg wins). cfg.SessionID, when non-empty,
// is forwarded to the ACP handshake, which reopens that session
// via session/resume (when advertised) or session/load.
func (s *Starter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	d, err := newDriver(ctx, s, cfg)
	if err != nil {
		return nil, err
	}
	return agent.NewAgent(s.Info(), d.PID(), d.events, d), nil
}

// RunOnce is the one-shot counterpart to Start. Spawns the acp
// server, runs the initialize + session/new handshake, sends the
// blocks over the protocol pipe, drains the turn's events, and
// closes the session before returning.
//
// The turn travels over the session transport, like every other
// one-shot turn in the codebase: the prompt goes over the pipe, so
// it is not bounded by the platform's argv limit.
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunSession(ctx, s, cfg, blocks, opts...)
}

// Review implements /review for the acp bridge: delegate to the
// shared agent.ReviewWithOcr (three-tier dispatch, docs/REVIEW.md
// §2). The acp-driven chat agent (used by opencode) reads the
// precomputed diff and outputs the structured review; ocr delegate
// rules fold in when ocr is on $PATH.
//
// acp is the generic ACP-protocol bridge; opencode is the only
// built-in that uses it. Review here serves both acp as a Starter
// and any future acp-backed agent that registers itself.
func (s *Starter) Review(ctx context.Context, cfg agent.StartConfig, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.ReviewDispatch(ctx, s, cfg, opts...)
}
