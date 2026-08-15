// Package dsh is the nightme bridge for DeepSeek Harness (dsh).
//
// Two modes, two paths:
//
//  1. Print-mode (`Starter.RunOnce` → `dsh --profile headless
//     -- "<prompt>"`): one-shot CLI invocation. Final assistant
//     text comes back on stdout. Bridges `/gtw commit`,
//     `/gtw pr`, and `buildAgentPrompt`. **No `--resume`
//     support** — dsh web's `headless` profile documents itself as
//     "Answer one task, print the final assistant message, and
//     exit"; each RunOnce spawns a fresh process with no carry-over.
//     Callers that need multi-turn context for print-mode must use
//     the chat-session path.
//
//  2. Chat session (`Starter.Start` → `dsh --profile web --port 0`):
//     long-lived process; the bridge dials two WebSocket downlinks
//     (`/api/events.mux` + `/api/events.host`) and POSTs prompts
//     via HTTP RPC (`/api/session.prompt`). Supports mixed
//     text+image content blocks (dsh web accepts both `type:"text"`
//     and `type:"image"` with base64 inline data; resource_link
//     is rejected at the prompt boundary per 实机 HTTP probe
//     2026-08-14).
//
//     Resume: cfg.SessionID triggers `POST /api/session.fork`,
//     which dsh web translates into a NEW server-assigned session
//     whose history mirrors the parent's. On fork failure (transport
//     error, business error, server missing the requested id) the
//     bridge deliberately refuses to spawn — Start returns an error
//     wrapping agent.ErrResumeUnhealthy so the runtime clears the
//     stale sessionId and respawns fresh on the user's next message.
//     This is strict resume (NOT a silent fall-back to session.create),
//     mirroring claudecode/claudecode.go's resume-preservation
//     invariant. See session.go:handshakeSession for the full
//     rationale. Resume-picker support: `Starter.ListSessions` runs
//     `POST /api/session.list` against a throwaway dsh web and
//     returns the full Session array (the runtime filters by
//     cwd before display).
//
// The bridge deliberately does NOT modify dsh's local default
// configuration. Per the user's locked-in principle
// (agent-no-config-tampering), nightme only injects:
//
//   - cmd.Dir = cfg.Workspace (runtime context, not configuration)
//   - DSH_PERMISSION_MODE=danger-full-access (permissions — the
//     one knob the user explicitly wants nightme to override)
//
// Everything else (provider / model / API key / system prompt /
// sandbox policy / compaction / etc.) flows from dsh's local
// defaults at `~/.dsh/settings.yaml` + `~/.dsh/.credentials.yaml`.
// See docs/bridge/dsh.md for the full rationale.
package dsh
