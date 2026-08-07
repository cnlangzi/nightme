# F-52 — Pi Bridge 流式事件整合

> 状态：✅ 已实现
> 版本：v1.3.x
> 前置：[F-32 Pi RPC Bridge](./F-32-pi-rpc-bridge.md)（§2.3 Event 表被本文修订）
> 相关：[F-40](./F-40-outreply-overflow.md)（OutReply 语义）、[F-44](./F-44-outreply-independent-and-task-receipt.md)（rolling log）、[F-49](./F-49-compaction-counter.md)（footer token 语义）

## 0. 问题

### 0.1 现象

Pi 回一句 `Hello! How can I help you today? I'm your coding assistant...` 时，飞书群里出现 **20 条独立的 💬 气泡**：

```
💬 Hello
💬 !
💬 How can
💬 I help
💬 you today
💬 ? I
💬 'm your
💬 coding assistant
...
```

### 0.2 直接原因

Pi 以 **token 粒度**推送 `message_update{assistantMessageEvent:{type:"text_delta"}}`。改动前 `internal/bridge/pi/translate.go` 把每个 delta 直接翻成一个 `agent.EventText`：

```
text_delta ──> EventText ──> gateway.Translate ──> OutReply ──> receipt.AppendEntry ──> 一条 💬
```

链路上没有任何一层做聚合，所以 token 数 = 气泡数 = 飞书卡片 PATCH 次数（限流风险）。

### 0.3 根本原因：`EventText` 的契约从未被定义

四个 bridge 对「一个 `EventText` 是什么」理解完全不同：

| bridge | 一个 EventText | 是否符合下游预期 |
|---|---|---|
| claudecode | 一个完整 content block（`stream.go:257` 遍历 `Message.Content` 逐块 emit） | ✅ |
| pi | 一个 token | ❌ |
| acp | 一个 chunk | ❌（未在生产暴露） |
| pty | 一坨裸字节 | N/A（raw 语义） |

而 `gateway.Translate` 把 `EventText` 映射成 `OutReply`，飞书 adapter 的 `OutReply` 语义是**追加一条日志条目**。这个语义只对第一种粒度成立。claudecode 碰巧对了，所以问题只在 pi 上暴露。

### 0.4 两个被掩盖的静默故障

调查过程中发现两个从未被报告的问题，根因同一处：

**(1) pi 永远不产生 OutResult。**

```go
// 改动前 translate.go:325
case "text_end":
    t.sawTextEnd = true      // ← 置位

// 改动前 translate.go:434
if !t.sawTextEnd {           // ← 据此把 finalText 置空
    for _, b := range msg.Content { ... }
}
```

流式发生过 → `finalText == ""` → `ResultEvent.Text == ""` → `internal/gateway/translate.go` 的 `Text=="" && !IsError` 判定返回 `ok=false` → **整个 OutResult 被丢弃**。用户从来没见过 pi 的 📝 最终卡片。

**(2) usage / cost 跟着一起丢。**

`cmd/nightme/run.go` 的顺序是：

```go
out, ok := gateway.Translate(chatID, ev)
if !ok { return }              // ← 这里就返回了
...
if out.Usage != nil { s.AccumulateUsage(out.Usage) }   // ← 到不了
```

pi 的 usage 挂在同一个被丢弃的 `ResultEvent` 上，所以 **pi 每一轮的 token 和费用都没进 `CumulativeUsage`**，footer 的 💰 行对 pi 恒为空。

## 1. 生态调研

对照三个同类实现（cc-connect、openclaw、openclaw-lark），结论出人意料：

### 1.1 它们也逐 token 产生内部事件

cc-connect `agent/pi/session.go:778`：

```go
case "text_delta":
    evt := core.Event{Type: core.EventText, Content: delta}   // 逐 token，不聚合
```

**差别全在下游**：cc-connect 在 `core/engine.go:4683 processInteractiveEvents` 用 turn 作用域的 `textParts` / `partialText` 累加，只在 `EventResult` 时 join 后发一次消息。openclaw 在 reply pipeline 累加（openclaw-lark 插件收到的 `onPartialReply` 已经是累积全文）。

**没有任何一个把 delta 直接当成一条消息。** 我们是唯一既不在传输层聚合、消费端也没有累加器的。

### 1.2 openclaw 跑 pi 用的是同一个协议

```
openclaw ──ACP(stdio JSON-RPC 2.0)──> npx pi-acp@^0.0.31 ──> pi --mode rpc --no-themes
```

`pi-acp` 是个独立适配器包，内部 spawn 的正是 `pi --mode rpc`——跟 F-32 选的同一个协议。**传输层选型是对的**，问题纯粹在事件整合层。ACP 那层是为了「一套代码接 N 个 harness」，不解决数据整合。

### 1.3 空回复兜底是共识，我们缺失

- cc-connect：`fullResponse = e.i18n.T(MsgEmptyResponse)`
- openclaw-lark：`EMPTY_REPLY_FALLBACK_TEXT = 'Done.'`
- nightme：静默丢弃 ← 就是 §0.4(1)

## 2. 设计

### 2.1 目标

**一轮对话 = 一次 OutResult**，同时保留带工具的长任务里的中途进度感。

### 2.2 事件映射

| pi wire 事件 | 改动前 | 改动后 |
|---|---|---|
| `text_start` | 丢 | 重置 `turn.textBuf[contentIndex]` |
| `text_delta` | **EventText × N** | 累加进 `turn.textBuf[contentIndex]`，不 emit |
| `text_end` | 只置 `sawTextEnd` | 该块转入 `turn.pendingText`，不 emit |
| `thinking_start` | 丢 | 重置 `turn.thinkBuf` |
| `thinking_delta` | **EventText × N** | 累加进 `turn.thinkBuf`，不 emit |
| `thinking_end` | 丢 | **`EventText{"[思考] " + 全文}`** |
| `tool_execution_start` | EventToolStart | **先 flush `pendingText`（非空则 EventText）**，再 EventToolStart |
| `tool_execution_end` | EventToolEnd | 不变 |
| `message_end(assistant)` | EventResult（文本被抑制→整个被丢） | 记录 `lastMessageText` / `stopReason` / `lastUsage`，不 emit |
| `message_end(toolResult)` | no-op | 不变 |
| `agent_settled` | EventDone | **EventResult → EventDone**，然后整个 turn 状态归零 |

其余事件（`agent_start` / `agent_end` / `turn_*` / `compaction_*` / `state_update` / 工具 orphan 路径）行为不变。

### 2.3 关键决策：flush 点选在 `tool_execution_start`

这是整个设计的支点。它让「一轮一次 OutResult」和「中途要能看到进度」两个目标不再冲突：

- **无工具的简单回答** → 0 个 EventText，1 个 EventResult。
- **有工具的复杂回答** → 工具前的每段叙述各出一条 💬，最后的结论出一条 📝。**零重复**——因为每次 flush 都清空 `pendingText`，EventResult 拿到的必然是最后一段。

考虑过但否决的替代方案：

| 方案 | 否决理由 |
|---|---|
| 在 `message_end(assistant)` flush | 那条 message 可能就是最终答案，flush 了会跟 EventResult 重复 |
| 全程不 flush，只在 settled 发 EventResult | 带工具的长任务里，agent 的中途叙述全憋到最后，IM 场景看不到进度 |
| 保留 per-block EventText + EventResult（对齐 claudecode 现状） | 最后一段必然重复出现两次（💬 + 📝） |

思考流单独在 `thinking_end` flush，不进 `pendingText`：它是另一个渲染面（💭 vs 💬），且绝不能落进 EventResult。

### 2.4 `EventResult.Text` 取值优先级

1. `turn.pendingText` — 我们累积的、用户**尚未看到**的那一段。正常路径。
2. `turn.lastMessageText` — `message_end.content[]` 拼接，pi 的权威版本。**仅当 1 为空且本轮尚未投递过任何正文**时使用，覆盖 pi 未走流式（重放）的场景。
3. `emptyReplyFallback = "Done."` — 兜底常量。

第 3 条**不是装饰**。`gateway.Translate` 会丢弃 `Text==""` 且 `IsError==false` 的 EventResult，而 runtime 是从**翻译后**的 OutboundMessage 上读 Usage 的——空文本会把这一轮的 token 数一起带走（§0.4(2)）。本次不动共享层，所以由 bridge 侧保证 Text 非空，让 usage 100% 通过。

#### 2.4.1 `textDelivered` 守卫（review 阶段发现的缺陷）

第 2 条的「尚未投递过任何正文」限定是**必需**的，第一版实现漏了它，导致「零重复」承诺在一类真实序列上不成立。

pi 的事件顺序是 `message_end(assistant, content=[text, toolCall])` **先于** `tool_execution_start`。于是一轮以工具调用结尾时：

```
text_delta "Let me check."          → pendingText = "Let me check."
message_end(assistant,[text,tool])  → lastMessageText = "Let me check."
tool_execution_start                → flush → EventText{"Let me check."}, pendingText = ""
agent_settled                       → pendingText 空 → 回退 lastMessageText
                                    → EventResult{"Let me check."}   ← 重复!
```

用户会先在 rolling log 看到 💬「Let me check.」，再在最终卡片看到 📝「Let me check.」——正是 F-52 要消除的重复，只是换了个形状，第一轮测试没覆盖到。

修法是给 turnState 加 `textDelivered bool`：只有**真正投递过** EventText（空 flush 不算）才置位，置位后禁用 lastMessageText 回退。用布尔标志而非「flush 时清空 lastMessageText」，是因为两个事件的到达顺序在不同 pi 版本下可能相反，标志与顺序无关而清空操作不是。

回归锁：`TestTranslate_ToolEndingTurn_NoDuplicate`（重复必须消失）+ `TestTranslate_NonStreamedTurnUsesMessageFallback`（空 flush 不能误伤回退）。

#### 2.4.2 `usage` 解码门（code-review 阶段发现的缺陷）

「usage 100% 送达」的承诺还有第二个漏点，跟 §2.4.1 独立：`decodeMessageUsage` 原来用**聚合字段** `totalTokens` 当「本条消息没有 usage」的判据：

```go
if u.Total == 0 && (u.Cost == nil || u.Cost.Total == 0) { return nil }
```

但 `totalTokens` 在 pi 的 wire 上跟 breakdown 是**分开上报**的，完全可能出现 `totalTokens=0` 而 `input`/`output` 非零（合成消息、schema 变体）。这种情况下整块 usage 被静默丢弃——正是 §0.4(2) 那个 bug 的另一条路径。

改为逐字段对称检查（全零且 cost 为零才判定为空），与 `claudecode/stream.go:decodeUsage` 的做法一致——那边本来就是对称的，pi 这个 `Total` 门是唯一的例外。

回归锁：`TestTranslate_UsageSurvivesZeroTotalTokens`（`totalTokens=0` 但 breakdown 非零必须保留）+ `TestTranslate_UsageDropsAllZeroIncludingZeroCost`（全零仍须丢弃，避免 footer 渲染 "$0.00"）。

#### 2.4.3 `active` 守卫：空 settle 不出卡片

`agent_settled` 的语义是「整个 session-level run 结算」（pi `docs/rpc.md`），它也用于结算 out-of-band 路径（例如 fire-and-forget 的压缩）。若这类 settle 落在一个什么都没观察到的 turn 上，无条件发 EventResult 会往用户会话里塞一张莫名其妙的「Done.」卡片。

`turnState.active` 在正文 delta / 思考 delta / assistant message_end / tool start 任一发生时置位；`finishTurnLocked` 在未置位时直接返回 nil，只留 EventDone。

回归锁：`TestTranslate_UntouchedSettleEmitsNoResult`。

#### 2.4.4 `/new` 抑制窗口（code-review PLAUSIBLE，评估后确认为真并修复）

code-review 把这条标为 PLAUSIBLE 并跳过（理由：`session.go` 注释里写明是有意的锁策略权衡）。**复评后判定为真 bug**——注释解释的是「为什么不用 translatorMu 包住 translate()」，那个权衡确实成立；但它没有覆盖「重置之后、EventInit 之前」这段时间里到达的事件。

`session.New()` 的时间线：

```
new_session 响应到达
  ↓
turnState 重置                     ← 只清掉「已经累积的」
  ↓
get_state RPC（10s 超时）          ← readPump 全程在跑!
  ↓
deliverInitLocked → EventInit
```

而 `/new` **可以打断进行中的 turn**：`NewActiveAgentSessions`（`chatsession.go:1233`）没有任何 Busy/Idle 守卫，slash command 也不走 InputBuffer 排队。所以用户在长回复中途打 `/new` 时，旧 turn 仍在管道里的事件会落进**全新的** turnState：

- 旧 `message_end` 把 usage 盖到新会话上——直接污染「上下文占用」数字，还跟 `handleNew` 紧随其后的 `ResetCumulative()` 抢先后；
- 旧 `agent_settled` 把**已被放弃的回复**当作新会话的 result 卡片发出去。

修法：`beginReset()` / `endReset()` 取代原来的 `resetTurn()`。`beginReset` 在重置 turnState 的同时打开抑制窗口，`translate()` 在窗口内直接丢弃所有事件。

窗口内丢弃是**无条件正确**的：新会话还没收到任何 prompt，此刻到达的东西不可能属于它。命令响应走 response 分支，`extension_ui_request` 的自动 cancel 和畸形帧的 EventError 都在 `readPump` 里、在 `translate()` **之前**处理（`session.go:812-817`），三者都不受影响。

`endReset` 用 `defer` 挂在 `New()` 上，确保任何错误返回路径都不会把 session 永久静音。

回归锁：`TestTranslate_ResetWindowDropsAbandonedTurn`（窗口内的 text/usage/tool/settled 全丢，且新 turn 的 usage 是 42 而非旧的 9999）+ `TestTranslate_EndResetRestoresTranslation`（静音必须能恢复）。

> 顺带修正：F-52 初版在 `session.go` 里写的「一次性丢弃 turnState，所以 /new 落在 turn 中间不会把半截回复漏进新会话」是**过度声称**——它只在重置那一瞬成立。注释已改。

### 2.5 usage 取最后一条快照，不累加

一轮里有多条 assistant message，每条带自己的 usage。**覆盖，不求和。**

理由：每次 Pi API call 报告的 input 侧（`input + cacheRead + cacheWrite`）**本身就已包含全部对话历史**，是当前上下文占用的**快照**。把一个多次调用的 turn 的快照相加，会按调用次数成倍虚高。

cc-connect 从另一个方向得出同样结论——它的 `handleAgentEnd` 倒序遍历 `agent_end.messages[]`，取最后一条 assistant 的 usage 作为 ContextUsage。

> **已知遗留（下个 PR）**：共享层 `AgentSession.AccumulateUsage`（`internal/chatsession/agentsession.go`）目前仍是**跨轮累加**。F-49 §1.2 已把 4 个 token 字段的语义定为「当前上下文占用」（compaction 时归零、`CostUSD` 保留），但累加"最后一次调用的快照"在语义上是错的——跑 N 轮就虚高 N 倍，compaction 归零只是把问题推后。正确做法是**覆盖**。
>
> 这个缺陷同样存在于 claudecode（`stream.go` 的 `decodeUsage` 取的也是最后一次调用的快照），所以修复应在共享层统一做，连带把 F-49 §6 列为 out-of-scope 的 `model → contextWindow` 分母补上（pi 的 `Model` 对象有 `contextWindow: 200000`，我们现在在 `getStateModel` 里丢掉了；它还有 `get_session_stats` 命令直接返回 `contextUsage.{tokens,contextWindow,percent}`）。
>
> 本 PR 范围内 pi 侧产出的已是正确的单点快照，共享层改成覆盖后立刻生效。

### 2.6 并发

所有 per-turn 可变状态收进一个 `turnState`：

```go
type turnState struct {
    textBuf         map[int]*strings.Builder
    thinkBuf        strings.Builder
    pendingText     string
    lastMessageText string
    stopReason      string
    lastUsage       *agent.UsageEvent
    pendingTools    map[string]pendingTool
}
```

`translate()` 跑在 readPump goroutine；`session.New()`（`/new`）从另一个 goroutine 重置。这与改动前 `pendingTools` 的情况完全一样，所以：

- 把 `pendingTools` 一并收进 `turnState`，用一把 `turnMu` 统一保护（原 `pendingMu` 改名）。
- `session.New()` 里原本单独重置 `pendingTools` 的三行改成一次 `translator.resetTurn()`。
- `translate()` 整个函数体在 `turnMu` 下跑——安全，因为翻译是纯 CPU（无 I/O、无 channel send、不回调 session），临界区被一次 JSON decode 界定；投递发生在 `translate` 返回**之后**的 readPump 里。

`initSent` 保持在 `translatorMu` 下不变。两把锁保护不相交的状态、从不同时持有（`New()` 是顺序获取而非嵌套），无锁序风险。这个不变式要保持——把 `translatorMu` 套在 `translate()` 外面会让每个 wire 事件都跟 `/new` 串行化。

分组的附带收益：重置一个 turn 现在是一次赋值，将来新增 per-turn 字段不可能被某条重置路径漏掉。

### 2.7 附带收益：丢事件风险下降

`session.go` 的 `deliver()` 在 events channel 满时最多阻塞 1 秒然后**静默丢弃**。改动后单轮事件数从 O(token) 降到 O(工具次数)，丢事件的概率大幅下降。

## 3. 实现

| 文件 | 改动 |
|---|---|
| `internal/bridge/pi/translate.go` | 核心。新增 `turnState` + `resetTurn` / `closeTextBlockLocked` / `closeOpenBlocksLocked` / `flushPendingTextLocked` / `finishTurnLocked` / `recordAssistantMessageLocked` / `decodeMessageUsage`；删除 `sawTextEnd` |
| `internal/bridge/pi/session.go` | `New()` 的 `pendingTools` 重置改为 `s.translator.resetTurn()` |
| `internal/bridge/pi/translate_test.go` | 改 5 个既有测试 + 新增 12 个 |
| `internal/agent/agent.go` | 仅注释：`EventText` 补上粒度契约 |

### 3.1 边界防御

- **漏掉的 `text_end`**（abort / error 路径）：`agent_settled` 时 `closeOpenBlocksLocked()` 兜底 flush，回复的尾巴不会静默丢失。
- **重复的 `text_start`**：丢弃该 index 上的残留 partial，上一块的尾巴不会串进新块。
- **多 `contentIndex` 交错**：按 index 分桶，flush 时按 index 升序 join（`sort.Ints`），避免 Go map 遍历顺序带来的不确定性。
- **压缩发生在 turn 中间**：`compaction_*` 刻意不碰 turn 缓冲，正在组装的回复能活过压缩周期。

## 4. 测试

### 4.1 单元测试（`translate_test.go`，42 个 Translate 用例全过）

核心回归锁：

| 测试 | 锁住的不变式 |
|---|---|
| `TestTranslate_SimpleTurn_SingleResult` | 无工具的一轮 = 恰好 `[result done]`，0 个 EventText（**§0.1 的直接回归锁**） |
| `TestTranslate_ToolTurn_NoDuplicateText` | 工具前的叙述出现在 EventText **且不出现在** EventResult（**零重复锁**），并断言完整事件顺序 |
| `TestTranslate_ToolEndingTurn_NoDuplicate` | §2.4.1 的回归锁：以工具调用结尾的一轮不得把已 flush 的段落再当作 result 发一遍 |
| `TestTranslate_NonStreamedTurnUsesMessageFallback` | 空 flush 不得误伤 lastMessageText 回退 |
| `TestTranslate_UntouchedSettleEmitsNoResult` | §2.4.3 的回归锁：未观察到任何事件的 settle 只出 EventDone |
| `TestTranslate_UsageSurvivesZeroTotalTokens` | §2.4.2 的回归锁：`totalTokens=0` 但 breakdown 非零时 usage 不得被丢 |
| `TestTranslate_UsageDropsAllZeroIncludingZeroCost` | 全零 usage 仍须丢弃，避免 footer 渲染 "$0.00" |
| `TestTranslate_UsageIsLatestSnapshotNotSum` | 两条 message（100/10 + 250/20）→ usage 是 `{250,20}` 而非 `{350,30}` |
| `TestTranslate_EmptyTurnStillCarriesUsage` | 以工具结尾的一轮 → Text 非空（兜底），usage 仍然送达 |
| `TestTranslate_ThinkingDoesNotLeakIntoResult` | 思考文本不进 EventResult |
| `TestTranslate_MissingTextEndStillFlushes` | abort 场景下缓冲的尾巴不丢 |
| `TestTranslate_InterleavedContentIndexes` | 两块并行流式不串字符，按 index 升序 join |
| `TestTranslate_TextStartDropsStalePartial` | 重复 text_start 丢弃残留 |
| `TestTranslate_TurnStateResetsBetweenTurns` | settled 后状态全清，第二轮不受污染 |
| `TestTranslate_ResetTurnClearsMidTurnState` | `/new` 打断流式后，半截文本不会出现在下一轮 |
| `TestTranslate_ResetWindowDropsAbandonedTurn` | §2.4.4 的回归锁：重置窗口内到达的旧 turn 事件全部丢弃，usage 不串 |
| `TestTranslate_EndResetRestoresTranslation` | 抑制标志必须能恢复，否则 session 永久静音 |
| `TestTranslate_CompactionPreservesTurnBuffers` | 压缩不清 turn 缓冲 |
| `TestTranslate_ConcurrentPendingTools`（既有，已改走 `resetTurn()`） | readPump × `/new` 并发无 race |

### 4.2 验证命令

```bash
go test ./internal/bridge/pi/ -race     # 35 个 Translate 用例 + 并发
go test ./internal/bridge/pi -run Real -v   # 真实 pi 二进制（靠 exec.LookPath("pi") 跳过，无 build tag）
go test ./... -race                     # 全量回归
```

### 4.3 真机结果（pi 0.83.0）

```
handshake ok: session="019fd7c2-…" model="deepseek-v4-flash"
turn-1: pi received our input and replied (60 chars) in 1.34s: "MIBLRE-CANARY-…-T1"
turn-2: … "[思考] The user wants me to repeat back the second ID verbatim." + "MIBLRE-CANARY-…-T2"
--- PASS
```

turn-1 一次性拿到完整的 60 字符 canary（改动前这里是逐 token 碎片）；turn-2 可见思考流作为独立 EventText、正文作为 EventResult，两者分离。

## 5. 不在本次范围

| 项 | 归属 |
|---|---|
| 共享层 `AccumulateUsage` 累加 → 覆盖 | 下个 PR（见 §2.5 注） |
| footer 分母 / 百分比（`7.8k / 200k · 4%`） | 下个 PR。数据源：pi `Model.contextWindow`（现被 `getStateModel` 丢弃）或 `get_session_stats.contextUsage` |
| acp bridge 的同类聚合 | acp 的块边界信号与 pi 不同（`stopReason`），各自按需处理 |
| pty bridge | raw 字节语义，本就不该被当作结构化文本 |
| 打字机 / 流式原地更新 | 明确不做——IM 异步远程编程场景没有意义 |

## 6. 对 `EventText` 契约的说明

本次在 `internal/agent/agent.go` 给 `EventText` 补了粒度契约注释：**一个 EventText 是一个完整的语义块，不是 delta**。claudecode 与 pi（F-52 后）符合；acp 尚未对齐，pty 是 raw 语义的例外。这条注释是为了让下一个写 bridge 的人不再踩同一个坑。
