# nightme — Product Requirements Document (PRD)

> **状态**：v1.0（已锁定）
> **作者**：🦞 虾哥（PM/Architect）
> **日期**：2026-07-31
> **来源**：与 Devin 在飞书 DM 的需求澄清对话
> **更新日志**：
> - v1.0 — 锁定 6 项关键决策（Q1-Q6），明确 Chat↔Session 1:1 绑定模型

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

## 4. 功能需求 (Functional Requirements)

### 4.1 必需（MVP）

| ID | 描述 |
|----|------|
| F-1 | **Session 创建**：用户在 Chat 首条消息写 `workspace: /path/to/project`，nightme 验证后创建 session 并 spawn CLI |
| F-2 | **消息透传**：用户在 Chat 里的输入 → 该 Chat 绑定 session 的 PTY stdin |
| F-3 | **输出推送**：session 的 PTY stdout/stderr → 该 Chat |
| F-4 | **PTY 模拟**：spawn CLI 时使用 PTY，cols/lines 由 nightme 配置（默认 120x40） |
| F-5 | **进程注册**：nightme 启动的所有 CLI 进程写入本地 registry 文件（pid, sid, agent, ws, started_at） |
| F-6 | **进程清理**：nightme 退出时（SIGTERM/SIGINT），按策略清理自己启动的进程（默认：保留，让用户手动 kill） |
| F-7 | **Workspace 绑定**：CLI 启动时 `cwd = session.workspace`；不存在则失败，提示用户 |
| F-8 | **Channel 抽象**：Channel 是 interface，MVP 实现 Feishu，结构上留位给其他 IM |
| F-9 | **Agent 抽象**：Agent 是 interface，MVP 实现 Claude Code，结构上留位给 Codex/OpenCode |
| F-10 | **Session 列表**（管理命令）：用户用 `nightme list` 命令行查所有 session 状态 |

> **删除项**：原 F-2 (Session 列表在 Channel 内查询)、F-3 (Session 切换) 因 Chat↔Session 1:1 模型被删除。

### 4.2 后续（v0.2+）

| ID | 描述 |
|----|------|
| F-11 | 多个 Channel 同时 attach 到一个 session（mirror 模式） |
| F-12 | WhatsApp / Telegram / Slack / Web UI Channel adapter |
| F-13 | 终端大小调整（用户手机横竖屏切换 → PTY SIGWINCH） |
| F-14 | 图片 / 文件附件透传 |
| F-15 | Session 持久化（nightme 重启后，已结束的 session 可恢复 stdout 历史） |
| F-16 | Web TTY UI（浏览器实时看 + 操作） |
| F-17 | 健康检查 / 心跳（session 失联自动告警） |
| F-18 | Token / API key 注入（避免在 channel 里裸露） |

---

## 5. 非功能需求 (NFR)

| ID | 指标 |
|----|------|
| N-1 | **延迟**：用户发消息 → CLI 收到输入 < 200ms |
| N-2 | **吞吐**：CLI 输出回推到 Channel，端到端延迟 < 1s（普通文本块） |
| N-3 | **资源占用**：单个 session PTY 空闲时 CPU ≈ 0；内存 ≈ 5-10MB |
| N-4 | **崩溃隔离**：单个 session PTY 死亡不影响其他 session，也不影响 nightme 主进程 |
| N-5 | **可观测**：每个 session 有结构化日志（sid / agent / ws / 输入 / 输出大小 / 错误） |
| N-6 | **可移植**：单二进制，macOS / Linux 双平台，**不依赖** systemd / launchd |

---

## 6. 技术栈（已锁定）

| 层 | 选型 | 备选 | 理由 |
|----|------|------|------|
| 主语言 | **Go 1.22+** | Rust / Node.js | 单二进制、creack/pty 成熟、跨平台编译简单 |
| PTY | **`github.com/aymanbagabas/go-pty`** | creack/pty | API 干净，跨平台抽象好，charm 生态背书 |
| Channel | **飞书官方 Go SDK**（lark-oapi）| 自实现 webhook | 文档全，长连接稳定 |
| HTTP API | **net/http + chi** | gin | minimal |
| 持久化 | **JSON 文件**（registry + session map）| SQLite | MVP 不需要 DB |
| 配置 | **YAML** | env | 直观 |
| 日志 | **`log/slog`**（标准库）| zap / zerolog | stdlib 够用，避免多余依赖 |

**为什么不选 Node.js**：node-pty 在 macOS 上偶发崩溃 + native rebuild 麻烦。
**为什么不选 Rust**：std::process + portable-pty 写起来 boilerplate 多，minimal 原则下不值。
**为什么不选 creack/pty**：aymanbagabas API 更现代、resize 简单、跨平台边界更清晰。

---

## 7. 已锁定的关键决策

| # | 决策 | 结论 |
|---|------|------|
| **Q1** | 技术栈 | **Go 1.22+** |
| **Q2** | MVP Channel 范围 | **只飞书**，通过 `Channel` interface 抽象，预留扩展位 |
| **Q3** | 第一版 Agent | **只 Claude Code**，通过 `Agent` interface 抽象 |
| **Q4** | Session 路由模型 | **Chat ↔ Session 1:1**（A 方案）。每个 IM Chat 锁一个项目，不支持 `/attach` 切换；切项目 = 新开 DM |
| **Q5** | CLI 进程 spawn 方式 | **自己 PTY**，用 `aymanbagabas/go-pty`，不依赖 tmux/zellij |
| **Q6** | 鉴权 | **单用户独占假设**，不需要设备配对。飞书 appSecret 即唯一凭证 |

**Q4 关键论证**：Devin 原话："如果在一个 Session 一直切换的话，我们会搞不清楚他的上下文"。Chat↔Session 1:1 把"项目上下文"固化到 IM chat 本身，飞书侧 DM 列表天然成为项目列表，零认知负担。

---

## 8. 范围外（明确不做）

- ❌ **不做 LLM 编排**：不调任何 LLM，不做 task decomposition
- ❌ **不做 code review / agent quality scoring**
- ❌ **不接管用户已有的 shell / terminal multiplexer**：用户可以同时开着自己的 tmux，nightme 不动它
- ❌ **不做 multi-user RBAC**：单用户场景
- ❌ **不做云端 SaaS**：nightme 始终跑在用户电脑上，**没有云端组件**
- ❌ **不写底层 AI Coding Agent 的 prompt / system message**

---

## 9. 下一步

PRD 已锁定。下一步：

1. ✅ 本 PRD 冻结
2. ⏭ 写 `docs/architecture.md`（PTY 桥、Session lifecycle、process registry schema、Channel/Agent interface）
3. ⏭ 写 `docs/cli-bridge.md`（byte pipe 协议 + ANSI 处理策略 + 重连/崩溃恢复）
4. ⏭ 出 **Implementation Brief**（milestone 拆分 + 第一个 PR 的 commit 计划）
5. ⏭ 动代码（先 docs 后 code 原则）

预期 Implementation Brief 完成时间：本轮对话内。
