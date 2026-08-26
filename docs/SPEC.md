# nightme — Technical Specification (SPEC)

> **作者**：🦞 虾哥（PM/Architect）
> **文档层级**：技术级（**不含实现细节 / 代码**）
> **关联文档**：
> - 产品定位 → [`PRD.md`](./PRD.md)
> - 功能索引 → [`FEATURES.md`](./FEATURES.md)
> - 每个 feature 的详细实现（含代码）→ [`feat/`](./feat/)
> - 职责隔离架构 → [`feat/F-gateway.md`](./feat/F-gateway.md)

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
│  │  • slash command 路由(走 Commander,见下)│            │
│  │  • reaction 路由(走 ReactionRouter,不经过 ChatSession)│ │
│  │  • ChatSession 生命周期管理 (Create / Restore)       │    │
│  │  • Channel ↔ ChatSession ↔ AgentSession 跨层调度     │    │
│  └──────────┬────────────────────────┬─────────────────┘    │
│             │                        │                       │
│             │      ┌─────────────────┴─────────────┐         │
│             │      │  Slash Command Layer         │         │
│             │      │  internal/command/           │         │
│             │      │  Commander 接口 + 各命令 Factory │       │
│             │      │  (/cwd /use /close /new /watch /│       │
│             │      │   /think /tools /gtw)        │         │
│             │      │  通过 RuntimeServices 访 service│       │
│             │      └─────────────────┬─────────────┘         │
│             │                        │                       │
│   spawn /   │                        │                       │
│   reuse /   │                        │                       │
│   kill      │                        │                       │
│             ▼                        ▼                       │
│  ┌─────────────────────┐  ┌─────────────────────┐           │
│  │  ChatSession (池)   │  │  AgentSession 池    │           │
│  │  ───────────────── │←→│  ─────────────────  │           │
│  │  selectedCwd          │  │  (claude, /A) 进程   │           │
│  │  selectedAgent        │  │  (codex,  /A) 进程   │           │
│  │  primaryAgent       │  │  (claude, /B) 进程   │           │
│  │  InputBuffer FSM    │  │  (codex,  /B) 进程   │           │
│  │  (idle ↔ busy)      │  │                      │           │
│  │  currentPrompt       │ │  1:1 with (agent,cwd)│           │
│  │  .LastMessageID      │ │  immutable 标识       │           │
│  │  (单数, 跟踪变量)   │  │  immutable 标识       │           │
│  └─────────────────────┘  └─────────────────────┘           │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

**当前行为**：Gateway 不再持有 "slash command 路由" 的具体实现（不再直接 import `handlers_*.go`）。slash command 路由通过 `Commander` 接口（住 `internal/command/`）间接完成；reaction 路由通过 `ReactionRouter` 接口间接完成。**Gateway 不 import chatsession 的具体类型**，只看 ChatSession 接口或 runtime 注入的 callback。

### 1.1 七个逻辑组件

| 组件 | 职责 | 它**不知道** |
|------|------|------------|
| **Channel Adapter** | IM 协议编解码；`Send(OutboundMessage)` 渲染；**自管** receipt card / thread / DOM 节点的完整生命周期（含 cold-create / PATCH / 终态） | ChatSession、AgentSession、workspace、agent、binding、任何渲染状态 |
| **Gateway** | 中枢 orchestrator：slash command 路由（**通过 Commander 抽象，不直接 import handlers_*.go**）、binding 表（chat_id ↔ ChatSession）、ChatSession 生命周期、Channel↔ChatSession↔AgentSession 跨层调度、outbound 路由（stamp `ReplyTo=userMsgID` 送到对应 Channel）、**多 channel 并行 pump（每 channel 一个 pump goroutine → 共享 inbound.Router 派发到 per-channel chatsession.Manager）** | IM 协议细节、agent 内部协议、PTY/ACP 细节、Channel 内部渲染状态、命令实现细节（具体命令实现住 `internal/command/<name>/`）、**chatID→channel 路由表**（partition 由 pump goroutine 闭包隐式决定） |
| **Slash Command Layer**（Commander）住 `internal/command/`，由 `SlashCommandFactory` 装配 | 命令抽象层：定义 `Commander` / `SlashCommandFactory` / `SlashRegistry` / `RuntimeServices` 接口；声明 `ReactionRouter` 服务接口；提供通用 `preflight` / `reply` helper。各命令子包（`internal/command/<name>/cmd.go`）实现 `SlashCommandFactory`，**Factory 不持 mgr 字段**——`Handle(ctx, mgr, input)` 接受 mgr per call（v1.3+ 多 channel 下 mgr 不可绑死）；shared `command.Registry` 跨所有 channel 共享 | 命令的具体业务逻辑由各 `<name>` 子包实现；command 包本身**不** import gtw / gateway / channel 具体类（chatsession 是命令 Factory 的直接依赖） |
| **ChatSession** | per chat 的会话上下文（持久化）：selectedCwd / selectedAgent / primaryAgent / InputBuffer FSM / AgentSession 池索引（注意：单 turn userMsgID 锚点 F-53 起迁移至 `AgentSession.currentPrompt.LastMessageID`，不再由 ChatSession 持有） | chat_id 之外没有"自己是谁"；Channel 协议细节；agent 内部协议；receipt 渲染；**slash command 详情；任何命令包路径；gtw / command / gateway 任何具体类型的名字**（强化） |
| **AgentSession**（住 `internal/agentsession/`）| CLI 进程句柄；`(agent, cwd)` 1:1 唯一标识（immutable）；events() chan；sendText/sendBlocks；close | chat_id、ChatSession、binding、Channel、slash command |
| **Bridge** | nightme 与底层 AI Coding CLI 之间的通信抽象；`AgentSession` 接口（Events / SendText / SendBlocks / SendPermission / Close）；四种模式（ACP / SDK / PTY / JSON-IO） | chat、binding、ChatSession、Channel |
| **Process Registry** | JSON 持久化层。两类 entry：`ChatSessionEntry`（chat_id ↔ ChatSession 绑定 + selectedCwd/selectedAgent/primaryAgent + AgentSession 索引）+ `AgentSessionEntry`（agent + cwd + pid + status）| 运行时语义；只持久化 |

### 1.1a 包结构（refactor-agentsession）

`AgentSession` 在 期间从 `internal/chatsession/` 提取到独立的 `internal/agentsession/` 包。语义不变（仍是一等运行时单元 + 桥层 handle 持有者 + EventBus 拥有者），但分层更清晰：

```
internal/agent/         抽象:AgentSpec / Agent / Bridge / Mode / ContentBlock / Info / Starter
                        ← 不依赖任何运行时
internal/agentsession/  运行时单元:AgentSession + Prompt + EnrichedEvent + Spawner + Status
                        ← 持有 *agent.Agent 句柄;EventBus 是 per-AS 私有
internal/chatsession/   池管理:ChatSession + Manager + Channel (interface) + queue + persistence
                        ← 持有 *agentsession.AgentSession 池
```

依赖方向严格自上而下（agentsession → agent, chatsession → agentsession → agent），无环。

CS 通过 type aliases 暴露 AS 类型保持调用面简洁（`chatsession.AgentSession` = `agentsession.AgentSession`）。

### 1.2 三状态机，三个 owner

核心架构不变式——任何状态机都**只有一个** owner，跨层状态机之间**没有循环依赖**：

| 状态机 | Owner | 状态空间 | 持久？ |
|--------|-------|----------|--------|
| **Binding FSM**（chat ↔ ChatSession）| Gateway | 1:1 绑定，永不删 | 是（ChatSessionEntry）|
| **InputBuffer FSM**（per ChatSession）| ChatSession | `idle ↔ busy` | 否（重启丢）|
| **MessageState FSM**（per userMsg）| ChatSession | `MessageQueued → MessageSubmitted → MessageDropped`（F-53 之前是 4 态 `Received/Forwarded/Done/Failed`，Done/Failed 已删）| 否（重启丢）|
| **PromptState FSM**（per userMsg）| Channel's receipt | `PromptRunning → PromptDone`（F-53 之前 4 态，已收为 2 态；Pending/Succeeded/Failed 已删）| 否（重启丢）|
| **AgentSession.Status**（per AgentSession）| AgentSession | `running → detached / exited` | 是（AgentSessionEntry）|
| **ChatSession.SelectedAgentSession**（per ChatSession）| ChatSession | 引用 pool 中的某个 AgentSession | 引用在 ChatSessionEntry |

**非 FSM 跟踪变量**：

| 变量 | Owner | 语义 |
|------|-------|------|
| **`currentPrompt.LastMessageID`**（per AgentSession，F-53 起）| AgentSession | 当前 turn 的单一 userMsgID 锚点。F-53 前为 `cs.currentTurnUserMsgID` 字符串标量；F-53 后改为 `as.currentPrompt.LastMessageID`（挂在 `Prompt` 一等公民上，详见 [`feat/F-53-message-prompt-lifecycle.md`](./feat/F-53-message-prompt-lifecycle.md) §4.2）。所有 outbound event 的 `OutboundMessage.ReplyTo` = LastMessageID。buffered batch 时锚到 batch 最后一条 |

**核心 FSM / 跟踪变量的耦合点**（全部经过 Gateway）：
- **Inbound 流**：Channel → Gateway.pumpInbound → dispatchLoop → DispatchInbound (inboundDispatcher) → 命中 `/cwd` `/use` `/close` 走 slashCommandDispatcher → 走 ChatSession；未命中走 messageDispatcher → `cs.emitMessageState(Received)` + `chatSession.QueueUserMessage` + `chatSession.LookupSelectedAgentSession`（lazy spawn）+ `agentSession.SendBlocks` + `cs.emitMessageState(Forwarded)`
- **Outbound 流**：`agentSession.Events()` → session 的 readPump（**单消费者**，F-53 起读的是 AS 自治的 `eventQueue`/`EventBus`） → ChatSession.EventCallback → 设 `out.ReplyTo = as.currentPrompt.LastMessageID`（F-53 前为 `cs.currentTurnUserMsgID`） → `channel.Send` → Channel 内部按 ReplyTo 路由到对应 receipt（card / thread / DOM 节点） → PATCH
- **切 AgentSession**：`/use` 触发 → ChatSession.LookupSelectedAgentSession 重新解析 → 切换 ChatSession.EventCallback 目标 → 老 AgentSession 的事件不再消费

### 1.3 不变式

- **`ChatSession` 不 import `channel/feishu`**（事实上根本不 import `channel/` 包）
- **`AgentSession` 不 import `channel/` 也不 import `ChatSession`**（纯进程句柄）
- **Gateway 不 import `channel/feishu`**（只 import `channel.Channel` interface）
- **`ChatSession` 不知道 Channel**（只持有 Gateway 注入的 callback）
- **`ChatSession` 不知道自己是哪个 channel**（无 `channelName` 字段，持久化 schema 零变更）——channel 归属由"该 ChatSession 落在哪个 chatsession.Manager"隐式决定
- **`AgentSession` 知道自己的 `(agent, cwd)` immutable 标识**
- **多 channel 并行接入**（v1.3 起）：所有已 login 的 channel 自动启动，无需 `--channel` flag；接入新 channel = 1 个 adapter 文件 + 1 个 `init()` 注册到 `channel.Registry`；runtime / gateway / chatsession / command 零修改
- **Per-channel `chatsession.Manager`**：每个 channel 一个 `Manager` 实例，`sessions map[chatID]*ChatSession` 装该 channel 的 chat；`Manager.emitter` 是该 channel 的 `outbound.Emitter`（= channel 自己，无 router 无 multi 概念）
- **Per-channel `outbound.Emitter` 单 channel 单一对象**：`outbound.New(ch, opts)` 直接拿 channel 构造；ChatSession.emitter 在 Manager 构造时绑定，ChatSession 继承；出站路径 `cs.emitter.Send(msg)` = `ch.Send(msg)`，**无 routing 表、无查表、无多 channel 概念**
- **Restore 懒加载**：daemon 启动时**不**调 `Manager.RestoreFromRegistry`；`Manager.GetOrCreate(chatID)` 在 in-memory miss 时走 `csFile.GetByChat(chatID)` 命中即 hydrate（恢复 selectedCwd/Agent/WatchMode/ThinkMode/ToolsMode + AgentSession 池 Detached），miss 才 `New`。`Manager.RestoreFromRegistry` 方法保留（21 个测试 caller 不破）但 runtime 路径不调
- **入站 partition 由 pump goroutine 闭包隐式决定**：`gw.pumpOne` 闭包 capture `(channel, mgr)`，dispatch chain 用 mgr per call；新 chatID 落到产生它的 channel 的 Manager；chatID 跨 channel 不撞（每 channel 用自己的 prefix 隔离：`tg_<digits>` / `oc_<hex>` / 各平台 namespace；详见 docs/CHANNEL.md §5.5）
- **共享 components 跨 mgr 查找**：`gtw.Manager` / `gitStatusLookup` / `command.Registry` 共享，通过 `runtime.findChatSession(chatID)` 跨 `runtime.allMgrs` 线性扫（N=2-3 时 O(N) 无压力）；这些组件无 `mgr` 字段，handle 由 dispatch chain 透传
- **gw 持 `[]gateway.Pump`**：每个 `Pump = { Channel channel.Channel; Manager *chatsession.Manager }`；删 `chatToChan` / `defaultChannel` / `channelCh` / `pumpInbound` / `dispatchLoop` / `resolveChannel` 等冗余 routing 表（partition 隐式，不再需要查表）
- **Channel 接口不暴露 ChatSession、AgentSession、binding、任何 receipt 概念**——Channel 自管渲染状态（receipt card / thread / DOM 节点），Gateway 一概不知。`Channel` 的唯一出站方法是 `Send(OutboundMessage)`；交互卡相关是 Channel 私有的（`Choice.RequestID` → 平台 message id），调用方不拿 bot-side message id，也不另开 `SendCard` / `SendAction`
- **`agentSession.Events()` chan 的唯一消费者是 session 自己的 readPump**；ChatSession 通过 `ChatSession.EventCallback` 接收事件，**不直接读 chan**（沿用 修复）
- **ChatSession 内 `(agent, cwd)` 唯一索引**（不是全局唯一；不同 ChatSession 可有独立 `(claude, /path/A)` AgentSession）
- **`/use` 不重启进程**：永远复用 pool 中的现有 AgentSession，找不到才 spawn 新进程
- **`/cwd` 不重启任何 AgentSession**：永远只改 selectedCwd，老 AgentSession 保留在 pool
- **`currentPrompt.LastMessageID` 单数（F-53 起）**：一个 turn 一个 userMsgID 锚点（来自 `as.currentPrompt.LastMessageID`）；buffered batch 时锚到最后一条；outbound event 的 `ReplyTo` 必等于此。F-53 前的字符串标量 `cs.currentTurnUserMsgID` 已删除
- **抽象归抽象 / 具体归具体**：Gateway = 路由器（不假设任何 Channel 渲染细节）；Channel = 渲染器（自管 receipt 生命周期，自选存储形态：per-chat map / per-thread map / DOM 节点 / ...）
- **outbound 路由唯一耦合点**：`EventHandler` 在每个 `OutboundMessage` 上设 `out.ReplyTo = as.currentPrompt.LastMessageID`（F-53 起；F-53 前为 `cs.currentTurnUserMsgID`）。Channel 据此 key 路由。Gateway 不需要知道 Channel 内部 receipt 怎么存
- **Task checklist 三层状态隔离（F-38）**：Claude bridge 持有 provider-session 的规范化 task map/order，并在成功 tool_result 后发完整 snapshot；Gateway 无状态 typed 透传；Channel receipt 只保存当前 userMsgID 的展示副本。任何一层都不得反向读取另一层状态

- **Gateway 不持有 ChatType**：Gateway 只见 `chat_id string`，假设所有 chat 同质；`InboundMessage` / `BindingEntry` / `ChatSession` / registry schema 都不带 `ChatType` 字段
- **Channel 自管 chat 语义**：Channel 知道 chat 类型（DM / group / topic）、知道 thread 渲染，但只通过 `OutboundMessage` 暴露渲染能力，不污染 Gateway / ChatSession / Registry 数据模型
- **`InboundMessage.ReplyTo = message.ParentId`**（Feishu 语义下）：thread 顶层 `RootId` 不进 nightme；`ReplyTo` 字段统一语义 = "被 reply 的那条 message_id"
- **`OutboundMessage.ReplyTo = as.currentPrompt.LastMessageID`**（F-53 前为 `currentTurnUserMsgID`）：bot reply 永远 anchor 到 user 当前 message_id，不爬 thread 树

- **`Message.HasMention` 由 Channel 计算，Gateway 不重复算**：channel adapter 读 `message.Mentions` + `chat_type` + `GetBotIdentity()` 拿 bot open_id 算 `HasMention`（DM 永远 true；group 含 bot/@_all 时 true）；Gateway 只 trust 这个 bool
- **`ChatSession.WatchMode` 决定 group 内非 mention 消息是否 drop**：默认 `WatchModeMention`（drop），`/watch on` 切 `WatchModeAll`（pass）；DM 下 `/watch` 为 no-op
- **drop 决策留在 Gateway**：channel 不读 `cs.WatchMode()`，gateway dispatcher 入口统一 gate `!HasMention && WatchMode != All`
- **不续接 Thread**：nightme 不主动追踪 / 创建 thread 上下文，不维护 thread 树；Feishu 端 thread 视觉由 Channel 自治
- **任何 Channel 都不引入 thread 概念**：nightme 数据模型永远不引入 thread 字段（`thread_ts` / `message_thread_id` / `is_threaded` / `thread_id` 等）。Channel 自管 thread 渲染细节（Feishu reply API path 参数 / Slack block kit / Telegram forum mode），但只通过 `OutboundMessage` 暴露能力，不污染 Gateway / ChatSession / Registry 数据模型

- **Channel 按 OutboundKind 自决渲染目标**：Channel 拿到 `OutboundMessage{Kind, ReplyTo, Text, Meta}` 后，**可以**按 `Kind` 自决 routing（thread reply / receipt card / reaction / ...），无需 Gateway 指示。F-thread-route 案例：Feishu adapter 把 `OutThinking` / `OutToolStart` / `OutToolEnd` / `OutCompaction` 投到 thread；`OutText` / `OutResult` / `OutInit` / `OutUsage` 投到 receipt card。**不动 OutboundMessage 契约 / Gateway / ChatSession**
- **OutToolEnd 类型感知摘要 = Channel 职责**：bridge 层把 `ToolEndEvent.Args` 填好（同一 message 的 tool_use block 拿）；Channel 层（Feishu adapter）按 tool name 生成单行摘要（"📄 Read /foo.go → 1234 lines"），不 dump 原始 output。摘要算法属于 Channel 自治（Feishu 用 emoji + 行数；Slack 可用 Block Kit；Web 可用折叠 div）
- **Routing 决策不写进 OutboundMessage.Meta**：Meta 只承载数据载荷（output / err / args），**不**承载 routing hint。Channel 看到 Kind 后自决。
- **F-thread-route 不构成"thread 概念侵入 nightme 数据模型"**：Channel 自管的 thread 是 Feishu SDK API 调用层面的细节（`POST /im/v1/messages/{rootID}/reply`）；nightme 仍然只见 `OutboundMessage.ReplyTo = as.currentPrompt.LastMessageID`（F-53 前为 `currentTurnUserMsgID`），跟 F-33 不变式完全兼容

- **`[]agent.ContentBlock` 是有序 composite**：单一 `ContentText` block 的 `Text` 字段仍是 String，但**承载组合能力的是整个 slice**——`Type ∈ {text, image, file}` 的元素按用户视角的顺序排列。这是对应 Anthropic API `content[]` heterogeneous array 的 1:1 数据结构,不能用 String-with-placeholder 替代（解析歧义 + 类型丢失 + 协议弱化）
- **`InboundMessage.Blocks` 仅 post 富文本非空**：`msg_type=post` 时 `extractAttachments` 返回 `Blocks=ordered-slice`,`Text=""`；其他 msg_types 走 `BuildBlocks(text, atts)` 路径,`Blocks=nil`。`Attachments` 持下载候选 file_key 列表,`Blocks` 持用户视角的有序 turn 形态——两者职责清晰,不冗余
- **`blocks` 顺序 end-to-end 保留**:从 `extractAttachments`(Feishu adapter) → `resolveBlocks`(下载后回填 Path) → `InboundMessage.Blocks` → `messageDispatcher` 选 `msg.Blocks` → `ChatSession.QueueUserMessage(blocks)` → `AgentSession.SendBlocks(blocks)` → bridge 编码到 `content[]` 数组,**每个层都不重排**。任何"先 text 后 image"的拍扁都是顺序 bug
- **path 字段在抽象层只持本地路径**:`ContentBlock.Path` 永远是绝对文件系统路径,**不**存 base64 / 不存 file_key / 不存 URL。base64 inflate 严格限制在 bridge 边界(`bridge/claudecode/session.go::SendBlocks` 的 `readFileAsBase64`)。这是 §1.4 "boundary normalize" 的具体落地:抽象层只持 primitive generic,concrete 编码细节留在具体实现层
- **失败 block omit,不放 placeholder**:post 富文本里某张图下载失败时,`resolveBlocks` 把对应 `ContentImage` block 从 slice 中剔除,**不**用占位符替换(避免 Claude 把"半截 array"误读为"用户传了 3 张图但其中 1 张是 placeholder")。text 上下文保留
- **`BuildBlocks` 顺序契约**:单资源消息(text+image/file)走 legacy 路径,blocks 顺序固定为 `[ContentText(caption)?, ContentImage×N, ContentFile×M]`。这条契约隐式被 单测覆盖,channel 实现应遵循

- **`ChatSession` 不 import `command/`、`internal/gtw`、`internal/gateway`**：chatsession 包只 import `agent` + `registry` + stdlib + `embed`。它是纯服务包，从今以后任何上层抽象（command 层、gateway）的名字都不许出现在 chatsession 包内。之前 chatsession 持有 `gtwContext` / `gtwDrafts` 字段、`SetActionHandler` / `HandleAction` 方法、整套 GTW* accessor；后**回归 §1.3 的"中立 session 持有者"**。
- **`command/` 包（`internal/command/` 及子包）不 import `chatsession` / `internal/gtw` 等具体类**：slash command 子包只 import `command/` 抽象层 + stdlib。adapter / 具体实例化放 `cmd/nightme/`。违反这条会让"command ↔ chatsession"反向耦合又出现，正是 要消除的。
- **Reaction 分发器变更**：`ChatSession.SetActionHandler` / `HandleAction` 整套 API 删除。reaction 路由改走 `command/services.ReactionRouter` 接口（住 `internal/command/services/reaction.go`），runtime 装配。gtw 反应在 `cmd/nightme/run.go` 启动时 `router.Register(gtwMgr.HandleReaction)` 注册。
- **`Command` 接口与注册协议抽象**：所有 slash command 必须通过 `SlashCommandFactory` 接口装配；不允许 `gateway.Register(Command{...})` 命令级注册（兼容期可双轨，但后段必须切到 factory 装配）。
- **ActionHandler 接口不暴露**：任何代码里出现 `SetActionHandler` / `HandleAction` / `ActionHandler()` 都是 review 时直接打回。
- **§1.4 抽象 / 具体边界规范的 4 层扩展**：原 3 层（concrete / abstract / concrete）补全为 4 层：
  ```
  Concrete impl (chatsession / gtw / channel)
       ↑ implements
  Service interface (command/services/*.go)
       ↑ implements
  Command abstraction (command/<name>/cmd.go + Commander/SlashCommandFactory)
       ↑ uses
  Gateway + Channel top layer
  ```
  依赖箭头**单向向下**。下层 import 上层即破不变式。

### 1.4 抽象 / 具体 边界规范

跨层通信的架构纪律。本节是一切不变式之上的元原则——其它不变式违反时，几乎都是这条先破了。

**规则（一句话）**：
> 抽象层只承载泛化统一的概念。底层具体实现的细节不得直接引入抽象层。如果某项具体信息确实需要进入抽象层，必须先在 boundary 处归一化（normalize）为泛化形式后才能跨越边界。这是软件工程中多态的核心思路。

**为什么**：每条跨层 hardcoded implicit 协议都是一次 leak。一旦 leak，后续 review 容易"基于现状优化"（typed struct、helper）导致边界进一步塌陷。设计上能站得住的唯一办法是：每条跨层数据要么是 generic primitive（string / int / error），要么 boundary 把它 normalize 成 generic primitive。

**三层边界 + 各自的归一化义务**（跨通用消息流；command 层另有 4 层扩展，见 §1.3 末段）：

```
┌─────────────────────────────────────────────────────────────┐
│ Concrete (bridge / Channel SDK)                              │
│   claudecode stream-json  ·  pi typed map  ·  Feishu API    │
└───────────────────────┬─────────────────────────────────────┘
                        │ ← 归一化边界 #1: bridge → agent
                        │   bridge 把 native representation
                        │   转成 human-readable string
                        ▼
┌─────────────────────────────────────────────────────────────┐
│ Abstract (agent / Gateway)                                    │
│   agent.AgentEvent  ·  gateway.OutboundMessage             │
│   只见到 generic type：string / int / float64 / error /    │
│   typed struct with generic field names                     │
└───────────────────────┬─────────────────────────────────────┘
                        │ ← 归一化边界 #2: Gateway → Channel
                        │   typed field (ToolInfo) 替代
                        │   Meta map 的 implicit key
                        ▼
┌─────────────────────────────────────────────────────────────┐
│ Concrete (Channel adapter)                                   │
│   Feishu  ·  Slack  ·  Web  ·  ...                          │
│   Channel 自管渲染，可 parse ToolInfo.Args 字符串为 typed    │
└─────────────────────────────────────────────────────────────┘
```

**规则的具体落地**：

1. **抽象层的字段名必须 generic**：禁止用 `file_path` / `command` / `content` 这种 bridge-specific schema 名字。任何 tool 都有 `Name/Args/Output/Err`，这是 generic 概念。
2. **抽象层的字段类型首选 primitive**：`string` / `int` / `float64` / `error`。**不要**为了"类型安全"在抽象层引入 typed struct / enum——那等于在抽象层 hardcode 一种 concrete 的 shape。
3. **bridge 是归一化边界 #1**：claudecode 用 raw JSON string 表达 args；pi 可能用 typed map → 也归一化成 string；pty 可能 raw bytes → 也归一化成 string。Gateway / Channel 只见到 string，不关心 bridge 内部 schema。
4. **Channel 拿到 string 后自己 parse**：如果 Channel 想做类型感知渲染（"Read /foo.go → 1234 lines"），它 parse `ToolInfo.Args` 字符串。但 parse 逻辑属于 Channel 自治，不进 Gateway / agent。
5. **禁止 `OutboundMessage` 上的 `Meta map[string]any`（已删除）**：Meta 是 opaque data container，consumer 不知道里面有什么，producer 也不知道 consumer 会读哪些 key。跨层契约只能走 typed field。`OutboundMessage` 当前 100% typed（§1.4），任何新增跨层数据必须走 typed struct / primitive，不能 re-introduce Meta。
6. **数值类除外**：`CostUSD` / `*Tokens` 保留 typed numeric——任何 agent 都有 token / cost 概念，不需要 string 化（但**走 typed field，不走 Meta**）。
7. **任务概念必须先归一化**：`TaskCreate.subject` / `TaskUpdate.taskId` 等 Claude-native 字段只能存在于 `bridge/claudecode`。跨过 boundary 后统一为 `TaskListEvent{Items: []TaskItem{ID, Subject, ActiveForm, Status}}`；Gateway 只做 `EventTask* → OutTask*` typed 映射，Channel 自决 checkbox / glyph / DOM 渲染。

**反例（2026-08-04 §1.4 终极落地）**：

`OutboundMessage.Meta` 字段已被删除（§1.4）。Meta 原本持有 11 个 implicit key（tool_name / args / output / err / is_error / subtype / state / message_id / reaction_id / session_id / model / agent_name / workspace / branch / input_tokens / output_tokens / cost_usd），全部 hardcoded 隐式协议。下面是 GateWay translate 曾经的 代码反例（已全部移除）：

```go
// ❌ 反例（已删除）: Gateway 的 translate 把 bridge-specific 字段名写进 Meta
case agent.EventToolEnd:
    return OutboundMessage{
        Meta: map[string]any{
            "tool_name": name,  // Feishu adapter 用的隐式 key
            "args":      ev.ToolEnd.Args,
            "output":    ev.ToolEnd.Output,
            "err":       ev.ToolEnd.Err,
        },
    }
```

```go
// ❌ 反例（已删除）: Channel 从 Meta 隐式 key 读 concrete 数据
func toolName(m gateway.OutboundMessage) string {
    if n, _ := m.Meta["tool_name"].(string); n != "" {
        return n
    }
    return "tool"
}
```

```go
// ❌ 反例（已删除）: Channel 反向重建 typed event
func durationMs(m gateway.OutboundMessage) int64 {
    return int64(metaInt(m, "duration_ms"))
}
// 然后 receipt.Append(ctx, agent.AgentEvent{Result: &agent.ResultEvent{DurationMs: durationMs(msg), ...}})
// ——Gateway 拆字段塞 Meta，Channel 从 Meta 读回来重建 typed event 再喂给 receipt，纯轮转。
```

**正例（最终状态）**：

```go
// ✅ 正例: Gateway translate 用 typed field 归一化 tool 概念
case agent.EventToolEnd:
    return OutboundMessage{
        Tool: &ToolInfo{
            Name:   name,
            Args:   ev.ToolEnd.Args,
            Output: ev.ToolEnd.Output,
            Err:    ev.ToolEnd.Err,
        },
    }
```

```go
// ✅ 正例: Channel 直接读 typed field
func toolName(m gateway.OutboundMessage) string {
    if m.Tool != nil && m.Tool.Name != "" {
        return m.Tool.Name
    }
    return "tool"
}
```

```go
// ✅ 正例: typed Result 直接传给 receipt.Append，零 round-trip
return receipt.Append(ctx, agent.AgentEvent{
    Kind:   agent.EventResult,
    Result: msg.Result,  // *agent.ResultEvent directly, no Meta reconstruction
})
```

**违反这条规则时的征兆**（review 时用作 checklist）：
- 抽象层 struct 里出现 `Meta map[string]any` 字段且 producer / consumer 各自 hardcode key 名（**已修复**——Meta 删除）
- 抽象层 field 名字含具体 schema（`file_path` / `command` / `pid` 等）
- 抽象层 field 类型是 `*SomeConcreteBridgeStruct`（concrete 类型漏到抽象层）
- Gateway / agent 包 import 了 channel 包（直接依赖关系）
- 文档里出现"key 由 Channel X 约定"——这等于承认 implicit 协议存在
- Gateway 把 typed event 拆字段塞 Meta / channel 又读 Meta 重建 typed event——纯轮转，无价值

**例外与升级路径**：

如果某个 concrete 实现真的需要向抽象层暴露一项数据（且无法在 boundary 归一化），按以下顺序升级：
1. 先问：能否不暴露？channel 自治自己读 SDK 不行吗？
2. 再问：能否在 boundary normalize 成 primitive？
3. 再问：能否抽成 typed struct（generic 字段名）？
4. 最后：扩展抽象层字段，并在 §1.3 不变式里明文记一条。

升级路径 #4 是最后一根稻草。F-37 review 把 `Meta["args"]` 升级到 `OutboundMessage.Tool *ToolInfo` 就是走的路径 #3（typed struct），不在抽象层暴露 schema。

---

## 2. 数据流（概念）

### 2.1 用户消息 → CLI 输入（Inbound）

```
IM 消息事件
  → Channel Adapter 解码为统一 InboundMessage{chat_id, user_id, text, attachments, blocks, message_id, reply_to, time, has_mention}
      ├─ reply_to = message.ParentId（F-33：Feishu 原生 parent_id，thread 顶层 RootId 不进 nightme）
      ├─ has_mention（F-watch：DM 永远 true；group 含 bot/@_all 时 true；由 channel adapter 根据 Mentions + chat_type + GetBotIdentity 计算）
      ├─ **同步下载 attachments 到本地路径**（F-14：publish 前必须填 LocalPath）
      │     ├─ 全失败 → ch.Send("❌ N attachments failed…") + return（不进 ch.Incoming，Agent 看不到这条）
      │     ├─ 部分失败 → ch.Send("⚠️ K of N failed; sending the rest") + 继续
      │     └─ 全部成功 → 静默继续
      ├─ **post 富文本**：`extractAttachments` 产出有序 `[]agent.ContentBlock`（blocks），image 节点占位 file_key → 下载后 resolve 回填 LocalPath（F-14）
      │     - 单资源消息（image/file/audio/media）：`blocks == nil`，走 legacy `BuildBlocks(text, atts)`
      └─ publish 到 ch.Incoming()

Gateway.pumpInbound (per-channel)
  └ push 到 channelCh

Gateway.dispatchLoop
  └ Handle(ctx, msg)
     ├ ParseCommand(msg.Text)
     │   ├ 命中 (/cwd /use /close /watch /help /agents) → handler(msg)
     │   │   └ handler 走 gateway.bindings → chatSession.xxx → reply via channel.Send
     │   └ 未命中 / 普通文本 → messageDispatcher(ctx, msg)
     ├ F-watch gate(F-watch 位置:Handle 入口在 dispatchLoop → slashCommandDispatcher 前)
     │   ├ msg.HasMention == true → pass-through(原有路径)
     │   ├ msg.HasMention == false 且 cs.WatchMode() == WatchModeAll → pass-through
     │   └ msg.HasMention == false 且 cs.WatchMode() == WatchModeMention(默认) → drop(F-watch:不发 ack,静默跳过)
     └ slashCommandDispatcher 走 handler；messageDispatcher 走 ChatSession（详见 §2.2）

messageDispatcher(ctx, msg)
  ├ gateway.bindings[msg.chat_id] 查 ChatSession
  │   ├ nil → channel.Send("no chat session, /cwd first")
  │   └ Status != Ready → channel.Send("not ready, /cwd + /use first")
  ├ (0) cs.emitMessageState(msg.MessageID, agent.MessageReceived)   ← F-31: 触发 MessageState(Received) 事件
  ├ (a) chatSession.LookupSelectedAgentSession() (lazy spawn on miss)
  ├ (b) cs.emitMessageState(msg.MessageID, agent.MessageForwarded)   ← F-31: dispatch 成功后触发
  ├ (c) **blocks 路径选择**（F-14）：
  │     ├─ msg.Blocks != nil（post path）   → blocks = msg.Blocks    ← 顺序由 Feishu paragraph 决定
  │     └─ else（single-resource）   → blocks = feishu.BuildBlocks(msg.Text, msg.Attachments)
  ├ (d) chatSession.QueueUserMessage(blocks, msg.MessageID)
  │       InputBuffer FSM:
  │         ├ Idle → 立即 SendBlocks(blocks) → return (dispatched=true)
  │         │   └ onFlush 钩子（ChatSession.defaultFlushHookLocked）触发:
  │         │       ├ as.currentPrompt.LastMessageID = msg.MessageID(F-53;前为 cs.currentTurnUserMsgID)
  │         │       └ agentSession.SendBlocks(combined)
  │         └ Busy → 入队 → return (dispatched=false)
  └ 如果 queued (Busy):
        as.currentPrompt.LastMessageID 不变(仍是上一 turn 的值;F-53 前为 cs.currentTurnUserMsgID)
        onFlush 钩子在 EventDone 触发 flush 时:
          ├ onFlush(blocks, userMsgIDs)
          │   ├ as.currentPrompt.LastMessageID = userMsgIDs[len-1](最后一条;F-53 前为 cs.currentTurnUserMsgID)
          │   └ agentSession.SendBlocks(combined)
          └

**当前行为（F-14）**：- `Channel Adapter` 在 `handleMessage` 内**同步**调 `DownloadAttachments`，确保 `InboundMessage.Attachments[i].LocalPath` 在 publish 前已填好。 该函数未被生产代码调用过（仅单测），导致所有 attachment 在 `BuildBlocks` 的 `LocalPath == ""` 分支被静默 skip。
- `post` 富文本按 paragraph node 顺序产出 `[]agent.ContentBlock`（`msg.Blocks`），`messageDispatcher` 优先使用，非 post 走 legacy `BuildBlocks(text, atts)`。单资源消息（image/file/audio/media）的 Text + Attachments 模型不变。
- 全失败 → Channel 自决发一条文本通知用户 + drop（不进 ch.Incoming）；部分失败 → 通知用户 + 继续把成功的部分转给 Agent（失败节点从 blocks 中 omit）。

**当前行为**：去掉 receipt lifecycle 步骤(Gateway 不再调 `ch.CreateReceipt / UpdateReceipt / DisposeReceipt`)。Channel 在收到第一个带 `ReplyTo=userMsgID` 的 OutboundMessage 时,自行决定 cold-create / 复用 receipt card / thread / DOM 节点。详见 §2.2。

**MessageState 事件**由 ChatSession 在 lifecycle 各点 emit(步骤 0、b),由 Gateway 的 `OnMessageState` 回调翻译为 `OutboundMessage{Kind: OutMessageState}` 并通过 Channel.Send 转发。详见 §2.5。
```

### 2.2 CLI 输出 → 用户（Outbound）

**核心语义：1 turn : 1 anchor, n events**。每个 agent turn 由 AgentSession 锚定到单一 `as.currentPrompt.LastMessageID`（buffered batch 时锚到最后一条 userMsgID；F-53 前为 `cs.currentTurnUserMsgID` 字符串标量）；EventHandler 在每个 OutboundMessage 上设 `out.ReplyTo = as.currentPrompt.LastMessageID`；Channel 据此路由到对应 receipt（card / thread / DOM 节点）。`ReplyTo == ""` 是仅有的"无锚" case（启动期 EventAgentConnected、系统日志、内部事件），Channel 走 plain text / 跳过。

```
Claude Code 进程 (PTY child, cwd = chatSession.selectedCwd)
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
  └ EventDone/Error → cs.emitMessageStateForCurrentTurn(agent.MessageDone|agent.MessageFailed)
  ↓
ChatSession.onAgentEvent(s, ev)        // EventCallback 驱动
  ├ InputBuffer.SetState(Busy)
  ├ out, send := gateway.Translate(chatID, ev)
  ├ out.ReplyTo = as.currentPrompt.LastMessageID    ← **关键耦合点**（唯一关联信息;F-53 前为 cs.currentTurnUserMsgID）
  ├ (TaskCreate / TaskUpdate) bridge 在匹配 tool_result 确认成功后更新 session-local task map
  │   ├ emit EventTaskCreate / EventTaskUpdate（每次携带完整 TaskListEvent snapshot）
  │   ├ gateway.Translate → OutTaskCreate / OutTaskUpdate（typed TaskList payload）
  │   └ Feishu Channel → 当前 ReplyTo 对应 receipt.SetTaskList → 原位 PATCH checklist
  └ if send: channel.Send(ctx, out)
  ↓
channel.Send(ctx, OutboundMessage)
  ├ 看 msg.ReplyTo（userMsgID）
  ├ 内部按 userMsgID 查自己的 receipt:
  │   ├ 命中 → PATCH / 更新（receipt card 追加内容、thread 发新回复、DOM 节点 in-place 编辑）
  │   └ miss → cold-create 一个新 receipt（userMsgID 作为 key），然后追加
  ├ (OutMessageState case) → AddReaction on Meta["message_id"] → 用户消息挂 ⏳/🔄/✅/❌
  └ (OutChoice case) → 发交互卡片（permission prompt 等）
```

**关键不变量**：
- `ChatSession.EventCallback` 是当前 **active AgentSession** 的唯一消费者。当 `/use` 切换 active 时，ChatSession 重新注册 callback 到新的 AgentSession，老 AgentSession 的 `Events()` 不再被消费（但进程可继续跑、产出事件被丢弃——符合 PRD §4.3 的"过时的不管"语义）
- Gateway **永不** 持有 receipt / receipt map / receipt FSM——Channel 自治
- `out.ReplyTo = as.currentPrompt.LastMessageID`（F-53 前为 `cs.currentTurnUserMsgID`）是 Gateway → Channel 的**唯一关联信息**——Channel 拿这个 key 路由，内部的存储形态（map / DOM / thread）自己选

**1 turn : 1 anchor 示例**（buffered batch：3 条用户消息被 flush 为 1 个 agent turn）：
- ChatSession 的 InputBuffer 攒着 [userMsgID_a, userMsgID_b, userMsgID_c]
- 上一 turn 的 EventDone 触发 onFlush → `as.currentPrompt.LastMessageID = msg_c`（最后一条;F-53 前为 `cs.currentTurnUserMsgID`）
- agent 看到 stdin 是 3 条合并输入，产出 N 个 event
- 每个 event 都 PATCH msg_c 对应的那张 receipt card（F-25 rolling-log）
- msg_a / msg_b 自身的 MessageState(Done) 仍然触发（走 §2.5 的 MessageState 路径）

### 2.3 用户用 slash command 管理 ChatSession

本节描述的命令（`/cwd` `/use` `/close` `/watch` `/think` `/tools` `/new` `/gtw`）均由 `internal/command/<name>/cmd.go` 中的 Factory 实现。Gateway 不持有命令 handler，只通过 Commander 路由 slash command；需要 chat-session 状态的 Factory 直接持有 `*chatsession.Manager`。

#### `/cwd <path>`

```
command.cwd.Factory.Handle(ctx, rt, input)
  ├ Factory.mgr: *chatsession.Manager  (直接持有)
  ├ 验证 path（~ 展开、绝对路径、目录存在）
  ├ cs := mgr.GetOrCreate(chatID, defaultPrimary)
  │   ├ err SetSelectedCwd → command.Reply("SetSelectedCwd failed: ...")
  │   └ OK → 继续
  ├ cs.SetSelectedCwd(abs)  ← 仅改 selectedCwd, 不动 AgentSession
  │   (AgentSession 池中的所有项不动; 切回原 cwd 时复用老 AgentSession)
  └ command.Reply("Workspace set to <abs>")
```

**当前行为**：`/cwd` **不触发 spawn**。它是"切换 selectedCwd"的纯状态变更命令。当用户后续发消息时，ChatSession 通过 `LookupSelectedAgentSession()` 重新解析 `(selectedAgent, selectedCwd)`，按需复用或 spawn。

**当前实现**：handler 直接拿 `*chatsession.Manager`。`expandTilde` 等纯函数仍在命令子包内实现。

#### `/use <agent>`

```
command.use.Factory.Handle(ctx, rt, input)
  ├ Factory.mgr: *chatsession.Manager  (直接持有)
  ├ cs := mgr.GetOrCreate(chatID, defaultPrimary)
  │   ├ SelectedCwd 空 → command.Reply("Send /cwd first")
  │   └ 存在 → 继续
  ├ agentName := input.Args[1]
  ├ cs.SetSelectedAgent(agentName)   ← 仅改 selectedAgent
  ├ as, err := cs.LookupSelectedAgentSession()
  │   ├ pool[(selectedAgent, selectedCwd)] 命中 → 复用 (不重启进程)
  │   └ miss → spawn 新 AgentSession(agentName, selectedCwd)
  ├ cs.StartReadPump()  ← commit 8c — 启动新 pump
  └ command.Reply("Now using <agent>, pid=<N>, cwd=<ws>, source=<spawn|resumed>")
```

**当前行为**：- `/use` **永不重启进程**——pool 里有就复用，没有才 spawn
- 切换前已 queued 的消息（InputBuffer）→ 自动 flush 到新的 active AgentSession
- 老 AgentSession 保留在 pool，切回原 agent/cwd 时能复用

#### `/close`

```
command.kill.Factory.Handle(ctx, rt, input)
  ├ Factory.mgr: *chatsession.Manager  (直接持有)
  ├ cs := mgr.Get(chatID)
  │   ├ nil → command.Reply("No active chat session to kill.")
  │   └ 存在 → 继续
  ├ command.RequireSelectedCwd(cs)   ← 没设 /cwd 就回复 "Send /cwd first"
  ├ 解析 args[1]:空 → 进入 KillAllAgents 分支
  │   非空 → 作为 agentName,进入 KillAgent 分支
  ├ case /close <agent>:
  │   result, err := kill.KillAgent(&kill.Cmd{CS: cs, Ctx: ctx}, agentName)
  │     ├ agentsession.ErrAgentNotFound → "No <agent> session in <cwd> to kill"
  │     └ 成功 → 返回 kill.Result
  └ case /close:
      results, err := kill.KillAllAgents(&kill.Cmd{CS: cs, Ctx: ctx})
        └ 成功 → 返回 []kill.Result(空池/无匹配 → "No active agents to kill.")
      → command.Reply(kill.FormatKillResults(results))
```

**关键变化**：scope 统一为 **selectedCwd 子集**(与 `/new` 对齐),但**两个入口**粒度不同:
- `/close`(无参)= 杀 selectedCwd 下 pool 中**所有 entries**
- `/close <agent>`= 杀 selectedCwd 下**单个**(agent, cwd) entry

`/close` 不再误伤其他 cwd 下的 AgentSession —— 通过 `/cwd` 切到目标目录再 `/close` 即可清理其他 workspace。

**当前实现**:`/close` 的进程关闭逻辑完全封装在 `internal/command/close/` 包(`kill.go` + `format.go`):
- `kill.KillAgent(cmd, agentName)` —— `/close <agent>` 路径
- `kill.KillAllAgents(cmd)` —— `/close` 路径
- `kill.Result` / `kill.FormatKillResults` —— 返回类型 + 渲染

kill 包通过 ChatSession 上的两个通用 lifecycle accessor 访问 pool / selectedAS / 持久化:
- `cs.AgentSessionsInCwd(cwd) []*AgentSession` —— 快照(只读)
- `cs.DropAgentSession(as)` —— 原子地 pool delete + selectedAS clear + asFile delete + persistChatEntry

`ChatSession` 上**没有任何 kill 方法**;handler 负责 RequireSelectedCwd preflight + args 解析 + 调包级函数 + FormatKillResults 渲染。

**Daemon shutdown 不调任何 kill 函数** —— agents 是独立于 nightme 生命周期的长进程。SIGINT/SIGTERM 时只 `Stop()` channel、persist final state,AgentSessions 在 registry 里以 `Detached` 保留,下次 `nightme run` 通过 `Manager.RestoreFromRegistry` + `LookupSelectedAgentSession` 自动复用 `--resume`。用户想真正关进程,通过 `/close` 在对应 chat 里发即可。

### 2.4 Receipt 渲染（Channel 自治）

> **本章不再包含 Gateway 端 FSM 描述**——Receipt FSM 从 Gateway 移除后，receipt 的生命周期完全由 Channel 自治。Gateway 不知道 Receipt 是什么、有几个、存哪里。

**Channel 自治范围内的渲染行为**（仅作协议描述，**不是 Gateway 责任**）：

- 每个 Channel 在收到第一个**有内容**的 `OutboundMessage{ReplyTo: userMsgID}` 时，自行决定如何"开张"：
  - **Feishu (F-42 前)**：cold-create 一张 minimal ⏳ 占位 card，记下 cardMsgID 供后续 PATCH
  - **Feishu (F-42 后)**：**lazy create** — 在收到第一个 `OutReply`（带 reply 文本）或第一个 `OutTaskCreate/Update`（带 task list）时，post 带有实际内容的 card；不创建空 receipt
  - **Slack**：在 userMsg 关联的 thread 下发第一条回复
  - **Web**：在 chat DOM 中插入一个 receipt block，带 `data-user-msg-id` 标记

- 每个 Channel 在收到后续 `OutboundMessage{ReplyTo: userMsgID}` 时，按自己定义的渲染规则 PATCH 对应 receipt：
  - Feishu：`UpdateMessage(cardMsgID, body)` in-place 编辑 card
  - Slack：thread 内发新回复 / 编辑已有回复
  - Web：更新 DOM block 的内容

- 每个 Channel 在收到 `OutMessageState` 时，**自决**哪些状态渲染、怎么渲染：
  - Feishu (F-42 后)：只渲染终态 — `StateDone` → ✅，`StateError` → ❌；`StateReceived` / `StateForwarded` silent drop（与 Tool/Think thread activity 信号重叠，无新增信息）
  - Feishu (F-42 前) / 其他 Channel：可全部 4 态都渲染 — `StateReceived/Forwarded/Done/Error` → ⏳ / 🔄 / ✅ / ❌

**Gateway 视角**：
- Gateway 只发 `OutboundMessage{ReplyTo: userMsgID, Kind: OutText|ToolStart|...}`
- Gateway **不知道** Channel 内部有没有 receipt、存了多少、是否要清理
- Channel 内部状态（Feishu 的 entries / tokens / agentName / state / ...）完全 Channel 私有

**实现细节**：见 [`channel/feishu-rendering.md`](./channel/feishu-rendering.md)（重命名为"rolling-log"，强调是 Channel 实现细节）。原 F-26 gateway-hub §6 中描述的 Gateway 端 Receipt FSM 代码路径全部删除。

---

### 2.5 Message Lifecycle Tracking（F-53 重构）

**核心问题**：用户在 IM 里发了一条消息，怎么知道系统处理到哪一步了？—— `MessageState` 是这个问题的答案。

#### 2.5.1 概念

**`MessageState` = 消息的投递阶段属性**。回答 2 个问题（，**不再**承载"执行结果"——见 F-53）：

1. 系统收到消息没有？ → `agent.MessageQueued`
2. 消息正式交给 AgentSession 了没有？ → `agent.MessageSubmitted`
3. 消息被主动清空了没有？ → `agent.MessageDropped`

> **F-53 重构**：常量从 `MessageReceived` / `MessageForwarded` / `MessageDone` /
> `MessageFailed`（4 态，含执行结果）改为 `MessageQueued` / `MessageSubmitted` / `MessageDropped`
> （3 态，纯投递语义）。`MessageDone` / `MessageFailed` 物理删除。详见
> [`feat/F-53-message-prompt-lifecycle.md`](./feat/F-53-message-prompt-lifecycle.md) §3 原则 1 / §6.3 / §7。

每条普通用户消息在系统里流转时，对应 `MessageState` 事件被 emit；Channel 把它渲染成平台原生视觉
表达（Feishu reaction emoji，Slack emoji 短码，Web UI DOM 元素）。

#### 2.5.2 4 层事件流

```
[1] ChatSession / AgentSession
        │  emit MessageState event (state + userMsgID)
        │  via callback mechanism
        ▼
[2] Gateway
        │  接 OnMessageState callback
        │  翻译成 OutboundMessage{Kind: OutMessageState, MessageState: &MessageStatePayload{MessageID, State}}
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
// §1.4 落地后 OutboundMessage 100% typed — Meta 字段已删除
OutboundMessage{
    Kind: OutMessageState,
    ChatID: chatID,
    MessageState: &MessageStatePayload{
        MessageID: userMsgID,              // 必填：标记哪条用户消息
        State:     state,                  // 必填：状态值 (MessageQueued / MessageSubmitted / MessageDropped)
    },
}
```

#### 2.5.4 触发点

| 触发时机 | 状态 | 说明 |
|---|---|---|
| `cmd/nightme/run.go newMessageDispatcher` 入口（Gateway inbound 拿到后、`LookupSelectedAgentSession` 之前） | `agent.MessageQueued` | 消息进 ChatSession 的消息队列（**不**依赖 AS spawn 是否成功） |
| `ChatSession.defaultPromptHookLocked` 内 `SendBlocks` 返回 nil 之后 | `agent.MessageSubmitted` | 提交事务成功；批量翻 `Message.Stage` + wire emit |
| `ChatSession.MarkDropped(userMsgID)` 调用时 | `agent.MessageDropped` | 仅由 `/close`、`/new`、`BufferClear` 触发，不覆盖投递失败 |

**Scope 强约束**：MessageState **只对普通用户消息触发**。Slash command（`/cwd` `/use` `/close` 等）不产生 MessageState —— 控制平面有 `OutCommandReply` 作为反馈。

**Channel 自治渲染选择**：上述 3 个状态由 ChatSession lifecycle emit 后，由 Gateway 翻译为 `OutboundMessage{Kind: OutMessageState}` 通过 `Channel.Send` 投递。每个 Channel 自决哪些状态需要渲染 + 怎么渲染（Feishu 加 reaction，Slack 加 emoji shortcode，Web 改 DOM 元素等）。 **F-08**（当前）：Feishu adapter 渲染 `MessageQueued` → ⏳ `OneSecond` + `MessageSubmitted` → 🔄 `OnIt`；`MessageDropped` 不渲染。终态进度（Running/Done）独立由 receipt card 通路承载（见 `mapPromptStateToFeishuEmoji` + `MessageReceipt.SetPromptState`），用户消息上不再叠加 ✅。Slack / Web Channel 实现可以选更窄的渲染策略。`agent.MessageState` enum 本身是抽象层契约 ── 不变式是 ChatSession 必须按表格触发，**不**约束 Channel 必须全部渲染。

** 显式 UX 回归**：Feishu 用户消息上的 ✅ / 👎 反应**永久下线**（`MessageDone` / `MessageFailed` 物理删除）。用户消息 reaction 序列：`⏳ → 🔄 → （永远停在 🔄）`。终态进度 ✅ 由 receipt card 上独立的 PromptState 通路承载（运行结束时 reaction `DONE`），不再走用户消息。

**Prompt 实体 + PromptState 平行 FSM**：F-53 起 `agentsession.Prompt` 是一等公民对象（见 [`feat/F-53-message-prompt-lifecycle.md`](./feat/F-53-message-prompt-lifecycle.md) §4.2），挂在 `AgentSession.currentPrompt` 上。Feishu receipt 内的 `PromptState` 同步收敛：`agent.PromptState` 整体从 `agent` 包私有化到 `internal/channel/feishu`，常量缩为 `Running` / `Done` 两值（`Pending` / `Succeeded` / `Failed` 物理删除；构造初始值改 `Running`；删 `Pending → Running` 转换判断）。与 MessageState 的关键区别：**PromptState 是 channel-internal 状态**（不走 wire event，每个 channel 各自的 receipt 自己观察 `agent.EventDone`/`EventError` 来 transition），而 MessageState 是抽象层广播事件（走 `OutboundMessage{Kind: OutMessageState}`）。两者都描述"消息处理到哪了"但回答的问题不同（投递 vs 执行），分别渲染到 user-message reaction 和 receipt card header。

详见 [`channel/feishu-rendering.md`](./channel/feishu-rendering.md)（历史参考）+ [`feat/F-53-message-prompt-lifecycle.md`](./feat/F-53-message-prompt-lifecycle.md)（ 权威定义）。

### 2.6 Interactive Choices

**核心问题**：gtw 决策卡的 worktree-fail 场景（[`channel/feishu-rendering.md`](./channel/feishu-rendering.md) §3.3）此前是纯文本，用户只能靠 emoji reaction 继续；IM 移动端找 emoji 体验差。

> **F-XX 注**：`/gtw fix` 的 **branch-exists** 决策卡已废除（branch 冲突改为硬失败），见 [`feat/F-gtw-fix.md`](./feat/F-gtw-fix.md)。Interactive Choices 在 gtw 侧仅剩 **worktree-fail**。

**F-46**：决策面改为**交互选择**（`OutChoice`）；用户选择后**原地更新**同一张 UI（选中态 + 禁用其余选项 + 可选结果摘要），而不是再发一条平行回复。Channel 可把它渲染成原生 card。

**概念数据流**：

```
用户在决策卡上选择
        │
        ▼
Channel：平台点击 → 归一化为 InboundMessage{Reaction|Action}
        │
        ▼
Gateway：dispatchAction → ChatSession.HandleAction
        │
        ▼
gtw：消费 draft → 执行决策 → 发出 OutChoicePatch（或无 bot 卡 id 时退化为文本）
        │
        ▼
Channel：按平台能力渲染原地更新（Feishu / Slack / Web 各自实现）
```

**抽象契约**（Gateway / gtw 看见的）：

| 概念 | 含义 |
|---|---|
| `OutChoice` | 发出一次新的交互选择（permission / question / gtw decision） |
| `OutChoicePatch` | 原地结算或更新已发出的选择（`Settled` + 可选 `SelectedID`） |
| typed `Choice` | RequestID / Kind / Title / Body / Options / Questions —— 描述要选什么，不含平台 chrome |
| 边界归一化 | 平台按钮点击在 Channel 边界折成既有 reaction/action 通路，与 emoji 汇合 |

**不变式**：

- §1.4：OutboundKind 仍 typed；Channel 自决渲染（含是否支持原地更新）
- §1.3：Channel 不 import chatsession；决策卡 ≠ receipt card（Receipt 自治不变）
- 不引入第二条「卡片专属」生命周期与 emoji reaction 分叉

**实现细节**（button value 编码、action 目录、Feishu 视觉）：见 [`channel/feishu-rendering.md`](./channel/feishu-rendering.md)。

### 2.7 Reaction 路由

> Decision card 按钮点击归一化为 `InboundMessage.Reaction`，与 emoji reaction 共用同一条通路；reaction 由 runtime 持有的 `command.ReactionRouter` 分发，`chatsession` 不承担 reaction 分发责任。

**核心问题**：gtw 决策卡被用户点击 / 打了 reaction，要让 gtw 的 draft handler 接住这条消息，反过来更新 Card、回应用户。整个分发应该在哪个对象上完成？

**当前分发链**：

```
ch.Incoming(reaction / action event)
  │
  ▼
Channel Adapter：归一化为 InboundMessage{Reaction: ...}（F-46 decision-card 按钮归一化到此）
  │
  ▼
Gateway.dispatchInbound(...)
  │  msg.Reaction != nil (or msg.Action != nil)
  ▼
Gateway.dispatchAction(ctx, msg)
  │  调 runtime 注入的 action handler closure
  ▼
runtime 装配时注入：
  gw.WithActionHandler(func(ctx, msg) bool {
      if msg.Reaction == nil { return false }
      return rt.ReactionRouter().Handle(ctx, msg.ChatID,
          command.ReactionEvent{
              TargetMsgID: msg.Reaction.TargetMsgID,
              Emoji:       msg.Reaction.Emoji,
              UserID:      msg.Reaction.UserID,
              ChatID:      msg.ChatID,
          })
  })
  │
  ▼
command.ReactionRouter 内部：
  ├ router 是 cmd/nightme 装配时 new 的 struct,持 map[chatID]handler
  ├ gtw.Manager 在 runtime 启动时调 router.Register(chatID, gtwMgr.HandleReaction)
  │   (gtw 注册自己的状态机 + draft 处理函数)
  ├ router.Handle(chatID, ev) 查 map 出 handler,调它
  │   ├ 命中且 handler 返回 true → consume, gateway 返 Consumed=true
  │   └ 未命中 / 返回 false → router 返 false, gateway 返 Consumed=true(事件已被 gate 持有)
  ▼
gtw.Manager.HandleReaction(ev)
  ├ 查自己的 states / drafts map(不再走 chatsession.gtwContext)
  ├ dispatch 到 executeWorktreeFailAction（branch-exists 路径已废除，见 F-gtw-fix.md）
  ├ 执行动作 + 发 OutChoicePatch / OutReply
  └ 返 true / false 给 router
```

**抽象层接口**（住 `internal/command/services/reaction.go`）：

```go
type ReactionEvent struct {
    TargetMsgID string   // bot 消息 id (用户对哪条消息反应的)
    Emoji       string   // "✅" / "🆕" / "🔗" / "❌" / "🔄" / "🤝"
    UserID      string   // 谁反应的
}

type ReactionRouter interface {
    // Register 把 handler 绑到某个 chat 的 reaction 上。
    // "*" = 监听所有 chat（gtw 这种全局 agent 用）。
    Register(chatID string, handler func(ctx context.Context, ev ReactionEvent) bool)
    // Handle 内部分发,返回 true = 已 consume,gate 决定是否继续。
    Handle(ctx context.Context, chatID string, ev ReactionEvent) bool
}
```

**为什么 `ChatSession` 退出 reaction 分发**：
- `ChatSession.SetActionHandler` / `HandleAction` / `ActionHandler()` 整套 API 删
- 反应分发从「session-aware」(ChatSession 每个 session 一个 handler) 改成「registry-aware」(ReactionRouter 单例 map，runtime 持)
- gtw 不需要 ChatSession 也能处理 reaction —— 它通过 `RuntimeServices.ReactionRouter()` 拿到 router，调 `router.Register(gtwMgr.HandleReaction)` 注册自己
- 未来任何 reaction handler（gtw / permission confirm / interactive prompt）都通过 `router.Register` 接，不再改动 ChatSession

**§2.6 ↔ §2.7 配合**：
- §2.6 解决"decision card 按钮如何变成 reaction"（Channel 边界归一化）
- §2.7 解决"reaction 由谁分发"（runtime ReationRouter，与 ChatSession 解耦）
- 两件事独立但组合工作：decision-card 点击 → Channel 归一化为 reaction → router.Handle → gtw.Manager → OutChoicePatch

**不变式**：
- §1.3 现有不变式"ChatSession 不 import channel/feishu"保持
- §1.3 **新增**"ChatSession 不 import command/、不 import gtw"（反应分发器不在 ChatSession）
- §1.3 **新增**"reaction 不走 ChatSession.HandleAction"（handle action 必须走 ReactionRouter）
- §2.6 决策卡 §2.6 不变式全部保留（按钮归一化、typed Choice、不引入第二条生命周期）
- gateway 持有了 `ReactionRouter`，但只通过接口持有，不直接实现

**实现细节**（ReactionRouter 的具体实现策略、gtw.Manager 注册时机）：见 [`channel/feishu-rendering.md`](./channel/feishu-rendering.md) §3 / §5。

---

## 3. ChatSession 生命周期

ChatSession 分**三层状态**——但**三层状态的所有权清晰分离**：

```
                       /cwd (binding 不存在)
    [no binding] ────────────────────────────────► [binding → ChatSession, no selectedCwd]
                                                                  │
                                                                  │ /cwd <path>
                                                                  ▼
                              /close ◄─────────────── [binding → ChatSession, selectedCwd=/path, no active AgentSession]
                                │                          ▲
                                │                          │ /use <agent> spawn
                                │                          │
                                ▼                          │
                         [binding → ChatSession, selectedCwd=/path, active=(agent, cwd), pool 多条]
```

**关键规则**：
- **selectedCwd 是 /use 的硬性前置条件**：没有 selectedCwd → ChatSession 无 active AgentSession → 不能 dispatch
- **`/cwd` 不杀任何 AgentSession**：永远只改 selectedCwd，老 AgentSession 保留在 pool
- **`/use` 永不重启进程**：永远复用 pool 中现有的，找不到才 spawn
- **Binding 永不过期**：chat_id 永久绑定 ChatSession；ChatSession 跨 daemon 重启恢复
- **AgentSession 不永生**：进程死掉就 status=exited；但 ChatSession 池里仍然引用它（pool 标记 `[exited]`）
- **`/close` 杀进程，cwd-scoped**：无参杀 selectedCwd 下所有 entries；带参杀 `(agent, selectedCwd)` 单个 entry；其他 cwd 下的 entries 不受影响；selectedCwd / selectedAgent / queue / InputBuffer 全部保留

### 3.1 Chat 类型语义

**ChatID 唯一** —— nightme 数据模型只有 `chat_id string`,不持有 chat 类型分类。所有 chat 在 Gateway / ChatSession / Registry 视角下**完全同质**。

**Channel 自管 chat 语义** —— Channel adapter 知道 chat 类型(DM / group / topic),但只通过 `OutboundMessage` 暴露渲染能力:
- Feishu 原生 chat_type:`p2p` / `group` / `topic_group`
- Channel 内部归一化只覆盖 DM / Group / NotSupported 三态(F-33 移除 `ChatTypeThread` 常量)
- topic_group(Feishu thread)**不特殊处理**:消息跟普通 group 走完全相同路径,thread 视觉由 Feishu 端决定

**任何 Channel 都不引入 thread 概念** —— Slack `thread_ts`、Telegram `message_thread_id`、Discord thread 等不进 nightme 数据模型,仅 Channel 内部渲染时使用。如未来 Slack thread 等场景需要支持,在 Channel 包内自治实现,**不动 nightme 数据模型**。

`ChatSessionEntry` 不含 `ChatType` 字段。

#### 3.1.1 WatchMode：per-chat 群消息全收开关

**问题**：nightme bot 加到飞书群后,默认只接收 @ bot 的消息（飞书 `im:message.group_at_msg:readonly` 行为）。如果用户想让 bot "听全群",得在飞书后台手动勾选 `im:message.group_msg` 权限 + 自己处理噪声。

**方案**：per-chat `WatchMode` 字段,默认 `WatchModeMention`(只 @ 收),`/watch on` 切到 `WatchModeAll`(全收)。

| 模式 | 含义 | 触发 |
|------|------|------|
| `WatchModeMention`（默认）| 只在 @ bot 或 @_all 时处理 | channel `HasMention == true` |
| `WatchModeAll` | 处理 chat 内所有消息,无论是否 @ | `cs.WatchMode() == WatchModeAll` |

**Channel 自管 chat 语义的不变式保留**：
- `WatchMode` 字段挂在 `ChatSession`,**不**在 `Message` / `OutboundMessage` / Channel interface
- Channel adapter 只暴露 **`HasMention bool`** 给 `Message`（含 bot 或 @_all 为 true；**DM 永远为 true**）
- Gateway dispatcher 拿 `HasMention` + `cs.WatchMode()` 做 gate,**不**读 chat type
- Chat type（DM/group/topic_group）**不**进 nightme 数据模型

**DM 不受 `WatchMode` 影响的锁定不变式**：
- DM 消息在 channel adapter 里永远被设为 `HasMention=true`（每条 DM 都是 "addressed to bot"）
- 因此 gateway gate `!HasMention && WatchMode != All` 对 DM 永远为 false → DM 永远不 drop
- `WatchMode` 字段在 DM 下被写入仅为保留用户偏好，切回 group 后生效
- 这条不变式由两层测试锁死：
  - `internal/channel/feishu/mention_test.go::TestComputeHasMention_DMInvariant`
  - `internal/gateway/dispatch_watch_test.go::TestDispatchInbound_WatchModeGate_DMInvariant`

**Slash command**：`/watch on` / `/watch off` / `/watch`(无参 = 显示状态)。 handler 由 Gateway dispatcher 处理,跟 `/cwd` `/use` `/close` 同一路径。

**持久化**：`ChatSessionEntry.WatchMode` 字段（默认 WatchModeMention）。缺失字段 fallback 到默认。

**飞书 scope 配合**：`DefaultAddons()` 始终包含 `im:message.group_msg`(不带 `:readonly`)——bot 默认接收所有群消息,由 `WatchMode` 在 nightme 侧 gate。**不**走 CLI flag opt-in,默认就是"飞书送全,nightme 决定要不要处理"。

**职责边界**：
- Channel adapter: 计算 `HasMention`(`message.Mentions` + `chat_type` + `GetBotIdentity()` 拿 bot open_id)
- Gateway dispatcher: 检查 `HasMention` + `WatchMode` 决定 drop 或 pass
- ChatSession: 持有 `WatchMode` 状态 + 提供 setter

**详细落地**：见 [`feat/F-08-channel-abstraction.md`](./feat/F-08-channel-abstraction.md) §Message 字段 + [`channel/feishu-rendering.md`](./channel/feishu-rendering.md) §6.7 mention strip。

### 3.1.2 ThinkMode：per-chat thinking 内容显示开关（F-think）

**问题**：F-thread-route 把 OutThinking 投到飞书 thread 后用户无法关闭 —— 既不能选择 plain text vs markdown，也不能选择看 vs 不看。F-think 让 nightme 侧接管这两个决定权。

**方案**：per-chat `ThinkMode` 字段，默认 `ThinkModeShow`（runtime 投递 OutThinking，由 Feishu adapter 渲染成 lark_md card），`/think off` 切到 `ThinkModeHide`（runtime 在 EventHandler gate 丢弃 OutThinking）。

| 模式 | 含义 | 触发 |
|------|------|------|
| `ThinkModeShow`（默认）| OutThinking 投递到 Channel，渲染为飞书 lark_md thread card | `/think on` |
| `ThinkModeHide` | EventHandler gate 静默丢弃 OutThinking，其他 OutboundKind 不受影响 | `/think off` |

**渲染细节（F-think §3.1.2.1）**：当 `ThinkMode=Show` 且 ChatType=Feishu，OutThinking 由 `internal/channel/feishu/thinking_card.go` 渲染：

```
buildThinkingCard("💭 <text>")  →  Card 2.0 JSON
  { config: {wide_screen_mode: true},
    elements: [{tag:"div", text:{tag:"lark_md", content:"💭 <text>"}}] }
```

长内容（> 1000 runes）自动按 F-37 `splitMarkdownForDivs` 切多 div，code block 整块保留。Echo / Web 等其他 Channel 仍可自由决定如何渲染 `OutboundMessage.Text`（不动抽象契约）。

**Channel 自管 chat 语义的不变式保留**：
- `ThinkMode` 字段挂在 `ChatSession`，**不**在 `Message` / `OutboundMessage` / Channel interface
- Channel adapter 不知道 `ThinkMode`，由 runtime EventHandler 在出站前决定
- Chat type（DM/group/topic_group）**不**进 nightme 数据模型（沿用 F-33 不变式）
- `OutboundMessage` schema 不变：`Text string` 仍是 primitive，Channel 自决 markdown 化

**DM 不受 `ThinkMode` 影响的不变式**：
- DM chat 下 `/think off` 仍按 Hide 处理 OutThinking —— 与 `/watch off` 不同（`/watch` 在 DM 下因 HasMention 恒为 true 而永远不丢消息，`/think` 是 Outbound 维度，与 HasMention 解耦）
- `ThinkMode` 字段在 DM 下被写入仅为保留用户偏好

**Slash command**：`/think on` / `/think off` / `/think`（无参 = 显示状态）。handler 由 Gateway dispatcher 处理,跟 `/cwd` `/use` `/close` `/watch` `/new` 同一路径。接受别名：`show`/`hide`。

**持久化**：`ChatSessionEntry.ThinkMode` 字段（默认 ThinkModeShow）。缺失字段 fallback 到默认（默认值方向：WatchMode = Mention，ThinkMode = Show）。

**职责边界**：
- ChatSession：持有 `ThinkMode` 状态 + 提供 setter
- **Gate 决策点**（F-CODEX-RUNONCE-REVIEW-EVENT 之后的现状，两路共享同一份 policy）：
  - `cmd/nightme/run.go::newEventHandler`（**长路径**，long-lived bridge 走 `cs.AgentEventBus` 的订阅器）—— 读 `cs.ThinkMode()`，对 `OutThinking` 应用 gate
  - `internal/gateway/outbound/emitter_sink.go::dispatchSinkEvent`（**一次路径**，`StreamRunOnceToEmitter` 的 drain goroutine，`/gtw commit` / `/review -a foo` 等 one-shot 调用都走这里）—— 同样读 `cs.ThinkMode()`，对 `OutThinking` 应用 gate
  - 两个 gate 共享 `internal/gateway/outbound/policy.go::ThinkModeGatePolicy`（F-CODEX-RUNONCE-REVIEW-EVENT 期间从 `internal/runtime/policy.go` 搬到 outbound，因为 policy 是 outbound 关注的事）
- Channel adapter：照常处理到达的 OutboundMessage，不感知 ThinkMode

**详细落地**：见 [`internal/command/think/cmd.go`](./internal/command/think/cmd.go) + [`cmd/nightme/run.go::newEventHandler`](./cmd/nightme/run.go) + [`internal/channel/feishu/thinking_card.go`](./internal/channel/feishu/thinking_card.go)。`/think` 由 `command.Commander`（[`internal/command/commander.go`](./internal/command/commander.go)）路由，ChatSession 持有 `ThinkMode` 状态、registry 侧 `ChatSessionEntry.ThinkMode` 持久化为裸 `int`，读侧由 `Manager.RestoreFromRegistry` 做 `ThinkMode(int)` cast。

### 3.1.3 ToolsMode：per-chat 工具调用显示开关 + 合并渲染（F-38）

**问题**：F-thread-route 把 `OutToolStart` / `OutToolEnd` 都投到飞书 thread，每个 tool 产生**两条**独立 thread reply（先 `● Tool(args)` 再 `⎿  …`）。一次 agent turn 调 10 个工具 = 20 条 thread reply，视觉噪声 + 限速成本都很高。同时用户没有 per-chat 开关控制工具调用是否显示。F-38 同时解决这两点：(a) 合并渲染——每对 tool 是**一条** thread reply，不是两条；(b) per-chat toggle 控制工具调用是否可见。

**方案**：per-chat `ToolsMode` 字段，默认 `ToolsModeHide`（runtime 丢弃 `OutToolStart` 和 `OutToolEnd`），`/tools on` 切到 `ToolsModeShow`（runtime 透传，Feishu adapter 走合并路径）。合并策略：在 Feishu adapter 内为每个 turn 维护 `userMsgID → FIFO(startMsgID, startBody)` 缓冲，OutToolStart 发新 thread reply 时记下 message_id，OutToolEnd 到达时用 PATCH 同一 reply 把 result 行追加进去。

| 模式 | 含义 | 触发 |
|------|------|------|
| `ToolsModeHide`（默认）| EventHandler gate 静默丢弃 `OutToolStart` 和 `OutToolEnd`，其他 OutboundKind 不受影响 | `/tools off`（或 default） |
| `ToolsModeShow` | Runtime 透传 `OutToolStart` / `OutToolEnd`；Feishu adapter 走合并路径——Start 发新 thread reply + 记下 msg_id，End 用 PATCH 同一 reply 追加 result 行 | `/tools on` |

**渲染细节（F-38 §3.1.3.1）**：当 `ToolsMode=Show` 且 ChatType=Feishu，OutToolStart 和 OutToolEnd 由 `internal/channel/feishu/tool_thread_merge.go` 合并：

```
postThreadReplyWithID("● Bash(ls)")      →  message_id = om_xxx
                                            (push FIFO: userMsgID → (om_xxx, "● Bash(ls)"))

... tool returns ...

mergeToolReply(om_xxx, "● Bash(ls)\n⎿  💻 Bash → 3 lines")
                                            →  Feishu PATCH om_xxx with merged body
```

长工具名 / 长 args 走现有 `formatToolStartCall` 的 rune-safe truncate；长 result 由 `summarizeToolResult` 单行摘要（无 PII leak）。Echo / Web 等其他 Channel 仍可自由决定如何渲染 `OutboundMessage.Tool`（不动抽象契约）。

**Channel 自管 chat 语义的不变式保留**：
- `ToolsMode` 字段挂在 `ChatSession`，**不**在 `Message` / `OutboundMessage` / Channel interface
- Channel adapter 不知道 `ToolsMode`，由 runtime EventHandler 在出站前决定
- Chat type（DM/group/topic_group）**不**进 nightme 数据模型（沿用 F-33 不变式）
- `OutboundMessage` schema 不变：`Tool *ToolInfo` 仍是 typed primitive，Channel 自决合并 vs 分开发
- 合并 vs 分开发是 **Channel 自治**的渲染细节——`Gateway` 不持有"thread 怎么聚合"的决策

**DM 不受 `ToolsMode` 影响的语义**：
- DM chat 下 `/tools off` 仍按 Hide 处理 `OutToolStart` / `OutToolEnd` —— 与 `/watch off` 不同（`/watch` 在 DM 下因 HasMention 恒为 true 而永远不丢消息，`/tools` 是 Outbound 维度，与 HasMention 解耦）
- `ToolsMode` 字段在 DM 下被写入仅为保留用户偏好

**Slash command**：`/tools on` / `/tools off` / `/tools`（无参 = 显示状态）。handler 由 Gateway dispatcher 处理，跟 `/cwd` `/use` `/close` `/watch` `/think` `/new` 同一路径。接受别名：`show`/`hide`。

**默认方向（vs `/think`）**：`/think` 默认 Hide（off by default —— quiet by default；opt-in 才显示 thinking）；`/tools` 默认 Hide（quiet by default —— 工具调用是 agent progress stream 中最吵的部分，多数用户不要；opt-in 才显示）。两者方向一致：thinking 与 tool-event 都默认关闭，看到的是干净的 final answer。

**持久化**：`ChatSessionEntry.ToolsMode` 字段（默认 ToolsModeHide）。缺失字段 fallback 到默认。

**职责边界**：
- ChatSession：持有 `ToolsMode` 状态 + 提供 setter
- **Gate 决策点**（F-CODEX-RUNONCE-REVIEW-EVENT 之后两路共享 policy，与 ThinkMode 同形）：
  - `cmd/nightme/run.go::newEventHandler`（**长路径**）—— 读 `cs.ToolsMode()`；仅对 `OutToolStart` / `OutToolEnd` 生效
  - `internal/gateway/outbound/emitter_sink.go::dispatchSinkEvent`（**一次路径**）—— 同样读 `cs.ToolsMode()`，对 `OutToolStart` / `OutToolEnd` 应用 gate
  - 两个 gate 共享 `internal/gateway/outbound/policy.go::ToolsModeGatePolicy`（同 ThinkMode 一起搬到 outbound）
- Channel adapter：照常处理到达的 OutboundMessage，不感知 ToolsMode；Feishu 自决是否合并
- 合并实现：Feishu adapter 自治（`internal/channel/feishu/tool_thread_merge.go`）；不动抽象层

**详细落地**：见 [`internal/chatsession/tools_mode.go`](./internal/chatsession/tools_mode.go) + `internal/command/tools/cmd.go` + [`cmd/nightme/run.go::newEventHandler`](./cmd/nightme/run.go) + [`internal/channel/feishu/tool_thread_merge.go`](./internal/channel/feishu/tool_thread_merge.go) + [`internal/channel/feishu/adapter.go::Send`](./internal/channel/feishu/adapter.go)。

### 3.2 状态转换触发器

| From | 触发 | To | 由谁驱动 |
|------|------|-----|----------|
| (no binding) | 用户发送 /cwd | binding 创建 + ChatSession.selectedCwd 设置 + pool 初始化 | Gateway.handler.cwd |
| ChatSession, no selectedCwd | 用户发送 /cwd | selectedCwd 设置 | Gateway.handler.cwd |
| ChatSession, selectedCwd=A, pool 含 (claude,A) | 用户发送 /use codex | selectedAgent=codex, lookup (codex, A) → spawn 新 (codex,A) | Gateway.handler.use |
| ChatSession, selectedCwd=A, active=(claude,A) | 用户发送 /use claude (同一) | noop (已在用) | Gateway.handler.use |
| ChatSession, selectedCwd=A, active=(claude,A) | 用户发送 /cwd B | selectedCwd=B; (claude,A) 仍在 pool; active=(claude,B) → spawn 新 (claude,B) | Gateway.handler.cwd |
| ChatSession, selectedCwd=A | 用户发送 /close | 清空 pool; selectedAgentSession=nil; 老 receipts dispose; **F-42**: graceful shutdown via bridge.Close (5s outer timeout); user reply is per-entry list (✓/✗/•) | Gateway.handler.kill |
| AgentSession.Running | CLI exit / EOF | AgentSession.Exited（仍在 pool） | AgentSession.readPump |
| AgentSession.Running | nightme SIGTERM (default) | AgentSession.Detached | cmd/nightme shutdownRun |
| AgentSession.Running | nightme SIGTERM | AgentSession.Detached (registry persists) | cmd/nightme shutdownRun |
| AgentSession.Detached | nightme 下次启动 | AgentSession.Detached (恢复) | Registry.Restore |
| AgentSession.Detached | 用户 /use (同 agent, 同 cwd) | spawn 新进程 (复用 entry 但新 pid) | Gateway.handler.use |

> **生命周期详细策略**（含进程归属、清理、reattach）：见 [`feat/F-runtime.md`](./feat/F-runtime.md)

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

** 新增并发约束**：
- **每个 ChatSession 维护 `selectedAgentSession` 指针**（原子读 / mutex 写）
- **`/use` 切换时**：先原子清空 selectedAgentSession → 等 in-flight EventCallback 完成 → 重设到新 AgentSession → 启动新 AgentSession 的 readPump（如果新 spawn）
- **老 AgentSession 的 readPump 不主动停**：继续跑，但 EventCallback 是 noop（因为不再 active）

**并发安全**：
- Gateway.bindings / receipts：mutex 保护
- ChatSession.selectedCwd / selectedAgent / pool：mutex 保护
- 每个 AgentSession 内部用 buffered channel 通信，无跨 session 共享状态
- 无全局锁、无 errgroup、无 singleflight

**Back-pressure**：Channel 发送慢 → EventCallback 阻塞 → readPump 阻塞 → `as.Events()` chan buffer 满 → Bridge 阻塞 → CLI 自己阻塞。

> **实现细节**：见 [`bridge/cli-transport.md`](../bridge/cli-transport.md) + [`feat/F-gateway.md`](./feat/F-gateway.md) + [`feat/F-chat-session.md`](./feat/F-chat-session.md) + [`feat/F-chat-session.md`](./feat/F-chat-session.md)

---

## 5. 技术栈（已锁定）

| 层 | 选型 | 备选 | 理由 |
|----|------|------|------|
| 主语言 | **Go 1.22+**（当前 go.mod: 1.26.4）| Rust / Node.js | 单二进制、跨平台编译简单 |
| PTY | **`github.com/aymanbagabas/go-pty`** | creack/pty | API 干净，跨平台抽象好 |
| Channel | **飞书官方 Go SDK**（`github.com/larksuite/oapi-sdk-go/v3`）| 自实现 webhook | 文档全，长连接稳定 |
| Daemon control IPC | **Unix socket / Windows named pipe**（`internal/daemoncontrol`）| HTTP server | 客户端/守护进程本地通信，避免 HTTP 协议与监听端口 |
| 持久化 | **JSON 文件**（`chat_sessions.json` + `agent_sessions.json` 两类 entry）| SQLite | MVP 不需要 DB |
| 配置 | **YAML** | env | 直观 |
| 日志 | **`log/slog`**（标准库）| zap / zerolog | stdlib 够用 |

**持久化 schema**：

```jsonc
{
  "chat_sessions": {
    "<chatSessionId>": {
      "id":                       "cs_oc_xxx",
      "chatId":                   "oc_xxx",            // UNIQUE 索引 (1 chat = 1 ChatSession); nightme 不持有 chat 类型
      "selectedCwd":              "/code/bailing",
      "selectedAgent":            "claude",
      "primaryAgent":             "claude",            // snapshot of cfg.Primary at creation; read-only
      "agentSessionIds":          ["as_1", "as_2"],
      "selectedAgentSessionId":   "as_1",              // 引用 pool 中某项; null 表示未激活
      "createdAt":                "...",
      "lastInteractionAt":        "...",
      "watchMode":                0,                   // 0 = WatchModeMention (默认), 1 = WatchModeAll (F-watch)
      "thinkMode":                0,                   // 0 = ThinkModeHide (默认, off by default), 1 = ThinkModeShow (F-think)
      "toolsMode":                0                    // 0 = ToolsModeHide (默认), 1 = ToolsModeShow (F-38)
    }
  },
  "agent_sessions": {
    "<agentSessionId>": {
      "id":                       "as_1",
      "chatSessionId":            "<chatSessionId>",   // FK, 标识属于哪个 ChatSession 的 pool
      "agent":                    "claude",            // IMMUTABLE
      "cwd":                      "/code/bailing",     // IMMUTABLE
      "pid":                      12345,               // 0 when Detached/Exited
      "status":                   "running | detached | exited",
      "args":                     ["--dangerously-skip-permissions"],  // preserved for respawn
      "sessionId":                "abc123",            // F-53: rename from resumeId; bridge-supplied init event id
      "createdAt":                "...",
      "lastRunAt":                "...",
      "exitCode":                 null,                // 非空当 status == exited
      "model":                    "claude-opus-4-5-20250929"  // F-45: 首 EventAgentReady 捕获
    }
  }
}
```

**Schema 版本**：
- `chat_sessions.json` 当前 schema（不含 `chatType` 字段）
- `agent_sessions.json` 当前 schema（`sessionId` 命名，`compactionCount` 不存在）


**唯一约束**：
- `chat_sessions.chatId` UNIQUE（保证 1 chat = 1 ChatSession）
- `agent_sessions.(chatSessionId, agent, cwd)` UNIQUE（保证 pool 内 (agent, cwd) 1:1；不同 ChatSession 各自独立）

**Config schema (config.yaml) (2026-08-02)**：

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
#       env: {}
```

---

## 6. 配置

[当前不变，新增 `primary` 字段承载 Primary Agent]

---

## 7. 非功能需求 (NFR)

[ 不变]

新增：
- **N-8 ()**：ChatSession active AgentSession 切换延迟 ≤ 100ms（不含 spawn 新进程）

---

## 8. 安全（高层）

[ 不变]

---

## 9. 已锁定的技术决策

| # | 决策 | 结论 |
|---|------|------|
| **Q1** | 技术栈 | **Go 1.22+** |
| **Q2** | Channel 多寡 | **多 channel 并行接入**（v1.3 起）：所有已 login 的 channel 全部自动启动；`channel.Registry` OCP 接入点；`feishu` + `telegram` 已实现，slack / web 是 roadmap。Channel interface 抽象不变。详见 [`CHANNEL.md`](./CHANNEL.md) |
| **Q3** | MVP Agent | **只 Claude Code** + Agent 抽象（`AgentSpec` / `Starter` interface）|
| **Q4** | Session 路由 | **Chat ↔ ChatSession 1:1**（Gateway 持有 binding 表）|
| **Q5** | CLI spawn 方式 | **自己 PTY**（aymanbagabas/go-pty）|
| **Q6** | 鉴权 | **单用户独占假设**，不需要设备配对 |
| **Q7** | Channel ↔ Session 通信 | **单向经过 Gateway**，Channel 与 Session 互不引用 |
| **Q8** | Receipt 状态机 owner | **Gateway**；Channel 只渲染 |
| **Q9** | `/cwd` 语义 | **只改 selectedCwd**，不触发 spawn / kill |
| **Q10** | `/use` 语义 | **永不重启进程**；复用 pool 中现有 AgentSession，没有再 spawn |
| **Q11** | `/close` 语义 | **杀 cwd 下的 AgentSession entries，零 ChatSession 方法暴露**：`/close` 杀 selectedCwd 下 pool 中所有 entries；`/close <agent>` 杀 `(agent, selectedCwd)` 单个 entry；其他 cwd 下的 entry 不受影响。Process-shutdown 逻辑封装在 `internal/command/close/` 包的 `kill.KillAgent` / `kill.KillAllAgents` package-level 函数里（不是 `ChatSession` 方法，零 `ChatSession` kill 方法）；通过 `cs.AgentSessionsInCwd` + `cs.DropAgentSession` 两个通用 lifecycle accessor 访问 pool / selectedAS / 持久化。下次消息触发 spawn 新。Graceful shutdown via bridge.Close，5s outer timeout；InputBuffer 保留；reply 是 per-entry list。Daemon shutdown 不调任何 kill 函数——agents 跨 `nightme` 重启通过 Detached registry state + `--resume` 自动恢复 |
| **Q12** | InputBuffer FSM owner | **ChatSession**（per ChatSession, 跨 `/use` 切换共享 queue）|
| **Q13** | AgentSession 唯一性 | **`(agent, cwd)` per ChatSession 唯一**；不同 ChatSession 可独立 |
| **Q14** | `session.Events()` 单消费者 | **readPump only**；ChatSession 通过 EventCallback 接收 |
| **Q15** | `/cwd` / `/use` 对 AgentSession 的影响 | **不杀任何 AgentSession**；pool 保留老 entry，切回能复用 |
| **Q16** | 多 channel 的 chatsession 归属 | **per-channel `chatsession.Manager`**：每个 channel 一个 `Manager`，chatID 天然 namespaced；ChatSession 不知道自己是哪个 channel（无 `channelName` 字段，持久化 schema 零变更）；入站 partition 隐式由 gw pump goroutine 闭包决定 |
| **Q17** | 多 channel 的 restore | **懒加载**：`Manager.GetOrCreate(chatID)` in-memory miss 时走 `csFile.GetByChat(chatID)` 命中即 hydrate，miss 才 `New`；daemon 启动时**不**做全量 restore。`Manager.RestoreFromRegistry` 方法保留（21 个测试 caller 不破）但 runtime 路径不调 |

**已确认（2026-08-03，F-watch 锁定）**：
- **Q-W1** ✅ 新增 `/watch on|off` slash command 控制 per-chat `WatchMode`：`WatchModeMention`（默认，只 @ 收）/ `WatchModeAll`（全收）；由 Gateway dispatcher 在 `Handle` 入口 gate
- **Q-W2** ✅ `Message.HasMention` 由 channel adapter 计算（DM 永远 true；group 看 `Mentions` + bot open_id）；Gateway 不重复计算
- **Q-W3** ✅ Feishu `DefaultAddons()` **始终包含** `im:message.group_msg`（不带 `:readonly`）：bot 默认接收全群消息，由 `WatchMode` 在 nightme 侧 gate。**不**走 CLI flag opt-in
- **Q-W4** ✅ Channel adapter 在构造 `Message` 前 strip 开头 `@bot_key ` / `@_all ` mention 前缀（还原为 nightme 支持的纯文本格式，让 `/watch on` 能被 `ParseCommand` 正确解析）

**已确认（2026-08-02，PRD 锁定）**：
- **Q-A** ✅ Primary Agent 仅全局 config（顶层 `primary`，与 `agents:` list 并列）；ChatSession.primaryAgent 是创建时 snapshot，不可变。**无 `/default` 命令**。
- **Q-B** ✅ LookupSelectedAgentSession 只看 `(selectedAgent, selectedCwd)`：命中 Running 复用，否则 spawn `(selectedAgent, selectedCwd)`。**没有运行时 fallback**：ChatSession 始终持有一个有效的 selectedAgent（创建/恢复时被 `cfg.Primary` 一次性填入），用户用 `/use` 显式覆盖，lookup 不再做降级判断。

**Q-A 锁定补充**：config schema 顶层 `primary` + `agents` list（`nightme config` 交互菜单生成）。

---

## 10. 与现有项目的关系

---

## 11.1 Daemon startup flow

`nightme run` 启动顺序（v1.3 起：多 channel + 懒加载 restore；见 `cmd/nightme/run.go::runDaemon` 与 [`CHANNEL.md`](./CHANNEL.md) §3）：

```
=== 阶段 1: 共享资源 ===
1.  loadConfig()                                 # ~/.nightme/config.yaml
2.  openChatSessions() / openAgentSessions()     # chat_sessions.json / agent_sessions.json
                                              # (整文件加载到 csFile.entries / asFile.entries in-memory)
3.  removeLegacyRegistryFile(cfg)                # 归档 registry.json 为 .v1.bak
4.  buildAgents(cfg)                             # cfg.Agents → agent.Registry
5.  prCacheReg := &prcache.Registry{}
    gtwDeps := gtw.HandlerDeps{Git, Prober, PRCache: prCacheReg}

=== 阶段 2: 共享编排（无 mgr 依赖） ===
6.  gtwMgr := gtw.NewManager(); gtwMgr.SetHandlerDeps(gtwDeps)
    gtwMgr.SetGetChatSession(runtime.findChatSession)  # 跨 mgr 线性扫
7.  reactionRouter := commandServices.NewReactionRouter()
    reactionRouter.Register("*", gtwMgr.HandleReaction)
8.  gitStatusLookup := func(ctx, chatID) *GitStatus {
        cs := runtime.findChatSession(chatID)  # 共享 closure
        if cs == nil { return nil }
        return cs.GitStatus(ctx)
    }
9.  command.SetDeps(command.Deps{Primary: cfg.Primary, GTWExt: gtwDeps})
    reg := command.Default()  # 已注册的 slash command factory
    commander := command.NewCommander(reg)
10. shellDispatcher := shell.NewDispatcher()    # 不再持 registry；持 mgr per call
11. ir := inbound.New(commander, shellDispatcher, reactionRouter, cfg.Primary)
                                              # 共享单例；4-branch dispatch chain
                                              # Dispatch(ctx, mgr, msg) mgr per call

=== 阶段 3: 启动 channels，逐个 buildStack ===
12. chs, _ := deps.NewChannels(cfg)              # channel.BuildAll(cfg) 扫 registry 逐个构造
    for _, ch := range chs {
        if err := ch.Start(ctx); err != nil {
            logger.Error("channel start failed", "name", ch.Name(), "err", err)
            continue                            # 单 channel 失败不阻塞其他
        }
        ch.SetLogger(logger)
        mgr := buildStack(ch, buildStackOpts{
            Agents: agents, CSFile: csFile, ASFile: asFile,
            Primary: cfg.Primary, GitStatusLookup: gitStatusLookup,
        })
        # buildStack 内部：
        #   spawner := chatsession.NewRegistrySpawner(opts.Agents)
        #   mgr := chatsession.NewManager().WithSpawner(spawner).WithPersistence(...)
        #   em := outbound.New(ch, opts)  # 单 channel Emitter
        #   mgr.WithEmitter(em).WithPrimaryAgent(...)
        #   mgr.WithOnCreate(runtimeHooks)  # 每个 mgr 独立装 onCreate
        #   return mgr
        runtime.allMgrs = append(runtime.allMgrs, mgr)  # 包私有，跨 mgr 查找用
        pumps = append(pumps, gateway.Pump{Channel: ch, Manager: mgr})
        if deps.RegisterHealth != nil {
            deps.RegisterHealth(ch.HealthSnapshot)
        }
    }

=== 阶段 4: 启动 gw（不调 RestoreFromRegistry！） ===
13. gw := gateway.New(ir)                         # 共享 ir，不持 em
14. gw.AttachPumps(pumps...)
15. gw.Start(ctx)                                # 每 pump 启一个 pumpOne goroutine
                                              # pumpOne 闭包 capture (channel, mgr)
                                              # 读 ch.Incoming() → r.dispatcher.Dispatch(ctx, p.Manager, msg)
16. block on signal / ctx.Done()
17. shutdownRun: stop all channels + persist final state
    # 不杀 agent 进程；AgentSession 在 registry 以 Detached 保留
    # 下次启动经 lazy hydrate（首次 GetOrCreate）+ LookupSelectedAgentSession
    # 自动 reuse 或 re-spawn
```

**关键不变量**：

- **每个 channel 一份完整 stack**：Channel + chatsession.Manager（per-channel）+ outbound.Emitter（= 该 channel 自己）。gw 持 `[]gateway.Pump`，每 Pump 闭包 capture `(channel, mgr)`
- **共享 singletons**：`inbound.Router` / `command.Commander` / `shell.Dispatcher` / `gtw.Manager` / `commandServices.ReactionRouter` 全部共享单例；`Dispatch(ctx, mgr, msg)` / `Factory.Handle(ctx, mgr, input)` / `Dispatcher.Handle(ctx, mgr, msg)` 全部接受 mgr per call；共享组件不持 `mgr` 字段
- **懒加载 restore**：daemon 启动时**不**做全量 restore。`csFile` / `asFile` 在 `OpenChatSessionFile` 时已全量加载到内存的 `entries map`；`Manager.GetOrCreate(chatID)` 在 in-memory miss 时走 `csFile.GetByChat(chatID)` 命中即 hydrate，miss 才 `New`
- **每个 mgr 独立装 onCreate**：`buildStack` 内部 `mgr.WithOnCreate(runtimeHooks)`，**先于**该 mgr 第一次 `GetOrCreate` 触发；确保 lazy hydrate 出来的 ChatSession 也能装上 EventHandler / MessageStateBus subscriber
- **outbound 路径无 routing**：`cs.emitter.Send(msg)` = `ch.Send(msg)`，单 channel 直接送；`outbound.Emitter` 仍是单一对象（per-Manager 持有），无 multi 概念
- **跨 mgr 共享查找**：`runtime.findChatSession(chatID)` 线性扫 `runtime.allMgrs`（N=2-3 无压力），给 gtw / gitStatusLookup 用；未来 N 增长再考虑 channel-level 索引
- **OCP 接入点**：`channel.Registry`（`internal/channel/registry.go`）—— 加新 channel = 1 个 `channel/<name>/init.go` 调 `channel.Register("<name>", NewAdapter)`，runtime / gateway / chatsession / command 全部零修改
- **多 channel 凭据 / login**：`nightme login <channel>` 按 `provider.Name()` 分派写 `cfg.<channel>.{...}`，互不干扰；`runDaemon` 通过 `channel.BuildAll(cfg)` 自动识别有凭据的 channel

---

## 11.2 Restart semantics

Daemon 重启后（用户发送 SIGINT 然后再启 `nightme run`，v1.3 起）：

| 数据 | 行为 |
|---|---|
| `cfg.Primary` | 重新读取配置。**不影响已存在的 ChatSession.primaryAgent**（Q-A snapshot）。 |
| `chat_sessions.json` | **懒加载**（v1.3+）：启动时**不**做全量恢复；`csFile` 整文件加载到 in-memory `entries map`；首次 `GetOrCreate(chatID)` 命中即 hydrate（`selectedCwd` / `selectedAgent` / `primaryAgent` / `watchMode` / `thinkMode` / `toolsMode` 复原），miss 才新建。`chatID` 跨 channel 不撞（feishu `oc_*` vs telegram 数字 namespace 天然分离）|
| `agent_sessions.json` | 跟 `chat_sessions.json` 一起懒 hydrate：hydrate ChatSession 时同步按 `chatSessionId` 过滤加载 AgentSession 池，**全部 `Status=Detached`，PID=0** |
| v1.x `registry.json` | 备份为 `.v1.bak`，不恢复数据（见 MIGRATION.md）。 |
| 各 channel Adapter | 启动时按 `channel.BuildAll(cfg)` 自动构造并 `Start`；channel 失败不影响其他 |

**用户感知**：
- 第一次发消息会卡 ~100ms-2s（Spawner 重新 fork + 首次 lazy hydrate 多一次 `csFile.GetByChat` in-memory O(N) 查表）。后续消息即时。
- 已 `/cwd` 但从未 `/use` 的 chat：发消息时 Spawner 触发 `/use` 等价的 lazy spawn（不是 `/use` 显式命令）。
- 显式 `/use` 过的 chat：第一次发消息触发 lazy spawn（因为没有运行中的进程）。
- 没启动的 channel（如只配了 feishu 的用户跑 daemon）只有 feishu Manager，telegram 不在 `allMgrs` 里，不影响。

---

## 12. 文档层级
---

## 13. 跨平台类库使用规范（Cross-Platform Library Discipline）

### 13.1 原则

> **任何跨平台行为 / 平台相关行为的代码，必须集中在专门命名的包内；调用方禁止散落 `filepath.*`、`os/exec` 直连、或 `runtime.GOOS` 分支判断。**

这条铁律源自 [`WINDOWS.md`](./WINDOWS.md) 中记录的多次"Windows 上 git 报错 `Invalid argument`"、"PowerShell 路径转换异常"等问题的根因——同一个平台问题被多个 caller 各自解决一遍,谁也没解决彻底。

### 13.2 已有的跨平台类库（"集中营"）

nightme 已经沉淀了几个跨平台类库,**任何新增代码碰到对应场景必须使用它们,禁止重复造轮子**:

| 包 | 职责 | 关键 API |
|----|------|---------|
| `internal/proc` | 跨平台子进程 spawn 与 console 行为 | `proc.New(ctx, name, args...)`、`proc.HideWindow`、`proc.ComSpecOrDefault`、`proc.OpenTerminal` |
| `internal/command/cwd` | `/cwd` 命令的 orchestration + OS 调用(`os.Stat` 校验目录存在性);**路径解析全部委托给 pathutil**(2026-08-21 迁移完成) | `verifyDirectory`(平台分流 OS 调用)、`expandTilde`(HOME 相对解析,这是 cwd 独有的语义) |
| `internal/pathutil` *(本规范新增,见 [`feat/F-PATHUTIL-001.md`](./feat/F-PATHUTIL-001-unified-path.md))* | 统一的路径规范化、跨平台等价比较、子路径归属判断、IME/CJK 规范化 | `NormalizeForOS`、`NormalizeForGit`、`Equal`、`IsUnder`、`FromSlash`、`ToSlash`、`Clean`、`Join`、`IsAbs`、`Base`、`Dir`、`NormalizeInput`、`NormalizeIMRichText` |

### 13.3 使用规则

#### 13.3.1 路径相关 — **必须**走 `internal/pathutil`

| 场景 | 必须用 | 禁止 |
|------|--------|------|
| 判断绝对路径 | `pathutil.IsAbs` 或 `NormalizeForOS` | `filepath.IsAbs` |
| 拼接路径 | `pathutil.Join` | `filepath.Join` |
| 清理路径字符串 | `pathutil.Clean` 或 `NormalizeForOS` | `filepath.Clean` |
| 比较两个路径是否同一 | `pathutil.Equal` | `==`、`strings.EqualFold`、`filepath.Clean(a) == filepath.Clean(b)` |
| 喂给 `git.Run` / 任何 git argv | `pathutil.NormalizeForGit` | 直接透传 |
| 喂给 `os.Stat` / `os.Open` / 任何 OS 调用 | `pathutil.NormalizeForOS` | 直接透传 |
| 转换分隔符 | `pathutil.FromSlash` / `ToSlash` | `filepath.FromSlash` / `filepath.ToSlash` |
| 写进 yml / json 持久化 | `pathutil.NormalizeForOS` 之后再写 | 原样写 |
| 从 yml / json 读出 | `pathutil.NormalizeForOS` 之后再用 | 原样用 |

#### 13.3.2 子进程 spawn — **必须**走 `internal/proc`

| 场景 | 必须用 | 禁止 |
|------|--------|------|
| 任何 `*exec.Cmd` 构造 | `proc.New(ctx, name, args...)` 或 `proc.NewWith(ctx, opts, ...)` | `exec.CommandContext(...)`、`exec.Command(...)` |
| 需要新开可见终端窗口(tray 用) | `proc.OpenTerminal` | 手搓 `cmd /c start` |
| Windows 下隐藏子进程 console | `proc.New` (默认 HideWindow=true) 或 `proc.HideWindow` | 手设 `syscall.SysProcAttr.CreationFlags \|= 0x08000000` |
| 解析 `%ComSpec%` | `proc.ComSpecOrDefault` | `os.Getenv("ComSpec")` 加 fallback |

#### 13.3.3 `/cwd` 命令专属 — 必须用 `internal/command/cwd`

| 场景 | 必须用 | 禁止 |
|------|--------|------|
| 解析 `/cwd <arg>` 的原始输入 | `cwd::normalizePathInput` → `cwd::expandTilde` → `cwd::resolvePath` → `cwd::verifyDirectory` | 自写 `~` 展开、自写驱动判断、自写存在性检查 |

### 13.4 反模式(出现一个就拒绝合并)

```go
// ❌ 错误:直接用 filepath 做平台相关判断
if filepath.IsAbs(path) { ... }

// ❌ 错误:在 caller 里做平台分流
if runtime.GOOS == "windows" {
    path = strings.ReplaceAll(path, "/", "\\")
}

// ❌ 错误:跨平台等价比较用字节级 ==
if filepath.Clean(a) == filepath.Clean(b) { ... }   // Windows 上大小写敏感,误判

// ❌ 错误:跨平台 spawn 用 exec.Command 直连
cmd := exec.CommandContext(ctx, "git", args...)    // Windows 下不隐藏 console + .cmd shim 失败

// ❌ 错误:把 yml 路径字段原样透传给 git
git.Run(ctx, c.RepoRoot, "worktree", "remove", c.Worktree)   // 见 F-PATHUTIL-001
```

### 13.5 例外与边界

- **`runtime.GOOS` 的合法使用**:仅在 `path_unix.go` / `path_windows.go` 这类**已存在的分流文件**内部;**禁止**在新 caller 里写 `if runtime.GOOS == ...`
- **build tag 平台分流文件**:仅在 `internal/pathutil/`、`internal/proc/`、`internal/command/cwd/` 这三个包内允许新增 `//go:build windows` 文件;其它包遇到平台差异应**先考虑**在以上三个包内增加 helper
- **测试例外**:`*_test.go` 里为验证 build-tag 行为可以临时用 `runtime.GOOS`,但不应在生产逻辑里用

### 13.6 新增跨平台类库的流程

如果发现现有三个包(`proc` / `cwd` / `pathutil`)都覆盖不到某个新场景,按以下流程处理:

1. **先开 issue / discussion** 描述场景、为什么现有包不够
2. **新建包** 命名遵循 `internal/<domain>`(如 `internal/terminal`、`internal/envutil`),不要在已存在的业务包里塞跨平台逻辑
3. **写 feat spec** 在 `docs/feat/` 下,模板参考 [`feat/F-PATHUTIL-001.md`](./feat/F-PATHUTIL-001-unified-path.md)
4. **更新本节** §13.2 的"已有的跨平台类库"表格,把新包登记进去
5. **强制迁移** 已有 caller 到新包,禁止共存期超过一个 minor 版本

### 13.7 历史教训(为何这条规范存在)

记录几个真实发生过的"应该走集中类库但没走"的事件:

1. **gtw close 在 Windows 报错 `Invalid argument`** — `git worktree remove` 收到带前斜杠的路径(`F:/...`),git 内部转换触发 `ERROR_INVALID_PARAMETER`。根因:yml 读端未规范化,调用方未走集中包。修复后归并到 `pathutil`。详见 [`feat/F-PATHUTIL-001.md`](./feat/F-PATHUTIL-001-unified-path.md) §1.1。
2. **Windows console 黑框闪烁** — 早期 `exec.Command` 直连,未走 `proc.New`,子进程弹出可见 console。修复:`proc.New` 强制 `CREATE_NO_WINDOW`。
3. **Windows 上 `.cmd` shim 无法启动** — `exec.CommandContext("claude.cmd", ...)` 触发 `ERROR_INVALID_PARAMETER`(`lpApplicationName` 不接受 `.cmd`)。修复:`proc::launchOnWindowsWith` 按扩展名路由(`cmd.exe /d /c` / `powershell.exe -File` / `node.exe`)。

每多一个 caller 走捷径,就多一个潜在 bug 候补。规范的价值不在"代码变长",在"问题集中"。

---
