# nightme — Feature Index

> **状态**：v1.0
> **作者**：🦞 虾哥（PM/Architect）
> **日期**：2026-07-31
> **关联文档**：
> - 产品定位 → [`PRD.md`](./PRD.md)
> - 技术架构 → [`SPEC.md`](./SPEC.md)
> - 实施计划 → [`PLAN.md`](./PLAN.md)
> - 每个 feature 的详细设计 → [`feat/`](./feat/)

本文档是 nightme 的**功能索引**。每项功能的设计细节（接口、实现、edge cases、测试计划）见 [`feat/`](./feat/) 目录下的独立文档。

---

## 1. MVP 功能（v0.1）

| ID | 功能 | 设计文档 | 里程碑 |
|----|------|----------|--------|
| F-01 | Session 生命周期（`/cwd` + `/run` 两步式；workspace 持久，CLI 可重启）| [feat/F-01-session-create.md](./feat/F-01-session-create.md) | M2 |
| F-02 | 消息透传（Channel → AgentSession via Gateway）| [feat/F-02-message-passthrough.md](./feat/F-02-message-passthrough.md) | M2 |
| F-03 | 输出推送（AgentSession Events → Channel）| [feat/F-03-output-push.md](./feat/F-03-output-push.md) | M2 |
| F-04 | PTY 模拟（aymanbagabas/go-pty）| [feat/F-04-pty-simulation.md](./feat/F-04-pty-simulation.md) | M1 |
| F-05 | 进程注册（JSON registry）| [feat/F-05-process-registry.md](./feat/F-05-process-registry.md) | M1 |
| F-06 | 进程清理（默认 detach，--cleanup kill）| [feat/F-06-process-cleanup.md](./feat/F-06-process-cleanup.md) | M3 |
| F-07 | Workspace 绑定（cwd 校验 + 路径展开）| [feat/F-07-workspace-binding.md](./feat/F-07-workspace-binding.md) | M1 |
| F-08 | Channel 抽象（interface + 飞书实现）| [feat/F-08-channel-abstraction.md](./feat/F-08-channel-abstraction.md) | M2 |
| F-09 | Agent 抽象（interface + mode 选择）| [feat/F-09-agent-abstraction.md](./feat/F-09-agent-abstraction.md) | M1 |
| F-10 | Session 列表命令（`nightme list` / `kill`）| [feat/F-10-session-list-cmd.md](./feat/F-10-session-list-cmd.md) | M3 |
| F-19 | PTY Mode Byte Pipe（Bridge 的 PTY 实现细节）| [feat/F-19-cli-bridge.md](./feat/F-19-cli-bridge.md) | M1 |
| F-20 | Command Gateway（slash command 路由 + /cwd /run /kill /help）| [feat/F-20-gateway.md](./feat/F-20-gateway.md) | M2 |
| F-21 | Agent Communication Modes（ACP / SDK / PTY 三层降级）| [feat/F-21-agent-modes.md](./feat/F-21-agent-modes.md) | M1 arch / M2 partial |

## 2. 后续功能（v0.2+）

| ID | 功能 | 设计文档 | 里程碑 |
|----|------|----------|--------|
| F-11 | 多 Channel mirror 模式 | [feat/F-11-multi-channel-mirror.md](./feat/F-11-multi-channel-mirror.md) | v0.2 |
| F-12 | 多 IM adapter（WhatsApp/Telegram/Slack/Web）| [feat/F-12-multi-im-adapter.md](./feat/F-12-multi-im-adapter.md) | v0.2 |
| F-13 | 终端大小调整（SIGWINCH）| [feat/F-13-terminal-resize.md](./feat/F-13-terminal-resize.md) | v0.2 |
| F-14 | 图片 / 文件附件透传 | [feat/F-14-attachment-passthrough.md](./feat/F-14-attachment-passthrough.md) | v0.2 |
| F-15 | Session 持久化（stdout 历史）| [feat/F-15-session-persistence.md](./feat/F-15-session-persistence.md) | v0.2 |
| F-16 | Web TTY UI（xterm.js + WebSocket）| [feat/F-16-web-tty-ui.md](./feat/F-16-web-tty-ui.md) | v0.2 |
| F-17 | 健康检查 / 心跳 | [feat/F-17-health-check.md](./feat/F-17-health-check.md) | v0.2 |
| F-18 | ~~Token / API key 注入~~ | — | **cancelled** |

> **F-19 编号说明**：cli-bridge 在早期版本是独立顶层文档；按"feature 都在 feat/"原则，作为基础设施类 feature 加入，编号 19 跟在 MVP 功能后面。v0.2+ 功能保持原编号。

> **注意**：v0.2+ 功能的设计文档目前是 stub，仅记录设计方向和 open questions。详细设计在 v0.2 设计阶段补全。
>
> **F-18 已取消**：原计划"检测 hidden input 模式 + 用飞书 card 输入"——违背透传原则。密码 / API key 走标准 IM 透传（详见 PRD §4.1）。F-18 设计文档保留作历史记录（feat/F-18-secret-injection.md）。

---

## 3. v0.1 release checklist

每项 MVP 功能必须有：
- ✅ 单测覆盖率 > 70%
- ✅ 集成测试通过
- ✅ E2E（M2 之后）手测通过

| ID | 必须 | 设计文档 | 单测 | 集成测试 | E2E |
|----|------|----------|------|----------|-----|
| F-01 | ✅ | ✅ | ⏳ | ⏳ | M2 |
| F-02 | ✅ | ✅ | ⏳ | ⏳ | M2 |
| F-03 | ✅ | ✅ | ⏳ | ⏳ | M2 |
| F-04 | ✅ | ✅ | ⏳ | ⏳ | M2 |
| F-05 | ✅ | ✅ | ⏳ | ⏳ | M3 |
| F-06 | ✅ | ✅ | ⏳ | ⏳ | M3 |
| F-07 | ✅ | ✅ | ⏳ | ⏳ | M2 |
| F-08 | ✅ | ✅ | ⏳ | ⏳ | M2 |
| F-09 | ✅ | ✅ | ⏳ | ⏳ | M2 |
| F-10 | ✅ | ✅ | ⏳ | ⏳ | M3 |
| F-19 | ✅ | ✅ | ⏳ | ⏳ | M2 |
| F-20 | ✅ | ✅ | ⏳ | ⏳ | M2 |
| F-21 | ✅ | ✅ | ⏳ | ⏳ | M1+M2 |

**全部 ✅ 后**，nightme v1.0.0 可发布。

---

## 4. 删除项（已从 v0.1 移除）

> 以下功能在 PRD 早期版本中存在，因 Chat↔Session 1:1 模型而删除

- ❌ **Session 列表 in Channel**：用户在新 Chat 首条消息触发 session 创建，无需在 Chat 内查询 session 列表（F-10 命令行版仍保留）
- ❌ **Session 切换 `/attach <sid>`**：因 Chat↔Session 1:1 绑定，切换 = 开新 Chat
