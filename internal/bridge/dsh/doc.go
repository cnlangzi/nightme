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
//     Resume: cfg.SessionID triggers POST /api/session.create
//     {sessionId, cwd} — the dashboard "select a session" attach
//     (dsh-api.md §2.1.3). Same id+cwd returns the same session and
//     joins the mux live set. session-conflict / transport / a
//     different returned id surface agent.ErrResumeUnhealthy so the
//     runtime clears the stale sessionId and respawns fresh.
//     F-DSH-NO-FORK (2026-08-16): session.fork mints a child and
//     abandons the parent; we never call it. Mux session/event is
//     the live path after attach; session.history backfill only
//     fills gaps. Resume-picker support: `Starter.ListSessions`
//     runs `POST /api/session.list` against the shared host.
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
//
// # Internal architecture (F-DSH-CHAT-001)
//
// The chat-session event translation layer is split into three
// cooperating components (all dsh-private — none of these are
// exposed to the agent / channel packages):
//
//   - translator (translate.go): F-52 streaming state machine.
//     Holds textBuf, pendingTools, and per-turn F-52 buffers
//     (pendingText / lastText / textDelivered / active). Mutated
//     by handler functions in dispatch.go.
//
//   - wireState (state.go): multi-source normalized truth mirror.
//     Holds tasks (by TodoItem ID), tools (by CallID), inflight
//     (tool_callIDs awaiting result), and title. Fed by three
//     wire sources (raw session/event via dispatcher, session/
//     projection via handle_mux, and ToolEventView via dispatcher).
//     Exposes DumpWireStats for ops triage.
//
//   - dispatcher (dispatch.go): registration-driven envelope
//     router. Replaces the prior `switch env.Type` in translate.go
//     with a registry of eventHandler functions; adding new event
//     types = new registration line + new handler, no switch
//     default to maintain.
//
// Lock discipline: dispatcher.dispatch acquires translator.mu +
// wireState.mu at entry (fixed order: translator first, wireState
// second). Handlers run with both locks held and MUST NOT
// re-acquire either (would deadlock). deliver() is invoked AFTER
// the locks are released. handle_mux.go's session/projection
// path runs OUTSIDE the dispatcher lock window and acquires
// only wireState.mu.
//
// Observability (P4): wireState maintains a 64-frame ring buffer
// of recent mux frames and an unknownCount counter; DumpWireStats
// surfaces both for ops triage.
package dsh
