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
//   - One-shot invocations (/gtw commit, buildAgentPrompt)
//     spawn `copilot --allow-all-tools -p "prompt"` directly.
//     The print-mode path reuses the cursor / opencode / codex
//     / claudecode / pi print-mode shape — single stdout
//     capture, process exits after the turn.
//
// The two paths share the same Starter; only RunOnce and the
// print-mode spawn in print.go differ from Start and the
// ACP-backed driver.
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
	"os/exec"

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
	name    string
	command string
	args    []string
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
		name:    name,
		command: command,
		args:    append([]string(nil), args...),
	}
}

// Info returns the fixed metadata for this starter. Observable
// at any time; used by `nightme agents` and any other spec-only
// consumer.
func (s *Starter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeACP, s.command, s.args, nil)
}

// Detect verifies the copilot binary resolves on PATH. Called
// by Spawner before Start; an error aborts session creation
// with a clear "copilot not installed" message.
func (s *Starter) Detect() error {
	_, err := exec.LookPath(s.command)
	return err
}

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
// cfg.SessionID, when non-empty, is reserved for future ACP
// session/load wiring. Today the bridge always opens a fresh
// session.
func (s *Starter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	if cfg.Workspace == "" {
		return nil, errors.New("copilot: workspace is required")
	}
	acpStarter := acp.NewStarter(s.name, s.command, s.args, nil, 0, 0)
	a, err := acpStarter.Start(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("agent %s: spawn: %w", s.Info().Name, err)
	}
	return a, nil
}

// RunOnce is the one-shot counterpart to Start. Spawns
// `copilot` + FullAccessArgs + `-p "prompt"` and returns the
// agent's final text. No chat session, no events channel, no
// busy guard — the process exits after the turn.
//
// Mirrors the F-CODEX-PRINT-001 / F-CLAUDE-PRINT-001 /
// F-PI-PRINT-001 / F-OPENCODE-PRINT-001 rationale: one-shot
// callers (/gtw commit, buildAgentPrompt) don't need the long-
// lived ACP handshake (~1s server boot + initialize +
// session/new round-trip) when `-p` already gives them a turn
// and exits cleanly. Copilot's `-p` mode is plain-text stdout
// (no NDJSON), so the print-mode path is simpler than
// opencode's NDJSON-parsing print-mode.
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	return runPrintMode(ctx, s, cfg, blocks, opts...)
}

// Review implements /review for the copilot bridge: delegate
// to the shared agent.ReviewWithOcr (three-tier dispatch,
// docs/REVIEW.md §2). Copilot CLI has no native review
// subcommand, so /review runs the Go-precompute-enhanced
// prompt via print-mode one-shot — ocr delegate rules fold
// in when ocr is on $PATH.
func (s *Starter) Review(ctx context.Context, cfg agent.StartConfig, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	if agent.OcrAvailable() {
		return agent.ReviewWithOcr(ctx, s, cfg, opts...)
	}
	return agent.ReviewWithPrompt(ctx, s, cfg, opts...)
}