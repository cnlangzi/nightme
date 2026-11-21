# CLI Transport (PTY Byte Pipe + Agent Communication Modes)

## A1. F-19: PTY Mode Byte Pipe (Bridge PTY Implementation)

> **Source**: `../bridge/cli-transport.md`


> **Depends on**: F-04 (PTY), F-08 (Channel)

> **Related docs**: SPEC.md(Bridge 组件), [../bridge/cli-transport.md](./../bridge/cli-transport.md) (Bridge 四层模式), [SPEC.md §1.2](../SPEC.md) (single-consumer), FEATURES.md

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
│     │ 每次 Read 一个 chunk → 一条 EventAgentText│
│     ▼                                       │
│ chunk = "\x1b[32m✓\x1b[0m Updated file..." │
│     │                                       │
│     │ Channel adapter：追加为 receipt 卡片上的│
│     │ 一条 rolling-log entry；超出单条上限时   │
│     │ 按 SendLongMessage 切分                │
│     ▼                                       │
│ 飞书 receipt 卡片（滚动日志）                 │
└─────────────────────────────────────────────┘
```

**关键点**：
- 飞书单条消息上限 ~4KB（`maxMessageBytes = 3800`，给协议开销留余量）
- `SendLongMessage` 在换行边界切分，保留 ANSI 码完整性（不在 escape sequence 中间断开）
- bridge 侧不做字节级聚合窗口；一次 `Read` 就是一条事件，聚合发生在 Channel 层

---

## 3. 缓冲与聚合

### 3.1 为什么需要聚合？

Claude Code 一次操作可能产生 50+ 个 stdout write（如进度更新），如果不聚合：
- 飞书群发：~50 条消息
- 飞书 QPS：单聊 5 QPS，超限后消息会被吞
- 用户体验：手机通知刷屏

### 3.2 聚合策略

聚合**不在 bridge 侧**。PTY bridge 的 read loop 每次 `Read` 拿到多少字节，就原样包成
一条 `EventAgentText` 推出去，不缓冲、不合并、不改写：

```go
// internal/bridge/pty/agent.go::readLoop
buf := make([]byte, 4096)
for {
    n, err := d.transport.Read(buf)
    if n > 0 {
        d.events <- agent.AgentEvent{
            Kind: agent.EventAgentText,
            Text: string(buf[:n]),
        }
    }
    if err != nil { /* EventAgentDone + close(events) */ }
}
```

Channel 层负责把这一串文本块收成用户看得到的形态：

| 层 | 行为 |
|---|---|
| `internal/gateway/outbound/translate.go` | `EventAgentText` → `OutReply`（一条事件一块文本）|
| `internal/channel/feishu/receipt.go` | `OutReply` 追加为 receipt 卡片的 rolling-log entry，追加后整卡重渲染 |
| `internal/channel/feishu/adapter.go` | 单条超过 `maxMessageBytes`(3800) 时走 `SendLongMessage` 切分 |

receipt 卡片的容量有硬上限：50 个元素 / 28 KB（`receiptMaxElements` /
`resultCardEnvelopeBudget`）。超限时 `AppendEntry` 返回 `ErrReceiptOverflow`，调用方
改成发一条新的顶层消息——不截断，也不丢用户已经看到的旧条目。

**不做**：
- 不设字节级 flush 窗口（PTY 侧没有 `Aggregator`）
- 不识别 prompt / 等待输入模式（nightme 不解析 CLI 输出）

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
pty driver 的 readLoop 发 EventAgentDone{ExitCode:-1}，然后 close(events)
  ↓
AgentSession 的 read pump 看到 events channel 关闭
  ↓
endPrompt(agent.PromptEndProcessDied) + emitLifecycleLocked(StatusExited)
  ↓
chatsession 的 PumpEvents 收到 KindLifecycle{Status: StatusExited} → as.SetExited(exitCode)
  ↓
退出码写进 agent_sessions.json（registry.AgentSessionFile）
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
2. 取消 daemon 级进程 scope → 所有 agent 子进程随各自的 ctx 结束
3. waitForAgentExit 等子进程上报退出事件（退出码由此落库）
4. persistAgentSessions / persistChatStates 落盘
5. 主进程退出

下次 nightme 启动:
  ↓
1. ReconcileAgentSessions：PID 已经死的条目翻成 exited / ExitCodeReconciledDead(-3)
2. SweepOrphanAgents：还活着的孤儿进程按 SIGTERM → grace → SIGKILL 收掉，
   条目改写成 exited / ExitCodeTerminatedByNightme(-4)，session id 保留
   （下一条用户消息用 --resume 接着聊）
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
func TestSpawnEcho(t *testing.T) {
    b, err := NewTransport(context.Background(), t.TempDir(),
        "/bin/echo", []string{"hello"}, nil, 80, 24)
    ...
    got := readWithTimeout(t, b, 2*time.Second)
    // 断言输出里有 "hello"
}
```

`internal/bridge/pty/pty_test.go` 另外覆盖 `TestSetsize`（窗口尺寸）、
`TestLongLineNotTruncated`（长行不被截断）、`TestCloseKillsChild`；driver 半边在
`internal/bridge/pty/session_test.go`（readLoop / SendBlocks / SendPermission / Close），
Starter 半边在 `internal/bridge/pty/agent_test.go`（Detect / Start 端到端）。

### 8.2 集成测试：PTY → Channel

bridge 侧没有 Aggregator 可测——聚合发生在 Channel 层。PTY → Channel 的链路由
`internal/bridge/pty/agent_test.go`（driver 端到端）与各 channel 包自己的测试覆盖。

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
> **Related docs**: SPEC.md §1.1 (Agent), [F-09-agent-abstraction.md](../feat/F-09-agent-abstraction.md), [../bridge/cli-transport.md](./../bridge/cli-transport.md), [../bridge/claude.md](./../bridge/claude.md)

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
  Starter.Info().Mode 是什么？
  ├─ ModeACP  → 用 ACP client（首选）
  │              → spawn ACP server 二进制
  │              → JSON-RPC 握手
  │              → 消费 ACP events → 转换 AgentEvent
  │              → AgentSession 包装 ACP 连接
  ├─ ModeSDK  → 用 vendor-specific SDK（当前没有实现该模式的 bridge）
  │              → 调 SDK client（如 Claude Code Agent SDK）
  │              → SDK 原生返回结构化 events
  │              → AgentSession 包装 SDK connection
  ├─ ModeJSONIO → 用 CLI 自家的 stream-json 模式
  │              → spawn `claude --input-format stream-json --output-format stream-json ...`
  │              → parse JSON events（system / assistant / tool_use / tool_result / result）
  │              → AskUserQuestion 双路兼容（tool_use 拦截 + text fallback）
  │              → AgentSession 包装 agent.Agent（内部是 claudecode driver）
  └─ ModePTY  → 兜底（任何 CLI 都能用）
                 → spawn CLI 在 PTY 中
                 → read goroutine 把 bytes 转 EventAgentText
                 → AgentSession 包装 agent.Agent（内部是 pty.Transport）
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
    EventAgentText EventKind = iota
    EventAgentThinking
    EventAgentPermission
    EventAgentToolStart
    EventAgentToolEnd
    EventAgentDone
    EventAgentError
    EventAgentResult
    EventAgentReady
    EventAgentTaskCreate
    EventAgentTaskUpdate
    EventAgentPermissionSettled
)

type AgentEvent struct {
    Kind       EventKind
    // bridge 侧上下文，每个事件都盖上
    SessionID  string
    Model      string
    AgentName  string
    Workspace  string
    Branch     string
    // 动作载荷（按 Kind 只有一个有意义）
    Text       string
    Permission *AgentPermissionRequest
    ToolStart  *AgentToolStartEvent
    ToolEnd    *AgentToolEndEvent
    Done       *AgentDoneEvent
    Err        error
    Result     *AgentResultEvent
}

type AgentPermissionRequest struct {
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
    Info      Info               // 固定元数据：Name / Mode / Command / Args / Env
    pid       int
    events    chan AgentEvent
    sessionID atomic.Value       // string，EventAgentReady 时写入
    driver    driver
}

type StartConfig struct {
    Workspace      string
    Args           []string
    Env            []string
    PermissionMode string
    SessionID      string
}

// 当前实际：Starter.Start 返回 *Agent（runtime handle）；
// Agent 的公共方法：PID / Keepalive / Events / SessionID / SendBlocks /
// SendPermission / New / Close / Driver / Stop。
// SetModel 不在 agent.Agent 上——它挂在 AgentSession 上
// （internal/agentsession/session.go::AgentSession.SetModel）。
// AgentSession（chatsession 包里的另一层抽象）的接口见 F-chat-session.md。
```

## 6. Agent 注册表

内置 agent 的注册表在 `cmd/nightme/agents.go`：`init()` 里逐条
`agent.Builtins.Register(<bridge>.NewStarter(...))`，注册顺序即 primary-agent 自动
探测顺序（见 `docs/primary-agent-detection.md`）。用户在 `cfg.Agents` 里写的二进制
路径由 `internal/agentregistry/agentregistry.go::Build` 落成 `ModePTY` 兜底。

```go
// claude -> ModeJSONIO (claudecode bridge)
// codex -> ModeJSONIO (codex app-server; see docs/bridge/codex.md)
// opencode -> ModeACP (opencode acp)
// unknown configured names and direct binary paths -> ModePTY
```

## 7. Channel 渲染（事件 → IM）

Channel Adapter 接收 `AgentSession.Events()` 的统一事件流：

- PTY mode sends raw `EventAgentText` bytes, including any ANSI sequences.
- ACP mode can render `EventAgentPermission`, `EventAgentToolStart`, and `EventAgentToolEnd` as structured UI elements.
- 没有 SDK 模式的 bridge——`agent.ModeSDK` 只是枚举里的占位值，没有实现它的 adapter。

## 8. SessionManager 变化

chat session 不直接持有 pty.Transport，也不直接调 bridge。它通过 `Spawner` 这个
seam 拿到 bridge 层的 `*agent.Agent`，再包成 `AgentSession`：

```go
// internal/agentsession/spawner.go
type Spawner interface {
    Spawn(ctx context.Context, agentName, cwd string, args []string, sessionID string) (*agent.Agent, error)
}
```

生产实现 `registrySpawner` 查 `agent.Registry`：`Get(agentName)` → `Detect()` →
`Start(ctx, agent.StartConfig{Workspace: cwd, Args: args, SessionID: sessionID})`。
返回的 `*agent.Agent` 上挂着 events channel、pid 和 driver；`AgentSession` 的 read
pump 是 events channel 的唯一读者。

## 9. 实施顺序（roadmap）

| 阶段 | Mode | 工作量 | 状态 |
|------|------|--------|------|
|  | **PTY** | 已完成 | 通用 byte pipe 兜底 |
|  | **ACP** | 已完成 | stable ACP v1 JSON-RPC bridge |
|  | **SDK** | 未实现 | 没有 Go Agent SDK；`agent.ModeSDK` 只是枚举里的占位值 |
| **** | 优化 | - | ACP capability expansion, SDK when available, persistence |

**已完成**：ACP session handshake/event mapping、Claude 的 JSON-IO bridge、配置驱动的
Claude/Codex/OpenCode mode selection，以及 PTY fallback。

**未来可能的演进**：
- Anthropic 发布官方 Go Claude Agent SDK → 为 ModeSDK 补一个真正的 session adapter
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
| ACP mode 下用户需要权限确认 | JSON-RPC permission request 映射为 `EventAgentPermission`，用户选择后发送 response ID |
| Channel 不支持 PermissionRequest 渲染 | 降级为文本（"Permission: X，respond 'yes' or 'no'"）|
| 配置里的 agent 名未注册 | `agent.Builtins.Get` 返回 `agent: unknown agent` |
| nightme 重启，session 已 detach（ACP mode）| 评估 ACP session persistence |

## 11. Test plan

**单元测试**：
- pty driver 的 readLoop 正确把 bytes 转 `EventAgentText`
- pty driver 的 SendBlocks / SendPermission / Close 正确
- acp driver 把 ACP v1 events 及 legacy aliases 转 AgentEvent
- acp driver 的 SendPermission 正确发给 ACP server

**集成测试**：
- mock ACP server（JSON-RPC）→ initialize + session/new + prompt + event mapping
- mock PTY → 同上

## 12. Open questions

- ACP v1 capability negotiation: when should filesystem, terminal, auth, and MCP methods be enabled?
- ACP v2 is experimental; should pin a schema version before adding v2-only methods.
- Should an ACP failure fall back automatically to PTY, or require an explicit user/config choice?
- How should Feishu render permission options and long-running tool progress? Separate UX work remains.
- When Anthropic publishes an official Go Agent SDK, should nightme add a ModeSDK adapter alongside the JSON-IO bridge, or keep the JSON-IO bridge as the only Claude path?

---

