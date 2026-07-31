# F-01: Session Creation

> **Status**: designed (v0.1)
> **Milestone**: M2 (Feishu integration)
> **Depends on**: F-04 (PTY), F-07 (Workspace binding), F-08 (Channel), F-09 (Agent)
> **Related docs**: SPEC.md §4.2 (数据流), §4.4 (lifecycle)

## 1. Description

用户在新 Chat（DM/group/thread）的**第一条**消息中以 `workspace: <path>` 触发 session 创建。nightme 验证路径存在 + agent 可执行，spawn PTY 进程，注册到 registry，回复 "Session started"。

## 2. Interface

```go
// Router returns ErrNewChat if chat_id has no session yet
type Router interface {
    Lookup(ctx context.Context, chatID string) (*Session, error)
}

// SessionManager.Create validates workspace and spawns PTY
type SessionManager interface {
    Create(ctx context.Context, req CreateRequest) (*Session, error)
}

type CreateRequest struct {
    ChatID    string  // from Channel.Message
    Workspace string  // parsed from first message
    Agent     string  // optional, defaults to config default_agent
}

// Workspace parser (internal/workspace/parser.go)
func ParseWorkspaceDirective(text string) (workspace string, body string, err error)
```

**消息格式**：
- 首条消息前缀 `workspace: ` 必须存在
- workspace 路径必须为绝对路径
- 后续 body 部分（如 `请帮我修复 login bug`）作为 PTY stdin 第一条输入

## 3. Implementation

**文件**：
- `internal/workspace/parser.go` — 解析 `workspace: <path>` 前缀
- `internal/session/manager.go` — `Create()` 方法
- `internal/pty/bridge.go` — `pty.New()` 实际 spawn

**流程**：
```
Router.Lookup(chatID)
  → ErrNewChat
  ↓
Channel.Message.Text 解析
  → ParseWorkspaceDirective → workspace, body
  ↓
SessionManager.Create(req)
  ├─ 验证 workspace 存在 (os.Stat)
  ├─ 验证 agent 可执行 (exec.LookPath)
  ├─ pty.New(workspace, agent.Command(), agent.Args())
  ├─ registry.Upsert(session)
  ├─ 启动 readPump / writePump goroutines
  └─ 返回 *Session
  ↓
Channel.SendMessage(chatID, "Session started in {workspace}")
Channel.SendMessage(chatID, body)  // 把第一条输入送进 PTY
```

## 4. Edge cases

| 场景 | 处理 |
|------|------|
| workspace 路径不存在 | 返回错误 → Channel 提示 "workspace does not exist: {path}" |
| workspace 是相对路径 | 拒绝，要求绝对路径 |
| agent 不在 PATH | 返回错误 → Channel 提示 "claude not found, please install" |
| PTY 启动后立刻 exit | 注册后立即取消，registry 删除记录，提示用户 |
| 首条消息没有 `workspace:` 前缀 | 提示用户 "please start with 'workspace: <abs path>'" |
| 用户输入的 workspace 包含特殊字符 | 转义处理（`"` `\` 等） |
| 同时多条首条消息（IM 重发） | 利用 chat_id 路由去重，第二次 Lookup 返回已有 session |

## 5. Test plan

**单元测试**：
- `ParseWorkspaceDirective("workspace: /tmp/foo 请帮我修 bug")` → `("/tmp/foo", "请帮我修 bug", nil)`
- `ParseWorkspaceDirective("hello")` → `("", "", ErrNoWorkspace)`
- `os.Stat` 不存在 → 返回 `ErrWorkspaceNotExist`

**集成测试**：
- 启动 mock channel + mock agent → Create → 验证 session 创建 + registry 写入

**E2E（M2）**：
- 飞书 DM 首条消息 `workspace: /tmp/foo` → 收到 "Session started" 消息

## 6. Open questions

- 是否支持 `~` 展开（`workspace: ~/code/bailing`）？倾向：是，但先 expand 再 stat
- 是否允许 workspace = `file://...`？倾向 v0.1 不支持，留给后续
- 首条消息 body 是否作为第一条 stdin 输入？倾向：是（参考 [F-19](./F-19-cli-bridge.md) §2.1）
