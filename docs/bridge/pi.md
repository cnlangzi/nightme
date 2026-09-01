# Pi Coding Agent Bridge

## A1. F-32: Pi Coding Agent Bridge (RPC 模式)

> **Source**: `../bridge/pi.md`


> **Depends on**: F-09 (Agent abstraction), F-19 (CLI Bridge), F-21 (Agent Modes), F-24 (Claude Code Bridge 模式), F-27 (ChatSession), F-28 (`/use`), F-29 (AgentSession pool)
> **Related**: [`../bridge/claude.md`](./../bridge/claude.md), [`../bridge/cli-transport.md`](./../bridge/cli-transport.md), [`F-chat-session.md`](./F-chat-session.md)

## 0. 摘要

把 [`@earendil-works/pi-coding-agent`](https://www.npmjs.com/package/@earendil-works/pi-coding-agent)（Mario Zechner 的 [earendil-works/pi](https://github.com/earendil-works/pi)，旧路径 `badlogic/pi-mono`）作为 nightme 的第三种内建 agent。bridge 直接 spawn `pi --mode rpc` 长驻进程，通过 Pi 自定义 JSONL 协议驱动；**不**依赖第三方 `pi-acp`，**不**复用现有 ACP wire 或 PTY transport。

实现只做 RPC 核心：`get_state` + `prompt` + 事件流 + `agent_settled` 终态；图片走 `prompt.images`；不打通 Extension UI 飞书回复，不实现 `/abort`、steer/follow-up、动态模型/思考切换、session switch/fork/clone。

## 1. 背景与决策

### 1.1 为什么选 RPC 而不是 JSON/ACP

调研 2026-08-03 锁定的事实：

- pi **不**原生支持 ACP（官方 monorepo `earendil-works/pi` 全仓 grep `agent-client-protocol` 零命中；`packages/` 没有 acp 子包；官方 IDE 入口是自研 `pi --mode rpc`）。
- 第三方 `pi-acp` (svkozak) 仍处 0.0.x，与 nightme 当前 ACP 实现的 wire / transport / 错误模型不兼容，**且** 现有 ACP bridge 走 PTY 物理通道（合并 stdout/stderr、回显、banner 污染），与 Pi 严格 LF 分帧相冲突。
- `pi --mode json -p`（cc-connect 初版路线）每发一条 prompt 重启一个进程，只能 fire-and-forget 一次 turn；没有 `agent_settled` 之上层 turn 边界、没有 abort、没有 Extension UI、没有长驻模型切换。
- `pi --mode rpc`（cc-connect 后补，PR #1440，2026-07-04）是官方自研的 stable 长驻 JSONL 协议，命令/响应/事件三分明确，并发安全、turn 边界（`agent_settled`）、中止（`abort`）、Extension UI（`extension_ui_request`/`extension_ui_response`）齐备。

**决策**：直接接 `pi --mode rpc`，避免 ACP 兼容成本与第三方 `pi-acp` 的版本漂移。ACP bridge 留作兜底不删除；新包与 ACP wire 完全解耦，等两边独立验证后再考虑抽公共 JSONL transport。

### 1.2 为什么首期范围保守

nightme 现有 FSM 假设"一个 AgentSession = 一个进程"；pi RPC 进程是 long-lived，但**一个进程承载多轮 turn**。两套生命周期需要解耦：

- **turn**：`prompt` 提交 → 若干 event → `agent_settled`（可能跨多次 retry / compaction / follow-up）。
- **process**：进程从 spawn 到 `cmd.Wait()` 返回；只有 process exit 才关闭 `Events` channel。

`agent_settled` 与 `EventDone` 对应，**不**关闭 channel。现有 `internal/chatsession/readpump.go` 在 `EventDone` 切 Idle/flush queue 并继续循环，正好支持长驻多轮。设计选择 **复用现有 FSM**，而不是为 pi 新增 `EventTurnEnd` 单独 kind（避免 EventKind 多义扩散；契约在 EventDone 上加注释与单测锁定）。

首期不实现：

- `/abort` + `agent.Abortable`：chat FSM 暂无 Aborting 态；加 `/close` 已能逃生；后续以可选接口追加。
- `extension_ui_request` → 飞书卡回复闭环：当前 feishu card action 只 toast，不发 decision；扩展 UI 表本身需要 keyed-by-id 的 request map，先把"两端之间加 channel"。
- `steer` / `follow_up`：复用现有 `InputBuffer` 在 `EventDone` 切 Idle 后 flush，避免给 pi 加双语义。
- `set_model` / `set_thinking_level` / `cycle_*`：MVP 留给用户在 pi CLI flag 配置；后续按需开 `/use pi --model=…`。
- `switch_session` / `fork` / `clone`：ChatSession 与 AgentSession pool 已经是 `(agent, cwd)` 维度，未要求 per-session switch；现有 `/use pi` + 换 cwd 已覆盖。
- ✅ **后续在 F-34 启用**：`new_session` —— F-34 让 `/new` 暴露给 IM 入口，bridge 实现为 `rpc.requestAsync("new_session", nil)`；F-32 当时 defer 的原因是没有 runtime 入口，现在补上。详见 [`F-chat-session.md`](./F-chat-session.md) §3.2.2。

## 2. Wire Protocol（来自 pi 官方 `docs/rpc.md`）

### 2.1 总览

- **Transport**：真实 stdio pipes（**非 PTY**）。stdout 仅含合法 JSONL（`\n` 分隔）；stderr 是日志。
- **Command**：客户端 → stdio。**不是** JSON-RPC 2.0；envelope 是 `{"id":<string|int|null>, "type":<command name>, ...command-specific fields}`，每行一个对象。
- **Response**：服务端 → stdout。`{"id":<...>, "type":"response", "command":<name>, "success":true|false, "data":<obj|absent>, "error":<string|absent>}`。
- **Event**：服务端 → stdout。`{"type":<event name>, ...event-specific fields}`。event 不携带 id（`bash_execution_update` 例外，携带其 RPC command id）。

### 2.2 Command 表（首期仅用前三个）

| command | 方向 | 字段 | 用途 |
|---|---|---|---|
| `get_state` | C→S | `type` | 握手 / 拉初始 model + sessionId |
| `prompt` | C→S | `type`, `message` (string), `images` ([ImageContent]), `id` (可选) | 发用户 turn |
| `extension_ui_response` | C→S | `type`, `id` (与 request id 匹配), `value` \| `confirmed` \| `cancelled` | **首期不发送**；本版本对 `extension_ui_request` 自动回 cancelled |
| `abort` | C→S | `type` | **首期不发送**；用 `/close` 替代 |

`ImageContent` shape：`{"type":"image", "data":<base64>, "mimeType":<MIME>}`。

### 2.3 Event 表（映射到 `agent.AgentEvent`）

| pi event | payload 关键字段 | 目标 `AgentEvent` | 备注 |
|---|---|---|---|
| `agent_start` | — | (log debug) | 不入 events |
| `agent_end` | `willRetry` | (log debug) | **不**作为 turn 终态 |
| `agent_settled` | — | ⟪F-52⟫ **`EventResult{...}` → `EventDone{Reason:"settled"}`** | turn 终态；**不关 channel**。result 必须排在 done **之前**——runtime readpump 在 EventDone 切 Idle 并 flush 队列。若本 turn 未观察到任何事件（out-of-band settle，如 fire-and-forget 压缩），**只发 EventDone**，不发 result——否则用户会收到一张莫名的「Done.」卡片。见 [F-52 §2.4.3](./../bridge/pi.md)<br><br>**用户视觉行为**：在 text+tool 的 turn 里，model 有时会在最后一次 tool call 之后**不再发一句收尾的话**。此时 pi 的最终 `message_end.content[]` 只有 thinking / toolCall，没有 text block——bridge 的 `finishTurnLocked` 三级 fallback 全空，落 `emptyReplyFallback = "Done."`。Channel 把它渲染成 📝 卡片文字。Rolling log 里 user 已经通过 `EventAgentText` 看到了之前的 narration（tool 边界 flush），所以这不是 agent 的真实回复，是占位符。**这是 by-design**——保证 `OutboundMessage.Usage` 不丢(StatusBar token 行)。不同 model 触发频率不同:sensenova-flash-lite 几乎不出现,Anthropic Claude / GPT 短确认回复更容易出现。 |
| `turn_start` / `turn_end` | — | (log debug) | 暂不消费 |
| `message_start` | `message` | (log debug) | — |
| `message_update` `{assistantMessageEvent:{type:"text_start"}}` | `contentIndex` | ⟪F-52⟫ no-op（重置该 index 的缓冲） | 防御:漏掉的 text_end 不会把上一块尾巴串进来 |
| `message_update` `{assistantMessageEvent:{type:"text_delta"}}` | `delta`, `contentIndex` | ⟪F-52⟫ **no-op（累加进缓冲）** | 按 contentIndex 分桶;不再逐 token emit |
| `message_update` `{assistantMessageEvent:{type:"text_end"}}` | `contentIndex` | ⟪F-52⟫ no-op（该块转入 pendingText） | 等待 tool 边界 flush 或 turn 终态 |
| `message_update` `{assistantMessageEvent:{type:"thinking_delta"}}` | `delta` | ⟪F-52⟫ **no-op（累加进 thinkBuf）** | — |
| `message_update` `{assistantMessageEvent:{type:"thinking_end"}}` | — | ⟪F-52⟫ **`EventText{Text: "[思考] " + 全文}`** | 复用 claudecode 前缀约定。思考在**自己**的边界 flush,不进 pendingText——它是另一个渲染面(💭 vs 💬),且绝不能落进 EventResult |
| `message_update` `{assistantMessageEvent:{type:"toolcall_start"}}` | `toolCallId`, `name`, `partial.arguments` | no-op | `tool_execution_start` 是唯一 EventToolStart 来源；此事件提前到达以解析 partial args，但渲染层等 canonical 事件 |
| `message_update` `{assistantMessageEvent:{type:"toolcall_end"}}` | `toolCallId`, `toolCall.{name, arguments}` | no-op | 同 toolcall_start：canonical 事件是 tool_execution_start |
| `tool_execution_start` | `toolCallId`, `toolName`, `args` | ⟪F-52⟫ **`EventText{pendingText}`（若非空）→** `EventToolStart{ToolUseID: toolCallId, Name: toolName, Input: raw(args)}` | canonical "tool starting" 事件。**这个 flush 点是 F-52 的关键**:它让「一轮一次 OutResult」和「中途能看到进度」不冲突,并且因为 flush 会清空 pendingText,最终 EventResult 永不重复已发出的文本 |
| `tool_execution_update` | `toolCallId`, `partialResult` | (log debug) | MVP 不暴露 partial；保留接口 |
| `tool_execution_end` | `toolCallId`, `result`, `isError` | `EventToolEnd{ToolUseID: toolCallId, Name: <from start>, Output: stringified(result), IsError: isError}` | — |
| `message_end` `{message:{role:"assistant", stopReason}}` | `message.{content[], usage, stopReason}` | ⟪F-52⟫ **no-op（记录 `lastMessageText` / `stopReason` / `lastUsage`）** | **不切 FSM**。一个 turn 可含多条 assistant message(text → toolCall → toolResult → 下一条),原实现逐条 emit 会得到「一条 message 一个 result」而非「一轮一个」。usage **覆盖**不累加:每次 API call 的 input 侧已含全部历史,是上下文占用**快照**,累加会按调用次数倍数虚高 |
| `message_end` `{message:{role:"toolResult"}}` | `toolCallId`, `content[]` | (log debug；合并入 tool_execution_end) | — |
| `message_end.usage`（同 row 触发） | `usage.{input,output,cacheRead,cacheWrite,totalTokens,cost.*}` | **co-located on `EventResult.Usage`** | 不再独立 emit `EventUsage` 事件——reasoning: 见 [F-49 §1.9 bridge 抽象统一](./../channel/feishu-rendering.md) + `internal/gateway/translate.go` 注释。runtime 从 `ev.Result.Usage` 一次取齐；`AgentSession.RecordUsage()` 由 runtime handler 调用，不依赖独立 Event
| `compaction_start` | `reason` | **（F-49: 屏蔽,`return nil, nil`）** | runtime 不需要瞬态信号;bridge 自己消化协议差异 |
| `compaction_end` | `reason`, `result.aborted` | **（F-49）** `EventCompaction`（无 Subtype 字段） | 一个完整的压缩周期 = runtime 收到一条 `EventCompaction` |
| `extension_ui_request` | `id`, `method` (select/confirm/input/editor/notify/setStatus/setWidget/setTitle), … | (log warning) | **首期**：自动回 `extension_ui_response{id, cancelled:true}`；不向 ChatSession 发 EventPermission |
| `extension_error` | `extensionPath`, `event`, `error` | (log warning) | 不杀 session |
| `auto_retry_*` / `summarization_retry_*` | — | (log debug) | 不暴露 UI |
| `bash_execution_update` | `id`, `delta` | (log debug) | bash 子命令 stream；MVP 不专门渲染 |
| 未知 type | — | (log debug) | **不**发 EventError；session 继续 |

### 2.4 `get_state` 响应 `data` 字段

```json
{
  "model": {"id": "...", "name": "...", "provider": "...", ...} | null,
  "thinkingLevel": "off|minimal|low|medium|high",
  "sessionId": "abc123",
  "sessionName": "...",
  "sessionFile": "/path",
  "messageCount": 5,
  "pendingMessageCount": 0,
  ...
}
```

bridge 只用：`sessionId`、`model.id`、`model.name`（→ `EventAgentConnected.SessionID` / `EventAgentConnected.Model`）。`sessionFile` 等文件路径字段**不**进 bridge —— `--session-id <id>` 在 spawn 时已经把文件名协商这层折过去了，不再二次引入 path abstraction（OOB 设计见 §12.2 P1 备注）。

## 3. 进程与生命周期

### 3.1 Spawn

```
exec.CommandContext(ctx, "pi",
  "--mode", "rpc",
  "--approve",
  "--no-themes",
  "--offline",
)
cmd.Dir = cfg.Workspace
cmd.Env = append(append(os.Environ(), cfg.Env...), "PI_TELEMETRY=0") // defensive
cmd.Stdin  = pipe
cmd.Stdout = pipe
cmd.Stderr = pipe
cmd.Start() → PID
```

### 3.1.1 Headless 启动契约

NightMe 是 Pi 唯一的交互方：没有人可以操作 Pi 的终端或 TUI，用户在 NightMe 启动前已经完成 Pi 配置，session 运行期间也不依赖 Pi 动态安装或重新加载 package。因此 RPC 进程默认使用一组 headless 参数：

| 参数 | 作用 |
|---|---|
| `--approve` | 本次运行直接信任项目目录，加载项目本地 `.pi` 配置、extension、skill、prompt、theme 和 `SYSTEM.md` / `APPEND_SYSTEM.md`；不写入用户 trust store |
| `--no-themes` | 禁用未被 RPC 使用的 TUI theme discovery/loading |
| `--offline` | 禁止启动时版本检查、model catalog 刷新和未安装 npm/git package 的自动安装；已安装的本地资源继续工作 |
| `PI_TELEMETRY=0` | 禁止安装/更新 telemetry 与 provider attribution header |

这些参数**不能**等价替换为 `--no-extensions`、`--no-context-files`、`--no-skills`、`--no-prompt-templates` 或 `--no-session`。后者会改变 NightMe 依赖的项目 context、工具能力和 session resume 语义。

`--offline` 依赖前置配置不变量：用户在 NightMe 调用 Pi 前已经安装好需要使用的 extension/package。离线模式只跳过网络安装，不应把缺失 package 静默当作已加载资源。

**严禁 PTY**。参照 `internal/bridge/claudecode/session.go:71-130` 的真实 pipe 模式，但去掉 `--print --input-format stream-json --output-format stream-json --permission-mode ...` 那一组 flag。

### 3.2 Goroutine 拓扑

```
session.Spawn()
  ├─ goroutine A: readPump (stdout scanner, 解 frame → 分流 response / event / extension_ui_request)
  ├─ goroutine B: drainStderr (stderr Read, debug log, 保留有界尾部用于错误)
  ├─ goroutine C: lifecycle (cmd.Wait, capture exitCode, 失败所有 pending, 关闭 events)
  └─ goroutine D: handshake  (sync, 启动时 GetState 超时 10s)
```

**唯一 Wait / events close owner = goroutine C**。所有 Close 路径（包括 graceful shutdown、error、异常退出）最终通过 C 写 `exitCodeSink` 并 close events。`Close()` 不直接关 events，由 C 收尾。**这修正了 claudecode 注释掉的 watchdog race，并避免 ACP `readPump` 与 `Close` 双重关闭的同形问题**。

### 3.3 状态机

| 时刻 | 行为 | channel 状态 |
|---|---|---|
| `t0` cmd.Start | 启 A/B/C/D | open |
| `t1` GetState 成功 | 在 C 或 A 中 emit `EventAgentConnected{SessionID, Model, AgentName:"pi", Workspace, Branch}` | open |
| `t1'` GetState 失败 / 超时 | 失败 pending + emit `EventError` + 通知 C 关闭 | 关闭 |
| `t2` SendBlocks → `prompt` | 写 stdin 等待 `response` ack | open |
| `t3` `prompt` ack 收到 | 等待 stream events；`SendBlocks` 返回 nil | open |
| `t3'` `prompt` ack `success:false` | `SendBlocks` 返回 `ErrPiRejected`；不切 FSM | open |
| `t4` stream events | 翻译为 AgentEvent | open |
| `t5` `agent_settled` | emit `EventDone{Reason:"settled"}` | **仍 open** |
| `t6` 下一轮 user msg | 重新 `prompt` | open |
| `t7` process exit (正常) | C: close events | 关闭 |
| `t7'` 异常 exit / scanner 错 / pipe 断 | C: emit `EventError` → close events | 关闭 |
| `t8` Close() | cancel ctx；stdin flush + close；SIGINT → 2s → SIGKILL；等 C | 关闭 |

`internal/chatsession/readpump.go:174-203` 在 channel close 时 `as.SetExited(0)`，与 `t7`/`t7'` 路径一致。

### 3.4 Close 幂等

`sync.Once` 守护 Close。Close 内：

1. `s.stdinMu.Lock(); _ = s.stdin.Flush(); s.stdinMu.Unlock()`（不主动 close stdin pipe — 留给 process 自然 EOF / SIGINT）。
2. `cmd.Process.Signal(os.Interrupt)`。
3. 等 2s on `exitCodeSink`；超时 `cmd.Process.Kill()` + 再等。
4. 返回；C goroutine 自然 close events。

多次 Close 调用安全：`sync.Once` 后只跑一次；后续直接返回 nil。

## 4. request 关联与并发

### 4.1 写入：单行原子

```go
type rpcClient struct {
    writeMu sync.Mutex
    writer  *bufio.Writer
}

func (c *rpcClient) writeLine(payload []byte) error {
    c.writeMu.Lock()
    defer c.writeMu.Unlock()
    if _, err := c.writer.Write(payload); err != nil { return err }
    if err := c.writer.WriteByte('\n'); err != nil { return err }
    return c.writer.Flush()
}
```

**严禁** `bufio.Scanner` 的默认 64 KiB 行上限。读侧把 buffer 提到 4 MiB（`bufio.Scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)`），超出显式 `ErrFrameTooLarge` 关闭 session；写侧 base64 图片可超过 4 MiB，因此 writer 用全缓冲 + 一次 Flush；超大行通过 `io.Pipe` 形式让单条 `prompt` 超过 reader 限制时仍可由 write 完。

### 4.2 响应关联

```go
type rpcClient struct {
    mu       sync.Mutex
    pending  map[string]chan responseEnvelope   // for get_state
    inflight string                              // current prompt id, 或 "" 表示空闲
}
```

- 启动时 `get_state` 分配 id `"boot"`，timeout 10s。
- `SendBlocks` 分配新 id，登记到 `pending` 等待 ack；ack 到达后从 pending 删除并允许下一 prompt。**只等 ack，不等 `agent_settled`**。
- 第二次 `SendBlocks` 在 ack 尚未到达时直接返回 `ErrTurnBusy`。
- 关闭 / 超时 / 异常时遍历 pending 写 `ErrSessionClosed`。

### 4.3 事件分流

`readPump` 每帧解出 envelope 后按字段分流：

```
if env.Type == "response"  → pending[env.ID] <- envelope
if env.Type == "extension_ui_request" → 记录 id, 在 SendPermission 路径之前自动 cancelled
if env.ID != "" && env.Type == "" → 防御：丢弃
else → 走 translate goroutine
```

`translate` 路径与 readPump 解耦：readPump 只管解析与分流，翻译放独立 goroutine 防止 scanner 被大 payload 阻塞。

## 5. Event 映射实现细节

### 5.1 复用与差异

- 现有 `internal/agent/agent.go` 的 `AgentEvent` / `EventKind` **不动**。
- `EventDone.Reason` 字段当前**不存在**；需在 `DoneEvent` 上加一个 `Reason string` 字段（兼容旧值零），并 `EventDone{Reason:"settled"}` 标明 turn-end。`internal/agent/agent.go:204-211` 同步注释，claudecode/pty 保持默认 `Reason:""`。
- 现有 `internal/gateway/translate.go` 的事件翻译不需为 pi 改逻辑；`EventAgentConnected` / `EventResult` / `EventUsage` / `EventCompaction` / `EventToolStart` / `EventToolEnd` / `EventText` / `EventPermission`（本期不通过此路径走）/ `EventError` 都已存在。

### 5.2 turn 终态语义（核心改动）

在 `internal/agent/agent.go:460-466` 把 `Events()` 的注释从 "channel is closed by the implementation after EventDone or a terminal EventError" 改为：

并在 `internal/chatsession/readpump_test.go` 加回归测试：`EventDone` 后下一个 event 仍能到达且 pump 不退出；仅在 channel close 时 `as.SetExited`。

claudecode bridge 仍然"每 turn = process"，但因为 `EventDone` 之后没有新事件，所以现有行为不变。

### 5.3 assistant text / thinking 聚合

pi 的 `text_delta` / `thinking_delta` 已经是流式；`text_end` / `thinking_end` 不需要专门处理。`EventText` 每条 delta 即发。thinking delta 携带 `[思考] ` 前缀与 F-24 一致。

最终 `message_end`（role=assistant）若其 content 是非空 text 且上一条 `text_end` 之后无新 delta，转 `EventResult{Text: content, Subtype: stopReason}`；否则跳过避免重复。

## 6. SendBlocks 编码

| `ContentBlock` | 处理 |
|---|---|
| `ContentText` | 顺序拼到 `prompt.message`（多块以 `"\n"` join，与 claudecode 一致） |
| `ContentImage` | 读 file → base64 → 验证 `MediaType` ∈ `image/{png,jpeg,gif,webp}` → 追加 `{"type":"image","data":"...","mimeType":"..."}` 到 `prompt.images` |
| `ContentFile` | **首期**：追加 `"\n[file: <absolute path>]"` 到 `prompt.message`（与 claudecode 退化为文本一致）；不报错；不读取文件内容 |

`prompt.message` 为空时返回 nil（与 F-24 `SendText` no-op 行为一致）。

`images` 单个文件 base64 后 >10 MiB 返回 `ErrImageTooLarge`；不静默截断。

并发：`SendBlocks` 调用前取 turn-lock；持锁 → 写 pending → 等 ack；释放。任何 panic / 超时由 lifecycle goroutine C 收尾。

## 7. 用户流

```yaml
# configs/nightme.example.yaml 新增
agents:
  - name: pi
    bridge: pi
    command: pi
```

安装官方包：

```bash
npm install -g @earendil-works/pi-coding-agent
# or
pnpm add -g @earendil-works/pi-coding-agent
```

`nightme agents` 表格显示 `pi / pi / --mode rpc --approve --no-themes --offline`；`nightme config` → Agents 可选为 primary；飞书会话中 `/use pi` 走现有 `agentsession.Spawner`，经 `Detect()` (`exec.LookPath("pi")`) → `Start()`（`exec.Command("pi", "--mode", "rpc", "--approve", "--no-themes", "--offline")` + 真实 pipes）→ `newSession(...)`。一次性任务使用 `pi --mode json --approve --no-themes --offline -p <prompt>`。两者都继承 `PI_TELEMETRY=0`。

## 8. 已知限制（首期明确）

| 能力 | 状态 |
|---|---|
| 文本 turn | ✅ |
| 图片 turn（`prompt.images`） | ✅ |
| 文件附件 | ⚠️ 退化为 `[file: <path>]` 文本 |
| thinking | ✅（`[思考] ` 前缀） |
| tool_call / tool_result | ✅（`EventToolStart` / `EventToolEnd`） |
| usage（input/output/cache/cost） | ✅ |
| compaction | ✅（F-49: `compaction_end` emit `EventCompaction` × 1;`compaction_start` 屏蔽） |
| session state → EventAgentConnected | ✅（首次 `get_state`） |
| 多轮长驻 | ✅（`agent_settled` 不关 channel） |
| `/close` 终止 | ✅（Close 路径） |
| **跨进程 Resume（daemon 重启续同会话）** | ✅（spawn-time `--session-id <id>`；`AgentSession.ResumeID` 由 `get_state.sessionId` 填入 → 写入 `agent_sessions.json` → 下次 spawn 翻译成 `--session-id`；与 claudecode 的 `--resume <id>` 同 bridge contract 各自翻译） |
| `/abort` 单 turn 取消 | ❌ 用 `/close` 替代 |
| Extension UI 飞书回复 | ❌ 自动 cancelled，不发卡 |
| steer / follow_up | ❌ 走 InputBuffer flush |
| 运行时切 model / thinking | ❌ 改 pi CLI flag |
| 运行时切 session（mid-process switch_session） | ❌ 当前走 spawn-time 注入即可覆盖 daemon 重启场景；mid-process switch 按需另开 |
| session fork / clone | ❌ |
| 真实 ACP wire / pi-acp | ❌ |

## 9. 验收矩阵

### 9.1 协议单测（`internal/bridge/pi/rpc_test.go`）

- 每帧一行 JSONL；CRLF / 多 frame / partial read / `U+2028` / `U+2029` / 大于 4 MiB 行。
- 写入多 goroutine 仍原子单行。
- 关联：成功 ack、ack `success:false`、late response、unknown id、timeout、关闭 fail-all-pending。
- 输出端：base64 + `\n` flush。

### 9.2 事件翻译（`internal/bridge/pi/translate_test.go`）

- 表驱动：上文 §2.3 全表每行一个 case。
- `agent_settled` 恰好 emit 一次 `EventDone{Reason:"settled"}`。
- `agent_end` / `turn_end` 不 emit 终态。
- `extension_ui_request` 自动回 cancelled 并 log warning。
- 未知 type 不杀 session。

### 9.3 真实子进程（`internal/bridge/pi/session_test.go`）

`internal/testdata/pi_mock.sh`（或 `.go`）：接收 stdin 模拟 pi，发出 get_state / 多 prompt / events；`PATH` 注入法同 `claudecode_test.go:741-756`：

- Start handshake、PID、workspace / env / args 正确。
- 两轮 prompt 复用同一 PID。
- 图片编码正确写出。
- stderr drain 持续；非零 exit 时 stderr 尾部进入 EventError。
- 异常退出 emit `EventError` 后关 channel。
- Close 幂等；Close 时 Write 立即返回错。
- `cmd.Wait` 唯一调用方（无 race detector 报警）。

### 9.4 ChatSession 回归（`internal/chatsession/readpump_test.go`）

- 多个 `EventDone` 后 pump 仍读下一个 event。
- 关闭后 `as.SetExited` 触发。
- queued message 在 `EventDone` 后 flush 到同一 handle。

### 9.5 CLI / 全库

- `go test ./internal/bridge/pi ./internal/chatsession ./cmd/nightme`
- `go test -race ./internal/bridge/pi ./internal/chatsession`
- `go test ./...`
- 安装官方 pi 后手工 smoke：
  - `nightme agents` 列出 `pi / pi / --mode rpc --approve --no-themes --offline`。
  - daemon 启动，`/use pi`；多轮文本 + 一次图片 + 一次 tool call 完整。
  - footer 显示 session id / model / tokens。
  - `/close` 回收子进程。

## 10. Critical Files

新增：

- `docs/bridge/pi.md`（本文件）
- `internal/bridge/pi/agent.go`
- `internal/bridge/pi/protocol.go`
- `internal/bridge/pi/rpc.go`
- `internal/bridge/pi/session.go`
- `internal/bridge/pi/translate.go`
- `internal/bridge/pi/*_test.go`
- `internal/testdata/pi_mock.sh` + `pi_mock.py`（或纯 Go fixture）

修改：

- `cmd/nightme/agents.go` — `agent.Builtins.Register(pi.New("pi", "pi", nil))`
- `cmd/nightme/agents_test.go` — 列表断言增加 `pi`
- `internal/agent/agent.go` — `DoneEvent.Reason` 字段 + `Events()` 注释澄清
- `internal/chatsession/readpump_test.go` — turn 完成后 pump 继续的回归
- `configs/nightme.example.yaml` — `pi` 示例
- `docs/FEATURES.md` — 加入 F-32 索引

参考（不修改）：

- `internal/bridge/claudecode/session.go` — pipe、stderr、writeLine 模式
- `internal/chatsession/spawn.go` — Detect / Start wiring
- `internal/chatsession/readpump.go` — EventDone 后 Idle + queue flush
- `internal/gateway/translate.go` — 现有 AgentEvent 渲染

## 11. 未来工作（明确 deferred）

1. **Extension UI → 飞书**：在 bridge 内 keyed-by-id `uiPending map[string]chan string`；补 Feishu card action → `AgentSession.SendPermission(option)` 回环（需要 gateway 加 pending permission 列表与 ChatSession 的 lookup）。
2. **`/abort` + `agent.Abortable`**：可选接口 `Abort(ctx context.Context) error`；ChatSession 增 `/abort` 路由，readpump 在 abort 后把当前 turn 的 userMsgIDs 标 `StateError`。
3. **`/use pi --model=…`**：把 `/use` 多余 args 透传到 `agent.StartConfig.Args`（修 `chatsession.go:496` 写死的 `nil`），bridge 把 `--model` 转 `set_model` 命令。
4. **steer / follow_up**：在 `EventDone` 前若 InputBuffer 收到新 user msg，改用 `{"type":"steer","message":"..."}` 而非排队等下轮。
5. **真实 ACP 桥抽取**：等 ACP 修掉当前 `requestAsync` 丢响应 / `tool_call_update` 误当 ToolEnd / PTY 通道问题，与 pi 一起抽公共 JSONL transport package。

---

---

## 12. 实施记录（2026-08-06 — task `T-pi-bridge-align`）

对照实现做了一次 gap 审计，落地 P0 / P1 / P4 三组改动；P2 / P3 仍按 §11 deferred。

### 12.1 P0 — `EventToolEnd` Name + Args 回填（修 `🔧 tool → N bytes` 渲染 bug）

**问题**：

- `OutToolStart` 已正确填 Name/Args → Feishu call 行 `● bash(...)` 正常
- `OutToolEnd` 此前**只**填 ID/Output/Err → `toolName()` fallback `"tool"` → result 行 `🔧 tool → N bytes`，类型感知摘要失效

**根因**：`pi/translate.go` 的 `tool_execution_end` case 当时只用 `ToolCallID/Result/IsError`，丢弃 Name/Args。Pi 的 wire 在 end 事件上**只**回传 `toolName`（不带 args）——args 仅出现在对应的 start 事件。

**修法**（claudecode `pendingTools` 同构，但更小）：

1. `protocol.go`：`toolExecutionEnd` 加 `ToolName string`（wire 已有，code 漏解）
2. `translate.go`：translator 加 `pendingTools map[string]pendingTool`（Name + raw Args），`tool_execution_start` 记录，`tool_execution_end` 弹出（pop 后 delete 防泄漏），`agent_settled` 清空（防止 turn-间 orphan）
3. 兜底：若 end 无对应 pending（orphan end / 早于 start），用 wire 上的 `toolName`，`Args` 留空（renderer 显示 "(no args)"，优于错挂历史 args）

**测试**（`translate_test.go` + `agent_test.go` 全绿）：

- `TestTranslate_ToolExecutionEnd_FillsNameAndArgs` — start→end 关联；断言 `Name == "bash"` / `Args` 包含 `ls -la`
- `TestTranslate_ToolExecutionEnd_WireToolNameFallback` — orphan end：Name 用 wire `toolName`，Args 为空
- `TestTranslate_AgentSettled_ClearsPendingTools` — orphan 防御清理
- `TestBuildArgs_*` — argv shaping（不直接覆盖本 P0，但同一 PR 验证 buildArgs 调度不破）

### 12.2 P1 — Resume：桥接 Pi 的 `--session-id` CLI flag

**问题**：原 `pi.Agent.Start` 完全忽略 `cfg.ResumeID`，daemon 重启后 pi 永远 fresh。

**调研**：查 Pi 官方 `docs/rpc.md` + `pi --help` —— Pi **原生**支持 spawn-time resume：

| Flag | 用途 |
|---|---|
| `--session-id <id>` | 传入已知 sessionId；agent 加载该 session |
| `--session <path\|id>` | 按文件路径或部分 UUID 加载 |
| `--no-session` | 不落盘 |

`get_state` 响应里的 `sessionId` 就是稳定 ID。nightme 已有 `AgentSession.ResumeID` 字段 + 持久化路径（`agent_sessions.json`）——只需把 opaque `ResumeID` 在 spawn 时翻译成 `--session-id <id>`。

**修法**（5 行 `buildArgs` + 1 行 `Start`）：

1. `agent.go`：改 `buildArgs(extraArgs []string, cfg agent.StartConfig)` 接 cfg；当 `cfg.ResumeID != ""` → 追加 `--session-id <id>`（按约定放 argv 末尾；与 claudecode `--resume` 同位）
2. 持久化路径不动：`EventAgentConnected.SessionID` 已由 `get_state` 写入（`emitInit`，旧实现）；runtime `SetResumeID` 已存在

**测试**（`agent_test.go`，5 个 case 全绿）：

- 缺省：`{"--mode","rpc"}`
- 带 ResumeID：`..., "--session-id", "sess-abc-123"`
- Resume flag 在 cfg.Args 之后：用户传的 `--model google/gemini` 仍能在 grep 中看见

**`cfg.ResumeID` 是 opaque bridge contract**：claude 翻译成 `--resume <id>`，pi 翻译成 `--session-id <id>`。同一个字段，各自翻译 —— `internal/chatsession` 层无需感知差异。

### 12.3 P4 — 文档卫生（本节落地点）

- FEATURES.md F-32 row：`📝 设计阶段` → `✅ 已实现（核心）；Extension UI + /abort 仍 deferred`
- F-32 §2.3 EventUsage row：原文档分两行（`EventResult` + `EventUsage`），现合并 — pi 把 usage 与 text 共载在**同一** `message_end` wire event，runtime 从 `Result.Usage` 一次取齐（避免 buffer OutResult 等 EventUsage 的顺序 hazard）
- F-32 §3.2 / §4.3 handshake timeout 5s → 10s（与代码 `handshakeTimeout = 10 * time.Second` 一致）

### 12.4 仍 deferred（不进本 PR，按 §11）

- §11.1 Extension UI → 飞书卡闭环
- §11.2 `/abort` + `agent.Abortable`
- `switch_session` 中途切 session：现在用 spawn-time `--session-id` 已经覆盖 daemon 重启恢复场景；mid-process switch 是独立使用场景（用户明确切到一份老 session 做对比），按需再开
- Task 清单（F-38 已为 claudecode 实现；pi 侧 TaskCreate/TaskUpdate 协议等价物待确认）

---

## A2. F-52 — Pi Bridge 流式事件整合

> **Source**: `../bridge/pi.md`

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
- nightme：静默丢弃 ← 就是 Abstract/Concrete 边界规范

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

第 3 条**不是装饰**。`gateway.Translate` 会丢弃 `Text==""` 且 `IsError==false` 的 EventResult，而 runtime 是从**翻译后**的 OutboundMessage 上读 Usage 的——空文本会把这一轮的 token 数一起带走（抽象/具体 边界规范（空文本被丢弃））。本次不动共享层，所以由 bridge 侧保证 Text 非空，让 usage 100% 通过。

**用户视觉行为（"Done." 什么时候会出现在 IM 里）**：

- **不出现**：纯文本回复 / final message 有 text / `active=false`(out-of-band settle)
- **出现**：turn 内有 streaming text(`textDelivered=true`) + final `message_end.content[]` 没有 text block(只有 thinking / toolCall / 空)

channel 把 Text 渲染成 📝 卡片文字。Rolling log 里 user 已经通过 💬(EventAgentText)看到了之前的 narration；📝 只是为了让 OutResult 不被 gateway drop,从而保住 StatusBar token footer。这跟 cc-connect / openclaw 的 `EMPTY_REPLY_FALLBACK_TEXT` 同源——是协议适配层的最小占位策略。**不是 bug,是 by-design**。不同 model 触发频率不同:sensenova-flash-lite 几乎不出现,Anthropic Claude / GPT 短确认回复更容易出现。

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

但 `totalTokens` 在 pi 的 wire 上跟 breakdown 是**分开上报**的，完全可能出现 `totalTokens=0` 而 `input`/`output` 非零（合成消息、schema 变体）。这种情况下整块 usage 被静默丢弃——正是 抽象/具体 边界规范（空文本被丢弃） 那个 bug 的另一条路径。

改为逐字段对称检查（全零且 cost 为零才判定为空），与 `claudecode/stream.go:decodeUsage` 的做法一致——那边本来就是对称的，pi 这个 `Total` 门是唯一的例外。

回归锁：`TestTranslate_UsageSurvivesZeroTotalTokens`（`totalTokens=0` 但 breakdown 非零必须保留）+ `TestTranslate_UsageDropsAllZeroIncludingZeroCost`（全零仍须丢弃，避免 footer 渲染 "$0.00"）。

#### 2.4.3 `active` 守卫：空 settle 不出卡片

`agent_settled` 的语义是「整个 session-level run 结算」（pi `docs/rpc.md`），它也用于结算 out-of-band 路径（例如 fire-and-forget 的压缩）。若这类 settle 落在一个什么都没观察到的 turn 上，无条件发 EventResult 会往用户会话里塞一张莫名其妙的「Done.」卡片。

`turnState.active` 在正文 delta / 思考 delta / assistant message_end / tool start 任一发生时置位；`finishTurnLocked` 在未置位时直接返回 nil，只留 EventDone。

回归锁：`TestTranslate_UntouchedSettleEmitsNoResult`。

#### 2.4.4 `/new` 抑制窗口（code-review PLAUSIBLE，评估后确认为真并修复）

code-review 把这条标为 PLAUSIBLE 并跳过（理由：`session.go` 注释里写明是有意的锁策略权衡）。**复评后判定为真 bug**——注释解释的是「为什么不用 translatorMu 包住 translate()」，那个权衡确实成立；但它没有覆盖「重置之后、EventAgentConnected 之前」这段时间里到达的事件。

`session.New()` 的时间线：

```
new_session 响应到达
  ↓
turnState 重置                     ← 只清掉「已经累积的」
  ↓
get_state RPC（10s 超时）          ← readPump 全程在跑!
  ↓
deliverInitLocked → EventAgentConnected
```

而 `/new` **可以打断进行中的 turn**：`NewActiveAgentSessions`（`chatsession.go:1233`）没有任何 Busy/Idle 守卫，slash command 也不走 InputBuffer 排队。所以用户在长回复中途打 `/new` 时，旧 turn 仍在管道里的事件会落进**全新的** turnState：

- `message_end` 把 usage 盖到新会话上——直接污染「上下文占用」数字，还跟 `handleNew` 紧随其后的 `ResetCumulative()` 抢先后；
- `agent_settled` 把**已被放弃的回复**当作新会话的 result 卡片发出去。

修法：`beginReset()` / `endReset()` 取代原来的 `resetTurn()`。`beginReset` 在重置 turnState 的同时打开抑制窗口，`translate()` 在窗口内直接丢弃所有事件。

窗口内丢弃是**无条件正确**的：新会话还没收到任何 prompt，此刻到达的东西不可能属于它。命令响应走 response 分支，`extension_ui_request` 的自动 cancel 和畸形帧的 EventError 都在 `readPump` 里、在 `translate()` **之前**处理（`session.go:812-817`），三者都不受影响。

`endReset` 用 `defer` 挂在 `New()` 上，确保任何错误返回路径都不会把 session 永久静音。

回归锁：`TestTranslate_ResetWindowDropsAbandonedTurn`（窗口内的 text/usage/tool/settled 全丢，且新 turn 的 usage 是 42 而非旧的 9999）+ `TestTranslate_EndResetRestoresTranslation`（静音必须能恢复）。

### 2.5 usage 取最后一条快照，不累加

一轮里有多条 assistant message，每条带自己的 usage。**覆盖，不求和。**

理由：每次 Pi API call 报告的 input 侧（`input + cacheRead + cacheWrite`）**本身就已包含全部对话历史**，是当前上下文占用的**快照**。把一个多次调用的 turn 的快照相加，会按调用次数成倍虚高。

cc-connect 从另一个方向得出同样结论——它的 `handleAgentEnd` 倒序遍历 `agent_end.messages[]`，取最后一条 assistant 的 usage 作为 ContextUsage。

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
| `TestTranslate_SimpleTurn_SingleResult` | 无工具的一轮 = 恰好 `[result done]`，0 个 EventText（**§1.4 的直接回归锁**） |
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

---

## A3. F-54: Pi Bridge 的 ContextWindow Lookup + 删除死字段

> **Source**: `../bridge/pi.md`


> **Depends**: [`../bridge/pi.md`](./../bridge/pi.md) (get_state RPC), [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md) (X% 渲染), [`../bridge/pi.md`](./../bridge/pi.md) (decodeMessageUsage)
> **Related**: [`../bridge/claude.md`](./../bridge/claude.md) (claudecode 的 decodeUsage 行为作对照)

## 0. 摘要

**两件事**:

1. **删除死字段**: `agent.UsageEvent.ContextWindow int` —— 全 codebase 0 read / 0 write,claudecode `decodeUsage` 写入后只在同一函数内消费算 pct,从未穿出 struct 边界。footer / channel / runtime 全部读 `ContextWindowPct`,没人需要原始 `ContextWindow` 值。
2. **Pi 补全 X%**: Pi bridge 的 `decodeMessageUsage` 当前只填 `CostUSD`,**没填** `ContextWindowPct`(注释里自己承认 "omits the 'X%' segment for pi users until pi plumbing for context-window lookup lands")。从 `get_state.data.model.contextWindow` 拿,bridge-local 算 pct,直接填到 `UsageEvent.ContextWindowPct`。

**不动的契约**: `UsageInfo` / `UsageEvent` 字段表不变(只删 1 个死字段)。Footer 渲染不变。ClaudeCode bridge 行为不变(本来就报 X%)。

## 1. 背景与决策

### 1.1 现状

| Bridge | 报 X%? | 怎么报 |
|---|---|---|
| ClaudeCode | ✅ | `decodeUsage` 从 `modelUsage[<model>].contextWindow` 解出,同一函数算 pct 填 `out.ContextWindowPct` |
| Pi | ❌ | `decodeMessageUsage` 只填 `CostUSD`,pct 字段永远 0,footer 永远省略 X% 段 |

代码注释 `internal/bridge/pi/translate.go:809-811` 自己写明:

——这是 F-52 重构时遗留的"等 pi plumbing"债务。本 PR 关闭它。

### 1.2 为什么删 `UsageEvent.ContextWindow` 字段

`tokensave_field_sites` 报告:整个 codebase 中

- **0 个 read site** (外部)
- **0 个 write site** (外部)

唯一一处写入它的代码是 `internal/bridge/claudecode/stream.go:642 decodeUsage`,但紧接的 6 行后它就被 `out.ContextWindowPct = float64(used) / float64(out.ContextWindow) * 100` 读完用了——**从没穿出 `decodeUsage` 函数边界**。它不是"传递中间值",而是"函数内用完即丢的临时存放"。

→ 完全改成本地变量。Struct 不需要这个字段。

### 1.3 为什么 Pi 能填 pct

Pi 的 `get_state` RPC 响应已经包含 `data.model.contextWindow`(详见 [`../bridge/pi.md`](./../bridge/pi.md) §2.4)。

F-32 实施时 `get_state` 响应只在 `session.New()` 调一次,目的是拿 `sessionId` 填 `ResumeID`。`data.model.contextWindow` 字段在 nightme 这边被静默丢弃。

**决策**:从 `get_state` 响应解 `data.model.contextWindow` → 存到 `translator.contextWindow`(bridge-local 状态)→ `decodeMessageUsage` 拿它算 pct。

### 1.4 为什么不需要 fallback 表

`contextWindow == 0`(pi get_state 失败 / 早期 pi 版本没这个字段 / 模型未识别)→ `decodeMessageUsage` 看到 0 → `ContextWindowPct = 0` → footer 按既有约定 `== 0 时省略`(F-45 §1.6)。**与 ClaudeCode 当前未报 ContextWindow 时行为一致**,无新代码路径。

## 2. 设计

### 2.1 字段变更

```diff
 type UsageEvent struct {
     InputTokens              int
     OutputTokens             int
     CacheCreationInputTokens int
     CacheReadInputTokens     int
     CostUSD                  float64
-    ContextWindow            int
     ContextWindowPct         float64
 }
```

`UsageInfo` 不动(本来就没 `ContextWindow` 字段)。

### 2.2 Pi bridge 改动

**`internal/bridge/pi/protocol.go`** — 扩展现有 `getStateModel`,新增 `ContextWindow` 字段:

```go
type getStateModel struct {
    ID            string `json:"id"`
    Name          string `json:"name"`
    Provider      string `json:"provider"`
    ContextWindow int    `json:"contextWindow"`  // 新增 (F-54)
}
```

`maxTokens` / cost rate table / `id` 已有但 nightme 不消费(`id` / `Name` 已用于 EventAgentConnected.Model 显示)。

**`internal/bridge/pi/translate.go`** — 改 3 处:

```go
type translator struct {
    // ... 既有字段
    contextWindow int   // 新增: 由 emitConnected 填,bridge-local 状态
}

// emitConnected 缓存 contextWindow:
if result.Model.ContextWindow > 0 {
    t.contextWindow = result.Model.ContextWindow
}

// decodeMessageUsage 接收 ctxWindow 算 pct:
func decodeMessageUsage(u *messageUsage, ctxWindow int) *agent.UsageEvent {
    // ... 既有字段填充
    if ctxWindow > 0 {
        used := u.Input + u.Output + u.CacheRead + u.CacheWrite
        if used > 0 {
            out.ContextWindowPct = float64(used) / float64(ctxWindow) * 100
        }
    }
    return out
}
```

调用点变化:`recordAssistantMessageLocked` 把 `t.contextWindow` 作为参数传给 `decodeMessageUsage`。

### 2.3 ClaudeCode bridge 改动

**`internal/bridge/claudecode/stream.go decodeUsage`** — 把 `out.ContextWindow` 改成本地变量:

```go
- out := &agent.UsageEvent{...}
- // ... 解 rawModelUsage
- out.ContextWindow = v.ContextWindow
- // 算 pct
- out.ContextWindowPct = ...
+ out := &agent.UsageEvent{...}
+ contextWindow := 0
+ // ... 解 rawModelUsage
+ if v.ContextWindow > 0 {
+     contextWindow = v.ContextWindow
+ }
+ // 算 pct
+ if contextWindow > 0 {
+     used := ...
+     out.ContextWindowPct = float64(used) / float64(contextWindow) * 100
+ }
```

### 2.4 不动的东西

- ❌ `agent.UsageInfo`(本来就没 `ContextWindow`)
- ❌ `ContextWindowPct` 字段(已存在)
- ❌ `usage_footer.go`(它只读 pct,自动显示 X%)
- ❌ `gateway/` / `chatsession/` / `command/`
- ❌ `SessionContext` schema / 持久化

## 3. 影响

| 维度 | 估 |
|---|---|
| 字段变更 | -1 (`UsageEvent.ContextWindow`) |
| 新增 struct | `modelInfo`, `getStateData`(2 个小 struct,~6 行) |
| 代码净增 | +22 / -7 |
| 涉及文件 | 4 (agent.go, claudecode/stream.go, pi/protocol.go, pi/translate.go) |
| 测试改动 | claudecode `decodeUsage` 测试不 assert `out.ContextWindow`(改 assert `out.ContextWindowPct`);pi `translate_test.go` 加 ~3 用例(get_state 解析 + ctxWindow=0 + ctxWindow>0 算 pct) |
| 持久化 schema | 不变 |
| Footer UX | Pi 用户 footer 出现 `X%` 段(从无到有,claudecode 用户无变化) |

## 4. 风险

- **< 某版本没有 `model.contextWindow`**: `get_state` 响应里 `data.model` 可能为 nil 或 `contextWindow` 为 0 → translator.contextWindow 留 0 → pct 不算 → footer 跳过 X%。**fallback 行为与现 ClaudeCode 未报场景一致。**
- **`/new` 后 contextWindow 失效?**: 不会(`/new` 是 reset conversation,模型没换)。当前没有 model-change 路径(pi F-32 MVP 不支持动态切模型),所以 `translator.contextWindow` 在 session 生命周期内有效。
- **删除字段是破坏性**: 仅删 `UsageEvent.ContextWindow`,**无外部 read site**(tokensave 验证),所以不会破坏调用方。

---

## A4. Pi Coding Agent — 集成方案与待办

> **Source**: `pi.md`


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

---

