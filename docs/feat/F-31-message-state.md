# F-31: MessageState — 消息生命周期进度跟踪

> **Status**: locked (v1.3; shipped in commit a6113d9)
> **Milestone**: v1.3
> **Depends on**: F-08 (Channel abstraction), F-26 (Gateway), F-27 (ChatSession), F-29 (AgentSession pool)
> **Used by**: end users (visual feedback), ChatSession lifecycle
> **Related docs**: [`SPEC.md`](../SPEC.md) v1.3 §2.5, [`F-26-gateway-hub.md`](./F-26-gateway-hub.md), [`channel/feishu.md`](../channel/feishu.md) §6.6, [`F-25-rolling-log.md`](./F-25-rolling-log.md)

---

## 1. Description

`MessageState` 是 nightme 对用户消息生命周期的可见性反馈机制。它回答 3 个问题：

1. **ChatSession 收到消息没有？** — `StateReceived`
2. **消息转给 AgentSession 了没有？** — `StateForwarded`
3. **AgentSession 执行完成了没有？** — `StateDone` (+ 可选 `StateError`)

每条普通用户消息在系统里流转时，会触发对应的 `MessageState` 事件；Channel 把它渲染成平台原生视觉表达（Feishu 渲染成 reaction emoji，Slack 渲染成 emoji 短码，Web UI 渲染成 DOM 元素，等等）。

---

## 2. Motivation & Problem

### 2.1 v1.2 现状（不理想）

v1.2 的 "reaction" 概念混在 feishu 实现里：

- `internal/channel/feishu/receipt.go` 内部维护 `currentReaction` / `appendReactionLocked`，直接调飞书 SDK
- 抽象层 `OutboundKind.OutReaction` 定义了但**没有调用方**（dead code；v1.3 已删除）
- 跨 channel 实现（Slack / Web UI）需要重新实现 FSM + idempotency + 状态映射

这违反了 nightme 的职责隔离原则：**抽象架构功能被泄漏到具体 channel 实现里**。

**v1.3 状态**：本 F-31 设计已落地（commit a6113d9）。`MessageState` FSM 由 ChatSession 拥有（lifecycle 触发），Gateway 转发为 `OutboundMessage{Kind: OutMessageState}`，Channel 通过 `Send` 派发到原生 API（Feishu: AddReaction; Slack: emoji shortcode; Web: DOM 元素）。Receipt FSM 完全独立处理（见 [`F-25-rolling-log.md`](./F-25-rolling-log.md)）—— v1.3 起 Receipt 概念本身从 Gateway 移除，MessageState 真正独立。

### 2.2 设计目标

1. **抽象**：MessageState 是消息的属性，跟 channel 无关
2. **统一事件**：通过 `Channel.Send(OutboundMessage)` 走现有 wire format，零新增 API surface
3. **Owner 清晰**：ChatSession 发出，Gateway 转发，Channel 适配
4. **可观测**：所有 MessageState 事件可被 logging / tracing 看到
5. **跨平台**：不同 channel 用各自的视觉表达承载同一抽象

### 2.3 与 Receipt 的关系（v1.3 真正独立）

v1.2 末注释中预想的 "MessageState 与 Receipt 两者完全独立"在 v1.3 才真正成立 —— v1.3 起 **Gateway 不再持有 Receipt 概念**（见 SPEC §0.1 与 [`F-25-rolling-log.md`](./F-25-rolling-log.md)），MessageState 不再需要与 Receipt FSM 共存。

| 概念 | 跟踪什么 | Owner | 渲染载体 |
|---|---|---|---|
| **MessageState** | 消息在系统里的处理进度 | ChatSession | Channel 自己（Feishu: reaction emoji）|
| **Rolling-log receipt** | agent 响应的内容卡片 | Channel 内部 | Channel 自己（Feishu: card PATCH）|

两者的语义、owner、触发点、渲染都**完全独立**：
- 共同点：都按 `userMsgID` 索引
- 不同点：MessageState 跟踪消息本身进度；rolling-log 跟踪响应内容增长
- 触发点：MessageState 由 ChatSession lifecycle 触发（`cs.emitMessageState`）；rolling-log 由 `OutboundMessage{ReplyTo: userMsgID}` 在 Channel.Send 内部触发 cold-create / PATCH
- 互不依赖：任一失败不影响另一个

详细协议见 [`F-25-rolling-log.md`](./F-25-rolling-log.md) §6。

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
    // LookupActiveAgentSession.
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
| `dispatchSlashCommand`（`/cwd` `/use` `/kill` 等） | ❌ 否 |
| ChatSession lifecycle 内部事件 | ❌ 否（除非该事件由用户消息触发） |

理由：slash command 是**控制平面**，用户发命令是为了控制系统状态，不是为了让系统"处理一条消息"。控制平面有它自己的反馈（`Channel.Send(OutboundMessage{Kind: OutCommandReply})`），不需要进度标记。

### 3.3 三个核心状态语义

| 状态 | 含义 | 何时触发 | 何时结束 |
|---|---|---|---|
| `StateReceived` | 系统已收到消息，等待 dispatch | `ChatSession.GetOrCreate(chatID)` 成功后 | `StateForwarded` 或消息丢弃 |
| `StateForwarded` | 消息已转给 AgentSession，正在处理 | `ChatSession.LookupActiveAgentSession()` 成功 | `StateDone` 或 `StateError` |
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

    // Reaction 字段保留（向后兼容）但 v1.3 后不再被 OutReaction kind 使用。
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
| `ChatSession.LookupActiveAgentSession()` 成功 | `StateForwarded` | spawn 成功或命中 running pool |
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
// 在 LookupActiveAgentSession 成功路径里调 cs.emitMessageState(userMsgID, StateForwarded)
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

详见 [`channel/feishu.md`](../channel/feishu.md) §6.6。摘要：

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
| `TestChatSession_EmitsForwarded_OnLookupActive` | LookupActiveAgentSession 成功后 callback 被调 |
| `TestChatSession_EmitsDone_OnEventDone` | readPump 收到 EventDone 后 callback 被调 |
| `TestChatSession_EmitsError_OnEventError` | readPump 收到 EventError 后 callback 被调 |
| `TestChatSession_NoEmit_OnSlashCommand` | `/cwd` `/use` `/kill` 不触发任何 MessageState |
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

## 11. Out of Scope（v1.3 / F-31）

| 项 | 描述 |
|---|---|
| `OutMessageStateRemoved` 实现 | 当前 append-only 不删反应；如果未来需要（飞书 API 允许 DeleteReaction），再实现 |
| `StateError` 的 emoji 映射细化 | 当前用 THUMBSUP 兜底，未来可加专用 emoji_type |
| 状态时间戳 / 序列号 / metadata | 当前只有 message_id + state；扩展字段后续可加 |
| Receipt FSM 与 MessageState 的统一编排 | 两者独立工作，不集成；如果未来需要（如 Receipt Done 时也强制 MessageState Done）再实现 |
| Slash command 的 MessageState | scope 明确不触发 |

---

## 12. Change Log

- **2026-08-03** — F-31 v1 draft: MessageState 概念抽象化，与 Receipt 解耦，4 层流转架构。提交到 `fix/inboud_buffer` 分支。

---

## 13. References

- [`SPEC.md`](../SPEC.md) v1.3 §2.5 — high-level architecture
- [`F-08-channel-abstraction.md`](./F-08-channel-abstraction.md) — Channel interface
- [`F-26-gateway-hub.md`](./F-26-gateway-hub.md) — Gateway architecture
- [`F-27-chatsession.md`](./F-27-chatsession.md) — ChatSession lifecycle
- [`F-29-agent-session-pool.md`](./F-29-agent-session-pool.md) — AgentSession pool
- [`channel/feishu.md`](../channel/feishu.md) §6.6 — feishu-specific implementation