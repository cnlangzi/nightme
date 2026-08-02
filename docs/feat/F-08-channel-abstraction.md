# F-08: Channel Abstraction

> **Status**: implemented (v1.1 — receipt-lifecycle extension landed)
> **Milestone**: M2 (Feishu implementation), v0.3 (interface extension for receipts)
> **Depends on**: F-22 (Feishu One-Click App Registration) — for credentials
> **Related docs**: [`SPEC.md`](../SPEC.md) v1.1 §1.1, §1.2, §2.4; [`F-26-gateway-hub.md`](./F-26-gateway-hub.md) §3

---

## 1. Description

定义 `Channel` interface 让 nightme 支持多种 IM backend。MVP 仅实现飞书（lark-oapi-go），但 interface 设计保证后续接入 WhatsApp / Telegram / Slack / Web UI 无需改动核心逻辑。

**v1.1 职责再定义**：Channel 是**纯渲染器**（dumb renderer）。它做三件事：

1. **IM 协议编解码** — 把 native event 解码成 `InboundMessage`；把 `OutboundMessage` 编码成 native API 调用
2. **`Send(OutboundMessage)` 通用渲染** — 文本 / tool_start / tool_end / thinking / card / reaction 等所有 `OutboundKind` 的视觉表达
3. **Receipt 生命周期渲染** — `CreateReceipt / UpdateReceipt / DisposeReceipt`，按 `ReceiptState` 切视觉（pending ⏳ / executing 🔄 / done ✅ / error ❌）

**Channel 不知道**：sessions、workspaces、agents、bindings、receipt 状态机本身、slash commands。状态机的"什么时候转移"是 Gateway 的事。

---

## 2. Interface (v1.1)

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
// adapter. In v1.1 it is intentionally thin: protocol conversion + display
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

    // --- v1.1 additions: receipt lifecycle RENDERING ---

    // CreateReceipt creates a new channel-native receipt for an incoming
    // user message. Returns an opaque Receipt handle (channel-private
    // concrete type) that Gateway holds for the lifetime of the user
    // message's receipt FSM.
    //
    // The channel decides how to render the initial state (typically
    // Pending → ⏳). The blocks parameter is the structured user turn
    // (text + optional image/file attachments); the channel formats
    // these into its native UI (Feishu: receipt text message + reaction;
    // echo: log line).
    //
    // Errors propagate to Gateway, which will skip receipt tracking and
    // fall back to plain Send for that user message.
    CreateReceipt(ctx context.Context, chatID, userMsgID string, blocks []agent.ContentBlock) (Receipt, error)

    // UpdateReceipt transitions the receipt to the given state. The
    // channel renders the state in its native UI:
    //
    //   ReceiptPending   → ⏳  (or keep current emoji if already Pending)
    //   ReceiptExecuting → 🔄  (swap reaction or append indicator)
    //   ReceiptDone      → ✅  (swap reaction, optionally edit body)
    //   ReceiptError     → ❌  (swap reaction)
    //
    // Gateway is the ONLY caller; channels should not assume any
    // particular transition order. UpdateReceipt is idempotent for the
    // same state (channel may short-circuit).
    UpdateReceipt(ctx context.Context, receipt Receipt, state ReceiptState) error

    // DisposeReceipt cleans up the receipt (Feishu: delete the receipt
    // message; echo: log a dispose line; web: remove the element).
    // Called after the final UpdateReceipt(Done|Error).
    DisposeReceipt(ctx context.Context, receipt Receipt) error
}

// Receipt is an opaque handle. Each Channel returns its own concrete
// type (Feishu: *MessageReceipt; echo: *echoReceipt). Gateway treats
// it as a token and never reads or writes fields.
type Receipt interface{}

// ReceiptState is the cross-channel state enum. Gateway decides when
// to transition; Channel only renders.
type ReceiptState int

const (
    // ReceiptPending is the initial state after CreateReceipt. The user
    // message is in the system but not yet dispatched to the agent
    // (either queued in InputBuffer or being dispatched).
    ReceiptPending ReceiptState = iota

    // ReceiptExecuting means the user message has been sent to the
    // agent's PTY stdin and the agent is processing it.
    ReceiptExecuting

    // ReceiptDone means the agent has finished processing this user
    // message and the final result has been emitted.
    ReceiptDone

    // ReceiptError means the agent (or the dispatch path) failed for
    // this user message; the user should retry.
    ReceiptError
)

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
┌──────────────────────────────────────────────────────────┐
│ Channel interface (channel.go)                            │
│   - Name / Start / Stop                                   │
│   - Incoming() <-chan Message                            │
│   - Send(OutboundMessage) error                          │
│   - CreateReceipt / UpdateReceipt / DisposeReceipt       │
└──────────────────────────────────────────────────────────┘
        ↑ implements
        │
┌────────────────────┐  ┌────────────────┐
│ feishu.Adapter     │  │ echo.Channel   │
│ (receipt.go)       │  │ (no-network    │
│ - lark NewClient   │  │  stub)         │
│ - WebSocket 长连接 │  │                │
│ - MessageReceipt   │  │ echoReceipt    │
│   {messageID,      │  │  {userMsgID}   │
│    reactionID,     │  │                │
│    replyMsgID}     │  │ log only       │
└────────────────────┘  └────────────────┘
```

### 3.1 Feishu receipt rendering details

`internal/channel/feishu/receipt.go` 的实现（v1.1）：

```go
type MessageReceipt struct {
    UserMsgID  string
    ReplyMsgID string  // the receipt message nightme posted
    ReactionID string  // current reaction handle
    currentEmoji ReactionEmoji
    mu          sync.Mutex
}

// CreateReceipt posts the receipt message and adds ⏳ reaction.
func (a *Adapter) CreateReceipt(ctx, chatID, userMsgID string, blocks []agent.ContentBlock) (Receipt, error) {
    text := buildReceiptText(blocks)  // Feishu-flavored formatter, may include image refs
    replyMsgID, err := a.bot.ReplyMessage(ctx, chatID, userMsgID, text)
    if err != nil { return nil, err }

    reactionID, err := a.bot.AddReaction(ctx, chatID, userMsgID, EmojiWaiting)
    if err != nil {
        // Reaction failed; the receipt message is still useful — return without reaction.
        log.Printf("receipt: reaction add failed: %v", err)
    }

    return &MessageReceipt{
        UserMsgID: userMsgID,
        ReplyMsgID: replyMsgID,
        ReactionID: reactionID,
        currentEmoji: EmojiWaiting,
    }, nil
}

// UpdateReceipt swaps reaction according to state.
func (a *Adapter) UpdateReceipt(ctx, receipt Receipt, state ReceiptState) error {
    r := receipt.(*MessageReceipt)
    r.mu.Lock()
    defer r.mu.Unlock()

    target := emojiForState(state)
    if r.currentEmoji == target { return nil }  // idempotent

    // Delete old reaction (if any).
    if r.currentEmoji != "" && r.ReactionID != "" {
        _ = a.bot.DeleteReaction(ctx, r.UserMsgID, r.ReactionID)
    }
    // Add new reaction (track new id).
    newID, err := a.bot.AddReaction(ctx, chatIDOf(r), r.UserMsgID, target)
    if err == nil {
        r.ReactionID = newID
        r.currentEmoji = target
    }
    return err  // best-effort; Gateway doesn't retry on receipt update failure
}

// DisposeReceipt deletes the receipt message entirely.
func (a *Adapter) DisposeReceipt(ctx, receipt Receipt) error {
    r := receipt.(*MessageReceipt)
    if r.ReplyMsgID == "" { return nil }
    return a.bot.DeleteMessage(ctx, r.ReplyMsgID)
}
```

### 3.2 Echo receipt (no-network stub)

```go
type echoReceipt struct{ userMsgID string }

func (c *Channel) CreateReceipt(ctx, chatID, userMsgID string, blocks []agent.ContentBlock) (Receipt, error) {
    fmt.Fprintf(c.out, "[receipt %s] created (state=pending, chat=%s)\n", userMsgID, chatID)
    return &echoReceipt{userMsgID: userMsgID}, nil
}

func (c *Channel) UpdateReceipt(ctx, receipt Receipt, state ReceiptState) error {
    fmt.Fprintf(c.out, "[receipt %s] state=%s\n", receipt.(*echoReceipt).userMsgID, stateName(state))
    return nil
}

func (c *Channel) DisposeReceipt(ctx, receipt Receipt) error {
    fmt.Fprintf(c.out, "[receipt %s] disposed\n", receipt.(*echoReceipt).userMsgID)
    return nil
}
```

---

## 4. The "Channel is dumb" contract

This section makes the responsibility split concrete. Channel **MUST NOT** do any of the following:

| ❌ Don't | ✅ Instead |
|---------|-----------|
| Track whether a user message has been dispatched | Gateway flips `Pending → Executing` via `UpdateReceipt` |
| Decide whether to queue vs send | Session.InputBuffer decides; Gateway observes and flips state accordingly |
| Lookup session or workspace | Gateway does binding lookup, hands Channel only `chatID` + `userMsgID` + `blocks` |
| Format receipt text from blocks using shared utils | Channel takes `[]agent.ContentBlock` directly; Channel formats internally |
| Call Session or Manager | Channel never imports `session/` |

Channel **MUST**:

| ✅ Do |
|------|
| Render `OutboundMessage` kinds (text/tool_start/tool_end/thinking/card/...) in native UI |
| Render `ReceiptState` transitions visually (emoji + body changes) |
| Manage its own internal receipt handles (Feishu message ids / reaction ids) |
| Return opaque `Receipt` tokens (no public fields) |
| Handle `DisposeReceipt` as a cleanup (delete message, remove element) |
| Fail gracefully on `UpdateReceipt` errors (log + continue) — receipt UI must not block the agent event stream |

---

## 5. SendLongMessage (legacy from v0.1) — v1.1 status

The v0.1 `SendMessage` / `SendLongMessage` distinction is **collapsed** into `Send(OutboundMessage)` in v0.3+. Feishu's adapter internally handles chunking if `OutboundMessage.Text > 4KB`. Echo does not chunk (terminal-friendly).

If you see references to `SendMessage` / `SendLongMessage` in older docs, they refer to the pre-v0.3 API. The current method is `Send(ctx, OutboundMessage)`.

---

## 6. Edge cases

| 场景 | 处理 |
|------|------|
| 飞书 WebSocket 断连 | lark-oapi SDK 自动重连（指数退避）；期间 Channel.Send 阻塞 / 失败 → Gateway 不重试，log warn |
| 飞书 QPS 限流（单聊 5 QPS）| 内部 token bucket；超限排队 |
| Channel.Incoming channel 满 | sendLoop 丢弃最早消息 + warn（已实现）|
| 用户发图片 / 文件 | Channel adapter 下载到本地路径，blocks 包含 ContentImage/ContentFile；Channel.CreateReceipt 自己决定是否把路径写到 receipt 文本里 |
| 用户撤回消息 | v0.1 忽略；v1.1 receipt 已 dispose，UI 已更新 |
| 飞书 appSecret 错误 | Start() 返回 error，nightme 启动失败 |
| 飞书权限被回收 | WebSocket 收到权限错误事件 → Channel.Send 返回 error → 日志告警 |
| CreateReceipt 失败 | Gateway 跳过 receipt 簿记，走纯 Send(OutboundMessage) fallback |
| UpdateReceipt 失败 | Channel 内部 log warn；Gateway 不重试（fire-and-ack）|
| DisposeReceipt 失败 | Channel 内部 log warn；Gateway 已从 receipts map 删除条目 |
| 用户在 receipt 已 Done 后再发同 userMsgID 的 receipt（如重发）| Gateway 视为新 receipt，不与旧 receipt 关联（userMsgID 应当唯一）|
| Receipt handle race（两个 goroutine 同时 UpdateReceipt 同一 receipt）| Channel 内部负责 lock（Feishu MessageReceipt 有 mu）；Gateway 不持有锁 |

---

## 7. Test plan

### 7.1 Unit

**Channel interface**:
- mock Channel 实现满足 interface（compile-time check）
- `Receipt` 接口是空接口：测试用 `mockReceipt` struct 通过编译

**Feishu receipt**:
- `CreateReceipt` post receipt message + add reaction, returns handle
- `UpdateReceipt(_, _, Executing)` deletes ⏳ reaction + adds 🔄 reaction
- `UpdateReceipt(_, _, Done)` swaps to ✅; idempotent
- `DisposeReceipt` deletes receipt message
- 失败路径：reaction API 失败 → return error（不阻塞）

**Echo receipt**:
- 三个调用都是 log 行
- UpdateReceipt 顺序：`pending → executing → done` 三行 log
- DisposeReceipt 后 receipt 不再被引用

### 7.2 Integration

- mock Channel → Gateway → fake Session: verify `CreateReceipt` 在 fallback 路径被调用，`UpdateReceipt(executing)` 在 dispatch 立即时被调用
- mock Channel → Gateway → fake Session with Busy buffer: verify `UpdateReceipt(executing)` 在 `onFlush` 钩子被调用（**不**在 fallback 路径调用）
- mock Channel → Gateway → fake Session with EventError: verify `UpdateReceipt(error)` + `DisposeReceipt` 在 EventCallback 中被调用

### 7.3 E2E（M2+）

- 飞书 DM 发消息 → receipt message 出现 + ⏳ reaction → CLI 处理中变 🔄 → 完成变 ✅ → receipt message 删除（或保留，看 UX 决策）
- 飞书 DM 发多条消息（buffer busy 期间）→ 所有 receipt 保持 ⏳ 直到 flush → 一起变 🔄 → 一起变 ✅
- `nightme run --channel=echo` → `[receipt <id>] state=pending/executing/done` log 行按顺序出现

---

## 8. Open questions

- Receipt 完成后是否删除 receipt message 还是保留？v1.1 决策：**保留**（✅ emoji + 内容即终态），用户可滚动 DM 看历史；`DisposeReceipt` 只在 `nightme stop` / 异常退出路径清理。
- 群聊多人 → 一个 userMsgID 对应一个 receipt，但 receipt message 是在群里发的；多人各自看到自己的 reaction。是否需要 per-user receipt？v1.1 决策：no，每个 user message 一个 receipt（sender 不区分）。
- 飞书 reaction emoji 顺序（添加时间 vs 类别）：v1.1 切换（删旧加新）而非累积，UI 一致。

---

## 9. Cross-references

- **OutboundMessage 结构**：见 [`SPEC.md`](../SPEC.md) §2.2 + `internal/gateway/messages.go`
- **Gateway 如何用 receipt FSM**：见 [`F-26-gateway-hub.md`](./F-26-gateway-hub.md) §2.4
- **Feishu QR onboarding**（app credentials 获取）：见 [`F-22-feishu-onclick-registration.md`](./F-22-feishu-onclick-registration.md)
- **输入透传（InboundMessage 怎么到 Gateway）**：见 [`F-02-message-passthrough.md`](./F-02-message-passthrough.md)

---

## 10. Change log

- **2026-08-02** — v1.1: add `CreateReceipt / UpdateReceipt / DisposeReceipt` + `ReceiptState` + `Receipt` opaque type. Channel becomes a pure renderer; receipt FSM state lives at Gateway. Doc rewritten to match.
- **2026-07-31** — v0.1: original `SendMessage / SendLongMessage` interface. Replaced by `Send(OutboundMessage)` in v0.3 (kept for historical reference).