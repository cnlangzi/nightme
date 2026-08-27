# Telegram Channel - Topic 方案与接入设计

> **Status**: implemented (v8 per-turn 占位) + v9 chain rolling log 即将落地（见 §11.12）。已知 gap 跟踪见 §15。
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
| 飞书占位卡 | Topic 中 qino 自己发送的第一条状态消息 | `placeholder_message_id` |
| 飞书 receipt 原位更新 | 对状态消息执行 `editMessageText` | `chat_id + placeholder_message_id` |
| thinking | Topic 内独立消息 | 消息自身的 `message_id` |
| tool start / tool end | Topic 内按时间顺序的独立消息 | 消息自身的 `message_id` |
| 交互 Choice | Topic 内的 `InlineKeyboardMarkup` 消息 | 消息自身的 `message_id` |
| 飞书 Choice 点击 | callback query，按 `message_id` 找回原 Choice | callback query 的 `message_id` |
| reaction | 对 Topic 内消息调用 `setMessageReaction` | `chat_id + message_id` |

核心原则：

1. **主窗口只作为入口**，不在主窗口发送 qino 的 thinking、工具、进度和 receipt。
2. **一个 qino 会话对应一个 Telegram Topic**，Topic 内可以积累任意数量的消息。
3. **Topic 本身不是占位卡**。Topic 是一条消息容器，不能用 `editForumTopic` 更新占位正文。
4. **Topic service message 只是导航入口**；真正的占位状态必须是 Topic 中另行发送的普通消息。
5. Telegram Topic 消息通过共同的 `message_thread_id` 分组，不存在可依赖的 `root_id -> children` 线程树。
6. 需要在 Topic 内保持可更新的状态时，必须持久化 `message_id`，后续通过 `editMessageText` / `editMessageReplyMarkup` 原地更新。

## 2. Topic 与占位卡的生命周期

### 2.1 创建 Topic

用户在主窗口发起消息后，适配器先为对应会话创建或查找 Topic：

```text
主窗口用户消息
      │
      ▼
查找 chat_id 对应的 message_thread_id
      │
      ├── 已存在 ──► 继续使用原 Topic
      │
      └── 不存在 ──► createForumTopic
                         │
                         ▼
                    保存返回的 message_thread_id
```

概念请求如下：

```json
{
  "chat_id": -1001234567890,
  "name": "qino · user · coding"
}
```

适配器持久化的最小 Topic 标识为：

```text
TelegramChatID
TelegramMessageThreadID
UserMessageID
PlaceholderMessageID（可选，创建状态消息后填写）
```

### 2.2 创建占位消息

Topic 创建成功后，qino 在 Topic 中发送第一条占位消息：

```text
Topic
├─ Topic service message
└─ 🤖 Working...
   └─ placeholder_message_id = 1001
```

概念请求如下：

```json
{
  "chat_id": -1001234567890,
  "message_thread_id": 42,
  "text": "🤖 Working..."
}
```

该普通消息的返回值必须保存为 `placeholder_message_id`。它不是 Topic 的 service message，也不依赖 `createForumTopic` 的返回值。

### 2.3 原位更新占位消息

后续心跳、工具状态或阶段状态使用：

```text
editMessageText(
    chat_id = TelegramChatID,
    message_id = PlaceholderMessageID,
    text = "💭 Thinking... · 🔧 1"
)
```

如果需要按钮，使用 `editMessageReplyMarkup` 或编辑包含 InlineKeyboard 的消息。不得把占位消息删除后重新发送，除非它已经无法编辑，例如消息被删除、超过 Telegram 编辑限制或属于已经关闭且无法继续发送的 Topic。

### 2.4 Topic 内追加完整过程

thinking 和工具调用可以作为 Topic 内的独立消息发送：

```text
Topic
├─ 🤖 Working...                         placeholder
├─ 💭 Thinking                            OutThinking
├─ 🔧 Read                                OutToolStart
├─ ✅ Read done                           OutToolEnd
├─ 🔧 Bash                                OutToolStart
├─ ✅ Bash done                           OutToolEnd
├─ 📝 Result                              OutResult
└─ ✅ Completed                            终态状态
```

这种消息累积是 Telegram 原生允许的：所有消息携带同一个 `message_thread_id`，在客户端中显示为同一 Topic 的时间线。

## 3. 事件映射

| nightme `OutboundMessage` | Telegram 实现 | Topic 位置 | 是否建议默认发送 |
| --- | --- | --- | --- |
| `OutThinking` | `sendMessage`，可选 HTML/MarkdownV2 | Topic | 是，作为 thinking 详情 |
| `OutToolStart` | `sendMessage` | Topic | 是，作为工具调用详情 |
| `OutToolEnd` | `sendMessage` | Topic | 是，作为工具结果详情 |
| `OutHeartbeat` | 优先 `editMessageText` 占位消息 | Topic 占位消息 | 是，保持状态紧凑 |
| `OutReply` | `sendMessage` 或编辑占位消息 | Topic | 视长度和阶段而定 |
| `OutResult` | `sendMessage`；超长时拆分 | Topic | 是 |
| `OutInit` | 更新占位消息的 header/footer，或发送初始化消息 | Topic | 是 |
| `OutChoice` | `sendMessage` + `InlineKeyboardMarkup` | Topic | 需要交互时发送 |
| `OutChoicePatch` | 通过 `Choice.RequestID` 找到原 Choice，再 `editMessageText` / `editMessageReplyMarkup` | 原 Topic 消息 | 是 |
| `OutError` | 新建错误消息或更新占位消息 | Topic | 是 |
| `OutTaskCreate` / `OutTaskUpdate` | 更新 Task 消息或占位消息 | Topic | 建议使用可更新消息 |
| `OutMessageState` | `setMessageReaction` | 对应入站用户消息 | 是 |
| `OutCommandReply` | 发送普通消息 + 末尾拼 StatusBar | Topic | 是 |

**所有 text 出口**(`OutReply` / `OutThinking` / `OutToolStart` / `OutToolEnd` / `OutResult` / `OutTaskCreate` / `OutTaskUpdate` / `OutError` / `OutCommandReply`)和 `OutHeartbeat` 占位 PATCH,body 末尾都拼 StatusBar 三行 footer(🤖 Identity / 💰 Usage / 📁 Git),用 `────────` 分隔。详见 §18。`OutChoice` / `OutMessageState*` / `OutInit` 不挂(分别走 InlineKeyboard card / reactions / silent drop)。

第一版建议采用“**占位状态 + 事件详情**”双层策略：

- 占位消息只展示当前状态、心跳、进度和最终状态，避免 Topic 需要实时重建 UI。
- thinking/tool 事件允许作为独立消息追加，用户进入 Topic 后可按顺序查看完整时间线。
- 工具失败、等待授权、用户选择和长耗时等高优先级事件可以额外发送独立消息。

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
4. Bot 使用长轮询（`getUpdates`）接收 `Message`、`CallbackQuery` 和 `MessageReactionUpdated` 等更新。每个 Bot 只能有一个 `getUpdates` consumer,daemon 重启时用持久化的 `update_id + 1` 继续消费。
5. 私聊没有 Forum Topic；私聊只能退化为普通消息，并在文档和 UI 中明确标注。
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
- greeting 内容：`Hi, this is NightMe 👋. Your pair programmer.` /
  `Set it running. Stay in the loop from your phone, on your terms 🚀.`
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
  "text": "🔧 Reading file.go..."
}
```

Topic 内的占位消息"Working..." 仍然用 `editMessageText(chat_id, placeholder_message_id, text)` 原地更新。**DM / 主窗口（thread_id=0）走 v3**：每条用户消息进来新建一条 `<b>🤖 Working...</b>` 占位（注意 v9 实际不再带 v7 的 `· ⏱ HH:MM:SS` 时间戳后缀 —— 那是 `placeholderInitialText(now)` helper 的老行为,v9 直接 `heartbeatText(nil)`,时间戳由首次 OutHeartbeat 通过 `LastBeatAt` 段补上),所有 OutXxx reply_to_message_id 锚到**用户原消息**（不是占位），turn 终态 PATCH 占位为 `✅ Completed`。跨 turn 的占位自然堆叠但语义清晰（每个都是独立 turn 的 permanent status marker）。banner 是否**绘制**以及显示内容**是什么**取决于 §11.12.5.1 (Compose header-skip rule) + §11.12.7.4 (inheritLatestHeader)。详见 §11.11。

`editForumTopic` 只修改 Topic 名称或图标，不修改 Topic 内正文，**不能**用来做占位更新。

### 10.6 与现有架构的映射

现有飞书适配器可以保持业务层不变，Telegram 只需要实现 `channel.Channel` 的等价接口：

| 通用能力 | Telegram Adapter |
| --- | --- |
| `Start` | 启动 Polling 接收循环 |
| `Incoming` | 发布转换后的 `messages.InboundMessage` |
| `Send` | 发送 `OutReply`、`OutResult`、工具和错误消息 |
| `Send` | 所有 `OutboundKind` 的唯一出口；交互选择也走这里 |
| `OnPromptEnded` | 更新占位消息或添加终态 reaction |
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

**修订 (2026-08)**：**不再有 `topic_mode: separate` / `shared` 这个配置**。`tg_<chat.id>:<thread_id>` 拼接已经天然把每个 topic 隔成独立 ChatSession，不同 topic 永久走不同 cs,不需要 sentinel topic / shared mode 这些复杂机制。Q: 旧 binary 怎么过渡？A: 见 §5.1 末"稳定性约束 5"和迁移说明。

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

**修订 (2026-08)**：原来的"主窗口创建 nightme sentinel topic"流程删除。理由:

- `tg_<chat.id>:<thread_id>` 拼接让 chatID 已经是(chat, topic)二元组的纯函数 — 不需要 Telegram 给你分配任何 sentinel topic
- sentinel topic 由 Telegram 分配,ID 不可控,daemon 重启 / state 丢失会导致 ID 漂移 → 违反 chatID 稳定性约束
- 编译选项里去掉了 `topic_mode: separate` / `shared` 开关(§11.6 修订)

**修订 (2026-08-22 plan-C v4)**：DM / 群主窗口（thread_id=0）和真实 topic（thread_id > 0）走**统一的 per-turn 占位 + reaction-driven 状态**方案：

- 每个用户消息进来 → `ensurePlaceholder` **新建**一条 bot 占位 `<b>🤖 Working...</b>`（不是 sentinel topic，是真实 Telegram message）。`PlaceholderMessageID` 和 `UserMessageID` 都覆盖到新 turn 的值。**v9 实际**:cold-create 文本是裸的 `<b>🤖 Working...</b>`,**不带 v7 plan-C 时代**的 `· ⏱ HH:MM:SS` 后缀 —— 时间戳由首次 `OutHeartbeat` 通过 `LastBeatAt` 段补上(`patchChainHeader` → `setHeaderFromHeartbeat`)。占位**是否实际渲染 `<b>🤖 Working...</b>`** 取决于 §11.12.5.1:如果接下来立刻有 body 内容落定 (slash command / 出错等非 agent turn),Compose 把 banner 藏起来,用户只看 body 不挂 "Working..." 假 alive 信号。Agent turn 则正常推进。
- 同一 turn 的所有 OutXxx（`OutReply` / `OutThinking` / `OutToolStart` / `OutToolEnd` / `OutResult` / `OutError` / `OutChoice`）都带 `reply_to_message_id = UserMessageID`，让 reply chain 锚到**用户的原消息**（不是占位）。Topic 模式下额外带 `message_thread_id = thread_id`。
- turn 状态走 **message reaction**：runtime `MessageStateBus` → `OutMessageState` 触发 `setMessageReaction(userMsgID, 👌/🧠)`；`OnPromptEnded` 触发 `setMessageReaction(userMsgID, ✅)` + `setMessageReaction(PlaceholderMessageID, ✅)`。
- `OutHeartbeat` PATCH 当前 turn 的占位文本（`🤖 Working...` / `💭 N · 🔧 M`）—— 是 in-turn 状态 ticker,**只 PATCH active cursor chunk**(§11.12.8)。`📈` 占位字符串怎么**呈现**+ frozen chunks 怎么**展示**也取决于 §11.12.5.1 和 §11.12.7.4 —— 后者让 overflow / split / rotate / tail piece 在**出生时刻** inherit 当下 active 状态,所以老 frozen chunks 仍读得出当时的 think/tool 计数。
- `OutHeartbeat` **不再 PATCH 为 `<b>✅ Completed</b>`**(由 `setMessageReaction(PlaceholderMessageID, 🎉)` 承担终态视觉)
- 跨 turn：老占位留作时间线状态标记（不被 ✅ PATCH、保持 working / heartbeat 文本），新 turn 创建新占位独立承载新状态。

详细方案见 §11.11（v4 历史快照）。当前即将演进到 **v9 per-turn multi-chunk chain rolling log**，见 §11.12。

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

### 11.11 per-turn placeholder + reaction-driven state（v4）

> ▶ **本节为 v4 / v8 历史快照**。当前实现即将演进到 **v9 per-turn multi-chunk chain rolling log**（见 §11.12）。v9 把"独立 bubble + 单一占位 ticker"合并成一条 chain 内的 active chunk + frozen chunks 全量回写，保留 Telegram 不支持 append 的硬约束同时获得飞书式 fold 视觉。v9 落地后，本节内容继续保留作设计决策历史。

Telegram Bot API 在 `chat.type == "private"`（DM）里不支持 Forum Topic，没有 `createForumTopic` / `message_thread_id` 概念。飞书靠 `reply_in_thread` 把中间事件折进 drawer，Telegram 没有等价物。

v4 把 Telegram DM/topic 的视觉状态完全用 **message reactions** 表达（跟 Feishu receipt `AddReaction` + `SetPromptState` 对位），**不再 PATCH 占位文本为 `<b>✅ Completed</b>`**。

> **§11.11 是 v4 / v8 设计快照**。**当前实现的真实行为取决于**:
> - **§11.12.5.1 Compose header-skip rule**(v9 P1)：决定 banner 是否绘制 —— entries 有内容但无心跳时,cold-create banner 隐藏
> - **§11.12.7.4 inheritLatestHeader 契约**(v9 P1.1)：决定每个新 chunk 出生时**继承** active 状态而非 cold "🤖 Working..."
>
> 想了解 banner / chunk 的真实渲染,跳到 §11.12.5 / §11.12.7.4。本节作为设计决策历史保留。

#### 11.11.1 v3 + v6.3 + v7 核心契约

每个用户消息进来 → `ensurePlaceholder` 创建**新的** bot 占位（per-turn），并通过 **`reply_to_message_id = userMessageID` 挂在用户消息下**（v7 改进）。同一 turn 的所有 OutXxx **以及 placeholder 自身** 都在 user message 的 reply thread 下，组成一个统一的对话气泡组。

占位文本承载 turn 状态（含 `⏱ HH:MM:SS` 时间戳 —— v7 改进）：

```text
turn 1: 用户发 "hi 1" (userMsgID=10)
    └─ ensurePlaceholder 创建 P1 = "<b>🤖 Working...</b>" bot 占位
                       (v7: reply_to_message_id=10, 挂在用户消息下)
                       (v9: 纯裸文本,无 v7 plan-C 的 `· ⏱ HH:MM:SS` 后缀;
                        时间戳由首次 OutHeartbeat 通过 LastBeatAt 段补)
        state.PlaceholderMessageID = 700   (P1 的 id)
        state.UserMessageID       = "10"
    ├─ runtime emit MessageQueued(10)   → silent drop (v6.3 单 emoji 预算)
    ├─ runtime emit MessageSubmitted(10)→ setMessageReaction(10, 👌)
    ├─ OutHeartbeat                    → patchChainHeader(700) → setHeaderFromHeartbeat("💭 2 · 🔧 1 · ⏱ 15:18:30")
                                            (v9: 只 PATCH active cursor chunk,frozen chunks 不主动广播)
    ├─ OutReply/Tool/Result             → sendMessage(reply_to_message_id=10, ...)
    └─ OnPromptEnded                    → setMessageReaction(700, 🎉)  ← v6.3: 不动 user msg reaction
```

**v6.3 单 emoji 预算**（user 决定）：Telegram bot 一次只能贴 1 个 reaction 到单条消息。预算用在最有信息量的 state（`MessageSubmitted` = 👌 "AI 在想"），其他 silent drop。

**v7 改进**：

- **Placeholder 也用 reply chain** 挂到 user message（之前是独立消息，现在视觉上是 user message 下的 reply 群）
- (v7 历史) **占位文本曾带 `⏱ HH:MM:SS` 时间戳** —— `placeholderInitialText(now)` helper 拼接当前时间,user 一眼看到 "agent 在 15:18:30 还在跑"。**v9 已移除**:cold-create 现在直接调 `heartbeatText(nil)` = `<b>🤖 Working...</b>`,时间戳由首次 `OutHeartbeat` 通过 `LastBeatAt` 段补上(因为 agent 才真正有"start time"可记)。一行好处:slash / error 等非 agent turn 占位不再带虚假的启动时间戳。
- **(v9 P1, 2026-08-23) 懒汉路径 `ensurePlaceholderForHeartbeat` 移除**：handleMessage 的 `ensurePlaceholder` 是同步、阻塞、并在 publish 之前执行 —— 没有再需要 "OutHeartbeat 先于 handleMessage 抢跑" 的兜底。原先的 race-window guard（`state.UserMessageID == ""` → 返回 `(0, nil)`）随方法一起删除。改由 §11.12.5.1 的 Compose header-skip rule 承接"非 agent turn 不留 stale Working banner"的职责 —— 那是个更干净的位置（render-time 而不是 path-time）。

#### 11.11.2 视觉

```text
Devin: hi 1  (react: 👌)                          11:50
nightme: (reply to hi 1) 🤖 Working...                ← P1 (v7 reply chain; v9 无 ⏱ 后缀)
                       (v9 P1.1 占位文本无 v7 时间戳;首次 OutHeartbeat 由 LastBeatAt 段补)
                     (react: 🎉 when done)
                     ├─ (PATCH) 💭 2 · 🔧 1 · ⏱ 15:18:25
                     ├─ (reply to hi 1) User keeps sending...
                     └─ (reply to hi 1) Hi! 👋 ...
Devin: hi 2  (react: 👌)                          11:55
nightme: (reply to hi 2) 🤖 Working...                ← P2 (v7 reply chain; v9 无 ⏱ 后缀)
                       (v9 P1.1 占位文本无 v7 时间戳)
                     (react: 🎉 when done)
                     ├─ (PATCH) 💭 1 · 🔧 0 · ⏱ 15:18:35
                     └─ (reply to hi 2) Hi! 👋 ...
```

**v7 改进**：所有 bot 消息（placeholder + OutXxx）**都挂在 user message 下**，形成统一 reply thread。

**v9 修订**:不再有 `⏱ HH:MM:SS` 冷启动时间戳(参见 §11.11.1 v7 → v9 段落) — 时间戳由首次 OutHeartbeat 的 `LastBeatAt` 段补,frozen chunks(overflow / split / rotate / tail)在 §11.12.7.4 持有**出生时刻**的 snapshot,让 scrollback 读起来有时序感(§11.12.7.4 完整 timeline 实例)。

#### 11.11.3 emoji 选择

Telegram Bot API 的 `setMessageReaction` 走固定 `ReactionTypeEmoji` 白名单(见 [gist.github.com/Soulter/3f22c8.../reactions-txt](https://gist.github.com/Soulter/3f22c8e5f9c7e152e967e8bc28c97fc9))。白名单外 emoji(包括 ✅ U+2705) API 返 `REACTION_INVALID`。

nightme 当前采用:

| 阶段 | emoji | 语义 | 贴哪条消息 |
| --- | --- | --- | --- |
| MessageSubmitted | 👌 OK-hand | "AI thinking" | user message(占单 reaction slot) |
| OnPromptEnded | 🎉 party popper | "完成 / 庆祝" | per-turn placeholder(独立消息,不复用 slot) |

`✅` 不可用 —— U+2705 check mark 不在 ReactionTypeEmoji 白名单里。v4 → v6.3 → v8(v8 = 现在的 plan-D 状态)讨论过程中,曾用 ✅ / 🎉 两种,最终 v5 live probe 确认 ✅ 被拒,稳定落 🎉。若后续 Telegram 把 ✅ 加进白名单,可考虑切回 ✅("完成" 语义更克制)。

**v6.3 单 emoji 预算**:user message 的单 reaction slot 留给最长持续的状态(MessageSubmitted);placeholder 单独有 reaction slot,装终态。两边不冲突。

#### 11.11.3.1 Telegram ReactionTypeEmoji 白名单(2026-08 snapshot)

来源:[gist.github.com/Soulter/3f22c8e5f9c7e152e967e8bc28c97fc9](https://gist.github.com/Soulter/3f22c8e5f9c7e152e967e8bc28c97fc9) —— Telegram 官方 `ReactionTypeEmoji` 列表。下一次接入新 reaction / 排查 `REACTION_INVALID` 时查这里。

完整列表(80 个 emoji,按原始顺序):

```text
👍 👎 ❤ 🔥 🥰 👏 😁 🤔 🤯 😱 🤬 😢 🎉 🤩 🤮 💩 🙏 👌 🕊 🤡 🥱 🥴 😍 🐳 ❤️‍🔥 🌚 🌭 💯 🤣 ⚡ 🍌 🏆 💔  😐 🍓 🍾 💋 🖕 😈 😴 😭  👻 👨‍💻 👀 🎃 🙈 😇 😨 🤝 ✍ 🤗 🫡 🎅 🎄 ☃ 💅 🤪 🗿 🆒 💘 🙉 🦄 😘 💊 🙊 😎 👾 🤷‍♂️ 🤷 🤷‍♀️ 😡
```

按语义分组(便于查询):

| 语义 | emoji |
| --- | --- |
| **OK / 确认** | 👌 👍 ❤ 🤝 👏 |
| **庆祝 / 完成** | 🎉 🥳 🤩 💯 🏆 ✨(✨ 不在白名单) |
| **思考 / 困惑** | 🤔 😐 🤨 🧐 🕊 |
| **强烈反应** | 🤯 😱 🤬 😡 🤮 💩 |
| **喜爱** | 🥰 😍 😘 ❤️‍🔥 💋 💘 |
| **笑** | 😁 🤣 😂(不在白名单) |
| **哭 / 同情** | 😢 😭 😨 😐 |
| **工具 / 工作** | 👨‍💻 🤓 🛠(不在白名单) |
| **季节 / 节日** | 🎃 🎄 🎅 ☃ |
| **动物** | 🐳 🦄 🕊 👻 |
| **食物** | 🍌 🍓 🍾 🌭 💊 |
| **神秘 / 离奇** | 🗿 🤡 🥱 🥴 🌚 |
| **手势 / 表情** | 🖕 ✍ 🤗 🫡 💅 👀 🙈 😇 🙉 🙊 😎 |
| **手指 / 人物** | 👾 🤷 🤷‍♂️ 🤷‍♀️ |
| **天气 / 自然** | ⚡ 🌭 🏆 💯 |
| **常用但** ❌**不在**白名单**(会被 API 拒) | ✅ ⭐ 🧠 🌟 🥳 ✨ 🙏(✅ 在) |
| **白名单内** ✅ 可用 | 👌 🎉 👏 💯 🤝 👀 🙏(🙏 在) |

**重点提醒**:

- ✅ U+2705 **不在** 白名单(`REACTION_INVALID`);🎉 是最直接的 "Done" 替代
- 🧠 🟢 ⭐ 🟡(彩色圆圈 emoji 部分)在白名单外
- `🙏` 在白名单(常被误以为不在,因为它常被错认成 fold-hands)
- 🙏= fold-hands(白色),🫰= crossed-fingers(可能不在白名单)

**怎么验证未来候选 emoji**:用 `cmd/probe-reaction/main.go`(已删除,需要时重建)或写 ad-hoc 脚本:

```bash
curl -s "https://api.telegram.org/bot$TOKEN/setMessageReaction" \
     -d chat_id=$CHAT_ID \
     -d message_id=$MSG_ID \
     -d 'reaction=[{"type":"emoji","emoji":"<candidate>"}]'
# 返回 {"ok":true,...} 即白名单内;{"ok":false,"error_code":400,"description":"Bad Request: REACTION_INVALID"} 即白名单外
```

未来若 Telegram 扩展白名单,把新 emoji 加进 `mapStateToTelegramEmoji` 即可(`internal/channel/telegram/adapter.go` 中的 switch)。

v4 的 👌/🧠/✅ 全部失败。v5 probe 结果（35 个候选 emoji 实测）：

| 白名单内（JSON ✓） | 白名单外（JSON ✗） |
|---|---|
| 👍 👎 👌 ❤ 🔥 🎉 👏 🙏 😁 🤔 💩 🤯 💯 👀 😐 🤝 🫡 🆒 🎃 😈 👻 | 🧠 ✅ 🥳 ⭐ 🙄 |

#### 11.11.3.1 单 reaction 限制（v6.1 修订）+ 单 emoji 预算（v6.3）

**Telegram 平台硬限制**：bot 在 `setMessageReaction` 一次调用中只能设 **1 个 reaction** 到单条消息（实测发 2 个 emoji 会返 `REACTIONS_TOO_MANY`，`max_reaction_count=11` 是 chat-level 总反应种类上限，bot 单 reactor 上限仍是 1）。

**v6 原始设想**（累计 list 模拟 append）：❌ 不可行 ——Telegram 拒绝 `[👌, 🤔, ✅]` list。

**v6.1 实际实现**：每个 state emit 1 个 reaction，**SET 语义**（覆盖而非 append）。

**v6.3 进一步收紧**（user 决定）：单 reaction 预算用在最有信息量的 state —— **`MessageSubmitted = 👌`**。

```text
Queued    → silent drop   (placeholder 文本 PATCH "🤖 Working..." 承担 "收到" 视觉;**v9 P1 修订**:如 §11.12.5.1,body 内容先于首次 OutHeartbeat 落地时 banner 自动隐藏,避免 stale Working 假 alive)
Submitted → 👌            (单 reaction slot 固定给 "AI thinking")
Done      → silent drop   (OnPromptEnded 在 placeholder 上贴 ✅)
```

**为什么这样**：

- `Queued` 太瞬时（消息到 adapter 立刻变 `Submitted`），reaction 来不及显示就变
- `Done` 终态由 placeholder 文本 PATCH + placeholder 🎉 reaction 承担，user message 上不再贴
- `Submitted` 是 turn 中持续时间最长的状态，最值得让 user 看到"AI 在想"

**视觉**（user message 角度）：

```text
Devin: hi
nightme: 🤖 Working... ← placeholder (v9 冷启动时 banner 仅在 entries 空时渲染)
            (用户消息上: 👌 一直挂着,直到下次 turn)
            (placeholder 文本: "💭 2 · 🔧 1" 持续更新)
OnPromptEnded:
            (placeholder 文本: 不再 PATCH, 保持 "💭 2 · 🔧 1")
            (placeholder reaction: 🎉 贴上)
            (user msg reaction: 仍为 👌 不变,留给下一 turn 看新进度)
```

**实现细节**：

- `mapStateToTelegramEmoji(state)`：v6.3 只对 `MessageSubmitted` 返回 `👌`，其他 silent drop
- `setMessageReactions(ctx, chatID, msgID, [reactions])` 接收 list 形参
- 同一 state 重复 set 是 idempotent（`messageStates` LRU dedup）
- `OnPromptEnded` 不再对 user message 贴 reaction（保留 reaction slot），只对 placeholder 贴 🎉

**对比飞书**：

- Feishu `AddReaction` 是 append-by-design，每条 user message 可以累积多个 reaction
- Telegram 平台硬限制只能 1 个 reaction 在 user message 上
- 这是 Telegram 平台 vs 飞书平台的根本 UX 差异，无法 workaround

Future work: 如果 Telegram 放宽 JSON API 白名单，可以重新启用 v4 的 👌/🧠/✅。`mapStateToTelegramEmoji` 的实现与白名单同步更新。

#### 11.11.4 Topic 路径同样适用

| 行为 | topic (thread_id>0) | DM (thread_id==0) |
| --- | --- | --- |
| `ensurePlaceholder` | 每条 user msg 创建新 P_N | 同上 |
| `OutHeartbeat` | `editMessageText(P_N)` PATCH（in-turn status） | 同上 |
| `OutReply/OutTool/OutThinking/OutResult/OutError/OutChoice` | Topic 内独立消息 + `message_thread_id` + `reply_to_message_id=userMsgID` | 主窗口消息 + `reply_to_message_id=userMsgID` |
| `OnPromptEnded` | 🎉 reaction on **P_N only**（userMsg 留 👌 不动，v6.3 单 reaction 预算） | 同上 |
| `👌` reaction | runtime eventbus 触发 → `MessageSubmitted` → `setMessageReaction(userMsgID, 👌)` | 同上 |

#### 11.11.5 chatID 稳定性约束保持

`sessionChatID(rawChatID, thread_id)` 仍是 `tg_<chatid>[:thread_id]`，纯函数。UserMessageID 和 PlaceholderMessageID 不进 chatID 拼接，只作为持久化字段存在 `telegram_state.json` 的 `TopicState`。§5.5 约束 1-4 全部满足。

跟历次方案对比：

| | sentinel topic（已废弃） | v1 (跨 turn 复用占位) | v2 (DM 无占位) | v3 (每 turn 占位 + 文本 ✅) | **v4 (每 turn 占位 + reaction ✅)** |
| --- | --- | --- | --- | --- | --- |
| 占位 ID 来源 | Telegram 分配（不可控） | 1 个/chat 复用 | DM 无 | 每 turn 新建 | **每 turn 新建** |
| reply chain 锚 | — | bot 占位（不直观） | user msg | user msg | **user msg**（topic + DM 一致） |
| daemon 重启后 chatID 一致 | ❌ 可能漂移 | ✅ | ✅ | ✅ | ✅ |
| 在 DM 里能跑 | ❌ Forum-only | ✅ | ✅ | ✅ | ✅ |
| Turn 状态视觉 | — | 占位文本 PATCH | reaction only | 占位文本 PATCH | **reaction on user msg + reaction on placeholder** |
| Turn 终态 | — | 占位 "✅ Completed" | reaction on user | 占位 "✅ Completed" | **✅ reaction on both (no text PATCH)** |
| 用户体验 | 飞书 drawer | 占位堆叠 | 飞书 receipt reaction | 占位堆叠 + ✅ | **飞书 receipt reaction 等价（user + card 都 ✅）** |

#### 11.11.6 跟飞书 receipt 的语义对位

| | Feishu receipt | Telegram v4 |
| --- | --- | --- |
| 状态 ticker | ✅ header PATCH（card） | placeholder PATCH（Working / 💭 N·🔧 M） |
| OutThinking | append div | 独立 reply to user msg |
| OutToolStart/End | append div | 独立 reply to user msg |
| OutReply | 独立气泡（F-44 后） | 独立 reply to user msg |
| OutResult | 独立气泡（F-39 后） | 独立 reply to user msg |
| **user message 状态** | 👌 / 🔄 / ✅ **reaction** (AddReaction) | 👌 **reaction** (setMessageReaction；✅ 在 JSON body 白名单外拒收) |
| **card / placeholder 状态** | ✅ header (SetPromptState) | 🎉 **reaction** (setMessageReaction on placeholder) |
| 终态 | ✅ reaction + card header ✅ | user msg 留 👌 不动（v6.3 单 reaction 预算）；placeholder 贴 🎉 |

两个维度正交：**状态走 reaction**（user msg 和 placeholder 两边都贴），**内容走 reply chain**（锚 user msg）。

#### 11.11.7 OutChoice 在 topic 和 DM

Choice card 也 `reply_to_message_id = userMsgID`，让权限/问题卡片挂在用户原消息下、视觉对齐。topic 模式下还带 `message_thread_id` 进入对应 Topic。

#### 11.11.8 验收

> 本节是 v4 / v8 行为快照。**当前实现的真实验收为** §11.12.16 测试矩阵(覆盖 §11.12.5.1 Compose header-skip + §11.12.7.4 inheritLatestHeader + §11.11.8 本节原始契约)。下面条目仅作 spec 背景,具体验收 = §11.12.16。

- 同一 DM/topic 跨 daemon 重启，chatID 仍是 `tg_<chatid>[:thread_id]`，state 从 `telegram_state.json` hydrate
- 每条用户消息进来 → `ensurePlaceholder` **新建** bot 占位，更新 `state.UserMessageID` 和 `state.PlaceholderMessageID`
- 同一 turn 的 `OutReply` / `OutThinking` / `OutTool*` / `OutResult` / `OutError` / `OutChoice` 都带 `reply_to_message_id = userMsgID`（topic 还带 `message_thread_id`）
- `OutHeartbeat` PATCH 当前 turn 的 active cursor chunk 文本（`🤖 Working...` 或 `💭 N · 🔧 M`)—— 不 PATCH 为 ✅ Completed。**v9 P1.1 修订**:只 PATCH active cursor,**不** 广播到所有 frozen chunks(避免 N×editMessageText 风暴);frozen chunks 在 §11.12.7.4 inheritLatestHeader 规则下保持**出生时刻**的 (header, hasHeartbeat) 快照,scrollback 仍读得出 think/tool 推进时序。**v9 P1 修订**:banner 是否绘制取决于 §11.12.5.1(非 agent turn 的 body+no-heartbeat 不画 banner)。
- 运行时 eventbus → OutMessageState → `setMessageReaction(userMsgID, 👌)`（MessageDone 不在 async dispatch emit，由 `OnPromptEnded` 兜底）
- `OnPromptEnded` 调 `setMessageReaction(PlaceholderMessageID, 🎉)`（**不**碰 user message；保留 👌 让下一 turn 的状态可见），**不调 editMessageText**
- 跨 turn：老占位 P_N-1 留作时间线证据（不被 ✅ Completed PATCH，但保持 working / heartbeat 文本）
- 测试矩阵：`TestMapStateToTelegramEmoji` (👌) / `TestAdapter_Send_OutMessageState_SubmittedRenders` (👌) / `TestAdapter_Send_OutMessageState_QueuedRenders` (silent drop) / `TestAdapter_Send_OutMessageState_DoneRenders` (silent drop) / `TestAdapter_OnPromptEnded_DM_ReactsOnUserAndPlaceholder` (🎉 ×1 on placeholder, NO reaction on user msg) / `TestAdapter_HandleUpdate_DM_CreatesPerTurnPlaceholder` / `TestAdapter_Send_DM_RepliesToUserMessage` / `TestAdapter_Send_DM_OutHeartbeat_PATCHesPlaceholder` / `TestAdapter_Send_Topic_ReplyToUserMessageToo` / `TestStateStore_DM_Persistence` / `TestSessionChatID_DM_StillStable`
- placeholder 不再"懒创建"——`ensurePlaceholder` 在 handleMessage 同步预先建好；OutHeartbeat 走 patchChainHeader 直接 PATCH 已存在的 chunk header（第 1383 行的 `placeholderAnchor` race-window guard 已移除）
- 跨 turn：老占位 P_N-1 留作时间线状态标记（不动），新 turn 创建 P_N 独立承载新状态
- 测试矩阵：`TestAdapter_HandleUpdate_DM_CreatesPerTurnPlaceholder` / `TestAdapter_Send_DM_RepliesToUserMessage` / `TestAdapter_Send_DM_OutHeartbeat_PATCHesPlaceholder` / `TestAdapter_OnPromptEnded_DM_PATCHesPlaceholder` / `TestAdapter_Send_Topic_ReplyToUserMessageToo` / `TestAdapter_Send_Topic_NoReplyToPlaceholder`（**已替换为 replyToUserMessage 版本**）/ `TestStateStore_DM_Persistence` / `TestSessionChatID_DM_StillStable`
- v9 P1 增加：`TestRenderActiveChunkBody_SkipsHeaderWhenBodyButNoHeartbeat` / `TestRenderActiveChunkBody_HeaderAndBody` / `TestRenderActiveChunkBody_HeaderOnlyAfterHeartbeat`（§11.12.5.1）
- (v9 P1 2026-08-23 移除) `TestAdapter_EnsurePlaceholderForHeartbeat_CreatesWhenMissing` / `TestAdapter_EnsurePlaceholderForHeartbeat_ReusesExisting` / `TestAdapter_EnsurePlaceholderForHeartbeat_DMCreates` / `TestAdapter_EnsurePlaceholderForHeartbeat_DeferWhenNoUserMsgID` / `TestAdapter_Send_OutHeartbeat_DeferWhenNoUserMsgID` —— 懒汉路径删除后随之清理

## 11.12 per-turn multi-chunk chain rolling log（v9）

> **v9 = §11.11 v8 的进化版**。落地中，分多 commit 推进，详细 diff 见 §11.12.15。本节是 spec，不是已实现的描述。

### 11.12.1 核心思路与 v4 / v8 的差异

§11.11 v4 / v8 的视觉模型是双轨制：
- 一张 per-turn 占位 + `OutHeartbeat` PATCH 文本 + `OnPromptEnded` 贴 🎉
- 7 种 text-emitting kind（OutReply/OutThinking/OutToolStart/End/OutResult/OutError/OutTask*）**各自发独立 bubble**，每条都钉 userMsgID 的 reply chain

后果：长 turn → chat 里出现几十条分散的小 bubble，占位本身不携带任何事件历史。

v9 把这两轨**合并成一条 chain**：
- 占位从一个常量 → 链子 `placeholderChain{chunks, cursor, lastFooter}`，每个 chunk 是一个独立的 Telegram message
- 7 种 text-emitting kind 不再发独立 bubble，全部 append 成单行 segment，**进当前 active chunk 的内存 buffer**
- buffer 累计 3500 chars raw → 当前 chunk 锁死 + 新建 chunk 接上
- 全量回写走 `editMessageText(activeChunkID, render(buffer))`，**保留 Telegram 不支持 append 的硬约束**

借用飞书 receipt card 的 fold 进同一 surface 的视觉模型，但用 debounce + chunk rotation 解决 Telegram 的全量替换本质。

### 11.12.2 数据模型（纯内存，OOP chunkBody）

```go
// internal/channel/telegram/placeholder_chain.go + chunk_body.go

type chainKey struct {
    chatID        string        // 原始 chat.id（无 tg_ 前缀）
    topicID       int           // 0 表示 DM / 主窗口
    userMessageID int           // 每个 user 消息一条独立 chain —— 锁死 back-to-back user msg 的 Out* 串扰
}

type placeholderChain struct {
    mu            sync.Mutex
    chunks        []*chunkBody         // OOP Layer 1 view type
    cursor        int                  // 当前可写 chunk 的 index；-1 = 空链
    lastFooter    []string             // 最近一次携带 status 数据的 Out* 留下的 StatusBar 行
    dirty         bool                 // 内存有更新未写回 Telegram（debounce 用）
    debounceTimer *time.Timer          // 当前 pending 的 debounce flush
}

// chunkBody — one Telegram message. Business code mutates fields
// through methods (setHeader / setHeaderFromHeartbeat /
// appendEntry / appendError / setFooter / markFull); Compose() is
// the sole render path.
type chunkBody struct {
    messageID    int64
    isFull       bool
    header       string                  // pre-baked HTML (e.g. "<b>💭 1</b>")
    hasHeartbeat bool                    // v9 P1 (§11.12.5.1): true once any OutHeartbeat
                                        // patched this chunk's header. Compose uses it
                                        // to decide whether to render header at all.
    entries      []chunkEntry            // ordered log lines
    footer       string                  // statusbar panel
    flushedLen   int                     // overflow tracking (P0 #2 fix)
}

type chunkEntry struct {
    text   string
    isHTML bool                        // skip RenderMarkdown when true
}

// chunkBody API (Layer 1 business methods + Compose):
//   setHeader(h)                  → status line (cold-create path: leaves hasHeartbeat=false)
//   setHeaderFromHeartbeat(h)     → status line + flips hasHeartbeat=true (OutHeartbeat path)
//   appendEntry(text)             → plain-text segment
//   appendEntryHTML(text)         → pre-rendered HTML segment (SPLIT path, §11.12.7.2 trigger 1)
//   appendError(text, stderr)     → wraps stderr in ```fences``` (Layer 3)
//   setFooter(f)                  → statusbar panel
//   markFull()                    → lock chunk
//   freezeAfterOverflow(n)        → clear entries, set flushedLen
//   markFlushedLen(n)             → record overflow emit bytes
//   Compose()                     → safe-HTML wire format (header-skip rule per §11.12.5.1)

// chainLRU is the Adapter-scoped index with cap-bounded LRU eviction.
type chainLRU struct {
    mu     sync.Mutex
    cap    int                      // 默认 1000（按 user 消息计数，非按 chat）
    chains map[chainKey]*placeholderChain
    order  []chainKey               // LRU 顺序（最近访问在尾）
}
```

**OOP 分层**：
- **Layer 1 (data + view)**: chunkBody + chunkEntry + Compose() — 单条 Telegram 消息的数据 + 渲染
- **Layer 2 (business API)**: chain 的 append / overflow / flush / purge 等业务方法
- **Layer 3 (format decisions)**: chunkBody.appendError 把 ```fences``` 决策封装在数据层
- **Layer 4 (UI text escape)**: escapeInline helper 收拢 InlineKeyboard 文本转义
- **Layer 5 (network)**: sendTelegramMessage / editTelegramMessage

**不持久化**：
- `TopicState.PlaceholderChunkIDs` 不写
- chain 内的 `entries` / `header` / `footer` / `lastFooter` 全部是纯内存字段
- daemon 重启 = chain 失；下次事件来时 `cursor=-1` → 走"建第一张 chunk"路径（旧 frozen chunks 留在 chat 里作为历史证据，没人再去 editMessageText）

`TopicState.PlaceholderMessageID` 保留为 read-only 兼容字段（不再写）。

**LRU cap 含义**：因 key 含 userMessageID，cap=1000 表示"1000 个 user 消息各持一条独立 chain"，而不是"1000 个 chat 各持一条 chain"。活跃 chat 跑 1000 turn 短期不会爆；不活跃 chat 的旧 chain 在 access-order 上被自动 evict。

### 11.12.3 三档阈值

| 阈值 | 值 | 触发条件 |
|---|---|---|
| 单 chunk 缓冲上限 | **3500** chars (raw) | `cur.charCount + len(segment) > 3500` → 锁当前 + 新建 chunk（ROTATE） |
| 单 chunk 渲染硬上限 | **3900** chars (rendered) | `len(rendered) > 3900` → flushChainNow 触发 safety-net ROTATE |
| Telegram API 硬限 | **4096** chars | `editMessageText` / `sendMessage` 直接拒绝超长，**永不发送** |

3500 - 3900 = 400 chars 给 HTML escape + emoji 字节增长。
3900 - 4096 = 196 chars 给 Telegram 内部 buffer。
复用 `maxTelegramTextLength = 3900`（已在 `topic.go:12` 定义）。

**为什么需要 SPLIT 路径**：单条 OutReply / OutResult 等 payload 可能本身就 > 4096 chars raw（例如用户粘贴长 stacktrace、agent 输出长文档）。这种情况即使 chain 是空的、即使 raw 累积阈值没触犯，第一条 `sendMessage` 仍会被 Telegram 拒。**SPLIT 路径**（§11.12.7.2 trigger 1）在 append 阶段把单条 entry 切成多张 Telegram message，绕开 API 硬限。

### 11.12.4 事件映射（每 Out* 进哪条路径）

| Kind | 路径 | segment 格式 |
|---|---|---|
| `OutReply` | `appendSegment` | `<text>\n`（流延续，无 icon） |
| `OutResult` | **独立 `sendMessage` (reply to user msg)** | `<text>` + StatusBar trailer；v9 P2 起不再进 chain。详见 §11.12.4.1 |
| `OutThinking` | `appendSegment` | `💭 <text>\n` |
| `OutToolStart` | `appendSegment` | `● <name>(<args truncated>)\n` ← `formatToolStartCall` |
| `OutToolEnd` | `appendSegment` | `⎿  <one-line summary>\n` ← `summarizeToolResult` |
| `OutError` | `appendErrorSegment` (via `chunkBody.appendError` ```fences```) | `❌ <text>\n````<stderr>\n```` |
| `OutTaskCreate` | `appendSegment` | `📋 🆕 <subject>\n` |
| `OutTaskUpdate` | `appendSegment` | `📋 <status emoji> <subject>\n` |
| `OutHeartbeat` | `patchActiveHeader` | （无 segment，刷新 active chunk headerLine） |
| `OutChoice` / `OutChoicePatch` | 独立 `sendMessage` + InlineKeyboard | （不进 chain） |
| `OutCommandReply` | `appendSegment` (折进 chain,带 StatusBar trailer) | `<text>\n` |
| `OutMessageState` / `OutMessageStateRemoved` | reaction 路径（`setMessageReactions`） | （不动） |
| `OutInit` | silent drop | — |

**Out* payload 超长处理**：上面 8 个走 `appendSegment` / `appendErrorSegment` 的 kind 中，若单条 payload 自身 raw > 3500 chars（接近 4096 Telegram 硬限），§11.12.7.2 trigger 1 在 append 阶段直接 SPLIT 成多张 Telegram message，不再走普通 ROTATE。用户视觉上看到的是同时间戳的多片连续消息（视觉连续 vs ROTATE 的页面跳转）。

#### 11.12.4.1 OutResult 独立消息（v9 P2, 2026-08-24）

v9 P2 之前，OutResult 跟其他文本 kind 一样走 `appendSegment` 进 active chunk buffer，没有视觉差异 —— 同一个 chunk 里 `💭 thinking → ● Bash → ⎿ done → 📝 <result>` 挤在一起，`📝` 前缀其实从没真正渲染过（v9 Send 没加，`docs §3 表格里的"📝 <text>"是 v3 时代留下的描述，跟当前实现不一致`）。OnPromptEnded 的 🎉 贴 active chunk 的 messageID —— 如果 OutResult 之后又来了 OutReply / OutToolEnd，🎉 就飞到非 result 的 chunk 上了，**语义错位**。

P2 把 OutResult 改独立 `sendMessage`：

```text
Devin: 帮我写一个 go http server                  userMsgID = 42
nightme: 🤖 Working...                            ← chunk #0 placeholder / active
         💭 thinking...
         ● Bash(go mod init)
         ⎿  📂 1 file
         💭 thinking...
         ● Write(...)
         ⎿  📝 Write → 42 bytes                   ← chunk #1 (ROTATE)
[独立新消息] reply_to_message_id = 42
nightme: 这是一个简单的 http server ...            ← result body
        ┌──────────────›                          ← StatusBar trailer 边框
        │  🤖: claude · opus-4-5 · sess-1
        │  💰:「$0.05」
        │  📁: code/nightme
        └───────────────›                          (无中间 ──────── 分隔;
        [🎉 reaction]                               trailer 自带 frame 边界)
```

**核心契约**：

1. **OutResult 不进 chain**。`Send()` 把 OutResult 从 default 分支挑出走 `sendOutResultMessage` helper —— 直接调 `sendTelegramMessage(chat_id, topic_id, reply_to_message_id=userMsgID, text=result+trailer)`，每条 OutResult 都是独立的 Telegram message。
2. **StatusBar trailer 一致，无中间分隔**。所有 text-emitting kind 都带 §18 trailer，OutResult 也不例外 —— body 末尾追加 `\n` + `statusbar.RenderPanel(sb)` 三行（`🤖 / 💰 / 📁`，box-drawing frame `┌──› / └──›` 提供视觉边界）。**OutResult standalone 不画 `────────` 横线**（2026-08-24 user feedback：trailer 自带 frame，分隔线反而让 result message 显得"断裂"）。**Chain chunk 仍然画 `────────────────` 分隔线**（chunk_body.Compose 在 entries 和 footer 之间硬编码这一行），因为 chain 上 entries 是一长串 activity log，footer 是状态 summary，两者之间需要强分隔。
3. **长 result 自动 split**。`len(result+trailer) > 3900` → `splitTelegramText` 切成多片，每片单独 sendMessage（都带 `reply_to_message_id=userMsgID`，视觉上是 user msg 下的一组 reply 簇）。只有**最后一片**的 messageID 记录到 `chain.resultMessageID`（参见 §11.12.4.1.1）。
4. **OnPromptEnded 🎉 锚点切换**。优先选 `chain.resultMessageID`；零值（turn 没收到任何 OutResult —— 纯 error / 纯 tool / 纯 slash command）回退到 active chunk 的 messageID，保住 v9 P1 行为。详见 §11.12.9。
5. **chain 仍然承载中间产物**。OutReply / OutThinking / OutToolStart / OutToolEnd / OutError / OutTaskCreate / OutTaskUpdate / OutCommandReply 全部继续走 `appendSegment` —— 这套不动。

##### 11.12.4.1.1 `chain.resultMessageID` 字段

```go
type placeholderChain struct {
    // ... 既有字段 ...

    // resultMessageID is the Telegram message_id of the most recent
    // OutResult sent as a standalone reply in this turn. OnPromptEnded
    // prefers this anchor for its terminal 🎉 reaction over the active
    // chunk's messageID. Zero means no OutResult landed this turn
    // (e.g. error-only / tool-only / slash-only turns) — fall back to
    // the active chunk to preserve v9 P1 behavior. Scoped per-chain
    // (key contains userMessageID) so LRU eviction of an unrelated
    // turn's chain drops it along with everything else. Pure in-memory,
    // not persisted to telegram_state.json.
    resultMessageID int64
}
```

**为什么需要这个字段**：OnPromptEnded 必须能精确定位"turn 的成品输出"是哪条 Telegram message。OutResult 之前进 chain 的时候不存在这个问题 —— 它就是 active chunk 的最后一条 entry；现在 OutResult 走独立消息，OnPromptEnded 需要一个 in-memory anchor。

**生命周期**：

- 在 `Send(OutResult)` 的 helper 里赋值（多次 OutResult 取最后一次的 messageID）
- 在 `chain.purge` / `chain.reset` 跟随 chain 一起清零（next turn 干净启动）
- 不进 telegram_state.json（跟 chain 的其余 in-memory 字段一致 —— §11.12.10 重启 = chain 失，重启后第一次 OutResult 重新 fill）

##### 11.12.4.1.2 跟飞书 receipt 的对位变化

| 维度 | Feishu receipt（v9 P1） | Telegram v9 P1 | Telegram v9 P2 |
|---|---|---|---|
| Surface | 单一 receipt Card 2.0，PATCH 复用 | chain of N chunks，editMessageText 复用 active | chain of N chunks（中间产物）+ 1 张独立 result 消息 |
| OutThinking / Tool / Reply / Error / Task / CommandReply | append 进 card body | append segment 进 active chunk | append segment 进 active chunk（不变） |
| OutResult | 独立 reply (F-39 后) | append segment 进 active chunk（跟其他文本混在一起） | **独立 sendMessage + reply_to_message_id=userMsgID** |
| StatusBar trailer | card `<hr>` + 灰色 markdown | chunk 末尾 renderPanel(lastFooter) | result 消息末尾 renderPanel(lastFooter) |
| 🎉 终态 | ✅ reaction + card header ✅ | user msg 👌 不动；active chunk 贴 🎉 | user msg 👌 不动；**result message 贴 🎉**（无 result 时回退 active chunk） |

跟飞书 F-39 决策完全对齐：result 是 turn 的成品输出，**独立消息**而非 inline 进 receipt。

### 11.12.5 核心 API

```go
// appendSegment 是热路径入口，所有 Out* 都过这里。
// Package-level (not Adapter method)：caller 注入 sendFn / editFn
// closure，测试用 test-double，生产用 Adapter.chainSendFn/chainEditFn。
// Pre-check (§11.12.7.2 trigger 1): len(segment) > chainChunkThresholdChars
// → splitOversizedSegmentLocked. 否则走 case 1/2/3 (cold-create /
// append-in-place / rotate). Mutates chain state under chain.mu;
// caller MUST NOT have chain.mu held when calling.
func appendSegment(
    ctx context.Context,
    chain *placeholderChain,
    chatID string,
    topicID int,
    userMessageID int,
    segment string,
    statusBarLines []string,    // nil = 不动 footer；非空 = 刷新 footer
    sendFn sendChunkFn,
    _ editChunkFn,               // unused; kept for signature symmetry
) error

// appendErrorSegment 是 OutError 专用路径。结构跟 appendSegment
// 平行但走 chunkBody.appendError (```fences``` wrapping 在 Layer 3)。
// 同样的 trigger 1 pre-check (estimateErrorSize > threshold) →
// splitOversizedErrorSegmentLocked.
func appendErrorSegment(
    ctx context.Context,
    chain *placeholderChain,
    chatID string,
    topicID int,
    userMessageID int,
    text, stderr string,
    statusBarLines []string,
    sendFn sendChunkFn,
    _ editChunkFn,
) error

// flushChainNow 全量同步写回 active chunk. debounce timer fires /
// OnPromptEnded / Stop / 测试用。No-op when chain.dirty=false.
// §11.12.7.2 trigger 3 (safety net): len(rendered) > 3900 →
// splitTelegramText + 多 chunk rotate (同 trigger 1 但走 flush 路径).
func flushChainNow(
    ctx context.Context,
    chain *placeholderChain,
    chatID string,
    topicID int,
    userMessageID int,
    editFn editChunkFn,
    sendFn sendChunkFn,
) error

// scheduleFlushDebounced arm 250ms timer. 重置之前的 timer 让 burst
// 合并成 1 edit。Fresh ctx with 5s timeout (request ctx 已 cancel
// 时 timer 还能 fire)。
func scheduleFlushDebounced(
    chain *placeholderChain,
    editFn editChunkFn,
    sendFn sendChunkFn,
    chatID string,
    topicID int,
    userMessageID int,
)

// chains.getOrCreate / lookup / purge — chainLRU 方法。Key =
// (chatID, topicID, userMessageID). cap = 1000 (per-user-msg, 见 §11.12.2).
func (l *chainLRU) getOrCreate(chatID string, topicID, userMessageID int) *placeholderChain
func (l *chainLRU) lookup(chatID string, topicID, userMessageID int) (*placeholderChain, bool)
func (l *chainLRU) purge(chatID string, topicID, userMessageID int)

// Adapter.patchChainHeader: OutHeartbeat 专用。setHeaderFromHeartbeat
// (active chunk, heartbeatText(msg.Heartbeat)) —— 注意是 setHeaderFromHeartbeat
// 不是 setHeader,前者会同时把 hasHeartbeat 翻成 true,触发 §11.12.5.1
// 的"render 规则"。scheduleFlushDebounced。Adapter 方法(不像
// appendSegment/flushChainNow 是 package-level),因为它要从 caller
// 显式透传 replyAnchor。
func (a *Adapter) patchChainHeader(
    chatID string,
    topicID int,
    userMessageID int,
    msg messages.OutboundMessage,
) error
```

#### 11.12.5.1 Compose header-skip rule（v9 P1, 2026-08-23）

替换掉原先的懒汉 placeholder-resolve 路径（`ensurePlaceholderForHeartbeat`），把"非 agent turn 不留 stale `🤖 Working...` banner"的职责挪到 render-time。这是 v9 唯一一处主动拒绝画 header 的位置。

**规则**（`chunkBody.Compose` 实现）：

```go
renderHeader := b.hasHeartbeat || len(b.entries) == 0
if renderHeader {
    out.WriteString(b.header)
    out.WriteByte('\n')
    if len(b.entries) > 0 {
        out.WriteString("────────────────\n")
    }
}
// entries + footer 不受影响
```

**矩阵**（每个 case 都对应实际生产场景）：

| 场景 | hasHeartbeat | entries | renderHeader | 用户看到 |
|---|---|---|---|---|
| Cold-create, body 空 | false | [] | true | `<b>🤖 Working...</b>` |
| Slash command reply (`/gtw fix` → OutCommandReply) 走完无 OutHeartbeat | false | ["✅ Local worktree ready"] | **false** | 仅 `✅ Local worktree ready` ✅ |
| Agent turn: cold-create → first OutHeartbeat | true | [] | true | `<b>💭 N · 🔧 M</b>` |
| Agent turn: cold-create → first OutReply 但 heartbeat 落后 | false | ["first reply"] | **false** | 仅 reply 内容 |
| Agent turn: heartbeat + body 都到了 | true | ["thought", "tool call"] | true | header + 分隔 + body |
| (reaction / callback click)| — | — | — | reaction path 走 `handleMessageReaction` 不进 `handleMessage`,不创建 placeholder —— 无 banner、无 chain,跟 v9 P1 无关 |

**为什么这么改**：

- **原痛点**：v8 / v9 早期实现里,`ensurePlaceholder` 是饿汉（每个 incoming msg 立刻 sendMessage 占位),但 `OutHeartbeat` 是 agent 才会发的。非 agent 路径(slash command / reaction / WatchMode 拒绝 / spawn failed)的 placeholder 永远停在 `🤖 Working...`,直到下条 inbound 触发 `chains.purge` 才被遗忘。屏幕上一直挂着一行假的 "Working"。
- **原 v9 尝试方案** (commit `33a1b81` 之前)：在 `Send()` 入口 lazy-create placeholder on demand (`ensurePlaceholderForHeartbeat`)。这个 path 跟饿汉 `ensurePlaceholder` 双发,偶尔孤儿 orphan 一条未被 patch 的占位。race-window guard `state.UserMessageID=="" → return (0, nil)` 被 codex review 标红过但不彻底。
- **P1 真正修复的位置**：render-time 而不是 path-time。一条死规矩 "outHeartbeat 来过 → header 必出;否则只在 entries 空时出" 即可同时解决所有 turn path(slash / agent / error / reaction)的视觉问题,不需要 lazy 也不需要 lazy 的 race guard。

**call site 配套改动**：

- `chunkBody` 加 `hasHeartbeat bool` 字段 + `setHeaderFromHeartbeat(h)` 方法
- `Compose()` 改"renderHeader = hasHeartbeat || entries empty"
- `Adapter.patchChainHeader` 真分支从 `chunk.setHeader(...)` 改为 `chunk.setHeaderFromHeartbeat(...)`(cold-create / chain rotation / OutHeartbeat 兜底分支保持 `setHeader` —— 维持 `hasHeartbeat=false`)
- `Adapter.Send` 删除 `placeholderAnchor, placeholderErr := a.ensurePlaceholderForHeartbeat(...)` + 错误日志块;OutHeartbeat case 简化为 `return a.patchChainHeader(...)`(去掉 `if placeholderAnchor > 0` guard,理由见下)
- `ensurePlaceholderForHeartbeat` 方法 + 5 个对应测试删除(`TestAdapter_EnsurePlaceholderForHeartbeat_*` / `TestAdapter_Send_OutHeartbeat_DeferWhenNoUserMsgID`)
- 加 4 个新测试(§11.12.16 矩阵)

**为什么 OutHeartbeat 去掉 `if placeholderAnchor > 0` guard 是安全的**：guard 的存在理由是"handleMessage 还没 populate state 时别发 placeholder",但 handleMessage 的 `ensurePlaceholder` 是同步阻塞且先于 `a.publish(inbound)` 执行的,Send() 拿到的 OutHeartbeat 必然已经走过 handleMessage。chain + chunk 都已就绪 —— 唯一可能 cursor<0 的场景是"OnPromptEnded 之后 daemon 重启",那种 patchChainHeader 自己有 `if chain.cursor < 0 { return nil }` 兜底,不算 placeholder-anchor 解析的责任。

**保留 v8 行为(intent unchanged)**：
- agent turn: header 仍随心跳变化 (`💭 N · 🔧 M`、可能带 `⏱ HH:MM:SS`);separator + body 渲染 —— 等价于原先"方括号 banner + chunk 内容"
- non-agent turn: 无 banner,而非 fake banner —— 比 v8 视觉更干净
- reaction / callback click: 不创建 placeholder,完全静默(同 v9 早期)

**与 §11.12.7.4 (inheritLatestHeader) 的关系**:
- §11.12.5.1 决定**Compose 时**画不画 banner —— `hasHeartbeat || entries.empty`
- §11.12.7.4 决定**chunk 出生时**带什么 (header, hasHeartbeat) —— inherit 当时的 active 状态
- 这两条互补:§11.12.7.4 让 frozen chunks 出生时持有真实 heartbeat 快照(不放 fake "🤖 Working...");§11.12.5.1 让 cold-create 但已收到 body 的 chunk 不画 stale "🤖 Working..."(frozen banner-skip)

### 11.12.6 Footer 内存语义

每 chunk **最多一个 footer**（lastFooter 刷新时同步到 footer 字段），`lastFooter == nil` → 该 chunk 上**没有 footer section**。

**lastFooter 刷新 = 数据驱动**（不是按 Kind 锁死）：
- 事件携带 status 数据（`AgentName` / `Model` / `SessionID` / `Usage` 等至少一个非零）→ `statusbar.StatusBarLines(msg) != nil` → 刷 `chain.lastFooter`
- 事件未携带 status 数据 → 返回 nil → `lastFooter` 不动
- Kind 不参与 policy 决定 —— runtime 在哪个 kind 上 stamp status 字段是 runtime 的决定，chain 代码照单全收

**runtime 当前 stamping 约定**（非强制，只是文档当前观察到的行为）：
- 通常 stamp：`OutReply` / `OutTaskCreate` / `OutTaskUpdate`（3 个进 chain 的 main-chat kind）
- 通常不 stamp：`OutThinking` / `OutToolStart` / `OutToolEnd` / `OutError` / `OutCommandReply` / `OutHeartbeat` / `OutInit`
- **v9 P2 修订**：`OutResult` 不再走 chain，但其 trailer 仍由 `sendOutResultMessage` 自身拼接（不依赖 `chain.lastFooter`）。runtime 是否 stamp OutResult 上的 statusbar 字段决定 result 消息是否带 footer 三行；语义跟 v9 P1 一致（footer-bearing = 非 nil statusbar）。

**Render 总是发生**：Telegram 不支持 append，每次 `editMessageText` 是 body 全量替换。所以即便 `lastFooter` 没变，新 `entries` 累积也要触发 Render。代码契约：
- `chain.dirty = true` 在每个 `appendSegment` 路径（cold-create / append-in-place / rotate）都置位
- `scheduleFlushDebounced` 在 `appendSegmentForKind` 返回前无条件调用
- `flushChainNow` 不检查 `lastFooter` 是否变化，只检查 `chain.dirty`

行为：
- lastFooter 刷新 → 更新 `chain.lastFooter` → 后续全量渲染带上新 footer
- lastFooter 不刷新 → `chain.lastFooter` 不动，但 Render 仍然发生（entries 累积 + footer 仍渲染上一份）
- 新 chunk 创建时如果 `chain.lastFooter != nil` → **沿用**上一 chunk 的 footer（防止 footer 突然消失）
- 重启后第一次携带 status 数据的事件之前 → 链上无 footer section（内存不持久化）

### 11.12.7 编辑频率控制（debounce + 1-Hz 自然上限）

#### 11.12.7.1 单 chunk debounce 250ms

```go
// appendSegment 调用末尾
chain.mu.Lock()
chain.dirty = true
chain.mu.Unlock()

timer := chain.debounceTimer
if timer != nil { timer.Stop() }
chain.debounceTimer = time.AfterFunc(250 * time.Millisecond, func() {
    a.flushChainNow(...)
})
```

- 250ms 内新事件来 → 重置 timer，**不发新 Telegram call**
- timer fires → `flushChainNow` 调 `editMessageText(activeChunk, render(buf))`
- burst 30 events/s 全合并成 1 edit → 实测命中数远低于 Telegram 的 1/sec/message / 20/day/message 限流（feishu §13.10 之外独有的"denounce 合并批量 update"模式）

#### 11.12.7.2 三触发器 overflow 决策

Telegram 的硬约束（不可 append + 4096 单消息上限 + 必须带 statusbar+heartbeat）决定了三种 overflow 路径必须在不同时间点走不同动作。下表是决策矩阵：

| 触发条件 | 触发时机 | 动作 | 用户语义 |
|---|---|---|---|
| **Trigger 1**:单条 entry 自身 raw > 3500 chars | `appendSegment` / `appendErrorSegment` 入口 | **SPLIT** —— 该 entry 切成 N 张 Telegram message（同时间戳、同 footer） | "我一句话太长了"—— 视觉连续的多片 |
| **Trigger 2**:当前 chunk `bufTextSize() + segment > 3500` raw | `appendSegment` case 3 / `appendErrorSegment` rotate 分支 | **ROTATE** —— 当前 chunk 锁死 + 新建 chunk 接收 entry | "我说了很多句"—— 页面跳转 |
| **Trigger 3 (safety net)**:当前 chunk 渲染后 `len(rendered) > 3900` | `flushChainNow` 内 | **ROTATE** —— `splitTelegramText` 切多片 + 多 chunk 重排 | "raw 没超但渲染膨胀"—— safety net |

**Trigger 1 SPLIT（单段超长）实现**：
```
appendSegment 入口加 pre-check:
if len(segment) > chainChunkThresholdChars (3500) {
    return splitOversizedSegmentLocked(...)
}

splitOversizedSegmentLocked:
1. RenderMarkdown(segment) 一次 (escapeHTML fallback)
2. splitTelegramText(rendered, maxTelegramTextLength=3900) → pieces[0..N-1]
3. 若当前 active chunk 存在 → markFull (冻结) —— SPLIT 意味着这一逻辑消息结束
4. 对每片 piece[i]:
   - 创建 chunkBody(messageID=0, headerLine=heartbeatText(nil))
   - appendEntryHTML(piece) // isHTML=true,跳过 RenderMarkdown (避免二次转义)
   - 若 chain.lastFooter != nil → setFooter(RenderPanel(chain.lastFooter))
   - body := Compose()
   - mid := sendFn(chatID, topicID, userMessageID, body)
   - chunk.messageID = mid
   - 若 i < N-1 → markFull (frozen)
5. chain.chunks = append(chain.chunks, newChunks...)
6. chain.cursor = len(chain.chunks) - 1 (最后一片 = 新 active)
7. chain.dirty = true
```

**关键 invariant**：
- SPLIT 路径所有 chunks 共享**同一次** `heartbeatText(nil)` 调用 → header 时间戳完全一致 → 视觉连续
- SPLIT 路径所有 chunks 共享**同一份** `chain.lastFooter`（SPLIT 期间 lastFooter 不刷新）
- 最后一片是 active chunk；后续 entry 走 `appendSegment` case 2（append-in-place）追加在最后一片
- 最后一片填满时 → 走 trigger 2 ROTATE → 新 chunk 接收
- sendFn 部分失败（发到第 k 片失败）：**chain.chunks 完全未修改**（0 个新 chunks 跟踪，return err 在 `chain.chunks = append(...)` 之前）；前 k-1 片已发到 Telegram（orphan 历史，daemon 重启后消失）；链状态 = 调用前状态；后续 appendSegment 会因旧 active chunk 已 markFull → case 2 miss → case 3 ROTATE 到新 chunk。

**Trigger 2 ROTATE（累积超长）实现**：即 `appendSegment` case 3 + `appendErrorSegment` overflow 分支。已有逻辑（markFull current + 创建新 chunk 接收 entry + lastFooter 继承 + **`newChunk.inheritLatestHeader(cur)` 拷贝 active chunk 的 (header, hasHeartbeat) 作为新 chunk 的快照，§11.12.7.4**)。

**Trigger 3 ROTATE（safety net）实现**：`flushChainNow` 内 `len(rendered) > 3900` 分支：
```
1. pieces := splitTelegramText(rendered, 3900)
2. pieces[0] → editMessageText(cur.messageID) (覆盖当前 chunk)
3. cur.markFull() + cur.freezeAfterOverflow(len(pieces[0]))
4. 对 pieces[1..N-2] 各 sendMessage 创建 frozen 中间 chunk (entries=nil, markFull)
5. pieces[N-1] → sendMessage 创建新 active chunk, appendEntry(pieces[N-1], isHTML=false),
   **inheritLatestHeader(cur)** ← 新 chunk header = active 状态的快照(§11.12.7.4)
6. chain.cursor = len(chain.chunks) - 1
```

中间 chunk (pieces[1..N-2]) 是 **marker-only**：`entries=nil` + `markFull=true` + `flushedLen=len(p)`，Compose() 输出空字符串。隐式保证：`chain.cursor` 单调 forward，永不回退到中间 chunk。如果将来有人改 cursor 回退逻辑，会触发空 Compose → `editMessageText(messageID, "")` 把 Telegram 上的非空内容抹掉 — 此 invariant 由 review 维护。

**与 v3 chain-key 修正的关系**：
- Trigger 1 / 2 / 3 都在 `appendSegment` / `flushChainNow` 内执行，传入 `chatID`/`topicID`/`userMessageID` 三个参数（§11.12.2）
- `chains.getOrCreate(chatID, topicID, userMessageID)` 返回的 chain 是这一 turn 独有的 — 跨 user msg 不串扰

#### 11.12.7.4 inheritLatestHeader 契约（v9 P1.1, 2026-08-23 晚）

**核心规则**:每条新创建的 chunk 必须是当前 active 状态的 (header, hasHeartbeat) 快照 — 不是冷 `heartbeatText(nil)` 通用 banner,也不是 chunk 自身的"创建时间"。

**实现位置**:`chunkBody.inheritLatestHeader(src *chunkBody)` 拷贝 src.header 和 src.hasHeartbeat 到 receiver。src 为 nil 时 no-op(冷场路径用)。

**调用点**:

| 路径 | 之前 | 现在 |
|---|---|---|
| `appendSegment` case 3 (ROTATE) | `newChunkBody(0, heartbeatText(nil))` | `newChunkBody(0, ""); newChunk.inheritLatestHeader(cur)` |
| `appendSegmentLocked` case 3 (OutToolStart ROTATE) | 同上 | 同上 |
| `appendErrorSegment` overflow | `newChunkBody(0, cur.headerText())`(只 copy header) | `newChunkBody(0, ""); newChunk.inheritLatestHeader(cur)`(header + hasHeartbeat) |
| `splitOversizedSegmentLocked` pieces | 全部 `heartbeatText(nil)` | 全部 `inheritLatestHeader(inheritFrom)`;inheritFrom 在 markFull 之前 capture |
| `splitOversizedErrorSegmentLocked` pieces | 同上 | 同上 |
| `flushChainNow` tail (trigger 3 last piece) | `newChunkBody(int64(mid), heartbeatText(nil))` | `newChunkBody(int64(mid), ""); newChunk.inheritLatestHeader(cur)` |

**Cold-create 路径**(chain.cursor<0 = 该 turn 第一条 chunk)保持 `heartbeatText(nil)`:没有 source 可 inherit,用冷-create banner。

**为什么不是 commit `a654fc3` 的"每条 message 的 header 反映创建时间"**:
- 用户明确指出应该"完全继承最新的 HeatbeatHeadline" —— chain 的所有 chunk 在每一刻共享同一个 headline
- 但 patchChainHeader 只更新 active cursor 的 chunk(避免 N 倍 editMessageText 风暴)
- 所以走中间路线:每个 chunk 出生时 adopt 当时的 active 状态,然后冻结;后续 patchChainHeartbeat 只动 cursor,但 frozen chunks 读出来仍然有意义 —— 用户看到的是一组"快照"序列,可以从 banner 时序读出 agent 思考/工具推进的节奏(冷 banner → 💭 N → 💭 N+1 → ...)
- 关闭 commit `aad7705` rationale:"每条 message 的 header 反映创建时间,而不是继承 active chunk 的状态" —— 错误决策,已 supersede

**滚动 timeline 实例**(agent: 5 think, 2 tool, 5 more think, error):

```
[💭 5 · 🔧 2 · ⏱ 10:01:00]  long thinking text 1...      ← chunk A (frozen)
[💭 5 · 🔧 2 · ⏱ 10:01:00]  long thinking text 2...      ← chunk B (frozen, SPLIT)
[💭 5 · 🔧 2 · ⏱ 10:01:00]  more thinking...            ← chunk C (frozen)
[💭 10 · 🔧 2 · ⏱ 10:01:30] (active, OutHeartbeat 后)    ← chunk D (active)
[❌ tool failed: out of disk]                             ← chunk E (active, post-error)
[💭 10 · 🔧 2 · ⏱ 10:01:30] 🎉                          ← chunk D 终态 (active)
```

关键观察:A/B/C 三块都冻结在"💭 5 · 🔧 2" 时刻(它们创建于 OutHeartbeat 之后但还没来下一个 heartbeat)—— 用户能数出来 "agent 想过 5 次,用过 2 个工具,然后接着想了 5 次,挂了"。

**PatchChainHeader 仍只 PATCH active**:
- patchChainHeader 维持"只更新 cursor chunk"的语义,不在所有 frozen 上广播 —— broadcast 会引发 N 倍 editMessageText,违反 §11.12.7.1 debounce budget 且破坏 chunk 时间线语义
- 用户读 chat 时,frozen chunks 永远在它们各自的"冻结时刻"snapshot;active chunk 持续 PATCH 到最新
- 这跟 §11.12.5.1 Compose header-skip rule 互补:Compose 让"body+no-heartbeat"不画 banner(防 stale 冻屏);inheritLatestHeader 让"body+with-heartbeat"画正确的 banner(防 cold-create 假 alive)

### 11.12.8 OutHeartbeat 路径

```go
case messages.OutHeartbeat:
    // v9 P1 (2026-08-23): the eager ensurePlaceholder in
    // handleMessage guarantees the chain + chunk exists before
    // any Out* lands, so there is no race to guard against.
    // patchChainHeader still defensively returns nil when
    // chain.cursor < 0 (a transient / purged state) — that is
    // its own correctness gate, not a placeholder-anchor
    // resolution step. OutMessageState's 👌 reaction still
    // announces the turn if a heartbeat ever gets silently
    // dropped.
    return a.patchChainHeader(rawChatID, topicID, replyAnchor, msg)
```

`patchChainHeader` 内部：

```go
if msg.Heartbeat != nil {
    chain.chunks[chain.cursor].setHeaderFromHeartbeat(heartbeatText(msg.Heartbeat))
} else {
    // defensive: gateway always populates msg.Heartbeat, but if
    // a future caller forgets we keep hasHeartbeat=false so the
    // banner-hide rule (§11.12.5.1) still applies
    chain.chunks[chain.cursor].setHeader(heartbeatText(nil))
}
chain.dirty = true
scheduleFlushDebounced(chain, ...)
```

要点：

- **真分支用 `setHeaderFromHeartbeat`** —— 同时翻 `hasHeartbeat=true`,否则 §11.12.5.1 会一直 hide 掉 header
- 只 PATCH active chunk 的 `header`，**不动 entries** 和 lastFooter（lastFooter 是数据驱动刷新，详见 §11.12.6）
- header 是预烘焙 HTML（`<b>...</b>` 是字面量，不是 RenderMarkdown 产物）—— Compose() 走"header verbatim, entries RenderMarkdown, footer verbatim"三路分发，避免 `<b>` 被二次转义成 `&lt;b&gt;`
- 其他 frozen chunks 永远不动
- (v9 P1 移除) `placeholderAnchor` / `ensurePlaceholderForHeartbeat` —— handleMessage 已经 eager 建好,OutHeartbeat 不需要再二次确认
- (v9 P1 移除) race-window guard —— handleMessage 是同步阻塞 + 先于 `a.publish(inbound)` 执行,race 不存在
- `getOrCreateChain` 的第三个参数 `userMessageID` 见 §11.12.2 —— 锁死 back-to-back user msg 不串扰

### 11.12.9 OnPromptEnded 路径

```go
// adapter.go:OnPromptEnded
func (a *Adapter) OnPromptEnded(ctx context.Context, chatID, userMsgID string) {
    ...
    parsedUserMsgID := atoiUserMsgID(userMsgID)
    chain := a.chains.getOrCreate(rawChatID, topicID, parsedUserMsgID)

    // P1 #2 fix (2026-08-23): take chain.mu for the full
    // flush → stamp → purge sequence so an in-flight
    // scheduleFlushDebounced from a concurrent Send can't arm a
    // timer that fires AFTER we purge the chain (orphan timer
    // editing the previous turn). We also cancel any currently-
    // pending timer under the same lock so its stop/release
    // ordering is unambiguous.
    chain.mu.Lock()
    if chain.cursor < 0 {
        stopDebounceTimer(chain)
        chain.mu.Unlock()
        a.chains.purge(rawChatID, topicID, parsedUserMsgID)
        return
    }
    stopDebounceTimer(chain)
    chain.mu.Unlock()

    // Best-effort flush before stamping the terminal reaction.
    if err := flushChainNow(ctx, chain, rawChatID, topicID, parsedUserMsgID,
        a.chainEditFn(), a.chainSendFn()); err != nil { /* log warn */ }

    chain.mu.Lock()
    // v9 P2: 🎉 anchor prefers the standalone result message (if any
    // OutResult landed this turn) over the active chunk. resultMessageID
    // is zero for error-only / tool-only / slash-only turns → fall
    // back to the active chunk to preserve v9 P1 behavior. Picking
    // the result message ties the "completed" visual directly to the
    // user-facing output instead of the last activity segment.
    var (
        targetID    int64
        cur         *chunkBody
    )
    if chain.cursor >= 0 {
        cur = chain.chunks[chain.cursor]
    }
    targetID = chain.resultMessageID
    if targetID == 0 && cur != nil {
        targetID = cur.messageID
    }
    chain.mu.Unlock()

    if targetID != 0 {
        // 🎉 reaction. v6.3 single-reaction budget on the USER MSG
        // slot is preserved (this lands on result message or active
        // chunk, not the user's original message). emoji is in the
        // official ReactionTypeEmoji whitelist (✅ U+2705 was rejected
        // by Telegram API).
        _ = a.setMessageReactions(ctx, rawChatID, int(targetID),
            []map[string]any{{"type": "emoji", "emoji": "\U0001F389"}})
    }

    // Turn-end cleanup: forget the in-memory chain. Frozen chunks
    // remain in chat as historical evidence (no edit touches them
    // again). Next user message re-materialises a fresh chain via
    // ensurePlaceholder.
    a.chains.purge(rawChatID, topicID, parsedUserMsgID)
}
```

**关键 invariant**:
- `stopDebounceTimer` 在 flush 前调用 (P1 #2 fix)，否则 orphan timer 会在 purge 后触发，ghost-edit 上一 turn 的 chunk messageID (违反 §11.12.9)。
- 锁覆盖范围 `stopDebounceTimer → flushChainNow → 🎉 stamp` 整段，使 in-flight `scheduleFlushDebounced` from concurrent Send 不能 arm 一个在 purge 之后 fire 的 timer。
- **v9 P2 🎉 anchor 选择**：先看 `chain.resultMessageID`（`Send(OutResult)` 在 `sendOutResultMessage` 里赋值），零值回退 active chunk。多次 OutResult 取最后一个 messageID（"last wins" 语义跟 v9 P1 的 active chunk 语义对齐）。
- 🎉 emoji 用 `\U0001F389` (U+1F389 PARTY POPPER)，不在 Telegram API 黑名单里 (U+2705 ✅ 之前被 REACTIONS_TOO_MANY 拒过)。
- 终态: `a.chains.purge` —— chain 对象从 LRU 移除 (key 含 userMessageID 所以只 purge 这一 turn 的 chain，不影响其他 chain)。

### 11.12.10 重启后的 chain loss 行为

**重启前**：chat 里看到 P1/P2/P3（frozen，各自带终态 footer）
**重启后**：`chains` map 空
- 下次 `OutX`：`cursor=-1` → "建第一张"路径 → `sendMessage` 新 P_new 钉 userMsgID
- 用户视觉：chat 里出现 P_new 跟在所有老 frozen P 后面，**老 P 不再被编辑**

**取舍**：chain 不持久化避免 LRU 复杂度上升 + state schema 不变；接受"重启 = chain 重启"语义。与 §11.11 v8 的"`PlaceholderMessageID` 持久化但不含 buf"取舍同源。

### 11.12.11 Topic vs DM

| 行为 | topic (thread_id > 0) | DM (thread_id == 0) |
|---|---|---|
| chain 创建 | OutX/OutHeartbeat 来时 | 同 |
| chunk `sendMessage` 带 `message_thread_id` | ✅ | ✗ |
| chunk `reply_to_message_id` | `userMessageID` | `userMessageID` |
| frozen chunk 处理 | 保持 frozen，不动 | 同 |
| turn-end 清 cursor + chunks + lastFooter | ✅ | ✅ |
| `OnPromptEnded` 🎉 on result message (fallback to active chunk) | ✅ | ✅ |

### 11.12.12 跟飞书 receipt 语义对位（v9）

| 维度 | Feishu receipt | Telegram v9 P1 | Telegram v9 P2 |
|---|---|---|---|
| Surface | 单一 receipt Card 2.0，PATCH 复用 | chain of N chunks，editMessageText 复用 active | chain of N chunks（中间产物）+ 1 张独立 result 消息 |
| 状态 ticker | header PATCH（card body） | active chunk headerLine PATCH | active chunk headerLine PATCH（不变） |
| OutThinking | append div 进 card body | append segment 进 active chunk buffer | append segment 进 active chunk buffer（不变） |
| OutToolStart/End | 合并 thread reply（Start+End merge，F-38） | 同 chunk buffer 内两 segment | 同 chunk buffer 内两 segment（不变） |
| OutReply | 独立 reply（F-44 后） | append segment 进 active chunk buffer | append segment 进 active chunk buffer（不变） |
| OutResult | 独立 reply（F-39 后） | append segment（📝 prefix）进 active chunk | **独立 sendMessage + reply_to_message_id=userMsgID** |
| user message 状态 | AddReaction（append-only 多 emoji 堆叠） | setMessageReaction（单 emoji 槽） | setMessageReaction（单 emoji 槽，不变） |
| placeholder / 终态 | ✅ header SetPromptState | 🎉 setMessageReactions on active chunk | 🎉 setMessageReactions on **result message** (fallback to active chunk) |
| 终态 | ✅ reaction + card header ✅ | user msg 留 👌 不动；active chunk 贴 🎉 | user msg 留 👌 不动；result message 贴 🎉（无 result 时回退 active chunk） |
| Footer | card `<hr>` + 灰色 markdown（3 行） | chunk 末尾 renderPanel(lastFooter)（仅 active chunk） | chunk 末尾 renderPanel(lastFooter) + **result 消息末尾 renderPanel(lastFooter)** |
| Persist | receipt state 进 MemoryStore | **chain 不持久化**（重启失） | **chain 不持久化**（重启失；resultMessageID 跟随 chain 失忆） |
| 4096 / 30KB 限制 | 30KB body + 50 elements | 4096 chars ×N chunks（debounce 内合并） | 4096 chars ×N chunks + **4096 chars ×M result messages**（split 切分） |
| OutToolStart dump | summarize_tool.go（call + result 双行） | 复刻 feishu 同款（emoji 风格统一） | 复刻 feishu 同款（emoji 风格统一，不变） |

v9 P2 把 OutResult 对齐到飞书 F-39 决策 —— **独立 reply 投递**而非 inline 进 receipt。Telegram 之前用 chain fold 是因为 Telegram 没有飞书式 dedup bug 可以避免，**但**牺牲了"result 跟中间产物视觉差异"这一 UX 信号。P2 修复这个 UX：result 独立 + 🎉 锚定。

### 11.12.13 长文本与 Markdown 渲染

#### OutResult 独立 reply（v9 P2 修订）

**v9 P2 起**：OutResult 改独立 `sendMessage(reply_to_message_id=userMsgID)`，对齐飞书 F-39 决策（独立 reply 投递，避免跟中间产物视觉同质）。

长 result 处理：单条 OutResult body + StatusBar trailer 长度 > 3900 chars → `splitTelegramText` 切成多片，每片单独 `sendMessage`，都带 `reply_to_message_id=userMsgID`（视觉上是 user msg 下的 reply 簇）。只有最后一片的 messageID 进 `chain.resultMessageID`（OnPromptEnded 🎉 锚点）。

#### Markdown 渲染

- `parse_mode=HTML` 走现有 `RenderMarkdown`，不切 MarkdownV2（escape 脆弱，参考 feishu.md §13.19）
- **三层渲染原语（v9 P3 落地，§11.12.19）**：
  - `renderMarkdownSafe(s string) string`（`render.go`，unexported）—— **唯一** 一处跑 `RenderMarkdown` + `escapeHTML` fallback + 空串 short-circuit。所有"raw markdown → safe HTML"的入口都走它，不在每个 call site 重复 try-render-or-escape 模式。
  - `RenderForWire(raw string) string`（`render.go`，exported）—— wire-facing block 入口。`sendOutResultMessage` 走它把 `msg.Text` 转成 HTML 再串 trailer；trailer 本身 (`statusbar.RenderPanel` 输出 `┌──› / └──›` 边框) 是手工构造的 safe HTML，**不再二次渲染**(否则 box-drawing 字符会被 escape)。
  - `chunkBody.Compose()` —— chain 消息入口，per-entry loop + `isHTML` flag 路由（`appendEntryHTML` 走 verbatim，跳过 `RenderMarkdown` 避免二次转义）。Compose 不套 `RenderForWire`，因为 `RenderForWire` 是 block-level 包装，会跟 per-entry `isHTML` 路由冲突。
- **两个 markdown block 入口覆盖两条路径，不算重复**：`renderMarkdownSafe` 是原语（被 `RenderForWire` 和 `Compose` 共享），`RenderForWire` 是 standalone block 入口，`Compose` 是 chain chunk 入口。三层各管一摊：原语 / block 包装 / chunk 组合。
- Feishu §13.17 / §13.19 同款 sanitize pipeline(非 HTTP URL → plain、fence newline、image strip、heading demotion)**只注入 `renderMarkdownSafe` 一次** —— 这就是它单独抽出来的最大动机：未来加 sanitize 只改原语一处，`RenderForWire` 和 `Compose` 自动继承，grep 不用扫整个 adapter。

### 11.12.14 summarize_tool 复用（同款）

新文件 `internal/channel/telegram/summarize_tool.go`，从 feishu 平移：

```go
package telegram

import (
    "fmt"
    "path/filepath"
    "strings"
)

const toolCallArgsMaxBytes = 100  // args 字节上限

// formatToolStartCall produces the "call" line for chain entry,
// matching Claude Code's terminal UX:
//   `● Bash(go build ./... 2>&1; echo "EXIT=$?")`
//   `● Read(/tmp/foo.go)`
func formatToolStartCall(name, args string) string {
    if args == "" { return "● " + name }
    return "● " + name + "(" + displayToolArgs(args) + ")"
}

func displayToolArgs(args string) string {
    if compact := compactJSONToolArgs(args); compact != "" { return compact }
    return truncate(args, toolCallArgsMaxBytes)
}

// summarizeToolResult produces the "result" line:
//   `⎿  📄 Read → 47 lines`
//   `⎿  ❌ Bash failed: exit code 1`
func summarizeToolResult(name, output string, err error) string {
    if err != nil {
        return fmt.Sprintf("⎿  ❌ %s failed: %s", name, err.Error())
    }
    switch strings.ToLower(name) {
    case "read":       return "⎿  📄 Read → " + itoa(countLines(output)) + " lines"
    case "write":      return "⎿  📝 Write → " + itoa(len(output)) + " bytes"
    case "edit", "multiedit": return "⎿  ✏️  applied"
    case "bash":       return "⎿  💻 Bash → " + itoa(countLines(output)) + " lines"
    case "grep":       return "⎿  🔍 Grep → " + itoa(countLines(output)) +
                              " matches across " + itoa(countUniqueFiles(output)) + " files"
    case "glob":       return "⎿  📂 Glob → " + itoa(countLines(output)) + " files"
    case "webfetch":   return "⎿  🌐 WebFetch → " + itoa(len(output)) + " chars fetched"
    case "websearch":  return "⎿  🔎 WebSearch → " + itoa(countLines(output)) + " results"
    default:           return "⎿  🔧 " + name + " → " + itoa(len(output)) + " bytes"
    }
}

// 共用 helpers: countLines, countUniqueFiles, truncate, compactJSONToolArgs
// 跟 feishu summarize_tool.go 一致; 想 100% reuse 也可以提一个 internal/summarizetool package
```

### 11.12.15 实施清单（commit 顺序）

实际 commit 顺序见 git log（`git log --oneline 84511ca..HEAD` on `fix-telegram-rolling-log` branch）。以下按主题分组列出关键 commit（哈希可能因后续 fix 而变化）：

**v9 骨架**:
- `[telegram] port summarize_tool from feishu` — 新增 `summarize_tool.go`
- `[telegram] add placeholder_chain skeleton` — `placeholder_chain.go` + chainLRU

**v9 Send 重写**:
- `[telegram] rewrite formatTool → call helpers` — `formatTool` → `formatToolStartCall` + `summarizeToolResult`
- `[telegram] rewrite Send: 8 Out* → appendSegment` — 8 个 kind 进 chain
- `[telegram] rewrite OutHeartbeat → patchActiveHeader + debounce`
- `[telegram] rewrite OnPromptEnded → flushChain + 🎉 + purge`

**v9 测试 + 修**:
- `[telegram] tests: placeholder_chain_test.go`
- `[telegram] tests: chain_integration_test.go`
- v9 chain 关键 fixes (P0 #1-3, P1 #1-2, P2 #1-2)

**v9 后续打磨**:
- `[telegram] chain: key LRU by userMessageID` (commit `a654fc3`) — back-to-back user msg race condition fix
- `[telegram] chain: §11.12.7.2 SPLIT path for single oversized segments` (commit `aad7705`) — trigger 1 SPLIT 落地
- `[telegram] chain: cleanup + footer regression tests + race fix` (commit `2e4fb85`)
- `[telegram] chain: regression tests for chain-key-by-userMessageID` (commit `614922e`)

**v9 P1 (2026-08-23) banner-hide 修复**:
- `[telegram] chunkBody: hasHeartbeat + Compose header-skip rule` —— 加 `hasHeartbeat bool` 字段、`setHeaderFromHeartbeat` 方法、`Compose` renderHeader 决策(§11.12.5.1)
- `[telegram] patchChainHeader: setHeaderFromHeartbeat` —— 真分支翻 `hasHeartbeat`,cold-create / 兜底分支保持 `setHeader`
- `[telegram] Send: drop ensurePlaceholderForHeartbeat + placeholderAnchor` —— Send 入口不再 lazy resolve;OutHeartbeat case 简化成无条件 `patchChainHeader`
- `[telegram] remove ensurePlaceholderForHeartbeat method + 5 tests` —— 移除懒汉路径;`TestAdapter_EnsurePlaceholderForHeartbeat_*` 和 `TestAdapter_Send_OutHeartbeat_DeferWhenNoUserMsgID` 删除
- `[telegram] tests: Compose header-skip + banner-hide e2e` —— 4 个新 Compose unit test + 1 个 banner-hide 集成测
- `[telegram] fix(docs): §11.11 / §11.12 sync` —— v9 P1 变更同步到 spec

**v9 P1.1 (2026-08-23 晚) — inheritLatestHeader 翻转 ROTATE/SPLIT rationale**:
- 推翻 commit `a654fc3` 的"ROTATE 用 heartbeatText(nil) 不是 cur.headerText()"决策 —— 正确语义是「每条 message 的 header 完全继承最新的 HeatbeatHeadline」,而不是"反映创建时间"
- `[telegram] chunkBody: inheritLatestHeader(src)` ——  新增方法,拷贝 src 的 (header, hasHeartbeat) 对;nil src 是 no-op(cold-create 路径用)
- `[telegram] placeholder_chain_flush: case 3 / appendErrorSegment overflow / SPLIT pieces / flushChainNow tail` —— 6 处全部从 `newChunkBody(0, heartbeatText(nil))` / `newChunkBody(int64(mid), heartbeatText(nil))` / `inheritedHeader := cur.headerText()` 改成 `newChunkBody(... , "")` 后 `inheritLatestHeader(cur)`。Cold-create 路径(chain.cursor<0 时)保留 heartbeatText(nil)
- `[telegram] tests: TestChain_RotateChunk_HeaderIsFreshNotInherited → TestChain_RotateChunk_InheritsLatestHeader` —— 单测翻转:ROTATE 现在必须 inherit,与 `TestChain_FrozenChunkHeaderSurvivesAcrossSubsequentPatch` 一起锁住 "frozen chunks keep snapshot / cursor's chunk updates" 的双轨语义
- `[telegram] tests: 4 new inheritLatestHeader tests` —— primitive 层 + 3 个集成层(rotate / split / flush tail)

每 commit 必跑：
- `go test ./internal/channel/telegram/`
- `go test -race ./internal/channel/telegram/` (commit `2e4fb85` 后干净)
- `golangci-lint`
- Telegram 实机 dotest（项目通常用 `cmd/probe-telegram` 或类似）

### 11.12.16 验收 / 测试矩阵

实际测试名（`internal/channel/telegram/` 下）：

**chain primitive 单测** (`placeholder_chain_test.go`):
| 测试 | 验证 |
|---|---|
| `TestChainLRU_EvictOldestOnCap` | 1001 chain 创建 → 最早 evict |
| `TestChainLRU_PurgeRemovesKey` | `purge` 后 key 移除 |
| `TestChainLRU_ResetClearsAll` | `reset` 清空 |
| `TestAppendSegment_CreatesFirstChunkWhenEmpty` | OutReply 在空 chain → 一张 chunk with single segment |
| `TestAppendSegment_AppendsToActiveChunkWithinThreshold` | 10 events ≤ 3500 chars → 都进同一 chunk |
| `TestAppendSegment_OverflowCreatesSecondChunk` | 累计 > 3500 → chunk 1 锁，chunk 2 新建 |
| `TestAppendSegment_FooterRefreshOnlyOnFooterBearing` | 非 footer-bearing 不动 lastFooter；footer-bearing 刷新 |
| `TestFlushChainNow_NoOpWhenClean` | `dirty=false` 时不调 editFn |
| `TestFlushChainNow_RendersHeaderBufFooter` | header / buf / footer 渲染 |
| `TestScheduleFlushDebounced_MergesBurst` | 250ms 内多次调用合并成 1 edit |
| `TestRenderActiveChunkBody_HeaderOnly` | 无 entries → 无 separator;cold-create header 仍渲染 |
| `TestRenderActiveChunkBody_SkipsHeaderWhenBodyButNoHeartbeat` (v9 P1 §11.12.5.1) | entries>0 + hasHeartbeat=false → 头被 hide |
| `TestRenderActiveChunkBody_HeaderAndBody` (v9 P1 §11.12.5.1) | entries>0 + hasHeartbeat=true → 头回来 + 分隔线 + body |
| `TestRenderActiveChunkBody_HeaderOnlyAfterHeartbeat` (v9 P1 §11.12.5.1) | entries 空 + hasHeartbeat=true → 头渲染(无 entries 所以无分隔)|

**新增回归测试**（commit 3 / 4 / 5 后）:
| 测试 | 验证 |
|---|---|
| `TestChain_RotateChunk_InheritsLatestHeader` (v9 P1.1) | ROTATE tail header inherit cur 的 (header, hasHeartbeat) —— §11.12.7.4 |
| `TestChain_RenderAlwaysHappen_EvenWhenLastFooterUnchanged` | 非 footer-bearing event → lastFooter 不动，但 dirty=true 触发 Render |
| `TestChain_DataDrivenFooter_OutThinkingWithAgentName_RefreshesFooter` | footer policy 数据驱动，Kind 不锁 |
| `TestChain_NewChunk_InheritsLastFooter` | overflow 时新 chunk 沿用 lastFooter |
| `TestChain_MultipleOverflow_ThreeChunks_FirstTwoFrozen` | 3 chunks 后，frozen 1/2 再发 events 不动 |
| `TestChain_OversizedSegment_SplitsIntoMultipleChunks` | SPLIT trigger 1: len(segment) > 3500 → 多 chunks |
| `TestChain_SplitChunks_AllCarrySameTimestamp` | SPLIT chunks 共享同一 heartbeatText(nil) 调用 |
| `TestChain_SplitChunks_FirstPiecesAreFrozen` | pieces 1..N-1 markFull, 最后一片 active |
| `TestChain_SplitChunks_SubsequentEntryLandsOnLastPiece` | SPLIT 后续 entry 落到最后一片 |
| `TestChain_OversizedError_SplitsIntoMultipleChunks` | OutError SPLIT |
| `TestChain_RotateAndSplitDistinguishedByHeader` | ROTATE/SPLIT 都 inherit cur —— 时间戳一致是设计预期(同源),不是 bug。Test 用 shared-header log 而非 fail |
| `TestChain_BackToBackUserMessages_AreSeparateChains` | chain-key-by-userMessageID 隔离 |
| `TestChain_DelayedOutReply_AfterNewUserMsg_DoesNotLeak` | 迟滞 OutReply 不串扰下一 turn |
| `TestChain_Heartbeat_DoesNotCrossUserMessageBoundary` | heartbeat PATCH 不跨 turn |

**adapter integration 测试** (`chain_integration_test.go`):
| 测试 | 验证 |
|---|---|
| `TestAdapter_Send_OutReply_FoldsIntoChain` | OutReply 不发独立 bubble，进 active chunk |
| `TestAdapter_Send_MultipleBurst_CoalesceIntoOneEdit` | burst 合并成 1 editMessageText |
| `TestAdapter_OutHeartbeat_PATCHesActiveChunkHeader` | heartbeat → chunk.headerLine 更新 + debounce flush |
| `TestAdapter_OnPromptEnded_DM_RendersOnActiveChunkThenPurges` | 🎉 贴在 active chunk + chain purge（v9 P1 行为,无 OutResult 时回退路径） |
| `TestAdapter_Send_OutError_FoldsIntoChainWithMarkdownFragment` | OutError 进 chain，stderr ```fences``` 渲染 |
| `TestChainAppendOnly_AfterStopFreshChain` | daemon restart → next event → 新 chain |
| `TestChain_HeartbeatBoldHeaderPreservedThroughFlush` | `<b>` 不被二次转义 |
| `TestChain_OutErrorStderrTailRendersAsPreBlock` | stderr 渲染成 `<pre>` |
| `TestChainOverflow_RotatesToNewChain` | ROTATE path (3500 raw overflow) |
| `TestChainOverflow_TailHasNonEmptyEntries` | P0 #2 lock-in: tail chunk 保留 long-text content |

**summarize_tool 测试** (`summarize_tool_test.go`):
- `TestSummarizeToolResult_ClaudeStyle` (11 sub-tests: read / write / edit / multiedit / bash / grep / glob / webfetch / websearch / unknown / err)
- `TestSummarizeToolLegCompat_FormatsMatchFeishu`

**adapter_statusbar 测试** (`adapter_statusbar_test.go`): 15 个 StatusBar trailer 测试，验证 §18 contract (每个 text-emitting kind 都带 StatusBar trailer, 包括 `TestAdapter_Send_DM_OutCommandReply_AppendsStatusBar`)。

**v9 P1 banner-hide 测试**:
- `TestRenderActiveChunkBody_SkipsHeaderWhenBodyButNoHeartbeat` (placeholder_chain_test.go) — §11.12.5.1 主规则
- `TestRenderActiveChunkBody_HeaderAndBody` (同上) — hasHeartbeat 后 separator 回来
- `TestRenderActiveChunkBody_HeaderOnlyAfterHeartbeat` (同上) — 早 heartbeat 早独立头部
- `TestAdapter_Send_DM_OutReply_NoFieldsNoCache_NoTrailer` 翻转 (adapter_statusbar_test.go) — body+no-heartbeat→无 `🤖` banner(原 v8 假设 "banner unconditional" 现在反过来)
- (v9 P1 移除) `TestAdapter_EnsurePlaceholderForHeartbeat_CreatesWhenMissing` / `_ReusesExisting` / `_DMCreates` / `_DeferWhenNoUserMsgID` / `TestAdapter_Send_OutHeartbeat_DeferWhenNoUserMsgID` — 懒汉路径不再存在

**v9 P1.1 inheritLatestHeader 测试**(§11.12.7.4):
- `TestChunkBody_InheritLatestHeader_HeaderAndFlag` —— primitive: 拷贝 header + hasHeartbeat,nil src no-op
- `TestChain_SplitOversizedSegment_AllPiecesInheritLatestHeader` —— SPLIT trigger 1:每块都 inherit
- `TestChain_AppendErrorSegment_OverflowInheritsLatestHeader` —— OutError overflow ROTATE: 新 chunk inherit
- `TestChain_FlushChainNow_TailInheritsLatestHeader` —— Trigger 3 tail piece: inherit
- `TestChain_RotateChunk_InheritsLatestHeader` —— (替换 `TestChain_RotateChunk_HeaderIsFreshNotInherited`) case 3 ROTATE: 翻转单测契约

**v9 P2 OutResult 独立消息测试**(§11.12.4.1 + §11.12.9):
- `TestAdapter_Send_OutResult_SendsStandaloneReply` —— OutResult 走独立 `sendMessage`，`reply_to_message_id=userMsgID`，body 含 result text + StatusBar 分隔线 + 三行 footer；**chain.chunks buffer entries count 不增加**
- `TestAdapter_Send_OutResult_DoesNotChangeActiveChunkText` —— OutResult 前后两次读 lastChunkText 完全一致（chain 文本不污染）
- `TestAdapter_Send_OutResult_LongText_SplitsAcrossMultipleMessages` —— body + trailer > 3900 → N 次 sendMessage，每片都带 reply_to_message_id=userMsgID；只有**最后一片**的 messageID 进 `chain.resultMessageID`
- `TestAdapter_Send_OutResult_MultipleInOneTurn_LastWins` —— 两条 OutResult → `chain.resultMessageID` = 第二条 messageID
- `TestAdapter_OnPromptEnded_DM_StampsOnResultMessage` —— OutResult + OnPromptEnded → `setMessageReaction` 命中 resultMessageID，**不**命中 active chunk messageID（v6.3 single-reaction 预算仍守：user msg 不动）
- `TestAdapter_OnPromptEnded_NoOutResult_FallsBackToActiveChunk` —— 无 OutResult → 回退到 active chunk 兜底（保住 error-only / tool-only / slash-only turn 行为）
- `TestAdapter_Send_OutResult_EmptyText_SilentDrop` —— 已有 `TestAdapter_Send_OutResultEmptyText`（adapter_test.go）继续守 empty-text silent drop 路径
- `TestAdapter_Send_DM_OutResult_StandaloneMessageWithStatusBar` —— 翻写 `TestAdapter_Send_DM_OutResult_AppendsStatusBar`（adapter_statusbar_test.go）：从读 lastChunkText 改成直接读 sendMessage params["text"]，断言 result body + trailer 三行

**v9 P3 渲染 DRY + blank-chunk 测试**(§11.12.19):

数据类单元测试（`chunk_body_test.go` 或 `placeholder_chain_test.go`）：
| 测试 | 验证 |
|---|---|
| `TestChunkBody_HasVisibleEntries_Empty` | entries=nil → false |
| `TestChunkBody_HasVisibleEntries_WhitespaceOnly` | entries=`[" "`, `"\n"`, `"\t\n"]` → false |
| `TestChunkBody_HasVisibleEntries_MixedHasReal` | entries=`[""`, `"real"]` → true（任一非空白即 true） |
| `TestChunkBody_HasVisibleEntries_FooterDoesNotCount` | entries=`[""]` + footer=StatusBar + header=banner → false（**这就是 bug fix 的回归点**） |

协调器单元测试（`placeholder_chain_test.go`）：
| 测试 | 验证 |
|---|---|
| `TestMaterializeChunk_DropsBlankChunk` | chunk 全空白 → `(false, nil)`，`sendFn` 未调用，`chain.chunks` 未变 |
| `TestMaterializeChunk_SendsVisibleChunk` | chunk 有 entries + footer → `(true, nil)`，`messageID` 写回，`chain.chunks` 增 1 |
| `TestMaterializeChunk_PartialFooter_StillBlank` | entries=`["\n"]` + footer=panel → `(false, nil)`（footer 不救活空白） |
| `TestMaterializeChunk_SendFnErrorPropagates` | sendFn 返 error → `(false, err)`，`chain.chunks` 未变 |

端到端回归（`placeholder_chain_test.go` + `chain_integration_test.go`）：
| 测试 | 验证 |
|---|---|
| `TestAppendSegment_WhitespaceSegment_NoNewChunk` | chain 已有 1 chunk，append 空白 segment → 不进 ROTATE mint 新 chunk；chain.chunks 长度不变 |
| `TestAppendSegmentLocked_WhitespaceSegment_NoNewChunk` | OutToolStart ROTATE 路径同样不 mint |
| `TestAppendErrorSegment_WhitespaceError_NoNewChunk` | OutError 全空白路径同样不 mint |
| `TestSplitOversizedSegment_BlankPiece_NoSendMessage` | SPLIT 产出的 piece 全空白 → 该 piece 不 sendFn |
| `TestFlushChainNow_OverflowPieces_DropsBlank` | trigger 3 safety net splitTelegramText 产出空白 piece → 不 sendFn |

`renderMarkdownSafe` 共享原语测试（`render_test.go`）：
| 测试 | 验证 |
|---|---|
| `TestRenderMarkdownSafe_EmptyReturnsEmpty` | `""` → `""`（short-circuit） |
| `TestRenderMarkdownSafe_BoldPassesThrough` | `"**bold**"` → `"<b>bold</b>"`（走 RenderMarkdown） |
| `TestRenderMarkdownSafe_FenceRendersAsPre` | ` ```\ncode\n``` ` → `<pre>code\n</pre>` |
| `TestRenderMarkdownSafe_RawHTMLEscapes` | `"<script>"` → `"&lt;script&gt;"` |
| `TestRenderMarkdownSafe_PreservesFallbackContract` | RenderMarkdown 返 error 时退到 `escapeHTML`（mock 验证） |

`appendTrailerToBody` 测试（`render_test.go`）：
| 测试 | 验证 |
|---|---|
| `TestAppendTrailerToBody_NoFooter` | `footerLines=nil` → body 原样返回 |
| `TestAppendTrailerToBody_WithFooter` | body + footerLines → body + `\n\n` + RenderPanel(footerLines) |
| `TestAppendTrailerToBody_PanelBoxDrawingPreserved` | panel 里的 `┌──›` / `└──›` 不被二次 escape |

### 11.12.17 已知限制

| Limit | 描述 | 缓解 / 后续 |
|---|---|---|
| 单 chunk 渲染后超 3900 | `flushChainNow` 走 trigger 3 safety-net ROTATE，多 message 在 chat 里不连号 | 接受；极少触发（Tool summarize 保证 segment 短小）|
| 单条 entry 自身超 3500 raw | SPLIT path (§11.12.7.2 trigger 1) 切成 N 张 Telegram message | 接受；同时间戳视觉连续 |
| `splitTelegramText` 行内硬切可能落 HTML tag 中间 | 切到 `<a href="...` 等会被 Telegram 当字面文本 | 接受；罕见（markdown 渲染后单行超 3900 的概率极低） |
| 重启后老 chunk 不被编辑 | chain 不持久化，frozen 自然冷冻 | 接受；视觉一致（old frozen 本就是历史）|
| LRU cap = 1000 | **per-user-message**（key 含 userMessageID）：1001 个 user 消息同时活跃 → 最早 chain evict | 可调；1MB 内存上限 |
| chain.cursor / chunks / lastFooter 不持久化 | daemon 重启 = chain 重建 | 接受（与 §11.11 v8 取舍同源）|
| 没用 Telegram Premium 付费能力 | 长 message 默认 fold 等 | backlog |
| `sendMessage` 同一 chat 串行速率 | agent turn 短时间内 burst 占位新建 chunk → 5 QPS per-chat 有封顶 | debounce 已经合并 hot path；overflow chunk 是冷路径，300-500ms 间隔足够 |
| SPLIT partial-failure | sendFn 第 k 片失败时前 k-1 片 Telegram orphan 历史 | 接受；daemon 重启后消失；后续 appendSegment 走 case 3 ROTATE |

### 11.12.18 变更日志

- **2026-08-22** - 引入 v9 per-turn multi-chunk chain rolling log，替换 v4 / v8 的"单占位 + 独立 bubble"双轨制。新增文件：`internal/channel/telegram/placeholder_chain.go`（含 chainLRU）/ `internal/channel/telegram/summarize_tool.go`（从 feishu 平移）。改动：`Adapter.Send` 8 个 Out* case 重写为 `appendSegment` 路径 / `OutHeartbeat` 改 `patchActiveHeader` / `OnPromptEnded` 改 flushChain + 🎉 + cursor reset / `formatTool` 改为调 summarize helpers / `ensurePlaceholder` delegate 到 chain。**未持久化**：`TopicState.PlaceholderChunkIDs`（本规划中曾计划加入，最终决定不写）；`buf` / `headerLine` / `lastFooter` 全部纯内存。

- **2026-08-22 (晚)** - 多次 P0/P1/P2 修复（P0 #1 cold-create 种子 entries，P0 #2 overflow tail 保留 content，P0 #3 case-3 内联 fast-forward 避免 mutex 重入，P1 byteOffset 死代码删除，P2 cold-start body 含 separator）。Commit `08f8f7e` 包含 codex review fixes。

- **2026-08-22 (晚)** - chain integration tests `chain_integration_test.go` 加入。Commit `e355153`。

- **2026-08-23** - §11.12.16 矩阵补完（`TestChainOverflow_TailHasNonEmptyEntries` P0 #2 lock-in 等），v9 chain codex review 收尾。

- **2026-08-23** - `placeholder_chain_flush.go:217` 删除 debug `fmt.Println`；footer policy 改成数据驱动（`statusbar.StatusBarLines(msg) != nil` 决定 lastFooter 刷新，Kind 不锁）；`appendSegment` 每路径必 `dirty=true` + `scheduleFlushDebounced` 无条件调用，确保 Render 总发生。Commit `39579b8`。

- **2026-08-23** - §18 StatusBar trailer 扩到所有 text-emitting kind（包括 OutThinking / OutToolStart / OutToolEnd / OutError / OutCommandReply）；`isTextEmittingKind` helper 收拢 policy。Commit `7bf76be`。

- **2026-08-23** - **chainKey 加 userMessageID 字段**（commit `a654fc3`）—— 锁死 back-to-back user msg 的 Out* 串扰（race condition）。`getOrCreate / lookup / purge` 三个 chainLRU 方法加 userMessageID 参数；14 个 adapter.go call site 全部更新。`patchChainHeader` 加 userMessageID 显式参数（替代原 hardcoded 0）。**ROTATE tail header 改用 `heartbeatText(nil)`**（同 commit）—— ~~视觉连续性优先让位于"每条 message 的 header 反映创建时间"~~。**2026-08-23 (v9 P1.1) 推翻**:新 chunk header 不再"反映创建时间",而是完全 inherit 当时的 active 状态快照(`inheritLatestHeader`)。见 §11.12.7.4 + 同日 P1.1 变更日志。

- **2026-08-23** - **SPLIT path 实现**（commit `aad7705`）—— §11.12.7.2 trigger 1 落地。`appendSegment` / `appendErrorSegment` 入口 pre-check（`len(segment) > chainChunkThresholdChars`）→ `splitOversizedSegmentLocked` / `splitOversizedErrorSegmentLocked`。`chunkBody.appendEntryHTML` 新方法（isHTML=true，Compose() 跳过 RenderMarkdown 避免二次转义）。6 个新 SPLIT 测试 + 1 个 ROTATE 测试数据修正（4000-char segment 改 3499-char，避开新 SPLIT 触发）。

- **2026-08-23** - **3 个 chain-key isolation 回归测试**（commit `614922e`）：`TestChain_BackToBackUserMessages_AreSeparateChains` / `TestChain_DelayedOutReply_AfterNewUserMsg_DoesNotLeak` / `TestChain_Heartbeat_DoesNotCrossUserMessageBoundary`。

- **2026-08-23** - **测试基础设施 race fix + footer 回归测试**（commit `2e4fb85`）：`sendMessageCounter` 改 `atomic.Int64`（pre-existing race 修干净，`go test -race` 现在 clean）；4 个 footer 回归测试（`TestChain_RenderAlwaysHappen_*` / `TestChain_DataDrivenFooter_*` / `TestChain_NewChunk_InheritsLastFooter` / `TestChain_MultipleOverflow_*`）；`fmt` import 清理。

- **2026-08-23** - **Spec 同步**（commit `b68cc30`）：§11.12.2 chainKey + LRU cap 含义 / §11.12.3 阈值 + SPLIT rationale / §11.12.4 OutCommandReply + 超长处理段 / §11.12.6 数据驱动 footer + Render-always / §11.12.7.2 三触发器决策矩阵 / §11.12.8 heartbeat 例代码 / §11.12.10 chain loss / §11.12.11 Topic vs DM 不变。

- **2026-08-23** - **Spec 进一步对齐**（本次 commit）：§11.12.2 chunkBody API 加 `appendEntryHTML` / §11.12.4 OutError 路径改为 `appendErrorSegment` / §11.12.5 核心 API 重写（package-level + chainLRU 实际签名）/ §11.12.7.2 trigger 1 partial-failure + trigger 3 step 5 header 来源 / §11.12.8 / §11.12.9 例代码改实际 / §11.12.15 commit 清单改 git log 引用 / §11.12.16 矩阵用实际 test 名 / §11.12.17 limits 表补 SPLIT + chain-key / §11.12.18 变更日志追加本批。

- **2026-08-24** - **v9 P2 OutResult 独立消息**：对齐飞书 F-39 决策。`Send(OutResult)` 从 default 分支挑出走 `sendOutResultMessage` helper，直接 `sendTelegramMessage(reply_to_message_id=userMsgID, text=result+StatusBar trailer)`。新增 `placeholderChain.resultMessageID` 字段记录最后一片 result messageID；长 result > 3900 chars 走 `splitTelegramText` 切多片。`OnPromptEnded` 🎉 锚点改为 `chain.resultMessageID` 优先、零值回退 active chunk（保住 error-only / tool-only / slash-only turn 行为）。改动：`adapter.go` Send / OnPromptEnded + `placeholder_chain.go` struct；新增 helper `sendOutResultMessage` / `sendResultChunk`。**对齐项**：§11.12.4 表格 OutResult 行 + §11.12.4.1 新增（独立消息契约 + resultMessageID 字段语义）+ §11.12.9 代码示例 + §11.12.11 table 行 + §11.12.12 三栏飞书对位 + §11.12.13 长文本段 + §11.12.16 测试矩阵。**commit 拆解**：`chain: add resultMessageID anchor field` → `Send: split OutResult → standalone sendOutResultMessage` → `OnPromptEnded: prefer resultMessageID over active chunk` → `tests: OutResult standalone message + 🎉 anchor switch` → `docs: §11.12 v9 P2 OutResult 独立消息`。

- **2026-08-24** - **v9 P2.1 OutResult standalone 移除中间分隔线**（user feedback on first dotest）。`sendOutResultMessage` 不再在 result body 和 trailer 之间插入 `\n────────\n` —— trailer 自带 `┌──› / └──›` box-drawing 边框提供视觉边界，额外横线让 standalone reply-anchored message 看起来"断裂"。`adapter.go` sendOutResultMessage helper 改为 `trailer = "\n" + statusbar.RenderPanel(sb)`。**chain chunk 仍保留 `────────────────` 分隔**（chunk_body.Compose 在 entries 和 footer 之间硬编码这一行）—— chain 上的 entries 是 activity log 序列，footer 是状态 summary，两者之间需要强分隔。**测试更新**：`TestAdapter_Send_OutResult_SendsStandaloneReply` 移除 `────────` 断言（其他 trailer 三行断言保留）。**doc 更新**：§11.12.4.1 视觉示例 + §11.12.4.1 契约第 2 条 + §11.12.18 变更日志（本条）。

- **2026-08-24** - **DRY: 收口 wire-facing markdown 渲染，新增 `RenderForWire`**。v9 P2 把 OutResult 改成独立 `sendMessage` 时漏掉了 markdown→HTML 渲染步骤（用户写 `**bold**` / ```fences``` / `[link](url)` 全渲染成字面字符）。本次修复：(1) `internal/channel/telegram/render.go` 新增 `RenderForWire(raw string) string` —— 单一 wire-facing 入口，包一层 `RenderMarkdown` + 空串 short-circuit + err fallback（escapeHTML）。`sendOutResultMessage` 调它一次，trailer (`statusbar.RenderPanel`) 仍直通不再二次渲染避免 box-drawing 被 escape。(2) DRY 清扫：`topic.go` 删除三个 dead method —— `sendText` / `sendRenderedText`(v8 per-bubble path 残留，v9 chain 接管后零调用) + `createTopic` / `editTelegramKeyboard`(从 topic-mode 评审期留下，代码默认论坛用现成 `message_thread_id`，从未有生产 caller)；`adapter.go:1638` 替换过时的 `sendRenderedText which v9 no longer routes` 历史注释。(3) **chunkBody.Compose 继续走 `RenderMarkdown` 不是 `RenderForWire`** —— Compose 是 per-entry loop + `isHTML` 路由，不能套 block-level 包装；两个入口覆盖两条路径不算重复，是 render.go 注释里固定的契约。(4) 测试：`render_test.go` 新增 5 个 `TestRenderForWire_*`（empty / bold / fence / link safe / raw HTML escape）+ `adapter_statusbar_test.go` 新增 `TestAdapter_Send_DM_OutResult_RendersMarkdownToHTML`（端到端断言 `<b>` / `<code>` / `<pre>` / `<a>` 都进 wire + literal `**` ``` ``` ``` `[..](..)` **不**进 wire + `parse_mode=HTML` 仍存在 + trailer 仍三行）。**doc 更新**：§11.12.13 重写 Markdown 渲染段，明确 `RenderForWire` 是 wire 入口 + Compose 是 chunk 入口 + 未来 sanitize pipeline 只注入 `RenderForWire` 一处；§11.12.18 变更日志（本条）。

- **2026-08-24** - **v9 P3: 渲染 DRY 收口 + blank-chunk 修复**。详见 §11.12.19。本次修复分两条独立但同 PR 的线：(1) **blank-chunk bug fix**：ROTATE / SPLIT 路径在边界条件下（segment 是空白 / entries 全是 `"\n"`）mint 出"只有 footer 的假空白 chunk"，用户视角像没说话又发一条。根因是 11 个 sendFn 站点缺守卫 + `strings.TrimSpace(Compose())` 被 footer box-drawing 字符欺骗。修法：`chunkBody.hasVisibleEntries()` —— 唯一一处"是空白"定义，看 entries + taskList 两个 section；`materializeChunk` 协调器封装 `stampFooter → hasVisibleEntries 检查 → sendFn → messageID 写回 → chain.chunks append` 五步，11 个 sendFn 站点（`appendSegment` cold-create / ROTATE / `appendSegmentLocked` cold-create / ROTATE / `appendErrorSegment` cold-create / ROTATE / `splitOversizedSegmentLocked` 循环 / `splitOversizedErrorSegmentLocked` 循环 / `flushChainNow` overflow intermediate + tail / `setTaskList` cold-create）全部收敛。 (2) **渲染 DRY**：5 处 `RenderMarkdown + escapeHTML fallback` 模式（`chunkBody.Compose` per-entry / `chunkBody.renderTaskSection` / `splitOversizedSegmentLocked` / `splitOversizedErrorSegmentLocked` / `RenderForWire`）+ 1 处 `body + "\n\n" + RenderPanel(footerLines)` trailer 拼接模式（`sendOutResultMessage`）—— 收口到两个共享原语：`renderMarkdownSafe(s)` 和 `appendTrailerToBody(body, footerLines)`。三层结构（数据类 → 协调器 → 共享原语）跟现有 v9 P1.1（`inheritLatestHeader` 收口）+ v9 P2（`RenderForWire` 收口）的演化路径一致。改动文件：`render.go`（新增两个原语 + `RenderForWire` 改薄壳）/ `chunk_body.go`（新增 `hasVisibleEntries` + 2 处 fallback 收敛）/ `placeholder_chain_flush.go`（新增 `materializeChunk` + 11 站点收敛 + 2 处 fallback 收敛）/ `adapter.go`（`sendOutResultMessage` 用 `appendTrailerToBody`）。**Compose 和 RenderForWire 保持分离**（结构性差异：chain 消息 vs standalone 消息），只是共享 `renderMarkdownSafe` 原语。**doc 更新**：§11.12.13（Markdown 渲染三层原语结构）/ §11.12.16（追加 P3 测试矩阵 22 个新 test case）/ §11.12.18（本条）/ 新增 §11.12.19（完整 P3 spec）。**commit 拆解**（建议）：`render: add renderMarkdownSafe + appendTrailerToBody primitives` → `chunkBody: hasVisibleEntries predicate` → `placeholder_chain_flush: materializeChunk coordinator` → `appendSegment / appendSegmentLocked / appendErrorSegment: route through materializeChunk` → `splitOversized*Locked / flushChainNow overflow: route through materializeChunk` → `chunkBody.Compose / renderTaskSection / split*: use renderMarkdownSafe` → `sendOutResultMessage: use appendTrailerToBody` → `tests: 22 new test cases for §11.12.16` → `docs: §11.12.19 P3 spec`。

### 11.12.19 渲染 DRY + blank-chunk 修复（2026-08-24）

本节是 v9 P3 —— 收口渲染原语 + 修复 ROTATE/SPLIT 路径在边界条件下 mint 出的"只有 footer 的假空白 chunk"。**两条线独立但同 PR**：渲染原语收口（DRY）是 clean-code 改进，blank-chunk 修复是 user-visible bug fix。

#### 11.12.19.1 现象：blank-chunk

dotest 截图里，agent turn 末尾偶尔出现一条**新 Telegram 消息**，正文区域几乎全空，只剩 header + 分隔线 + StatusBar 三行 footer。从用户视角看像"什么都没说但又发了一条"。多次复现条件：ROTATE 触发的瞬间，新 segment 本身是空白（trim 后空），或 entries 里堆了一堆 `"\n"`（来自流式 flush 之间的空白分隔 + ACP bridge `flushBuffer` 残留）。

#### 11.12.19.2 根因：两层叠加

**根因 1：ROTATE / SPLIT 路径缺空白守卫**

5 个真正调 `sendFn` mint 新 Telegram 消息的站点：

| # | 站点 | 文件:行 | 守卫？ |
|---|---|---|---|
| 1 | `appendSegment` case 1 (cold-create) | `placeholder_chain_flush.go:123-158` | 无 |
| 2 | `appendSegment` case 3 (ROTATE) | `placeholder_chain_flush.go:168-198` | 无 |
| 3 | `appendSegmentLocked` case 1/3 (OutToolStart) | `placeholder_chain_flush.go:220-298` | 无 |
| 4 | `splitOversizedSegmentLocked` (SPLIT trigger 1) | `placeholder_chain_flush.go:415-479` | 无 |
| 5 | `splitOversizedErrorSegmentLocked` (OutError SPLIT) | `placeholder_chain_flush.go:912-` | 无 |
| 6 | `flushChainNow` overflow (trigger 3 safety net) | `placeholder_chain_flush.go:551-614` | 无 |

唯一一道防线在 `appendSegmentForKind`（`adapter.go:1262-1264`），但它只覆盖 OutReply / OutThinking / OutCommandReply / OutTask* 等 text-emitting kind；`appendSegmentLocked`（OutToolStart）绕过它直调 `appendSegmentLocked`，SPLIT 和 overflow 也不经过它。

**根因 2：第一直觉的 `strings.TrimSpace(Compose()) != ""` 不灵**

footer 是 box-drawing + emoji + 路径（`┌──› ... 🤖 ... 💰 ... 📁 ... └───›`），全是非空白字符。ROTATE 触发的空白 chunk 在 Telegram 端渲染后：

```
[banner header]
────────────────
\n\n
────────────────
┌──────────────›
│ 🤖: claude · opus-4-5 · ...
│ 💰: 「$0.05」
│ 📁: code/nightme
└───────────────›
```

`strings.TrimSpace` 只剥首尾空白，对中间的 footer 一行没辙。所以 `hasVisibleBody()` 永远返回 true，等于没守卫。**footer 是 chrome，不是内容** —— 真正的"内容"是 entries。

#### 11.12.19.3 修复方案：三层（数据类 → 协调器 → 共享原语）

```
Layer 1 (data+view, chunk_body.go):
    chunkBody.hasVisibleEntries() bool           ← 唯一的"是空白"定义
    ── 只看 entries，footer/header/banner 都跳过

Layer 2 (协调器, placeholder_chain_flush.go):
    materializeChunk(ctx, chain, chunk, ...) (materialized bool, err error)
    ── 唯一一处做 stampFooter+Compose+blank-check+sendFn+messageID 写回
    ── 11 个 sendFn 站点都收敛到这里

Layer 3 (共享原语, render.go):
    renderMarkdownSafe(s) string                 ← 唯一一处 RenderMarkdown + escapeHTML fallback
    appendTrailerToBody(body, footerLines) string ← 唯一一处 body + RenderPanel trailer 拼接
    ── 5 处 RenderMarkdown fallback + 1 处 trailer 拼接都收敛到这里
```

##### Layer 1：`chunkBody.hasVisibleEntries()`

```go
// hasVisibleEntries answers "does this chunk carry real content
// that the user will see in Telegram?".
// Header is a status banner (rendered or skipped by banner-skip
// rule), footer is the StatusBar chrome — neither counts as
// content.
//
// Content sections counted:
//   - entries: one or more non-whitespace text rows (the main
//     activity log)
//   - taskList: non-empty agent task snapshot (renders as the
//     `<b>📋 Tasks</b>` headline + at least one task row in Compose)
//
// The blank-chunk bug fires when ROTATE / SPLIT mints a chunk
// whose entries are pure whitespace: the chunk would visually
// show as header-divider-footer with no body, but
// strings.TrimSpace(Compose()) is fooled by the footer's
// box-drawing chars and emoji and returns false-blank.
//
// Caller must populate entries via appendEntry / appendEntryHTML
// / appendError AND/OR taskList via setTaskList before asking.
// A freshly newChunkBody() chunk with zero entries AND nil
// taskList returns false — consistent with "don't mint an orphan
// placeholder".
func (b *chunkBody) hasVisibleEntries() bool {
    for _, e := range b.entries {
        if strings.TrimSpace(e.text) != "" {
            return true
        }
    }
    if len(b.taskList) > 0 {
        return true
    }
    return false
}
```

**为什么不直接看 `Compose()`**？footer 的 box-drawing 字符让 `TrimSpace(Compose())` 永远 non-empty —— 这正是 bug 漏出来的原因。**为什么不看 `len(b.entries) == 0`**？ROTATE 触发的空白 chunk entries 不空（`["\n"]`），但内容是空白 —— 必须看 entries 的实际内容。

##### Layer 2：`materializeChunk` 协调器

```go
// materializeChunk is the SOLE place that calls sendFn for a
// freshly-born chunk. Encapsulates:
//   1. stamp lastFooter onto chunk (if present)
//   2. compose + hasVisibleEntries() check → drop if blank
//   3. send via sendFn
//   4. assign messageID back onto chunk
//   5. append chunk to chain.chunks
//   6. mark chain.dirty = true
//
// Callers decide what to do with `materialized`:
//   - cold-create / ROTATE / SPLIT-tail: advance cursor
//   - SPLIT intermediate: leave cursor alone (it's frozen)
//
// Returns (false, nil) for a dropped blank chunk — not an error,
// just a no-op the caller should NOT advance cursor for.
func materializeChunk(
    ctx context.Context,
    chain *placeholderChain,
    chunk *chunkBody,
    chatID string,
    topicID, userMessageID int,
    sendFn sendChunkFn,
) (materialized bool, err error) {
    if chain.lastFooter != nil {
        chunk.setFooter(statusbar.RenderPanel(chain.lastFooter))
    }
    if !chunk.hasVisibleEntries() {
        return false, nil
    }
    body := chunk.Compose()
    mid, err := sendFn(ctx, chatID, topicID, userMessageID, body)
    if err != nil {
        return false, err
    }
    chunk.messageID = mid
    chain.chunks = append(chain.chunks, chunk)
    chain.dirty = true
    return true, nil
}
```

5 个站点收敛前：

```go
// (a) appendSegment case 1 cold-create
if chain.cursor < 0 {
    headerLine := heartbeatText(nil)
    chunk := newChunkBody(0, headerLine)
    chunk.appendEntry(segment)
    if chain.lastFooter != nil {
        chunk.setFooter(statusbar.RenderPanel(chain.lastFooter))
    }
    body := chunk.Compose()
    messageID, err := sendFn(ctx, chatID, topicID, userMessageID, body)
    if err != nil { return err }
    chunk.messageID = messageID
    chain.chunks = []*chunkBody{chunk}
    chain.cursor = 0
    chain.dirty = true
    return nil
}
```

收敛后：

```go
if chain.cursor < 0 {
    chunk := newChunkBody(0, heartbeatText(nil))
    chunk.appendEntry(segment)
    materialized, err := materializeChunk(ctx, chain, chunk,
        chatID, topicID, userMessageID, sendFn)
    if err != nil { return err }
    if materialized {
        chain.cursor = 0
        chain.dirty = true
    }
    return nil
}
```

ROTATE / SPLIT / flushChainNow overflow / setTaskList cold-create 等 11 个 sendFn 站点同样收敛。`dirty=true` 由协调器保证，调用方不再各自写。

##### Layer 3：`renderMarkdownSafe` + `appendTrailerToBody`

**`renderMarkdownSafe`** —— 5 处 fallback 收口：

```go
// renderMarkdownSafe is the SOLE place that runs RenderMarkdown +
// escapeHTML fallback. Callers that need "markdown → safe HTML
// for Telegram wire" should use this rather than duplicating the
// try-render-or-escape pattern. RenderMarkdown and escapeHTML
// remain exported for the rare caller (tests, low-level chunk
// Compose per-entry loop) that wants raw escape or raw render.
func renderMarkdownSafe(s string) string {
    if s == "" {
        return ""
    }
    out, err := RenderMarkdown(s)
    if err != nil {
        return escapeHTML(s)
    }
    return out
}
```

调用方收敛：

| 位置 | 之前 | 之后 |
|---|---|---|
| `chunkBody.Compose` per-entry | `RenderMarkdown + err→escapeHTML` | `text = renderMarkdownSafe(text)` |
| `chunkBody.renderTaskSection` | 同上 | `renderedMarkdown := renderMarkdownSafe(joined)` |
| `splitOversizedSegmentLocked` | 同上 | `rendered := renderMarkdownSafe(segment)` |
| `splitOversizedErrorSegmentLocked` | 同上 | `rendered := renderMarkdownSafe(body)` |
| `RenderForWire` | `RenderMarkdown + err→escapeHTML` | `return renderMarkdownSafe(raw)` |

**`appendTrailerToBody`** —— 1 处 trailer 拼接收口：

```go
// appendTrailerToBody appends the StatusBar panel to body if
// footerLines is non-nil. Returns body unchanged when footer is
// absent. Used by sendOutResultMessage and any future single-
// shot message render path.
func appendTrailerToBody(body string, footerLines []string) string {
    if len(footerLines) == 0 {
        return body
    }
    return body + "\n\n" + statusbar.RenderPanel(footerLines)
}
```

调用方收敛：

| 位置 | 之前 | 之后 |
|---|---|---|
| `sendOutResultMessage` (adapter.go) | `body + "\n\n" + statusbar.RenderPanel(sb)` | `appendTrailerToBody(body, sb)` |

#### 11.12.19.4 边界覆盖

`hasVisibleEntries()` 矩阵（每个 case 都对应实际生产场景）：

| 场景 | entries | hasVisibleEntries | 行为 |
|---|---|---|---|
| Cold-create + 真实 segment（`appendSegmentForKind` 已保 non-blank） | `["● Bash(go build)"]` | true | send ✓ |
| Cold-create + 空白 segment（理论上被外层 guard 拦住，belt-and-suspenders） | `["\n"]` | false | skip ✓ |
| ROTATE + 真实 segment | `["real content"]` | true | send ✓ |
| **ROTATE + 空白 segment** | `["\n"]` | **false** | **skip ✓ ← 这就是 bug fix** |
| SPLIT 单 piece 全空白 | `[""]` | false | skip ✓ |
| SPLIT 多 piece，前几 piece 空白 | `[""]` | false | skip ✓ |
| flushChainNow overflow piece 全空白 | `[""]` | false | skip ✓ |
| SPLIT 后续 replaceEntry 把 entry 改空白（理论上不发生，guard 守住） | `["\n"]` | false | skip ✓ |

#### 11.12.19.5 三层之间的边界

- **Layer 1 ↔ Layer 2**：`chunkBody.hasVisibleEntries()` 由 `materializeChunk` 调用；其他 call site 不直接调它（保持单一权威点）。
- **Layer 2 ↔ Layer 3**：`materializeChunk` 调 `chunkBody.Compose()`；`Compose()` per-entry 调 `renderMarkdownSafe`；`materializeChunk` 本身**不**直接调 `renderMarkdownSafe`（chunk 是已组装好的数据，不是 raw markdown）。
- **Layer 3 之间**：`renderMarkdownSafe` 是 markdown → HTML 原语；`appendTrailerToBody` 是 body + panel 拼接原语。两者无依赖。

#### 11.12.19.6 不做的事

- **不**把 `Compose()` 和 `RenderForWire` 合并成一个 —— 结构性差异：chain 消息有 header/entries 多 section；standalone 是一段文本。强行合并要给零 entries / 单 entry 加分支判断。
- **不**让 SPLIT 路径也走 lazy-render（per-piece Compose）—— 性能+正确性问题：单 entry > 3500 时 Compose 输出会超 4096 硬限，且每片都跑一遍 RenderMarkdown 是浪费。
- **不**让 `hasVisibleEntries()` 直接看 `len(entries) == 0` 或 `Compose()` 整体 —— ROTATE 触发的空白 chunk entries 是 `["\n"]`（非空但内容空白），Compose 整体被 footer 干扰。只有逐条看 entry text 才能正确判定。
- **不**wrap `chainSendFn()` 让 blank 返 0 —— sendFn 的纯 send 契约会被破坏，且每个站点还得处理"返 0 怎么办"，退化成 DRY 散落。

#### 11.12.19.7 跟 §11.12.13 的关系

§11.12.13（Markdown 渲染段）描述**结构**：三层原语 `renderMarkdownSafe` / `RenderForWire` / `chunkBody.Compose` 各管一摊。本节描述**演化**：v9 P3 把 4 处 `RenderMarkdown + escapeHTML` fallback 收口到 `renderMarkdownSafe`，把 5 处 sendFn + Compose 模板收口到 `materializeChunk`。两条线的语义不变（standalone 仍是 `RenderForWire`，chain 仍是 `Compose`），只是把"重复的样板代码"集中到原语层。

## 12. Telegram 交互输入：Type your answer + ForceReply

Telegram 的 InlineKeyboard 只能展示按钮，不能像飞书 Card 一样在按钮旁边直接渲染文本输入框。对于需要用户自由输入的答案，推荐采用两步方案：

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

### 12.1 推荐消息流

```text
Telegram Topic
├─ 👉 Action Needed · 1/2
│  ├─ [ Option A ]
│  ├─ [ Option B ]
│  ├─ [ Skip this question ]
│  └─ [ Type your answer ]
│
├─ 请输入你的答案……（ForceReply）
│
└─ 用户输入的文本（reply to ForceReply 消息）
```

虽然消息被拆成两条，但它们都位于同一个 Telegram Topic，用户仍然能沿着 Topic 时间线理解上下文。

### 12.2 第一步：处理 Type your answer 按钮

按钮使用短小、可解析且不泄露敏感信息的 `callback_data`：

```json
{
  "text": "Type your answer",
  "callback_data": "input:card123:q1"
}
```

收到 callback 后，Telegram Adapter 应立即：

1. 校验 `callback_query.from.id` 是否为当前 Choice 操作人。
2. 校验 `callback_query.message.chat.id` 和 `message_thread_id`。
3. 解析 `input:card123:q1`，查找 `ChoiceState`。
4. 调用 `answerCallbackQuery`，结束按钮 loading 状态。
5. 将 Choice 提示更新为“等待输入”状态，并禁用或删除 `Type your answer` 按钮。
6. 在同一个 Topic 中发送 ForceReply 消息。
7. 保存输入提示消息的 `message_id`，进入 `waiting_input` 状态。

ChoiceState 至少包含：

```text
CardID
MessageID
ChatID
MessageThreadID
UserID
RequestID
QuestionID
State
ForceReplyMessageID
```

其中：

```text
State = waiting_input
```

表示当前 Choice 正在等待用户输入。

### 12.3 第二步：发送 ForceReply 消息

在当前 `message_thread_id` 中发送：

```text
请输入你的答案
```

Telegram 侧概念请求：

```json
{
  "chat_id": -1001234567890,
  "message_thread_id": 42,
  "text": "请输入你的答案……",
  "reply_markup": {
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

### 12.4 与现有 qino Action 协议对应

为了复用当前 `chatsession.Manager.SendPermission(chatID, option string)`，第一版不需要修改 Agent 或 Gateway 的权限协议。

单问题：

```text
用户输入 = custom
        │
        ▼
Action.Option = custom
```

多问题：

```text
Q1 selected = staging
Q2 custom   = feat: add telegram topic support
        │
        ▼
EncodeQuestionPicks()
        │
        ▼
Action.Option = nm-q:<JSON>
```

`ActionPayload.Form` 已存在于统一消息模型中，但当前权限分发路径主要消费 `Action.Option`。第一版让 Telegram Adapter 按 Feishu 的既有方式生成 `Action.Option`，避免为了 ForceReply 立即修改所有 Bridge 的 permission 协议。

### 12.5 多问题向导处理

以两个问题为例：

```text
Q1: 选择环境
[ staging ] [ production ] [ Type your answer ]
```

用户选择 staging 后，Adapter 更新卡片：

```text
Q2: 输入 PR 标题
[ Type your answer ] [ Skip this question ]
```

用户点击输入按钮后：

```text
Q2 输入提示（ForceReply）
用户回复：feat: add telegram topic support
```

Adapter 更新本地 ChoiceState：

```text
Questions = [q1, q2]
Step = 1
Picks = ["staging", "nm-c:feat: add telegram topic support"]
```

如果这是最后一步，生成：

```json
{
  "Option": "nm-q:[{\"id\":\"env\",\"selected\":[\"staging\"]},{\"id\":\"pr_title\",\"custom\":\"feat: add telegram topic support\"}]"
}
```

再走现有：

```text
InboundMessage.Action
        │
        ▼
Gateway
        │
        ▼
chatsession.Manager.SendPermission
        │
        ▼
Agent / Bridge
```

中间步骤不触发 Agent，只有完成最后一步或用户点击 Skip/选项后才继续提交。

### 12.6 ForceReply 期间如何处理普通消息

用户可能忽略 ForceReply 提示，在群里直接发一条普通消息：

```text
staging
```

Adapter 不应仅凭文本内容把它当作答案。必须检查回复目标：

```text
是回复 ForceReply 消息
  └── 作为 custom answer 处理

不是回复 ForceReply 消息
  └── 不作为当前答案处理
```

可以提供以下降级体验：

- 在同 Topic 发送“请点击输入按钮后回复上一条消息”的提示。
- 在原 Choice 提示上恢复 `Type your answer` 按钮。
- 增加 `Cancel input` 按钮，删除输入提示消息并清理 `waiting_input` 状态。
- 对长期未输入的提示设置超时和清理策略。

### 12.7 按钮、提示消息和 Choice 消息的更新

用户点击 `Type your answer` 后：

```text
原 Choice 消息
  └── editMessageReplyMarkup：移除或禁用输入按钮

新消息
  └── sendMessage：ForceReply 输入提示

用户回复
  └── deleteMessage 或 editMessageText：清理输入提示

原 Choice 消息
  └── editMessageText：显示下一题或完成状态
```

不要删除原 Choice 消息本身，除非它已经被用户删除或 Telegram 无法继续编辑。

### 12.8 与 Mini App 的关系

ForceReply 是第一版推荐方案：

- 只需要一条额外的 Bot 消息
- 不需要公网 Web App
- 不需要验证 `initData`
- 不需要维护前端表单
- 适合答案较短、需要文本输入的场景

如果以后需要多字段表单、日期选择、文件选择或复杂下拉框，可以将同一个 `Type your answer` 入口升级为 `WebAppInfo` + Mini App。两种方案可以共存，ChoiceState 和 `RequestID` 保持不变。

### 12.9 验收标准

- 点击 `Type your answer` 后，按钮 loading 状态立即结束。
- ForceReply 提示消息带有正确的 Topic ID，并启用 `force_reply`。
- 用户直接回复 ForceReply 消息时，答案被识别为当前问题的 custom input。
- 用户发送普通群消息时，不会被误判为 custom input。
- 单问题输入能转换为 `Action.Option`。
- 多问题最后一步能转换为 `nm-q:` batch。
- 重复点击输入按钮不会创建多个 ForceReply 状态。
- `Cancel input`、超时和 daemon 重启不会让 ChoiceState 永久卡在 `waiting_input`。

## 13. 完整实施蓝图

本节将前面关于 Bot 开通、多群组、Forum Topic、消息路由、交互卡、ForceReply 和 Markdown 的结论收束成一份实施蓝图。

### 13.1 最终产品决策

采用以下模式：

```text
用户自建 Telegram Bot
        +
每个 nightme daemon 使用自己的 Bot Token
        +
daemon 直接调用 Telegram Bot API
        +
Forum Supergroup Topic 作为 qino 会话容器
        +
主窗口只作为请求入口
        +
thinking/tools/result 在 Topic 内累积
        +
普通 Telegram 消息 + InlineKeyboard + ForceReply 表达交互
```

明确不采用：

```text
中心共享 Bot
Relay / tenant 路由服务
多个 daemon 共享一个 Bot Token
在主窗口持续发送 qino 中间状态
将 Telegram 格式逻辑写入 Agent / Gateway / 公共消息模型
```

以上设计与 `main` 的当前 Channel 架构一致：Channel 的唯一出站方法是
`Channel.Send(OutboundMessage)`。`OutChoice`、`OutChoicePatch` 和所有文本、
工具、receipt、reaction 事件都经过同一个出口；调用方不直接拿 Telegram
message ID，也不存在 `SendCard` / `SendAction` 第二条出站通道。

### 13.2 用户开通与群组准备

每个用户的使用流程如下：

```text
1. 用户在 @BotFather 执行 /newbot
2. 用户获得唯一 bot username 和 Bot Token
3. 用户在 nightme 配置自己的 Bot Token
4. 创建一个 Telegram Forum Supergroup
5. 在 BotFather 执行 /setprivacy 并选择 Disable
6. 手动把 Bot 加入目标群组
7. 给 Bot 发送、管理和接收 Topic 消息所需权限
8. 启动 nightme daemon
9. 用户在主窗口发送请求
10. qino 创建或复用 Topic，后续过程全部进入 Topic
```

BotFather 的二维码能力边界：

```text
已有 Bot
  └── t.me/<bot_username>?startgroup
        └── 扫码后选择群组并把已有 Bot 添加进去
```

二维码不能自动创建 Bot、生成 Token、关闭 Privacy Mode 或授予管理员权限。首次开通必须由用户通过 BotFather 手动创建 Bot 并保护 Token。

### 13.3 接收模式

**唯一支持 Long Polling**。每个 Bot 只能有一个 `getUpdates` consumer。

```yaml
telegram:
  bot_token: "<local secret>"
  polling_timeout: 30
```

daemon 重启时使用持久化的 `update_id + 1` 继续消费。**不实现 Webhook 模式** —— 这避免每个用户都需要公网 HTTPS 入口和 secret 校验逻辑。

接收到的更新至少包括：

```text
message
callback_query
my_chat_member / chat_member
message_reaction
```

Bot 没有 Telegram 提供的“列出自己已加入的全部群组”接口，因此不能靠 API 一次性恢复完整群组列表。群组信息应通过首次加入事件、持续更新和本地持久化逐步建立。

### 13.4 多群组和 Topic 路由

同一个 Bot 可以加入多个群组。建议默认一个群组作为一个 qino workspace，一个 Forum Topic 作为一个独立 qino 会话：

```text
chat_id = -100111
├── message_thread_id = 1  → Topic 会话 A
└── message_thread_id = 42 → Topic 会话 B

chat_id = -100222
└── message_thread_id = 7  → Topic 会话 C
```

Adapter 将 Telegram 原生字段映射为：

```text
message.chat.id
  └── messages.InboundMessage.ChatID

message.from.id
  └── messages.InboundMessage.UserID

message.message_id
  └── messages.InboundMessage.MessageID

message.message_thread_id
  └── Telegram TopicID
```

Topic 生命周期状态至少需要持久化：

```text
ChatID
MessageThreadID
PlaceholderMessageID
UserMessageID
LastMessageID
CreatedAt
UpdatedAt
```

主窗口/General Topic 只用于入口，不作为 qino 日志堆积位置。Topic 内的 thinking、tool、result、状态和交互卡可以持续累积，用户进入 Topic 后查看完整过程。

### 13.5 消息和事件映射

| 夜 Me 事件 | Telegram Adapter 行为 |
| --- | --- |
| `OutThinking` | 在 Topic 中新增一条 thinking 消息 |
| `OutToolStart` | 在 Topic 中新增一条工具开始消息 |
| `OutToolEnd` | 在 Topic 中新增一条工具结束消息 |
| `OutHeartbeat` | 优先更新 Topic 中的占位消息 |
| `OutReply` | 发送或更新 Topic 消息 |
| `OutResult` | 在 Topic 中发送最终结果，长内容拆分 |
| `OutInit` | 更新占位消息 header/footer |
| `OutChoice` | 发送普通文本消息和 `InlineKeyboardMarkup` |
| `OutChoicePatch` | `editMessageText` / `editMessageReplyMarkup` |
| `OutError` | 在 Topic 中发送或更新错误状态 |
| `OutTaskCreate` / `OutTaskUpdate` | 更新任务消息或占位消息 |
| `OutCommandReply` | 在 Topic 中发送普通回复 |
| `OutMessageState` | `setMessageReaction`，目标必须是正确的入站消息 |

发送消息时：

```text
sendMessage / sendPhoto / sendDocument / sendMediaGroup
    └── 携带 chat_id + message_thread_id
```

编辑已有消息时：

```text
editMessageText / editMessageReplyMarkup / editMessageMedia
    └── 使用 chat_id + message_id
```

Topic 本身只通过 `editForumTopic` 修改名称或图标，不能通过 `editForumTopic` 更新占位正文。

### 13.6 交互 Choice 方案

Telegram 不接收飞书 Card JSON，而是接收语义化的 `messages.Choice`，再由
Telegram Adapter 拆成普通消息、InlineKeyboard、CallbackQuery 和可选的
ForceReply 消息。

当前 `messages.Choice` 的语义字段是：

```text
RequestID       关联一次交互选择
Kind            ChoiceKindPermission / Question / Decision
Title / Body    要展示的语义标题和正文
Options         []ChoiceOption
Questions       []ChoiceQuestion
Settled         是否已结算
SelectedID      结算时选中的 ChoiceOption.ID
```

`messages.Choice` 不携带 Telegram message ID、Card JSON、Step/Picks、
按钮布局或主题样式。`Step/Picks` 由 Telegram Adapter 私有的 ChoiceState
维护；`ChoiceOption.ID` 是语义选择值，`Emoji` 是 gtw Decision 的 reaction key。

整体交互拆成：

```text
普通文本消息
    +
InlineKeyboardMarkup
    +
CallbackQuery
    +
可选 ForceReply 输入消息
```

映射关系：

| 飞书 Choice 元素 | Telegram 实现 |
| --- | --- |
| 标题 | 加粗的普通文本行 |
| 正文 | Telegram 安全 HTML 文本 |
| Button | `InlineKeyboardButton` |
| ChoiceOption ID / Emoji | 短的 `callback_data` token；Telegram Adapter 负责反向查表 |
| Choice callback | `CallbackQuery` |
| Toast | `answerCallbackQuery` |
| Choice 原地更新 | `editMessageText` + `editMessageReplyMarkup` |
| Form | ForceReply 两步输入，复杂场景再上 Mini App |
| 多步骤问题 | Telegram Adapter 维护 `Step` 和 `Picks` |
| Choice 禁用 | 移除或替换 InlineKeyboard |

推荐使用受限 `callback_data`，只放不透明短 token：

```text
perm:card123:allow
input:card123:q1
skip:card123:q1
act:card456:commit
```

真实 `RequestID`、ChoiceKind、当前 Question、已选 Picks、ChoiceOption 和
`Settled/SelectedID` 关联放在 Telegram Adapter 的私有 `ChoiceState` 中，不把
完整语义 ID 或 gtw action 放进 callback 数据。`messages.Choice` 本身不携带
Step/Picks。

### 13.7 Choice 处理生命周期

`Channel.Send(OutboundMessage{Kind: OutChoice, Choice: ...})`：

```text
1. 校验 msg.ChatID 和 Topic 路由
2. 渲染 Choice 文本
3. 渲染 InlineKeyboard
4. 调用 sendMessage
5. 保存返回 message_id
6. 建立私有 ChoiceState
7. 记录 request_id、kind、questions、step、picks
```

`OutChoicePatch` 禁止通过 `ReplyTo` 传递 Telegram message ID。调用方只传
`Choice.RequestID`、新的语义状态 `Settled/SelectedID` 等内容；Telegram
Adapter 用自己的 `RequestID -> Telegram message ID` 映射找到目标消息。

CallbackQuery：

```text
1. 验证 user_id、chat_id、message_thread_id
2. 解析短 callback_data
3. 查找 ChoiceState
4. 立即 answerCallbackQuery
5. 根据 ChoiceKind 和选项更新本地 ChoiceState
6. 必要情况下 editMessageText / editMessageReplyMarkup
7. Permission / Question 选择发布 `Action.Option`
8. Decision 选择发布 `ReactionEvent`，走 gtw ReactionRouter
9. 多题仅在最后一步发布 EncodeQuestionPicks
10. 重复 callback 通过 callback_id / ChoiceState 做幂等
```

Approval 卡片处理：

```text
Waiting for approval
[Allow once] [Reject]
        │
        ▼
CallbackQuery
        │
        ├── 立即移除或禁用按钮
        ├── answerCallbackQuery
        └── Action.Option = allow / reject
```

Decision 卡片处理：

```text
Commit | Create PR | Cancel
        │
        ▼
ReactionEvent.Emoji = card state 中的 ChoiceOption.Emoji
        │
        ▼
复用现有 gtw ReactionRouter 路径
```

### 13.8 ForceReply 自定义输入方案

对于 Type your answer，采用：

```text
用户点击 [Type your answer]
        │
        ▼
原 Choice 消息进入 waiting_input
        │
        ▼
同 Topic 发送 ForceReply 消息
        │
        ▼
用户回复 ForceReply 消息
        │
        ▼
校验 reply_to_message、user_id、chat_id、topic_id
        │
        ▼
更新 ChoiceState / 下一题 / nm-q: payload
```

ForceReply 消息不是第一张 Choice 消息本身，而是当前 Choice 状态机的一步。用户不需要 Mini App，也不需要额外 Web 服务。

普通消息不能仅凭文本内容被当作 custom answer。必须验证：

```text
message.reply_to_message.message_id == ForceReplyMessageID
```

### 13.9 Markdown 渲染边界

Agent 输出原始 Markdown，不携带 Telegram `parse_mode`。Telegram Adapter 负责：

```text
LLM Markdown
    │
    ▼
Telegram Renderer
    │
    ├── 基础文本
    ├── HTML 语义标签
    ├── 代码块
    ├── 链接
    ├── 纯文本列表
    ├── 表格降级
    ├── HTML 转义
    ├── 消息长度拆分
    └── 渲染失败回退纯文本
```

默认使用 Telegram 受限 HTML 风格。MarkdownV2 需要对大量保留字符进行转义，不适合直接处理不可信的 LLM 原始文本。

推荐的语义降级：

```text
标题       → 粗体文本
粗体       → <b>
斜体       → <i>
行内代码   → <code>
代码块     → <pre>
链接       → <a href="...">
列表       → • / 1. 纯文本
表格       → 对齐文本或 <pre>
复杂布局   → 多条普通消息
颜色       → Emoji + 粗体
复杂 HTML  → 转义或删除
```

工具参数、工具输出、stderr 和错误信息也必须经过安全处理。Telegram 不支持的 HTML 标签和不合法的 URL 不能透传。

### 13.10 配置模型

建议的配置语义：

```yaml
telegram:
  bot_token: "<local secret>"
  polling_timeout: 30
```

> 早期设计曾考虑 `listen` / `routing` / `access` / `interaction` / `messages` 等多个分组,本次实现只保留 `bot_token` 和 `polling_timeout`。群组 mention gate 走 `chatsession.WatchMode`(`/watch all|mention|off`),不再用 config 控制。

实际字段名在实现时应以 `internal/config.Config` 为准。Bot Token 必须遵循本机凭证权限和敏感字段脱敏规则。

### 13.11 建议的代码结构

```text
internal/channel/telegram/
├── adapter.go
├── polling.go
├── topic.go
├── message.go
├── card.go
├── callback.go
├── render.go
├── attachment.go
├── reaction.go
├── health.go
└── tests
```

职责建议：

```text
adapter.go     Channel 接口、生命周期、In/Out 路由
polling.go     getUpdates offset、重连、错误分类
topic.go       createForumTopic、Topic ID 持久化
message.go     Message/Update 转换为 InboundMessage
choice.go      Choice 文本和 InlineKeyboard 渲染
callback.go    CallbackQuery 校验、ChoiceState、ActionPayload 和 ReactionEvent
render.go      Telegram Markdown/HTML 安全转换和拆分
attachment.go  Telegram file_id 下载和本地附件保存
reaction.go    setMessageReaction 和 message-state 映射
health.go      Bot API、polling、Topic 状态健康快照
```

公共的 `messages`、`Gateway` 和 Agent 协议不增加 Telegram 专属字段。Telegram 原生 ID 应在 Channel 层映射为 qino 的 `ChatID`、`MessageID` 和 `Action`。

### 13.12 安全与可靠性

必须实现：

- Bot Token 不出现在日志、错误、群消息和普通遥测中。
- 一个 Bot Token 只由一个 daemon 消费。
- callback 必须校验来源用户、chat、Topic 和 message ID。
- callback_data 不放 Bot Token、完整 gtw action 或用户输入。
- callback callback_id 做幂等。
- ChoiceState 在多问题向导和 ForceReply 等待期间可恢复。
- 发送错误按 `retry_after` 处理 429。
- Topic 或占位消息删除后可安全重建。
- 用户输入和 LLM 输出都要 HTML 转义。
- 不在主窗口静默处理 Topic 相关失败，应发送明确错误。

### 13.13 实施阶段

**Phase 1：基础 Bot 和群组消息**

- Token 配置和 Bot API 验证。
- Long Polling 和重连。
- Message → InboundMessage。
- 私聊、普通群组和 Topic 基础路由。

**Phase 2：Topic 消息和占位状态**

- createForumTopic。
- chat/topic/message ID 持久化。
- thinking/tools/result 发送到 Topic。
- OutHeartbeat 和 OutChoicePatch 原地更新。

**Phase 3：按钮交互**

- Permission / Decision / Option。
- InlineKeyboard。
- CallbackQuery。
- ChoiceState 和 OutChoicePatch。

**Phase 4：自定义输入**

- Type your answer 按钮。
- ForceReply 消息。
- reply_to_message 关联。
- 多问题 `nm-q:` batch。

**Phase 5：格式、附件和可靠性**

- Telegram Markdown/HTML Renderer。
- 表格和长消息降级。
- 附件下载和 Agent 侧 Attachments。
- reaction、限流、健康检查和 E2E 测试。

### 13.14 最终验收清单

- 用户可以在 BotFather 创建自己的 Bot 并配置 Token。
- Bot 可以加入多个群组。
- 群组是 Forum Supergroup 时可以创建或复用 Topic。
- 主窗口不会堆积 thinking/tools/result。
- Topic 内可以查看完整事件时间线。
- `chat_id` 能区分群组，`message_thread_id` 能区分 Topic。
- `placeholder_message_id` 可以支持状态原位编辑。
- Approval / Question 复用现有 `Action` 路径，Decision 复用 gtw
  `ReactionRouter` 路径。
- Type your answer 会发送 ForceReply 消息，并能正确处理用户回复。
- 普通群消息不会被误判为 ForceReply 答案。
- 多问题向导只对选项和自定义答案做正确汇总。
- Telegram Adapter 内部完成 Markdown、HTML 转义和消息拆分。
- LLM 原始 Markdown 不需要改变 Agent 输出协议。
- callback 重复、消息删除、Bot 权限变化和 429 都有明确处理。

## 14. MessageState 与 Card 独立轨道（v1.3 自治实现）

### 14.1 两条轨道

Telegram Channel 自治实现 `MessageState`（user-message reaction）与 `Card Body`（Topic 内的占位 / 事件详情）两条完全独立的渲染轨道，对齐 [docs/channel/feishu.md §6.6](./feishu.md) 的契约：

| 轨道 | 源 | 抽象事件 | 渲染目标 | Telegram 实现 |
| --- | --- | --- | --- | --- |
| **MessageState** | ChatSession lifecycle | `OutboundMessage{Kind: OutMessageState, MessageState: {State, MessageID}}` | **userMsgID** | `setMessageReaction(userMsgID, emoji)` |
| **Card Body（v9 chain）** | Topic placeholder chain（C2 链子，per-turn N 个 chunks）+ 事件流 | `OutboundMessage{Kind: OutHeartbeat/OutTool*/OutThinking/...}` | **active chunk messageID**（chain.cursor 指向）/ frozen chunks 不动 | `editMessageText(activeChunk, render(buf))`（debounce 合并 burst）/ 新建 chunk 时 `sendMessage` |

两者完全独立：一个失败不影响另一个（`MessageState` 渲染失败仅 log warn，不阻塞 card body）；都按 userMsgID / chatID 索引，但服务不同语义。详见 [`docs/feat/F-31-message-state.md`](../feat/F-31-message-state.md) 与 `SPEC.md §2.5`。

### 14.2 state → emoji 映射（Channel 自治）

```go
// internal/channel/telegram/adapter.go
func mapStateToTelegramEmoji(state agent.MessageState) string {
    switch state {
    case agent.MessageQueued:    return "⏳"
    case agent.MessageSubmitted: return "🔄"
    case agent.MessageDone:      return "✅"
    }
    return ""
}
```

跟 `internal/channel/feishu/adapter.go::mapStateToFeishuEmoji` 对位，但有两个本质差异：

1. **Emoji 形态**：Telegram reaction 接受 unicode codepoint（用户消息上直接显示 ⏳/🔄/✅）；Feishu 用预定义 `emoji_type` 名（`OneSecond`/`OnIt`/`DONE`），因为 Feishu reaction 服务拒绝 unicode 输入（返回 `99992354 data not found`，见 [feishu.md §6.6.3](./feishu.md)）。
2. **MessageDropped**：Telegram 当前不映射（silent drop），跟 Feishu 对齐 —— 失败由 reply 文本的 ❌ 前缀表达，不在 user-message reaction 上叠加 ❌。

未知 state 返回 `""` 让 caller silent drop，跟 Feishu 的 forward-compatible 行为一致。

### 14.3 字段删除决策（2026-08-22 fix-telegram）

`messages.MessageStatePayload.Emoji` 字段**已删除**。

- **删除理由**：该字段原本设计为 runtime 半成品 emoji，但 `runtime/eventbus.go` 唯一一处生产 `MessageStatePayload` 的代码不填该字段（只设 `State` / `MessageID`）；同时 `mapStateToTelegramEmoji` / `mapStateToFeishuEmoji` 都是 Channel 自治的，从不读 `Emoji` 字段。该字段在 production 路径上是纯传输浪费。
- **新的契约**：runtime 只 forward `agent.MessageState` 抽象枚举；每个 Channel adapter 自维护 state→emoji 映射函数（不依赖 payload 字段）。
- **wire format 兼容性**：`MessageStatePayload` 是 `internal/` 内部类型，无对外 wire format 暴露，删除零影响。
- **反向回归保护**：如果未来有人重构加回 `Emoji` 字段并期望 adapter 用它，编译会直接报错（telegram `adapter.go` 不再读该字段 + 测试 fixture 已用 `State` 字段）。

### 14.4 幂等（避免 Telegram API 抖动）

Adapter 维护 `messageStates map[userMsgID]agent.MessageState`：

- 同 state 第二次 emit 跳过 `setMessageReaction` 调用，节省 API 配额。
- **关键陷阱**：`agent.MessageState` 的零值是 `MessageQueued`，跟首次合法 emit 重合 —— 必须用 `bool ok` 区分"未记录"和"记录了 MessageQueued"。`lastMessageState` 返回 `(state, ok)`，`bool ok` 是 load-bearing（见 `adapter.go::lastMessageState` 注释）。
- **OutMessageStateRemoved 不更新 LRU**：删除态只调 `setMessageReaction(reaction: [])`，不调用 `rememberMessageState`。这保证后续 `OutMessageState` emit 仍按真实最后渲染状态判等，不会被一个 sentinel 污染。

### 14.5 append-only 语义

Telegram reaction API 是 append-only：`setMessageReaction` 每次用新列表**整体替换** reaction set，但不支持按 emoji id 删除单个 reaction。

- ⏳ → 🔄 → ✅ 在用户消息上**累积**为三个独立 reaction，形成完整状态轨迹。
- Telegram 每条消息允许 11+ 种 reaction 类型，4 态未超上限。
- 未来需要"删单个 reaction"：跟 Feishu 一样实现 `OutMessageStateRemoved` 携带 `ReactionID`，调 `setMessageReaction(reaction: [])`（Telegram 不支持按 ID 删，只能整体替换 —— Telegram 实际只有"全清"语义）。

### 14.6 ChatSession 侧契约

- `chatsession.EmitMessageState(userMsgID, state)`（`internal/chatsession/chatsession.go`）发布 `MessageStateEvent` 到 `cs.MessageStateBus`。
- `internal/runtime/eventbus.go` 的 `MessageStateBus` subscriber（每个 ChatSession 一个）把 `MessageStateEvent` 翻译为 `OutboundMessage{Kind: OutMessageState}` 并通过 `em.Send` 发出。
- Adapter case 在 `Send` 里消费，跟 Feishu 对位：判 emoji → 判 dedup → `strconv.Atoi(MessageID)` → `setMessageReaction` → `rememberMessageState`。

runtime 侧零修改（emoji 决策完全 Channel 自治）；Channel 侧只动 telegram（feishu 原本就走 `mapStateToFeishuEmoji(state)` 自决路径，未受字段删除影响）。

### 14.7 测试契约

`internal/channel/telegram/adapter_test.go` 锁死的契约：

| 测试 | 锁死什么 |
| --- | --- |
| `TestMapStateToTelegramEmoji` | 4 态映射 + `MessageDropped` silent drop + 未知 state silent drop |
| `TestAdapter_Send_OutMessageState_QueuedRenders` / `_SubmittedRenders` / `_DoneRenders` | 每个非空映射都打到 `setMessageReaction` + reaction 字段正确 |
| `TestAdapter_Send_OutMessageState_UnknownStateDrops` | 未知 state 不调 API |
| `TestAdapter_Send_OutMessageState_DroppedSilentDrops` | `MessageDropped` 显式不渲染（防未来"加 ❌"误改） |
| `TestAdapter_Send_OutMessageState_TracksStateIdempotency` | 同 state 第二次 skip + 不同 state 第三次触发 |
| `TestAdapter_Send_OutMessageState_FirstReceivedNotSkipped` | 第一次 emit 不被零值误判为已记录（lock 住 `bool ok`） |
| `TestAdapter_Send_OutMessageStateRemoved_DoesNotPolluteLRU` | Removed 不污染 LRU：后续不同 state 仍触发 |
| `TestAdapter_Send_OutMessageState_BadID` | 非数字 MessageID 报错，不污染 LRU |

## 15. 已知限制 / Gap（截至本次实现）

下面这些是文档里讨论过、Telegram Bot API 的能力差距或实现优先级选择导致没做的点。每条都标明属于哪一类：

| 类别 | 说明 |
| --- | --- |
| **限制** | Telegram Bot API 本身不支持，靠 adapter 怎么写都做不到 |
| **降级** | 飞书有原生能力、Telegram 没有对应物，已用近似手段实现 |
| **未实现** | 设计上想做、但目前没实现（不在本期 scope） |

### 15.1 限制类（API 做不到）

#### L1. 没有"卡片"概念，Telegram 不支持结构化 card 元素

- 飞书用 `<div>` / `<form>` / `<hr>` 等元素构成 receipt card，可以原位 append 多条 log entry。
- Telegram 只能 `editMessageText` 整体替换文本，不能 append 单条 log entry。
- 后果：长回复（一个 turn 100+ 行）会变成 Topic 内 100+ 条独立消息。
- 缓解：所有 reply 已经在 Topic 内（不污染主窗口），用户可折叠 Topic；设计上接受了这个 trade-off。
- 未来替代方案见 14.3 未实现类（receipt-on-edit）。

#### L2. `editMessageText` 整体替换，48 小时内有效

- Telegram 没有 "append to existing message" 语义。所有"原位更新"都是替换全部文本。
- 每次 edit 都需要重新发送**全部**历史文本（如果想保留之前的内容）。
- 单条消息文本上限 4096 字符。
- 文本消息编辑受 48 小时限制（`editMessageReplyMarkup` / `editMessageMedia` 无限制）。

#### L3. callback_data 64 字节限制

- 已用 `shortID(req[:8] + "-" + req[len-8:])` 应对，完整 RequestID 走 state store 反查。
- 但如果 RequestID 数量爆炸增长，shortID 可能碰撞（8+8 hex = 16 字节，碰撞概率按 1/2^64 估算，安全）。

#### L4. ForceReply 仅对下一条用户消息生效

- 用户发了别的消息后，force_reply 自动失效。
- 多问题向导场景下，如果用户在 ForceReply 期间发了不相关的消息，ForceReply prompt 会沉默失效。
- 缓解：handler 内检查 `pendingInput` 状态，未匹配则当普通消息处理。

#### L5. 没有 `reply_in_thread` 等价物

- 飞书 reply_in_thread 把消息收到 thread drawer，主消息流只剩 1 条气泡。
- Telegram 的 `reply_to_message_id` 只在视觉上"引用"，消息本身仍然显示在 Topic 主消息流。
- 后果：bot 收到用户消息后的所有 OutReply 都堆在 Topic 时间线上，无折叠效果。
- 设计上靠 Topic 自身隔离来替代。

#### L6. 没有 markdown 原生支持，只能用受限 HTML 子集

- Telegram 只支持 `<b>` `<i>` `<u>` `<s>` `<strike>` `<del>` `<code>` `<pre>` `<a href>` `<tg-spoiler>` 这几个标签。
- Markdown 表格、复杂布局、颜色、字号全部不支持。
- 已实现 `RenderMarkdown`（标题/列表/代码块/链接/粗体/斜体/spoiler/blockquote/表格/水平线/HorizontalRule）+ HTML 转义。
- 但**颜色**没有替代（飞书可用 `<font color="grey">`），用 emoji + 粗体近似。

#### L7. 没有"原生 task list" / checklist 元素

- 飞书 receipt 用 `<checkbox>` 元素。
- Telegram 只能用文本 `[x]` / `[ ]` / `[~]` 模拟。无法点击切换。
- 后果：用户不能在 Telegram 内更新 task 状态，必须等下一个 OutTaskUpdate 自动重发。

#### L8. 没有 "Mini App form" 原生输入控件（除 ForceReply）

- 飞书的 form 可以让用户在卡片内填多个字段一次提交。
- Telegram 只支持 ForceReply（单次单字段）+ Web App（要 URL、要 HTTPS、自行实现）。
- 后果：复杂多字段输入（如"输入仓库名 + 分支名"）要走两轮：先 option 选择字段类型，再 ForceReply 输入内容。

#### L9. `editForumTopic` 只能改名称/图标，不能改"正文"

- Telegram Forum Topic 本身没有消息正文，`editForumTopic` 只接受 name + icon_custom_emoji。
- 这意味着 Topic 永远是"空容器"，永远要靠内部的占位消息表达"会话状态"。
- 已确认无替代方案。

#### L10. Telegram reaction update 不带 `message_thread_id`

- `MessageReactionUpdate`（`setMessageReaction` 触发的 👍/✅/🔄 等 emoji 反应）只携带 `chat.id` 和 `message_id`，没有 `message_thread_id`。
- 后果：topic 内的 reaction 永远路由不到该 topic 的 ChatSession（chatID 不带 thread 后缀），只能路由到 chat-level ChatSession。
- 实际影响：gtw drafts 存在 topic 内时（`tg_<chatid>:<thread_id>`），用户给 topic 内 message 加 ✅ reaction **无法触发** gtw draft 处理。DM 和群主窗口的反应（无 thread）正常工作。
- 修法（2026-08-22）：chat-level chatID 现在统一 namespaced（`tg_<chatid>`），DM / 主窗口 emoji reactions 能正常进 gtw draft 流程。Topic 内的 emoji reactions 仍是平台能力限制。

### 15.2 降级类（用近似手段实现，已 work）

#### D1. OutReply / OutResult / OutThinking / OutTool 都用独立消息

- 飞书 receipt 把这些都装在一张 card 内，通过 div 元素结构区分。
- Telegram 没有等价物，每条 OutReply / OutTool / OutThinking 都是独立消息。
- 视觉区分靠 emoji 前缀：`💭` thinking / `🔧` tool / `✅` tool_end / `📝` result。
- Topic 内的可读性靠消息时间线排序，不靠布局结构。

#### D2. OutError 渲染为带 ⚠️ 标题的纯文本消息

- 飞书 `encodeErrorCard(title, body)` 用红色 card 元素 + 标题。
- Telegram 没有红色 card。降级为 `⚠️ <error title>\n\n<body>\n\n<pre>stderr tail</pre>`。

#### D3. OutCommandReply 走纯文本，不进入 markdown 渲染

- 飞书走独立顶层 Create 通道。
- Telegram 同样 sendMessage 但跳过 `RenderMarkdown`（slash 命令输出已是纯文本，再渲染一遍会有 `<` `>` 转义问题）。

#### D4. `addReaction` / `deleteReaction` 都用 `setMessageReaction`

- Telegram 没有"删除单个 reaction"的 API，"删除"通过 `reaction: []` 实现。
- Adapter 层在日志上区分意图，但 Telegram 看到的是同一个 API 调用。

#### D5. OutHeartbeat 走占位消息 PATCH

- 飞书有 receipt header 区域专门承载 heartbeat，PATCH 时不影响 log entries。
- Telegram 占位消息就是"Working..."那一条，PATCH 直接改文本（替换 thinking/tool 计数）。
- 缺点：占位消息上**不能**同时显示工作状态和历史 thinking/log（飞书可以分层）。
- **§18 plan-D 改进**：占位 PATCH 文本由 `[status line] + ──────── + StatusBar` 三段组成，每条 OutHeartbeat 都重新拼接 footer；用户一眼看到当前 turn 的 identity / usage / git。详见 §18.4。

### 15.3 未实现类（设计意图存在，本次没做）

#### N1. Rolling-log receipt card

- 飞书 F-25/F-40：长回复 PATCH 同一张 card，多 div 元素累积。
- Telegram 技术上能做（一条 message + 多次 `editMessageText`），但每次 edit 都重发全部历史。
- **本期决定不实现**，因为：
  1. Topic 内已经隔离 reply 流（核心价值"主窗口不污染"已达成）
  2. 实现复杂：需要 adapter 内维护 full-text history、4096 char 限制下需要 overflow rollover 策略
  3. 视觉收益相对 Topic 已经带来的隔离较小
- **未来重做评估**：如果用户反馈"Topic 内 reply 太多看不清"再回来做。届时需要：
  - 一个 receipt message ID per turn
  - 全 history 字符串拼接 + overflow 检测
  - 与 OutThinking / OutToolStart / OutToolEnd / OutHeartbeat 的整合规则

#### N2. `pendingHeartbeats` 缓冲（feishu F-63.1）

- 飞书：在 receipt 还没创建前缓存 heartbeat snapshot，receipt 一旦创建立即应用。
- Telegram 当前实现在 C2（C2 已实现 OutHeartbeat 自动创建 placeholder）。但**没有 buffer**：第一次 OutHeartbeat 创建占位并 PATCH，后续 OutHeartbeat 走 PATCH。
- **缺失的部分**：如果在 placeholder 创建**之前**就有心跳进入且创建失败，没有重试机制。
- **缓解**：placeholder 创建失败时 OutHeartbeat 走 fallback（发独立消息），不丢数据。

#### N3. OutResult 之前显示 `✅ 完成` reaction

- 飞书：OnPromptEnded 给 receipt 加 ✅ reaction，**不**编辑文本。
- 当前 Telegram：OnPromptEnded 把占位消息文本改成 `<b>✅ Completed</b>`。
- **设计意图改用 setMessageReaction 实现**，本次没改，留待后续。
- 替换理由：当前实现把占位文本改成 "Completed" 后，下一次 turn 开始时无法回退到 "Working..."。

#### N4. Orphan reply fallback（feishu `postOrphanReplyCard`）

- 飞书：SendCard 失败时降级到顶层 Create。
- Telegram：sendMessage 失败 → retry 3 次 → 仍失败就返回 error，runtime 看到 error。
- **缓解**：已有 retry 层兜底 transient 错误；如果用户配置 bot 权限问题（terminal 错误）确实无解。
- **未来**：可以加"orphan fallback"用 `chat_id` 直接发（绕过 topic_id），但当前实现的 retry 已经覆盖 90% 场景。

#### N5. 编辑消息用 `msg.ReplyTo` 作为锚点

- 飞书：OutReply 携带 `msg.ReplyTo`（user message id），receipt 锚定到该用户消息。
- Telegram：Send() 当前完全忽略 `msg.ReplyTo`。
- **影响有限**：Telegram Topic 本身就是 scope，不需要锚定到 user message。`reply_to_message_id` 在 Topic 内也只起视觉引用作用，不影响消息流。
- **未来**：如果要做"reply-only"模式（即只回复某条用户消息但不开新 bubble），可以用 `reply_to_message_id` 实现。

#### N6. 心跳 header 的 agent identity 注入（session_id / model / agent_name）

- 飞书：receipt header 行有 "Agent · Model · Session"。
- Telegram 当前 OutInit 已 silent drop（见 C1），占位消息上也没有 header。
- 状态丢失：用户进入 Topic 后看不到当前 turn 的 session 标识。
- **未来**：占位消息 PATCH 时把 SessionID / Model / AgentName 拼到 heartbeat 文本头部。

### 15.4 实现优先级建议

如果未来要做 follow-up，建议按这个顺序：

1. **N3**（OnPromptEnded 用 reaction 而非改文本）—— 1 行改动，恢复力强。
2. **N6**（占位 header 加 session identity）—— 小改，提升可观测性。
3. **N4**（orphan fallback）—— 中等改，覆盖极端场景。
4. **N2**（pendingHeartbeats）—— 中等改，但 C2 已经覆盖大部分场景。
5. **N1**（rolling-log receipt）—— 大改，需要权衡 4096 限制 + 历史重发成本。
6. **N5**（用 ReplyTo）—— 视觉改进，影响小。

### 15.5 本次实现完成（C1, C2）

| ID | 内容 | 状态 |
| --- | --- | --- |
| C1 | OutInit silent drop（与 feishu F-44 对齐） | 已实现 |
| C2 | (v9 P1 2026-08-23 移除) OutHeartbeat 路径 ensurePlaceholderForHeartbeat 懒创建 —— handleMessage 的 ensurePlaceholder 已 eager 同步预先建好,OutHeartbeat 不需要再二次确认 | **已下线** |
| C3 | 2026-08-22 plan-C：DM / 主窗口 placeholder + reply chain；详见 §11.11 | **v7.1 修订**（v7 + codex review race guard；2026-08-23 v9 P1 改为 Compose header-skip rule §11.12.5.1,原 lazy path 整条删除） |
| C4 | 2026-08-22 reaction chatID namespacing（修 `handleMessageReaction` 用 raw chatID 导致 emoji reaction 进不了 gtw 的 bug） | 已实现 |
| C5 | 2026-08-22 stateStore TTL on-load prune（30 天未活动 topic 自动清理） | 已实现 |
| C6 | 2026-08-22 sendChoice / patchChoice / handleInputClick / handleForceReply / callback wizard editMessageText 把 session ChatID 传给 Telegram API 的生产路径 bug（修 `rawChatIDFromSession` helper） | 已实现 |
| C7 | 2026-08-22 plan-D StatusBar 全量贴附：抽 `internal/statusbar` 共享包，feishu adapter 切到 `statusbar.StatusBarLines`，telegram adapter 所有 text 出口拼 StatusBar trailer，占位 PATCH 也带 footer，详见 §18 | 已实现 |

后续修订请直接在本节追加，并把对应 issue 编号填进去。

## 16. Telegram 独有、未利用的能力

下面这些 API 飞书**没有**对应物，Telegram 原生支持但当前 adapter 没有用。每条标出"对应飞书体验"和"启用后能补齐哪个 gap"，便于后续讨论优先级。

### 16.1 P1 - `pinChatMessage` 钉住最终结果

**能力**：`pinChatMessage(chat_id, message_id)` 把任意消息钉在 Topic（或群组）顶部。

**对应飞书体验**：飞书没有 pin 概念。

**补齐的 gap**：

- 用户痛点：长 Topic 内回复滚动后，用户很难找到 OutResult 的最终答案。
- 启用后：每个 turn 结束后自动把 OutResult 消息 pin 在 Topic 顶部；新一轮开始时 unpin 旧 result。

**当前为什么没做**：

- 14.3 N1（rolling-log receipt）的替代方案：如果不做 receipt，pin 是次优选择 —— 视觉上不那么"集成"，但用户能找到结果。
- 14.3 N3（OnPromptEnded 用 reaction 而非改文本）的延伸：可以在 ✅ reaction 之外再叠加 pin，提升发现性。

**工作量估算**：~15 行（OutResult 后调 pin；新一轮 unpin 旧 result）。

**风险 / 边界**：

- pinChatMessage 调用也有速率限制（每个 chat 5 次/min）。
- 多 Topic 同 chat 时，pin 在 chat 级别可见，跨 Topic 共享 pin 位 —— 可能造成 pin 抖动。
- 解决：每个 chat 只 pin 当前 turn 的 result，unpin 之前的。

### 16.2 P2 - `deleteMessage` 删除占位消息

**能力**：`deleteMessage(chat_id, message_id)` 删除任何消息（48 小时内）。

**对应飞书体验**：飞书 receipt 不能"删除"（会丢上下文）。

**补齐的 gap**：

- 14.3 N3 的"Topic 流更干净"版本：OnPromptEnded 时删占位，配合 ✅ reaction 作为完成标识。
- 14.3 D5 的扩展：占位消息不再需要"原地切换 Working ↔ Completed"。

**当前为什么没做**：

- 删除消息看起来"激进"，用户可能依赖占位消息作为会话时间线锚点。
- 飞书没有等价操作可对比，没经验数据。

**工作量估算**：~10 行（OnPromptEnded 时调 deleteMessage 替代 editMessageText）。

**风险 / 边界**：

- 删除后用户没法"回到这条占位看历史"——但 Topic 内其他消息仍然是时间线。
- 必须配合 ✅ reaction（不能既删又没标识）。

### 16.3 P3 - OnPromptEnded 用 ✅ reaction + delete placeholder 组合

**能力**：组合 P2 + setMessageReaction("✅")。

**对应飞书体验**：飞书 receipt 不删但加 ✅ reaction。

**补齐的 gap**：

- 14.3 N3 的最佳实现：视觉上看到 ✅（reaction）+ Topic 流不堆 "✅ Completed" 文本。

**当前为什么没做**：见 14.3 N3。

**工作量估算**：~15 行。

**风险 / 边界**：同 P2。

### 16.4 P4 - `editMessageReplyMarkup` 只更新键盘

**能力**：`editMessageReplyMarkup(chat_id, msg_id, keyboard)` 单独更新消息的 keyboard，**不**改 text。

**对应飞书体验**：飞书 PATCH card 必须整体替换 schema。

**补齐的 gap**：

- 当前实现每次点 button 都 `editMessageText(text + keyboard)`，触发 markdown 重新渲染（耗时、可能引入渲染差异）。
- Choice settle 时只更新 keyboard（移除其他按钮）应走 editMessageReplyMarkup，不动 text。

**当前为什么没做**：

- 当前实现是 `editMessageText(text, keyboard)`，简化实现。
- 性能影响在小流量下看不出来，未做 profile。

**工作量估算**：~20 行（patchChoice settle 路径改用 editMessageReplyMarkup）。

**风险 / 边界**：

- 没发现 Telegram API 差异。
- 收益主要是性能（少一次 markdown parse）和一致性（不会因为 markdown 渲染规则变化导致已 settle 的 choice 文本微变）。

### 16.5 P5 - `sendMediaGroup` 批量附件

**能力**：`sendMediaGroup(chat_id, media[])` 一次发送最多 10 个 media（photo/video）作为**一条**消息的相册。

**对应飞书体验**：飞书 upload_file 可以批量（im.message.batch_send），但实现细节不同。

**补齐的 gap**：

- 当前附件下载 + 上传：每个图片/视频单独 `sendPhoto` / `sendVideo`，多附件变成多条消息。
- 多张图体验差：用户收不到"相册视图"，需要滑动。

**当前为什么没做**：

- 当前 attachments.go 按 1 个 media 1 条消息处理。
- 实现 batch 需要改 attachment pipeline（流式而非全部加载后批量）。

**工作量估算**：~30 行（attachment 收集路径 + sendMediaGroup 调用）。

**风险 / 边界**：

- sendMediaGroup 不支持 caption（caption 必须是 media[0] 的 caption，其他 media 无 caption）。
- 混合类型（photo + video）OK，但 document 不能混在 media group 里。
- 如果用户发的是 11+ 张图，需要 fallback 成多条 media group 消息。

### 16.6 P6 - `unpinAllChatMessages` 清理历史 pin

**能力**：`unpinAllChatMessages(chat_id)` 清空 chat 内所有 pin。

**对应飞书体验**：飞书没有 pin。

**补齐的 gap**：

- Bot 维护时（升级、迁移）可能留下过时 pin。
- 单元测试 / 集成测试需要在每个 case 之间清理 pin。

**当前为什么没做**：产品需求不明确。

**工作量估算**：~5 行（暴露为 adapter method + 测试 helper）。

### 16.7 P7 - `sendChatAction` 显示 typing 状态

**能力**：`sendChatAction(chat_id, action="typing")` 在 chat 内显示"bot 正在输入..."指示。

**对应飞书体验**：飞书 SDK 内置 typing indicator。

**补齐的 gap**：

- 当前用户发完消息后，bot 处理期间 Topic 内**无任何反馈**直到第一条 OutReply 或 OutHeartbeat 到达。
- typing 状态让用户立即知道"bot 收到了，正在处理"。

**当前为什么没做**：

- typing 状态默认 5 秒过期，需要持续刷新（每 4-5 秒重发）。
- LLM turn 通常 < 5s 时不需要，但长 turn（>10s）用户体验差。
- 之前没考虑过补这个细节。

**工作量估算**：~25 行（adapter 持有 typing goroutine；在 OutReply 第一次到达时停止）。

**风险 / 边界**：

- typing 不能跨 Topic（typing 显示在 chat 级别，不是 topic 级别）—— 用户在 main window 也会看到。
- 频率限制：typing 调用本身也吃 API 配额（每个 chat 1 次/5s）。
- 如果 chat 不是 forum，typing 显示在 main window 会让用户觉得 bot 在 main window 回复 —— 实际只是 feedback。

### 16.8 P8 - `sendPoll` 作为 Choice 的另一种渲染

**能力**：`sendPoll(chat_id, question, options[])` 发一个 poll。

**对应飞书体验**：飞书 AskUserQuestion 用 `<select>` form components。

**补齐的 gap**：

- 飞书 AskUserQuestion 用 form 让用户填答案。
- Telegram 可以用 sendPoll 实现"让用户选 1-N 个选项"——但**不是 1:1 等价**：
  - sendPoll 选项数不限
  - 选项是文字标签
  - 用户选完后 poll 自动 settle
  - 但选项 ID 跟 ChoiceOption.ID 的映射需要 adapter 自己做
  - 不能做"输入自定义答案"（除非 poll 之外再补 ForceReply）

**当前为什么没做**：

- 已经用 InlineKeyboardMarkup 实现了 Choice，体验够用。
- sendPoll 是**可选替代**，不是必须。

**工作量估算**：~80 行（sendPoll 作为 OutChoice 的另一种渲染 + 监听 poll_answer update）。

**风险 / 边界**：

- sendPoll 投完票后自动 settle，bot 收 `poll_answer` update —— 需要处理新的 update 类型。
- 选项不能像 InlineKeyboard 那样灵活（不支持 emoji icon、不支持 URL 等）。

### 16.9 优先级建议

按 ROI 排序（参考 14.4）：

| 优先级 | 项 | 工作量 | 价值 | 理由 |
| --- | --- | --- | --- | --- |
| 1 | **P3** reaction + delete placeholder | ~15 行 | 高 | 替换 N3，最干净的实现 |
| 2 | **P1** pin OutResult | ~15 行 | 高 | UX 提升大（长 Topic 内找答案） |
| 3 | **P7** typing indicator | ~25 行 | 中 | 长 turn 反馈 |
| 4 | **P4** editMessageReplyMarkup 单独更新 | ~20 行 | 低 | 性能优化 |
| 5 | **P5** sendMediaGroup | ~30 行 | 低 | 少见场景 |
| 6 | **P2** deleteMessage（仅作为 P3 子步骤） | ~10 行 | 中 | 已在 P3 中 |
| 7 | **P6** unpinAllChatMessages | ~5 行 | 极低 | 测试维护 |
| 8 | **P8** sendPoll 替代 Choice | ~80 行 | 中 | 可选替代方案，争议大 |

### 16.10 与已有 gap 的关系

| Telegram 能力 | 补齐的 gap |
| --- | --- |
| P1 pin | N1（rolling-log 替代方案） |
| P2 delete | N3 变体 |
| P3 reaction+delete | N3 最佳实现 |
| P4 edit markup | D1（thinking/tool 独立消息优化） |
| P5 media group | 附件流程优化 |
| P7 typing | 用户反馈体验（不在 §15 内） |

后续讨论时优先关注 P1/P3/P7（性价比最高）。

## 17. 网络代理

Telegram Bot API 在某些网络环境（如中国大陆）下不可达。nightme **默认继承** 标准代理环境变量，无需任何配置。

### 17.1 支持的环境变量

| 变量 | 作用 |
| --- | --- |
| `HTTP_PROXY` | HTTP 请求的代理 URL（如 `http://127.0.0.1:7890`） |
| `HTTPS_PROXY` | HTTPS 请求的代理 URL（对 `api.telegram.org` 生效） |
| `NO_PROXY` | 不走代理的域名/网段（逗号分隔） |
| `ALL_PROXY` | 兜底代理，HTTP_PROXY/HTTPS_PROXY 未设置时生效 |

Go 标准库的 `http.ProxyFromEnvironment` 自动读取这些变量，无需 nightme 介入。

### 17.2 使用示例

**Clash / Surge（mixed-port 模式，HTTP 代理）**：

```bash
export HTTPS_PROXY=http://127.0.0.1:7890
nightme start --channel=telegram
```

**v2ray / shadowsocks（SOCKS5 代理）**：
SOCKS5 代理无法通过环境变量直接配置，需要在系统层做透明代理转发。
或者用 `proxychains` / `tsocks` 之类的工具包装 nightme：

```bash
proxychains4 nightme start --channel=telegram
```

**不走某些域名**：

```bash
export NO_PROXY=localhost,127.0.0.1,*.internal
nightme start --channel=telegram
```

### 17.3 内部实现

所有 outbound HTTP 请求统一走 `internal/httpclient` 包：

```go
import "github.com/cnlangzi/nightme/internal/httpclient"

client := httpclient.Default()           // 45s timeout, proxy from env
client := httpclient.DefaultWithTimeout(10*time.Second)
```

该包封装的原则：

- 唯一职责：把 `&http.Client{Timeout: ...}` 集中到一个地方，避免散落重复
- 默认行为：复用 `http.DefaultTransport`（已经指向 `http.ProxyFromEnvironment`）
- 不做 retry / rate limit / logging —— 这些由调用层组合（参考 `internal/channel/telegram/retry.go` 和 `ratelimit.go`）

被改造的位置：

- `internal/updater` —— 检查 GitHub release
- `internal/version` —— 检查 nightme.dev 版本
- `internal/bridge/dsh/host` —— dsh 主机 RPC
- `internal/channel/telegram` —— Telegram Bot API
- `internal/login/telegram` —— login 时的 `getMe` 校验

### 17.4 不暴露代理配置的原因

不提供 `cfg.Telegram.ProxyURL` 之类的配置项，原因：

- 代理需求来自环境（用户机器的网络），不是配置决策
- 配置项会被各种 secret 管理工具、CI/CD 流水线暴露在 diff 里
- 环境变量是 OS-level 的标准机制，工具链（Docker、Kubernetes、systemd）都支持

## 18. StatusBar 全量贴附（2026-08-22 plan-D）

Telegram 没有"card"结构（见 §15 L1），"rolling-log card"模式（feishu F-25 / F-40）做不到 —— 同一张消息上 append 多条 log entry，本质依赖飞书 `<div>` 累积结构。Telegram 的 `editMessageText` 是整体替换 + 48h 限制 + 4096 字符上限，无法承载 turn 长 content。

**结论**：每条发出的文本消息都附 StatusBar trailer，而不是依赖单一占位或卡片。

### 18.1 字段与渲染

StatusBar 三行由 `internal/statusbar.StatusBarLines(&msg)` 生成，从 `OutboundMessage` flat 字段读取：

```text
Line 1: 🤖: AgentName · Model · SessionID       (Identity)
Line 2: 💰:「 new / cache / out · X% (window) · $cost 」   (Usage)
Line 3: 📁: ws · ⎇ branch · + N · − N · ± N · ? N · ! N · ⇡ N · [#PR](url)   (GitStatus)
       非 git workspace（cwd 不在 repo / git 不可用 / CollectGit 超时）:
         📁: ws
```

每行 zero-omit（F-45 §1.6）。整行字段全空 → 该行不渲染。Line 3 是唯一例外：Workspace 已设但 git 没产出 snapshot 时仍渲染 `📁: <ws>`（仅 workspace，无 git 段），确保用户始终能看到 agent 的工作目录。StatusBar 完全为空 → 不发 panel，只发 body。

Telegram adapter 用 `statusbar.RenderPanel(lines)` 把三行包成 **chevron-tail ASCII frame marker**（左 `┌` / `└`，右 `›`，无 `│` 侧栏）：

```text
[message body]

┌─────────────────────────────›
  🤖: claude · MiniMax-M3[1m] · 61c4ec9d-dbb0-418c-bbe7-8d4bfbc1a135
  💰:「 31.1k / 128 / 37 · 3.1% (1M) · $0.157 」
  📁: cnlangzi/nightme · ⎇ main
└─────────────────────────────›
```

**保守设计（Android 折行修复，迭代 30 → 15 → 8 → 16）**：

迭代历史：

1. **30 字符** —— iOS 安全，Android 实测折行
2. **15 字符** —— Android 仍折行
3. **8 字符**(`┌──────┐`)—— Android 不折行但太稀疏（"分隔栏的字符太短了"）
4. **16 字符**(`┌──────────────›`)—— 当前；Android chat bubble 仍安全，视觉有 frame 感(2026-08-22 user feedback)

当前 `PanelMaxWidth = 16`，bars 形如：

```text
┌──────────────›
  🤖: claude · opus-4-5 · 61c4ec9d-dbb0-418c-bbe7-8d4bfbc1a135
  💰:「 31.1k / 128 / 37 · 3.1% (1M) · $0.157 」
  📁: cnlangzi/nightme · ⎇ main
└──────────────›
```

**右收口设计**（2026-08-22 user feedback）：

Bars 左端是 `┌` / `└`（方角，"从此处开始"），右端是 `›`（chevron tail，"向右继续"）。**不**用闭合的 `┐` / `┘`，因为 StatusBar 内容可能向右延伸超出 bars，硬闭合会暗示一个不存在的边界。`›` 是 CLI / 编辑器 fold-marker 通用约定，传达"信息从这里流向右边"的延续感。

内容行原样输出（每行前缀 2 空格做左边栏留白），**不截断** —— 长 StatusBar 行（如 Identity 带完整 session ID ~50 字符）可延伸超出栏宽，在窄屏自然换行。无 `│` 侧栏 —— 内容换行不受 panel 几何约束。

完整契约 / 测试在 `internal/statusbar/statusbar_test.go`（从 feishu F-45 §1.6 的 `usage_footer_test.go` 迁移，15 个 `StatusBarLines` 测试 + 5 个 `RenderPanel` 测试）。

### 18.2 贴附规则

| Kind | 行为 |
| --- | --- |
| `OutReply` / `OutResult` / `OutCommandReply` / `OutThinking` / `OutTaskCreate` / `OutTaskUpdate` | `body + "\n\n" + statusbar.RenderPanel(lines)`，整体走 `RenderMarkdown`（`<b>` `<code>` `<a>` 受限 HTML 子集；panel 边框是 box-drawing 字符不走 HTML parser） |
| `OutToolStart` / `OutToolEnd` | `formatTool(msg)`（🔧/✅ prefix + tool name + args/output）+ `RenderPanel` trailer |
| `OutError` | `body + "<pre>" + escapeHTML(StderrTail) + "</pre>"` + `RenderPanel`（raw 拼接，**不走** RenderMarkdown，因为预 escape 的 `<pre>` 标签会被 RenderMarkdown 当字面量再次 escape —— 与 panel 同理手工 stitch） |
| `OutHeartbeat`（占位 PATCH） | `status line + "\n\n" + statusbar.RenderPanel(lines)`，整体走 RenderMarkdown |
| `OutChoice` / `OutChoicePatch` | **不挂**（InlineKeyboard 自含，挂 footer 污染选择 UI） |
| `OutMessageState` / `OutMessageStateRemoved` | **不挂**（reactions 独立轨道，§14.1） |
| `OutInit` | **不挂**（silent drop，F-44 对齐） |

### 18.3 无 cache —— pure consumer 契约

`StatusBarLines(&msg)` 是 `internal/statusbar` 的纯 renderer，**不持有任何 state**：

- `Identity`（AgentName / Model / SessionID）：由 `MessageStateBus` subscriber 在 dispatch 路径 stamp 到 OutboundMessage（F-44 / fix-placehold-card）
- `Usage`：由 bridge 在终态 OutResult 上填（Claude Code `result.usage + result.modelUsage`，Pi `message_end.usage`）；streaming 中间 chunk 该字段为 nil → `StatusBarLines` zero-omit Line 2（F-45 §1.6）
- `GitStatus`：由 chatsession 在 `SetSelectedCwd` / `/gtw commit` / `/gtw pr` 时刷，runtime 透传

到 `Send` 时，**该有的字段都已经填好**；空就是空（zero-omit 兜底，不发空 divider）。早期曾考虑过加 `lastStatusBar` 跨 chunk fallback 缓存，后来撤掉——理由是这等于在 channel 层帮 runtime 兜"忘填字段"的责任，违反职责边界。F-45 的 zero-omit + F-44 的 Identity stamp 已经覆盖所有"空字段"场景。

### 18.4 占位（PATCH）也带 footer

per-turn 占位的两步生命周期：

**Step 1：handleMessage 创建占位**（`ensurePlaceholder`）—— 文本是裸的 `<b>🤖 Working...</b>` (**v9 已不带 v7 plan-C 时代的 `· ⏱ HH:MM:SS` 冷启动后缀**,时间戳由首次 OutHeartbeat 的 `LastBeatAt` 段补上),**不含 footer**。原因:handleMessage 这一刻 OutboundMessage 还没生成,runtime 还没 stamp Identity / Usage / GitStatus —— 没东西可拼。等 runtime 出 OutMessageState / OutReply 时再决定 footer 内容。**v9 P1 附加**:banner 是否**绘制**取决于 §11.12.5.1 renderHeader 决策 —— 紧接着 slash command / OutError 等非 agent turn 的 body 内容落定时,banner 被 Compose 主动隐藏,转 body 单独渲染。

**Step 2：首次 OutHeartbeat PATCH 叠加 footer** —— `[status line] + \n\n---\n + StatusBar`（由 v9 chain 的 `renderActiveChunkBody` 拼装）。后续每条 OutHeartbeat 都重新拼接 footer：

```text
turn N：用户发 "hi N"
    └─ handleMessage 时刻 →  placeholder = "<b>🤖 Working...</b>"            (无 footer;无 v7 ⏱ 后缀)
    └─ 首次 OutHeartbeat PATCH →  "💭 0 · 🔧 0 · ⏱ HH:MM:SS ┌─…─›\n│<panel>│\n└─…─›"   (panel 落地)
    └─ 后续 OutHeartbeat        →  status line + RenderPanel
    └─ OutResult                →  "[result text] ┌─…─›\n│<panel>│\n└─…─›"   (独立气泡)
    └─ OnPromptEnded            →  不改 placeholder 文本，贴 🎉 reaction on placeholder
```

`ensurePlaceholder`(handleMessage 同步饿汉路径)走 `heartbeatText(nil) == "<b>🤖 Working...</b>"` 这个 cold-create header。**(v9 P1 2026-08-23 移除)** 原来同源说明的 `ensurePlaceholderForHeartbeat`(Send 入口的 lazy 路径)整条删除 —— handleMessage 先于 publish 的同步性质让 race guard 不再需要,文档此处精简为单条路径。两条路原本共用 `placeholderInitialText(now)` helper,`placeholderInitialText` 本身也已并入 `heartbeatText(nil)`(无时间戳) —— cold-create banner 形如 `<b>🤖 Working...</b>`,时间戳由 OutHeartbeat 第一个 patch 在 `LastBeatAt` 段补上。

### 18.5 OutError 的特殊处理

OutError 的 `<pre>stderr</pre>` 是 pre-escape 的合法 Telegram HTML 标签。但当前 `RenderMarkdown` 不识别已存在的 `<pre>` 标签，会作为字面量再次 escapeHTML，产生 `&lt;pre&gt;...&lt;/pre&gt;`。这是**预先存在**的行为（`internal/channel/telegram/render.go` 全局 escapeHTML 策略），跟 StatusBar 无关 —— 锁在 `TestAdapter_Send_DM_OutError_AppendsStatusBar`。

未来要让 stderr 显示为真正的 `<pre>` 块，需要绕过 RenderMarkdown 走 raw HTML 路径（参考 feishu `sendRawOutText`）；本次不动。

### 18.6 跟 feishu 的差异

| | feishu | Telegram |
| --- | --- | --- |
| StatusBar 载体 | Card footer（`<hr> + <div text_color="grey">`） | 文本 panel（`┌─…─›` + 三行 + `└─…─›`） |
| 同 turn 内 StatusBar 重复 | 否（footer 跟 card 一对一） | 是（每条消息都拼一次） |
| Streaming chunk 渲染 | PATCH 同一张 card 累积 div | 每条 sendMessage 独立气泡 + trailer |
| 编辑语义 | card 整体 PATCH（50 元素 / 30KB 上限） | 整体替换 text（4096 字符 + 48h 上限，见 §15 L2） |
| Edit 触发条件 | AppendEntry / RolloverTo | 每条 OutXxx 都触发 |

**为什么 Telegram 选择"重复"而非"折叠"**：无 card 元素 + 无原生 divider，只能重复。Topic 本身已经在做 turn 范围隔离（主窗口不污染，§11.7），trailer 的重复换来"每条消息自含上下文"的 UX 收益。

### 18.7 验收清单

- [x] `internal/statusbar/statusbar.go` export `StatusBarLines`，feishu adapter 5 处 `formatStatusBarLines` 调用全部切到 `statusbar.StatusBarLines`（`internal/channel/feishu/adapter.go`）
- [x] feishu `usage_footer.go` / `usage_footer_test.go` 已删除（按 `no-type-aliases` 不留薄壳）
- [x] telegram adapter Send switch 的 text 出口全走 `appendSegmentForKind`（v9 chain 路径；chain 内部调 `renderActiveChunkBody`）。`renderBodyWithStatusBar` 在 v9 chain 重写时被删除（footer 语义迁到 `chain.lastFooter` + `renderActiveChunkBody`）
- [x] `OutHeartbeat` 占位 PATCH 拼接 footer
- [x] `OutChoice` / `OutMessageState*` / `OutInit` 不挂
- [x] 无 cache —— `StatusBarLines(&msg)` 纯 consumer，零状态
- [x] 测试矩阵（15 个 case）：`TestAdapter_Send_DM_OutReply_AppendsStatusBar` / `OutResult` / `OutThinking` / `OutToolStart` / `OutToolEnd` / `OutTaskCreate` / `OutCommandReply` / `OutError` / `OutError_NoDiagnostic_AppendsStatusBar` / `OutHeartbeat_PATCHesPlaceholderWithStatusBar` / `OutReply_NoFieldsNoCache_NoTrailer` / `OutChoice_NoStatusBar` / `OutMessageState_NoTextChange` / `Topic_OutReply_AppendsStatusBar` / `OutReply_OutOrderPreservesCache`
- [x] feishu 16 个测试不变，迁移后零回归

### 18.8 已知限制

- StatusBar 在每条消息上重复，长 turn 会产生 N 份同形 footer。Topic 内可折叠视觉隔离缓解，但仍是平台 trade-off（§15 L5）
- OutError 的 `<pre>` 块被 RenderMarkdown 二次 escape（§18.5），修法 = 走 raw HTML 路径
- Markdown 表格 / 颜色 / 字号 / `<hr>` 仍不支持（§15 L6）
- StatusBar 本身走纯文本（emoji + 中点 `·` + 半角空格），没用 `<b>` `<code>` 强调（避免 OutError 那类 escape 边界），视觉不如 feishu grey footer，但 parse 零失败

## 19. 变更日志

- **2026-08-22（v9 chain rolling log）** - 引入 per-turn multi-chunk chain，替代 v4 / v8 的"单占位 + 独立 bubble"双轨制。完整 spec 见 §11.12。新增文件：`internal/channel/telegram/placeholder_chain.go`（chainKey / placeholderChain / placeholderChunk / chainLRU，含 `appendSegment` / `flushChainNow` / `scheduleFlushDebounced` / `getOrCreateChain` / `patchActiveHeader` / `activeChunkMessageID`）/ `internal/channel/telegram/summarize_tool.go`（从 feishu 平移，含 `formatToolStartCall` / `summarizeToolResult` / `displayToolArgs` / `compactJSONToolArgs` / `countLines` / `countUniqueFiles` / `truncate`）。改动：`Adapter.Send` 8 个 Out* case（OutReply/OutResult/OutThinking/OutToolStart/OutToolEnd/OutError/OutTaskCreate/OutTaskUpdate）重写为 `appendSegment` 路径；`OutHeartbeat` 改 `patchActiveHeader` + 走 debounce；`OnPromptEnded` 改 `flushChainNow` + 🎉 on active chunk + cursor reset；`formatTool` 内联实现替换为调 summarize helpers；`ensurePlaceholder` delegate 到 `appendSegment` 创建第一张 chunk。**未持久化**：`TopicState.PlaceholderChunkIDs`（本规划中曾计划加入，最终决定不写）；`buf` / `headerLine` / `lastFooter` 全部纯内存。`TopicState.PlaceholderMessageID` 保留为 read-only 兼容字段（不再写）。debounce window = 250 ms。LRU cap = 1000 chains。阈值三档：3500 chars raw buffer / 3900 chars rendered split / 4096 chars Telegram 硬限。Footer 内存语义：每 chunk 最多一个 footer，footer-bearing 事件（OutReply / OutResult / OutTaskCreate / OutTaskUpdate）来时刷新，其他不动。重启后 chain 失 = 下次事件来时建新 chunk（旧 frozen chunks 在 chat 里保留为历史证据）。

- **2026-08-23 (v9 P1) — banner-hide 修复 + 懒汉路径下线**。修复 v8 / v9 早期未解决的"非 agent turn 的 stale `🤖 Working...` banner 永不清除"问题。完整 spec 见 §11.12.5.1。
  - `chunkBody` 加 `hasHeartbeat bool` 字段 + `setHeaderFromHeartbeat(h)` 方法（同步翻 flag）。`Compose()` 改 renderHeader 决策：`renderHeader = hasHeartbeat || len(entries) == 0` —— entries 有内容但无心跳时跳过 header,banner 藏起来,让 body 独自渲染;entries 空时仍然画 banner（cold-create alive 反馈）;hasHeartbeat 一旦为 true 后续都画 header（agent 真在跑）。
  - `Adapter.patchChainHeader` 真分支从 `chunk.setHeader(...)` 改为 `setHeaderFromHeartbeat(...)`,翻 flag。cold-create / chain rotation / OutHeartbeat 兜底分支保持 `setHeader` 不动 flag。
  - **懒汉路径整条删除**：`Send()` 入口去掉 `placeholderAnchor, placeholderErr := ensurePlaceholderForHeartbeat(...)` + 错误日志块;`OutHeartbeat` case 简化为 `return a.patchChainHeader(...)`(去掉 `if placeholderAnchor > 0` race guard,因为 handleMessage 已 eager 预先建好,无 race 可言);`ensurePlaceholderForHeartbeat` 方法体删除;`placeholder_chain.go` chainKey 顶部 doc 收紧(去掉 race-window sentinel 措辞);adapter.go 顶部 + ensurePlaceholder + Send 三处提及 ensurePlaceholderForHeartbeat 的 stale 注释更新或删除。
  - 5 个旧测试删除(`TestAdapter_EnsurePlaceholderForHeartbeat_CreatesWhenMissing` / `_ReusesExisting` / `_DMCreates` / `_DeferWhenNoUserMsgID` / `TestAdapter_Send_OutHeartbeat_DeferWhenNoUserMsgID`)。
  - 4 个新 unit test 加进 `placeholder_chain_test.go`:`TestRenderActiveChunkBody_HeaderOnly`(cold-create 路径保持 header 渲染)/ `_SkipsHeaderWhenBodyButNoHeartbeat`(主规则 —— entries>0 且 !hasHeartbeat 时 banner 藏掉)/ `_HeaderAndBody`(hasHeartbeat 后 separator 回来)/ `_HeaderOnlyAfterHeartbeat`(早 heartbeat 早独立 header)。
  - 1 个 v8 假设翻转:`TestAdapter_Send_DM_OutReply_NoFieldsNoCache_NoTrailer` —— body+no-heartbeat 现在反过来不带 `🤖` banner(v8 假设 "banner unconditional" 不再成立)。
  - 行为后果：slash command(`/gtw fix` → `OutCommandReply`)/ WatchMode 拒绝 / spawn failed(`OutError`)/ Agent turn 早于第一次心跳的 OutReply —— 这些路径之前各自挂着一行永远不更新的 `🤖 Working...`,现在 banner 立刻被替换/隐藏。Reaction-only click(无任何 Out*)保留 v8 行为(空 banner),已知遗留,无回归。Agent turn 视觉无变化。

- **2026-08-23 (v9 P1.1) — inheritLatestHeader 翻转 ROTATE/SPLIT rationale**。推翻之前 `commit a654fc3 + aad7705` 引入的"新 chunk header 用 `heartbeatText(nil)` 反映创建时间"决策 —— 改回"完全继承最新的 HeatbeatHeadline"。完整 spec 见 §11.12.7.4。
  - `chunkBody` 加 `inheritLatestHeader(src *chunkBody)` 方法,拷贝 src 的 (header, hasHeartbeat) 对;nil src 是 no-op。
  - `placeholder_chain_flush.go` 6 处全部翻新:`appendSegment` case 3 / `appendSegmentLocked` case 3 / `appendErrorSegment` overflow / `splitOversizedSegmentLocked` pieces / `splitOversizedErrorSegmentLocked` pieces / `flushChainNow` tail piece —— 全部从 `newChunkBody(.., heartbeatText(nil))` 改为 `newChunkBody(.., ""); newChunk.inheritLatestHeader(cur)`。Cold-create 路径(chain.cursor<0)保留 `heartbeatText(nil)`,无 source 可 inherit。
  - `TestChain_RotateChunk_HeaderIsFreshNotInherited` 翻转为 `TestChain_RotateChunk_InheritsLatestHeader` —— 单测契约从"ROTATE 不 inherit"改成"ROTATE 必须 inherit cur 快照"。
  - 加 4 个新单测:`TestChunkBody_InheritLatestHeader_HeaderAndFlag` / `TestChain_SplitOversizedSegment_AllPiecesInheritLatestHeader` / `TestChain_AppendErrorSegment_OverflowInheritsLatestHeader` / `TestChain_FlushChainNow_TailInheritsLatestHeader`。
  - `TestChain_RotateAndSplitDistinguishedByHeader` 行为变更解释:SPLIT 和 ROTATE 现在都 inherit cur 的 (header, hasHeartbeat),因此时间戳一致是设计预期 —— 测试中"shared-header 是 log 不是 fail"的注释已更新。
  - 行为后果:frozen chunks 读出来仍然有意义 —— 用户可以从 banner 时序数出 agent 思考/工具推进节奏。`patchChainHeader` 维持"只更新 active cursor"的语义,避免 N 倍 `editMessageText` 风暴 —— 这是 inherit + patch 组合而非 broadcast 的关键。
  - 关闭 commit `a654fc3` 的 "ROTATE tail header 用 `heartbeatText(nil)` 不是 `cur.headerText()`" 决策 —— 错判,supersede。

