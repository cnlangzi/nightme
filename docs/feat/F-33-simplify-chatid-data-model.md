# F-33: Simplify ChatID Data Model — Drop ChatType, RootId, Wire ReplyTo

> **Status**: designing (Phase 1 — Devin review; 文档定稿后进 Phase 2 实现)
> **Milestone**: v1.3.x (post-§13.10)
> **Depends on**: F-08 (Channel abstraction), F-25 (rolling-log receipt), F-26 (Gateway), F-27 (ChatSession), F-29 (AgentSession pool)
> **Related docs**: [`SPEC.md`](../SPEC.md) v1.3 §1.2 / §1.3 / §2.1 / §2.2 / §3.1 / §5, [`channel/feishu.md`](../channel/feishu.md) §13.10 / §13.11(新增)

---

## 0. 摘要

在 v1.3 §13.10 落地 reply-in-thread 之后,我们对 chatID 数据模型做一次系统性清理,落地三个**收口决策**:

| # | 决策 | 影响范围 |
|---|------|----------|
| **D1** | **ChatType 不进 Gateway** | 删 `internal/gateway/messages.go` 的 ChatType 4 个常量、`InboundMessage.ChatType`、`IsDM()`、`BindingEntry.ChatType`、`ChatSession.ChatType`、registry 持久化字段 |
| **D2** | **topic_group 不特殊处理** | Feishu adapter 不丢弃、不分支,topic_group 跟普通 group 走相同路径 |
| **D3** | **`ReplyTo = ParentId`,RootId 不进 nightme** | Inbound wire `msg.ReplyTo = message.ParentId`;整个项目不读 `event.Message.RootId` |

**架构原则**:nightme 数据模型只追踪**点对点 reply 关系**,不追踪 thread 树。thread 是 Channel 渲染细节,Feishu 端决定 thread 视觉,nightme 内部完全不见。

---

## 1. Motivation

### 1.1 当前数据模型冗余

v1.3 落地后,我们审计 chatID 数据模型,发现三层未使用/不完整的抽象:

**ChatType**(Gateway 持有):
- `internal/gateway/messages.go:18-27` 定义 4 个常量:`ChatTypeP2P/Group/Thread/Other`
- `InboundMessage.ChatType ChatType` 字段携带(`messages.go:38`)
- `InboundMessage.IsDM()` 方法(`messages.go:66-73`),channel-neutral 但 Gateway 内部无人调用
- `BindingEntry.ChatType`(`binding.go:26`),注释说"carried for /status replies"
- `ChatSession.ChatType`(`internal/chatsession/chatsession.go:36`),构造/序列化/反序列化全有
- Registry 持久化字段(`internal/registry/chat_session_entry.go:29`)

实际用途盘点:**仅用于 `/status` 命令回复展示**。其余都是 metadata,Gateway 没有分支决策依赖它。

**RootId**(完全未 wire):
- Feishu SDK `EventMessage.RootId`(`oapi-sdk-go@v1.1.48/service/im/v1/model.go:244`)是 thread 顶层 message_id
- 项目内 `grep -rn "\.RootId" internal/` **0 命中**
- `InboundMessage.ReplyTo` 字段定义在 `gateway/messages.go:50`,注释说 "Feishu root_id",但 Feishu adapter `handleMessage` 构造 msg 时**不读 RootId**,也不读 ParentId

**ParentId → ReplyTo 接线**(未完成):
- `handleMessage`(`internal/channel/feishu/adapter.go:1593-1617`)构造 `channel.Message` 时 `ReplyTo` 永远是空字符串
- 结果:user 在 DM/group 里 reply-in-thread 发消息时,`InboundMessage.ReplyTo == ""`,dispatch 完全丢失 reply 关系

### 1.2 §13.10 之后语义不清

`docs/channel/feishu.md` §13.10 落地了 outbound 方向的 reply-in-thread(`msg.ReplyTo` 作为 Feishu root_id 投递,走 `POST /im/v1/messages/{rootID}/reply`)。但 inbound 方向仍没 wire,文档语义和实现不对齐。

更根本的问题:**"nightme 的 ReplyTo 字段到底代表什么"** 没明确定义。

可能的解读:
- (a) `ReplyTo` = "user 当前这条 message_id"(跟 MessageID 重复,无意义)
- (b) `ReplyTo` = "user reply 的目标 message_id"(`ParentId`)
- (c) `ReplyTo` = "thread 顶层 message_id"(`RootId`)

之前 ChatType 命名空间不一致(channel 包 `"topic_group"` vs gateway 包 `"thread"`)也是同一类问题:**抽象层定义时未对齐语义**。

### 1.3 §13.10 已落地的语义可以"收口"

§13.10 决议(2026-08-03)采用方案 B:adapter.Send 在 `msg.ReplyTo != ""` 时透传 rootID。这意味着 nightme 不需要知道 Feishu 的 thread 语义,只需要**点对点的 message_id 关系**。所有三个决策(D1/D2/D3)本质都是同一件事:**最小化数据模型,只保留必要维度**。

---

## 2. 决策

### D1: ChatType 不进 Gateway

**结论**:
- `internal/gateway/messages.go:18-27` 的 `ChatType` 类型 + 4 个常量全删
- `InboundMessage.ChatType` 字段删
- `InboundMessage.IsDM()` 方法删
- `BindingEntry.ChatType` 字段 + 注释删
- `ChatSession.ChatType` 字段 + 构造参数 + 序列化字段全删
- Registry `ChatSessionEntry.ChatType` JSON 字段 + 注释删
- `/status` 命令输出不再展示 "DM" / "Group" 标签(若需要,改成 "ChatID: oc_xxx")

**理由**:
- Gateway 只看 `ChatID string`,假设所有 chat 同质
- ChatType 当前唯一用途是 `/status` 命令展示,影响范围小
- 后续 Slack/Telegram 接入时,Channel 自管 chat 类型分类;Gateway 数据模型不变
- 删除后 binding 表 / 持久化 schema 更干净,ChatType 污染问题彻底解决

### D2: topic_group 不特殊处理

**结论**:
- Feishu adapter `handleMessage` **完全不看 chat_type**,只过滤 `chatID == ""`
- topic_group 消息跟普通 group 消息走**完全相同**的路径:
  1. `bindings[msg.ChatID]` 查 ChatSession(`oc_topic_group_xxx` 跟 `oc_group_xxx` 在 binding 表里平等)
  2. ChatSession 按 `MessageID` 跟踪 turn
  3. outbound `msg.ReplyTo = currentTurnUserMsgID`,Feishu Reply API 投递

**理由**:
- Feishu SDK `EventMessage.ChatId` 在 topic_group 下跟 group **同构**(都是 `oc_xxx`)
- Feishu 的 thread 是**消息级逻辑分组**,不是 chat 级 — 不创建新 chat_id
- topic_group 在 nightme 内部就是"group,thread 在外面",符合"Channel 自管 thread 渲染"原则

### D3: ReplyTo = ParentId,RootId 不进 nightme

**结论**:
- Inbound `msg.ReplyTo = message.ParentId`(Feishu 原生 `parent_id` 字段)
- Outbound `msg.ReplyTo = currentTurnUserMsgID`(不变,§13.10 已落地)
- **`RootId` 整个项目永远不读、不存、不传**

**理由**:
- `ReplyTo` 字段统一语义 = "**被 reply 的那条 message_id**"
- Inbound 角度:user 主动 reply 的目标(`ParentId`)
- Outbound 角度:bot reply 的目标(user 当前 message_id,因为 bot turn 锚到它)
- 两个 `ReplyTo` 是**同一个字段,不同方向**,语义统一为"reply 关系中的被 reply 方"
- thread 概念(RootId / thread 顶层 / thread 中间层)彻底不进 nightme 数据模型
- Feishu 端 thread 视觉由 Channel 自管(Reply API path 参数语义由 Feishu 决定)

**对当前 dispatch 的影响**:`InboundMessage.ReplyTo` 在 dispatch 流里**当前未被任何逻辑使用**(`chatSession.QueueUserMessage` 用 `msg.MessageID`,binding 用 `msg.ChatID`)。所以 wire ReplyTo = ParentId 是**纯增量,不改现有行为**,只是补全数据模型,便于将来做"user 回复某条历史 message → 拉那条 context 进 prompt"。

---

## 3. 数据流(影响)

### 3.1 Inbound

```
Feishu SDK event(EventMessage{ChatId, ChatType, MessageId, ParentId, RootId, ...})
  └─ handleMessage (internal/channel/feishu/adapter.go)
     ├─ chatID = message.ChatId                  (原值透传,不变)
     ├─ msg = channel.Message{
     │    ChatID:    chatID,                     // 不变
     │    MessageID: message.MessageId,          // 不变
     │    ReplyTo:   message.ParentId,           // ← 新增 wire (D3)
     │    Text:      text,                       // 不变
     │    UserID:    senderID(event),            // 不变
     │    Time:      message.CreateTime,         // 不变
     │    Attachments: attachments,              // 不变
     │    // ChatType 字段删除                    // ← D1 移除
     │  }
     └─ publish a.incoming
        │
        ▼
Gateway.dispatchLoop → Handle(ctx, msg)
  ├ ParseCommand → slash command path → 不变
  └ messageDispatcher(ctx, msg)
     ├ gateway.bindings[msg.ChatID]               // ChatID 字符串查找(无 ChatType 过滤)
     ├ cs.emitMessageState(msg.MessageID, ...)   // 不变
     ├ chatSession.LookupSelectedAgentSession()    // 不变
     ├ cs.emitMessageState(msg.MessageID, ...)   // 不变
     └ chatSession.QueueUserMessage(blocks, msg.MessageID)
       └ InputBuffer FSM (idle ↔ busy)
          └ currentTurnUserMsgID = msg.MessageID   (不用 ReplyTo)
```

**变化点**:
- `ReplyTo` 字段被填(ParentId)
- `ChatType` 字段不再设
- 其余路径不变

### 3.2 Outbound

```
ChatSession.onAgentEvent(s, ev) → ChatSession.EventCallback
  ├ out := gateway.Translate(chatID, ev)
  ├ out.ReplyTo = cs.currentTurnUserMsgID        (不变,§13.10 已落地)
  └ channel.Send(ctx, out)
        │
        ▼
Adapter.Send(internal/channel/feishu/adapter.go)
  ├ msg.ChatID 透传给 sendContent / receiptFor    (不变)
  ├ msg.ReplyTo 透传给 SendMessageText / SendCard (不变,§13.10 已落地)
  └ sendViaLarkReply(POST /im/v1/messages/{rootID}/reply)
     rootID = msg.ReplyTo = currentTurnUserMsgID  (user 当前 message_id)
     ↓
     Feishu 端:把 reply 视觉放到 user 当前 message 附近
               (thread 内/外由 Feishu 决定,nightme 不假设)
```

**变化点**:无。§13.10 已落地,本次 PR 不改 outbound 代码。

---

## 4. 不变式(Invariants)

新增到 SPEC §1.3:

- **Gateway 不持有 ChatType**:Gateway 只见 `ChatID string`,假设所有 chat 同质
- **Channel 自管 chat 语义**:Channel 知道 chat 类型(DM/group/topic)、知道 thread 渲染,但只通过 `OutboundMessage` 暴露渲染能力,不污染 Gateway 数据模型
- **`InboundMessage.ReplyTo = message.ParentId`**(Feishu 语义下):thread 顶层 `RootId` 不进 nightme
- **`OutboundMessage.ReplyTo = currentTurnUserMsgID`**:bot reply 永远 anchor 到 user 当前 message,不爬 thread 树
- **不续接 Thread**:nightme 不主动追踪/创建 thread 上下文,不维护 thread 树;Feishu 端 thread 视觉由 Channel 自治
- **任何 Channel 都不引入 thread 概念**:nightme 数据模型永远不引入 thread 字段(`thread_ts` / `message_thread_id` / `is_threaded` / `thread_id` 等)。Channel 自管 thread 渲染细节(Feishu reply API path 参数 / Slack block kit / Telegram forum mode),但只通过 `OutboundMessage` 暴露能力,不污染 Gateway / ChatSession / Registry 数据模型

---

## 5. 文件改动清单

### 5.1 Docs(先动,按工作流偏好)

| 文件 | 改动 |
|------|------|
| `docs/SPEC.md` | §1.3 不变式新增 4 条(D1/D2/D3);§5 schema 删 `ChatSessionEntry.ChatType` 字段;§3.1 DM vs Group 简化为"ChatID 唯一,Channel 自管 thread 渲染";§2.1/2.2 InboundMessage 描述加 `ReplyTo = ParentId`;§11 backlog 勾掉本项 |
| `docs/channel/feishu.md` | 加 **§13.11 决策记录**(D1 ChatType 移除 + D2 topic_group 不特殊 + D3 ReplyTo = ParentId + RootId 不进);§13.10 配对说明 inbound 这一腿 |

### 5.2 Code

**Channel 包 / Feishu adapter**:
| 文件 | 改动 |
|------|------|
| `internal/channel/channel.go:46-48` | 删 `ChatTypeThread` 常量;`ChatTypeP2P/Group` 常量保留(Channel 包私有,供 chat 类型分类) |
| `internal/channel/feishu/adapter.go:1599-1610` | `handleMessage` 加 `ReplyTo: stringValue(message.ParentId)`;删 `ChatType: gateway.ChatType(chatType)` |
| `internal/channel/feishu/adapter.go:1722-1738` | 删 `normalizeChatType` 函数(无用,ChatType 不再 set) |

**Gateway**:
| 文件 | 改动 |
|------|------|
| `internal/gateway/messages.go:18-27` | 删 `ChatType` 类型 + 4 个常量 |
| `internal/gateway/messages.go:38` | 删 `InboundMessage.ChatType` 字段 |
| `internal/gateway/messages.go:66-73` | 删 `IsDM()` 方法 |
| `internal/gateway/binding.go:26` | 删 `BindingEntry.ChatType` 字段 + 注释 |
| `internal/gateway/handlers_chatsession.go:276-282` | 删 `chatTypeFromMessage`（handlers 文件后已统一迁移；`chatTypeFromMessage` 在仓库中无残留，可直接 grep 验证） |
| `internal/gateway/gateway.go:515-523` | BindingEntry 写入去掉 ChatType 字段 |

**ChatSession / Registry**:
| 文件 | 改动 |
|------|------|
| `internal/chatsession/chatsession.go:36` | 删 `ChatType` 字段 |
| `internal/chatsession/chatsession.go:130` | 构造参数删 |
| `internal/chatsession/chatsession.go:668` | `ChatSessionEntry` 序列化去字段 |
| `internal/chatsession/manager.go:165` | `RestoreFromRegistry` 不传 ChatType |
| `internal/registry/chat_session_entry.go` | 删 JSON 字段 + 注释更新 |

### 5.3 Test

- `internal/channel/feishu/adapter_test.go`:加 `TestHandleMessage_ReplyToFromParentId`(verify event.Message.ParentId 出现在 `msg.ReplyTo`;ParentId 为空时 `msg.ReplyTo == ""`);现有 ChatType 相关 assertion 改/删
- `internal/gateway/*_test.go`:删/改 ChatType 相关 test
- `internal/chatsession/*_test.go`:删/改 ChatType 相关 test

### 5.4 Verify

- `go build ./...` 全绿
- `go test -race ./...` 全绿
- `go vet ./...` 干净

---

## 6. 迁移 / 兼容

### 6.1 Registry 兼容

`ChatSessionEntry.ChatType` 字段删除后,**已存在的 `chat_sessions.json` 文件里仍有这个字段**。

**策略**:
- Go JSON unmarshal **默认容忍未知字段**,旧文件能继续加载
- `entry.ChatType` 永远是零值,不会被读取(已删除)
- 不需要 migrate 脚本,因为数据不再被使用
- 旧 daemon 重启 + 新 daemon 加载,无数据丢失

### 6.2 Forward compat

**任何 Channel 都不引入 thread 概念**(Devin 2026-08-03 21:11 拍板,B 选项):

- `internal/channel/channel.go` 的 `ChatTypeP2P/Group` 常量保留(Channel 包私有,供内部 chat 类型分类)
- `ChatTypeThread` 常量删除(`"topic_group"` 命名也不再用)
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
| 旧 `chat_sessions.json` 加载问题 | 低 | Go JSON unmarshal 默认容忍未知字段,无需迁移 |
| `/status` 命令不再显示"DM/Group"标签 | 低 | 影响范围仅 `/status` 输出格式;若需要可后续按 ChatID 自行判定 |
| `InboundMessage.ReplyTo` wire 后无 dispatch 逻辑使用 | 极低 | 纯增量,不改现有行为;为将来"reply context pull"留接口 |
| Outbound `msg.ReplyTo = currentTurnUserMsgID` 已工作 | 零 | §13.10 已落地且测试覆盖 |
| topic_group 不特殊处理导致 thread 上下文丢失 | 中(产品视角) | 设计意图就是丢失;Feishu 端 thread 视觉保留,bot 仍能正常工作(每条 user msg 是独立 turn) |

---

## 8. 不做的事

- 不维护 thread 树 / 不爬 thread / 不拉 thread 上下文
- 不在 Gateway 里加 IsDM / ChatType / TopicGroup 任何分支
- 不动 Outbound `msg.ReplyTo` 的赋值(已落地 §13.10,语义符合"点对点 ReplyTo")

> **2026-08-04 更新**：原 §8 第一条 ("不实现 reply_in_thread:true") 在 F-37 子决议已落地 —— `OutThinking` / `OutToolStart` / `OutToolEnd` / `OutCompaction` 这四条 path 现在显式设 `reply_in_thread=true`，让中间过程只在线程面板可见。这一层仍然是 Channel (Feishu SDK) 自治决定，不进 nightme 数据模型；只是 SDK body 的字段，不破坏"不爬 thread / thread 不进 ChatSession"不变式。详见 `docs/feat/F-37-tool-thread-routing.md` §2.1 + `docs/channel/feishu.md` §13.10 子决议。
- 不引入 forward-compat hook(彻底关 thread 概念入口)
- **任何 Channel 都不引入 thread 概念**(Slack thread_ts / Telegram message_thread_id / Discord thread 等不进 nightme 数据模型,仅 Channel 内部渲染时使用)

---

## 9. 实施顺序

按 MEMORY.md 工作流偏好 **先 docs 再 code**:

1. **Step 1 - Docs(本 PR 第一个 commit)**:`docs/SPEC.md` + `docs/channel/feishu.md` 落地决策记录
2. **Step 2 - Code(本 PR 第二个 commit)**:按 "Channel → Gateway → ChatSession → Registry → Test" 顺序
3. **Step 3 - Verify**:`go build ./...` + `go test -race ./...` + `go vet ./...`
4. **Step 4 - Commit**:1 个 docs commit + 1 个 code commit(分两个便于 review)

**PR 标题候选**:
- `refactor(chatid): drop ChatType from Gateway, wire ReplyTo = ParentId`
- `chore(data-model): simplify chatID abstraction per F-33`

---

## 10. 验收清单

- [ ] `docs/SPEC.md` 不变式新增 5 条;schema 删 ChatType 字段
- [ ] `docs/channel/feishu.md` §13.11 决策记录完整(含"任何 Channel 都不引入 thread 概念")
- [ ] `internal/channel/channel.go` 删 `ChatTypeThread` 常量;保留 `ChatTypeP2P/Group`
- [ ] `internal/channel/feishu/adapter.go` handleMessage wire `ReplyTo = message.ParentId`
- [ ] `internal/channel/feishu/adapter.go` 删 `normalizeChatType`
- [ ] `internal/gateway/messages.go` 删 ChatType 类型 + 常量 + 字段 + IsDM
- [ ] `internal/gateway/binding.go` 删 BindingEntry.ChatType
- [x] `internal/gateway/handlers_chatsession.go` 删 chatTypeFromMessage（已完成 + handlers 文件整段迁移；`chatTypeFromMessage` 在仓库中已无残留）
- [ ] `internal/gateway/gateway.go` BindingEntry 写入去 ChatType
- [ ] `internal/chatsession/chatsession.go` 删 ChatType 字段
- [ ] `internal/chatsession/manager.go` RestoreFromRegistry 去 ChatType
- [ ] `internal/registry/chat_session_entry.go` 删 JSON 字段
- [ ] `adapter_test.go` 加 `TestHandleMessage_ReplyToFromParentId`
- [ ] 现有测试更新(删/改 ChatType assertion)
- [ ] `go build ./...` 全绿
- [ ] `go test -race ./...` 全绿
- [ ] `go vet ./...` 干净

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
