# F-09: Agent Abstraction

> **Status**: implemented

> **Depends on**: (none — interface)
> **Related docs**: [`SPEC.md`](../SPEC.md); [`F-gateway.md`](./F-gateway.md) §5; [`../bridge/cli-transport.md`](./../bridge/cli-transport.md) (Mode 决策)

---

## 1. Description

定义 `Agent` interface 让 nightme 支持多种 AI Coding CLI。MVP 注册：
- `codex` → ModeACP（Codex CLI 支持 ACP）
- `opencode` → ModeACP（如支持）
- `claude` → ModeJSONIO

每个 Starter 实现自己的 Mode（ACP / SDK / PTY / JSON-IO），通过 `Info().Mode` 暴露，nightme 在 spawn session 时根据 Mode 选择对应的 bridge backend（见 [bridge/cli-transport.md](../bridge/cli-transport.md)）。

**调用路径**：

---

## 2. Interface（实际是三层抽象）

Agent 包内是**三个不同的概念**，分别承担"静态元数据 / spawn recipe / runtime handle"：

```go
// internal/agent/agent.go
package agent

// AgentSpec 是 agent 的静态元数据（name / mode / argv）。
// 每个 bridge 包都提供一个实现了 AgentSpec 的类型作为 spawn 配方。
type AgentSpec interface {
    Name() string       // 唯一标识，用于 config + registry
    Mode() Mode          // 告诉 Bridge 用哪种 backend（ACP/SDK/PTY/JSON-IO）
    Command() string     // 要 spawn 的可执行文件路径（经 exec.LookPath 解析）
    Args() []string      // spawn recipe 默认 argv（在 binary 之后）
    Env() []string       // 额外环境变量（KEY=VALUE）
    Detect() error       // 检查可用性（PATH 找得到 / SDK 可用）
}

// Starter 知道怎么 spawn + 运行一个 agent。
// RunOnce 用于 /gtw commit /pr 这种一次性调用（一轮 turn）；
// Start 返回 runtime handle 用于多轮 chat session。
type Starter interface {
    Info() Info
    Detect() error
    Start(ctx context.Context, cfg StartConfig) (*Agent, error)
    RunOnce(ctx context.Context, cfg StartConfig, blocks []ContentBlock) (string, error)
}

// Agent 是 runtime handle（per-spawn 实例）。
// 不导出 PID/events 之外的字段；通过 Start 返回。
type Agent struct {
    Info   Info      // 不可变
    pid    int
    events chan AgentEvent
    driver driver     // 私有；每种 bridge 各自的 driver 实现
}

// Agent 的公共方法：
func (a *Agent) PID() int
func (a *Agent) Events() <-chan AgentEvent
func (a *Agent) SessionID() string
func (a *Agent) SendBlocks(ctx context.Context, blocks []ContentBlock) error
func (a *Agent) SendPermission(resp string) error
func (a *Agent) New(ctx context.Context) error             // 重置对话上下文（保留进程）
func (a *Agent) Close() error
func (a *Agent) Stop(ctx context.Context) error
func (a *Agent) SetModel(ctx context.Context, providerID, modelID string) error
```

### Registry

```go
// Registry 是 Starter 的注册表（按 name 查找）。
type Registry struct{ /* map + mutex */ }

func NewRegistry() *Registry
func (r *Registry) Register(s Starter) (replaced bool)
func (r *Registry) Get(name string) (Starter, error)
func (r *Registry) List() []Starter

var (
    ErrUnknownAgent = errors.New("agent: unknown agent")
)

// Builtins 是与 nightme 二进制一起发布的 agent registry。
// 每个 bridge 包的 init() 注册自己。
var Builtins = NewRegistry()
```

---

## 3. Implementation

**文件**：
- `internal/agent/agent.go` — AgentSpec / Starter interface + Agent struct + Event / EventKind / ContentBlock
- `internal/agent/registry.go` — Registry 实现
- `internal/agent/registry_test.go`
- `internal/agent/mode_string.go` — Mode ↔ string 转换
- `internal/bridge/claudecode/claudecode.go` — Claude Code Starter 实现（ModeJSONIO）
- `internal/bridge/codex/` — Codex Starter 实现
- `internal/bridge/opencode/` — OpenCode Starter 实现
- `internal/bridge/pi/` — Pi Coding Agent Starter 实现
- `internal/bridge/pty/` — PTY Starter 实现（通用 byte pipe）

**Claude Code 实现示例**：
```go
package claudecode

import "github.com/cnlangzi/nightme/internal/agent"

// Driver 满足 Starter interface（+ 私有 driver 接口约束 Agent 操作）。
type Driver struct {
    name    string
    command string
    args    []string
}

func init() {
    agent.Builtins.Register(New("claude", "claude", nil))
}

func (d *Driver) Info() agent.Info {
    return agent.Info{Name: d.name, Mode: agent.ModeJSONIO, Command: d.command, Args: d.args}
}

func (d *Driver) Detect() error {
    _, err := exec.LookPath(d.command)
    return err
}
```

**配置选择默认 agent**：
```yaml
# ~/.nightme/config.yaml
primary: claude
agents:
  - name: claude
    command: claude
```

---

## 4. 调用路径

```
gateway.handler.cwd(ctx, msg, [path])
  ├ workspace.Validate(abs)
  ├ agentName := (existing binding.Agent) || (agents.List()[0].Name()) || "claude"
  ├ manager.Create(ctx, CreateRequest{Workspace: abs, Agent: agentName, ...})
  │     └ chatsession.Manager 内部:
  │         ├ reg.Get(agentName) → Starter interface
  │         ├ agents.Detect() → 验证二进制
  │         └ Agent.Start(ctx, StartConfig{Workspace: abs, ...}) → *agent.Agent
  │             └ Bridge（PTY/ACP/SDK/JSON-IO）实际 spawn 子进程
  └ gateway.Bind(msg.ChatID, msg.ChatType, sess.ID, abs, agentName)
```

```
gateway.handler.run(ctx, msg, [agentName, args...])
  ├ agents.Get(agentName) → 校验存在
  ├ agents.Detect() → 校验二进制
  ├ binding := gw.LookupByChat(msg.ChatID)
  ├ sess := manager.Get(binding.SessionID)
  ├ sess.Status == Running → Reply "already running"
  └ 否则 manager.Create(...)
```

`chatsession.Manager` 持有 `*agent.Registry` 引用，用于 `LookupSelectedAgentSession` 时通过 `Spawner.Spawn` 取 `Starter`。session 层只通过 `Starter` interface 调 `Start` 拿 `*agent.Agent`，不 import `bridge/*`。

---

## 5. Edge cases

| 场景 | 处理 |
|------|------|
| 用户指定未知 agent | `reg.Get(name)` 返回 ErrUnknownAgent → Reply "unknown agent: <name>" |
| agent 不在 PATH | Detect 返回 error → handler Reply "claude not found, please install" |
| agent 二进制存在但版本不兼容 | 不检查版本（用户自管兼容性）|
| 多个 agent binary 同名 | exec.LookPath 用 PATH 第一个，不去重 |
| agent 启动需要 env vars（ANTHROPIC_API_KEY）| 用户在 shell 配置（zshrc / bashrc）；不注入 |
| 用户想用绝对路径的 agent | 配置覆盖：`agent.claude.command: /usr/local/bin/claude` |
| agent 启动参数（如 `--model opus`）| 配置化：`/run claude --model opus` → cfg.Args |
| 同一个 name 注册两次 | `Registry.Register` 返回 `replaced=true`；latest-wins |

---

## 6. Test plan

**单元测试**：
- `claudecode.Driver.Info().Name == "claude"`
- `claudecode.Driver.Info().Mode == agent.ModeJSONIO`
- `claudecode.Driver.Detect()` 在有 claude 的环境返回 nil
- `agent.Builtins.Get("claude")` 返回非 nil（Starter）
- `agent.Builtins.Get("nonexistent")` 返回 ErrUnknownAgent
- `Registry` 并发 Register/Get 无 race

**集成测试**：
- 注册多个 mock agent → `List()` 返回所有
- Spawn session with unknown agent → Reply "unknown agent"
- `gateway.handler.cwd` 用不存在的 agentName → Reply "unknown agent"

**手动 E2E**：
- `nightme agents` → 列出所有注册的 agents
- `nightme run --channel=echo` → `/run claude -> 启动 claude 子进程
- PATH 里没有 claude -> /run claude -> Reply "claude not found"

---

## 7. Open questions

- 加 Codex / OpenCode 时是否需要自动 detect 用户装了哪些？倾向：可以（`Detect()` 试 spawn 一下）
- 是否需要 agent-specific 预处理（如 Claude Code 需要特定 env）？不需要
- 用户是否能 runtime 切换 agent（同 session 内）？不支持，session 锁 agent
- Bridge 选 Mode 是 Starter 内部决策；用户/配置能否 override？不能（每个 Starter 决定自己的 Mode）

---

## 8. Cross-references

- **Mode 决策（ACP/SDK/PTY/JSON-IO）**：见 [`../bridge/cli-transport.md`](./../bridge/cli-transport.md)
- **Claude Code Bridge 细节**：见 [`F-claude.md -> bridge/claude.md`](./F-claude.md -> bridge/claude.md)
- **Agent.Session 事件流 → Gateway**：见 [`F-gateway.md`](./F-gateway.md) §2.3
- **Manager 调用 Starter 的代码**：见 [`internal/chatsession/manager.go`](../../internal/chatsession/manager.go) `Manager.WithSpawner`

---

## 9. Change log

