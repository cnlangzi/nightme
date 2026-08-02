# F-25: Input Buffer & Message Status

> **Status**: implemented (v1.1 — InputBuffer 不再持有 receipt；onFlush 钩子由 Gateway 注入)
> **Milestone**: v0.2 (input buffer FSM), v0.3 (hook relocation to Gateway)
> **Depends on**: F-08 (channel abstraction, receipt API), F-23 (heartbeat), F-24 (claudecode bridge)
> **Related**: [`SPEC.md`](../SPEC.md) v1.1 §1.2, §2.4; [`F-26-gateway-hub.md`](./F-26-gateway-hub.md) §2.4; [`F-08-channel-abstraction.md`](./F-08-channel-abstraction.md) §3

---

## 1. Description

Claude Code turn 执行中（BUSY）时，用户可能继续发消息。这些消息需要：

1. **buffer 起来**——避免一条条发给 agent 浪费 token / 算力；让用户多条消息合成一个语义单位
2. **清晰反馈状态**——用户能看到"我的消息在哪"
3. **user 重发兜底**——重启 / flush 失败 → 用户自己重发；nightme 不持久化、不 retry

每条用户消息 = **双轨状态显示**（在 Channel 侧实现）：
- **Reaction emoji**（4 种切换）：⏳ 等待 / 🔄 执行 / ✅ 完成 / ❌ 失败
- **Reply message**（一行 note）：详细状态 + heartbeat 序号

**永远只有一行 note + 一个 reaction emoji**，不堆叠。

**v1.1 职责再定义**：

| 旧（v0.2）| 新（v1.1）|
|-----------|-----------|
| InputBuffer 持有 receipts map (`map[userMsgID]*MessageReceipt`) | InputBuffer 不知道 receipt 存在 |
| InputBuffer 直接调 receipt.SetExecuting() | Session flush 时回调 Gateway → Gateway 调 `channel.UpdateReceipt(_, ReceiptExecuting)` |
| receipt 是 `*MessageReceipt`（feishu 具体类型）| receipt 是 `channel.Receipt` opaque（任何 channel 实现）|
| InputBuffer 收到 onFlush 时自行 flush + receipts 联动 | InputBuffer 触发 onFlush（Gateway 注入）→ Gateway 翻 receipt + Session.SendBlocks |

**InputBuffer 在 v1.1 只管一件事**：决定新用户消息是**立即 dispatch** 还是**入队**。所有跨层通知（receipt flip / 渲染 / 持久化）经过 Gateway。

---

## 2. Message Lifecycle (4 States, 4 ReceiptStates)

```
[State 1: ReceiptPending / BufferIdle 或 BufferBusy]
  ReceiptState: ⏳ (Pending)
  Buffer FSM:
    ├ Idle  → 立即 SendText → UpdateReceipt(Executing)
    └ Busy  → 入队 → 保持 Pending 直到 buffer flush
  ↓
[State 2: ReceiptExecuting]
  ReceiptState: 🔄 (Executing)
  Buffer FSM: Busy
  ↓ buffer.OnTurnEnded() → flush → UpdateReceipt(Executing) × N (for queued) → SendBlocks(combined)
  ↓ claude result event → UpdateReceipt(Done)
  ↓
[State 3: ReceiptDone]
  ReceiptState: ✅ (Done)
  Receipt disposed
  ↓
[State 4: ReceiptError] (任何阶段失败可达)
  ReceiptState: ❌ (Error)
  Receipt disposed
```

**状态转换**：

| From | To | Trigger | 由谁驱动 |
|------|-----|---------|---------|
| (none) | Pending | `ch.CreateReceipt(...)` | Gateway.fallback (a) |
| Pending | Executing | `sess.QueueUserMessage` 且 Buffer Idle | Gateway.fallback (d) |
| Pending | Executing | `InputBuffer.onFlush` (queued msgs 实际 dispatch 时) | Gateway.onInputBufferFlush |
| Executing | Done | Claude `result` event | Gateway.OnSessionEvent |
| Executing | Error | Agent error / dispatch error / panic | Gateway.OnSessionEvent + fallback err path |
| Done / Error | (none) | `ch.DisposeReceipt(rcpt)` + `delete(receipts, userMsgID)` | Gateway |

**不存在其他状态**。

---

## 3. State Machine (Session-level InputBuffer FSM)

```go
type SessionState int32

const (
    StateIdle SessionState = iota  // agent 空闲，新消息直接发
    StateBusy                       // agent 在执行，新消息 buffer
)
```

**InputBuffer state 转换来源**：agent 事件流

| Event | state 转换 | 驱动 |
|-------|-----------|------|
| `system.init` | → IDLE | session.readPump |
| `assistant.message` (首次) | → BUSY | session.readPump |
| `assistant.message` (后续 chunks) | 保持 BUSY | session.readPump |
| `result` | → IDLE + OnTurnEnded() + flush buffer | session.readPump |

**没有第三条**：进程退出（signal 0 / EOF）走 F-23 §5 DEAD 检测，**不影响 InputBuffer state 机**。

---

## 4. Channel 侧的 Receipt 实现（v1.1：opaque + Channel 拥有 object）

### 4.1 ReceiptState 枚举

```go
// internal/channel/channel.go — Gateway 和 Channel 共用

type ReceiptState int
const (
    ReceiptPending   ReceiptState = iota  // ⏳
    ReceiptExecuting                      // 🔄
    ReceiptDone                           // ✅
    ReceiptError                          // ❌
)
```

### 4.2 Feishu 实现（`internal/channel/feishu/receipt.go`）

```go
type MessageReceipt struct {
    UserMsgID  string    // 飞书 user message ID（reaction target）
    ReplyMsgID string    // 飞书 receipt message ID
    ReactionID string    // current reaction handle

    currentEmoji ReactionEmoji  // ⏳ / 🔄 / ✅ / ❌
    mu           sync.Mutex     // protect concurrent UpdateReceipt calls
}

type ReactionEmoji string
const (
    EmojiWaiting   ReactionEmoji = "⏳"
    EmojiExecuting ReactionEmoji = "🔄"
    EmojiCompleted ReactionEmoji = "✅"
    EmojiError     ReactionEmoji = "❌"
)

// CreateReceipt → ReplyMessage + AddReaction(⏳)
func (a *Adapter) CreateReceipt(ctx, chatID, userMsgID string, blocks []agent.ContentBlock) (channel.Receipt, error) {
    text := buildReceiptText(blocks)
    replyMsgID, err := a.bot.ReplyMessage(ctx, chatID, userMsgID, text)
    if err != nil { return nil, err }
    reactionID, _ := a.bot.AddReaction(ctx, chatID, userMsgID, EmojiWaiting)
    return &MessageReceipt{
        UserMsgID: userMsgID, ReplyMsgID: replyMsgID,
        ReactionID: reactionID, currentEmoji: EmojiWaiting,
    }, nil
}

// UpdateReceipt swaps reaction according to state.
func (a *Adapter) UpdateReceipt(ctx, receipt channel.Receipt, state channel.ReceiptState) error {
    r := receipt.(*MessageReceipt)
    r.mu.Lock()
    defer r.mu.Unlock()
    target := emojiForState(state)
    if r.currentEmoji == target { return nil }  // idempotent
    // delete old + add new
    if r.ReactionID != "" {
        _ = a.bot.DeleteReaction(ctx, chatIDOf(r), r.UserMsgID, r.ReactionID)
    }
    newID, err := a.bot.AddReaction(ctx, chatIDOf(r), r.UserMsgID, target)
    if err == nil { r.ReactionID = newID; r.currentEmoji = target }
    return err
}

// DisposeReceipt → DeleteMessage(replyMsgID)
func (a *Adapter) DisposeReceipt(ctx, receipt channel.Receipt) error {
    r := receipt.(*MessageReceipt)
    if r.ReplyMsgID == "" { return nil }
    return a.bot.DeleteMessage(ctx, r.ReplyMsgID)
}
```

### 4.3 Echo 实现（`internal/channel/echo/echo.go`）

```go
type echoReceipt struct{ userMsgID string }

func (c *Channel) CreateReceipt(ctx, chatID, userMsgID string, blocks []agent.ContentBlock) (channel.Receipt, error) {
    fmt.Fprintf(c.out, "[receipt %s] created (state=pending, chat=%s)\n", userMsgID, chatID)
    return &echoReceipt{userMsgID: userMsgID}, nil
}
func (c *Channel) UpdateReceipt(ctx, receipt channel.Receipt, state channel.ReceiptState) error {
    fmt.Fprintf(c.out, "[receipt %s] state=%s\n", receipt.(*echoReceipt).userMsgID, stateName(state))
    return nil
}
func (c *Channel) DisposeReceipt(ctx, receipt channel.Receipt) error {
    fmt.Fprintf(c.out, "[receipt %s] disposed\n", receipt.(*echoReceipt).userMsgID)
    return nil
}
```

---

## 5. Buffer (in-memory only) — v1.1 瘦身

```go
// internal/session/input_buffer.go

type InputBuffer struct {
    mu       sync.Mutex
    state    atomic.Int32        // SessionState (IDLE/BUSY)
    messages []bufferedMsg        // (ContentBlock 切片) + (userMsgID)
    maxMsgs  int                  // 50
    maxBytes int                  // 100KB

    // v1.1: NO receipts map. NO OnUserMessage callback.
    // ONLY one hook: onFlush, set by Gateway at session creation.
    onFlush func(blocks []agent.ContentBlock, userMsgIDs []string) error
}

type bufferedMsg struct {
    Blocks     []agent.ContentBlock
    UserMsgID  string
}

// OnFlush fires when Buffer transitions Busy → Idle (i.e. agent's
// result event arrives and we want to dispatch queued messages).
// Gateway installs this hook to (1) flip queued receipts to Executing
// (2) call session.SendBlocks(combined).
func (b *InputBuffer) OnFlush() error {
    b.mu.Lock()
    if len(b.messages) == 0 {
        b.mu.Unlock()
        return nil
    }
    blocks := combineBlocks(b.messages)
    userMsgIDs := collectUserMsgIDs(b.messages)
    b.messages = nil
    b.mu.Unlock()
    return b.onFlush(blocks, userMsgIDs)
}

// Add decides dispatch vs buffer based on state.
func (b *InputBuffer) Add(blocks []agent.ContentBlock, userMsgID string) error {
    b.mu.Lock()
    state := SessionState(b.state.Load())
    if state == StateIdle {
        // Idle: dispatch immediately via onFlush (single msg path).
        b.mu.Unlock()
        return b.onFlush(blocks, []string{userMsgID})
    }
    // Busy: enqueue
    if len(b.messages) >= b.maxMsgs { return ErrBufferFull }
    if totalBytes(b.messages)+blockBytes(blocks) > b.maxBytes { return ErrBufferFull }
    b.messages = append(b.messages, bufferedMsg{Blocks: blocks, UserMsgID: userMsgID})
    b.mu.Unlock()
    return nil
}

func (b *InputBuffer) SetState(s SessionState) { b.state.Store(int32(s)) }
func (b *InputBuffer) State() SessionState       { return SessionState(b.state.Load()) }
func (b *InputBuffer) Pending() int              { /* read len(messages) under mu */ }
```

**关键变化（v0.2 → v1.1）**：
- ❌ 删除 `receipts map[string]*MessageReceipt`——receipt 不属于 buffer
- ❌ 删除 `OnUserMessage(content, userMsgID) error`——call site 是 Gateway，不是 session
- ✅ 唯一保留的 hook：`onFlush(blocks, userMsgIDs) error`——由 Gateway 装入
- `messages` 改为 `[]bufferedMsg`（带 userMsgID）而不是 `[]string`——因为 flush 时需要 userMsgIDs 给 receipt 反查

### 5.1 Gateway 装 onFlush 的代码

```go
// 在 cmd/nightme/run.go 或 gateway 启动时
mgr := session.NewMemoryManager(agents, reg, /* EventCallback = */ gw.OnSessionEvent)

// 或者在 gw.SpawnAgent 内部（v1.1 推荐：把 hook 装入作为 CreateRequest 的字段）
sess, err := mgr.Create(ctx, session.CreateRequest{
    Workspace: ws,
    Agent:     name,
    Args:      extraArgs,
    OnFlushHook: gw.onInputBufferFlush,  // ← Gateway 注入
})
```

```go
// gw.onInputBufferFlush 实现
func (g *Gateway) onInputBufferFlush(blocks []agent.ContentBlock, userMsgIDs []string) error {
    // (1) Flip queued receipts to Executing
    for _, umid := range userMsgIDs {
        g.mu.Lock()
        entry, ok := g.receipts[umid]
        if ok && entry.state == channel.ReceiptPending {
            entry.state = channel.ReceiptExecuting
        }
        g.mu.Unlock()
        if ok {
            _ = g.channel.UpdateReceipt(context.Background(), entry.receipt, channel.ReceiptExecuting)
        }
    }
    // (2) Dispatch via session.SendBlocks
    // session 由 Manager 持有；hook 是从 session.readPump 里调起的，
    // 所以通过闭包捕获 sess 引用
    return g.activeSession.SendBlocks(context.Background(), blocks)
}
```

---

## 6. Integration with F-23 Heartbeat

F-23 heartbeat 改由 **Channel 实现**而不是 receipt 维护：

- Feishu adapter 在 `MessageReceipt` 上挂一个 heartbeat ticker（独立 goroutine），按 F-23 节奏调用 `bot.UpdateMessage(replyMsgID, "🔄 ⏳ N · HH:MM:SS")`
- 状态机：仅当 `currentEmoji == EmojiExecuting` 时跑 ticker；切到 Done/Error 时停
- InputBuffer **不再持有** heartbeat 状态

详见 [`F-23-heartbeat.md`](./F-23-heartbeat.md) §7（v1.1 修订）。

---

## 7. File Structure (v1.1)

```
internal/
├── session/
│   ├── input_buffer.go       # InputBuffer (纯内存 + onFlush hook)
│   ├── input_buffer_test.go
│   └── state.go              # SessionState type
├── gateway/
│   ├── gateway.go            # Gateway (含 receipts map, bindings map)
│   ├── receipts.go           # receiptEntry + Flip/Dispose
│   └── receipts_test.go
├── channel/
│   ├── channel.go            # Receipt + ReceiptState + interface
│   └── feishu/
│       ├── receipt.go        # MessageReceipt (Channel 拥有 object)
│       ├── receipt_test.go
│       └── reaction.go       # ReactionAdd / DeleteReaction 封装
└── bridge/
    └── claudecode/
        └── session.go        # 不知道 receipt 存在
```

---

## 8. Configuration

```yaml
# configs/nightme.example.yaml
input_buffer:
  enabled: true
  max_msgs: 50
  max_bytes: 102400           # 100KB

  flush_triggers:
    on_turn_end: true         # agent result event 自动 flush
    on_flush_command: true    # /flush 命令（v0.4 计划）
    on_clear_command: true    # /clear 命令（v0.4 计划）

receipt:
  waiting_emoji:   "⏳"
  executing_emoji: "🔄"
  completed_emoji: "✅"
  error_emoji:     "❌"
```

（v0.2 配置里的 `waiting_text / executing_text / completed_text` 字段移除——receipt text 现在由 channel 在 CreateReceipt 内部按 blocks 生成，不再有 gateway 端的文本模板。）

---

## 9. Edge Cases

| 场景 | 处理 |
|------|------|
| Buffer 超过 50 条 | 返回 `ErrBufferFull`，新消息丢弃（不阻塞）|
| Buffer 超过 100KB | 同上（按 bytes 算）|
| `onFlush` 失败 | Gateway log warn，receipts 已翻 Executing；用户重发 |
| Claude result 在 buffer 空时 | `OnFlush()` no-op |
| 用户 /flush 但 buffer 空 | no-op（v0.4 命令）|
| 用户 /clear | 清空 buffer，receipts 不动（v0.4 命令）|
| /kill 时 buffer 还在 | buffer 内容写 slog（debug 用途），不发送 |
| 并发 Add（群聊多人）| `sync.Mutex` 保护 |
| 重启 | buffer 全丢 + receipt map 全丢，pending receipt 静默消失 |
| Channel.CreateReceipt 失败 | Gateway 走 fallback：ch.Send(OutText msg.Text) + 不创建 receipt entry |
| Channel.UpdateReceipt 失败 | Channel 内部 log warn；receipt state 仍推进（Gateway 不重试）|
| Channel.DisposeReceipt 失败 | Channel 内部 log warn；Gateway 已从 receipts map 删除条目 |
| ReactionAdd 失败（API 临时错）| Feishu adapter 内部 log，UpdateReceipt 返回 err；Gateway 不重试 |
| UpdateMessage 失败（heartbeat）| Feishu adapter 内部 log warn；下个 tick 再试 |

---

## 10. Test Plan

### 10.1 Unit

```go
// internal/session/input_buffer_test.go

func TestInputBuffer_IdleState_AddGoesThrough(t *testing.T)
func TestInputBuffer_BusyState_AddBuffers(t *testing.T)
func TestInputBuffer_OnTurnEnded_FlushesCombined(t *testing.T)
func TestInputBuffer_OnTurnEnded_EmptyBuffer_NoOp(t *testing.T)
func TestInputBuffer_BufferFull_ReturnsErr(t *testing.T)
func TestInputBuffer_ConcurrentAdd_NoRace(t *testing.T)
// v1.1 新增：
func TestInputBuffer_NoReceiptsField_NotInStruct(t *testing.T)  // 静态检查
func TestInputBuffer_OnFlush_CallsHookWithBlocksAndIDs(t *testing.T)
func TestInputBuffer_OnFlush_HookErr_Propagates(t *testing.T)
```

```go
// internal/gateway/receipts_test.go（v1.1 新增）

func TestReceipts_Create_StoresEntry(t *testing.T)
func TestReceipts_Flip_PendingToExecuting_CallsChannelUpdate(t *testing.T)
func TestReceipts_Flip_ExecutingToDone_CallsChannelUpdatePlusDispose(t *testing.T)
func TestReceipts_Dispose_RemovesEntry(t *testing.T)
func TestReceipts_EventError_FlipsAllSessionReceiptsToError(t *testing.T)
```

```go
// internal/channel/feishu/receipt_test.go

func TestFeishu_CreateReceipt_PostsReplyAndReaction(t *testing.T)
func TestFeishu_UpdateReceipt_SwapsReactionPerState(t *testing.T)
func TestFeishu_DisposeReceipt_DeletesMessage(t *testing.T)
func TestFeishu_UpdateReceipt_Idempotent(t *testing.T)
func TestFeishu_ReactionFailure_ContinuesGracefully(t *testing.T)
```

### 10.2 Integration

```go
func TestIntegration_BufferFlush_FlipsQueuedReceipts(t *testing.T) {
    // Gateway + Manager + mock Channel
    // 模拟 3 条 userMsg, Buffer Busy → all 3 receipts Pending
    // 触发 EventResult → onFlush → 3 receipts → Executing
    // 再触发 EventDone → 3 receipts → Done → Dispose
}

func TestIntegration_IdleDispatch_FlipsImmediately(t *testing.T) {
    // 1 条 userMsg, Buffer Idle → receipt Pending → Executing (立即)
    // EventDone → Done → Dispose
}

func TestIntegration_EventError_FlipsAllReceiptsToError(t *testing.T) {
    // 1 条 userMsg, Buffer Idle → Executing
    // EventError → Error → Dispose
}

func TestIntegration_ReceiptCreateFailure_FallsBackToPlainSend(t *testing.T) {
    // mock Channel.CreateReceipt 返回 err
    // 验证 ch.Send(OutText msg.Text) 被调，receipts map 不增 entry
}
```

### 10.3 E2E

| 场景 | 步骤 | 期望 |
|------|------|------|
| Idle 直发 | `/run claude` + 发消息 | Reaction ⏳→🔄→✅；receipt message 内容最终态 |
| Busy buffer | Claude 跑中发 3 条 | 3 个 receipt 都是 ⏳ 等待中；EventResult 后一起变 🔄 → ✅ |
| Heartbeat | Claude 处理中观察 | receipt message 内容 "🔄 ⏳ N · 时间" N 持续增长（Channel 内部 ticker）|
| 重启丢失 | 重启 nightme | buffer 全丢 + receipt map 全丢，pending receipt 静默消失 |
| Channel error | mock Channel.UpdateReceipt 失败 | log warn，receipt state 仍推进，UI 略不完整但功能正常 |

---

## 11. Anti-patterns

| ❌ 反例 | 为什么错 |
|---------|---------|
| Buffer 持有 receipt 引用 | receipt 是 Channel object；session 不知道 Channel 存在 |
| session import `channel/feishu` | v1.1 强制隔离：session 是纯域对象 |
| Gateway import `channel/feishu` | v1.1 强制隔离：Gateway 只知道 Channel interface |
| Retry receipt update 失败 | Channel 内部 log + 下次自然刷新即可 |
| Silent fail（失败不通知）| log warn |
| 多于 4 个 receipt state | 用户视角混乱 |
| 多行 note | 永远只有一行 |
| Reaction 堆叠（累积 emoji）| 切换而非累积，4 个 emoji 即可 |
| Receipt 持有 content（消息内容）| privacy，receipt 只追踪状态不存内容 |
| 在 buffer.OnFlush 里调 receipt.SetExecuting() | receipt 不是 buffer 知道的类型；通过 onFlush hook 让 Gateway 处理 |

---

## 12. Open questions

- **群聊多人**：receipt map key 是 userMsgID（IM 原生唯一），群聊每条 message 一个 receipt；不需要 per-user 隔离
- **Reaction 顺序**：飞书 reaction 是按添加时间排序，v1.1 切换（删旧加新）保持 UI 一致
- **飞书 UpdateMessage QPS 限制**：heartbeat 由 Channel 内部 ticker 驱动，频率可控（每秒 ≤ 几条），可接受
- **Buffer 大小自适应**：max_bytes 是固定 100KB；超大单条消息（> 100KB）直接拒绝入队——用户拆分

---

## 13. Cross-references

- **Receipt FSM 状态机**：见 [`F-26-gateway-hub.md`](./F-26-gateway-hub.md) §2.4
- **Channel interface 形状**：见 [`F-08-channel-abstraction.md`](./F-08-channel-abstraction.md) §2, §3
- **Gateway fallback 流**：见 [`F-20-gateway.md`](./F-20-gateway.md) §5
- **Manager.EventCallback 单一消费者**：见 [`F-26-gateway-hub.md`](./F-26-gateway-hub.md) §2.3

---

## 14. Change log

- **2026-08-02** — v1.1: InputBuffer 不再持有 receipt（删除 `receipts map` 字段）。唯一保留 `onFlush(blocks, userMsgIDs)` hook，由 Gateway 注入。Session 不知道 receipt 存在。Doc 重写。
- **2026-08-01** — v0.2: InputBuffer 持有 receipt map + 双轨 Reaction+Reply + 3 状态。已被 v1.1 取代（state 数从 3 扩到 4，添加 Error 状态；Buffer 责任从"管 receipt"缩到"管 dispatch/queue"）。