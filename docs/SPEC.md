# nightme — Technical Specification (SPEC)

> **状态**：v1.3 SPEC **已落地 docs**（2026-08-03；代码改动 backlog §11）。v1.2 架构不变式全部保留，v1.3 是职责再切分——Gateway 端 Receipt FSM 移除，Channel 自治渲染
> **作者**：🦞 虾哥（PM/Architect）
> **日期**：2026-08-03（v1.3）；2026-08-02（v1.2）
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

## 0.1 文档变更摘要（v1.2 → v1.3）

v1.3 在 v1.2 架构上做**职责再切分**——核心变化是**删除 Gateway 端的 Receipt 抽象**，让"抽象归抽象、具体归具体"：

1. **Receipt FSM 从 Gateway 端移除**
   - Gateway 不再持有 `receipts[userMsgID]` map
   - `Channel.CreateReceipt / UpdateReceipt / DisposeReceipt` 接口方法从 `Channel` 接口移除
   - `internal/receipt/receipt.go` 中 `Receipt` interface + `ReceiptState` enum 删掉（保留 `MessageState`）

2. **outbound 路由改为 userMsgID-driven**
   - EventHandler 在每个 `OutboundMessage` 上设 `out.ReplyTo = cs.currentTurnUserMsgID`
   - Channel.Send 拿 `ReplyTo` 当路由 key，自行 material 化（receipt card / thread / DOM 节点）
   - Gateway 只负责"把消息送到对的 Channel"，Channel 决定怎么渲染、怎么存、怎么 PATCH

3. **`currentTurnUserMsgIDs []string` → `currentTurnUserMsgID string`**
   - 一个 turn 一个锚点（single）
   - buffered batch 时锚到这一批的最后一条 userMsgID
   - 1 turn : 1 anchor, n events（无需 fanout 多 receipt）

4. **MessageState 与 Receipt 真正解耦** — v1.2 末尾已加注释；v1.3 把 Receipt 整个删了，MessageState 真正独立运作，不再有"两个 owner 都说自己拥有 receipt"的歧义

**不变式**：
- **抽象归抽象**：Gateway = 路由器，不假设任何 Channel 渲染细节
- **具体归具体**：Channel = 渲染器，自管 receipt 生命周期，自选存储形态
- Channel 接口永远不暴露 receipt / receipt map / receipt FSM / 任何渲染状态
- Gateway 对 Channel 内部状态一无所知，只通过 `OutboundMessage` 交流

**为什么不叫 v2.0**：v1.2 的核心架构不变式（Binding FSM owner、ChatSession 三层状态、单消费者事件流、`agentSession.Events()` 单读者、InputBuffer FSM owner）**全部保留**。v1.3 是"职责再切分"的延续——把已经过度渗入 Gateway 的 Receipt 概念撤回 Channel 自治域。

---

## 1. 架构总览

nightme 是一个**单进程 daemon**，运行在用户的电脑上。它由以下**逻辑组件**组成：

```
┌─────────────────────────────────────────────────────────────┐
│  Channel Adapter (Feishu / WhatsApp / Web UI / Echo ...)   │
│   │  ↑ 自管 receipt card / thread / DOM 节点              │
│   │  ↑ Send(OutboundMessage) → 拿 ReplyTo=userMsgID 路由  │
│   │  user text / file / voice                               │
└────────────────┬────────────────────────────────────────────┘
                 │  InboundMessage / OutboundMessage（带 ReplyTo）
                 ▼
┌─────────────────────────────────────────────────────────────┐
│  nightme (single binary on user's laptop)                   │
│                                                              │
│  ┌─────────────────────────────────────────────────────┐    │
│  │  Gateway  ← 中枢 orchestrator                       │    │
│  │  • chat_id ↔ ChatSession 绑定 (Binding FSM)         │    │
│  │  • outbound 路由 (stamp ReplyTo=userMsgID 送到 Channel)│    │
│  │  • slash command 路由 (/cwd /use /kill /help /agents)│    │
│  │  • ChatSession 生命周期管理 (Create / Restore)       │    │
│  │  • Channel ↔ ChatSession ↔ AgentSession 跨层调度     │    │
│  └──────────┬────────────────────────┬─────────────────┘    │
│             │                        │                       │
│   spawn /   │                        │                       │
│   reuse /   │                        │                       │
│   kill      │                        │                       │
│             ▼                        ▼                       │
│  ┌─────────────────────┐  ┌─────────────────────┐           │
│  │  ChatSession (池)   │  │  AgentSession 池    │           │
│  │  ───────────────── │←→│  ─────────────────  │           │
│  │  activeCwd          │  │  (claude, /A) 进程   │           │
│  │  activeAgent        │  │  (codex,  /A) 进程   │           │
│  │  primaryAgent       │  │  (claude, /B) 进程   │           │
│  │  InputBuffer FSM    │  │  (codex,  /B) 进程   │           │
│  │  (idle ↔ busy)      │  │                      │           │
│  │  currentTurnUserMsgID│ │  1:1 with (agent,cwd)│           │
│  │  (单数, 跟踪变量)   │  │  immutable 标识       │           │
│  └─────────────────────┘  └─────────────────────┘           │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

### 1.1 五个逻辑组件（v1.2）

| 组件 | 职责 | 它**不知道** |
|------|------|------------|
| **Channel Adapter** | IM 协议编解码；`Send(OutboundMessage)` 渲染；**自管** receipt card / thread / DOM 节点的完整生命周期（含 cold-create / PATCH / 终态） | ChatSession、AgentSession、workspace、agent、binding、任何渲染状态 |
| **Gateway** | 中枢 orchestrator：slash command 路由、binding 表（chat_id ↔ ChatSession）、ChatSession 生命周期、Channel↔ChatSession↔AgentSession 跨层调度、outbound 路由（stamp `ReplyTo=userMsgID` 送到对应 Channel） | IM 协议细节、agent 内部协议、PTY/ACP 细节、Channel 内部渲染状态 |
| **ChatSession** | per chat 的会话上下文（持久化）：activeCwd / activeAgent / primaryAgent / InputBuffer FSM / `currentTurnUserMsgID` 跟踪 / AgentSession 池索引 | chat_id 之外没有"自己是谁"；Channel 协议细节；agent 内部协议；receipt 渲染 |
| **AgentSession** | CLI 进程句柄；`(agent, cwd)` 1:1 唯一标识（immutable）；events() chan；sendText/sendBlocks；close | chat_id、ChatSession、binding、Channel、slash command |
| **Bridge** | nightme 与底层 AI Coding CLI 之间的通信抽象；`AgentSession` 接口（Events / SendText / SendBlocks / SendPermission / Close）；四种模式（ACP / SDK / PTY / JSON-IO） | chat、binding、ChatSession、Channel |
| **Process Registry** | JSON 持久化层。两类 entry：`ChatSessionEntry`（chat_id ↔ ChatSession 绑定 + activeCwd/activeAgent/primaryAgent + AgentSession 索引）+ `AgentSessionEntry`（agent + cwd + pid + status）| 运行时语义；只持久化 |

### 1.2 三状态机，三个 owner（v1.2）

v1.3 核心架构不变式——任何状态机都**只有一个** owner，跨层状态机之间**没有循环依赖**：

| 状态机 | Owner | 状态空间 | 持久？ |
|--------|-------|----------|--------|
| **Binding FSM**（chat ↔ ChatSession）| Gateway | 1:1 绑定，永不删 | 是（ChatSessionEntry）|
| **InputBuffer FSM**（per ChatSession）| ChatSession | `idle ↔ busy` | 否（重启丢）|
| **MessageState FSM**（per userMsg）| ChatSession | `received → forwarded → done / error` | 否（重启丢）|
| **AgentSession.Status**（per AgentSession）| AgentSession | `running → detached / exited` | 是（AgentSessionEntry）|
| **ChatSession.ActiveAgentSession**（per ChatSession）| ChatSession | 引用 pool 中的某个 AgentSession | 引用在 ChatSessionEntry |

**非 FSM 跟踪变量（v1.3 新增）**：

| 变量 | Owner | 语义 |
|------|-------|------|
| **`currentTurnUserMsgID`**（per ChatSession）| ChatSession | 当前 turn 的单一 userMsgID 锚点。buffered batch 时锚到最后一条。所有 outbound event 的 `OutboundMessage.ReplyTo` = currentTurnUserMsgID |

**核心 FSM / 跟踪变量的耦合点**（全部经过 Gateway）：
- **Inbound 流**：Channel → Gateway.pumpInbound → dispatchLoop → DispatchInbound (inboundDispatcher) → 命中 `/cwd` `/use` `/kill` 走 slashCommandDispatcher → 走 ChatSession；未命中走 messageDispatcher → `cs.emitMessageState(Received)` + `chatSession.QueueUserMessage` + `chatSession.LookupActiveAgentSession`（lazy spawn）+ `agentSession.SendBlocks` + `cs.emitMessageState(Forwarded)`
- **Outbound 流**：`agentSession.Events()` → session 的 readPump（**单消费者**） → ChatSession.EventCallback → 设 `out.ReplyTo = cs.currentTurnUserMsgID` → `channel.Send` → Channel 内部按 ReplyTo 路由到对应 receipt（card / thread / DOM 节点） → PATCH
- **切 AgentSession**：`/use` 触发 → ChatSession.LookupActiveAgentSession 重新解析 → 切换 ChatSession.EventCallback 目标 → 老 AgentSession 的事件不再消费

### 1.3 不变式（v1.3 强制）

- **`ChatSession` 不 import `channel/feishu`**（事实上根本不 import `channel/` 包）
- **`AgentSession` 不 import `channel/` 也不 import `ChatSession`**（纯进程句柄）
- **Gateway 不 import `channel/feishu`**（只 import `channel.Channel` interface）
- **`ChatSession` 不知道 Channel**（只持有 Gateway 注入的 callback）
- **`AgentSession` 知道自己的 `(agent, cwd)` immutable 标识**
- **Channel 接口不暴露 ChatSession、AgentSession、binding、任何 receipt 概念**——Channel 自管渲染状态（receipt card / thread / DOM 节点），Gateway 一概不知。`Channel` interface 5 个方法：`Name / Start / Stop / Incoming / Send`
- **`agentSession.Events()` chan 的唯一消费者是 session 自己的 readPump**；ChatSession 通过 `ChatSession.EventCallback` 接收事件，**不直接读 chan**（沿用 v1.1 修复）
- **ChatSession 内 `(agent, cwd)` 唯一索引**（不是全局唯一；不同 ChatSession 可有独立 `(claude, /path/A)` AgentSession）
- **`/use` 不重启进程**：永远复用 pool 中的现有 AgentSession，找不到才 spawn 新进程
- **`/cwd` 不重启任何 AgentSession**：永远只改 activeCwd，老 AgentSession 保留在 pool
- **`currentTurnUserMsgID` 单数**：一个 turn 一个 userMsgID 锚点；buffered batch 时锚到最后一条；outbound event 的 `ReplyTo` 必等于此
- **抽象归抽象 / 具体归具体**：Gateway = 路由器（不假设任何 Channel 渲染细节）；Channel = 渲染器（自管 receipt 生命周期，自选存储形态：per-chat map / per-thread map / DOM 节点 / ...）
- **outbound 路由唯一耦合点**：`EventHandler` 在每个 `OutboundMessage` 上设 `out.ReplyTo = cs.currentTurnUserMsgID`。Channel 据此 key 路由。Gateway 不需要知道 Channel 内部 receipt 怎么存

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
     │   └ 未命中 / 普通文本 → messageDispatcher(ctx, msg)
     └ slashCommandDispatcher 走 handler；messageDispatcher 走 ChatSession（详见 §2.2）

messageDispatcher(ctx, msg)
  ├ gateway.bindings[msg.chat_id] 查 ChatSession
  │   ├ nil → channel.Send("no chat session, /cwd first")
  │   └ Status != Ready → channel.Send("not ready, /cwd + /use first")
  ├ (0) cs.emitMessageState(msg.MessageID, StateReceived)   ← F-31: 触发 MessageState(Received) 事件
  ├ (a) chatSession.LookupActiveAgentSession() (lazy spawn on miss)
  ├ (b) cs.emitMessageState(msg.MessageID, StateForwarded)   ← F-31: dispatch 成功后触发
  ├ (c) chatSession.QueueUserMessage(msg.Blocks, msg.MessageID)
  │       InputBuffer FSM:
  │         ├ Idle → 立即 SendBlocks(blocks) → return (dispatched=true)
  │         │   └ onFlush 钩子（ChatSession.defaultFlushHookLocked）触发:
  │         │       ├ cs.currentTurnUserMsgID = msg.MessageID(单数)
  │         │       └ agentSession.SendBlocks(combined)
  │         └ Busy → 入队 → return (dispatched=false)
  └ 如果 queued (Busy):
        cs.currentTurnUserMsgID 不变(仍是上一 turn 的值)
        onFlush 钩子在 EventDone 触发 flush 时:
          ├ onFlush(blocks, userMsgIDs)
          │   ├ cs.currentTurnUserMsgID = userMsgIDs[len-1](最后一条)
          │   └ agentSession.SendBlocks(combined)
          └

**v1.3 变化**：去掉 receipt lifecycle 步骤(Gateway 不再调 `ch.CreateReceipt / UpdateReceipt / DisposeReceipt`)。Channel 在收到第一个带 `ReplyTo=userMsgID` 的 OutboundMessage 时,自行决定 cold-create / 复用 receipt card / thread / DOM 节点。详见 §2.2。

**MessageState 事件**由 ChatSession 在 lifecycle 各点 emit(步骤 0、b),由 Gateway 的 `OnMessageState` 回调翻译为 `OutboundMessage{Kind: OutMessageState}` 并通过 Channel.Send 转发。详见 §2.5。
```

### 2.2 CLI 输出 → 用户（Outbound）

**核心语义：1 turn : 1 anchor, n events**。每个 agent turn 由 ChatSession 锚定到单一 `currentTurnUserMsgID`（buffered batch 时锚到最后一条 userMsgID）；EventHandler 在每个 OutboundMessage 上设 `out.ReplyTo = currentTurnUserMsgID`；Channel 据此路由到对应 receipt（card / thread / DOM 节点）。`ReplyTo == ""` 是仅有的"无锚" case（启动期 EventInit、系统日志、内部事件），Channel 走 plain text / 跳过。

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
  ├ EventDone/Error → SetState(Idle) + OnTurnEnded() + 翻 queued 消息出去
  └ EventDone/Error → cs.emitMessageStateForCurrentTurn(StateDone|StateError)
  ↓
ChatSession.onAgentEvent(s, ev)        // EventCallback 驱动
  ├ InputBuffer.SetState(Busy)
  ├ out, send := gateway.Translate(chatID, ev)
  ├ out.ReplyTo = cs.currentTurnUserMsgID    ← **关键耦合点**（唯一关联信息）
  └ if send: channel.Send(ctx, out)
  ↓
channel.Send(ctx, OutboundMessage)
  ├ 看 msg.ReplyTo（userMsgID）
  ├ 内部按 userMsgID 查自己的 receipt:
  │   ├ 命中 → PATCH / 更新（receipt card 追加内容、thread 发新回复、DOM 节点 in-place 编辑）
  │   └ miss → cold-create 一个新 receipt（userMsgID 作为 key），然后追加
  ├ (OutMessageState case) → AddReaction on Meta["message_id"] → 用户消息挂 ⏳/🔄/✅/❌
  └ (OutCard case) → 发交互卡片（permission prompt 等）
```

**关键不变量（v1.3）**：
- `ChatSession.EventCallback` 是当前 **active AgentSession** 的唯一消费者。当 `/use` 切换 active 时，ChatSession 重新注册 callback 到新的 AgentSession，老 AgentSession 的 `Events()` 不再被消费（但进程可继续跑、产出事件被丢弃——符合 PRD §4.3 的"过时的不管"语义）
- Gateway **永不** 持有 receipt / receipt map / receipt FSM——Channel 自治
- `out.ReplyTo = cs.currentTurnUserMsgID` 是 Gateway → Channel 的**唯一关联信息**——Channel 拿这个 key 路由，内部的存储形态（map / DOM / thread）自己选

**1 turn : 1 anchor 示例**（buffered batch：3 条用户消息被 flush 为 1 个 agent turn）：
- ChatSession 的 InputBuffer 攒着 [userMsgID_a, userMsgID_b, userMsgID_c]
- 上一 turn 的 EventDone 触发 onFlush → `cs.currentTurnUserMsgID = msg_c`（最后一条）
- agent 看到 stdin 是 3 条合并输入，产出 N 个 event
- 每个 event 都 PATCH msg_c 对应的那张 receipt card（F-25 rolling-log）
- msg_a / msg_b 自身的 MessageState(Done) 仍然触发（走 §2.5 的 MessageState 路径）

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
  │   └ miss → spawn 新 AgentSession(agentName, activeCwd)
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

### 2.4 Receipt 渲染（Channel 自治，v1.3）

> **v1.3 起本章不再包含 Gateway 端 FSM 描述**——Receipt FSM 从 Gateway 移除后，receipt 的生命周期完全由 Channel 自治。Gateway 不知道 Receipt 是什么、有几个、存哪里。

**Channel 自治范围内的渲染行为**（仅作协议描述，**不是 Gateway 责任**）：

- 每个 Channel 在收到第一个 `OutboundMessage{ReplyTo: userMsgID}` 时，自行决定如何"开张"：
  - **Feishu**：cold-create 一张 interactive card 作为 userMsg 的 reply；记下 cardMsgID 供后续 PATCH
  - **Slack**：在 userMsg 关联的 thread 下发第一条回复
  - **Web**：在 chat DOM 中插入一个 receipt block，带 `data-user-msg-id` 标记

- 每个 Channel 在收到后续 `OutboundMessage{ReplyTo: userMsgID}` 时，按自己定义的渲染规则 PATCH 对应 receipt：
  - Feishu：`UpdateMessage(cardMsgID, body)` in-place 编辑 card
  - Slack：thread 内发新回复 / 编辑已有回复
  - Web：更新 DOM block 的内容

- 每个 Channel 在收到 `OutMessageState` 时，按状态映射 emoji：
  - StateReceived/Forwarded/Done/Error → ⏳ / 🔄 / ✅ / ❌

**Gateway 视角**：
- Gateway 只发 `OutboundMessage{ReplyTo: userMsgID, Kind: OutText|ToolStart|...}`
- Gateway **不知道** Channel 内部有没有 receipt、存了多少、是否要清理
- Channel 内部状态（Feishu 的 entries / tokens / agentName / state / ...）完全 Channel 私有

**实现细节**：见 [`feat/F-25-rolling-log.md`](./feat/F-25-rolling-log.md)（v1.3 重命名为"rolling-log"，强调是 Channel 实现细节）。原 F-26 gateway-hub §6 中描述的 Gateway 端 Receipt FSM 代码路径全部删除。

---

### 2.5 Message Lifecycle Tracking（v1.3 新增）

**核心问题**：用户在 IM 里发了一条消息，怎么知道系统处理到哪一步了？—— `MessageState` 是这个问题的答案。

#### 2.5.1 概念

**`MessageState` = 消息的生命周期阶段属性**。回答 3 个问题：

1. ChatSession 收到消息没有？ → `StateReceived`
2. 消息转给 AgentSession 了没有？ → `StateForwarded`
3. AgentSession 执行完成了没有？ → `StateDone` (+ `StateError` 可选)

每条普通用户消息在系统里流转时，对应 `MessageState` 事件被 emit；Channel 把它渲染成平台原生视觉表达（Feishu reaction emoji，Slack emoji 短码，Web UI DOM 元素）。

#### 2.5.2 4 层事件流

```
[1] ChatSession / AgentSession
        │  emit MessageState event (state + userMsgID)
        │  via callback mechanism
        ▼
[2] Gateway
        │  接 OnMessageState callback
        │  翻译成 OutboundMessage{Kind: OutMessageState, Meta: {message_id, state}}
        │  通过 Channel.Send 提交
        ▼
[3] Channel (feishu adapter / future slack / future web ...)
        │  在 Send dispatcher 看到 OutMessageState case
        │  state → 平台原生视觉表达
        │  Feishu: AddReaction(userMsgID, emoji_type)
        ▼
[4] Platform SDK (Feishu / Slack / DOM)
        ▼
    用户看到视觉反馈（emoji / icon / progress bar）
```

**每层职责**：

| 层 | 知道什么 | 不知道什么 |
|---|---|---|
| ChatSession / AgentSession | "现在消息进入 X 状态了" | 怎么传输、谁接收、长什么样 |
| Gateway | OutboundMessage wire format；Channel interface | emoji 是什么、平台细节 |
| Channel (feishu / future) | state → emoji_type 映射；平台 SDK | 事件从哪来、谁 emit |
| Platform SDK | 平台原生 API | — |

#### 2.5.3 抽象事件契约

**`OutboundKind.OutMessageState`** — 新增枚举值，承载 message state 变化事件。

```go
OutboundMessage{
    Kind: OutMessageState,
    ChatID: chatID,
    Meta: map[string]any{
        "message_id": userMsgID,           // 必填：标记哪条用户消息
        "state":      receipt.MessageState, // 必填：状态值
    },
}
```

#### 2.5.4 触发点

| 触发时机 | 状态 | 说明 |
|---|---|---|
| `ChatSession.GetOrCreate(chatID)` 成功后 | `StateReceived` | 消息首次进 ChatSession |
| `ChatSession.LookupActiveAgentSession()` 成功 | `StateForwarded` | spawn 成功或命中 running pool |
| `ChatSession.runReadPump` 收到 `EventDone` | `StateDone` | agent 处理完 |
| `ChatSession.runReadPump` 收到 `EventError` | `StateError` | agent 出错 |

**Scope 强约束**：MessageState **只对普通用户消息触发**。Slash command（`/cwd` `/use` `/kill` 等）不产生 MessageState —— 控制平面有 `OutCommandReply` 作为反馈。

详见 [`feat/F-31-message-state.md`](./feat/F-31-message-state.md)。

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
      "primaryAgent":         "claude",          // snapshot of cfg.Primary at creation; read-only
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

**Q-A 锁定 (2026-08-02)**：Primary Agent 仅全局 config（YAML `primary`）；ChatSession.primaryAgent 是创建时的 snapshot，不可变。**无 `/default` 命令**。**`nightme config` 交互模式**用于选择 primary（见 F-30）。

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

[v1.1 不变，新增 `primary` 字段承载 Primary Agent]

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
- **Q-A** ✅ Primary Agent 仅全局 config（顶层 `primary`，与 `agents:` list 并列）；ChatSession.primaryAgent 是创建时 snapshot，不可变。**无 `/default` 命令**。
- **Q-B** ✅ LookupActiveAgentSession 只看 `(activeAgent, activeCwd)`：命中 Running 复用，否则 spawn `(activeAgent, activeCwd)`。**没有运行时 fallback**：ChatSession 始终持有一个有效的 activeAgent（创建/恢复时被 `cfg.Primary` 一次性填入），用户用 `/use` 显式覆盖，lookup 不再做降级判断。

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
- ⏭ **v1.3 SPEC 落地代码改动**：
  - `internal/receipt/receipt.go` 删 `Receipt` interface + `ReceiptState` enum（保留 `MessageState`）
  - `internal/channel/channel.go` 删 `CreateReceipt / UpdateReceipt / DisposeReceipt` 三个方法
  - `internal/gateway/gateway.go` 删 `receipts` map + `CreateReceipt / UpdateReceipt / DisposeReceipt` 方法 + 死代码 `translateAndSend` / `receiptsForSession`
  - `internal/channel/feishu/adapter.go` `Send` 路由改 userMsgID-driven（`msg.ReplyTo` 查 `receiptsByUserMsgID`，miss 时 cold-create）
  - `internal/chatsession/chatsession.go` `currentTurnUserMsgIDs []string` → `currentTurnUserMsgID string`
  - `internal/chatsession/readpump.go` `emitMessageStateForCurrentTurn` 改用单一 ID
  - `cmd/nightme/run.go` `newEventHandler` 设 `out.ReplyTo = cs.currentTurnUserMsgID`
- ⏭ 同步更新 `docs/feat/F-26-gateway-hub.md` / `docs/feat/F-25-input-buffer.md` 中描述 Receipt FSM 的段落（后者重命名为 `F-25-rolling-log.md`，强调 Channel 实现细节主导）

---

## 11.1 Daemon startup flow

`nightme run` 启动顺序（见 `cmd/nightme/run.go`）：

```
1. loadConfig()                       # ~/.config/nightme/config.yaml
2. openChatSessions() / openAgentSessions()   # chat_sessions.json / agent_sessions.json
3. buildAgents(cfg)                   # cfg.Agents → agent.Registry
4. MigrateV1ToV2(v1RegistryPath)     # 备份 registry.json → .v1.bak (idempotent)
5. spawner := NewRegistrySpawner(agents)
6. mgr := NewManager().WithSpawner(spawner).WithPersistence(csFile, asFile)
7. mgr.RestoreFromRegistry()          # 重建内存中 ChatSession 池 (AgentSession 状态=Detached)
8. ch.Start()                         # Feishu WebSocket / echo channel
9. gw := gateway.New(newMessageDispatcher(mgr, ch, cfg.Primary), nil)
10. RegisterChatSessionCommands(gw, mgr, ch, cfg.Primary)
11. for each cs in mgr.List(): cs.SetEventHandler(newEventHandler(ch, logger))
12. gwImpl.AttachChannels(ch) + gwImpl.Start()
13. block on signal / ctx.Done()
14. shutdownRun: stop channel + (cleanup? KillAll : detach)
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
| `cfg.Primary` | 重新读取配置。**不影响已存在的 ChatSession.primaryAgent**（Q-A snapshot）。 |
| `chat_sessions.json` | 全量恢复为 in-memory ChatSession。`activeCwd`、`activeAgent`、`primaryAgent` 复原。 |
| `agent_sessions.json` | 恢复为 in-memory AgentSession，**全部 `Status=Detached`，PID=0**。 |
| v1.x `registry.json` | 备份为 `.v1.bak`，不恢复数据（见 MIGRATION.md）。 |

**用户感知**：
- 第一次发消息会卡 ~100ms-2s（Spawner 重新 fork）。后续消息即时。
- 已 `/cwd` 但从未 `/use` 的 chat：发消息时 Spawner 触发 `/use` 等价的 lazy spawn（不是 `/use` 显式命令）。
- 显式 `/use` 过的 chat：第一次发消息触发 lazy spawn（因为没有运行中的进程）。

---

## 12. 文档层级

[v1.1 不变]