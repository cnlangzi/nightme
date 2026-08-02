# nightme — Implementation Plan (PLAN)

> **状态**：v2.0（重写：适配最新架构）
> **作者**：🦞 虾哥（PM/Architect）
> **日期**：2026-07-31
> **关联文档**：
> - 产品 → [`PRD.md`](./PRD.md)
> - 技术架构 → [`SPEC.md`](./SPEC.md)
> - 功能索引 → [`FEATURES.md`](./FEATURES.md)
> - 每个 feature 的详细设计 → [`feat/`](./feat/)

本文档回答：**怎么把 nightme 从 0 写到能跑通的最小闭环，分几个里程碑，每个 milestone 怎么拆 commit**。

---

## 1. 总体里程碑

```
M0 (已完成)  ──  docs only, no code yet
  PRD + SPEC + FEATURES + feat/F-01..F-21 + PLAN (本文)
  验收: 22 个 md 文件 commit，Devin review 通过
  8 commits: docs only

M1 (下一个 session)  ──  "Local Bridge"（PTY 模式能跑，无 Channel）
  目标: 实现 Bridge 抽象 + PTY 后端 + Session Manager + Registry
       `nightme test --workspace <path> --agent <name>` 命令能启动 CLI
  不接飞书，纯粹验证 Bridge/Session/Registry 链路
  不实现 Gateway（留给 M2）

M2  ──  "Feishu Round-Trip MVP"（ACP 模式 + Gateway）
  目标: 飞书 DM → nightme → Codex (via ACP) → 飞书 DM 回包
  包含: Gateway (/cwd /run /kill /help) + ACP adapter + Channel
  Claude Code 暂时走 PTY（Anthropic 不支持 ACP）

M3  ──  "Hardening + v0.1 release"
  目标: 错误处理 + 重连 + 进程清理 + CI + 文档 + v0.1 release

v0.2  ──  Claude Code 专用 bridge（F-24）+ 心跳/streaming status（F-23）
v0.3+ ──  更多 CLI 支持、终端 resize、SDK 等
```

**M0 → M1 → M2 → M3 是主要工作**。M1 必须稳（架构骨架），M2 加 IM 链路（最大跳跃），M3 是 production-readiness。

---

## 2. M1: Local Bridge（PTY 模式能跑）

### 2.1 目标

```
$ nightme test --workspace /tmp/myproject --agent codex
[nightme] session s_01H created in /tmp/myproject
[nightme] PTY spawned, pid=12345
> User input here (Ctrl+D to exit):
hello codex
[codex stdout] Hello! How can I help?
...
```

注：`nightme test` 是 M1 临时命令（直接 CLI 参数，无 IM）。M2 改成 `/cwd` + `/run` slash command via 飞书。

### 2.2 范围

- ✅ Go module + 目录骨架
- ✅ Config 加载（YAML）
- ✅ **Agent 接口（统一抽象）**：`Mode()` + `Start() (AgentSession, error)`
- ✅ **AgentSession 接口**：Events / SendText / SendPermission / Close
- ✅ **PTY backend** (`internal/bridge/pty/`)：aymanbagabas/go-pty 封装
- ✅ **ACP backend stub** (`internal/bridge/acp/`)：v0.1 留接口，M2 实施
- ✅ **SDK backend stub** (`internal/bridge/sdk/`)：v0.2 才实施
- ✅ **Session 数据结构** + **SessionManager**
- ✅ **Process Registry**（JSON 持久化）
- ✅ CLI 命令：`nightme test <ws> --agent <name>`
- ✅ SIGINT 优雅退出（默认 detach，session 保留）
- ❌ Gateway（留给 M2）
- ❌ Channel（留给 M2）
- ❌ 飞书 / IM（留给 M2）

### 2.3 文件结构

```
nightme/
├── go.mod
├── go.sum
├── README.md
├── docs/
│   ├── PRD.md
│   ├── SPEC.md
│   ├── FEATURES.md
│   ├── PLAN.md                 (本文)
│   └── feat/                   (21 个 F-XX 详细设计)
├── configs/
│   └── nightme.example.yaml
├── cmd/
│   └── nightme/
│       └── main.go              # 入口
└── internal/
    ├── config/
    │   └── config.go            # YAML 加载
    ├── agent/
    │   ├── agent.go             # Agent / AgentSession / Event 接口
    │   └── registry.go          # agent 注册表
    ├── bridge/                  # ← 新命名（替代原 internal/pty）
    │   ├── acp/
    │   │   ├── acp.go           # ACP client（v0.1 stub，v0.2 实施）
    │   │   └── acp_test.go
    │   ├── pty/
    │   │   ├── pty.go           # aymanbagabas 封装
    │   │   ├── aggregator.go    # 200ms / 4KB 聚合
    │   │   └── ansi.go          # ANSI 处理 stub
    │   └── sdk/                 # v0.2 才创建
    ├── session/
    │   ├── session.go           # Session struct
    │   └── manager.go           # SessionManager
    ├── registry/                # 进程注册表
    │   └── registry.go          # JSON 持久化
    └── cli/
        └── test.go              # M1 临时命令：nightme test --workspace
```

### 2.4 第一个 PR 的 Commit 拆分（M1）

**PR #1: scaffold + config + agent**（4 commits）

```
commit 1: chore: initial go module + directory skeleton
  - go mod init github.com/cnlangzi/nightme
  - 创建 internal/{config,agent,bridge,session,registry,cli} 目录
  - README + .gitignore 更新
  - go.sum + go.mod 提交

commit 2: feat(agent): Agent / AgentSession / Event interfaces
  - internal/agent/agent.go
    - type Agent interface { Name, Mode, Detect, Start }
    - type AgentSession interface { Events, SendText, SendPermission, Close }
    - type AgentEvent struct (Text / Permission / ToolStart / ToolEnd / Done / Error)
    - type Mode enum (ModeACP, ModeSDK, ModePTY)
  - internal/agent/registry.go: agent 注册表
  - 单测：interface compile check + Event 实现

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

**PR #2: Bridge PTY backend + Session Manager**（4 commits）

```
commit 5: feat(bridge): PTY backend (aymanbagabas/go-pty)
  - internal/bridge/pty/pty.go
  - New(workspace, command, args, env, cols, rows) → Bridge
  - Read / Write / Setsize / Close / PID
  - 单测：spawn /bin/echo 验证输出

commit 6: feat(bridge): PTY mode AgentSession implementation
  - internal/bridge/pty/session.go
    - ptySession wraps pty.Bridge
    - Events channel emits TextEvent / DoneEvent
    - SendText / SendPermission (best-effort) / Close
    - readLoop goroutine: bytes → TextEvent
  - internal/agent/ptyagent/ptyagent.go: PTY mode Agent
  - 单测：readLoop 正确转 events

commit 7: feat(bridge): ACP / SDK backend stubs
  - internal/bridge/acp/acp.go: ModeACP Agent stub (v0.1 不实现)
  - internal/bridge/sdk/sdk.go: ModeSDK Agent stub (v0.2 才用)
  - interface 实现存在但 body 返回 ErrNotImplemented
  - 单测：stub 能注册

commit 8: feat(session): Session + SessionManager
  - internal/session/session.go
  - internal/session/manager.go: Create / Get / List / Kill
  - Create 调用 agent.Start() → AgentSession（不是直接 pty.New）
  - readPump goroutine: AgentSession.Events() → channel adapter
  - 集成测试：Create + consume events + Close
```

**PR #3: registry persistence + CLI test command**（3 commits）

```
commit 9: feat(registry): persist session map to JSON
  - SessionManager 集成 registry
  - Upsert on Create, Delete on cleanup
  - chat_id 作为自然键
  - 集成测试：restart 后能 Load

commit 10: feat(cmd): nightme test --workspace --agent
  - cmd/nightme/main.go: cobra 替代 stub
  - cmd/nightme/test.go: 启动 session + 直接 I/O loop（stdin ↔ session）
  - M1 临时命令，M2 会被 /cwd /run 替代
  - 集成测试：spawn mock agent + verify round-trip

commit 11: docs: README quickstart + test command usage
  - 编译步骤（go build）
  - `nightme test --workspace /tmp/foo --agent /bin/echo` 示例
  - 配置文件说明
```

### 2.5 验收标准（M1）

| 项 | 标准 |
|----|------|
| `go build ./...` | 0 error |
| `go test ./...` | 全绿，coverage > 70% |
| `go vet ./...` | 0 warning |
| `golangci-lint run` | 0 issue |
| `nightme test /tmp/foo --agent /bin/echo` | 能 spawn `/bin/echo` 并打印其输出 |
| 跨平台编译 | macOS + Linux 各产出一个二进制，单文件 < 15MB |
| 进程归属 | `ps --ppid <nightme-pid>` 能看到所有 session CLI |
| 优雅退出 | Ctrl+C 后 nightme 退出，session CLI **仍存活**（默认 detach 策略）|
| Registry | `cat registry.json` 能看到当前所有 session |
| Agent interface | Mode 字段可正确区分 ACP/SDK/PTY |

### 2.6 风险 & 备选

| 风险 | 备选 |
|------|------|
| `aymanbagabas/go-pty` 在某 OS 上编译失败 | 切 `creack/pty`（~30 行 wrapper 改动） |
| 测试环境没装 claude/codex | M1 用 `/bin/echo` 验证 bridge |
| ANSI 转义符导致终端显示问题 | M1 简单 strip，M2 优化 |
| Agent interface 后续要改 | M1 是 stable interface，v0.2 加 backend 不动 interface |

---

## 3. M2: Feishu Round-Trip MVP

### 3.1 目标

飞书 DM 中发 `/cwd /tmp/myproject` → `/run codex` → nightme 创建 session + 通过 ACP 启动 codex → 后续消息透传 → codex 输出回飞书。

**ACP 模式**（baseline）：codex 已支持 ACP，nightme 通过 ACP client 接 codex。

**PTY 模式**（fallback）：Claude Code 暂时用 PTY（Anthropic 不支持 ACP，v0.2 切 SDK）。

### 3.2 范围

- ✅ **Gateway**（F-20）：slash command 路由
  - `/cwd <path>`：设置 workspace
  - `/run <agent> [args...]`：启动 CLI（spawn 或 reconnect）
  - `/kill`：停止 CLI（session 保留）
  - `/help`：列命令
  - 未命中 slash command 透传给 agent
- ✅ **Channel interface**（F-08）+ **feishu adapter**（lark-oapi-go）
- ✅ **ACP backend** 实现（v0.1 stub → v0.2 实施）
  - `internal/bridge/acp/acp.go`: JSON-RPC client
  - 启动 ACP server，握手，消费 events
- ✅ **PTY 模式用于 Claude Code**（`claude` agent 注册为 ModePTY）
- ✅ **Router**：chat_id → session 路由
- ✅ **Channel renderer**：AgentEvent → IM
  - TextEvent → 普通消息
  - PermissionRequest → 飞书 interactive card（按钮）
  - ToolStart/ToolEnd → 带 emoji 的消息
- ✅ **Output Aggregator**（200ms / 4KB 合并窗口）
- ✅ **Logger**（slog + JSON file）
- ❌ 图片 / 文件 / 富文本（v0.2）
- ❌ Web TTY UI（v0.2）
- ❌ 多 Channel mirror（v0.2）

### 3.3 M2 Commit 拆分

**PR #4: Gateway + Channel interface**（4 commits）

```
commit 12: feat(gateway): slash command parser + Gateway interface
  - internal/gateway/gateway.go
  - internal/gateway/parser.go: ParseCommand
  - 4 个命令注册（cwd / run / kill / help）
  - 未命中命令透传给 fallback
  - 单测：parse / match / fallback

commit 13: feat(channel): Channel interface + mock impl
  - internal/channel/channel.go
  - internal/channel/mock/mock.go
  - 单测：mock 行为

commit 14: feat(channel): feishu adapter skeleton
  - internal/channel/feishu/feishu.go
  - lark-oapi-go 长连接启动
  - Incoming / SendMessage / SendLongMessage 实现
  - AgentEvent 渲染（Text → 文本，Permission → card）
  - 单测：mock SDK 验证消息路由

commit 15: feat(session): Gateway integration + chat_id routing
  - internal/session/router.go
  - Channel.Incoming → Gateway.Handle → Session
  - Lookup(chat_id) → session
  - 集成测试：mock channel 触发 /cwd /run
```

**PR #5: ACP backend + end-to-end**（4 commits）

```
commit 16: feat(bridge): ACP backend implementation
  - internal/bridge/acp/acp.go
  - JSON-RPC client over PTY (PTY 作为 ACP 通信的物理载体)
  - Initialize / NewSession handshake
  - Events: message_chunk / permission_request / tool_start / tool_end
  - 转换为 AgentEvent
  - 单测：mock ACP server 验证 protocol

commit 17: feat(bridge): register agents with proper mode
  - internal/agent/registry.go: 更新
  - codex → ModeACP
  - opencode → ModeACP (v0.2 实施时验证支持)
  - claude → ModePTY (临时，v0.2 切 ModeSDK)
  - 单测：每个 agent 返回正确 Mode

commit 18: feat(cmd): nightme run (daemon mode)
  - cmd/nightme/run.go
  - 启动 channel + gateway + manager + signal handling
  - 配置加载 + logger 初始化
  - 集成测试：mock 全链路 round-trip

commit 19: docs: e2e manual test guide + ACP/PTY design notes
  - docs/E2E_TESTING.md
  - 飞书 app 配置步骤
  - 跑通第一个 demo 的 step-by-step
  - ACP 协议说明
```

### 3.4 验收标准（M2）

| 项 | 标准 |
|----|------|
| 飞书 DM 首条 `/cwd /tmp/foo` | 收到 "Workspace set to /tmp/foo. Send /run <agent>." |
| 飞书 DM 后续 `/run codex` | 收到 "Started: codex, cwd=/tmp/foo" |
| 飞书 DM 后续 `/run codex`（CLI 在跑）| 收到 "Already running (pid=12345). Connected."（不重启）|
| 飞书 DM 后续 `/kill` | 收到 "session killed" |
| 飞书 DM 后续 `/help` | 收到 4 个命令的列表 |
| 飞书 DM 发 `hello`（普通文本）| codex 收到 stdin |
| 飞书 DM 发 `/clear`（未命中 nightme 表）| 透传给 codex（codex 自己处理）|
| codex 输出 | 飞书 DM 收到（聚合 200ms 内）|
| codex 退出 | 飞书 DM 收到 "Session ended" |
| nightme 重启 | 已存在的 session 自动 reattach（如 PID 活）|
| 错误处理 | workspace 不存在 / agent 找不到 / ACP server 失败 → 友好提示 |
| `nightme list` | 能列出所有 session（chat_id / ws / pid / started_at）|
| ACP permission 渲染 | 飞书收到 interactive card（按钮）|

### 3.5 飞书配置前置

**Devin 需要提前准备**（不在虾哥交付范围内）：
1. 飞书开放平台 → 创建企业自建应用
2. 获取 app_id + app_secret
3. 配置机器人能力
4. 把 app 添加到 IM 联系人
5. 填 `configs/nightme.example.yaml`

虾哥给 templates，Devin 填好 credentials。

### 3.6 Codex / OpenCode / Claude Code 的 Mode 部署

| CLI | Mode | 实施状态 | 备注 |
|-----|------|----------|------|
| **codex** | ACP | M2 实施 | 复用 acp.go |
| **opencode** | ACP | M2 实施（如支持）| 需要 OpenCode 的 ACP server flag，v0.1 stub，verify in M2 |
| **claude** | PTY → JSON-IO | M2 PTY（临时）/ v0.2 JSON-IO | Anthropic 不支持 ACP，v0.2 切专用 JSON-IO bridge（F-24）|
| **未知 CLI** | PTY | M2 fallback | 任何 CLI 都能用 |

---

## 4.5 v0.2: Claude Code Bridge + Heartbeat

### 4.5.1 目标

让 Claude Code 在 nightme 里获得原生体验：

- **结构化 event 流**：不再有 ANSI 垃圾
- **自动接受权限**：`--permission-mode bypassPermissions`
- **AskUserQuestion 卡片渲染**：用户可以在飞书里多选 / 自定义
- **心跳可见性**：用户随时知道"还在响应 / 真断了"

**心路历程**：原计划"v0.2 = SDK adapter" → Anthropic 没 Go SDK → 改走"Claude Code 专用 bridge (JSON-IO)"。详见 [F-24-claudecode-bridge.md](./feat/F-24-claudecode-bridge.md) §1。

**F-23 vs F-17 关系**：原 F-17（v0.2 stub）基于"30s/5min 阈值"判断 idle/timeout — 这是错误设计。**F-23 取代 F-17**，基于 event-driven tick + 进程级 DEAD 检测。

### 4.5.2 范围

- ✅ **F-24**: Claude Code bridge (JSON-IO + auto-accept + AskUserQuestion)
- ✅ **F-23**: Heartbeat (event-driven + 进程级 DEAD + 用户主权)
- ❌ PreToolUse hook（v0.3 评估）
- ❌ Codex / OpenCode 升级（维持 ACP 模式）

### 4.5.3 文件结构（v0.2 新增）

```
internal/
├── bridge/
│   └── claudecode/        # v0.2 新增
│       ├── claudecode.go
│       ├── session.go
│       ├── stream.go
│       ├── permissions.go
│       ├── ask.go          # AskUserQuestion 拦截
│       ├── format.go
│       └── testdata/
│           ├── init.json
│           ├── text_chunk.json
│           ├── tool_use.json
│           ├── tool_result.json
│           ├── ask_question.json
│           └── result.json
├── heartbeat/              # v0.2 新增
│   ├── heartbeat.go
│   ├── process.go          # ProcessProbe interface
│   ├── format.go           # text/idle duration
│   └── heartbeat_test.go
└── channel/
    └── feishu/
        └── card.go         # v0.2 扩展：card note update API
```

### 4.5.4 v0.2 Commit 拆分（6 commits）

```
commit A: docs(feat): F-23 heartbeat (event-driven tick + 进程级 DEAD)
  - docs/feat/F-23-heartbeat.md
  - 取代 F-17 stub

commit B: docs(feat): F-24 claudecode-bridge spec
  - docs/feat/F-24-claudecode-bridge.md
  - 含 4 个触发条件 / JSON-IO schema / AskUserQuestion 双路兼容

commit B2: docs(feat): F-25 input-buffer spec
  - docs/feat/F-25-input-buffer.md
  - 3 状态 + 双轨 Reaction/Reply + 纯内存 buffer

commit C: feat(heartbeat): event-driven tick + ProcessProbe
  - internal/heartbeat/{heartbeat,process,format}.go
  - heartbeat_test.go (含 mock ProcessProbe)
  - single-line note update 机制
  - ✅ test: 单元测试全绿（覆盖 idle 检测、DEAD 双路、format）

commit D: feat(bridge/claudecode): JSON-IO + auto-accept
  - internal/bridge/claudecode/{claudecode,session,stream,permissions}.go
  - spawn `claude --print --input-format stream-json --output-format stream-json --permission-mode bypassPermissions --verbose`
  - parse stream-json events → AgentEvent
  - testdata/ 提供 6 个 mock JSON fixture
  - ✅ test: 单元测试用 fixture 验证 event parse

commit E: feat(bridge/claudecode): AskUserQuestion 双路兼容
  - internal/bridge/claudecode/ask.go
  - tool_use 拦截路径 + text fallback 路径
  - 答案回写：string (单选) + string (多选逗号) + array (多选 array) 都支持
  - ✅ test: 5 个 trigger prompt 测试用例（待真 Claude 验证）

commit F: feat(channel/feishu): AskUserQuestion 卡片 + MessageReceipt
  - internal/channel/feishu/{card,receipt}.go 扩展
  - AskUserQuestion 卡片渲染（第一项 Recommended 高亮）
  - MessageReceipt: 双轨 Reaction + Reply (3 状态)
  - ✅ test: 卡片 schema 验证 + receipt state transition

commit G: feat(session): InputBuffer (3 状态 + 纯内存 buffer)
  - internal/session/{input_buffer,state}.go
  - 50 条 / 100KB 上限
  - 集成 F-25 + F-23 heartbeat (Heartbeat() callback)
  - 集成 F-24 claudecode bridge (state 转换来源)
  - ✅ test: buffer 流转 + 并发 + 边界
```

**实际可能拆 7+ commits**，每个 commit 单独可验收。

### 4.5.5 验收标准（v0.2）

| 项 | 标准 |
|----|------|
| Claude Code spawn | `claude --print --input-format stream-json --output-format stream-json --permission-mode bypassPermissions` 启动成功 |
| Event parse | 6 个 fixture 全部正确 parse（init/text/tool_use/tool_result/ask/result）|
| AskUserQuestion tool_use 拦截 | 检测到 `tool_use.name=="AskUserQuestion"` → emit EventPermissionRequest |
| AskUserQuestion text fallback | 检测 markdown 表格 + "Pick one" 关键词 → emit EventPermissionRequest |
| 飞书卡片 | AskUserQuestion 渲染为 interactive card，第一项加 (Recommended) |
| 答案回写（单选）| string 格式正确写入 stdin |
| 答案回写（多选 array）| array 格式正确写入 stdin |
| 答案回写（多选 string）| 逗号分隔 string 正确写入 stdin |
| Heartbeat tick | 每个 event +1，card note 显示 "⏳ N · HH:MM:SS" |
| Heartbeat idle | 30s+ 没 event 显示 "⏳ N · HH:MM:SS · idle Xs/Xm" |
| DEAD 检测（进程退出）| signal 0 失败 → card note 变 "❌ 已退出（exit code: X）" |
| DEAD 检测（stdout EOF）| pipe 关闭 → card note 变 "❌ 输出流已关闭" |
| 不自动 kill | 长 idle 30 min 也不报 DEAD，不 kill 进程 |
| 飞书 reaction | turn 开始加 1 个 "👀"，不堆叠 |

### 4.5.6 与 v0.1 PTY mode 的关系

Claude Code 在 v0.1 走 PTY。v0.2 切 JSON-IO 后：

- **PTY 仍然保留**（v0.1 不变）—— 兜底给未知 CLI
- **Claude Code 切到 JSON-IO** —— 用专用 bridge package
- **每 CLI 独立 bridge** —— per-agent bridge 架构（详见 F-21 §1 + §8.1）

### 4.5.7 未实测部分

- **本地测试环境限制**：ANTHROPIC_BASE_URL 强制 routing 到 MiniMax，本地不能实测真 Claude 的 AskUserQuestion 行为
- **user answer 实际 wire format**：需要真 Claude 验证（推断为 tool_result.content 字符串/数组）
- **CHANGELOG 显示 AskUserQuestion 持续维护**——大方向稳定，细节需实测

**v0.2 release note** 必须标"待真 Claude 验证"。

---

## 4. M3: Hardening + v0.1 release

### 4.1 范围

- 错误处理边界（所有 panic 兜底）
- 日志脱敏（app_secret / API key）
- SIGTERM 优雅退出（标记 detached，不杀 CLI）
- `--cleanup` flag 支持 kill 所有 session
- 文档完善（README + E2E_TESTING.md）
- GitHub Actions CI（go test / go vet / build matrix）
- v0.1.0 release + binary 发布
- 错误码 + 退出码统一
- 配置 hot-reload（可选）

### 4.2 不在 M3 范围（留给 v0.2+）

- **F-23** Heartbeat（event-driven + 进程级 DEAD）— v0.2
- **F-24** Claude Code Bridge（JSON-IO + AskUserQuestion）— v0.2
- 图片 / 文件透传（F-14）— v0.2+
- 终端 resize（F-13）— v0.2+
- 多 Channel mirror（F-11）— v0.2+
- WhatsApp / Telegram（F-12）— v0.2+
- Web TTY UI（F-16）— v0.2+
- ~~F-17 健康检查~~ — **superseded by F-23**
- Session 历史持久化（F-15）— v0.2+
- ~~密码注入（F-18）~~ — **cancelled**，透传处理（PRD §4.1）

> **SDK adapter 取消**：原计划"v0.2 = SDK adapter"，但 Anthropic 没官方 Go Claude Agent SDK（只有 Python/TS）。改走"Claude Code 专用 JSON-IO bridge"（F-24）。

---

## 4.6 v1.2: ChatSession 重构

> **状态**：草案（待 Devin 确认 PRD/SPEC/FEATURES 草案 + Q-A/Q-B）
> **目标 session 数**：3-5 sessions
> **预估代码量**：~1500 行 Go（包含 schema 改造 + 状态机迁移 + 测试）
> **关键交付**：F-27 ChatSession + F-28 `/use` + F-29 AgentSession 池；向后迁移 v0.x 持久化数据

### 4.6.1 目标

把 v1.1 单层 `Session` 模型重构为 v1.2 双层 `ChatSession` + `AgentSession` 模型：

1. **ChatSession**：per chat 持久化会话上下文，跨 daemon 重启
2. **AgentSession**：per `(agent, cwd)` 1:1 唯一，池化在 ChatSession 下
3. **`/use <agent>`**：lazy switch，复用或 spawn，**永不重启进程**
4. **`/cwd`**：只改 activeCwd，不触发 spawn / kill
5. **`/kill`**：清空整个 ChatSession pool
6. **删除 `/run`**：被 `/use` 完全替代

### 4.6.2 范围

**In scope**：
- 持久化 schema 改造（v1.x → v1.2 双表）
- `Session` 类型拆分为 `ChatSession` + `AgentSession`
- Gateway handler 重写：`/cwd` / `/use` / `/kill`（**无 `/default`** — Q-A 已确认全局 only）
- v1.x registry 自动迁移到 v1.2
- 测试（unit + integration + E2E 飞书）
- SPEC / PRD / FEATURES 草案 → 锁定

**Out of scope**（明确不做）：
- 多 AgentSession 并行协作（v0.4+）
- 跨 chat 共享 ChatSession
- AgentSession 跨 ChatSession 共享
- LRU eviction（v0.4+）
- `/default` 命令（Q-A 已确认全局 only，不需要 per-chat command）

### 4.6.3 文件结构（v1.2 新增 / 修改）

```
internal/
├── chatsession/                   # NEW
│   ├── chatsession.go             # ChatSession struct + lifecycle
│   ├── lookup.go                  # LookupActiveAgentSession (resolution logic)
│   ├── restore.go                 # Restore from registry
│   └── *_test.go
├── agentsession/                  # NEW (replaces internal/session/session.go)
│   ├── agentsession.go            # AgentSession struct + bridge integration
│   ├── pool.go                    # (concept, but logic lives in chatsession)
│   ├── spawn.go                   # Spawn / Respawn / Kill
│   └── *_test.go
├── session/                       # SHRINK: only InputBuffer FSM (F-25) remains here
│   ├── inputbuffer.go             # moved ownership → chatsession callsite
│   └── ...
├── registry/                      # MODIFIED
│   ├── chat_session_entry.go      # NEW (split from v1.x SessionEntry)
│   ├── agent_session_entry.go     # NEW
│   ├── migrate_v1_to_v2.go        # NEW (auto-migration on startup)
│   ├── registry.go                # MODIFIED (two-file persistence)
│   └── *_test.go
├── gateway/                       # MODIFIED
│   ├── handlers/
│   │   ├── cwd.go                 # SIMPLIFIED (no spawn trigger)
│   │   ├── use.go                 # NEW (replaces run.go)
│   │   ├── kill.go                # MODIFIED (kills entire pool)
│   │   ├── default.go             # NEW (Q-A decision pending)
│   │   └── run.go                 # DELETED
│   └── ...
└── ...
```

### 4.6.4 v1.2 Commit 拆分（不分子 PR）

> **Devin 明确指示**：分步 commit，但不分 PR（一次直推 main）

按 docs-first → code 顺序：

| # | Commit | 内容 | 类型 | 风险 |
|---|--------|------|------|------|
| 1 | `docs(prd): v1.2 ChatSession 设计哲学` | PRD.md §4.3/§4.6/§5 更新 | docs | Low |
| 2 | `docs(spec): v1.2 两层 Session 架构` | SPEC.md 架构重写 + schema 草案 | docs | Low |
| 3 | `docs(features): v1.2 F-27/28/29` | FEATURES.md + feat/F-27/28/29 | docs | Low |
| 4 | `docs(plan): v1.2 实施计划` | PLAN.md §4.6 | docs | Low |
| 5 | `refactor(registry): split SessionEntry → ChatSessionEntry + AgentSessionEntry` | schema 拆分 + 迁移 | code | Medium |
| 6 | `feat(chatsession): ChatSession struct + lifecycle` | 新建 ChatSession + LookupActiveAgentSession | code | Medium |
| 7 | `refactor(agentsession): AgentSession pool + spawn/reuse/respawn` | 新建 AgentSession + pool ops | code | Medium |
| 8 | `refactor(gateway): handlers for /cwd /use /kill (no /run)` | handlers 重写 | code | High |
| 9 | `refactor(inputbuffer): ownership moves to ChatSession` | InputBuffer FSM 迁移 | code | Medium |
| 10 | `test(integration): /use reuse + pool survive /kill + restart` | 集成测试 | test | Low |
| 11 | `test(e2e): 飞书 DM 三态切换 (claude→codex→claude)` | E2E 测试 | test | Low |
| 12 | `docs: v1.2 锁定 + release notes` | SPEC/FEATURES/PRD 状态从"草案"改"锁定" | docs | Low |

**关键 commit 顺序约束**：
- commit 5-9 必须连续推送（不能拆 PR）；半完成状态 runtime 不一致
- commit 5 落地后 commit 6-9 立刻跟上
- commit 10-11 测试可与 5-9 交错（不同 package）

### 4.6.5 验收标准（v1.2）

**单元测试**：
- ChatSession SetActiveCwd / SetActiveAgent 纯状态变更（不触发 spawn/kill）
- LookupActiveAgentSession 三个分支（exact / default fallback / spawn）
- AgentSession pool `(chatSessionId, agent, cwd)` 唯一索引
- KillAll 清空 pool + activeAS=nil
- 并发 `/use` + readPump event（`go test -race`）

**集成测试**：
- /use claude → /use codex → /use claude：第一次和第三次 PID 一致
- /kill → 下一条消息：新 PID，pool 空
- daemon 重启：AgentSession status=Detached，respawn on lookup
- v1.x registry 自动迁移：单个 v1 SessionEntry → ChatSessionEntry + AgentSessionEntry

**E2E（飞书 DM）**：
- /cwd → /use claude → "fix bug X" → 看到 claude 输出
- /use codex → "review this code" → 看到 codex 输出
- /use claude → 继续 → claude 进程是第一次那个 PID（对话上下文保留）
- /cwd /other/path → /use codex → pool 中 (codex, /original) 仍在
- /use claude → 切回时 spawn 新 (claude, /other/path)
- 切换过程中 Receipt FSM 不受影响（⏳ → 🔄 → ✅ 正常流转）

**资源占用**：
- 单 ChatSession 5 个 AgentSession：RSS < 50MB
- 5 个 ChatSession 各 3 个 AgentSession：RSS < 100MB

### 4.6.6 风险 & 备选

| 风险 | 影响 | 缓解 |
|------|------|------|
| v1.x registry 迁移失败 | 用户启动失败 | 迁移前自动备份 `sessions.v1.json.bak`；失败回滚 |
| Pool 无限增长 | 内存泄漏 | v1.2 不加 LRU（监控；v0.4+ 加） |
| /use 切换时老 AgentSession 事件丢失 | 用户体验下降 | 文档化（PRD §4.3 已说明"过时的不管"） |
| 并发 /use + readPump race | 事件丢失 / panic | 严格 poolMu RWMutex；`go test -race` 覆盖 |
| Bridge 在 respawn 时复用问题 | 进程崩溃 | 每次 respawn 创建新 Bridge；老 Bridge 关闭 |

**备选方案**（如 v1.2 失败回退）：
- 回退到 v1.1 单层 Session（保留 commit 5 schema 拆分，因为不破坏 v1.1 runtime）
- 不实现 pool，回退到 v1.x 单 chat 单 session（需要回退 commit 7/8/9）

### 4.6.7 与 v0.x 的兼容

**不兼容点**（v1.2 是 breaking change）：
- `/run` 命令删除
- `Session` 类型不存在（被 ChatSession + AgentSession 替代）
- Registry 文件从单文件变双文件（但内容等价）

**兼容路径**：
- v1.x → v1.2 升级：自动迁移；无需手动操作
- v1.2 → v1.x 回退：不可逆（v1.x 不识别双表）；但 v1.2 部署后再回退 v1.x 不推荐

### 4.6.8 决策确认

| 决策 | 影响 | 状态 |
|------|------|------|
| **Q-A**: Default Agent 设置粒度 | 仅全局 `defaults.agent` config；ChatSession.defaultAgent 是创建时 snapshot；**无 `/default` 命令** | ✅ 已确认 (2026-08-02) |
| **Q-B**: `(activeAgent, activeCwd)` 不在 pool 时 fallback 顺序 | 影响 LookupActiveAgentSession 逻辑 | 待 Devin |
| **Q-C**: ChatSession.ID 来源 | 影响 ChatSessionEntry schema | 倾向 derived from chatId |
| **Q-D**: /kill 时 InputBuffer 队列消息处理（drop / persist）| 影响 ChatSession.KillAll 行为 | 倾向 drop |
| **Q-E**: AgentSessionEntry 持久化是否包括 Bridge 类型 | 影响 registry schema | 倾向包括 |
| **Q-F**: ChatSession 持久化位置（单文件 / 双文件 / SQLite）| 影响 registry 复杂度 | 倾向双 JSON 文件 |

---

## 5. 时间预估（虾哥节奏）

| Milestone | 预估 session 数 | 预估代码量 | 关键交付 |
|-----------|----------------|------------|---------|
| M0 | ✅ 已完成 | 0 行 Go，~2700 行 spec | docs |
| M1 | ✅ 已完成 | ~1000 行 Go | Bridge 抽象 + PTY backend + Session + Registry |
| M2 | ✅ 已完成 | ~1800 行 Go | Gateway + ACP backend + Channel + 飞书 round-trip |
| M3 | ✅ 已完成 | ~400 行 Go + docs | hardening + CI + v0.1 release |
| v0.2 | ✅ 已完成 | ~1200 行 Go | F-23 heartbeat + F-24 claudecode-bridge |
| v0.3 (v1.1) | ✅ 已完成 | ~800 行 Go | F-25 + F-26 职责隔离架构 |
| **v1.2** | **3-5 sessions** | **~1500 行 Go** | **F-27 + F-28 + F-29** |
| v0.2 | 2-3 sessions | ~1200 行 Go | F-23 heartbeat + F-24 claudecode-bridge |

每个 session 内：
- 读 docs → 写 commit → 跑测试 → 汇报 → 等用户反馈
- 不批量 commit，**每个 commit 都要验收**

---

## 6. 与 OpenClaw gtw 集成（可选）

如果用户想用 OpenClaw 的 gtw plugin 管理 nightme 的 issue / PR：
- 仓库地址：`github.com/cnlangzi/nightme`
- gtw 配置：`/gtw on ~/code/nightme`
- issue label：复用 `gtw/ready | gtw/wip | gtw/lgtm | gtw/revise | gtw/stuck`
- commit message：英文（nightme 是开源项目，README.md 是英文）

但这不强制，Devin 可以用 git + gh 命令行手管。

---

## 7. 下一步

**M0 已完成**（docs only）。M1 触发词：
- "开始 M1" / "go" / "开干"
- 或 "先帮我 review docs"（虾哥等反馈）

M1 的第一个 PR：
- PR #1：scaffold + config + agent (4 commits)
- PR #2：Bridge PTY backend + Session Manager (4 commits)
- PR #3：registry persistence + CLI test command (3 commits)

总 11 commits 进入 M1 第一个可跑通的版本：`nightme test --workspace <path> --agent <name>`。

---

## 8. 文档变更日志

- **v1.0** — 初始版本（基于早期 PRD/SPEC 设计）
- **v2.2** — **v1.2 设计加入**：
  - Session → ChatSession + AgentSession 双层模型
  - `/run` 删除，`/use <agent>` 替代
  - AgentSession 池化（per ChatSession `(agent, cwd)` 1:1）
  - `/cwd` 只改 activeCwd，不触发 spawn
  - `/kill` 清空整个 pool
  - Registry 双表拆分（ChatSessionEntry + AgentSessionEntry）
  - InputBuffer FSM 迁移到 ChatSession 级
- **v2.1** — v0.2 设计加入：
  - F-23 heartbeat 取代 F-17（event-driven tick + 进程级 DEAD + 用户主权）
  - F-24 claudecode-bridge（JSON-IO + AskUserQuestion 双路兼容）
  - F-21 ModeJSONIO（v0.2 新增模式，取代原计划 SDK adapter）
  - per-agent bridge 架构明确化（每 CLI 独立 bridge package）
- **v2.0** — 重写以适配最新架构：
  - Session lifecycle 改为 `/cwd` + `/run` 两步式（不再用文字 `workspace:` 前缀）
  - Bridge 抽象支持 ACP/SDK/PTY 三层模式（不再只是 PTY）
  - Agent interface 改为统一抽象（Mode + AgentSession + Event）
  - 触发词改用 slash command（Gateway）
  - 文件结构 `internal/pty/` → `internal/bridge/{acp,pty,sdk}/`
  - M1 命令改名：`nightme demo` → `nightme test`
  - M2 起点：ACP 模式（不是 PTY）
  - commit 数量从 18 → 19（F-20, F-21 加入）
  - F-18 标记为 cancelled（透传策略，PRD §4.1）
