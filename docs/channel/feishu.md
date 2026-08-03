# Feishu Channel — 调研与迁移规划

> **Status**: design → implementation plan (v0.x; minimal scope — 详见 §9)
> **Scope**: nightme 内部 Feishu/Lark IM 适配器 (`internal/channel/feishu/*`)
> **目的**: 把 receipt 从 `msg_type: "text"` 切到 `msg_type: "interactive"`,支持更丰富的卡片体验。本期不引入新元数据(agent_name / provider 透传**延期**),卡片内容用现有 receipt 状态(state + entries)。
> **Related docs**:
> - [F-08-channel-abstraction.md](../feat/F-08-channel-abstraction.md) — Channel interface 与 v1.1 receipt FSM
> - [F-25-input-buffer.md](../feat/F-25-input-buffer.md) — receipt 触发源
> - [F-26-gateway-hub.md](../feat/F-26-gateway-hub.md) — Gateway ↔ Channel 边界
> - [F-22-feishu-onclick-registration.md](../feat/F-22-feishu-onclick-registration.md) — app 鉴权
> - [F-23-heartbeat.md](../feat/F-23-heartbeat.md) — receipt 心跳
> **官方文档**:
> - [Create JSON message content](https://open.feishu.cn/document/server-docs/im-v1/message-content-description/create_json) — 顶层卡片信封
> - [Card JSON 2.0 components](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-json-v2-components/component-json-v2-overview) — 组件列表(`div` / `markdown` / `hr` / `note` 等)
> - [Update sent message card (PATCH)](https://open.feishu.cn/document/server-docs/im-v1/message-card/patch) — 卡片原地更新 API

## 1. 背景:为什么从 text 切到 card

当前 nightme 的 receipt 走纯文本路径(`msg_type: "text"`):
- `internal/channel/feishu/adapter.go:890 SendMessageText` 编码为 `{"text": "..."}` 后 `sendContent` 发出
- `internal/channel/feishu/receipt.go:515 renderLocked` 每个事件 `SendMessageText` 发一条新消息
- 没有 footer / 没有按钮 / 表格需要 markdown hack 才能近似

切到 interactive card 可以:
1. **footer 行** — 展示 `Agent · X | Model · Y | Provider · Z` 这类元数据(对齐 OpenClaw 风格,见 §2)
2. **按钮 / action** — 一键确认、二次确认、复制 session id 等交互
3. **结构化展示** — 工具调用折叠为 `collapsible_panel`、长输出折叠、表格、彩色状态等
4. **原地更新** — 用 PATCH API 改一张卡,而不是发 N 条消息,降低噪音

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

### 2.3 OpenClaw issue #59360 — root cause

- **Title**: "Feishu card message footer causes agent name to appear at message start (Markdown definition list parsing)"
- **现象**: Feishu 的 Markdown 渲染器把 `Agent: main | Model: ... | Provider: ...` 解析成 **Markdown 定义列表**(`key: value`),把第一项的 value(`main`)hoisting 到消息开头
- **结果**: 用户看到的卡片正文最上面突然多一行 `main`(agent 名),footer 反而被解读成普通段落
- **复现**: 发送任意包含 `Key: value | Key2: value2` 的灰色 markdown 卡片即可触发
- **关闭状态**: closed as not planned, 2026-07-20
- **未合并修复**: [PR #84122](https://github.com/openclaw/openclaw/pull/84122) — 把 `Agent: ` 改成 `Agent · `(中点),让渲染器认不出是定义列表
  - 描述: "Feishu's card markdown renderer parses 'Agent: name' as definition-list syntax and hoists the agent name to the top of the rendered message. Switch the key/value separator from ': ' to ' · ' so the footer stays in the footer."

### 2.4 我们的截图

用户截图(2026-06-11)中的红色框:

```
─────────────────────────────────
Agent: main | Model: MiniMax-M2.7 | Provider: minimax
```

—— 与 OpenClaw 的 card note footer 一致,但 nightme 当前的 text 路径**不会**输出这种卡片(没 hr、没灰色),所以截图来自**别的工具**(可能是 OpenClaw/同款渲染)。**这个 bug 在 nightme 切到 card 之前不会触发**;切换后必须规避(见 §6)。

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

**重要**: Feishu `lark_md` **不支持** `<font color='grey'>`。Feishu 官方允许的 inline 颜色用 `<text_tag color='...'>`,允许值:`neutral`、`blue`、`turquoise`、`lime`、`orange`、`violet`、`indigo`、`wathet`、`green`、`yellow`、`red`、`purple`、`carmine`。`neutral` 视觉上接近灰色。OpenClaw `send.ts:768` 写的 `<font color='grey'>` 实际渲染靠 Feishu 容错 —— 本项目**严格用 `<text_tag color='neutral'>`**。

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

`PATCH /im/v1/messages/{message_id}` **整体替换** `card` 字段 —— 不能只改一个 element。所以 nightme 的"原地编辑 receipt"语义就是:**每次状态变化都重新构建完整 card body 然后 PATCH**。

**SDK 提醒**: `lark-oapi-go/v3` 提供两个不同的方法,**`Update` 只能改文本/富文本,不能改卡片**;**卡片必须用 `Patch`**。两个方法对应不同的 HTTP method:
- `Update` (PUT `/open-apis/im/v1/messages/:id`) — 仅文本/富文本。SDK 注释: "当前仅支持编辑文本和富文本消息"
- `Patch` (PATCH `/open-apis/im/v1/messages/:id`) — 卡片/富文本都支持,5 QPS 频控,30 KB body 上限

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

**本期不**新增 `agentName` / `provider` / `model` 字段 —— foot note 全部从已有 state 字段组装(state.String() + eventCount + lastEventAt)。这样不需要触动 `agent.InitEvent` / `gateway/translate.go` / `OutboundMessage.Meta` 任何上游。

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
            "content": fmt.Sprintf("<text_tag color='neutral'>…(前 %d 条已省略)</text_tag>", r.evicted),
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

`UpdateMessage` 调 `larkClient.Im.V1.Message.Patch(...)`(注意是 **Patch**,不是 Update —— Update 只能改文本)。`SendCard` 是 `sendContent(chatID, larkim.MsgTypeInteractive, content)` 的薄包装。

PATCH 失败时不降级为新消息 —— 简单实现,日志告警即可;下次 `renderLocked` 仍然 PATCH 同一个 messageID。

### 5.5 迁移步骤(本期)

1. **Phase 1 — adapter 层支持**: 加 `SendCard` / `UpdateMessage`(内部调 Patch),`buildReceiptCard` + `footLine` 静态实现
2. **Phase 2 — receipt 切换**: `renderLocked` 改为 first-send-then-PATCH;`MessageReceipt` 加 `cardMsgID`;`evictOverflowLocked` 扩展为字节+元素双约束
3. **Phase 3 — 测试更新**: `mockReceiptBot` 加 SendCard / PatchMessage stubs;`TestReceipt_PerEventFreshMessage` 改为断言 PATCH 行为;新增 `TestFootLine_*` / `TestBuildReceiptCard_*` 系列
4. **Phase 4 — 文档收尾**: 在 §11 记录本期落地状态;把"未做"留给 follow-up issue

**OutInit / `agent_name` / `provider` 透传** —— **DEFERRED**(见 §9.4)。当后续 PR 加这三个字段时,`buildReceiptCard` 只需要把它们 append 到 `footLine` 后面,不需要再次动 receipt / adapter 主体。

## 6. 已知坑(从 OpenClaw 学到)

### 6.1 冒号 → 中点(防 hoisting,前瞻要求)

参考 OpenClaw PR #84122。**所有 footer / card note 的 `key: value` 必须改成 `key · value`**,否则 Feishu 渲染器把 `key: value` 解析为 Markdown 定义列表,把第一项的 value hoisting 到卡片正文开头。

**本期 foot note 不会出现 `key: value` 形态**(内容是 `state · N entries · HH:MM:SS`),但**约束记在 §9.3 后续加 agent info 时必须遵守**。中点字符用 U+00B7(`·`),不是句号或星号。

### 6.2 `<text_tag color='neutral'>` 而非 `<font color='grey'>`

Feishu `lark_md` **不支持** `<font color='grey'>`。**严格用 `<text_tag color='neutral'>`**(允许值: `neutral`, `blue`, `turquoise`, `lime`, `orange`, `violet`, `indigo`, `wathet`, `green`, `yellow`, `red`, `purple`, `carmine`)。OpenClaw `send.ts:768` 写的 `<font color='grey'>` 靠 Feishu 容错 —— 本项目不依赖容错。

### 6.3 SDK: Patch ≠ Update

`lark-oapi-go/v3` 提供的 `Message.Update`(PUT) **只支持文本/富文本**,**不能改卡片**。**改卡片必须用 `Message.Patch`**(PATCH)。两个方法签名相似,容易踩坑。详见 §3.4。

### 6.4 PATCH 是整体替换

`PATCH /im/v1/messages/{id}` 的 `card` 字段是**整个 card 对象**,不是 diff。要保留元素(折叠面板的展开状态等)需要在 client 端维护当前状态,然后每次 PATCH 把所有 elements 重新构建。本期 receipt 没有折叠面板,不受影响。

### 6.5 心跳(Heartbeat)行为变化

`F-23-heartbeat.md` 当前实现是周期性重新 `SendMessageText` 同一 header。切到 card 后,心跳 = 周期性 PATCH 同一张卡的 card body(刷新 header 时间戳 + foot note)。频率/阈值不变。

### 6.6 MessageState 与 Card 共存（v1.3 重构）

**v1.3 变更**：MessageState（reaction emoji 轨道）与 Receipt（card body 轨道）解耦为两个独立的 channel 实现。

#### 6.6.1 两个轨道

| 轨道 | 源 | 抽象事件 | 渲染目标 | Feishu 实现 |
|---|---|---|---|---|
| **MessageState** | ChatSession lifecycle | `OutboundMessage{Kind: OutMessageState, Meta: {message_id, state}}` | **userMsgID** | `AddReaction(userMsgID, emoji_type)` |
| **Card Body** | Receipt FSM (v1.x 没实现,v1.3 仍是 backlog) | `Channel.UpdateReceipt / DisposeReceipt` | replyMsgID | `SendCard / PatchMessage` |

两者**完全独立**:
- 一个失败不影响另一个 (MessageState 渲染失败仅 log warn,不阻塞 card body)
- 都按 userMsgID / chatID 索引,但服务不同语义
- 详见 [`docs/feat/F-31-message-state.md`](../feat/F-31-message-state.md) 与 `SPEC.md §2.5`

#### 6.6.2 MessageState 渲染实现

```go
// internal/channel/feishu/adapter.go — Send dispatcher 新增 case
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

| `MessageState` | emoji_type（飞书预定义） | 用户视觉 |
|---|---|---|
| `StateReceived` | `OK` | 👌 |
| `StateForwarded` | `OnIt` | 🔄 |
| `StateDone` | `DONE` | ✅ |
| `StateError` | `THUMBSUP` | ❌ (closest 预定义 indicator) |

**重要**：必须用飞书预定义 `emoji_type` 标识符,不是 unicode。传 unicode `⏳` 给飞书 reaction API 返回 `99992354 data not found`(reaction service 只识别预定义集合)。

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

Card 2.0 官方组件列表**没有 `tag: "note"`**。我们走 v2 风格(`<hr>` + 中性色 markdown),**不要**用 v1 的 `note` 元素。

## 7. 验收 / 测试(本期 minimal scope)

- 单元: `buildReceiptCard` 产出合法 JSON,`elements` 末尾是 `<hr>` + `<text_tag color='neutral'>` foot note;`footLine` 为空时整段省略
- 单元: `footLine` 在 `state=executing, eventCount=5, lastEventAt=14:32:05` → `"executing · 5 entries · 14:32:05"`;`eventCount=0` 时不渲染 `0 entries`
- 单元: `MessageReceipt.renderLocked` 第一次调 → `SendCard`;之后 → `PatchMessage` 同一个 messageID;**不再**调 `SendMessageText`
- 单元: 元素数 = 60 entries → 47 entries + `…(前 N 条已省略)` 标记
- 单元: 字节数 = 收到超大 entries → 驱逐最老直到 < 24 KiB
- 单元: 回归 `mockReceiptBot.AddReaction` 不变（v1.3 后,reaction 由 MessageState FSM 触发,仍走 userMsgID,但已从 MessageReceipt 解耦到 Adapter 顶层）
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
- [`docs/feat/F-31-message-state.md`](../feat/F-31-message-state.md) — v1.3 MessageState 抽象事件,本文件 §6.6 是其 feishu-specific 实现补充
- [`docs/SPEC.md`](../SPEC.md) §2.5 — MessageState 架构概述

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
- `replyMaxBytes = 24 KB` —— 留 6 KB 头空间
- entries 上限 = 47 —— 留 3 元素给 header/hr/footer
- 5 QPS 频控靠 receipt 单线程 `renderLocked`(已串行)+ 实际 agent event 频率远低于 5/s,不主动限流

**超出限制时的降级**: `PatchMessage` 失败 → 记录日志 + 下次 render 仍 PATCH 同一 messageID;不重发新消息以避免重复 receipt。**已知风险**: 持续失败 → 卡片一直不更新,直到 receipt 销毁。后续可加重试/降级到"再发新卡"。
