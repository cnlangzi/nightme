# F-20: Command Gateway (Slash Command Router)

> **Status**: designed (v0.1)
> **Milestone**: M2 (used by all chat input)
> **Depends on**: F-08 (Channel), F-01 (Session)
> **Used by**: 所有 IM → nightme 的输入
> **Related docs**: SPEC.md §1.1 (Gateway 组件), §2.1 (input flow)

## 1. Description

**Gateway** 是 nightme 在 Channel Adapter 和 Session Manager 之间的**命令路由器**。它判断每条进来的 IM 消息是：

- **系统级 slash command**（以 `/` 开头）→ 查命令表 → 命中执行 / 不命中报错
- **普通文本**（不以 `/` 开头）→ 透传给 Session Manager → 写入 PTY stdin

Gateway 的存在让用户能用**显式语法**控制 nightme（创建 session、kill session、查询帮助），不再依赖"看消息内容像不像指令"的脆弱文字识别。

## 2. Interface

```go
// internal/gateway/gateway.go
package gateway

type Command struct {
    Name        string                                          // "cwd", "kill", "help"
    Aliases     []string                                        // ["workspace"] 别名
    Description string                                          // 帮助文本
    Handler     func(ctx context.Context, msg *Message, args []string) (*CommandResult, error)
}

type CommandResult struct {
    Reply     string  // 回复给用户的文本
    Consumed  bool    // true = 已处理，false = 让 fallback 处理
    Terminate bool    // true = 终止 session（如 /kill）
}

type FallbackHandler func(ctx context.Context, msg *Message) error

type Gateway interface {
    // Register 注册一个 slash command
    Register(cmd Command)

    // Handle 是 Gateway 的主入口，每条 IM 消息进来都调一次
    Handle(ctx context.Context, msg *Message) error

    // ListCommands 返回所有已注册命令的描述（用于 /help）
    ListCommands() []Command
}

func New(fallback FallbackHandler) *Gateway
```

## 3. Implementation

**文件**：
- `internal/gateway/gateway.go` — Gateway 接口 + 实现
- `internal/gateway/parser.go` — slash command 解析（`/cmd arg1 arg2` → name + args）
- `internal/gateway/commands.go` — v0.1 命令注册（cwd / kill / help）

**流程**：
```
Channel Adapter.Incoming() → Message
  ↓
Gateway.Handle(ctx, msg)
  ├─ text 不以 "/" 开头 → fallback(msg) → SessionManager → PTY stdin
  └─ text 以 "/" 开头
      ├─ ParseCommand(text) → (name, args)
      ├─ 查 commands[name]
      │   ├─ 命中 → Command.Handler(ctx, msg, args) → 回复用户
      │   └─ 不命中 → 回复 "unknown command: /xxx, try /help"
      └─ done
```

**关键设计决策**：
- **严格匹配 `/` 前缀**：不以 `/` 开头的消息永远透传。避免误判（用户消息含 "workspace:" 不是指令）
- **未知 slash command 报错，不透传**：避免歧义。如果用户写 `/foo`，明确报错"未知命令"而不是发给 PTY 当普通文本
- **`/` 命令优先级高于文字**：用户可以用任何内容当 PTY 输入，**只要不以 `/` 开头**

**Parser 行为**：
- `/cmd` → name="cmd", args=[]
- `/cmd arg1 arg2` → name="cmd", args=["arg1", "arg2"]
- `/cmd "arg with space"` → name="cmd", args=["arg with space"]（v0.2 加引号支持）
- `/  cmd  ` → 忽略前导 / 尾部空白
- `//cmd` → 不识别（只有第一个 `/` 是 prefix，其余是字面量）

## 4. v0.1 命令集

| 命令 | 参数 | 行为 | Session 要求 |
|------|------|------|--------------|
| `/cwd` | `<path>` | 创建 session（cwd = path）；返回 "Session started in {path}" | 必须**没有** active session |
| `/help` | (无) | 返回所有已注册命令的列表和描述 | 任意 |
| `/kill` | (无) | 终止当前 session；返回 "session killed" | 必须有 active session |

**别名**：`/workspace` 是 `/cwd` 的别名（更友好）。两者等价。

### 4.1 `/cwd <path>` 详细行为

- 解析 `path`（支持 `~` 展开）
- 验证 path 是已存在的目录
- 验证 agent（默认 claude）可执行
- 调 `SessionManager.Create(chatID, path, agent)`
- 返回 "Session started in {path}" 或错误

**已存在 session 时**：
- v0.1：拒绝，回复 "session already active in {path}, /kill first to switch"
- v0.2 可能加 `/force` flag

### 4.2 `/help` 详细行为

返回格式（飞书用 markdown）：
```
Available commands:
/cwd <path>     Create session in workspace
/help           Show this help
/kill           Terminate current session

Anything else is sent to the active session's terminal.
```

### 4.3 `/kill` 详细行为

- 调 `SessionManager.Kill(sessionID)` → `bridge.Close()` → SIGTERM → 等 5s → SIGKILL
- 标记 session 为 `exited`
- 从 active session 移除
- 回复 "session killed"

## 5. Edge cases

| 场景 | 处理 |
|------|------|
| 用户消息以 `/` 开头但不是合法命令（如 `/foo`）| 报错 "unknown command: /foo, try /help" |
| `/cwd` 无参数 | 报错 "usage: /cwd <path>" |
| `/cwd /nonexistent` | 报错 "workspace does not exist: /nonexistent" |
| `/cwd` 但 session 已存在 | 报错 "session already active, /kill first" |
| 用户发空消息（""）| 丢弃，不传给 PTY |
| 用户发 `/help` 但 Gateway 出错 | 兜底回复 "internal error, please retry" |
| 用户狂发 `/` 命令超过 Channel QPS 限制 | Channel adapter 内部 rate limit |
| `/cwd` 后 agent spawn 失败 | 报错 "failed to spawn agent: {err}"，session 创建回滚 |
| 中文 slash command（如 `/帮助`）| v0.1 不支持，必须英文 |
| 同一 chat 收到 `/cwd` + 同时收到文字消息 | Gateway 串行处理，不并发 |

## 6. Test plan

**单元测试**：
- `ParseCommand("/cwd /tmp/foo")` → `("cwd", ["/tmp/foo"], nil)`
- `ParseCommand("/help")` → `("help", [], nil)`
- `ParseCommand("/foo")` → `("", nil, ErrUnknownCommand)`
- `ParseCommand("not a command")` → 标记 Consumed=false（fallback 处理）
- `ParseCommand("")` → 标记 Consumed=false

**集成测试**：
- Gateway.Handle(普通消息) → fallback 被调
- Gateway.Handle("/cwd /tmp/foo") → cwd handler 被调，无 fallback
- Gateway.Handle("/foo") → 回复 "unknown command"，无 fallback

**手动 / E2E（M2）**：
- 飞书 DM 发 `/help` → bot 回复命令列表
- 飞书 DM 发 `/cwd /tmp/foo` → bot 回复 "Session started"
- 飞书 DM 发 `hello` → claude 收到 "hello"
- 飞书 DM 发 `/kill` → bot 回复 "session killed"，claude 进程退出
- 飞书 DM 发 `/foo` → bot 回复 "unknown command"

## 7. Open questions

- 是否支持 `/cwd -` 切回上一个 workspace？v0.1 不支持
- 是否支持 `/env KEY=VALUE` 注入 agent 环境变量？v0.1 不支持（v0.2 F-19 改）
- 是否需要 `/status` 显示 session 状态？v0.1 不支持（用 `nightme list` CLI 替代）
- v0.2 是否引入 command 自动补全（飞书侧）？v0.1 不支持
- `/kill` 之外是否需要 `/restart`（重启 agent 但保留 cwd）？v0.1 不支持，v0.2 加
