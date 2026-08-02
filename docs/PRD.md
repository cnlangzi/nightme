# nightme — Product Requirements Document (PRD)

> **状态**：v1.2 **已锁定**（2026-08-02；Q-A ✅ 全局 Default only；Q-B 默认 exact → default → spawn；架构已落地于 commits 5/6/7/8a/8b/8c/9）
> **作者**：🦞 虾哥（PM/Architect）
> **日期**：2026-08-02
> **文档层级**：产品级（**不含技术内容**）
> **关联文档**：
> - 技术架构 → [`SPEC.md`](./SPEC.md) **v1.2**
> - 功能索引 → [`FEATURES.md`](./FEATURES.md)
> - 每个 feature 的详细实现 → [`feat/`](./feat/)

---

## 1. 产品定义

**nightme** 是一个运行在用户电脑上的"AI Coding CLI 远程遥控桥"——让用户从手机 / IM 端远程驱动本地 Claude Code / Codex / OpenCode，在多项目之间无缝切换。

nightme 自己**不调用 LLM、不做任务编排、不做代码审查**。它只是一个 I/O 搬运工：把 IM 消息搬到本地 CLI 的 TTY 输入框，再把 CLI 的屏幕输出搬回 IM。

**Slogan**：`Sleep tight, code all night.`

可以把它想象成：一个"被 IM 控制的 Web TTY"，但这个 TTY 不是通用 shell，只针对 AI Coding Agent。

---

## 2. 目标用户

nightme 为以下用户设计：

- **使用 AI Coding CLI 的开发者**：Claude Code / Codex / OpenCode 的活跃用户
- **手机办公者**：通勤、出差、躺床上时也想继续跟 agent 协作
- **多项目并行者**：同时维护 2~5 个项目，需要在不同 context 间切换
- **IM 重度用户**：把飞书 / WhatsApp 当作主要沟通工具

**用户画像假设**：单用户独占一台电脑。不假设多用户共享设备。

---

## 3. 典型使用场景

### 3.1 通勤路上继续写代码
- 早上地铁上打开手机 → DM nightme bot → 发 slash command 设置 cwd
- bot 回复 "Session started"，用户在飞书里继续跟 Claude Code 聊
- Claude Code 实际写在用户的 MacBook 上（agent 在本地跑）
- 到公司打开电脑，看到工作目录里已经有了改动

### 3.2 多项目并行管理
用户的飞书 DM 列表：
```
nightme · bailing        ← ChatSession A，workspace ~/code/bailing
nightme · nightme        ← ChatSession B，workspace ~/code/nightme
nightme · side-project   ← ChatSession C，workspace ~/code/side
```
每个 DM = 一个 ChatSession = 一个项目（ChatSession 内可有多个 AgentSession——见 §4.3）。飞书侧 DM 列表天然成为项目列表。

### 3.3 夜间让 agent 跑长任务
- 晚上 11 点：发任务 "把 bailing 的字幕系统改成 VTT"
- 睡觉
- 早上醒来查 DM，看到 Claude Code 的工作进度 + 文件改动

### 3.4 应急修复
- 周末出门在外，线上服务报警
- DM nightme · production-tools → slash command 设置 cwd 到 ~/tools/prod
- 通过 Claude Code 排查问题（agent 有完整的工具 + 上下文）

### 3.5 同一 chat 内切换 agent 试不同方案（v1.2 新增）
- 在 bailing 项目的 chat 里跟 claude 聊了一会
- 想让 codex 也看看 → 发 `/use codex`
- nightme 启动 `(codex, /code/bailing)` AgentSession（pool 里没有，新建）
- 跟 codex 聊完，再 `/use claude` → 切回 claude，**进程是上次那个**（对话上下文保留）
- 切到不同 cwd `/use codex` 后再 `/cwd /code/bailing` 切回，原 `(claude, /code/bailing)` AgentSession 又被接回来用
- chat 列表没变，飞书历史完整

---

## 4. 核心设计哲学

### 4.1 完全透传，不解析
nightme **不解析** CLI 输出，不识别"成功 / 失败 / TODO"，不拆分任务、不合并结果、不主动建议。它就是一个 byte pipe。

**包括敏感内容**：密码、API key、token 也走透传。用户从 IM 输入，nightme 原样转给 PTY stdin，不做任何过滤、重定向、检测。代价是密码会进入 IM 聊天记录——这是透传的必然结果。

**为什么**：让用户感觉 agent 在"另一个终端"运行，不是被 nightme 过滤过的版本。透明带来信任。多做一层"密码检测"等于违背整个 nightme 的存在意义。

### 4.2 模拟 TTY，让 CLI 无感
nightme **不调** Claude Code 的 non-interactive 模式（`--print` / `-p`）。它必须用 PTY 启动 CLI，让 CLI 以为自己跑在真正的终端里——这样颜色、进度条、交互 prompt 全部正常。

**为什么**：用户体验跟 macOS 终端一致 = 用户对结果有信心。

### 4.3 Chat = ChatSession（一个 ChatSession 承载多个 AgentSession）
每个 IM chat 绑定一个 **ChatSession**——它是该 chat 的**持久会话上下文**（跨 daemon 重启、跨 CLI 进程死亡）。ChatSession 持有：

- 当前活跃工作目录（`/cwd` 设）
- 当前活跃 agent 类型（`/use <agent>` 设）
- 一个 AgentSession 池

**AgentSession** 是 ChatSession 池里的一项，对应一对 `(agent, cwd)` 组合，是实际跑 CLI 子进程（Claude Code / Codex / OpenCode）的会话句柄。

**关键规则**：
- **1 chat = 1 ChatSession**（永久绑定，跨重启）
- **ChatSession 内可有多个 AgentSession**（每个 `(agent, cwd)` 一份）
- **同一时刻只有 1 个 active AgentSession** 在被推送/接收
- **切换 `/cwd` 或 `/use` 不杀任何 AgentSession**——只是改 ChatSession 的 activeCwd / activeAgent 和消息推送目标
- **切回原 cwd/agent 时复用之前的 AgentSession**（保持进程和对话上下文）

**不支持**（v1.2 范围内）：
- 同一 chat 内同时跑多个 AgentSession（并行协作场景放 v0.4+）

**为什么这样设计**：
- **Chat 仍是项目边界**（飞书 DM 列表 = 项目列表）—— 用户认知不变
- **持久 ChatSession** 让对话历史 = 飞书聊天历史 + ChatSession 状态（无需 nightme 持久化完整消息流）
- **AgentSession 池化** 让用户能"切去 codex 看个东西，再切回 claude 继续"，不丢失任何 agent 的上下文
- **active 单一** 保持心智模型简单：现在跟谁说话，一目了然

### 4.4 进程归属 = nightme 自启动
nightme **只控制自己启动的进程**。用户的 bash / zsh / vscode / 其他手启动的进程，完全不管。

**为什么**：跟"做一件事做到极致"的原则一致。nightme 不接管用户电脑，它只管理自己创建的 agent 进程。

### 4.5 Minimal 原则
v0.1 MVP **只做一件事**：从 IM 转发文本到 PTY stdin，再把 PTY 输出回推给 IM。文件、图片、语音、按钮、卡片全部放后期。

**为什么**：先把最核心的"打字 → agent 看见 → agent 回答 → 用户看见"链路跑通，其他都是装饰。

**v1.2 的扩展**：在 MVP 之上加 ChatSession 模型（架构层），但**产品语义保持简单**——1 个 active AgentSession、可切换但不并行、不支持跨 chat 共享。ChatSession 的"魔法"对用户透明。

### 4.6 ChatSession vs AgentSession — 分层的意义（v1.2 新增）
v1.1 之前的 nightme 里"Session" = CLI 进程，session 死了就没了。v1.2 把"会话上下文"和"CLI 进程"分开：

- **ChatSession**（产品概念）= "我跟这个 chat 的会话"，由 nightme 持久化
- **AgentSession**（技术实现）= 一个 CLI 进程的会话句柄，由 ChatSession 池化保留

**用户感知**：
- ChatSession 是**透明的**——用户不需要知道它存在。飞书 DM 看起来还是"会话历史"
- AgentSession 是**显式的**——用户用 `/use claude` / `/use codex` 切换
- 切换的"魔法"在 ChatSession 内部完成：复用旧进程、保留上下文

**为什么这样分层**：
- **简单心智**：chat ↔ 会话历史（不变）
- **灵活能力**：agent 切换 / 进程复用（v1.2 新增）
- **进程归属不变**（v1.1 §4.4 不变）：nightme 仍只管自己启动的进程

---

## 5. 功能范围

完整功能列表（F-01 ~ F-29）见 [`FEATURES.md`](./FEATURES.md)。每个功能的设计细节见 [`feat/`](./feat/)。

**v1.2 新增**（PRD v1.2 → SPEC v1.2 锁定）：
- **ChatSession 模型**（F-27）：ChatSession 取代 v1.1 的 Session，作为 chat ↔ AgentSession 间的会话上下文
- **`/use <agent>` 命令**（F-28）：切换 ChatSession 的 activeAgent；复用或新建 AgentSession
- **AgentSession 池**（F-29）：ChatSession 内 `(agent, cwd)` 1:1 池化；切换 cwd/agent 不杀进程

**MVP（v0.1）已发布**：F-01 ~ F-10, F-19 ~ F-22。
**v0.2 → v0.3 增量**：F-23（心跳）、F-24（Claude Code Bridge）、F-25（Input Buffer）、F-26（v1.1 职责隔离架构）。

**v1.2 范围外**（明确不做）：
- 多 AgentSession 并行协作（v0.4+）
- 跨 chat 共享 ChatSession（保留 Channel 隔离）
- AgentSession 跨 ChatSession 共享（每个 chat 独立进程池）

---

## 6. 范围外（明确不做）

nightme 明确**不**做以下事情：

| ❌ 不做 | 原因 |
|---------|------|
| LLM 编排（task decomposition）| 透传原则 |
| Code review / agent quality scoring | 透传原则 |
| 接管用户已有的 shell / terminal multiplexer | 进程归属原则 |
| Multi-user RBAC | 单用户假设 |
| 云端 SaaS | 用户电脑始终是唯一运行环境 |
| 写底层 AI Coding Agent 的 prompt / system message | 透传原则 |
| 主动补全 / 联想 / 模板化建议 | 透传原则 |
| 项目的 git / 文件系统扫描 | nightme 不关心项目结构 |
| 多 AgentSession 并行协作（v1.2 范围外）| ChatSession active 单一原则（v1.2 §4.3）|
| 跨 chat 共享 ChatSession（v1.2 范围外）| Channel 隔离原则 |

---

## 7. 成功标准

nightme v1.2 发布时，以下场景必须能跑通：

1. ✅ 用户从飞书 DM 创建 ChatSession，workspace 验证生效，agent 启动
2. ✅ 用户在飞书发的每条消息 ≤ 200ms 到达 agent stdin
3. ✅ agent 的输出聚合后 ≤ 1s 出现在飞书 DM
4. ✅ agent 正常 / 异常退出后，nightme 推送 "session ended" 给用户
5. ✅ nightme 重启后，已存在的 ChatSession 自动 reattach
6. ✅ `nightme list` 命令能列出所有 ChatSession（含状态、workspace、pid、active agent）
7. ✅ 单 laptop 跑 5+ 并发 ChatSession，资源占用 < 100MB
8. ✅ 用户的非 nightme 启动的进程不受任何影响
9. ✅（v1.2 新增）`/use codex` 切到 codex 后 `/use claude` 切回，claude 进程是同一个（对话上下文保留）
10. ✅（v1.2 新增）`/cwd /path/B` 切到新 cwd，原 `(claude, /path/A)` AgentSession 不被杀，再 `/cwd /path/A` 能接回

---

## 8. 更新日志

- **v1.0**：锁定 6 项关键决策 + Chat↔Session 1:1 模型 + Minimal MVP 范围
- **v1.0r**：从 SPEC.md 分离，独立维护产品级文档
- **v1.2**：Chat=Session 1:1 → **Chat=ChatSession** 模型；新增 AgentSession 池 + `/use` 命令；产品语义锁定，架构在 SPEC v1.2
  - **状态**：**已锁定**（2026-08-02；Q-A ✅ 全局 Default only；Q-B ✅ exact → default fallback → spawn；落地 commits 5/6/7/8a/8b/8c/9）