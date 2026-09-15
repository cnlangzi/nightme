# Telegram Channel - Topic 方案与接入设计

> **Scope**: nightme Telegram Bot API 适配器（`internal/channel/telegram/*`）
> **目的**: 在 Telegram Forum Supergroup 中，将主窗口作为会话入口，将每个 qino 会话映射为一个 Topic，并在 Topic 内承载占位状态、thinking、工具调用、结果和交互卡。
> **Related docs**:
>
> - [feishu.md](./feishu.md) - 飞书 receipt、reply-in-thread 和交互卡实现
> - [F-08-channel-abstraction.md](../feat/F-08-channel-abstraction.md) - Channel 抽象与 Gateway 边界
> - [F-message-flow.md](../feat/F-message-flow.md) - 消息生命周期
> - [SPEC.md](../SPEC.md) - 统一消息模型
>
> **Telegram 官方文档**:
>
> - [Telegram Bot API](https://core.telegram.org/bots/api)
> - [`createForumTopic`](https://core.telegram.org/bots/api#createforumtopic)
> - [`Message`](https://core.telegram.org/bots/api#message)
> - [`sendMessage`](https://core.telegram.org/bots/api#sendmessage)
> - [`editMessageText`](https://core.telegram.org/bots/api#editmessagetext)
> - [`ReplyParameters`](https://core.telegram.org/bots/api#replyparameters)
> - [`InlineKeyboardMarkup`](https://core.telegram.org/bots/api#inlinekeyboardmarkup)
> - [`setMessageReaction`](https://core.telegram.org/bots/api#setmessagereaction)

## 1. 核心对应关系

Telegram 不提供飞书 `root_id + reply_in_thread` 的等价线程树，但 Telegram Forum Topic 可以承担“会话分组容器”的职责：

| 飞书体验 | Telegram 形态 | nightme 负责维护的标识 |
| --- | --- | --- |
| 用户在主窗口发消息 | 主窗口或 General Topic 中的一条用户消息 | `chat_id + message_id` |
| 飞书 thread root | 一个 Telegram Forum Topic | `message_thread_id` |
| 飞书 receipt 卡 | per-turn 单一 rich message（`sendRichMessage` + `editMessageText(rich_message=…)`） | rich turn `messageID` |
| thinking | rich turn 内一段 paragraph block | rich turn entries 序号 |
| tool start / tool end | rich turn 内相邻的 paragraph block | rich turn entries 序号 |
| 交互 Choice | Topic 内的 `InlineKeyboardMarkup` 消息 | 消息自身的 `message_id` |
| 飞书 Choice 点击 | callback query，按 `message_id` 找回原 Choice | callback query 的 `message_id` |
| reaction | 对用户原消息或 result 消息调用 `setMessageReaction` | `chat_id + message_id` |

核心原则：

1. **主窗口只作为入口**，不在主窗口发送 qino 的 thinking、工具、进度和 receipt。
2. **一个 qino 会话对应一个 Telegram Topic**，Topic 内可以积累任意数量的消息。
3. **Topic 本身不是占位卡**。Topic 是一条消息容器，不能用 `editForumTopic` 更新占位正文。
4. **Per-turn 状态由单一 rich message 承载**：每条 user message 触发一个独立 rich message，后续 Out* 都通过 `editMessageText(rich_message=…)` PATCH 同一条消息；OutResult 独立发送，🎉 落在 result 消息上。
5. Telegram Topic 消息通过共同的 `message_thread_id` 分组，不存在可依赖的 `root_id -> children` 线程树。

## 2. Per-turn rich message 生命周期

### 2.1 Topic 路由

用户在主窗口发起消息后，适配器把 `chat.id` + `message_thread_id` 解析为 TopicState key：

```text
主窗口用户消息 (thread_id=0)
      │
      ▼
TopicState{ChatID, TopicID: 0, ChatKind}
      │
      ▼
InBoundMessage.ChatID = "tg_<chat.id>"
```

```text
群内 topic 42 用户消息 (thread_id=42)
      │
      ▼
TopicState{ChatID, TopicID: 42, ChatKind}
      │
      ▼
InboundMessage.ChatID = "tg_<chat.id>:42"
```

`ChatKind` 由 `ClassifyChat(Message.Chat.Type)` 在 `ensurePlaceholder` 时收敛：

| Telegram 字面 chat.type | nightme ChatKind |
| --- | --- |
| `private` | `private` |
| `group` / `supergroup`（Forum 开关不影响） | `group` |
| `channel` | `channel`（outbound silent-drop） |
| 其它 / 空 | `group`（defensive fallback） |

TopicState 持久化字段：

```text
ChatID
TopicID
ChatKind        (private / group / channel)
LegacyChatType  (read-only migration 字段)
DraftMessageID  (liveDraft simulated DraftMessage 的真实 message_id；fallback 字段，当前 turn 主要从 in-memory liveDraftEntry.messageID 读)
UserMessageID   (本次 turn 的 user message_id)
PlaceholderMessageID (read-only 兼容字段；新 turn 不再写)
LastMessageID
CreatedAt / UpdatedAt
```

### 2.2 创建 per-turn rich message

`ensurePlaceholder` 在 `handleMessage` 同步路径里 eager 创建 per-turn rich message：

```text
Topic
├─ User message (thread_id = X)
└─ 🤖 Working...                ← per-turn rich message (sendRichMessage)
   └─ messageID = P1
```

DM 私聊下不创建这个 rich message（draft 是 live surface，empty placeholder 只是噪音）；后续 `OutReply` / `OutResult` / `OutError` 等需要真实消息的 Out* 走 `appendRichTurn` 的 lazy path，冷创建并 render 第一段内容。

非 DM（supergroup / basic group）一律 eager 创建 — 用户在第一段 reply 到达前就看到"agent 收到了，正在处理"。

### 2.3 原位更新 rich message

后续每条 Out* 都通过 `editMessageText(rich_message=…)` PATCH 同一条 rich message，把新一段 block 追加到 entries 列表：

```text
editMessageText(
    chat_id = rawChatID,
    message_id = richTurn.messageID,
    rich_message = {"blocks": [
        {"type":"paragraph","text":"💭 thinking"},   ← heartbeat header
        {"type":"divider"},
        {"type":"paragraph","text":"● Read(...)"},   ← OutToolStart
        {"type":"paragraph","text":"⎿  📄 Read → 47 lines"},   ← OutToolEnd
        ...
        {"type":"divider"},
        {"type":"footer","text":"🤖 claude · opus-4-5 · …"}   ← StatusBar
    ]}
)
```

250ms debounce 合并 burst edit，避免短时间内多次 PATCH 同一消息。OnPromptEnded 时停止 debounce timer 并同步 flush，让 🎉 reaction 落在最终完整渲染上。

### 2.4 OutResult 独立发送

OutResult 不进 rich turn（否则 result 跟中间产物视觉同质，丢失 "成品输出" 的 UX 信号）。`sendOutResultMessage` 调 `sendRichMessage(rich_message[blocks])` 单独发一条 reply-anchored 消息；只有这条 result 消息的 messageID 进入 `richTurn.resultMessageID`，OnPromptEnded 的 🎉 优先贴这条消息。

```text
Topic
├─ User message (thread_id = X)
├─ 🤖 Working...                ← rich turn PATCH 多段
│   ├─ 💭 thinking
│   ├─ ● Read(...)
│   ├─ ⎿  📄 Read → 47 lines
│   └─ ⎿  📂 Glob → 1 file
├─ Result body (paragraph / heading / pre blocks)   ← sendRichMessage
│   ─────────
│   🤖 claude · opus-4-5 · sess-1   ← footer block
└─ [🎉 reaction on result message]

## 3. 事件映射

每条 Out* 通过 `Send()` 路由到以下三条出站路径之一：

| nightme `OutboundMessage` | 出站路径 | wire form |
| --- | --- | --- |
| `OutReply` / `OutCommandReply` / `OutThinking` / `OutToolStart` / `OutToolEnd` / `OutError` / `OutTaskCreate` / `OutTaskUpdate` | rich turn（per-turn 单一 rich message） | `appendRichTurn` / `appendRichTurnAndFlush` → `editMessageText(rich_message=…)` |
| `OutResult` | 独立 sendRichMessage | `sendOutResultMessage` → `sendRichMessage(rich_message[blocks])`，reply-anchored 到 user message |
| `OutChoice` / `OutChoicePatch` | 独立 InlineKeyboard 消息 | `sendChoice` / `patchChoice` → `sendRichMessage(rich_message=…)` / `editMessageText(rich_message=…)` |
| `OutHeartbeat` | rich turn header PATCH | `patchChainHeader` → `updateRichTurnHeader`（header line 走 `paragraph` block，无 footer） |
| `OutMessageState` / `OutMessageStateRemoved` | reactions 独立轨道 | `setMessageReaction` 贴到 user message（v6.3 单 reaction 预算） |
| `OutInit` | silent drop | — |

`OutThinking` / `OutToolStart` / `OutToolEnd` 在 DM 和 group / forum topic 下走**同一条** unified 路径（§11.12.1 simulated DraftMessage），都跟 rich turn 并行 — DraftMessage 是这三类事件的 live streaming surface，rich turn 是其它事件的承载面。Draft path 失败时还原 buffer（issue #391）下次重试；最终落败才 DROP。

视觉形态：

```text
Topic
├─ User message (thread_id = X)
├─ Per-turn rich message                ← editMessageText(rich_message=…) PATCH 多段
│   ├─ heartbeat header paragraph
│   ├─ divider
│   ├─ 💭 thinking paragraph
│   ├─ ● Tool(args) paragraph
│   ├─ ⎿ result paragraph
│   ├─ divider
│   └─ footer block (StatusBar)
└─ Result 独立消息                        ← sendRichMessage, reply_to_message_id=userMsgID
    ├─ body blocks (paragraph / heading / pre / list / quote / table)
    ├─ divider
    └─ footer block (StatusBar)
    [🎉 reaction on this message]
```

## 4. Telegram 不应强行模拟的部分

### 4.1 不模拟任意消息的线程树

Telegram 的 `message_thread_id` 是 Topic 分组标识，不等于飞书 thread root。没有通用的“把任意消息挂到任意 thread root 下”的 API，也不应依赖一个可查询的 child 消息树。

需要表达直接回复时使用：

```json
{
  "message_id": 1002,
  "allow_sending_without_reply": true
}
```

`ReplyParameters.message_id` 只建立回复关系，不会把回复消息变成另一个 Topic，也不会建立飞书式 root/children 结构。

### 4.2 不把 Topic 当作可编辑文本

Topic 元数据更新使用 `editForumTopic`，占位正文更新使用 `editMessageText`。两者不能混用：

```text
editForumTopic       -> 修改 Topic 名称、图标
editMessageText      -> 修改 Topic 内某条普通消息
closeForumTopic      -> 关闭 Topic
```

### 4.3 不依赖 Telegram 原生消息折叠

Telegram Topic 可以包含很多消息，但没有“Topic 默认折叠”或“任意普通文本消息默认收起”的 Bot API。用户需要在客户端中进入 Topic 查看全部 thinking、tools 和结果。

qino 可以通过占位消息、摘要和状态更新控制噪音，但不应宣称这是 Telegram 原生折叠。

## 5. 标识与持久化

### 5.1 chatID 命名空间（InboundMessage.ChatID）

每条 Telegram update 映射到 InboundMessage.ChatID 时，**先加 `tg_` 前缀**:

```go
// telegram adapter 的纯函数
func sessionChatID(rawChatID string, threadID int) string {
    const prefix = "tg_"
    if threadID > 0 {
        return prefix + rawChatID + ":" + strconv.Itoa(threadID)
    }
    return prefix + rawChatID
}
```

| 场景 | Telegram 原生字段 | **InboundMessage.ChatID** |
| --- | --- | --- |
| DM (private) | chat.id = `8684538097` | `tg_8684538097` |
| 群主窗口 (Forum 关) | chat.id = `-10012345` | `tg_-10012345` |
| 群主窗口 (Forum 开) | chat.id = `-10012345`, thread_id = 0 | `tg_-10012345` |
| 群内 topic 42 | chat.id = `-10012345`, thread_id = 42 | `tg_-10012345:42` |
| 群内 topic 88 | chat.id = `-10012345`, thread_id = 88 | `tg_-10012345:88` |

**稳定性约束**（核心）：

1. chatID 必须是 update 内容的纯函数 — 不依赖 daemon state、不依赖 config
2. 同一 DM / 同一群 / 同一 topic 跨 daemon 重启 / 升级 / 状态文件丢失, chatID 永远一致
3. **不允许在 chatID 拼接中引用任何运行时状态**（如自动创建的 sentinel topic ID）
4. **不允许在 chatID 拼接中引用任何配置**（如 `topic_mode: separate` vs `shared`）
5. 旧 binary 不会产生新格式的 chatID —— 升级到本版本后启动时跑一次迁移,把旧的纯数字 chatID 加 `tg_` 前缀

反向 split (`splitSessionID`) 也必须是纯函数,确保 inbound 和 outbound 两侧始终看到同一 chatID。

### 5.2 不需要 ChatSession 加 Telegram 专用字段

修订前的设计曾考虑在 ChatSession 加 Telegram 专用字段（`TelegramChatID` / `TelegramTopicID` / `TelegramPlaceholderID`）。**修订后不需要**——这些状态走 Telegram adapter 自己的 state file,ChatSession 完全不感知 IM 协议细节:

```text
ChatSession          ← chatsession 包,不知道 telegram
├── ChatID            ← tg_<chat.id>[:thread_id]   (跟其他 channel 共用)
├── SelectedCwd       ← /cwd 设
├── SelectedAgent     ← /use 设
└── InputBuffer FSM   ← 通用
```

`tg_` 前缀让 ChatSession 这层就跟飞书的 `oc_<hex>` 一样不透明——它只是收到一个 string,用它作 chatstore 的 key。

### 5.3 路由规则（基于 tg_ 前缀）

发送消息时的拆分:

```go
// 解析 chatID → (rawChatID, threadID)
func splitSessionID(sessionID string) (rawChatID string, threadID int, ok bool) {
    if !strings.HasPrefix(sessionID, "tg_") {
        return "", 0, false
    }
    body := sessionID[3:]                          // strip "tg_"
    if idx := strings.Index(body, ":"); idx < 0 {
        return body, 0, true
    }
    tid, _ := strconv.Atoi(body[idx+1:])
    return body[:idx], tid, true
}
```

发送消息时的路由规则:

```text
threadID > 0
  ├── sendMessage / sendPhoto / sendDocument / sendMediaGroup
  │     └── 携带 message_thread_id = threadID
  └── editMessageText / editMessageReplyMarkup
        └── 使用 chat_id + message_id，不需要 message_thread_id

threadID == 0
  ├── sendMessage → 主窗口 / 私聊（带 reply_to_message_id = userMsgID；topic 模式下额外带 message_thread_id。v3 修订）
  └── editMessageText → 占位消息（v3：每 turn 一个新占位，跨 turn 留作时间线状态标记；详见 §11.11）
```

```

所有回调处理都必须以 `callback_query.message.message_id` 为准查找原卡；不能只按 `callback_data` 中的 `message_id` 盲信，因为 Telegram 的 `callback_data` 是适配器自己编码的字段。

## 6. 接入前置条件

Topic 方案要求：

1. 群组是 **Forum Supergroup**；普通群组没有 Forum Topic。
2. 群组已开启 Topics。
3. Bot 是群组成员，并具备创建/管理 Topic 所需的权限；建议配置为管理员。
4. Bot 使用长轮询（`getUpdates`）只订阅 `Message` 与 `CallbackQuery` 两类 update；所有其它 update 类型（`message_reaction` / `message_reaction_count` / `chat_member` / `my_chat_member` / `edited_message` / `channel_post` 等）一律不下发，避免任何非用户主动消息的事件推送到 `incoming` 通道。每个 Bot 只能有一个 `getUpdates` consumer,daemon 重启时用持久化的 `update_id + 1` 继续消费。
5. 私聊场景 **不做** Forum Topic 分流。Bot API 10.3 起 DM 在 BotFather 启用 Topic Mode 后支持 `createForumTopic` / `message_thread_id > 0`(`Bot Platform Developer Terms of Service` §6.2.6),但仅 "one or more eligible TPAs they own" 可在 BotFather 看到该开关,eligibility 由 Telegram 单方决定;启用后该 TPA 内 Stars 购买按 15% 非退款抽成,且 `closeForumTopic` / `reopenForumTopic` 仍不支持私聊。综合考虑 eligibility 不可控、合规绑定和 API 缺口,nightme 维持 DM 走主窗口堆叠(每 turn 一条 `<b>🤖 Working...</b>` 占位 + reply 链 userMsgID),不在 adapter 增加第三路径。决策记录见 §15 N7。
6. Topic 内发送的所有事件都必须显式携带正确的 `message_thread_id`。

## 7. 故障与边界

- Topic 被关闭：使用 `reopenForumTopic` 重新打开；是否允许 Bot 继续发送以实际群组权限和 Telegram 客户端行为为准。
- Topic 被删除或无法继续使用：新建 Topic，并更新 `message_thread_id`；需要决定是否迁移占位状态。
- 占位消息被用户删除：重新发送状态消息并替换 `placeholder_message_id`，不能继续调用旧的 `editMessageText`。
- 消息超过 Telegram 文本长度限制：按 API 限制拆分，结果消息保留顺序并明确 continuation。
- Bot 被移出群组或权限变化：进入降级路径；在主窗口发送一次不可恢复的连接错误，而不是继续静默丢弃。
- 多个 chat 共享一个 Telegram 群组：按 `tg_<chat.id>:<thread_id>` 路由,不能只靠群组 `chat.id`——chatID 必须含 thread_id 才能 partition。
- 同一 Topic 被重复创建：Telegram 原生 message_thread_id 唯一——adapter 只需用 thread_id 而非自建 sentinel topic,见 §5.1 修订。

## 8. 实施顺序

### Phase 1：基础 Topic 收发

- 增加 Telegram 配置、Bot token 和长轮询接收。
- 实现私聊和群组消息的基础 `InboundMessage` 映射。
- 实现 Topic 查重、创建和持久化。

### Phase 2：占位消息与事件详情

- 在 Topic 中发送 `placeholder_message_id`。
- 将 `OutThinking`、`OutToolStart`、`OutToolEnd` 追加到 Topic。
- 使用 `OutHeartbeat` 更新占位消息。
- 实现 Topic 消息去重、限流和错误降级。

### Phase 3：交互与完整体验

- 将 Feishu card 的按钮映射为 Telegram `InlineKeyboardMarkup`。
- 将 card action 映射为 `CallbackQuery`。
- 将 card PATCH 映射为 `editMessageText` / `editMessageReplyMarkup`。
- 将 message-state 映射为 `setMessageReaction`。
- 实现附件、文件、Topic 内媒体发送和错误反馈。

### Phase 4：质量与运维

- 增加主窗口不发送中间事件、Topic 内事件完整累积的回归测试。
- 增加 daemon 重启后的 Topic 恢复测试。
- 增加 `message_thread_id` / `message_id` 持久化与迁移测试。
- 增加权限、限流、Topic 关闭和消息删除测试。

## 9. 验收标准

- 同一 DM / 同一群 / 同一 topic 跨 daemon 重启, chatID 永远稳定 (`tg_<chatid>[:thread_id]`) —— 见 §5.1 稳定性约束。
- 升级到本版本后, 启动时跑一次迁移把旧的纯数字 chatID 加 `tg_` 前缀 —— 见 §5.1 稳定性约束 5。
- 每个 qino 会话默认只有一个工作 Topic，Topic 内允许按时间顺序出现多条消息。
- Topic 内的占位状态可以通过 `placeholder_message_id` 原位更新。
- thinking、tool start、tool end 全部在 Topic 内可见，顺序不丢失。
- 交互卡、reaction、附件和错误消息都指向正确的 Topic 消息 ID。
- Topic 不可用时有明确降级和恢复策略，不会静默丢失用户请求。

## 10. 方案确定：用户自建 Bot，daemon 直连 Telegram

本方案**不采用集中式 Relay**。每个 nightme 部署实例使用自己的 Telegram Bot，直连 Telegram Bot API：

```text
用户机器 A                                      用户机器 B
┌──────────────────────────┐                   ┌──────────────────────────┐
│ nightme daemon           │                   │ nightme daemon           │
│ ├─ Telegram Bot Token A  │                   │ ├─ Telegram Bot Token B  │
│ ├─ Telegram Adapter      │                   │ ├─ Telegram Adapter      │
│ └─ ChatSession           │                   │ └─ ChatSession           │
└────────────┬─────────────┘                   └────────────┬─────────────┘
             │ Bot API                                  │ Bot API
             └──────────────► Telegram ◄─────────────────┘
```

每个 Bot 只服务一个 nightme 部署实例。用户 daemon 自己完成：

- 接收 Telegram update
- 维护群组和 Topic 绑定
- 调用 `sendMessage` / `editMessageText` / `setMessageReaction` 等 Bot API
- 下载附件和转发给 Agent
- 处理 callback query、消息状态和错误恢复

### 10.1 为什么不做共享 Bot Relay

集中式 Bot 可以让用户扫码后直接添加官方 Bot，但会引入一个中心服务、共享 Token、用户到 daemon 的路由、daemon 心跳、媒体转发和更复杂的安全边界。

本次选择自建 Bot，保留当前飞书的“每个用户自己运行、自己持有凭证、自己直连 IM”的结构：

```text
一个 nightme daemon
    = 一个 Telegram Bot Token
    = 一个 Telegram Bot identity
```

不要让多个 daemon 使用同一个 Bot Token，也不要让多个 daemon 同时对同一个 Token 调用 `getUpdates`。一个 Token 只能有一个 update consumer，否则会发生消息竞争、重复消费和丢消息。

### 10.2 用户开通流程

Telegram 没有类似飞书注册 API 的“扫码后自动创建 Bot”能力。Bot 只能通过 `@BotFather` 官方流程创建：

```text
1. 用户打开 @BotFather
2. 执行 /newbot
3. 输入 Bot 显示名称
4. 输入以 bot 结尾的唯一 username
5. 保存 BotFather 返回的 Bot Token
6. 执行 nightme login telegram
7. CLI 打印 BotFather 走查步骤 + 提示输入 Token
8. 输入 Token 后自动调用 getMe 校验（拒绝 user account）
9. CLI 校验通过后原子写入 config.yaml（chmod 0600）
10. 将 Bot 添加到已开启 Topics 的 Forum Supergroup
11. 启动 nightme daemon（nightme start --channel=telegram）
```

`nightme login telegram` 的实现要点：

- 不做 QR 扫码（飞书模式）。Telegram 没有第三方代注册 bot 的接口。
- 打印 @BotFather 走查步骤（12 行说明，适配 80x24 终端）。
- 从 stdin 读取 token，过滤空白行，10 分钟超时。
- 调用 `getMe` 校验 token 是否属于 bot 账号（拒绝 user account token）。
- 校验通过后回写 `cfg.Telegram.BotToken`，原子写入 config.yaml。
- 也支持手动编辑 `~/.nightme/config.yaml` 的 `telegram.bot_token`
  字段（适用于无法用交互式 CLI 的场景，例如远程 headless 服务器）。

### Greeting 发送流程

`nightme login telegram` 在 token 写入 config 后会**主动发送**问候消息给 bot 的 owner：

1. CLI 提示用户在 Telegram 客户端打开 `@<bot_username>`，发送任意消息（`/start` 是惯例）。
2. CLI 启动 getUpdates long-polling（25 秒每次，最多 2 分钟），等待**第一条私聊**消息。
3. 收到合法私聊消息后，CLI 用 sendMessage 把 canonical greeting（只发英语
   副本）发给 owner 的 chat_id。
4. 2 分钟内用户没消息：CLI 友好提示"你可以稍后 /start"，不报错。
   daemon 启动后用户消息进来时会正常处理。

实现细节：

- 只接私聊消息（chat.type == "private"）；群消息跳过，避免在群里广播 greeting。
- 跳过 bot-from 消息（防止把别的 bot 的消息误认为 owner）。
- greeting 内容：`Hi, this is NightMe 👋.` /
  `Run your local coding agents. Control them from your phone, on your terms 🚀`
  只发英语：Telegram 没有飞书那种 post 双语信封，默认英语客户端也
  不会渲染中文，多发只会成噪音。中文副本留给飞书的 post envelope。
- 错误是 soft-failure：greeting 失败不回滚 token 写入。

Bot Token 属于敏感凭证，应像 App Secret 一样保存在本机配置中，不得通过群消息、命令行参数日志或普通遥测上报。配置应遵守现有 nightme 凭证权限和敏感字段脱敏规则。

### 10.3 可提供有限的二维码体验

用户完成 BotFather 创建后，可以由 `nightme login telegram` 生成：

```text
https://t.me/<bot_username>?startgroup
```

该链接可以让用户扫码后选择群组并把 Bot 添加进去，但它不能代替 BotFather 创建 Bot，也不能自动读取或下发 Bot Token。

因此二维码体验应描述为：

```text
用户创建 Bot → 粘贴 Token → 扫码添加 Bot 到群组 → 开始使用
```

而不是描述为“扫码后自动创建 Telegram Bot”。

### 10.4 接入方式

nightme 只支持 Long Polling 模式,不实现 Webhook。每个 Bot 必须由持有它的 daemon 自己消费 `getUpdates`,**不能把同一个 Bot Token 复制给多个 daemon** —— 否则会重复消费或丢消息。

`TelegramConfig` 字段只有两个:

```yaml
telegram:
  bot_token: "<BotFather token>"
  polling_timeout: 30   # getUpdates 长轮询秒数,1-50,默认 30
```

### 10.5 Topic 路由与消息发送

Telegram 消息通过 `chat.id` + `message_thread_id` 两个 native 字段拼出稳定的 InboundMessage.ChatID,不再由 daemon 自建 sentinel topic:

```text
主窗口用户消息 (thread_id=0)
       │
       ▼
chatID = "tg_<chat.id>"             ← 主窗口直接用 chat.id
   │
   ▼
InboundMessage 进入 chatstore
（chatstore 的 key = "tg_<chat.id>"）


群内 topic 42 用户消息 (thread_id=42)
       │
       ▼
chatID = "tg_<chat.id>:42"          ← 真有 topic 时加 thread_id
   │
   ▼
InboundMessage 进入 chatstore
（chatstore 的 key = "tg_<chat.id>:42"）
```

所有发送方法都携带 Topic ID（仅当 thread_id > 0 时）：

```json
{
  "chat_id": -1001234567890,
  "message_thread_id": 42,
  "rich_message": {"blocks": [...]}
}
```

每条用户消息进来 → `ensurePlaceholder` 在 Topic 内（或 DM 主窗口）**冷创建一条 rich message**（DM 下不创建，draft 是 live surface）。后续所有 Out* 通过 `editMessageText(rich_message=…)` 原地 PATCH 这条消息，turn 终态由 `OnPromptEnded` 同步 flush + 在 result 消息（fallback rich message）上贴 🎉 reaction。详细 spec 见 §11.11。

`editForumTopic` 只修改 Topic 名称或图标，不修改 Topic 内正文，**不能**用来做 rich message 内容更新。

### 10.6 与现有架构的映射

现有飞书适配器可以保持业务层不变，Telegram 只需要实现 `channel.Channel` 的等价接口：

| 通用能力 | Telegram Adapter |
| --- | --- |
| `Start` | 启动 Polling 接收循环 |
| `Incoming` | 发布转换后的 `messages.InboundMessage` |
| `Send` | 发送 `OutReply`、`OutResult`、工具和错误消息 |
| `Send` | 所有 `OutboundKind` 的唯一出口；交互选择也走这里 |
| `OnPromptEnded` | flush rich turn + 在 result 消息（fallback rich message）贴 🎉 / ❌ reaction + 清 draft streamer |
| `HealthSnapshot` | 汇报 API、Polling 和 Topic 状态 |
| `SetLogger` | 输出 Telegram 收发和重试日志 |
| `BuildBlocks` | 将文本、caption、附件转换为统一内容块 |

Gateway、Chatsession 和 Agent 继续使用现有 `messages` 类型，不感知 Telegram 的 BotFather、Bot Token、Forum Topic 或 callback query。

### 10.7 该模式的边界

- 用户必须先通过 `@BotFather` 手动创建 Bot。
- Bot Token 不能共享,不能由多个 daemon 共同轮询。
- 群组可以是普通群(不开启 Forum)或 Forum Supergroup —— 都可以聊天,只有后者有 message_thread_id 概念(见 §5.1)
- 私聊没有 Forum Topic,直接走 chat_id
- 用户需要自行配置 Bot 的群组权限;如果 Bot 没有发消息或管理 Topic 的权限, qino 必须在 reply 里明确报错,而不是静默丢弃
- 由于 Bot 是用户自己的, qino 只能控制该 Bot, 不能帮助用户恢复或修改 BotFather 账号凭证
- 二维码可以简化"把已有 Bot 添加到群组"的步骤, 但不能实现飞书式的全自动应用注册

### 10.8 本方案验收补充

- 每个 daemon 只使用一个自己的 Bot Token
- `getUpdates` 更新不会被其他 daemon 重复消费
- Bot Token、用户文本和附件不会写入普通日志
- 用户通过 BotFather 创建 Bot 后, 可以按 CLI 引导完成 Token 配置、群组添加和 daemon 启动
- 同一 DM / 同一群 / 同一 topic 跨 daemon 重启, chatID 永远稳定 (`tg_<chatid>[:thread_id]`)
- daemon 重启后可以恢复 `chat_id + message_thread_id + placeholder_message_id` (topic 内的占位消息)
- 用户从 `@BotFather` 执行 `/token` 或 `/revoke` 后, qino 能在下次启动时检测凭证失效并给出明确修复提示

## 11. 用户自建 Bot 与多群组开通指南

本节是用户实际使用时的推荐操作流程。它适用于“一个 nightme daemon 使用一个用户自建 Bot，并手动把 Bot 加入多个群组”的模式。

### 11.1 前置准备

用户需要准备：

- 一个用于运行 nightme 的账号或设备
- 一个 Telegram 账号
- 至少一个 Telegram 群组
- 如果使用 Forum Topic，需要将群组创建为 Forum Supergroup
- BotFather 创建的 Bot Token

每个 nightme daemon 只能使用自己的 Bot Token。一个 Token 不要被多个 daemon 同时使用。

### 11.2 通过 BotFather 创建 Bot

1. 在 Telegram 搜索并打开 `@BotFather`。
2. 发送：

   ```text
   /newbot
   ```

3. 按提示输入 Bot 的显示名称，例如 `NightMe Coding`。
4. 按提示输入 Bot username。username 必须唯一，并且通常以 `bot` 结尾，例如：

   ```text
   nightme_coding_bot
   ```

5. 复制 BotFather 返回的 Token。Token 格式类似：

   ```text
   123456789:AAExampleSecretToken
   ```

6. 不要把 Token 发到群组、截图、Issue 或普通日志中。

如果 Token 泄露，应在 BotFather 中使用 `/revoke` 撤销，然后重新生成 Token。

### 11.3 配置 Bot 加入群组

在每个要使用的群组中手动操作：

```text
打开群组 → 群组信息 → 添加成员 → 搜索 Bot username → 添加
```

Bot 加入群组后，在 BotFather 中确认 `/setjoingroups` 没有被关闭。正常情况下应允许 Bot 加入群组。

如果要监听群组里的全部普通消息，还需要在 BotFather 中执行：

```text
/setprivacy
```

然后选择：

```text
Disable
```

关闭 Privacy Mode 后，Bot 才能收到群组中的普通消息；开启 Privacy Mode 时，Bot 主要只能收到命令、回复和 mention。

Privacy Mode 的开关是 BotFather 配置，Telegram Bot API 没有对应的修改接口。因此首次开通时需要用户手动完成一次。

### 11.4 开启 Forum Topics

每个需要 qino 使用 Topic 的群组都必须是 Forum Supergroup：

1. 在 Telegram 客户端创建或转换群组为 Supergroup。
2. 开启群组的 Topics 功能。
3. 确认 Bot 已加入该群组。
4. 为 Bot 授予发送消息以及创建、管理 Topic 所需的权限。
5. 推荐将 Bot 设为管理员，避免因普通成员权限变化导致无法发送或维护 Topic。

普通群组没有 Forum Topic；这种群组只能使用普通消息模式。

### 11.5 推荐配置示例

用户完成 BotFather 和群组配置后，在 nightme 中配置：

```yaml
telegram:
  bot_token: "<BotFather token>"
  polling_timeout: 30
```

`TelegramConfig` 只有这两个字段。群组 mention gate 不再走配置,而是按 `chatsession.WatchMode`(`/watch all|mention|off`)在 chatsession 层判定 —— 详见 §11.7。

### 11.6 多群组如何工作

同一个 Bot 可以加入多个群组：

```text
                    ┌─► 群组 A chat_id=-100111
Telegram getUpdates ─┼─► 群组 B chat_id=-100222
                    └─► 群组 C chat_id=-100333
                              │
                              ▼
                    Telegram Adapter
                              │
              ┌───────────────┼───────────────┐
              ▼               ▼               ▼
            会话 A          会话 B          会话 C
          (tg_-100111)   (tg_-100222)   (tg_-100333)
```

每次收到消息后，适配器使用以下字段路由（拼出 `tg_` 前缀的 chatID）：

```text
chat.id + message_thread_id
    │
    ├─ thread_id == 0  → "tg_<chat.id>"              (DM / 群主窗口)
    └─ thread_id  > 0  → "tg_<chat.id>:<thread_id>"  (群内 topic)

from.id              → qino UserID
message.message_id   → qino MessageID
```

建议的持久化关系（chatstore key 已经是 `tg_` 前缀）：

```text
chat_id                        ← "tg_<digits>" or "tg_<digits>:<thread_id>"
  └── user_id
        └── message_thread_id  ← Telegram 内部用,不入 ChatSession
              └── placeholder_message_id  ← Telegram adapter 自己的 state file
```

不同的 Topic 永远走不同 ChatSession（chatID 已含 topic 后缀,天然 partition）：

```text
群组 A (-100111)
├── Topic 42 → 会话 "tg_-100111:42"
├── Topic 88 → 会话 "tg_-100111:88"
└── 主窗口    → 会话 "tg_-100111"          (thread_id=0)
```

**`topic_mode: separate` / `shared` 配置不存在**：`tg_<chat.id>:<thread_id>` 拼接已经天然把每个 topic 隔成独立 ChatSession，不同 topic 永久走不同 cs，不需要 sentinel topic / shared mode 这些复杂机制。旧 binary 过渡说明见 §5.1 末"稳定性约束 5"。

### 11.7 主窗口、Topic 和监听模式

主窗口消息直接在主窗口回,不再创建 sentinel topic。Bot 收到群主窗口消息后,chatID = `tg_-10012345` (无 thread_id 后缀),所有回复走主窗口:

```text
群主窗口 (Forum-enabled)
└── 用户消息 (thread_id=0)
    └── adapter 拼 chatID = "tg_-10012345"
        └── 走普通 slash / agent 流程
            ├── thinking → 独立消息
            ├── tool start → 独立消息
            ├── tool end → 独立消息
            └── result → 独立消息
```

```text
群内 topic 42
└── 用户消息 (thread_id=42)
    └── adapter 拼 chatID = "tg_-10012345:42"
        └── 走普通 slash / agent 流程 (与主窗口一样)
            ├── thinking → 独立消息
            ├── tool start → 独立消息
            ├── tool end → 独立消息
            └── result → 独立消息
```

**Sentinel topic 不存在**：原来"主窗口创建 nightme sentinel topic"流程不存在。`tg_<chat.id>:<thread_id>` 拼接让 chatID 是 (chat, topic) 二元组的纯函数——不需要 Telegram 分配 sentinel topic；sentinel topic ID 不可控，daemon 重启 / state 丢失会导致 ID 漂移，违反 chatID 稳定性约束。

DM / 群主窗口（thread_id=0）和真实 topic（thread_id > 0）走**统一的 per-turn rich message**方案：

- 每个用户消息进来 → `ensurePlaceholder` **新建**一条 rich message（`sendRichMessage(rich_message=…)`），DM 下不创建（draft 是 live surface，empty placeholder 只是噪音）。
- 同一 turn 的所有 OutXxx（`OutReply` / `OutThinking` / `OutToolStart` / `OutToolEnd` / `OutResult` / `OutError` / `OutChoice`）走 `appendRichTurn` / `appendRichTurnAndFlush`，由 `renderRichTurnBlocksLocked` 渲染为同一 rich message 的多段 blocks；非 DM 时携带 `reply_to_message_id = UserMessageID` + `message_thread_id = thread_id`，DM 下 reply-to 仍带，message_thread_id 不带。
- `OutResult` 单独 `sendRichMessage` 走独立消息，reply-anchored 到 user message，不进 rich turn。
- turn 状态走 **message reaction**：runtime `MessageStateBus` → `OutMessageState` 触发 `setMessageReaction(userMsgID, 👌)`（v6.3 单 reaction 预算）；`OnPromptEnded` 在 result 消息（fallback rich message）上贴 🎉 / ❌。
- `OutHeartbeat` PATCH 当前 turn 的 rich message header（`💭 N · 🔧 M · ⏱ HH:MM:SS`）—— 是 in-turn 状态 ticker，单独 paragraph block，不带 footer。
- 跨 turn：老 rich message 留作历史证据（不再被 PATCH），新 turn 创建新 rich message 独立承载新状态。

详细 spec 见 §11.11。

如果群组没有开启 Privacy Mode，Bot 会收到更多群组普通消息。qino 不应默认把每条群消息都交给 Agent，而应继续遵守群组 mention 策略：

| 消息类型 | 默认行为 |
| --- | --- |
| 私聊普通消息 | 处理 |
| 私聊命令 | 处理 |
| 群组 `/command` | 处理 |
| 群组回复 Bot 消息 | 处理 |
| 群组 `@BotUsername` mention | 处理 |
| 群组普通消息，`watch_mode=all` | 处理 |
| 群组普通消息，`watch_mode=mention` | 丢弃 |

`watch_mode=mention` 是 qino 的业务层控制，不等同于 Telegram Privacy Mode。Privacy Mode 控制 Bot API 是否收到消息，mention gate 控制收到后是否交给 Agent。

### 11.8 二维码的实际使用边界

用户创建 Bot 并配置 Token 后，CLI 可以展示：

```text
https://t.me/<bot_username>?startgroup
```

用户扫码后， Telegram 会让用户选择群组并把该 Bot 添加进去。

该二维码**不能**：

- 自动创建 Telegram Bot
- 自动生成或传递 Bot Token
- 自动关闭 Privacy Mode
- 自动把普通群组转换成 Forum Supergroup
- 自动授予 Bot 管理员权限

因此推荐的用户引导文案是：

```text
1. 先在 BotFather 创建自己的 Bot
2. 将 Token 填入 nightme
3. 扫码把 Bot 添加到目标群组
4. 如需监听群组全部消息，请在 BotFather 关闭 Privacy Mode
5. 如需 qino Topic，请将群组设置为 Forum Supergroup
```

### 11.9 故障排查

#### Bot 加入群组但收不到普通消息

检查：

1. Bot 是否仍在群组内。
2. Bot 是否被授予管理员权限。
3. BotFather 的 `/setprivacy` 是否为 Disable。
4. nightme 是否只配置了 group mention gate，导致普通消息被业务层丢弃。
5. `getUpdates` 是否被其他 daemon 错误地同时消费。

#### Bot 收得到消息但无法创建 Topic

检查：

1. 群组是否开启 Topics。
2. 群组是否是 Supergroup。
3. Bot 是否有创建和管理 Topic 的权限。
4. 发送消息时是否携带正确的 `message_thread_id`。

#### 重启后群组没有恢复

Telegram 没有提供“列出 Bot 已加入全部群组”的 Bot API 接口。daemon 应通过更新流接收新消息，并持久化已收到的 `chat_id`、`user_id` 和 `message_thread_id`；不要把“从本地配置文件恢复完整群组列表”当作 Telegram 提供的功能。

重启后使用 `update_id + 1` 作为 `getUpdates` offset,继续消费尚未确认的更新。daemon 持久化 `offset` 到 telegram_state.json,重启后无缝接续。

### 11.10 开通验收

- 用户能通过 `@BotFather /newbot` 创建 Bot 并安全保存 Token。
- Bot 能被手动加入多个群组。
- 关闭 Privacy Mode 后，群组普通消息可以到达 nightme。
- `chat_id` 能区分不同群组，相同群组内 `message_thread_id` 能区分不同 Topic。
- Topic 内可以按顺序看到 thinking、tools、结果和交互卡。
- 群组 mention gate 和私聊行为符合预期。
- Token 失效、权限不足、群组不是 Forum 等错误都有明确提示。

### 11.11 per-turn rich message（单 rich message 原位 PATCH）

每个用户消息进来 → adapter 在 Topic（或 DM 主窗口）里**冷创建**一条 `rich_message` 消息，后续所有 Out* 通过 `editMessageText(rich_message=…)` 原位 PATCH 这条消息；turn 结束由 OnPromptEnded 同步 flush + 贴 🎉 reaction。`richTurn`（`internal/channel/telegram/rich_turn.go`）维护这份 per-turn in-memory state。

#### 11.11.1 数据模型

```go
type richTurn struct {
    mu             sync.Mutex
    chatID         string
    topicID        int
    userMessageID  int           // turn anchor；per-turn key 末尾
    messageID      int64         // Telegram message_id，0 = 未冷创建
    headerLine     string        // heartbeat header 文本
    hasContent     bool          // entries / task / footer-bearing 已落地
    entries        []richTurnEntry
    taskList       []taskListItem
    footer         []string      // StatusBar 三行 footer
    dirty          bool
    debounceTimer  *time.Timer
    resultMessageID int64        // OutResult 独立消息 id（OnPromptEnded 🎉 优先锚点）
}
```

`richTurnsIndex` 按 `(chatID, topicID, userMessageID)` 索引，cap = 1000（FIFO evict，不是 LRU — turn 结束主动 purge，index 短周期）。

#### 11.11.2 cold-create 时机

`ensurePlaceholder` 在 `handleMessage` 同步路径里决定：

| ChatKind | 行为 |
| --- | --- |
| `private`（DM） | **不** 冷创建 rich turn（draft 是 live surface，empty placeholder 只是噪音）。后续 `OutReply` / `OutResult` / `OutError` 需要真实消息时由 `appendRichTurn` lazy 路径冷创建。 |
| `group`（supergroup / basic group） | 一律 eager 冷创建：用户先看到 `"🤖 Working..."` paragraph，agent 第一次真正活动之前不空等。 |
| `channel` | silent drop |

#### 11.11.3 Out* 路径

`Send()` switch 按 kind 分派：

| Kind | 路径 |
| --- | --- |
| `OutReply` / `OutCommandReply` | `appendSegmentForKind` → `appendRichTurn`（debounced PATCH） |
| `OutThinking` / `OutToolStart` / `OutToolEnd` | 先调 `streamDraftEvent`（DM/group draft path）失败或非 draft 路径则 `appendRichTurn`/`appendAndFlush`；`OutToolEnd` 走同步 flush 让 `● Tool(args)` + `⎿ result` 紧邻出现 |
| `OutTaskCreate` / `OutTaskUpdate` | `setRichTurnTaskList`（taskList section） |
| `OutError` | `appendRichTurn`（stderr tail 拼成 fenced-code block） |
| `OutHeartbeat` | `patchChainHeader` → `updateRichTurnHeader`（只动 headerLine，不带 footer） |
| `OutResult` | `sendOutResultMessage` → `sendRichMessage(rich_message[blocks])`，reply 到 user message，独立消息；resultMessageID 写入 turn |
| `OutChoice` / `OutChoicePatch` | `sendChoice` / `patchChoice`（独立 InlineKeyboard 消息） |
| `OutMessageState` / `OutMessageStateRemoved` | `setMessageReaction`（reactions 独立轨道） |
| `OutInit` | silent drop |

#### 11.11.4 Compose 规则（`renderRichTurnBlocksLocked`）

rich turn flush 时按以下顺序组装 blocks：

```text
[headerLine paragraph]    ← hasHeartbeat 时 always render；headerLine == default "🤖 Working..."
                             且 hasContent 时 skip（避免 stale "Working..." 跟 body 抢戏）
[divider]                  ← header 跟 body 之间（entries / taskList 都空时省略）
[entry paragraph ×N]      ← 每条 Out* 一段；OutError 走 markdownToRichBlocks 出 pre block
[task list block]          ← OutTask* 触发；空 taskList 不出现
[divider]                  ← body 跟 footer 之间
[footer block]             ← footer 三行（statusbar.StatusBarLines(&msg) 非空时）
```

- header / footer 是预烘焙文本（`<b>` 是字面量，rich block 不解析 HTML，避免二次转义）
- entries body走 `markdownToRichBlocks`（walker 拒收形状退化成 paragraph block）
- footer 是 Telegram `{"type":"footer","text":RichText}` block（Bot API 10.1+），客户端渲染为 muted caption region

#### 11.11.5 debounce 250ms

`appendRichTurn` 末尾 `scheduleRichTurnFlush`：同 turn 内 250ms burst 合并成 1 次 `editMessageText(rich_message=…)`。`OnPromptEnded` 同步 flush + stop debounce timer，避免 turn 终态还在等 timer。

#### 11.11.6 Footer 内存语义

每 turn 最多一个 footer block，data-driven：

- 事件携带 status 数据（`AgentName` / `Model` / `SessionID` / `Usage` / `GitStatus` 任意非零）→ `statusbar.StatusBarLines(&msg) != nil` → 刷 `turn.footer`
- 事件不携带 status 数据 → footer 不动
- Kind 不锁 policy：runtime 在哪个 kind 上 stamp status 字段由 runtime 决定，rich turn 照收

flush 总是发生（rich turn 没有 chain 的 ROTATE 概念）：turn.dirty = true 就会触发 PATCH，无论 footer 变没变。

#### 11.11.7 终态

`OnPromptEnded` 流程：

1. 停止 pending debounce timer
2. 同步 `flushRichTurn` —— 让 🎉 落在已完整渲染的 final state 上
3. 选 🎉 锚点：`turn.resultMessageID > 0`（本 turn 收到 OutResult）→ 用 result 消息；否则回退 `turn.messageID`（rich turn placeholder）
4. `setMessageReaction(targetID, 🎉)`；`reason.IsError()` 时换 ❌（user message slot 不动，v6.3 单 reaction 预算守）
5. `richTurns.purge(chatID, topicID, userMessageID)` —— turn 结束清 in-memory state
6. `liveDraft.endProcess(...)` —— DM/group liveDraft surface 清理（详见 §11.12.1 unified simulated DraftMessage）

`reason.IsError()` 时 🎉 换 ❌ —— 用 reaction 表达错误终态，不动 user message slot。

#### 11.11.8 restart 行为

rich turn 不持久化。daemon 重启 = in-memory rich turn 失。turn  N  已发送的 rich message 留在 Telegram chat（不会消失，没人 PATCH）。下次 user message 进来 `ensurePlaceholder` 冷创建新 rich message。

LRU cap = 1000（按 user message 计，cap = 1000 个并发 turn）。FIFO evict（不是 LRU —— turn 结束主动 purge，index 不需要 access order 跟踪）。

#### 11.11.9 测试契约

`internal/channel/telegram/rich_turn_test.go` + `adapter_test.go` 锁死的契约：

| 测试 | 锁死什么 |
| --- | --- |
| `TestRenderRichTurnBlocksLocked_HeaderOnly` | 冷创建 rich turn render 单一 paragraph block |
| `TestRenderRichTurnBlocksLocked_HeaderAndBody` | header + divider + body 三段结构 |
| `TestRenderRichTurnBlocksLocked_FooterAfterEntries` | footer block 位于 entries 之后，跟 body 之间有 divider |
| `TestRenderRichTurnBlocksLocked_HeaderSkippedWhenDefaultAndHasContent` | `headerLine == "🤖 Working..."` 且 `hasContent` 时 banner 隐藏（避免 stale alive 信号） |
| `TestRenderRichTurnBlocksLocked_TaskListSection` | taskList 非空时 render heading + list block |
| `TestRenderRichTurnBlocksLocked_EmptyTurnFallback` | 空 entries / 空 taskList / 空 footer 仍产生合法 blocks 数组（`{"blocks":[{"type":"paragraph","text":""}]}`） |
| `TestAppendRichTurn_ColdCreatesWhenMessageIDZero` | 首次 appendRichTurn 调 `sendRichMessage` |
| `TestAppendRichTurn_SubsequentCallsEdit` | 后续 appendRichTurn 调 `editMessageText(rich_message=…)` |
| `TestAppendRichTurnAndFlush_Synchronous` | OutToolEnd 的同步 flush 让 Start + End 紧邻 render |
| `TestScheduleRichTurnFlush_Debounce250ms` | burst 30 events 合并成 1 次 edit |
| `TestFlushRichTurn_NoOpWhenClean` | dirty=false 时不调 API |
| `TestFlushRichTurn_RendersHeaderBodyFooter` | 三段全在 |
| `TestOnPromptEndedRichTurn_StampsOnResultMessage` | turn 有 OutResult → 🎉 贴 resultMessageID，不贴 rich turn messageID |
| `TestOnPromptEndedRichTurn_FallsBackToRichTurnMessageID` | turn 无 OutResult → 回退 rich turn messageID |
| `TestOnPromptEndedRichTurn_PurgesTurn` | flush 完 purge turn 出 index |
| `TestRichTurnsIndex_FIFOEviction` | cap 满后 evict 最老的（按插入顺序） |
| `TestRichTurnsIndex_PurgeRemovesKey` | purge 后 lookup miss |
| `TestRenderMarkdownToRichBlocks_HeadingFenceListTable` | walker 渲染 L2 验证矩阵 |
| `TestRenderMarkdownToRichBlocks_StrikethroughQuirk` | `~~strike~~` 渲染为字面 `~~strike~~`（rich block 无 strike entity） |

## 11.12 OutThinking / OutToolStart / OutToolEnd 的 live streaming surface

三类事件在 DM / group / forum topic 下都走同一条 unified 路径：bot-owned rich message + `editMessageText(rich_message=…)` PATCH in place + `deleteMessage` 清理。**没有 ChatKind 分支**：DM 跟 group 共享 `groupDraftManager`（命名沿用历史，行为已统一）。行为对位飞书 receipt 的"live streaming"语义：用户进入 chat 立刻看到当前 think/tool 进度，turn 终止后自动消失；用户仍可照常打字 / interject，bot 不锁 send button。

### 11.12.1 统一流式表面（DM + group / forum topic）

所有 `ChatKind != "channel"` 的 turn 都过同一条路径：bot 发一条 rich message → 持续 `editMessageText(rich_message=…)` PATCH 最新 5 thinking + 最新 5 tool blocks → turn end `deleteMessage` 清理。模拟 sendMessageDraft 的"live streaming surface"角色。

**为什么不直接用 `sendMessageDraft`（plain text）/ `sendRichMessageDraft`（rich blocks）**：

- Telegram `sendMessageDraft` 是 DM-only API（spec："target private chat"，basic group 直接 `Bad Request`），无法覆盖 forum topic / basic group 场景
- 实机验证 animated draft 行为：bot 调 `sendMessageDraft` 后，client 端 send button 会显示三点动画，期间 user 不能发送新消息——这对"在 turn 期间 user 想 interject / 补充上下文"的常见 UX 是不可接受的
- 选择真实 rich message（用户可照常打字）+ turn end `deleteMessage` 自动消失，UX 与 animated draft 等价但没有 send button 锁

**核心对位**（与 §11.11 rich turn 对齐）：

| 维度 | Telegram v11.12 统一流式表面 | Feishu receipt |
| --- | --- | --- |
| 存储 | bot 拥有的真实 rich message（in-memory `entry.messageID`） | receipt card（in-memory） |
| 创建 | 第一次 flush 触发 `sendRichMessage(rich_message={"blocks":[…]})` 返回 message_id | `SendMessageReceipt` 首次 render |
| 更新 | 后续 flush 触发 `editMessageText(message_id=…, rich_message={"blocks":[…]})` | `PatchMessage` 增删 div |
| 触发 | 10s timer / endProcess（**无 count 触发**） | Out* event 立即 |
| Send window | 每次 flush 取各 stack 最新 5（共 5+5 = 10 blocks，稳在 Telegram 长度限制内） | full receipt 累积 |
| Windowed flush | 每次成功 flush 后清掉取走的 entries，其余留在 buffer | n/a |
| Delete at turn end | bot `deleteMessage` + drop entry | receipt 保留作为时间线证据 |
| 锚点 | `reply_to_message_id = userMessageID`（cold-create 时携带） | thread reply chain |
| 线程隔离 | `message_thread_id = topicID`（forum topic 内 DraftMessage 留在 topic；DM 下 topicID == 0 不带） | thread_id |
| 失败语义 | sendRichMessage / editMessageText 失败统一保留 buffer，下次 trigger 续试（issue #391 contract） | retry / fall through |

**两个独立 FIFO 栈，各容量 50**：

| 栈 | 类型 | Cap | 内容 |
| --- | --- | --- | --- |
| `thinkingStack` | `[]richTurnEntry` | 50 | OutThinking 事件；满 50 → FIFO evict 最旧 |
| `toolsStack` | `[]toolSlot` | 50 slots | 每个 slot = 1 OutToolStart + N 个 OutToolEnd；新 Start 开新 slot，满 50 → FIFO evict 最旧 slot |

**每次 flush 行为**：

1. 从 `thinkingStack` 取最新 5（不足则全取）→ 渲染成 paragraph blocks
2. 从 `toolsStack` 取最新 5 slots → 每个 slot 渲染成 [Start?, End1, End2, …] blocks
3. 合并成一段 `{"blocks":[…]}` JSON
4. `sendRichMessage`（首次）/ `editMessageText`（后续）发给 Telegram
5. 失败时把取走的 entries 还原（issue #391）

5 thinking + 5 tools = 10 blocks × ~1KB ≈ 10KB，远低于 Telegram 4096 字符限制或 rich_message block 上限。

**Lifecycle**（per turn，ChatKind == "private" / "group" 共用）：

```text
turn start
  ensurePlaceholder(...)
    ├─ state.ChatKind 写入
    └─ 内存分配新 groupDraftEntry（无 state 持久化）

turn N 期间，OutThinking / OutToolStart / OutToolEnd
  streamDraftEvent → groupDraft.streamDraftEvent
    ├─ append 到对应 stack：
    │   OutThinking  → thinkingStack（满 50 → FIFO evict 最旧）
    │   OutToolStart → toolsStack 新 slot（满 50 → FIFO evict 最旧 slot）
    │   OutToolEnd   → 最后一个 slot 的 ends（无 open slot → 孤儿 slot，body 当 Start 渲染）
    └─ 启动 10s flush timer（若 idle）

flush 触发
  - 10s timer 到期
  - endProcess / OnPromptEnded
  行为：
  - entry.mu 持锁下 pop 最新 5+5 → 释放 entry.mu
  - entry.flushMu 持锁下 sendRichMessage / editMessageText → API call（**不持 buffer lock**）
  - 失败时 entry.mu 持锁下还原 entries

turn N ends
  OutResult / OnPromptEnded → endProcess(ctx, rawChatID, topicID, parsedUserMsgID)
    ├─ Stop flush timer
    ├─ 若 buffer 非空 → flushLocked（最后 flush）
    └─ deleteMessage(entry.messageID) + drop entry
```

**关键 lock 纪律**：

`entry.mu`（buffer mutation，append / peek / pop 期间持锁，**网络 roundtrip 期间释放**）+ `entry.flushMu`（API call 期间持锁）。两把锁分离保证：

- `streamDraftEvent` 调 `append` 时**不**调 API，只持 `mu` 极短时间
- 即使 Telegram API 慢/卡住，新的 streamDraftEvent 调用仍能 append 进 buffer，agent runtime 不会卡住（2026-09-15 21:00 观察到的"主流程卡住"现象的修复）

**OutToolEnd 孤儿处理**：若 `toolsStack` 为空（无 open slot），OutToolEnd 被当成新 slot 的隐式 Start（body 进入 `slot.start`，后续 Ends 可 attach 到这个 slot）。

**诊断日志**（每个 silent path 都有 INFO log，方便诊断事件流）：

- `telegram: streamDraftEvent entered` — adapter 入口
- `telegram: streamDraftEvent fallthrough (no topic state)` — `state.topic()` 缺失
- `telegram: streamDraftEvent fallthrough (userMsgID<=0)` — replyAnchor=0
- `telegram: streamDraftEvent buffered` — 每个 event 入 buffer（`thinking_stack_len` / `tools_stack_len` / `message_id`）
- `telegram: group DraftMessage flushed` — 每次成功 flush（`method` / `sent_thinking` / `sent_tools` / `remaining_thinking` / `remaining_tools` / `reason`）
- `telegram: group DraftMessage flush failed; buffer restored` — 失败时 buffer 还原
- `telegram: group DraftMessage delete failed` — turn end deleteMessage 失败

**为什么用 rich_message 而不是 plain text**：

- **格式一致性**：跟 §11.11 rich turn placeholder 同款 envelope，process 提示和 answer 提示视觉统一
- **零 escape 负担**：rich_message 直接传 `json.RawMessage`，blocks 是结构化对象，无需 `parse_mode=HTML` 的 `<`,`>`,`&` 转义
- **future-proof**：未来想给 thinking 加 spoiler、给 tool result 加 `<pre>` fence，直接在 paragraph block 上加 entity / 替换 block type
- **user freedom**：simulated DraftMessage 是真实 Telegram message，user 在 bot thinking 期间可以照常打字 / interject，不会被 draft 锁住 send button

**视觉位置**：

```text
DM 私聊 (user 视角)
├─ User message: "帮我看看 foo.go"
├─ DraftMessage (rich): 最新 5 thinking + 最新 5 tool blocks   ← reply_to 挂 user message
├─ rich turn (OutHeartbeat PATCH): "🤖 Working... 💭 N · 🔧 M"
├─ OutReply: "answer text..."（独立 sendRichMessage）
└─ OutResult: "📝 final answer..."（独立 sendRichMessage）

turn end: DraftMessage 被 deleteMessage 移除
```

forum topic 视觉形态相同：用户消息 → DraftMessage (rich, message_thread_id=topicID) → rich turn → OutReply / OutResult。DM 与 group / topic 唯一差异：DM 下 `message_thread_id` 不带。

**为什么不持久化 DraftMessageID**：`groupDraft` 完全 in-memory，daemon 重启会丢失当前 turn 的 in-flight DraftMessage。trade-off：

- ✅ 简化数据流（无需 orphan recovery 路径）
- ✅ `state` 结构体少一个字段
- ⚠️ daemon 重启 mid-turn 会留孤儿在 chat；用户不会主动 resume 同 turn，影响有限
- ⚠️ 第 2 次 turn 自动 cold-create 新 message

**Bot API 兼容性**：纯 `sendRichMessage` / `editMessageText(rich_message=…)` / `deleteMessage`，需要 Bot API 10.1+（2025-06 引入的 `rich_message` 参数和 `sendRichMessage`）。sendRichMessage / editMessageText 失败统一保留 buffer 不退到 rich turn（issue #391 contract）。

**Rate-limit 友好度**（与 §11.11 整体设计一致）：每 turn 最多 `ceil(N/50)` 次 flush（N = 该 turn 的 events 总数，cap 50 后开始 evict 老的），每次 flush 最多 5+5=10 blocks。远低于 Telegram per-chat 1/s 和 per-group 20/min 硬限。

### 11.12.2 per-prompt isolation

每个 Out* event 的 turn anchor（`reply_to_message_id` + `groupDraftKey` 后缀 + `richTurns` key）**必须**取自 `msg.ReplyTo`，不取自 `state.UserMessageID`。`state.UserMessageID` 只在 `ensurePlaceholder` 创建占位那一刻读一次，后续 Send 路径再读它就违反 per-turn 隔离 — back-to-back turn 时旧 turn 滞留的 Out* 会写到新 turn 的 rich turn 上，产生"信息串位"。

**实现**：

- `adapter.Send()` 优先用 `msg.ReplyTo` 解析 `replyAnchor`；兜底 `state.UserMessageID`（兼容 shell / 框架 / 测试入口不 stamp `ReplyTo` 的场景，跟 Feishu orphan-fallback 行为对位）
- `adapter.patchChainHeader`（OutHeartbeat）同样优先 `msg.ReplyTo`，兜底 state
- per-turn 隔离靠两套并行的 per-`userMsgID` 索引：
  - **stream surface**：`groupDraftKey = chatID|topicID|userMsgID`（DM 与 group 共用，chat_kind 不参与 key）
  - **rich turn**：`richTurns` key 已含 userMessageID

  同 key 必然同 turn，不需要额外的 binding guard。

- `OnPromptEnded` 末尾调**两套** endProcess 作为 safety net，覆盖无 OutResult 的 turn（error / abort / bridge crash）：
  - `groupDraft.endProcess(ctx, rawChatID, topicID, parsedUserMsgID)`（stream surface）
  - `richTurns.purge(rawChatID, topicID, parsedUserMsgID)`

  `parsedUserMsgID > 0` 才调 stream surface 端，避免 startup / test orphan 路径打 warn；rich turn purge 自带 no-op 守卫。

跟 Feishu receipt 的语义对位：

| 维度 | Feishu receipt | Telegram v11.12 |
| --- | --- | --- |
| turn anchor 源 | `msg.ReplyTo` 唯一来源 | `msg.ReplyTo` 优先 + state 兜底 |
| 找不到 anchor | orphan path 走独立消息 | 兜底到 `state.UserMessageID`（同 Feishu orphan 行为） |
| per-userMsgID 隔离 | `receiptsByUserMsgID` map | `groupDraftKey` map + `richTurns` map |
| OnPromptEnded 清理 | `SetPromptState(terminal)` 走 receipt | 两套 endProcess safety net + rich turn 🎉 |
| 跨 turn 串位防护 | receipt 严格 per-userMsgID | 优先 ReplyTo + 所有索引 key 都含 userMsgID |
| `endProcess` 清理 in-memory state | n/a | `delete(m.entries, key)` |

### 11.12.3 已知限制

| Limit | 描述 | 缓解 |
| --- | --- | --- |
| simulated DraftMessage 是真实 message | turn 期间用户可见（不像 animated draft 那样只在 client 渲染）；如 sendRichMessage 失败 fall through 到 rich turn，会同时存在 DraftMessage + rich turn 两条 think/tool 痕迹 | cold-create 失败 → handled=false fall through（rich turn 第一段追加）；edit 失败 → 静默 log |
| `group_draft.go` DraftMessage 持久化 | daemon 重启 mid-turn 后 `state.DraftMessageID > 0`，下次 event 拿回同 message_id 继续 edit；turn end safety net 删之 | orphan recovery：下次 `ensurePlaceholder` 先 `deleteOrphanSync` 清上 turn 残留 |
| `state.ChatType` 老格式 state 文件 | 老 daemon 写入的 `chat_type` 字段在 `migrateChatNew` 迁移；load 时立即 save 持久化，零迁移延迟 | `LegacyChatType` 字段保留读路径；新写不再带 |

## 12. Telegram 交互输入：Type your answer + ForceReply

Telegram 的 InlineKeyboard 只能展示按钮，不能像飞书 Card 一样在按钮旁边直接渲染文本输入框。对于需要用户自由输入的答案，采用两步方案：

```text
第一步：用户点击 [Type your answer]
        │
        ▼
第二步：Bot 在同一个 Telegram Topic 中发送 ForceReply 消息
        请输入你的答案
        │
        ▼
用户回复 ForceReply 消息
        │
        ▼
qino 提交答案并继续当前交互
```

### 12.1 第一步：处理 Type your answer 按钮

按钮使用短小、可解析且不泄露敏感信息的 `callback_data`：

```json
{
  "text": "Type your answer",
  "callback_data": "i:<shortID>"
}
```

收到 callback 后，Telegram Adapter 应立即：

1. 校验 `callback_query.from.id` 是否为当前 Choice 操作人。
2. 校验 `callback_query.message.chat.id` 和 `message_thread_id`。
3. 通过 `shortID` 查 `ChoiceState`（完整 RequestID 走 state store 反查，`callback_data` 64 字节限制详见 §15 L3）。
4. 调用 `answerCallbackQuery` 结束按钮 loading。
5. 将 Choice 提示更新为"等待输入"状态，禁用 / 删除 `Type your answer` 按钮。
6. 在同一个 Topic 中发送 ForceReply 消息。
7. 保存输入提示消息的 `message_id`，进入 `waiting_input` 状态。

### 12.2 第二步：发送 ForceReply 消息

在当前 `message_thread_id` 中发送：

```json
{
  "chat_id": -1001234567890,
  "message_thread_id": 42,
  "text": "请输入你的答案……",
  "reply_mark": {
    "force_reply": true,
    "input_field_placeholder": "输入你的答案"
  }
}
```

ForceReply 消息发送成功后，保存返回的 `message_id`。Bot 收到用户回复后，必须验证：

```text
message.reply_to_message.message_id == ForceReplyMessageID
message.from.id == ChoiceState.UserID
message.chat.id == ChoiceState.ChatID
message.message_thread_id == ChoiceState.MessageThreadID
ChoiceState.State == waiting_input
```

只有通过这些校验，文本才作为当前问题的 `custom` 答案。

### 12.3 Action 协议映射

为了复用当前 `chatsession.Manager.SendPermission(chatID, option string)`，第一版不需要修改 Agent 或 Gateway 的权限协议：

- 单问题：`用户输入 → Action.Option = custom`
- 多问题：`EncodeQuestionPicks()` → `Action.Option = nm-q:<JSON>`（最后一步生成）

`ActionPayload.Form` 已存在于统一消息模型中，但当前权限分发路径主要消费 `Action.Option`。第一版让 Telegram Adapter 按 Feishu 的既有方式生成 `Action.Option`，避免为了 ForceReply 立即修改所有 Bridge 的 permission 协议。

### 12.4 ForceReply 期间处理普通消息

用户可能忽略 ForceReply 提示，在群里直接发一条普通消息：

- 必须检查回复目标，不能仅凭文本内容当作答案
- 不是回复 ForceReply 消息 → 当普通消息处理，不算 custom answer
- 长期未输入的提示由超时清理策略回收

## 13. 已知限制 / Gap

下面这些是 Telegram Bot API 的能力差距或实现优先级选择导致没做的点：

| 类别 | 说明 |
| --- | --- |
| **限制** | Telegram Bot API 本身不支持，靠 adapter 怎么写都做不到 |
| **降级** | 飞书有原生能力、Telegram 没有对应物，已用近似手段实现 |
| **未实现** | 设计上想做、但目前没实现（不在本期 scope） |

### 13.1 限制类（API 做不到）

#### L1. 没有"卡片"概念

Telegram 没有 receipt card 元素；`editMessageText` 是整体替换，不能 append 单条 log entry。

#### L2. `editMessageText` 整体替换，48 小时内有效

Telegram 没有 "append to existing message" 语义。所有"原位更新"都是替换全部文本。文本消息编辑受 48 小时限制（`editMessageReplyMarkup` / `editMessageMedia` 无限制）。

#### L3. callback_data 64 字节限制

`shortID(req[:8] + "-" + req[len-8:])` 应对，完整 RequestID 走 state store 反查。

#### L4. ForceReply 仅对下一条用户消息生效

用户发了别的消息后，force_reply 自动失效。多多多问题向导场景下 ForceReply prompt 会沉默失效。

#### L5. 没有 `reply_in_thread` 等价物

Telegram 的 `reply_to_message_id` 只在视觉上"引用"，消息本身仍然显示在 Topic 主消息流。设计上靠 Topic 自身隔离来替代。

#### L6. 没有 markdown 原生支持，只能用受限 HTML 子集（已退化为 rich blocks）

Telegram 只支持 `<b>` `<i>` `<u>` `<s>` `<strike>` `<del>` `<code>` `<pre>` `<a href>` `<tg-spoiler>`。所有 text 出口走 `rich_message[blocks]`（Bot API 10.1+）渲染为原生 rich block（heading / pre / list / blockquote / table / footer 等），不再是 markdown→HTML 的近似。颜色 / 字号仍不支持。

#### L7. 没有"原生 task list" / checklist 元素

Telegram 只能用文本 `[x]` / `[ ]` / `[~]` 模拟。当前 rich turn 用 `list` block 渲染 task snapshot，点击切换需走 callback。

#### L8. 没有 "Mini App form" 原生输入控件（除 ForceReply）

复杂多字段输入走两轮：先 option 选择字段类型，再 ForceReply 输入内容。

#### L9. `editForumTopic` 只能改名称/图标，不能改"正文"

Forum Topic 本身没有消息正文，永远靠内部的 rich message 表达"会话状态"。

#### L10. 入站 update 严格收敛到 `message` + `callback_query`

`allowed_updates` 显式列出 `["message", "callback_query"]`；其余所有 update 类型（`message_reaction` / `message_reaction_count` / `chat_member` / `my_chat_member` / `edited_message` / `channel_post` 等）服务端不下发。reaction 仅作为出站通道（`OutMessageState` → `setMessageReaction`）用于在 user 消息 / 占位消息上贴视觉状态。

### 13.2 降级类（用近似手段实现）

#### D1. rich message 是单条 Telegram message 的多 block 数组

不像飞书 receipt 能 append div 累积。Telegram 走 `editMessageText(rich_message=…)` 整体替换 blocks。多个 activity 累积在同一条 rich message 的 blocks 数组里，客户端渲染为多个 paragraph / list / divider。

#### D2. OutError 渲染为 rich block 内的 fenced-code pre block

Telegram 没有红色 card。`❌ <text>` 标题 + fenced-code block 拼成 `pre` block。

#### D3. OutCommandReply 走 rich turn path

slash 命令输出经 rich turn path 渲染，walker 处理 markdown 转 rich blocks，不需要手动走 HTML escape。

#### D4. `addReaction` / `deleteReaction` 都用 `setMessageReaction`

Telegram 没有"删除单个 reaction"的 API，"删除"通过 `reaction: []` 实现。`OutMessageStateRemoved` 携带 `MessageID`，调 `setMessageReaction(reaction: [])` 全清。

### 13.3 未实现类

#### N1. PinChatMessage 钉住最终结果

长 Topic 内回复滚动后，用户很难找到 OutResult 的最终答案。`pinChatMessage` 可把 OutResult 消息钉在 Topic 顶部；当前实现没做（依赖 §14.1 P1）。

#### N2. `pendingHeartbeats` 缓冲

跟 feishu F-63.1 同款。当前 `ensurePlaceholder` 在 `handleMessage` 同步路径里 eager 创建 rich turn + `updateRichTurnHeader` 已就位，第一次 OutHeartbeat 直接 PATCH header，不需要 buffer。

#### N3. OnPromptEnded 用 reaction 表达错误终态

已用 `setMessageReaction(targetID, ❌)` 承担错误终态（reason.IsError() 时换 emoji）。user message slot 不动（v6.3 单 reaction 预算守）。

#### N4. Orphan reply fallback

sendMessage 失败 → retry 3 次 → 仍失败就返回 error，runtime 看到 error。如果用户配置 bot 权限问题（terminal 错误）确实无解。

#### N5. 用 `msg.ReplyTo` 作为锚点（编辑消息）

已实现 — `adapter.Send()` 优先用 `msg.ReplyTo` 解析 `replyAnchor`；兜底 `state.UserMessageID`。

#### N6. 心跳 header 加 session identity

`heartbeatText(snapshot)` 已经包含 `LastBeatAt` 时间戳 + `ThinkCount` / `ToolCount`。SessionID / Model / AgentName 通过 StatusBar footer 块呈现。

#### N7. DM 私聊 Topic 模式（不实现）

- **结论**：nightme 不实现 Bot API 10.3 起的 DM 私聊 Topic 模式
- **API 现状**：Bot API 10.3 起 `getMe.has_topics_enabled == true` 的 bot 可在私聊中调用 `createForumTopic` / `editForumTopic` / `deleteForumTopic` / `unpinAllForumTopicMessages`，`Message.message_thread_id` / `Message.is_topic_message` 也已扩到 private chat。**`closeForumTopic` / `reopenForumTopic` 仍仅支持 forum supergroup chat**，DM 调用 server 拒
- **部分缓解**：`OutThinking` / `OutToolStart` / `OutToolEnd` 三类事件在 DM 下走 §11.12.1 unified simulated DraftMessage，无须 forum topic 容器就能给用户"思考中"的视觉反馈
- **不支持理由**：
  1. **eligibility 不可控**：`Bot Platform Developer Terms of Service` §6.2.6 限定 "one or more eligible TPAs they own" 可启用该能力，Telegram 未公开 eligibility 判定细则
  2. **API 缺口**：DM 不支持 `closeForumTopic` / `reopenForumTopic`，Topic 复用、归档、限流清理都得改走 `deleteForumTopic`（一次性删 topic + 全部消息）
  3. **合规绑定**：启用后该 TPA 内 Stars 购买按 15% 非退款抽成（§6.2.6）
  4. **现有路径够用**：DM §11.12.1 simulated DraftMessage 给用户清晰的"思考中"视觉，rich turn 单条 rich message 在 DM 下承担 turn 状态机，topic 容器增益边际低
- **重审触发条件**：Telegram 把 DM topic mode 开放给所有 bot / Bot API 新增 DM `closeForumTopic` / `reopenForumTopic` / nightme 业务侧有"DM 内多任务并行"硬需求

## 14. Telegram 独有、未利用的能力

下面这些 API 飞书**没有**对应物，Telegram 原生支持但当前 adapter 没有用。每条标出"对应飞书体验"和"启用后能补齐哪个 gap"。

### 14.1 P1 - `pinChatMessage` 钉住最终结果

**能力**：`pinChatMessage(chat_id, message_id)` 把任意消息钉在 Topic（或群组）顶部。

**对应飞书体验**：飞书没有 pin 概念。

**补齐的 gap**：

- 用户痛点：长 Topic 内回复滚动后，用户很难找到 OutResult 的最终答案
- 启用后：每个 turn 结束后自动把 OutResult 消息 pin 在 Topic 顶部；新一轮开始时 unpin 旧 result

**当前为什么没做**：

- §13.3 N1（rolling-log receipt）的替代方案：如果不做 receipt，pin 是次优选择——视觉上不那么"集成"，但用户能找到结果
- §13.3 N3 的延伸：可以在 ❌/🎉 reaction 之外再叠加 pin，提升发现性

**工作量估算**：~15 行（OutResult 后调 pin；新一轮 unpin 旧 result）

**风险 / 边界**：

- pinChatMessage 调用也有速率限制（每个 chat 5 次/min）
- 多 Topic 同 chat 时，pin 在 chat 级别可见，跨 Topic 共享 pin 位

### 14.2 P2 - `deleteMessage` 删除占位消息

**能力**：`deleteMessage(chat_id, message_id)` 删除任何消息（48 小时内）。

**对应飞书体验**：飞书 receipt 不能"删除"（会丢上下文）。

**补齐的 gap**：

- §13.3 N3 的"Topic 流更干净"版本：OnPromptEnded 时删 rich message 占位，配合 🎉 reaction 作为完成标识
- 不再做 rich message 原位 PATCH

**当前为什么没做**：rich turn 留作 turn 历史证据，删除会让用户失去时间线锚点。

### 14.3 P3 - OnPromptEnded 用 🎉 reaction + delete placeholder 组合

组合 P2 + `setMessageReaction("🎉")`。当前实现已经用 reaction（P3 部分完成），删除占位未做。

### 14.4 P4 - `editMessageReplyMarkup` 只更新键盘

**能力**：`editMessageReplyMarkup(chat_id, msg_id, keyboard)` 单独更新消息的 keyboard，**不**改 text。

**对应飞书体验**：飞书 PATCH card 必须整体替换 schema。

**补齐的 gap**：

- 当前实现每次点 button 都 `editMessageText(text + keyboard)`，触发 markdown 重新渲染
- Choice settle 时只更新 keyboard（移除其他按钮）应走 `editMessageReplyMarkup`，不动 text

**工作量估算**：~20 行（patchChoice settle 路径改用 editMessageReplyMarkup）

### 14.5 P5 - `sendMediaGroup` 批量附件

**能力**：`sendMediaGroup(chat_id, media[])` 一次发送最多 10 个 media 作为一条消息的相册。

**对应飞书体验**：飞书 `upload_file` 可以批量（im.message.batch_send），但实现细节不同。

**补齐的 gap**：

- 当前附件下载 + 上传：每个图片/视频单独 `sendPhoto` / `sendVideo`
- 多张图变成多条消息

**当前为什么没做**：当前 attachments 按 1 个 media 1 条消息处理。sendMediaGroup 不支持 caption（caption 必须是 media[0]），混合类型 OK，但 document 不能混。

### 14.6 P6 - `unpinAllChatMessages` 清理历史 pin

**能力**：`unpinAllChatMessages(chat_id)` 清空 chat 内所有 pin。

**当前为什么没做**：产品需求不明确。

### 14.7 P7 - `sendChatAction` 显示 typing 状态

**能力**：`sendChatAction(chat_id, action="typing")` 在 chat 内显示"bot 正在输入..."指示。

**当前为什么没做**：typing 默认 5 秒过期，需持续刷新（每 4-5 秒重发）。typing 不能跨 Topic（chat 级别），群组启用 typing 可能让用户误解 bot 在主窗口回复。

### 14.8 P8 - `sendPoll` 作为 Choice 的另一种渲染

**能力**：`sendPoll(chat_id, question, options[])` 发一个 poll。

**补齐的 gap**：

- 当前 `InlineKeyboardMarkup` 实现 Choice，体验够用
- sendPoll 选项数不限，但不支持 emoji icon、不支持 URL

**当前为什么没做**：不是必须。

### 14.9 优先级建议

按 ROI 排序：

| 优先级 | 项 | 工作量 | 价值 | 理由 |
| --- | --- | --- | --- | --- |
| 1 | **P1** pin OutResult | ~15 行 | 高 | UX 提升大（长 Topic 内找答案） |
| 2 | **P4** editMessageReplyMarkup 单独更新 | ~20 行 | 低 | 性能优化 |
| 3 | **P5** sendMediaGroup | ~30 行 | 低 | 少见场景 |
| 4 | **P7** typing indicator | ~25 行 | 中 | 长 turn 反馈 |
| 5 | **P2** deleteMessage（仅作为 P3 子步骤） | ~10 行 | 中 | 已在 P3 中 |
| 6 | **P6** unpinAllChatMessages | ~5 行 | 极低 | 测试维护 |
| 7 | **P8** sendPoll 替代 Choice | ~80 行 | 中 | 可选替代方案，争议大 |

## 15. 网络代理

Telegram Bot API 在某些网络环境（如中国大陆）下不可达。nightme 继承标准代理环境变量，无需任何配置。

### 15.1 支持的环境变量

| 变量 | 作用 |
| --- | --- |
| `HTTP_PROXY` | HTTP 请求的代理 URL（如 `http://127.0.0.1:7890`） |
| `HTTPS_PROXY` | HTTPS 请求的代理（对 `api.telegram.org` 生效） |
| `NO_PROXY` | 不走代理的域名/网段（逗号分隔） |
| `ALL_PROXY` | 兜底代理，HTTP_PROXY/HTTPS_PROXY 未设置时生效 |

Go 标准库的 `http.ProxyFromEnvironment` 自动读取这些变量。

### 15.2 使用示例

**Clash / Surge（mixed-port 模式，HTTP 代理）**：

```bash
export HTTPS_PROXY=http://127.0.0.1:7890
nightme start --channel=telegram
```

**v2ray / shadowsocks（SOCKS5 代理）**：SOCKS5 代理无法通过环境变量直接配置，需要在系统层做透明代理转发。或用 `proxychains` / `tsocks` 之类包装 nightme：

```bash
proxychains4 nightme start --channel=telegram
```

**不走某些域名**：

```bash
export NO_PROXY=localhost,127.0.0.1,*.internal
nightme start --channel=telegram
```

### 15.3 内部实现

所有 outbound HTTP 请求统一走 `internal/httpclient` 包：

```go
import "github.com/cnlangzi/nightme/internal/httpclient"

client := httpclient.Default()           // 45s timeout, proxy from env
client := httpclient.DefaultWithTimeout(10*time.Second)
```

该包封装的原则：

- 唯一职责：把 `&http.Client{Timeout: ...}` 集中到一个地方，避免散落重复
- 默认行为：复用 `http.DefaultTransport`（已经指向 `http.ProxyFromEnvironment`）
- 不做 retry / rate limit / logging——这些由调用层组合（参考 `internal/channel/telegram/retry.go` 和 `ratelimit.go`）

被改造的位置：

- `internal/updater` —— 检查 GitHub release
- `internal/version` —— 检查 nightme.dev 版本
- `internal/bridge/dsh/host` —— dsh 主机 RPC
- `internal/channel/telegram` —— Telegram Bot API
- `internal/login/telegram` —— login 时的 `getMe` 校验

### 15.4 不暴露代理配置的原因

不提供 `cfg.Telegram.ProxyURL` 之类的配置项：

- 代理需求来自环境（用户机器的网络），不是配置决策
- 配置项会被各种 secret 管理工具、CI/CD 流水线暴露在 diff 里
- 环境变量是 OS-level 的标准机制，工具链（Docker、Kubernetes、systemd）都支持

## 16. StatusBar footer 渲染

Telegram 没有"card"结构（见 §13 L1），不能 append log entries 到同一条消息。每条 rich message 末尾独立的 footer block 承担 StatusBar 元数据展示。

### 16.1 字段与渲染

StatusBar 三行由 `internal/statusbar.StatusBarLines(&msg)` 生成，从 `OutboundMessage` flat 字段读取：

```text
Line 1: 🤖: AgentName · Model · SessionID       (Identity)
Line 2: 💰:「 new / cache / out · X% (window) · $cost 」   (Usage)
Line 3: 📁: ws · ⎇ branch · + N · − N · ± N · ? N · ! N · ⇡ N · [#PR](url)   (GitStatus)
       非 git workspace：📁: ws
```

每行 zero-omit（F-45 §1.6）。整行字段全空 →该行不渲染。Line 3 是唯一例外：Workspace 已设但 git 没产出 snapshot 时仍渲染 `📁: <ws>`。StatusBar 完全为空 → 不发 footer block。

Telegram adapter 把 StatusBar 渲染为 `rich_message[blocks]` 的 footer block（Bot API 10.1+ `{"type":"footer","text":RichText}`）：

```text
[rich message body blocks]
───────────
[footer block]   ← StatusBar 三行作为 muted caption region
```

footer block 的 `text` 由 `footerLinesToRichText(footerLines)`（`internal/channel/telegram/result_blocks.go`）生成：

- 全 entity-free 时：返回 `"\n"-joined string`（common case）
- 任意行带 inline entity（PR 锚点 `[#N](url)` → `{"type":"url",...}`）：返回 `[]any` 混合 plain `\n` 分隔符和 inline entity map

PR 锚点保留为 clickable url entity，不会退化为字面 markdown 文本。

完整契约 / 测试在 `internal/statusbar/statusbar_test.go`（从 feishu F-45 §1.6 的 `usage_footer_test.go` 迁移）。

### 16.2 贴附规则

| Kind | 是否带 footer |
| --- | --- |
| `OutReply` / `OutCommandReply` / `OutThinking` / `OutToolStart` / `OutToolEnd` / `OutError` / `OutTaskCreate` / `OutTaskUpdate` | footer-bearing kind：streaming 事件 `msg.Usage == nil`，所以 `statusbar.StatusBarLines(&msg)` 只产 Identity / Git 行；rich turn PATCH 时刷 `turn.footer`（缺 Usage 行） |
| `OutResult` | 双重贴附：(1) 独立 rich message footer 走 `buildResultBlocks` 的 `footerLinesToRichText(footerLines)`；(2) 同一份 `footerLines` 回写到占位卡的 `turn.footer`，让 💰 行追上 turn 内最后状态 |
| `OutHeartbeat` | 不带（headerLine 是 heartbeat 文本本身） |
| `OutChoice` / `OutChoicePatch` | 不挂（InlineKeyboard 自含，挂 footer 污染选择 UI） |
| `OutMessageState` / `OutMessageStateRemoved` | 不挂（reactions 独立轨道，§14.1） |
| `OutInit` | 不挂（silent drop） |

### 16.3 data-driven footer 刷新

`statusbar.StatusBarLines(&msg)` 是 `internal/statusbar` 的纯 renderer，**不持有任何 state**：

- `Identity`（AgentName / Model / SessionID）：由 `MessageStateBus` subscriber 在 dispatch 路径 stamp 到 OutboundMessage（F-44 / fix-placehold-card）
- `Usage`：由 bridge 在终态 OutResult 上填（Claude Code `result.usage + result.modelUsage`，Pi `message_end.usage`）；streaming 中间 chunk 该字段为 nil → `StatusBarLines` zero-omit Line 2
- `GitStatus`：由 chatsession 在 `SetSelectedCwd` / `/gtw commit` / `/gtw pr` 时刷，runtime 透传

Kind 不锁 policy：runtime 在哪个 kind 上 stamp status 字段由 runtime 决定，rich turn 照单全收；`statusbar.StatusBarLines(&msg) == nil` 时 footer 不动，`!= nil` 时刷 `turn.footer`。OutResult 是唯一同时刷新占位卡 + 独立消息 footer 的 kind —— 见 §16.4。

### 16.4 rich turn footer 生命周期

每 turn 最多一个 footer block：

- turn 开始时 footer = nil（footer block 不出现）
- streaming footer-bearing event 来 → `turn.footer` 刷新（Identity / Git 两行，Usage 行因 `msg.Usage == nil` 缺席），dirty=true，scheduled flush
- OutResult 到达时，footer 同时承担两件事：(1) 组装到独立 rich message 的 footer block；(2) 把同一份 `statusbar.StatusBarLines(&msg)` 回写到占位卡的 `turn.footer`，让 💰 行追上 turn 内最后状态。下次 debounce（250ms）或 `OnPromptEndedRichTurn` 的同步 flush 把新 footer PATCH 到占位卡
- flush 时 `renderRichTurnBlocksLocked` 检查 `len(turn.footer) > 0`，非零时在 entries 之后追加 divider + footer block
- turn 结束 → turn purge，footer 跟 turn 一起清零（下次 turn 干净启动）

### 16.5 已知限制

- Telegram footer block 是 Bot API 10.1+ 特性，旧客户端可能不渲染 footer block（rich block 整体的 32K cap 仍生效）
- PR 锚点作为 url entity 保留 clickable 行为，但客户端 footer region 的 link 视觉是 muted caption 风格
- StatusBar 走纯文本 + 中点 `·` + 半角空格，不用 `<b>` `<code>` 强调，视觉不如 feishu grey footer，但 parse 零失败
- OutResult 的 Usage 仅在终态一次性回写到占位卡 footer；streaming 阶段（OutReply / OutToolStart / OutToolEnd / …）的 PATCH 不带 Usage 行 —— 用户看到的"💰 出现"是 OutResult 到达后的最后一跳，不是实时 ticker

## 17. Rich Messages 路径

Telegram Bot API 10.1（2026-06-11）起 `sendMessage` / `editMessageText` 之外另设 `rich_message[blocks]` 路径：

- `sendRichMessage(chat_id, rich_message={"blocks":[…]})` —— 单 block 32K+ chars、500 blocks / message
- `editMessageText(chat_id, message_id, rich_message={"blocks":[…]})` —— 原位编辑富文本（body schema 同 `sendRichMessage`）

### 17.1 上限对比

| 维度 | `sendMessage` text 路径（弃用） | `sendRichMessage` rich_message 路径（现状） |
| --- | --- | --- |
| 单 message 字符 | **4096** 严格 | 单 block 32K+ chars（实测 40K 通过） |
| 单 message blocks | n/a（单 string） | **500** 严格（501 → `RICH_MESSAGE_BLOCKS_TOO_MANY`） |
| 顶层字段名 | `text`（string） | `rich_message`（JSON object，`blocks` 数组） |
| 原位编辑 | `editMessageText(text)`（4096 cap） | `editMessageText(rich_message=…)`（32K+ cap） |
| `parse_mode` | `HTML` / `MarkdownV2` / `Markdown` | 不使用；结构通过 JSON 表达 |

### 17.2 Block 类型

Bot API 10.1 定义 25 种 input block type：paragraph、heading、pre、list、blockquote、expandable_blockquote、pullquote、divider、details、table、photo、video、audio、voice_note、animation、document、collage、slideshow、map、mathematical_expression、thinking、buttons、footer、anchor。

**JSON 形状约定**：所有字段平铺在 block 自身上，**不嵌套**在同名 wrapper key 下。例：

```json
// ✓ 正确
{"blocks":[{"type":"heading","text":"Title","size":1}]}

// ✗ 错误（早期 round 5/6/7 一直用这种，全失败）
{"blocks":[{"type":"heading","heading":{"text":"Title","size":1}}]}
```

`paragraph` 是唯一对嵌套形式宽容的 type。其他 24 种严格按平铺形式解析。

| Type | 状态 | 字段 / 约束 |
| --- | --- | --- |
| `paragraph` | ✓ | `text`（40K chars 单 block 通过） |
| `heading` | ✓ | `text` + `size:1-6` |
| `pre` | ✓ | `text` + `language`（可选） |
| `divider` | ✓ **inline only** | 无字段；不能作为 blocks 数组唯一元素，必须跟 paragraph / blockquote 等并列或嵌套 |
| `list` | ✓ | `items:[{blocks,1}, ...]`（bot API 10.1 `list` 无 ordered enum，client 从 item `1.` prefix 推断） |
| `blockquote` | ✓ | `blocks:[InputRichBlock, ...]` + `credit`（可选） |
| `expandable_blockquote` | ✓ | `text` + `credit`（可选） |
| `pullquote` | ✓ | `text` + `credit`（可选） |
| `details` | ✓ | `summary` + `blocks` + `is_open`（可选） |
| `table` | ✓ | `cells:[[RichBlockTableCell, ...], ...]`（2D 数组，cell 是 `{text, is_header, colspan, rowspan, align, valign}` 对象） |
| `map` | ✓ | `location:{latitude, longitude, ...}` + `zoom` / `width` / `height` / `caption` |
| `mathematical_expression` | ✓ | `expression`（LaTeX 字符串） |
| `footer` | ✓ | `text` |
| `anchor` | ✗ **inline only** | `name` 字段存在但 standalone 报 `RICH_MESSAGE_EMPTY`，需要作为其他 block 的 child |
| `thinking` | ✗ | `RICH_MESSAGE_BLOCK_UNSUPPORTED` |
| `buttons` | ✓ | `buttons:[RichMessageButton, ...]`（1-8 个）；`RichMessageButton` 含 `text` / `url` / `callback_data` / `style / web_app` |
| `collage` / `slideshow` | △ media-only children | server 拒 paragraph child（`BLOCK_UNEXPECTED`） |
| `photo` | ✓ | `photo:{type:"photo", media:"<url-or-file_id>"}`（**仅 `https://telegram.org/` 域名实测 fetch 通过**） |
| `video` / `audio` / `voice_note` / `animation` / `document` | 未测 | 推断结构同 photo（`InputMedia*` 嵌套） |

### 17.3 渲染原语

所有 Telegram 出站走 `sendRichMessage(rich_message={"blocks":[…]})`：

- rich turn body：`markdownToRichBlocks`（`rich_walker.go`）翻 blocks + `renderRichTurnBlocksLocked`（`rich_turn.go`）组装 header + entries + taskList + footer
- OutResult standalone：`buildResultBlocks`（`result_blocks.go`）= markdownToRichBlocks body + divider + footer block
- Choice / Permission / ForceReply：`sendRichFromHTML` / `editRichFromHTML`（`rich.go`）走 `htmlChoiceBodyToBlocks` 把 renderChoice 的 `<b>Title</b>\n\nBody` 转 heading + paragraph
- Footer block：`footerLinesToRichText`（`result_blocks.go`）把 StatusBar 三行翻成 RichText

### 17.4 walker 行为（`rich_walker.go`）

| Markdown 构造 | walker 处理 | block 形态 |
| --- | --- | --- |
| `# / ## / ###` heading | `walkHeading` | `{"type":"heading","text":<inline>,"size":<N}` |
| ` ```lang ` fence / ` ``` ` no-lang | `walkFence` | `{"type":"pre","text":<joined>,"language":<lang>?}` |
| `-` / `*` / `+` bullet list | `walkList` | `{"type":"list","items":[{blocks:[paragraph]}]}` |
| `1.` / `2.` ordered list | `walkList`（同函数） | 同 bullet |
| `>` blockquote | `walkBlockquote` | `{"type":"blockquote","blocks":[paragraph]}` |
| `---` / `***` / `___` thematic break | inline in main loop | `{"type":"divider"}` |
| GFM table | `walkTable` | `{"type":"table","cells":[[{text,is_header?,align?}],...]}` |
| 段落（兜底） | `walkParagraph` | `{"type":"paragraph","text":<inline>}` |
| 空字符串 / 仅 whitespace / >32K chars | — | `ok=false`（caller fallback paragraph） |
| 未闭合 fence / table 缺 separator / 列数 mismatch | — | `ok=false`（caller fallback paragraph） |

Inline 节点映射（`inlineToRichText`）：

| Markdown inline | 处理 |
| --- | --- |
| `` `code` `` | `{"type":"code","text":...}`（priority 在 `*` / `_` 之前） |
| `**bold**` / `__bold__` | `{"type":"bold","text":...}` |
| `*italic*` / `_italic_` | `{"type":"italic","text":...}` |
| `[text](https?://\|tg://url)` | `{"type":"url","text":...,"url":...}`（scheme 白名单） |
| `![alt](https?://...)` | 降级到 `{"type":"url","text":alt,"url":...}`（rich blocks 无 inline image entity） |
| `[^id]` GFM footnote ref | stripped |
| `<tag>...</tag>` raw HTML | 保留为 literal 文本 |
| `~~strike~~` strikethrough | 渲染为字面 `~~strike~~`（rich block 无 strike entity） |

### 17.5 失败 / 降级语义

walker 拒收形状（block-cap / char-cap / malformed shape）→ `ok=false` → caller fallback 到 paragraph block（`{"type":"paragraph","text":<raw_body_with_inline_entities>}`）。**没有** plain text fallback：失败仍走 rich_message[blocks]，只是单 paragraph block。失败的核心契约是 "rich path failure surfaces to runtime, no silent truncation"。

### 17.6 风险评估

| 风险 | 状态 | 缓解 |
| --- | --- | --- |
| Pre-10.1 客户端显示空白 | 接受 | RichMode 常驻，无 plain-text fallback。Pre-10.1 客户端收到 `rich_message` 可能渲染为空白或报错。客户端版本探测 client_version API 不存在，待 Bot API 落地 |
| Markdown auto-parse cliff（>500 blocks） | 实测 200/400 ✓ 600+ ✗ | L2 走显式 blocks 绕开 |
| Photo URL 白名单（仅 telegram.org 实测通过） | 实测 | walker 不主动 emit photo block；显式发图仍走 `sendPhoto` |
| Thinking block Premium-only | 实测 `BLOCK_UNSUPPORTED` | walker 把 markdown emphasis 走 italic 不用 thinking |
| Anchor / divider 不能 standalone | 实测 | walker 规则：divider 必须有前后 sibling；anchor 仅作 list/blockquote 子块 |
| Collage / slideshow 仅 media child | 实测 `BLOCK_UNEXPECTED` | walker 跳过 collage/slideshow |
| 富文本字段名未来变更 | 低 | 字段名集中在 `rich.go` 一处 |
| Concurrent edit race（两个 OutHeartbeat 同时 PATCH） | 通过 250ms debounce 合并 | 同一 turn 内的 PATCH 串行化在 turn.mu 锁下 |

