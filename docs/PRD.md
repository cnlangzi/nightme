# nightme — Product Requirements Document (PRD)

> **状态**：v1.0（已锁定）
> **作者**：🦞 虾哥（PM/Architect）
> **日期**：2026-07-31
> **文档层级**：产品级（**不含技术内容**）
> **关联文档**：
> - 技术架构 → [`SPEC.md`](./SPEC.md)
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
nightme · bailing        ← session A，workspace ~/code/bailing
nightme · nightme        ← session B，workspace ~/code/nightme
nightme · side-project   ← session C，workspace ~/code/side
```
每个 DM = 一个独立 Claude Code session = 一个项目。飞书侧 DM 列表天然成为项目列表。

### 3.3 夜间让 agent 跑长任务
- 晚上 11 点：发任务 "把 bailing 的字幕系统改成 VTT"
- 睡觉
- 早上醒来查 DM，看到 Claude Code 的工作进度 + 文件改动

### 3.4 应急修复
- 周末出门在外，线上服务报警
- DM nightme · production-tools → slash command 设置 cwd 到 ~/tools/prod
- 通过 Claude Code 排查问题（agent 有完整的工具 + 上下文）

---

## 4. 核心设计哲学

### 4.1 完全透传，不解析
nightme **不解析** CLI 输出，不识别"成功 / 失败 / TODO"，不拆分任务、不合并结果、不主动建议。它就是一个 byte pipe。

**包括敏感内容**：密码、API key、token 也走透传。用户从 IM 输入，nightme 原样转给 PTY stdin，不做任何过滤、重定向、检测。代价是密码会进入 IM 聊天记录——这是透传的必然结果。

**为什么**：让用户感觉 agent 在"另一个终端"运行，不是被 nightme 过滤过的版本。透明带来信任。多做一层"密码检测"等于违背整个 nightme 的存在意义。

### 4.2 模拟 TTY，让 CLI 无感
nightme **不调** Claude Code 的 non-interactive 模式（`--print` / `-p`）。它必须用 PTY 启动 CLI，让 CLI 以为自己跑在真正的终端里——这样颜色、进度条、交互 prompt 全部正常。

**为什么**：用户体验跟 macOS 终端一致 = 用户对结果有信心。

### 4.3 Chat = Session = Project
每个 IM chat 绑定一个 session，一个 session 锁定一个 workspace。**不支持**在同一 chat 内切换项目。

**为什么**：切换项目 = 开新 DM = 飞书侧天然的项目上下文边界。用户切项目时**零认知负担**，飞书的聊天历史 = 项目历史，无需 nightme 持久化。

### 4.4 进程归属 = nightme 自启动
nightme **只控制自己启动的进程**。用户的 bash / zsh / vscode / 其他手启动的进程，完全不管。

**为什么**：跟"做一件事做到极致"的原则一致。nightme 不接管用户电脑，它只管理自己创建的 agent 进程。

### 4.5 Minimal 原则
第一版**只做一件事**：从 IM 转发文本到 PTY stdin，再把 PTY 输出回推给 IM。文件、图片、语音、按钮、卡片全部放后期。

**为什么**：先把最核心的"打字 → agent 看见 → agent 回答 → 用户看见"链路跑通，其他都是装饰。

---

## 5. 功能范围

完整功能列表（F-01 ~ F-20）见 [`FEATURES.md`](./FEATURES.md)。每个功能的设计细节见 [`feat/`](./feat/)。

**MVP（v0.1）**：
- F-01 Session 生命周期（`/cwd` 设 workspace + `/run` 启 CLI 两步式；workspace 持久，CLI 可重启）
- F-02 消息透传（IM → PTY stdin）
- F-03 输出推送（PTY stdout → IM）
- F-04 PTY 模拟
- F-05 进程注册（持久化）
- F-06 进程清理（默认 detach）
- F-07 Workspace 绑定
- F-08 Channel 抽象（飞书 MVP）
- F-09 Agent 抽象（Claude Code MVP）
- F-10 Session 列表命令
- F-19 PTY Mode Byte Pipe（Bridge 的 PTY 实现；ACP/SDK 见 F-21）
- F-20 Command Gateway（slash command 路由）

**v0.2+ 后续**：F-11 ~ F-17（多 Channel、附件、resize、Web UI、健康检查等；F-18 已取消，见 §4.1）

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

---

## 7. 成功标准

nightme v1.0 发布时，以下场景必须能跑通：

1. ✅ 用户从飞书 DM 创建 session，workspace 验证生效，agent 启动
2. ✅ 用户在飞书发的每条消息 ≤ 200ms 到达 agent stdin
3. ✅ agent 的输出聚合后 ≤ 1s 出现在飞书 DM
4. ✅ agent 正常 / 异常退出后，nightme 推送 "session ended" 给用户
5. ✅ nightme 重启后，已存在的 session 自动 reattach
6. ✅ `nightme list` 命令能列出所有 session（含状态、workspace、pid）
7. ✅ 单 laptop 跑 5+ 并发 session，资源占用 < 100MB
8. ✅ 用户的非 nightme 启动的进程不受任何影响

---

## 8. 更新日志

- **v1.0**：锁定 6 项关键决策 + Chat↔Session 1:1 模型 + Minimal MVP 范围
- **v1.0r**：从 SPEC.md 分离，独立维护产品级文档
