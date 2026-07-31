# F-09: Agent Abstraction

> **Status**: designed (v0.1)
> **Milestone**: M1 (used by M2)
> **Depends on**: (none — interface)
> **Related docs**: SPEC.md §1.1 (PTY Bridge 组件 spawn agent)

## 1. Description

定义 `Agent` interface 让 nightme 支持多种 AI Coding CLI。MVP 仅实现 Claude Code，通过抽象 interface 保留扩展位（Codex / OpenCode / Goose / Aider 等）。

## 2. Interface

```go
// internal/agent/agent.go
package agent

type Agent interface {
    // Name 唯一标识，用于 config + registry
    Name() string

    // Command 返回要 spawn 的可执行文件路径
    // （可以是相对或绝对路径，会经过 exec.LookPath 解析）
    Command() string

    // Args 返回额外参数
    // v0.1 留空（靠 CLI 自己启动 interactive mode）
    Args() []string

    // Env 返回额外环境变量
    // v0.1 留空（用户在 shell 配置了 ANTHROPIC_API_KEY 等）
    Env() []string

    // Detect 检查 agent 是否可用（PATH 中能找到 + 版本兼容）
    // v0.1 简单实现：exec.LookPath(Command())
    Detect() error
}

// Registry: 通过 init() 注册所有可用 agent
var registry = make(map[string]Agent)

func Register(a Agent) {
    registry[a.Name()] = a
}

func Get(name string) (Agent, error) {
    a, ok := registry[name]
    if !ok { return nil, ErrUnknownAgent }
    return a, nil
}

func List() []Agent {
    var out []Agent
    for _, a := range registry { out = append(out, a) }
    return out
}

// Compile-time check
var _ Agent = (*claude.Agent)(nil)
```

## 3. Implementation

**文件**：
- `internal/agent/agent.go` — interface + registry
- `internal/agent/claude/claude.go` — Claude Code 实现
- `internal/agent/claude/claude_test.go` — 单测

**Claude Code 实现**：
```go
package claude

import "github.com/cnlangzi/nightme/internal/agent"

type Agent struct{}

func init() { agent.Register(&Agent{}) }

func (a *Agent) Name() string            { return "claude" }
func (a *Agent) Command() string         { return "claude" }
func (a *Agent) Args() []string          { return nil }
func (a *Agent) Env() []string           { return nil }

func (a *Agent) Detect() error {
    _, err := exec.LookPath(a.Command())
    return err
}
```

**配置选择默认 agent**：
```yaml
# configs/nightme.example.yaml
agent:
  default: "claude"  # 不在配置里改的话就是 claude
```

## 4. Edge cases

| 场景 | 处理 |
|------|------|
| 用户指定未知 agent | SessionManager.Create 返回 ErrUnknownAgent |
| agent 不在 PATH | Detect 返回 error → 用户提示 "claude not found, please install" |
| agent 二进制存在但版本不兼容 | v0.1 不检查版本（兼容性靠用户自己）|
| 多个 agent binary 同名 | exec.LookPath 用 PATH 第一个，不去重 |
| agent 启动需要 env vars（ANTHROPIC_API_KEY）| 用户在 shell 配置（zshrc / bashrc）；v0.1 不注入 |
| 用户想用绝对路径的 agent | 配置覆盖：`agent.claude.command: /usr/local/bin/claude` |
| agent 启动参数（如 `--model opus`）| v0.1 留 Args() 空；v0.2 配置化 |

## 5. Test plan

**单元测试**：
- claude.Agent.Name() == "claude"
- claude.Agent.Detect() 在有 claude 的环境返回 nil
- registry.Get("claude") 返回非 nil
- registry.Get("nonexistent") 返回 ErrUnknownAgent

**集成测试**：
- 注册多个 mock agent → List() 返回所有
- Spawn session with unknown agent → error

## 6. Open questions

- v0.2 加 Codex / OpenCode 时是否需要自动 detect 用户装了哪些？倾向：可以（`Detect()` 试 spawn 一下）
- 是否需要 agent-specific 预处理（如 Claude Code 需要特定 env）？v0.1 不需要
- 用户是否能 runtime 切换 agent（同 session 内）？v0.1 不支持，session 锁 agent
