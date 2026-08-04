# nightme — Technical Specification (SPEC)

> **状态**：v1.3 SPEC **已落地 docs**（2026-08-03；代码改动 backlog §11）。v1.2 架构不变式全部保留，v1.3 是职责再切分——Gateway 端 Receipt FSM 移除，Channel 自治渲染
> **作者**：🦞 虾哥（PM/Architect）
> **日期**：2026-08-03（v1.3）；2026-08-02（v1.2）
> **文档层级**：技术级（**不含实现细节 / 代码**）
> **关联文档**：
> - 产品定位 → [`PRD.md`](./PRD.md) v1.2
> - 功能索引 → [`FEATURES.md`](./FEATURES.md)
> - 每个 feature 的详细实现（含代码）→ [`feat/`](./feat/)
> - 职责隔离架构 v1.1 → [`feat/F-26-gateway-hub.md`](./feat/F-26-gateway-hub.md)

---

## 0. 文档变更摘要（v1.1 → v1.2）

v1.2 在 v1.1 锁定的职责隔离架构上做**结构重组**——不变的是"职责隔离"，变的是"会话分层"：

1. **v1.1 三层 → v1.2 两层合并**
   - v1.1：`Channel Adapter` / `Gateway` / `Session Manager` + 三种状态机（Binding / Receipt / InputBuffer）
   - v1.2：`Channel Adapter` / `Gateway` + **`ChatSession`**（合并 v1.1 ChannelSession + GatewaySession 的逻辑中枢）+ **`AgentSession`**（取代 v1.1 Session，1:1 绑定 `(agent, cwd)`）

2. **Chat ↔ Session 1:1 不变式 → Chat ↔ ChatSession 1:1 不变式**
   - 不变：1 个 IM chat 永久绑定 1 个会话上下文
   - 变：会话上下文内部可有多个 AgentSession（按 `(agent, cwd)` 1:1 唯一）

3. **`/run` 删除，`/use <agent>` 替代**
   - `/use <agent>` 切换 activeAgent；复用或新建 `(activeAgent, activeCwd)` AgentSession
   - `/cwd` 只改 activeCwd，不触发 spawn；切回能复用之前留下的 AgentSession

4. **InputBuffer FSM 位置迁移：Session → ChatSession**
   - v1.1：挂在 Session 上（per chat）
   - v1.2：挂在 ChatSession 上，跨 `/use` 切换共享 queue（已 queued 的消息发到新 active）

5. **Receipt FSM 不变**（per chatId，跨 `/use` / `/cwd` 不变）

**为什么不叫 v2.0**：v1.1 的核心架构不变式（职责隔离、Binding FSM owner、Receipt FSM owner、单消费者事件流）**全部保留**。改的是"会话的内部结构"，对外产品语义（Chat = Project 边界）保持不变。v0.4 release tag 继续。

---

## 0.1 文档变更摘要（v1.2 → v1.3）

v1.3 在 v1.2 架构上做**职责再切分**——核心变化是**删除 Gateway 端的 Receipt 抽象**，让"抽象归抽象、具体归具体"：

1. **Receipt FSM 从 Gateway 端移除**
   - Gateway 不再持有 `receipts[userMsgID]` map
   - `Channel.CreateReceipt / UpdateReceipt / DisposeReceipt` 接口方法从 `Channel` 接口移除
   - `internal/receipt/` 整个包删除(v1.2 仅保留 `MessageState` 一个 enum;v1.3 把它搬到 `internal/agent/` 因为所有 layer 都已依赖 agent)

2. **outbound 路由改为 userMsgID-driven**
   - EventHandler 在每个 `OutboundMessage` 上设 `out.ReplyTo = cs.currentTurnUserMsgID`
   - Channel.Send 拿 `ReplyTo` 当路由 key，自行 material 化（receipt card / thread / DOM 节点）
   - Gateway 只负责"把消息送到对的 Channel"，Channel 决定怎么渲染、怎么存、怎么 PATCH

3. **`currentTurnUserMsgIDs []string` → `currentTurnUserMsgID string`**
   - 一个 turn 一个锚点（single）
   - buffered batch 时锚到这一批的最后一条 userMsgID
   - 1 turn : 1 anchor, n events（无需 fanout 多 receipt）

4. **MessageState 与 Receipt 真正解耦** — v1.2 末尾已加注释；v1.3 把 Receipt 整个删了，MessageState 真正独立运作，不再有"两个 owner 都说自己拥有 receipt"的歧义

**不变式**：
- **抽象归抽象**：Gateway = 路由器，不假设任何 Channel 渲染细节
- **具体归具体**：Channel = 渲染器，自管 receipt 生命周期，自选存储形态
- Channel 接口永远不暴露 receipt / receipt map / receipt FSM / 任何渲染状态

## 0.2 文档变更摘要（v1.3 → v1.3.x F-watch 增量）

**背景**：v1.3 在 F-33 之后，ChatType 从 nightme 数据模型中删除；群聊 “只 @ 才收”的默认行为由飞书 `im:message.group_at_msg:readonly` scope 决定，用户无法 opt-out。 F-watch 让 nightme 侧接管这个决定权。

**增量变化**：

1. **新增 `WatchMode` per-chat 状态**
   - `ChatSession.WatchMode`：`WatchModeMention`（默认，只 @ 收）/ `WatchModeAll`（`/watch on`，全收）
   - `ChatSessionEntry.WatchMode` 持久化字段（Go JSON 容忍缺失）
   - setter `ChatSession.SetWatchMode` + getter `ChatSession.WatchMode()`

2. **新增 `Message.HasMention bool` 字段**
   - Channel adapter 计算：DM 永远 true；group 含 bot/@_all 时 true
   - Gateway dispatcher 在 `Handle` 入口做 gate：`!HasMention && WatchMode != All → drop`
   - 详细职责划分：Channel 不知道 ChatSession；ChatSession 不知道 chat type；双方都只读 `HasMention` 这一个 bit

3. **Channel adapter mention strip**
   - 构造 `Message.Text` 前，strip 开头的 `@bot_key ` 或 `@_all ` mention 前缀 + 末尾空格
   - 还原为 nightme 支持的纯文本格式，让 `/watch on` 能被 `ParseCommand` 正确解析
   - 只 strip 开头；中段 mention 不动

4. **飞书 `DefaultAddons()` 变更**
   - 始终包含 `im:message.group_msg`（不带 `:readonly`）：bot 默认接收全群消息
   - 由 `WatchMode` 在 nightme 侧决定 drop 还是 pass
   - **不**走 CLI flag opt-in —— 默认就是“飞书送全，nightme 决定要不要处理”

5. **新增 `/watch on|off` slash command**
   - Gateway dispatcher 入口与 `/cwd` `/use` `/kill` 同源
   - 三种调用：`/watch on`、`/watch off`、`/watch`（无参 = 显示状态）
   - DM 下为 no-op（DM 永远 `HasMention=true`，不走 gate）

**v1.3 不变式依然保持**：
- Gateway 不持有 chat type
- Channel 不 import `internal/chatsession`
- `WatchMode` 状态只挂在 ChatSession
- Channel adapter 只通过 `Message.HasMention` 与 Gateway 交流 chat type / mention 信息
- Gateway 对 Channel 内部状态一无所知，只通过 `OutboundMessage` 交流

**为什么不叫 v2.0**：v1.2 的核心架构不变式（Binding FSM owner、ChatSession 三层状态、单消费者事件流、`agentSession.Events()` 单读者、InputBuffer FSM owner）**全部保留**。v1.3 是"职责再切分"的延续——把已经过度渗入 Gateway 的 Receipt 概念撤回 Channel 自治域。

---

## 0.3 文档变更摘要（v1.3.x F-thread-route 增量，2026-08-04）

**背景**：v1.3 §13.6 拍板的折叠方案（OutThinking / OutToolStart / OutToolEnd 在 receipt card body 里用 `collapsible_panel` 平铺折叠）在实机上验证失败 —— agent turn 调 10 个工具 = 30 个 panel，Feishu 50 element 上限被频繁撞破；用户首要看到的"最终回答"被挤到 card 末尾甚至消失。

**F-thread-route 反转**：Channel 自治范围内重新决策 —— 这三类 OutboundKind **不进 receipt card**，作为独立 thread reply 投递到 user message 的 Feishu thread。Receipt card 收窄到只承载最终答复（OutText / OutResult）+ 元数据（OutInit / OutUsage）。

**核心变化**：

1. **Channel.Send dispatcher 按 Kind 分流**
   - `OutThinking` / `OutToolStart` / `OutToolEnd` / `OutCompaction` → 直接 POST 到 Feishu thread（rootID = userMsgID），每 event = 一条 thread reply
   - `OutText` / `OutResult` / `OutInit` / `OutUsage` → 继续 fold 进 receipt card（不变）
   - `OutMessageState` → 仍然挂在 user msg 的 reaction 上（不变）
   - `OutCard`（权限请求等）→ 仍然发到 thread，跟 OutToolEnd 一样是 thread reply（不变）

2. **OutToolEnd 类型感知摘要（"决断处理"）**
   - Bridge 层给 `ToolEndEvent.Args` 字段填 args（在同一 message 的 tool_use block 拿）
   - Channel 层 `summarizeToolEnd(name, args, output, err)` 按 tool 类型生成单行摘要（不 dump 原始 output）
   - 默认走字节截断（向后兼容未知 tool）

3. **Receipt card 瘦身**
   - 删 `buildReceiptCard` 的 `Kind="thinking"` / `Kind="tool"` collapsible_panel 分支
   - 删 `eventToEntry` 对 EventText-with-thinking-prefix / EventToolStart / EventToolEnd / EventCompaction 的 entry 生成（这些走 thread）
   - 50 element 上限不再是个问题

4. **F-08 / F-25 文档同步**
   - F-25 §3 Channel Implementation Contract 表更新 —— OutThinking / OutToolStart / OutToolEnd 不再进 receipt
   - F-08 §4 加 "Channel autonomous routing examples" —— Feishu 选 thread 是 Channel 自治的具体例子

**不变式**：
- OutboundMessage 不动（无新 Kind，无删 Kind）
- Gateway 不动（不持有 thread 概念、不感知 channel 分流）
- ChatSession 不动（`currentTurnUserMsgID` 单数锚点不变）
- 1 turn : 1 anchor 不变式保留（所有 event 仍 anchor 到同一个 userMsgID）
- 抽象归抽象 / 具体归具体原则保留（thread 路由是 Feishu 自治决定）
- `OutboundMessage.ReplyTo = currentTurnUserMsgID` 契约不变（thread 路由用它作 Feishu `root_id`）

**为什么不叫 v2.0**：v1.3 的核心不变式（职责隔离、Binding FSM owner、MessageState 独立、Receipt 自治）全部保留。F-thread-route 是 Channel 自治范围内的渲染细节变化，不影响 nightme 数据模型与 Gateway 契约。

**详细落地**：见 [`feat/F-37-tool-thread-routing.md`](./feat/F-37-tool-thread-routing.md) + [`channel/feishu.md`](./channel/feishu.md) §13.12。

### 0.4 文档变更摘要（v1.3.x 抽象/具体边界规范，2026-08-04）

**背景**：F-37 review 揭示 `OutboundMessage.Meta["tool_name"]` / `["args"]` / `["output"]` / `["err"]` 是隐式协议 —— Gateway（抽象层） hardcode 了 Feishu adapter（具体层）需要的字段名，本质上是把 concrete 实现细节 leak 进 abstract 层。同类违反在 `OutboundMessage.Meta` 的其他字段（`is_error` / `subtype` / `state` / `message_id` 等）以小范围存在。

### 0.5 文档变更摘要（v1.3.x Meta 彻底删除，2026-08-04）

**背景**：§1.4 元原则落地后还残留着 Meta 黑盒——`Meta` 字段是 opaque data container，但 producer（gateway） 在里面塞了 11 个 implicit key（tool_name / args / output / err / state / message_id / reaction_id / session_id / model / agent_name / workspace / branch / input_tokens / output_tokens / cost_usd 等），consumer（feishu adapter）按名字读 + type assert。Channel 不知道 Meta 里有什么（type system 也不告诉你），但 producer / consumer 之间靠 hardcoded 字符串约定通信——最严重的 leak。

**根因**：Meta 是 generic map，约定是字符串 key + `.(string) / .(int) / .(error)` 强转，编译期无法检查。F-37 review 把 tool 字段清掉后，Meta 里还有：
- 死数据（OutResult 的 duration_ms / is_error / subtype，channel 实际 round-trip 重建成 `agent.ResultEvent` 再喂给 receipt.Append）
- 冗余数据（OutMessageState 的 message_id / state，已有 `MessageState *MessageStatePayload` typed field 但内容不全）

**清理**（commit 待定）：
- **删 `Meta` 字段**——从 `OutboundMessage` 结构体移除
- **删 `Reaction` 字段 + `Reaction` struct**——F-31 迁移后死代码，零 producer / 零 consumer
- **新增 typed payload**：
  - `Result *agent.ResultEvent`（OutResult）
  - `Usage *UsageInfo`（OutUsage，5 个 token/cost 字段）
  - `Init *agent.InitEvent`（OutInit，session_id / model / workspace / branch）
  - `MessageStatePayload.MessageID` + `ReactionID`（扩展 typed field，OutMessageState / Removed）
- **删 helper 函数**：metaString / metaInt / metaFloat / metaBool / durationMs / isErrorOut / subtypeOut / usageFromMeta（全部读 Meta 的 typed assertion + reverse rebuild）

**结果**：`OutboundMessage` 100% typed：
```go
type OutboundMessage struct {
    ChatID       string
    Kind         OutboundKind
    Text         string
    Card         *Card
    Tool         *ToolInfo
    TaskList     *agent.TaskListEvent
    Result       *agent.ResultEvent
    Usage        *UsageInfo
    MessageState *MessageStatePayload
    Init         *agent.InitEvent
    ReplyTo      string
}
```

任何 producer / consumer 之间的契约现在由 Go 类型系统强制保证。Meta 反面教材**不再是反例**——该字段已不存在。

**新增规范**：§1.4 「抽象 / 具体 边界规范」作为新的不变式类型—— 跨层通信的架构纪律，位阶高于现有 §1.3 的具体不变式。

**核心原则**（一句话）：
> 抽象层只承载泛化统一的概念。底层具体实现的细节不得直接引入抽象层。如果某项具体信息确实需要进入抽象层，必须先在 boundary 处归一化（normalize）为泛化形式后才能跨越边界。这是软件工程中多态的核心思路。

**F-37 review 落地的归一化路径**：
- `OutboundMessage.Meta["args"]` / `["tool_name"]` 等隐式 key → 升级到 `OutboundMessage.Tool *ToolInfo` typed field
- `ToolInfo.Args string` —— bridge（claudecode / pi / pty）把各自的 native representation 归一化成 string，Gateway / Channel 只见到 generic primitive
- Channel 拿到 string 后 parse 出 typed 视图（用于类型感知渲染），parse 逻辑属于 Channel 自治

**保守范围**：只迁移 tool info 这一组字段。其他 Feishu-specific Meta 字段（`is_error` / `subtype` / `duration_ms` / `state` / `message_id` / `reaction_id`）保留原状，留 follow-up PR 清理，避免 F-37 PR 范围失控。

**详细落地**：见 §1.4 + commit `921c862`（typed ToolInfo 升级）。

### 0.6 文档变更摘要（v1.3.x F-38 Task Checklist，2026-08-04）

**背景**：Claude Code 的 `TaskCreate` / `TaskUpdate` 不是独立的 stream-json 顶层事件，而是普通 `tool_use` + `tool_result`。nightme 过去将其当作 OutToolStart / OutToolEnd 投到 Feishu thread，用户只能看到低层工具调用，看不到结构化任务进度。

**核心变化**：

1. **Bridge 在成功结果后归一化任务**：`tool_use` 阶段只缓存 pending operation；匹配的 `tool_result` 确认成功后，bridge 才用 provider 分配的真实 task ID 更新 session-local task map，并发出完整 `TaskListEvent` snapshot。
2. **新增 typed task contract**：`agent.EventTaskCreate / EventTaskUpdate` → `gateway.OutTaskCreate / OutTaskUpdate`；`OutboundMessage.TaskList *agent.TaskListEvent` 承载 generic `ID / Subject / ActiveForm / Status`，禁止 Meta 和 Claude-specific field 泄漏。
3. **Gateway 保持无状态**：只翻译 typed event 并由 runtime stamp `ReplyTo=currentTurnUserMsgID`；不保存任务、不解析 TaskCreate、也不决定 checklist UI。
4. **Feishu receipt 自治渲染**：Channel 保存当前 receipt 的最新 snapshot，在最终答复 entries 后、footer 前渲染一个有界 markdown checklist element；成功的 task tool 不再重复进入 thread。

**不变式**：

- Channel interface、ChatSession、AgentSession、Registry schema 均不变。
- `ReplyTo=currentTurnUserMsgID` 仍是唯一跨层关联信息；旧 receipt 不被跨 turn 回写。
- Bridge 持有的是 provider-session 的规范化任务状态；receipt 只持有展示副本；Gateway 两者都不持有。
- Task result 无法确认成功时不猜测状态，降级为普通 ToolEnd 并记录 warn。
- Feishu checklist 只占一个 card element，最终答复优先，card 总元素继续受 50 上限保护。

**详细落地**：见 [`feat/F-38-task-checklist.md`](./feat/F-38-task-checklist.md) + [`channel/feishu.md`](./channel/feishu.md) §13.14 / §18。

---

## 0.8 文档变更摘要（v1.3.x F-39 增量，2026-08-04）

**背景**：F-37 multi-div 把 OutResult 多 div 拆进 receipt,导致实机出现 `OutResult dedup` 静默丢长答复的 bug。Claude Code stream-json 的 `result.result` 与最后一条 `assistant.event` 内容字节级相等,经 `truncateForLog(text, 600)` 砍后两侧必撞 dedup。长答复(> 600 字)的最终答复整段静默丢失,只在 receipt 里看到 N 条碎裂的"前 600 字 + …" 💬 行,没有 📝 完整文本。

**F-39 反转**：OutResult 不再 fold 进 rolling-log receipt card,改为独立 reply 投递到 userMsgID thread。两个职责清晰分离:
- **Receipt card** 退化为"事件日志 + 元数据"(OutText chunks + state header + footer)
- **Final Result Reply** 独立为"答案交付"(OutResult 完整文本,无 600 cap,无 dedup)

**核心变化**：

1. **`gateway/translate.go`** 不动(`OutboundMessage{Kind: OutResult, Result}` 契约保留)
2. **`channel/feishu/adapter.go::Send(OutResult)`** 重写:`receipt.SetCompleted(ctx)` 关 receipt → `sendResultAsReply` helper 独立发
3. **`channel/feishu/receipt_event.go`** 删 dedup 协调 + 删 `case agent.EventResult`(不再被 receipt 路径触发)
4. **新文件**:
   - `card_sanitize.go` — 移植 cc-connect `sanitizeMarkdownURLs / preprocessFeishuMarkdown / optimizeFeishuCardMarkdown / stripInvalidFeishuCardImages` pipeline(URL / fence / heading demotion / image strip)
   - `result_render.go` — 三段 dispatch(text / post+md / card 2.0)+ `splitMarkdownForDivs` 复用 + 28 KB envelope 防御

**核心 3 段 dispatch** (抄 cc-connect `buildReplyContent`):
- 无 markdown 指示符 → `MsgTypeText`(plain text bubble)
- markdown 存在且 tables > 5 → `MsgTypePost + tag:"md"`(GFM 兜底,无 Card 2.0 表格硬限)
- 默认 → `MsgTypeInteractive` Card 2.0 + 单/多 `tag:"markdown"` div(用 F-37 `splitMarkdownForDivs` 拆 ≤ 1000 runes/div)

**不变式**:
- `OutboundMessage` 契约不变(无新 Kind,无删 Kind,无改 `Result` typed field)
- Gateway 不动(`Translate` 仍产 OutboundMessage)
- ChatSession 不动(`currentTurnUserMsgID` 单数锚点保留)
- `ReplyTo = currentTurnUserMsgID` 不变(独立 reply 也锚同 userMsgID;Feishu 端视觉连接保留)
- §1.4 边界规范保留(OutResult 字段是 typed `agent.ResultEvent`,Channel 自决 target)
- 抽象归抽象 / 具体归具体原则保留(独立 reply target 是 Feishu 自治)
- 1 turn : 1 anchor 不变式保留

**为什么不叫 v2.0**:v1.3 核心不变式(职责隔离、Binding FSM owner、Receipt 自治)全部保留。F-39 是 Channel 自治范围内的渲染目标切换(从"fold into receipt card"到"independent reply"),不影响 nightme 数据模型与 Gateway 契约。

**详细落地**：见 [`docs/feat/F-39-result-as-new-reply.md`](./feat/F-39-result-as-new-reply.md) + `docs/channel/feishu.md` §13.16。

---

### 0.6 文档变更摘要（v1.3.x F-think 增量，2026-08-04）

**背景**：F-thread-route 把 OutThinking / OutToolStart / OutToolEnd / OutCompaction 投到飞书 thread。OutThinking 当前用 plain text（`postThreadReply`）渲染，代码块 / 列表 / 加粗全部丢失；同时用户没有 per-chat 控制 thinking 是否显示的开关。F-think 同时解决这两点：

**增量变化**：

1. **OutThinking → 飞书 lark_md card**
   - 新 helper：`internal/channel/feishu/thinking_card.go` 的 `buildThinkingCard` + `postThreadMarkdownReply`
   - 把 OutThinking 包成 `Card 2.0 interactive` + 单/多 `lark_md` div elements
   - 长内容走 F-37 `splitMarkdownForDivs` 自动按 lark_md div 硬限切分，保留 code block 原子性
   - 其他 OutboundKind（OutToolStart / OutToolEnd / OutCompaction）继续走 plain text `postThreadReply`（它们本身就是单行摘要，markdown 化没意义）

2. **新增 `ThinkMode` per-chat 状态**
   - `ChatSession.ThinkMode`：`ThinkModeShow`（默认，forward OutThinking）/ `ThinkModeHide`（EventHandler gate 丢弃 OutThinking）
   - `ChatSessionEntry.ThinkMode` 持久化字段（Go JSON 容忍缺失）
   - setter `ChatSession.SetThinkMode` + getter `ChatSession.ThinkMode()`

3. **新增 `/think on|off` slash command**
   - 三种调用：`/think on`、`/think off`、`/think`（无参 = 显示状态）
   - 在 Gateway dispatcher 与 `/cwd` `/use` `/kill` `/watch` `/new` 同源
   - 接受别名：`show`/`hide`（语义更准确）

4. **新增 runtime EventHandler gate**
   - 位置：`cmd/nightme/run.go::newEventHandler`，在 Translate + ReplyTo 戳印完成后、`ch.Send` 之前
   - 当 `cs.ThinkMode() == ThinkModeHide && out.Kind == OutThinking` → 静默丢弃 + debug log
   - 其他 OutboundKind 不受影响（OutText / OutResult / OutToolStart / OutToolEnd / OutCompaction / OutInit / OutUsage 全部照旧）
   - 失败开放：ChatSession lookup miss 时仍投递（不静默丢数据）

**v1.3.x 不变式保留**：
- `ChatSession` 不 import `channel/feishu`（不变）
- Channel 接口不暴露 ThinkMode / ChatSession（不变）
- `OutboundMessage` 字段不变（markdown 是 Feishu 渲染层决定，抽象契约仍是 primitive string）
- 抽象归抽象 / 具体归具体原则保留（markdown 渲染是 Channel 自决）
- §1.4 边界规范：thinking content 跨层仍是 string primitive；Feishu 自决是否包装成 lark_md

**为什么不叫 v2.0**：v1.3 核心不变式全部保留。F-think 是：(a) 一个新 per-chat toggle（镜像 F-watch 的模式），(b) 一个 OutThinking 渲染升级（不引入新 Kind、不动 Gateway / ChatSession）。两件事都在 v1.3.x 范畴内。

**详细落地**：见 `internal/registry/think_mode.go` + `internal/chatsession/thinkmode.go` + `internal/gateway/handlers_think.go` + `cmd/nightme/run.go::newEventHandler` + `internal/channel/feishu/thinking_card.go`。

---

### 0.7 文档变更摘要（v1.3.x F-38 增量，2026-08-04）

**背景**：F-thread-route 把 `OutToolStart` / `OutToolEnd` 都投到飞书 thread，每个 tool 产生**两条**独立 thread reply（先 `● Tool(args)` 再 `⎿  …`）。一次 agent turn 调 10 个工具 = 20 条 thread reply，视觉噪声 + 限速成本都很高。同时用户没有 per-chat 开关控制工具调用是否显示——既不能选择 plain text vs 合并格式，也不能选择看 vs 不看。F-38 同时解决这两点。

**增量变化**：

1. **OutToolStart + OutToolEnd 合并为同一条 thread reply**
   - 飞书 adapter 维护 per-turn `userMsgID → FIFO(startMsgID, startBody)` 缓冲（`toolEventBuf`）
   - OutToolStart 发新 thread reply，记下 `startMsgID`
   - OutToolEnd 用 `startMsgID` PATCH 同一 reply（飞书 `PUT /im/v1/messages/{id}` 支持 text 类型 thread reply 就地编辑）
   - merged body = startBody + "\n" + resultBody；用户看到一条 thread reply 同时含 call + result
   - 失败开放：orphan End（buffer miss）或 PATCH 失败 → 走原 `postThreadReply` fallback 发新 thread reply，不静默丢数据
   - **不动** `OutboundMessage` 契约 / Gateway / ChatSession；完全是 Feishu adapter 自治的渲染细节

2. **新增 `ToolsMode` per-chat 状态**
   - `ChatSession.ToolsMode`：`ToolsModeHide`（默认，runtime 丢弃 `OutToolStart` 和 `OutToolEnd`）/ `ToolsModeShow`（runtime 透传，Feishu adapter 走合并路径）
   - `ChatSessionEntry.ToolsMode` 持久化字段（Go JSON 容忍缺失）
   - setter `ChatSession.SetToolsMode` + getter `ChatSession.ToolsMode()`

3. **新增 `/tools on|off` slash command**
   - 三种调用：`/tools on`、`/tools off`、`/tools`（无参 = 显示状态）
   - 在 Gateway dispatcher 与 `/cwd` `/use` `/kill` `/watch` `/think` `/new` 同源
   - 接受别名：`show`/`hide`（语义更准确）
   - 默认值方向与 `/think` **相反**：`/think` 默认 Show（保留 F-thread-route 现有 UX），`/tools` 默认 Hide（quiet by default；用户主动 opt-in 看工具调用）

4. **新增 runtime EventHandler gate**
   - 位置：`cmd/nightme/run.go::newEventHandler`，紧跟现有 ThinkMode gate
   - 当 `cs.ToolsMode() == ToolsModeHide && (out.Kind == OutToolStart || out.Kind == OutToolEnd)` → 静默丢弃 + info log
   - 其他 OutboundKind 不受影响（OutText / OutResult / OutThinking / OutCompaction / OutInit / OutUsage 全部照旧）
   - 失败开放：ChatSession lookup miss 时仍投递（不静默丢数据）
   - 与 ThinkMode gate 正交：两个 gate 可独立配置

**v1.3.x 不变式保留**：
- `ChatSession` 不 import `channel/feishu`（不变）
- Channel 接口不暴露 ToolsMode / ChatSession（不变；合并是 Feishu adapter 自治）
- `OutboundMessage` 字段不变（合并是 Feishu 渲染层决定，抽象契约仍是 primitive ToolInfo）
- 抽象归抽象 / 具体归具体原则保留（thread 合并是 Channel 自决）
- §1.4 边界规范：tool 概念跨层仍是 typed ToolInfo；Feishu 自决是否合并

**为什么不叫 v2.0**：v1.3 核心不变式全部保留。F-38 是：(a) 一个新 per-chat toggle（镜像 F-think 的模式，但默认方向相反），(b) 一个 OutToolStart/End 渲染升级（不引入新 Kind、不动 Gateway / ChatSession；纯 Channel 自治的 PATCH 合并）。两件事都在 v1.3.x 范畴内。

**详细落地**：见 `internal/registry/tools_mode.go` + `internal/chatsession/toolsmode.go` + `internal/gateway/handlers_tools.go` + `cmd/nightme/run.go::newEventHandler` + `internal/channel/feishu/tool_thread_merge.go` + `internal/channel/feishu/adapter.go::Send`。

---

## 1. 架构总览

nightme 是一个**单进程 daemon**，运行在用户的电脑上。它由以下**逻辑组件**组成：

```
┌─────────────────────────────────────────────────────────────┐
│  Channel Adapter (Feishu / WhatsApp / Web UI / Echo ...)   │
│   │  ↑ 自管 receipt card / thread / DOM 节点              │
│   │  ↑ Send(OutboundMessage) → 拿 ReplyTo=userMsgID 路由  │
│   │  user text / file / voice                               │
└────────────────┬────────────────────────────────────────────┘
                 │  InboundMessage / OutboundMessage（带 ReplyTo）
                 ▼
┌─────────────────────────────────────────────────────────────┐
│  nightme (single binary on user's laptop)                   │
│                                                              │
│  ┌─────────────────────────────────────────────────────┐    │
│  │  Gateway  ← 中枢 orchestrator                       │    │
│  │  • chat_id ↔ ChatSession 绑定 (Binding FSM)         │    │
│  │  • outbound 路由 (stamp ReplyTo=userMsgID 送到 Channel)│    │
│  │  • slash command 路由 (/cwd /use /kill /help /agents)│    │
│  │  • ChatSession 生命周期管理 (Create / Restore)       │    │
│  │  • Channel ↔ ChatSession ↔ AgentSession 跨层调度     │    │
│  └──────────┬────────────────────────┬─────────────────┘    │
│             │                        │                       │
│   spawn /   │                        │                       │
│   reuse /   │                        │                       │
│   kill      │                        │                       │
│             ▼                        ▼                       │
│  ┌─────────────────────┐  ┌─────────────────────┐           │
│  │  ChatSession (池)   │  │  AgentSession 池    │           │
│  │  ───────────────── │←→│  ─────────────────  │           │
│  │  activeCwd          │  │  (claude, /A) 进程   │           │
│  │  activeAgent        │  │  (codex,  /A) 进程   │           │
│  │  primaryAgent       │  │  (claude, /B) 进程   │           │
│  │  InputBuffer FSM    │  │  (codex,  /B) 进程   │           │
│  │  (idle ↔ busy)      │  │                      │           │
│  │  currentTurnUserMsgID│ │  1:1 with (agent,cwd)│           │
│  │  (单数, 跟踪变量)   │  │  immutable 标识       │           │
│  └─────────────────────┘  └─────────────────────┘           │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

### 1.1 五个逻辑组件（v1.2）

| 组件 | 职责 | 它**不知道** |
|------|------|------------|
| **Channel Adapter** | IM 协议编解码；`Send(OutboundMessage)` 渲染；**自管** receipt card / thread / DOM 节点的完整生命周期（含 cold-create / PATCH / 终态） | ChatSession、AgentSession、workspace、agent、binding、任何渲染状态 |
| **Gateway** | 中枢 orchestrator：slash command 路由、binding 表（chat_id ↔ ChatSession）、ChatSession 生命周期、Channel↔ChatSession↔AgentSession 跨层调度、outbound 路由（stamp `ReplyTo=userMsgID` 送到对应 Channel） | IM 协议细节、agent 内部协议、PTY/ACP 细节、Channel 内部渲染状态 |
| **ChatSession** | per chat 的会话上下文（持久化）：activeCwd / activeAgent / primaryAgent / InputBuffer FSM / `currentTurnUserMsgID` 跟踪 / AgentSession 池索引 | chat_id 之外没有"自己是谁"；Channel 协议细节；agent 内部协议；receipt 渲染 |
| **AgentSession** | CLI 进程句柄；`(agent, cwd)` 1:1 唯一标识（immutable）；events() chan；sendText/sendBlocks；close | chat_id、ChatSession、binding、Channel、slash command |
| **Bridge** | nightme 与底层 AI Coding CLI 之间的通信抽象；`AgentSession` 接口（Events / SendText / SendBlocks / SendPermission / Close）；四种模式（ACP / SDK / PTY / JSON-IO） | chat、binding、ChatSession、Channel |
| **Process Registry** | JSON 持久化层。两类 entry：`ChatSessionEntry`（chat_id ↔ ChatSession 绑定 + activeCwd/activeAgent/primaryAgent + AgentSession 索引）+ `AgentSessionEntry`（agent + cwd + pid + status）| 运行时语义；只持久化 |

### 1.2 三状态机，三个 owner（v1.2）

v1.3 核心架构不变式——任何状态机都**只有一个** owner，跨层状态机之间**没有循环依赖**：

| 状态机 | Owner | 状态空间 | 持久？ |
|--------|-------|----------|--------|
| **Binding FSM**（chat ↔ ChatSession）| Gateway | 1:1 绑定，永不删 | 是（ChatSessionEntry）|
| **InputBuffer FSM**（per ChatSession）| ChatSession | `idle ↔ busy` | 否（重启丢）|
| **MessageState FSM**（per userMsg）| ChatSession | `received → forwarded → done / error` | 否（重启丢）|
| **AgentSession.Status**（per AgentSession）| AgentSession | `running → detached / exited` | 是（AgentSessionEntry）|
| **ChatSession.ActiveAgentSession**（per ChatSession）| ChatSession | 引用 pool 中的某个 AgentSession | 引用在 ChatSessionEntry |

**非 FSM 跟踪变量（v1.3 新增）**：

| 变量 | Owner | 语义 |
|------|-------|------|
| **`currentTurnUserMsgID`**（per ChatSession）| ChatSession | 当前 turn 的单一 userMsgID 锚点。buffered batch 时锚到最后一条。所有 outbound event 的 `OutboundMessage.ReplyTo` = currentTurnUserMsgID |

**核心 FSM / 跟踪变量的耦合点**（全部经过 Gateway）：
- **Inbound 流**：Channel → Gateway.pumpInbound → dispatchLoop → DispatchInbound (inboundDispatcher) → 命中 `/cwd` `/use` `/kill` 走 slashCommandDispatcher → 走 ChatSession；未命中走 messageDispatcher → `cs.emitMessageState(Received)` + `chatSession.QueueUserMessage` + `chatSession.LookupActiveAgentSession`（lazy spawn）+ `agentSession.SendBlocks` + `cs.emitMessageState(Forwarded)`
- **Outbound 流**：`agentSession.Events()` → session 的 readPump（**单消费者**） → ChatSession.EventCallback → 设 `out.ReplyTo = cs.currentTurnUserMsgID` → `channel.Send` → Channel 内部按 ReplyTo 路由到对应 receipt（card / thread / DOM 节点） → PATCH
- **切 AgentSession**：`/use` 触发 → ChatSession.LookupActiveAgentSession 重新解析 → 切换 ChatSession.EventCallback 目标 → 老 AgentSession 的事件不再消费

### 1.3 不变式（v1.3 强制）

- **`ChatSession` 不 import `channel/feishu`**（事实上根本不 import `channel/` 包）
- **`AgentSession` 不 import `channel/` 也不 import `ChatSession`**（纯进程句柄）
- **Gateway 不 import `channel/feishu`**（只 import `channel.Channel` interface）
- **`ChatSession` 不知道 Channel**（只持有 Gateway 注入的 callback）
- **`AgentSession` 知道自己的 `(agent, cwd)` immutable 标识**
- **Channel 接口不暴露 ChatSession、AgentSession、binding、任何 receipt 概念**——Channel 自管渲染状态（receipt card / thread / DOM 节点），Gateway 一概不知。`Channel` interface 5 个方法：`Name / Start / Stop / Incoming / Send`
- **`agentSession.Events()` chan 的唯一消费者是 session 自己的 readPump**；ChatSession 通过 `ChatSession.EventCallback` 接收事件，**不直接读 chan**（沿用 v1.1 修复）
- **ChatSession 内 `(agent, cwd)` 唯一索引**（不是全局唯一；不同 ChatSession 可有独立 `(claude, /path/A)` AgentSession）
- **`/use` 不重启进程**：永远复用 pool 中的现有 AgentSession，找不到才 spawn 新进程
- **`/cwd` 不重启任何 AgentSession**：永远只改 activeCwd，老 AgentSession 保留在 pool
- **`currentTurnUserMsgID` 单数**：一个 turn 一个 userMsgID 锚点；buffered batch 时锚到最后一条；outbound event 的 `ReplyTo` 必等于此
- **抽象归抽象 / 具体归具体**：Gateway = 路由器（不假设任何 Channel 渲染细节）；Channel = 渲染器（自管 receipt 生命周期，自选存储形态：per-chat map / per-thread map / DOM 节点 / ...）
- **outbound 路由唯一耦合点**：`EventHandler` 在每个 `OutboundMessage` 上设 `out.ReplyTo = cs.currentTurnUserMsgID`。Channel 据此 key 路由。Gateway 不需要知道 Channel 内部 receipt 怎么存
- **Task checklist 三层状态隔离（F-38）**：Claude bridge 持有 provider-session 的规范化 task map/order，并在成功 tool_result 后发完整 snapshot；Gateway 无状态 typed 透传；Channel receipt 只保存当前 userMsgID 的展示副本。任何一层都不得反向读取另一层状态

**v1.3.x 新增（F-33 落地）**：

- **Gateway 不持有 ChatType**：Gateway 只见 `chat_id string`，假设所有 chat 同质；`InboundMessage` / `BindingEntry` / `ChatSession` / registry schema 都不带 `ChatType` 字段
- **Channel 自管 chat 语义**：Channel 知道 chat 类型（DM / group / topic）、知道 thread 渲染，但只通过 `OutboundMessage` 暴露渲染能力，不污染 Gateway / ChatSession / Registry 数据模型
- **`InboundMessage.ReplyTo = message.ParentId`**（Feishu 语义下）：thread 顶层 `RootId` 不进 nightme；`ReplyTo` 字段统一语义 = "被 reply 的那条 message_id"
- **`OutboundMessage.ReplyTo = currentTurnUserMsgID`**：bot reply 永远 anchor 到 user 当前 message_id，不爬 thread 树

**v1.3.x 新增（F-watch 落地）**：

- **`Message.HasMention` 由 Channel 计算，Gateway 不重复算**：channel adapter 读 `message.Mentions` + `chat_type` + `GetBotIdentity()` 拿 bot open_id 算 `HasMention`（DM 永远 true；group 含 bot/@_all 时 true）；Gateway 只 trust 这个 bool
- **`ChatSession.WatchMode` 决定 group 内非 mention 消息是否 drop**：默认 `WatchModeMention`（drop），`/watch on` 切 `WatchModeAll`（pass）；DM 下 `/watch` 为 no-op
- **drop 决策留在 Gateway**：channel 不读 `cs.WatchMode()`，gateway dispatcher 入口统一 gate `!HasMention && WatchMode != All`
- **不续接 Thread**：nightme 不主动追踪 / 创建 thread 上下文，不维护 thread 树；Feishu 端 thread 视觉由 Channel 自治
- **任何 Channel 都不引入 thread 概念**：nightme 数据模型永远不引入 thread 字段（`thread_ts` / `message_thread_id` / `is_threaded` / `thread_id` 等）。Channel 自管 thread 渲染细节（Feishu reply API path 参数 / Slack block kit / Telegram forum mode），但只通过 `OutboundMessage` 暴露能力，不污染 Gateway / ChatSession / Registry 数据模型

**v1.3.x 新增（F-thread-route 落地）**：

- **Channel 按 OutboundKind 自决渲染目标**：Channel 拿到 `OutboundMessage{Kind, ReplyTo, Text, Meta}` 后，**可以**按 `Kind` 自决 routing（thread reply / receipt card / reaction / ...），无需 Gateway 指示。F-thread-route 案例：Feishu adapter 把 `OutThinking` / `OutToolStart` / `OutToolEnd` / `OutCompaction` 投到 thread；`OutText` / `OutResult` / `OutInit` / `OutUsage` 投到 receipt card。**不动 OutboundMessage 契约 / Gateway / ChatSession**
- **OutToolEnd 类型感知摘要 = Channel 职责**：bridge 层把 `ToolEndEvent.Args` 填好（同一 message 的 tool_use block 拿）；Channel 层（Feishu adapter）按 tool name 生成单行摘要（"📄 Read /foo.go → 1234 lines"），不 dump 原始 output。摘要算法属于 Channel 自治（Feishu 用 emoji + 行数；Slack 可用 Block Kit；Web 可用折叠 div）
- **Routing 决策不写进 OutboundMessage.Meta**：Meta 只承载数据载荷（output / err / args），**不**承载 routing hint。Channel 看到 Kind 后自决。
- **F-thread-route 不构成"thread 概念侵入 nightme 数据模型"**：Channel 自管的 thread 是 Feishu SDK API 调用层面的细节（`POST /im/v1/messages/{rootID}/reply`）；nightme 仍然只见 `OutboundMessage.ReplyTo = currentTurnUserMsgID`，跟 F-33 不变式完全兼容

**v1.4 新增（F-14 post rich-text ordering 落地）**：

- **`[]agent.ContentBlock` 是有序 composite**：单一 `ContentText` block 的 `Text` 字段仍是 String，但**承载组合能力的是整个 slice**——`Type ∈ {text, image, file}` 的元素按用户视角的顺序排列。这是对应 Anthropic API `content[]` heterogeneous array 的 1:1 数据结构,不能用 String-with-placeholder 替代（解析歧义 + 类型丢失 + 协议弱化）
- **`InboundMessage.Blocks` 仅 post 富文本非空**：`msg_type=post` 时 `extractAttachments` 返回 `Blocks=ordered-slice`,`Text=""`；其他 msg_types 走 legacy `BuildBlocks(text, atts)` 路径,`Blocks=nil`。`Attachments` 持下载候选 file_key 列表,`Blocks` 持用户视角的有序 turn 形态——两者职责清晰,不冗余
- **`blocks` 顺序 end-to-end 保留**:从 `extractAttachments`(Feishu adapter) → `resolveBlocks`(下载后回填 Path) → `InboundMessage.Blocks` → `messageDispatcher` 选 `msg.Blocks` → `ChatSession.QueueUserMessage(blocks)` → `AgentSession.SendBlocks(blocks)` → bridge 编码到 `content[]` 数组,**每个层都不重排**。任何"先 text 后 image"的拍扁都是顺序 bug
- **path 字段在抽象层只持本地路径**:`ContentBlock.Path` 永远是绝对文件系统路径,**不**存 base64 / 不存 file_key / 不存 URL。base64 inflate 严格限制在 bridge 边界(`bridge/claudecode/session.go::SendBlocks` 的 `readFileAsBase64`)。这是 §1.4 "boundary normalize" 的具体落地:抽象层只持 primitive generic,concrete 编码细节留在具体实现层
- **失败 block omit,不放 placeholder**:post 富文本里某张图下载失败时,`resolveBlocks` 把对应 `ContentImage` block 从 slice 中剔除,**不**用占位符替换(避免 Claude 把"半截 array"误读为"用户传了 3 张图但其中 1 张是 placeholder")。text 上下文保留
- **legacy `BuildBlocks` 顺序契约**:单资源消息(text+image/file)走 legacy 路径,blocks 顺序固定为 `[ContentText(caption)?, ContentImage×N, ContentFile×M]`。这条契约隐式被 v1.1 单测覆盖,新 channel 实现应遵循

### 1.4 抽象 / 具体 边界规范（v1.3.x 强制，多态的核心思路）

跨层通信的架构纪律。本节是一切不变式之上的元原则——其它不变式违反时，几乎都是这条先破了。

**规则（一句话）**：
> 抽象层只承载泛化统一的概念。底层具体实现的细节不得直接引入抽象层。如果某项具体信息确实需要进入抽象层，必须先在 boundary 处归一化（normalize）为泛化形式后才能跨越边界。这是软件工程中多态的核心思路。

**为什么**：每条跨层 hardcoded implicit 协议都是一次 leak。一旦 leak，后续 review 容易"基于现状优化"（typed struct、helper）导致边界进一步塌陷。设计上能站得住的唯一办法是：每条跨层数据要么是 generic primitive（string / int / error），要么 boundary 把它 normalize 成 generic primitive。

**三层边界 + 各自的归一化义务**：

```
┌─────────────────────────────────────────────────────────────┐
│ Concrete (bridge / Channel SDK)                              │
│   claudecode stream-json  ·  pi typed map  ·  Feishu API    │
└───────────────────────┬─────────────────────────────────────┘
                        │ ← 归一化边界 #1: bridge → agent
                        │   bridge 把 native representation
                        │   转成 human-readable string
                        ▼
┌─────────────────────────────────────────────────────────────┐
│ Abstract (agent / Gateway)                                    │
│   agent.AgentEvent  ·  gateway.OutboundMessage             │
│   只见到 generic type：string / int / float64 / error /    │
│   typed struct with generic field names                     │
└───────────────────────┬─────────────────────────────────────┘
                        │ ← 归一化边界 #2: Gateway → Channel
                        │   typed field (ToolInfo) 替代
                        │   Meta map 的 implicit key
                        ▼
┌─────────────────────────────────────────────────────────────┐
│ Concrete (Channel adapter)                                   │
│   Feishu  ·  Slack  ·  Web  ·  ...                          │
│   Channel 自管渲染，可 parse ToolInfo.Args 字符串为 typed    │
└─────────────────────────────────────────────────────────────┘
```

**规则的具体落地**：

1. **抽象层的字段名必须 generic**：禁止用 `file_path` / `command` / `content` 这种 bridge-specific schema 名字。任何 tool 都有 `Name/Args/Output/Err`，这是 generic 概念。
2. **抽象层的字段类型首选 primitive**：`string` / `int` / `float64` / `error`。**不要**为了"类型安全"在抽象层引入 typed struct / enum——那等于在抽象层 hardcode 一种 concrete 的 shape。
3. **bridge 是归一化边界 #1**：claudecode 用 raw JSON string 表达 args；pi 可能用 typed map → 也归一化成 string；pty 可能 raw bytes → 也归一化成 string。Gateway / Channel 只见到 string，不关心 bridge 内部 schema。
4. **Channel 拿到 string 后自己 parse**：如果 Channel 想做类型感知渲染（"Read /foo.go → 1234 lines"），它 parse `ToolInfo.Args` 字符串。但 parse 逻辑属于 Channel 自治，不进 Gateway / agent。
5. **禁止 `OutboundMessage` 上的 `Meta map[string]any`（已删除）**：Meta 是 opaque data container，consumer 不知道里面有什么，producer 也不知道 consumer 会读哪些 key。跨层契约只能走 typed field。`OutboundMessage` 当前 100% typed（§0.5），任何新增跨层数据必须走 typed struct / primitive，不能 re-introduce Meta。
6. **数值类除外**：`CostUSD` / `*Tokens` 保留 typed numeric——任何 agent 都有 token / cost 概念，不需要 string 化（但**走 typed field，不走 Meta**）。
7. **任务概念必须先归一化**：`TaskCreate.subject` / `TaskUpdate.taskId` 等 Claude-native 字段只能存在于 `bridge/claudecode`。跨过 boundary 后统一为 `TaskListEvent{Items: []TaskItem{ID, Subject, ActiveForm, Status}}`；Gateway 只做 `EventTask* → OutTask*` typed 映射，Channel 自决 checkbox / glyph / DOM 渲染。

**反例（2026-08-04 §1.4 终极落地）**：

`OutboundMessage.Meta` 字段已被删除（§0.5）。Meta 原本持有 11 个 implicit key（tool_name / args / output / err / is_error / subtype / state / message_id / reaction_id / session_id / model / agent_name / workspace / branch / input_tokens / output_tokens / cost_usd），全部 hardcoded 隐式协议。下面是 GateWay translate 曾经的 v0.2 代码反例（已全部移除）：

```go
// ❌ 反例（已删除）: Gateway 的 translate 把 bridge-specific 字段名写进 Meta
case agent.EventToolEnd:
    return OutboundMessage{
        Meta: map[string]any{
            "tool_name": name,  // Feishu adapter 用的隐式 key
            "args":      ev.ToolEnd.Args,
            "output":    ev.ToolEnd.Output,
            "err":       ev.ToolEnd.Err,
        },
    }
```

```go
// ❌ 反例（已删除）: Channel 从 Meta 隐式 key 读 concrete 数据
func toolName(m gateway.OutboundMessage) string {
    if n, _ := m.Meta["tool_name"].(string); n != "" {
        return n
    }
    return "tool"
}
```

```go
// ❌ 反例（已删除）: Channel 反向重建 typed event
func durationMs(m gateway.OutboundMessage) int64 {
    return int64(metaInt(m, "duration_ms"))
}
// 然后 receipt.Append(ctx, agent.AgentEvent{Result: &agent.ResultEvent{DurationMs: durationMs(msg), ...}})
// ——Gateway 拆字段塞 Meta，Channel 从 Meta 读回来重建 typed event 再喂给 receipt，纯轮转。
```

**正例（最终状态）**：

```go
// ✅ 正例: Gateway translate 用 typed field 归一化 tool 概念
case agent.EventToolEnd:
    return OutboundMessage{
        Tool: &ToolInfo{
            Name:   name,
            Args:   ev.ToolEnd.Args,
            Output: ev.ToolEnd.Output,
            Err:    ev.ToolEnd.Err,
        },
    }
```

```go
// ✅ 正例: Channel 直接读 typed field
func toolName(m gateway.OutboundMessage) string {
    if m.Tool != nil && m.Tool.Name != "" {
        return m.Tool.Name
    }
    return "tool"
}
```

```go
// ✅ 正例: typed Result 直接传给 receipt.Append，零 round-trip
return receipt.Append(ctx, agent.AgentEvent{
    Kind:   agent.EventResult,
    Result: msg.Result,  // *agent.ResultEvent directly, no Meta reconstruction
})
```

**违反这条规则时的征兆**（review 时用作 checklist）：
- 抽象层 struct 里出现 `Meta map[string]any` 字段且 producer / consumer 各自 hardcode key 名（**已修复**——Meta 删除）
- 抽象层 field 名字含具体 schema（`file_path` / `command` / `pid` 等）
- 抽象层 field 类型是 `*SomeConcreteBridgeStruct`（concrete 类型漏到抽象层）
- Gateway / agent 包 import 了 channel 包（直接依赖关系）
- 文档里出现"key 由 Channel X 约定"——这等于承认 implicit 协议存在
- Gateway 把 typed event 拆字段塞 Meta / channel 又读 Meta 重建 typed event——纯轮转，无价值

**例外与升级路径**：

如果某个 concrete 实现真的需要向抽象层暴露一项数据（且无法在 boundary 归一化），按以下顺序升级：
1. 先问：能否不暴露？channel 自治自己读 SDK 不行吗？
2. 再问：能否在 boundary normalize 成 primitive？
3. 再问：能否抽成 typed struct（generic 字段名）？
4. 最后：扩展抽象层字段，并在 §1.3 不变式里明文记一条。

升级路径 #4 是最后一根稻草。F-37 review 把 `Meta["args"]` 升级到 `OutboundMessage.Tool *ToolInfo` 就是走的路径 #3（typed struct），不在抽象层暴露 schema。

---

## 2. 数据流（概念）

### 2.1 用户消息 → CLI 输入（Inbound）

```
IM 消息事件
  → Channel Adapter 解码为统一 InboundMessage{chat_id, user_id, text, attachments, blocks, message_id, reply_to, time, has_mention}
      ├─ reply_to = message.ParentId（F-33：Feishu 原生 parent_id，thread 顶层 RootId 不进 nightme）
      ├─ has_mention（F-watch：DM 永远 true；group 含 bot/@_all 时 true；由 channel adapter 根据 Mentions + chat_type + GetBotIdentity 计算）
      ├─ **同步下载 attachments 到本地路径**（F-14 v1.4a：publish 前必须填 LocalPath）
      │     ├─ 全失败 → ch.Send("❌ N attachments failed…") + return（不进 ch.Incoming，Agent 看不到这条）
      │     ├─ 部分失败 → ch.Send("⚠️ K of N failed; sending the rest") + 继续
      │     └─ 全部成功 → 静默继续
      ├─ **post 富文本**：`extractAttachments` 产出有序 `[]agent.ContentBlock`（blocks），image 节点占位 file_key → 下载后 resolve 回填 LocalPath（F-14 v1.4b）
      │     - 单资源消息（image/file/audio/media）：`blocks == nil`，走 legacy `BuildBlocks(text, atts)`
      └─ publish 到 ch.Incoming()

Gateway.pumpInbound (per-channel)
  └ push 到 channelCh

Gateway.dispatchLoop
  └ Handle(ctx, msg)
     ├ ParseCommand(msg.Text)
     │   ├ 命中 (/cwd /use /kill /watch /help /agents) → handler(msg)
     │   │   └ handler 走 gateway.bindings → chatSession.xxx → reply via channel.Send
     │   └ 未命中 / 普通文本 → messageDispatcher(ctx, msg)
     ├ F-watch gate(F-watch 位置:Handle 入口在 dispatchLoop → slashCommandDispatcher 前)
     │   ├ msg.HasMention == true → pass-through(原有路径)
     │   ├ msg.HasMention == false 且 cs.WatchMode() == WatchModeAll → pass-through
     │   └ msg.HasMention == false 且 cs.WatchMode() == WatchModeMention(默认) → drop(F-watch:不发 ack,静默跳过)
     └ slashCommandDispatcher 走 handler；messageDispatcher 走 ChatSession（详见 §2.2）

messageDispatcher(ctx, msg)
  ├ gateway.bindings[msg.chat_id] 查 ChatSession
  │   ├ nil → channel.Send("no chat session, /cwd first")
  │   └ Status != Ready → channel.Send("not ready, /cwd + /use first")
  ├ (0) cs.emitMessageState(msg.MessageID, StateReceived)   ← F-31: 触发 MessageState(Received) 事件
  ├ (a) chatSession.LookupActiveAgentSession() (lazy spawn on miss)
  ├ (b) cs.emitMessageState(msg.MessageID, StateForwarded)   ← F-31: dispatch 成功后触发
  ├ (c) **blocks 路径选择**（F-14 v1.4b）：
  │     ├─ msg.Blocks != nil（post path）   → blocks = msg.Blocks    ← 顺序由 Feishu paragraph 决定
  │     └─ else（legacy single-resource）   → blocks = feishu.BuildBlocks(msg.Text, msg.Attachments)
  ├ (d) chatSession.QueueUserMessage(blocks, msg.MessageID)
  │       InputBuffer FSM:
  │         ├ Idle → 立即 SendBlocks(blocks) → return (dispatched=true)
  │         │   └ onFlush 钩子（ChatSession.defaultFlushHookLocked）触发:
  │         │       ├ cs.currentTurnUserMsgID = msg.MessageID(单数)
  │         │       └ agentSession.SendBlocks(combined)
  │         └ Busy → 入队 → return (dispatched=false)
  └ 如果 queued (Busy):
        cs.currentTurnUserMsgID 不变(仍是上一 turn 的值)
        onFlush 钩子在 EventDone 触发 flush 时:
          ├ onFlush(blocks, userMsgIDs)
          │   ├ cs.currentTurnUserMsgID = userMsgIDs[len-1](最后一条)
          │   └ agentSession.SendBlocks(combined)
          └

**v1.4 变化**（F-14）：
- **v1.4a**：`Channel Adapter` 在 `handleMessage` 内**同步**调 `DownloadAttachments`，确保 `InboundMessage.Attachments[i].LocalPath` 在 publish 前已填好。v1.1–v1.3 该函数未被生产代码调用过（仅单测），导致所有 attachment 在 `BuildBlocks` 的 `LocalPath == ""` 分支被静默 skip。
- **v1.4b**：`post` 富文本按 paragraph node 顺序产出 `[]agent.ContentBlock`（`msg.Blocks`），`messageDispatcher` 优先使用，非 post 走 legacy `BuildBlocks(text, atts)`。单资源消息（image/file/audio/media）的 Text + Attachments 模型不变。
- 全失败 → Channel 自决发一条文本通知用户 + drop（不进 ch.Incoming）；部分失败 → 通知用户 + 继续把成功的部分转给 Agent（失败节点从 blocks 中 omit）。

**v1.3 变化**：去掉 receipt lifecycle 步骤(Gateway 不再调 `ch.CreateReceipt / UpdateReceipt / DisposeReceipt`)。Channel 在收到第一个带 `ReplyTo=userMsgID` 的 OutboundMessage 时,自行决定 cold-create / 复用 receipt card / thread / DOM 节点。详见 §2.2。

**MessageState 事件**由 ChatSession 在 lifecycle 各点 emit(步骤 0、b),由 Gateway 的 `OnMessageState` 回调翻译为 `OutboundMessage{Kind: OutMessageState}` 并通过 Channel.Send 转发。详见 §2.5。
```

### 2.2 CLI 输出 → 用户（Outbound）

**核心语义：1 turn : 1 anchor, n events**。每个 agent turn 由 ChatSession 锚定到单一 `currentTurnUserMsgID`（buffered batch 时锚到最后一条 userMsgID）；EventHandler 在每个 OutboundMessage 上设 `out.ReplyTo = currentTurnUserMsgID`；Channel 据此路由到对应 receipt（card / thread / DOM 节点）。`ReplyTo == ""` 是仅有的"无锚" case（启动期 EventInit、系统日志、内部事件），Channel 走 plain text / 跳过。

```
Claude Code 进程 (PTY child, cwd = chatSession.activeCwd)
  ↓ stdout 是 stream-json 行
bridge/claudecode.pumpStream
  ├ 逐行解析 JSON
  ├ 翻译成 agent.AgentEvent (Text / ToolStart / ToolEnd / Permission / Done / Error / Result / Usage / Compaction / Init)
  └ 写入 agentSession.events (chan cap 64)
  ↓
agentSession.readPump (单消费者)
  ├ for ev := range as.Events()
  ├ ChatSession.EventCallback(s, ev)   ← ChatSession 在这里注册的回调（**不另起 pump**）
  ├ InputBuffer.SetState(Busy)
  ├ EventDone/Error → SetState(Idle) + OnTurnEnded() + 翻 queued 消息出去
  └ EventDone/Error → cs.emitMessageStateForCurrentTurn(StateDone|StateError)
  ↓
ChatSession.onAgentEvent(s, ev)        // EventCallback 驱动
  ├ InputBuffer.SetState(Busy)
  ├ out, send := gateway.Translate(chatID, ev)
  ├ out.ReplyTo = cs.currentTurnUserMsgID    ← **关键耦合点**（唯一关联信息）
  ├ (TaskCreate / TaskUpdate) bridge 在匹配 tool_result 确认成功后更新 session-local task map
  │   ├ emit EventTaskCreate / EventTaskUpdate（每次携带完整 TaskListEvent snapshot）
  │   ├ gateway.Translate → OutTaskCreate / OutTaskUpdate（typed TaskList payload）
  │   └ Feishu Channel → 当前 ReplyTo 对应 receipt.SetTaskList → 原位 PATCH checklist
  └ if send: channel.Send(ctx, out)
  ↓
channel.Send(ctx, OutboundMessage)
  ├ 看 msg.ReplyTo（userMsgID）
  ├ 内部按 userMsgID 查自己的 receipt:
  │   ├ 命中 → PATCH / 更新（receipt card 追加内容、thread 发新回复、DOM 节点 in-place 编辑）
  │   └ miss → cold-create 一个新 receipt（userMsgID 作为 key），然后追加
  ├ (OutMessageState case) → AddReaction on Meta["message_id"] → 用户消息挂 ⏳/🔄/✅/❌
  └ (OutCard case) → 发交互卡片（permission prompt 等）
```

**关键不变量（v1.3）**：
- `ChatSession.EventCallback` 是当前 **active AgentSession** 的唯一消费者。当 `/use` 切换 active 时，ChatSession 重新注册 callback 到新的 AgentSession，老 AgentSession 的 `Events()` 不再被消费（但进程可继续跑、产出事件被丢弃——符合 PRD §4.3 的"过时的不管"语义）
- Gateway **永不** 持有 receipt / receipt map / receipt FSM——Channel 自治
- `out.ReplyTo = cs.currentTurnUserMsgID` 是 Gateway → Channel 的**唯一关联信息**——Channel 拿这个 key 路由，内部的存储形态（map / DOM / thread）自己选

**1 turn : 1 anchor 示例**（buffered batch：3 条用户消息被 flush 为 1 个 agent turn）：
- ChatSession 的 InputBuffer 攒着 [userMsgID_a, userMsgID_b, userMsgID_c]
- 上一 turn 的 EventDone 触发 onFlush → `cs.currentTurnUserMsgID = msg_c`（最后一条）
- agent 看到 stdin 是 3 条合并输入，产出 N 个 event
- 每个 event 都 PATCH msg_c 对应的那张 receipt card（F-25 rolling-log）
- msg_a / msg_b 自身的 MessageState(Done) 仍然触发（走 §2.5 的 MessageState 路径）

### 2.3 用户用 slash command 管理 ChatSession

#### `/cwd <path>`

```
handler.cwd(ctx, msg, args)
  ├ 验证 path（~ 展开、绝对路径、目录存在）
  ├ gateway.bindings[msg.chat_id] 查 ChatSession
  │   ├ nil → channel.Send("no chat session yet, send /cwd first... wait, you are sending /cwd. retry.")
  │   └ 存在 → 继续
  ├ chatSession.SetActiveCwd(abs)  ← 仅改 activeCwd, 不动 AgentSession
  │   (AgentSession 池中的所有项不动; 切回原 cwd 时复用老 AgentSession)
  ├ registry.Upsert(ChatSessionEntry)
  └ ch.Send("Workspace set to <abs>")
```

**关键变化（v1.2）**：`/cwd` **不触发 spawn**。它是"切换 activeCwd"的纯状态变更命令。当用户后续发消息时，ChatSession 通过 `LookupActiveAgentSession()` 重新解析 `(activeAgent, activeCwd)`，按需复用或 spawn。

#### `/use <agent>`

```
handler.use(ctx, msg, args)
  ├ gateway.bindings[msg.chat_id] 查 ChatSession
  │   ├ nil → channel.Send("no chat session, /cwd first")
  │   └ 存在 → 继续
  ├ agentName := args[0]
  ├ chatSession.SetActiveAgent(agentName)   ← 仅改 activeAgent
  ├ agentSession = chatSession.LookupActiveAgentSession()
  │   ├ pool[(activeAgent, activeCwd)] 命中 → 复用 (不重启进程)
  │   └ miss → spawn 新 AgentSession(agentName, activeCwd)
  ├ chatSession.SetActiveAgentSession(agentSession)
  ├ registry.Upsert(ChatSessionEntry + AgentSessionEntry)
  └ ch.Send("Now using <agent>, pid=<N>, cwd=<ws>")
```

**关键变化（v1.2）**：
- `/use` **永不重启进程**——pool 里有就复用，没有才 spawn
- 切换前已 queued 的消息（InputBuffer）→ 自动 flush 到新的 active AgentSession
- 老 AgentSession 保留在 pool，切回原 agent/cwd 时能复用

#### `/kill`

```
handler.kill(ctx, msg, args)
  ├ gateway.bindings[msg.chat_id] 查 ChatSession
  ├ 杀 ChatSession 内所有 AgentSession (清空 pool)
  ├ ChatSession.SetActiveAgentSession(nil)
  ├ InputBuffer 清空 (queued 消息丢失, 不重发)
  ├ 老 receipts 强制 dispose (或等自然衰减)
  ├ registry 更新 (pool 清空, activeAgentSessionId=null)
  └ ch.Send("All agents killed. Send a message to start fresh.")
```

**关键变化（v1.2）**：`/kill` = "清空 ChatSession 的所有 AgentSession 上下文，重启新的"。下次消息触发 spawn 新 AgentSession。

### 2.4 Receipt 渲染（Channel 自治，v1.3）

> **v1.3 起本章不再包含 Gateway 端 FSM 描述**——Receipt FSM 从 Gateway 移除后，receipt 的生命周期完全由 Channel 自治。Gateway 不知道 Receipt 是什么、有几个、存哪里。

**Channel 自治范围内的渲染行为**（仅作协议描述，**不是 Gateway 责任**）：

- 每个 Channel 在收到第一个 `OutboundMessage{ReplyTo: userMsgID}` 时，自行决定如何"开张"：
  - **Feishu**：cold-create 一张 interactive card 作为 userMsg 的 reply；记下 cardMsgID 供后续 PATCH
  - **Slack**：在 userMsg 关联的 thread 下发第一条回复
  - **Web**：在 chat DOM 中插入一个 receipt block，带 `data-user-msg-id` 标记

- 每个 Channel 在收到后续 `OutboundMessage{ReplyTo: userMsgID}` 时，按自己定义的渲染规则 PATCH 对应 receipt：
  - Feishu：`UpdateMessage(cardMsgID, body)` in-place 编辑 card
  - Slack：thread 内发新回复 / 编辑已有回复
  - Web：更新 DOM block 的内容

- 每个 Channel 在收到 `OutMessageState` 时，按状态映射 emoji：
  - StateReceived/Forwarded/Done/Error → ⏳ / 🔄 / ✅ / ❌

**Gateway 视角**：
- Gateway 只发 `OutboundMessage{ReplyTo: userMsgID, Kind: OutText|ToolStart|...}`
- Gateway **不知道** Channel 内部有没有 receipt、存了多少、是否要清理
- Channel 内部状态（Feishu 的 entries / tokens / agentName / state / ...）完全 Channel 私有

**实现细节**：见 [`feat/F-25-rolling-log.md`](./feat/F-25-rolling-log.md)（v1.3 重命名为"rolling-log"，强调是 Channel 实现细节）。原 F-26 gateway-hub §6 中描述的 Gateway 端 Receipt FSM 代码路径全部删除。

---

### 2.5 Message Lifecycle Tracking（v1.3 新增）

**核心问题**：用户在 IM 里发了一条消息，怎么知道系统处理到哪一步了？—— `MessageState` 是这个问题的答案。

#### 2.5.1 概念

**`MessageState` = 消息的生命周期阶段属性**。回答 3 个问题：

1. ChatSession 收到消息没有？ → `StateReceived`
2. 消息转给 AgentSession 了没有？ → `StateForwarded`
3. AgentSession 执行完成了没有？ → `StateDone` (+ `StateError` 可选)

每条普通用户消息在系统里流转时，对应 `MessageState` 事件被 emit；Channel 把它渲染成平台原生视觉表达（Feishu reaction emoji，Slack emoji 短码，Web UI DOM 元素）。

#### 2.5.2 4 层事件流

```
[1] ChatSession / AgentSession
        │  emit MessageState event (state + userMsgID)
        │  via callback mechanism
        ▼
[2] Gateway
        │  接 OnMessageState callback
        │  翻译成 OutboundMessage{Kind: OutMessageState, Meta: {message_id, state}}
        │  通过 Channel.Send 提交
        ▼
[3] Channel (feishu adapter / future slack / future web ...)
        │  在 Send dispatcher 看到 OutMessageState case
        │  state → 平台原生视觉表达
        │  Feishu: AddReaction(userMsgID, emoji_type)
        ▼
[4] Platform SDK (Feishu / Slack / DOM)
        ▼
    用户看到视觉反馈（emoji / icon / progress bar）
```

**每层职责**：

| 层 | 知道什么 | 不知道什么 |
|---|---|---|
| ChatSession / AgentSession | "现在消息进入 X 状态了" | 怎么传输、谁接收、长什么样 |
| Gateway | OutboundMessage wire format；Channel interface | emoji 是什么、平台细节 |
| Channel (feishu / future) | state → emoji_type 映射；平台 SDK | 事件从哪来、谁 emit |
| Platform SDK | 平台原生 API | — |

#### 2.5.3 抽象事件契约

**`OutboundKind.OutMessageState`** — 新增枚举值，承载 message state 变化事件。

```go
OutboundMessage{
    Kind: OutMessageState,
    ChatID: chatID,
    Meta: map[string]any{
        "message_id": userMsgID,           // 必填：标记哪条用户消息
        "state":      receipt.MessageState, // 必填：状态值
    },
}
```

#### 2.5.4 触发点

| 触发时机 | 状态 | 说明 |
|---|---|---|
| `ChatSession.GetOrCreate(chatID)` 成功后 | `StateReceived` | 消息首次进 ChatSession |
| `ChatSession.LookupActiveAgentSession()` 成功 | `StateForwarded` | spawn 成功或命中 running pool |
| `ChatSession.runReadPump` 收到 `EventDone` | `StateDone` | agent 处理完 |
| `ChatSession.runReadPump` 收到 `EventError` | `StateError` | agent 出错 |

**Scope 强约束**：MessageState **只对普通用户消息触发**。Slash command（`/cwd` `/use` `/kill` 等）不产生 MessageState —— 控制平面有 `OutCommandReply` 作为反馈。

详见 [`feat/F-31-message-state.md`](./feat/F-31-message-state.md)。

---

## 3. ChatSession 生命周期

ChatSession 分**三层状态**——但**三层状态的所有权清晰分离**：

```
                       /cwd (binding 不存在)
    [no binding] ────────────────────────────────► [binding → ChatSession, no activeCwd]
                                                                  │
                                                                  │ /cwd <path>
                                                                  ▼
                              /kill ◄─────────────── [binding → ChatSession, activeCwd=/path, no active AgentSession]
                                │                          ▲
                                │                          │ /use <agent> spawn
                                │                          │
                                ▼                          │
                         [binding → ChatSession, activeCwd=/path, active=(agent, cwd), pool 多条]
```

**关键规则**（v1.2）：
- **activeCwd 是 /use 的硬性前置条件**：没有 activeCwd → ChatSession 无 active AgentSession → 不能 dispatch
- **`/cwd` 不杀任何 AgentSession**：永远只改 activeCwd，老 AgentSession 保留在 pool
- **`/use` 永不重启进程**：永远复用 pool 中现有的，找不到才 spawn
- **Binding 永不过期**：chat_id 永久绑定 ChatSession；ChatSession 跨 daemon 重启恢复
- **AgentSession 不永生**：进程死掉就 status=exited；但 ChatSession 池里仍然引用它（pool 标记 `[exited]`）
- **/kill 清空 pool**：所有 AgentSession 被杀 + 池清空

### 3.1 Chat 类型语义（F-33 重写）

**ChatID 唯一** —— nightme 数据模型只有 `chat_id string`,不持有 chat 类型分类。所有 chat 在 Gateway / ChatSession / Registry 视角下**完全同质**。

**Channel 自管 chat 语义** —— Channel adapter 知道 chat 类型(DM / group / topic),但只通过 `OutboundMessage` 暴露渲染能力:
- Feishu 原生 chat_type:`p2p` / `group` / `topic_group`
- Channel 内部归一化只覆盖 DM / Group / NotSupported 三态(F-33 移除 `ChatTypeThread` 常量)
- topic_group(Feishu thread)**不特殊处理**:消息跟普通 group 走完全相同路径,thread 视觉由 Feishu 端决定

**任何 Channel 都不引入 thread 概念** —— Slack `thread_ts`、Telegram `message_thread_id`、Discord thread 等不进 nightme 数据模型,仅 Channel 内部渲染时使用。如未来 Slack thread 等场景需要支持,在 Channel 包内自治实现,**不动 nightme 数据模型**。

**注册兼容性** —— `ChatSessionEntry.ChatType` 字段删除后,旧 `chat_sessions.json` 文件中残留的 `chatType` 字段被 Go JSON unmarshal 默认容忍,不破坏加载;数据不再被使用,无需迁移脚本。

#### 3.1.1 WatchMode：per-chat 群消息全收开关

**问题**：nightme bot 加到飞书群后,默认只接收 @ bot 的消息（飞书 `im:message.group_at_msg:readonly` 行为）。如果用户想让 bot "听全群",得在飞书后台手动勾选 `im:message.group_msg` 权限 + 自己处理噪声。

**方案**：per-chat `WatchMode` 字段,默认 `WatchModeMention`(只 @ 收),`/watch on` 切到 `WatchModeAll`(全收)。

| 模式 | 含义 | 触发 |
|------|------|------|
| `WatchModeMention`（默认）| 只在 @ bot 或 @_all 时处理 | channel `HasMention == true` |
| `WatchModeAll` | 处理 chat 内所有消息,无论是否 @ | `cs.WatchMode() == WatchModeAll` |

**Channel 自管 chat 语义的不变式保留**：
- `WatchMode` 字段挂在 `ChatSession`,**不**在 `Message` / `OutboundMessage` / Channel interface
- Channel adapter 只暴露 **`HasMention bool`** 给 `Message`（含 bot 或 @_all 为 true；**DM 永远为 true**）
- Gateway dispatcher 拿 `HasMention` + `cs.WatchMode()` 做 gate,**不**读 chat type
- Chat type（DM/group/topic_group）**不**进 nightme 数据模型

**DM 不受 `WatchMode` 影响的锁定不变式**：
- DM 消息在 channel adapter 里永远被设为 `HasMention=true`（每条 DM 都是 "addressed to bot"）
- 因此 gateway gate `!HasMention && WatchMode != All` 对 DM 永远为 false → DM 永远不 drop
- `WatchMode` 字段在 DM 下被写入仅为保留用户偏好，切回 group 后生效
- 这条不变式由两层测试锁死：
  - `internal/channel/feishu/mention_test.go::TestComputeHasMention_DMInvariant`
  - `internal/gateway/dispatch_watch_test.go::TestDispatchInbound_WatchModeGate_DMInvariant`

**Slash command**：`/watch on` / `/watch off` / `/watch`(无参 = 显示状态)。 handler 由 Gateway dispatcher 处理,跟 `/cwd` `/use` `/kill` 同一路径。

**持久化**：`ChatSessionEntry.WatchMode` 字段(默认 `WatchModeMention`)。Go JSON unmarshal 容忍缺失字段,旧 `chat_sessions.json` 无 `watchMode` 字段时安全 fallback 到默认。

**飞书 scope 配合**：`DefaultAddons()` 始终包含 `im:message.group_msg`(不带 `:readonly`)——bot 默认接收所有群消息,由 `WatchMode` 在 nightme 侧 gate。**不**走 CLI flag opt-in,默认就是"飞书送全,nightme 决定要不要处理"。

**职责边界**：
- Channel adapter: 计算 `HasMention`(`message.Mentions` + `chat_type` + `GetBotIdentity()` 拿 bot open_id)
- Gateway dispatcher: 检查 `HasMention` + `WatchMode` 决定 drop 或 pass
- ChatSession: 持有 `WatchMode` 状态 + 提供 setter

**详细落地**：见 [`feat/F-08-channel-abstraction.md`](./feat/F-08-channel-abstraction.md) §Message 字段 + [`channel/feishu.md`](./channel/feishu.md) §6.7 mention strip。

### 3.1.2 ThinkMode：per-chat thinking 内容显示开关（F-think）

**问题**：F-thread-route 把 OutThinking 投到飞书 thread 后用户无法关闭 —— 既不能选择 plain text vs markdown，也不能选择看 vs 不看。F-think 让 nightme 侧接管这两个决定权。

**方案**：per-chat `ThinkMode` 字段，默认 `ThinkModeShow`（runtime 投递 OutThinking，由 Feishu adapter 渲染成 lark_md card），`/think off` 切到 `ThinkModeHide`（runtime 在 EventHandler gate 丢弃 OutThinking）。

| 模式 | 含义 | 触发 |
|------|------|------|
| `ThinkModeShow`（默认）| OutThinking 投递到 Channel，渲染为飞书 lark_md thread card | `/think on` |
| `ThinkModeHide` | EventHandler gate 静默丢弃 OutThinking，其他 OutboundKind 不受影响 | `/think off` |

**渲染细节（F-think §3.1.2.1）**：当 `ThinkMode=Show` 且 ChatType=Feishu，OutThinking 由 `internal/channel/feishu/thinking_card.go` 渲染：

```
buildThinkingCard("💭 <text>")  →  Card 2.0 JSON
  { config: {wide_screen_mode: true},
    elements: [{tag:"div", text:{tag:"lark_md", content:"💭 <text>"}}] }
```

长内容（> 1000 runes）自动按 F-37 `splitMarkdownForDivs` 切多 div，code block 整块保留。Echo / Web 等其他 Channel 仍可自由决定如何渲染 `OutboundMessage.Text`（不动抽象契约）。

**Channel 自管 chat 语义的不变式保留**：
- `ThinkMode` 字段挂在 `ChatSession`，**不**在 `Message` / `OutboundMessage` / Channel interface
- Channel adapter 不知道 `ThinkMode`，由 runtime EventHandler 在出站前决定
- Chat type（DM/group/topic_group）**不**进 nightme 数据模型（沿用 F-33 不变式）
- `OutboundMessage` schema 不变：`Text string` 仍是 primitive，Channel 自决 markdown 化

**DM 不受 `ThinkMode` 影响的不变式**：
- DM chat 下 `/think off` 仍按 Hide 处理 OutThinking —— 与 `/watch off` 不同（`/watch` 在 DM 下因 HasMention 恒为 true 而永远不丢消息，`/think` 是 Outbound 维度，与 HasMention 解耦）
- `ThinkMode` 字段在 DM 下被写入仅为保留用户偏好

**Slash command**：`/think on` / `/think off` / `/think`（无参 = 显示状态）。handler 由 Gateway dispatcher 处理,跟 `/cwd` `/use` `/kill` `/watch` `/new` 同一路径。接受别名：`show`/`hide`。

**持久化**：`ChatSessionEntry.ThinkMode` 字段（默认 `ThinkModeShow`）。Go JSON unmarshal 容忍缺失字段，旧 `chat_sessions.json` 无 `thinkMode` 字段时安全 fallback 到默认（与 WatchMode 设计完全镜像，但默认值方向相反 —— WatchMode 是"安全 = 少收"= Mention 默认；ThinkMode 是"安全 = 不动现有行为"= Show 默认）。

**职责边界**：
- ChatSession：持有 `ThinkMode` 状态 + 提供 setter
- runtime EventHandler（`cmd/nightme/run.go::newEventHandler`）：gate 决策点，读 `cs.ThinkMode()`
- Channel adapter：照常处理到达的 OutboundMessage，不感知 ThinkMode

**详细落地**：见 [`internal/gateway/handlers_think.go`](./internal/gateway/handlers_think.go) + [`cmd/nightme/run.go::newEventHandler`](./cmd/nightme/run.go) + [`internal/channel/feishu/thinking_card.go`](./internal/channel/feishu/thinking_card.go)。

### 3.1.3 ToolsMode：per-chat 工具调用显示开关 + 合并渲染（F-38）

**问题**：F-thread-route 把 `OutToolStart` / `OutToolEnd` 都投到飞书 thread，每个 tool 产生**两条**独立 thread reply（先 `● Tool(args)` 再 `⎿  …`）。一次 agent turn 调 10 个工具 = 20 条 thread reply，视觉噪声 + 限速成本都很高。同时用户没有 per-chat 开关控制工具调用是否显示。F-38 同时解决这两点：(a) 合并渲染——每对 tool 是**一条** thread reply，不是两条；(b) per-chat toggle 控制工具调用是否可见。

**方案**：per-chat `ToolsMode` 字段，默认 `ToolsModeHide`（runtime 丢弃 `OutToolStart` 和 `OutToolEnd`），`/tools on` 切到 `ToolsModeShow`（runtime 透传，Feishu adapter 走合并路径）。合并策略：在 Feishu adapter 内为每个 turn 维护 `userMsgID → FIFO(startMsgID, startBody)` 缓冲，OutToolStart 发新 thread reply 时记下 message_id，OutToolEnd 到达时用 PATCH 同一 reply 把 result 行追加进去。

| 模式 | 含义 | 触发 |
|------|------|------|
| `ToolsModeHide`（默认）| EventHandler gate 静默丢弃 `OutToolStart` 和 `OutToolEnd`，其他 OutboundKind 不受影响 | `/tools off`（或 default） |
| `ToolsModeShow` | Runtime 透传 `OutToolStart` / `OutToolEnd`；Feishu adapter 走合并路径——Start 发新 thread reply + 记下 msg_id，End 用 PATCH 同一 reply 追加 result 行 | `/tools on` |

**渲染细节（F-38 §3.1.3.1）**：当 `ToolsMode=Show` 且 ChatType=Feishu，OutToolStart 和 OutToolEnd 由 `internal/channel/feishu/tool_thread_merge.go` 合并：

```
postThreadReplyWithID("● Bash(ls)")      →  message_id = om_xxx
                                            (push FIFO: userMsgID → (om_xxx, "● Bash(ls)"))

... tool returns ...

mergeToolReply(om_xxx, "● Bash(ls)\n⎿  💻 Bash → 3 lines")
                                            →  Feishu PATCH om_xxx with merged body
```

长工具名 / 长 args 走现有 `formatToolStartCall` 的 rune-safe truncate；长 result 由 `summarizeToolResult` 单行摘要（无 PII leak）。Echo / Web 等其他 Channel 仍可自由决定如何渲染 `OutboundMessage.Tool`（不动抽象契约）。

**Channel 自管 chat 语义的不变式保留**：
- `ToolsMode` 字段挂在 `ChatSession`，**不**在 `Message` / `OutboundMessage` / Channel interface
- Channel adapter 不知道 `ToolsMode`，由 runtime EventHandler 在出站前决定
- Chat type（DM/group/topic_group）**不**进 nightme 数据模型（沿用 F-33 不变式）
- `OutboundMessage` schema 不变：`Tool *ToolInfo` 仍是 typed primitive，Channel 自决合并 vs 分开发
- 合并 vs 分开发是 **Channel 自治**的渲染细节——`Gateway` 不持有"thread 怎么聚合"的决策

**DM 不受 `ToolsMode` 影响的语义**：
- DM chat 下 `/tools off` 仍按 Hide 处理 `OutToolStart` / `OutToolEnd` —— 与 `/watch off` 不同（`/watch` 在 DM 下因 HasMention 恒为 true 而永远不丢消息，`/tools` 是 Outbound 维度，与 HasMention 解耦）
- `ToolsMode` 字段在 DM 下被写入仅为保留用户偏好

**Slash command**：`/tools on` / `/tools off` / `/tools`（无参 = 显示状态）。handler 由 Gateway dispatcher 处理，跟 `/cwd` `/use` `/kill` `/watch` `/think` `/new` 同一路径。接受别名：`show`/`hide`。

**默认方向（vs `/think`）**：`/think` 默认 Show（保留 F-thread-route 现有 UX —— 默认让用户看到 thinking）；`/tools` 默认 Hide（quiet by default —— 工具调用是 agent progress stream 中最吵的部分，多数用户不要；opt-in 才显示）。两者方向相反但都是"safe default"的不同解读。

**持久化**：`ChatSessionEntry.ToolsMode` 字段（默认 `ToolsModeHide`）。Go JSON unmarshal 容忍缺失字段，旧 `chat_sessions.json` 无 `toolsMode` 字段时安全 fallback 到默认（与 WatchMode / ThinkMode 设计完全镜像）。

**职责边界**：
- ChatSession：持有 `ToolsMode` 状态 + 提供 setter
- runtime EventHandler（`cmd/nightme/run.go::newEventHandler`）：gate 决策点，读 `cs.ToolsMode()`；仅对 `OutToolStart` / `OutToolEnd` 生效
- Channel adapter：照常处理到达的 OutboundMessage，不感知 ToolsMode；Feishu 自决是否合并
- 合并实现：Feishu adapter 自治（`internal/channel/feishu/tool_thread_merge.go`）；不动抽象层

**详细落地**：见 [`internal/registry/tools_mode.go`](./internal/registry/tools_mode.go) + [`internal/chatsession/toolsmode.go`](./internal/chatsession/toolsmode.go) + [`internal/gateway/handlers_tools.go`](./internal/gateway/handlers_tools.go) + [`cmd/nightme/run.go::newEventHandler`](./cmd/nightme/run.go) + [`internal/channel/feishu/tool_thread_merge.go`](./internal/channel/feishu/tool_thread_merge.go) + [`internal/channel/feishu/adapter.go::Send`](./internal/channel/feishu/adapter.go)。

### 3.2 状态转换触发器（v1.2）

| From | 触发 | To | 由谁驱动 |
|------|------|-----|----------|
| (no binding) | 用户发送 /cwd | binding 创建 + ChatSession.activeCwd 设置 + pool 初始化 | Gateway.handler.cwd |
| ChatSession, no activeCwd | 用户发送 /cwd | activeCwd 设置 | Gateway.handler.cwd |
| ChatSession, activeCwd=A, pool 含 (claude,A) | 用户发送 /use codex | activeAgent=codex, lookup (codex, A) → spawn 新 (codex,A) | Gateway.handler.use |
| ChatSession, activeCwd=A, active=(claude,A) | 用户发送 /use claude (同一) | noop (已在用) | Gateway.handler.use |
| ChatSession, activeCwd=A, active=(claude,A) | 用户发送 /cwd B | activeCwd=B; (claude,A) 仍在 pool; active=(claude,B) → spawn 新 (claude,B) | Gateway.handler.cwd |
| ChatSession, activeCwd=A | 用户发送 /kill | 清空 pool; activeAgentSession=nil; 老 receipts dispose | Gateway.handler.kill |
| AgentSession.Running | CLI exit / EOF | AgentSession.Exited（仍在 pool） | AgentSession.readPump |
| AgentSession.Running | nightme SIGTERM (default) | AgentSession.Detached | cmd/nightme shutdownRun |
| AgentSession.Running | nightme SIGTERM --cleanup | AgentSession.Exited (Kill) | cmd/nightme shutdownRun |
| AgentSession.Detached | nightme 下次启动 | AgentSession.Detached (恢复) | Registry.Restore |
| AgentSession.Detached | 用户 /use (同 agent, 同 cwd) | spawn 新进程 (复用 entry 但新 pid) | Gateway.handler.use |

> **生命周期详细策略**（含进程归属、清理、reattach）：见 [`feat/F-06-process-cleanup.md`](./feat/F-06-process-cleanup.md)

---

## 4. 并发模型

nightme 用 Go 的 goroutine 实现并发，结构如下：

- **Main goroutine**：信号处理、组件启动顺序、优雅退出
- **Channel goroutines**：每个 Channel adapter 一组（WebSocket 收发 + sendLoop）
- **Gateway pumpInbound goroutines**：per-channel
- **Gateway dispatchLoop goroutine**：单 consumer，从 channelCh 读 → Handle
- **Gateway sweepSessions goroutine**：5s ticker，检测新 ChatSession / AgentSession
- **Per-AgentSession readPump goroutine**：AgentSession 内部，单消费者消费 `as.Events()`
- **ChatSession EventCallback（同步）**：从 readPump 调用，UpdateReceipt + Translate + ch.Send
- **ChatSession 在 /use 时切换 EventCallback 目标**（老 AgentSession 的 readPump 仍在跑，事件被丢弃）

**v1.2 新增并发约束**：
- **每个 ChatSession 维护 `activeAgentSession` 指针**（原子读 / mutex 写）
- **`/use` 切换时**：先原子清空 activeAgentSession → 等 in-flight EventCallback 完成 → 重设到新 AgentSession → 启动新 AgentSession 的 readPump（如果新 spawn）
- **老 AgentSession 的 readPump 不主动停**：继续跑，但 EventCallback 是 noop（因为不再 active）

**并发安全**：
- Gateway.bindings / receipts：mutex 保护
- ChatSession.activeCwd / activeAgent / pool：mutex 保护
- 每个 AgentSession 内部用 buffered channel 通信，无跨 session 共享状态
- 无全局锁、无 errgroup、无 singleflight

**Back-pressure**：Channel 发送慢 → EventCallback 阻塞 → readPump 阻塞 → `as.Events()` chan buffer 满 → Bridge 阻塞 → CLI 自己阻塞。

> **实现细节**：见 [`feat/F-19-cli-bridge.md`](./feat/F-19-cli-bridge.md) + [`feat/F-26-gateway-hub.md`](./feat/F-26-gateway-hub.md) + [`feat/F-27-chatsession.md`](./feat/F-27-chatsession.md) + [`feat/F-29-agent-session-pool.md`](./feat/F-29-agent-session-pool.md)

---

## 5. 技术栈（已锁定）

| 层 | 选型 | 备选 | 理由 |
|----|------|------|------|
| 主语言 | **Go 1.22+** | Rust / Node.js | 单二进制、跨平台编译简单 |
| PTY | **`github.com/aymanbagabas/go-pty`** | creack/pty | API 干净，跨平台抽象好 |
| Channel | **飞书官方 Go SDK**（lark-oapi）| 自实现 webhook | 文档全，长连接稳定 |
| HTTP API | **net/http + chi** | gin | minimal |
| 持久化 | **JSON 文件**（registry ChatSession + AgentSession 两类 entry）| SQLite | MVP 不需要 DB |
| 配置 | **YAML** | env | 直观 |
| 日志 | **`log/slog`**（标准库）| zap / zerolog | stdlib 够用 |

**v1.2 持久化 schema（已落地）**：

```jsonc
{
  "chat_sessions": {
    "<chatSessionId>": {
      "chatId":               "oc_xxx",          // UNIQUE 索引 (1 chat = 1 ChatSession); nightme 不持有 chat 类型
      "activeCwd":            "/code/bailing",
      "activeAgent":          "claude",
      "primaryAgent":         "claude",          // snapshot of cfg.Primary at creation; read-only
      "agentSessionIds":      ["as_1", "as_2"],
      "activeAgentSessionId": "as_1",            // 引用 pool 中某项; null 表示未激活
      "createdAt":            "...",
      "lastInteractionAt":    "..."
    }
  },
  "agent_sessions": {
    "<agentSessionId>": {
      "chatSessionId":        "<chatSessionId>", // FK, 标识属于哪个 ChatSession 的 pool
      "agent":                "claude",          // IMMUTABLE
      "cwd":                  "/code/bailing",   // IMMUTABLE
      "pid":                  12345,
      "status":               "running | detached | exited",
      "createdAt":            "...",
      "lastRunAt":            "..."
    }
  }
}
```

**唯一约束**：
- `chat_sessions.chatId` UNIQUE（保证 1 chat = 1 ChatSession）
- `agent_sessions.(chatSessionId, agent, cwd)` UNIQUE（保证 pool 内 (agent, cwd) 1:1；不同 ChatSession 各自独立）

**Config schema (config.yaml) v1.2 (2026-08-02)**：

```yaml
primary: cc                    # top-level: global default agent
agents:                        # top-level: list of available agents
  - name: cc                  # user-defined name (overrides builtin of same name)
    bridge: claude             # bridge backend type
    command: "claude --dangerously-skip-permissions"  # full command line (binary + args)
  - name: claude              # builtin claude override (custom binary path)
    bridge: claude
    command: /custom/path/claude
```

User-configured `agents:` entries override built-ins of the same name (merge happens at runtime, not parse time).

**Q-A 锁定 (2026-08-02)**：Primary Agent 仅全局 config（YAML `primary`）；ChatSession.primaryAgent 是创建时的 snapshot，不可变。**无 `/default` 命令**。**`nightme config` 交互模式**用于选择 primary（见 F-30）。

```yaml
# 旧的 v1.x schema (仅用于历史参考, v1.2 已废弃)
# agent:
#   default: claude
#   agents:
#     claude:
#       command: claude
#       args: []
#       env: {}
```

---

## 6. 配置

[v1.1 不变，新增 `primary` 字段承载 Primary Agent]

---

## 7. 非功能需求 (NFR)

[v1.1 不变]

新增：
- **N-8 (v1.2)**：ChatSession active AgentSession 切换延迟 ≤ 100ms（不含 spawn 新进程）

---

## 8. 安全（高层）

[v1.1 不变]

---

## 9. 已锁定的技术决策（v1.2 更新）

| # | 决策 | 结论 |
|---|------|------|
| **Q1** | 技术栈 | **Go 1.22+** |
| **Q2** | MVP Channel | **只飞书** + Channel interface 抽象 |
| **Q3** | MVP Agent | **只 Claude Code** + Agent interface 抽象 |
| **Q4** | Session 路由 | **Chat ↔ ChatSession 1:1**（Gateway 持有 binding 表）|
| **Q5** | CLI spawn 方式 | **自己 PTY**（aymanbagabas/go-pty）|
| **Q6** | 鉴权 | **单用户独占假设**，不需要设备配对 |
| **Q7** | Channel ↔ Session 通信 | **单向经过 Gateway**，Channel 与 Session 互不引用 |
| **Q8** | Receipt 状态机 owner | **Gateway**；Channel 只渲染 |
| **Q9** | `/cwd` 语义 | **只改 activeCwd**，不触发 spawn / kill |
| **Q10** | `/use` 语义 | **永不重启进程**；复用 pool 中现有 AgentSession，没有再 spawn |
| **Q11** | `/kill` 语义 | **清空 ChatSession 整个 AgentSession 池**，下次消息触发 spawn 新 |
| **Q12** | InputBuffer FSM owner | **ChatSession**（per ChatSession, 跨 `/use` 切换共享 queue）|
| **Q13** | AgentSession 唯一性 | **`(agent, cwd)` per ChatSession 唯一**；不同 ChatSession 可独立 |
| **Q14** | `session.Events()` 单消费者 | **readPump only**；ChatSession 通过 EventCallback 接收 |
| **Q15** | `/cwd` / `/use` 对 AgentSession 的影响 | **不杀任何 AgentSession**；pool 保留老 entry，切回能复用 |

**已确认（2026-08-03，F-watch 锁定）**：
- **Q-W1** ✅ 新增 `/watch on|off` slash command 控制 per-chat `WatchMode`：`WatchModeMention`（默认，只 @ 收）/ `WatchModeAll`（全收）；由 Gateway dispatcher 在 `Handle` 入口 gate
- **Q-W2** ✅ `Message.HasMention` 由 channel adapter 计算（DM 永远 true；group 看 `Mentions` + bot open_id）；Gateway 不重复计算
- **Q-W3** ✅ Feishu `DefaultAddons()` **始终包含** `im:message.group_msg`（不带 `:readonly`）：bot 默认接收全群消息，由 `WatchMode` 在 nightme 侧 gate。**不**走 CLI flag opt-in
- **Q-W4** ✅ Channel adapter 在构造 `Message` 前 strip 开头 `@bot_key ` / `@_all ` mention 前缀（还原为 nightme 支持的纯文本格式，让 `/watch on` 能被 `ParseCommand` 正确解析）

**已确认（2026-08-02，PRD v1.2 锁定）**：
- **Q-A** ✅ Primary Agent 仅全局 config（顶层 `primary`，与 `agents:` list 并列）；ChatSession.primaryAgent 是创建时 snapshot，不可变。**无 `/default` 命令**。
- **Q-B** ✅ LookupActiveAgentSession 只看 `(activeAgent, activeCwd)`：命中 Running 复用，否则 spawn `(activeAgent, activeCwd)`。**没有运行时 fallback**：ChatSession 始终持有一个有效的 activeAgent（创建/恢复时被 `cfg.Primary` 一次性填入），用户用 `/use` 显式覆盖，lookup 不再做降级判断。

**Q-A 锁定补充**：config schema 顶层 `primary` + `agents` list（`nightme config` 交互菜单生成）。

---

## 10. 与现有项目的关系

[v1.1 不变]

---

## 11. 下一步

技术规范已落地（commits 5/6/7/8a/8b/8c/9 + 后续 fix，`fix/cwd_session` 分支）：

- ✅ ChatSession / AgentSession 数据结构 + I/O（commits 5/6）
- ✅ Spawner 抽象 + AgentSession 真实 fork-exec（commit 7）
- ✅ Manager + `/cwd` `/use` `/kill` handlers（commit 8a）
- ✅ v1.2 daemon 切换到 `chatsession.Manager`（commits 8b/8c）
- ✅ InputBuffer FSM ownership 移到 ChatSession（commit 9）
- ✅ Config schema `primary` + `agents` list + `nightme config` 交互模式（F-30）
- ✅ FlushHook 默认转发到 active AgentSession（commit `4119e2c`）
- ✅ Command names 不带前导斜杠（commit `d54a4c1`）
- ✅ User / ops docs：README / CHANGELOG / MIGRATION（commits `2ccc443` / `dc75493`）

剩余（backlog）：
- ⏭ 真实 E2E 飞书 DM round-trip test
- ⏭ 删除 `internal/session/` v1.1 MemoryManager（仍被 `internal/gateway/cmd/handlers.go` BindingEntry shim 使用）
- ⏭ **v1.3 SPEC 落地代码改动**：
  - ✅ done: `internal/receipt/` 包删除;`MessageState` 移到 `internal/agent/`
  - `internal/channel/channel.go` 删 `CreateReceipt / UpdateReceipt / DisposeReceipt` 三个方法
  - `internal/gateway/gateway.go` 删 `receipts` map + `CreateReceipt / UpdateReceipt / DisposeReceipt` 方法 + 死代码 `translateAndSend` / `receiptsForSession`
  - `internal/channel/feishu/adapter.go` `Send` 路由改 userMsgID-driven（`msg.ReplyTo` 查 `receiptsByUserMsgID`，miss 时 cold-create）
  - `internal/chatsession/chatsession.go` `currentTurnUserMsgIDs []string` → `currentTurnUserMsgID string`
  - `internal/chatsession/readpump.go` `emitMessageStateForCurrentTurn` 改用单一 ID
  - `cmd/nightme/run.go` `newEventHandler` 设 `out.ReplyTo = cs.currentTurnUserMsgID`
- ✅ done in v1.3: docs/feat/F-25-rolling-log.md renamed from F-25-input-buffer.md; F-26-gateway-hub.md + F-08-channel-abstraction.md + F-31-message-state.md + docs/channel/feishu.md updated with v1.3 annotations
- ⏭ **F-33（chatID 数据模型简化）**：删 Gateway ChatType 抽象 + topic_group 不特殊处理 + InboundMessage.ReplyTo = message.ParentId。详见 [`docs/feat/F-33-simplify-chatid-data-model.md`](./feat/F-33-simplify-chatid-data-model.md)
- ⏭ **F-34（`/new` slash command）**：不退进程重置 agent 对话上下文；`/new` 清 activeCwd 下 pool 全部 AS；`/new <agent>` 清指定 AS；清 InputBuffer。详见 [`docs/feat/F-34-new-slash-command.md`](./feat/F-34-new-slash-command.md)
- ⏭ **F-watch（WatchMode per-chat 群消息全收 + mention strip）**：
  - `internal/channel/channel.go` `Message.HasMention bool` 字段 + 接口扩展
  - `internal/channel/feishu/adapter.go::handleMessage` 加 mention strip + `HasMention` 计算
  - `internal/chatsession/chat_session.go` `WatchMode` 类型 + getter/setter
  - `internal/chatsession/registry.go` `ChatSessionEntry.WatchMode` 字段
  - `internal/gateway/handlers_watch.go` 新文件：`handleWatch` + `/watch` 注册
  - `internal/gateway/gateway.go::Handle` 入口加 `HasMention` gate
  - `internal/auth/feishu/feishu.go::DefaultAddons` 加 `im:message.group_msg`
  - `internal/auth/feishu/feishu_test.go` 加 case
  - `cmd/nightme/auth_login.go` 移除 `--group-messages` flag 设计（默认开启）
  - 详纸面设计见 [`docs/SPEC.md`](./SPEC.md) §3.1.1 + [`docs/feat/F-08-channel-abstraction.md`](./feat/F-08-channel-abstraction.md)
- ⏭ **F-thread-route（OutThinking/Tool → Feishu thread + 类型感知摘要）**：
  - 反转 v1.3 §13.6/§13.7/§13.9 折叠决议（collapsible_panel 实机验证失败）
  - `internal/agent/agent.go` `ToolEndEvent` 加 `Args string` 字段
  - `internal/bridge/claudecode/stream.go` 解析 `tool_result` 时从同 message 的 `tool_use` block 拿 args 填进 `ToolEndEvent.Args`
  - `internal/channel/feishu/adapter.go` `Send` dispatcher 按 Kind 分流：thinking/tool/compaction → thread；text/result/init/usage → receipt card
  - `internal/channel/feishu/summarize_tool.go` 新文件：`summarizeToolEnd` + `countLines` + `truncate` helper
  - `internal/channel/feishu/receipt_event.go` `eventToEntry` 对 thinking/tool/compaction 返回 `(_, false)`（不进 receipt）
  - `internal/channel/feishu/adapter.go` `buildReceiptCard` 删 collapsible_panel 分支（`Kind=="thinking"` / `Kind=="tool"`）
  - `internal/channel/feishu/receipt_event_test.go` 删 thinking/tool assertion；新增 `TestSend_Out*_PostsToThread` + `TestSummarizeToolEnd`
  - `docs/channel/feishu.md` §13.12 决策反转记录 + §15 实施计划修订
  - 详见 [`docs/feat/F-37-tool-thread-routing.md`](./feat/F-37-tool-thread-routing.md) + [`docs/channel/feishu.md`](./channel/feishu.md) §13.12
- ⏭ **F-35（feishu 全局限速器）**：`internal/channel/feishu/ratelimit.go` 单桶 token bucket(5 QPS / burst 1 / lazy refill)，4 个底出口(`sendViaLarkCreate` / `sendViaLarkReply` / `updateViaLark` / `AddReaction`)SDK call 前 `Wait()`。`internal/config/config.go::FeishuConfig` 加 `RateLimit` 字段。详见 [`docs/feat/F-35-ratelimit.md`](./feat/F-35-ratelimit.md) + [`docs/channel/feishu.md`](./channel/feishu.md) §16。
- ⏭ **F-36（feishu transient retry + 降级日志）**：`internal/channel/feishu/retry.go` 指数退避重试(3 次尝试 / 500ms→5s / ±25% jitter)，包裹 `sendContent` / `updateViaLark` / `AddReaction`。所有降级路径(retry exhausted / ctx cancel / fallback top-level)emit warn 级结构化日志。详见 [`docs/feat/F-36-transient-retry.md`](./feat/F-36-transient-retry.md) + [`docs/channel/feishu.md`](./channel/feishu.md) §17。
- ⏭ **F-37（receipt 多 div 拆分）**：`internal/channel/feishu/receipt_split.go` `splitMarkdownForDivs` 把单 entry 内容按段落/语义边界拆成多个 `div` 元素，每 div ≤ 1000 chars（Feishu `div` text 硬限），绕过 600 B 截断 backlog，保留 `lark_md` 渲染。`buildReceiptCard` 多 div 路径、`totalLogBytesLocked` 估算修正、`perEntryMaxRunes = 8000`。详见 [`docs/feat/F-37-multi-div-content-split.md`](./feat/F-37-multi-div-content-split.md)。**resolve SPEC §13.3 `OutResult` 600 字节截断 backlog**。
- 🚧 **F-38（Claude task checklist）**：Claude bridge 在 `TaskCreate` / `TaskUpdate` 的匹配 `tool_result` 确认成功后维护 task map/order，发完整 typed snapshot；Gateway 新增 `OutTaskCreate` / `OutTaskUpdate` 无状态透传；Feishu receipt 用单 markdown element 原位 PATCH 任务清单。详见 [`docs/feat/F-38-task-checklist.md`](./feat/F-38-task-checklist.md) + [`docs/channel/feishu.md`](./channel/feishu.md) §13.14 / §18。
- ✅ done: **F-38** 落地（docs-first；2026-08-04）。`internal/agent` + `internal/gateway` typed contract、`internal/bridge/claudecode/task.go` 解析、`internal/channel/feishu/receipt_task.go` 单 markdown element 渲染。`go test -race ./...` 全绿。
---

## 11.1 Daemon startup flow

`nightme run` 启动顺序（见 `cmd/nightme/run.go`）：

```
1. loadConfig()                       # ~/.config/nightme/config.yaml
2. openChatSessions() / openAgentSessions()   # chat_sessions.json / agent_sessions.json
3. buildAgents(cfg)                   # cfg.Agents → agent.Registry
4. MigrateV1ToV2(v1RegistryPath)     # 备份 registry.json → .v1.bak (idempotent)
5. spawner := NewRegistrySpawner(agents)
6. mgr := NewManager().WithSpawner(spawner).WithPersistence(csFile, asFile)
7. mgr.RestoreFromRegistry()          # 重建内存中 ChatSession 池 (AgentSession 状态=Detached)
8. ch.Start()                         # Feishu WebSocket / echo channel
9. gw := gateway.New(newMessageDispatcher(mgr, ch, cfg.Primary), nil)
10. RegisterChatSessionCommands(gw, mgr, ch, cfg.Primary)
11. for each cs in mgr.List(): cs.SetEventHandler(newEventHandler(ch, logger))
12. gwImpl.AttachChannels(ch) + gwImpl.Start()
13. block on signal / ctx.Done()
14. shutdownRun: stop channel + (cleanup? KillAll : detach)
```

**关键不变量**：
- Step 11 的 `SetEventHandler` 在所有 ChatSession 上一次性安装；handler 跨 `/use` 持久。
- Step 4-7 在没有 v1.x 数据时无操作（idempotent）。
- Step 7 后所有 AgentSession 是 `Detached`（无进程）。用户第一次发消息 → `LookupActiveAgentSession` → Spawner spawn。

---

## 11.2 Restart semantics

Daemon 重启后（用户发送 SIGINT 然后再启 `nightme run`）：

| 数据 | 行为 |
|---|---|
| `cfg.Primary` | 重新读取配置。**不影响已存在的 ChatSession.primaryAgent**（Q-A snapshot）。 |
| `chat_sessions.json` | 全量恢复为 in-memory ChatSession。`activeCwd`、`activeAgent`、`primaryAgent` 复原。 |
| `agent_sessions.json` | 恢复为 in-memory AgentSession，**全部 `Status=Detached`，PID=0**。 |
| v1.x `registry.json` | 备份为 `.v1.bak`，不恢复数据（见 MIGRATION.md）。 |

**用户感知**：
- 第一次发消息会卡 ~100ms-2s（Spawner 重新 fork）。后续消息即时。
- 已 `/cwd` 但从未 `/use` 的 chat：发消息时 Spawner 触发 `/use` 等价的 lazy spawn（不是 `/use` 显式命令）。
- 显式 `/use` 过的 chat：第一次发消息触发 lazy spawn（因为没有运行中的进程）。

---

## 12. 文档层级

[v1.1 不变]