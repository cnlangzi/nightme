# Feishu Channel - 调研与迁移规划

> **Status**: design + implementation reference (v1.3; Channel-autonomous rendering per SPEC §0.1)
> **Scope**: nightme 内部 Feishu/Lark IM 适配器 (`internal/channel/feishu/*`)
> **目的**: 描述 Feishu 侧 rolling-log card 实现策略 -- 收到 `OutboundMessage{ReplyTo: userMsgID}` 时如何 cold-create / PATCH / 终态 card。
> **Related docs**:
> - [F-08-channel-abstraction.md](../feat/F-08-channel-abstraction.md) - 5-method Channel interface(v1.3 缩水)
> - [F-25-rolling-log.md](../feat/F-25-rolling-log.md) - rolling-log UX 整体协议
> - [F-26-gateway-hub.md](../feat/F-26-gateway-hub.md) - v1.1 Gateway ↔ Channel 边界(历史,v1.3 改)
> - [F-22-feishu-onclick-registration.md](../feat/F-22-feishu-onclick-registration.md) - app 鉴权
> - [F-31-message-state.md](../feat/F-31-message-state.md) - progress indicator(独立于 receipt)
> **官方文档**:
> - [Create JSON message content](https://open.feishu.cn/document/server-docs/im-v1/message-content-description/create_json) - 顶层卡片信封
> - [Card JSON 2.0 components](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-json-v2-components/component-json-v2-overview) - 组件列表(`div` / `markdown` / `hr` / `note` 等)
> - [Update sent message card (PATCH)](https://open.feishu.cn/document/server-docs/im-v1/message-card/patch) - 卡片原地更新 API

## 1. 背景:为什么从 text 切到 card

当前 nightme 的 receipt 走纯文本路径(`msg_type: "text"`):
- `internal/channel/feishu/adapter.go:890 SendMessageText` 编码为 `{"text": "..."}` 后 `sendContent` 发出
- `internal/channel/feishu/receipt.go:515 renderLocked` 每个事件 `SendMessageText` 发一条新消息
- 没有 footer / 没有按钮 / 表格需要 markdown hack 才能近似

切到 interactive card 可以:
1. **footer 行** - 展示 `Agent · X | Model · Y | Provider · Z` 这类元数据(对齐 OpenClaw 风格,见 §2)
2. **按钮 / action** - 一键确认、二次确认、复制 session id 等交互
3. **结构化展示** - 工具调用折叠为 `collapsible_panel`、长输出折叠、表格、彩色状态等
4. **原地更新** - 用 PATCH API 改一张卡,而不是发 N 条消息,降低噪音

## 2. OpenClaw 的"card note footer" 调研

### 2.1 这是什么

OpenClaw 在每张 Feishu 卡片底部加一行灰色文字,展示当前 agent 的身份与运行上下文:

```
Agent: main | Model: glm-5 | Provider: tencentcodingplan
```

不是用户写的"备注",而是**运行时自动生成**的卡片 footer。

### 2.2 实现来源(OpenClaw)

- **生成位置**: `extensions/feishu/src/reply-dispatcher.ts::resolveCardNote`
  - 引入 commit: https://github.com/openclaw/openclaw/commit/df3a247db2a90da2a2593f85bdd5ef07f6b39a91
  - JSDoc: "Build a card note footer from agent identity and model context."
  - 硬编码拼接 `Agent: <name>` + ` | Model: <model>` + ` | Provider: <provider>`
  - **不可配置**(issue #59360 指明: "There is no configuration to disable it")
- **传入路径**:
  - 流式卡 start/close: `reply-dispatcher.ts:393, :437` 接收/重算 `note`
  - 结构化卡: 传入 `sendStructuredCardFeishu(..., { note: cardNote })`
- **渲染位置**: `extensions/feishu/src/send.ts:768`
  ```ts
  if (options?.note) {
    elements.push({ tag: "hr" });
    elements.push({ tag: "markdown", content: `<font color='grey'>${options.note}</font>` });
  }
  ```
  即:`<hr>` + 灰色 markdown。这是 v2 card 风格。

### 2.3 OpenClaw issue #59360 - root cause

- **Title**: "Feishu card message footer causes agent name to appear at message start (Markdown definition list parsing)"
- **现象**: Feishu 的 Markdown 渲染器把 `Agent: main | Model: ... | Provider: ...` 解析成 **Markdown 定义列表**(`key: value`),把第一项的 value(`main`)hoisting 到消息开头
- **结果**: 用户看到的卡片正文最上面突然多一行 `main`(agent 名),footer 反而被解读成普通段落
- **复现**: 发送任意包含 `Key: value | Key2: value2` 的灰色 markdown 卡片即可触发
- **关闭状态**: closed as not planned, 2026-07-20
- **未合并修复**: [PR #84122](https://github.com/openclaw/openclaw/pull/84122) - 把 `Agent: ` 改成 `Agent · `(中点),让渲染器认不出是定义列表
  - 描述: "Feishu's card markdown renderer parses 'Agent: name' as definition-list syntax and hoists the agent name to the top of the rendered message. Switch the key/value separator from ': ' to ' · ' so the footer stays in the footer."

### 2.4 我们的截图

用户截图(2026-06-11)中的红色框:

```
─────────────────────────────────
Agent: main | Model: MiniMax-M2.7 | Provider: minimax
```

-- 与 OpenClaw 的 card note footer 一致,但 nightme 当前的 text 路径**不会**输出这种卡片(没 hr、没灰色),所以截图来自**别的工具**(可能是 OpenClaw/同款渲染)。**这个 bug 在 nightme 切到 card 之前不会触发**;切换后必须规避(见 §6)。

## 3. Feishu 卡片 schema 摘要

### 3.1 顶层信封(参考 create_json 文档)

```json
{
  "msg_type": "interactive",
  "card": {
    "header": { "title": { "tag": "plain_text", "content": "..." }, "template": "blue" },
    "config":  { "wide_screen_mode": true, ... },
    "elements": [ ... ]
  }
}
```

### 3.2 footer / note 的两种实现

| 方案 | JSON | 兼容性 |
|---|---|---|
| **v1 `note` element** | `{ "tag": "note", "elements": [{ "tag": "plain_text", "content": "..." }] }` | Card v1,大多数场景可用;Card 2.0 官方组件列表里**没有**该 tag |
| **v2 neutral markdown + `<hr>`** | `elements.push({tag:"hr"}); elements.push({tag:"markdown", content:"<text_tag color='neutral'>...</text_tag>"});` | OpenClaw / 主流 v2 实践,**推荐** |

**重要**: Feishu `lark_md` **不支持** `<font color='grey'>`。Feishu 官方允许的 inline 颜色用 `<text_tag color='...'>`,允许值:`neutral`、`blue`、`turquoise`、`lime`、`orange`、`violet`、`indigo`、`wathet`、`green`、`yellow`、`red`、`purple`、`carmine`。`neutral` 视觉上接近灰色。OpenClaw `send.ts:768` 写的 `<font color='grey'>` 实际渲染靠 Feishu 容错 -- 本项目**严格用 `<text_tag color='neutral'>`**。

参考: `gcmsg/openclaw-feishu/src/menu.ts` 用前者(纯 v1),`openclaw/openclaw/extensions/feishu/src/send.ts:768` 用后者(纯 v2)。

### 3.3 常用元素

| tag | 用途 |
|---|---|
| `div` | 块容器,内嵌 `lark_md` / `plain_text` |
| `markdown` | Markdown 段(支持 `<font>` / `<at>` 等内联标签) |
| `hr` | 水平分隔线 |
| `note` | v1 footer 文字 |
| `action` + `button` | 按钮(action 数组元素) |
| `collapsible_panel` | 折叠面板(放长输出) |
| `image` / `img_combination` | 图片 |

### 3.4 更新策略

`PATCH /im/v1/messages/{message_id}` **整体替换** `card` 字段 -- 不能只改一个 element。所以 nightme 的"原地编辑 receipt"语义就是:**每次状态变化都重新构建完整 card body 然后 PATCH**。

**SDK 提醒**: `lark-oapi-go/v3` 提供两个不同的方法,**`Update` 只能改文本/富文本,不能改卡片**;**卡片必须用 `Patch`**。两个方法对应不同的 HTTP method:
- `Update` (PUT `/open-apis/im/v1/messages/:id`) - 仅文本/富文本。SDK 注释: "当前仅支持编辑文本和富文本消息"
- `Patch` (PATCH `/open-apis/im/v1/messages/:id`) - 卡片/富文本都支持,5 QPS 频控,30 KB body 上限

`update_multi` 不是独立接口,是 card `config` 里的一个 flag(`"update_multi": true`),让卡变成"共享卡"在所有接收方同步更新。nightme 单聊场景不启用。

## 4. nightme 当前现状(text 模式)

### 4.1 关键代码位置

| 文件 | 作用 |
|---|---|
| `internal/channel/feishu/adapter.go:890 SendMessageText` | 发 `msg_type: "text"` 消息,返回 messageID |
| `internal/channel/feishu/adapter.go:765 buildInteractiveCard` | 已有的 OutCard 卡片构建(permission card),仅用于 OutCard 路径 |
| `internal/channel/feishu/adapter.go:1066 sendContent` | 透传到 lark client,支持任意 msgType |
| `internal/channel/feishu/receipt.go:515 renderLocked` | 每个事件 `SendMessageText` 发新消息 |
| `internal/channel/feishu/receipt.go:455 formatLocked` | 构造 plain text body(header + entries) |
| `internal/channel/feishu/receipt.go:188 headerLine` | 单行 header(⏳ / 🔄 / ✅) |
| `internal/gateway/messages.go:162 OutCard` / `:254 Card` | 已存在 interactive card 的抽象类型 |
| `internal/gateway/messages.go:182 OutInit` | 携带 `session_id` + `model`(无 `agent_name` / `provider`) |

### 4.2 现状(切到 card 之前)

- 用户看到的 receipt 是一连串**短文本消息**(`⏳ 等待中` / `🔄 工具: Bash` / `✅ 已完成`)
- 切到 card 后,这些短消息将合并为**一张可原地 PATCH 的卡片**
- 已有的 `OutCard` 路径独立(permission card 走 `buildInteractiveCard` → `sendContent`),不影响 receipt 切换

## 5. 迁移方案:receipt → interactive card

### 5.1 目标

让 receipt message 是**一张 interactive card**:
- body 是滚动的 log entries(`div` + `markdown`)
- 末尾 `<hr>` + 中性色 footer(`markdown` + `<text_tag color='neutral'>`)
- header(可选): 状态 emoji + 时间戳(`✅ 已完成 12:34:56`)
- 状态变化/心跳通过 `PATCH im/v1/messages/{id}` 整体更新

**本期 minimal scope**: 不引入新元数据。card body 用现有 receipt 状态(state + entries + last event time)。agent_name / provider 透传 **延期** 到后续 PR(见 §9.4)。

### 5.2 接口与数据流

```
Adapter.Send(OutboundMessage)
   │
   ├── OutText / OutResult / OutUsage / OutToolStart / OutToolEnd / OutThinking / OutCompaction
   │     → receipt.Append(AgentEvent) → renderLocked(ctx) → SendCard(首次) / PatchMessage(后续)
   │
   ├── OutInit
   │     → receipt.Append(InitEvent) → renderLocked → Patch(刷新 footer)
   │
   └── OutCard (permission)
         → sendContent(interactive, buildInteractiveCard(...))   ← 不变
```

### 5.3 MessageReceipt 字段扩展

```go
type MessageReceipt struct {
    // ... 现有字段
    cardMsgID string  // 首次 SendCard 后记录;后续 PatchMessage 用它
}
```

**本期不**新增 `agentName` / `provider` / `model` 字段 -- foot note 全部从已有 state 字段组装(state.String() + eventCount + lastEventAt)。这样不需要触动 `agent.InitEvent` / `gateway/translate.go` / `OutboundMessage.Meta` 任何上游。

`renderLocked` 替换为新的 card-first 策略:
- 第一次:`sendContent(chatID, MsgTypeInteractive, buildReceiptCard(r))` 拿到 messageID
- 之后:`PatchMessage(messageID, buildReceiptCard(r))` 整体替换

`buildReceiptCard(r)` 产出(伪代码,见 §9.3 真实签名):
```go
func buildReceiptCard(r *MessageReceipt) (string, error) {
    elements := []any{
        // header(状态 + 时间戳)
        {"tag": "div", "text": {"tag": "lark_md", "content": r.state.headerLine(r)}},
    }
    if r.evicted > 0 {
        elements = append(elements, map[string]any{
            "tag":     "markdown",
            "content": fmt.Sprintf("<text_tag color='neutral'>...(前 %d 条已省略)</text_tag>", r.evicted),
        })
    }
    for _, e := range r.entries {
        elements = append(elements, map[string]any{
            "tag": "div",
            "text": map[string]any{
                "tag":   "lark_md",
                "content": e.Icon + " " + e.Text,
            },
        })
    }
    // foot note: 用现有 state 数据;无内容时整段省略
    if note := r.state.footLine(r); note != "" {
        elements = append(elements, map[string]any{"tag": "hr"})
        elements = append(elements, map[string]any{
            "tag":     "markdown",
            "content": fmt.Sprintf("<text_tag color='neutral'>%s</text_tag>", note),
        })
    }
    card := map[string]any{
        "config":   map[string]any{"wide_screen_mode": true},
        "elements": elements,
    }
    env := map[string]any{"card": card}
    b, _ := json.Marshal(env)
    return string(b), nil
}

func (s ReceiptState) footLine(r *MessageReceipt) string {
    if r == nil { return "" }
    parts := []string{s.String()}
    if r.eventCount > 0 {
        parts = append(parts, fmt.Sprintf("%d entries", r.eventCount))
    }
    if !r.lastEventAt.IsZero() {
        parts = append(parts, r.lastEventAt.Format("15:04:05"))
    }
    return strings.Join(parts, " · ")
}
```

### 5.4 PATCH 路径

`internal/channel/feishu/adapter.go` 新增(复用 `sendContent` / `sendFunc` / `sendViaLark` 的 dispatch 模式):
```go
func (a *Adapter) UpdateMessage(ctx context.Context, messageID, content string) error
func (a *Adapter) SendCard(ctx context.Context, chatID, content string) (string, error)
```

`UpdateMessage` 调 `larkClient.Im.V1.Message.Patch(...)`(注意是 **Patch**,不是 Update -- Update 只能改文本)。`SendCard` 是 `sendContent(chatID, larkim.MsgTypeInteractive, content)` 的薄包装。

PATCH 失败时不降级为新消息 -- 简单实现,日志告警即可;下次 `renderLocked` 仍然 PATCH 同一个 messageID。

### 5.5 迁移步骤(本期)

1. **Phase 1 - adapter 层支持**: 加 `SendCard` / `UpdateMessage`(内部调 Patch),`buildReceiptCard` + `footLine` 静态实现
2. **Phase 2 - receipt 切换**: `renderLocked` 改为 first-send-then-PATCH;`MessageReceipt` 加 `cardMsgID`;`evictOverflowLocked` 扩展为字节+元素双约束
3. **Phase 3 - 测试更新**: `mockReceiptBot` 加 SendCard / PatchMessage stubs;`TestReceipt_PerEventFreshMessage` 改为断言 PATCH 行为;新增 `TestFootLine_*` / `TestBuildReceiptCard_*` 系列
4. **Phase 4 - 文档收尾**: 在 §11 记录本期落地状态;把"未做"留给 follow-up issue

**OutInit / `agent_name` / `provider` 透传** -- **DEFERRED**(见 §9.4)。当后续 PR 加这三个字段时,`buildReceiptCard` 只需要把它们 append 到 `footLine` 后面,不需要再次动 receipt / adapter 主体。

## 6. 已知坑(从 OpenClaw 学到)

### 6.1 冒号 → 中点(防 hoisting,前瞻要求)

参考 OpenClaw PR #84122。**所有 footer / card note 的 `key: value` 必须改成 `key · value`**,否则 Feishu 渲染器把 `key: value` 解析为 Markdown 定义列表,把第一项的 value hoisting 到卡片正文开头。

**本期 foot note 不会出现 `key: value` 形态**(内容是 `state · N entries · HH:MM:SS`),但**约束记在 §9.3 后续加 agent info 时必须遵守**。中点字符用 U+00B7(`·`),不是句号或星号。

### 6.2 `<text_tag color='neutral'>` 而非 `<font color='grey'>`

Feishu `lark_md` **不支持** `<font color='grey'>`。**严格用 `<text_tag color='neutral'>`**(允许值: `neutral`, `blue`, `turquoise`, `lime`, `orange`, `violet`, `indigo`, `wathet`, `green`, `yellow`, `red`, `purple`, `carmine`)。OpenClaw `send.ts:768` 写的 `<font color='grey'>` 靠 Feishu 容错 -- 本项目不依赖容错。

### 6.3 SDK: Patch ≠ Update

`lark-oapi-go/v3` 提供的 `Message.Update`(PUT) **只支持文本/富文本**,**不能改卡片**。**改卡片必须用 `Message.Patch`**(PATCH)。两个方法签名相似,容易踩坑。详见 §3.4。

### 6.4 PATCH 是整体替换

`PATCH /im/v1/messages/{id}` 的 `card` 字段是**整个 card 对象**,不是 diff。要保留元素(折叠面板的展开状态等)需要在 client 端维护当前状态,然后每次 PATCH 把所有 elements 重新构建。本期 receipt 没有折叠面板,不受影响。

### 6.5 心跳(Heartbeat)行为变化

`F-23-heartbeat.md` 当前实现是周期性重新 `SendMessageText` 同一 header。切到 card 后,心跳 = 周期性 PATCH 同一张卡的 card body(刷新 header 时间戳 + foot note)。频率/阈值不变。

### 6.6 MessageState 与 Card 共存(v1.3 重构)

**v1.3 变更**:MessageState(reaction emoji 轨道)与 Receipt(card body 轨道)解耦为两个独立的 channel 实现。

#### 6.6.1 两个轨道

| 轨道 | 源 | 抽象事件 | 渲染目标 | Feishu 实现 |
|---|---|---|---|---|
| **MessageState** | ChatSession lifecycle | `OutboundMessage{Kind: OutMessageState, Meta: {message_id, state}}` | **userMsgID** | `AddReaction(userMsgID, emoji_type)` |
| **Card Body** | Rolling-log receipt (v1.3 实现;Channel 自治) | `OutboundMessage{ReplyTo: userMsgID}` → Channel.Send → 内部 cold-create / PATCH | replyMsgID / cardMsgID | `SendCard / PatchMessage` |

两者**完全独立**:
- 一个失败不影响另一个 (MessageState 渲染失败仅 log warn,不阻塞 card body)
- 都按 userMsgID / chatID 索引,但服务不同语义
- 详见 [`docs/feat/F-31-message-state.md`](../feat/F-31-message-state.md) 与 `SPEC.md §2.5`

#### 6.6.2 MessageState 渲染实现

```go
// internal/channel/feishu/adapter.go - Send dispatcher 新增 case
case gateway.OutMessageState:
    messageID, _ := msg.Meta["message_id"].(string)
    if messageID == "" {
        return errors.New("feishu: OutMessageState missing message_id")
    }
    state, ok := msg.Meta["state"].(receipt.MessageState)
    if !ok {
        return errors.New("feishu: OutMessageState missing state")
    }
    emoji := mapStateToFeishuEmoji(state)
    if emoji == "" {
        return nil    // 未知 state 静默 drop
    }
    _, err := a.AddReaction(ctx, messageID, emoji)
    return err
```

#### 6.6.3 state → emoji_type 映射

| `MessageState` | emoji_type(飞书预定义) | 用户视觉 |
|---|---|---|
| `StateReceived` | `OneSecond` | ⏳ |
| `StateForwarded` | `OnIt` | 🔄 |
| `StateDone` | `DONE` | ✅ |
| `StateError` | `THUMBSUP` | ❌ (closest 预定义 indicator) |

**重要**:必须用飞书预定义 `emoji_type` 标识符,不是 unicode。传 unicode `⏳` 给飞书 reaction API 返回 `99992354 data not found`(reaction service 只识别预定义集合)。

#### 6.6.4 内部 idempotency

`Adapter` 维护 `messageStates map[string]receipt.MessageState`(userMsgID → 上次渲染的 state)。同 state 跳过 AddReaction 调用,避免网络抖动。

#### 6.6.5 append-only 语义

飞书 reaction API 是 append-only:每次 AddReaction 加新 emoji,不删老 emoji。这意味着 ⏳ → 🔄 → ✅ 在用户消息上**堆叠**为 3 个 emoji,形成完整状态轨迹。这是飞书平台特性,channel adapter 不主动删。

如果未来需要删,实现 `OutMessageStateRemoved` 事件 + `DeleteReaction(msgID, reactionID)` 路径(参考 adapter.Send 中 OutReactionRemoved case 实现)。

#### 6.6.6 渲染失败

`AddReaction` 失败时 log warn,返回 error 由 caller(`gateway.OnMessageState`)处理。**永不阻塞** ChatSession lifecycle 或消息处理主流程。

### 6.7 30 KB body 限制(本期放宽)

Feishu card body 上限 **30 KB**(Create 和 PATCH 相同)。本期 `replyMaxBytes` 从 4 KiB 放宽到 **24 KiB**(留 6 KiB 头空间)。`evictOverflowLocked` 同时受字节和元素数(50)两个约束,先触发的先 evict。

### 6.8 元素数 50 限制

`body.elements` 上限 **50 个**。本期布局预算:1 header + 1 evicted marker(可选)+ ≤47 entries + 1 hr + 1 footer = 50。`evictOverflowLocked` 把 entries 限制为 ≤47,新事件到达时驱逐最老。

### 6.9 `note` 元素的 v2 兼容性

Card 2.0 官方组件列表**没有 `tag: "note"`**。我们走 v2 风格（`<hr>` + 中性色 markdown），**不要**用 v1 的 `note` 元素。

### 6.10 Mention 前缀 strip（F-watch 增量）

**问题**：飞书群聊里，@ bot 后的消息文本以 `@_user_N ` 开头的占位符表示 mention（Feishu SDK 中以 `Mentions[].Key` 形式出现在 `message.Content` 里）。如：

```
@_user_1 /cwd /tmp
```

`ParseCommand` 要求 `strings.HasPrefix(trimmed, "/")`（`internal/gateway/parser.go:36`），这条文本会以 `@` 开头，被判为 `ErrParseFailure` → slash command **拦截失败**。

**方案**：`handleMessage` 构造 `channel.Message` 前，strip 开头的 mention 前缀。

| 场景 | Text 原始 | Text strip 后 | HasMention |
|------|----------|--------------|------------|
| 群聊 @ bot | `@_user_1 /watch on` | `/watch on` | `true` |
| 群聊 @_all | `@_all /cwd /a` | `/cwd /a` | `true` |
| 群聊多个 mention 开头 | `@_all @_user_1 hello` | `hello` | `true` |
| 群聊无 mention | `hello bot` | `hello bot` | `false` |
| 群聊 mention 在中段 | `look at this @_user_1 bug` | `look at this @_user_1 bug`（不剥）| `true` |
| DM | `hello` | `hello` | `true`（DM 永远 true）|

**实现位置**：`internal/channel/feishu/adapter.go::handleMessage`，`extractAttachments` 之后、构造 `channel.Message` 之前。

```go
text, hasMention := stripAndDetectMention(
    text, message.Mentions, a.getBotOpenIDCached(), stringValue(message.ChatType),
)
```

**strip 规则**：
1. 只剥**开头**连续出现的 mention 前缀（循环跳过中间的非 mention 文本，例如 `@_all @bot hello` → `hello`）
2. 中段的 mention 不动（保留用户原始语义）
3. 前缀必须是 mention + 至少一个空白字符（空格 / Tab / 全角空格 / `\u00A0`），避免误删正文中以 `@` 开头的单词（但正文中以 `@_user_N` 开头的字串不会被误判，因为正文中不会出现在最前面）
4. `@_all` 始终 strip（无需 bot open_id）

**`hasMention` 计算**：
- DM（`chat_type == "p2p"`）→ **永远 `true`**
- group/topic_group → `mentions` 列表中含 bot open_id 或 `@_all` 时 `true`
- `chat_type` 为空 / 未知 → 默认 `true`（安全 fallback，宁可多处理）

> **DM 不变式（锁死）**：DM 消息 `HasMention` 必须永远是 `true`。这是 F-watch 的核心不变式 ——只有这条不变式成立，gateway dispatcher 才能放心地 drop 非 mention 群消息而不误伤 DM。由 `TestComputeHasMention_DMInvariant` （adapter 层）+ `TestDispatchInbound_WatchModeGate_DMInvariant`（gateway 层）两个测试锁死，任一 regressed 都会被 CI 拦住。

**bot open_id 获取**：调 SDK `a.larkClient.GetBotIdentity(ctx)`（`channel/channel.go:152`），30 分钟 TTL cache 由 SDK 内部管理。第一次消息进来 cache miss → 同步 fetch；后续命中 cache，零延迟。fetch 失败 → 记 log，`HasMention` 退化为 `false`（保守策略：DM/group 都当 group 处理）。

**ChatSession 侧接入**：`/watch on` / `/watch off` 控制 `ChatSession.WatchMode()`；Gateway dispatcher 拿 `Message.HasMention` + `cs.WatchMode()` 决定 drop 或 pass。Channel adapter **不读** `ChatSession` —— 详细职责划分见 `docs/SPEC.md §3.1.1`。

**测试覆盖**：
- 群消息 @bot / @_all → strip + HasMention=true
- 群消息无 mention → 不 strip + HasMention=false
- 群消息多 mention 串前 → 全剥
- 群消息 mention 中段 → 不动
- DM → 不 strip + HasMention=true（不调 bot identity）
- bot identity cache miss + fetch 失败 → fallback 到 HasMention=false + log warn

### 6.11 WatchMode per-chat 群消息全收（F-watch 增量）

**背景**：飞书默认 `im:message.group_at_msg:readonly` 只让 bot 收 @ 自己的消息。nightme F-watch 反转：bot 默认收全群（需要 `im:message.group_msg` scope，默认在 `DefaultAddons()` 里），由 `ChatSession.WatchMode` 在 nightme 侧决定要不要处理。

**实现位置**：
- `internal/chatsession/chat_session.go`：`WatchMode` 类型 + getter / setter
- `internal/gateway/handlers_watch.go`：`/watch on|off` slash command handler
- `internal/gateway/gateway.go::Handle`：`HasMention` + `WatchMode` gate

**`/watch` slash command**：

| 调用 | 行为 |
|------|------|
| `/watch on` | `ChatSession.WatchMode = WatchModeAll`；持久化；reply "watching all messages in this chat" |
| `/watch off` | `ChatSession.WatchMode = WatchModeMention`；持久化；reply "watching mentions only (default)" |
| `/watch`（无参）| 显示当前 mode + 简短说明 |

**DM 为 no-op**：DM 下 `HasMention` 永远为 true，gate 永不触发；运行 `/watch on/off` 状态正常写入但不影响消息处理（DM 全收）。文档在 `docs/feat/F-22-feishu-onclick-registration.md` §4 Edge cases 说明。

**飞书 scope 默认开启**：`DefaultAddons()` 始终包含 `im:message.group_msg`（不带 `:readonly` —— bot 需要回复到群里）。**不**走 CLI flag opt-in，由 Devin 拍板（2026-08-03）。

**详细设计**：见 `docs/SPEC.md §3.1.1` + §9 Q-W1/Q-W2/Q-W3/Q-W4。

## 7. 验收 / 测试(本期 minimal scope)

- 单元: `buildReceiptCard` 产出合法 JSON,`elements` 末尾是 `<hr>` + `<text_tag color='neutral'>` foot note;`footLine` 为空时整段省略
- 单元: `footLine` 在 `state=executing, eventCount=5, lastEventAt=14:32:05` → `"executing · 5 entries · 14:32:05"`;`eventCount=0` 时不渲染 `0 entries`
- 单元: `MessageReceipt.renderLocked` 第一次调 → `SendCard`;之后 → `PatchMessage` 同一个 messageID;**不再**调 `SendMessageText`
- 单元: 元素数 = 60 entries → 47 entries + `...(前 N 条已省略)` 标记
- 单元: 字节数 = 收到超大 entries → 驱逐最老直到 < 24 KiB
- 单元: 回归 `mockReceiptBot.AddReaction` 不变(v1.3 后,reaction 由 MessageState FSM 触发,仍走 userMsgID,但已从 MessageReceipt 解耦到 Adapter 顶层)
- 集成: 端到端: user message → 一张 receipt card(后续 agent event 不再发新消息,而是 PATCH);最终状态 `✅` 出现在 header;foot note 随状态变化
- 回归: permission card (`OutCard`) 不受影响,继续走原 `buildInteractiveCard`
- 回归: heartbeat (`F-23`) 行为对齐,只是底层从 SendMessageText 变为 PATCH

## 8. 参考资料

- OpenClaw issue #59360: https://github.com/openclaw/openclaw/issues/59360
- OpenClaw PR #84122(middle-dot fix,未合): https://github.com/openclaw/openclaw/pull/84122
- OpenClaw `resolveCardNote` 引入 commit: https://github.com/openclaw/openclaw/commit/df3a247db2a90da2a2593f85bdd5ef07f6b39a91
- OpenClaw card 渲染位置: https://github.com/openclaw/openclaw/blob/1b8b8500cee077d7ac7927def0f566febf7dacb8/extensions/feishu/src/send.ts#L768
- 官方 lark plugin(`openclaw-lark`)的 `FeishuFooterConfig` 模式: https://github.com/larksuite/openclaw-lark
- 社区 fork(`gcmsg/openclaw-feishu`)用 v1 `note` 元素: https://github.com/gcmsg/openclaw-feishu/blob/main/src/menu.ts
- 飞书 create_json 文档: https://open.feishu.cn/document/server-docs/im-v1/message-content-description/create_json
- 飞书 Card 2.0 组件总览: https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-json-v2-components/component-json-v2-overview
- 飞书 PATCH card: https://open.feishu.cn/document/server-docs/im-v1/message-card/patch
- 飞书 markdown 内联标签规范: https://open.feishu.cn/document/common-capabilities/message-card/message-cards-content/using-markdown-tags
- 飞书 lark_md 元素 / 行长度 / 字符限制: https://open.larkoffice.com/document/server-docs/im-v1/message-card/message-card-content/message-card-text-element
- [`docs/feat/F-31-message-state.md`](../feat/F-31-message-state.md) - v1.3 MessageState 抽象事件,本文件 §6.6 是其 feishu-specific 实现补充
- [`docs/SPEC.md`](../SPEC.md) §2.5 - MessageState 架构概述

## 9. Implementation plan(本期落地,minimal scope)

本期**只做**结构性迁移(receipt 从 text 改 card),不动元数据管线。**agent_name / provider / model 在 foot note 的展示 = 后续 PR**(§9.4)。

### 9.1 改动文件清单

| 文件 | 改动 |
|---|---|
| `internal/channel/feishu/adapter.go` | 新增 `updateMessageFunc` 字段、`patchViaLark`、`patchContent`、`UpdateMessage`、`SendCard`、`buildReceiptCard` |
| `internal/channel/feishu/receipt.go` | 改 `renderLocked` 走 SendCard/PatchMessage;`receiptBot` 接口加 `SendCard`/`PatchMessage`;`MessageReceipt` 加 `cardMsgID`;`ReceiptState` 加 `footLine`;`replyMaxBytes` 改 24576 |
| `internal/channel/feishu/receipt_test.go` | `mockReceiptBot` 加 stubs;新增 / 重写 §7 列出的测试 |
| `internal/channel/feishu/adapter_test.go` | 加 SendCard / PatchMessage 走通测试 |

**不改**: `internal/agent/agent.go`、`internal/bridge/claudecode/stream.go`、`internal/gateway/translate.go`、`internal/gateway/messages.go`、`internal/bridge/acp/*`、`internal/bridge/pty/*`、`internal/bridge/sdk/*`、`cmd/nightme/*`。

### 9.2 关键代码契约

```go
// internal/channel/feishu/receipt.go
type receiptBot interface {
    AddReaction(ctx context.Context, msgID, emoji string) (string, error)
    UpdateMessage(ctx context.Context, messageID, text string) error  // 改:内部走 Patch
    SendMessageText(ctx context.Context, chatID, text string) (string, error)
    SendCard(ctx context.Context, chatID, cardJSON string) (string, error)
    PatchMessage(ctx context.Context, messageID, cardJSON string) error
}

type MessageReceipt struct {
    // ... 现有字段不动 ...
    cardMsgID string  // 新增:首次 SendCard 后记录
}

func (r *MessageReceipt) renderLocked(ctx context.Context) error {
    r.appendReactionLocked(ctx, r.state.Emoji())
    body, err := buildReceiptCard(r)
    if err != nil { return err }
    if r.cardMsgID == "" {
        msgID, err := r.bot.SendCard(ctx, r.chatID, body)
        if err != nil { return fmt.Errorf("... create card: %w", err) }
        r.cardMsgID = msgID
        return nil
    }
    return r.bot.PatchMessage(ctx, r.cardMsgID, body)
}
```

```go
// internal/channel/feishu/adapter.go
func (a *Adapter) SendCard(ctx context.Context, chatID, content string) (string, error) {
    // 透传 sendContent + larkim.MsgTypeInteractive
}
func (a *Adapter) UpdateMessage(ctx context.Context, messageID, content string) error {
    // 走 patchContent + larkim.NewPatchMessageReqBuilder + Message.Patch
}
```

### 9.3 Foot note 格式(本期,仅现有数据)

`<text_tag color='neutral'>{state} · {N entries} · {HH:MM:SS}</text_tag>`

示例:
- 等待中 + 0 事件 → 无 foot note(整段省略)
- 处理中 + 5 事件 + 14:32:05 → `<text_tag color='neutral'>executing · 5 entries · 14:32:05</text_tag>`
- 已完成 + 10 事件 + 14:35:00 → `<text_tag color='neutral'>completed · 10 entries · 14:35:00</text_tag>`

字段缺失时跳过,**绝不**出现连续分隔符(不写 `executing · · 14:32:05`)。

### 9.4 后续 PR(已规划,本期不做)

当 `agent_name` / `provider` 透传落地后,`buildReceiptCard` 的 foot note 升级为:

```
<text_tag color='neutral'>executing · 5 entries · 14:32:05 | Agent · main · Model · claude-sonnet-4-5 · Provider · claudecode</text_tag>
```

所有 `key: value` 段必须用中点 `·`(防 hoisting,见 §6.1)。**留给下一份 PR 的契约**:
- `agent.InitEvent` 新增 `AgentName` / `Provider` 字段
- `internal/bridge/claudecode/stream.go` 在 system/init 翻译处填充
- `internal/gateway/translate.go` 写 `Meta["agent_name"]` / `Meta["provider"]`
- `MessageReceipt` 新增对应字段,`Append(InitEvent)` 写入
- `footLine` 拼上 ` · Agent · X · Provider · Y`(model 已有,补中点)

**这部分改动是叠加的,不动 card 渲染主体。**

## 10. Known limits(本期须记住)

| Limit | 值 | 来源 |
|---|---|---|
| card body 字节数 | 30 KB(Create / PATCH 相同) | SDK `resource.go:1381` 注释 |
| card elements 数 | 50 | Feishu card 文档 |
| `lark_md` 单行 | 1000 chars | Feishu card 文档 |
| `lark_md` 总长 | 4000 chars | Feishu card 文档 |
| `plain_text` | 500 chars | Feishu card 文档 |
| `div` text | 1000 chars | Feishu card 文档 |
| PATCH 频控 | **5 QPS / message** | SDK 注释 |
| 消息可 PATCH 期限 | 14 天 | Feishu PATCH 文档 |
| `update_multi` 共享卡 | 仅当 `config.update_multi = true` 创建时启用 | Feishu card 文档 |

**本期防御**:
- `replyMaxBytes = 24 KB` -- 留 6 KB 头空间
- entries 上限 = 47 -- 留 3 元素给 header/hr/footer
- 5 QPS 频控靠 receipt 单线程 `renderLocked`(已串行)+ 实际 agent event 频率远低于 5/s,不主动限流

**超出限制时的降级**: `PatchMessage` 失败 → 记录日志 + 下次 render 仍 PATCH 同一 messageID;不重发新消息以避免重复 receipt。**已知风险**: 持续失败 → 卡片一直不更新,直到 receipt 销毁。后续可加重试/降级到"再发新卡"。

## 11. Feishu msg_type 全集(参考)

Feishu IM API 官方支持的顶层 `msg_type`(参考 [create_json 文档](https://open.feishu.cn/document/server-docs/im-v1/message-content-description/create_json))。`internal/channel/feishu/adapter.go` 走 `sendContent(chatID, msgType, content)` 任意 msg_type 都通;**当前 nightme 只用到 2 种**(`text` + `interactive`),其余 9 种是未来扩展的候选。

| `msg_type` | `content` 结构 | 用途 | nightme 现状 | 未来是否用 |
|------------|----------------|------|--------------|------------|
| `text` | `{"text":"..."}` | 纯文本(支持 `<at>` / 超链接 / 4 种 inline 样式) | ✅ `OutCommandReply` | 是 |
| `post` | `{"zh_cn":{"title","content":[[{tag,...}]]}}` | 富文本。tag: `text/a/at/img/media/emotion/hr/code_block`/`md`(CommonMark+GFM 表格/任务列表/删除线) | ❌ 未用 | 视情况(见 §12) |
| `image` | `{"image_key":"img_xxx"}` | 图片(先 [`upload_image`](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/image/create) 拿 key) | ❌ 未用 | 预留(见 §12.2) |
| `file` | `{"file_key":"file_xxx"}` | 文件(先 [`upload_file`](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/file/create)) | ❌ 未用 | 预留 |
| `audio` | `{"file_key":"file_xxx"}` | 音频 | ❌ 未用 | 暂不 |
| `media` | `{"file_key":"...","image_key":"..."}` | 视频(mp4) | ❌ 未用 | 暂不 |
| `sticker` | `{"emoji_type":"SMILE"}` | 表情包(预定义 emoji_type) | ❌ 未用 | 暂不 |
| `interactive` | `{"schema":"2.0","config","header?","body":{"elements":[...]}}` | 交互卡片(Card 2.0) | ✅ **所有 receipt card + 权限卡** | 长期主路径 |
| `share_chat` | `{"chat_id":"oc_xxx"}` | 分享群名片 | ❌ 未用 | 否 |
| `share_user` | `{"user_id":"ou_xxx"}` | 分享个人名片 | ❌ 未用 | 否 |

**独立 reaction API**(`POST /im/v1/messages/{id}/reactions`,body 用预定义 `emoji_type`):nightme 用于 `OutMessageState`。**append-only** -- 每次 AddReaction 加新 emoji,通道不删老的。unicode emoji 直接返回 `99992354 data not found`,必须用飞书预定义名(OneSecond/OnIt/DONE/THUMBSUP 等)。

### 11.1 选型约束(为什么不用 `post` 走 receipt)

`post` 富文本支持 `md` 标签原生渲染 CommonMark+GFM,**看起来比塞进 card body 简单**。但和 rolling-log UX **根本冲突**:

- `post` 是**整体替换语义** -- 每次发新 `post` 消息,飞书会渲染成新气泡,无法原地编辑
- 飞书没有 `PATCH post` 接口;`Update` / `Patch` 只对 `text` 和 `interactive` 生效
- receipt 的核心 UX 是**一张可原地更新的卡片承载整轮事件**;`post` 实现不了 PATCH-in-place

→ 结论:**文本类(OutText/OutTool/OutResult/...)继续走 `interactive` card**,**不切 `post` `md` 标签**。`post` 留作未来"一次性富文本消息"的载体(比如 help / changelog 推送,见 §12.2)。

### 11.2 与 OpenClaw 官方插件(openclaw-lark)的对比

| 用法 | openclaw-lark | nightme |
|------|---------------|---------|
| 文本 | `post` + `tag:"md"`(CommonMark+GFM) | `interactive` card body(`markdown` element) |
| 思考 | `collapsible_panel` + `text_size:"notation"` + 双语 `i18n_content` | `collapsible_panel` -- **但 §13.1 bug 导致永远走不到这条分支** |
| 工具 | `collapsible_panel` 折叠工具步骤 | `div` + `markdown` 平铺 + emoji 图标 |
| footer | `<hr>` + `<text_tag color='neutral'>`(中点 `·`,非冒号) | 同上 |
| 卡片样式 | Card 2.0 + `update_multi:true` + 双语 | Card 2.0(单语,**未启用 update_multi**) |

→ nightme 的 card body 渲染器比 openclaw-lark **更简单**(无折叠工具、无 i18n),但**少了 thinking 的折叠能力**(死代码 bug,见 §13.1)。

## 12. OutboundKind → Feishu 渲染映射表(当前状态)

每行 = 一个 `gateway.OutboundKind`(定义见 [`internal/gateway/messages.go`](../../internal/gateway/messages.go)),描述 adapter 怎么渲染。`Receipt` 列指是否进 rolling-log card 路径。

| OutboundKind | 源 AgentEvent | 触发点 | Feishu 渲染 | msg_type / API | Receipt? |
|--------------|---------------|--------|-------------|----------------|----------|
| `OutText` | `EventText`(无前缀) | agent 流式文本 | card body `markdown` element + `💬` 图标 | `interactive` PATCH | ✅ |
| `OutThinking` | `EventText`(带 `[思考] ` 前缀,Gateway 已剥) | agent reasoning | **`collapsible_panel` + `💭` 折叠**(§13.6 设计决策;§13.1 bug 待修) | `interactive` PATCH | ✅ |
| `OutToolStart` | `EventToolStart` | 工具开始 | **`collapsible_panel` + `🔧` 折叠**(§13.6 设计决策,粒度待定 §13.7) | `interactive` PATCH | ✅ |
| `OutToolEnd` | `EventToolEnd` | 工具结束(成功/失败) | **`collapsible_panel` + `✅` / `❌` 折叠**(§13.6 设计决策,与 Start 合并 or 独立待定 §13.9) | `interactive` PATCH | ✅ |
| `OutTaskCreate` | `EventTaskCreate` | Claude TaskCreate 成功结果 | 替换 receipt 内最新 typed task snapshot；单 markdown checklist element（§13.14 / §18） | `interactive` PATCH | ✅（独立 checklist block） |
| `OutTaskUpdate` | `EventTaskUpdate` | Claude TaskUpdate / delete 成功结果 | 同上；空 snapshot 清除 checklist；不进入 tool thread | `interactive` PATCH | ✅（独立 checklist block） |
| `OutResult` | `EventResult` | 最终回复 | card body `markdown` + `📝` 图标(text 经 `truncateForLog` 限 `perEntryMaxRunes=8000`; F-37 拆 ≤ 1000 chars/div) | `interactive` PATCH | ✅ |
| `OutUsage` | `EventUsage` | token 用量 | card body `markdown` + `"1.2k tokens · $0.012"`(无图标) | `interactive` PATCH | ✅ |
| `OutCompaction` | `EventCompaction` | 中途压缩 | card body `markdown` + `✶ Compacting conversation...` | `interactive` PATCH | ✅ |
| `OutInit` | `EventInit` | 会话初始化 | card body `markdown` + `session initialized (model: X)`,**Meta 字段(session_id/agent_name/workspace/branch)未渲染**(见 §13.2) | `interactive` PATCH | ✅ |
| `OutCard` | `EventPermission` | 权限请求 | `buildInteractiveCard` → header(title,template:blue) + markdown body + action buttons(value 携带 request_id) | `interactive` Create | ❌(独立气泡) |
| `OutMessageState` | ChatSession lifecycle | 消息进度变化 | `AddReaction(userMsgID, emoji_type)` -- 走 `messageStates` map 做 idempotency | reaction API | ❌(标在用户消息上) |
| `OutMessageStateRemoved` | (reserved) | 撤销进度标记 | `DeleteReaction`(v1.3 未用,append-only) | reaction API | ❌ |
| `OutTyping` | (orphan) | typing 指示 | **silent drop**(飞书 bot 无原生 typing API) | - | ❌ |
| `OutCommandReply` | (slash cmd / runtime error) | `/cwd` `/use` `/kill` `/help` `/agents` 等 | `SendMessageText` -- 独立 text 消息,**绕过** receipt | `text` Create | ❌ |

### 12.1 映射决策的"为什么"

- **receipt card 路径覆盖 8 种** -- 选 `interactive` 是为了 PATCH-in-place(对抗 chat spam);选 markdown element 是为了渲染表格/代码块/超链接(后续会用)
- **MessageState 单独走 reaction** -- append-only emoji 是飞书最轻量、最稳定的进度表达;走 reaction API 不挤占 card body 预算
- **OutCard 走独立 card(非 receipt)** -- 权限卡是单轮交互,需要按钮 + callback,不适合进 rolling log
- **OutCommandReply 走纯文本 `text`** -- 命令反馈是"短而独立"语义,绕过 receipt 让用户看到干净气泡(参见 F-08 §4 "Channel is dumb" contract: command reply 不属于滚动日志)

### 12.2 未来扩展槽位(不实现,但留位)

| 未来需求 | 候选 msg_type | 候选 OutboundKind | 备注 |
|----------|---------------|-------------------|------|
| agent 生成图片 | `image`(先 upload 拿 image_key) | 新加 `OutAttachment{Type: "image", FileKey, FileName}` | 不并入 receipt card(打散 PATCH),走独立 Create |
| agent 生成文件 | `file` | `OutAttachment{Type: "file", ...}` | 同上 |
| help / changelog 推送 | `post` + `tag:"md"` | 复用 `OutCommandReply` 或新增 `OutPost` | 一次性富文本 |
| bot 自定义表情包 | `sticker` | 复用 `OutMessageState` 或新增 | 仅 DM 可用 |

## 13. 2026-08-03 审计结果

### 13.1 🐛 Bug: `OutThinking` 的 `collapsible_panel` 折叠分支是死代码

**证据链**(可逐行复现):

```go
// internal/gateway/translate.go:54-58
if strings.HasPrefix(text, thinkingPrefix) {  // "[思考] "
    return OutboundMessage{
        Kind:   OutThinking,
        Text:   strings.TrimPrefix(text, thinkingPrefix),  // ← 前缀在这里被剥掉
    }, true
}
```

```go
// internal/channel/feishu/adapter.go:435-441
case gateway.OutThinking:
    receipt := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo)
    if receipt == nil { return nil }
    return receipt.Append(ctx, agent.AgentEvent{
        Kind: agent.EventText,
        Text: msg.Text,  // ← 没有前缀
    })
```

```go
// internal/channel/feishu/receipt_event.go:45
case agent.EventText:
    text := strings.TrimSpace(ae.Text)
    if text == "" { return LogEntry{}, false }
    if strings.HasPrefix(text, thinkingPrefix) {  // ← 永远 false(前缀在 Gateway 被剥了)
        return LogEntry{ ..., Kind: "thinking" }, true
    }
    return LogEntry{ Icon: "💬", ..., Kind: "reply" }, true
```

```go
// internal/channel/feishu/adapter.go buildReceiptCard (注释声称的折叠面板分支)
if e.Kind == "thinking" {  // ← 永远不会为真
    elements = append(elements, map[string]any{
        "tag": "collapsible_panel", ...
    })
}
```

**后果**: 长 thinking 直接平铺在 card body 里,挤掉最终回答的可见空间。这与 `buildReceiptCard` 注释 + §9.3 foot note 引用的设计意图完全相反。

**修复方案**(选其一,推荐 A):

- **A. Adapter 补回前缀**(最小侵入,1 行):adapter 在 append 前 `Text: "[思考] " + msg.Text`,`receipt_event.go` 的现有 detection 即可 catch。**代价**:`truncateForLog` 走 thinking 分支时拿到的是剥后的正文(已是 adapter 写死的常量);prefix 是个识别 sentinel,不影响正文渲染
- **B. agent 包加 `EventThinking` 枚举值** + receipt 直接 case;`translate.go` 改发 `EventThinking`,adapter 也直接转发。**代价**:跨 5 个文件动(agent / translate / adapter / receipt / receipt_event);但语义最清晰
- **C. receipt 加 `appendThinkingLocked`,adapter 走专用路径** -- 与 B 类似,但不污染 agent 层

**附议**(无论选哪种方案):`receipt_event.go:40-53` 加注释明确 "prefix MUST be present;Gateway/Caller 负责保证"。否则后人改 transport 又踩一遍。

### 13.2 ⚠️ 待澄清: `OutInit` 的 Meta 字段全部丢失

`translate.go:144-156` 把以下字段写到 Meta:

```go
Meta: {
    "session_id": ev.Init.SessionID,
    "model":      ev.Init.Model,
    "agent_name": ev.Init.AgentName,  // 新加
    "workspace":  ev.Init.Workspace,  // 新加
    "branch":     ev.Init.Branch,     // 新加
}
```

`adapter.go:600-616` 也读到了 `agent.InitEvent` 字段里。但:

- `MessageReceipt` 结构体没有存这些字段
- `buildReceiptCard.footLine()` 只拼 `state + N entries + HH:MM:SS`,**session_id / model / agent_name / workspace / branch 全部丢失**

→ `OutInit` 在 card 上**只有 `state.String()` 变化时会通过 PATCH 触发一次 render**;Meta 携带的元数据**无任何视觉表达**。文档 §9.4 已标记 deferred(目标:"下次 PR 接入 foot note 模板")。

**建议**:Devin 拍板是否在下一份 PR 落地 foot note 扩展(`executing · 5 entries · 14:32:05 | Agent · main · cwd · /repo | Model · claude-sonnet-4-5 | Branch · main`),否则 OutInit 的 5 个 Meta 字段是**纯传输浪费**。

### 13.3 ✅ 已决议(F-37, 2026-08-04): `OutResult` 600 字节截断 → 多 div 拆分解决

**决策状态**: 已决议。F-37 实现落地后,本节 backlog 自动 resolve。

**旧方案**: `truncateForLog(text, perEntryMaxBytes=600)` 切最终回答,Claude Code 1-3 KB `result.Result` 被切掉一半,用户看到 "half answer" 体验。

**新方案**: `splitMarkdownForDivs` 把单个 entry 内容按段落/语义边界拆成多个 `div` 元素,每 div ≤ 1000 chars (Feishu `div` text 硬限),总内容受 30 KB card body envelope 约束:
- 中文: 最多 ~9 divs ≈ 9 KB
- 英文: 最多 ~26 divs ≈ 26 KB

**lark_md 渲染保留** (对比 text fallback 方案),代码块/列表项不会被切坏 (`splitMarkdownForDivs` 守住边界)。

**关联**:
- 实现: `internal/channel/feishu/receipt_split.go` (new)
- 设计: [`docs/feat/F-37-multi-div-content-split.md`](../feat/F-37-multi-div-content-split.md)
- 配置: `perEntryMaxRunes = 8000` (从 600 B 上调)

### 13.4 i️ 未来关注: 没有 OutboundAttachment kind

`InboundMessage.Attachments` 存在(incoming 图片/文件),但 `OutboundKind` **没有对应反向类型**。如果 agent 未来通过工具生成图片/文件,目前**无投递路径**。

MVP 不阻塞(Claude Code 不生成媒体),但 Channel 抽象层对外非对称。建议下一份抽象文档(Gateway hub)补 `OutboundAttachment` 类型,把 §12.2 表里的"未来扩展"沉淀进代码契约。

### 13.5 i️ 已知接受: `OutCard` RequestID 临时生成

`adapter.go:530-533` 用 `fmt.Sprintf("%s:%d", msg.ChatID, time.Now().UnixNano())` 拼 RequestID:

- 同 chatID 同 ns 内理论可重复(纳秒精度在某些平台受限于时钟粒度)
- chatID 前缀降低了碰撞面
- 实际场景:同一 chat 用户不可能在 1ns 内连点 2 次不同权限

→ **接受现状**,但建议加注释解释 chatID 前缀的意图 + "碰撞面已被 chatID 限定"。

### 13.6 🎯 设计决策(2026-08-03 Devin 拍板): Thinking + Tool Start/End 全部折叠

**决策状态:已决议 (DECIDED 2026-08-03,Devin "按你的建议修改")**

**采用方案**: §13.7 方案 1(per-event) + Q2=a + Q3=全部折叠 + Q4=a。

**实施要点**:
1. 修 §13.1 bug(adapter 补回 `[思考] ` 前缀)-- `receipt_event.go` 的现有 detection 即可 catch
2. `receipt_event.go` 给 `tool_start` / `tool_end` 标新 `Kind="tool"`(或分别 `Kind="tool_start"` / `Kind="tool_end"` -- 二者皆可,前者更简单)
3. `buildReceiptCard` 新增 `Kind="tool"` 折叠分支:header 为 `🔧 tool_name(args)` / `✅ tool_name → output` / `❌ tool_name failed: err`,body 为 entry 文本
4. 折叠默认 `expanded: false`(所有 thinking / tool 默认折叠)
5. 最终回复(📝)、OutUsage、OutCompaction、OutInit 保持平铺 `markdown` element

**结论**: `OutThinking` / `OutToolStart` / `OutToolEnd` 在 card body 里**全部走 `collapsible_panel`**,默认折叠。最终回复(📝)、token 用量(OutUsage)、compaction(OutCompaction)、init(OutInit)保持平铺(`markdown` element)。

**理由**:
- Thinking / Tool 调用是"agent 在做什么"的中间过程,对用户阅读最终结果**不是关键** -- 折叠才能让卡片聚焦答案
- 平铺会让 agent 调 10 次工具的卡变成"工具清单 + 答案尾巴",最终答案被挤到屏幕外(§13.1 已记录的实际问题)
- 与 openclaw-lark `buildCompleteCard` 的 `toolUseSteps` `collapsible_panel` 模式一致(§11.2)

### 13.7 ✅ 已决议: UX 折叠粒度(方案 1,per-event)

**决策状态:已决议 (2026-08-03)。选方案 1(per-event)。**

详见 §13.6 决策摘要。理由:与现有 thinking 折叠逻辑一致(每 entry 一个 panel),改动最小;先解决"折不折"问题,聚合(方案 2 / 3)留给下一代重构。

**未选择方案**:
- 方案 2(aggregate-paired):需维护 Start→End 配对状态机,跨 event 状态复杂度增加
- 方案 3(category-aggregate):需新增聚合 buffer,PATCH 字节膨胀,QPS 风险

→ 方案 2 / 3 作为 backlog(§15 future work),不在本 PR scope。

**Per-event 形态**:

```
💭 [panel: 💭 思考] (折叠)
   让我看一下代码...

🔧 [panel: 🔧 Read(/a.py)] (折叠)
   Read(/a.py)

✅ [panel: ✅ Read done] (折叠)
   Read → opened file, 47 lines

🔧 [panel: 🔧 Bash(git status)] (折叠)
   Bash(git status)

✅ [panel: ✅ Bash done] (折叠)
   Bash → on branch main, clean

📝 最终回复 (平铺)
   API handler 在 /api/v1/foo.py:42
```

每个 entry 一个 panel,header 用 entry 的 icon + name + args/output。

**方案 1: 每个 entry 一个折叠面板(per-event)**

```
💭 [面板标题: 💭 思考] (折叠)
   让我看一下...

🔧 [面板标题: 🔧 Read(/a.py)] (折叠)
   Read(/a.py)

✅ [面板标题: ✅ Read done] (折叠)
   Read done

💬 最终回答 (平铺)
```

- **优点**: 与现有 thinking 折叠逻辑一致;不需要 Start→End 配对
- **缺点**: 一个 agent turn 调 10 个工具 = 30 个 panel,卡片嵌套很深

**方案 2: 同类聚合 + 配对(aggregate-paired)**

把 Thinking 聚合成 1 个面板(顺序追加),Tool Start+End 配对聚合成 1 个面板:

```
💭 [面板标题: 💭 思考 (3 段)] (折叠)
   让我看一下...
   然后再...
   嗯...

🔧 [面板标题: 🔧 Read(/a.py)] (折叠)
   ✅ done

🔧 [面板标题: 🔧 Bash(git status)] (折叠)
   ✅ done

💬 最终回答 (平铺)
```

- **优点**: 卡片扁平、阅读体验更好;agent turn 调 N 个工具只占 N 个 panel 而非 2N
- **缺点**:
  - 需要 receipt 维护"当前 thinking panel / 当前 tool pair"指针(类似流式 buffer)
  - Start 来时建面板,End 来时把结果 fold 进同一面板 -- **跨 event 状态机变复杂**
  - 工具面板关闭/开启时 `expanded` 状态需保留(用户展开后面板又来一个 event,PATCH 卡片时要不要把它默认折叠回去?)

**方案 3: 类别聚合(category-aggregate,推荐)**

把所有 Thinking 聚合成 1 个面板,**所有 Tool 调用** 聚合成 **1 个面板**(内置子结构:每个 tool 一行):

```
💭 [面板标题: 💭 思考] (折叠)
   <全部 thinking 文本>

🛠 [面板标题: 🔧 工具调用 (5 次)] (折叠)
   Read(/a.py) → done
   Bash(git status) → done
   Edit(/a/b.py) → done
   Read(/a/c.py) → done
   Bash(make test) → done

💬 最终回答 (平铺)
```

- **优点**: 最扁平;阅读体验最好;openclaw-lark 的 `toolUseSteps` 默认就是这种
- **缺点**:
  - 需要在 `MessageReceipt` 加 `thinkingPanel` + `toolsPanel` 两个聚合 buffer
  - **不能并入现有 `entries []LogEntry` 模型** -- 现有模型是 FIFO list,聚合需要 map/set 或 lazy 渲染
  - 增量 PATCH 时聚合面板的文本变化较大(每来一个 event 都触发完整重渲染整个面板内容)→ PATCH 字节膨胀,QPS 风险

### 13.8 实施建议(已决议采用)

**首版实施步骤**(§13.6 / §13.7 / §13.9 决议后):

1. 修 §13.1 bug(adapter 补回 `[思考] ` 前缀)
2. `receipt_event.go` 给 `tool_start` / `tool_end` 标 `Kind="tool"`(统一,无需区分 start/end)
3. `buildReceiptCard` 新增 `Kind="tool"` 折叠分支(同 thinking 的 `collapsible_panel` 结构)
4. **不**加 `card.tool_fold` 配置开关--所有 entry 统一折叠行为,保持简洁(可在未来 PR 加)

**`truncateForLog` 限额考虑**:
- `OutResult` / `OutToolEnd` 的 output 可能超过 `perEntryMaxBytes=600` -- 折叠面板展开后用户能看到完整内容,但默认折叠时只显示 icon + header
- 折叠 panel 的 body 内容建议也走 `truncateForLog` 与现有逻辑一致
- **不在本 PR 解决 §13.3**(OutResult 限额拓宽) -- 是独立 PR

**测试覆盖**:
- `receipt_test.go` 加例:append 一连串 EventText(thinking + normal) + EventToolStart/End,验证生成的 card JSON 元素数组顺序 + 每 entry 的 `collapsible_panel` 结构
- `adapter_test.go` 加例:Verify `Send(OutboundMessage{ReplyTo: "user_123", Kind: OutThinking})` 走到 receipt.Append 时 prefix 已加回

**未来可演进到方案 3**:等 next-gen rolling-log 重构时再做聚合;当前 v1.3 不动 receipt 渲染结构,只解决"折叠 or 不折叠"的开关。

### 13.9 ✅ 已决议(2026-08-03):4 个 UX 细节

**决策状态:全部已决议,采用我推荐的组合**。

| 问题 | 决议 | 备注 |
|------|------|------|
| **Q1 折叠粒度** | 方案 1(per-event) | 每个 entry 一个 panel;不改 receipt 状态机 |
| **Q2 header 内容** | (a) 静态 `🔧 tool_name(args)` / `✅ tool_name → output` | 简单、不加耗时(耗时属 §13.3 backlog) |
| **Q3 默认折叠** | **全部折叠**(thinking + tool_start + tool_end) | 卡片聚焦最终回复 |
| **Q4 Start+End 合并?** | (a) 独立 panel | Start panel `🔧 Read(/a.py)`,End panel `✅ Read done` -- 与 entry 一一对应,不改 receipt 结构 |

### 13.10 🐛 Bug:`OutboundMessage.ReplyTo` 没有被作为 Feishu `root_id` 投递

**Devin 原话(2026-08-03 17:04)**:"我们现在回复的不是 Reply in Thread 的方式,看不出来我这条回复回的是哪个消息,reply_to 没有真正用到对不对?"

**调查结论**:**对,ReplyTo 字段被消费在内部(用作 receipt map 查找 key),但从来没有被投递到 Feishu API**。bot 的所有回复都是顶层消息,看不到与用户消息的连接。

**证据链**:

1. `internal/chatsession/readpump.go:198` -- `readpump` 读 `cs.currentTurnUserMsgID` 并以 `userMsgID` 参数调用 gateway 的 event handler
2. gateway 的 event handler 把它写到 `OutboundMessage.ReplyTo`(参考 `internal/gateway/binding.go:8` 与 `messages.go:48`)
3. `internal/channel/feishu/adapter.go:424/436/525/535/550/571/581/591` -- 8 处都只调用 `a.receiptFor(ctx, msg.ChatID, msg.ReplyTo)`,**只用 `ReplyTo` 查 `receiptsByUserMsgID` map**
4. `sendViaLark` (`adapter.go:1295-1324`) 构造的 `CreateMessageReqBody` 只有 `ReceiveId / MsgType / Content`,**没有 `RootId` 字段**
5. `larkim.NewCreateMessageReqBuilder` SDK 调用 `Message.Create`(顶层创建 API,非 Reply)
6. `PatchMessage` (`adapter.go:1387`) PATCH `/im/v1/messages/{id}` 也不传 root_id -- PATCH 会**保留**被 PATCH 消息的原始 root_id,但因为原始 create 没设,结果依然无 root_id
7. `larkim.CreateMessageReqBody.RootId` 字段在 SDK 中存在(`oapi-sdk-go/v3@v3.5.3/service/im/v1/model.go:2125`),**SDK 完全支持,代码没用而已**
8. `larkim.ReplyMessageReq{RootId?, ReplyInThread, ...}` **也存在**(`model.go:11385+`),是专门回复的 endpoint

**与历史设计的关系**: `docs/feat/F-26-gateway-hub.md:223-225`(已被 v1.3 标 SUPERSEDED,但语义依然适用)写过:

> ReplyTo 非空 → 必须镇定到该 userMsgID(**用 ReplyMessage API 或已有 receipt**)

v1.3 refactor(`a38fa5b refactor(gateway): Remove Gateway-side Receipt FSM`)把 receipt FSM 从 Gateway 搬到 Channel,但**没有把这条 reply-in-thread 约束带过去**。结果:docs 写过、SDK 支持、当前实现忘了。

**当前 UX 问题**:

| 场景 | 用户看到的 | 应该看到的 |
|------|-----------|-----------|
| 用户发消息 → agent 处理 5s | `user_msg` → `bot_card`(漂浮,无连接) | `user_msg` ⤓ `bot_card`(回复线连接) |
| 用户连发 3 条 | 3 条 user_msg + 1 张 bot_card(飘在最后一条下面,但看不出回的是哪条) | 3 条 user_msg 各带 ⏳→🔄,最后一条 ⤓ bot_card |
| 用户发 `/help` | `user_msg`(/help) → bot_msg(help text,无连接) | `user_msg`(/help) ⤓ bot_msg |
| 权限请求 | `user_msg`(task) → bot_card(权限,无连接) | `user_msg`(task) ⤓ bot_card(权限) |
| 同一 turn 多次 PATCH | bot_card 自己更新 | bot_card 自己更新(root_id 由 Feishu 保留,无需重复传) |

**修复方案**(Devin 拍板范围):

| 范围 | 改哪些 | 影响 |
|------|--------|------|
| **A. 最小:只 thread receipt card** | `receiptFor()` 冷启动走 `CreateMessage{RootId: userMsgID}` | receipt card 与 user_msg 视觉连接;slash command / 权限卡仍漂浮 |
| **B. 中等:thread 所有 SendContent 调用** | `sendContent` / `SendCard` / `sendMessageText` 都接 `rootID` 参数;adapter.Send 在 `msg.ReplyTo != ""` 时透传 | 所有 bot 回复都有视觉连接;`OutCommandReply` 注释里的 "no ReplyTo threading" 需要同步更新 |
| **C. 完整:thread + 可选 reply_in_thread** | 同 B,外加支持 `reply_in_thread: true`(把整个 receipt 移入 thread) | 用户可配 "thread 模式" 还是 "main chat 模式" |

**SDK 调用细节**(方案 B / C 落地):

```go
// sendViaLark 当前:
body := &larkim.CreateMessageReqBody{
    ReceiveId: &chatID,
    MsgType:   &msgType,
    Content:   &content,
}

// 改后:
body := &larkim.CreateMessageReqBody{
    ReceiveId: &chatID,
    MsgType:   &msgType,
    Content:   &content,
}
if rootID != "" {
    body.RootId = &rootID  // SDK 字段,直接用
}
```

PATCH 路径不动 -- Feishu 的 PATCH 接口会自动保留被 PATCH 消息的原始 `root_id`。

**与 §13.6(折叠设计)的关系**: 这是独立的两件事,但**建议合并到同一份 PR**:
1. §13.6 修折叠渲染(per-event / 聚合 / 配对 三选一)
2. §13.10 修 reply-in-thread(`root_id` 投递)
3. 一并加测试(冷启动 receipt card 后,Feishu 端能看到 root_id)

**待 Devin 拍板(开 PR 前)**:

1. **修复范围**:A / B / C?
2. **OutCommandReply 同步 thread**?(B/C 方案下,slash command 的回复会变 threaded;`adapter.go:608-619` 注释需要更新)
3. **OutCard(权限卡)同步 thread**?(B/C 方案下,权限请求与用户原消息连接)
4. **`reply_in_thread` 模式是否需要**?(C 方案专属;默认 false 走 main chat,保持 discoverable;用户可选 true 移入 thread)

**附议**: 落地后 `adapter.go:608-619` 的 "no ReplyTo threading" 注释需要更新;`docs/feat/F-26-gateway-hub.md:223` 已经描述了这条约束,可以作为权威参考,无需修改 F-26(它是 v1.1 文档,已 superseded,但语义对 v1.3 仍生效)。

**✅ 决议(2026-08-03,Devin "按你的建议修改")**: 采用 **方案 B**,同步 thread OutCard 与 OutCommandReply。**不**实现 `reply_in_thread` 模式(属未来 P2)。

**✅ 子决议(2026-08-04,F-37 落地)**: `reply_in_thread` 不再"P2 一刀切",而**按 OutboundKind 拆分**到飞书 3 种 reply 形态：

> 飞书实机验证（2026-08-04, Frtpilot-Xiage 群）确认 3 种 reply 形态，命名来自 ops 现场观察：
>
> **作用域声明**：`ReplyInChat` / `ReplyInThreadAndChat` / `ReplyInThread` 这三个名字是 **`channel/feishu` 自治范围内的渲染决策**（具体到飞书 thread UI 行为），**不**上升到 `gateway.OutboundMessage` / `OutboundKind` 抽象层——其他 channel（如未来 Web / Slack）应**各自**决定怎么渲染 OutThinking / OutTool* ，不复制 Feishu 的 thread 方案（详见 `docs/feat/F-08-channel-abstraction.md` §4）。Gateway / ChatSession / OutboundMessage 契约不变。

| 形态名 | `reply_in_thread` 字段 | main chat | thread panel | `thread_id` 响应 |
|---|---|---|---|---|
| **ReplyInChat** | n/a（顶级 Create，不走 reply API）| 独立气泡 | 不在 thread | `""` |
| **ReplyInThreadAndChat** | **字段省略**（SDK `omitempty` nil 指针）| **正文内联** | **同一份正文** | `""` |
| **ReplyInThread** | `true` | **"X replies" 灰条**（无正文）| **正文** | `omt_xxx`（首次分配，后续复用）|

按 OutboundKind 拆分（与上表路径一致，2026-08-04 ops 实机确认）：

- **ReplyInThread (`reply_in_thread=true`)** — `OutThinking` / `OutToolStart` / `OutToolEnd`：agent 进度流，绝不污染 main chat
- **ReplyInThreadAndChat (字段省略)** — receipt 冷启动卡 / `OutCard` (permission) / `OutCommandReply` / `OutCompaction`：用户首要看到的答案 / 不可漏看的权限请求 / slash 命令回应 / brief 进度 marker，必须 main chat 可见
- **ReplyInChat (顶级 Create)** — nightme **不**走此形态（fallback 路径 230011/231003 才退化到此，详见 §15.2）

实现：`sendMessageFunc` / `sendContent` / `sendViaLarkReply` / `SendMessageText` / `SendCard` / `postThreadReply` 全链路加一个尾部 `replyInThread bool` 参数；`sendViaLarkReply` 内部 `larkim.NewReplyMessageReqBodyBuilder()` 仅在 `true` 时调 `.ReplyInThread(true)`（**不能简化成** `.ReplyInThread(replyInThread)`，否则 false 路径多 28 字节破坏 pre-F-37 idempotency cache）。详见 `docs/feat/F-37-tool-thread-routing.md` §2.1 + §7.5 实机验证 + adapter.go。

**相关测试**：`adapter_test.go::TestSend_ThreadOnlyEvents_PassReplyInThreadTrue` (4 kinds × ReplyInThread) + `TestSend_ChatVisibleEvents_PassReplyInThreadFalse` (3 paths × ReplyInThread+Also send it to chat)。

### 13.11 决策记录(2026-08-03,F-33):ChatID 数据模型简化

**Devin 原话**(2026-08-03 21:08):"我们是不续接任何 Thread,最多就是点对点的 ReplyTo" - chatID 数据模型做一次系统性清理。

#### 13.11.1 三个核心决策

**D1: ChatType 不进 Gateway**
- 删除 `internal/gateway/messages.go:18-27` 的 `ChatType` 类型 + 4 个常量(`ChatTypeP2P/Group/Thread/Other`)
- 删除 `InboundMessage.ChatType` 字段 + `IsDM()` 方法
- 删除 `BindingEntry.ChatType` / `ChatSession.ChatType` / `ChatSessionEntry.ChatType` 持久化字段
- `/status` 命令不再展示 "DM/Group" 标签
- Gateway 只看 `chat_id string`,假设所有 chat 同质

**D2: topic_group 不特殊处理**
- Feishu `chat_type == "topic_group"` 的消息**完全不分支**,跟普通 group 走相同路径
- 原因:Feishu SDK `EventMessage.ChatId` 在 topic_group 下跟 group **同构**(都是 `oc_xxx`),thread 是消息级逻辑分组,不是 chat 级
- binding 表 `gateway.bindings[chat_id]` 天然兼容,topic_group 在 nightme 内部就是"group,thread 在外面"
- 适配器 `internal/channel/channel.go` 删 `ChatTypeThread` 常量;保留 `ChatTypeP2P/Group`(Channel 包私有)

**D3: ReplyTo = ParentId,RootId 不进 nightme**
- Inbound `msg.ReplyTo = message.ParentId`(Feishu 原生 `parent_id`)
- Outbound `msg.ReplyTo = currentTurnUserMsgID`(不变,§13.10 已落地)
- **`RootId` 整个项目永远不读、不存、不传**(`grep -rn "\.RootId" internal/` 应为 0 命中)

**D4: 任何 Channel 都不引入 thread 概念**(Devin 2026-08-03 21:11 拍板,B 选项)
- Slack `thread_ts` / Telegram `message_thread_id` / Discord thread 等**不进 nightme 数据模型**
- 仅 Channel 内部渲染时使用(thread 视觉由 Channel 自治)
- 如未来 Slack thread 等场景需要支持,在 Channel 包内自治实现,**不动 nightme 数据模型**

#### 13.11.2 语义统一:ReplyTo = "被 reply 的那条 message_id"

| 方向 | 值 | 含义 |
|------|-----|------|
| Inbound `msg.ReplyTo` | `message.ParentId` | user 在 Feishu 端 @ 的那条 message_id;top-level 消息(没 @)则 `ParentId == ""` → `ReplyTo == ""` |
| Outbound `msg.ReplyTo` | `currentTurnUserMsgID` | user 当前这条 message_id(不是 thread 顶层,也不是 user @ 的目标) |

两个 `ReplyTo` 是**同一个字段,不同方向**,语义统一为 "reply 关系中的被 reply 方"。

#### 13.11.3 当前 inbound bug:`msg.ReplyTo` 永远是空字符串

`handleMessage`(`internal/channel/feishu/adapter.go:1593-1617`)构造 `channel.Message` 时**不读 `event.Message.RootId`,也不读 `event.Message.ParentId`**:
```go
msg := channel.Message{
    ChatID:      chatID,                        // ✅ 已填
    Text:        text,
    UserID:      senderID(event),
    Time:        messageTime(message.CreateTime),
    ChatType:    gateway.ChatType(chatType),    // ❌ D1 移除
    MessageID:   stringValue(message.MessageId),
    // ReplyTo 字段永远不存在 ❌ D3 需要新增 wire
    Attachments: attachments,
}
```

**修复**:加 `ReplyTo: stringValue(message.ParentId)`,删 `ChatType: gateway.ChatType(chatType)`。

#### 13.11.4 数据流影响

**Inbound**(改动):
```
Feishu SDK event(EventMessage{ChatId, ChatType, MessageId, ParentId, RootId, ...})
  └─ handleMessage
     ├─ msg = channel.Message{
     │    ChatID:    chatID,                   // 不变
     │    MessageID: message.MessageId,        // 不变
     │    ReplyTo:   message.ParentId,         // ← 新增 wire (D3)
     │    Text/Attachments/UserID/Time: 不变,
     │    // ChatType 字段删除                  // ← D1 移除
     │  }
     └─ publish a.incoming → Gateway.dispatchLoop → bindings[msg.ChatID]
```

**Outbound**(无变化,§13.10 已落地):
```
ChatSession.onAgentEvent
  ├─ out.ReplyTo = cs.currentTurnUserMsgID     // 不变
  └─ channel.Send → sendViaLarkReply(POST /im/v1/messages/{rootID}/reply)
     rootID = msg.ReplyTo = user 当前 message_id
     ↓
     Feishu 端把 reply 视觉放到 user 当前 message 附近
```

#### 13.11.5 跟 §13.10 的关系

§13.10 修了 **outbound**(bot reply 用 `msg.ReplyTo` 作 Feishu root_id 投递)。
§13.11(D3)修了 **inbound**(wire `msg.ReplyTo = message.ParentId`)。
两条合起来构成完整 reply-in-thread 数据流闭环:
- inbound 方向:user reply 的目标(`ParentId`)被记录
- outbound 方向:bot reply 永远 anchor 到 user 当前 message(`currentTurnUserMsgID`)

不引入 thread 树,不爬 RootId,Feishu 端 thread 视觉由 Channel 自治。

#### 13.11.6 实施细节

详见 [`docs/feat/F-33-simplify-chatid-data-model.md`](../feat/F-33-simplify-chatid-data-model.md),包括:
- 12 处代码改动清单(Channel → Gateway → ChatSession → Registry)
- Test 改动 + 新增 `TestHandleMessage_ReplyToFromParentId`
- Registry 兼容(Go JSON unmarshal 默认容忍未知字段,无需迁移)
- 验收清单 16 项

### 13.12 🎯 设计决策反转 (2026-08-04 Devin 拍板):折叠 → Thread Reply

> **后续**:F-39 §13.15 在 §13.12 基础上**进一步合并** `OutToolStart` + `OutToolEnd` 为**一条** thread reply(call + result 通过 PATCH 同一 message_id 合并)。§13.12 描述"每事件一条 reply";§13.15 后变成"每个 tool 一条 reply(Start + End 合并)"。OutThinking / OutCompaction 仍按 §13.12 各一条。

**背景**:§13.6 拍板的折叠方案(OutThinking / OutToolStart / OutToolEnd 在 receipt card body 里走 `collapsible_panel`)实机上视觉体验差,详尽分析 + 新方案设计见 [`docs/feat/F-37-tool-thread-routing.md`](../feat/F-37-tool-thread-routing.md) §1。

**新决议**:OutThinking / OutToolStart / OutToolEnd / OutCompaction 从 receipt card body 里抽出,作为独立 thread reply 投递到 user message 的 Feishu thread;receipt card 收窄到只承载最终答复(OutText / OutResult)+ 元数据(OutInit / OutUsage)。

**Receipt card 视觉对比**:

旧(折叠方案):card body 含 30+ collapsible_panel,Feishu 50 element 上限频繁撞破,最终回答被挤到屏外。

新(thread 方案):card body 只含 header + 最终回答 + footer(≤5 element);user message 下方出现 "X replies" 指示器,click 后进入 thread 看 💭🔧✅ 流。

**OutToolEnd 类型感知摘要**(不再 dump 原始 output 到 thread,改为按 tool name 生成单行摘要):

```
Read       -> 1 lines 截断
Write      -> 5678 bytes
Edit       -> applied
Bash       -> 3 lines (cmd 截断 80 字符)
Grep       -> 12 matches across 5 files
Glob       -> 8 files
WebFetch   -> 4321 chars fetched
WebSearch  -> 10 results
(default)  -> 截断到 200 chars
(err != nil) -> failed: err.message
```

**架构不变式保留**:OutboundMessage 不动(无新 Kind)、Gateway 不动、ChatSession 不动、`currentTurnUserMsgID` 单数锚点保留、F-33 thread 概念不进 nightme 数据模型不变式保留。

**与 §13.10 (reply-in-thread) 的关系**:§13.10 修了"ReplyTo 未投递为 root_id";§13.12 走得更远——除 receipt card 之外的其他 OutboundKind 也不进 main chat,只进 thread。

**§13.1 bug 不再需要修**:旧决议下 thinkingPrefix 剥除是 bug(导致 collapsible_panel 死代码)。新方案下 Gateway 不再剥,adapter 直接拿 `msg.Text` 加 💭 前缀发 thread。

**实施步骤总览**(详见 F-37 §3.1):

1. **Bridge contract 扩展**:`agent.ToolEndEvent.Args string` 字段;claudecode bridge 从同 message `tool_use` block 拿 args 填入
2. **Feishu adapter `Send` 分流**:thinking/tool/compaction → `postThreadReply`;text/result/init/usage → `receiptFor.Append`
3. **`summarize_tool.go` 新文件**:`summarizeToolEnd` + `countLines` + `truncate` + `countUniqueFiles` helpers
4. **Receipt card 瘦身**:`buildReceiptCard` 删 `Kind="thinking"` / `Kind="tool"` collapsible_panel 分支
5. **`eventToEntry` 收窄**:对 thinking/tool/compaction 返回 `(_, false)`
6. **测试改造**:删 thinking/tool entry assertion;新增 `TestSend_Out*_PostsToThread` + `TestSummarizeToolEnd`

**Backlog**:OutThinking 多 chunk 聚合(streaming 模式,最后一段更新)、Web UI / Slack 适配各 channel 自治决定。

### 13.13 F-37 实机验证方法论(2026-08-04 群 Frtpilot-Xiage 落地)

> 命名：3 组合是 **`channel/feishu` 自治**的渲染决策(具体到飞书 thread UI 行为),不上升到 `gateway.OutboundMessage` 抽象层。其他 channel(Web / Slack)应各自决定怎么渲染 OutThinking / OutTool*,不复制飞书的 thread 方案。

**3 种飞书 reply 形态**(实机验证,2026-08-04):

| 形态 | 飞书 `reply_in_thread` body 字段 | main chat 实际显示 | thread panel 实际显示 | 飞书响应 `thread_id` |
|---|---|---|---|---|
| **ReplyInChat** | n/a(顶级 Create) | 独立气泡(不挂 anchor 下) | 不在 thread | `""`(飞书不分配) |
| **ReplyInThreadAndChat** | **字段省略**(`omitempty` nil 指针) | **正文**内联 reply(带回复箭头) | **同一份正文** | `""`(飞书不分配独立 thread)|
| **ReplyInThread** | `true` | **"X replies" 灰条**(无正文)| **正文**(多条 share 同一 thread)| `omt_xxx`(首次分配,后续 reply-true 复用)|

**OutboundKind 路径拆分**(F-37 thread-route 设计):

| Kind | 形态 | main chat | 备注 |
|---|---|---|---|
| `OutToolStart` | ReplyInThread | 隐藏 | Claude Code-style `● name(args)` call 行 |
| `OutToolEnd` | ReplyInThread | 隐藏 | Claude Code-style `⎿  …` result 行 |
| `OutThinking` | ReplyInThread | 隐藏 | `💭 <text>` |
| `OutCompaction` | ReplyInThreadAndChat | **可见** | `✶ Compacting conversation…`(ops 决策 2026-08-04:brief marker 是 informative 不是 noise)|
| `OutCard`(permission)| ReplyInThreadAndChat | 可见 | 必须 main chat 可视(discoverability > cleanliness)|
| `OutCommandReply`(slash)| ReplyInThreadAndChat | 可见 | 用户在等回复 |
| Receipt 冷启动卡 | ReplyInThreadAndChat | 可见 | receipt card 收窄到只承载 OutText / OutResult + 元数据 |

> Kinds 命名 ops 用 past tense(`OutToolStarted/Ended/Think`),nightme enum 实际是 present tense(`OutToolStart/End/OutThinking`)。不**改 enum 名(牵动 Gateway 抽象层多个包),只按 enum 行为归属。

#### 13.13.1 实机验证方法(when to run)

**何时需要再跑一遍**:
- 升级 `larksuite/oapi-sdk-go` 跨 minor 版本(>= v3.10)
- 飞书发布新 doc 关于 `reply_in_thread` 字段语义变更
- nightme 引入新 OutboundKind(检查它的形态归属)
- 用户报告"thread 行为不对"——验证 round-trip ID 关系

**前 2 步准备**:
1. **确认目标 chat 是群**(非 DM):DM 没有"X replies"指示器 UI,`reply_in_thread=true/false` 视觉差异看不出来。推荐用一个**活跃的** group chat(不是 nightly internal DM)。
2. **取得锚点 user message id**:用户在那条群发任意一句话,得到 message_id(`om_xxx`)。或者用 nightme 已有的 `cs_oc_xxx` chat 里最近一条用户消息 id。

#### 13.13.2 8 种验证组合(what to send)

每组发**一条**消息,带**不同前缀**让你在飞书 UI 上一眼区分,**串行发,每发一条停下来目视确认**。8 组按顺序:

| # | 组合 | wire 形态 | 预期 main chat | 预期 thread panel |
|---|---|---|---|---|
| 1 | A. 顶级 Create | `POST /im/v1/messages` (无 root_id) | 独立气泡 | 不在 thread |
| 2 | B. Reply to anchor,字段省略 | `POST /messages/{om_M0}/reply` (无 reply_in_thread) | **正文**内联 + 线程入口 | 同一份正文 |
| 3 | C. Reply to anchor,显式 false | 同上 + body `reply_in_thread:false` | 同 B(字节差 28B,UI 等价)| 同 B |
| 4 | **D. Reply to anchor,`reply_in_thread:true`** | 同上 + body `reply_in_thread:true` | **"X replies" 灰条**(无正文)| **正文** + 首次给 `omt_xxx` |
| 5 | E. Chain reply(reply to D 的 id) | `POST /messages/{D.id}/reply` (parent ≠ M0) | 内联 reply 到 D | thread 碎裂(独立 thread_id)|
| 6 | F. 顶级 Create + raw `root_id` body | `POST /im/v1/messages` body `{..., root_id:om_M0}` | **当前 SDK 拒绝**(结构体无 `RootId` 字段) | — |
| 7 | G1+G2. 两条 reply-true 续发 | `POST /messages/{om_M0}/reply` 两次 + `reply_in_thread:true` | 共享 "X replies" 灰条(数字累加)| 两条 share `omt_xxx` |
| 8 | H. DM context(同 D) | 同 D | DM 没有 thread panel,UI 不可见差异 | — |

#### 13.13.3 验证方法(how to send + what to capture)

**用 larkim SDK 直接发**(无需 cmd/_probe/ 工具,工具已删;需要时按本节重建):

1. 构造 `lark.Client` 用 `~/.config/nightme/config.yaml` 里的 `app_id` / `app_secret`(或从环境变量读)
2. Create 顶级:`cli.Im.V1.Message.Create(ctx, NewCreateMessageReqBuilder().ReceiveIdType("chat_id").Body(&CreateMessageReqBody{ReceiveId:&chatID, MsgType:&MsgTypeText, Content:&json}).Build())`
3. Reply 默认:`cli.Im.V1.Message.Reply(ctx, NewReplyMessageReqBuilder().MessageId(om_M0).Body(NewReplyMessageReqBodyBuilder().MsgType(MsgTypeText).Content(json).Build()).Build())` — **不**调 `.ReplyInThread(...)`
4. Reply true:在 3 的 builder 链上**追加** `.ReplyInThread(true)`
5. Reply false 显式:追加 `.ReplyInThread(false)`(与 3 字节差 28B)

**每个响应打印**(从 `larkim.ReplyMessageResp.Data` / `CreateMessageResp.Data` 提取):
- `message_id` —— 这条新消息的 id
- `parent_id` —— 父消息(Reply API 时 = path 里的 message_id;Create 时空)
- `root_id` —— 根消息(Reply API 时飞书沿 parent 链爬到根;Create 时**空字符串**)
- `thread_id` —— thread 容器 id(ReplyInThread + true 首次分配 `omt_xxx`;其他情况空)

**关键判读规则**:
- B vs C:body 字节差 28B(多 `"reply_in_thread":false`),但飞书 UI 行为**完全等价**——这是 `omitempty` 的设计
- B vs D:body 字节差 28B(多 `"reply_in_thread":true`),飞书 UI 行为**完全不同**——B 显正文,D 只显 "X replies"
- D 续发 G1/G2:都 share `thread_id`(`omt_xxx` 同值),main chat 累加 1→2→3 replies
- E (chain reply):飞书**会**沿 parent 链爬到根,`root_id` = M0;但 thread_id 是新分配的独立 id → thread 碎裂 → 验证"单数锚点"不变式不可妥协

#### 13.13.4 注意事项(pitfalls)

- **`SendMessageFunc` 注入点不要漏**:`internal/channel/feishu/adapter.go` 的 `sendMessageFunc` 是 6 参(含 `replyInThread bool`);**单测 mock 必须用同样签名**,否则编译挂
- **绝对不要简化**:`if replyInThread { bodyBuilder.ReplyInThread(true) }` → 不能改成 `bodyBuilder.ReplyInThread(replyInThread)`,后者 false 路径会多 28 字节破坏 pre-F-37 idempotency cache 字节级 hash
- **mock probe 不要相信**:mock server 假设 `root_id = message_id` 是错的(实机 Create 返回空),`thread_id` 总是有值也是错的(实机 false 形态返回空)。mock 只能验证 SDK 字节格式;实机才能验证 ID 关系
- **DM 测不出 thread 视觉差异**:1-on-1 没有"X replies"指示器 UI,`reply_in_thread=true/false` 视觉合一。要测 thread 行为必须用群
- **夜间飞书 API 有维护窗**:5 QPS 是 bot 限速,但飞书自身偶尔 5xx;持续失败时先 `GET /open-apis/health` 查服务端状态,别反复重试把单测打挂
- **不要把回包 `thread_id` 当持久 ID**:`omt_xxx` 是飞书服务端分配的,跨会话不保证稳定;nightme 不存它(再次验证 F-33 thread 不进 nightme 数据模型)

#### 13.13.5 历史与移除记录

2026-08-04 在 Frtpilot-Xiage 群(`oc_4a06da49bc0131ff14b381498e4fed9d`)实机跑过一轮,得到关键发现:
- 顶级 Create 不分配 thread_id(根/thread 都空,跟 mock 假设不同)
- ReplyInThread+AndChat 也不分配 thread_id(只是 main chat 内联 reply)
- ReplyInThread 才分配独立 thread_id(`omt_xxx`,首次分配后续复用)
- 4 条 reply-true 全部 share 同一 `thread_id`(反向验证"单数锚点"设计对的)
- 字节差异:字段省略 vs 显式 false = 28 字节(omitempty 决定)

`cmd/_probe/`(mock probe + 真实发送 CLI)曾保留在 working tree 一段时间,后于 `c52ad06` 删除——决策已落地,工具不再有保留价值。**当未来需要再跑验证时**:按本节 §13.13.3 重建 SDK 调用即可,无需还原工具源码。

### 13.14 🎯 F-38 决策：TaskCreate / TaskUpdate → Receipt Checklist（2026-08-04）

**背景**：Claude Code 的 `TaskCreate` / `TaskUpdate` 在 stream-json 中是普通 `tool_use` + `tool_result`。继续沿用 F-37 的 generic tool route 会在 thread 中产生 `● TaskCreate(...)` / `⎿ ...`，但无法表达任务清单。

**决议**：

1. Claude bridge 仅在 matching `tool_result` 确认成功后更新 provider-session task state；TaskCreate 使用 result 返回的真实 ID，禁止 subject hash。
2. `EventTaskCreate / EventTaskUpdate` 与 `OutTaskCreate / OutTaskUpdate` 每次携带完整 typed snapshot。Gateway 不保存 task state，也不解析 Claude field。
3. Feishu adapter 不把成功 task tool 投到 thread，而是调用当前 `ReplyTo=userMsgID` 的 receipt setter。
4. Receipt card 在 answer entries 后、footer 前加入**一个** markdown checklist element。任务不是 rolling `LogEntry`，不会随着旧日志 eviction 单独丢失。
5. checklist 状态视觉由 Feishu 自治：`⏳ pending` / `🔄 in progress` / `✅ completed`；in-progress 优先，completed 空间不足时优先省略。
6. 结果无法确认、协议漂移或失败时不猜测状态：bridge warn + generic ToolEnd fallback。

**架构边界**：Claude-native `subject / activeForm / taskId` 仅存在于 `bridge/claudecode`；跨层后只见 generic `TaskItem{ID, Subject, ActiveForm, Status}`。`ReplyTo=currentTurnUserMsgID` 仍是唯一关联信息，Channel 不回写旧 turn 的 receipt。

**详细规格**：[`docs/feat/F-38-task-checklist.md`](../feat/F-38-task-checklist.md) + §18。

### 13.14 � F-38 tool-event merge + `/tools on|off` toggle (2026-08-04)

**背景**:§13.12 反转后的方案仍然把每个 `OutToolStart` 和 `OutToolEnd` 当作**两条独立** thread reply 投递。Hot agent 一次 turn 调 10 个工具 = 20 条 thread reply,视觉噪声 + 限速成本都过大。同时用户没有 per-chat 开关控制工具调用是否显示。F-39 同时解决这两点:**(a) 合并渲染**——每对 tool 是**一条** thread reply(call + result 通过 PATCH 同一 message_id);**(b) per-chat toggle**——`/tools on|off` 控制可见性,默认 Hide(quiet by default)。

**新决议**:

1. **`OutToolStart` + `OutToolEnd` 合并为一条 thread reply**:
   - Feishu adapter 维护 per-turn FIFO `toolEventBuf[userMsgID] → []toolEventEntry{startMsgID, startBody}`
   - `OutToolStart` 通过 `postThreadReplyWithID(...)` 发新 thread reply,记下 message_id
   - `OutToolEnd` 到达时 `popToolStart` 取 front entry,用 `mergeToolReply(startMsgID, startBody + "\n" + resultBody)` PATCH 同一 reply
   - 用户看到:同一条 chat message 内 `● Bash(ls)` + `⎿  💻 Bash → 3 lines` 两行,而不是两条独立气泡
   - 失败开放:orphan End(buffer miss)或 PATCH 失败 → fallback 到 `postThreadReply`(发新 reply),永不静默丢数据

2. **`ToolsMode` per-chat 状态**:
   - `ChatSession.ToolsMode`:`ToolsModeHide`(默认)/ `ToolsModeShow`
   - 持久化为 `ChatSessionEntry.ToolsMode`(JSON omitempty;旧 `chat_sessions.json` 无该字段时零值 fallback 到 Hide)
   - setter `ChatSession.SetToolsMode` + getter `ChatSession.ToolsMode()`

3. **`/tools on|off` slash command**(镜像 `/think` 但默认方向相反):
   - 三种调用:`/tools on`、`/tools off`、`/tools`(无参 = 显示状态)
   - 接受别名 `show` / `hide`
   - **默认 Hide**(vs `/think` 默认 Show)——理由:tool spam 是 agent stream 中最吵的部分,多数用户不要;opt-in 才显示

4. **runtime EventHandler gate**(`cmd/nightme/run.go::newEventHandler`):
   - 紧跟现有 ThinkMode gate
   - 当 `cs.ToolsMode() == ToolsModeHide && (out.Kind == OutToolStart || out.Kind == OutToolEnd)` → 静默丢弃 + info log
   - 其他 OutboundKind(`OutText` / `OutResult` / OutThinking / OutCompaction / OutInit / OutUsage`)不受影响
   - 与 ThinkMode gate 正交:两个 gate 可独立配置

**OutboundKind 路径拆分(F-38 thread-merge 设计)**:

| Kind | 形态 | main chat | thread 表现 |
|------|------|-----------|-------------|
| `OutToolStart` | ReplyInThread + **merge-able** | 隐藏 | 发新 thread reply,记 `startMsgID`(后续 End 来 PATCH 这条) |
| `OutToolEnd` | ReplyInThread + **merge** | 隐藏 | PATCH 同 `startMsgID`,body = startBody + "\n" + resultBody;miss / PATCH 失败时 fallback 发新 reply |
| `OutThinking` | ReplyInThread(`postThreadMarkdownReply`,独立) | 隐藏 | lark_md card,**不**与 Tool 合并 |
| `OutCompaction` | ReplyInThreadAndChat(独立) | 可见 | `✶ Compacting conversation…` |

> 与 §13.12 的差异:§13.12 时代 `OutToolStart` + `OutToolEnd` 是两条独立 thread reply;F-38 后是**一条** reply(call + result 合并)。Feishu-specific 渲染细节,**不动** `OutboundMessage` schema / Gateway / ChatSession。

**架构不变式保留**:
- `OutboundMessage` shape 不变(无新 Kind、无新字段、`Tool *ToolInfo` 仍是 typed primitive)
- `ChatSession` 不 import `channel/feishu`(不变)
- Channel interface 不暴露 `ToolsMode` / 任何渲染细节(不变)
- 合并 vs 分开发是 **Feishu 自治**的渲染决策(Echo / Slack / Web 各自决定,不复制飞书方案)
- §1.4 边界规则:tool 概念跨层仍是 typed `ToolInfo`;Feishu 自决是否合并
- 1 turn : 1 userMsgID(SPEC §2.2):FIFO buffer 由 tools-per-turn 自然界定,无需 turn-end 显式清理

**Feishu API 约束**:
- `PUT /im/v1/messages/{id}` 支持编辑 text / post 类型 thread reply
- 单条消息最多 20 次编辑(agent 单 turn 工具数远低于此)
- 编辑时间窗 24h(覆盖任何现实 tool latency)
- msg_type 必须匹配(不能 text → card 或反之)

**与 §13.12 的关系**:§13.12 反转了折叠→thread 决策,但仍按"每事件一条 reply"投递。F-38 在 §13.12 基础上**进一步**合并 OutToolStart+OutToolEnd 为一条 reply。OutThinking / OutCompaction 仍按 §13.12 各自一条 reply(它们之间没有 pair 关系)。

**实施步骤**(详见 F-38 §8):

1. **Foundation**:`internal/registry/tools_mode.go` + `internal/chatsession/toolsmode.go`;`ChatSession.toolsMode` + `SetToolsMode` / `ToolsMode()`;`Manager.RestoreFromRegistry` 恢复;`ChatSessionEntry.ToolsMode` JSON 字段
2. **Slash command + gate**:`internal/gateway/handlers_tools.go`(镜像 `handleThink`);`RegisterChatSessionCommands` 注册;`cmd/nightme/run.go::newEventHandler` 在 ThinkMode gate 后加 ToolsMode gate
3. **Feishu merge**:`internal/channel/feishu/tool_thread_merge.go`(`toolEventEntry` + `pushToolStart` / `popToolStart` / `clearToolEvents` / `clearAllToolEvents` / `mergeToolReply`);`Adapter.toolEventBuf` + `mergeTextFunc` hookable field;`postThreadReplyWithID` sibling helper;`OutToolStart` / `OutToolEnd` cases 重写;`Adapter.Stop` 调 `clearAllToolEvents`
4. **Tests**:registry round-trip + missing-field + omitempty;chatsession type-alias + default-is-Hide;handlers / event handler gate + 独立性;Feishu FIFO + miss + empty msg_id + parallel tool_use + cross-turn isolation + PATCH failure fallback

**Backlog**(F-38 out of scope):
- Per-tool 粒度 toggle(目前只 binary on/off)
- 跨 Channel 的合并标准(Echo / Slack / Web 各自决定)
- Tool output preview(result line 永远是 `summarizeToolResult` 单行摘要,永不 dump 原文)
- Auto-disable after N turns(用户 opt-out 总是显式)

**详细设计**:见 [`docs/feat/F-39-tool-merge-and-toggle.md`](../feat/F-39-tool-merge-and-toggle.md) + `docs/SPEC.md` §0.7 + §3.1.3。

## 14. 变更日志

- **2026-08-03** - 加入 §11-§13: Feishu msg_type 全集参考、OutboundKind → Feishu 渲染映射表、审计结果(1 bug + 4 澄清)。基于 `internal/channel/feishu/*` 与 `internal/gateway/*` 现状。
- **2026-08-03(同日增量)** - 加入 §13.6-§13.9:Devin 拍板 Thinking/ToolStart/ToolEnd 全部折叠;列出 3 个 UX 折叠粒度方案(per-event / aggregate-paired / category-aggregate)+ 4 个待确认问题。等 Devin 决定后启动 PR。
- **2026-08-03(同日再增量)** - 加入 §13.10:Devin 发现 `OutboundMessage.ReplyTo` 字段被消费在内部 receipt map 但**从未投递为 Feishu `root_id`**,所有 bot 回复都是顶层消息,与用户消息无视觉连接。SDK 字段 `larkim.CreateMessageReqBody.RootId` 已存在但代码没用。F-26 v1.1 设计文档 `ReplyTo 非空 → 必须镇定到该 userMsgID(用 ReplyMessage API 或已有 receipt)` 在 v1.3 refactor 中丢失。提供 A/B/C 三种修复范围(最小/中等/完整)+ 4 个待确认问题。
- **2026-08-03(同日三增量)** - 加入 §13.11(F-33 决策记录):D1 ChatType 不进 Gateway + D2 topic_group 不特殊处理 + D3 `ReplyTo = ParentId`(RootId 不进 nightme)+ D4 任何 Channel 都不引入 thread 概念。落地 chatID 数据模型系统性清理,关闭 inbound 方向 ReplyTo 接线缺失。详见 [`docs/feat/F-33-simplify-chatid-data-model.md`](../feat/F-33-simplify-chatid-data-model.md)。
- **2026-08-04** - 加入 §13.12(F-thread-route 决策反转):折叠方案(§13.6-§13.9)实机验证失败,反转决策 → OutThinking / OutToolStart / OutToolEnd / OutCompaction 作为独立 thread reply 投递;receipt card 收窄到只承载最终答复 + 元数据;OutToolEnd 走类型感知摘要(`summarize_tool.go`)。新建 [`docs/feat/F-37-tool-thread-routing.md`](../feat/F-37-tool-thread-routing.md) 作为本 feature 的权威文档。`docs/SPEC.md` §0.3 同步加变更摘要;§15 实施计划待修订(下个 commit)。
- **2026-08-04(同日增量 F-38)** - 加入 §13.14 决策:Claude TaskCreate / TaskUpdate → Receipt Checklist。Bridge 在 `tool_result` 确认成功后发完整 typed task snapshot;Gateway 增加 `OutTaskCreate` / `OutTaskUpdate` 无状态透传;Feishu receipt 在 answer entries 后、footer 前加入单一 markdown checklist element,成功 task tool 不再投递到 thread。新建 [`docs/feat/F-38-task-checklist.md`](../feat/F-38-task-checklist.md) 作为权威设计。`docs/SPEC.md` §0.6 + §11 backlog 同步登记。

- **2026-08-04(同日增量)** - 加入 §13.14(F-38 tool-merge + `/tools` toggle):§13.12 的 OutToolStart + OutToolEnd 各自发一条 thread reply 的方案在 10 工具/turn 的 agent 上视觉噪声过大,反转 → OutToolEnd PATCH 同一 message_id 的 start reply(call + result 合并为一条);新增 `ChatSession.ToolsMode`(默认 Hide,quiet by default)+ `/tools on|off` slash command + runtime EventHandler gate。详见 [`docs/feat/F-38-tool-merge-and-toggle.md`](../feat/F-38-tool-merge-and-toggle.md) + `docs/SPEC.md` §0.7 + §3.1.3。
- **v0.3 ~ v1.3** - 早期章节(背景、OpenClaw 调研、迁移方案、已知坑等)保留;参见章节顶部 Status 行。

## 15. v1.3.x 实施计划

### 15.0 状态(2026-08-04 F-thread-route + F-38 反转后)

�️ **本节已被 §13.12(F-37 thread-route)+ §13.14(F-38 tool-merge)+ [`docs/feat/F-37-tool-thread-routing.md`](../feat/F-37-tool-thread-routing.md) + [`docs/feat/F-38-tool-merge-and-toggle.md`](../feat/F-38-tool-merge-and-toggle.md) 反转**。

- 原 §15 目标"折叠 + reply-in-thread" → 折叠方案被 §13.12 推翻
- §13.1 修复不再需要(thinkingPrefix 不再被 strip)
- §13.10 reply-in-thread 部分保留(§13.12 复用其基础设施 `sendViaLarkReply` + `SendMessageText` 的 rootID 参数)
- §13.12 的"每事件一条 reply"被 §13.14 进一步合并:`OutToolStart` + `OutToolEnd` → 一条 thread reply(Feishu `PUT /im/v1/messages/{id}` PATCH 同一 message_id)
- §13.14 新增 `ChatSession.ToolsMode`(默认 Hide)+ `/tools on|off` slash command + runtime EventHandler gate
- 新实施计划见 F-37 §3.1 + F-38 §8 文件级变更清单

历史保留:§15.1 ~ §15.5 描述折叠方案的旧实施细节,作为决策记录。

---

## 15. v1.3.x 实施计划:折叠 + reply-in-thread(已被反转,见 §15.0)

**目标**: 修 §13.1(OutThinking 折叠死代码)+ §13.10(ReplyTo 未投递为 root_id),实现 Devin 拍板的折叠设计 + reply-in-thread。

**变更范围**: 仅 `internal/channel/feishu/*`;Gateway / Chatsession / agent 包**不动**。

### 15.1 文件级变更清单

| 文件 | 变更 | 说明 |
|------|------|------|
| `internal/channel/feishu/adapter.go` | 1. `OutThinking` case 补回 `[思考] ` 前缀<br>2. `SendContent` / `SendCard` / `sendMessageText` 加 `rootID` 参数<br>3. `sendViaLark` 设 `body.RootId`<br>4. adapter.Send 在 `msg.ReplyTo != ""` 时透传 rootID<br>5. 删除 `OutCommandReply` 注释里的 "no ReplyTo threading"<br>6. `OutCard` case 也透传 rootID | §15.2 详情 |
| `internal/channel/feishu/receipt_event.go` | 1. `EventToolStart` / `EventToolEnd` 新增 `Kind="tool"` 输出<br>2. 加注释:`thinkingPrefix` MUST be present | §15.3 详情 |
| `internal/channel/feishu/adapter.go` (`buildReceiptCard`) | 新增 `Kind="tool"` 折叠分支(header + body 同 thinking 结构) | §15.3 详情 |
| `internal/channel/feishu/receipt_test.go` | 加测试用例 | §15.4 详情 |
| `internal/channel/feishu/adapter_test.go` | 加测试用例 | §15.4 详情 |
| `docs/channel/feishu.md` | §11-§15 已落地 | 本节 |

### 15.2 §13.10 修复细节(reply-in-thread)

**`sendContent` 加 `rootID` 参数**:

```go
// 现有签名:
func (a *Adapter) sendContent(ctx context.Context, chatID, msgType, content string) (string, error)
// 改后:
func (a *Adapter) sendContent(ctx context.Context, chatID, msgType, content, rootID string) (string, error)
```

**`SendCard` 透传 rootID**:

```go
// 现有:
func (a *Adapter) SendCard(ctx context.Context, chatID, content string) (string, error)
// 改后:
func (a *Adapter) SendCard(ctx context.Context, chatID, content, rootID string) (string, error)
```

**`SendMessageText` 透传 rootID**: 同样模式。`OutCommandReply` 路径的调用从 `SendMessageText(ctx, chatID, text)` 改成 `SendMessageText(ctx, chatID, text, msg.ReplyTo)`。

**`sendViaLark` dispatch 到 Reply / Create(SDK 修正版)**:

最初计划是设 `body.RootId = &rootID`(SDK 字段 `larkim.CreateMessageReqBody.RootId`,参考 `oapi-sdk-go/v3@v3.5.3/service/im/v1/model.go:2125`)。**修正**: `CreateMessageReqBody` **没有** `RootId` 字段--只有 `ReplyMessageResp` 数据结构有 `RootId`(响应体)。真正能设 root_id 的 API 是 `POST /im/v1/messages/{message_id}/reply`(path 参数即 root_id),所以 `sendViaLark` 拆成两条:

```go
func (a *Adapter) sendViaLark(ctx, chatID, msgType, content, rootID string) (string, error) {
    if rootID != "" {
        return a.sendViaLarkReply(ctx, rootID, msgType, content)
    }
    return a.sendViaLarkCreate(ctx, chatID, msgType, content)
}

// sendViaLarkReply: POST /im/v1/messages/{rootID}/reply
// sendViaLarkCreate: POST /im/v1/messages  (top-level)
```

两者都是 `sendViaLark` 的实现细节;`sendContent` 只通过 `sendFunc` 函数字段(可被测试 mock)调用,不需要知道 API 拆分。

**Terminal-code fallback**(`sendContent` 包装):

```go
func (a *Adapter) sendContent(ctx, chatID, msgType, content, rootID string) (string, error) {
    // ... get send from a.sendFunc or a.sendViaLark ...
    msgID, err := send(ctx, chatID, msgType, content, rootID)
    if err != nil && rootID != "" && isFeishuTerminalMessageCode(err) {
        // v1.3.x (§13.10 fallback): the target user message was
        // recalled (230011) or deleted (231003). The Reply
        // endpoint is permanently invalid for that root_id;
        // retry as top-level Create so the user still sees a
        // message. Mirrors openclaw-lark's
        // runWithMessageUnavailableGuard (src/core/message-unavailable.ts).
        a.logger.Warn("feishu: reply target unavailable, falling back to top-level",
            "root_id", rootID, "msg_type", msgType, "err", err)
        return send(ctx, chatID, msgType, content, "")
    }
    return msgID, err
}
```

`isFeishuTerminalMessageCode(err)` 检测 Feishu 错误码 230011 / 231003(两者表示 user message 被发起人撤回/删除,root_id 永远不可用)。格式兼容 `"code NNNNN"` 和 `"code:NNNNN"` 两种形态,加 `*larkcore.CodeError` unwrap 防御。

**`sendFunc` / `updateFunc` 字段类型更新**:

```go
type sendMessageFunc func(ctx context.Context, chatID, msgType, content, rootID string) (string, error)
type updateMessageFunc func(ctx context.Context, messageID, content string) error  // PATCH 不变
```

**Adapter.Send dispatcher 透传**:

```go
case gateway.OutCard:
    // ...
    content, err := buildInteractiveCard(msg.Card)
    if err != nil { return err }
    _, err = a.sendContent(ctx, msg.ChatID, interactiveMessageType, content, msg.ReplyTo)  // ← 加 ReplyTo
    return err

case gateway.OutCommandReply:
    // ...
    _, err := a.SendMessageText(ctx, msg.ChatID, msg.Text, msg.ReplyTo)  // ← 加 ReplyTo
    return err
```

**Receipt cold-start 路径**(在 `receiptFor` 内):

```go
msgID, err := a.SendCard(ctx, chatID, cardBody, userMsgID)  // ← userMsgID 作为 rootID
```

**Cold-start 工具函数 `buildColdStartCard`** 不变(body 一样,只换调用 API)。

**PatchMessage 路径**: **不动** -- Feishu PATCH `/im/v1/messages/{id}` 自动保留被 PATCH 消息的原始 root_id,无需在 PATCH body 里重复传。

### 15.3 §13.6 折叠修复细节

**Adapter `OutThinking` case 补回前缀**:

```go
// 现有:
case gateway.OutThinking:
    receipt := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo)
    if receipt == nil { return nil }
    return receipt.Append(ctx, agent.AgentEvent{
        Kind: agent.EventText,
        Text: msg.Text,
    })

// 改后:
case gateway.OutThinking:
    receipt := a.receiptFor(ctx, msg.ChatID, msg.ReplyTo)
    if receipt == nil { return nil }
    return receipt.Append(ctx, agent.AgentEvent{
        Kind: agent.EventText,
        Text: thinkingPrefix + msg.Text,  // ← 补回前缀,让 receipt_event.go 的 detection catch
    })
```

**`receipt_event.go` 给 tool 标 Kind**:

```go
case agent.EventToolStart:
    if ev.ToolStart == nil { return LogEntry{}, false }
    name := ev.ToolStart.Name
    if name == "" { name = "tool" }
    text := name
    if ev.ToolStart.Args != "" {
        text = name + "(" + ev.ToolStart.Args + ")"
    }
    return LogEntry{
        Time: now,
        Icon: "🔧",
        Text: truncateForLog(text, perEntryMaxBytes),
        Kind: "tool",  // ← 新增,触发 buildReceiptCard 折叠
    }, true

case agent.EventToolEnd:
    if ev.ToolEnd == nil { return LogEntry{}, false }
    name := ev.ToolEnd.Name
    if name == "" { name = "tool" }
    var text string
    if ev.ToolEnd.Err != nil {
        text = fmt.Sprintf("%s failed: %s", name, ev.ToolEnd.Err.Error())
    } else if ev.ToolEnd.Output != "" {
        text = fmt.Sprintf("%s → %s", name, ev.ToolEnd.Output)
    } else {
        text = name + " done"
    }
    icon := "✅"
    if ev.ToolEnd.Err != nil { icon = "❌" }
    return LogEntry{
        Time: now,
        Icon: icon,
        Text: truncateForLog(text, perEntryMaxBytes),
        Kind: "tool",  // ← 新增
    }, true
```

**`buildReceiptCard` 新增 `Kind="tool"` 折叠分支**(紧跟现有 `Kind="thinking"` 分支):

```go
if e.Kind == "tool" {
    elements = append(elements, map[string]any{
        "tag":      "collapsible_panel",
        "expanded": false,
        "header": map[string]any{
            "title": map[string]any{
                "tag":     "markdown",
                "content": e.Icon + " " + e.Text,  // 复用 e.Text 作为 header 短描述
            },
            "vertical_align": "center",
            "icon": map[string]any{
                "tag":   "standard_icon",
                "token": "down-s...ined",
                "size":  "16px 16px",
            },
            "icon_position":       "follow_text",
            "icon_expanded_angle": -180,
        },
        "border": map[string]any{
            "color":        "grey",
            "corner_radius": "5px",
        },
        "vertical_spacing": "8px",
        "padding":          "8px 8px 8px 8px",
        "elements": []map[string]any{
            {
                "tag":       "markdown",
                "content":   e.Text,
                "text_size": "notation",
            },
        },
    })
    continue
}
```

**Panel header 文案规则**(由 `receipt_event.go` 在生成 entry 时决定):
- Tool Start: `🔧 tool_name(args)` → `Icon="🔧"`, `Text="Read(/a.py)"`
- Tool End (成功): `✅ tool_name → output_first_line`(`output` 完整在 body)
- Tool End (失败): `❌ tool_name failed: err`

**简化策略**:`receipt_event.go` 把 icon + name + 短描述拼成 `Icon + Text`,`buildReceiptCard` 直接用 `e.Icon + " " + e.Text` 作为 header title,`e.Text` 整段作为展开 body。**不新增字段**,改动最小。

### 15.4 测试计划

**`receipt_event_test.go` 新增**:

| 用例 | 验证 |
|------|------|
| `TestEventToEntry_ToolStart_KindTool` | `EventToolStart{Name:"Read", Args:"/a.py"}` → entry `Kind="tool"`, `Icon="🔧"`, `Text="Read(/a.py)"` |
| `TestEventToEntry_ToolEnd_Success_KindTool` | `EventToolEnd{Name:"Read", Output:"47 lines"}` → entry `Kind="tool"`, `Icon="✅"`, `Text="Read → 47 lines"` |
| `TestEventToEntry_ToolEnd_Failure_KindTool` | `EventToolEnd{Err: errors.New("perm denied")}` → entry `Kind="tool"`, `Icon="❌"`, `Text="Read failed: perm denied"` |
| `TestEventToEntry_ThinkingPrefix_DetectsThinking` | `EventText{Text:"[思考] hello"}` → entry `Kind="thinking"`(已存在,确认 prefix detection 仍然 work) |

**`receipt_test.go` 新增**:

| 用例 | 验证 |
|------|------|
| `TestReceipt_BuildCard_FoldedEntries` | append thinking + tool_start + tool_end + reply → 生成的 card JSON 包含 3 个 `collapsible_panel`(前 3 个)+ 1 个平铺 `markdown`(reply) |
| `TestReceipt_BuildCard_AllCollapsed` | 默认 `expanded: false`(思考 + 工具全部折叠) |
| `TestReceipt_BuildCard_NoFoldedForUsage` | `EventUsage` / `EventCompaction` / `EventInit` / `EventResult` 不走折叠分支 |

**`adapter_test.go` 新增**:

| 用例 | 验证 |
|------|------|
| `TestAdapter_Send_OutThinking_AppendsWithPrefix` | `Send(OutboundMessage{Kind: OutThinking, Text: "reasoning"})` 走到 receipt.Append 时,`event.Text == "[思考] reasoning"` |
| `TestAdapter_Send_OutCard_PassesReplyTo` | `Send(OutboundMessage{Kind: OutCard, ReplyTo: "user_123", Card: ...})` → `sendContent` 收到的 `rootID == "user_123"` |
| `TestAdapter_Send_OutCommandReply_PassesReplyTo` | `Send(OutboundMessage{Kind: OutCommandReply, ReplyTo: "user_123", Text: "..."})` → `SendMessageText` 收到的 `rootID == "user_123"` |
| `TestAdapter_ReceiptFor_ColdStartPassesUserMsgID` | 冷启动 receipt 时,`SendCard` 收到的 `rootID == msg.ReplyTo` |

**`sendViaLark` 单元测试**(可能已有 mock,需补充):

| 用例 | 验证 |
|------|------|
| `TestSendViaLark_RootIdSet` | mock `sendFunc` 记录调用 `rootID`,验证传入非空 |
| `TestSendViaLark_NoRootId` | 调用时 `rootID == ""` → mock 收到 `rootID=""`(top-level Create) |
| `TestSendViaLark_TerminalCodeFallsBackToCreate` | Reply mock 返回 `"...code 230011"` → sendContent 自动重试 mock `rootID=""`,应收到 `om_created` |
| `TestSendViaLark_NonTerminalErrorPropagates` | Reply mock 返回 `"...code 230020"` 或 transport error → sendContent 不重试,原错误传递 |
| `TestSendViaLark_NoRootIDSkipsReply` | `rootID == ""` 路径下 mock 只收 Create 调用 |
| `TestIsFeishuTerminalMessageCode` | 6 case 单测: 230011 / 231003 → true,230020 / transport / unrelated / nil → false |

### 15.5 验收

| 项 | 状态 |
|----|------|
| go build | 必过 |
| go test ./... | 必过(含新增测试) |
| go vet | 必过 |
| golangci-lint | 必过 |
| E2E: 飞书 DM round-trip | **必跑**(Feishu 测试号 + 真实消息往返,验证 root_id 在 Feishu 端可见) |
| 视觉截图: receipt card 折叠态 | **推荐**(折叠 + 展开两态) |

### 15.6 不在本 PR scope(backlog)

- §13.2 OutInit Meta 字段渲染(foot note 扩展)
- §13.3 OutResult 限额放宽(600→2048 或折叠展开全文)
- §13.10 方案 C `reply_in_thread` 模式(用户可配)
- §13.7 方案 2 / 方案 3(工具调用聚合)
- §13.4 OutboundAttachment kind(agent 生成图片/文件)

### 15.7 §13.10 Fallback:Reply target unavailable(2026-08-03 增量)

**问题**: Reply API 在 user message 被撤回(230011)或删除(231003)时**永久失败**。Pre-fix 行为:Create API 不读 root_id,OutCard / OutCommandReply 在 msg.ReplyTo 是 dead message id 时仍然能发出(只是没视觉连接)。Post-fix v1.3.x 把所有 reply 路径都迁到 Message.Reply,**会硬失败**。

**openclaw-lark 模式**(src/core/message-unavailable.ts):用 `runWithMessageUnavailableGuard` 包装每次 API 调用,识别 230011/231003 后:
1. 把 message_id 加入 30 分钟 TTL 的 unavailability cache
2. 后续该 message_id 的所有 API 调用 fast-fail,避免日志 spam
3. 抛 `MessageUnavailableError` 给上游决定如何处理(abort / fallback / retry)

**nightme v1.3.x 简化版**(已落地):

只在 `sendContent` 加一层 fallback:**仅当 Reply 失败且错误码是 230011/231003 时**,retry 一次不带 rootID 的 Create,然后日志 warn。**不做**全局 cache(per-turn retry storm 在我们的 hot path 里基本不会发生,加了反而增复杂度)。

```go
msgID, err := send(ctx, chatID, msgType, content, rootID)
if err != nil && rootID != "" && isFeishuTerminalMessageCode(err) {
    a.logger.Warn("feishu: reply target unavailable, falling back to top-level", ...)
    return send(ctx, chatID, msgType, content, "")
}
return msgID, err
```

`isFeishuTerminalMessageCode(err)` 检测 230011/231003: 格式兼容 `"code NNNNN"` 和 `"code:NNNNN"`,加 `*larkcore.CodeError` unwrap 防御 SDK 未来变化。

**测试覆盖**:
- `TestSendViaLark_TerminalCodeFallsBackToCreate` - 230011 和 231003 都触发 fallback
- `TestSendViaLark_NonTerminalErrorPropagates` - 230020 和 transport error 不触发 fallback,原错误向上传
- `TestIsFeishuTerminalMessageCode` - 6 case 单测

**与 openclaw-lark 的差异**(值得记录):
- **不做 cache**: openclaw-lark 把 message_id 存进 30min TTL map;我们只在 sendContent 这一层做单次 fallback。开销更小,但未来如果出现"同一 dead root_id 被反复请求"的场景,需补上 cache。
- **不打 sentinel error 类型**: openclaw-lark 抛 `MessageUnavailableError`;我们走标准 error + log warn,上层 gateway 不需特判。

**后续优化(backlog)**:
- 加 30min TTL cache(若 230011 触发频次超过预期)
- 上层 Chatsession 接收 230011 / 231003 时,emit MessageState=error 并中断 turn(避免 agent 继续发无主回复)

## 16. Rate Limit 控制(F-37,2026-08-04)

### 16.1 问题

nightme 的 feishu adapter hot path 完全同步 —— readPump → `EventCallback` → `channel.Send` → SDK call,无 backpressure、无重试(除 230011/231003 fallback)。**agent turn 内 receipt PATCH storm**(一个 turn 5-20 次 PATCH)+ 多 chat 并发 → 飞书触发 `230001` / `230020` 限流码 → event 丢失。

### 16.2 方案:feishu 包内全局 token bucket

在 `internal/channel/feishu/ratelimit.go` 落地一个 **单桶** token bucket(无 per-op-kind 分桶),覆盖所有 5 类出口 API(send / reply / patch / reaction / upload)。每个 SDK call 前 `a.limiter.Wait(ctx)`,**预防**触发限流,而不是事后补救。

**为什么不分多桶**:所有 5 类 API 文档限速完全相同(1000 QPM / 50 QPS per app),且 nightme 热路径受 per-user 5 QPS 约束,单桶足够。

**为什么 burst=1**:飞书硬上限是绝对值,不留弹性 —— 宁可慢一点,也不要触发 230001。

**为什么 lazy refill**:不启后台 goroutine,token refill 在 `Wait()` 调用时按 elapsed 时间计算。零 goroutine 泄漏风险。

### 16.3 实测飞书限频(2026-08-04 查 open.feishu.cn)

| API | App QPS | App QPM | Per-resource |
|---|---|---|---|
| Send / Reply message | 50 | 1000 | **5 QPS per user** + **5 QPS per group**(群内机器人共享) |
| PATCH message | 50 | 1000 | **5 QPS per message_id** |
| Delete / AddReaction / Upload | 50 | 1000 | — |

nightme 热路径被 **per-user 5 QPS** 约束 —— 单 chat PATCH storm 受 per-message_id 5 QPS 限制(由 `renderLocked` mutex 天然满足),多 chat 并发受 per-user 5 QPS 限制。

### 16.4 配置

```yaml
# config.yaml
feishu:
  rate_limit:           # 可选;不填 = StrictDefault
    rate_per_sec: 5     # 每秒补充令牌数(默认 5,贴 per-user 硬限)
    burst: 1            # 桶容量(默认 1,无突发)
```

**StrictDefault** (`internal/channel/feishu/ratelimit.go`)：

```go
var StrictDefault = config.FeishuRateLimitConfig{
    RatePerSec: 5,  // per-user 硬限
    Burst:      1,  // 无突发
}
```

**调高的代价**:rate_per_sec > 5 → 单 chat PATCH storm 可能短暂触顶 per-user 限流;burst > 1 → 启动期或空闲后突发达 N 个调用,可能触顶。

### 16.5 接入点

`internal/channel/feishu/adapter.go` 4 个底出口,每个 SDK call 前都过 `Wait`：

| 函数 | 加 Wait 的位置 |
|---|---|
| `sendViaLarkCreate` | `a.limiter.Wait(ctx)` 在 `client.Im.V1.Message.Create(...)` 之前 |
| `sendViaLarkReply` | 同上,在 `Message.Reply(...)` 之前 |
| `updateViaLark` | 在 `Message.Patch(...)` 之前 |
| `AddReaction` | 在 `MessageReaction.Create(...)` 之前 |

**`sendContent` 包装层不动**:rootID fallback 路径(230011 → top-level Create)第二次走 `sendViaLarkCreate` 仍会经 Wait,**单桶自动覆盖**。

**`GetBotIdentity` 不走 limiter**:启动期低频,且不走 IM 配额。

**Heartbeat PATCH 也走 `updateViaLark`** → 自动经 Wait。30 min 一次,远低于 5 QPS,**不需特判**。

### 16.6 与现有组件的边界

- **不改 `Channel` 接口契约**(`Send` 仍 fire-and-ack)
- **不与 Layer 1 重试耦合**(retry 在 sendContent 外层,limiter 在 SDK call 内层;二者正交)
- **不影响 per-message_id 5 QPS**(`renderLocked` mutex 天然满足)

### 16.7 监控埋点

Wait 阻塞 > 100ms 时记 debug log:

```go
l.logger.Debug("feishu rate limit blocked",
    "wait_ms", waitDur.Milliseconds(),
    "tokens", l.tokens,
)
```

不记 INFO —— 频繁触发说明配置需调整,但 hot path 日志噪音太大。

### 16.8 测试

`ratelimit_test.go` 6 个单测:

| 用例 | 验证 |
|---|---|
| `TestNewLimiter_StrictDefault` | cfg=nil → RatePerSec=5, Burst=1 |
| `TestLimiter_ConfigOverride` | cfg={RatePerSec:10, Burst:2} → 应用生效 |
| `TestLimiter_InitialBurst` | 初始 tokens=1;连续 2 次 acquire:第一次立即成功,第二次 wait ≥ 200ms |
| `TestLimiter_Refill` | fakeClock.Advance(200ms) → tokens 重填 |
| `TestLimiter_ContextCancel` | Wait 阻塞中 ctx.Done() 立即返回 |
| `TestLimiter_LongRunNoOvershoot` | fakeClock 跑 10s,acquire 51 次(5/s × 10 + initial burst),等待总和符合配置 |

`adapter_test.go` 集成测试:
- `TestAdapter_RateLimit_PATCHStormThrottled`:配 StrictDefault;mock sendFunc 记录 timestamp;触发 20 个连续 OutText → mock 收到的 timestamp 间隔 ≥ 200ms

### 16.9 详细规格

详见 [`docs/feat/F-35-ratelimit.md`](../feat/F-35-ratelimit.md)。

### 16.10 与 receipt PATCH storm 的 UX 权衡

Receipt PATCH storm（一个 agent turn 内 receipt 被 PATCH 多次）受 F-37 limiter 串行化。**测算**：

- 每个 PATCH 过 `Wait()`，等待 ~200ms（5 QPS）
- 10 events 的 PATCH storm 总耗时 ≈ 1.8s
- 用户视觉：receipt 卡片内容更新稍慢（动画可见），但绝不触顶飞书限流

如果未来需要更激进的 PATCH 频率，可考虑:
- 提高 `feishu.rate_limit.rate_per_sec`（牺牲限流保护）
- 改 PATCH 为事件合并（多个 event 攒成一次 PATCH，独立 feature）

**当前选择**：保守 5 QPS，**不暴露** override（避免误调高触发 230001）。

---

## 17. Transient Retry(F-36,2026-08-04)

### 17.1 与 F-37 的关系

F-37 是"事前预防"（防 230001 限流），F-36 是"事后补救"（防 transient 网络抖动）。两者正交：

```
sendContent(ctx, chatID, msgType, content, rootID)
  └ WithTransientRetryMsg (F-36 外层)
    └ send() ───┐
                ├ limiter.Wait (F-37 内层)
                └ SDK call
```

每次 retry 都重新过 F-35 limiter：单次 retry 至少等 200ms (5 QPS)。3 次 retry 总耗时 ≈ 1.5s（500ms + 1s backoff + 200ms × 3 limiter wait）。

### 17.2 错误分类（IsTransient）

| 类别 | 例子 | 处理 |
|---|---|---|
| **Transient**（重试） | `net.Error.Timeout()` / `io.EOF` / `syscall.ECONNRESET,EPIPE` / "connection reset" / "broken pipe" / "i/o timeout" / "TLS handshake timeout" / "connection refused" / "no such host" | 指数退避重试 |
| **Permanent**（不重试） | 230011 / 231003（terminal → fallback）/ 230001（rate-limit → limiter 应已防住）/ 其他飞书永久错误码 | 立即返回 |

### 17.3 DefaultRetryConfig

```go
MaxAttempts:    3,                  // initial + 2 retries
InitialBackoff: 500ms,
MaxBackoff:     5s,
JitterPercent:  0.25,               // ±25% 防 thundering herd
```

零 / 负值由 `RetryConfig.normalize()` 静默回退到默认值。

### 17.4 接入点

4 个 SDK call 出口都包 retry：

| 函数 | Op 名 | 备注 |
|---|---|---|
| `sendContent` 主路径 | `"send"` | 含 rootID fallback 也走 retry |
| `sendContent` fallback | `"send_top_level"` | fallback 后仍走 retry 救活瞬时失败 |
| `updateViaLark` | `"patch_message"` | 拆 `patchMessageOnce` 给 retry 调 |
| `AddReaction` | `"add_reaction"` | 拆 `addReactionOnce` 给 retry 调 |

### 17.5 ctx cancel 优先

`ctx.Done()` 在 retry backoff / limiter wait 任何等待点都立即返回 `ctx.Err()`。daemon shutdown 不被 retry 阻塞。

### 17.6 降级日志

每次降级事件（retry exhausted / ctx cancel / fallback top-level / limiter wait cancel）emit warn 级结构化日志：

| 字段 | 类型 | 含义 |
|---|---|---|
| `degradation` | string | 事件类型（见下） |
| `op` | string | `"send"` / `"send_top_level"` / `"patch_message"` / `"add_reaction"` |
| `attempts` | int | 已尝试次数 |
| `total_wait_ms` | int | 累计等待毫秒 |
| `final_err` | string | 最终错误 |
| `ctx_err` | string | ctx 取消时填 |
| `chat_id` | string | call-site |
| `message_id` | string | call-site |
| `root_id` | string | call-site |
| `msg_type` | string | call-site |
| `reaction_type` | string | call-site |

**事件类型**：
- `retry_exhausted` — retry 3 次都失败
- `ctx_cancel_during_wait` — retry 中 ctx 被 cancel
- `ctx_cancel_at_entry` — entry 时 ctx 已 cancel
- `fallback_to_top_level` — 230011/231003 触发 rootID fallback
- `limiter_wait_cancelled` — limiter.Wait 中 ctx 被 cancel

**grep 模式**（post-analysis）：

```bash
grep 'feishu degradation' /var/log/nightme.log
grep '"degradation":"retry_exhausted"' /var/log/nightme.log   # 最严重
grep '"degradation":"ctx_cancel_during_wait"' /var/log/nightme.log  # daemon shutdown
grep '"degradation":"fallback_to_top_level"' /var/log/nightme.log   # user 撤回过原消息
```

### 17.7 详细规格

详见 [`docs/feat/F-36-transient-retry.md`](../feat/F-36-transient-retry.md)。

## 18. F-38 TaskCreate / TaskUpdate → Receipt Checklist（2026-08-04）

### 18.1 背景

Claude Code 的 `TaskCreate` / `TaskUpdate` 在 `--output-format stream-json` 中是普通 `tool_use`，结果在后续 `user.tool_result` 中。F-37 之后没有 task 概念；继续走 generic tool route 会在 Feishu thread 出现 `● TaskCreate(...)` / `⎿ ...`，丢失结构化任务清单。

### 18.2 方案

1. Claude bridge 仅在 `tool_result` 确认成功后更新 session-local task map/order，并发完整 typed snapshot。
2. Gateway 增加 `OutTaskCreate` / `OutTaskUpdate` + `OutboundMessage.TaskList *agent.TaskListEvent`；不解析 Claude field，不保存 task state。
3. Feishu adapter `Send` 新增两个 case：调用 `receiptFor(ctx, chatID, ReplyTo)` 后执行 `SetTaskList(snapshot)`；不调用 thread reply。
4. receipt `MessageReceipt` 持有 `tasks []agent.TaskItem` 副本，独立于 rolling `LogEntry`。
5. `buildReceiptCard` 在 answer entries 之后、footer 之前插入**一个** markdown checklist element。预算预留 + 50 element + 24KB 防御同时保留。
6. checklist 视觉自治：in-progress 优先、pending 次之、completed 最后；in-progress 有 `ActiveForm` 时显示后缀；空间不足时优先省略 completed 并显示“另有 N 项任务”。

### 18.3 接入点

| 路径 | 文件 |
|---|---|
| Adapter dispatch | `internal/channel/feishu/adapter.go::Adapter.Send` 新增 `OutTaskCreate` / `OutTaskUpdate` 两个 case |
| Receipt state | `internal/channel/feishu/receipt.go::MessageReceipt.tasks` + `SetTaskList` |
| 渲染 | `internal/channel/feishu/receipt_task.go`（新文件）`buildTaskChecklist` 纯函数；`buildReceiptCard` 在 entries 循环后调用一次 |
| 事件契约 | `internal/agent/agent.go`（typed task events）、`internal/gateway/messages.go`（OutboundKind + TaskList 字段）、`internal/gateway/translate.go`（两个 case） |
| Bridge | `internal/bridge/claudecode/stream.go`（pending correlation + result dispatch）、`internal/bridge/claudecode/task.go`（provider-native 解析与 snapshot） |

### 18.4 容量

- 整个 checklist 限定在 `divTextCharLimit` 内，超过时按 in-progress → pending → completed 优先级裁剪，并显示 `…另有 N 项任务`。
- `buildReceiptCard` 在 element reservation 中预留 checklist element，与 entries / footer / hr 一起受 50 element / 24KB 预算约束。
- 同 snapshot 重复导致 `body` 不变，依赖现有 `renderLocked` body diff 跳过 PATCH。

### 18.5 测试

- Bridge 单元测试覆盖：tool_use 不提前发 event；TaskCreate success 提取真实 ID；多 create 聚合；TaskUpdate 改 status/subject/activeForm；deleted 移除；out-of-order 结果按 tool_use_id 关联；解析失败降级；pending record 清理。
- Adapter / receipt 单元测试覆盖：cold-create + 后续 PATCH；glyph 与排序；activeForm；删除；空 snapshot 清空；重复 snapshot 去重；thread 不调用；element / byte 预算。
- 实机：飞书群聊触发多个 TaskCreate/TaskUpdate，确认主 receipt 单卡 PATCH、thread 无任务工具噪音、最终答复优先。

### 18.6 详细规格

[`docs/feat/F-38-task-checklist.md`](../feat/F-38-task-checklist.md)。
