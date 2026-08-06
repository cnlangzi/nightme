# ChatSession → AgentSession → Conversation 三层同步控制流

> 这份文档讲清楚 nightme runtime 怎么把"一个 IM chat"映射到"一段对话"上。
> 三层:
> - **ChatSession**(per-IM-chat):会话级状态、池、ctx
> - **AgentSession**(per-(agent,cwd)):子进程句柄、生命周期
> - **Conversation / Turn**(per-message):单次往返、ctx 派生、事件消费

## 0. 三层概览

```
┌─────────────────────────────────────────────────────────────────┐
│  IM Chat (Feishu chatID)                                        │
│                                                                 │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │  ChatSession  (cs.mu)                                     │  │
│  │  ┣━ activeCwd / activeAgent / activeAS                    │  │
│  │  ┣━ pool map[(agent, cwd)] → *AgentSession               │  │
│  │  ┣━ currentTurnUserMsgID                                 │  │
│  │  ┣━ inputBuffer (FSM Idle↔Busy) + flushHook              │  │
│  │  ┣━ eventHandler (runtime-installed, persists across /use)│  │
│  │  ┣━ pump / pumpRunning (readPump controller)             │  │
│  │  ┣━ asCtxMu, asCtx, asCancel    ← per-AS ctx            │  │
│  │  ┗━ turnCtxMu, turnCtx, turnCancel ← per-turn ctx        │  │
│  │         (turnCtx is derived from asCtx)                   │  │
│  └───────────────────────────────────────────────────────────┘  │
│            │                                                     │
│            │ activeAS                                             │
│            ▼                                                     │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │  AgentSession  (handleMu, status, ...)                    │  │
│  │  ┣━ ID, Agent, Cwd, args, ResumeID                        │  │
│  │  ┣━ pid (atomic.Int32), stat (Status: Running/Detached/   │  │
│  │  ┃   Exited, guarded by sync.RWMutex)                    │  │
│  │  ┣━ handle agent.AgentSession  ← bridge-level live       │  │
│  │  ┃   session (nil until Spawn succeeds)                   │  │
│  │  ┣━ cumulativeUsage, compactionCount (F-45/F-49)          │  │
│  │  ┗━ model, exitCode                                       │  │
│  └───────────────────────────────────────────────────────────┘  │
│            │                                                     │
│            │ handle                                              │
│            ▼                                                     │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │  bridge session (pi / claudecode / acp / pty)             │  │
│  │  ┣━ cmd, stdin/stdout pipes, rpc client                   │  │
│  │  ┣━ events chan agent.AgentEvent (buffered 64)           │  │
│  │  ┣━ turnMu / turnActive (only one prompt at a time)      │  │
│  │  ┣━ readPump goroutine (stdout → events)                 │  │
│  │  ┣━ lifecycle goroutine (cmd.Wait + close(events))       │  │
│  │  ┣━ closeOnce / closed / exitDone / pumpWG               │  │
│  │  ┗━ (per-turn) ctx flows in: SendBlocks(ctx, blocks)     │  │
│  └───────────────────────────────────────────────────────────┘  │
│                                                                 │
│  ReadPump goroutine (per ChatSession):                          │
│     chat-side: events <- as.Events()                            │
│                handler(ev) -> ch.Send(OutboundMessage)          │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

**ctx 派生链(自上而下,Cancel 自下而上级联):**

```
context.Background()
    └─ asCtx              (per-AS, lives with active AS)
         └─ turnCtx       (per-turn, BeginTurn 时 derive 自 asCtx)
              └─ bridge ctx (SendBlocks(ctx, ...) 接收的就是 turnCtx)
                   └─ bridge 内部的 rpc.request(ctx, ...) select
```

cancelling asCtx → cascade cancel turnCtx → cascade cancel bridge 的 in-flight `prompt` RPC。
cancelling turnCtx alone → 只干掉当前 turn,下一个 BeginTurn 仍从 asCtx 派生。

---

## 1. ChatSession(per-chat)

### 字段分类

| 字段 | 类型 | 锁 | 说明 |
|------|------|------|------|
| `ID`, `ChatID` | string | 不可变 | 由 |
| `activeCwd`, `activeAgent` | string | `mu` | /cwd / /use 改;LookupActiveAgentSession 读 |
| `primaryAgent` | string | 不可变 | cfg.Primary 快照,只读 |
| `pool` | `map[agentCwdKey]*AgentSession` | `mu` | (agent, cwd) → AS 句柄 |
| `activeAS` | `*AgentSession` | `mu` | 当前激活的 AS;`/use` + Lookup 改变 |
| `currentTurnUserMsgID` | string | `mu` | 本轮 single anchor;`/use` 清掉 |
| `inputBuffer` | `*InputBuffer` | lazy init | FSM Idle↔Busy + 队列 |
| `watchMode` / `thinkMode` / `toolsMode` | enums | `mu` | 聊天级偏好 |
| `eventHandler` | `EventHandler` | `mu` | runtime 装一次,跨 /use 持久 |
| `onMessageState`, `onReaction` | callbacks | `mu` | F-31 / F-46 |
| `gtwContext`, `gtwDrafts` | F-45 | `mu` | /gtw fix state |
| `exitObserver` | callback | `mu` | AS 进程死了的时候调用 |
| `pumpMu`, `pump`, `pumpRunning` | pump 控制器 | `pumpMu` | 见 §4 |
| **`asCtx`, `asCancel`** | ctx | **`asCtxMu`** | per-AS |
| **`turnCtx`, `turnCancel`** | ctx | **`turnCtxMu`** | per-turn(derived from asCtx) |

### 锁的顺序约定

- **`mu` 是大锁**——改 `activeAS` / `pool` / `activeAgent` / `currentTurnUserMsgID` 等核心状态时持有。
- **`pumpMu` 独立**——只控制 readPump 启动/停止。
- **`asCtxMu` / `turnCtxMu` 独立**——只保护 ctx 切换。
- 跨锁顺序:`mu` → `asCtxMu`/`turnCtxMu`/`pumpMu` 各自独立,可以按任意顺序,但**不能把 `mu` 拿住再去拿别的锁**(会让 SendText 等被饿死)。

### 生命周期

```
New(chatID, primaryAgent)         in-memory, no spawn
WithPersistence(csFile, asFile)   装持久化
WithSpawner(spawner)              装 spawner(默认 nil,测试用)
...
RestoreFromRegistry()             从 chat_sessions.json + agent_sessions.json 还原
                                  → FromAgentSessionEntry 重建所有 AS(status=Detached)
...
GetOrCreate(chatID, primary)      manager 调用,缺失时 New,已有时返回
```

### /use 走的路径(切换 active AS)

```go
handleUse(ctx, mgr, ch, msg, args, primary):
  cs := mgr.GetOrCreate(msg.ChatID, primary)
  if cs.ActiveCwd() == "" → "send /cwd first"

  cs.ResetASContext()       // ← 关键:cancel asCtx + cancel turnCtx + 装新 ctx
                            //   旧 AS 上任何 in-flight bridge.SendBlocks 立刻 wake

  cs.SetActiveAgent(name)   // mu.Lock: activeAgent=..., currentTurnUserMsgID=""

  as, err := cs.LookupActiveAgentSession()  // mu.Lock: 查/建 (agent, cwd) AS,可能 Spawn
  if err != nil → reply with error

  _ = cs.StartReadPump()    // pumpMu: 停旧 pump + 起新 pump for 新 as

  source := "spawn" or "resumed"
  reply "Now using <name> (pid=..., source=...)"
```

**注意:`/use` 不 kill 旧 AS 的子进程**——只是断开 ctx 关联,旧 AS 仍留在 `pool`,旧 bridge 仍活着(可能在后台跑,可能 hung)。后续 `/kill` 才能真正关掉。这是 UX 上需要补的地方。

---

## 2. AgentSession(per-(agent, cwd))

### 字段

```go
type AgentSession struct {
    ID, ChatSessionID, Agent, Cwd   // 不可变
    pid    atomic.Int32              // OS PID, 0 = not running
    status sync.RWMutex              // value: Status (Running/Detached/Exited)
    stat   Status
    args   []string                  // spawn 参数(保留跨 respawn)

    exitCodeMu  sync.RWMutex
    exitCode    *int

    resumeIDMu  sync.RWMutex
    resumeID    string                // Claude Code 的 --resume id

    handleMu  sync.RWMutex            // 守卫 handle 字段
    handle    agent.AgentSession      // bridge-level live session(nil before Spawn)
    handleEventsClosed chan struct{}  // handle Events() close 的信号

    modelMu, cumulativeUsageMu  ...   // F-45 / F-49
}
```

### 状态机

```
        Spawn() 成功
Detached ─────────────► Running
   ▲                      │
   │ SetDetached           │ SetExited (from ObserveClose on handle.Events closed)
   │                      ▼
   └──────────────── Exited
```

- **`Status`** 用 `sync.RWMutex` 而不是 `atomic`(因为它是 string 别名,atomic.Int32 装不下)。
- **`pid`** 用 `atomic.Int32`,因为是纯数值。
- **`handle`** 用 `handleMu`,因为换 handle 的过程是"先 nil,再 newHandle,再 SetRunning",读端需要看到一致快照。

### LookupActiveAgentSession(选 AS 的核心入口)

```
cs.mu.Lock
if activeCwd == "" → ErrNoActiveCwd
if activeAgent == "" → ErrNoActiveAgent

key := (activeAgent, activeCwd)
if pool[key] exists AND Status==Running AND Handle() != nil:
    activeAS = pool[key]    // 命中,直接返回
    return

// 否则用旧 entry(Detached/Exited 状态保留 ID + ResumeID)或新建
newAS, hadPrior := pool[key]
if !hadPrior:
    newAS = NewAgentSession(newID(), cs.ID, agent, cwd, nil)
    pool[key] = newAS
activeAS = newAS
asFile.Upsert(newAS.Entry())

// Spawn outside of cs.mu to avoid holding write lock across fork+exec
if cs.spawner != nil:
    cs.mu.Unlock()
    spawnErr := newAS.Spawn(context.Background(), spawner)
    cs.mu.Lock()
    if err → 保留 entry,status 留 Detached,返回 error
    asFile.Upsert(newAS.Entry())  // 更新 PID

persistChatEntryLocked()
// 注意:不自动 StartReadPump — runtime 显式调
return newAS
```

**关键不变量:**`activeAS` 永远等于 `pool[(activeAgent, activeCwd)]`,status 为 Running。

### AgentSession.SendBlocks / SendText 的 ctx 处理

```go
func (as *AgentSession) SendText(text string) error:
    as.handleMu.RLock(); h := as.handle; as.handleMu.RUnlock()
    if h == nil → ErrNotRunning
    return h.SendText(text)     // ← pi.session.SendText 会套 promptTimeout

func (as *AgentSession) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error:
    as.handleMu.RLock(); h := as.handle; as.handleMu.RUnlock()
    if h == nil → ErrNotRunning
    return h.SendBlocks(ctx, blocks)   // ← 用 caller 给的 ctx(运行时: cs.TurnContext())
```

`AgentSession` 层**不创建 ctx**,只是 pass-through 到 bridge。ctx 的生命周期由 ChatSession 管理(asCtx / turnCtx)。

---

## 3. Conversation / Turn(per-message 往返)

"Conversation" 在本设计里不是一个 struct,而是 ChatSession + AgentSession + bridge 共同体现出来的一段**有起止的状态**:
- 起:InputBuffer 状态 IDLE + 收到新消息 → FlushHook → BeginTurn → SendBlocks
- 止:bridge 发 EventDone / EventError → runReadPump 收 → SetIdle + OnTurnEnded → EndTurn

### Turn 状态机

```
                    BeginTurn
                       ↓
   (no turn) ─────────────────► turn-active
                                       │
                                  SendBlocks(ctx)
                                  ctx = cs.TurnContext()
                                       │
                  ┌────────────────────┼────────────────────┐
                  │                    │                    │
            bridge 返回成功    bridge 返回 ctx.Canceled   EndTurn(EventDone/Error)
            (msg acked)        (ResetASContext / EndTurn)  (runReadPump)
                  │                    │                    │
                  └────────────────────┴────────────────────┘
                                       │
                                       ▼
                                  (no turn)
```

### 三层各自对 turn 的关注点

| 层 | 关注 | 操作 |
|----|------|------|
| ChatSession | 持有 ctx,BeginTurn/EndTurn 切换,InputBuffer FSM | `BeginTurn()`, `EndTurn()`, `ResetASContext()`, `ASContext()`, `TurnContext()` |
| AgentSession | 不感知 turn,但 turnActive 是 bridge 自己的 mutex | (无) |
| bridge session | `turnMu` + `turnActive`,序列化"同一进程"内的多次 prompt | SendBlocks 进出 turnMu,deferred release |

**注意:**bridge 自己的 `turnActive` 是**进程内串行**(同一进程同一时刻只能有一个 in-flight prompt)。它跟 ChatSession 的 turnCtx 是**两套独立**的串行机制:
- `turnActive` 防"同一个 bridge 被两个 caller 同时调 SendBlocks"
- `turnCtx` 让外部(cancel / 超时)能 wake 当前 turn 的 in-flight wait

---

## 4. readPump 控制器(pumpMu)

```go
type EventPumpState struct { stop, done chan struct{} }

type EventPump struct {
    mu sync.Mutex
    cur EventPumpState
}
```

每次 `StartReadPump`:
1. 读 `activeAS` (cs.mu.RLock)
2. 读 `eventHandler` (cs.mu.RLock)
3. **StopReadPump**:把旧 pump 的 stop 关掉,等 <-done
4. 起新 `runReadPump(as, h, stop, done)` goroutine

`runReadPump` 主循环:
```go
for {
    select {
    case <-stop:                  return
    case ev, ok := <-as.Events():
        if !ok → as.SetExited(0); return  // 进程死了
        h(cs.ChatID, as, ev, cs.currentTurnUserMsgID)
        switch ev.Kind:
        case EventDone:  emitMessageStateForCurrentTurn(MessageDone); EndTurn; SetIdle; OnTurnEnded
        case EventError: emitMessageStateForCurrentTurn(MessageFailed); EndTurn; SetIdle; OnTurnEnded
        default:         SetBusy
    }
}
```

`pumpRunning.Store(false)` 在 defer 里设,`close(done)` 同步给 StopReadPump。

**关键事实:每次只有一个 readPump 在跑。** `/use` 切 AS 时,StopReadPump 把旧的停掉再起新的(基于新的 activeAS)。事件流从 `as.Events()` 出来,自动绑死在 StartReadPump 时拍下的那个 AS 上。

---

## 5. ctx 传递链详解

### 创建

```go
cs, _ := New(chatID, primary)
cs.asCtx, cs.asCancel   = context.WithCancel(context.Background())
cs.turnCtx, cs.turnCancel = context.WithCancel(cs.asCtx)   // 预装,TurnContext 不会 nil
```

### 第一次发消息(Idle → 触发 turn)

```go
// newMessageDispatcher → cs.QueueUserMessage → InputBuffer.Add
// → state=IDLE → 同步调 flushHook:

flushHook(combined, userMsgIDs):
    cs.BeginTurn()           // cancel 旧 turnCtx + 装新 turnCtx derived from asCtx
    cs.mu.Lock
    cs.currentTurnUserMsgID = userMsgIDs[n-1]
    as := cs.activeAS
    cs.mu.Unlock
    return as.SendBlocks(cs.TurnContext(), combined)
                            // ↑ as.SendBlocks 把 ctx 透传给 bridge.SendBlocks
                            // bridge.SendBlocks 把 ctx 透传给 rpc.request
                            // rpc.request select { <-ch | <-ctx.Done() }
```

### /use 切换 AS

```go
cs.ResetASContext():
    cs.asCtxMu.Lock
    cs.asCancel()                    // cascade: 旧 asCtx 上的所有 turnCtx 收到 Done
    cs.asCtx, cs.asCancel = WithCancel(Background())
    cs.turnCtxMu.Lock
    cs.turnCancel()                  // 显式再来一遍(防御 + 确保旧 turnCtx 死透)
    cs.turnCtx, cs.turnCancel = WithCancel(cs.asCtx)
    // ↑ 新 turnCtx 从新 asCtx 派生
```

旧 turnCtx 上的 bridge.SendBlocks 立刻 wake,返回 ctx.Canceled,`turnActive` 通过 defer 释放。

新 AS 上第一条新消息走 BeginTurn → 新 turnCtx(从新 asCtx 派生)。

### EventDone / EventError 收尾

```go
case agent.EventDone:
    cs.emitMessageStateForCurrentTurn(agent.MessageDone)
    cs.EndTurn()                    // ← cancel 当前 turnCtx
    cs.SetIdle()
    _ = cs.OnTurnEnded()            // 可能触发 buffered message → 再次 BeginTurn
```

`OnTurnEnded` 调 flushHook 时,**新 turn 已经 BeginTurn** 了(flushHook 第一件事就是 BeginTurn),所以 chain 是顺的。

### 外部做超时

外部 caller(timeout wrapper / debug tooling / tests)可以:
- 拿 `cs.ASContext()`,挂 deadline / 派生 child
- 拿 `cs.TurnContext()`,同上
- 调 `cs.EndTurn()` 干掉当前 turn
- 调 `cs.ResetASContext()` 干掉当前 AS 上的所有 turn

桥接的 in-flight `rpc.request` 立刻返回 ctx.Canceled。

---

## 6. 关键路径的同步点总览

### "用户发 hi 到 pi"

| 步骤 | 谁做 | 持锁 | 阻塞点 |
|------|------|------|--------|
| 1. SDK → dispatchLoop → DispatchInbound | gateway | `g.mu` | 无 |
| 2. DispatchInbound → newMessageDispatcher | cmd/nightme | 无 | 无 |
| 3. cs.EmitMessageState(Received) | runtime | `cs.mu` | 无 |
| 4. cs.LookupActiveAgentSession | ChatSession | `cs.mu` | Spawn 可能(懒生成) |
| 5. cs.StartReadPump | ChatSession | `pumpMu` | 等旧 pump done |
| 6. cs.EmitMessageState(Forwarded) | runtime | `cs.mu` | 无 |
| 7. cs.QueueUserMessage → InputBuffer.Add | ChatSession | `b.mu` | 无(Idle) |
| 8. flushHook → cs.BeginTurn | ChatSession | `asCtxMu` → `turnCtxMu` | 无 |
| 9. flushHook → as.SendBlocks(cs.TurnContext()) | AS → bridge | `cs.mu` → `handleMu` → bridge turnMu | **bridge.SendBlocks:rpc.request 在 prompt ack / ctx.Done 上 select** |
| 10. bridge.readPump 收 stdout | bridge goroutine | 无 | `<-scanner.Scan()` |
| 11. bridge.readPump → s.deliver(ev) → events | bridge | `s.events` (buffered 64) | 满时 1s 超时丢 |
| 12. ChatSession.runReadPump → h() | ChatSession pump | `cs.mu` | EventHandler 内可能阻塞(ch.Send) |
| 13. EventHandler → ch.Send(OutboundMessage) | runtime | (无,由 channel 内部 mutex) | 飞 Feishu |
| 14. EventDone/Error → emitMessageState(Done) + EndTurn | ChatSession | `cs.mu` | 无 |

### "/use claude"

| 步骤 | 谁做 | 持锁 | 阻塞点 |
|------|------|------|--------|
| 1. /use → handleUse | gateway | 无 | 无 |
| 2. **cs.ResetASContext** | ChatSession | `asCtxMu` + `turnCtxMu` | **cascade cancel 旧 turnCtx → step 9 上一轮还在等的那条 wake** |
| 3. cs.SetActiveAgent | ChatSession | `cs.mu` | 无 |
| 4. cs.LookupActiveAgentSession | ChatSession | `cs.mu` | (Spawn 仅在 claude 第一次才走) |
| 5. cs.StartReadPump | ChatSession | `pumpMu` | 等旧 pump done |
| 6. reply "Now using claude" | runtime | 无 | ch.Send |

### "/kill"

| 步骤 | 谁做 | 持锁 | 阻塞点 |
|------|------|------|--------|
| 1. handleKill | gateway | 无 | 无 |
| 2. cs.KillAll | ChatSession | `cs.mu` | 每个 AS 的 `bridge.Close()` (bounded by closeDrainTimeout) |
| 3. bridge.Close → SIGINT → watchdog → SIGKILL | bridge | `closeOnce` | closeDrainTimeout (5s) |
| 4. bridge.Close → <-s.exitDone | bridge | `closeOnce` | 5s 超时后强返回 |
| 5. bridge.lifecycle: cmd.Wait + close(events) + close(exitDone) | bridge goroutine | `s.events` | 无(走完后) |

---

## 7. 死锁 / 风险面盘点

1. **bridge.SendBlocks(ctx) 没 deadline**:除了 pi.SendText 自带 promptTimeout,其他 bridge(claudecode/acp/pty)的 SendBlocks 是 fire-and-forget 写 stdiin,不会卡。
2. **bridge.Close 的 cmd.Wait 可能 hang**:有 `closeDrainTimeout = 5s` 兜底,会强返。
3. **readPump stuck in h(cs.ChatID, as, ev, ...)**:h 是 runtime 装的 EventHandler,里头可能 ch.Send 阻塞(Feishu 慢)。StopReadPump 要等 <-cur.done,而 done 只在 runReadPump 的 defer 里 close。如果 h 永远不返,**pump 永远不会停**——`/use` 的 StartReadPump 就会卡。
4. **ActiveAS swap 时 pump 还在跑旧 AS 的 events**:StopReadPump 关闭 stop 之后,runReadPump 下一轮 select 见到 stop 就 return,**不会**消费旧 AS 的 events。旧 AS 的 events 在 buffered channel 里 GC 掉。
5. **同一 turn 内的 cancel race**:BeginTurn / EndTurn / ResetASContext 都按 `asCtxMu → turnCtxMu` 顺序加锁;读路径 ASContext/TurnContext 也是单独加对应 mu。**但同一时刻 BeginTurn 和 EndTurn 并发不会出问题**——都是 turnCtxMu 串行化。
6. **cs.mu 是大锁**:LookupActiveAgentSession 在里头 Spawn 时会 Unlock 然后 Lock 回来,避免 fork+exec 持锁。但任何长持锁路径都会让 SendText 类操作饿——目前没看到这种路径。

---

## 8. 已知 UX 缺陷(不在当前 PR 范围)

1. **被 abandon 的 receipt 不会翻 `❌`**:pi hang 时 /use 切走,但 pi 的 receipt 永远停在 `Working...`——因为 runReadPump 在 stop 路径上没发 MessageState。建议:`handleUse` 在 SetActiveAgent 之前,如果旧 as 上有 in-flight turn,emit MessageState(Error) 给旧 userMsgID。
2. **旧 AS 进程没被 kill**:`/use` 只取消 ctx 关联,旧 AS 还留在 pool 里,旧 bridge 还活着。建议:`/use` 时如果检测到 AS swap,Close 旧 AS(SetRunning → SetExited,bridge.Close,信号结束进程)。
3. **InputBuffer FSM 没在 cancel 时复位**:`SetBusy` 是 readPump 在非 terminal event 上设的;如果 pump 被 stop 路径打断,SetIdle 不会调用,buffer FSM 留 Busy。建议:StopReadPump 路径上如果离开时 buffer 仍是 Busy,SetIdle。
4. **dispatchLoop 单 goroutine 同步**:一个慢 handler 会阻塞所有后续 inbound(包括另一个 chat 的)。与本 PR 范围无关,但用户提的"agents 互相影响"在这个层面也成立。

---

## 9. 一句话总结

ChatSession 用 `cs.mu` 守住"哪个 (agent, cwd) 是 active AS"这个事实,把 ctx 派生关系(asCtx → turnCtx → bridge)挂在 AS-swap 和 turn 边界上,每条 bridge 自己的 `turnMu`/`lifecycle`/`closeOnce` 保证子进程的串行化和单一所有者。readPump 在 ChatSession 上是单 goroutine,跟着 activeAS 走,事件从 bridge.events 出,经 EventHandler 转成 OutboundMessage 发到 IM channel。