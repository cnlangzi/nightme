
// Package opencode_acp is the nightme bridge for the opencode CLI.
//
// Unlike the other per-CLI bridges (claudecode / codex / pi), this
// one intentionally ships TWO spawn paths and lets the caller pick:
//
//   - Start (long-lived chat session) → opencode acp over PTY.
//     Vendor-recommended integration mode
//     (https://opencode.ai/docs/acp/ — "All features are supported").
//     The actual JSON-RPC 2.0 / PTY / session lifecycle work lives in
//     internal/bridge/acp; this package only supplies the
//     bridge-specific sessionUpdate translator
//     (internal/bridge/opencode_acp/update.go).
//
//   - RunOnce (one-shot: /gtw commit, buildAgentPrompt) →
//     `opencode run --format json <prompt>`. Mirrors
//     codex/claudecode/pi print-mode; one process per call.
//
// Why ACP, not HTTP serve
//
// An earlier version of this package implemented the long-lived
// path against `opencode serve` (HTTP + SSE + ~3500 lines of
// hand-written event translation). That implementation was
// retired for F-OPENCODE-ACP-MIGRATION. The reasons, in one
// paragraph: opencode's own `acp` subcommand runs the SAME HTTP
// server underneath and adds a JSON-RPC 2.0 stdio adapter on top,
// so switching to ACP does not lose any capability — but it
// removes the need to maintain ~1300 lines of opencode-private
// SSE event translation, ~400 lines of stream-buffer bookkeeping,
// ~300 lines of HTTP serve plumbing, and 9 endpoint-specific
// tests. The same generic acp bridge used by codex / pi / future
// ACP backends now serves opencode, with a thin per-bridge
// UpdateHandler that translates the 5 sessionUpdate variants
// opencode actually emits on the wire (user_message_chunk,
// agent_message_chunk, agent_thought_chunk, tool_call,
// tool_call_update — see update.go for the full routing table).
//
// Historical `opencode serve` artifacts
//
// The retired `internal/bridge/opencode/` package was deleted in
// Phase 2 of the migration (commit 45e7b21). References to
// `opencode.NewStarter` must use `opencode_acp.NewStarter`
// instead. See docs/feat/F-OPENCODE-ACP-MIGRATION.md §6 for the
// phased removal plan (all three phases are now complete).
package opencode_acp

import (
	"log/slog"
	"os"
	"strings"
)

// ─── env / debug ───

// bridgeName is the AgentName stamped on every AgentEvent. Stable
// across sessions; consumed by the runtime's translator to
// attribute events to the opencode bridge (multi-bridge chat
// sessions use this to route the correct renderer / slash command
// set).
const bridgeName = "opencode"

// ─── debug ───

// opencodeDebug toggles the bridge's detailed debug logging.
// Default ON so a "why is opencode stuck" incident produces a
// usable breadcrumb trail. Silence with NIGHTME_OPENCODE_DEBUG=0
// (also accepts "false", "no", "off", case-folded).
var opencodeDebug = opencodeDebugEnabled()

func opencodeDebugEnabled() bool {
	v := os.Getenv("NIGHTME_OPENCODE_DEBUG")
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "1", "true", "yes", "on":
		return true
	}
	return false
}

// ─── logging ───

// oLog emits an info-level message tagged [opencode] (component=
// "opencode") when debug is enabled. Mirrors the codex / pi /
// claudecode cLog pattern so log scrapers see a consistent
// component label across all bridges. Tests in this package may
// swap slog.Default via slog.SetDefault() to keep test output
// clean.
func oLog(msg string, args ...any) {
	if !opencodeDebug {
		return
	}
	all := make([]any, 0, len(args)+2)
	all = append(all, "component", "opencode")
	all = append(all, args...)
	slog.Default().Info("[opencode] "+msg, all...)
}
