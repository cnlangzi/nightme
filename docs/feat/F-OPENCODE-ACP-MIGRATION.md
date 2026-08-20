# F-OPENCODE-ACP-MIGRATION — opencode bridge 迁移到 ACP 模式

> **Status**: ✅ 完成 (Phase 0 / Phase 1 / Phase 2 / 收尾清理 全部落地)
> **Scope**: `internal/bridge/opencode/`(Phase 0 期间临时叫 `opencode_acp` 以与旧 HTTP serve 包并存;Phase 2 旧包删除后改回 `opencode`)
> **Supersedes**: `docs/bridge/opencode.md` §1.2 "三不做"(已随 Phase 2 一并删除 — 见本 doc §6)
> **Related**: [`docs/bridge/acp.md`](./acp.md), [`docs/bridge/codex.md`](./codex.md), [`docs/bridge/pi.md`](./pi.md) — 同期 long-lived bridge 设计基线
> [`docs/bridge/codex.md`](../bridge/codex.md), [`docs/bridge/pi.md`](../bridge/pi.md) — 同期 long-lived bridge 设计基线
> **官方资料**: <https://opencode.ai/docs/acp/> ("All features are supported"), <https://agentclientprotocol.com/>

---

## 0. 动机(为什么做这次迁移)

### 0.1 现状

nightme 的 opencode 集成(`internal/bridge/opencode/`)走 **HTTP serve** 路径:`opencode serve --hostname=127.0.0.1 --port=0` → 手写 Go HTTP client 9 个 endpoint → 自实现 SSE 解析器 → 14 种 SSE event 类型翻译为 `AgentEvent`。

**代码体量**:4 个核心文件,~3500 行 Go 代码

| 文件 | 行数 | 角色 |
|---|---:|---|
| `server.go` | 349 | spawn `opencode serve` + 解析 stdout banner URL |
| `client.go` | 545 | 9 个 HTTP endpoint + `decodeSession` 信封兼容 |
| `translate.go` | ~1300 | 14 种 SSE event → AgentEvent 翻译表 + 流式 text buffer |
| `agent.go` + `driver.go` | ~1500 | 生命周期、SSE 重连、watchdog、liveness probe |

**踩坑密度**(从 `docs/bridge/opencode.md` 和 CHANGELOG 整理):
- opencode 1.18 `{data: Session}` 信封(translate decode 双形态)
- 1.18.x SSE 500 ServeError on `GET /api/session/{id}/event`(e2e 自动 skip)
- readSSE + lifecycle 死锁(`pumpWG` + `stopDeliver` + `close(events)` 顺序)
- closeOnce 死锁(SIGINT/SIGKILL 与 Wait() 互锁)
- session.next.text.{started,delta,ended} 流式缓冲(每 token 刷新 → 缓冲到 flush 边界)
- bash → Bash 等工具名大小写 normalization
- `x-opencode-directory` 路由 + InstanceContext cwd 坑
- liveness probe 三次失败才 kill,`serverStartTimeout=10s` 太短冷启动模型下载超时
- `turnWatchdogTimeout` 10 分钟超时

### 0.2 opencode 官方推荐路径

`https://opencode.ai/docs/acp/` 原文:

> **"OpenCode works the same via ACP as it does in the terminal. All features are supported:"**
> - Built-in tools (file operations, terminal commands, etc.)
> - Custom tools and slash commands
> - MCP servers configured in your Opencode config
> - Project-specific rules from AGENTS.md
> - Custom formatters and linters
> - Agents and permissions system

官方文档(Zed / JetBrains / Avante.nvim / CodeCompanion.nvim)统一配:

```json
{ "command": "opencode", "args": ["acp"] }
```

**关键事实**:`opencode acp` 命令在 opencode 1.18.18 源码(`/tmp/acp.ts`)中**自己就是用 `opencode serve` 启动的同一个 HTTP server**,然后在前面套一层 `@agentclientprotocol/sdk` 的 JSON-RPC 2.0 stdio 适配器:

```ts
// packages/opencode/src/cli/cmd/acp.ts (opencode 1.18.18)
const { Server } = yield* Effect.promise(() => import("@/server/server"))
const { ACP } = yield* Effect.promise(() => import("@/acp/agent"))
process.env.OPENCODE_CLIENT = "acp"
const server = yield* Effect.promise(() => Server.listen(opts))  // ← 同 Server.listen()
const sdk = createOpencodeClient({                              // ← HTTP SDK
  baseUrl: `http://${server.hostname}:${server.port}`,
})
const stream = ndJsonStream(input, output)                       // ← JSON-RPC over stdio
const agent = ACP.init({ sdk })
new AgentSideConnection((conn) => agent.create(conn))             // ← @agentclientprotocol/sdk
```

也就是说:**走 acp 不损失 opencode HTTP 层的能力**(providers、sessions、messages、tools、agents、config、mcp 全部覆盖),反而拿到的:
- 业界标准协议 `agentclientprotocol` 的 JSON-RPC 2.0 wire
- opencode 上游主动维护的 ACP 适配器(每次升级自动跟 opencode 同步)
- nightme 自家 `internal/bridge/acp/` 已经实现的通用设施(JSON-RPC client、PTY transport、Stop、Permission、Reset)

### 0.3 历史 drift 痕迹

`cmd/nightme/agents_test.go:34` 写的期望就是 `Args: []string{"acp"}`:

```go
{Name: "opencode", Command: "opencode", Args: []string{"acp"}},
```

`internal/agent/interface_external_unix_test.go:43` 也有一个独立 acp 注册:

```go
{"acp", acp.NewStarter("acp", "opencode", []string{"acp"}, nil, 0, 0)},
```

但 `cmd/nightme/agents.go:68` 实际注册的是 `opencode.NewStarter("opencode", "opencode", nil)`(无参,触发 serve 路径)。**测试期望的 acp 路径从未真的被默认用户走通**。这是历史 drift 留下的矛盾。

---

## 1. 目标

### 1.1 主目标

把 nightme 的 opencode 集成从私有 HTTP serve wire 切到标准 ACP wire,删除 ~3500 行 opencode-私有 SSE 翻译代码,复用 `internal/bridge/acp/` 通用设施,保留 `opencode run --format json` 的 print-mode 单转路径。

### 1.2 非目标(本期不做)

- 不动 `internal/bridge/acp/` 自身的 wire-level 代码(JSON-RPC framing、PTY transport、Stop/Reset)。
  - 既有 acp bridge 已经覆盖: initialize / session/new / session/prompt / session/cancel / session/request_permission / tool_call / tool_call_update / session/update 路由。
- 不实现 `available_commands_update` 路由到飞书 slash command(等 slash-command-reactions 落地再做)。
- 不实现 `plan` 路由到飞书 checklist(opencode 1.18.18 在 plan 更新上还不稳定;`internal/agentsession/event_types.go` 已有 `EventAgentTaskCreate/Update` 类型,但目前没有 driver 真正产过,留 v2 接)。
- 不实现 `current_mode_update` / `config_option_update`(opencode acp 这两个 update 在 1.18.18 还偶发空 payload,等上游稳定再做)。
- 不支持 `loadSession` / `unstable_forkSession`(opencode 1.18 的 `loadSession` 走 `messages/` HTTP SDK + acp 适配层语义不稳;resume 改用 `cfg.SessionID` → 启动后 `setSessionConfigOption`/`setSessionMode` 来还原)。
- 不实现 `authenticate` JSON-RPC 调用(走 OPENCODE 配置文件 + `opencode providers login` 完成,跟 basic auth 路径一致)。
- 不支持 `listSessions` / `resumeSession` / `closeSession` 通过 acp 调用(nightme 自己的 AgentSession 模型已经把这些语义吸收了:resume 用 `cfg.SessionID` 走 `session/load`,close 走 PTY EOF 或 transport.Close())。

---

## 2. 设计基线

### 2.1 文件 layout(从 ~4000 行收到 ~1500 行)

```
internal/bridge/opencode/
├── opencode.go            # 常量 + 错误 + 包级 helper + Info + Starter(~100 行)
├── acp.go                 # thin wrapper over internal/bridge/acp (~50 行)
├── print.go               # one-shot `opencode run --format json`(~600 行,保留)
├── print_test.go          # 单转模式测试(~300 行,保留)
└── (删除)
    server.go              # 已被 acp transport 替代
    client.go              # 已被 acp JSON-RPC 替代
    translate.go           # 已被 acp session/update 路由替代
    agent.go / driver.go   # 已被 acp driver + new acp.go 替代
    transport.go           # 已被 acp JSON-RPC framing 替代
    think_tags.go          # 已被 acp session/update 路由的 agent_thought_chunk 分支替代
    testdata/              # 已被 acp mockTransport 替代
    *_test.go (12 个)      # 已被 acp 现有 8 个 test 替代 + print 单转测试
```

**总体减负**:~3500 行删除 + ~150 行新增 = **净减 ~3350 行**。这部分代码不需要重写,只是换成 ACP 标准 wire。

### 2.2 复用策略

```text
nightme ──spawn──> opencode acp  (PTY 走 stdin/stdout)
                    │
                    ├── JSON-RPC 2.0 over stdio
                    ├── initialize / session/new / session/prompt / session/cancel
                    ├── session/request_permission
                    ├── session/update notifications:
                    │   ├── user_message_chunk
                    │   ├── agent_message_chunk      → EventAgentText
                    │   ├── agent_thought_chunk      → EventAgentText("[思考] " + ...)
                    │   ├── tool_call                → EventAgentToolStart
                    │   ├── tool_call_update         → EventAgentToolEnd (status→done/error)
                    │   ├── usage_update             → stash → 落 Done.Usage
                    │   ├── available_commands_update → 日志(log only)
                    │   └── session.status:idle      → EventAgentDone (Reason="settled")
                    │
                    └── (one-shot)
                        │
                        └── opencode run --format json <prompt>
                            └── NDJSON 流: text / reasoning / tool_use / step_start / step_finish
                                → RunResult
```

### 2.3 路径分流

| 调用入口 | 协议 | 物理载体 | 文件 |
|---|---|---|---|
| `Start()` (chat session 长生命周期) | ACP JSON-RPC 2.0 | PTY(opencode 自己的 stdin/stdout) | `acp.go` |
| `RunOnce()` (one-shot: `/gtw` commit, buildAgentPrompt) | `opencode run --format json` NDJSON | 直接 stdout pipe | `print.go` |
| `ListModels/Providers` 等 list 查询(本期不需要) | (deprecated) | — | — |

### 2.4 不变量(跨路径统一)

1. `EventAgentDone ≠ close events channel`
   - `acp.go` 长生命周期:PTY EOF / `Close()` → close(events);`EventAgentDone` 只标志 turn 结束
   - `print.go` one-shot:读完 stdout EOF + cmd.Wait 完成后 → 返回 `RunResult`,无 events channel

2. Producer-side back-pressure contract
   - `acp.go` 复用 `acp.deliver()` 的"events chan 40960 + 不 drop"模式
   - `print.go` 用 buffered scanner + stdout pipe,无 producer back-pressure 概念

3. `EventAgentError.Diagnostic` 永远带 `StderrTail`(让 runtime 能 surface "为什么 opencode 死了")
   - `acp.go` 复用 `pty.Transport` 的 stderr 收集
   - `print.go` 走原有 stderr 收集 + `cmd.Process.Wait()`

---

## 3. ACP wire → AgentEvent 完整映射

opencode 1.18.18 acp server 实际产出的 sessionUpdate 类型(`/tmp/acp-event.ts` + `/tmp/opencode-acp-full.ts` 已验证):

### 3.1 SessionUpdate 路由表

| ACP sessionUpdate | 含义(opencode 1.18) | AgentEvent 映射 | 处理位置 |
|---|---|---|---|
| `user_message_chunk` | 回放用户消息(replay) | (drop) | **generic fallback** |
| `agent_message_chunk` | assistant 文本流 | `EventAgentText{Text}` | **generic fallback** |
| `agent_thought_chunk` | reasoning / thinking 文本流 | `EventAgentText{Text="[思考] " + ...}` | **generic fallback** |
| `tool_call` (status=pending) | 工具调用开始 | `EventAgentToolStart{ID, Name, Args}` | **generic fallback** |
| `tool_call_update` (status=running) | 工具运行中 | (可选)更新 output | **generic fallback**（status=running 当前 drop，飞书/tui 不渲染 mid-progress）|
| `tool_call_update` (status=completed) | 工具完成 | `EventAgentToolEnd{ID, Name, Output}` | **generic fallback** |
| `tool_call_update` (status=errored) | 工具失败 | `EventAgentToolEnd{ID, Name, Err}` | **generic fallback** |
| `usage_update` | token 用量 / context window | stash 到 `lastUsage`; turn-end 落 `Done.Usage` | **generic fallback** |
| `session.status` (idle) | turn-end signal | `EventAgentDone{Reason:"settled", Usage: lastUsage}` | **generic fallback** |
| `session_info_update` | session metadata 变化 | `model` 字段写 `d.model`（其他字段 reserved）| **generic fallback** |
| `available_commands_update` | 可用 slash command 列表 | log only | **generic fallback** default（v2 wire `plans/slash-command-reactions`）|
| `current_mode_update` | 模式变更(build/plan) | log only | **generic fallback** default（opencode 1.18.18 偶发空 payload,留 v2）|
| `config_option_update` | 配置变更 | log only | **generic fallback** default |
| `plan` | 任务规划 | log only(v1); v2 wire `EventAgentTaskCreate/Update` | **generic fallback** default |

> **更新（v0.x）**：上表所有 kind 自 [date of fix-acp-model PR] 起由 **generic fallback**（`internal/bridge/acp/agent.go::handleSessionUpdate`）接管，opencode **不再需要**写自己的 `update.go`。§4.4 的设计仍保留作为"opencode 未来如果引入 vendor-private 协议时可参照"的模板，但**当前不需要落地**。详见 `docs/bridge/acp.md` §1.1 / §2.3。

### 3.2 SessionRequest / Notification 路由

| ACP method | 方向 | 处理 |
|---|---|---|
| `initialize` request | C→A | `acp.handshake()` 已实现 |
| `session/new` request | C→A | `acp.handshake()` + `emit(EventAgentReady)` 已实现 |
| `session/load` request | C→A | 本期通过 `cfg.SessionID` → acp driver 启动后用 `session/load`(v2 视情况加) |
| `session/prompt` request | C→A | `acp.SendBlocks` 已实现,补 image/file block |
| `session/cancel` notification | C→A | `acp.Stop` 已实现(Fix-Stop 后) |
| `session/request_permission` request | A→C | `acp.handlePermission` 已实现,optionId 透传 |
| `session/update` notification | A→C | **本次新增**:完整 sessionUpdate 路由表 |
| `session/status` event | A→C(`session.status` type=idle) | **本次新增**:`EventAgentDone{Reason="settled"}` |

### 3.3 AgentEvent emission 规则（已被 generic fallback 取代）

> **更新（v0.x）**：本节原本描述的 opencode `update.go` 设计**不再需要落地**。所有 emission 规则已由 `internal/bridge/acp/agent.go::handleSessionUpdate` 的 generic fallback 实现。opencode 直接走 generic，不装任何 per-bridge UpdateHandler。完整 mapping 见 [docs/bridge/acp.md §2.1](../../bridge/acp.md)。
>
>下面是**历史设想**的 opencode `update.go` 实现，仅作为"未来如需 vendor-private 协议扩展"的参考模板保留：

```go
// (历史草图 — 当前不实现；保留作为扩展模板)
// text / reasoning
case "user_message_chunk":     // drop
case "agent_message_chunk":    emit(EventAgentText{Text: chunk.content.text})
case "agent_thought_chunk":    emit(EventAgentText{Text: "[思考] " + chunk.content.text})

// tool calls
case "tool_call":              emit(EventAgentToolStart{ID, Name, Args=string(rawInput)})
case "tool_call_update":
    switch status:
        case "completed":      emit(EventAgentToolEnd{ID, Name, Output=string(rawOutput)})
        case "errored":        emit(EventAgentToolEnd{ID, Name, Err: errFrom(rawOutput)})

// usage
case "usage_update":           d.lastUsage = &UsageInfo{Used, Size, Cost}; (defer to turn-end)

// session status
case "session.status:idle":    emit(EventAgentDone{Reason: "settled", Usage: d.lastUsage})

// modes / config / info / plan / commands
case "available_commands_update", "current_mode_update", "config_option_update",
     "session_info_update", "plan":
    oLog("[opencode/acp] update received", "kind", kind, ...)
    // v2 routing
```

---

## 4. 文件-by-文件设计

### 4.1 `internal/bridge/opencode/opencode.go`(包常量 + 错误)

参考 `internal/bridge/opencode/opencode.go` 当前 257 行,**精简约一半**,只保留:

```go
package opencode

import "time"

// ─── timing ───
const (
    serverStartTimeout   = 10 * time.Second // shared with pty.NewTransport handshake
    permissionTimeout    = 5 * time.Minute
    turnWatchdogTimeout  = 10 * time.Minute
)

// ─── buffer / cap ───
const (
    eventBufferSize = 40960 // match codex/pi/claudecode/acp producer-side contract
    maxImageBytes   = 10 * 1024 * 1024
)

// ─── errors ───
var (
    ErrPermissionTimeout = errors.New("opencode: permission request timed out")
    ErrSessionClosed     = errors.New("opencode: session closed")
)

// ─── env ───
const bridgeName = "opencode"  // agent.AgentName stamped on every event
const version    = "0.2.0"     // bumped: migration to ACP

// oLog is the bridge-side debug logger.
func oLog(msg string, args ...any) { /* same as current */ }
```

### 4.2 `internal/bridge/opencode/acp.go`(~250 行)

```go
package opencode

import (
    "context"
    "encoding/json"
    "sync"
    
    "github.com/cnlangzi/nightme/internal/agent"
    "github.com/cnlangzi/nightme/internal/bridge/acp"
)

// acpStarter is the spawn recipe. Same shape as internal/bridge/acp.Starter,
// but specialized to opencode's binary + default args. The wire-format
// details (JSON-RPC framing, PTY transport, Stop fallback) all live in
// internal/bridge/acp; this file is the opencode-specific thin wrapper.
type acpStarter struct {
    name    string
    command string
    args    []string  // ["acp"]
}

func NewACPStarter(name, command string) *acpStarter {
    return &acpStarter{name: name, command: command, args: []string{"acp"}}
}

// Info returns agent.Info with ModeACP.
func (s *acpStarter) Info() agent.Info {
    return agent.NewInfo(s.name, agent.ModeACP, s.command, s.args, nil)
}

// Start delegates to internal/bridge/acp. The bridge-specific session
// surface (text stream buffer, think tags, tool name normalization,
// usage carry-over) lives in this file's wrapped driver.
func (s *acpStarter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
    return acp.NewStarter(s.name, s.command, s.args, nil, 0, 0).
        Start(ctx, cfg, opencodeSessionDecorator())
}

// RunOnce delegates to print.go.
func (s *acpStarter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
    return runPrintMode(ctx, /* ... */)
}
```

> **更新（v0.x）**：原 §4.2 设计 `internal/bridge/opencode/update.go` + `SetUpdateHandler()` 路径**不再落地**。
>
> 经验证，opencode 1.18.x wire 实际产出的所有 sessionUpdate 类型（包括 `usage_update`、`session.status:idle`、`session_info_update` 等）都已落在 [docs/bridge/acp.md §2.1](../../bridge/acp.md) 列出的"generic fallback"覆盖范围内。直接复用 `acp.NewStarter` 就够 —— 不需要任何 per-bridge translator。
>
> 本节保留作为"未来 opencode 引入 vendor-private 协议时可参照"的设计模板，但**当前不需要落地**。如果未来 opencode 添加 ACP spec 之外的私有 sessionUpdate kind 或 JSON-RPC method，模式如下：
>
> ```go
> drv.SetUpdateHandler(opencodeSessionDecorator)  // 或 SetMethodHandler
> ```
>
> 详见 `docs/bridge/acp.md` §2.3 / §6 的扩展点 API。

~~但等等,**简单复用 `acp.NewStarter` 不够** —— opencode acp server 产出的 sessionUpdate 类型(`agent_thought_chunk`、`usage_update`、`tool_call_update.status=errored` 等)需要专门翻译,而 `internal/bridge/acp/agent.go` 的 `handleSessionUpdate` 只硬编码了 4 个 case。~~（历史描述，已被上面的更新取代）

**两个选择**:

**方案 A**(选这个):扩展 `internal/bridge/acp/agent.go::handleSessionUpdate`,把 sessionUpdate 路由表抽出来做成可注入的 `updateRouter`:

```go
// internal/bridge/acp/agent.go
type UpdateHandler func(ctx context.Context, d *driver, update json.RawMessage) error

func (d *driver) SetUpdateHandler(h UpdateHandler) { d.updateHandler = h }

func (d *driver) handleSessionUpdate(raw json.RawMessage) {
    var params struct{ Update json.RawMessage `json:"update"` }
    json.Unmarshal(raw, &params)
    
    var head struct{ SessionUpdate string `json:"sessionUpdate"` }
    json.Unmarshal(params.Update, &head)
    
    if d.updateHandler != nil {
        if err := d.updateHandler(context.Background(), d, params.Update); err != nil {
            // log only — don't kill the stream
        }
        return
    }
    // fall back to existing 4-case behavior
    switch head.SessionUpdate { /* ... */ }
}
```

然后 opencode 包装层注入完整的 opencode update 路由器(`handleUpdate` 闭包),保持 `internal/bridge/acp/` 的 wire-level 代码 100% 不动。

**方案 B**(不选):完全复制 `internal/bridge/acp/` 到 opencode 子包。重复代码 ~800 行,后期 patch acp 通用 bug 要同步两份。**否决**。

### 4.3 `internal/bridge/opencode/print.go`(保留原 ~600 行)

跟现有 `print.go` 一致,不变。`opencode run --format json` 是 opencode CLI 单转模式,跟 ACP 是完全不同的子命令,不会被影响。

### 4.4 `internal/bridge/opencode/update.go`(~400 行,新增)

```go
package opencode

// opencodeSessionDecorator returns the opencode-specific UpdateHandler
// injected into the generic ACP driver. It implements the full
// sessionUpdate → AgentEvent mapping table documented in §3.
func opencodeSessionDecorator() acp.UpdateHandler {
    return func(ctx context.Context, d *acp.driver, raw json.RawMessage) error {
        var head struct {
            SessionUpdate string          `json:"sessionUpdate"`
            Content       json.RawMessage `json:"content"`
            ToolCallID    string          `json:"toolCallId"`
            Title         string          `json:"title"`
            Status        string          `json:"status"`        // for tool_call_update
            RawInput      json.RawMessage `json:"rawInput"`
            RawOutput     json.RawMessage `json:"rawOutput"`
        }
        if err := json.Unmarshal(raw, &head); err != nil {
            return err
        }
        switch head.SessionUpdate {
        case "user_message_chunk":                // drop
        case "agent_message_chunk":
            text := decodeTextChunk(head.Content)
            if text != "" { d.emit(agent.AgentEvent{Kind: agent.EventAgentText, Text: text}) }
        case "agent_thought_chunk":
            text := decodeTextChunk(head.Content)
            if text != "" { d.emit(agent.AgentEvent{Kind: agent.EventAgentText, Text: "[思考] " + text}) }
        case "tool_call":
            argsJSON, _ := json.Marshal(head.RawInput)
            d.emit(agent.AgentEvent{Kind: agent.EventAgentToolStart, ToolStart: &agent.AgentToolStartEvent{
                ID:   head.ToolCallID,
                Name: head.Title, // opencode uses `title` for human-readable name; rawInput has more
                Args: string(argsJSON),
            }})
        case "tool_call_update":
            switch head.Status {
            case "completed":
                outJSON, _ := json.Marshal(head.RawOutput)
                d.emit(agent.AgentEvent{Kind: agent.EventAgentToolEnd, ToolEnd: &agent.AgentToolEndEvent{
                    ID: head.ToolCallID, Name: head.Title, Output: string(outJSON),
                }})
            case "errored":
                d.emit(agent.AgentEvent{Kind: agent.EventAgentToolEnd, ToolEnd: &agent.AgentToolEndEvent{
                    ID: head.ToolCallID, Name: head.Title,
                }, Err: fmt.Errorf("opencode: tool %s failed: %s", head.Title, string(head.RawOutput))})
            case "running":
                // optional: emit a second EventAgentToolStart with running progress
                // v1: drop (飞书渲染层目前不支持 mid-progress)
            }
        case "usage_update":
            var u struct {
                Used int64   `json:"used"`
                Size int64   `json:"size"`
                Cost float64 `json:"cost"`
            }
            json.Unmarshal(raw, &u)
            d.stashUsage(&agent.UsageInfo{
                InputTokens: int(u.Used), // opencode reports "used" not input; v2 split
                OutputTokens: 0,
                CostUSD: u.Cost,
                ContextWindow: int(u.Size),
                ContextWindowPct: percent(u.Used, u.Size),
            })
        case "available_commands_update", "current_mode_update",
             "config_option_update", "session_info_update", "plan":
            oLog("[opencode/acp] update received", "kind", head.SessionUpdate)
            // v2: route to runtime (F-slash-command-reactions / F-task-list etc.)
        }
        return nil
    }
}
```

### 4.5 `internal/bridge/opencode/acp_emit_helpers.go`(~150 行,新增)

```go
// decodeTextChunk extracts the text payload from a ContentChunk.
func decodeTextChunk(raw json.RawMessage) string {
    if len(raw) == 0 { return "" }
    var c struct {
        Type string `json:"type"`
        Text string `json:"text"`
    }
    json.Unmarshal(raw, &c)
    if c.Type != "text" { return "" }
    return c.Text
}

// percent computes (used/size)*100 for usage_update, clamped to 0..100.
func percent(used, size int64) float64 {
    if size <= 0 { return 0 }
    p := float64(used) / float64(size) * 100
    if p > 100 { p = 100 }
    if p < 0 { p = 0 }
    return p
}
```

### 4.6 缺失能力补丁(给 `internal/bridge/acp/agent.go`)

#### 4.6.1 `acp.go::driver` 增加 `updateHandler` 字段 + `SetUpdateHandler`

```go
type driver struct {
    /* ... existing fields ... */
    updateHandler UpdateHandler  // nil → use built-in 4-case fallback
}

// UpdateHandler receives raw session/update params.update payloads and
// is responsible for emitting AgentEvents. Returning an error logs the
// error but does NOT kill the stream — wire-level decoding stays tolerant.
type UpdateHandler func(ctx context.Context, d *driver, update json.RawMessage) error
```

**修改点**:
- `driver` struct +1 字段
- `(d *driver) SetUpdateHandler(h UpdateHandler)` (导出方法)
- `handleSessionUpdate` 改成:if `d.updateHandler != nil`,route to it;else fall back to existing switch
- export the `driver` field for access from opencode package... 或者通过 `agent.NewAgent` 出来的 wrapper 提供 setter

后者更好:**给 `agent.Agent` 加一个 `OnSessionUpdate(UpdateHandler)` 配置方法**,跟 `OnText`、`OnToolStart` 之类的扩展点并列。

#### 4.6.2 `agent.go::SendBlocks` 补 Image/File ACP 编码

当前 `acp.SendBlocks` 注释说"Phase 2: encode as proper ACP image/file blocks",目前用 `@<path>` 兜底。

按 ACP v1 spec 的 `PromptRequest.prompt` 是 `ContentBlock[]`,允许类型:

```go
type contentBlock struct {
    Type string `json:"type"`
    Text string `json:"text,omitempty"`
    // Phase 2: extend to:
    //   "image" with {mimeType, data | uri}
    //   "audio" with {mimeType, data | uri}
    //   "resource" with {resource: {uri, mimeType, text | blob}}
}
```

按 ACP schema 实测,`ContentBlock` 子类型需要查;v1 spec 里的 `ContentBlock` 类型允许 `text | image | audio | resource_link | resource`(见 [ACP schema `ContentBlock`](https://github.com/agentclientprotocol/agent-client-protocol/blob/main/schema/v1/schema.json))。opencode acp 实际怎么消费:在 `service.ts` 的 `promptContentToParts`(`./content.ts`)。

**短期方案**:保持 `@<path>` 兜底,但记录 emit `agent.AgentEvent{Kind: EventAgentToolStart, Name: "Read", Args: path}` 让飞书侧看见一个工具调用,**用户/agent 都知道这是图片附件**。这一行为跟 codex bridge 一致。

**中期方案**(F-OPENCODE-ACP-IMAGE):ACP 标准 image block `{type:"image", mimeType:"image/png", data:"<base64>", uri:"file://..."}`。依赖 opencode 上游 promptContentToParts 接受这个 schema。

### 4.7 测试策略

#### 4.7.1 单元测试(纯函数)

`internal/bridge/opencode/update_test.go`:

```go
func TestHandleUpdate_AgentMessageChunk(t *testing.T) { /* ... */ }
func TestHandleUpdate_AgentThoughtChunk_PrependsThinkPrefix(t *testing.T) { /* ... */ }
func TestHandleUpdate_ToolCall_EmitsStart(t *testing.T) { /* ... */ }
func TestHandleUpdate_ToolCallUpdate_Completed_EmitsEnd(t *testing.T) { /* ... */ }
func TestHandleUpdate_ToolCallUpdate_Errored_EmitsEndWithErr(t *testing.T) { /* ... */ }
func TestHandleUpdate_UsageUpdate_StashesUsage(t *testing.T) { /* ... */ }
func TestHandleUpdate_UserMessageChunk_Drops(t *testing.T) { /* ... */ }
func TestHandleUpdate_AvailableCommandsUpdate_LogsButDoesNotEmit(t *testing.T) { /* ... */ }
```

#### 4.7.2 集成测试(mockTransport + mock opencode acp server)

复用 `internal/bridge/acp/acp_test.go` 的 mockTransport + net.Pipe 模式。增加:

```go
func TestOpencodeACP_FullTurn_Text(t *testing.T) {
    // spawn mockTransport that:
    //   1. responds to initialize / session/new
    //   2. on session/prompt, pushes:
    //      session/update {sessionUpdate: "agent_message_chunk", content: {type:"text", text:"hello"}}
    //      session/status {status: {type:"idle"}}
    //   3. expect: EventAgentText "hello", EventAgentDone{Reason:"settled"}
}

func TestOpencodeACP_FullTurn_ToolCall(t *testing.T) {
    // tool_call → tool_call_update:completed
    // expect: EventAgentToolStart, EventAgentToolEnd
}

func TestOpencodeACP_Permission(t *testing.T) {
    // session/request_permission → SendPermission("once") → permission_response
}

func TestOpencodeACP_Stop(t *testing.T) {
    // session/cancel from acp.Stop → mock verifies request was sent, no SIGINT
}
```

#### 4.7.3 真机 e2e

```go
//go:build opencode_real_e2e

func TestE2E_OpencodeACP_FreshSession(t *testing.T) {
    requireRealOpencode(t)
    // Start("opencode", "opencode", ["acp"])
    // SendBlocks([{Type: ContentText, Text: "echo 'hi'"}])
    // expect: EventAgentText "...hi...", EventAgentDone
}

func TestE2E_OpencodeACP_ToolUse(t *testing.T) {
    // prompt that triggers bash tool
    // expect: EventAgentToolStart{Name:"bash"}, EventAgentToolEnd
}
```

跟现有 `real_e2e_unix_test.go` 同样的 `OC_REAL_BIN` 开关,不强制 CI 跑。

#### 4.7.4 print-mode 测试

`print_test.go` + `print_stub_test.go` + `stage2/5` 测试**完整保留**,因为 `opencode run --format json` 不受 ACP 迁移影响。

---

## 5. 注册改动

### 5.1 `cmd/nightme/agents.go`(改 1 行)

```diff
- import "github.com/cnlangzi/nightme/internal/bridge/opencode"
+ import "github.com/cnlangzi/nightme/internal/bridge/opencode"

  func init() {
    /* ... claudecode, codex, dsh ... */
    
-   // opencode — the `opencode serve` HTTP bridge.
-   agent.Builtins.Register(opencode.NewStarter("opencode", "opencode", nil))
+   // opencode — the `opencode acp` ACP bridge. Long-lived chat sessions
+   // go through ACP JSON-RPC over PTY (vendor-recommended path:
+   // https://opencode.ai/docs/acp/, "All features are supported").
+   // One-shot invocations (/gtw commit, buildAgentPrompt) use the
+   // opencode run --format json print-mode spawn (RunOnce).
+   agent.Builtins.Register(opencode.NewACPStarter("opencode", "opencode"))
    
    /* ... pi, bash ... */
  }
```

### 5.2 旧 API 兼容性

- `opencode.NewStarter(name, command, args)` → 删除,改名 `NewACPStarter(name, command)`
- `opencode.Starter.Info()` → 还在,只是 mode 从 `ModeJSONIO` 变成 `ModeACP`
- `opencode.Starter.Detect()` → 还在
- `opencode.Starter.Start()` → 还在,但内部走 acp.NewStarter().Start + opencodeSessionDecorator
- `opencode.Starter.RunOnce()` → 还在,内部走 print.go

任何 import `opencode.NewStarter("opencode-serve", ...)` 的用户配置(罕见,只有自己研究 serve 路径的人)→ 暂时保留一个 `NewHTTPServeStarter` 给 `internal/bridge/opencode-serve/`(如果将来需要做回来),但本期不做。

### 5.3 配置兼容性

用户的 `~/.nightme/config.yaml` 现有:

```yaml
- name: opencode
  bridge: opencode
  command: opencode
```

不需要改。新注册走 acp,`command: opencode` 仍合法,自动 spawn `opencode acp`。

如果用户**坚持要走旧的 HTTP serve**(不可能,但万一),加一个 env 旁路:`NIGHTME_OPENCODE_BRIDGE=serve` → 启动 server.go 子包,给一两个 release 当 escape hatch,然后删除。

---

## 6. 删除清单(落地动作)

执行顺序:**先双注册共存 → 验证 → 切换默认 → 删除 serve**。三个阶段已全部落地(`fix-opencode` 分支):

- **Phase 0**: ✅ 准备完成 — 新建 `opencode` 包(当时路径 `opencode_acp`)+ `UpdateHandler` hook + 本 doc 全部提交
- **Phase 1**: ✅ 默认切换完成 — `cmd/nightme/agents.go` 已注册 `opencode.NewStarter`,`agents_test.go` 期望 `Args: [acp]` 一致
- **Phase 2**: ✅ 删除完成 — `internal/bridge/opencode/` 26 文件 ~12000 行已 git rm
- **收尾**: ✅ `update.go` 从 449 行收敛到 302 行;`SessionView.StashUsage` 与 per-bridge `lastUsage` 跟踪删除


### Phase 0(原计划:本 PR):准备

- 新增 `internal/bridge/opencode/acp.go` + `update.go` + `update_test.go`
- 扩展 `internal/bridge/acp/agent.go` 加 `UpdateHandler` hook(最小侵入,~30 行 diff)
- 写 `docs/feat/F-OPENCODE-ACP-MIGRATION.md`(本文档)
- 双注册:`agents.go` 同时注册 `NewACPStarter("opencode-acp", "opencode")` + 旧的 `NewStarter("opencode", "opencode", nil)`,`opencode` 仍是 serve,`opencode-acp` 是新路径
- CI:跑 `go test -race ./...` 确认全绿

### Phase 1(本 PR):切换默认

- `agents.go`:默认 `opencode` 改成 `NewACPStarter`
- `agents_test.go`:期望 `Args: []string{"acp"}` 一致
- 手动 staging 跑 7 天:`/use opencode`,验证:
  - 普通问答 / 写代码 / 跑 bash / 多 turn
  - `/stop`(session/cancel 不杀进程,下一轮能续)
  - `/new`(走 `session/load` 或 `New` 重置)
  - 图片附件(`@<path>` 路径生效,飞书能看到 Read 工具)
  - 权限请求(permission 卡片按钮能回)
  - usage footer(`usage_update` 落到 footer)

### Phase 2(下个 PR):删除 serve 路径

- `internal/bridge/opencode/{server.go, client.go, transport.go, translate.go, agent.go, driver.go, think_tags.go, testdata/, *_test.go(12 个)}` 一并删除
- 旧 `NewStarter(name, command, args)` 函数删除
- 仅保留 `opencode.go`(常量)+ `acp.go`(~50 行)+ `update.go`(~400 行)+ `print.go`(~600 行)+ 测试
- 验证 `go test -race ./...` 全绿
- 验证 `go vet ./...` 0 warning
- 验证 `go build ./cmd/nightme` 通过

---

## 7. 风险评估 + 缓解

| 风险 | 影响 | 缓解 |
|---|---|---|
| opencode acp 在某些 1.18.x patch 版本有 bug(已知 1.18.18 `setSessionModel` 偶发 noop) | 中:用户切换模型失败 | 本期 v1 不实现 setSessionModel,留 `/api/session/{id}/model` HTTP 备用路径 |
| ACP spec 升级(目前 v1 stable,但 spec 注释"protocolVersion: 1";未来 v2) | 中:未来需适配 | 用 negotiated `protocolVersion` 字段分支处理;`UpdateHandler` 闭包保留 spec-version 感知 |
| 用户历史 session id 在 serve 路径下持久化,切到 acp 后无法 resume | 低:`/api/session/{id}` 是 HTTP 层通用,acp 内部也用 | 让 acp driver 启动时如果 `cfg.SessionID != ""` → fallback 走 `setSessionMode` + `setSessionConfigOption` 还原,或简单强制 fresh session |
| acp protocol 的 `sessionInfoUpdate.title` 没透出到 runtime | 低:飞书侧目前没渲染 session title | v1 不做,留 v2 |
| `plan` 字段没路由到 runtime checklist | 低:飞书侧 /todo 由 codex/claude 路径驱动,opencode 用户少 | v1 log only,v2 wire `EventAgentTaskCreate/Update` |
| image attachment 走 `@<path>` 文本注释,丢失 mime 信息 | 中:agent 看到 `[image: /tmp/x.png]` 注释,自己调 Read 工具看 | 跟 codex 一致,跟旧 opencode-serve 路径的 file://URL 也"接近",acceptable;v1 ship,v2 走 ACP image block |
| PTY 模式下 stdin/stdout echo 干扰 JSON-RPC framing | 低:`acp/agent.go::handleMethod` 已经有 echo filter | 复用,不动 |
| opencode 启动慢(冷启动模型下载 60s+) | 中:用户体验差 | 复用 `acp.handshake` 的 10s startupTimeout,跟旧 serve 一致;让 runtime 报"opencode starting up...",超时返错误 |

---

## 8. 测试矩阵(交付前必跑)

### 8.1 单元 + 集成(mock)

```bash
go test -race ./internal/bridge/opencode/ -count=1
go test -race ./internal/bridge/acp/ -count=1
```

期望:全绿,~20s 完成。

### 8.2 真机 e2e

```bash
NIGHTME_OPENCODE_E2E=1 go test -tags 'unix opencode_real_e2e' \
  -timeout 240s ./internal/bridge/opencode -v -run TestE2E
```

期望:
- `TestE2E_OpencodeACP_FreshSession` PASS(< 60s)
- `TestE2E_OpencodeACP_ToolUse` PASS(< 90s)
- `TestE2E_OpencodeACP_Print` PASS(< 30s,one-shot)

### 8.3 daemon 端到端

手动:
1. `make build && make restart`
2. 启动飞书 daemon,`/use opencode` 切到新路径
3. 发"写一个 hello.go" → 看到飞书收到 hello.go 内容 + bash 输出 + EventAgentResult
4. 发"运行它" → bash tool 起,收到 stdout
5. `/stop` → 立即停
6. 再发"再加注释" → 同一 session 续
7. `/new` → 新 session
8. 飞书侧看 receipt,验证:
   - 工具名称 Bash / Read / Write 正常显示
   - reasoning 部分 `[思考] ...` 正确
   - usage footer `X% (200k)` 正确
   - 权限卡片按钮能回

---

## 9. 文档影响

| 文档 | 改动 |
|---|---|
| ~~`docs/bridge/opencode.md`~~ | 已删除(Phase 2 一并下线,内容迁移到本文档 + `internal/bridge/opencode/` 源码注释) |
| `docs/SPEC.md` §1(组件表) | 不动(opencode bridge 还在,只是实现路径换了) |
| `docs/FEATURES.md` | 在 ACP 集成表添加 opencode 行 |
| `README.md` 章节 "Bridge" | 不动 |
| `CHANGELOG.md` `[Unreleased]` | 加条目:"opencode: 迁移到 ACP 模式;删除 HTTP serve 路径"`bridge/opencode/server.go` `client.go` `translate.go` 移除;新增 `bridge/opencode/update.go` 走 ACP sessionUpdate 路由" |
| `MIGRATION.md` | 加章节:"v0.x → v0.y: opencode 默认走 acp;旧 serve 路径需 `--bridge serve` 才能用" |

---

## 10. 实施 checklist(本 PR)

- [x] `internal/bridge/acp/agent.go`:
  - [x] driver struct 加 `updateHandler UpdateHandler` 字段
  - [x] `SetUpdateHandler(h UpdateHandler)` 导出方法
  - [x] `handleSessionUpdate` 改为 dispatch to handler if set,else existing switch
- [x] ~~`internal/bridge/opencode/opencode.go`~~ → 已删除(Phase 2);常量转移到 `internal/bridge/opencode/opencode.go`
- [x] `internal/bridge/opencode/starter.go`(Phase 0 临时路径 `opencode_acp/starter.go`,Phase 2 旧包删除后改回 `opencode/starter.go`):
  - [x] `Starter` + `NewStarter(name, command, args)`
  - [x] `Info()` 返回 `ModeACP`
  - [x] `Detect()` LookPath
  - [x] `Start()` 委派 `acp.NewStarter(...).Start()` + `drv.SetUpdateHandler(newUpdateHandler(...))`
  - [x] `RunOnce()` 委派 `print.go::runPrintMode`
- [x] ~~`internal/bridge/opencode/update.go`~~ → **已不需要**。opencode 直接走 generic fallback（见 [docs/bridge/acp.md §2.1](../../bridge/acp.md)）；`usage_update` / `session.status` / `session_info_update` / `agent_thought_chunk` 等全部由 generic 接管。仅当 opencode 引入 vendor-private 协议时才需要落地 `update.go`（§4.2 设计保留作模板）。
- [x] ~~`internal/bridge/opencode/update_test.go`~~ → **已不需要**（原因同 update.go）。通用 fallback 的测试在 `internal/bridge/acp/deliver_stamp_test.go` 覆盖（13 个新测试）。
- [x] ~~`internal/bridge/opencode/acp_integration_test.go`~~ → 不再需要,acp 桥本身的 `acp_test.go` 已覆盖;opencode 端的 e2e 走 `internal/bridge/opencode/starter_test.go` 的 Detect 路径
- [x] `cmd/nightme/agents.go`:
  - [x] 改默认 `opencode` 为 `opencode.NewStarter("opencode", "opencode", []string{"acp"})`
  - [x] 注释更新:指 `https://opencode.ai/docs/acp/` "All features are supported"
- [x] `cmd/nightme/agents_test.go`:
  - [x] 验证 `Args: []string{"acp"}` 期望 PASS
- [x] ~~`docs/bridge/opencode.md`:重写~~ → 不重写;直接删除(本期随 Phase 2 一并落地)。`docs/FEATURES.md` OpenCode Bridge 行改指本文档;`docs/bridge/dsh.md` 中"HTTP+SSE 同构"姊妹链接删除(opencode 已不是 HTTP+SSE)
- [x] `docs/feat/F-OPENCODE-ACP-MIGRATION.md`:写完(本文档,顶部 Status 已翻 ✅)
- [x] `CHANGELOG.md`:加条目(`[Unreleased]` 块)
- [x] `MIGRATION.md`:加章节("opencode bridge migration: HTTP serve → ACP (Phase 2)")

---

## 11. 后续(v2+ 留待 issue)

- [ ] `Plan` → `EventAgentTaskCreate/Update` 路由(需要 agent.Agent 加 `EmitTaskList` 扩展点)
- [ ] `available_commands_update` → runtime 路由到 slash command(等 slash-command-reactions 落地)
- [ ] `current_mode_update` / `config_option_update` 路由(等 opencode 1.19+ 稳定)
- [ ] ACP `session/load` 真正的 resume 路径(目前走 `cfg.SessionID` 启动 + `setSessionMode/Option`)
- [ ] ACP image / file block(目前 `@<path>` 兜底)
- [ ] `setSessionModel` (目前 HTTP 路径)
- [ ] `unstable_forkSession` (等 spec stable)
- [ ] `authenticate` JSON-RPC 调用(等第三方 provider auth 场景)

---

## 12. 时间线

| 阶段 | 内容 | 估时 |
|---|---|---|
| Phase 0 — 准备 | acp.go hook + update.go + 单元/集成测试 + 本设计 doc | ✅ 1-2 天(实际) |
| Phase 1 — 切换默认 | agents.go 切换 + agents_test 验证 + CHANGELOG/MIGRATION 更新 | ✅ 0.5 天(实际) |
| Phase 2 — 删除 serve | 删除 ~12000 行旧 code(26 文件) | ✅ 0.5 天(实际,合并到此分支 + 修 1 处 merge conflict + 1 处 build error) |
| 收尾 — update.go 收敛 | 删除 6 个不发出的 sessionUpdate 分支 + StashUsage | ✅ 0.5 天(实际) |
| **总计** | — | **~1 周**(实际 ~3 天,合并冲突意外耗时) |
