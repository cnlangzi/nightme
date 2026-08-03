# nightme — Feature Index

> **状态**：v1.2 **已锁定**（2026-08-02；commits 5/6/7/8a/8b/8c/9 + F-30；落地 `fix/cwd_session` 分支）
> **作者**：🦞 虾哥（PM/Architect）
> **日期**：2026-08-02
> **关联文档**：
> - 产品定位 → [`PRD.md`](./PRD.md) **v1.2**
> - 技术架构 → [`SPEC.md`](./SPEC.md) **v1.2**
> - 实施计划 → [`PLAN.md`](./PLAN.md)
> - 每个 feature 的详细设计 → [`feat/`](./feat/)

> **v1.2 架构变更摘要**：v1.1 三层（Channel Adapter / Gateway / Session Manager）→ v1.2 两层（Channel Adapter / Gateway + **ChatSession** + **AgentSession**）。ChatSession 合并了 v1.1 ChannelSession + GatewaySession 的逻辑中枢；AgentSession 取代 v1.1 Session，以 `(agent, cwd)` 1:1 唯一标识。详见 [SPEC §0](./SPEC.md) 和 [F-27](./feat/F-27-chatsession.md) / [F-28](./feat/F-28-use-command.md) / [F-29](./feat/F-29-agent-session-pool.md)。

本文档是 nightme 的**功能索引**。每项功能的设计细节（接口、实现、edge cases、测试计划）见 [`feat/`](./feat/) 目录下的独立文档。

---

## 1. v1.2 新增功能（已落地）

| ID | 功能 | 设计文档 | 里程碑 | 状态 |
|----|------|----------|--------|------|
| F-27 | **ChatSession 模型**（per chat 持久化会话上下文；合并 v1.1 ChannelSession + GatewaySession 逻辑）| [feat/F-27-chatsession.md](./feat/F-27-chatsession.md) | v1.2 (current) | ✅ 已实现 (commits 5/6/8b) |
| F-28 | **`/use <agent>` 命令**（切换 activeAgent；复用或新建 AgentSession；永不重启进程）| [feat/F-28-use-command.md](./feat/F-28-use-command.md) | v1.2 (current) | ✅ 已实现 (commits 8a/8c) |
| F-29 | **AgentSession 池**（`(agent, cwd)` 1:1 池化；`/cwd` / `/use` 不杀任何 AgentSession，切回能复用）| [feat/F-29-agent-session-pool.md](./feat/F-29-agent-session-pool.md) | v1.2 (current) | ✅ 已实现 (commits 7/8c) |
| F-30 | **Interactive Config**（`nightme config` 进交互菜单；二级菜单只做 Agents；merge builtin + cfg；选 primary）| [feat/F-30-interactive-config.md](./feat/F-30-interactive-config.md) | v1.2 (current) | ✅ 已实现 |

**v1.2 关键变化**：
- 删除：`/run` 命令（被 `/use` 替代）
- 删除：`Session` 类型（被 ChatSession + AgentSession 替代）
- 删除：`CreateOrUpdate(chatID, ...)` / `Run(chatID, agent, args)` / `GetByChat` / `KillByChat`（v1.1 已删除，v1.2 不重新引入）
- 新增：`/use <agent>` 切换命令
- 新增：`nightme config` 交互模式（顶层菜单 + Agents 子菜单）
- Config schema 重构：top-level `primary` + `agents` list（替代 v1.1 `agent.default` + `agent.agents` map）
- 保留：所有 v1.1 职责隔离不变式（Channel 与 Session 互不知道；Gateway 是 binding + receipt FSM owner）

**Status**: **已锁定**（2026-08-02；Q-B ✅ exact → default → spawn）

---

## 2. MVP 功能（v0.1，已发布）

| ID | 功能 | 设计文档 | 里程碑 |
|----|------|----------|--------|
| F-01 | Session 生命周期（`/cwd` + `/run` 两步式；workspace 持久，CLI 可重启）| [feat/F-01-session-create.md](./feat/F-01-session-create.md) | M2 |
| F-02 | 消息透传（Channel → AgentSession via Gateway）| [feat/F-02-message-passthrough.md](./feat/F-02-message-passthrough.md) | M2 |
| F-03 | 输出推送（AgentSession Events → Channel）| [feat/F-03-output-push.md](./feat/F-03-output-push.md) | M2 |
| F-04 | PTY 模拟（aymanbagabas/go-pty）| [feat/F-04-pty-simulation.md](./feat/F-04-pty-simulation.md) | M1 |
| F-05 | 进程注册（JSON registry）| [feat/F-05-process-registry.md](./feat/F-05-process-registry.md) | M1 |
| F-06 | 进程清理（默认 detach，--cleanup kill）| [feat/F-06-process-cleanup.md](./feat/F-06-process-cleanup.md) | M3 |
| F-07 | Workspace 绑定（cwd 校验 + 路径展开）| [feat/F-07-workspace-binding.md](./feat/F-07-workspace-binding.md) | M1 |
| F-08 | Channel 抽象（interface + 飞书实现 + receipt lifecycle 渲染）| [feat/F-08-channel-abstraction.md](./feat/F-08-channel-abstraction.md) | M2 / v0.3 receipt |
| F-09 | Agent 抽象（interface + mode 选择）| [feat/F-09-agent-abstraction.md](./feat/F-09-agent-abstraction.md) | M1 |
| F-10 | Session 列表命令（`nightme list` / `kill`）| [feat/F-10-session-list-cmd.md](./feat/F-10-session-list-cmd.md) | M3 |
| F-19 | PTY Mode Byte Pipe（Bridge 的 PTY 实现细节）| [feat/F-19-cli-bridge.md](./feat/F-19-cli-bridge.md) | M1 |
| F-20 | Command Gateway + Binding 表 + Run 决策 + Receipt FSM owner | [feat/F-20-gateway.md](./feat/F-20-gateway.md) | M2 / v0.3 增强 |
| F-21 | Agent Communication Modes（ACP / SDK / PTY / JSON-IO 四层降级）| [feat/F-21-agent-modes.md](./feat/F-21-agent-modes.md) | M1 arch / M2 partial / v0.2 JSON-IO |
| F-22 | Feishu One-Click App Registration（QR 扫码授权 onboarding）| [feat/F-22-feishu-onclick-registration.md](./feat/F-22-feishu-onclick-registration.md) | M2 |
| F-23 | Heartbeat & Streaming Status（Channel-driven ticker）| [feat/F-23-heartbeat.md](./feat/F-23-heartbeat.md) | v0.2 / v0.3 迁移 |
| F-24 | Claude Code Bridge（JSON-IO + auto-accept + AskUserQuestion）| [feat/F-24-claudecode-bridge.md](./feat/F-24-claudecode-bridge.md) | v0.2 |
| F-25 | Input Buffer（FSM only；receipt 由 Gateway + Channel 联合管理）| [feat/F-25-input-buffer.md](./feat/F-25-input-buffer.md) | v0.2 / v0.3 瘦身 |
| F-26 | Gateway Hub & Responsibility Isolation（v1.1 职责隔离权威参考）| [feat/F-26-gateway-hub.md](./feat/F-26-gateway-hub.md) | v0.3 |

> **F-01 / F-07 / F-20 / F-25 在 v1.2 会有 breaking 改动**（`/cwd` / `/run` / `Session` 概念重做）。详见 F-27 / F-28 / F-29。

---

## 3. 后续功能（v0.2+，未实现）

| ID | 功能 | 设计文档 | 里程碑 |
|----|------|----------|--------|
| F-11 | 多 Channel mirror 模式 | [feat/F-11-multi-channel-mirror.md](./feat/F-11-multi-channel-mirror.md) | v0.2 |
| F-12 | 多 IM adapter（WhatsApp/Telegram/Slack/Web）| [feat/F-12-multi-im-adapters.md](./feat/F-12-multi-im-adapters.md) | v0.2 |
| F-13 | 终端大小调整（SIGWINCH）| [feat/F-13-terminal-resize.md](./feat/F-13-terminal-resize.md) | v0.2 |
| F-14 | 图片 / 文件附件透传 | [feat/F-14-attachment-passthrough.md](./feat/F-14-attachment-passthrough.md) | v0.2 |
| F-15 | Session 持久化（stdout 历史）| [feat/F-15-session-persistence.md](./feat/F-15-session-persistence.md) | v0.2 |
| F-16 | Web TTY UI（xterm.js + WebSocket）| [feat/F-16-web-tty-ui.md](./feat/F-16-web-tty-ui.md) | v0.2 |
| F-17 | ~~健康检查 / 心跳~~ | ~~[feat/F-17-health-check.md](./feat/F-17-health-check.md)~~ | **superseded by F-23** |
| F-18 | ~~Token / API key 注入~~ | — | **cancelled** |

[v1.2 不变]

---

## 4. v1.2 release checklist

每项 v1.2 功能必须有：
- ✅ 单测覆盖率 > 70%
- ✅ 集成测试通过
- ✅ E2E 飞书 DM round-trip 手测通过
- ✅ PRD/SPEC/FEATURES 草案 → 锁定

| ID | 必须 | 设计文档 | 单测 | 集成测试 | E2E |
|----|------|----------|------|----------|-----|
| F-27 | ✅ | ✅ | ✅ | ✅ | ⏳ |
| F-28 | ✅ | ✅ | ✅ | ✅ | ⏳ |
| F-29 | ✅ | ✅ | ✅ | ✅ | ⏳ |
| F-30 | ✅ | ✅ | ✅ | ✅ | ⏳ |

**E2E 飞书 DM round-trip 手测**仍是 v0.4 release gate。单测 + 集成测试已覆盖 F-27/28/29/30。

---

## 5. 删除项（已从设计中移除）

> 以下功能在 PRD 早期版本中存在，因 ChatSession 模型而删除 / 调整

- ❌ **Session 列表 in Channel**：用户在新 Chat 首条消息触发 ChatSession 创建，无需在 Chat 内查询 session 列表（F-10 命令行版仍保留）
- ❌ **Session 切换 `/attach <sid>`**：因 Chat↔ChatSession 1:1 绑定，切换 = 开新 Chat
- ❌（v1.2 删除）**`/run` 命令**：被 `/use` 替代；`/run` 在 v0.x 阶段用于显式启动 CLI，v1.2 改为 `/use` 隐式管理
- ❌（v1.2 重命名）**`Session` 类型**：v1.1 的 `Session` 在 v1.2 拆成 `ChatSession`（会话上下文）+ `AgentSession`（CLI 进程句柄）
- ❌（v1.2 强化）**ChatSession 与 AgentSession 的耦合**：v1.1 已通过 Session 拆 ChatID 实现去耦；v1.2 通过 ChatSession.pool 强化

---

## 6. v1.2 决策记录（待锁定）

| 决策 | 现状 | 状态 |
|------|------|------|
| Q-A: Primary Agent 设置粒度 | 全局 only (`config.yaml` 的 `primary`)；ChatSession.primaryAgent 是创建时 snapshot；**无 `/default` 命令** | ✅ 已锁定 (2026-08-02) |
| Q-B: `(activeAgent, activeCwd)` lookup 行为 | 只看 `(activeAgent, activeCwd)`：命中 Running 复用，否则 spawn；**无运行时 fallback** | ✅ 已锁定 (2026-08-02) |
| `ChatSession.primaryAgent` 字段持久化位置 | `ChatSessionEntry.primaryAgent` (snapshot，写时不变) | ✅ 已锁定 |
| Config schema 顶层字段 | `primary` (top-level scalar) + `agents:` (top-level list); bridge 是每个 AgentEntry 的字段; command 是 full string 含 args | ✅ 已锁定 (2026-08-02) |

---

## 7. Backlog (deferred, tracked)

These are known gaps in the current dev snapshot. Each has a
clear scope but was deferred to keep the v1.2 cut small. They
are tracked here (not in a TODO file) so they don't get lost.

| Item | Description | Impact | Tracking |
|------|-------------|--------|----------|
| **E2E 飞书 DM round-trip** | Manual smoke test only; unit + integration cover F-27/28/29/30. Real Feishu WS + multi-turn send/receive not automated. | Release gate (PR `feat/air-dashboard-realization` and any future "this is production-ready" claim) | `docs/E2E_TESTING.md` (manual checklist) |
| **`internal/session/MemoryManager` cleanup** | v1.x `session.MemoryManager` still used by `internal/gateway/cmd/handlers.go` (binding helpers, type alias `Session = *session.Session`). Needs `internal/gateway/cmd` to switch to `chatsession.ChatSession` (drop binding table — bindings now live in `Manager`). | Cleans up the last v1.x runtime residue; reduces gateway surface area; unlocks removal of `cmd/nightme/run.go` entirely | `docs/PLAN.md` §4.6.8 |
| **Rolling-log receipt card UX (v1.x)** | v1.1 had: one user message → ONE Feishu receipt card; ⏳/🔄/✅/❌ reaction emoji + rolling body. v1.2 sends plain `OutText` replies (no card). | Chat UX regression vs v1.1; users may want the receipt back. Plan: implement receipt FSM in newMessageDispatcher (call `ch.CreateReceipt` from Feishu adapter) | `docs/feat/F-25-input-buffer.md` (stale) + `CHANGELOG.md` "Known gaps" |
| **Exit observer wiring** | `ChatSession.SetAgentExitObserver` + `StartObserveClose` exist; the runtime does not register an observer. readPump's natural exit is currently sufficient. | Reserved API for future work (respawn on death, /kill auto-reply, log user-visible "agent died" message). | `docs/feat/F-27-chatsession.md` §5.1.5 |
| **`nightme config` second-tier menu** | Currently only the `Agents` submenu exists. Future: `Feishu`, `Session`, `Logging`, `Paths` submenus for the same interactive workflow. | Each submenu is a small independent feature; defer until users ask. | `docs/feat/F-30-interactive-config.md` §8 |
| **Command string quoting** | `Command: "claude --dangerously-skip-permissions"` is split via `strings.Fields`. Paths with spaces are not supported. | If a binary path has spaces, the spawn breaks. Not common (binaries live in `$PATH`); defer. | `MIGRATION.md` §"Config schema" |

---

## 8. v1.2 docs status

| Doc | Status |
|-----|--------|
| `docs/PRD.md` | ✅ locked 2026-08-02 |
| `docs/SPEC.md` | ✅ locked 2026-08-02 |
| `docs/FEATURES.md` | ✅ locked 2026-08-02 |
| `docs/PLAN.md` §4.6 | ✅ all 12 commits landed + cleanup commit 13 |
| `docs/feat/F-27-chatsession.md` | ✅ includes runtime contracts (Spawner / FlushHook / Manager / EventHandler) |
| `docs/feat/F-28-use-command.md` | ✅ |
| `docs/feat/F-29-agent-session-pool.md` | ✅ includes Spawner production wiring + corrected v1.1 migration story |
| `docs/feat/F-30-interactive-config.md` | ✅ |
| `docs/feat/F-25-input-buffer.md` | ⚠️ STALE (v1.x; superseded by v1.2 chatsession/input_buffer.go) |
| `README.md` | ✅ one-dev-version framing |
| `CHANGELOG.md` | ✅ single [Unreleased] covering current dev |
| `MIGRATION.md` | ✅ breaking-changes guide from v1.x |