# nightme 运行时 (Session / PTY / Process / Workspace)

## A1. F-01: Session Lifecycle

> **Source**: `F-runtime.md`


> **Depends on**: F-04 (PTY), F-07 (Workspace binding), F-08 (Channel), F-09 (Agent), F-20 (Gateway)
> **Related docs**: [`SPEC.md`](../SPEC.md); [`F-gateway.md`](./F-gateway.md) §4; [`F-gateway.md`](./F-gateway.md) §5

---

## 1. Description

Session 是 nightme 管理的**单个 CLI 子进程**的进程抽象。它包含：

- **Workspace** —— CLI 进程的 cwd（不可变，创建后不变）
- **Agent** —— CLI 二进制 + 默认 args（不可变）
- **Args** —— `/run` 时透传的额外 args
- **PID** —— 当前 CLI 进程 pid；0 表示没跑
- **Status** —— `Running / Detached / Exited` 生命周期状态

**核心变化**：Session **不**持有 `ChatID` / `ChatType` / `OnUserMessage`。**Chat 绑定关系是 Gateway 的事**，由 Gateway 维护的 `BindingEntry` 表达。Session 是纯进程域对象，不知道 channel 存在、不知道 chat 存在、不知道 slash command 存在。

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

## 2. Session 数据模型

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

    inputBuffer  *InputBuffer  // F-25；不持有 receipt

    // 删除：
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

## 3. Manager Interface

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

    // SetEventCallback 注册 manager.readPump 的事件回调。
    SetEventCallback(cb EventCallback)
}
```

**删除**（这些 leak 了 chatID 进 session）：
- ❌ `CreateOrUpdate(chatID, chatType, workspace, agent, args)`
- ❌ `Run(chatID, agent, extraArgs)`
- ❌ `GetByChat(chatID)`
- ❌ `KillByChat(chatID)`

`Binding → Session` 的查找是 Gateway 责任：`binding.SessionID` → `manager.Get(binding.SessionID)`。

```go
// CreateRequest
type CreateRequest struct {
    Workspace string
    Agent     string
    Args      []string

    // OnFlushHook 由 Gateway 装入（InputBuffer flush 时触发）
    OnFlushHook func(blocks []agent.ContentBlock, userMsgIDs []string) error

    // EventCallback 是  的另一种注入方式（Manager-level；用于 OnSessionEvent）
    // Gateway 在 startup 调 manager.SetEventCallback(gw.OnSessionEvent)
}
```

**Registry 持久化 schema**：
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

**schema migration**：从 registry 读 `chat_id` 字段时，自动生成 `BindingEntry{ChatID, SessionID}`；`SessionEntry` 移除 `chat_id` 字段。详见 [`F-runtime.md`](./F-runtime.md) §4。

---

## 4. Implementation

**文件**：
- `internal/session/session.go` — Session 数据结构（无 ChatID）
- `internal/session/manager.go` — Manager interface + MemoryManager 实现
- `internal/session/lifecycle.go` — Create / Get / Kill / MarkDetached
- `internal/session/input_buffer.go` — InputBuffer（无 receipts map）
- `internal/bridge/pty/pty.go` — pty.New
- `internal/registry/registry.go` — JSON 持久化
- `internal/gateway/cmd/handlers.go` — `/cwd` `/run` `/close` handlers（用 Gateway 的 binding 表）

### 4.1 `/cwd <path>` handler

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

### 4.2 `/run <agent> [args]` handler

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

## 5. 状态转换图

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

**关键不变量**：

- 一旦 binding 存在，chat_id ↔ session_id 永久绑定（除非显式删除 binding，才有）
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
- `MemoryManager.Restore` 从 schema 读 sessions + bindings
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

- `/cwd <path>` 在 CLI 跑着时能否 update workspace？拒绝；可加 `--force` 或先 kill
- `/run` 是否允许切换 agent？拒绝（必须 /close 后再 /run 新 agent），评估
- 是否需要 `/forget` 命令清空 binding？不需要
- session 永不过期吗？是的；可加 session TTL
- nightme 重启时如何判断 session 该 reconnect 还是 spawn？detach 后 PID 失效一律 spawn（不尝试 reattach 老 PID）

---

## 9. Cross-references

- **完整的 binding + Run 逻辑**：见 [`F-gateway.md`](./F-gateway.md) §4
- **Registry 两张表 schema**：见 [`F-runtime.md`](./F-runtime.md) §3
- **InputBuffer onFlush hook**：见 [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md) §5.1
- **Cleanup / detach 策略**：见 [`F-runtime.md`](./F-runtime.md)
- **完整 架构**：见 [`F-gateway.md`](./F-gateway.md) §5

---

## 10. Change log

---

## A2. F-04: PTY Simulation (Bridge PTY Backend)

> **Source**: `F-runtime.md`


> **Depends on**: (none — foundation)
> **Related docs**: SPEC.md §1.1 (Bridge 组件), [../bridge/cli-transport.md](./../bridge/cli-transport.md) (PTY byte pipe), [../bridge/cli-transport.md](./../bridge/cli-transport.md) (Bridge 三层模式), §4 (并发模型)

## 1. Description

在 pseudo-terminal (PTY) 中 spawn AI Coding CLI 进程，让 CLI 以为自己跑在真实终端里（颜色、进度条、交互 prompt 都正常）。nightme 通过 PTY master fd 与 CLI 通信。

## 2. Interface

```go
// internal/bridge/pty/pty.go
type Bridge interface {
    io.ReadWriteCloser
    PID() int
    Setsize(cols, rows int) error
}

func New(workspace string, command string, args []string, env []string, cols int, rows int) (Bridge, error)
```

**实现细节**（基于 `aymanbagabas/go-pty`）：

```go
import "github.com/aymanbagabas/go-pty"

type ptyBridge struct {
    ptmx pty.Pty   // master fd
    cmd  *exec.Cmd
}

func New(workspace, command string, args []string, env []string, cols, rows int) (Bridge, error) {
    cmd := exec.Command(command, args...)
    cmd.Dir = workspace
    cmd.Env = append(os.Environ(), env...)

    ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
        Rows: uint16(rows),
        Cols: uint16(cols),
    })
    if err != nil {
        return nil, err
    }
    return &ptyBridge{ptmx: ptmx, cmd: cmd}, nil
}

func (b *ptyBridge) Read(p []byte) (int, error)  { return b.ptmx.Read(p) }
func (b *ptyBridge) Write(p []byte) (int, error) { return b.ptmx.Write(p) }
func (b *ptyBridge) Close() error                 { return b.ptmx.Close() }
func (b *ptyBridge) PID() int                     { return b.cmd.Process.Pid }
func (b *ptyBridge) Setsize(cols, rows int) error {
    return b.ptmx.Setsize(&pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}
```

## 3. Implementation

**文件**：
- `internal/bridge/pty/pty.go` — Bridge 接口 + ptyBridge 实现
- `go.mod` — `github.com/aymanbagabas/go-pty`

**默认配置**（来自 `configs/nightme.example.yaml`）：
```yaml
session:
  default_pty_cols: 120
  default_pty_rows: 40
```

**为什么选 aymanbagabas/go-pty**：
- API 干净（一个 `Start` 一个 `Setsize`）
- macOS/Linux 跨平台处理已经做好
- resize 支持简单
- 与 creack/pty 接口差异小，备选切换成本 ~30 行

## 4. Edge cases

| 场景 | 处理 |
|------|------|
| command 不在 PATH | `pty.Start` 返回 error → 冒泡到 session manager |
| workspace 不存在 | `cmd.Start` 报错（在 aymanbagabas 内部处理）|
| command 启动后立刻 exit | `Read` 返回 EOF → session 立即进入 exited |
| 用户配置了超大的 cols/rows (e.g. 9999) | 限制上限（如 500x500），避免 PTY buffer 爆 |
| SIGWINCH resize | `Setsize()` 调用 |
| PTY 在 macOS 上的 fork+exec 问题 | aymanbagabas 已处理；如果遇到 fallback 到 creack/pty |
| cmd 已 Exit 但 ptmx 没 close | `Read` 返回 0 + nil error，跳过；`cmd.Wait()` 检测退出状态 |

## 5. Test plan

**单元测试**：
- `New(t.TempDir(), "/bin/echo", []string{"hello"}, ...)` → bridge.Read() 应返回 "hello\n"
- `Setsize(200, 50)` 不报错

**集成测试**：
- spawn `tty` 命令 → 验证 stdout 是 "not a tty" → 改为 PTY → 验证 stdout 是 "/dev/ttysXXX"
- spawn `stty size` → 验证 Setsize 生效

**手动测试**：
- spawn `/bin/zsh --interactive` → nightme 输入 `ls` → 看到颜色输出

## 6. Open questions

- 是否需要 PTY echo 关闭？倾向 保持默认 echo on（Claude Code 不依赖）
- macOS ConPTY 是否要支持？不支持（macOS 用 POSIX PTY）
- 是否需要记录 PTY 字节流到 log（debug 用）？不记录，加开关

---

## A3. F-05: Process Registry (Two-Table Persistence)

> **Source**: `F-runtime.md`


> **Depends on**: F-01 (Session)
> **Related docs**: [`SPEC.md`](../SPEC.md); [`F-gateway.md`](./F-gateway.md) §6 commit 5; [`F-runtime.md`](./F-runtime.md) §3

---

## 1. Description

nightme 把所有 runtime state 持久化到 JSON 文件，用于：
1. 重启后恢复 binding + session（F-01 lifecycle）
2. `nightme list` 命令（F-10）查询
3. nightme 崩溃后知道哪些进程是自己的（F-06 cleanup）

**核心变化**：registry 由**两张表**组成——`SessionEntry`（session 状态 + workspace + agent + PID）与 `BindingEntry`（chat_id ↔ session_id + chat_type）。的两张表组成。

---

## 2. Interface

```go
// internal/registry/registry.go

type Status string
const (
    StatusRunning  Status = "running"
    StatusDetached Status = "detached"
    StatusExited   Status = "exited"
)

// SessionEntry 是 session 状态持久化记录。不含 ChatID。
type SessionEntry struct {
    SessionID  string    `json:"session_id"`
    Workspace  string    `json:"workspace"`
    Agent      string    `json:"agent"`
    Args       []string  `json:"args"`
    PID        int       `json:"pid"`
    StartedAt  time.Time `json:"started_at"`
    LastRunAt  time.Time `json:"last_run_at"`
    Status     Status    `json:"status"`
    ExitCode   *int      `json:"exit_code,omitempty"`
}

// BindingEntry 是 Gateway 维护的 chat ↔ session 映射。新增。
type BindingEntry struct {
    ChatID    string `json:"chat_id"`
    ChatType  string `json:"chat_type"`
    SessionID string `json:"session_id"`
    Workspace string `json:"workspace"`  // denormalized for /cwd reply
    Agent     string `json:"agent"`      // denormalized for /run reply
}

// File 是 registry 的 JSON 持久化实现。
type File struct {
    mu       sync.RWMutex
    path     string
    sessions map[string]SessionEntry  // SessionID → entry
    bindings map[string]BindingEntry  // ChatID → entry
}

func Open(path string) (*File, error)
func (f *File) Close() error

// SessionEntry ops:
func (f *File) UpsertSession(e SessionEntry) error
func (f *File) GetSession(sid string) (SessionEntry, bool)
func (f *File) DeleteSession(sid string) error
func (f *File) ListSessions() []SessionEntry

// BindingEntry ops:
func (f *File) UpsertBinding(e BindingEntry) error
func (f *File) GetBinding(chatID string) (BindingEntry, bool)
func (f *File) DeleteBinding(chatID string) error
func (f *File) ListBindings() []BindingEntry

// Migration:
func (f *File) Migrate() error  // 不同 schema → 新 schema
```

**文件位置**：`~/.nightme/registry.json`（可通过配置覆盖）
**文件权限**：`0600`
**持久化**：每次 Upsert 后立即 `fsync`

---

## 3. JSON schema

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
      "started_at": "2026-07-31T10:30:00+08:00",
      "last_run_at": "2026-07-31T11:00:00+08:00",
      "status": "running"
    }
  },
  "bindings": {
    "oc_abc123": {
      "chat_id": "oc_abc123",
      "chat_type": "p2p",
      "session_id": "s_01HF8XXXXX",
      "workspace": "/home/devin/code/bailing",
      "agent": "claude"
    }
  }
}
```

**关键变化**：
- 顶层 `version: 3`
- 新增 `bindings` 顶层 key
- `SessionEntry` 移除 `chat_id` 字段（不再属于 session 域）

---

## 4. Schema migration

**触发条件**：`Open()` 读 registry.json 时检测 `version != 3`。

**schema (version 2)**：
```json
{
  "version": 2,
  "sessions": {
    "s_01HF8XXXXX": {
      "session_id": "s_01HF8XXXXX",
      "chat_id": "oc_abc123",
      "workspace": "/home/devin/code/bailing",
      "agent": "claude",
      "pid": 12345,
      "started_at": "...",
      "status": "running"
    }
  }
}
```

**Migrate() 步骤**：
1. 备份 `registry.json` → `registry.json.v2.bak`
2. 遍历 `sessions[*]`：
   - 提取 `chat_id` 字段
   - 创建 `BindingEntry{ChatID: chat_id, ChatType: "" /* unknown → "group" 安全侧 */, SessionID: session_id, Workspace, Agent}`
   - 写入 `bindings` 表
   - 从 `SessionEntry` 删除 `chat_id` 字段
3. 顶层 `version: 2` → `3`
4. 写回文件 + fsync

**迁移后 invariant**：
- 所有 sessions 都有对应的 binding（即使 ChatType 未知，按 group 处理）
- PID 字段保留（`MarkDetached` 后 PID 检查可用）
- 如果同一个 chat_id 在 有多个 sessions（不该发生，但防御）：保留最新启动的 binding，其他 binding.ChatID 不重复

**降级**：不识别 `bindings` key，会忽略；能继续工作但 binding 信息丢失。建议 binary 启动时检测到 时拒绝启动并提示升级。

---

## 5. Implementation

**文件**：
- `internal/registry/registry.go` — File struct + 两表 + Migrate
- `internal/registry/registry_test.go` — 单测 + migration 测试
- `internal/registry/migrate.go` — version 2 → version 3 转换

**持久化流程**：
```
session.Create / session.Kill → MemoryManager.upsertEntry → registry.UpsertSession
gateway.Bind / Rebind → registry.UpsertBinding
nightme shutdownRun → manager.Persist + registry.Persist (一次 fsync 两表)
nightme 启动 → registry.Open → Migrate if needed → MemoryManager.Restore + Gateway.RestoreBindings
```

**并发安全**：
- 全局 `sync.RWMutex` 保护两个 map
- 写操作：加 Lock → 改 map → 写文件 → Unlock
- 读操作：加 RLock → 读 map → Unlock（不读文件）
- UpsertSession 和 UpsertBinding **分别独立** lock（避免锁整个 registry）

---

## 6. Edge cases

| 场景 | 处理 |
|------|------|
| 文件不存在 | Open() 创建空 registry（version=3, sessions={}, bindings={}）|
| 文件（不同 schema）| Migrate() 自动转换 + 备份原文件 |
| 文件（更不同 schema）| Open() 返回 error + log "nightme requires migration ; please run nightme first" |
| JSON 解析失败 | log error + 自动备份损坏文件为 `.bak` + 重置为空 |
| 文件权限 0600 被改成 0644 | Open() 检测 + warn（不强制修复）|
| 并发 Upsert 同一 session | mutex 串行化；冲突时后写覆盖前写 |
| registry 文件丢失 | 启动时检测 + warn，binding + session 列表为空 |
| 磁盘满 | fsync 失败 → 返回 error → session 创建回滚 |
| 文件被外部修改 | 下次 Upsert 时覆盖（nightme 是 single owner）|
| Session.PID 在系统重启后失效 | `MarkDetached` + Restore 标 Detached；`/run` 时检测到 PID 失效直接 spawn 新 CLI |
| 升级时 binding.ChatType 未知 | 默认 "group"（安全侧）|
| 升级后 binary 写出 → binary 读 | 忽略 bindings 字段；可能错过 binding 信息，提示用户升级 |

---

## 7. Test plan

**单元测试**：
- `Open` + `UpsertSession` + `GetSession` 一致性
- `UpsertBinding` + `GetBinding` 一致性
- `Delete` 后 `Get` 返回 false
- `ListSessions` / `ListBindings` 排序（按 StartedAt / ChatID）
- 并发 Upsert 无 race（`-race` flag）
- 文件损坏恢复（写入垃圾 → Open → 应 fallback 到空 + 备份）
- **→ migration**：构造 schema 文件 → Migrate → 验证两表内容正确 + 备份文件存在 + version=3

**集成测试**：
- Gateway + MemoryManager + registry: Create session → 验证 SessionEntry 写入
- Gateway Bind → 验证 BindingEntry 写入
- 重启 mock（重新 Open registry）→ MemoryManager.Restore + Gateway.RestoreBindings → 状态一致
- 模拟文件损坏 → 自动备份 + 重置
- **→ upgrade path**：binary 创建的 registry 用 binary 启动 → Migrate 自动跑 → 后续行为正确

**手动 E2E**：
- nightme 跑 → 创建 sessions + 落盘 → 关闭
- 升级到 binary → 启动 → log "migrated registry from v2 to v3" → 飞书 DM 一切正常

---

## 8. Open questions

- 是否需要迁移到 SQLite？不需要（< 100 sessions + bindings，JSON 够用）
- 是否记录 session 的 stdout 历史？不记录（F-15 再做）
- PID 在 macOS 上的 recycle 问题？简化：detach 后不检查 PID，下次 /run 直接 spawn 新 CLI 覆盖之前的 PID
- BindingEntry.Workspace / Agent 冗余：denormalized 是为了 /cwd reply / /run reply 不需要再读 SessionEntry；trade-off 是 workspace 改了需要更新两个地方（已实现 Rebind 路径同步）

---

## 9. Cross-references

- **Session 数据模型**：见 [`F-runtime.md`](./F-runtime.md) §2, §3
- **Cleanup 行为**：见 [`F-runtime.md`](./F-runtime.md)
- **完整 架构**：见 [`F-gateway.md`](./F-gateway.md) §6 commit 5

---

## 10. Change log

---

## A4. F-06: Process Cleanup

> **Source**: `F-runtime.md`


> **Depends on**: F-05 (Registry)
> **Related docs**: [`SPEC.md`](../SPEC.md); [`F-runtime.md`](./F-runtime.md); [`F-runtime.md`](./F-runtime.md); [`F-gateway.md`](./F-gateway.md) §4.3

---

## 1. Description

nightme 关闭时（SIGTERM/SIGINT/crash）决定如何处理自己启动的 CLI 进程。**默认策略 = 不 kill，标记 detached**，让 CLI 继续在后台跑；用户下次启动 nightme 时 binding + session 自动恢复。可选 `--cleanup` 标志位强制 kill。

**路径变化**：

| 旧| 新|
|-----------|-----------|
| 关闭路径用 `session.ChatID` 索引 | 关闭路径用 `manager.List() → manager.MarkDetached(id)` / `manager.Kill(id)` |
| `cleanup.OnShutdown(policy)` 单独组件 | 直接在 `cmd/nightme/shutdownRun` 里实现（无独立 cleanup 包）|
| `--cleanup` flag 在 main.go 注册 | `--cleanup` flag 仍在 runCmd 注册 |

---

## 2. Interface

```go
// 在 session.Manager 上
type Manager interface {
    // ... 其它 ...
    MarkDetached(sid string) error  // 释放 live handle，不杀进程
    Kill(sid string) error          // 终止进程，sess.Status → Exited
}

// CLI flag
var cleanup = flag.Bool("cleanup", false, "kill all running nightme sessions on shutdown")
```

**行为矩阵**：

| 触发 | 默认 (detach) | --cleanup |
|------|---------------|-----------|
| SIGTERM | 标记所有 sessions 为 detached | kill 所有 running sessions |
| SIGINT (Ctrl+C) | 同上 | 同上 |
| SIGKILL | 子进程变孤儿，OS 兜底 | 同上 |
| 启动时 | Restore sessions + bindings；StatusRunning/Detached 都映射成 StatusDetached | 同上；/run 时会 spawn 新 CLI（不试图 reattach 之前的 PID）|

---

## 3. Implementation

**文件**：
- `cmd/nightme/run.go` — `runRun` + `shutdownRun`（不再有独立 `cleanup` 包）
- `internal/session/manager.go` — `MarkDetached` / `Kill` 实现

**流程（SIGTERM / SIGINT）**：
```
main 收到 SIGTERM (signal.Notify)
  ↓
runRun 退出 → defer shutdownRun()
  ↓
shutdownRun(ctx, ch, mgr, cleanup)
  ├ ch.Stop(ctx)                    // 关 Channel adapter
  ├ gw.Stop(ctx)                    // 关 gateway dispatch goroutines
  ├ gw.PersistBindings()            // flush binding 表到 registry
  ├ mgr.Persist()                   // flush session 表到 registry
  ├ if cleanup:
  │   └ for sess in mgr.List():
  │       └ if sess.Status == Running:
  │           └ mgr.Kill(sess.ID)   // SIGTERM → 5s → SIGKILL
  └ else:
      └ for sess in mgr.List():
          └ if sess.Status == Running:
              └ mgr.MarkDetached(sess.ID)   // 释放 handle，CLI 继续跑
  ↓
main 退出
```

**关键不变式**：
- shutdownRun 不知道 chat_id；只遍历 `manager.List()` 处理每个 session
- binding 由 Gateway 持久化（独立调用 `gw.PersistBindings()`）
- session 由 Manager 持久化（`mgr.Persist()`）
- 两次 registry 写都在 ch.Stop 之后，避免发消息时 registry 已经标 detached

**流程（启动 restore）**：
```
main 启动
  ↓
registry.Open(path) → 不同 schema 时 Migrate()
  ↓
agents := buildRegistry(cfg)
  ↓
mgr := session.NewMemoryManager(agents, reg, /* EventCallback */ gw.OnSessionEvent)
mgr.Restore(ctx)            // 从 registry.sessions 重建 in-memory 表
  ↓
gw := gateway.New(messageDispatcher)
gw.RestoreBindings(reg.ListBindings())  // 从 registry.bindings 重建 binding 表
  ↓
ch.Start(ctx)  // Feishu WS 连上
  ↓
... 处理消息 ...
```

**Status mapping on Restore**：
- registry.StatusRunning → MemoryManager.StatusDetached（PID 可能已死，下次 /run 重 spawn）
- registry.StatusDetached → MemoryManager.StatusDetached
- registry.StatusExited → MemoryManager.StatusExited

binding 重建：每个 `BindingEntry{ChatID, ChatType, SessionID, Workspace, Agent}` → `gw.bindings[ChatID] = BindingEntry{...}`。如 SessionEntry 不存在（脏数据），binding 保留但下次 `/cwd` 时覆盖。

---

## 4. Edge cases

| 场景 | 处理 |
|------|------|
| 子进程在 SIGTERM 后 5s 内不退 | bridge.Close() 发 SIGKILL 兜底 |
| 子进程已被用户手动 kill | Wait() 立即返回 → skip kill |
| 子进程变 zombie | registry.DeleteSession，OS 自动 reap |
| nightme crash（无 SIGTERM）| 子进程变孤儿；下次启动 Restore 标 Detached；/run 时 spawn 新 CLI |
| 用户 `kill -9 nightme` | SIGTERM handler 不触发，子进程孤儿；下次启动处理同上 |
| 多个 nightme 实例同时跑（用户误操作）| 不防，假设只有一个 nightme 进程 |
| 子进程的 stdin 已经断开但还在跑 | bridge.Close() 发 SIGHUP（待 aymanbagabas 是否支持）|
| Detach 时 binding 已不存在 | 不影响（shutdownRun 不依赖 binding）|
| Detach 时 session 已 Exited（race）| MarkDetached idempotent：sess.Status != Exited 才标 Detached |
| Restore 时 SessionEntry 缺失但 BindingEntry 在 | binding 保留；下次 /cwd 覆盖；log warn |
| Restore 时 BindingEntry 缺失但 SessionEntry 在 | session 留在 manager；binding 在下次 /cwd 时建立 |
| Restore 时两边都有但 SessionID 对不上 | 重建两表时按 ID 索引，不强一致；下次 /run 时 binding.SessionID 替换 |
| registry migration 失败| Open() 返回 error；nightme 启动失败 + 提示 |

---

## 5. Test plan

**单元测试**：
- `MemoryManager.MarkDetached` 释放 handle + 标 StatusDetached + upsert SessionEntry
- `MemoryManager.Kill` 终止 + 标 StatusExited + upsert SessionEntry
- `cmd/nightme/shutdownRun` mock Channel + manager → 验证 detach / kill 路径
- `registry.Migrate` → 数据转换正确

**集成测试**：
- 启动 nightme + 创建 session + 发 SIGTERM → 验证 session CLI 仍跑 + registry 标 detached
- 启动 nightme + --cleanup + 发 SIGTERM → 验证 session CLI 被 kill
- 启动 nightme 写 registry → 升级到 → 启动 → registry 自动 migrate

**手动测试**：
- 启动 session → `kill -TERM <nightme_pid>` → `ps aux | grep claude` → 仍在跑
- 重启 nightme → 飞书 DM 一切正常（binding + session 已恢复）
- 启动 session → `kill -9 <nightme_pid>` → 子进程孤儿 → 重启 nightme → /run → 新 CLI spawn

---

## 6. Open questions

- 是否需要 "soft kill"（先发退出命令，再 SIGTERM）？不做
- 默认 detach 是否会让用户困惑？决策：detach 是合理的（用户主动 /close 显式语义）
- 是否检测 "用户通过其他 channel kill 了 session"？不做
- Restore 时是否检查 PID 还活着？决策：**不检查**，下次 /run 一律 spawn 新 CLI 覆盖之前的 PID。简化实现，避免 PID recycle 误判

---

## 7. Cross-references

- **Session Status 状态机**：见 [`F-runtime.md`](./F-runtime.md) §2
- **Registry schema + migration**：见 [`F-runtime.md`](./F-runtime.md) §3, §4
- **/close slash command**：见 [`F-gateway.md`](./F-gateway.md) §4.3
- **完整架构**：见 [`F-gateway.md`](./F-gateway.md)

---

## 8. Change log

---

## A5. F-07: Workspace Binding

> **Source**: `F-runtime.md`


> **Depends on**: F-01 (Session), F-04 (PTY)
> **Related docs**: [`SPEC.md`](../SPEC.md)§4; [`F-gateway.md`](./F-gateway.md) §4.1; [`F-runtime.md`](./F-runtime.md)

---

## 1. Description

CLI 进程启动时 `cwd = session.Workspace`。验证 workspace 路径存在 + 是目录 + 当前用户有可执行权限（确保 agent CLI 能跑）。

**调用方变化**：

| 旧| 新|
|-----------|-----------|
| `session.Manager.CreateOrUpdate(chatID, ..., abs, ...)` 内部调 Validate | `gateway.handler.cwd` 内部调 Validate → `manager.Create(abs, ...)` |
| workspace.Validate 是 session 包的内部细节 | workspace.Validate 是 gateway.handler 调用的工具函数（仍是 `internal/workspace/` 包）|

Workspace 验证本身**没变**——还是 Resolve（`~` 展开 + 绝对路径）+ Validate（存在 + 目录 + 可执行）。变的只是"谁调用"。

---

## 2. Interface

```go
// internal/workspace/workspace.go

type Validator interface {
    Validate(path string) error
}

type PathResolver interface {
    Resolve(path string) (string, error)
}

// Validate returns nil if path is usable, error otherwise
func Validate(path string) error {
    info, err := os.Stat(path)
    if err != nil { return err }
    if !info.IsDir() { return ErrNotDirectory }
    // 检查可执行权限（用于后续 spawn 子进程）
    if info.Mode().Perm()&0o700 == 0 { return ErrNoExecute }
    return nil
}

func Resolve(path string) (string, error) {
    if strings.HasPrefix(path, "~") {
        home, _ := os.UserHomeDir()
        path = filepath.Join(home, path[1:])
    }
    return filepath.Abs(path)
}

var (
    ErrNotExist    = errors.New("workspace: not exist")
    ErrNotDirectory = errors.New("workspace: not a directory")
    ErrNoExecute   = errors.New("workspace: no execute permission")
)
```

---

## 3. Implementation

**文件**：
- `internal/workspace/workspace.go` — Validate + Resolve
- `internal/workspace/workspace_test.go` — 单测

**调用点**：
- `internal/gateway/cmd/handlers.go` 的 `handler.cwd` —— 在 `manager.Create` 之前调 `workspace.Validate(abs)`
- 不在 session 包内调

**为什么在 spawn 之前验证**：
- 避免 spawn 失败的副作用（PTY half-open 等）
- 给用户清晰的错误信息（path 不存在 / 不是目录 / 不可执行）

---

## 4. 调用顺序（`/cwd` handler）

```go
// internal/gateway/cmd/handlers.go
func (h *handlerContext) cwd(ctx context.Context, msg *gateway.InboundMessage, args []string) (*gateway.CommandResult, error) {
    if len(args) == 0 {
        existing, err := h.manager.Get(h.gateway.LookupByChat(msg.ChatID).SessionID)
        // ... 显示当前 workspace
    }

    path := args[0]
    if strings.HasPrefix(path, "~") {
        home, _ := os.UserHomeDir()
        path = filepath.Join(home, strings.TrimPrefix(path, "~"))
    } else if !filepath.IsAbs(path) {
        home, _ := os.UserHomeDir()
        path = filepath.Join(home, path)
    }
    abs, err := filepath.Abs(path)
    if err != nil { return reply(err) }
    info, err := os.Stat(abs)
    if err != nil { return reply(err) }
    if !info.IsDir() { return reply("not a directory") }

    // workspace.Validate 已通过（手写展开 + Stat）
    // 决定 agentName → manager.Create → gateway.Bind
    // ...
}
```

注：实际实现里，handlers.go 仍手写 `~` 展开 + `filepath.Abs` + `os.Stat` 而不是调 `workspace.Resolve` + `workspace.Validate`。这两套路径等价；可以选择保留手写或迁到 workspace 包调用。两者都满足"session 不 import workspace 包"的约束。

---

## 5. Edge cases

| 场景 | 处理 |
|------|------|
| 路径不存在 | 返回 ErrNotExist → handler Reply "workspace not found: <path>" |
| 路径是文件不是目录 | 返回 ErrNotDirectory → handler Reply |
| 路径无执行权限（755 但用户无 x）| 返回 ErrNoExecute → handler Reply |
| 路径是 symlink | 跟随 symlink 验证 target（`os.Stat` 默认跟随）|
| 路径是相对路径 | Resolve 转为绝对路径|
| 路径含 `~` | Resolve 展开为 `$HOME` |
| 路径含空格 / 中文 / emoji | 原样保留（PTY 启动不 care）|
| 路径在 remote filesystem（NFS）| Validate 可能慢，加 timeout|
| 用户取消（chat 走了 /close）| 不影响验证（验证是无状态的）|

---

## 6. Test plan

**单元测试**：
- `workspace.Validate("/tmp")` → nil
- `workspace.Validate("/nonexistent")` → ErrNotExist
- `workspace.Validate("/etc/passwd")` → ErrNotDirectory
- `workspace.Resolve("~/code")` → `/home/devin/code`（mock UserHomeDir）
- `workspace.Resolve(".")` → `/cwd/absolute/path`

**集成测试**：
- `handler.cwd` 收到 `/cwd /nonexistent` → Reply error，不创建 binding
- `handler.cwd` 收到 `/cwd /etc/passwd` → Reply "not a directory"
- `handler.cwd` 收到 `/cwd /tmp/foo` → binding 创建 + Session spawn + registry 两表写入

---

## 7. Open questions

- 是否支持 workspace 是 git URL（如 `git@github.com:foo/bar` 自动 clone）？不支持
- 是否在 session 跑的过程中检测 workspace 被删除？不检测（F-06 cleanup 时才检查）
- 可执行权限检查在 macOS / Linux 上是否准确？`info.Mode().Perm()&0o700` 是 user bit，root 用户不需要检查
- workspace.Validate 是否应该返回 typed error 而不是 wrapped error？当前用 fmt.Errorf("%w")，调用方 errors.Is 检查

---

## 8. Cross-references

- **/cwd handler 完整流程**：见 [`F-gateway.md`](./F-gateway.md) §4.1
- **Session 数据模型**：见 [`F-runtime.md`](./F-runtime.md) §2
- **完整 架构**：见 [`F-gateway.md`](./F-gateway.md)

---

## 9. Change log

---

## A6. F-10: Session List Command

> **Source**: `F-10-session-list-cmd.md`


> **Depends on**: F-05 (Registry)
> **Related docs**: [SPEC.md](../SPEC.md)

## 1. Description

`nightme list` CLI 命令通过本地 HTTP API（127.0.0.1:7823）查询主进程，返回所有 session 状态（running / detached / exited）。`nightme kill <sid>` 强制 kill 指定 session。

## 2. Interface

```go
// internal/ipc/server.go
type Server interface {
    Start(ctx context.Context) error  // listen 127.0.0.1:7823
    Stop(ctx context.Context) error
}

type ListResponse struct {
    Sessions []SessionInfo `json:"sessions"`
}

type SessionInfo struct {
    SID        string    `json:"sid"`
    ChatID     string    `json:"chat_id"`
    Workspace  string    `json:"workspace"`
    Agent      string    `json:"agent"`
    PID        int       `json:"pid"`
    Status     string    `json:"status"`  // "running" | "detached" | "exited"
    StartedAt  time.Time `json:"started_at"`
    ExitCode   *int      `json:"exit_code,omitempty"`
}

// HTTP routes (internal/ipc/router.go):
//   GET  /v1/sessions        → ListResponse
//   GET  /v1/sessions/{sid}  → SessionInfo
//   POST /v1/sessions/{sid}/close  → 204
```

**CLI 命令**：
```bash
nightme list              # 列出所有 session
nightme list --json       # 输出 JSON
nightme kill <sid>        # 强制 kill session CLI
nightme status            # 检查主进程是否在跑 + session 数
```

## 3. Implementation

**文件**：
- `internal/ipc/server.go` — HTTP server（仅 127.0.0.1:7823）
- `internal/ipc/router.go` — chi router + handlers
- `cmd/nightme/list.go` — `nightme list` 子命令
- `cmd/nightme/close.go` — `nightme kill` 子命令

**输出格式**（text mode）：
```
SID              AGENT   WORKSPACE                              PID     STATUS      STARTED
s_01HF8XXXXX     claude  /home/devin/code/bailing               12345   running     10:30:00
s_01HF9XXXXX     claude  /home/devin/code/nightme               12350   detached    10:35:12
s_01HF10XXXXX    claude  /tmp/test                               -       exited(0)   11:00:00
```

**实现选择**：用 `cobra` 做 CLI 框架。

## 4. Edge cases

| 场景 | 处理 |
|------|------|
| 主进程没跑（nightme daemon 没启动）| list 命令 HTTP connect 失败 → 提示 "nightme daemon not running" |
| registry 文件损坏 | API 返回 500 + error message |
| kill 的 session 不存在 | API 返回 404 |
| kill 的 session 状态是 exited | 返回 200，提示 "session already exited" |
| 多个 nightme daemon（用户误操作）| 不防，假设只有一个 |
| 本地 7823 端口被占用 | Server.Start 返回 error，主进程退出 |
| 通过远程访问| Server 仅 listen 127.0.0.1，远程不可达 |

## 5. Test plan

**单元测试**：
- 表格格式化：3 个 session → 输出 3 行 + header
- JSON 序列化：SessionInfo → JSON marshal 一致

**集成测试**：
- 启动 mock IPC server → 调用 GET /v1/sessions → 验证 response
- POST /v1/sessions/{sid}/close → 验证 mock process 被 kill

**手动测试**：
- `nightme list` → 看到当前 session
- `nightme kill s_XXX` → 飞书 DM 收到 "session killed"

## 6. Open questions

- 是否需要 `nightme attach <sid>` 进入交互式 terminal？不做，留 - 是否需要 `nightme logs <sid>` 看 stdout 历史？不记录历史（F-15）
- 是否需要 Unix socket（不用 TCP）？用 TCP 简单；改 socket
- IPC 是否要鉴权？不需要（仅 127.0.0.1）

---

