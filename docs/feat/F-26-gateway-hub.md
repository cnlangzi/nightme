# F-26: Gateway Hub & Responsibility Isolation

> **Status**: ⚠️ **SUPERSEDED in v1.3** — see amendment below
> **Milestone**: v0.3 (commit 1-6 of "responsibility isolation" refactor)
> **Depends on**: F-08 (Channel), F-20 (Gateway command router), F-21 (agent modes), F-25 (rolling-log / input buffer)
> **Used by**: every Channel + every Agent
> **Related docs**: [`SPEC.md`](../SPEC.md) v1.3 §0.1, [`F-08-channel-abstraction.md`](./F-08-channel-abstraction.md), [`F-20-gateway.md`](./F-20-gateway.md), [`F-25-rolling-log.md`](./F-25-rolling-log.md)

> ## ⚠️ v1.3 amendment (2026-08-03)
>
> The original v1.1 design (this doc) had Gateway owning the **Receipt FSM** (per-userMsg `pending → executing → done/error`) and the Channel interface carrying `CreateReceipt / UpdateReceipt / DisposeReceipt` + `ReceiptState` enum. **v1.3 removes all of this.**
>
> Per SPEC §0.1 ("abstract stays abstract, concrete stays concrete"):
>
> - Gateway no longer owns any receipt FSM. The `receipts map[userMsgID]*receiptEntry` is gone.
> - `Channel` interface is now 5 methods: `Name / Start / Stop / Send / Incoming`. The three receipt-lifecycle methods are gone.
> - Each Channel owns its own receipt OBJECT internally (Feishu: `*MessageReceipt`; Slack: thread map; Web: DOM). Gateway sees none of it.
> - The `Receipt` opaque type and `ReceiptState` enum from `internal/receipt/receipt.go` are deleted. Only `MessageState` remains (independent concern for progress emoji).
> - Outbound routing: Gateway stamps `OutboundMessage.ReplyTo = currentTurnUserMsgID` (single anchor per turn). Channel looks up its own receipt by that userMsgID.
>
> **What this doc still describes accurately (v1.3)**:
>
> - §1 "responsibility isolation" principle (still true — Gateway is a router, Channel renders)
> - §2.1 "Channel owns Receipt rendering" — accurate; just the Receipt object is now entirely Channel-private (no opaque handle crosses the boundary)
> - §2.2 "what each layer does not know" — strengthened; Gateway knows even less about Channel internals
> - §2.3 "single-consumer rule" — still true; the EventCallback pattern is preserved
> - §3.1 "what is NOT in the Channel interface" — accurate (and the receipt lifecycle API is also gone now)
>
> **What is now WRONG / removed in v1.3**:
>
> - §2.1 "Gateway owns Receipt FSM" row — removed
> - §2.2 Channel interface "exposes `Receipt` opaque type + `ReceiptState` enum" — removed; channel exposes 5 methods only
> - §2.4 "Receipt data flow (v1.1)" — describes dead code; see [`F-25-rolling-log.md`](./F-25-rolling-log.md) §3 for the v1.3 replacement
> - §2.5 "OutboundMessage.ReplyTo contract (v1.1)" — fanout model replaced by 1:1 anchor (SPEC §2.2)
> - §3 "Channel interface change (the receipt-lifecycle extension)" — entire section describes removed API
> - §4.2 "Receipt table operations" — describes removed Gateway methods
> - §5 migration stages — receipt-lifecycle commits (3, 4, 6) superseded; see git log for the v1.3 removal commits
>
> **Migration pointer**: For the v1.3 outbound flow, see [`SPEC.md`](../SPEC.md) §2.2 + §2.4 and [`F-25-rolling-log.md`](./F-25-rolling-log.md).

---

## 1. Description

This doc is the **authoritative reference for the v1.1 responsibility-isolation refactor**. It exists because the refactor was large enough that scattered cross-references in SPEC.md / F-08 / F-20 / F-25 are not enough — anyone touching the three layers (Channel / Gateway / Session) needs to read this first.

**v1.1 core invariant** (one line): **Channel and Session are mutually ignorant; everything between them is routed through Gateway**.

---

## 2. The v1.1 architecture (responsibility isolation)

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

### 2.3 The single-consumer rule (v0.2.x bug fix)

`session.Events()` chan has **exactly one consumer**: the `MemoryManager.readPump` goroutine spawned at `Create()` time. Gateway does **not** spawn a separate `pumpOutbound` goroutine to read from `Events()` (the v0.2.x approach, which had two readers racing on the same channel).

Instead, the `MemoryManager` takes an `EventCallback(s *Session, ev AgentEvent)` at construction time. The callback is invoked synchronously from inside the `readPump` goroutine, after the InputBuffer FSM transition. Gateway registers its `onSessionEvent` method as the callback at startup.

**Why this matters**:
- Single-consumer removes the v0.2.x race where readPump and pumpOutbound both pulled from `Events()` and each event went to only one of them
- InputBuffer FSM is updated **before** the callback fires, so Gateway's translation always sees the correct buffer state
- Backpressure is natural: slow channel.Send blocks the callback, blocks readPump, blocks `as.Events()`, blocks the bridge, blocks the CLI

### 2.4 Receipt data flow (v1.1)

The `Receipt` is an **opaque type**. Gateway holds it as `channel.Receipt` (interface); the concrete type is `*feishu.MessageReceipt` or `*echo.messageReceipt` (or future channels' types). Gateway treats it as a token — never reads or writes fields.

**v1.1 模型：receipt 按 `userMsgID` 索引**（不在 chatID）。一个用户消息 = 一个 receipt。多 receipt/chat 可共存（buffered batch），每个 receipt 镇定到自己的用户消息。

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

// v1.1 outbound: 1 request : n response fan-out.
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

### 2.5 OutboundMessage.ReplyTo contract (v1.1)

Gateway 的事件路由遵循 **1 request : n response**：每个用户发起的会话都有一个 `userMsgID`；Gateway 在转发 agent event 时总是带上它（`OutboundMessage.ReplyTo` 字段）。Channel 根据这个字段决定镇定点。

```go
// internal/gateway/messages.go — v1.3 update
type OutboundMessage struct {
    ChatID  string
    Kind    OutboundKind
    Text    string
    Card    *Card
    // MessageState 承载 OutMessageState kind 的 payload（v1.3 新增）。
    // 详见 docs/feat/F-31-message-state.md。
    MessageState *MessageStatePayload
    // Reaction 保留向后兼容但 v1.3 后 OutMessageState 不再使用此字段。
    Reaction *Reaction
    ReplyTo string      // v1.1: userMsgID 镇定；"" 表示"真正无镇"
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

这个二分设计是为了防止 v0.3 的"跨用户消息折叠"bug——一个聊天下 fallback 路径与 agent event 路径被强制隔开。

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

    // New in v1.1 — receipt lifecycle rendering:
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
- `CreateReceipt`: build receipt text from blocks (via Feishu helper), post the receipt message, return `*MessageReceipt{messageID, replyMsgID}`. **v1.3 变更**：不再 add ⏳ reaction — reaction 由 MessageState FSM 负责（详见 F-31），与 Receipt 解耦。
- `UpdateReceipt(_, _, Pending)`: 仅更新 receipt 内部 state，不操作 reaction
- `UpdateReceipt(_, _, Executing)`: 仅更新 receipt 内部 state + PATCH card body（event count / timestamp）
- `UpdateReceipt(_, _, Done)`: 仅更新 receipt 内部 state + PATCH card body 为最终结果
- `UpdateReceipt(_, _, Error)`: 仅更新 receipt 内部 state + PATCH card body 为错误态
- `DisposeReceipt`: delete the receipt message (or no-op if channel UI prefers to keep)

**Reaction 触发转移（v1.3）**：v1.x 的 "CreateReceipt add ⏳ reaction / UpdateReceipt swap reaction" 全部下放给 MessageState FSM（详见 F-31）。Receipt FSM 不再触发任何 reaction — MessageState 与 Receipt 解耦后,两者走各自的 Channel.Send dispatcher 路径。

**Echo implementation** (in `internal/channel/echo/echo.go`):
- All three methods are logging-only: print `[receipt <userMsgID>] state=<state>` lines to stdout. Echo channel never returns errors from these (no network backend).

### 3.1 What is NOT in the Channel interface (deliberately)

- `MarkExecuting / MarkDone / MarkError` — replaced by `UpdateReceipt(_, _, ReceiptState)`
- `BuildForwardedText(blocks)` — channel takes blocks directly in `CreateReceipt`
- `ReceiptHandle` exposed fields — gateway never reads; pure opaque
- `ChatID` on receipt — channel knows the chat it created the receipt in

### 3.2 Channel.Send dispatch (v1.1)

`Channel.Send(OutboundMessage)` 是路由分发点。根据 `OutboundMessage.Kind` 和 `ReplyTo` 字段分发：

```go
// internal/channel/feishu/adapter.go — Send dispatcher
func (a *Adapter) Send(ctx context.Context, msg gateway.OutboundMessage) error {
    switch msg.Kind {
    case gateway.OutMessageState, gateway.OutMessageStateRemoved, OutCard, OutTyping:
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
3. **Receipt 按 userMsgID 索引**：多 receipt/chat 共存 (buffered batch)。Eviction 逻辑从 v0.3 中删除（不适用多 receipt 模型）；各 receipt 独立 lifecycle。
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

    // v1.1 additions:
    bindings map[string]*BindingEntry  // chatID → binding
    receipts map[string]*receiptEntry  // userMsgID → receipt

    // v1.3 additions: MessageState event hook.
    // Registered into ChatSession via SetMessageStateHandler at startup.
    // ChatSession calls onMessageState(chatID, userMsgID, state) at
    // lifecycle events; we translate to OutboundMessage and forward
    // via Channel.Send. See docs/feat/F-31-message-state.md.
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

// OnMessageState is the v1.3 ChatSession-callback entry point. The
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
| `Unbind(chatID)` | not used (bindings are permanent) | reserved for v0.4 multi-session |
| `RestoreBindings([]BindingEntry)` | manager.RestoreBindings step | bulk-load from registry |

### 4.2 Receipt table operations

| Op | Where | Side effects |
|----|-------|-------------|
| `Create(chatID, sessID, rcpt)` | fallback flow (a) | adds to map |
| `Flip(userMsgID, state)` | fallback flow (d) + onInputBufferFlush + onSessionEvent | updates entry.state; **Channel.UpdateReceipt called inside the flip** |
| `Dispose(userMsgID)` | onSessionEvent on EventResult/Error | calls Channel.DisposeReceipt, removes from map |

### 4.3 /run is Gateway's logic (v1.1 statement)

`/run <agent> [args]` does **not** call `manager.Run(chatID, agent)`. That method was the leak — it took a `chatID` and implicitly did a binding lookup inside the Manager. v1.1 removes it.

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

`manager.Create` signature (v1.1):
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

## 5. Session Manager interface (v1.1 slim)

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

**Removed from v1.1** (these leaked chat_id into session):
- `CreateOrUpdate(chatID, chatType, workspace, agent, args)`
- `Run(chatID, agent, extraArgs)`
- `GetByChat(chatID)`
- `KillByChat(chatID)`
- `MarkDetached(id)` — was process-aware; **kept** because it doesn't take chat_id

`Session` struct (v1.1):
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

## 6. Migration stages (the commit plan that landed v1.1)

| Commit | Scope | Risk | Behaviour preservation |
|--------|-------|------|-----------------------|
| **1** | Channel interface: add `CreateReceipt / UpdateReceipt / DisposeReceipt` + `ReceiptState` enum + `Receipt` opaque type. Feishu adapter implements. Echo implements. No business logic change. | Low (additive) | E2E identical |
| **2** | Session slim-down: remove `ChatID`, `ChatType`, `OnUserMessage` from Session struct. Remove `CreateOrUpdate`, `Run`, `GetByChat`, `KillByChat` from Manager interface. Remove `feishu` import from session package. **Session tests updated**; gateway/cmd still bridges via runtime closure (temporary). | Medium | E2E identical (manager still works because runtime translates chat→session) |
| **3** | Gateway gets `bindings` table + `receipts` table. New methods: `Bind / Rebind / LookupByChat / LookupSessionByChat / SpawnAgent`. Gateway handlers (`/cwd` / `/run` / `/kill`) rewritten to use them. Fallback rewritten to use `ch.CreateReceipt` + `sess.QueueUserMessage` + `ch.UpdateReceipt(executing)`. **Delete** the `SessionManager` interface in `gateway/cmd/handlers.go`. | High (largest single change) | E2E must be byte-identical for slash commands; receipt UI may shift slightly (closer to v1.1 design) |
| **4** | Single-consumer fix: gateway `pumpOutbound` goroutine removed. `Manager.EventCallback` registered at startup. Callback drives `Translate` + `Channel.Send` + receipt flip on `EventResult` / `EventError`. | Medium (lifecycle change) | This is the v0.2.x bug fix; output flow may have been silently broken before |
| **5** | Registry: add `BindingEntry` table. Restore order: sessions first, then bindings. Old v0.2.x registry files migrate by extracting `ChatID` from `SessionEntry` into a synthetic `BindingEntry{ChatID, SessionID}`. | Medium (data shape change) | All previously persisted state recoverable |
| **6** | Docs (PRD/SPEC/FEATURES/F-08/F-20/F-25) updated to v1.1 shape. (This is the commit you are reading the spec for.) | Low | N/A |

Each commit is its own PR. Commits 3-4 should ship together — a half-done refactor leaves the runtime in an inconsistent state where Gateway has `bindings` but Session still holds `ChatID`.

---

## 7. Behaviour preserved by the refactor

- ✅ Slash commands (`/cwd`, `/run`, `/kill`, `/help`, `/agents`)
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

## 8. Behaviour new in v1.1

- ➕ Channel interface has explicit receipt lifecycle hooks (rendering only — state is Gateway's)
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

## 10. Out of scope (v0.3 / v1.1)

- Retry queue / dead-letter — per Devin, "送达 = sent to target"
- Real second IM (Slack/WhatsApp/Telegram) — Stage 4 ships echo only
- Cross-channel bridge (F-11) — requires Channel multiplexing in Gateway; defer to v0.4
- Web UI / TTY (F-16) — separate effort
- DM `/sessions` and `/switch` commands (planned v0.3) — independent of responsibility isolation; can land after v1.1 ships

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

- `nightme run --channel=feishu`: all v0.2.x slash commands work; reply strings identical.
- `nightme run --channel=echo`: receipt UI shows `[receipt <id>] state=pending|executing|done|error` lines in order.
- `nightme run` then `/cwd` → `/run` → message → CLI reply: receipt transitions match the receipt FSM diagram in §2.4.

---

## 12. Rollout status

| Stage | Status | Tag |
|-------|--------|-----|
| Stage 1 (interface extension) | ✅ committed | pre-v1.1 |
| Stage 2 (session slim-down) | ✅ committed | pre-v1.1 |
| Stage 3 (gateway binding + receipt) | ✅ committed | v1.1 |
| Stage 4 (single-consumer fix) | ✅ committed | v1.1 |
| Stage 5 (registry + bindings) | ✅ committed | v1.1 |
| Stage 6 (docs) | ✅ this commit | v1.1 |

**Branch strategy**: `refactor/responsibility-isolation` was the integration branch; rebased onto `main` as each commit landed. v0.3 release tag carries the full v1.1 shape.

---

## 13. Change log

- **2026-08-02** — v1.1.2 (this commit): §2.4 receipt data flow 重写为 1 request : n response 扇出模型 (gateway.receiptsForSession → N 条 OutboundMessage，各带不同 ReplyTo)。新增 §2.5 OutboundMessage.ReplyTo contract 表 + fan-out 示例 + Channel 严格二分语义。新增 §3.2 Channel.Send dispatch 三分枝 (global / anchored-with-receipt / cold-start / orphan-plain-text)。
- **2026-08-02** — v1.1 final: responsibility isolation locked. SPEC.md bumped to v1.1. This doc rewritten to be the authoritative reference (was previously Stage 2 design notes).
- **2026-08-01** — Stage 2 design sketched (Gateway becomes central hub). Replaced by v1.1.