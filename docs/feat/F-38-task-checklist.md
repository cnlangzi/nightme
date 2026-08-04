# F-38: Claude Task Checklist in Feishu Receipt

> **Status**: ✅ 已实现 · docs-first locked (2026-08-04)
> **Milestone**: v1.3.x
> **Depends on**: F-24 (Claude Code bridge), F-25 (Channel-owned receipt), F-37 (tool thread routing), SPEC §1.4 (typed boundary)
> **Related**: [`SPEC.md`](../SPEC.md) §0.6 / §1.3 / §2.2; [`channel/feishu.md`](../channel/feishu.md) §13.14 / §18

---

## 1. Motivation

Claude Code exposes `TaskCreate` and `TaskUpdate` tools for maintaining a task list. In `--output-format stream-json` they are **not dedicated top-level event types**. They arrive as normal `tool_use` blocks in an assistant message and are followed by a matching `tool_result` block in a user message.

nightme currently translates both tools through the generic tool path:

```text
assistant.tool_use  → EventToolStart → OutToolStart → Feishu thread `● TaskCreate(...)`
user.tool_result    → EventToolEnd   → OutToolEnd   → Feishu thread `⎿ ...`
```

This preserves protocol visibility but loses the product-level concept: users need a compact task checklist in the receipt card, not two low-level thread lines per task mutation.

F-38 adds a generic typed task concept without moving Claude-specific names into Gateway or Channel.

## 2. Observed Claude Code contract

The stream-json schema is not officially documented as a stable wire contract. The following shape is observed in Claude Code 2.1.220 and must be locked by fixtures.

### 2.1 TaskCreate

```json
{
  "type": "assistant",
  "message": {
    "role": "assistant",
    "content": [{
      "type": "tool_use",
      "id": "toolu_create_1",
      "name": "TaskCreate",
      "input": {
        "subject": "Implement task checklist",
        "description": "Render Claude tasks in the Feishu receipt",
        "activeForm": "Implementing task checklist",
        "metadata": {}
      }
    }]
  }
}
```

The assigned task ID is not present in the input. It arrives in the successful result text:

```text
Task #1 created successfully: Implement task checklist
```

### 2.2 TaskUpdate

```json
{
  "type": "assistant",
  "message": {
    "role": "assistant",
    "content": [{
      "type": "tool_use",
      "id": "toolu_update_1",
      "name": "TaskUpdate",
      "input": {
        "taskId": "1",
        "status": "in_progress"
      }
    }]
  }
}
```

TaskUpdate may also carry `subject`, `description`, `activeForm`, `owner`, dependency and metadata fields. A normal success result starts with `Updated task #<id>`; deletion may use a dedicated success phrase. A result with `is_error=true`, a known failure phrase, or an unrecognised shape must not mutate task state.

## 3. Locked decisions

### D1 — emit only after a successful tool_result

The bridge records the pending operation at `tool_use` time but emits no task event yet. It mutates task state only after the matching `tool_result` confirms success.

Reasons:

- TaskCreate has no stable ID until its result.
- TaskUpdate can fail or be vetoed.
- Optimistic UI would display state that never existed.

### D2 — use the provider-assigned task ID

The TaskCreate result is parsed for the real ID. Subject/content hashes are forbidden: duplicate subjects collide and cannot correlate with later numeric/string `taskId` updates.

### D3 — bridge owns normalized provider-session task state

The Claude bridge keeps:

```text
tasks:     taskID → generic TaskItem
taskOrder: stable creation order
```

After every confirmed create/update/delete it emits the **complete current snapshot**. This state is process/session-local and is not added to ChatSession or Registry.

### D4 — every task event carries a full snapshot

`TaskUpdate` is a delta, but a new receipt may not have seen the original TaskCreate. Full snapshots make each outbound event self-contained and keep Gateway stateless.

```go
type TaskStatus int

const (
    TaskPending TaskStatus = iota
    TaskInProgress
    TaskCompleted
)

type TaskItem struct {
    ID         string
    Subject    string
    ActiveForm string
    Status     TaskStatus
}

type TaskListEvent struct {
    Items []TaskItem
}
```

Both `EventTaskCreate` and `EventTaskUpdate` carry `*TaskListEvent`.

### D5 — typed Gateway contract

Gateway adds `OutTaskCreate` / `OutTaskUpdate` and `OutboundMessage.TaskList *agent.TaskListEvent`. It does not parse Claude input, store task state, format glyphs, or know receipt layout.

This is not a concrete-schema leak: task ID, subject and pending/in-progress/completed are generic agent concepts. The Claude field names `taskId` and `activeForm` stay inside `bridge/claudecode`.

### D6 — successful task tools do not also become thread replies

On a confirmed task operation the bridge emits the task event only. It suppresses the generic ToolStart/ToolEnd pair, avoiding duplicate UI. On parse failure or protocol drift it logs a warning and degrades to a generic ToolEnd so the operation is still visible.

### D7 — Feishu renders one dedicated checklist element

The receipt stores only the latest copied snapshot. Tasks are not `LogEntry` history and are not individually evicted.

Card order:

```text
header
answer/result entries
one task-checklist markdown element
footer divider + footer
```

Keeping the answer first preserves the F-thread-route decision that the final response is the receipt's primary content.

## 4. End-to-end flow

```text
Claude assistant.tool_use(TaskCreate/TaskUpdate)
  → bridge caches pending operation by tool_use_id
Claude user.tool_result
  → correlate pending operation
  → verify success
  → update bridge task map/order
  → emit EventTaskCreate or EventTaskUpdate with full snapshot
ChatSession readPump
  → current turn userMsgID
Gateway.Translate
  → OutTaskCreate / OutTaskUpdate + typed TaskList
runtime EventHandler
  → out.ReplyTo = currentTurnUserMsgID
Feishu Adapter.Send
  → receiptFor(chatID, ReplyTo)
  → receipt.SetTaskList(snapshot)
  → PATCH the existing receipt card
```

No Channel interface, ChatSession, binding, registry, or receipt map API changes are required.

## 5. Feishu checklist UX

### 5.1 Status mapping

| Generic status | Feishu rendering |
|---|---|
| pending | `⏳ Subject` |
| in progress | `🔄 Subject · ActiveForm` (suffix only when present) |
| completed | `✅ Subject` |

Display order is in-progress, pending, completed; order within each group follows bridge task order.

### 5.2 Capacity

The entire checklist is one markdown card element. It must fit within `divTextCharLimit` and the receipt's existing 24KB defensive body budget.

When content exceeds the checklist budget:

1. keep in-progress tasks;
2. keep pending tasks;
3. include completed tasks only while space remains;
4. append `…另有 N 项任务`.

The card element calculation must reserve one element when a checklist is present and keep `body.elements <= 50`.

### 5.3 Idempotency

- Bridge upserts by real task ID and emits deterministic snapshots.
- Receipt replaces its copied snapshot wholesale.
- Identical snapshots produce identical card JSON.
- Existing `renderLocked` body diff skips duplicate PATCH calls.

## 6. Failure and compatibility behavior

| Scenario | Behavior |
|---|---|
| `tool_result.is_error=true` | Do not mutate tasks; degrade to generic tool result visibility. |
| Unknown success text / protocol drift | Warn with tool name/use ID; do not guess an ID or status; generic ToolEnd fallback. |
| Update for an unknown task ID | Create a placeholder subject `Task #<id>` and apply the confirmed status; later hydration may replace it. |
| Delete success | Remove the task from map/order and emit an update snapshot; an empty snapshot clears the checklist. |
| Duplicate result | Pending operation has already been removed; ignore/log rather than applying twice. |
| Late task event after receipt completed | Drop, matching existing late-event semantics. |
| Daemon restart / resumed external task list | Bridge state starts empty. TaskList hydration is follow-up unless a stable fixture-backed parser is available. |

## 7. Implementation scope

### Agent / Gateway

- `internal/agent/agent.go`: task types, event kinds and payload.
- `internal/gateway/messages.go`: outbound kinds and typed payload.
- `internal/gateway/translate.go`: pure mappings.

### Claude bridge

- `internal/bridge/claudecode/stream.go`: pending tool correlation and result dispatch.
- `internal/bridge/claudecode/task.go`: Claude-native parsing, success confirmation, task state and snapshots.

### Feishu

- `internal/channel/feishu/adapter.go`: outbound routing and card insertion.
- `internal/channel/feishu/receipt.go`: latest task snapshot and setter.
- `internal/channel/feishu/receipt_task.go`: bounded checklist renderer.

## 8. Test plan

### Bridge

- tool_use does not emit an optimistic task event;
- create success extracts real ID;
- failed/unrecognised result leaves state unchanged and falls back;
- multiple creates accumulate;
- update changes status/subject/activeForm;
- delete removes task;
- out-of-order results correlate by tool_use_id;
- pending records are removed;
- task success emits no generic task thread events.

### Gateway

- both task kinds map correctly;
- nil payload drops;
- empty update snapshot is preserved for clear semantics.

### Feishu

- cold-create and PATCH reuse the same receipt;
- status glyphs, ordering and ActiveForm are correct;
- checklist is after answer and before footer;
- duplicate snapshot skips PATCH;
- large checklist shows omitted count;
- total card elements never exceed 50;
- task events never call thread reply.

### E2E smoke

In a Feishu group, ask Claude to create and complete several tasks. Verify one main receipt card is PATCHed, task tool lines do not appear in the thread, and the final answer stays above the checklist.

## 9. Out of scope / follow-up

- `TaskList` hydration for resumed/cross-session task lists;
- legacy `TodoWrite` normalization;
- ACP / Pi / PTY task primitives;
- clickable checklist rows or user-driven task mutation;
- task persistence in nightme Registry;
- cross-turn updates to old receipt cards.

## 10. Change log

- **2026-08-04** — docs-first design locked: confirmed-result semantics, real task IDs, bridge-owned full snapshots, typed Gateway contract, one-element Feishu checklist.
- **2026-08-04** — code implementation landed: `agent.TaskStatus/TaskItem/TaskListEvent`, `EventTaskCreate/EventTaskUpdate`, `OutTaskCreate/OutTaskUpdate` + `OutboundMessage.TaskList`, claudecode bridge pending correlation + success-only emit, Feishu receipt `SetTaskList` + single-element checklist render. `go test -race ./...` 全绿。
