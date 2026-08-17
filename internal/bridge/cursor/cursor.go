// Package cursor is the nightme bridge for the Cursor CLI.
//
// Cursor CLI natively supports ACP via `agent acp` command.
// This package wraps the generic acp bridge, similar to opencode.
//
// Two spawn paths:
//
//   - Start (long-lived chat session) → `agent acp` over PTY.
//     Reuses the generic ACP bridge for protocol handling.
//     No sessionUpdate translator needed (unlike opencode) —
//     Cursor's sessionUpdate events are handled by the generic
//     acp bridge's fallback path.
//
//   - RunOnce (one-shot: /gtw commit, buildAgentPrompt) →
//     `agent -p "prompt" --output-format text`. The process
//     exits after the turn.
package cursor

import (
	"log/slog"
	"os"
	"strings"
)

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
