# nightme — Technical Specification (SPEC)

> **状态**：v1.0（已锁定）
> **作者**：🦞 虾哥（PM/Architect）
> **日期**：2026-07-31
> **文档层级**：技术级（**不含实现细节 / 代码**）
> **关联文档**：
> - 产品定位 → [`PRD.md`](./PRD.md)
> - 功能索引 → [`FEATURES.md`](./FEATURES.md)
> - 每个 feature 的实现细节（含代码）→ [`feat/`](./feat/)
> - 实施计划 → [`PLAN.md`](./PLAN.md)

---

## 1. 架构总览

nightme 是一个**单进程 daemon**，运行在用户的电脑上。它由以下五个**逻辑组件**组成：

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
│                                                              │
│  ┌────────────┐  ┌──────────┐  ┌────────────────┐  ┌───────┐ │
│  │ Channel    │→ │ Gateway  │→ │ Session Manager│  │Bridge │ │
│  │ Adapter    │  │ (slash   │←→│ (+ Workspace:  │←→│ (ACP  │ │
│  │            │  │  cmd)    │  │  session.ws)   │  │ /SDK  │ │
│  └────────────┘  └──────────┘  └────────┬───────┘  │ /PTY) │ │
│                                          │              │     │
│                                          ▼              ▼     │
│                                   (session 状态)   Claude / │
│                                                  Codex /    │
│                                                  OpenCode   │
└─────────────────────────────────────────────────────────────┘
```

### 1.1 五个逻辑组件

| 组件 | 职责 |
|------|------|
| **Channel Adapter** | 把 IM 协议（飞书 WebSocket / WhatsApp webhook 等）抽象成统一接口；把 IM 消息收上来、把 nightme 输出推回去。飞书凭证获取走 [F-22 QR 扫码授权](./feat/F-22-feishu-onclick-registration.md) |
| **Gateway** | Slash command 路由器：判断每条消息是系统命令还是普通文本；系统命令命中表后执行 / 不命中透传给 SessionManager，普通文本也透传给 SessionManager |
| **Session Manager** | 维护 chat_id ↔ session 的绑定；管理 session 的创建、查询、销毁；**每个 session 绑定一个 workspace**（session.Workspace 字段，session 创建时确定，生命周期内不变） |
| **Bridge** | nightme 与底层 AI Coding CLI 之间的通信抽象；提供统一的 `AgentSession` 接口（Events / SendText / SendPermission / Close）；**有三种实现模式**：ACP（标准化，优先）、SDK（vendor-specific，如 Claude Code Agent SDK）、PTY（透明透传，兑底）。Session Manager 只跟 Bridge 接口交互，不关心具体模式。 |
| **Process Registry** | 记录 nightme 启动的所有进程（pid + chat_id + workspace + 启动时间）；用于查询、重启恢复、清理 |

> **Workspace 不再是独立组件**：原"Workspace Mapper"是 Session Manager 的子功能——每个 session 自带 workspace 字段，查找 chat_id 对应 workspace = `session.Workspace`，无需独立映射表。

> **Gateway 与 Channel Adapter 的关系**：Channel Adapter 只负责 IM 协议编解码；Gateway 负责"这是命令还是文本"的语义判断。两者职责单一不重叠。

> **实现细节**：这些组件的 Go 接口、struct、文件路径在 [`feat/`](./feat/) 各自的 feature doc 里。

---

## 2. 数据流（概念）

### 2.1 用户消息 → CLI 输入

```
IM 消息事件
  → Channel Adapter 解码为统一 Message{chat_id, text}
  → Gateway 判断 text 是否以 / 开头
      ├─ 否（普通文本）→ 透传给 Session Manager
      │       → Router 查 chat_id → session
      │       → Session 把 text 写入 PTY stdin
      │       → CLI 收到输入
      └─ 是 slash command → 查 nightme 命令表
              ├─ 命中（/cwd /kill /help）→ nightme 执行
              │       ├─ 成功 → 回复用户
              │       └─ 参数错 → 回复 usage error（属 nightme namespace）
              └─ 未命中（如 /clear /compact /init）→ 透传
                      → Session Manager 写入 PTY stdin
                      → CLI / Agent 自己处理这个命令
```

**核心规则**：nightme 只拦截 session 管理类命令（/cwd /kill /help）。其他 slash 命令属于 agent 的 namespace，nightme **透传**而不是拒绝——避免破坏 agent 自身的 slash command UX。

### 2.2 CLI 输出 → 用户

```
CLI stdout/stderr
  → Bridge 读取事件流（ACP/SDK 模式）或字节流（PTY 模式）
  → Aggregator 按窗口聚合（200ms / 4KB）
  → Channel Adapter 推送（>4KB 自动分段）
  → IM 用户收到消息
```

### 2.3 用户用 slash command 创建 Session

```
用户在新 DM 发 "/cwd /path/to/project"
  → Channel Adapter 收到 Message
  → Gateway 识别为 /cwd slash command
  → Gateway 查 commands 表 → 命中
  → cwd handler 验证 path + agent
  → Session Manager 创建 session（chat_id, workspace, agent）
  → Bridge 启动 CLI（cwd = workspace；模式取决于 agent 配置）
  → Process Registry 记录 PID
  → Gateway 回复 "Session started in {workspace}"
```

**为什么用 slash command**：原方案"workspace:" 文字前缀识别不可靠——用户可能真在聊 workspace 这个词。`/` 前缀明确，无歧义，Gateway 路由表清晰。

---

## 3. Session 生命周期

Session 分**两层状态**：
- **Session 层**（持久）：chat_id ↔ workspace，由 /cwd 设置
- **CLI 层**（瞬时）：CLI 进程是否在跑，由 /run 启动、/kill 停止

```
                       /cwd (session 不存在)
    [no session] ────────────────────────────────► [session, no CLI]
                                                           │
                                                           │ /run
                                                           ▼
                           /kill ◄─────────────── [session, CLI running]
                             │                          ▲
                             │                          │ /run (CLI 死了)
                             ▼                          │
                      [session, no CLI] ────── /run ────┘
```

**关键规则**：
- **Workspace 是启动 CLI 的硬性前置条件**：没有 /cwd → 无法 /run
- ** 智能处理**：CLI 没跑 → spawn；CLI 在跑 → reconnect（不重启）
- **Session 永不过期**：workspace 永久绑定 chat；CLI 死后 session 保留

**状态转换触发器**：

| From | 触发 | To |
|------|------|-----|
| (no session) | 用户发送 /cwd | session created, workspace set |
| session, no CLI | 用户发送 /cwd | workspace updated |
| session, CLI running | 用户发送 /cwd | rejected (CLI running) |
| session, no CLI | 用户发送 /run | CLI spawned |
| session, CLI running | 用户发送 /run | reconnect (no-op for CLI) |
| session, CLI running | CLI exit / PTY EOF | session, no CLI |
| session, CLI running | 用户发送 /kill | session, no CLI |
| running | nightme SIGTERM | detached（registry 标记，进程继续） |
| exited | 用户下次创建 session | (走 pending 流程) |

> **生命周期详细策略**（含进程归属、清理、reattach）：见 [`feat/F-06-process-cleanup.md`](./feat/F-06-process-cleanup.md)

---

## 4. 并发模型

nightme 用 Go 的 goroutine 实现并发，结构如下：

- **Main goroutine**：信号处理、组件启动顺序、优雅退出
- **Channel goroutines**：长连接收发 + 发送队列（每个 Channel adapter 一组）
- **Gateway**：在 Channel handler goroutine 内同步执行（命令路由极快，不阻塞 I/O）
- **Per-session goroutines**：每个 session 两个 goroutine
  - `readPump`：PTY → Aggregator → Channel
  - `writePump`：Channel input → PTY stdin

**并发安全**：
- Session 列表用 mutex 保护的 map（极少写、极多读）
- 每个 session 内部用 buffered channel 通信，无跨 session 共享状态
- 无全局锁、无 errgroup、无 singleflight

**Back-pressure**：Channel 发送慢 → aggregator callback 阻塞 → readPump 阻塞 → PTY buffer 满 → CLI 自己阻塞。整个链路自然限速。

> **实现细节**（goroutine 代码、channel buffer 大小）：见 [`feat/F-19-cli-bridge.md`](./feat/F-19-cli-bridge.md)

---

## 5. 技术栈（已锁定）

| 层 | 选型 | 备选 | 理由 |
|----|------|------|------|
| 主语言 | **Go 1.22+** | Rust / Node.js | 单二进制、跨平台编译简单 |
| PTY | **`github.com/aymanbagabas/go-pty`** | creack/pty | API 干净，跨平台抽象好 |
| Channel | **飞书官方 Go SDK**（lark-oapi）| 自实现 webhook | 文档全，长连接稳定 |
| HTTP API | **net/http + chi** | gin | minimal |
| 持久化 | **JSON 文件** | SQLite | MVP 不需要 DB |
| 配置 | **YAML** | env | 直观 |
| 日志 | **`log/slog`**（标准库）| zap / zerolog | stdlib 够用 |

**为什么不选 Node.js**：node-pty 在 macOS 上偶发崩溃 + native rebuild 麻烦。
**为什么不选 Rust**：minimal 原则下 boilerplate 太多。

---

## 6. 配置

nightme 从 `~/.config/nightme/config.yaml` 读取配置。

**配置类别**（无代码，详见 PLAN.md / README）：

| 类别 | 内容 |
|------|------|
| `feishu` | app_id / app_secret / verification_token / encrypt_key |
| `agent` | default + 每个 agent 的 command/args/env |
| `session` | default PTY 大小 + 输出聚合参数 |
| `logging` | level + file path |
| `paths` | data_dir / registry_file / sessions_file |

**环境变量覆盖**：所有配置项支持 `NIGHTME_<SECTION>_<KEY>` 大写覆写。

**配置示例**（实际值与默认值）：见 [`PLAN.md`](./PLAN.md) §附录 / `configs/nightme.example.yaml`。

---

## 7. 非功能需求 (NFR)

| ID | 指标 |
|----|------|
| N-1 | **延迟**：用户发消息 → CLI 收到输入 < 200ms |
| N-2 | **吞吐**：CLI 输出回推到 Channel，端到端延迟 < 1s |
| N-3 | **资源占用**：单个 session PTY 空闲时 CPU ≈ 0；内存 ≈ 5-10MB |
| N-4 | **崩溃隔离**：单个 session PTY 死亡不影响其他 session |
| N-5 | **可观测**：每个 session 有结构化日志 |
| N-6 | **可移植**：单二进制，macOS / Linux 双平台 |
| N-7 | **文件权限**：config / log / registry 全部 0600 |

---

## 8. 安全（高层）

- **Channel 鉴权**：依赖 IM 平台原生鉴权（飞书 appSecret / verification token）
- **单用户假设**：MVP 不做设备配对、多用户隔离
- **Onboarding**：飞书凭证优先通过 [F-22 QR 扫码授权](./feat/F-22-feishu-onclick-registration.md)（OAuth 2.0 Device Authorization Grant）获得，避免手动复制 app_id/app_secret。详见 F-22。
- **进程隔离**：nightme 不接管用户已有进程；只能 spawn 自己创建的进程
- **网络出站**：仅连 IM 平台的长连接 endpoint，无其他出口
- **本地 IPC**：仅 listen `127.0.0.1`，不暴露 `0.0.0.0`
- **日志脱敏**：app_secret / API key 一律 redact
- **密码 / API key 透传**：见 [PRD §4.1](./PRD.md#41-完全透传不解析) — 透传原则优先，不做特殊处理

---

## 9. 已锁定的技术决策

| # | 决策 | 结论 |
|---|------|------|
| **Q1** | 技术栈 | **Go 1.22+** |
| **Q2** | MVP Channel | **只飞书** + Channel interface 抽象 |
| **Q3** | MVP Agent | **只 Claude Code** + Agent interface 抽象 |
| **Q4** | Session 路由 | **Chat ↔ Session 1:1** |
| **Q5** | CLI spawn 方式 | **自己 PTY**（aymanbagabas/go-pty）|
| **Q6** | 鉴权 | **单用户独占假设**，不需要设备配对 |

详细论证见 PRD §4（产品哲学）+ 各 feat 文档。

---

## 10. 与现有项目的关系

| 项目 | 关系 |
|------|------|
| pangolin | 不引用，独立项目 |
| OpenClaw | 不引用，PR/issue 流程可借用 gtw plugin |
| chrome-use / gfwproxy | 无关系 |

nightme 是**全新独立项目**，不依赖任何现有代码。

---

## 11. 下一步

技术规范已锁定。下一步：

1. ✅ SPEC（含 architecture）冻结
2. ⏭ 按 [`PLAN.md`](./PLAN.md) 实施：M1 → M2 → M3
3. ⏭ 每个 F-XX 详细设计按需迭代

---

## 12. 文档层级

```
PRD.md       ← 产品（什么 / 为什么 / 给谁 / 边界）
   ↓
SPEC.md      ← 技术架构（怎么构成 / 数据怎么流 / 技术栈）
   ↓
FEATURES.md  ← 功能索引（哪些 feature 需要实现）
   ↓
feat/F-XX    ← 每个 feature 的详细实现（含代码、接口、schema）
   ↓
PLAN.md      ← 实施计划（按什么顺序实现 / 怎么拆 commit）
```
