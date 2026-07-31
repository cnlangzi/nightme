# F-01: Session Lifecycle

> **Status**: designed (v0.1)
> **Milestone**: M2 (Feishu integration)
> **Depends on**: F-04 (PTY), F-07 (Workspace binding), F-08 (Channel), F-09 (Agent), F-20 (Gateway)
> **Related docs**: SPEC.md §2.1, §3 (lifecycle), [F-20-gateway.md](./F-20-gateway.md), [F-04-pty-simulation.md](./F-04-pty-simulation.md)

## 1. Description

Session 由 nightme 的 slash command 管理。Session 分**两层状态**：

- **Session 层**（持久）：chat_id ↔ workspace（被 `/cwd` 设置后永久绑定）
- **CLI 层**（瞬时）：当前 CLI 进程是否在跑（被 `/run` 启动，被 `/kill` / 异常退出停止）

**Workspace 是启动 CLI 的硬性前置条件**——没有 workspace 就无法 spawn claude/codex/opencode。完整流程：

```
1. /cwd <path>     → workspace set, session created
2. /run <agent> [args]  → CLI spawned in workspace
3. ... 工作 ...
4. /kill           → CLI stopped (workspace 保留)
5. /run ...        → CLI 重启（workspace + agent + args 持久化）
```

**关键设计**：`/run` 智能处理：
- CLI 没跑 → spawn 新 CLI
- CLI 在跑 → reconnect（不重启，避免丢失 agent 内部状态）

## 2. Session 数据模型

```go
// internal/session/session.go
type Session struct {
    ID         string    // uuid
    ChatID     string    // IM chat_id（自然键）
    Workspace  string    // 被 /cwd 设置；session 级持久
    Agent      string    // 被 /run 设置；session 级持久
    Args       []string  // 被 /run 设置；透传给 agent CLI
    PID        int       // 当前 CLI 进程 pid；0 表示没跑
    CreatedAt  time.Time
    LastRunAt  time.Time

    bridge     pty.Bridge     // PTY 句柄（PID=0 时 nil）
    cancel     context.CancelFunc
}
```

**Session vs CLI 状态**：

| Session 存在？ | CLI 在跑？ | 含义 |
|---------------|-----------|------|
| 否 | 否 | 该 chat 从未 /cwd |
| 是 | 否 | workspace 已设，CLI 死了或被 /kill |
| 是 | 是 | 完整工作状态 |

## 3. Interface

```go
// SessionManager 接口
type SessionManager interface {
    // GetOrCreateByChat 根据 chat_id 查找 session；不存在则报错（要求先 /cwd）
    GetByChat(ctx context.Context, chatID string) (*Session, error)

    // CwdHandler 设置 workspace；session 不存在则创建
    CwdHandler(ctx context.Context, msg *Message, args []string) (*gateway.CommandResult, error)

    // RunHandler 启动或重连 CLI
    RunHandler(ctx context.Context, msg *Message, args []string) (*gateway.CommandResult, error)

    // KillHandler 停止 CLI（保留 session）
    KillHandler(ctx context.Context, msg *Message, args []string) (*gateway.CommandResult, error)
}
```

**Registry 持久化 schema**（`~/.local/share/nightme/registry.json`）：
```json
{
  "version": 2,
  "sessions": {
    "oc_xxxxx": {
      "chat_id": "oc_xxxxx",
      "workspace": "/home/devin/code/bailing",
      "agent": "claude",
      "args": ["--model", "opus"],
      "pid": 12345,
      "created_at": "2026-07-31T10:00:00+08:00",
      "last_run_at": "2026-07-31T11:00:00+08:00"
    }
  }
}
```

注：v0.1 用 `chat_id` 做自然键（一个 chat 一个 session），不需要 session_id。

## 4. Implementation

**文件**：
- `internal/gateway/commands.go` — `/cwd` 和 `/run` handlers
- `internal/session/manager.go` — SessionManager 实现
- `internal/session/session.go` — Session 数据结构
- `internal/bridge/pty/pty.go` — pty.New
- `internal/registry/registry.go` — JSON 持久化

**`/cwd <path>` handler 流程**：
```
CwdHandler(ctx, msg, [path])
  ├─ 验证 args 长度（必须 1）
  ├─ workspace.Resolve(path) — 展开 ~
  ├─ workspace.Validate(path) — 存在性 + 目录 + 权限
  ├─ session = mgr.GetByChat(msg.ChatID)
  │   ├─ session == nil → 创建新 session，workspace = path
  │   └─ session != nil
  │       ├─ session.PID == 0 → 更新 workspace
  │       └─ session.PID > 0 → 报错 "CLI running, /kill first"
  ├─ registry.Upsert(session)
  └─ Reply("Workspace set to {path}. Send /run <agent> to start CLI.")
```

**`/run <agent> [args]` handler 流程**：
```
RunHandler(ctx, msg, args)
  ├─ 验证 args 长度（至少 1）
  ├─ session = mgr.GetByChat(msg.ChatID)
  │   ├─ session == nil → 报错 "no workspace set, /cwd first"
  │   └─ session.Workspace == "" → 报错 "no workspace set, /cwd first"（防御）
  ├─ 解析 args: agentName = args[0], extraArgs = args[1:]
  ├─ agent = agent.Get(agentName)
  │   └─ 找不到 → 报错 "unknown agent: {name}"
  ├─ agent.Detect() 验证二进制
  │   └─ 失败 → 报错 "{name} binary not found"
  ├─ 更新 session.Agent = agentName, session.Args = extraArgs
  ├─ 检查 session.PID 状态
  │   ├─ PID > 0 且 alive → Reply "Already running (pid={pid}). Connected."（不重启）
  │   └─ PID == 0 或 dead → 启动新 CLI
  │       ├─ pty.New(workspace, agent.Command(), append(agent.Args(), extraArgs...))
  │       ├─ session.PID = bridge.PID()
  │       ├─ session.LastRunAt = now
  │       ├─ 启动 readPump / writePump goroutines
  │       ├─ registry.Upsert(session)
  │       └─ Reply "Started: {agent} {args}, cwd={workspace}"
```

**PTY 启动时的 args 合并**：
```go
// internal/bridge/pty/pty.go
finalArgs := append(agent.Args(), userArgs...)  // agent 默认 + 用户透传
cmd := exec.Command(agent.Command(), finalArgs...)
cmd.Dir = workspace
```

## 5. 状态转换图

```
                      /cwd (session 不存在)
[no session] ────────────────────────────────► [session, no CLI]
                                                       │
                                                       │ /run
                                                       ▼
                       /kill ◄─────────────── [session, CLI running]
                         │                          ▲
                         │                          │ /run (CLI 死了 or session 新建)
                         ▼                          │
                  [session, no CLI] ──── /run ──────┘
                         │
                         │ (异常) CLI exit
                         ▼
                  [session, no CLI]
```

**关键不变量**：
- 一旦 session 存在，workspace 永久绑定（不能跨 workspace 复用 session）
- session 可在 CLI 死掉后持续存在（不删除）
- nightme 重启后 session 从 registry 恢复

## 6. Edge cases

| 场景 | 处理 |
|------|------|
| `/cwd` 无参数 | "usage: /cwd <path>" |
| `/cwd /nonexistent` | "workspace does not exist: /nonexistent" |
| `/cwd` 时 CLI 在跑 | "CLI running, /kill first to change workspace" |
| `/run` 前没 /cwd | "no workspace set, send /cwd <path> first" |
| `/run` 无参数 | "usage: /run <agent> [args...]" |
| `/run foo`（未知 agent）| "unknown agent: foo" |
| `/run claude` 但 claude 不在 PATH | "claude binary not found" |
| `/run codex --bad-flag` | 透传，codex 自己报错 |
| `/run` 时 CLI 死了（PID stale）| spawn 新 CLI，覆盖旧 PID |
| `/run` 时 CLI 在跑（PID alive）| reconnect，不重启 |
| `/kill` 但 CLI 没在跑 | "no running CLI to kill" |
| `/kill` 后 `/run` 正常 | spawn 新 CLI（workspace 保留）|
| nightme 重启，session 已 detach，PID 活着 | `/run` 时检测 PID alive → "Already running, reconnected" |
| nightme 重启，session 已 detach，PID 死了 | `/run` 时检测 PID dead → spawn 新 CLI |
| `/cwd /a` 然后 `/run claude` 然后 `/cwd /b` | 第二 /cwd 拒绝 "CLI running, /kill first" |
| 用户狂发 `/run`（CLI 已在跑）| 每次都返回 "Already running"，不重启 |

## 7. Test plan

**单元测试**：
- `CwdHandler` 无 args → usage error
- `CwdHandler` chat 无 session → 创建 session，返回 success
- `CwdHandler` chat 有 session（CLI 没跑）→ 更新 workspace
- `CwdHandler` chat 有 session（CLI 在跑）→ 拒绝
- `RunHandler` chat 无 session → "no workspace set"
- `RunHandler` chat 有 session（无 PID）→ spawn，PID 更新
- `RunHandler` chat 有 session（PID alive）→ "Already running"，PID 不变
- `RunHandler` chat 有 session（PID dead）→ spawn 新 CLI
- `RunHandler` 无 args → usage error
- `RunHandler` 未知 agent → "unknown agent"
- `KillHandler` CLI 在跑 → SIGTERM，PID=0
- `KillHandler` CLI 没跑 → "no running CLI to kill"
- `workspace.Resolve("~/code")` → `/home/devin/code`

**集成测试**：
- mock Channel + mock SessionManager → 触发 /cwd → 验证 session 创建
- 触发 /run → 验证 PTY spawn
- 触发 /run 两次 → 验证第二次不重启
- 触发 /kill → 验证 PID 清零

**E2E（M2）**：
- 飞书 DM `/cwd /tmp/foo` → "Workspace set"
- 飞书 DM `/run claude` → "Started: claude, cwd=/tmp/foo"
- 飞书 DM 发 "hello" → claude 收到
- `ps aux | grep claude` → 看到进程
- 飞书 DM `/run claude` → "Already running"
- 飞书 DM `/kill` → "session killed"
- `ps aux | grep claude` → 没了
- 飞书 DM `/run claude --model opus` → "Started: claude --model opus"
- nightme 重启，session 从 registry 恢复
- 飞书 DM `/run claude` → "Already running (pid=12345). Connected." 或 spawn（取决于 PID 是否还活着）

## 8. Open questions

- `/cwd <path>` 在 CLI 跑着时如何 update workspace？v0.1 拒绝；v0.2 加 `--force` 或先 /kill
- `/run` 是否允许切换 agent？v0.1 不允许（CLI 在跑就不重启；要切 agent 必须 /kill 后 /run）
- 是否需要 `/forget` 命令清空 session？v0.1 不需要
- session 永不过期吗？v0.1 是的；v0.2 可加 session TTL
- nightme 重启时如何判断 session 该 reconnect 还是 spawn？v0.1：检查 PID 是否在系统进程表中
