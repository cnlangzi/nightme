# ChatSession 会话模型 + 命令 (/cwd /use /new /close)

## A1. F-27: ChatSession Model

> **Source**: `F-chat-session.md`


> **Depends on**: F-26, F-08 (Channel), F-20 (Gateway)

> **Related docs**: [`SPEC.md`](../SPEC.md)/§1.3/§3, [`PRD.md`](../PRD.md)/§4.6

## 1. Description

`ChatSession` is the **persistent session context** bound 1:1 to an IM chat (chat_id). It replaces the legacy `Session` type into:

- **ChatSession** (this feature) — the persistent, per-chat state container
- **AgentSession** (F-29) — the per-CLI-process handle, pooled under ChatSession

The split exists because needs:
1. **Agent switching mid-conversation** — same chat, different agents
2. **Process preservation across switches** — switch agent, switch back, get the original process (and its context) back
3. **Persistence independence** — chat context (active cwd / agent) survives daemon restart; CLI processes may die and be respawned

---

## 2. Data structure

```go
// internal/chatsession/chatsession.go

type ChatSession struct {
    ID              string              // derived from chatID; natural key
    ChatID          string              // unique — gateway.bindings[ChatID] → this
    ChatType        string              // p2p | group | topic_group | ""

    // Active routing state (mutated by /cwd /use; primaryAgent is
    // captured at New() time from global config and never mutated).
    SelectedCwd       string              // /cwd sets; immutable per AgentSession
    SelectedAgent     string              // /use sets; immutable per AgentSession
    PrimaryAgent    string              // snapshot of cfg.Primary at New(); read-only

    // AgentSession pool (per-ChatSession unique on (agent, cwd))
    poolMu          sync.RWMutex
    pool            map[agentCwdKey]*AgentSession  // agent + cwd → AgentSession
    selectedAS        *AgentSession                  // current active (may be nil)

    // FSMs owned by ChatSession
    inputBuffer     *InputBuffer                   // F-25; idle ↔ busy; cross-/use shared
    // Receipt FSM is Gateway-owned (receipts[userMsgID]); ChatSession only forwards events

    // Persistence
    createdAt           time.Time
    lastInteractionAt   time.Time
    flushHook           func(blocks []agent.ContentBlock, userMsgIDs []string) error
                                          // Gateway injects; called on InputBuffer Busy→Idle
    eventCallback       func(s *AgentSession, ev agent.AgentEvent)
                                          // ChatSession injects into AgentSession.readPump

    // Lifecycle
    cancel              context.CancelFunc
}

type agentCwdKey struct {
    Agent string
    Cwd   string
}
```

**Persistence entry**:

```go
// internal/registry/chat_session_entry.go

type ChatSessionEntry struct {
    ID                  string    `json:"id"`
    ChatID              string    `json:"chatId"`               // UNIQUE
    ChatType            string    `json:"chatType"`
    SelectedCwd           string    `json:"selectedCwd"`            // empty → not yet /cwd'd
    SelectedAgent         string    `json:"selectedAgent"`          // empty → not yet /use'd
    PrimaryAgent        string    `json:"primaryAgent"`         // snapshot of cfg.Primary at creation; read-only
    AgentSessionIDs     []string  `json:"agentSessionIds"`      // pool index
    SelectedAgentSessionID *string   `json:"selectedAgentSessionId"` // null → no active
    CreatedAt           time.Time `json:"createdAt"`
    LastInteractionAt   time.Time `json:"lastInteractionAt"`
}
```

---

## 3. Lifecycle

### 3.1 Creation

```
Gateway.handler.cwd  (or first inbound msg if binding missing):
  1. registry.Bindings[chatId] check
     ├─ exists → load ChatSession (Restore), SetSelectedCwd
     └─ missing → ChatSession.Create({ChatID, ChatType, SelectedCwd})
                   → registry.Upsert(ChatSessionEntry)
                   → bindings[ChatID] = new ChatSession
```

### 3.2 Restoration (after daemon restart)

```
nightme run (startup):
  1. registry.LoadAll() → []ChatSessionEntry, []AgentSessionEntry
  2. For each ChatSessionEntry:
     ├─ new ChatSession (id, chatId, chatType, selectedCwd, selectedAgent, primaryAgent)
     ├─ For each agentSessionID:
     │   └─ new AgentSession (id, agent, cwd, status=Detached, pid=0)
     │       └─ chatSession.pool[(agent,cwd)] = agentSession
     └─ selectedAS = pool[(selectedAgent, selectedCwd)]  // may be nil if cwd changed
  3. bindings[ChatID] = restored ChatSession
```

**Detached state**: `pid=0, status=Detached`. Process not running; will be respawned on first message via `LookupSelectedAgentSession()`.

### 3.3 Active AgentSession lookup

```go
// LookupSelectedAgentSession is the ONLY entry point for resolving
// "which AgentSession should this message go to".
//
// Logic (single path — no runtime fallback):
//   - ChatSession always carries an effective selectedAgent:
//       · at construction, selectedAgent is seeded from cfg.Primary
//       · /use overwrites selectedAgent
//   - Resolve pool[(selectedAgent, selectedCwd)]:
//       · hit (StatusRunning) → reuse, register callback, return
//       · miss (or non-Running) → spawn (selectedAgent, selectedCwd)
//
// Returns ErrNoSelectedCwd if selectedCwd is empty (user has not /cwd'd yet).
func (cs *ChatSession) LookupSelectedAgentSession() (*AgentSession, error) {
    cs.poolMu.Lock()
    defer cs.poolMu.Unlock()

    if cs.SelectedCwd == "" {
        return nil, ErrNoSelectedCwd  // "no workspace, /cwd first"
    }

    key := agentCwdKey{Agent: cs.SelectedAgent, Cwd: cs.SelectedCwd}
    if as, ok := cs.pool[key]; ok {
        if as.Status() == StatusExited {
            // Process died; respawn with same (agent, cwd) entry
            as.Respawn(cs.SelectedCwd)
        }
        cs.selectedAS = as
        as.RegisterEventCallback(cs.eventCallback)
        return as, nil
    }

    // Miss: spawn (selectedAgent, selectedCwd). No fallback to any
    // "default" agent — chatSession.selectedAgent is the only
    // authority (seeded from cfg.Primary at New() time, /use
    // overrides it).
    as := SpawnAgentSession(cs.SelectedAgent, cs.SelectedCwd)
    cs.pool[key] = as
    as.RegisterEventCallback(cs.eventCallback)
    cs.selectedAS = as
    cs.flushAgentSessionsToRegistry()
    return as, nil
}
```

### 3.4 Slash command handlers

| Command | Handler behavior | Side effects |
|---------|------------------|--------------|
| `/cwd <path>` | Validate → `chatSession.SetSelectedCwd(abs)` | Updates `selectedCwd`; pool untouched; next message triggers `LookupSelectedAgentSession()` |
| `/use <agent>` | Validate → `chatSession.SetSelectedAgent(name)` → `LookupSelectedAgentSession()` | May spawn new AgentSession if `(agent, selectedCwd)` not in pool |
| `/close` | `chatSession.KillAll()` | Kills every AgentSession in pool; clears `selectedAS`; old receipts dispose |

**No `/default` command**: the only user-facing Primary Agent is the global `primary` config. The `primaryAgent` field on ChatSession is captured at `New()` time (snapshot of `cfg.Primary`) and never mutated post-construction. Future feature: per-chat Primary via config (not command) — ### 3.5 SetSelectedCwd / SetSelectedAgent (state mutations)

```go
func (cs *ChatSession) SetSelectedCwd(cwd string) error {
    if !isValidCwd(cwd) {
        return ErrInvalidCwd
    }
    cs.poolMu.Lock()
    cs.SelectedCwd = cwd
    cs.poolMu.Unlock()
    cs.flushEntryToRegistry()
    // No agent respawn here — LookupSelectedAgentSession happens lazily
    return nil
}

func (cs *ChatSession) SetSelectedAgent(agent string) error {
    cs.poolMu.Lock()
    cs.SelectedAgent = agent
    cs.poolMu.Unlock()
    cs.flushEntryToRegistry()
    return nil
}
```

**Critical invariant**: these methods MUST NOT spawn or kill any AgentSession. Spawning is lazy (next message). Killing is explicit (`/close`).

---

## 4. Concurrency model

### 4.1 Goroutines owned by ChatSession

- **None directly**. ChatSession is a state container; goroutines live in AgentSession (readPump per AgentSession).

### 4.2 Per-AgentSession goroutines

- **`AgentSession.readPump`** (one per AgentSession in pool, running or detached)
  - Reads from `as.Events()` (single consumer)
  - Calls `cs.eventCallback(s, ev)` if `s == cs.selectedAS` (set under poolMu read lock)
  - Drops events from non-active AgentSession (logs at debug level)

### 4.3 Locks

- `cs.poolMu` — guards `pool`, `selectedAS`, `SelectedCwd`, `SelectedAgent`
  - **Write**: `/cwd`, `/use`, `/close`, `LookupSelectedAgentSession()`, spawn, registry flush
  - **Read**: readPump callback registration, status queries

### 4.4 /use switch race window

When `/use` switches active:
```
1. /use claude enters LookupSelectedAgentSession
2. Take poolMu.Lock
3. Resolve new selectedAS (reuse or spawn)
4. Set cs.selectedAS = newAS
5. Release poolMu.Lock
6. (In flight events from OLD AgentSession's readPump, if any:)
   - Check cs.selectedAS under poolMu.RLock
   - If old, drop event (log: "event from non-active AgentSession, dropping")
7. New events flow through newAS.readPump → cs.eventCallback
```

**Race window**: between step 5 and step 6, old in-flight events are dropped. This is acceptable (PRD §4.3 "过时的不管").

---

## 5. FSM ownership summary

| FSM | Owner | Storage | Notes |
|-----|-------|---------|-------|
| Binding FSM (chat ↔ ChatSession) | Gateway | `Gateway.bindings[chatId] → ChatSession` (map) + ChatSessionEntry (persisted) | 1:1 永久绑定 |
| Receipt FSM (per userMsgID) | Gateway | `Gateway.receipts[userMsgID]` (map, in-memory) | 跨 /use /cwd 不变 |
| InputBuffer FSM (idle ↔ busy) | **ChatSession** | `ChatSession.inputBuffer` | 跨 /use 切换共享 queue |
| AgentSession.Status (running/detached/exited) | AgentSession | AgentSession.status (atomic) | 独立于 ChatSession |
| ChatSession.SelectedAgentSession pointer | ChatSession | `cs.selectedAS` (under poolMu) | /use 时切换 |

---

## 5.1 Runtime contracts (seams between ChatSession and the runtime)

ChatSession is pure data + FSM; it knows nothing about agents,
channels, or the gateway. The runtime injects three pieces of
behaviour that make the FSMs come alive.

### 5.1.1 Spawner (lazy fork-exec seam)

`Spawner` is the only way a ChatSession brings a new AgentSession
to life (Step 3 of `LookupSelectedAgentSession`). It is
**injected** via `ChatSession.WithSpawner(s)`; the runtime wires
the production implementation.

```go
// internal/chatsession/spawn.go

type Spawner interface {
    Spawn(ctx context.Context, agentName, cwd string, args []string) (agent.AgentSession, error)
}
```

**Production** (`registrySpawner`): wraps `agent.Registry.Get →
Detect → Start`. Returns the live bridge-level handle.

**Test** (`fakeSpawner`): returns a `fakeAgentSession` without
forking — used by `internal/chatsession/spawn_test.go` and the
flush-hook tests.

Dependency direction: `chatsession → agent.AgentSession` (via the
return type). `chatsession` does **not** import `agent.Registry`
directly; the runtime does and adapts.

See [`docs/feat/F-chat-session.md §AgentSession 池`](./F-chat-session.md)
§3.1 for the production wiring.

### 5.1.2 Default FlushHook (queued-message forwarder)

`ChatSession.ensureBuffer` installs a **default** FlushHook on the
InputBuffer at construction time:

```go
func (cs *ChatSession) defaultFlushHookLocked() FlushHook {
    return func(combined []agent.ContentBlock, userMsgIDs []string) error {
        as := cs.selectedAS
        if as == nil || as.Handle() == nil {
            return ErrNotRunning
        }
        return as.SendBlocks(context.Background(), combined)
    }
}
```

**Contract**: every queued user message reaches the currently
active AgentSession's `SendBlocks`. Without this hook (commit 9
left it nil), Idle-flushed messages were silently dropped — a
critical bug fixed in `4119e2c`.

The runtime can override via `SetFlushHook` (e.g., to add
receipt-card side effects before forwarding).

### 5.1.3 EventHandler (per-event translation seam)

`EventHandler` is invoked by the per-ChatSession readPump for
each event drained from the active AgentSession's `Events()`
channel:

```go
// internal/chatsession/readpump.go

type EventHandler func(chatID string, s *AgentSession, ev agent.AgentEvent)
```

**Install**: `cs.SetEventHandler(h)` once per ChatSession at
runtime startup. The handler **persists across `/use`** — only
the pump restarts, not the handler.

**Runtime typical implementation** (see
`cmd/nightme/run.go::newEventHandler`):

```go
return func(chatID string, s *AgentSession, ev agent.AgentEvent) {
    out, ok := gateway.Translate(chatID, ev)
    if !ok { return }
    out.ReplyTo = ""  // ReplyTo wired by channel-layer receipt FSM
    _ = ch.Send(context.Background(), out)
}
```

### 5.1.4 ReadPump lifecycle (commit 8c)

Each ChatSession has at most **one** active readPump goroutine
(`internal/chatsession/readpump.go`). Lifecycle:

- **`StartReadPump()`** — start pump for `cs.selectedAS`. Captures
  `cs.eventHandler` at start time. Stops any existing pump first.
  Returns `ErrNoSelectedAgentSession` if no active AS yet.
- **`StopReadPump()`** — signal `stop`, wait for `done`.
  Idempotent. Called by `KillAll` (commit 8c).
- **`HasPump()`** — atomic bool, true while the pump goroutine is
  alive. Reads `false` after natural exit (channel close from
  process death) as well as explicit stop.
- **`runReadPump`** (internal) — the goroutine body. Drains
  `as.Events()` with a `select` on `stop` + `evCh`. For each
  event: invoke handler, then drive FSM (non-terminal →
  `SetBusy`; `EventDone` / `EventError` → `SetIdle` +
  `OnTurnEnded`).

**Trigger points** (where the runtime calls `StartReadPump`):
- After `/use` resolves the new active AgentSession
  (`internal/command/use/cmd.go::Handle`)。

**Why not auto-start on spawn?** LookupSelectedAgentSession does
**not** auto-start the pump — keeps ChatSession unit-testable
without leaking goroutines (commit 8c).

### 5.1.5 Exit observer (process death notification)

`StartObserveClose` launches a goroutine that drains an
AgentSession's events channel to detect close. When the channel
closes (process died), the registered `AgentExitObserver`
fires. Currently the runtime does not wire an observer — the
readPump's natural exit is sufficient. The API is reserved
for future work (e.g., respawn on death, /close auto-reply).

---

## 5.2 Primary Agent snapshot (Q-A semantics)

`ChatSession.primaryAgent` is captured **once** at creation time
from `cfg.Primary` (via `chatsession.NewManager.GetOrCreate`).
Subsequent edits to `cfg.Primary` do **not** propagate to existing
ChatSessions — the snapshot is read-only.

For most use cases this is the right behaviour: a chat that
already started with `primaryAgent=claude` keeps using `claude`
even if the operator later sets `primary: codex` in
`config.yaml`. To force a new chat onto the new Primary, the
user simply opens a fresh chat.

---

## 6. Registry schema

```jsonc
// File: ~/.nightme/chat_sessions.json
{
  "version": 2,
  "chatSessions": {
    "<chatSessionId>": {
      "id": "cs_xxx",
      "chatId": "oc_xxx",
      "chatType": "p2p",
      "selectedCwd": "/code/bailing",
      "selectedAgent": "claude",
      "primaryAgent": "claude",
      "agentSessionIds": ["as_1", "as_2"],
      "selectedAgentSessionId": "as_1",
      "createdAt": "2026-08-02T...",
      "lastInteractionAt": "2026-08-02T..."
    }
  }
}

// File: ~/.nightme/agent_sessions.json
{
  "version": 2,
  "agentSessions": {
    "<agentSessionId>": {
      "id": "as_xxx",
      "chatSessionId": "cs_xxx",
      "agent": "claude",
      "cwd": "/code/bailing",
      "pid": 12345,
      "status": "running",
      "createdAt": "...",
      "lastRunAt": "..."
    }
  }
}
```

**Unique constraints** (registry enforces on write):
- `chatSessions[].chatId` UNIQUE
- `agentSessions[].(chatSessionId, agent, cwd)` UNIQUE

**Migration from registry**:
- had single `sessions.json` with `SessionEntry{ChatID, Workspace, Agent, Args, PID, Status}`
- migration:
  1. For each SessionEntry: create ChatSessionEntry(chatId, chatType="p2p") + AgentSessionEntry(agent, cwd=workspace, pid, status)
  2. Wire references: `chatSessionEntry.agentSessionIds = [newAS.id]`
  3. Write both files; archive file as `sessions.v1.json.bak`

---

## 7. Test strategy

### 7.1 Unit

- `SetSelectedCwd` / `SetSelectedAgent` — pure state mutation, no spawn/close
- `LookupSelectedAgentSession()` resolution (single path: hit → reuse, miss → spawn `(selectedAgent, selectedCwd)`; no runtime fallback to any "default" agent)
- `KillAll()` — all AgentSessions killed, selectedAS=nil, pool emptied
- `Restore()` from ChatSessionEntry + AgentSessionEntry — detached state, no process
- Concurrent `/use` + selectedAS read — race-free (uses sync.RWMutex)

### 7.2 Integration

- `nightme run` → /cwd → /use → message → /use (switch) → /use (switch back) → assert same PID
- /close → assert all PIDs gone → message → assert new PIDs spawned
- Daemon restart with active cwd/B → assert AgentSession for (A,A) detached but still in pool

### 7.3 Regression (E2E)

- `nightme run --channel=feishu`: slash commands work (`/cwd` new semantics, `/use`, `/close`, `/help`, `/agents`)
- `/use codex` after `/use claude` — rolling-log receipt cards remain coherent (Receipt FSM不变)
- Concurrent /use while AgentSession events flowing — no race; old events dropped

---

## 8. Out of scope

- **Multi-AgentSession parallelism** (同一 chat 多 agent 同时跑) — 
- **Cross-chat ChatSession sharing** (一个 chat 共享另一个 chat 的 AgentSession) — 明确不做
- **Hot-reload of ChatSessionEntry** while messages in flight — restart-based only
- **AgentSession migration across machines** — single-machine only
- **Auto-discovery of AgentSession when status=Exited** — explicit `/use` only triggers respawn

---

## 9. Open questions (draft)

- **Q-A**: Default Agent setting granularity — global config only? per ChatSession command? both? (Lean: both)
- **Q-B** (closed 2026-08-03): lookup only resolves `(selectedAgent, selectedCwd)`. No runtime fallback. selectedAgent is seeded from `cfg.Primary` at ChatSession creation and only mutated by `/use`.
- **Q-C**: Should `chatSession.SetSelectedCwd` log to user "selectedCwd changed, next message will spawn new AgentSession"? (Lean: yes, ephemeral info message)
- **Q-D**: When `/close` clears pool, should queued InputBuffer messages be persisted or dropped? (Lean: dropped; user explicitly killed)
- **Q-E**: ChatSession.ID is generated once or derived from chatId? (Lean: derived from chatId for 1:1 invariant enforcement)

---

## 10. Change log

---

## A2. F-28: `/use <agent>` Command

> **Source**: `F-chat-session.md`


> **Depends on**: F-27 (ChatSession), F-09 (Agent 抽象 — `AgentSpec` / `Starter` / `Agent`), F-29 (AgentSession pool)

> **Related docs**: [`SPEC.md`](../SPEC.md)[`PRD.md`](../PRD.md)---

## 1. Description

`/use <agent>` is the replacement for the `/run <agent>`. It switches the ChatSession's `selectedAgent` and routes future messages to the corresponding AgentSession.

**Critical difference from `/run`**:
- `/run` was an explicit spawn/reconnect command
- `/use` is a **lazy switch** — only spawns if `(selectedAgent, selectedCwd)` is not already in the pool
- `/use` **never restarts an existing process** — reuse is the default

---

## 2. Command syntax

```
/use <agent_name> [args...]
```

| Argument | Required | Description |
|----------|----------|-------------|
| `agent_name` | yes | One of: `claude`, `codex`, `opencode`, or any registered custom agent |
| `args...` | no | Forwarded to AgentSession on first spawn (e.g., `--model opus`) |

**Examples**:
```
/use claude                  # Switch to claude at selectedCwd (reuse or spawn)
/use codex --auto-approve    # Switch to codex with custom args (spawn only)
/use claude                  # Switch back; if (claude, /code/bailing) exists, reuse
```

---

## 3. Handler behavior

### 3.1 Pseudocode

```go
// internal/gateway/handlers/use.go

func (g *Gateway) handleUse(ctx context.Context, msg InboundMessage, args []string) error {
    if len(args) < 1 {
        return g.channel.Send(ctx, OutboundMessage{
            ChatID: msg.ChatID,
            Text:   "Usage: /use <agent> [args...]",
        })
    }

    cs, ok := g.bindings[msg.ChatID]
    if !ok {
        return g.channel.Send(ctx, OutboundMessage{
            ChatID: msg.ChatID,
            Text:   "No chat session yet. Send /cwd <path> first to bind this chat to a workspace.",
        })
    }

    agentName := args[0]
    extraArgs := args[1:]

    // Validate agent is registered
    if g.agents.Get(agentName) == nil {
        return g.channel.Send(ctx, OutboundMessage{
            ChatID: msg.ChatID,
            Text:   fmt.Sprintf("Unknown agent: %s. Run /agents to see available agents.", agentName),
        })
    }

    // Check ChatSession has selectedCwd (required for /use)
    if cs.SelectedCwd == "" {
        return g.channel.Send(ctx, OutboundMessage{
            ChatID: msg.ChatID,
            Text:   "No active workspace. Send /cwd <path> first.",
        })
    }

    // Pre-update selectedAgent (pure state mutation, no spawn)
    if err := cs.SetSelectedAgent(agentName); err != nil {
        return err
    }

    // Lazy lookup (may spawn or reuse)
    as, err := cs.LookupSelectedAgentSession()
    if err != nil {
        return g.channel.Send(ctx, OutboundMessage{
            ChatID: msg.ChatID,
            Text:   fmt.Sprintf("Failed to activate agent: %v", err),
        })
    }

    // Apply extraArgs on first spawn only (idempotent for existing sessions)
    if as.IsFirstSpawn() && len(extraArgs) > 0 {
        as.SetArgs(extraArgs)
    }

    // Persist
    g.registry.UpsertChatSession(cs.Entry())
    g.registry.UpsertAgentSessions(cs.PoolEntries()...)

    // Reply with state confirmation
    return g.channel.Send(ctx, OutboundMessage{
        ChatID: msg.ChatID,
        Text:   fmt.Sprintf("Now using %s, pid=%d, cwd=%s", as.Agent(), as.Pid(), as.Cwd()),
    })
}
```

### 3.2 LookupSelectedAgentSession (delegated to ChatSession)

See F-27 §3.3 for full lookup logic. The `/use` handler calls it after setting `selectedAgent`. The lookup returns:
- **Reused** AgentSession (no spawn) — fast path
- **Spawned** AgentSession (new process) — slow path (100ms ~ 2s depending on agent)

---

## 4. State machine impact

### 4.1 What `/use` does NOT do

- ❌ Restart existing AgentSession process (reuse only)
- ❌ Kill any AgentSession (preserves pool)
- ❌ Drop InputBuffer queue (messages remain)
- ❌ Drop receipts (Receipt FSM continues across /use)

### 4.2 What `/use` DOES do

- ✅ Update `ChatSession.selectedAgent`
- ✅ Trigger `LookupSelectedAgentSession()` (may spawn)
- ✅ Switch `eventCallback` registration to new active AgentSession
- ✅ Old active AgentSession remains in pool (its events become dropped)
- ✅ Persist `ChatSessionEntry` and `AgentSessionEntry` updates

### 4.3 What happens to in-flight events

```
Timeline:
T0: User sends "fix bug X" → routed to (claude, /code/A)
T1: claude AgentSession processing, 3 events already emitted
T2: User sends "/use codex"
T3: /use handler sets selectedAgent=codex, lookup (codex, /code/A) → spawn new
T4: New codex AgentSession starts; cs.selectedAS = codex_AS
T5: claude's remaining events arrive at claude_AS.Events() → readPump → callback
T6: Callback checks cs.selectedAS == claude_AS → NO → drop event, log "stale event"
T7: codex's events arrive at codex_AS.Events() → readPump → callback
T8: Callback checks cs.selectedAS == codex_AS → YES → Translate + ch.Send
```

**Result**: User sees codex responses. Claude's remaining output is silently dropped (with debug log).

---

## 5. Edge cases

### 5.1 Same agent, same cwd (no-op reuse)

```
/use claude  # selectedAgent already claude, pool has (claude, selectedCwd)
→ noop, just re-confirm state
→ reply: "Already using claude, pid=N, cwd=/path/A"
```

### 5.2 Same agent, different cwd (new AgentSession)

```
state: selectedAgent=claude, selectedCwd=/code/A, pool has (claude, /code/A)
/use claude  # selectedCwd already /code/A → same as 5.1
```

```
state: selectedAgent=claude, selectedCwd=/code/B (just /cwd'd), pool has (claude, /code/A) only
/cwd /code/A  # now selectedCwd=/code/A, pool still has (claude, /code/A)
/use claude  # reuse (claude, /code/A), no spawn
```

```
state: selectedAgent=claude, selectedCwd=/code/B
/cwd /code/A  # selectedCwd=/code/A
/use claude  # reuse (claude, /code/A), no spawn — but note: selectedAgent was already claude
```

```
state: selectedAgent=claude, selectedCwd=/code/B, pool has (claude, /code/B) only
/use codex  # spawn new (codex, /code/B); selectedAgent=codex
```

### 5.3 Agent exits unexpectedly

```
state: selectedAgent=claude, pool has (claude, /code/A) status=Exited (PID died)
/use claude  # LookupSelectedAgentSession detects Exited → respawn with same (agent, cwd)
            # → new PID, same ChatSessionEntry + AgentSessionEntry (same agentSessionId)
```

### 5.4 Concurrent /use / message

```
T0: User A sends "hello"
T1: User B sends "/use codex"
T2: handler.cwd processing
T3: messageDispatcher branch processing ("hello")
T4: Both reach ChatSession; serialized via poolMu
```

**Outcome**: Either "hello" goes to old claude or new codex, depending on lock acquisition order. No corruption; both flows complete correctly.

### 5.5 /use with invalid agent

```
/use invalid-agent
→ reply: "Unknown agent: invalid-agent. Run /agents to see available agents."
→ no state mutation
```

### 5.6 /use when selectedCwd is empty

```
/use claude  # no selectedCwd
→ reply: "No active workspace. Send /cwd <path> first."
→ no state mutation
```

---

## 6. Migration from `/run`

### 6.1 Behavior changes

| Aspect | `/run` | `/use` |
|--------|--------------|--------------|
| Spawn semantics | Always spawn (or reconnect if running) | Lazy spawn (reuse if pool has) |
| Restart on existing | Implicit reconnect | **Never restart**; reuse only |
| Multiple per chat | Not supported | **Multiple AgentSessions in pool** |
| /close interaction | /close kills the run session | /close kills entire pool |

### 6.2 Command mapping

| | equivalent |
|------|-----------------|
| `/cwd <path>` then `/run claude` | `/cwd <path>` then `/use claude` |
| `/run claude` (no /cwd yet) | `/cwd <path>` then `/use claude` (or `/use` after `/cwd`) |
| `/close` | `/close` (kills entire pool instead of single session) |
| `/cwd <new-path>` then `/run claude` | `/cwd <new-path>` then `/use claude` (spawns new AgentSession for new cwd) |

### 6.3 Backward compatibility

**No backward compatibility** — is a breaking change for `/run`. Migration path:
1. users upgrade to 2. Their persisted sessions.json auto-migrates to ChatSessionEntry + AgentSessionEntry (see F-27 §6)
3. Existing `/cwd` settings preserved; AgentSession = (agent=their-configured-agent, cwd=their-workspace)
4. Future behavior: `/use claude` will reuse that existing AgentSession (no re-spawn needed)

---

## 7. Test strategy

### 7.1 Unit

- `handleUse` with various (selectedAgent, pool) combinations
- LookupSelectedAgentSession() — exact match → reuse, miss → spawn `(selectedAgent, selectedCwd)`, respawn on exited; no runtime fallback to any "default" agent
- /use with no selectedCwd → error reply
- /use with unknown agent → error reply
- /use when already on that agent → noop

### 7.2 Integration

- /use claude → /use codex → /use claude: assert same PID for claude at each "use claude" call
- /close → /use claude: assert new PID (old AgentSession removed from pool)
- /cwd /A → /use claude → /cwd /B → /use claude: assert (claude, /A) and (claude, /B) both exist in pool

### 7.3 E2E

- Feishu DM round-trip: /cwd → /use claude → message → /use codex → message → /use claude → message → verify receipt FSM unaffected, AgentSessions reused

---

## 8. CLI subcommand equivalent

For consistency with `nightme list`, add `nightme use <chatId> <agent>` admin command:

```bash
# Force /use for a specific chat (admin/debug)
nightme use oc_xxx claude
nightme use oc_xxx codex --auto-approve
```

This is a thin wrapper around `Gateway.handleUse` with explicit `chatId` instead of inbound message routing.

---

## 9. Out of scope (F-28)

- **Auto-switch** (detect language of message → switch agent) — explicit only
- **Multi-agent per message** (route one message to multiple agents) — 
- **Agent capability negotiation** (only allow /use codex if codex supports cwd) — not needed, agents handle own validity
- **/use with prompt override** — extraArgs passed to spawn only, not applied to existing sessions

---

## 10. Open questions (draft)

- **Q-F**: Should `/use` without selectedCwd auto-default to a workspace (e.g., `~/.openclaw/workspace`)? (Lean: no, require explicit `/cwd`)
- **Q-G**: When extraArgs provided but AgentSession already exists, silently drop or warn? (Lean: warn in reply "args ignored, agent already running")
- **Q-H**: /use reply format — single line vs multi-line status (pid, cwd, agent, uptime)? (Lean: multi-line for diagnostic clarity)
- **Q-I**: Should `/use` support a keyword to reset selectedAgent to primaryAgent? (Lean: no — Q-A simplified to global Primary only; per-chat Primary not exposed via command)

---

## 11. Change log

---

## A3. F-29: AgentSession Pool

> **Source**: `F-chat-session.md`


> **Depends on**: F-27 (ChatSession), F-09 (Agent 抽象 — `AgentSpec` / `Starter` / `Agent`), F-19/F-21 (Bridge modes)

> **Related docs**: [`SPEC.md`](../SPEC.md)[`PRD.md`](../PRD.md)---

## 1. Description

`AgentSession` is the **per-CLI-process handle** Each `AgentSession` is uniquely identified by the immutable tuple `(chatSessionId, agent, cwd)`. A ChatSession owns a **pool** of AgentSessions — one per `(agent, cwd)` combination.

The pool enables:
- **Lazy reuse** — switching to a previously-used agent/cwd reuses the existing process
- **Preservation across switches** — `/cwd` and `/use` never kill AgentSessions; old entries stay in the pool
- **Independent state** — each AgentSession has its own PID, status, transport, and conversation context

---

## 2. Data structure

```go
// internal/agentsession/agentsession.go

type AgentSession struct {
    ID            string         // UUID v7, natural key
    ChatSessionID string         // FK → ChatSession.ID
    Agent         string         // IMMUTABLE — claude | codex | opencode | ...
    Cwd           string         // IMMUTABLE — absolute path

    // Runtime state (atomic)
    pid           atomic.Int32   // 0 when not running
    status        atomic.Int32   // StatusRunning | StatusDetached | StatusExited
    args          []string       // spawn args (set on first spawn, applied on respawn)

    // Transport (renamed from `bridge` field, see refactor-agentsession)
    transport     bridge.Transport  // PTY | ACP | SDK | JSON-IO
    events        chan agent.AgentEvent  // cap 64

    // Callbacks (set by ChatSession when active)
    eventCallback func(s *AgentSession, ev agent.AgentEvent)
    onFlushHook   func(blocks []agent.ContentBlock, userMsgIDs []string) error

    // Lifecycle
    cancel        context.CancelFunc
    wg            sync.WaitGroup
    startedAt     time.Time
    lastRunAt     time.Time
}

type Status int32
const (
    StatusRunning Status = iota  // process alive
    StatusDetached               // process was running, daemon restarted; not yet respawned
    StatusExited                 // process died (graceful or crash)
)
```

**Persistence entry**:

```go
// internal/registry/agent_session_entry.go

type AgentSessionEntry struct {
    ID            string    `json:"id"`
    ChatSessionID string    `json:"chatSessionId"`     // FK
    Agent         string    `json:"agent"`             // IMMUTABLE
    Cwd           string    `json:"cwd"`               // IMMUTABLE
    PID           int       `json:"pid"`               // 0 when not running
    Status        string    `json:"status"`            // running | detached | exited
    Args          []string  `json:"args"`              // spawn args
    CreatedAt     time.Time `json:"createdAt"`
    LastRunAt     time.Time `json:"lastRunAt"`
}
```

---

## 3. Pool semantics

### 3.1 Key invariant

`(chatSessionId, agent, cwd)` UNIQUE within the pool. Two AgentSessions with the same `(chatSessionId, agent, cwd)` cannot coexist.

**Implementation**:

```go
type ChatSession struct {
    poolMu sync.RWMutex
    pool   map[agentCwdKey]*AgentSession  // (agent, cwd) → AgentSession
}

type agentCwdKey struct {
    Agent string
    Cwd   string
}

func poolKey(chatSessionID, agent, cwd string) string {
    return chatSessionID + "␟" + agent + "␟" + cwd
}
```

Note: `chatSessionId` is implied by `cs.pool` already; only `(agent, cwd)` is the in-pool key.

### 3.2 Pool membership rules

| Event | Pool change |
|-------|-------------|
| First `/cwd` | pool empty; no AgentSession created |
| First `/use` | lookup `(selectedAgent, selectedCwd)` → spawn → add to pool |
| `/use` (reuse) | no change (entry already exists) |
| `/use` (new agent, same cwd) | spawn new AgentSession → add to pool |
| `/use` (same agent, new cwd) | spawn new AgentSession → add to pool |
| `/cwd` (any) | **no change** to pool; selectedCwd updated |
| `/close` | **all** entries killed and removed from pool |
| Agent process dies (natural exit) | status → Exited; **entry remains in pool** (pid=0) |
| Agent process detached (daemon restart) | status → Detached; entry remains; pid=0 |
| Daemon restart → restore | all entries restored with status=Detached; respawn on lookup |

### 3.3 Pool size limits

**No hard limit** Typical scenarios:
- 2 agents × 2 cwds = 4 AgentSessions max
- Power user with 5 agents × 3 projects = 15 AgentSessions
- Resource cost: each idle AgentSession ≈ 5-10MB RSS (PTY child)

Future  may add LRU eviction if pool grows unbounded.

---

## 4. Spawn / Reuse / Respawn lifecycle

### 4.1 First spawn

```
LookupSelectedAgentSession():
  1. pool[(selectedAgent, selectedCwd)] miss
  2. spawn (selectedAgent, selectedCwd) — no runtime fallback to any "default" agent
     a. Generate AgentSession.ID (UUID v7)
     b. Create Bridge (PTY/ACP/SDK/JSON-IO based on agent config)
     c. agentSession := &AgentSession{
            ID, ChatSessionID, Agent, Cwd, args,
            bridge, events (cap 64), status=Running, pid=<forked>
        }
     d. cs.pool[key] = agentSession
     e. Start readPump goroutine
     f. agentSession.RegisterEventCallback(cs.eventCallback)
     g. registry.Upsert(AgentSessionEntry)
     h. cs.selectedAS = agentSession
```

### 4.2 Reuse

```
LookupSelectedAgentSession():
  1. pool[(selectedAgent, selectedCwd)] hit
  2. status check:
     a. Running → cs.selectedAS = as; RegisterEventCallback; return as
     b. Detached/Exited → Respawn (see 4.3)
```

### 4.3 Respawn (process died or daemon restart)

```
Respawn():
  1. as.pid = 0 (clear old PID)
  2. as.status = Running (after fork)
  3. Re-create Bridge (or reuse if cached?)
  4. Fork new process with same (agent, cwd, args)
  5. New readPump goroutine
  6. Same AgentSession.ID (preserve pool identity)
  7. Registry update (new pid, same id)
```

**Critical**: Same `AgentSession.ID` is preserved across respawn. This maintains the pool invariant and lets external observers (logs, ChatSessionEntry) see continuity.

### 4.4 KillAll

```
KillAll():
  1. poolMu.Lock()
  2. for as := range cs.pool:
       as.Cancel() → SIGTERM to child → wait for exit (5s grace) → SIGKILL if needed
       delete(cs.pool, as.key)
  3. cs.selectedAS = nil
  4. cs.inputBuffer.Clear()  // queued messages lost
  5. poolMu.Unlock()
  6. registry.Upsert(ChatSessionEntry{...agentSessionIds=[], selectedAgentSessionId=null})
```

### 4.5 Death detection

```
AgentSession.readPump:
  for ev := range as.Events():
      if ev.Kind == EventDone || ev.Kind == EventError:
          as.status = StatusExited
          as.pid = 0
          registry.Upsert(AgentSessionEntry{pid=0, status=exited})
          // entry stays in pool
```

If process crashes (no EventDone/Error emitted):
```
bridgePTY:
  for ptmx.Read():
      ...
  // EOF / error → PTY died
  as.status = StatusExited
  as.pid = 0
```

---

## 5. Bridge integration

Each AgentSession owns one Bridge. Bridge choices (F-21):

| Agent | Default Bridge | Notes |
|-------|---------------|-------|
| `claude` | `JSON-IO` | Stream-JSON protocol; auto-accept permissions |
| `codex` | `ACP` | Agent Communication Protocol; structured events |
| `opencode` | `PTY` | Generic TTY passthrough |

**Bridge lifecycle** (per AgentSession):
1. Create Bridge on first spawn
2. Bridge fork-execs child process with `(cwd, args)`
3. Bridge pumps child stdout → `AgentSession.events` chan
4. Bridge consumes `AgentSession.SendText/SendBlocks/SendPermission` → child stdin

**Bridge uniqueness**: One Bridge per AgentSession. Switching agent (different `agent` field) creates new Bridge. Same agent + same cwd reuses existing AgentSession → existing Bridge.

### 5.1 Spawner wiring (production)

The runtime wires `chatsession.NewRegistrySpawner(reg)` into
`chatsession.Manager.WithSpawner(...)`. The Spawner is then
passed to every ChatSession created via `Manager.GetOrCreate`:

```go
// cmd/nightme/run.go (excerpt)

spawner := chatsession.NewRegistrySpawner(agents)
mgr := chatsession.NewManager().
    WithSpawner(spawner).
    WithPersistence(csFile, asFile)

cs := mgr.GetOrCreate(chatID, chatType, cfg.Primary)
// cs already has spawner inherited via Manager.GetOrCreate path
```

See [`F-27` §5.1.1](./F-chat-session.md) for the Spawner
contract and test substitution.

---

## 6. Concurrency model

### 6.1 Per-AgentSession goroutines

- **`readPump`** — single consumer of `as.Events()`
- **`bridge.stdinPump`** — if needed (PTY mode writes need backpressure handling)
- **`bridge.stdoutPump`** — translates child stdout → events chan

### 6.2 Per-pool goroutines (none directly)

Pool operations happen under `cs.poolMu` from the dispatchLoop or handler goroutines. No dedicated pool goroutine.

### 6.3 Race scenarios

**Race A: /use while old selectedAS is processing**

```
T0: claude_AS processing, 5 events queued in as.events
T1: User sends /use codex
T2: handler.use takes cs.poolMu.Lock
T3: SetSelectedAgent("codex")
T4: LookupSelectedAgentSession → spawn codex_AS, set cs.selectedAS = codex_AS
T5: cs.poolMu.Unlock
T6: claude_AS.readPump picks next event from claude.events
T7: callback(s=claude_AS, ev) → check cs.selectedAS == claude_AS (under poolMu.RLock) → NO → drop
T8: codex_AS.readPump picks events from codex.events → callback → cs.selectedAS == codex_AS → YES → Translate + Send
```

**Drop semantics**: Old in-flight events from claude_AS are dropped silently. User sees only codex output.

**Race B: Concurrent lookup from message and /use**

```
T0: User A sends "hi"
T1: User B sends /use codex
T2: messageDispatcher branch ("hi") acquires poolMu.Lock
T3: LookupSelectedAgentSession → resolves (claude, selectedCwd) → cs.selectedAS = claude_AS
T4: poolMu.Unlock
T5: "hi" dispatched to claude_AS
T6: handler.use acquires poolMu.Lock
T7: LookupSelectedAgentSession → spawns codex_AS → cs.selectedAS = codex_AS
T8: poolMu.Unlock
```

**Result**: "hi" goes to claude (correct). Future messages go to codex (correct).

### 6.4 Lock ordering

`cs.poolMu` is the single lock guarding pool state. No nested locks. Event callbacks take `poolMu.RLock` for `selectedAS` check — no deadlock risk.

---

## 7. Persistence schema

```jsonc
{
  "version": 2,
  "agentSessions": {
    "as_001": {
      "id": "as_001",
      "chatSessionId": "cs_xxx",
      "agent": "claude",
      "cwd": "/code/bailing",
      "pid": 12345,
      "status": "running",
      "args": [],
      "createdAt": "2026-08-02T10:00:00Z",
      "lastRunAt": "2026-08-02T10:30:00Z"
    },
    "as_002": {
      "id": "as_002",
      "chatSessionId": "cs_xxx",
      "agent": "codex",
      "cwd": "/code/bailing",
      "pid": 67890,
      "status": "running",
      "args": [],
      "createdAt": "2026-08-02T10:15:00Z",
      "lastRunAt": "2026-08-02T10:15:30Z"
    }
  }
}
```

**Migration ** (archive only — no transparent data lift):

```go
// internal/registry/migrate.go

func MigrateV1ToV2(v1RegistryPath string) (int, error)
``` did not persist `chat_id` on its session records (the binding
was in-memory only — see `internal/gateway/binding.go` v1.x), so
the chat → session mapping cannot be reconstructed from disk
alone. **The current dev does not transparently migrate v1.x data
to entries.** The startup flow (`cmd/nightme/run.go`)
calls `MigrateV1ToV2(v1RegistryPath)` which:

1. Reads `registry.json` if present.
2. Copies it to `registry.json.v1.bak` (idempotent; existing
   backup is preserved).
3. Does **not** write any entries — the v1.x data is
   archived only.
4. The runtime starts with an empty `chat_sessions.json` and
   `agent_sessions.json`.

**Action required after upgrade**: re-issue `/cwd` for each chat.
The `MigrateV1ToV2` backup file is kept on disk for forensic
recovery only — delete it once you're confident you don't need it.

See [`MIGRATION.md`](../../MIGRATION.md) for the full upgrade
guide.

---

## 8. Test strategy

### 8.1 Unit

- Pool membership rules (each event in §3.2)
- Spawn / Reuse / Respawn / KillAll transitions
- `(chatSessionId, agent, cwd)` uniqueness enforcement
- Race scenarios A and B from §6.3 with `go test -race`

### 8.2 Integration

- Pool grows on /use (new agent or new cwd)
- Pool stable on /cwd (no spawn, no kill)
- Pool cleared on /close
- Pool survives daemon restart (status=Detached, respawn on next message)

### 8.3 E2E

- 3-ways: claude@/A → codex@/A → claude@/A → assert same PID for first and third
- 4-ways: claude@/A → codex@/A → /cwd /B → claude@/B → assert (claude, /A) and (claude, /B) both in pool, different PIDs

---

## 9. CLI observability

```bash
$ nightme list

CHAT          CWD                  ACTIVE    AGENT        PID    STATUS
oc_aaa        /code/bailing        *         claude       12345  running
oc_aaa        /code/bailing                  codex        67890  running
oc_aaa        /code/nightme                  claude       11111  exited
oc_bbb        /code/nightme        *         claude       22222  running
```

`*` marks the active AgentSession for each chat. Multiple rows per chat show the pool.

---

## 10. Out of scope (F-29)

- **LRU eviction**
- **Cross-chat AgentSession sharing** (明确不做 — 每个 chat 独立进程池)
- **Pool rebalancing** (auto-promote another entry to active)
- **AgentSession snapshot/restore** (PTY process state cannot be snapshotted; only metadata persists)
- **Hot attach** to existing detached AgentSession's process (always respawn)

---

## 11. Open questions (draft)

- **Q-J**: When pool has (claude, /A) status=Exited, and user sends /use claude without /cwd — does lookup reuse the exited entry (respawn) or treat as new? (Lean: respawn; same identity)
- **Q-K**: When user sends /use claude with cwd-change-via-extraArgs (e.g., `--cd /other`), should that affect pool key? (Lean: no, extraArgs are spawn args only; cwd is ChatSession.SelectedCwd)
- **Q-L**: Should /list show pool or only active? (Lean: show pool, with `*` marker for active)
- **Q-M**: When two ChatSessions (different chats) both have (claude, /A), are PIDs guaranteed different? (Lean: yes, each ChatSession manages its own pool, spawn is per-pool)

---

## 12. Change log

---

## A4. F-34: `/new` Slash Command — Agent Conversation Reset

> **Source**: `F-chat-session.md`


> **Depends on**: F-09 (Agent abstraction), F-19 (CLI Bridge), F-21 (Agent Modes), F-24 (Claude Code Bridge), F-27 (ChatSession), F-28 (`/use`), F-29 (AgentSession pool), F-32 (Pi RPC Bridge)
> **Related**: [`SPEC.md`](../SPEC.md) §3.2 状态转换触发器, [`F-chat-session.md`](./F-chat-session.md), [`F-chat-session.md`](./F-chat-session.md), [`../bridge/pi.md`](./../bridge/pi.md), [`F-chat-session.md`](./F-chat-session.md)

---

## 1. Description

`/new` slash command 让用户在不退出 nightme daemon、不杀任何 CLI 进程的前提下，**丢弃 agent 的当前对话上下文**（history / 累积 usage / conversation state），从干净状态重新开始。语义对齐 claudecode 的内置 `/clear` 命令。

```
/new                    → 重置当前 selectedCwd 下 pool 里全部 AgentSession
/new <agent>            → 只重置当前 selectedCwd 下名为 <agent> 的那一条 AgentSession
```

为什么需要它：

- **Claude Code**：跑久了 context 满、对话偏离、需要清理；`/clear` 是 claude 自身的命令，但用户要在外层触发（IM 里打字），不是再开 TUI 输一次。
- **Pi**：交互模式有 `/new` 内置命令（用户验证过），等价于开启一个全新 session；nightme 把它暴露到 IM 入口。
- **ACP**：`session/new` JSON-RPC 原生支持，无需重启 transport。

不变式（受 SPEC §1.3 约束）：

- **不杀进程**：PTY 模式下 claude 子进程不退出（claude 自己处理 `/clear`）；long-lived 模式下 transport 保持（pi RPC / acp session-over-transport）。
- **不动 `AgentSession` 池身份**：ID / `(agent, cwd)` / args / CreatedAt 全部保留 —— `/use`、`/cwd` 切回老槽位时仍是同一个 AgentSession，只是底层对话状态被 reset。
- **不动 `Events()` chan**：readPump 不需要重启，event 流继续。

---

## 2. Motivation & Problem

### 2.1 现状

nightme 已有 `/close`（清空 pool + 杀全部进程）和 `/use`（切 active）。但都"过重"：用户只是想 reset 对话上下文、不想丢 pool 槽位或重启进程。

**场景**：

| 用户场景 | 当前可用命令 | 问题 |
|---|---|---|
| Context 满 / 想重新开始 | `/close` | 太重：杀掉所有进程 + 清空 pool；下次消息要重新 fork（~500ms-2s），所有挂起消息丢失 |
| 切到另一 agent | `/use` | 语义错：`/use` 是"换 agent"，不是"清上下文"。同 agent 没法 `/use cc`（noop）|
| 只想清 claudecode 的对话 | 无 | 必须 `/close` 或手动 TUI 输 `/clear` |

### 2.2 设计目标

1. **轻量级 reset**：与 `/close` 区分 —— 只丢对话，不丢进程 / pool 槽位 / args。
2. **跨 agent 协议统一**：每个 bridge 暴露 `AgentSession.New(ctx) error`，把"reset conversation"的语义收敛到一个方法。
3. **复用现有持久化链路**：bridge reset 后 emit `EventAgentConnected`（带新 SessionID）→ 现有 `cmd/nightme/run.go:467` 路径自动捕获 + 持久化。零新增 wiring。
4. **可选精修粒度**：`/new <agent>` 让用户只 reset 一个 agent 的对话，不动其他。

---

## 3. Concept

### 3.1 `AgentSession.New` 接口

新增方法到 `internal/agent/agent.go:494` 的 `AgentSession` 接口：

```go
type AgentSession interface {
    Events() <-chan AgentEvent
    PID() int
    SendText(text string) error
    SendBlocks(ctx context.Context, blocks []ContentBlock) error
    SendPermission(resp string) error
    Close() error

    // New resets the conversation context on the running session.
    // The underlying process (or transport, for long-lived bridges)
    // stays alive. Events() stays open. PID stays the same.
    // Subsequent SendText/SendBlocks operate on the fresh conversation.
    //
    // After New returns, the bridge MUST emit a new EventAgentConnected carrying
    // the new SessionID; the runtime's existing eventHandler captures
    // it via SetResumeID and persists. See cmd/nightme/run.go:467.
    //
    // Bridge-specific implementations:
    //   - claudecode: writeLine("/clear")       // stdin slash command
    //   - pi:         send {"type":"new_session"} RPC
    //   - acp:        send "session/new" JSON-RPC over the existing transport
    New(ctx context.Context) error
}
```

**错误契约**：bridge 实现保证 `New` 是 best-effort + 幂等。如果 reset 命令本身被 agent 拒绝（罕见；如 pi 还在处理上一 turn），`New` 返回非 nil error。调用方（`ChatSession.NewActiveAgentSessions`）继续清空其他 AS + InputBuffer，但 reply 里附上 error 信息。

### 3.2 三 bridge 实现

#### 3.2.1 claudecode — 发送 `{"type":"user",...,"content":"/clear"}` (in-process reset)

```go
// internal/bridge/claudecode/session.go
func (s *session) New(ctx context.Context) error {
    payload := []byte(`{"type":"user","message":{"role":"user","content":"/clear"}}`)
    return s.writeLine(payload)
}
```

**实测结论**（F-34 Phase 3 final，2026-08-04 实跑 `claude --print --input-format stream-json --output-format stream-json --verbose --dangerously-skip-permissions --model claude-haiku-4-5`）：

| 试探输入 | 结果 |
|---|---|
| `{"type":"user","message":{"role":"user","content":"Remember 77777"}}` | claude 答 "REMEMBERED" |
| recall | claude 答 "77777" |
| `{"type":"user","message":{"role":"user","content":"/clear"}}` | ✓ 触发 `SessionStart:clear` hook + **新 session_id** + 新 `system/init` |
| recall | claude 答 "NONE"（记忆被清空）|
| `{"type":"control","control":{"type":"clear"}}` | ✗ 静默忽略，session_id 不变 |
| `{"type":"control","control":{"type":"rewind"}}` | ✗ 静默忽略 |
| `{"type":"control","control":{"type":"interrupt"}}` | ✗ 静默忽略（capabilities 列表里有 `interrupt_receipt_v1` 但 stream-json stdin 路径没生效）|
| `{"type":"control","control":{"type":"compact"}}` | ✗ 静默忽略 |
| `{"type":"control","control":{"type":"reset"}}` | ✗ 静默忽略 |
| `{"type":"control","control":{"type":"new_session"}}` | ✗ 静默忽略 |
| `{"type":"control","control":{"type":"exit"}}` | ✗ 静默忽略 |

**关键发现**：
- claude-code 在 stream-json 模式下确实**接受 `/clear` 作为 user-typed slash command**（跟交互模式一样会触发内部 hook + 分配新 session）
- `{"type":"control","control":{"type":"..."}}` 各种 subtype 全部被静默丢弃 —— control 消息在 stdin 上**当前不工作**，尽管 `init.capabilities` 列了 `interrupt_receipt_v1`
- 因此 claudecode.New **不需要**杀进程走 fallback（用户初判是错的；实测证明 in-place reset 可用）
- bridge emit 新 `system/init{ session_id: <new> }` → runtime eventHandler 通过 `SetResumeID` + `PersistAgentSession` 持久化新 ResumeID

#### 3.2.2 pi — `{"type":"new_session"}` RPC

```go
// internal/bridge/pi/session.go
func (s *session) New(ctx context.Context) error {
    return s.rpc.requestAsync("new_session", nil)
}
```

**重要更正**（2026-08-04 经 pi-coding-agent 官方 `docs/rpc.md` 二次确认）：

所以 pi 的 reset 必须走 **RPC command `new_session`**（F-32 §1.2 中原本 deferred 的命令）。这是用户看到的 `/new` 在交互模式下的等价物 —— pi 把交互模式的 `/new` 内部映射到同一个 `new_session` RPC。

**协议补充**（F-32 §2.2 表格加一行）：

| command | 方向 | 字段 | 用途 |
|---|---|---|---|
| `new_session` | C→S | `type` | 丢弃当前 session 对话上下文，server 端分配新 sessionId 并 emit `state_update` 等 init 类事件。**不**杀进程。 |

**Translator 补丁（F-34 Phase 3 发现）**：原 F-32 translator 没有处理 `state_update` 事件（落入 default 分支被 log debug 丢弃），runtime 拿不到新 EventAgentConnected → ResumeID 不会更新到 `agent_sessions.json`。Phase 3 在 `internal/bridge/pi/translate.go` 加了 case `"state_update"`：解析 `sessionId`（+ 可选 `modelId`/`modelName`/`sessionFile`），emit `EventAgentConnected{SessionID: <new>}`，**绕开 `initSent` 守卫**（每次 new_session 都要让 runtime 重新捕获）。runtime eventHandler（`cmd/nightme/run.go newEventHandler`）自身有 `if ev.Connected.SessionID != ""` 守卫，重复 init 是幂等的。

**但**：实测 pi-coding-agent 官方 `docs/rpc.md` 二次确认 —— **`state_update` 不在官方事件列表**，`new_session` 响应也**不带新 sessionId**。唯一的获取方式是发完 `new_session` 后再调一次 `get_state`。

**修正实现**（F-34 Phase 3 final，`internal/bridge/pi/session.go::New`）：

```go
// 1. Send new_session, wait for response.
respEnv, err := s.rpc.request(ctx, "new_session", nil, "")
// 2. Inspect data.cancelled (extension may veto the reset).
// 3. Re-arm initSent under s.translatorMu + call get_state.
stateEnv, err := s.rpc.request(ctx, "get_state", map[string]any{}, "")
// 4. Decode get_state.data.sessionId → translator.emitInit → s.deliver
```

translator 里保留的 `case "state_update"` 是**防御性**的（若 pi 未来版本加了 state_update 事件，runtime 仍能拿到 sessionId），但**当前不依赖**。

#### 3.2.3 pty — `ErrRestartRequired` fallback (kill + respawn via Spawner)

PTY 是协议无关的字节管道，没有 "reset conversation context" 的概念（产品澄清 2026-08-04："pty 是删掉进程, 重启进程"）。`ptySession.New(ctx)` 返回 `agent.ErrRestartRequired`；wrapper 层（`agentsession.AgentSession.New`）捕获这个 sentinel 后走 fallback 路径：关掉 handle，调 `spawner.Spawn(ctx, agent, cwd, args, "")`（resumeID 为空），把新 handle 装回 `as.handle`，并 `SetResumeID("")` 清掉 id。下次 runtime 收到新 child 的 `EventAgentConnected` 时会重新捕获新 ResumeID。

```go
// internal/bridge/pty/session.go
func (s *ptySession) New(ctx context.Context) error {
    return agent.ErrRestartRequired
}

// internal/chatsession/agentsession.go (wrapper fallback)
if err := h.New(ctx); !errors.Is(err, agent.ErrRestartRequired) {
    return err  // nil or real error
}
if spawner == nil {
    return agent.ErrRestartRequired
}
_ = h.Close()
as.handle = nil
newHandle, err := spawner.Spawn(ctx, as.Agent, as.Cwd, as.args, "")
if err != nil { return err }
as.handle = newHandle
as.SetRunning(newHandle.PID())
as.SetResumeID("")
```

#### 3.2.4 acp — `session/new` JSON-RPC

```go
// internal/bridge/acp/session.go
func (s *acpSession) New(ctx context.Context) error {
    result, err := s.rpc.request(ctx, "session/new", newSessionParams{
        CWD:        s.workspace,
        MCPServers: []any{},
    })
    if err != nil {
        return err
    }
    // 用新 sessionID 替换；emit 新的 EventAgentConnected 让 runtime 持久化
    if err := s.setSessionID(result); err != nil {
        return err
    }
    return nil
}
```

ACP 的 `session/new` 本来就是创建新 session 的命令；当 transport 已存在时，它等价于"在现有 transport 上换 session"。复用现有 `setSessionID` + `emitInit` 路径，**无需拆 transport**（与 F-34 Phase 1 的设计一致；Phase 2 才考虑更激进的 transport 复用重构）。

### 3.3 `agentsession.AgentSession.New` wrapper + fallback

```go
// internal/chatsession/agentsession.go
//
// Signature is New(ctx, spawner Spawner). spawner is the chat's
// configured Spawner; nil-safe for bridges that handle reset in-place
// (pi / claudecode / acp); required for the pty fallback path.
func (as *AgentSession) New(ctx context.Context, spawner Spawner) error {
    as.handleMu.Lock()
    defer as.handleMu.Unlock()

    h := as.handle
    if h == nil {
        return ErrNotRunning   // Detached / Exited
    }
    if err := h.New(ctx); !errors.Is(err, agent.ErrRestartRequired) {
        return err  // nil (success) or real error (propagate)
    }
    if spawner == nil {
        return agent.ErrRestartRequired
    }
    _ = h.Close()
    as.handle = nil
    newHandle, err := spawner.Spawn(ctx, as.Agent, as.Cwd, as.args, "")
    if err != nil {
        return fmt.Errorf("chatsession: restart %s at %s: %w", as.Agent, as.Cwd, err)
    }
    as.handle = newHandle
    as.SetRunning(newHandle.PID())
    as.SetResumeID("")
    return nil
}
```

### 3.4 `ChatSession.NewActiveAgentSessions` 批量方法

```go
// internal/chatsession/chatsession.go
func (cs *ChatSession) NewActiveAgentSessions(ctx context.Context, agentName string) (matched, reset int, firstErr error) {
    cs.mu.RLock()
    cwd := cs.selectedCwd
    if cwd == "" {
        cs.mu.RUnlock()
        return 0, 0, nil   // caller replies "send /cwd first"
    }
    cs.mu.RUnlock()

    // 1. 收集 RUNNING targets（filter by cwd + optional agent + Status）
    cs.mu.RLock()
    targets := make([]*AgentSession, 0)
    for _, as := range cs.pool {
        if as.Cwd != cwd { continue }
        if agentName != "" && as.Agent != agentName { continue }
        if as.Status() != StatusRunning { continue }   // 只看 Running
        targets = append(targets, as)
    }
    cs.mu.RUnlock()

    if len(targets) == 0 {
        return 0, 0, nil
    }

    // 2. 串行 reset (避免 stdin / RPC 交错)
    for _, as := range targets {
        matched++
        if err := as.New(ctx); err != nil {
            if firstErr == nil { firstErr = err }
            continue
        }
        reset++
    }

    // 3. 清空 InputBuffer queued messages
    if cs.inputBuffer != nil {
        cs.inputBuffer.Clear()
    }
    return matched, reset, firstErr
}
```

**行为锁**：
- 串行 reset：3 个 AS 排队发，~3× 单次延迟；total 通常 < 50ms。
- **Status == StatusRunning 是必备过滤**（产品澄清 2026-08-04）：未启动的 AS 没有对话上下文，不应被启动后再 reset；直接跳过、静默不计 matched。
- InputBuffer 清空**总是**触发（在 reset 失败/部分失败时也清）—— 用户决策 #3。
- **不动** `currentTurnUserMsgID`：下一条 user msg 自然开新 turn + 新 anchor + Channel 冷开新 receipt。

---

## 4. `/new` Slash Command

`internal/command/newcmd/cmd.go`（对应 `internal/command/watch/cmd.go` 风格；F-102 之后 handlers_*.go 全部迁移到 `internal/command/<name>/`）：

```go
func handleNew(ctx context.Context, mgr *chatsession.Manager, channel Channel,
               msg *InboundMessage, args []string, globalPrimary string) (*CommandResult, error) {

    cs := mgr.GetOrCreate(msg.ChatID, globalPrimary)
    if cs.SelectedCwd() == "" {
        return reply(ctx, channel, msg.ChatID, "No active workspace. Send /cwd <path> first."), nil
    }

    agentName := ""
    if len(args) > 0 {
        agentName = strings.TrimSpace(args[0])
        if agentName == "" {
            return reply(ctx, channel, msg.ChatID, "Usage: /new [<agent>]"), nil
        }
    }

    matched, reset, err := cs.NewActiveAgentSessions(ctx, agentName)

    if matched == 0 {
        if agentName != "" {
            return reply(ctx, channel, msg.ChatID,
                fmt.Sprintf("No agent session for %q in current workspace. Try /agents.", agentName)), nil
        }
        return reply(ctx, channel, msg.ChatID,
            "No agent session in current workspace to reset. Send a message to start one."), nil
    }

    text := fmt.Sprintf("Reset %d/%d agent session(s).", reset, matched)
    if err != nil {
        text += fmt.Sprintf(" (errors: %v)", err)
    }
    return reply(ctx, channel, msg.ChatID, text), nil
}
```

注册到 `command.Commander`（[`internal/command/commander.go`](../../internal/command/commander.go)）—— runtime 在 `cmd/nightme/run.go` 把 `newcmd.Factory` 加进注册表：

```go
gw.Register(gateway.Command{
    Name:        "new",
    Description: "Reset conversation context. /new for all sessions in current workspace, /new <agent> for one.",
    Handler: func(ctx context.Context, msg *InboundMessage, args []string) (*CommandResult, error) {
        return handleNew(ctx, mgr, channel, msg, args, globalPrimary)
    },
})
```

### 4.1 语义对照表

| 命令 | 过滤 | 清空的 AS | 清空 InputBuffer | 杀掉进程 |
|---|---|---|---|---|
| `/new`（默认）| `Cwd == selectedCwd`（无 agent 过滤）| pool 中 selectedCwd 下全部 | ✓ | ✗ |
| `/new <agent>` | `Cwd == selectedCwd && Agent == <name>` | 至多 1 条 | ✓ | ✗ |
| `/close` | 整个 pool | 全部 | ✓ | ✓ |
| `/use <agent>` | 无 | 无（只切 selectedAS）| ✗ | ✗ |

### 4.2 `/new <agent>` 的 cwd 范围（决策锁）

**为什么限定 selectedCwd**：

- 与 `/new`（无参）保持对称 —— 两者都在当前 workspace 作用域内 reset。
- 避免"在 /A reset /B 的 session"的反直觉行为 —— 用户已经在 /A 工作，不应该莫名其妙影响 /B 的 agent 对话。
- 如果用户想 reset 另一 cwd 的 AS，先 `/cwd` 切换，再 `/new <agent>` —— 显式胜过隐式。

### 4.3 持久化副作用

`/new` 后 bridge emit `EventAgentConnected{ SessionID: newID }` → 现有 `newEventHandler`（`cmd/nightme/run.go:467`）走：

```go
if ev.Kind == agent.EventAgentConnected && ev.Connected != nil && ev.Connected.SessionID != "" {
    s.SetResumeID(ev.Connected.SessionID)
    if mgr != nil {
        if err := mgr.PersistAgentSession(s); err != nil && logger != nil {
            logger.Warn("persist agent session (init) failed", ...)
        }
    }
}
```

→ `s.SetResumeID(newID)` 覆盖旧的 `ResumeID` 字段  
→ `mgr.PersistAgentSession(s)` 把新 `ResumeID` 写盘到 `agent_sessions.json`

**零 schema 改动**。下次 daemon 重启时，spawn 携带新 `ResumeID`；各 bridge 各自翻译成自己的 CLI flag（chatsession 层只持有 opaque id，不感知 agent 差异）：

- `claudecode`：`--resume <newID>` —— `internal/bridge/claudecode/claudecode.go::buildArgs`
- `pi`：`--session-id <newID>` —— `internal/bridge/pi/agent.go::buildArgs`（2026-08-06 task `T-pi-bridge-align` P1 落地；之前 pi 忽略 `cfg.ResumeID` → 跨重启 pi 永远 fresh，现在 daemon 重启也能续接）
- `acp`：`session/load` JSON-RPC（OOB 走 transport-reuse 路径）

详见 [F-32 §12.2 实施记录](./../bridge/pi.md) 同款翻译契约说明。

---

## 5. 不变式 checklist

| 不变式（来自 SPEC §1.3）| 影响 |
|---|---|
| `Chat ↔ ChatSession` 1:1 | `/new` 不动 binding；✓ |
| `(agent, cwd)` per ChatSession 1:1（Q13）| AgentSession.ID/Cwd 不动；✓ |
| `agentSession.Events()` 单消费者是 readPump（Q14）| Events() 不关，readPump 不动；✓ |
| `ChatSession` 不 import channel/ | 仍满足；✓ |
| `currentTurnUserMsgID` 单数 | 不清；新 turn 自然覆盖；✓ |
| OutboundMessage.ReplyTo = currentTurnUserMsgID | Channel 冷开新 receipt；✓ |
| Message.HasMention + WatchMode（F-watch）| 与 `/new` 无关；✓ |

**新增不变式**：

---

## 6. 错误处理矩阵

| 场景 | 行为 |
|---|---|
| `selectedCwd == ""` | reply "No active workspace. Send /cwd <path> first." |
| `/new` 命中 0 条 AS | reply "No agent session in current workspace to reset." |
| `/new <agent>` 找不到 | reply "No agent session for <agent> in current workspace. Try /agents." |
| 单个 AS reset 失败（如 pi 还在处理 turn）| reset 计数 -1；InputBuffer **仍清空**；reply 附 "errors: <first err>" |
| 所有 AS reset 失败 | matched > 0, reset == 0；reply "Reset 0/N agent session(s). (errors: ...)" |
| pool 有 AS 但都未启动 (Status==Detached/Exited) | **F-43 ⚠ supersedes**: matched == 1, reset == 1, Action == `marked-fresh`; ResumeID cleared in-memory + persisted; reply 用 `FormatResetResults` per-entry list。原 F-34 "Q-N4 silently skip" 行为已被替换。 |
| pool 在 selectedCwd 下完全为空 | matched == 0；reply 同上 |

---

## 7. 测试计划

| 文件 | 测试 |
|---|---|
| `internal/bridge/claudecode/claudecode_test.go` | `New()` 写 `/clear\n` 到 mock stdin；writeLine 锁不与 SendBlocks 竞争 |
| `internal/bridge/pi/session_test.go` | `New()` 发 `{"type":"new_session"}`；mock RPC server 返回新 sessionId；验证后续 EventAgentConnected 带新 SessionID |
| `internal/bridge/acp/acp_test.go` | `New()` 发 `session/new`；验证 `s.sessionID` 被替换 + EventAgentConnected emit |
| `internal/chatsession/new_test.go` | `ChatSession.NewActiveAgentSessions`：filter / 计数 / InputBuffer.Clear / Status 跳过 / firstErr 聚合 |
| `internal/chatsession/agentsession_test.go` | `AgentSession.New` delegate：handle=nil 返回 ErrNotRunning |
| `internal/command/newcmd/commands_test.go` | `Handle` 命中 + 空 pool 报错 + `/new <agent>` 找不到 + 部分失败 reply |

---

## 8. 不在范围内（Out of Scope）

- **`/new` 跨 cwd reset**：`/new <agent>` 限定 selectedCwd；reset 其他 cwd 的 AS 由用户 `/cwd` 切换触发（决策 §4.2）。
- **bridge 层的并发 reset 优化**：当前串行；如有性能诉求可在 Phase 3 加 per-bridge 并发（注意 stdin / RPC 各自串行约束）。
- **ACP transport 复用重构**：当前每次 `New` 都复用 transport 已有 session/new；不抽公共 transport（与 F-32 / F-21 Phase 2 一致）。
- **`/new` 清空用户消息历史**：仅清 agent 对话上下文 + InputBuffer queued；不删 Channel 端已发的 user message（Channel 自管 receipt，与 `/new` 正交）。
- **UI 反馈**：`/new` 只 reply 一行文本；不触发额外 OutboundMessage / reaction（与 `/watch`、`/close` 一致）。
- **`/reset` 别名**：暂不提供；如用户想要，`/new` 已被锁定。

---

## 9. 决策记录

| # | 决策 | 结论 | 日期 |
|---|---|---|---|
| Q-N1 | `New` 放哪个接口 | `agent.AgentSession` 接口（不是 `agent.Agent`）| 2026-08-04 |
| Q-N2 | `/new` 无参的清空范围 | pool 中 `Cwd == selectedCwd` 的全部 | 2026-08-04 |
| Q-N3 | `/new <agent>` 的清空范围 | pool 中 `Cwd == selectedCwd && Agent == <name>`（限定 cwd，对称）| 2026-08-04 |
| Q-N4 | InputBuffer 处理 | **清空** queued（与 `/close` 行为对齐）| 2026-08-04 |
| Q-N5 | claudecode reset 命令 | `writeLine({"type":"user","message":{...,"content":"/clear"}})` —— claude-code 在 stream-json 模式下接受 `/clear` 作为 user-typed slash command（实测 2026-08-04）；控制消息 `{"type":"control",...}` 各种 subtype 全部无效 | 2026-08-04 |
| Q-N6 | pi reset 协议 | **RPC command** `{"type":"new_session"}`（不是 prompt 文本 `/new`）| 2026-08-04 |
| Q-N7 | acp reset 协议 | JSON-RPC `session/new` over existing transport | 2026-08-04 |
| Q-N8 | `AgentSession` ID 是否换 | **不换** —— pool 槽位稳定 | 2026-08-04 |
| Q-N9 | ResumeID 更新时机 | bridge emit EventAgentConnected 后，runtime 走现有路径自动持久化 | 2026-08-04 |
| Q-N10 | handle / readPump 处理 | 不动；bridge 在原 transport 上发 reset，Events() 不关 | 2026-08-04 |

---

## 10. 实施清单（Phase 2）

| 文件 | 改动 | 行数估算 |
|---|---|---|
| `internal/agent/agent.go` | `AgentSession` 接口加 `New(ctx) error` | +10 |
| `internal/bridge/claudecode/session.go` | 实现 `New` | +6 |
| `internal/bridge/pi/session.go` | 实现 `New`（RPC requestAsync）| +12 |
| `internal/bridge/acp/session.go` | 实现 `New`（session/new + setSessionID）| +20 |
| `internal/agentsession/session.go` (F-102 重构后） | `New` delegate | +10 |
| `internal/chatsession/chatsession.go` | `NewActiveAgentSessions` | +30 |
| `internal/command/newcmd/cmd.go` | `Handle` 实现 + `/new` 路径 | +60 |
| `internal/command/commander.go` + `cmd/nightme/run.go` | 注册 `/new` 命令（`newcmd.Factory` 加进 `command.Commander`） | +8 |
| 测试 6 文件 | bridge + chatsession + gateway | +200 |
| **合计** | | **~360** |

实施顺序：

1. 改 `agent.AgentSession` 接口（编译会断 3 个 bridge；先 stub）
2. 三 bridge 各自实现 `New`（claudecode 最简；acp 次之；pi 需要 RPC schema 验证）
3. chatsession 包装 + `NewActiveAgentSessions`
4. `internal/command/newcmd/cmd.go` + 注册到 `command.Commander`
5. 测试

---

## A5. F-43: `/close` Graceful Shutdown + `/new` ResumeID Clear + 列表式回复

> **Source**: `F-chat-session.md`


> **Depends on**: F-34 (`/new` slash command), F-27 (ChatSession), F-29 (AgentSession pool), F-19 (CLI Bridge), F-24 (Claude Code Bridge), F-32 (Pi RPC Bridge), F-33 (ACP Bridge)
> **Related**: [`SPEC.md`](../SPEC.md) §3.2 状态转换触发器, [`F-runtime.md`](./F-runtime.md), [`F-chat-session.md`](./F-chat-session.md), [`F-chat-session.md`](./F-chat-session.md), [`F-chat-session.md`](./F-chat-session.md), [`../channel/feishu-rendering.md`](./../channel/feishu-rendering.md)

---

## 1. Description

三个独立但相互关联的修复,统一打包:

1. **`/close` 改成 bridge graceful shutdown** —— 当前 `ChatSession.KillAll` 只清理 nightme 侧的内存和 disk,**不向 child CLI 发任何信号**,导致进程孤儿化继续运行;同时清掉 InputBuffer 等不属于该命令管理的 state。
2. **`/new` 对 dead/detached AgentSession 清 ResumeID** —— 当前 `NewActiveAgentSessions` 对 `StatusRunning` 之外的 entry **silently skip**,但 pool entry 里残留的 ResumeID 会在下次 spawn 时被透传,导致 `--resume <死 id>` 试图续接一个停止的 session(违背 `/new` 的语义)。
3. **两个命令的回复文案升级为 per-entry 列表** —— 用户从 "Killed 2 agents" 升级到 "✓ claude @ /A\n✓ codex @ /B",每条 entry 都有明确归属。

不变式（受 SPEC §1.3 约束）：

- **slash command 边界**:`/close` 和 `/new` **只与 agent 进程交互**。`selectedCwd` / `selectedAgent` / `currentTurnUserMsgID` / `InputBuffer` 都不属于它们。
- **graceful 优先**:每个 bridge 的 `Close()` 已有 stdin EOF + SIGINT + 2s grace + SIGKILL 兜底的本地 watchdog（见 `claudecode` `session.go:466-498`）。nightme 端**不二次升级**信号。
- **不主动浪费资源**:`/new` **永不**为触发"reset"而 spawn 一个 dead agent。dead 状态 = 不动进程,只清 ResumeID。
- **state 同步**:disk 删除必须在进程死 *之后*,不能根据"还没发生的事"提前删。

---

## 2. Motivation & Problem

### 2.1 当前 `/close` 的三个缺陷

`internal/chatsession/chatsession.go:769-796` 的 `KillAll` 注释自陈 "commit 6: this is a data-only operation — no actual signal is sent (commit 7 will wire SIGTERM)",代码也确实如此:

```go
func (cs *ChatSession) KillAll() error {
    cs.StopReadPump()                  // ← 停 nightme 侧 read goroutine
    cs.mu.Lock()
    cs.pool = make(map[agentCwdKey]*AgentSession)  // ← 整张 map 重新分配
    cs.selectedAS = nil
    cs.currentTurnUserMsgID = ""
    cs.mu.Unlock()
    if cs.inputBuffer != nil {
        cs.inputBuffer.Clear()         // ← 副作用:清掉用户排队的消息
    }
    if cs.asFile != nil {
        for _, e := range cs.asFile.GetByChatPool(cs.ID) {
            _ = cs.asFile.Delete(e.ID)   // ← 副作用:agent_sessions.json 删 entry
        }
    }
    cs.persistChatEntry()
    return nil
}
```

具体问题:

| 问题 | 后果 |
|------|------|
| **没真杀** | `cs.pool = make(...)` 抛弃 Go 指针,child CLI 进程孤儿化继续跑,占用 CPU/memory/PTY |
| **没走 bridge graceful** | `AgentSession.Close()`（`agentsession.go:449`）完全没被调;每个 bridge 自带的 graceful shutdown + SIGKILL 兜底路径浪费 |
| **副作用太多** | `InputBuffer.Clear()` 丢用户消息;`agent_sessions.json` entry 在进程 *未死* 时被删;`currentTurnUserMsgID` 清零 |
| **回复撒谎** | handler 报 "Killed 2 agent session(s)",但实际 child 没杀 |

### 2.2 当前 `/new` 的一个隐藏 bug

`internal/chatsession/chatsession.go:832-936` 的 `NewActiveAgentSessions` 对 dead/detached entry **silently skip**:

```go
if as.Status() != StatusRunning {
    // Not started → no conversation → skip silently.
    // Do NOT trigger a lazy spawn here (F-34 §6 Q-N4 / product
    // clarification 2026-08-04).
    continue
}
```

但是这个 entry 还在 pool 里,**它的 `ResumeID` 也没清**。下次消息触发 `LookupSelectedAgentSession` → pool hit but Status=Exited → spawn with `as.ResumeID()` 的旧值 → Claude Code 桥拼 `--resume <旧id>` → **试图续接一个停止的 session**。这跟 `/new` 的"下一次 fresh"语义完全相反。

### 2.3 UX 不一致

handler 只报 "Killed/Reset N":

```
Killed 2 agent session(s). Send a message to start fresh.
Reset 3 session(s). Send a message to start fresh.
```

用户无法知道:
- 哪些 agent 被处理了
- 哪些成功、哪些失败
- 死状态的 entry 是否清干净

### 2.4 设计目标

1. **`/close` 真正 graceful kill**:每个 bridge 走自己的 Close() 路径,等进程真死,再清 disk。
2. **`/new` 对 dead state 也要有副作用**:清 ResumeID,让下次 spawn 必然 fresh。
3. **不动 slash command 边界外的 state**:InputBuffer、selectedCwd、用户当前消息都不归 `/close` 管。
4. **per-entry 列表回复**:每个 agent 一行,带 ✓ / ✗ / • 状态标记,失败带 error msg。

---

## 3. Concept

### 3.1 graceful shutdown 全貌

```
user /close
  ↓
kill.Factory.Handle (internal/command/close/close.go)
  ↓
cs.KillAll() —— 改造后
  ├ snapshot pool(拷贝 Go 指针,不在原 map 上原地删)
  ├ 对每个 Running entry:
  │   as.Close()  ← 触发 bridge.Close()
  │     ├ claudecode: stdin EOF + SIGINT + 等 2s + SIGKILL 兜底
  │     ├ pi:         RPC shutdown + 等 2s + SIGKILL 兜底
  │     ├ acp:        session/close RPC(transport 不关)
  │     └ pty:        stdin EOF
  │   ObserveClose goroutine:events chan 关闭 → SetExited(0)
  ├ wg.Wait() ← 等所有 bridge 走完 graceful
  ├ 设 5s 整体 timeout —— bridge 内部 SIGKILL 兜底后这个 timeout 几乎不触发
  ├ selectedAS = nil; currentTurnUserMsgID = ""
  ├ asFile.Delete(each entry) ← 进程死 *之后* 删 disk
  └ persistChatEntry()
```

### 3.2 `/new` 对 dead state 的"轻量 reset"

```
user /new
  ↓
newcmd.Factory.Handle (internal/command/newcmd/cmd.go)
  ↓
cs.NewActiveAgentSessions(ctx, agentName)
  ├ for each (cwd, [agent]) 匹配 entry:
  │   switch as.Status():
  │     case StatusRunning:
  │       pump 协调:StopReadPump → as.New(ctx, spawner) → StartReadPump
  │       matched++, reset++
  │
  │     case StatusDetached:
  │     case StatusExited:
  │       as.SetResumeID("")              ← 不 spawn!只清 ResumeID
  │       cs.asFile.Upsert(as.Entry())    ← 持久化清空的 ResumeID
  │       matched++, reset++
  │
  ├ InputBuffer.Clear()                  ← 不变:F-34 review #1
  └ return (matched, reset, results, firstErr)
```

### 3.3 列表式回复文案

详见 §5。

---

## 4. `/close` Slash Command

### 4.1 不变量

| 触碰 | 不触碰 |
|------|--------|
| agent 进程(graceful shutdown) | `selectedCwd` / `selectedAgent` / `currentTurnUserMsgID` |
| pool entry 状态(Running → Exited) | InputBuffer 排队消息 |
| `agent_sessions.json` entry(**进程死 *之后***删除) | ChatSession 本身的 binding |
| `selectedAS` 在 ChatSession 内的引用 | 其他 chat 状态 |

### 4.2 实现位置

`internal/chatsession/chatsession.go` —— `KillAll` 整体重写,新增 `KillResult` 类型 + `FormatKillResults` helper。

### 4.3 `KillResult` 类型

```go
// KillResult is one row of the /close reply. It captures what
// happened to a single pool entry during KillAll so the handler
// can render a per-agent status instead of a bare count.
type KillResult struct {
    Agent       string  // "claude", "codex", ...
    Cwd         string  // "/code/A"
    BeforeState Status  // StatusRunning / Detached / Exited
    Action      string  // "killed" / "stale-cleared"
    Error       error   // nil for success
}
```

### 4.4 `KillAll` 新签名

```go
// Old: func (cs *ChatSession) KillAll() error
// New:
func (cs *ChatSession) KillAll() ([]KillResult, error)
```

返回 `[]KillResult` 让 handler 知道每条 entry 的命运。`error` 仍保留(整体性错误,比如 registry 损坏)。

### 4.5 实现

```go
// KillAll kicks every AgentSession in the pool out of the running
// state via each bridge's graceful Close() path. After all child
// processes have exited, their persistent entries are deleted from
// agent_sessions.json so the next spawn won't resume the dead
// sessions. The InputBuffer is left alone — the user's queued
// messages are not /close's concern.
func (cs *ChatSession) KillAll() ([]KillResult, error) {
    // 1. snapshot pool under read lock; don't mutate pool until
    //    every bridge has confirmed shutdown.
    cs.mu.RLock()
    snapshot := make([]*AgentSession, 0, len(cs.pool))
    for _, as := range cs.pool {
        snapshot = append(snapshot, as)
    }
    cs.mu.RUnlock()

    results := make([]KillResult, 0, len(snapshot))

    // 2. fan out graceful shutdown. Each bridge drives its own
    //    shutdown sequence (stdin EOF + SIGINT, RPC close, etc.)
    //    with a SIGKILL fallback if the agent doesn't honor the
    //    graceful path within ~2s. We don't add a second
    //    escalation here — bridging the dial-in/wait dance would
    //    race with the bridge's local watchdog.
    var wg sync.WaitGroup
    for _, as := range snapshot {
        result := KillResult{
            Agent:       as.Agent,
            Cwd:         as.Cwd,
            BeforeState: as.Status(),
        }
        if as.Status() != StatusRunning {
            // Already dead or detached; no bridge to signal.
            result.Action = "stale-cleared"
        } else {
            result.Action = "killed"
            wg.Add(1)
            go func(as *AgentSession) {
                defer wg.Done()
                _ = as.Close()
            }(as)
        }
        results = append(results, result)
    }

    // 3. wait for all bridges to confirm exit. Bridge Close sets
    //    events chan closed; ObserveClose goroutine then flips
    //    as.Status to Exited (which wg.Wait correlates with
    //    by the bridge's own Wait).
    done := make(chan struct{})
    go func() { wg.Wait(); close(done) }()
    select {
    case <-done:
    case <-time.After(killGraceTotal):
        // Bridge's own watchdog should have SIGKILL'd by now.
        // If we still hit this, the child is wedged in a way
        // even SIGKILL can't fix (zombie / uninterruptible io).
        // Log and proceed — we still want to clean our state.
        log.Warn("killAll: graceful shutdown timeout", "limit", killGraceTotal)
    }

    // 4. wipe selectedAS pointer BEFORE removing from disk so a
    //    follow-up message sees "no active" and goes through
    //    LookupSelectedAgentSession -> spawn fresh.
    cs.mu.Lock()
    cs.selectedAS = nil
    cs.currentTurnUserMsgID = ""
    cs.mu.Unlock()

    // 5. delete persistent entries. Now safe: child is dead (or
    //    was already dead), so any stale ResumeID would point to
    //    a corpse.
    if cs.asFile != nil {
        for _, as := range snapshot {
            _ = cs.asFile.Delete(as.ID)
        }
    }
    cs.persistChatEntry()
    return results, nil
}
```

`killGraceTotal` 建议 5s(比 bridge 内部 2s 多一倍,留 2 次 grace 重试 + SIGKILL 余量)。

### 4.6 `/close` 行为总结

| 行为 | 旧 | 新 |
|------|----|----|
| 杀进程 | ❌ `cs.pool = make(...)` | ✅ `as.Close()` 走 bridge graceful |
| InputBuffer | ❌ `Clear()` | ✅ **保留** |
| `agent_sessions.json` | ❌ 进程死 *前* 删 | ✅ 进程死 *后* 删 |
| `currentTurnUserMsgID` | ❌ `= ""` | ✅ `= ""`(同) |
| `selectedAS` | ❌ `nil` | ✅ `nil`(同) |
| 返回值 | `error` | `([]KillResult, error)` |
| 5s 整体 timeout | ❌ | ✅ 兜底 |

---

## 5. `/new` Slash Command

### 5.1 不变量

| 触碰 | 不触碰 |
|------|--------|
| pool entry.ResumeID(Running / Detached / Exited 都支持) | 进程本身 |
| `agent_sessions.json` entry.ResumeID 字段 | InputBuffer 排队消息 |
| matched / reset 计数 | 进程启动 |

### 5.2 `ResetResult` 类型

```go
type ResetResult struct {
    Agent       string
    Cwd         string
    BeforeState Status
    Action      string  // "in-place-reset" / "marked-fresh"
    Error       error
}
```

### 5.3 `NewActiveAgentSessions` 新签名

```go
// Old: func (cs *ChatSession) NewActiveAgentSessions(ctx context.Context, agentName string) (matched, reset int, firstErr error)
// New:
func (cs *ChatSession) NewActiveAgentSessions(ctx context.Context, agentName string) (matched, reset int, results []ResetResult, firstErr error)
```

`results` 包含所有 entry 的完整轨迹(`len(results) == matched`)。`matched` / `reset` 保留为简单 int,老调用点不依赖 `results` 也能用。

### 5.4 dead/detached 分支改造

`internal/chatsession/chatsession.go:850-855` 替换:

```go
// 改动前
if as.Status() != StatusRunning {
    // Not started → no conversation → skip silently.
    // Do NOT trigger a lazy spawn here (F-34 §6 Q-N4 / product
    // clarification 2026-08-04).
    continue
}
```

```go
// 改动后
if as.Status() != StatusRunning {
    // F-34 §6 Q-N4: do NOT trigger a lazy spawn for /new.
    // But the entry's stale ResumeID must not be replayed on the
    // next spawn — that would resurrect the dead session, defeating
    // the user's /new intent. Clear ResumeID in-memory + persist
    // so the next LookupSelectedAgentSession spawns fresh.
    as.SetResumeID("")
    if cs.asFile != nil {
        _ = cs.asFile.Upsert(as.Entry())
    }
    matched++
    reset++
    continue
}
```

### 5.5 `/new` vs `/close` vs `/stop` 对比

| 维度 | `/stop` | `/close` | `/new` |
|------|---------|---------|--------|
| **目的** | 中断当前 turn,bridge 继续 | 终止 bridge 进程,会话保留 | 重置对话上下文,下次 fresh |
| **进程状态** | `Running → Running`(可能,结构化 abort) / `Running → Exited`(可能,SIGINT) | `Running → Exited`(graceful) | `Running → Running`(in-place reset) / `Exited/Detached → Exited/Detached`(只清 ResumeID) |
| **是否 spawn** | ❌ 永不 spawn | ❌ 永不 spawn | ❌ 永不 spawn |
| **InputBuffer** | ✅ 保留(用户消息) | ✅ 保留(用户消息) | ❌ 清掉(旧对话的一部分) |
| **AgentSession pool entry** | 保留(`Handle` 还在) | 保留(`Handle` 失效,sessionID 还在) | 保留 |
| `agent_sessions.json` | 不动 | entry 保留(下次 spawn 用 `--resume <id>` 续上) | entry.ResumeID 清空(entry 保留) |
| `currentTurnUserMsgID` | 不动 | 不动 | 不动 |
| **下次 spawn** | 同 AS,继续 | 同 AS ID,`--resume <id>` 续上 | fresh(无 ResumeID) |
| **bridge 调用** | `as.Stop(ctx)` | `as.Close()`(graceful) | `as.New(ctx, spawner)`(in-place);dead 分支 0 bridge 调用 |
| **强杀兜底** | N/A | bridge 内部 2s 后 SIGKILL | N/A |

**关键区别**:

- `/stop` = signal only;`/close` = signal + kill process;`/new` = in-place context reset.
- `/close` **保留** AgentSession 的 identity(pool entry + sessionID + agent_sessions.json row)以便下次 spawn 续上对话;真正"丢弃 session"需要 daemon shutdown 或手动清理 `agent_sessions.json`。
- 三者都不 spawn 新进程 —— 用户下次发消息时才走 spawn 路径。

---

## 6. 列表式回复文案

### 6.1 `/close` 模板

**空 pool**:
```
No active agents to kill.
```

**全部成功**:
```
Stopped 2 agent session(s):
  ✓ claude @ /code/A
  ✓ codex @ /code/B
```

**部分失败**:
```
Stopped 1 agent session(s), 1 failed:
  ✓ claude @ /code/A
  ✗ pi @ /code/B — kill timeout after 5s
```

**所有 entry 已经死(state == Exited/Detached)**:
```
Cleared 2 stale agent session(s) (no live processes):
  • claude @ /code/A — already exited, entry cleaned
  • codex @ /code/B — already exited, entry cleaned
```

**混合 alive + 死**:
```
Stopped 1 agent session(s), 1 stale entry cleared:
  ✓ claude @ /code/A — killed
  • codex @ /code/B — already exited, entry cleaned
```

### 6.2 `/new` 模板

**全部 running + in-place reset OK**:
```
Reset 2 session(s):
  ✓ claude @ /code/A
  ✓ codex @ /code/A
```

**混合 running + dead**:
```
Reset 3 session(s):
  ✓ claude @ /code/A — reset in-place
  ✓ codex @ /code/B — already exited, marked fresh for next spawn
  ✓ pi @ /code/C — already exited, marked fresh for next spawn
```

**全部死**(纯标记 fresh):
```
Marked 2 session(s) fresh for next spawn:
  ✓ claude @ /code/A — already exited, ResumeID cleared
  ✓ codex @ /code/B — already exited, ResumeID cleared
```

**部分失败**(bridge.New 出错):
```
Reset 1 session(s), 1 failed:
  ✓ claude @ /code/A — reset in-place
  ✗ pi @ /code/B — bridge reset: <error>
```

### 6.3 图标选型

| 状态 | 图标 | 含义 |
|------|------|------|
| 成功 | `✓` | 真正发生了动作(killed / reset / cleared) |
| 失败 | `✗` | 出错,需要用户感知 |
| 跳过(已经是死状态) | `•` | 没有失败,但也没有"kill"这件事发生 —— 只是清理了 disk |

`✓` / `✗` 在 Feishu 普遍支持,`•` 是普通 bullet 不渲染问题。

### 6.4 排序

按 **"(成功 → 失败) → agent 名字 → cwd"** 排序:

```go
sort.SliceStable(results, func(i, j int) bool {
    // 失败组排后面
    if (results[i].Error != nil) != (results[j].Error != nil) {
        return results[j].Error != nil  // 无 err 的在前
    }
    if results[i].Agent != results[j].Agent {
        return results[i].Agent < results[j].Agent
    }
    return results[i].Cwd < results[j].Cwd
})
```

### 6.5 长度限制

Feishu 单条 4KB 限制。典型 pool 量(< 10)远远低于这个限制。**防御性截断**:

```go
const maxResultLines = 20
if len(lines) > maxResultLines {
    lines = lines[:maxResultLines]
    lines = append(lines, fmt.Sprintf("  ... and %d more", len(results)-maxResultLines))
}
```

### 6.6 `FormatKillResults` / `FormatResetResults` helper

放在 `internal/chatsession/chatsession.go` 末尾,handler 调用即可:

```go
// FormatKillResults produces a human-readable summary suitable for
// channel.Send. Caller passes the results slice from KillAll;
// FormatKillResults handles the per-state branching.
func FormatKillResults(results []KillResult) string {
    if len(results) == 0 {
        return "No active agents to kill."
    }

    var killed, stale, failed int
    lines := make([]string, 0, len(results))

    for _, r := range results {
        if r.Error != nil {
            failed++
            lines = append(lines, fmt.Sprintf("  ✗ %s @ %s — %s: %v",
                r.Agent, r.Cwd, humanAction(r.Action), r.Error))
            continue
        }
        switch r.Action {
        case "killed":
            killed++
            lines = append(lines, fmt.Sprintf("  ✓ %s @ %s", r.Agent, r.Cwd))
        case "stale-cleared":
            stale++
            lines = append(lines, fmt.Sprintf("  • %s @ %s — already exited, entry cleaned",
                r.Agent, r.Cwd))
        }
    }

    sort.Strings(lines)  // 简单按字符串 sort,保持稳定

    header := buildKillHeader(killed, stale, failed)
    return header + "\n" + strings.Join(lines, "\n")
}

func buildKillHeader(killed, stale, failed int) string {
    if failed == 0 && stale == 0 {
        return fmt.Sprintf("Stopped %d agent session(s):", killed)
    }
    if killed == 0 && stale > 0 && failed == 0 {
        return fmt.Sprintf("Cleared %d stale agent session(s) (no live processes):", stale)
    }
    parts := []string{}
    if killed > 0 {
        parts = append(parts, fmt.Sprintf("Stopped %d", killed))
    }
    if stale > 0 {
        parts = append(parts, fmt.Sprintf("%d stale entry cleared", stale))
    }
    if failed > 0 {
        parts = append(parts, fmt.Sprintf("%d failed", failed))
    }
    return strings.Join(parts, ", ") + ":"
}
```

`FormatResetResults` 同结构,差别只在 `Action` 字符串(`"in-place-reset"` / `"marked-fresh"`)和对应模板。

---

## 7. 不变式 checklist

- [ ] `/close` **永不动** `selectedCwd` / `selectedAgent` / `currentTurnUserMsgID` / `InputBuffer`
- [ ] `/close` 调 `as.Close()` 而不是 `cs.pool = make(...)` 直接丢指针
- [ ] `/close` 删 `agent_sessions.json` entry 必须在 bridge 关闭 *之后*
- [ ] `/close` 整体 timeout 5s 是兜底,bridge 内部 2s 已 SIGKILL,几乎不触发
- [ ] `/new` 对 dead/detached 也清 `ResumeID`,不 silently skip
- [ ] `/new` 不 spawn 任何 dead agent
- [ ] `/new` **不动** `currentTurnUserMsgID`(下条消息自然重新锚)
- [ ] `/new` 仍然 `InputBuffer.Clear()`(F-34 review #1 不变)
- [ ] handler 报 per-entry 列表,不是 `count`
- [ ] bridge 自治 graceful shutdown,nightme 不二次升级

---

## 8. 错误处理矩阵

### 8.1 `/close`

| 场景 | 行为 |
|------|------|
| pool 空 | "No active agents to kill." |
| 所有 Running 的 entry 都 graceful OK | `Stopped N agent session(s):` + 每行 ✓ |
| bridge.Close 内部 SIGKILL 兜底触发 | `Stopped N...` (outcome 一样,error msg 由 bridge log) |
| bridge 整体 timeout 5s | `log.Warn` + 继续清 disk,返回 best-effort 结果 |
| 某 entry bridge.Close() 报错 | 单行 `✗ ... — error: <msg>`;其他 entry 继续 |
| registry 写失败 | `KillAll` 返回整体 error,handler `reply "Kill failed: ..."` |

### 8.2 `/new`

| 场景 | 行为 |
|------|------|
| pool 空 | `Matched 0 sessions.`(沿用 F-34) |
| 全部 Running,bridge.New OK | `Reset N session(s):` + 每行 `✓ reset in-place` |
| 全部 dead | `Marked N session(s) fresh for next spawn:` + 每行 `✓ already exited, ResumeID cleared` |
| 混合 | `Reset N session(s):` + mixed |
| bridge.New报错 | 单行 `✗ ... — bridge reset: <error>`;其他 entry 继续 |
| InputBuffer 已空 | `Clear()` no-op,handler 仍 assert |

---

## 9. 测试计划

### 9.1 `internal/chatsession/kill_test.go`(新建)

```
TestKillAll_GracefulShutdown
  - spawn fake agent
  - 调 KillAll
  - 断言:fake agent.Close() 被调
  - 断言:fake agent Events() 关闭后 SetExited(0) 触发
  - 断言:fake agent 没收到 SIGKILL(graceful 路径直接通过)

TestKillAll_GracefulTimesOut_BridgeEscalates
  - spawn fake agent,Close() hang > 2s 模拟 grace 失败
  - bridge 内部 watchdog 升级到 SIGKILL
  - 调 KillAll,设 killGraceTotal 略高于 2s
  - 断言:超时 5s 内 SIGKILL 已发出,nightme 端不二次升级

TestKillAll_InputBufferPreserved
  - 排队 3 条
  - 调 KillAll
  - 断言:inputBuffer.Len() == 3

TestKillAll_AgentSessionEntriesDeleted
  - spawn 2 个 agent → /cwd → spawn 2 个
  - 调 KillAll
  - 断言:agent_sessions.json 里这 4 个 entry 全删
  - 断言:pool entry 状态 Exited(ObserveClose 触发)

TestKillAll_ActiveASCleared
  - 调 KillAll
  - 断言:cs.SelectedAgentSession() == nil
  - 断言:cs.currentTurnUserMsgID == ""

TestKillAll_OnlyExitedEntries
  - mock pool 全是 Status=Exited(无进程)
  - 调 KillAll
  - 断言:每条 result.Action == "stale-cleared"
  - 断言:无 bridge.Close 调用
  - 断言:FormatKillResults 输出 "Cleared N stale agent session(s)"

TestKillAll_ResultsSortedStable
  - 3 个 entry,(success, failure, success) 顺序构造
  - 调 KillAll
  - 断言:results 已按 (失败在后) → agent → cwd 排序
```

### 9.2 `internal/chatsession/new_test.go`(扩展)

```
TestNewActiveAgentSessions_ClearsResumeIDForExited
  - pool 有 (claude, /A) Status=Exited,ResumeID="old-id"
  - 调 NewActiveAgentSessions
  - 断言:as.ResumeID() == ""
  - 断言:agent_sessions.json entry.ResumeID == ""
  - 断言:matched == 1, reset == 1
  - 断言:Handle() == nil(没动进程)

TestNewActiveAgentSessions_ClearsResumeIDForDetached
  - 同上,Status=Detached(daemon restart 后状态)

TestNewActiveAgentSessions_DoesNotSpawn
  - Spawner.Spawn 加 spy
  - 调 NewActiveAgentSessions 命中 Exited entry
  - 断言:Spawner.Spawn 未被调

TestNewActiveAgentSessions_Running_HitsInPlaceReset
  - spawn 1 个 Running
  - 调 NewActiveAgentSessions
  - 断言:as.Handle().New() 被调(bridge reset)
  - 断言:as.Status() == StatusRunning(原进程没死)

TestNewActiveAgentSessions_BufferClearedEvenIfMatched0
  - 排队 2 条
  - 调 NewActiveAgentSessions 没有 Running entry
  - 断言:inputBuffer.Len() == 0
```

### 9.3 `internal/chatsession/format_test.go`(新建)

```
TestFormatKillResults_Empty
  - 输入:[]KillResult{}
  - 输出:"No active agents to kill."

TestFormatKillResults_AllKilled
  - 2 个 KILLED
  - 输出:含 "Stopped 2" + 2 行 ✓

TestFormatKillResults_AllStale
  - 2 个 STALE-CLEARED
  - 输出:含 "Cleared 2 stale" + 2 行 •

TestFormatKillResults_Mixed
  - 1 killed + 1 stale + 1 failed
  - 输出:含 "Stopped 1... 1 stale entry cleared, 1 failed:" + 3 行

TestFormatKillResults_SortedSuccessFirst
  - 手动构造结果,failure 在 success 前
  - 输出:success 排在前面(✓ ... ✓ ... ✗ ...)

TestFormatKillResults_LongListTruncated
  - 25 个 entry
  - 输出:首 20 行 + "  ... and 5 more"

TestFormatResetResults_AllRunning
  - 2 个 in-place-reset
  - 输出:"Reset 2 session(s):" + 2 行 ✓

TestFormatResetResults_AllDead
  - 2 个 marked-fresh
  - 输出:"Marked 2 session(s) fresh for next spawn:" + 2 行 ✓
```

---

## 10. 不在范围内（Out of Scope）

- **🟢 改 bridge.Close() 实现**:本次只调用现有 Close(),不动 bridge 自己的 graceful shutdown 时序(2s grace + SIGKILL)。
- **🟢 改 `/cwd` / `/use` 语义**:已经是正确的不动 agent 进程,继续保留。
- **🟢 改 Feishu card 渲染**:先用 plain text,等用户反馈再升级 Card 2.0 富文本。
- **🟢 引入 `StatusDraining` 中间态**:当前依赖 `ObserveClose` 异步感知 entries 死亡,wg.Wait + bridge 内部 2s SIGKILL 兜底足够。
- **🟢 `/close <agent>` 细粒度**:本次先做"全 pool kill",agent 子集过滤可以下一个 PR。

---

## 11. 决策记录

| # | 决策 | 结论 |
|---|------|------|
| D-1 | `/close` 是否清 InputBuffer | **否**。用户消息不属于 `/close` 的语义边界。 |
| D-2 | `/close` 是否清 `currentTurnUserMsgID` | **是**。下一条消息该重新锚。 |
| D-3 | `/close` 是否做 SIGKILL 兜底 | **不**。bridge 内部 2s 后已 SIGKILL,nightme 端做会和 bridge 抢信号。 |
| D-4 | `/close` 整体 timeout | **5s**。比 bridge 内部 2s 多一倍,留 SIGKILL 余量。 |
| D-5 | `/new` 是否 spawn dead agent | **否**。只清 ResumeID。 |
| D-6 | `/new` 是否清 `currentTurnUserMsgID` | **否**。下条消息自然重新锚。 |
| D-7 | `/new` 是否清 InputBuffer | **是**。沿用 F-34 review #1。 |
| D-8 | `/new` 对 dead entry 是否静默 | **否**。返回 per-entry result,带 `marked-fresh` Action。 |
| D-9 | `/close` / `/new` 报告形式 | **per-entry 列表**,带 `✓` / `✗` / `•` 标记。 |
| D-10 | 报告格式 | **plain text**(后续可升级 Card 2.0)。 |
| D-11 | 长度截断 | **20 行 + "... and N more"**。 |
| D-12 | 报告用词 | **"Stopped" / "Reset" / "Cleared" / "Marked fresh"**。"kill" 实现是 graceful,不用 "killed"。 |

---

## 12. 实施清单

### 12.1 `internal/chatsession/chatsession.go`

- [ ] 新增 `KillResult` struct(Agent, Cwd, BeforeState, Action, Error)
- [ ] 新增 `ResetResult` struct(Agent, Cwd, BeforeState, Action, Error)
- [ ] `KillAll` 改签名 `() ([]KillResult, error)`,实现 graceful + 5s timeout + 后置删 disk
- [ ] `NewActiveAgentSessions` 改签名 `(...) (matched, reset int, results []ResetResult, firstErr error)`,dead/detached 分支清 ResumeID + 持久化
- [ ] 新增 `FormatKillResults` helper
- [ ] 新增 `FormatResetResults` helper
- [ ] 新增 `killGraceTotal = 5 * time.Second` 常量

### 12.2 `internal/command/close/close.go`

- [ ] `Handle` 改用 `FormatKillResults`（位于 `internal/command/close/format.go`）

### 12.3 `internal/command/newcmd/cmd.go`

- [ ] `Handle` 改用 `FormatResetResults`

### 12.4 测试

- [ ] `internal/chatsession/kill_test.go`(新建,7 个 case)
- [ ] `internal/chatsession/new_test.go`(扩展,5 个新 case)
- [ ] `internal/chatsession/format_test.go`(新建,8 个 case)

### 12.5 文档

- [ ] SPEC.md §3.2 状态转换触发器表更新(`/close` 行注明走 graceful)
- [ ] F-34 §6 错误处理矩阵加 dead/detached 的 result 行
- [ ] F-34 README linking 加 F-43

### 12.6 估计工作量

| 类别 | 行数 |
|------|------|
| `chatsession.go` 改动 | ~120 |
| handler 改动 | ~10 |
| tests | ~330 |
| 文档 | ~80 |
| **合计** | ~540 |

---

## 13. 风险与回滚

| 风险 | 缓解 |
|------|------|
| Bridge `Close()` hang 超过 5s 导致 `/close` UX 慢 | `killGraceTotal` 可调;bridge 内部 2s 已 SIGKILL |
| `SetResumeID("")` + 持久化 race(其它 goroutine 同时 `LookupSelectedAgentSession`) | entry 复用已有 `resumeIDMu`,Upsert 是 atomic write |
| `/close` 保留 InputBuffer 但用户期望清 | 设计决策(D-1);若用户反馈,加 `/clear-buffer` 单独命令 |
| `agent_sessions.json` 没 ResumeID 字段 | GO JSON 容忍,不破坏现有数据 |

**回滚方案**:改动都在 `internal/chatsession/` 内部,git revert 那个 commit 即可,不涉及 schema 或 wire format。

---

## 14. 后续 PR(不在本次)

- [ ] `/close <agent>` agent 子集过滤
- [ ] 升级 Feishu Card 2.0 富文本渲染(目前 plain text)
- [ ] per-entry 操作耗时显示(`killed (1.2s)`)
- [ ] 长结果 list 折叠(accordion)

---

