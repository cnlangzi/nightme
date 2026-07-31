# F-25: Input Buffer & Message Status (Reaction + Reply, 3 State)

> **Status**: designed (v0.2)
> **Milestone**: v0.2
> **Depends on**: F-08 (channel abstraction), F-22 (feishu adapter), F-23 (heartbeat), F-24 (claudecode bridge)
> **Related**: [F-23-heartbeat.md](./F-23-heartbeat.md), [F-24-claudecode-bridge.md](./F-24-claudecode-bridge.md), [F-22-feishu-onclick-registration.md](./F-22-feishu-onclick-registration.md)

## 1. Description

Claude Code turn 执行中（BUSY）时，飞书侧用户可能继续发消息。这些消息需要：

1. **buffer 起来**（避免一条条发给 Claude，浪费 token + 算力；让用户的多条消息合成一个语义单位）
2. **清晰反馈状态**（用户能看到"我的消息在哪"）
3. **user 重发兜底**（重启 / flush 失败 → 用户自己重发；nightme 不持久化、不 retry）

每条用户消息 = **双轨状态显示**：
- **Reaction emoji**（3 种切换）：⏳ 等待 / 🔄 执行 / ✅ 完成
- **Reply message**（一行 note）：详细状态 + heartbeat 序号

**永远只有一行 note + 一个 reaction emoji**，不堆叠。

## 2. Message Lifecycle (3 States)

```
[State 1: Waiting]
  Reaction: ⏳
  Reply note: "⏳ 等待中"
      ↓ Add() 且 state=Idle → 立即 SendText → [State 2]
      ↓ Add() 且 state=BUSY → buffer.append → 保持 [State 1]
      ↓ buffer.flush() → SendText(combined) → [State 2]

[State 2: Executing]
  Reaction: 🔄
  Reply note: "🔄 ⏳ N · HH:MM:SS"  ← heartbeat 持续 update ⏳ 行
      ↓ Claude result event → [State 3]

[State 3: Completed]
  Reaction: ✅
  Reply note: "✅ 已完成 HH:MM:SS"
```

**状态转换**：

| From | To | Trigger |
|------|-----|---------|
| Waiting | Executing | `InputBuffer.Add()` 且 `state=Idle` (立即发) |
| Waiting | Executing | `InputBuffer.OnTurnEnded()` (buffer flush) |
| Executing | Completed | Claude `result` event |

**不存在其他状态**。

## 3. State Machine (Session-level)

```go
type SessionState int32

const (
    StateIdle SessionState = iota  // Claude 空闲，新消息直接发
    StateBusy                       // Claude 在执行，新消息 buffer
)
```

**state 转换来源**：Claude Code `stream-json` event：

| Event | state 转换 |
|-------|-----------|
| `system.init` | → IDLE |
| `assistant.message` (首次) | → BUSY |
| `assistant.message` (后续 chunks) | 保持 BUSY |
| `result` | → IDLE |

**没有第三条**：进程退出（signal 0 / EOF）走 F-23 §5 DEAD 检测，**不影响 buffer 状态机**。

## 4. Feishu Implementation (双轨)

### 4.1 Reaction Emoji 切换

```go
// internal/channel/feishu/receipt.go

type ReactionEmoji string

const (
    EmojiWaiting   ReactionEmoji = "⏳"
    EmojiExecuting ReactionEmoji = "🔄"
    EmojiCompleted ReactionEmoji = "✅"
)

// reaction 切换：删旧的加新的
func (r *MessageReceipt) switchReaction(target ReactionEmoji) {
    if r.currentEmoji == target { return }
    
    if r.currentEmoji != "" {
        r.bot.ReactionDelete(r.userMsgID, string(r.currentEmoji))
    }
    r.bot.ReactionAdd(r.userMsgID, string(target))
    r.currentEmoji = target
}
```

### 4.2 Reply Message（单行 note）

```go
func (r *MessageReceipt) renderNote() string {
    switch r.state {
    case StateWaiting:
        return "⏳ 等待中"
    case StateExecuting:
        return fmt.Sprintf("🔄 ⏳ %d · %s",
            r.eventCount, r.lastEventAt.Format("15:04:05"))
    case StateCompleted:
        return fmt.Sprintf("✅ 已完成 %s", r.completedAt.Format("15:04:05"))
    }
    return ""
}

func (r *MessageReceipt) updateNote() {
    r.bot.UpdateMessage(r.replyMsgID, r.renderNote())
}
```

### 4.3 双轨同步

```go
func (r *MessageReceipt) sync() {
    r.updateNote()
    r.switchReaction(emojiForState(r.state))
}

func emojiForState(s SessionState) ReactionEmoji {
    switch s {
    case StateWaiting:    return EmojiWaiting
    case StateExecuting:  return EmojiExecuting
    case StateCompleted:  return EmojiCompleted
    }
    return ""
}
```

**关键**：
- `sync()` 是**唯一**的状态更新入口
- `sync()` 同时更新 reaction + note
- 任何状态转换、heartbeat tick 都调用 `sync()`

## 5. Buffer (in-memory only)

```go
// internal/session/input_buffer.go

type InputBuffer struct {
    mu       sync.Mutex
    state    atomic.Int32        // SessionState (IDLE/BUSY)
    messages []string            // 纯字符串 slice，无 ID 无 status
    maxMsgs  int                 // 50
    maxBytes int                 // 100KB
    
    // 飞书 receipts：userMsgID → *MessageReceipt（buffer 中每条用户消息的视觉反馈）
    receipts map[string]*MessageReceipt  // key = userMsgID
    
    // callbacks
    onFlush func(combined string, userMsgIDs []string) error  // 发送给 Claude
    onError func(err error)
}

func NewInputBuffer(maxMsgs, maxBytes int) *InputBuffer {
    return &InputBuffer{
        state:    atomic.Int32{},
        maxMsgs:  maxMsgs,
        maxBytes: maxBytes,
        receipts: make(map[string]*MessageReceipt),
    }
}

// Add 接收用户消息（来自飞书）
func (b *InputBuffer) Add(content string, receipt *MessageReceipt) error {
    b.mu.Lock()
    defer b.mu.Unlock()
    
    state := SessionState(b.state.Load())
    
    if state == StateIdle {
        // 直接发送（不 buffer）
        b.mu.Unlock()
        err := b.onFlush(content, []string{receipt.UserMsgID})
        if err != nil {
            b.onError(err)  // 失败用户重发
        }
        return err
    }
    
    // BUSY → buffer
    if len(b.messages) >= b.maxMsgs {
        return ErrBufferFull  // 满了，丢弃
    }
    
    b.messages = append(b.messages, content)
    b.receipts[receipt.UserMsgID] = receipt  // 关联 receipt
    
    return nil
}

// OnTurnEnded Claude 完成时调用
func (b *InputBuffer) OnTurnEnded() error {
    b.mu.Lock()
    
    if len(b.messages) == 0 {
        b.mu.Unlock()
        return nil
    }
    
    combined := strings.Join(b.messages, "\n")
    userMsgIDs := make([]string, 0, len(b.receipts))
    for id := range b.receipts {
        userMsgIDs = append(userMsgIDs, id)
    }
    
    b.messages = nil
    b.receipts = make(map[string]*MessageReceipt)
    b.mu.Unlock()
    
    return b.onFlush(combined, userMsgIDs)
}

// Clear 用户 /clear 命令
func (b *InputBuffer) Clear() int {
    b.mu.Lock()
    defer b.mu.Unlock()
    
    count := len(b.messages)
    b.messages = nil
    // receipts 不清，留在各自状态机（仍显示"已发送"等）
    b.receipts = make(map[string]*MessageReceipt)
    
    return count
}

// Flush 用户 /flush 命令
func (b *InputBuffer) Flush() error {
    return b.OnTurnEnded()
}

func (b *InputBuffer) SetState(s SessionState) {
    b.state.Store(int32(s))
}

func (b *InputBuffer) State() SessionState {
    return SessionState(b.state.Load())
}

func (b *InputBuffer) Pending() int {
    b.mu.Lock()
    defer b.mu.Unlock()
    return len(b.messages)
}
```

**关键**：
- `[]string` 不是 `[]BufferedMessage`（不需要 ID、不需要 status）
- `receipts map` 关联 userMsgID → MessageReceipt（用于 buffer flush 后批量 update receipts）
- flush **之前**就清空 buffer（flush 失败用户重发）
- 没有持久化、没有 retry、没有 silent fail 后用户不知

## 6. MessageReceipt

```go
// internal/channel/feishu/receipt.go

type MessageReceipt struct {
    UserMsgID  string    // 飞书 user message ID（reaction target）
    ReplyMsgID string    // 飞书 reply message ID（update target）
    
    state        SessionState
    eventCount   int
    lastEventAt  time.Time
    receivedAt   time.Time
    forwardedAt  time.Time
    completedAt  time.Time
    
    currentEmoji ReactionEmoji
    bot          FeishuBot
}

// NewMessageReceipt 用户发消息时调用
func NewMessageReceipt(bot FeishuBot, userMsgID, content string) *MessageReceipt {
    r := &MessageReceipt{
        UserMsgID:  userMsgID,
        state:      StateWaiting,
        receivedAt: time.Now(),
        bot:        bot,
    }
    
    // 1. 创建 reply message（一行）
    r.ReplyMsgID = bot.SendReply(userMsgID, r.renderNote())
    
    // 2. 加 reaction
    r.bot.ReactionAdd(userMsgID, string(EmojiWaiting))
    r.currentEmoji = EmojiWaiting
    
    return r
}

func (r *MessageReceipt) State() SessionState { return r.state }
func (r *MessageReceipt) EventCount() int { return r.eventCount }
func (r *MessageReceipt) LastEventAt() time.Time { return r.lastEventAt }

// SetExecuting 状态转换：Waiting → Executing
func (r *MessageReceipt) SetExecuting() {
    if r.state == StateExecuting { return }
    
    r.state = StateExecuting
    r.forwardedAt = time.Now()
    r.eventCount = 1
    r.lastEventAt = time.Now()
    r.sync()
}

// Heartbeat heartbeat tick（每个 event）
func (r *MessageReceipt) Heartbeat() {
    if r.state != StateExecuting { return }
    
    r.eventCount++
    r.lastEventAt = time.Now()
    r.sync()
}

// SetCompleted 状态转换：Executing → Completed
func (r *MessageReceipt) SetCompleted() {
    if r.state == StateCompleted { return }
    
    r.state = StateCompleted
    r.completedAt = time.Now()
    r.sync()
}

func (r *MessageReceipt) sync() {
    r.updateNote()
    r.switchReaction(emojiForState(r.state))
}
```

## 7. Integration with F-23 Heartbeat

F-25 拥有 `MessageReceipt`，F-23 调用 `Heartbeat()`：

```go
// internal/bridge/claudecode/session.go

func (s *claudeSession) handleStreamEvent(ev StreamEvent) {
    switch ev.Type {
    case "assistant":
        // 首次 → state=Executing
        if !s.isBusy {
            s.isBusy = true
            s.inputBuffer.SetState(StateBusy)
            // 每个 buffered receipt 进入 Executing 状态
            for _, receipt := range s.inputBuffer.GetReceipts() {
                receipt.SetExecuting()
            }
        }
        
        // 每个 event → heartbeat
        for _, receipt := range s.activeReceipts() {
            receipt.Heartbeat()
        }
        
    case "result":
        s.isBusy = false
        s.inputBuffer.SetState(StateIdle)
        // 所有 active receipts 进入 Completed
        for _, receipt := range s.activeReceipts() {
            receipt.SetCompleted()
        }
    }
}
```

## 8. File Structure

```
internal/
├── session/
│   ├── input_buffer.go       # InputBuffer (纯内存)
│   ├── input_buffer_test.go
│   └── state.go              # SessionState type
├── channel/
│   └── feishu/
│       ├── receipt.go        # MessageReceipt (双轨 + 3 状态)
│       ├── receipt_test.go
│       └── reaction.go       # ReactionAdd / ReactionDelete 封装
```

## 9. Configuration

```yaml
# configs/nightme.example.yaml
input_buffer:
  enabled: true
  max_msgs: 50                # buffer 上限（条数）
  max_bytes: 102400           # buffer 上限（100KB）
  
  flush_triggers:
    on_turn_end: true         # Claude result event 自动 flush
    on_flush_command: true    # /flush 命令
    on_clear_command: true    # /clear 命令
  
  message_status:
    waiting_emoji: "⏳"
    executing_emoji: "🔄"
    completed_emoji: "✅"
    waiting_text: "⏳ 等待中"
    executing_text: "🔄 ⏳ {count} · {time}"
    completed_text: "✅ 已完成 {time}"
```

## 10. Edge Cases

| 场景 | 处理 |
|------|------|
| Buffer 超过 50 条 | 返回 `ErrBufferFull`，新消息丢弃（不阻塞）|
| Buffer 超过 100KB | 同上（按 bytes 算）|
| `onFlush` 失败 | 不 retry，log warn，message 已丢（用户重发）|
| Claude result 在 buffer 空时 | `OnTurnEnded()` no-op |
| 用户 /flush 但 buffer 空 | no-op |
| 用户 /clear | 清空 buffer，receipts 保留各自状态机 |
| /kill 时 buffer 还在 | buffer 内容写 slog（debug 用途），不发送 |
| 并发 Add（群聊多人）| `sync.Mutex` 保护 |
| 重启 | buffer 全丢 + 启动消息通知用户 |
| ReactionAdd 失败 | log warn，UI 不完整但功能正常 |
| UpdateMessage 失败 | log warn，下一次 sync 重试 |

## 11. Test Plan

### 11.1 Unit Tests

```go
// internal/session/input_buffer_test.go

func TestInputBuffer_IdleState_AddGoesThrough(t *testing.T)
func TestInputBuffer_BusyState_AddBuffers(t *testing.T)
func TestInputBuffer_OnTurnEnded_FlushesCombined(t *testing.T)
func TestInputBuffer_OnTurnEnded_EmptyBuffer_NoOp(t *testing.T)
func TestInputBuffer_BufferFull_ReturnsErr(t *testing.T)
func TestInputBuffer_Clear_ResetsBuffer(t *testing.T)
func TestInputBuffer_Flush_ManualFlush(t *testing.T)
func TestInputBuffer_ConcurrentAdd_NoRace(t *testing.T)

// internal/channel/feishu/receipt_test.go

func TestReceipt_NewMessage_CreatesReplyAndReaction(t *testing.T)
func TestReceipt_SetExecuting_UpdatesNoteAndReaction(t *testing.T)
func TestReceipt_Heartbeat_IncrementsCount(t *testing.T)
func TestReceipt_SetCompleted_UpdatesToFinalState(t *testing.T)
func TestReceipt_RenderNote_AllThreeStates(t *testing.T)
func TestReceipt_SwitchReaction_DeletesOldAndAddsNew(t *testing.T)
```

### 11.2 Integration Tests

```go
func TestIntegration_BufferFlush_AllReceiptsUpdate(t *testing.T) {
    // mock 飞书 bot
    // 模拟 3 条用户消息，state=BUSY
    // 验证 3 个 receipt 都从 Waiting → Executing → Completed
}

func TestIntegration_Heartbeat_UpdatesAllActiveReceipts(t *testing.T) {
    // 5 个 receipt，10 个 event
    // 验证每个 receipt 的 EventCount 都是 10
}
```

### 11.3 Manual E2E

| 场景 | 步骤 | 期望 |
|------|------|------|
| Idle 直发 | `/run claude` + 发消息 | Reaction: ⏳ → 🔄 → ✅；Note 切换对应内容 |
| Busy buffer | Claude 跑中，发多条消息 | Reaction 都是 ⏳ 等待中；flush 后同时变 🔄 |
| Heartbeat | Claude 处理中观察 | Note "🔄 ⏳ N · 时间" N 持续增长 |
| 重启丢失 | 重启 nightme | Buffer 全丢，启动消息通知 |
| /flush | Claude 跑中发 /flush | 立即 flush，receipts 变 Executing |

## 12. Anti-patterns

| ❌ 反例 | 为什么错 |
|---------|---------|
| Buffer 持久化到 disk | 重启即丢即可，用户重发兜底 |
| 追踪 msg.Status (buffered/sent) | 不需要，flush 同步，失败用户重发 |
| Retry flush 失败 | 用户会重发，不需要系统 retry |
| Silent fail（失败不通知）| 至少 log warn |
| 多于 3 个状态 | 用户视角混乱 |
| 多行 note | 永远只有一行 |
| Reaction 堆叠（累积 emoji）| 切换而非累积，3 个 emoji 即可 |
| Receipt 持有 content（消息内容）| privacy，receipt 只追踪状态不存内容 |

## 13. Open questions

- **群聊多人**：receipt map 的 key 是 userMsgID，群聊需要按 user 隔离 buffer 吗？
  - v0.2 决策：单聊够用，群聊 v0.3 评估
- **Reaction 顺序**：飞书 reaction 是按添加时间排序吗？跨平台是否一致？
  - 当前设计：reaction 切换（删旧加新），不影响顺序
- **飞书 UpdateMessage QPS 限制**：heartbeat 频繁触发 update 会不会触发限流？
  - 飞书卡片 update QPS 较高，stream-json event 频率有限（每秒 ≤ 几条），可接受
  - 如果限流，fallback 降级（不再每次 update，只更新关键节点）

## 14. Change log

- **2026-08-01** — 初版设计
  - 三个状态：等待中 / 执行中 / 已完成
  - 双轨显示：Reaction emoji + Reply note（一行）
  - Buffer 纯内存，无 ID 无 status
  - 失败用户重发兜底
  - 集成 F-23 heartbeat（⏳ N · 时间）
  - 集成 F-24 claudecode bridge（state 转换来源）