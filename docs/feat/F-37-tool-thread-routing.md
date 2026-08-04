# F-37: OutToolEnd + OutThinking → Thread Reply (with Type-Aware Summary)

> **Status**: ⏭ Planned (2026-08-04 Devin 拍板) · Docs-first · 代码改动 backlog
>
> **反转 v1.3 §13.6 / §13.7 / §13.9 折叠决议**。原计划把 OutThinking / OutToolStart / OutToolEnd 在 receipt card body 里用 `collapsible_panel` 折叠，实机上验证失败 —— agent turn 调 10 个工具 = 30 个 panel，Feishu 50 element 上限被频繁撞破；用户首要看到的"最终回答"被挤到 card 末尾甚至消失。
>
> **Milestone**: v1.3.x (post-F-watch)
> **Depends on**: F-08 (Channel interface), F-25 (rolling-log), F-33 (chatID 简化), §13.10 (reply-in-thread)
> **Related**: [`SPEC.md`](../SPEC.md) §0.3 摘要 + §1.3 v1.3.x 新增（F-thread-route） + §11 backlog; [`channel/feishu.md`](../channel/feishu.md) §13.12; [`F-08-channel-abstraction.md`](./F-08-channel-abstraction.md) §4; [`F-25-rolling-log.md`](./F-25-rolling-log.md) §3 + §3.1.1

---

## 1. Motivation

### 1.1 折叠方案失败

v1.3 §13.6 拍板的方案：

```
receipt card body (POST → PATCH in place)
├ header
├ 💭 [collapsible_panel] (折叠)
├ 🔧 Read(/a.py)        [collapsible_panel] (折叠)
├ ✅ Read done           [collapsible_panel] (折叠)
├ 🔧 Bash(...)           [collapsible_panel] (折叠)
├ ✅ Bash done           [collapsible_panel] (折叠)
├ 💭 [collapsible_panel] (折叠)
├ 📝 最终回复
└ footer
```

**实机问题**：

1. **元素预算爆**：Claude Code 一个 turn 调 5~10 个工具很常见 → 10 工具 × 3 entry (thinking + start + end) + N 段 thinking + 1 result = 30~50 element。Feishu card body 硬上限 50 element，**频繁撞破**触发 `evictOverflowLocked` 丢弃最早条目 → 用户看不到关键信息。
2. **视觉噪声 > 折叠收益**：折叠面板 header 自带 icon + 名字 + 一行 output 摘要。Header 文本比折叠内容还长，**用户看不到折叠前的内容就已经有信息过载感**。
3. **最终回答被挤掉**：折叠 panel 占据 card 大部分空间，最终回答（📝）和 footer（token / agent）被推到屏外，需要滚动才能看到 —— **这与"折叠让卡片聚焦答案"的设计目标背道而驰**。

### 1.2 替代方案：Thread Reply

把"中间过程"从 receipt card 移到 Feishu thread：

```
main chat:
  user_msg ⤵
  ⤵ receipt card (rootID=userMsgID)
      header (started · final state)
      💬 最终回复
      ────────────────────────
      Agent X · 1.2k tokens
  ↳ "X replies" 指示器（Feishu 自动汇总）

thread (click 指示器进入):
  💭 让我看一下...           (OutThinking)
  🔧 Read(/a.py)             (OutToolStart)
  ✅ Read /a.py → 1234 lines (OutToolEnd, 类型感知摘要)
  🔧 Bash(git status)        (OutToolStart)
  ✅ Bash git status → exit 0 (3 lines)  (OutToolEnd)
  💭 然后...                  (OutThinking)
```

**收益**：

- **Main chat 极简**：只看最终回答 + metadata。用户首要看到的就是答案。
- **Thread 自然容纳过程**：Feishu thread 没 50 element 上限；过程流按时间顺序排，用户想看就 click 指示器。
- **类型感知摘要**：OutToolEnd 不再 dump 原始 output（4KB+）到 thread，而是生成单行摘要（"Read → 1234 lines"）。thread 视觉清爽。
- **OutText / OutResult 不变**：最终回答继续走 receipt card（保持 ChatGPT-style "答案在主聊天显眼位置" UX）。

---

## 2. Design

### 2.1 Channel 按 OutboundKind 分流

Feishu adapter 在 `Send` dispatcher 按 Kind 自决 routing。  
**飞书有 3 种 reply 形态（实机验证，2026-08-04）**：

| 形态名 | Wire（HTTP body / path） | 飞书 main chat 看到 | 飞书 thread panel 看到 | 飞书响应里 `thread_id` |
|---|---|---|---|---|
| **Reply**（顶级 Create） | `POST /im/v1/messages` body `{receive_id, msg_type, content}`（**无** `root_id`） | 独立气泡（不挂任何 anchor 下） | 不在 thread panel（没有 thread 概念） | `""`（飞书不分配） |
| **ReplyInThread + Also send it to chat** | `POST /messages/{om_M0}/reply` body `{msg_type, content}`（`reply_in_thread` **字段省略**） | **正文**（内联 reply，带回复箭头） | **同一份正文**（按时间序） | `""`（飞书不分配独立 thread，reply 只是 main chat 的一条内联消息） |
| **ReplyInThread** | `POST /messages/{om_M0}/reply` body `{msg_type, content, reply_in_thread: true}` | **"X replies" 灰条**（无正文） | **正文**（按时间序；多条 share 同一 thread） | `omt_xxx`（飞书**第一次** reply-true 时分配，之后同 root_id 复用） |

> 命名约定：上表"形态名"来自 ops 确认（2026-08-04 实机飞书群 Frtpilot-Xiage 验证）。
> "ReplyInThread + Also send it to chat" 在 SDK body 上**就是 `reply_in_thread` 字段省略**——即 0 字节差异化。
> "ReplyInThread" 在 SDK body 上**就是 `reply_in_thread: true` 28 字节**——nightme F-37 选的路径。

按 OutboundKind 映射：

| OutboundKind | 飞书 reply 形态 | nightme 实际行为 |
|---|---|---|
| `OutThinking` | **ReplyInThread** | 纯文本 `💭 <text>`（每 event 一条）|
| `OutToolStart` | **ReplyInThread** | 纯文本 `● <name>(<args>)`（每 event 一条）|
| `OutToolEnd` | **ReplyInThread** | 纯文本 `⎿  <summary>`（类型感知摘要）|
| `OutCompaction` | **ReplyInThread** | 纯文本 `✶ Compacting conversation…` |
| `OutText` / `OutResult` / `OutInit` / `OutUsage` | n/a（PATCH in place 不走 reply API） | 进 receipt card body |
| `OutMessageState` | n/a | AddReaction ⏳/🔄/✅/❌ 在 user msg 上 |
| `OutCard`（permission card） | **ReplyInThread + Also send it to chat** | 进 main chat 内联回复 |
| `OutCommandReply`（slash 回应） | **ReplyInThread + Also send it to chat** | 进 main chat 内联回复 |
| Receipt 冷启动卡 | **ReplyInThread + Also send it to chat** | 进 main chat 内联回复（PATCH in-place） |
| 顶级 Create 形态 | **Reply** | nightme **不**用（fallback 路径 230011/231003 退化时才走顶级 Create，详见 §15.2） |

**`reply_in_thread` 字段语义**（来自 `larkim.ReplyMessageReqBody.ReplyInThread *bool`，
SDK 注释：「是否以话题形式回复；若群聊已经是话题模式，则自动回复该条消息所在的话题」）：

- **字段省略**（`omitempty` nil 指针）→ bot 消息**在 main chat 内联显示 + 进 thread panel** = "ReplyInThread + Also send it to chat"
- `false`（显式设 false）→ **字节级与"字段省略"不同**（多 28 字节 `"reply_in_thread":false`），但**飞书 UI 行为完全一致**（与"省略"等价）
- `true` → bot 消息**只在 thread 面板显示**，main chat 只看到 "X replies" 指示器 = "ReplyInThread"

**F-37 选 ReplyInThread**（`true`）给四条中间过程 path，让 main chat 干净只露 receipt card。

> **⚠️ 代码纪律**：`internal/channel/feishu/adapter.go` 的 `sendViaLarkReply` 必须保持 `if replyInThread { ... .ReplyInThread(true) }` 的写法，**不能简化成** `.ReplyInThread(replyInThread)`——后者在 false 路径会**多 28 字节** (`"reply_in_thread":false`)，破坏 pre-F-37 字节级 hash（recorder log / idempotency cache 失效）。`TestSend_ChatVisibleEvents_PassReplyInThreadFalse` 是这条约束的回归测试。

**为什么这是 Channel 自治**：Gateway 仍然只 stamp `out.ReplyTo = cs.currentTurnUserMsgID`；OutboundMessage 契约不变。Channel 看到 Kind 后自决 routing 目标（thread vs card vs reaction）。完全符合 SPEC §1.3 "抽象归抽象 / 具体归具体" 不变式。

### 2.2 Bridge 层 contract 扩展

`agent.ToolEndEvent` 加 `Args string` 字段：

```go
type ToolEndEvent struct {
    ID     string
    Name   string
    Args   string  // ← 新增：从同 message 的 tool_use block 拿
    Output string
    Err    error
    // ...
}
```

claudecode bridge 在解析 `tool_result` block 时，从**同一 message 的 content 里**找到对应的 `tool_use` block（ID 匹配），把它的 `input` 反序列化进 `ToolEndEvent.Args`。其他 bridge（pi / pty / acp / sdk）也按此 contract 填。

**为什么不只填到 OutboundMessage.Meta**：Meta 在 Gateway 翻译阶段读，bridge 不直接填 Meta。**bridge 填 event 字段，Gateway 翻译时把字段抄到 Meta**。这样 bridge contract 不依赖 Gateway 字段名。

### 2.3 类型感知摘要（"决断处理"）

Feishu adapter 包内 `summarize_tool.go` 提供 `summarizeToolEnd(name, args, output, err) string`：

| Tool Name | 摘要格式 | 示例 |
|-----------|---------|------|
| `Read` | `📄 Read <args> → <N> lines` | `📄 Read /foo/bar.go → 1234 lines` |
| `Write` | `📝 Write <args> → <N> bytes` | `📝 Write /foo.go → 5678 bytes` |
| `Edit` / `MultiEdit` | `✏️ <name> <args> → applied` | `✏️ Edit /foo.go → applied` |
| `Bash` | `💻 Bash \`<truncated args>\` → <N> lines` | `💻 Bash \`git status\` → 3 lines` |
| `Grep` | `🔍 Grep → <N> matches across <M> files` | `🔍 Grep → 12 matches across 5 files` |
| `Glob` | `📂 Glob → <N> files` | `📂 Glob → 8 files` |
| `WebFetch` | `🌐 WebFetch <args> → <N> chars fetched` | `🌐 WebFetch https://... → 4321 chars` |
| `WebSearch` | `🔎 WebSearch "<args>" → <N> results` | `🔎 WebSearch "go context" → 10 results` |
| `(default)` | `🔧 <name> → <first 200 chars of output>` | `🔧 CustomTool → first line of output...` |
| `err != nil` | `❌ <name> failed: <err.message>` | `❌ Bash failed: exit code 1` |

**不 dump 原始 output**：避免 thread 里出现 4KB+ 的文件内容 / bash 输出 / grep 匹配。

**Bash args 截断**：`truncate(args, 80)`，避免超长命令挤占 thread。

**args 缺失 fallback**：如果 bridge 没填 `Args`（旧 bridge 升级、或者 tool 不属于已知类型），用 `(name, output)` 生成摘要（不显示路径/命令）。

### 2.4 Receipt card 瘦身

`buildReceiptCard` 删 `Kind="thinking"` / `Kind="tool"` collapsible_panel 分支。Receipt card body 只承载：

- header line（状态 + 时间）
- OutText entry（assistant 中间文本，可能 0~N 个）
- OutResult entry（最终回答）
- eviction marker（如果触发）
- footer（agent · tokens）

`eventToEntry` 对以下 event 返回 `(_, false)`：
- `EventText` 且 text 以 `[思考] ` 前缀开头（thinking）
- `EventToolStart`
- `EventToolEnd`
- `EventCompaction`

→ `MessageReceipt.entries` 收窄到只装 OutText / OutResult / OutInit / OutUsage 派生的 entry。Card body 元素数通常 ≤ 5，**50 element 上限永远不破**。

**Silent PATCH（实现细节）**：`MessageReceipt.Append` 对 `EventToolStart` / `EventToolEnd` / `EventCompaction` 这三类返回 `(_, false)` 的 kind **不**写 entries，但**仍然**触发 `renderLocked`，同步 bump `eventCount` + `lastEventAt` 并 PATCH card。理由：thinking/tool/compaction 现在走 thread reply，main chat 的 card header（`🔄 ⏳ N · HH:MM:SS`）必须反映 agent 仍 busy，否则 header 会冻结在 tool 之前的时刻。PATCH 频率：每个 tool event 一次（≈ 50/min 在 hot agent 上，远低于 Feishu 1000/min rate limit）。

### 2.5 不变式

- **OutboundMessage 不动**：无新 Kind，无 Meta 字段约定（Meta 只承载数据载荷 output / err / args，不承载 routing hint）
- **Gateway 不动**：不感知 channel 分流
- **ChatSession 不动**：`currentTurnUserMsgID` 单数锚点保留
- **1 turn : 1 anchor 不变式保留**：所有 event 仍 anchor 到同一个 userMsgID；thread reply 的 rootID = userMsgID（跟 receipt card 同一个 rootID）
- **F-33 不变式保留**：nightme 数据模型不见 thread 字段（`thread_ts` / `message_thread_id` 等）；thread 路由是 Feishu SDK API 调用层面的细节
- **抽象归抽象 / 具体归具体**：thread 路由是 Feishu 自治决定，Slack / Web 各自决定怎么渲染 thinking/tool

---

## 3. Implementation

### 3.1 文件级变更清单

| 文件 | 改动 | 详细 |
|------|------|------|
| `internal/agent/agent.go` | `ToolEndEvent` 加 `Args string` 字段 + 注释 | §3.2 |
| `internal/bridge/claudecode/stream.go` | 解析 `tool_result` 时从同 message `tool_use` block 拿 args 填进 `ToolEndEvent.Args` | §3.3 |
| `internal/bridge/claudecode/claudecode_test.go` | 加 case：tool_use + tool_result 在同 message 时 `ToolEndEvent.Args` 非空 | — |
| `internal/bridge/pi/translate.go` | 同样填 `ToolEndEvent.Args`（如果 pi bridge 支持 tool_result） | — |
| `internal/channel/feishu/adapter.go` `Send` | 按 Kind 分流：thinking/tool/compaction → thread；text/result/init/usage → receipt card | §3.4 |
| `internal/channel/feishu/adapter.go` `buildReceiptCard` | 删 `Kind="thinking"` / `Kind="tool"` collapsible_panel 分支 | §3.4 |
| `internal/channel/feishu/receipt_event.go` | `eventToEntry` 对 thinking/tool/compaction 返回 `(_, false)` | §3.4 |
| `internal/channel/feishu/summarize_tool.go` | 新文件：`summarizeToolEnd` + `countLines` + `truncate` + `countUniqueFiles` helpers | §3.5 |
| `internal/channel/feishu/summarize_tool_test.go` | 新文件：覆盖各 tool 类型 + 错误分支 + args 缺失 fallback | — |
| `internal/channel/feishu/adapter_test.go` | `TestSend_OutThinking_PostsToThread` + `TestSend_OutToolStart_PostsToThread` + `TestSend_OutToolEnd_PostsToThread` + `TestSend_OutCompaction_PostsToThread` | — |
| `internal/channel/feishu/receipt_event_test.go` | 删 thinking/tool/compaction assertion（这些走 thread 不进 receipt） | — |
| `internal/channel/feishu/adapter_test.go` | 删 `TestSend_OutThinking_AppendsWithPrefix`（§13.1 bug 修复不再需要 —— prefix 不再被 strip） | — |
| `docs/channel/feishu.md` | §13.12 新增（决策反转记录）+ §15 实施计划修订 | §4.1 |
| `docs/feat/F-25-rolling-log.md` | §3 contract 表更新 + §3.1.1 新增 "Thread reply path" | §4.2 |
| `docs/feat/F-08-channel-abstraction.md` | §4 加 "Channel autonomous routing examples" + §6 边界情况表更新 | §4.3 |
| `docs/SPEC.md` | §0.3 摘要 + §1.3 v1.3.x 新增 + §11 backlog | ✅ 已落地 |
| `CHANGELOG.md` | [Unreleased] 条目 | — |

### 3.2 `agent.ToolEndEvent.Args`

```go
type ToolEndEvent struct {
    ID string
    // Name mirrors the tool name for symmetry with ToolStartEvent.
    Name string

    // Args are the raw or structured arguments passed to the tool.
    // Bridges populate this from the corresponding tool_use block
    // (same message, ID-matched) so channel renderers can produce
    // type-aware summaries (F-37 §2.3) without re-parsing the
    // tool_result content. May be empty if the bridge couldn't
    // correlate the result with a tool_use (defensive).
    Args string

    // Output is a short textual summary of the tool's result, suitable
    // for the renderer to surface in the rolling log. Bridges should
    // populate this from the tool's stdout / structured result /
    // response payload. The renderer truncates to perEntryMaxBytes
    // before display, so bridges may pass large payloads verbatim
    // without pre-truncating.
    Output string

    // Err is non-nil when the tool failed. When Err is set, Output
    // typically holds nothing (the failure path bypasses the
    // payload); channels may use either field for display.
    Err error
    // ...
}
```

### 3.3 claudecode bridge 关联 args

当前 `tool_result` 处理：

```go
case "tool_result":
    events <- agent.AgentEvent{
        Kind: agent.EventToolEnd,
        ToolEnd: &agent.ToolEndEvent{
            ID:     block.ToolUseID,
            Name:   block.Name,
            Output: stringifyToolResult(block.Content),
        },
    }
```

**改动**：在 `case "user"` 内收集 `tool_use` block（按 ID），在 `case "tool_result"` 内查表填 Args：

```go
case "user":
    // ... existing parsing ...
    for _, block := range ev.Message.Content {
        switch block.Type {
        case "tool_use":
            toolUseArgs[block.ID] = block.Input  // ← 新增：收集 args
            // emit EventToolStart
        case "tool_result":
            args := toolUseArgs[block.ToolUseID]  // ← 新增：查表拿 args
            events <- agent.AgentEvent{
                Kind: agent.EventToolEnd,
                ToolEnd: &agent.ToolEndEvent{
                    ID:     block.ToolUseID,
                    Name:   block.Name,
                    Args:   args,                  // ← 新增
                    Output: stringifyToolResult(block.Content),
                },
            }
        }
    }
```

`toolUseArgs` 是 per-message 的局部 map（`for message` 循环内重置）。`block.Input` 是 `json.RawMessage`，按 tool name 决定是否 `json.Marshal` 成 string（vs 留 raw）。

### 3.4 Feishu adapter `Send` 分流

```go
func (a *Adapter) Send(ctx context.Context, msg gateway.OutboundMessage) error {
    switch msg.Kind {
    case gateway.OutThinking:
        body := "💭 " + msg.Text
        // replyOnly=true: 💭 不进 main chat
        return a.postThreadReply(ctx, msg.ChatID, msg.ReplyTo, body, true)

    case gateway.OutToolStart:
        name, _ := msg.Meta["tool_name"].(string)
        args, _ := msg.Meta["args"].(string)
        body := "🔧 " + formatToolStart(name, args)
        return a.postThreadReply(ctx, msg.ChatID, msg.ReplyTo, body, true)

    case gateway.OutToolEnd:
        name, _ := msg.Meta["tool_name"].(string)
        args, _ := msg.Meta["args"].(string)
        output, _ := msg.Meta["output"].(string)
        err, _ := msg.Meta["err"].(error)
        body := summarizeToolEnd(name, args, output, err)
        return a.postThreadReply(ctx, msg.ChatID, msg.ReplyTo, body, true)

    case gateway.OutCompaction:
        return a.postThreadReply(ctx, msg.ChatID, msg.ReplyTo,
            "✶ Compacting conversation…", true)

    case gateway.OutText, gateway.OutResult, gateway.OutInit, gateway.OutUsage:
        // 不变：fold into receipt card
        receipt := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo)
        if receipt == nil {
            return a.sendRawOutText(ctx, msg.ChatID, msg.Text)
        }
        return receipt.Append(ctx, /* translated event */)

    // OutMessageState / OutMessageStateRemoved / OutCard: 不变
    //   - OutCard (permission card): reply_in_thread=false，权限卡必须 main chat 可见
    //   - OutCommandReply: reply_in_thread=false，slash 回应必须 main chat 可见
    }
}

// postThreadReply 直接 POST 到 Feishu thread。
// rootID = msg.ReplyTo = currentTurnUserMsgID。
// replyOnly=true: 内部 sendViaLarkReply 会设 body.reply_in_thread=true
// （larkim.ReplyMessageReqBody.ReplyInThread field），让消息只在线程
// 面板显示、main chat 只看见 "X replies" 指示器。
func (a *Adapter) postThreadReply(ctx context.Context, chatID, rootID, body string, replyOnly bool) error {
    if rootID == "" {
        // Orphan event (startup EventInit etc.) — fall back to top-level
        return a.sendRawOutText(ctx, chatID, body)
    }
    _, err := a.SendMessageText(ctx, chatID, body, rootID)
    return err
}
```

`SendMessageText` 已经接 `rootID` 参数（§13.10 已落地）。`postThreadReply` 只是薄包装。

**`buildReceiptCard` 删 collapsible_panel 分支**：删除 `if e.Kind == "thinking"` 和 `if e.Kind == "tool"` 两个 block，连同对应的 case label。

**`eventToEntry` 收窄**：

```go
case agent.EventText:
    text := strings.TrimSpace(ae.Text)
    if text == "" { return LogEntry{}, false }
    if strings.HasPrefix(text, thinkingPrefix) {
        // F-37: thinking 走 thread，不再 fold 进 receipt
        return LogEntry{}, false
    }
    return LogEntry{Icon: "💬", Text: truncateForLog(text, perEntryMaxBytes), Kind: "reply"}, true

case agent.EventToolStart:
    // F-37: tool_start 走 thread
    return LogEntry{}, false

case agent.EventToolEnd:
    // F-37: tool_end 走 thread (类型感知摘要)
    return LogEntry{}, false

case agent.EventCompaction:
    // F-37: compaction 走 thread
    return LogEntry{}, false
```

**`Gateway.translate.go` 是否需要改**？

`OutThinking` 当前由 Gateway 在 `Translate` 里把 `[思考] ` 前缀剥掉再 emit。**F-37 之后**：

- Gateway 不再剥前缀（adapter 不再依赖 receipt_event 的 prefix detection）
- adapter 直接拿 `msg.Text` 当 thread body，加 `💭 ` 前缀即可

→ **Gateway `translate.go` 简化**：删 thinkingPrefix 剥除逻辑（或保留作为兼容性 vestigial；建议保留以免未来回滚）。

**OutboundMessage.Meta 增加 `args`**：

```go
// gateway/translate.go
case agent.EventToolStart:
    return OutboundMessage{
        Kind: OutToolStart,
        Text: text,
        Meta: map[string]any{
            "tool_name": name,
            "args":      ev.ToolStart.Args,  // ← 已有,无须加
        },
    }, true

case agent.EventToolEnd:
    return OutboundMessage{
        Kind: OutToolEnd,
        Text: text,
        Meta: map[string]any{
            "tool_name": name,
            "output":    ev.ToolEnd.Output,
            "err":       ev.ToolEnd.Err,
            "args":      ev.ToolEnd.Args,  // ← 新增
        },
    }, true
```

### 3.5 `summarize_tool.go`

```go
package feishu

import (
    "fmt"
    "strings"
)

const (
    toolArgsMaxBytes = 80
    toolOutputPreviewBytes = 200
)

// summarizeToolEnd produces a one-line summary of a tool's result.
// Per-tool-type heuristics so the user sees the signal (file path,
// line count, exit status) instead of a wall of raw output.
// Falls back to byte truncation for unknown tools. Err wins over
// success path.
func summarizeToolEnd(name, args, output string, err error) string {
    if err != nil {
        return fmt.Sprintf("❌ %s failed: %s", name, err.Error())
    }
    switch strings.ToLower(name) {
    case "read":
        return fmt.Sprintf("📄 %s %s → %d lines", name, args, countLines(output))
    case "write":
        return fmt.Sprintf("📝 %s %s → %d bytes", name, args, len(output))
    case "edit", "multiedit":
        return fmt.Sprintf("✏️ %s %s → applied", name, args)
    case "bash":
        cmd := truncate(args, toolArgsMaxBytes)
        return fmt.Sprintf("💻 Bash `%s` → %d lines", cmd, countLines(output))
    case "grep":
        return fmt.Sprintf("🔍 Grep → %d matches across %d files",
            countLines(output), countUniqueFiles(output))
    case "glob":
        return fmt.Sprintf("📂 Glob → %d files", countLines(output))
    case "webfetch":
        return fmt.Sprintf("🌐 WebFetch %s → %d chars fetched", args, len(output))
    case "websearch":
        return fmt.Sprintf("🔎 WebSearch %q → %d results",
            truncate(args, toolArgsMaxBytes), countLines(output))
    default:
        return fmt.Sprintf("🔧 %s → %s", name, truncate(output, toolOutputPreviewBytes))
    }
}

func countLines(s string) int {
    if s == "" { return 0 }
    return strings.Count(s, "\n") + 1
}

func countUniqueFiles(s string) int {
    // Grep output typical format: "path/to/file:line:match"
    // We extract unique paths.
    seen := make(map[string]struct{})
    for _, line := range strings.Split(s, "\n") {
        if idx := strings.Index(line, ":"); idx > 0 {
            seen[line[:idx]] = struct{}{}
        }
    }
    return len(seen)
}

func truncate(s string, max int) string {
    if max <= 3 || len(s) <= max { return s }
    budget := max - 3
    for i := 0; i < len(s); i++ {
        if i > budget { return s[:i] + "..." }
    }
    return s
}
```

### 3.6 Receipt card 元素数（实机数据）

折叠方案下典型 agent turn (10 个工具 + 3 段 thinking + 1 result) 的 card 元素数：

```
1 (header) + 3 (thinking panel) + 10 (tool start panel) + 10 (tool end panel) + 1 (result) + 1 (hr) + 1 (footer) = 27
```

→ 已接近 50 上限；turn 再大点直接撞破。

F-thread-route 方案下同一 turn：

```
1 (header) + 0 (no OutText in receipt) + 1 (result) + 1 (hr) + 1 (footer) = 4
```

→ 永远在 50 上限内，evictOverflowLocked 永不触发。

---

## 4. Documentation Updates

### 4.1 `docs/channel/feishu.md`

- §13.12 新增：**决策反转记录**（详述折叠方案失败原因 + thread 方案收益 + OutToolEnd 类型感知摘要表 + 实施要点）
- §15 实施计划修订：删原"§13.1 + §13.6" 修复步骤，换成 F-thread-route 的实施步骤（adapter 分流 + summarize_tool.go + bridge args + receipt 瘦身 + 测试改造）
- §14 changelog 加 2026-08-04 条目

### 4.2 `docs/feat/F-25-rolling-log.md`

- §3 Channel Implementation Contract 表更新：
  - `OutThinking` / `OutToolStart` / `OutToolEnd` 行 → "Channel-specific (Feishu: thread reply with type-aware summary)"
  - `OutCompaction` 行 → "Channel-specific (Feishu: thread reply)"
- §3.1 Feishu implementation reference 加 §3.1.1 "Thread reply path"
- §1 Description 加一段：receipt card 收窄到只承载 OutText / OutResult / OutInit / OutUsage

### 4.3 `docs/feat/F-08-channel-abstraction.md`

- §4 "Channel is dumb" contract 表加一行：`✅ 自行决定按 OutboundKind 分流（thread / card / reaction / ...）— Channel 自治范围内的渲染决策`
- §6 边界情况表新增：OutThinking / OutToolStart / OutToolEnd 走 thread（Feishu 行为）；其他 Channel 自决
- §7 Test plan 加 case：mock Channel 验证收到 OutThinking 时不调 receipt（如果它实现了 thread 分流）

### 4.4 `CHANGELOG.md`

[Unreleased] 段加 "F-thread-route" 条目（参见 §5 模板）。

---

## 5. CHANGELOG Entry Template

```markdown
### F-thread-route: OutThinking / OutToolStart / OutToolEnd → Feishu thread reply

反转 v1.3 §13.6 折叠方案（实机验证失败：30 panel 撞破 50 element
上限、视觉噪声大于折叠收益、最终回答被挤掉）。新方案：Channel 按
OutboundKind 自决 routing——thinking/tool/compaction 直接 POST 到
Feishu thread（rootID = userMsgID），receipt card 收窄到只承载
最终答复（OutText / OutResult）+ 元数据（OutInit / OutUsage）。

OutToolEnd 类型感知摘要（"决断处理"）：bridge 层把
`ToolEndEvent.Args` 填好；Channel 层 `summarizeToolEnd(name, args,
output, err)` 按 tool name 生成单行摘要（"📄 Read /foo.go → 1234
lines"），不 dump 原始 output 到 thread。Receipt card body 元素数
从 ~30 降到 ≤5，50 element 上限永远不破。

Bridge 层 contract 扩展：`agent.ToolEndEvent.Args string` 字段；
claudecode bridge 从同 message `tool_use` block 拿 args 填入。

不变式：OutboundMessage 不动（无新 Kind）；Gateway 不动；ChatSession
不动；`currentTurnUserMsgID` 单数锚点保留；F-33 thread 概念不进
nightme 数据模型不变式保留。

详见 [`docs/SPEC.md` §0.3](./SPEC.md) + [`docs/channel/feishu.md` §13.12](./channel/feishu.md)。
```

---

## 6. Test Plan

### 6.1 Unit

**Bridge 层**：

- `internal/bridge/claudecode/claudecode_test.go`：构造 fixture 含同 message 的 `tool_use` + `tool_result` block，验证 `ToolEndEvent.Args` 非空且内容匹配 `tool_use.input`。
- 反向 case：`tool_result` 找不到对应 `tool_use`（不同 message）→ `Args` 为空（不 panic、不报错）。
- 反向 case：`tool_use` 没有 `tool_result`（罕见）→ 不影响 emit `EventToolStart`。

**Channel 层**：

- `internal/channel/feishu/summarize_tool_test.go`：覆盖各 tool name 分支（Read / Write / Edit / Bash / Grep / Glob / WebFetch / WebSearch / default）+ 错误分支 + args 缺失 fallback。
- `internal/channel/feishu/adapter_test.go`：
  - `TestSend_OutThinking_PostsToThread`：mock `sendViaLarkReply`，验证收到 `OutThinking` 时调用 Reply endpoint（rootID = msg.ReplyTo）+ body 含 `💭` 前缀。
  - `TestSend_OutToolStart_PostsToThread`：验证收到 `OutToolStart` 时调 Reply + body 含 `🔧 <name>(<args>)`。
  - `TestSend_OutToolEnd_PostsToThread`：验证收到 `OutToolEnd` 时调 Reply + body 经 `summarizeToolEnd` 生成（含 `📄 Read /foo.go → 1234 lines` 这类格式）。
  - `TestSend_OutCompaction_PostsToThread`：验证 thread reply + `✶ Compacting conversation…`。
  - `TestSend_OutText_FoldsIntoReceipt`：回归测试，确保 OutText / OutResult / OutInit / OutUsage 仍然 fold 进 receipt（不变）。
- `internal/channel/feishu/receipt_event_test.go`：
  - 删 thinking/tool/compaction 的 entry assertion（这些 event 不再生成 entry）
  - 加 case：调用 `eventToEntry(EventText, "[思考] foo")` 返回 `(_, false)`（不再生成 thinking entry）
- `internal/channel/feishu/receipt_test.go`：
  - 回归测试：receipt card body 不再含 `collapsible_panel` 元素
  - 加 case：receipt card body 元素数 ≤ 5 (header + result + hr + footer)

**Gateway 层**：

- `internal/gateway/translate_test.go`：
  - 加 case：`Translate(EventToolEnd{Name: "Read", Args: "/foo.go", Output: "..."})` 返回 `OutboundMessage{Kind: OutToolEnd, Meta["args"]: "/foo.go"}`

### 6.2 集成

- 端到端 mock Channel + Gateway：发一条消息 → agent turn 产出 1 个 OutThinking + 1 个 OutToolStart + 1 个 OutToolEnd → mock Channel 收到 3 条 OutboundMessage → 验证 mock 的 Send 函数被调 3 次，且每次 Kind 不同。
- Receipt 端到端：mock Channel 收到 OutText + OutResult → 验证 receipt card body 只有这俩 entry，无 collapsible_panel。

### 6.3 E2E（实机飞书 DM）

- DM 发消息 → agent turn 调 3 个工具（Read / Bash / Edit）→ 验证：
  - Main chat：receipt card 显示最终回答 + 1 个 thread indicator "4 replies"（💭 thinking + 3 工具 messages）
  - Click thread indicator → 看到 5 条 thread messages：💭 + 🔧 Read + ✅ Read → ... + 🔧 Bash + ✅ Bash → ... + 🔧 Edit + ✅ Edit → ...
  - 不再有 30 个 collapsible_panel 视觉噪声
- DM 发消息 → agent turn 只产生 thinking 无工具调用 → 验证：receipt card + thread 里只有 💭（无 🔧/✅）
- DM 发消息 → agent turn 出错（tool failed） → 验证：thread 里 `❌ Bash failed: exit code 1`

---

## 7. Backlog

- **OutThinking 多 chunk 聚合**：agent turn 里 OutThinking 经常多次 emit（每段推理一个）。当前每段一条 thread reply → thread 里 N 条 💭。可聚合成 "💭 N 段" 单条消息（streaming 模式，最后一段更新），减少 thread 噪声。**不在本 PR scope**。
- **未知 tool type 摘要策略**：默认走字节截断（200 chars）。如果 claudecode 后续新增 tool name，channel 自动 fallback 到默认路径。**无需额外工作**。
- **Web UI / Slack 适配**：本决策仅影响 Feishu adapter。其他 channel（如未来 Web / Slack）应各自决定怎么渲染 OutThinking / OutTool* —— **不**复制 Feishu 的 thread 方案，保持各自平台原生 UX。
- **Thread reply 失败 vs receipt card 失败**：当前 thread reply 失败 → log warn + drop（不影响 receipt card）。如果某些场景下 thread reply 必须成功（比如用户依赖 thread 看过程），可以加重试 / fallback。但 MVP 不需要，backlog。

---

## 7.5 实机飞书群验证（2026-08-04，Frtpilot-Xiage）

| 发送 | 形态 | 飞书响应 | main chat UI 实际显示 |
|---|---|---|---|
| `[probe-A]` Create 顶级 (`oc_4a06da49bc0131ff14b381498e4fed9d`） | Reply | `message_id=om_xxx, parent_id="", root_id="", thread_id=""` | 独立气泡，不挂 M0 下 ✓ |
| `[probe-B]` Reply to M0，省略 reply_in_thread | ReplyInThread + Also send it to chat | `parent_id=M0, root_id=M0, thread_id=""` | main chat 显示**正文内联**（带回复箭头），thread panel 也有 ✓ |
| `[probe-D]` Reply to M0, reply_in_thread=true | ReplyInThread | `parent_id=M0, root_id=M0, thread_id=omt_19141bf7110e1c89` | main chat 只显示 "X replies" 灰条，**正文只在线程里** ✓ |
| `[probe-D2/D3/D4]` 续发 3 条 reply-true | ReplyInThread | 全部 `thread_id=omt_19141bf7110e1c89`（共享） | 4 条 D share 同一个 thread，main chat 看到 "4 replies" 灰条 |

**关键发现**：

1. **顶级 Create 不分配 thread_id**（飞书响应 `thread_id=""`）——这跟 mock 假设的"self-root"不同。
2. **ReplyInThread + Also send it to chat 也不分配 thread_id**（B 响应 `thread_id=""`）——只是 main chat 的内联 reply。
3. **ReplyInThread 才分配独立 thread_id**（D 响应 `thread_id=omt_19141bf7110e1c89`）——之后同 root_id 的 reply-true 复用此 thread。
4. **msg.ReplyTo 必须始终是 M0**（当前用户消息 id）—— 4 条 D 全部 reply M0，**不**chain reply 到上一条 D；这反向验证 §13.10 / F-33 "单数锚点 currentTurnUserMsgID" 不变式**真的必要**（如果链式 reply，thread 碎裂成 N 个独立 "1 reply" 指示器，UI 不再汇总）。

Probe 工具代码：`cmd/_probe/feishu_thread_probe.go`（mock 版）+ `cmd/_probe/send_one/main.go`（真实发送版）。决策落地后建议删除（保留实机飞书响应记录在本节即可）。

---

## 8. Change log

- **2026-08-04 (a)** — F-37 草案（Devin 拍板反转 §13.6 折叠方案）。Docs 落地（SPEC §0.3 + 本 doc + channel/feishu §13.12 + F-25 §3 收窄 + F-08 §4 自治路由例子 + CHANGELOG）。代码改动 backlog §3.1。
- **2026-08-04 (b)** — 实机飞书群（Frtpilot-Xiage）验证 "Reply / ReplyInThread+Also send it to chat / ReplyInThread" 三种形态；用 `cmd/_probe/send_one` 直接发 8 条组合消息，把命名固化进 §2.1 表格和 §7.5 实验记录；记录 `thread_id` 分配规则（顶级 / default-reply 不分配；reply-true 分配并复用）。
- **2026-08-04 (c)** — `reply_in_thread` 字段"省略 vs 显式 false"字节差异（28B）发现：`TestSend_ChatVisibleEvents_PassReplyInThreadFalse` 单测 + 代码注释固化"if replyInThread { .ReplyInThread(true) }" 必须保持的纪律。
