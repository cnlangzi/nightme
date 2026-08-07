# 开发任务：Message / Prompt 改名与对象化

> **Status**: ✅ **Phase 0 已落地**（commits `5999057` + `9b3cd38` + `14b9dc6` + `fa5e9c5` on `main`，
>   PR #65, 2026-08-08）。Phase 1（`!ok` 分支 `endPrompt(ProcessDied)` 收口 + `SetAgentExitObserver` 接线）仍 TBD。
> **Design doc**: [`docs/feat/message_lifecycle.md`](../docs/feat/message_lifecycle.md)（先读这个，理解
>   "为什么"和"最终形态"；本文档只管"怎么改代码"）
> **Scope**: `internal/chatsession`（`Message`/`Prompt` 核心对象）+ `internal/agent`（`MessageState`/
>   `PromptState` 枚举收敛）+ `internal/channel/feishu`（`receipt.go` 的 `PromptState` 迁移）+
>   `cmd/nightme/run.go`（接线更新）
> **不含**：L1-L4 探活/健康监控相关的开发任务（`Pinger`、stall watchdog、`nightme health` 扩展、
>   `AgentSession` 状态队列）——那些见 [`tasks/wip.md`](./wip.md)，依赖本任务但不在本任务范围内

---

## 1. 最终设计摘要（对照实现，细节看设计文档）

- `chatsession.Message.Stage`：`Queued` / `Submitted` / `Dropped` 三态，纯投递语义，不镜像执行结果。
- 投递失败（`SendBlocks` 报错）：**不创建 `Prompt`**，消息保持 `Queued`；重试机制留到下一个 PR，本次
  只需要保证"不误判成 `Dropped`，也不误判成 `Submitted`"。
- `Dropped` 只对应主动清空队列的场景（例如 `/new`、`/kill`、`ClearBuffer` 调用），不覆盖投递失败。
- `chatsession.Prompt`：诞生即 `Running`（没有 `Pending` 阶段），终态只有 `Done`（不再区分
  `Succeeded`/`Failed`），原因单独放 `EndReason`。
- `endPrompt` **不再需要对 `Prompt.MessageIDs` 做终态 fan-out**——每条 `Message` 在 `Submitted` 那一刻
  已经拿到了自己最终的状态，不需要等 `Prompt` 结束后再广播。这是相对 `tasks/wip.md` 最初草案（`endPrompt`
  伪代码里那段 `for _, msgID := range p.MessageIDs { cs.EmitMessageState(...) }`）的关键简化，**实现时
  不要照抄那段旧伪代码**。

---

## 2. 代码证据（现状缺口，锚定改动点）

### 2.1 `MessageForwarded` 触发时机过早

```560:600:cmd/nightme/run.go
cs := mgr.GetOrCreate(msg.ChatID, primary)
cs.EmitMessageState(userMsgID, agent.MessageReceived)
_, err := cs.LookupActiveAgentSession()
...
cs.EmitMessageState(userMsgID, agent.MessageForwarded)
```

`MessageForwarded` 在 `LookupActiveAgentSession()` 成功后立刻触发，发生在 `QueueUserMessage` →
`SendBlocks` 真正调用**之前**。新设计里 `Submitted` 必须严格对齐"提交成功"的事务边界，不能在这个
时间点触发。

### 2.2 `Prompt` 从未被实体化

```84:87:internal/chatsession/input_buffer.go
type bufferEntry struct {
    Blocks    []agent.ContentBlock
    UserMsgID string
}
```

```62:65:internal/chatsession/input_buffer.go
type FlushHook func(combined []agent.ContentBlock, userMsgIDs []string) error
```

```507:557:internal/chatsession/chatsession.go
func (cs *ChatSession) defaultFlushHookLocked() FlushHook {
    return func(combined []agent.ContentBlock, userMsgIDs []string) error {
        ...
        cs.currentTurnUserMsgID = userMsgIDs[n-1]
        ...
        err := as.SendBlocks(as.OpContext(), combined)
```

```749:776:internal/chatsession/chatsession.go
func (cs *ChatSession) emitMessageStateForCurrentTurn(state agent.MessageState) {
    cs.mu.Lock()
    id := cs.currentTurnUserMsgID
    cs.currentTurnUserMsgID = ""
    ...
```

"一次提交给 agent 的执行单元"今天只是 flush 时临时拼出来的裸元组，唯一的"身份"是 ChatSession 上
一个用完即扔的字符串标量。批次合并时只有最后一条消息（anchor）能收到终态——这是新设计要修的问题，
但**修法不是 fan-out**（旧草案的思路），而是让每条消息在 `Submitted` 时就已经是终态，不需要等
`Prompt` 结束。

### 2.3 命名冲突：`agent.PromptState` 已经是 shipped 代码，但形状要收缩

`internal/agent/prompt_state.go` 定义的 `PromptState`（`Pending`/`Running`/`Succeeded`/`Failed`）不是
本任务新提出的类型——它是 F-44 已经 shipped、被 `internal/channel/feishu/receipt.go` 使用的 Channel
内部状态机。关键发现（降低了迁移风险评估）：

- `PromptSucceeded`/`PromptFailed` 这两个值**在生产代码里从未被真正赋值过**——`receipt.go` 原话注释：
  "The receipt does not transition to Succeeded / Failed (terminal signals travel via OutMessageState
  → AddReaction, not via receipt state)"。F-44 的回归测试（`receipt_test.go`）甚至专门断言卡片里
  **不应该**出现 `✅ 已完成` / `❌` 这类字样。也就是说，砍掉这两个值本身是**零功能影响**的机械改动。
- `PromptPending` 是活的：`receipt.go` 构造函数的初始值 + `SetTaskListWithFooter` 里 "Pending→Running"
  的转换判断。新设计"诞生即 Running"要求这里改成构造时直接给 `Running`，去掉那个转换判断。

真正驱动"任务成功/失败"视觉反馈的，是 `agent.MessageState`（`MessageDone`/`MessageFailed`）→
`adapter.go` 的 `mapStateToFeishuEmoji` → 加在**用户原始消息**上的表情反应（✅ / 👎），**不是**卡片。

```2475:2487:internal/channel/feishu/adapter.go
func mapStateToFeishuEmoji(state agent.MessageState) string {
	switch state {
	case agent.MessageReceived:
		return "OneSecond" // ⏳
	case agent.MessageForwarded:
		return "OnIt" // 🔄
	case agent.MessageDone:
		return "DONE" // ✅
	case agent.MessageFailed:
		return "THUMBSDOWN" // 👎
	}
	return ""
}
```

新设计里 `Message.Stage` 不再有 `Done`/`Failed`，这两个值以后不会再被 `chatsession` 触发。**这次任务
不改这部分 Feishu 渲染代码**（`mapStateToFeishuEmoji` / 用户消息 reaction 的退役 / 卡片展示终态的
恢复），留给独立的后续任务——但需要决定 §5 里列的"死值怎么处理"问题，避免类型层面留下误导性的死代码。

---

## 3. 对象设计（实现级，含字段/类型）

### 3.1 `chatsession.Message`

```go
type Message struct {
    ID         string // channel-native id，退化时用 UserID:RFC3339Nano（沿用现有生成规则）
    ChatID     string
    Blocks     []agent.ContentBlock
    ReceivedAt time.Time

    // PromptID 在这条消息进入 Submitted 时回填；之前为空。
    // 一旦设置不可变。
    PromptID string
}

type MessageStage int

const (
    MessageQueued MessageStage = iota
    MessageSubmitted
    MessageDropped
)
```

### 3.2 `chatsession.Prompt`

```go
type Prompt struct {
    ID             string   // 建议 as.ID + "-p" + 单调序号，如 "as_3-p7"
    ChatSessionID  string
    AgentSessionID string   // 提交目标，创建时快照
    MessageIDs     []string // 有序，合并进这个 Prompt 的全部 Message.ID
    Blocks         []agent.ContentBlock

    CreatedAt   time.Time // 创建（诞生即 Running，无需单独的 Pending 时间戳）
    AckedAt     time.Time // SendBlocks 返回 nil 的时刻——正式提交成功的权威时间戳

    LastProgressAt time.Time // 每个 AgentEvent 到达都 touch（供 tasks/wip.md 的 L2 使用）

    EndedAt   time.Time
    EndReason PromptEndReason
}

type PromptEndReason int

const (
    PromptEndClean       PromptEndReason = iota // EventDone
    PromptEndError                               // EventError
    PromptEndProcessDied                         // channel 关闭但无 Done/Error（tasks/wip.md L3）
    PromptEndStalledKilled                       // L2 watchdog 主动杀掉（tasks/wip.md L2，本任务不实现检测逻辑）
    PromptEndUserKilled                          // /kill
)
```

**待定（见 §5 开放问题）**：`Prompt` 要不要一个独立的 `State`/`Status` 字段（`Running`/`Done` 两值），
还是干脆只用 `EndedAt.IsZero()` 判断，不设专门字段。

### 3.3 完整符号表（老 → 新）

| 概念 | 现有写法（含 Turn 残留） | 重命名后 |
|---|---|---|
| 提交单元（结构体） | 无实体，只是裸元组 | `Prompt`（结构体） |
| 终止原因枚举 | 无 | `PromptEndReason`：`Clean/Error/ProcessDied/StalledKilled/UserKilled` |
| ChatSession 上"当前进行中提交"的引用 | `currentTurnUserMsgID string` | `currentPrompt *Prompt`（挂在哪个对象上见 §5 开放问题） |
| 结束当前提交 + 触发下一批 flush（今天混在一起） | `OnTurnEnded() error` / `Flush() error`（二者等价） | 拆成两个单一职责方法：`endPrompt(reason PromptEndReason)`（收口当前 `Prompt`，清空 `currentPrompt`，**不**对 `MessageIDs` 做状态 fan-out）+ `flushPending() error`（把排队的 `Message` 打包成新 `Prompt` 并提交；`/flush` 命令和 agent 结束后自动触发都调用同一个 `flushPending`） |
| flush 回调签名 | `FlushHook func(combined []ContentBlock, userMsgIDs []string) error` | `PromptHook func(p *Prompt) error` |
| 消息侧的投递状态 | `agent.MessageState`（`Received/Forwarded/Done/Failed`，混了投递和执行两层语义） | `chatsession.Message.Stage`（`Queued/Submitted/Dropped`，纯投递，不镜像执行结果） |
| `bufferEntry` | `bufferEntry{Blocks, UserMsgID}` | 直接用 `Message`（多一个 `PromptID` 字段） |
| Prompt 执行状态 | `agent.PromptState`（`Pending/Running/Succeeded/Failed`，Feishu 卡片用，`Succeeded`/`Failed` 死代码） | 收缩为 `Running`/`Done` 两值（类型归属见 §5 开放问题） |

---

## 4. 迁移检查清单（涉及的文件）

- [ ] `internal/chatsession/input_buffer.go`：`bufferEntry` → `Message`；`FlushHook` → `PromptHook`；
      `Flush()`/`OnTurnEnded()` 拆成 `flushPending()`/`endPrompt()`。
- [ ] `internal/chatsession/chatsession.go`：`currentTurnUserMsgID` → `currentPrompt`（归属见开放问题）；
      `defaultFlushHookLocked` 改造为构造 `Prompt` 并调用 `PromptHook`；`emitMessageStateForCurrentTurn`
      整体重新设计（不再需要"当前 turn 的 anchor"这个概念，`Message.Stage` 在 `Submitted` 时已经是终态）。
- [ ] `internal/chatsession/readpump.go`：`runReadPump` 里 `EventDone`/`EventError`/`!ok` 三个分支改调
      统一的 `endPrompt(reason)`；不再触发任何 `Message` 状态变更（`Message` 早在提交时就已定终态）。
- [ ] `internal/agent/message_state.go`：`MessageState` 枚举收敛（是否物理删除 `MessageDone`/
      `MessageFailed`，见开放问题）。
- [ ] `internal/agent/prompt_state.go`：`PromptState` 收缩为 2 值（或者干脆搬到 `chatsession` 包里做成
      新类型，见开放问题）。
- [ ] `internal/channel/feishu/receipt.go`：构造函数初始值改 `Running`（不再有 `Pending`）；去掉
      `SetTaskListWithFooter` 里 "Pending→Running" 的转换判断。
- [ ] `internal/channel/feishu/receipt_test.go`：更新初始状态断言（不再存在 `Pending`）。
- [ ] `internal/channel/feishu/adapter.go`：**本次不改** `mapStateToFeishuEmoji` / 终态 reaction 逻辑
      （功能性改动留给独立后续任务）；只需要确认 `chatsession` 不再触发 `MessageDone`/`MessageFailed`
      后这条路径的降级行为是可接受的（不再有终态表情，只停在 🔄）。
- [ ] `cmd/nightme/run.go`：`newMessageDispatcher` 等调用点同步更新命名。
- [ ] 相关测试：`internal/chatsession/*_test.go`（尤其 `buffer_swap_test.go`、
      `chatsession_test.go` 里断言 `currentTurnUserMsgID` 的用例）。

---

## 5. 开放问题（实现前需要拍板）

1. **`Prompt` 要不要一个专门的执行状态字段？** 选项 A：`State`（`Running`/`Done` 两值，新类型，不复用
   `agent.PromptState` 避免同名不同形状）。选项 B：不设字段，直接用 `EndedAt.IsZero()` 判断，`EndReason`
   为零值时视为仍在运行。倾向 A（可读性/日志友好），但需要定最终类型名和归属包。
2. **`agent.PromptState`（Feishu 卡片用的那个）收缩之后叫什么？** 是否还叫 `PromptState`（4 值收缩成 2
   值，原地改），还是整体挪到 `internal/channel/feishu` 包私有化（因为它本质上是 Channel 内部状态，
   `agent` 包只是历史上顺手放的地方）？
3. **`agent.MessageState` 的 `MessageDone`/`MessageFailed` 两个值物理删除，还是保留占位？** 删除是更
   干净的收敛，但要确认没有遗漏的引用（`mapStateToFeishuEmoji`、`messageStates` 缓存里的终态判断逻辑
   等）；保留则要在注释里明确写清楚"chatsession 不会再产生这两个值"。
4. **`currentPrompt` 挂在 `ChatSession` 还是 `AgentSession` 上？** 本次任务只需要 `ChatSession` 语义
   正确即可（对应今天 `currentTurnUserMsgID` 的位置）；但 `tasks/wip.md` 的 L1.5（`AgentSession` 状态
   队列）依赖 `currentPrompt` 挂在 `AgentSession` 上才能正确工作。建议**本次直接挂在 `AgentSession`**，
   `ChatSession` 通过 `cs.activeAS.currentPrompt` 访问，避免以后为了 L1.5 返工。
5. **`Prompt` 要不要持久化？** 倾向：只在内存维护"当前 + 最近 K 个"（用于调试/未来的 `/health`），不写
   registry。如果需要"崩溃后还能看到上一个 Prompt 卡在哪一步"，则需要落盘，待确认。
6. **`PromptID` 生成方案**：建议 `as.ID + "-p" + 单调序号`（如 `as_3-p7`），足够可读、无需引入新依赖。
7. **`Message.ID` 生成规则**：假设保持现状不变（channel-native id 优先，否则 `UserID:RFC3339Nano`），
   只是包一层结构体。
8. **投递失败后消息留在 `Queued`，重试机制什么时候触发？** 本次任务只需要保证状态定义正确（不误判
   成 `Dropped`/`Submitted`），具体的重试触发点（下一次用户消息到达时？定时器？手动 `/flush`？）留给
   下一个 PR，这里只需要确认现在的行为不会比"重试机制上线前"更差（不能比现状——静默丢弃/日志兜底——
   更差，但也不强求现在就修好）。

---

## 6. 分阶段计划（本任务范围内）

| Phase | 内容 | 性质 | 风险 |
|---|---|---|---|
| **Phase 0** | `Message`/`Prompt` 对象重构：新增两个类型，替换 `bufferEntry`/`FlushHook`/`currentTurnUserMsgID`；`agent.MessageState`/`agent.PromptState` 收敛；`Turn` 系命名全部退役 | 重构，无新行为（Feishu 渲染层不动） | 中——改动面覆盖 `internal/chatsession` 全部读写 `currentTurnUserMsgID`/`OnTurnEnded` 的地方，需要配套改测试 |
| **Phase 1** | 在 Phase 0 的对象上修 `!ok` 分支统一收口（`endPrompt`）+ `SetAgentExitObserver` 接线（详见 `tasks/wip.md` §L3，属于交界地带，两边都要看） | Bug fix | 低——不改变对外 wire 契约 |

Phase 0 是 `tasks/wip.md` 里 L1-L4 所有后续 Phase 的地基，建议先落地。

---

## 7. References

- [`docs/feat/message_lifecycle.md`](../docs/feat/message_lifecycle.md) — 设计文档（先读这个）
- [`tasks/wip.md`](./wip.md) — 建立在本任务之上的健康监控体系（L1-L4）
- `docs/SPEC.md` §2.5 Message Lifecycle Tracking
- `docs/feat/F-31-message-state.md`
- `docs/feat/F-42-lazy-receipt-creation.md`
- `docs/feat/F-44-outreply-independent-and-task-receipt.md`
