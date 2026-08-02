# nightme — Technical Specification (SPEC)

> **状态**：v1.2 **已锁定**（2026-08-02；架构重写自 v1.1 职责隔离；Q-A ✅ + Q-B ✅ + Q-Default ✅ 均已确认并落地）
> **作者**：🦞 虾哥（PM/Architect）
> **日期**：2026-08-02
> **文档层级**：技术级（**不含实现细节 / 代码**）
> **关联文档**：
> - 产品定位 → [`PRD.md`](./PRD.md) v1.2
> - 功能索引 → [`FEATURES.md`](./FEATURES.md)
> - 每个 feature 的详细实现（含代码）→ [`feat/`](./feat/)
> - 实施计划 → [`PLAN.md`](./PLAN.md)
> - 职责隔离架构 v1.1 → [`feat/F-26-gateway-hub.md`](./feat/F-26-gateway-hub.md)

---

## 0. 文档变更摘要（v1.1 → v1.2）

v1.2 在 v1.1 锁定的职责隔离架构上做**结构重组**——不变的是"职责隔离"，变的是"会话分层"：

1. **v1.1 三层 → v1.2 两层合并**
   - v1.1：`Channel Adapter` / `Gateway` / `Session Manager` + 三种状态机（Binding / Receipt / InputBuffer）
   - v1.2：`Channel Adapter` / `Gateway` + **`ChatSession`**（合并 v1.1 ChannelSession + GatewaySession 的逻辑中枢）+ **`AgentSession`**（取代 v1.1 Session，1:1 绑定 `(agent, cwd)`）

2. **Chat ↔ Session 1:1 不变式 → Chat ↔ ChatSession 1:1 不变式**
   - 不变：1 个 IM chat 永久绑定 1 个会话上下文
   - 变：会话上下文内部可有多个 AgentSession（按 `(agent, cwd)` 1:1 唯一）

3. **`/run` 删除，`/use <agent>` 替代**
   - `/use <agent>` 切换 activeAgent；复用或新建 `(activeAgent, activeCwd)` AgentSession
   - `/cwd` 只改 activeCwd，不触发 spawn；切回能复用之前留下的 AgentSession

4. **InputBuffer FSM 位置迁移：Session → ChatSession**
   - v1.1：挂在 Session 上（per chat）
   - v1.2：挂在 ChatSession 上，跨 `/use` 切换共享 queue（已 queued 的消息发到新 active）

5. **Receipt FSM 不变**（per chatId，跨 `/use` / `/cwd` 不变）

**为什么不叫 v2.0**：v1.1 的核心架构不变式（职责隔离、Binding FSM owner、Receipt FSM owner、单消费者事件流）**全部保留**。改的是"会话的内部结构"，对外产品语义（Chat = Project 边界）保持不变。v0.4 release tag 继续。

---

## 1. 架构总览

nightme 是一个**单进程 daemon**，运行在用户的电脑上。它由以下**逻辑组件**组成：

```
┌─────────────────────────────────────────────────────────────┐
│  Channel Adapter (Feishu / WhatsApp / Web UI / Echo ...)   │
│   │  ↑ reply / 渲染 receipt state / Send(OutboundMessage)  │
│   │  user text / file / voice                               │
└────────────────┬────────────────────────────────────────────┘
                 │  InboundMessage / OutboundMessage / Receipt API
                 ▼
┌─────────────────────────────────────────────────────────────┐
│  nightme (single binary on user's laptop)                   │
│                                                              │
│  ┌─────────────────────────────────────────────────────┐    │
│  │  Gateway  ← 中枢 orchestrator                       │    │
│  │  • chat_id ↔ ChatSession 绑定 (Binding FSM)         │    │
│  │  • userMsgID ↔ receipt 簿记 (Receipt FSM)            │    │
│  │  • slash command 路由 (/cwd /use /kill /help /agents)│    │
│  │  • ChatSession 生命周期管理 (Create / Restore)       │    │
│  │  • Channel ↔ ChatSession ↔ AgentSession 跨层调度     │    │
│  └──────────┬────────────────────────┬─────────────────┘    │
│             │                        │                       │
│   spawn /   │                        │  attachment /         │
│   reuse /   │                        │  receipt handle       │
│   kill      │                        │                       │
│             ▼                        ▼                       │
│  ┌─────────────────────┐  ┌─────────────────────┐           │
│  │  ChatSession (池)   │  │  AgentSession 池    │           │
│  │  ───────────────── │←→│  ─────────────────  │           │
│  │  activeCwd          │  │  (claude, /A) 进程   │           │
│  │  activeAgent        │  │  (codex,  /A) 进程   │           │
│  │  defaultAgent       │  │  (claude, /B) 进程   │           │
│  │  InputBuffer FSM    │  │  (codex,  /B) 进程   │           │
│  │  (idle ↔ busy)      │  │                      │           │
│  │  Receipt FSM        │  │  1:1 with (agent,cwd)│           │
│  │  (per userMsgID)    │  │  immutable 标识       │           │
│  └─────────────────────┘  └─────────────────────┘           │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

### 1.1 五个逻辑组件（v1.2）

| 组件 | 职责 | 它**不知道** |
|------|------|------------|
| **Channel Adapter** | IM 协议编解码；`Send(OutboundMessage)` 渲染；**渲染** receipt state（pending/executing/done/error）的 UI 表现 | ChatSession、AgentSession、workspace、agent、binding、receipt state 机 |
| **Gateway** | 中枢 orchestrator：slash command 路由、binding 表（chat_id ↔ ChatSession）、receipt 簿记（userMsgID ↔ Receipt + state）、ChatSession 生命周期、Channel↔ChatSession↔AgentSession 跨层调度 | IM 协议细节、agent 内部协议、PTY/ACP 细节 |
| **ChatSession** | per chat 的会话上下文（持久化）：activeCwd / activeAgent / defaultAgent / InputBuffer FSM / Receipt FSM（数据）/ AgentSession 池索引 | chat_id 之外没有"自己是谁"；Channel 协议细节；agent 内部协议 |
| **AgentSession** | CLI 进程句柄；`(agent, cwd)` 1:1 唯一标识（immutable）；events() chan；sendText/sendBlocks；close | chat_id、ChatSession、binding、receipt、slash command |
| **Bridge** | nightme 与底层 AI Coding CLI 之间的通信抽象；`AgentSession` 接口（Events / SendText / SendBlocks / SendPermission / Close）；四种模式（ACP / SDK / PTY / JSON-IO） | chat、binding、receipt、ChatSession |
| **Process Registry** | JSON 持久化层。两类 entry：`ChatSessionEntry`（chat_id ↔ ChatSession 绑定 + activeCwd/activeAgent/defaultAgent + AgentSession 索引）+ `AgentSessionEntry`（agent + cwd + pid + status）| 运行时语义；只持久化 |

### 1.2 三状态机，三个 owner（v1.2）

v1.2 核心架构不变式——任何状态机都**只有一个** owner，跨层状态机之间**没有循环依赖**：

| 状态机 | Owner | 状态空间 | 持久？ |
|--------|-------|----------|--------|
| **Binding FSM**（chat ↔ ChatSession）| Gateway | 1:1 绑定，永不删 | 是（ChatSessionEntry）|
| **Receipt FSM**（per userMsg）| Gateway | `pending → executing → done/error` | 否（重启丢）|
| **InputBuffer FSM**（per ChatSession）| ChatSession | `idle ↔ busy` | 否（重启丢）|
| **AgentSession.Status**（per AgentSession）| AgentSession | `running → detached / exited` | 是（AgentSessionEntry）|
| **ChatSession.ActiveAgentSession**（per ChatSession）| ChatSession | 引用 pool 中的某个 AgentSession | 引用在 ChatSessionEntry |

**三个核心 FSM 的耦合点**（全部经过 Gateway）：
- **Inbound 流**：Channel → Gateway.pumpInbound → dispatchLoop → Handle → 命中 `/cwd` `/use` `/kill` 走 binding → 走 ChatSession；未命中走 fallback → `ch.CreateReceipt` + `chatSession.QueueUserMessage` + `chatSession.LookupActiveAgentSession` + `agentSession.SendBlocks` + `ch.UpdateReceipt(executing)`
- **Outbound 流**：`agentSession.Events()` → session 的 readPump（**单消费者**） → ChatSession.EventCallback → Gateway.translateAndSend → Channel.Send → 渲染
- **切 AgentSession**：`/use` 触发 → ChatSession.LookupActiveAgentSession 重新解析 → 切换 ChatSession.EventCallback 目标 → 老 AgentSession 的事件不再消费

### 1.3 不变式（v1.2 强制）

- **`ChatSession` 不 import `channel/feishu`**（事实上根本不 import `channel/` 包）
- **`AgentSession` 不 import `channel/` 也不 import `ChatSession`**（纯进程句柄）
- **Gateway 不 import `channel/feishu`**（只 import `channel.Channel` interface）
- **`ChatSession` 不知道 Channel**（只持有 Gateway 注入的 callback）
- **`AgentSession` 知道自己的 `(agent, cwd)` immutable 标识**
- **Channel 接口不暴露 ChatSession、AgentSession、binding、receipt map**——只暴露 `Receipt` opaque 类型 + `ReceiptState` enum
- **`agentSession.Events()` chan 的唯一消费者是 session 自己的 readPump**；ChatSession 通过 `ChatSession.EventCallback` 接收事件，**不直接读 chan**（沿用 v1.1 修复）
- **ChatSession 内 `(agent, cwd)` 唯一索引**（不是全局唯一；不同 ChatSession 可有独立 `(claude, /path/A)` AgentSession）
- **`/use` 不重启进程**：永远复用 pool 中的现有 AgentSession，找不到才 spawn 新进程
- **`/cwd` 不重启任何 AgentSession**：永远只改 activeCwd，老 AgentSession 保留在 pool
- **Receipt FSM 跨 `/use` / `/cwd` 不变**：receipt 按 userMsgID 索引，与 active AgentSession 死活解耦

---

## 2. 数据流（概念）

### 2.1 用户消息 → CLI 输入（Inbound）

```
IM 消息事件
  → Channel Adapter 解码为统一 InboundMessage{chat_id, user_id, chat_type, text, attachments, message_id, time}
      ├─ 异步下载 attachments 到本地路径
      └─ publish 到 ch.Incoming()

Gateway.pumpInbound (per-channel)
  └ push 到 channelCh

Gateway.dispatchLoop
  └ Handle(ctx, msg)
     ├ ParseCommand(msg.Text)
     │   ├ 命中 (/cwd /use /kill /help /agents) → handler(msg)
     │   │   └ handler 走 gateway.bindings → chatSession.xxx → reply via channel.Send
     │   └ 未命中 / 普通文本 → fallback(ctx, msg)
     └ handler 或 fallback 走 Receipt Lifecycle（见 §2.4）

fallback(ctx, msg)
  ├ gateway.bindings[msg.chat_id] 查 ChatSession
  │   ├ nil → channel.Send("no chat session, /cwd first")
  │   └ Status != Ready → channel.Send("not ready, /cwd + /use first")
  ├ (a) receipt = ch.CreateReceipt(ctx, chat_id, msg.MessageID, msg.Blocks)
  ├ (b) gateway.receipts[msg.MessageID] = {chatId, chatSessionId, receipt, state: Pending}
  ├ (c) chatSession.QueueUserMessage(msg.Blocks, msg.MessageID)
  │       InputBuffer FSM:
  │         ├ Idle → 立即 SendBlocks(blocks) → return (dispatched=true)
  │         └ Busy → 入队 → return (dispatched=false)
  ├ (d) 如果 dispatched → ch.UpdateReceipt(receipt, Executing) → state: Executing
  └ 如果 queued (Busy):
        receipt 保持 Pending
        chatSession.InputBuffer.onFlush 钩子（Gateway 注入）会在 EventDone 触发 flush 时:
          ├ onFlush(blocks, userMsgIDs)
          │   ├ 对每个 userMsgID → ch.UpdateReceipt(receipt, Executing) → state: Executing
          │   └ agentSession.SendBlocks(combined)
          └
```

### 2.2 CLI 输出 → 用户（Outbound）

**核心语义：1 request : n response**。每个用户发起的对话都携带 `userMsgID`；Gateway 为每个 event 携带这个 userMsgID 转发到 Channel，Channel 据此决定把响应镇在哪个用户消息的 reply card 上。ReplyTo == "" 是仅有的"真正无镇"的 case（启动提示、内部日志），走 plain text。

```
Claude Code 进程 (PTY child, cwd = chatSession.activeCwd)
  ↓ stdout 是 stream-json 行
bridge/claudecode.pumpStream
  ├ 逐行解析 JSON
  ├ 翻译成 agent.AgentEvent (Text / ToolStart / ToolEnd / Permission / Done / Error / Result / Usage / Compaction / Init)
  └ 写入 agentSession.events (chan cap 64)
  ↓
agentSession.readPump (单消费者)
  ├ for ev := range as.Events()
  ├ ChatSession.EventCallback(s, ev)   ← ChatSession 在这里注册的回调（**不另起 pump**）
  ├ InputBuffer.SetState(Busy)
  ├ EventDone/Error → SetState(Idle) + OnTurnEnded()
  └ EventDone → onFlush 钩子（ChatSession 注入）→ 翻 queued receipt 到 Executing
  ↓
ChatSession.onAgentEvent(s, ev)        // EventCallback 驱动
  ├ InputBuffer.SetState(Busy)
  ├ Gateway.translateAndSend(chatId, ev)  ← 把 AgentEvent 转 OutboundMessage + 扇出到 receipts
  └ EventResult/Error → ChatSession.notifyResult(userMsgID) → 翻 receipt 到 Done/Error
  ↓
Gateway.translateAndSend(chatId, ev)
  ├ Translate(chat_id, ev) → OutboundMessage（抽象 wire format，ReplyTo 未填）
  ├ enrichOutboundMeta(out, s)          // 注入 OutInit.Meta 的 agent_name / workspace / provider
  ├ receiptsForChatSession(chatId) → [userMsgID1, userMsgID2, ...]
  ├ if len(targets) > 0:
  │   for _, umid := range targets:
  │     out.ReplyTo = umid
  │     channel.Send(ctx, out)
  ├ else (孤儿事件):
  │   out.ReplyTo = ""
  │   channel.Send(ctx, out)
  └ EventResult → gateway.receipts 反查 userMsgID
      ├ 找到 rcpt → ch.UpdateReceipt(Done|Error) + ch.DisposeReceipt
      └ 找不到 → 不处理
```

**关键变更（v1.2）**：`ChatSession.EventCallback` 是当前 **active AgentSession** 的唯一消费者。当 `/use` 切换 active 时，ChatSession 重新注册 callback 到新的 AgentSession，老 AgentSession 的 `Events()` 不再被消费（但进程可继续跑、产出事件被丢弃——符合 PRD §4.3 的"过时的不管"语义）。

**1 request : n response 扇出示例**（buffered batch：3 条用户消息被 flush 为 1 个 agent turn）：
- ChatSession 绑定 receipts: [userMsgID_a, userMsgID_b, userMsgID_c]
- agent emit 一个 EventText → Translate 出 1 个 OutboundMessage → 扇出 3 个 OutboundMessage（各带不同 ReplyTo）
- Channel 为每个 userMsgID 路由到对应的 receipt；3 张 reply card 同时 in-place edit

### 2.3 用户用 slash command 管理 ChatSession

#### `/cwd <path>`

```
handler.cwd(ctx, msg, args)
  ├ 验证 path（~ 展开、绝对路径、目录存在）
  ├ gateway.bindings[msg.chat_id] 查 ChatSession
  │   ├ nil → channel.Send("no chat session yet, send /cwd first... wait, you are sending /cwd. retry.")
  │   └ 存在 → 继续
  ├ chatSession.SetActiveCwd(abs)  ← 仅改 activeCwd, 不动 AgentSession
  │   (AgentSession 池中的所有项不动; 切回原 cwd 时复用老 AgentSession)
  ├ registry.Upsert(ChatSessionEntry)
  └ ch.Send("Workspace set to <abs>")
```

**关键变化（v1.2）**：`/cwd` **不触发 spawn**。它是"切换 activeCwd"的纯状态变更命令。当用户后续发消息时，ChatSession 通过 `LookupActiveAgentSession()` 重新解析 `(activeAgent, activeCwd)`，按需复用或 spawn。

#### `/use <agent>`

```
handler.use(ctx, msg, args)
  ├ gateway.bindings[msg.chat_id] 查 ChatSession
  │   ├ nil → channel.Send("no chat session, /cwd first")
  │   └ 存在 → 继续
  ├ agentName := args[0]
  ├ chatSession.SetActiveAgent(agentName)   ← 仅改 activeAgent
  ├ agentSession = chatSession.LookupActiveAgentSession()
  │   ├ pool[(activeAgent, activeCwd)] 命中 → 复用 (不重启进程)
  │   ├ pool[(defaultAgent, activeCwd)] 命中 → 用 fallback（仅此条用 default）
  │   └ 都没有 → spawn 新 AgentSession(agentName, activeCwd)
  ├ chatSession.SetActiveAgentSession(agentSession)
  ├ registry.Upsert(ChatSessionEntry + AgentSessionEntry)
  └ ch.Send("Now using <agent>, pid=<N>, cwd=<ws>")
```

**关键变化（v1.2）**：
- `/use` **永不重启进程**——pool 里有就复用，没有才 spawn
- 切换前已 queued 的消息（InputBuffer）→ 自动 flush 到新的 active AgentSession
- 老 AgentSession 保留在 pool，切回原 agent/cwd 时能复用

#### `/kill`

```
handler.kill(ctx, msg, args)
  ├ gateway.bindings[msg.chat_id] 查 ChatSession
  ├ 杀 ChatSession 内所有 AgentSession (清空 pool)
  ├ ChatSession.SetActiveAgentSession(nil)
  ├ InputBuffer 清空 (queued 消息丢失, 不重发)
  ├ 老 receipts 强制 dispose (或等自然衰减)
  ├ registry 更新 (pool 清空, activeAgentSessionId=null)
  └ ch.Send("All agents killed. Send a message to start fresh.")
```

**关键变化（v1.2）**：`/kill` = "清空 ChatSession 的所有 AgentSession 上下文，重启新的"。下次消息触发 spawn 新 AgentSession。

### 2.4 Receipt 生命周期（v1.2）

**关键设计**：receipt 按 `userMsgID` 索引（**不是** chatID 也不是 AgentSession）。多 receipt/chat 可共存（buffered batch 场景），每个 receipt 镇定到自己的用户消息。

**与 v1.1 的关键差异**：
- Receipt FSM 不再与"AgentSession 死活"绑定
- `/use` / `/cwd` 切换不改变 receipt 状态机（继续 pending → executing → done）
- 老 AgentSession 死掉时其 in-flight receipt 仍按 FSM 推进（实际无新事件推进，自然衰减到 done 或永久卡住——前者正常后者需要 cleanup）

```
   ch.CreateReceipt(chat_id, user_msg_id, blocks)         [Gateway 调 channel]
        ↓
   Pending (⏳)  ─── ch.UpdateReceipt(Executing) ────►  Executing (🔄)
       │                  ▲                              │
       │ queued           │ immediate                    │ gateway.receipts 反查
       │ (Buffer Busy)    │ (Buffer Idle)                │ userMsgID on EventResult
       │                  │                              │
       │           onFlush 钩子触发 ─────────────────────┤
       │           (ChatSession.InputBuffer.Busy → Idle) │
       │                                                ▼
       └─→ ch.DisposeReceipt (cancel path / err)       ch.UpdateReceipt(Done|Error)
                                                            ↓
                                                       Done (✅) / Error (❌)
                                                            ↓
                                                       ch.DisposeReceipt(rcpt)
                                                            ↓
                                                       delete(receipts, userMsgID)
```

---

## 3. ChatSession 生命周期

ChatSession 分**三层状态**——但**三层状态的所有权清晰分离**：

```
                       /cwd (binding 不存在)
    [no binding] ────────────────────────────────► [binding → ChatSession, no activeCwd]
                                                                  │
                                                                  │ /cwd <path>
                                                                  ▼
                              /kill ◄─────────────── [binding → ChatSession, activeCwd=/path, no active AgentSession]
                                │                          ▲
                                │                          │ /use <agent> spawn
                                │                          │
                                ▼                          │
                         [binding → ChatSession, activeCwd=/path, active=(agent, cwd), pool 多条]
```

**关键规则**（v1.2）：
- **activeCwd 是 /use 的硬性前置条件**：没有 activeCwd → ChatSession 无 active AgentSession → 不能 dispatch
- **`/cwd` 不杀任何 AgentSession**：永远只改 activeCwd，老 AgentSession 保留在 pool
- **`/use` 永不重启进程**：永远复用 pool 中现有的，找不到才 spawn
- **Binding 永不过期**：chat_id 永久绑定 ChatSession；ChatSession 跨 daemon 重启恢复
- **AgentSession 不永生**：进程死掉就 status=exited；但 ChatSession 池里仍然引用它（pool 标记 `[exited]`）
- **/kill 清空 pool**：所有 AgentSession 被杀 + 池清空

### 3.1 DM vs Group Chat（Chat Type 语义）

[v1.1 不变]

### 3.2 状态转换触发器（v1.2）

| From | 触发 | To | 由谁驱动 |
|------|------|-----|----------|
| (no binding) | 用户发送 /cwd | binding 创建 + ChatSession.activeCwd 设置 + pool 初始化 | Gateway.handler.cwd |
| ChatSession, no activeCwd | 用户发送 /cwd | activeCwd 设置 | Gateway.handler.cwd |
| ChatSession, activeCwd=A, pool 含 (claude,A) | 用户发送 /use codex | activeAgent=codex, lookup (codex, A) → spawn 新 (codex,A) | Gateway.handler.use |
| ChatSession, activeCwd=A, active=(claude,A) | 用户发送 /use claude (同一) | noop (已在用) | Gateway.handler.use |
| ChatSession, activeCwd=A, active=(claude,A) | 用户发送 /cwd B | activeCwd=B; (claude,A) 仍在 pool; active=(claude,B) → spawn 新 (claude,B) | Gateway.handler.cwd |
| ChatSession, activeCwd=A | 用户发送 /kill | 清空 pool; activeAgentSession=nil; 老 receipts dispose | Gateway.handler.kill |
| AgentSession.Running | CLI exit / EOF | AgentSession.Exited（仍在 pool） | AgentSession.readPump |
| AgentSession.Running | nightme SIGTERM (default) | AgentSession.Detached | cmd/nightme shutdownRun |
| AgentSession.Running | nightme SIGTERM --cleanup | AgentSession.Exited (Kill) | cmd/nightme shutdownRun |
| AgentSession.Detached | nightme 下次启动 | AgentSession.Detached (恢复) | Registry.Restore |
| AgentSession.Detached | 用户 /use (同 agent, 同 cwd) | spawn 新进程 (复用 entry 但新 pid) | Gateway.handler.use |

> **生命周期详细策略**（含进程归属、清理、reattach）：见 [`feat/F-06-process-cleanup.md`](./feat/F-06-process-cleanup.md)

---

## 4. 并发模型

nightme 用 Go 的 goroutine 实现并发，结构如下：

- **Main goroutine**：信号处理、组件启动顺序、优雅退出
- **Channel goroutines**：每个 Channel adapter 一组（WebSocket 收发 + sendLoop）
- **Gateway pumpInbound goroutines**：per-channel
- **Gateway dispatchLoop goroutine**：单 consumer，从 channelCh 读 → Handle
- **Gateway sweepSessions goroutine**：5s ticker，检测新 ChatSession / AgentSession
- **Per-AgentSession readPump goroutine**：AgentSession 内部，单消费者消费 `as.Events()`
- **ChatSession EventCallback（同步）**：从 readPump 调用，UpdateReceipt + Translate + ch.Send
- **ChatSession 在 /use 时切换 EventCallback 目标**（老 AgentSession 的 readPump 仍在跑，事件被丢弃）

**v1.2 新增并发约束**：
- **每个 ChatSession 维护 `activeAgentSession` 指针**（原子读 / mutex 写）
- **`/use` 切换时**：先原子清空 activeAgentSession → 等 in-flight EventCallback 完成 → 重设到新 AgentSession → 启动新 AgentSession 的 readPump（如果新 spawn）
- **老 AgentSession 的 readPump 不主动停**：继续跑，但 EventCallback 是 noop（因为不再 active）

**并发安全**：
- Gateway.bindings / receipts：mutex 保护
- ChatSession.activeCwd / activeAgent / pool：mutex 保护
- 每个 AgentSession 内部用 buffered channel 通信，无跨 session 共享状态
- 无全局锁、无 errgroup、无 singleflight

**Back-pressure**：Channel 发送慢 → EventCallback 阻塞 → readPump 阻塞 → `as.Events()` chan buffer 满 → Bridge 阻塞 → CLI 自己阻塞。

> **实现细节**：见 [`feat/F-19-cli-bridge.md`](./feat/F-19-cli-bridge.md) + [`feat/F-26-gateway-hub.md`](./feat/F-26-gateway-hub.md) + [`feat/F-27-chatsession.md`](./feat/F-27-chatsession.md) + [`feat/F-29-agent-session-pool.md`](./feat/F-29-agent-session-pool.md)

---

## 5. 技术栈（已锁定）

| 层 | 选型 | 备选 | 理由 |
|----|------|------|------|
| 主语言 | **Go 1.22+** | Rust / Node.js | 单二进制、跨平台编译简单 |
| PTY | **`github.com/aymanbagabas/go-pty`** | creack/pty | API 干净，跨平台抽象好 |
| Channel | **飞书官方 Go SDK**（lark-oapi）| 自实现 webhook | 文档全，长连接稳定 |
| HTTP API | **net/http + chi** | gin | minimal |
| 持久化 | **JSON 文件**（registry ChatSession + AgentSession 两类 entry）| SQLite | MVP 不需要 DB |
| 配置 | **YAML** | env | 直观 |
| 日志 | **`log/slog`**（标准库）| zap / zerolog | stdlib 够用 |

**v1.2 持久化 schema（已落地）**：

```jsonc
{
  "chat_sessions": {
    "<chatSessionId>": {
      "chatId":               "oc_xxx",          // UNIQUE 索引 (1 chat = 1 ChatSession)
      "chatType":             "p2p",
      "activeCwd":            "/code/bailing",
      "activeAgent":          "claude",
      "defaultAgent":         "claude",          // snapshot of global Default at creation; read-only
      "agentSessionIds":      ["as_1", "as_2"],
      "activeAgentSessionId": "as_1",            // 引用 pool 中某项; null 表示未激活
      "createdAt":            "...",
      "lastInteractionAt":    "..."
    }
  },
  "agent_sessions": {
    "<agentSessionId>": {
      "chatSessionId":        "<chatSessionId>", // FK, 标识属于哪个 ChatSession 的 pool
      "agent":                "claude",          // IMMUTABLE
      "cwd":                  "/code/bailing",   // IMMUTABLE
      "pid":                  12345,
      "status":               "running | detached | exited",
      "createdAt":            "...",
      "lastRunAt":            "..."
    }
  }
}
```

**唯一约束**：
- `chat_sessions.chatId` UNIQUE（保证 1 chat = 1 ChatSession）
- `agent_sessions.(chatSessionId, agent, cwd)` UNIQUE（保证 pool 内 (agent, cwd) 1:1；不同 ChatSession 各自独立）

**Config schema (config.yaml) v1.2 (2026-08-02)**：

```yaml
primary: cc                    # top-level: global default agent
agents:                        # top-level: list of available agents
  - name: cc                  # user-defined name (overrides builtin of same name)
    bridge: claude             # bridge backend type
    command: "claude --dangerously-skip-permissions"  # full command line (binary + args)
  - name: claude              # builtin claude override (custom binary path)
    bridge: claude
    command: /custom/path/claude
```

User-configured `agents:` entries override built-ins of the same name (merge happens at runtime, not parse time).

**Q-A 锁定 (2026-08-02)**：Default 仅全局 config (YAML `primary`)；ChatSession.defaultAgent 是创建时的 snapshot，不可变。**无 `/default` 命令**。**`nightme config` 交互模式**用于选择 primary（见 F-30）。

```yaml
# 旧的 v1.x schema (仅用于历史参考, v1.2 已废弃)
# agent:
#   default: claude
#   agents:
#     claude:
#       command: claude
#       args: []
#       env: {}
```

---

## 6. 配置

[v1.1 不变，新增 `defaults.agent` 字段用于 fallback]

---

## 7. 非功能需求 (NFR)

[v1.1 不变]

新增：
- **N-8 (v1.2)**：ChatSession active AgentSession 切换延迟 ≤ 100ms（不含 spawn 新进程）

---

## 8. 安全（高层）

[v1.1 不变]

---

## 9. 已锁定的技术决策（v1.2 更新）

| # | 决策 | 结论 |
|---|------|------|
| **Q1** | 技术栈 | **Go 1.22+** |
| **Q2** | MVP Channel | **只飞书** + Channel interface 抽象 |
| **Q3** | MVP Agent | **只 Claude Code** + Agent interface 抽象 |
| **Q4** | Session 路由 | **Chat ↔ ChatSession 1:1**（Gateway 持有 binding 表）|
| **Q5** | CLI spawn 方式 | **自己 PTY**（aymanbagabas/go-pty）|
| **Q6** | 鉴权 | **单用户独占假设**，不需要设备配对 |
| **Q7** | Channel ↔ Session 通信 | **单向经过 Gateway**，Channel 与 Session 互不引用 |
| **Q8** | Receipt 状态机 owner | **Gateway**；Channel 只渲染 |
| **Q9** | `/cwd` 语义 | **只改 activeCwd**，不触发 spawn / kill |
| **Q10** | `/use` 语义 | **永不重启进程**；复用 pool 中现有 AgentSession，没有再 spawn |
| **Q11** | `/kill` 语义 | **清空 ChatSession 整个 AgentSession 池**，下次消息触发 spawn 新 |
| **Q12** | InputBuffer FSM owner | **ChatSession**（per ChatSession, 跨 `/use` 切换共享 queue）|
| **Q13** | AgentSession 唯一性 | **`(agent, cwd)` per ChatSession 唯一**；不同 ChatSession 可独立 |
| **Q14** | `session.Events()` 单消费者 | **readPump only**；ChatSession 通过 EventCallback 接收 |
| **Q15** | `/cwd` / `/use` 对 AgentSession 的影响 | **不杀任何 AgentSession**；pool 保留老 entry，切回能复用 |

**已确认（2026-08-02，PRD v1.2 锁定）**：
- **Q-A** ✅ Default 仅全局 config (`agents.default` → `agents.primary`)；ChatSession.defaultAgent 是创建时 snapshot，不可变。**无 `/default` 命令**。
- **Q-B** ✅ `(activeAgent, activeCwd)` 不在 pool 时 fallback 顺序 = **exact → `(defaultAgent, activeCwd)` → spawn `(activeAgent, activeCwd)`**（activeAgent 是用户真实意图，避免偷换）

**Q-A 锁定补充**：config schema 顶层 `primary` + `agents` list（`nightme config` 交互菜单生成）。

---

## 10. 与现有项目的关系

[v1.1 不变]

---

## 11. 下一步

技术规范已落地（commits 5/6/7/8a/8b/8c/9 + 后续 fix，`fix/cwd_session` 分支）：

- ✅ ChatSession / AgentSession 数据结构 + I/O（commits 5/6）
- ✅ Spawner 抽象 + AgentSession 真实 fork-exec（commit 7）
- ✅ Manager + `/cwd` `/use` `/kill` handlers（commit 8a）
- ✅ v1.2 daemon 切换到 `chatsession.Manager`（commits 8b/8c）
- ✅ InputBuffer FSM ownership 移到 ChatSession（commit 9）
- ✅ Config schema `primary` + `agents` list + `nightme config` 交互模式（F-30）
- ✅ FlushHook 默认转发到 active AgentSession（commit `4119e2c`）
- ✅ Command names 不带前导斜杠（commit `d54a4c1`）
- ✅ User / ops docs：README / CHANGELOG / MIGRATION（commits `2ccc443` / `dc75493`）

剩余（backlog）：
- ⏭ 真实 E2E 飞书 DM round-trip test
- ⏭ 删除 `internal/session/` v1.1 MemoryManager（仍被 `internal/gateway/cmd/handlers.go` BindingEntry shim 使用）
- ⏭ 重新实现 v1.x 的 rolling-log receipt card UX（v1.2 目前发 plain `OutText`）

---

## 11.1 Daemon startup flow

`nightme run` 启动顺序（见 `cmd/nightme/run_v12.go`）：

```
1. loadConfig()                       # ~/.config/nightme/config.yaml
2. openChatSessions() / openAgentSessions()   # chat_sessions.json / agent_sessions.json
3. buildAgents(cfg)                   # cfg.Agents → agent.Registry
4. MigrateV1ToV2(v1RegistryPath)     # 备份 registry.json → .v1.bak (idempotent)
5. spawner := NewRegistrySpawner(agents)
6. mgr := NewManager().WithSpawner(spawner).WithPersistence(csFile, asFile)
7. mgr.RestoreFromRegistry()          # 重建内存中 ChatSession 池 (AgentSession 状态=Detached)
8. ch.Start()                         # Feishu WebSocket / echo channel
9. gw := gateway.New(v12Fallback(mgr, ch, cfg.Primary), nil)
10. RegisterChatSessionCommands(gw, mgr, ch, cfg.Primary)
11. for each cs in mgr.List(): cs.SetEventHandler(v12EventHandler(ch, logger))
12. gwImpl.AttachChannels(ch) + gwImpl.Start()
13. block on signal / ctx.Done()
14. shutdownRun_v12: stop channel + (cleanup? KillAll : detach)
```

**关键不变量**：
- Step 11 的 `SetEventHandler` 在所有 ChatSession 上一次性安装；handler 跨 `/use` 持久。
- Step 4-7 在没有 v1.x 数据时无操作（idempotent）。
- Step 7 后所有 AgentSession 是 `Detached`（无进程）。用户第一次发消息 → `LookupActiveAgentSession` → Spawner spawn。

---

## 11.2 Restart semantics

Daemon 重启后（用户发送 SIGINT 然后再启 `nightme run`）：

| 数据 | 行为 |
|---|---|
| `cfg.Primary` | 重新读取配置。**不影响已存在的 ChatSession.defaultAgent**（Q-A snapshot）。 |
| `chat_sessions.json` | 全量恢复为 in-memory ChatSession。`activeCwd`、`activeAgent`、`defaultAgent` 复原。 |
| `agent_sessions.json` | 恢复为 in-memory AgentSession，**全部 `Status=Detached`，PID=0**。 |
| v1.x `registry.json` | 备份为 `.v1.bak`，不恢复数据（见 MIGRATION.md）。 |

**用户感知**：
- 第一次发消息会卡 ~100ms-2s（Spawner 重新 fork）。后续消息即时。
- 已 `/cwd` 但从未 `/use` 的 chat：发消息时 Spawner 触发 `/use` 等价的 lazy spawn（不是 `/use` 显式命令）。
- 显式 `/use` 过的 chat：第一次发消息触发 lazy spawn（因为没有运行中的进程）。

---

## 12. 文档层级

[v1.1 不变]