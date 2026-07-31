# F-01: Session Creation

> **Status**: designed (v0.1)
> **Milestone**: M2 (Feishu integration)
> **Depends on**: F-04 (PTY), F-07 (Workspace binding), F-08 (Channel), F-09 (Agent), F-20 (Gateway)
> **Related docs**: SPEC.md §2.1, §2.3 (数据流), §3 (lifecycle), [F-20-gateway.md](./F-20-gateway.md)

## 1. Description

用户在新 Chat（DM/group/thread）发送 **`/cwd <path>`** slash command 触发 session 创建。

slash command 由 [F-20 Gateway](./F-20-gateway.md) 识别并路由到本 feature 的 handler。handler 验证路径存在 + agent 可执行，spawn PTY 进程，注册到 registry，回复 "Session started"。

**触发协议**：见 [F-20 §4.1](./F-20-gateway.md#41-cwd-path-详细行为)

## 2. Interface

```go
// SessionManager.Create validates workspace and spawns PTY
type SessionManager interface {
    Create(ctx context.Context, req CreateRequest) (*Session, error)
}

type CreateRequest struct {
    ChatID    string  // from Channel.Message
    Workspace string  // parsed from /cwd args[0]
    Agent     string  // optional, defaults to config default_agent
}

// Triggered by Gateway when /cwd <path> is received
func CwdHandler(ctx context.Context, msg *Message, args []string) (*gateway.CommandResult, error) {
    if len(args) != 1 {
        return errorReply("usage: /cwd <path>"), nil
    }
    session, err := sessionManager.Create(ctx, CreateRequest{
        ChatID:    msg.ChatID,
        Workspace: args[0],
    })
    if err != nil {
        return errorReply(err.Error()), nil
    }
    return successReply(fmt.Sprintf("Session started in %s", session.Workspace)), nil
}
```

## 3. Implementation

**文件**：
- `internal/gateway/commands.go` — `/cwd` handler（调 SessionManager）
- `internal/session/manager.go` — `Create()` 方法
- `internal/pty/bridge.go` — `pty.New()` 实际 spawn
- `internal/workspace/workspace.go` — path 验证 + `~` 展开

**流程**：
```
用户 DM 发 "/cwd /tmp/foo"
  ↓
ChannelAdapter.Incoming() → Message{ChatID, Text="/cwd /tmp/foo"}
  ↓
Gateway.Handle(msg)
  ├─ 识别以 / 开头 → 走命令路由
  ├─ ParseCommand("/cwd /tmp/foo") → ("cwd", ["/tmp/foo"])
  ├─ 查 commands["cwd"] → 命中
  └─ CwdHandler(ctx, msg, ["/tmp/foo"])
      ├─ args 长度检查
      ├─ Resolve(path) — 展开 ~
      ├─ Validate(path) — 存在性 + 目录 + 权限
      ├─ sessionManager.Create(req)
      │   ├─ 检查 session 是否已存在（chat_id 已绑定）→ 报错
      │   ├─ pty.New(workspace, agent.Command(), agent.Args())
      │   ├─ registry.Upsert(session)
      │   ├─ 启动 readPump / writePump goroutines
      │   └─ 返回 *Session
      └─ Reply("Session started in /tmp/foo")
```

## 4. Edge cases

| 场景 | 处理 |
|------|------|
| `/cwd` 无参数 | 报错 "usage: /cwd `<path>`" |
| `/cwd /nonexistent` | 报错 "workspace does not exist: /nonexistent" |
| `/cwd /path/to/file`（不是目录）| 报错 "not a directory" |
| `/cwd /path/no/exec` | 报错 "no execute permission" |
| `claude` 不在 PATH | 报错 "claude binary not found" |
| PTY 启动后立刻 exit | registry 立即删除，回复 "agent failed to start" |
| 该 chat_id 已有 active session | 报错 "session already active in {path}, /kill first to switch" |
| `/cwd` 同时收到多次（IM 重发）| Gateway 串行处理，第二个请求被"session already active"挡住 |
| 路径含空格 / 中文 / emoji | 原样保留，PTY 启动不 care |
| `~` 展开 | Resolve 展开为 $HOME 再 Validate |

## 5. Test plan

**单元测试**：
- `CwdHandler` 无 args → 返回 usage error
- `CwdHandler` 有效 path → 调 SessionManager.Create，返回 success
- `CwdHandler` chat 已有 session → 返回 "already active"
- `workspace.Resolve("~/code")` → `/home/devin/code`
- `workspace.Validate("/tmp")` → nil
- `workspace.Validate("/nonexistent")` → ErrNotExist

**集成测试**：
- mock Channel + mock SessionManager → 触发 /cwd → 验证 reply 内容

**E2E（M2）**：
- 飞书 DM 发 `/cwd /tmp/foo` → 收到 "Session started" 消息
- 飞书 DM 后续发 "hello" → claude 收到 stdin
- 飞书 DM 再发 `/cwd /tmp/bar` → 收到 "session already active" 错误

## 6. Open questions

- 是否允许 `/cwd ~/code/bailing`（带 `~`）？倾向：是，先 expand 再 stat
- session 内 `/cwd <new path>` 是否自动 kill 旧 session 并创建新 session？v0.1 拒绝（用 `/kill` 后再 `/cwd`）；v0.2 可能加 `/force` flag
- 是否支持 `/cwd -` 切回上一个 workspace？v0.1 不支持
