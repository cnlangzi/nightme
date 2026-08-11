# F-01: Session Lifecycle

> **Status**: implemented (v1.1 — Session 是纯域对象，没有 ChatID)
> **Milestone**: M2 (session lifecycle), v0.3 (ChatID 移到 BindingEntry)
> **Depends on**: F-04 (PTY), F-07 (Workspace binding), F-08 (Channel), F-09 (Agent), F-20 (Gateway)
> **Related docs**: [`SPEC.md`](../SPEC.md) v1.1 §3; [`F-20-gateway.md`](./F-20-gateway.md) §4; [`F-26-gateway-hub.md`](./F-26-gateway-hub.md) §5

---

## 1. Description

Session 是 nightme 管理的**单个 CLI 子进程**的进程抽象。它包含：

- **Workspace** —— CLI 进程的 cwd（不可变，创建后不变）
- **Agent** —— CLI 二进制 + 默认 args（不可变）
- **Args** —— `/run` 时透传的额外 args
- **PID** —— 当前 CLI 进程 pid；0 表示没跑
- **Status** —— `Running / Detached / Exited` 生命周期状态

**v1.1 核心变化**：Session **不**持有 `ChatID` / `ChatType` / `OnUserMessage`。**Chat 绑定关系是 Gateway 的事**，由 Gateway 维护的 `BindingEntry` 表达。Session 是纯进程域对象，不知道 channel 存在、不知道 chat 存在、不知道 slash command 存在。

完整流程（用户视角不变）：
```
1. /cwd <path>     → binding 创建 + Session.Create（workspace）
2. /run <agent> [args]  → Session.Create（agent + args，workspace 复用）
3. ... 工作 ...
4. /close           → Session.Kill（binding 保留）
5. /run ...        → Session.Create 重 spawn（workspace + binding 复用）
```

**关键设计**：`/run` 智能处理——
- Session.Exited → spawn 新 CLI（保留 binding + workspace）
- Session.Running → reconnect（不重启，避免丢失 agent 内部状态）

---

## 2. Session 数据模型（v1.1）

```go
// internal/session/session.go
type Session struct {
    ID         string    // uuid / timestamp-based
    Workspace  string    // 被 /cwd 设置；session 级持久；创建后不变
    Agent      string    // 被 /run 设置；session 级持久
    Args       []string  // 被 /run 设置；透传给 agent CLI
    PID        int       // 当前 CLI 进程 pid；0 表示没跑
    StartedAt  time.Time
    LastRunAt  time.Time

    agentSession agent.AgentSession  // 进程句柄（PID=0 / Exited 时 nil）
    cancel       context.CancelFunc

    inputBuffer  *InputBuffer  // F-25；v1.1 不持有 receipt

    // v1.1 删除：
    // ChatID    string
    // ChatType  string
    // OnUserMessage func(content, userMsgID string) error
}
```

**关键不变式**：
- `Workspace` 在 `Session.Create` 之后**不可变**（即使 binding.Workspace 更新了，Session.Workspace 保持原值）
- `Agent` 在 `Session.Create` 之后**不可变**
- `Args` 在 `Session.Create` 之后**不可变**
- 改 workspace = 新 Session（binding 替换 SessionID）
- 改 agent = 先 `/close` 再 `/run`

**Session vs CLI 状态**：

| Session.Status | CLI 在跑？ | 含义 |
|----------------|-----------|------|
| Running | 是 | 完整工作状态；PID 有效 |
| Detached | 是（理论上）| nightme 关闭后 registry 标记；CLI 继续跑 |
| Exited | 否 | workspace + agent 保留；可 /run 重 spawn |

---

## 3. Manager Interface（v1.1 slim）

```go
// internal/session/manager.go

type Manager interface {
    // Create 注册新 session + spawn agent。返回的 Session.Status == Running 成功。
    Create(ctx context.Context, req CreateRequest) (*Session, error)

    // Get 返回 session by ID。
    Get(sid string) (*Session, error)

    // List 返回所有 session 的快照。
    List() []*Session

    // Kill 终止 agent 进程；session.Status → Exited；session record 保留。
    Kill(sid string) error

    // Restore 从 registry 读取 sessions 重建 in-memory 表（StatusRunning/Detached → Detached）。
    Restore(ctx context.Context) error

    // Persist 把当前 in-memory 状态写回 registry。
    Persist() error

    // MarkDetached 释放 live handle，不杀进程（daemon 关闭时用）。
    MarkDetached(sid string) error

    // SetEventCallback 注册 manager.readPump 的事件回调（v1.1 单消费者修复点）。
    SetEventCallback(cb EventCallback)
}
```

**v1.1 删除**（这些 leak 了 chatID 进 session）：
- ❌ `CreateOrUpdate(chatID, chatType, workspace, agent, args)`
- ❌ `Run(chatID, agent, extraArgs)`
- ❌ `GetByChat(chatID)`
- ❌ `KillByChat(chatID)`

`Binding → Session` 的查找是 Gateway 责任：`binding.SessionID` → `manager.Get(binding.SessionID)`。

```go
// v1.1 CreateRequest
type CreateRequest struct {
    Workspace string
    Agent     string
    Args      []string

    // OnFlushHook 由 Gateway 装入（InputBuffer flush 时触发）
    OnFlushHook func(blocks []agent.ContentBlock, userMsgIDs []string) error

    // EventCallback 是 v0.3+ 的另一种注入方式（Manager-level；用于 OnSessionEvent）
    // Gateway 在 startup 调 manager.SetEventCallback(gw.OnSessionEvent)
}
```

**Registry 持久化 schema**（v1.1 — 两张表）：
```json
{
  "version": 3,
  "sessions": {
    "s_01HF8XXXXX": {
      "session_id": "s_01HF8XXXXX",
      "workspace": "/home/devin/code/bailing",
      "agent": "claude",
      "args": ["--model", "opus"],
      "pid": 12345,
      "started_at": "2026-07-31T10:00:00+08:00",
      "last_run_at": "2026-07-31T11:00:00+08:00",
      "status": "running"
    }
  },
  "bindings": {
    "oc_xxxxx": {
      "chat_id": "oc_xxxxx",
      "chat_type": "p2p",
      "session_id": "s_01HF8XXXXX",
      "workspace": "/home/devin/code/bailing",
      "agent": "claude"
    }
  }
}
```

**v1.1 schema migration**：从 v0.2.x registry 读 `chat_id` 字段时，自动生成 `BindingEntry{ChatID, SessionID}`；旧 `SessionEntry` 移除 `chat_id` 字段。详见 [`F-05-process-registry.md`](./F-05-process-registry.md) §4。

---

## 4. Implementation

**文件**：
- `internal/session/session.go` — Session 数据结构（无 ChatID）
- `internal/session/manager.go` — Manager interface + MemoryManager 实现
- `internal/session/lifecycle.go` — Create / Get / Kill / MarkDetached
- `internal/session/input_buffer.go` — InputBuffer（无 receipts map）
- `internal/bridge/pty/pty.go` — pty.New
- `internal/registry/registry.go` — JSON 持久化（v1.1：两张表）
- `internal/gateway/cmd/handlers.go` — `/cwd` `/run` `/close` handlers（用 Gateway 的 binding 表）

### 4.1 `/cwd <path>` handler（v1.1：走 Gateway.binding）

```
handler.cwd(ctx, msg, args)
  ├ args 为空 → binding := gw.LookupByChat(msg.ChatID)
  │   └ binding.Workspace → Reply
  └ args 有 → workspace.Validate(path)
     ├ binding := gw.LookupByChat(msg.ChatID)
     │   ├ 存在且 binding.SessionID 指向 sess.Status == Running → 拒绝（"CLI running, /close first"）
     │   └ 否则 → 继续
     ├ agentName := (binding?.Agent) || (Registry.List() 第一个) || "claude"
     ├ sess := manager.Create(ctx, CreateRequest{
     │       Workspace: abs, Agent: agentName, Args: nil,
     │       OnFlushHook: gw.onInputBufferFlush,
     │   })
     ├ gw.Bind(msg.ChatID, msg.ChatType, sess.ID, abs, agentName)  // 新建或替换 binding
     ├ registry.Upsert(SessionEntry) + registry.Upsert(BindingEntry)
     └ Reply success
```

### 4.2 `/run <agent> [args]` handler（v1.1：Run 是 Gateway 逻辑）

```
handler.run(ctx, msg, args)
  ├ args 为空 → Reply usage
  ├ agentName := args[0]; agent.Get 校验
  ├ binding := gw.LookupByChat(msg.ChatID)
  │   └ nil → Reply "no workspace set"
  ├ sess := manager.Get(binding.SessionID)
  ├ sess.Status == Running → Reply "Already running, pid=N"
  └ 否则：
     ├ newSess := manager.Create(ctx, CreateRequest{
     │       Workspace: binding.Workspace,
     │       Agent: agentName,
     │       Args: args[1:],
     │       OnFlushHook: gw.onInputBufferFlush,
     │   })
     ├ gw.bindings[msg.ChatID].SessionID = newSess.ID
     ├ gw.bindings[msg.ChatID].Agent = agentName
     ├ registry.Upsert x2
     └ Reply "Started: <agent>, pid=N, cwd=<ws>"
```

**PTY 启动时的 args 合并**：
```go
// internal/bridge/pty/pty.go（或 claudecode）
finalArgs := append(agent.Args(), userArgs...)  // agent 默认 + 用户透传
cmd := exec.Command(agent.Command(), finalArgs...)
cmd.Dir = workspace
```

### 4.3 `/close` handler

```
handler.kill(ctx, msg, _)
  ├ binding := gw.LookupByChat(msg.ChatID)
  │   └ nil → Reply "no session to kill"
  ├ manager.Kill(binding.SessionID)
  └ Reply "session killed"
```

---

## 5. 状态转换图（v1.1）

```
                       /cwd (binding 不存在)
    [no binding] ────────────────────────────────► [binding → session, Running]
                                                                  │
                                                                  │ /run (or /cwd 触发 respawn)
                                                                  ▼
                              /close ◄─────────────── [binding → session, Running]
                                │                          ▲
                                │                          │ /run (CLI 死了)
                                ▼                          │
                         [binding → session, Exited] ──/run┘
                                │
                                │ (CLI 异常退出 / EOF)
                                ▼
                         [binding → session, Exited]   ← readPump 观察 EventDone/Error
```

**关键不变量**（v1.1）：

- 一旦 binding 存在，chat_id ↔ session_id 永久绑定（除非显式删除 binding，v0.4 才有）
- binding.Workspace 可更新（仅当 session.Exited）；session.Workspace 不可变
- session.Status 在 readPump 观察 `EventDone / EventError / EOF` 时从 Running → Exited
- nightme 重启后 session 从 registry 恢复（StatusRunning/Detached 都映射成 Detached）

---

## 6. Edge cases

| 场景 | 处理 |
|------|------|
| `/cwd` 无参数 | "usage: /cwd <path>" 或 "current workspace: <ws>"（binding 存在时）|
| `/cwd /nonexistent` | "workspace does not exist: /nonexistent" |
| `/cwd` 时 CLI 在跑 | "CLI running, /close first to change workspace" |
| `/run` 前没 `/cwd` | "no workspace set, send /cwd <path> first" |
| `/run` 无参数 | "usage: /run <agent> [args...]" |
| `/run foo`（未知 agent）| "unknown agent: foo" |
| `/run claude` 但 claude 不在 PATH | "claude binary not found" |
| `/run codex --bad-flag` | 透传，codex 自己报错 |
| `/run` 时 CLI 死了（sess.Exited）| spawn 新 CLI |
| `/run` 时 CLI 在跑（sess.Running）| reconnect，不重启 |
| `/close` 但 binding 不存在 | "no session to kill" |
| `/close` 后 binding 保留，user 再 `/run` | spawn 新 CLI（workspace 保留）|
| nightme 重启，binding 恢复 + session.Detached + PID 活着 | `/run` 时 sess.Status == Detached → spawn（PID 被覆盖）|
| nightme 重启，binding 恢复 + session.Detached + PID 死了 | `/run` 时 spawn |
| `/cwd /a` 然后 `/run claude` 然后 `/cwd /b` | 第二 /cwd 拒绝 "CLI running, /close first" |
| 用户狂发 `/run`（CLI 已在跑）| 每次都返回 "Already running"，不重启 |
| `manager.Create` 失败（PTY 错误）| 返回 err；binding 不更新；用户看到 error reply |
| `manager.Create` 成功但 registry.Upsert 失败 | log warn；in-memory 状态正确；下次 Persist 修复 |

---

## 7. Test plan

**单元测试**：
- `Session` struct 不含 ChatID 字段（静态检查）
- `MemoryManager.Create` 不知道 chatID；`CreateRequest` 不接受 ChatID
- `MemoryManager.Get` / `List` / `Kill` 用 session.ID 索引
- `MemoryManager.Restore` 从 v1.1 schema 读 sessions + bindings
- `MemoryManager.SetEventCallback` 注册回调，readPump 调用
- Session lifecycle：`Create → Running → Kill → Exited → Create (复用 ID) → Running`
- Session detach：`Running → MarkDetached → Detached → Restore → Detached`

**集成测试**：
- Gateway + MemoryManager + mock Channel: `/cwd` → `manager.Create` + `gateway.Bind` + `registry.Upsert x2`
- Gateway + MemoryManager + mock Channel: `/run` → `manager.Create` (新 session) + `binding.SessionID` 替换
- Gateway + MemoryManager + mock Channel: `/close` → `manager.Kill` + binding 保留

**E2E（M2）**：
- 飞书 DM `/cwd /tmp/foo` → "Workspace set"
- 飞书 DM `/run claude` → "Started: claude, cwd=/tmp/foo"
- 飞书 DM 发 "hello" → claude 收到
- `ps aux | grep claude` → 看到进程
- 飞书 DM `/run claude` → "Already running"
- 飞书 DM `/close` → "session killed"
- 飞书 DM `/run claude --model opus` → "Started: claude --model opus"
- nightme 重启，binding + session 从 registry 恢复
- 飞书 DM `/run claude` → spawn 或 reconnect（取决于 PID）

---

## 8. Open questions

- `/cwd <path>` 在 CLI 跑着时能否 update workspace？v1.1 拒绝；v0.4 可加 `--force` 或先 kill
- `/run` 是否允许切换 agent？v1.1 拒绝（必须 /close 后再 /run 新 agent），v0.4 评估
- 是否需要 `/forget` 命令清空 binding？v1.1 不需要
- session 永不过期吗？v1.1 是的；v0.4 可加 session TTL
- nightme 重启时如何判断 session 该 reconnect 还是 spawn？v1.1：detach 后 PID 失效一律 spawn（不尝试 reattach 老 PID）

---

## 9. Cross-references

- **完整的 binding + Run 逻辑**：见 [`F-20-gateway.md`](./F-20-gateway.md) §4
- **Registry 两张表 schema**：见 [`F-05-process-registry.md`](./F-05-process-registry.md) §3
- **InputBuffer onFlush hook**：见 [`F-25-rolling-log.md`](./F-25-rolling-log.md) §5.1
- **Cleanup / detach 策略**：见 [`F-06-process-cleanup.md`](./F-06-process-cleanup.md)
- **完整 v1.1 架构**：见 [`F-26-gateway-hub.md`](./F-26-gateway-hub.md) §5

---

## 10. Change log

- **2026-08-02** — v1.1: Session 删除 ChatID / ChatType / OnUserMessage 字段。Manager interface 移除 GetByChat / CreateOrUpdate / Run / KillByChat。binding 关系移至 Gateway 维护的 BindingEntry。Doc 重写。
- **2026-07-31** — v0.1: 原始 Session 设计（含 ChatID 字段、session 内部管 binding）。已被 v1.1 取代。