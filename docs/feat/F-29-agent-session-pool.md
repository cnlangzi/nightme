# F-29: AgentSession Pool

> **Status**: locked (v1.2; shipped in commits 7/8c on `fix/cwd_session`; 2026-08-02)
> **Milestone**: v1.2 (commit 3 of "ChatSession refactor")
> **Depends on**: F-27 (ChatSession), F-09 (Agent interface), F-19/F-21 (Bridge modes)
> **Replaces**: v1.1 single-session-per-chat (Session type)
> **Used by**: F-28 (`/use`), ChatSession.LookupActiveAgentSession
> **Related docs**: [`SPEC.md`](../SPEC.md) v1.2 §3, [`PRD.md`](../PRD.md) v1.2 §4.3

---

## 1. Description

`AgentSession` is the **per-CLI-process handle** in v1.2. Each `AgentSession` is uniquely identified by the immutable tuple `(chatSessionId, agent, cwd)`. A ChatSession owns a **pool** of AgentSessions — one per `(agent, cwd)` combination.

The pool enables:
- **Lazy reuse** — switching to a previously-used agent/cwd reuses the existing process
- **Preservation across switches** — `/cwd` and `/use` never kill AgentSessions; old entries stay in the pool
- **Independent state** — each AgentSession has its own PID, status, Bridge, and conversation context

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

    // Bridge
    bridge        bridge.Bridge  // PTY | ACP | SDK | JSON-IO
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
| First `/use` | lookup `(activeAgent, activeCwd)` → spawn → add to pool |
| `/use` (reuse) | no change (entry already exists) |
| `/use` (new agent, same cwd) | spawn new AgentSession → add to pool |
| `/use` (same agent, new cwd) | spawn new AgentSession → add to pool |
| `/cwd` (any) | **no change** to pool; activeCwd updated |
| `/kill` | **all** entries killed and removed from pool |
| Agent process dies (natural exit) | status → Exited; **entry remains in pool** (pid=0) |
| Agent process detached (daemon restart) | status → Detached; entry remains; pid=0 |
| Daemon restart → restore | all entries restored with status=Detached; respawn on lookup |

### 3.3 Pool size limits

**No hard limit** in v1.2. Typical scenarios:
- 2 agents × 2 cwds = 4 AgentSessions max
- Power user with 5 agents × 3 projects = 15 AgentSessions
- Resource cost: each idle AgentSession ≈ 5-10MB RSS (PTY child)

Future v0.4+ may add LRU eviction if pool grows unbounded.

---

## 4. Spawn / Reuse / Respawn lifecycle

### 4.1 First spawn

```
LookupActiveAgentSession():
  1. pool[(activeAgent, activeCwd)] miss
  2. spawn (activeAgent, activeCwd) — no runtime fallback to any "default" agent
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
     h. cs.activeAS = agentSession
```

### 4.2 Reuse

```
LookupActiveAgentSession():
  1. pool[(activeAgent, activeCwd)] hit
  2. status check:
     a. Running → cs.activeAS = as; RegisterEventCallback; return as
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
  3. cs.activeAS = nil
  4. cs.inputBuffer.Clear()  // queued messages lost
  5. poolMu.Unlock()
  6. registry.Upsert(ChatSessionEntry{...agentSessionIds=[], activeAgentSessionId=null})
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
| `claude` | `JSON-IO` (v0.2+) | Stream-JSON protocol; auto-accept permissions |
| `codex` | `ACP` (v0.3+) | Agent Communication Protocol; structured events |
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
// cmd/nightme/run_v12.go (excerpt)

spawner := chatsession.NewRegistrySpawner(agents)
mgr := chatsession.NewManager().
    WithSpawner(spawner).
    WithPersistence(csFile, asFile)

cs := mgr.GetOrCreate(chatID, chatType, cfg.Primary)
// cs already has spawner inherited via Manager.GetOrCreate path
```

See [`F-27` §5.1.1](./F-27-chatsession.md) for the Spawner
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

**Race A: /use while old activeAS is processing**

```
T0: claude_AS processing, 5 events queued in as.events
T1: User sends /use codex
T2: handler.use takes cs.poolMu.Lock
T3: SetActiveAgent("codex")
T4: LookupActiveAgentSession → spawn codex_AS, set cs.activeAS = codex_AS
T5: cs.poolMu.Unlock
T6: claude_AS.readPump picks next event from claude.events
T7: callback(s=claude_AS, ev) → check cs.activeAS == claude_AS (under poolMu.RLock) → NO → drop
T8: codex_AS.readPump picks events from codex.events → callback → cs.activeAS == codex_AS → YES → Translate + Send
```

**Drop semantics**: Old in-flight events from claude_AS are dropped silently. User sees only codex output.

**Race B: Concurrent lookup from message and /use**

```
T0: User A sends "hi"
T1: User B sends /use codex
T2: messageDispatcher branch ("hi") acquires poolMu.Lock
T3: LookupActiveAgentSession → resolves (claude, activeCwd) → cs.activeAS = claude_AS
T4: poolMu.Unlock
T5: "hi" dispatched to claude_AS
T6: handler.use acquires poolMu.Lock
T7: LookupActiveAgentSession → spawns codex_AS → cs.activeAS = codex_AS
T8: poolMu.Unlock
```

**Result**: "hi" goes to claude (correct). Future messages go to codex (correct).

### 6.4 Lock ordering

`cs.poolMu` is the single lock guarding pool state. No nested locks. Event callbacks take `poolMu.RLock` for `activeAS` check — no deadlock risk.

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

**Migration from v1.1** (archive only — no transparent data lift):

```go
// internal/registry/migrate.go

func MigrateV1ToV2(v1RegistryPath string) (int, error)
```

v1.1 did not persist `chat_id` on its session records (the binding
was in-memory only — see `internal/gateway/binding.go` v1.x), so
the chat → session mapping cannot be reconstructed from disk
alone. **The current dev does not transparently migrate v1.x data
to v1.2 entries.** The startup flow (`cmd/nightme/run_v12.go`)
calls `MigrateV1ToV2(v1RegistryPath)` which:

1. Reads `registry.json` if present.
2. Copies it to `registry.json.v1.bak` (idempotent; existing
   backup is preserved).
3. Does **not** write any v1.2 entries — the v1.x data is
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
- Pool cleared on /kill
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

- **LRU eviction** (v0.4+)
- **Cross-chat AgentSession sharing** (明确不做 — 每个 chat 独立进程池)
- **Pool rebalancing** (auto-promote another entry to active)
- **AgentSession snapshot/restore** (PTY process state cannot be snapshotted; only metadata persists)
- **Hot attach** to existing detached AgentSession's process (always respawn)

---

## 11. Open questions (draft)

- **Q-J**: When pool has (claude, /A) status=Exited, and user sends /use claude without /cwd — does lookup reuse the exited entry (respawn) or treat as new? (Lean: respawn; same identity)
- **Q-K**: When user sends /use claude with cwd-change-via-extraArgs (e.g., `--cd /other`), should that affect pool key? (Lean: no, extraArgs are spawn args only; cwd is ChatSession.ActiveCwd)
- **Q-L**: Should /list show pool or only active? (Lean: show pool, with `*` marker for active)
- **Q-M**: When two ChatSessions (different chats) both have (claude, /A), are PIDs guaranteed different? (Lean: yes, each ChatSession manages its own pool, spawn is per-pool)

---

## 12. Change log

- **2026-08-02** — v1.2 draft: AgentSession pool designed; replaces v1.1 single-session-per-chat. Pool membership rules, spawn/reuse/respawn/kill semantics, race scenarios, persistence schema, migration from v1.1 drafted.