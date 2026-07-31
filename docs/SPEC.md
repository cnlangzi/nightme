# nightme — Technical Specification (SPEC)

> **状态**：v1.0（已锁定）
> **作者**：🦞 虾哥（PM/Architect）
> **日期**：2026-07-31
> **来源**：与 Devin 在飞书 DM 的需求澄清对话
> **更新日志**：
> - v1.0 — 锁定 6 项关键决策（Q1-Q6），明确 Chat↔Session 1:1 绑定模型
> - v1.0r — 重命名 PRD → SPEC，功能列表抽取到 FEATURES.md
> - v1.0s — 合并 architecture.md 入 SPEC.md，cli-bridge.md 转入 feat/，IMPLEMENTATION.md → PLAN.md

---

## 1. 一句话定义

**nightme 是一个运行在用户电脑上的"AI Coding CLI 远程遥控桥"**：在用户的电脑后台跑一个进程，从手机 / IM 端把消息透传到本地 Claude Code / Codex / OpenCode 的 TTY 输入框，同时把这些 CLI 的屏幕输出抓回来推送到 IM。它不调 LLM，不做编排决策，**只是 I/O 的搬运工**。

可以把它想象成：一个"被 IM 控制的 Web TTY"，但这个 TTY 不是通用 shell，只针对 AI Coding Agent。

**Slogan**：`Sleep tight, code all night.`

---

## 2. 核心模型

```
┌─────────────────────────────────────────────────────────────┐
│  Channel (Feishu / WhatsApp / Web UI ...)                   │
│   ↑ reply                                                  │
│   │ user text / file / voice                               │
└────────────────┬────────────────────────────────────────────┘
                 │
                 ▼
┌─────────────────────────────────────────────────────────────┐
│  nightme (single binary on user's laptop)                   │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────┐  │
│  │ Channel      │  │ Session      │  │ PTY Bridge       │  │
│  │ Adapter      │←→│ Manager      │←→│ (per session)    │  │
│  │ (Feishu,...) │  │ (registry)   │  │  - stdin pipe    │  │
│  └──────────────┘  └──────┬───────┘  │  - stdout capture│  │
│                           │          │  - resize signal │  │
│                           ▼          └────────┬─────────┘  │
│                    ┌─────────────────┐        │             │
│                    │ Workspace Map  │        │ spawn       │
│                    │ chat_id → ws   │        ▼             │
│                    └─────────────────┘  ┌──────────────────┐│
│                                      │ Claude Code CLI  ││
│                                      │ Codex CLI        ││
│                                      │ OpenCode CLI     ││
│                                      │ (PTY, cwd=ws)    ││
│                                      └──────────────────┘│
└─────────────────────────────────────────────────────────────┘
```

---

## 3. 关键设计原则

### 3.1 完全透传，不解析
- nightme **不解析** Claude Code 的输出（不识别成功 / 失败 / TODO）。
- nightme **不编排**（不拆分任务、不合并结果、不主动建议）。
- 它就是一个 **byte pipe** + **session registry**。

### 3.2 模拟 TTY，让 CLI 无感
- 用 **PTY**（pseudo-terminal）启动 AI Coding CLI，不是 non-interactive 模式。
- 目的：Claude Code / Codex / OpenCode 内部行为依赖 TTY 检测（颜色、进度条、交互 prompt）。
- 副作用：nightme 必须转发 ANSI 转义码（在 Channel 端做格式化适配）。

### 3.3 进程归属 = nightme 自启动
- nightme **只控制自己启动的进程**。
- 用户的 bash / zsh / vscode / 其他手启动的进程，**完全不管**。
- 必须有 process registry：记录 `pid → session_id → workspace → started_at`。
- 清理规则：nightme 关闭时，可选 kill 自己启动的所有进程（默认不强制，由用户决定）。

### 3.4 Session = 一个 AI Coding CLI 进程 + 一个 Workspace
- 一个 session = 一个 PTY 进程。
- 每个 session **绑定一个 workspace 目录**（绝对路径，CLI 启动时 `cwd=workspace`）。
- 不同 session 可以绑定不同 workspace，但**一个 session 只对应一个 workspace**。
- workspace 之间隔离：session A 看不到 session B 的文件、看不到对方的 PTY。

### 3.5 Chat = Session = Project（核心洞察）
- **IM Chat（DM/group/thread）↔ Session，1:1 绑定**。
- 同一个 Chat 永远只对应一个 session，**不支持 `/attach` 切换**。
- 想换项目？**新开一个 DM**（飞书侧就是"项目列表"）。
- 好处：
  - 飞书的聊天历史 = 项目历史，无需 nightme 持久化
  - 用户切换项目 = 切换 DM，零认知负担
  - 多项目并行 = 多 DM 并行，互不干扰

### 3.6 Minimal 原则
- **第一个版本只做一件事**：从 IM 转发文本到 PTY stdin，再把 PTY 输出回推给 IM。
- 文件、图片、语音、按钮、卡片、threading 全部放后期。
- Channel 第一版只做一个（**优先飞书**，因为是主战场）。

---

## 4. Architecture

### 4.1 模块概览

```
nightme/
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
│   │   └── aggregator.go             #   200ms / 4KB 聚合
│   ├── registry/                     # 进程注册表
│   │   └── registry.go               #   JSON 持久化
│   └── ipc/                          # 本地 HTTP API（CLI 管理命令用）
│       └── server.go
├── docs/
│   ├── SPEC.md                       # 本文件
│   ├── FEATURES.md
│   ├── PLAN.md
│   └── feat/                         # 18 个 F-XX 详细设计
├── go.mod
├── go.sum
├── README.md
└── configs/
    └── nightme.example.yaml
```

### 4.2 核心数据流

**用户发送消息（Channel → PTY）**：
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

**CLI 输出（PTY → Channel）**：
```
Claude Code stdout/stderr
  ↓
[Bridge.Read()] (字节流)
  ↓ ANSI 处理（详见 feat/F-19-cli-bridge.md）
[Aggregator] (200ms / 4KB)
  ↓
[Channel adapter: feishu.SendLongMessage(chat_id, text)]
  ↓
用户飞书收到消息
```

**新 Chat 触发 Session 创建**：
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

### 4.3 接口契约

**`Channel` interface** (`internal/channel/channel.go`)：
```go
type Message struct {
    ChatID   string
    Text     string
    SenderID string
    Time     time.Time
}

type Channel interface {
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
    SendMessage(ctx context.Context, chatID string, text string) error
    SendLongMessage(ctx context.Context, chatID string, text string) error
    Incoming() <-chan Message
}
```

**`Agent` interface** (`internal/agent/agent.go`)：
```go
type Agent interface {
    Name() string
    Command() string
    Args() []string
    Env() []string
    Detect() error
}
```

**`Session` 数据结构** (`internal/session/session.go`)：
```go
type Session struct {
    ID        string            // uuid
    ChatID    string            // IM chat_id
    Workspace string            // 绝对路径
    Agent     string            // agent.Name()
    PID       int               // Claude Code 进程 pid
    StartedAt time.Time
    LastInput time.Time         // 用于 idle 检测（v0.2）

    bridge    pty.Bridge        // PTY bridge 句柄
    cancel    context.CancelFunc
}
```

**`Bridge` interface** (`internal/pty/bridge.go`)：
```go
type Bridge interface {
    Read(p []byte) (n int, err error)
    Write(p []byte) (n int, err error)
    Setsize(cols, rows int) error
    PID() int
    Close() error
}
```

详细设计见各 `feat/F-XX-*.md`。

### 4.4 Session 生命周期

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

**关键事件处理**：

| 事件 | 处理 |
|------|------|
| CLI 正常 exit (code 0) | session 进入 `exited`，通知用户 "session ended" |
| CLI 异常 exit (code != 0) | 同上，附加 exit code |
| PTY read 返回 EOF | session 进入 `exited` |
| 用户发送 `/kill` (v0.2) | bridge.Close() → SIGTERM → 等 5s → SIGKILL |
| nightme SIGTERM | 默认不 kill session CLI（保留后台跑） |
| nightme 重启 | 读 session map，重新 attach 到已有 PTY（如进程还活着） |

详细策略见 [`feat/F-06-process-cleanup.md`](./feat/F-06-process-cleanup.md)。

### 4.5 并发模型

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
│    readPump:   bridge.Read() → aggregator → outputStream │
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

### 4.6 进程注册与归属

**注册文件**：`~/.local/share/nightme/registry.json`（0600 权限）

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

**归属判定**：
- nightme 启动的 PTY 子进程**一定**满足：
  - `pid > 0` 且已写入 registry
  - `ppid == os.Getpid()`（双保险）
- 启动后立即 `cmd.Start()` 同步返回，记录 PID → fsync 写 registry
- 中途 PTY 死亡 → registry 删除记录（不保留 zombie）

**清理策略**：

| 触发 | 行为 |
|------|------|
| nightme SIGTERM | 默认：**不 kill**，session 标记 "detached" |
| nightme SIGTERM + `--cleanup` | kill 所有 session CLI（SIGTERM → 5s → SIGKILL） |
| nightme crash | 子进程变孤儿，依赖 OS 进程组清理 |

**默认 "不 kill" 是有意设计**：用户手机断网、nightme 重启，CLI 进程继续工作。

详细 schema 见 [`feat/F-05-process-registry.md`](./feat/F-05-process-registry.md)。

### 4.7 配置

`~/.config/nightme/config.yaml`：

```yaml
feishu:
  app_id: "cli_xxxx"
  app_secret: "***"
  verification_token: "tok_xxxx"   # 可选
  encrypt_key: "enc_xxxx"          # 可选

agent:
  default: "claude"
  claude:
    command: "claude"
    # args: []
    # env: {}

session:
  default_pty_cols: 120
  default_pty_rows: 40
  output_chunk_size: 4096
  output_flush_interval_ms: 200

logging:
  level: "info"
  file: "~/.local/share/nightme/nightme.log"

paths:
  data_dir: "~/.local/share/nightme"
  registry_file: "~/.local/share/nightme/registry.json"
  sessions_file: "~/.local/share/nightme/sessions.json"
```

**环境变量覆盖**：所有配置项支持 `NIGHTME_<SECTION>_<KEY>` 大写覆写。

### 4.8 IPC（本地管理）

`nightme list` / `nightme kill <sid>` 等命令通过 **本地 HTTP** 跟主进程通信：

```
$ nightme list
curl http://127.0.0.1:7823/v1/sessions
```

监听 `127.0.0.1:7823`，仅本机访问，无鉴权（MVP 单用户）。详细见 [`feat/F-10-session-list-cmd.md`](./feat/F-10-session-list-cmd.md)。

### 4.9 失败模式 & 错误处理

| 场景 | 行为 |
|------|------|
| workspace 路径不存在 | 拒绝创建 session，提示用户 |
| `claude` 不在 PATH | 创建失败，提示 "claude binary not found" |
| PTY 启动后立刻 exit | 注册后立刻取消，registry 删除记录 |
| 飞书 WebSocket 断连 | SDK 自动重连（指数退避） |
| 飞书发消息频率超限 | Channel adapter 内部 token bucket 限速 |
| 用户发消息但 chat 无 session | 提示 "please start with 'workspace: <path>'" |
| Session CLI 卡死 | v0.1 不处理；v0.2 加 idle timeout |
| nightme 内存爆 | 依赖 Go GC；v0.2 加 resource limit |

### 4.10 文件权限与安全

- **config + log + registry**：`chmod 600`
- **PTY 子进程**：默认 inherit 父进程环境变量，**不** 注入额外 token
- **网络出站**：仅连飞书 WebSocket + 长连接 API endpoint
- **本地 IPC**：`127.0.0.1` only，**不**监听 `0.0.0.0`
- **日志脱敏**：app_secret / API key 一律 redact

### 4.11 测试策略

| 层 | 测试方式 |
|----|----------|
| Channel interface | mock 实现，单测 Router 行为 |
| PTY bridge | 集成测试：spawn `/bin/echo hello` 验证 Read |
| Session lifecycle | table-driven：Create / Send / Exit / Cleanup |
| Process registry | tmpdir 下跑，读 JSON 验证 schema |
| 飞书 adapter | mock 飞书 SDK（接口化后），不依赖真实 app_id |
| E2E | 手动：飞书消息 → nightme → 本地 claude → 飞书回包 |

**E2E 自动化 v0.1 不做**：依赖真实飞书 app + claude CLI。

### 4.12 与现有项目的关系

| 现有项目 | 关系 |
|----------|------|
| pangolin (~/code/pangolin) | **不引用**，独立项目 |
| OpenClaw | **不引用**，PR/issue 流程可以用 gtw plugin |
| chrome-use | 无关系 |
| gfwproxy | 无关系 |

nightme 是**全新独立项目**，不依赖任何现有代码。

---

## 5. 功能需求

完整功能列表（F-1 ~ F-19）见 [`FEATURES.md`](./FEATURES.md)。

详细设计见 [`feat/`](./feat/) 目录下的 19 份独立设计文档。

---

## 6. 非功能需求 (NFR)

| ID | 指标 |
|----|------|
| N-1 | **延迟**：用户发消息 → CLI 收到输入 < 200ms |
| N-2 | **吞吐**：CLI 输出回推到 Channel，端到端延迟 < 1s |
| N-3 | **资源占用**：单个 session PTY 空闲时 CPU ≈ 0；内存 ≈ 5-10MB |
| N-4 | **崩溃隔离**：单个 session PTY 死亡不影响其他 session |
| N-5 | **可观测**：每个 session 有结构化日志 |
| N-6 | **可移植**：单二进制，macOS / Linux 双平台 |

---

## 7. 技术栈（已锁定）

| 层 | 选型 | 备选 | 理由 |
|----|------|------|------|
| 主语言 | **Go 1.22+** | Rust / Node.js | 单二进制、跨平台编译简单 |
| PTY | **`github.com/aymanbagabas/go-pty`** | creack/pty | API 干净，跨平台抽象好 |
| Channel | **飞书官方 Go SDK**（lark-oapi）| 自实现 webhook | 文档全，长连接稳定 |
| HTTP API | **net/http + chi** | gin | minimal |
| 持久化 | **JSON 文件** | SQLite | MVP 不需要 DB |
| 配置 | **YAML** | env | 直观 |
| 日志 | **`log/slog`**（标准库）| zap / zerolog | stdlib 够用 |

---

## 8. 已锁定的关键决策

| # | 决策 | 结论 |
|---|------|------|
| **Q1** | 技术栈 | **Go 1.22+** |
| **Q2** | MVP Channel 范围 | **只飞书**，通过 `Channel` interface 抽象 |
| **Q3** | 第一版 Agent | **只 Claude Code**，通过 `Agent` interface 抽象 |
| **Q4** | Session 路由模型 | **Chat ↔ Session 1:1** |
| **Q5** | CLI 进程 spawn 方式 | **自己 PTY**（aymanbagabas/go-pty）|
| **Q6** | 鉴权 | **单用户独占假设**，不需要设备配对 |

---

## 9. 范围外（明确不做）

- ❌ **不做 LLM 编排**
- ❌ **不做 code review / agent quality scoring**
- ❌ **不接管用户已有的 shell / terminal multiplexer**
- ❌ **不做 multi-user RBAC**
- ❌ **不做云端 SaaS**
- ❌ **不写底层 AI Coding Agent 的 prompt / system message**

---

## 10. 下一步

SPEC 已锁定。下一步：

1. ✅ 本 SPEC 冻结（含 architecture）
2. ⏭ 按 [`PLAN.md`](./PLAN.md) 实施：M1 → M2 → M3
3. ⏭ 每个 F-XX 详细设计按需迭代
