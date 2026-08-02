# nightme — Technical Specification (SPEC)

> **状态**：v1.1（架构职责隔离重写；架构层已锁定，实现细节按 [`feat/`](./feat/) 各自的 feature doc）
> **作者**：🦞 虾哥（PM/Architect）
> **日期**：2026-08-02
> **文档层级**：技术级（**不含实现细节 / 代码**）
> **关联文档**：
> - 产品定位 → [`PRD.md`](./PRD.md)
> - 功能索引 → [`FEATURES.md`](./FEATURES.md)
> - 每个 feature 的详细实现（含代码）→ [`feat/`](./feat/)
> - 实施计划 → [`PLAN.md`](./PLAN.md)
> - 职责隔离架构变更说明 → [`feat/F-26-gateway-hub.md`](./feat/F-26-gateway-hub.md) §13

---

## 0. 文档变更摘要（v1.0 → v1.1）

v1.1 在 v1.0 锁定架构之上做了一次**职责隔离重切**：

1. **Channel 与 Session 互不知道**——它们之间没有 import 关系、没有共享类型、没有方法依赖。所有跨层通信经过 Gateway。
2. **Gateway 是三个状态机的 owner**：
   - **Binding FSM**（chat_id ↔ session_id）—— Gateway 维护的绑定表
   - **Receipt FSM**（per-user-message: pending → executing → done/error）—— Gateway 簿记，Channel 渲染
   - **Run FSM**（/run 触发 spawn / reconnect）—— Gateway 决策，Session Manager 只做 factory
3. **Session Manager 退化成纯进程 factory**——它创建 Session（workspace + agent + PID + 状态），不绑定 chat_id、不调度 receipt、不调用 channel 接口。
4. **Channel 只做三件事**：IM 协议解码 / 编码、`Send(OutboundMessage)` 渲染、Receipt 生命周期**渲染**（不是状态机本身）。

**为什么不叫 v2.0**：PRD 锁定的产品语义（Chat ↔ Session 1:1、Chat = Project、透传原则）**一字未改**。改的是 SPEC 内部的"谁拥有什么"。v0.3 代码 release tag 仍然继续。

---

## 1. 架构总览

nightme 是一个**单进程 daemon**，运行在用户的电脑上。它由以下**逻辑组件**组成：

```
┌─────────────────────────────────────────────────────────────┐
│  Channel (Feishu / WhatsApp / Web UI / Echo ...)            │
│   │  ↑ reply / 渲染 receipt state / Send(OutboundMessage)  │
│   │  user text / file / voice                               │
└────────────────┬────────────────────────────────────────────┘
                 │  InboundMessage / OutboundMessage / Receipt API
                 ▼
┌─────────────────────────────────────────────────────────────┐
│  nightme (single binary on user's laptop)                   │
│                                                              │
│  ┌─────────────────────────────┐                             │
│  │  Gateway  ← 中枢 orchestrator│                             │
│  │  • chat_id ↔ session_id 绑定 │                             │
│  │  • userMsgID ↔ receipt 簿记  │                             │
│  │  • slash command 路由        │                             │
│  │  • /run 决策 (spawn/reconn)  │                             │
│  │  • Channel + Session 之间的  │                             │
│  │    所有事件跨层调度          │                             │
│  └──────────┬───────────────┬───┘                             │
│             │               │                                 │
│   spawn /   │               │  attachment /                  │
│   kill      │               │  receipt handle                │
│             ▼               ▼                                 │
│  ┌─────────────────┐  ┌─────────────────┐                   │
│  │ Session Manager │  │   Bridge        │                   │
│  │ (纯 factory)    │←→│ (PTY/ACP/SDK/   │                   │
│  │ • Create(ws,    │  │  JSON-IO)       │                   │
│  │   agent, args)  │  │                 │                   │
│  │ • Kill / List   │  │  events() chan  │                   │
│  │ • 不知道 chat,  │  │  sendText/Block │                   │
│  │   channel       │  │                 │                   │
│  └─────────────────┘  └─────────────────┘                   │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

### 1.1 五个逻辑组件（v1.1）

| 组件 | 职责 | 它**不知道** |
|------|------|------------|
| **Channel Adapter** | IM 协议编解码（Feishu/WhatsApp/Web/Echo）；`Send(OutboundMessage)` 渲染；**渲染** receipt state（pending/executing/done/error）的 UI 表现 | session、workspace、agent、binding 表、receipt state 机 |
| **Gateway** | 中枢 orchestrator：slash command 路由、binding 表（chat_id ↔ session_id）、receipt 簿记（userMsgID ↔ Receipt + state）、/run 决策、Channel↔Session 跨层调度、Receipt FSM 状态机 | IM 协议细节、agent 内部协议、PTY/ACP 细节 |
| **Session Manager** | 纯进程 factory：`Create(workspace, agent, args) → Session`；`Kill / List / Restore / Persist`；InputBuffer FSM (idle ↔ busy) | chat_id、channel、receipt、binding 关系 |
| **Bridge** | nightme 与底层 AI Coding CLI 之间的通信抽象；`AgentSession` 接口（Events / SendText / SendBlocks / SendPermission / Close）；四种模式（ACP / SDK / PTY / JSON-IO） | chat、binding、receipt |
| **Process Registry** | JSON 持久化层。两类 entry：`SessionEntry`（session 状态 + workspace + agent + PID）与 `BindingEntry`（chat_id ↔ session_id + chat_type）| 运行时语义；只持久化 |

### 1.2 三状态机，三个 owner

v1.1 核心架构不变式——任何状态机都**只有一个** owner，跨层状态机之间**没有循环依赖**：

| 状态机 | Owner | 状态空间 | 持久？ |
|--------|-------|----------|--------|
| **Binding FSM**（chat ↔ session）| Gateway | 1:1 绑定，永不删 | 是（BindingEntry）|
| **Receipt FSM**（per userMsg）| Gateway | `pending → executing → done/error` | 否（重启丢）|
| **InputBuffer FSM**（per session）| Session | `idle ↔ busy` | 否（重启丢）|
| **Session.Status**（per session）| Session | `running → detached / exited` | 是（SessionEntry）|

**三个状态机的耦合点**（全部经过 Gateway）：
- Fallback 流：Gateway 触发 `ch.CreateReceipt` → `session.QueueUserMessage` → `ch.UpdateReceipt(executing)` → session 内部 InputBuffer 决定 dispatch / buffer → 如果 buffer，`session.InputBuffer.onFlush`（由 Gateway 注入）→ `ch.UpdateReceipt(executing)` + `session.SendBlocks`
- Outbound 流：`session.Events()` → session 的 readPump（**单消费者**） → Manager.EventCallback（Gateway 注入）→ `Translate` → `channel.Send` → 渲染到用户
- 完成信号：Gateway 在 `EventResult`/`EventError` 上反查 `receipts[userMsgID]` → `ch.UpdateReceipt(done|error)` + `ch.DisposeReceipt`

### 1.3 不变式（v1.1 强制）

- **`session.Session` 不 import `channel/feishu`**（事实上根本不 import `channel/` 包）。
- **Gateway 不 import `channel/feishu`**（只 import `channel.Channel` interface）。
- **Session Manager 不调用任何 `Channel` 接口**。
- **Channel 接口不暴露 session、binding、receipt map**——只暴露 `Receipt` opaque 类型 + `ReceiptState` enum。
- **`session.Events()` chan 的唯一消费者是 session 的 readPump**；Gateway 通过 `Manager.EventCallback` 接收事件，**不直接读 chan**（修复 v0.2.x 的双消费者 race bug）。

---

## 2. 数据流（概念）

### 2.1 用户消息 → CLI 输入（Inbound）

```
IM 消息事件
  → Channel Adapter 解码为统一 InboundMessage{chat_id, user_id, chat_type, text, attachments, message_id, time}
      ├─ 异步下载 attachments 到本地路径（失败则 DropOrContinue）
      └─ publish 到 ch.Incoming() (chan InboundMessage)

Gateway.pumpInbound (per-channel goroutine)
  ├ chatToChan[chat_id] = ch
  └ push 到 channelCh (central chan, cap 64)

Gateway.dispatchLoop (single goroutine)
  └ Handle(ctx, msg)
     ├ ParseCommand(msg.Text)
     │   ├ 命中 (/cwd /run /kill /help /agents) → handler(msg)
     │   │   └ handler 走 gateway.bindings → manager.Create/Run/Kill → reply via channel.Send
     │   └ 未命中 / 普通文本 → fallback(ctx, msg)
     └ handler 或 fallback 走 Receipt Lifecycle（见 §2.4）

fallback(ctx, msg)
  ├ gateway.bindings[msg.chat_id] 查 session
  │   ├ nil → channel.Send("no workspace, /cwd first")
  │   └ Status != Running → channel.Send("CLI not running, /run first")
  ├ (a) receipt = ch.CreateReceipt(ctx, chat_id, msg.MessageID, msg.Blocks)
  │       (channel 内部：Feishu 把 blocks 文本化 → 发 message + 加 ⏳ reaction → 返回 opaque Receipt)
  ├ (b) gateway.receipts[msg.MessageID] = {chat_id, session_id, receipt, state: Pending}
  ├ (c) session.QueueUserMessage(msg.Blocks, msg.MessageID)
  │       InputBuffer FSM:
  │         ├ Idle → 立即 SendBlocks(blocks) → return (dispatched=true)
  │         └ Busy → 入队 → return (dispatched=false)
  ├ (d) 如果 dispatched → ch.UpdateReceipt(receipt, Executing) → state: Executing
  └ 如果 queued (Busy):
        receipt 保持 Pending
        session.InputBuffer.onFlush 钩子（Gateway 注入）会在 EventDone 触发 flush 时:
          ├ onFlush(blocks, userMsgIDs)
          │   ├ 对每个 userMsgID → ch.UpdateReceipt(receipt, Executing) → state: Executing
          │   └ session.SendBlocks(combined)
          └
```

### 2.2 CLI 输出 → 用户（Outbound）

```
Claude Code 进程 (PTY child, cwd = session.Workspace)
  ↓ stdout 是 stream-json 行
bridge/claudecode.pumpStream
  ├ 逐行解析 JSON
  ├ 翻译成 agent.AgentEvent (Text / ToolStart / ToolEnd / Permission / Done / Error / Result / Usage / Compaction / Init)
  └ 写入 agentSession.events (chan cap 64)
  ↓
session.MemoryManager.readPump (单消费者)
  ├ for ev := range as.Events()
  ├ Manager.EventCallback(s, ev)   ← Gateway 在这里注册的回调（**不另起 pump**）
  ├ InputBuffer.SetState(Busy)
  ├ EventDone/Error → SetState(Idle) + OnTurnEnded() + markExited
  └ EventDone → onFlush 钩子（Gateway 注入）→ 翻 queued receipt 到 Executing
  ↓
Gateway.EventCallback(s, ev)
  ├ Translate(chat_id, ev) → OutboundMessage (抽象 wire format)
  ├ OutResult/EventError → gateway.receipts 反查 userMsgID
  │   ├ 找到 rcpt → ch.UpdateReceipt(Done|Error) + ch.DisposeReceipt(rcpt)
  │   └ 找不到（agent 主动说话无对应 userMsg）→ 不处理
  └ channel.Send(ctx, OutboundMessage{ChatID, Kind, Text, ...})
      ├ Feishu: append 到 rolling-log message (FIFO evict) / OutCard 渲染 / OutInit 更新 receipt header
      ├ echo: 打印到 stdout
      └ future channels: 各自 native UI
```

**单消费者修正（v0.3 修复点）**：`session.Events()` chan 是单消费者——只有 session 的 readPump 读。Gateway 通过 `Manager.EventCallback` 接收事件，**不**起 pumpOutbound goroutine 去读 Events()（v0.2.x 实现里两个 reader 抢同一个 chan 是 bug）。Gateway 仍可有 sweepSessions ticker 做心跳 + 新 session 检测，但不再需要 pump goroutine。

### 2.3 用户用 slash command 创建 Session

```
用户在新 DM 发 "/cwd /path/to/project"
  → Channel Adapter 收到 InboundMessage
  → Gateway.pumpInbound / dispatchLoop → Handle()
  → ParseCommand → 命中 /cwd → handler.cwd(ctx, msg, args)
       ├ 验证 path（~ 展开、绝对路径、目录存在）
       ├ agentName = (现有 session 的 agent) || (Registry 第一个) || "claude"
       ├ gateway.bindings[msg.chat_id] 查现有 session
       │   ├ 存在且 StatusRunning → 拒绝（"CLI running, /kill first"）
       │   └ 不存在 或 Exited → 继续
       ├ call manager.Create(abs, agentName, args)
       │       → 纯 factory 返回 Session{ID, Workspace, Agent, Args, PID, Status=Running}
       ├ gateway.bindings[msg.chat_id] = BindingEntry{ChatID, ChatType, SessionID, Workspace, Agent}
       ├ registry.Upsert(SessionEntry) + registry.Upsert(BindingEntry)
       └ ch.Send(OutboundMessage{Kind: OutText, Text: "Workspace set to <abs>"})
```

`/run <agent>` 同形但只做"启动 CLI"——**Run 是 Gateway 的逻辑**：
```
handler.run(ctx, msg, args)
  ├ gateway.bindings[msg.chat_id] 拿 session
  ├ 校验 agentName 在 Registry 里
  ├ session.Status() == Running → ch.Send("Already running, pid=N")
  ├ 否则 call manager.Create(session.Workspace, agentName, args[1:]) → 新 Session
  ├ gateway.bindings[msg.chat_id].SessionID = 新 ID
  ├ registry 更新两条 entry
  └ ch.Send("Started: <agent>, pid=N, cwd=<ws>")
```

**为什么 Run 归 Gateway**：Gateway 拥有绑定关系、知道 chat 是否已有 session、决定何时 spawn。Manager 只暴露纯 factory `Create(workspace, agent, args)`。

### 2.4 Receipt 生命周期（v1.1 新增小节）

```
   ch.CreateReceipt(chat_id, user_msg_id, blocks)         [Gateway 调 channel]
        ↓
   Pending (⏳)  ─── ch.UpdateReceipt(Executing) ────►  Executing (🔄)
       │                  ▲                              │
       │ queued           │ immediate                    │ gateway.receipts 反查
       │ (Buffer Busy)    │ (Buffer Idle)                │ userMsgID on EventResult
       │                  │                              │
       │           onFlush 钩子触发 ─────────────────────┤
       │           (session.InputBuffer.Busy → Idle)    │
       │                                                ▼
       └─→ ch.DisposeReceipt (cancel path / err)       ch.UpdateReceipt(Done|Error)
                                                            ↓
                                                       Done (✅) / Error (❌)
                                                            ↓
                                                       ch.DisposeReceipt(rcpt)
                                                            ↓
                                                       delete(receipts, userMsgID)
```

**关键属性**：
- **Gateway 是 FSM 状态转移时机 owner**——它决定什么时候 UpdateReceipt
- **Channel 是 FSM 状态视觉 owner**——它决定 ⏳/🔄/✅/❌ 在自家 UI 上长什么样（Feishu swap reaction / echo 打印 / web data-attribute）
- **Session 完全不知道 receipt 存在**——InputBuffer FSM 只管 idle/busy
- **Receipt 数据是 channel 私有的**——Gateway 持 opaque `Receipt` 句柄，不解引用

---

## 3. Session 生命周期

Session 分**两层状态**——但**两层状态的所有权清晰分离**：
- **Session record**（持久，纯域）：Session Manager 拥有，`Session{ID, Workspace, Agent, Args, PID, Status}`
- **Binding**（持久，gateway 域）：Gateway 拥有，`BindingEntry{ChatID, ChatType, SessionID, Workspace, Agent}`
- **CLI process**（瞬时）：Session Manager 拥有 Session.agentSession 句柄

```
                       /cwd (binding 不存在)
    [no binding] ────────────────────────────────► [binding → session, no CLI]
                                                                  │
                                                                  │ /run
                                                                  ▼
                              /kill ◄─────────────── [binding → session, CLI running]
                                │                          ▲
                                │                          │ /run (CLI 死了)
                                ▼                          │
                         [binding → session, no CLI] ──/run┘
```

**关键规则**（v1.1 强化）：
- **Workspace 是启动 CLI 的硬性前置条件**：没有 /cwd → 没有 binding → 无法 /run
- **/run 智能处理**：session.Exited → spawn；session.Running → reconnect（不重启）
- **Binding 永不过期**：chat_id 永久绑定 Session record；workspace 不可跨 session 改
- **Session 永不过期**：CLI 死后 Session record 保留，binding 不动

### 3.1 DM vs Group Chat（Chat Type 语义）

Gateway 维护 `BindingEntry.ChatType`（v0.2 引入）作为元数据：

| ChatType | 含义 | Binding 行为 |
|----------|------|--------------|
| `p2p` (Feishu) / `private` (Telegram) / `im` (Slack) | 1-on-1 DM | **每个 DM 最多一个 binding** — bot 与用户是 1:1 关系。/cwd 仍可以设独立 workspace。 |
| `group` / `topic_group` | 群聊 / 话题群 | **每个 chat_id 一个独立 binding** — workspace 不冲突，多个项目并行。 |
| `""` | 未知 / 遗留 | **缺省按 group 处理**（安全侧）|

ChatType 在三个层流转：
1. Feishu adapter `handleMessage` 从 `P2MessageReceiveV1.event.message.chat_type` 提取
2. `channel.Message.ChatType` 传递
3. `gateway.Message.ChatType` 转发
4. **不进入 Session record**——只落在 `BindingEntry.ChatType`（Gateway 域）

### 3.2 状态转换触发器

| From | 触发 | To | 由谁驱动 |
|------|------|------|---------|
| (no binding) | 用户发送 /cwd | binding 创建 + Session.Create | Gateway.handler.cwd |
| binding, Session.Exited | 用户发送 /cwd | binding.Workspace 更新 | Gateway.handler.cwd |
| binding, Session.Running | 用户发送 /cwd | rejected (CLI running) | Gateway.handler.cwd |
| binding, Session.Exited | 用户发送 /run | Session.Create (复用 ID) | Gateway.handler.run |
| binding, Session.Running | 用户发送 /run | reconnect (no-op) | Gateway.handler.run |
| Session.Running | CLI exit / EOF | Session.Exited | Session.readPump (manager 内部) |
| Session.Running | 用户发送 /kill | Session.Exited | Gateway.handler.kill → manager.Kill |
| Session.Running | nightme SIGTERM (default) | Session.Detached（registry 标记，进程继续）| cmd/nightme shutdownRun |
| Session.Running | nightme SIGTERM --cleanup | Session.Exited (Kill) | cmd/nightme shutdownRun |
| Session.Detached | nightme 下次启动 | Session.Detached (恢复) | Manager.Restore |
| Session.Detached | 用户 /run | Session.Create (respawn) | Gateway.handler.run |

> **生命周期详细策略**（含进程归属、清理、reattach）：见 [`feat/F-06-process-cleanup.md`](./feat/F-06-process-cleanup.md)

---

## 4. 并发模型

nightme 用 Go 的 goroutine 实现并发，结构如下：

- **Main goroutine**：信号处理、组件启动顺序、优雅退出
- **Channel goroutines**：每个 Channel adapter 一组（WebSocket 收发 + sendLoop）
- **Gateway pumpInbound goroutines**：per-channel，每条 inbound message 推入 central channelCh
- **Gateway dispatchLoop goroutine**：单 consumer，从 channelCh 读 → Handle
- **Gateway sweepSessions goroutine**：5s ticker，检测新 session（**不**起 pump goroutine）
- **Per-session readPump goroutine**：session 内部，单消费者消费 `as.Events()`，驱动 InputBuffer FSM + EventCallback
- **Gateway EventCallback（同步）**：从 readPump 调用，Translate + ch.Send + receipt 反查

**并发安全**：
- Gateway.bindings / receipts：mutex 保护（极少写、极多读）
- Session 列表用 mutex 保护的 map（极少写、极多读）
- 每个 session 内部用 buffered channel 通信，无跨 session 共享状态
- 无全局锁、无 errgroup、无 singleflight

**Back-pressure**：Channel 发送慢 → gateway Translate 阻塞 → EventCallback 阻塞 → readPump 阻塞 → `as.Events()` chan buffer 满 → Bridge 阻塞 → CLI 自己阻塞。整个链路自然限速。

> **实现细节**（goroutine 代码、channel buffer 大小）：见 [`feat/F-19-cli-bridge.md`](./feat/F-19-cli-bridge.md) + [`feat/F-26-gateway-hub.md`](./feat/F-26-gateway-hub.md)

---

## 5. 技术栈（已锁定）

| 层 | 选型 | 备选 | 理由 |
|----|------|------|------|
| 主语言 | **Go 1.22+** | Rust / Node.js | 单二进制、跨平台编译简单 |
| PTY | **`github.com/aymanbagabas/go-pty`** | creack/pty | API 干净，跨平台抽象好 |
| Channel | **飞书官方 Go SDK**（lark-oapi）| 自实现 webhook | 文档全，长连接稳定 |
| HTTP API | **net/http + chi** | gin | minimal |
| 持久化 | **JSON 文件**（registry 两张表 + bindings）| SQLite | MVP 不需要 DB |
| 配置 | **YAML** | env | 直观 |
| 日志 | **`log/slog`**（标准库）| zap / zerolog | stdlib 够用 |

**为什么不选 Node.js**：node-pty 在 macOS 上偶发崩溃 + native rebuild 麻烦。
**为什么不选 Rust**：minimal 原则下 boilerplate 太多。

---

## 6. 配置

nightme 从 `~/.config/nightme/config.yaml` 读取配置。

**配置类别**（无代码，详见 PLAN.md / README）：

| 类别 | 内容 |
|------|------|
| `feishu` | app_id / app_secret / verification_token / encrypt_key |
| `agent` | default + 每个 agent 的 command/args/env |
| `session` | default PTY 大小 + 输出聚合参数 |
| `logging` | level + file path |
| `paths` | data_dir / registry_file / sessions_file / bindings_file |

**环境变量覆盖**：所有配置项支持 `NIGHTME_<SECTION>_<KEY>` 大写覆写。

**配置示例**（实际值与默认值）：见 [`PLAN.md`](./PLAN.md) §附录 / `configs/nightme.example.yaml`。

---

## 7. 非功能需求 (NFR)

| ID | 指标 |
|----|------|
| N-1 | **延迟**：用户发消息 → CLI 收到输入 < 200ms |
| N-2 | **吞吐**：CLI 输出回推到 Channel，端到端延迟 < 1s |
| N-3 | **资源占用**：单个 session PTY 空闲时 CPU ≈ 0；内存 ≈ 5-10MB |
| N-4 | **崩溃隔离**：单个 session PTY 死亡不影响其他 session |
| N-5 | **可观测**：每个 session + 每个 receipt 有结构化日志 |
| N-6 | **可移植**：单二进制，macOS / Linux 双平台 |
| N-7 | **文件权限**：config / log / registry / bindings 全部 0600 |

---

## 8. 安全（高层）

- **Channel 鉴权**：依赖 IM 平台原生鉴权（飞书 appSecret / verification token）
- **单用户假设**：MVP 不做设备配对、多用户隔离
- **Onboarding**：飞书凭证优先通过 [F-22 QR 扫码授权](./feat/F-22-feishu-onclick-registration.md)（OAuth 2.0 Device Authorization Grant）获得，避免手动复制 app_id/app_secret。详见 F-22。
- **进程隔离**：nightme 不接管用户已有进程；只能 spawn 自己创建的进程
- **网络出站**：仅连 IM 平台的长连接 endpoint，无其他出口
- **本地 IPC**：仅 listen `127.0.0.1`，不暴露 `0.0.0.0`
- **日志脱敏**：app_secret / API key 一律 redact
- **密码 / API key 透传**：见 [PRD §4.1](./PRD.md#41-完全透传不解析) — 透传原则优先，不做特殊处理

---

## 9. 已锁定的技术决策

| # | 决策 | 结论 |
|---|------|------|
| **Q1** | 技术栈 | **Go 1.22+** |
| **Q2** | MVP Channel | **只飞书** + Channel interface 抽象（含 receipt lifecycle 渲染 API）|
| **Q3** | MVP Agent | **只 Claude Code** + Agent interface 抽象 |
| **Q4** | Session 路由 | **Chat ↔ Session 1:1**（Gateway 持有 binding 表）|
| **Q5** | CLI spawn 方式 | **自己 PTY**（aymanbagabas/go-pty）|
| **Q6** | 鉴权 | **单用户独占假设**，不需要设备配对 |
| **Q7** | Channel ↔ Session 通信 | **单向经过 Gateway**，Channel 与 Session 互不引用 |
| **Q8** | Receipt 状态机 owner | **Gateway**；Channel 只渲染 |
| **Q9** | /run 逻辑归属 | **Gateway**；Manager 只做 factory |
| **Q10** | session.Events() 单消费者 | **readPump only**；Gateway 通过 EventCallback 接收 |

详细论证见 PRD §4（产品哲学）+ 各 feat 文档。

---

## 10. 与现有项目的关系

| 项目 | 关系 |
|------|------|
| pangolin | 不引用，独立项目 |
| OpenClaw | 不引用，PR/issue 流程可借用 gtw plugin |
| chrome-use / gfwproxy | 无关系 |

nightme 是**全新独立项目**，不依赖任何现有代码。

---

## 11. 下一步

技术规范 v1.1 已锁定。下一步：

1. ✅ SPEC v1.1（含职责隔离）冻结
2. ⏭ 按 [`PLAN.md`](./PLAN.md) 实施：M1 → M2 → M3 → v0.3 refactor（commit 1-6 见 F-26 §13）
3. ⏭ 每个 F-XX 详细设计按需迭代

---

## 12. 文档层级

```
PRD.md       ← 产品（什么 / 为什么 / 给谁 / 边界）
   ↓
SPEC.md      ← 技术架构（怎么构成 / 数据怎么流 / 技术栈 + 谁拥有什么）
   ↓
FEATURES.md  ← 功能索引（哪些 feature 需要实现）
   ↓
feat/F-XX    ← 每个 feature 的详细实现（含代码、接口、schema）
   ↓
PLAN.md      ← 实施计划（按什么顺序实现 / 怎么拆 commit）
```