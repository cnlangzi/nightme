# Pi Coding Agent — 集成方案与待办

> **Status**: implementation reference（F-32 传输层 + F-52 事件整合已落地；§6 待定项未落地）
> **Scope**: `internal/bridge/pi/*` — nightme 侧的 pi coding agent 适配器
> **Related docs**:
> - [F-32-pi-rpc-bridge.md](../feat/F-32-pi-rpc-bridge.md) — RPC 传输层 + 原始 event 映射表
> - [F-52-pi-stream-aggregation.md](../feat/F-52-pi-stream-aggregation.md) — 流式事件整合（本文 §3 的权威来源）
> - [F-34-new-slash-command.md](../feat/F-34-new-slash-command.md) — `/new` → `new_session`
> - [F-49-compaction-counter.md](../feat/F-49-compaction-counter.md) — footer token 语义
> **上游文档**: pi 自带 `docs/rpc.md`（随 npm 包分发，本机在
> `$(npm root -g)/@earendil-works/pi-coding-agent/docs/rpc.md`）

---

## 1. 传输层选型

nightme 通过 **pi 私有的 `--mode rpc` 协议**直连，不经任何适配层：

```
nightme ──stdio JSONL──> pi --mode rpc [--session-id <resume>]
```

严格 LF 分帧，无 JSON-RPC 2.0 信封。三种帧：

| 帧 | 形状 |
|---|---|
| command | `{"id":<id>, "type":<name>, ...}` |
| response | `{"id":<id>, "type":"response", "command":<name>, "success":<bool>, "data":<obj>, "error":<str>}` |
| event | `{"type":<name>, ...}` |

### 1.1 为什么不走 ACP

openclaw 的链路是 `openclaw --ACP--> npx pi-acp@^0.0.31 --> pi --mode rpc --no-themes`：`pi-acp` 是个独立适配器包，**内部 spawn 的正是 `pi --mode rpc`**。ACP 那层的价值是「一套代码接 N 个 harness」，不解决数据整合，代价是丢掉 pi 私有协议独有的能力（extension UI、compaction 事件、pi 结构化 usage）。

nightme 已经为每个 agent 写 bridge，所以直连收益更高。**结论：传输层选型与 openclaw 的实际底层一致，无需改动。**

### 1.2 与 cc-connect 的差异

cc-connect 早期走 `pi --mode json -p`（每条 prompt 重启一个进程，fire-and-forget，无 turn 边界 / 无 abort / 无 Extension UI），后来补了 rpc 模式。nightme 从一开始就用 rpc。

---

## 2. 生命周期

```
进程: newSession() ──> handshake(get_state) ──> ... ──> cmd.Wait() ──> close(events)
轮次: prompt ack ──> 若干 event ──> agent_settled ──> EventDone
```

一个进程承载多个 turn。**`EventDone` 不关闭 events channel**，只有进程退出或 `Close()` 才关——`ChatSession.runReadPump` 依赖这点跨 turn 持续读取。

| 阶段 | 超时 | 说明 |
|---|---|---|
| handshake（`get_state`） | 10s | 冷启动含模型预热 |
| prompt ack（`SendBlocks`） | 90s | 只等 ack，**不**等 `agent_settled` |
| `Close()` SIGINT→SIGKILL | 2s | |
| Close 等待 reap | 5s | 超时也返回，避免 wedge 住 runtime |

`/new` → `new_session` + `get_state`（各 10s），**不重启进程**。

---

## 3. 事件整合（F-52）

### 3.1 核心问题

pi 以 **token 粒度**推 `text_delta`。若逐条翻成 `EventText`，gateway 会映射成 `OutReply`，飞书 adapter 再把每条渲染成独立的 💬 日志条目——一句话裂成 ~20 个气泡 + ~20 次卡片 PATCH（限流风险）。

根因是 `EventText` 的粒度契约从未定义。四个 bridge 各自理解不同，只有 claudecode（按 content block 发）碰巧正确。

### 3.2 映射表

| pi wire 事件 | 处理 |
|---|---|
| `text_start` | 重置 `turn.textBuf[contentIndex]` |
| `text_delta` | 累加，**不 emit** |
| `text_end` | 转入 `turn.pendingText`，**不 emit** |
| `thinking_start` / `thinking_delta` | 累加 `turn.thinkBuf`，**不 emit** |
| `thinking_end` | **`EventText{"[思考] " + 全文}`** |
| `tool_execution_start` | **先 flush `pendingText`**（非空则 EventText），再 `EventToolStart` |
| `tool_execution_end` | `EventToolEnd`（从 `pendingTools` 回填 Name/Args） |
| `message_end(assistant)` | 记录 `lastMessageText` / `stopReason` / `lastUsage`，**不 emit** |
| `message_end(toolResult)` | no-op（已由 `tool_execution_end` 覆盖） |
| `compaction_end` | `EventCompaction`（`compaction_start` 屏蔽，一个周期只计一次） |
| **`agent_settled`** | **`EventResult` → `EventDone`**，然后 turn 状态归零 |
| `agent_end` | **不作终态**（带 `willRetry`，一个 turn 内可多次） |
| 其余 / 未知 | debug 日志，不 emit，不杀 session |

净效果：

- **无工具的一轮** → 0 个 EventText，1 个 EventResult
- **有工具的一轮** → 每段叙述一条 💬 + 最终结论一条 📝，**零重复**

### 3.3 flush 点为什么选 `tool_execution_start`

这是整个设计的支点：它让「一轮一次 OutResult」和「中途能看到进度」不冲突。每次 flush 都清空 `pendingText`，所以 `EventResult` 拿到的必然是最后一段，永不重复已投递的文本。

不在 `message_end(assistant)` flush：那条消息可能就是最终答案。
不全程憋到 settled：带工具的长任务会看不到中途进度。

### 3.4 四个必须保留的守卫

| 守卫 | 防的问题 |
|---|---|
| `textDelivered` | pi 的 `message_end(assistant, [text, toolCall])` **先于** `tool_execution_start`，以工具结尾的一轮会让 `lastMessageText` 回退重放已 flush 的段落 |
| `active` | `agent_settled` 也用于结算 out-of-band 路径（如 fire-and-forget 压缩），空 turn 无条件出 result 会塞一张莫名的「Done.」卡 |
| `beginReset`/`endReset` 抑制窗口 | `/new` 可打断进行中的 turn（无 Busy 守卫、slash command 不排队），而 `get_state` 有 10s 窗口，期间旧 turn 事件会污染新 turnState |
| `emptyReplyFallback = "Done."` | `gateway.Translate` 丢弃空文本 result，会连带丢掉 usage |

### 3.5 usage 语义

一轮里多条 assistant message，每条带 usage。**取最后一条（覆盖），不求和**——每次 API call 的 input 侧已含全部历史，是上下文占用**快照**，求和会按调用次数倍数虚高。cc-connect 的 `handleAgentEnd` 倒序取最后一条，结论一致。

---

## 4. 已知的 provider 差异

pi 的事件流**随 provider / api 类型变化**，不能假设所有字段都在。实测 `SenseNova / deepseek-v4-flash / api:"openai-completions"`：

- `message_end` 会出现 `role:"user"`（bridge 记 debug 日志后丢弃）
- 完整 assistant 文本**同时**出现在 `turn_end` 和 `agent_end.messages[]`——两者都不作终态，仅 `agent_settled` 是
- 部分轮次无 thinking，部分轮次无工具

bridge 对未知字段一律宽松（`json.RawMessage` + 忽略未知 key），未知事件只记 debug 不杀 session。**新增字段解析前请先在 §7 的方法下抓真实 wire 样本。**

---

## 5. 可观测性

| 开关 | 作用 |
|---|---|
| `NIGHTME_PI_DEBUG=0` | 关闭 bridge 的 `[pi]` 面包屑（默认开） |
| `NIGHTME_LOGGING_LEVEL=debug` | 打开逐事件 debug 日志（含被忽略的 event 类型 + raw） |
| `NIGHTME_STDERR_FILE=<path>` | **捕获 daemon 的 panic 栈**。默认 `child.Stderr = devNull`，崩溃时日志表现为「突然不写了」，无任何线索 |

`deliver()` 在 events channel 满时最多阻塞 1s 后**丢弃**事件，现已记录 `deliver dropped` 告警——丢一个 EventResult 等于用户的回复凭空消失。

---

## 6. 待定事项

### 6.1 共享层 usage 语义：累加 → 覆盖（**已知缺陷，未修**）

F-49 §1.2 已把 4 个 token 字段定义为「当前上下文占用」（compaction 归零、`CostUSD` 保留），但 `AgentSession.AccumulateUsage`（`internal/chatsession/agentsession.go`）实现上是**跨轮累加**。**2026-08 update**:`AccumulateUsage` 已被后续 single-shot 重构删除,bridge 报的 `ResultEvent.Usage` 直接透传到 `SessionContext.Usage`,runtime 不再累加也不在 compaction 时归零 —— 下一轮 EventResult / EventDone 自带新的(压缩后)context 快照。详见 [`../wip/usage.md`](../wip/usage.md) 重构清单。

而 bridge 报的是「最后一次 API call 的快照」，累加 N 轮就虚高 N 倍——正是 F-49 §0.1 抱怨的 "1.37M mystery number" 的根因，compaction 归零只是把问题往后推。

**正确做法是覆盖。** 该缺陷同样存在于 claudecode（`stream.go` 的 `decodeUsage` 取的也是最后一次调用），所以应在共享层统一修，不是 pi bridge 的事。

> 现状：pi 侧产出的已是正确的单点快照，共享层改成覆盖后立刻生效。

### 6.2 footer 分母 / 百分比（未落地）

要表达「上下文满没满」需要分母。数据源已确认存在，只是被丢弃：

- pi 的 `Model` 对象含 `contextWindow`（如 `200000`）——我们的 `getStateModel` 只取了 `id`/`name`/`provider`
- pi 还有 `get_session_stats` 命令，直接返回 `contextUsage: {tokens, contextWindow, percent}`，文档称其为「压缩和 footer 显示实际使用的估算」，是最权威的来源（代价：每轮一次额外 RPC，且 claudecode 无对应命令，跨 bridge 不统一）

F-49 §6 把这项列为「后续 PR 单独讨论」。倾向方案：`getStateModel` 加 `contextWindow` → `InitEvent` → `AgentSession` → `SessionContext` → footer 渲染 `7.8k / 200k · 4%`；claudecode 无来源则留空降级。

### 6.3 acp bridge 未对齐 `EventText` 契约

`internal/bridge/acp/session.go` 仍逐 chunk emit `EventText`，按 F-52 定义的粒度契约属违约状态。未在生产暴露（acp 目前无活跃用户），且它的块边界信号是 `stopReason` 而非 `text_end`，需单独设计。`internal/agent/agent.go` 的 `EventText` 注释已标注此事。

### 6.4 `sendViaLark` 无出站日志（排查盲区）

`logOutgoing` 覆盖 `send_text` / `add_reaction` / `delete_reaction` / `update_message` / `send_card` / `patch_message` 六条路径，但 `sendResultAsReply → sendContent → sendViaLark → ReplyInBoth/ReplyInChat` **一行日志都不打**。

这直接导致一次线上排查误判——「没有 outgoing 日志」被当成「result 被丢弃」，而实际上那条路径本来就不留痕。建议补齐对齐其余六条。

### 6.5 Extension UI 未打通

`extension_ui_request` 目前在 `readPump` 里自动回 `cancelled`（在 `translate()` 之前处理，`continue`），不转发到 channel。完整 schema（select / confirm / input / editor / notify / setStatus / setWidget / setTitle）已在 `protocol.go` 留了 raw 字段。

### 6.6 `/abort` 未实现

pi 有 `abort` 命令，nightme 目前用 `/kill`（杀进程）替代。

### 6.7 生产端到端验证未闭环

F-52 的单测（42 个 Translate 用例）、`-race`、以及真实 pi 二进制 e2e（fresh / resume / 真实 workspace 三组对照）全部通过，日志也确认生产环境 `EventResult` 正常产出并成功送达 `ch.Send`（返回 nil）。

**但飞书端最终渲染结果尚未由人工确认。** 见 §6.4——该路径无日志，无法从服务端证实。合并后需目视验证：发一句 "hi" 应得到**一条 📝 卡片**，footer 💰 行有非零 token。

---

## 7. 抓真实 wire 样本

单测用 JSON fixture，但 provider 差异（§4）只能靠真机发现。两个办法：

**A. 真实二进制 e2e**（本机装了 pi 即自动跑）

```bash
go test ./internal/bridge/pi -run Real -v
```

`session_real_test.go` 靠 `exec.LookPath("pi")` 跳过，无 build tag。

**B. 生产 debug 日志**

```bash
NIGHTME_LOGGING_LEVEL=debug NIGHTME_STDERR_FILE=/tmp/nm-stderr.log nightme restart
```

被忽略的事件会带完整 raw 打出来，可直接复制成 fixture。
