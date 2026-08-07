# ChatSession ↔ AgentSession 边界重构 —— 实施计划（Phase 1，单 PR）

> **Status**: 待用户确认后开工
> **Design**: [`tasks/wip-cs-as-boundary.md`](./wip-cs-as-boundary.md)（先读这个，理解"为什么"和"最终形态";
>   本文档只管"怎么改代码"）
> **依赖**:Phase 0 (`Message`/`Prompt` 对象化) 已合到 `feat/alive` 分支
> **Scope**: `internal/chatsession` + `cmd/nightme/run.go` + 全部受影响的测试
> **Out of scope(明示不做)**:
>   - L1 Pinger / ping 失败触发 respawn(下一 PR)
>   - L2 stall watchdog(下一 PR)
>   - L1.5 离线 AS 跟踪的"快照补投"实现(下一 PR,本设计的 eventQueue 256 容量是基础)
>   - `nightme health` 扩展(Phase 4)
>   - `Prompt` 持久化(独立话题)

---

## 1. 命名约定(贯穿所有任务)

```go
// internal/chatsession/events.go (新增)
type EnrichedEvent struct {
    ChatID         string
    AgentSessionID string
    UserMsgID      string
    PromptID       string
    Prompt         *Prompt  // 引用,AS 持有,运行时按需读
    Kind           EnrichedEventKind
    AgentEvent     *agent.AgentEvent
    PromptEnded    *PromptEndedChange
    Lifecycle      *LifecycleChange
}

type EnrichedEventKind int
const (
    KindAgentEvent EnrichedEventKind = iota
    KindPromptEnded
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

// eventQueue 容量
const eventQueueCapacity = 256

// AgentSession 新增公开方法
func (as *AgentSession) Submit(p *Prompt) error
func (as *AgentSession) IsReady() bool
func (as *AgentSession) Events() <-chan EnrichedEvent
func (as *AgentSession) Shutdown()  // 新增,真取消 opCtx

// AgentSession 行为变更
func (as *AgentSession) Activate(parent context.Context)  // 改:幂等,首次设 opCtx
func (as *AgentSession) Background()                     // 改:no-op
func (as *AgentSession) Shutdown()                       // 新增:取消 opCtx + 关 readpump + 关 eventQueue

// ChatSession 保留 callback
func (cs *ChatSession) EmitMessageState(userMsgID string, state agent.MessageState)
func (cs *ChatSession) SetMessageStateHandler(h func(chatID, userMsgID string, state agent.MessageState))
// 唯一保留的 callback,用于 MessageState wire event (不归 AS 域)
```

---

## 2. Wire 时机

| 事件 | 触发点 | 调用位置 |
|---|---|---|
| `EnrichedEvent{KindAgentEvent}` | 每条桥事件 | `AgentSession.readpump` 内部 |
| `EnrichedEvent{KindPromptEnded}` | endPrompt(reason) | `AgentSession.endPrompt` |
| `EnrichedEvent{KindLifecycle}` | AS 启动 / 进程崩 / /kill | `AgentSession.readpump` !ok 分支 + `SetRunning` 后 |

**ChatSession 收到 `KindPromptEnded` 后**:
- 遍历 `ev.Prompt.MessageIDs`
- 对每个 msg 设 `msg.LastProcessedAt = ev.Prompt.EndedAt` / `msg.LastPromptID = ev.Prompt.ID` / `msg.LastEndReason = ev.Prompt.EndReason`
- `msg.Stage` 不动(仍是 Submitted 终态)
- 调 `cs.firePromptEnded(msgID, reason)` 通知 runtime 渲染 ✅/❌

## 2. Wire 时机

| 事件 | 触发点 | 调用位置 |
|---|---|---|
| `MessageState{Submitted}` | Submit 成功 | `ChatSession.TryFlush` |
| `MessageState{Dropped}` | `MarkDropped` 调用 | `ChatSession.BufferClear` / `/kill` / `/new` |
| `MessageState{Queued}` | dispatcher 拿到 inbound | `cmd/nightme/run.go` newMessageDispatcher |
| `EnrichedEvent{KindAgentEvent}` | 每条桥事件 | `AgentSession.readpump` 内部 |
| `EnrichedEvent{KindPromptEnded}` | `endPrompt(reason)` | `AgentSession.endPrompt` |
| `EnrichedEvent{KindLifecycle}` | Spawned / `!ok` / Shutdown | `as.SetRunning` 后 / `readpump !ok` / `as.Shutdown` |

**MessageState 事件 (Submitted / Dropped / Queued) 保留 `cs.EmitMessageState` survivor callback**,
理由:MessageState 是 ChatSession 域事件,不是 AS 域;`onMessageState` 字段保留,runtime 在启动时
通过 `cs.SetMessageStateHandler` 注入。

**ChatSession 收到 `KindPromptEnded` 后**:
- 遍历 `ev.Prompt.MessageIDs`
- 对每个 msg 设 `msg.LastProcessedAt = ev.Prompt.EndedAt` / `msg.LastPromptID = ev.Prompt.ID` / `msg.LastEndReason = ev.Prompt.EndReason`
- `msg.Stage` 不动(仍是 Submitted 终态)
- 调 `cs.firePromptEnded(msgID, reason)` 通知 runtime 渲染 ✅/❌

**明确不在 ChatSession 触发**:
- 不再有 `onPromptEnd` callback → `PromptEnded` 改为 EnrichedEvent 流
- 不再有 `EventHandler` callback → 全部 AS 事件走 `cs.ActiveEvents()` 流

---

## 3. Queue 语义(至少一次成功提交)

```
ChatSession.queue 语义:
  - 头元素 = 当前正在尝试提交的那批
  - 成功 Submit → 出队 + msg.Stage = Submitted
  - 失败 Submit → 头元素不动,等下一次 TryFlush
  - 没有 retry counter / backoff / max attempts
  - 唯一死态 = AS 永久死(Phase 1 接受,Phase 2 L1 Pinger 解决)

TryFlush 触发位置:
  - 新消息入队时(QueueMessage)
  - `KindPromptEnded` 事件后(等 AS 跑完)
  - /use 切换 AS 后
  - 新 activeAS 从非 Ready 翻 Ready 后(等 PromptEnded 事件触发)

TryFlush 串行化:全程 `cs.mu` 持有,Submit 阶段临时释放(SendBlocks 阻塞时无锁)。

**至少一次成功提交的边界**:
- Submit 失败 → 不出队 + msg.Stage 留 Queued
- 等下一次 TryFlush 自愈
- 唯一死态 = AS 永久死(Phase 1 接受,Phase 2 L1 Pinger 解决)
```

---

## 4. 数据归属

| 数据 | 归属 | 锁 |
|---|---|---|
| `*Message` / `messagesByID` | ChatSession | ChatSession.mu |
| `queue` | ChatSession | ChatSession.mu (TryFlush 全程持有) |
| `*Prompt` (in-flight) | AgentSession | asMu (Submit / readpump / endPrompt 同步) |
| `currentPrompt` | AgentSession 私有 | asMu |
| `isReady` | AgentSession | atomic.Bool |
| `eventQueue` | AgentSession | chan 锁 (内置) |
| `opCtx` / `opCancel` | AgentSession | asMu |
| `handle` | AgentSession | asMu |

**锁顺序**:
- `cs.mu` 不再触碰 `as.currentPrompt` / `as.isReady` / `as.eventQueue`
- AS 内部 readpump 单 goroutine 访问 currentPrompt/eventQueue,无锁
- 唯一跨 AS 边界的访问 = `as.Events()` 返回 `<-chan`,由 chan 自身同步

---

## 5. 任务分解

### T01. chatsession：EnrichedEvent 类型定义

**新增**:`internal/chatsession/events.go`
- `EnrichedEvent` struct
- `EnrichedEventKind` enum + 3 个常量
- `PromptEndedChange` / `LifecycleChange` struct
- `eventQueueCapacity = 256` const

**依赖**:无
**AC**:类型定义编译通过;字段与本文档 §1 一致

### T02. AgentSession：isReady atomic + eventQueue + readpump 启动位

**改**:`internal/chatsession/agentsession.go`
- 加 `isReady atomic.Bool`(初始 true)
- 加 `eventQueue chan EnrichedEvent` (cap 256,构造时创建)
- 加 `readpumpStarted bool`(用于首次 Activate 启动 readpump,subsume 原 `ObserveClose` 概念)
- 加 `readpumpStop chan struct{}` / `readpumpDone chan struct{}`
- `NewAgentSession` 构造 eventQueue

**依赖**:T01
**AC**:字段添加完成,eventQueue 已经有 owner

### T03. AgentSession：Background no-op + Activate 幂等 + Shutdown 新增

**改**:`internal/chatsession/agentsession.go`
- `Background()`:函数体改成空,保留作为 API 兼容
- `Activate(parent context.Context)`:加幂等判断
  ```go
  if as.opCtx != nil { return }
  as.opCtx, as.opCancel = context.WithCancel(parent)
  ```
- `Shutdown()`:新增,真正取消 opCtx,关闭 readpump,关闭 eventQueue

**依赖**:T02
**AC**:Background 调用 no-op;Activate 多次调用只设一次 opCtx;Shutdown 取消 opCtx + 关闭 eventQueue

### T04. AgentSession：readpump goroutine

**新增**:`internal/chatsession/agentsession.go`
- `startReadPump()`:首次 Activate 时启动,`go as.readpumpLoop()`
- `readpumpLoop()`:
  ```go
  for {
      select {
      case <-as.readpumpStop:
          return
      case ev, ok := <-as.handle.Events():
          if !ok {
              // 进程崩 / 退
              as.endPrompt(PromptEndProcessDied)  // 修复死锁
              as.eventQueue <- EnrichedEvent{
                  Kind: KindLifecycle,
                  Lifecycle: &LifecycleChange{Status: StatusExited},
              }
              return
          }
          // enrich 事件
          var userMsgID, promptID string
          if p := as.currentPrompt; p != nil {
              userMsgID = p.LastMessageID
              promptID = p.ID
          }
          as.eventQueue <- EnrichedEvent{
              ChatID: as.ChatSessionID,
              AgentSessionID: as.ID,
              UserMsgID: userMsgID,
              PromptID: promptID,
              Prompt: as.currentPrompt,
              Kind: KindAgentEvent,
              AgentEvent: &ev,
          }
          // 终态收口
          if ev.Kind == EventDone || ev.Kind == EventError {
              reason := PromptEndClean
              if ev.Kind == EventError { reason = PromptEndError }
              as.endPrompt(reason)
          }
      }
  }
  ```

**依赖**:T03
**AC**:!ok 分支调 endPrompt(ProcessDied);每条桥事件 enrich 后推 eventQueue;终态事件调 endPrompt

### T05. AgentSession：endPrompt 内部化

**改**:`internal/chatsession/agentsession.go`
- `endPrompt(reason PromptEndReason)` 改成 AS 内部方法(无 cs.mu 依赖)
- 逻辑:
  ```go
  func (as *AgentSession) endPrompt(reason PromptEndReason) {
      if as.currentPrompt == nil {
          return
      }
      p := as.currentPrompt
      p.EndedAt = time.Now()
      p.EndReason = reason
      as.currentPrompt = nil
      as.isReady.Store(true)
      
      // 推送 PromptEnded 事件
      as.eventQueue <- EnrichedEvent{
          ChatID: as.ChatSessionID,
          AgentSessionID: as.ID,
          UserMsgID: p.LastMessageID,
          PromptID: p.ID,
          Prompt: p,
          Kind: KindPromptEnded,
          PromptEnded: &PromptEndedChange{
              EndedAt: p.EndedAt,
              EndReason: reason,
          },
      }
  }
  ```

**依赖**:T04
**AC**:endPrompt 单一收口;事件推 eventQueue;isReady 翻转

### T06. AgentSession：Submit / IsReady / Events 公开方法

**改**:`internal/chatsession/agentsession.go`
- `Submit(p *Prompt) error`:
  ```go
  func (as *AgentSession) Submit(p *Prompt) error {
      if as.handle == nil {
          return ErrNotRunning
      }
      p.ID = as.NewPromptID()
      p.AgentSessionID = as.ID
      p.AckedAt = time.Now()
      p.LastProgressAt = time.Now()
      
      // SendBlocks 在 opCtx 之外执行
      err := as.handle.SendBlocks(as.opCtx, p.Blocks)
      if err != nil {
          return err
      }
      
      as.currentPrompt = p
      as.isReady.Store(false)
      return nil
  }
  ```
- `IsReady() bool`:`return as.isReady.Load()`
- `Events() <-chan EnrichedEvent`:`return as.eventQueue`

**依赖**:T05
**AC**:Submit 成功填 ID/时间戳 + 置 currentPrompt + 翻 isReady;SendBlocks 失败回 err 不动状态

### T07. ChatSession：promoteActiveLocked 简化

**改**:`internal/chatsession/chatsession.go`
- `promoteActiveLocked`:删 `prev.Background()` 调用,代码缩到 5 行
- 同步检查:`promoteActive` 路径不再触动 AS 内部状态

**依赖**:T06
**AC**:编译通过;flying / no-op 处理正确

### T08. ChatSession：buildPrompt + TryFlush + writebackMessageState

**新增**:`internal/chatsession/chatsession.go`
- `buildPromptLocked()` (callsite 持有 cs.mu):
  ```go
  func (cs *ChatSession) buildPromptLocked() *Prompt {
      ids := make([]string, 0, len(cs.queue))
      var blocks []agent.ContentBlock
      for _, m := range cs.queue {
          ids = append(ids, m.ID)
          blocks = append(blocks, m.Blocks...)
      }
      return &Prompt{
          ChatSessionID: cs.ID,
          MessageIDs:    ids,
          Blocks:        blocks,
          CreatedAt:     time.Now(),
      }
  }
  ```
- `TryFlush()`:
  ```go
  func (cs *ChatSession) TryFlush() error {
      cs.mu.Lock()
      if len(cs.queue) == 0 { cs.mu.Unlock(); return nil }
      as := cs.activeAS
      if as == nil || !as.IsReady() { cs.mu.Unlock(); return nil }
      p := cs.buildPromptLocked()
      cs.mu.Unlock()

      // Submit 锁外(SendBlocks 阻塞)
      err := as.Submit(p)

      cs.mu.Lock()
      defer cs.mu.Unlock()
      if err != nil { return err }  // queue 不动,等下次 TryFlush
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
- `writebackMessageState(p *Prompt)`:
  ```go
  func (cs *ChatSession) writebackMessageState(p *Prompt) {
      cs.mu.Lock()
      for _, mid := range p.MessageIDs {
          if v, ok := cs.messagesByID.Load(mid); ok {
              msg := v.(*Message)
              msg.LastProcessedAt = p.EndedAt
              msg.LastPromptID = p.ID
              msg.LastEndReason = p.EndReason
          }
      }
      cs.mu.Unlock()
      // 触发 runtime 渲染
      cs.firePromptEnded(p.LastMessageID, p.EndReason)
  }
  ```

**依赖**:T07
**AC**:TryFlush 全程 cs.mu 串行化(Submit 阶段临时释放);buildPromptLocked 要求 cs.mu 持有;
EmitMessageState (survivor callback) 在 Submit 成功后正确触发;queue 失败时不动

### T09. ChatSession：QueueMessage 简化

**改**:`internal/chatsession/chatsession.go`
- `QueueUserMessage(msg *Message)`:入 `messagesByID` + 入 `queue` + emit `MessageQueued` + 调 `TryFlush()`

**依赖**:T08
**AC**:入队后立即触发 TryFlush;queue F/I 状态正确

### T10. ChatSession：主循环读 ActiveEvents

**改**:`internal/chatsession/chatsession.go`
- 加 `ActiveEvents() <-chan EnrichedEvent` = `cs.activeAS.Events()`(处理 nil)
- runtime 改读 `cs.ActiveEvents()` 而不是 callback
- delete `cs.SetEventHandler` / `cs.EventHandler` / `cs.SetMessageStateHandler` / `cs.SetPromptEndHandler` 公开 API

**依赖**:T09
**AC**:runtime 编译通过;事件路由改流驱动

### T11. chatsession：删除 defaultPromptHookLocked

**改**:`internal/chatsession/chatsession.go`
- 删除 `defaultPromptHookLocked` 方法
- 删除 `cs.SetPromptHook` / `cs.SetFlushHook` 公开 API
- 删除 `cs.onPromptEnd` 字段

**依赖**:T10
**AC**:无 callback 残留

### T12. chatsession：删除 InputBuffer FSM

**改**:`internal/chatsession/input_buffer.go`
- `InputBuffer` 缩成队列容器,删 `state atomic.Int32` / `StateIdle` / `StateBusy` / `SetState`
- `flushPending` / `OnTurnEnded` 改为 `DrainQueue() []*Message`(返回切片,不调 hook)
- `Add` / `Clear` / `Flush` 保留,但 Add 不再触发 flush(由 ChatSession.TryFlush 触发)

**依赖**:T11
**AC**:InputBuffer 形态缩成"无 FSM 的切片 + 简单锁"

### T13. chatsession：删除 readpump

**改**:`internal/chatsession/readpump.go`
- 删除 `EventPump` / `EventPumpState` / `EventHandler` / `StartReadPump` / `StopReadPump` / `runReadPump` / `HasPump`
- 删除 `cs.SetEventHandler` 字段引用
- 删除 `cs.EventHandler` 字段

**依赖**:T12
**AC**:readpump.go 文件可整体删除或留空

### T14. cmd/nightme/run.go：runtime 改读 cs.ActiveEvents

**改**:`cmd/nightme/run.go`
- 删除 `wireRuntimeCallbacksAndRestore` 中的 `cs.SetEventHandler` / `cs.SetPromptEndHandler` / `cs.SetMessageStateHandler` 调用
- 新增 `go cs.PumpEvents(ctx)`:从 `cs.ActiveEvents()` 读,按 `EnrichedEvent.Kind` 路由
  - `KindAgentEvent` → 调 `cs.ProcessAgentEvent(ev)` (旧 EventHandler 逻辑)
  - `KindPromptEnded` → 调 `cs.writebackMessageState` + 渲染
  - `KindLifecycle` → 调 `cs.ProcessLifecycleEvent(ev)`
- 删除 `cs.OnTurnEnded` / `cs.SetIdle` / `cs.SetBusy` 引用

**依赖**:T13
**AC**:runtime 走流驱动;无 callback 残留

### T15. Message 字段扩展(LastProcessedAt / LastPromptID / LastEndReason)

**改**:`internal/chatsession/message.go`
- 加 `LastProcessedAt time.Time`
- 加 `LastPromptID string`
- 加 `LastEndReason PromptEndReason`

**依赖**:T11
**AC**:消息字段就绪

### T16. 测试更新：chatsession

**改**:`internal/chatsession/*_test.go`
- `input_buffer_test.go`:删 `FlushHook` / `StateIdle` / `StateBusy` 相关assert
- `readpump_*` 测试:删掉(readpump 不再属于 ChatSession),改测 AS 内部 readpump
- `chatsession_test.go`:TryFlush / writebackMessageState 覆盖
- 新增 `agentsession_readpump_test.go` 覆盖 AS 内部 readpump + endPrompt + !ok 分支

**依赖**:T14
**AC**:`go test ./internal/chatsession/...` 全绿

### T17. 测试更新：feishu

**改**:`internal/channel/feishu/*_test.go`
- runtime 接入方式变更后,删 `EventHandler` mock 相关
- `Receipt` 渲染测试:确保 `EnrichedEvent` 路由后 `PromptEnded` 触发 ✅/❌ reaction 正确

**依赖**:T16
**AC**:`go test ./internal/channel/feishu/...` 全绿

### T18. 全量验证

**执行**:`go vet ./... && go build ./... && go test ./...`
- 修任何残留
- 跑 `readpump_real_pi_test`(如果可跑)

**依赖**:T17 完成
**AC**:全绿;无 vet warning

### T19. 文档同步

**改**:
- `docs/FEATURES.md`:勾掉/更新 `Exit observer wiring` 条目(已接线)
- `docs/feat/message_lifecycle.md`:补一节"CS-AS 边界 v2"
- `docs/SPEC.md` §2.5:如需更新
- commit message:明示"On Turn Ended / InputBuffer FSM 已删,Prompt 生命周期下沉到 AS"

**依赖**:T18 完成
**AC**:文档与代码一致

---

## 6. 关键实现细节

### 6.1 readpump 启动时机

跟 AS 首次 Activate 同步启动,跟 AS 生命周期对齐:

```go
func (as *AgentSession) Activate(parent context.Context) {
    as.asMu.Lock()
    defer as.asMu.Unlock()
    if as.opCtx != nil {
        return
    }
    as.opCtx, as.opCancel = context.WithCancel(parent)
    // 首次 Activate 启动 readpump
    if !as.readpumpStarted {
        as.readpumpStarted = true
        as.readpumpStop = make(chan struct{})
        as.readpumpDone = make(chan struct{})
        go as.readpumpLoop()
    }
}
```

`/use` 切换 AS 时:`promoteActiveLocked` 只设 `cs.activeAS = as`,不动 AS 内部 readpump。

### 6.2 `Shutdown` 路径

```go
func (as *AgentSession) Shutdown() {
    as.asMu.Lock()
    if as.opCancel != nil {
        as.opCancel()
    }
    as.asMu.Unlock()
    
    // 等 readpump 退出
    <-as.readpumpDone
    
    // 关闭 eventQueue
    close(as.eventQueue)
}
```

ChatSession 关闭时,pool 里所有 AS 都要 Shutdown。`/kill` 显式调 Shutdown(不通过 Background)。

### 6.3 PromoteActive / DemoteActive 行为差异

| 操作 | 当前 | 新 |
|---|---|---|
| `promoteActiveLocked(activeAS)` | 调 `prev.Background()` 取消 opCtx | 仅设 `cs.activeAS = as` + 首次 Activate |
| `/use` 切换 | 旧 AS 被 Background | 旧 AS 继续跑,readpump 不停 |
| `cs.activeAS` 重新解析 | 每次访问都要 lock | 同 |

### 6.4 EnrichedEvent 路由

runtime 入口(`cmd/nightme/run.go`):

```go
func (cs *ChatSession) PumpEvents(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            return
        case ev, ok := <-cs.ActiveEvents():
            if !ok {
                return
            }
            switch ev.Kind {
            case KindAgentEvent:
                cs.processAgentEvent(ev)
            case KindPromptEnded:
                cs.writebackMessageState(ev.Prompt)
            case KindLifecycle:
                cs.processLifecycleEvent(ev)
            }
        }
    }
}
```

### 6.5 `Message.Stage` 写操作位置

| 状态 | 触发位置 | 时机 |
|---|---|---|
| `MessageQueued` | `cmd/nightme/run.go` dispatcher | 消息入队时 |
| `MessageSubmitted` | `ChatSession.TryFlush` | Submit 成功时 |
| `MessageDropped` | `ChatSession.MarkDropped` | BufferClear / /kill / /new 主动清空 |

**没有任何路径在 AS 内部写 `Message.Stage`** —— AS 不持有 Message 引用,符合职责边界。

### 6.6 `defaultPromptHookLocked` 退场

彻底删除。`cs.SetPromptHook` / `cs.SetFlushHook` 公开 API 也删除(Phase 0 alias 配合删除)。

`cs.onPromptEnd` 字段删除,`cs.WritebackMessageState` 替代其作用。

### 6.7 `cs.SetExited` 路径

当前 `cs.SetExited(code)` 在 readpump `!ok` 分支调,改由 AS 内部 readpump `!ok` 分支调
`as.endPrompt(ProcessDied)` 代替。`cs.SetExited` 公开行为(写 as.exitCode)仍然存在,但触发点
在 AS 内部,不再经过 ChatSession。

---

## 7. 风险与回滚

| 风险 | 影响 | 缓解 |
|---|---|---|
| `Background()` 改 no-op 跟现有调用方语义不匹配 | `/use` 切换时旧 AS 还在跑,reader 期待 cancel | 接受;commit message 明示;SetExited 路径独立 |
| `currentPrompt` 写从 cs.mu 改为 AS 内部不加锁 | AS 内部 readpump 单 goroutine 访问,无竞争 | 接受;AS 内部访问本身有序 |
| `defaultPromptHookLocked` 删除触发编译失败 | 现存 caller 多 | T11 删前先 T10 接入,逐步迁移 |
| `EventHandler` callback 删除触发编译失败 | cmd/nightme/run.go 依赖 | T14 同步改 |
| eventQueue 256 容量在极端 case 不够 | 大 Prompt 跑时丢中间事件 | 强制 flush 终态事件;丢的只是中间过程,receipt card 不丢 |
| `/use` 切走时旧 AS 仍在累积 events 占用内存 | 长期不切回时累积 | 256 上限;Prompt 结束是天然终止点;L1.5 后续处理 |

**回滚**:单 PR,回滚即 `git revert`;无数据迁移。

---

## 8. 执行顺序一览

```
T01 (EnrichedEvent 类型) ─┐
                          ├─→ T02 (AS fields) ─→ T03 (Background/Activate/Shutdown) ─→ T04 (readpump) ─→ T05 (endPrompt) ─→ T06 (Submit/IsReady/Events) ─┐
                          │                                                                                                                              │
                          │                                                                                                                              ├─→ T07 (promoteActive 简化) ─→ T08 (buildPrompt/TryFlush/writeback) ─→ T09 (QueueMessage) ─→ T10 (ActiveEvents) ─→ T11 (defaultPromptHook 删) ─┐
                          │                                                                                                                                                                                                                       │
                          │                                                                                                                                                                                                                       ├─→ T12 (InputBuffer 简化) ─→ T13 (readpump 删) ─→ T14 (runtime 改流) ─→ T15 (Message 字段) ─→ T16 (tests/chatsession) ─→ T17 (tests/feishu) ─→ T18 (全量验证) ─→ T19 (文档)
                          │
                          T01 提前可独立起手
```

依赖图:T01 单独无依赖,T02-T19 串行推进,T15 可与 T11-T14 并行。

---

## 9. References

- [`tasks/wip-cs-as-boundary.md`](./wip-cs-as-boundary.md) — 设计文档
- [`docs/feat/message_lifecycle.md`](../docs/feat/message_lifecycle.md) — Phase 0 设计
- [`tasks/wip-message-prompt.md`](./wip-message-prompt.md) — Phase 0 实施计划
- [`tasks/wip.md`](./wip.md) — L1/L2 探活 + L1.5 离线 AS 跟踪
- `internal/agent/agent.go` AgentSession 接口
- `internal/chatsession/agentsession.go` 当前实现
- `internal/chatsession/chatsession.go` 当前实现
- `internal/chatsession/readpump.go` 即将删除
- `internal/chatsession/input_buffer.go` 即将简化
- `cmd/nightme/run.go` 即将改流驱动
