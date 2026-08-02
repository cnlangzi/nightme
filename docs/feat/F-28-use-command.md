# F-28: `/use <agent>` Command

> **Status**: locked (v1.2; shipped in commits 8a/8c on `fix/cwd_session`; 2026-08-02)
> **Milestone**: v1.2 (commit 2 of "ChatSession refactor")
> **Depends on**: F-27 (ChatSession), F-09 (Agent interface), F-29 (AgentSession pool)
> **Replaces**: v1.1 `/run <agent>` (see F-20 for original design)
> **Used by**: every user-facing chat workflow
> **Related docs**: [`SPEC.md`](../SPEC.md) v1.2 §2.3, [`PRD.md`](../PRD.md) v1.2 §4.3

---

## 1. Description

`/use <agent>` is the v1.2 replacement for v1.1's `/run <agent>`. It switches the ChatSession's `activeAgent` and routes future messages to the corresponding AgentSession.

**Critical difference from `/run`**:
- v1.1 `/run` was an explicit spawn/reconnect command
- v1.2 `/use` is a **lazy switch** — only spawns if `(activeAgent, activeCwd)` is not already in the pool
- v1.2 `/use` **never restarts an existing process** — reuse is the default

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
/use claude                  # Switch to claude at activeCwd (reuse or spawn)
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

    // Check ChatSession has activeCwd (required for /use)
    if cs.ActiveCwd == "" {
        return g.channel.Send(ctx, OutboundMessage{
            ChatID: msg.ChatID,
            Text:   "No active workspace. Send /cwd <path> first.",
        })
    }

    // Pre-update activeAgent (pure state mutation, no spawn)
    if err := cs.SetActiveAgent(agentName); err != nil {
        return err
    }

    // Lazy lookup (may spawn or reuse)
    as, err := cs.LookupActiveAgentSession()
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

### 3.2 LookupActiveAgentSession (delegated to ChatSession)

See F-27 §3.3 for full lookup logic. The `/use` handler calls it after setting `activeAgent`. The lookup returns:
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

- ✅ Update `ChatSession.activeAgent`
- ✅ Trigger `LookupActiveAgentSession()` (may spawn)
- ✅ Switch `eventCallback` registration to new active AgentSession
- ✅ Old active AgentSession remains in pool (its events become dropped)
- ✅ Persist `ChatSessionEntry` and `AgentSessionEntry` updates

### 4.3 What happens to in-flight events

```
Timeline:
T0: User sends "fix bug X" → routed to (claude, /code/A)
T1: claude AgentSession processing, 3 events already emitted
T2: User sends "/use codex"
T3: /use handler sets activeAgent=codex, lookup (codex, /code/A) → spawn new
T4: New codex AgentSession starts; cs.activeAS = codex_AS
T5: claude's remaining events arrive at claude_AS.Events() → readPump → callback
T6: Callback checks cs.activeAS == claude_AS → NO → drop event, log "stale event"
T7: codex's events arrive at codex_AS.Events() → readPump → callback
T8: Callback checks cs.activeAS == codex_AS → YES → Translate + ch.Send
```

**Result**: User sees codex responses. Claude's remaining output is silently dropped (with debug log).

---

## 5. Edge cases

### 5.1 Same agent, same cwd (no-op reuse)

```
/use claude  # activeAgent already claude, pool has (claude, activeCwd)
→ noop, just re-confirm state
→ reply: "Already using claude, pid=N, cwd=/path/A"
```

### 5.2 Same agent, different cwd (new AgentSession)

```
state: activeAgent=claude, activeCwd=/code/A, pool has (claude, /code/A)
/use claude  # activeCwd already /code/A → same as 5.1
```

```
state: activeAgent=claude, activeCwd=/code/B (just /cwd'd), pool has (claude, /code/A) only
/cwd /code/A  # now activeCwd=/code/A, pool still has (claude, /code/A)
/use claude  # reuse (claude, /code/A), no spawn
```

```
state: activeAgent=claude, activeCwd=/code/B
/cwd /code/A  # activeCwd=/code/A
/use claude  # reuse (claude, /code/A), no spawn — but note: activeAgent was already claude
```

```
state: activeAgent=claude, activeCwd=/code/B, pool has (claude, /code/B) only
/use codex  # spawn new (codex, /code/B); activeAgent=codex
```

### 5.3 Agent exits unexpectedly

```
state: activeAgent=claude, pool has (claude, /code/A) status=Exited (PID died)
/use claude  # LookupActiveAgentSession detects Exited → respawn with same (agent, cwd)
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

### 5.6 /use when activeCwd is empty

```
/use claude  # no activeCwd
→ reply: "No active workspace. Send /cwd <path> first."
→ no state mutation
```

---

## 6. Migration from v1.1 `/run`

### 6.1 Behavior changes

| Aspect | v1.1 `/run` | v1.2 `/use` |
|--------|--------------|--------------|
| Spawn semantics | Always spawn (or reconnect if running) | Lazy spawn (reuse if pool has) |
| Restart on existing | Implicit reconnect | **Never restart**; reuse only |
| Multiple per chat | Not supported | **Multiple AgentSessions in pool** |
| /kill interaction | /kill kills the run session | /kill kills entire pool |

### 6.2 Command mapping

| v1.1 | v1.2 equivalent |
|------|-----------------|
| `/cwd <path>` then `/run claude` | `/cwd <path>` then `/use claude` |
| `/run claude` (no /cwd yet) | `/cwd <path>` then `/use claude` (or `/use` after `/cwd`) |
| `/kill` | `/kill` (kills entire pool instead of single session) |
| `/cwd <new-path>` then `/run claude` | `/cwd <new-path>` then `/use claude` (spawns new AgentSession for new cwd) |

### 6.3 Backward compatibility

**No backward compatibility** — v1.2 is a breaking change for `/run`. Migration path:
1. v1.1 users upgrade to v1.2
2. Their persisted v1.1 sessions.json auto-migrates to v1.2 ChatSessionEntry + AgentSessionEntry (see F-27 §6)
3. Existing `/cwd` settings preserved; AgentSession = (agent=their-configured-agent, cwd=their-workspace)
4. Future behavior: `/use claude` will reuse that existing AgentSession (no re-spawn needed)

---

## 7. Test strategy

### 7.1 Unit

- `handleUse` with various (activeAgent, pool) combinations
- LookupActiveAgentSession() — exact match → reuse, miss → spawn `(activeAgent, activeCwd)`, respawn on exited; no runtime fallback to any "default" agent
- /use with no activeCwd → error reply
- /use with unknown agent → error reply
- /use when already on that agent → noop

### 7.2 Integration

- /use claude → /use codex → /use claude: assert same PID for claude at each "use claude" call
- /kill → /use claude: assert new PID (old AgentSession removed from pool)
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
- **Multi-agent per message** (route one message to multiple agents) — v0.4+
- **Agent capability negotiation** (only allow /use codex if codex supports cwd) — not needed, agents handle own validity
- **/use with prompt override** — extraArgs passed to spawn only, not applied to existing sessions

---

## 10. Open questions (draft)

- **Q-F**: Should `/use` without activeCwd auto-default to a workspace (e.g., `~/.openclaw/workspace`)? (Lean: no, require explicit `/cwd`)
- **Q-G**: When extraArgs provided but AgentSession already exists, silently drop or warn? (Lean: warn in reply "args ignored, agent already running")
- **Q-H**: /use reply format — single line vs multi-line status (pid, cwd, agent, uptime)? (Lean: multi-line for diagnostic clarity)
- **Q-I**: Should `/use` support a keyword to reset activeAgent to primaryAgent? (Lean: no — Q-A simplified to global Primary only; per-chat Primary not exposed via command)

---

## 11. Change log

- **2026-08-02** — v1.2 draft: `/use` command designed to replace `/run`. Lazy spawn semantics, pool reuse, never restart existing process.