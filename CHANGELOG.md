# Changelog

All notable changes to nightme are documented here. This project
locks a **single development version** on `main` — there is no
versioned release ladder, no semver tags, no per-version branches.
The current development snapshot lives at HEAD of `main`; whatever
is committed there is the version users build and run.

> **Migration**: see [`MIGRATION.md`](./MIGRATION.md) for breaking
> changes between earlier snapshots.

## [Unreleased] — current dev (locked 2026-08-02)

### F-41: Active Reconnect — 30s forced Stop+Start (no HTTP probe, no tier)

**Background**: F-40 added observability (`nightme health` + WSHealth struct + SDK lifecycle callbacks) so we can see when the Feishu WebSocket is down. F-41 closes the loop with **active recovery**: a 30s ticker that forces `ch.Stop() → 100ms → ch.Start()` whenever the SDK reports `OnDisconnected`, so the user-visible "no response" window drops from SDK's default 2min reconnectInterval to **30s**, and continues at 30s cadence for as long as the network stays down (no "give up after N tries" logic).

**Mechanism**: each prober tick kills the SDK's internal reconnect goroutine and starts a fresh `Start()` cycle. This effectively overrides the SDK's 2min default without changing its parameters. The prober stops on `OnReconnected` / `OnReady`; otherwise it ticks forever. No HTTP probe, no circuit breaker, no tier escalation, no watchdog.

**Files**: `internal/channel/feishu/reconnect.go` (NEW — prober struct, ticker, force-restart, snapshot), `internal/channel/feishu/reconnect_test.go` (NEW), `internal/channel/feishu/adapter.go` (wire SDK callbacks), `internal/channel/feishu/health.go` (add `Prober ProberSnapshot` to `WSHealthSnapshot`), `cmd/nightme/health.go` (new PROBER section).

**Docs**: `docs/feat/F-41-active-reconnect.md` (canonical), `docs/SPEC.md §0.9`, `docs/channel/feishu.md §13.18`.

**不变式**:
- `OutboundMessage` 契约不变 — prober 不影响 `channel.Send()`
- daemoncontrol RPC 协议向后兼容 — `health` JSON 多了 `prober` 字段,旧 client 忽略
- prober 永不主动退出(Connected 恢复或 daemon shutdown 除外)— 故意不引入"放弃重连"语义
- SDK 默认 `autoReconnect=true` 保留 — prober 跟 SDK reconnect timer **并行**,prober 抢先,SDK 兜底

### F-40: WS reconnect observability + `nightme health` command

**Background**: when a user reported "feishu消息nightme没收到" there was no signal we could read to distinguish WS down / SDK dead / reply path stuck. F-40 adds observability + a CLI command for first-stop diagnosis.

**Changes**:
- `internal/channel/feishu/health.go` (NEW): `WSHealth` struct + thread-safe ring buffers for `EventRing` (32 lifecycle events), `InboundRing` / `OutboundRing` (8 most recent successful samples). Updated by SDK `OnReady` / `OnError` / `OnDisconnected` / `OnReconnecting` / `OnReconnected` callbacks.
- `internal/daemoncontrol/`: new `health` RPC command + `HealthProvider` interface + `GetHealth` client function. `cmd/nightme/run.go` wires the post-`newChannel` adapter into the server's health provider.
- `cmd/nightme/health.go` (NEW): `nightme health [--json]` — human-readable or raw JSON status with `STATUS` / `LIVENESS` / `LAST ERROR` / `RECENT EVENTS` / `RECENT INBOUND` / `RECENT OUTBOUND` sections.

**Tests**: 8 `WSHealth` unit tests, existing `./...` tests still pass.

### F-39: `OutResult` → independent reply (reverse F-37 §13.3)

**Reverse-section proof**: Claude Code stream-json's `result.result` is byte-level equal to the last `assistant.event` content, so the previous dedup logic (`receipt_event.go:113-124`) silently swallowed the full final answer on any reply > 600 chars. F-39 reverses that path: `OutResult` no longer folds into the rolling-log receipt card, but is delivered as an **independent reply** anchored at `userMsgID` so the receipt card and the final answer become two separate surfaces.

**Three-stage dispatch** (ported from cc-connect `platform/feishu/feishu.go::buildReplyContent` + openclaw-lark `card/builder.ts::buildCompleteCard`):
- no markdown indicators → `MsgTypeText` (plain text bubble)
- markdown + tables > 5 → `MsgTypePost` + `tag:"md"` (GFM, no Card 2.0 5-table cap)
- default → `MsgTypeInteractive` (Card 2.0) with one or more `tag:"markdown"` divs, split by `splitMarkdownForDivs` at ≤ 1000 runes/div

**Markdown sanitize pipeline** (ported from cc-connect):
- `sanitizeMarkdownURLs` — non-HTTP(S) link → plain text (avoids 230001 invalid href)
- `preprocessFeishuMarkdown` — ensure ``` fence preceded by newline (lark_md renders as code block, not inline)
- `stripInvalidFeishuCardImages` — drop `![alt](not-img_xxx)`, keep Feishu image keys
- `optimizeFeishuCardMarkdown` — H1→H4, H2-H6→H5, code-block protect, newline compression

**Envelope defense**: 28 KB hard cap on the rendered card body; OutResult over the cap is truncated via `truncateRunes` and re-built. The 30 KB Feishu envelope is the ceiling; cap leaves 2 KB headroom.

**Files**: `internal/channel/feishu/{adapter.go::Send(OutResult),adapter.go::sendResultAsReceipt (new helper),card_sanitize.go (new),result_render.go (new),receipt_event.go (remove dedup + EventResult case)}`; tests `card_sanitize_test.go (new), result_render_test.go (new), adapter_test.go (TestSend_OutResult_*), receipt_event_test.go (TestEventToEntry_Result_Dropped)`.

**Docs**: `docs/feat/F-39-result-as-new-reply.md` (canonical design); `docs/SPEC.md` §0.8; `docs/channel/feishu.md` §13.16 + §13.17 + §12 渲染表 + §13.3 反转注 + §15.0 状态汇总.

**不变式**:
- `OutboundMessage` 契约不变(`Kind: OutResult`, `Result *agent.ResultEvent` typed field)
- Gateway 不动(`Translate` 仍产 OutboundMessage)
- ChatSession 不动(`currentTurnUserMsgID` 单数锚点保留)
- `ReplyTo = currentTurnUserMsgID` 不变(独立 reply 也锚同 userMsgID;Feishu 端视觉连接保留)
- 抽象归抽象 / 具体归具体(独立 reply target 是 Feishu 自治)
- §1.4 边界规范保留(OutResult 字段是 typed,Channel 自决 target)

### F-38: `/tools on|off` + per-tool thread-merge via PATCH

**Slash command**: `/tools on | /tools off` (also accepts `show`/`hide` aliases; `/tools` with no args reports current mode).

**State**: `ChatSession.ToolsMode` per-chat (`ToolsModeHide` default — opposite of `/think` which defaults to Show; rationale: tool spam is the loudest part of the agent stream and most users want it off by default), persisted as `ChatSessionEntry.ToolsMode` (JSON omitempty so old `chat_sessions.json` files decode to the safe Hide default).

**Gate**: `cmd/nightme/run.go::newEventHandler` drops `OutToolStart` and `OutToolEnd` after Translate + ReplyTo stamping when `cs.ToolsMode() == ToolsModeHide`. Other OutboundKinds (`OutText` / `OutResult` / `OutThinking` / `OutCompaction` / `OutInit` / `OutUsage`) are unaffected. Independent of the existing ThinkMode gate.

**Render upgrade** (Feishu only, `/tools on`): `internal/channel/feishu/tool_thread_merge.go` — each `OutToolStart` posts a fresh thread reply and remembers the Feishu message_id; the matching `OutToolEnd` PATCHes that same reply with the merged body (start body + newline + result body). Result: 10 tools in one agent turn = 10 thread replies (one per tool, call+result merged) instead of 20. Falls back to fresh reply on PATCH failure or orphan End (no silent data loss). Echo / other Channels unaffected — merge is Feishu-specific Channel rendering.

**Abstraction boundary preserved**: `OutboundMessage.Tool` is still a typed `ToolInfo` primitive; merging is a Channel-internal rendering decision (Feishu-specific, via `PUT /im/v1/messages/{id}` text PATCH). No new `OutboundKind`, no Gateway / ChatSession schema changes; `OutboundMessage` shape 100% unchanged from F-think.

**Files**: `internal/registry/tools_mode.go`, `internal/chatsession/toolsmode.go`, `internal/chatsession/chatsession.go`, `internal/chatsession/manager.go`, `internal/gateway/handlers_tools.go`, `internal/gateway/handlers_chatsession.go`, `cmd/nightme/run.go`, `internal/channel/feishu/tool_thread_merge.go`, `internal/channel/feishu/adapter.go`.

**Docs**: see `docs/SPEC.md` §0.7 + §3.1.3 + `docs/feat/F-38-tool-merge-and-toggle.md`.

### F-think: per-chat thinking-content visibility toggle + markdown rendering

**Slash command**: `/think on | /think off` (also accepts `show`/`hide` aliases; `/think` with no args reports current mode).

**State**: `ChatSession.ThinkMode` per-chat (`ThinkModeShow` default, `ThinkModeHide` opt-in), persisted as `ChatSessionEntry.ThinkMode` (JSON omitempty so old `chat_sessions.json` files decode to default).

**Gate**: `cmd/nightme/run.go::newEventHandler` drops `OutThinking` after Translate + ReplyTo stamping when `cs.ThinkMode() == ThinkModeHide`. Other OutboundKinds are unaffected.

**Render upgrade**: `internal/channel/feishu/thinking_card.go` — OutThinking now posts to the Feishu thread as a `Card 2.0` interactive card with `lark_md` content (via `postThreadMarkdownReply`). Long bodies split into multiple div elements via F-37 `splitMarkdownForDivs`, preserving code-block atomicity. Plain text `postThreadReply` is unchanged for OutToolStart / OutToolEnd / OutCompaction (those remain single-line summaries).

**Abstraction boundary preserved**: `OutboundMessage.Text` is still a primitive string; markdown rendering is a Channel-internal decision (Feishu-specific). No new `OutboundKind`, no Gateway / ChatSession schema changes.

**Files**: `internal/registry/think_mode.go`, `internal/chatsession/thinkmode.go`, `internal/chatsession/chatsession.go`, `internal/chatsession/manager.go`, `internal/gateway/handlers_think.go`, `internal/gateway/handlers_chatsession.go`, `cmd/nightme/run.go`, `internal/channel/feishu/thinking_card.go`, `internal/channel/feishu/adapter.go`.

**Docs**: see `docs/SPEC.md` §0.6 + §3.1.2.

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

**飞书 3 种 reply 形态 (实机群 Frtpilot-Xiage 验证，2026-08-04 子决议，关闭 §13.10 P2)**:

> **作用域**：这三个名字（`ReplyInChat` / `ReplyInThreadAndChat` / `ReplyInThread`）是 **`channel/feishu` 自治**——不上升到 Gateway / OutboundMessage 抽象层。其他 channel（Web / Slack）应**各自**决定怎么渲染 OutThinking / OutTool*，不复制飞书的 thread 方案。

| 形态 | 飞书 `reply_in_thread` 字段 | main chat 显示 | thread panel 显示 | `thread_id` 响应 |
|---|---|---|---|---|
| **ReplyInChat** (顶级 Create) | n/a | 独立气泡 | 不在 thread | `""` |
| **ReplyInThreadAndChat** | **字段省略** (`omitempty` nil) | **正文内联** | 同一份正文 | `""` |
| **ReplyInThread** | `true` | **"X replies" 灰条** | **正文** | `omt_xxx` (首次分配，后续 reply-true 复用) |

`sendMessageFunc` / `sendContent` / `sendViaLarkReply` / `SendMessageText` / `SendCard` / `postThreadReply` 全链路加尾部 `replyInThread bool` 参数。`sendViaLarkReply` 内部 `larkim.NewReplyMessageReqBodyBuilder()` **仅在 `true` 时**调 `.ReplyInThread(true)` (false 路径靠 `omitempty` 字段省略保留 recorder log / idempotency cache 字节级兼容；**严禁**简化成 `.ReplyInThread(replyInThread)` 否则 false 路径多 28 字节破坏兼容性)。

按 OutboundKind 路径拆分（2026-08-04 ops 实机确认）：

- `OutThinking` / `OutToolStart` / `OutToolEnd` → **ReplyInThread** (agent 进度只进 thread panel,main chat 仅显示 "X replies" 指示器)
- `OutCompaction` / receipt 冷启动卡 / `OutCard` (permission) / `OutCommandReply` → **ReplyInThreadAndChat** (必须 main chat 可见)
- 顶级 Create (ReplyInChat) 形态 → nightme **不**走 (fallback 230011/231003 才退化)

> Kinds 命名 ops 用 past tense (`OutToolStarted/Ended/Think`)，但 nightme enum 实际是 present tense (`OutToolStart/OutToolEnd/OutThinking`)。**不**改 enum 名（会牵动 Gateway 抽象层多个包），只按 enum 行为归属。

测试：`TestSend_ThreadOnlyEvents_PassReplyInThreadTrue` (3 kinds × ReplyInThread: OutThinking/OutToolStart/OutToolEnd) + `TestSend_ChatVisibleEvents_PassReplyInThreadFalse` (4 paths × ReplyInThreadAndChat: ReceiptColdStart/OutCard/OutCommandReply/OutCompaction) + `cmd/_probe/send_one` 实机飞书群验证。详见 `docs/feat/F-37-tool-thread-routing.md` §7.5。

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
scripts.

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