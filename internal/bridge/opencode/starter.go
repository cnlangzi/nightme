
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
// bridge), runs the initialize + session/new handshake, installs
// the opencode-specific sessionUpdate translator, and returns a
// live *agent.Agent.
//
// The runtime state (transport / rpc / events / driver) lives
// inside the generic acp bridge — this package only contributes:
//
//   - The sessionUpdate → AgentEvent translator (update.go),
//     installed via SetUpdateHandler after Start returns.
//   - Per-bridge session context fields (AgentName=opencode,
//     Workspace=cfg.Workspace) stamped on every event.
//
// cfg.SessionID, when non-empty, is reserved for v2 ACP
// session/load wiring. Today the bridge always opens a fresh
// session; resume via cfg.SessionID is implemented in codex /
// pi bridges already and will follow the same shape here in v2.
//
// Race note: SetUpdateHandler must be called BEFORE the readPump
// observes the first session/update. The race-free storage is
// the acp bridge's atomic.Pointer on d.updateHandler, but the
// acp readPump goroutine is started inside acpStarter.Start —
// so by the time SetUpdateHandler returns, early sessionUpdate
// notifications could already be in flight. For opencode's
// fresh-session path this is a no-op (no notifications arrive
// before the client's first session/prompt). It will matter
// once v2 wires session/load replay.
func (s *Starter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	if cfg.Workspace == "" {
		return nil, errors.New("opencode: workspace is required")
	}
	// The generic acp bridge handles spawn + JSON-RPC + PTY +
	// Stop / Reset / Permission. We inject the opencode-specific
	// sessionUpdate translator via SetUpdateHandler after Start
	// returns the live *agent.Agent.
	acpStarter := acp.NewStarter(s.name, s.command, s.args, nil, 0, 0)
	a, err := acpStarter.Start(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("agent %s: spawn: %w", s.Info().Name, err)
	}

	// Walk back to the *acp.driver through the public Driver()
	// accessor. This is a type assertion, but Driver() is the
	// documented bridge-extension point — see internal/agent
	// agent.go §Driver. The Driver() interface{} return type is
	// package-private by design; bridge-specific extensions
	// like this one consume it via SetUpdateHandler / View which
	// we expose just for this purpose.
	drv, ok := a.Driver().(*acp.DriverHandle)
	if !ok || drv == nil {
		// Defensive: should never happen because acp.NewStarter
		// always constructs *driver. If a future refactor
		// changes this, fall back to a no-op update handler.
		oLog("Start: could not access acp driver; sessionUpdate translator disabled",
			"driver_type", fmt.Sprintf("%T", a.Driver()))
		return a, nil
	}
	updater := newUpdateHandler(cfg.Workspace)
	drv.SetUpdateHandler(updater.asUpdateHandler())
	// Wire the per-turn flush hook the generic acp bridge invokes
	// right before EventAgentDone. Without this the trailing text
	// the agent produced after the last sentence-end stays in
	// textBuf until the turn-end drop — the user would see only
	// the partial reply with no Done / no result card. See
	// F-OPENCODE-ACP-MIGRATION §5.2 (drain-on-turn-end).
	drv.SetFlushHandler(updater.Flush)
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
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
	return runPrintMode(ctx, s, cfg, blocks)
}

// Review implements /review for opencode: delegate to shared
// StandardPrompt. opencode's chat agent (driven by the ACP bridge)
// reads git diff and outputs the structured review.
func (s *Starter) Review(ctx context.Context, rc agent.ReviewContext) (agent.RunResult, error) {
	return agent.Review(ctx, s, rc)
}
