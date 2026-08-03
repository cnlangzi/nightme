# F-32: Pi Coding Agent Bridge (RPC 模式)

> **Status**: designing (Phase 1 — 文档评审通过后进入 Phase 2 实现)
> **Milestone**: v1.3
> **Depends on**: F-09 (Agent abstraction), F-19 (CLI Bridge), F-21 (Agent Modes), F-24 (Claude Code Bridge 模式), F-27 (ChatSession), F-28 (`/use`), F-29 (AgentSession pool)
> **Related**: [`F-24-claudecode-bridge.md`](./F-24-claudecode-bridge.md), [`F-21-agent-modes.md`](./F-21-agent-modes.md), [`F-29-agent-session-pool.md`](./F-29-agent-session-pool.md)

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

**决策**：直接接 `pi --mode rpc`，避免 ACP 兼容成本与第三方 `pi-acp` 的版本漂移。ACP bridge 留作 v0.x 兜底不删除；新包与 ACP wire 完全解耦，等两边独立验证后再考虑抽公共 JSONL transport。

### 1.2 为什么首期范围保守

nightme 现有 FSM 假设"一个 AgentSession = 一个进程"；pi RPC 进程是 long-lived，但**一个进程承载多轮 turn**。两套生命周期需要解耦：

- **turn**：`prompt` 提交 → 若干 event → `agent_settled`（可能跨多次 retry / compaction / follow-up）。
- **process**：进程从 spawn 到 `cmd.Wait()` 返回；只有 process exit 才关闭 `Events` channel。

`agent_settled` 与 `EventDone` 对应，**不**关闭 channel。现有 `internal/chatsession/readpump.go` 在 `EventDone` 切 Idle/flush queue 并继续循环，正好支持长驻多轮。设计选择 **复用现有 FSM**，而不是为 pi 新增 `EventTurnEnd` 单独 kind（避免 EventKind 多义扩散；契约在 EventDone 上加注释与单测锁定）。

首期不实现：

- `/abort` + `agent.Abortable`：chat FSM 暂无 Aborting 态；加 `/kill` 已能逃生；后续以可选接口追加。
- `extension_ui_request` → 飞书卡回复闭环：当前 feishu card action 只 toast，不发 decision；扩展 UI 表本身需要 keyed-by-id 的 request map，先把"两端之间加 channel"留到 v1.3.x。
- `steer` / `follow_up`：复用现有 `InputBuffer` 在 `EventDone` 切 Idle 后 flush，避免给 pi 加双语义。
- `set_model` / `set_thinking_level` / `cycle_*`：MVP 留给用户在 pi CLI flag 配置；后续按需开 `/use pi --model=…`。
- `new_session` / `switch_session` / `fork` / `clone`：ChatSession 与 AgentSession pool 已经是 `(agent, cwd)` 维度，未要求 per-session switch；现有 `/use pi` + 换 cwd 已覆盖。

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
| `abort` | C→S | `type` | **首期不发送**；用 `/kill` 替代 |

`ImageContent` shape：`{"type":"image", "data":<base64>, "mimeType":<MIME>}`。

### 2.3 Event 表（映射到 `agent.AgentEvent`）

| pi event | payload 关键字段 | 目标 `AgentEvent` | 备注 |
|---|---|---|---|
| `agent_start` | — | (log debug) | 不入 events |
| `agent_end` | `willRetry` | (log debug) | **不**作为 turn 终态 |
| `agent_settled` | — | **`EventDone{Reason:"settled"}`** | turn 终态；**不关 channel** |
| `turn_start` / `turn_end` | — | (log debug) | 暂不消费 |
| `message_start` | `message` | (log debug) | — |
| `message_update` `{assistantMessageEvent:{type:"text_delta"}}` | `delta`, `contentIndex` | `EventText{Text: delta}` | 与 F-24 assistant 行为对齐 |
| `message_update` `{assistantMessageEvent:{type:"thinking_delta"}}` | `delta` | `EventText{Text: "[思考] " + delta}` | 复用 claudecode 前缀约定 |
| `message_update` `{assistantMessageEvent:{type:"toolcall_start"}}` | `toolCallId`, `name`, `partial.arguments` | no-op | `tool_execution_start` 是唯一 EventToolStart 来源；此事件提前到达以解析 partial args，但渲染层等 canonical 事件 |
| `message_update` `{assistantMessageEvent:{type:"toolcall_end"}}` | `toolCallId`, `toolCall.{name, arguments}` | no-op | 同 toolcall_start：canonical 事件是 tool_execution_start |
| `tool_execution_start` | `toolCallId`, `toolName`, `args` | `EventToolStart{ToolUseID: toolCallId, Name: toolName, Input: raw(args)}` | canonical "tool starting" 事件 |
| `tool_execution_update` | `toolCallId`, `partialResult` | (log debug) | MVP 不暴露 partial；保留接口 |
| `tool_execution_end` | `toolCallId`, `result`, `isError` | `EventToolEnd{ToolUseID: toolCallId, Name: <from start>, Output: stringified(result), IsError: isError}` | — |
| `message_end` `{message:{role:"assistant", stopReason}}` | `message.{content[], usage, stopReason}` | `EventResult{Text: <joined assistant text>, DurationMs: 0, IsError: stopReason=="error", Subtype: stopReason}` | **不切 FSM** |
| `message_end` `{message:{role:"toolResult"}}` | `toolCallId`, `content[]` | (log debug；合并入 tool_execution_end) | — |
| `message_end.usage` | `usage.{input,output,cacheRead,cacheWrite,totalTokens,cost.{input,output,cacheRead,cacheWrite,total}}` | `EventUsage{InputTokens, OutputTokens, CacheCreation: cacheWrite, CacheRead, CostUSD: cost.total}` | 仅当本 turn 累积 usage 非空；多个 message_end 累加 |
| `compaction_start` | `reason` | `EventCompaction{Subtype: "start:" + reason}` | — |
| `compaction_end` | `reason`, `result.aborted` | `EventCompaction{Subtype: "end:" + reason}` | — |
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

bridge 只用：`sessionId`、`model.id`、`model.name`、`sessionFile`（可选，注入到 EventInit metadata）。

## 3. 进程与生命周期

### 3.1 Spawn

```
exec.CommandContext(ctx, "pi", "--mode", "rpc")
cmd.Dir = cfg.Workspace
cmd.Env = os.Environ() + cfg.Env + append(self.command)   // defensive
cmd.Stdin  = pipe
cmd.Stdout = pipe
cmd.Stderr = pipe
cmd.Start() → PID
```

**严禁 PTY**。参照 `internal/bridge/claudecode/session.go:71-130` 的真实 pipe 模式，但去掉 `--print --input-format stream-json --output-format stream-json --permission-mode ...` 那一组 flag。

### 3.2 Goroutine 拓扑

```
session.Spawn()
  ├─ goroutine A: readPump (stdout scanner, 解 frame → 分流 response / event / extension_ui_request)
  ├─ goroutine B: drainStderr (stderr Read, debug log, 保留有界尾部用于错误)
  ├─ goroutine C: lifecycle (cmd.Wait, capture exitCode, 失败所有 pending, 关闭 events)
  └─ goroutine D: handshake  (sync, 启动时 GetState 超时 5s)
```

**唯一 Wait / events close owner = goroutine C**。所有 Close 路径（包括 graceful shutdown、error、异常退出）最终通过 C 写 `exitCodeSink` 并 close events。`Close()` 不直接关 events，由 C 收尾。**这修正了 claudecode 注释掉的 watchdog race，并避免 ACP `readPump` 与 `Close` 双重关闭的同形问题**。

### 3.3 状态机

| 时刻 | 行为 | channel 状态 |
|---|---|---|
| `t0` cmd.Start | 启 A/B/C/D | open |
| `t1` GetState 成功 | 在 C 或 A 中 emit `EventInit{SessionID, Model, AgentName:"pi", Workspace, Branch}` | open |
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

- 启动时 `get_state` 分配 id `"boot"`，timeout 5s。
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
- 现有 `internal/gateway/translate.go` 的事件翻译不需为 pi 改逻辑；`EventInit` / `EventResult` / `EventUsage` / `EventCompaction` / `EventToolStart` / `EventToolEnd` / `EventText` / `EventPermission`（本期不通过此路径走）/ `EventError` 都已存在。

### 5.2 turn 终态语义（核心改动）

在 `internal/agent/agent.go:460-466` 把 `Events()` 的注释从 "channel is closed by the implementation after EventDone or a terminal EventError" 改为：

> Events streams AgentEvent values until the underlying session (process) ends. EventDone marks the completion of a single turn within a long-lived session; the channel stays open across many turns and is closed only by process exit or Close().

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

`nightme agents` 表格显示 `pi / pi / --mode rpc`；`nightme config` → Agents 可选为 primary；飞书会话中 `/use pi` 走现有 `chatsession.Spawner`，经 `Detect()` (`exec.LookPath("pi")`) → `Start()`（`exec.Command("pi", "--mode", "rpc")` + 真实 pipes）→ `newSession(...)`。

## 8. 已知限制（首期明确）

| 能力 | 状态 |
|---|---|
| 文本 turn | ✅ |
| 图片 turn（`prompt.images`） | ✅ |
| 文件附件 | ⚠️ 退化为 `[file: <path>]` 文本 |
| thinking | ✅（`[思考] ` 前缀） |
| tool_call / tool_result | ✅（`EventToolStart` / `EventToolEnd`） |
| usage（input/output/cache/cost） | ✅ |
| compaction | ✅（`EventCompaction`） |
| session state → EventInit | ✅（首次 `get_state`） |
| 多轮长驻 | ✅（`agent_settled` 不关 channel） |
| `/kill` 终止 | ✅（Close 路径） |
| `/abort` 单 turn 取消 | ❌ 用 `/kill` 替代 |
| Extension UI 飞书回复 | ❌ 自动 cancelled，不发卡 |
| steer / follow_up | ❌ 走 InputBuffer flush |
| 运行时切 model / thinking | ❌ 改 pi CLI flag |
| session switch / fork / clone | ❌ |
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
  - `nightme agents` 列出 `pi / pi / --mode rpc`。
  - daemon 启动，`/use pi`；多轮文本 + 一次图片 + 一次 tool call 完整。
  - footer 显示 session id / model / tokens。
  - `/kill` 回收子进程。

## 10. Critical Files

新增：

- `docs/feat/F-32-pi-rpc-bridge.md`（本文件）
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

> **v1.3 release gate**：本 feature 与 F-31（MessageState）、ACP bridge 修复、文档一并合入 v1.3 后再开放主分支。
