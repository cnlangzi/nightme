# Slack Channel - 调研与接入方案

> **Status**: implemented (2026-08-30) — 编译 / vet / 测试全绿，全仓 3607 tests 无回归。**Phase 0 的 7 条探针仍未在真实 workspace 跑过**（§9），未经真机验收。
> **Scope**: nightme Slack 适配器（`internal/channel/slack/*`）
> **目的**: 用 Slack 原生的 agent 流式协议（`chat.startStream` / `appendStream` / `stopStream`）承载 rolling-log 占位体验，对标 Feishu receipt card 的观感。
> **Related docs**:
>
> - [feishu.md](./feishu.md) - 飞书 receipt card / reply-in-thread / 交互卡实现（**本方案的体验基线**）
> - [telegram.md](./telegram.md) - Telegram 占位链方案（**本方案不采用**，理由见 §1.2）
> - [feishu-reliability.md](./feishu-reliability.md) - 限流与重试分层
> - [CHANNEL.md](../CHANNEL.md) - Multi-channel 架构与接入不变量
> - [F-08-channel-abstraction.md](../feat/F-08-channel-abstraction.md) - Channel 抽象与 Gateway 边界
> - [F-63-heartbeat.md](../feat/F-63-heartbeat.md) - OutHeartbeat 契约
>
> **Slack 官方文档**:
>
> - [`chat.startStream`](https://docs.slack.dev/reference/methods/chat.startStream)
> - [`chat.appendStream`](https://docs.slack.dev/reference/methods/chat.appendStream)
> - [`chat.stopStream`](https://docs.slack.dev/reference/methods/chat.stopStream)
> - [`chat.postMessage`](https://docs.slack.dev/reference/methods/chat.postMessage) / [`chat.update`](https://docs.slack.dev/reference/methods/chat.update)
> - [Block Kit blocks](https://docs.slack.dev/reference/block-kit/blocks) / [markdown block](https://docs.slack.dev/reference/block-kit/blocks/markdown-block)
> - [Rate limits](https://docs.slack.dev/apis/web-api/rate-limits)
> - [Socket Mode](https://docs.slack.dev/apis/events-api/using-socket-mode)
> - [`assistant.threads.setStatus`](https://docs.slack.dev/reference/methods/assistant.threads.setStatus)
> - [`assistant_thread_started`](https://docs.slack.dev/reference/events/assistant_thread_started)
> - [App manifest schema](https://docs.slack.dev/reference/app-manifest.md)
> - [`files.getUploadURLExternal`](https://docs.slack.dev/reference/methods/files.getUploadURLExternal)
>
> **注意**：Slack 文档站已从 `api.slack.com` 迁到 `docs.slack.dev`，旧链接 302 跳转。
>
> **外部参考实现**:
>
> - `cc-connect` `platform/slack/slack.go`（本机 `/home/devin/cc-connect`）- 生产可用但基于旧 SDK，协议细节可抄，UX 层不可抄（§1.3）

---

## 1. 选型结论

### 1.1 核心判断：用 Slack 原生流式协议，不模仿 Feishu 的 PATCH

Slack 提供了一套**专为 agent 输出设计**的流式消息协议：

```
chat.startStream(channel[, thread_ts])  → 返回 ts（占位体）
chat.appendStream(channel, ts, chunks)  → 追加增量（可反复调用）
chat.stopStream(channel, ts)            → 终态封口
```

关键差异：**`appendStream` 是追加语义，不是整体替换**。

| | Feishu 占位卡 | Slack 流式消息 |
|---|---|---|
| 建占位 | `reply` 发 interactive card | `chat.startStream` |
| 更新 | `PATCH` 整个 card body | `appendStream` 只发增量 |
| 终态 | 最后一次 PATCH | `chat.stopStream` |
| 更新频控 | 5 QPS / message | Tier 4（100+/min） |
| 单次容量 | 30 KB 整包 | `markdown_text` 12,000 字符 / 次 |
| 总容量 | 30 KB 封顶 | 无整包上限 |
| Markdown | 6 步 sanitize + 按 1000 字拆 div | 原生 markdown，Slack 自行渲染 |

因此 Feishu 侧下列机制在 Slack **全部不需要**：

- `receiptMaxElements = 50` 元素预算与 `wouldReceiptOverflow` 判定
- `ErrReceiptOverflow` bail-out + `RolloverTo` 迁卡
- `splitMarkdownForDivs` 按 `divTextCharLimit` 拆分
- `renderLocked` 的 same-body 短路（防重复全量 PATCH）
- `SanitizeCardMarkdown` 六步管线（URL 白名单 / fence 换行 / 图片剥离 / 代码块保护 / 标题降级）

### 1.2 为什么不移植 Telegram 的占位链

Telegram 的 v9 chain（ROTATE / SPLIT / `inheritLatestHeader` / chain LRU）是**为 4096 字符硬上限 + 整体替换语义**设计的补偿机制。Slack 的追加语义让这套机制失去存在理由。

体验基线明确对标 Feishu，不对标 Telegram（Devin 拍板 2026-08-29）。

### 1.3 cc-connect 的可复用边界

cc-connect 有一份生产在跑的 Slack 实现，但它钉在 `slack-go v0.16.0`——该版本**没有** `markdown` block，**更没有**流式 API。它的 `streaming_card.go` 是在「`chat.postMessage` + `chat.update`」约束下憋出来的：3 秒节流、3500 字节上限、超限后 `Finalize` 改用 `postMessage` 重发全文以规避 `msg_too_long`。

**采用流式协议后这套补偿全部作废。**

| cc-connect 的东西 | 是否复用 |
|---|---|
| Socket Mode 事件循环 + Ack 模式 | ✅ 抄 |
| `assistantOrThreadTS`（Assistant Chat tab 路由，§5.3） | ✅ 抄 |
| 文件下载（Bearer + HTML 响应检测） | ✅ 抄 |
| `AppMentionEvent` 缺 Files 字段的绕过 | ✅ 抄 |
| `markdown_slack.go`（markdown → mrkdwn 手工转换） | ❌ 弃用，改用原生 `markdown_text` |
| `streaming_card.go`（3500 字节上限 + 重发 fallback） | ❌ 弃用 |
| 限流 / 重试 | ❌ 它没有，需自建 |
| 交互（block_actions / 模态） | ❌ 它没有，需自建 |

**SDK 版本：用 `slack-go/slack v0.29.0`**（2026-08-15 发布，活跃维护），不用 cc-connect 钉的 v0.16.0。

---

## 2. Slack 平台能力摘要

### 2.1 容量限制（已核实）

| 项 | 限制 | 来源 |
|---|---|---|
| `text` 参数 | 4,000 字符 | `chat.update` 文档 |
| `markdown_text` 参数 / chunk | 12,000 字符 | `chat.update` / `appendStream` 文档 |
| Block Kit blocks / 消息 | 50（模态和 Home tab 为 100） | blocks 文档 |
| section block `text` | 3,000 字符 | section-block 文档 |
| section block `fields` | 每项 2,000 字符，最多 10 项 | 同上 |
| `markdown` block | 单次 payload 内所有 markdown block 累计 12,000 字符 | markdown-block 文档 |
| `task_update` / `plan_update` chunk | 各 256 字符 | `appendStream` 文档 |
| `blocks` chunk | 每个数组最多 50 个 block（超出静默丢弃） | 同上 |
| **`chat.startStream` thread_ts** | **必填（普通 channel）** | **2026-08-31 实测** |
| **`chat.startStream` recipient_team_id + recipient_user_id** | **必填（流向 channel 时）** | **2026-08-31 实测** |
| **`assistant.threads.setStatus` thread_ts** | **必填** | **2026-08-31 实测** |

> **注意**：`text` 的 4,000 是按**字节**在服务端判定的，CJK 一字 3 字节，实际约 1,300 汉字。cc-connect 因此把上限压到 3,500 字节。用 `markdown_text` 可绕开这个坑。

> **⚠️ 关于 thread_ts**：Slack 官方文档将 `thread_ts` 标注为「Omit for top-level messages; only supported in channels where the whole channel is one session」。**实测与文档不符**——2026-08-31 在普通 channel（含 `#all-nightme`、`D0BTPL0K3UN`）下，不传 `thread_ts` 一律返回 `invalid_thread_ts`；传了真 ts 立即 OK。**结论**：所有 streaming 输出必须在 thread 里——普通文本用 user message ts 作 thread_ts，slash command 需 adapter 先 postMessage 建 parent thread。详见 §5.2 与 §9.2。

### 2.2 限流档位（已核实）

| 方法 | 档位 | 额度 |
|---|---|---|
| `chat.startStream` | Tier 2 | 20+/min |
| `chat.appendStream` | **Tier 4** | **100+/min** |
| `chat.stopStream` | Tier 2 | 20+/min |
| `chat.update` | Tier 3 | 50+/min |
| `chat.postMessage` | Special | **1/秒/频道**，允许短时突发 |
| `assistant.threads.setStatus` | 专用 | **600/min / app / team** |
| `reactions.add` / `remove` | Tier 1 | ~50/min（池化） |

超限返回 HTTP 429 + `Retry-After`（秒）。Slack 官方建议按 1 req/s 设计，突发额度不公开，各档位数值文档标注为"约"，**以 `Retry-After` 为准，不要硬编码档位数字**。

**事件投递侧另有配额**：约 **30,000 events / workspace / 小时**。这一条与 §6.1 的"权限全开"决策直接相关——订阅 `message.channels` 后繁忙 workspace 有触顶可能。

### 2.3 节流决策：3 秒

`appendStream` 的 Tier 4 理论上支持约 600ms 一次，但**不堵极限**（Devin 拍板 2026-08-29）：

> 聊天的更新间隔不需要那么短，没必要把这些极限都堵死。

**采用 3 秒最小间隔**，与 cc-connect 的经验值一致。理由：

- 留足突发余量，避免 429 后被动退避导致观感更差
- 3 秒的刷新率对「agent 还活着」的表达完全够用
- 与 `OutHeartbeat` 的 2 秒节流（Feishu `heartbeatMinInterval`）量级相当

实现上沿用 Feishu `renderLocked` 的模式：**最小间隔 + 定时器等待 + `ctx.Done()` 可取消**，而不是简单丢弃。

### 2.4 markdown 块的原生渲染

`markdown_text` / `markdown` block 支持：粗体、斜体、粗斜体、链接、有序/无序列表、删除线、标题（各级渲染一致）、行内代码、引用块、**带语法高亮的代码块**、分隔线、**表格**、任务列表。

官方文档明确说明该 block 的设计目标就是「LLM 返回的 markdown 在 Slack 渲染时丢失」这一场景。

**因此不写 markdown → mrkdwn 转换器。** 连带消除 cc-connect 的一个已知缺陷：它手拼 mrkdwn 但不转义 `<` / `>` / `&`，agent 输出里的裸 `<` 会破坏消息结构（Slack 把 `<` 当 mention/link 转义起始）。

### 2.5 `assistant.threads.setStatus`：更合适的心跳载体

Slack 为 AI app 提供了专用的"思考中"指示器：

```
assistant.threads.setStatus(channel_id, thread_ts, status[, loading_messages])
```

- 配额 **600/min per app per team**，比 `appendStream` 的 Tier 4 宽裕得多
- `status` 传空串即清除；app 回复后自动清除；若 app 一直不回复，**2 分钟超时**自动消失
- `loading_messages` 可给最多 10 条字符串，由 Slack 轮播
- 当前接受 `assistant:write` 或 `chat:write`，官方文档说明正在向仅 `chat:write` 过渡

**⚠️ 2026-08-31 实测修正**：
- **`thread_ts` 必填**——Slack 文档标注「可选」，实测 `invalid_arguments: missing required field: thread_ts`
- `channel_id` 字段名（不是 `channel`）
- `loading_messages` 必须是 JSON 数组

**决策：`OutHeartbeat` 优先走 `setStatus`，不烧 `appendStream` 配额。** 理由见 §2.6 的并发预算——`appendStream` 的配额要留给真正的内容增量。

依赖前提：app 需开启 AI 能力（manifest 的 `features.assistant_view`）。
- **实测通过**（2026-08-31）：`chat:write` + `assistant:write` 两个 scope 都齐（auth.test 错码 `missing_scope: team:read` 把所有已安装 scope 都列出来了），API 服务端返回 `x-accepted-oauth-scopes: chat:write`，scope 不是限制因素
- thread_ts 是真 Slack message ts（不是 TriggerID），传给 setStatus 即可

### 2.6 Socket Mode 约束与并发预算

**Socket Mode 硬约束**：

- 每个 app 最多 **10 条并发 WebSocket 连接**（单 daemon 场景不构成限制）
- 交互 payload 仍有 **3 秒 ack 窗口**——`block_actions` 必须先 ack 再处理，不能同步等 agent
- **Socket Mode 的 app 不允许上架公开 Slack Marketplace**。这意味着 nightme 无法提供 Feishu 那种"扫码一键装"的分发路径；如果未来要做应用市场分发，必须改用 Events API + 公网入口
- RTM（旧 WebSocket API）将于 **2026 年 11 月弃用**，Socket Mode 是其继任者——本方案不受影响

**并发预算（重要）**：

nightme 的核心卖点是多项目并行（README：所有项目从单实例并行跑）。这与 Slack 的限流直接冲突：

```
appendStream Tier 4 = 100+/min ≈ 1.67 req/s
每个活跃 turn 按 3 秒节流 ≈ 0.33 req/s
→ 约 5 个并发 turn 就会打满 Tier 4
```

**因此必须实现全局令牌桶**，而不是 per-chat 节流——照 Feishu 的 `internal/channel/feishu/ratelimit.go`（单桶覆盖所有出站 API，`StrictDefault` 惰性补充、无后台 goroutine、`ctx` 可取消）。3 秒的 per-turn 节流是**观感层**的选择；全局桶是**保护层**，两者叠加。

同时 §2.2 的事件投递配额（30,000/workspace/小时）在"权限全开 + `message.channels`"下也需要观测，超限的表现是事件丢失而非报错，必须有日志告警。

### 2.7 连接健康与重连

Slack Socket Mode 与 Feishu 的 `larkws` 同属"长连接 + 重连"模型（Telegram 的长轮询不是）。Feishu 侧为此建了两套设施：

- `internal/channel/feishu/health.go` — `WSHealth` 事件环形缓冲（connect / disconnect / reconnecting / error + 入站出站采样），供 `HealthSnapshot()` 吐给 daemoncontrol 的 health RPC
- `internal/channel/feishu/reconnect.go` — `prober` 看门狗，探活失败时重启连接

**Slack 侧必须对等实现**。`Channel` 接口的 `HealthSnapshot()` 是必填方法，返回空 payload 虽然合法（echo adapter 就这么做）但会让 `nightme doctor` 对 Slack 通道失明。

slack-go 的 `socketmode` 包自带重连，但它的回调粒度是否够构造 `WSHealth` 的五类事件——**未验证**，需在 Phase 1 确认；不够的话要在 `api.go` 的封装层自行观测。

---

## 3. 占位体生命周期

### 3.1 状态机（2026-08-31 实测修正）

**thread_ts 强制要求**——所有 streaming 输出必须在 thread 里。两条来源：
- **普通文本消息**：用户消息本身就是 thread root，`anchor = user.ts`（直接用）
- **Slash command**：`cmd.TriggerID` **不是** Slack message ts，adapter 在收事件时先 `chat.postMessage` 建一个"⏳ Processing..."占位消息，拿真 ts 当 thread root

```
[普通文本消息路径]
用户消息到达 (msg.ReplyTo = user.ts，真 ts)
   │
   ├─ chat.startStream(channel, thread_ts=user.ts, recipient_team_id, recipient_user_id, chunks)
   │                                                                              ──→ ts（占位体）
   ├─ OutReply / OutThinking / OutResult      ──→ appendStream(ts, markdown_text chunk)
   ├─ OutToolStart / OutToolEnd               ──→ appendStream(ts, task_update chunk，同 id 合并)
   ├─ OutTaskCreate / OutTaskUpdate           ──→ appendStream(ts, task_update chunk)
   ├─ OutHeartbeat                            ──→ assistant.threads.setStatus(channel_id, thread_ts=user.ts, status)

[Slash command 路径]
/cwd /path 触发
   │
   ├─ adapter.handleSlashCommand: chat.postMessage(channel, text="⏳ Processing /cwd...")
   │                                                                                       ──→ parentTS
   ├─ InboundMessage.MessageID = parentTS（真 Slack ts，透传 dispatcher）
   ├─ /cwd 命令执行，返回 SlashOutput{Reply: "Workspace set to /path"}
   │
   ├─ OutCommandReply → chat.postMessage(channel, thread_ts=parentTS, text="Workspace set to /path")
   │                                                                                       ──→ replyTS（在 parent thread 里）

[OnPromptEnded]
chat.stopStream(channel, ts, finalization blocks) ──→ 终态封口
   │
   └─ reactions.add(✅) 终态标记
```

**实测（2026-08-31）确认整套链路**：`chat.postMessage`(建 parent) → `chat.startStream`(thread_ts + recipient) → `chat.appendStream` → `chat.stopStream` 全部成功。Slack UI 里能看到流式消息作为 thread reply 出现在 #all-nightme channel。

节流层位于 `appendStream` 之前：3 秒窗口内的增量在内存缓冲区合并，窗口开启时一次性发出。

### 3.2 ⚠️ 模式锁死约束

`appendStream` 文档明确：

> The streaming mode must be the same as the streaming mode used to start the stream. If you started the stream with `markdown_text`, you must append with `markdown_text`. If you started the stream with `chunks`, you must append with `chunks`.

**因此必须一律用 `chunks` 模式开流**。若用 `markdown_text` 模式开流，中途出现 `OutToolStart` 想发 `task_update` 就发不出去了。

这条是硬约束，写进 adapter 的构造路径，不给调用方选择余地。

### 3.3 与 Feishu 占位卡的观感对齐

| Feishu | Slack 等价 |
|---|---|
| cold-create 时的 `🤖 Working` 占位行 | `startStream` 后首个 `plan_update` chunk |
| heartbeat header `💭 N · 🔧 M · ⏱ HH:MM:SS` | `plan_update` chunk（256 字符够用） |
| rolling-log entries | 连续的 `markdown_text` chunk |
| `**📋 Tasks**` checklist | `task_update` chunk + `task_display_mode` |
| `<hr>` + 灰色 StatusBar footer | 终态前最后一个 `markdown_text` chunk |
| `AddReaction(DONE)` 终态 ✅ | `reactions.add` |

StatusBar 复用 `internal/statusbar/statusbar.go::StatusBarLines`——它已经是 channel 无关的三行渲染（identity / usage / git），Feishu 和 Telegram 都在用。Slack 侧只需决定拼进哪个 chunk，不需要新的取数逻辑。

**StatusBar 走 `chat.stopStream` 的 finalization blocks**：`stopStream` 可携带最多 **50 个 block**，渲染在流式内容下方（因此 chunks 模式下总计可达 50 流式 + 50 终态 = 100 block）。这正好承载 Feishu 的 `<hr>` + 灰色 footer 语义，且只在终态发一次，不占 `appendStream` 配额。

`stopStream` 另有 `session_status` 参数（`active` / `processing` / `suspended` / `closed`），与 nightme 的 prompt 生命周期对应——`OnPromptEnded` 的成功 / 出错 / 取消三态可映射过去，细节待 Phase 0 实测确认各值的视觉表现。

### 3.4 追加语义的代价（不只是好处）

§1.1 强调了追加语义省掉多少 Feishu 机制，但它同时是**限制**：**已追加的内容无法撤回或改写**。

对照 Feishu 的 PATCH（可重写整张卡）会丢失以下能力：

| Feishu 能做 | Slack 追加模式下 |
|---|---|
| heartbeat header 原地刷新计数 | ❌ 已追加的 chunk 不可改。改用 `setStatus`（§2.5）或 `plan_update`（只改标题这个可变槽位） |
| `OutToolEnd` 回头改写 `OutToolStart` 那条消息 | ⚠️ **依赖"同 `id` 的 `task_update` 是更新而非追加"这一假设——未验证**（§9） |
| 出错时重写整张卡 | ❌ 只能再追加一段更正 |
| same-body 短路避免重复渲染 | ✅ 不需要——增量天然不重复 |

**这条是本方案最大的隐性风险**：§4 把 `OutToolStart` / `OutToolEnd` 合并寄托在 `task_update` 的同 id 语义上，若实测发现是追加而非更新，工具调用会变成两条独立卡片，需要退回"只在 End 时发一次"的策略。

### 3.5 流泄漏与 daemon 重启

Feishu 的卡片是无状态的——daemon 崩了，卡片就停在最后一次 PATCH，不会有副作用。**Slack 的流是有状态的**：`startStream` 之后若没有 `stopStream`，消息可能停留在"流式进行中"的视觉状态。

必须处理的场景：

| 场景 | 处理 |
|---|---|
| daemon 崩溃后重启，存在未关闭的流 | 持久化 `(channel, ts)` 的开流记录；启动时对每条记录补一次 `stopStream` |
| `stopStream` 调用失败 | 重试；仍失败则记录待清理，下次启动补 |
| 同一频道并发多个 turn | ⚠️ **未验证 Slack 是否允许同频道多条流并存**（§9） |
| 流开着但 agent 长时间无输出 | `setStatus` 有 2 分钟自动超时；流本身是否有超时未知 |

per-chat 状态（照 `telegram/state.go` 的模式）需要持久化开流记录，这是 Feishu 侧不存在的新增职责。

---

## 4. OutboundKind → Slack 映射

| OutboundKind | Slack 载体 | 说明 |
|---|---|---|
| `OutReply` | `markdown_text` chunk | 直接追加，12,000 字符/次 |
| `OutResult` | `markdown_text` chunk | 同上 |
| `OutThinking` | `markdown_text` chunk（💭 前缀） | |
| `OutToolStart` | `task_update` chunk | 带 `id` / `title` / `status` / `details` |
| `OutToolEnd` | `task_update` chunk（**同 `id`**） | 同 id 即天然合并——Feishu 的 `pushToolStart` / `popToolStart` / `mergeToolReply` FIFO 配对状态机不需要 |
| `OutTaskCreate` / `OutTaskUpdate` | `task_update` chunk | 原生 task card，不用手拼 `- [x]` |
| `OutHeartbeat` | `assistant.threads.setStatus` | 见 §2.5；配额 600/min，不烧 `appendStream`。退路为 `plan_update` chunk（256 字符） |
| `OutChoice` | **`chat.postMessage` + section/actions blocks** | 落地时偏离了原设计（原计划走 `blocks` chunk）。理由见 §4.4 |
| `OutChoicePatch` | `chat.update` | 选择卡settled 后原地改 |
| `OutMessageState` | `reactions.add` / `reactions.remove` | 见 §4.1 |
| `OutCommandReply` | `chat.postMessage` | 短状态消息不必开流 |
| `OutError` | `chat.postMessage` | 独立可见 |
| `OutInit` | 折进 StatusBar | 同 Feishu：不单独渲染 |
| `OutMessageStateRemoved` | `reactions.remove` | |

### 4.1 MessageState：Slack 是三家里唯一能做真状态替换的

| 平台 | reaction 能力 |
|---|---|
| Feishu | append-only；必须用预定义 `emoji_type`（`OneSecond`/`OnIt`/`DONE`），传 unicode 报 `99992354` |
| Telegram | **只能挂一个** emoji，状态槽位要精打细算 |
| **Slack** | 可叠加，**且可删除自己加的** |

因此 Slack 可以实现真正的状态替换：⏳ → 删掉换 🔄 → 删掉换 ✅，而不是像 Feishu 那样堆叠三个。需要 `reactions:read` scope 来确认当前已加了什么。

emoji 用**名字**（`thumbsup`，不带冒号），不接受 unicode 码点——与 Feishu 要求预定义 `emoji_type` 类似，与 Telegram 的 unicode 相反。错误码：`already_reacted` / `no_reaction` / `too_many_reactions`。

### 4.2 节流豁免规则

§2.3 的 3 秒节流是**内容流**的策略，不能一刀切套到所有 kind 上。阻塞型 UI 必须立即可见：

| kind | 节流 |
|---|---|
| `OutReply` / `OutThinking` / `OutToolStart` / `OutToolEnd` / `OutTaskCreate` / `OutTaskUpdate` | 3 秒窗口合并 |
| **`OutChoice`** | **豁免——立即发送** |
| `OutError` | 豁免 |
| `OutCommandReply` | 豁免（本就走 `chat.postMessage`） |
| `OutResult` | 豁免（终态，紧跟 `stopStream`） |

`OutChoice` 是用户必须点按才能继续的阻塞式交互，延迟最多 3 秒出现会让用户以为 agent 卡住——这与占位卡要解决的问题正好相反。全局令牌桶（§2.6）仍然适用，豁免的只是观感层的 3 秒窗口。

### 4.3 附件

Feishu 侧的附件处理在 `internal/channel/feishu/attachment.go`（27.6 KB）。Slack 侧对应：

**接收**：message 事件带 `files: [...]` 数组（`id` / `name` / `mimetype` / `url_private` / `url_private_download` / `size`）。下载必须带 `Authorization: Bearer xoxb-…` 头——**光有 URL 不够**。

cc-connect 的防御值得抄（`platform/slack/slack.go:586-588`）：若 `files:read` scope 缺失，Slack 返回的是 HTML 登录页而非文件内容，需检测 `<!DOCTYPE` / `<html` 前缀并报明确错误，否则会把一页 HTML 当成图片喂给 agent。

**发送**：`files.upload` **已于 2025-11-12 正式下线**，必须走三步流程：

```
1. files.getUploadURLExternal(filename, length)  → { upload_url, file_id }
2. POST 原始字节到 upload_url（form 字段名 file）
3. files.completeUploadExternal(files:[{id,title}], channel_id[, thread_ts])
```

scope 均为 `files:write`。已知坑：`completeUploadExternal` 返回时频道共享元数据可能尚未就绪，需要 `files.info` 轮询才能拿到消息 `ts`。

映射到 nightme：入站走 `BuildBlocks` 产出 `ContentImage` / `ContentFile`（照 Feishu 的 `docs/channel/feishu-onboarding.md` §A1 附件透传管线）；出站目前 `OutboundKind` **没有** attachment 类型（feishu.md §13.4 记录了这个不对称），Slack 侧同样不实现，留待抽象层补齐。

### 4.4 `OutChoice` 为什么没走 stream（落地偏离）

原设计（§4 表格）把 `OutChoice` 放在 `blocks` chunk 上，理由是 agent-UI block 只能经 chunks 传输。**实现时改为 `chat.postMessage` + 普通 section/actions block**，原因有二：

1. **规避 §9 探针 4 的风险**。"流关闭后按钮还能不能点"没有验证过。权限卡是用户必须点才能继续的阻塞式 UI，把它的可交互性绑在流的生命周期上，是拿最关键的一条路径去赌一个未验证的假设。
2. **`chat.postMessage` 只拒绝 agent-UI block**（Alert / Card / Carousel / TaskCard），普通的 section + actions + button 完全支持。权限卡只需要后者。

代价：用不了 Slack 的 Alert / Card 视觉。等探针 4 验证通过后可以再评估是否切回。

`OutChoicePatch` 相应走 `chat.update`，靠 `stateStore` 里按 `RequestID` 存的 `(channel, ts)` 定位原消息。

---

## 5. Thread 与群聊

### 5.1 三形态映射（`chat.postMessage` 路径）

| Feishu 形态 | Slack 等价 |
|---|---|
| `ReplyInChat` | `chat.postMessage(channel)`，不带 `thread_ts` |
| `ReplyInThread` | `chat.postMessage(channel, thread_ts=X)` |
| `ReplyInThreadAndChat` | `chat.postMessage(channel, thread_ts=X, reply_broadcast=true)` |

**Slack 没有 Feishu 的「不可逆提升」问题**——`thread_ts` 是每条消息自带的参数，不会永久改变父消息状态。Feishu 因此被迫把 `OutResult` / `OutChoice` / `OutCommandReply` / `OutTask*` 踢到顶级 Create（见 [feishu.md](./feishu.md) §13.10）的妥协，在 `chat.postMessage` 路径上不需要。

### 5.2 ✅ 流式 API 必须在 thread 内（2026-08-31 实测）

**doc vs 实测的差异**：

| | Slack 文档 | 实测结果 |
|---|---|---|
| `thread_ts` | 可选（"Omit for top-level"）| **必填** |
| 顶层 streaming | 普通 channel 不支持 | **一律 `invalid_thread_ts`** |
| In-thread streaming | Slack Code 频道支持 | **任何 channel 都支持** |
| `recipient_team_id` / `recipient_user_id` | 流向 channel 时必填 | 同 |

**核心约束**：所有 streaming 输出必须在 thread 里。thread_ts 必须是真 Slack message ts（**不是** TriggerID）。

**两路径都适用**：

**普通文本消息**——天然有 thread root：
- 用户消息本身是 thread root（`msg.ReplyTo = user.ts`，真 ts）
- `chat.startStream(channel, thread_ts=user.ts, recipient_team_id, recipient_user_id, chunks)` 立即 OK
- 整个 agent turn 的所有 chunk 都 thread 在 user message 下

**Slash command**——需要 adapter 建 parent thread：
- `cmd.TriggerID` 是请求 token，不是 Slack message ts
- `chat.startStream(channel, thread_ts=TriggerID, ...)` → `invalid_thread_ts`
- 修复：adapter 在 `handleSlashCommand` 时先 `chat.postMessage(channel, text="⏳ Processing /cwd...")` 建一个占位消息 → 拿返回的 ts 当 thread root
- 之后的所有 stream/reply 都用这个 ts 作为 thread_ts

**curl 实测证据**（非 SDK）：

| 测试 | 参数 | 错码 |
|---|---|---|
| `#all-nightme`, 无 thread_ts | 缺 thread_ts | `invalid_thread_ts` |
| `#all-nightme`, 有 thread_ts（真 ts） | 缺 recipient | `missing_recipient_team_id` |
| `#all-nightme`, thread_ts + recipient | 完整 | **OK**，返回 ts |
| `D0BTPL0K3UN` (DM), 无 thread_ts | 缺 thread_ts | `invalid_thread_ts` |
| `D0BUKGAKB4Y` (DM), thread_ts + recipient | 完整 | **OK** |

`x-accepted-oauth-scopes: chat:write` 头表明 scope 不是限制因素。

**Slack UI 验证**：`chat.startStream` + `chat.appendStream` + `chat.stopStream` 在 #all-nightme 里成功跑完，渲染成 thread reply（"Full info stream appended" 出现在 parent 消息下方）。

**doc §9.1 的混合模式 fallback 作废**——不再需要因为 thread 限制退而求其次走 PATCH 模式。但 §1.3 中 cc-connect 的 PATCH 模式仍然是 streaming API 不可用时的备选方案，需要保留作为代码注释里的 design rationale。

### 5.3 Assistant Chat tab 路由陷阱

cc-connect 用血标出来的坑（`platform/slack/slack.go:423-449`，注释很长）：

> 若 app 开启了 Assistant/Agent 模式，用户在「Chat」tab 里打的字会带上 assistant thread 的 `ThreadTimeStamp`。**回复必须带上同一个 `thread_ts`**，否则会落到 DM root、出现在 History tab，整个对话 UX 断裂。

其 `assistantOrThreadTS()` 的判定逻辑直接照抄：

```
ThreadTimeStamp != ""        → 用 ThreadTimeStamp（已在 thread / Assistant Chat tab）
ChannelType != "im"          → 用 TimeStamp（频道内挂到用户消息下）
其余（DM 顶层）              → 空
```

### 5.4 群聊接收模式

Slack 平台层面：

- `app_mention` 事件 = 只收 @ 自己的
- `message.channels` / `message.groups` / `message.mpim` = 收全部

nightme 的 `/watch all` 语义要求群里全收，因此**必须订阅 `message.*` 系列**。

**⚠️ 同时订阅 `app_mention` 和 `message.channels`，@ 消息会被投递两次**（两个事件各来一次）。必须按 `(channel, ts)` 建短 TTL LRU 去重。cc-connect 未遇到此问题（它只订阅了 `app_mention` + `message.im`）。

gate 逻辑不变：channel 只负责在 `InboundMessage.HasMention` 上打标，`/watch` 的判定仍在 `internal/chatsession/watchmode.go`。DM 恒 `true`；频道内 `<@BOT>` / `@here` / `@channel` / 回复 bot 消息 / slash 开头均为 `true`。

### 5.5 chatID 命名空间

```
sl_<team>:<channel>[:<thread_ts>]
```

硬约束（[CHANNEL.md](../CHANNEL.md) §5.5）：chatID 必须是 `(team, channel, thread_ts)` 的**纯函数**，不依赖 daemon 状态。

**绝不自动创建任何资源（频道 / thread / canvas）并把它编进 chatID。** Telegram 早期版本因自动创建 sentinel topic 导致 chatID 取决于「nightme 有没有建过」，违反稳定性契约，2026-08 整体重写（见 `internal/channel/telegram/adapter.go:538-543`）。

---

## 6. 权限与机器人开通

### 6.1 决策：权限全开

Devin 拍板（2026-08-29）：

> 权限全开。因为它是自己的机器人，自己的群，没有隐私问题。

与 Feishu 侧一致（`im:message.group_msg` 常驻 `DefaultAddons()`，2026-08-03 拍板）：平台层全收，收窄交给 nightme 侧的 `/watch`。

### 6.2 Bot Token Scopes

```
app_mentions:read     chat:write          chat:write.public   commands
channels:history      channels:read       groups:history      groups:read
im:history            im:read             im:write            mpim:history
reactions:read        reactions:write     files:read          files:write
users:read            assistant:write
```

`assistant:write` 用于 `assistant.threads.*`（§2.5）。官方文档说明该 scope 正在向 `chat:write` 过渡，具体弃用时间未公布——两个都申请，过渡完成后再删。

### 6.2.1 Slash Commands（必须显式注册）

`commands` scope 只是允许 bot 调用 Slack slash-command API 的 OAuth 权限，**不**代表任何命令已经注册。Slack 客户端（Web/Desktop/Electron 的 `composer.js`）对所有以 `/` 开头的输入做客户端拦截：先查 workspace 内已注册的 command 列表，命中才走 command 事件，没命中就弹「Only visible to you」+ Slackbot 的 "is not a valid command" 错——**根本不会发**到 bot。

因此 nightme 的每个 IM 命令（`/cwd`、`/use`、`/watch`、`/stop` 等 12 个）必须在 manifest 的 `features.slash_commands` 段显式列出。这是 2026-08-31 e2e 测试时才发现的盲区——之前的 manifest 只列了 scope，没注册任何 command，导致所有 `/xxx` 输入都被 Slack 客户端吃掉，bot 永远收不到。

参考 cc-connect `docs/slack-app-manifest.json` 的 `features.slash_commands` 段（24 个命令），以及 OpenClaw 的 `commands.native` 配置（Slack 默认 off，需 opt-in）。nightme 现在的做法跟 cc-connect 一致：manifest 全列、handler 收到 `SlashCommand` 事件后重序列化为普通 `InboundMessage` 走同一套 dispatcher（见 `internal/channel/slack/adapter.go:647 handleSlashCommand`）。

**改 manifest 后必须 Reinstall to Workspace**（cc-connect FAQ 第 1 条已记这条坑）。

### 6.3 Event Subscriptions（bot events）

```
app_mention   message.im   message.channels   message.groups   message.mpim
assistant_thread_started
```

`assistant_thread_started` 无需额外 scope，在用户打开 AI 容器 / 新建 AI 会话时触发，payload 含 `assistant_thread.channel_id` / `thread_ts` / `context`（用户当时所在的频道）。

### 6.3.1 AI 能力开关

流式 API 与 `assistant.threads.*` 依赖 manifest 的 `features.assistant_view`（新版 Agents & AI Apps 界面为 `features.agent_view`）。**必须开启**——§9 开放项 3 就是验证不开启时流式 API 是否可用。

### 6.4 App-Level Token

Socket Mode 需要一个 App-Level Token（`xapp-` 前缀），scope 为 `connections:write`。

### 6.5 用户开通流程

Slack 支持 **App Manifest** 一次性配好全部 scope 与事件，不用逐项点选。cc-connect 的 `docs/slack-app-manifest.json` 是可用模板。

```
1. api.slack.com/apps → Create New App → From an app manifest
2. 选 workspace，粘贴 manifest JSON，确认创建
3. Socket Mode → 开启 → 生成 App-Level Token（scope: connections:write）
   → 拿到 xapp-...（⚠️ 只显示一次）
4. Install App → Install to Workspace → 授权
   → 拿到 Bot User OAuth Token xoxb-...
5. nightme login slack → 粘贴两个 token
6. 群里 /invite @nightme，然后 @ 它
```

**无需公网 IP / 域名 / HTTPS 证书 / 反向代理**——Socket Mode 走 WebSocket 出站连接。

**两条必须写进 onboarding 文案的坑**（cc-connect FAQ）：

- 改完 scope 或 event 之后**必须 Reinstall to Workspace**，否则不生效
- App-Level Token 只显示一次，漏存需重新生成

### 6.6 与 Feishu / Telegram 开通方式的对比

| | Feishu | Telegram | Slack |
|---|---|---|---|
| 建 app | 扫码，飞书自动建（OAuth 2.0 Device Grant） | @BotFather 手动 | manifest 一次贴，半自动 |
| 凭证 | 自动回填 app_id / app_secret | 手动粘 bot_token | 手动粘 xoxb + xapp |
| 公网入口 | 不需要（WS 长连接） | 不需要（长轮询） | 不需要（Socket Mode） |

Slack 无法做到 Feishu 那种「扫码零手动」——没有第三方代注册接口。体验介于 Feishu 和 Telegram 之间。

---

## 7. 接入 wiring 清单

runtime / gateway / chatsession / dispatcher **零改动**——这是 [CHANNEL.md](../CHANNEL.md) 承诺的 OCP 扩展点（`internal/runtime/runtime.go:39-46`）。

### 7.1 配置层

| # | 文件 | 动作 |
|---|---|---|
| 1 | `internal/config/config.go:70` | `Config` 加 `Slack SlackConfig` |
| 2 | `internal/config/config.go:124-127` | 照 `TelegramConfig` 加 `SlackConfig`（`BotToken` / `AppToken`） |
| 3 | `internal/config/config.go:383-385` | `applyDefaults` |
| 4 | `internal/config/config.go:404-411` | `applyEnvOverrides` 加 `NIGHTME_SLACK_*` |
| 5 | `configs/nightme.example.yaml` | 加 `slack:` 段 |

### 7.2 适配器包 `internal/channel/slack/`

| 文件 | 对照 |
|---|---|
| `init.go` | 5 行，照 `telegram/init.go:13-17` 调 `channel.Register("slack", ...)` |
| `adapter.go` | 实现 `Channel` 八方法 |
| `api.go` | slack-go 的窄接口封装（见 §7.5） |
| `stream.go` | 流式占位体状态机 + 3 秒节流 |
| `state.go` | per-chat 状态持久化，照 `telegram/state.go` |
| `ratelimit.go` | 令牌桶 + 429 / `Retry-After` 退避 |
| `retry.go` | 瞬时错误分类与重试 |
| `callback.go` | `block_actions` / `view_submission` |
| `model.go` | 事件结构体 |

### 7.3 Login

| # | 文件 | 动作 |
|---|---|---|
| 6 | `internal/login/slack/register.go` | 照 `login/telegram/register.go:20-69` |
| 7 | `internal/login/slack/provider.go` | 实现 `login.Provider`（Name / Login / Greet） |
| 8 | `internal/login/login.go:243-254` | 加 `case "slack":` 写凭证（当前落 `default:` 报 unknown provider） |
| 9 | `internal/login/login.go:266-272` | 保存失败时的凭证回显分支 |
| 10 | `internal/login/login.go:288-296` | 成功摘要分支 |

### 7.4 二进制装配

| # | 文件 | 动作 |
|---|---|---|
| 11 | `cmd/nightme/root.go:16-17` | 加 `_ ".../internal/channel/slack"` |
| 12 | `cmd/nightme/login.go:37-38` | 加 `_ ".../internal/login/slack"` |

### 7.5 SDK 隔离（可测性硬约束）

现有两个 adapter 都把外部 SDK 挡在窄接口后：

- Feishu：`sendFunc` / `updateFunc` / `mergeTextFunc` 函数字段
- Telegram：`apiClient` interface（仅 `call` + `download` 两方法，`telegram/api.go:15-33`）

**slack-go 只允许出现在 `api.go` 一个文件里**，其余代码依赖自定义窄接口。否则 `adapter_test.go` 无法 mock。

---

## 8. 实施顺序

**Phase 0 — 实测 spike（先做，不可跳过）**

产出一个一次性的 `cmd/slackprobe`（不进主干，验完即删），逐条打 §9 的探针并打印原始响应——照 Feishu 团队 2026-08-05 做 probe-A/B/C/D 的方式（结论沉淀在 [feishu.md](./feishu.md) §13.10）。**实测结论回写本文档 §9，再动 Phase 3。**

**Phase 1 — 骨架**
§7 的 12 处 wiring + Socket Mode 事件循环。目标：DM 收发纯文本走通。

**Phase 2 — 协议层**（从 cc-connect 移植）
`assistantOrThreadTS`（§5.3）/ 文件下载（Bearer + HTML 响应检测，§4.3）/ `AppMentionEvent` 缺 Files 字段的绕过（重新 unmarshal 原始 inner event JSON）/ 事件去重（§5.4）。

**Phase 3 — 流式占位**（核心）
`startStream(chunks)` → 3 秒节流的 `appendStream` → `stopStream` + finalization blocks；全局令牌桶（§2.6）；429 + `Retry-After` 退避；流泄漏恢复（§3.5）。

**Phase 4 — 交互与状态**
`OutChoice` 走 `blocks` chunk + `block_actions` 回调（3 秒内先 ack 再处理）；MessageState 用 add / remove 做真状态替换（§4.1）。

---

## 9. 开放项（2026-08-31 实测后状态）

按风险级别。**已通过实测的探针更新为 ✅，仍有疑问的留为 🟡**。

| # | 探针 | 状态 | 结果 |
|---|---|---|---|
| **1** | `assistant_view` 是否必填？| ✅ | 必须开。manifest 已声明，**实测** `chat.startStream` 在 `assistant_view` + `assistant:write` + `chat:write` 三个都齐的 app 上可调用 |
| **2** | 同 `id` 的 `task_update` 是"更新"还是"追加"？| 🟡 | 未实测。`appendStream` 文档未说明，§3.4 已标为隐性风险。建议 Phase 3 实测：`appendStream(chunks=[{type: task_update, id: "t1", ...}])` 两次，看是否合并 |
| **3** | 同一频道能否并存多条流？| 🟡 | 未实测。建议 Phase 3 实测：在 #all-nightme 开两条不同 thread 的流，看是否互不干扰 |
| **4** | `blocks` chunk 里的按钮在 `stopStream` 之后还能不能点？| 🟡 | 未实测。`OutChoice` 当前走 `chat.postMessage`（§4.4），不依赖 stopStream，所以这条已**规避**风险——但需验证按钮在 postMessage 路径上确实可点 |
| **5** | 普通频道内 `startStream` 带 `thread_ts` 是否报 `invalid_thread_ts`？DM 内如何？ | ✅ | **thread_ts 必填**——Slack 文档与实测不符。普通 channel、DM 都是 `invalid_thread_ts` 当 thread_ts 缺失。修复方案见 §5.2 与 §11 |
| **6** | `task_display_mode` 选 `timeline` 还是 `plan`？| 🟡 | 未实测。一条流只能选一个，建议两条流对比渲染 |
| **7** | 流开着但长时间无追加，是否有服务端超时？| 🟡 | 未实测。`setStatus` 有 2 分钟自动超时，stream 本身未知。建议 phase 3 实测：开流不追加，等 5 分钟，看 Slack 是否主动 stopStream |
| **8** | `handleSlashCommand` 端到端 | 🟡 | **部分通过**——adapter 收到 `SlashCommand` 事件，重序列化为 `/cmd args` 文本，进 dispatcher，command parser 识别，路由到 `internal/command/<name>`。**整链路未实跑**（manifest 修了，但 `OutboundMessage.ReplyTo = cmd.TriggerID` 不是真 Slack ts，会导致 streaming 失败——详见 §9.2） |

另有一项工程决策待定：`OutResult` 是否使用 `reply_broadcast=true`（回到频道更醒目，但群里人多时噪音大）。

### 9.0 已修复

- **2026-08-31 e2e 测试 A**：发现所有 `/xxx` 输入被 Slack 客户端拦截，nightme manifest 没注册任何 slash command，bot 永远收不到。**修复**：`internal/login/slack/manifest.go` 加 `features.slash_commands` 段（12 个命令），`/cwd`、`/use`、`/watch`、`/stop`、`/steer`、`/queue`、`/new`、`/close`、`/think`、`/tools`、`/review`、`/gtw`。`/close` 改成 `Slack `（Slack 保留 `/close`，manifest validator 拒绝）；adapter handleSlashCommand 把 `Slack ` → `/close` 翻译回去
- **2026-08-31 e2e 测试 B**：manifest 改了必须 **Reinstall to Workspace**。已已通过 Slack Dashboard 操作完成

### 9.1 Fallback 设计（探针 1 为否时——已不再需要）

~~退回 cc-connect 验证过的老路，**不是重新设计**~~

```
~~chat.postMessage（首发，容量宽松）→ 拿 ts
  → chat.update 每 3 秒改一次（Tier 3，50+/min）
  → 内容超 3,500 字节停止 update
  → 终态用 chat.postMessage 重发完整内容~~
```

~~代价：失去 `task_update` 原生任务卡、失去追加语义（每次重传全文）、撞 4,000 字符墙需要 Telegram 式的 ROTATE。~~

**2026-08-31 更新**：streaming API 实测可用（`thread_ts` + `recipient_team_id` + `recipient_user_id` 三参数都齐），不再需要这个 fallback。但 cc-connect 的 `streaming_card.go` 实现仍保留为代码注释中的 design rationale——若未来 workspace 升级或 Slack Code 频道出现，可能需要重新评估。

### 9.2 2026-08-31 实测发现的问题清单

按严重度：| ID | 严重度 | 描述 | 状态 |
|---|---|---|---|
| A | CRITICAL | `chatstore/store.go:87` 验证只接受 `tg_` / `oc_` 前缀，**不认 `sl_`**。daemon 重启加载旧 `sl_` session 时炸 → 自动备份 + 清空 chat_sessions.json | 待修 |
| B | CRITICAL | `send.go:streamFor` 的 `anchor` 兜底用 `msg.ReplyTo`。**slash command** 时 `msg.ReplyTo = cmd.TriggerID`（不是 Slack message ts），`chat.startStream(thread_ts=TriggerID)` → `invalid_thread_ts`。**普通文本** 时 `msg.ReplyTo = user.ts`（真 ts）但代码没传 `recipient_team_id` / `recipient_user_id` → `missing_recipient_team_id` | 待修（已测，方案见 §5.2） |
| C | MEDIUM | `mention.go:publishInbound` 把 Slack DM 的 `thread_ts` 编进 chatID（`sl_<team>:<channel>:<greeting_ts>`），违反 §5.5 不变量 | 待修 |
| D | HIGH | Slack DM 客户端拦截所有 `/xxx` slash command —— 错误信息"isNotX-xxx is not supported in threads" 来自 Slackbot，非我们的 bot。这是 Slack 平台行为：DM 内的 slash command 强制走 thread 上下文 | 已记录，DM slash command 不可用（用户需在 channel 用） |
| E | MEDIUM | `chat.update` / `chat.startStream` 等的 `MessageState` reaction 在 slash command 上失败（`message_not_found`）—— `handleSlashCommand` 把 `cmd.TriggerID` 当 MessageID，trigger_id 不是真消息 ts | 待修（与 B 一起） |
| F | MEDIUM | `nightme doctor` `last outbound: never`（实测 bot 回复多次仍显示），`last inbound` 的 `chat=` 为空 | 待修 |
| G | LOW | `/close` → `/kclose` 跨平台命名差异（Feishu/Telegram 仍是 `/close`）| 已记录 |

---

## 10. 测试策略

对齐 Feishu / Telegram 两份文档的做法：SDK 挡在窄接口后（§7.5），全部用 mock 打。

| 层 | 覆盖 |
|---|---|
| 单元 | chunk 构造（各 `StreamChunkType` 的 JSON 形状）；3 秒节流窗口内的增量合并；§4.2 豁免规则逐 kind 断言；chatID 编解码往返；`HasMention` 判定（DM 恒 true 的不变式，照 Feishu 的 `TestComputeHasMention_DMInvariant`） |
| 单元 | 事件去重：同一 `(channel, ts)` 从 `app_mention` 和 `message.channels` 各来一次，只投递一次 |
| 单元 | 限流：全局桶在 N 并发 turn 下的间隔断言（照 Feishu `TestLimiter_InitialBurst`）；429 + `Retry-After` 退避 |
| 集成 | 完整 turn：startStream → N 次 append → stopStream，断言调用序列与模式一致性（§3.2 的 chunks 模式锁死） |
| 集成 | 流泄漏恢复：模拟重启后存在未关闭流，断言补发 `stopStream` |
| 回归 | `OutChoice` 不被节流（§4.2） |

---

## 11. 已决议

| 日期 | 决议 | 决策人 |
|---|---|---|
| 2026-08-29 | 权限全开（scope / event 全订阅），收窄交给 `/watch` | Devin |
| 2026-08-29 | 体验对标 Feishu，不对标 Telegram | Devin |
| 2026-08-29 | 更新间隔 3 秒，不堵限流极限 | Devin |
| 2026-08-29 | 用 slack-go v0.29.0，不用 cc-connect 钉的 v0.16.0 | 本文档 §1.3 |
| 2026-08-29 | 一律用 `chunks` 模式开流（模式锁死约束） | 本文档 §3.2 |
| 2026-08-29 | 不写 markdown → mrkdwn 转换器，用原生 `markdown_text` | 本文档 §2.4 |
| 2026-08-29 | `OutHeartbeat` 走 `assistant.threads.setStatus`（600/min），不烧 `appendStream` 配额 | 本文档 §2.5 |
| 2026-08-29 | StatusBar 走 `stopStream` 的 finalization blocks，终态发一次 | 本文档 §3.3 |
| 2026-08-29 | 全局令牌桶（照 Feishu `ratelimit.go`）+ per-turn 3 秒节流，两层叠加 | 本文档 §2.6 |
| 2026-08-29 | `OutChoice` / `OutError` / `OutResult` 豁免 3 秒节流 | 本文档 §4.2 |
| 2026-08-29 | 出站附件不实现（`OutboundKind` 无 attachment 类型，与 Feishu 同）| 本文档 §4.3 |
| 2026-08-30 | `OutChoice` 改走 `chat.postMessage` + 普通 block，不走 stream | 本文档 §4.4 |
| 2026-08-30 | `login.Credentials` 新增 `AppToken` 字段（Slack 是唯一需要双凭证的 channel）| 落地 |
| 2026-08-31 | manifest 加 `features.slash_commands` 段（12 个命令），解 Slack 客户端拦截 `/` 前缀导致 bot 收不到命令的问题 | e2e 测试发现 |
| 2026-08-31 | `/close` 改名为 `Slack `（Slack 保留 `/close`），adapter 把 `Slack ` 翻译回 `/close` | e2e 测试发现（manifest validator 拒绝 `/close`） |
| 2026-08-31 | **`chat.startStream` 必须传 `thread_ts` + `recipient_team_id` + `recipient_user_id`**（Slack 文档与实测不符；文档标「可选」实测「必填」）| e2e 实测确认 |
| 2026-08-31 | **所有 streaming 输出必须在 thread 内**。普通文本：用 `msg.ReplyTo` (user.ts) 当 thread_ts。Slash command：adapter 在 handleSlashCommand 先 `chat.postMessage` 建 parent thread | e2e 实测确认 |
| 2026-08-31 | `assistant.threads.setStatus` 也需要 `thread_ts`（文档标「可选」实测「必填」）| e2e 实测确认 |
| 2026-08-31 | 保留 streaming 路径（§3），**不再采用** §9.1 的 PATCH fallback（实测 streaming API 可用） | e2e 实测结论 |

---

## 12. 落地实况（2026-08-30）

### 12.1 文件清单

| 文件 | 职责 |
|---|---|
| `internal/channel/slack/api.go` | **唯一** 直接引用 slack-go 的文件；`apiClient` / `socketRunner` 两个窄接口 |
| `internal/channel/slack/adapter.go` | `Channel` 八方法、Socket Mode 事件循环、入站解析、附件下载 |
| `internal/channel/slack/send.go` | `Send` 分发表、`OnPromptEnded`、Choice 渲染、MessageState 状态机 |
| `internal/channel/slack/stream.go` | `turnStream`：缓冲 / 节流 / start-append-stop / 工具 FIFO 配对 |
| `internal/channel/slack/streamindex.go` | 按 turn 索引的 LRU |
| `internal/channel/slack/state.go` | 开流记录与 Choice 状态持久化（`slack_state.json`） |
| `internal/channel/slack/dedup.go` | `(channel, ts)` 双投递去重 |
| `internal/channel/slack/chatid.go` | `sl_<team>:<channel>[:<threadTS>]` 编解码 |
| `internal/channel/slack/mention.go` | `HasMention` 判定与 mention 前缀剥离 |
| `internal/channel/slack/render.go` | 分块、截断、footer blocks、heartbeat 文案 |
| `internal/channel/slack/ratelimit.go` | 全局令牌桶 |
| `internal/channel/slack/retry.go` | 瞬时错误分类 + `Retry-After` 优先 |
| `internal/channel/slack/health.go` | 连接健康环形缓冲（`HealthSnapshot`） |
| `internal/channel/slack/init.go` | `channel.Register("slack", …)` |
| `internal/login/slack/{provider,register,manifest}.go` | `nightme login slack` + `--manifest` |

wiring：`internal/config/config.go`（`SlackConfig` + defaults + env）、`configs/nightme.example.yaml`、`internal/login/login.go`（三处 switch）、`cmd/nightme/{root,login}.go`（各一行 blank import）。runtime / gateway / chatsession **零改动**。

### 12.2 验收结果

#### 2026-08-30 初版（编译 / 单测）

| 项 | 结果 |
|---|---|
| `go build ./...` | 通过 |
| `go vet ./...` | 无告警 |
| `go test ./...` | **3607 passed / 63 packages**，无回归 |
| slack 两包测试 | 127 个用例 |
| `gofmt` | 新增文件全部干净 |

#### 2026-08-31 e2e 实测（Slack workspace + 真 token）

通过 curl **直连 Slack API**（不经 SDK）+ Slack UI 验证：

| 探针 / 行为 | 实测结果 |
|---|---|
| `chat.postMessage` 普通文本 | ✅ OK |
| `chat.postMessage` + `thread_ts` | ✅ OK |
| `chat.update`（text 和 blocks）| ✅ OK |
| `chat.startStream`（无 thread_ts）| ❌ `invalid_thread_ts`（Slack 文档说不必填，实测必填） |
| `chat.startStream`（thread_ts 但缺 recipient）| ❌ `missing_recipient_team_id` |
| `chat.startStream`（thread_ts + recipient_team_id + recipient_user_id）| ✅ **OK** |
| `chat.appendStream`（在 startStream 之后）| ✅ OK |
| `chat.stopStream`（含 finalization blocks）| ✅ OK |
| `assistant.threads.setStatus`（无 thread_ts）| ❌ `missing required field: thread_ts` |
| `assistant.threads.setStatus`（有 thread_ts）| 🟡 未实测（curl 错误已确认是 thread_ts 缺失）|
| Slack UI 渲染流式消息 | ✅ thread reply 出现在 #all-nightme |
| 12 个 slash command 在 Slack UI 显示 | ✅（Slack Commands 页面列出 `/cwd`/`/use`/`/watch` 等） |
| `/cwd /path` 在 #all-nightme 生效（CWD 设置）| ✅ chat session `sl_T:D:...` 的 `selectedCwd` 已写入 |
| `/cwd` 后 bot 静默（CWD 设了但无确认回复）| ❌ `invalid_thread_ts` —— Issue B（待修）|
| `/watch on` 后 bot 静默 | ❌ 同上 |
| Slash command 在 DM 被拦截（"not supported in threads"）| ❌ Slack 平台行为——DM slash command 不可用 |
| Bot 加入 #all-nightme 后正常收发 | ✅ 通过 "Channel details → Agents & apps → Add" |

**结论**：streaming API **完全可用**，但需要：
1. `thread_ts` 必填（真 Slack message ts，不是 TriggerID）
2. `recipient_team_id` + `recipient_user_id` 必填（流向 channel 时）
3. Slash command 需 adapter 先 `postMessage` 建 parent thread

详见 §5.2、§9.2、§11。
