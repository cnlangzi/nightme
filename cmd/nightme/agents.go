// Package main — agent registration table.
// This file is the single source of truth for which agents ship
// with nightme at compile time. Each line is one entry; the agent
// name is what /run <name> (and `nightme agents`) accepts, the
// bridge package supplies the underlying protocol.
//
// To add a new built-in agent:
//  1. Implement a `Starter` (Info/Detect/Start) in the relevant
//     `internal/bridge/<protocol>/` package (or extend one). Use
//     the bridge's exported `NewStarter(...)` constructor.
//  2. Add a Builtins.Register line below — append to the END of
//     the existing list. Registration order drives primary-agent
//     auto-detection (see docs/primary-agent-detection.md), so
//     inserting earlier pushes other agents down the priority
//     chain.
//
// There is no name-based dispatch table elsewhere in the binary —
// if an agent is not listed here and not in user config, /run
// returns "unknown agent". This is intentional.
//
// Cross-platform: this file compiles on both Unix and Windows.
// All builtin starters are cross-platform — binary-not-found
// errors surface from Detect() at spawn time, never at
// registration. The internal/bridge/pty package is shared
// infrastructure (used by bridges that need a PTY transport and
// by user-defined cfg.Agents entries) and is intentionally NOT
// registered here; see docs/primary-agent-detection.md §"Why
// PTY is not in Builtins".
package main

import (
	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/claudecode"
	"github.com/cnlangzi/nightme/internal/bridge/codex"
	"github.com/cnlangzi/nightme/internal/bridge/cursor"
	"github.com/cnlangzi/nightme/internal/bridge/dsh"
	"github.com/cnlangzi/nightme/internal/bridge/opencode"
	"github.com/cnlangzi/nightme/internal/bridge/pi"
)

func init() {
	// claude — the JSON-IO bridge. Dedicated implementation; user
	// config can override the command path but the override drops
	// back to PTY (loses AskUserQuestion, structured events).
	agent.Builtins.Register(claudecode.NewStarter("claude", "claude", nil))

	// codex — the `codex app-server --listen stdio://` JSON-RPC 2.0
	// bridge. Spawns codex in app-server mode and drives it via
	// raw stdio pipes (no PTY). Single backend — see
	// docs/bridge/codex.md §1 for the rationale on not supporting
	// the legacy `codex exec` backend.
	agent.Builtins.Register(codex.NewStarter("codex", "codex", nil))

	// dsh — DeepSeek Harness bridge. One mode, one path: every dsh
	// session (chat session + RunOnce + Review) goes through the
	// shared `dsh --profile web` daemon (canonical port 3080).
	// Start holds a long-lived *Agent; RunOnce is structurally
	// Start + drain + defer Close — the Close path archives the
	// session via workspace.archiveSession so it doesn't pile up
	// in dsh web's in-memory store. cfg.SessionID on RunOnce is
	// always ignored — every RunOnce gets a fresh sessionId for
	// explicit isolation from the chat session's context.
	//
	// Per the agent-no-config-tampering principle, the bridge only
	// injects cmd.Dir (workspace) and DSH_PERMISSION_MODE=danger-
	// full-access; model / provider / credentials flow from the
	// user's `~/.dsh/settings.yaml` + `~/.dsh/.credentials.yaml`.
	// See docs/bridge/dsh.md §15 for the migration rationale
	// (RunOnce moved from --profile headless to the shared host
	// on 2026-08-22 to get explicit sessionId isolation + avoid
	// the per-call cold start cost).
	//
	// Cross-platform: dsh ships as a Node.js CLI; on Windows the
	// npm installer drops a `dsh.cmd` shim which proc.New's
	// Windows branch routes through cmd.exe /d /c (see
	// internal/proc/exec_windows.go launchOnWindows). The
	// bridge code itself is stdlib-only and has no runtime.GOOS
	// gate inside. If dsh is not installed Detect() returns a
	// clear "not found in PATH" error at spawn time, not at
	// registration time.
	agent.Builtins.Register(dsh.NewStarter("dsh"))

	// opencode — the `opencode acp` Agent Client Protocol bridge.
	// Long-lived chat sessions spawn `opencode acp` under a PTY
	// and drive the standard ACP JSON-RPC 2.0 wire
	// (initialize / session/new / session/prompt /
	// session/cancel / session/request_permission). Per
	// https://opencode.ai/docs/acp/ — opencode's vendor-
	// recommended integration mode ("All features are supported").
	//
	// One-shot invocations (/gtw commit, buildAgentPrompt) use
	// the opencode run --format json print-mode spawn in print.go.
	//
	// Migration history: prior versions of this package shipped
	// an HTTP `opencode serve` SSE bridge (~3500 lines of
	// opencode-private event translation). That implementation
	// was retired in F-OPENCODE-ACP-MIGRATION — the same wire
	// surface is now available via the standard ACP protocol
	// with a thin per-bridge sessionUpdate translator (see
	// internal/bridge/opencode/update.go).
	agent.Builtins.Register(opencode.NewStarter("opencode", "opencode", []string{"acp"}))

	// cursor — the `cursor-agent acp` Agent Client Protocol bridge.
	// Cursor CLI natively supports ACP via `cursor-agent acp`.
	// Reuses the generic ACP bridge for protocol handling;
	// unlike opencode, no sessionUpdate translator is needed
	// (Cursor's events are handled by the generic acp fallback).
	//
	// Per the agent-no-config-tampering principle, the bridge does
	// not rewrite `~/.cursor/`. It injects spawn-time full-access
	// flags (`--force --trust --sandbox disabled --approve-mcps`)
	// ahead of the `acp` subcommand — same role as Claude's
	// --permission-mode bypassPermissions and Codex's
	// approval_policy=never + sandbox_mode=danger-full-access.
	// Parent flags MUST precede `acp` (the acp subcommand does
	// not parse --force; putting it after `acp` is a silent no-op).
	//
	// One-shot invocations use `cursor-agent -p` print-mode (plain
	// text output, not NDJSON) with the same full-access flags.
	// The binary name is `cursor-agent` because that is the
	// entry-point the official installer drops onto PATH: bash
	// installer creates it as a legacy symlink alongside the
	// primary `agent` alias, PowerShell installer
	// (https://cursor.com/install?win32=true) creates
	// cursor-agent.cmd as the real entry and copies it to agent.cmd
	// as a courtesy alias. Bridge picks the canonical real name
	// (the installer's actual binary, not its alias) so detection
	// works on every platform the official installer supports
	// without us having to mirror its aliasing logic in CI.
	agent.Builtins.Register(cursor.NewStarter("cursor", "cursor-agent", cursor.DefaultACPArgs))

	// pi — the long-lived `pi --mode rpc` JSONL bridge. The agent
	// driver is the @earendil-works/pi-coding-agent CLI; see
	// docs/feat/F-32-pi-rpc-bridge.md for wire details and the
	// F-32 MVP scope. User config can override the command path;
	// the structured bridge only works for builtin registration,
	// not for the PTY fallback in agentregistry.Build.
	agent.Builtins.Register(pi.NewStarter("pi", "pi", nil))
}
