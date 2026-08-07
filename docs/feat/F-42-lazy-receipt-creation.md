# F-42: Lazy Receipt Creation + Simplified MessageState + TaskList Markdown Title

> **Status**: 📝 设计阶段（doc-first）— **SUPERSEDED by F-53（部分）**
> **Milestone**: v1.3.x
> **Scope**: `internal/channel/feishu/{adapter.go,receipt.go,receipt_task.go}` + 文档同步
> **Depends on**: F-25 (rolling-log receipt), F-31 (MessageState), F-37 (thread routing), F-38 (task checklist), F-40 (OutReply overflow)
> **Related**: [`SPEC.md`](../SPEC.md) §0.10 / §2.4 / §2.5; [`channel/feishu.md`](../channel/feishu.md) §6.6 / §13.20 / §15.0
>
> **Superseded 部分**（F-53 已重做）：
> - §0.2 "⏳ / 🔄 reactions 跟其他 waiting 信号重复" 的判断过时：F-53 决定 Phase 0 **彻底删除** ✅ / 👎
>   反应（`MessageDone` / `MessageFailed` 物理删除），**不**只是删 ⏳ / 🔄。本文 §0.2 的"F-42 选择"在 v1.3.x 不再适用。
> - "✅ / ❌ 是**终态不可替代**的确认，保留" —— ❌（对应 `MessageFailed`）已删除；✅ 对应的
>   `MessageDone` 也已删除。用户消息 reaction 终态不再由 MessageState 承载。
>
> F-42 的其它内容（lazy receipt creation、TaskList 标题）仍有效，未被 F-53 影响。
> 新设计权威定义见 [`feat/message_lifecycle.md`](./message_lifecycle.md) §7。

---

## 0. 背景

Feishu receipt 在 v1.3 + F-25 落地后已经形成完整 rolling-log 形态，但渲染层仍有 3 处冗余 / 不清晰：

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
// 之前 (v1.3 - F-42 前):
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
         docs/SPEC.md (§0.10 changelog)
         docs/channel/feishu.md (§6.6.7 + §13.20 + §15.0 status)
         docs/FEATURES.md (§1b 加 F-42 行)
         docs/feat/F-42-lazy-receipt-creation.md (NEW)

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

## 7. 不叫 v2.0 的理由

v1.3 全部不变式保留 — F-42 纯 Channel 自治范围内的渲染细节调整：
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

## 9. 变更日志

- **2026-08-05** — docs-first design locked：lazy receipt + simplified MessageState（drop ⏳/🔄, 留 ✅/❌）+ TaskList markdown 标题。Feishu adapter 自决中间态渲染。