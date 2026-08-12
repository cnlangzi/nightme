# nightme — Feature Index

> **关联文档**：
> - 产品定位 → [`PRD.md`](./PRD.md)
> - 技术架构 → [`SPEC.md`](./SPEC.md)

本文档是 nightme 的**功能索引**。每项功能的设计细节见对应文档。

---

## 1. 运行时与核心架构

| 功能 | 设计文档 |
|------|----------|
| Session 生命周期（`/cwd` 设 workspace；CLI 进程可重启）| [feat/F-runtime.md](./feat/F-runtime.md) |
| 进程注册 / 清理 / Workspace 绑定 / Session 列表命令 | [feat/F-runtime.md](./feat/F-runtime.md) |
| PTY 模拟 + CLI Transport + Agent Communication Modes | [feat/F-runtime.md](./feat/F-runtime.md) + [bridge/cli-transport.md](./bridge/cli-transport.md) |
| 消息透传（Channel → AgentSession via Gateway）| [feat/F-message-flow.md](./feat/F-message-flow.md) |
| 输出推送（AgentSession Events → Channel）| [feat/F-message-flow.md](./feat/F-message-flow.md) |
| Command Gateway + Binding 表 + 职责隔离 | [feat/F-gateway.md](./feat/F-gateway.md) |

## 2. ChatSession 会话模型

| 功能 | 设计文档 |
|------|----------|
| ChatSession 模型（per chat 持久化会话上下文）| [feat/F-chat-session.md](./feat/F-chat-session.md) |
| `/use <agent>` 命令 | [feat/F-chat-session.md](./feat/F-chat-session.md) |
| AgentSession 池（`(agent, cwd)` 1:1 池化）| [feat/F-chat-session.md](./feat/F-chat-session.md) |
| `/new` slash command | [feat/F-chat-session.md](./feat/F-chat-session.md) |
| `/close` graceful + per-entry list reply | [feat/F-chat-session.md](./feat/F-chat-session.md) |
| Message / Prompt 生命周期模型（`Prompt` 一等公民）| [feat/F-53-message-prompt-lifecycle.md](./feat/F-53-message-prompt-lifecycle.md) |

## 3. Bridge / Agent 集成层

| 功能 | 设计文档 |
|------|----------|
| Claude Code Bridge | [bridge/claude.md](./bridge/claude.md) |
| Codex App-Server Bridge | [bridge/codex.md](./bridge/codex.md) |
| OpenCode Bridge | [bridge/opencode.md](./bridge/opencode.md) |
| Pi Coding Agent Bridge（传输 + 流式事件 + contextWindow）| [bridge/pi.md](./bridge/pi.md) |
| Channel interface 抽象 | [feat/F-08-channel-abstraction.md](./feat/F-08-channel-abstraction.md) |
| Agent 抽象（`AgentSpec` / `Starter` interface + `Agent` runtime handle）| [feat/F-09-agent-abstraction.md](./feat/F-09-agent-abstraction.md) |

## 4. Channel 渲染（飞书 IM）

| 功能 | 设计文档 |
|------|----------|
| Feishu 渲染策略总览（Receipt / Thread / Footer / Card）| [channel/feishu-rendering.md](./channel/feishu-rendering.md) |
| Rolling-Log Receipt UX | [channel/feishu-rendering.md](./channel/feishu-rendering.md) |
| MessageState 进度反应（⏳ / 🔄 / ✅ / ❌）| [channel/feishu-rendering.md](./channel/feishu-rendering.md) |
| Tool/Thinking → Thread Reply + 类型感知摘要 | [channel/feishu-rendering.md](./channel/feishu-rendering.md) |
| Task Checklist in Receipt | [channel/feishu-rendering.md](./channel/feishu-rendering.md) |
| `/tools on\|off` + 合并渲染 | [channel/feishu-rendering.md](./channel/feishu-rendering.md) |
| OutReply 超限改独立 Reply | [channel/feishu-rendering.md](./channel/feishu-rendering.md) |
| Lazy Receipt Creation | [channel/feishu-rendering.md](./channel/feishu-rendering.md) |
| OutReply 拆出 Receipt + Task Receipt 瘦身 | [channel/feishu-rendering.md](./channel/feishu-rendering.md) |
| Main-Chat 卡片 Footer (per-turn snapshot) | [channel/feishu-rendering.md](./channel/feishu-rendering.md) |
| Interactive Decision Cards | [channel/feishu-rendering.md](./channel/feishu-rendering.md) |
| Compaction Counter | [channel/feishu-rendering.md](./channel/feishu-rendering.md) |
| Footer Context Window 显示 | [channel/feishu-rendering.md](./channel/feishu-rendering.md) |
| OutResult 独立 Reply（不再 fold receipt）| [channel/feishu-rendering.md](./channel/feishu-rendering.md) |
| Feishu 限速 / 重试 / WS 重连（可靠性）| [channel/feishu-reliability.md](./channel/feishu-reliability.md) |
| Feishu Onboarding（附件透传 / App QR 注册）| [channel/feishu-onboarding.md](./channel/feishu-onboarding.md) |

## 5. 配置 / 数据模型

| 功能 | 设计文档 |
|------|----------|
| Interactive Config（`nightme config` 交互菜单）| [feat/F-30-interactive-config.md](./feat/F-30-interactive-config.md) |
| ChatID 数据模型简化 | [feat/F-33-simplify-chatid-data-model.md](./feat/F-33-simplify-chatid-data-model.md) |

## 6. Git Worktree 自动化（`/gtw`）

| 功能 | 设计文档 |
|------|----------|
| `/gtw` 子命令集：`fix` / `close` / `commit` / `push` / `pr` / `sync` | [feat/F-gtw.md](./feat/F-gtw.md) |
| `/gtw push` 三分支流 | [feat/F-gtw.md](./feat/F-gtw.md) |
| `/gtw push` + `/gtw pr` 联动 Readiness Gate | [feat/F-gtw.md](./feat/F-gtw.md) |
| GitProvider 抽象 + 两阶段 Provider 探测 | [feat/F-50-git-provider.md](./feat/F-50-git-provider.md) |

## 7. per-chat 行为开关

- **`/watch on|off`**：群聊是否接收所有消息（默认只 @ 收）— 见 [channel/feishu-rendering.md](./channel/feishu-rendering.md)
- **`/think on|off`**：是否在 thread reply 显示 agent 思考内容 — 见 [channel/feishu-rendering.md](./channel/feishu-rendering.md)
- **`/tools on|off`**：是否显示工具调用的 thread reply — 见 [channel/feishu-rendering.md](./channel/feishu-rendering.md)

三个 toggle 模式一致：ChatSession 上挂 mode 字段 + Gateway dispatcher 入口 gate + Channel 自治渲染。

## 8. 范围外（明确不做）

| ❌ 不做 | 原因 |
|---------|------|
| 多 AgentSession 并行协作 | ChatSession active 单一原则 |
| 跨 chat 共享 ChatSession | Channel 隔离 |
| LLM 编排 / Code review / 提示词改写 | 透传原则 |
| 接管用户已有的 shell / terminal multiplexer | 进程归属原则 |
| Multi-user RBAC / 云端 SaaS | 单用户独占假设 |