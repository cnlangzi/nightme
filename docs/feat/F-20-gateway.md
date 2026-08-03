# F-20: Command Gateway (Slash Command Router + Binding + Receipt FSM)

> **Status**: implemented (v1.1 — Gateway owns binding table, Run logic, receipt FSM)
> **Milestone**: M2 (slash routing), v0.3 (binding + receipt FSM)
> **Depends on**: F-08 (Channel), F-01 (Session), F-09 (Agent)
> **Used by**: 所有 IM → nightme 的输入
> **Related docs**: [`SPEC.md`](../SPEC.md) v1.1 §1.1, §1.2, §2.1, §2.2, §2.3, §2.4; [`F-26-gateway-hub.md`](./F-26-gateway-hub.md) §4; [`F-08-channel-abstraction.md`](./F-08-channel-abstraction.md)

---

## 1. Description

**Gateway** 是 nightme 的**中枢 orchestrator**，在 Channel Adapter 和 Session Manager 之间：

1. **Slash command 路由**——判断每条 IM 消息是 nightme 命令、agent 命令还是普通文本
2. **Binding 表 owner**——`chat_id ↔ session_id` + `chat_type` + denormalized `workspace` / `agent`
3. **Run FSM owner**——`/run` 触发 spawn / reconnect 决策
4. **Receipt FSM owner**——每个 userMsgID 的 `pending → executing → done/error` 状态转移
5. **Channel↔Session 跨层调度**——所有 inbound / outbound 事件都过 Gateway

**核心原则**：nightme 只拦截**真正需要 session 管理**的命令。其他 slash 命令属于 agent namespace，nightme 透传不拒绝。

**v1.1 职责再定义**：Gateway 不知道 IM 协议细节（Feishu API / reaction / message id），不调用 agent 内部协议（PTY/ACP/JSON-IO），不持久化任何 runtime state（registry 是另一层）。它**只**通过 Channel interface 和 Session Manager interface 与两边对话。

---

## 2. Interface

```go
// internal/gateway/gateway.go
package gateway

import (
    "context"

    "github.com/cnlangzi/nightme/internal/agent"
    "github.com/cnlangzi/nightme/internal/channel"
    "github.com/cnlangzi/nightme/internal/session"
)

type Command struct {
    Name        string
    Aliases     []string
    Description string
    Handler     func(ctx context.Context, msg *InboundMessage, args []string) (*CommandResult, error)
}

type CommandResult struct {
    Reply    string
    Consumed bool
}

type MessageDispatcher func(ctx context.Context, msg *InboundMessage) error

// Gateway is the public contract exposed by the gateway package.
type Gateway interface {
    // Slash command registration:
    Register(cmd Command) bool

    // Handle dispatches one inbound message through the command table,
    // calling the MessageDispatcher when no command matches.
    Handle(ctx context.Context, msg *InboundMessage) (*CommandResult, error)

    ListCommands() []Command

    // v1.1 additions — binding + receipt FSM:

    // LookupByChat returns the binding for a chat, or nil.
    LookupByChat(chatID string) *BindingEntry

    // LookupSessionByChat returns the session bound to chatID, or nil.
    LookupSessionByChat(chatID string) *session.Session

    // SpawnAgent creates a new Session in the bound workspace, replacing
    // the binding's SessionID. /run uses this.
    SpawnAgent(ctx context.Context, chatID, agentName string, extraArgs []string) (*session.Session, error)

    // OnSessionEvent is the callback Manager invokes from inside its
    // readPump. Translates agent events and sends to Channel. Flips
    // receipts on EventResult / EventError.
    OnSessionEvent(s *session.Session, ev agent.AgentEvent)
}

// BindingEntry is the chat_id → session_id mapping Gateway owns.
// Persisted to registry.File as a separate table.
type BindingEntry struct {
    ChatID    string  // natural key
    ChatType  string  // p2p / group / thread (metadata only)
    SessionID string  // FK into manager.sessions
    Workspace string  // denormalized for /cwd reply and /status
    Agent     string  // denormalized for /run reply
}

// receiptEntry is Gateway's per-userMsg bookkeeping for the receipt FSM.
type receiptEntry struct {
    chatID    string
    sessionID string
    receipt   channel.Receipt  // opaque handle from channel.CreateReceipt
    state     channel.ReceiptState
}
```

### 2.1 OutboundSource / SweepSessions (Stage 2 evolution)

保留 Stage 2 引入的 `OutboundSource` / `SweepSessions` 机制，但**目的**变了：

- **Stage 2 目的**：sweeper 5s tick → 发现新 session → 起 pumpOutbound goroutine 读 `session.Events()`
- **v1.1 目的**：sweeper 5s tick → 检测新 session → 注册 callback 到 manager（如未注册）

`SweepSessions` 在 v1.1 返回 `[]Session`（无 channel chan）：

```go
type SweepSessions func() []*session.Session

// Gateway attaches sweeper via:
func (g *gateway) AttachSweeper(s SweepSessions)
```

callback 注册在 `OnSessionEvent`，由 `manager.SetEventCallback(...)` 在 startup 时一次设入。

---

## 3. Implementation

**文件**：
- `internal/gateway/gateway.go` — Gateway 接口 + 实现 + dispatcher runtime
- `internal/gateway/parser.go` — slash command 解析
- `internal/gateway/translate.go` — `agent.AgentEvent → gateway.OutboundMessage`
- `internal/gateway/messages.go` — `InboundMessage / OutboundMessage / OutboundKind` 等
- `internal/gateway/cmd/handlers.go` — `/cwd /run /kill /help /agents` handler 实现

**核心流程**：
```
Channel Adapter.Incoming() → InboundMessage
  ↓
Gateway.pumpInbound (per-channel goroutine)
  ├ chatToChan[chatID] = ch
  └ push 到 channelCh (cap 64)
  ↓
Gateway.dispatchLoop (single goroutine)
  └ Handle(ctx, msg)
     ├ ParseCommand(msg.Text)
     │   ├ 命中表 (/cwd /run /kill /help /agents) → handler(msg)
     │   │   └ handler 内部走 gateway.bindings → manager.Create/Run/Kill → ch.Send
     │   └ 未命中 / 普通文本 → messageDispatcher(msg)
     └ handler 或 messageDispatcher 走 Receipt FSM（见 §5）
```

**关键设计决策**：
- **未命中命令透传，不拒绝**——避免跟 agent 的 slash commands 冲突
- **`/` 前缀不等于 nightme 命令**——nightme 的责任范围**只限于 session 管理**
- **slash command 命中后，即使参数错误也由 nightme 报错**——因为这个命令确实属于 nightme 的 namespace
- **v1.1：所有跨层协调（CreateReceipt + QueueUserMessage + UpdateReceipt）在 Gateway 皂 messageDispatcher 里**，不再在 channel adapter 里

**Parser 行为**：
- `/cmd` → name="cmd", args=[]
- `/cmd arg1 arg2` → name="cmd", args=["arg1", "arg2"]
- `/cmd "arg with space"` → v0.2+ 支持
- 解析失败的输入（如纯 `/` 后无字符）→ 视为普通文本，走 messageDispatcher

---

## 4. v0.1 命令集（nightme 的 namespace）

| 命令 | 参数 | 行为 | 前置条件 |
|------|------|------|----------|
| `/cwd` | `<path>` | 设置/更新 workspace + 创建 binding | 任意 |
| `/run` | `<agent> [args...]` | **确保 CLI 在跑**（spawn 或 attach）；binding 必须存在 | **binding 存在** |
| `/help` | (无) | 返回所有 nightme 命令列表 | 任意 |
| `/kill` | (无) | 停止当前 CLI（保留 binding + Session record）| CLI 正在跑 |
| `/agents` | (无) | 列出已注册的 agent | 任意 |

**别名**：`/workspace` 是 `/cwd` 的别名。

### 4.1 `/cwd <path>` 详细行为（v1.1）

```
handler.cwd(ctx, msg, args)
  ├ args 为空 → 从 gateway.bindings[msg.ChatID] 读 workspace → Reply 当前 workspace
  └ args 有 → 验证 path
     ├ gateway.bindings[msg.ChatID] 查现有 binding
     │   ├ 存在 → 取 binding.SessionID → manager.Get → sess
     │   │   ├ sess != nil && sess.Status == Running → 拒绝（"CLI running, /kill first"）
     │   │   └ 否则 → 更新 binding.Workspace + 重 spawn（如果有 workspace）
     │   └ 不存在 → 继续
     ├ agentName = (binding 现有 agent) || (Registry.List() 第一个) || "claude"
     ├ workspace.Validate(path)（~ 展开、绝对路径、目录存在）
     ├ call manager.Create(ctx, CreateRequest{
     │       Workspace: abs,
     │       Agent:     agentName,
     │       Args:      nil,
     │       OnFlushHook: g.onInputBufferFlush,  // gateway 装入
     │   }) → Session{ID, Workspace, Agent, PID, Status=Running}
     ├ gateway.bindings[msg.ChatID] = BindingEntry{ChatID, ChatType, SessionID, Workspace, Agent}
     ├ registry.Upsert(SessionEntry) + registry.Upsert(BindingEntry)
     └ Reply "Workspace set to <abs>. Send /run <agent> to start CLI."
```

**回复模板**：
- 首次创建："Workspace set to {path}. Send /run <agent> to start CLI."
- 更新："Workspace updated to {path}."
- 拒绝："CLI running, /kill first to change workspace"

### 4.2 `/run <agent> [args...]` 详细行为（v1.1：Run 是 Gateway 的逻辑）

```
handler.run(ctx, msg, args)
  ├ args 为空 → Reply usage
  ├ agentName := args[0]
  ├ agent.Registry.Get(agentName) 校验
  │   └ 失败 → Reply "unknown agent: {name}"
  ├ binding := gateway.LookupByChat(msg.ChatID)
  │   └ nil → Reply "no workspace set, send /cwd <path> first"
  ├ sess := manager.Get(binding.SessionID)
  ├ sess.Status == Running → Reply "Already running (pid=N)"
  └ 否则：
     ├ newSess := manager.Create(ctx, CreateRequest{
     │       Workspace:   binding.Workspace,
     │       Agent:       agentName,
     │       Args:        args[1:],
     │       OnFlushHook: g.onInputBufferFlush,
     │   }) → 纯 factory 调用，不接收 chatID
     ├ gateway.bindings[msg.ChatID].SessionID = newSess.ID
     ├ registry 更新两条 entry
     └ Reply "Started: <agent>, pid=<N>, cwd=<ws>"
```

**为什么"智能"**：
- 用户不需要记"CLI 现在跑没跑"
- Gateway 根据 `sess.Status` 自动决定 spawn / reconnect
- **绝不无故重启正在跑的 CLI**（避免丢失 agent 内部状态）

**args 透传**：
- `args[0]` = agent name
- `args[1:]` = 额外参数，原样透传给 agent CLI
- nightme 不解析 / 不验证 / 不 sanitize

**回复模板**：

| 场景 | Reply |
|------|-------|
| CLI 没在跑，成功启动 | "Started: `{agent} {args}`, cwd=`{workspace}`" |
| CLI 已在跑，reconnect | "Already running (pid={pid}). Connected." |
| workspace 没设 | "no workspace set, send /cwd <path> first" |
| agent 未知 | "unknown agent: {name}" |
| agent 二进制找不到 | "{name} binary not found, please install" |

**示例**：

| 顺序 | 输入 | 行为 |
|------|------|------|
| 1 | `/cwd /tmp/foo` | binding 创建 + session spawn |
| 2 | `/run claude` | spawn claude in /tmp/foo |
| 3 | `/run claude --model opus` | CLI 在跑 → reconnect（不动 args）|
| 4 | `/kill` | CLI 停止，binding 不动 |
| 5 | `/run claude --model opus` | spawn claude --model opus |
| 6 | `/run` 无参数 | "usage: /run <agent> [args...]" |
| 7 | `/run foo` | "unknown agent: foo" |

### 4.3 `/kill` 详细行为（v1.1）

```
handler.kill(ctx, msg, _)
  ├ binding := gateway.LookupByChat(msg.ChatID)
  │   └ nil → Reply "no session to kill"
  ├ sess := manager.Get(binding.SessionID)
  ├ sess.Status == Running → manager.Kill(binding.SessionID)
  │       manager 内部：close agent session + SetStatus(Exited) + upsert SessionEntry
  └ Reply "session killed (was: {agent}, cwd={workspace})"
```

**关键**：kill 只停止 CLI，**binding + Session record 保留**。`/run` 可以复用 Session record + workspace 重新 spawn。

### 4.4 `/help` 详细行为

```
handler.help(ctx, msg, _)
  ├ 从 ctx 拿 gateway（dispatchLoop 装的）
  ├ cmds := gw.ListCommands()
  └ Reply help text（飞书 markdown）
```

```
Available commands:
/cwd <path>          Set workspace (session-level)
/run <agent> [args]  Ensure CLI running (spawn or attach)
/help                Show this help
/kill                Stop current CLI (keep session)
/agents              List registered agents

Workflow:
  1. /cwd /path/to/project
  2. /run claude
  3. ... work ...
  4. /kill    (or restart with /run again)

Anything else (including unknown /-commands) is sent to the agent.
```

### 4.5 `/agents` 详细行为

```
handler.listAgents(ctx, msg, _)
  └ Reply "Registered agents: • claude — claude • codex — codex-acp • ..."（每个一行）
```

---

## 5. MessageDispatcher 流 + Receipt FSM（v1.1 核心）

```
messageDispatcher(ctx, msg)
  ├ binding := gateway.LookupByChat(msg.ChatID)
  │   ├ nil → ch.Send(OutText "no workspace, /cwd first")
  │   └ sess := manager.Get(binding.SessionID)
  │       └ sess.Status != Running → ch.Send(OutText "CLI not running, /run first")
  │
  ├ userMsgID := msg.MessageID
  │   └ 空 → userID + ":" + msg.Time.UTC().Format(RFC3339Nano)
  ├ blocks := msg.Blocks  // 或由 channel 在 InboundMessage 里直接给
  │
  ├ (a) rcpt, err := g.channel.CreateReceipt(ctx, msg.ChatID, userMsgID, blocks)
  │       ├ err != nil → log warn + ch.Send(OutText msg.Text) → return  // degraded send path (no receipt)
  │       └ err == nil → g.receipts[userMsgID] = {chatID, sess.ID, rcpt, Pending}
  │
  ├ (c) err := sess.QueueUserMessage(blocks, userMsgID)
  │       InputBuffer FSM 决定 dispatch (Idle) 或 buffer (Busy)
  │       ├ Idle → 立即 SendBlocks(blocks)
  │       │   └ 立即 → ch.UpdateReceipt(rcpt, Executing) → state: Executing
  │       └ Busy → 入队
  │           └ onFlush 钩子触发时（Gateway 装的）→ 批量 UpdateReceipt(Executing) + SendBlocks(combined)
  │
  └ return err
```

**`onInputBufferFlush` 实现**（Gateway 上的方法，挂在 InputBuffer）：
```
g.onInputBufferFlush(s *Session, blocks []ContentBlock, userMsgIDs []string) error
  ├ for _, umid := range userMsgIDs:
  │   ├ entry, ok := g.receipts[umid]
  │   ├ ok && entry.state == Pending:
  │   │   └ ch.UpdateReceipt(ctx, entry.receipt, ReceiptExecuting); entry.state = Executing
  └ return s.SendBlocks(ctx, blocks)
```

**EventCallback（在 manager.readPump 里调用）**：
```
g.OnSessionEvent(s *Session, ev AgentEvent)
  ├ chatID := g.lookupChatBySession(s.ID)  // 反查 binding
  ├ out, send := Translate(chatID, ev)
  ├ if send:
  │   └ ch.Send(ctx, out)  // fire-and-ack
  ├ if ev.Kind == EventResult || ev.Kind == EventError:
  │   ├ 反查 g.receipts[userMsgID] for each entry with entry.sessionID == s.ID
  │   ├ for each:
  │   │   ├ ev.Kind == EventError → ch.UpdateReceipt(rcpt, Error)
  │   │   └ else → ch.UpdateReceipt(rcpt, Done)
  │   ├ ch.DisposeReceipt(rcpt)
  │   └ delete(g.receipts, userMsgID)
  └ return
```

---

## 6. 透传语义详解（保持不变）

nightme 只拦截**表 4** 列出的 5 个命令。其他所有以 `/` 开头的输入都透传：

| 用户输入 | nightme 表命中？ | 行为 |
|----------|-----------------|------|
| `/cwd /tmp/foo` | ✅ | nightme 设置 workspace + 创建 binding |
| `/run claude` | ✅ | nightme spawn / reconnect CLI |
| `/help` | ✅ | nightme 列命令 |
| `/kill` | ✅ | nightme 停止 CLI |
| `/workspace /tmp/foo` | ✅（alias）| 等同 `/cwd` |
| `/clear` | ❌ | 透传 → agent 收到 `/clear` |
| `/compact` | ❌ | 透传 → agent 收到 `/compact` |
| `/foo` | ❌ | 透传 → agent 收到 `/foo` |
| `hello` | — | 透传 → agent 收到 `hello` |

---

## 7. Edge cases

| 场景 | 处理 |
|------|------|
| `/cwd /nonexistent` | "workspace does not exist: /nonexistent" |
| `/cwd <path>` 但 CLI 在跑 | 拒绝 "CLI running, /kill first" |
| `/run` 前没发过 `/cwd` | "no workspace set, /cwd first" |
| `/run foo`（未知 agent）| "unknown agent: foo" |
| `/run codex --bad-flag` | 透传，codex 自己报错 |
| `/run` 时 CLI 死了（sess.Status == Exited）| spawn 新 CLI |
| `/run` 时 CLI 在跑（sess.Status == Running）| reconnect，不重启 |
| `/kill` 但 binding 不存在 | "no session to kill" |
| `/kill` 后 binding 保留，user 再 `/run` | 正常 spawn（Session record 复用）|
| nightme 重启后，binding 恢复 + session 标 Detached + PID 活着 | `/run` 时 sess.Status == Detached → spawn 新 CLI（覆盖旧 PID，旧 CLI 变孤儿）|
| nightme 重启后，binding 恢复 + session 标 Detached + PID 死了 | `/run` 时 spawn 新 CLI |
| CreateReceipt 失败（飞书 API 错）| log warn + ch.Send(OutText msg.Text)（degraded send 路径） |
| UpdateReceipt 失败 | Channel 内部 log；Gateway 不重试 |
| DisposeReceipt 失败 | Channel 内部 log；Gateway 已从 receipts 删除 |
| 多 channel 同时发同一 userMsgID | userMsgID 不唯一（应当 IM 原生唯一）→ 后到的覆盖前面，receipt map 替换 |
| 中文 slash command（如 `/帮助`）| v0.1 不支持，会**透传**给 agent |
| 群聊消息无 sender | binding 仍然 1 个（binding 是 chat → session，sender 不参与）|

---

## 8. Test plan

### 8.1 Unit

**Parser**:
- `ParseCommand("/cwd /tmp/foo")` → `("cwd", ["/tmp/foo"], nil)`
- `ParseCommand("/run claude --model opus")` → `("run", ["claude", "--model", "opus"], nil)`
- `ParseCommand("/foo")` → `("foo", [], nil)`（**不报错**）
- `ParseCommand("not a command")` → Consumed=false
- `ParseCommand("")` → Consumed=false

**Gateway state**:
- `Bind → LookupByChat` round-trip
- `Rebind` 替换 workspace / agent
- `SpawnAgent` 在 binding 缺失时返回 error
- `SpawnAgent` 在 sess.Running 时是 no-op
- `receipts` table: `Create → Flip Pending→Executing → Flip Executing→Done → Dispose` 全流程

**Handlers**:
- `handler.cwd` 无 args → 读 binding.Workspace 返回
- `handler.cwd` 新 chat → 创建 binding + spawn
- `handler.cwd` 已有 binding 但 CLI 没跑 → 更新 binding.Workspace
- `handler.cwd` 已有 binding 且 CLI 在跑 → 拒绝
- `handler.run` 无 args → usage error
- `handler.run` chat 无 binding → "no workspace"
- `handler.run` chat 有 binding (sess.Exited) → spawn
- `handler.run` chat 有 binding (sess.Running) → "already running"
- `handler.run` 未知 agent → error
- `handler.kill` binding 不存在 → "no session to kill"
- `handler.kill` sess.Exited → no-op + reply

### 8.2 Integration

- Gateway + MemoryManager + mock Channel: `/cwd` → binding.Upsert → registry.Upsert x2
- Gateway + MemoryManager + mock Channel: `/run` → manager.Create → binding.SessionID 更新
- Gateway + MemoryManager + mock Channel: messageDispatcher (Idle) → CreateReceipt → UpdateReceipt(Executing)
- Gateway + MemoryManager + mock Channel: messageDispatcher (Busy) → CreateReceipt → queued → onFlush → UpdateReceipt(Executing) × N
- Gateway + MemoryManager + mock Channel: EventResult → 反查 receipts → UpdateReceipt(Done) + DisposeReceipt

### 8.3 E2E（M2+）

- 飞书 DM 发 `/cwd /tmp/foo` → "Workspace set"
- 飞书 DM 发 `/run claude` → "Started: claude, cwd=/tmp/foo"
- 飞书 DM 发 `hello` → receipt ⏳ → 🔄（立即执行）→ ✅
- 飞书 DM 发多条消息（Claude 忙） → 所有 receipt ⏳，buffer flush 后同时变 🔄
- 飞书 DM 发 `/run claude` → "Already running (pid=12345)"
- 飞书 DM 发 `/kill` → "session killed"
- 飞书 DM 发 `/run claude --model opus` → "Started: claude --model opus"
- 飞书 DM 发 `/clear` → Claude 收到 `/clear`（透传）
- `ps aux | grep claude` → 验证进程命令行

---

## 9. Open questions

- `/cwd <path>` 在 CLI 跑着时能否 update workspace？v1.1 拒绝；v0.4 可加 `--force` 或先 kill
- `/run` 是否允许切换 agent？v1.1 拒绝（必须 /kill 后再 /run 新 agent），v0.4 评估
- agent args 跟之前不同时，是否需要先 /kill？v1.1 智能：如果 CLI 在跑就不变（保持 agent 状态），如果死了才 spawn 新的
- `/run` 启动失败后 binding 状态？v1.1 报错后 binding 保留但 Session record 标 Exited（用户可重试）
- 是否需要 `/forget` 命令清空 binding？v1.1 不需要（/cwd 覆盖 + /kill + /run 重启就够）

---

## 10. Cross-references

- **Channel interface**（receipt API 形状）：见 [`F-08-channel-abstraction.md`](./F-08-channel-abstraction.md) §2, §3
- **Session 数据模型**（不带 ChatID 的纯域）：见 [`F-01-session-create.md`](./F-01-session-create.md)
- **Registry 两张表**（SessionEntry + BindingEntry）：见 [`F-05-process-registry.md`](./F-05-process-registry.md)
- **InputBuffer FSM**（Gateway 注入 onFlush 钩子）：见 [`F-25-input-buffer.md`](./F-25-input-buffer.md) §5
- **完整 v1.1 架构**：见 [`F-26-gateway-hub.md`](./F-26-gateway-hub.md)

---

## 11. Change log

- **2026-08-02** — v1.1: Gateway 接管 binding 表 + Run 决策 + Receipt FSM。Manager interface 简化（移除 GetByChat/CreateOrUpdate/Run/KillByChat）。Session 失去 ChatID 字段。Doc 重写。
- **2026-07-31** — v0.1: 原始 slash command 路由。SessionManager 当时负责 `/run` 逻辑 + chat 查找。已被 v1.1 取代。