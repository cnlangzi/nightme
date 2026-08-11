# CLI Transport (PTY Byte Pipe + Agent Communication Modes)

## A1. F-19: PTY Mode Byte Pipe (Bridge PTY Implementation)

> **Source**: `../bridge/cli-transport.md`


> **Depends on**: F-04 (PTY), F-08 (Channel)

> **Related docs**: SPEC.md(Bridge 组件), [../bridge/cli-transport.md](./../bridge/cli-transport.md) (Bridge 四层模式), [F-gateway.md §2.3](./F-gateway.md) (single-consumer), FEATURES.md

本文档回答：**nightme 的 Bridge 在 PTY 模式下怎么把 CLI 的 TTY 字节流搬到飞书、再把飞书的字节流搬回 TTY**。

---

## 1. 核心约束

nightme 是 **byte pipe**，不是 **translator**：

| 方向 | 模式 |
|------|------|
| Channel → PTY stdin | **纯字节透传**。用户发什么，PTY 收什么（除 \r\n → \n 标准化） |
| PTY stdout/stderr → Channel | **保留 ANSI 转义码**，Channel adapter 决定怎么渲染 |

**绝对不做**：
- 不解析 CLI 输出识别 "success" / "failure"
- 不识别 prompt / 等待输入状态
- 不替换 emoji / 颜色 / 进度条
- 不主动补全 / 联想

如果用户想看效果，他应该在 macOS 上手动跑 Claude Code，看终端长什么样；飞书上能保留 80% 视觉一致性就够了。

---

## 2. PTY ↔ Channel 协议

### 2.1 Channel → PTY

```
┌─────────────────────────────────────────────┐
│ 飞书消息: "请帮我修复 login bug"            │
│     │                                       │
│     │ Channel adapter 提取 text              │
│     ▼                                       │
│ raw = "请帮我修复 login bug"                │
│     │                                       │
│     │ 标准化：去除 \r，统一 \n              │
│     ▼                                       │
│ normalized = "请帮我修复 login bug\n"        │
│     │                                       │
│     │ Bridge.Write(normalized)               │
│     ▼                                       │
│ PTY master fd → PTY slave → claude stdin   │
└─────────────────────────────────────────────┘
```

**关键点**：
- 飞书用户**手动**按回车发消息，所以用户消息自带 `\n` 或 `\r\n`
- 如果用户发的是单行文本（不带 `\n`），nightme 自动补 `\n`（避免 Claude Code 等不到 Enter）
- 飞书富文本（如 @、图片）直接丢弃图片，只保留纯文本

### 2.2 PTY → Channel

```
┌─────────────────────────────────────────────┐
│ claude stdout:                              │
│   "\x1b[32m✓\x1b[0m Updated file login.ts\n"│
│   "Checking tests...\n"                      │
│   "\x1b[?25l"  ← 隐藏光标                   │
│     │                                       │
│     │ PTY master fd Read(buf 4KB)            │
│     ▼                                       │
│ raw bytes: b"\x1b[32m✓\x1b[0m Updated..."  │
│     │                                       │
│     │ 缓冲 + 聚合窗口（200ms 或 4KB）         │
│     ▼                                       │
│ chunk = "\x1b[32m✓\x1b[0m Updated file..." │
│     │                                       │
│     │ Channel adapter.SendLongMessage()      │
│     │ （>4KB 自动分段）                       │
│     ▼                                       │
│ 飞书消息 1 (4KB)                             │
│ 飞书消息 2 (剩余)                             │
└─────────────────────────────────────────────┘
```

**关键点**：
- 飞书单条消息上限 ~4KB（markdown text 实际 4096 字符）
- `SendLongMessage` 自动切分，保留 ANSI 码完整性（不在 escape sequence 中间断开）
- 200ms 聚合窗口：减少消息条数，避免飞书"刷屏"触犯 QPS 限制

---

## 3. 缓冲与聚合

### 3.1 为什么需要聚合？

Claude Code 一次操作可能产生 50+ 个 stdout write（如进度更新），如果不聚合：
- 飞书群发：~50 条消息
- 飞书 QPS：单聊 5 QPS，超限后消息会被吞
- 用户体验：手机通知刷屏

### 3.2 聚合策略

```go
type Aggregator struct {
    buf       []byte
    maxSize   int           // 4KB
    maxWait   time.Duration // 200ms
    flushCh   chan []byte
    timer     *time.Timer
    mu        sync.Mutex
}

func (a *Aggregator) Write(p []byte) {
    a.mu.Lock()
    a.buf = append(a.buf, p...)
    if len(a.buf) >= a.maxSize {
        a.flushLocked()
    } else if a.timer == nil {
        a.timer = time.AfterFunc(a.maxWait, a.flushFromTimer)
    }
    a.mu.Unlock()
}
```

**触发 flush 的条件**（任一满足）：
1. buffer ≥ 4KB（飞书单条上限）
2. 距离上次 flush 满 200ms
3. 检测到 PTY 空闲（无新数据 500ms）— 加
4. 检测到 prompt / 等待输入模式 — 不做（nightme 不识别 prompt）

### 3.3 ANSI 处理

**策略**：保留 ANSI 转义码，发到飞书。
- 飞书 markdown 不渲染 ANSI（会显示成乱码 `^[[32m`）
- 但飞书**支持** `<text color="green">` 等富文本标签（card 模式）
- 选择最简单：**保留为字面量字符串**，用户看到 ANSI 字面量

**优化**：
- 检测 ANSI 颜色码，转换成飞书富文本标签
- 或：把 ANSI 编码后的内容用图片渲染（puppeteer 截图）→ 发图片
- 再讨论

**接受 ANSI 显示乱码**的理由：
- 用户多数情况下看 Claude Code 是文字内容，ANSI 只是装饰
- 飞书支持"代码块"语法（```\`\`\` ```），可以把整段输出塞代码块，可读性还行
- 简单优于完美

---

## 4. 特殊字符 / 控制序列

### 4.1 必须正确处理的

| 序列 | 含义 | 处理 |
|------|------|------|
| `\n` / `\r\n` | 换行 | 透传 |
| `\r` | 回车 | 转 `\n`（飞书用 `\n` 换行） |
| `\x1b[2J` | 清屏 | 透传（飞书会显示为空白） |
| `\x1b[H` | 光标归零 | 透传 |
| `\x1b[?25l/h` | 隐藏/显示光标 | 透传 |
| `\x1b[8m` / `\x1b[28m` | 隐藏/显示密码 | **透传**（密码与普通文本一视同仁，详见 PRD §4.1）|

### 4.2 输入方向（Channel → PTY）

| 输入 | 处理 |
|------|------|
| 普通文本 | 透传 + 自动补 `\n`（如果用户没发） |
| `\r` 或 `\r\n` | 转 `\n` |
| 粘贴多行 | 飞书粘贴板原样转发，PTY 自行处理 |
| 飞书 @机器人 | 丢弃 @ 前缀，保留正文 |
| 飞书 emoji | 转 UTF-8 字节透传（PTY 一般能显示） |
| 图片 / 文件 | 丢弃，提示 "not supported" |

### 4.3 密码 / API key 处理（已决策）

nightme 对密码 / API key 与普通文本一视同仁——**完全透传**，不做任何过滤、重定向或检测。

**决策依据**：见 [PRD §4.1](../PRD.md#41-完全透传不解析)。 nightme 的"完全透传"哲学。

**实际行为**：
- 用户在 IM 输入密码 → 原样转给 PTY stdin（跟普通字符完全一样）
- Claude Code 收到密码 → 进入正常登录流程
- 密码出现在 IM 聊天记录中（飞书侧的存储 = 飞书的责任）
- nightme 日志 redact 密码字符串（避免本地泄露），但**不**阻止传输

**用户须知**：nightme README 应明确提示"飞书聊天记录包含密码 / API key"作为知情同意。

---

## 5. 错误恢复

### 5.1 PTY 异常关闭

```
Claude Code crash → PTY EOF → bridge.Read() returns io.EOF
  ↓
Session.readPump 退出
  ↓
readPump 标记 sess.Status → Exited + EventDone 事件
  ↓
Manager.EventCallback 触发 → Gateway.OnSessionEvent → Translate → channel.Send(OutboundMessage{Kind: OutText, Text: "Session ended (exit code: {code})"})
  ↓
registry.UpsertSession(StatusExited)
```

### 5.2 Channel 断连（飞书）

飞书 WebSocket 断连 → lark-oapi SDK 自动重连（指数退避 1s → 2s → 4s → ... → 60s）。
期间 PTY 输出会**丢失**（nightme 不知道发给谁）。

**行为**：
- 断连期间 PTY 输出写入 "buffered" channel（无接收者即丢弃）
- 重连后 next message 处理正常
- **不补偿历史消息**（避免飞书刷屏）

**改进**：
- 断连时 PTY 输出暂存到 session 内存 buffer
- 重连后发 "while you were offline: ..." 摘要

### 5.3 nightme 重启

```
nightme SIGTERM
  ↓
1. Channel.Stop() — 优雅停 WebSocket
2. SessionManager 遍历所有 session:
     - 不 kill PTY 子进程（默认策略）
     - 标记 session 为 "detached"
3. registry 持久化（包括 detached 状态）
4. 主进程退出

下次 nightme 启动:
  ↓
1. 加载 registry
2. 对每个 detached session:
     - 检查 PID 是否还活着
     - 活着 → 自动 reattach（重建 readPump/writePump）
     - 死了 → 删除 registry 记录
```

---

## 6. 性能预算

| 指标 | 预算 | 实测 |
|------|------|------|
| Channel → PTY 延迟 | < 50ms（P50）/ < 200ms（P99）| TBD |
| PTY → Channel 延迟 | < 500ms（聚合窗口上限）| TBD |
| PTY 读取吞吐 | 1MB/s 单 session | TBD |
| 并发 session 数 | 50（单 laptop 上限）| TBD |
| nightme 内存占用 | < 50MB（10 sessions idle）| TBD |
| nightme 启动时间 | < 2s | TBD |

**性能测试方法**：
- 启动 10 个 session，每个跑 `cat /dev/urandom` 灌数据
- 持续 5 分钟，观察 CPU / 内存 / 飞书 QPS

---

## 7. 实现注意（给开发者）

### 7.1 goroutine 生命周期

```go
// 每个 Session 两个 goroutine
func (s *Session) readPump(ctx context.Context) {
    defer s.cleanup()
    buf := make([]byte, 4096)
    for {
        n, err := s.bridge.Read(buf)
        if err != nil { return }  // PTY closed
        // 发送到 outputStream（buffered channel）
        select {
        case s.outputStream <- buf[:n]:
        case <-ctx.Done():
            return
        }
    }
}
```

**关键点**：
- `outputStream` 是 buffered channel，buffer = 16
- 如果 Channel adapter 处理慢（飞书 QPS 限），outputStream 满了怎么办？
  - **丢弃**（PTY 不阻塞）
  - **记录 metric + 断开 session**

### 7.2 Channel adapter 的发送去抖

```go
type feishuAdapter struct {
    sendQueue chan sendReq  // buffered 100
    workers   int           // 默认 2
}

func (a *feishuAdapter) sendLoop() {
    for req := range a.sendQueue {
        // 飞书 API 限速：单聊 5 QPS
        // 用 token bucket 控制
        if err := a.rateLimit.Wait(ctx); err != nil { continue }
        a.client.SendMessage(req.chatID, req.text)
    }
}
```

### 7.3 不在 Channel adapter 里做 ANSI 解析

ANSI 解析是**沉重**的活（状态机、各种 escape 变体）。在 PTY bridge 直通字节流，ANSI 字符串原样塞飞书消息。

如果用户抱怨"飞书显示乱码"，的方案是：
- **方案 A**：用 `mattn/go-runewidth` + `aymanbagabas/go-pty` 提供的 `render` 函数（如果支持）
- **方案 B**：用 `puppeteer/playwright` 渲染 ANSI 到 PNG，发图片
- **方案 C**：写一个最小 ANSI parser（~500 行 Go），提取颜色 + 文本，转飞书富文本 不做决策，等用户反馈。

---

## 8. 测试场景

### 8.1 单元测试

```go
// internal/bridge/pty/pty_test.go
func TestBridge_BasicEcho(t *testing.T) {
    b, _ := pty.New(t.TempDir(), "/bin/echo", []string{"hello"})
    defer b.Close()

    buf := make([]byte, 1024)
    n, _ := b.Read(buf)
    assert.Equal(t, "hello\n", string(buf[:n]))
}
```

### 8.2 集成测试：PTY → Aggregator → 飞书 mock

```go
func TestAggregator_FlushOnSize(t *testing.T) {
    mock := &mockChannel{}
    agg := NewAggregator(mock, 100, 0) // 100B 上限，0 等待

    agg.Write(bytes.Repeat([]byte("a"), 150))
    assert.Equal(t, 1, mock.callCount)  // 100B 一次 flush
    assert.Equal(t, 100, len(mock.lastMsg))
}
```

### 8.3 E2E（手动）

1. 启动 nightme，加载飞书 app config
2. 飞书 DM 机器人：`workspace: /tmp/test`
3. 验证：
   - 收到 "Session started in /tmp/test"
   - `/tmp/test` 下有 claude 进程在跑（`ps aux | grep claude`）
   - 在 DM 发 "hello"
   - 飞书收到 claude 输出
4. kill nightme，验证 claude 进程**仍在跑**（默认策略）
5. 重启 nightme，验证自动 reattach

---

## 9. 下一步

1. ✅ 本 F-19 完成
2. ⏭ 出 **Implementation Brief**（milestone + 第一个 PR commit 计划）
3. ⏭ 动代码：`go mod init` + 目录骨架 + 第一段能跑的代码（local PTY echo）

---

## A2. F-21: Agent Communication Modes (ACP | SDK | PTY | JSON-IO)

> **Source**: `../bridge/cli-transport.md`


> **Depends on**: F-09 (Agent abstraction), F-19 (CLI Bridge)
> **Related docs**: SPEC.md §1.1 (Agent), [F-09-agent-abstraction.md](./F-09-agent-abstraction.md), [../bridge/cli-transport.md](./../bridge/cli-transport.md), [../bridge/claude.md](./../bridge/claude.md)

## 1. Description

不同 AI Coding CLI 提供不同程度的"可控性"。nightme 的策略是**优先用最标准的协议**，避免 vendor lock-in：

| 优先级 | Mode | 适用 | UX | 实施难度 |
|--------|------|------|-----|----------|
| **1 (baseline)** | **ACP**（Agent Client Protocol）| Codex、OpenCode、未来 ACP agent | 好：结构化事件、权限确认、工具进度 | 中：协议标准、实现一次复用多 CLI |
| **2 (vendor-specific)** | **SDK**（CLI 自定义 SDK）| Claude Code（Anthropic 暂不支持 ACP）| 优：原生体验 | 高：每个 CLI 单独写，vendor lock-in |
| **3 (JSON-IO)** | **JSON-IO**（专用 stream-json 模式）| Claude Code| 优：结构化 event + AskUserQuestion 渲染 | 中：每个 CLI 独立 bridge package |
| **4 (fallback)** | **PTY 透传** | 任何不支持以上三种的 CLI | 差：ANSI / 进度条 / spinner 乱码 | 低：通用 byte pipe |

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
    ModeJSONIO           // 专用 stream-json 模式（如 Claude Code ）
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

// 当前 nightme 实际定义见 internal/agent/agent.go：
//   - AgentSpec interface（Name/Mode/Command/Args/Env/Detect）—— agent 元数据
//   - Starter   interface（Info/Detect/Start/RunOnce）       —— spawn 配方
//   - Agent     struct + 方法集                               —— runtime handle
// 详细见 [F-09-agent-abstraction.md](../feat/F-09-agent-abstraction.md)。
//
// 历史愿景（已废弃）：本文档早期版本把 Agent 写成 interface、Start 返回 AgentSession
// interface——当前实现把 spawn 配方（Starter）和 runtime handle
//（Agent）分开，runtime handle 直接返回 *Agent，session lifecycle 由
// chatsession.AgentSession 独立承担（见 [F-chat-session.md §AgentSession 模型]
// (../feat/F-chat-session.md)）。

type Agent struct {
    name    string  // "claude", "codex", "opencode", "pi", ...
    command string  // CLI binary 路径（exec.LookPath 解析）
    args    []string // 默认 argv
}

type StartConfig struct {
    Workspace string
    Args      []string
    Env       []string
}

// 当前实际：Starter.Start 返回 *Agent（runtime handle）；
// Agent 的公共方法：PID / Events / SessionID / SendBlocks /
// SendPermission / New / Close / Stop / SetModel。
// AgentSession（chatsession 包里的另一层抽象）的接口见 F-chat-session.md。
```

## 6. Agent 注册表

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
|  | **PTY** | 已完成 | 通用 byte pipe 兜底 |
|  | **ACP** | 已完成 | stable ACP v1 JSON-RPC bridge |
|  | **SDK** | 已完成（fallback） | Go Agent SDK 不存在，保留 ModeSDK seam |
| **** | 优化 | - | ACP capability expansion, SDK when available, persistence |

**已完成**：ACP session handshake/event mapping、Claude ModeSDK adapter seam、配置驱动的 Claude/Codex/OpenCode mode selection，以及 PTY fallback。

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
| nightme 重启，session 已 detach（ACP mode）| 评估 ACP session persistence |

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
- ACP v2 is experimental; should pin a schema version before adding v2-only methods.
- Should an ACP failure fall back automatically to PTY, or require an explicit user/config choice?
- How should Feishu render permission options and long-running tool progress? Separate UX work remains.
- When Anthropic publishes an official Go Agent SDK, should nightme switch from the current sentinel to that adapter, or continue using Claude Code CLI PTY as a compatibility fallback?

---

