# nightme — Architecture

> **状态**：v1.0（与 SPEC 同步）
> **作者**：🦞 虾哥（PM/Architect）
> **日期**：2026-07-31
> **依赖**：`SPEC.md`、`FEATURES.md`

本文档回答：**nightme 由哪些模块组成、各模块怎么协作、数据/控制流怎么走**。

---

## 1. 模块概览

```
nightme
├── cmd/nightme/main.go              # 入口
├── internal/
│   ├── config/                       # YAML 解析 + 默认值
│   ├── channel/                      # Channel interface + 实现
│   │   ├── channel.go                #   interface 定义
│   │   └── feishu/                   #   飞书 adapter（lark-oapi）
│   ├── agent/                        # Agent interface + 实现
│   │   ├── agent.go                  #   interface 定义
│   │   └── claude/                   #   Claude Code adapter
│   ├── session/                      # Session 生命周期 + chat↔workspace 绑定
│   │   ├── manager.go                #   SessionManager（registry）
│   │   ├── session.go                #   Session 数据结构
│   │   └── router.go                 #   chat_id → session 路由
│   ├── pty/                          # PTY bridge 封装
│   │   ├── bridge.go                 #   aymanbagabas/go-pty 封装
│   │   └── ansi.go                   #   ANSI 转义处理
│   ├── registry/                     # 进程注册表
│   │   └── registry.go               #   JSON 持久化
│   └── ipc/                          # 本地 HTTP API（CLI 管理命令用）
│       └── server.go
├── docs/
│   ├── SPEC.md
│   ├── FEATURES.md
│   ├── architecture.md               # 本文件
│   └── cli-bridge.md
├── go.mod
├── go.sum
├── README.md
└── configs/
    └── nightme.example.yaml
```

---

## 2. 核心数据流

### 2.1 用户发送消息（Channel → PTY）

```
飞书消息事件
  ↓ lark-oapi websocket
[Channel adapter: feishu]
  ↓ (chat_id, text)
[Router.Lookup(chat_id)] → session_id (或 "new" 触发器)
  ↓
[SessionManager.Get(session_id)] → Session
  ↓
[Bridge.Write(text)] → PTY stdin → Claude Code
```

### 2.2 CLI 输出（PTY → Channel）

```
Claude Code stdout/stderr
  ↓
[Bridge.Read()] (字节流)
  ↓ ANSI 处理（详见 cli-bridge.md）
[Session.OutputStream] (chan []byte)
  ↓
[Channel adapter: feishu.SendMessage(chat_id, text)]
  ↓
用户飞书收到消息
```

### 2.3 新 Chat 触发 Session 创建

```
用户在 DM 首条消息: "workspace: /home/devin/code/bailing"
  ↓
[Router.Lookup] → 命中 "new"（该 chat_id 没有 session）
  ↓
[SessionManager.Create(chat_id, workspace, agent)]
  - 验证 workspace 路径存在
  - 选择 agent（默认 claude）
  - spawn PTY: exec.CommandContext("claude")，cmd.Dir = workspace
  - 注册 pid → session_id 到 registry
  - 写 chat_id → session_id 到 session map（JSON）
  ↓
[Bridge.Read goroutine] 启动（持续推 PTY 输出到该 chat）
  ↓
[Channel.SendMessage(chat_id, "Session started in {workspace}")]
```

---

## 3. 接口契约

### 3.1 `Channel` interface

```go
// internal/channel/channel.go
package channel

type Message struct {
    ChatID   string    // IM 端唯一标识（飞书 open_chat_id）
    Text     string
    SenderID string    // 用户 open_id（v0.2 用于多用户扩展）
    Time     time.Time
}

type Channel interface {
    // Start 启动长连接（飞书 WebSocket），收到消息推送到 Incoming
    Start(ctx context.Context) error

    // Stop 优雅停止
    Stop(ctx context.Context) error

    // SendMessage 发送文本消息到指定 chat
    SendMessage(ctx context.Context, chatID string, text string) error

    // SendLongMessage 自动分段（飞书单条 4KB 限制）
    SendLongMessage(ctx context.Context, chatID string, text string) error

    // Incoming 用户消息通道（Channel adapter → Router）
    Incoming() <-chan Message
}
```

### 3.2 `Agent` interface

```go
// internal/agent/agent.go
package agent

type Agent interface {
    // Name 唯一标识（"claude" / "codex" / "opencode"）
    Name() string

    // Command 返回要 spawn 的可执行文件路径
    Command() string

    // Args 返回额外参数（v0.1 留空，靠 CLI 自己启动）
    Args() []string

    // Env 返回额外环境变量（如 ANTHROPIC_API_KEY，未来 F-18 用）
    Env() []string
}
```

MVP 实现：
- `claude.Agent{Command: "claude"}`（假设 `claude` 在 PATH）
- 通过 `internal/agent/registry.go` 注册

### 3.3 `Session` 数据结构

```go
// internal/session/session.go
type Session struct {
    ID        string            // uuid，session 创建时生成
    ChatID    string            // IM chat_id（飞书 open_chat_id）
    Workspace string            // 绝对路径
    Agent     string            // agent.Name()
    PID       int               // Claude Code 进程 pid
    StartedAt time.Time
    LastInput time.Time         // 用于 idle 检测（v0.2）

    bridge    pty.Bridge        // PTY bridge 句柄（非导出）
    cancel    context.CancelFunc
}
```

### 3.4 `Bridge` 接口

```go
// internal/pty/bridge.go
type Bridge interface {
    // Read 读 PTY 输出（阻塞直到有数据或 PTY 关闭）
    Read(p []byte) (n int, err error)

    // Write 写 PTY 输入
    Write(p []byte) (n int, err error)

    // Setsize 调整 PTY 大小（v0.2）
    Setsize(cols, rows int) error

    // PID 子进程 pid
    PID() int

    // Close 关闭 PTY + 杀子进程
    Close() error
}
```

---

## 4. Session 生命周期

### 4.1 状态机

```
                    workspace: <path>
    [无 session] ────────────────────────► [pending] (验证 workspace)
                                              │
                                              │  workspace 存在 + agent 可执行
                                              ▼
                                          [running] (PTY alive)
                                              │
                                              │  CLI exit / PTY 关闭 / 用户 kill
                                              ▼
                                          [exited] (保留在 registry)
```

### 4.2 关键事件处理

| 事件 | 处理 |
|------|------|
| CLI 正常 exit (code 0) | session 进入 `exited`，通知用户 "session ended" |
| CLI 异常 exit (code != 0) | 同上，附加 exit code |
| PTY read 返回 EOF | session 进入 `exited` |
| 用户发送 `/kill` (v0.2) | bridge.Close() → SIGTERM → 等 5s → SIGKILL |
| nightme SIGTERM | 默认不 kill session CLI（保留后台跑） |
| nightme 重启 | 读 session map，重新 attach 到已有 PTY（如进程还活着） |

### 4.3 进程归属保证

nightme 启动的所有 PTY 子进程，在 spawn 时记录到 `registry/sessions.json`：

```json
{
  "version": 1,
  "sessions": {
    "s_01HF8...": {
      "chat_id": "oc_xxxxx",
      "workspace": "/home/devin/code/bailing",
      "agent": "claude",
      "pid": 12345,
      "ppid": 6789,
      "started_at": "2026-07-31T10:30:00+08:00"
    }
  }
}
```

**写入时机**：`session.create` 后立即 fsync。
**清理时机**：nightme 退出 + `--cleanup` 标志位（默认关闭）。

---

## 5. 并发模型

```
┌─────────────────────────────────────────────────────────┐
│                    main goroutine                        │
│  - signal handling (SIGTERM/SIGINT)                      │
│  - 启动 Channel.Start()                                  │
│  - 启动 SessionManager                                   │
│  - 启动 IPC server                                       │
└─────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────┐
│  per-session goroutines (fan-out)                        │
│                                                          │
│  per session:                                            │
│    readPump:   bridge.Read() → outputStream chan         │
│    writePump:  inputStream chan → bridge.Write()         │
│    watch:      <-session.done → cleanup()                │
└─────────────────────────────────────────────────────────┘

Channel goroutines:
  feishu.websocketLoop  → chan Message → router
  feishu.sendLoop       ← chan sendReq
```

**同步原语**：
- `SessionManager.sessions`: `sync.RWMutex` 保护的 `map[string]*Session`
- 每个 Session 内部：`chan []byte` 用于 input/output stream，buffered 64KB
- 无跨 session 共享状态 → 不需要全局锁

**为什么不用 errgroup / singleflight**：每个 session 独立生命周期，没有跨 session 的原子操作。

---

## 6. 进程注册与归属（详细）

### 6.1 注册时机

```go
func (m *Manager) Create(ctx context.Context, chatID, workspace, agentName string) (*Session, error) {
    // 1. 验证
    if _, err := os.Stat(workspace); err != nil { return nil, ErrWorkspaceNotExist }

    // 2. spawn PTY
    bridge, err := pty.New(workspace, agent.Command(), agent.Args())
    if err != nil { return nil, err }
    pid := bridge.PID()

    // 3. 注册（写入磁盘）
    s := &Session{ID: uuid(), ChatID: chatID, Workspace: workspace, PID: pid, ...}
    m.registry.Upsert(s)  // fsync

    // 4. 启动 goroutines
    go s.readPump(ctx)
    go s.writePump(ctx)

    return s, nil
}
```

### 6.2 归属判定

nightme 启动的 PTY 子进程**一定**满足：
- `pid > 0` 且已写入 registry
- `ppid == os.Getpid()`（双保险，防止有人手工 attach 假进程）

启动后立即 `cmd.Start()` 同步返回，记录 `cmd.Process.Pid()` → 写 registry。如果中途 PTY 死了，从 registry 删除记录（不保留 zombie）。

### 6.3 清理策略

| 触发 | 行为 |
|------|------|
| nightme SIGTERM | 默认：**不 kill** 子进程。session 标记 "detached"，用户下次启动 nightme 时自动 reattach（如 PID 还活着） |
| nightme SIGTERM + `--cleanup` | kill 所有 session CLI（SIGTERM → 5s → SIGKILL） |
| nightme crash（无优雅退出） | 子进程变孤儿，依赖 OS 进程组清理（macOS/Linux 默认） |

**默认 "不 kill" 是有意设计**：用户手机断网、nightme 重启，CLI 进程继续工作；用户回来能继续对话。

---

## 7. 配置

`~/.config/nightme/config.yaml`：

```yaml
# nightme 配置
feishu:
  app_id: "cli_xxxx"
  app_secret: "secret_xxxx"
  verification_token: "tok_xxxx"   # 可选
  encrypt_key: "enc_xxxx"          # 可选
  # API endpoints 默认走 lark-oapi，无需配置

agent:
  claude:
    command: "claude"              # 或 "/usr/local/bin/claude"
    # args: []                    # v0.1 留空
    # env:                        # v0.1 不需要
    #   ANTHROPIC_API_KEY: "..."

session:
  default_pty_cols: 120
  default_pty_rows: 40
  output_chunk_size: 4096          # 飞书单条上限 4KB
  output_flush_interval_ms: 200    # 聚合窗口，减少消息条数

logging:
  level: "info"                    # debug / info / warn / error
  file: "~/.local/share/nightme/nightme.log"

paths:
  data_dir: "~/.local/share/nightme"
  registry_file: "~/.local/share/nightme/registry.json"
  sessions_file: "~/.local/share/nightme/sessions.json"
```

**环境变量覆盖**：所有配置项支持 `NIGHTME_<SECTION>_<KEY>` 大写覆写（如 `NIGHTME_FEISHU_APP_ID`）。

---

## 8. IPC（本地管理）

`nightme list` / `nightme kill <sid>` / `nightme attach <sid>`（debug 用）等命令通过 **本地 HTTP** 跟主进程通信：

```
$ nightme list
curl http://127.0.0.1:7823/v1/sessions
```

监听 `127.0.0.1:7823`，仅本机访问，无鉴权（MVP 假设单用户）。如果担心暴露，可在 v0.2 加 Unix socket。

---

## 9. 失败模式 & 错误处理

| 场景 | 行为 |
|------|------|
| workspace 路径不存在 | 拒绝创建 session，提示用户 "workspace does not exist" |
| `claude` 不在 PATH | 创建失败，提示 "claude binary not found, please install" |
| PTY 启动后立刻 exit | 注册后立刻取消，registry 删除记录，通知用户 |
| 飞书 WebSocket 断连 | SDK 自动重连（指数退避），nightme 不需特殊处理 |
| 飞书发消息频率超限 | 单聊 5 QPS，群聊 1 QPS，超限走 Channel adapter 的内部队列 |
| 用户发消息但 chat 无 session | 提示 "please start with 'workspace: <path>'" |
| Session CLI 卡死 | v0.1 不处理；v0.2 加 idle timeout（默认 30min） |
| nightme 内存爆 | 依赖 Go GC；极端情况 OOM kill，等 v0.2 加 resource limit |

---

## 10. 文件权限与安全

- **config + log + registry**：`chmod 600`（含 app_secret）
- **PTY 子进程**：默认 inherit 父进程环境变量，**不** 注入任何额外 token
- **网络出站**：仅连飞书 WebSocket + 长连接 API endpoint，无其他出口
- **本地 IPC**：`127.0.0.1` only，**不**监听 `0.0.0.0`
- **日志脱敏**：app_secret / API key 一律 redact（`***`）

---

## 11. 测试策略

| 层 | 测试方式 |
|----|----------|
| Channel interface | mock 实现，单测 Router 行为 |
| PTY bridge | 集成测试：spawn `/bin/echo hello`，验证 Read 拿到 "hello\n" |
| Session lifecycle | table-driven：Create / Send / Exit / Cleanup |
| Process registry | tmpdir 下跑，读 JSON 验证 schema |
| 飞书 adapter | mock 飞书 SDK（接口化后），不依赖真实 app_id |
| E2E | 手动：飞书消息 → nightme → 本地 claude → 飞书回包 |

**E2E 自动化 v0.1 不做**：依赖真实飞书 app + claude CLI，环境复杂。手测 + 单元 + 集成测试覆盖 90%。

---

## 12. 与现有项目的关系

| 现有项目 | 关系 |
|----------|------|
| pangolin (~/code/pangolin) | **不引用**，独立项目，但 channel 抽象思路可借鉴（pangolin 也是多 backend 路由） |
| OpenClaw | **不引用**，但是 PR/issue 流程可以用 OpenClaw 的 gtw plugin |
| chrome-use | 无关系 |
| gfwproxy | 无关系 |

nightme 是**全新独立项目**，不依赖任何现有代码。

---

## 13. 下一步

1. ✅ 本 architecture.md 完成
2. ⏭ 写 `docs/cli-bridge.md`（byte pipe 协议 + ANSI 处理）
3. ⏭ 出 **Implementation Brief**（milestone 拆分 + 第一个 PR 的 commit 计划）
