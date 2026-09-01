// starter.go — the spawn recipe for the cursor ACP bridge.
//
// Cursor CLI natively supports ACP via `cursor-agent acp`.
// This package wraps the generic acp bridge, similar to opencode.
//
// Two spawn paths:
//
//   - Start (long-lived chat session) → `cursor-agent` +
//     DefaultACPArgs under PTY (parent full-access flags then
//     `acp`). Reuses the generic ACP bridge for protocol handling.
//     No sessionUpdate translator needed (unlike opencode) —
//     Cursor's sessionUpdate events are handled by the generic
//     acp bridge's fallback path.
//
//   - RunOnce (one-shot: /gtw commit, buildAgentPrompt) →
//     `cursor-agent` + FullAccessArgs + `-p "prompt"
//     --output-format text`. The process exits after the turn.
//
// The two paths share the same Starter; only RunOnce and the
// print-mode spawn in print.go differ from Start and the
// ACP-backed driver.
package cursor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/acp"
)

// Starter is the cursor spawn recipe. Held in agent.Builtins as
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

// NewStarter constructs the cursor spawn recipe. Entry point
// used at registration time (cmd/nightme/agents.go calls it
// from init()).
//
// args are the protocol flags. The bridge passes them as-is to
// the cursor binary; the canonical value is DefaultACPArgs
// (parent full-access flags then `acp` — see cursor.go).
// Flags MUST precede `acp`: the parent CLI owns --force /
// --sandbox / --trust / --approve-mcps.
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

// Detect verifies the cursor binary resolves on PATH. Called
// by Spawner before Start; an error aborts session creation with
// a clear "cursor not installed" message.
func (s *Starter) Detect() error {
	_, err := exec.LookPath(s.command)
	return err
}

// Start spawns `cursor-agent` + s.args under a PTY (via the
// generic acp bridge), runs the initialize + session/new
// handshake, and returns a live *agent.Agent.
//
// The runtime state (transport / rpc / events / driver) lives
// inside the generic acp bridge — this package only contributes:
//   - Per-bridge session context fields (AgentName=cursor,
//     Workspace=cfg.Workspace) stamped on every event.
//
// Unlike opencode, cursor does NOT need a sessionUpdate
// translator. Cursor's ACP server emits standard sessionUpdate
// events that the generic acp bridge handles via its fallback
// path (text/tool events). If future Cursor CLI versions emit
// custom sessionUpdate variants, a translator can be added
// following the opencode/update.go pattern.
//
// cfg.SessionID, when non-empty, is reserved for future ACP
// session/load wiring. Today the bridge always opens a fresh
// session.
func (s *Starter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	if cfg.Workspace == "" {
		return nil, errors.New("cursor: workspace is required")
	}
	acpStarter := acp.NewStarter(s.name, s.command, s.args, nil, 0, 0)
	a, err := acpStarter.Start(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("agent %s: spawn: %w", s.Info().Name, err)
	}
	// Install Cursor-specific extension method handler. Intercepts
	// cursor/update_todos, cursor/create_plan, cursor/task,
	// cursor/ask_question, cursor/generate_image and maps them to
	// the corresponding AgentEvent types.
	drv, ok := a.Driver().(*acp.DriverHandle)
	if !ok {
		cLog("Start: driver type assert failed", "type", fmt.Sprintf("%T", a.Driver()))
		return a, nil
	}
	drv.SetMethodHandler(NewMethodHandler(drv.View()))
	return a, nil
}

// RunOnce is the one-shot counterpart to Start. Spawns
// `cursor-agent` + FullAccessArgs + `-p "prompt" --output-format
// text` and returns the agent's final text. No chat session, no
// events channel — the process exits after the turn.
//
// Cursor's print-mode uses `cursor-agent -p` (not ACP), which is
// simpler and faster for one-shot invocations (/gtw commit,
// buildAgentPrompt). Mirrors the codex/claudecode/pi/opencode
// print-mode pattern: bypass the long-lived ACP bridge driver,
// spawn a fresh process, capture stdout, reap on exit.
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	return runPrintMode(ctx, s, cfg, blocks, opts...)
}

// Review implements /review for the cursor bridge via the existing
// delegateReviewMultiJob fan-out machinery (docs/REVIEW.md §2.5).
//
// Groups:
//   1. nativeReviewGroup("/review-bugbot") — cursor-agent's built-in
//      Bugbot subagent. Verified empirically 2026-09-01 against
//      cursor-agent 2026.08.11: cursor-agent auto-loads
//      ~/.cursor/skills-cursor/review-bugbot/SKILL.md and dispatches
//      the bugbot subagent when "/review-bugbot" appears as the
//      positional -p prompt. The bridge's RunOnce spawns cursor-agent
//      with -p "/review-bugbot" --output-format text and Bugbot runs.
//   2. simplifyGroup(pre.reviewable) — nightme-owned simplify lens
//      (reuse / simplification / efficiency / altitude axes).
//      Bugbot does NOT cover these axes (verified by reading the
//      SKILL.md and confirming cursor-agent CLI has no /simplify
//      skill — see docs/REVIEW.md §2.1.1). We run them in parallel
//      to fill the gap and merge the results.
//
// Why the mixed pattern (vs codex/claudecode single-call native):
// those bridges' native review already covers the full review
// surface (severity grouping / multi-agent pipeline), so a
// single native call is enough. Bugbot is missing the simplify
// axes, hence the second goroutine.
//
// Why we go through ReviewWithMixed (vs writing our own fan-out):
// delegateReviewMultiJob already handles parallel goroutines,
// eventAggregator (3-phase state machine), per-job ToolStart/End
// pairing, cross-job Task dedup, and mergeRunResults. Reusing it
// keeps the cursor path symmetric with codex/claudecode's
// ReviewWithNative path AND with Tier 2/3's ReviewWithPrompt path.
//
// See agent.ReviewWithMixed (internal/agent/review.go) for the
// generic helper; this method is a one-liner wiring the cursor
// bridge's slash command into it.
func (s *Starter) Review(ctx context.Context, cfg agent.StartConfig, opts ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.ReviewWithMixed(ctx, s, cfg, []string{"/review-bugbot"}, opts...)
}
