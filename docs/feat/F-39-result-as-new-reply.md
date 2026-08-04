# F-39: OutResult → Independent Reply (Receipt 不再 fold 最终答复)

> **Status**: implemented (v1.3.x, 2026-08-04)
> **Scope**: `internal/channel/feishu/{adapter,receipt_event,card_sanitize,result_render}.go` — `OutResult` 不再 fold 进 rolling-log receipt card,改为独立 reply 投递
> **目的**: 消除 `OutResult → receipt.Append → 📝 Entry` 路径上的"长答复被 dedup 静默吞"问题;与 [cc-connect `platform/feishu/feishu.go::buildReplyContent`](https://github.com/chenhg5/cc-connect/blob/main/platform/feishu/feishu.go) + [openclaw-lark `card/builder.ts::buildCompleteCard`](https://github.com/larksuite/openclaw-lark/blob/main/src/card/builder.ts) 的"最终答复独立 surface"模式对齐
> **Reverse**: [SPEC §13.3](../SPEC.md) §"OutResult 600 字节截断 → F-37 multi-div 拆解"、F-37 doc §0.1 §1.4 中"OutResult entry 走 receipt"假设
> **Related docs**:
> - [docs/feat/F-25-rolling-log.md](./F-25-rolling-log.md) — receipt 整体 UX (F-39 后剥离 OutResult)
> - [docs/feat/F-37-multi-div-content-split.md](./F-37-multi-div-content-split.md) — `splitMarkdownForDivs` 仍用于新 helper,不再服务于 receipt 内 OutResult
> - [docs/feat/F-37-tool-thread-routing.md](./F-37-tool-thread-routing.md) — thinking/tool/compaction 已经在 thread;F-39 加 OutResult 也独立
> - [docs/feat/F-08-channel-abstraction.md](./F-08-channel-abstraction.md) — Channel 自治范围
> - [docs/SPEC.md §1.4](../SPEC.md) — 抽象 / 具体 边界规范(OutResult 字段不变,Channel 自决渲染目标)
> - [docs/channel/feishu.md §13](../channel/feishu.md) — F-39 decision record + 渲染映射表更新

---

## 0. 背景

### 0.1 旧结构(被推翻)

v1.3.x F-37 之前,receipt card 是 rolling-log events 容器:

```
User msg → Receipt Card (单张,反复 PATCH)
   ├ ⏳ / 🔄 / ✅  header (state 变化)
   ├ 💬 💬 💬    OutText 流式 chunks
   ├ 📝          OutResult 完整文本 (eventToEntry.EventResult 输出)
   ├ ✅ done / ❌  EventDone / EventError 状态转换
   └ <hr> + footer (Agent · cwd · tokens)
```

F-37 把 thinking/tool 移到 thread receipt 后:

```
User msg → Receipt Card (单张)
   ├ header
   ├ 💬 💬 💬    OutText 流式 chunks
   ├ 📝          OutResult
   └ <hr> + footer
```

### 0.2 dedup 协调 — 这次踩到的实际 bug

为避免"流式 chunk + 最终 result"在 receipt 内显示重复,`eventToEntry.EventResult` 有 dedup:

```go
// internal/channel/feishu/receipt_event.go:113-124 (旧)
if !ae.Result.IsError && lastEntry != nil &&
    lastEntry.Kind == "reply" &&
    lastEntry.Text == truncateForLog(text, perEntryMaxBytes) {
    return LogEntry{}, false   // ← OutResult 直接静默丢
}
```

`perEntryMaxBytes = 600`。Claude Code stream-json 的语义:`result.result` 是 assistant 流式累积的最终文本,与最后一条 `assistant` event 的 `content[0].text` **字节级相等**。落到 receipt 端:

- 最后一条 `EventText` entry 经 `truncateForLog(text, 600)` 处理 → "前 600 字 + …"
- 同样 `truncateForLog(resultText, 600)` → "前 600 字 + …"
- 两侧字节级相等 → dedup 触发 → **OutResult entry 不加**

**用户实际行为**:长答复(> 600 字)场景下收不到完整 📝 行,只看到 N 条碎裂的"前 600 字 + …" 💬 行。这就是 user 实机报告的"**答复被截断了**"现象(与 element 数无关,F-39 §0.3 详述)。

### 0.3 元素数问题 ≠ 真正的截断因

F-thread-route (commit 098fdb7) 落地后,receipt card 只承载 OutText / OutResult / 状态 header / footer / 任务清单。典型一轮 turn:

| 来源 | 元素 |
|---|---|
| header (state) | 1 |
| OutText chunk N (各 ≤ 600 char) | N (单 div,600 < 1000 不需拆) |
| OutResult (旧路径) | 1-8 (F-37 multi-div) |
| task checklist (F-38) | 0-3 |
| `<hr>` + footer | 2 |

**典型 8-25 元素,50 元素上限几乎从不撞。** envelope 30 KB 也几乎从不撞(45 × 600 ≈ 27 KB 边缘)。**真正的"截断"是 dedup,不是 element / envelope。**

### 0.4 候选方案

| 方案 | 描述 | 改动 |
|---|---|---|
| (a) 加 dedup 判定 `len(text) <= perEntryMaxBytes` | 让短 result 仍 dedup,长 result 强制进 receipt | 1 行 + 测试 |
| (b) **OutResult 独立 reply(本 feature)** | OutResult 不进 receipt;独立 helper 渲染 markdown 投递 | adapter.go + 2 个新文件 + ~200 行 |
| (c) 维持现状继续修 dedup 边界 | 只动 receipt_event.go | 中等 |

**选择 (b)** 因为:

1. **彻底消除 dedup 需要协同工作的对象**(根本没"流式 chunk + 最终 result 同一 surface"的协调问题)
2. **架构上与 cc-connect / openclaw-lark 对齐**(业界已验证的"streaming card for progress, complete reply for deliverable"模式)
3. **打开 envelope 真撞墙的降级路径**(独立 helper 可以做 30 KB hard cap + fallback,无需保护 receipt 内的其他 entry)
4. **治了"用户偶尔看到一条独立 text 气泡 + 一张卡"的 race**(旧路径 cold-start 失败 → `sendRawOutText` fallback,可能跟后续成功的 receipt card 并存;新路径无此 race)

### 0.5 不可变约束

- **`OutboundMessage` 契约不变**:`Kind: OutResult` + `Result *agent.ResultEvent` typed field 保留 (§1.4 边界规范)
- **Gateway 不动**:`gateway/translate.go::Translate` 不需改
- **ChatSession 不动**:per-turn / per-chat 状态机无变化
- **抽象归抽象**:Channel 自治范围内决定渲染目标(从 receipt card 转独立 reply 是 Channel 决策,不影响抽象层)
- **`OutboundMessage.ReplyTo` 不动**:`currentTurnUserMsgID` 仍作为锚点,新 reply 也锚到同 userMsgID

---

## 1. 设计

### 1.1 视觉对比

**改前**:

```
user_msg om_A
  └ Receipt Card ⤓ (thread 视觉连接到 om_A; visible in main chat)
      ⏳ 等待中
      💬 前 600 字 + …
      💬 前 600 字 + …
      📝 ???  ← dedup 静默吞,长答复看不到这里
      <hr>
      footer
```

**改后**:

```
user_msg om_A
  ├ Receipt Card ⤓ (rolling log, 锚定 om_A)
  │   ⏳ → 🔄 处理中 → ✅ 已完成 10:11:11
  │   💬 前 600 字 + …
  │   💬 前 600 字 + …
  │   <hr> + footer (agent · workspace · tokens)
  └ Final Result Reply ⤓ (锚定 om_A; 独立 message; 富 markdown 渲染)
      📝 完整 OutResult text (无 600 cap,无 dedup)
```

Receipt card 退化为"事件日志 + 元数据",final answer 独立成为"答案交付"。**两条独立 surface,无需 dedup。**

### 1.2 Dispatch(三段式,抄 cc-connect)

`sendResultAsReply` helper 的 dispatch 逻辑(与 cc-connect `buildReplyContent` 镜像):

```
            ┌─ has <at ...> OR no markdown indicators
            │   → MsgTypeText (无 markdown → 单 plain text bubble)
            │
sanitize    ├─ markdown 且 tables > 5
  ↓         │   → MsgTypePost + tag:"md"  (GFM 渲染,无 Card 2.0 表格硬限)
content     │       (Feishu post 内容通常 unlimited; 长短都接受)
            │
            └─ markdown 且 tables ≤ 5
                → MsgTypeInteractive (Card 2.0)
                  elements: [
                    {tag:"markdown", content: chunk 1},  ← F-37 splitMarkdownForDivs
                    {tag:"markdown", content: chunk 2},  ← 1000 runes/div hard cap
                    ...,
                  ]
                  sanitized via SanitizeCardMarkdown
```

**关键决定**:

- **`MsgTypeText` 极少用** —— Claude Code 几乎永远输出 markdown(text chunk 含 `` ``` ``, `*`, `` `- `` 等),走 plain text 会丢失代码块、链接、列表、表格
- **`MsgTypePost` + `md` 兜底多表** —— Card 2.0 表格硬限 5 张,超出返 11310 错误;Post + md 是 GFM 全套且无该限制
- **Card 2.0 + single/multi-div 主力** —— 默认路径,SanitizeCardMarkdown 处理(URL / fence / heading / image strip),splitMarkdownForDivs 处理超长(从 receipt.go 复用)

### 1.3 Envelope 防御

每个 result reply 在 `sendContent` 前过 byte budget check:

```go
const resultCardEnvelopeBudget = 28 * 1024  // 30 KB - 2 KB headroom

if len(body) > resultCardEnvelopeBudget {
    log.Warn(...)
    truncated := truncateRunes(sanitized, int(resultCardEnvelopeBudget/3))
    msgType, body = buildResultPayload(truncated)
}
```

OutResult 经 `perEntryMaxRunes = 8000` cap(CJK 3 B/char ≈ 24 KB,远低于 30 KB envelope),实际撞 envelope 概率低;这里是 defensive fallback。

### 1.4 状态机变化

`MessageReceipt` 状态机不变(Waiting → Executing → Completed → Error)。新增一条触发点:当 `OutResult` 到达时,**先调 `receipt.SetCompleted(ctx)`** 把 receipt 标记为终态 ✅,然后投递独立 result reply。两个动作**原子性不强**(中间失败有日志兜底),但顺序保证:

1. 用户先看到 receipt card 切到"✅ 已完成 HH:MM:SS"(滚动日志收尾)
2. 用户后看到 Final Result Reply(完整最终答复)

视觉顺序在 Feishu 消息流上是 PATCH → Send,API 调用顺序与用户感知顺序一致。

---

## 2. 文件 & 接口

### 2.1 新文件

**`internal/channel/feishu/card_sanitize.go`** —— markdown sanitize pipeline(移植 cc-connect):

```go
// SanitizeCardMarkdown 是 result reply 内容的统一入口处理。Pipeline:
//   1. URL sanitize     — non-HTTP(S) link → plain text (避免 230001 invalid href)
//   2. Fence newline    — ``` 前必须 newline,否则 lark_md 当 inline code 渲染
//   3. Image strip      — 删 ![alt](not-img_xxx),只留 Feishu image_key
//   4. Heading demotion — H1 → H4, H2-H6 → H5 (lark_md heading 范围窄)
//   5. Code-block protect — ```block``` 在所有变换中保护不动
//
// 来自 cc-connect platform/feishu/feishu.go:3017-3104 (preprocessFeishuMarkdown / 
// sanitizeMarkdownURLs / stripInvalidFeishuCardImages / optimizeFeishuCardMarkdown)。
// Nightme 不需要 cc-connect 的 `<at>` 处理(已在 Gateway 层 / mention.go 解决)。
func SanitizeCardMarkdown(text string) string
```

**`internal/channel/feishu/result_render.go`** —— result reply 渲染 helper:

```go
// containsMarkdown 检测 markdown 指示符(抄 cc-connect)
func containsMarkdown(s string) bool

// countMarkdownTables 超过 maxCardTables(5)就走 Post (抄 cc-connect)
func countMarkdownTables(s string) int

// buildPostMdJSON  Post + tag:"md" 渲染
func buildPostMdJSON(content string) string

// buildResultCardJSON  Card 2.0 + 多 markdown 元素(用 splitMarkdownForDivs 拆)
func buildResultCardJSON(content string) (string, error)

// buildResultPayload 三段 dispatch(返回 msgType + body + error)
func buildResultPayload(sanitized string) (string, string, error)

// truncateRunes 字数 cap(避免 envelope 撞墙)
func truncateRunes(s string, maxRunes int) string
```

### 2.2 改动的文件

**`internal/channel/feishu/adapter.go`** —— `Send(case gateway.OutResult)` 重写 + 新增 `sendResultAsReply`:

```go
// (1) 新 helper
func (a *Adapter) sendResultAsReply(
    ctx context.Context, chatID, userMsgID, text string, replyOnly bool,
) error

// (2) Send(OutResult) case 改写
case gateway.OutResult:
    if msg.Result == nil { return errors.New(...) }
    text := msg.Result.Text
    if text == "" && !msg.Result.IsError { return nil }
    if r := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo); r != nil {
        _ = r.SetCompleted(ctx)
    }
    if msg.Result.IsError { text = "❌ " + text }
    return a.sendResultAsReply(ctx, msg.ChatID, msg.ReplyTo, text, false)
```

**`internal/channel/feishu/receipt_event.go`** —— 删 dedup 协调 + 删 EventResult case:

```go
// (1) case agent.EventResult 整段删除 (101-140)
//
//     adapter.go::Send(OutResult) 改走后,EventResult 不再进 receipt.Append。
//     eventToEntry 此 case 永不命中,defensive 也不需要 —— 删干净。
//
// (2) case agent.EventText dedup 比较键也不需要(不再是同一 surface 的竞争)
//
//     保持单文件 minimal 改动,不动 EventTool*/Usage/Compaction/Init。
```

### 2.3 保留不变的(确认无副作用)

- `gateway.OutboundMessage{Kind: OutResult, Result: ...}` —— 契约不变
- `gateway/translate.go::Translate(EventResult)` —— 不动
- `chatSession` 状态机 —— 不动
- `MessageReceipt.SetCompleted` / `Append(EventDone)` / `Append(EventError)` —— 不动
- `OutText` 路径(`case agent.EventText`)—— 不动
- `OutInit` / `OutUsage` 路径 —— 不动(receipt 仍承载 metadata)
- F-37 `splitMarkdownForDivs` —— 复用,只是换了 caller
- F-38 `task checklist` —— 不动
- F-thread-route(thinking/tool → thread reply)—— 不动

---

## 3. 测试覆盖

### 3.1 单元测试

| 文件 | 用例 | 断言 |
|---|---|---|
| `internal/channel/feishu/card_sanitize_test.go` | `TestSanitize_URL_NonHTTPToText` | `[x](relative)` → plain text `[x](relative)` |
| 同上 | `TestSanitize_URL_HTTPKeep` | `[x](https://...)` → 仍 link |
| 同上 | `TestSanitize_FenceMissingNewline_Injected` | `text<NL>\`\`\`code` 路径不变;`text\`\`\`code` 自动插 newline |
| 同上 | `TestSanitize_HeadingDemote_H2H6ToH5` | `## ` → `##### ` |
| 同上 | `TestSanitize_HeadingDemote_H1ToH4` | `# ` → `#### ` |
| 同上 | `TestSanitize_ImageStrip_NonFeishuKey` | `![x](https://...)` → 删除 |
| 同上 | `TestSanitize_ImageKeep_ImgPrefix` | `![x](img_xxx)` → 保留 |
| 同上 | `TestSanitize_CodeBlockProtected` | ``` ```go ... ``` ``` 内部 H1 行不被 demote |
| `internal/channel/feishu/result_render_test.go` | `TestContainsMarkdown_True` | 含 ` ``` `,`**`, `- ` 等 → true |
| 同上 | `TestContainsMarkdown_False_Plain` | 仅普通文字 → false |
| 同上 | `TestCountMarkdownTables_None` | 0 |
| 同上 | `TestCountMarkdownTables_Five` | 5 |
| 同上 | `TestCountMarkdownTables_Six` | 6 (超限) |
| 同上 | `TestBuildPostMdJSON_Shape` | output 包 zh_cn.content[0][0].tag="md" |
| 同上 | `TestBuildResultCardJSON_SingleDiv` | text < 1000 runes → 1 markdown element |
| 同上 | `TestBuildResultCardJSON_MultiDiv` | text > 1000 runes → N markdown elements,每个 ≤ 1000 runes |
| 同上 | `TestBuildResultPayload_NoMarkdown_UsesText` | 无 markdown → MsgTypeText |
| 同上 | `TestBuildResultPayload_LotsTables_UsesPost` | 6 表 → MsgTypePost |
| 同上 | `TestBuildResultPayload_Default_UsesCard` | 默认 → MsgTypeInteractive |
| 同上 | `TestTruncateRunes_KeepsUnder` | 2000 chars → ≤ maxRunes |
| `internal/channel/feishu/adapter_test.go` | `TestSend_OutResult_GoesToNewReply_NotReceipt` | mock SDK;调 Send(OutResult) → 1 次 sendContent,**0 次** receipt.Append |
| 同上 | `TestSend_OutResult_ClosesReceiptFirst` | mock receipt;验证 SetCompleted 调在 sendContent 之前 |
| 同上 | `TestSend_OutResult_LongText_UsesCard2_0` | text 5000 runes → MsgTypeInteractive + Card 2.0 envelope |
| 同上 | `TestSend_OutResult_VeryLongText_TruncatesToEnvelope` | text 10000 runes → 进入 28 KB budget 路径,log warn + truncate |
| 同上 | `TestSend_OutResult_EmptySkipped` | text="",!IsError → return nil,no send |
| 同上 | `TestSend_OutResult_IsError_PrefixedWithIcon` | text + "❌ " 前缀 |
| 同上 | `TestSend_OutResult_Orphan_NoUserMsgID_TopLevel` | userMsgID="" → 走 sendRawOutText fallback |
| `internal/channel/feishu/receipt_event_test.go` | `TestEventToEntry_NoEventResultCase` | 删除后:eventToEntry 没有 EventResult 命中;`(_, false)` 验证 |
| 同上 | 更新 `TestTruncateForLog_RuneAware`:删 EventResult 相关断言(只剩 EventText / EventError 用) |

### 3.2 集成测试(收尾阶段)

`internal/channel/feishu/adapter_test.go` 全量回归 `TestReceipt_*`,确认 OutText 仍 fold 进 receipt、OutInit/OutUsage 仍进入 footer、OutToolStart/End/Thinking 仍走 thread。

### 3.3 E2E(可选)

`internal/channel/feishu/e2e_test.go`(如未存在可暂缓):用 mock SDK 模拟完整 turn:

```
1. user 发消息
2. agent 流式 OutText × 3 (各 300 char, total 900)
3. agent OutResult 5000 char
4. mock SDK 记录所有 call

断言:
  - 1 SendMessageText(receiptFor 之前)
    (可能还有其他 thread reply / reaction 等)
  - 1 PatchMessage(... body 含 3 个 💬 entry + footer)
  - 1 SendCard/SendMessageText(... body 含 OutResult 5000 char, MsgTypeInteractive)
  - receipt.GetState() == Completed
```

---

## 4. 落地顺序

每步独立 commit,可单独 review + revert:

| Step | 内容 | 文件 | 风险 |
|---|---|---|---|
| 1 | **本文档**(`F-39-result-as-new-reply.md`) | `docs/feat/` | 零 |
| 2 | SPEC.md §0.x + §12 更新 | `docs/SPEC.md` | 零 |
| 3 | channel/feishu.md §13 + §12 更新 | `docs/channel/feishu.md` | 零 |
| 4 | `card_sanitize.go` 移植 | `internal/channel/feishu/card_sanitize.go`(新) | 低 |
| 5 | `result_render.go` 移植 | `internal/channel/feishu/result_render.go`(新) | 低 |
| 6 | `Send(OutResult)` + `sendResultAsReply` | `internal/channel/feishu/adapter.go`(改) | 中 |
| 7 | 删除 dedup + EventResult case | `internal/channel/feishu/receipt_event.go`(改) | 低 |
| 8 | `card_sanitize_test.go` 全覆盖 | 新文件 | 零 |
| 9 | `result_render_test.go` 全覆盖 | 新文件 | 零 |
| 10 | `adapter_test.go` 新增 OutResult 系用例 | 改 | 零 |
| 11 | `receipt_event_test.go` 调整 | 改 | 零 |

---

## 5. 与上下游契约

### 5.1 OutboundMessage 契约

不变。`Kind: OutResult` 仍存在,`Result *agent.ResultEvent` typed field 不动。Channel 自决渲染(specific 实现从"fold into receipt card"改为"independent reply")完全在 §1.4 边界规范允许范围。

### 5.2 ChatSession 状态机

不变。`OutResult` 仍由 `cs.EventCallback` 触发 → `gateway.Translate` → `channel.Send`。Channel 内的渲染分支改了,但状态机意义不变。

### 5.3 Tool thread 路由(F-37 tool-routing)

不动。F-thread-route 描述的是 thinking/tool/compaction,**已经独立**于 OutResult。F-39 加 OutResult 也独立后,两类 "独立 thread / 独立 reply" 平行。

### 5.4 多 div 拆分(F-37 multi-div)

仍用于 `buildResultCardJSON`(单 helper).F-37 在 receipt 内的多 div 拆不再服务于 OutResult,但仍服务于任何 OutText chunk > 1000 chars 的极端 case。`splitMarkdownForDivs` 函数保留,只是 caller 减少。

---

## 6. 后续工作(本文档不做)

- **退 splitMarkdownForDivs**(P1-1 in prior discussion):如果 telemetry 显示 envelope 真撞墙(实际低概率,因为 OutResult 不再 fold receipt,receipt 内 OutText ≤ 600 char/条 × ≤ 45 条 ≈ 27 KB 边缘),进一步考虑纯 envelope 防御 + fallback.
- **Header color 改 design**(P2):footer 的 inline `<text_tag>` / `<font>` 改用 `header.template = "neutral"/"red"`,对齐 cc-connect / openclaw-lark.
- **CardKit streaming**(可选):openclaw-lark 用了 [CardKit streaming API](https://github.com/larksuite/openclaw-lark/blob/main/src/card/streaming-card-controller.ts),飞书原生支持 server-side delta update,sender 只送 delta.这一改动需要飞书 [CardKit API](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/cardkit-introduction) 接入,跟 F-39 独立,作为更彻底 envelope 治本方案.

---

## 7. 不变式总结(本文档特殊要求,F-37 反转同款)

**F-39 反转 §0.1 / §0.2 旧结构,但保留:**

- OutboundMessage 不变(`Kind: OutResult`, `Result *agent.ResultEvent`)
- Gateway 不变(`Translate` 仍产 OutboundMessage)
- ChatSession 不变(emit MessageState 逻辑不动)
- `currentTurnUserMsgID` 单数锚点保留(`ReplyTo` 仍等于此)
- 1 turn : 1 anchor 不变式保留
- §1.4 边界规范保留(OutResult 字段是 typed `agent.ResultEvent`,Channel 自决 target)
- 抽象归抽象 / 具体归具体原则保留(独立 reply 是 Feishu 自治)

**为什么不叫 v2.0**:v1.3 核心不变式(职责隔离、Binding FSM owner、Receipt 自治)全部保留。F-39 是 Channel 自治范围内的渲染目标切换(从"fold 进 receipt card"到"独立 reply"),不影响 nightme 数据模型与 Gateway 契约。
