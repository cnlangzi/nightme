# F-25: Rolling-Log Receipt UX (Channel-Autonomous)

> **Status**: ✅ **v1.3 — re-implemented & Channel-owned**
>
> v1.x: rolling-log receipt card was a Gateway-driven FSM (per
> SPEC §1.2 v1.1 row "Receipt FSM owner: Gateway"). v1.2: brief
> gap — daemon sent plain `OutText` (no card). v1.3: revived, but
> with **the receipt OBJECT entirely Channel-internal**. Gateway
> knows nothing about receipts; each Channel picks its own state
> shape and storage form.
>
> **This doc is the canonical reference for the v1.3 rolling-log UX.**
> For the InputBuffer FSM (idle/busy, separate concern owned by
> ChatSession), see [`F-27-chatsession.md`](./F-27-chatsession.md) §5
> and [`internal/chatsession/input_buffer.go`](../../internal/chatsession/input_buffer.go).
>
> **Milestone**: v1.3 (Channel-autonomous)
> **Depends on**: F-08 (channel abstraction), F-24 (claudecode bridge), F-31 (message state)
> **Related**: [`SPEC.md`](../SPEC.md) v1.3 §2.2, §2.4; [`F-08-channel-abstraction.md`](./F-08-channel-abstraction.md); [`F-26-gateway-hub.md`](./F-26-gateway-hub.md)

---

## 1. Description

Each user message triggers an agent turn. The agent emits a stream
of events (`EventText`, `EventToolStart`, `EventToolEnd`, …) until
the turn ends with `EventDone` / `EventError`. The user should see
**one coherent visual artifact** for that turn — not a flurry of
separate messages.

The **rolling-log receipt** is that artifact:

- **Per-turn scope**: one receipt object per turn (anchored to the
  single `currentTurnUserMsgID` — buffered batch flushes anchor to
  the last userMsgID in the batch).
- **Channel-native rendering**: each Channel picks the form that
  fits its platform:
  - **Feishu**: an interactive card (Card 2.0) PATCHed in place via
    `UpdateMessage` — the user sees a single bot reply growing
    under their message
  - **Slack**: a thread reply (or thread root + reactions) under
    the user's message
  - **Web**: a DOM block with `data-receipt-for="<userMsgID>"`
- **Single consumer of `OutboundMessage`**: each `OutboundMessage`
  carries `ReplyTo = currentTurnUserMsgID`; Channel routes by that
  key to its own per-userMsgID receipt object.

**Gateway sees none of this**. Gateway stamps `ReplyTo` and sends;
Channel decides everything else (storage, lifecycle, terminal state,
card body formatting).

## 2. Design Principles

### 2.1 "Abstract stays abstract, concrete stays concrete"

Gateway's job for the outbound flow is now **purely mechanical**:

```
AgentEvent
  → gateway.Translate → OutboundMessage{Kind, Text, Meta, ReplyTo}
  → ch.Send(ctx, out)
```

That's it. Three lines of behavior. No receipt map, no FSM, no
fanout. Channel does the rest.

### 2.2 1 turn : 1 anchor, n events

A turn emits N events. Each event carries the same `ReplyTo =
currentTurnUserMsgID` (the single anchor). The Channel routes every
one of them to the same receipt object, accumulating content:

```
EventText "好的,让我..."        → PATCH card with "好的,让我..."
EventToolStart "Read(/a.py)"   → PATCH card with "🔧 Read(/a.py)"
EventToolEnd   "✓"              → PATCH card with "✓ Read"
EventText "...然后..."          → PATCH card with "...然后..."
EventResult   "📝 最终回复"      → PATCH card with final block
EventUsage    "1.2k tokens"     → PATCH card with footer
EventDone                       → (no PATCH; gateway handles MessageState separately)
```

The user sees **one card** that grew from "⏳ 等待中" to the final
content. No 50 messages in their chat.

### 2.3 Buffered batch → single anchor (last userMsgID)

If the user sends 3 messages while the previous turn is still in
flight, those messages are queued in InputBuffer (separate concern;
see F-27 §5). When the turn ends, `defaultFlushHookLocked` flushes
the batch and sets:

```go
cs.currentTurnUserMsgID = userMsgIDs[len(userMsgIDs)-1]
```

The agent sees the 3 messages as one combined input. All events
from that turn anchor to `userMsgID_last`. The receipt card lives
under the user's most-recent message — matching ChatGPT-style "all
3 submitted together, agent replies once under the last one" UX.

Earlier userMsgIDs in the batch keep their own `MessageState`
reactions (⏳ → 🔄, but no ✅) — terminal `MessageState(Done)` only
fires for the anchor.

## 3. Channel Implementation Contract

While each Channel can pick its own storage form, the
**observational contract** is uniform:

| Event | What Channel MUST do |
|-------|----------------------|
| First `OutboundMessage{ReplyTo: userMsgID, Kind: Out*}` for a userMsgID | Cold-create the receipt (card / thread / DOM node). Idempotent on retries. |
| Subsequent `OutboundMessage{ReplyTo: userMsgID, …}` | PATCH / update the existing receipt — append the event's content |
| `OutboundMessage{Kind: OutMessageState, Meta: {state}}` | AddReaction / DOM state / status emoji on the user's message |
| `OutboundMessage{Kind: OutText, ReplyTo: ""}` | Orphan: render as plain text (no anchor) |
| `OutboundMessage{Kind: OutCard}` | Send as an interactive card (permission prompts etc.) |

`OutThinking` and `OutTyping` are platform-dependent — Channels
may drop them silently (Feishu has no equivalent UX).

### 3.1 Feishu implementation reference

[`internal/channel/feishu/receipt.go`](../../internal/channel/feishu/receipt.go)
+ [`adapter.go`](../../internal/channel/feishu/adapter.go) §6:

- Receipt object: `*MessageReceipt` keyed by `userMsgID` in
  `a.receiptsByUserMsgID[userMsgID]` (NOT per-chat anymore)
- Cold-start path: `receiptFor(ctx, chatID, userMsgID)` — if no
  receipt for userMsgID, post a cold-start ⏳ card and create the
  receipt
- Patch path: `receipt.Append(ctx, AgentEvent)` — formats entry via
  `eventToEntry`, appends to entries slice, renders card body, calls
  `bot.PatchMessage(cardMsgID, body)`
- Card body budget: 30 KB cap (Feishu hard limit) → `evictOverflowLocked`
  drops oldest entries beyond the byte / count budget; card header
  shows "…(前 N 条已省略)"

### 3.2 Slack / Web / future

- Slack: implement a thread reply strategy. The first
  `OutboundMessage{ReplyTo: userMsgID}` posts the thread root;
  subsequent events post thread replies or edit the root via
  `chat.update`.
- Web: maintain a `Map<userMsgID, DOMElement>` keyed by
  `data-user-msg-id`. Append events as child nodes; patch parent
  on terminal state.

## 4. Failure Modes

| Failure | Channel behavior | Gateway behavior |
|---------|-----------------|------------------|
| `ch.Send` returns error | Log warn; keep receipt alive; next event retries | Continue draining pump (no retry) |
| Cold-start `SendCard` fails | Receipt is nil; subsequent events log warn + drop | Channel falls back to plain text (no rolling log UX) |
| `PatchMessage` rate-limited (Feishu 230020 / 429) | Coalesce / debounce events; resync on next successful PATCH | No retry; eventual convergence |
| Receipt body exceeds Feishu 30 KB cap | `evictOverflowLocked` drops oldest entries; header shows eviction count | — |

## 5. Rate-Limit Mitigation

High-throughput agents can emit dozens of events per second. Feishu's
`UpdateMessage` API has QPS limits (typically ~100/min per app). Two
strategies:

1. **Coalesce at the Channel layer**: buffer `OutText` /
   `OutToolStart` events for ~50ms windows, then PATCH once per
   window. Lossy but visually fine — a rolling log is meant to be
   "what's happened recently", not "every microsecond".
2. **Idempotent convergence**: each PATCH sends the full current
   card body. If a PATCH fails, the next successful one carries
   the latest state. Out-of-order PATCHes converge (Feishu accepts
   the last-write-wins model).

Channels SHOULD implement coalescing internally; Gateway stays
unaware.

## 6. MessageState Interaction

`MessageState` (F-31) is **independent** of rolling-log receipt:

- **MessageState** = progress indicator on the user's message
  (⏳ → 🔄 → ✅ / ❌). Owned by ChatSession.
- **Rolling-log receipt** = content artifact under the user's message.
  Owned by Channel.

Both are keyed by `userMsgID`. They are triggered by separate
events (`OutMessageState` vs `OutText` / `OutToolStart` / …). A
failure in one does not affect the other.

See [`F-31-message-state.md`](./F-31-message-state.md) for the
full MessageState lifecycle.

## 7. /kill / dispose semantics

`/kill` clears the ChatSession's AgentSession pool. The Channel's
receipt objects are NOT touched — they're Channel-private state.
Feishu receipts are simply never PATCHed again; they stay as the
last-rendered visual until the user / IM backend cleans them up
(typical IM: ~24h retention, then server-side GC).

If a Channel wants to actively clean up receipts on `/kill`, it
MAY subscribe to a future `/kill` event (not currently exposed;
out of scope for v1.3).

## 8. History

| Version | What changed |
|---------|--------------|
| v1.0 / v1.1 | Receipt FSM owned by Gateway (`internal/gateway.CreateReceipt / UpdateReceipt / DisposeReceipt`). Channel painted state transitions. Worked for Feishu; mismatched Slack / Web abstractions. |
| v1.2 | Receipt FSM temporarily disabled; daemon sent plain `OutText` (no card UX). Documented as "known gap" in CHANGELOG. |
| **v1.3** | **Receipt FSM removed from Gateway entirely.** Receipt is now a Channel-internal object keyed by `userMsgID`. Gateway stamps `OutboundMessage.ReplyTo = currentTurnUserMsgID`; each Channel routes + PATCHes its own receipt. "Abstract stays abstract, concrete stays concrete" principle applied throughout. |