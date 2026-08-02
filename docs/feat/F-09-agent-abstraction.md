# F-09: Agent Abstraction

> **Status**: implemented (v1.1 — Agent interface 未变；调用方变为 Gateway 通过 Session Manager factory 调它)
> **Milestone**: M1 (interface), M2 (claude 实现)
> **Depends on**: (none — interface)
> **Related docs**: [`SPEC.md`](../SPEC.md) v1.1 §1.1; [`F-26-gateway-hub.md`](./F-26-gateway-hub.md) §5; [`F-21-agent-modes.md`](./F-21-agent-modes.md) (Mode 决策)

---

## 1. Description

定义 `Agent` interface 让 nightme 支持多种 AI Coding CLI。MVP 注册：
- `codex` → ModeACP（Codex CLI 支持 ACP）
- `opencode` → ModeACP（如支持）
- `claude` → ModeJSONIO（v0.2+ 替代 ModePTY）

每个 agent 实现自己的 Mode（ACP/SDK/PTY/JSON-IO），通过 `Mode()` 暴露。nightme spawn session 时根据 Mode 选择对应的 Bridge backend（见 F-21）。

**v1.1 调用路径变化**：

| 旧（v0.2）| 新（v1.1）|
|-----------|-----------|
| `session.Manager.CreateOrUpdate` / `Run` 间接调 `agent.Get` / `agent.Detect` | `gateway.handler.cwd` / `handler.run` 直接调 `agents.Get` / `agents.Detect`（绕开 session）|
| `agents` 是 session 包依赖 | `agents` 通过 `manager.NewMemoryManager(agents, ...)` 注入 session 包，但 session 不直接调用 agents——manager 内部用 agents 来 spawn agent |

实际上 Agent interface 在 v1.1 **没变**；变的只是 registry 的查找从"session 内部"挪到"gateway 内部"。Session Manager 仍然持有 agents 引用，因为 `manager.Create` 内部需要 `agents.Get(name).Start(...)` 来 spawn agent。

---

## 2. Interface

```go
// internal/agent/agent.go
package agent

type Agent interface {
    // Name 唯一标识，用于 config + registry
    Name() string

    // Mode 告诉 Bridge 用哪种 backend 实现（ACP/SDK/PTY/JSON-IO）
    Mode() Mode

    // Command 返回要 spawn 的可执行文件路径
    // （可以是相对或绝对路径，会经过 exec.LookPath 解析）
    Command() string

    // Args 返回额外参数（spawn recipe 默认）
    Args() []string

    // Env 返回额外环境变量
    Env() []string

    // Detect 检查 agent 是否可用（PATH 中能找到 + 版本兼容）
    // v0.1 简单实现：exec.LookPath(Command())
    Detect() error
}

// Registry: 通过 init() 注册所有可用 agent
type Registry struct{ /* map + mutex */ }

func NewRegistry() *Registry
func (r *Registry) Register(a Agent) (replaced bool)
func (r *Registry) Get(name string) (Agent, error)
func (r *Registry) List() []Agent

var (
    ErrUnknownAgent = errors.New("agent: unknown agent")
)

// Builtins is the package-level registry of agents that ship with
// the nightme binary. Each agent package's init() registers itself.
var Builtins = NewRegistry()
```

---

## 3. Implementation

**文件**：
- `internal/agent/agent.go` — interface + AgentSession + Event / EventKind / ContentBlock
- `internal/agent/registry.go` — Registry 实现
- `internal/agent/registry_test.go`
- `internal/agent/mode_string.go` — Mode ↔ string 转换
- `internal/bridge/claudecode/claudecode.go` — Claude Code 实现（ModeJSONIO）
- `internal/bridge/acp/...` — ACP 实现（v0.4+）

**Claude Code 实现**（v1.1）：
```go
package claudecode

import "github.com/cnlangzi/nightme/internal/agent"

type Agent struct {
    name    string
    command string
    args    []string
}

func init() {
    agent.Builtins.Register(New("claude", "claude", nil))
}

func (a *Agent) Name() string            { return a.name }
func (a *Agent) Mode() agent.Mode        { return agent.ModeJSONIO }  // v0.2+
func (a *Agent) Command() string         { return a.command }
func (a *Agent) Args() []string          { return append([]string(nil), a.args...) }
func (a *Agent) Env() []string           { return nil }
func (a *Agent) Detect() error {
    _, err := exec.LookPath(a.command)
    return err
}
```

**配置选择默认 agent**：
```yaml
# configs/nightme.example.yaml
agent:
  default: "claude"  # 不在配置里改的话就是 claude
```

---

## 4. 调用路径（v1.1）

```
gateway.handler.cwd(ctx, msg, [path])
  ├ workspace.Validate(abs)
  ├ agentName := (existing binding.Agent) || (agents.List()[0].Name()) || "claude"
  ├ manager.Create(ctx, CreateRequest{Workspace: abs, Agent: agentName, ...})
  │     └ MemoryManager 内部:
  │         ├ agents.Get(agentName) → Agent interface
  │         ├ agents.Detect() → 验证二进制
  │         └ Agent.Start(ctx, StartConfig{Workspace: abs, ...}) → AgentSession
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

`session.MemoryManager` 内部**仍然**持有 `*agent.Registry` 引用（用于 `manager.Create` 里 spawn agent）。这是 v1.1 唯一保留的"session → agent"间接访问——但它**没有 leak**：session 只通过 `agent.Agent` interface 调 `Start` 拿 `AgentSession`，不 import `bridge/*`。

---

## 5. Edge cases

| 场景 | 处理 |
|------|------|
| 用户指定未知 agent | `agents.Get(name)` 返回 ErrUnknownAgent → handler Reply "unknown agent: <name>" |
| agent 不在 PATH | Detect 返回 error → handler Reply "claude not found, please install" |
| agent 二进制存在但版本不兼容 | v0.1 不检查版本（兼容性靠用户自己）|
| 多个 agent binary 同名 | exec.LookPath 用 PATH 第一个，不去重 |
| agent 启动需要 env vars（ANTHROPIC_API_KEY）| 用户在 shell 配置（zshrc / bashrc）；v1.1 不注入 |
| 用户想用绝对路径的 agent | 配置覆盖：`agent.claude.command: /usr/local/bin/claude` |
| agent 启动参数（如 `--model opus`）| 配置化：`/run claude --model opus` → cfg.Args |
| 同一个 name 注册两次 | `Registry.Register` 返回 `replaced=true`；latest-wins |

---

## 6. Test plan

**单元测试**：
- `claudecode.Agent.Name() == "claude"`
- `claudecode.Agent.Mode() == agent.ModeJSONIO`
- `claudecode.Agent.Detect()` 在有 claude 的环境返回 nil
- `agent.Builtins.Get("claude")` 返回非 nil
- `agent.Builtins.Get("nonexistent")` 返回 ErrUnknownAgent
- `Registry` 并发 Register/Get 无 race

**集成测试**：
- 注册多个 mock agent → `List()` 返回所有
- Spawn session with unknown agent → handler Reply error
- `gateway.handler.cwd` 用不存在的 agentName → Reply "unknown agent"

**手动 E2E**：
- `nightme agents` → 列出所有注册的 agents（命令在 [`cmd/nightme/agents_cmd.go`](../../cmd/nightme/agents_cmd.go)）
- `nightme run --channel=echo` → `/run claude` → claude 进程 spawn
- PATH 里没有 claude → `/run claude` → Reply "claude not found"

---

## 7. Open questions

- v0.4 加 Codex / OpenCode 时是否需要自动 detect 用户装了哪些？倾向：可以（`Detect()` 试 spawn 一下）
- 是否需要 agent-specific 预处理（如 Claude Code 需要特定 env）？v1.1 不需要
- 用户是否能 runtime 切换 agent（同 session 内）？v1.1 不支持，session 锁 agent
- Bridge 选 Mode 是 Agent.Start 内部决策；用户/配置能否 override？v1.1 不能（每个 Agent 决定自己的 Mode）

---

## 8. Cross-references

- **Mode 决策（ACP/SDK/PTY/JSON-IO）**：见 [`F-21-agent-modes.md`](./F-21-agent-modes.md)
- **Claude Code Bridge 细节**：见 [`F-24-claudecode-bridge.md`](./F-24-claudecode-bridge.md)
- **Agent.Session 事件流 → Gateway**：见 [`F-26-gateway-hub.md`](./F-26-gateway-hub.md) §2.3
- **Manager 调用 Agent.Start 的代码**：见 [`internal/session/manager.go`](../../internal/session/manager.go) `MemoryManager.startAgent`

---

## 9. Change log

- **2026-08-02** — v1.1: 文档更新调用路径（agent registry 查找从 session 内部挪到 gateway 内部）。Agent interface / Registry 实现未变。
- **2026-07-31** — v0.1: 原始 Agent interface 设计。仍然适用；调用方变。