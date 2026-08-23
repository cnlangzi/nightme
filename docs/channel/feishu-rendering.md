# Feishu Channel 渲染策略 (Receipt / Thread / Footer / Card)

## A1. F-25: Rolling-Log Receipt UX (Channel-Autonomous)

> **Source**: `../channel/feishu-rendering.md`


> **Related**: [`SPEC.md`](../SPEC.md)§2.4 + §0.3 F-thread-route; [`F-08-channel-abstraction.md`](./F-08-channel-abstraction.md); [`F-gateway.md`](./F-gateway.md); [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md); [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md) — F-37 解决 `OutResult` 600 B 截断 backlog

---

## 1. Description

Each user message triggers an agent turn. The agent emits a stream
of events (`EventText`, `EventToolStart`, `EventToolEnd`, …) until
the turn ends with `EventDone` / `EventError`. The user should see
**one coherent visual artifact** for that turn — not a flurry of
separate messages.

**F-thread-route 收窄**:Receipt card 不再承载 agent turn 的全部 event,只承载**最终答复相关的 entry**(`OutText` / `OutResult` / `OutInit` / `OutUsage`)。中间过程(`OutThinking` / `OutToolStart` / `OutToolEnd`)由 Channel 路由到独立 thread reply / 折叠 section / DOM 子节点(Feishu 选择 thread reply + 类型感知摘要,详见 [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md))。Receipt card body 元素数从 ~30 降到 ≤5,Feishu 50 element 上限不再是个问题。

The **rolling-log receipt** is that artifact :

- **Per-turn scope**: one receipt object per turn (anchored to the
  single `currentTurnUserMsgID` — buffered batch flushes anchor to
  the last userMsgID in the batch).
- **Channel-native rendering**: each Channel picks the form that
  fits its platform:
  - **Feishu**: an interactive card (Card 2.0) PATCHed in place via
    `UpdateMessage` — the user sees a single bot reply growing
    under their message
  - **Slack**: a thread reply (or thread root + reactions) under
    the user's message
  - **Web**: a DOM block with `data-receipt-for="<userMsgID>"`
- **Single consumer of `OutboundMessage`**: each `OutboundMessage`
  carries `ReplyTo = currentTurnUserMsgID`; Channel routes by that
  key to its own per-userMsgID receipt object.

**Gateway sees none of this**. Gateway stamps `ReplyTo` and sends;
Channel decides everything else (storage, lifecycle, terminal state,
card body formatting).

## 2. Design Principles

### 2.1 "Abstract stays abstract, concrete stays concrete"

Gateway's job for the outbound flow is now **purely mechanical**:

```
AgentEvent
  → gateway.Translate → OutboundMessage{Kind, Text, Meta, ReplyTo}
  → ch.Send(ctx, out)
```

That's it. Three lines of behavior. No receipt map, no FSM, no
fanout. Channel does the rest.

### 2.2 1 turn : 1 anchor, n events

A turn emits N events. Each event carries the same `ReplyTo =
currentTurnUserMsgID` (the single anchor). The Channel routes every
one of them, but **routing target depends on Kind**:

```
EventText "好的,让我..."        → PATCH card with "好的,让我..."
EventToolStart "Read(/a.py)"   → thread reply "🔧 Read(/a.py)"
EventToolEnd   "..."           → thread reply "✅ Read /a.py → 1234 lines" (类型感知摘要)
EventText "...然后..."          → PATCH card with "...然后..."
EventResult   "📝 最终回复"      → PATCH card with final block
EventUsage    "1.2k tokens"     → PATCH card with footer    [F-44/F-45 后: silent drop;footer 改走 SessionContext typed field → 4 个 main-chat Kind 各自的末尾]
EventDone                       → (no PATCH; gateway handles MessageState separately)
```

The user sees **one concise card** (final answer + metadata) under
their message, with a "X replies" thread indicator. Click the
indicator → see 💭🔧✅ flow in the thread. No 30-element card
visual clutter.

### 2.3 Buffered batch → single anchor (last userMsgID)

If the user sends 3 messages while the previous turn is still in
flight, those messages are queued in InputBuffer (separate concern;
see F-27 §5). When the turn ends, `defaultFlushHookLocked` flushes
the batch and sets:

```go
cs.currentTurnUserMsgID = userMsgIDs[len(userMsgIDs)-1]
```

The agent sees the 3 messages as one combined input. All events
from that turn anchor to `userMsgID_last`. The receipt card lives
under the user's most-recent message — matching ChatGPT-style "all
3 submitted together, agent replies once under the last one" UX.

Earlier userMsgIDs in the batch keep their own `MessageState`
reactions (⏳ → 🔄, but no ✅) — terminal `MessageState(Done)` only
fires for the anchor.

## 3. Channel Implementation Contract

While each Channel can pick its own storage form, the
**observational contract** is uniform:

| Event | What Channel MUST do |
|-------|----------------------|
| First `OutboundMessage{ReplyTo: userMsgID, Kind: OutText\|OutResult\|OutInit\|OutUsage}` for a userMsgID | Cold-create the receipt (card / thread / DOM node). Idempotent on retries. |
| Subsequent `OutboundMessage{ReplyTo: userMsgID, Kind: OutText\|OutResult\|OutInit\|OutUsage}` | PATCH / update the existing receipt — append the event's content |
| `OutboundMessage{Kind: OutMessageState, Meta: {state}}` | AddReaction / DOM state / status emoji on the user's message |
| `OutboundMessage{Kind: OutText, ReplyTo: ""}` | Orphan: render as plain text (no anchor) |
| `OutboundMessage{Kind: OutChoice}` | Send as an interactive card (permission prompts etc.) — thread reply if ReplyTo set |
| `OutboundMessage{Kind: OutChoicePatch}` | **F-46 增量**: 原地 PATCH 已有交互卡（Feishu `PATCH /im/v1/messages/{id}`），用 `ReplyTo`=bot card msg id。`buildCardButtons` 在 `Disabled+ChosenChoiceEmoji` 时把选中按钮染绿 (`type: "success"` + `✓` 前缀)，没选按钮灰描边 disabled。详见 [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md) §10.2.3 |
| `OutboundMessage{Kind: OutThinking\|OutToolStart\|OutToolEnd}` | **F-thread-route**: Channel-specific routing. Feishu: post as plain text thread reply (rootID = msg.ReplyTo). Other Channels: pick their own routing (fold into receipt / separate message / drop). See [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md) §2.1. |
| ~~`OutboundMessage{Kind: OutCompaction}`~~ | ~~**F-thread-route**: Feishu: `postThreadReply(... "✶ Compacting conversation…")`~~ | **F-49 删除**：`OutCompaction` kind 整条 path 删除(runtime 不再产生此 Outbound)。详见 [`F-49 §1.9`](./../channel/feishu-rendering.md)。|

`OutTyping` 是 platform-dependent — Channels may drop it silently
(Feishu has no equivalent UX).

### 3.1 Feishu implementation reference

[`internal/channel/feishu/receipt.go`](../../internal/channel/feishu/receipt.go)
+ [`adapter.go`](../../internal/channel/feishu/adapter.go) §6:

- Receipt object: `*MessageReceipt` keyed by `userMsgID` in
  `a.receiptsByUserMsgID[userMsgID]` (NOT per-chat anymore)
- Cold-start path: `receiptFor(ctx, chatID, userMsgID)` — if no
  receipt for userMsgID, post a cold-start ⏳ card and create the
  receipt
- Patch path: `receipt.Append(ctx, AgentEvent)` — formats entry via
  `eventToEntry`, appends to entries slice, renders card body, calls
  `bot.PatchMessage(cardMsgID, body)`
- Card body budget: 30 KB cap (Feishu hard limit) → `evictOverflowLocked`
  drops oldest entries beyond the byte / count budget; card header
  shows "…(前 N 条已省略)"
- **F-thread-route**: receipt card body 只承载 OutText / OutResult /
  OutInit / OutUsage 派生的 entry;thinking/tool 走 §3.1.1 thread
  reply path(不进入 receipt)。Receipt card body 元素数从 ~30 降到 ≤5,
  Feishu 50 element 上限不再是个问题。

### 3.1.1 Thread reply path (Feishu F-thread-route)

[`internal/channel/feishu/adapter.go`](../../internal/channel/feishu/adapter.go) §6 `Send` dispatcher 按 Kind 分流:

- `OutThinking` → `postThreadReply(ctx, chatID, rootID=userMsgID, body)` (plain text)
- `OutToolStart` → `postThreadReply(... body)` (plain text, args inline)
- `OutToolEnd` → 经 `summarizeToolEnd(name, args, output, err)` 生成单行摘要 → `postThreadReply(... body)`
- ~~`OutCompaction` → `postThreadReply(... body)` (✶ Compacting...)~~ → **F-49 删除**:`OutCompaction` kind 整条 path 删除。runtime handler 在 EventCompaction 上只调 `s.RecordCompaction()` 累加计数,不产生 Outbound;channel 不发任何"压缩进行中"marker。详见 [`F-49 §1.9`](./../channel/feishu-rendering.md)。

底层走 `SendMessageText(ctx, chatID, text, rootID)` → `sendContent` → `sendViaLarkReply`(POST `/im/v1/messages/{rootID}/reply`,§13.10 已落地)。

**OutToolEnd 类型感知摘要**("decision 处理"):按 tool name 分支生成单行(不 dump 原始 output 到 thread),详见 [`../channel/feishu-rendering.md` §2.3](./../channel/feishu-rendering.md)。

### 3.2 Slack / Web / future

- Slack: implement a thread reply strategy. The first
  `OutboundMessage{ReplyTo: userMsgID}` posts the thread root;
  subsequent events post thread replies or edit the root via
  `chat.update`.
  - : thinking/tool 决策自决 —— 可走 Slack Block Kit
    的折叠 section、可走独立 thread reply、可 drop。**不**复制 Feishu 的 emoji 摘要
- Web: maintain a `Map<userMsgID, DOMElement>` keyed by
  `data-user-msg-id`. Append events as child nodes; patch parent
  on terminal state.
  - : thinking/tool 决策自决 —— DOM 子节点 + 折叠、可独立子面板、可 drop

## 4. Failure Modes

| Failure | Channel behavior | Gateway behavior |
|---------|-----------------|------------------|
| `ch.Send` returns error | Log warn; keep receipt alive; next event retries | Continue draining pump (no retry) |
| Cold-start `SendCard` fails | Receipt is nil; subsequent events log warn + drop | Channel falls back to plain text (no rolling log UX) |
| `PatchMessage` rate-limited (Feishu 230020 / 429) | Coalesce / debounce events; resync on next successful PATCH | No retry; eventual convergence |
| Receipt body exceeds Feishu 30 KB cap | `evictOverflowLocked` drops oldest entries; header shows eviction count | — |

## 5. Rate-Limit Mitigation

High-throughput agents can emit dozens of events per second. Feishu's
`UpdateMessage` API has QPS limits (typically ~100/min per app). Two
strategies:

1. **Coalesce at the Channel layer**: buffer `OutText` /
   `OutToolStart` events for ~50ms windows, then PATCH once per
   window. Lossy but visually fine — a rolling log is meant to be
   "what's happened recently", not "every microsecond".
2. **Idempotent convergence**: each PATCH sends the full current
   card body. If a PATCH fails, the next successful one carries
   the latest state. Out-of-order PATCHes converge (Feishu accepts
   the last-write-wins model).

Channels SHOULD implement coalescing internally; Gateway stays
unaware.

## 6. MessageState Interaction

`MessageState` (F-31) is **independent** of rolling-log receipt:

- **MessageState** = progress indicator on the user's message
  (⏳ → 🔄 → ✅ / ❌). Owned by ChatSession.
- **Rolling-log receipt** = content artifact under the user's message.
  Owned by Channel.

Both are keyed by `userMsgID`. They are triggered by separate
events (`OutMessageState` vs `OutText` / `OutToolStart` / …). A
failure in one does not affect the other.

See [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md) for the
full MessageState lifecycle.

## 7. /close / dispose semantics

`/close` clears the ChatSession's AgentSession pool. The Channel's
receipt objects are NOT touched — they're Channel-private state.
Feishu receipts are simply never PATCHed again; they stay as the
last-rendered visual until the user / IM backend cleans them up
(typical IM: ~24h retention, then server-side GC).

If a Channel wants to actively clean up receipts on `/close`, it
MAY subscribe to a future `/close` event (not currently exposed; ).

## 8. History

| Version | What changed |
|---------|--------------|
| / | Receipt FSM owned by Gateway (`internal/gateway.CreateReceipt / UpdateReceipt / DisposeReceipt`). Channel painted state transitions. Worked for Feishu; mismatched Slack / Web abstractions. |
| | Receipt FSM temporarily disabled; daemon sent plain `OutText` (no card UX). Documented as "known gap" in CHANGELOG. |
|  | **Receipt FSM removed from Gateway entirely.** Receipt is now a Channel-internal object keyed by `userMsgID`. Gateway stamps `OutboundMessage.ReplyTo = currentTurnUserMsgID`; each Channel routes + PATCHes its own receipt. "Abstract stays abstract, concrete stays concrete" principle applied throughout. |
| **(F-thread-route)** | **Receipt scope 收窄**: receipt card body 不再承载全部 event,只承载 OutText / OutResult / OutInit / OutUsage 派生的 entry。OutThinking / OutToolStart / OutToolEnd 路由到 Channel 自治的 thread reply(Feishu 选 thread + 类型感知摘要)。~~OutCompaction 同理走 thread reply;F-49 删除该 path——runtime 不再产生 OutCompaction,count 由 `SessionContext.CompactionCount` 携带,footer Line 1 渲染 `🗜 N`。~~ Card body 元素数从 ~30 降到 ≤5。详见 [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md) + [`channel/feishu.md` §13.12](../channel/feishu-rendering.md) + [`channel/feishu.md` §13.25](../channel/feishu-rendering.md) (F-49)。 |

---

## A2. F-31: MessageState — 消息生命周期进度跟踪

> **Source**: `../channel/feishu-rendering.md`


> **Depends on**: F-08 (Channel abstraction), F-26 (Gateway), F-27 (ChatSession), F-29 (AgentSession pool)

> **Related docs**: [`SPEC.md`](../SPEC.md)[`F-gateway.md`](./F-gateway.md), [`channel/feishu-rendering.md`](../channel/feishu-rendering.md), [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md)

## 1. Description

`MessageState` 是 nightme 对用户消息生命周期的可见性反馈机制。它回答 3 个问题：

1. **ChatSession 收到消息没有？** — `StateReceived`
2. **消息转给 AgentSession 了没有？** — `StateForwarded`
3. **AgentSession 执行完成了没有？** — `StateDone` (+ 可选 `StateError`)

每条普通用户消息在系统里流转时，会触发对应的 `MessageState` 事件；Channel 把它渲染成平台原生视觉表达（Feishu 渲染成 reaction emoji，Slack 渲染成 emoji 短码，Web UI 渲染成 DOM 元素，等等）。

---

## 2. Motivation & Problem

### 2.1 现状（不理想） 的 "reaction" 概念混在 feishu 实现里：

- `internal/channel/feishu/receipt.go` 内部维护 `currentReaction` / `appendReactionLocked`，直接调飞书 SDK
- 抽象层 `OutboundKind.OutReaction` 定义了但**没有调用方**（dead code；已删除）
- 跨 channel 实现（Slack / Web UI）需要重新实现 FSM + idempotency + 状态映射

这违反了 nightme 的职责隔离原则：**抽象架构功能被泄漏到具体 channel 实现里**。

**状态**：本 F-31 设计已落地（commit a6113d9）。`MessageState` FSM 由 ChatSession 拥有（lifecycle 触发），Gateway 转发为 `OutboundMessage{Kind: OutMessageState}`，Channel 通过 `Send` 派发到原生 API（Feishu: AddReaction; Slack: emoji shortcode; Web: DOM 元素）。Receipt FSM 完全独立处理（见 [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md)）—— 起 Receipt 概念本身从 Gateway 移除，MessageState 真正独立。

### 2.2 设计目标

1. **抽象**：MessageState 是消息的属性，跟 channel 无关
2. **统一事件**：通过 `Channel.Send(OutboundMessage)` 走现有 wire format，零新增 API surface
3. **Owner 清晰**：ChatSession 发出，Gateway 转发，Channel 适配
4. **可观测**：所有 MessageState 事件可被 logging / tracing 看到
5. **跨平台**：不同 channel 用各自的视觉表达承载同一抽象

### 2.3 与 Receipt 的关系 末注释中预想的 "MessageState 与 Receipt 两者完全独立"在 才真正成立 —— 起 **Gateway 不再持有 Receipt 概念**（见 SPEC 与 [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md)），MessageState 不再需要与 Receipt FSM 共存。

| 概念 | 跟踪什么 | Owner | 渲染载体 |
|---|---|---|---|
| **MessageState** | 消息在系统里的处理进度 | ChatSession | Channel 自己（Feishu: reaction emoji）|
| **Rolling-log receipt** | agent 响应的内容卡片 | Channel 内部 | Channel 自己（Feishu: card PATCH）|

两者的语义、owner、触发点、渲染都**完全独立**：
- 共同点：都按 `userMsgID` 索引
- 不同点：MessageState 跟踪消息本身进度；rolling-log 跟踪响应内容增长
- 触发点：MessageState 由 ChatSession lifecycle 触发（`cs.emitMessageState`）；rolling-log 由 `OutboundMessage{ReplyTo: userMsgID}` 在 Channel.Send 内部触发 cold-create / PATCH
- 互不依赖：任一失败不影响另一个

详细协议见 [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md) §6。

---

## 3. Concept

### 3.1 MessageState 定义

```go
// internal/receipt/state.go (新文件)
package receipt

// MessageState is the lifecycle stage of one inbound user message
// within nightme's processing pipeline. It answers "where is this
// message in the system right now?" so channels can render a
// user-visible progress indicator.
type MessageState int

const (
    // StateReceived: ChatSession has accepted the message but not
    // yet dispatched it to an AgentSession. Triggered on
    // ChatSession.GetOrCreate.
    StateReceived MessageState = iota

    // StateForwarded: the message has been dispatched to an
    // AgentSession (lazy spawn succeeded; blocks enqueued or
    // sent to PTY stdin). Triggered on successful
    // LookupSelectedAgentSession.
    StateForwarded

    // StateDone: the AgentSession has finished processing this
    // message (EventDone arrived on the readPump). Triggered by
    // ChatSession.runReadPump on EventDone.
    StateDone

    // StateError (optional): the AgentSession reported an error
    // for this message (EventError arrived). Triggered by
    // ChatSession.runReadPump on EventError.
    StateError
)

func (s MessageState) String() string {
    switch s {
    case StateReceived:  return "received"
    case StateForwarded: return "forwarded"
    case StateDone:      return "done"
    case StateError:     return "error"
    }
    return "unknown"
}
```

### 3.2 Scope 规则（强约束）

**MessageState 只对普通用户消息触发，不对 slash command 触发**。

| 触发源 | 是否产生 MessageState |
|---|---|
| `dispatchMessage`（普通文本消息） | ✅ 是 |
| `dispatchSlashCommand`（`/cwd` `/use` `/close` 等） | ❌ 否 |
| ChatSession lifecycle 内部事件 | ❌ 否（除非该事件由用户消息触发） |

理由：slash command 是**控制平面**，用户发命令是为了控制系统状态，不是为了让系统"处理一条消息"。控制平面有它自己的反馈（`Channel.Send(OutboundMessage{Kind: OutCommandReply})`），不需要进度标记。

### 3.3 三个核心状态语义

| 状态 | 含义 | 何时触发 | 何时结束 |
|---|---|---|---|
| `StateReceived` | 系统已收到消息，等待 dispatch | `ChatSession.GetOrCreate(chatID)` 成功后 | `StateForwarded` 或消息丢弃 |
| `StateForwarded` | 消息已转给 AgentSession，正在处理 | `ChatSession.LookupSelectedAgentSession()` 成功 | `StateDone` 或 `StateError` |
| `StateDone` | AgentSession 处理完成 | `ChatSession.runReadPump` 收到 `EventDone` | 终态 |
| `StateError` | AgentSession 处理出错 | `ChatSession.runReadPump` 收到 `EventError` | 终态 |

---

## 4. Architecture（4 层流转）

```
[1] ChatSession / AgentSession
        │  emit MessageState event (state + userMsgID)
        │  via callback mechanism
        ▼
[2] Gateway
        │  接 OnMessageState callback
        │  翻译成 OutboundMessage{Kind: OutMessageState, Meta: {message_id, state}}
        │  通过 Channel.Send 提交
        ▼
[3] Channel (feishu adapter / future slack / future web ...)
        │  在 Send dispatcher 看到 OutMessageState case
        │  state → 平台原生视觉表达
        │  Feishu: AddReaction(userMsgID, emoji_type)
        │  Slack:  reactions.add(name=":hourglass:")
        │  Web UI: DOM element change
        ▼
[4] Platform SDK (Feishu / Slack / DOM)
        │  实际的 reaction / progress bar / icon 变化
        ▼
    用户看到视觉反馈（emoji / icon / progress bar）
```

### 4.1 每层职责

| 层 | 知道什么 | 不知道什么 |
|---|---|---|
| ChatSession / AgentSession | "现在消息进入 X 状态了" | 怎么传输、谁接收、长什么样 |
| Gateway | OutboundMessage wire format；Channel interface | emoji 是什么、平台细节 |
| Channel (feishu / future) | state → emoji_type 映射；平台 SDK | 事件从哪来、谁 emit |
| Platform SDK | 平台原生 API | — |

### 4.2 与现有架构的关系

复用现有 EventHandler / Channel.Send 通道，**零新增 API surface**：

| 现有机制 | 新机制 | 关系 |
|---|---|---|
| AgentEvent → readPump → ChatSession.EventHandler → Translate → Channel.Send | ChatSession lifecycle → OnMessageState callback → Gateway → Channel.Send | 复用 transport，独立 semantics |
| `OutboundKind.OutReaction`（dead code） | `OutboundKind.OutMessageState`（替换） | enum 值替换，Send dispatcher 同步改 |
| `Channel.Send(OutboundMessage)` | 同上 | 零变化 |

---

## 5. Event Contract（抽象事件）

### 5.1 OutboundKind 枚举

```go
// internal/gateway/messages.go — 替换现有 OutReaction
type OutboundKind int

const (
    // ... existing kinds (OutText, OutToolStart, ...) ...

    OutMessageState            // MessageState event: state change for a user message
    OutMessageStateRemoved     // 撤销 MessageState 标记（可选，append-only 通常不用）
)
```

### 5.2 OutboundMessage 字段

新增 `MessageState` 字段，承载状态事件负载：

```go
type MessageStatePayload struct {
    State    receipt.MessageState  // 状态值
    Emoji    string                // 可选：channel-specific 显式 emoji（override 推导）
    // 未来扩展可加：timestamp, sequence 等
}

type OutboundMessage struct {
    // ... existing fields (ChatID, Kind, Text, Card, ReplyTo, Meta) ...

    // MessageState 是 OutMessageState kind 的 payload。
    // 其他 kind 应忽略。
    MessageState *MessageStatePayload

    // Reaction 字段保留（向后兼容）但 后不再被 OutReaction kind 使用。
    Reaction *Reaction
}
```

### 5.3 Meta 约定

```go
OutboundMessage{
    Kind: OutMessageState,
    ChatID: chatID,
    Meta: map[string]any{
        "message_id": userMsgID,           // 必填：标记哪条用户消息
        "state":      receipt.MessageState, // 必填：状态值
        // 可选扩展：
        // "timestamp":  time.Now(),         // 触发时间
        // "sequence":   int,                // 同 message 内的事件序号
    },
}
```

**约定**：Channel 必须从 `Meta["message_id"]` + `Meta["state"]` 读出渲染目标 + 状态值。`MessageState` 字段是冗余载体，便于直接读用。

---

## 6. Trigger Points（ChatSession lifecycle）

### 6.1 触发点总览

| 触发时机 | 状态 | 说明 |
|---|---|---|
| `ChatSession.GetOrCreate(chatID)` 成功后 | `StateReceived` | 消息首次进 ChatSession |
| `ChatSession.LookupSelectedAgentSession()` 成功 | `StateForwarded` | spawn 成功或命中 running pool |
| `ChatSession.runReadPump` 收到 `EventDone` | `StateDone` | agent 处理完 |
| `ChatSession.runReadPump` 收到 `EventError` | `StateError` | agent 出错 |

### 6.2 触发实现机制（伪代码）

```go
// internal/chatsession/chatsession.go

// 新增 callback 字段
type ChatSession struct {
    // ... existing fields ...

    onMessageState func(chatID, userMsgID string, state receipt.MessageState)
}

// 新增 setter
func (cs *ChatSession) SetMessageStateHandler(h func(string, string, receipt.MessageState)) {
    cs.mu.Lock()
    cs.onMessageState = h
    cs.mu.Unlock()
}

// 新增内部 emit 方法
func (cs *ChatSession) emitMessageState(userMsgID string, state receipt.MessageState) {
    cs.mu.RLock()
    h := cs.onMessageState
    chatID := cs.ChatID
    cs.mu.RUnlock()
    if h != nil {
        h(chatID, userMsgID, state)
    }
}

// 在 GetOrCreate 成功路径里调 cs.emitMessageState(userMsgID, StateReceived)
// 在 LookupSelectedAgentSession 成功路径里调 cs.emitMessageState(userMsgID, StateForwarded)
// 在 runReadPump EventDone 分支调 cs.emitMessageState(userMsgID, StateDone)
// 在 runReadPump EventError 分支调 cs.emitMessageState(userMsgID, StateError)
```

### 6.3 Runtime wiring（启动时）

```go
// cmd/nightme/run.go — startup flow

// 1. Gateway 提供 OnMessageState 方法
gw := gateway.New(messageDispatcher, nil)

// 2. 注册到每个 ChatSession
onMessageState := gw.OnMessageState    // func(chatID, userMsgID string, state receipt.MessageState)
for _, cs := range mgr.List() {
    cs.SetMessageStateHandler(onMessageState)
    // ... existing cs.SetEventHandler(eventHandler) ...
}

// 3. Gateway.OnMessageState 实现
func (g *gateway) OnMessageState(chatID, userMsgID string, state receipt.MessageState) {
    out := gateway.OutboundMessage{
        Kind:   gateway.OutMessageState,
        ChatID: chatID,
        Meta: map[string]any{
            "message_id": userMsgID,
            "state":      state,
        },
    }
    // resolve channel via chatID
    ch := g.resolveChannel(chatID)
    if ch == nil {
        return
    }
    _ = ch.Send(context.Background(), out)    // 渲染失败 log warn 不阻塞
}
```

---

## 7. Channel Implementation Contract

### 7.1 通用契约

每个 Channel adapter 必须：

1. 在 `Send` dispatcher 里识别 `OutMessageState` kind
2. 从 `Meta["message_id"]` 读渲染目标 userMsgID
3. 从 `Meta["state"]` 读状态值
4. 按 channel 特定方式渲染（emoji / progress / DOM）
5. 渲染失败：**log warn，return nil**（不阻塞消息处理主流程）
6. 不支持 MessageState 的 channel：return nil（degrade gracefully）

### 7.2 Channel 不知道

- 状态什么时候被触发（ChatSession 生命周期内部）
- 状态值怎么用（每个 channel 自己决定 emoji 映射）
- 跨 chat / 跨 session 的状态管理（每条消息独立）

### 7.3 Idempotency（同 state 不重复渲染）

**Channel 内部**维护"最近一次渲染的 state"，同 state 跳过渲染以避免网络抖动：

- Feishu：`currentMessageState` 字段跟踪
- Slack：类似
- Web UI：DOM diff 自带 idempotency

**Idempotency 是 channel 实现细节**，不在抽象层约束。Gateway 发送的语义是"我建议你渲染 state X"，channel 可以重复触发（幂等）或跳过（取决于实现）。

---

## 8. Feishu-Specific Implementation

详见 [`channel/feishu-rendering.md`](../channel/feishu-rendering.md)。摘要：

### 8.1 state → emoji_type 映射

| MessageState | emoji_type（飞书预定义） | 用户视觉 |
|---|---|---|
| `StateReceived` | `OneSecond` | ⏳ |
| `StateForwarded` | `OnIt` | 🔄 |
| `StateDone` | `DONE` | ✅ |
| `StateError` | `THUMBSUP` | ❌ (closest 预定义) |

**注意**：必须用飞书预定义 `emoji_type` 标识符，不是 unicode。传 unicode `⏳` 给飞书 reaction API 会返回 `99992354 data not found`。

### 8.2 Send dispatcher case

```go
// internal/channel/feishu/adapter.go

case gateway.OutMessageState:
    messageID, _ := msg.Meta["message_id"].(string)
    if messageID == "" {
        return errors.New("feishu: OutMessageState missing message_id")
    }
    state, ok := msg.Meta["state"].(receipt.MessageState)
    if !ok {
        return errors.New("feishu: OutMessageState missing state")
    }
    emoji := mapStateToFeishuEmoji(state)
    if emoji == "" {
        return nil    // 未知 state 静默 drop
    }
    _, err := a.AddReaction(ctx, messageID, emoji)
    return err
```

### 8.3 内部 idempotency 跟踪

`Adapter` 维护 `messageStates[messageID] → MessageState`，同 state 跳过 AddReaction 调用。

### 8.4 append-only 语义

飞书 reaction API 是 append-only（每次 AddReaction 加新 emoji，不删老 emoji）。这意味着：
- ⏳ → 🔄 → ✅ 在用户消息上**堆叠**为 3 个 emoji
- 用户看到完整的状态轨迹
- 这是飞书平台特性，channel adapter 不主动删（除非未来加 `OutMessageStateRemoved`）

---

## 9. Failure Semantics

### 9.1 渲染失败

| 失败点 | 行为 | 用户感知 |
|---|---|---|
| ChatSession.emitMessageState callback nil | 静默 drop | 无视觉反馈（debug 模式可见） |
| Gateway.OnMessageState 解析失败 | log warn | 无视觉反馈 |
| Channel.Send 调用失败 | log warn（per `cmd/nightme/run.go`） | 飞书自己后续 retry |
| Feishu AddReaction API 失败 | log warn（feishu adapter 内部） | 用户消息上缺该 state 的 emoji，但其他轨道继续工作 |

**关键不变式**：MessageState 渲染失败**永不阻塞**消息处理主流程（agent spawn / dispatch / response 都不受影响）。

### 9.2 状态机一致性

- 同一 userMsgID 的状态转换必须按 `Received → Forwarded → Done/Error` 顺序
- 反向或跳跃状态：定义不明确，**当前不处理**（log warn 即可）
- 同 state 重复触发：channel 自带 idempotency 处理

---

## 10. Test Strategy

### 10.1 抽象层测试（mock Channel）

| 测试 | 验证 |
|---|---|
| `TestChatSession_EmitsReceived_OnGetOrCreate` | GetOrCreate 后 callback 被调，参数正确 |
| `TestChatSession_EmitsForwarded_OnLookupActive` | LookupSelectedAgentSession 成功后 callback 被调 |
| `TestChatSession_EmitsDone_OnEventDone` | readPump 收到 EventDone 后 callback 被调 |
| `TestChatSession_EmitsError_OnEventError` | readPump 收到 EventError 后 callback 被调 |
| `TestChatSession_NoEmit_OnSlashCommand` | `/cwd` `/use` `/close` 不触发任何 MessageState |
| `TestGateway_TranslatesMessageState_ToOutbound` | OnMessageState 产生正确的 OutboundMessage{Kind, Meta} |
| `TestGateway_RoutesMessageState_ToCorrectChannel` | multi-channel 场景下 chatID 路由正确 |

### 10.2 Channel 实现测试

| Channel | 测试 |
|---|---|
| Feishu | `TestFeishu_OutMessageState_OneSecond` / `_OnIt` / `_DONE` 各映射 |
| Feishu | `TestFeishu_OutMessageState_Idempotency` 同 state 不重复 AddReaction |
| Feishu | `TestFeishu_OutMessageState_AddReactionFailure` log warn 不阻塞 |
| Feishu | `TestFeishu_OutMessageState_MissingMeta` error return |

### 10.3 集成测试

E2E 飞书 DM round-trip（FEATURES.md backlog 已列）：验证用户消息从发出到收到响应过程中，userMsgID 上出现 ⏳ → 🔄 → ✅ 的 emoji 序列。

---

## 11. Out of Scope

| 项 | 描述 |
|---|---|
| `OutMessageStateRemoved` 实现 | 当前 append-only 不删反应；如果未来需要（飞书 API 允许 DeleteReaction），再实现 |
| `StateError` 的 emoji 映射细化 | 当前用 THUMBSUP 兜底，未来可加专用 emoji_type |
| 状态时间戳 / 序列号 / metadata | 当前只有 message_id + state；扩展字段后续可加 |
| Receipt FSM 与 MessageState 的统一编排 | 两者独立工作，不集成；如果未来需要（如 Receipt Done 时也强制 MessageState Done）再实现 |
| Slash command 的 MessageState | scope 明确不触发 |

---

## 12. Change Log

---

## 13. References

- [`SPEC.md`](../SPEC.md)— high-level architecture
- [`F-08-channel-abstraction.md`](./F-08-channel-abstraction.md) — Channel interface
- [`F-gateway.md`](./F-gateway.md) — Gateway architecture
- [`F-chat-session.md`](./F-chat-session.md) — ChatSession lifecycle
- [`F-chat-session.md`](./F-chat-session.md) — AgentSession pool
- [`channel/feishu-rendering.md`](../channel/feishu-rendering.md) — feishu-specific implementation

---

## A3. F-37: OutToolEnd + OutThinking → Thread Reply (with Type-Aware Summary)

> **Source**: `../channel/feishu-rendering.md`


## 1. Motivation

### 1.1 折叠方案失败拍板的方案：

```
receipt card body (POST → PATCH in place)
├ header
├ 💭 [collapsible_panel] (折叠)
├ 🔧 Read(/a.py)        [collapsible_panel] (折叠)
├ ✅ Read done           [collapsible_panel] (折叠)
├ 🔧 Bash(...)           [collapsible_panel] (折叠)
├ ✅ Bash done           [collapsible_panel] (折叠)
├ 💭 [collapsible_panel] (折叠)
├ 📝 最终回复
└ footer
```

**实机问题**：

1. **元素预算爆**：Claude Code 一个 turn 调 5~10 个工具很常见 → 10 工具 × 3 entry (thinking + start + end) + N 段 thinking + 1 result = 30~50 element。Feishu card body 硬上限 50 element，**频繁撞破**触发 `evictOverflowLocked` 丢弃最早条目 → 用户看不到关键信息。
2. **视觉噪声 > 折叠收益**：折叠面板 header 自带 icon + 名字 + 一行 output 摘要。Header 文本比折叠内容还长，**用户看不到折叠前的内容就已经有信息过载感**。
3. **最终回答被挤掉**：折叠 panel 占据 card 大部分空间，最终回答（📝）和 footer（token / agent）被推到屏外，需要滚动才能看到 —— **这与"折叠让卡片聚焦答案"的设计目标背道而驰**。

### 1.2 替代方案：Thread Reply

把"中间过程"从 receipt card 移到 Feishu thread：

```
main chat:
  user_msg ⤵
  ⤵ receipt card (rootID=userMsgID)
      header (started · final state)
      💬 最终回复
      ────────────────────────
      Agent X · 1.2k tokens
  ↳ "X replies" 指示器（Feishu 自动汇总）

thread (click 指示器进入):
  💭 让我看一下...           (OutThinking)
  🔧 Read(/a.py)             (OutToolStart)
  ✅ Read /a.py → 1234 lines (OutToolEnd, 类型感知摘要)
  🔧 Bash(git status)        (OutToolStart)
  ✅ Bash git status → exit 0 (3 lines)  (OutToolEnd)
  💭 然后...                  (OutThinking)
```

**收益**：

- **Main chat 极简**：只看最终回答 + metadata。用户首要看到的就是答案。
- **Thread 自然容纳过程**：Feishu thread 没 50 element 上限；过程流按时间顺序排，用户想看就 click 指示器。
- **类型感知摘要**：OutToolEnd 不再 dump 原始 output（4KB+）到 thread，而是生成单行摘要（"Read → 1234 lines"）。thread 视觉清爽。
- **OutText / OutResult 不变**：最终回答继续走 receipt card（保持 ChatGPT-style "答案在主聊天显眼位置" UX）。

---

## 2. Design

### 2.1 Channel 按 OutboundKind 分流

Feishu adapter 在 `Send` dispatcher 按 Kind 自决 routing。  
**飞书有 3 种 reply 形态（实机验证，2026-08-04）**：

| 形态名 | Wire（HTTP body / path） | 飞书 main chat 看到 | 飞书 thread panel 看到 | 飞书响应里 `thread_id` |
|---|---|---|---|---|
| **ReplyInChat**（顶级 Create） | `POST /im/v1/messages` body `{receive_id, msg_type, content}`（**无** `root_id`） | 独立气泡（不挂任何 anchor 下） | 不在 thread panel（没有 thread 概念） | `""`（飞书不分配） |
| **ReplyInThreadAndChat** | `POST /messages/{om_M0}/reply` body `{msg_type, content}`（`reply_in_thread` **字段省略**） | **正文**（内联 reply，带回复箭头） | **同一份正文**（按时间序） | `""`（飞书不分配独立 thread，reply 只是 main chat 的一条内联消息） |
| **ReplyInThread** | `POST /messages/{om_M0}/reply` body `{msg_type, content, reply_in_thread: true}` | **"X replies" 灰条**（无正文） | **正文**（按时间序；多条 share 同一 thread） | `omt_xxx`（飞书**第一次** reply-true 时分配，之后同 root_id 复用） |

按 OutboundKind 映射（2026-08-04 ops 实机确认）：

| OutboundKind | 飞书 reply 形态 | nightme 实际行为 |
|---|---|---|
| `OutThinking` | **ReplyInThread** | 纯文本 `💭 <text>`（每 event 一条）|
| `OutToolStart` | **ReplyInThread** | 纯文本 `● <name>(<args>)`（每 event 一条）|
| `OutToolEnd` | **ReplyInThread** | 纯文本 `⎿  <summary>`（类型感知摘要）|
| ~~`OutCompaction`~~ | ~~**ReplyInThreadAndChat**~~ | ~~`✶ Compacting conversation…`~~ | **F-49 删除**：`OutCompaction` kind 整条 path 删除（无瞬时"压缩进行中"提示需求）。runtime 在 `EventCompaction` 上只调 `s.RecordCompaction()` 累加计数，不产生 Outbound。Channel 不再发任何 marker。详见 [`F-49 §1.9`](./../channel/feishu-rendering.md)。 |
| `OutText` / `OutResult` / `OutInit` / `OutUsage` | n/a（PATCH in place 不走 reply API） | 进 receipt card body |
| `OutMessageState` | n/a | AddReaction ⏳/🔄/✅/❌ 在 user msg 上 |
| `OutChoice`（permission card） | **ReplyInThreadAndChat** | 进 main chat 内联回复 |
| `OutCommandReply`（slash 回应） | **ReplyInThreadAndChat** | 进 main chat 内联回复 |
| Receipt 冷启动卡 | **ReplyInThreadAndChat** | 进 main chat 内联回复（PATCH in-place） |
| 顶级 Create 形态 | **ReplyInChat** | nightme **不**用（fallback 路径 230011/231003 退化时才走顶级 Create，详见 §15.2） |

**`reply_in_thread` 字段语义**（来自 `larkim.ReplyMessageReqBody.ReplyInThread *bool`，
SDK 注释：「是否以话题形式回复；若群聊已经是话题模式，则自动回复该条消息所在的话题」）：

- **字段省略**（`omitempty` nil 指针）→ bot 消息**在 main chat 内联显示 + 进 thread panel** = "ReplyInThreadAndChat"
- `false`（显式设 false）→ **字节级与"字段省略"不同**（多 28 字节 `"reply_in_thread":false`），但**飞书 UI 行为完全一致**（与"省略"等价）
- `true` → bot 消息**只在 thread 面板显示**，main chat 只看到 "X replies" 指示器 = "ReplyInThread"

**F-37 选 ReplyInThread**（`true`）给四条中间过程 path，让 main chat 干净只露 receipt card。

**为什么这是 Channel 自治**：Gateway 仍然只 stamp `out.ReplyTo = cs.currentTurnUserMsgID`；OutboundMessage 契约不变。Channel 看到 Kind 后自决 routing 目标（thread vs card vs reaction）。完全符合 SPEC §1.3 "抽象归抽象 / 具体归具体" 不变式。

### 2.2 Bridge 层 contract 扩展

`agent.ToolEndEvent` 加 `Args string` 字段：

```go
type ToolEndEvent struct {
    ID     string
    Name   string
    Args   string  // ← 新增：从同 message 的 tool_use block 拿
    Output string
    Err    error
    // ...
}
```

claudecode bridge 在解析 `tool_result` block 时，从**同一 message 的 content 里**找到对应的 `tool_use` block（ID 匹配），把它的 `input` 反序列化进 `ToolEndEvent.Args`。其他 bridge（pi / pty / acp / sdk）也按此 contract 填。

**为什么不只填到 OutboundMessage.Meta**：Meta 在 Gateway 翻译阶段读，bridge 不直接填 Meta。**bridge 填 event 字段，Gateway 翻译时把字段抄到 Meta**。这样 bridge contract 不依赖 Gateway 字段名。

### 2.3 类型感知摘要（"决断处理"）

Feishu adapter 包内 `summarize_tool.go` 提供 `summarizeToolEnd(name, args, output, err) string`：

| Tool Name | 摘要格式 | 示例 |
|-----------|---------|------|
| `Read` | `📄 Read <args> → <N> lines` | `📄 Read /foo/bar.go → 1234 lines` |
| `Write` | `📝 Write <args> → <N> bytes` | `📝 Write /foo.go → 5678 bytes` |
| `Edit` / `MultiEdit` | `✏️ <name> <args> → applied` | `✏️ Edit /foo.go → applied` |
| `Bash` | `💻 Bash \`<truncated args>\` → <N> lines` | `💻 Bash \`git status\` → 3 lines` |
| `Grep` | `🔍 Grep → <N> matches across <M> files` | `🔍 Grep → 12 matches across 5 files` |
| `Glob` | `📂 Glob → <N> files` | `📂 Glob → 8 files` |
| `WebFetch` | `🌐 WebFetch <args> → <N> chars fetched` | `🌐 WebFetch https://... → 4321 chars` |
| `WebSearch` | `🔎 WebSearch "<args>" → <N> results` | `🔎 WebSearch "go context" → 10 results` |
| `(default)` | `🔧 <name> → <first 200 chars of output>` | `🔧 CustomTool → first line of output...` |
| `err != nil` | `❌ <name> failed: <err.message>` | `❌ Bash failed: exit code 1` |

**不 dump 原始 output**：避免 thread 里出现 4KB+ 的文件内容 / bash 输出 / grep 匹配。

**Bash args 截断**：`truncate(args, 80)`，避免超长命令挤占 thread。

**args 缺失 fallback**：如果 bridge 没填 `Args`（旧 bridge 升级、或者 tool 不属于已知类型），用 `(name, output)` 生成摘要（不显示路径/命令）。

### 2.4 Receipt card 瘦身

`buildReceiptCard` 删 `Kind="thinking"` / `Kind="tool"` collapsible_panel 分支。Receipt card body 只承载：

- header line（状态 + 时间）
- OutText entry（assistant 中间文本，可能 0~N 个）
- OutResult entry（最终回答）
- eviction marker（如果触发）
- footer（agent · tokens）

`eventToEntry` 对以下 event 返回 `(_, false)`：
- `EventText` 且 text 以 `[思考] ` 前缀开头（thinking）
- `EventToolStart`
- `EventToolEnd`
- ~~`EventCompaction`~~（**F-49 删除**：runtime 不再产生 `OutboundMessage{Kind: OutCompaction}`，receipt 也不再有 EventCompaction 分支）

→ `MessageReceipt.entries` 收窄到只装 OutText / OutResult / OutInit / OutUsage 派生的 entry。Card body 元素数通常 ≤ 5，**50 element 上限永远不破**。

**Silent PATCH（实现细节）**：`MessageReceipt.Append` 对 `EventToolStart` / `EventToolEnd` 这两类返回 `(_, false)` 的 kind **不**写 entries，但**仍然**触发 `renderLocked`，同步 bump `eventCount` + `lastEventAt` 并 PATCH card。理由：thinking/tool 现在走 thread reply，main chat 的 card header（`🔄 ⏳ N · HH:MM:SS`）必须反映 agent 仍 busy，否则 header 会冻结在 tool 之前的时刻。~~EventCompaction 同理，但 F-49 后整条 path 删除，receipt 不再 bump。~~ PATCH 频率：每个 tool event 一次（≈ 50/min 在 hot agent 上，远低于 Feishu 1000/min rate limit）。

### 2.5 不变式

- **OutboundMessage 不动**：无新 Kind，无 Meta 字段约定（Meta 只承载数据载荷 output / err / args，不承载 routing hint）
- **Gateway 不动**：不感知 channel 分流
- **ChatSession 不动**：`currentTurnUserMsgID` 单数锚点保留
- **1 turn : 1 anchor 不变式保留**：所有 event 仍 anchor 到同一个 userMsgID；thread reply 的 rootID = userMsgID（跟 receipt card 同一个 rootID）
- **F-33 不变式保留**：nightme 数据模型不见 thread 字段（`thread_ts` / `message_thread_id` 等）；thread 路由是 Feishu SDK API 调用层面的细节
- **抽象归抽象 / 具体归具体**：thread 路由是 Feishu 自治决定，Slack / Web 各自决定怎么渲染 thinking/tool

---

## 3. Implementation

### 3.1 文件级变更清单

| 文件 | 改动 | 详细 |
|------|------|------|
| `internal/agent/agent.go` | `ToolEndEvent` 加 `Args string` 字段 + 注释 | §3.2 |
| `internal/bridge/claudecode/stream.go` | 解析 `tool_result` 时从同 message `tool_use` block 拿 args 填进 `ToolEndEvent.Args` | §3.3 |
| `internal/bridge/claudecode/claudecode_test.go` | 加 case：tool_use + tool_result 在同 message 时 `ToolEndEvent.Args` 非空 | — |
| `internal/bridge/pi/translate.go` | 同样填 `ToolEndEvent.Args`（如果 pi bridge 支持 tool_result） | — |
| `internal/channel/feishu/adapter.go` `Send` | 按 Kind 分流：thinking/tool → thread；text/result/init/usage → receipt card | §3.4 |
| `internal/channel/feishu/adapter.go` `buildReceiptCard` | 删 `Kind="thinking"` / `Kind="tool"` collapsible_panel 分支 | §3.4 |
| `internal/channel/feishu/receipt_event.go` | `eventToEntry` 对 thinking/tool 返回 `(_, false)` | §3.4 |
| `internal/channel/feishu/summarize_tool.go` | 新文件：`summarizeToolEnd` + `countLines` + `truncate` + `countUniqueFiles` helpers | §3.5 |
| `internal/channel/feishu/summarize_tool_test.go` | 新文件：覆盖各 tool 类型 + 错误分支 + args 缺失 fallback | — |
| `internal/channel/feishu/adapter_test.go` | `TestSend_OutThinking_PostsToThread` + `TestSend_OutToolStart_PostsToThread` + `TestSend_OutToolEnd_PostsToThread`（**F-49 删除** `TestSend_OutCompaction_PostsToThread`） | — |
| `internal/channel/feishu/receipt_event_test.go` | 删 thinking/tool assertion（这些走 thread 不进 receipt） | — |
| `internal/channel/feishu/adapter_test.go` | 删 `TestSend_OutThinking_AppendsWithPrefix`（§13.1 bug 修复不再需要 —— prefix 不再被 strip） | — |
| `docs/channel/feishu-rendering.md` | §13.12 新增（决策反转记录）+ §15 实施计划修订 | §4.1 |
| `docs/channel/feishu-rendering.md` | §3 contract 表更新 + §X.Y Thread reply path | §4.2 |
| `docs/feat/F-08-channel-abstraction.md` | §4 加 "Channel autonomous routing examples" + §6 边界情况表更新 | §4.3 |
| `docs/SPEC.md` | §0.3 摘要 + §1.3 新增 + §11 backlog | ✅ 已落地 |
| `CHANGELOG.md` | [Unreleased] 条目 | — |

### 3.2 `agent.ToolEndEvent.Args`

```go
type ToolEndEvent struct {
    ID string
    // Name mirrors the tool name for symmetry with ToolStartEvent.
    Name string

    // Args are the raw or structured arguments passed to the tool.
    // Bridges populate this from the corresponding tool_use block
    // (same message, ID-matched) so channel renderers can produce
    // type-aware summaries (F-37 §2.3) without re-parsing the
    // tool_result content. May be empty if the bridge couldn't
    // correlate the result with a tool_use (defensive).
    Args string

    // Output is a short textual summary of the tool's result, suitable
    // for the renderer to surface in the rolling log. Bridges should
    // populate this from the tool's stdout / structured result /
    // response payload. The renderer truncates to perEntryMaxBytes
    // before display, so bridges may pass large payloads verbatim
    // without pre-truncating.
    Output string

    // Err is non-nil when the tool failed. When Err is set, Output
    // typically holds nothing (the failure path bypasses the
    // payload); channels may use either field for display.
    Err error
    // ...
}
```

### 3.3 claudecode bridge 关联 args

当前 `tool_result` 处理：

```go
case "tool_result":
    events <- agent.AgentEvent{
        Kind: agent.EventToolEnd,
        ToolEnd: &agent.ToolEndEvent{
            ID:     block.ToolUseID,
            Name:   block.Name,
            Output: stringifyToolResult(block.Content),
        },
    }
```

**改动**：在 `case "user"` 内收集 `tool_use` block（按 ID），在 `case "tool_result"` 内查表填 Args：

```go
case "user":
    // ... existing parsing ...
    for _, block := range ev.Message.Content {
        switch block.Type {
        case "tool_use":
            toolUseArgs[block.ID] = block.Input  // ← 新增：收集 args
            // emit EventToolStart
        case "tool_result":
            args := toolUseArgs[block.ToolUseID]  // ← 新增：查表拿 args
            events <- agent.AgentEvent{
                Kind: agent.EventToolEnd,
                ToolEnd: &agent.ToolEndEvent{
                    ID:     block.ToolUseID,
                    Name:   block.Name,
                    Args:   args,                  // ← 新增
                    Output: stringifyToolResult(block.Content),
                },
            }
        }
    }
```

`toolUseArgs` 是 per-message 的局部 map（`for message` 循环内重置）。`block.Input` 是 `json.RawMessage`，按 tool name 决定是否 `json.Marshal` 成 string（vs 留 raw）。

### 3.4 Feishu adapter `Send` 分流

```go
func (a *Adapter) Send(ctx context.Context, msg gateway.OutboundMessage) error {
    switch msg.Kind {
    case gateway.OutThinking:
        body := "💭 " + msg.Text
        // replyOnly=true: 💭 不进 main chat
        return a.postThreadReply(ctx, msg.ChatID, msg.ReplyTo, body, true)

    case gateway.OutToolStart:
        name, _ := msg.Meta["tool_name"].(string)
        args, _ := msg.Meta["args"].(string)
        body := "🔧 " + formatToolStart(name, args)
        return a.postThreadReply(ctx, msg.ChatID, msg.ReplyTo, body, true)

    case gateway.OutToolEnd:
        name, _ := msg.Meta["tool_name"].(string)
        args, _ := msg.Meta["args"].(string)
        output, _ := msg.Meta["output"].(string)
        err, _ := msg.Meta["err"].(error)
        body := summarizeToolEnd(name, args, output, err)
        return a.postThreadReply(ctx, msg.ChatID, msg.ReplyTo, body, true)

    // F-49 删除 case gateway.OutCompaction: — runtime 不再产生此 Outbound,
    // 不再有 "✶ Compacting conversation…" thread reply。

    case gateway.OutText, gateway.OutResult, gateway.OutInit, gateway.OutUsage:
        // 不变：fold into receipt card
        receipt := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo)
        if receipt == nil {
            return a.sendRawOutText(ctx, msg.ChatID, msg.Text)
        }
        return receipt.Append(ctx, /* translated event */)

    // OutMessageState / OutMessageStateRemoved / OutChoice: 不变
    //   - OutChoice (permission card): reply_in_thread=false，权限卡必须 main chat 可见
    //   - OutCommandReply: reply_in_thread=false，slash 回应必须 main chat 可见
    }
}

// postThreadReply 直接 POST 到 Feishu thread。
// rootID = msg.ReplyTo = currentTurnUserMsgID。
// replyOnly=true: 内部 sendViaLarkReply 会设 body.reply_in_thread=true
// （larkim.ReplyMessageReqBody.ReplyInThread field），让消息只在线程
// 面板显示、main chat 只看见 "X replies" 指示器。
func (a *Adapter) postThreadReply(ctx context.Context, chatID, rootID, body string, replyOnly bool) error {
    if rootID == "" {
        // Orphan event (startup EventAgentConnected etc.) — fall back to top-level
        return a.sendRawOutText(ctx, chatID, body)
    }
    _, err := a.SendMessageText(ctx, chatID, body, rootID)
    return err
}
```

`SendMessageText` 已经接 `rootID` 参数（§13.10 已落地）。`postThreadReply` 只是薄包装。

**`buildReceiptCard` 删 collapsible_panel 分支**：删除 `if e.Kind == "thinking"` 和 `if e.Kind == "tool"` 两个 block，连同对应的 case label。

**`eventToEntry` 收窄**：

```go
case agent.EventText:
    text := strings.TrimSpace(ae.Text)
    if text == "" { return LogEntry{}, false }
    if strings.HasPrefix(text, thinkingPrefix) {
        // F-37: thinking 走 thread，不再 fold 进 receipt
        return LogEntry{}, false
    }
    return LogEntry{Icon: "💬", Text: truncateForLog(text, perEntryMaxBytes), Kind: "reply"}, true

case agent.EventToolStart:
    // F-37: tool_start 走 thread
    return LogEntry{}, false

case agent.EventToolEnd:
    // F-37: tool_end 走 thread (类型感知摘要)
    return LogEntry{}, false

// F-49 删除 case agent.EventCompaction: — runtime 不再产生 OutCompaction,
// receipt 不再有 EventCompaction 分支。runtime 在 EventCompaction 上只调
// s.RecordCompaction() 累加计数,无 receipt 副作用。
```

**`Gateway.translate.go` 是否需要改**？

`OutThinking` 当前由 Gateway 在 `Translate` 里把 `[思考] ` 前缀剥掉再 emit。**F-37 之后**：

- Gateway 不再剥前缀（adapter 不再依赖 receipt_event 的 prefix detection）
- adapter 直接拿 `msg.Text` 当 thread body，加 `💭 ` 前缀即可

→ **Gateway `translate.go` 简化**：删 thinkingPrefix 剥除逻辑（或保留作为兼容性 vestigial；建议保留以免未来回滚）。

**OutboundMessage.Meta 增加 `args`**：

```go
// gateway/translate.go
case agent.EventToolStart:
    return OutboundMessage{
        Kind: OutToolStart,
        Text: text,
        Meta: map[string]any{
            "tool_name": name,
            "args":      ev.ToolStart.Args,  // ← 已有,无须加
        },
    }, true

case agent.EventToolEnd:
    return OutboundMessage{
        Kind: OutToolEnd,
        Text: text,
        Meta: map[string]any{
            "tool_name": name,
            "output":    ev.ToolEnd.Output,
            "err":       ev.ToolEnd.Err,
            "args":      ev.ToolEnd.Args,  // ← 新增
        },
    }, true
```

### 3.5 `summarize_tool.go`

```go
package feishu

import (
    "fmt"
    "strings"
)

const (
    toolArgsMaxBytes = 80
    toolOutputPreviewBytes = 200
)

// summarizeToolEnd produces a one-line summary of a tool's result.
// Per-tool-type heuristics so the user sees the signal (file path,
// line count, exit status) instead of a wall of raw output.
// Falls back to byte truncation for unknown tools. Err wins over
// success path.
func summarizeToolEnd(name, args, output string, err error) string {
    if err != nil {
        return fmt.Sprintf("❌ %s failed: %s", name, err.Error())
    }
    switch strings.ToLower(name) {
    case "read":
        return fmt.Sprintf("📄 %s %s → %d lines", name, args, countLines(output))
    case "write":
        return fmt.Sprintf("📝 %s %s → %d bytes", name, args, len(output))
    case "edit", "multiedit":
        return fmt.Sprintf("✏️ %s %s → applied", name, args)
    case "bash":
        cmd := truncate(args, toolArgsMaxBytes)
        return fmt.Sprintf("💻 Bash `%s` → %d lines", cmd, countLines(output))
    case "grep":
        return fmt.Sprintf("🔍 Grep → %d matches across %d files",
            countLines(output), countUniqueFiles(output))
    case "glob":
        return fmt.Sprintf("📂 Glob → %d files", countLines(output))
    case "webfetch":
        return fmt.Sprintf("🌐 WebFetch %s → %d chars fetched", args, len(output))
    case "websearch":
        return fmt.Sprintf("🔎 WebSearch %q → %d results",
            truncate(args, toolArgsMaxBytes), countLines(output))
    default:
        return fmt.Sprintf("🔧 %s → %s", name, truncate(output, toolOutputPreviewBytes))
    }
}

func countLines(s string) int {
    if s == "" { return 0 }
    return strings.Count(s, "\n") + 1
}

func countUniqueFiles(s string) int {
    // Grep output typical format: "path/to/file:line:match"
    // We extract unique paths.
    seen := make(map[string]struct{})
    for _, line := range strings.Split(s, "\n") {
        if idx := strings.Index(line, ":"); idx > 0 {
            seen[line[:idx]] = struct{}{}
        }
    }
    return len(seen)
}

func truncate(s string, max int) string {
    if max <= 3 || len(s) <= max { return s }
    budget := max - 3
    for i := 0; i < len(s); i++ {
        if i > budget { return s[:i] + "..." }
    }
    return s
}
```

### 3.6 Receipt card 元素数（实机数据）

折叠方案下典型 agent turn (10 个工具 + 3 段 thinking + 1 result) 的 card 元素数：

```
1 (header) + 3 (thinking panel) + 10 (tool start panel) + 10 (tool end panel) + 1 (result) + 1 (hr) + 1 (footer) = 27
```

→ 已接近 50 上限；turn 再大点直接撞破。

F-thread-route 方案下同一 turn：

```
1 (header) + 0 (no OutText in receipt) + 1 (result) + 1 (hr) + 1 (footer) = 4
```

→ 永远在 50 上限内，evictOverflowLocked 永不触发。

---

## 4. Documentation Updates

### 4.1 `docs/channel/feishu-rendering.md`

- §13.12 新增：**决策反转记录**（详述折叠方案失败原因 + thread 方案收益 + OutToolEnd 类型感知摘要表 + 实施要点）
- §15 实施计划修订：删原"§13.1 + §13.6" 修复步骤，换成 F-thread-route 的实施步骤（adapter 分流 + summarize_tool.go + bridge args + receipt 瘦身 + 测试改造）
- §14 changelog 加 2026-08-04 条目

### 4.2 `docs/channel/feishu-rendering.md`

- §3 Channel Implementation Contract 表更新：
  - `OutThinking` / `OutToolStart` / `OutToolEnd` 行 → "Channel-specific (Feishu: thread reply with type-aware summary)"
  - ~~`OutCompaction` 行~~ → **F-49 删除**（runtime 不再产生此 Outbound；详见 `F-25 §3.1.1` 更新说明）
- §3.1 Feishu implementation reference 加 §3.1.1 "Thread reply path"
- §1 Description 加一段：receipt card 收窄到只承载 OutText / OutResult / OutInit / OutUsage

### 4.3 `docs/feat/F-08-channel-abstraction.md`

- §4 "Channel is dumb" contract 表加一行：`✅ 自行决定按 OutboundKind 分流（thread / card / reaction / ...）— Channel 自治范围内的渲染决策`
- §6 边界情况表新增：OutThinking / OutToolStart / OutToolEnd 走 thread（Feishu 行为）；其他 Channel 自决
- §7 Test plan 加 case：mock Channel 验证收到 OutThinking 时不调 receipt（如果它实现了 thread 分流）

### 4.4 `CHANGELOG.md`

[Unreleased] 段加 "F-thread-route" 条目（参见 §5 模板）。

---

## 5. CHANGELOG Entry Template

```markdown
### F-thread-route: OutThinking / OutToolStart / OutToolEnd → Feishu thread reply

反转折叠方案（实机验证失败：30 panel 撞破 50 element
上限、视觉噪声大于折叠收益、最终回答被挤掉）。新方案：Channel 按
OutboundKind 自决 routing——thinking/tool 直接 POST 到
Feishu thread（rootID = userMsgID），receipt card 收窄到只承载
最终答复（OutText / OutResult）+ 元数据（OutInit / OutUsage）。~~`compaction` 同理走 thread reply (`✶ Compacting conversation…`)；F-49 删除该 path——runtime 不再产生 OutCompaction，count 由 `SessionContext.CompactionCount` 携带，footer Line 1 渲染 `🗜 N`。~~

OutToolEnd 类型感知摘要（"决断处理"）：bridge 层把
`ToolEndEvent.Args` 填好；Channel 层 `summarizeToolEnd(name, args,
output, err)` 按 tool name 生成单行摘要（"📄 Read /foo.go → 1234
lines"），不 dump 原始 output 到 thread。Receipt card body 元素数
从 ~30 降到 ≤5，50 element 上限永远不破。

Bridge 层 contract 扩展：`agent.ToolEndEvent.Args string` 字段；
claudecode bridge 从同 message `tool_use` block 拿 args 填入。

不变式：OutboundMessage 不动（无新 Kind）；Gateway 不动；ChatSession
不动；`currentTurnUserMsgID` 单数锚点保留；F-33 thread 概念不进
nightme 数据模型不变式保留。

详见 [`docs/SPEC.md` §0.3](./SPEC.md) + [`docs/channel/feishu-rendering.md` §13.12](./channel/feishu-rendering.md)。
```

---

## 6. Test Plan

### 6.1 Unit

**Bridge 层**：

- `internal/bridge/claudecode/claudecode_test.go`：构造 fixture 含同 message 的 `tool_use` + `tool_result` block，验证 `ToolEndEvent.Args` 非空且内容匹配 `tool_use.input`。
- 反向 case：`tool_result` 找不到对应 `tool_use`（不同 message）→ `Args` 为空（不 panic、不报错）。
- 反向 case：`tool_use` 没有 `tool_result`（罕见）→ 不影响 emit `EventToolStart`。

**Channel 层**：

- `internal/channel/feishu/summarize_tool_test.go`：覆盖各 tool name 分支（Read / Write / Edit / Bash / Grep / Glob / WebFetch / WebSearch / default）+ 错误分支 + args 缺失 fallback。
- `internal/channel/feishu/adapter_test.go`：
  - `TestSend_OutThinking_PostsToThread`：mock `sendViaLarkReply`，验证收到 `OutThinking` 时调用 Reply endpoint（rootID = msg.ReplyTo）+ body 含 `💭` 前缀。
  - `TestSend_OutToolStart_PostsToThread`：验证收到 `OutToolStart` 时调 Reply + body 含 `🔧 <name>(<args>)`。
  - `TestSend_OutToolEnd_PostsToThread`：验证收到 `OutToolEnd` 时调 Reply + body 经 `summarizeToolEnd` 生成（含 `📄 Read /foo.go → 1234 lines` 这类格式）。
  - ~~`TestSend_OutCompaction_PostsToThread`~~：**F-49 删除**（`OutCompaction` kind 已删除）。
  - `TestSend_OutText_FoldsIntoReceipt`：回归测试，确保 OutText / OutResult / OutInit / OutUsage 仍然 fold 进 receipt（不变）。
- `internal/channel/feishu/receipt_event_test.go`：
  - 删 thinking/tool 的 entry assertion（这些 event 不再生成 entry）~~+ compaction~~
  - 加 case：调用 `eventToEntry(EventText, "[思考] foo")` 返回 `(_, false)`（不再生成 thinking entry）
- `internal/channel/feishu/receipt_test.go`：
  - 回归测试：receipt card body 不再含 `collapsible_panel` 元素
  - 加 case：receipt card body 元素数 ≤ 5 (header + result + hr + footer)

**Gateway 层**：

- `internal/gateway/translate_test.go`：
  - 加 case：`Translate(EventToolEnd{Name: "Read", Args: "/foo.go", Output: "..."})` 返回 `OutboundMessage{Kind: OutToolEnd, Meta["args"]: "/foo.go"}`

### 6.2 集成

- 端到端 mock Channel + Gateway：发一条消息 → agent turn 产出 1 个 OutThinking + 1 个 OutToolStart + 1 个 OutToolEnd → mock Channel 收到 3 条 OutboundMessage → 验证 mock 的 Send 函数被调 3 次，且每次 Kind 不同。
- Receipt 端到端：mock Channel 收到 OutText + OutResult → 验证 receipt card body 只有这俩 entry，无 collapsible_panel。

### 6.3 E2E（实机飞书 DM）

- DM 发消息 → agent turn 调 3 个工具（Read / Bash / Edit）→ 验证：
  - Main chat：receipt card 显示最终回答 + 1 个 thread indicator "4 replies"（💭 thinking + 3 工具 messages）
  - Click thread indicator → 看到 5 条 thread messages：💭 + 🔧 Read + ✅ Read → ... + 🔧 Bash + ✅ Bash → ... + 🔧 Edit + ✅ Edit → ...
  - 不再有 30 个 collapsible_panel 视觉噪声
- DM 发消息 → agent turn 只产生 thinking 无工具调用 → 验证：receipt card + thread 里只有 💭（无 🔧/✅）
- DM 发消息 → agent turn 出错（tool failed） → 验证：thread 里 `❌ Bash failed: exit code 1`

---

## 7. Backlog

- **OutThinking 多 chunk 聚合**：agent turn 里 OutThinking 经常多次 emit（每段推理一个）。当前每段一条 thread reply → thread 里 N 条 💭。可聚合成 "💭 N 段" 单条消息（streaming 模式，最后一段更新），减少 thread 噪声。**不在本 PR scope**。
- **未知 tool type 摘要策略**：默认走字节截断（200 chars）。如果 claudecode 后续新增 tool name，channel 自动 fallback 到默认路径。**无需额外工作**。
- **Web UI / Slack 适配**：本决策仅影响 Feishu adapter。其他 channel（如未来 Web / Slack）应各自决定怎么渲染 OutThinking / OutTool* —— **不**复制 Feishu 的 thread 方案，保持各自平台原生 UX。
- **Thread reply 失败 vs receipt card 失败**：当前 thread reply 失败 → log warn + drop（不影响 receipt card）。如果某些场景下 thread reply 必须成功（比如用户依赖 thread 看过程），可以加重试 / fallback。但 MVP 不需要，backlog。

---

## 7.5 实机飞书群验证（2026-08-04，Frtpilot-Xiage）

| 发送 | 形态 | 飞书响应 | main chat UI 实际显示 |
|---|---|---|---|
| `[probe-A]` Create 顶级 (`oc_4a06da49bc0131ff14b381498e4fed9d`） | ReplyInChat | `message_id=om_xxx, parent_id="", root_id="", thread_id=""` | 独立气泡，不挂 M0 下 ✓ |
| `[probe-B]` Reply to M0，省略 reply_in_thread | ReplyInThreadAndChat | `parent_id=M0, root_id=M0, thread_id=""` | main chat 显示**正文内联**（带回复箭头），thread panel 也有 ✓ |
| `[probe-D]` Reply to M0, reply_in_thread=true | ReplyInThread | `parent_id=M0, root_id=M0, thread_id=omt_19141bf7110e1c89` | main chat 只显示 "X replies" 灰条，**正文只在线程里** ✓ |
| `[probe-D2/D3/D4]` 续发 3 条 reply-true | ReplyInThread | 全部 `thread_id=omt_19141bf7110e1c89`（共享） | 4 条 D share 同一个 thread，main chat 看到 "4 replies" 灰条 |

**关键发现**：

1. **顶级 Create 不分配 thread_id**（飞书响应 `thread_id=""`）——这跟 mock 假设的"self-root"不同。
2. **ReplyInThread + Also send it to chat 也不分配 thread_id**（B 响应 `thread_id=""`）——只是 main chat 的内联 reply。
3. **ReplyInThread 才分配独立 thread_id**（D 响应 `thread_id=omt_19141bf7110e1c89`）——之后同 root_id 的 reply-true 复用此 thread。
4. **msg.ReplyTo 必须始终是 M0**（当前用户消息 id）—— 4 条 D 全部 reply M0，**不**chain reply 到上一条 D；这反向验证 §13.10 / F-33 "单数锚点 currentTurnUserMsgID" 不变式**真的必要**（如果链式 reply，thread 碎裂成 N 个独立 "1 reply" 指示器，UI 不再汇总）。

Probe 工具代码：`cmd/_probe/feishu_thread_probe.go`（mock 版）+ `cmd/_probe/send_one/main.go`（真实发送版）。决策落地后建议删除（保留实机飞书响应记录在本节即可）。

---

## 8. Change log

- **2026-08-04 (a)** — F-37 草案（Devin 拍板反转 §13.6 折叠方案）。Docs 落地（SPEC + 本 doc + channel/feishu §13.12 + F-25 §3 收窄 + F-08 §4 自治路由例子 + CHANGELOG）。代码改动 backlog §3.1。
- **2026-08-04 (b)** — 实机飞书群（Frtpilot-Xiage）验证 "Reply / ReplyInThread+Also send it to chat / ReplyInThread" 三种形态；用 `cmd/_probe/send_one` 直接发 8 条组合消息，把命名固化进 §2.1 表格和 §7.5 实验记录；记录 `thread_id` 分配规则（顶级 / default-reply 不分配；reply-true 分配并复用）。
- **2026-08-04 (c)** — `reply_in_thread` 字段"省略 vs 显式 false"字节差异（28B）发现：`TestSend_ChatVisibleEvents_PassReplyInThreadFalse` 单测 + 代码注释固化"if replyInThread { .ReplyInThread(true) }" 必须保持的纪律。

---

## A4. F-38: Claude Task Checklist in Feishu Receipt

> **Source**: `../channel/feishu-rendering.md`


> **Depends on**: F-24 (Claude Code bridge), F-25 (Channel-owned receipt), F-37 (tool thread routing), SPEC §1.4 (typed boundary)
> **Related**: [`SPEC.md`](../SPEC.md) / §1.3 / §2.2; [`channel/feishu-rendering.md`](../channel/feishu-rendering.md) / §18

---

## 1. Motivation

Claude Code exposes `TaskCreate` and `TaskUpdate` tools for maintaining a task list. In `--output-format stream-json` they are **not dedicated top-level event types**. They arrive as normal `tool_use` blocks in an assistant message and are followed by a matching `tool_result` block in a user message.

nightme currently translates both tools through the generic tool path:

```text
assistant.tool_use  → EventToolStart → OutToolStart → Feishu thread `● TaskCreate(...)`
user.tool_result    → EventToolEnd   → OutToolEnd   → Feishu thread `⎿ ...`
```

This preserves protocol visibility but loses the product-level concept: users need a compact task checklist in the receipt card, not two low-level thread lines per task mutation.

F-38 adds a generic typed task concept without moving Claude-specific names into Gateway or Channel.

## 2. Observed Claude Code contract

The stream-json schema is not officially documented as a stable wire contract. The following shape is observed in Claude Code 2.1.220 and must be locked by fixtures.

### 2.1 TaskCreate

```json
{
  "type": "assistant",
  "message": {
    "role": "assistant",
    "content": [{
      "type": "tool_use",
      "id": "toolu_create_1",
      "name": "TaskCreate",
      "input": {
        "subject": "Implement task checklist",
        "description": "Render Claude tasks in the Feishu receipt",
        "activeForm": "Implementing task checklist",
        "metadata": {}
      }
    }]
  }
}
```

The assigned task ID is not present in the input. It arrives in the successful result text:

```text
Task #1 created successfully: Implement task checklist
```

### 2.2 TaskUpdate

```json
{
  "type": "assistant",
  "message": {
    "role": "assistant",
    "content": [{
      "type": "tool_use",
      "id": "toolu_update_1",
      "name": "TaskUpdate",
      "input": {
        "taskId": "1",
        "status": "in_progress"
      }
    }]
  }
}
```

TaskUpdate may also carry `subject`, `description`, `activeForm`, `owner`, dependency and metadata fields. A normal success result starts with `Updated task #<id>`; deletion may use a dedicated success phrase. A result with `is_error=true`, a known failure phrase, or an unrecognised shape must not mutate task state.

## 3. Locked decisions

### D1 — emit only after a successful tool_result

The bridge records the pending operation at `tool_use` time but emits no task event yet. It mutates task state only after the matching `tool_result` confirms success.

Reasons:

- TaskCreate has no stable ID until its result.
- TaskUpdate can fail or be vetoed.
- Optimistic UI would display state that never existed.

### D2 — use the provider-assigned task ID

The TaskCreate result is parsed for the real ID. Subject/content hashes are forbidden: duplicate subjects collide and cannot correlate with later numeric/string `taskId` updates.

### D3 — bridge owns normalized provider-session task state

The Claude bridge keeps:

```text
tasks:     taskID → generic TaskItem
taskOrder: stable creation order
```

After every confirmed create/update/delete it emits the **complete current snapshot**. This state is process/session-local and is not added to ChatSession or Registry.

### D4 — every task event carries a full snapshot

`TaskUpdate` is a delta, but a new receipt may not have seen the original TaskCreate. Full snapshots make each outbound event self-contained and keep Gateway stateless.

```go
type TaskStatus int

const (
    TaskPending TaskStatus = iota
    TaskInProgress
    TaskCompleted
)

type TaskItem struct {
    ID         string
    Subject    string
    ActiveForm string
    Status     TaskStatus
}

type TaskListEvent struct {
    Items []TaskItem
}
```

Both `EventTaskCreate` and `EventTaskUpdate` carry `*TaskListEvent`.

### D5 — typed Gateway contract

Gateway adds `OutTaskCreate` / `OutTaskUpdate` and `OutboundMessage.TaskList *agent.TaskListEvent`. It does not parse Claude input, store task state, format glyphs, or know receipt layout.

This is not a concrete-schema leak: task ID, subject and pending/in-progress/completed are generic agent concepts. The Claude field names `taskId` and `activeForm` stay inside `bridge/claudecode`.

### D6 — successful task tools do not also become thread replies

On a confirmed task operation the bridge emits the task event only. It suppresses the generic ToolStart/ToolEnd pair, avoiding duplicate UI. On parse failure or protocol drift it logs a warning and degrades to a generic ToolEnd so the operation is still visible.

### D7 — Feishu renders one dedicated checklist element

The receipt stores only the latest copied snapshot. Tasks are not `LogEntry` history and are not individually evicted.

Card order:

```text
header
answer/result entries
one task-checklist markdown element
footer divider + footer
```

Keeping the answer first preserves the F-thread-route decision that the final response is the receipt's primary content.

## 4. End-to-end flow

```text
Claude assistant.tool_use(TaskCreate/TaskUpdate)
  → bridge caches pending operation by tool_use_id
Claude user.tool_result
  → correlate pending operation
  → verify success
  → update bridge task map/order
  → emit EventTaskCreate or EventTaskUpdate with full snapshot
ChatSession readPump
  → current turn userMsgID
Gateway.Translate
  → OutTaskCreate / OutTaskUpdate + typed TaskList
runtime EventHandler
  → out.ReplyTo = currentTurnUserMsgID
Feishu Adapter.Send
  → receiptFor(chatID, ReplyTo)
  → receipt.SetTaskList(snapshot)
  → PATCH the existing receipt card
```

No Channel interface, ChatSession, binding, registry, or receipt map API changes are required.

## 5. Feishu checklist UX

### 5.1 Status mapping

The Feishu receipt renders the task snapshot as a standard markdown todo list. Feishu's `lark_md` parses the leading `[ ]` / `[x]` as a checkbox, so the user sees a real interactive checklist. Display order is in-progress, pending, completed; order within each group follows bridge task order.

- [ ] `pending` — render as `- [ ] Subject` (open checkbox)
  - [ ] Insert before `completed` rows; after `in_progress` rows
- [ ] `in_progress` — render as `- [ ] Subject (ActiveForm)` (open checkbox + grey note)
  - [ ] Suffix `(ActiveForm)` is appended only when the field is non-empty
  - [ ] Insert FIRST in the checklist (highest visual priority)
- [ ] `completed` — render as `- [x] Subject` (closed checkbox)
  - [ ] Insert LAST in the checklist (lowest visual priority)
- [ ] `deleted` — bridge-only; the receipt MUST NOT render any row with this status (defensive filter in `buildTaskChecklistChunks`)

### 5.2 Capacity

The entire checklist is one markdown card element. It must fit within `divTextCharLimit` and the receipt's existing 24KB defensive body budget.

When content exceeds the checklist budget:

1. keep in-progress tasks;
2. keep pending tasks;
3. include completed tasks only while space remains;
4. append `…另有 N 项任务`.

The card element calculation must reserve one element when a checklist is present and keep `body.elements <= 50`.

### 5.3 Idempotency

- Bridge upserts by real task ID and emits deterministic snapshots.
- Receipt replaces its copied snapshot wholesale.
- Identical snapshots produce identical card JSON.
- Existing `renderLocked` body diff skips duplicate PATCH calls.

## 6. Failure and compatibility behavior

| Scenario | Behavior |
|---|---|
| `tool_result.is_error=true` | Do not mutate tasks; degrade to generic tool result visibility. |
| Unknown success text / protocol drift | Warn with tool name/use ID; do not guess an ID or status; generic ToolEnd fallback. |
| Update for an unknown task ID | Create a placeholder subject `Task #<id>` and apply the confirmed status; later hydration may replace it. |
| Delete success | Remove the task from map/order and emit an update snapshot; an empty snapshot clears the checklist. |
| Duplicate result | Pending operation has already been removed; ignore/log rather than applying twice. |
| Late task event after receipt completed | Drop, matching existing late-event semantics. |
| Daemon restart / resumed external task list | Bridge state starts empty. TaskList hydration is follow-up unless a stable fixture-backed parser is available. |

## 7. Implementation scope

### Agent / Gateway

- `internal/agent/agent.go`: task types, event kinds and payload.
- `internal/gateway/messages.go`: outbound kinds and typed payload.
- `internal/gateway/translate.go`: pure mappings.

### Claude bridge

- `internal/bridge/claudecode/stream.go`: pending tool correlation and result dispatch.
- `internal/bridge/claudecode/task.go`: Claude-native parsing, success confirmation, task state and snapshots.

### Feishu

- `internal/channel/feishu/adapter.go`: outbound routing and card insertion.
- `internal/channel/feishu/receipt.go`: latest task snapshot and setter.
- `internal/channel/feishu/receipt_task.go`: bounded checklist renderer.

## 8. Test plan

### Bridge

- tool_use does not emit an optimistic task event;
- create success extracts real ID;
- failed/unrecognised result leaves state unchanged and falls back;
- multiple creates accumulate;
- update changes status/subject/activeForm;
- delete removes task;
- out-of-order results correlate by tool_use_id;
- pending records are removed;
- task success emits no generic task thread events.

### Gateway

- both task kinds map correctly;
- nil payload drops;
- empty update snapshot is preserved for clear semantics.

### Feishu

- cold-create and PATCH reuse the same receipt;
- status glyphs, ordering and ActiveForm are correct;
- checklist is after answer and before footer;
- duplicate snapshot skips PATCH;
- large checklist shows omitted count;
- total card elements never exceed 50;
- task events never call thread reply.

### E2E smoke

In a Feishu group, ask Claude to create and complete several tasks. Verify one main receipt card is PATCHed, task tool lines do not appear in the thread, and the final answer stays above the checklist.

## 9. Out of scope / follow-up

- `TaskList` hydration for resumed/cross-session task lists;
- legacy `TodoWrite` normalization;
- ACP / Pi / PTY task primitives;
- clickable checklist rows or user-driven task mutation;
- task persistence in nightme Registry;
- cross-turn updates to old receipt cards.

## 10. Change log

---

## A5. F-39: OutResult → Independent Reply (Receipt 不再 fold 最终答复)

> **Source**: `../channel/feishu-rendering.md`


## 0. 背景

### 0.1 旧结构(被推翻)

F-37 之前,receipt card 是 rolling-log events 容器:

```
User msg → Receipt Card (单张,反复 PATCH)
   ├ ⏳ / 🔄 / ✅  header (state 变化)
   ├ 💬 💬 💬    OutText 流式 chunks
   ├ 📝          OutResult 完整文本 (eventToEntry.EventResult 输出)
   ├ ✅ done / ❌  EventDone / EventError 状态转换
   └ <hr> + footer (Agent · cwd · tokens)
```

F-37 把 thinking/tool 移到 thread receipt 后:

```
User msg → Receipt Card (单张)
   ├ header
   ├ 💬 💬 💬    OutText 流式 chunks
   ├ 📝          OutResult
   └ <hr> + footer
```

### 0.2 dedup 协调 — 这次踩到的实际 bug

为避免"流式 chunk + 最终 result"在 receipt 内显示重复,`eventToEntry.EventResult` 有 dedup:

```go
// internal/channel/feishu/receipt_event.go:113-124 (旧)
if !ae.Result.IsError && lastEntry != nil &&
    lastEntry.Kind == "reply" &&
    lastEntry.Text == truncateForLog(text, perEntryMaxBytes) {
    return LogEntry{}, false   // ← OutResult 直接静默丢
}
```

`perEntryMaxBytes = 600`。Claude Code stream-json 的语义:`result.result` 是 assistant 流式累积的最终文本,与最后一条 `assistant` event 的 `content[0].text` **字节级相等**。落到 receipt 端:

- 最后一条 `EventText` entry 经 `truncateForLog(text, 600)` 处理 → "前 600 字 + …"
- 同样 `truncateForLog(resultText, 600)` → "前 600 字 + …"
- 两侧字节级相等 → dedup 触发 → **OutResult entry 不加**

**用户实际行为**:长答复(> 600 字)场景下收不到完整 📝 行,只看到 N 条碎裂的"前 600 字 + …" 💬 行。这就是 user 实机报告的"**答复被截断了**"现象(与 element 数无关,F-39 §0.3 详述)。

### 0.3 元素数问题 ≠ 真正的截断因

F-thread-route (commit 098fdb7) 落地后,receipt card 只承载 OutText / OutResult / 状态 header / footer / 任务清单。典型一轮 turn:

| 来源 | 元素 |
|---|---|
| header (state) | 1 |
| OutText chunk N (各 ≤ 600 char) | N (单 div,600 < 1000 不需拆) |
| OutResult (旧路径) | 1-8 (F-37 multi-div) |
| task checklist (F-38) | 0-3 |
| `<hr>` + footer | 2 |

**典型 8-25 元素,50 元素上限几乎从不撞。** envelope 30 KB 也几乎从不撞(45 × 600 ≈ 27 KB 边缘)。**真正的"截断"是 dedup,不是 element / envelope。**

### 0.4 候选方案

| 方案 | 描述 | 改动 |
|---|---|---|
| (a) 加 dedup 判定 `len(text) <= perEntryMaxBytes` | 让短 result 仍 dedup,长 result 强制进 receipt | 1 行 + 测试 |
| (b) **OutResult 独立 reply(本 feature)** | OutResult 不进 receipt;独立 helper 渲染 markdown 投递 | adapter.go + 2 个新文件 + ~200 行 |
| (c) 维持现状继续修 dedup 边界 | 只动 receipt_event.go | 中等 |

**选择 (b)** 因为:

1. **彻底消除 dedup 需要协同工作的对象**(根本没"流式 chunk + 最终 result 同一 surface"的协调问题)
2. **架构上与 cc-connect / openclaw-lark 对齐**(业界已验证的"streaming card for progress, complete reply for deliverable"模式)
3. **打开 envelope 真撞墙的降级路径**(独立 helper 可以做 30 KB hard cap + fallback,无需保护 receipt 内的其他 entry)
4. **治了"用户偶尔看到一条独立 text 气泡 + 一张卡"的 race**(旧路径 cold-start 失败 → `sendRawOutText` fallback,可能跟后续成功的 receipt card 并存;新路径无此 race)

### 0.5 不可变约束

- **`OutboundMessage` 契约不变**:`Kind: OutResult` + `Result *agent.ResultEvent` typed field 保留 (§1.4 边界规范)
- **Gateway 不动**:`gateway/translate.go::Translate` 不需改
- **ChatSession 不动**:per-turn / per-chat 状态机无变化
- **抽象归抽象**:Channel 自治范围内决定渲染目标(从 receipt card 转独立 reply 是 Channel 决策,不影响抽象层)
- **`OutboundMessage.ReplyTo` 不动**:`currentTurnUserMsgID` 仍作为锚点,新 reply 也锚到同 userMsgID

---

## 1. 设计

### 1.1 视觉对比

**改前**:

```
user_msg om_A
  └ Receipt Card ⤓ (thread 视觉连接到 om_A; visible in main chat)
      ⏳ 等待中
      💬 前 600 字 + …
      💬 前 600 字 + …
      📝 ???  ← dedup 静默吞,长答复看不到这里
      <hr>
      footer
```

**改后**:

```
user_msg om_A
  ├ Receipt Card ⤓ (rolling log, 锚定 om_A)
  │   ⏳ → 🔄 处理中 → ✅ 已完成 10:11:11
  │   💬 前 600 字 + …
  │   💬 前 600 字 + …
  │   <hr> + footer (agent · workspace · tokens)
  └ Final Result Reply ⤓ (锚定 om_A; 独立 message; 富 markdown 渲染)
      📝 完整 OutResult text (无 600 cap,无 dedup)
```

Receipt card 退化为"事件日志 + 元数据",final answer 独立成为"答案交付"。**两条独立 surface,无需 dedup。**

### 1.2 Dispatch(三段式,抄 cc-connect)

`sendResultAsReply` helper 的 dispatch 逻辑(与 cc-connect `buildReplyContent` 镜像):

```
            ┌─ has <at ...> OR no markdown indicators
            │   → MsgTypeText (无 markdown → 单 plain text bubble)
            │
sanitize    ├─ markdown 且 tables > 5
  ↓         │   → MsgTypePost + tag:"md"  (GFM 渲染,无 Card 2.0 表格硬限)
content     │       (Feishu post 内容通常 unlimited; 长短都接受)
            │
            └─ markdown 且 tables ≤ 5
                → MsgTypeInteractive (Card 2.0)
                  elements: [
                    {tag:"markdown", content: chunk 1},  ← F-37 splitMarkdownForDivs
                    {tag:"markdown", content: chunk 2},  ← 1000 runes/div hard cap
                    ...,
                  ]
                  sanitized via SanitizeCardMarkdown
```

**关键决定**:

- **`MsgTypeText` 极少用** —— Claude Code 几乎永远输出 markdown(text chunk 含 `` ``` ``, `*`, `` `- `` 等),走 plain text 会丢失代码块、链接、列表、表格
- **`MsgTypePost` + `md` 兜底多表** —— Card 2.0 表格硬限 5 张,超出返 11310 错误;Post + md 是 GFM 全套且无该限制
- **Card 2.0 + single/multi-div 主力** —— 默认路径,SanitizeCardMarkdown 处理(URL / fence / heading / image strip),splitMarkdownForDivs 处理超长(从 receipt.go 复用)

### 1.3 Envelope 防御

每个 result reply 在 `sendContent` 前过 byte budget check:

```go
const resultCardEnvelopeBudget = 28 * 1024  // 30 KB - 2 KB headroom

if len(body) > resultCardEnvelopeBudget {
    log.Warn(...)
    truncated := truncateRunes(sanitized, int(resultCardEnvelopeBudget/3))
    msgType, body = buildResultPayload(truncated)
}
```

OutResult 经 `perEntryMaxRunes = 8000` cap(CJK 3 B/char ≈ 24 KB,远低于 30 KB envelope),实际撞 envelope 概率低;这里是 defensive fallback。

### 1.4 状态机变化

`MessageReceipt` 状态机不变(Waiting → Executing → Completed → Error)。新增一条触发点:当 `OutResult` 到达时,**先调 `receipt.SetCompleted(ctx)`** 把 receipt 标记为终态 ✅,然后投递独立 result reply。两个动作**原子性不强**(中间失败有日志兜底),但顺序保证:

1. 用户先看到 receipt card 切到"✅ 已完成 HH:MM:SS"(滚动日志收尾)
2. 用户后看到 Final Result Reply(完整最终答复)

视觉顺序在 Feishu 消息流上是 PATCH → Send,API 调用顺序与用户感知顺序一致。

---

## 2. 文件 & 接口

### 2.1 新文件

**`internal/channel/feishu/card_sanitize.go`** —— markdown sanitize pipeline(移植 cc-connect):

```go
// SanitizeCardMarkdown 是 result reply 内容的统一入口处理。Pipeline:
//   1. URL sanitize     — non-HTTP(S) link → plain text (避免 230001 invalid href)
//   2. Fence newline    — ``` 前必须 newline,否则 lark_md 当 inline code 渲染
//   3. Image strip      — 删 ![alt](not-img_xxx),只留 Feishu image_key
//   4. Heading demotion — H1 → H4, H2-H6 → H5 (lark_md heading 范围窄)
//   5. Code-block protect — ```block``` 在所有变换中保护不动
//
// 来自 cc-connect platform/feishu/feishu.go:3017-3104 (preprocessFeishuMarkdown / 
// sanitizeMarkdownURLs / stripInvalidFeishuCardImages / optimizeFeishuCardMarkdown)。
// Nightme 不需要 cc-connect 的 `<at>` 处理(已在 Gateway 层 / mention.go 解决)。
func SanitizeCardMarkdown(text string) string
```

**`internal/channel/feishu/result_render.go`** —— result reply 渲染 helper:

```go
// containsMarkdown 检测 markdown 指示符(抄 cc-connect)
func containsMarkdown(s string) bool

// countMarkdownTables 超过 maxCardTables(5)就走 Post (抄 cc-connect)
func countMarkdownTables(s string) int

// buildPostMdJSON  Post + tag:"md" 渲染
func buildPostMdJSON(content string) string

// buildResultCardJSON  Card 2.0 + 多 markdown 元素(用 splitMarkdownForDivs 拆)
func buildResultCardJSON(content string) (string, error)

// buildResultPayload 三段 dispatch(返回 msgType + body + error)
func buildResultPayload(sanitized string) (string, string, error)

// truncateRunes 字数 cap(避免 envelope 撞墙)
func truncateRunes(s string, maxRunes int) string
```

### 2.2 改动的文件

**`internal/channel/feishu/adapter.go`** —— `Send(case gateway.OutResult)` 重写 + 新增 `sendResultAsReply`:

```go
// (1) 新 helper
func (a *Adapter) sendResultAsReply(
    ctx context.Context, chatID, userMsgID, text string, replyOnly bool,
) error

// (2) Send(OutResult) case 改写
case gateway.OutResult:
    if msg.Result == nil { return errors.New(...) }
    text := msg.Result.Text
    if text == "" && !msg.Result.IsError { return nil }
    if r := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo); r != nil {
        _ = r.SetCompleted(ctx)
    }
    if msg.Result.IsError { text = "❌ " + text }
    return a.sendResultAsReply(ctx, msg.ChatID, msg.ReplyTo, text, false)
```

**`internal/channel/feishu/receipt_event.go`** —— 删 dedup 协调 + 删 EventResult case:

```go
// (1) case agent.EventResult 整段删除 (101-140)
//
//     adapter.go::Send(OutResult) 改走后,EventResult 不再进 receipt.Append。
//     eventToEntry 此 case 永不命中,defensive 也不需要 —— 删干净。
//
// (2) case agent.EventText dedup 比较键也不需要(不再是同一 surface 的竞争)
//
//     保持单文件 minimal 改动,不动 EventTool*/Usage/Compaction/Init。
```

### 2.3 保留不变的(确认无副作用)

- `gateway.OutboundMessage{Kind: OutResult, Result: ...}` —— 契约不变
- `gateway/translate.go::Translate(EventResult)` —— 不动
- `chatSession` 状态机 —— 不动
- `MessageReceipt.SetCompleted` / `Append(EventDone)` / `Append(EventError)` —— 不动
- `OutText` 路径(`case agent.EventText`)—— 不动
- `OutInit` / `OutUsage` 路径 —— 不动(receipt 仍承载 metadata)
- F-37 `splitMarkdownForDivs` —— 复用,只是换了 caller
- F-38 `task checklist` —— 不动
- F-thread-route(thinking/tool → thread reply)—— 不动

---

## 3. 测试覆盖

### 3.1 单元测试

| 文件 | 用例 | 断言 |
|---|---|---|
| `internal/channel/feishu/card_sanitize_test.go` | `TestSanitize_URL_NonHTTPToText` | `[x](relative)` → plain text `[x](relative)` |
| 同上 | `TestSanitize_URL_HTTPKeep` | `[x](https://...)` → 仍 link |
| 同上 | `TestSanitize_FenceMissingNewline_Injected` | `text<NL>\`\`\`code` 路径不变;`text\`\`\`code` 自动插 newline |
| 同上 | `TestSanitize_HeadingDemote_H2H6ToH5` | `## ` → `##### ` |
| 同上 | `TestSanitize_HeadingDemote_H1ToH4` | `# ` → `#### ` |
| 同上 | `TestSanitize_ImageStrip_NonFeishuKey` | `![x](https://...)` → 删除 |
| 同上 | `TestSanitize_ImageKeep_ImgPrefix` | `![x](img_xxx)` → 保留 |
| 同上 | `TestSanitize_CodeBlockProtected` | ``` ```go ... ``` ``` 内部 H1 行不被 demote |
| `internal/channel/feishu/result_render_test.go` | `TestContainsMarkdown_True` | 含 ` ``` `,`**`, `- ` 等 → true |
| 同上 | `TestContainsMarkdown_False_Plain` | 仅普通文字 → false |
| 同上 | `TestCountMarkdownTables_None` | 0 |
| 同上 | `TestCountMarkdownTables_Five` | 5 |
| 同上 | `TestCountMarkdownTables_Six` | 6 (超限) |
| 同上 | `TestBuildPostMdJSON_Shape` | output 包 zh_cn.content[0][0].tag="md" |
| 同上 | `TestBuildResultCardJSON_SingleDiv` | text < 1000 runes → 1 markdown element |
| 同上 | `TestBuildResultCardJSON_MultiDiv` | text > 1000 runes → N markdown elements,每个 ≤ 1000 runes |
| 同上 | `TestBuildResultPayload_NoMarkdown_UsesText` | 无 markdown → MsgTypeText |
| 同上 | `TestBuildResultPayload_LotsTables_UsesPost` | 6 表 → MsgTypePost |
| 同上 | `TestBuildResultPayload_Default_UsesCard` | 默认 → MsgTypeInteractive |
| 同上 | `TestTruncateRunes_KeepsUnder` | 2000 chars → ≤ maxRunes |
| `internal/channel/feishu/adapter_test.go` | `TestSend_OutResult_GoesToNewReply_NotReceipt` | mock SDK;调 Send(OutResult) → 1 次 sendContent,**0 次** receipt.Append |
| 同上 | `TestSend_OutResult_ClosesReceiptFirst` | mock receipt;验证 SetCompleted 调在 sendContent 之前 |
| 同上 | `TestSend_OutResult_LongText_UsesCard2_0` | text 5000 runes → MsgTypeInteractive + Card 2.0 envelope |
| 同上 | `TestSend_OutResult_VeryLongText_TruncatesToEnvelope` | text 10000 runes → 进入 28 KB budget 路径,log warn + truncate |
| 同上 | `TestSend_OutResult_EmptySkipped` | text="",!IsError → return nil,no send |
| 同上 | `TestSend_OutResult_IsError_PrefixedWithIcon` | text + "❌ " 前缀 |
| 同上 | `TestSend_OutResult_Orphan_NoUserMsgID_TopLevel` | userMsgID="" → 走 sendRawOutText fallback |
| `internal/channel/feishu/receipt_event_test.go` | `TestEventToEntry_NoEventResultCase` | 删除后:eventToEntry 没有 EventResult 命中;`(_, false)` 验证 |
| 同上 | 更新 `TestTruncateForLog_RuneAware`:删 EventResult 相关断言(只剩 EventText / EventError 用) |

### 3.2 集成测试(收尾阶段)

`internal/channel/feishu/adapter_test.go` 全量回归 `TestReceipt_*`,确认 OutText 仍 fold 进 receipt、OutInit/OutUsage 仍进入 footer、OutToolStart/End/Thinking 仍走 thread。

### 3.3 E2E(可选)

`internal/channel/feishu/e2e_test.go`(如未存在可暂缓):用 mock SDK 模拟完整 turn:

```
1. user 发消息
2. agent 流式 OutText × 3 (各 300 char, total 900)
3. agent OutResult 5000 char
4. mock SDK 记录所有 call

断言:
  - 1 SendMessageText(receiptFor 之前)
    (可能还有其他 thread reply / reaction 等)
  - 1 PatchMessage(... body 含 3 个 💬 entry + footer)
  - 1 SendCard/SendMessageText(... body 含 OutResult 5000 char, MsgTypeInteractive)
  - receipt.GetState() == Completed
```

---

## 4. 落地顺序

每步独立 commit,可单独 review + revert:

| Step | 内容 | 文件 | 风险 |
|---|---|---|---|
| 1 | **本文档**(`../channel/feishu-rendering.md`) | `docs/feat/` | 零 |
| 2 | SPEC.md §0.x + §12 更新 | `docs/SPEC.md` | 零 |
| 3 | channel/feishu.md + §12 更新 | `docs/channel/feishu-rendering.md` | 零 |
| 4 | `card_sanitize.go` 移植 | `internal/channel/feishu/card_sanitize.go`(新) | 低 |
| 5 | `result_render.go` 移植 | `internal/channel/feishu/result_render.go`(新) | 低 |
| 6 | `Send(OutResult)` + `sendResultAsReply` | `internal/channel/feishu/adapter.go`(改) | 中 |
| 7 | 删除 dedup + EventResult case | `internal/channel/feishu/receipt_event.go`(改) | 低 |
| 8 | `card_sanitize_test.go` 全覆盖 | 新文件 | 零 |
| 9 | `result_render_test.go` 全覆盖 | 新文件 | 零 |
| 10 | `adapter_test.go` 新增 OutResult 系用例 | 改 | 零 |
| 11 | `receipt_event_test.go` 调整 | 改 | 零 |

---

## 5. 与上下游契约

### 5.1 OutboundMessage 契约

不变。`Kind: OutResult` 仍存在,`Result *agent.ResultEvent` typed field 不动。Channel 自决渲染(specific 实现从"fold into receipt card"改为"independent reply")完全在 §1.4 边界规范允许范围。

### 5.2 ChatSession 状态机

不变。`OutResult` 仍由 `cs.EventCallback` 触发 → `gateway.Translate` → `channel.Send`。Channel 内的渲染分支改了,但状态机意义不变。

### 5.3 Tool thread 路由(F-37 tool-routing)

不动。F-thread-route 描述的是 thinking/tool/compaction,**已经独立**于 OutResult。F-39 加 OutResult 也独立后,两类 "独立 thread / 独立 reply" 平行。

### 5.4 多 div 拆分(F-37 multi-div)

仍用于 `buildResultCardJSON`(单 helper).F-37 在 receipt 内的多 div 拆不再服务于 OutResult,但仍服务于任何 OutText chunk > 1000 chars 的极端 case。`splitMarkdownForDivs` 函数保留,只是 caller 减少。

---

## 6. 后续工作(本文档不做)

- **退 splitMarkdownForDivs**(P1-1 in prior discussion):如果 telemetry 显示 envelope 真撞墙(实际低概率,因为 OutResult 不再 fold receipt,receipt 内 OutText ≤ 600 char/条 × ≤ 45 条 ≈ 27 KB 边缘),进一步考虑纯 envelope 防御 + fallback.
- **Header color 改 design**(P2):footer 的 inline `<text_tag>` / `<font>` 改用 `header.template = "neutral"/"red"`,对齐 cc-connect / openclaw-lark.
- **CardKit streaming**(可选):openclaw-lark 用了 [CardKit streaming API](https://github.com/larksuite/openclaw-lark/blob/main/src/card/streaming-card-controller.ts),飞书原生支持 server-side delta update,sender 只送 delta.这一改动需要飞书 [CardKit API](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/cardkit-introduction) 接入,跟 F-39 独立,作为更彻底 envelope 治本方案.

---

## 7. 不变式总结(本文档特殊要求,F-37 反转同款)

**F-39 反转 §0.1 / §0.2 旧结构,但保留:**

- OutboundMessage 不变(`Kind: OutResult`, `Result *agent.ResultEvent`)
- Gateway 不变(`Translate` 仍产 OutboundMessage)
- ChatSession 不变(emit MessageState 逻辑不动)
- `currentTurnUserMsgID` 单数锚点保留(`ReplyTo` 仍等于此)
- 1 turn : 1 anchor 不变式保留
- §1.4 边界规范保留(OutResult 字段是 typed `agent.ResultEvent`,Channel 自决 target)
- 抽象归抽象 / 具体归具体原则保留(独立 reply 是 Feishu 自治)

---

## A6. F-38: Tool-event Thread-Merge + `/tools on|off` Toggle

> **Source**: `../channel/feishu-rendering.md`


---

## 1. Problem

F-thread-route (commit `098fdb7`) sends every `OutToolStart` and
`OutToolEnd` as its own thread reply under the user message:

```
● Bash(go build ./... 2>&1)
⎿  💻 Bash → 3 lines
```

This is correct per-pair UX (matches Claude Code's terminal
two-line format), but scales poorly:

- **Visual noise**: a hot agent that calls 10 tools in one turn
  produces **20** thread replies — the user's thread becomes
  unreadable.
- **Rate-limit cost**: each reply hits the per-chat 5 QPS bucket
  independently. A 10-tool turn blocks 2s on rate-limit waiting
  alone (10 starts + 10 ends ÷ 5 QPS = 4s; the limiter actually
  makes it 4s worst case since each Create goes through the
  limiter sequentially).
- **No opt-out**: unlike `/think` (OutThinking), there is no
  per-chat toggle to disable tool display entirely.

F-38 solves both:

1. **Merge**: each tool pair becomes **one** thread reply (call
   line + result line), PATCHed in-place by the matching End.
2. **Toggle**: `/tools on|off` controls whether tool events
   reach the Channel at all. Default off.

---

## 2. Design

### 2.1 Per-chat `ToolsMode`

Mirrors `ThinkMode` (F-think) and `WatchMode` (F-watch) — a small
`int` enum on `ChatSession` that the runtime EventHandler reads
after Translate + ReplyTo stamping and before `ch.Send`.

| Value | Meaning | Trigger |
|-------|---------|---------|
| `ToolsModeHide` (default, 0) | Drop `OutToolStart` and `OutToolEnd` at the gate | `/tools off` or fresh chat |
| `ToolsModeShow` (1) | Forward to Channel; Feishu adapter merges each pair | `/tools on` |

Default direction is **the same** as `ToolsMode` (both
quiet by default):
- `/think` defaults to `ThinkModeHide` (off by default — thinking
  spam is loud; opt in with `/think on`).
- `/tools` defaults to `ToolsModeHide` (tool spam is the loudest
  part of the agent progress stream; quiet by default; opt in to
  see tools).

Both defaults are "safe" by hiding the noisiest parts of the
agent stream; users opt in to whichever they want surfaced.
The default was historically `ThinkModeShow` for `ThinkMode`
(preserve F-thread-route UX), but that flipped to
`ThinkModeHide` — see CHANGELOG. Rationale (post-flip):
- Thinking + tool-call events are both high-volume, low-signal
  during the bulk of an agent turn; surfacing them by default
  overwhelms the user-message thread with mid-turn noise.
- Both are recoverable: the receipt card always carries the
  final answer, and `/think on` / `/tools on` opt each in.
- Visual symmetry between the two toggles (both off, both opt-
  in) is easier to teach than "off, off, but thinking on."

### 2.2 Slash command

```
/tools on           → set ToolsModeShow, persist, reply
/tools off          → set ToolsModeHide, persist, reply
/tools              → reply current mode + usage hint
/tools maybe        → reply "Unknown tools mode" (parse fail, no state mutation)
/tools <other>      → same
```

Aliases `show` / `hide` accepted alongside `on` / `off` per the
`/think` precedent — users pick whichever phrasing they remember.

### 2.3 Runtime gate

`cmd/nightme/run.go::newEventHandler` — immediately after the
existing `ThinkMode` gate:

```go
if (out.Kind == gateway.OutToolStart || out.Kind == gateway.OutToolEnd) &&
    cs != nil && cs.ToolsMode() == chatsession.ToolsModeHide {
    logger.Info("tools dropped", "chat_id", chatID, "kind", out.Kind.String())
    return
}
```

Other `OutboundKind`s (`OutText` / `OutResult` / `OutThinking` /
`OutCompaction` / `OutInit` / `OutUsage`) are unaffected. The
gate is strictly KKind-scoped — no accidental widening to "drop
everything" if a future refactor touches it.

`ToolsMode` and `ThinkMode` gates are **independent**: setting
one doesn't affect the other. Tested by
`TestEventHandler_ToolsAndThinkGatesIndependent`.

### 2.4 Channel-internal merge (Feishu only)

When `ToolsMode=Show` and the Channel is Feishu, the adapter
merges each tool pair into a single thread reply. The flow:

**OutToolStart**:
1. Format call line: `formatToolStartCall(name, args)` → `"● Bash(ls)"`
2. `postThreadReplyWithID(...)` → posts the text reply, returns `message_id`
3. `pushToolStart(userMsgID, message_id, body)` — FIFO push onto
   `toolEventBuf[userMsgID]`

**OutToolEnd**:
1. Format result line: `summarizeToolResult(name, output, err)` → `"⎿  💻 Bash → 3 lines"`
2. `popToolStart(userMsgID)` — FIFO pop returns `(startMsgID, startBody)` or miss
3. On hit: `mergeToolReply(startMsgID, startBody + "\n" + resultBody)` —
   PATCH the start reply with the merged body (F-36 transient retry wraps it)
4. On miss (orphan End) or PATCH failure: fall back to
   `postThreadReply` (post result as a fresh thread reply) so the
   data is never silently dropped.

The user sees:

```
● Bash(ls)
⎿  💻 Bash → 3 lines
```

as a single chat message under the receipt card. 10 tools in a
turn = 10 thread replies (one per tool, merged), not 20.

**The merge is Feishu-specific Channel rendering** — it lives
entirely in `internal/channel/feishu/`. Other Channels (Echo,
future Slack / Web) see `OutboundMessage.Tool` unchanged and
decide their own rendering.

---

## 3. Data Flow

### 3.1 `/tools off` (default) — events dropped

```
EventToolStart / EventToolEnd
  → Translate → OutboundMessage{Kind: OutToolStart/End}
  → EventHandler gate (cs.ToolsMode()==Hide) → return
  → 飞书 no side effect; receipt card still carries final answer
```

### 3.2 `/tools on` — events merged

```
EventToolStart
  → Translate → OutboundMessage{Kind: OutToolStart}
  → EventHandler gate pass-through
  → Adapter.Send:
       postThreadReplyWithID(● Tool(args))    // Create
       buf[userMsgID] push (startMsgID, body)
       receiptFor + Touch (header keeps ticking)

EventToolEnd (matching pair, FIFO order)
  → Translate → OutboundMessage{Kind: OutToolEnd}
  → EventHandler gate pass-through
  → Adapter.Send:
       pop buf[userMsgID] → (startMsgID, startBody)
       mergeToolReply(startMsgID, startBody + "\n" + resultBody)
         → Feishu PATCH same message_id (F-36 retry)
       receiptFor + Touch

EventToolEnd (orphan — no matching Start in buffer)
  → Translate → OutboundMessage{Kind: OutToolEnd}
  → EventHandler gate pass-through
  → Adapter.Send:
       pop buf[userMsgID] → miss
       fallback: postThreadReply(⎿ ...)        // fresh reply, old behaviour
       receiptFor + Touch
```

### 3.3 PATCH failure path

```
mergeToolReply returns err (retry exhausted or non-transient)
  → log warn ("tool merge PATCH failed, falling back to fresh thread reply")
  → fall through to fallback postThreadReply
  → Send returns nil (data preserved via fallback)
```

The fallback preserves the **pre-F-38** behaviour for the unhappy
path: an orphan End, or a Start whose PATCH target became invalid
(message deleted by user, message_id typo, etc.) still shows up
to the user. **No silent data loss.**

---

## 4. State Lifecycle

### 4.1 `toolEventBuf` (per-adapter in-memory)

| Event | Effect on `toolEventBuf[userMsgID]` |
|-------|-------------------------------------|
| `OutToolStart` posted, msg_id returned | Push `(startMsgID, startBody)` onto the FIFO |
| `OutToolStart` posted but msg_id empty (orphan path) | No-op (push refused) |
| `OutToolEnd` matched (FIFO non-empty) | Pop front entry; PATCH; user sees merged body |
| `OutToolEnd` orphan (FIFO empty) | No buffer change; fallback to fresh reply |
| `Adapter.Stop` | `clearAllToolEvents()` drops every entry |

The buffer is **bounded** by `tools-per-turn` (typically <50). No
explicit turn-end cleanup is needed because `userMsgID` is unique
per turn (SPEC §2.2 invariant). Stale entries can only appear if
the same `userMsgID` is reused after a partial flush — discarded
on next push (rare edge case; not currently triggered).

`clearToolEvents(userMsgID)` is exposed for future turn-end hooks
(e.g. when a `OutDone` / `OutError` event is added to the
dispatch path) but currently is not auto-called.

### 4.2 `ChatSession.ToolsMode`

| Event | Effect on `cs.ToolsMode` |
|-------|---------------------------|
| `ChatSession.New(chatID, primaryAgent)` | Seeded to `ToolsModeHide` (default) |
| `Manager.RestoreFromRegistry` reads entry | Restored from `entry.ToolsMode` (0 == Hide) |
| `cs.SetToolsMode(mode)` | Mutated + persisted via `persistChatEntry` |
| `/tools on` / `/tools off` | Calls `cs.SetToolsMode(...)` |
| Restart (daemon `nightme run` exit + relaunch) | Restored from `chat_sessions.json` |

Persistence is `omitempty`-guarded: a fresh chat writes no
`toolsMode` key to disk. Old `chat_sessions.json` files
(pre-F-38) without the field decode to `ToolsModeHide` via
Go's zero-value semantics.

---

## 5. Concurrency

### 5.1 Single-consumer guarantee preserved

`AgentSession.Events()` has exactly one consumer — the
AgentSession's own `readPump`, which calls
`ChatSession.EventCallback` synchronously (SPEC §1.3, Q14).
ChatSession calls `cs.SetEventHandler(...)` once at startup; the
handler closure is the single writer to `outbound` events.

Within the handler:
- `Translate` (gateway) is single-threaded per ChatSession.
- `ch.Send` (Feishu adapter) is single-threaded per ChatSession.

So `toolEventBuf[userMsgID]` push/pop is effectively single-threaded
within a chat. The adapter still takes `a.mu` for the map op
because the runtime can have multiple chats in flight, and `a.mu`
guards the adapter's other shared state (`receipts`,
`messageStates`).

### 5.2 No new goroutines

The merge is fully synchronous inside `Send`. No timers, no
background flushers, no eviction sweeps. The buffer dies with
the adapter on `Stop`.

### 5.3 PATCH under load

Feishu's PATCH endpoint counts against the same per-chat 5 QPS
bucket as Create. `mergeToolReply` is intentionally NOT gated by
`threadReplyLimiter` because:

- The Create path already gates each new start thread reply.
- PATCH fires once per tool pair (≤ 1 per turn-second) — well
  below the 5 QPS bucket even for the hottest agent.
- Adding a limiter would serialize PATCH behind Create in the
  same chat, doubling the latency for no observable benefit.

If a future workload exceeds the bucket, add a `Wait()` on
`a.threadReplyLimiter` inside `mergeTextViaUpdate` before the
SDK call. Documented inline.

---

## 6. API Constraints (verified)

### 6.1 Feishu PUT /im/v1/messages/{id}

- **Supports** text and post message types (thread replies count
  as text when posted via the reply API path).
- **Limit**: 20 edits per message (well above any tool-call
  burst — Claude Code's per-turn tool count is typically <50,
  and the edit count per Start is exactly 1).
- **Editable time window**: Feishu's 24-hour edit window
  comfortably covers any realistic tool latency.
- **Sender restriction**: only the bot that created the message
  can edit it — trivially satisfied (we edit our own replies).
- **msg_type match**: cannot edit text → card or vice versa —
  satisfied (we edit text with text).

Sources:
- <https://open.feishu.cn/document/server-docs/im-v1/message/update>
- F-37 review noted the same API for card-PATCH.

### 6.2 No new `OutboundMessage` fields

§1.4 boundary rule: tool concept remains a typed `ToolInfo`
struct with `Name` / `Args` / `Output` / `Err`. The merge is
purely Feishu-side rendering — Gateway still emits the same
two events.

---

## 7. Failure Modes & Fallbacks

| Failure | Behaviour |
|---------|-----------|
| Orphan `OutToolEnd` (buffer empty for userMsgID) | Fallback: post resultBody as fresh thread reply (pre-F-38 UX) |
| `mergeToolReply` retry exhausted (F-36 transient) | Fallback: post resultBody as fresh thread reply + warn log |
| `mergeToolReply` non-transient error | Fallback: post resultBody as fresh thread reply + warn log |
| `pushToolStart` empty msg_id (orphan path — rootID was "") | push is a no-op; matching End falls back to fresh reply |
| `Adapter.Stop` mid-turn | `clearAllToolEvents` drops buffer; orphan Starts lose their End (acceptable — daemon is going down anyway) |
| Parallel `tool_use` blocks in one message | FIFO pairing ensures each End edits the correct Start's msg_id |
| Cross-turn orphan End (different userMsgID) | Falls back to fresh reply — never cross-matches turns (1 turn : 1 userMsgID invariant, SPEC §2.2) |
| Feishu PATCH 230071 (sender mismatch) | Rare — message was created by another bot. Retry returns error → fallback |
| Feishu PATCH 230072 (>20 edits) | Theoretically possible after 20 PATCHes on the same msg_id. Fallback still preserves data via fresh reply |

**No silent data loss** in any branch — at worst, a tool pair
becomes two thread replies again (pre-F-38 UX).

---

## 8. Files Touched

| File | Change |
|------|--------|
| `internal/chatsession/tools_mode.go` | NEW — `ToolsMode` enum + `ParseToolsMode` + `String`（F-102 重构后从 `internal/registry/tools_mode.go` 搬过来；不再有 alias 文件） |
| `internal/chatsession/tools_mode_test.go` | NEW — round-trip, missing-field default, omitempty on zero, type-safety |
| `internal/registry/chat_session_entry.go` | Add `ToolsMode int` field with `omitempty`（F-102 后由 `agent.ToolsMode` 改为裸 int） |
| `internal/chatsession/chatsession.go` | Add `toolsMode` field, default `ToolsModeHide` in `New()`, `SetToolsMode` / `ToolsMode()`, persistence cast `int(cs.toolsMode)` |
| `internal/chatsession/manager.go` | `RestoreFromRegistry` restores `cs.toolsMode = ToolsMode(entry.ToolsMode)` |
| `internal/command/tools/cmd.go` | NEW — `/tools` slash command (`Handle` method；与 `think/cmd.go` 同形态) |
| `internal/command/tools/commands_test.go` | NEW — 6 sub-tests covering toggle, aliases, lazy-create, registration, default, independence |
| `internal/command/commander.go` + `cmd/nightme/run.go` | Register `tools` command alongside `think`（走 `command.Commander` 注册表，不再用单点 `RegisterChatSessionCommands`） |
| `cmd/nightme/run.go::newEventHandler` | Add `ToolsMode` gate after `ThinkMode` gate |
| `cmd/nightme/run_test.go` | 5 new tests: Show-passes-through, Hide-drops-both, Hide-doesn't-affect-other, persists-across-invocations, Tools+Think-independent; existing `HideDoesNotAffectOtherKinds` updated to opt into `/tools on` for the OutToolStart assertion |
| `internal/channel/feishu/adapter.go` | Add `toolEventBuf` + `mergeTextFunc` fields; new `postThreadReplyWithID` helper; `OutToolStart` / `OutToolEnd` cases rewritten to merge; `Adapter.Stop` calls `clearAllToolEvents` |
| `internal/channel/feishu/tool_thread_merge.go` | NEW — `toolEventEntry`, `pushToolStart`, `popToolStart`, `clearToolEvents`, `clearAllToolEvents`, `mergeToolReply`, `mergeTextViaUpdate` |
| `internal/channel/feishu/tool_thread_merge_test.go` | NEW — 9 sub-tests covering FIFO, miss, empty msg_id no-op, clear, parallel tool_use, cross-turn isolation, PATCH failure fallback, orphan End fallback, defensive empty-msg_id guard |
| `docs/SPEC.md` | §0.7 changelog + §3.1.3 design section |

---

## 9. Out of Scope

- **Per-tool toggle**: not currently planned. `ToolsMode` is
  binary (all tool events on / off). A finer-grained "show only
  Bash / Read, hide Edit / Write" toggle is feasible later but
  out of scope for F-38.
- **Cross-channel merge**: Echo / Slack / Web all render
  `OutboundMessage.Tool` unchanged. The merge is a Feishu
  adapter detail — other Channels can opt in later without
  changes to Gateway or ChatSession.
- **Tool output preview**: the result line is always a single-
  line summary (`summarizeToolResult`); full tool output never
  reaches the Channel. Unchanged from F-thread-route.
- **Auto-disable after N turns**: not planned. User opt-out is
  always explicit (`/tools off`).

---

## 10. Backwards Compatibility

- `chat_sessions.json` files written before F-38 lack the
  `toolsMode` key. Go's zero-value semantics give them
  `ToolsModeHide` (the new safe default). No migration script
  needed.
- `OutboundMessage` shape unchanged. Gateway callers don't
  observe a difference — the merge is purely adapter-side.
- `Channel` interface unchanged. New helpers (`postThreadReplyWithID`,
  `mergeTextFunc`, etc.) are adapter-private.
- The runtime's `/tools off` default is a **change** in user-
  visible behaviour: pre-F-38, every user saw tool events in
  the thread. Post-F-38, only users who explicitly ran
  `/tools on` see them. This is the **intent** of F-38 ("tool
  spam is the loudest part of the agent stream — quiet by
  default") but is worth calling out in CHANGELOG.

---

## A7. F-40: OutReply 超限改独立 Reply + OutText → OutReply 改名

> **Source**: `../channel/feishu-rendering.md`


---

## 0. 背景

### 0.1 现状(被改)

F-37 / F-38 / F-39 之后,receipt card 是 rolling-log events 容器:

```
User msg → Receipt Card (单张,反复 PATCH)
   ├ ⏳ / 🔄 / ✅  header (state 变化)
   ├ 💬 💬 💬    OutText 流式 chunks (各 ≤ 600 字节)
   ├ ✅ done / ❌  EventDone / EventError 状态转换
   ├ task checklist (F-38)
   └ <hr> + footer (agent · cwd · tokens)    [F-44 后: header/entries/footer/hr 全部删除;F-45 后: footer 改走 SessionContext typed field → 4 个 main-chat Kind 文末,详见 F-44 §1.4 + F-45 §1.4]

User msg → Final Result Reply (F-39 独立 reply, 锚同 userMsgID)
   └ 📝 完整 OutResult text    [F-45 后: + formatSessionFooter 拼文末]
```

`OutText` 是 agent **对** user 当前 turn 的 reply 主体(流式 chunks),由 `cmd/nightme/run.go::responder.Send` 在每次 `EventText` 时投递。但它名义叫"Text"——`text` 在 OutboundKind 体系里是最弱泛化的名字,跟 F-38 加 `OutTaskCreate / OutTaskUpdate`、F-39 加 OutResult 独立 reply 后,体系需要更准确的命名:

- `OutText` = agent 对当前 turn 的 reply chunks → **`OutReply`** 更准确
- `OutResult` = 最终完整 reply(F-39 独立)
- `OutUsage` / `OutInit` / `OutMessageState` / `OutCommandReply` 等都有专门名字

### 0.2 600 字节截断 — 这次的丢字 bug

`eventToEntry(EventText)` 当前对所有流式 chunk 走 `truncateForLog(text, perEntryMaxBytes=600)`:

```go
// internal/channel/feishu/receipt_event.go:52-57 (现状)
case agent.EventText:
    text := strings.TrimSpace(ae.Text)
    if text == "" { return LogEntry{}, false }
    if strings.HasPrefix(text, thinkingPrefix) { return LogEntry{}, false }
    return LogEntry{
        Time: now,
        Icon: "💬",
        Text: truncateForLog(text, perEntryMaxBytes),  // ← 600 字节截断
        Kind: "reply",
    }, true
```

`perEntryMaxBytes = 600`。Claude Code stream-json 单 chunk 常见 800-2000 字节(代码示例、文档引用、Markdown 表格行等),落到 receipt 端:

- 800-2000 字节 → "前 600 字 + …"
- 多个连续 chunks → 全部被截,用户看到"N 条碎裂的'前 600 字 + …' 💬 行"

**用户实际行为**:长 reply 场景下看不到完整内容。F-39 修了 OutResult 路径(最终答复独立 reply,无 600 cap),但 **OutReply 流式 chunk 仍在 receipt 内被截**——同一 turn 里 OutResult 完整、OutReply 流式碎裂,UX 不一致。

### 0.3 真正的问题不是 element / envelope

跟 F-39 thread routing 同样的诊断:

| 来源 | 元素 |
|---|---|
| header (state) | 1 |
| OutReply chunk N (各 ≤ 2000 char) | N(单 div,1000 char 内不需拆;1000-2000 拆 2 div) |
| task checklist (F-38) | 0-3 |
| `<hr>` + footer | 2 |

**典型 5-15 元素,50 元素上限几乎从不撞。** envelope 30 KB 也几乎从不撞。**真正的"截断"是 `eventToEntry` 的 600 字节硬截,不是 element / envelope。**

### 0.4 候选方案

| 方案 | 描述 | 改动 |
|---|---|---|
| (a) **删 `truncateForLog(text, 600)`,允许多 div 进 receipt** | receipt 内 OutReply 走 `splitMarkdownForDivs` 拆多 div,无截断;超 `perEntryMaxRunes=8000` 才外溢 | eventToEntry + buildReceiptCard |
| (b) **删 600 截断 + 超限改独立 reply(本 feature)** | (a) + 单条 OutReply > 8000 runes 或 receipt 已 45 entries 时,改 `ReplyInThreadAndChat` 投递 | adapter.go + 新 helper `sendReplyAsMessage` + (a) 的全部 |
| (c) 维持 600 截断,只放宽到 2000 | 单 entry 字节 cap 抬高 | 1 行 |
| (d) 维持现状,继续修截断边界 | 只动 receipt_event.go | 中等 |

**选择 (b)** 因为:

1. **彻底消除 OutReply 路径上的所有截断**——治本,不是抬 cap 数字
2. **架构上与 F-39 OutResult 平行**——两条独立 reply surface(receipt 内的 chunk + 超限后的独立 reply),与"streaming card for progress + complete reply for deliverable"模式对齐
3. **打开 envelope 真撞墙的降级路径**——独立 helper 可以做 30 KB hard cap + fallback,无需保护 receipt 内其他 entry
4. **数量超限(45 entries)也得有出路**——旧路径只删旧 entries,长 reply 触发 FIFO 驱逐,用户可能看到"前 5 条消失,新一条进来",语义模糊。新路径直接"超 45 → 走独立 reply",语义清晰
5. **顺手改 `OutText` → `OutReply` 命名**——一次 PR 解决"命名不准 + 内容丢字"两个问题

### 0.5 不可变约束

- **`OutboundMessage` 契约字段不变**:`Kind: OutReply` 替换 `OutText`(语义更准,wire 不变);`Text string` 字段不动;`ReplyTo` 不动;§1.4 边界规范保留
- **`EventText` 不动**——bridge 层 `claudecode/stream.go` 仍产 `EventText`(无前缀),由 adapter 决定 fold / 独立 reply
- **Gateway 翻译路径不动**:`gateway/translate.go::Translate(EventText)` 仍产 `OutboundMessage{Kind: OutReply, Text}`(仅 Kind 改名)
- **ChatSession 不动**:per-turn / per-chat 状态机无变化
- **抽象归抽象**:Channel 自治范围内决定渲染目标(receipt 内 fold / 独立 reply 是 Channel 决策,不影响抽象层)
- **`OutboundMessage.ReplyTo` 不动**:`currentTurnUserMsgID` 仍作为锚点,独立 reply 也锚到同 userMsgID
- **`OutResult` 路径(F-39)不动** — F-39 决策依然成立,OutResult 不进 receipt 是独立决策

---

## 1. 设计

### 1.1 视觉对比

**改前**(以一条 1500 char OutReply chunk 为例):

```
user_msg om_A
  └ Receipt Card ⤓ (锚定 om_A; visible in main chat)
      ⏳ 处理中
      💬 前 600 字 + …
      <hr>
      footer
```

用户看到 600 截断的"半截回答"——`1500 char` 内容里后 900 字 + markdown 后半段全丢。

**改后**(同 1500 char 例子):

```
user_msg om_A
  ├ Receipt Card ⤓ (rolling log, 锚定 om_A)
  │   ⏳ → 🔄 处理中
  │   💬 完整 1500 char (拆 2 个 div: ≤ 1000 + ≤ 500)
  │   <hr> + footer
```

正常 fold 路径:`eventToEntry` 不再截断,`buildReceiptCard` 用 `splitMarkdownForDivs` 把单条 OutReply 拆多 div,完整内容进 receipt。

**改后**(极端 case:OutReply 12000 runes > perEntryMaxRunes 8000):

```
user_msg om_A
  ├ Receipt Card ⤓ (rolling log, 锚定 om_A)
  │   ⏳ → 🔄 处理中
  │   (无 OutReply entry)
  │   <hr> + footer
  └ OutReply Reply ⤓ (锚定 om_A; 独立 reply,完整 12000 char markdown)
      💬 完整 12000 char(走 3 段 dispatch:sanitize + multi-div)
```

超长 OutReply 直接走独立 reply,跟 F-39 OutResult 同 surface(receipt card + 独立 reply 锚同 userMsgID),但**不带 icon 前缀**——它是 reply 流的延续,不是新条目。

**改后**(另一极端 case:receipt 已有 45 entries,新 OutReply 到达):

```
user_msg om_A
  ├ Receipt Card ⤓ (rolling log, 锚定 om_A; 45 entries 已满)
  │   ⏳ → 🔄 处理中
  │   💬 × 45 entries
  │   <hr> + footer
  └ OutReply Reply ⤓ (锚定 om_A; 独立 reply)
      💬 完整新 OutReply text
```

数量超限语义:不再 FIFO 驱逐旧 entries(F-39 之前做法),新 OutReply 走独立 reply。receipt 是"事件流摘要",独立 reply 是"超量后续",语义清晰。

### 1.2 超限判定(`isOverflowingReceipt`)

```go
// isOverflowingReceipt 在 receipt.mu 持有时调,决定 OutReply 是 fold 还是外溢。
func isOverflowingReceipt(r *MessageReceipt, text string) bool {
    // 长度:单条 OutReply 超过 entry rune 上限(8000 runes)
    if utf8.RuneCountInString(text) > perEntryMaxRunes {
        return true
    }
    // 数量:receipt 已有 entries 数达到上限(45)
    if len(r.entries) >= replyMaxEntries {
        return true
    }
    return false
}
```

阈值复用现有常量:
- `perEntryMaxRunes = 8000`(F-39 后给 result 用的;F-40 也适用于 OutReply 上限)
- `replyMaxEntries = 45`(F-25 既有)

**不新增常量**——保持常量表精简,与 F-39 result 共享"8000 runes 是 receipt 内单 entry 实际容量"的认知。

### 1.3 Dispatch(超限后走 `sendReplyAsMessage`)

平行 `sendResultAsReply` (F-39),3 段 dispatch:

```
             ┌─ has no markdown indicators
             │   → MsgTypeText (plain text bubble, Feishu 渲染 <at> + 4-style)
             │
sanitize     ├─ markdown 且 tables > 5
  ↓          │   → MsgTypePost + tag:"md" (GFM 全套,无 Card 2.0 表格硬限)
content      │
             └─ markdown 且 tables ≤ 5
                 → MsgTypeInteractive (Card 2.0)
                   elements: [
                     {tag:"markdown", content: chunk 1},  ← F-37 splitMarkdownForDivs
                     {tag:"markdown", content: chunk 2},  ← 1000 runes/div hard cap
                     ...,
                   ]
                   sanitized via SanitizeCardMarkdown
```

复用 F-39 的 `SanitizeCardMarkdown` + `splitMarkdownForDivs` + `buildResultPayload` + `truncateRunes`。**不新增 helper**——只有 adapter 入口 + 路由分流是 F-40 新增。

### 1.4 Wire 形态:`ReplyInThreadAndChat`

| 形态 | Feishu wire | main chat | thread |
|---|---|---|---|
| **`ReplyInThreadAndChat`**(本 feature 默认)| `POST /messages/{rootID}/reply`,`reply_in_thread` 字段**省略** | **正文内联 + thread 入口** | 同一份正文 |
| `ReplyInThread`(OutThinking / OutToolStart / OutToolEnd)| `reply_in_thread:true` | "X replies" 灰条 | 正文 |

F-40 的 `sendReplyAsMessage` 走 `sendContent(chatID, msgType, body, userMsgID, replyInThread=false)`——`replyInThread=false` = `ReplyInThreadAndChat`(字段省略 = main chat 可见正文 + thread 入口)。与 F-39 OutResult 同形态(replyInThread=false,锚同 userMsgID)。

### 1.5 状态机变化

`MessageReceipt` 状态机不变(Waiting → Executing → Completed → Error)。**两点触发点变化:**

1. **`Adapter.Send(OutReply)` 入口**:不直接调 `receipt.Append`,先看 receipt 状态:
   - `receipt == nil` → fail-safe 走 `sendRawOutText` (top-level plain text bubble)
   - `receipt != nil && r.State() == StateCompleted` → 迟到 OutReply,直接走 `sendReplyAsMessage`(不静默丢)
   - `receipt != nil && isOverflowingReceipt(r, text)` → 超限,走 `sendReplyAsMessage`
   - 否则 fold → `receipt.Append(EventText{Text: text})`(不截断)

2. **`receipt.Append(EventText)` 内部**:不再 `truncateForLog(text, 600)`;`eventToEntry(EventText)` 输出 `LogEntry{Text: full text, Kind:"reply"}`;`buildReceiptCard` 用 `splitMarkdownForDivs(entry.Text, divTextCharLimit=1000)` 拆多 div 进 card body。

### 1.6 Receipt body 预算

不变:

| 限制 | 值 | 来源 |
|---|---|---|
| `replyMaxBytes` | 24 KiB | 飞书 30KB envelope 留 6KB 头 |
| `replyMaxElements` | 50 | 飞书 `body.elements` 硬限 |
| `replyMaxEntries` | 45 | entries 总数(留 5 给 header / hr / footer / checklist × 1-2) |
| `divTextCharLimit` | 1000 runes | 单 `div.text.content` 硬限 |

F-40 删了 600 字节 entry-level 截断,允许单 entry 占多个 div。一个 2500 runes OutReply 进 receipt 占 3 div(1000 + 1000 + 500),仍受 `replyMaxElements=50` 约束——这正是超限改独立 reply 的另一原因(单 entry 占太多 div 会挤掉后续 event / footer 预算)。

---

## 2. 文件 & 接口

### 2.1 改动的文件

**`internal/gateway/messages.go`** —— `OutText` 常量改名为 `OutReply`:

```go
const (
    // OutReply is a streaming reply chunk — the most common case
    // for both final agent replies (multi-chunk) and intermediate
    // status lines (single chunk). F-40 rename from OutText for
    // semantic accuracy: this is the agent's reply to the user's
    // current turn, not a generic "text" payload.
    OutReply OutboundKind = iota
    OutToolStart
    OutToolEnd
    OutThinking
    // ...
)
```

**`internal/gateway/translate.go`** —— `EventText` 翻译路径里 `Kind: OutText` → `Kind: OutReply`:

```go
case agent.EventText:
    // thinkingPrefix 已剥 / 未剥两种 case
    return OutboundMessage{
        Kind:   OutReply,  // ← rename
        Text:   text,
    }, true
```

**`internal/channel/feishu/adapter.go`** —— `Send` case 改名 + 新增 `sendReplyAsMessage` + `isOverflowingReceipt` 判断 + 迟到 OutReply 处理:

```go
// (1) case label rename
case gateway.OutReply:
    text := strings.TrimSpace(msg.Text)
    if text == "" {
        return nil
    }
    
    receipt := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo)
    if receipt == nil {
        return a.sendRawOutText(ctx, msg.ChatID, text)
    }
    
    // 迟到 OutReply:receipt 已 StateCompleted,不再 fold,走独立 reply
    if receipt.IsCompleted() {
        return a.sendReplyAsMessage(ctx, msg.ChatID, msg.ReplyTo, text)
    }
    
    // 超限判断:长度或数量任一触发
    if isOverflowingReceipt(receipt, text) {
        return a.sendReplyAsMessage(ctx, msg.ChatID, msg.ReplyTo, text)
    }
    
    // 正常 fold:不截断,buildReceiptCard 内部 multi-div
    return receipt.Append(ctx, agent.AgentEvent{
        Kind: agent.EventText,
        Text: text,
    })

// (2) 新 helper
func (a *Adapter) sendReplyAsMessage(
    ctx context.Context, chatID, userMsgID, text string,
) error {
    // 镜像 sendResultAsReply (F-39) 的 3 段 dispatch + sanitize + envelope defense
    // 唯一差别:replyInThread=false (默认) = ReplyInThreadAndChat
    // 唯一差别:不加 icon 前缀 (OutReply 是流延续,不是新条目)
}

// (3) 超限判定
func isOverflowingReceipt(r *MessageReceipt, text string) bool {
    if utf8.RuneCountInString(text) > perEntryMaxRunes {
        return true
    }
    r.mu.RLock()  // r.mu 是 sync.RWMutex;调用方已持有 r.mu(Append 持锁路径)
    defer r.mu.RUnlock()
    return len(r.entries) >= replyMaxEntries
}
```

注:`isOverflowingReceipt` 锁语义需要 review——`receipt.Append` 持有 `r.mu.Lock()`,调用 `isOverflowingReceipt` 时 r.mu 已持锁(Write lock),子函数不能用 `RLock`(会死锁)。改成 caller 在调用前已持锁,`isOverflowingReceipt` 不加锁:

```go
func isOverflowingReceipt(r *MessageReceipt, text string) bool {
    // Caller holds r.mu (write lock) — see MessageReceipt.Append.
    if utf8.RuneCountInString(text) > perEntryMaxRunes {
        return true
    }
    return len(r.entries) >= replyMaxEntries
}
```

但 `Adapter.Send` → `receiptFor` 路径不持 `r.mu`(只在 `Append` 内部拿锁)。所以这里需要 `r.entriesSnapshot()` 之类 read-only helper,或 `r.entryCount()`。新增最小 helper:

```go
// 在 MessageReceipt 上新增:
func (r *MessageReceipt) EntryCount() int {
    r.mu.RLock()
    defer r.mu.RUnlock()
    return len(r.entries)
}

func (r *MessageReceipt) IsCompleted() bool {
    r.mu.RLock()
    defer r.mu.RUnlock()
    return r.state == StateCompleted
}
```

**`internal/channel/feishu/receipt_event.go`** —— `eventToEntry(EventText)` 删 `truncateForLog(text, 600)`:

```go
// (1) case agent.EventText:删 truncateForLog
case agent.EventText:
    text := strings.TrimSpace(ae.Text)
    if text == "" {
        return LogEntry{}, false
    }
    if strings.HasPrefix(text, thinkingPrefix) {
        // F-34: thinking no longer folds into the receipt card
        return LogEntry{}, false
    }
    return LogEntry{
        Time: now,
        Icon: "💬",
        Text: text,  // ← 不再 truncate;buildReceiptCard 拆多 div
        Kind: "reply",
    }, true
```

**`internal/channel/feishu/receipt.go`** —— `buildReceiptCard` 改用 `splitMarkdownForDivs` 拆多 div:

```go
// 当前:
for _, e := range r.entries {
    elements = append(elements, map[string]any{
        "tag": "div",
        "text": map[string]any{
            "tag":   "lark_md",
            "content": e.Icon + " " + e.Text,  // 单 div,无 split
        },
    })
}

// 改后:
for _, e := range r.entries {
    body := e.Icon + " " + e.Text
    if e.Icon == "" { body = e.Text }  // usage/compaction/init 不带 icon
    chunks := splitMarkdownForDivs(body, divTextCharLimit)
    for _, c := range chunks {
        elements = append(elements, map[string]any{
            "tag": "div",
            "text": map[string]any{"tag": "lark_md", "content": c},
        })
    }
}
```

`splitMarkdownForDivs` 复用 F-37 helper,长 entry 自动拆多 div,code block / list 块保持 atomic。

**`cmd/nightme/run.go`** —— `responder.Send` Kind 字段值改名:

```go
return r.ch.Send(ctx, gateway.OutboundMessage{
    ChatID:  chatID,
    Kind:    gateway.OutReply,  // ← rename
    Text:    text,
    ReplyTo: userMsgID,
})
```

### 2.2 保留不变的(确认无副作用)

- `OutboundMessage{Text, ReplyTo, ChatID, ...}` —— wire 字段全不变,仅 `Kind` enum 改名
- `gateway/translate.go::Translate(EventText)` —— 路径不变,只改 Kind 字面量
- `chatSession` 状态机 —— 不动
- `MessageReceipt.SetCompleted / Append(EventDone / Error / Init / Usage)` —— 不动
- F-37 `splitMarkdownForDivs` —— 复用,新增 caller(receipt 内 OutReply entry)
- F-38 task checklist —— 不动
- F-thread-route(thinking/tool → thread reply)—— 不动
- F-39 OutResult `sendResultAsReply` —— 不动,F-40 新 `sendReplyAsMessage` 是 sibling helper(共享 3 段 dispatch + sanitize + envelope defense)

### 2.3 Send vs Reply 行为对比

| OutboundKind | F-39 (OutResult) | F-40 (OutReply) |
|---|---|---|
| 进入 receipt card | ❌(完全独立 reply)| ✅(默认 fold)|
| 超限改独立 reply | N/A(OutResult 永远独立)| ✅(长度 / 数量)|
| 3 段 dispatch(text / post+md / card) | ✅ | ✅(复用) |
| Sanitize pipeline | ✅(`SanitizeCardMarkdown`) | ✅(复用)|
| Envelope defense(28 KB) | ✅(`resultCardEnvelopeBudget`) | ✅(复用)|
| 锚 userMsgID | ✅ | ✅ |
| `replyInThread` flag | `false`(ReplyInThreadAndChat) | `false`(ReplyInThreadAndChat)|
| Icon 前缀 | `❌`(error) / 无(success)| 无(始终)|
| 拆多 div | ✅(F-37) | ✅(F-37)|
| 复用 helper | — | 复用 F-39 的 `sendResultAsReply` 内部 helper |

**关键共享**:`SanitizeCardMarkdown` / `splitMarkdownForDivs` / `buildResultPayload` / `truncateRunes` 全部复用,F-40 新增的只有:
- `sendReplyAsMessage`(adapter 顶层 helper,共享内部逻辑)
- `isOverflowingReceipt`(adapter 顶层判定)
- `MessageReceipt.EntryCount() / IsCompleted()`(receipt 公开 read-only helper)
- `buildReceiptCard` 改写(用 splitMarkdownForDivs)
- `eventToEntry(EventText)` 删 600B truncate

---

## 3. 测试覆盖

### 3.1 单元测试

| 文件 | 用例 | 断言 |
|---|---|---|
| `internal/channel/feishu/receipt_event_test.go` | `TestEventToEntry_Text_NoTruncate` | `EventText{Text: 1500 chars}` → entry `Text == 原始 1500 chars`(无 600B truncate) |
| 同上 | `TestEventToEntry_Text_EmptySkipped` | `EventText{Text: "  "}` → `(_, false)` |
| 同上 | `TestEventToEntry_Text_ThinkingPrefix_Skipped` | `EventText{Text: "[思考] hello"}` → `(_, false)`(F-34 不变)|
| `internal/channel/feishu/receipt_test.go` | `TestBuildReceiptCard_LongReply_SplitMultiDiv` | append 一条 2500 char EventText → card 含 3 个 div(≤ 1000 runes each),code block 保持 atomic |
| 同上 | `TestBuildReceiptCard_ShortReply_SingleDiv` | append 一条 500 char EventText → card 含 1 个 div |
| 同上 | `TestBuildReceiptCard_CodeBlockAtomic` | 1500 char 包含 `` ```code block``` `` → 整段不切 |
| `internal/channel/feishu/adapter_test.go` | `TestSend_OutReply_FoldsIntoReceipt_NoTruncate` | mock receipt;调 `Send(OutReply{Text: 1500 chars})` → receipt.Append called **1 次** + EventText.Text 完整 1500 chars(无 600B truncate) |
| 同上 | `TestSend_OutReply_OverflowLength_AsReply` | `OutReply{Text: 9000 runes (> perEntryMaxRunes=8000)}` → 走 `sendReplyAsMessage`,**不**调 `receipt.Append`,mock sendContent called **1 次** with ReplyInThreadAndChat |
| 同上 | `TestSend_OutReply_OverflowQuantity_AsReply` | 预填 receipt 45 entries,新 `OutReply{Text: 100 chars}` → 走 `sendReplyAsMessage`,**不**调 `receipt.Append` |
| 同上 | `TestSend_OutReply_AfterCompletion_AsReply` | receipt 已 StateCompleted,新 `OutReply{Text: 100 chars}` → 走 `sendReplyAsMessage`(不静默丢) |
| 同上 | `TestSend_OutReply_NoReceiptFallback` | receiptFor 返回 nil(receipt 冷启动失败)→ 走 `sendRawOutText`(top-level plain text bubble, fail-safe) |
| 同上 | `TestSend_OutReply_NoIconPrefix_OnOverflow` | `sendReplyAsMessage` 输出 body **不带** `💬` 前缀 |
| 同上 | `TestSend_OutReply_3SegmentDispatch_NoMarkdown_Text` | `OutReply{Text: "plain text"}` → `MsgTypeText` |
| 同上 | `TestSend_OutReply_3SegmentDispatch_LotsTables_Post` | 6 markdown tables → `MsgTypePost + tag:"md"` |
| 同上 | `TestSend_OutReply_3SegmentDispatch_Default_Card` | markdown ≤ 5 tables → `MsgTypeInteractive` Card 2.0 |
| 同上 | `TestSend_OutReply_LongText_TruncatesToEnvelope` | text 15000 runes → 进入 28 KB budget 路径,log warn + truncate |
| 同上 | `TestSend_OutReply_Orphan_NoUserMsgID_TopLevel` | `userMsgID == ""` → fail-safe `sendRawOutText` |
| 同上 | `TestSend_OutReply_SanitizeApplied` | text 含 `[x](relative)` → sanitize 后 plain text(`230001 invalid href` 防御) |
| `internal/gateway/translate_test.go`(若存在) | `TestTranslate_EventText_OutReply` | `Translate(EventText{Text: "hello"})` → `OutboundMessage{Kind: OutReply, Text: "hello"}` |
| `cmd/nightme/run_test.go`(若存在) | `TestResponderSend_OutReply` | 验证 `Kind: gateway.OutReply`(compile-time + runtime) |

### 3.2 集成测试(回归)

| 文件 | 用例 | 断言 |
|---|---|---|
| `internal/channel/feishu/adapter_test.go` | `TestReceipt_FullTurn_OutReplyFlow` | 完整 turn: user msg → 5 个 OutReply chunk(各 200-300 char)→ 1 个 OutResult 5000 char。receipt PATCH 多次,OutReply entries 完整 200-300 char 无截断;OutResult 走独立 reply 路径 |
| 同上 | `TestReceipt_StreamingToReply_Handoff` | 前 10 chunks 走 receipt fold, 第 11 个 chunk 触发超限(`perEntryMaxRunes`),改独立 reply;receipt 内只有 10 entries,后续 chunk 不进 receipt |
| 同上 | `TestReceipt_LongSingleReply_SplitMultiDiv` | 单条 5000 char OutReply → receipt 内 entry 占 5 个 div,无截断 |

### 3.3 grep / 回归(收尾)

```bash
# 验证 OutText 完全消失(除 git 历史 + 文档 changelog)
rg -n "OutText" --type=go  # 期望: 0 命中

# 验证 OutReply 全部覆盖
rg -n "OutReply" --type=go  # 期望: 含 gateway/messages.go + translate.go + adapter.go + run.go + 测试

# 验证 OutResult 路径仍工作(F-39 不被 F-40 影响)
go test ./internal/channel/feishu/... -run TestSend_OutResult
```

---

## 4. 落地顺序

每步独立 commit,可单独 review + revert:

| Step | 内容 | 文件 | 风险 |
|---|---|---|---|
| 1 | **本文档**(`../channel/feishu-rendering.md`) | `docs/feat/` | 零 |
| 2 | SPEC.md + §12 backlog 更新 | `docs/SPEC.md` | 零 |
| 3 | channel/feishu.md 更新 | `docs/channel/feishu-rendering.md` | 零 |
| 4 | `OutText` → `OutReply` rename | `messages.go` + `translate.go` + `adapter.go` + `run.go` | 低(纯 enum rename,编译期 fail-fast)|
| 5 | `MessageReceipt.EntryCount() / IsCompleted()` helper | `receipt.go` | 低 |
| 6 | `eventToEntry(EventText)` 删 600B truncate | `receipt_event.go` | 低 |
| 7 | `buildReceiptCard` 用 `splitMarkdownForDivs` 拆多 div | `receipt.go` | 中 |
| 8 | `isOverflowingReceipt` + `Adapter.Send(OutReply)` 路由分流 | `adapter.go` | 中 |
| 9 | `sendReplyAsMessage` 新 helper(复用 F-39 sanitize / buildResultPayload / splitMarkdownForDivs) | `adapter.go` | 中 |
| 10 | `receipt_event_test.go` 改:删 600B truncate assertion | 改 | 低 |
| 11 | `receipt_test.go` 新增 `TestBuildReceiptCard_LongReply_SplitMultiDiv` | 改 | 零 |
| 12 | `adapter_test.go` 新增 5+ OutReply case(fold / overflow-length / overflow-quantity / late / no-receipt / 3-segment) | 改 | 零 |
| 13 | 全量 `go test ./...` + `go vet` + `golangci-lint` | — | 必过 |

---

## 5. 与上下游契约

### 5.1 OutboundMessage 契约

`Kind: OutText` → `Kind: OutReply`(enum 重命名)。**wire format 之外,字段不变**:`Text string` / `ReplyTo string` / `ChatID string` 全保留。Channel 自决渲染目标(receipt fold / 独立 reply)完全在 §1.4 边界规范允许范围。

### 5.2 ChatSession 状态机

不变。`EventText` 仍由 `cs.EventCallback` 触发 → `gateway.Translate` → `channel.Send`。Channel 内的渲染分支改了(从"强制 fold + 600B 截断"改为"fold 不截断 / 超限改独立 reply"),但状态机意义不变。

### 5.3 Tool thread 路由(F-37 tool-routing)

不动。F-thread-route 描述的是 thinking/tool/compaction,已经独立于 receipt。F-40 加 OutReply 超限也独立后,"独立 surface"模式更一致(thinking/tool → thread-only,result / reply-overflow → main-chat-visible reply)。

### 5.4 Result 路径(F-39)

不动。F-39 决策依然成立(OutResult 不进 receipt,走独立 reply)。F-40 加 OutReply 超限也独立后,两条独立 reply surface 平行:

- **OutResult**(F-39):always independent reply;`replyInThread=false`(ReplyInThreadAndChat)
- **OutReply 超限**(F-40):conditional independent reply;`replyInThread=false`(ReplyInThreadAndChat)
- **OutThinking / OutToolStart / OutToolEnd**(F-37):always thread reply;`replyInThread=true`

三组 surface 互不重叠,但 wire 层都复用 `sendContent` 底层。

### 5.5 多 div 拆分(F-37 multi-div)

复用扩展:`splitMarkdownForDivs` 现在服务 3 个 caller:
- `buildResultCardJSON`(F-39 OutResult surface)
- `buildThinkingCard`(F-think OutThinking)
- `buildReceiptCard`(F-40 receipt 内长 OutReply entry)

---

## 6. 后续工作(本文档不做)

- **OutReply multi-div 阈值上限**(backlog):当前超 `perEntryMaxRunes=8000` 改独立 reply,但 receipt 内单 entry 拆 N 个 div 不应挤掉 50 element / 24KB 预算。后续可加 receipt 内 "single entry max div count"(如 ≤ 5)防御
- **OutReply telemetry**(backlog):打点 `outreply.{fold_count, overflow_length_count, overflow_quantity_count, late_count, no_receipt_count}` 到 metric,看实际分布;若 overflow 比例高,考虑降 `perEntryMaxRunes` 到 4000
- **OutReply 合并**(P2):OutReply 流式 chunks 当前每 chunk 一个 `LogEntry`,可以借鉴 F-38 OutToolStart+End 合并模式,在 receipt 内做 chunk-merge(50ms 内的连续 chunks 合并成一个 entry),减少 receipt PATCH storm
- **OutReply markdown 渲染选项**(P2):当前 OutReply 走 lark_md(`buildReceiptCard` 用 `lark_md`),OutText 的 icon 是 `💬`。后续可加 per-chat toggle `/reply plain|markdown` 让用户选纯文本 / markdown

---

## 7. 不变式总结(本文档特殊要求)

**F-40 改 `OutText` 命名 + 删 600B 截断 + 加超限改独立 reply,但保留:**

- OutboundMessage 字段不变(`Kind` 改名 `OutText` → `OutReply`,其他全保留)
- Gateway 不变(`Translate` 仍产 OutboundMessage)
- ChatSession 不变(`currentTurnUserMsgID` 单数锚点保留)
- `OutboundMessage.ReplyTo = currentTurnUserMsgID` 不变(独立 reply 也锚同 userMsgID)
- 1 turn : 1 anchor 不变式保留
- §1.4 边界规范保留(OutReply 字段是 typed primitive string,Channel 自决 target)
- 抽象归抽象 / 具体归具体原则保留(超限改独立 reply 是 Feishu 自治决策)
- F-25 rolling-log UX 不变(receipt card 仍是"事件日志 + 元数据")
- F-39 OutResult 决策不变(OutResult 不进 receipt)
- F-37 / F-38 / F-think / F-38-tool-merge 全部决策不变

---

## A8. F-42: Lazy Receipt Creation + Simplified MessageState + TaskList Markdown Title

> **Source**: `../channel/feishu-rendering.md`


> **Scope**: `internal/channel/feishu/{adapter.go,receipt.go,receipt_task.go}` + 文档同步
> **Depends on**: F-25 (rolling-log receipt), F-31 (MessageState), F-37 (thread routing), F-38 (task checklist), F-40 (OutReply overflow)
> **Related**: [`SPEC.md`](../SPEC.md) / §2.4 / §2.5; [`channel/feishu-rendering.md`](../channel/feishu-rendering.md) / §13.20 / §15.0

## 0. 背景

Feishu receipt 在 + F-25 落地后已经形成完整 rolling-log 形态，但渲染层仍有 3 处冗余 / 不清晰：

### 0.1 Cold-start 空 Receipt card 是 design smell

**现状**：`Adapter.receiptFor()` 在 cache miss 时会主动 `buildColdStartCard()` + `SendCard()` 出一个 minimal ⏳ 占位卡，记下 `cardMsgID` 给后续 PATCH。

**问题**：
- 短 turn（首 OutReply < 几百 ms）— 占位卡几乎立刻被第一条内容 PATCH 覆盖，**用户感知不到它存在**
- 长 turn — 占位卡只是 "空白 + ⏳" 视觉，⏳ reaction 已经在 user msg 上存在，card 本身没新增信息
- OutInit 单飞 — 占位卡只有 footer 元数据，没 body，**视觉空旷**

**结论**：占位卡是"为了有 receipt 而有 receipt"，删。

### 0.2 ⏳ / 🔄 reactions 跟其他 waiting 信号重复

**Feishu 端已经存在的 waiting 信号**：
1. ⏳ reaction (StateReceived) ── AddReaction(userMsgID, "OneSecond")
2. 🔄 reaction (StateForwarded) ── AddReaction(userMsgID, "OnIt")
3. Tool / Think thread replies (F-37 thread route) ── execution 持续在 thread 报
4. 未来可能的 OutReply / OutTaskCreate ── 一旦内容到达，card 出现

**分析**：
- ⏳ / 🔄 在 Tool/Think thread 已经在持续报的语境下是**冗余信号**
- ✅ / ❌ 是**终态不可替代**的确认，保留
- 删 ⏳ + 🔄，留 ✅ + ❌

### 0.3 TaskList 没标题，视觉不清晰

**现状**：`buildTaskChecklistChunks` 产出 `- [ ] Subject` `- [x] Subject` 直接挂在一堆 reply entries 后面，没 section header。

**问题**：当 TaskList 是 receipt 唯一内容（agent 只调 TaskCreate 不出 OutReply），用户看到一堆 checkbox 但没"这是什么"的语境。

**修复**：TaskList 永远 prepend markdown 标题 `**📋 Tasks**` 跟其他内容视觉分开。

---

## 1. 设计

### 1.1 Receipt lazy creation

**核心变化**：`Adapter.receiptFor()` 退化为纯 cache lookup，不再 cold-start。

```go
// 之前:
func (a *Adapter) receiptFor(ctx context.Context, chatID, userMsgID string) *MessageReceipt {
    if userMsgID == "" { return nil }
    a.mu.Lock()
    if r, ok := a.receiptsByUserMsgID[userMsgID]; ok && r != nil {
        a.mu.Unlock()
        return r
    }
    a.mu.Unlock()
    // ← 这里 cold-start: buildColdStartCard + SendCard + NewMessageReceiptForCard
}

// 之后 (F-42):
func (a *Adapter) receiptFor(ctx context.Context, chatID, userMsgID string) *MessageReceipt {
    if userMsgID == "" { return nil }
    a.mu.Lock()
    defer a.mu.Unlock()
    return a.receiptsByUserMsgID[userMsgID]  // nil on miss
}
```

**首次创建时机**：Send() dispatcher 中**第一个需要 receipt 的 OutboundKind** 到达时：

| OutboundKind | 创建路径 | 首次 card body |
|---|---|---|
| `OutReply` | `ensureReceiptForReply(ctx, chatID, userMsgID, text)` | 该条 reply 内容（eventToEntry(EventText)）|
| `OutTaskCreate` / `OutTaskUpdate` | `ensureReceiptForTask(ctx, chatID, userMsgID)` | `**📋 Tasks**` + 完整 task list |
| `OutInit` / `OutUsage` (miss) | 不创建 | silent drop（同今天 cold-start 失败的 fallback） |

`ensureReceiptForReply` / `ensureReceiptForTask` 内部：
1. 调 `buildReceiptCard` / `buildReceiptCardWithTaskList` 产出首版 card body
2. 调 `SendCard(ctx, chatID, body, userMsgID, replyInThread=false)` POST 出去
3. 拿到 `cardMsgID` 后 `NewMessageReceiptForReply(chatID, userMsgID, cardMsgID, bot)` 创建 receipt
4. 存进 `receiptsByUserMsgID[userMsgID]`
5. 返回 receipt

后续同 userMsgID 的事件走 PATCH 路径（不变）。

### 1.2 MessageState reactions 简化

**Feishu adapter self-determines** 哪些 state 渲染。`agent.MessageState` enum 不动（其他 Channel / 未来用）。

```go
// internal/channel/feishu/adapter.go - Send dispatcher OutMessageState case
case gateway.OutMessageState:
    if msg.MessageState == nil {
        return errors.New("feishu: OutMessageState missing MessageState payload")
    }
    if msg.MessageState.MessageID == "" {
        return errors.New("feishu: OutMessageState missing MessageID")
    }
    messageID := msg.MessageState.MessageID
    state := msg.MessageState.State

    // F-42: 中间态 (StateReceived / StateForwarded) silent drop
    // - OutThinking / OutToolStart / OutToolEnd 已经在 thread 持续报
    // - 首 OutReply 到达时 receipt 直接出,无需前置 reaction
    // - 终态 (StateDone / StateError) 仍渲染作为不可替代的确认
    switch state {
    case agent.StateReceived, agent.StateForwarded:
        return nil
    }

    emoji := mapStateToFeishuEmoji(state)
    if emoji == "" {
        return nil
    }
    // ... 现有 idempotency + AddReaction 逻辑
```

**保留**：
- `mapStateToFeishuEmoji` 函数（仍产 "DONE" / "THUMBSUP"）
- `messageStates map[string]agent.MessageState` idempotency 表
- `AddReaction` 实现

**删除**：StateReceived / StateForwarded 在 Send() case 中的处理分支（return nil 兜底）。

### 1.3 TaskList markdown 标题

`internal/channel/feishu/receipt_task.go::buildTaskChecklistChunks` 第一行改成 `**📋 Tasks**\n\n` + 原 list。

```go
func buildTaskChecklistChunks(tasks []agent.TaskItem) []string {
    var body strings.Builder
    // F-42: TaskList 永远带 markdown 标题,跟其他 reply body 视觉分开
    body.WriteString("**📋 Tasks**\n\n")
    // ... 现有 in_progress / pending / completed 排序 + checkbox 渲染
    return splitMarkdownForDivs(body.String(), divTextCharLimit)
}
```

**跟 OutReply body 共存时也加**（之前讨论过 "only add when no body"，但**永远加**更一致 ── 用户看到标题立刻知道有 task section，不用记 "没 body 才有标题" 这种条件）。

**容量计算**：F-38 §5.2 的 `divTextCharLimit` 预算不变（标题占 ~12 runes，影响可忽略）。

---

## 2. Files & 接口

### 2.1 改动文件

**`internal/channel/feishu/adapter.go`**

```go
// 退化为 cache lookup
func (a *Adapter) receiptFor(ctx context.Context, chatID, userMsgID string) *MessageReceipt

// NEW: 首次 OutReply 触发 receipt 创建 + 立即 post card with content
func (a *Adapter) ensureReceiptForReply(
    ctx context.Context, chatID, userMsgID, text string,
) (*MessageReceipt, error)

// NEW: 首次 OutTaskCreate/Update 触发 receipt 创建 + 立即 post card with task list
func (a *Adapter) ensureReceiptForTask(
    ctx context.Context, chatID, userMsgID string, tasks []agent.TaskItem,
) (*MessageReceipt, error)

// Send() OutReply case: receipt nil 时调 ensureReceiptForReply
// Send() OutTaskCreate/OutTaskUpdate case: receipt nil 时调 ensureReceiptForTask
// Send() OutMessageState case: StateReceived/Forwarded → return nil
// Send() OutInit/OutUsage case: receipt nil → silent drop (今天行为不变)
```

**`internal/channel/feishu/receipt.go`**
- 删 `NewMessageReceiptForCard`（cold-start 路径独有，ensure helpers 改走 `NewMessageReceiptForReply`）

**`internal/channel/feishu/receipt_task.go`**
- `buildTaskChecklistChunks` 第一行加 `**📋 Tasks**\n\n`
- F-38 §5.2 capacity 预算不变（标题占少量预算）

### 2.2 保留不变

- `agent.MessageState` enum / 抽象契约
- `OutboundMessage` schema（无新 Kind / 无删 Kind / 无改字段）
- `OutboundMessage.ReplyTo = currentTurnUserMsgID` 契约
- Gateway / ChatSession / Registry / agent package ── 全部不动
- F-25 rolling-log card UX（card 仍是事件日志载体）
- F-31 MessageState 抽象契约（state → reaction 是 channel 自治，Feishu 现在选 drop 中间态）
- F-37 thread routing（Tool/Think → thread 不变）
- F-38 task checklist（结构不变，加标题）
- F-39 OutResult 独立 reply（不变）
- F-40 OutReply overflow / rename（不变）
- F-41 active reconnect（不变）
- §1.3 不变式 / §1.4 边界规范

---

## 3. 测试

### 3.1 单元测试

| 用例 | 断言 |
|---|---|
| `TestSend_OutReply_FirstContentCreatesReceipt` (NEW) | 首次 OutReply → receipt 创建 + `SendCard` 调 1 次 + 拿到 `cardMsgID` 存进 `receiptsByUserMsgID` |
| `TestSend_OutReply_SubsequentPATCHesCard` (NEW) | 第二次同 userMsgID OutReply → 走 `PatchMessage` 不再 `SendCard` |
| `TestSend_OutTaskCreate_FirstEventCreatesReceipt` (NEW) | 首次 OutTaskCreate → ensure path + card body 含 `**📋 Tasks**` + task list |
| `TestSend_OutInit_BeforeReceipt_SilentlyDrop` (NEW) | receipt nil 时 OutInit → return nil，`SendCard` 不调 |
| `TestSend_OutUsage_BeforeReceipt_SilentlyDrop` (NEW) | 同上 |
| `TestSend_OutMessageState_StateReceivedIsSilentDrop` (RENAME from `..._FirstReceivedNotSkipped`) | `StateReceived` → return nil，`AddReaction` 不调 |
| `TestSend_OutMessageState_StateForwardedIsSilentDrop` (NEW) | `StateForwarded` → 同上 |
| `TestSend_OutMessageState_StateDoneAddsReaction` (NEW) | `StateDone` → `AddReaction(messageID, "DONE")` 仍调 |
| `TestSend_OutMessageState_StateErrorAddsReaction` (NEW) | `StateError` → `AddReaction(messageID, "THUMBSUP")` 仍调 |
| `TestBuildTaskChecklist_AlwaysHasMarkdownTitle` (NEW) | task list 渲染首行 = `**📋 Tasks**` |
| `TestBuildTaskChecklist_TitleWithReplyBody` (NEW) | 跟 OutReply body 共存时仍加标题 |

### 3.2 删 / 改

- 删 `TestBuildColdStartCard_Card2Shape` ── `buildColdStartCard` 函数删除
- 删 `TestReceipt_PerEventFreshMessage`（假设 first-send-fresh-message 行为）
- 改 `TestSend_OutReply_*` 系列：假设 receipt 不存在时不再 fresh-message，改为 "first creates + subsequent PATCHes"
- 改 `TestReceiptState_*` 系列：`MessageReceipt` 构造走 `NewMessageReceiptForReply` 而不是 `NewMessageReceiptForCard`

### 3.3 E2E smoke

- **DM round-trip**：
  1. 发消息 → ⏳ reaction **不再加**（之前会加）
  2. 立刻出第一条 OutReply → receipt card 直接出现（无占位卡）
  3. ✅ reaction 加到 user msg
- **TaskCreate 单飞**：
  1. agent 调 TaskCreate 不出 OutReply → card 直接出，body 只有 `**📋 Tasks**` + list
  2. ✅ reaction 仍加
- **Tool + Reply turn**：
  1. thread 报 `● Bash(...)` / `⎿ ...`（不变）
  2. receipt card 出现 with reply content（首次 OutReply 触发）
  3. ✅ reaction 加（不变）

---

## 4. 不变式

- **OutboundMessage 契约不变** — Kind / ReplyTo / payload 不动
- **agent.MessageState enum 不动** — Feishu adapter 自决 drop 中间态（Slack / Web 仍可渲染）
- **Gateway / ChatSession / Registry / agent package 不动** — 纯 Channel adapter 自治
- **F-25 rolling-log card UX 不变** — card 仍是事件日志 + metadata 载体
- **F-31 MessageState 抽象契约不变** — state → reaction 是 channel 自治
- **F-37 thread routing 不变** — Tool/Think 仍走 thread
- **F-38 task checklist 决策不变** — TaskList 仍是 receipt 一部分
- **§1.3 不变式不动** — Channel 自管渲染
- **§1.4 边界规范不动** — OutboundMessage 仍是 typed，Channel 自决

---

## 5. 落地顺序 (commit 切分)

```
commit A: docs(feishu): F-42 lazy receipt + simplified MessageState
         docs/SPEC.md (§1.4 changelog)
         docs/channel/feishu.md (§6.6.7 + §13.20 + §15.0 status)
         docs/FEATURES.md (§1b 加 F-42 行)
         docs/channel/feishu-rendering.md (NEW)

commit B: feat(feishu): F-42 lazy receipt creation + simplified reactions
         internal/channel/feishu/adapter.go (receiptFor 退化 + ensure helpers + OutMessageState 中间态 drop)
         internal/channel/feishu/receipt.go (删 NewMessageReceiptForCard, ensure 走 NewMessageReceiptForReply)
         internal/channel/feishu/receipt_task.go (TaskList markdown 标题)
         internal/channel/feishu/{adapter_test,receipt_test,message_state_test}.go (测试更新)

2 commits, ~80 行代码改动 + ~120 行测试调整 + 4 个 doc 文件同步
```

---

## 6. 与 F-25 / F-31 / F-38 的关系

| 已有 feature | F-42 关系 |
|---|---|
| **F-25 (rolling-log receipt UX)** | F-25 把 receipt 全部交给 Channel 自治；F-42 进一步简化 ── 不再有 "无内容的 receipt" |
| **F-31 (MessageState)** | F-31 引入 4-state lifecycle；F-42 在 Feishu adapter 自决 drop 中间态，保留终态 |
| **F-38 (task checklist)** | F-38 引入 TaskList 渲染；F-42 加 markdown 标题让 section header 清晰 |

---

## 7. 不叫 的理由 全部不变式保留 — F-42 纯 Channel 自治范围内的渲染细节调整：
1. 何时建 card（lazy 而非 eager）
2. 哪些 MessageState 渲染（只渲染终态）
3. TaskList 视觉样式（加 markdown 标题）

不动 nightme 数据模型与 Gateway 契约。跟 F-37 / F-38 / F-39 / F-40 / F-41 同一范畴。

---

## 8. 已知边界 / Out of Scope

- **OutInit / OutUsage 单飞** — silent drop，用户 0 反馈。Terminal reaction (✅/❌) 仍发，所以 turn 结束有信号。可接受。
- **极快 turn（< 100ms 首 OutReply）** — 用户看不到任何中间信号。这是 acceptable ── turn 太快本来也不需要反馈。
- **极慢 turn（无 Tool/Think，纯 OutReply）** — 罕见场景下 5s+ 思考期 0 反馈。F-37 thread route 保证大多数 turn 有 Tool/Think activity。极少数纯 OutReply turn 接受此 trade-off。
- **TaskList-only turn** — TaskList 永远带标题，用户清楚看到 `📋 Tasks` section header。
- **多 ChatSession 并发** — 每个 userMsgID 一个 receipt，`receiptsByUserMsgID` map 按 userMsgID 索引，不变。

---

---

## A9. F-44: OutReply 拆出 Receipt + Task Receipt 瘦身 + OutInit/OutUsage 推迟

> **Source**: `../channel/feishu-rendering.md`


> **Scope**: `internal/channel/feishu/{adapter.go, receipt.go, receipt_event.go, card_sanitize.go}` + 文档同步
> **Depends on**: F-25 (rolling-log receipt), F-37 (multi-div split + thread routing), F-38 (task checklist), F-39 (OutResult 独立 reply), F-40 (OutReply 改名 + 600B truncate 删除 + overflow bail-out), F-42 (lazy receipt creation)
> **Related**: [`SPEC.md`](../SPEC.md) §2.4; [`channel/feishu-rendering.md`](../channel/feishu-rendering.md)

## 0. 背景

### 0.1 现状（被改）

F-25 → F-40 → F-42 三轮演进后，Feishu receipt card 承担了 4 类内容：

```
┌─ 当前 receipt card (rolling-log + 元数据) ─┐
│ Header (prompt state icon ⏳/🔄/✅/❌)    │ ← F-25
│ Entries (OutReply text chunks，多 div)     │ ← F-25 → F-40
│ **📋 Tasks** checklist (F-42 title)       │ ← F-38 → F-42
│ <hr> + footer (init / usage)              │ ← F-25
└────────────────────────────────────────────┘
```

承载逻辑由 `MessageReceipt` 状态机 + `buildReceiptCard` + `renderLocked` 共同维护。**每加一类内容就要扩 receipt 一段代码**（F-38 加 TaskList、F-42 加 title、F-40 加 multi-div），经过 5 轮迭代，receipt 渲染路径上的协调逻辑已经接近 ~1350 行（`buildReceiptCard` + `renderLocked` + `receipt_event.go` 整个文件 + `ensureReceipt*` + `isOverflowingReceipt` + `MessageReceipt.Append` + state machine 方法）。

### 0.2 三类问题

#### (1) OutReply fold 进 receipt 路径的价值被稀释

`OutReply` 是 agent **对** user 当前 turn 的 reply 主体（流式 chunks），但 fold 进 receipt 后：

- 用户看到的是 "1 张 card 反复 PATCH" — 需要等 PATCH 周期才能看到完整内容
- 多 div 拆 + 50 element / 30 KB envelope 防御需要复杂预算逻辑（F-37 + F-40 共同投入）
- `eventToEntry(EventText)` + `truncateForLog(text, 600)` 历史包袱虽已删（F-40），但 `entryToDiv` + `splitMarkdownForDivs` 在 receipt 路径里仍占 ~150 行
- 数量超限 / 长度超限 / 迟到 reply 三种 bail-out（F-40）逻辑让 `Send(OutReply)` case 变 ~80 行

**反观 OutResult**（F-39 已是独立 reply）：`sendResultAsReply` ~80 行，没有任何 fold / overflow / bail-out 协调。用户视觉上更清晰 — "完整答案作为独立气泡，rolling 进程折叠到 card 里" 不再是 UX 必需。

#### (2) Receipt "元数据容器" 职责不再需要

`OutInit`（agent 身份 / model / workspace / branch）和 `OutUsage`（tokens / cost）当前作为 receipt card 的 footer / header。F-42 之后：

- receipt lazy create 后，若 turn 没有任何 `OutReply` / `OutTask*`，receipt 不存在
- 此时 `OutInit` / `OutUsage` 走 F-42 silent drop — **token 成本信息丢失**
- 若 turn 有 `OutReply` / `OutTask*`，footer 跟 reply entries / tasks 挤在 50 element 预算里

Footer 设计（F-44 设计阶段讨论过）应该独立成"每次 ReplyInThreadAndChat 都带 footer"的语义（snapshot Init + live Branch），但这需要扩展 `OutboundMessage` wire format + ChatSession 状态 + EventHandler 协调 — **跨层改动太大**，应该单独一个 PR。

#### (3) Task Receipt 才是真正必要的 folding surface

`OutTaskCreate` / `OutTaskUpdate` 是结构化任务清单，多个事件 fold 成 1 张 card 才**有视觉价值**（持续 PATCH 一个 markdown checklist section）。这个表面跟 OutReply fold 完全不同的渲染需求：

- 单一 section（`**📋 Tasks**` + 多行 checkbox）
- 静态结构（每次 event 替换整段 snapshot，不追加 entries）
- 不需要 PATCH storm（snapshot 替换式更新）

Task Receipt 完全可以脱离 OutReply fold 路径独立存在。

### 0.3 简化目标

**一个折叠 + 全部 top-level 应急/独立 surface**：

| OutboundKind | 表面 | 锚定 | F-44 后 |
|---|---|---|---|
| `OutReply` | **Rolling-log receipt**（N 事件 = 1 张 card，每 chunk = 1+ div；超限转 top-level） | `ReplyInChat` (cold-start card 是 top-level Create，rootID=""，no anchor)；overflow → `ReplyInChat` | ✅ F-44 revert: 重新 fold，card 走 top-level 永远主 chat 可见 |
| `OutResult` | 独立 top-level Create（每条 = 1 条消息） | `ReplyInChat` (top-level Create, no anchor) | ✅ 不变（F-39） |
| `OutTaskCreate` / `OutTaskUpdate` | **Rolling-log receipt**（N 事件 = 1 张 card） | `ReplyInChat` (cold-start card 是 top-level Create; 后续 PATCH 保持) | ✅ 简化为只装 Tasks |
| `OutChoice` (permission) | Top-level Create + 👉 emoji 前缀（`Action Needed`；多题向导带 `· i/n`） | `ReplyInChat` (no anchor) | ✅ 切到 top-level (用户可一眼识别需要点选的卡) |
| `OutCommandReply` (slash command) | Top-level Create + ❯ emoji 前缀 | `ReplyInChat` (no anchor) | ✅ 切到 top-level (短状态消息, 不需要 thread anchor) |
| `OutCompaction` | ReplyInBoth (brief 进度 marker) | `ReplyInBoth` (low frequency) | ✅ 不变 |
| `OutInit` / `OutUsage` | Silent drop | — | ⏸ 推迟到 footer PR |
| `OutThinking` / `OutToolStart` / `OutToolEnd` | ReplyInThread | thread 抽屉 | ✅ 不变 |

**F-44 的最终视觉**（更新版）：
- 主 chat 永远可见，所有 user-visible 消息都是 top-level Create（独立 bubble / card）
- `OutReply` 多 chunk 折进 1 张 rolling-log card（PATCH 维护，Card 2.0 视觉，top-level Create）
- `OutReply` overflow（50 elements / 30 KB envelope）→ 独立 top-level bubble（plain text / markdown，区分于 card 视觉）
- `OutResult` / `OutTask*` / `OutChoice` / `OutCommandReply` 都是 top-level Create，每条独立 surface
- emoji 前缀让用户在主 chat 里能扫一眼就知道是哪种消息：
  - 💬 = OutReply (rolling-log card 内的 entry)
  - 📋 = Tasks (rolling-log card 内的 checklist)
  - ✶ = Compacting
  - 💭 = Thinking (ReplyInThread, thread 抽屉内)
  - 👉 = Action Needed (OutChoice; AskUserQuestion / 权限点选)
  - ❯ = Slash command response (OutCommandReply)
  - ❌ = Error result (OutResult.IsError)

### 0.4 为什么不是 核心不变式全部保留：

- `OutboundMessage` 字段不变（仅 Channel 内部渲染目标分流）
- Gateway 不持有 receipt / thread / ChatSession 状态概念
- ChatSession 不 import channel/feishu
- Channel 不 import chatsession
- `out.ReplyTo = cs.currentTurnUserMsgID` 不变
- §1.4 抽象 / 具体 边界规范保留
- F-31 / F-37 / F-38 / F-39 / F-40 / F-42 各自决策**保持成立**，F-44 是它们的**简化合并**

F-44 是 Channel 自治范围内的渲染目标重排：删除过时的 fold 路径 + 简化 task receipt + 推迟 footer 渲染。

---

## 1. 设计

### 1.1 视觉对比

**改前**（典型 turn：5 个 OutReply chunk + 2 个 OutTaskCreate + 1 个 OutResult）：

```
user_msg om_A
  ├ Receipt Card (rolling log + 元数据)
  │   ⏳ → 🔄 → ✅
  │   💬 chunk 1
  │   💬 chunk 2
  │   💬 chunk 3
  │   💬 chunk 4
  │   💬 chunk 5
  │   **📋 Tasks** checklist (2 items)
  │   <hr>
  │   footer (init / usage)
  └ Final Result Reply (独立 reply, 锚 om_A)
      📝 完整 OutResult text
```

**改后**（同样 turn）：

```
main chat (top-level Create, no anchor — F-44 + F-44 revert #2):
  ├ Reply 1  💬 chunk 1   ⬅ rolling-log card (Card 2.0, top-level)
  │   💬 chunk 2
  │   💬 chunk 3
  │   💬 chunk 4
  │   💬 chunk 5
  │   **📋 Tasks** checklist (2 items)
  ├ Reply 2  ❯ command response    ⬅ top-level Create, no anchor
  ├ Reply 3  👉 Action Needed  ⬅ top-level Create, no anchor
  ├ Reply 4  📝 complete OutResult text
  └ Thread (side panel only):
      💭 thinking
      🔧 Bash(ls)
      ⎿  file1
         file2
```

视觉变化：
- ✅ 每个 reply chunk 立刻可见（不再等 PATCH 周期，不被 thread 抽屉吸走）
- ✅ Rolling-log receipt card 永远在主 chat 可见（top-level Create，无 parent-thread 风险）
- ✅ Task Receipt 单卡（不混 reply 流，PATCH 维护）
- ✅ Tool stream（💭/🔧/⎿）跟 reply 流完全分离 — tool 在 thread 抽屉，reply 在主 chat 流
- ✅ 各种消息有不同 emoji 前缀，扫一眼就识别类型（💬 reply, ❯ command, 👉 Action Needed, 📝 result）
- ⚠️ 失去 "Reply to <sender>" 头部（top-level Create 没有 reply anchor） — 跟 行为一致

### 1.2 Routing 分流表（最终）

```go
// internal/channel/feishu/adapter.go::Send (case 分支)
switch msg.Kind {
case gateway.OutReply:
    // F-44: 每 chunk → 独立 ReplyInThreadAndChat
    return a.sendReplyInThreadAndChat(ctx, msg.ChatID, msg.ReplyTo, text)

case gateway.OutResult:
    // F-39 不变: 独立 ReplyInThreadAndChat
    return a.sendResultAsReply(ctx, msg.ChatID, msg.ReplyTo, text)

case gateway.OutTaskCreate, gateway.OutTaskUpdate:
    // F-44: rolling-log receipt 简化为只装 Tasks
    receipt, created, err := a.ensureReceiptForTask(ctx, msg.ChatID, msg.ReplyTo, msg.TaskList)
    if err != nil {
        return a.sendRawOutText(ctx, msg.ChatID, renderTaskFallbackText(msg.TaskList))
    }
    if !created {
        return receipt.SetTaskList(ctx, msg.TaskList)
    }
    return nil

case gateway.OutInit, gateway.OutUsage:
    // F-44: silent drop，footer 设计推迟
    return nil

// 其他 case 不变
case gateway.OutThinking: ...
case gateway.OutToolStart: ...
case gateway.OutToolEnd: ...
case gateway.OutMessageState: ...
case gateway.OutMessageStateRemoved: ...
case gateway.OutCompaction: ...
case gateway.OutChoice: ...
case gateway.OutTyping: ...
case gateway.OutCommandReply: ...
}
```

### 1.3 Task Receipt 简化

**当前 receipt card**（F-25 → F-42 累计）：

```go
// buildReceiptCard (简化伪代码)
elements := []any{}
elements = append(elements, headerLine(r))       // ⏳/🔄/✅/❌
for _, e := range r.entries {
    chunks := splitMarkdownForDivs(e.Icon+" "+e.Text, divTextCharLimit)
    elements = append(elements, divElements(chunks)...)  // OutReply entries
}
elements = append(elements, taskElements...)      // **📋 Tasks** checklist
elements = append(elements, hrElement)            // <hr>
elements = append(elements, footerLine(r))        // init / usage
```

**改后**：

```go
// buildReceiptCard (F-44 后)
elements := []any{}
elements = append(elements, taskElements...)      // **📋 Tasks** checklist ONLY
```

**删除的 receipt 段**：
- `headerLine(r)` — prompt state icon（⏳/🔄/✅/❌）
- entries loop — OutReply chunks（OutReply 不再 fold）
- `hrElement` — 跟 footer 一起删
- `footerLine(r)` — init / usage（推迟到 footer PR）

**保留的 receipt 段**：
- `**📋 Tasks**` + checklist（`buildTaskChecklistChunks` 输出）

### 1.4 `sendReplyInThreadAndChat` 新 helper

平行 `sendResultAsReply`（F-39）：

```go
// internal/channel/feishu/adapter.go (NEW)
//
// sendReplyInThreadAndChat 投递 OutReply chunk 为 ReplyInThreadAndChat 独立消息。
// 锚定 msg.ReplyTo（userMsgID）让 reply 在 main chat 可见且视觉上挂在 user msg 下。
//
// 跟 sendResultAsReply (F-39) 共享 3 段 dispatch:
//   - 无 markdown → MsgTypeText
//   - tables > 5 → MsgTypePost + tag:"md"
//   - 默认 → MsgTypeInteractive Card 2.0
//
// 唯一差别:
//   - 不加 icon 前缀（OutReply 是流延续,不是新条目）
//   - 28 KB envelope defense 复用
//   - 复用 SanitizeCardMarkdown + splitMarkdownForDivs
func (a *Adapter) sendReplyInThreadAndChat(
    ctx context.Context,
    chatID, userMsgID, text string,
) error {
    if strings.TrimSpace(text) == "" {
        return nil  // 空 reply 静默 drop,跟 F-40 行为一致
    }
    sanitized, err := SanitizeCardMarkdown(text)
    if err != nil {
        a.logger.Warn("feishu: reply sanitize failed, sending raw",
            "err", err, "chat_id", chatID, "user_msg_id", userMsgID)
        sanitized = text  // sanitize 失败降级用原文
    }
    if len(sanitized) > perReplyMaxBytes {
        sanitized = truncateForLog(sanitized, perReplyMaxBytes)  // envelope defense
    }
    msgType, body, err := buildResultPayload(sanitized)  // F-39 三段 dispatch 复用
    if err != nil {
        return fmt.Errorf("feishu: encode reply: %w", err)
    }
    _, err = a.sendContent(ctx, chatID, msgType, body, userMsgID, false)  // replyInThread=false = ReplyInThreadAndChat
    return err
}
```

**复用基础设施**（不新增 helper）：
- `SanitizeCardMarkdown` — 从 `card_sanitize.go` 移到 `result_render.go`（仍 exported，因为 `buildInteractiveCard` 也调用它，见 §2.1）
- `splitMarkdownForDivs` — 已在 `receipt_split.go`
- `buildResultPayload` — F-39 已存在，三段 dispatch（text / post+md / card）
- `sendContent` — 底层 send，复用 F-37 / F-39
- `truncateForLog` — `receipt_event.go:176` 原始 helper；`result_render.go::truncateRunes` 是 thin alias（F-39 加的）。F-44 后统一调 `truncateForLog`，`truncateRunes` 删除（仅 OutResult 一处用，无 caller）

**新增常量**：
- `perReplyMaxBytes = 6 * 1024`（与 `perResultMaxBytes` 同值，独立常量保证语义清晰）— 在 `result_render.go` 定义

### 1.5 Wire 形态

| OutboundKind | F-44 wire | Feishu API | main chat | thread 视觉 |
|---|---|---|---|---|
| `OutReply` | **ReplyInThreadAndChat**（每 chunk） | `POST /messages/{rootID}/reply`, `reply_in_thread` 字段省略 | ✅ 可见 *(group chat)* / ❌ thread-only *(p2p / topic)* | ✅ reply 视觉 |
| `OutResult` | ReplyInThreadAndChat | 同上 | ✅ *(group)* / ❌ *(p2p / topic)* | ✅ *(group)* / ❌ *(p2p / topic)* |
| `OutTaskCreate` / `OutTaskUpdate` | ReplyInThreadAndChat（rolling-log card） | `POST /messages/{rootID}/reply` 创建 + `PUT /messages/{id}` PATCH | 同上 | 同上 |
| `OutInit` / `OutUsage` | Silent drop | — | — | — |
| **fallback** *(p2p / topic)* | **ReplyInChat** | `POST /im/v1/messages` 顶级 Create | ✅ 可见 | ❌ 无 thread 关联 |

**关键术语**(来自 `docs/channel/feishu-rendering.md` §2.1 + `docs/channel/feishu-rendering.md`):

- **ReplyInChat**:顶级 `Create` 端点,无 `root_id`,消息仅在 main chat 显示,**无 thread 关联、无 reply 箭头**
- **ReplyInThreadAndChat**:reply 端点 + `reply_in_thread` 字段省略,group chat 下消息显示在 main chat + thread(同正文,带 reply 箭头)
- **ReplyInThread**:reply 端点 + `reply_in_thread=true`,main chat 只显示 "X replies" 灰条,正文仅在 thread

**已知 chat-mode 影响**(2026-08-05 实机 probe,DM `oc_7cc94a3ed15afb8ac60c4ab7344d5cfd` + group `oc_4a06da49bc0131ff14b381498e4fed9d`):

| chat_mode | reply endpoint `thread_id=""` (ReplyInThreadAndChat) | reply endpoint `thread_id="omt_xxx"` (ReplyInThread) | Create 端点 (ReplyInChat) |
|---|---|---|---|
| `p2p`(DM) | ❌ 永远继承父消息 `thread_id`,看不到 main chat 可见版本 | ✅ thread-only(灰条 + thread) | ✅ **唯一** main chat 可见方式 |
| `group`(普通群)| ✅ 字段省略 / `false` → main chat 可见;`true` → thread-only | ✅ 行为如 doc 描述 | ✅ 也 main chat 可见 |
| `topic`(话题群)| ❌ SDK 注释:「若群聊已经是话题模式,则自动回复该条消息所在的话题」| ❌ 强制 topic,无法 escape | ❓ 需测试(推测:也不可见)|

**3 种形态实机确认**(2026-08-05 12:50~12:59,DM):

| Probe | Endpoint | 字段 | `thread_id` | 视觉 | 形态 |
|---|---|---|---|---|---|
| D | Create | n/a | `<empty>` | 独立气泡,main chat | ReplyInChat ✅ |
| C | reply | `true` | `omt_xxx` | 灰条 + thread | ReplyInThread ✅ |
| A/B | reply | omit / `false` | `omt_xxx`(DM 下)| 仅 thread(DM 特性) | (DM 下:仅 thread;group 下:ReplyInThreadAndChat) |

**F-44 P2/P3 backlog**:
- **P2**:chat-mode 探测(`chat_mode` LRU 缓存,类似 `messageStates`),p2p/topic 群下自动 fallback 到 `sendViaLarkCreate`,达成 main chat 可见
- **P3**:`p2p` / `topic` 群检测在 `Adapter` startup 时 warm-up,避免首条消息延迟
- **接受现状** 也行:当前实现跟 F-37 / F-40 在 group chat 下行为完全一致;p2p / topic 群用户在 DM 里看到 "X replies" 灰条不影响 main chat 可达性(只是不直观)

其他 OutboundKind 不变(F-37 已处理 thinking/tool/compaction 走 `ReplyInThread`,`OutChoice` / `OutCommandReply` 走 `ReplyInThreadAndChat`)。

**`ReplyInThreadAndChat` 锚定语义**：所有 reply 都设 `root_id = userMsgID`，`reply_in_thread = false`（字段省略）。飞书端：消息在 main chat 可见正文，同时在 thread 入口处有视觉 reply 链。多个 reply 共享同一 `root_id` → 飞书把它们组织成"同一 user msg 的 reply 串"。

### 1.6 Receipt 状态机简化

**当前状态**（F-25 → F-42 累计）：

```
PromptPending → PromptRunning → PromptSucceeded / PromptFailed
                  ├ entries: append
                  ├ tasks: snapshot replace
                  └ init/usage: footer append
```

**改后状态**（F-44）：

```
PromptPending → PromptRunning → PromptSucceeded / PromptFailed
                  └ tasks: snapshot replace (only)
```

**删除的状态**：
- `entries []LogEntry` — 整个字段 + 配套 append / dedup / 截断 / 拆 div 逻辑
- `headerLine` / `footerLine` 计算 — header / footer sections 删除
- `promptHeaderLine` 调用 — header section 删除
- `promptState` 转 `PromptRunning` 时机 — 仍然保留（用于 SetCompleted / SetExecuting），但驱动源从 `OutReply` 首次到达改成 `OutTaskCreate/Update` 首次到达

**注意**：`promptState` 字段保留（仍由 `EventDone` / `EventError` 翻终态），但 state transition 触发源变化：
- 当前：`OutReply` 首次到达 → `PromptRunning`；后续 chunks 不再 transition
- 改后：`OutTaskCreate/Update` 首次到达 → `PromptRunning`（task receipt 创建时设）；`OutReply` 不再触发 transition
- 边缘 case：turn 无 task 仅有 reply → receipt 不存在 → `PromptRunning` / `PromptSucceeded` 永远不设（**符合 §1.4 边界**：state 是 Channel 内部状态，缺失不影响其他 Kinds 渲染）

---

## 2. 文件 & 接口

### 2.1 改动的文件

#### **`internal/channel/feishu/adapter.go`**

```go
// (1) Send case OutReply:重写
case gateway.OutReply:
    // F-44: 独立 ReplyInThreadAndChat,不再 fold
    text := strings.TrimSpace(msg.Text)
    if text == "" {
        return nil
    }
    return a.sendReplyInThreadAndChat(ctx, msg.ChatID, msg.ReplyTo, text)

// (2) Send case OutInit / OutUsage: silent drop
case gateway.OutInit, gateway.OutUsage:
    // F-44: silent drop;footer 设计推迟到 footer PR
    // 字段仍在 OutboundMessage wire 上(translate.go 不变),Channel 自决不渲染
    return nil

// (3) 新 helper sendReplyInThreadAndChat (见 §1.4)

// (4) 删除:ensureReceiptForReply / isOverflowingReceipt / sendReplyAsMessage
//     这三个 helper 是 F-40 / F-42 加的,F-44 不再需要
```

**删除的代码**：
- `ensureReceiptForReply` (~80 行)
- `isOverflowingReceipt` (~30 行)
- `sendReplyAsMessage` (~80 行)
- `Send` case `OutReply` 中 fold / overflow / late-reply / no-receipt-fallback 协调逻辑 (~60 行)
- `Send` case `OutInit` / `OutUsage` 中 `receipt.Append` 调用 (~30 行)

#### **`internal/channel/feishu/receipt.go`**

```go
// (1) buildReceiptCard 简化:只剩 tasks section
func buildReceiptCard(r *MessageReceipt) (string, error) {
    elements := []any{}
    if chunks := buildTaskChecklistChunks(r.tasks); len(chunks) > 0 {
        for _, c := range chunks {
            elements = append(elements, map[string]any{
                "tag": "markdown",
                "content": c,
            })
        }
    }
    card := map[string]any{
        "schema": "2.0",
        "config": map[string]any{"wide_screen_mode": true},
        "body":   map[string]any{"elements": elements},
    }
    // encodeCardJSON ...
}

// (2) renderLocked 简化:只剩 task snapshot 替换路径
func (r *MessageReceipt) renderLocked(ctx context.Context) error {
    // 保留:task snapshot replace → buildReceiptCard → PATCH
}

// (3) 删除的内部方法:
func (r *MessageReceipt) Append(...) error            // ← 删除,见 §2.1 备注
func (r *MessageReceipt) SetExecuting(...) error     // ← 删除(无 caller)
func (r *MessageReceipt) SetCompleted(...) error     // ← 删除(无 caller)
func (r *MessageReceipt) appendEntryLocked(...)      // ← 删除(Append 私有)
func (r *MessageReceipt) lastEntryLocked() *LogEntry // ← 删除(Append 私有)
func (r *MessageReceipt) EntryCount() int            // ← 删除(isOverflowingReceipt 唯一 caller 删除)
func (r *MessageReceipt) evictOverflowLocked()       // ← 删除(appendEntryLocked 私有)

// (4) 保留:SetTaskList (task snapshot replace)

// (5) 删除的字段:
//     entries []LogEntry                  ← appendEntryLocked 唯一写者删除,无读者
//     agentName / workspace / branch      ← OutInit silent drop,无写者;buildReceiptCard 不读
//     inputTokens / outputTokens          ← OutUsage silent drop,无写者;buildReceiptCard 不读
//     completedAt                         ← promptHeaderLine 整体删除,无读者;footer PR 重新引入 header 时同步加回
// 保留的字段:
//     promptState                         ← 保留(用于 footer PR 状态恢复,见 §6.2;当前 EventDone/EventError 不通过 Send 触发 transition,实际也是 dead state,见 §5.2)
//     chatID / userMsgID / replyMsgID / cardMsgID / bot / logger / tasks / mu
```

**删除的代码**：
- `buildReceiptCard` 中 header / entries / footer / hr sections (~250 行)
- `renderLocked` 中 entries PATCH / init / usage 部分 (~80 行)
- `Append` (~70 行) — 内部仍保留 `case EventDone` / `case EventError` / `case EventAgentConnected` / `case EventUsage` 四个 case，但**这些 case 在 F-44 后都成为 unreachable**（详见 §5.2）
- `SetExecuting` (~20 行) — 无 production caller
- `SetCompleted` (~40 行) — 无 production caller
- `appendEntryLocked` / `lastEntryLocked` / `EntryCount` / `evictOverflowLocked` (~55 行) — Append 私有 / 唯一外部 caller 已删
- `MessageReceipt.entries` / `agentName` / `workspace` / `branch` / `inputTokens` / `outputTokens` 字段（6 个）+ setter (~50 行)
- `promptHeaderLine` / `footerLine` 计算 (~50 行)
- **receipt.go 段小计: ~615 行**

#### **`internal/channel/feishu/receipt_event.go`**

**整个文件删除**。

理由：F-44 后 `MessageReceipt.Append` 整体删除，`Append` 是 `eventToEntry` 在 production 的唯一 caller（`ensureReceiptForReply` 也调它但同步删除）。`eventToEntry` 9 个 case（`EventText` / `EventToolStart` / `EventToolEnd` / `EventError` / `EventPermission` / `EventDone` / `EventUsage` / `EventCompaction` / `EventAgentConnected`）全部 unreachable：

| eventToEntry case | Append 前是否有用 | F-44 后状态 |
|---|---|---|
| `EventText` | OutReply case 触发，append 💬 entry | Append 删除 → unreachable |
| `EventToolStart` / `EventToolEnd` | F-34 后已不返回 entry（(_, false)） | Append 删除 → unreachable |
| `EventError` | OutReply case 不传 EventError；EventError 不通过 Send | unreachable（Append 即使保留，OutReply 也不会传 EventError） |
| `EventPermission` | F-34 后已不返回 entry | Append 删除 → unreachable |
| `EventDone` | EventDone 不通过 Send | unreachable |
| `EventUsage` | OutUsage case 触发，但 F-44 改 silent drop | Append 删除 → unreachable |
| `EventCompaction` | F-34 后已不返回 entry；OutCompaction 也不调 Append | Append 删除 → unreachable |
| `EventAgentConnected` | OutInit case 触发，但 F-44 改 silent drop | Append 删除 → unreachable |

**连带删除**：
- `LogEntry` struct（仅 `eventToEntry` / `appendEntryLocked` 用，0 caller）
- `formatUsageText`（仅 `eventToEntry(EventUsage)` / test 用，0 caller）
- `truncateForLog`（原始 helper；但 `receipt_event_test.go` 仍有测试，且 `internal/bridge/claudecode/stream.go:584` 有同名 duplicate。F-44 后 `truncateForLog` 留在 `result_render.go` 作为唯一实现；claudecode duplicate 是另一包，**不动**）
- `thinkingPrefix` 常量（仅 `eventToEntry(EventText)` 用，0 caller）
- **receipt_event.go 段小计: ~210 行**（整个文件）

**连带删除测试文件**：
- `receipt_event_test.go` ~ 全文件删除（覆盖 `eventToEntry` / `truncateForLog` / `formatUsageText`，这些都删了）

#### **`internal/channel/feishu/card_sanitize.go`**

整个文件**合并进 `result_render.go`** 作为私有函数。当前 OutReply + OutResult 都用 `SanitizeCardMarkdown`，F-44 后只剩 OutReply + OutResult 两个 caller（都是 `buildResultPayload` 路径），不需要独立文件。

**删除的代码**：
- `card_sanitize.go` 整个文件 (~200 行)
- 移到 `result_render.go` 私有函数 + 移除独立 export

#### **`internal/channel/feishu/receipt_task.go`**

**不变**。`buildTaskChecklistChunks` / `renderTaskLine` / `renderTaskFallbackText` 继续为 task receipt 服务。`renderTaskFallbackText` **保留** — 作为 `ensureReceiptForTask` SendCard 失败时的降级路径（F-44 不动 task receipt，失败路径仍可用）。

#### **`internal/gateway/messages.go`**

**不变**。`OutboundMessage` 字段（`Init` / `Usage` typed field）保持；F-44 仅在 Channel 层 silent drop，wire format 不变。

#### **`internal/gateway/translate.go`**

**不变**。`EventAgentConnected` / `EventUsage` 仍翻译为 `OutboundMessage{Init: ...}` / `{Usage: ...}`；footer PR 会用。

### 2.2 保留不变的（确认无副作用）

- **`OutboundMessage` 全字段** — `Init` / `Usage` typed field 保留（footer PR 用）
- **Gateway / ChatSession / Bridge / Registry** — 全部不变
- **EventHandler** (`cmd/nightme/run.go::newEventHandler`) — 不变；F-44 在 Channel 层完成
- **`OutResult` 路径** (F-39 `sendResultAsReply`) — 不变
- **`OutThinking` 路径** (F-think `postThreadMarkdownReply`) — 不变
- **`OutToolStart` / `OutToolEnd` 路径** (F-38 `tool_thread_merge.go`) — 不变
- **`OutCompaction` 路径** (`postThreadReply`) — 不变
- **`OutChoice` 路径** (`buildInteractiveCard`) — 不变 — 注意此路径也调用 `SanitizeCardMarkdown`，所以 `SanitizeCardMarkdown` 必须保留为 exported
- **`OutMessageState` 路径** (F-31 reactions) — 不变
- **`OutCommandReply` 路径** (`SendMessageText`) — 不变
- **Task receipt 路径** (`ensureReceiptForTask` / `SetTaskList`) — 不变
- **`splitMarkdownForDivs` / `buildResultPayload` / `SanitizeCardMarkdown` / `truncateForLog`** — 内部 helper 全部复用（F-44 把 `SanitizeCardMarkdown` / `truncateForLog` 从 `card_sanitize.go` / `receipt_event.go` 搬到 `result_render.go` 集中管理）
- **`MessageReceipt.promptState` 字段** — 保留（footer PR 可恢复 receipt prompt state header，见 §6.2；当前 EventDone/EventError 不通过 Send 触发 transition，state 字段暂时不写入也不读取，无副作用）

### 2.3 Send case 改动汇总

| Case | F-44 前 | F-44 后 | 副作用 |
|---|---|---|---|
| `OutReply` | ensureReceipt + overflow check + fold / bail-out (~80 行) | `sendReplyInThreadAndChat` (~10 行) | `receipt.Append` 调用消失；无 LogEntry 写入 |
| `OutResult` | `sendResultAsReply` | 不变 | 无 |
| `OutTaskCreate` / `OutTaskUpdate` | `ensureReceiptForTask` + fallback | 不变 | 无 |
| `OutInit` / `OutUsage` | `receipt.Append(EventAgentConnected/Usage)` 写 `agentName/workspace/branch/tokens` 字段 | `return nil` silent drop | `agentName/workspace/branch/tokens` 字段变 orphan → 整体删除 |
| `OutMessageState` / 其他 | — | 不变 | 无 |

---

## 3. 测试覆盖

### 3.1 单元测试

| 文件 | 用例 | 断言 |
|---|---|---|
| `internal/channel/feishu/adapter_test.go` | `TestSend_OutReply_IndependentReply` | mock sendContent；`Send(OutReply{Text: "hello"})` → `sendContent` called **1 次** with `replyInThread=false`（ReplyInThreadAndChat）；**不**调 `receipt.Append` |
| 同上 | `TestSend_OutReply_EmptyText_SilentDrop` | `OutReply{Text: "  "}` → 静默 drop,无 sendContent 调用 |
| 同上 | `TestSend_OutReply_NoReceiptRequired` | `receiptFor` 返回 nil 也不影响 OutReply 投递（不需要 receipt） |
| 同上 | `TestSend_OutReply_LongText_EnvelopeDefense` | 15000 runes text → 触发 `truncateRunes` 到 envelope budget |
| 同上 | `TestSend_OutReply_NoIconPrefix` | 投递 body **不带** `💬` 前缀（OutReply 是流延续） |
| 同上 | `TestSend_OutReply_3SegmentDispatch_NoMarkdown` | plain text → `MsgTypeText` |
| 同上 | `TestSend_OutReply_3SegmentDispatch_LotsTables` | 6 markdown tables → `MsgTypePost + tag:"md"` |
| 同上 | `TestSend_OutReply_3SegmentDispatch_Default` | markdown ≤ 5 tables → `MsgTypeInteractive` |
| 同上 | `TestSend_OutReply_SanitizeApplied` | text 含 `[x](relative)` → sanitize 后 plain text |
| 同上 | `TestSend_OutResult_Unchanged` | F-39 路径回归测试,确保 OutResult 不受 F-44 影响 |
| 同上 | `TestSend_OutInit_SilentDrop` | `OutInit` → 无 sendContent / receipt.Append 调用 |
| 同上 | `TestSend_OutUsage_SilentDrop` | 同上 |
| `internal/channel/feishu/receipt_test.go` | `TestBuildReceiptCard_TaskOnly` | receipt 含 task → card body 只含 `**📋 Tasks**` markdown elements;**不**含 header / entries / footer / hr |
| 同上 | `TestBuildReceiptCard_NoTask_EmptyCard` | receipt 无 task → card body elements 为空（SendCard 仍可发,Feishu 接受空 elements） |
| 同上 | `TestBuildReceiptCard_TaskSnapshotReplace` | 两次 `SetTaskList` 不同 snapshot → 第二次 PATCH 替换整段 checklist |
| 同上 | `TestRenderLocked_TaskReceiptUpdates` | task snapshot 变化触发 PATCH;无 task → 无 PATCH |
| `internal/channel/feishu/receipt_event_test.go` | **整个文件删除** | 覆盖 `eventToEntry` / `truncateForLog` / `formatUsageText` — 这些函数全部删除 |
| `internal/channel/feishu/receipt_test.go`(扩) | `TestAppend_Deleted` | 删除测试:compile-time + grep 确认 `MessageReceipt.Append` 不存在 |
| 同上 | `TestSetCompleted_SetExecuting_Deleted` | 删除测试:确认 `SetCompleted` / `SetExecuting` 不存在 |
| 同上 | `TestLogEntry_Deleted` | 删除测试:确认 `LogEntry` struct 不存在 |

### 3.2 集成测试（回归）

| 文件 | 用例 | 断言 |
|---|---|---|
| `internal/channel/feishu/adapter_test.go` | `TestFullTurn_OutReplyIndependent_TaskReceipt` | 完整 turn：5 个 OutReply chunk + 2 个 OutTaskCreate + 1 个 OutResult。验证：5 个独立 reply 投递（按顺序）+ 1 张 task receipt（PATCH 2 次）+ 1 个独立 OutResult。`receiptFor` 调用次数 = 0（OutReply 不查 receipt） |
| 同上 | `TestFullTurn_OnlyReply_NoReceipt` | 完整 turn：5 个 OutReply chunk + 1 个 OutResult。验证：5 个独立 reply + 1 个 OutResult;**不创建** receipt card;`MessageReceipt` 实例计数 = 0 |
| 同上 | `TestFullTurn_OnlyTask_NoReply` | 完整 turn：3 个 OutTaskUpdate + 1 个 OutResult。验证：1 张 task receipt + 1 个 OutResult |
| 同上 | `TestFullTurn_OutUsage_Init_Dropped` | OutInit + OutUsage 到达 → 静默 drop,无 sendContent / receipt 操作 |

### 3.3 grep / 回归（收尾）

```bash
# 验证 ensureReceiptForReply / isOverflowingReceipt / sendReplyAsMessage 完全消失
rg -n "ensureReceiptForReply|isOverflowingReceipt|sendReplyAsMessage" --type=go  # 期望: 0 命中

# 验证 Append / SetExecuting / SetCompleted 整条 dead chain 消失
rg -n "func.*MessageReceipt.*(Append|SetExecuting|SetCompleted|appendEntryLocked|lastEntryLocked|EntryCount|evictOverflowLocked)" --type=go  # 期望: 0 命中
rg -n "type LogEntry struct" --type=go  # 期望: 0 命中
rg -n "func eventToEntry|func formatUsageText" --type=go  # 期望: 0 命中
ls internal/channel/feishu/receipt_event.go 2>&1  # 期望: No such file
ls internal/channel/feishu/receipt_event_test.go 2>&1  # 期望: No such file

# 验证 buildReceiptCard 不再引用 entries / agentName / workspace / branch / inputTokens / outputTokens
rg -n "r\.entries|r\.agentName|r\.workspace|r\.branch|r\.inputTokens|r\.outputTokens" internal/channel/feishu/receipt.go  # 期望: 0 命中

# 验证 card_sanitize.go 已删除,SanitizeCardMarkdown 移到 result_render.go
ls internal/channel/feishu/card_sanitize.go 2>&1  # 期望: No such file
rg -n "func SanitizeCardMarkdown" --type=go  # 期望: 1 命中 (in result_render.go)

# 验证 OutReply / OutResult / OutTask* 路径独立可测
go test ./internal/channel/feishu/... -run TestSend_OutReply
go test ./internal/channel/feishu/... -run TestSend_OutResult
go test ./internal/channel/feishu/... -run TestSend_OutTask
```

---

## 4. 落地顺序

每步独立 commit，可单独 review + revert：

| Step | 内容 | 文件 | 风险 |
|---|---|---|---|
| 1 | **本文档** (`../channel/feishu-rendering.md`) | `docs/feat/` | 零 |
| 2 | SPEC.md + §2.4 更新 | `docs/SPEC.md` | 零 |
| 3 | channel/feishu.md 渲染映射表更新 | `docs/channel/feishu-rendering.md` | 零 |
| 4 | FEATURES.md 索引条目 | `docs/FEATURES.md` | 零 |
| 5 | `card_sanitize.go` 合并到 `result_render.go`(`SanitizeCardMarkdown` 仍 exported,因为 `buildInteractiveCard` 也调用) | `internal/channel/feishu/` | 低 |
| 6 | `receipt_event.go` 整个文件删除 | `internal/channel/feishu/receipt_event.go` | 中（连带删 `LogEntry` / `eventToEntry` / `formatUsageText` / `thinkingPrefix`） |
| 7 | `receipt_event_test.go` 整个文件删除 | `internal/channel/feishu/receipt_event_test.go` | 低 |
| 8 | `receipt.go` 删 `Append` / `SetExecuting` / `SetCompleted` / `appendEntryLocked` / `lastEntryLocked` / `EntryCount` / `evictOverflowLocked` + `entries` / `agentName` / `workspace` / `branch` / `inputTokens` / `outputTokens` 字段 + 简化 `buildReceiptCard` + `renderLocked` | `internal/channel/feishu/receipt.go` | 中-高 |
| 9 | `adapter.go` 删 `ensureReceiptForReply` / `isOverflowingReceipt` / `sendReplyAsMessage` | `internal/channel/feishu/adapter.go` | 中 |
| 10 | `adapter.go` 加 `sendReplyInThreadAndChat` helper + 新增 `perReplyMaxBytes` 常量 | `internal/channel/feishu/adapter.go` | 中 |
| 11 | `adapter.go::Send` case `OutReply` 重写 + case `OutInit`/`OutUsage` silent drop | `internal/channel/feishu/adapter.go` | 中 |
| 12 | `adapter_test.go` 新增 OutReply 独立 reply 测试 + 删 OutReply fold / overflow 测试 | `internal/channel/feishu/adapter_test.go` | 低 |
| 13 | `receipt_test.go` 新增 Task-only 测试 + 简化既有测试 + 加 dead code 删除测试（`Append` / `SetCompleted` / `LogEntry` 不存在） | `internal/channel/feishu/receipt_test.go` | 低 |
| 14 | 全量 `go test ./...` + `go vet` + `golangci-lint` | — | 必过 |

---

## 5. 与上下游契约

### 5.1 OutboundMessage 契约

**不变**。`Init` / `Usage` typed field 保留（footer PR 会用）；`ReplyTo` 不变；`Kind` enum 不变；`Text` / `Result` / `TaskList` 等不变。F-44 是 Channel 内部路由调整，wire format 完全保持。

### 5.2 ChatSession 状态机 + EventDone/EventError 流

**ChatSession 状态机不变**。`currentTurnUserMsgID` / `InputBuffer` / `EventCallback` 全部不变。F-44 不读 ChatSession 任何新字段。

**`EventDone` / `EventError` 流说明（重要，跟当前实现的 dead code 路径对齐）**：

- `agent.EventDone` / `agent.EventError` **不通过** `OutboundMessage` 路径走 `Adapter.Send`
- 它们通过 `ChatSession.emitMessageStateForCurrentTurn(MessageDone / MessageFailed)` → `Gateway.OnMessageState` → `OutboundMessage{Kind: OutMessageState, MessageState: {State, MessageID}}` → `Adapter.Send(OutMessageState)` → Feishu `AddReaction`
- 这意味着 `MessageReceipt.Append` 内部的 `case agent.EventDone` / `case agent.EventError` 分支在当前 production 上**已经是 unreachable** — `Append` 唯一 production caller (`Adapter.Send(OutReply)`) 只传 `EventText`，不传 `EventDone/Error`
- F-44 把 `Append` 整体删除即可，不需要单独迁移 EventDone/Error 处理逻辑
- 终态信号完全靠 `OutMessageState` → Feishu `AddReaction(userMsgID, ✅/❌)` 表达，跟 receipt 状态机彻底解耦

**`promptState` 字段保留**:`completedAt` 实际已删(没有读者 — `promptHeaderLine` 函数整体删除)。理论上 footer PR 可以让 receipt prompt state header 复活（§6.2）— 届时 `promptState` 字段的 `PromptPending / PromptRunning / PromptSucceeded / PromptFailed` 转换由新增的 renderLocked 触发器驱动(`completedAt` 字段会同时加回以承载终态时间戳)。当前 EventDone/EventError 不通过 Send 触发 transition,字段暂时不写入也不读取,无副作用。

### 5.3 F-31 MessageState（reaction lifecycle）

**不变**。`MessageState` 4 态 → AddReaction 路径完全保留；F-44 不影响 user msg reaction 渲染。

### 5.4 F-37 thread routing（OutThinking / OutTool* / OutCompaction）

**不变**。三类 Kind 仍走 `postThreadReply` / `postThreadMarkdownReply`（thread reply，不是 main chat）。F-44 不触及。

### 5.5 F-38 task checklist

**保留并简化**。Task snapshot 替换式更新逻辑不变；receipt card 只剩 task section，删除 header / entries / footer sections。`ensureReceiptForTask` / `SetTaskList` / `buildTaskChecklistChunks` 不动。

### 5.6 F-39 OutResult 独立 reply

**不变**。`sendResultAsReply` 三段 dispatch 仍服务 OutResult。F-44 新增 `sendReplyInThreadAndChat` 是 sibling helper，共享 `buildResultPayload` / `SanitizeCardMarkdown` / `splitMarkdownForDivs` / `truncateRunes`。

### 5.7 F-40 OutReply 改名 + 超限改独立 reply

**反转（部分）**：
- ✅ `OutText` → `OutReply` 改名**保留**（语义正确）
- ✅ 删 600B truncate **保留**
- ✅ 删 `eventToEntry(EventText)` case **保留**（F-44 进一步把整个 `eventToEntry` 函数连带文件删除）
- ✅ 删 `MessageReceipt.entries` 字段 `LogEntry` 路径 **保留**（F-44 进一步把 `LogEntry` struct 连带整个 `receipt_event.go` 文件删除）
- ❌ Overflow bail-out（长度 / 数量）→ **删除**（OutReply 不再 fold，无 overflow 概念）
- ❌ 迟到 reply bail-out → **删除**（OutReply 独立后不再有"迟到"语义）
- ❌ `sendReplyAsMessage` helper → **删除**（被 `sendReplyInThreadAndChat` 替代）
- ❌ `MessageReceipt.Append` 整体 → **删除**（OutReply / OutInit / OutUsage 三个 caller 全消失；EventDone/Error case 本就是 dead code，详见 §5.2）
- ❌ `MessageReceipt.SetExecuting` / `SetCompleted` → **删除**（无 production caller）

### 5.8 F-42 lazy receipt creation

**简化**：
- ✅ Receipt lazy create（不在 cold-start 空 receipt）**保留**（OutTask* 触发）
- ✅ MessageState ⏳/🔄 silent drop 留 ✅/❌ **保留**（不受 F-44 影响）
- ✅ TaskList `**📋 Tasks**` title **保留**
- ❌ `ensureReceiptForReply` → **删除**（OutReply 不再触发 receipt 创建）
- ❌ Cold-start fallback text for reply → **删除**

### 5.9 §1.4 抽象 / 具体 边界

**保留**。F-44 不引入新 typed field，不修改 `OutboundMessage` wire format；所有变化在 Channel 内部渲染目标分流范畴内。

---

## 6. 后续工作（本文档不做 — 推迟到 footer PR）

### 6.1 OutInit / OutUsage footer 渲染

- 新增 `SessionMeta *SessionMeta` typed field 到 `OutboundMessage`（或扩展 `Init` 字段）
- ChatSession 持有 `SnapshotInit()` + `LiveBranch()` + `LiveCwd()` + `InvalidateBranchCache()` API
- EventHandler 在每次 emit 时戳印 `SessionMeta`
- Channel 在 `sendReplyInThreadAndChat` / `sendResultAsReply` / `ensureReceiptForTask` 内部读 `msg.SessionMeta` 渲染 footer

### 6.2 Task receipt header 恢复（可选）

- 如果 footer PR 决定给 task receipt 恢复 prompt state header（⏳/🔄/✅/❌），加 `promptHeaderLine` 回 `buildReceiptCard`
- F-44 不预设

### 6.3 Receipt "OutReply history" 折叠（可选 / 长期）

- 如果用户觉得"多 reply 流"过长，可以加 per-turn "展开历史"折叠按钮
- 需要 Web / Slack 等其他 Channel 适配
- F-44 不预设

---

## 7. 不变式总结（本文档特殊要求）

**F-44 改 OutReply fold → 独立 reply + 简化 task receipt + silent drop OutInit/OutUsage，但保留：**

- `OutboundMessage` 全字段不变（`Init` / `Usage` typed field 保留，footer PR 用）
- Gateway 不变（`Translate` 仍产 OutboundMessage）
- ChatSession 不变（`currentTurnUserMsgID` 单数锚点 + `InputBuffer` 不变）
- `OutboundMessage.ReplyTo = cs.currentTurnUserMsgID` 不变（独立 reply 也锚同 userMsgID）
- 1 turn : 1 anchor 不变式保留
- §1.4 抽象 / 具体 边界规范保留（Init/Usage 是 typed primitive，Channel 自决渲染目标）
- 抽象归抽象 / 具体归具体原则保留（独立 reply 是 Feishu 自治决策）
- F-25 rolling-log UX **部分保留**：task receipt 仍是 rolling-log；OutReply 不再 rolling-log（改为独立 reply 流）
- F-31 MessageState 抽象契约不变
- F-37 thread routing 决策不变（thinking/tool/compaction 仍 thread reply）
- F-38 task checklist 决策不变（task snapshot 仍是 receipt 单一 section）
- F-39 OutResult 决策不变（OutResult 仍独立 reply）
- F-40 OutReply 命名 + 删 600B truncate 决策保留；overflow / late-reply bail-out 删除（不再需要）
- F-42 lazy receipt creation 决策保留；仅 task receipt 路径触发，reply 路径删除

---

## A10. F-45: Main-Chat 卡片 Footer (per-turn snapshot)

> **Source**: `../channel/feishu-rendering.md`


> **Scope**:

> **Related**: [`SPEC.md`](../SPEC.md)（本文落地）/ §1.3 / §1.4 / §2.2；[`channel/feishu-rendering.md`](../channel/feishu-rendering.md) / §13.22 / §13.24 (F-48 git branch) / §13.25 (F-49 compaction counter) / §18；[`F-44 §6.1`](./../channel/feishu-rendering.md) 推迟项兑现；**[`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md)** 决策卡 button + 原地 PATCH；§1.7 F-48 git branch follow-up；§1.8 F-49 compaction counter follow-up；详细 F-49 设计见 [`F-49`](./../channel/feishu-rendering.md)

---

## 0. 背景

### 0.1 F-44 §6.1 推迟的 footer

F-44 在 `internal/channel/feishu/receipt.go` 把 receipt 简化为只装 Task checklist，删除了 footer（`init` / `usage`）段。同时 §6.1 明确推迟了 footer 的兑现：

**F-45 是这份兑现**。但设计经过两轮迭代（见 §0.3），比 F-44 §6.1 草拟更紧凑。

### 0.2 现状

**token 数据已经到达 Gateway，但不被消费**：
- `internal/agent/agent.go:296` `UsageEvent`（4 个 token 字段 + CostUSD）
- `internal/bridge/claudecode/stream.go:601` `decodeUsage` 解析 Anthropic API 字段
- `internal/gateway/translate.go:158` `Translate(EventUsage)` 产出 `OutboundMessage{Kind: OutUsage, Usage: *UsageInfo}`
- `internal/channel/feishu/adapter.go::Send` case `OutUsage`：**silent drop**（F-44 §0.11 落地）

**model 数据已经到达 Gateway，也不被消费**：
- `agent.AgentConnectedEvent.Model` 字段已存在（`internal/agent/agent.go:341`）
- `internal/gateway/translate.go:205` 拼字符串 `"session initialized (model: %s)"`，但这个字符串随 `OutInit` 一起被 silent drop

**AgentSession wrapper 完全不感知这些**：
- `internal/chatsession/agentsession.go:43` `AgentSession` struct 只有 ID / ChatSessionID / Agent / Cwd / pid / status / args / 时间戳 / ExitCode / ResumeID / handle / handleEventsClosed
- **没有任何 token / model / cost 字段**

### 0.3 设计迭代（两轮收紧）

#### 第一轮：3 个独立 typed field

最初设想是给 `OutboundMessage` 加 3 个分散字段：

```go
AgentName       string  // runtime 填 s.Agent
Model           string  // runtime 填 s.Model()
Usage *agent.UsageEvent  // bridge 报的本轮 usage — runtime 直接 out.Usage 透传
```

**问题**：Channel 拿到 3 个字段后要自己拼装 footer，metadata 关系散落 3 处，扩展新字段（如 `provider_url` / `agent_version`）要继续加字段。

#### 第二轮（采纳）：1 个 `StatusBar` typed snapshot

把所有 metadata 收拢到 1 个 typed struct：

```go
type StatusBar struct {
    Agent           string
    Model           string
    Usage *agent.UsageEvent
}

type OutboundMessage struct {
    // ...既有字段...
    StatusBar *StatusBar  // ← 单一字段
}
```

**收益**：
- wire 更紧凑（1 个字段 vs 3 个）
- Channel 不需要知道"agent / model / tokens 是分别维护的"——`StatusBar` 是 1 个 atomic snapshot
- 未来扩展新字段只改 `StatusBar` 定义，不破 Channel 接口
- runtime 维护 AgentSession 的 metadata 是单一职责——「AgentSession 自描述」

### 0.4 用户问题澄清

实施前的对话澄清了三个关键点：

**Q：IN 和 Cache Read 是包含还是分开？**

**Q：Agent name 是不是已经在 AgentSession 上？**

**Q：Model 能不能也放到 AgentSession 上？**

**Q：footer 格式偏好？**

### 0.5 持久化范围澄清

**Q：cumulative 持久化到文件？什么时候清零？**

---

## 1. 设计

### 1.1 视觉对比

**改前**（典型 turn：5 个 OutReply chunk + 2 个 OutTaskCreate + 1 个 OutResult）：

```
main chat:
  ├ Reply 1  💬 chunk 1                    ⬅ 纯文本（无 token 信息）
  ├ Reply 2  💬 chunk 2
  ├ Reply 3  � chunk 3
  ├ Receipt Card (only Tasks, F-44 瘦身后)
  │   **📋 Tasks** checklist (2 items)
  └ Reply 4  📝 complete OutResult text

用户看不到 token / model / cost —— 信息丢失（F-44 §6.1 推迟项）。
```

**改后**（同样 turn，footer 在每条 main-chat 消息底部）：

```
main chat:
  ├ Reply 1  💬 chunk 1
  │           ─────────────────────────────
  │           claude · opus-4-5 · ↓ 12.3k · ↻ 8.2k cached · ↑ 1.5k · Total 22.0k · $0.087
  ├ Reply 2  � chunk 2
  │           claude · opus-4-5 · ↓ 15.1k · ↻ 9.4k cached · ↑ 1.8k · Total 26.3k · $0.103
  ├ Reply 3  💬 chunk 3
  │           claude · opus-4-5 · ↓ 18.0k · ↻ 10.7k cached · ↑ 2.1k · Total 30.8k · $0.119
  ├ Receipt Card (Tasks)
  │   **📋 Tasks** checklist (2 items)
  │   ────────────────────────────────────
  │   claude · opus-4-5 · ↓ 18.0k · ↻ 10.7k cached · ↑ 2.1k · Total 30.8k · $0.119
  └ Reply 4  📝 complete OutResult text
              ─────────────────────────────
              claude · opus-4-5 · ↓ 18.0k · ↻ 10.7k cached · ↑ 2.1k · Total 30.8k · $0.119
```

**关键变化**：
- 每条 main-chat 消息底部都带 footer
- cumulative 单调递增（每次 footer 显示"截至此刻"）
- 第一次 reply 已经有完整 footer（cumulative 已经含前几个 turn 的数据）
- Task receipt card 末尾也带 footer（与 reply 同步）
- footer 跟 reply 主体视觉上分隔（用 hr / 空白行）

### 1.2 Routing 分流表（F-45 后）

| OutboundKind | 表面 | Footer |
|---|---|---|
| `OutReply` | **ReplyInThreadAndChat**（每 chunk） | ✅ footer 在文末 |
| `OutResult` | ReplyInThreadAndChat | ✅ footer 在文末 |
| `OutTaskCreate` / `OutTaskUpdate` | **Rolling-log receipt card**（Tasks） | ✅ footer 在 checklist 末尾 |
| `OutChoice` (permission) | Top-level Create | ❌ 不带 footer（短状态消息） |
| `OutCommandReply` | Top-level Create | ❌ 不带 footer |
| `OutThinking` / `OutToolStart` / `OutToolEnd` | `ReplyInThread` | ❌ 不带 footer（thread 视觉独立） |
| `OutMessageState` | AddReaction | ❌ 不带 footer |
| `OutInit` / `OutUsage` | Silent drop（F-44 不变） | — |

**stamping 规则**（runtime 决定，不在 Channel）：
```go
switch out.Kind {
case gateway.OutReply, gateway.OutResult,
    gateway.OutTaskCreate, gateway.OutTaskUpdate:
    stamp StatusBar  // ← 4 个 main-chat Kind
}
```

### 1.3 StatusBar 字段语义

```go
// internal/gateway/messages.go (NEW)
type StatusBar struct {
    // Agent is the registry name of the agent that produced this
    // outbound event (e.g. "claude", "codex"). Sourced from
    // AgentSession.Agent — immutable, no lock needed at read site.
    Agent string

    // Model is the model the agent selected (Claude Code:
    // system/init.model). Sourced from AgentSession.Model, which
    // the runtime captures on first EventAgentConnected. Empty before
    // EventAgentConnected lands — footer helper omits the segment when "".
    Model string

    // CumulativeUsage is the per-AgentSession running total of
    // token / cost stats as of this event's emission. Sourced
    // from AgentSession.CumulativeUsage, which the runtime
    // accumulates on every EventUsage. Captured by VALUE (struct
    // copy under RWMutex) so Channel can render at leisure.
    //
    // All 4 token fields are zero on a fresh /new'd session.
    // Channel derives Total = In + CacheCreate + CacheRead + Out
    // at render time; no Total field on the wire (avoids redundancy).
    Usage *agent.UsageEvent
    // F-49: cumulative count of completed context compactions on
    // this AgentSession. 0 when never compacted. Persists across
    // daemon restarts; cleared only by /new. Sourced from
    // AgentSession.CompactionCount at the same instant as
    // CumulativeUsage so Line 1 (🗜 N) and Line 2 (↓ ↻ ↑ total)
    // tell a coherent story.
    //
    // F-49 SUPERSEDED (2026-08-08): CompactionCount field deleted
    // across the runtime. Footer no longer renders the 🗜 N
    // segment. See F-49 OBSOLETE notice at the top of this file.
    // CompactionCount int    ← REMOVED
}
```

### 1.4 AgentSession 元数据（runtime 自管）

```go
// internal/chatsession/agentsession.go
type AgentSession struct {
    // ... 既有字段 ...
    Agent         string  // 已有，immutable
    
    // NEW: captured from EventAgentConnected on first observation.
    // Mutex-guarded because SetModel races with concurrent
    // reads (footer rendering). Empty before EventAgentConnected lands.
    modelMu       sync.RWMutex
    Model         string
    
    // NEW: per-AgentSession cumulative token / cost totals.
    // Persists across daemon restarts; cleared only by /new.
    // F-49: also guards compactionCount (RecordCompaction modifies
    // both atomically — see F-49 §1.4).
    //
    // F-49 SUPERSEDED (2026-08-08): compactionCount field
    // deleted. The cumulativeUsageMu now guards only cumulativeUsage.
    cumulativeUsageMu sync.RWMutex
    cumulativeUsage   UsageInfo
    // compactionCount int  ← REMOVED
    cumulativeDirty   bool
}
```

**API（线程安全）**：
```go
// Model
func (as *AgentSession) SetModel(m string)   // idempotent: 已有非空值不覆盖
func (as *AgentSession) Model() string

// Usage
func (as *AgentSession) AccumulateUsage(u *agent.UsageEvent)  // 加锁累加，dirty=true
func (as *AgentSession) ResetCumulative()                      // 清零 + dirty=true（仅 /new）
func (as *AgentSession) CumulativeUsage() UsageInfo            // RLock 快照
// F-49 SUPERSEDED: RecordCompaction / CompactionCount methods removed
func (as *AgentSession) PersistIfDirty(persist func(*registry.AgentSessionEntry) error) error
```

### 1.5 Wire 形态（F-45 后）

`OutboundMessage` 加 1 个 typed field：

```go
// StatusBar carries the runtime-stamped AgentSession
// snapshot for footer rendering. Stamped ONLY on OutReply /
// OutResult / OutTaskCreate / OutTaskUpdate. nil on every other
// kind (thread-only, lifecycle, init/usage payloads themselves).
//
// Bridges never populate this field; runtime's newEventHandler
// closure is the single owner of "what footer should this card
// render?". See docs/channel/feishu-rendering.md §1.3.
StatusBar *StatusBar
```

**不变式**：
- 1 个字段，不是 3 个（§0.3 论述）
- bridges 不动（仍是 EventAgentConnected / EventUsage 事件）
- runtime 唯一 owner
- Channel 读 `msg.StatusBar`，nil 时跳过 footer

### 1.6 Footer 渲染规则（formatStatusBar）

**F-52 重构 (2026-08)**：F-45 原本把 `in / out / cache / total / $cost` 拆成多段（`↓ in · ↻ cached · ↑ out · Total · $cost`）。F-52 统一为更紧凑的「`💰:「 in / out · X% · $cost 」`」格式，理由：
- "in" 按 https://yb.tencent.com/s/3G6HphjOxM70 的口径合并三个 input-side 字段（`InputTokens + CacheCreationInputTokens + CacheReadInputTokens`），避免用户在 IM 里还要心算 cache_creation + cache_read。
- "X%" 是 per-turn context-window 使用率（`used / contextWindow * 100`，`contextWindow` 是 bridge-local 变量 — Claude Code 来自 `modelUsage[<model>].contextWindow`,Pi 来自 `get_state.data.model.contextWindow`,详见 [`F-54`](./../bridge/pi.md)），让用户一眼看到距离 ceiling 还剩多少。
- "$cost" 直接透传 API 报的 `total_cost_usd`，客户端不计算（没有 rate table / 没有 per-model pricing）。

**新格式 (Format D, 「」 enclosed)**：

```
🤖: claude · opus-4-5
💰:「 20.5k / 1.5k · 10.5% · $0.087 」
📁: code/nightme · ⎇ main · ↑ 3 · ? 2 · ⇡ 2
```

**Line 2 segments**：

| 段 | 来源 | Omit 规则 | 渲染 |
|---|---|---|---|
| `in / out` | `in = InputTokens + CacheCreationInputTokens + CacheReadInputTokens`；`out = OutputTokens`。F-55.1 进一步把 `in` 拆 `new / cache / out`： | `in == 0 && out == 0` 时整段省略（无 usage） | F-55.1 render:`new / cache / out`,纯数字无 label；每段按 `> 0` 独立 omit。`cache == 0` 时退回 `new / out` 布局。`new = InputTokens + CacheCreationInputTokens`,`cache = CacheReadInputTokens`。 |

| `X% (window)` | `StatusBar.ContextWindowPct` + `StatusBar.Usage.ContextWindow`(F-55 透传:Claude Code `modelUsage[<model>].contextWindow`,Pi `get_state.data.model.contextWindow`) | `ContextWindowPct == 0` 时整段省略(`window == 0 && pct == 0` 也走 omit 路径) | `fmt.Sprintf("%.1f%% (%s)", pct, abbrevWindow(window))` — 一位小数;`99.6%` 不能四舍五入到 `100%`;`pct > 100%` **不 clamp 不告警**,让用户看到分母自行判断(`101.6% (200k)` 即是 MiniMax 兼容端把 1M 模型错报成 200K 的诊断信号) |
| `$cost` | `agent.UsageEvent.CostUSD`（F-52 透传 API 报的 `total_cost_usd`） | `== 0` 时省略（API 没报） | `fmt.Sprintf("$%.3f", cost)` — 三位小数，与 F-45 原约定一致 |

段之间用 ` · ` 分隔；`「」` 括号只在至少一个段非空时才包裹整行。Line 1 / Line 3 的 omit 规则、emoji 选择（🤖 / 🗜 / 📁）均不变，但 Line 1 / Line 3 的 emoji 头部加了 `:` 后缀（`🤖:` / `📁:`），与 Line 2 的 `💰:「」` 共享 category-prefix 形态 — 见 §1.9（F-56 follow-up）。

**Why F-52 改这三件事**：
1. **in = uncached + cache_creation + cache_read**：Tencent YB 文档 + Claude Code `/cost` 统计口径一致。之前的 `↓ in · ↻ cached` 拆法让用户得自己加两个数才知道"in 总共多少"，违反 footer 一次成型的目的。
2. **加 `X%` 段**：F-52 引入的 `ContextWindowPct`（Doc 1 公式 = `used / contextWindow * 100`，`contextWindow` 是 bridge-local 变量，见 [`F-54`](./../bridge/pi.md) §1.2）是 chat session 用户最关心的"距离 ceiling 还剩多少"指标，独立成段比塞进 `in / out` 自然。
3. **`$cost` 客户端不计算**：Anthropic API 的 `total_cost_usd` 已经把不同模型的差异化定价算好了，客户端维护 rate table 既过时又错。直接透传是唯一正解。

**实测样例 (F-52 后)**：

```
🤖: claude · opus-4-5
💰:「 20.5k / 1.5k · $0.087 」                                  # 无 ContextWindow 报回
🤖: claude · opus-4-5
💰:「 20.0k / 1.0k · 10.5% 」                                   # 有 X%，无 cost
🤖: claude · opus-4-5
💰:「 1.4M / 18.0k · 99.6% · $1.234 」                          # 大 turn，接近 ceiling
🤖: claude · opus-4-5
💰:「 $1.245 」                                                 # 只有 cost（极少见）
🤖: claude · opus-4-5
💰:「 100.0% 」                                                 # 满 context
```

**`abbrevTokens`**（未变）：

```go
// abbrevTokens: <1000 raw, 1000-999999 → "%.1fk", >=1M → "%.1fM"
func abbrevTokens(n int) string {
    switch {
    case n >= 1_000_000:
        return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
    case n >= 1_000:
        return fmt.Sprintf("%.1fk", float64(n)/1_000)
    default:
        return fmt.Sprintf("%d", n)
    }
}
```

**F-45 原 Format C 样例（保留以便 review 对照）**：

```
↓ 234 · ↻ 5.6k cached · ↑ 89 · Total 5.9k                    # 小 turn，无 agent/model
claude · opus-4-5 · ↓ 12.3k · ↻ 8.2k cached · ↑ 1.5k · Total 22.0k · $0.087   # 标准
claude · opus-4-5 · ↓ 156k · ↻ 1.2M cached · ↑ 18k · Total 1.37M · $1.245    # 大 turn
claude · ↓ 12.3k · ↑ 1.5k · Total 13.8k                                       # 无 model 无 cost 无 cache
claude · opus-4-5 · ↓ 12.3k · ↻ 8.2k cached · ↑ 1.5k · Total 22.0k          # 无 cost
```

### 1.7 Git Branch Tracking (F-48 follow-up)

**Why**：用户在 IM 里看不到当前的 workspace / branch / dirty 状态 — 每次要确认"我正在哪个 repo"都得跳到 terminal。Footer 加一行 git tracking 让 Feishu 卡片本身就是 ground truth：workspace 路径 + branch + 未提交 + 未跟踪 + 未推送。

**Format**（footer 第 3 行）：

```
📁: code/nightme · ⎇ main · ↑ 3 · ? 2 · ⇡ 2
```

| 段 | 来源 | Omit 规则 |
|---|---|---|
| 📁 `<workspace>` | `StatusBar.Workspace` = `s.Cwd` | 整段在 Workspace=="" 或 `GitStatus==nil` 时省略（review fix：non-git workspace 不显示误导性的 "⎇ ?"） |
| ⎇ `<branch>` | `GitStatus.Branch` | 永远显示（当行渲染时）；`Branch==""`（detached HEAD 在 git repo 内）→ 写 `?` |
| ↑ `<n>` | `GitStatus.Uncommitted` | `n==0` 省略 |
| ? `<n>` | `GitStatus.Untracked` | `n==0` 省略 |
| ⇡ `<n>` | `GitStatus.AheadOfRemote` | `HasUpstream==false \|\| n==0` 省略 |

**Workspace 显示规则**（简化版，Devin 拍板 2026-08-06）：
- **不加任何前缀**（既不 `~` 也不 `/`）—— 路径是什么就显示什么。理由：`~` 在 workspace 不在 HOME 下时会误导（不同 operator / 容器化 session / 非标准 HOME 布局）
- ≤ 2 个目录组件 → 完整显示：`/home/devin` → `home/devin`、`/tmp/foo` → `tmp/foo`、`/home/devin/code` → `devin/code`
- > 2 个目录组件 → 只显示最后两个：`/home/devin/code/nightme` → `code/nightme`、`/home/devin/code/nightme/internal` → `nightme/internal`、`/tmp/a/b/c` → `b/c`

**Arrow 选型**（与 F-45 约定一致）：
- `↑` / `⇡` / `?` — ASCII / Unicode 符号（非 emoji 字体），middle dot ` · ` 分隔
- `⎇` — Unicode Alternative Key Symbol (U+2387)，代表 branch
- `📁:` — folder emoji + colon，仅作 category header（与 F-45 line 1 🤖: / line 2 💰:「」 风格一致 — F-56 统一了三条 footer 行的 category-prefix 形态）

**Stamping 规则**：
- 在 `cmd/nightme/run.go::newEventHandler` 的 4 个 main-chat kind 上 stamp
- 每次 stamp 都跑 `gtw.CollectStatus(s.Cwd, gtw.ExecGitRunner{})` —— **无缓存**，footer 永远反映当前 worktree
- Git 调用的 **3 秒 deadline**（review fix）：stalled NFS / broken .git/index 不能阻塞消息路径；超时返回 (nil, nil)，footer 静默省略 git 段
- `Workspace` = `s.Cwd`（immutable 字段，无锁读）
- `GitStatus` = parse 结果；非 git repo / git 失败 / git 超时 → `nil`（整段省略）
- stamp condition 扩展：`hasGit := gitSnap != nil && s.Cwd != ""`；其他 usage/model 条件不变

**Wire 形态**（`gateway.StatusBar`）：

```go
type StatusBar struct {
    Agent           string
    Model           string
    Usage *agent.UsageEvent
    Workspace       string                  // NEW (F-48)
    GitStatus       *gtw.GitStatusSnapshot  // NEW (F-48)
    CompactionCount int                     // NEW (F-49: 🗜 N 计数 — SUPERSEDED 2026-08-08)
    SessionID       string                  // NEW (F-56: agent 自身 session id)
}

type GitStatusSnapshot struct {
    Branch        string  // empty when detached HEAD / not-a-repo
    Uncommitted   int     // M/A/D/R/C + 冲突 (UU/AA/DD/...)
    Untracked     int     // ??
    AheadOfRemote int     // 0 when no upstream
    HasUpstream   bool    // false for detached HEAD / new branch
}
```

**Render 路径**（`internal/statusbar/statusbar.go::StatusBarLines`,原 `internal/channel/feishu/usage_footer.go::formatStatusBarLines`,2026-08-22 plan-D 抽到共享包）：
- 现有 line 1 / line 2 不变
- 新增 line 3：`formatGitLine(ctx)` 返回非空时 append
- `formatGitLine` 内部调 `formatWorkspacePath`（无 HOME 处理、≤ 2 组件完整、> 2 截尾）

**测试覆盖**：
- `internal/gtw/git_status_test.go` (NEW) — 12 case：clean / dirty / detached HEAD (3 sub) / no upstream / ahead+behind / conflicts / ignored / empty output / not-a-repo
- `internal/channel/feishu/usage_footer_test.go` — 新增 `TestFormatWorkspacePath`（17 case）+ `TestFormatGitLine_*`（8 case）+ `TestFormatSessionFooterLines_WithGitLine` / `_GitOnly`

**不变式**：
- `formatStatusBarLines` 已存在测试全部通过（line 1/2 行为不变）
- `OutboundMessage.StatusBar` wire 兼容性保持：Channel 不读 `Workspace` / `GitStatus` 时零影响
- 不在 Channel 调 git（保持 F-08 "Channel is dumb" 边界）—— git CLI 只在 runtime stamp 时跑
- 无生产代码注入：测试用真实 mock git output + 直接构造 `StatusBar` 输入，不引入 test-only 变量

**F-48 PR scope**：
- `internal/gtw/git_status.go` (NEW)
- `internal/gtw/git_status_test.go` (NEW)
- `internal/gateway/messages.go` — `StatusBar` 加 2 个字段
- `cmd/nightme/run.go::newEventHandler` — stamp 时调用 `gtw.CollectStatus`
- `internal/channel/feishu/usage_footer.go` — line 3 渲染 + `formatWorkspacePath`
- `internal/channel/feishu/usage_footer_test.go` — 扩展测试

### 1.8 Compaction Counter (F-49 follow-up)

**Why**：F-45 的 Line 2 token 数字是 cumulative across the entire AgentSession。当 agent 执行 context compaction（截断/摘要化输入）后，cumulative 仍持续累加，导致用户看到的 total tokens 远超上下文窗口上限，**无法判断**是"真的用了那么多"还是"已压缩多次但 cumulative 不反映"。用户在 IM 视角下看到的是 mystery number。

F-49 给 Line 1 加 `· 🗜 N` 段（compaction 计数），同时把 Line 2 的 token 部分改成"since-last-compaction 归零"语义，而 `$cost` 保留为 lifetime spend——两个目的清晰分离。

**完整设计**：见 [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md)。本节只列与 F-45 footer 的交互点。

#### 1.8.1 Footer 渲染差异

**改前**（典型长 session，已 compact 3 次）：

```
🤖: claude · opus-4-5
� ↓ 156k · ↻ 1.2M cached · ↑ 18k · Total 1.37M · $1.245
📁: code/nightme · ⎇ main · ↑ 3 · ? 2 · ⇡ 2
```

**改后**（同 session，加 🗜 段 + token 归零）：

```
🤖: claude · opus-4-5 · 🗜 3
💰 ↓ 5k · ↻ 2k · ↑ 0.8k · 7.8k · $1.245
📁: code/nightme · ⎇ main · ↑ 3 · ? 2 · ⇡ 2
```

**关键变化**：
- **Line 1 末尾**追加 `· 🗜 N`（仅 N>0）
- **Line 2 token 部分**语义反转：从"lifetime"变成"since-last-compaction"——压缩后归零，从下一次 EventUsage 重新累加
- **Line 2 `$cost`** 保持累加：lifetime spend 跨压缩单调累加

**用户视角解读**：
- `🗜 3` + `↓ 5k · Total 7.8k` → "当前上下文用了 7.8k（压缩后），3 次压缩前实际用过 1.37M tokens"
- `$1.245` → "这个 Session 总共花了 $1.245，无论压缩几次都不会变"

#### 1.8.2 两个 token 目的 → 两个独立 metric

| 用户目的 | Footer 段 | 字段 | 压缩时行为 |
|---|---|---|---|
| **总耗费**（lifetime spend） | `$X.XXX` | `CostUSD` | **保留**，单调累加 |
| **当前 Session 上下文用量** | `↓ X · ↻ X · ↑ X · Total X` | 4 个 token 字段 | **归零**，重新累加 |

**为什么这样切**：
- `CostUSD` 是货币量——花了不会退，跨压缩累加
- 4 个 token 字段是**输入窗口**快照——压缩后输入窗口被截断，下一个 turn 的 input 自然变小；归零后从下一次 EventUsage 重新累加，**无需特殊处理**

#### 1.8.3 Bridge 抽象（与 F-45 的解耦点）

F-45 假设 bridge 只产生 `EventAgentConnected` / `EventUsage` / `EventResult` / `EventText` / `EventToolStart` / `EventToolEnd`。**F-49 新增一个 consumer**：`EventCompaction`。但 bridge 层有协议差异：

- **Pi**：`compaction_start` + `compaction_end` 两条
- **Claude Code**：result subtype `compact` / `compaction` 一条

**抽象归抽象 / 具体归具体**（SPEC §1.4）：bridge 自己消化协议差异，runtime 一视同仁。

```
                  ┌─────────────────────────────────────────────┐
  Pi 协议         │  compaction_start → [suppressed]            │
  compaction_start│  compaction_end   → EventCompaction × 1    │
  compaction_end  │                                             │
                  └─────────────────────────────────────────────┘
                                ↓
                  ┌─────────────────────────────────────────────┐
  Claude 协议     │  result.subtype == "compact" /              │
  result.subtype  │  "compaction" → EventCompaction × 1         │
                  └─────────────────────────────────────────────┘
                                ↓
                  ┌─────────────────────────────────────────────┐
  runtime handler │  case EventCompaction:                       │
  (协议无关)      │    as.RecordCompaction()                     │
                  │    // 不判断 Subtype，不产生 Outbound        │
                  └─────────────────────────────────────────────┘
```

详见 [`F-49 §1.3`](./../channel/feishu-rendering.md) 与 [`F-32 §2.3`](./../bridge/pi.md) 改动说明。

#### 1.8.4 F-45 routing 表更新（删除 OutCompaction）

F-45 §1.2 的 OutboundKind → Footer 路由表中 `OutCompaction` 一行删除：

```diff
  | OutboundKind | 表面 | Footer |
  |---|---|---|
  | `OutReply` | **ReplyInThreadAndChat**（每 chunk） | ✅ footer 在文末 |
  | `OutResult` | ReplyInThreadAndChat | ✅ footer 在文末 |
  | `OutTaskCreate` / `OutTaskUpdate` | **Rolling-log receipt card**（Tasks） | ✅ footer 在 checklist 末尾 |
  | `OutChoice` (permission) | Top-level Create | ❌ 不带 footer（短状态消息） |
  | `OutCommandReply` | Top-level Create | ❌ 不带 footer |
- | `OutThinking` / `OutToolStart` / `OutToolEnd` | `ReplyInThread` | ❌ 不带 footer（thread 视觉独立） |
- | `OutCompaction` | `ReplyInBoth` | ❌ 不带 footer（短暂 marker） |
  | `OutMessageState` | AddReaction | ❌ 不带 footer |
  | `OutInit` / `OutUsage` | Silent drop（F-44 不变） | — |
```

**`OutCompaction` kind 整体删除**（不是"保留但 runtime 不发"）——runtime handler 不再产生 OutboundMessage；channel adapter 不再有 case 处理。理由见 [`F-49 §1.9`](./../channel/feishu-rendering.md)。

#### 1.8.5 StatusBar 扩展

```go
type StatusBar struct {
    Agent           string
    Model           string
    Usage *agent.UsageEvent
    Workspace       string                  // F-48
    GitStatus       *gtw.GitStatusSnapshot  // F-48
    CompactionCount int                     // F-49: 🗜 N 计数
}
```

#### 1.8.6 与 F-45 §1.5 stamping 规则的交互

F-45 §1.5 在 4 个 main-chat Kind 上 stamp StatusBar。F-49 不改变这一规则——只在 stamp 时多填一个 `CompactionCount` 字段。Stamp condition 扩展（[`F-49 §1.8`](./../channel/feishu-rendering.md)）：

```go
// 既有 condition（来自 F-45 §2.5 改动 C）
if snap.InputTokens != 0 || snap.OutputTokens != 0 ||
    snap.CacheCreationInputTokens != 0 ||
    snap.CacheReadInputTokens != 0 || snap.CostUSD != 0 ||
    s.Model() != "" || hasGit {
    out.StatusBar = &gateway.StatusBar{...}
}

// F-49 扩展：compactionCount > 0 也触发 stamp
if snap.InputTokens != 0 || ... || hasGit ||
    s.CompactionCount() > 0 {
    out.StatusBar = &gateway.StatusBar{
        // ...
        CompactionCount: s.CompactionCount(),
    }
}
```

实际上 `CompactionCount() > 0` 几乎不会单独触发 stamp（compaction 必然发生在至少 1 个 turn 之后，前几个 OR 条件已覆盖），但保持对称——理论上 `/new` 后立刻 compaction 也能显示 🗜 段。

#### 1.8.7 F-49 与 F-45 §1.8 (State 流转) 的交互

State 流转图（见下 §1.9）加一条 compaction 分支：

```
  SetModel(ev.Connected.Model)     ← EventAgentConnected 触发；idempotent
  AccumulateUsage(ev.Usage)   ← EventUsage 触发（每个 turn 一次）
  ...
+ RecordCompaction()          ← EventCompaction 触发；count++ + 4 token 字段归零
+                              CostUSD 保留；后续 EventUsage 从零重新累加 token
  ...
  /new → ResetCumulative      ← 用户主动重置（compactionCount + usage 全清零）
```

#### 1.8.8 F-49 不在 F-45 PR scope

F-49 是独立 PR（详见 §7 实施计划），不在 F-45 当初落地范围内。F-45 的 footer helper `formatStatusBarLines`（现 `statusbar.StatusBarLines`）在 F-49 PR 里加 `🗜 N` 段；其余文件（AgentSession / StatusBar / newEventHandler / bridges）都是 F-49 新增改动，与 F-45 已落地的代码解耦。

### 1.9 State 流转

```
AgentSession 生命周期:
  [spawn]
    ↓
  SetModel(ev.Connected.Model)     ← EventAgentConnected 触发；idempotent
  AccumulateUsage(ev.Usage)   ← EventUsage 触发（每个 turn 一次）
    ↓
  ... 持续累积 ...
    ↓
  PersistIfDirty              ← EventDone 触发；落盘 agent_sessions.json
    ↓
  /new → ResetCumulative      ← 用户主动重置上下文（唯一清零点）
    ↓
  PersistAgentSession         ← 立即落盘
```

**与现有字段的关系**：
- `ResumeID`：EventAgentConnected 时捕获，**永不重置**（除非 `/new` 通过 bridge `New()` 让 agent 重发 EventAgentConnected）
- `Model`：EventAgentConnected 时捕获，**永不重置**（同 ResumeID 语义）
- `CumulativeUsage`：EventUsage 时累加，**`/new` 重置**

### 1.10 SessionID + Category-Prefix 形态统一 (F-56 follow-up)

**Why**：三件事打包：(a) `AgentSession.SessionID`（Claude Code 的 `system/init.session_id`、ACP 合成 uuid 等）已经有 runtime 端的 RLock accessor，但没进 `gateway.StatusBar`，footer 拿不到——用户看不到"这是哪个 agent session"；(b) 三条 footer 行的 category-prefix 形态不一致（Line 2 是 `💰:「」`，Line 1 是 `🤖`，Line 3 是 `📁 `），视觉上读起来像两条规则。

**改动**：

**(1) `gateway.StatusBar` 加 `SessionID string` 字段**

```go
type StatusBar struct {
    Agent     string
    Model     string
    SessionID string  // NEW (F-56)
    Workspace string
    GitStatus *gtw.GitStatusSnapshot
    Usage     *agent.UsageInfo
}
```

字段来源：`cmd/nightme/run.go::stampFromAS` 在 stamp 时调 `s.SessionID()`（RLock）填入；stamp condition 增加 `s.SessionID() != ""` 单独触发 footer 渲染（与 `Agent` / `Model` / `hasGit` / `Usage` 并列）。空值时 footer 跳过该 segment——保持既有"each segment omitted independently"约定。

**(2) Footer Line 1 渲染追加 `· <sessionid>` 段**

```
🤖: claude · opus-4-5 · abc123-uuid-here
💰:「 33.3k / 876k / 3.9k · 456.6% (200k) · $0.733 」
📁: code/nightme · ⎇ main · ↑ 3 · ? 2 · ⇡ 2
```

Separator 沿用 ` · ` middle-dot（与 Line 1 已有 `Agent · Model`、Line 2 已有 `new / cache / out` 同一族）；不引入新符号。

Leading-separator caveat：`SessionID != "" && Agent == "" && Model == ""` 时渲染成 `🤖: · <sid>`（前置 `·`）。在生产路径上 `stampFromAS` 的 materialize 条件保证至少有 1 个字段非空，但单 SessionID 触发 footer 时仍会出现该形态。判定为可接受——文档在 `statusbar.StatusBarLines` 注释（原 `formatStatusBarLines`）+ `statusbar_test.go::TestStatusBarLines_SessionIDOnly`（原 `usage_footer_test.go::TestFormatSessionFooterLines_SessionIDOnly`,2026-08-22 plan-D 迁到共享包）锁定行为，避免后续 PR "修 leading dot" 时无意回退。

**(3) Line 1 / Line 3 emoji 加 `:` 后缀**

`🤖` → `🤖:`、`📁 ` → `📁: `，与 Line 2 既有 `💰:「」` 共享 `emoji: <content>` 的 category-prefix 形态。两条 line 上的「Segment 头部」对齐后，整张卡片的 `emoji: ... · ... · ...` 节奏统一。

```
🤖: <agent> · <model> · <sessionid>
💰:「 <stats> 」
📁: <workspace> · <branch> · <git-counts>
```

`💰:` 自身带 `「」` enclosure，因为它承载多段（in/cache/out + X% + $cost），需要括号表达"多段合集"的语义；Line 1 / Line 3 是平铺 segment 链，不需要 enclosure——保留各自 segment 链 + ` · ` separator 的逻辑，不强行套「」。

**Wire 形态**：

```go
type StatusBar struct {
    Agent     string
    Model     string
    SessionID string                  // NEW (F-56)
    Workspace string                  // F-48
    GitStatus *gtw.GitStatusSnapshot  // F-48
    Usage     *agent.UsageInfo        // F-52 (was *UsageEvent)
}
```

**Render 路径**（`internal/statusbar/statusbar.go::StatusBarLines`,原 `internal/channel/feishu/usage_footer.go::formatStatusBarLines`,2026-08-22 plan-D 抽到共享包）：
- Line 1 idParts：`["🤖:", ctx.Agent, "·", ctx.Model, "·", ctx.SessionID]`（Agent / Model / SessionID 各自独立 omit）
- Line 3 formatGitLine：`parts := []string{"📁: " + ws, ...}`（emoji 头部加 `:`）
- Line 2 不变

**Stamp condition**（`cmd/nightme/run.go::stampFromAS`）：

```go
if s.Agent != "" || s.Model() != "" || s.SessionID() != "" || hasGit ||
    out.Usage != nil {
    out.StatusBar = &gateway.StatusBar{
        Agent:     s.Agent,
        Model:     s.Model(),
        SessionID: s.SessionID(),
        Workspace: s.Cwd,
        GitStatus: gitSnap,
        Usage:     out.Usage,
    }
}
```

**测试覆盖**（`internal/channel/feishu/usage_footer_test.go`）：
- 既有 27 case 全部更新 `🤖` → `🤖:`、`📁 ` → `📁: `（byte-level snapshot 测试需要同步）
- 新增 `TestFormatSessionFooterLines_IdentityWithSessionID`：渲染 `🤖: claude · opus-4-5 · <sid>`
- 新增 `TestFormatSessionFooterLines_SessionIDOnly`：锁定 leading-separator caveat（`🤖: · <sid>`）

**Bridge 兼容性**：
- `claudecode`：`EventAgentReady.SessionID`（来自 `system/init.session_id`）—— 已实现
- `pi`：同上
- `acp`：ACP **协议本身**返回 sessionId —— `internal/bridge/acp/agent.go::setSessionID` 解码 session/new 响应的 `sessionId` / `session_id` 字段；runtime 端 *synthesizes 的是 `EventAgentReady` envelope*（`emitConnected`），不是 sessionID 值。Footer 显示 ACP 真实 sessionId，对 debug 有帮助（识别"这是 ACP 那条 session"），不算误导
- `codex`：`EventAgentReady.SessionID` 来自 `internal/bridge/codex/session.go::ensureThread` 的 `thread.id`（app-server JSON-RPC thread.start 响应）。同上，真实 id，不是合成
- `pty`：pty 没有 init 事件，SessionID 为空，footer 跳过该 segment（既有 `each segment omitted independently` 规则自然处理）

**F-56 PR scope**：
- `internal/gateway/messages.go` — `StatusBar` 加 1 个字段
- `cmd/nightme/run.go::stampFromAS` — 填充 + materialize条件
- `internal/channel/feishu/usage_footer.go` — Line 1 加 sessionid 段 + emoji `:` 后缀 + Line 3 emoji `:` 后缀
- `internal/channel/feishu/usage_footer_test.go` — 同步既有 fixture + 新增 2 个 case
- `docs/channel/feishu-rendering.md` — 本节

---

## 2. 文件 & 接口

### 2.1 `internal/agent/agent.go`

**改动 A**：`UsageInfo` 从 `internal/gateway/messages.go` 搬到 `internal/agent/agent.go`（紧挨 `UsageEvent`）。

```go
// 原因：chatsession 包要 import UsageInfo，但不应反向 import gateway。
//       agent 是底层包，UsageInfo 与 UsageEvent 同语义层级，放一起。
type UsageInfo struct {
    InputTokens              int
    OutputTokens             int
    CacheCreationInputTokens int
    CacheReadInputTokens     int
    CostUSD                  float64
}
```

**改动 B**：修 `UsageInfo.InputTokens` 注释——原注释 "the total input tokens consumed across the turn (prompt + cache reads + tool input)" 是误导（实现只搬运裸 `input_tokens`，不包含 cache reads），新注释：

```go
// InputTokens is the non-cached input token count from the
// last LLM call (Anthropic API: input_tokens field).
// Cache hits are NOT included — see CacheReadInputTokens.
InputTokens int
```

### 2.2 `internal/gateway/messages.go`

**改动 A**：定义 `StatusBar` typed struct（见 §1.3）。

**改动 B**：给 `OutboundMessage` 加 `StatusBar *StatusBar` 字段（见 §1.5）。

**改动 C**：保留 `UsageInfo` 的 type alias 兼容：

```go
// UsageInfo is the cumulative form of UsageEvent — used on
// AgentSession wrapper and OutboundMessage.StatusBar.
// Re-exported as a type alias for backward compatibility with
// existing gateway code (translate.go / OutUsage payload).
type UsageInfo = agent.UsageInfo
```

### 2.3 `internal/registry/agent_session_entry.go`

**改动**：加 2 个字段：

```go
type AgentSessionEntry struct {
    // ... 既有字段 ...
    
    // F-45: cumulative token / cost stats. Persists across
    // daemon restarts; cleared only by /new (see F-45 §1.7).
    // nil on legacy entries (zero-value behavior on read).
    CumulativeUsage *UsageInfo `json:"cumulativeUsage,omitempty"`
    
    // F-45: model captured on first EventAgentConnected. Persists for
    // the lifetime of the AgentSession IDENTITY (until /new
    // re-emits EventAgentConnected with a new model — rare).
    Model string `json:"model,omitempty"`
}
```

**JSON 兼容**：Go 默认 JSON unmarshal 容忍缺失字段——`agent_sessions.json` 无 `cumulativeUsage` / `model` 时安全 fallback 到零值。

### 2.4 `internal/chatsession/agentsession.go`

**改动 A**：struct 加 3 个字段（`modelMu` / `Model` / `cumulativeUsageMu` / `cumulativeUsage` / `cumulativeDirty`）。

**改动 B**：加 6 个方法（`SetModel` / `Model` / `AccumulateUsage` / `ResetCumulative` / `CumulativeUsage` / `PersistIfDirty`）。

**改动 C**：`FromAgentSessionEntry` 恢复时：

```go
if e.CumulativeUsage != nil {
    as.cumulativeUsage = *e.CumulativeUsage  // 拷贝，不是引用
}
if e.Model != "" {
    as.Model = e.Model  // 直接写，无需锁（构造时无并发读）
}
```

**改动 D**：`Entry()` 序列化时：

```go
as.cumulativeUsageMu.RLock()
cum := as.cumulativeUsage
as.cumulativeUsageMu.RUnlock()
return &registry.AgentSessionEntry{
    // ... 既有字段 ...
    CumulativeUsage: &cum,  // 永远非 nil：即使全零也带 — 区分"从未跑过"vs"跑了但=0"无意义，统一写
    Model:           as.Model(),
}
```

### 2.5 `cmd/nightme/run.go::newEventHandler`

**改动 A**：`EventAgentConnected` 处理块里加 Model 捕获：

```go
// 既有：
if ev.Kind == agent.EventAgentConnected && ev.Connected != nil && ev.Connected.SessionID != "" {
    s.SetResumeID(ev.Connected.SessionID)
    if mgr != nil { _ = mgr.PersistAgentSession(s) }
}

// NEW:
if ev.Kind == agent.EventAgentConnected && ev.Connected != nil && ev.Connected.Model != "" {
    s.SetModel(ev.Connected.Model)
}
```

**改动 B**：`Translate` 前加 usage 累加：

```go
// NEW: 累计 per-turn usage。Translate 前跑，保证 stamp 时已含本 turn。
if ev.Kind == agent.EventUsage && ev.Usage != nil {
    s.AccumulateUsage(ev.Usage)
}
```

**改动 C**：`out.ReplyTo = userMsgID` 之后 stamp StatusBar：

```go
// NEW: 在 4 个 main-chat Kind 上 stamp StatusBar 快照
switch out.Kind {
case gateway.OutReply, gateway.OutResult,
    gateway.OutTaskCreate, gateway.OutTaskUpdate:
    snap := s.CumulativeUsage()
    if snap.InputTokens != 0 || snap.OutputTokens != 0 ||
        snap.CacheReadInputTokens != 0 || snap.CostUSD != 0 ||
        s.Model() != "" {
        out.StatusBar = &gateway.StatusBar{
            Agent:           s.Agent,        // immutable string, 无锁
            Model:           s.Model(),
            CumulativeUsage: snap,
        }
    }
}
```

**改动 D**：EventDone 处理路径加持久化：

```go
// 在 emitMessageStateForCurrentTurn 之后：
if ev.Kind == agent.EventDone {
    if err := s.PersistIfDirty(func(e *registry.AgentSessionEntry) error {
        if mgr == nil { return nil }
        return mgr.PersistAgentSession(s)
    }); err != nil && logger != nil {
        logger.Warn("persist agent session (usage) failed",
            "agent_session_id", s.ID, "err", err)
    }
}
```

**改动 E**：（已废弃）早期的 OutResult 缓冲机制。后续 bridge 重构把 Usage 合并到 ResultEvent 之后不再需要 —— 见 §2.5 changelog 末尾的 "single-event design" 注释。

### 2.6 `internal/command/newcmd/cmd.go::Handle`（原 `internal/gateway/handlers_chatsession.go::handleNew`）

**改动**：在调 `agentSession.New(ctx)` 之后立刻持久化。`ResetCumulative` 在 F-102 重构里已删除（cumulative-usage 概念被替换为 `AgentResultEvent`-内 字段，accumulator 重构移走了清零 hook），所以这一步现在只做持久化：

```go
// 既有：
if err := as.New(ctx); err != nil { ... }

// NEW（F-45 原文追加 ResetCumulative + PersistAgentSession；F-102 后只剩 PersistAgentSession）
_ = mgr.PersistAgentSession(as)
```

**scope**：
- `/new <agent>`：只重建单个 AgentSession
- `/new`：重建 selectedCwd 下所有 AgentSession（pool 内）

### 2.7 `internal/channel/feishu/usage_footer.go` (NEW)

**新文件**：

```go
package feishu

// formatStatusBar 渲染 StatusBar 为单行 markdown。
// 返回 "" 表示无需 footer（nil 或全零）。
//
// 见 docs/channel/feishu-rendering.md §1.6 完整规则。
func formatStatusBar(ctx *gateway.StatusBar) string

// abbrevTokens token 数字缩写：<1000 raw，≥1k "X.Xk"，≥1M "X.XM"
func abbrevTokens(n int) string
```

### 2.8 `internal/channel/feishu/adapter.go`

**改动**：3 个 main-chat case 在发送前拼 footer。

#### Case `OutReply` → `sendReplyInThreadAndChat`

扩展 helper 接受 `footer string` 参数（或在 caller 拼好再传）：

```go
func (a *Adapter) sendReplyInThreadAndChat(
    ctx context.Context, chatID, userMsgID, text string,
) error {
    // ... 既有 sanitize / truncate / buildResultPayload 逻辑 ...
    
    // NEW: 在 text 末尾追加 footer（如果有）
    if footer := formatStatusBar(msg.StatusBar); footer != "" {
        // 用 "\n\n" 分隔 footer 与正文（lark_md 渲染时换行）
        text = text + "\n\n" + footer
    }
    
    _, err = a.sendContent(...)
    return err
}
```

**调用方改动**（`Send` case `OutReply`）：

```go
case gateway.OutReply:
    text := strings.TrimSpace(msg.Text)
    if text == "" { return nil }
    if msg.ReplyTo == "" {
        return a.sendReplyInThreadAndChat(ctx, msg.ChatID, "", text, msg.StatusBar)
    }
    // ... 既有 fold 进 receipt 路径 ...
    return a.sendReplyInThreadAndChat(ctx, msg.ChatID, msg.ReplyTo, text, msg.StatusBar)
```

#### Case `OutResult` → `sendResultAsReply`

平行改造，签名加 `ctx *StatusBar`。

#### Case `OutTaskCreate` / `OutTaskUpdate` → receipt card

`buildReceiptCard(entries, tasks)` 加第三参数 `footer string`：

```go
// buildReceiptCard 签名变更
func buildReceiptCard(tasks []agent.TaskItem, footer string) (json.RawMessage, error)

// 内部：
if footer != "" {
    elements = append(elements, map[string]any{
        "tag": "hr",
    })
    elements = append(elements, map[string]any{
        "tag":  "div",
        "text": map[string]any{
            "tag":     "lark_md",
            "content": footer,
        },
    })
}
```

**元素预算影响**：footer 加 1 个 hr + 1 个 lark_md div = 2 额外元素。当前 receipt 50 element 预算，task checklist 一般 5-15 个 element，加 footer 后 7-17，**远未撞上限**。

### 2.9 Echo channel 透传

`internal/channel/echo/echo.go::Send` —— **零改动**。`StatusBar` 字段自然落到 `recorded` slice，测试用 `c.Record()` 验证被 stamp。

---

## 3. 实施计划

按 9 个独立 commit 顺序落地，每步可单独 revert：

1. **`refactor(agent): move UsageInfo to agent package (alias in gateway)`**
   - `internal/agent/agent.go` 新增 `UsageInfo` struct
   - `internal/gateway/messages.go` 改为 `type UsageInfo = agent.UsageInfo`
   - 修 `UsageInfo.InputTokens` 注释（§2.1 改动 B）

2. **`feat(registry): AgentSessionEntry add Model + CumulativeUsage fields`**
   - `internal/registry/agent_session_entry.go` 加 2 个字段
   - 无 JSON 迁移（缺失字段容忍）

3. **`feat(chatsession): AgentSession Model + CumulativeUsage API`**
   - struct 加 3 个字段
   - 加 6 个方法（§2.4）
   - `Entry()` / `FromAgentSessionEntry` 同步

4. **`feat(gateway): StatusBar typed field on OutboundMessage`**
   - `internal/gateway/messages.go` 新增 `StatusBar` struct
   - `OutboundMessage` 加 `StatusBar *StatusBar` 字段

5. **`feat(runtime): newEventHandler accumulate + capture + stamp StatusBar + PersistIfDirty`**
   - `cmd/nightme/run.go` 4 处改动（§2.5 改动 A/B/C/D）

6. **`feat(gateway): /new handler ResetCumulative + PersistAgentSession`**
   - `internal/command/newcmd/cmd.go::Handle` 加 2 行（原 `internal/gateway/handlers_chatsession.go::handleNew`；F-102 之后 slash command 已统一迁移到 `internal/command/<name>/`）

7. **`feat(feishu): formatStatusBar helper + 3 main-chat case 渲染 footer`**
   - 新文件 `internal/channel/feishu/usage_footer.go`
   - `internal/channel/feishu/adapter.go` 改 `Send` 3 case + 改 `buildReceiptCard` 签名 + 改 `sendReplyInThreadAndChat` / `sendResultAsReply` 签名

8. **`docs(SPEC): §0.12 F-45 footer + cumulative persistence`**
   - `docs/SPEC.md` 加 §0.12 增量变更摘要

9. **`docs(feat): F-44 §6.1 cross-link + channel/feishu.md`**
   - F-44 §6.1 加一行 "实现见 F-45"
   - `docs/channel/feishu-rendering.md` §12 渲染映射表更新 + §13.22 新增 section

---

## 4. 测试

### 4.1 单元测试

| 测试 | 文件 | 覆盖 |
|------|------|------|
| `TestSetModel_Idempotent` | `internal/chatsession/agentsession_test.go` (NEW) | 第一次 SetModel 设值，第二次空值不覆盖，第二次非空值仍允许覆盖（用于 --model 切换场景） |
| `TestAccumulateUsage_Race` | `internal/chatsession/agentsession_test.go` | 100 goroutine × 1000 increments，验证最终 sum 正确 + race detector 无 warning |
| `TestResetCumulative_Clears` | `internal/chatsession/agentsession_test.go` | 累加 → ResetCumulative → CumulativeUsage() 全零 + dirty=true |
| `TestPersistIfDirty_NoOpWhenClean` | `internal/chatsession/agentsession_test.go` | dirty=false 时 PersistIfDirty 不调 persist callback |
| `TestPersistIfDirty_DirtyResets` | `internal/chatsession/agentsession_test.go` | dirty=true 时调一次 callback，dirty 立即重置（不会双重落盘） |
| `TestEntry_RoundtripPreserves` | `internal/chatsession/agentsession_test.go` | `Entry() → JSON marshal → unmarshal → FromAgentSessionEntry` 字段全相等 |
| ~~`TestHandleNew_ResetsCumulative`~~ | ~~`internal/gateway/handlers_new_test.go`~~ (OBSOLETE, **F-102 重构已删**） | F-102 之后 `ResetCumulative` / `CumulativeUsage` API 不再存在 —— usage 累积改走 `internal/agentsession/` accumulator + `AgentResultEvent.Usage`。此 case 已被 `TestHandleNew_PersistsAgentSession`（在 `internal/command/newcmd/commands_test.go`）取代，只断言 `/new` 后持久化路径 |
| `TestFormatSessionFooter_*` | `internal/channel/feishu/usage_footer_test.go` (NEW) | nil / all-zero / 仅 in / 含 cost / cache 标记 / 大数缩写 |
| `TestSend_OutReply_AppendsFooter` | `internal/channel/feishu/adapter_test.go` (EXTEND) | msg.StatusBar 非 nil 时，sendContent 收到 body 包含 footer 行 |
| `TestSend_OutResult_AppendsFooter` | 同上 | 同上 for OutResult |
| `TestBuildReceiptCard_WithFooter` | `internal/channel/feishu/receipt_test.go` (EXTEND) | footer 字符串出现在 card body 的最后 div element |
| `TestEcho_RecordsStatusBar` | `internal/channel/echo/echo_test.go` (EXTEND) | c.Record() 验证 StatusBar 字段被填充 |

### 4.2 集成测试

| 测试 | 文件 | 覆盖 |
|------|------|------|
| `TestNewEventHandler_AccumulatesAcrossTurns` | `cmd/nightme/run_test.go` (EXTEND) | mock 5 个 turn 的 EventUsage → s.CumulativeUsage() 等于 5 个 turn 之和 |
| `TestNewEventHandler_StampsOnlyMainChatKinds` | 同上 | 5 种 OutboundKind 各发一个 Event，验证 StatusBar 仅在 4 个 main-chat Kind 上非 nil |
| `TestNewEventHandler_PersistsOnEventDone` | 同上 | 模拟 EventUsage + EventDone，验证 PersistAgentSession 被调一次 |
| `TestRestart_PreservesCumulative` | `cmd/nightme/run_test.go` | spawn AgentSession → 累加 → 模拟 daemon 重启 → 新 AgentSession.CumulativeUsage() 等于上次落盘值 |

### 4.3 边界测试

| 测试 | 场景 |
|------|------|
| `TestFooter_OmitsZeroSegments` | Model="" / Cost=0 / CacheRead=0 时对应 segment 不显示 |
| `TestFooter_AllZero_ReturnsEmpty` | 全零时返回 ""，caller 不拼到 text |
| `TestStatusBar_NeverStampedOnThreadKinds` | OutThinking / OutToolStart / OutToolEnd / OutCompaction 不带 StatusBar |
| `TestStatusBar_NeverStampedOnLifecycleKinds` | OutInit / OutUsage / OutMessageState / OutChoice / OutCommandReply 不带 StatusBar |

---

## 5. Migration & 兼容性

### 5.1 JSON 兼容性

**`agent_sessions.json`**：
- 缺失 `cumulativeUsage` 字段 → Go JSON unmarshal 容忍 → 内存里 `*UsageInfo == nil` → 视为"从未跑过"，cumulative 从零开始
- 缺失 `model` 字段 → 视为空字符串 → footer 不显示 model segment
- 第一次写入新字段后，新文件包含 `cumulativeUsage` + `model`——向后兼容（读端都容忍缺失）

**`chat_sessions.json`**：无变化（cumulative 是 per-AgentSession，不是 per-ChatSession）。

### 5.2 wire 兼容性

**`OutboundMessage`**：
- 新字段 `StatusBar *StatusBar`——Channel 实现需要适配（Feishu adapter 改 3 case）
- 其他 Channel 实现（Echo / Slack / Web）零改动也能编译（只是不渲染 footer）
- 未来 Channel 想支持 footer：读 `msg.StatusBar` 即可

**bridge 协议**：零变化。bridges 仍发 `EventAgentConnected` / `EventUsage`，runtime 负责捕获并 stamp。

### 5.3 行为兼容性

- **F-44 silent drop**：`OutInit` / `OutUsage` 仍 silent drop（F-45 不改 Channel 的 silent drop 决策）
- **F-39 OutResult 独立 reply**：不变
- **F-37 thread routing**：不变
- **F-25 rolling-log UX**：task receipt 仍是 rolling-log，footer 是新增 segment
- **F-31 MessageState 抽象契约**：不变

---

## 6. 不在本 PR 范围

- **`/cost` slash command**（读 cumulative stats 主动展示）—— 后续 PR
- **per-model breakdown**（Anthropic API `modelUsage` map 展开成 multi-line footer）—— 后续 PR
- **ChatSession-level 总计**（pool 内所有 AgentSession 之和）—— 后续 PR
- **token 数据准确性改进**（bridge 在 EventText / EventToolStart 之间插 token snapshot）—— 超出 PR 范围，claudecode 当前 `result.usage` 是最后 LLM call 的 token 数
- **agent_version / provider_url** 等扩展字段——`StatusBar` 已预留扩展位置，按需加

---

## 7. 不变式总结

**F-45 加 AgentSession 元数据 + 1 个 wire field + footer 渲染，但保留**：

- **`OutboundMessage` 契约 100% typed**（§1.4 不变；新字段 typed 不破 §1.4 边界规范）
- **§1.3 ChatSession 不 import channel/feishu**（不变）
- **Channel 不 import chatsession**（不变；Channel 通过 typed `StatusBar` 字段读 metadata）
- **1 turn : 1 anchor 不变式**保留（`ReplyTo = currentTurnUserMsgID` 仍是唯一 coordination key）
- **抽象归抽象 / 具体归具体**（footer 渲染细节由 Feishu adapter 自决，Slack / Web / Echo 各自决定）
- **bridges 协议零变化**（仍发 EventAgentConnected / EventUsage，runtime 翻译）
- **OutboundKind 不增不减**（`StatusBar` 是字段，不是新 Kind）
- **OutInit / OutUsage 仍是 silent drop**（F-44 决策保留；footer 走 `StatusBar` 单独路径）
- **§1.4 抽象 / 具体 边界规范**：metadata 是 typed primitive，Channel 自决渲染目标
- **F-25 rolling-log UX**：task receipt 仍是 rolling-log，footer 是新增 lark_md div
- **F-31 MessageState 抽象契约**：不变
- **F-37 thread routing**：不变
- **F-38 task checklist 决策**：不变
- **F-39 OutResult 决策**：不变（独立 reply + footer 拼文末）
- **F-40 OutReply 命名**：不变
- **F-42 lazy receipt creation**：不变
- **F-43 `/close` graceful / `/new` ResetID**：本 PR 在 `handleNew` 加 `ResetCumulative`，与 F-43 的"clear ResumeID"语义对称
- **F-44 OutReply 拆出 + task receipt 瘦身**：不变

---

## A11. F-46: 交互卡按钮回灌 + 原地 PATCH（Interactive Decision Cards）

> **Source**: `../channel/feishu-rendering.md`
>
> **AskUserQuestion / form / 回调踩坑**（比 F-46 决策卡更晚、也更容易翻车）：见 [`feishu-cards.md`](./feishu-cards.md)。本节只记 gtw `act:` 决策卡落地。

> **Scope**:

## 1. 背景

### 1.1 现状

nightme 现在有两套并行通路驱动 gtw 决策卡：

| 通路 | 触发 | 处理函数 | 状态 |
| --- | --- | --- | --- |
| emoji reaction | 用户在 bot 消息上点 `🔄` 等 | `OnP2MessageReactionCreatedV1` → `handleReactionCreated` → 推进 inbound → `WithActionHandler` → `gtw.HandleAction` | ✅ 已通 |
| interactive card button | 用户点 bot 卡片上的 `🆕` 按钮 | `OnP2CardActionTrigger` → `handleCardAction` (stub) | ❌ stub：log + 弹 toast |

`/gtw fix` 跑出来的决策卡（branch-exists / worktree-fail 场景，见 §3.3）是纯文本 markdown（见 `internal/gtw/render.go`），用户必须打 emoji 才能继续。React Native / web 客户端上的表情输入体验不好（选 emoji 面板要找），所以要给决策卡加 button + `select_static` 让用户点一下。

### 1.2 邻近实现

- **cc-connect** ([card.go](https://github.com/cccZone/cc-connect/blob/main/platform/feishu/card.go))：Card 2.0 schema + `{"action": btn.Value, "session_key":..., "extra":...}` value 编码 + `nav:` / `act:` / `cmd:` 三类前缀
- **cc-connect** ([feishu.go::onCardAction](https://github.com/cccZone/cc-connect/blob/main/platform/feishu/feishu.go))：按前缀分发 + receipt card PATCH 替换（不是新发）
- **Hermes feishu-card** ([__init__.py](https://github.com/ai-eifying/hermes-feishu-card/blob/main/__init__.py))：Card 2.0 schema + native `table` / `column_set` 富元素（无 button）
- **Hermes lark-skill-collection** ([bettersoul](https://github.com/bettersoul/hermes-lark-skill-collection))：button + form input + card 原地 update + token cache

cc-connect 的模式最贴近 nightme 的需求：bot 决策卡要支持 button + receipt card PATCH + 派发回 action handler。

## 2. 目标

1. `/gtw fix` 决策卡用 Card 2.0 渲染，按钮 / `select_static` 让用户点一下就能继续
2. 用户点按钮 → `card.action.trigger` → 推进 inbound → 复用 `gtw.HandleAction`
3. 派发后**原地 PATCH** receipt card（按钮变 "✅ 已选择" / 选项变灰），不要发新消息
4. emoji reaction 路径保留（飞书桌面端不渲染 button 时降级用 emoji）
5. 老的 `buildInteractiveCard` 复用，button value 编码标准化

## 3. 设计

### 3.1 Button value 编码

现在 `buildInteractiveCard` 的 button value：

```json
{"request_id": "...", "option": "🆕"}
```

改成 cc-connect 风格：

```json
{"action": "act:/gtw/branch-newv2", "request_id": "..."}
```

`action` 字段是协议级语义（点完去哪里），`request_id` 是卡片关联 token。`option` 字段废弃——`option` 只对 `select_static` 组件有意义，按钮不该用 emoji 当 option text。

**Action 前缀约定**（沿用 cc-connect 三类）：

| 前缀 | 语义 | 落地 |
| --- | --- | --- |
| `nav:/xxx` | 切到新卡（不动业务） | 暂不实现，留 F-47 |
| `act:/xxx` | 执行 action，原地 PATCH | F-46 主体 |
| `cmd:/xxx` | 当用户命令派发（绕过 reaction） | F-46 不做，留 F-48 |

### 3.2 `Card` 字段扩展

```go
type Card struct {
    Title     string
    Body      string
    Options   []string             // button 文本 / select_static 选项
    RequestID string
    // F-46 新增
    Kind      ChoiceKind             // Permission / Decision / Preview；决定 header 配色 + 是否加 👉
    Action    string               // 当只有单一 action 时（替代 options）
    Choices   []ChoiceOption         // 比 Options 更结构化：每个选项可以指定 emoji + label + action
    Form      []CardFormField      // 预留 form input（F-48）
    HeaderColor string             // blue / red / green / grey；默认按 Kind 推
    // AskUserQuestion：Questions + Step + Picks。len<=1 一击即答；len>1 卡内向导
}
```

`ChoiceKind`：

```go
type ChoiceKind int
const (
    ChoiceKindPermission ChoiceKind = iota  // Action Needed（👉 前缀；AskUserQuestion / 权限点选）
    ChoiceKindDecision                     // 决策卡（无 👉，自带 Choices，等宽 column_set）
)
```

### 3.3 Decision card 渲染（branch-exists / worktree-fail）

```go
// branch-exists scenario (gtw fix flow decision)
&Card{
    Kind:    ChoiceKindDecision,
    Title:   fmt.Sprintf("⚠️ 分支 `%s` 已存在", payload.Branch),
    Body:    fmt.Sprintf("issue: #%d  %s\n\n选择操作:", payload.IssueID, payload.Title),
    Choices: []ChoiceOption{
        {Emoji: "🆕", Label: "用 -v2 新分支", Action: "act:/gtw/branch-newv2"},
        {Emoji: "🔗", Label: "加入现有协作",  Action: "act:/gtw/branch-join"},
        {Emoji: "❌", Label: "取消",          Action: "act:/gtw/cancel"},
    },
    RequestID: "gtw-fix-branch-exists-" + payload.IssueID,  // 关联 userMsgID
}

// worktree-fail scenario (gtw fix flow decision)
&Card{
    Kind:    ChoiceKindDecision,
    Title:   fmt.Sprintf("❌ 创建 worktree 失败(#%d)", payload.IssueID),
    Body:    fmt.Sprintf("branch: %s\n\n选择操作:", payload.Branch),
    Choices: []ChoiceOption{
        {Emoji: "🔄", Label: "重试", Action: "act:/gtw/worktree-retry"},
        {Emoji: "❌", Label: "取消", Action: "act:/gtw/cancel"},
    },
    RequestID: "gtw-fix-worktree-fail-" + payload.IssueID,
}
```

### 3.4 Button 渲染（card.go 改造）

`buildInteractiveCard` 改造点：

1. 拆 header 配色：根据 `ChoiceKind` 选 `template`，默认 blue（permission 仍 blue + 👉）
2. `👉 ` 前缀只在 `ChoiceKindPermission` 时加（历史上曾用 🔐）
3. gtw `Choices` 渲染为 `column_set` 等宽布局（cc-connect `CardActionLayoutEqualColumns`），3 个按钮横排
4. 单按钮场景（worktree-fail 两选项）也用 `column_set` 一致布局
5. **Action Needed 选项一行一个**（`buildStackedButtons`）：长中文 label 不被等宽列截断。每题底部有 dashboard 同款 **Type your answer** 输入 + **Skip this question** + **Submit**（`form` / `custom:` / `skip:`）。`len(Questions)>1` 时卡内向导 `👉 Action Needed · i/n`，中间 click 除 PATCH 外还要在 `card.action.trigger` 回调里带回下一张卡（`card.type=raw`），否则飞书 form 停在「已提交」、客户端不翻到 2/N。最后一步 inbound `nm-q:` 批答（host `matchesQuestions` 要求整批 id 对齐）

```go
// cc-connect 的等宽布局
if e.Layout == core.CardActionLayoutEqualColumns {
    columns := make([]map[string]any, 0, len(actions))
    for _, action := range actions {
        columns = append(columns, map[string]any{
            "tag": "column", "width": "weighted", "weight": 1,
            "vertical_align": "center", "horizontal_align": "center",
            "elements": []map[string]any{action},
        })
    }
    columnSet := map[string]any{
        "tag": "column_set", "columns": columns,
    }
    if len(actions) == 2 { columnSet["flex_mode"] = "bisect" }
    elements = append(elements, columnSet)
}
```

### 3.5 `handleCardAction` 路由

```go
func (a *Adapter) handleCardAction(ctx context.Context, event *larkcallback.CardActionTriggerEvent) (*larkcallback.CardActionTriggerResponse, error) {
    if event.Event == nil || event.Event.Action == nil { return nil, nil }

    // 1. 取 action string（兼容 button.value 与 select_static.option）
    actionStr := ""
    if v, ok := event.Event.Action.Value["action"].(string); ok { actionStr = v }
    if actionStr == "" && event.Event.Action.Option != "" {
        actionStr = "opt:" + event.Event.Action.Option  // select_static 走 opt: 前缀
    }
    if actionStr == "" { return nil, nil }

    // 2. 按前缀分发
    switch {
    case strings.HasPrefix(actionStr, "act:"):
        return a.handleActCardAction(ctx, event, actionStr)
    case strings.HasPrefix(actionStr, "nav:"):
        return a.handleNavCardAction(ctx, event, actionStr)  // F-47
    case strings.HasPrefix(actionStr, "cmd:"):
        return a.handleCmdCardAction(ctx, event, actionStr)  // F-48
    }

    return nil, nil
}
```

### 3.6 `act:` 派发路径

```go
func (a *Adapter) handleActCardAction(
    ctx context.Context,
    event *larkcallback.CardActionTriggerEvent,
    actionStr string,
) (*larkcallback.CardActionTriggerResponse, error) {
    if event.Event.Context == nil { return nil, nil }
    chatID := event.Event.Context.OpenChatID
    messageID := event.Event.Context.OpenMessageID
    userID := ""
    if event.Event.Operator != nil { userID = event.Event.Operator.OpenID }

    // 1. actionStr → (ReactionKind, draftKind)。"act:/gtw/branch-newv2"
    //    映射成 (ReactionNewV2, DraftFixBranchExists)。
    kind, targetEmoji, ok := gtwActionMap(actionStr)
    if !ok {
        return &larkcallback.CardActionTriggerResponse{
            Toast: &larkcallback.Toast{Type: "warning", Content: "未知操作: " + actionStr},
        }, nil
    }

    // 2. 构造 synthetic ReactionEvent，发布到 inbound 流
    synthetic := &InboundMessage{
        ChatID:     chatID,
        UserID:     userID,
        Text:       "",
        HasMention: true,
        MessageID:  messageID,
        Reaction: &chatsession.ReactionEvent{
            TargetMsgID: messageID,
            Emoji:       targetEmoji,
            UserID:      userID,
            ChatID:      chatID,
        },
    }
    select {
    case a.incoming <- channel.Message{Msg: synthetic}:
    case <-ctx.Done(): return nil, ctx.Err()
    default:
        // inbound 满：记 warn 后继续（生产 daemon inbound 128 buffer，正常不会满）
    }

    // 3. PATCH 原卡（异步，等 action handler 派发完后更新）
    //    "原生状态卡" 在 inbound 流里被 gtw.HandleAction 处理完后，
    //    会再发一条 OutboundMessage 触发 PATCH。我们在这里只先 toast。
    return &larkcallback.CardActionTriggerResponse{
        Toast: &larkcallback.Toast{Type: "info", Content: fmt.Sprintf("✅ 已选择 %s", targetEmoji)},
    }, nil
}
```

`gtwActionMap` 在 `internal/gtw/action_routing.go`：

```go
var gtwActionPrefixes = map[string]ReactionKind{
    "act:/gtw/branch-newv2":   ReactionNewV2,
    "act:/gtw/branch-join":    ReactionJoin,
    "act:/gtw/worktree-retry": ReactionRetry,
    "act:/gtw/cancel":         ReactionCancel, // any decision card
}

func ActionLookup(action string) (ReactionKind, bool) {
    // unknown / retired (label-force, worktree-cancel, …) → false
}
```

### 3.7 原地 PATCH（action 完成后）

gtw 派发完成后 (`gtw.HandleAction` 返回 true)，`HandleAction` 在 CardType 卡上 follow-up 发一条 `OutboundMessage{Kind: OutChoicePatch, Card: <updatedCard>, ReplyTo: userMsgID}`。Feishu adapter 的 `Send` 把 `OutChoicePatch` 路由到 `PatchMessage`：

```go
case gateway.OutChoicePatch:
    if msg.Choice == nil || msg.ReplyTo == "" { return errors.New(...) }
    body, err := buildInteractiveCard(msg.Choice)
    if err != nil { return err }
    _, err = a.updateContent(ctx, msg.ReplyTo, interactiveMessageType, body, false)
    return err
```

`buildInteractiveCard` 复用，PATCH 后的卡是 `Choices` + disabled 状态：

```go
&Card{
    Kind:    ChoiceKindDecision,
    Title:   ...,
    Body:    ... + "\n\n✅ 已选择 " + chosenEmoji,
    Choices: []ChoiceOption{chosen},
    RequestID: ...,
}
```

render 时所有 button 的 `disabled: true`：

```go
action := map[string]any{
    "tag": "button", "text": plainText(btn.Text), "type": btnType, "value": valMap,
    "disabled": true,  // F-46 新增
}
```

### 3.8 派发后 follow-up

`gtw.HandleAction` 在 `executeBranchExistsAction` / `executeWorktreeFailAction` 完成后调 `deps.Send` 发 follow-up text（"❌ Cancelled fix #N." 等）。F-46 把这些 text 改成 `OutChoicePatch`：

```go
// 之前
deps.Send(ctx, OutMsg{ChatID: ev.ChatID, Text: fmt.Sprintf("❌ Cancelled fix #%d.", p.IssueID)})

// F-46
deps.SendCard(ctx, OutChoiceMsg{
    ChatID:   ev.ChatID,
    ReplyTo:  ev.TargetMsgID,
    Card:     &Card{Kind: ChoiceKindResult, Title: fmt.Sprintf("❌ Cancelled fix #%d", p.IssueID)},
})
```

`deps.Send` 类型扩展：

```go
type OutMsg struct {
    ChatID    string
    Text      string
    ReplyTo   string
    // F-46 新增
    Card      *Card       // 当需要发/ PATCH 交互卡时填
    ChoiceKind  string      // "create" | "patch"，create 走 sendContent，patch 走 PatchMessage
}
```

### 3.9 inbound 流容量

`Adapter.incoming` 当前 buffer=128。`/gtw fix` 跑的时候 inbound 流主要是用户输入 + reaction，F-46 加 button callback 后同一个 flow 多了一条合成消息路径——128 buffer 足够。监控：如果有"channel full"warn，加 buffer 或直接同步 `cs.HandleAction`（不走 inbound）。

## 4. 接口

### 4.1 Feishu adapter

```go
// internal/channel/feishu/adapter.go
func (a *Adapter) handleActCardAction(ctx context.Context, event *larkcallback.CardActionTriggerEvent) (*larkcallback.CardActionTriggerResponse, error) {
    // 1. 从 event.Action.Value["action"] 抽 "act:/gtw/<scenario>"
    // 2. messages.ActionLookup(actionStr) → ReactionKind
    //    （契约由 internal/command/gtw/render_lookup_contract_test.go 锁住）
    // 3. 合成 InboundMessage{Reaction: services.ReactionEvent{TargetMsgID, Emoji, ChatID, UserID}}
    // 4. push 到 a.incoming（buffer=128, 默认 0 阻塞）
    // 5. 返回 Toast（unknown action 时给 warning toast "未知操作: ..."）
}
```

### 4.2 gateway Card 字段

```go
// internal/gateway/messages.go
type Card struct {
    Title      string
    Body       string
    Options    []string
    RequestID  string
    Kind       ChoiceKind       // F-46
    Action     string         // F-46
    Choices    []ChoiceOption   // F-46
    Form       []CardFormField // F-48
    HeaderColor string        // F-46
}
type ChoiceKind int
type ChoiceOption struct {
    Emoji  string
    Label  string
    Action string  // act:/gtw/...
}
```

### 4.3 OutboundKind 新增

```go
const (
    // ...existing...
    OutChoice        OutboundKind = iota  // 已有
    OutChoicePatch  OutboundKind          // F-46 新增：PATCH 现有卡（不是发新卡）
)
```

### 4.4 决策卡入口

```go
// internal/gtw/fix.go
// emitBranchExistsDraft / emitWorktreeFailDraft 改为构造 gateway.Card 而不是纯文本
func emitBranchExistsDraft(...) (*Result, error) {
    card := buildDecisionCard(payload, existingPath)  // F-46
    return sendCard(ctx, deps, chatID, messageID, userMsgID, card, ...)
}
```

## 5. 测试

### 5.1 单元测试

- `internal/gtw/action_routing_test.go`：`gtwActionMap` 全 prefix 命中
- `internal/channel/feishu/adapter_test.go::TestHandleCardAction_ActRouting`：合成 `CardActionTriggerEvent`，验证 inbound 流收到正确 `ReactionEvent`
- `internal/channel/feishu/card_test.go::TestBuildInteractiveCard_DecisionKind`：验证 `ChoiceKindDecision` 不加 👉、3 个 button 用 `column_set`
- `internal/channel/feishu/adapter_opt_test.go`：单题 `opt:` 立即 inbound；**Type your answer** Submit / **Skip this question** 走 `nm-q:`；多题向导中间 click PATCH **且**回调带回 `card.type=raw` 的 2/N，最后一步 `nm-q:` 批答；选项一行一个；approval 卡无 custom/skip。完整规则见 [`feishu-cards.md`](./feishu-cards.md)
- `internal/channel/feishu/card_test.go::TestBuildInteractiveCard_DisabledButtons`：PATCH 后的卡 button 全 disabled

### 5.2 集成测试

- `internal/gateway/dispatch_action_test.go::TestDispatch_CardAction_RoutesToGTW`：合成 `card.action.trigger` → 走完 `gtw.HandleAction` → 验证 dispatched `ReactionEvent`

### 5.3 手动验证

- 飞书客户端（iOS/Android）：点 branch-exists 卡片按钮 → toast "✅ 已选择 🆕" → 原卡变成 "已选择 🆕" 状态
- 飞书桌面：同上
- 飞书 Web：部分版本不渲染 button，确认走 emoji 降级路径
- daemon 重启后跑 `/gtw fix 42`，决策卡渲染形状与点按钮的反馈

## 6. 风险与回退

1. **button 客户端兼容**：飞书 Web 部分版本不渲染 button → 决策卡必须保留纯文本 markdown 降级
   - 回退：`if !Card.SupportsInteractive { render as markdown }`——`SupportsInteractive` 字段由 channel adapter 报告
2. **action handler 路由回灌**：`handleCardAction` 同步入 inbound 流，inbound 满会丢
   - 监控：加 metric `nightme_card_action_inbound_full_total`
3. **PATCH 失败**：PATCH 失败时用户看到旧卡 + action 没生效提示
   - 现状：PATCH 失败由 `WithTransientRetry` 兜底（retry.go）
4. **ChoiceKind 误用**：decision 卡错填 `ChoiceKindPermission` 会被加 👉 + 颜色不对
   - 默认零值 `ChoiceKind(0)` 保留为 Permission 行为；新增 `ChoiceKindDecision` 起 iota=1

## 7. 实施状态

| 步 | 计划 | 实际 |
| --- | --- | --- |
| 1. `Card` / `OutboundKind.OutChoicePatch` 字段 | 1d | ✅ done |
| 2. `buildInteractiveCard` 改造（column_set 等宽 + Disabled + ChosenChoiceEmoji） | 1d | ✅ done |
| 3. `gtwActionMap` + `handleActCardAction` | 1d | ✅ done |
| 4. Feishu adapter `OutChoicePatch` case | 0.5d | ✅ done |
| 5. `executeXxxAction` follow-up 改发 OutChoicePatch | 1d | ✅ done（emitFollowUp + gtwSendAdapter） |
| 6. `/gtw fix` 决策卡改用 `buildDecisionCard` | 1d | ❌ 推迟（`/gtw fix` 路径仍走纯文本 markdown，未来再迁）|
| 7. 单元 + 集成测试 | 1d | 🟡 部分（`handlers_gtw_test.go` 6 个 case，但 `/gtw fix` 路径未覆盖）|
| 8. 飞书三端验证 | 1d | 🟡 用户 UAT（无真飞书账号）|

实际花费：3 人·天（大部分是踩坑时间）。**踩坑时间 ≈ 实现时间**，见 §10。

## 8. 文档同步

- `SPEC.md` §3.5（F-45 reaction → gtw pipeline）补 button → reaction 通路
- `channel/feishu.md` §13（card lifecycle）补 button click handler + PATCH 路径
- `../channel/feishu-rendering.md` §6 cross-link（决策卡 footer 复用 SessionContext）
- `../channel/feishu-rendering.md` §3 cross-link（决策卡不进入 receipt，是独立 card message）

## 9. 不在 F-46 范围

- `nav:` 前缀（卡片内导航 / 翻页）
- `cmd:` 前缀（button click 当用户命令派发）
- `form input`（删除模式那种多选表单）
- `select_static` 下拉组件（cc-connect 用 select_static 替代 button 列表——这是 UX 增强，不是必需）
- 卡 disable 后 emoji reaction 是否还生效（默认是；用户已经点过 button 了再点 emoji 是 noop）

## 10. 实现过程总结（按踩坑时序）

这一节记录落地过程中遇到的关键 design decision 与 debug 经验，下次再写类似的交互卡 PATCH 路径可以照抄。

### 10.1 完整链路（生产运行时）

```
用户点 Feishu 卡上的按钮
        │
        ▼
Feishu SDK 收到 card.action.trigger 事件
        │
        ▼
internal/channel/feishu/adapter.go::handleCardAction (OnP2CardActionTrigger 注册)
        │
        ▼
messages.ActionLookup(actionStr) → 解析 act:/gtw/<scenario> → ReactionKind (🔄 / 🆕 / 🔗 / ❌)
        │
        ▼
handleActCardAction 合成一个 InboundMessage{Reaction: <ReactionEvent>}
        push 到 a.incoming channel（buffer=128）
        │
        ▼
internal/gateway/gateway.go::dispatchAction
   ├─ g.actionHandler != nil?  ── 岔路 A：nil → Consumed:true Dropped:true（pre-F-45 行为）
   └─ g.actionHandler(ctx, msg)  ── 由 cmd/nightme/run.go 装的生产 trampoline
        │
        ▼
生产 trampoline：cs := mgr.Get(msg.ChatID) → cs.HandleAction(ctx, ev)
   ├─ cs == nil?  ── 岔路 B：return false
   └─ cs.onReaction(ctx, ev)        ← 由 `internal/command/gtw/` 的 reaction handling 装上（原 `internal/gateway/handlers_gtw.go::wireGTWActionOnSession`；F-102 重构后 `gtw` 整体迁到 `internal/command/gtw/`，reaction 路由走 `services.ReactionRouter`，已不再走 `cs.SetActionHandler`）
        （`SetActionHandler` 仅 debug 时代使用；生产路径是 `cs.onReaction`）
        │
        ▼
gtw.HandleAction → executeXxxAction → emitFollowUp
        │
        ▼
emitFollowUp：if draft.BotMessageID != "" → PATCH 原卡；else 落 plain text
        │
        ▼
gtwSendAdapter → channel.Send(OutboundMessage{Kind: OutChoicePatch)
        │
        ▼
Feishu adapter Send → OutChoicePatch case
        ├─ msg.Choice == nil  ── 岔路 C：return error (被 _ = 吞掉)
        ├─ buildInteractiveCard(msg.Choice)  ── 岔路 D：return error (被 _ = 吞掉)
        └─ a.PatchMessage(ctx, msg.ReplyTo, content)
                └─ a.logOutgoing("patch_message", ..., err)  ── logOutgoing 总 fire
```

每一步**都加了 debug log**（`slog.Default().Warn("F-46 debug: <step>", ...)`），可以从前到后串起来定位断点。

### 10.2 关键设计决定

#### 10.2.1 Button value 编码：`action` 替代 `option`

`buildCardButtons` 的 button value 之前是：

```json
{"request_id": "...", "option": "🆕"}
```

Feishu SDK 的 `event.Action.Option` 字段是 `select_static` 组件的选项值，**不是**按钮的语义含义。改成 cc-connect 风格：

```json
{"action": "act:/gtw/branch-newv2", "request_id": "..."}
```

`action` 字段是协议级语义（点完去哪里），`request_id` 保留卡片关联 token。`option` 字段完全废弃。

#### 10.2.2 `act:` 前缀三段式（沿用 cc-connect）

| 前缀 | 语义 | F-46 落地 |
| --- | --- | --- |
| `act:/gtw/branch-newv2` | branch-exists 🆕 | ✅ 已实现 |
| `act:/gtw/branch-join` | branch-exists 🔗 | ✅ 已实现 |
| `act:/gtw/worktree-retry` | §5.3.3 🔄 | ✅ 已实现 |
| `act:/gtw/cancel` | 任意决策卡 ❌ | ✅ 已实现 |
| `nav:/xxx` / `cmd:/xxx` / `act:/gtw/label-force` | 导航 / 命令 / §5.3.2 强制接管 | ❌ 未进 map（F-47/48/49） |

`ActionLookup` 只收录**当前卡面真实发出的** action；占位 / alias（`label-force`、`worktree-cancel`）已从 map 清掉。

#### 10.2.3 PATCH 视觉：颜色反转 + 完整 label + 无 "已选择" 头

PATCH 后的卡布局（`buildCardButtons` 中处理）：

- **选中的按钮**：`type: "success"` 填充绿 + `✓` 前缀 + 完整 label（如 `✓ 🔄 重试`）。Feishu 把 `success` 类型渲染为绿色填充，与 `default` 灰描边对比强烈。
- **没选的按钮**：`type: "default"` 灰描边 + `disabled: true` + 完整 label（如 `❌ 取消`）。完整 label 解决"用户只看 icon 不知道意思"的痛点。
- **body 不再有"✅ 已选择 X"独立行**——那个是冗余的视觉噪声，PATCH 后的按钮绿色已经传达"已选"语义。
- body 只剩原始 body + 底部一行 `Retry failed: ...`（来自 `m.PatchResult`）。

#### 10.2.6 统一 logger：打通 `slog.Default()` 和 plumbed logger

**问题**：`slog.Default().Warn(...)` 在 daemon 进程里是 **Go 默认的 no-op logger**。Feishu adapter 的 `feishu: outgoing` 走 `a.logger.Info/Warn`（plumbed 的 runtime logger），能进 log 文件。我加的 F-46 debug log 走 `slog.Default()`，**全部进黑洞**。

**根因**：`cmd/nightme/main.go` 调 `logging.New(cfg)` 而不是 `logging.Setup(cfg)`。`Setup` 内部就是 `New + slog.SetDefault(lg)`，关键差那一行 SetDefault。

**修法**（`main.go`）：

```go
var logger *slog.Logger
if l, err := logging.New(cfg); err != nil {
    ...
} else {
    logger = l
}
// F-46 debug: install logger as slog.Default so all
// downstream code (handlers_gtw.go, gateway.go, action.go,
// chatsession.go) that calls slog.Default().Warn(...) lands
// in the same MultiWriter sink as the plumbed logger.
slog.SetDefault(logger)
defer func() { _ = logging.Close(logger) }()
Execute(logger)
```

现在 `slog.Default()` 和 plumbed `logger` 指向**同一个 handler**，`MultiWriter(file, stdout, stderr)` 三路都会出。`add_reaction` 类的 7 路日志和 debug 类 9 路日志都进同一个文件。

### 10.3 调试心得

调试这种"dispatch + 回调 + 异步 + 跨模块"的链路，有 **9 个经典岔路**必须每处都打 log：

| 岔路 | 出现场景 | log 在哪 |
| --- | --- | --- |
| A | `g.actionHandler` 没装 | `gateway.dispatchAction` 入口 |
| B | `cs.onReaction` 没装 | `chatsession.HandleAction` 入口 |
| C | `OutChoicePatch` case 入口 `msg.Choice == nil` | adapter Send OutChoicePatch case |
| D | `buildInteractiveCard` 失败（RequestID 空 / JSON 失败）| adapter buildInteractiveCard 入口 |
| E | `gtwDrafts.Lookup(ev.TargetMsgID) == nil`（draft 被 Take 走）| gtw 包装 closure 入口 |
| F | switch emoji 不匹配（`if rk == ReactionRetry` 打印 `matches`）| executeXxxAction 入口 |
| G | `draft.BotMessageID == ""`（stamp 没跑）| emitFollowUp 入口 |
| H | `deps.Send` 返回 error 但被 `_ =` 吞掉 | emitFollowUp 入口 + Send 后 |
| I | `PatchMessage` 返回 error | Feishu adapter PatchMessage 内部 logOutgoing |

每条岔路要 `slog.Default().Warn("F-46 debug: <岔路>")` + 关键字段值（如 `target_msg_id`、`bot_msg_id`、`matches`）。这样出错时一眼看出断在哪。

### 10.4 后续 F-47 / F-48 / F-49 排期

| F | 工作量 | 内容 |
| --- | --- | --- |
| F-47 | 2d | `nav:` 前缀（卡片内导航 / 翻页 / 关闭按钮）|
| F-48 | 1d | `cmd:` 前缀（button click 当用户命令派发到 `gtwRunFix`）|
| F-49 | 3d | `act:/gtw/label-force` + §5.3.2 强制接管 + emoji label 替换 |
| 后续 | 1d | `select_static` 下拉组件（替换 button 列表的 UX 增强） |
| 后续 | 2d | form input（删除模式多选表单）|
| 后续 | 1d | 卡 disable 后 emoji reaction 行为审计（应该 noop）|
| 后续 | 1d | 真实飞书 iOS / Android / Web 三端视觉回归（success type 按钮绿色渲染一致性）|

这些留给 F-47+。

---

## A12. F-49: Context Compaction Counter + Footer Line 1 🗜 N

> **Source**: `../channel/feishu-rendering.md`

---

## 0. 背景

### 0.1 用户痛点

当前 footer 的 `💰 ↓ X · ↻ X · ↑ X · Total X` 是 **cumulative across the entire AgentSession**（F-45 决策）。当 agent 执行 context compaction 时（Claude Code 在接近上下文窗口上限时自动 compact；Pi 显式 emit `compaction_start/end`），本轮输入被截断/摘要化，但 cumulative 仍持续累加。结果：

```
💰 ↓ 156k · ↻ 1.2M cached · ↑ 18k · Total 1.37M · $1.245
```

用户看到 1.37M total **远超** 上下文窗口上限（如 200k），**无法判断**到底是"真的用了 1.37M 上下文"（不可能，会爆窗口）还是"agent 已 compact 多次但 cumulative 不反映"。从 IM 视角看是 mystery number。

**用户原话**（2026-08-06）：

### 0.2 现状

**EventCompaction 已存在但 runtime 完全不消费**：
- `internal/agent/agent.go:109` `EventCompaction` Kind 定义存在
- `internal/agent/agent.go:374` `CompactionEvent{Subtype string}` payload
- `internal/bridge/pi/translate.go` emit `compaction_start` + `compaction_end` 各一条
- `internal/bridge/claudecode/stream.go` emit `compact` / `compaction` 各一条
- `internal/gateway/translate.go` 把 `EventCompaction` translate 成 `OutCompaction`
- `internal/channel/feishu/adapter.go` `Send` case `OutCompaction` → `ReplyInThreadAndChat` 发 `✶ Compacting conversation…`（F-37 决策）
- `internal/channel/feishu/receipt_event.go` `eventToEntry` 对 `EventCompaction` 返回 `(_, false)`（不进 receipt）
- **`runtime handler 完全不感知`**——既不累加也不持久化，只是把它当普通事件透传到 IM

**问题**：count 信息全丢失，footer 显示的 cumulative 数字对用户来说**没有 compaction 这个调节变量**。

### 0.3 用户澄清（2026-08-06 对话）

**Q：每个 agent 的协议差异谁负责消化？**

**Q：emoji 选什么？**

**Q：压缩时 token 怎么处理？**

**Q：`CompactionEvent.Subtype` 字段还有用吗？**

**Q：`OutCompaction` kind 还要保留吗？**

---

## 1. 设计

### 1.1 视觉对比

**改前**（典型长 session，已 compact 3 次）：

```
🤖 claude · opus-4-5
💰 ↓ 156k · ↻ 1.2M cached · ↑ 18k · Total 1.37M · $1.245
📁 code/nightme · ⎇ main · ↑ 3 · ? 2 · � 2
```

用户看到 1.37M total **不知所云**：超过上下文窗口 6 倍，怀疑数据错误或 agent bug。

**改后**（同样 session，加了 🗜 计数 + token 归零语义）：

```
🤖 claude · opus-4-5 · 🗜 3
💰 ↓ 5k · ↻ 2k · ↑ 0.8k · 7.8k · $1.245
📁 code/nightme · ⎇ main · ↑ 3 · ? 2 · ⇡ 2
```

**关键变化**：
- Line 1 末尾追加 `· � 3`——直接告诉用户"本 Session 已 compact 3 次"
- Line 2 的 token 部分从"累计 1.37M"变成"压缩后 7.8k"——直观显示**当前上下文用量**
- `· $1.245` **保持不变**——lifetime cost 跨压缩单调累加，与 token 数字形成对比

**用户视角解读**：
- `🗜 3` + `↓ 5k · Total 7.8k` = "当前会话上下文用了 7.8k，离 200k 上限还很远，但这是压缩后的数字，3 次压缩之前实际用过 1.37M tokens"
- `$1.245` = "这个 Session 总共花了 $1.245，无论压缩几次都不会变"

### 1.2 两个 token 目的 → 两个独立 metric

| 用户目的 | Footer 段 | 字段 | 压缩时行为 |
|---|---|---|---|
| **总耗费**（lifetime spend） | `$1.245` | `CostUSD` | **保留**，单调累加 |
| **当前 Session 上下文用量** | `↓ 5k · ↻ 2k · ↑ 0.8k · 7.8k` | 4 个 token 字段 | **归零**，重新累加 |

**为什么这样切**：
- `CostUSD` 是货币量，跟"花了多少"绑定，不会因为压缩退钱——跨压缩累加
- 4 个 token 字段是**输入窗口**的快照——压缩后输入窗口被截断/摘要，下一个 turn 的 input 自然变小，所以归零后**自然从下一次 EventUsage 重新累加**，无需特殊处理

### 1.3 Bridge 抽象（核心不变式）

```
                  ┌─────────────────────────────────────────────�
  Pi 协议         │  compaction_start → [suppressed]            │
  compaction_start│  compaction_end   → EventCompaction × 1    │
  compaction_end  │                                             │
                  └─────────────────────────────────────────────┘
                                ↓
                  ┌─────────────────────────────────────────────┐
  Claude 协议     │  result.subtype == "compact" /              │
  result.subtype  │  "compaction" → EventCompaction × 1         │
                  └─────────────────────────────────────────────�
                                ↓
                  ┌─────────────────────────────────────────────┐
  runtime handler │  case EventCompaction:                       │
  (协议无关)      │    as.RecordCompaction()                     │
                  │    // 不判断 Subtype，不产生 Outbound        │
                  └─────────────────────────────────────────────┘
                                ↓
                  ┌─────────────────────────────────────────────┐
  AgentSession    │  RecordCompaction:                          │
  (累计 + 归零)   │    compactionCount++                        │
                  │    cumulativeUsage.InputTokens        = 0   │
                  │    cumulativeUsage.CacheCreation...   = 0   │
                  │    cumulativeUsage.CacheRead...       = 0   │
                  │    cumulativeUsage.OutputTokens       = 0   │
                  │    // CostUSD 保留                          │
                  │    cumulativeDirty = true                   │
                  └─────────────────────────────────────────────┘
                                ↓
                  ┌─────────────────────────────────────────────┐
  Channel         │  Footer Line 1 末尾：                       │
  (渲染)          │    🤖 Agent · Model · 🗜 N                  │
                  └─────────────────────────────────────────────┘
```

**关键约束**：
- **runtime 不基于 Subtype 字符串 dispatch**——`CompactionEvent.Subtype` 字段**不存在**
- **runtime 不产生 OutboundMessage 给 channel**——`OutCompaction` kind **不存在**
- **bridge 各自负责把协议差异消化掉**——Pi 屏蔽 start；Claude Code 自然就是 1 条
- **新 agent 接入时**只需保证"一次压缩周期 = 一个 EventCompaction"——runtime 一视同仁

### 1.4 AgentSession 新增 API

```go
// internal/chatsession/agentsession.go

type AgentSession struct {
    // ... 既有字段 ...

    // F-49: cumulative compaction count + per-cycle token stats.
    // 由 cumulativeUsageMu 守护（与 cumulativeUsage 共用同一把锁，
    // 因为 RecordCompaction 原子修改两者）。Persists across daemon
    // restarts; cleared only by /new (see F-45 §1.5).
    cumulativeUsageMu sync.RWMutex
    cumulativeUsage   agent.UsageInfo
    compactionCount   int       // ← NEW
    cumulativeDirty   bool
}

// RecordCompaction atomically:
//   1. increments compactionCount;
//   2. zeroes the four token fields of cumulativeUsage
//      (InputTokens, CacheCreationInputTokens, CacheReadInputTokens,
//      OutputTokens), preserving CostUSD;
//   3. marks cumulativeDirty so the next PersistIfDirty flushes
//      both the new count and the post-reset token snapshot.
//
// The token reset makes Footer Line 2 (↓ ↻ ↑ total) reflect
// "since-last-compaction" — i.e. the agent's current context window
// usage, while $cost stays as lifetime spend. See F-49 §1.2.
func (as *AgentSession) RecordCompaction() {
    as.cumulativeUsageMu.Lock()
    defer as.cumulativeUsageMu.Unlock()
    as.compactionCount++
    as.cumulativeUsage.InputTokens = 0
    as.cumulativeUsage.CacheCreationInputTokens = 0
    as.cumulativeUsage.CacheReadInputTokens = 0
    as.cumulativeUsage.OutputTokens = 0
    // CostUSD deliberately preserved.
    as.cumulativeDirty = true
}

// CompactionCount returns the cumulative number of completed
// compaction cycles observed on this AgentSession. Snapshot under
// RLock; safe for concurrent read alongside RecordCompaction.
func (as *AgentSession) CompactionCount() int {
    as.cumulativeUsageMu.RLock()
    defer as.cumulativeUsageMu.RUnlock()
    return as.compactionCount
}
```

**ResetCumulative 同步修改**（`/new` 命令清零所有累计，包括 compactionCount）：

```go
func (as *AgentSession) ResetCumulative() {
    as.cumulativeUsageMu.Lock()
    as.cumulativeUsage = agent.UsageInfo{}
    as.compactionCount = 0  // ← NEW
    as.cumulativeDirty = true
    as.cumulativeUsageMu.Unlock()
}
```

**Entry() 序列化同步修改**：

```go
func (as *AgentSession) Entry() *registry.AgentSessionEntry {
    as.cumulativeUsageMu.RLock()
    cum := as.cumulativeUsage
    cc := as.compactionCount  // ← NEW
    as.cumulativeUsageMu.RUnlock()
    return &registry.AgentSessionEntry{
        // ... 既有字段 ...
        CumulativeUsage: &cum,
        CompactionCount: cc,  // ← NEW
        Model:           as.Model(),
    }
}
```

**FromAgentSessionEntry 还原同步修改**：

```go
if e.CumulativeUsage != nil {
    as.cumulativeUsage = *e.CumulativeUsage
}
as.compactionCount = e.CompactionCount  // ← NEW：JSON 默认零值兼容老数据
```

### 1.5 SessionContext 扩展

```go
// internal/gateway/messages.go
type SessionContext struct {
    // ... 既有字段（Agent / Model / CumulativeUsage / Workspace / GitStatus）...

    // F-49: cumulative count of completed context compactions on
    // this AgentSession. 0 when never compacted. Persists across
    // daemon restarts; cleared only by /new. Sourced from
    // AgentSession.CompactionCount at the same instant as
    // CumulativeUsage so the footer Line 1 (🗜 N) and Line 2 (↓ ↻
    // ↑ total) tell a coherent story: "lifetime cost grew by $X,
    // context window was reset and now totals Y since the last of
    // N compactions".
    CompactionCount int `json:"compactionCount,omitempty"`
}
```

### 1.6 Footer Line 1 渲染规则

```go
// internal/channel/feishu/usage_footer.go
// Line 1: identity (🤖 Agent · Model · 🗜 N).
idParts := []string{"🤖"}
if ctx.Agent != "" {
    idParts = append(idParts, ctx.Agent)
}
if ctx.Model != "" {
    idParts = append(idParts, "·", ctx.Model)
}
if ctx.CompactionCount > 0 {
    idParts = append(idParts, "·", "�", strconv.Itoa(ctx.CompactionCount))
}
if len(idParts) > 1 {
    lines = append(lines, strings.Join(idParts, " "))
}
```

**Segment 规则**：
| 段 | 来源 | Omit 规则 |
|---|---|---|
| 🤖 | literal | 永远显示 |
| `<Agent>` | `ctx.Agent` | `""` 时省略 |
| `· <Model>` | `ctx.Model` | `""` 时省略 |
| `· 🗜 <N>` | `ctx.CompactionCount` | **仅 N > 0 时显示**（沿用 F-45 §1.6 zero-omit 约定） |

**实测样例**：

```
🤖 claude                                         # 无 model 无 compaction
🤖 claude · opus-4-5                              # 标准
🤖 claude · opus-4-5 · � 3                       # 3 次压缩
🤖 claude · opus-4-5 · � 1                       # 1 次压缩
```

**Glyph 选型**：🗜 (U+1F5DC, Unicode 正式名 "COMPRESSION")——语义零歧义；与 F-45 line 1 🤖 / F-45 line 2 💰 / F-48 line 3 📁 emoji category header 风格一致。

### 1.7 Bridge 改动（细节）

**Pi bridge (`internal/bridge/pi/translate.go`)**：

```go
case "compaction_start":
    // F-49: 屏蔽瞬态信号。runtime handler 不区分 start / end ——
    // 任何 EventCompaction 都计为一次完成的压缩周期。如果 start
    // 不屏蔽，runtime 会被双数（Pi 一个压缩周期 = start + end 两条）。
    // 原因在 F-49 §1.3 "Bridge 抽象"。
    return nil, nil

case "compaction_end":
    // F-49: 仍 emit EventCompaction（不带 Subtype，因为字段已删除）。
    return []agent.AgentEvent{{
        Kind: agent.EventCompaction,
    }}, nil
```

**Claude Code bridge (`internal/bridge/claudecode/stream.go`)**：

```go
// F-49: 之前 emit EventCompaction{Subtype: subtype}（subtype="compact" / "compaction"）。
// 现在 Subtype 字段已删除，bridge 只 emit 一个 marker EventCompaction。
return agent.AgentEvent{
    Kind: agent.EventCompaction,
}, true
```

### 1.8 Runtime handler 改动

**`cmd/nightme/run.go::newEventHandler`**：

```go
// F-49: EventCompaction 不再产生 OutboundMessage（无 OutCompaction kind）。
// 也不再判断 Subtype（字段已删除）。runtime 一视同仁，任何 EventCompaction
// 都视为一次完成的压缩周期——bridge 层负责把协议差异消化掉。
case agent.EventCompaction:
    s.RecordCompaction()
    if logger != nil {
        logger.Debug("runtime: compaction observed",
            "agent", s.Agent,
            "count", s.CompactionCount())
    }
```

**`sessionContextInto` stamp 扩展**：

```go
out.SessionContext = &gateway.SessionContext{
    Agent:           s.Agent,
    Model:           s.Model(),
    CumulativeUsage: snap,
    CompactionCount: s.CompactionCount(),  // ← NEW
    Workspace:       s.Cwd,
    GitStatus:       gitSnap,
}
```

**stamp condition 扩展**：现有 condition 是 `usage 或 model 或 git 至少一个非空`。新增 `|| s.CompactionCount() > 0`——这样即使前 3 个都还没拿到，count 也能让 footer Line 1 显示出来。但实际上 compaction 必然发生在至少 1 个 turn 之后，所以这条 OR 几乎不会触发（除非 `/new` 后立刻发生 compaction，罕见）。

### 1.9 OutCompaction kind 删除

**为什么删**：
- runtime handler 不再产生 OutboundMessage for EventCompaction（§1.8）
- gateway.translate.go 不再有 `case agent.EventCompaction:` 分支
- 没有任何 producer，自然没有 consumer

**删什么**：
- `internal/gateway/messages.go` `OutboundKind` 常量删除 `OutCompaction`
- `internal/channel/feishu/adapter.go` `Send` case `OutCompaction` 删除
- `internal/channel/feishu/receipt_event.go` `eventToEntry` 对 EventCompaction 的 `(_, false)` 分支删除
- `internal/channel/feishu/receipt.go` `Append` 对 EventCompaction 的 silent PATCH 分支删除
- 文档删除所有 OutCompaction 引用（F-37 §2.1 表行；F-25 §3.1.1 thread reply 行；F-25 §2.4 silent PATCH 段）

**为什么不是保留 OutCompaction + runtime 不发**：
- 死代码——没有 producer 的 kind 就是死代码
- 删干净符合 "future 再说" 原则（要加字段 / 新行为时再加新 kind）

### 1.10 CompactionEvent / AgentEvent 字段删除

**`internal/agent/agent.go`**：

```go
// 改动前
type CompactionEvent struct {
    Subtype string
}

type AgentEvent struct {
    Kind EventKind
    // ...
    Compaction *CompactionEvent  // ← 删
    // ...
}

// 改动后
// CompactionEvent 是 EventCompaction 的 payload —— 当前没有字段，
// 纯粹作为 marker 存在。Bridge 各自负责把协议差异消化成"一个
// EventCompaction = 一次完成的压缩周期"。未来要加字段（如压缩后
// token 数）时在此扩展。
type CompactionEvent struct{}

type AgentEvent struct {
    Kind EventKind
    // ...
    // Compaction 字段删除 —— runtime 不再基于此指针判别类型，
    // Kind == EventCompaction 已是唯一判别依据。
    // ...
}
```

**为什么不留 `Compaction *CompactionEvent` 指针作为 marker**：
- 字段无值（空 struct 指针只是 `Kind` 的冗余表示）
- 删掉减少字段扫描，让 `AgentEvent` 更紧凑
- 未来真要加字段时一并加（避免现在留个空 marker 后续还要改）

**Bridges 同步修改**：
- Pi translate.go：`Kind: agent.EventCompaction`（不再附 `Compaction: &agent.CompactionEvent{}`）
- Claude Code stream.go：同上

---

## 2. 文件 & 接口

### 2.1 `internal/agent/agent.go`

**改动 A**：`CompactionEvent` 删除 `Subtype` 字段，变空 struct。
**改动 B**：`AgentEvent` 删除 `Compaction *CompactionEvent` 字段。
**改动 C**：`EventKind.String()` 对 `EventCompaction` 返回 `"compaction"` 不变（仅 debug）。

### 2.2 `internal/bridge/pi/translate.go`

**改动**：`compaction_start` case `return nil, nil`；`compaction_end` case 去掉 `Compaction:` 字段。

### 2.3 `internal/bridge/claudecode/stream.go`

**改动**：emit `EventCompaction` 时不再赋值 `Compaction: &CompactionEvent{Subtype: ...}`。

### 2.4 `internal/chatsession/agentsession.go`

**改动 A**：struct 加 `compactionCount int` 字段（由 `cumulativeUsageMu` 守护）。
**改动 B**：加 `RecordCompaction()` 和 `CompactionCount() int` 方法。
**改动 C**：`ResetCumulative` 同时清零 `compactionCount`。
**改动 D**：`Entry()` 拷出 `compactionCount`。
**改动 E**：`FromAgentSessionEntry` 还原 `compactionCount = e.CompactionCount`（默认 0 兼容老数据）。

### 2.5 `internal/registry/agent_session_entry.go`

**改动 A**：`AgentSessionEntry` 加 `CompactionCount int json:"compactionCount,omitempty"`。
**改动 B**：`AgentSessionFileVersion` +1。

### 2.6 `internal/gateway/messages.go`

**改动 A**：`SessionContext` 加 `CompactionCount int json:"compactionCount,omitempty"`。
**改动 B**：`OutboundKind` 删除 `OutCompaction` 常量。

### 2.7 `internal/gateway/translate.go`

**改动**：删除 `case agent.EventCompaction:` 分支（不再产生 OutboundMessage）。

### 2.8 `cmd/nightme/run.go::newEventHandler`

**改动 A**：`case agent.EventCompaction: s.RecordCompaction(); logger.Debug(...)`——**不**走 `gateway.Translate`，**不**产生 OutboundMessage。
**改动 B**：`sessionContextInto` stamp `CompactionCount`。
**改动 C**：stamp condition 加 `|| s.CompactionCount() > 0`（理论上不会触发但保持对称）。

### 2.9 `internal/channel/feishu/usage_footer.go`

**改动**：Line 1 末尾追加 `· 🗜 N`（仅 N>0）。需要 import `strconv`。

### 2.10 `internal/channel/feishu/adapter.go`

**改动**：删除 `Send` case `gateway.OutCompaction` 分支（不再有 OutCompaction kind）。

### 2.11 `internal/channel/feishu/receipt_event.go`

**改动**：删除 `eventToEntry` 对 `agent.EventCompaction` 返回 `(_, false)` 的分支。

### 2.12 `internal/channel/feishu/receipt.go`

**改动**：删除 `Append` 对 `agent.EventCompaction` 的 silent PATCH 分支（不 bump eventCount / lastEventAt）。

---

## 3. 测试

### 3.1 单元测试

| 测试 | 文件 | 覆盖 |
|------|------|------|
| `TestRecordCompaction_BumpsCount` | `internal/chatsession/agentsession_meta_test.go` (EXTEND) | 调一次 RecordCompaction → CompactionCount() == 1；调两次 → == 2 |
| `TestRecordCompaction_ResetsTokens` | 同上 | 先 AccumulateUsage(5k in + 2k cache + 1k out + $0.05) → RecordCompaction → CumulativeUsage().InputTokens=0, CacheCreation=0, CacheRead=0, Output=0, **CostUSD=$0.05** |
| `TestRecordCompaction_Race` | 同上 | 100 goroutine × 1000 RecordCompaction → 最终 CompactionCount() == 100000 + race detector clean |
| `TestResetCumulative_ClearsCount` | 同上 | 累加 + RecordCompaction → ResetCumulative → CompactionCount() == 0 + CumulativeUsage 全零 |
| `TestEntry_RoundtripPreservesCount` | 同上 | Entry() → JSON marshal → unmarshal → FromAgentSessionEntry → CompactionCount() == 原值 |
| `TestFormatSessionFooter_Line1_WithClamp` | `internal/channel/feishu/usage_footer_test.go` (EXTEND) | SessionContext{Agent:"claude", Model:"opus", CompactionCount:3} → line 1 == "🤖 claude · opus-4-5 · 🗜 3" |
| `TestFormatSessionFooter_Line1_NoClamp` | 同上 | CompactionCount:0 → line 1 == "🤖 claude · opus-4-5"（无 � 段） |
| `TestFormatSessionFooter_Line1_CostAfterReset` | 同上 | 模拟累积后压缩：footer line 2 = "💰 ↓ 5k · ↻ 2k · ↑ 0.8k · 7.8k · $1.245"（cost 保留，token 归零） |
| `TestPiBridge_SuppressesCompactionStart` | `internal/bridge/pi/translate_test.go` (EXTEND) | 输入 `compaction_start` event → 返回 `nil, nil`；输入 `compaction_end` → 返回 1 条 EventCompaction |
| `TestClaudeCodeBridge_EmitsEmptyCompaction` | `internal/bridge/claudecode/stream_test.go` (EXTEND) | result subtype "compact" → emit 1 条 EventCompaction（无 Subtype 字段可断言） |
| `TestTranslate_NoOutboundForCompaction` | `internal/gateway/translate_test.go` (EXTEND) | 调 Translate(EventCompaction) → 返回 `nil, nil`（无 Outbound 产生） |
| `TestFeishuAdapter_NoOutCompactionCase` | `internal/channel/feishu/adapter_test.go` (EXTEND) | 验证 `OutCompaction` case 已删除（grep 反向断言） |
| `TestNewEventHandler_BumpsOnCompaction` | `cmd/nightme/run_test.go` (EXTEND) | 注入 EventCompaction → s.CompactionCount() == 1 + 无 Outbound 发送到 channel |

### 3.2 集成测试

| 测试 | 文件 | 覆盖 |
|------|------|------|
| `TestPiBridge_OneCycle_OneCompactionCount` | `internal/bridge/pi/translate_test.go` | 完整模拟一次压缩周期（compaction_start + compaction_end）→ runtime 收到 1 条 EventCompaction → CompactionCount == 1 |
| `TestRuntime_FullCycle_BumpAndReset` | `cmd/nightme/run_test.go` | 累加 5 个 turn usage → EventCompaction → 下一个 EventUsage 只加新值（不叠加旧的） → footer Line 2 显示"压缩后"数字 |
| `TestRuntime_FullCycle_CostPreserved` | 同上 | 同上 + cost 跨压缩累加不变 |

### 3.3 边界测试

| 测试 | 场景 |
|------|------|
| `TestCompactionEvent_NoFields` | `agent.CompactionEvent{}` 空 struct 编译通过 + `agent.CompactionEvent{} == agent.CompactionEvent{}` |
| `TestAgentEvent_NoCompactionField` | `agent.AgentEvent{Kind: EventCompaction}` 编译通过（不依赖已删除字段） |
| `TestLegacyAgentSessionEntry_ZeroCount` | 老 JSON 文件无 `compactionCount` 字段 → FromAgentSessionEntry → CompactionCount() == 0 |
| `TestFooter_NoNegativeCount` | CompactionCount < 0 永不会出现（runtime 不产生负值；老数据 0；新累加 +1） |

---

## 4. Migration & 兼容性

### 4.1 JSON 兼容性

**`agent_sessions.json`**：
- 旧文件无 `compactionCount` 字段 → Go JSON unmarshal 容忍 → 内存里 `int == 0` → footer 不显示 🗜 段（符合零值约定）
- 第一次写入新字段后，新文件包含 `compactionCount`——向后兼容（读端都容忍缺失）
- `AgentSessionFileVersion` +1：标记 schema 升级，但**读端不强制版本检查**（沿用 F-45 的宽容策略）

### 4.2 Wire 兼容性

**`OutboundMessage.Kind`**：
- **删除 `OutCompaction` 常量**——这是破坏性变更。但 Channel 实现都是内部编译（Feishu adapter），无外部 wire consumer
- Echo channel 同样无 OutCompaction 处理代码（grep 确认）
- 编译期保证：删除 OutCompaction 后所有 switch case 都更新（`go build` 会报错）

**`SessionContext.CompactionCount`**：
- 新增字段（`omitempty`）——Channel 不读时零影响
- Feishu adapter 读 → 渲染 Line 1 🗜 段

**`AgentEvent.Compaction` 字段**：
- **删除**——任何引用此字段的代码编译失败。grep 全仓库确保只有 bridge / translate 引用
- bridges 同步删除 `Compaction: ...` 赋值

### 4.3 bridge 协议

- **Pi 行为变更**：`compaction_start` 不再产生 EventCompaction（之前产生 `EventCompaction{Subtype:"start:..."}`）
  - 兼容性：如果未来 Pi 改回只发 start 不发 end，runtime 会漏数；当前 Pi 协议保证两条都发，无问题
  - 文档更新：`docs/bridge/pi.md §2.3` wire translation 表更新
- **Claude Code 行为变更**：emit EventCompaction 时不再带 Subtype 字段（字段已删除）
  - 兼容性：Subtype 仅做 debug，无功能性副作用

### 4.4 runtime handler 行为

- **新增副作用**：EventCompaction 会触发 AgentSession 累加（包括内存写 + dirty flag）+ PersistIfDirty 后续落盘
- **删除副作用**：EventCompaction 不再产生 OutboundMessage——channel 不再发任何 thread reply / receipt update / "Compacting…" marker
- 用户视角：从"看到 transient 提示"变成"footer Line 1 数字安静增长"

---

## 5. 不变式总结

**F-49 删字段 + 加 counter + 改 footer，但保留**：

### 5.1 抽象 / 具体边界（SPEC §1.4 强制）

- ✅ **bridge 消化协议差异**——Pi 屏蔽 start、Claude Code 自然 1 条、runtime 一视同仁
- ✅ **runtime 不基于 Subtype dispatch**——Subtype 字段不存在，runtime 不可能 string-sniff
- ✅ **Channel 不感知协议**——Feishu adapter 只读 SessionContext.CompactionCount，不知道它来自 Pi 还是 Claude Code

### 5.2 F-45 footer 不变式

- ✅ **Footer 仍是 3 行结构**：Line 1 identity + Line 2 tokens/cost + Line 3 git
- ✅ **Line 1 仍是 `🤖 <Agent> · <Model>` 起手**——🗜 段是 append，不是 replace
- ✅ **每段独立 omit**——零值不显示（🗜 在 count==0 时不显示）
- ✅ **`· ` middle-dot 分隔符**——与 F-37/F-44/F-48 一致

### 5.3 Channel / Runtime / Bridge 职责

- ✅ **Bridge 是 EventCompaction 的唯一 producer**——runtime 不造 EventCompaction
- ✅ **Runtime 是 AgentSession 元数据的唯一 owner**——RecordCompaction 只在 runtime handler 调
- ✅ **Channel 是 footer 渲染的唯一 owner**——runtime 只 stamp SessionContext，不直接拼字符串
- ✅ **Channel 不调 git、不算 token、不调 RecordCompaction**——保持 F-08 "Channel is dumb"

### 5.4 持久化

- ✅ **AgentSession 是 metadata 的唯一持久化载体**——compactionCount 跟 cumulativeUsage 同源（同一个 struct，同一把锁）
- ✅ **`/new` 是唯一清零入口**——compactionCount 与 cumulativeUsage 一同清零
- ✅ **daemon 重启不丢**——`FromAgentSessionEntry` 还原

### 5.5 1 turn : 1 anchor 不变式

- ✅ **OutCompaction 整条 path 删除**——之前 F-37 给 OutCompaction 发 thread reply，违反 1 turn : 1 anchor 不变式的精神（一个 turn 中间突然多一条 thread message）；删掉后更干净
- ✅ **EventCompaction 不再产生 thread reply / receipt entry / state reaction**

### 5.6 OutboundKind 集合

- ✅ **不增**——F-49 删除 OutCompaction，kind 集合净减少 1 个
- ✅ **未来要发 "压缩进行中" 提示时**——可以新增 OutCompaction 或别的 kind，但目前不需要（用户明确说"不用做进过程的显示"）

---

## 6. 不在本 PR 范围

- **`/cost` slash command**——展示 lifetime cost + since-last-compaction 拆分；后续 PR
- **per-model breakdown**——Anthropic API `modelUsage` map 展开成 multi-line footer；后续 PR
- **ChatSession-level 总计**——pool 内所有 AgentSession 之和；后续 PR
- **model → max_context 静态表**——可加 Line 2.5 显示"5k / 200k · 22%"，但需要查 model metadata；后续 PR 单独讨论
- **"压缩进行中"瞬时提示**——用户明确不需要；若未来需要，新增 OutboundKind 而不是复活 OutCompaction

---

## 7. 实施计划

按 8 个独立 commit 顺序落地，每步可单独 revert：

1. **`feat(agent): delete CompactionEvent.Subtype + AgentEvent.Compaction`**
   - `internal/agent/agent.go` 删 Subtype + Compaction 字段
   - bridges 同步删 `Compaction: &CompactionEvent{...}` 赋值
   - `internal/bridge/pi/translate.go` 屏蔽 compaction_start

2. **`feat(chatsession): AgentSession.CompactionCount + RecordCompaction`**
   - struct 加 `compactionCount int`
   - 加 `RecordCompaction()` / `CompactionCount()`
   - `ResetCumulative` / `Entry` / `FromAgentSessionEntry` 同步

3. **`feat(registry): AgentSessionEntry.CompactionCount + file version bump`**
   - 加字段
   - `AgentSessionFileVersion` +1

4. **`feat(gateway): SessionContext.CompactionCount + remove OutCompaction kind`**
   - `SessionContext` 加字段
   - `OutboundKind` 删除 `OutCompaction` 常量
   - `gateway.translate.go` 删除 `case agent.EventCompaction:`

5. **`feat(runtime): newEventHandler RecordCompaction + stamp CompactionCount`**
   - handler case EventCompaction → RecordCompaction
   - `sessionContextInto` stamp
   - stamp condition 扩展

6. **`feat(feishu): footer Line 1 🗜 N + remove OutCompaction adapter case + remove receipt entry`**
   - `usage_footer.go` Line 1 末尾追加
   - `adapter.go` Send 删 OutCompaction case
   - `receipt_event.go` 删 EventCompaction 分支
   - `receipt.go` 删 Append silent PATCH

7. **`docs(SPEC): F-49 changelog`**
   - `docs/SPEC.md` 加 F-49 增量变更摘要

8. **`docs(feat): F-45 §1.8 follow-up + F-32 bridge behavior + F-37 remove thread route + F-25 remove receipt entry`**
   - `docs/channel/feishu-rendering.md` 加 §1.8
   - `docs/bridge/pi.md` 更新 §2.3 wire translation 表
   - `docs/channel/feishu-rendering.md` 删 §2.1 OutCompaction 行 + §3.4 silent PATCH 段
   - `docs/channel/feishu-rendering.md` 删 §3.1.1 OutCompaction thread reply 行 + §2.4 silent PATCH 段
   - `docs/channel/feishu-rendering.md` 加 F-49 decision

---

---

## A13. F-55: Footer 显示 `(<window>)` 让用户自己判断窗口值

> **Source**: `../channel/feishu-rendering.md`


> **Depends**: [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md) (footer 第二行 X% 渲染), [`../bridge/pi.md`](./../bridge/pi.md) (bridge-local 取 `contextWindow`)
> **Related**: [`../bridge/claude.md`](./../bridge/claude.md) (ClaudeCode `decodeUsage` 行为), [`../bridge/pi.md`](./../bridge/pi.md) (Pi `get_state`)

## 0. 摘要

**一件事**:footer 第二行 `X%` 段后面追加 `(window)`,把分母一并展示,让用户自己判断 `pct > 100%` 时窗口值是不是上游兼容端(`MiniMax`、代理、Bedrock 转发等)报错了。

**为什么不做查表 / override / clamp**:nightme 不维护模型目录表,F-54 已经把窗口值定位成"CLI/Agent 上游报什么就是什么";再加 hybrid 信任分级会把架构复杂度推给用户,而多数用户其实只需要看到分母就能自行判断。

**具体格式**:

```text
💰:「 202.5k / 603 · 101.6% (200k) · $0.520 」
```

```text
💰:「 202.5k / 603 · 20.3% (1.0M) · $0.520 」
```

`X% (window)` 是一个语义单元,中间一个空格。

## 1. 背景与决策

### 1.1 现状

`internal/agent/agent.go:400` 上 `UsageEvent.ContextWindowPct` 是 bridge-local 算出来的:

```text
pct = (input + output + cache_creation + cache_read) / contextWindow * 100
```

`contextWindow` 来自子进程 wire 字段:

- ClaudeCode:`result.modelUsage[<model>].contextWindow` (`internal/bridge/claudecode/stream.go:685`)
- Pi:`get_state.data.model.contextWindow` (`internal/bridge/pi/protocol.go:111`)

F-54 §1.2 明确把 `agent.UsageEvent.ContextWindow` 字段删了,理由是"全 codebase 0 read / 0 write,bridge-local 算完即丢"。但 footer 渲染时用户其实**想知道**这个分母——`pct = 101.6%` 时,如果不显示分母,用户无法判断是模型真的吃满了 200K,还是 MiniMax 这种兼容端把 1M 模型错报成 200K。

### 1.2 决定

恢复 `agent.UsageEvent.ContextWindow int` 字段,语义保持 F-54 §1.2 的"bridge-local 透传"——nightme 自己**不**基于它做任何 decision(不重算 pct、不查表、不 clamp、不告警)。它跨过 bridge struct 边界,目的是让 footer 渲染时能给用户看到分母。

**nightme 不做的三件事**(明确否决):

1. ❌ 维护模型 → 窗口查表(避免引入 `anthropic-models-2026-06-24` 之类的 catalog,以及"Nightly 拉 `/v1/models` 校准"之类的运维负担)
2. ❌ 配置 override(避免 `~/.nightme/config.yaml` 里出现 `agents.contextWindow: 1000000` 之类的 hack)
3. ❌ `pct > 100%` 时 clamp 或告警(让用户看到原始事实;clamp 会把上游 bug 隐藏)

**一句话立场**:CLI Agent 报什么就显示什么,错了让用户自行计算。

### 1.3 受影响的显示格式

**当前 footer 第二行**(`internal/channel/feishu/usage_footer.go:183`):

```text
💰:「 in / out · X% · $cost 」
```

**改后**:

```text
💰:「 in / out · X% (window) · $cost 」
```

`(window)` 与 `X%` 中间一个空格,与现有 `· $cost` 一致用 `·` 分隔;括号包裹 `window` 是为了让用户一眼看出"这是分母,不是百分比继续累积"。

## 2. 设计

### 2.1 字段变更

```diff
 type UsageEvent struct {
     InputTokens              int
     OutputTokens             int
     CacheCreationInputTokens int
     CacheReadInputTokens     int
     CostUSD                  float64
+    ContextWindow            int
     ContextWindowPct         float64
 }
```

- `UsageInfo` 也同步加 `ContextWindow int`(二者字段表保持一致,见 F-52 §2.4)。
- 字段语义:**bridge-local 透传,仅供 footer 渲染**。runtime 不重算,不查表,不基于它做决策。
- 不引入 `ContextWindowSource string`("wire" / "catalog" / "override" 之类的来源标记)——本次刻意不增加信任分级架构。
- 不引入 `ContextWindowReliable bool`——同理由。

### 2.2 Bridge 改动

**`internal/bridge/claudecode/stream.go decodeUsage`**(`stream.go:643`):

当前把 `contextWindow` 当本地变量用完即丢:

```diff
-    if contextWindow > 0 {
-        used := out.InputTokens + out.OutputTokens +
-            out.CacheCreationInputTokens + out.CacheReadInputTokens
-        if used > 0 {
-            out.ContextWindowPct = float64(used) / float64(contextWindow) * 100
-        }
+    if contextWindow > 0 {
+        used := out.InputTokens + out.OutputTokens +
+            out.CacheCreationInputTokens + out.CacheReadInputTokens
+        if used > 0 {
+            out.ContextWindowPct = float64(used) / float64(contextWindow) * 100
+        }
+        // F-55: 透传 window 给 footer,仅渲染用途,nightme 不基于它重算/查表/clamp
+        out.ContextWindow = contextWindow
     }
```

**`internal/bridge/pi/translate.go decodeMessageUsage`**(`translate.go:837`):

```diff
-    if ctxWindow > 0 {
-        used := u.Input + u.Output + u.CacheRead + u.CacheWrite
-        if used > 0 {
-            out.ContextWindowPct = float64(used) / float64(ctxWindow) * 100
-        }
+    if ctxWindow > 0 {
+        used := u.Input + u.Output + u.CacheRead + u.CacheWrite
+        if used > 0 {
+            out.ContextWindowPct = float64(used) / float64(ctxWindow) * 100
+        }
+        // F-55: 透传 window 给 footer,仅渲染用途,nightme 不基于它重算/查表/clamp
+        out.ContextWindow = ctxWindow
     }
```

**emitConnected / modelUsage map iteration**:本次不动。`for _, v := range m` 的 map 随机遍历顺序在单模型场景下没有可见影响(只有一个 entry);多模型场景里的小毛病留待后续单独 PR。本次改动只透传 window,不动取值逻辑。

### 2.3 Footer 渲染

**`internal/channel/feishu/usage_footer.go formatSessionFooterLines`**(`usage_footer.go:183`):

当前:

```go
if u.ContextWindowPct > 0 {
    usageParts = append(usageParts, fmt.Sprintf("%.1f%%", u.ContextWindowPct))
}
```

改后:

```go
if u.ContextWindowPct > 0 {
    usageParts = append(usageParts, fmt.Sprintf("%.1f%% (%s)", u.ContextWindowPct, abbrevWindow(u.ContextWindow)))
}
```

**新 helper `abbrevWindow`**(同 `usage_footer.go:242 abbrevTokens` 口径):

```go
// abbrevWindow 格式化模型上下文窗口:
//   < 1000          -> 数字 (如 "999")
//   1000 <= n < 1M  -> K 缩写 (如 "1.0k", "200k", "999k")
//   n >= 1M         -> M 缩写 (如 "1.0M")
//
// 与 abbrevTokens 完全同款 (后者用于 in/out token 计数),
// 抽出来是为了 footer 渲染可读,不混淆"窗口大小"与"token 数"。
func abbrevWindow(n int) string {
    switch {
    case n >= 1_000_000:
        return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
    case n >= 1_000:
        return fmt.Sprintf("%.1fk", float64(n)/1_000)
    default:
        return fmt.Sprintf("%d", n)
    }
}
```

**渲染规则**:

| `ContextWindowPct` | `ContextWindow` | 渲染 |
|---|---|---|
| `== 0` | `== 0` | 不显示 X% 段(沿用 F-45 §1.6 zero-omit) |
| `== 0` | `> 0` | 显示 `(200k)` 不带 X%(理论上不该发生:pct==0 时 window 也应==0;但保留兜底) |
| `> 0` | `== 0` | 只显示 `X%`(沿用 F-54,极少见,模型未识别窗口) |
| `> 0` | `> 0` | 显示 `X% (window)` |
| `> 100` | `> 0` | 显示 `X% (window)`,**不 clamp 不告警** |

### 2.4 不动的东西

- ❌ `UsageEvent` / `UsageInfo` 上**不**新增 `ContextWindowSource` / `ContextWindowReliable` 之类的 trust-tier 字段(见 §1.2)
- ❌ 不维护模型 → 窗口查表
- ❌ 不加 `/v1/models` 拉取逻辑
- ❌ 不加 config override
- ❌ 不改 `emitConnected` 的 window 缓存语义
- ❌ 不改 pct 公式
- ❌ 不改 `pct > 100%` 的现有零 clamp / 零告警行为
- ❌ 不动 `claudecode/stream.go decodeUsage` 的 `modelUsage` map iteration(留待单独 PR)

## 3. 影响

| 维度 | 估 |
|---|---|
| 字段变更 | +1 (`UsageEvent.ContextWindow`,`UsageInfo.ContextWindow` 同步) |
| 代码净增 | +15 / -2 (bridge 透传 + footer helper + 测试) |
| 涉及文件 | 5 (agent.go, claudecode/stream.go, pi/translate.go, feishu/usage_footer.go, 对应测试) |
| 测试改动 | claudecode / pi 各自加 ~1 case 验证 `out.ContextWindow` 透传;footer 加 ~3 case 覆盖三种渲染规则 |
| 持久化 schema | 不变(`ContextWindow` 不进 registry,只在 `SessionContext.Usage` 内存态) |
| Footer UX | 第二行 X% 段从 `20.3%` 变成 `20.3% (1.0M)`,用户可读分母;`pct > 100%` 时上下文一目了然(`101.6% (200k)` 让用户立刻看出窗口元数据可疑) |

## 4. 风险

- **括号里的 `200k` / `1.0M` 可能与上游实际能力不一致**:这是本次刻意保留的——让用户看到原始事实,自己判断。如果后续用户反馈"太长了看不清"再考虑 trust-tier(本次不做)。
- **测试 fixture 里 mock 桥发的 `modelUsage.contextWindow: 200000` 不变**:frozen snapshot,F-54 当时就这么写了;改后测试照旧。
- **`SessionContext.Usage` 多了一个 `ContextWindow` 字段**:runtime 通过 `OutboundMessage.Usage` 接收这条数据,文档已注明"runtime 不基于它重算"。runtime 唯一消费方是 footer 渲染。
- **delete 字段的反悔**:F-54 §1.2 删 `ContextWindow` 的理由是"全 codebase 0 read / 0 write"。本次恢复后字段有了**唯一**消费方(footer)。说明:`pct > 100%` 排查场景下,这就是用户需要的诊断信号;如果不显示分母,排查成本反而更高。

## 2.5 后续:把 `in` 拆成 `new` + `cache`(F-55.1, 2026-08)

**问题**: 用户反馈一张 footer 显示 `13.7M / 21.3k · 1374.5%`,13.7M input 远超模型窗口(1M),看起来像累计 bug。但官方文档明确三个 input-side 字段**互斥**(没有重复计数);实际 13.7M 全在 `cache_read_input_tokens` 里——上游兼容端把 cache hit 报成了 session 累计。

**做法**: Footer 第二行把 `in` 拆成两段:

```text
new   = input_tokens + cache_creation_input_tokens   // 本轮新增,不命中缓存
cache = cache_read_input_tokens                       // 命中缓存
out   = output_tokens
```

**渲染**:

```text
💰:「 new / cache / out · X% (window) · $cost 」
```

每段独立按 `> 0` omit(F-45 §1.6),纯数字,**无 label**。`cache == 0` 时退回原 `new / out` 布局。

**实例**(用户截图):

```text
💰:「 1.2k / 13.0M / 21.3k · 1374.5% (200.0k) · $24.422 」
```

中间段 `13.0M` 立刻告诉用户"这一轮 13M 是缓存命中,新内容只有 1.2k"。如果是 MiniMax 这种把 cache_read 报成累计的上游,这个数字会**非常醒目**——和 F-55 立场一致:CLI Agent 报什么显示什么,用户自行判断。

**不变**:
- pct 仍按 Doc 1 公式:`(new + cache + out) / contextWindow`(没改)
- `(window)` 仍按 F-55 显示
- `pct > 100%` 不 clamp 不告警
- nightme 不查表、不 override、不重算

**改动**:
- `internal/channel/feishu/usage_footer.go` `formatSessionFooterLines`: 行内 `in` 拆 `new` + `cache`
- `internal/channel/feishu/usage_footer_test.go`: `TestFormatSessionFooterLines_OmitsZeroSegments` 等几个 case 更新 + 加 2 个新 case(cache-only / cache+out)
- `docs/channel/feishu-rendering.md` Line 2 表格: 把 `in / out` 一行拆 `new` + `cache` + `out`
- `docs/SPEC.md`: Line 2 段规则同步

---

## A14. Feishu Channel - 调研与迁移规划

> **Source**: `feishu.md`


## 1. 背景:为什么从 text 切到 card

当前 nightme 的 receipt 走纯文本路径(`msg_type: "text"`):
- `internal/channel/feishu/adapter.go:890 SendMessageText` 编码为 `{"text": "..."}` 后 `sendContent` 发出
- `internal/channel/feishu/receipt.go:515 renderLocked` 每个事件 `SendMessageText` 发一条新消息
- 没有 footer / 没有按钮 / 表格需要 markdown hack 才能近似

切到 interactive card 可以:
1. **footer 行** - 展示 `Agent · X | Model · Y | Provider · Z` 这类元数据(对齐 OpenClaw 风格,见 §2)
2. **按钮 / action** - 一键确认、二次确认、复制 session id 等交互
3. **结构化展示** - 工具调用折叠为 `collapsible_panel`、长输出折叠、表格、彩色状态等
4. **原地更新** - 用 PATCH API 改一张卡,而不是发 N 条消息,降低噪音

## 2. OpenClaw 的"card note footer" 调研

### 2.1 这是什么

OpenClaw 在每张 Feishu 卡片底部加一行灰色文字,展示当前 agent 的身份与运行上下文:

```
Agent: main | Model: glm-5 | Provider: tencentcodingplan
```

不是用户写的"备注",而是**运行时自动生成**的卡片 footer。

### 2.2 实现来源(OpenClaw)

- **生成位置**: `extensions/feishu/src/reply-dispatcher.ts::resolveCardNote`
  - 引入 commit: https://github.com/openclaw/openclaw/commit/df3a247db2a90da2a2593f85bdd5ef07f6b39a91
  - JSDoc: "Build a card note footer from agent identity and model context."
  - 硬编码拼接 `Agent: <name>` + ` | Model: <model>` + ` | Provider: <provider>`
  - **不可配置**(issue #59360 指明: "There is no configuration to disable it")
- **传入路径**:
  - 流式卡 start/close: `reply-dispatcher.ts:393, :437` 接收/重算 `note`
  - 结构化卡: 传入 `sendStructuredCardFeishu(..., { note: cardNote })`
- **渲染位置**: `extensions/feishu/src/send.ts:768`
  ```ts
  if (options?.note) {
    elements.push({ tag: "hr" });
    elements.push({ tag: "markdown", content: `<font color='grey'>${options.note}</font>` });
  }
  ```
  即:`<hr>` + 灰色 markdown。这是 v2 card 风格。

### 2.3 OpenClaw issue #59360 - root cause

- **Title**: "Feishu card message footer causes agent name to appear at message start (Markdown definition list parsing)"
- **现象**: Feishu 的 Markdown 渲染器把 `Agent: main | Model: ... | Provider: ...` 解析成 **Markdown 定义列表**(`key: value`),把第一项的 value(`main`)hoisting 到消息开头
- **结果**: 用户看到的卡片正文最上面突然多一行 `main`(agent 名),footer 反而被解读成普通段落
- **复现**: 发送任意包含 `Key: value | Key2: value2` 的灰色 markdown 卡片即可触发
- **关闭状态**: closed as not planned, 2026-07-20
- **未合并修复**: [PR #84122](https://github.com/openclaw/openclaw/pull/84122) - 把 `Agent: ` 改成 `Agent · `(中点),让渲染器认不出是定义列表
  - 描述: "Feishu's card markdown renderer parses 'Agent: name' as definition-list syntax and hoists the agent name to the top of the rendered message. Switch the key/value separator from ': ' to ' · ' so the footer stays in the footer."

### 2.4 我们的截图

用户截图(2026-06-11)中的红色框:

```
─────────────────────────────────
Agent: main | Model: MiniMax-M2.7 | Provider: minimax
```

-- 与 OpenClaw 的 card note footer 一致,但 nightme 当前的 text 路径**不会**输出这种卡片(没 hr、没灰色),所以截图来自**别的工具**(可能是 OpenClaw/同款渲染)。**这个 bug 在 nightme 切到 card 之前不会触发**;切换后必须规避(见 §6)。

## 3. Feishu 卡片 schema 摘要

### 3.1 顶层信封(参考 create_json 文档)

```json
{
  "msg_type": "interactive",
  "card": {
    "header": { "title": { "tag": "plain_text", "content": "..." }, "template": "blue" },
    "config":  { "wide_screen_mode": true, ... },
    "elements": [ ... ]
  }
}
```

### 3.2 footer / note 的两种实现

| 方案 | JSON | 兼容性 |
|---|---|---|
| **v1 `note` element** | `{ "tag": "note", "elements": [{ "tag": "plain_text", "content": "..." }] }` | Card v1,大多数场景可用;Card 2.0 官方组件列表里**没有**该 tag |
| **v2 hr + markdown + inline color** | `elements.push({tag:"hr"}); elements.push({tag:"markdown", content:"<font color='grey'>line1</font>\n<font color='grey'>line2</font>"});` | Card 2.0,**本项目采用的方案** |

**颜色语法实测结论**(PR #52 验证):
- `<font color='...'>` 在 `<markdown>` 标签内**有效** —— openclaw-lark (`src/card/builder.ts::buildFooter`) 用的就是这个,且实测生产可用。
- 颜色值必须用**命名色**: `grey`、`red`、`green`、`turquoise`、`blue`、`yellow`、`orange`、`purple`、`neutral`、`carmine` 等。**不支持 hex**(`#999999` 会被 Feishu 直接拒绝,报错 `230099 invalid color`)。
- `<text_tag color='neutral'>` 也是有效语法,但**只对 plain_text 的 `text_color` 字段生效**,在 `<markdown>` 内联语法里不支持 —— §6.2 的旧结论基于这个误解,已修订。
- 不要把 footer 用独立的 `<plain_text>` 元素 + `text_color` 字段渲染 —— `text_color` 字段只接受 `grey-100`/`grey-500` 等具名调色板 token,不接受命名色,且与 `<font color>` 内联色重叠。**统一用 `<markdown>` + `<font color='grey'>`** 是最简单、最可控的方案。

参考: `openclaw/openclaw/extensions/feishu/src/card/builder.ts::buildFooter` 是本项目最终采用的 v2 模式参考实现。本项目不再使用 v1 `<note>`。

### 3.3 常用元素

| tag | 用途 |
|---|---|
| `div` | 块容器,内嵌 `lark_md` / `plain_text` |
| `markdown` | Markdown 段(支持 `<font>` / `<at>` 等内联标签) |
| `hr` | 水平分隔线 |
| `note` | v1 footer 文字 |
| `action` + `button` | 按钮(action 数组元素) |
| `collapsible_panel` | 折叠面板(放长输出) |
| `image` / `img_combination` | 图片 |

### 3.4 更新策略

`PATCH /im/v1/messages/{message_id}` **整体替换** `card` 字段 -- 不能只改一个 element。所以 nightme 的"原地编辑 receipt"语义就是:**每次状态变化都重新构建完整 card body 然后 PATCH**。

**SDK 提醒**: `lark-oapi-go/v3` 提供两个不同的方法,**`Update` 只能改文本/富文本,不能改卡片**;**卡片必须用 `Patch`**。两个方法对应不同的 HTTP method:
- `Update` (PUT `/open-apis/im/v1/messages/:id`) - 仅文本/富文本。SDK 注释: "当前仅支持编辑文本和富文本消息"
- `Patch` (PATCH `/open-apis/im/v1/messages/:id`) - 卡片/富文本都支持,5 QPS 频控,30 KB body 上限

`update_multi` 不是独立接口,是 card `config` 里的一个 flag(`"update_multi": true`),让卡变成"共享卡"在所有接收方同步更新。nightme 单聊场景不启用。**点按者**看到下一张卡靠的是 `card.action.trigger` 回调里的 `card.type=raw`，不是这个 flag。交互卡完整规则见 [feishu-cards.md](./feishu-cards.md)。

## 4. nightme 当前现状(text 模式)

### 4.1 关键代码位置

| 文件 | 作用 |
|---|---|
| `internal/channel/feishu/adapter.go:890 SendMessageText` | 发 `msg_type: "text"` 消息,返回 messageID |
| `internal/channel/feishu/adapter.go:765 buildInteractiveCard` | 已有的 OutChoice 卡片构建(permission card),仅用于 OutChoice 路径 |
| `internal/channel/feishu/adapter.go:1066 sendContent` | 透传到 lark client,支持任意 msgType |
| `internal/channel/feishu/receipt.go:515 renderLocked` | 每个事件 `SendMessageText` 发新消息 |
| `internal/channel/feishu/receipt.go:455 formatLocked` | 构造 plain text body(header + entries) |
| `internal/channel/feishu/receipt.go:188 headerLine` | 单行 header(⏳ / 🔄 / ✅) |
| `internal/gateway/messages.go:162 OutChoice` / `:254 Card` | 已存在 interactive card 的抽象类型 |
| `internal/gateway/messages.go:182 OutInit` | 携带 `session_id` + `model`(无 `agent_name` / `provider`) |

### 4.2 现状(切到 card 之前)

- 用户看到的 receipt 是一连串**短文本消息**(`⏳ 等待中` / `🔄 工具: Bash` / `✅ 已完成`)
- 切到 card 后,这些短消息将合并为**一张可原地 PATCH 的卡片**
- 已有的 `OutChoice` 路径独立(permission card 走 `buildInteractiveCard` → `sendContent`),不影响 receipt 切换

## 5. 迁移方案:receipt → interactive card

### 5.1 目标

让 receipt message 是**一张 interactive card**:
- body 是滚动的 log entries(`div` + `markdown`)
- 末尾 `<hr>` + 中性色 footer(`markdown` + `<text_tag color='neutral'>`)
- header(可选): 状态 emoji + 时间戳(`✅ 已完成 12:34:56`)
- 状态变化/心跳通过 `PATCH im/v1/messages/{id}` 整体更新

**本期 minimal scope**: 不引入新元数据。card body 用现有 receipt 状态(state + entries + last event time)。agent_name / provider 透传 **延期** 到后续 PR(见 §9.4)。

### 5.2 接口与数据流

```
Adapter.Send(OutboundMessage)
   │
   ├── OutText / OutResult / OutUsage / OutToolStart / OutToolEnd / OutThinking / OutCompaction
   │     → receipt.Append(AgentEvent) → renderLocked(ctx) → SendCard(首次) / PatchMessage(后续)
   │
   ├── OutInit
   │     → receipt.Append(AgentConnectedEvent) → renderLocked → Patch(刷新 footer)
   │
   └── OutChoice (permission)
         → sendContent(interactive, buildInteractiveCard(...))   ← 不变
```

### 5.3 MessageReceipt 字段扩展

```go
type MessageReceipt struct {
    // ... 现有字段
    cardMsgID string  // 首次 SendCard 后记录;后续 PatchMessage 用它
}
```

**本期不**新增 `agentName` / `provider` / `model` 字段 -- foot note 全部从已有 state 字段组装(state.String() + eventCount + lastEventAt)。这样不需要触动 `agent.AgentConnectedEvent` / `gateway/translate.go` / `OutboundMessage.Meta` 任何上游。

`renderLocked` 替换为新的 card-first 策略:
- 第一次:`sendContent(chatID, MsgTypeInteractive, buildReceiptCard(r))` 拿到 messageID
- 之后:`PatchMessage(messageID, buildReceiptCard(r))` 整体替换

`buildReceiptCard(r)` 产出(伪代码,见 §9.3 真实签名):
```go
func buildReceiptCard(r *MessageReceipt) (string, error) {
    elements := []any{
        // header(状态 + 时间戳)
        {"tag": "div", "text": {"tag": "lark_md", "content": r.state.headerLine(r)}},
    }
    if r.evicted > 0 {
        elements = append(elements, map[string]any{
            "tag":     "markdown",
            "content": fmt.Sprintf("<text_tag color='neutral'>...(前 %d 条已省略)</text_tag>", r.evicted),
        })
    }
    for _, e := range r.entries {
        elements = append(elements, map[string]any{
            "tag": "div",
            "text": map[string]any{
                "tag":   "lark_md",
                "content": e.Icon + " " + e.Text,
            },
        })
    }
    // foot note: 用现有 state 数据;无内容时整段省略
    if note := r.state.footLine(r); note != "" {
        elements = append(elements, map[string]any{"tag": "hr"})
        elements = append(elements, map[string]any{
            "tag":     "markdown",
            "content": fmt.Sprintf("<text_tag color='neutral'>%s</text_tag>", note),
        })
    }
    card := map[string]any{
        "config":   map[string]any{"wide_screen_mode": true},
        "elements": elements,
    }
    env := map[string]any{"card": card}
    b, _ := json.Marshal(env)
    return string(b), nil
}

func (s ReceiptState) footLine(r *MessageReceipt) string {
    if r == nil { return "" }
    parts := []string{s.String()}
    if r.eventCount > 0 {
        parts = append(parts, fmt.Sprintf("%d entries", r.eventCount))
    }
    if !r.lastEventAt.IsZero() {
        parts = append(parts, r.lastEventAt.Format("15:04:05"))
    }
    return strings.Join(parts, " · ")
}
```

### 5.4 PATCH 路径

`internal/channel/feishu/adapter.go` 新增(复用 `sendContent` / `sendFunc` / `sendViaLark` 的 dispatch 模式):
```go
func (a *Adapter) UpdateMessage(ctx context.Context, messageID, content string) error
func (a *Adapter) SendCard(ctx context.Context, chatID, content string) (string, error)
```

`UpdateMessage` 调 `larkClient.Im.V1.Message.Patch(...)`(注意是 **Patch**,不是 Update -- Update 只能改文本)。`SendCard` 是 `sendContent(chatID, larkim.MsgTypeInteractive, content)` 的薄包装。

PATCH 失败时不降级为新消息 -- 简单实现,日志告警即可;下次 `renderLocked` 仍然 PATCH 同一个 messageID。

### 5.5 迁移步骤(本期)

1. **Phase 1 - adapter 层支持**: 加 `SendCard` / `UpdateMessage`(内部调 Patch),`buildReceiptCard` + `footLine` 静态实现
2. **Phase 2 - receipt 切换**: `renderLocked` 改为 first-send-then-PATCH;`MessageReceipt` 加 `cardMsgID`;`evictOverflowLocked` 扩展为字节+元素双约束
3. **Phase 3 - 测试更新**: `mockReceiptBot` 加 SendCard / PatchMessage stubs;`TestReceipt_PerEventFreshMessage` 改为断言 PATCH 行为;新增 `TestFootLine_*` / `TestBuildReceiptCard_*` 系列
4. **Phase 4 - 文档收尾**: 在 §11 记录本期落地状态;把"未做"留给 follow-up issue

**OutInit / `agent_name` / `provider` 透传** -- **DEFERRED**(见 §9.4)。当后续 PR 加这三个字段时,`buildReceiptCard` 只需要把它们 append 到 `footLine` 后面,不需要再次动 receipt / adapter 主体。

## 6. 已知坑(从 OpenClaw 学到)

### 6.1 冒号 → 中点(防 hoisting,前瞻要求)

参考 OpenClaw PR #84122。**所有 footer / card note 的 `key: value` 必须改成 `key · value`**,否则 Feishu 渲染器把 `key: value` 解析为 Markdown 定义列表,把第一项的 value hoisting 到卡片正文开头。

**本期 foot note 不会出现 `key: value` 形态**(内容是 `state · N entries · HH:MM:SS`),但**约束记在 §9.3 后续加 agent info 时必须遵守**。中点字符用 U+00B7(`·`),不是句号或星号。

### 6.2 ~~`<text_tag color='neutral'>` 而非 `<font color='grey'>`~~（已修订）

~~Feishu `lark_md` **不支持** `<font color='grey'>` ...~~ **这条结论是错的**(本项目最终用了 `<font color='grey'>`)。修订见 §3.2。简短版:
- `<font color='grey'>` 在 `<markdown>` 内**有效**且**推荐** —— openclaw-lark 同款
- `<text_tag color='neutral'>` 只在 plain_text 元素的 `text_color` 字段上有效,**不能放在 markdown 内联**
- 不支持 hex 颜色(`#999999` 直接报错);只能用命名色

### 6.3 SDK: Patch ≠ Update

`lark-oapi-go/v3` 提供的 `Message.Update`(PUT) **只支持文本/富文本**,**不能改卡片**。**改卡片必须用 `Message.Patch`**(PATCH)。两个方法签名相似,容易踩坑。详见 §3.4。

### 6.4 PATCH 是整体替换

`PATCH /im/v1/messages/{id}` 的 `card` 字段是**整个 card 对象**,不是 diff。要保留元素(折叠面板的展开状态等)需要在 client 端维护当前状态,然后每次 PATCH 把所有 elements 重新构建。本期 receipt 没有折叠面板,不受影响。

### 6.6 MessageState 与 Card 共存

MessageState(reaction emoji 轨道)与 Receipt(card body 轨道)解耦为两个独立的 channel 实现。

#### 6.6.1 两个轨道

| 轨道 | 源 | 抽象事件 | 渲染目标 | Feishu 实现 |
|---|---|---|---|---|
| **MessageState** | ChatSession lifecycle | `OutboundMessage{Kind: OutMessageState, Meta: {message_id, state}}` | **userMsgID** | `AddReaction(userMsgID, emoji_type)` |
| **Card Body** | Rolling-log receipt（Channel 自治）| `OutboundMessage{ReplyTo: userMsgID}` → Channel.Send → 内部 cold-create / PATCH | replyMsgID / cardMsgID | `SendCard / PatchMessage` |

两者**完全独立**:
- 一个失败不影响另一个 (MessageState 渲染失败仅 log warn,不阻塞 card body)
- 都按 userMsgID / chatID 索引,但服务不同语义
- 详见 [`channel/feishu-rendering.md`](../channel/feishu-rendering.md) 与 `SPEC.md §2.5`

#### 6.6.2 MessageState 渲染实现

```go
// internal/channel/feishu/adapter.go - Send dispatcher 新增 case
case gateway.OutMessageState:
    messageID, _ := msg.Meta["message_id"].(string)
    if messageID == "" {
        return errors.New("feishu: OutMessageState missing message_id")
    }
    state, ok := msg.Meta["state"].(receipt.MessageState)
    if !ok {
        return errors.New("feishu: OutMessageState missing state")
    }
    emoji := mapStateToFeishuEmoji(state)
    if emoji == "" {
        return nil    // 未知 state 静默 drop
    }
    _, err := a.AddReaction(ctx, messageID, emoji)
    return err
```

#### 6.6.3 state → emoji_type 映射

| `MessageState` | emoji_type(飞书预定义) | 用户视觉 |
|---|---|---|
| `StateReceived` | `OneSecond` | ⏳ |
| `StateForwarded` | `OnIt` | 🔄 |
| `StateDone` | `DONE` | ✅ |
| `StateError` | `THUMBSUP` | ❌ (closest 预定义 indicator) |

**重要**:必须用飞书预定义 `emoji_type` 标识符,不是 unicode。传 unicode `⏳` 给飞书 reaction API 返回 `99992354 data not found`(reaction service 只识别预定义集合)。

#### 6.6.4 内部 idempotency

`Adapter` 维护 `messageStates map[string]receipt.MessageState`(userMsgID → 上次渲染的 state)。同 state 跳过 AddReaction 调用,避免网络抖动。

#### 6.6.5 append-only 语义

飞书 reaction API 是 append-only:每次 AddReaction 加新 emoji,不删老 emoji。这意味着 ⏳ → 🔄 → ✅ 在用户消息上**堆叠**为 3 个 emoji,形成完整状态轨迹。这是飞书平台特性,channel adapter 不主动删。

如果未来需要删,实现 `OutMessageStateRemoved` 事件 + `DeleteReaction(msgID, reactionID)` 路径(参考 adapter.Send 中 OutReactionRemoved case 实现)。

#### 6.6.6 渲染失败

`AddReaction` 失败时 log warn,返回 error 由 caller(`gateway.OnMessageState`)处理。**永不阻塞** ChatSession lifecycle 或消息处理主流程。

#### 6.6.7 中间态 silent drop(F-42 简化)

**F-42 后**：Feishu adapter 自决只渲染 `StateDone` / `StateError` 两个终态，`StateReceived` / `StateForwarded` silent drop。

```go
// Send() OutMessageState case (F-42 后)
switch state {
case agent.StateReceived, agent.StateForwarded:
    return nil   // silent drop
}
// 仅 StateDone (✅ "DONE") / StateError (❌ "THUMBSUP") 走 AddReaction
```

**理由**:
- F-37 thread route 让 Tool/Think reply 持续在 thread 报 execution activity ── ⏳/🔄 是冗余信号
- 首 OutReply / OutTask 到达时 receipt 直接出,无需前置 reaction
- ✅/❌ 终态不可替代的确认,保留

`agent.MessageState` enum / `mapStateToFeishuEmoji` / `AddReaction` 实现均不变 ── 4 态契约是抽象层;Feishu adapter 在 channel 自治范围内选择哪些渲染。

### 6.7 30 KB body 限制(本期放宽)

Feishu card body 上限 **30 KB**(Create 和 PATCH 相同)。本期 `replyMaxBytes` 从 4 KiB 放宽到 **24 KiB**(留 6 KiB 头空间)。`evictOverflowLocked` 同时受字节和元素数(50)两个约束,先触发的先 evict。

### 6.8 元素数 50 限制

`body.elements` 上限 **50 个**。本期布局预算:1 header + 1 evicted marker(可选)+ ≤47 entries + 1 hr + 1 footer = 50。`evictOverflowLocked` 把 entries 限制为 ≤47,新事件到达时驱逐最老。

### 6.9 `note` 元素的 v2 兼容性

Card 2.0 官方组件列表**没有 `tag: "note"`**。我们走 v2 风格（`<hr>` + 中性色 markdown），**不要**用 v1 的 `note` 元素。

### 6.10 Mention 前缀 strip（F-watch 增量）

**问题**：飞书群聊里，@ bot 后的消息文本以 `@_user_N ` 开头的占位符表示 mention（Feishu SDK 中以 `Mentions[].Key` 形式出现在 `message.Content` 里）。如：

```
@_user_1 /cwd /tmp
```

`ParseCommand` 要求 `strings.HasPrefix(trimmed, "/")`（`internal/gateway/parser.go:36`），这条文本会以 `@` 开头，被判为 `ErrParseFailure` → slash command **拦截失败**。

**方案**：`handleMessage` 构造 `channel.Message` 前，strip 开头的 mention 前缀。

| 场景 | Text 原始 | Text strip 后 | HasMention |
|------|----------|--------------|------------|
| 群聊 @ bot | `@_user_1 /watch on` | `/watch on` | `true` |
| 群聊 @_all | `@_all /cwd /a` | `/cwd /a` | `true` |
| 群聊多个 mention 开头 | `@_all @_user_1 hello` | `hello` | `true` |
| 群聊无 mention | `hello bot` | `hello bot` | `false` |
| 群聊 mention 在中段 | `look at this @_user_1 bug` | `look at this @_user_1 bug`（不剥）| `true` |
| DM | `hello` | `hello` | `true`（DM 永远 true）|

**实现位置**：`internal/channel/feishu/adapter.go::handleMessage`，`extractAttachments` 之后、构造 `channel.Message` 之前。

```go
text, hasMention := stripAndDetectMention(
    text, message.Mentions, a.getBotOpenIDCached(), stringValue(message.ChatType),
)
```

**strip 规则**：
1. 只剥**开头**连续出现的 mention 前缀（循环跳过中间的非 mention 文本，例如 `@_all @bot hello` → `hello`）
2. 中段的 mention 不动（保留用户原始语义）
3. 前缀必须是 mention + 至少一个空白字符（空格 / Tab / 全角空格 / `\u00A0`），避免误删正文中以 `@` 开头的单词（但正文中以 `@_user_N` 开头的字串不会被误判，因为正文中不会出现在最前面）
4. `@_all` 始终 strip（无需 bot open_id）

**`hasMention` 计算**：
- DM（`chat_type == "p2p"`）→ **永远 `true`**
- group/topic_group → `mentions` 列表中含 bot open_id 或 `@_all` 时 `true`
- `chat_type` 为空 / 未知 → 默认 `true`（安全 fallback，宁可多处理）

> **DM 不变式（锁死）**：DM 消息 `HasMention` 必须永远是 `true`。这是 F-watch 的核心不变式 ——只有这条不变式成立，gateway dispatcher 才能放心地 drop 非 mention 群消息而不误伤 DM。由 `TestComputeHasMention_DMInvariant` （adapter 层）+ `TestDispatchInbound_WatchModeGate_DMInvariant`（gateway 层）两个测试锁死，任一 regressed 都会被 CI 拦住。

**bot open_id 获取**：调 SDK `a.larkClient.GetBotIdentity(ctx)`（`channel/channel.go:152`），30 分钟 TTL cache 由 SDK 内部管理。第一次消息进来 cache miss → 同步 fetch；后续命中 cache，零延迟。fetch 失败 → 记 log，`HasMention` 退化为 `false`（保守策略：DM/group 都当 group 处理）。

**ChatSession 侧接入**：`/watch on` / `/watch off` 控制 `ChatSession.WatchMode()`；Gateway dispatcher 拿 `Message.HasMention` + `cs.WatchMode()` 决定 drop 或 pass。Channel adapter **不读** `ChatSession` —— 详细职责划分见 `docs/SPEC.md §3.1.1`。

**测试覆盖**：
- 群消息 @bot / @_all → strip + HasMention=true
- 群消息无 mention → 不 strip + HasMention=false
- 群消息多 mention 串前 → 全剥
- 群消息 mention 中段 → 不动
- DM → 不 strip + HasMention=true（不调 bot identity）
- bot identity cache miss + fetch 失败 → fallback 到 HasMention=false + log warn

### 6.11 WatchMode per-chat 群消息全收（F-watch 增量）

**背景**：飞书默认 `im:message.group_at_msg:readonly` 只让 bot 收 @ 自己的消息。nightme F-watch 反转：bot 默认收全群（需要 `im:message.group_msg` scope，默认在 `DefaultAddons()` 里），由 `ChatSession.WatchMode` 在 nightme 侧决定要不要处理。

**实现位置**：
- `internal/chatsession/watchmode.go`：`WatchMode` 类型 + getter / setter（位于 `chat_session.go` → `chatsession.go` 重构后独立成 `watchmode.go`）
- `internal/command/watch/cmd.go`：`/watch on|off` slash command handler（注册走 `command.Commander`）
- `internal/gateway/gateway.go::Handle`：`HasMention` + `WatchMode` gate

**`/watch` slash command**：

| 调用 | 行为 |
|------|------|
| `/watch on` | `ChatSession.WatchMode = WatchModeAll`；持久化；reply "watching all messages in this chat" |
| `/watch off` | `ChatSession.WatchMode = WatchModeMention`；持久化；reply "watching mentions only (default)" |
| `/watch`（无参）| 显示当前 mode + 简短说明 |

**DM 为 no-op**：DM 下 `HasMention` 永远为 true，gate 永不触发；运行 `/watch on/off` 状态正常写入但不影响消息处理（DM 全收）。文档在 `docs/channel/feishu-onboarding.md` §4 Edge cases 说明。

**飞书 scope 默认开启**：`DefaultAddons()` 始终包含 `im:message.group_msg`（不带 `:readonly` —— bot 需要回复到群里）。**不**走 CLI flag opt-in，由 Devin 拍板（2026-08-03）。

**详细设计**：见 `docs/SPEC.md §3.1.1` + §9 Q-W1/Q-W2/Q-W3/Q-W4。

## 7. 验收 / 测试(本期 minimal scope)

- 单元: `buildReceiptCard` 产出合法 JSON,`elements` 末尾是 `<hr>` + `<text_tag color='neutral'>` foot note;`footLine` 为空时整段省略
- 单元: `footLine` 在 `state=executing, eventCount=5, lastEventAt=14:32:05` → `"executing · 5 entries · 14:32:05"`;`eventCount=0` 时不渲染 `0 entries`
- 单元: `MessageReceipt.renderLocked` 第一次调 → `SendCard`;之后 → `PatchMessage` 同一个 messageID;**不再**调 `SendMessageText`
- 单元: 元素数 = 60 entries → 47 entries + `...(前 N 条已省略)` 标记
- 单元: 字节数 = 收到超大 entries → 驱逐最老直到 < 24 KiB
- 单元：回归 `mockReceiptBot.AddReaction` 不变（reaction 由 MessageState FSM 触发,仍走 userMsgID,但已从 MessageReceipt 解耦到 Adapter 顶层）
- 集成: 端到端: user message → 一张 receipt card(后续 agent event 不再发新消息,而是 PATCH);最终状态 `✅` 出现在 header;foot note 随状态变化
- 回归: permission card (`OutChoice`) 不受影响,继续走原 `buildInteractiveCard`

## 8. 参考资料

- OpenClaw issue #59360: https://github.com/openclaw/openclaw/issues/59360
- OpenClaw PR #84122(middle-dot fix,未合): https://github.com/openclaw/openclaw/pull/84122
- OpenClaw `resolveCardNote` 引入 commit: https://github.com/openclaw/openclaw/commit/df3a247db2a90da2a2593f85bdd5ef07f6b39a91
- OpenClaw card 渲染位置: https://github.com/openclaw/openclaw/blob/1b8b8500cee077d7ac7927def0f566febf7dacb8/extensions/feishu/src/send.ts#L768
- 官方 lark plugin(`openclaw-lark`)的 `FeishuFooterConfig` 模式: https://github.com/larksuite/openclaw-lark
- 社区 fork(`gcmsg/openclaw-feishu`)用 v1 `note` 元素: https://github.com/gcmsg/openclaw-feishu/blob/main/src/menu.ts
- 飞书 create_json 文档: https://open.feishu.cn/document/server-docs/im-v1/message-content-description/create_json
- 飞书 Card 2.0 组件总览: https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-json-v2-components/component-json-v2-overview
- 飞书 PATCH card: https://open.feishu.cn/document/server-docs/im-v1/message-card/patch
- 飞书 markdown 内联标签规范: https://open.feishu.cn/document/common-capabilities/message-card/message-cards-content/using-markdown-tags
- 飞书 lark_md 元素 / 行长度 / 字符限制: https://open.larkoffice.com/document/server-docs/im-v1/message-card/message-card-content/message-card-text-element
- [`channel/feishu-rendering.md`](../channel/feishu-rendering.md) - MessageState 抽象事件，本文件 §6.6 是其 feishu-specific 实现补充
- [`docs/SPEC.md`](../SPEC.md) §2.5 - MessageState 架构概述

## 9. Implementation plan(本期落地,minimal scope)

本期**只做**结构性迁移(receipt 从 text 改 card),不动元数据管线。**agent_name / provider / model 在 foot note 的展示 = 后续 PR**(§9.4)。

### 9.1 改动文件清单

| 文件 | 改动 |
|---|---|
| `internal/channel/feishu/adapter.go` | 新增 `updateMessageFunc` 字段、`patchViaLark`、`patchContent`、`UpdateMessage`、`SendCard`、`buildReceiptCard` |
| `internal/channel/feishu/receipt.go` | 改 `renderLocked` 走 SendCard/PatchMessage;`receiptBot` 接口加 `SendCard`/`PatchMessage`;`MessageReceipt` 加 `cardMsgID`;`ReceiptState` 加 `footLine`;`replyMaxBytes` 改 24576 |
| `internal/channel/feishu/receipt_test.go` | `mockReceiptBot` 加 stubs;新增 / 重写 §7 列出的测试 |
| `internal/channel/feishu/adapter_test.go` | 加 SendCard / PatchMessage 走通测试 |

**不改**: `internal/agent/agent.go`、`internal/bridge/claudecode/stream.go`、`internal/gateway/translate.go`、`internal/gateway/messages.go`、`internal/bridge/acp/*`、`internal/bridge/pty/*`、`internal/bridge/sdk/*`、`cmd/nightme/*`。

### 9.2 关键代码契约

```go
// internal/channel/feishu/receipt.go
type receiptBot interface {
    AddReaction(ctx context.Context, msgID, emoji string) (string, error)
    UpdateMessage(ctx context.Context, messageID, text string) error  // 改:内部走 Patch
    SendMessageText(ctx context.Context, chatID, text string) (string, error)
    SendCard(ctx context.Context, chatID, cardJSON string) (string, error)
    PatchMessage(ctx context.Context, messageID, cardJSON string) error
}

type MessageReceipt struct {
    // ... 现有字段不动 ...
    cardMsgID string  // 新增:首次 SendCard 后记录
}

func (r *MessageReceipt) renderLocked(ctx context.Context) error {
    r.appendReactionLocked(ctx, r.state.Emoji())
    body, err := buildReceiptCard(r)
    if err != nil { return err }
    if r.cardMsgID == "" {
        msgID, err := r.bot.SendCard(ctx, r.chatID, body)
        if err != nil { return fmt.Errorf("... create card: %w", err) }
        r.cardMsgID = msgID
        return nil
    }
    return r.bot.PatchMessage(ctx, r.cardMsgID, body)
}
```

```go
// internal/channel/feishu/adapter.go
func (a *Adapter) SendCard(ctx context.Context, chatID, content string) (string, error) {
    // 透传 sendContent + larkim.MsgTypeInteractive
}
func (a *Adapter) UpdateMessage(ctx context.Context, messageID, content string) error {
    // 走 patchContent + larkim.NewPatchMessageReqBuilder + Message.Patch
}
```

### 9.3 Foot note 格式(本期,仅现有数据)

`<text_tag color='neutral'>{state} · {N entries} · {HH:MM:SS}</text_tag>`

示例:
- 等待中 + 0 事件 → 无 foot note(整段省略)
- 处理中 + 5 事件 + 14:32:05 → `<text_tag color='neutral'>executing · 5 entries · 14:32:05</text_tag>`
- 已完成 + 10 事件 + 14:35:00 → `<text_tag color='neutral'>completed · 10 entries · 14:35:00</text_tag>`

字段缺失时跳过,**绝不**出现连续分隔符(不写 `executing · · 14:32:05`)。

### 9.4 后续 PR(已规划,本期不做)

当 `agent_name` / `provider` 透传落地后,`buildReceiptCard` 的 foot note 升级为:

```
<text_tag color='neutral'>executing · 5 entries · 14:32:05 | Agent · main · Model · claude-sonnet-4-5 · Provider · claudecode</text_tag>
```

所有 `key: value` 段必须用中点 `·`(防 hoisting,见 §6.1)。**留给下一份 PR 的契约**:
- `agent.AgentConnectedEvent` 新增 `AgentName` / `Provider` 字段
- `internal/bridge/claudecode/stream.go` 在 system/init 翻译处填充
- `internal/gateway/translate.go` 写 `Meta["agent_name"]` / `Meta["provider"]`
- `MessageReceipt` 新增对应字段,`Append(AgentConnectedEvent)` 写入
- `footLine` 拼上 ` · Agent · X · Provider · Y`(model 已有,补中点)

**这部分改动是叠加的,不动 card 渲染主体。**

## 10. Known limits(本期须记住)

| Limit | 值 | 来源 |
|---|---|---|
| card body 字节数 | 30 KB(Create / PATCH 相同) | SDK `resource.go:1381` 注释 |
| card elements 数 | 50 | Feishu card 文档 |
| `lark_md` 单行 | 1000 chars | Feishu card 文档 |
| `lark_md` 总长 | 4000 chars | Feishu card 文档 |
| `plain_text` | 500 chars | Feishu card 文档 |
| `div` text | 1000 chars | Feishu card 文档 |
| PATCH 频控 | **5 QPS / message** | SDK 注释 |
| 消息可 PATCH 期限 | 14 天 | Feishu PATCH 文档 |
| `update_multi` 共享卡 | 仅当 `config.update_multi = true` 创建时启用 | Feishu card 文档 |

**本期防御**:
- `replyMaxBytes = 24 KB` -- 留 6 KB 头空间
- entries 上限 = 47 -- 留 3 元素给 header/hr/footer
- 5 QPS 频控靠 receipt 单线程 `renderLocked`(已串行)+ 实际 agent event 频率远低于 5/s,不主动限流

**超出限制时的降级**: `PatchMessage` 失败 → 记录日志 + 下次 render 仍 PATCH 同一 messageID;不重发新消息以避免重复 receipt。**已知风险**: 持续失败 → 卡片一直不更新,直到 receipt 销毁。后续可加重试/降级到"再发新卡"。

## 11. Feishu msg_type 全集(参考)

Feishu IM API 官方支持的顶层 `msg_type`(参考 [create_json 文档](https://open.feishu.cn/document/server-docs/im-v1/message-content-description/create_json))。`internal/channel/feishu/adapter.go` 走 `sendContent(chatID, msgType, content)` 任意 msg_type 都通;**当前 nightme 只用到 2 种**(`text` + `interactive`),其余 9 种是未来扩展的候选。

| `msg_type` | `content` 结构 | 用途 | nightme 现状 | 未来是否用 |
|------------|----------------|------|--------------|------------|
| `text` | `{"text":"..."}` | 纯文本(支持 `<at>` / 超链接 / 4 种 inline 样式) | ✅ `OutCommandReply` | 是 |
| `post` | `{"zh_cn":{"title","content":[[{tag,...}]]}}` | 富文本。tag: `text/a/at/img/media/emotion/hr/code_block`/`md`(CommonMark+GFM 表格/任务列表/删除线) | ❌ 未用 | 视情况(见 §12) |
| `image` | `{"image_key":"img_xxx"}` | 图片(先 [`upload_image`](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/image/create) 拿 key) | ❌ 未用 | 预留(见 §12.2) |
| `file` | `{"file_key":"file_xxx"}` | 文件(先 [`upload_file`](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/file/create)) | ❌ 未用 | 预留 |
| `audio` | `{"file_key":"file_xxx"}` | 音频 | ❌ 未用 | 暂不 |
| `media` | `{"file_key":"...","image_key":"..."}` | 视频(mp4) | ❌ 未用 | 暂不 |
| `sticker` | `{"emoji_type":"SMILE"}` | 表情包(预定义 emoji_type) | ❌ 未用 | 暂不 |
| `interactive` | `{"schema":"2.0","config","header?","body":{"elements":[...]}}` | 交互卡片(Card 2.0) | ✅ **所有 receipt card + 权限卡** | 长期主路径 |
| `share_chat` | `{"chat_id":"oc_xxx"}` | 分享群名片 | ❌ 未用 | 否 |
| `share_user` | `{"user_id":"ou_xxx"}` | 分享个人名片 | ❌ 未用 | 否 |

**独立 reaction API**(`POST /im/v1/messages/{id}/reactions`,body 用预定义 `emoji_type`):nightme 用于 `OutMessageState`。**append-only** -- 每次 AddReaction 加新 emoji,通道不删老的。unicode emoji 直接返回 `99992354 data not found`,必须用飞书预定义名(OneSecond/OnIt/DONE/THUMBSUP 等)。

### 11.1 选型约束(为什么不用 `post` 走 receipt)

`post` 富文本支持 `md` 标签原生渲染 CommonMark+GFM,**看起来比塞进 card body 简单**。但和 rolling-log UX **根本冲突**:

- `post` 是**整体替换语义** -- 每次发新 `post` 消息,飞书会渲染成新气泡,无法原地编辑
- 飞书没有 `PATCH post` 接口;`Update` / `Patch` 只对 `text` 和 `interactive` 生效
- receipt 的核心 UX 是**一张可原地更新的卡片承载整轮事件**;`post` 实现不了 PATCH-in-place

→ 结论:**文本类(OutText/OutTool/OutResult/...)继续走 `interactive` card**,**不切 `post` `md` 标签**。`post` 留作未来"一次性富文本消息"的载体(比如 help / changelog 推送,见 §12.2)。

### 11.2 与 OpenClaw 官方插件(openclaw-lark)的对比

| 用法 | openclaw-lark | nightme |
|------|---------------|---------|
| 文本 | `post` + `tag:"md"`(CommonMark+GFM) | `interactive` card body(`markdown` element) |
| 思考 | `collapsible_panel` + `text_size:"notation"` + 双语 `i18n_content` | `collapsible_panel` -- **但旧折叠方案已废弃,详见 F-37 thread 路由** |
| 工具 | `collapsible_panel` 折叠工具步骤 | `div` + `markdown` 平铺 + emoji 图标 |
| footer | `<hr>` + `<text_tag color='neutral'>`(中点 `·`,非冒号) | 同上 |
| 卡片样式 | Card 2.0 + `update_multi:true` + 双语 | Card 2.0(单语,**未启用 update_multi**) |

→ nightme 的 card body 渲染器比 openclaw-lark **更简单**(无折叠工具、无 i18n),但**少了 thinking 的折叠能力**(已被 F-37 thread 路由取代)。

## 12. OutboundKind → Feishu 渲染映射表(当前状态)

每行 = 一个 `gateway.OutboundKind`(定义见 [`internal/gateway/messages.go`](../../internal/gateway/messages.go)),描述 adapter 怎么渲染。`Receipt` 列指是否进 rolling-log card 路径。

| OutboundKind | 源 AgentEvent | 触发点 | Feishu 渲染 | msg_type / API | Receipt? |
|--------------|---------------|--------|-------------|----------------|----------|
| `OutReply` | `EventText`(无前缀) | agent 对当前 turn 的 reply 流式 chunks(F-40 改名,原 `OutText`) | **F-44: 每 chunk → 独立 `ReplyInThreadAndChat` 消息**(不再 fold 进 receipt)。`sendReplyInThreadAndChat` 走 3 段 dispatch:无 markdown → `MsgTypeText`;tables>5 → `MsgTypePost+md`;默认 → `MsgTypeInteractive` Card 2.0 + 1/`N` `tag:"markdown"` div(F-37 `splitMarkdownForDivs` 拆 ≤ 1000 runes/div)。复用 F-39 `SanitizeCardMarkdown` + `truncateRunes` 28 KB envelope defense。**不加 icon 前缀**(流延续,不是新条目)。**F-45:文本末尾追加 `formatSessionFooter(msg.SessionContext)`**(F-52 新格式:`🤖 Agent · Model` + `💰:「 in / out · X% · $cost 」`;`in` = 三个 input-side 字段之和;`X%` = per-turn context-window 占比,客户端从 bridge-local `contextWindow` + used 算(Claude Code 来自 `modelUsage[<model>].contextWindow`;Pi 来自 `get_state.data.model.contextWindow`,见 [`bridge/pi.md §ContextWindow`](../bridge/pi.md));`$cost` = API 透传 `total_cost_usd`,客户端不计算) | `interactive` Create / `post` Create / `text` Create | ❌ (独立气泡,锚同 userMsgID) |
| `OutThinking` | `EventText`(带 `[思考] ` 前缀,Gateway 已剥) | agent reasoning | **Feishu thread reply**（F-37：类型感知摘要,`💭` 折叠）| `interactive` PATCH | ✅ |
| `OutToolStart` | `EventToolStart` | 工具开始 | **Feishu thread reply**（F-37：`🔧` + 类型感知摘要）| `interactive` PATCH | ✅ |
| `OutToolEnd` | `EventToolEnd` | 工具结束(成功/失败) | **Feishu thread reply**（F-37：`✅` / `❌` + 类型感知摘要）| `interactive` PATCH | ✅ |
| `OutTaskCreate` | `EventTaskCreate` | Claude TaskCreate 成功结果 | **F-44: rolling-log receipt,card body 只剩 `**📋 Tasks**` checklist**;N 个 OutTask* 事件 → 1 张 card 反复 PATCH 同一 snapshot。`ensureReceiptForTask` lazy create(首个 OutTask* 触发)。**F-45:checklist 末尾追加 `formatSessionFooter(msg.SessionContext)`(hr + lark_md div)** | `interactive` PATCH | ✅（仅 Task 单一 section + footer） |
| `OutTaskUpdate` | `EventTaskUpdate` | Claude TaskUpdate / delete 成功结果 | 同上；空 snapshot 清除 checklist；不进入 tool thread。**F-45 footer 同上** | `interactive` PATCH | ✅（仅 Task 单一 section + footer） |
| `OutResult` | `EventResult` | 最终回复 | **独立 reply 到 userMsgID**(F-39;不 fold 进 receipt card)— 三段 dispatch:无 markdown → `MsgTypeText`;tables>5 → `MsgTypePost+md`;默认 → `MsgTypeInteractive` Card 2.0 + 1/`N` `tag:"markdown"` div(F-37 `splitMarkdownForDivs` 拆 ≤ 1000 runes/div)。sanitize via `result_render.go`(cc-connect 移植,F-44 后从 `card_sanitize.go` 合并)。**F-45:文本末尾追加 `formatSessionFooter(msg.SessionContext)`** | `interactive` Create (新 reply) / `post` Create / `text` Create | ❌ (独立气泡,锚同 userMsgID) |
| `OutUsage` | `EventUsage` | token 用量 | **F-44: silent drop**(footer 设计推迟到 footer PR)。`agent.EventUsage` → `OutboundMessage{Usage}` Translate 路径保留 | — | ❌ (不渲染) |
| `OutCompaction` | `EventCompaction` | 中途压缩 | card body `markdown` + `✶ Compacting conversation...` | `interactive` PATCH | ✅ |
| `OutInit` | `EventAgentConnected` | 会话初始化 | **F-44: silent drop**(footer 设计推迟到 footer PR)。`agent.EventAgentConnected` → `OutboundMessage{Init}` Translate 路径保留 | — | ❌ (不渲染) |
| `OutChoice` | `EventPermission` | 权限请求 | `buildInteractiveCard` → header(title,template:blue) + markdown body + action buttons(value 携带 request_id) | `interactive` Create | ❌(独立气泡) |
| `OutMessageState` | ChatSession lifecycle | 消息进度变化 | `AddReaction(userMsgID, emoji_type)` -- 走 `messageStates` map 做 idempotency | reaction API | ❌(标在用户消息上) |
| `OutMessageStateRemoved` | (reserved) | 撤销进度标记 | `DeleteReaction`(未使用,append-only) | reaction API | ❌ |
| `OutTyping` | (orphan) | typing 指示 | **silent drop**(飞书 bot 无原生 typing API) | - | ❌ |
| `OutCommandReply` | (slash cmd / runtime error) | `/cwd` `/use` `/close` `/help` `/agents` 等 | `SendMessageText` -- 独立 text 消息,**绕过** receipt | `text` Create | ❌ |

### 12.1 映射决策的"为什么"

- **receipt card 路径覆盖 8 种** -- 选 `interactive` 是为了 PATCH-in-place(对抗 chat spam);选 markdown element 是为了渲染表格/代码块/超链接(后续会用)
- **MessageState 单独走 reaction** -- append-only emoji 是飞书最轻量、最稳定的进度表达;走 reaction API 不挤占 card body 预算
- **OutChoice 走独立 card(非 receipt)** -- 权限卡是单轮交互,需要按钮 + callback,不适合进 rolling log
- **OutCommandReply 走纯文本 `text`** -- 命令反馈是"短而独立"语义,绕过 receipt 让用户看到干净气泡(参见 F-08 §4 "Channel is dumb" contract: command reply 不属于滚动日志)

### 12.2 未来扩展槽位(不实现,但留位)

| 未来需求 | 候选 msg_type | 候选 OutboundKind | 备注 |
|----------|---------------|-------------------|------|
| agent 生成图片 | `image`(先 upload 拿 image_key) | 新加 `OutAttachment{Type: "image", FileKey, FileName}` | 不并入 receipt card(打散 PATCH),走独立 Create |
| agent 生成文件 | `file` | `OutAttachment{Type: "file", ...}` | 同上 |
| help / changelog 推送 | `post` + `tag:"md"` | 复用 `OutCommandReply` 或新增 `OutPost` | 一次性富文本 |
| bot 自定义表情包 | `sticker` | 复用 `OutMessageState` 或新增 | 仅 DM 可用 |

---

