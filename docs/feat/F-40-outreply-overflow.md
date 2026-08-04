# F-40: OutReply 超限改独立 Reply + OutText → OutReply 改名

> **Status**: design (v1.3.x, 2026-08-04)
> **Scope**: `internal/channel/feishu/{adapter,receipt_event,receipt}.go` + `internal/gateway/messages.go` + `internal/gateway/translate.go` + `cmd/nightme/run.go`
> **目的**: 消除 `OutReply → receipt.Append` 路径上的 600 字节截断丢字;当单条 reply 文本超过 receipt 长度 / 数量任一上限时,**不再 truncate**,改为 `ReplyInThreadAndChat` 独立 reply 投递(与 F-39 OutResult 同模式)。同步把 `gateway.OutboundKind.OutText` 改名 `OutReply`,语义更准确——流式 chunk 是 agent **对** user 当前 turn 的 reply 主体,不是泛指"text"
> **Related docs**:
> - [docs/feat/F-25-rolling-log.md](./F-25-rolling-log.md) — receipt 整体 UX(F-40 后 OutReply 仍 fold 但不截断)
> - [docs/feat/F-37-multi-div-content-split.md](./F-37-multi-div-content-split.md) — `splitMarkdownForDivs` 复用,F-40 后也服务于 receipt 内 OutReply 多 div
> - [docs/feat/F-37-tool-thread-routing.md](./F-37-tool-thread-routing.md) — thinking/tool/compaction 在 thread;F-40 加 OutReply 超限也独立
> - [docs/feat/F-39-result-as-new-reply.md](./F-39-result-as-new-reply.md) — F-40 的直接模板,平行案例
> - [docs/feat/F-08-channel-abstraction.md](./F-08-channel-abstraction.md) — Channel 自治范围
> - [docs/SPEC.md §1.4](../SPEC.md) — 抽象 / 具体 边界规范(`OutReply` 字段不变,Channel 自决渲染目标)
> - [docs/channel/feishu.md §13](../channel/feishu.md) — F-40 decision record + 渲染映射表更新

---

## 0. 背景

### 0.1 现状(被改)

v1.3.x F-37 / F-38 / F-39 之后,receipt card 是 rolling-log events 容器:

```
User msg → Receipt Card (单张,反复 PATCH)
   ├ ⏳ / 🔄 / ✅  header (state 变化)
   ├ 💬 💬 💬    OutText 流式 chunks (各 ≤ 600 字节)
   ├ ✅ done / ❌  EventDone / EventError 状态转换
   ├ task checklist (F-38)
   └ <hr> + footer (agent · cwd · tokens)

User msg → Final Result Reply (F-39 独立 reply, 锚同 userMsgID)
   └ 📝 完整 OutResult text
```

`OutText` 是 agent **对** user 当前 turn 的 reply 主体(流式 chunks),由 `cmd/nightme/run.go::responder.Send` 在每次 `EventText` 时投递。但它名义叫"Text"——`text` 在 OutboundKind 体系里是最弱泛化的名字,跟 F-38 加 `OutTaskCreate / OutTaskUpdate`、F-39 加 OutResult 独立 reply 后,体系需要更准确的命名:

- `OutText` = agent 对当前 turn 的 reply chunks → **`OutReply`** 更准确
- `OutResult` = 最终完整 reply(F-39 独立)
- `OutUsage` / `OutInit` / `OutMessageState` / `OutCommandReply` 等都有专门名字

### 0.2 600 字节截断 — 这次的丢字 bug

`eventToEntry(EventText)` 当前对所有流式 chunk 走 `truncateForLog(text, perEntryMaxBytes=600)`:

```go
// internal/channel/feishu/receipt_event.go:52-57 (现状)
case agent.EventText:
    text := strings.TrimSpace(ae.Text)
    if text == "" { return LogEntry{}, false }
    if strings.HasPrefix(text, thinkingPrefix) { return LogEntry{}, false }
    return LogEntry{
        Time: now,
        Icon: "💬",
        Text: truncateForLog(text, perEntryMaxBytes),  // ← 600 字节截断
        Kind: "reply",
    }, true
```

`perEntryMaxBytes = 600`。Claude Code stream-json 单 chunk 常见 800-2000 字节(代码示例、文档引用、Markdown 表格行等),落到 receipt 端:

- 800-2000 字节 → "前 600 字 + …"
- 多个连续 chunks → 全部被截,用户看到"N 条碎裂的'前 600 字 + …' 💬 行"

**用户实际行为**:长 reply 场景下看不到完整内容。F-39 修了 OutResult 路径(最终答复独立 reply,无 600 cap),但 **OutReply 流式 chunk 仍在 receipt 内被截**——同一 turn 里 OutResult 完整、OutReply 流式碎裂,UX 不一致。

### 0.3 真正的问题不是 element / envelope

跟 F-39 §0.3 同样的诊断:

| 来源 | 元素 |
|---|---|
| header (state) | 1 |
| OutReply chunk N (各 ≤ 2000 char) | N(单 div,1000 char 内不需拆;1000-2000 拆 2 div) |
| task checklist (F-38) | 0-3 |
| `<hr>` + footer | 2 |

**典型 5-15 元素,50 元素上限几乎从不撞。** envelope 30 KB 也几乎从不撞。**真正的"截断"是 `eventToEntry` 的 600 字节硬截,不是 element / envelope。**

### 0.4 候选方案

| 方案 | 描述 | 改动 |
|---|---|---|
| (a) **删 `truncateForLog(text, 600)`,允许多 div 进 receipt** | receipt 内 OutReply 走 `splitMarkdownForDivs` 拆多 div,无截断;超 `perEntryMaxRunes=8000` 才外溢 | eventToEntry + buildReceiptCard |
| (b) **删 600 截断 + 超限改独立 reply(本 feature)** | (a) + 单条 OutReply > 8000 runes 或 receipt 已 45 entries 时,改 `ReplyInThreadAndChat` 投递 | adapter.go + 新 helper `sendReplyAsMessage` + (a) 的全部 |
| (c) 维持 600 截断,只放宽到 2000 | 单 entry 字节 cap 抬高 | 1 行 |
| (d) 维持现状,继续修截断边界 | 只动 receipt_event.go | 中等 |

**选择 (b)** 因为:

1. **彻底消除 OutReply 路径上的所有截断**——治本,不是抬 cap 数字
2. **架构上与 F-39 OutResult 平行**——两条独立 reply surface(receipt 内的 chunk + 超限后的独立 reply),与"streaming card for progress + complete reply for deliverable"模式对齐
3. **打开 envelope 真撞墙的降级路径**——独立 helper 可以做 30 KB hard cap + fallback,无需保护 receipt 内其他 entry
4. **数量超限(45 entries)也得有出路**——旧路径只删旧 entries,长 reply 触发 FIFO 驱逐,用户可能看到"前 5 条消失,新一条进来",语义模糊。新路径直接"超 45 → 走独立 reply",语义清晰
5. **顺手改 `OutText` → `OutReply` 命名**——一次 PR 解决"命名不准 + 内容丢字"两个问题

### 0.5 不可变约束

- **`OutboundMessage` 契约字段不变**:`Kind: OutReply` 替换 `OutText`(语义更准,wire 不变);`Text string` 字段不动;`ReplyTo` 不动;§1.4 边界规范保留
- **`EventText` 不动**——bridge 层 `claudecode/stream.go` 仍产 `EventText`(无前缀),由 adapter 决定 fold / 独立 reply
- **Gateway 翻译路径不动**:`gateway/translate.go::Translate(EventText)` 仍产 `OutboundMessage{Kind: OutReply, Text}`(仅 Kind 改名)
- **ChatSession 不动**:per-turn / per-chat 状态机无变化
- **抽象归抽象**:Channel 自治范围内决定渲染目标(receipt 内 fold / 独立 reply 是 Channel 决策,不影响抽象层)
- **`OutboundMessage.ReplyTo` 不动**:`currentTurnUserMsgID` 仍作为锚点,独立 reply 也锚到同 userMsgID
- **`OutResult` 路径(F-39)不动** — F-39 决策依然成立,OutResult 不进 receipt 是独立决策

---

## 1. 设计

### 1.1 视觉对比

**改前**(以一条 1500 char OutReply chunk 为例):

```
user_msg om_A
  └ Receipt Card ⤓ (锚定 om_A; visible in main chat)
      ⏳ 处理中
      💬 前 600 字 + …
      <hr>
      footer
```

用户看到 600 截断的"半截回答"——`1500 char` 内容里后 900 字 + markdown 后半段全丢。

**改后**(同 1500 char 例子):

```
user_msg om_A
  ├ Receipt Card ⤓ (rolling log, 锚定 om_A)
  │   ⏳ → 🔄 处理中
  │   💬 完整 1500 char (拆 2 个 div: ≤ 1000 + ≤ 500)
  │   <hr> + footer
```

正常 fold 路径:`eventToEntry` 不再截断,`buildReceiptCard` 用 `splitMarkdownForDivs` 把单条 OutReply 拆多 div,完整内容进 receipt。

**改后**(极端 case:OutReply 12000 runes > perEntryMaxRunes 8000):

```
user_msg om_A
  ├ Receipt Card ⤓ (rolling log, 锚定 om_A)
  │   ⏳ → 🔄 处理中
  │   (无 OutReply entry)
  │   <hr> + footer
  └ OutReply Reply ⤓ (锚定 om_A; 独立 reply,完整 12000 char markdown)
      💬 完整 12000 char(走 3 段 dispatch:sanitize + multi-div)
```

超长 OutReply 直接走独立 reply,跟 F-39 OutResult 同 surface(receipt card + 独立 reply 锚同 userMsgID),但**不带 icon 前缀**——它是 reply 流的延续,不是新条目。

**改后**(另一极端 case:receipt 已有 45 entries,新 OutReply 到达):

```
user_msg om_A
  ├ Receipt Card ⤓ (rolling log, 锚定 om_A; 45 entries 已满)
  │   ⏳ → 🔄 处理中
  │   💬 × 45 entries
  │   <hr> + footer
  └ OutReply Reply ⤓ (锚定 om_A; 独立 reply)
      💬 完整新 OutReply text
```

数量超限语义:不再 FIFO 驱逐旧 entries(F-39 之前做法),新 OutReply 走独立 reply。receipt 是"事件流摘要",独立 reply 是"超量后续",语义清晰。

### 1.2 超限判定(`isOverflowingReceipt`)

```go
// isOverflowingReceipt 在 receipt.mu 持有时调,决定 OutReply 是 fold 还是外溢。
func isOverflowingReceipt(r *MessageReceipt, text string) bool {
    // 长度:单条 OutReply 超过 entry rune 上限(8000 runes)
    if utf8.RuneCountInString(text) > perEntryMaxRunes {
        return true
    }
    // 数量:receipt 已有 entries 数达到上限(45)
    if len(r.entries) >= replyMaxEntries {
        return true
    }
    return false
}
```

阈值复用现有常量:
- `perEntryMaxRunes = 8000`(F-39 后给 result 用的;F-40 也适用于 OutReply 上限)
- `replyMaxEntries = 45`(F-25 既有)

**不新增常量**——保持常量表精简,与 F-39 result 共享"8000 runes 是 receipt 内单 entry 实际容量"的认知。

### 1.3 Dispatch(超限后走 `sendReplyAsMessage`)

平行 `sendResultAsReply` (F-39),3 段 dispatch:

```
             ┌─ has no markdown indicators
             │   → MsgTypeText (plain text bubble, Feishu 渲染 <at> + 4-style)
             │
sanitize     ├─ markdown 且 tables > 5
  ↓          │   → MsgTypePost + tag:"md" (GFM 全套,无 Card 2.0 表格硬限)
content      │
             └─ markdown 且 tables ≤ 5
                 → MsgTypeInteractive (Card 2.0)
                   elements: [
                     {tag:"markdown", content: chunk 1},  ← F-37 splitMarkdownForDivs
                     {tag:"markdown", content: chunk 2},  ← 1000 runes/div hard cap
                     ...,
                   ]
                   sanitized via SanitizeCardMarkdown
```

复用 F-39 的 `SanitizeCardMarkdown` + `splitMarkdownForDivs` + `buildResultPayload` + `truncateRunes`。**不新增 helper**——只有 adapter 入口 + 路由分流是 F-40 新增。

### 1.4 Wire 形态:`ReplyInThreadAndChat`

| 形态 | Feishu wire | main chat | thread |
|---|---|---|---|
| **`ReplyInThreadAndChat`**(本 feature 默认)| `POST /messages/{rootID}/reply`,`reply_in_thread` 字段**省略** | **正文内联 + thread 入口** | 同一份正文 |
| `ReplyInThread`(OutThinking / OutToolStart / OutToolEnd)| `reply_in_thread:true` | "X replies" 灰条 | 正文 |

F-40 的 `sendReplyAsMessage` 走 `sendContent(chatID, msgType, body, userMsgID, replyInThread=false)`——`replyInThread=false` = `ReplyInThreadAndChat`(字段省略 = main chat 可见正文 + thread 入口)。与 F-39 OutResult 同形态(replyInThread=false,锚同 userMsgID)。

### 1.5 状态机变化

`MessageReceipt` 状态机不变(Waiting → Executing → Completed → Error)。**两点触发点变化:**

1. **`Adapter.Send(OutReply)` 入口**:不直接调 `receipt.Append`,先看 receipt 状态:
   - `receipt == nil` → fail-safe 走 `sendRawOutText` (top-level plain text bubble)
   - `receipt != nil && r.State() == StateCompleted` → 迟到 OutReply,直接走 `sendReplyAsMessage`(不静默丢)
   - `receipt != nil && isOverflowingReceipt(r, text)` → 超限,走 `sendReplyAsMessage`
   - 否则 fold → `receipt.Append(EventText{Text: text})`(不截断)

2. **`receipt.Append(EventText)` 内部**:不再 `truncateForLog(text, 600)`;`eventToEntry(EventText)` 输出 `LogEntry{Text: full text, Kind:"reply"}`;`buildReceiptCard` 用 `splitMarkdownForDivs(entry.Text, divTextCharLimit=1000)` 拆多 div 进 card body。

### 1.6 Receipt body 预算

不变:

| 限制 | 值 | 来源 |
|---|---|---|
| `replyMaxBytes` | 24 KiB | 飞书 30KB envelope 留 6KB 头 |
| `replyMaxElements` | 50 | 飞书 `body.elements` 硬限 |
| `replyMaxEntries` | 45 | entries 总数(留 5 给 header / hr / footer / checklist × 1-2) |
| `divTextCharLimit` | 1000 runes | 单 `div.text.content` 硬限 |

F-40 删了 600 字节 entry-level 截断,允许单 entry 占多个 div。一个 2500 runes OutReply 进 receipt 占 3 div(1000 + 1000 + 500),仍受 `replyMaxElements=50` 约束——这正是超限改独立 reply 的另一原因(单 entry 占太多 div 会挤掉后续 event / footer 预算)。

---

## 2. 文件 & 接口

### 2.1 改动的文件

**`internal/gateway/messages.go`** —— `OutText` 常量改名为 `OutReply`:

```go
const (
    // OutReply is a streaming reply chunk — the most common case
    // for both final agent replies (multi-chunk) and intermediate
    // status lines (single chunk). F-40 rename from OutText for
    // semantic accuracy: this is the agent's reply to the user's
    // current turn, not a generic "text" payload.
    OutReply OutboundKind = iota
    OutToolStart
    OutToolEnd
    OutThinking
    // ...
)
```

**`internal/gateway/translate.go`** —— `EventText` 翻译路径里 `Kind: OutText` → `Kind: OutReply`:

```go
case agent.EventText:
    // thinkingPrefix 已剥 / 未剥两种 case
    return OutboundMessage{
        Kind:   OutReply,  // ← rename
        Text:   text,
    }, true
```

**`internal/channel/feishu/adapter.go`** —— `Send` case 改名 + 新增 `sendReplyAsMessage` + `isOverflowingReceipt` 判断 + 迟到 OutReply 处理:

```go
// (1) case label rename
case gateway.OutReply:
    text := strings.TrimSpace(msg.Text)
    if text == "" {
        return nil
    }
    
    receipt := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo)
    if receipt == nil {
        return a.sendRawOutText(ctx, msg.ChatID, text)
    }
    
    // 迟到 OutReply:receipt 已 StateCompleted,不再 fold,走独立 reply
    if receipt.IsCompleted() {
        return a.sendReplyAsMessage(ctx, msg.ChatID, msg.ReplyTo, text)
    }
    
    // 超限判断:长度或数量任一触发
    if isOverflowingReceipt(receipt, text) {
        return a.sendReplyAsMessage(ctx, msg.ChatID, msg.ReplyTo, text)
    }
    
    // 正常 fold:不截断,buildReceiptCard 内部 multi-div
    return receipt.Append(ctx, agent.AgentEvent{
        Kind: agent.EventText,
        Text: text,
    })

// (2) 新 helper
func (a *Adapter) sendReplyAsMessage(
    ctx context.Context, chatID, userMsgID, text string,
) error {
    // 镜像 sendResultAsReply (F-39) 的 3 段 dispatch + sanitize + envelope defense
    // 唯一差别:replyInThread=false (默认) = ReplyInThreadAndChat
    // 唯一差别:不加 icon 前缀 (OutReply 是流延续,不是新条目)
}

// (3) 超限判定
func isOverflowingReceipt(r *MessageReceipt, text string) bool {
    if utf8.RuneCountInString(text) > perEntryMaxRunes {
        return true
    }
    r.mu.RLock()  // r.mu 是 sync.RWMutex;调用方已持有 r.mu(Append 持锁路径)
    defer r.mu.RUnlock()
    return len(r.entries) >= replyMaxEntries
}
```

注:`isOverflowingReceipt` 锁语义需要 review——`receipt.Append` 持有 `r.mu.Lock()`,调用 `isOverflowingReceipt` 时 r.mu 已持锁(Write lock),子函数不能用 `RLock`(会死锁)。改成 caller 在调用前已持锁,`isOverflowingReceipt` 不加锁:

```go
func isOverflowingReceipt(r *MessageReceipt, text string) bool {
    // Caller holds r.mu (write lock) — see MessageReceipt.Append.
    if utf8.RuneCountInString(text) > perEntryMaxRunes {
        return true
    }
    return len(r.entries) >= replyMaxEntries
}
```

但 `Adapter.Send` → `receiptFor` 路径不持 `r.mu`(只在 `Append` 内部拿锁)。所以这里需要 `r.entriesSnapshot()` 之类 read-only helper,或 `r.entryCount()`。新增最小 helper:

```go
// 在 MessageReceipt 上新增:
func (r *MessageReceipt) EntryCount() int {
    r.mu.RLock()
    defer r.mu.RUnlock()
    return len(r.entries)
}

func (r *MessageReceipt) IsCompleted() bool {
    r.mu.RLock()
    defer r.mu.RUnlock()
    return r.state == StateCompleted
}
```

**`internal/channel/feishu/receipt_event.go`** —— `eventToEntry(EventText)` 删 `truncateForLog(text, 600)`:

```go
// (1) case agent.EventText:删 truncateForLog
case agent.EventText:
    text := strings.TrimSpace(ae.Text)
    if text == "" {
        return LogEntry{}, false
    }
    if strings.HasPrefix(text, thinkingPrefix) {
        // F-34: thinking no longer folds into the receipt card
        return LogEntry{}, false
    }
    return LogEntry{
        Time: now,
        Icon: "💬",
        Text: text,  // ← 不再 truncate;buildReceiptCard 拆多 div
        Kind: "reply",
    }, true
```

**`internal/channel/feishu/receipt.go`** —— `buildReceiptCard` 改用 `splitMarkdownForDivs` 拆多 div:

```go
// 当前:
for _, e := range r.entries {
    elements = append(elements, map[string]any{
        "tag": "div",
        "text": map[string]any{
            "tag":   "lark_md",
            "content": e.Icon + " " + e.Text,  // 单 div,无 split
        },
    })
}

// 改后:
for _, e := range r.entries {
    body := e.Icon + " " + e.Text
    if e.Icon == "" { body = e.Text }  // usage/compaction/init 不带 icon
    chunks := splitMarkdownForDivs(body, divTextCharLimit)
    for _, c := range chunks {
        elements = append(elements, map[string]any{
            "tag": "div",
            "text": map[string]any{"tag": "lark_md", "content": c},
        })
    }
}
```

`splitMarkdownForDivs` 复用 F-37 helper,长 entry 自动拆多 div,code block / list 块保持 atomic。

**`cmd/nightme/run.go`** —— `responder.Send` Kind 字段值改名:

```go
return r.ch.Send(ctx, gateway.OutboundMessage{
    ChatID:  chatID,
    Kind:    gateway.OutReply,  // ← rename
    Text:    text,
    ReplyTo: userMsgID,
})
```

### 2.2 保留不变的(确认无副作用)

- `OutboundMessage{Text, ReplyTo, ChatID, ...}` —— wire 字段全不变,仅 `Kind` enum 改名
- `gateway/translate.go::Translate(EventText)` —— 路径不变,只改 Kind 字面量
- `chatSession` 状态机 —— 不动
- `MessageReceipt.SetCompleted / Append(EventDone / Error / Init / Usage)` —— 不动
- F-37 `splitMarkdownForDivs` —— 复用,新增 caller(receipt 内 OutReply entry)
- F-38 task checklist —— 不动
- F-thread-route(thinking/tool → thread reply)—— 不动
- F-39 OutResult `sendResultAsReply` —— 不动,F-40 新 `sendReplyAsMessage` 是 sibling helper(共享 3 段 dispatch + sanitize + envelope defense)

### 2.3 Send vs Reply 行为对比

| OutboundKind | F-39 (OutResult) | F-40 (OutReply) |
|---|---|---|
| 进入 receipt card | ❌(完全独立 reply)| ✅(默认 fold)|
| 超限改独立 reply | N/A(OutResult 永远独立)| ✅(长度 / 数量)|
| 3 段 dispatch(text / post+md / card) | ✅ | ✅(复用) |
| Sanitize pipeline | ✅(`SanitizeCardMarkdown`) | ✅(复用)|
| Envelope defense(28 KB) | ✅(`resultCardEnvelopeBudget`) | ✅(复用)|
| 锚 userMsgID | ✅ | ✅ |
| `replyInThread` flag | `false`(ReplyInThreadAndChat) | `false`(ReplyInThreadAndChat)|
| Icon 前缀 | `❌`(error) / 无(success)| 无(始终)|
| 拆多 div | ✅(F-37) | ✅(F-37)|
| 复用 helper | — | 复用 F-39 的 `sendResultAsReply` 内部 helper |

**关键共享**:`SanitizeCardMarkdown` / `splitMarkdownForDivs` / `buildResultPayload` / `truncateRunes` 全部复用,F-40 新增的只有:
- `sendReplyAsMessage`(adapter 顶层 helper,共享内部逻辑)
- `isOverflowingReceipt`(adapter 顶层判定)
- `MessageReceipt.EntryCount() / IsCompleted()`(receipt 公开 read-only helper)
- `buildReceiptCard` 改写(用 splitMarkdownForDivs)
- `eventToEntry(EventText)` 删 600B truncate

---

## 3. 测试覆盖

### 3.1 单元测试

| 文件 | 用例 | 断言 |
|---|---|---|
| `internal/channel/feishu/receipt_event_test.go` | `TestEventToEntry_Text_NoTruncate` | `EventText{Text: 1500 chars}` → entry `Text == 原始 1500 chars`(无 600B truncate) |
| 同上 | `TestEventToEntry_Text_EmptySkipped` | `EventText{Text: "  "}` → `(_, false)` |
| 同上 | `TestEventToEntry_Text_ThinkingPrefix_Skipped` | `EventText{Text: "[思考] hello"}` → `(_, false)`(F-34 不变)|
| `internal/channel/feishu/receipt_test.go` | `TestBuildReceiptCard_LongReply_SplitMultiDiv` | append 一条 2500 char EventText → card 含 3 个 div(≤ 1000 runes each),code block 保持 atomic |
| 同上 | `TestBuildReceiptCard_ShortReply_SingleDiv` | append 一条 500 char EventText → card 含 1 个 div |
| 同上 | `TestBuildReceiptCard_CodeBlockAtomic` | 1500 char 包含 `` ```code block``` `` → 整段不切 |
| `internal/channel/feishu/adapter_test.go` | `TestSend_OutReply_FoldsIntoReceipt_NoTruncate` | mock receipt;调 `Send(OutReply{Text: 1500 chars})` → receipt.Append called **1 次** + EventText.Text 完整 1500 chars(无 600B truncate) |
| 同上 | `TestSend_OutReply_OverflowLength_AsReply` | `OutReply{Text: 9000 runes (> perEntryMaxRunes=8000)}` → 走 `sendReplyAsMessage`,**不**调 `receipt.Append`,mock sendContent called **1 次** with ReplyInThreadAndChat |
| 同上 | `TestSend_OutReply_OverflowQuantity_AsReply` | 预填 receipt 45 entries,新 `OutReply{Text: 100 chars}` → 走 `sendReplyAsMessage`,**不**调 `receipt.Append` |
| 同上 | `TestSend_OutReply_AfterCompletion_AsReply` | receipt 已 StateCompleted,新 `OutReply{Text: 100 chars}` → 走 `sendReplyAsMessage`(不静默丢) |
| 同上 | `TestSend_OutReply_NoReceiptFallback` | receiptFor 返回 nil(receipt 冷启动失败)→ 走 `sendRawOutText`(top-level plain text bubble, fail-safe) |
| 同上 | `TestSend_OutReply_NoIconPrefix_OnOverflow` | `sendReplyAsMessage` 输出 body **不带** `💬` 前缀 |
| 同上 | `TestSend_OutReply_3SegmentDispatch_NoMarkdown_Text` | `OutReply{Text: "plain text"}` → `MsgTypeText` |
| 同上 | `TestSend_OutReply_3SegmentDispatch_LotsTables_Post` | 6 markdown tables → `MsgTypePost + tag:"md"` |
| 同上 | `TestSend_OutReply_3SegmentDispatch_Default_Card` | markdown ≤ 5 tables → `MsgTypeInteractive` Card 2.0 |
| 同上 | `TestSend_OutReply_LongText_TruncatesToEnvelope` | text 15000 runes → 进入 28 KB budget 路径,log warn + truncate |
| 同上 | `TestSend_OutReply_Orphan_NoUserMsgID_TopLevel` | `userMsgID == ""` → fail-safe `sendRawOutText` |
| 同上 | `TestSend_OutReply_SanitizeApplied` | text 含 `[x](relative)` → sanitize 后 plain text(`230001 invalid href` 防御) |
| `internal/gateway/translate_test.go`(若存在) | `TestTranslate_EventText_OutReply` | `Translate(EventText{Text: "hello"})` → `OutboundMessage{Kind: OutReply, Text: "hello"}` |
| `cmd/nightme/run_test.go`(若存在) | `TestResponderSend_OutReply` | 验证 `Kind: gateway.OutReply`(compile-time + runtime) |

### 3.2 集成测试(回归)

| 文件 | 用例 | 断言 |
|---|---|---|
| `internal/channel/feishu/adapter_test.go` | `TestReceipt_FullTurn_OutReplyFlow` | 完整 turn: user msg → 5 个 OutReply chunk(各 200-300 char)→ 1 个 OutResult 5000 char。receipt PATCH 多次,OutReply entries 完整 200-300 char 无截断;OutResult 走独立 reply 路径 |
| 同上 | `TestReceipt_StreamingToReply_Handoff` | 前 10 chunks 走 receipt fold, 第 11 个 chunk 触发超限(`perEntryMaxRunes`),改独立 reply;receipt 内只有 10 entries,后续 chunk 不进 receipt |
| 同上 | `TestReceipt_LongSingleReply_SplitMultiDiv` | 单条 5000 char OutReply → receipt 内 entry 占 5 个 div,无截断 |

### 3.3 grep / 回归(收尾)

```bash
# 验证 OutText 完全消失(除 git 历史 + 文档 changelog)
rg -n "OutText" --type=go  # 期望: 0 命中

# 验证 OutReply 全部覆盖
rg -n "OutReply" --type=go  # 期望: 含 gateway/messages.go + translate.go + adapter.go + run.go + 测试

# 验证 OutResult 路径仍工作(F-39 不被 F-40 影响)
go test ./internal/channel/feishu/... -run TestSend_OutResult
```

---

## 4. 落地顺序

每步独立 commit,可单独 review + revert:

| Step | 内容 | 文件 | 风险 |
|---|---|---|---|
| 1 | **本文档**(`F-40-outreply-overflow.md`) | `docs/feat/` | 零 |
| 2 | SPEC.md §0.9 + §12 backlog 更新 | `docs/SPEC.md` | 零 |
| 3 | channel/feishu.md §12 + §13.19 + §14 更新 | `docs/channel/feishu.md` | 零 |
| 4 | `OutText` → `OutReply` rename | `messages.go` + `translate.go` + `adapter.go` + `run.go` | 低(纯 enum rename,编译期 fail-fast)|
| 5 | `MessageReceipt.EntryCount() / IsCompleted()` helper | `receipt.go` | 低 |
| 6 | `eventToEntry(EventText)` 删 600B truncate | `receipt_event.go` | 低 |
| 7 | `buildReceiptCard` 用 `splitMarkdownForDivs` 拆多 div | `receipt.go` | 中 |
| 8 | `isOverflowingReceipt` + `Adapter.Send(OutReply)` 路由分流 | `adapter.go` | 中 |
| 9 | `sendReplyAsMessage` 新 helper(复用 F-39 sanitize / buildResultPayload / splitMarkdownForDivs) | `adapter.go` | 中 |
| 10 | `receipt_event_test.go` 改:删 600B truncate assertion | 改 | 低 |
| 11 | `receipt_test.go` 新增 `TestBuildReceiptCard_LongReply_SplitMultiDiv` | 改 | 零 |
| 12 | `adapter_test.go` 新增 5+ OutReply case(fold / overflow-length / overflow-quantity / late / no-receipt / 3-segment) | 改 | 零 |
| 13 | 全量 `go test ./...` + `go vet` + `golangci-lint` | — | 必过 |

---

## 5. 与上下游契约

### 5.1 OutboundMessage 契约

`Kind: OutText` → `Kind: OutReply`(enum 重命名)。**wire format 之外,字段不变**:`Text string` / `ReplyTo string` / `ChatID string` 全保留。Channel 自决渲染目标(receipt fold / 独立 reply)完全在 §1.4 边界规范允许范围。

### 5.2 ChatSession 状态机

不变。`EventText` 仍由 `cs.EventCallback` 触发 → `gateway.Translate` → `channel.Send`。Channel 内的渲染分支改了(从"强制 fold + 600B 截断"改为"fold 不截断 / 超限改独立 reply"),但状态机意义不变。

### 5.3 Tool thread 路由(F-37 tool-routing)

不动。F-thread-route 描述的是 thinking/tool/compaction,已经独立于 receipt。F-40 加 OutReply 超限也独立后,"独立 surface"模式更一致(thinking/tool → thread-only,result / reply-overflow → main-chat-visible reply)。

### 5.4 Result 路径(F-39)

不动。F-39 决策依然成立(OutResult 不进 receipt,走独立 reply)。F-40 加 OutReply 超限也独立后,两条独立 reply surface 平行:

- **OutResult**(F-39):always independent reply;`replyInThread=false`(ReplyInThreadAndChat)
- **OutReply 超限**(F-40):conditional independent reply;`replyInThread=false`(ReplyInThreadAndChat)
- **OutThinking / OutToolStart / OutToolEnd**(F-37):always thread reply;`replyInThread=true`

三组 surface 互不重叠,但 wire 层都复用 `sendContent` 底层。

### 5.5 多 div 拆分(F-37 multi-div)

复用扩展:`splitMarkdownForDivs` 现在服务 3 个 caller:
- `buildResultCardJSON`(F-39 OutResult surface)
- `buildThinkingCard`(F-think OutThinking)
- `buildReceiptCard`(F-40 receipt 内长 OutReply entry)

---

## 6. 后续工作(本文档不做)

- **OutReply multi-div 阈值上限**(backlog):当前超 `perEntryMaxRunes=8000` 改独立 reply,但 receipt 内单 entry 拆 N 个 div 不应挤掉 50 element / 24KB 预算。后续可加 receipt 内 "single entry max div count"(如 ≤ 5)防御
- **OutReply telemetry**(backlog):打点 `outreply.{fold_count, overflow_length_count, overflow_quantity_count, late_count, no_receipt_count}` 到 metric,看实际分布;若 overflow 比例高,考虑降 `perEntryMaxRunes` 到 4000
- **OutReply 合并**(P2):OutReply 流式 chunks 当前每 chunk 一个 `LogEntry`,可以借鉴 F-38 OutToolStart+End 合并模式,在 receipt 内做 chunk-merge(50ms 内的连续 chunks 合并成一个 entry),减少 receipt PATCH storm
- **OutReply markdown 渲染选项**(P2):当前 OutReply 走 lark_md(`buildReceiptCard` 用 `lark_md`),OutText 的 icon 是 `💬`。后续可加 per-chat toggle `/reply plain|markdown` 让用户选纯文本 / markdown

---

## 7. 不变式总结(本文档特殊要求)

**F-40 改 `OutText` 命名 + 删 600B 截断 + 加超限改独立 reply,但保留:**

- OutboundMessage 字段不变(`Kind` 改名 `OutText` → `OutReply`,其他全保留)
- Gateway 不变(`Translate` 仍产 OutboundMessage)
- ChatSession 不变(`currentTurnUserMsgID` 单数锚点保留)
- `OutboundMessage.ReplyTo = currentTurnUserMsgID` 不变(独立 reply 也锚同 userMsgID)
- 1 turn : 1 anchor 不变式保留
- §1.4 边界规范保留(OutReply 字段是 typed primitive string,Channel 自决 target)
- 抽象归抽象 / 具体归具体原则保留(超限改独立 reply 是 Feishu 自治决策)
- F-25 rolling-log UX 不变(receipt card 仍是"事件日志 + 元数据")
- F-39 OutResult 决策不变(OutResult 不进 receipt)
- F-37 / F-38 / F-think / F-38-tool-merge 全部决策不变

**为什么不叫 v2.0**:v1.3 核心不变式(职责隔离、Binding FSM owner、Receipt 自治、抽象归抽象 / 具体归具体)全部保留。F-40 是 Channel 自治范围内的渲染策略调整(命名 + 截断 + 超限路由),不影响 nightme 数据模型与 Gateway 契约。
