# F-33: Simplify ChatID Data Model — Drop ChatType, RootId, Wire ReplyTo

> **Status**: implemented（D1 / D2 / D3 三个决策均已落地）

> **Depends on**: F-08 (Channel abstraction), F-27 (ChatSession), F-29 (AgentSession pool)
> **Related docs**: [`SPEC.md`](../SPEC.md)/ §1.3 / §2.1 / §2.2 / §3.1 / §5, [`channel/feishu.md`](../channel/feishu.md) §13.11

---

## 0. 摘要

chatID 数据模型收口为三个**决策**:

| # | 决策 | 落地形态 |
|---|------|----------|
| **D1** | **ChatType 不进 Gateway** | `InboundMessage` / `BindingEntry` / `ChatSession` / `ChatSessionEntry` 均不带 ChatType 字段;`InboundMessage.IsDM()` 已删除;`internal/channel/channel.go` 只保留 `ChatTypeP2P` / `ChatTypeGroup` 两个 Channel 私有常量 |
| **D2** | **topic_group 不特殊处理** | Feishu adapter 不丢弃、不分支,topic_group 跟普通 group 走相同路径 |
| **D3** | **`ReplyTo = ParentId`,RootId 不进 nightme** | Inbound wire `msg.ReplyTo = message.ParentId`;整个项目不读 `event.Message.RootId` |

**架构原则**:nightme 数据模型只追踪**点对点 reply 关系**,不追踪 thread 树。thread 是 Channel 渲染细节,Feishu 端决定 thread 视觉,nightme 内部完全不见。

---

## 1. 现状

### 1.1 chatID 数据模型的边界

nightme 的 chatID 数据模型只保留**点对点 reply 关系**,不保存 chat 类型分类,也不保存 thread 树。

**ChatType**(不进 nightme 数据模型):
- `internal/channel/channel.go` 只保留 `ChatTypeP2P` / `ChatTypeGroup` 两个常量,供 Channel 内部做 chat 类型分类;`ChatTypeThread` 已删除
- `internal/messages/inbound.go` 的 `InboundMessage` 不带 ChatType 字段
- `internal/gateway/binding.go` 的 `BindingEntry` 不带 ChatType 字段
- `internal/chatsession/chatsession.go` 的 `ChatSession` 不带 ChatType 字段
- `internal/registry/chat_session_entry.go` 的 `ChatSessionEntry` 不带 ChatType 持久化字段
- `internal/channel/channel.go` 的 `IsDM` 辅助函数已删除;Channel 需要区分 DM / group 时按平台 chat id 形状自行判定

Gateway 只看 `ChatID string`,假设所有 chat 同质;chat 类型分类是 Channel 的自治范围。

**RootId**(不进 nightme):
- Feishu SDK 的 `EventMessage.RootId` 是 thread 顶层 message_id
- 项目内不读 `event.Message.RootId`
- `InboundMessage.ReplyTo`(`internal/messages/inbound.go`)只承载 Feishu 原生 `parent_id`

**ParentId → ReplyTo 接线**:
- `handleMessage`(`internal/channel/feishu/adapter.go`)构造 `channel.Message` 时填 `ReplyTo: stringValue(message.ParentId)`
- user 在 DM / group 里 reply-in-thread 发消息时,`InboundMessage.ReplyTo` 带上被 reply 的那条 message_id;顶层消息(`ParentId == ""`)时为空

### 1.2 ReplyTo 的语义

nightme 内部只跟踪**点对点的 message_id 关系**,不爬 thread 树。`ReplyTo` 只有一个语义:**"被 reply 的那条 message_id"**,两个方向共用同一个字段。

| 方向 | 值 | 含义 |
|---|---|---|
| Inbound | `message.ParentId` | user 直接回复的那条 message_id;顶层消息为空 |
| Outbound | `Prompt.LastMessageID` | bot reply 锚到 user 当前 message_id |

`docs/channel/feishu.md` §13.10 落地 outbound 方向的 reply-in-thread(`msg.ReplyTo` 作为 Feishu root_id 投递,走 `POST /im/v1/messages/{rootID}/reply`);§13.11(D3)补上 inbound 方向。两条合起来构成 reply-in-thread 数据流闭环。

### 1.3 三个决策的共同点

D1 / D2 / D3 本质是同一件事:**最小化数据模型,只保留必要维度**。nightme 不需要知道 Feishu 的 thread 语义,只需要点对点的 message_id 关系;thread 视觉由 Channel 自治。

---

## 2. 决策

### D1: ChatType 不进 Gateway

**结论**:
- nightme 数据模型里没有 `ChatType` 类型:`InboundMessage`(`internal/messages/inbound.go`)、`BindingEntry`(`internal/gateway/binding.go`)、`ChatSession`(`internal/chatsession/chatsession.go`)、`ChatSessionEntry`(`internal/registry/chat_session_entry.go`)都不带该字段
- `InboundMessage.IsDM()` 方法不存在
- `/status` 命令输出不展示 "DM" / "Group" 标签(若需要,改成 "ChatID: oc_xxx")

**理由**:
- Gateway 只看 `ChatID string`,假设所有 chat 同质
- ChatType 当前唯一用途是 `/status` 命令展示,影响范围小
- 后续 Slack/Telegram 接入时,Channel 自管 chat 类型分类;Gateway 数据模型不变
- 删除后 binding 表 / 持久化 schema 更干净,ChatType 污染问题彻底解决

### D2: topic_group 不特殊处理

**结论**:
- Feishu adapter `handleMessage`(`internal/channel/feishu/adapter.go`)**完全不看 chat_type**,只过滤 `chatID == ""`
- topic_group 消息跟普通 group 消息走**完全相同**的路径:
  1. `bindings[msg.ChatID]` 查 ChatSession(`oc_topic_group_xxx` 跟 `oc_group_xxx` 在 binding 表里平等)
  2. ChatSession 用 `Prompt.LastMessageID` 跟踪当前 turn 的锚点
  3. outbound `msg.ReplyTo` = 当前 turn 的 user message_id,Feishu Reply API 投递

**理由**:
- Feishu SDK `EventMessage.ChatId` 在 topic_group 下跟 group **同构**(都是 `oc_xxx`)
- Feishu 的 thread 是**消息级逻辑分组**,不是 chat 级 — 不创建新 chat_id
- topic_group 在 nightme 内部就是"group,thread 在外面",符合"Channel 自管 thread 渲染"原则

### D3: ReplyTo = ParentId,RootId 不进 nightme

**结论**:
- Inbound `msg.ReplyTo = message.ParentId`(Feishu 原生 `parent_id` 字段)
- Outbound `msg.ReplyTo` = 当前 turn 的 user message_id(`Prompt.LastMessageID`;§13.10 落地)
- **`RootId` 整个项目永远不读、不存、不传**

**理由**:
- `ReplyTo` 字段统一语义 = "**被 reply 的那条 message_id**"
- Inbound 角度:user 主动 reply 的目标(`ParentId`)
- Outbound 角度:bot reply 的目标(user 当前 message_id,因为 bot turn 锚到它)
- 两个 `ReplyTo` 是**同一个字段,不同方向**,语义统一为"reply 关系中的被 reply 方"
- thread 概念(RootId / thread 顶层 / thread 中间层)彻底不进 nightme 数据模型
- Feishu 端 thread 视觉由 Channel 自管(Reply API path 参数语义由 Feishu 决定)

**对 dispatch 的影响**:`InboundMessage.ReplyTo` 在 dispatch 流里**未被任何逻辑使用**(`chatsession.QueueUserMessage` 用 `msg.ID`,binding 用 `msg.ChatID`)。所以 wire ReplyTo = ParentId 是**纯增量,不改现有行为**,只是补全数据模型,便于将来做"user 回复某条历史 message → 拉那条 context 进 prompt"。

---

## 3. 数据流(影响)

### 3.1 Inbound

```
Feishu SDK event(EventMessage{ChatId, ChatType, MessageId, ParentId, RootId, ...})
  └─ handleMessage (internal/channel/feishu/adapter.go)
     ├─ chatID = SessionChatID(event)             (原值透传)
     ├─ msg = channel.Message{
     │    ChatID:    chatID,
     │    MessageID: message.MessageId,
     │    ReplyTo:   message.ParentId,           // ← D3 wire
     │    Text:      text,
     │    UserID:    senderID(event),
     │    Time:      message.CreateTime,
     │    Attachments: attachments,
     │    // 无 ChatType 字段                     // ← D1
     │  }
     └─ publish a.incoming
        │
        ▼
gateway/inbound.Router.Dispatch (internal/gateway/inbound/inbound.go)
  ├ tryCommandDispatch → slash command path
  └ runtime.NewMessageDispatcher (internal/runtime/dispatcher.go)
     ├ cs = mgr.GetOrCreate(msg.ChatID, primary) // ChatID 字符串查找(无 ChatType 过滤)
     ├ cs.LookupSelectedAgentSession()            // lazy spawn
     ├ cs.EmitMessageState(userMsgID, MessageQueued)
     ├ cs.EmitMessageState(userMsgID, MessageSubmitted)
     └ cs.QueueUserMessage(chatsession.Message{ID: userMsgID, ...})
       └ MessageQueue → buildPromptLocked → AgentSession.Submit
          └ Prompt.LastMessageID = batch 最后一条消息 id(turn 锚点)
```

Inbound 方向:`ReplyTo` 填 `message.ParentId`,没有 ChatType 字段。

### 3.2 Outbound

```
cs.AgentEventBus.Publish(AgentEventEnvelope{...}) → runtime event handler
  ├ out := outbound.Translate(chatID, ev)         (internal/gateway/outbound/translate.go)
  ├ out.ReplyTo = userMsgID                        (= Prompt.LastMessageID)
  └ emitter.Send(ctx, out)
        │
        ▼
Adapter.Send(internal/channel/feishu/adapter.go)
  ├ msg.ChatID 透传给 sendContent / receiptFor
  ├ msg.ReplyTo 透传给 SendMessageText / SendCard
  └ sendViaLarkReply(POST /im/v1/messages/{rootID}/reply)
     rootID = msg.ReplyTo = 当前 turn 的 user message_id
     ↓
     Feishu 端:把 reply 视觉放到 user 当前 message 附近
               (thread 内/外由 Feishu 决定,nightme 不假设)
```

Outbound 方向:`ReplyTo` 锚到 user 当前 message,不爬 thread 树。

---

## 4. 不变式(Invariants)

SPEC §1.3 的不变式:

- **Gateway 不持有 ChatType**:Gateway 只见 `ChatID string`,假设所有 chat 同质
- **Channel 自管 chat 语义**:Channel 知道 chat 类型(DM/group/topic)、知道 thread 渲染,但只通过 `OutboundMessage` 暴露渲染能力,不污染 Gateway 数据模型
- **`InboundMessage.ReplyTo = message.ParentId`**(Feishu 语义下):thread 顶层 `RootId` 不进 nightme
- **`OutboundMessage.ReplyTo = Prompt.LastMessageID`**:bot reply 永远 anchor 到 user 当前 message,不爬 thread 树
- **不续接 Thread**:nightme 不主动追踪/创建 thread 上下文,不维护 thread 树;Feishu 端 thread 视觉由 Channel 自治
- **任何 Channel 都不引入 thread 概念**:nightme 数据模型永远不引入 thread 字段(`thread_ts` / `message_thread_id` / `is_threaded` / `thread_id` 等)。Channel 自管 thread 渲染细节(Feishu reply API path 参数 / Slack block kit / Telegram forum mode),但只通过 `OutboundMessage` 暴露能力,不污染 Gateway / ChatSession / Registry 数据模型

---

## 5. 落地位置

### 5.1 Docs

| 文件 | 内容 |
|------|------|
| `docs/SPEC.md` | §1.3 含 D1/D2/D3 的不变式;§5 schema 无 `ChatSessionEntry.ChatType` 字段;§3.1 DM vs Group 简化为"ChatID 唯一,Channel 自管 thread 渲染";§2.1/2.2 InboundMessage 描述 `ReplyTo = ParentId` |
| `docs/channel/feishu.md` | §13.11 决策记录(D1 ChatType 移除 + D2 topic_group 不特殊 + D3 ReplyTo = ParentId + RootId 不进);§13.10 配对说明 inbound 这一腿 |

### 5.2 Code

**Channel 包 / Feishu adapter**:
| 文件 | 落地形态 |
|------|------|
| `internal/channel/channel.go` | 只有 `ChatTypeP2P` / `ChatTypeGroup` 两个常量(Channel 包私有,供 chat 类型分类);无 `ChatTypeThread`、无 `IsDM` 辅助函数 |
| `internal/channel/feishu/adapter.go` | `handleMessage` 填 `ReplyTo: stringValue(message.ParentId)`;没有 `normalizeChatType` |

**Gateway**:
| 文件 | 落地形态 |
|------|------|
| `internal/messages/inbound.go` | `InboundMessage` 无 ChatType 字段、无 `IsDM()` |
| `internal/gateway/binding.go` | `BindingEntry` 只有 `ChatID` / `SessionID` / `Workspace` / `Agent` |
| `internal/gateway/gateway.go` | `Router.Bind` 写入的 `BindingEntry` 不带 ChatType |

**ChatSession / Registry**:
| 文件 | 落地形态 |
|------|------|
| `internal/chatsession/chatsession.go` | `ChatSession` 无 ChatType 字段 |
| `internal/chatsession/manager.go` | `Manager.RestoreFromRegistry` 不涉及 ChatType |
| `internal/registry/chat_session_entry.go` | `ChatSessionEntry` 无 ChatType JSON 字段 |

### 5.3 Test

- `internal/channel/feishu/adapter_test.go`:`TestHandleMessage_ReplyToFromParentId` 验证 event.Message.ParentId 出现在 `msg.ReplyTo`;ParentId 为空时 `msg.ReplyTo == ""`

### 5.4 Verify

- `go build ./...` 全绿
- `go test -race ./...` 全绿
- `go vet ./...` 干净

---

## 6. 迁移 / 兼容

### 6.1 Registry 兼容

历史版本写入的 `chat_sessions.json` 可能仍带 `chatType` 字段。

**策略**:
- Go JSON unmarshal **默认容忍未知字段**,文件能继续加载
- 该字段没有对应的结构体成员,永远读不到
- 不需要 migrate 脚本,因为数据不再被使用
- daemon 重启后再加载,无数据丢失

### 6.2 Forward compat

**任何 Channel 都不引入 thread 概念**(Devin 2026-08-03 21:11 拍板,B 选项):

- `internal/channel/channel.go` 的 `ChatTypeP2P/Group` 常量保留(Channel 包私有,供内部 chat 类型分类)
- `ChatTypeThread` 常量不存在(`"topic_group"` 命名也不再用)
- Channel 内部归一化只覆盖 DM / Group / NotSupported 三态
- Gateway / ChatSession / Registry **数据模型完全不变**,**永远不变**
- `OutboundMessage.ReplyTo` 在 Slack 下指向 `message_ts`,在 Telegram 下指向 `message_id`(都是 message 级 ID,**不是 thread 级 ID**),字段名不变,语义按 Channel 解释
- Slack 的 `thread_ts`、Telegram 的 `message_thread_id`、Discord 的 thread 概念**不进 nightme 数据模型**,仅 Channel 内部渲染时使用

### 6.3 不引入 forward-compat hook

本次决策**彻底关掉 thread 概念在 nightme 数据模型里的入口**:
- 不预留 thread tree 抽象
- 不预留 `IsThreaded` 标志
- 不预留 `thread_id` / `thread_ts` / `message_thread_id` 字段
- Channel 自管 thread 渲染细节,但只通过 `OutboundMessage` 暴露能力

如未来 Slack thread 等场景需要支持,在 Channel 包内自治实现,**不动 nightme 数据模型**。

---

## 7. 风险

| 风险 | 等级 | 缓解 |
|------|------|------|
| chat_sessions.json 加载问题 | 低 | Go JSON unmarshal 默认容忍未知字段,无需迁移 |
| `/status` 命令不再显示"DM/Group"标签 | 低 | 影响范围仅 `/status` 输出格式;若需要可后续按 ChatID 自行判定 |
| `InboundMessage.ReplyTo` wire 后无 dispatch 逻辑使用 | 极低 | 纯增量,不改现有行为;为将来"reply context pull"留接口 |
| Outbound `msg.ReplyTo = Prompt.LastMessageID` 已工作 | 零 | §13.10 已落地且测试覆盖 |
| topic_group 不特殊处理导致 thread 上下文丢失 | 中(产品视角) | 设计意图就是丢失;Feishu 端 thread 视觉保留,bot 仍能正常工作(每条 user msg 是独立 turn) |

---

## 8. 不做的事

- 不维护 thread 树 / 不爬 thread / 不拉 thread 上下文
- 不在 Gateway 里加 IsDM / ChatType / TopicGroup 任何分支
- 不动 Outbound `msg.ReplyTo` 的赋值(语义符合"点对点 ReplyTo")

- 不引入 forward-compat hook(彻底关 thread 概念入口)
- **任何 Channel 都不引入 thread 概念**(Slack thread_ts / Telegram message_thread_id / Discord thread 等不进 nightme 数据模型,仅 Channel 内部渲染时使用)

---

## 9. 验证点

- `internal/channel/feishu/adapter.go` 的 `handleMessage` 只读 `message.ParentId`,不读 `event.Message.RootId`
- `internal/messages/inbound.go` 的 `InboundMessage` 没有 ChatType 字段
- `internal/gateway/binding.go` 的 `BindingEntry` 没有 ChatType 字段
- `internal/chatsession/chatsession.go` 的 `ChatSession` 没有 ChatType 字段
- `internal/registry/chat_session_entry.go` 的 `ChatSessionEntry` 没有 ChatType 字段
- `internal/channel/feishu/adapter_test.go` 的 `TestHandleMessage_ReplyToFromParentId` 覆盖 inbound wire

---

## 10. 验收清单

- [x] `docs/SPEC.md` 含 D1/D2/D3 不变式;schema 无 ChatType 字段
- [x] `docs/channel/feishu.md` §13.11 决策记录完整(含"任何 Channel 都不引入 thread 概念")
- [x] `internal/channel/channel.go` 只有 `ChatTypeP2P/Group`;无 `ChatTypeThread`
- [x] `internal/channel/feishu/adapter.go` handleMessage wire `ReplyTo = message.ParentId`
- [x] `internal/channel/feishu/adapter.go` 无 `normalizeChatType`
- [x] `internal/messages/inbound.go` 无 ChatType 字段、无 `IsDM()`
- [x] `internal/gateway/binding.go` 无 `BindingEntry.ChatType`
- [x] `internal/gateway/gateway.go` `Router.Bind` 写入无 ChatType
- [x] `internal/chatsession/chatsession.go` 无 ChatType 字段
- [x] `internal/chatsession/manager.go` `RestoreFromRegistry` 不涉及 ChatType
- [x] `internal/registry/chat_session_entry.go` 无 ChatType JSON 字段
- [x] `internal/channel/feishu/adapter_test.go` 有 `TestHandleMessage_ReplyToFromParentId`
- [x] `chatTypeFromMessage` 在仓库中无残留(gateway 的 `handlers_*.go` 已拆到 `internal/gateway/inbound/` / `internal/gateway/outbound/` / `internal/gateway/observe/`)

---

## 11. 相关决策追溯

- **2026-08-03 17:04** — Devin 提问 "reply_to 没有真正用到对不对?",触发 `docs/channel/feishu.md` §13.10 调查,落地 outbound reply-in-thread
- **2026-08-03 20:35** — Devin 询问 "chatid 怎么设定的?",虾哥审计 chatID 数据流,识别 ChatType 命名空间不一致(channel `"topic_group"` vs gateway `"thread"`)
- **2026-08-03 20:44** — Devin 询问 Feishu chatid 类型,确认 3 种原生 + nightme 命名空间冲突
- **2026-08-03 20:49** — Devin 询问 DM reply-in-thread 传几个 ID,确认 ChatId 不变,RootId/ParentId 是 message 级
- **2026-08-03 21:04** — Devin 提出 ChatType 不进 Gateway 决策
- **2026-08-03 21:05** — Devin 提出 `ReplyTo = ParentId`,RootId 不进 nightme 决策
- **2026-08-03 21:07** — Devin 确认 topic_group 不丢弃,只点对点 ReplyTo
- **2026-08-03 21:08** — Devin 最终精炼:不续接任何 Thread,最多点对点 ReplyTo
- **2026-08-03 21:09** — Devin 要求综合方案 → 本文档
- **2026-08-03 21:11** — Devin 拍板 B:未来任何 channel 都不引入 thread 概念,Slack/Telegram/Discord 等的 thread 语义仅 Channel 内部渲染,nigthme 数据模型永远不变
