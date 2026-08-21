# Feishu Interactive Cards — Design & Pitfalls

> **Status**: implementation playbook (2026-08-17)
> **Scope**: nightme 飞书 **交互卡**（按钮 / form / `card.action.trigger`），不是 receipt rolling-log。
> **目的**: 下次改 Action Needed / approval / 决策卡时，先读本文，不要再对照官方文档从零试错。
> **Code**: `internal/channel/feishu/adapter.go`（`buildInteractiveCard` / `handleCardAction`）
> **Tests**: `internal/channel/feishu/adapter_opt_test.go`
>
> **Related**:
> - [feishu.md](./feishu.md) — Card 2.0 信封、PATCH vs Update、footer 颜色
> - [feishu-rendering.md](./feishu-rendering.md) A11 — F-46 决策卡 (`act:`) 落地过程
> - [feishu-reliability.md](./feishu-reliability.md) — PATCH 5 QPS / 回调 3s
> - [dsh-api.md](../bridge/dsh-api.md) §3.4.9 — host `AskUserQuestion` / `matchesQuestions`
>
> **官方**:
> - [卡片回传交互（回调结构）](https://open.feishu.cn/document/feishu-cards/card-callback-communication)
> - [处理卡片回调](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/handle-card-callbacks)
> - [Card JSON 2.0](https://open.feishu.cn/document/uAjLw4CM/ukzMukzMukzM/feishu-cards/card-json-v2-structure)
> - [PATCH 已发卡片](https://open.feishu.cn/document/server-docs/im-v1/message-card/patch)

---

## 1. 先分清四类卡

| 家族 | 标题 / 视觉 | 交互 | 点完去哪 |
|------|-------------|------|----------|
| **Receipt** | 无 👉；header 心跳 / 终态 | **无按钮** | PATCH 同一张 rolling-log |
| **AskUserQuestion** | `👉 Action Needed`；多题 `· i/n` | 选项 + Type your answer + Skip + Submit | 单题立即 inbound；多题卡内翻页，最后一步 `nm-q:` |
| **Approval** | **Waiting for approval** | Allow once / Reject **only** | `ApprovalResponse`；**不要**加 Type your answer |
| **Decision (gtw)** | 无 👉；`ChoiceKindDecision` | `act:/gtw/...` 等宽 `column_set` | 合成 reaction → `gtw.HandleAction` → `OutChoicePatch` |

AskUserQuestion 与 Approval 走同一条 `opt:` inbound，但 **卡面和 host 协议不同**。dashboard Allow once 解决 approval 后，bridge 继续收 mux，并 PATCH 掉飞书按钮；不要把它做成 Action Needed 向导。

Receipt / footer / thread 的坑在 [feishu.md](./feishu.md) §6 / §13.23，本文不重复。

---

## 2. AskUserQuestion 设计

Dashboard 的 1/N 是 **host UI**：只有整批 `POST /api/respond` 之后才会翻页（host `matchesQuestions` 要求 `answers.length == questions.length` 且 `answer.id` 按序对齐）。

飞书自己做 **卡内向导**：`Step` / `Picks` 存在 adapter 的 `optWizards` 里（不在 `messages.Choice` 上）。中间一步 **不** 打 `/api/respond`。所以：

- 飞书从 1/3 翻到 2/3，dashboard 仍停在 1/3 —— 这是对的。
- 飞书停在 1/2 但 toast「已提交」—— 多半是客户端没拿到下一张卡（见 §3），不是 host 没收到。

### 2.1 一击 vs 向导

| `len(Questions)` | 卡面 | 点击 | inbound |
|------------------|------|------|---------|
| 0（approval / 旧 Options） | 选项按钮，无 input | `opt:<label>` | 立即，Option = 标签 |
| 1 | 选项 + Type your answer + Skip + Submit | 选项 → 标签；Submit → `nm-q:` custom；Skip → `nm-q:` 空 selected | 立即 |
| >1 | `👉 Action Needed · i/n`，每步同样 chrome | 中间：记 `Picks[i]`，`Step++`，**不 inbound**；最后一步 `nm-q:` 整批 | 仅最后一步 |

### 2.2 三种答案形状

Host 非 multi 题 **拒绝** `custom` 与 `selected` 同时有值。

| 用户动作 | `QuestionPick` | 卡内 `Picks[i]` |
|----------|----------------|-----------------|
| 点选项 | `{id, selected:[label]}` | 选项原文 |
| Skip this question | `{id, selected:[]}` | `""` |
| Type your answer + Submit | `{id, selected:[], custom}` | `nm-c:` + 原文（encode 前剥前缀） |

主聊天框里打的字 **不是** 当前题的答案。不要把普通 inbound text 当成 `custom`。

### 2.3 选项排版

长中文 label 用 **一行一个** `width: fill`（`buildStackedButtons`）。等宽 `column_set` 会把 Q1+Q2 混排的长句截断。gtw 决策卡仍用等宽 `column_set`（短 emoji+label）。

标题 emoji 是 **👉**，不是 🔐。`ChoiceKindPermission` 和 `ChoiceKindQuestion` 才加；`ChoiceKindDecision` 不加。错误走 `OutError`（红 header），不是 Choice。

---

## 3. Form + 回调（交互卡最容易翻车的一层）

飞书把「卡片上的 form」和「IM PATCH」分成两条通道。AskUserQuestion 的选项 / 输入 / Skip / Submit **必须**活在 **同一张 body 根上的 `form`** 里。

### 3.1 必做清单

1. **一张 form 包住全部可点控件**。同卡上、form **外面** 的 button **不会** 打 `card.action.trigger`（点了没日志、没 toast）。
2. 每个 form 内控件要有 **ASCII `name`**（字母 / 数字 / 下划线）。中文 label 不能当 `name`。
3. 要回调的 button 设 `form_action_type: "submit"`。普通 `value` button 在 form 里会被吞。
4. 选项 `name` = `opt_0` / `opt_1` / …（下标，不是 label）。Skip = `skip_question`，Submit = `submit_custom`，input = `custom`。
5. 向导每一步换 form 名：`question_form_<Step>`。Submit 之后飞书会把 **同名 form** 锁在已提交态，2/N 若仍叫 `question_form` 会点不动。
6. **`card.action.trigger` 的 HTTP 响应里必须带回下一张卡**：

```json
{
  "toast": { "type": "info", "content": "✅ 已提交" },
  "card": {
    "type": "raw",
    "data": { "schema": "2.0", "header": {}, "body": {}, "config": {} }
  }
}
```

SDK：`larkcallback.CardActionTriggerResponse{Toast, Card: &larkcallback.Card{Type: "raw", Data: unmarshaled}}`（`callbackRawCard`）。

7. 回调必须在 **3 秒** 内 200。超时 = 200341，用户只看到转圈 / 已提交。
8. `input` **不要** 设 `icon`（200621 `unknown property, property: icon`）。密码框用 `show_icon`，普通 Type your answer 不要 icon。

### 3.2 PATCH 更新不了点按者的 form

| 通道 | 作用 | 对点按者 form 卡 |
|------|------|------------------|
| `PATCH /im/v1/messages/{id}` | 消息存储、其他端、回看 | **不够**。form 提交后客户端等的是回调 body |
| 回调 `card.type=raw` | 点按者立刻换成下一张卡 | **必须** |
| 只回 toast | 「已提交」 | 卡停在 1/N，对话不继续 |

中间步骤仍然 PATCH（给消息库 / 别的客户端），但 **同时** 把同一份 JSON 放进回调。最后一步 inbound `nm-q:`，回调带回去掉按钮的终态卡。

`update_multi` 是 config 里的共享卡开关，**不是** 点按者刷新手段。nightme 单聊不显式打开；Card 2.0 文档写默认 true。点按者刷新靠回调 `card`，不要靠这个 flag 碰运气。

### 3.3 回调里实际能读到什么

form submit **经常省略** `value.action`，只带 `Action.Name`。

`resolveCardAction` 顺序：

1. `value.action` 若有，直接用（`opt:<label>` / `skip:` / `custom:` / `act:...`）
2. 否则 `Name`：`submit_custom` → `custom:`，`skip_question` → `skip:`，`opt_N` → 当前题第 N 个 label
3. 中文 label 从 `rememberOptCard` 记住的那张卡的 `cardOptionLabels` 取，**不要** 放进 `name`

自定义文本：`Action.FormValue["custom"]`（string 或 `{value: string}`），不要只读 `InputValue`。

未知 `Name`（例如漏了 `opt_N` 映射）会落到 toast **`Recorded: opt_0`** —— 这是「回调到了、路由没认出来」，不是飞书没点上。

---

## 4. Card JSON 2.0 硬规则

这些在 receipt 和交互卡上都会炸，改卡 JSON 前先扫一眼。

| 规则 | 做错时 |
|------|--------|
| `schema: "2.0"` 必须显式声明 | 被当成 1.0，`body.elements` 对不上 |
| create / PATCH 的 `content` 就是 **卡片对象本身**，不要再包 `{"card": ...}` | 卡空白或垃圾 |
| SDK **`Patch` ≠ `Update`**。改卡必须 `PATCH /im/v1/messages/:id` | Update 只能改文本 |
| PATCH 是 **整卡替换**，不能只改一个 element | 局部更新幻想 |
| Card 2.0 `div` **没有** `elements` 子数组，只有 `text` | 230099 `unknown property, property: elements` |
| markdown 颜色用命名色 `<font color='grey'>`，禁止 hex | 230099 `invalid color: #999999` |
| footer `Key: value` 会被当成 markdown 定义列表，hoist 到卡顶 | 用 `Key · value`（中点） |
| `RequestID` 为空则 `buildInteractiveCard` 直接 fail | PATCH 静默失败（若调用方 `_ = Send`） |
| 回调 3s / 响应结构错 | 200341 / 200672 / 200673 |

---

## 5. 踩坑目录（按症状查）

先对症状，再改代码。多数「飞书坏了」其实是回调 / form / 旧卡 JSON。

| # | 用户看到 | 日志 / API | 根因 | 规则 |
|---|---------|------------|------|------|
| 1 | 点选项完全没反应 | 无 `feishu: opt card action` | 按钮在 form **外面** | 选项+input+Skip+Submit 同一 `form` |
| 2 | toast `Recorded: opt_0` | `handleCardAction` 走 unknown 分支 | form 只回了 `name=opt_0`，没映射 | `resolveCardAction` 用 `opt_N` + 记住的 labels |
| 3 | 点了 `latest_cli` 等长选项没进下一题 | 无 callback | 同 #1，或 `name` 非法 | `name=opt_N`，label 只放 `text` |
| 4 | 填了 input，点 Submit，toast「已提交」，卡停在 1/2，对话不继续 | 有 `opt card action custom=true` + `patch_message`，**无** history 新 seq | 回调只有 toast，客户端 form 不重绘；Q2 未答所以不 `/api/respond` | 回调必须带 `card.type=raw`；每步 `question_form_<Step>` |
| 5 | 飞书已 2/2，dashboard 仍 1/N | 正常 | dashboard 等整批 respond | 不要用 dashboard 判断飞书是否翻页 |
| 6 | `ReplyInChat failed` 200621 `property: icon` | create/PATCH 被拒 | `input.icon` 不是 Card 2.0 字段 | 去掉 `icon`；密码才用 `show_icon` |
| 7 | 卡发出去是空白 | PATCH/create 成功但看不见 | `content` 多包了一层 `card` | `content` = schema 2.0 对象 |
| 8 | 改了代码飞书还是旧卡 | 新进程日志对、旧 `om_…` 不对 | 已发出的消息仍是旧 JSON | **rebuild + 重启 daemon**；用 **新一轮** AskUserQuestion 测，不要点旧卡 |
| 9 | 旧卡点选项，行为错乱 / 直接答完 | 服务端 `Step` 已前进，客户端仍显示 1/N | 点的是过期 UI，服务端按当前 Step 记账 | 重启后不要继续点旧向导 |
| 10 | 自定义+选项一起 POST，host 拒 | respond 4xx / mux 不匹配 | 非 multi 不能 `custom`+`selected` | Submit = 空 selected + custom；点选项 = 无 custom |
| 11 | 主聊天打了一段话，题还在 | 当新 user prompt | 有意为之 | 只有卡上 Skip/Submit/选项才答 |
| 12 | PATCH 了但按钮还在 | `buildInteractiveCard` 失败被吞 | `RequestID` 空 | 出卡和 PATCH 用同一 RequestID；看 `feishu: outgoing patch_message` err |
| 14 | debug log 文件里没有 | 打了 `slog.Default()` | Default 未 `SetDefault` | 用 adapter `a.logger` 或进程已 SetDefault |
| 15 | 点 Allow once 却出了 Type your answer | 审批卡走了 question chrome | Kind/标题分错 | approval = Waiting for approval，无 form |

---

## 6. 代码地图

```
OutboundMessage{Kind: OutChoice, Choice}
        │
        ▼
buildInteractiveCard          schema 2.0 + header 👉（Permission/Question）
  ├ optWizards                Channel 私有 Step/Picks（不在 messages.Choice 上）
  ├ questionStepOpen          → body 根 form question_form_<Step>
  │    ├ buildStackedButtons  opt_N + form_action_type=submit
  │    └ questionCustomFields input name=custom / skip_question / submit_custom
  └ rememberOptCard(open_message_id)   供 opt_N → label

用户点击
        │
        ▼
OnP2CardActionTrigger → handleCardAction
        │
        ▼
resolveCardAction     value.action | Name(opt_N/skip/submit)
        │
        ├ act:  → handleActCardAction → inbound Reaction
        ├ opt:/skip:/custom: → handleOptCardAction
        │         applyOptClick / applyWizardClick
        │         PATCH + 返回 callbackRawCard
        │         最后一步 inbound Option=nm-q:...
        └ else → toast "Recorded: <name>"
```

| 符号 | 文件 |
|------|------|
| `messages.Choice` / `ChoiceQuestion` / `ChoiceOption` | `internal/messages/session.go` |
| `nm-q:` / `QuestionPick` | `internal/messages/question_picks.go` |
| 卡 JSON / form / 回调 | `internal/channel/feishu/adapter.go` |
| host 批答 / custom | `internal/bridge/dsh/permissions.go` |
| inbound 不把点击当新 prompt | `internal/gateway/inbound/action.go` |

---

## 7. 测试（改交互卡必跑）

```
go test ./internal/channel/feishu/ -count=1 -timeout 60s
```

`adapter_opt_test.go` 锁住的不变量：

- 选项+input 在同一 form；`opt_0` 有 unique name；input **无** `icon`
- 向导第一步 click：**不** inbound；回调 `card.type=raw` 含 `· 2/2` 且 form 名变成 `question_form_1`
- Submit 自定义同左（这就是「已提交但不翻页」回归）
- 最后一步 inbound `nm-q:` 批答；终态回调卡无 form
- `opt_0` 仅有 `Name`、没有 `value.action` 时仍能路由到 label
- approval 卡没有 Type your answer / Skip

改回调结构或 form 名而不改这些测试，等于没锁。

---

## 8. 实机注意

1. 改 `adapter.go` 之后必须 **rebuild 并重启** `bin/nightme _daemon --channel feishu`。旧进程继续喂旧 JSON。
2. 飞书客户端上已经发出的卡 **不会** 热换成新 schema。验证用新的 AskUserQuestion，不要点截图里那张 1/2。
3. 看 `~/.nightme/nightme.log`：`feishu: opt card action` 证明回调进了 nightme；紧接着应有 `patch_message`。若只有 toast、没有下一张卡，查回调 `resp.Card`。
4. `session.list` 卡住（dsh host CPU）不影响 `session.history`；不要用 list 失败判断飞书卡死。

---

## 9. 变更记录

- **2026-08-17** — 从 AskUserQuestion 飞书向导一轮实机踩坑抽出独立 playbook：form 必须包住选项、`opt_N` 路由、`input.icon` 200621、回调必须返回 `card.type=raw`（PATCH 不够）、每步独立 `question_form_<Step>`。
