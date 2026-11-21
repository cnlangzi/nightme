# F-53: Message / Prompt 生命周期模型

> **Status**: implemented（`Message` / `Prompt` / `PromptState` 已落地）

> **Depends on**: F-27（ChatSession）、F-29（AgentSession Pool）、F-31（MessageState）、F-32（Pi RPC Bridge）
> **Related docs**: [`SPEC.md`](../SPEC.md) §2.5、[`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md)、

---

## 1. Description

`Message` 和 `Prompt` 是 nightme 对"一条用户消息"和"一次提交给 agent 的执行"这两个概念的正式建模。
`Message`（`internal/agentsession/message.go`）是 channel 送进来的最小不可分单元，回答"我发的这条
消息去哪了"；`Prompt`（`internal/agentsession/prompt.go`）是一次真正发生的提交，回答"这次提交现在
什么状态、什么时候结束的、为什么结束"。

两者合起来替代了零散的标量锚点（`currentTurnUserMsgID`）和 flush 时现拼的 `(blocks, userMsgIDs)`
临时元组，让"这次提交包含哪些消息""提交给了谁""什么时候确认收到"这些问题有实体可以回答；遗留的
`Turn` 系命名（`OnTurnEnded` 等）也一并收编到 `Prompt` 这个词下面。

---

## 2. Motivation

驱动这次设计的是三个问题：

1. 怎么判断一条消息是否**正式提交**到 agent 里了？
2. 怎么判断这次提交**执行完成**了（不管成功还是失败）？
3. 一批消息合并提交时，每一条消息各自的进度应该怎么体现？

三个问题的根源是同一个：**"一次提交给 agent 的执行单元"必须被实体化**。它如果只是 flush 时临时拼
出来的裸元组，唯一的"身份"是 ChatSession 上一个用完即扔的字符串标量，就会带来几个连锁问题：

- 消息被判定为"已转发"（旧名 `MessageForwarded`）的时机，只代表"路由目标已确定"，不代表字节
  真的进了 agent 进程——这个状态名和它实际代表的含义对不上。
- 一批消息合并提交后，只有批次里**最后一条**消息能在 agent 完成时收到终态反馈，其余消息永远停在
  中间状态——不是有意为之的 UX 选择，而是因为没有一个对象能同时记住"这次提交包含哪些消息"。
- 提交尝试失败时（无论是排队没排上、还是发送本身报错），系统没有一个清晰、显式的终态来描述"这条
  消息压根没到 agent 那"，只能日志兜底。

---

## 3. Design Principles

这次讨论收敛出的几条核心原则，是理解下面对象设计的关键：

1. **`Message` 只反映投递管线，不反映执行结果**。一条消息"到没到 agent 那"和"agent 跑得怎么样"是
   两个独立的问题；过去这两层语义混在同一个 4 态枚举里，现在彻底拆开。
2. **`Prompt` 只反映"是否还在跑"，不区分成功/失败**。从 Prompt 自己的视角看，无论 agent 是正常完成
   还是报错退出，这次提交的生命周期都**结束了**——"结束"是一个事实，"结束得好不好"是另一件事，由
   单独的原因字段承载，不需要在核心状态机里重复表达。
3. **"Submitted" 阶段事件与 `Prompt` 落地是同一提交路径上的两个步骤，不要求强原子性**。
   提交路径：构造候选 `Prompt` → `AgentSession.Submit`（内部 `SendBlocks`）→ 成功时 `Prompt` 装入
   `AgentSession.currentPrompt`、队列 `Commit`、wire emit `MessageSubmitted`；失败时 `Submit` 回滚
   `currentPrompt`、队列 `Rewind`，消息**仍留在队列**等待下一次 `TryFlush` 自然重投。**极小概率**下
   SendBlocks 成功但回写之前 panic 会留下"Prompt 存在但消息仍在队列"——下次 flush 会重复投递。
   重复投递可接受（user 决策 #B），不引入补偿 / 去重。
4. **主动清空 ≠ 投递失败**。用户/系统主动清空队列（例如切换 agent、重置对话）和"尝试提交但没成功"
   是两件性质不同的事，需要用不同的终态区分，不能都含糊地叫"丢弃"。
5. **`Queued` 状态的天然重投**：SendBlocks 失败后消息仍 `Queued`，下次 `TryFlush`（下次
   `QueueUserMessage`、上一个 turn 的 `KindPromptEnded`、bridge 死亡后的 respawn）会再投递一次。
   **不**引入 timer 重试、**不**做去重；用户接受重复投递（agent 自己按幂等性兜底）。

---

## 4. Object Model

### 4.1 `Message`

一条用户消息——channel 送进来的最小不可分单元，回答"我发的这条消息去哪了"。定义在
`internal/agentsession/message.go`。

| 字段 | 含义 |
|---|---|
| `ID` | 消息标识（channel-native id 优先，缺失时退化生成） |
| `ChatID` | 所属会话 |
| `Blocks` | 消息内容（文本/图片/文件等结构化块） |
| `ReceivedAt` | 进入系统的时间 |
| `Kind` | 队列分段标记（`MessageKindNormal` / `MessageKindQueue`），决定这条消息能不能跟邻居合并进同一个 Prompt |

`Message` 是**不可变值**：它没有 `PromptID` 字段，也没有 `Stage` 字段。投递阶段不挂在消息上，
而是走**队列 + wire 事件**表达：

| 阶段 | 含义 | 触发点 |
|---|---|---|
| `Queued` | 已收到，正在等待被提交；或提交尝试失败后仍在等待 | `internal/runtime/dispatcher.go` 在 `QueueUserMessage` 之前 emit `agent.MessageQueued`；提交失败时该批次被 `Rewind` 回队列待重投 |
| `Submitted` | 已经正式交给 agent | `ChatSession.TryFlush` 在 `AgentSession.Submit` 返回 nil 后 emit `agent.MessageSubmitted` |
| `Dropped` | 被主动清空，从未提交 | `ChatSession.DropQueue`（由 `/close`、`/new` 触发）对每条被清空的消息 emit `agent.MessageDropped`；不覆盖投递失败 |

阶段值本身是 `agent.MessageState`（`internal/agent/message_state.go`），经
`ChatSession.MessageStateBus` 以 `MessageStateEvent` 广播给 Channel 渲染。

**关键澄清（投递失败时的行为）**：如果提交尝试本身失败了（例如发送给 agent 时报错），候选
`Prompt` 被丢弃（`AgentSession.Submit` 回滚 `currentPrompt`），对应的消息**仍然停留在 `Queued`**——
既不转 `Submitted`，也不转 `Dropped`。队列把该批次 `Rewind` 回 pending 区，下次 `TryFlush`
（下次 `QueueUserMessage` / 上一个 turn 的 `KindPromptEnded` / bridge 死亡后的 respawn）会自然重投
——这是设计上唯一允许的"重试机制"，不引入 timer / 补偿 / 去重。

### 4.2 `Prompt`

合并后提交给 agent 的执行单元——一次真正发生的"提交"，回答"这次提交现在什么状态、什么时候结束的、
为什么结束"。

| 字段 | 含义 |
|---|---|
| `ID` | 提交单元标识，格式 `<AgentSessionID>-p<seq>`（见 §6.3） |
| `ChatSessionID` | 所属会话 |
| `AgentSessionID` | 提交目标（创建时快照，不随后续 agent 切换改变） |
| `Messages` | 合并进这次提交的全部 `Message` **值**（有序，按出队顺序） |
| `LastMessageID` | 本批次最后一条 `Message.ID`——事件的 anchor 来源（占位卡挂载点） |
| `Blocks` | 合并后实际发送的内容 |
| `CreatedAt` / `AckedAt` | 创建与确认收到的时间线；`AckedAt` = Submit 成功（SendBlocks 返回 nil）的时刻（权威提交成功时间戳） |
| `LastProgressAt` | 最近一次观察到进展的时间（用于未来的卡死检测） |
| `EndedAt` | 结束时间 |
| `EndReason` | 结束原因（见下） |

`Prompt.Messages` 携带完整的 `Message` 值（而不是 ID 列表），这样 prompt-end 路径
（`ChatSession.writebackMessageState`）和 AS 侧的 readpump 都能直接读到每条消息的数据，不需要
per-chat 的 ID→Message 索引；这些 `Message` 是不可变的，没有消费者会改它们。

**存储归属**：`*Prompt` 挂在 `AgentSession.currentPrompt`（不是 `ChatSession`）。理由：

- Prompt 与 active AgentSession 的生命周期强绑定：`/use` 切走 AS 后，老的 Prompt 即视为离线，
  `AgentSession` 自身状态队列（L1.5）需要从 AS 这一侧观测。
- 切换到新 AS 后，新 Prompt 写在 `newAS.currentPrompt`，老 Prompt 仍由 `oldAS` 持有，归属清晰。
- 写 `currentPrompt` 的临界区在 `AgentSession.Submit` 内，由 `as.asMu` 保护（ChatSession 侧不持有
  `cs.mu` 跨越 `Submit`，避免 `cs.mu` ↔ `as.mu` 的双锁顺序问题）。

**事件的 anchor**：`internal/agentsession/readpump.go` 的 readpump 在 enrich 事件时取
`as.currentPrompt.LastMessageID` 作为 `UserMsgID`。占位卡挂在最后一条 message 上（用户的视线焦点）。
事件不再走单订阅者回调：ChatSession 持有 `services.EventBus[AgentEventEnvelope]`
（`cs.AgentEventBus`），runtime 订阅它，订阅者签名是 `func(env chatsession.AgentEventEnvelope)`。

**执行状态**：`agentsession.PromptState`（`internal/agentsession/prompt_state.go`）是三值枚举，
`Prompt` 自身**没有** state 字段：

| 状态 | 含义 |
|---|---|
| `PromptRunning` | 已提交，正在执行。诞生的那一刻就是这个状态——不存在"已创建但还没运行"的可观察阶段（诞生本身就等价于对应消息进入 `Submitted`，见 §3 原则 3） |
| `PromptDone` | 已结束，且是干净结束（`agent.PromptEndClean`）——Channel 画 ✅ |
| `PromptError` | 已结束，但原因非 clean——Channel 画 ❌ 而不是 ✅ |

**`EndReason`**（结束原因，独立于执行状态；类型是 `agent.PromptEndReason`，
定义在 `internal/agent/prompt_end_reason.go`）：

| 原因 | 含义 | 备注 |
|---|---|---|
| `PromptEndClean` | 正常完成 | |
| `PromptEndError` | agent 报告了错误 | |
| `PromptEndProcessDied` | 进程异常退出，既没有正常完成也没有报错 | readpump 在事件通道关闭时以这个原因结束 in-flight Prompt |
| `PromptEndStalledKilled` | 因为长时间无进展被主动判定为卡死并终止 | 依赖尚未实现的卡死检测能力 |
| `PromptEndUserKilled` | 用户主动终止 | |
| `PromptEndUserStopped` | `/stop` 结束了当前 turn | bridge 进程可能继续运行 |

### 4.3 关系与基数

- 一个 `ChatSession` 拥有多个 `Message`：待投递的在 `ChatSession` 的 `MessageQueue`
  （`internal/chatsession/message_queue.go`）里，已提交的挂在对应 `Prompt.Messages` 上。ChatSession
  **不维护** per-chat 的 `messagesByID` 索引——消息按值在队列与 Prompt 之间流动。
- 一个 `Prompt` 合并自一个或多个 `Message`（N:1）。
- 一个 `Prompt` 提交给恰好一个 `AgentSession`（快照，不追溯变化）；该 AS 的 `currentPrompt` 字段
  在 `endPrompt` 时清空。
- 一个 `Message` 至多归属一个 `Prompt`（由 `Prompt.Messages` 反向指认；仍在队列里等待重投的消息
  没有归属）。

---

## 5. State Machines

### 5.1 投递阶段

- `Queued → Submitted`：`ChatSession.TryFlush` 在 `AgentSession.Submit` 返回 nil 后逐条 emit
  `agent.MessageSubmitted`；失败路径不会到达此状态。
- `Queued → Dropped`：仅当消息被主动从队列清空（`/close`、`/new` 走 `ChatSession.DropQueue`），
  不覆盖投递失败。
- 投递尝试失败：候选 `Prompt` 被丢弃，消息保持 `Queued`；下次 `TryFlush` 自动重投（§3 原则 5）。

投递阶段不是消息上的字段，而是 wire 事件流上的一次性转换：**`Message` 不会因为它所属 `Prompt` 后续的
执行结果而改变自己的投递阶段**。这是与（消息状态镜像 Prompt 终态、需要 fan-out）最大的区别，也是
"批次内消息进度不一致"这个老问题的根本解法：每条消息在提交那一刻就已经拿到了自己诚实、最终的状态，
不需要等待、也不需要之后再广播。

### 5.2 `Prompt` 执行状态

- 诞生即 `PromptRunning`：Submit 返回 nil 才把 `Prompt` 装进 `AgentSession.currentPrompt`，没有
  "已创建未运行"的可观察阶段。
- `PromptRunning → PromptDone` / `PromptRunning → PromptError`：`AgentSession.endPrompt(reason)` 是
  唯一的终态转换——`agent.PromptEndClean` 落 `PromptDone`，其余原因落 `PromptError`。
- `endPrompt` **不**遍历 `Prompt.Messages` 做投递阶段 fan-out——阶段在 `Submitted` 时已经通过 wire
  事件定终态。

---

## 6. Naming Decisions

### 6.1 为什么 `Message` 不改名 `InboundMessage`

系统里已经存在一个 wire 层的 `InboundMessage`（Channel 协议边界的 DTO，字段更宽：附件、回复目标、
反应、动作等）。`Message` 是内部领域对象，两者故意保持不同名字：

- 如果合并成同一个类型，内部领域包就必须反向依赖 wire 层的包，破坏既有的职责分层。
- 就算各自定义同名类型，"同名不同形状"跨包出现在同一段代码里，比用不同名字更容易读错。

**结论**：保留两个不同名字——"边界 DTO" 与 "内部领域对象" 用不同名字，这本身就是有效信息。

### 6.2 为什么用 `Prompt` 而不是 `Turn`

`Turn` 在多轮对话语境里通常指"一整个回合"（用户说话 + agent 回应，双向）。但这里要建模的对象只
负责"提交了什么、提交给谁、何时确认收到、何时/为何结束"——不持有 agent 的回复内容本身（回复内容
是独立的事件流，由 Channel 层单独消费）。用 `Turn` 命名语义超发了。

`Prompt` 精确地只指"提交的那一侧"，而且不是新造词——系统里已经有相关概念在用这个词（执行状态类型、
底层 bridge 协议的 RPC 命令名）。同时借这次机会退役一个历史遗留的重复命名：过去"手动 `/flush`" 和
"agent 结束后自动flush" 是同一份逻辑的两个名字，这次统一收编到 `Prompt` 生命周期的动词下面。

### 6.3 为什么 `agent.MessageState` 的投递常量是 `Queued` / `Submitted` / `Dropped`

`agent.MessageState`（`internal/agent/message_state.go`）的**投递阶段**常量是 `MessageQueued` /
`MessageSubmitted` / `MessageDropped`。理由：

- **名字准确地描述状态**：`Queued` 表达"在消息队列里等待提交"；`Submitted` 表达"字节已
  正式交给 agent"；`Dropped` 表达"被主动清空"。
- **执行结果不属于 `MessageState`**：它们是"执行结果"，由 `Prompt.EndReason` 承载，
  与投递语义解耦（§3 原则 1）。`MessageFailed` 不存在。
- **`MessageDone` 是唯一的例外**：它只服务于同步 dispatch 路径（slash command / shell dispatch）的
  "dispatcher 完成"语义，不表达执行成功 / 失败（见 §8）。
- **wire 与内部统一命名**：Channel 层（Feishu `mapStateToFeishuEmoji`）和 `OutboundMessage.State`
  payload 都直接用 `agent.MessageState`，无类型重命名或映射层。

---

## 7. 与现有概念的关系

这次设计不是从零开始，而是收敛了两套已经存在、但语义有交叉的旧概念：

| 旧概念 | 现状 | 与本设计的关系 |
|---|---|---|
| `agent.MessageState`（投递 3 态：`MessageQueued` / `MessageSubmitted` / `MessageDropped`） | 混合了"投递"和"执行结果"两层语义；在 Feishu 里体现为加在用户原始消息上的表情反应（⏳ / 🔄） | `MessageFailed` 不存在；`MessageDone` 只保留给同步 dispatch 路径（§8）。Feishu 用户消息上 ✅ / 👎 反应不再出现（`mapStateToFeishuEmoji` 这两个 case 删除）——终态进度由 receipt card 的 PromptState 通路承载 |
| `agentsession.PromptState`（3 态：`PromptRunning` / `PromptDone` / `PromptError`） | Channel 渲染用的状态机：Feishu receipt card 按它画 🔄 / ✅ / ❌ | 定义在 `internal/agentsession/prompt_state.go`，`chatsession` 包 re-export `PromptRunning` / `PromptDone`。没有 `Pending`（诞生的那一刻就是 `PromptRunning`），也不把"成功 / 失败"折进同一个终态——`PromptDone` 与 `PromptError` 分开，Channel 据此选 ✅ 还是 ❌ |

---

## 8. Out of Scope / 后续任务

以下内容依赖本设计（`Message`/`Prompt` 对象）才能展开，但不在核心状态机范围内，作为后续独立任务：

- **用户消息上的终态反馈**：✅ / 👎 反应从 Feishu 用户消息上移除（`MessageFailed` 不存在）。
  用户消息 reaction 序列是"⏳ → 🔄"；终态进度由 receipt card 的 PromptState 通路承载。
  替代 UX（占位卡上展示终态？reaction 移到卡片？）由独立后续任务决定。
  - `agent.MessageDone` 通过
    [slash-command-reactions.md](slash-command-reactions.md) 重新引入,
    仅服务于**同步 dispatch 路径**(slash command / shell dispatch)的"dispatcher 完成"语义;
    不表达执行成功 / 失败(失败仍由 reply 文本的 ❌ prefix 表达)。`MessageFailed`
    仍物理不存在。Framework 层(`commander.Dispatch`)在 `cmd.Handle`
    调用前后自动埋两个 emit,所有 slash command 自动获得 ⏳ → ✅ 对,
    无需各 command 单独接线。Receipt card 上的 ✅ 仍走
    `PromptEndBus → SetPromptState(PromptDone)` 老路径不变。
- **`MessageDropped` 的视觉反馈**：当前 `mapStateToFeishuEmoji(MessageDropped) = ""`，被清空
  的消息不会有任何视觉标记。占位卡 / 文本 reply 形式由独立 UX PR 决定。
- **Feishu 卡片展示执行结果 + 用户消息表情反应的去留（产品侧）**：是否要把"成功/失败"从用户消息上
  的表情反应搬到占位卡片上展示，涉及具体的渲染改动和一次产品侧的 UX 评审，不夹在对象重构里做。
- **Agent 存活探测（进程/传输层）**、**Prompt 卡死检测（长时间无进展）**、**AgentSession 状态队列**
  （切换会话上下文后离线 Prompt 的追踪）、**`nightme health` 扩展**——这些都是建立在 `Message`/
  `Prompt` 对象之上的健康监控能力。
- **`Prompt` 是否持久化**：不持久化（仅内存）。崩溃后能看到"上一个
  Prompt 卡在哪一步"需要落盘，待确认。

---

## 9. References

- [`SPEC.md`](../SPEC.md) §2.5 Message Lifecycle Tracking
- [`F-chat-session.md`](./F-chat-session.md) — ChatSession / AgentSession 设计
- [`../channel/feishu-rendering.md`](../channel/feishu-rendering.md) — Channel 侧渲染
- [`../bridge/pi.md`](../bridge/pi.md) — Pi bridge 的 `new_session` reset 协议
