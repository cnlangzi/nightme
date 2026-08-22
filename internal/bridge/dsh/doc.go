// Package dsh is the nightme bridge for DeepSeek Harness (dsh).
//
// One mode, one path — `dsh --profile web` shared host serves
// every dsh session, both long-lived chat and one-shot
// RunOnce/Review. There is no headless subprocess path anymore:
//
//  1. `Starter.Start` (chat session, long-lived): the bridge
//     looks up the shared dsh web daemon (lazy-started if needed;
//     user-launched dsh on the canonical port 3080 is auto-
//     attached via `host.EnsureSharedHost`), performs the
//     `session.create` handshake, subscribes to the session's
//     mux frames via the host's Router, and emits
//     `EventAgentReady`. The returned `*agent.Agent` streams
//     events until `Close()` is called.
//
//  2. `Starter.RunOnce` / `Starter.Review` (one-shot): structurally
//     `Start + drain + defer Close`. Each call opens a fresh
//     sessionId on the shared host via `session.create {cwd}` (R2
//     isolation is explicit, not implicit like dsh CLI's old
//     headless path was), submits the prompt via `session.prompt`,
//     drains the events channel for a terminal `EventAgentResult`,
//     then `Close()` archives the session via
//     `workspace.archiveSession` so the session does not pile up
//     in dsh web's in-memory store. `cfg.SessionID` is ignored
//     — RunOnce always creates a fresh session.
//
// Wire (HTTP + WebSocket on the canonical port 3080):
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
