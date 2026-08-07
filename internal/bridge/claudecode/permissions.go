// Package claudecode implements bridge.Agent for Anthropic's Claude Code
// CLI using its stream-json mode. See docs/feat/F-24-claudecode-bridge.md.
//
// The bridge spawns `claude --print --input-format stream-json
// --output-format stream-json --permission-mode bypassPermissions
// --verbose` and parses one JSON event per stdout line. Events map to
// AgentEvent (EventText / EventToolStart / EventToolEnd / EventPermission
// / EventDone / EventError / EventResult / EventUsage / EventCompaction /
// EventInit).
//
// We deliberately do NOT pass --replay-user-messages. The flag echoes
// every user-role message back on stdout, which the channel would
// otherwise surface as a "you said …" line in the chat. The F-25 v1.1
// design replaces that with a single reply card anchored to the user
// message via Feishu's ReplyMessage API — Feishu renders the native
// "Reply to <user>: <preview>" header above the body, so the channel
// doesn't need to re-render the user's text. Skipping --replay-user-
// messages keeps the chat surface one-reply-per-user-message
// (rolling-log single card).
//
// Permission model: bypassPermissions (default) — Claude Code
// auto-accepts all permission prompts. AskUserQuestion (the structured
// user-decision tool) is detected and surfaced as EventPermission for
// the channel to render as an interactive card. See ask.go.
//
// cfg.PermissionMode (in agent.StartConfig) overrides the default; the
// Agent.Start path (claudecode.go) rewrites --permission-mode in
// DefaultArgs before exec.
package claudecode

// DefaultArgs returns the canonical flags used when spawning Claude Code
// in stream-json mode. Callers may append their own cfg.Args.
//
// The flag set is intentionally minimal:
//
//	--print                       : non-interactive (no TUI). Init emits immediately.
//	                                With bridge-held-open stdin, claude stays alive
//	                                across turns (does NOT exit after one result).
//	--input-format stream-json    : stdin = line-delimited JSON user msgs
//	--output-format stream-json   : stdout = line-delimited JSON events
//	--permission-mode bypassPermissions : auto-accept (PLACEHOLDER — Agent.Start rewrites this from cfg.PermissionMode)
//	--verbose                     : required to enable stream-json output
//
// T-alive (2026-08-07): restored `--print` after an interactive-mode
// experiment. Without `--print`, claude runs as a TUI-style process
// that gates `system init` on stdin data — Spawn returns with no
// init in the events channel and the first user message hangs.
// With `--print` + held-open stdin (the production model), init
// emits immediately and the SAME claude process handles every turn
// of a chat via repeated SendBlocks. `--resume <id>` is only needed
// on daemon restart / AS respawn (not on every in-chat turn). See
// docs/bridge/claude.md §2.
//
// We deliberately do NOT pass --model. Selection is the user's choice via
// Claude Code's own config / --settings, and forcing a model here would
// break users who route through custom model providers (e.g. MinMax).
//
// We deliberately do NOT pass --replay-user-messages. See the package
// doc for the rationale.
var DefaultArgs = []string{
	"--print",
	"--input-format", "stream-json",
	"--output-format", "stream-json",
	"--permission-mode", "bypassPermissions",
	"--verbose",
}

// Permission-mode strings accepted by Claude Code's --permission-mode
// flag. PermissionBypass is the default (preserves v0.1 behaviour).
// The other two are exposed via agent.StartConfig.PermissionMode for
// future /run flags (v0.4); see F-24 §5 follow-up notes.
//
//	PermissionBypass  — auto-approve everything
//	PermissionDefault — every tool call requires user approval (would
//	                    route through EventPermission / control_request
//	                    once stream.go's control_request hook is wired)
//	PermissionAuto    — Claude's automatic permission classifier
const (
	PermissionBypass  = "bypassPermissions"
	PermissionDefault = "default"
	PermissionAuto    = "auto"
)

// PermissionMode returns the permission mode string passed to Claude Code
// when no override is provided via agent.StartConfig.PermissionMode.
// Kept for backwards compatibility with callers that read the constant
// directly (see CHANGELOG v0.2).
const PermissionMode = PermissionBypass
