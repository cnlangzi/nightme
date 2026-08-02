# F-27: ChatSession Model

> **Status**: locked (v1.2; shipped in commits 5/6/8b/8c on `fix/cwd_session`; 2026-08-02)
> **Milestone**: v1.2 (commit 1 of "ChatSession refactor")
> **Depends on**: F-26 (v1.1 responsibility isolation), F-08 (Channel), F-20 (Gateway)
> **Replaces**: v1.1 `Session` (see F-01/F-07 for original design)
> **Used by**: F-28 (`/use`), F-29 (AgentSession pool), F-25 (InputBuffer FSM)
> **Related docs**: [`SPEC.md`](../SPEC.md) v1.2 §1.2/§1.3/§3, [`PRD.md`](../PRD.md) v1.2 §4.3/§4.6

---

## 1. Description

`ChatSession` is the **persistent session context** bound 1:1 to an IM chat (chat_id). It replaces v1.1's `Session` type, splitting v1.1's single-layer Session into:

- **ChatSession** (this feature) — the persistent, per-chat state container
- **AgentSession** (F-29) — the per-CLI-process handle, pooled under ChatSession

The split exists because v1.2 needs:
1. **Agent switching mid-conversation** — same chat, different agents
2. **Process preservation across switches** — switch agent, switch back, get the original process (and its context) back
3. **Persistence independence** — chat context (active cwd / agent) survives daemon restart; CLI processes may die and be respawned

---

## 2. Data structure (v1.2)

```go
// internal/chatsession/chatsession.go

type ChatSession struct {
    ID              string              // derived from chatID; natural key
    ChatID          string              // unique — gateway.bindings[ChatID] → this
    ChatType        string              // p2p | group | topic_group | ""

    // Active routing state (mutated by /cwd /use; primaryAgent is
    // captured at New() time from global config and never mutated).
    ActiveCwd       string              // /cwd sets; immutable per AgentSession
    ActiveAgent     string              // /use sets; immutable per AgentSession
    PrimaryAgent    string              // snapshot of cfg.Primary at New(); read-only

    // AgentSession pool (per-ChatSession unique on (agent, cwd))
    poolMu          sync.RWMutex
    pool            map[agentCwdKey]*AgentSession  // agent + cwd → AgentSession
    activeAS        *AgentSession                  // current active (may be nil)

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
    ActiveCwd           string    `json:"activeCwd"`            // empty → not yet /cwd'd
    ActiveAgent         string    `json:"activeAgent"`          // empty → not yet /use'd
    PrimaryAgent        string    `json:"primaryAgent"`         // snapshot of cfg.Primary at creation; read-only
    AgentSessionIDs     []string  `json:"agentSessionIds"`      // pool index
    ActiveAgentSessionID *string   `json:"activeAgentSessionId"` // null → no active
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
     ├─ exists → load ChatSession (Restore), SetActiveCwd
     └─ missing → ChatSession.Create({ChatID, ChatType, ActiveCwd})
                   → registry.Upsert(ChatSessionEntry)
                   → bindings[ChatID] = new ChatSession
```

### 3.2 Restoration (after daemon restart)

```
nightme run (startup):
  1. registry.LoadAll() → []ChatSessionEntry, []AgentSessionEntry
  2. For each ChatSessionEntry:
     ├─ new ChatSession (id, chatId, chatType, activeCwd, activeAgent, primaryAgent)
     ├─ For each agentSessionID:
     │   └─ new AgentSession (id, agent, cwd, status=Detached, pid=0)
     │       └─ chatSession.pool[(agent,cwd)] = agentSession
     └─ activeAS = pool[(activeAgent, activeCwd)]  // may be nil if cwd changed
  3. bindings[ChatID] = restored ChatSession
```

**Detached state**: `pid=0, status=Detached`. Process not running; will be respawned on first message via `LookupActiveAgentSession()`.

### 3.3 Active AgentSession lookup (the heart of v1.2)

```go
// LookupActiveAgentSession is the ONLY entry point for resolving
// "which AgentSession should this message go to".
//
// Logic (single path — no runtime fallback):
//   - ChatSession always carries an effective activeAgent:
//       · at construction, activeAgent is seeded from cfg.Primary
//       · /use overwrites activeAgent
//   - Resolve pool[(activeAgent, activeCwd)]:
//       · hit (StatusRunning) → reuse, register callback, return
//       · miss (or non-Running) → spawn (activeAgent, activeCwd)
//
// Returns ErrNoActiveCwd if activeCwd is empty (user has not /cwd'd yet).
func (cs *ChatSession) LookupActiveAgentSession() (*AgentSession, error) {
    cs.poolMu.Lock()
    defer cs.poolMu.Unlock()

    if cs.ActiveCwd == "" {
        return nil, ErrNoActiveCwd  // "no workspace, /cwd first"
    }

    key := agentCwdKey{Agent: cs.ActiveAgent, Cwd: cs.ActiveCwd}
    if as, ok := cs.pool[key]; ok {
        if as.Status() == StatusExited {
            // Process died; respawn with same (agent, cwd) entry
            as.Respawn(cs.ActiveCwd)
        }
        cs.activeAS = as
        as.RegisterEventCallback(cs.eventCallback)
        return as, nil
    }

    // Miss: spawn (activeAgent, activeCwd). No fallback to any
    // "default" agent — chatSession.activeAgent is the only
    // authority (seeded from cfg.Primary at New() time, /use
    // overrides it).
    as := SpawnAgentSession(cs.ActiveAgent, cs.ActiveCwd)
    cs.pool[key] = as
    as.RegisterEventCallback(cs.eventCallback)
    cs.activeAS = as
    cs.flushAgentSessionsToRegistry()
    return as, nil
}
```

### 3.4 Slash command handlers (v1.2 changes)

| Command | Handler behavior | Side effects |
|---------|------------------|--------------|
| `/cwd <path>` | Validate → `chatSession.SetActiveCwd(abs)` | Updates `activeCwd`; pool untouched; next message triggers `LookupActiveAgentSession()` |
| `/use <agent>` | Validate → `chatSession.SetActiveAgent(name)` → `LookupActiveAgentSession()` | May spawn new AgentSession if `(agent, activeCwd)` not in pool |
| `/kill` | `chatSession.KillAll()` | Kills every AgentSession in pool; clears `activeAS`; old receipts dispose |

**No `/default` command** (Q-A simplification, 2026-08-02): the only user-facing Primary Agent is the global `primary` config. The `primaryAgent` field on ChatSession is captured at `New()` time (snapshot of `cfg.Primary`) and never mutated post-construction. Future feature: per-chat Primary via config (not command) — out of scope for v1.2.

### 3.5 SetActiveCwd / SetActiveAgent (state mutations)

```go
func (cs *ChatSession) SetActiveCwd(cwd string) error {
    if !isValidCwd(cwd) {
        return ErrInvalidCwd
    }
    cs.poolMu.Lock()
    cs.ActiveCwd = cwd
    cs.poolMu.Unlock()
    cs.flushEntryToRegistry()
    // No agent respawn here — LookupActiveAgentSession happens lazily
    return nil
}

func (cs *ChatSession) SetActiveAgent(agent string) error {
    cs.poolMu.Lock()
    cs.ActiveAgent = agent
    cs.poolMu.Unlock()
    cs.flushEntryToRegistry()
    return nil
}
```

**Critical invariant**: these methods MUST NOT spawn or kill any AgentSession. Spawning is lazy (next message). Killing is explicit (`/kill`).

---

## 4. Concurrency model

### 4.1 Goroutines owned by ChatSession

- **None directly**. ChatSession is a state container; goroutines live in AgentSession (readPump per AgentSession).

### 4.2 Per-AgentSession goroutines

- **`AgentSession.readPump`** (one per AgentSession in pool, running or detached)
  - Reads from `as.Events()` (single consumer)
  - Calls `cs.eventCallback(s, ev)` if `s == cs.activeAS` (set under poolMu read lock)
  - Drops events from non-active AgentSession (logs at debug level)

### 4.3 Locks

- `cs.poolMu` — guards `pool`, `activeAS`, `ActiveCwd`, `ActiveAgent`
  - **Write**: `/cwd`, `/use`, `/kill`, `LookupActiveAgentSession()`, spawn, registry flush
  - **Read**: readPump callback registration, status queries

### 4.4 /use switch race window

When `/use` switches active:
```
1. /use claude enters LookupActiveAgentSession
2. Take poolMu.Lock
3. Resolve new activeAS (reuse or spawn)
4. Set cs.activeAS = newAS
5. Release poolMu.Lock
6. (In flight events from OLD AgentSession's readPump, if any:)
   - Check cs.activeAS under poolMu.RLock
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
| ChatSession.ActiveAgentSession pointer | ChatSession | `cs.activeAS` (under poolMu) | /use 时切换 |

---

## 5.1 Runtime contracts (seams between ChatSession and the runtime)

ChatSession is pure data + FSM; it knows nothing about agents,
channels, or the gateway. The runtime injects three pieces of
behaviour that make the FSMs come alive.

### 5.1.1 Spawner (lazy fork-exec seam)

`Spawner` is the only way a ChatSession brings a new AgentSession
to life (Step 3 of `LookupActiveAgentSession`). It is
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

See [`docs/feat/F-29-agent-session-pool.md`](./F-29-agent-session-pool.md)
§3.1 for the production wiring.

### 5.1.2 Default FlushHook (queued-message forwarder)

`ChatSession.ensureBuffer` installs a **default** FlushHook on the
InputBuffer at construction time:

```go
func (cs *ChatSession) defaultFlushHookLocked() FlushHook {
    return func(combined []agent.ContentBlock, userMsgIDs []string) error {
        as := cs.activeAS
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
`cmd/nightme/run_v12.go::v12EventHandler`):

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

- **`StartReadPump()`** — start pump for `cs.activeAS`. Captures
  `cs.eventHandler` at start time. Stops any existing pump first.
  Returns `ErrNoActiveAgentSession` if no active AS yet.
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
  (`internal/gateway/handlers_chatsession.go::handleUse`).

**Why not auto-start on spawn?** LookupActiveAgentSession does
**not** auto-start the pump — keeps ChatSession unit-testable
without leaking goroutines (commit 8c).

### 5.1.5 Exit observer (process death notification)

`StartObserveClose` launches a goroutine that drains an
AgentSession's events channel to detect close. When the channel
closes (process died), the registered `AgentExitObserver`
fires. Currently the runtime does not wire an observer — the
readPump's natural exit is sufficient. The API is reserved
for future work (e.g., respawn on death, /kill auto-reply).

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

## 6. Registry schema (v1.2)

```jsonc
// File: ~/.local/share/nightme/registry/chat_sessions.json
{
  "version": 2,
  "chatSessions": {
    "<chatSessionId>": {
      "id": "cs_xxx",
      "chatId": "oc_xxx",
      "chatType": "p2p",
      "activeCwd": "/code/bailing",
      "activeAgent": "claude",
      "primaryAgent": "claude",
      "agentSessionIds": ["as_1", "as_2"],
      "activeAgentSessionId": "as_1",
      "createdAt": "2026-08-02T...",
      "lastInteractionAt": "2026-08-02T..."
    }
  }
}

// File: ~/.local/share/nightme/registry/agent_sessions.json
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

**Migration from v1.1 registry**:
- v1.1 had single `sessions.json` with `SessionEntry{ChatID, Workspace, Agent, Args, PID, Status}`
- v1.2 migration:
  1. For each v1.1 SessionEntry: create v1.2 ChatSessionEntry(chatId, chatType="p2p") + AgentSessionEntry(agent, cwd=workspace, pid, status)
  2. Wire references: `chatSessionEntry.agentSessionIds = [newAS.id]`
  3. Write both v1.2 files; archive v1.1 file as `sessions.v1.json.bak`

---

## 7. Test strategy

### 7.1 Unit

- `SetActiveCwd` / `SetActiveAgent` — pure state mutation, no spawn/kill
- `LookupActiveAgentSession()` resolution (single path: hit → reuse, miss → spawn `(activeAgent, activeCwd)`; no runtime fallback to any "default" agent)
- `KillAll()` — all AgentSessions killed, activeAS=nil, pool emptied
- `Restore()` from ChatSessionEntry + AgentSessionEntry — detached state, no process
- Concurrent `/use` + activeAS read — race-free (uses sync.RWMutex)

### 7.2 Integration

- `nightme run` → /cwd → /use → message → /use (switch) → /use (switch back) → assert same PID
- /kill → assert all PIDs gone → message → assert new PIDs spawned
- Daemon restart with active cwd/B → assert AgentSession for (A,A) detached but still in pool

### 7.3 Regression (E2E)

- `nightme run --channel=feishu`: all v0.x slash commands work (`/cwd` new semantics, `/use`, `/kill`, `/help`, `/agents`)
- `/use codex` after `/use claude` — rolling-log receipt cards remain coherent (Receipt FSM不变)
- Concurrent /use while AgentSession events flowing — no race; old events dropped

---

## 8. Out of scope (v1.2 / F-27)

- **Multi-AgentSession parallelism** (同一 chat 多 agent 同时跑) — v0.4+
- **Cross-chat ChatSession sharing** (一个 chat 共享另一个 chat 的 AgentSession) — 明确不做
- **Hot-reload of ChatSessionEntry** while messages in flight — restart-based only
- **AgentSession migration across machines** — single-machine only
- **Auto-discovery of AgentSession when status=Exited** — explicit `/use` only triggers respawn

---

## 9. Open questions (draft)

- **Q-A**: Default Agent setting granularity — global config only? per ChatSession command? both? (Lean: both)
- **Q-B** (closed 2026-08-03): lookup only resolves `(activeAgent, activeCwd)`. No runtime fallback. activeAgent is seeded from `cfg.Primary` at ChatSession creation and only mutated by `/use`.
- **Q-C**: Should `chatSession.SetActiveCwd` log to user "activeCwd changed, next message will spawn new AgentSession"? (Lean: yes, ephemeral info message)
- **Q-D**: When `/kill` clears pool, should queued InputBuffer messages be persisted or dropped? (Lean: dropped; user explicitly killed)
- **Q-E**: ChatSession.ID is generated once or derived from chatId? (Lean: derived from chatId for 1:1 invariant enforcement)

---

## 10. Change log

- **2026-08-02** — v1.2 draft: ChatSession split out from v1.1 Session; replaces F-01/F-07/F-25 ownership of state. Schema, lifecycle, concurrency model, registry schema drafted.