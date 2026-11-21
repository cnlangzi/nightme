# F-08: Channel Abstraction

> **Status**: implemented

> **Depends on**: F-22 (Feishu One-Click App Registration) — for credentials
> **Related docs**: [`SPEC.md`](../SPEC.md)§2.4; [`SPEC.md`](../SPEC.md); [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md)

---

## 1. Description

定义 `Channel` interface 让 nightme 支持多种 IM backend。已注册的 channel：飞书（lark-oapi-go）、Telegram、Slack、Discord；`internal/channel/echo` 是无网络的测试桩，不在 `channel` registry 中注册。interface 设计保证新增平台无需改动核心逻辑。

**职责再定义**：Channel 是**纯渲染器**（dumb renderer）。它做两件事：

1. **IM 协议编解码** — 把 native event 解码成 `InboundMessage`；把 `OutboundMessage` 编码成 native API 调用
2. **`Send(OutboundMessage)` 通用渲染** — `OutReply` / `OutToolStart` / `OutToolEnd` / `OutThinking` / `OutChoice` / `OutResult` 等所有 `OutboundKind` 的视觉表达 + 自管 receipt 生命周期（card / thread reply / reaction）

除这两件事之外，interface 上还有一组 runtime 调用的扩展方法（`OnPromptEnded` / `HealthSnapshot` / `HealthSummary` / `SetLogger` / `BuildBlocks`），它们同样不引入任何 session / workspace 知识。

**Channel 不知道**：sessions、workspaces、agents、bindings、slash commands、receipt 状态机的任何细节。Gateway 完全不持有 receipt —— Channel 在内部按 `OutboundMessage.ReplyTo = userMsgID` 路由到自己的 receipt 对象，自己决定怎么 cold-create / PATCH / 终态。

**接口精简**：Channel 的唯一**出站消息**方法是 `Send(OutboundMessage)`。交互选择（OutChoice / OutChoicePatch）也走这条出口；相关键是 `Choice.RequestID`，Channel 私有映射到平台 message id。`messages.Choice` 只带语义（Kind / Options / Questions / Settled / SelectedID）；标题 emoji、header 颜色、按钮绿灰、卡内翻页游标都在 Channel。调用方不拿 bot-side id，也不另开 `SendCard` / `SendAction`。Receipt 状态机完全在 Channel 内部，详见 SPEC 与 [`channel/feishu-rendering.md`](../channel/feishu-rendering.md)。

**扩展**：Channel 按 `OutboundKind` 自决 routing（Feishu 选 thread + 类型感知摘要，详见 [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md) + [`channel/feishu-rendering.md`](../channel/feishu-rendering.md)）。这是 "concrete stays concrete" 原则的具体落地：Gateway 只发 `OutboundMessage{Kind, ReplyTo, ...}`；Channel 看到 Kind 后自决 thread reply / receipt card / reaction。

---

## 2. Interface

```go
// internal/channel/channel.go
package channel

import (
    "context"
    "encoding/json"
    "log/slog"

    "github.com/cnlangzi/nightme/internal/agent"
    "github.com/cnlangzi/nightme/internal/messages"
)

// Message is an alias for messages.InboundMessage. Kept as a type alias
// (not a duplicate struct) so adapter code keeps a single canonical
// definition.
type Message = messages.InboundMessage

// Attachment is an alias for messages.Attachment.
type Attachment = messages.Attachment

// Normalized chat type constants. Channel adapters may classify chats
// internally, but no Channel introduces a thread concept into the
// nightme data model: thread is a rendering concern the Channel
// handles on its own (see docs/feat/F-33-simplify-chatid-data-model.md).
const (
    ChatTypeP2P   = "p2p"   // 1-on-1 DM (Feishu) / private (Telegram) / im (Slack)
    ChatTypeGroup = "group" // group chat (Feishu / Telegram / Slack channel)
)

// Channel is the lifecycle and messaging contract implemented by each IM
// adapter. It is intentionally thin: protocol conversion + display
// strategy. All state-machine logic (bindings, receipt FSM, session lookup)
// lives above the Channel.
type Channel interface {
    // Name returns the channel's identifier (e.g. "feishu", "slack").
    Name() string

    // Start starts the adapter's long-lived receive loop. The adapter
    // publishes normalized InboundMessages on Incoming() from this point
    // until Stop is called.
    Start(ctx context.Context) error

    // Stop closes the receive loop and releases adapter resources.
    Stop(ctx context.Context) error

    // Incoming returns the channel's normalized message stream.
    Incoming() <-chan messages.InboundMessage

    // Send dispatches one OutboundMessage to the channel's native UI.
    // "Delivered" means this call returned nil — the runtime treats Send
    // as fire-and-ack and does not retry. The Channel may silently drop
    // OutboundMessage kinds its UI cannot represent without surfacing an
    // error (e.g. Slack cannot swap reactions in place).
    Send(ctx context.Context, msg messages.OutboundMessage) error

    // OnPromptEnded fires when a chat's active prompt terminates
    // (success / error / cancel). Adapters with receipt rendering
    // transition their card to the terminal state; others no-op.
    OnPromptEnded(ctx context.Context, chatID, userMsgID string, reason agent.PromptEndReason)

    // HealthSnapshot returns the adapter's own health payload. The
    // daemoncontrol server serves the JSON verbatim.
    HealthSnapshot() (name string, payload json.RawMessage, err error)

    // HealthSummary returns the cross-channel normalized health view so
    // `nightme doctor` can render one section per channel without
    // decoding any adapter's payload shape.
    HealthSummary() HealthSummary

    // SetLogger installs the daemon's structured logger. The runtime
    // calls it once after construction, before Start.
    SetLogger(logger *slog.Logger)

    // BuildBlocks converts an inbound text + attachment list into the
    // channel's own block shape. The dispatcher calls it as the
    // fallback when msg.Blocks is empty.
    BuildBlocks(text string, attachments []messages.Attachment) []agent.ContentBlock
}
```

Compile-time checks live in each adapter package (`var _ channel.Channel =
(*Adapter)(nil)`), e.g. `internal/channel/feishu/adapter.go`、
`internal/channel/telegram/adapter.go`、`internal/channel/slack/adapter.go`、
`internal/channel/discord/adapter.go`、`internal/channel/echo/echo.go`。

---

## 3. Implementation

**文件**：
- `internal/channel/channel.go` — interface 定义 + `Message` / `Attachment` alias + `HealthSummary`
- `internal/channel/registry.go` — channel 注册表（每个 adapter 的 `init()` 调 `channel.Register`；`BuildAll` 按 cfg 凭据构造）
- `internal/channel/feishu/adapter.go` — 飞书 adapter 实现（receipt 在 `receipt.go`）
- `internal/channel/telegram/adapter.go` — Telegram adapter
- `internal/channel/slack/adapter.go` — Slack adapter
- `internal/channel/discord/adapter.go` — Discord adapter
- `internal/channel/echo/echo.go` — no-network stub（只把消息写进 writer）

**架构**：
```
┌─────────────────────────────────────────────────────────┐
│ Channel interface (channel.go) — 10 methods            │
│   - Name / Start / Stop                                 │
│   - Incoming() <-chan messages.InboundMessage           │
│   - Send(messages.OutboundMessage) error ← 所有渲染走这里│
│   - OnPromptEnded / HealthSnapshot / HealthSummary      │
│   - SetLogger / BuildBlocks                            │
└─────────────────────────────────────────────────────────┘
        ↑ implements
        │
┌────────────────────┐  ┌────────────────┐  ┌────────────────┐
│ feishu.Adapter     │  │ telegram.      │  │ slack.Adapter  │
│ (receipt.go)       │  │ Adapter        │  │                │
│ - lark NewClient   │  │ - Bot API      │  │ - slackgo      │
│ - WebSocket 长连接 │  │ - long polling │  │ - Socket Mode  │
│ - MessageReceipt   │  │ - 无 receipt   │  │ - 无 receipt   │
│   {replyMsgID,     │  │   对象         │  │   对象         │
│    cardMsgID,      │  │                │  │                │
│    entries,        │  │                │  │                │
│    state, tokens}  │  │                │  │                │
│ receiptsByUserMsgID│ │                │  │                │
└────────────────────┘  └────────────────┘  └────────────────┘
        ┌────────────────────┐
        │ discord.Adapter    │
        │ - Discord REST API │
        │ - 无 receipt 对象  │
        └────────────────────┘
```

**Receipt 归属**:Receipt object 完全 Channel 私有。`feishu.Adapter` 通过 `receiptsByUserMsgID[userMsgID]` 维护自己的 receipt 簿记（按 userMsgID 索引）。其余 adapter 不维护 receipt —— 它们的 `Send` 直接投递平台原生消息。`Gateway` 完全不知道这些。

**实现参考**:
- Feishu: [`internal/channel/feishu/adapter.go`](../../internal/channel/feishu/adapter.go) (`Send` 分发) + [`internal/channel/feishu/receipt.go`](../../internal/channel/feishu/receipt.go) (rolling-log 状态机)
- Echo: [`internal/channel/echo/echo.go`](../../internal/channel/echo/echo.go) (`Send` 一行输出)

**完整 协议契约 + 各 Channel 实现策略**:见 [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md) §3.

## 4. The "Channel is dumb" contract

This section makes the responsibility split concrete. Channel **MUST NOT** do any of the following:

| ❌ Don't | ✅ Instead |
|---------|-----------|
| Track whether a user message has been dispatched | **N/A** — 上层不跟踪 receipt。Channel 通过 `OutMessageState` 渲染进度（见 [`F-53-message-prompt-lifecycle.md`](./F-53-message-prompt-lifecycle.md)） |
| Decide whether to queue vs send | `ChatSession` 的 `MessageQueue`（`internal/chatsession/message_queue.go`）决定；runtime 调用 `cs.QueueUserMessage` |
| Lookup session or workspace | 上层做 binding lookup，交给 Channel 的只有一个 `messages.OutboundMessage`（`ChatID` / `ReplyTo` / `Kind` + 类型化 payload） |
| Format receipt text from blocks using shared utils | 出站方向 Channel 自己渲染 `OutboundMessage`；入站方向的平台 block 形状由 `Channel.BuildBlocks` 产出 |
| Call Session or Manager | Channel 不 spawn agent、不查 session；只有飞书 receipt 读 `chatsession.PromptState` 这个状态枚举 |
| **Maintain an opaque `Receipt` handle for the caller** | **Receipt handle 不出 Channel** —— Channel 内部持有自己的 receipt 对象；上层只看到 `OutboundMessage.ReplyTo = userMsgID` |
| **Put a routing hint into an outbound payload field** | **payload 只带数据**（`Tool` / `TaskList` / `Result` / `Usage` / `MessageState` 等类型化字段），不带路由提示；Channel 读 `Kind` 自行决定渲染（Feishu：thinking/tool 走 thread reply；reply/result/init/usage 走 receipt card） |

Channel **MUST**:

| ✅ Do |
|------|
| Render `OutboundMessage` kinds（`OutReply` / `OutToolStart` / `OutToolEnd` / `OutThinking` / `OutChoice` / `OutChoicePatch` / `OutResult` / `OutInit` / `OutCommandReply` / `OutError` ...）in native UI |
| Route `OutboundMessage{ReplyTo: userMsgID}` to its own per-userMsgID receipt (飞书：cold-create if missing; PATCH if exists）|
| Manage its own internal receipt state (Feishu: message IDs, cardMsgID, entries, prompt state) — fully Channel-private |
| Render `OutMessageState` events as platform-native progress indicators (Feishu: AddReaction; Discord: 👌 reaction) |
| Fail gracefully on `Send` errors (log warn; continue) — receipt UI must not block the agent event stream |
| **自决按 OutboundKind 分流**（thread reply / receipt card / reaction / ...）— 自治范围内的渲染决策。例：Feishu 选 thinking/tool → thread reply（详见 [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md)）|

---

## 5. SendLongMessage

`Channel` interface 只有一条出站方法：`Send(ctx, messages.OutboundMessage)`。所有 `OutboundKind`（reply / thinking / tool / choice / result / receipt / reaction ...）都走它。

飞书 adapter 另有两个内部 helper：`SendMessage`（发一条文本）与 `SendLongMessage`（按换行边界切分长文本后逐块发送，单块上限 3800 字节）。它们是 adapter 私有实现细节，不是 `Channel` 契约的一部分。Echo 不切分（终端友好）。

---

## 6. Edge cases

| 场景 | 处理 |
|------|------|
| 飞书 WebSocket 断连 | lark-oapi SDK 自动重连（指数退避）；期间 Channel.Send 阻塞 / 失败 → Gateway 不重试，log warn |
| 飞书 QPS 限流（单聊 5 QPS）| 内部 token bucket；超限排队 |
| Channel.Incoming channel 满 | 普通消息投递阻塞等待（ctx 取消可打断）；卡片点击等合成消息走 drop-newest + log warn（飞书 inbound 缓冲 128）|
| 用户发图片 / 文件 | Channel adapter 下载到本地路径，blocks 包含 ContentImage/ContentFile；Channel.Send 第一个 OutboundMessage 时 cold-create receipt，把 attachment path 写到 receipt 文本里 |
| 用户撤回消息 | 忽略；receipt 状态保留在 Channel 内部，撤回不会触发额外清理 |
| 飞书 appSecret 错误 | Start() 返回 error，nightme 启动失败 |
| 飞书权限被回收 | WebSocket 收到权限错误事件 → Channel.Send 返回 error → 日志告警 |
| Channel.Send cold-start receipt 失败 | Channel 内部 log warn；下次 OutboundMessage 再试；永远不向上传播（Gateway 不持有 receipt） |
| Channel.Send PATCH 失败（Feishu 429 / 230020）| Channel 内部 log warn；下次事件重试；最终一致性 |
| 用户在 receipt 已 Done 后再发同 userMsgID 的消息（重发）| Channel 按新消息处理；receipt 保留终态不动，新消息触发新 turn 的新 anchor |
| Receipt handle race（两个 goroutine 同时 PATCH 同一 receipt）| Channel 内部负责 lock（Feishu MessageReceipt 有 mu）；Gateway 不持有锁 |
| Mention prefix（`@_user_N `）导致 slash command 解析失败 | Channel adapter 在构造 `Message.Text` 前 strip 开头的 `@bot_key ` / `@_all ` mention 前缀；中段 mention 不动（F-watch）|
| Group 里用户未 @ bot 的消息 | `Message.HasMention = false`；`chatsession.Manager.AcceptInbound` 结合 `ChatSession.WatchMode()` 决定 drop（`WatchModeMention`，默认值）或 pass（`WatchModeAll`）。DM 下 `HasMention` 永远为 true，gate 为 no-op（F-watch）|
| OutThinking / OutToolStart / OutToolEnd 太多元素挤占 receipt card | **F-thread-route**: Feishu 把这些 Kind 路由到独立 thread reply（rootID = userMsgID），receipt card 收窄到只承载最终答复；详见 [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md)。其他 Channel 自决（折叠 section / 子节点 / drop）|

---

## 7. Test plan

### 7.1 Unit

**Channel interface**:
- 每个 adapter 包自带 compile-time check（`var _ channel.Channel = (*Adapter)(nil)`），保证 10-method interface 被满足
- Feishu adapter `MessageReceipt` 内部状态机测试：`AppendEntry` / `SetPromptState` / `SetTaskList` / `RolloverTo` 路径
- Echo adapter: `Send` 写出预期的一行输出

**Feishu rolling-log**:
- 第一个 `OutboundMessage{ReplyTo: userMsgID}` 触发 cold-start card 创建
- 后续同 userMsgID 的事件 PATCH 同一张 card
- `OutMessageState{MessageDone}` 走 `AddReaction` 路径，不动 card body
- 失败路径：cold-start 失败 → 下次 Send 重试；PATCH 失败 → log warn + 继续

**Echo**:
- 每个 `Send` 写出一行输出
- 多次 `Send` 累加在 `recorded` slice

### 7.2 Integration

- fake Channel + Gateway: 验证 `OutboundMessage.ReplyTo` 等于当前 turn 的 user message id
- fake Channel + ChatSession: 发 2 条消息 → 第一个 turn 的 receipt 完成前第二条入队 → 第二个 turn anchor 到第二条
- fake Channel + MessageState: 验证 `OutMessageState` 事件按预期顺序到达 (`MessageQueued` → `MessageSubmitted` → `MessageDone`)

### 7.3 E2E

- 飞书 DM 发消息 → ⏳ reaction → 🔄 → 一张 card 持续 PATCH → ✅ reaction；card 最终内容包含全部 event 内容
- 飞书 DM 连发 3 条（在 turn 进行中）→ ⏳ 3 次 reaction；只有最后一条得 ✅；3 条合并一个 turn，card 只一张
- echo channel 作为无凭据的测试桩接入 runtime：每条出站消息一行输出，全部带 `ReplyTo=userMsgID`

---

## 8. Open questions

- Receipt card 完成后是否保留还是删除？决策：**保留**。Card body 是最终内容 + ✅ emoji 即终态，用户可滚动 DM 看历史。Channel 内部不主动删除。
- 群聊多人 → 一个 userMsgID 对应一个 receipt，但 receipt card 在群里发；多人各自看到自己的 reaction。一致：每个 user message 一个 anchor，sender 不区分。
- 飞书 reaction emoji 累积 vs 切换：由 `MessageState` FSM 处理（append-only ⏳ → 🔄 → ✅），不再是 receipt FSM 切换。

---

## 9. Cross-references

- **OutboundMessage 结构**：见 [`SPEC.md`](../SPEC.md) §2.2 + `internal/messages/outbound.go`
- **InboundMessage 结构**：见 `internal/messages/inbound.go`
- **Gateway 如何用 receipt FSM**：见 [`SPEC.md`](../SPEC.md) §2.4
- **Feishu QR onboarding**（app credentials 获取）：见 [`../channel/feishu-onboarding.md`](./../channel/feishu-onboarding.md)
- **输入透传（InboundMessage 怎么到 Gateway）**：见 [`SPEC.md`](../SPEC.md) §2.1

