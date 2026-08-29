# Generic ACP Bridge — Design Document

> **Status**: ✅ Active (with model/usage fix landed)
> **Scope**: `internal/bridge/acp/` — Agent Client Protocol通用 client端实现
> **适用**: 任何原生支持 ACP JSON-RPC 2.0 over stdio/PTY 的 CLI（opencode、cursor、codex、未来 ACP server）

---

## 1. 设计基线

### 1.1 职责划分原则

bridge 设计的**单一原则**：

> **ACP 协议与其常见 vendor 扩展能覆盖的一切，统一由 generic fallback 接管；只有 vendor 私有、非 ACP spec 的协议层定制才走 UpdateHandler / MethodHandler 扩展点。**

这避免了历史上"每个新 vendor 都要写 400 行 translator"的重复劳动 —— opencode 1.18.x 的实测 wire 表明，常见 vendor 扩展（`usage_update`、`session.status:idle`、`session_info_update` 等）已经在 ACP 协议约定的"vendor 扩展空间"内，没有 vendor-private 私有层。opencode 因此**不需要**装任何 UpdateHandler，直接走 generic 即可（参见 §2.3 反例规则）。

### 1.2 文件结构

```
internal/bridge/acp/
├── acp.go              # 包级常量（protocolVersion、clientName/version）+ Transport 接口
├── agent.go            # driver + 全部 generic fallback handler + deliver/emit
├── rpc.go              # JSON-RPC 2.0 client（pending request 池 + 写锁）
├── starter.go          # acp.NewStarter + RunOnce + collectResult
├── emit_contract_test.go   # deliver() / emitConnected() 阻塞语义回归测试
├── deliver_stamp_test.go   # deliver() 盖章 + 新 handler 测试
└── *_test.go           # 其它测试（handshake / parse / RFC decode 等）
```

### 1.3 与其他 bridge 的关系

```text
internal/bridge/
├── acp/           # ★ 本文档：通用 ACP client 端（JSON-RPC 2.0 over PTY）
├── claudecode/    # 自有 JSON-IO bridge（不走 ACP）
├── codex/         # 自有 app-server JSON-RPC bridge（不走 ACP；用 acp 是独立 SDK）
├── dsh/           # 自有 HTTP+SSE bridge
├── pi/            # 自有 pi --mode rpc bridge
├── opencode/      # 走 acp/ 的 CLI 包装层（spawn recipe + print-mode）
└── cursor/        # 走 acp/ 的 CLI 包装层（spawn recipe + print-mode）
```

claudecode / codex / dsh / pi 是**独立的 transport 实现**，不走 acp bridge；opencode / cursor / 未来 ACP server 走 acp bridge 并在其包内提供 spawn recipe。

### 1.4 Handshake timeout

`handshake()` 按阶段各自计时（`internal/bridge/acp/agent.go`），父 ctx 仍可提前取消：

| 阶段 | 预算 | 覆盖什么 |
|---|---|---|
| `initialize` | 10s (`initializeTimeout`) | spawn + server boot + protocolVersion 握手。与 pi/codex `handshakeTimeout` 对齐；3s 对 cursor-agent 冷启动和低配机器不够。 |
| `session/new` | 45s (`newSessionTimeout`) | ACP server 真正干活（opencode 内部 HTTP backend + loadDirectorySnapshot 等）。 |

超时错误带阶段名和预算，例如 `bridge/acp: initialize (timeout=10s): context deadline exceeded`。

---

## 2. 协议支持矩阵

### 2.1 Generic fallback 识别的 sessionUpdate kind

| `sessionUpdate` | Wire 字段 | 处理 | 出处 |
|---|---|---|---|
| `agent_message_chunk` | `content.text` | 缓冲到 textBuf；**滑动空闲** `flushDebounce`(800ms)：每个 token 重置计时。静默后分别检查 text/thought，ready 的一侧独立 flush（互不阻塞）。真断句：≥160 rune + 中文 `。！？` / ASCII `.?!`+空白（排除 `e.g.`/`Mr.` 等缩写与 `session.idle_timeout`）；列表 `"4."` 不断。tool / turn-end / Close **立即** drain | ACP spec |
| `agent_thought_chunk` | `content.text` | 同 textBuf 规则，flush 时带 `[思考]` 前缀 | ACP spec |
| `tool_call` | `toolCallId / title / rawInput` | EventAgentToolStart | ACP spec |
| `tool_call_update` | `toolCallId / status / rawOutput` | EventAgentToolEnd | ACP spec |
| `message_chunk` | `content.text` | legacy，同 textBuf 规则 | legacy |
| `usage_update` | 见下表（两种 shape 都认）| stash 到 `d.lastUsage`；`model` 字段写 `d.model` | ACP spec + opencode vendor |
| `session.status` | `{status: "idle"}` | turn-end：flush buffer + EventAgentDone{Usage: lastUsage} | opencode turn-end signal |
| `session_info_update` | `model` 等 vendor 字段 | 写 `d.model` | vendor 扩展 |
| **default** | — | 忽略（不 flush 缓冲，避免 vendor 扩展插在 token 之间时打散回复） | 兜底 |

#### `usage_update` 的两种 wire shape

| 来源 | 字段 | 映射 |
|---|---|---|
| **opencode vendor** | `used` / `size` / `cost` | `used → InputTokens`，`size → ContextWindow + pct`，`cost → CostUSD` |
| **ACP spec** | `inputTokens` / `outputTokens` / `cacheReadInputTokens` / `cacheCreationInputTokens` / `totalTokens` / `costUSD` | 直接对应 agent.UsageInfo 同名字段 |

两者都解析；任一非零即写入 `d.lastUsage`（标准 shape 优先，因粒度更细）。All-zero payload 不清空已有 stash —— 防止 server 用零值做"stream end"标记时误清。

### 2.2 Generic fallback 识别的 JSON-RPC method

| method | 方向 | 处理 |
|---|---|---|
| `session/update` | A→C | 走 §2.1 表 |
| `session/request_permission` / `permission_request` | A→C | EventAgentPermission |
| `message_chunk`（top-level） | A→C | legacy EventAgentText |
| `tool_start` / `tool_end` | A→C | legacy EventAgentTool{Start,End} |
| `session_end` | A→C | EventAgentDone{ExitCode:0} |
| `initialize` / `session/new` / `session/prompt` | C→A echo | 忽略（PTY echo 兜底）|

### 2.3 vendor-private → UpdateHandler / MethodHandler 扩展点

只有**vendor 私有协议**（ACP spec + 通用 vendor 扩展**之外**的）才走扩展点。例如：

- **cursor 的 `cursor/update_todos` JSON-RPC method** → 走 `SetMethodHandler()`
- **cursor 的 `cursor/create_plan` JSON-RPC method** → 同上
- 假设性的 vendor-private slash-command 通知 → 走 `SetUpdateHandler()`

**反例**（不应走扩展点）：
- opencode 的 `usage_update`（opencode 1.18.x wire 实测）→ 已在 §2.1 通用 fallback
- 任何 ACP spec 已定义或常见 vendor 扩展已覆盖的 kind → 走 fallback

扩展点 API：

```go
// UpdateHandler：vendor-private sessionUpdate kind
driver.SetUpdateHandler(opencodeSessionDecorator)

// MethodHandler：vendor-private JSON-RPC method
driver.SetMethodHandler(cursorMethodDecorator)

// FlushHandler：bridge-specific text buffer 兜底
driver.SetFlushHandler(myFlushHook)
```

**All-or-nothing 语义**：装了 UpdateHandler 就 100% 接管 sessionUpdate 流（防止双发）；装了 MethodHandler 仅接管**未识别的**method，已识别的（§2.2）继续走 fallback。

---

## 3. bridge-local 状态与 per-event 盖章

### 3.1 driver 状态字段

| 字段 | 来源 | 何时变化 | 用途 |
|---|---|---|---|
| `sessionID` | `session/new` 响应 | handshake + `New()` 时更新 | per-event 盖章 |
| `agentName` | NewStarter 注入 | 不可变 | per-event 盖章 |
| `workspace` | `cfg.Workspace` | 不可变 | per-event 盖章 |
| `model` | `usage_update.model` / `session_info_update.model` | vendor 报就更新 | per-event 盖章 |
| `lastUsage` | `usage_update` | 每个 turn 写入、turn-end 清零 | 落到 Done.Usage |
| `turnSettled` | local | turn-end set；**`SendBlocks` 顶部 reset** | 防止 turn-end 双发 |

### 3.2 `deliver()` — 唯一 producer-side 发送路径

每次发送前盖：

```go
if ev.SessionID == "" { ev.SessionID = d.sessionID }
if ev.AgentName == "" { ev.AgentName = d.agentName }
if ev.Workspace == "" { ev.Workspace = d.workspace }
if ev.Model == ""    { ev.Model = d.model }
```

**已设不覆盖、空才填** —— caller 显式给的值（如 `flushBuffer` 已经塞了 `SessionID`）不会被覆盖。发送阻塞语义：通道满 → block；ctx.Done → drop；**没有 default: 分支**（`TestDeliver_NoInstantDrop` 锁定此契约）。

### 3.3 model 字段的并发安全

`d.model` 是**多写少读**状态：

- **写**：`handleUsageUpdate` / `handleSessionInfoUpdate` 在 readPump goroutine 上写
- **读**：`deliver()` 在任何 goroutine 上读（handshake、SendBlocks via `translatePromptResponse`、`flushBuffer`、`emitConnected` 都可能调）

读端最危险的路径是 `translatePromptResponse` —— 它在 SendBlocks goroutine 上跑（SendBlocks 调用 `d.rpc.request` 然后同步 `d.translatePromptResponse(result)`），同时 readPump 也在跑、可能在写 `d.model`。**没有 mutex 的话 race detector 会抓到 torn string read**（P1）。

修复：`modelMu sync.Mutex`。读端快速 path：

```go
d.modelMu.Lock()
ev.Model = d.model
d.modelMu.Unlock()
```

写端对称。Mutex 开销可以忽略 —— 写端每个 turn 只发生几次，读端虽然频繁但锁持续时间是纳秒级。

### 3.4 runtime 端的配合

`internal/runtime/handler.go:117` 已放开 SetModel 捕获条件：

```go
if ev.Model != "" {
    s.SetModel(ev.Model)
}
```

（之前只 `EventAgentReady && ev.Model != ""`，对 ACP 不够 —— wire 不在 handshake 报 model）

意味着 bridge 通过 `deliver()` 盖在后续 Text / Tool / Result 事件上的 Model 会被 runtime 写进 `s.Model()`，footer 跨整个 session 都能显示。ACP 之前 footer 全空的问题由此修复。

---

## 4. turn-end 信号的去重

ACP turn-end 有**多个**可能来源，bridge 必须去重：

| 来源 | 路径 | 触发条件 |
|---|---|---|
| `session/prompt` 同步响应 | `translatePromptResponse`（success / cancelled / max_tokens / refusal 各路径）| opencode 同步模式 |
| `session.status:{status:"idle"}` sessionUpdate | `handleSessionStatus` | opencode 流式 turn-end |

`turnSettled` 标志保证同 turn 内只发一次 `EventAgentDone`（含 error 路径，因为 cancelled/max_tokens/refusal 也 bump）。**首个信号 wins；handleSessionStatus 见到 turnSettled=true 时跳过**。

**关键：`turnSettled` 在每次 `SendBlocks` 顶部重置为 false**（在 busy-guard 之后、defer 之前）。如果忘了重置，多 turn 场景下第一个 turn 把它设为 true 后，第二个 turn 的两个 terminal 路径都会 silent skip `EventAgentDone` —— busy guard 永远不清，spinner 永远转（P0）。

成功路径发送 `EventAgentDone{Reason:"settled", Usage: usage}`；错误路径只发 `EventAgentError`（不附带 Done，匹配 pre-fix 行为）。

---

## 5. 设计约束 / 已知限制

### 5.1 Model 在 handshake 时未知

ACP 的 `initialize` 和 `session/new` 响应都**不带** model 字段。Model 只在后续 vendor 扩展（`usage_update.model` / `session_info_update.model`）里出现。所以：

- `EventAgentReady.Model` 在 handshake 时为空（runtime 容忍）
- Model 在第一次 vendor 扩展送达后才可见（runtime 通过 §3.4 SetModel 放开自动捕获）
- server 完全不报 model 的极端情况：footer 模型段不显示

### 5.2 opencode 的 `used` 字段语义模糊

opencode `usage_update.used` 是**模型上下文累计用量**（不是单 turn 的 input tokens）。bridge 把它映射到 `agent.UsageInfo.InputTokens`，channel footer 会显示。**这是 best-effort 映射，不是精确语义**；更精确的实现要等 opencode 1.19+ 改 wire。

### 5.3 turn-end 双发的最坏情况

opencode 同时发 prompt 响应 + session.status:idle 时，`turnSettled` 保证只发一次。但若 server 实现有 bug 导致 cancelled 之后还发 status:idle，bridge 会 silently 丢弃 status:idle 的 Done（`turnSettled` 已 bump）—— 这是正确行为，因为 turn 已经以 Error 终态结束。

---

## 6. 扩展点 API 摘要

```go
// UpdateHandler：vendor-private sessionUpdate kind
type UpdateHandler func(view *SessionView, raw json.RawMessage) error
driver.SetUpdateHandler(h UpdateHandler) // nil = revert to fallback

// MethodHandler：vendor-private JSON-RPC method
type MethodHandler func(method string, params json.RawMessage,
    respond func(id json.RawMessage, result any, err error) bool) bool
driver.SetMethodHandler(h MethodHandler)

// FlushHandler：bridge-specific text buffer 兜底（turn-end 时调用）
type FlushHandler func(view *SessionView)
driver.SetFlushHandler(h FlushHandler)

// SessionView：通过 View() 拿到，只暴露 driver 的有限表面
type SessionView struct {
    Emit         func(ev agent.AgentEvent)
    SessionID    func() string
    AgentName    string
    Workspace    string
    FlushPending func()
}
```

**Lifetime 约束**：`SetXxxHandler` 必须在 readPump 观察到第一个相关事件**之前**调用；调用之后改 handler 仍然 race-safe（atomic.Pointer），但事件已经 race 过了。建议在 `Starter.Start()` 返回后立即 set。

---

## 7. 迁移 / 未来工作

### 7.1 已完成（v0.x）

- ✅ generic fallback 接管 `usage_update` / `session.status` / `session_info_update`
- ✅ `deliver()` 自动盖 SessionID / Model / AgentName / Workspace
- ✅ runtime `SetModel` 捕获条件放开
- ✅ 13 个新单元测试（`deliver_stamp_test.go`）

### 7.2 v2+ 留待 issue

- ACP `session/load` resume 路径（目前 `cfg.SessionID` 启动 + `setSessionMode/Option`）
- ACP image / file block（目前 `@<path>` 兜底）
- `setSessionModel` JSON-RPC 方法（用于 `/model` slash command）
- `available_commands_update` → runtime slash command 路由（等 slash-command-reactions 落地）
- `current_mode_update` / `config_option_update` 路由（等 opencode 1.19+ 稳定）
- `Plan` → `EventAgentTaskCreate/Update` 路由
- ACP spec v2 兼容（用 negotiated `protocolVersion` 分支）
