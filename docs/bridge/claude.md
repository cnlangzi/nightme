# Claude Code Bridge — 集成经验与规范

> **Status**: living reference（生产踩坑 + 约定；不是 feature 设计稿）
> **Scope**: `internal/bridge/claudecode/*` + 与之耦合的 `internal/chatsession` 运行时约定
> **Related docs**:
> - [F-24-claudecode-bridge.md](../feat/F-24-claudecode-bridge.md) — stream-json / AskUserQuestion 设计稿
> - [F-26-gateway-hub.md](../feat/F-26-gateway-hub.md) §2.3 — Events 单消费者
> - [F-27-chatsession.md](../feat/F-27-chatsession.md) — ChatSession / AgentSession 边界
> - [channel/pi.md](../channel/pi.md) — 姊妹文档（pi 侧同类经验）
> - [`tasks/claude.md`](../../tasks/claude.md) — T-alive hang 调研交接（含 test20 根因）

本文记录 **对接真实 `claude` CLI 时必须遵守的约定**。F-24 讲「怎么设计」；本文讲「实测行为是什么、哪些做法会 silent hang、测试/日志怎么写才不炸 CI」。

---

## 1. 传输层

```
nightme ──stdio JSONL──> claude --print \
  --input-format stream-json \
  --output-format stream-json \
  --permission-mode bypassPermissions \
  --verbose \
  [--resume <session_id>]
```

| Flag | 作用 | 备注 |
|------|------|------|
| `--print` | 非交互（无 TUI） | **必须**。缺了会走 interactive，`system init` 被 stdin 门控，Spawn 后无 init → 首条消息 hang |
| `--input-format stream-json` | stdin = 一行一条 JSON user msg | |
| `--output-format stream-json` | stdout = 一行一条 JSON event | |
| `--permission-mode …` | 权限策略 | 默认 `bypassPermissions`；可由 `StartConfig.PermissionMode` 覆盖 |
| `--verbose` | 打开 stream-json 输出 | 官方要求；缺了无结构化事件 |
| `--resume <id>` | 从磁盘 session 恢复上下文 | 仅 daemon 重启 / AS respawn；**不是**每轮用户消息 |

**故意不传**：

- `--model` — 交给用户的 `~/.claude/settings.json` / 自定义 provider（MiniMax 等）
- `--replay-user-messages` — 会把 user 文本回显到 stdout，飞书侧会多出「你说了…」气泡；用 Reply 锚点代替

权威 argv 组装：`DefaultArgs`（`permissions.go`）+ `buildArgs`（`claudecode.go`）。

---

## 2. 进程生命周期（实测，2026-08-07）

> 旧注释曾写「`--print` = 每 Spawn 一轮，result 后 claude 退出」。**实测不成立。**

### 2.1 生产模型：单进程多轮

```
Spawn(claude --print …)          ← 一个 bridge session / 一个 OS 进程
  │
  ├─ emit system/init            ← 立即（不依赖先写 stdin）
  ├─ SendBlocks(turn1) → … → result
  ├─ SendBlocks(turn2) → … → result   ← 同一进程，stdin 一直开着
  ├─ …
  └─ Close() / 进程退出          ← 只有显式关闭或异常才结束
```

关键事实：

1. **`--print` + bridge 持有 stdin 不关** ⇒ claude 处理完一条 **不退出**，等下一条 newline-terminated JSON。
2. 同 chat 内多轮 **不需要** respawn，也不需要每次带 `--resume`。
3. `--resume <id>` 只在 **daemon 重启 / AS 变成 Detached 后重新 Spawn** 时使用，靠 claude 磁盘 session 恢复上下文。

### 2.2 为什么必须 `--print`

没有 `--print` 时 claude 走 interactive/TUI 风格：

- `system init` **门控在 stdin 有数据之后**
- bridge `Start` 返回时 events 里还没有 init
- 首条用户消息看起来「写进去了」但永远等不到 OutReply

因此：`DefaultArgs` **永远**带 `--print`。不要为了「多轮」去删它——多轮靠持有 stdin，不靠 interactive 模式。

### 2.3 与 pi 的对照

| | Claude | Pi |
|---|---|---|
| 传输 | stream-json stdout/stdin | `--mode rpc` JSONL |
| 进程/轮次 | 一进程多轮（held stdin） | 一进程多轮（RPC session） |
| 恢复 | `--resume <id>` | `--session-id` / `new_session` |
| 终态信号 | `result` → `EventResult`/`EventDone`；进程退出 → `KindLifecycle` | `agent_settled` → `EventResult`/`EventDone` |

两边共性：**`EventDone` ≠ 关 events channel**；只有进程退出或 `Close()` 才关。上层 PumpEvents 依赖这点跨 turn 持续读。

---

## 3. 事件通路（两级 channel，单消费者）

```
claude stdout
  → pumpStream → sess.events (cap 64)
       │              ↑ 唯一读者 = AS readpump
       │              ✗ 禁止第二读者（含 resume probe）
  → AS readpump enrich → as.eventQueue (cap 256)
       │                      ↑ 唯一读者 = cs.PumpEvents
  → routeEvent → eventHandler → feishu OutReply / OutResult
```

### 3.1 硬性规则

1. **`sess.Events()` 只有一个 consumer**（AS readpump）。任何「旁路偷看」都会抢走 init/result，表现为「进程活着但 nightme 收不到事件」。
2. **`as.eventQueue` 必须非 nil**。往 nil channel 发 = 永久阻塞 → `sess.events` 填满 → claude stdout pipe 背压 → 进程 0% CPU「假 hang」。
3. **构造路径必须合一**：所有运行时字段（channel / atomic / opCtx）只在 `newAgentSessionRuntime` 分配；`NewAgentSession` 与 `FromAgentSessionEntry` 都先调它。**禁止**在包内另起 `&AgentSession{}` 字面量。

### 3.2 生产事故：daemon 重启后 test20 hang

```
daemon restart
  → FromAgentSessionEntry 漏 make(eventQueue)   ← 旧 bug
  → Spawn → startReadPump → SendBlocks ok
  → claude emit init → pumpStream → sess.events
  → readpump: as.eventQueue <- …                ← send on nil = 死锁
  → 无 OutReply；claude 活着、0% CPU、pipe 仍连着 nightme
```

症状极易误判成「claude/`--resume` 坏了」。对照检查：

| 现象 | 含义 |
|------|------|
| 日志有 `Submit SendBlocks ok`，之后完全静默 | 卡在 **events 回流**，不是 stdin |
| 同机手动 `claude --print … --resume <id>` 正常 | CLI / session id / workspace 没问题 |
| main 正常、带 eventQueue 的分支必 hang | 构造路径漂移 |

回归：`TestFromAgentSessionEntry_InitializesEventQueue`（`internal/chatsession/restore_respawn_test.go`）。

教训（一句话）：

> `NewX` 分配的每个运行时字段（尤其是 channel），`FromPersistedX` 必须同样分配。漏一个 channel = send on nil = 永久阻塞，日志看起来像「一切正常只是没回包」。

---

## 4. `--resume` 语义与健康探测

### 4.1 何时传 ResumeID

| 场景 | 是否 `--resume` |
|------|-----------------|
| chat 内第 2、3、… 条用户消息（同 AS Running） | **否** — 复用 handle + SendBlocks |
| AS Exited / Detached 后同 chat 再发 | **是** — Spawn 带磁盘上的 ResumeID |
| daemon 重启后首次消息 | **是** — 同上 |
| `/new` 或明确开新会话 | **否** — 空 ResumeID，fresh session |

### 4.2 Probe：stderr-only

`probeResume` **只读 stderr**，窗口 `resumeFallbackTimeout = 5s`。

原因：早期版本同时读 `sess.Events()`，与 AS readpump 抢同一 buffered channel → probe 常抢走 init → 上层永远看不到会话就绪（手动 claude 17s 出结果，nightme 却 60s 超时）。

Probe **不**验证「是否在响应」——那是 readpump 的事。它只看：

- 已知的 resume 拒绝信号
- probe 窗口内进程是否干净退出（无拒绝信号 → 仍视为 healthy，交给上层）

### 4.3 拒绝静默 fallback

`--resume` 失败时返回 **`ErrResumeUnhealthy`**，**禁止**静默丢掉 ResumeID 开 fresh session。

| stderr / result 文本 | 行为 |
|----------------------|------|
| `No conversation found with session ID` | unhealthy → `ErrResumeUnhealthy` |
| `--resume requires a valid session ID…` | unhealthy → `ErrResumeUnhealthy` |
| `Failed to connect MCP server …` | **healthy（忽略）** — MCP 挂不影响 init / 处理消息 |

MCP 曾被误收进 classifier：配置坏掉的 MCP 会触发「假 unhealthy」+ 静默 fallback → 用户上下文无声丢失。分类器见 `classifyStderrLineForResume`。

### 4.4 Workspace 必须匹配

`--resume` 的 session 与 **cwd** 绑定。错误 workspace 会立刻：

```
No conversation found with session ID: <uuid>
```

生产 Spawn 必须用 AS 持久化的 `Cwd`，本地 repro 也必须对齐。

---

## 5. 上层对接约定（chatsession / feishu）

### 5.1 OutReply 锚点

`OutboundMessage.ReplyTo` **必须**等于用户消息 ID（`Prompt.LastMessageID`）。

`buildPromptLocked` 末尾赋值 `p.LastMessageID = ids[n-1]`。漏了 → receipt 找不到锚点 → 新卡片 orphan → 用户感觉「没回包」（其实事件到了）。

### 5.2 KindLifecycle → SetExited

per-AS readpump 拆分后，consumer 必须把 `KindLifecycle{StatusExited}` 接到 `as.SetExited(0)`。否则 AS 永远 `StatusRunning`，复用已死 handle。

### 5.3 isReady 与 TryFlush

磁盘恢复 / Spawn 成功后必须 `isReady.Store(true)`，否则 `TryFlush SKIP reason=as_not_ready` 永久跳过投递。该字段同样经 `newAgentSessionRuntime` 统一初始化。

### 5.4 启动审计日志

daemon 启动时应能看到 `runtime: handlers installed for chat`（证明该 chat 的 eventHandler / MessageState 等已挂上）。缺这条再查 WithOnCreate 路径。

---

## 6. 可观测性与日志级别

原则：**Info = 生命周期节点；Debug = 每条消息轨迹；Warn = 失败/拒绝。**

| 级别 | 保留的内容 | 示例 |
|------|------------|------|
| **Info** | 进程/AS 生命周期、handler 安装 | `AS marked Exited`、`handlers installed for chat`、`feishu: ws connected` |
| **Warn** | 投递失败、resume 拒绝、异常路径 | `Submit SendBlocks FAILED`、`--resume spawn unhealthy` |
| **Debug** | 热路径成功轨迹 | `Submit` / `Submit SendBlocks ok`、`TryFlush SKIP`、逐事件 / stderr 普通行 |

排查「发了消息没回包」时：

```bash
bin/nightme logs --once -n 200 | grep -E "chatsession:|claudecode:|runtime:|feishu: outgoing"
```

健康路径期望：

1. `feishu: incoming`
2. （Debug）`Submit` → `Submit SendBlocks ok`
3. 很快有 `feishu: outgoing`（`send_card` / reply）或至少 handler 侧痕迹
4. **不应**出现 SendBlocks ok 后长时间完全静默

若静默：先查 `eventQueue` 是否非 nil、readpump 是否在跑、是否有第二消费者抢 events——不要先怀疑模型 API。

---

## 7. 测试规范

### 7.1 真机 vs mock

| 类型 | 何时用 | PATH 守卫 |
|------|--------|-----------|
| **真机**（spawn 真实 `claude`） | `--resume`、stream-json、MCP、多轮 stdin | **必须** `requireRealClaude(t)` |
| **Mock**（`testdata/claude_mock.py` 等） | argv / 解析 / 协议单测 | **不要** skip — CI 必跑 |

共享 helper：`internal/bridge/claudecode/testhelpers_realclaude_test.go`：

```go
func requireRealClaude(t *testing.T) {
    t.Helper()
    if _, err := exec.LookPath("claude"); err != nil {
        t.Skipf("claude binary not on PATH: %v", err)
    }
}
```

约定：

- 真机测试 **第一行**调用 `requireRealClaude(t)`，禁止各文件复制 `LookPath`
- CI / 无 `claude` 的机器 → **SKIP**，不得 FAIL
- Mock / 纯单元测试不要调用该 helper

当前真机入口（均已守卫）：

- `fresh_liveness_test.go`
- `repro_real_test.go`
- `resume_fallback_test.go`
- `resume_multi_turn_test.go`
- `resume_paths_test.go`

### 7.2 本地环境必须完整

本地手动或真机测试 **必须**让 `claude` 加载完整 `~/.claude/settings.json`（模型、`env` 里的 proxy / API key 等）。

- 二进制会自己读用户配置；不要用空环境硬 spawn
- 自定义 provider（如 MiniMax）+ `HTTP(S)_PROXY` 缺失时，表现是「hang / 无 result」，易误判成 bridge bug
- 对照实验：与 nightme 相同的 argv + cwd + settings，用独立脚本跑一遍

### 7.3 常用命令

```bash
# 不依赖真机 claude（CI 安全）
go test ./internal/bridge/claudecode/ -count=1
go test ./internal/chatsession/ -count=1 \
  -run 'TestFromAgentSessionEntry_InitializesEventQueue|TestRestoreFromRegistry'

# 本机有 claude 时跑真机 resume / 多轮（无二进制则 Skip）
go test ./internal/bridge/claudecode/ -count=1 -timeout 240s \
  -run 'TestStart_Resume|TestResumePaths|TestFresh'
```

---

## 8. 调试 checklist（用户报「没 OutReply」）

按顺序排除，避免重复踩坑：

1. **Daemon 是否新二进制？** `make restart` / 确认日志里有 `handlers installed`。
2. **日志停在哪？**
   - 无 `Submit` → 卡在 Queue / TryFlush / isReady
   - 有 `SendBlocks ok` 无后续 → events 回流（eventQueue / 消费者抢占 / 背压）
   - 有事件无飞书卡片 → ReplyTo / LastMessageID / receipt
3. **AS 是否 Running 但 handle 已死？** KindLifecycle 是否调用了 `SetExited`。
4. **是否刚重启过 daemon？** 优先怀疑 restore 构造路径（`FromAgentSessionEntry`）。
5. **同 cwd 手动 `--resume` 是否成功？** 成功则 CLI/session 正常，问题在 nightme；失败则查 workspace / session id。
6. **settings / proxy / 模型** 是否与日常交互一致。
7. **残留 `claude --print` 孤儿进程** 是否占着旧 pipe（旧 daemon 杀掉后应清理）。

---

## 9. 代码锚点（改行为时先读）

| 主题 | 位置 |
|------|------|
| 默认 argv / `--print` | `internal/bridge/claudecode/permissions.go` |
| Start + resume probe + `ErrResumeUnhealthy` | `internal/bridge/claudecode/claudecode.go` |
| session / pumpStream / stderr | `internal/bridge/claudecode/session.go` |
| 事件解码 | `internal/bridge/claudecode/stream.go` |
| 运行时唯一构造器 | `internal/chatsession/agentsession.go` → `newAgentSessionRuntime` |
| restore | `FromAgentSessionEntry`（同上） |
| Exited 接线 | `internal/chatsession/pump_events.go` |
| 真机 skip helper | `testhelpers_realclaude_test.go` |
| restore 回归 | `restore_respawn_test.go` → `TestFromAgentSessionEntry_InitializesEventQueue` |

---

## 10. 变更历史

- **2026-08-07**：T-alive 落地后初版。收录 `--print`+held-stdin 多轮实测、stderr-only probe、`ErrResumeUnhealthy`、`newAgentSessionRuntime` / eventQueue 死锁、真机 `requireRealClaude`、日志级别约定。
