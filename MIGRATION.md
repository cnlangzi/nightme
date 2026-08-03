# Migration Guide

nightme locks a single development version on `main` — there is
no versioned release ladder. This document describes **breaking
changes between earlier snapshots** of the codebase and the
current dev (locked 2026-08-02). If you build from `main` today,
this is the migration you must apply.

---

## From v1.x (MemoryManager + Session) → current dev (ChatSession + AgentSession)

The biggest breaking change is the replacement of `session.Session`
with the two-layer `chatsession.ChatSession` + `chatsession.AgentSession`.
A v1.x install on the current dev binary will fail to start.

### Slash commands

| v1.x | current dev |
|---|---|
| `/cwd /tmp` then `/run claude` | `/cwd /tmp` then `/use claude` |
| `/run <agent>` exists | **deleted** — `/use` is the replacement (lazy spawn) |
| `/kill` | `/kill` (same name, different semantics — clears pool, preserves cwd/agent) |

`/use` semantics:
- If `(agent, cwd)` already in the chat's AgentSession pool →
  **reuse** the existing process (no restart).
- Otherwise → spawn a new AgentSession and add to the pool.

There is no explicit "spawn" command. The first message after
`/cwd` triggers lazy spawn automatically (via
`ChatSession.LookupActiveAgentSession`).

### Config schema (breaking)

```yaml
# v1.x
agent:
  default: claude
  agents:
    claude:
      command: claude
      args: []
      env: {}
    codex:
      command: codex-acp
      args: []
      env: {}

# current dev
primary: cc                              # top-level scalar
agents:                                  # top-level LIST (was nested map)
  - name: cc
    bridge: claude
    command: "claude --dangerously-skip-permissions"
  - name: claude
    bridge: claude
    command: claude
  - name: codex
    bridge: codex
    command: codex-acp
```

Field changes:

| v1.x | current dev |
|---|---|
| `agent.default` | `primary` (top-level) |
| `agent.agents.<name>` (map) | `agents[]` (list, with `name` field) |
| `command: claude` + `args: []` | `command: "claude --auto-approve"` (single string) |
| — | `bridge: claude` (new field, picks Bridge backend) |
| `env: {KEY: VALUE}` | (removed; use shell env) |

`Command` is now a single string parsed at spawn time with
`strings.Fields` (whitespace-split; no quoting yet — if your
command has spaces in paths, this is a rough edge).

Override env vars:
- `NIGHTME_AGENT_DEFAULT` → `NIGHTME_PRIMARY`

### Persistence (breaking)

v1.x: single file `registry.json` (or under `cfg.Paths.DataDir`).
current dev: two files `chat_sessions.json` + `agent_sessions.json`.

**Migration is NOT transparent.** v1.x did not persist `chat_id`
on its session records (the binding was in-memory only — see
`internal/gateway/binding.go` v1.x), so the chat → session
mapping cannot be reconstructed from disk alone.

On startup, `cmd/nightme/run.go` runs
`registry.MigrateV1ToV2(v1RegistryPath)` which:
1. Reads `registry.json` if present.
2. Copies it to `registry.json.v1.bak` (idempotent; existing
   backup is preserved).
3. Does **not** write any v1.2 entries — v1.x data is archived
   only.
4. The runtime starts with an empty `chat_sessions.json` and
   `agent_sessions.json`.

**Action required**: after upgrading, re-do `/cwd` for each chat.
The `MigrateV1ToV2` backup file is kept on disk for forensic
recovery only; you can delete it once you're confident you don't
need it.

### Code-level migration (Go API consumers)

If you import `internal/session` (or the older `internal/session`
package) for any reason, note:

| v1.x symbol | current dev equivalent |
|---|---|
| `session.Session` | `chatsession.AgentSession` (1:1 with `(agent, cwd)`) |
| `session.NewMemoryManager` | `chatsession.NewManager` |
| `mgr.Create` / `mgr.Register` | `mgr.GetOrCreate(chatID, ...)` then `cs.LookupActiveAgentSession` |
| `mgr.Kill(sessionID)` | `cs.KillAll()` (per chat) |
| `sess.QueueUserMessage` | `cs.QueueUserMessage` (same signature; flush via default hook) |
| `session.EventCallback` (set on Manager) | `cs.SetEventHandler` (per-chat) |
| `session.InputBuffer` (per-Session) | `chatsession.InputBuffer` (per-ChatSession; survives `/use`) |

The `internal/session/` package still exists in the codebase for
the v1.x `internal/gateway/cmd/handlers.go` binding helpers
(`Session = *session.Session` type alias). Cleanup of those
helpers is **pending** — no separate tracking doc; see git history.

---

## From "single-command `nightme test` only" → `nightme run` daemon

Earlier snapshots had `nightme test` (PTY passthrough, one
process, stdin/stdout) and `nightme run` (daemon). The current
dev's `nightme run` is the **only** daemon path; v1.x's
MemoryManager-based daemon was deleted in commit `13fe21c`.

If you previously ran `nightme test` for the full feature set
(`/cwd`, `/run`, agent spawn), switch to `nightme run`.

### Channel selection

- `--channel=feishu` (default): WebSocket adapter; requires
  `~/.config/nightme/config.yaml` with `feishu.app_id` and
  `feishu.app_secret` (run `nightme auth login feishu` to get them).
- `--channel=echo`: no-network stub that prints outbound messages
  to stdout. Useful for smoke tests.

---

## Common pitfalls after upgrade

1. **Forgot to re-`/cwd`**: after restart, every chat replies with
   "No workspace set. Send `/cwd <path>` first." (correct v1.2
   behaviour — `LookupActiveAgentSession` returns
   `ErrNoActiveCwd`). The v1.x `registry.json` data was
   intentionally not migrated (see above); re-issue `/cwd` per
   chat.

2. **`/run <agent>` fails**: `/run` is deleted. Use `/use <agent>`
   instead. `/use` is lazy: it spawns only if no `(agent, cwd)`
   exists in the chat's pool.

3. **Messages appear to disappear**: if your custom hook or
   adapter overrides `ChatSession.SetFlushHook` with a nil-returning
   closure, queued messages will silently drop. The default
   FlushHook forwards to `cs.activeAS.SendBlocks`. Don't replace
   it without an equivalent.

4. **Per-chat AgentSession is no longer globally unique**: two
   different chats can each have `(claude, /code/A)` running
   independently. `(agent, cwd)` is unique **per ChatSession**,
   not globally. If you need a single shared CLI across chats,
   you need a different design — v1.2 explicitly does not support it.

5. **Receipt card UX changed**: `nightme run` on the current dev
   sends plain `OutText` replies for non-slash messages, not
   v1.x's rolling-log receipt card. Receipt-card UX is not
   re-implemented in v1.2 yet (tracked as deferred in
   `docs/SPEC.md` §11).

---

## Compatibility matrix

| Component | v1.x | current dev |
|---|---|---|
| Config file | `agent:` map | `agents:` list + `primary` scalar |
| Persistence | `registry.json` | `chat_sessions.json` + `agent_sessions.json` (v1.x archived to `.v1.bak`) |
| `/run` | exists | deleted |
| `/use` | n/a | lazy spawn (reuse or new) |
| Session model | `Session` per chat | `ChatSession` per chat + `AgentSession` pool |
| InputBuffer | per `Session` | per `ChatSession` (survives `/use`) |
| Spawner abstraction | none | `internal/chatsession.Spawner` |
| ReadPump lifecycle | per `Session` | per `ChatSession` (one active AgentSession at a time) |
| Default FlushHook | n/a | always installed (`cs.activeAS.SendBlocks`) |
| v1.x registry → v1.2 entries | n/a | **not migrated**; archived only |
| `nightme config` command | n/a | new (Agents submenu; saves `primary`) |
| `--v12` flag | n/a | removed (v1.2 is the only path) |

---

## Rollback

There is no clean rollback path. The binary at any commit is
self-consistent (CLI, daemon, persistence), so `git checkout
<old-sha>` + rebuild + restore `registry.json` from
`registry.json.v1.bak` (if you kept it) restores v1.x behaviour
for that branch.

For production, take a backup before upgrading:

```bash
cp ~/.local/share/nightme/registry.json{,.pre-v1.2.bak}
cp ~/.config/nightme/config.yaml{,.pre-v1.2.bak}
```