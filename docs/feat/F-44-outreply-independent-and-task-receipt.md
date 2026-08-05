# F-44: OutReply 拆出 Receipt + Task Receipt 瘦身 + OutInit/OutUsage 推迟

> **Status**: 📝 设计阶段（doc-first，2026-08-05）
> **Milestone**: v1.3.x
> **Scope**: `internal/channel/feishu/{adapter.go, receipt.go, receipt_event.go, card_sanitize.go}` + 文档同步
> **Depends on**: F-25 (rolling-log receipt), F-37 (multi-div split + thread routing), F-38 (task checklist), F-39 (OutResult 独立 reply), F-40 (OutReply 改名 + 600B truncate 删除 + overflow bail-out), F-42 (lazy receipt creation)
> **Related**: [`SPEC.md`](../SPEC.md) §0.11 / §2.4; [`channel/feishu.md`](../channel/feishu.md) §12 / §13.21 / §18

---

## 0. 背景

### 0.1 现状（被改）

F-25 → F-40 → F-42 三轮演进后，Feishu receipt card 承担了 4 类内容：

```
┌─ 当前 receipt card (rolling-log + 元数据) ─┐
│ Header (prompt state icon ⏳/🔄/✅/❌)    │ ← F-25
│ Entries (OutReply text chunks，多 div)     │ ← F-25 → F-40
│ **📋 Tasks** checklist (F-42 title)       │ ← F-38 → F-42
│ <hr> + footer (init / usage)              │ ← F-25
└────────────────────────────────────────────┘
```

承载逻辑由 `MessageReceipt` 状态机 + `buildReceiptCard` + `renderLocked` 共同维护。**每加一类内容就要扩 receipt 一段代码**（F-38 加 TaskList、F-42 加 title、F-40 加 multi-div），经过 5 轮迭代，receipt 渲染路径上的协调逻辑已经接近 ~1350 行（`buildReceiptCard` + `renderLocked` + `receipt_event.go` 整个文件 + `ensureReceipt*` + `isOverflowingReceipt` + `MessageReceipt.Append` + state machine 方法）。

### 0.2 三类问题

#### (1) OutReply fold 进 receipt 路径的价值被稀释

`OutReply` 是 agent **对** user 当前 turn 的 reply 主体（流式 chunks），但 fold 进 receipt 后：

- 用户看到的是 "1 张 card 反复 PATCH" — 需要等 PATCH 周期才能看到完整内容
- 多 div 拆 + 50 element / 30 KB envelope 防御需要复杂预算逻辑（F-37 + F-40 共同投入）
- `eventToEntry(EventText)` + `truncateForLog(text, 600)` 历史包袱虽已删（F-40），但 `entryToDiv` + `splitMarkdownForDivs` 在 receipt 路径里仍占 ~150 行
- 数量超限 / 长度超限 / 迟到 reply 三种 bail-out（F-40）逻辑让 `Send(OutReply)` case 变 ~80 行

**反观 OutResult**（F-39 已是独立 reply）：`sendResultAsReply` ~80 行，没有任何 fold / overflow / bail-out 协调。用户视觉上更清晰 — "完整答案作为独立气泡，rolling 进程折叠到 card 里" 不再是 UX 必需。

#### (2) Receipt "元数据容器" 职责不再需要

`OutInit`（agent 身份 / model / workspace / branch）和 `OutUsage`（tokens / cost）当前作为 receipt card 的 footer / header。F-42 之后：

- receipt lazy create 后，若 turn 没有任何 `OutReply` / `OutTask*`，receipt 不存在
- 此时 `OutInit` / `OutUsage` 走 F-42 silent drop — **token 成本信息丢失**
- 若 turn 有 `OutReply` / `OutTask*`，footer 跟 reply entries / tasks 挤在 50 element 预算里

Footer 设计（F-44 设计阶段讨论过）应该独立成"每次 ReplyInThreadAndChat 都带 footer"的语义（snapshot Init + live Branch），但这需要扩展 `OutboundMessage` wire format + ChatSession 状态 + EventHandler 协调 — **跨层改动太大**，应该单独一个 PR。

#### (3) Task Receipt 才是真正必要的 folding surface

`OutTaskCreate` / `OutTaskUpdate` 是结构化任务清单，多个事件 fold 成 1 张 card 才**有视觉价值**（持续 PATCH 一个 markdown checklist section）。这个表面跟 OutReply fold 完全不同的渲染需求：

- 单一 section（`**📋 Tasks**` + 多行 checkbox）
- 静态结构（每次 event 替换整段 snapshot，不追加 entries）
- 不需要 PATCH storm（snapshot 替换式更新）

Task Receipt 完全可以脱离 OutReply fold 路径独立存在。

### 0.3 简化目标

**三个独立 surface + 一个折叠 + 一个 top-level 应急 surface**：

| OutboundKind | 表面 | 锚定 | F-44 后 |
|---|---|---|---|
| `OutReply` | **Rolling-log receipt**（N 事件 = 1 张 card，每 chunk = 1+ div；超限转 top-level） | `ReplyInBoth` (reply endpoint, reply_in_thread omitted, 锚定 userMsgID)；overflow → `ReplyInChat` | ✅ F-44 revert: 重新 fold |
| `OutResult` | 独立 top-level Create（每条 = 1 条消息） | `ReplyInChat` (top-level Create, no anchor) | ✅ 不变（F-39） |
| `OutTaskCreate` / `OutTaskUpdate` | **Rolling-log receipt**（N 事件 = 1 张 card） | `ReplyInChat` (cold-start card 是 top-level Create; 后续 PATCH 保持) | ✅ 简化为只装 Tasks |
| `OutCard` / `OutCommandReply` / `OutCompaction` | ReplyInBoth（reply endpoint，reply_in_thread omitted） | 各自 surface | ✅ |
| `OutInit` / `OutUsage` | Silent drop | — | ⏸ 推迟到 footer PR |
| `OutThinking` / `OutToolStart` / `OutToolEnd` | ReplyInThread | thread 抽屉 | ✅ 不变 |

> **F-44 + F-44 follow-up timeline**:
> 1. **F-44 first-pass**: `OutReply` 改为独立 top-level Create (ReplyInChat), 原因 parent-thread gotcha (一旦同 turn `OutToolStart`/`OutToolEnd` 在 user message 上建 thread, 后续 `ReplyInBoth` 被拉进 thread 抽屉)
> 2. **F-44 follow-up #1**: 同样理由把 `OutResult` 切到 `ReplyInChat`
> 3. **F-44 follow-up #2**: 同样理由把 `OutTask*` 切到 `ReplyInChat` (cold-start)
> 4. **F-44 revert (this)**: `OutReply` 改回 fold 进 receipt card (`ReplyInBoth` 锚定 userMsgID)，原因: top-level Create 表面产生 N 个独立气泡, 视觉上难 scan。修法是 F-40 的 overflow bail-out — 一旦单 card 超过 50 elements / 30 KB envelope, 该 chunk 转 `ReplyInChat` (top-level Create, 永远在主 chat 可见)。`OutResult` / `OutTask*` 保持 `ReplyInChat`。

**F-44 revert 的 trade-off**:
- ✅ 视觉回到 F-25 → F-40 那种 "1 张 card 装 N 段 reply"，比 N 个独立气泡好 scan
- ✅ Overflow 自动 bail-out：极端长 reply（每 chunk 都 1 KB+ × 50 个）不会让 card 爆炸
- ❌ Receipt card 仍然受 parent-thread gotcha 影响（锚定 userMsgID），有 tool 的 turn 仍然会被吸进 thread 抽屉
- ❌ Overflow 的 chunk 视觉上跟 fold 的 chunk 不一样（独立气泡 vs card 内的 div），用户可能感觉到割裂

用户接受这个 trade-off：F-40 时代的 receipt card 是用户熟悉的视觉模型，且 overflow bail-out 保证了 "信息不会丢"。F-44 follow-up #1/#2 仍然解决 `OutResult` / `OutTask*` 的 thread 可见性问题（这两个没有 chunk-stream 视觉问题）。

### 0.4 为什么不是 v2.0

v1.3 核心不变式全部保留：

- `OutboundMessage` 字段不变（仅 Channel 内部渲染目标分流）
- Gateway 不持有 receipt / thread / ChatSession 状态概念
- ChatSession 不 import channel/feishu
- Channel 不 import chatsession
- `out.ReplyTo = cs.currentTurnUserMsgID` 不变
- §1.4 抽象 / 具体 边界规范保留
- F-31 / F-37 / F-38 / F-39 / F-40 / F-42 各自决策**保持成立**，F-44 是它们的**简化合并**

F-44 是 Channel 自治范围内的渲染目标重排：删除过时的 fold 路径 + 简化 task receipt + 推迟 footer 渲染。

---

## 1. 设计

### 1.1 视觉对比

**改前**（典型 turn：5 个 OutReply chunk + 2 个 OutTaskCreate + 1 个 OutResult）：

```
user_msg om_A
  ├ Receipt Card (rolling log + 元数据)
  │   ⏳ → 🔄 → ✅
  │   💬 chunk 1
  │   💬 chunk 2
  │   💬 chunk 3
  │   💬 chunk 4
  │   💬 chunk 5
  │   **📋 Tasks** checklist (2 items)
  │   <hr>
  │   footer (init / usage)
  └ Final Result Reply (独立 reply, 锚 om_A)
      📝 完整 OutResult text
```

**改后**（同样 turn）：

```
user_msg om_A
  ├ Thread (tool stream, only visible in side panel)
  │   💭 thinking
  │   🔧 Bash(ls)
  │   ⎿  file1
  │   file2
  ├ Task Receipt (ReplyInThreadAndChat, 锚 om_A — single card, 任务清单 PATCH)
  │   **📋 Tasks** checklist (2 items)
  └ main chat (top-level Create, no anchor — F-44 follow-up):
      ├ Reply 1  💬 chunk 1
      ├ Reply 2  💬 chunk 2
      ├ Reply 3  💬 chunk 3
      ├ Reply 4  💬 chunk 4
      ├ Reply 5  💬 chunk 5
      └ Final Result 📝 complete OutResult text
```

视觉变化：
- ✅ 每个 reply chunk 立刻可见（不再等 PATCH 周期，不被 thread 抽屉吸走）
- ✅ Task Receipt 单卡（不混 reply 流，PATCH 维护）
- ✅ Tool stream（💭/🔧/⎿）跟 reply 流完全分离 — tool 在 thread 抽屉，reply 在主 chat 流
- ⚠️ 失去 "Reply to <sender>" 头部（ReplyInChat 改写） — 跟 v1.3 行为一致（top-level bubble）

### 1.2 Routing 分流表（最终）

```go
// internal/channel/feishu/adapter.go::Send (case 分支)
switch msg.Kind {
case gateway.OutReply:
    // F-44: 每 chunk → 独立 ReplyInThreadAndChat
    return a.sendReplyInThreadAndChat(ctx, msg.ChatID, msg.ReplyTo, text)

case gateway.OutResult:
    // F-39 不变: 独立 ReplyInThreadAndChat
    return a.sendResultAsReply(ctx, msg.ChatID, msg.ReplyTo, text)

case gateway.OutTaskCreate, gateway.OutTaskUpdate:
    // F-44: rolling-log receipt 简化为只装 Tasks
    receipt, created, err := a.ensureReceiptForTask(ctx, msg.ChatID, msg.ReplyTo, msg.TaskList)
    if err != nil {
        return a.sendRawOutText(ctx, msg.ChatID, renderTaskFallbackText(msg.TaskList))
    }
    if !created {
        return receipt.SetTaskList(ctx, msg.TaskList)
    }
    return nil

case gateway.OutInit, gateway.OutUsage:
    // F-44: silent drop，footer 设计推迟
    return nil

// 其他 case 不变
case gateway.OutThinking: ...
case gateway.OutToolStart: ...
case gateway.OutToolEnd: ...
case gateway.OutMessageState: ...
case gateway.OutMessageStateRemoved: ...
case gateway.OutCompaction: ...
case gateway.OutCard: ...
case gateway.OutTyping: ...
case gateway.OutCommandReply: ...
}
```

### 1.3 Task Receipt 简化

> **注**：以下伪代码是简化示意。当前 `buildReceiptCard` 实际更内联 — 没有 `headerLine` / `footerLine` / `hrElement` 独立函数，prompt state header / footer / hr 逻辑直接写在 `buildReceiptCard` 主体内。F-44 后实际只剩 `buildTaskChecklistChunks(r.tasks)` 输出转 `tag:"markdown"` element 一段。

**当前 receipt card**（F-25 → F-42 累计）：

```go
// buildReceiptCard (简化伪代码)
elements := []any{}
elements = append(elements, headerLine(r))       // ⏳/🔄/✅/❌
for _, e := range r.entries {
    chunks := splitMarkdownForDivs(e.Icon+" "+e.Text, divTextCharLimit)
    elements = append(elements, divElements(chunks)...)  // OutReply entries
}
elements = append(elements, taskElements...)      // **📋 Tasks** checklist
elements = append(elements, hrElement)            // <hr>
elements = append(elements, footerLine(r))        // init / usage
```

**改后**：

```go
// buildReceiptCard (F-44 后)
elements := []any{}
elements = append(elements, taskElements...)      // **📋 Tasks** checklist ONLY
```

**删除的 receipt 段**：
- `headerLine(r)` — prompt state icon（⏳/🔄/✅/❌）
- entries loop — OutReply chunks（OutReply 不再 fold）
- `hrElement` — 跟 footer 一起删
- `footerLine(r)` — init / usage（推迟到 footer PR）

**保留的 receipt 段**：
- `**📋 Tasks**` + checklist（`buildTaskChecklistChunks` 输出）

### 1.4 `sendReplyInThreadAndChat` 新 helper

平行 `sendResultAsReply`（F-39）：

```go
// internal/channel/feishu/adapter.go (NEW)
//
// sendReplyInThreadAndChat 投递 OutReply chunk 为 ReplyInThreadAndChat 独立消息。
// 锚定 msg.ReplyTo（userMsgID）让 reply 在 main chat 可见且视觉上挂在 user msg 下。
//
// 跟 sendResultAsReply (F-39) 共享 3 段 dispatch:
//   - 无 markdown → MsgTypeText
//   - tables > 5 → MsgTypePost + tag:"md"
//   - 默认 → MsgTypeInteractive Card 2.0
//
// 唯一差别:
//   - 不加 icon 前缀（OutReply 是流延续,不是新条目）
//   - 28 KB envelope defense 复用
//   - 复用 SanitizeCardMarkdown + splitMarkdownForDivs
func (a *Adapter) sendReplyInThreadAndChat(
    ctx context.Context,
    chatID, userMsgID, text string,
) error {
    if strings.TrimSpace(text) == "" {
        return nil  // 空 reply 静默 drop,跟 F-40 行为一致
    }
    sanitized, err := SanitizeCardMarkdown(text)
    if err != nil {
        a.logger.Warn("feishu: reply sanitize failed, sending raw",
            "err", err, "chat_id", chatID, "user_msg_id", userMsgID)
        sanitized = text  // sanitize 失败降级用原文
    }
    if len(sanitized) > perReplyMaxBytes {
        sanitized = truncateForLog(sanitized, perReplyMaxBytes)  // envelope defense
    }
    msgType, body, err := buildResultPayload(sanitized)  // F-39 三段 dispatch 复用
    if err != nil {
        return fmt.Errorf("feishu: encode reply: %w", err)
    }
    _, err = a.sendContent(ctx, chatID, msgType, body, userMsgID, false)  // replyInThread=false = ReplyInThreadAndChat
    return err
}
```

**复用基础设施**（不新增 helper）：
- `SanitizeCardMarkdown` — 从 `card_sanitize.go` 移到 `result_render.go`（仍 exported，因为 `buildInteractiveCard` 也调用它，见 §2.1）
- `splitMarkdownForDivs` — 已在 `receipt_split.go`
- `buildResultPayload` — F-39 已存在，三段 dispatch（text / post+md / card）
- `sendContent` — 底层 send，复用 F-37 / F-39
- `truncateForLog` — `receipt_event.go:176` 原始 helper；`result_render.go::truncateRunes` 是 thin alias（F-39 加的）。F-44 后统一调 `truncateForLog`，`truncateRunes` 删除（仅 OutResult 一处用，无 caller）

**新增常量**：
- `perReplyMaxBytes = 6 * 1024`（与 `perResultMaxBytes` 同值，独立常量保证语义清晰）— 在 `result_render.go` 定义

### 1.5 Wire 形态

| OutboundKind | F-44 wire | Feishu API | main chat | thread 视觉 |
|---|---|---|---|---|
| `OutReply` | **ReplyInThreadAndChat**（每 chunk） | `POST /messages/{rootID}/reply`, `reply_in_thread` 字段省略 | ✅ 可见 *(group chat)* / ❌ thread-only *(p2p / topic)* | ✅ reply 视觉 |
| `OutResult` | ReplyInThreadAndChat | 同上 | ✅ *(group)* / ❌ *(p2p / topic)* | ✅ *(group)* / ❌ *(p2p / topic)* |
| `OutTaskCreate` / `OutTaskUpdate` | ReplyInThreadAndChat（rolling-log card） | `POST /messages/{rootID}/reply` 创建 + `PUT /messages/{id}` PATCH | 同上 | 同上 |
| `OutInit` / `OutUsage` | Silent drop | — | — | — |
| **fallback** *(p2p / topic)* | **ReplyInChat** | `POST /im/v1/messages` 顶级 Create | ✅ 可见 | ❌ 无 thread 关联 |

**关键术语**(来自 `docs/feat/F-37-tool-thread-routing.md` §2.1 + `docs/channel/feishu.md` §13.11):

- **ReplyInChat**:顶级 `Create` 端点,无 `root_id`,消息仅在 main chat 显示,**无 thread 关联、无 reply 箭头**
- **ReplyInThreadAndChat**:reply 端点 + `reply_in_thread` 字段省略,group chat 下消息显示在 main chat + thread(同正文,带 reply 箭头)
- **ReplyInThread**:reply 端点 + `reply_in_thread=true`,main chat 只显示 "X replies" 灰条,正文仅在 thread

**已知 chat-mode 影响**(2026-08-05 实机 probe,DM `oc_7cc94a3ed15afb8ac60c4ab7344d5cfd` + group `oc_4a06da49bc0131ff14b381498e4fed9d`):

| chat_mode | reply endpoint `thread_id=""` (ReplyInThreadAndChat) | reply endpoint `thread_id="omt_xxx"` (ReplyInThread) | Create 端点 (ReplyInChat) |
|---|---|---|---|
| `p2p`(DM) | ❌ 永远继承父消息 `thread_id`,看不到 main chat 可见版本 | ✅ thread-only(灰条 + thread) | ✅ **唯一** main chat 可见方式 |
| `group`(普通群)| ✅ 字段省略 / `false` → main chat 可见;`true` → thread-only | ✅ 行为如 doc 描述 | ✅ 也 main chat 可见 |
| `topic`(话题群)| ❌ SDK 注释:「若群聊已经是话题模式,则自动回复该条消息所在的话题」| ❌ 强制 topic,无法 escape | ❓ 需测试(推测:也不可见)|

**3 种形态实机确认**(2026-08-05 12:50~12:59,DM):

| Probe | Endpoint | 字段 | `thread_id` | 视觉 | 形态 |
|---|---|---|---|---|---|
| D | Create | n/a | `<empty>` | 独立气泡,main chat | ReplyInChat ✅ |
| C | reply | `true` | `omt_xxx` | 灰条 + thread | ReplyInThread ✅ |
| A/B | reply | omit / `false` | `omt_xxx`(DM 下)| 仅 thread(DM 特性) | (DM 下:仅 thread;group 下:ReplyInThreadAndChat) |

**F-44 P2/P3 backlog**:
- **P2**:chat-mode 探测(`chat_mode` LRU 缓存,类似 `messageStates`),p2p/topic 群下自动 fallback 到 `sendViaLarkCreate`,达成 main chat 可见
- **P3**:`p2p` / `topic` 群检测在 `Adapter` startup 时 warm-up,避免首条消息延迟
- **接受现状** 也行:当前实现跟 F-37 / F-40 在 group chat 下行为完全一致;p2p / topic 群用户在 DM 里看到 "X replies" 灰条不影响 main chat 可达性(只是不直观)

其他 OutboundKind 不变(F-37 已处理 thinking/tool/compaction 走 `ReplyInThread`,`OutCard` / `OutCommandReply` 走 `ReplyInThreadAndChat`)。

**`ReplyInThreadAndChat` 锚定语义**：所有 reply 都设 `root_id = userMsgID`，`reply_in_thread = false`（字段省略）。飞书端：消息在 main chat 可见正文，同时在 thread 入口处有视觉 reply 链。多个 reply 共享同一 `root_id` → 飞书把它们组织成"同一 user msg 的 reply 串"。

### 1.6 Receipt 状态机简化

**当前状态**（F-25 → F-42 累计）：

```
PromptPending → PromptRunning → PromptSucceeded / PromptFailed
                  ├ entries: append
                  ├ tasks: snapshot replace
                  └ init/usage: footer append
```

**改后状态**（F-44）：

```
PromptPending → PromptRunning → PromptSucceeded / PromptFailed
                  └ tasks: snapshot replace (only)
```

**删除的状态**：
- `entries []LogEntry` — 整个字段 + 配套 append / dedup / 截断 / 拆 div 逻辑
- `headerLine` / `footerLine` 计算 — header / footer sections 删除
- `promptHeaderLine` 调用 — header section 删除
- `promptState` 转 `PromptRunning` 时机 — 仍然保留（用于 SetCompleted / SetExecuting），但驱动源从 `OutReply` 首次到达改成 `OutTaskCreate/Update` 首次到达

**注意**：`promptState` 字段保留（仍由 `EventDone` / `EventError` 翻终态），但 state transition 触发源变化：
- 当前：`OutReply` 首次到达 → `PromptRunning`；后续 chunks 不再 transition
- 改后：`OutTaskCreate/Update` 首次到达 → `PromptRunning`（task receipt 创建时设）；`OutReply` 不再触发 transition
- 边缘 case：turn 无 task 仅有 reply → receipt 不存在 → `PromptRunning` / `PromptSucceeded` 永远不设（**符合 §1.4 边界**：state 是 Channel 内部状态，缺失不影响其他 Kinds 渲染）

---

## 2. 文件 & 接口

### 2.1 改动的文件

#### **`internal/channel/feishu/adapter.go`**

```go
// (1) Send case OutReply:重写
case gateway.OutReply:
    // F-44: 独立 ReplyInThreadAndChat,不再 fold
    text := strings.TrimSpace(msg.Text)
    if text == "" {
        return nil
    }
    return a.sendReplyInThreadAndChat(ctx, msg.ChatID, msg.ReplyTo, text)

// (2) Send case OutInit / OutUsage: silent drop
case gateway.OutInit, gateway.OutUsage:
    // F-44: silent drop;footer 设计推迟到 footer PR
    // 字段仍在 OutboundMessage wire 上(translate.go 不变),Channel 自决不渲染
    return nil

// (3) 新 helper sendReplyInThreadAndChat (见 §1.4)

// (4) 删除:ensureReceiptForReply / isOverflowingReceipt / sendReplyAsMessage
//     这三个 helper 是 F-40 / F-42 加的,F-44 不再需要
```

**删除的代码**：
- `ensureReceiptForReply` (~80 行)
- `isOverflowingReceipt` (~30 行)
- `sendReplyAsMessage` (~80 行)
- `Send` case `OutReply` 中 fold / overflow / late-reply / no-receipt-fallback 协调逻辑 (~60 行)
- `Send` case `OutInit` / `OutUsage` 中 `receipt.Append` 调用 (~30 行)

#### **`internal/channel/feishu/receipt.go`**

```go
// (1) buildReceiptCard 简化:只剩 tasks section
func buildReceiptCard(r *MessageReceipt) (string, error) {
    elements := []any{}
    if chunks := buildTaskChecklistChunks(r.tasks); len(chunks) > 0 {
        for _, c := range chunks {
            elements = append(elements, map[string]any{
                "tag": "markdown",
                "content": c,
            })
        }
    }
    card := map[string]any{
        "schema": "2.0",
        "config": map[string]any{"wide_screen_mode": true},
        "body":   map[string]any{"elements": elements},
    }
    // encodeCardJSON ...
}

// (2) renderLocked 简化:只剩 task snapshot 替换路径
func (r *MessageReceipt) renderLocked(ctx context.Context) error {
    // 保留:task snapshot replace → buildReceiptCard → PATCH
}

// (3) 删除的内部方法:
func (r *MessageReceipt) Append(...) error            // ← 删除,见 §2.1 备注
func (r *MessageReceipt) SetExecuting(...) error     // ← 删除(无 caller)
func (r *MessageReceipt) SetCompleted(...) error     // ← 删除(无 caller)
func (r *MessageReceipt) appendEntryLocked(...)      // ← 删除(Append 私有)
func (r *MessageReceipt) lastEntryLocked() *LogEntry // ← 删除(Append 私有)
func (r *MessageReceipt) EntryCount() int            // ← 删除(isOverflowingReceipt 唯一 caller 删除)
func (r *MessageReceipt) evictOverflowLocked()       // ← 删除(appendEntryLocked 私有)

// (4) 保留:SetTaskList (task snapshot replace)

// (5) 删除的字段:
//     entries []LogEntry                  ← appendEntryLocked 唯一写者删除,无读者
//     agentName / workspace / branch      ← OutInit silent drop,无写者;buildReceiptCard 不读
//     inputTokens / outputTokens          ← OutUsage silent drop,无写者;buildReceiptCard 不读
//     completedAt                         ← promptHeaderLine 整体删除,无读者;footer PR 重新引入 header 时同步加回
// 保留的字段:
//     promptState                         ← 保留(用于 footer PR 状态恢复,见 §6.2;当前 EventDone/EventError 不通过 Send 触发 transition,实际也是 dead state,见 §5.2)
//     chatID / userMsgID / replyMsgID / cardMsgID / bot / logger / tasks / mu
```

**删除的代码**：
- `buildReceiptCard` 中 header / entries / footer / hr sections (~250 行)
- `renderLocked` 中 entries PATCH / init / usage 部分 (~80 行)
- `Append` (~70 行) — 内部仍保留 `case EventDone` / `case EventError` / `case EventInit` / `case EventUsage` 四个 case，但**这些 case 在 F-44 后都成为 unreachable**（详见 §5.2）
- `SetExecuting` (~20 行) — 无 production caller
- `SetCompleted` (~40 行) — 无 production caller
- `appendEntryLocked` / `lastEntryLocked` / `EntryCount` / `evictOverflowLocked` (~55 行) — Append 私有 / 唯一外部 caller 已删
- `MessageReceipt.entries` / `agentName` / `workspace` / `branch` / `inputTokens` / `outputTokens` 字段（6 个）+ setter (~50 行)
- `promptHeaderLine` / `footerLine` 计算 (~50 行)
- **receipt.go 段小计: ~615 行**

#### **`internal/channel/feishu/receipt_event.go`**

**整个文件删除**。

理由：F-44 后 `MessageReceipt.Append` 整体删除，`Append` 是 `eventToEntry` 在 production 的唯一 caller（`ensureReceiptForReply` 也调它但同步删除）。`eventToEntry` 9 个 case（`EventText` / `EventToolStart` / `EventToolEnd` / `EventError` / `EventPermission` / `EventDone` / `EventUsage` / `EventCompaction` / `EventInit`）全部 unreachable：

| eventToEntry case | Append 前是否有用 | F-44 后状态 |
|---|---|---|
| `EventText` | OutReply case 触发，append 💬 entry | Append 删除 → unreachable |
| `EventToolStart` / `EventToolEnd` | F-34 后已不返回 entry（(_, false)） | Append 删除 → unreachable |
| `EventError` | OutReply case 不传 EventError；EventError 不通过 Send | unreachable（Append 即使保留，OutReply 也不会传 EventError） |
| `EventPermission` | F-34 后已不返回 entry | Append 删除 → unreachable |
| `EventDone` | EventDone 不通过 Send | unreachable |
| `EventUsage` | OutUsage case 触发，但 F-44 改 silent drop | Append 删除 → unreachable |
| `EventCompaction` | F-34 后已不返回 entry；OutCompaction 也不调 Append | Append 删除 → unreachable |
| `EventInit` | OutInit case 触发，但 F-44 改 silent drop | Append 删除 → unreachable |

> **注**：`eventToEntry(EventError)` 即使在 F-44 前也是 dead code path — `EventError` 不通过 `OutboundMessage` 走 Send（它走 `ChatSession.emitMessageStateForCurrentTurn(MessageFailed)` → `OutMessageState` → AddReaction）。F-44 之前唯一可能调用 `Append(EventError)` 的路径已被 EventError 的 wire 路由切断；F-44 让这层 dead code 暴露并删除。

**连带删除**：
- `LogEntry` struct（仅 `eventToEntry` / `appendEntryLocked` 用，0 caller）
- `formatUsageText`（仅 `eventToEntry(EventUsage)` / test 用，0 caller）
- `truncateForLog`（原始 helper；但 `receipt_event_test.go` 仍有测试，且 `internal/bridge/claudecode/stream.go:584` 有同名 duplicate。F-44 后 `truncateForLog` 留在 `result_render.go` 作为唯一实现；claudecode duplicate 是另一包，**不动**）
- `thinkingPrefix` 常量（仅 `eventToEntry(EventText)` 用，0 caller）
- **receipt_event.go 段小计: ~210 行**（整个文件）

**连带删除测试文件**：
- `receipt_event_test.go` ~ 全文件删除（覆盖 `eventToEntry` / `truncateForLog` / `formatUsageText`，这些都删了）

#### **`internal/channel/feishu/card_sanitize.go`**

整个文件**合并进 `result_render.go`** 作为私有函数。当前 OutReply + OutResult 都用 `SanitizeCardMarkdown`，F-44 后只剩 OutReply + OutResult 两个 caller（都是 `buildResultPayload` 路径），不需要独立文件。

**删除的代码**：
- `card_sanitize.go` 整个文件 (~200 行)
- 移到 `result_render.go` 私有函数 + 移除独立 export

#### **`internal/channel/feishu/receipt_task.go`**

**不变**。`buildTaskChecklistChunks` / `renderTaskLine` / `renderTaskFallbackText` 继续为 task receipt 服务。`renderTaskFallbackText` **保留** — 作为 `ensureReceiptForTask` SendCard 失败时的降级路径（F-44 不动 task receipt，失败路径仍可用）。

#### **`internal/gateway/messages.go`**

**不变**。`OutboundMessage` 字段（`Init` / `Usage` typed field）保持；F-44 仅在 Channel 层 silent drop，wire format 不变。

#### **`internal/gateway/translate.go`**

**不变**。`EventInit` / `EventUsage` 仍翻译为 `OutboundMessage{Init: ...}` / `{Usage: ...}`；footer PR 会用。

### 2.2 保留不变的（确认无副作用）

- **`OutboundMessage` 全字段** — `Init` / `Usage` typed field 保留（footer PR 用）
- **Gateway / ChatSession / Bridge / Registry** — 全部不变
- **EventHandler** (`cmd/nightme/run.go::newEventHandler`) — 不变；F-44 在 Channel 层完成
- **`OutResult` 路径** (F-39 `sendResultAsReply`) — 不变
- **`OutThinking` 路径** (F-think `postThreadMarkdownReply`) — 不变
- **`OutToolStart` / `OutToolEnd` 路径** (F-38 `tool_thread_merge.go`) — 不变
- **`OutCompaction` 路径** (`postThreadReply`) — 不变
- **`OutCard` 路径** (`buildInteractiveCard`) — 不变 — 注意此路径也调用 `SanitizeCardMarkdown`，所以 `SanitizeCardMarkdown` 必须保留为 exported
- **`OutMessageState` 路径** (F-31 reactions) — 不变
- **`OutCommandReply` 路径** (`SendMessageText`) — 不变
- **Task receipt 路径** (`ensureReceiptForTask` / `SetTaskList`) — 不变
- **`splitMarkdownForDivs` / `buildResultPayload` / `SanitizeCardMarkdown` / `truncateForLog`** — 内部 helper 全部复用（F-44 把 `SanitizeCardMarkdown` / `truncateForLog` 从 `card_sanitize.go` / `receipt_event.go` 搬到 `result_render.go` 集中管理）
- **`MessageReceipt.promptState` 字段** — 保留（footer PR 可恢复 receipt prompt state header，见 §6.2；当前 EventDone/EventError 不通过 Send 触发 transition，state 字段暂时不写入也不读取，无副作用）

### 2.3 Send case 改动汇总

| Case | F-44 前 | F-44 后 | 副作用 |
|---|---|---|---|
| `OutReply` | ensureReceipt + overflow check + fold / bail-out (~80 行) | `sendReplyInThreadAndChat` (~10 行) | `receipt.Append` 调用消失；无 LogEntry 写入 |
| `OutResult` | `sendResultAsReply` | 不变 | 无 |
| `OutTaskCreate` / `OutTaskUpdate` | `ensureReceiptForTask` + fallback | 不变 | 无 |
| `OutInit` / `OutUsage` | `receipt.Append(EventInit/Usage)` 写 `agentName/workspace/branch/tokens` 字段 | `return nil` silent drop | `agentName/workspace/branch/tokens` 字段变 orphan → 整体删除 |
| `OutMessageState` / 其他 | — | 不变 | 无 |

---

## 3. 测试覆盖

### 3.1 单元测试

| 文件 | 用例 | 断言 |
|---|---|---|
| `internal/channel/feishu/adapter_test.go` | `TestSend_OutReply_IndependentReply` | mock sendContent；`Send(OutReply{Text: "hello"})` → `sendContent` called **1 次** with `replyInThread=false`（ReplyInThreadAndChat）；**不**调 `receipt.Append` |
| 同上 | `TestSend_OutReply_EmptyText_SilentDrop` | `OutReply{Text: "  "}` → 静默 drop,无 sendContent 调用 |
| 同上 | `TestSend_OutReply_NoReceiptRequired` | `receiptFor` 返回 nil 也不影响 OutReply 投递（不需要 receipt） |
| 同上 | `TestSend_OutReply_LongText_EnvelopeDefense` | 15000 runes text → 触发 `truncateRunes` 到 envelope budget |
| 同上 | `TestSend_OutReply_NoIconPrefix` | 投递 body **不带** `💬` 前缀（OutReply 是流延续） |
| 同上 | `TestSend_OutReply_3SegmentDispatch_NoMarkdown` | plain text → `MsgTypeText` |
| 同上 | `TestSend_OutReply_3SegmentDispatch_LotsTables` | 6 markdown tables → `MsgTypePost + tag:"md"` |
| 同上 | `TestSend_OutReply_3SegmentDispatch_Default` | markdown ≤ 5 tables → `MsgTypeInteractive` |
| 同上 | `TestSend_OutReply_SanitizeApplied` | text 含 `[x](relative)` → sanitize 后 plain text |
| 同上 | `TestSend_OutResult_Unchanged` | F-39 路径回归测试,确保 OutResult 不受 F-44 影响 |
| 同上 | `TestSend_OutInit_SilentDrop` | `OutInit` → 无 sendContent / receipt.Append 调用 |
| 同上 | `TestSend_OutUsage_SilentDrop` | 同上 |
| `internal/channel/feishu/receipt_test.go` | `TestBuildReceiptCard_TaskOnly` | receipt 含 task → card body 只含 `**📋 Tasks**` markdown elements;**不**含 header / entries / footer / hr |
| 同上 | `TestBuildReceiptCard_NoTask_EmptyCard` | receipt 无 task → card body elements 为空（SendCard 仍可发,Feishu 接受空 elements） |
| 同上 | `TestBuildReceiptCard_TaskSnapshotReplace` | 两次 `SetTaskList` 不同 snapshot → 第二次 PATCH 替换整段 checklist |
| 同上 | `TestRenderLocked_TaskReceiptUpdates` | task snapshot 变化触发 PATCH;无 task → 无 PATCH |
| `internal/channel/feishu/receipt_event_test.go` | **整个文件删除** | 覆盖 `eventToEntry` / `truncateForLog` / `formatUsageText` — 这些函数全部删除 |
| `internal/channel/feishu/receipt_test.go`(扩) | `TestAppend_Deleted` | 删除测试:compile-time + grep 确认 `MessageReceipt.Append` 不存在 |
| 同上 | `TestSetCompleted_SetExecuting_Deleted` | 删除测试:确认 `SetCompleted` / `SetExecuting` 不存在 |
| 同上 | `TestLogEntry_Deleted` | 删除测试:确认 `LogEntry` struct 不存在 |

### 3.2 集成测试（回归）

| 文件 | 用例 | 断言 |
|---|---|---|
| `internal/channel/feishu/adapter_test.go` | `TestFullTurn_OutReplyIndependent_TaskReceipt` | 完整 turn：5 个 OutReply chunk + 2 个 OutTaskCreate + 1 个 OutResult。验证：5 个独立 reply 投递（按顺序）+ 1 张 task receipt（PATCH 2 次）+ 1 个独立 OutResult。`receiptFor` 调用次数 = 0（OutReply 不查 receipt） |
| 同上 | `TestFullTurn_OnlyReply_NoReceipt` | 完整 turn：5 个 OutReply chunk + 1 个 OutResult。验证：5 个独立 reply + 1 个 OutResult;**不创建** receipt card;`MessageReceipt` 实例计数 = 0 |
| 同上 | `TestFullTurn_OnlyTask_NoReply` | 完整 turn：3 个 OutTaskUpdate + 1 个 OutResult。验证：1 张 task receipt + 1 个 OutResult |
| 同上 | `TestFullTurn_OutUsage_Init_Dropped` | OutInit + OutUsage 到达 → 静默 drop,无 sendContent / receipt 操作 |

### 3.3 grep / 回归（收尾）

```bash
# 验证 ensureReceiptForReply / isOverflowingReceipt / sendReplyAsMessage 完全消失
rg -n "ensureReceiptForReply|isOverflowingReceipt|sendReplyAsMessage" --type=go  # 期望: 0 命中

# 验证 Append / SetExecuting / SetCompleted 整条 dead chain 消失
rg -n "func.*MessageReceipt.*(Append|SetExecuting|SetCompleted|appendEntryLocked|lastEntryLocked|EntryCount|evictOverflowLocked)" --type=go  # 期望: 0 命中
rg -n "type LogEntry struct" --type=go  # 期望: 0 命中
rg -n "func eventToEntry|func formatUsageText" --type=go  # 期望: 0 命中
ls internal/channel/feishu/receipt_event.go 2>&1  # 期望: No such file
ls internal/channel/feishu/receipt_event_test.go 2>&1  # 期望: No such file

# 验证 buildReceiptCard 不再引用 entries / agentName / workspace / branch / inputTokens / outputTokens
rg -n "r\.entries|r\.agentName|r\.workspace|r\.branch|r\.inputTokens|r\.outputTokens" internal/channel/feishu/receipt.go  # 期望: 0 命中

# 验证 card_sanitize.go 已删除,SanitizeCardMarkdown 移到 result_render.go
ls internal/channel/feishu/card_sanitize.go 2>&1  # 期望: No such file
rg -n "func SanitizeCardMarkdown" --type=go  # 期望: 1 命中 (in result_render.go)

# 验证 OutReply / OutResult / OutTask* 路径独立可测
go test ./internal/channel/feishu/... -run TestSend_OutReply
go test ./internal/channel/feishu/... -run TestSend_OutResult
go test ./internal/channel/feishu/... -run TestSend_OutTask
```

---

## 4. 落地顺序

每步独立 commit，可单独 review + revert：

| Step | 内容 | 文件 | 风险 |
|---|---|---|---|
| 1 | **本文档** (`F-44-outreply-independent-and-task-receipt.md`) | `docs/feat/` | 零 |
| 2 | SPEC.md §0.11 + §2.4 更新 | `docs/SPEC.md` | 零 |
| 3 | channel/feishu.md §12 渲染映射表 + §13.21 + §14 changelog 更新 | `docs/channel/feishu.md` | 零 |
| 4 | FEATURES.md 索引条目 | `docs/FEATURES.md` | 零 |
| 5 | `card_sanitize.go` 合并到 `result_render.go`(`SanitizeCardMarkdown` 仍 exported,因为 `buildInteractiveCard` 也调用) | `internal/channel/feishu/` | 低 |
| 6 | `receipt_event.go` 整个文件删除 | `internal/channel/feishu/receipt_event.go` | 中（连带删 `LogEntry` / `eventToEntry` / `formatUsageText` / `thinkingPrefix`） |
| 7 | `receipt_event_test.go` 整个文件删除 | `internal/channel/feishu/receipt_event_test.go` | 低 |
| 8 | `receipt.go` 删 `Append` / `SetExecuting` / `SetCompleted` / `appendEntryLocked` / `lastEntryLocked` / `EntryCount` / `evictOverflowLocked` + `entries` / `agentName` / `workspace` / `branch` / `inputTokens` / `outputTokens` 字段 + 简化 `buildReceiptCard` + `renderLocked` | `internal/channel/feishu/receipt.go` | 中-高 |
| 9 | `adapter.go` 删 `ensureReceiptForReply` / `isOverflowingReceipt` / `sendReplyAsMessage` | `internal/channel/feishu/adapter.go` | 中 |
| 10 | `adapter.go` 加 `sendReplyInThreadAndChat` helper + 新增 `perReplyMaxBytes` 常量 | `internal/channel/feishu/adapter.go` | 中 |
| 11 | `adapter.go::Send` case `OutReply` 重写 + case `OutInit`/`OutUsage` silent drop | `internal/channel/feishu/adapter.go` | 中 |
| 12 | `adapter_test.go` 新增 OutReply 独立 reply 测试 + 删 OutReply fold / overflow 测试 | `internal/channel/feishu/adapter_test.go` | 低 |
| 13 | `receipt_test.go` 新增 Task-only 测试 + 简化既有测试 + 加 dead code 删除测试（`Append` / `SetCompleted` / `LogEntry` 不存在） | `internal/channel/feishu/receipt_test.go` | 低 |
| 14 | 全量 `go test ./...` + `go vet` + `golangci-lint` | — | 必过 |

---

## 5. 与上下游契约

### 5.1 OutboundMessage 契约

**不变**。`Init` / `Usage` typed field 保留（footer PR 会用）；`ReplyTo` 不变；`Kind` enum 不变；`Text` / `Result` / `TaskList` 等不变。F-44 是 Channel 内部路由调整，wire format 完全保持。

### 5.2 ChatSession 状态机 + EventDone/EventError 流

**ChatSession 状态机不变**。`currentTurnUserMsgID` / `InputBuffer` / `EventCallback` 全部不变。F-44 不读 ChatSession 任何新字段。

**`EventDone` / `EventError` 流说明（重要，跟当前实现的 dead code 路径对齐）**：

- `agent.EventDone` / `agent.EventError` **不通过** `OutboundMessage` 路径走 `Adapter.Send`
- 它们通过 `ChatSession.emitMessageStateForCurrentTurn(MessageDone / MessageFailed)` → `Gateway.OnMessageState` → `OutboundMessage{Kind: OutMessageState, MessageState: {State, MessageID}}` → `Adapter.Send(OutMessageState)` → Feishu `AddReaction`
- 这意味着 `MessageReceipt.Append` 内部的 `case agent.EventDone` / `case agent.EventError` 分支在当前 production 上**已经是 unreachable** — `Append` 唯一 production caller (`Adapter.Send(OutReply)`) 只传 `EventText`，不传 `EventDone/Error`
- F-44 把 `Append` 整体删除即可，不需要单独迁移 EventDone/Error 处理逻辑
- 终态信号完全靠 `OutMessageState` → Feishu `AddReaction(userMsgID, ✅/❌)` 表达，跟 receipt 状态机彻底解耦（v1.3 不变式：state 是 Channel 内部状态，缺失不影响其他 Kinds 渲染）

**`promptState` 字段保留**:`completedAt` 实际已删(没有读者 — `promptHeaderLine` 函数整体删除)。理论上 footer PR 可以让 receipt prompt state header 复活（§6.2）— 届时 `promptState` 字段的 `PromptPending / PromptRunning / PromptSucceeded / PromptFailed` 转换由新增的 renderLocked 触发器驱动(`completedAt` 字段会同时加回以承载终态时间戳)。当前 EventDone/EventError 不通过 Send 触发 transition,字段暂时不写入也不读取,无副作用。

### 5.3 F-31 MessageState（reaction lifecycle）

**不变**。`MessageState` 4 态 → AddReaction 路径完全保留；F-44 不影响 user msg reaction 渲染。

### 5.4 F-37 thread routing（OutThinking / OutTool* / OutCompaction）

**不变**。三类 Kind 仍走 `postThreadReply` / `postThreadMarkdownReply`（thread reply，不是 main chat）。F-44 不触及。

### 5.5 F-38 task checklist

**保留并简化**。Task snapshot 替换式更新逻辑不变；receipt card 只剩 task section，删除 header / entries / footer sections。`ensureReceiptForTask` / `SetTaskList` / `buildTaskChecklistChunks` 不动。

### 5.6 F-39 OutResult 独立 reply

**不变**。`sendResultAsReply` 三段 dispatch 仍服务 OutResult。F-44 新增 `sendReplyInThreadAndChat` 是 sibling helper，共享 `buildResultPayload` / `SanitizeCardMarkdown` / `splitMarkdownForDivs` / `truncateRunes`。

### 5.7 F-40 OutReply 改名 + 超限改独立 reply

**反转（部分）**：
- ✅ `OutText` → `OutReply` 改名**保留**（语义正确）
- ✅ 删 600B truncate **保留**
- ✅ 删 `eventToEntry(EventText)` case **保留**（F-44 进一步把整个 `eventToEntry` 函数连带文件删除）
- ✅ 删 `MessageReceipt.entries` 字段 `LogEntry` 路径 **保留**（F-44 进一步把 `LogEntry` struct 连带整个 `receipt_event.go` 文件删除）
- ❌ Overflow bail-out（长度 / 数量）→ **删除**（OutReply 不再 fold，无 overflow 概念）
- ❌ 迟到 reply bail-out → **删除**（OutReply 独立后不再有"迟到"语义）
- ❌ `sendReplyAsMessage` helper → **删除**（被 `sendReplyInThreadAndChat` 替代）
- ❌ `MessageReceipt.Append` 整体 → **删除**（OutReply / OutInit / OutUsage 三个 caller 全消失；EventDone/Error case 本就是 dead code，详见 §5.2）
- ❌ `MessageReceipt.SetExecuting` / `SetCompleted` → **删除**（无 production caller）

### 5.8 F-42 lazy receipt creation

**简化**：
- ✅ Receipt lazy create（不在 cold-start 空 receipt）**保留**（OutTask* 触发）
- ✅ MessageState ⏳/🔄 silent drop 留 ✅/❌ **保留**（不受 F-44 影响）
- ✅ TaskList `**📋 Tasks**` title **保留**
- ❌ `ensureReceiptForReply` → **删除**（OutReply 不再触发 receipt 创建）
- ❌ Cold-start fallback text for reply → **删除**

### 5.9 §1.4 抽象 / 具体 边界

**保留**。F-44 不引入新 typed field，不修改 `OutboundMessage` wire format；所有变化在 Channel 内部渲染目标分流范畴内。

---

## 6. 后续工作（本文档不做 — 推迟到 footer PR）

### 6.1 OutInit / OutUsage footer 渲染

- 新增 `SessionMeta *SessionMeta` typed field 到 `OutboundMessage`（或扩展 `Init` 字段）
- ChatSession 持有 `SnapshotInit()` + `LiveBranch()` + `LiveCwd()` + `InvalidateBranchCache()` API
- EventHandler 在每次 emit 时戳印 `SessionMeta`
- Channel 在 `sendReplyInThreadAndChat` / `sendResultAsReply` / `ensureReceiptForTask` 内部读 `msg.SessionMeta` 渲染 footer

### 6.2 Task receipt header 恢复（可选）

- 如果 footer PR 决定给 task receipt 恢复 prompt state header（⏳/🔄/✅/❌），加 `promptHeaderLine` 回 `buildReceiptCard`
- F-44 不预设

### 6.3 Receipt "OutReply history" 折叠（可选 / 长期）

- 如果用户觉得"多 reply 流"过长，可以加 per-turn "展开历史"折叠按钮
- 需要 Web / Slack 等其他 Channel 适配
- F-44 不预设

---

## 7. 不变式总结（本文档特殊要求）

**F-44 改 OutReply fold → 独立 reply + 简化 task receipt + silent drop OutInit/OutUsage，但保留：**

- `OutboundMessage` 全字段不变（`Init` / `Usage` typed field 保留，footer PR 用）
- Gateway 不变（`Translate` 仍产 OutboundMessage）
- ChatSession 不变（`currentTurnUserMsgID` 单数锚点 + `InputBuffer` 不变）
- `OutboundMessage.ReplyTo = cs.currentTurnUserMsgID` 不变（独立 reply 也锚同 userMsgID）
- 1 turn : 1 anchor 不变式保留
- §1.4 抽象 / 具体 边界规范保留（Init/Usage 是 typed primitive，Channel 自决渲染目标）
- 抽象归抽象 / 具体归具体原则保留（独立 reply 是 Feishu 自治决策）
- F-25 rolling-log UX **部分保留**：task receipt 仍是 rolling-log；OutReply 不再 rolling-log（改为独立 reply 流）
- F-31 MessageState 抽象契约不变
- F-37 thread routing 决策不变（thinking/tool/compaction 仍 thread reply）
- F-38 task checklist 决策不变（task snapshot 仍是 receipt 单一 section）
- F-39 OutResult 决策不变（OutResult 仍独立 reply）
- F-40 OutReply 命名 + 删 600B truncate 决策保留；overflow / late-reply bail-out 删除（不再需要）
- F-42 lazy receipt creation 决策保留；仅 task receipt 路径触发，reply 路径删除

**为什么不叫 v2.0**：v1.3 核心不变式（职责隔离、Binding FSM owner、Receipt 自治、抽象归抽象 / 具体归具体）全部保留。F-44 是 Channel 自治范围内的渲染路径简化（OutReply 拆出 receipt + task receipt 瘦身 + OutInit/OutUsage 推迟），不影响 nightme 数据模型与 Gateway 契约。

---

## 8. 变更日志

| 日期 | 版本 | 变更 |
|---|---|---|
| 2026-08-05 | 草案 | 初稿；F-44 设计讨论落地 |
| 2026-08-05 | wire 细化 | 删 `sendViaLarkReply`：dispatch 收口到 `sendViaLark` 一处，按 PR #47 三型 taxonomy 路由——`rootID == ""` → `ReplyInChat`（顶级 Create）/ `replyInThread=true` → `ReplyInThread` / `replyInThread=false` → `ReplyInBoth`。`OutReply` / `OutResult` / `OutTaskCreate` / `OutTaskUpdate` / `OutCard` / `OutCommandReply` 全部走 `ReplyInBoth`；`OutThinking` / `OutToolStart` / `OutToolEnd` 走 `ReplyInThread`。`isFeishuTerminalMessageCode` 增加 `code=NNNNN` 格式匹配（PR #47 `ReplyInBoth` 错误格式）。新增测试 `TestSendViaLark_ReplyInBoth_Dispatch` / `TestSendViaLark_ReplyInThread_Dispatch` 锁定 dispatch 表 |