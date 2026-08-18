# Telegram Channel - Topic 方案与接入设计

> **Status**: implemented (核心场景) + 已知 gap 跟踪（见 §14）
> **Scope**: nightme Telegram Bot API 适配器（`internal/channel/telegram/*`）
> **目的**: 在 Telegram Forum Supergroup 中，将主窗口作为会话入口，将每个 qino 会话映射为一个 Topic，并在 Topic 内承载占位状态、thinking、工具调用、结果和交互卡。
> **Related docs**:
> - [feishu.md](./feishu.md) - 飞书 receipt、reply-in-thread 和交互卡实现
> - [F-08-channel-abstraction.md](../feat/F-08-channel-abstraction.md) - Channel 抽象与 Gateway 边界
> - [F-message-flow.md](../feat/F-message-flow.md) - 消息生命周期
> - [SPEC.md](../SPEC.md) - 统一消息模型
>
> **Telegram 官方文档**:
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
| `OutCommandReply` | 发送普通消息 | Topic | 是 |

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

建议在 ChatSession / Binding 层增加 Telegram 专用字段，而不把 Telegram 原生 ID 写入 Gateway 通用协议：

```text
ChatSession
├── ChatID                 # 现有 qino 聊天标识
├── TelegramChatID         # Telegram chat_id
├── TelegramTopicID        # message_thread_id
├── TelegramPlaceholderID  # 占位消息 message_id，可选
└── MessageID              # 当前入站消息 message_id
```

发送消息时的路由规则：

```text
topic_id 非空
  ├── sendMessage / sendPhoto / sendDocument / sendMediaGroup
  │     └── 携带 message_thread_id
  └── editMessageText / editMessageReplyMarkup
        └── 使用 chat_id + message_id，不需要 message_thread_id
```

所有回调处理都必须以 `callback_query.message.message_id` 为准查找原卡；不能只按 `callback_data` 中的 `message_id` 盲信，因为 Telegram 的 `callback_data` 是适配器自己编码的字段。

## 6. 接入前置条件

Topic 方案要求：

1. 群组是 **Forum Supergroup**；普通群组没有 Forum Topic。
2. 群组已开启 Topics。
3. Bot 是群组成员，并具备创建/管理 Topic 所需的权限；建议配置为管理员。
4. Bot 使用长轮询（`getUpdates`）或 Webhook 接收 `Message`、`CallbackQuery` 和 `MessageReactionUpdated` 等更新。
5. 私聊没有 Forum Topic；私聊只能退化为普通消息，并在文档和 UI 中明确标注。
6. Topic 内发送的所有事件都必须显式携带正确的 `message_thread_id`。

## 7. 故障与边界

- Topic 被关闭：使用 `reopenForumTopic` 重新打开；是否允许 Bot 继续发送以实际群组权限和 Telegram 客户端行为为准。
- Topic 被删除或无法继续使用：新建 Topic，并更新 `message_thread_id`；需要决定是否迁移占位状态。
- 占位消息被用户删除：重新发送状态消息并替换 `placeholder_message_id`，不能继续调用旧的 `editMessageText`。
- 消息超过 Telegram 文本长度限制：按 API 限制拆分，结果消息保留顺序并明确 continuation。
- Bot 被移出群组或权限变化：进入降级路径；在主窗口发送一次不可恢复的连接错误，而不是继续静默丢弃。
- 多个 chat 共享一个 Telegram 群组：按 `chat_id + message_thread_id` 路由，不能只用群组 `chat_id`。
- 同一 Topic 被重复创建：通过持久化的 `message_thread_id` 查重，避免重启 daemon 后为同一会话创建多个 Topic。

## 8. 实施顺序

### Phase 1：基础 Topic 收发

- 增加 Telegram 配置、Bot token 和连接模式。
- 实现长轮询或 Webhook 更新入口。
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
- 增加权限、限流、Webhook 签名、Topic 关闭和消息删除测试。

## 9. 验收标准

- 用户在主窗口发送消息后，不会看到 qino 的 thinking/tools 堆积在主窗口。
- daemon 重启后仍能通过 `chat_id + message_thread_id` 找回原 Topic。
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

第一版默认采用 Long Polling，避免每个用户都需要公网 HTTPS Webhook 地址：

```yaml
telegram:
  mode: polling
  bot_token: "<local secret>"
  polling:
    timeout: 30
  require_forum: true
  create_topic_per_chat: true
```

后续可为有公网入口的部署增加 Webhook：

```yaml
telegram:
  mode: webhook
  bot_token: "<local secret>"
  webhook:
    url: "https://nightme.example.com/telegram/webhook"
    secret: "<random verification secret>"
```

无论使用 Polling 还是 Webhook，接收方都必须是该 daemon 自己；不能把同一个 Bot Token 复制给多个 daemon。

### 10.5 Topic 路由与消息发送

用户自建 Bot 模式下，Topic 仍由每个 daemon 自己管理：

```text
主窗口用户消息
        │
        ▼
读取 chat_id + user_id
        │
        ▼
查找本地 message_thread_id
        │
        ├── 已有 Topic ──► 使用原 Topic
        │
        └── 没有 Topic ──► createForumTopic
                                  │
                                  ▼
                         保存 message_thread_id
                                  │
                                  ▼
                         发送占位消息并保存 message_id
```

所有发送方法都携带 Topic ID：

```json
{
  "chat_id": -1001234567890,
  "message_thread_id": 42,
  "text": "🔧 Reading file.go..."
}
```

占位消息的更新仍然使用：

```text
editMessageText(chat_id, placeholder_message_id, text)
```

而不是使用 `editForumTopic`。`editForumTopic` 只修改 Topic 名称或图标，不修改 Topic 内正文。

### 10.6 与现有架构的映射

现有飞书适配器可以保持业务层不变，Telegram 只需要实现 `channel.Channel` 的等价接口：

| 通用能力 | Telegram Adapter |
| --- | --- |
| `Start` | 启动 Polling 或 Webhook 接收循环 |
| `Incoming` | 发布转换后的 `messages.InboundMessage` |
| `Send` | 发送 `OutReply`、`OutResult`、工具和错误消息 |
| `Send` | 所有 `OutboundKind` 的唯一出口；交互选择也走这里 |
| `OnPromptEnded` | 更新占位消息或添加终态 reaction |
| `HealthSnapshot` | 汇报 API、Polling、Webhook 和 Topic 状态 |
| `SetLogger` | 输出 Telegram 收发和重试日志 |
| `BuildBlocks` | 将文本、caption、附件转换为统一内容块 |

Gateway、Chatsession 和 Agent 继续使用现有 `messages` 类型，不感知 Telegram 的 BotFather、Bot Token、Forum Topic 或 callback query。

### 10.7 该模式的边界

- 用户必须先通过 `@BotFather` 手动创建 Bot。
- Bot Token 不能共享，不能由多个 daemon 共同轮询。
- Topic 方案要求目标群组是 Forum Supergroup；普通群组没有 Forum Topic。
- 私聊没有 Forum Topic，只能退化为普通消息。
- 用户需要自行配置 Bot 的群组权限；如果 Bot 没有发消息、创建 Topic 或管理 Topic 的权限，qino 必须在主窗口明确报错，而不是静默丢弃。
- 由于 Bot 是用户自己的，qino 只能控制该 Bot，不能帮助用户恢复或修改 BotFather 账号凭证。
- 二维码可以简化“把已有 Bot 添加到群组”的步骤，但不能实现飞书式的全自动应用注册。

### 10.8 本方案验收补充

- 每个 daemon 只使用一个自己的 Bot Token。
- `getUpdates` / Webhook 更新不会被其他 daemon 重复消费。
- Bot Token、用户文本和附件不会写入普通日志。
- 用户通过 BotFather 创建 Bot 后，可以按 CLI 引导完成 Token 配置、群组添加和 daemon 启动。
- 主窗口只作为入口，所有 thinking、tools、结果和交互卡都进入对应 Topic。
- daemon 重启后可以恢复 `chat_id + message_thread_id + placeholder_message_id`。
- 用户从 `@BotFather` 执行 `/token` 或 `/revoke` 后，qino 能在下次启动时检测凭证失效并给出明确修复提示。

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
  mode: polling
  bot_token: "<BotFather token>"

  listen:
    private_chats: true
    groups: true
    forum_topics: true

  routing:
    chat_id_per_workspace: true
    topic_mode: separate

  access:
    # 私聊默认处理
    private_require_allowlist: true
    # 群组默认只处理命令、回复和 mention
    group_require_mention: true
```

实际配置字段名应与后续 `Config` 实现保持一致；这里的语义是：每个 Telegram `chat_id` 作为一个 workspace，Forum Topic 作为独立 qino 会话。

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
          qino 工作区 A   qino 工作区 B   qino 工作区 C
```

每次收到消息后，适配器使用以下字段路由：

```text
chat.id              → qino ChatID
message_thread_id    → Telegram Topic ID
from.id              → Telegram UserID
message.message_id   → Telegram MessageID
```

建议的持久化关系：

```text
chat_id
  └── user_id
        └── message_thread_id
              └── placeholder_message_id
```

同一个群组内的不同 Topic 默认使用独立 `ChatSession`：

```text
群组 A
├── Topic 42 → qino 会话 A-42
└── Topic 88 → qino 会话 A-88
```

如果产品希望群组内所有 Topic 共用 Agent Session，可将 `topic_mode` 改为 `shared`。

### 11.7 主窗口、Topic 和监听模式

用户在主窗口发送消息后，Bot 可以在该群组创建或复用 Topic：

```text
主窗口 / General Topic
└── 用户消息
    └── qino 创建或查找 message_thread_id
        └── 在 Topic 中发送占位消息
            ├── thinking
            ├── tool start
            ├── tool end
            └── result
```

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

重启后使用 `update_id + 1` 作为 `getUpdates` offset，继续消费尚未确认的更新。Webhook 场景则需要通过持久化更新去重和重试机制处理重复投递。

### 11.10 开通验收

- 用户能通过 `@BotFather /newbot` 创建 Bot 并安全保存 Token。
- Bot 能被手动加入多个群组。
- 关闭 Privacy Mode 后，群组普通消息可以到达 nightme。
- `chat_id` 能区分不同群组，相同群组内 `message_thread_id` 能区分不同 Topic。
- 主窗口入口不会累积 qino 的 thinking、tools 和 receipt。
- Topic 内可以按顺序看到 thinking、tools、结果和交互卡。
- 群组 mention gate 和私聊行为符合预期。
- Token 失效、权限不足、群组不是 Forum 等错误都有明确提示。

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

第一版推荐 Long Polling：

```yaml
telegram:
  mode: polling
  bot_token: "<local secret>"
  polling:
    timeout: 30
```

每个 Bot 只能有一个 `getUpdates` consumer。daemon 重启时使用持久化的 `update_id + 1` 继续消费。后续可以为具有公网入口的实例增加 Webhook，并使用 Webhook secret 验证请求来源。

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
  mode: polling
  bot_token: "<local secret>"
  polling:
    timeout: 30
  listen:
    private_chats: true
    groups: true
    forum_topics: true
  routing:
    chat_id_per_workspace: true
    topic_mode: separate
  access:
    private_require_allowlist: true
    group_require_mention: true
  interaction:
    custom_input: force_reply
    card_state_store: local
  messages:
    thinking_mode: topic
    tools_mode: topic
    heartbeat_mode: edit_placeholder
    long_result: split_in_topic
```

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
- Webhook 使用 secret header 验证请求。
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

## 14. 已知限制 / Gap（截至本次实现）

下面这些是文档里讨论过、Telegram Bot API 的能力差距或实现优先级选择导致没做的点。每条都标明属于哪一类：

| 类别 | 说明 |
|---|---|
| **限制** | Telegram Bot API 本身不支持，靠 adapter 怎么写都做不到 |
| **降级** | 飞书有原生能力、Telegram 没有对应物，已用近似手段实现 |
| **未实现** | 设计上想做、但目前没实现（不在本期 scope） |

### 14.1 限制类（API 做不到）

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

### 14.2 降级类（用近似手段实现，已 work）

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

### 14.3 未实现类（设计意图存在，本次没做）

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

### 14.4 实现优先级建议

如果未来要做 follow-up，建议按这个顺序：

1. **N3**（OnPromptEnded 用 reaction 而非改文本）—— 1 行改动，恢复力强。
2. **N6**（占位 header 加 session identity）—— 小改，提升可观测性。
3. **N4**（orphan fallback）—— 中等改，覆盖极端场景。
4. **N2**（pendingHeartbeats）—— 中等改，但 C2 已经覆盖大部分场景。
5. **N1**（rolling-log receipt）—— 大改，需要权衡 4096 限制 + 历史重发成本。
6. **N5**（用 ReplyTo）—— 视觉改进，影响小。

### 14.5 本次实现完成（C1, C2）

| ID | 内容 | 状态 |
|---|---|---|
| C1 | OutInit silent drop（与 feishu F-44 对齐） | 已实现 |
| C2 | OutHeartbeat 路径 ensurePlaceholderForHeartbeat（占位缺失时自动创建） | 已实现 |

后续修订请直接在本节追加，并把对应 issue 编号填进去。

## 15. Telegram 独有、未利用的能力

下面这些 API 飞书**没有**对应物，Telegram 原生支持但当前 adapter 没有用。每条标出"对应飞书体验"和"启用后能补齐哪个 gap"，便于后续讨论优先级。

### 15.1 P1 - `pinChatMessage` 钉住最终结果

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

### 15.2 P2 - `deleteMessage` 删除占位消息

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

### 15.3 P3 - OnPromptEnded 用 ✅ reaction + delete placeholder 组合

**能力**：组合 P2 + setMessageReaction("✅")。

**对应飞书体验**：飞书 receipt 不删但加 ✅ reaction。

**补齐的 gap**：
- 14.3 N3 的最佳实现：视觉上看到 ✅（reaction）+ Topic 流不堆 "✅ Completed" 文本。

**当前为什么没做**：见 14.3 N3。

**工作量估算**：~15 行。

**风险 / 边界**：同 P2。

### 15.4 P4 - `editMessageReplyMarkup` 只更新键盘

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

### 15.5 P5 - `sendMediaGroup` 批量附件

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

### 15.6 P6 - `unpinAllChatMessages` 清理历史 pin

**能力**：`unpinAllChatMessages(chat_id)` 清空 chat 内所有 pin。

**对应飞书体验**：飞书没有 pin。

**补齐的 gap**：
- Bot 维护时（升级、迁移）可能留下过时 pin。
- 单元测试 / 集成测试需要在每个 case 之间清理 pin。

**当前为什么没做**：产品需求不明确。

**工作量估算**：~5 行（暴露为 adapter method + 测试 helper）。

### 15.7 P7 - `sendChatAction` 显示 typing 状态

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

### 15.8 P8 - `sendPoll` 作为 Choice 的另一种渲染

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

### 15.9 优先级建议

按 ROI 排序（参考 14.4）：

| 优先级 | 项 | 工作量 | 价值 | 理由 |
|---|---|---|---|---|
| 1 | **P3** reaction + delete placeholder | ~15 行 | 高 | 替换 N3，最干净的实现 |
| 2 | **P1** pin OutResult | ~15 行 | 高 | UX 提升大（长 Topic 内找答案） |
| 3 | **P7** typing indicator | ~25 行 | 中 | 长 turn 反馈 |
| 4 | **P4** editMessageReplyMarkup 单独更新 | ~20 行 | 低 | 性能优化 |
| 5 | **P5** sendMediaGroup | ~30 行 | 低 | 少见场景 |
| 6 | **P2** deleteMessage（仅作为 P3 子步骤） | ~10 行 | 中 | 已在 P3 中 |
| 7 | **P6** unpinAllChatMessages | ~5 行 | 极低 | 测试维护 |
| 8 | **P8** sendPoll 替代 Choice | ~80 行 | 中 | 可选替代方案，争议大 |

### 15.10 与已有 gap 的关系

| Telegram 能力 | 补齐的 gap |
|---|---|
| P1 pin | N1（rolling-log 替代方案） |
| P2 delete | N3 变体 |
| P3 reaction+delete | N3 最佳实现 |
| P4 edit markup | D1（thinking/tool 独立消息优化） |
| P5 media group | 附件流程优化 |
| P7 typing | 用户反馈体验（不在 §14 内） |

后续讨论时优先关注 P1/P3/P7（性价比最高）。


## 16. 网络代理

Telegram Bot API 在某些网络环境（如中国大陆）下不可达。nightme **默认继承** 标准代理环境变量，无需任何配置。

### 16.1 支持的环境变量

| 变量 | 作用 |
|---|---|
| `HTTP_PROXY` | HTTP 请求的代理 URL（如 `http://127.0.0.1:7890`） |
| `HTTPS_PROXY` | HTTPS 请求的代理 URL（对 `api.telegram.org` 生效） |
| `NO_PROXY` | 不走代理的域名/网段（逗号分隔） |
| `ALL_PROXY` | 兜底代理，HTTP_PROXY/HTTPS_PROXY 未设置时生效 |

Go 标准库的 `http.ProxyFromEnvironment` 自动读取这些变量，无需 nightme 介入。

### 16.2 使用示例

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

### 16.3 内部实现

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

### 16.4 不暴露代理配置的原因

不提供 `cfg.Telegram.ProxyURL` 之类的配置项，原因：
- 代理需求来自环境（用户机器的网络），不是配置决策
- 配置项会被各种 secret 管理工具、CI/CD 流水线暴露在 diff 里
- 环境变量是 OS-level 的标准机制，工具链（Docker、Kubernetes、systemd）都支持
