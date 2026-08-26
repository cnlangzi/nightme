# Cursor CLI Bridge — 集成方案

> **Status**: 设计完成，待实现
> **Scope**: `internal/bridge/cursor/` — nightme 侧的 Cursor CLI 适配器
> **参考实现**: `internal/bridge/opencode/` — 同为 ACP 包装层
> **姊妹文档**:
> - [docs/bridge/claude.md](./claude.md) — JSON-IO bridge 参考
> - [docs/bridge/codex.md](./codex.md) — JSON-RPC bridge 参考
> - [docs/bridge/cli-transport.md](./cli-transport.md) — 通用 CLI 传输层

---

## 1. 设计基线

### 1.1 传输选型

```text
nightme ──PTY JSON-RPC 2.0──> cursor-agent --force --trust --sandbox disabled --approve-mcps acp
```

Cursor CLI 原生支持 **ACP (Agent Client Protocol)**，与 opencode 使用相同的协议。我们可以**完全复用现有的 `internal/bridge/acp` 包**，只需创建一个薄包装层。

### 1.2 与现有 bridge 的关系

```
internal/bridge/
├── acp/              # 通用 ACP 实现（JSON-RPC 2.0 over PTY）
│   └── ...           # 已有完整实现
├── opencode/         # opencode 包装 acp（参考实现）
│   ├── starter.go    # 包装 acp.NewStarter
│   ├── update.go     # opencode 特定的 sessionUpdate 翻译器
│   ├── print.go      # RunOnce print-mode
│   └── opencode.go   # 包级常量 + debug 日志
└── cursor/           # 新增：cursor 包装 acp
    ├── starter.go    # 包装 acp.NewStarter
    ├── cursor.go     # 包级常量 + debug 日志
    └── print.go      # RunOnce print-mode (cursor-agent -p)
```

---

## 2. Cursor CLI 实测验证

### 2.1 安装与版本

```bash
# 安装
curl https://cursor.com/install -fsS | bash

# 版本（2026-08-17 实测）
agent --version
# 2026.08.11-e8db854
```

### 2.2 ACP 协议验证

```bash
# ACP 子命令存在
agent acp --help
# Usage: agent acp [options]
# Start the Cursor Agent as an ACP (Agent Client Protocol) server

# ACP initialize 握手（protocolVersion=1，数值类型）
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"capabilities":{},"clientInfo":{"name":"nightme","title":"nightme (cursor)","version":"1.0"}}}' | agent acp

# 响应
{
  "jsonrpc":"2.0",
  "id":1,
  "result":{
    "protocolVersion":1,
    "agentCapabilities":{
      "loadSession":true,
      "mcpCapabilities":{"http":true,"sse":true},
      "promptCapabilities":{"audio":false,"embeddedContext":false,"image":true},
      "sessionCapabilities":{"list":{}}
    },
    "authMethods":[
      {
        "id":"cursor_login",
        "name":"Cursor Login",
        "description":"Authenticate using existing Cursor login credentials."
      }
    ]
  }
}
```

### 2.3 关键发现

| 项目 | 值 | 与现有 ACP bridge 兼容性 |
|------|-----|--------------------------|
| protocolVersion | 1 (数值类型) | ✅ 完全兼容 |
| clientInfo.name | 自由指定 | ✅ |
| agentCapabilities | loadSession, mcpCapabilities, promptCapabilities, sessionCapabilities | ✅ |
| authMethods | cursor_login（需先 `cursor-agent login`） | ⚠️ 需要用户登录 |
| 命令名 | `cursor-agent`（不是 `cursor`） | ✅ 可配置 |
| ACP 启动参数 | `cursor-agent --force --trust --sandbox disabled --approve-mcps acp` | ✅ 可配置（parent flags 必须在 `acp` 前） |

---

## 3. 实现方案

### 3.1 文件结构

```
internal/bridge/cursor/
├── starter.go      # Starter 实现（包装 acp.NewStarter）
├── cursor.go       # 包级常量 + debug 日志
└── print.go        # RunOnce print-mode (cursor-agent -p)
```

### 3.2 cursor.go — 包级常量与日志

```go
// Package cursor is the nightme bridge for the Cursor CLI.
//
// Cursor CLI natively supports ACP via `cursor-agent acp` (the
// canonical real binary the official installer drops on PATH;
// `agent` is a primary alias the bash installer creates and
// a courtesy copy the PowerShell installer makes). This package
// wraps the generic acp bridge, similar to opencode.
//
// Two spawn paths:
//
//   - Start (long-lived chat session) → `cursor-agent acp` over
//     PTY. Reuses the generic ACP bridge for protocol handling.
//     No sessionUpdate translator needed (unlike opencode) —
//     Cursor's sessionUpdate events are handled by the generic
//     acp bridge's fallback path.
//
//   - RunOnce (one-shot: /gtw commit, buildAgentPrompt) →
//     `cursor-agent -p "prompt" --output-format text`. The
//     process exits after the turn.
package cursor

import (
	"log/slog"
	"os"
	"strings"
)

const bridgeName = "cursor"

var cursorDebug = cursorDebugEnabled()

func cursorDebugEnabled() bool {
	v := os.Getenv("NIGHTME_CURSOR_DEBUG")
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "1", "true", "yes", "on":
		return true
	}
	return false
}

func cLog(msg string, args ...any) {
	if !cursorDebug {
		return
	}
	all := make([]any, 0, len(args)+2)
	all = append(all, "component", "cursor")
	all = append(all, args...)
	slog.Default().Info("[cursor] "+msg, all...)
}
```

### 3.3 starter.go — 核心实现

```go
// starter.go — the spawn recipe for the cursor ACP bridge.
//
// Cursor CLI natively supports ACP via `cursor-agent acp`
// command. This package wraps the generic acp bridge, similar
// to opencode.
//
// The two paths share the same Starter; only RunOnce and the
// print-mode spawn in print.go differ from Start and the
// ACP-backed driver.
package cursor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/acp"
)

// Starter is the cursor spawn recipe. Held in agent.Builtins as
// a singleton per agent name.
type Starter struct {
	name    string
	command string
	args    []string
}

// NewStarter constructs the cursor spawn recipe. Entry point used
// at registration time (cmd/nightme/agents.go calls it from init()).
//
// args are the protocol flags. The canonical value is
// DefaultACPArgs (parent full-access flags then `acp`).
func NewStarter(name, command string, args []string) *Starter {
	return &Starter{
		name:    name,
		command: command,
		args:    append([]string(nil), args...),
	}
}

// Info returns the fixed metadata for this starter.
func (s *Starter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeACP, s.command, s.args, nil)
}

// Detect verifies the cursor binary resolves on PATH.
func (s *Starter) Detect() error {
	_, err := exec.LookPath(s.command)
	return err
}

// Start spawns `cursor-agent acp` under a PTY (via the generic acp bridge),
// runs the initialize + session/new handshake, and returns a live
// *agent.Agent.
//
// The runtime state lives inside the generic acp bridge — this
// package only contributes:
//   - Per-bridge session context fields (AgentName=cursor, Workspace=cfg.Workspace)
//
// cfg.SessionID, when non-empty, is reserved for future ACP session/load
// wiring. Today the bridge always opens a fresh session.
//
// Unlike opencode, cursor does NOT need a sessionUpdate translator.
// Cursor's ACP server emits standard sessionUpdate events that the
// generic acp bridge handles via its fallback path (text/tool events).
// If future Cursor CLI versions emit custom sessionUpdate variants,
// a translator can be added following the opencode/update.go pattern.
func (s *Starter) Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error) {
	if cfg.Workspace == "" {
		return nil, errors.New("cursor: workspace is required")
	}
	acpStarter := acp.NewStarter(s.name, s.command, s.args, nil, 0, 0)
	a, err := acpStarter.Start(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("agent %s: spawn: %w", s.Info().Name, err)
	}
	return a, nil
}

// RunOnce is the one-shot counterpart to Start. Spawns
// `cursor-agent -p "prompt" --output-format text` directly and returns the
// agent's final text.
//
// Cursor's print-mode uses `cursor-agent -p` (not ACP), which is simpler
// and faster for one-shot invocations (/gtw commit, buildAgentPrompt).
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
	return runPrintMode(ctx, s, cfg, blocks)
}
```

### 3.4 print.go — RunOnce 实现

```go
// print.go — one-shot print mode for cursor using `cursor-agent -p`.
//
// Cursor CLI has a built-in print-mode: `cursor-agent -p "prompt"
// --output-format text`. The process exits after the turn —
// no multi-turn, no events channel. This mirrors the print-mode
// path in codex/claudecode/pi/opencode bridges.
package cursor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// runPrintMode spawns `cursor-agent -p "prompt"` for one-shot invocations.
// The process exits after the turn — no multi-turn, no events channel.
func runPrintMode(ctx context.Context, s *Starter, cfg agent.StartConfig, blocks []agent.ContentBlock) (agent.RunResult, error) {
	if cfg.Workspace == "" {
		return agent.RunResult{}, fmt.Errorf("cursor: workspace is required")
	}

	prompt := extractText(blocks)
	if prompt == "" {
		return agent.RunResult{}, fmt.Errorf("cursor: empty prompt")
	}

	start := time.Now()

	args := []string{"-p", prompt, "--output-format", "text"}
	args = append(args, cfg.Args...)

	cmd := exec.CommandContext(ctx, s.command, args...)
	cmd.Dir = cfg.Workspace
	if len(cfg.Env) > 0 {
		cmd.Env = append(os.Environ(), cfg.Env...)
	}

	cLog("PrintMode Start",
		"command", s.command,
		"workspace", cfg.Workspace,
		"prompt_bytes", len(prompt))

	output, err := cmd.CombinedOutput()

	elapsedMs := time.Since(start).Milliseconds()

	cLog("PrintMode Exit",
		"elapsed_ms", elapsedMs,
		"output_bytes", len(output),
		"err", errStr(err))

	if err != nil {
		return agent.RunResult{}, fmt.Errorf("cursor run: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}

	return agent.RunResult{
		Text:       strings.TrimSpace(string(output)),
		DurationMs: elapsedMs,
		Subtype:    "completed",
	}, nil
}

// extractText concatenates all ContentText blocks into a single prompt.
func extractText(blocks []agent.ContentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == agent.ContentText && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// errStr renders an error's string form, returning "<nil>" for nil.
func errStr(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
```

---

## 4. 注册与配置

### 4.1 注册代码

在 `cmd/nightme/agents.go` 的 `init()` 中添加：

```go
import "github.com/cnlangzi/nightme/internal/bridge/cursor"

func init() {
    // ... existing registrations ...

    // cursor — the `cursor-agent acp` Agent Client Protocol
    // bridge. Cursor CLI natively supports ACP via
    // `cursor-agent acp` command. Reuses the generic ACP
    // bridge for protocol handling.
    //
    // Note: The bridge binary name is `cursor-agent`, the
    // canonical real entry point the official installer
    // drops on PATH on every platform:
    //   unix    → bash installer creates it as a legacy
    //             symlink alongside the primary `agent`
    //             (https://cursor.com/install)
    //   windows → PowerShell installer (https://cursor.com/
    //             install?win32=true) creates cursor-agent.cmd
    //             as the primary and copies it to agent.cmd
    //             as a courtesy alias.
    // Bridge picks the canonical name (not the alias) so
    // detection works on every platform without mirroring
    // the installer's alias logic.
    agent.Builtins.Register(cursor.NewStarter("cursor", "cursor-agent", cursor.DefaultACPArgs))
}
```

### 4.2 配置示例

```yaml
# configs/nightme.example.yaml
agents:
  - name: cursor
    bridge: cursor
    command: cursor-agent  # Cursor CLI binary name
    # Bridge spawns `cursor-agent --force --trust --sandbox disabled --approve-mcps acp` under PTY
    # User's local Cursor CLI configuration and auth state are reused
    # (nightme does not rewrite ~/.cursor/; full access is spawn-time flags only)
```

### 4.3 用户配置覆盖

Builtin 已经带 full-access 参数。用户配置覆盖 `args` 时是**整表替换**（不是追加），所以自定义 argv 必须自己带上权限开关，并且 **parent flags 必须写在 `acp` 前面** — `cursor-agent acp --sandbox disabled` 会被 acp 子命令忽略。

```yaml
agents:
  - name: cursor
    bridge: cursor
    command: /custom/path/to/cursor-agent
    args: ["--force", "--trust", "--sandbox", "disabled", "--approve-mcps", "acp"]
```

---

## 5. 设计原则

### 5.1 本机状态复用

nightme 作为本机 daemon，**直接复用用户已完成的认证和配置状态**。我们不负责处理：

- 认证（login）
- API Key 管理
- 模型配置
- 本地状态存储

这与其他 bridge 的设计哲学一致：

| Bridge | 认证方式 | nightme 职责 |
|--------|----------|--------------|
| claude | `~/.claude/` 配置 | 只调用 `claude` 命令 |
| codex | `~/.codex/` 配置 | 只调用 `codex` 命令 |
| cursor | `~/.cursor/` 配置 | 只调用 `cursor-agent` 命令 |

**原则**：本机能跑，nightme 就能跑。CLI 本身处理所有认证和状态管理。

### 5.2 前提条件

用户在使用 nightme 之前，需要确保本机 Cursor CLI 已就绪：

```bash
# 1. 安装 Cursor CLI
curl https://cursor.com/install -fsS | bash

# 2. 登录 Cursor（只需一次，之后状态持久化在 ~/.cursor/）
cursor-agent login

# 3. 验证本机可以正常运行
cursor-agent -p "hello" --output-format text
```

nightme 启动时只做一件事：调用 `cursor-agent` 命令（官方 installer 在 PATH 上创建的"真名字"binary）。如果本机 CLI 能跑，nightme 就能跑。

### 5.3 权限默认全开（spawn-time flags）

nightme **不改** `~/.cursor/`。和其他 bridge 一样，只在 spawn 时注入权限开关，让 IM 会话能直接执行工具、写文件、跑命令，而不是卡在本机审批弹窗上：

| Bridge | 权限默认 |
|--------|----------|
| claude | `--permission-mode bypassPermissions` |
| codex | `-c approval_policy="never" -c sandbox_mode="danger-full-access"` |
| dsh | `DSH_PERMISSION_MODE=danger-full-access` |
| cursor | `--force --trust --sandbox disabled --approve-mcps`（写在 `acp` / `-p` **前面**） |

Cursor CLI 对应关系（`cursor-agent --help`，2026.08.11-e8db854）：

| Flag | 行为 |
|------|------|
| `--force` (`--yolo` 别名) | Force allow commands unless explicitly denied |
| `--trust` | Trust workspace without prompting |
| `--sandbox disabled` | 关闭 FS / 网络 sandbox（`enabled` 才是限制模式） |
| `--approve-mcps` | Auto-approve MCP servers |

这些是 **parent CLI flags**，必须写在子命令前面：

```text
cursor-agent --force --trust --sandbox disabled --approve-mcps acp
cursor-agent --force --trust --sandbox disabled --approve-mcps -p "hello" --output-format text
```

`cursor-agent acp --force` 会被 acp 子命令当成未知参数丢掉，进程照样起来，权限仍是默认收紧。

---

## 6. 生命周期与事件

### 6.1 进程级状态机

```
newDriver() ──> handshake ──> session/new ──> …turns… ──> Close()
     │              │              │
     │              │              └─ EventAgentReady
     │              └─ initialize + initialized
     └─ spawn PTY
```

### 6.2 事件流（每 turn）

```
session/prompt ──> session/update* ──> session/idle ──> EventAgentResult + EventAgentDone
```

### 6.3 关键不变量

与 acp bridge 一致：

1. **`EventAgentDone` ≠ close events** — 只有进程退出或 `Close()` 才关闭 events channel
2. **单消费者** — `sess.Events()` 只有 AS readpump 一个 consumer
3. **pendingTurnActive** — 由 session/idle 或 error 事件释放

---

## 7. 与 opencode 的对比

| 维度 | opencode | cursor |
|------|----------|--------|
| 二进制名 | `opencode` | `cursor-agent` |
| ACP 启动参数 | `opencode acp` | `cursor-agent --force --trust --sandbox disabled --approve-mcps acp` |
| sessionUpdate 翻译器 | 需要（opencode 特定事件：user_message_chunk, agent_message_chunk, agent_thought_chunk, tool_call, tool_call_update） | **不需要**（通用 ACP fallback 即可） |
| print-mode | `opencode run --format json` | `cursor-agent --force --trust --sandbox disabled --approve-mcps -p` |
| 本地配置 | `~/.opencode/` | `~/.cursor/` |
| 代码量 | ~800 行（starter + update + print + opencode.go） | ~150 行（starter + print + cursor.go） |

**为什么 cursor 不需要 sessionUpdate 翻译器**：

opencode 的翻译器处理 5 种特定 sessionUpdate 变体（agent_message_chunk、agent_thought_chunk、tool_call、tool_call_update、user_message_chunk），这些是 opencode ACP server 的私有扩展。Cursor CLI 的 ACP server 只发射标准 ACP 事件（text/tool），通用 acp bridge 的 fallback 路径已经覆盖。如果未来 Cursor 版本添加了自定义 sessionUpdate，可以参照 `opencode/update.go` 的模式添加。

---

## 8. Edge Cases

| 场景 | 处理 |
|------|------|
| `cursor-agent` 不在 PATH | Detect 返回 error，nightme 提示安装 |
| 未登录 | ACP 握手失败，错误信息会提示用户 |
| Cursor CLI 版本不兼容 | protocolVersion 不匹配时报错 |
| ACP 协议变化 | 通用 acp bridge 已处理，cursor 层无需修改 |
| 空 prompt | RunOnce 返回 error "cursor: empty prompt" |
| workspace 为空 | Start / RunOnce 均返回 error |
| 进程异常退出 | 通用 acp bridge 的 stderr 诊断 + EventAgentDone |

---

## 9. 测试计划

### 9.1 单元测试

```go
// internal/bridge/cursor/starter_test.go

func TestStarter_Info(t *testing.T) {
    s := NewStarter("cursor", "cursor-agent", []string{"acp"})
    info := s.Info()
    if info.Name != "cursor" {
        t.Errorf("expected name cursor, got %s", info.Name)
    }
    if info.Mode != agent.ModeACP {
        t.Errorf("expected ModeACP, got %v", info.Mode)
    }
}

func TestStarter_Detect(t *testing.T) {
    if _, err := exec.LookPath("cursor-agent"); err != nil {
        t.Skip("agent not on PATH")
    }
    s := NewStarter("cursor", "cursor-agent", []string{"acp"})
    if err := s.Detect(); err != nil {
        t.Errorf("Detect failed: %v", err)
    }
}

func TestStarter_Detect_NotFound(t *testing.T) {
    s := NewStarter("cursor", "nonexistent_binary_xyz", []string{"acp"})
    if err := s.Detect(); err == nil {
        t.Error("expected error for nonexistent binary")
    }
}
```

### 9.2 集成测试

```go
// internal/bridge/cursor/cursor_e2e_test.go

func requireRealCursor(t *testing.T) {
    t.Helper()
    if _, err := exec.LookPath("cursor-agent"); err != nil {
        t.Skipf("agent binary not on PATH: %v", err)
    }
}

func TestE2E_FreshSession(t *testing.T) {
    requireRealCursor(t)

    s := NewStarter("cursor", "cursor-agent", []string{"acp"})
    ctx := context.Background()
    cfg := agent.StartConfig{
        Workspace: t.TempDir(),
    }

    a, err := s.Start(ctx, cfg)
    if err != nil {
        t.Fatal(err)
    }
    defer a.Close()

    // Verify session is ready
    select {
    case ev := <-a.Events():
        if ev.Kind != agent.EventAgentReady {
            t.Errorf("expected EventAgentReady, got %v", ev.Kind)
        }
    case <-time.After(10 * time.Second):
        t.Fatal("timeout waiting for EventAgentReady")
    }
}

func TestE2E_PrintMode(t *testing.T) {
    requireRealCursor(t)

    s := NewStarter("cursor", "cursor-agent", []string{"acp"})
    ctx := context.Background()
    cfg := agent.StartConfig{
        Workspace: t.TempDir(),
    }
    blocks := []agent.ContentBlock{
        {Type: agent.ContentText, Text: "Say hello in one word"},
    }

    result, err := s.RunOnce(ctx, cfg, blocks)
    if err != nil {
        t.Fatal(err)
    }
    if result.Text == "" {
        t.Error("expected non-empty result text")
    }
    if result.Subtype != "completed" {
        t.Errorf("expected subtype completed, got %s", result.Subtype)
    }
}
```

### 9.3 测试命令

```bash
# 单元测试（不需要 cursor binary）
go test ./internal/bridge/cursor/ -count=1

# 集成测试（需要 cursor binary 已安装并登录）
go test ./internal/bridge/cursor/ -count=1 -timeout 60s

# 真机 E2E（需要完整 Cursor CLI 环境）
go test ./internal/bridge/cursor/ -count=1 -timeout 120s -run 'TestE2E'
```

---

## 10. 排错速查

| 症状 | 根因 | 修法 |
|------|------|------|
| `cursor: cursor-agent not found` | cursor-agent 不在 PATH | 安装 Cursor CLI |
| `cursor: workspace is required` | cfg.Workspace 为空 | 检查配置 |
| ACP 握手失败 | CLI 未登录、配置问题，或 `initialize` 超时 | 用户在本机验证 `cursor-agent acp` 是否正常；`initialize` 预算 10s（低配冷启动不够再查 CLI 本身） |
| 权限请求卡住 / 工具被拒 | 旧 binary 仍是 `cursor-agent acp`（无 `--force`），或自定义 `args` 把 flags 写在 `acp` 后面 | 确认 `nightme agents` 列出的 argv 以 `--force … acp` 结尾；`make restart` 后重生 |
| session 卡死 | events channel 消费者问题 | 检查 AS readpump |
| print-mode 超时 | cursor-agent -p 执行时间过长 | 检查 Cursor CLI 本地环境（模型、proxy） |
| print-mode 空输出 | prompt 为空或 CLI 异常 | 检查 blocks 内容 + stderr |

---

## 11. 版本与兼容性

- **最低 Cursor CLI 版本**: ≥ 2026.08.11（ACP 支持）
- **已知兼容**: Cursor CLI 2026.08.11+
- **协议**: ACP JSON-RPC 2.0 (protocolVersion=1)

---

## 12. 实现步骤清单

按照 opencode bridge 的实现模式，cursor bridge 的实现分 4 步：

### Step 1: 创建包骨架

创建 `internal/bridge/cursor/` 目录，写入 `cursor.go`（包级常量 + debug 日志）。

### Step 2: 实现 starter.go

包装 `acp.NewStarter`，提供 `Info()` / `Detect()` / `Start()` / `RunOnce()` 四个方法。`Start` 委托给 `acp.NewStarter().Start()`，不需要 sessionUpdate 翻译器。

### Step 3: 实现 print.go

`runPrintMode` 调用 `agent -p "prompt" --output-format text`，捕获 stdout 作为结果文本。

### Step 4: 注册到 agents.go

在 `cmd/nightme/agents.go` 的 `init()` 中添加：
```go
agent.Builtins.Register(cursor.NewStarter("cursor", "cursor-agent", cursor.DefaultACPArgs))
```

### Step 5: 编写测试

- `starter_test.go`: Info / Detect 单元测试
- `cursor_e2e_test.go`: Start / RunOnce 真机测试（requireRealCursor guard）

### Step 6: 更新配置

在 `configs/nightme.example.yaml` 中添加 cursor agent 配置示例。

---

## 13. Change Log

- 2026-08-17: 初始设计文档
- 2026-08-17: 验证 Cursor CLI ACP 协议兼容性
- 2026-08-17: 确认复用 acp bridge 方案，无需 sessionUpdate 翻译器
- 2026-08-17: 完成集成方案文档（基于 opencode bridge 实际代码模式）
