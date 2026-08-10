# F-38: Tool-event Thread-Merge + `/tools on|off` Toggle

> **Status**: Implemented (2026-08-04)
> **Scope**: v1.3.x F-thread-route follow-up
> **Files (current)**:`internal/chatsession/tools_mode.go`、`internal/command/tools/cmd.go`、`cmd/nightme/run.go::newEventHandler`、`internal/channel/feishu/tool_thread_merge.go`、`internal/channel/feishu/adapter.go::Send`
> **Files (original F-38 commit, 已被 F-102 重构迁移)**:`internal/registry/tools_mode.go`（已并入 `chatsession/tools_mode.go`）、`internal/chatsession/toolsmode.go`（已改名为 `tools_mode.go`）、`internal/gateway/handlers_tools.go`（slash 命令已迁移到 `internal/command/<name>/`）
> **Related**: [`F-think`](./F-29-think-mode-toggle.md) (mirrored pattern),
> [`F-watch`](./F-27-watch-mode-toggle.md) (mirror toggle), F-thread-route (predecessor)

---

## 1. Problem

F-thread-route (commit `098fdb7`) sends every `OutToolStart` and
`OutToolEnd` as its own thread reply under the user message:

```
● Bash(go build ./... 2>&1)
⎿  💻 Bash → 3 lines
```

This is correct per-pair UX (matches Claude Code's terminal
two-line format), but scales poorly:

- **Visual noise**: a hot agent that calls 10 tools in one turn
  produces **20** thread replies — the user's thread becomes
  unreadable.
- **Rate-limit cost**: each reply hits the per-chat 5 QPS bucket
  independently. A 10-tool turn blocks 2s on rate-limit waiting
  alone (10 starts + 10 ends ÷ 5 QPS = 4s; the limiter actually
  makes it 4s worst case since each Create goes through the
  limiter sequentially).
- **No opt-out**: unlike `/think` (OutThinking), there is no
  per-chat toggle to disable tool display entirely.

F-38 solves both:

1. **Merge**: each tool pair becomes **one** thread reply (call
   line + result line), PATCHed in-place by the matching End.
2. **Toggle**: `/tools on|off` controls whether tool events
   reach the Channel at all. Default off.

---

## 2. Design

### 2.1 Per-chat `ToolsMode`

Mirrors `ThinkMode` (F-think) and `WatchMode` (F-watch) — a small
`int` enum on `ChatSession` that the runtime EventHandler reads
after Translate + ReplyTo stamping and before `ch.Send`.

| Value | Meaning | Trigger |
|-------|---------|---------|
| `ToolsModeHide` (default, 0) | Drop `OutToolStart` and `OutToolEnd` at the gate | `/tools off` or fresh chat |
| `ToolsModeShow` (1) | Forward to Channel; Feishu adapter merges each pair | `/tools on` |

Default direction is **opposite** of `ThinkMode`:
- `/think` defaults to `ThinkModeShow` (preserve existing
  F-thread-route UX — users see thinking unless they opt out).
- `/tools` defaults to `ToolsModeHide` (tool spam is the loudest
  part of the agent progress stream; quiet by default; opt in to
  see tools).

Both defaults are "safe" but interpret safety differently:
- Thinking content is information-dense and rare; losing it
  silently is bad → default Show.
- Tool calls are noise-dense and frequent; losing them silently
  is OK because the receipt card still carries the final answer
  → default Hide.

### 2.2 Slash command

```
/tools on           → set ToolsModeShow, persist, reply
/tools off          → set ToolsModeHide, persist, reply
/tools              → reply current mode + usage hint
/tools maybe        → reply "Unknown tools mode" (parse fail, no state mutation)
/tools <other>      → same
```

Aliases `show` / `hide` accepted alongside `on` / `off` per the
`/think` precedent — users pick whichever phrasing they remember.

### 2.3 Runtime gate

`cmd/nightme/run.go::newEventHandler` — immediately after the
existing `ThinkMode` gate:

```go
if (out.Kind == gateway.OutToolStart || out.Kind == gateway.OutToolEnd) &&
    cs != nil && cs.ToolsMode() == chatsession.ToolsModeHide {
    logger.Info("tools dropped", "chat_id", chatID, "kind", out.Kind.String())
    return
}
```

Other `OutboundKind`s (`OutText` / `OutResult` / `OutThinking` /
`OutCompaction` / `OutInit` / `OutUsage`) are unaffected. The
gate is strictly KKind-scoped — no accidental widening to "drop
everything" if a future refactor touches it.

`ToolsMode` and `ThinkMode` gates are **independent**: setting
one doesn't affect the other. Tested by
`TestEventHandler_ToolsAndThinkGatesIndependent`.

### 2.4 Channel-internal merge (Feishu only)

When `ToolsMode=Show` and the Channel is Feishu, the adapter
merges each tool pair into a single thread reply. The flow:

**OutToolStart**:
1. Format call line: `formatToolStartCall(name, args)` → `"● Bash(ls)"`
2. `postThreadReplyWithID(...)` → posts the text reply, returns `message_id`
3. `pushToolStart(userMsgID, message_id, body)` — FIFO push onto
   `toolEventBuf[userMsgID]`

**OutToolEnd**:
1. Format result line: `summarizeToolResult(name, output, err)` → `"⎿  💻 Bash → 3 lines"`
2. `popToolStart(userMsgID)` — FIFO pop returns `(startMsgID, startBody)` or miss
3. On hit: `mergeToolReply(startMsgID, startBody + "\n" + resultBody)` —
   PATCH the start reply with the merged body (F-36 transient retry wraps it)
4. On miss (orphan End) or PATCH failure: fall back to
   `postThreadReply` (post result as a fresh thread reply) so the
   data is never silently dropped.

The user sees:

```
● Bash(ls)
⎿  💻 Bash → 3 lines
```

as a single chat message under the receipt card. 10 tools in a
turn = 10 thread replies (one per tool, merged), not 20.

**The merge is Feishu-specific Channel rendering** — it lives
entirely in `internal/channel/feishu/`. Other Channels (Echo,
future Slack / Web) see `OutboundMessage.Tool` unchanged and
decide their own rendering.

---

## 3. Data Flow

### 3.1 `/tools off` (default) — events dropped

```
EventToolStart / EventToolEnd
  → Translate → OutboundMessage{Kind: OutToolStart/End}
  → EventHandler gate (cs.ToolsMode()==Hide) → return
  → 飞书 no side effect; receipt card still carries final answer
```

### 3.2 `/tools on` — events merged

```
EventToolStart
  → Translate → OutboundMessage{Kind: OutToolStart}
  → EventHandler gate pass-through
  → Adapter.Send:
       postThreadReplyWithID(● Tool(args))    // Create
       buf[userMsgID] push (startMsgID, body)
       receiptFor + Touch (header keeps ticking)

EventToolEnd (matching pair, FIFO order)
  → Translate → OutboundMessage{Kind: OutToolEnd}
  → EventHandler gate pass-through
  → Adapter.Send:
       pop buf[userMsgID] → (startMsgID, startBody)
       mergeToolReply(startMsgID, startBody + "\n" + resultBody)
         → Feishu PATCH same message_id (F-36 retry)
       receiptFor + Touch

EventToolEnd (orphan — no matching Start in buffer)
  → Translate → OutboundMessage{Kind: OutToolEnd}
  → EventHandler gate pass-through
  → Adapter.Send:
       pop buf[userMsgID] → miss
       fallback: postThreadReply(⎿ ...)        // fresh reply, old behaviour
       receiptFor + Touch
```

### 3.3 PATCH failure path

```
mergeToolReply returns err (retry exhausted or non-transient)
  → log warn ("tool merge PATCH failed, falling back to fresh thread reply")
  → fall through to fallback postThreadReply
  → Send returns nil (data preserved via fallback)
```

The fallback preserves the **pre-F-38** behaviour for the unhappy
path: an orphan End, or a Start whose PATCH target became invalid
(message deleted by user, message_id typo, etc.) still shows up
to the user. **No silent data loss.**

---

## 4. State Lifecycle

### 4.1 `toolEventBuf` (per-adapter in-memory)

| Event | Effect on `toolEventBuf[userMsgID]` |
|-------|-------------------------------------|
| `OutToolStart` posted, msg_id returned | Push `(startMsgID, startBody)` onto the FIFO |
| `OutToolStart` posted but msg_id empty (orphan path) | No-op (push refused) |
| `OutToolEnd` matched (FIFO non-empty) | Pop front entry; PATCH; user sees merged body |
| `OutToolEnd` orphan (FIFO empty) | No buffer change; fallback to fresh reply |
| `Adapter.Stop` | `clearAllToolEvents()` drops every entry |

The buffer is **bounded** by `tools-per-turn` (typically <50). No
explicit turn-end cleanup is needed because `userMsgID` is unique
per turn (SPEC §2.2 invariant). Stale entries can only appear if
the same `userMsgID` is reused after a partial flush — discarded
on next push (rare edge case; not currently triggered).

`clearToolEvents(userMsgID)` is exposed for future turn-end hooks
(e.g. when a `OutDone` / `OutError` event is added to the
dispatch path) but currently is not auto-called.

### 4.2 `ChatSession.ToolsMode`

| Event | Effect on `cs.ToolsMode` |
|-------|---------------------------|
| `ChatSession.New(chatID, primaryAgent)` | Seeded to `ToolsModeHide` (default) |
| `Manager.RestoreFromRegistry` reads entry | Restored from `entry.ToolsMode` (0 == Hide) |
| `cs.SetToolsMode(mode)` | Mutated + persisted via `persistChatEntry` |
| `/tools on` / `/tools off` | Calls `cs.SetToolsMode(...)` |
| Restart (daemon `nightme run` exit + relaunch) | Restored from `chat_sessions.json` |

Persistence is `omitempty`-guarded: a fresh chat writes no
`toolsMode` key to disk. Old `chat_sessions.json` files
(pre-F-38) without the field decode to `ToolsModeHide` via
Go's zero-value semantics.

---

## 5. Concurrency

### 5.1 Single-consumer guarantee preserved

`AgentSession.Events()` has exactly one consumer — the
AgentSession's own `readPump`, which calls
`ChatSession.EventCallback` synchronously (SPEC §1.3, Q14).
ChatSession calls `cs.SetEventHandler(...)` once at startup; the
handler closure is the single writer to `outbound` events.

Within the handler:
- `Translate` (gateway) is single-threaded per ChatSession.
- `ch.Send` (Feishu adapter) is single-threaded per ChatSession.

So `toolEventBuf[userMsgID]` push/pop is effectively single-threaded
within a chat. The adapter still takes `a.mu` for the map op
because the runtime can have multiple chats in flight, and `a.mu`
guards the adapter's other shared state (`receipts`,
`messageStates`).

### 5.2 No new goroutines

The merge is fully synchronous inside `Send`. No timers, no
background flushers, no eviction sweeps. The buffer dies with
the adapter on `Stop`.

### 5.3 PATCH under load

Feishu's PATCH endpoint counts against the same per-chat 5 QPS
bucket as Create. `mergeToolReply` is intentionally NOT gated by
`threadReplyLimiter` because:

- The Create path already gates each new start thread reply.
- PATCH fires once per tool pair (≤ 1 per turn-second) — well
  below the 5 QPS bucket even for the hottest agent.
- Adding a limiter would serialize PATCH behind Create in the
  same chat, doubling the latency for no observable benefit.

If a future workload exceeds the bucket, add a `Wait()` on
`a.threadReplyLimiter` inside `mergeTextViaUpdate` before the
SDK call. Documented inline.

---

## 6. API Constraints (verified)

### 6.1 Feishu PUT /im/v1/messages/{id}

- **Supports** text and post message types (thread replies count
  as text when posted via the reply API path).
- **Limit**: 20 edits per message (well above any tool-call
  burst — Claude Code's per-turn tool count is typically <50,
  and the edit count per Start is exactly 1).
- **Editable time window**: Feishu's 24-hour edit window
  comfortably covers any realistic tool latency.
- **Sender restriction**: only the bot that created the message
  can edit it — trivially satisfied (we edit our own replies).
- **msg_type match**: cannot edit text → card or vice versa —
  satisfied (we edit text with text).

Sources:
- <https://open.feishu.cn/document/server-docs/im-v1/message/update>
- F-37 review noted the same API for card-PATCH.

### 6.2 No new `OutboundMessage` fields

§1.4 boundary rule: tool concept remains a typed `ToolInfo`
struct with `Name` / `Args` / `Output` / `Err`. The merge is
purely Feishu-side rendering — Gateway still emits the same
two events.

---

## 7. Failure Modes & Fallbacks

| Failure | Behaviour |
|---------|-----------|
| Orphan `OutToolEnd` (buffer empty for userMsgID) | Fallback: post resultBody as fresh thread reply (pre-F-38 UX) |
| `mergeToolReply` retry exhausted (F-36 transient) | Fallback: post resultBody as fresh thread reply + warn log |
| `mergeToolReply` non-transient error | Fallback: post resultBody as fresh thread reply + warn log |
| `pushToolStart` empty msg_id (orphan path — rootID was "") | push is a no-op; matching End falls back to fresh reply |
| `Adapter.Stop` mid-turn | `clearAllToolEvents` drops buffer; orphan Starts lose their End (acceptable — daemon is going down anyway) |
| Parallel `tool_use` blocks in one message | FIFO pairing ensures each End edits the correct Start's msg_id |
| Cross-turn orphan End (different userMsgID) | Falls back to fresh reply — never cross-matches turns (1 turn : 1 userMsgID invariant, SPEC §2.2) |
| Feishu PATCH 230071 (sender mismatch) | Rare — message was created by another bot. Retry returns error → fallback |
| Feishu PATCH 230072 (>20 edits) | Theoretically possible after 20 PATCHes on the same msg_id. Fallback still preserves data via fresh reply |

**No silent data loss** in any branch — at worst, a tool pair
becomes two thread replies again (pre-F-38 UX).

---

## 8. Files Touched

| File | Change |
|------|--------|
| `internal/chatsession/tools_mode.go` | NEW — `ToolsMode` enum + `ParseToolsMode` + `String`（F-102 重构后从 `internal/registry/tools_mode.go` 搬过来；不再有 alias 文件） |
| `internal/chatsession/tools_mode_test.go` | NEW — round-trip, missing-field default, omitempty on zero, type-safety |
| `internal/registry/chat_session_entry.go` | Add `ToolsMode int` field with `omitempty`（F-102 后由 `agent.ToolsMode` 改为裸 int） |
| `internal/chatsession/chatsession.go` | Add `toolsMode` field, default `ToolsModeHide` in `New()`, `SetToolsMode` / `ToolsMode()`, persistence cast `int(cs.toolsMode)` |
| `internal/chatsession/manager.go` | `RestoreFromRegistry` restores `cs.toolsMode = ToolsMode(entry.ToolsMode)` |
| `internal/command/tools/cmd.go` | NEW — `/tools` slash command (`Handle` method；与 `think/cmd.go` 同形态) |
| `internal/command/tools/commands_test.go` | NEW — 6 sub-tests covering toggle, aliases, lazy-create, registration, default, independence |
| `internal/command/commander.go` + `cmd/nightme/run.go` | Register `tools` command alongside `think`（走 `command.Commander` 注册表，不再用单点 `RegisterChatSessionCommands`） |
| `cmd/nightme/run.go::newEventHandler` | Add `ToolsMode` gate after `ThinkMode` gate |
| `cmd/nightme/run_test.go` | 5 new tests: Show-passes-through, Hide-drops-both, Hide-doesn't-affect-other, persists-across-invocations, Tools+Think-independent; existing `HideDoesNotAffectOtherKinds` updated to opt into `/tools on` for the OutToolStart assertion |
| `internal/channel/feishu/adapter.go` | Add `toolEventBuf` + `mergeTextFunc` fields; new `postThreadReplyWithID` helper; `OutToolStart` / `OutToolEnd` cases rewritten to merge; `Adapter.Stop` calls `clearAllToolEvents` |
| `internal/channel/feishu/tool_thread_merge.go` | NEW — `toolEventEntry`, `pushToolStart`, `popToolStart`, `clearToolEvents`, `clearAllToolEvents`, `mergeToolReply`, `mergeTextViaUpdate` |
| `internal/channel/feishu/tool_thread_merge_test.go` | NEW — 9 sub-tests covering FIFO, miss, empty msg_id no-op, clear, parallel tool_use, cross-turn isolation, PATCH failure fallback, orphan End fallback, defensive empty-msg_id guard |
| `docs/SPEC.md` | §0.7 changelog + §3.1.3 design section |

---

## 9. Out of Scope

- **Per-tool toggle**: not currently planned. `ToolsMode` is
  binary (all tool events on / off). A finer-grained "show only
  Bash / Read, hide Edit / Write" toggle is feasible later but
  out of scope for F-38.
- **Cross-channel merge**: Echo / Slack / Web all render
  `OutboundMessage.Tool` unchanged. The merge is a Feishu
  adapter detail — other Channels can opt in later without
  changes to Gateway or ChatSession.
- **Tool output preview**: the result line is always a single-
  line summary (`summarizeToolResult`); full tool output never
  reaches the Channel. Unchanged from F-thread-route.
- **Auto-disable after N turns**: not planned. User opt-out is
  always explicit (`/tools off`).

---

## 10. Backwards Compatibility

- `chat_sessions.json` files written before F-38 lack the
  `toolsMode` key. Go's zero-value semantics give them
  `ToolsModeHide` (the new safe default). No migration script
  needed.
- `OutboundMessage` shape unchanged. Gateway callers don't
  observe a difference — the merge is purely adapter-side.
- `Channel` interface unchanged. New helpers (`postThreadReplyWithID`,
  `mergeTextFunc`, etc.) are adapter-private.
- The runtime's `/tools off` default is a **change** in user-
  visible behaviour: pre-F-38, every user saw tool events in
  the thread. Post-F-38, only users who explicitly ran
  `/tools on` see them. This is the **intent** of F-38 ("tool
  spam is the loudest part of the agent stream — quiet by
  default") but is worth calling out in CHANGELOG.
