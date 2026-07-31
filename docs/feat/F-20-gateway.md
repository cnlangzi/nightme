# F-20: Command Gateway (Slash Command Router)

> **Status**: designed (v0.1)
> **Milestone**: M2 (used by all chat input)
> **Depends on**: F-08 (Channel), F-01 (Session), F-09 (Agent)
> **Used by**: 所有 IM → nightme 的输入
> **Related docs**: SPEC.md §1.1 (Gateway 组件), §2.1 (input flow)

## 1. Description

**Gateway** 是 nightme 在 Channel Adapter 和 Session Manager 之间的**命令路由器**。它判断每条进来的 IM 消息是：

- **slash command 命中 nightme 表**（`/cwd`、`/start`、`/kill`、`/help`） → nightme 执行
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
    Reply     string  // 回复给用户的文本（仅当 nightme 处理时用）
    Consumed  bool    // true = nightme 已处理；false = 应交给 fallback（agent）
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
- `internal/gateway/parser.go` — slash command 解析（`/cmd arg1 arg2` → name + args）
- `internal/gateway/commands.go` — v0.1 命令注册（cwd / start / kill / help）

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
      │   │         ├─ 执行成功 → 回复用户（如 "Session started"）
      │   │         └─ 执行失败（如 args 错误） → 回复用户 error
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
- `/cmd "arg with space"` → v0.2 支持引号；v0.1 按空格切分（限制）
- `/  cmd  ` → 忽略前导 / 尾部空白
- `//cmd` → v0.1 不识别（只切第一个 / 后面的全当 args）
- 解析失败的输入（如纯 `/` 后无字符）→ 视为普通文本，走 fallback

## 4. v0.1 命令集（nightme 的 namespace）

| 命令 | 参数 | 行为 | Session 要求 |
|------|------|------|--------------|
| `/cwd` | `<path>` | 创建 session（cwd = path）| 必须**没有** active session |
| `/start` | `<agent> [args...]` | 创建 session（agent = `<agent>`，透传 args 给 agent CLI）| 必须**没有** active session |
| `/help` | (无) | 返回所有已注册 nightme 命令的列表和描述 | 任意 |
| `/kill` | (无) | 终止当前 session | 必须有 active session |

**别名**：`/workspace` 是 `/cwd` 的别名。两者等价。

### 4.1 `/cwd <path>` 详细行为

- 解析 `path`（支持 `~` 展开）
- 验证 path 是已存在的目录
- 验证 agent（默认 claude）可执行
- 调 `SessionManager.Create(chatID, path, agent, [])`
- 返回 "Session started in {path}" 或错误

**已存在 session 时**：
- v0.1：拒绝，回复 "session already active in {path}, /kill first to switch"

### 4.2 `/start <agent> [args...]` 详细行为

- `args[0]` 必须是一个已注册的 agent name（claude / codex / opencode 等）
- `args[1:]` 是要透传给 agent 的额外参数（**nightme 不解析**）
- 默认 workspace = `$HOME`（如果该 chat 之前没有 `/cwd`）
- 验证 agent 二进制存在（`agent.Detect()`）
- 调 `SessionManager.Create(chatID, workspace, agent, extraArgs)`
- 返回 "Session started: {agent} {args}, cwd={workspace}"

**关键点**：
- **args 透明透传**：nightme 不验证 / 不清理 / 不解释 agent 命令的 flag
- 用户的 `/start claude --model opus` → spawn `claude --model opus`
- 用户的 `/start codex --sandbox workspace-write` → spawn `codex --sandbox workspace-write`
- agent CLI 自己负责 args 合法性（不符合会自己报错）

**已存在 session 时**：
- v0.1：拒绝（同 `/cwd`）

**示例**：
| 输入 | 行为 |
|------|------|
| `/start claude` | spawn `claude` in $HOME |
| `/start codex` | spawn `codex` in $HOME |
| `/start claude --model opus` | spawn `claude --model opus` in $HOME |
| `/start codex --full-auto` | spawn `codex --full-auto` in $HOME |
| `/cwd /tmp/foo` then `/start codex` | spawn `codex` in /tmp/foo（/cwd workspace 优先）|
| `/start foo` | 报错 "unknown agent: foo" |
| `/start` | 报错 "usage: /start <agent> [args...]" |

**跟 `/cwd` 的协作**：
- 如果 chat 内之前发过 `/cwd <path>`，`/start` 使用该 workspace
- 如果 chat 内之前**没**发过 `/cwd`，`/start` 使用 `$HOME`
- 反过来同理（`/start` 先于 `/cwd`：`/cwd` 也使用 `$HOME`，但 agent 来自 `/start`）

### 4.3 `/help` 详细行为

返回格式（飞书用 markdown）：
```
Available commands:
/cwd <path>          Create session in workspace
/start <agent> [args] Create session with specific CLI + args
/help                Show this help
/kill                Terminate current session

Anything else (including unknown /-commands) is sent to the agent.
```

**已知 trade-off**：nightme 的 `/help` 会拦截 agent 的 `/help`。

### 4.4 `/kill` 详细行为

- 调 `SessionManager.Kill(sessionID)` → `bridge.Close()` → SIGTERM → 等 5s → SIGKILL
- 标记 session 为 `exited`
- 从 active session 移除
- 回复 "session killed"

## 5. 透传语义详解（重要）

nightme 只在**表 4** 列出的 4 个命令上拦截。其他所有以 `/` 开头的输入都透传：

| 用户输入 | nightme 表命中？ | 行为 |
|----------|-----------------|------|
| `/cwd /tmp/foo` | ✅ | nightme 创建 session |
| `/start codex --flag` | ✅ | nightme 创建 session（透传 `--flag` 给 codex）|
| `/help` | ✅ | nightme 列命令 |
| `/kill` | ✅ | nightme kill session |
| `/workspace /tmp/foo` | ✅（alias）| 等同 `/cwd` |
| `/clear` | ❌ | 透传 → agent 收到 `/clear` |
| `/compact` | ❌ | 透传 → agent 收到 `/compact` |
| `/foo` | ❌ | 透传 → agent 收到 `/foo` |
| `hello` | — | 透传 → agent 收到 `hello` |
| `/cwd`（无参数）| ✅ | nightme 报 usage 错误 |

## 6. Edge cases

| 场景 | 处理 |
|------|------|
| 用户消息以 `/` 开头但不是合法 nightme 命令 | **透传**给 agent（不拒绝） |
| `/cwd` 无参数 | nightme 报 "usage: /cwd <path>" |
| `/cwd /nonexistent` | nightme 报 "workspace does not exist: /nonexistent" |
| `/start foo`（未知 agent）| nightme 报 "unknown agent: foo" |
| `/start codex --bad-flag` | nightme 透传，codex 自己报错 |
| `/cwd` 但 session 已存在 | nightme 报 "session already active, /kill first" |
| `/start` 但 session 已存在 | nightme 报 "session already active, /kill first" |
| 用户发空消息（""）| 丢弃，不传给 PTY |
| 中文 slash command（如 `/帮助`）| v0.1 不支持，会**透传**给 agent |
| `/` 单独一个字符 | Parser 失败 → 视为普通文本，透传 |
| `//cmd`（双斜杠）| v0.1 不识别为 escape，第一个 `/` 后整个 `/cmd` 当 agent command？需确认实现 |

## 7. Test plan

**单元测试**：
- `ParseCommand("/cwd /tmp/foo")` → `("cwd", ["/tmp/foo"], nil)`
- `ParseCommand("/start codex --flag")` → `("start", ["codex", "--flag"], nil)`
- `ParseCommand("/help")` → `("help", [], nil)`
- `ParseCommand("/foo")` → `("foo", [], nil)`（**不报错**——只是没命中 nightme 表）
- `ParseCommand("not a command")` → 标记 Consumed=false
- `ParseCommand("")` → 标记 Consumed=false
- `ParseCommand("/")` → 标记 Consumed=false（fallback 处理）

**集成测试**：
- Gateway.Handle(普通消息) → fallback 被调
- Gateway.Handle("/cwd /tmp/foo") → cwd handler 被调
- Gateway.Handle("/start claude") → start handler 被调，fallback 未被调
- Gateway.Handle("/start claude --model opus") → start handler 被调，args = ["claude", "--model opus"]
- Gateway.Handle("/foo") → **fallback 被调**（未命中透传）
- Gateway.Handle("/clear") → fallback 被调（agent 命令透传）

**手动 / E2E（M2）**：
- 飞书 DM 发 `/help` → bot 回复 nightme 命令列表
- 飞书 DM 发 `/cwd /tmp/foo` → bot 回复 "Session started"
- 飞书 DM 发 `/start codex --full-auto` → bot 回复 "Session started: codex --full-auto"
- 飞书 DM 发 `/clear` → claude/codex 收到 `/clear` 字面量 → 自己处理
- 飞书 DM 发 `/foo` → claude/codex 收到 `/foo`（可能当文本显示）
- 飞书 DM 发 `hello` → agent 收到 `hello`
- 验证：spawn 的进程命令行真的是 `claude --model opus`（用 `ps aux | grep`）

## 8. Open questions

- 是否支持 `/cwd -` 切回上一个 workspace？v0.1 不支持
- 是否支持 `/start -` 切回上一个 agent？v0.1 不支持
- v0.2 是否引入 `//escape` 语法让用户把 `/<cmd>` 字面量发给 nightme？v0.2 评估
- v0.2 是否需要 namespace（如 `/nm/kill`）？取决于 agent slash command 冲突程度
- args 含空格怎么办？v0.1 按空格切分（限制），v0.2 加引号支持
- `//cmd` 是否要识别为 escape syntax（让 nightme 收到 `/cmd` 字面量）？v0.1 不识别
