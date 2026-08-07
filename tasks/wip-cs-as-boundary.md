# ChatSession ↔ AgentSession 边界重构 —— 设计

> **Status**: ✅ **Phase 1 已落地 (commits 60b0a1c + f7e7522 on `feat/alive`)**。
>   Phase 1.x / 2 / 3 设计工作见 [`tasks/wip.md`](./wip.md) (L1 Pinger / L2 Stall / L1.5 离线 AS 跟踪)。
> **Plan**: [`tasks/plan-cs-as-boundary.md`](./plan-cs-as-boundary.md)（落地细节，含实施回顾）
> **依赖**：[`docs/feat/message_lifecycle.md`](../docs/feat/message_lifecycle.md)（Phase 0 已合，本
>   设计基于其上）
> **Scope**: `internal/chatsession`（ChatSession 与 AgentSession 边界）+ `internal/agent`
>   + `cmd/nightme/run.go`
> **不含**:
>   - L1/L2 探活、Pinger、stall watchdog（见 [`tasks/wip.md`](./wip.md)，下一阶段）
>   - `Message`/`Prompt` 对象本身（Phase 0 已合）
>   - `nightme health` 扩展（Phase 4）

---

## 1. 背景与目标

Phase 0 已经把 `Message`/`Prompt` 对象化，但**用法**还停留在 ChatSession 视角：ChatSession 在
Submit 路径上做了 Prompt 拼装、Message.Stage 翻转、wire 事件 emit、Prompt 生命周期管理等 4 件
事。这造成三个具体问题：

1. **职责越界**：ChatSession 越界修改 `as.currentPrompt`、遍历 `cs.messagesByID` 改 `Message.Stage`、
   触发 `onMessageState` callback。AgentSession 被 ChatSession 戳来戳去。
2. **死锁瓶颈**：Prompt 状态写在 `cs.mu` 锁内,F-27 留的 `AgentExitObserver` 接入位被这条耦合堵
   死——任何"agent 进程崩了"路径要在 `cs.mu` 下才能改状态,意味着 readpump 不能安全地自治。
3. **L1.5 离线 AS 跟踪无法落地**：当前 `defaultPromptHookLocked` 把所有 Prompt 操作都耦合到
   `cs.activeAS`,旧 AS 上的 Prompt 在 `/use` 切走时没有任何自治能力。

**设计目标**:把 ChatSession 收成"队列 + 活跃 AS 指针"的薄壳,AgentSession 收成"Prompt 生命周期
+ 事件流"的自治单元,两者之间通过 3 个方法 + 1 条流交互。

---

## 2. 核心边界划分

### 2.1 ChatSession 拥有

- `messagesByID` (sync.Map):所有 Message 的唯一权威存储
- `queue` (```[]*Message```):待提交消息的有序队列,**至少一次成功提交语义**
- `Message.Stage` 字段的所有写操作(Queued 入队时、Submitted 提交成功时、Dropped 主动清空时)
- "消息永远发向当前激活的 AS" 的路由决策

### 2.2 AgentSession 拥有

- `currentPrompt *Prompt`:in-flight Prompt,完全私有
- OpContext 生命周期:从首次 Activate 到 Shutdown
- Prompt 整个生命周期:ID 分配、SendBlocks 调用、LastProgressAt 推进、endPrompt 收口
- 事件流(eventQueue):enriched 后的 EnrichedEvent 持续累积
- `IsReady()` 状态翻转:由 endPrompt 内部驱动

### 2.3 共同引用 `Prompt`

- ChatSession 拼装候选 Prompt → 传给 AgentSession.Submit
- AgentSession 接收后填 ID/AckedAt/LastProgressAt,提交完成后持有 currentPrompt
- Prompt 通过引用(指针)在两边流转,**不复制**
- EnrichedEvent 携带 `*Prompt` 引用,ChatSession 读 `ev.Prompt.EndReason` 写回消息状态

---

## 3. 接口协议

ChatSession 与 AgentSession 之间的协议,就此收敛到 3 个方法 + 1 条流:

```
AgentSession 暴露给 ChatSession:
    Submit(p *Prompt) error         // 提交一个候选 Prompt
    IsReady() bool                  // 能否接受下一个 Submit
    Events() <-chan EnrichedEvent   // 持续事件流
    Shutdown()                      // 真正取消 opCtx + 关 readpump + 关 eventQueue
    
ChatSession 内部使用:
    cs.buildPrompt() *Prompt       // 从 queue 拼装
    cs.TryFlush()                   // 驱动 TryFlush
    for ev := range cs.activeAS.Events():
        cs.routeEvent(ev)
```

**CS ↔ AS 边界无 callback**——所有 AS 域事件走流。`onMessageEnd` / `EventHandler` 这两个 callback
删除,统一为 `EnrichedEvent` 流。

**唯一例外**:`cs.EmitMessageState(userMsgID, state)` 保留为 ChatSession 域的 survivor callback,
用于发射 `MessageSubmitted` / `MessageDropped` wire event。理由:MessageState 是 ChatSession 域
状态,不是 AS 域;"no callback"原则针对 CS↔AS 边界,不是 CS→runtime。`cs.onMessageState` 实例字段
保留,`runtime` 在启动时通过 `cs.SetMessageStateHandler` 注入。

---

## 4. EnrichedEvent 设计

### 4.1 形态

```go
type EnrichedEvent struct {
    // 上下文 (AS 填,来自自身)
    ChatID         string  // = as.ChatSessionID
    AgentSessionID string  // = as.ID

    // 锚点 (AS 填,来自 currentPrompt)
    // 非 prompt 期事件 (Spawned/Exited) 这两个字段为空
    UserMsgID      string  // = currentPrompt.LastMessageID
    PromptID       string  // = currentPrompt.ID

    // 引用 (不复制,运行时按需读)
    Prompt *Prompt  // 终态时 EndedAt/EndReason 已填

    // 事件体 (恰好一个不为 nil)
    Kind              EnrichedEventKind
    AgentEvent        *agent.AgentEvent  // 桥层透传
    PromptEnded       *PromptEndedChange
    Lifecycle         *LifecycleChange
}

type EnrichedEventKind int

const (
    // 桥层透传:每一条 AgentEvent 都包成这个
    KindAgentEvent EnrichedEventKind = iota

    // Prompt 结束:统一 EventDone/Error/!ok/stalled-killed 四条路径收口
    KindPromptEnded

    // AS 生命周期:Spawned / Exited / Killed
    // 不带锚点 (UserMsgID 空)
    KindLifecycle
)

type PromptEndedChange struct {
    EndedAt   time.Time
    EndReason PromptEndReason
}

type LifecycleChange struct {
    PID    int
    Status Status
}
```

### 4.2 设计原则

- **引用优于复制**:Prompt / Message / AgentEvent 全部指针,运行时按需读,避免两者之间搬砖
- **多发不吞**:每条桥事件都透传,AS 不替上层做"这事件对 chat 有没有用"的判断
- **抽象化上行**:MessageState 变更(Prompt 终态)用 `KindPromptEnded` 取代原来的 onMessageState
  callback,所有 wire 事件消费统一走 EnrichedEvent → runtime 路由
- **锚点可选**:AS-level 事件 (Spawned/Exited) 的 UserMsgID 为空,上层按"非锚点事件"路由

### 4.3 字段所有权

| 字段 | 来源 | ChatSession 是否读 |
|---|---|---|
| `ChatID` / `AgentSessionID` | AS 自身 | 读用于路由 |
| `UserMsgID` / `PromptID` | AS.currentPrompt | 读用于锚定 receipt card |
| `Prompt` | AS 持有 | 读 `Prompt.EndReason` / `EndedAt` 写回消息状态 |
| `AgentEvent` | 桥层透传 | 转发给 runtime 渲染 |
| `PromptEnded` / `Lifecycle` | AS 内部 | 读用于路由决策 |

**ChatSession 不读 `Prompt.Blocks`**(那是 SendBlocks 用的载荷,AS 域)。ChatSession 不读
`Prompt.LastMessageID`——已经冗余在 `EnrichedEvent.UserMsgID`。

---

## 5. OpContext 语义

### 5.1 生命周期

```
当前形态                              新形态
────────                            ────────
opCtx = derive(parent) on Activate    opCtx = derive(parent) 一次,首次 Activate
opCtx = cancel on Background          opCtx = no-op
opCtx = re-derive on next Activate    opCtx = 不变
opCtx = cancel only on AS close       opCtx = cancel only on AS.Shutdown()
```

### 5.2 方法语义

- `Activate(parent context.Context)`:幂等。首次调用时 `opCtx, opCancel = context.WithCancel(parent)`;
  后续调用 no-op。
- `Background()`:**no-op**。保留方法是为 API 兼容,但内部什么都不做。`/use` 切换 AS 时不取消旧 AS
  的 opCtx。
- `Shutdown()` (新增):真正取消 opCtx。`/kill`、AS 销毁、ChatSession 关闭时调用。

### 5.3 promoteActiveLocked 简化

```go
func (cs *ChatSession) promoteActiveLocked(as *AgentSession) {
    cs.activeAS = as
    if as == nil {
        return
    }
    if as.IsActivated() {
        return
    }
    as.Activate(cs.Context())
}
```

5 行实现。`prev.Background()` 删。`/use` 切换 = `cs.activeAS = as` + 首次 Activate。

### 5.4 `as.Close()` 与 `as.Shutdown()` 的区别

- `as.handle.Close()` (桥层 `AgentSession` 接口):关底层 transport (PTY/进程/stdin)
- `as.Close()` (wrapper 层,保留):调用 `as.handle.Close()` + 后续清理
- `as.Shutdown()` (新增):取消 opCtx + 等待 readpump 退出 + 关闭 eventQueue

`/kill` 走 `as.Shutdown()` 而非 `as.Close()` — `Shutdown` 是 AS 整个生命周期终止,`Close` 是关
单个桥层 transport。Phase 1 不动 `as.Close()` 路径,只新增 `Shutdown()`。

---

## 6. Locking 策略

### 6.1 `currentPrompt` 锁归属

`as.currentPrompt` 由 `asMu` (现有锁) 保护。Submit / readpump / endPrompt 必须通过 asMu 访问:

```go
// Submit:写 currentPrompt
as.asMu.Lock()
as.currentPrompt = p
as.isReady.Store(false)
as.asMu.Unlock()

// readpump:读 currentPrompt (anchoring)
as.asMu.RLock()
prompt := as.currentPrompt
as.asMu.RUnlock()

// endPrompt:读 + 清空 currentPrompt
as.asMu.Lock()
prompt := as.currentPrompt
if prompt == nil { as.asMu.Unlock(); return }
prompt.EndedAt = time.Now()
prompt.EndReason = reason
as.currentPrompt = nil
as.isReady.Store(true)
copiedPrompt := prompt  // 引用,之后用
as.asMu.Unlock()
// 推 eventQueue (在锁外)
as.eventQueue <- EnrichedEvent{...Prompt: copiedPrompt...}
```

**为什么不无锁**: Submit 写 currentPrompt 跟 readpump 读 currentPrompt 必须序列化,否则
bridge 立即回 EventDone + Submit 还没 commit 的 race 会导致 stuck。asMu 是已有锁,扩展它
覆盖 currentPrompt 即可,避免引入新锁。

### 6.2 `TryFlush` 串行化

`cs.TryFlush` 全程用 `cs.mu` 串行化(只 Submit 阶段临时释放):

```go
func (cs *ChatSession) TryFlush() error {
    cs.mu.Lock()
    if len(cs.queue) == 0 { cs.mu.Unlock(); return nil }
    as := cs.activeAS
    if as == nil || !as.IsReady() { cs.mu.Unlock(); return nil }
    p := cs.buildPromptLocked()  // 队列入锁后构造
    cs.mu.Unlock()
    
    err := as.Submit(p)  // 阻塞,锁外
    
    cs.mu.Lock()
    defer cs.mu.Unlock()
    if err != nil {
        return err  // queue 不动
    }
    cs.queue = nil
    for _, mid := range p.MessageIDs {
        if v, ok := cs.messagesByID.Load(mid); ok {
            msg := v.(*Message)
            msg.Stage = agent.MessageSubmitted
            cs.EmitMessageState(msg.ID, agent.MessageSubmitted)  // survivor callback
        }
    }
    return nil
}
```

`buildPrompt` 拆成 `buildPromptLocked` (要求 cs.mu 持有) 和 `buildPrompt` (独立调用,内部取锁)。

---

## 7. 生命周期-事件触发表

| 触发位置 | 事件 |
|---|---|
| `as.Spawn()` 完成后 (handle 已就位) | readpump 启动 (Phase 1 实现:在 Spawn 末尾 `as.startReadPump()`) |
| `readpumpLoop` `!ok` 分支 | `KindLifecycle{Status: StatusExited}` + `endPrompt(ProcessDied)` |
| `as.Shutdown()` (正常结束) | 关闭 readpump + 关闭 eventQueue |
| `/kill` 命令 (T14 接线) | `as.Shutdown()` → 触发上面 |
| `endPrompt(reason)` 后 | `KindPromptEnded{Prompt: p, EndReason: reason}` |
| 桥事件透传 | `KindAgentEvent{AgentEvent: &ev}` |

**注**:KindLifecycle{Status: StatusRunning} 由 readpump 启动时桥层
EventInit 触发的 AgentEvent 自然产生(运行时通过 `KindAgentEvent` 透传,
不单独发 Lifecycle 事件)。设计 §11 列了 SetRunning 触发,但实际
实现:运行时只关心 EventInit 携带的 SessionID 和 Model,不需要独立的
Spawned 事件。Phase 1.x 后续 PR 可能添加专门的 Lifecycle 事件用于
`nightme health` 观测。

---

## 8. readpump 跨 AS 自治

### 6.1 生命周期

- **启动**:AS 首次 `Activate` 时启动。每个 AS 自己的 readpump。
- **运行**:`/use` 切走时**不停止**。旧 AS 在后台跑,readpump 持续消费 events,enriched 后推到
  eventQueue 累积。
- **停止**:AS `Shutdown()` 时关 handle → `handle.Events()` channel 关闭 → readpump 退出 → 关闭
  eventQueue。

### 6.2 关键不变量

- eventQueue 容量 **256**(见 §7)
- eventQueue 满后行为:读端阻塞 → 桥层 readLoop 阻塞 → 桥层实现的 backpressure 策略生效
- 旧 AS 累积的事件在切回时由 ChatSession 继续消费,**不断流**

### 6.3 Main loop 在 ChatSession 侧

```go
for ev := range cs.activeAS.Events() {
    cs.routeEvent(ev)
    if ev.Kind == KindPromptEnded {
        cs.writebackMessageState(ev.Prompt)
    }
}
```

`cs.activeAS` 在 `/use` 切换时改变,后续 `<-cs.activeAS.Events()` 解析到新的 channel——这是
**引用解析**而非 channel 切换,旧的 eventQueue 仍然可达、可继续读。

---

## 9. Queue 语义:至少一次成功提交

### 7.1 语义定义

`ChatSession.queue` 不是普通 FIFO,是**至少一次成功提交队列**:

- 头元素 = 当前正在尝试提交的那批
- 成功提交 → 出队
- 失败提交 → 头元素不动,等下一次 TryFlush
- **没有**重试次数上限、**没有**指数退避、**没有**手动重试入口
- 唯一能让人盯死态 = AS 永久死(Phase 1 接受,Phase 2 L1 Pinger 解决)

### 7.2 自愈路径

```
Submit 失败 → msg.Stage 留 Queued,queue 不动
       ↓
IsReady() 翻转 = endPrompt(EventDone/Error/!ok/stalled)
       ↓
TryFlush() 看到 IsReady=true,自动重试
       ↓
queue 头重新 build → Submit
       ↓
成功 → 出队 + msg.Stage = Submitted
```

链条闭合,**不需要额外的 retry 逻辑**。

### 7.3 关于 /use 切走

- 旧 AS 上的 in-flight Prompt 继续跑(/use 不取消 opCtx)
- 旧 AS 的 events 累积到 eventQueue(cap 256)
- 用户在 ChatSession 上发新消息 → 进 queue → 试 Submit 到**当前 activeAS**(可能是新的)
- 如果用户要给旧 AS 发消息,需要 /use 切回去

**结论**:消息永远发向当前激活的 AS。已经发出去的,在原 AS 上跑完。

---

## 10. EnrichedEvent 容量与丢弃策略

eventQueue 默认 **256**。理由:

- 桥层 cap 64 是 burst 缓冲,目的是让 AS 不阻塞
- chat 层 eventQueue 是**持久累积**窗口,语义是"AS 在后台跑时,chat 切换积累了事件的窗口"
- 256 是 2 KiB 不到(64-byte 事件估算,256 * 64 = 16KB),完全在内存承受范围
- 跟"Prompt 持续累积但最终会发完 EventDone"匹配,不会无限增长

**降级策略**:Prompt 结束事件(`KindPromptEnded`)和生命周期事件(`KindLifecycle`)由 readpump
**强制 flush**(即使 queue 满也保证送达),稳态事件(`KindAgentEvent`)可丢最老的。

---

## 11. Phase 1 范围

本设计落地的一次性 PR(Phase 1)范围:

| 改动 | 范围 |
|---|---|
| `internal/chatsession/events.go` 新增 EnrichedEvent 类型 | 新增 |
| `Background()` → no-op,`Activate()` → 幂等,`Shutdown()` 新增 | 改动 |
| `AgentSession` 新增 `isReady atomic.Bool` / `eventQueue chan EnrichedEvent` | 改动 |
| `AgentSession` 新增 `Submit(p *Prompt) error` / `IsReady()` / `Events()` | 新增 |
| `AgentSession` 内部 readpump(first Activate 起,Shutdown 停) | 新增 |
| `AgentSession.endPrompt(reason)` 内部化 | 改动 |
| `ChatSession.promoteActiveLocked` 简化 | 改动 |
| `ChatSession.TryFlush()` / `buildPrompt()` / `writebackMessageState()` 新增 | 新增 |
| `defaultPromptHookLocked` 删除 | 删除 |
| `InputBuffer.flushPending` / `StateBusy` / `StateIdle` 简化 | 改动 |
| `cs.Run/StopReadPump` 删除(readpump 跟 AS) | 删除 |
| `runtime` (cmd/nightme/run.go) 改读 `cs.ActiveEvents()` | 改动 |
| `onMessageState` / `onPromptEnd` / `EventHandler` callback 删除 | 删除 |

**Phase 1 不做**:
- L1 Pinger / ping 失败触发 respawn
- L2 stall watchdog / Stall 检测
- /use 切回时基于快照的"补投"(L1.5 方案 A)
- `nightme health` 扩展
- `Prompt` 持久化

---

## 12. 一句话总结

**ChatSession 收成"队列 + 路由 + 写回"的薄壳,AgentSession 收成"Prompt 生命周期 + 事件流"
的自治单元,中间用 3 个方法 + 1 条 EnrichedEvent 流绑定。OpContext 跟 AS 不跟 activeAS,
Background 变 no-op,Submit 失败发自愈。**

---

## 13. References

- [`docs/feat/message_lifecycle.md`](../docs/feat/message_lifecycle.md) — Phase 0 设计
- [`tasks/wip-message-prompt.md`](./wip-message-prompt.md) — Phase 0 实施计划
- [`tasks/wip.md`](./wip.md) — L1/L2 探活 + L1.5 离线 AS 跟踪(本设计的地基之一)
- `internal/agent/agent.go` AgentSession 接口
- `internal/chatsession/agentsession.go` 当前实现
- `internal/chatsession/chatsession.go` 当前实现
- `internal/chatsession/readpump.go` 当前实现(将被删除)
- `internal/chatsession/input_buffer.go` 当前实现(将被简化)
