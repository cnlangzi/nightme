# F-01: Session Creation

> **Status**: designed (v0.1)
> **Milestone**: M2 (Feishu integration)
> **Depends on**: F-04 (PTY), F-07 (Workspace binding), F-08 (Channel), F-09 (Agent), F-20 (Gateway)
> **Related docs**: SPEC.md §2.1, §2.3 (数据流), §3 (lifecycle), [F-20-gateway.md](./F-20-gateway.md)

## 1. Description

Session 由 nightme 的 slash command 创建。两个独立的触发命令：

- **`/cwd <path>`** — 设置 workspace，使用默认 agent（claude）+ 默认 args
- **`/start <agent> [args...]`** — 设置 agent + args，使用默认 workspace（$HOME）

两个命令任一发到 chat 都会触发 session 创建（缺的字段走默认）。session 已存在时两个命令都拒绝（必须先 `/kill`）。

slash command 由 [F-20 Gateway](./F-20-gateway.md) 识别并路由到本 feature 的 handler。handler 验证路径存在 + agent 可执行，spawn PTY 进程，注册到 registry，回复 "Session started"。

## 2. Interface

```go
// SessionManager.Create validates workspace and spawns PTY
type SessionManager interface {
    Create(ctx context.Context, req CreateRequest) (*Session, error)
}

type CreateRequest struct {
    ChatID    string    // from Channel.Message
    Workspace string    // 已 expand 的绝对路径
    Agent     string    // agent name (must be in agent registry)
    Args      []string  // 额外参数，透传给 agent CLI
}

// 由 Gateway 调用：处理 /cwd <path>
func CwdHandler(ctx context.Context, msg *Message, args []string) (*gateway.CommandResult, error) {
    if len(args) != 1 {
        return errorReply("usage: /cwd <path>"), nil
    }
    workspace, err := workspace.Resolve(args[0])
    if err != nil {
        return errorReply(err.Error()), nil
    }
    session, err := sessionManager.Create(ctx, CreateRequest{
        ChatID:    msg.ChatID,
        Workspace: workspace,
        Agent:     config.DefaultAgent(),  // "claude"
        Args:      nil,
    })
    // ...
}

// 由 Gateway 调用：处理 /start <agent> [args...]
func StartHandler(ctx context.Context, msg *Message, args []string) (*gateway.CommandResult, error) {
    if len(args) < 1 {
        return errorReply("usage: /start <agent> [args...]"), nil
    }
    agentName := args[0]
    extraArgs := args[1:]

    agent, err := agent.Get(agentName)
    if err != nil {
        return errorReply("unknown agent: " + agentName), nil
    }
    if err := agent.Detect(); err != nil {
        return errorReply("agent not found: " + err.Error()), nil
    }

    workspace := chatContext.GetWorkspaceOrDefault(msg.ChatID)  // /cwd 优先；否则 $HOME

    session, err := sessionManager.Create(ctx, CreateRequest{
        ChatID:    msg.ChatID,
        Workspace: workspace,
        Agent:     agentName,
        Args:      extraArgs,  // 透传给 agent CLI
    })
    // ...
}
```

**`chatContext` 的作用**：记录该 chat 之前是否发过 `/cwd`。v0.1 简单实现：每个 chat 记一个 `lastCwd string`，session 销毁后清空。

## 3. Implementation

**文件**：
- `internal/gateway/commands.go` — `/cwd` 和 `/start` handlers
- `internal/session/manager.go` — `Create()` 方法
- `internal/pty/bridge.go` — `pty.New()` 实际 spawn
- `internal/workspace/workspace.go` — path 验证 + `~` 展开
- `internal/chatcontext/` — chat 维度的临时状态（last cwd 等）

**session 创建流程**（统一）：
```
Gateway.Handle("/cwd /tmp/foo") 或 Gateway.Handle("/start codex --flag")
  ↓
对应 handler (CwdHandler 或 StartHandler)
  ├─ 解析 / 验证参数
  ├─ workspace.Resolve(path)（/start 用 chatContext 里的 last cwd 或 $HOME）
  ├─ agent.Get + agent.Detect（/cwd 用默认 claude，无需 Detect）
  ├─ sessionManager.Create(req)
  │   ├─ 检查 session 是否已存在（chat_id 已绑定）→ 报错
  │   ├─ pty.New(workspace, agent.Command(), append(agent.Args(), extraArgs...))
  │   ├─ registry.Upsert(session)
  │   ├─ 启动 readPump / writePump goroutines
  │   └─ 返回 *Session
  └─ Reply("Session started: ...")
```

**PTY 启动时的 args 合并**：
```go
// internal/pty/bridge.go
func New(workspace, command string, args []string, ...) (Bridge, error) {
    cmd := exec.Command(command, args...)  // 透传所有 args
    cmd.Dir = workspace
    // ...
}

// 调用：
agentArgs := append(agent.Args(), req.Args...)  // 合并：agent 默认 args + 透传 args
pty.New(req.Workspace, agent.Command(), agentArgs, ...)
```

- `agent.Args()` 是 agent 的固定 args（如 claude 可能需要 `--quiet`）
- `req.Args` 是用户通过 `/start` 透传的 args
- nightme 把两者合并后给 PTY（agent 的在前，用户的在后；v0.1 简单合并，不去重）

## 4. Edge cases

| 场景 | 处理 |
|------|------|
| `/cwd` 无参数 | 报错 "usage: /cwd <path>" |
| `/cwd /nonexistent` | 报错 "workspace does not exist: /nonexistent" |
| `/cwd` 但 session 已存在 | 报错 "session already active, /kill first" |
| `/start` 无参数 | 报错 "usage: /start <agent> [args...]" |
| `/start foo`（未知 agent）| 报错 "unknown agent: foo" |
| `/start claude` 但 claude 不在 PATH | 报错 "claude binary not found, please install" |
| `/start codex --bad-flag` | 透传，codex 自己报错（nightme 不验证 args 合法性）|
| `/start` 但 session 已存在 | 报错 "session already active, /kill first" |
| `/cwd` 后 `/start claude --flag` | `/start` 用 /cwd 设的 workspace，agent 用 claude，args=["--flag"] |
| `/start` 后 `/cwd /tmp/foo` | `/cwd` 用 /start 设的 agent，workspace 改成 /tmp/foo |
| `/kill` 后 `/start codex` | 创建新 session（agent=codex, workspace=$HOME 默认） |
| PTY 启动后立刻 exit | registry 立即删除，回复 "agent failed to start" |
| 路径含空格 / 中文 / emoji | 原样保留，PTY 启动不 care |
| `~` 展开 | Resolve 展开为 $HOME 再 Validate |
| 用户 args 包含 `--help` | nightme 不识别为 nightme help，直接透传给 agent |

## 5. Test plan

**单元测试**：
- `CwdHandler` 无 args → 返回 usage error
- `CwdHandler` 有效 path → 调 SessionManager.Create，返回 success
- `CwdHandler` chat 已有 session → 返回 "already active"
- `StartHandler` 无 args → 返回 usage error
- `StartHandler("foo")` → "unknown agent"
- `StartHandler("claude", "--model", "opus")` → CreateRequest{Agent:"claude", Args:["--model","opus"]}
- `StartHandler` chat 已有 session → 返回 "already active"
- `StartHandler` 前 chat 发过 `/cwd /tmp/foo` → workspace=/tmp/foo
- `StartHandler` 前 chat 没发过 `/cwd` → workspace=$HOME
- `pty.New` 启动 `claude --model opus` → 验证 cmd.Args 包含 "--model opus"
- `workspace.Resolve("~/code")` → `/home/devin/code`
- `workspace.Validate("/tmp")` → nil

**集成测试**：
- mock Channel + mock SessionManager + mock Agent registry → 触发 /cwd → 验证 CreateRequest
- 触发 /start claude --model opus → 验证 agent 收到 args

**E2E（M2）**：
- 飞书 DM 发 `/cwd /tmp/foo` → "Session started in /tmp/foo"
- 飞书 DM 发 `/start codex --full-auto` → "Session started: codex --full-auto, cwd=$HOME"
- 飞书 DM 发 `/cwd /tmp/foo` → `/start codex` → session 用 workspace=/tmp/foo + agent=codex
- 验证 spawn 的进程：`ps aux | grep codex` 看到 `codex --full-auto`

## 6. Open questions

- `/cwd` 和 `/start` 的顺序是否影响结果？v0.1：后者覆盖前者（但 session 已存在时都拒绝）
- v0.2 是否支持 `/start --workspace <path> claude --flag` 一次性设置？v0.2 评估
- `/start` 的 args 含特殊字符（如空格、`"`、`'`）怎么办？v0.1 按空格切分；v0.2 加引号支持
- agent 默认 args 与用户 args 冲突时如何处理？v0.1 简单拼接（用户在后）；v0.2 可考虑覆盖
