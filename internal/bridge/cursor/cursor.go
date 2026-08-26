// Package cursor is the nightme bridge for the Cursor CLI.
//
// Cursor CLI natively supports ACP via `cursor-agent acp`.
// This package wraps the generic acp bridge, similar to opencode.
//
// Two spawn paths:
//
//   - Start (long-lived chat session) →
//     `cursor-agent --force --trust --sandbox disabled --approve-mcps acp`
//     under PTY. Reuses the generic ACP bridge for protocol handling.
//     No sessionUpdate translator needed (unlike opencode) —
//     Cursor's sessionUpdate events are handled by the generic
//     acp bridge's fallback path.
//
//   - RunOnce (one-shot: /gtw commit, buildAgentPrompt) →
//     `cursor-agent --force --trust --sandbox disabled --approve-mcps
//     -p "prompt" --output-format text`. The process exits after
//     the turn.
//
// Permission defaults match the other ChatOps bridges (Claude
// `--permission-mode bypassPermissions`, Codex
// `approval_policy=never` + `sandbox_mode=danger-full-access`,
// dsh `DSH_PERMISSION_MODE=danger-full-access`): nightme does
// not rewrite `~/.cursor/` — it only injects spawn-time flags
// so the IM session can act without a local approval prompt.
package cursor

import (
	"log/slog"
	"os"
	"strings"
)

// FullAccessArgs are parent-level cursor-agent flags that default
// the ChatOps session to acting. They MUST precede the subcommand:
// `cursor-agent acp --force` is parsed by the acp subcommand (which
// has no such flag) and starts the ACP server anyway;
// `cursor-agent --force acp` is the form the parent CLI accepts
// (verified against cursor-agent 2026.08.11-e8db854).
//
//	--force            Force allow commands unless explicitly denied
//	                   (`--yolo` is an alias).
//	--trust            Trust the workspace without prompting.
//	--sandbox disabled Disable the FS / network sandbox.
//	--approve-mcps     Auto-approve MCP servers.
var FullAccessArgs = []string{
	"--force",
	"--trust",
	"--sandbox", "disabled",
	"--approve-mcps",
}

// DefaultACPArgs is the argv nightme registers for the cursor
// builtin: parent full-access flags, then the `acp` subcommand.
var DefaultACPArgs = withFullAccess("acp")

// withFullAccess prepends FullAccessArgs to rest. Used by both
// the ACP spawn (rest = "acp") and print-mode (rest = -p …).
func withFullAccess(rest ...string) []string {
	out := make([]string, 0, len(FullAccessArgs)+len(rest))
	out = append(out, FullAccessArgs...)
	return append(out, rest...)
}

// bridgeName is the AgentName stamped on every AgentEvent.
const bridgeName = "cursor"

// cursorDebug toggles the bridge's detailed debug logging.
// Default ON so a "why is cursor stuck" incident produces a
// usable breadcrumb trail. Silence with NIGHTME_CURSOR_DEBUG=0
// (also accepts "false", "no", "off", case-folded).
var cursorDebug = cursorDebugEnabled()

func cursorDebugEnabled() bool {
	v := os.Getenv("NIGHTME_CURSOR_DEBUG")
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "1", "true", "yes", "on":
		return true
	}
	return false
}

// cLog emits an info-level message tagged [cursor] (component=
// "cursor") when debug is enabled. Mirrors the opencode / codex /
// pi / claudecode cLog pattern so log scrapers see a consistent
// component label across all bridges.
func cLog(msg string, args ...any) {
	if !cursorDebug {
		return
	}
	all := make([]any, 0, len(args)+2)
	all = append(all, "component", "cursor")
	all = append(all, args...)
	slog.Default().Info("[cursor] "+msg, all...)
}
