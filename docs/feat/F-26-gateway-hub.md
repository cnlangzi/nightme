# F-26: Gateway Hub & Responsibility Isolation

> **Status**: implemented (v1.1 architectural pivot; v0.3 release tag carries the new shape)
> **Milestone**: v0.3 (commit 1-6 of "responsibility isolation" refactor)
> **Depends on**: F-08 (Channel), F-20 (Gateway command router), F-21 (agent modes), F-25 (input buffer)
> **Used by**: every Channel + every Agent
> **Related docs**: [`SPEC.md`](../SPEC.md) v1.1, [`F-08-channel-abstraction.md`](./F-08-channel-abstraction.md), [`F-20-gateway.md`](./F-20-gateway.md), [`F-25-input-buffer.md`](./F-25-input-buffer.md)

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

func (g *Gateway) onSessionEvent(s *Session, ev AgentEvent) {
    out := Translate(s.ChatID_or_lookup_from_binding, ev)  // → OutboundMessage
    g.channel.Send(ctx, out)

    if ev.Kind == EventResult || ev.Kind == EventError {
        // Find receipts bound to this session; close all that's still open.
        for userMsgID, entry := range g.receipts {
            if entry.sessionID != s.ID { continue }
            if ev.Kind == EventError {
                g.channel.UpdateReceipt(ctx, entry.receipt, ReceiptError)
            } else {
                g.channel.UpdateReceipt(ctx, entry.receipt, ReceiptDone)
            }
            g.channel.DisposeReceipt(ctx, entry.receipt)
            delete(g.receipts, userMsgID)
        }
    }
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
- `CreateReceipt`: build receipt text from blocks (via Feishu helper), post the receipt message, add ⏳ reaction, return `*MessageReceipt{messageID, reactionID, replyMsgID}`
- `UpdateReceipt(_, _, Pending)`: swap reaction to ⏳ (or add ⏳ if previously null)
- `UpdateReceipt(_, _, Executing)`: swap reaction ⏳ → 🔄
- `UpdateReceipt(_, _, Done)`: swap reaction → ✅; optionally edit receipt body to show final result
- `UpdateReceipt(_, _, Error)`: swap reaction → ❌
- `DisposeReceipt`: delete the receipt message (or no-op if channel UI prefers to keep)

**Echo implementation** (in `internal/channel/echo/echo.go`):
- All three methods are logging-only: print `[receipt <userMsgID>] state=<state>` lines to stdout. Echo channel never returns errors from these (no network backend).

### 3.1 What is NOT in the Channel interface (deliberately)

- `MarkExecuting / MarkDone / MarkError` — replaced by `UpdateReceipt(_, _, ReceiptState)`
- `BuildForwardedText(blocks)` — channel takes blocks directly in `CreateReceipt`
- `ReceiptHandle` exposed fields — gateway never reads; pure opaque
- `ChatID` on receipt — channel knows the chat it created the receipt in

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

- **2026-08-02** — v1.1 final: responsibility isolation locked. SPEC.md bumped to v1.1. This doc rewritten to be the authoritative reference (was previously Stage 2 design notes).
- **2026-08-01** — Stage 2 design sketched (Gateway becomes central hub). Replaced by v1.1.