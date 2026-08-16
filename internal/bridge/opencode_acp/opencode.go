
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
// hand-written event translation). That implementation was retired
// for F-OPENCODE-ACP-MIGRATION. The reasons, in one paragraph:
// opencode's own `acp` subcommand runs the SAME HTTP server
// underneath and adds a JSON-RPC 2.0 stdio adapter on top, so
// switching to ACP does not lose any capability — but it removes
// the need to maintain ~1300 lines of opencode-private SSE event
// translation, ~400 lines of stream-buffer bookkeeping, ~300 lines
// of HTTP serve plumbing, and 9 endpoint-specific tests. The same
// generic acp bridge used by codex / pi / future ACP backends now
// serves opencode, with a thin per-bridge UpdateHandler that
// translates sessionUpdate variants the generic fallback does not
// know about (usage_update, available_commands_update,
// current_mode_update, plan).
//
// Historical `opencode serve` artifacts
//
// The retired `internal/bridge/opencode/` package is deleted in
// Phase 2 of the migration. References to `opencode.NewStarter`
// must use `opencode_acp.NewStarter` instead. See
// docs/feat/F-OPENCODE-ACP-MIGRATION.md §6 for the phased
// removal plan.
package opencode_acp

import (
	"errors"
	"log/slog"
	"os"
	"strings"
	"time"
)

// ─── errors ───

// ErrSessionClosed is returned by Start / Send* calls after the
// underlying opencode acp process has exited (PTY EOF). Mirrors
// codex / pi / claudecode / acp so the runtime can distinguish
// "send-on-dead-session" from a real wire error.
var ErrSessionClosed = errors.New("opencode: session closed")

// ErrNoPendingPermission is returned by SendPermission when there
// is no outstanding session/request_permission call to answer.
// Matches the convention used by codex / pi / claudecode.
var ErrNoPendingPermission = errors.New("opencode: no pending permission answer")

// ErrImageTooLarge is returned by SendBlocks when a single image
// exceeds maxImageBytes. Matches codex / pi limits so the runtime
// can surface a uniform "attachment too large" error.
var ErrImageTooLarge = errors.New("opencode: image too large")

// ErrTurnBusy is returned by SendBlocks when a previous turn is
// still in flight. Mirrors codex's ErrTurnBusy semantics: the
// caller may retry once the bridge's readpump observes
// session.status:idle (translated to EventAgentDone by the
// UpdateHandler in update.go).
var ErrTurnBusy = errors.New("opencode: previous turn still active")

// ─── timing ───

// permissionTimeout is how long an EventAgentPermission waits
// for a user decision before defaulting to reject. Mirrors
// codex's 5-minute default.
const permissionTimeout = 5 * time.Minute

// turnWatchdogTimeout bounds the wall time between consecutive
// sessionUpdate events during a turn. Resets on every event
// delivered by the UpdateHandler (model is alive, plugin loaded,
// etc.). On timeout the bridge synthesizes an EventAgentError so
// the runtime readpump clears the busy guard and the chat
// surfaces a clear "agent session timed out (no response)"
// message instead of hanging on the busy spinner.
//
// 10 minutes matches the model of "humans typing into chat apps
// are patient but not infinitely so". Override via
// NIGHTME_OPENCODE_TURN_WATCHDOG (positive duration).
const turnWatchdogTimeout = 10 * time.Minute

// ─── buffer / cap ───

// maxImageBytes is the upper bound for a single image attachment
// read into memory before the bridge decides how to encode it.
// 10 MiB matches codex / pi limits.
const maxImageBytes = 10 * 1024 * 1024

// ─── env / version ───

// bridgeName is the AgentName stamped on every AgentEvent. Stable
// across sessions; consumed by the runtime's translator to
// attribute events to the opencode bridge (multi-bridge chat
// sessions use this to route the correct renderer / slash command
// set).
const bridgeName = "opencode"

// version is the bridge version reported in the ACP initialize
// clientInfo. Kept in sync with the module's semantic intent;
// bump manually when the wire-level contract changes
// materially. Bumped from 0.1.0 → 0.2.0 for the ACP migration
// (F-OPENCODE-ACP-MIGRATION).
const version = "0.2.0"

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
