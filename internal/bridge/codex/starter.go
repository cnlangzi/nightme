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

	"github.com/cnlangzi/nightme/internal/agent"
)

// Starter is the codex spawn recipe. Held in agent.Builtins as a
// singleton per agent name.
type Starter struct {
	name           string
	command        string
	defaultCommand string
	args           []string
}

// NewStarter constructs the codex spawn recipe. Entry point used
// at registration time (cmd/nightme/agents.go calls it from init()).
func NewStarter(name, command string, args []string) *Starter {
	return &Starter{
		name:           name,
		command:        command,
		defaultCommand: command,
		args:           append([]string(nil), args...),
	}
}

// Info returns the fixed metadata for this starter. Observable
// at any time; used by `nightme agents` and any other spec-only
// consumer.
//
// Args are the registered args. Unlike the other bridges, codex
// builds its app-server argv at spawn time (see appServerArgs), so
// the telemetry knob does not show up here — it is applied to the
// child either way.
func (s *Starter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeJSONIO, s.command, s.args, nil)
}

// Detect verifies the configured command resolves to an invokable
// binary (absolute path or PATH-relative name). Called by Spawner
// before Start; an error aborts session creation.
func (s *Starter) Detect() error {
	return agent.ResolveCommand(s.command)
}

// Init overrides the executable path used by Detect and
// Info. cfg.Agents path overrides flow through
// agentregistry.Build. Passing "" resets to the default
// baked in by NewStarter — used by Build to drop stale
// overrides when cfg.Agents no longer names this agent.
//
// The mutation hits the singleton held in agent.Builtins;
// tests that exercise cfg.Agents overrides should snapshot
// and restore via Command().
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

// RunOnce is the one-shot counterpart to Start. The turn runs over
// the session transport — Start → SendBlocks → drain → Close — so
// the prompt travels over the bridge's protocol pipe instead of
// argv. A prompt larger than the platform's argv limit (131072
// bytes per string on Linux, 32767 characters per command line on
// Windows) therefore still reaches the agent.
//
// The turn always opens a fresh session, so it never splices itself
// into the caller's conversation.
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunSession(ctx, s, cfg, blocks, opts...)
}

// Review implements /review for codex via the shared
// agent.ReviewDispatch — same uniform path every other bridge
// uses (codex no longer invokes `codex review --base ...`
// directly; the review prompt reaches codex over the session path
// and codex outputs the structured review).
//
// If the user wants codex's built-in `codex review` subcommand
// (review-specific rubric + structured output), they send the
// `/codex-review` slash command directly in chat — the chat
// session's long-lived Start path forwards it to codex the same
// way it forwards any prompt, and codex dispatches the
// subcommand itself.
func (s *Starter) Review(ctx context.Context, cfg agent.StartConfig, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.ReviewDispatch(ctx, s, cfg, opts...)
}
