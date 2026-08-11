# F-08: Channel Abstraction

> **Status**: implemented

> **Depends on**: F-22 (Feishu One-Click App Registration) — for credentials
> **Related docs**: [`SPEC.md`](../SPEC.md)§2.4; [`F-gateway.md`](./F-gateway.md); [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md)

---

## 1. Description

定义 `Channel` interface 让 nightme 支持多种 IM backend。MVP 仅实现飞书（lark-oapi-go），但 interface 设计保证后续接入 WhatsApp / Telegram / Slack / Web UI 无需改动核心逻辑。

**职责再定义**：Channel 是**纯渲染器**（dumb renderer）。它做两件事：

1. **IM 协议编解码** — 把 native event 解码成 `InboundMessage`；把 `OutboundMessage` 编码成 native API 调用
2. **`Send(OutboundMessage)` 通用渲染** — 文本 / tool_start / tool_end / thinking / card / reaction 等所有 `OutboundKind` 的视觉表达 + 自管 receipt 生命周期（card / thread / DOM 节点）

**Channel 不知道**：sessions、workspaces、agents、bindings、slash commands、receipt 状态机的任何细节。Gateway 完全不持有 receipt —— Channel 在内部按 `OutboundMessage.ReplyTo = userMsgID` 路由到自己的 receipt 对象，自己决定怎么 cold-create / PATCH / 终态。

**接口精简**：Channel interface 当前为 **6 个方法**（`Name / Start / Stop / Send / SendCard / Incoming`）。其中 `SendCard` 是飞书等 IM 的 card PATCH 专用通道，独立于通用 `Send`。Receipt FSM 整体从 Gateway 撤回，详见 SPEC 与 [`channel/feishu-rendering.md`](../channel/feishu-rendering.md)。

**扩展**：Channel 按 `OutboundKind` 自决 routing（Feishu 选 thread + 类型感知摘要，详见 [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md) + [`channel/feishu-rendering.md`](../channel/feishu-rendering.md)）。这是 "concrete stays concrete" 原则的具体落地：Gateway 只发 `OutboundMessage{Kind, ReplyTo, ...}`；Channel 看到 Kind 后自决 thread reply / receipt card / reaction。

---

## 2. Interface

```go
// internal/channel/channel.go
package channel

import (
    "context"

    "github.com/cnlangzi/nightme/internal/agent"
    "github.com/cnlangzi/nightme/internal/gateway"
)

// Message is an alias for gateway.InboundMessage. Kept as a type alias
// (not a duplicate struct) so existing callers continue to compile
// while the source-of-truth lives in the gateway package.
type Message = gateway.InboundMessage

// Attachment is an alias for gateway.Attachment.
type Attachment = gateway.Attachment

// Normalized chat type constants. Channel adapters should map their
// native values onto these.
const (
    ChatTypeP2P    = "p2p"         // 1-on-1 DM
    ChatTypeGroup  = "group"       // group chat
    ChatTypeThread = "topic_group" // Feishu topic group
)

// Channel is the lifecycle and messaging contract implemented by each IM
// adapter. In it is intentionally thin: protocol conversion + display
// strategy. All state-machine logic (bindings, receipt FSM, session lookup)
// lives at the Gateway layer.
type Channel interface {
    // Name returns the channel's identifier (e.g. "feishu", "echo",
    // "slack"). The Gateway uses this as the lookup key when resolving
    // an outbound message's destination.
    Name() string

    // Start starts the adapter's long-lived receive loop. The adapter
    // publishes normalized InboundMessages on Incoming() from this point
    // until Stop is called.
    Start(ctx context.Context) error

    // Stop closes the receive loop and releases adapter resources.
    Stop(ctx context.Context) error

    // Send dispatches one OutboundMessage to the channel's native UI.
    // "Delivered" means this call returned nil — Gateway treats Send as
    // fire-and-ack and does not retry. The Channel may silently drop
    // OutboundMessage kinds its UI cannot represent without surfacing an
    // error (e.g. Slack cannot swap reactions in place).
    Send(ctx context.Context, msg gateway.OutboundMessage) error

    // Incoming returns the channel's normalized message stream.
    Incoming() <-chan Message

    // (SPEC): receipt lifecycle methods are GONE from
    // the Channel interface. Receipt is now entirely Channel-
    // internal. Send() handles all rendering — including receipt
    // creation / PATCH / terminal state — by routing each
    // OutboundMessage via its ReplyTo=userMsgID field to Channel's
    // own per-userMsgID receipt (card / thread / DOM node).
}

// Compile-time check
var _ Channel = (*feishu.Adapter)(nil)
var _ Channel = (*echo.Channel)(nil)
```

---

## 3. Implementation

**文件**：
- `internal/channel/channel.go` — interface 定义 + alias + receipt types
- `internal/channel/feishu/adapter.go` — 飞书 adapter 实现（receipt 在 `receipt.go`）
- `internal/channel/echo/echo.go` — no-network stub（receipt 是 log 行）
- `internal/channel/mock/mock.go` — 测试用 mock

**架构**：
```
┌─────────────────────────────────────────────────────────┐
│ Channel interface (channel.go) — 6 methods             │
│   - Name / Start / Stop                                 │
│   - Incoming() <-chan Message                           │
│   - Send(OutboundMessage) error   ← 所有渲染走这里       │
└─────────────────────────────────────────────────────────┘
        ↑ implements
        │
┌────────────────────┐  ┌────────────────┐
│ feishu.Adapter     │  │ echo.Channel   │
│ (receipt.go)       │  │ (no-network    │
│ - lark NewClient   │  │  stub)         │
│ - WebSocket 长连接 │  │                │
│ - MessageReceipt   │  │ log only       │
│   {replyMsgID,     │  │ (no receipt    │
│    cardMsgID,      │  │  object)       │
│    entries,        │  │                │
│    state, tokens}  │  │                │
│ receiptsByUserMsgID│ │                │
└────────────────────┘  └────────────────┘
```

**关键变化**:Receipt object 完全 Channel 私有。`feishu.Adapter` 通过 `receiptsByUserMsgID[userMsgID]` 维护自己的 receipt 簿记(不再是 per-chat map)。`echo.Channel` 不维护任何 receipt —— 它的 `Send` 直接 log 一行即可。`Gateway` 完全不知道这些。

**实现参考**:
- Feishu: [`internal/channel/feishu/adapter.go`](../../internal/channel/feishu/adapter.go) §6 (`Send` 分发) + [`internal/channel/feishu/receipt.go`](../../internal/channel/feishu/receipt.go) (rolling-log 状态机)
- Echo: [`internal/channel/echo/echo.go`](../../internal/channel/echo/echo.go) (`Send` 一行 log)

**完整 协议契约 + 各 Channel 实现策略**:见 [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md) §3.

## 4. The "Channel is dumb" contract

This section makes the responsibility split concrete. Channel **MUST NOT** do any of the following:

| ❌ Don't | ✅ Instead |
|---------|-----------|
| Track whether a user message has been dispatched | **N/A** — Gateway no longer tracks receipts at all. Channel renders progress via `OutMessageState` (separate concern, see F-31) |
| Decide whether to queue vs send | ChatSession.InputBuffer decides; Gateway observes and calls `cs.QueueUserMessage` |
| Lookup session or workspace | Gateway does binding lookup, hands Channel only `chatID` + `userMsgID` + `blocks` |
| Format receipt text from blocks using shared utils | Channel takes `[]agent.ContentBlock` directly; Channel formats internally |
| Call Session or Manager | Channel never imports `chatsession/` |
| **Maintain an opaque `Receipt` handle for Gateway** | **Receipt handle is gone** — Channel owns its receipt objects internally; Gateway only sees `OutboundMessage.ReplyTo = userMsgID` |
| **Put routing hint in `OutboundMessage.Meta`** | **Meta carries data only** (output / err / args), not routing hint. Channel reads Kind and decides routing itself (Feishu: thread reply for thinking/tool/compaction; receipt card for text/result/init/usage) |

Channel **MUST**:

| ✅ Do |
|------|
| Render `OutboundMessage` kinds (text/tool_start/tool_end/thinking/card/...) in native UI |
| Route `OutboundMessage{ReplyTo: userMsgID}` to its own per-userMsgID receipt (cold-create if missing; PATCH if exists) |
| Manage its own internal receipt state (Feishu: message IDs, cardMsgID, entries; Slack: thread map; Web: DOM nodes) — fully Channel-private |
| Render `OutMessageState` events as platform-native progress indicators (Feishu: AddReaction; Slack: emoji shortcode; Web: DOM class) |
| Fail gracefully on `Send` errors (log warn; continue) — receipt UI must not block the agent event stream |
| **自决按 OutboundKind 分流**（thread reply / receipt card / reaction / ...）— 自治范围内的渲染决策。例：Feishu 选 thinking/tool/compaction → thread reply（详见 [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md)）|

---

## 5. SendLongMessage

The `SendMessage` / `SendLongMessage` distinction is **collapsed** into `Send(OutboundMessage)` in . Feishu's adapter internally handles chunking if `OutboundMessage.Text > 4KB`. Echo does not chunk (terminal-friendly).

If you see references to `SendMessage` / `SendLongMessage` in older docs, they refer to the pre-API. The current method is `Send(ctx, OutboundMessage)`.

---

## 6. Edge cases

| 场景 | 处理 |
|------|------|
| 飞书 WebSocket 断连 | lark-oapi SDK 自动重连（指数退避）；期间 Channel.Send 阻塞 / 失败 → Gateway 不重试，log warn |
| 飞书 QPS 限流（单聊 5 QPS）| 内部 token bucket；超限排队 |
| Channel.Incoming channel 满 | sendLoop 丢弃最早消息 + warn（已实现）|
| 用户发图片 / 文件 | Channel adapter 下载到本地路径，blocks 包含 ContentImage/ContentFile；Channel.Send 第一个 OutboundMessage 时 cold-create receipt，把 attachment path 写到 receipt 文本里 |
| 用户撤回消息 | 忽略；receipt 状态保留在 Channel 内部，撤回不会触发额外清理 |
| 飞书 appSecret 错误 | Start() 返回 error，nightme 启动失败 |
| 飞书权限被回收 | WebSocket 收到权限错误事件 → Channel.Send 返回 error → 日志告警 |
| Channel.Send cold-start receipt 失败 | Channel 内部 log warn；下次 OutboundMessage 再试；永远不向上传播（Gateway 不持有 receipt） |
| Channel.Send PATCH 失败（Feishu 429 / 230020）| Channel 内部 log warn；下次事件重试；最终一致性 |
| 用户在 receipt 已 Done 后再发同 userMsgID 的消息（重发）| Channel 按新消息处理；receipt 保留终态不动，新消息触发新 turn 的新 anchor |
| Receipt handle race（两个 goroutine 同时 PATCH 同一 receipt）| Channel 内部负责 lock（Feishu MessageReceipt 有 mu）；Gateway 不持有锁 |
| Mention prefix（`@_user_N `）导致 slash command 解析失败 | Channel adapter 在构造 `Message.Text` 前 strip 开头的 `@bot_key ` / `@_all ` mention 前缀；中段 mention 不动（F-watch）|
| Group 里用户未 @ bot 的消息 | `Message.HasMention = false`；Gateway dispatcher 根据 `ChatSession.WatchMode()` 决定 drop（`WatchModeMention`）或 pass（`WatchModeAll`）。DM 下 `HasMention` 永远为 true，gate 为 no-op（F-watch）|
| OutThinking / OutToolStart / OutToolEnd 太多元素挤占 receipt card | **F-thread-route**: Feishu 把这些 Kind 路由到独立 thread reply（rootID = userMsgID），receipt card 收窄到只承载最终答复；详见 [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md)。其他 Channel 自决（折叠 section / 子节点 / drop）|

---

## 7. Test plan

### 7.1 Unit

**Channel interface**:
- mock Channel 实现满足 6-method interface（compile-time check via `var _ Channel = (*mock.Channel)(nil)`）
- Feishu adapter `MessageReceipt` 内部状态机测试：`Append(EventText)` / `Append(EventDone)` 路径
- Echo adapter: `Send` 写出预期的 log 行

**Feishu rolling-log**:
- 第一个 `OutboundMessage{ReplyTo: userMsgID}` 触发 cold-start card 创建
- 后续同 userMsgID 的事件 PATCH 同一张 card
- `OutMessageState{StateDone}` 走 `AddReaction` 路径，不动 card body
- 失败路径：cold-start 失败 → 下次 Send 重试；PATCH 失败 → log warn + 继续

**Echo**:
- 每个 `Send` 写出 log 行
- 多次 `Send` 累加在 `recorded` slice

### 7.2 Integration

- fake Channel + Gateway: 验证 `OutboundMessage.ReplyTo` 等于 `cs.currentTurnUserMsgID`
- fake Channel + ChatSession: 发 2 条消息 → 第一个 turn 的 receipt 完成前第二条入队 → 第二个 turn anchor 到第二条
- fake Channel + MessageState: 验证 `OutMessageState` 事件按预期顺序到达 (`StateReceived` → `StateForwarded` → `StateDone`)

### 7.3 E2E（M2+）

- 飞书 DM 发消息 → ⏳ reaction → 🔄 → 一张 card 持续 PATCH → ✅ reaction；card 最终内容包含全部 event 内容
- 飞书 DM 连发 3 条（在 turn 进行中）→ ⏳ 3 次 reaction；只有最后一条得 ✅；3 条合并一个 turn，card 只一张
- `nightme run --channel=echo` → 每条 event 一行 log，全部 ReplyTo=userMsgID

---

## 8. Open questions

- Receipt card 完成后是否保留还是删除？决策：**保留**。Card body 是最终内容 + ✅ emoji 即终态，用户可滚动 DM 看历史。Channel 内部不主动删除。
- 群聊多人 → 一个 userMsgID 对应一个 receipt，但 receipt card 在群里发；多人各自看到自己的 reaction。与 一致：每个 user message 一个 anchor，sender 不区分。
- 飞书 reaction emoji 累积 vs 切换：由 `MessageState` FSM 处理（append-only ⏳ → 🔄 → ✅），不再是 receipt FSM 切换。

---

## 9. Cross-references

- **OutboundMessage 结构**：见 [`SPEC.md`](../SPEC.md) §2.2 + `internal/gateway/messages.go`
- **Gateway 如何用 receipt FSM**：见 [`F-gateway.md`](./F-gateway.md) §2.4
- **Feishu QR onboarding**（app credentials 获取）：见 [`../channel/feishu-onboarding.md`](./../channel/feishu-onboarding.md)
- **输入透传（InboundMessage 怎么到 Gateway）**：见 [`F-message-flow.md`](./F-message-flow.md)

---

## 10. Change log

- **2026-08-03 (F-watch 增量)** — `InboundMessage.HasMention bool` 字段添加。Channel adapter 在 decode 时根据 `chat_type` + `Mentions` + `GetBotIdentity()` 计算；DM 永远 true；group 含 bot/@_all 时 true。Gateway dispatcher 入口根据 `cs.WatchMode()` 决定是否 drop 非 mention 群消息。Channel 不读 `WatchMode`，保持“不变式"。

