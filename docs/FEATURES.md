# nightme — Feature Index

> **状态**：v1.2 **已锁定**（2026-08-02；commits 5/6/7/8a/8b/8c/9 + F-30；落地 `fix/cwd_session` 分支）
> **作者**：🦞 虾哥（PM/Architect）
> **日期**：2026-08-02
> **关联文档**：
> - 产品定位 → [`PRD.md`](./PRD.md) **v1.2**
> - 技术架构 → [`SPEC.md`](./SPEC.md) **v1.2**
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

## 1b. v1.3 新增功能（设计阶段）

| ID | 功能 | 设计文档 | 里程碑 | 状态 |
|----|------|----------|--------|------|
| F-32 | **Pi Coding Agent Bridge (`pi --mode rpc`)** — 真实 stdio pipes 长驻 JSONL；MVP turn 循环（`get_state` + `prompt` + `agent_settled`）+ `new_session` (F-34) + compaction (F-49) + Resume 经 Pi 的 `--session-id` CLI flag；不打通 Extension UI 飞书闭环；不实现 `/abort` | [feat/F-32-pi-rpc-bridge.md](./feat/F-32-pi-rpc-bridge.md) | v1.3 | ✅ 已实现（核心）；Extension UI + /abort 仍 deferred（见 F-32 §11 未知限制）|
| F-33 | **ChatID 数据模型简化**（删 ChatType 抽象 + topic_group 不特殊处理 + ReplyTo = ParentId）| [feat/F-33-simplify-chatid-data-model.md](./feat/F-33-simplify-chatid-data-model.md) | v1.3.x | ✅ Docs 完成（代码 backlog）|
| F-34 | **`/new` slash command** — 不退进程重置 agent 对话上下文（对齐 claudecode `/clear` / pi 内置 `/new` / acp `session/new`）；可选 `/new <agent>` 精修粒度；清 InputBuffer | [feat/F-34-new-slash-command.md](./feat/F-34-new-slash-command.md) | v1.3.x | ✅ 已实现（Phase 3 review 完成）|
| F-37 | **OutThinking / OutToolStart / OutToolEnd → Feishu thread reply + 类型感知摘要**（反转 §13.6 折叠方案；含 §1.4 抽象/具体边界规范的最终落地——OutboundMessage 100% typed，Meta + Reaction 死代码删除）| [feat/F-37-tool-thread-routing.md](./feat/F-37-tool-thread-routing.md) | v1.3.x | ✅ 已实现（9 commits / PR #31；review-clean）|
| F-38 | **Claude TaskCreate / TaskUpdate → Feishu receipt 任务清单**（成功 tool_result 后归一化完整 task snapshot；Gateway typed 透传；Feishu 单 markdown element 原位 PATCH）| [feat/F-38-task-checklist.md](./feat/F-38-task-checklist.md) | v1.3.x | ✅ 已实现（doc-first + 完整 race 测试覆盖）|
| F-39 | **`/tools on\|off` + 合并渲染** — per-chat ToolsMode 开关（默认 Hide，runtime 丢弃 OutToolStart / OutToolEnd）；当 on 时 Feishu adapter 合并每对 tool 的 start + end 为**一条** thread reply（PATCH 同一 message_id）；10 tools/turn 从 20 thread replies 降到 10 | [feat/F-38-tool-merge-and-toggle.md](./feat/F-38-tool-merge-and-toggle.md) | v1.3.x | ✅ 已实现（4 commits / 单 PR）|
| F-40 | **OutReply 超限改独立 Reply + `OutText` → `OutReply` 改名** — 长度(> 8000 runes)或数量(≥ 45 entries)超限 → `ReplyInThreadAndChat` 独立 reply；删 receipt 内 600B truncate;迟到 OutReply 走独立 reply 不静默丢 | [feat/F-40-outreply-overflow.md](./feat/F-40-outreply-overflow.md) | v1.3.x | ✅ 已实现（PR #43）|
| F-41 | **WS active reconnect** — 30s ticker 在 `OnDisconnected` 后周期性 `Stop() + 100ms + Start()`，把 WS 断开到重连最大等待从 SDK 默认 2min 压到 30s；无 HTTP probe / 无 tier / 无 circuit breaker | [feat/F-41-active-reconnect.md](./feat/F-41-active-reconnect.md) | v1.3.x | ✅ 已实现 |
| F-42 | **Lazy Receipt Creation + MessageState 简化 + TaskList 标题** — 删 cold-start 空 Receipt card，改 lazy create（首个 OutReply / OutTask 触发）；Feishu MessageState reactions 删 ⏳/🔄 留 ✅/❌；TaskList 永远加 `**📋 Tasks**` markdown 标题 | [feat/F-42-lazy-receipt-creation.md](./feat/F-42-lazy-receipt-creation.md) | v1.3.x | 📝 设计阶段（doc-first）|
| F-43 | **`/kill` graceful + `/new` ResumeID clear + per-entry list reply** — `KillAll` 走 bridge.Close graceful 路径（5s outer timeout）；dead entry 的 ResumeID 清空防止 `--resume <dead-id>` 复活；`✓/✗/•` 三种 emoji 的 per-agent 列表回复，4KB 字节 cap + ...and N more 截断（Feishu 限制）| [feat/F-43-kill-new-graceful-and-reset.md](./feat/F-43-kill-new-graceful-and-reset.md) | v1.3.x | 🚧 PR #44 已开（review 中）|
| F-44 | **OutReply 拆出 Receipt + Task Receipt 瘦身 + OutInit/OutUsage 推迟** — `OutReply` 改为每 chunk 独立 `ReplyInThreadAndChat`（复用 F-39 `sendResultAsReply` 的 3 段 dispatch + sanitize + envelope defense）；Task Receipt card 只剩 `**📋 Tasks**` checklist section（删 header / entries / footer / hr）；`OutInit` / `OutUsage` silent drop 推迟到 footer PR。删除 `ensureReceiptForReply` / `isOverflowingReceipt` / `sendReplyAsMessage` | [feat/F-44-outreply-independent-and-task-receipt.md](./feat/F-44-outreply-independent-and-task-receipt.md) | v1.3.x | 📝 设计阶段（doc-first）|
| F-45 | **Main-Chat 卡片 Footer + AgentSession 累计 Token 持久化**（兑现 F-44 §6.1 推迟）— `AgentSession` 自管 metadata（`Model` + `cumulativeUsage UsageInfo` + mutex + dirty flag），EventAgentConnected 时 capture model，EventUsage 时累加，EventDone 时落盘 `agent_sessions.json`；**仅 `/new` 清零**（daemon 重启 / `/cwd` / `/use` / `/kill` 保留）。`OutboundMessage` 加 1 个 typed snapshot field `SessionContext *SessionContext`（不是 3 个分散字段），runtime 在 `newEventHandler` 一次性 stamp 到 4 个 main-chat Kind（`OutReply` / `OutResult` / `OutTaskCreate` / `OutTaskUpdate`）。`UsageInfo` 从 `gateway` 搬到 `agent` 包消除反向 import。Footer 格式 C 版（ASCII 箭头无 emoji）：`claude · opus-4-5 · ↓ 12.3k · ↻ 8.2k cached · ↑ 1.5k · Total 22.0k · $0.087`。`OutInit` / `OutUsage` 仍是 silent drop（不变），footer 数据走 `SessionContext` 单独路径 | [feat/F-45-session-footer.md](./feat/F-45-session-footer.md) | v1.3.x | 📝 设计阶段（doc-first）|
| F-46 | **Interactive Decision Cards** — gtw 决策卡升级为交互卡 + 原地更新；按钮点击在 Channel 边界归一化为既有 reaction/action 通路；新增 `OutCard` / `OutCardPatch` | [feat/F-46-interactive-cards.md](./feat/F-46-interactive-cards.md) | v1.3.x | ✅ 已落地（UAT via `/gtw test`）|
| F-48 | **Footer Line 3 — Git Branch Tracking** — SessionContext 加 workspace / git snapshot；main-chat footer 第三行展示 branch / dirty / ahead | [feat/F-45-session-footer.md](./feat/F-45-session-footer.md) §1.7 | v1.3.x | ✅ 已落地 |
| F-49 | **Compaction Counter + Footer 语义拆分** — footer 暴露压缩次数；token 行为 since-last-compaction，`$cost` 仍为 lifetime；删「正在压缩…」瞬时出站通路；bridge 归一化协议差异 | [feat/F-49-compaction-counter.md](./feat/F-49-compaction-counter.md) | v1.3.x | 📝 设计阶段（doc-first）|
| F-50 | **GitProvider 抽象 + 两阶段 Provider 探测** — 抽象层重命名 `Platform*` → `Provider*`；`Detect` 两阶段：URL hint（`github.com` / `gitlab` 子串零网络直返）+ API probe fallback（GitLab `/api/v4/version` / GitHub Enterprise `/api/v3/meta`）；新增 `HTTPProber` 接口对齐 `CLIRunner` 模式；自建 GitHub Enterprise / GitLab 现在能被识别。是 `F-45 §3.5` / `gtw §5.x` / `F-45 §7.2` 悬空引用的归宿 | [feat/F-50-git-provider.md](./feat/F-50-git-provider.md) | v1.3.x | 📝 设计阶段（doc-first）|
| F-51 | **Slash Command 分层** — `/cwd` `/use` `/kill` `/new` `/watch` `/think` `/tools` `/gtw` 统一由 `internal/command/` 的 Commander / Registry / Factory 路由；需要 chat-session 状态的 Factory 直接使用 `*chatsession.Manager`，reaction 由 `ReactionRouter` 统一分发 | [SPEC](./SPEC.md) | v1.3.x | ✅ 已实现 |
| F-52 | **Pi Bridge 流式事件整合** — pi 以 token 粒度推 `text_delta`，改动前逐 token emit `EventText`，一句话在飞书裂成 ~20 条 💬 + ~20 次卡片 PATCH；translator 改为缓冲 delta，在 `tool_execution_start` flush 中途叙述、在 `agent_settled` 发**一轮唯一**的 `EventResult`；顺带修复两处静默故障：pi 从来不产生 OutResult（`sawTextEnd` 抑制 → gateway 丢空文本 result）、以及连带丢失的 usage/cost。usage 改为取最后一条快照而非累加（上下文占用语义）。共享层 `AccumulateUsage` 累加→覆盖 + footer 分母留待下个 PR | [feat/F-52-pi-stream-aggregation.md](./feat/F-52-pi-stream-aggregation.md) | v1.3.x | ✅ 已实现 |
| F-53 | **Message / Prompt 生命周期模型** — 把"一次提交给 agent 的执行单元"从裸元组 + 用完即扔的字符串标量（`currentTurnUserMsgID`）正式实体化为 `Prompt`；`Message.Stage` 收敛为纯投递语义（`Queued`/`Submitted`/`Dropped`，不再镜像执行结果，堵住批次内非 anchor 消息永远卡在中间态的问题）；`Prompt` 执行状态收敛为 `Running`/`Done` 两态，成功/失败只由 `EndReason` 承载；`Turn` 系命名（`OnTurnEnded`/`currentTurnUserMsgID`）全面退役。`agent.MessageState` 常量从 `Received/Forwarded/Done/Failed` 物理改名为 `Queued/Submitted/Dropped`，`Done/Failed` 删除（Feishu 用户消息上 ✅/👎 反应**永久下线**，明确 UX 回归点）。`Prompt` 实体挂 `AgentSession.currentPrompt`，anchor 由 `Prompt.LastMessageID` 提供 | [feat/message_lifecycle.md](./feat/message_lifecycle.md) | v1.3.x | 🚧 Phase 0 实施中 |

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
| F-24 | Claude Code Bridge（JSON-IO + auto-accept + AskUserQuestion）| [feat/F-24-claudecode-bridge.md](./feat/F-24-claudecode-bridge.md) | v0.2 |
| F-25 | Rolling-Log Receipt UX (Channel-Autonomous) | [feat/F-25-rolling-log.md](./feat/F-25-rolling-log.md) | v1.3 |
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
| **`internal/session/MemoryManager` cleanup** | v1.x `session.MemoryManager` still used by `internal/gateway/cmd/handlers.go` (binding helpers, type alias `Session = *session.Session`). Needs `internal/gateway/cmd` to switch to `chatsession.ChatSession` (drop binding table — bindings now live in `Manager`). | Cleans up the last v1.x runtime residue; reduces gateway surface area; unlocks removal of `cmd/nightme/run.go` entirely | see git history |
| **Rolling-log receipt card UX (v1.x → v1.3)** | ✅ **Resolved in v1.3**: one user message → ONE Feishu receipt card (cold-create + PATCH per turn); reactions ⏳/🔄/✅ by `MessageState` FSM (separate concern); content by `OutboundMessage{ReplyTo: userMsgID}`. Gateway no longer holds receipt FSM (SPEC §0.1). | Aligned with original v1.1 intent. See [`F-25-rolling-log.md`](./feat/F-25-rolling-log.md). |
| **MessageState (reaction lifecycle)** | ✅ Done in v1.3 (F-31, branch `fix/inboud_buffer`); **v1.3.x F-53 重做**：4 态（`StateReceived` ⏳ / `StateForwarded` 🔄 / `StateDone` ✅ / `StateError` ❌）→ 3 态（`MessageQueued` ⏳ / `MessageSubmitted` 🔄 / `MessageDropped` 空），`MessageDone/MessageFailed` 物理删除，Feishu ✅/👎 反应**永久下线**。原 F-31 文档保留作 v1.3 历史参考，新设计权威定义见 `docs/feat/message_lifecycle.md`。 | Tracks message lifecycle visually | `docs/feat/F-31-message-state.md`（superseded）+ `docs/feat/message_lifecycle.md`（authoritative）|
| **Exit observer wiring** | ✅ **Resolved in `feat/alive` (CS-AS 边界重构 Phase 1)**: per-AS readpump now lives inside `AgentSession` (started by `Spawn`, exited by `Shutdown`). The F-53 deadlock (process crashed → 🔄 stuck forever) is fixed by `endPrompt(ProcessDied)` in the `!ok` branch. `SetAgentExitObserver` / `StartObserveClose` are kept as no-op stubs for compat; the new model surfaces lifecycle via `KindLifecycle` events in the `EnrichedEvent` stream. Respawn-on-death wiring is still future work (next PR — L1 Pinger). | Tracks prompt lifecycle through process death cleanly | `git log feat/alive` |
| **`nightme config` second-tier menu** | Currently only the `Agents` submenu exists. Future: `Feishu`, `Session`, `Logging`, `Paths` submenus for the same interactive workflow. | Each submenu is a small independent feature; defer until users ask. | `docs/feat/F-30-interactive-config.md` §8 |
| **Command string quoting** | `Command: "claude --dangerously-skip-permissions"` is split via `strings.Fields`. Paths with spaces are not supported. | If a binary path has spaces, the spawn breaks. Not common (binaries live in `$PATH`); defer. | `MIGRATION.md` §"Config schema" |

---

## 8. v1.2 docs status

| Doc | Status |
|-----|--------|
| `docs/PRD.md` | ✅ locked 2026-08-02 |
| `docs/SPEC.md` | ✅ locked 2026-08-02 |
| `docs/FEATURES.md` | ✅ locked 2026-08-02 |
| git history | ✅ all v1.x commits landed |
| `docs/feat/F-27-chatsession.md` | ✅ includes runtime contracts (Spawner / FlushHook / Manager / EventHandler) |
| `docs/feat/F-28-use-command.md` | ✅ |
| `docs/feat/F-29-agent-session-pool.md` | ✅ includes Spawner production wiring + corrected v1.1 migration story |
| `docs/feat/F-30-interactive-config.md` | ✅ |
| `docs/feat/F-25-rolling-log.md` | ✅ Current (v1.3; Channel-autonomous rolling-log UX) |
| `docs/feat/message_lifecycle.md` | 🚧 F-53 Phase 0 实施中（设计已对齐最终方案） |
| `README.md` | ✅ one-dev-version framing |
| `CHANGELOG.md` | ✅ single [Unreleased] covering current dev |
| `MIGRATION.md` | ✅ breaking-changes guide from v1.x |