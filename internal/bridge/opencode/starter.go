// Package opencode — Starter (spawn recipe) for the opencode
// ACP bridge.
//
// Model choice (F-OPENCODE-ACP-MIGRATION §2):
//
//   - Long-lived chat sessions spawn `opencode acp` under a PTY
//     and drive the standard ACP JSON-RPC 2.0 wire (initialize →
//     session/new → session/prompt → ... → session/cancel).
//     One opencode process per chat session; many turns over
//     its lifetime.
//
//   - One-shot invocations (/gtw commit, /gtw pr) run over the
//     same ACP transport. The prompt travels over the protocol
//     pipe, so it is not bounded by the platform's argv limit.
//
// The two paths share the same Starter; only `RunOnce` differs
// from `Start`: it opens a fresh session and closes it again
// when the turn ends.
package opencode

import (
	"context"
	"errors"
	"fmt"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/acp"
	"github.com/cnlangzi/nightme/internal/bridge/telemetry"
)

// telemetryEnv disables opencode's client telemetry for the child the
// bridge spawns. DO_NOT_TRACK is the standard do-not-track switch.
//
// Applied unless the user opts out of nightme's telemetry policy —
// see internal/bridge/telemetry.
var telemetryEnv = []string{"DO_NOT_TRACK=1"}

// childEnv returns the environment entries the bridge adds to every
// child it spawns: the telemetry knob, unless the user allowed agent
// telemetry.
func childEnv() []string {
	if telemetry.Allowed() {
		return nil
	}
	return append([]string(nil), telemetryEnv...)
}

// Starter is the opencode spawn recipe. Held in agent.Builtins as
// a singleton per agent name.
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

// NewStarter constructs the opencode spawn recipe. Entry point
// used at registration time (cmd/nightme/agents.go calls it
// from init()).
//
// args are the protocol flags. The bridge passes them as-is to
// the opencode binary; the canonical value is `[]string{"acp"}`
// (matches every supported editor's documented integration:
// https://opencode.ai/docs/acp/).
//
// The defensive copy means later mutation of the caller's slice
// does not affect us.
func NewStarter(name, command string, args []string) *Starter {
	return &Starter{
		name:           name,
		command:        command,
		defaultCommand: command,
		args:           append([]string(nil), args...),
	}
}

// Info returns the fixed metadata for this starter. Observable at
// any time; used by `nightme agents` and any other spec-only
// consumer.
//
// Env carries the telemetry knob the bridge adds to the child, unless
// the user opted out of nightme's telemetry policy — the advertised
// env therefore matches what the child actually receives.
func (s *Starter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeACP, s.command, s.args, childEnv())
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

// Start spawns `opencode acp` under a PTY (via the generic acp
// bridge), runs the initialize + session/new handshake, and returns
// a live *agent.Agent.
//
// The runtime state (transport / rpc / events / driver) lives
// inside the generic acp bridge. opencode emits no vendor-private
// sessionUpdate kinds or JSON-RPC methods that the generic acp
// fallback does not already handle, so no per-bridge
// UpdateHandler is installed here — the built-in text buffering
// (agent_message_chunk / agent_thought_chunk / usage_update /
// session.status / etc.) all lives in the generic bridge.
// If a future opencode version adds PRIVATE protocol extensions,
// a thin per-bridge UpdateHandler can be installed via
// (*driver).SetUpdateHandler(...) — see docs/bridge/acp.md §2.3.
//
// cfg.SessionID, when non-empty, is forwarded to the generic ACP
// bridge, which reopens that session via session/resume or
// session/load.
//
// The session runs on the model opencode's own configuration selects:
// the bridge reads that model (see model.go) and applies it to the
// session before the first prompt. The model is the user's — the
// bridge forwards it, it never picks one.
func (s *Starter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	if cfg.Workspace == "" {
		return nil, errors.New("opencode: workspace is required")
	}
	// The generic acp bridge handles spawn + JSON-RPC + PTY +
	// Stop / Reset / Permission. The built-in text buffering in
	// the ACP bridge handles agent_message_chunk /
	// agent_thought_chunk accumulation and sentence-level flushing.
	// The full sessionUpdate surface (agent_message_chunk /
	// tool_call / tool_call_update / usage_update / session.status
	// / session_info_update / ...) is recognised by the generic
	// fallback — no per-bridge translator needed.
	acpStarter := s.acpStarter(ctx, cfg)
	a, err := acpStarter.Start(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("agent %s: spawn: %w", s.Info().Name, err)
	}
	return a, nil
}

// acpStarter builds the generic ACP starter this bridge drives. The
// starter's env carries the telemetry knob, so the spawned child
// receives it.
func (s *Starter) acpStarter(ctx context.Context, cfg agent.StartConfig) *acp.Starter {
	return acp.NewStarter(s.name, s.command, s.args, childEnv(), 0, 0,
		acp.WithSessionIDHook(s.applyConfiguredModel(ctx, cfg.Workspace)))
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

// Review implements /review for opencode via the shared
// agent.ReviewDispatch. opencode's chat agent (driven by the ACP
// bridge) reads the precomputed diff and outputs the structured
// review; ocr delegate rules fold in when ocr is on $PATH.
func (s *Starter) Review(ctx context.Context, cfg agent.StartConfig, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.ReviewDispatch(ctx, s, cfg, opts...)
}
