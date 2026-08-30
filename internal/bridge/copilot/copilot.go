// Package copilot is the nightme bridge for the GitHub Copilot CLI.
//
// Copilot CLI natively supports ACP via `copilot --acp --stdio`
// (NDJSON over stdio, same wire surface as opencode / cursor).
// Requires Copilot CLI >= 1.0.x — older preview builds (e.g.
// 0.0.361) reject `--acp` with "unknown option" and must be
// upgraded via `npm install -g @github/copilot@latest`.
//
// Two spawn paths (mirroring cursor / opencode):
//
//   - Start (long-lived chat session) → `copilot --allow-all-tools
//     --acp --stdio` under PTY. Reuses the generic ACP bridge.
//     No sessionUpdate / MethodHandler needed — Copilot's wire
//     surface is fully ACP-spec conformant (per
//     docs.github.com/en/copilot/reference/acp-server); the
//     generic fallback covers all standard sessionUpdate kinds.
//
//   - RunOnce (one-shot: /gtw commit, buildAgentPrompt) →
//     `copilot --allow-all-tools -p "prompt" -s`. The process
//     outputs the agent's final response to stdout and exits —
//     no JSON events, no multi-turn, no events channel. `-s`
//     (`--silent`) suppresses the post-answer stats decoration
//     so stdout is just the final text (verified on 1.0.81).
//
// Permission defaults match the other ChatOps bridges (Claude
// --permission-mode bypassPermissions, Codex approvalPolicy=
// never + sandboxMode=danger-full-access, cursor --force
// --trust --sandbox disabled, dsh DSH_PERMISSION_MODE=danger-
// full-access): nightme does NOT rewrite ~/.copilot/ — it only
// injects spawn-time `--allow-all-tools` so the IM session can
// act without per-tool approval prompts. `--allow-all-tools`
// is the canonical long form Copilot exposes (verified against
// `copilot --help`; the older `--yolo` alias is NOT a flag in
// the actual binary).
package copilot

import (
	"log/slog"
	"os"
	"strings"
)

// FullAccessArgs is the spawn-time permission flag that turns
// off all per-tool approval prompts. Prepended to both the ACP
// start path and the print-mode RunOnce path.
//
// Per docs/bridge/acp.md §1.1 + agent-no-config-tampering
// principle: this is the ONLY behavior-modifying flag the
// bridge injects. Model, provider, MCP servers, and tool
// allow-lists flow from the user's ~/.copilot/ settings.
//
// Verified against `copilot --help` on v1.0.81 (the currently
// installed GA build on 2026-08-29) and per
// docs.github.com/en/copilot/concepts/agents/copilot-cli/
// autopilot. Older `--yolo` alias mentioned in third-party
// docs is NOT a flag in the actual binary — `--allow-all-
// tools` is the canonical long form.
var FullAccessArgs = []string{"--allow-all-tools"}

// DefaultACPArgs is the argv nightme registers for the
// copilot builtin: `--allow-all-tools` first (parent-level
// permission flag), then `--acp --stdio` (ACP server transport).
//
// The order does not matter for Copilot (all three are top-level
// flags, not subcommand-prefixed), but we put permission before
// transport for symmetry with cursor / opencode.
var DefaultACPArgs = append(
	append([]string(nil), FullAccessArgs...),
	"--acp", "--stdio",
)

// bridgeName is the AgentName stamped on every AgentEvent.
// Stable across the package; consumed by the runtime's
// translator to attribute events to the copilot bridge.
const bridgeName = "copilot"

// copilotDebug toggles the bridge's detailed debug logging.
// Default ON so a "why is copilot stuck" incident produces a
// usable breadcrumb trail. Silence with NIGHTME_COPILOT_DEBUG=0
// (also accepts "false", "no", "off", case-folded).
//
// Mirrors the cursor / opencode / codex / pi cLog pattern so
// log scrapers see a consistent component label across all
// bridges.
var copilotDebug = copilotDebugEnabled()

func copilotDebugEnabled() bool {
	v := os.Getenv("NIGHTME_COPILOT_DEBUG")
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "1", "true", "yes", "on":
		return true
	}
	return false
}

// cLog emits an info-level message tagged [copilot] (component=
// "copilot") when debug is enabled. Mirrors the cursor / opencode
// / codex / pi / claudecode cLog pattern.
func cLog(msg string, args ...any) {
	if !copilotDebug {
		return
	}
	all := make([]any, 0, len(args)+2)
	all = append(all, "component", "copilot")
	all = append(all, args...)
	slog.Default().Info("[copilot] "+msg, all...)
}