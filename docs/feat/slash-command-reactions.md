# Slash command ⏳ → ✅ reactions

**Status**: shipped (F-53 §8 follow-up)

**Author**: F-XX

**Scope**: `internal/command/commander.go`, `internal/agent/message_state.go`,
`internal/channel/feishu/adapter.go`, removal of redundant per-command emit
in `internal/command/steer/cmd.go`.

---

## Context

F-53 ([F-53-message-prompt-lifecycle.md](F-53-message-prompt-lifecycle.md))
collapsed `agent.MessageState` from 4 values (`Received` / `Forwarded` /
`Done` / `Failed`) to 3 (`Queued` / `Submitted` / `Dropped`). The
deletion was driven by §3 原则 1: **Message reflects only the delivery
pipeline, not execution result.** The 4-value enum conflated "did the
bridge hand the bytes to the agent?" with "did the agent finish?"; F-53
split these into `MessageState` (delivery) and `chatsession.PromptState`
+ `Prompt.EndReason` (execution).

F-53 §8 explicitly logged the regression as **awaiting independent
follow-up**:

> 用户消息 reaction 序列由"⏳ → 🔄 → ✅"变为"⏳ → 🔄 →（不变）"。替代 UX
> （占位卡上展示终态？reaction 移到卡片？）由独立后续任务决定。

This document is the follow-up task. We restored the missing
user-message ✅ reaction for **synchronous dispatch paths** (slash
command, shell dispatch) where there is no Prompt lifecycle to carry
the "dispatcher finished" signal — a class of operations F-53's
Prompt-based design left uncovered.

## The gap

Before this change, only **regular messages** had a visible feedback
sequence on the user message:

```
inbound text
   ↓ Manager.HandleInbound
   ↓ cs.EmitMessageState(userMsgID, MessageQueued)        → ⏳
   ↓ cs.EmitMessageState(userMsgID, MessageSubmitted)      → 🔄
   ↓ (agent streams results)
   ↓
   (no further MessageState transition; ✅ comes from PromptEndBus on the receipt card, not the user message)
```

Slash commands had **nothing**:

```
inbound "/gtw commit"
   ↓ commander.Dispatch
   ↓ cmd.Handle → dispatchCommit → runAgentToCommit → RunOnce → ... (5-30s)
   ↓ (silent the whole time)
   ↓
   OutReply ← user finally sees the result
```

The user-perceived symptom: "I sent `/gtw commit` and nothing
happened for 15 seconds." The technical symptom: slash commands
never publish on `MessageStateBus`.

## The fix

Reintroduce `agent.MessageDone` (the F-53-deleted value) with a
narrow, explicitly-delivery-pipeline semantic, and have the
commander framework emit it automatically around every matched
slash command.

### What `MessageDone` means now

> The dispatcher has finished interacting with this user message;
> no further MessageState transitions will arrive for this user
> message id.

This is **orthogonal** to `chatsession.PromptDone`:

| Constant | Target | Emitted by | Semantics |
|---|---|---|---|
| `agent.MessageDone` | user message | commander framework after `cmd.Handle` returns | "the dispatcher is done with this user message" |
| `agentsession.PromptDone` | receipt card | runtime readpump via `PromptEndBus` | "the agent finished a turn on this prompt" |

Both render as ✅ on the Feishu side via `AddReaction("DONE", ...)`,
but to **different message ids** (the user's inbound text vs. the
bot's outbound receipt card). They are not redundant; they cover
the two distinct surfaces that nightme operates on.

### What `MessageDone` does NOT mean

- **It does NOT mean "success".** Success / failure is conveyed by
  the reply text's contents — the existing ❌ prefix convention for
  failures covers the error case. `MessageFailed` is intentionally
  still absent; it would re-introduce the conflation F-53 removed.
- **It does NOT mean "no more work will ever happen on this chat".**
  Subsequent user input or follow-up replies from the agent loop are
  unrelated. Each new user message gets its own MessageState
  sequence.

### Where the emission lives

`internal/command/commander.go` `commander.Dispatch`:

```go
emitSlashState(cs, input.MessageID, agent.MessageQueued)
out, err := cmd.Handle(ctx, rt, cs, input)
emitSlashState(cs, input.MessageID, agent.MessageDone)
```

The helper (`emitSlashState`) adds the nil-guard for `cs == nil`,
empty-MessageID, and `cs.MessageStateBus == nil`. The latter lets
unit tests construct bare `&chatsession.ChatSession{}` without a
fully-wired bus — production code always passes a session built
via `chatsession.New`, which wires the bus eagerly.

### What fall-through paths do

- `handled=false` (input wasn't a slash command at all): no emit.
  The text falls through to `tryMessageDispatch` which has its own
  `Manager.HandleInbound` MessageQueued path.
- `handled=true + Consumed=false` (slash command attempt but no
  factory matched, e.g. `/etc/passwd`): no emit. Unknown commands
  shouldn't get ⏳/✅ since they're not actually commands.

These two paths return *before* the `emitSlashState` calls, so the
contract falls out naturally without an explicit branch.

### Feishu rendering

`mapStateToFeishuEmoji` (internal/channel/feishu/adapter.go) gains
one new case:

```go
case agent.MessageDone:
    return "DONE" // ✅
```

This reuses the same `"DONE"` string as
`mapPromptStateToFeishuEmoji(agentsession.PromptDone)`. The runtime
routes the two events to different reaction targets, but the emoji
glyphs match — the user sees a consistent ✅ across surfaces.

The existing `case messages.OutMessageState` handler in feishu's
`Send` switch (line 1264 in `adapter.go`) needs no new branches:
`mapStateToFeishuEmoji` returning `"DONE"` flows through the same
LRU dedup + `AddReaction` + failure-revert pipeline that
`"OneSecond"` and `"OnIt"` already use.

### What `MessageDropped` does (unchanged)

`MessageDropped` is still not mapped in `mapStateToFeishuEmoji`
(returns `""`). It exists for delivery-side uses (e.g.
`ChatSession.emitMessageDropped` clearing queued messages) but
doesn't have a user-visible reaction.

---

## What slash command authors need to do

**Nothing.** The framework covers it. Future slash commands get
the ⏳ → ✅ pair automatically as soon as their factory is registered.

The previous "best practice" — manually calling
`cs.EmitMessageState(input.MessageID, agent.MessageQueued)` at
the start of `Handle` — is now redundant. The framework emits
before the call enters `Handle`, so a manual emit would be
dropped by feishu's LRU dedup. `/steer` had this exact pattern
(see git history of `internal/command/steer/cmd.go` pre-this-PR);
it's been removed with a pointer comment to this doc.

If a particular command needs a **mid-work** indicator beyond the
entry ⏳ (analogous to `MessageSubmitted` for regular messages), it
can still call `cs.EmitMessageState(input.MessageID, agent.MessageSubmitted)`
internally. The LRU dedup means this is additive — the framework's
Queued + Done pair won't double-fire.

---

## Tests

### Commander framework contract (internal/command/commander_test.go)

- `TestDispatch_EmitsQueuedThenDone` — happy path.
- `TestDispatch_EmitsDoneOnHandlerError` — error path still gets ✅.
- `TestDispatch_FallThroughEmitsNothing` — non-slash / unknown slash emit zero events.
- `TestDispatch_EmptyMessageIDSkipsEmit` — empty MessageID guard.
- `TestDispatch_NilCSSkipsEmit` — nil cs guard.
- `TestDispatch_ZeroValueCSDoesNotPanic` — `&chatsession.ChatSession{}` (no MessageStateBus) doesn't crash.
- `TestDispatch_QueuedBeforeHandleDoneAfter` — ordering: Queued is published before Handle, Done after.

### Mapping & semantics (internal/agent/message_state_test.go, internal/channel/feishu/adapter_message_state_test.go)

- `TestMessageState_String` — every value renders its label, including the new `MessageDone → "done"`.
- `TestMessageState_DistinctValues` — the 4 consts are distinct ints (catches future refactor regressions).
- `TestMapStateToFeishuEmoji` — Queued/Submitted/Done/Dropped map correctly.
- `TestMapPromptStateToFeishuEmoji` — orthogonal receipt-card mapping stays in lock-step.
- `TestMapStateAndPrompt_DoneUseSameEmoji` — the cross-surface invariant: user-msg ✅ and receipt-card ✅ use the same emoji string.

---

## Open considerations

- **Other channels (Slack, Web)**: their emoji mappings aren't yet
  wired. Adding a third (or fourth) value to `agent.MessageState` is
  backward-compatible — channels that don't know `MessageDone` will
  return `""` from their mapping, the same silent-drop behavior
  they had for the (now-deleted) MessageFailed. Future channel
  authors can adopt the value at their own pace.

- **Mid-work indicator (`MessageSubmitted`)**: not emitted by the
  framework. Slash commands that genuinely want a 🔄 mid-progress
  can still call `cs.EmitMessageState(input.MessageID,
  agent.MessageSubmitted)` from inside `Handle`. This is a per-
  command decision, not a framework concern.

- **Receipt cards for slash commands**: not introduced here. The
  framework's user-message ✅ covers the gap F-53 left for sync
  operations. If a future task wants slash commands to also create
  receipt cards (analogous to regular messages), that's a
  larger refactor — see F-44 receipt architecture
  (internal/channel/feishu/receipt.go) for the surface area.

---

## Shell `!cmd` path — same ⏳→✅ contract

Shell commands (`!ls`, `!make build`, etc.) share the user-message
⏳→✅ UX with slash commands. The implementation differs in two
ways:

1. **Async goroutine preserved**: shell commands often run 5-60s,
   so `Dispatcher.Handle` cannot synchronously wait for completion
   the way `commander.Dispatch` does. The goroutine outlives the
   inbound ctx — that's intentional (e.g. `!make restart` lets
   the shell command finish even after the inbound-ctx cancel).
2. **Reply posted async inside the goroutine**: shell replies
   don't flow through the runtime shim the way slash replies do.
   `ShellOutput` therefore doesn't carry a Reply field — the
   goroutine calls `emitter.Send(messages.OutboundMessage{Kind:
   OutCommandReply, ...})` directly.

### Framework-level emission in `Dispatcher.Handle`

```go
if _, matched := parseShell(ir.Text); !matched {
    return nil, false  // not a !cmd — fall through
}
emitShellState(cs, ir.MessageID, agent.MessageQueued)  // ⏳ synchronously, before goroutine
go d.runShell(cs, ir)                                  // work + reply + ✅ inside
return &ShellOutput{Consumed: true}, true
```

`runShell`'s first defer is `emitShellState(cs, ir.MessageID,
agent.MessageDone)`. Because defers are LIFO, it runs LAST — after
the inner panic-recovery defer. This guarantees ✅ fires on every
exit path:

- normal completion (reply sent successfully)
- command failed (exit non-zero, dispatch error)
- reply Send failed
- panic in the goroutine

### Why shell package now imports chatsession + outbound

Pre-refactor: `internal/shell.Sender` interface + `sender_chatsession.go`
bridge translated `shell.Outbound` → `messages.OutboundMessage` and
delegated to the per-chat outbound.Emitter. The bridge existed to
keep `internal/shell` from importing `internal/chatsession` /
`internal/gateway/outbound`.

The decoupling never paid off: the only call site was
`cmd/nightme/run.go:480` (the production wire-up) and two test
wirings. There was no third-party `internal/shell` consumer that
needed the package-import isolation.

Post-refactor: `Dispatcher` holds `outbound.Emitter` directly, no
bridge, and `Handle` accepts `cs *chatsession.ChatSession` as its
first parameter. Same shape as `commander.Dispatch` — both
packages now follow the convention `(ctx-or-implicit, cs,
input) → (*Output, bool)`. The shell package joins command in
walking the full chat-session lifecycle instead of skirting it.

### What fall-through paths do

- `parseShell` no-match → `return nil, false`, no goroutine, no
  emit. Identical contract to `commander.Dispatch`'s no-match
  branch: don't flash ⏳ on inputs that aren't actually commands.

### Tests

`internal/shell/dispatch_test.go` — new tests mirror the commander
set:

- `TestDispatcherHandle_EmitsQueuedThenDone` — happy path,
  ⏳ → ✅ sequence.
- `TestDispatcherHandle_FallThroughEmitsNothing` — non-!cmd must
  not emit any MessageState.
- `TestDispatcherHandle_NilMessageIDSkipsEmit` — empty MessageID
  guard.
- `TestDispatcherHandle_NilCSSkipsEmit` — nil cs guard.
- `TestDispatcherHandle_ZeroValueCSDoesNotPanic` — bare
  `&chatsession.ChatSession{}` (nil MessageStateBus) doesn't
  crash; the nil-bus guard in `emitShellState` keeps the contract
  local and test-friendly.
- `TestDispatcherHandle_NilEmitter` — `NewDispatcher(nil)`
  doesn't panic; reply is silently dropped.
- `TestDispatcherHandle_ShellCommand` — happy path: `!echo hello`
  produces a summary card with `OutCommandReply` kind and the
  correct `ChatID` / `ReplyTo`.

---

## Migration checklist (for someone reading this in the future)

1. ✅ `agent.MessageDone` constant + String case — `internal/agent/message_state.go`
2. ✅ Feishu `mapStateToFeishuEmoji` mapping — `internal/channel/feishu/adapter.go`
3. ✅ Commander framework wrap — `internal/command/commander.go`
4. ✅ Remove manual emit in `/steer` — `internal/command/steer/cmd.go`
5. ✅ Tests — `internal/command/commander_test.go`, `internal/agent/message_state_test.go`, `internal/channel/feishu/adapter_message_state_test.go`
6. ✅ Update F-53 §8 row — see [F-53-message-prompt-lifecycle.md](F-53-message-prompt-lifecycle.md) follow-up note
7. ✅ Shell ⏳→✅ (F-XX Sender→Emitter refactor) — `internal/shell/dispatch.go` (drop `Sender`/`Outbound`/bridge, take `outbound.Emitter` + `cs` directly), `internal/shell/sender_chatsession.go` deleted, `internal/gateway/inbound/shell.go` + `inbound.go` updated to new shape, `internal/gateway/inbound/teststubs/teststubs.go` + `internal/gateway/dispatch_slash_reply_test.go` stubs updated, `cmd/nightme/run.go:480` wires `mgr.Emitter()`.