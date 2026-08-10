# F-54: 事件总线（EventBus）— 统一 ChatSession 事件订阅与分发

> **Status**: design draft（讨论已收敛，待实现）
> **Milestone**: v1.3.x
> **Depends on**: F-27（ChatSession）、F-31（MessageState）、[`feat/message_lifecycle.md`](./message_lifecycle.md)（F-53 Prompt/Message 模型）
> **Used by**: `cmd/nightme/run.go` wiring、feishu adapter reaction 渲染、未来 Slack/Web 通道、HUD/审计/metrics 等多订阅者场景
> **Related docs**: [`SPEC.md`](../SPEC.md) §2.5、[`F-31-message-state.md`](./F-31-message-state.md)、[`F-29-agent-session-pool.md`](./F-29-agent-session-pool.md)、[`internal/command/services/reaction.go`](../../internal/command/services/reaction.go)（与之并存的 ReactionRouter）

---

## 1. Description

把 ChatSession 上现有的 3 个单观察者 callback（`eventHandler` / `onMessageState` / `onPromptEnd`）统一替换为一个**泛型事件总线**（`services.EventBus[T]`）。每个事件类型独占一个 `*EventBus[X]`，由 ChatSession 持有，对外暴露 Subscribe/Publish/Clear/Close 四个 API。

效果：

1. **统一接口** — 所有事件类型走同一套 Subscribe/Publish 形态，不再为每种事件写一套 setter/fire。
2. **类型安全** — `Subscribe(func(MessageStateEvent) bool)` 在编译期保证 handler 签名匹配。
3. **多订阅者** — 不再是 last-wins；多个 subscriber 共存，按注册顺序触发，第一个返回 `true` 的消费事件。
4. **业务无关** — `EventBus[T]` 不知道任何 domain 概念，可被任何 package 复用。
5. **panic 隔离** — 单个 handler panic 不影响 daemon，模仿既有 `invokeReactionHandler`。
6. **Clear 逃生口** — 需要"换一组订阅者"时显式调 `Clear()`，没有 last-wins 包袱但仍能兼容迁移期 caller。

---

## 2. Motivation & Problem

### 2.1 现状（v1.3.x 三个单观察者）

`internal/chatsession/chatsession.go` 当前持有 3 个 callback 字段：

```go
eventHandler   EventHandler                                         // AgentEvent 包装
onMessageState func(chatID, userMsgID string, state agent.MessageState)
onPromptEnd    PromptEndHandler                                     // func(userMsgID string, reason PromptEndReason)
```

每个字段都是**单 `func`**（不是 `[]func`），setter 走 last-wins 替换：

```go
func (cs *ChatSession) SetEventHandler(h EventHandler)        { cs.eventHandler = h }
func (cs *ChatSession) SetMessageStateHandler(h func(...))    { cs.onMessageState = h }
func (cs *ChatSession) SetPromptEndHandler(h PromptEndHandler) { cs.onPromptEnd = h }
```

fire 点散落 3 处（`routeEvent` / `EmitMessageState` / `writebackMessageState`），且**签名各异**（3 参数 / 2 参数 / 含 AS 指针 / 含 agent.MessageState / 含 PromptEndReason）。

### 2.2 三个具体问题

**问题 A：last-wins 不利于多订阅者。** v1.3.x 实际上每类事件只挂了 1 个 handler（`cmd/nightme/run.go` 各注册一个），所以 last-wins 表面上"够用"。但未来要加 audit logger、metrics collector、HUD renderer 等多消费者时，每个新订阅者都要去抢那 1 个 slot，扩展性差。

**问题 B：参数扁平、无 envelope。** 老 fire 用 3-4 个散参数（`chatID, userMsgID, state`），新事件要加一个字段时（比如 `PromptID`、`EndedAt`），签名要么加参数、要么破坏 API。Envelope（typed struct）天然可扩展。

**问题 C：跟 ReactionRouter 形态不一致。** `internal/command/services/reaction.go` 已经有"按 chatID 路由的 pub/sub-类"机制，但 ChatSession 这边的 3 个 callback 又是另一种风格。两条机制在同一个进程里共存但形态分裂。F-54 至少把 ChatSession 这边统一起来。

### 2.3 设计目标

1. **业务无关** — `EventBus[T]` 不 import 任何 domain 类型，可被任何包使用
2. **不新增顶层包** — 放进已有 `internal/command/services/`，跟 `ReactionRouter` 同包
3. **不在 `chatsession` 下开子包** — `event_types.go` 留在 `chatsession` 主包（跟 `prompt_state.go` 同级）
4. **类型安全** — 编译期保证 handler 签名
5. **多订阅者** — pub/sub 原生语义
6. **兼容迁移** — 通过 `Clear()` 模拟旧 last-wins 行为

---

## 3. Design

### 3.1 包与文件布局

| 文件 | 角色 |
|---|---|
| `internal/command/services/eventbus.go` | 新增。`EventBus[T any]` 泛型，零 domain 耦合 |
| `internal/command/services/eventbus_test.go` | 新增。覆盖 9 个核心不变量 |
| `internal/chatsession/event_types.go` | 新增。4 个 typed event struct |
| `internal/chatsession/chatsession.go` | 改造：删 3 个 setter + 1 个 getter，加 4 个 `*EventBus[X]` 字段 + 4 个 getter，3 个 fire 点改 `Publish` |
| `internal/chatsession/compat_stubs.go` | 删 `EventHandler` 类型别名 |
| `cmd/nightme/run.go` | 改造：3 个 `SetXxxHandler` → 3 个 `XxxBus().Subscribe(...)` lambda |
| `internal/channel/feishu/adapter.go` | 改造：handler 签名从散参数 → typed event struct |

### 3.2 `EventBus[T]` API

```go
package services

type EventBus[T any] struct { /* ... */ }

func NewEventBus[T any]() *EventBus[T]

// Subscribe 注册 handler；返回的 func 是 unsubscribe，幂等；
// 从 handler 内调用 unsubscribe 安全；nil handler 静默丢弃。
func (b *EventBus[T]) Subscribe(fn Handler[T]) (unsubscribe func())

// Publish 按注册顺序触发 handler；第一个返回 true 的"消费"事件，后续不再调。
// nil / closed Bus 是 no-op。每 handler panic 单独 recover + log。
func (b *EventBus[T]) Publish(v T) bool

// Clear 清空所有 subscriber。Bus 不进入 closed 状态，可继续 Subscribe。
// 不允许在 handler 内调用（会死锁）。
func (b *EventBus[T]) Clear()

// Close 永久关闭。后续 Publish/Subscribe/Clear 都是 no-op。
func (b *EventBus[T]) Close()

func (b *EventBus[T]) Len() int  // 仅 debug / test
```

类型参数 `T` 是事件 payload。`MessageStateEvent` / `PromptEndedEvent` / `AgentEventEnvelope` / `LifecycleEvent` 各自独占一个 `*EventBus[X]`。

### 3.3 4 个事件类型

放在 `internal/chatsession/event_types.go`（留在 chatsession 主包，**不开子包**）：

```go
type AgentEventEnvelope struct {
    ChatID       string
    UserMsgID    string
    PromptID     string
    AgentSession *AgentSession   // 跟老 EventHandler 签名对齐
    Event        *agent.AgentEvent
}

type MessageStateEvent struct {
    ChatID    string
    UserMsgID string
    State     agent.MessageState
    At        time.Time
}

type PromptEndedEvent struct {
    ChatID    string
    UserMsgID string
    PromptID  string
    Reason    PromptEndReason
    EndedAt   time.Time
}

type LifecycleEvent struct {
    ChatID         string
    AgentSessionID string
    PID            int
    Status         Status   // agentsession.Status
}
```

> 注：`AgentEventEnvelope` 比老 `EventHandler` 签名多了个 `PromptID` 和 `AgentSession *AgentSession`，后者替代"老 setter 直接传 `s *AgentSession`"。这让 envelope 自包含。

### 3.4 ChatSession 改造

**字段（删 3 → 加 4）**：

```go
type ChatSession struct {
    // 删:
    // eventHandler   EventHandler
    // onMessageState func(chatID, userMsgID string, state agent.MessageState)
    // onPromptEnd    PromptEndHandler

    // 加:
    agentEventBus    *services.Bus[AgentEventEnvelope]
    messageStateBus  *services.Bus[MessageStateEvent]
    promptEndBus     *services.Bus[PromptEndedEvent]
    lifecycleBus     *services.Bus[LifecycleEvent]
}
```

**构造函数**（`NewChatSession` / `RestoreFromRegistry`）：

```go
cs.agentEventBus   = services.NewEventBus[AgentEventEnvelope]()
cs.messageStateBus = services.NewEventBus[MessageStateEvent]()
cs.promptEndBus    = services.NewEventBus[PromptEndedEvent]()
cs.lifecycleBus    = services.NewEventBus[LifecycleEvent]()
```

**3 个 getter**（替代 3 个 Setter + 1 个 getter）：

```go
func (cs *ChatSession) AgentEventBus() *services.Bus[AgentEventEnvelope]   { return cs.agentEventBus }
func (cs *ChatSession) MessageStateBus() *services.Bus[MessageStateEvent]   { return cs.messageStateBus }
func (cs *ChatSession) PromptEndBus() *services.Bus[PromptEndedEvent]       { return cs.promptEndBus }
func (cs *ChatSession) LifecycleBus() *services.Bus[LifecycleEvent]         { return cs.lifecycleBus }
```

**3 个 fire 点改造**：

| 位置 | 改前 | 改后 |
|---|---|---|
| `routeEvent` (KindAgentEvent 分支) | `cs.eventHandler(chatID, as, *ev.AgentEvent, userMsgID)` | `cs.agentEventBus.Publish(AgentEventEnvelope{...})` |
| `EmitMessageState` | 读 `cs.onMessageState` → 调 | `cs.messageStateBus.Publish(MessageStateEvent{...})` |
| `writebackMessageState` 末尾 | `cs.onPromptEnd(p.LastMessageID, p.EndReason)` | `cs.promptEndBus.Publish(PromptEndedEvent{...})` |

### 3.5 不保留 deprecated wrapper

F-54 设计明确**不保留** last-wins Setter wrapper。理由：

- 旧 `SetXxxHandler(h)` 的语义是"如果之前有 handler 会被替换"。新 `Bus` 没有这个语义。
- 加 wrapper 反而要再存 unbind 字段、模拟替换 — 这是 F-54 想消除的复杂度。
- 迁移期 caller 想模拟 last-wins，只需 `Clear()` + `Subscribe(h)`：
  ```go
  cs.AgentEventBus().Clear()
  cs.AgentEventBus().Subscribe(h)
  ```
  显式两步比隐式 Setter 更清晰。

### 3.6 run.go wiring

```go
// 改前
cs.SetEventHandler(adapter.HandleAgentEvent)
cs.SetMessageStateHandler(adapter.HandleMessageState)
cs.SetPromptEndHandler(func(mid string, reason agentsession.PromptEndReason) {
    adapter.MarkReceiptPromptDone(ctx, cs.ChatID, mid)
})

// 改后
cs.AgentEventBus().Subscribe(func(env chatsession.AgentEventEnvelope) bool {
    return adapter.HandleAgentEvent(env)
})
cs.MessageStateBus().Subscribe(func(e chatsession.MessageStateEvent) bool {
    return adapter.HandleMessageState(e)
})
cs.PromptEndBus().Subscribe(func(e agentsession.PromptEndedEvent) bool {
    adapter.MarkReceiptPromptDone(ctx, cs.ChatID, e.UserMsgID)
    return true
})
```

`unbind` 不显式 defer：handler 生命周期跟 cs 一致，cs 关闭时 GC 自动清理。

### 3.7 feishu adapter 改动

`HandleAgentEvent` / `HandleMessageState` 签名改成接收 typed event struct：

```go
// 改前
func (a *Adapter) HandleAgentEvent(chatID string, s *agentsession.AgentSession, ev agent.AgentEvent, userMsgID string) bool
func (a *Adapter) HandleMessageState(chatID, userMsgID string, state agent.MessageState) bool

// 改后
func (a *Adapter) HandleAgentEvent(env chatsession.AgentEventEnvelope) bool
func (a *Adapter) HandleMessageState(e chatsession.MessageStateEvent) bool
```

内部实现照旧（用 env.UserMsgID 替代 mid 参数，等等）。这是签名纯收紧，对调用方零成本。

---

## 4. 不变量与边界

1. **同步 fan-out** — `Publish` 是同步调用，handler 看到的状态不变量保留（跟 `writebackMessageState` 旧 fire 一致）。
2. **顺序保证** — handler 按 Subscribe 注册顺序触发（同 `ReactionRouter` 语义）。
3. **类型安全** — `Subscribe(func(X) bool)` 编译期校验签名。
4. **panic 隔离** — 每个 handler 用 `defer recover()` 包住；panic 后 `consumed = false`，chain 继续。
5. **nil-safe** — 所有方法对 `(b *EventBus[T])(nil)` 是 no-op。
6. **closed-safe** — `Close` 后 `Publish` / `Subscribe` / `Clear` 全部 no-op。
7. **unsubscribe 幂等** — `sync.Once` 包装，多次调安全。
8. **handler 内 unsubscribe 安全** — entry 在 handler 返回后才删，不影响本次 fan-out。
9. **handler 内 Subscribe 安全** — 因为 Publish 先 snapshot 再 invoke，加锁在 snapshot 阶段。
10. **handler 内 Clear 不安全** — Clear 会立即修改 handlers slice，会破坏 snapshot + 死锁。

---

## 5. 测试覆盖（`eventbus_test.go`）

| 用例 | 覆盖不变量 |
|---|---|
| `TestBus_PublishOrder` | 注册顺序触发 |
| `TestBus_ConsumeStopsChain` | 第一个 `true` 后后续不调 |
| `TestBus_AllFalseContinues` | 全 false 时全部跑完 |
| `TestBus_PanicRecovered` | panic 不影响后续 handler |
| `TestBus_UnsubscribeIdempotent` | 多次 unsub 安全 |
| `TestBus_UnsubscribeFromInsideHandler` | handler 内 unsub 自己 |
| `TestBus_ClearDropsAll` | Clear 后可继续 Subscribe |
| `TestBus_CloseStopsPublish` | Close 后 Publish 无效 |
| `TestBus_NilSafe` | nil receiver 全 no-op |
| `TestBus_ConcurrentSubscribe` | 并发 Subscribe 安全（`-race`） |

---

## 6. 迁移步骤

1. 写 `internal/command/services/eventbus.go` (~120 行)
2. 写 `internal/command/services/eventbus_test.go`（10 个用例）
3. 写 `internal/chatsession/event_types.go`（4 个 typed struct）
4. 改 `ChatSession`：
   - 加 4 个 `*EventBus[X]` 字段 + 构造时 `NewEventBus[T]()`
   - 加 4 个 getter
   - 删 `SetEventHandler` / `SetMessageStateHandler` / `SetPromptEndHandler` 3 个 setter
   - 删 `EventHandler()` / `MessageStateHandler()` 2 个 getter
   - 改 `EmitMessageState` 内部 fire 逻辑
   - 改 `writebackMessageState` 末尾 fire 逻辑
   - 改 `routeEvent` 的 `KindAgentEvent` 分支
5. 删 `internal/chatsession/compat_stubs.go` 里 `EventHandler` 类型别名
6. 改 `cmd/nightme/run.go` 的 3 个 wiring lambda
7. 改 `internal/channel/feishu/adapter.go` 的 handler 签名（`HandleAgentEvent` / `HandleMessageState`）
8. 改所有引用旧 setter/getter 的测试
9. `go build ./...` + `go test ./...` 验证（含 `-race`）

---

## 7. 不动的部分

| 模块 | 原因 |
|---|---|
| `ReactionRouter`（`internal/command/services/reaction.go`） | 按 chatID 路由 + 通配回退，pub/sub 给不出。保留。 |
| `EnrichedEvent`（`internal/chatsession/events.go`） | CS/AS 桥协议，跟 Bus 平行（一个在 CS/AS 边界，一个在 CS 之上）。 |
| bridge 层 events channel（`bridge/pi`、`bridge/acp`） | 不进 Bus。 |

---

## 8. 已知限制 / 后续工作

- `EventBus[T]` 是同步 fan-out；如果未来要异步 fan-out（goroutine pool），需独立 PR，不在 F-54 范围
- `Clear()` 不允许在 handler 内调（死锁）；迁移期 caller 注意
- 没有 wildcard / topic 路由（这是 ReactionRouter 的职责，不重叠）