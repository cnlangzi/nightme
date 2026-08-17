# Command Gateway + 职责隔离

## A1. F-20: Command Gateway (Slash Command Router + Binding + Receipt FSM)

> **Source**: `F-gateway.md`


> **Depends on**: F-08 (Channel), F-01 (Session), F-09 (Agent)

> **Related docs**: [`SPEC.md`](../SPEC.md)§1.2, §2.1, §2.2, §2.3, §2.4; [`F-gateway.md`](./F-gateway.md) §4; [`F-08-channel-abstraction.md`](./F-08-channel-abstraction.md)

---

## 1. Description

**Gateway** 是 nightme 的**中枢 orchestrator**，在 Channel Adapter 和 Session Manager 之间：

1. **Slash command 路由**——判断每条 IM 消息是 nightme 命令、agent 命令还是普通文本
2. **Binding 表 owner**——`chat_id ↔ session_id` + `chat_type` + denormalized `workspace` / `agent`
3. **Run FSM owner**——`/run` 触发 spawn / reconnect 决策
4. **Receipt FSM owner**——每个 userMsgID 的 `pending → executing → done/error` 状态转移
5. **Channel↔Session 跨层调度**——所有 inbound / outbound 事件都过 Gateway

**核心原则**：nightme 只拦截**真正需要 session 管理**的命令。其他 slash 命令属于 agent namespace，nightme 透传不拒绝。

**职责再定义**：Gateway 不知道 IM 协议细节（Feishu API / reaction / message id），不调用 agent 内部协议（PTY/ACP/JSON-IO），不持久化任何 runtime state（registry 是另一层）。它**只**通过 Channel interface 和 Session Manager interface 与两边对话。

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

    // additions — binding + receipt FSM:

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
- **目的**：sweeper 5s tick → 检测新 session → 注册 callback 到 manager（如未注册）

`SweepSessions` 在 返回 `[]Session`（无 channel chan）：

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
- `internal/gateway/cmd/handlers.go` — `/cwd /run /close /help /agents` handler 实现

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
     │   ├ 命中表 (/cwd /run /close /help /agents) → handler(msg)
     │   │   └ handler 内部走 gateway.bindings → manager.Create/Run/Kill → ch.Send
     │   └ 未命中 / 普通文本 → messageDispatcher(msg)
     └ handler 或 messageDispatcher 走 Receipt FSM（见 §5）
```

**关键设计决策**：
- **未命中命令透传，不拒绝**——避免跟 agent 的 slash commands 冲突
- **`/` 前缀不等于 nightme 命令**——nightme 的责任范围**只限于 session 管理**
- **slash command 命中后，即使参数错误也由 nightme 报错**——因为这个命令确实属于 nightme 的 namespace
- **所有跨层协调（CreateReceipt + QueueUserMessage + UpdateReceipt）在 Gateway 皂 messageDispatcher 里**，不再在 channel adapter 里

**Parser 行为**：
- `/cmd` → name="cmd", args=[]
- `/cmd arg1 arg2` → name="cmd", args=["arg1", "arg2"]
- `/cmd "arg with space"` → 支持
- 解析失败的输入（如纯 `/` 后无字符）→ 视为普通文本，走 messageDispatcher

---

## 4. 命令集（nightme 的 namespace）

| 命令 | 参数 | 行为 | 前置条件 |
|------|------|------|----------|
| `/cwd` | `<path>` | 设置/更新 workspace + 创建 binding | 任意 |
| `/run` | `<agent> [args...]` | **确保 CLI 在跑**（spawn 或 attach）；binding 必须存在 | **binding 存在** |
| `/help` | (无) | 返回所有 nightme 命令列表 | 任意 |
| `/close` | (无) | 停止当前 CLI（保留 binding + Session record）| CLI 正在跑 |
| `/agents` | (无) | 列出已注册的 agent | 任意 |

**别名**：`/workspace` 是 `/cwd` 的别名。

### 4.1 `/cwd <path>` 详细行为

```
handler.cwd(ctx, msg, args)
  ├ args 为空 → 从 gateway.bindings[msg.ChatID] 读 workspace → Reply 当前 workspace
  └ args 有 → 验证 path
     ├ gateway.bindings[msg.ChatID] 查现有 binding
     │   ├ 存在 → 取 binding.SessionID → manager.Get → sess
     │   │   ├ sess != nil && sess.Status == Running → 拒绝（"CLI running, /close first"）
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
- 拒绝："CLI running, /close first to change workspace"

### 4.2 `/run <agent> [args...]` 详细行为

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
| 4 | `/close` | CLI 停止，binding 不动 |
| 5 | `/run claude --model opus` | spawn claude --model opus |
| 6 | `/run` 无参数 | "usage: /run <agent> [args...]" |
| 7 | `/run foo` | "unknown agent: foo" |

### 4.3 `/close` 详细行为

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
/close                Stop current CLI (keep session)
/agents              List registered agents

Workflow:
  1. /cwd /path/to/project
  2. /run claude
  3. ... work ...
  4. /close    (or restart with /run again)

Anything else (including unknown /-commands) is sent to the agent.
```

### 4.5 `/agents` 详细行为

```
handler.listAgents(ctx, msg, _)
  └ Reply "Registered agents: • claude — claude • codex — codex (app-server) • ..."（每个一行）
```

---

## 5. MessageDispatcher 流 + Receipt FSM

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
  │       InputBuffer FSM (see F-27 §5) 决定 dispatch (Idle) 或 buffer (Busy)
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
| `/close` | ✅ | nightme 停止 CLI |
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
| `/cwd <path>` 但 CLI 在跑 | 拒绝 "CLI running, /close first" |
| `/run` 前没发过 `/cwd` | "no workspace set, /cwd first" |
| `/run foo`（未知 agent）| "unknown agent: foo" |
| `/run codex --bad-flag` | 透传，codex 自己报错 |
| `/run` 时 CLI 死了（sess.Status == Exited）| spawn 新 CLI |
| `/run` 时 CLI 在跑（sess.Status == Running）| reconnect，不重启 |
| `/close` 但 binding 不存在 | "no session to kill" |
| `/close` 后 binding 保留，user 再 `/run` | 正常 spawn（Session record 复用）|
| nightme 重启后，binding 恢复 + session 标 Detached + PID 活着 | `/run` 时 sess.Status == Detached → spawn 新 CLI（覆盖之前的 PID，之前的 CLI 变孤儿）|
| nightme 重启后，binding 恢复 + session 标 Detached + PID 死了 | `/run` 时 spawn 新 CLI |
| CreateReceipt 失败（飞书 API 错）| log warn + ch.Send(OutText msg.Text)（degraded send 路径） |
| UpdateReceipt 失败 | Channel 内部 log；Gateway 不重试 |
| DisposeReceipt 失败 | Channel 内部 log；Gateway 已从 receipts 删除 |
| 多 channel 同时发同一 userMsgID | userMsgID 不唯一（应当 IM 原生唯一）→ 后到的覆盖前面，receipt map 替换 |
| 中文 slash command（如 `/帮助`）| 不支持，会**透传**给 agent |
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
- 飞书 DM 发 `/close` → "session killed"
- 飞书 DM 发 `/run claude --model opus` → "Started: claude --model opus"
- 飞书 DM 发 `/clear` → Claude 收到 `/clear`（透传）
- `ps aux | grep claude` → 验证进程命令行

---

## 9. Open questions

- `/cwd <path>` 在 CLI 跑着时能否 update workspace？拒绝；可加 `--force` 或先 kill
- `/run` 是否允许切换 agent？拒绝（必须 /close 后再 /run 新 agent），评估
- agent args 跟之前不同时，是否需要先 /close？智能：如果 CLI 在跑就不变（保持 agent 状态），如果死了才 spawn 新的
- `/run` 启动失败后 binding 状态？报错后 binding 保留但 Session record 标 Exited（用户可重试）
- 是否需要 `/forget` 命令清空 binding？不需要（/cwd 覆盖 + /close + /run 重启就够）

---

## 10. Cross-references

- **Channel interface**（receipt API 形状）：见 [`F-08-channel-abstraction.md`](./F-08-channel-abstraction.md) §2, §3
- **Session 数据模型**（不带 ChatID 的纯域）：见 [`F-runtime.md`](./F-runtime.md)
- **Registry 两张表**（SessionEntry + BindingEntry）：见 [`F-runtime.md`](./F-runtime.md)
- **InputBuffer FSM (see F-27 §5)**（Gateway 注入 onFlush 钩子）：见 [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md) §5
- **完整 架构**：见 [`F-gateway.md`](./F-gateway.md)

---

## 11. Change log

---

## A2. F-26: Gateway Hub & Responsibility Isolation

> **Source**: `F-gateway.md`


> **Depends on**: F-08 (Channel), F-20 (Gateway command router), F-21 (agent modes), F-25 (rolling-log / input buffer)

> **Related docs**: [`SPEC.md`](../SPEC.md)[`F-08-channel-abstraction.md`](./F-08-channel-abstraction.md), [`F-gateway.md`](./F-gateway.md), [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md)

---

## 1. Description

This doc is the **authoritative reference for the responsibility-isolation refactor**. It exists because the refactor was large enough that scattered cross-references in SPEC.md / F-08 / F-20 / F-25 are not enough — anyone touching the three layers (Channel / Gateway / Session) needs to read this first.

**core invariant** (one line): **Channel and Session are mutually ignorant; everything between them is routed through Gateway**.

---

## 2. The architecture (responsibility isolation)

### 2.1 Three layers, three FSMs, three owners

| Layer | FSM it owns | FSM data | Persistence |
|-------|-------------|----------|-------------|
| **Channel** (Feishu / echo / future) | **Receipt rendering** (visual interpretation of `ReceiptState`) | channel-private: `message_id`, `reaction_id`, content body | channel backend |
| **Gateway** | **Binding FSM** (chat ↔ session) + **Receipt FSM** (per userMsg) | `bindings map[chatID]*BindingEntry`, `receipts map[userMsgID]*receiptEntry` | BindingEntry persisted; receipts in-memory only |
| **Session Manager** | **InputBuffer FSM** (idle ↔ busy) + **Session.Status FSM** (running ↔ detached / exited) | `Session{ID, Workspace, Agent, Args, PID, Status}` + per-session `InputBuffer` | SessionEntry persisted; InputBuffer in-memory only |

### 2.2 What each layer **does not** know

| Layer | Does not know | Enforced by |
|-------|--------------|-------------|
| **Channel** | sessions, workspaces, agents, bindings, receipt state machine, chat → session mapping | Channel interface only exposes `Receipt` opaque type + `ReceiptState` enum |
| **Gateway** | IM protocol details (Feishu API specifics, message ids, reactions), agent internal protocol (PTY vs ACP vs JSON-IO), receipt rendering | Gateway only knows `Channel` interface + `SessionManager` interface; never imports `channel/feishu` or `bridge/*` |
| **Session** | chat_id, channel, receipt, binding relation, slash commands | Session struct has no `ChatID` field; Session package imports neither `channel/` nor `gateway/` |

### 2.3 The single-consumer rule

`session.Events()` chan has **exactly one consumer**: the `MemoryManager.readPump` goroutine spawned at `Create()` time. Gateway does **not** spawn a separate `pumpOutbound` goroutine to read from `Events()` (the approach, which had two readers racing on the same channel).

Instead, the `MemoryManager` takes an `EventCallback(s *Session, ev AgentEvent)` at construction time. The callback is invoked synchronously from inside the `readPump` goroutine, after the InputBuffer FSM transition. Gateway registers its `onSessionEvent` method as the callback at startup.

**Why this matters**:
- Single-consumer removes the race where readPump and pumpOutbound both pulled from `Events()` and each event went to only one of them
- InputBuffer FSM is updated **before** the callback fires, so Gateway's translation always sees the correct buffer state
- Backpressure is natural: slow channel.Send blocks the callback, blocks readPump, blocks `as.Events()`, blocks the bridge, blocks the CLI

### 2.4 Receipt data flow

The `Receipt` is an **opaque type**. Gateway holds it as `channel.Receipt` (interface); the concrete type is `*feishu.MessageReceipt` or `*echo.messageReceipt` (or future channels' types). Gateway treats it as a token — never reads or writes fields.

**模型：receipt 按 `userMsgID` 索引**（不在 chatID）。一个用户消息 = 一个 receipt。多 receipt/chat 可共存（buffered batch），每个 receipt 镇定到自己的用户消息。

**外发路径：1 request : n response 扇出**。Gateway 在转发每个 agent event 时，查询该 session 绑定的所有 receipt，为每个 receipt 发一条 `OutboundMessage{ReplyTo: receipt.userMsgID}`。Channel 据此决定怎么镇定（已有 receipt 就 in-place edit，无 receipt 就 ReplyMessage 创建新卡）。

```
Gateway code (pseudocode):

func (g *Gateway) onFallback(ctx, msg) error {
    sess := g.bindings[msg.ChatID].session  // may be nil
    if sess == nil || sess.Status() != Running { return reply(...) }

    // (a) Channel owns the receipt OBJECT; returns opaque handle
    rcpt, err := g.channel.CreateReceipt(ctx, msg.ChatID, msg.MessageID, msg.Blocks)
    if err != nil { return err }

    // (b) Gateway owns the receipt STATE
    g.receipts[msg.MessageID] = &receiptEntry{
        chatID: msg.ChatID, sessionID: sess.ID,
        receipt: rcpt, state: Pending,
    }

    // (c) Session owns the InputBuffer FSM (decides dispatch vs buffer)
    if err := sess.QueueUserMessage(msg.Blocks, msg.MessageID); err != nil {
        g.channel.UpdateReceipt(ctx, rcpt, ReceiptError)
        return err
    }

    // (d) Flip to Executing if dispatch was immediate (Buffer was Idle)
    //     If Busy, InputBuffer.onFlush (installed by Gateway) will flip it on flush.
    return nil
}

// outbound: 1 request : n response fan-out.
func (g *Gateway) translateAndSend(s *Session, ev AgentEvent) {
    chatID := g.lookupChatBySession(s.ID)
    ch := g.resolveChannel(chatID)
    out, send := Translate(chatID, ev)
    if !send { return }

    enrichOutboundMeta(out, s)   // OutInit.Meta 注入 agent_name / workspace / provider

    targets := g.receiptsForSession(s.ID)
    if len(targets) == 0 {
        // Orphan event (session 不绑任何 receipt)。以 plain text 发出。
        out.ReplyTo = ""
        ch.Send(ctx, out)
    } else {
        for _, umid := range targets {
            fanout := out
            fanout.ReplyTo = umid    // 同一 event、不同镇定
            ch.Send(ctx, fanout)
        }
    }

    if ev.Kind == EventResult || ev.Kind == EventError {
        for umid, entry := range g.receipts {
            if entry.sessionID != s.ID { continue }
            target := ReceiptDone
            if ev.Kind == EventError { target = ReceiptError }
            g.channel.UpdateReceipt(ctx, entry.receipt, target)
            _ = g.channel.DisposeReceipt(ctx, entry.receipt)
            delete(g.receipts, umid)
        }
    }
}

// receiptsForSession — 扇出列表 (1 agent turn 可能绑 N 个 receipt,
// 每个 userMsgID 在 buffered batch 中都是一个独立 receipt)
func (g *Gateway) receiptsForSession(sid string) []string {
    var ids []string
    for umid, entry := range g.receipts {
        if entry.sessionID == sid { ids = append(ids, umid) }
    }
    return ids
}

// InputBuffer.onFlush installed by Gateway on session creation:
func (g *Gateway) onInputBufferFlush(s *Session, blocks []ContentBlock, userMsgIDs []string) error {
    // Flip each queued receipt to Executing (now actually being sent)
    for _, umid := range userMsgIDs {
        if entry, ok := g.receipts[umid]; ok && entry.state == Pending {
            g.channel.UpdateReceipt(ctx, entry.receipt, ReceiptExecuting)
            entry.state = Executing
        }
    }
    return s.SendBlocks(ctx, blocks)
}

// Channel adapter — 1 request : n response 分发 (Feishu 实现)
func (a *Adapter) Send(ctx context.Context, msg OutboundMessage) error {
    if msg.ReplyTo == "" {
        // “真正无镇”—— plain text，不进任何 card
        return a.sendPlainText(ctx, msg.ChatID, msg.Text)
    }
    receipt := a.receiptFor(msg.ReplyTo)   // 按 userMsgID 查
    if receipt == nil {
        // cold-start：该 userMsgID 还没 receipt → ReplyMessage 创建镇定到该用户消息的新卡
        receipt = a.coldStartReceipt(ctx, msg)
        if receipt == nil { return nil }
    }
    return a.appendToReceipt(ctx, receipt, msg)   // UpdateMessage in place
}}

---

### 2.5 OutboundMessage.ReplyTo contract

Gateway 的事件路由遵循 **1 request : n response**：每个用户发起的会话都有一个 `userMsgID`；Gateway 在转发 agent event 时总是带上它（`OutboundMessage.ReplyTo` 字段）。Channel 根据这个字段决定镇定点。

```go
// internal/gateway/messages.go — update
type OutboundMessage struct {
    ChatID  string
    Kind    OutboundKind
    Text    string
    Card    *Card
    // MessageState 承载 OutMessageState kind 的 payload。
    // 详见 docs/channel/feishu-rendering.md。
    MessageState *MessageStatePayload
    // Reaction 保留向后兼容但 后 OutMessageState 不再使用此字段。
    Reaction *Reaction
    ReplyTo string      //  userMsgID 镇定；"" 表示"真正无镇"
    Meta    map[string]any
}

// MessageStatePayload 是 OutMessageState kind 的负载。
// Channel 从 Meta["message_id"] + Meta["state"] 也能读出相同信息；
// 这个 struct 是方便直接访问的冗余载体。
type MessageStatePayload struct {
    State receipt.MessageState  // 状态值
    Emoji string                // 可选：channel-specific 显式 emoji（override 推导）
}
```

**ReplyTo 语义表**：

| ReplyTo | Gateway 如何发出 | Channel 渲染逻辑 |
|---|---|---|
| `""` | 孤儿事件（session 不绑任何 receipt） | plain text，不进任何 card |
| `userMsgID` | 扇出（buffered batch 为每个 bound receipt 发一条） | reply-in-thread：有 receipt → in-place edit；无 receipt → cold-start ReplyMessage |

**Fan-out 路径**（buffered batch 示例：3 个 userMsgID 绑定同一个 session）：

```
Agent emit EventText → Translate 出 1 个 OutboundMessage（ReplyTo 未填）
             ↓
gateway.receiptsForSession(s.ID) → [userMsgID_a, userMsgID_b, userMsgID_c]
             ↓
同 1 个 OutboundMessage 拷贝 3 份，每份 ReplyTo 设为不同 userMsgID
             ↓
3 次 channel.Send — 每张 reply card 同步 in-place edit
```

**Channel 严格二分**（无 fallback 路径）：

- ReplyTo 非空 → 必须镇定到该 userMsgID（用 ReplyMessage API 或已有 receipt）
- ReplyTo 空 → plain text，不创建 receipt，不进任何 card

这个二分设计是为了防止 的"跨用户消息折叠"bug——一个聊天下 fallback 路径与 agent event 路径被强制隔开。

```

---

## 3. Channel interface change (the receipt-lifecycle extension)

```go
// internal/channel/channel.go — additive extension to existing interface

type Channel interface {
    // Existing (unchanged):
    Name() string
    Incoming() <-chan gateway.InboundMessage
    Send(ctx context.Context, msg gateway.OutboundMessage) error

    // New in — receipt lifecycle rendering:
    CreateReceipt(ctx context.Context, chatID, userMsgID string, blocks []agent.ContentBlock) (Receipt, error)
    UpdateReceipt(ctx context.Context, receipt Receipt, state ReceiptState) error
    DisposeReceipt(ctx context.Context, receipt Receipt) error
}

// Receipt is an opaque handle. Channel returns its own concrete type
// (e.g. *feishu.MessageReceipt). Gateway does not read or write fields.
type Receipt interface{}

// ReceiptState is the cross-channel state enum. Gateway is the only
// code that decides when to transition; Channel only renders.
type ReceiptState int
const (
    ReceiptPending   ReceiptState = iota  // ⏳
    ReceiptExecuting                      // 🔄
    ReceiptDone                           // ✅
    ReceiptError                          // ❌
)
```

**Feishu implementation** (in `internal/channel/feishu/adapter.go`):
- `CreateReceipt`: build receipt text from blocks (via Feishu helper), post the receipt message, return `*MessageReceipt{messageID, replyMsgID}`. **变更**：不再 add ⏳ reaction — reaction 由 MessageState FSM 负责（详见 F-31），与 Receipt 解耦。
- `UpdateReceipt(_, _, Pending)`: 仅更新 receipt 内部 state，不操作 reaction
- `UpdateReceipt(_, _, Executing)`: 仅更新 receipt 内部 state + PATCH card body（event count / timestamp）
- `UpdateReceipt(_, _, Done)`: 仅更新 receipt 内部 state + PATCH card body 为最终结果
- `UpdateReceipt(_, _, Error)`: 仅更新 receipt 内部 state + PATCH card body 为错误态
- `DisposeReceipt`: delete the receipt message (or no-op if channel UI prefers to keep)

**Reaction 触发转移**：v1.x 的 "CreateReceipt add ⏳ reaction / UpdateReceipt swap reaction" 全部下放给 MessageState FSM（详见 F-31）。Receipt FSM 不再触发任何 reaction — MessageState 与 Receipt 解耦后,两者走各自的 Channel.Send dispatcher 路径。

**Echo implementation** (in `internal/channel/echo/echo.go`):
- All three methods are logging-only: print `[receipt <userMsgID>] state=<state>` lines to stdout. Echo channel never returns errors from these (no network backend).

### 3.1 What is NOT in the Channel interface (deliberately)

- `MarkExecuting / MarkDone / MarkError` — replaced by `UpdateReceipt(_, _, ReceiptState)`
- `BuildForwardedText(blocks)` — channel takes blocks directly in `CreateReceipt`
- `ReceiptHandle` exposed fields — gateway never reads; pure opaque
- `ChatID` on receipt — channel knows the chat it created the receipt in

### 3.2 Channel.Send dispatch

`Channel.Send(OutboundMessage)` 是路由分发点。根据 `OutboundMessage.Kind` 和 `ReplyTo` 字段分发：

```go
// internal/channel/feishu/adapter.go — Send dispatcher
func (a *Adapter) Send(ctx context.Context, msg gateway.OutboundMessage) error {
    switch msg.Kind {
    case gateway.OutMessageState, gateway.OutMessageStateRemoved, OutChoice, OutTyping:
        // 全局 kinds — ReplyTo 无意义（message state / card / typing 是 chat-全局）
        return a.sendGlobal(ctx, msg)
    default:
        // agent-event-bearing kinds — 必须镇定到 ReplyTo
        return a.sendAnchored(ctx, msg)
    }
}

func (a *Adapter) sendAnchored(ctx context.Context, msg gateway.OutboundMessage) error {
    if msg.ReplyTo == "" {
        return a.sendUnanchored(ctx, msg)   // 孤儿事件：plain text / drop
    }
    receipt := a.receiptFor(msg.ReplyTo)
    if receipt == nil {
        receipt = a.coldStartReceipt(ctx, msg)   // ReplyMessage 创建镇定卡
        if receipt == nil { return nil }
    }
    return a.appendToReceipt(ctx, receipt, msg)   // UpdateMessage in place
}
```

**关键性质**：

1. **二分路径**：ReplyTo 空/非空 是唯一决定渲染路径的字段。Channel **不做** “找不到 receipt 就丢到共享 card” 这种 fallback。
2. **冷启动**：当一个 agent event 到达时该 userMsgID 还没有 receipt（理论上不应该发生——Gateway messageDispatcher 总是先 CreateReceipt，但边缘情况下 cold-start 是 defensive）。`coldStartReceipt` 调用 Feishu `ReplyMessage` API 发布初始 card 并注册 receipt。
3. **Receipt 按 userMsgID 索引**：多 receipt/chat 共存 (buffered batch)。Eviction 逻辑从 中删除（不适用多 receipt 模型）；各 receipt 独立 lifecycle。
4. **Orphan event 降级**：Receipt-only kinds (OutInit / OutUsage / OutCompaction) 在 ReplyTo 空时静默 drop——这些只对 receipt card 有意义，plain text 发送是无意义的。用户-facing kinds (OutText / OutThinking / OutResult / OutToolStart / OutToolEnd) 在 ReplyTo 空时降级为 plain text。

---

## 4. Gateway internal structure

```go
// internal/gateway/gateway.go — the new state

type gateway struct {
    mu       sync.RWMutex
    cmds     map[string]Command
    fb       FallbackHandler

    channels       []Channel
    channelCh      chan InboundMessage

    // additions:
    bindings map[string]*BindingEntry  // chatID → binding
    receipts map[string]*receiptEntry  // userMsgID → receipt

    // additions: MessageState event hook.
    // Registered into ChatSession via SetMessageStateHandler at startup.
    // ChatSession calls onMessageState(chatID, userMsgID, state) at
    // lifecycle events; we translate to OutboundMessage and forward
    // via Channel.Send. See docs/channel/feishu-rendering.md.
    onMessageState func(chatID, userMsgID string, state receipt.MessageState)

    // Runtime state:
    stopCh   chan struct{}
    stopOnce sync.Once
    wg       sync.WaitGroup
    chatToChan  map[string]Channel
    defaultChan Channel

    // Manager handles:
    manager session.Manager
    agents  *agent.Registry

    // Receipt FSM hook into session InputBuffer.onFlush
    onBufferFlush func(s *Session, blocks []agent.ContentBlock, userMsgIDs []string) error
}

// OnMessageState is the ChatSession-callback entry point. The
// runtime (cmd/nightme) wires gw.OnMessageState into every ChatSession
// at startup via SetMessageStateHandler. ChatSession calls it on
// lifecycle events (received / forwarded / done / error); Gateway
// translates to OutboundMessage{Kind: OutMessageState} and forwards
// to the appropriate channel via resolveChannel + Send.
func (g *gateway) OnMessageState(chatID, userMsgID string, state receipt.MessageState) {
    g.mu.RLock()
    ch := g.chatToChan[chatID]
    if ch == nil {
        ch = g.defaultChannel
    }
    g.mu.RUnlock()
    if ch == nil {
        return
    }
    out := OutboundMessage{
        Kind:   OutMessageState,
        ChatID: chatID,
        Meta: map[string]any{
            "message_id": userMsgID,
            "state":      state,
        },
    }
    if err := ch.Send(context.Background(), out); err != nil {
        // Fire-and-ack: log warn, never block ChatSession lifecycle.
        log.Printf("gateway: MessageState send failed (chat=%s, state=%s): %v", chatID, state, err)
    }
}

type BindingEntry struct {
    ChatID    string  // natural key
    ChatType  string  // p2p / group / thread (metadata only)
    SessionID string  // foreign key into manager.sessions
    Workspace string  // denormalized for /cwd reply and status
    Agent     string  // denormalized for /run reply
}

type receiptEntry struct {
    chatID    string
    sessionID string
    receipt   channel.Receipt  // opaque
    state     ReceiptState
}
```

### 4.1 Binding table operations

| Op | Where | Side effects |
|----|-------|-------------|
| `LookupByChat(chatID) → *BindingEntry` | all fallback / handler paths | read-only |
| `Bind(chatID, chatType, sess)` | `/cwd` handler on first creation | adds to map, persists BindingEntry |
| `Rebind(chatID, sess)` | `/cwd` handler on workspace update | replaces map entry, persists |
| `Unbind(chatID)` | not used (bindings are permanent) | reserved for multi-session |
| `RestoreBindings([]BindingEntry)` | manager.RestoreBindings step | bulk-load from registry |

### 4.2 Receipt table operations

| Op | Where | Side effects |
|----|-------|-------------|
| `Create(chatID, sessID, rcpt)` | fallback flow (a) | adds to map |
| `Flip(userMsgID, state)` | fallback flow (d) + onInputBufferFlush + onSessionEvent | updates entry.state; **Channel.UpdateReceipt called inside the flip** |
| `Dispose(userMsgID)` | onSessionEvent on EventResult/Error | calls Channel.DisposeReceipt, removes from map |

### 4.3 /run is Gateway's logic

`/run <agent> [args]` does **not** call `manager.Run(chatID, agent)`. That method was the leak — it took a `chatID` and implicitly did a binding lookup inside the Manager. removes it.

`/run` is now:
```
handler.run(ctx, msg, args):
    binding := gw.LookupByChat(msg.ChatID)
    if binding == nil:
        return reply("no workspace set, /cwd first")

    agentName := args[0]
    if gw.agents.Get(agentName) == error:
        return reply("unknown agent: " + agentName)

    sess, _ := gw.manager.Get(binding.SessionID)
    if sess.Status() == StatusRunning:
        return reply("Already running, pid=N")

    // Pure factory call — no chatID, no binding logic inside manager
    newSess, err := gw.manager.Create(ctx, CreateRequest{
        Workspace: binding.Workspace,
        Agent:     agentName,
        Args:      args[1:],
        OnFlushHook: gw.onInputBufferFlush,  // gateway installs the hook
    })

    // Update binding to point at the new Session
    gw.bindings[msg.ChatID].SessionID = newSess.ID
    gw.upsertBinding(gw.bindings[msg.ChatID])
    gw.manager.UpsertSession(newSess)

    return reply("Started: <agent>, pid=<N>, cwd=<ws>")
```

`manager.Create` signature:
```go
type CreateRequest struct {
    Workspace   string  // required
    Agent       string  // required
    Args        []string
    OnFlushHook func(s *Session, blocks []agent.ContentBlock, userMsgIDs []string) error
    // ^ gateway installs this; session stores it in InputBuffer.onFlush
}
```

The `OnFlushHook` is the **only** session → gateway callback surface. It fires when the InputBuffer transitions Busy → Idle and flushes its queued messages. Gateway uses the `userMsgIDs` to flip receipts from Pending → Executing.

---

## 5. Session Manager interface

```go
// internal/session/manager.go

type Manager interface {
    Create(ctx context.Context, req CreateRequest) (*Session, error)
    Get(id string) (*Session, error)
    List() []*Session
    Kill(id string) error
    Restore(ctx context.Context) error
    Persist() error
}
```

**Removed ** (these leaked chat_id into session):
- `CreateOrUpdate(chatID, chatType, workspace, agent, args)`
- `Run(chatID, agent, extraArgs)`
- `GetByChat(chatID)`
- `KillByChat(chatID)`
- `MarkDetached(id)` — was process-aware; **kept** because it doesn't take chat_id

`Session` struct:
```go
type Session struct {
    ID         string         // natural key
    Workspace  string         // immutable after Create
    Agent      string         // immutable after Create
    Args       []string
    PID        int            // 0 when Exited
    StartedAt  time.Time
    LastRunAt  time.Time

    status     Status         // Running / Detached / Exited
    exitCode   *int
    agentSession agent.AgentSession
    cancel       context.CancelFunc
    inputBuffer  *InputBuffer  // F-25

    // No ChatID, no ChatType, no OnUserMessage
}
```

---

## 6. Migration stages

| Commit | Scope | Risk | Behaviour preservation |
|--------|-------|------|-----------------------|
| **1** | Channel interface: add `CreateReceipt / UpdateReceipt / DisposeReceipt` + `ReceiptState` enum + `Receipt` opaque type. Feishu adapter implements. Echo implements. No business logic change. | Low (additive) | E2E identical |
| **2** | Session slim-down: remove `ChatID`, `ChatType`, `OnUserMessage` from Session struct. Remove `CreateOrUpdate`, `Run`, `GetByChat`, `KillByChat` from Manager interface. Remove `feishu` import from session package. **Session tests updated**; gateway/cmd still bridges via runtime closure (temporary). | Medium | E2E identical (manager still works because runtime translates chat→session) |
| **3** | Gateway gets `bindings` table + `receipts` table. New methods: `Bind / Rebind / LookupByChat / LookupSessionByChat / SpawnAgent`. Gateway handlers (`/cwd` / `/run` / `/close`) rewritten to use them. Fallback rewritten to use `ch.CreateReceipt` + `sess.QueueUserMessage` + `ch.UpdateReceipt(executing)`. **Delete** the `SessionManager` interface in `gateway/cmd/handlers.go`. | High (largest single change) | E2E must be byte-identical for slash commands; receipt UI may shift slightly (closer to design) |
| **4** | Single-consumer fix: gateway `pumpOutbound` goroutine removed. `Manager.EventCallback` registered at startup. Callback drives `Translate` + `Channel.Send` + receipt flip on `EventResult` / `EventError`. | Medium (lifecycle change) | This is the bug fix; output flow may have been silently broken before |
| **5** | Registry: add `BindingEntry` table. Restore order: sessions first, then bindings. Old registry files migrate by extracting `ChatID` from `SessionEntry` into a synthetic `BindingEntry{ChatID, SessionID}`. | Medium (data shape change) | All previously persisted state recoverable |
| **6** | Docs (PRD/SPEC/FEATURES/F-08/F-20/F-25) updated to shape. (This is the commit you are reading the spec for.) | Low | N/A |

Each commit is its own PR. Commits 3-4 should ship together — a half-done refactor leaves the runtime in an inconsistent state where Gateway has `bindings` but Session still holds `ChatID`.

---

## 7. Behaviour preserved by the refactor

- ✅ Slash commands (`/cwd`, `/run`, `/close`, `/help`, `/agents`)
- ✅ Inbound fallback to session (now via binding lookup)
- ✅ Feishu rolling-log with FIFO eviction (unchanged in Translate; Feishu adapter handles the same `OutboundMessage`s)
- ✅ Tool output surfacing (`✅ Read → 47 lines`)
- ✅ Thinking surfacing (`💭 I'll explore…`)
- ✅ Permission cards + Allow/Deny round-trip
- ✅ Bidirectional CLI logs (`received: …` + outbound trace)
- ✅ Registration pattern (`agent.Builtins`, `cmd/nightme/agents.go`)
- ✅ Session 1:1 binding to chat (binding table enforces same invariant)
- ✅ Default-detach on SIGTERM (`manager.MarkDetached(id)` — still exists, used by `cmd/nightme/shutdownRun` after iterating `manager.List()`)

---

## 8. Behaviour new in - ➕ Channel interface has explicit receipt lifecycle hooks (rendering only — state is Gateway's)
- ➕ Session is a **pure domain object** (no `ChatID`, no `feishu` import) — testable without any channel infrastructure
- ➕ Single-consumer event flow (no more double-reader race)
- ➕ Binding persistence is a separate table (`BindingEntry`) — survives registry schema migrations cleanly
- ➕ `/run` is Gateway's logic (was leaking into Manager before)
- ➕ `manager.Create` returns pure factory result — no implicit chat lookup

---

## 9. Behaviour removed

- ❌ `manager.GetByChat(chatID)` — replaced by `gateway.LookupByChat` → `manager.Get(binding.SessionID)`
- ❌ `manager.CreateOrUpdate(chatID, ...)` — replaced by `gateway.handler.cwd` doing binding + manager.Create explicitly
- ❌ `manager.Run(chatID, agent, args)` — replaced by `gateway.handler.run` doing binding lookup + manager.Create
- ❌ `session.Session.ChatID` / `ChatType` / `OnUserMessage` — moved to `BindingEntry` + `CreateRequest.OnFlushHook`
- ❌ `feishu.BuildForwardedTextFromBlocks(blocks)` called from session package — moved into `feishu` adapter's `CreateReceipt` internal helper
- ❌ Gateway's `pumpOutbound` goroutine reading `session.Events()` — replaced by `Manager.EventCallback`

---

## 10. Out of scope

- Retry queue / dead-letter — per Devin, "送达 = sent to target"
- Real second IM (Slack/WhatsApp/Telegram) — Stage 4 ships echo only
- Cross-channel bridge (F-11) — requires Channel multiplexing in Gateway; defer to - Web UI / TTY (F-16) — separate effort
- DM `/sessions` and `/switch` commands ( ) — independent of responsibility isolation; can land after ships

---

## 11. Test strategy

### 11.1 Unit

- **`session/`** tests rewritten to use `sess.ID` instead of `sess.ChatID`. No `channel/feishu` test deps.
- **`gateway/`** tests cover binding table: `Bind → LookupByChat → Rebind` round-trips; persistence.
- **`gateway/`** tests cover receipt table: `Create → Flip(Pending→Executing) → Flip(Executing→Done) → Dispose`; orphan disposal on EventError.
- **`channel/feishu/`** tests cover receipt interface: `CreateReceipt` returns handle, `UpdateReceipt` calls correct Feishu API per state, `DisposeReceipt` deletes the message.
- **`channel/echo/`** tests cover no-op receipt interface.

### 11.2 Integration

- Gateway + manager + fake channel: verify binding survives restart (registry write + read).
- Gateway + manager + fake channel: verify receipt FSM goes `Pending → Executing → Done` for an Idle dispatch.
- Gateway + manager + fake channel: verify receipt FSM goes `Pending → Executing → Done` for a Busy dispatch (via `onFlush` hook).
- Gateway + manager + fake channel: verify `EventError` flips receipt to `Error` and disposes.

### 11.3 Regression (E2E)

- `nightme run --channel=feishu`: all slash commands work; reply strings identical.
- `nightme run --channel=echo`: receipt UI shows `[receipt <id>] state=pending|executing|done|error` lines in order.
- `nightme run` then `/cwd` → `/run` → message → CLI reply: receipt transitions match the receipt FSM diagram in §2.4.

---

## 12. Rollout status

| Stage | Status | Tag |
|-------|--------|-----|
| Stage 1 (interface extension) | ✅ committed | pre-|
| Stage 2 (session slim-down) | ✅ committed | pre-|
| Stage 3 (gateway binding + receipt) | ✅ committed | |
| Stage 4 (single-consumer fix) | ✅ committed | |
| Stage 5 (registry + bindings) | ✅ committed | |
| Stage 6 (docs) | ✅ this commit | |

**Branch strategy**: `refactor/responsibility-isolation` was the integration branch; rebased onto `main` as each commit landed. release tag carries the full shape.

---

## 13. Change log

---

