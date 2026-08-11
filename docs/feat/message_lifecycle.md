# F-53: Message / Prompt 生命周期模型

> **Status**: design draft（讨论已收敛，未实现）
> **Milestone**: v1.3.x
> **Depends on**: F-27（ChatSession）、F-29（AgentSession Pool）、F-31（MessageState）、F-32（Pi RPC Bridge）
> **Related docs**: [`SPEC.md`](../SPEC.md) §2.5、[`F-31-message-state.md`](./F-31-message-state.md)、
>   [`F-42-lazy-receipt-creation.md`](./F-42-lazy-receipt-creation.md)、[`F-44-outreply-independent-and-task-receipt.md`](./F-44-outreply-independent-and-task-receipt.md)
> **Out of scope（本文档不覆盖）**：
> - Agent 存活探测 / Prompt 卡死检测 / `nightme health` 扩展等健康监控体系 → 留给"Prompt 投递稳定性优化" PR
> - Feishu 卡片 / 消息 reaction 的具体渲染调整（对 F-42 / F-44 的后续修订）→ 独立任务，未立项

---

## 1. Description

`Message` 和 `Prompt` 是 nightme 对"一条用户消息"和"一次提交给 agent 的执行"这两个概念的正式建模。
在此之前，系统里只有零散的标量（`currentTurnUserMsgID`）和临时元组（flush 时现拼的
`(blocks, userMsgIDs)`），没有一个实体能回答"这次提交包含哪些消息""提交给了谁""什么时候确认收到"
这些问题。本设计把它们提升为两个一等公民对象，并顺带把遗留的 `Turn` 系命名（`OnTurnEnded`、
`currentTurnUserMsgID` 等）统一收编到 `Prompt` 这个词下面。

---

## 2. Motivation

驱动这次设计的是三个问题：

1. 怎么判断一条消息是否**正式提交**到 agent 里了？
2. 怎么判断这次提交**执行完成**了（不管成功还是失败）？
3. 一批消息合并提交时，每一条消息各自的进度应该怎么体现？

现状的问题根源是同一个：**"一次提交给 agent 的执行单元"从未被实体化**。它只是 flush 时临时拼出来的
裸元组，唯一的"身份"是 ChatSession 上一个用完即扔的字符串标量。这带来几个连锁问题：

- 消息被判定为"已转发"（今天叫 `MessageForwarded`）的时机，只代表"路由目标已确定"，不代表字节
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
3. **`Message.Stage: Submitted` 与 `Prompt` 持久化是同一提交路径上的两个步骤，不要求强原子性**。
   提交路径：构造候选 `Prompt` → SendBlocks → 成功时 `Prompt` 装入 `AgentSession.currentPrompt`、
   批量翻 `Message.Stage = Submitted`、wire emit `MessageSubmitted`；失败时 `Prompt` 不创建、
   `Message.Stage` 留 `Queued`、下次 `flushPending` 自然重投。**极小概率**下 SendBlocks 成功但回写
   `Stage` 之前 panic 会留下"Prompt 存在但 message 仍 Queued"——下次 flush 会重复投递。重复投递
   可接受（user 决策 #B），不引入补偿 / 去重。
4. **主动清空 ≠ 投递失败**。用户/系统主动清空队列（例如切换 agent、重置对话）和"尝试提交但没成功"
   是两件性质不同的事，需要用不同的终态区分，不能都含糊地叫"丢弃"。
5. **`Queued` 状态的天然重投**：SendBlocks 失败后消息仍 `Queued`，下次 `flushPending`（下次 `Add`、
   下次 `OnTurnEnded`、手动 `/flush`）会再投递一次。**不**引入 timer 重试、**不**做去重；用户接受
   重复投递（agent 自己按幂等性兜底）。

---

## 4. Object Model

### 4.1 `Message`

一条用户消息——channel 送进来的最小不可分单元，回答"我发的这条消息去哪了"。

| 字段 | 含义 |
|---|---|
| `ID` | 消息标识（channel-native id 优先，缺失时退化生成） |
| `ChatID` | 所属会话 |
| `Blocks` | 消息内容（文本/图片/文件等结构化块） |
| `ReceivedAt` | 进入系统的时间 |
| `PromptID` | 所属 `Prompt` 的引用；只有进入 `Submitted` 时才回填 |

**`Message.Stage`**（三态，纯投递语义）：

| 状态 | 含义 | 进入条件 |
|---|---|---|
| `Queued` | 已收到，正在等待被提交；或提交尝试失败后仍在等待 | 消息进入系统的默认起点；下次 `flushPending` 触发再投 |
| `Submitted` | 已经正式交给 agent | SendBlocks 返回 nil 后批量置位；失败路径不会到达此状态 |
| `Dropped` | 被主动清空，从未提交 | 仅对应**主动**的队列清空操作（例如 `/close`、`/new`、`BufferClear` 调用），不覆盖投递失败 |

**关键澄清（投递失败时的行为）**：如果提交尝试本身失败了（例如发送给 agent 时报错），**不会创建
`Prompt`**，对应的消息**仍然停留在 `Queued`**——既不转 `Submitted`，也不转 `Dropped`。下次
`flushPending`（下次 `Add` / 下次 `OnTurnEnded` / 手动 `/flush`）会自然重投——这是设计上唯一允许
的"重试机制"，不引入 timer / 补偿 / 去重。

### 4.2 `Prompt`

合并后提交给 agent 的执行单元——一次真正发生的"提交"，回答"这次提交现在什么状态、什么时候结束的、
为什么结束"。

| 字段 | 含义 |
|---|---|
| `ID` | 提交单元标识，格式 `as_<n>-p<seq>`（见 §6.3） |
| `ChatSessionID` | 所属会话 |
| `AgentSessionID` | 提交目标（创建时快照，不随后续 agent 切换改变） |
| `MessageIDs` | 合并进这次提交的全部 `Message.ID`（有序） |
| `LastMessageID` | 本批次最后一条 `Message.ID`——EventHandler 的 anchor 来源（占位卡挂载点） |
| `Blocks` | 合并后实际发送的内容 |
| `CreatedAt` / `AckedAt` | 创建与确认收到的时间线；`AckedAt` = SendBlocks 返回 nil 的时刻（权威提交成功时间戳） |
| `LastProgressAt` | 最近一次观察到进展的时间（用于未来的卡死检测） |
| `EndedAt` | 结束时间 |
| `EndReason` | 结束原因（见下） |

**存储归属**：`*Prompt` 挂在 `AgentSession.currentPrompt`（不是 `ChatSession`）。理由：

- Prompt 与 active AgentSession 的生命周期强绑定：`/use` 切走 AS 后，老的 Prompt 即视为离线，
  `AgentSession` 自身状态队列（L1.5）需要从 AS 这一侧观测。
- 切换到新 AS 后，新 Prompt 写在 `newAS.currentPrompt`，老 Prompt 仍由 `oldAS` 持有，归属清晰。
- 写 `currentPrompt` 的临界区在 `ChatSession.defaultPromptHookLocked` 内，由 `cs.mu` 保护
  （避免 `cs.mu` ↔ `as.mu` 双锁顺序问题，承认耦合，先 work）。

**EventHandler 的 anchor**：`runReadPump` 调用 handler 时传 `userMsgID = as.currentPrompt.LastMessageID`。
占位卡挂在最后一条 message 上（用户的视线焦点）；`EventHandler` 签名不变，仍是
`func(chatID string, s *AgentSession, ev agent.AgentEvent, userMsgID string)`。

**执行状态（两态，不再区分成功/失败）**：

| 状态 | 含义 |
|---|---|
| `Running` | 已提交，正在执行。诞生的那一刻就是这个状态——不存在"已创建但还没运行"的可观察阶段（因为诞生本身就等价于对应 `Message` 进入 `Submitted`，见 §3 原则 3） |
| `Done` | 已结束。**成功和失败都是 `Done`**——具体原因看 `EndReason`，状态机本身不重复表达 |

**`EndReason`**（结束原因，独立于执行状态）：

| 原因 | 含义 | 备注 |
|---|---|---|
| `Clean` | 正常完成 | |
| `Error` | agent 报告了错误 | |
| `ProcessDied` | 进程异常退出，既没有正常完成也没有报错 | 对应现状里"进程崩溃导致永久卡住"的那类 bug，留给"Prompt 投递稳定性优化" PR |
| `StalledKilled` | 因为长时间无进展被主动判定为卡死并终止 | 依赖尚未实现的卡死检测能力，留给"Prompt 投递稳定性优化" PR |
| `UserKilled` | 用户主动终止 | |

### 4.3 关系与基数

- 一个 `ChatSession` 累计拥有多个 `Message`（挂在 `ChatSession.messagesByID`）和多个 `Prompt`
  （通过其各自的 `AgentSession.currentPrompt` 间接引用）。
- 一个 `Prompt` 合并自一个或多个 `Message`（N:1）。
- 一个 `Prompt` 提交给恰好一个 `AgentSession`（快照，不追溯变化）；该 AS 的 `currentPrompt` 字段
  在 `endPrompt` 时清空。
- 一个 `Message` 至多归属一个 `Prompt`（`Submitted` 之后才建立这层关联；`Dropped` 或仍在 `Queued`
  的消息没有归属）。

---

## 5. State Machines

### 5.1 `Message.Stage`

- `Queued → Submitted`：`PromptHook` 内 `SendBlocks` 返回 nil 后批量置位；失败路径不会到达此状态。
- `Queued → Dropped`：仅当消息被主动从队列清空（`/close`、`/new`、`BufferClear`），不覆盖投递失败。
- 投递尝试失败：不创建 `Prompt`，消息保持 `Queued`；下次 `flushPending` 自动重投（§3 原则 5）。

一条消息一旦进入 `Submitted` 或 `Dropped`，就不再变化——**`Message` 不会因为它所属 `Prompt` 后续的
执行结果而改变自己的状态**。这是与旧设计（消息状态镜像 Prompt 终态、需要 fan-out）最大的区别，也是
"批次内消息进度不一致"这个老问题的根本解法：每条消息在提交那一刻就已经拿到了自己诚实、最终的状态，
不需要等待、也不需要之后再广播。

### 5.2 `Prompt` 执行状态

- 诞生即 `Running`：SendBlocks 返回 nil 才创建 `Prompt`，没有"已创建未运行"的可观察阶段。
- `Running → Done`：`endPrompt(reason)` 唯一终态转换，成功/失败都走这条路径，区别只在 `EndReason`。
- `endPrompt` **不**遍历 `Prompt.MessageIDs` 做状态 fan-out——`Message.Stage` 在 `Submitted` 时已定终态。

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

### 6.3 为什么 `agent.MessageState` 常量是 `Queued` / `Submitted` / `Dropped`

Phase 0 把 `agent.MessageState` 的常量从旧的 `Received` / `Forwarded` / `Done` / `Failed` 改名收敛为
`MessageQueued` / `MessageSubmitted` / `MessageDropped`。理由：

- **新名字更准确地描述状态**：`Queued` 表达"在消息队列里等待提交"；`Submitted` 表达"字节已
  正式交给 agent"；`Dropped` 表达"被主动清空"。
- **`Done` / `Failed` 不再属于 `MessageState`**：它们是"执行结果"，由 `Prompt.EndReason` 承载，
  与投递语义解耦（§3 原则 1）。常量**物理删除**——不留占位。
- **wire 与内部统一命名**：Channel 层（Feishu `mapStateToFeishuEmoji`）和 `OutboundMessage.State`
  payload 都直接用 `agent.MessageState`，无类型重命名或映射层。

---

## 7. 与现有概念的关系

这次设计不是从零开始，而是收敛了两套已经存在、但语义有交叉的旧概念：

| 旧概念 | 现状 | 与本设计的关系 |
|---|---|---|
| `agent.MessageState`（4 态：`Received` / `Forwarded` / `Done` / `Failed`） | 混合了"投递"和"执行结果"两层语义；今天在 Feishu 里体现为加在用户原始消息上的表情反应（⏳ / 🔄 / ✅ / 👎） | 物理删除 `MessageDone` / `MessageFailed`；`Received` → `Queued`、`Forwarded` → `Submitted`、新增 `Dropped`。**Phase 0 副作用**：Feishu 用户消息上 ✅ / 👎 反应不再出现（`mapStateToFeishuEmoji` 这两个 case 删除）——这是显式的 UX 回归点，commit message 必须明示，UX 替代方案由独立后续任务处理 |
| `agent.PromptState`（4 态：`Pending` / `Running` / `Succeeded` / `Failed`） | 是 Feishu 卡片渲染用的 Channel 内部状态机；但"成功"/"失败"两个值在生产代码里实际从未被真正使用过（F-44 已确认） | 整体从 `agent` 包私有化到 `internal/channel/feishu` 包；常量缩为 `Running` / `Done` 两值（死代码 `Succeeded` / `Failed` 物理删除），构造初始值改 `Running`，删 `Pending → Running` 转换判断。卡片要不要在此基础上恢复"展示执行结果"的能力，是独立的后续任务 |

---

## 8. Out of Scope / 后续任务

以下内容依赖本设计（`Message`/`Prompt` 对象）才能展开，但**不在本次设计 / Phase 0 范围内**，作为
后续独立任务：

- **Phase 0 显式行为变更**：✅ / 👎 反应从 Feishu 用户消息上**永久移除**（`MessageDone` /
  `MessageFailed` 物理删除）。用户消息 reaction 序列由"⏳ → 🔄 → ✅"变为"⏳ → 🔄 →（不变）"。
  替代 UX（占位卡上展示终态？reaction 移到卡片？）由独立后续任务决定。
- **进程异常退出时 `endPrompt(ProcessDied)` 的收口**：`runReadPump` 的 `!ok` 分支目前只
  `SetExited(0)` 后 return，buffer 永久 Busy。下一阶段"Prompt 投递稳定性优化" PR 统一处理
  （包括主动 respawn、stall watchdog、`nightme health` 扩展）。Phase 0 不实现。
- **`MessageDropped` 的视觉反馈**：当前 `mapStateToFeishuEmoji(MessageDropped) = ""`，被清空
  的消息不会有任何视觉标记。占位卡 / 文本 reply 形式由独立 UX PR 决定。
- **Feishu 卡片展示执行结果 + 用户消息表情反应的去留（产品侧）**：是否要把"成功/失败"从用户消息上
  的表情反应搬到占位卡片上展示，涉及具体的渲染改动和一次产品侧的 UX 评审，不夹在对象重构里做。
- **Agent 存活探测（进程/传输层）**、**Prompt 卡死检测（长时间无进展）**、**AgentSession 状态队列**
  （切换会话上下文后离线 Prompt 的追踪）、**`nightme health` 扩展**——这些都是建立在 `Message`/
- Agent 存活探测（进程/传输层）、Prompt 卡死检测（长时间无进展）、AgentSession 状态队列（切换会话上下文后离线 Prompt 的追踪）、`nightme health` 扩展——这些都是建立在 `Message`/`Prompt` 对象之上的健康监控能力，留给"Prompt 投递稳定性优化" PR。
- **`Prompt` 是否持久化**：Phase 0 不持久化（仅内存 + 最近 K 个用于调试）。崩溃后能看到"上一个
  Prompt 卡在哪一步"需要落盘，待确认。

---

## 9. References

- [`SPEC.md`](../SPEC.md) §2.5 Message Lifecycle Tracking
- [`F-27-chatsession.md`](./F-27-chatsession.md) — v1.2 ChatSession 设计（已 superseded by F-53；见文档顶部 SUPERSEDED 横幅）
- [`F-29-agent-session-pool.md`](./F-29-agent-session-pool.md)
- [`F-31-message-state.md`](./F-31-message-state.md) — v1.3 MessageState 4 态设计（已 superseded by F-53）
- [`F-32-pi-rpc-bridge.md`](./F-32-pi-rpc-bridge.md)
- [`F-42-lazy-receipt-creation.md`](./F-42-lazy-receipt-creation.md) — 已 superseded by F-53
- [`F-44-outreply-independent-and-task-receipt.md`](./F-44-outreply-independent-and-task-receipt.md) — 已 superseded by F-53
