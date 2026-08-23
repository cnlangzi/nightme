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
//   - One-shot invocations (/gtw commit, buildAgentPrompt,
//     nightly CI smoke tests) spawn `opencode run --format json
//     <prompt>` directly. The print-mode path reuses the
//     codex / claudecode / pi print-mode shape — single
//     NDJSON stream, single result event, process exits.
//
// The two paths share the same Starter; only `RunOnce` and the
// print-mode spawn in print.go differ from `Start` and the
// ACP-backed driver.
package opencode

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/acp"
)

// Starter is the opencode spawn recipe. Held in agent.Builtins as
// a singleton per agent name.
//
// Mode is ModeACP: the chat-session runtime needs to know the
// bridge speaks Agent Client Protocol so it can apply the
// right per-mode behavior (timeout settings, event queue
// routing, /stop propagation).
type Starter struct {
	name    string
	command string
	args    []string
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
		name:    name,
		command: command,
		args:    append([]string(nil), args...),
	}
}

// Info returns the fixed metadata for this starter. Observable at
// any time; used by `nightme agents` and any other spec-only
// consumer.
func (s *Starter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeACP, s.command, s.args, nil)
}

// Detect verifies the opencode binary resolves on PATH. Called
// by Spawner before Start; an error aborts session creation with
// a clear "opencode not installed" message.
func (s *Starter) Detect() error {
	_, err := exec.LookPath(s.command)
	return err
}

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
// cfg.SessionID, when non-empty, is reserved for v2 ACP
// session/load wiring. Today the bridge always opens a fresh
// session; resume via cfg.SessionID is implemented in codex /
// pi bridges already and will follow the same shape here in v2.
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
	acpStarter := acp.NewStarter(s.name, s.command, s.args, nil, 0, 0)
	a, err := acpStarter.Start(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("agent %s: spawn: %w", s.Info().Name, err)
	}
	return a, nil
}

// RunOnce is the one-shot counterpart to Start. Spawns
// `opencode run --format json <prompt>` directly and returns the
// agent's final text. No chat session, no events channel, no
// busy guard — the process exits after the turn.
//
// Mirrors the F-CODEX-PRINT-001 / F-CLAUDE-PRINT-001 /
// F-PI-PRINT-001 rationale: one-shot callers (/gtw commit,
// buildAgentPrompt) do not need the long-lived ACP handshake
// (~1s server boot + initialize + session/new round-trip) when
// `opencode run` already gives them a turn and exits cleanly.
//
// The print-mode spawn lives in print.go (verbatim copy of the
// retired opencode print.go, with cosmetic cleanups — the wire
// is `opencode run --format json` and is not affected by the
// ACP migration).
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	return runPrintMode(ctx, s, cfg, blocks, opts...)
}

// Review implements /review for opencode: delegate to the shared
// agent.DelegateReview (three-tier dispatch, docs/REVIEW.md §2).
// opencode's chat agent (driven by the ACP bridge) reads the
// precomputed diff and outputs the structured review; ocr delegate
// rules fold in when ocr is on $PATH.
func (s *Starter) Review(ctx context.Context, cfg agent.StartConfig, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.DelegateReview(ctx, s, cfg, opts...)
}
