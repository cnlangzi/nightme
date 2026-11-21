# ChatSession → AgentSession → Conversation 三层同步控制流

> 这份文档讲清楚 nightme runtime 怎么把"一个 IM chat"映射到"一段对话"上。
> 三层:
> - **ChatSession**(per-IM-chat):会话级状态、AgentSession 池、消息队列
> - **AgentSession**(per-(agent,cwd)):子进程句柄、生命周期、per-AS ctx
> - **Prompt**(per-message):单次往返的状态载体与事件锚点

---

## 0. 三层概览

```
┌─────────────────────────────────────────────────────────────────┐
│  IM Chat (Feishu chatID)                                        │
│                                                                 │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │  ChatSession  (cs.mu + cs.routeMu)                        │  │
│  │  ┣━ selectedCwd / selectedAgent / primaryAgent            │  │
│  │  ┣━ pool map[agentCwdKey] → *AgentSession                 │  │
│  │  ┣━ selectedAS *AgentSession                             │  │
│  │  ┣━ queue *MessageQueue (FSM Idle↔Busy) + flushHook      │  │
│  │  ┣━ AgentEventBus / MessageStateBus / PromptEndBus        │  │
│  │  ┣━ emitter messages.Emitter(出站咽喉)                   │  │
│  │  ┗━ watchMode / thinkMode / toolsMode(per-chat 开关)      │  │
│  └───────────────────────────────────────────────────────────┘  │
│            │                                                     │
│            │ selectedAS / pool 查找                              │
│            ▼                                                     │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │  AgentSession  (as.mu, status, ...)                       │  │
│  │  ┣━ ID, Agent, Cwd, args, SessionID                      │  │
│  │  ┣━ opCtx / opCancel  ← per-AS 操作 ctx                  │  │
│  │  ┣━ currentPrompt *Prompt ← 当前 turn 的状态载体         │  │
│  │  ┣━ events chan agent.AgentEvent(单消费者)              │  │
│  │  ┗━ model, exitCode, status                              │  │
│  └───────────────────────────────────────────────────────────┘  │
│            │                                                     │
│            │ handle / SendBlocks                                │
│            ▼                                                     │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │  bridge session (claudecode / codex / dsh / pi / acp / …) │  │
│  │  ┣━ cmd, stdin/stdout pipes, rpc client                   │  │
```

---

## 1. ChatSession(per-chat)

### 字段分类

| 类别 | 字段 | 锁 |
|---|---|---|
| 路由状态 | `selectedCwd` / `selectedAgent` / `primaryAgent` | `cs.mu` |
| 路由状态(落盘) | 同上,经 `chatstore.Store` 写盘 | `cs.routeMu` 串行化写序 |
| 池 | `pool map[agentCwdKey]*AgentSession` / `selectedAS` | `cs.mu` |
| 队列 | `queue *MessageQueue` | queue 自带锁 |
| 事件总线 | `AgentEventBus` / `MessageStateBus` / `PromptEndBus` | 各自 `services.EventBus` |
| 出站 | `emitter messages.Emitter` | `cs.mu` |
| 开关 | `watchMode` / `thinkMode` / `toolsMode` | `cs.mu` |
| 共享 | `asPool *AgentSessionPool`(进程级 warm 池) | 注入 |

`primaryAgent` 是构造时的快照,构造后只读(Q-A:没有 `/default`,没有 per-chat 覆盖)。
`selectedAgent` 在 `New()` 时由 `primaryAgent` 播种,所以新 chat 不会看到空的 `selectedAgent`。

### 锁的顺序约定

`cs.mu` 是 ChatSession 状态锁。`cs.routeMu` 单独串行化"写路由字段 + 落盘"这一段,
避免并发的 `/cwd` / `/use` / `/watch` 让磁盘写入乱序(`docs/CHATSTORE.md`)。
任何路径都不同时持有两把锁。

### 生命周期

- **创建**:`Manager.GetOrCreate(chatID, primaryAgent)` —— 先查内存,miss 时从
  `chatstore.Store` hydrate,再 miss 才新建。落盘由 `Store.Bootstrap(chatID, primaryAgent)`
  负责(create-or-load)。
- **销毁**:chat 本身不销毁;`/close` 只关掉 pool 里的 AgentSession。
- **持久化**:路由字段经 `cs.routeMu` 落到 `chat_sessions.json`;AgentSession 元数据落在
  `agent_sessions.json`。

### /use 走的路径(切换 active AS)

1. `/use <agent>` 解析出目标 agent 名。
2. `LookupSelectedAgentSession(agent)` 在 `cs.pool` 里按 `(agent, cwd)` 找;命中就复用,
   miss 就 spawn 一个新的 AgentSession 并入池。
3. `cs.selectedAS` 指向新的 AS;旧的 AS 留在 pool 里(warm),不被杀。
4. `selectedAgent` 落盘。

**关键不变式**:切 AS 不切 chat —— findings / 出站消息仍然回到当前 chat 的 receipt。

---

## 2. AgentSession(per-(agent, cwd))

### 字段

| 字段 | 含义 |
|---|---|
| `ID` / `Agent` / `Cwd` | 身份;`(agent, cwd)` 1:1 唯一且 immutable |
| `opCtx` / `opCancel` | per-AS 的操作 ctx,见 §5 |
| `currentPrompt` | 当前 in-flight 的 `*Prompt`;没有就 nil |
| `events` | `chan agent.AgentEvent`,**单消费者** |
| `SessionID` | agent 侧自己的 session id(不是 nightme 的) |
| `model` / `exitCode` / `status` | 运行元数据 |

### 状态机

`StatusRunning` / `StatusDetached` / `StatusExited`。进程死了 → `StatusExited`;
`/new` 之类的 reset 会把 dead/detached 的 AS 标成 fresh 并清掉 `SessionID`。

### LookupSelectedAgentSession(选 AS 的核心入口)

1. 在 `cs.pool` 里按 `(agent, cwd)` 查。
2. 命中且 running → 直接返回。
3. 未命中 → 走 `Spawner` fork-exec 起一个新的 AgentSession,入池,返回。

spawn 在 `cs.mu` 之外做(外部回调不重入 manager 锁)。

### SendBlocks / SendText 的 ctx 处理

两个方法都从 `as.OpContext()` 派生 call ctx,而不是直接用调用方的 ctx ——
调用方(cmd 的 goroutine)的 ctx 可能比 AS 的寿命短。

---

## 3. Prompt(per-message 往返)

### Prompt 是什么

`agentsession.Prompt` 是"一条用户消息 → 一次 agent 往返"的状态载体:

| 字段 | 含义 |
|---|---|
| `Messages []Message` | 合并进这一轮的用户消息(有序) |
| `LastMessageID` | 本轮的 userMsgID 锚点 —— 所有 outbound event 的 `ReplyTo` 都取它 |
| `AgentSessionID` | 挂在哪个 AS 上 |
| `EndReason` | 终态原因 |

**锚点归属**:Prompt 挂在 **AgentSession** 上(不是 ChatSession),因为一个 Prompt 绑定到
一个具体的 AS,生命周期跟 AS 走。见 `docs/feat/F-53-message-prompt-lifecycle.md` §4.2。

### 三层各自对 turn 的关注点

| 层 | 关注 |
|---|---|
| ChatSession | 队列里的消息有没有被领走;`selectedAS` 是谁 |
| AgentSession | 当前 in-flight 的 Prompt 是哪个;bridge 事件怎么翻译成 AgentEvent |
| Prompt | 这一轮的锚点与终态 |

---

## 4. readPump 控制器

每个 AgentSession 有自己的 readPump(`internal/agentsession/readpump.go`),
**单消费者**地读 `as.events`:

1. 收到 event → 按 kind 分发:文本进 Prompt 的文本累加器,工具事件进 tool 缓冲,
   终态(`EventAgentDone` / `EventAgentError`)结束这一轮 Prompt。
2. channel 关闭(`!ok`)→ `as.endPrompt(agent.PromptEndProcessDied)` + 标记 `StatusExited`。
3. 事件到达会重置 idle 计时器(daemon 的 reaper 读它)。

事件缓冲在 AS 侧自治,ChatSession 的 readpump 不再是唯一的消费者。

---

## 5. ctx 传递链

### 创建

`New()` 预装一个可用的 `opCtx`(`context.Background()` 派生,无 cancel)。

### 激活

`as.Activate(parent)` 安装一个从 `parent` 派生的 `opCtx` + `opCancel`。
**幂等**:重复调用(例如 `/use` 重新提升同一个 AS)是 no-op —— 第一次 `Activate` 装的
`opCtx` 覆盖整个 AS 的寿命;再 `Activate` 会打断在飞的 `SendBlocks`,而这已经不需要了。

### 后台化

`as.Background()` 是 **no-op**:不取消 `opCtx`。AS 的 ctx 随它的寿命走,不由调用方决定。

### 取消

`as.Shutdown()` 取消 `opCtx` —— 从它派生的任何 bridge call ctx 一起取消(在飞的
`SendBlocks` 会被 `ctx.Canceled` 唤醒)。

### 外部做超时

调用方(cmd / runtime)自己给 call ctx 加超时;AS 的 `opCtx` 不被影响。

---

## 6. 关键路径的同步点总览

### "用户发 hi 到 pi"

1. adapter 收到消息 → `Channel.Incoming()`。
2. Gateway 的 pump → `Manager.HandleInbound` → `cs.QueueUserMessage(blocks)`。
3. `cs.LookupSelectedAgentSession()` 命中或 spawn。
4. `as.Submit(blocks)` → bridge `SendBlocks`。
5. bridge 的事件 → `as.events` → readPump → `cs` 的 event 回调 → `emitter.Send(out)`。
6. `out.ReplyTo = as.currentPrompt.LastMessageID` → Channel 渲染到对应的 receipt。

### "/use claude"

1. commander 解析 → `cs.LookupSelectedAgentSession("claude")`。
2. 命中 warm AS → `cs.selectedAS = as`;否则 spawn 新 AS。
3. `selectedAgent` 落盘。
4. 旧 AS 留在 pool 里(warm),它的 readPump 继续跑 —— 切 AS 不打断旧会话。

### "/close"

1. commander → `internal/command/close` 的 `CloseAllAgents`。
2. 拿 `cs.AgentSessionsForCwd(cs.SelectedCwd())` 的快照。
3. 每个 alive 的 AS 走 bridge 的 graceful `Close()`(bridge 自己的 SIGINT → SIGKILL 兜底);
   已经死了的标 `stale-cleared`。
4. 汇总成 `[]close.Result` → `close.FormatResults` → 回一条 per-entry 列表。
5. pool 本身不动 —— respawn 还能续上每个会话。

---

## 7. 死锁 / 风险面盘点

| 风险 | 现状 |
|---|---|
| `cs.mu` 嵌套 | 不嵌套。构造 AgentSession / 落盘都在锁外做 |
| `cs.routeMu` 与 `cs.mu` | 不同时持有 |
| AgentSession 的 `as.mu` 与 `cs.mu` | AgentSession 的锁在 cs.mu 之外获取,不反向 |
| bridge 阻塞 | bridge 调用走 AS 的 call ctx,AS 关闭时会 cancel |
| 事件背压 | `as.events` 有缓冲;producer 侧由 readPump 消费 |

---

## 9. 一句话总结

ChatSession 持有"这个 chat 现在指向哪个 workspace / 哪个 agent / 哪些 AgentSession";
AgentSession 持有"这个 (agent, cwd) 进程现在活着吗、opCtx 是什么、当前 turn 的锚点在哪";
Prompt 持有"这一轮的用户消息与锚点"。三层之间只通过 `cs.mu` 保护的状态和
`as.events` 这条单消费者通道同步。
