# F-53: Message / Prompt 对象化 — 实施计划（Phase 0，单 PR）

> **Status**: 待用户确认后开工
> **Design**: [`docs/feat/message_lifecycle.md`](../docs/feat/message_lifecycle.md)（先读这个）
> **Dev task brief**: [`tasks/wip-message-prompt.md`](./wip-message-prompt.md)（实现级需求）
> **Scope**: `internal/chatsession` + `internal/agent` + `internal/channel/feishu` + `cmd/nightme/run.go` + 全部受影响的测试
> **Out of scope（明示不做）**：
>   - 进程异常退出 `endPrompt(ProcessDied)` 接线 → 下次"Prompt 投递稳定性优化" PR
>   - `Prompt` 持久化
>   - 显式 retry 层（靠 Queued + 下次 flushPending 自然重投，见 §3）
>   - 用户消息 ✅/👎 反应的替代 UX（commit message 明示回归点）

---

## 1. 命名约定（贯穿所有任务）

```go
// agent 包（wire + 内部统一）
type MessageState int  // 已是现有类型；本任务改常量

const (
    MessageQueued    MessageState = iota  // 原 MessageReceived
    MessageSubmitted                       // 原 MessageForwarded
    MessageDropped                         // 新增（主动清空）
    // 删除：MessageDone, MessageFailed
)

// chatsession 包（仅内部）
type Prompt struct {
    ID             string
    ChatSessionID  string
    AgentSessionID string
    MessageIDs     []string  // 有序
    Blocks         []agent.ContentBlock
    LastMessageID  string    // 本批次最后一条 message 的 ID（占位卡挂靠点）
    CreatedAt      time.Time
    AckedAt        time.Time
    LastProgressAt time.Time
    EndedAt        time.Time
    EndReason      PromptEndReason
}

type PromptEndReason int
const (
    PromptEndClean       PromptEndReason = iota  // EventDone
    PromptEndError                               // EventError
    PromptEndProcessDied                         // 本次不触发
    PromptEndStalledKilled                       // 本次不触发
    PromptEndUserKilled                          // 本次不触发
)

type PromptHook func(p *Prompt) error

// feishu 包（从 agent 包私有化）
type PromptState int  // 由原 agent.PromptState 改名为 feishu.PromptState
const (
    PromptRunning PromptState = iota
    PromptDone  // 实际仍死代码（与 F-44 同），但保留 2 值便于将来恢复
)
```

---

## 2. Wire 时机

| 状态 | 触发点 | 调用位置（实现后） |
|---|---|---|
| `MessageQueued` | dispatcher 拿到 inbound 后、`LookupActiveAgentSession` 之前 | `cmd/nightme/run.go` newMessageDispatcher（接当前 565 行 MessageReceived 位置） |
| `MessageSubmitted` | PromptHook 内 SendBlocks 返回 nil 之后，批量翻 Stage 之前 | `internal/chatsession/chatsession.go` defaultPromptHookLocked |
| `MessageDropped` | `ChatSession.MarkDropped(userMsgID)` 调用时 | `ChatSession.BufferClear`、`/kill`、`/new` 调用方 |

**明确不触发**：
- spawn 失败（`LookupActiveAgentSession` 返回 error）→ 只发文本 OutReply，**不**创建 Message、**不** emit Queued
- SendBlocks 失败 → Message 留 Queued，**不** emit 任何状态变更
- `!ok`（进程异常退出）→ 本次不动（user #7）

---

## 3. Retry 语义（user #B 确认）

```
SendBlocks → err != nil
  └─ Prompt 不创建
  └─ Message.Stage 留 Queued
  └─ 下次 flushPending()（下次 Add / 下次 OnTurnEnded / 手动 /flush）再投一次
```

不做：timer 重试、补偿逻辑、去重。重复投递接受。

---

## 4. 数据归属

| 数据 | 归属 | 锁 |
|---|---|---|
| `*Message` | `ChatSession.messagesByID`（sync.Map） | InputBuffer.mu 保护队列；ChatSession.mu 保护 messagesByID |
| `*Prompt` | `AgentSession.currentPrompt` | AgentSession 自身锁 |
| `promptCounter atomic.Uint64` | `AgentSession` | atomic |
| `ChatSession.activeAS` | 不变 | cs.mu |

**关键锁顺序**（在 defaultPromptHookLocked 路径上）：
1. `InputBuffer.mu` 排空队列（释放）
2. `cs.mu` 读 activeAS（释放）
3. `as.SendBlocks(...)`（不持锁）
4. `cs.mu` 写 `messages[*].Stage = Submitted`、写 `as.currentPrompt = p`（持锁；as 的 currentPrompt 写也在同一持锁窗口内，需要 as 暴露 setter 或直接字段访问 + as.mu）

锁顺序最简方案：`as` 的 `currentPrompt` 字段读写都走 `cs.mu`（即 currentPrompt 物理上住在 AgentSession，但访问必经 cs.mu）。这是新引入的耦合，但避免了双锁顺序问题。如果后续 AS 自己也要访问 currentPrompt，再加 as.mu 并排序。

---

## 5. 任务分解

每条任务都有 acceptance criteria。依赖用 `→` 表示。编号是建议执行顺序。

### T01. agent 包：MessageState 常量改名 + 物理删除 Done/Failed

**改**：`internal/agent/message_state.go`
- `MessageReceived` → `MessageQueued`
- `MessageForwarded` → `MessageSubmitted`
- 删 `MessageDone`、`MessageFailed`
- `String()` 同步

**依赖**：无
**AC**：包内无 MessageReceived/MessageForwarded/MessageDone/MessageFailed 引用（编译通过即可证明）
**注意**：这是最大破坏性变更；T14/T15/T17 之前不能合入

### T02. chatsession 包：Message / MessageStage 类型定义

**新增**：`internal/chatsession/message.go`
```go
type Message struct {
    ID         string  // channel-native id（UserID:Time fallback 在 dispatcher 做）
    ChatID     string
    Blocks     []agent.ContentBlock
    ReceivedAt time.Time
    PromptID   string  // 进入 Submitted 时回填；之后不可变
    Stage      agent.MessageState  // 直接用 agent.MessageState，不另起别名
}
```

**依赖**：T01
**AC**：类型定义编译通过；Stage 字段类型为 `agent.MessageState`

### T03. chatsession 包：Prompt / PromptEndReason / PromptHook 类型定义

**新增**：`internal/chatsession/prompt.go`
- 字段见 §1
- `PromptEndReason` 5 个常量（CLean / Error / ProcessDied / StalledKilled / UserKilled）
- `PromptHook func(p *Prompt) error`

**依赖**：T02
**AC**：类型定义编译通过

### T04. AgentSession：currentPrompt + promptCounter 字段

**改**：`internal/chatsession/agentsession.go`
- 加 `currentPrompt *Prompt` 字段
- 加 `promptCounter atomic.Uint64` 字段
- 加方法 `NewPromptID() string`（递增 + 拼 `as.ID + "-p" + n`）

**注意**：`Prompt` 类型在 T03 才存在，本任务可拆为两步（T04a 加字段占位、T04b 接类型），或与 T03 合并 commit

**依赖**：T03
**AC**：`AgentSession.NewPromptID()` 返回 `as_3-p7` 形式（id 来源见 §6）；counter 单调递增

### T05. ChatSession：messagesByID 索引 + MarkDropped

**改**：`internal/chatsession/chatsession.go`
- 加 `messagesByID sync.Map` 字段
- 加方法 `MarkDropped(userMsgID string)`：从 map 取 Message，置 Stage=Dropped，emit MessageDropped
- 加方法 `GetMessage(userMsgID string) *Message`（readpump 用）

**依赖**：T02
**AC**：MarkDropped 在消息存在时 wire emit MessageDropped；消息不存在时静默

### T06. ChatSession：删除 currentTurnUserMsgID 相关

**改**：`internal/chatsession/chatsession.go`、`internal/chatsession/readpump.go`
- 删 `cs.currentTurnUserMsgID` 字段
- 删 `emitMessageStateForCurrentTurn` 方法
- readpump 改读 `as.currentPrompt.LastMessageID`（此时 `as.currentPrompt` 可能为 nil，需处理）

**注意**：T13 才真正接入；本任务先拆字段+方法，让编译报错暴露所有 caller

**依赖**：T04, T03
**AC**：包内无 `currentTurnUserMsgID` 引用（编译通过）

### T07. InputBuffer：bufferEntry → *Message

**改**：`internal/chatsession/input_buffer.go`
- `bufferEntry` 字段改为 `*Message`（保留 Blocks + UserMsgID 但都从 Message 取）
- `FlushHook` → `PromptHook` 签名（同时也是 T08）
- `Add(blocks, userMsgID)` → `Add(msg *Message)`：构造后入 map（ChatSession.messagesByID）、入 buffer 队列
- `OnTurnEnded()` 内部临时仍按旧 FlushHook 调，但 message 列表用 `[]*Message`

**依赖**：T02, T03
**AC**：Add/OnTurnEnded 改用 *Message；FlushHook→PromptHook 同步改名

### T08. InputBuffer：OnTurnEnded 拆为 flushPending + Clear 语义

**改**：`internal/chatsession/input_buffer.go`
- `OnTurnEnded()` 改名 `flushPending()`：排空队列、调 PromptHook、Clear 失败补偿（暂不做，user #B）
- `Flush()` 调 `flushPending()`（用户手动 /flush）
- `Clear()` 改成返回被清空的 `[]*Message` 切片，由 ChatSession 负责 emit Dropped

**依赖**：T07
**AC**：flushPending 是唯一排空+提交路径；Clear 返回 messages 让 caller 处理 Dropped

### T09. ChatSession：defaultPromptHookLocked 实现

**改**：`internal/chatsession/chatsession.go`
- `defaultFlushHookLocked` → `defaultPromptHookLocked`
- 内部：构造候选 `*Prompt`（MessageIDs、Blocks、ChatSessionID、AgentSessionID=activeAS.ID、LastMessageID=最后一条 msg.ID）→ `as.SendBlocks(...)` → 成功时：`cs.mu` 下写 `messages[*].Stage=Submitted`、写 `messages[*].PromptID=p.ID`、写 `as.currentPrompt=p`、设 `p.AckedAt/p.LastProgressAt=now`、批量 emit MessageSubmitted、调用 NewPromptID 后置；失败时：丢弃 Prompt、返回 error

**依赖**：T04, T05, T07, T08
**AC**：
- SendBlocks nil → 全部 message Stage=Submitted，wire emit 每个一次
- SendBlocks err → Prompt 不创建，message Stage 不变，wire 不 emit

### T10. ChatSession：endPrompt + BufferClear 改造

**改**：`internal/chatsession/chatsession.go`
- 加 `endPrompt(reason PromptEndReason)`：取 `as.currentPrompt`，置 EndedAt+EndReason，清 `as.currentPrompt=nil`，**不**遍历 MessageIDs（user #1 + §3）
- `BufferClear()` 改：调 `inputBuffer.Clear()` 拿到 `[]*Message`，遍历调 `cs.MarkDropped(m.ID)`，返回数量

**依赖**：T09
**AC**：endPrompt 单 sink；BufferClear 触发 Dropped emit；endPrompt 不动 messages（Stage 早定终态）

### T11. readpump：endPrompt + flushPending 接线

**改**：`internal/chatsession/readpump.go`
- `EventDone` 分支：`endPrompt(PromptEndClean)` → `flushPending()`
- `EventError` 分支：`endPrompt(PromptEndError)` → `flushPending()`
- default 分支：`SetBusy()` + touch `as.currentPrompt.LastProgressAt`
- EventInit 分支：保持原样（不 SetBusy）
- `!ok` 分支：保持原样（本次不动）
- EventHandler 调用的 userMsgID 来源：`as.currentPrompt.LastMessageID`（nil 时传空）

**依赖**：T06, T10
**AC**：两条终态分支走 endPrompt + flushPending；LastProgressAt 被 touch；handler 收到 anchor

### T12. feishu 包：PromptState 私有化 + 收缩

**改**：
- `internal/agent/prompt_state.go` 删
- `internal/channel/feishu/prompt_state.go` 新增（`PromptState int`，常量 `PromptRunning/PromptDone`）
- `internal/channel/feishu/receipt.go` 改用 `feishu.PromptState`（替换 `agent.PromptState`）
- 其他可能的 caller（CodeGraph 查）

**依赖**：无（独立）
**AC**：`agent` 包无 PromptState；feishu 包本地定义且 2 值

### T13. feishu 包：mapStateToFeishuEmoji + receipt 改造

**改**：
- `internal/channel/feishu/adapter.go`：`mapStateToFeishuEmoji` case 改用 `MessageQueued/MessageSubmitted/MessageDropped`；删 MessageDone/MessageFailed case
- `internal/channel/feishu/receipt.go`：构造初始 `promptState` 改 `PromptRunning`；删 `SetTaskListWithFooter` 里的 `Pending→Running` 转换判断；`PromptState()` 返回类型改 `feishu.PromptState`

**依赖**：T01, T12
**AC**：
- emoji 映射：Queued → OneSecond ⏳，Submitted → OnIt 🔄，Dropped → ""
- receipt 初始 Running；SetTaskList 不再做转换

### T14. cmd/nightme/run.go：dispatcher 改造

**改**：`newMessageDispatcher`
- dispatcher 入口构造 `*chatsession.Message`（ID=msg.MessageID 带 fallback，ChatID=msg.ChatID，Blocks=msg.Blocks，ReceivedAt=msg.Time，Stage=MessageQueued）
- 发 `MessageQueued` 替换当前 `MessageReceived`（line 565）
- 删 `MessageForwarded` emit（line 600）
- 调 `cs.QueueUserMessage(msg)` 替换原 `(blocks, userMsgID)`
- spawn 失败路径不变（只发 OutReply，不创建 Message）

**依赖**：T01, T07
**AC**：dispatcher 不再产生 Received/Forwarded；Queued 在 spawn 之前发出

### T15. cmd/nightme/run.go：/kill、/new 调用 MarkDropped

**改**：slash 命令处理
- `/kill`：调 `cs.MarkDropped` 对所有 queued message（通过 `cs.BufferClear` → T10 已实现）
- `/new`：同上（视当前 BufferClear 是否调用而定）

**依赖**：T10
**AC**：/kill 后剩余 queued message 的 wire reaction 为空（Dropped → 无 reaction）

### T16. 测试更新：chatsession

**改**：`internal/chatsession/*_test.go`
- `input_buffer_test.go`：FlushHook 签名、Add 签名、bufferEntry 不可见
- `flushhook_test.go`：PromptHook 签名、提交后 Stage=Submitted
- `buffer_swap_test.go`：currentTurnUserMsgID 断言改为 as.currentPrompt.LastMessageID
- `message_state_test.go`：删 MessageDone/MessageFailed 用例；改 MessageReceived/MessageForwarded 为 Queued/Submitted
- `readpump_test.go`, `readpump_real_pi_test.go`, `readpump_fsm_test.go`：终态分支断言 endPrompt 被调 + flushPending 被调；LastMessageID 来自 currentPrompt
- `chatsession_test.go`：currentTurnUserMsgID 不可见，改读 currentPrompt

**依赖**：T11 完成
**AC**：`go test ./internal/chatsession/...` 全绿

### T17. 测试更新：feishu

**改**：`internal/channel/feishu/receipt_test.go`
- 初始状态断言改 `PromptRunning`
- 删 Pending→Running 转换相关断言
- mapStateToFeishuEmoji 断言（如有）改 Queued/Submitted/Dropped

**依赖**：T13 完成
**AC**：`go test ./internal/channel/feishu/...` 全绿

### T18. 全量验证

**执行**：`go vet ./... && go build ./... && go test ./...`
- 修任何残留
- 跑 `readpump_real_pi_test`（如果可跑）

**依赖**：T14-T17 全完成
**AC**：全绿；无 vet warning

### T19. 文档同步

**改**：
- `docs/FEATURES.md`：勾掉/更新对应条目（具体看现状）
- `docs/feat/message_lifecycle.md`：如有实现与设计偏差，更新对应段落
- commit message：明示 ✅/👎 反应下线 + `endPrompt(ProcessDied)` 留待下次

**依赖**：T18 完成
**AC**：文档与代码一致；commit message 含 UX 回归说明

---

## 6. 关键实现细节

### 6.1 `Prompt.ID` 生成（user #2 决定挂 AS）

```go
// AgentSession
func (as *AgentSession) NewPromptID() string {
    n := as.promptCounter.Add(1)
    return fmt.Sprintf("%s-p%d", as.ID, n)
}
```

`as.ID` 现有格式（agentSessionMeta 决定，CodeGraph 查），假设 `as_3` 等。新 ID 如 `as_3-p7`。

### 6.2 defaultPromptHookLocked 实现骨架

```go
func (cs *ChatSession) defaultPromptHookLocked() PromptHook {
    return func(p *Prompt) error {
        as := cs.activeAS
        if as == nil || as.Handle() == nil {
            return ErrNotRunning
        }
        // 候选 Prompt 已经由 flushPending 构造（含 MessageIDs/Blocks）
        // 本钩子只负责：SendBlocks → 成功翻 Stage + 装 currentPrompt
        err := as.SendBlocks(as.OpContext(), p.Blocks)
        if err != nil {
            if errors.Is(err, context.Canceled) {
                return fmt.Errorf("flush: AS backgrounded during send (likely /use): %w", err)
            }
            return err
        }
        cs.mu.Lock()
        now := time.Now()
        p.AckedAt = now
        p.LastProgressAt = now
        for _, mid := range p.MessageIDs {
            if m, ok := cs.messagesByID.Load(mid); ok {
                msg := m.(*Message)
                msg.Stage = agent.MessageSubmitted
                msg.PromptID = p.ID
                cs.emitMessageState(msg.ID, agent.MessageSubmitted)  // 需要一个内部 helper
            }
        }
        as.currentPrompt = p
        cs.mu.Unlock()
        return nil
    }
}
```

注：`as.currentPrompt = p` 在 `cs.mu` 下写，违反"currentPrompt 是 AS 状态"语义，但避免双锁；T11 后 readpump 也在 `cs.mu` 下读 `as.currentPrompt.LastMessageID`，一致。

### 6.3 flushPending 实现骨架（ChatSession 层，而非 InputBuffer）

把 flushPending 放在 ChatSession 而不是 InputBuffer，因为 Prompt 构造需要 ChatSession + AgentSession 信息（blocks 已经合并，MessageIDs 来自 InputBuffer，但 ChatSessionID + AgentSessionID + LastMessageID 需要 ChatSession 上下文）。

```go
// ChatSession.flushPending 调 inputBuffer.Drain() 拿 []*Message
// 构造 Prompt → 调 defaultPromptHookLocked()(p)
```

或者反过来：InputBuffer.flushPending 接收一个 factory 函数 `func([]*Message) *Prompt`，由 ChatSession 提供。这样 InputBuffer 仍是纯 FSM（user #3 提到的"ChatSession 不持有 active 读队列"的延续）。

倾向后者（InputBuffer 仍纯 FSM），保持关注点分离。

### 6.4 `MessageReceived` 是不是已经 `Queued`？

对照 `cmd/nightme/run.go:565`：

```go
cs := mgr.GetOrCreate(msg.ChatID, primary)
cs.EmitMessageState(userMsgID, agent.MessageReceived)
```

`GetOrCreate` 之后立即 emit，不依赖 AS 存在。所以 Queued 的语义就是"系统知道这条消息了"，与 user #4 一致。

### 6.5 `MessageDropped` 的反应

`mapStateToFeishuEmoji(MessageDropped) = ""` —— 不在用户消息上加 reaction。但是为了让"被清空"这个事实在用户侧可见，**commit message 明示**：当前 Feishu 实现下 Dropped 不会有任何视觉反馈，留待 UX PR 处理。

---

## 7. 风险与回滚

| 风险 | 影响 | 缓解 |
|---|---|---|
| `MessageDone/MessageFailed` 物理删除导致 wire 兼容破坏 | Feishu 端 ✅/👎 反应消失（已是设计决策） | commit message 明示；Feishu `mapStateToFeishuEmoji` 改成接 `MessageDropped` 返回 "" |
| `currentPrompt` 写在 `cs.mu` 下与 AS 自治冲突 | AS 自己未来访问 currentPrompt 时要排 cs.mu | 接受；下次重构再迁 |
| `endPrompt` 不遍历 MessageIDs，测试期望 fan-out 的用例会挂 | 测试更新（T16） | T16 同步改 |
| `flushPending` 路径未覆盖 → 现有测试依赖旧 OnTurnEnded 行为 | 编译失败 | T06 已先删字段暴露所有 caller |
| 重复投递触发 agent 端误判（agent 期望 1:1） | 用户接受（user #B） | agent 自己处理；不在我们层做去重 |

**回滚**：单 PR，回滚即 `git revert`；无需数据迁移（无持久化）。

---

## 8. 执行顺序一览

```
T01 (MessageState 改名) ─┐
                         ├─→ T02 (Message) ─→ T05 (messagesByID+MarkDropped) ─┐
                         │                                                    ├─→ T09 (defaultPromptHook) ─→ T10 (endPrompt+BufferClear)
                         │                                                    │
T03 (Prompt 类型) ─→ T04 (AS currentPrompt) ─→ T06 (删 currentTurnUserMsgID) ─┘                              │
                                                                                                              ↓
T07 (InputBuffer: *Message) ─→ T08 (InputBuffer: flushPending/Clear) ──────────────────────────────────────────┘
                                                                                                              ↓
T11 (readpump: endPrompt + flushPending) ←────────────────────────────────────────────────────────────────────┘
                                                                                                              ↓
T12 (feishu PromptState) ─→ T13 (feishu emoji + receipt) ←─────────────────────────────────────────────────────┘
                                                                                                              ↓
T14 (dispatcher MessageQueued) ─→ T15 (/kill /new MarkDropped) ←──────────────────────────────────────────────┘
                                                                                                              ↓
T16 (chatsession tests) ─┐
                         ├─→ T18 (全量验证) ─→ T19 (文档同步)
T17 (feishu tests) ──────┘
```

依赖图中 T01/T02/T03/T07 可并行起手（不依赖彼此），T12 独立分支。T18 是合并点。

---

## 9. References

- [`docs/feat/message_lifecycle.md`](../docs/feat/message_lifecycle.md) — 设计文档
- [`tasks/wip-message-prompt.md`](./wip-message-prompt.md) — 原始 dev task brief
- `docs/SPEC.md` §2.5 Message Lifecycle Tracking
- `docs/feat/F-31-message-state.md`、`F-42-lazy-receipt-creation.md`、`F-44-outreply-independent-and-task-receipt.md`