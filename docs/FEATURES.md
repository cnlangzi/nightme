# nightme — Functional Features

> **状态**：v1.0
> **作者**：🦞 虾哥（PM/Architect）
> **日期**：2026-07-31
> **依赖**：`SPEC.md`、`architecture.md`、`cli-bridge.md`
> **变更记录**：从 SPEC.md v1.0 抽取，独立维护

本文档是 nightme 的**功能清单**。SPEC.md 回答"nightme 是什么 / 怎么做"，本文档回答"nightme 能做什么 / 每项功能怎么验收"。

按版本切片：
- **F-1 ~ F-10**：MVP（v0.1，必须实现）
- **F-11 ~ F-18**：v0.2+（后续迭代）

---

## 1. MVP 功能（v0.1）

### F-1 Session 创建
- **描述**：用户在 Chat 首条消息写 `workspace: /path/to/project`，nightme 验证后创建 session 并 spawn CLI
- **触发**：用户在任意 Chat（DM/group/thread）的**第一条**消息以 `workspace:` 开头
- **验收**：
  - workspace 路径必须存在（绝对路径）
  - 默认 agent = claude（v0.1）
  - spawn 成功后回复 "Session started in {workspace}"
  - 失败时回复明确错误（"workspace does not exist" / "claude not found"）
- **依赖**：F-4（PTY 模拟）、F-7（Workspace 绑定）、F-8/F-9（Channel/Agent 抽象）

### F-2 消息透传（Channel → PTY）
- **描述**：用户在 Chat 里的输入 → 该 Chat 绑定 session 的 PTY stdin
- **验收**：
  - 用户发纯文本 → claude 进程 stdin 收到同样的字节 + `\n`（如果用户没自带）
  - 端到端延迟 < 200ms（P50）
  - 不破坏 IM 富文本（@ / emoji 处理见 cli-bridge.md 第 4 节）

### F-3 输出推送（PTY → Channel）
- **描述**：session 的 PTY stdout/stderr → 该 Chat
- **验收**：
  - claude 输出经 200ms / 4KB 聚合后推送（详见 cli-bridge.md 第 3 节）
  - > 4KB 自动分段（飞书单条上限）
  - ANSI 转义码 v0.1 原样透传（用户可能在飞书看到 `^[[32m` 等字面量，v0.2 优化）
  - 端到端延迟 < 1s（P50 文本块）

### F-4 PTY 模拟
- **描述**：spawn CLI 时使用 PTY，cols/lines 由 nightme 配置
- **实现**：`aymanbagabas/go-pty`
- **默认值**：cols=120, rows=40（用户可配置）
- **验收**：
  - claude 进程能正确检测到自己运行在 TTY（颜色、进度条正常）
  - PTY 进程由 nightme fork，ppid 校验通过

### F-5 进程注册
- **描述**：nightme 启动的所有 CLI 进程写入本地 registry 文件
- **位置**：`~/.local/share/nightme/registry.json`（用户可配置）
- **格式**：
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
- **验收**：
  - session 创建后立即 fsync 写入
  - 文件权限 0600
  - 重启 nightme 后能读回已存在的 session

### F-6 进程清理
- **描述**：nightme 退出时（SIGTERM/SIGINT），按策略清理自己启动的进程
- **默认策略**：**不 kill** 子进程，session 标记 "detached"
- **可选策略**：`nightme --cleanup` 启动标志位 → 杀所有 session CLI（SIGTERM → 5s → SIGKILL）
- **验收**：
  - 默认：nightme 退出后 `ps` 仍能看到 session CLI 在跑
  - cleanup：nightme 启动时杀掉所有之前 session CLI（孤儿清理）

### F-7 Workspace 绑定
- **描述**：CLI 启动时 `cwd = session.workspace`
- **验收**：
  - workspace 路径不存在 → 拒绝创建 session，提示用户
  - workspace 存在 → claude 进程的 cwd 是该路径（`/proc/<pid>/cwd` 验证）

### F-8 Channel 抽象
- **描述**：Channel 是 interface，MVP 实现 Feishu，结构上留位给其他 IM
- **interface 位置**：`internal/channel/channel.go`
- **MVP 实现**：`internal/channel/feishu/feishu.go`（基于 lark-oapi-go）
- **验收**：
  - Channel interface 有清晰的 SendMessage / SendLongMessage / Incoming 三个方法
  - 飞书实现通过抽象接口注入到 SessionManager
  - 添加新 IM 只需新写一个 adapter，无需改其他模块

### F-9 Agent 抽象
- **描述**：Agent 是 interface，MVP 实现 Claude Code，结构上留位给 Codex/OpenCode
- **interface 位置**：`internal/agent/agent.go`
- **MVP 实现**：`internal/agent/claude/claude.go`（Command="claude"）
- **验收**：
  - Agent interface 有 Name() / Command() / Args() / Env() 四个方法
  - Claude Code 通过 registry 注册到 Agent 列表
  - 添加新 agent（如 codex）只需新写一个 adapter

### F-10 Session 列表（管理命令）
- **描述**：用户用 `nightme list` 命令行查所有 session 状态
- **输出**：
  ```
  SID         AGENT   WORKSPACE                          PID    STARTED
  s_01HF8...  claude  /home/devin/code/bailing           12345  10:30:00
  s_01HF9...  claude  /home/devin/code/nightme           12350  10:35:12
  ```
- **验收**：
  - 通过本地 HTTP API（127.0.0.1:7823）查询
  - 默认从 registry.json 读取
  - 列表包含 detached 状态的 session（未运行但有记录的）

---

## 2. 后续功能（v0.2+）

### F-11 多 Channel mirror 模式
- **描述**：多个 Channel 同时 attach 到一个 session
- **场景**：用户想在飞书 + Web UI 同时看同一个 claude 输出
- **验收**：
  - 一个 session 的 PTY 输出广播到多个 chat
  - 任意 channel 的输入都进入同一个 PTY stdin

### F-12 多 IM adapter
- **描述**：WhatsApp / Telegram / Slack / Web UI Channel adapter
- **优先级**：WhatsApp > Telegram > Slack > Web UI
- **验收**：每个 adapter 都通过 `Channel` interface 测试

### F-13 终端大小调整
- **描述**：用户手机横竖屏切换 → PTY SIGWINCH
- **验收**：
  - Channel adapter 检测屏幕方向（mobile）或浏览器 resize（web）
  - 调用 `pty.Setsize(cols, rows)` 通知 CLI
  - claude 等 TUI 应用能正确响应

### F-14 图片 / 文件附件透传
- **描述**：Channel 收到的图片 / 文件 → PTY（粘贴或写文件）
- **验收**：
  - 飞书图片 → nightme 下载 → 保存到 workspace 临时目录 → 写 "file://" 路径到 PTY
  - claude 进程能引用（取决于 agent 是否支持）

### F-15 Session 持久化
- **描述**：nightme 重启后，已结束的 session 可恢复 stdout 历史
- **验收**：
  - session 退出后 stdout 保留到 disk（截断滚动，如最后 1MB）
  - 重启后 `nightme history <sid>` 可查

### F-16 Web TTY UI
- **描述**：浏览器实时看 + 操作 session PTY
- **实现**：基于 xterm.js + WebSocket
- **验收**：
  - 浏览器看到 ANSI 正确渲染
  - 浏览器输入直接进 PTY stdin
  - 与 Channel adapter 并行不冲突

### F-17 健康检查 / 心跳
- **描述**：session 失联自动告警
- **验收**：
  - PTY 进程死亡 → Channel 推送 "session ended" 消息
  - Channel 长连接断 → 5s 内重连，重连失败告警

### F-18 Token / API key 注入
- **描述**：避免在 channel 里裸露 API key / 密码
- **场景**：
  - claude 需要输入 API key（首次配置）
  - claude 进入 hidden input 模式（密码）
- **验收**：
  - nightme 检测到 "hidden input" 模式 → 弹出飞书 input card
  - 用户输入走加密通道，**不**写入飞书聊天记录

---

## 3. 删除项（已从 v0.1 移除）

> 以下功能在 PRD 早期版本中存在，因 Chat↔Session 1:1 模型而删除

- ❌ **Session 列表 in Channel**：用户在新 Chat 首条消息触发 session 创建，无需在 Chat 内查询 session 列表（F-10 命令行版仍保留）
- ❌ **Session 切换 `/attach <sid>`**：因 Chat↔Session 1:1 绑定，切换 = 开新 Chat

---

## 4. 验收总览（v0.1 release checklist）

| ID | 必须 | 验收方式 |
|----|------|----------|
| F-1 | ✅ | E2E：飞书 DM 首条消息触发 session |
| F-2 | ✅ | E2E：飞书消息 → claude stdin |
| F-3 | ✅ | E2E：claude 输出 → 飞书 DM |
| F-4 | ✅ | 单元：spawn `/bin/echo` 验证 PTY |
| F-5 | ✅ | 单元：registry.json 读写一致性 |
| F-6 | ✅ | 单元：默认策略不杀 CLI；cleanup 标志位杀 |
| F-7 | ✅ | 单元：cwd 校验 |
| F-8 | ✅ | 单元：mock Channel 实现跑通 SessionManager |
| F-9 | ✅ | 单元：mock Agent 实现跑通 Session |
| F-10 | ✅ | 单元：`nightme list` 输出格式正确 |

**全部勾选后**，nightme v0.1.0 可发布。
