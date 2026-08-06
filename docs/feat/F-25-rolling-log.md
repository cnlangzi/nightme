# F-25: Rolling-Log Receipt UX (Channel-Autonomous)

> **Status**: ✅ **v1.3 — re-implemented & Channel-owned** · ⚠️ **v1.3.x F-thread-route 收窄 scope** (2026-08-04)
>
> v1.x: rolling-log receipt card was a Gateway-driven FSM (per
> SPEC §1.2 v1.1 row "Receipt FSM owner: Gateway"). v1.2: brief
> gap — daemon sent plain `OutText` (no card). v1.3: revived, but
> with **the receipt OBJECT entirely Channel-internal**. Gateway
> knows nothing about receipts; each Channel picks its own state
> shape and storage form.
>
> **v1.3.x F-thread-route 收窄 scope**(2026-08-04):rolling-log receipt card 收窄到只承载 `OutText` / `OutResult` / `OutInit` / `OutUsage` 派生的 entry。`OutThinking` / `OutToolStart` / `OutToolEnd` **不再进 receipt**,由 Feishu adapter 路由到 user message 的 thread reply(详见 [`F-37-tool-thread-routing.md`](./F-37-tool-thread-routing.md) + [`channel/feishu.md` §13.12](../channel/feishu.md))。Receipt card body 元素数从 ~30 降到 ≤5,Feishu 50 element 上限不再是个问题。
>
> **v1.3.x F-49 行为变更**(2026-08-06):`OutCompaction` kind 整条 path 删除(详见 [`F-49-compaction-counter.md`](./F-49-compaction-counter.md) + [`SPEC §0.14`](../SPEC.md) + [`channel/feishu.md` §13.25](../channel/feishu.md))。Runtime handler 不再产生 `OutboundMessage{Kind: OutCompaction}`,Feishu adapter 不再有 `Send` case `OutCompaction`,receipt `eventToEntry` / `Append` 不再有 EventCompaction 分支。本文档关于 OutCompaction 的描述(§2.4 silent PATCH、§3.1.1 thread reply 行)全部作废;`F-25 §3.1.1` 列表删除 `OutCompaction → postThreadReply(... body)` 一行。
>
> **This doc is the canonical reference for the v1.3 rolling-log UX.**
> For the InputBuffer FSM (idle/busy, separate concern owned by
> ChatSession), see [`F-27-chatsession.md`](./F-27-chatsession.md) §5
> and [`internal/chatsession/input_buffer.go`](../../internal/chatsession/input_buffer.go).
>
> **Milestone**: v1.3 (Channel-autonomous) · v1.3.x (F-thread-route 收窄)
> **Depends on**: F-08 (channel abstraction), F-24 (claudecode bridge), F-31 (message state), F-37 (tool thread routing) + F-37 multi-div content split
> **Related**: [`SPEC.md`](../SPEC.md) v1.3 §2.2, §2.4 + §0.3 F-thread-route; [`F-08-channel-abstraction.md`](./F-08-channel-abstraction.md); [`F-26-gateway-hub.md`](./F-26-gateway-hub.md); [`F-37-tool-thread-routing.md`](./F-37-tool-thread-routing.md); [`F-37-multi-div-content-split.md`](./F-37-multi-div-content-split.md) — F-37 解决 `OutResult` 600 B 截断 backlog

---

## 1. Description

Each user message triggers an agent turn. The agent emits a stream
of events (`EventText`, `EventToolStart`, `EventToolEnd`, …) until
the turn ends with `EventDone` / `EventError`. The user should see
**one coherent visual artifact** for that turn — not a flurry of
separate messages.

**v1.3.x F-thread-route 收窄**:Receipt card 不再承载 agent turn 的全部 event,只承载**最终答复相关的 entry**(`OutText` / `OutResult` / `OutInit` / `OutUsage`)。中间过程(`OutThinking` / `OutToolStart` / `OutToolEnd`)由 Channel 路由到独立 thread reply / 折叠 section / DOM 子节点(Feishu 选择 thread reply + 类型感知摘要,详见 [`F-37-tool-thread-routing.md`](./F-37-tool-thread-routing.md))。Receipt card body 元素数从 ~30 降到 ≤5,Feishu 50 element 上限不再是个问题。

The **rolling-log receipt** is that artifact (v1.3.x scope):

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

> **决策卡 vs Receipt 卡**：`OutCard` 走 F-46 决策卡（gtw §5.3.1 / §5.3.3 确认分支）路径——bot 发独立 card message，用户点 button → reaction pipeline → 原卡 PATCH。`OutCard` 也用于 receipt（如 `OutCard` 类型 receipt checklist）。两者**不**冲突：决策卡的 PATCH 改 `Disabled+ChosenChoiceEmoji`；receipt 的 PATCH 改 entry list。详见 [`F-46-interactive-cards.md`](./F-46-interactive-cards.md) §2.6。

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
one of them, but **routing target depends on Kind** (v1.3.x F-thread-route):

```
EventText "好的,让我..."        → PATCH card with "好的,让我..."
EventToolStart "Read(/a.py)"   → thread reply "🔧 Read(/a.py)"
EventToolEnd   "..."           → thread reply "✅ Read /a.py → 1234 lines" (类型感知摘要)
EventText "...然后..."          → PATCH card with "...然后..."
EventResult   "📝 最终回复"      → PATCH card with final block
EventUsage    "1.2k tokens"     → PATCH card with footer    [F-44/F-45 后: silent drop;footer 改走 SessionContext typed field → 4 个 main-chat Kind 各自的末尾]
EventDone                       → (no PATCH; gateway handles MessageState separately)
```

The user sees **one concise card** (final answer + metadata) under
their message, with a "X replies" thread indicator. Click the
indicator → see 💭🔧✅ flow in the thread. No 30-element card
visual clutter.

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
| First `OutboundMessage{ReplyTo: userMsgID, Kind: OutText\|OutResult\|OutInit\|OutUsage}` for a userMsgID | Cold-create the receipt (card / thread / DOM node). Idempotent on retries. |
| Subsequent `OutboundMessage{ReplyTo: userMsgID, Kind: OutText\|OutResult\|OutInit\|OutUsage}` | PATCH / update the existing receipt — append the event's content |
| `OutboundMessage{Kind: OutMessageState, Meta: {state}}` | AddReaction / DOM state / status emoji on the user's message |
| `OutboundMessage{Kind: OutText, ReplyTo: ""}` | Orphan: render as plain text (no anchor) |
| `OutboundMessage{Kind: OutCard}` | Send as an interactive card (permission prompts etc.) — thread reply if ReplyTo set |
| `OutboundMessage{Kind: OutCardPatch}` | **F-46 增量**: 原地 PATCH 已有交互卡（Feishu `PATCH /im/v1/messages/{id}`），用 `ReplyTo`=bot card msg id。`buildCardButtons` 在 `Disabled+ChosenChoiceEmoji` 时把选中按钮染绿 (`type: "success"` + `✓` 前缀)，没选按钮灰描边 disabled。详见 [`F-46-interactive-cards.md`](./F-46-interactive-cards.md) §10.2.3 |
| `OutboundMessage{Kind: OutThinking\|OutToolStart\|OutToolEnd}` | **v1.3.x F-thread-route**: Channel-specific routing. Feishu: post as plain text thread reply (rootID = msg.ReplyTo). Other Channels: pick their own routing (fold into receipt / separate message / drop). See [`F-37-tool-thread-routing.md`](./F-37-tool-thread-routing.md) §2.1. |
| ~~`OutboundMessage{Kind: OutCompaction}`~~ | ~~**v1.3.x F-thread-route**: Feishu: `postThreadReply(... "✶ Compacting conversation…")`~~ | **F-49 删除**：`OutCompaction` kind 整条 path 删除(runtime 不再产生此 Outbound)。详见 [`F-49 §1.9`](./F-49-compaction-counter.md)。|

> **v1.3.x 变更说明**: 折叠方案被 §13.12 反转,`OutThinking` / `OutToolStart` / `OutToolEnd` 不再 fold 进 receipt(走 thread + 类型感知摘要)。Receipt card scope 收窄到 OutText / OutResult / OutInit / OutUsage。

`OutTyping` 是 platform-dependent — Channels may drop it silently
(Feishu has no equivalent UX).

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
- **v1.3.x F-thread-route**: receipt card body 只承载 OutText / OutResult /
  OutInit / OutUsage 派生的 entry;thinking/tool 走 §3.1.1 thread
  reply path(不进入 receipt)。Receipt card body 元素数从 ~30 降到 ≤5,
  Feishu 50 element 上限不再是个问题。

### 3.1.1 Thread reply path (Feishu v1.3.x F-thread-route)

[`internal/channel/feishu/adapter.go`](../../internal/channel/feishu/adapter.go) §6 `Send` dispatcher 按 Kind 分流:

- `OutThinking` → `postThreadReply(ctx, chatID, rootID=userMsgID, body)` (plain text)
- `OutToolStart` → `postThreadReply(... body)` (plain text, args inline)
- `OutToolEnd` → 经 `summarizeToolEnd(name, args, output, err)` 生成单行摘要 → `postThreadReply(... body)`
- ~~`OutCompaction` → `postThreadReply(... body)` (✶ Compacting...)~~ → **F-49 删除**:`OutCompaction` kind 整条 path 删除。runtime handler 在 EventCompaction 上只调 `s.RecordCompaction()` 累加计数,不产生 Outbound;channel 不发任何"压缩进行中"marker。详见 [`F-49 §1.9`](./F-49-compaction-counter.md)。

底层走 `SendMessageText(ctx, chatID, text, rootID)` → `sendContent` → `sendViaLarkReply`(POST `/im/v1/messages/{rootID}/reply`,§13.10 已落地)。

**OutToolEnd 类型感知摘要**("decision 处理"):按 tool name 分支生成单行(不 dump 原始 output 到 thread),详见 [`F-37-tool-thread-routing.md` §2.3](./F-37-tool-thread-routing.md)。

### 3.2 Slack / Web / future

- Slack: implement a thread reply strategy. The first
  `OutboundMessage{ReplyTo: userMsgID}` posts the thread root;
  subsequent events post thread replies or edit the root via
  `chat.update`.
  - **v1.3.x**: thinking/tool 决策自决 —— 可走 Slack Block Kit
    的折叠 section、可走独立 thread reply、可 drop。**不**复制 Feishu 的 emoji 摘要
- Web: maintain a `Map<userMsgID, DOMElement>` keyed by
  `data-user-msg-id`. Append events as child nodes; patch parent
  on terminal state.
  - **v1.3.x**: thinking/tool 决策自决 —— DOM 子节点 + 折叠、可独立子面板、可 drop

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
| **v1.3.x (F-thread-route)** | **Receipt scope 收窄**: receipt card body 不再承载全部 event,只承载 OutText / OutResult / OutInit / OutUsage 派生的 entry。OutThinking / OutToolStart / OutToolEnd 路由到 Channel 自治的 thread reply(Feishu 选 thread + 类型感知摘要)。~~OutCompaction 同理走 thread reply;F-49 删除该 path——runtime 不再产生 OutCompaction,count 由 `SessionContext.CompactionCount` 携带,footer Line 1 渲染 `🗜 N`。~~ Card body 元素数从 ~30 降到 ≤5。详见 [`F-37-tool-thread-routing.md`](./F-37-tool-thread-routing.md) + [`channel/feishu.md` §13.12](../channel/feishu.md) + [`channel/feishu.md` §13.25](../channel/feishu.md) (F-49)。 |