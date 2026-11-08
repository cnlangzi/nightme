// starter.go — the spawn recipe for the copilot ACP bridge.
//
// Model choice:
//
//   - Long-lived chat sessions spawn `copilot --allow-all-tools
//     --acp --stdio` under a PTY and drive the standard ACP
//     JSON-RPC 2.0 wire (initialize → session/new →
//     session/prompt → ... → session/cancel). One copilot
//     process per chat session; many turns over its lifetime.
//     Requires Copilot CLI >= 1.0.x (older preview builds
//     reject `--acp`).
//
//   - One-shot invocations (/gtw commit, /gtw pr) run over the
//     same ACP transport. The prompt travels over the protocol
//     pipe, so it is not bounded by the platform's argv limit.
//
// The two paths share the same Starter; only RunOnce differs from
// Start: it opens a fresh session and closes it again when the
// turn ends.
//
// No per-bridge UpdateHandler / MethodHandler is installed:
// Copilot's ACP server emits standard sessionUpdate events
// (handled by the generic acp bridge fallback per
// docs/bridge/acp.md §1.1) and no documented vendor-private
// JSON-RPC methods. If a future Copilot release adds PRIVATE
// protocol extensions (copilot/* methods), a thin per-bridge
// MethodHandler can be installed following the cursor/
// handler.go pattern.
package copilot

import (
	"context"
	"errors"
	"fmt"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/acp"
)

// Starter is the copilot spawn recipe. Held in agent.Builtins
// as a singleton per agent name.
//
// Mode is ModeACP: the chat-session runtime needs to know the
// bridge speaks Agent Client Protocol so it can apply the
// right per-mode behavior (timeout settings, event queue
// routing, /stop propagation).
type Starter struct {
	name           string
	command        string
	defaultCommand string
	args           []string
}

// NewStarter constructs the copilot spawn recipe. Entry point
// used at registration time (cmd/nightme/agents.go calls it
// from init()).
//
// args are the protocol flags. The bridge passes them as-is to
// the copilot binary; the canonical value is DefaultACPArgs
// (FullAccessArgs + "--acp --stdio"). The defensive copy means
// later mutation of the caller's slice does not affect us.
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
// Args carry the telemetry knob the bridge adds to the child, unless
// the user opted out of nightme's telemetry policy — the advertised
// argv therefore matches what the child actually receives.
func (s *Starter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeACP, s.command, childArgs(s.args, s.command), nil)
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

// Start spawns `copilot` + s.args under a PTY (via the generic
// acp bridge), runs the initialize + session/new handshake,
// and returns a live *agent.Agent.
//
// The runtime state (transport / rpc / events / driver) lives
// inside the generic acp bridge — this package only supplies:
//   - Per-bridge session context fields (AgentName=copilot,
//     Workspace=cfg.Workspace) stamped on every event.
//
// Copilot emits no vendor-private sessionUpdate kinds or
// JSON-RPC methods that the generic acp fallback does not
// already handle, so no per-bridge translator is installed.
// The built-in text buffering (agent_message_chunk /
// agent_thought_chunk / usage_update / session.status / etc.)
// all lives in the generic bridge.
//
// cfg.SessionID, when non-empty, is forwarded to the generic ACP
// bridge, which reopens that session via session/resume or
// session/load.
func (s *Starter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	if cfg.Workspace == "" {
		return nil, errors.New("copilot: workspace is required")
	}
	acpStarter := s.acpStarter()
	a, err := acpStarter.Start(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("agent %s: spawn: %w", s.Info().Name, err)
	}
	return a, nil
}

// acpStarter builds the generic ACP starter this bridge drives. The
// starter's argv carries the telemetry knob, so the spawned child
// receives it.
func (s *Starter) acpStarter() *acp.Starter {
	return acp.NewStarter(s.name, s.command, childArgs(s.args, s.command), nil, 0, 0)
}

// RunOnce is the one-shot counterpart to Start. The turn runs over
// the session transport — Start → SendBlocks → drain → Close — so
// the prompt travels over the ACP pipe instead of argv. A prompt
// larger than the platform's argv limit (131072 bytes per string on
// Linux, 32767 characters per command line on Windows) therefore
// still reaches the agent.
//
// The turn always opens a fresh session, so it never splices itself
// into the caller's conversation.
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunSession(ctx, s, cfg, blocks, opts...)
}

// Review implements /review for the copilot bridge: delegate
// to the shared agent.ReviewWithOcr (three-tier dispatch,
// docs/REVIEW.md §2). Copilot CLI has no native review
// subcommand, so /review runs the Go-precompute-enhanced
// prompt on the agent's session path — ocr delegate rules
// fold in when ocr is on $PATH.
func (s *Starter) Review(ctx context.Context, cfg agent.StartConfig, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.ReviewDispatch(ctx, s, cfg, opts...)
}
