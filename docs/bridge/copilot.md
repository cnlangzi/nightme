# GitHub Copilot CLI Bridge — 集成方案

> **Status**: ✅ Active (实机验证通过 2026-08-29,copilot 1.0.81)
> **Scope**: `internal/bridge/copilot/` — nightme 侧的 GitHub Copilot CLI 适配器
> **参考实现**: `internal/bridge/opencode/` — 同为 ACP 包装层,**无 vendor handler**
> **姊妹文档**:
> - [docs/bridge/claude.md](./claude.md) — JSON-IO bridge 参考
> - [docs/bridge/codex.md](./codex.md) — JSON-RPC bridge 参考
> - [docs/bridge/cursor.md](./cursor.md) — 同款 ACP 包装层(含 vendor MethodHandler)
> - [docs/bridge/cli-transport.md](./cli-transport.md) — 通用 CLI 传输层
> - [docs/bridge/acp.md](./acp.md) — 通用 ACP client 实现 + 反例规则

---

## 1. 设计基线

### 1.1 传输选型

```text
nightme ──PTY JSON-RPC 2.0──> copilot --allow-all-tools --acp --stdio
```

GitHub Copilot CLI 自 v1.0.x 起原生支持 **ACP (Agent Client Protocol)**,与 opencode / cursor 使用相同的协议。我们**完全复用现有的 `internal/bridge/acp` 包**,只需创建一个薄包装层 —— 跟 opencode 风格一致(无 vendor handler,纯包装)。

### 1.2 与现有 bridge 的关系

```text
internal/bridge/
├── acp/              # 通用 ACP 实现(JSON-RPC 2.0 over PTY)
│   └── ...           # 已有完整实现
├── opencode/         # opencode 包装 acp(参考实现,无 vendor handler)
│   └── ...
├── cursor/           # cursor 包装 acp(含 vendor MethodHandler —— 反例)
│   └── ...
└── copilot/          # 新增:copilot 包装 acp(opencode 风格,无 handler)
    ├── starter.go    # 包装 acp.NewStarter
    ├── copilot.go    # 包级常量 + debug 日志
    ├── starter_test.go       # 单元测试
    ├── start_real_unix_test.go    # 真机 Start 链路测试
    └── acp_probe_unix_test.go     # 真机 ACP handshake 探针
```

### 1.3 协议版本约束

| Copilot CLI 版本 | `--acp` flag | 备注 |
|---|---|---|
| `< 1.0.x`(e.g. 0.0.361) | ❌ 报 "unknown option" | preview build,需升级 |
| `>= 1.0.x`(e.g. 1.0.81 GA) | ✅ | 实测 2026-08-29 通过 |

升级方法:

```bash
npm install -g @github/copilot@latest
```

nightme **不**做运行时版本检查;若 Start 失败显示 "unknown option --acp",用户需手动升级 CLI。

---

## 2. GitHub Copilot CLI 实测验证

### 2.1 安装与版本

```bash
# 安装(npm 官方 channel,所有平台)
npm install -g @github/copilot

# 版本(2026-08-29 实测)
copilot --version
# GitHub Copilot CLI 1.0.81.
# Run 'copilot update' to check for updates.

# 关键 flag 列表(简化)
copilot --help 2>&1 | grep -E "allow-all|acp|stdio|prompt|silent"
# --acp                Start as Agent Client Protocol server
# --allow-all-tools    Allow all tools to run automatically without confirmation
# -p, --prompt         Execute a prompt directly without interactive mode
# -s, --silent         Output only the agent response (no stats), useful for scripting with -p
# --stdio              (with --acp) — stdio transport
```

### 2.2 ACP 协议验证

实测用 NDJSON 探针直接驱动 `copilot --acp --stdio`,不走 nightme bridge:

```bash
# 1. initialize 握手
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{},"clientInfo":{"name":"nightme-probe","version":"0.0.1"}}}' \
  | copilot --allow-all-tools --acp --stdio

# 响应(实测 1.0.81):
{
  "jsonrpc":"2.0",
  "id":1,
  "result":{
    "protocolVersion":1,
    "agentCapabilities":{
      "loadSession":true,
      "mcpCapabilities":{"http":true,"sse":true},
      "promptCapabilities":{"image":true,"audio":false,"embeddedContext":true},
      "sessionCapabilities":{"close":{},"list":{}}
    },
    "agentInfo":{"name":"Copilot","title":"Copilot","version":"1.0.81"},
    "authMethods":[
      {"id":"copilot-login","name":"Log in with Copilot CLI",...}
    ]
  }
}

# 2. notifications/initialized(无需响应)
# 3. session/new → 返回 sessionId (UUID)
```

### 2.3 关键发现

| 项目 | 值 | 与现有 ACP bridge 兼容性 |
|---|---|---|
| protocolVersion | `1` (数值类型) | ✅ 完全兼容 |
| agentCapabilities | loadSession, mcp/http+sse, prompt/image+embeddedContext, session/close+list | ✅ |
| 传输 | NDJSON / JSON-RPC 2.0 / stdio | ✅ 与 opencode / cursor 同款 |
| 权限 flag | `--allow-all-tools` (实测 binary 真实存在; `--yolo` 不存在) | ✅ 可配置 |
| 命令名 | `copilot`(npm wrapper shim,Unix 上是 shell script,Windows 上是 `.cmd`) | ✅ exec.LookPath 自动解析 |
| ACP 启动参数 | `copilot --allow-all-tools --acp --stdio` | ✅ flat flags,顺序无关 |
| 认证 | `copilot-login`(需先 `copilot login`) | ⚠️ 用户登录一次,状态持久化在 `~/.copilot/` |
| One-shot | 与 chat 同一份 ACP transport(prompt 走管道) | ✅ |
| vendor 私有扩展 | 无(公开 doc 未暴露 `copilot/*` method) | ✅ 通用 ACP fallback 覆盖 |

---

## 3. 实现方案

### 3.1 文件结构

```text
internal/bridge/copilot/
├── copilot.go                    # 包级常量 + debug 日志
├── starter.go                    # Starter 实现(包装 acp.NewStarter)
├── starter_test.go               # 单元测试
├── start_real_unix_test.go       # 真机 Start 全链路测试
└── acp_probe_unix_test.go        # 真机 ACP handshake 探针
```

### 3.2 copilot.go — 包级常量与日志

```go
// Package copilot is the nightme bridge for the GitHub Copilot CLI.
//
// Copilot CLI natively supports ACP via `copilot --acp --stdio`
// (NDJSON over stdio, same wire surface as opencode / cursor).
// Requires Copilot CLI >= 1.0.x — older preview builds (e.g.
// 0.0.361) reject `--acp` with "unknown option" and must be
// upgraded via `npm install -g @github/copilot@latest`.
//
// Two spawn paths (mirroring cursor / opencode):
//
//   - Start (long-lived chat session) → `copilot --allow-all-tools
//     --acp --stdio` under PTY. Reuses the generic ACP bridge.
//     No sessionUpdate / MethodHandler needed — Copilot's wire
//     surface is fully ACP-spec conformant (per
//     docs.github.com/en/copilot/reference/acp-server); the
//     generic fallback covers all standard sessionUpdate kinds.
//
//   - RunOnce (one-shot: /gtw commit, /gtw pr) → the same
//     ACP transport. The turn runs over the session protocol
//     pipe, so the prompt is not bounded by the platform's
//     argv limit.
//
// Permission defaults match the other ChatOps bridges (Claude
// --permission-mode bypassPermissions, Codex approvalPolicy=
// never + sandboxMode=danger-full-access, cursor --force
// --trust --sandbox disabled, dsh DSH_PERMISSION_MODE=danger-
// full-access): nightme does NOT rewrite ~/.copilot/ — it only
// injects spawn-time `--allow-all-tools` so the IM session can
// act without per-tool approval prompts. `--allow-all-tools`
// is the canonical long form Copilot exposes (verified against
// `copilot --help`; the older `--yolo` alias is NOT a flag in
// the actual binary).
package copilot

import (
    "log/slog"
    "os"
    "strings"
)

// FullAccessArgs is the spawn-time permission flag that turns
// off all per-tool approval prompts. Prepended to the ACP
// spawn args.
var FullAccessArgs = []string{"--allow-all-tools"}

// DefaultACPArgs is the argv nightme registers for the
// copilot builtin: `--allow-all-tools` first (parent-level
// permission flag), then `--acp --stdio` (ACP server transport).
var DefaultACPArgs = append(
    append([]string(nil), FullAccessArgs...),
    "--acp", "--stdio",
)

const bridgeName = "copilot"

// copilotDebug toggles the bridge's detailed debug logging.
var copilotDebug = copilotDebugEnabled()

func copilotDebugEnabled() bool {
    v := os.Getenv("NIGHTME_COPILOT_DEBUG")
    switch strings.ToLower(strings.TrimSpace(v)) {
    case "", "1", "true", "yes", "on":
        return true
    }
    return false
}

// cLog emits an info-level message tagged [copilot].
func cLog(msg string, args ...any) {
    if !copilotDebug { return }
    all := make([]any, 0, len(args)+2)
    all = append(all, "component", "copilot")
    all = append(all, args...)
    slog.Default().Info("[copilot] "+msg, all...)
}
```

### 3.3 starter.go — 核心实现

```go
// starter.go — the spawn recipe for the copilot ACP bridge.
//
// Model choice:
//
//   - Long-lived chat sessions spawn `copilot --allow-all-tools
//     --acp --stdio` under a PTY and drive the standard ACP
//     JSON-RPC 2.0 wire (initialize → session/new →
//     session/prompt → ... → session/cancel). One copilot
//     process per chat session; many turns over its lifetime.
//     Requires Copilot CLI >= 1.0.x (older preview builds
//     reject `--acp`).
//
//   - One-shot invocations (/gtw commit, /gtw pr) run over
//     the same ACP transport. The prompt travels over the
//     protocol pipe, so it is not bounded by the platform's
//     argv limit.
//
// No per-bridge UpdateHandler / MethodHandler is installed:
// Copilot's ACP server emits standard sessionUpdate events
// (handled by the generic acp bridge fallback per
// docs/bridge/acp.md §1.1) and no documented vendor-private
// JSON-RPC methods. If a future Copilot release adds PRIVATE
// protocol extensions (copilot/* methods), a thin per-bridge
// MethodHandler can be installed following the cursor/handler.go
// pattern.
package copilot

import (
    "context"
    "errors"
    "fmt"
    "os/exec"

    "github.com/cnlangzi/nightme/internal/agent"
    "github.com/cnlangzi/nightme/internal/bridge/acp"
)

// Starter is the copilot spawn recipe. Held in agent.Builtins
// as a singleton per agent name.
//
// Mode is ModeACP: the chat-session runtime needs to know the
// bridge speaks Agent Client Protocol so it can apply the
// right per-mode behavior (timeout settings, event queue
// routing, /stop propagation).
type Starter struct {
    name    string
    command string
    args    []string
}

// NewStarter constructs the copilot spawn recipe.
func NewStarter(name, command string, args []string) *Starter {
    return &Starter{
        name:    name,
        command: command,
        args:    append([]string(nil), args...), // defensive copy
    }
}

// Info returns the fixed metadata for this starter.
func (s *Starter) Info() agent.Info {
    return agent.NewInfo(s.name, agent.ModeACP, s.command, s.args, nil)
}

// Detect verifies the copilot binary resolves on PATH.
func (s *Starter) Detect() error {
    _, err := exec.LookPath(s.command)
    return err
}

// Start spawns `copilot` + s.args under a PTY (via the generic
// acp bridge), runs the initialize + session/new handshake,
// and returns a live *agent.Agent.
//
// Copilot emits no vendor-private sessionUpdate kinds or
// JSON-RPC methods that the generic acp fallback does not
// already handle, so no per-bridge translator is installed.
//
// cfg.SessionID, when non-empty, is reserved for future ACP
// session/load wiring. Today the bridge always opens a fresh
// session.
func (s *Starter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
    if cfg.Workspace == "" {
        return nil, errors.New("copilot: workspace is required")
    }
    acpStarter := acp.NewStarter(s.name, s.command, s.args, nil, 0, 0)
    a, err := acpStarter.Start(ctx, cfg)
    if err != nil {
        return nil, fmt.Errorf("agent %s: spawn: %w", s.Info().Name, err)
    }
    return a, nil
}

// RunOnce is the one-shot counterpart to Start. The turn runs over
// the session transport, so the prompt travels over the protocol
// pipe instead of argv.
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock, opts ...agent.RunOnceOption) (agent.RunResult, error) {
    return agent.RunSession(ctx, s, cfg, blocks, opts...)
}

// Review delegates to the shared agent.ReviewDispatch — picks
// ReviewWithOcr when `ocr` is on $PATH, otherwise ReviewWithPrompt.
func (s *Starter) Review(ctx context.Context, cfg agent.StartConfig, opts ...agent.RunOnceOption) (agent.RunResult, error) {
    return agent.ReviewDispatch(ctx, s, cfg, opts...)
}
```

### 3.4 one-shot turn — 走 session transport

copilot 没有独立的 one-shot 路径。`Starter.RunOnce` 与 chat session 共用同一份
ACP transport,由共享的 `agent.RunSession` 驱动:

```text
RunOnce → agent.RunSession → Start(handshake) → SendBlocks(prompt 写进 pipe)
        → drain(事件流) → Close
```

- **prompt 走管道**,不受平台 argv 上限约束(Linux 单个 argv 字符串 131072 字节,
  Windows 整条命令行 32767 字符)。`/gtw pr` 的 prompt 内嵌完整 diff,超限即
  spawn 失败 —— 这正是走管道的原因。
- **正文按 ACP 的原样返回**。ACP bridge 的 `agent_message_chunk` 逐块到达,每个
  flush 的块会被 trim;acp bridge 另外维护一份 per-turn 的原文累加器
  (`agent.TurnText`),one-shot drain 优先取它,所以 PR body 的空行不会丢。
- **one-shot turn 总是开新 session**(`agent.RunSession` 清掉 `cfg.SessionID`),
  不会接进 chat session 的对话里。
- **turn 结束信号**:`EventAgentDone{Reason:"settled"}`。正文为空时 turn 失败
  (`turn ended without a reply`)。

---


---

## 4. 注册与配置

### 4.1 注册代码

在 `cmd/nightme/agents.go` 的 `init()` 末尾追加:

```go
import "github.com/cnlangzi/nightme/internal/bridge/copilot"

func init() {
    // ... existing registrations (claude / codex / dsh / opencode / cursor / pi) ...

    // copilot — the `copilot --acp --stdio` Agent Client
    // Protocol bridge. GitHub Copilot CLI natively supports ACP
    // as of v1.0.x — same wire surface (NDJSON / JSON-RPC 2.0
    // over stdio) as opencode / cursor. Requires Copilot CLI
    // >= 1.0.x; older preview builds (e.g. 0.0.361) ship the
    // `copilot` binary but reject `--acp` with "unknown option".
    //
    // No per-bridge UpdateHandler / MethodHandler is installed:
    // Copilot's wire is ACP-spec conformant and the generic
    // acp bridge's fallback covers all common surface.
    //
    // Permission default: --allow-all-tools (parent-level flag).
    // nightme does NOT rewrite ~/.copilot/ — per the agent-no-
    // config-tampering principle, model / provider / MCP
    // servers flow from the user's own settings.
    //
    // One-shot invocations (/gtw commit, /gtw pr) run over the
    // same ACP transport — the prompt travels over the protocol
    // pipe, so it is not bounded by the platform's argv limit.
    agent.Builtins.Register(
        copilot.NewStarter("copilot", "copilot", copilot.DefaultACPArgs))
}
```

`nightme agents` 输出确认:

```
NAME      COMMAND       ARGS
claude    claude
codex     codex
dsh       dsh
opencode  opencode      acp
cursor    cursor-agent  --force --trust --sandbox disabled --approve-mcps acp
pi        pi
copilot   copilot       --allow-all-tools --acp --stdio
```

### 4.2 配置示例

```yaml
# configs/nightme.example.yaml
agents:
  - name: copilot
    bridge: copilot
    command: copilot       # Bridge 自动注入 `--allow-all-tools --acp --stdio`
    # bridge 默认行为:
    #   长会话 / 一次性: `copilot --allow-all-tools --acp --stdio`
    #   (PTY + JSON-RPC;一次性 turn 的 prompt 走同一条管道)
    # 用户本地的 ~/.copilot/ 配置 + 认证状态由 Copilot CLI 自己处理,
    # nightme 不重写、不注入 model / provider / API key。
```

### 4.3 用户配置覆盖

Builtin 已经带 `--allow-all-tools`(permission)和 `--acp --stdio`(transport)。用户配置覆盖 `args` 时是**整表替换**(不是追加),所以自定义 argv 必须自己带上权限开关和 transport flag:

```yaml
agents:
  - name: copilot
    bridge: copilot
    command: /custom/path/to/copilot
    args: ["--allow-all-tools", "--acp", "--stdio"]
```

**注意**:`--acp` 和 `--stdio` 都是 top-level flags(不是 subcommand),顺序无关,但**两者都必须存在**(没有 `--stdio` 就走 TCP,默认监听端口)。

---

## 5. 设计原则

### 5.1 本机状态复用(agent-no-config-tampering)

nightme 作为本机 daemon,**直接复用用户已完成的认证和配置状态**。我们不负责处理:

- 认证(login / API Key 管理)
- 模型配置
- MCP 服务器配置
- 本地状态存储

这与其他 bridge 的设计哲学一致(per `agent-no-config-tampering` 原则):

| Bridge | 认证方式 | nightme 职责 |
|---|---|---|
| claude | `~/.claude/` 配置 | 只调用 `claude` 命令 |
| codex | `~/.codex/` 配置 | 只调用 `codex` 命令 |
| dsh | `~/.dsh/settings.yaml` + `.credentials.yaml` | 只调 `dsh` 命令 + 注入 `DSH_PERMISSION_MODE` |
| opencode | `~/.opencode/` 配置 | 只调用 `opencode acp` |
| cursor | `~/.cursor/` 配置 | 只调用 `cursor-agent` + spawn-time flags |
| pi | pi-coding-agent 内置 | 只调用 `pi --mode rpc` |
| **copilot** | **`~/.copilot/` 配置** | **只调用 `copilot` + spawn-time `--allow-all-tools`** |

**原则**:本机能跑,nightme 就能跑。CLI 本身处理所有认证和状态管理。

### 5.2 权限默认全开(spawn-time flags)

nightme **不改** `~/.copilot/`。和其他 bridge 一样,只在 spawn 时注入权限开关,让 IM 会话能直接执行工具、写文件、跑命令,而不是卡在本机审批弹窗上:

| Bridge | 权限默认 |
|---|---|
| claude | `--permission-mode bypassPermissions` |
| codex | `-c approval_policy="never" -c sandbox_mode="danger-full-access"` |
| dsh | `DSH_PERMISSION_MODE=danger-full-access` |
| cursor | `--force --trust --sandbox disabled --approve-mcps` |
| pi | (无显式 permission flag;走 RPC 协议的 `--permission-mode`) |
| **copilot** | **`--allow-all-tools`** |

Copilot CLI 对应关系(`copilot --help`,1.0.81 实测):

| Flag | 行为 |
|---|---|
| `--allow-all-tools` | Allow all tools to run automatically without confirmation; required for non-interactive mode |
| `--allow-all-paths` | Disable file path verification and allow access to any path |
| `--allow-tool` | Approve selected tools; Bash commands use `shell(...)` syntax, e.g. `--allow-tool='shell(git status)'` |
| `--deny-tool` | Block specific commands(优先级高于 `--allow-tool` / `--allow-all-tools`) |

**注意**:`--allow-all-tools` **不需要**写在 `--acp` / `--stdio` 前面(flat flags,顺序无关)。但权限生效需要 `--allow-all-tools` + transport flag 同时存在。

### 5.3 vendor 私有扩展 = 0(per `docs/bridge/acp.md §1.1`)

按 `acp.md §1.1` 的反例规则:**ACP 协议与常见 vendor 扩展能覆盖的一切,统一由 generic fallback 接管**。Copilot CLI 公开 doc 未暴露任何 `copilot/*` JSON-RPC method,因此**不安装** UpdateHandler / MethodHandler(对比 cursor 的 5 个 vendor method,opencode 的零 method —— copilot 跟 opencode 同款)。

如果未来 Copilot release 添加 `copilot/diff` / `copilot/plan` 等私有 method,可以参照 `cursor/handler.go` 的 `MethodHandler` 模式添加薄 handler。

---

## 6. 生命周期与事件

### 6.1 进程级状态机

跟 opencode / cursor 一致 —— 全部由通用 `internal/bridge/acp` 包提供:

```text
newDriver() ──> handshake ──> session/new ──> …turns… ──> Close()
     │              │              │
     │              │              └─ EventAgentReady
     │              └─ initialize + notifications/initialized
     └─ spawn PTY + cmd.Start
```

### 6.2 事件流(每 turn)

ACP bridge 的事件路径(`docs/bridge/acp.md §2.1`):

```text
session/prompt ──> session/update (agent_message_chunk / agent_thought_chunk / tool_call / tool_call_update) ──> session.status:idle ──> EventAgentDone{Reason:"settled"}
```

| EventKind | 何时触发 | 备注 |
|---|---|---|
| `EventAgentReady` | handshake 完成 | 携带 SessionID(从 `session/new` 响应) |
| `EventAgentText` | 每次 `agent_message_chunk` / `agent_thought_chunk` flush | 含 `[思考]` 前缀的 thought chunk |
| `EventAgentToolStart` | `tool_call` | toolCallId / title / rawInput |
| `EventAgentToolEnd` | `tool_call_update` | toolCallId / status / rawOutput |
| `EventAgentDone` | `session.status:idle` 或 `session/prompt` 同步响应 | turn-end signal,带 `Reason:"settled"` + Usage |
| `EventAgentError` | 错误路径 | turn 不再发 `EventAgentResult` / `EventAgentDone` |

### 6.3 关键不变量

与 acp bridge / opencode / cursor 一致:

1. **`EventAgentDone` ≠ 关 events channel** — 只有进程退出或 `Close()` 才关
2. **单消费者** — `sess.Events()` 只有 AS readpump 一个 consumer
3. **`turnSettled` 去重** — `session.status:idle` 与 `session/prompt` 同步响应可能双发,只发一次 `EventAgentDone`
4. **Permission request 走通用 `session/request_permission`** — 通用 fallback 已覆盖

---

## 7. 与 opencode / cursor 的对比

| 维度 | opencode | cursor | **copilot**(本实现) |
|---|---|---|---|
| 二进制名 | `opencode` | `cursor-agent` | `copilot` |
| 安装方式 | `npm install -g opencode-ai` 或 curl 脚本 | curl 官方 installer | `npm install -g @github/copilot` |
| ACP 启动参数 | `opencode acp`(subcommand) | `cursor-agent --force … acp`(subcommand) | `copilot --allow-all-tools --acp --stdio`(flat flags) |
| Permission flag | 无(ACP 内部) | `--force --trust --sandbox disabled --approve-mcps`(4 个) | `--allow-all-tools`(1 个) |
| Vendor MethodHandler | 无 | **有**(5 个 `cursor/*` method) | **无**(`copilot/*` 未公开) |
| sessionUpdate 翻译器 | 无(generic 覆盖) | 无 | **无**(opencode 风格) |
| One-shot | 与 chat 同一份 ACP transport | 与 chat 同一份 ACP transport | 与 chat 同一份 ACP transport |
| 本地配置 | `~/.opencode/` | `~/.cursor/` | `~/.copilot/` |
| 代码量 | ~800 行 | ~600 行(含 handler) | **~360 行**(无 handler) |

**为什么 copilot 不需要 vendor handler**:跟 opencode 同款 —— Copilot CLI 公开 doc 未暴露 vendor-private JSON-RPC method,所有 event 都走 ACP spec 或 common vendor extension(usage_update / session.status / session_info_update),通用 fallback 已覆盖。

---

## 8. Edge Cases

| 场景 | 处理 |
|---|---|
| `copilot` 不在 PATH | Detect 返回 error,nightme 提示安装 |
| Copilot CLI < 1.0.x(老 preview) | Start 失败,错误信息 "unknown option --acp",提示升级 |
| 未登录 | ACP 握手阶段 authMethods 报告需 `copilot login`,chat session 失败 |
| 空 prompt | RunOnce 返回 error "copilot: empty prompt" |
| workspace 为空 | Start / RunOnce 均返回 error |
| 进程异常退出 | 通用 acp bridge 的 stderr 诊断 + EventAgentDone{Reason:"settled" 或 error path} |
| `--acp` flag 顺序写错 | Copilot 是 flat flags,无 subcommand,顺序无关,但**必须** `--allow-all-tools` + `--acp` + `--stdio` 三件齐 |
| one-shot 正文丢失空行 | ACP bridge 的 `agent.TurnText` 累加器被绕过 | 确认 acp bridge 仍在维护 per-turn 原文累加器 |
| Windows `.cmd` shim | npm wrapper 在 Windows 装 `copilot.cmd`,exec.LookPath 自动解析 PATHEXT |

---

## 9. 测试计划

### 9.1 单元测试(`internal/bridge/copilot/starter_test.go`)

5 个测试,镜像 cursor/starter_test.go:

| Test | 断言 |
|---|---|
| `TestStarter_Info` | `Info().Name == "copilot"`,`Mode == ModeACP`,`Args == DefaultACPArgs` |
| `TestDefaultACPArgs_AllowAllBeforeACP` | DefaultACPArgs = `[--allow-all-tools, --acp, --stdio]` |
| `TestStarter_Info_DefensiveCopy` | caller 修改 args slice 不影响 Starter |
| `TestStarter_Detect_BinaryNotOnPath` | 不存在的 binary → error |
| `TestStarter_Detect_PassForRealBinary` | 真 `copilot` 在 PATH 时 Detect 通过(skip if not) |

### 9.2 真机测试(2 个,均需 `copilot` on PATH)

| Test | 验证 |
|---|---|
| `TestStart_RealBinary_FullPromptFlow` | `Starter.Start` 全链路:ACP 握手 → `EventAgentReady` → `SendBlocks` → 流式 `[思考]` + 答案 `EventAgentText` → `EventAgentDone` |
| `TestACP_Handshake_RealBinary` | 不走 nightme bridge,直接 `exec.CommandContext("copilot", "--acp", "--stdio")` 跑 NDJSON `initialize` + `session/new`,断言 protocolVersion=1 + 合法 sessionId |

### 9.3 契约测试(`internal/agent/interface_external_unix_test.go`)

`TestBuiltinBridges_SatisfyAgentInterface` 表里加一行:

```go
{"copilot", copilot.NewStarter("copilot", "copilot", copilot.DefaultACPArgs)},
```

保证 Starter 实现 `Info / Detect / Start / RunOnce / Review` 5 个方法 —— 编译期 + 运行期双重验证。

### 9.4 测试命令

```bash
# 单元测试(不需要 copilot binary,CI 安全)
go test ./internal/bridge/copilot/ -count=1

# 真机测试(需要 copilot binary 已安装并登录)
go test ./internal/bridge/copilot/ -count=1 -timeout 180s

# 完整测试(含真机 + 契约)
go test ./internal/bridge/copilot/ ./internal/agent/... -count=1 -timeout 240s
```

---

## 10. 排错速查

| 症状 | 根因 | 修法 |
|---|---|---|
| `copilot: copilot not found` | copilot 不在 PATH | `npm install -g @github/copilot@latest` |
| Start 失败:`unknown option --acp` | Copilot CLI 版本 < 1.0.x | `npm install -g @github/copilot@latest` 升级 |
| ACP 握手失败 / 未登录 | 用户本机未 `copilot login` | 让用户跑 `copilot login`,状态持久化在 `~/.copilot/` |
| 权限请求卡住 / 工具被拒 | 用户自定义 `args` 把 `--allow-all-tools` 删了,或覆盖为 deny-tool | 确认 `nightme agents` 列出的 argv 含 `--allow-all-tools`;`make restart` 后重生 |
| session 卡死 | events channel 消费者问题 | 检查 AS readpump(events 单消费者,见 `claude.md §3`) |
| one-shot 超时 | copilot 的 turn 执行时间过长 | 检查 Copilot CLI 本地环境(模型、proxy) |
| one-shot 空输出 | prompt 为空或 CLI 异常 | 检查 blocks 内容 + stderr |

---

## 11. 版本与兼容性

- **最低 Copilot CLI 版本**: ≥ 1.0.x(1.0.81 GA 实测通过,2026-08-29)
- **已知不兼容**: `< 1.0.x` 的 preview builds(e.g. 0.0.361)
- **协议**: ACP JSON-RPC 2.0(protocolVersion=1)
- **未来兼容性**:
  - 协议变更 → 通用 acp bridge 处理,copilot 层无需改
  - vendor 私有 method 出现 → 加 `SetMethodHandler` 薄 handler(`cursor/handler.go` 模式)
  - Copilot `--acp --stdio` flag 名变更 → 用户需升级 CLI;nightme 报错清晰

---

## 12. 实现步骤清单

实现已落地,以下是回放步骤(便于未来参考):

### Step 1: 创建包骨架 ✅

创建 `internal/bridge/copilot/` 目录,写入 `copilot.go`(包级常量 + debug 日志)。FullAccessArgs / DefaultACPArgs / bridgeName / cLog。

### Step 2: 实现 starter.go ✅

包装 `acp.NewStarter`,提供 `Info()` / `Detect()` / `Start()` / `RunOnce()` / `Review()` 5 个方法。`Start` 委托给 `acp.NewStarter().Start()`。**不安装** vendor handler(Copilot 无 vendor 私有 method)。

### Step 3: one-shot 走 session transport ✅

`Starter.RunOnce` 委托给共享的 `agent.RunSession`(`internal/agent/run_session.go`):`Start → SendBlocks → drain → Close`。prompt 写进 ACP 管道,不受平台 argv 上限约束;正文由 acp bridge 的 per-turn 原文累加器(`agent.TurnText`)按原样返回。

### Step 4: 注册到 agents.go ✅

在 `cmd/nightme/agents.go` 的 `init()` 末尾追加:

```go
agent.Builtins.Register(
    copilot.NewStarter("copilot", "copilot", copilot.DefaultACPArgs))
```

### Step 5: 编写测试 ✅

- `starter_test.go`: 5 个单元测试
- `start_real_unix_test.go`: 1 个真机 Start 全链路测试
- `acp_probe_unix_test.go`: 1 个真机 ACP handshake 探针

### Step 6: 契约测试 + CI 验证 ✅

- `internal/agent/interface_external_unix_test.go` 加 copilot 行
- `.github/workflows/ci.yml` 加 `npm install -g @github/copilot` + 4 个 agent 循环加 copilot

### Step 7: 文档归档 ✅

- README.md + README.zh-CN.md 加 GitHub Copilot CLI 章节
- docs/bridge/copilot.md(本文档)

---

## 13. Change Log

- 2026-08-29: 初始设计文档 + 实现 + 实机验证
  - 设计:opencode 风格薄 wrapper,无 vendor handler(`docs/bridge/acp.md §1.1` 反例规则)
  - 调研:确认 GitHub Copilot CLI ≥ 1.0.x 原生支持 ACP(`copilot --acp --stdio`)
  - 实测 wire:`initialize` / `session/new` / `session/prompt` + text chunk → EventAgentText + `session.status:idle` → EventAgentDone
  - 实现:`internal/bridge/copilot/` 6 个文件 + `cmd/nightme/agents.go` 注册 + 契约测试 + CI 集成
  - 实机验证:10/10 测试 PASS(7 单元 + 3 真机,总 54s)
  - 文档:README / README.zh-CN.md / docs/bridge/copilot.md 同步
  - code-review: 1 真问题(dead code)已修,其余 6 个 candidates 与现有 cursor / opencode / codex / pi 约定一致,本 PR 不动