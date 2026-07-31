# F-20: Command Gateway (Slash Command Router)

> **Status**: designed (v0.1)
> **Milestone**: M2 (used by all chat input)
> **Depends on**: F-08 (Channel), F-01 (Session), F-09 (Agent)
> **Used by**: 所有 IM → nightme 的输入
> **Related docs**: SPEC.md §1.1 (Gateway 组件), §2.1 (input flow)

## 1. Description

**Gateway** 是 nightme 在 Channel Adapter 和 Session Manager 之间的**命令路由器**。它判断每条进来的 IM 消息是：

- **slash command 命中 nightme 表**（`/cwd`、`/run`、`/kill`、`/help`） → nightme 执行
- **slash command 未命中 nightme 表**（如 `/clear`、`/compact`、`/init`）→ **透传**给 PTY stdin，让底层 AI agent 自己处理
- **非 `/` 开头的普通文本** → 透传给 Session Manager → 写入 PTY stdin

**核心原则**：nightme 只拦截**真正需要 session 管理**的命令。其他 slash 命令属于 agent 自己的 namespace，nightme 不做 opinionated 拒绝——`/` 前缀不等于"必是 nightme 命令"。

## 2. Interface

```go
// internal/gateway/gateway.go
package gateway

type Command struct {
    Name        string
    Aliases     []string
    Description string
    Handler     func(ctx context.Context, msg *Message, args []string) (*CommandResult, error)
}

type CommandResult struct {
    Reply     string
    Consumed  bool
}

type FallbackHandler func(ctx context.Context, msg *Message) error

type Gateway interface {
    Register(cmd Command)
    Handle(ctx context.Context, msg *Message) error
    ListCommands() []Command
}

func New(fallback FallbackHandler) *Gateway
```

## 3. Implementation

**文件**：
- `internal/gateway/gateway.go` — Gateway 接口 + 实现
- `internal/gateway/parser.go` — slash command 解析
- `internal/gateway/commands.go` — v0.1 命令注册（cwd / run / kill / help）

**核心流程**：
```
Channel Adapter.Incoming() → Message
  ↓
Gateway.Handle(ctx, msg)
  ├─ text 不以 "/" 开头 → fallback(msg) → SessionManager → PTY stdin
  └─ text 以 "/" 开头
      ├─ ParseCommand(text) → (name, args)
      ├─ 查 commands[name]（含 Aliases）
      │   ├─ 命中 → Command.Handler(ctx, msg, args)
      │   └─ 未命中 → fallback(msg) → SessionManager → PTY stdin
      │                                  （agent 自己处理 /foo）
```

**关键设计决策**：
- **未命中命令透传，不拒绝**：避免跟 agent 的 slash commands 冲突
- **`/` 前缀不等于 nightme 命令**：nightme 的责任范围**只限于 session 管理**
- **slash command 命中后，即使参数错误也由 nightme 报错**：因为这个命令确实属于 nightme 的 namespace

**Parser 行为**：
- `/cmd` → name="cmd", args=[]
- `/cmd arg1 arg2` → name="cmd", args=["arg1", "arg2"]
- `/cmd "arg with space"` → v0.2 支持；v0.1 按空格切分
- 解析失败的输入（如纯 `/` 后无字符）→ 视为普通文本，走 fallback

## 4. v0.1 命令集（nightme 的 namespace）

| 命令 | 参数 | 行为 | 前置条件 |
|------|------|------|----------|
| `/cwd` | `<path>` | 设置/更新 workspace（创建 session 如不存在）| 任意 |
| `/run` | `<agent> [args...]` | 确保 CLI 在跑（spawn 或 attach）| **workspace 已设** |
| `/help` | (无) | 返回所有 nightme 命令列表 | 任意 |
| `/kill` | (无) | 停止当前 CLI（保留 session）| CLI 正在跑 |

**别名**：`/workspace` 是 `/cwd` 的别名。

### 4.1 `/cwd <path>` 详细行为

- 解析 `path`（支持 `~` 展开）
- 验证 path 是已存在的目录
- **创建或更新 session**：
  - chat_id 无 session → 创建，workspace = path
  - chat_id 已有 session 且 CLI 没在跑 → 更新 workspace
  - chat_id 已有 session 且 CLI 在跑 → **拒绝** "CLI running, /kill first to change workspace"
- CLI **不**自动启动（必须 `/run` 单独触发）

**回复**：
- 首次创建："Workspace set to {path}. Send /run <agent> to start CLI."
- 更新："Workspace updated to {path}."
- 拒绝："CLI running, /kill first to change workspace"

### 4.2 `/run <agent> [args...]` 详细行为

**核心**：**Workspace 是 /run 的硬性前置条件**。没有 workspace → 不能 spawn claude/codex/opencode。

**逻辑**：
```
/run <agent> [args...]
  ↓
1. 查 chat_id 的 session
   - 不存在 → 报错 "no workspace set, send /cwd <path> first"
2. session.Workspace 必须存在（registry 已存）
3. 检查 CLI 当前状态
   ├─ PID alive（PTY 还连着）→ "Already running, reconnecting..."
   │     * 实际上：readPump/writePump 已在跑，这条消息只是 user feedback
   └─ PID dead 或 session 没 CLI → 启动新 CLI
         ├─ agent.Get(name) → 验证 agent 已注册
         ├─ agent.Detect() → 验证二进制存在
         ├─ pty.New(workspace, agent.Command(), append(agent.Args(), args...))
         ├─ registry.Upsert(session with new PID)
         ├─ 启动 readPump/writePump goroutines
         └─ Reply "Started: {agent} {args}, cwd={workspace}"
```

**args 透传**：
- `args[0]` = agent name
- `args[1:]` = 额外参数，原样透传给 agent CLI
- nightme 不解析 / 不验证 / 不 sanitize

**为什么"智能"**：
- 用户不需要记"CLI 现在跑没跑"
- nightme 根据 PID 状态自动决定 spawn 或 reconnect
- **绝不无故重启正在跑的 CLI**（避免丢失 agent 内部状态）

**回复模板**：
| 场景 | 回复 |
|------|------|
| CLI 没在跑，成功启动 | "Started: `{agent} {args}`, cwd=`{workspace}`" |
| CLI 已在跑，reconnect | "Already running (pid={pid}). Connected." |
| workspace 没设 | "no workspace set, send /cwd `<path>` first" |
| agent 未知 | "unknown agent: {name}" |
| agent 二进制找不到 | "{name} binary not found, please install" |

**示例**：
| 顺序 | 输入 | 行为 |
|------|------|------|
| 1 | `/cwd /tmp/foo` | workspace set |
| 2 | `/run claude` | spawn `claude` in /tmp/foo |
| 3 | `/run claude --model opus` | CLI 在跑 → reconnect（不动 args）|
| 4 | `/kill` | CLI 停止 |
| 5 | `/run claude --model opus` | spawn `claude --model opus` |
| 6 | `/run` 无参数 | "usage: /run <agent> [args...]" |
| 7 | `/run foo` | "unknown agent: foo" |

### 4.3 `/help` 详细行为

返回（飞书 markdown）：
```
Available commands:
/cwd <path>          Set workspace (session-level)
/run <agent> [args]  Ensure CLI running (spawn or attach)
/help                Show this help
/kill                Stop current CLI (keep session)

Workflow:
  1. /cwd /path/to/project
  2. /run claude
  3. ... work ...
  4. /kill    (or restart with /run again)

Anything else (including unknown /-commands) is sent to the agent.
```

### 4.4 `/kill` 详细行为

- 调 `SessionManager.Kill(sessionID)`
- bridge.Close() → SIGTERM → 等 5s → SIGKILL
- 标记 session.PID = nil（session 保留，workspace 保留）
- 回复 "session killed (was: {agent} {args}, cwd={workspace})"

**关键**：kill 只停止 CLI，**不删除 session**。workspace / agent / args 都保留，方便 `/run` 重启。

## 5. 透传语义详解（重要）

nightme 只在**表 4** 列出的 4 个命令上拦截。其他所有以 `/` 开头的输入都透传：

| 用户输入 | nightme 表命中？ | 行为 |
|----------|-----------------|------|
| `/cwd /tmp/foo` | ✅ | nightme 设置 workspace |
| `/run claude` | ✅ | nightme 启动 CLI |
| `/help` | ✅ | nightme 列命令 |
| `/kill` | ✅ | nightme 停止 CLI |
| `/workspace /tmp/foo` | ✅（alias）| 等同 `/cwd` |
| `/clear` | ❌ | 透传 → agent 收到 `/clear` |
| `/compact` | ❌ | 透传 → agent 收到 `/compact` |
| `/foo` | ❌ | 透传 → agent 收到 `/foo` |
| `hello` | — | 透传 → agent 收到 `hello` |

## 6. Edge cases

| 场景 | 处理 |
|------|------|
| `/cwd /nonexistent` | 报错 "workspace does not exist: /nonexistent" |
| `/cwd <path>` 但 CLI 在跑 | 拒绝 "CLI running, /kill first" |
| `/run` 前没发过 `/cwd` | 报错 "no workspace set, /cwd first" |
| `/run foo`（未知 agent）| 报错 "unknown agent: foo" |
| `/run codex --bad-flag` | 透传，codex 自己报错 |
| `/run` 但 session 不存在 | 同上（自动隐含） |
| `/run` 时 nightme 之前检测到 CLI 死了 | spawn 新 CLI |
| `/kill` 但 CLI 没在跑 | "no running CLI to kill" |
| `/kill` 后 session 已存在，user 再 `/run` | 正常 spawn（session 没被删除）|
| nightme 重启后，session 已 detached 且 PID 还活着 | `/run` 时检测到 PID alive → reconnect |
| nightme 重启后，session 已 detached 且 PID 死了 | `/run` 时检测到 PID dead → spawn 新 CLI |
| 中文 slash command（如 `/帮助`）| v0.1 不支持，会**透传**给 agent |

## 7. Test plan

**单元测试**：
- `ParseCommand("/cwd /tmp/foo")` → `("cwd", ["/tmp/foo"], nil)`
- `ParseCommand("/run claude --model opus")` → `("run", ["claude", "--model", "opus"], nil)`
- `ParseCommand("/foo")` → `("foo", [], nil)`（**不报错**）
- `ParseCommand("not a command")` → Consumed=false
- `ParseCommand("")` → Consumed=false

**集成测试**：
- Gateway.Handle("/cwd /tmp/foo") → cwd handler 被调
- Gateway.Handle("/run claude") → run handler 被调（前置条件：workspace 已设）
- Gateway.Handle("/run") 无参数 → usage error
- Gateway.Handle("/run foo") → unknown agent
- Gateway.Handle("/foo") → fallback 被调（未命中透传）
- Gateway.Handle("/clear") → fallback 被调
- Gateway.Handle("hello") → fallback 被调

**RunHandler 行为测试**（集成）：
- workspace 已设，PID=0 → spawn new CLI → PID 更新
- workspace 已设，PID=alive → 返回 "already running"，不动 CLI
- workspace 已设，PID=dead → spawn new CLI（覆盖旧 PID）
- workspace 未设 → 报错

**手动 / E2E（M2）**：
- 飞书 DM 发 `/cwd /tmp/foo` → "Workspace set"
- 飞书 DM 发 `/run claude` → "Started: claude, cwd=/tmp/foo"
- 飞书 DM 发 `hello` → claude 收到
- 飞书 DM 发 `/run claude` → "Already running (pid=12345). Connected."
- 飞书 DM 发 `/kill` → "session killed"
- 飞书 DM 发 `/run claude` → "Started: claude"（新 PID）
- 飞书 DM 发 `/clear` → claude 收到 `/clear`（透传）
- `ps aux | grep claude` → 验证进程命令行

## 8. Open questions

- `/cwd <path>` 在 CLI 跑着时能否 update workspace？v0.1 拒绝，v0.2 可加 `--force` 或先 kill
- `/run` 是否允许切换 agent？v0.1 不允许（必须 /kill 后再 /run 新 agent），v0.2 评估
- agent args 跟之前不同时，是否需要先 /kill？v0.1 智能：如果 CLI 在跑就不变（保持 agent 状态），如果死了才 spawn 新的
- `/run` 启动失败后 session 状态？v0.1 报错后 PID 仍为 nil（用户可重试）
- 是否需要 `/forget` 命令清空 session？v0.1 不需要（/cwd 覆盖 + /run 重启就够）
