# Changelog

All notable changes to nightme are documented here. This project
locks a **single development version** on `main` — there is no
versioned release ladder, no semver tags, no per-version branches.
The current development snapshot lives at HEAD of `main`; whatever
is committed there is the version users build and run.

> **Migration**: see [`MIGRATION.md`](./MIGRATION.md) for breaking
> changes between earlier snapshots.

## [Unreleased] — current dev (locked 2026-08-02)

### Architecture: ChatSession + AgentSession (replaces v1.x Session)

The v1.x model bound one chat to one CLI process. This snapshot
splits that into two layers:

- **`ChatSession`** (per-chat): persistent per-chat context.
  Bound 1:1 to an IM chat. Owns an `AgentSession` pool keyed by
  `(agent, cwd)`, the InputBuffer FSM, the readPump, and the
  `EventHandler` callback. See [`docs/feat/F-27-chatsession.md`](./docs/feat/F-27-chatsession.md).
- **`AgentSession`** (per-`(agent, cwd)`): the actual CLI process
  handle. `(ChatSessionID, agent, cwd)` is unique within a chat's
  pool. See [`docs/feat/F-29-agent-session-pool.md`](./docs/feat/F-29-agent-session-pool.md).

The runtime manages chats via `chatsession.Manager` (replaces
v1.x `session.MemoryManager`). See
[`docs/feat/F-27-chatsession.md` §3.4](./docs/feat/F-27-chatsession.md).

### Slash commands (replaces `/run`)

| Old (v1.x) | New (current dev) | Semantics |
|---|---|---|
| `/cwd <path>` | `/cwd <path>` | Set workspace for the chat. **Does not spawn.** |
| `/run <agent>` | (deleted) | Spawn was eager; now lazy. |
| (none) | `/use <agent>` | Switch active agent; **lazy spawn** — reuse pool if present, else spawn. |
| `/kill` | `/kill` | Clear the AgentSession pool (activeCwd/activeAgent survive). |

InputBuffer FSM moves to ChatSession level: queued messages flush
to whichever `AgentSession` is currently active. The buffer state
survives `/use` switches (it is keyed on the chat, not the agent).

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

# current dev
primary: cc                    # top-level (was agent.default)
agents:                        # top-level list (was nested map)
  - name: cc
    bridge: claude
    command: "claude --dangerously-skip-permissions"
  - name: claude
    bridge: claude
    command: claude
```

`Command` is now a single string (binary + args) — split at spawn
time with `strings.Fields`. `Args` and `Env` fields are removed.
`Bridge` is a new per-entry field. See
[`configs/nightme.example.yaml`](./configs/nightme.example.yaml).

### Persistence (breaking)

| v1.x | current dev |
|---|---|
| `registry.json` (single file) | `chat_sessions.json` + `agent_sessions.json` |

The v1.x file is **not** transparently migrated to v1.2 entries —
v1.x did not persist `chat_id`, so the chat → session binding
cannot be reconstructed. On startup, v1.x's `registry.json` is
archived to `registry.json.v1.bak` and the runtime starts fresh.
See [`MIGRATION.md`](./MIGRATION.md).

### Interactive configuration

`nightme config` opens a two-level menu for setting up
agents. Currently only the `Agents` submenu exists: it merges
built-in agents (`agent.Builtins`) with user-configured entries
from `cfg.Agents`, lets the user pick which one to set as
`primary`, and saves back to `config.yaml`. Binary detection
is **not** performed — if you select a non-installed agent, the
spawn will fail at runtime. See
[`docs/feat/F-30-interactive-config.md`](./docs/feat/F-30-interactive-config.md).

### Runtime contracts (new abstractions)

- **`Spawner` interface** (`internal/chatsession/spawn.go`):
  ChatSession ↔ agent.Registry seam. The production
  `registrySpawner` wraps `agent.Registry.Get/Detect/Start`;
  tests substitute a `fakeSpawner` without forking. See
  [`docs/feat/F-27-chatsession.md`](./docs/feat/F-27-chatsession.md).
- **`EventHandler`** (`internal/chatsession/readpump.go`):
  per-ChatSession callback `func(chatID, *AgentSession,
  agent.AgentEvent)`. The runtime installs it once at startup;
  it persists across `/use` switches.
- **Default `FlushHook`**: every ChatSession has a built-in
  FlushHook that forwards queued user messages to the current
  active AgentSession's `SendBlocks`. Without this, queued
  messages would silently drop. The runtime may override via
  `SetFlushHook` (e.g., to add receipt-card side effects).

### Removed

- `cmd/nightme/run.go` (v1.x daemon): replaced by
  `cmd/nightme/run_v12.go`. The v1.x escape hatch (`--v12`
  flag, `runDeps`-based fallback) is gone — there is no flag.
- `internal/session/session.go::Session` (the per-chat single-CLI
  type): replaced by `internal/chatsession/AgentSession`.
- `internal/session/manager.go::MemoryManager`: replaced by
  `internal/chatsession/manager.go::Manager`.
- **ChatType field removed from data model** (F-33): ChatSession,
  BindingEntry, ChatSessionEntry, and `gateway.InboundMessage` no
  longer carry chat-type classification. The Gateway treats all
  chats as opaque string IDs; channel adapters classify chats
  internally for rendering decisions only. The `channel.ChatTypeThread`
  constant is dropped as well — Feishu `topic_group` (thread)
  messages flow through the same path as `p2p` / `group`. Pre-F-33
  `chat_sessions.json` files continue to load transparently (the
  `chatType` JSON field is silently ignored). The `/status`
  command no longer shows a DM/Group label. See
  [`docs/feat/F-33-simplify-chatid-data-model.md`](./docs/feat/F-33-simplify-chatid-data-model.md).
- **`InboundMessage.ReplyTo` was always empty** in pre-F-33 builds:
  F-33 wires the field from `event.Message.ParentId` (Feishu SDK's
  native parent_id). The thread-top-level `RootId` is intentionally
  not surfaced — nightme data model never carries a thread concept.
  The wire-up is currently metadata-only (no dispatch logic
  consumes `InboundMessage.ReplyTo` yet); future "reply context
  pull" features can rely on it. Outbound `ReplyTo` semantics are
  unchanged (still `currentTurnUserMsgID` per §13.10).

### Bug fixes

- **`/cwd` etc. silently failed** — `RegisterChatSessionCommands`
  registered command names with a leading slash (`"/cwd"`); the
  Gateway `ParseCommand` already strips the slash on lookup, so
  commands never matched and fell through to the fallback.
  Fixed: register as `"cwd"` / `"use"` / `"kill"`.
- **User messages silently dropped on Idle** —
  `InputBuffer.Add` calls the flush hook only when not nil;
  ChatSession constructed the buffer with `nil` hook, so
  Idle-flushed messages were no-op'd before reaching the agent.
  Fixed: `ensureBuffer` installs a default FlushHook that
  forwards to `cs.activeAS.SendBlocks`.
- **v12Fallback duplicated `errors.Is(ErrNoActiveCwd)` check** —
  cosmetic; the second branch was unreachable.

### Known gaps (deferred)

- **E2E Feishu DM round-trip test** — manual verification only;
  unit + integration tests cover F-27 / F-28 / F-29 / F-30.
- **`internal/session/` v1.x residue** — `MemoryManager` is still
  referenced by `internal/gateway/cmd/handlers.go` (v1.x binding
  helpers). Cleanup pending — tracked in
  (No separate tracking doc; see git history.)

### F-thread-route: OutThinking / OutToolStart / OutToolEnd → Feishu thread reply (2026-08-04)

反转 v1.3 §13.6 折叠方案(实机验证失败:30 panel 撞破 50 element 上限、视觉噪声大于折叠收益、最终回答被挤掉)。新方案:Channel 按 OutboundKind 自决 routing——thinking/tool/compaction 直接 POST 到 Feishu thread(rootID = userMsgID),receipt card 收窄到只承载最终答复(OutText / OutResult)+ 元数据(OutInit / OutUsage)。

**OutToolEnd 类型感知摘要**("decision 处理"):bridge 层把 `ToolEndEvent.Args` 填好;Channel 层 `summarizeToolEnd(name, args, output, err)` 按 tool name 生成单行摘要(`Read /foo.go → 1234 lines`),不 dump 原始 output 到 thread。Receipt card body 元素数从 ~30 降到 ≤5,50 element 上限永远不破。

**Bridge 层 contract 扩展**:`agent.ToolEndEvent.Args string` 字段;claudecode bridge 从同 message `tool_use` block 拿 args 填入。

**不变式**:OutboundMessage 不动(无新 Kind);Gateway 不动;ChatSession 不动;`currentTurnUserMsgID` 单数锚点保留;F-33 thread 概念不进 nightme 数据模型不变式保留;抽象归抽象 / 具体归具体 —— thread 路由是 Feishu 自治决定,Slack / Web 各自决定怎么渲染 thinking/tool。

详见 [`docs/SPEC.md` §0.3](./docs/SPEC.md) + [`docs/feat/F-37-tool-thread-routing.md`](./docs/feat/F-37-tool-thread-routing.md) + [`docs/channel/feishu.md` §13.12](./docs/channel/feishu.md) + [`docs/feat/F-25-rolling-log.md` §3.1.1](./docs/feat/F-25-rolling-log.md) + [`docs/feat/F-08-channel-abstraction.md` §4](./docs/feat/F-08-channel-abstraction.md)。

---

## Earlier snapshots (v1.x series, archived for reference)

These are preserved for diff archaeology. They predate the
current-dev model and are not compatible.

### v1.x — receipt card rolling-log

One user message → ONE Feishu reply card. The card body grows
over the agent's lifetime and FIFO-evicts from the front when
it overflows `replyMaxBytes`. Structured receipt footer
(`Agent: <name> | cwd: <path> | tokens: <in>K / <out>K`).

### v1.x — Gateway responsibility isolation

Channel ↔ Session ↔ Bridge three-layer separation. Binding table
(`chat_id → SessionID`) and per-userMessage receipt FSM owned by
Gateway; Channel renders transitions; Session is process-domain
only. See [`docs/feat/F-26-gateway-hub.md`](./docs/feat/F-26-gateway-hub.md).

### v1.x — Feishu one-click registration

QR-code onboarding via Feishu app credentials. See
[`docs/feat/F-22-feishu-onclick-registration.md`](./docs/feat/F-22-feishu-onclick-registration.md).

### v1.x — local bridge smoke test (`nightme test`)

PTY-byte-pipe smoke test for any CLI. `--cleanup` kills the child
on SIGINT/SIGTERM; default detaches. See
[`docs/feat/F-19-cli-bridge.md`](./docs/feat/F-19-cli-bridge.md).

### v1.x — structured logging + panic recovery + unified exit codes

slog + JSON output, secret redaction, `Recover()` middleware
maps panics → `CodeGenericError`, unified `ExitCode()` for CI
scripts. See [`docs/feat/F-23-heartbeat.md`](./docs/feat/F-23-heartbeat.md).

---

## Architecture (single-line summary)

```
nightme (single binary on user's laptop)
├── channel/feishu/    IM transport (Feishu WebSocket)
├── gateway/           Slash router + binding + receipt FSM
├── chatsession/        Per-chat session context + AgentSession pool
│   ├── Manager         chat_id → ChatSession
│   ├── ChatSession     activeCwd / activeAgent / pool / InputBuffer / readPump
│   ├── AgentSession    bridge.AgentSession wrapper (per (agent, cwd))
│   ├── Spawner         Detect → Start via agent.Registry
│   └── InputBuffer     Idle/Busy FSM + FlushHook
├── agent/              Agent interface + Builtins + Event
├── bridge/             Bridge abstraction (PTY / ACP / SDK / JSON-IO)
├── registry/           chat_sessions.json + agent_sessions.json (0600)
└── config/             YAML + NIGHTME_* env overrides
```

See [`docs/SPEC.md` §1](./docs/SPEC.md) for the full
responsibility table.