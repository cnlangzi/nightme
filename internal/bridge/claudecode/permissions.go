// Package claudecode implements bridge.Agent for Anthropic's Claude Code
// CLI using its stream-json mode. See docs/feat/F-24-claudecode-bridge.md.
//
// The bridge spawns `claude --print --input-format stream-json
// --output-format stream-json --permission-mode bypassPermissions --verbose`
// and parses one JSON event per stdout line. Events map to AgentEvent
// (EventText / EventToolStart / EventToolEnd / EventPermission / EventDone /
// EventError).
//
// Permission model: bypassPermissions — Claude Code auto-accepts all
// permission prompts. AskUserQuestion (the structured user-decision tool)
// is detected and surfaced as EventPermission for the channel to render
// as an interactive card. See ask.go.
package claudecode

// DefaultArgs returns the canonical flags used when spawning Claude Code
// in stream-json mode. Callers may append their own cfg.Args.
//
// The flag set is intentionally minimal:
//
//	--print                  : non-interactive (no TUI)
//	--input-format stream-json : stdin = line-delimited JSON user msgs
//	--output-format stream-json: stdout = line-delimited JSON events
//	--permission-mode bypassPermissions : auto-accept all permissions
//	--verbose                : required to enable stream-json output
//
// We deliberately do NOT pass --model. Selection is the user's choice via
// Claude Code's own config / --settings, and forcing a model here would
// break users who route through custom model providers (e.g. MinMax).
var DefaultArgs = []string{
	"--print",
	"--input-format", "stream-json",
	"--output-format", "stream-json",
	"--permission-mode", "bypassPermissions",
	"--verbose",
}

// PermissionMode returns the permission mode string passed to Claude Code.
// Currently fixed to bypassPermissions. The user is sovereign — /kill
// remains the escape hatch for any unwanted action.
const PermissionMode = "bypassPermissions"
