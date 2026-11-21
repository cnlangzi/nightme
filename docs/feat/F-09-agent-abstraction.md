# F-09: Agent Abstraction

> **Status**: implemented

> **Depends on**: (none — interface)
> **Related docs**: [`SPEC.md`](../SPEC.md); [`SPEC.md`](../SPEC.md) §1.1; [`../bridge/cli-transport.md`](./../bridge/cli-transport.md) (Mode 决策)

---

## 1. Description

定义 `AgentSpec` / `Starter` 让 nightme 支持多种 AI Coding CLI。内置 agent（8 个，注册表见 `cmd/nightme/agents.go`）：

| agent | Mode | 传输 |
|---|---|---|
| `claude` | ModeJSONIO | Claude Code stream-json |
| `codex` | ModeJSONIO | `codex app-server --listen stdio://` 的 JSON-RPC over stdio |
| `dsh` | ModeJSONIO | 共享的 `dsh --profile web` host |
| `opencode` | ModeACP | `opencode acp` |
| `cursor` | ModeACP | `cursor-agent acp` |
| `pi` | ModeJSONIO | `pi --mode rpc` |
| `copilot` | ModeACP | `copilot --acp --stdio` |
| `omp` | ModeJSONIO | `omp --mode rpc` |

每个 Starter 实现自己的 Mode（ACP / SDK / PTY / JSON-IO），通过 `Info().Mode` 暴露，nightme 在 spawn session 时根据 Mode 选择对应的 bridge backend（见 [bridge/cli-transport.md](../bridge/cli-transport.md)）。注册顺序同时是 primary agent 自动探测的优先级顺序，见 [`../primary-agent-detection.md`](../primary-agent-detection.md)。

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
// RunOnce 用于 /gtw commit /pr 这种一次性调用（一轮 turn）——
// 它走 session transport：Start → SendBlocks → drain → Close，
// prompt 写进 bridge 的协议管道，因此不受平台 argv 上限约束。
// Start 返回 runtime handle 用于多轮 chat session。
type Starter interface {
    Info() Info
    Detect() error
    Init(string)                                   // 覆盖可执行文件路径（cfg.Agents 覆盖走这里）
    Start(ctx context.Context, cfg StartConfig) (*Agent, error)
    RunOnce(ctx context.Context, cfg StartConfig, blocks []ContentBlock, opts ...RunOnceOption) (RunResult, error)
    Review(ctx context.Context, cfg StartConfig, opts ...RunOnceOption) (RunResult, error)
}

// TurnText 是 bridge driver 可以实现的可选能力：报告当前 turn 的
// 原样正文。one-shot drain 优先取它，取不到才回退到从事件流拼装
// 出来的正文（分句流式 + trim 会丢掉段落空行）。
type TurnText interface {
    TurnText() string
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
func (a *Agent) Keepalive(ctx context.Context, onRecover func(context.Context) error) error
func (a *Agent) Events() <-chan AgentEvent
func (a *Agent) SessionID() string
func (a *Agent) SendBlocks(ctx context.Context, blocks []ContentBlock) error
func (a *Agent) SendPermission(resp string) error
func (a *Agent) New(ctx context.Context) error             // 重置对话上下文（保留进程）
func (a *Agent) Close() error
func (a *Agent) Stop(ctx context.Context) error
```

`Agent` 上没有"设置模型"的入口：模型名由 bridge 从 agent 事件里读出来，
runtime 在 `internal/runtime/handler.go` 里调
`AgentSession.SetModel(m string)`（`internal/agentsession/session.go`）
写进 session —— `SetModel` 幂等，空值不覆盖已有模型。

### Registry

```go
// internal/agent/registry.go
package agent

// Registry 是 Starter 的注册表（按 name 查找），带首次插入顺序。
type Registry struct{ /* map + mutex + order slice */ }

func New() *Registry
func (r *Registry) Register(s Starter) (replaced bool)
func (r *Registry) Get(name string) (Starter, error)
func (r *Registry) List() []Starter

// Builtins 是与 nightme 二进制一起发布的 agent registry。
// 注册发生在 cmd/nightme/agents.go 的 init()。
var Builtins = New()
```

`ErrUnknownAgent` 定义在 `internal/agent/agent.go`。

`List()` 按**首次注册顺序**返回。同名重复注册只替换实例、不改变位置 —— 顺序因此稳定，也是 primary agent 自动探测的优先级链（见 [`../primary-agent-detection.md`](../primary-agent-detection.md)）。

---

## 3. Implementation

**文件**：
- `internal/agent/agent.go` — Mode 常量 + AgentSpec / Starter interface + Info + Agent struct + Event / EventKind / ContentBlock
- `internal/agent/run_session.go` — 共享的 one-shot turn 驱动（`RunSession` / `ResumeSession` / `DrainSession`）
- `internal/agent/registry.go` — Registry 实现
- `internal/agent/registry_test.go`
- `cmd/nightme/agents.go` — 内置 agent 的注册表（`agent.Builtins.Register`）
- `internal/bridge/claudecode/starter.go` — Claude Code Starter 实现（ModeJSONIO）
- `internal/bridge/codex/starter.go` — Codex Starter 实现
- `internal/bridge/dsh/starter.go` — DSH Starter 实现
- `internal/bridge/opencode/starter.go` — OpenCode Starter 实现
- `internal/bridge/cursor/starter.go` — Cursor Starter 实现
- `internal/bridge/pi/starter.go` — Pi Coding Agent Starter 实现
- `internal/bridge/copilot/starter.go` — Copilot Starter 实现
- `internal/bridge/omp/starter.go` — OMP Starter 实现
- `internal/bridge/pty/starter.go` — PTY Starter 实现（通用 byte pipe；共享基础设施，不注册进 `Builtins`）

**Claude Code 实现示例**：
```go
package claudecode

import "github.com/cnlangzi/nightme/internal/agent"

// Starter 满足 Starter interface（+ 私有 driver 接口约束 Agent 操作）。
type Starter struct {
    name           string
    command        string
    defaultCommand string
    args           []string
}

func NewStarter(name, command string, args []string) *Starter {
    return &Starter{name: name, command: command, defaultCommand: command, args: args}
}

func (s *Starter) Info() agent.Info {
    return agent.NewInfo(s.name, agent.ModeJSONIO, s.command, s.args, telemetry.Env(telemetryEnv...))
}

func (s *Starter) Detect() error {
    return agent.ResolveCommand(s.command)
}
```

注册发生在 `cmd/nightme/agents.go`：

```go
agent.Builtins.Register(claudecode.NewStarter("claude", "claude", nil))
```

**配置选择默认 agent**：
```yaml
# ~/.nightme/config.yaml
primary: claude
agents:
  - name: claude
    bridge: claude
    command: /usr/local/bin/claude
```

`primary` 是顶层标量；`agents:` 是顶层列表，每项是一个内置 agent 的路径覆盖（`name` 必须是内置 agent 名，`command` 是可执行文件的绝对路径，整串取用不做分词）。见 [`../primary-agent-detection.md`](../primary-agent-detection.md) §2。

---

## 4. 调用路径

入站消息先经 `gateway.Router` 的 pump 交给 `internal/gateway/inbound` 的 dispatch chain，slash command 由 `internal/command/commander.go` 分派到对应 factory。

```
/use claude
  ├ command.ParseCmdArgs → 恰好 1 个位置参数（多一个就报错）
  ├ command.RequireActiveCwd(cs) → 没有 selectedCwd 就回复 "No active workspace. Send /cwd <path> first."
  ├ cs.SetSelectedAgent("claude")
  ├ cs.LookupSelectedAgentSession()          ← 懒 spawn（已存在的 session 复用）
  │   └ Spawner.Spawn(ctx, "claude", cwd, args, sessionID)   (internal/agentsession/spawner.go)
  │       ├ reg.Get("claude")     → Starter（ErrUnknownAgent → "agent: unknown agent"）
  │       ├ starter.Detect()      → 验证二进制
  │       └ starter.Start(ctx, agent.StartConfig{Workspace: cwd, ...}) → *agent.Agent
  │           └ Bridge（PTY/ACP/SDK/JSON-IO）实际 spawn 子进程
  └ 回复 "now using claude"
```

```
/cwd /home/user/proj
  ├ ~ / $HOME 相对路径展开 + 目录存在性检查
  ├ cs.SetSelectedCwd(abs)
  └ cs 侧清空 cwd 边界上的待发队列（clearQueueOnCwdBoundary）
```

`chatsession.Manager` 持有 `Spawner` 接口（`internal/chatsession/manager.go` 的 `Manager.WithSpawner` 注入），生产实现是 `agentsession.NewRegistrySpawner(reg, scope)`（`internal/agentsession/spawner.go`），由它把 `agentName` 解析成 `Starter`。session 层只通过 `Starter` interface 调 `Start` 拿 `*agent.Agent`，不 import `bridge/*`。

---

## 5. Edge cases

| 场景 | 处理 |
|------|------|
| 用户指定未知 agent | `reg.Get(name)` 返回 `ErrUnknownAgent` → `/use` 回复 "Failed to activate agent: agent: unknown agent" |
| agent 不在 PATH | `Starter.Detect()` 返回 error（`exec.LookPath` 失败 / 绝对路径 stat 失败）→ `/use` 回复 "Failed to activate agent: ..." |
| agent 二进制存在但版本不兼容 | 不检查版本（用户自管兼容性）|
| 多个 agent binary 同名 | exec.LookPath 用 PATH 第一个，不去重 |
| agent 启动需要 env vars（ANTHROPIC_API_KEY）| 用户在 shell 配置（zshrc / bashrc）；bridge 不注入凭据 |
| 用户想用绝对路径的 agent | 配置覆盖：`agents: [{name: claude, command: /usr/local/bin/claude}]`（`agentregistry.Build` → `Starter.Init(path)`）|
| agent 启动参数（如 `--model opus`）| 各 bridge 的 argv 固定；`/use` 只接受一个 agent 名，多余参数直接报错 |
| 同一个 name 注册两次 | `Registry.Register` 返回 `replaced=true`；latest-wins，位置不变 |

---

## 6. Test plan

**单元测试**：
- `claudecode.Starter.Info().Name == "claude"`
- `claudecode.Starter.Info().Mode == agent.ModeJSONIO`
- `claudecode.Starter.Detect()` 在有 claude 的环境返回 nil
- `agent.Builtins.Get("claude")` 返回非 nil（Starter）
- `agent.Builtins.Get("nonexistent")` 返回 ErrUnknownAgent
- `Registry` 并发 Register/Get 无 race
- `Registry.List()` 保留首次注册顺序

**集成测试**：
- 注册多个 mock agent → `List()` 返回所有
- Spawn session with unknown agent → Reply "Failed to activate agent: agent: unknown agent"
- `/use` 用不存在的 agentName → Reply "Failed to activate agent: agent: unknown agent"

**手动 E2E**：
- `nightme config` → Agents 子菜单列出所有注册的 agent
- `/use claude` → 启动 claude 子进程
- PATH 里没有 claude → `/use claude` → Reply "Failed to activate agent: ..."

---

## 7. Design decisions

- **自动探测已安装的 agent**：primary agent 未配置时按 `agent.Builtins` 的注册顺序逐个 `Detect()`，取第一个成功的；见 [`../primary-agent-detection.md`](../primary-agent-detection.md)。
- **agent-specific 预处理**：bridge 只注入传输与权限相关的参数（permission bypass、telemetry 开关）；模型 / provider / 凭据由用户自己的 CLI 配置决定，nightme 不写用户的 agent 配置。
- **运行时切换 agent**：支持 —— `/use <agent>` 在同一个 chat 内切换当前 agent（见 §4）。
- **Mode 由 Starter 决定**：用户与配置都不能 override `Info().Mode`。

---

## 8. Cross-references

- **Mode 决策（ACP/SDK/PTY/JSON-IO）**：见 [`../bridge/cli-transport.md`](./../bridge/cli-transport.md)
- **Claude Code Bridge 细节**：见 [`bridge/claude.md`](../bridge/claude.md)
- **Agent.Session 事件流 → Gateway**：见 [`SPEC.md`](../SPEC.md) §1.2
- **Manager 注入 Spawner**：见 [`internal/chatsession/manager.go`](../../internal/chatsession/manager.go) `Manager.WithSpawner`
- **Spawner 把 agent 名解析成 Starter**：见 [`internal/agentsession/spawner.go`](../../internal/agentsession/spawner.go)
- **内置 agent 注册表与 primary 探测**：见 [`../primary-agent-detection.md`](../primary-agent-detection.md)
