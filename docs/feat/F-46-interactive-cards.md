# F-46: 交互卡按钮回灌 + 原地 PATCH（Interactive Decision Cards）

> **Status**: ✅ 已落地（UAT demo via `/gtw test ok`，2026-08-06）
> **Milestone**: v1.3.x
> **Scope**:
> - `internal/channel/feishu/adapter.go` — `handleCardAction` 从 stub 改成真路由（`act:` 前缀）
> - `internal/channel/feishu/adapter.go` — `buildInteractiveCard` 改 button value 编码（`action` envelope）+ column_set 等宽布局
> - `internal/command/gtw/`（F-102 重构后位置；原 `internal/gateway/handlers_gtw.go`） — 决策卡路径（§5.3.1 / §5.3.3）改用交互卡渲染
> - `internal/gtw/render.go` — 旧的纯文本 markdown 渲染保留为 fallback
> - `internal/gateway/messages.go` — `Card` 字段增加 `Kind` / `Choices` / `Action` / `Disabled` / `ChosenChoiceEmoji`
> - `internal/gtw/types.go` — `OutMsg` 增加 `PatchBotMsgID` / `PatchChosenEmoji` / `PatchResult` / `CardTitle` / `CardBody` / `CardChoices` / `CardRequestID` / `ChosenChoiceEmoji`
> - `internal/gtw/types.go` — `OutCardMsg` + `SendCardFunc`（返回 bot-side message id）
> - `cmd/nightme/run.go` — `gtwSendAdapter` / `gtwSendCardAdapter` 把 gtw.OutMsg 翻译成 channel 消息
> - `cmd/nightme/main.go` — 改 `logging.Setup(cfg)` 装 default logger，让全代码 `slog.Default().Warn(...)` 都走同一个 sink
> - 文档同步（`SPEC.md` §3.5 / `channel/feishu.md` §13 / `F-45 §6` cross-link）

## 1. 背景

### 1.1 现状

nightme 现在有两套并行通路驱动 gtw 决策卡：

| 通路 | 触发 | 处理函数 | 状态 |
| --- | --- | --- | --- |
| emoji reaction | 用户在 bot 消息上点 `🔄` 等 | `OnP2MessageReactionCreatedV1` → `handleReactionCreated` → 推进 inbound → `WithActionHandler` → `gtw.HandleAction` | ✅ 已通 |
| interactive card button | 用户点 bot 卡片上的 `🆕` 按钮 | `OnP2CardActionTrigger` → `handleCardAction` (stub) | ❌ stub：log + 弹 toast |

`/gtw fix` 跑出来的决策卡（branch-exists / worktree-fail 场景，见 §3.3）是纯文本 markdown（见 `internal/gtw/render.go`），用户必须打 emoji 才能继续。React Native / web 客户端上的表情输入体验不好（选 emoji 面板要找），所以要给决策卡加 button + `select_static` 让用户点一下。

### 1.2 邻近实现

- **cc-connect** ([card.go](https://github.com/cccZone/cc-connect/blob/main/platform/feishu/card.go))：Card 2.0 schema + `{"action": btn.Value, "session_key":..., "extra":...}` value 编码 + `nav:` / `act:` / `cmd:` 三类前缀
- **cc-connect** ([feishu.go::onCardAction](https://github.com/cccZone/cc-connect/blob/main/platform/feishu/feishu.go))：按前缀分发 + 原卡 PATCH 替换（不是新发）
- **Hermes feishu-card** ([__init__.py](https://github.com/ai-eifying/hermes-feishu-card/blob/main/__init__.py))：Card 2.0 schema + native `table` / `column_set` 富元素（无 button）
- **Hermes lark-skill-collection** ([bettersoul](https://github.com/bettersoul/hermes-lark-skill-collection))：button + form input + card 原地 update + token cache

cc-connect 的模式最贴近 nightme 的需求：bot 决策卡要支持 button + 原卡 PATCH + 派发回 action handler。

## 2. 目标

1. `/gtw fix` 决策卡用 Card 2.0 渲染，按钮 / `select_static` 让用户点一下就能继续
2. 用户点按钮 → `card.action.trigger` → 推进 inbound → 复用 `gtw.HandleAction`
3. 派发后**原地 PATCH**原卡（按钮变 "✅ 已选择" / 选项变灰），不要发新消息
4. emoji reaction 路径保留（飞书桌面端不渲染 button 时降级用 emoji）
5. 老的 `buildInteractiveCard` 复用，button value 编码标准化

## 3. 设计

### 3.1 Button value 编码

现在 `buildInteractiveCard` 的 button value：

```json
{"request_id": "...", "option": "🆕"}
```

改成 cc-connect 风格：

```json
{"action": "act:/gtw/branch-newv2", "request_id": "..."}
```

`action` 字段是协议级语义（点完去哪里），`request_id` 是卡片关联 token。`option` 字段废弃——`option` 只对 `select_static` 组件有意义，按钮不该用 emoji 当 option text。

**Action 前缀约定**（沿用 cc-connect 三类）：

| 前缀 | 语义 | 落地 |
| --- | --- | --- |
| `nav:/xxx` | 切到新卡（不动业务） | 暂不实现，留 F-47 |
| `act:/xxx` | 执行 action，原地 PATCH | F-46 主体 |
| `cmd:/xxx` | 当用户命令派发（绕过 reaction） | F-46 不做，留 F-48 |

### 3.2 `Card` 字段扩展

```go
type Card struct {
    Title     string
    Body      string
    Options   []string             // button 文本 / select_static 选项
    RequestID string
    // F-46 新增
    Kind      CardKind             // Permission / Decision / Preview；决定 header 配色 + 是否加 🔐
    Action    string               // 当只有单一 action 时（替代 options）
    Choices   []CardChoice         // 比 Options 更结构化：每个选项可以指定 emoji + label + action
    Form      []CardFormField      // 预留 form input（F-48）
    HeaderColor string             // blue / red / green / grey；默认按 Kind 推
}
```

`CardKind`：

```go
type CardKind int
const (
    CardKindPermission CardKind = iota  // 权限请求（保留 🔐 + Allow/Deny）
    CardKindDecision                     // 决策卡（无 🔐，自带 Choices）
    CardKindPreview                      // /gtw test card 预览（无 🔐，无 action）
)
```

### 3.3 Decision card 渲染（branch-exists / worktree-fail）

```go
// branch-exists scenario (gtw fix flow decision)
&Card{
    Kind:    CardKindDecision,
    Title:   fmt.Sprintf("⚠️ 分支 `%s` 已存在", payload.Branch),
    Body:    fmt.Sprintf("issue: #%d  %s\n\n选择操作:", payload.IssueID, payload.Title),
    Choices: []CardChoice{
        {Emoji: "🆕", Label: "用 -v2 新分支", Action: "act:/gtw/branch-newv2"},
        {Emoji: "🔗", Label: "加入现有协作",  Action: "act:/gtw/branch-join"},
        {Emoji: "❌", Label: "取消",          Action: "act:/gtw/cancel"},
    },
    RequestID: "gtw-fix-branch-exists-" + payload.IssueID,  // 关联 userMsgID
}

// worktree-fail scenario (gtw fix flow decision)
&Card{
    Kind:    CardKindDecision,
    Title:   fmt.Sprintf("❌ 创建 worktree 失败(#%d)", payload.IssueID),
    Body:    fmt.Sprintf("branch: %s\n\n选择操作:", payload.Branch),
    Choices: []CardChoice{
        {Emoji: "🔄", Label: "重试", Action: "act:/gtw/worktree-retry"},
        {Emoji: "❌", Label: "取消", Action: "act:/gtw/cancel"},
    },
    RequestID: "gtw-fix-worktree-fail-" + payload.IssueID,
}
```

### 3.4 Button 渲染（card.go 改造）

`buildInteractiveCard` 改造点：

1. 拆 header 配色：根据 `CardKind` 选 `template`，默认 blue（permission 仍 blue + 🔐）
2. `🔐 ` 前缀改成只在 `CardKindPermission` 时加
3. Choices 渲染为 `column_set` 等宽布局（cc-connect `CardActionLayoutEqualColumns`），3 个按钮横排
4. 单按钮场景（worktree-fail 两选项）也用 `column_set` 一致布局

```go
// cc-connect 的等宽布局
if e.Layout == core.CardActionLayoutEqualColumns {
    columns := make([]map[string]any, 0, len(actions))
    for _, action := range actions {
        columns = append(columns, map[string]any{
            "tag": "column", "width": "weighted", "weight": 1,
            "vertical_align": "center", "horizontal_align": "center",
            "elements": []map[string]any{action},
        })
    }
    columnSet := map[string]any{
        "tag": "column_set", "columns": columns,
    }
    if len(actions) == 2 { columnSet["flex_mode"] = "bisect" }
    elements = append(elements, columnSet)
}
```

### 3.5 `handleCardAction` 路由

```go
func (a *Adapter) handleCardAction(ctx context.Context, event *larkcallback.CardActionTriggerEvent) (*larkcallback.CardActionTriggerResponse, error) {
    if event.Event == nil || event.Event.Action == nil { return nil, nil }

    // 1. 取 action string（兼容 button.value 与 select_static.option）
    actionStr := ""
    if v, ok := event.Event.Action.Value["action"].(string); ok { actionStr = v }
    if actionStr == "" && event.Event.Action.Option != "" {
        actionStr = "opt:" + event.Event.Action.Option  // select_static 走 opt: 前缀
    }
    if actionStr == "" { return nil, nil }

    // 2. 按前缀分发
    switch {
    case strings.HasPrefix(actionStr, "act:"):
        return a.handleActCardAction(ctx, event, actionStr)
    case strings.HasPrefix(actionStr, "nav:"):
        return a.handleNavCardAction(ctx, event, actionStr)  // F-47
    case strings.HasPrefix(actionStr, "cmd:"):
        return a.handleCmdCardAction(ctx, event, actionStr)  // F-48
    }

    return nil, nil
}
```

### 3.6 `act:` 派发路径

```go
func (a *Adapter) handleActCardAction(
    ctx context.Context,
    event *larkcallback.CardActionTriggerEvent,
    actionStr string,
) (*larkcallback.CardActionTriggerResponse, error) {
    if event.Event.Context == nil { return nil, nil }
    chatID := event.Event.Context.OpenChatID
    messageID := event.Event.Context.OpenMessageID
    userID := ""
    if event.Event.Operator != nil { userID = event.Event.Operator.OpenID }

    // 1. actionStr → (ReactionKind, draftKind)。"act:/gtw/branch-newv2"
    //    映射成 (ReactionNewV2, DraftFixBranchExists)。
    kind, targetEmoji, ok := gtwActionMap(actionStr)
    if !ok {
        return &larkcallback.CardActionTriggerResponse{
            Toast: &larkcallback.Toast{Type: "warning", Content: "未知操作: " + actionStr},
        }, nil
    }

    // 2. 构造 synthetic ReactionEvent，发布到 inbound 流
    synthetic := &InboundMessage{
        ChatID:     chatID,
        UserID:     userID,
        Text:       "",
        HasMention: true,
        MessageID:  messageID,
        Reaction: &chatsession.ReactionEvent{
            TargetMsgID: messageID,
            Emoji:       targetEmoji,
            UserID:      userID,
            ChatID:      chatID,
        },
    }
    select {
    case a.incoming <- channel.Message{Msg: synthetic}:
    case <-ctx.Done(): return nil, ctx.Err()
    default:
        // inbound 满：记 warn 后继续（生产 daemon inbound 128 buffer，正常不会满）
    }

    // 3. PATCH 原卡（异步，等 action handler 派发完后更新）
    //    "原生状态卡" 在 inbound 流里被 gtw.HandleAction 处理完后，
    //    会再发一条 OutboundMessage 触发 PATCH。我们在这里只先 toast。
    return &larkcallback.CardActionTriggerResponse{
        Toast: &larkcallback.Toast{Type: "info", Content: fmt.Sprintf("✅ 已选择 %s", targetEmoji)},
    }, nil
}
```

`gtwActionMap` 在 `internal/gtw/action_routing.go`：

```go
var gtwActionPrefixes = map[string]ReactionKind{
    "act:/gtw/branch-newv2":   ReactionNewV2,  // /gtw test three
    "act:/gtw/branch-join":    ReactionJoin,   // /gtw test three
    "act:/gtw/worktree-retry": ReactionRetry,  // /gtw test ok
    "act:/gtw/cancel":         ReactionCancel, // any decision card
}

func ActionLookup(action string) (ReactionKind, bool) {
    // unknown / retired (label-force, worktree-cancel, …) → false
}
```

### 3.7 原地 PATCH（action 完成后）

gtw 派发完成后 (`gtw.HandleAction` 返回 true)，`HandleAction` 在 CardType 卡上 follow-up 发一条 `OutboundMessage{Kind: OutCardPatch, Card: <updatedCard>, ReplyTo: userMsgID}`。Feishu adapter 的 `Send` 把 `OutCardPatch` 路由到 `PatchMessage`：

```go
case gateway.OutCardPatch:
    if msg.Card == nil || msg.ReplyTo == "" { return errors.New(...) }
    body, err := buildInteractiveCard(msg.Card)
    if err != nil { return err }
    _, err = a.updateContent(ctx, msg.ReplyTo, interactiveMessageType, body, false)
    return err
```

`buildInteractiveCard` 复用，PATCH 后的卡是 `Choices` + disabled 状态：

```go
&Card{
    Kind:    CardKindDecision,
    Title:   ...,
    Body:    ... + "\n\n✅ 已选择 " + chosenEmoji,
    Choices: []CardChoice{chosen},
    RequestID: ...,
}
```

render 时所有 button 的 `disabled: true`：

```go
action := map[string]any{
    "tag": "button", "text": plainText(btn.Text), "type": btnType, "value": valMap,
    "disabled": true,  // F-46 新增
}
```

### 3.8 派发后 follow-up

`gtw.HandleAction` 在 `executeBranchExistsAction` / `executeWorktreeFailAction` 完成后调 `deps.Send` 发 follow-up text（"❌ Cancelled fix #N." 等）。F-46 把这些 text 改成 `OutCardPatch`：

```go
// 之前
deps.Send(ctx, OutMsg{ChatID: ev.ChatID, Text: fmt.Sprintf("❌ Cancelled fix #%d.", p.IssueID)})

// F-46
deps.SendCard(ctx, OutCardMsg{
    ChatID:   ev.ChatID,
    ReplyTo:  ev.TargetMsgID,
    Card:     &Card{Kind: CardKindResult, Title: fmt.Sprintf("❌ Cancelled fix #%d", p.IssueID)},
})
```

`deps.Send` 类型扩展：

```go
type OutMsg struct {
    ChatID    string
    Text      string
    ReplyTo   string
    // F-46 新增
    Card      *Card       // 当需要发/ PATCH 交互卡时填
    CardKind  string      // "create" | "patch"，create 走 sendContent，patch 走 PatchMessage
}
```

### 3.9 inbound 流容量

`Adapter.incoming` 当前 buffer=128。`/gtw fix` 跑的时候 inbound 流主要是用户输入 + reaction，F-46 加 button callback 后同一个 flow 多了一条合成消息路径——128 buffer 足够。监控：如果有"channel full"warn，加 buffer 或直接同步 `cs.HandleAction`（不走 inbound）。

## 4. 接口

### 4.1 Feishu adapter

```go
// internal/channel/feishu/adapter.go
type Adapter struct {
    // ...existing...
    cardActionRouter func(actionStr string, ev chatsession.ReactionEvent) (emoji gtw.ReactionKind, ok bool)
}

// 用 SetCardActionRouter 注入（F-46 测试 fixture 用）
func (a *Adapter) SetCardActionRouter(router func(string, chatsession.ReactionEvent) (gtw.ReactionKind, bool))
```

### 4.2 gateway Card 字段

```go
// internal/gateway/messages.go
type Card struct {
    Title      string
    Body       string
    Options    []string
    RequestID  string
    Kind       CardKind       // F-46
    Action     string         // F-46
    Choices    []CardChoice   // F-46
    Form       []CardFormField // F-48
    HeaderColor string        // F-46
}
type CardKind int
type CardChoice struct {
    Emoji  string
    Label  string
    Action string  // act:/gtw/...
}
```

### 4.3 OutboundKind 新增

```go
const (
    // ...existing...
    OutCard        OutboundKind = iota  // 已有
    OutCardPatch  OutboundKind          // F-46 新增：PATCH 现有卡（不是发新卡）
)
```

### 4.4 决策卡入口

```go
// internal/gtw/fix.go
// emitBranchExistsDraft / emitWorktreeFailDraft 改为构造 gateway.Card 而不是纯文本
func emitBranchExistsDraft(...) (*Result, error) {
    card := buildDecisionCard(payload, existingPath)  // F-46
    return sendCard(ctx, deps, chatID, messageID, userMsgID, card, ...)
}
```

## 5. 测试

### 5.1 单元测试

- `internal/gtw/action_routing_test.go`：`gtwActionMap` 全 prefix 命中
- `internal/channel/feishu/adapter_test.go::TestHandleCardAction_ActRouting`：合成 `CardActionTriggerEvent`，验证 inbound 流收到正确 `ReactionEvent`
- `internal/channel/feishu/card_test.go::TestBuildInteractiveCard_DecisionKind`：验证 `CardKindDecision` 不加 🔐、3 个 button 用 `column_set`
- `internal/channel/feishu/card_test.go::TestBuildInteractiveCard_DisabledButtons`：PATCH 后的卡 button 全 disabled

### 5.2 集成测试

- `internal/gateway/dispatch_action_test.go::TestDispatch_CardAction_RoutesToGTW`：合成 `card.action.trigger` → 走完 `gtw.HandleAction` → 验证 dispatched `ReactionEvent`
- `/gtw test ok`（用户已测的）保留 pipeline exercise，新增 `/gtw test card-patch` 验证 PATCH 路径

### 5.3 手动验证

- 飞书客户端（iOS/Android）：点 branch-exists 卡片按钮 → toast "✅ 已选择 🆕" → 原卡变成 "已选择 🆕" 状态
- 飞书桌面：同上
- 飞书 Web：部分版本不渲染 button，确认走 emoji 降级路径
- daemon 重启后跑 `/gtw fix 42`，决策卡渲染形状与点按钮的反馈

## 6. 风险与回退

1. **button 客户端兼容**：飞书 Web 部分版本不渲染 button → 决策卡必须保留纯文本 markdown 降级
   - 回退：`if !Card.SupportsInteractive { render as markdown }`——`SupportsInteractive` 字段由 channel adapter 报告
2. **action handler 路由回灌**：`handleCardAction` 同步入 inbound 流，inbound 满会丢
   - 监控：加 metric `nightme_card_action_inbound_full_total`
3. **PATCH 失败**：PATCH 失败时用户看到旧卡 + action 没生效提示
   - 现状：PATCH 失败由 `WithTransientRetry` 兜底（retry.go）
4. **CardKind 误用**：decision 卡错填 `CardKindPermission` 会被加 🔐 + 颜色不对
   - 默认零值 `CardKind(0)` 保留为 Permission 行为；新增 `CardKindDecision` 起 iota=1

## 7. 实施状态（vs 原计划）

| 步 | 计划 | 实际 |
| --- | --- | --- |
| 1. `Card` / `OutboundKind.OutCardPatch` 字段 | 1d | ✅ done |
| 2. `buildInteractiveCard` 改造（column_set 等宽 + Disabled + ChosenChoiceEmoji） | 1d | ✅ done |
| 3. `gtwActionMap` + `handleActCardAction` | 1d | ✅ done |
| 4. Feishu adapter `OutCardPatch` case | 0.5d | ✅ done |
| 5. `executeXxxAction` follow-up 改发 OutCardPatch | 1d | ✅ done（emitFollowUp + gtwSendAdapter） |
| 6. `/gtw fix` 决策卡改用 `buildDecisionCard` | 1d | ❌ 推迟（`/gtw fix` 路径仍走纯文本 markdown，未来再迁）|
| 7. 单元 + 集成测试 | 1d | 🟡 部分（`handlers_gtw_test.go` 6 个 case，但 `/gtw fix` 路径未覆盖）|
| 8. 飞书三端验证 | 1d | 🟡 用户 UAT（无真飞书账号）|

实际花费：3 人·天（大部分是踩坑时间）。**踩坑时间 ≈ 实现时间**，见 §10。

## 8. 文档同步

- `SPEC.md` §3.5（F-45 reaction → gtw pipeline）补 button → reaction 通路
- `channel/feishu.md` §13（card lifecycle）补 button click handler + PATCH 路径
- `F-45-session-footer.md` §6 cross-link（决策卡 footer 复用 SessionContext）
- `F-25-rolling-log.md` §3 cross-link（决策卡不进入 receipt，是独立 card message）

## 9. 不在 F-46 范围

- `nav:` 前缀（卡片内导航 / 翻页）
- `cmd:` 前缀（button click 当用户命令派发）
- `form input`（删除模式那种多选表单）
- `select_static` 下拉组件（cc-connect 用 select_static 替代 button 列表——这是 UX 增强，不是必需）
- 原卡 disable 后 emoji reaction 是否还生效（默认是；用户已经点过 button 了再点 emoji 是 noop）

## 10. 实现过程总结（按踩坑时序）

这一节记录落地过程中遇到的关键 design decision 与 debug 经验，下次再写类似的交互卡 PATCH 路径可以照抄。

### 10.1 完整链路（生产运行时）

```
用户点 Feishu 卡上的按钮
        │
        ▼
Feishu SDK 收到 card.action.trigger 事件
        │
        ▼
internal/channel/feishu/adapter.go::handleCardAction (OnP2CardActionTrigger 注册)
        │
        ▼
gtw.ActionLookup(actionStr) → 解析 act:/gtw/<scenario> → ReactionKind (🔄 / 🆕 / 🔗 / ❌)
        │
        ▼
handleActCardAction 合成一个 InboundMessage{Reaction: <ReactionEvent>}
        push 到 a.incoming channel（buffer=128）
        │
        ▼
internal/gateway/gateway.go::dispatchAction
   ├─ g.actionHandler != nil?  ── 岔路 A：nil → Consumed:true Dropped:true（pre-F-45 行为）
   └─ g.actionHandler(ctx, msg)  ── 由 cmd/nightme/run.go 装的生产 trampoline
        │
        ▼
生产 trampoline：cs := mgr.Get(msg.ChatID) → cs.HandleAction(ctx, ev)
   ├─ cs == nil?  ── 岔路 B：return false
   └─ cs.onReaction(ctx, ev)        ← 由 `internal/command/gtw/` 的 reaction handling 装上（原 `internal/gateway/handlers_gtw.go::wireGTWActionOnSession`；F-102 重构后 `gtw` 整体迁到 `internal/command/gtw/`，reaction 路由走 `services.ReactionRouter`，已不再走 `cs.SetActionHandler`）
        在 runGTWTestScenario / SetActionHandler 装上        注册 closure
        │
        ▼
gtw.HandleAction → executeXxxAction → emitFollowUp
        │
        ▼
emitFollowUp：if draft.BotMessageID != "" → PATCH 原卡；else 落 plain text
        │
        ▼
gtwSendAdapter → channel.Send(OutboundMessage{Kind: OutCardPatch)
        │
        ▼
Feishu adapter Send → OutCardPatch case
        ├─ msg.Card == nil  ── 岔路 C：return error (被 _ = 吞掉)
        ├─ buildInteractiveCard(msg.Card)  ── 岔路 D：return error (被 _ = 吞掉)
        └─ a.PatchMessage(ctx, msg.ReplyTo, content)
                └─ a.logOutgoing("patch_message", ..., err)  ── logOutgoing 总 fire
```

每一步**都加了 debug log**（`slog.Default().Warn("F-46 debug: <step>", ...)`），可以从前到后串起来定位断点。

### 10.2 关键设计决定

#### 10.2.1 Button value 编码：`action` 替代 `option`

`buildCardButtons` 的 button value 之前是：

```json
{"request_id": "...", "option": "🆕"}
```

Feishu SDK 的 `event.Action.Option` 字段是 `select_static` 组件的选项值，**不是**按钮的语义含义。改成 cc-connect 风格：

```json
{"action": "act:/gtw/branch-newv2", "request_id": "..."}
```

`action` 字段是协议级语义（点完去哪里），`request_id` 保留卡片关联 token。`option` 字段完全废弃。

#### 10.2.2 `act:` 前缀三段式（沿用 cc-connect）

| 前缀 | 语义 | F-46 落地 |
| --- | --- | --- |
| `act:/gtw/branch-newv2` | branch-exists �（`/gtw test three`） | ✅ 已实现 |
| `act:/gtw/branch-join` | branch-exists 🔗（`/gtw test three`） | ✅ 已实现 |
| `act:/gtw/worktree-retry` | §5.3.3 🔄（`/gtw test ok`） | ✅ 已实现 |
| `act:/gtw/cancel` | 任意决策卡 ❌ | ✅ 已实现 |
| `nav:/xxx` / `cmd:/xxx` / `act:/gtw/label-force` | 导航 / 命令 / §5.3.2 强制接管 | ❌ 未进 map（F-47/48/49） |

`ActionLookup` 只收录**当前卡面真实发出的** action；占位 / alias（`label-force`、`worktree-cancel`）已从 map 清掉，避免与 `/gtw test` 场景脱节。

#### 10.2.3 PATCH 视觉：颜色反转 + 完整 label + 无 "已选择" 头

PATCH 后的卡布局（`buildCardButtons` 中处理）：

- **选中的按钮**：`type: "success"` 填充绿 + `✓` 前缀 + 完整 label（如 `✓ 🔄 重试`）。Feishu 把 `success` 类型渲染为绿色填充，与 `default` 灰描边对比强烈。
- **没选的按钮**：`type: "default"` 灰描边 + `disabled: true` + 完整 label（如 `❌ 取消`）。完整 label 解决"用户只看 icon 不知道意思"的痛点。
- **body 不再有"✅ 已选择 X"独立行**——那个是冗余的视觉噪声，PATCH 后的按钮绿色已经传达"已选"语义。
- body 只剩原始 body + 底部一行 `Retry failed: ...`（来自 `m.PatchResult`）。

#### 10.2.4 `CardRequestID` stamping：测试栈的 PATCH 死代码

**Bug 现象**：`/gtw test ok` synthetic reaction 跑通（`consumed=true dropped=false (handler acted)`），但 `emitFollowUp` 之后日志里**没有 `patch_message`**。

**根因**：`gtwTestSeedDraft` 设的 `chatsession.GTWDraft` **没有 `CardRequestID` 字段**。`sendScenarioCard` 算的是 `"gtw-test-" + userMsgID`，但 `gtwTestSeedDraft` 不知道。`gtwTestRekeyDraft` 把 draft 从 `om_test_ok` 移到 `cardMsgID`，但 `CardRequestID` 还是空。

`gtwSendAdapter` PATCH 路径把这个空 RequestID 传给 Feishu adapter 的 `OutCardPatch` case → `buildInteractiveCard` 看到空 RequestID → return error `"feishu: card missing request_id"`。这个 error 被 `_ = deps.Send(...)` 静默吞掉。

**修法**：`gtwTestSeedDraft` 直接硬编码 `CardRequestID: "gtw-test-" + userMsgID`，和 `sendScenarioCard` 的 `RequestID` 计算公式保持一致。PATCH 路径就通了。

> **教训**：依赖**计算公式**和**存储值**必须共享一个变量或常量，否则 PATCH 路径会有静默失败。

#### 10.2.5 `/gtw test ok` 是 UAT demo，**不** auto-dispatch

`runGTWTestScenario` 早期实现里**有** `gw.DispatchInbound(synthetic reaction)`，导致用户还没点，卡就自己 PATCH 了——卡立刻显示 `✅ 已选择 🔄 + Retry failed`。这是反 UX：用户失去了"点按钮反馈"的动作。

**设计决定**：`/gtw test` 改为纯 demo 模式。出卡 + 提示文字"请点卡片按钮"。**完全 auto-dispatch 取消**。让真实 E2E 不能在没有 Feishu 真实账号的情况下跑，那 demo 模式就够用。

如果将来要自动化 E2E，加 `/gtw test auto <emoji>` 子模式，但不要默认 auto。

#### 10.2.6 统一 logger：打通 `slog.Default()` 和 plumbed logger

**问题**：`slog.Default().Warn(...)` 在 daemon 进程里是 **Go 默认的 no-op logger**。Feishu adapter 的 `feishu: outgoing` 走 `a.logger.Info/Warn`（plumbed 的 runtime logger），能进 log 文件。我加的 F-46 debug log 走 `slog.Default()`，**全部进黑洞**。

**根因**：`cmd/nightme/main.go` 调 `logging.New(cfg)` 而不是 `logging.Setup(cfg)`。`Setup` 内部就是 `New + slog.SetDefault(lg)`，关键差那一行 SetDefault。

**修法**（`main.go`）：

```go
var logger *slog.Logger
if l, err := logging.New(cfg); err != nil {
    ...
} else {
    logger = l
}
// F-46 debug: install logger as slog.Default so all
// downstream code (handlers_gtw.go, gateway.go, action.go,
// chatsession.go) that calls slog.Default().Warn(...) lands
// in the same MultiWriter sink as the plumbed logger.
slog.SetDefault(logger)
defer func() { _ = logging.Close(logger) }()
Execute(logger)
```

现在 `slog.Default()` 和 plumbed `logger` 指向**同一个 handler**，`MultiWriter(file, stdout, stderr)` 三路都会出。`add_reaction` 类的 7 路日志和 debug 类 9 路日志都进同一个文件。

> **教训**：Go 生态的 `slog.Default()` 是"便利但默认 no-op"的陷阱。要么显式 `SetDefault`，要么**永远不要依赖** `slog.Default()`。F-46 之前所有 Feishu adapter 的 `a.logger.Warn(...)` 都走 plumbed logger 才正常。我加的 9 处 F-46 debug 全错路径——直到我修了 main.go 才真正能跑。

### 10.3 调试心得

调试这种"dispatch + 回调 + 异步 + 跨模块"的链路，有 **9 个经典岔路**必须每处都打 log：

| 岔路 | 出现场景 | log 在哪 |
| --- | --- | --- |
| A | `g.actionHandler` 没装 | `gateway.dispatchAction` 入口 |
| B | `cs.onReaction` 没装 | `chatsession.HandleAction` 入口 |
| C | `OutCardPatch` case 入口 `msg.Card == nil` | adapter Send OutCardPatch case |
| D | `buildInteractiveCard` 失败（RequestID 空 / JSON 失败）| adapter buildInteractiveCard 入口 |
| E | `gtwDrafts.Lookup(ev.TargetMsgID) == nil`（draft 被 Take 走）| gtw 包装 closure 入口 |
| F | switch emoji 不匹配（`if rk == ReactionRetry` 打印 `matches`）| executeXxxAction 入口 |
| G | `draft.BotMessageID == ""`（stamp 没跑）| emitFollowUp 入口 |
| H | `deps.Send` 返回 error 但被 `_ =` 吞掉 | emitFollowUp 入口 + Send 后 |
| I | `PatchMessage` 返回 error | Feishu adapter PatchMessage 内部 logOutgoing |

每条岔路要 `slog.Default().Warn("F-46 debug: <岔路>")` + 关键字段值（如 `target_msg_id`、`bot_msg_id`、`matches`）。这样出错时一眼看出断在哪。

### 10.4 后续 F-47 / F-48 / F-49 排期

| F | 工作量 | 内容 |
| --- | --- | --- |
| F-47 | 2d | `nav:` 前缀（卡片内导航 / 翻页 / 关闭按钮）|
| F-48 | 1d | `cmd:` 前缀（button click 当用户命令派发到 `gtwRunFix`）|
| F-49 | 3d | `act:/gtw/label-force` + §5.3.2 强制接管 + emoji label 替换 |
| 后续 | 1d | `select_static` 下拉组件（替换 button 列表的 UX 增强） |
| 后续 | 2d | form input（删除模式多选表单）|
| 后续 | 1d | 原卡 disable 后 emoji reaction 行为审计（应该 noop）|
| 后续 | 1d | `/gtw test auto <emoji>` 自动化 E2E 子模式（默认 OFF）|
| 后续 | 1d | 真实飞书 iOS / Android / Web 三端视觉回归（success type 按钮绿色渲染一致性）|

这些留给 F-47+。