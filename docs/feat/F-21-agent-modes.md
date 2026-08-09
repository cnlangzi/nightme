# F-21: Agent Communication Modes (ACP | SDK | PTY | JSON-IO)

> **Status**: implemented (v0.2: ACP + SDK fallback + PTY) / v0.2 extension (JSON-IO for Claude Code)
> **Milestone**: M1 architecture / v0.1 PTY fallback / v0.2 ACP and SDK adapter / v0.2 JSON-IO for Claude Code
> **Depends on**: F-09 (Agent abstraction), F-19 (CLI Bridge)
> **Related docs**: SPEC.md §1.1 (Agent), [F-09-agent-abstraction.md](./F-09-agent-abstraction.md), [F-19-cli-bridge.md](./F-19-cli-bridge.md), [F-24-claudecode-bridge.md](./F-24-claudecode-bridge.md)

## 1. Description

不同 AI Coding CLI 提供不同程度的"可控性"。nightme 的策略是**优先用最标准的协议**，避免 vendor lock-in：

| 优先级 | Mode | 适用 | UX | 实施难度 |
|--------|------|------|-----|----------|
| **1 (baseline)** | **ACP**（Agent Client Protocol）| Codex、OpenCode、未来 ACP agent | 好：结构化事件、权限确认、工具进度 | 中：协议标准、实现一次复用多 CLI |
| **2 (vendor-specific)** | **SDK**（CLI 自定义 SDK）| Claude Code（Anthropic 暂不支持 ACP）| 优：原生体验 | 高：每个 CLI 单独写，vendor lock-in |
| **3 (JSON-IO)** | **JSON-IO**（专用 stream-json 模式）| Claude Code（v0.2+）| 优：结构化 event + AskUserQuestion 渲染 | 中：每个 CLI 独立 bridge package |
| **4 (fallback)** | **PTY 透传** | 任何不支持以上三种的 CLI | 差：ANSI / 进度条 / spinner 乱码 | 低：通用 byte pipe |

> **v0.2 新增 ModeJSONIO**：Claude Code 走专用 bridge（不用 SDK 也不用 PTY）。详细设计见 [F-24-claudecode-bridge.md](./F-24-claudecode-bridge.md)。

**设计原则**：
- **ACP 优先**——标准化协议，nightme 不需要为每个 CLI 写 vendor-specific 代码
- **SDK 是过渡方案**——当 CLI 不支持 ACP 时才用（如 Claude Code 现状）
- **PTY 是最后兜底**——保证任何 CLI 至少能跑

## 2. 为什么 ACP 第一

1. **标准化**：ACP 是 Agent Client Protocol 的开源标准（Happy Coder 已用它接 Codex）
2. **复用性**：nightme 写一个 ACP client 适配器，所有支持 ACP 的 CLI 都能用
3. **降低差异化要求**：nightme 的价值在 channel 抽象 + 编排层，**不**在 agent-specific 知识
4. **未来扩展**：新 CLI（如 OpenCode、未来的 Anthropic agent）如果支持 ACP，nightme 自动适配
5. **UX 够用**：ACP 提供结构化事件（权限、工具调用），PTY 没法比

**SDK 排第二**：因为它是 vendor-specific（如 Claude Code Agent SDK）。如果未来 Anthropic 加入 ACP，nightme 可以下掉 SDK adapter。

**PTY 排最后**：因为 UX 差（ANSI 乱码、spinner 刷屏）。只在前面两个都不支持时才用。

## 3. 模式选择决策树

```
nightme 启动 session 时:
  agent.Mode() 返回什么？
  ├─ ModeACP  → 用 ACP client（首选）
  │              → spawn ACP server 二进制
  │              → JSON-RPC 握手
  │              → 消费 ACP events → 转换 AgentEvent
  │              → AgentSession 包装 ACP 连接
  ├─ ModeSDK  → 用 vendor-specific SDK
  │              → 调 SDK client（如 Claude Code Agent SDK）
  │              → SDK 原生返回结构化 events
  │              → AgentSession 包装 SDK connection
  ├─ ModeJSONIO → 用 CLI 自家的 stream-json 模式
  │              → spawn `claude --input-format stream-json --output-format stream-json ...`
  │              → parse JSON events（system / assistant / tool_use / tool_result / result）
  │              → AskUserQuestion 双路兼容（tool_use 拦截 + text fallback）
  │              → AgentSession 包装 claudecode.Transport
  └─ ModePTY  → 兜底（任何 CLI 都能用）
                 → spawn CLI 在 PTY 中
                 → read goroutine 把 bytes 转 TextEvent
                 → AgentSession 包装 pty.Transport
```

**关键**：SessionManager 跟 AgentSession 交互，**不直接**跟 PTY/ACP/SDK 交互。所有模式都返回统一的 `AgentSession` 接口。

## 4. Agent 接口（统一抽象）

```go
// internal/agent/agent.go
package agent

type Mode int
const (
    ModeACP Mode = iota  // 优先：Agent Client Protocol
    ModeSDK              // vendor-specific（如 Claude Code SDK）
    ModePTY              // 兜底：透明透传
    ModeJSONIO           // 专用 stream-json 模式（如 Claude Code v0.2+）
)

type EventKind int
const (
    EventText EventKind = iota
    EventPermission
    EventToolStart
    EventToolEnd
    EventDone
    EventError
)

type AgentEvent struct {
    Kind       EventKind
    Text       string
    Permission *PermissionRequest
    ToolStart  *ToolStartEvent
    ToolEnd    *ToolEndEvent
    Done       *DoneEvent
    Error      *ErrorEvent
}

type PermissionRequest struct {
    Tool       string         // "Bash" / "Write" / etc.
    Action     string         // human-readable description
    Options    []string       // ["once", "session", "reject"]
    ResponseCh chan string    // handler 写入用户选择
}

type Agent interface {
    Name() string
    Mode() Mode
    Detect() error  // binary / SDK availability check
    
    Start(ctx context.Context, cfg StartConfig) (AgentSession, error)
}

type StartConfig struct {
    Workspace string
    Args      []string
    Env       []string
}

type AgentSession interface {
    Events() <-chan AgentEvent      // 结构化事件流
    SendText(text string) error     // 发送用户输入
    SendPermission(resp string) error  // 响应权限请求（ACP/SDK 模式）
    Close() error
}
```

## 5. 三个 Mode 实现（实施顺序）

### 5.1 ModeACP (v0.2 implemented)

**为什么 ACP 优先实施**：
- Codex 已支持（Happy Coder 验证）
- OpenCode 可能支持
- 一个 adapter 服务多个 CLI
- 提供结构化事件

**实现**：
```go
// internal/agent/acpagent/acpagent.go (v0.1)
type Agent struct {
    name    string  // "codex", "opencode"
    command string  // ACP server 二进制路径
    args    []string
}

func (a *Agent) Mode() Mode { return ModeACP }

func (a *Agent) Start(ctx context.Context, cfg StartConfig) (AgentSession, error) {
    // 1. spawn ACP server（CLI 在 PTY 中跑，作为 ACP server）
    bridge, err := pty.New(cfg.Workspace, a.command, a.args...)
    if err != nil { return nil, err }
    
    // 2. 启动 ACP client，通过 stdin/stdout 与 server 通信
    //    ACP 是 JSON-RPC over stdio
    client := acp.NewClient(bridge)
    
    // 3. 握手 + 启动 session
    if err := client.Initialize(ctx); err != nil {
        bridge.Close()
        return nil, err
    }
    if err := client.NewSession(ctx, cfg.Workspace); err != nil {
        bridge.Close()
        return nil, err
    }
    
    return &acpSession{client: client, bridge: bridge, events: make(chan AgentEvent, 64)}, nil
}

type acpSession struct {
    client *acp.Client
    transport pty.Transport          // ACP 通信的物理载体
    events chan AgentEvent
}

func (s *acpSession) Events() <-chan AgentEvent { return s.events }
func (s *acpSession) SendText(text string) error { return s.client.SendPrompt(text) }
func (s *acpSession) SendPermission(resp string) error {
    return s.client.SendPermissionDecision(resp)
}
func (s *acpSession) Close() error {
    s.client.Close()
    return s.bridge.Close()
}

// background: 把 ACP events 转 AgentEvent
func (s *acpSession) pump() {
    for acpEvent := range s.client.Events() {
        switch acpEvent.Type {
        case "message_chunk":
            s.events <- AgentEvent{Kind: EventText, Text: acpEvent.Content}
        case "permission_request":
            s.events <- AgentEvent{
                Kind: EventPermission,
                Permission: &PermissionRequest{
                    Tool:    acpEvent.Tool,
                    Action:  acpEvent.Description,
                    Options: acpEvent.Options,
                    ResponseCh: make(chan string, 1),
                },
            }
        case "tool_start":
            s.events <- AgentEvent{Kind: EventToolStart, ToolStart: &ToolStartEvent{...}}
        case "tool_end":
            s.events <- AgentEvent{Kind: EventToolEnd, ToolEnd: &ToolEndEvent{...}}
        case "session_end":
            s.events <- AgentEvent{Kind: EventDone, Done: &DoneEvent{ExitCode: 0}}
            close(s.events)
            return
        }
    }
}
```

**ACP 的好处**：
- ✅ 结构化权限请求（IM 渲染为按钮）
- ✅ 工具调用进度可视化
- ✅ 无 ANSI 垃圾
- ✅ session 状态机标准化
- ✅ 一个 adapter 服务所有 ACP-supporting CLIs

### 5.2 ModeSDK（v0.2 实施 — vendor-specific）

**仅当 CLI 不支持 ACP 时才用**。当前只有 Claude Code 走这条路。

**实现**：
```go
// internal/agent/claudesdk/claudesdk.go (v0.2)
type Agent struct{}

func (a *Agent) Mode() Mode { return ModeSDK }
func (a *Agent) Detect() error { return nil }  // SDK 是 Go 库，无 binary 检查

func (a *Agent) Start(ctx context.Context, cfg StartConfig) (AgentSession, error) {
    client, err := claudecode.NewClient(claudecode.Options{
        Cwd:  cfg.Workspace,
        Args: cfg.Args,
        Env:  cfg.Env,
    })
    if err != nil { return nil, err }
    
    session := client.NewSession()
    return &claudeSession{client: client, session: session, events: make(chan AgentEvent, 64)}, nil
}

type claudeSession struct {
    client  *claudecode.Client
    session *claudecode.Session
    events  chan AgentEvent
}

// SendText / SendPermission / Close / Events 类似 acpSession
// 区别在于 client 是 SDK（不是 JSON-RPC）
```

**SDK 模式的取舍**：
- 优点：Claude Code 原生体验，无 vendor-specific 协议限制
- 缺点：nightme 必须懂 Claude Code SDK；将来 Anthropic 改 SDK，nightme 要跟进
- v0.2 加；如果将来 Claude Code 支持 ACP，可以下掉 SDK adapter

### 5.3 ModePTY（v0.1 实施 — 兜底）

**作为 ACP/SDK 都不支持时的最后兜底**。当前已经在做。

**实现**：
```go
// internal/agent/ptyagent/ptyagent.go (v0.1)
type Agent struct {
    name    string
    command string
    args    []string
}

func (a *Agent) Mode() Mode { return ModePTY }

func (a *Agent) Start(ctx context.Context, cfg StartConfig) (AgentSession, error) {
    bridge, err := pty.New(cfg.Workspace, a.command, a.args...)
    if err != nil { return nil, err }
    
    s := &ptySession{bridge: bridge, events: make(chan AgentEvent, 64)}
    go s.readLoop()
    return s, nil
}

type ptySession struct {
    transport pty.Transport
    events    chan AgentEvent
}

func (s *ptySession) Events() <-chan AgentEvent { return s.events }
func (s *ptySession) SendText(text string) error { return s.transport.Write([]byte(text)) }
func (s *ptySession) SendPermission(resp string) error {
    return s.transport.Write([]byte(resp))  // best-effort：PTY 不理解"permission"，原样写入
}
func (s *ptySession) Close() error { return s.bridge.Close() }

func (s *ptySession) readLoop() {
    buf := make([]byte, 4096)
    for {
        n, err := s.bridge.Read(buf)
        if err != nil {
            s.events <- AgentEvent{Kind: EventDone, Done: &DoneEvent{ExitCode: -1}}
            close(s.events)
            return
        }
        s.events <- AgentEvent{Kind: EventText, Text: string(buf[:n])}
    }
}
```

**PTY 的局限**（已知）：
- 只产生 `TextEvent`，没有 `PermissionRequest` / `ToolStart` / `ToolEnd`
- 权限确认靠用户手动输入 `Y` / `n`（看到 "Allow? [Y/n]" 后输）
- ANSI / 进度条 / spinner 在 IM 显示为乱码

**v0.1 实际用法**：
- Claude Code 暂时走 PTY；v0.2 registry 已切到 ModeSDK seam，但 Go SDK 缺失时明确返回 `ErrNotImplemented`
- 未知 CLI 走 PTY


### 5.4 ModeJSONIO（v0.2 实施 — Claude Code 专用）

**适用**：Claude Code（Anthropic CLI）。使用 `--input-format stream-json --output-format stream-json` 模式 + `--permission-mode bypassPermissions`。

**详细设计**：[F-24-claudecode-bridge.md](./F-24-claudecode-bridge.md)。

**核心机制**：

```go
// internal/bridge/claudecode/claudecode.go

type Agent struct{}

func (a *Agent) Mode() Mode { return ModeJSONIO }

func (a *Agent) Start(ctx context.Context, cfg StartConfig) (AgentSession, error) {
    cmd := exec.CommandContext(ctx, "claude",
        "--print",
        "--input-format", "stream-json",
        "--output-format", "stream-json",
        "--permission-mode", "bypassPermissions",
        "--verbose",
    )
    cmd.Dir = cfg.Workspace
    // ... spawn + pipe stdin/stdout
    
    session := &claudeSession{
        cmd: cmd, stdin: stdin, stdout: stdout,
        events: make(chan AgentEvent, 64),
    }
    go session.pumpStdout()  // parse stream-json events
    return session, nil
}
```

**为什么不用 SDK**：
- Anthropic 官方 Claude Agent SDK 只发布 Python / TypeScript 版本
- 没有官方 Go SDK
- ModeSDK seam 在 v0.1 是占位（`ErrNotImplemented`）

**为什么不用 PTY**：
- PTY 看不到结构化 event（只有 raw bytes + ANSI 垃圾）
- 权限确认靠用户手动输 `Y\n`
- AskUserQuestion 工具的卡片渲染不可能（PTY 看不出 tool_use）

**为什么不用 ACP**：
- Claude Code 暂不支持 ACP
- 如果未来支持，nightme 可以下掉 JSON-IO bridge 切 ACP

**v0.2 决策**：每 CLI 独立 bridge package（per-agent bridge 架构），不复用 PTY/ACP/SDK 基础设施。

### 5.5 v0.2 implementation notes

#### ACP wire protocol

The bridge implements stable ACP v1 over newline-delimited JSON-RPC 2.0. `internal/bridge/acp/rpc.go` owns request IDs, concurrent writes, response correlation, and JSON-RPC error decoding. `internal/bridge/acp/session.go` starts the read pump before the handshake, sends:

1. `initialize` with `protocolVersion: 1`, `clientInfo`, and conservative filesystem/terminal capabilities;
2. `session/new` with `cwd` and an empty `mcpServers` list; and
3. `session/prompt` for each `SendText` call.

Stable ACP uses `session/update` notifications. The adapter maps `agent_message_chunk` to `EventText`, `tool_call` / `tool_call_update` to tool events, and `session/request_permission` requests to `EventPermission`. The older `message_chunk`, `permission_request`, `tool_start`, `tool_end`, and `session_end` names from the original design are accepted as compatibility aliases for mock servers and early adapters. ACP v1 has no required `session_end` notification; EOF is mapped to `EventDone{ExitCode: -1}`.

The PTY is only the physical carrier. Invalid non-JSON banner lines are ignored so a CLI banner cannot corrupt JSON-RPC framing. `Close` closes the transport; it does not wait for an optional `shutdown` method because ACP v1 sessions use `session/close` or process lifetime rather than a universal JSON-RPC shutdown handshake.

#### Agent launch commands

- **OpenCode** exposes ACP as `opencode acp`, not `opencode --acp`; the example config uses `command: opencode` and `args: [acp]`.
- **Codex CLI** exposes `codex app-server --listen stdio://` as a first-party JSON-RPC 2.0 transport. The bridge spawns it directly (no ACP middleware like `@agentclientprotocol/codex-acp`); the example config uses `command: codex`. See [docs/bridge/codex.md](../bridge/codex.md) for the wire contract and rationale for not supporting the legacy `codex exec` backend.
- Custom agent names remain ModePTY. A configured `claude`, `codex`, or `opencode` selects SDK, ACP, or ACP respectively; a path passed directly to the CLI remains PTY.

#### Claude Code SDK finding

Anthropic's official Claude Agent SDK is currently published for Python and TypeScript only. There is no official `github.com/anthropics/claude-code-sdk-go` package. `github.com/anthropics/anthropic-sdk-go` is the Messages API client, not the Claude Code agent runtime, and would require reimplementing the tool loop and built-in tools. Therefore `internal/bridge/sdk` implements the requested ModeSDK surface and returns `ErrNotImplemented` with a PTY fallback instruction. When Anthropic publishes an official Go Agent SDK, this adapter is the narrow replacement point.

#### Deliberate deviations and deferred work

- The original design's `new_session` / `send_message` names are represented by ACP v1's `session/new` / `session/prompt` methods.
- Initial prompts are sent after session creation through `SendText`; `StartConfig` has no initial-message field.
- Permission responses use the ACP request response ID and `outcome.selected.optionId`, rather than a second request, while the legacy notification shape is still accepted for compatibility.
- ACP filesystem, terminal, authentication, session persistence, resize, attachment, and MCP forwarding are not implemented in this sub-task. They are v0.3+ work.

## 6. v0.2 Agent 注册表

注册表由配置驱动（`cmd/nightme/test.go`）；实现选择保持在统一 `agent.Agent` 接口之后：

```go
// claude -> ModeSDK (SDK adapter currently returns ErrNotImplemented)
// codex -> ModeJSONIO (codex app-server; see docs/bridge/codex.md)
// opencode -> ModeACP (opencode acp)
// unknown configured names and direct binary paths -> ModePTY
```

## 7. Channel 渲染（事件 → IM）

Channel Adapter 接收 `AgentSession.Events()` 的统一事件流：

- PTY mode sends raw `EventText` bytes, including any ANSI sequences.
- ACP mode can render `EventPermission`, `EventToolStart`, and `EventToolEnd` as structured UI elements.
- SDK mode has the same target event model; the current Go adapter reports its unavailable SDK explicitly.

## 8. SessionManager 变化
type Session struct {
    ChatID       string
    Workspace    string
    AgentName    string
    Args         []string
    agentSession agent.AgentSession  // 统一接口，不直接持有 pty.Transport
    cancel       context.CancelFunc
}

func (m *Manager) Create(ctx context.Context, req CreateRequest) (*Session, error) {
    a, err := agent.Get(req.Agent)
    if err != nil { return nil, err }
    
    agentSession, err := a.Start(ctx, agent.StartConfig{
        Workspace: req.Workspace,
        Args:      req.Args,
    })
    if err != nil { return nil, err }
    
    s := &Session{
        ChatID:       req.ChatID,
        Workspace:    req.Workspace,
        AgentName:    req.Agent,
        Args:         req.Args,
        agentSession: agentSession,
    }
    
    go s.pumpEvents(ctx)  // 消费 AgentSession.Events() → 推给 Channel
    return s, nil
}
```

## 9. 实施顺序（roadmap）

| 阶段 | Mode | 工作量 | 状态 |
|------|------|--------|------|
| **v0.1** | **PTY** | 已完成 | 通用 byte pipe 兜底 |
| **v0.2** | **ACP** | 已完成 | stable ACP v1 JSON-RPC bridge |
| **v0.2** | **SDK** | 已完成（fallback） | Go Agent SDK 不存在，保留 ModeSDK seam |
| **v0.3+** | 优化 | - | ACP capability expansion, SDK when available, persistence |

**v0.2 已完成**：ACP session handshake/event mapping、Claude ModeSDK adapter seam、配置驱动的 Claude/Codex/OpenCode mode selection，以及 PTY fallback。

**未来可能的演进**：
- Anthropic 发布官方 Go Claude Agent SDK → replace `internal/bridge/sdk` sentinel with native session adapter
- Claude Code 支持 ACP → prefer ACP and remove the vendor-specific path
- Add ACP filesystem/terminal/MCP capabilities only with explicit permission and security review

## 10. Edge cases

| 场景 | 处理 |
|------|------|
| Agent Mode=ACP 但 ACP server 启动失败 | Start 返回 error → 报错 "ACP server unavailable" |
| Agent Mode=ACP 但 ACP server crash | AgentSession.Events() 收到 error event → SessionManager 标记 session exited |
| Agent Mode=ACP 但 handshake 失败 | Start 返回 error |
| Agent Mode=PTY 但 binary 不存在 | Detect 提前检查，Start 返回 error |
| PTY mode 下用户需要权限确认 | 用户看到 "Allow? [Y/n]"，手动输 `Y\n` |
| ACP mode 下用户需要权限确认 | JSON-RPC permission request 映射为 `EventPermission`，用户选择后发送 response ID |
| Channel 不支持 PermissionRequest 渲染 | 降级为文本（"Permission: X，respond 'yes' or 'no'"）|
| SDK adapter 不可用 | 返回 `sdk.ErrNotImplemented`，用户改用 PTY |
| nightme 重启，session 已 detach（ACP mode）| v0.3 评估 ACP session persistence |

## 11. Test plan

**单元测试**：
- ptySession.readLoop 正确把 bytes 转 TextEvent
- ptySession.SendText / SendPermission / Close 正确
- acpSession 把 ACP v1 events 及 legacy aliases 转 AgentEvent
- acpSession.SendPermission 正确发给 ACP server
- SDK adapter Detect / Mode / ErrNotImplemented

**集成测试**：
- mock ACP server（JSON-RPC）→ initialize + session/new + prompt + event mapping
- mock SDK fallback → ErrNotImplemented
- mock PTY → 同上

## 12. Open questions

- ACP v1 capability negotiation: when should filesystem, terminal, auth, and MCP methods be enabled?
- ACP v2 is experimental; v0.3 should pin a schema version before adding v2-only methods.
- Should an ACP failure fall back automatically to PTY, or require an explicit user/config choice?
- How should Feishu render permission options and long-running tool progress? Separate UX work remains.
- When Anthropic publishes an official Go Agent SDK, should nightme switch from the current sentinel to that adapter, or continue using Claude Code CLI PTY as a compatibility fallback?
