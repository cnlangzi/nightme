# nightme — Implementation Brief

> **状态**：v1.0
> **作者**：🦞 虾哥（PM/Architect）
> **日期**：2026-07-31
> **依赖**：`PRD.md`、`architecture.md`、`cli-bridge.md`

本文档回答：**怎么把 nightme 从 0 写到能跑通的最小闭环，分几个里程碑，第一个 PR 怎么拆 commit**。

---

## 1. 总体里程碑

```
M0 (本轮对话)  ──  docs only, no code yet
  PRD + architecture + cli-bridge + IMPLEMENTATION (本文)
  验收: 4 个 md 文件 commit，Devin review 通过

M1 (下一个 session)  ──  "Local PTY Echo"（本地能跑，无 Channel）
  目标: `nightme demo <workspace>` 命令启动一个 claude 进程并打印其输出到 stdout
  不接飞书，纯粹验证 PTY bridge + 进程管理

M2  ──  "Feishu Round-Trip MVP"
  目标: 飞书 DM → nightme → 本地 claude → 飞书 DM 回包
  第一个端到端可用的版本

M3  ──  "Hardening"
  目标: 错误处理 + 重连 + 进程清理 + 文档 + 第一个 release
```

**M1 → M2 是最大的跳跃**（从本地 CLI 工具变成 IM-driven daemon）。M1 必须稳，否则 M2 全是飞书相关调试，看不清根因。

---

## 2. M1: Local PTY Echo

### 2.1 目标

```
$ nightme demo /tmp/myproject --agent claude
[nightme] session s_01H created in /tmp/myproject
[nightme] PTY spawned, pid=12345
> User input here (Ctrl+D to exit):
hello claude
[claude stdout] Hello! How can I help?
...
```

### 2.2 范围

- ✅ Go module + 目录骨架
- ✅ Config 加载（YAML）
- ✅ PTY bridge 封装（基于 aymanbagabas/go-pty）
- ✅ Session 数据结构 + Manager（内存版，registry 暂不持久化）
- ✅ Process registry（内存 + JSON dump）
- ✅ CLI 命令：`nightme demo <ws> [--agent]`
- ✅ 简单的 read/write loop（stdin ↔ PTY，PTY ↔ stdout）
- ✅ SIGINT 优雅退出（不杀 PTY 子进程，符合默认策略）
- ❌ 飞书（留给 M2）
- ❌ 多 session 并发（M2 加）

### 2.3 文件结构

```
nightme/
├── go.mod
├── go.sum
├── README.md
├── docs/
│   ├── PRD.md                  (v1.0)
│   ├── architecture.md         (v1.0)
│   ├── cli-bridge.md           (v1.0)
│   └── IMPLEMENTATION.md       (v1.0, 本文件)
├── configs/
│   └── nightme.example.yaml
├── cmd/
│   └── nightme/
│       └── main.go              # 入口，解析 subcommand
└── internal/
    ├── config/
    │   └── config.go            # YAML 加载
    ├── agent/
    │   ├── agent.go             # Agent interface
    │   ├── registry.go          # 注册表
    │   └── claude/
    │       └── claude.go        # Claude Code impl
    ├── session/
    │   ├── session.go           # Session struct
    │   ├── manager.go           # Manager (memory only in M1)
    │   └── router.go            # M2 再加（chat_id → session）
    ├── pty/
    │   ├── bridge.go            # aymanbagabas 封装
    │   ├── aggregator.go        # 200ms / 4KB 聚合（M1 简化版）
    │   └── ansi.go              # ANSI 处理 stub
    └── registry/
        └── registry.go          # JSON 持久化
```

### 2.4 第一个 PR 的 Commit 拆分

**PR #1: scaffold + config + agent**（5 commits）

```
commit 1: chore: initial go module + directory skeleton
  - go mod init github.com/cnlangzi/nightme
  - 创建 internal/ 目录结构
  - README + .gitignore 更新

commit 2: feat(agent): Agent interface + Claude Code impl
  - internal/agent/agent.go: interface
  - internal/agent/claude/claude.go: 默认实现
  - internal/agent/registry.go: 注册表
  - 单测：agent name / command 正确

commit 3: feat(config): YAML config loader
  - internal/config/config.go
  - 默认值 fallback
  - 环境变量覆盖 (NIGHTME_*)
  - 单测：load / merge / override

commit 4: feat(registry): JSON-backed process registry
  - internal/registry/registry.go
  - Upsert / Get / Delete / List
  - 文件权限 0600
  - 单测：tmpdir 下读写一致
```

**PR #2: pty bridge + session manager**（4 commits）

```
commit 5: feat(pty): bridge wrapping aymanbagabas/go-pty
  - internal/pty/bridge.go
  - Setsize / Read / Write / Close / PID
  - 单测：spawn /bin/echo 验证输出

commit 6: feat(pty): output aggregator (200ms / 4KB)
  - internal/pty/aggregator.go
  - buffer + timer flush
  - 单测：size threshold + timer threshold

commit 7: feat(session): Session + Manager (memory)
  - internal/session/session.go
  - internal/session/manager.go: Create / Get / List / Kill
  - 集成测试：Create + read + Close

commit 8: feat(registry): persist session map to JSON
  - SessionManager 集成 registry
  - Upsert on Create, Delete on cleanup
  - 集成测试：restart 后能 Load
```

**PR #3: cli + e2e (local)**（3 commits）

```
commit 9: feat(cmd): nightme demo <workspace> --agent <name>
  - cmd/nightme/main.go: cobra 替代 stub
  - internal/cli/demo.go: 启动 session + I/O loop
  - 集成测试：手动 spawn claude 不实际跑（用 /bin/echo）

commit 10: docs: README quickstart + demo usage
  - 编译步骤
  - 运行示例
  - 配置文件说明

commit 11: chore: v0.1.0 tag + release notes
  - git tag v0.1.0
  - GitHub release notes 草稿
```

### 2.5 验收标准

| 项 | 标准 |
|----|------|
| `go build ./...` | 0 error |
| `go test ./...` | 全绿，coverage > 70% |
| `go vet ./...` | 0 warning |
| `golangci-lint run` | 0 issue |
| `nightme demo /tmp/foo --agent echo` | 能 spawn /bin/echo 并打印其输出 |
| 跨平台编译 | macOS + Linux 各产出一个二进制，单文件 < 15MB |
| 进程归属 | `ps --ppid <nightme-pid>` 能看到所有 session CLI |
| 优雅退出 | Ctrl+C 后 nightme 退出，session CLI **仍存活** |
| Registry | `cat registry.json` 能看到当前所有 session |

### 2.6 风险 & 备选

| 风险 | 备选 |
|------|------|
| `aymanbagabas/go-pty` 在某 OS 上编译失败 | 切 `creack/pty`（~30 行 wrapper 改动） |
| Claude Code CLI 在测试环境没装 | M1 用 `/bin/echo` 验证 PTY bridge，claude 集成留 M2 |
| ANSI 转义符导致终端显示问题 | M1 简单 strip（v0.1 策略），M2 优化 |

---

## 3. M2: Feishu Round-Trip

### 3.1 目标

飞书 DM 中发 "workspace: /tmp/myproject" → nightme 创建 session → 后续消息透传 → claude 输出回飞书。

### 3.2 范围

- ✅ 飞书 SDK 集成（lark-oapi-go）
- ✅ `Channel` interface + `feishu` adapter
- ✅ Router：chat_id → session_id 路由
- ✅ 新 chat 首条消息识别 "workspace: <path>"
- ✅ Workspace 路径验证
- ✅ Session CLI 自动 spawn
- ✅ Output 推到飞书（SendLongMessage 处理分段）
- ✅ Logger（slog + JSON file）
- ❌ 图片 / 文件 / 富文本（v0.2）
- ❌ Web TTY UI（v0.2）

### 3.3 第一个飞书 demo commit 拆分

**PR #4: channel + feishu**（4 commits）

```
commit 12: feat(channel): Channel interface + mock impl
  - internal/channel/channel.go
  - internal/channel/mock/mock.go (测试用)
  - 单测：mock 行为

commit 13: feat(channel): feishu adapter skeleton
  - internal/channel/feishu/feishu.go
  - lark-oapi-go 长连接启动
  - Incoming / SendMessage 接口实现
  - 单测：mock SDK 验证消息路由

commit 14: feat(session): chat_id → session routing
  - internal/session/router.go
  - Lookup(chat_id) → session_id | ErrNewChat
  - 集成测试：mock channel 触发 session 创建

commit 15: feat(cmd): nightme run (daemon mode)
  - cmd/nightme/run.go
  - 启动 channel + manager + signal handling
  - 配置加载 + logger 初始化
```

**PR #5: workspace binding + message loop**（3 commits）

```
commit 16: feat(session): workspace validation + auto-create
  - workspace path 存在性检查
  - 首条消息 "workspace: <path>" 解析
  - 错误回复（workspace 不存在 / agent 找不到）

commit 17: feat(bridge): end-to-end message loop
  - Channel.Incoming → session.bridge.Write
  - session.bridge.Read → Channel.SendLongMessage
  - Aggregator 集成
  - 集成测试：spawn mock agent + verify round-trip

commit 18: docs: e2e manual test guide
  - docs/E2E_TESTING.md
  - 飞书 app 配置步骤
  - 跑通第一个 demo 的 step-by-step
```

### 3.4 验收标准（M2）

| 项 | 标准 |
|----|------|
| 飞书 DM 首条 "workspace: /tmp/foo" | 收到 "Session started in /tmp/foo" |
| 飞书 DM 后续消息 | claude 进程 stdin 收到（PTY 检测得到） |
| claude 输出 | 飞书 DM 收到（聚合 200ms 内） |
| claude 退出 | 飞书 DM 收到 "Session ended" |
| nightme 重启 | 已存在的 session 自动 reattach（如 PID 活） |
| 错误处理 | workspace 不存在 / agent 找不到 → 友好提示 |
| `nightme list` | 能列出所有 session（chat_id / ws / pid / started_at）|

### 3.5 飞书配置前置

**Devin 需要提前准备**（不在虾哥交付范围内）：
1. 飞书开放平台 → 创建企业自建应用
2. 获取 app_id + app_secret
3. 配置机器人能力
4. 配置事件订阅 URL（nightme 的 webhook，v0.1 用 WebSocket 不需要）
5. 把 app 添加到 IM 联系人

虾哥给一个 `configs/nightme.example.yaml` 模板，Devin 填好 credentials。

---

## 4. M3: Hardening + v1.0

### 4.1 范围

- 错误处理边界（所有 panic 兜底）
- 日志脱敏（app_secret / API key）
- SIGTERM 优雅退出（标记 detached，不杀 CLI）
- `--cleanup` flag 支持 kill 所有 session
- 文档完善（README + docs/）
- GitHub Actions CI（go test / go vet / build matrix）
- v1.0.0 release + binary 发布

### 4.2 不在 M3 范围（留给 v0.2）

- 图片 / 文件透传（F-14）
- 终端 resize（F-13）
- 多 Channel mirror（F-11）
- WhatsApp / Telegram（F-12）
- Web TTY UI（F-16）
- 健康检查（F-17）
- 密码注入（F-18）
- Session 历史持久化（F-15）

---

## 5. 时间预估（虾哥节奏）

| Milestone | 预估 session 数 | 预估代码量 |
|-----------|----------------|------------|
| M1 | 2-3 sessions | ~800 行 Go |
| M2 | 3-4 sessions | ~1200 行 Go |
| M3 | 1-2 sessions | ~300 行 Go + docs |

每个 session 内：
- 读 docs → 写 commit → 跑测试 → 汇报 → 等用户反馈
- 不批量 commit，**每个 commit 都要验收**

---

## 6. 与 OpenClaw gtw 集成（可选）

如果用户想用 OpenClaw 的 gtw plugin 管理 nightme 的 issue / PR：
- 仓库地址：`github.com/cnlangzi/nightme`
- gtw 配置：`/gtw on ~/code/nightme`
- issue label：复用 `gtw/ready | gtw/wip | gtw/lgtm | gtw/revise | gtw/stuck`
- commit message：英文（nightme 看起来是开源项目，README.md 是英文）

但这不强制，Devin 可以用 git + gh 命令行手管。

---

## 7. 下一步

**当前会话（M0）即将结束**。已交付：
- ✅ `docs/PRD.md` v1.0
- ✅ `docs/architecture.md` v1.0
- ✅ `docs/cli-bridge.md` v1.0
- ✅ `docs/IMPLEMENTATION.md` v1.0（本文）

**等 Devin 确认后开始 M1**。M1 的触发词：
- "开始 M1" / "go" / "开干"
- 或 "先帮我 review docs"（虾哥等反馈）
