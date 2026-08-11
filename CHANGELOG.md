# Changelog

All notable changes to nightme are documented here. This project
locks a **single development version** on `main` — there is no
versioned release ladder, no semver tags, no per-version branches.
The current development snapshot lives at HEAD of `main`; whatever
is committed there is the version users build and run.

> **Migration**: see [`MIGRATION.md`](./MIGRATION.md) for breaking
> changes between earlier snapshots.

## [Unreleased] — current dev (locked 2026-08-02)

### `outbound`: rename SessionContext / Stamper to StatusBar / StatusBarSource (F-58)

The per-message metadata envelope attached to every outbound
message is renamed end-to-end to align with its user-facing
mental model. The rename touches the type, the field, the
function, and every comment / doc.

**Terminology:**

| Old | New |
| --- | --- |
| `SessionContext` (flat struct) | `StatusBar` (sub-bar struct) |
| `OutboundMessage.SessionContext` | `OutboundMessage.StatusBar` |
| `outbound.Stamper` (function type) | `outbound.StatusBarSource` |
| `Options.Stamper` | `Options.Source` |
| `stampIfNeeded` | `attachStatusBarIfMissing` |
| `sessionContextInto` (pre-fill) | `stampFromAS` (same role, same name style) |
| `newRuntimeStamper` | `newRuntimeStatusBar` |
| `buildSessionContext` | `buildStatusBar` |
| `formatSessionFooterLines` / `formatSessionFooter` / `formatPRSegment` | `formatStatusBarLines` / `formatStatusBar` / `formatPRSegment` (param changed from `*SessionContext` to `*GitStatusBar` for `formatPRSegment` only) |

**Sub-bar structure (the semantic fix that came with the rename):**

`StatusBar` is no longer a flat struct of `Agent` / `Model` /
`Workspace` / `GitStatus` / `Usage` / etc. fields. It groups
into three sub-bars that encode the "show what exists, hide
what doesn't" rule structurally:

- `GitBar *GitStatusBar` — workspace + git + PR context.
  **Always populated** when the chat has a workspace
  (`cs.SelectedCwd != ""`), even if no `AgentSession` is
  selected. This is the "git status is always there" rule —
  every outbound message that flows through the runtime carries
  its worktree, so slash-command replies in a chat that hasn't
  spawned an AS yet, and pre-spawn `MessageQueued` placeholder
  cards, all surface the workspace line. PullRequest is nil
  when no AS is selected (PR lookup is per-AS).
- `AgentBar *AgentStatusBar` — agent identity (`Agent` /
  `Model` / `SessionID`). Populated only when a chat has a
  selected AgentSession — without an AS there's no agent
  identity to surface (the "AgentBar / TokensBar 没有 AS 则
  忽略" rule).
- `UsageBar *UsageStatusBar` — per-turn usage (in/out tokens,
  context window %, cost). Populated only when the bridge event
  carries Usage (typically `OutResult` / `EventAgentResult`).
  Streaming `OutReply` chunks without usage omit this entirely.

**Stamper → GitBar fallback (the behaviour change that came
with the rename):**

Pre-rename, `newRuntimeStamper` returned `nil` when the chat had
no selected `AgentSession`, even if the chat had a workspace.
That meant slash-command replies in a pre-spawn chat, and
`MessageQueued` placeholder cards, all rendered with no footer
at all. Post-rename, the source produces a `StatusBar` with
only `GitBar` populated when no AS exists — the user always
sees what worktree they're talking about.

**Why "StatusBar":**

- The data is semantically *not* just a session context:
  Workspace / GitStatus are workspace fields, Usage is a
  per-turn field, only Agent/Model/SessionID are session fields.
- "Context" collided with Go's `context.Context` at every read
  site.
- The new name matches the user-facing mental model: it's a
  status bar at the bottom of every outbound message, not a
  free-floating context bag.

### `gtw`: `/gtw push` + `/gtw pr` unified Readiness gate (F-57)

Both commands now share a single `Readiness` snapshot (one
`git status --porcelain --branch --untracked-files=normal` call),
parsed into a `GitStatusSnapshot` with two new fields:
`BehindRemote` (counts upstream commits the local branch is
behind) and `HasConflicts` (true when the porcelain scan finds
unmerged paths). `CollectStatus` is renamed `CollectReadiness`
to reflect the new role.

The three orthogonal atomic predicates on the snapshot
(`HasUpstreamBranch`, `LocalIsAtUpstreamTip`, `WorkingTreeIsClean`)
compose differently per command:

- `/gtw push` uses `HasNothingToPush()` + `PushBlockReason()`.
  Only unresolved conflicts are hard-refused; "no upstream" is
  the legitimate first-push case and falls through to
  `programmaticPush` (which now always runs `git push -u origin <branch>`).
- `/gtw pr` uses `IsReadyForPR()` + `PRBlockReason()`. A single
  priority-ordered message covers all six block dimensions
  (detached / conflicts / no upstream / ahead / behind / dirty /
  untracked) instead of the prior nested if-ladder.

**Bug fix** that triggered this design: `/gtw pr` previously
reused `countUnpushed` — whose "no upstream configured = 0"
semantic is correct for `/gtw push` but wrong for `/gtw pr`.
A local branch that had never been pushed slipped through the
gate and reached `gh pr create`, which rejected with
`Head ref must be a branch`. The new gate's
`!HasUpstream` branch catches this explicitly and points the
user at `/gtw push first to publish the branch to origin`.

Continuity property: a successful `/gtw push` exit guarantees
`/gtw pr` readiness passes (modulo external race) — see
`docs/feat/F-57-gtw-push-pr-readiness.md` §5 for the formal
proof.

**Field renames / API changes**

- `internal/messages.GitStatusSnapshot`: added `BehindRemote`,
  `HasConflicts`.
- `internal/command/gtw.CollectStatus` → `CollectReadiness`.
  Callers (`cmd/nightme/run.go`, footer render) updated.
- `internal/command/gtw.dispatchPR` entry no longer calls
  `countUnpushed`. The legacy `countUnpushed` helper stays
  in `programmaticPushWithRetry` for push-side verify only.
- `internal/command/gtw.dispatchPush` entry no longer calls
  `countUnpushed` either; `isClean` string-compare on
  `git status --porcelain` is gone; the conflict pre-check
  (previously `detectConflicts(statusOut)`) is folded into
  `PushBlockReason`.

**Tests**

- `internal/command/gtw/readiness_test.go` (new): shared
  `setupReadiness` + `porcelainFromSnapshot` fixtures, used by
  both pr and push test files.
- 7 new `/gtw pr` dimension tests (`TestDispatchPR_DetachedHead`,
  `_NoUpstream`, `_AheadOfUpstream`, `_BehindUpstream`,
  `_Uncommitted`, `_Untracked`, `_HasConflicts`, `_ReadyNothingNew`).
- 4 new `/gtw push` matrix tests (`TestDispatchPush_HardRefuse_Conflicts`,
  `_NothingToPush`, `_NoUpstreamFreshBranch`, `_AgentIntroducedConflicts`).
- `TestDispatchPR_NothingToPR_UncommittedHints` deleted: the
  pre-F-57 nested "uncommitted hint inside the nothing-to-PR
  branch" code path no longer exists; readiness gate catches
  it first.

**Docs**

- `docs/feat/F-57-gtw-push-pr-readiness.md`: full design +
  continuity proof + test matrix + migration steps.
- `internal/command/gtw/README.md`: no change (emoji + IM
  card conventions unchanged).

### `gtw`: split `/gtw push` into `/gtw commit` + `/gtw push` (F-XX)

The combined `/gtw push` (which both committed via a one-shot
agent and then pushed to origin) is split into two
single-purpose subcommands:

- **`/gtw commit [-a <agent>]`** — owns the agent path:
  readiness gate → headBefore capture → one-shot agent → verify
  HEAD-advance + worktree-clean + branch → re-snapshot (catch
  agent-introduced conflicts) → commit card. Refuses a clean
  worktree ("ℹ️ nothing to commit"), conflicts ("unmerged paths"),
  detached HEAD, and an unavailable agent.
- **`/gtw push`** — now push-only. Refuses a dirty worktree
  ("❌ worktree is dirty — /gtw push no longer auto-commits, run
  `/gtw commit` first, then `/gtw push`"). Continues to be Branch 1
  (no-op) + Branch 3 (programmatic push with retry verify);
  Branch 2 ("agent + push") is gone — that agent path moved
  into `/gtw commit`.

The Commit path's success card switches from
`🤖 <agent> committed N change(s) and pushed to <branch>`
(pre-F-XX) to `🤖 <agent> committed N change(s) on <branch>` —
"pushed" is now a separate step. The Push card stays
`✅ pushed N commit(s) to <branch>`.

**Field renames / API changes**

- `~/.nightme/gtw.yml`: new `commit:` section (mirror of
  `push:`) carrying `agent` + `hooks.before` / `hooks.after`.
  `push.agent` is now ignored when present (parser keeps it
  for schema back-compat with users' muscle memory). `push:`
  itself keeps `hooks.before` / `hooks.after` (called around
  the push-only flow).
- `internal/command/gtw.commit_push.go`: renamed flow —
  `dispatchPush` keeps Branches 1+3, drops Branch 2 +
  post-agent re-snapshot. `RunOnceTimeout` (5 min) stays in
  this file because `/gtw pr` also uses it.
- `internal/command/gtw/commit.go` (new): `dispatchCommit`,
  `runAgentToCommit`, `verifyAgentCommitted`, `buildAgentPrompt`.
- `internal/command/gtw/cmd.go::Factory.Handle`: routes
  `"commit"` alongside `fix / close / push / pr / sync`.
  `runCommit` mirrors `runPush`.
- `internal/command/gtw/render.go::replySuccessCard` splits
  into `replyCommitSuccessCard` + `replyPushSuccessCard`.
- `internal/command/gtw/commit_push.go` header comment updated
  to describe the push-only shape.

**Tests**

- `commit_push_test.go` (1514 lines) splits into
  `commit_test.go` (commit-path coverage) + `push_test.go`
  (push-only coverage + shared helpers).
- New push test `TestRunPush_DirtyRefused` covers the
  "commit first" hint.
- New commit test `TestRunCommit_HappyPath` covers the full
  agent-commit + verify + re-snapshot + commit-card chain.

**Docs**

- `README.md` + `README.zh-CN.md` Usage lines split
  `/gtw push` into `/gtw commit` + `/gtw push`.
- `gtw` Spec / Usage + emoji vocabulary unchanged
  (`🤖 committed-on` / `✅ pushed-to` are existing
  headings of the same row count).

### Breaking: `/kill` renamed to `/close`, and `/close` no longer drops the AgentSession

Two related changes that together clarify the session-lifecycle
command surface.

**1. Rename.** The slash command previously known as `/kill` is
renamed to `/close`. The implementation has always been a graceful
close (close stdin → SIGINT → 2 s grace → SIGKILL fallback), so the
old name overstated the cost and confused users into expecting a
hard SIGKILL. `/close` matches the actual semantics and parallels
the rest of the session-lifecycle naming.

What changed in the rename:

- `internal/command/kill/` → `internal/command/close/`. Package
  symbols renamed in lock-step: `KillAgent` → `CloseAgent`,
  `KillAllAgents` → `CloseAllAgents`, `FormatKillResults` →
  `FormatResults`, `killGraceTotal` → `closeGraceTotal`. Internal
  `Result.Action` strings: `"killed"` → `"closed"`,
  `"kill-failed"` → `"close-failed"`.
- `Spec.Name = "close"`. No `/kill` alias — this is a hard rename,
  not a soft migration. Existing users must update their IM
  shortcuts and any muscle memory.
- All comments, tests, doc files cross-referencing `/kill` updated;
  `docs/feat/F-43-kill-new-graceful-and-reset.md` renamed to
  `docs/feat/F-43-close-new-graceful-and-reset.md`.

**2. `/close` no longer drops the AgentSession entry.** Previously
`/kill` (now `/close`) called `ChatSession.DropAgentSession` after
the graceful close — removing the entry from the pool, clearing
`selectedAS`, and deleting the row from `agent_sessions.json`. That
was inconsistent with the "session identity" theme of the rest of
the runtime: even `/stop` preserves the AgentSession, and the
intended semantic of a graceful close is "give the bridge process
a hard restart while preserving the conversation context".

The new `/close` semantics:

- Calls `as.Close()` to terminate the bridge process (graceful
  shutdown, same as before).
- Does **not** call `DropAgentSession`. The AgentSession entry stays
  in the pool; `selectedAS` stays pointed at it; the row in
  `agent_sessions.json` stays; the captured `sessionID` stays.
- The AS goes to `StatusExited` (via `ObserveClose` once the events
  channel drains) and `Handle()` returns a defunct handle.
- The next user message triggers a respawn via the same Spawner on
  the same `(Agent, Cwd)` key, which detects `StatusRunning &&
  Handle() != nil` mismatch in `LookupSelectedAgentSession` and
  re-execs the child with `--resume <sessionID>` to continue the
  conversation.

What this means operationally:

- `/close` is now "give the bridge a hard restart without losing
  the conversation" — use it when the bridge is wedged or you want
  to flush its in-memory state.
- To fully discard a session (kill process AND forget
  conversation), wait for daemon shutdown (stale Detached entries
  are reaped) or edit `agent_sessions.json` directly.
- `cs.DropAgentSession` is no longer called by `/close`. The
  accessor is still exported for callers that need it (daemon
  shutdown reaper, future `/forget` command); see the updated doc
  comment in `internal/chatsession/chatsession.go`.

Compare the three commands:

| Command | Process | AS entry | sessionID | resume on next msg |
|---------|---------|----------|-----------|--------------------|
| `/stop` | may or may not exit | preserved | preserved | n/a |
| `/close` | killed | preserved | preserved | yes (`--resume`) |
| `/new` | reset in-place | preserved | cleared | no |

### TryFlush SKIP reason `as_status_exited`

Added a new skip path in `ChatSession.TryFlush` so that when the
selected AS is in `StatusExited` (e.g. between `/close` and the
next respawn), queued messages stay in the queue instead of being
submitted to a defunct handle.

### `/steer <message>` — stop the in-flight turn and prepend to the queue

New slash command that gives the user "redirect" semantics when the
agent's current direction is wrong. Two runtime additions:

- `MessageQueue.PushFront(msg)` — symmetric to `Push`, prepends
  `msg` to the head of the pending region so the next `Peek`
  returns it first.
- `ChatSession.SteerUserMessage(msg)` — calls `as.Stop(ctx)`
  fire-and-forget (to nudge the bridge into emitting its terminal
  event sooner) and then `queue.PushFront(msg)`. Stop's outcome
  is ignored; PushFront always runs.

The slash command factory (`internal/command/steer/`) parses
trailing args (via `strings.TrimPrefix(input.Text, "/steer")` to
preserve multi-word bodies), builds a `Message{Kind: Normal}`,
emits `MessageQueued` for the F-54 wire contract, and replies with
a short `🛑 Steering: <preview>` card.

Distinguishing semantics vs the other session commands:

| Command | Process | AS entry | sessionID | Queue effect |
|---------|---------|----------|-----------|--------------|
| `/stop`  | may or may not exit | preserved | preserved | none |
| `/steer` | may or may not exit | preserved | preserved | prepend |
| `/close` | killed (graceful) | preserved | preserved | none |
| `/new`   | reset in-place | preserved | cleared | none |

See `docs/feat/F-55-steer-slash-command.md` for the full design +
per-bridge Stop behavior.

### /gtw pr: generate Conventional Commits title+body and open the PR

Closes the loop with `/gtw push`. The user types `/gtw pr [-a <agent>]`
after pushing; nightme spawns a one-shot agent to read the commits /
diff, asks it for a Conventional Commits 1.0.0 title + structured
body (fenced markdown block), and then calls `gh pr create` or
`glab mr create` to open the PR / MR. GitHub and GitLab both go
through the same dispatch code path via the unified
`GitProvider.CreatePR` interface (added to provider.go; both
implementations live next to the existing `GetIssue` /
`AddLabel` / `RemoveLabel` methods).

- **Head unpushed check**: refuses when `rev-list @{u}..HEAD > 0`
  and points the user at `/gtw push first`. No silent push.
- **Nothing-to-PR check**: refuses when `rev-list <base>..HEAD == 0`
  so empty PRs aren't opened by accident.
- **Provider resolution**: yml `Repo` / `Provider` (set by
  `/gtw fix`) win over a `RemoteOriginURL` + `Detect` fallback,
  matching the `/gtw fix` policy.
- **IM-friendly card**: ✅ PR opened with branch / base / url /
  worktree, same style as `/gtw push` and `/gtw close`. Errors
  echo `gh` / `glab` stderr verbatim so the user sees the actual
  reason (auth, head not pushed, repo not found, …).
- **Agent safety**: prompt explicitly forbids `git commit`,
  `git push`, `gh pr create`, `glab mr create`; the agent only
  generates text.
- **ErrPRExists sentinel**: `gh pr create`'s "already exists"
  path is mapped to a friendly ❌ instead of a raw error.
- **Non-worktree mode**: `/gtw pr` (and `/gtw push`) work on a
  plain `git checkout -b <branch>` checkout without a prior
  `/gtw fix` — `loadDispatchContext` derives Worktree /
  Branch / RepoRoot from `git rev-parse --show-toplevel` +
  `--abbrev-ref HEAD` when there's no `.nightme/gtw.yml`.
- **Chat-CWD reads**: `dispatchPush` / `dispatchPR` now read
  `cs.SelectedCwd()` instead of the daemon's process pwd
  (the previous `pushCwd()` shell-out silently returned the
  daemon's startup directory and broke any user who `/cwd`'d
  into a worktree after launching nightme).
- See [`wip/gtw-pr.md`](./wip/gtw-pr.md) for the design rationale
  and [`wip/gtw-pr-plan.md`](./wip/gtw-pr-plan.md) for the
  implementation phases.

### gtw hooks + agent config (`~/.nightme/gtw.yml`)

`/gtw <fix|push|close|sync>` now supports a per-command user-level
configuration loaded from `~/.nightme/gtw.yml`. Two surfaces:

- **Agent priority**: `cli -a <name>` > `yml <cmd>.agent` >
  `cs.SelectedAgent()`. Yml-referenced agent unknown to
  `agent.Builtins` → warn + fall through to session default
  (never silently swap, never brick `/gtw`).
- **before/after hooks**: shell hooks run sequentially around the
  main command. v1 only supports `type: shell` (or bare-string
  sugar); the structured form leaves room for `type: agent` /
  `type: notify` in v2. Implementation lives in
  `internal/command/gtw/hooks.go` and is wrapped by
  `Factory.withHooks` at the factory layer; see
  [`wip/gtw-hooks.md`](./wip/gtw-hooks.md) for the full design.

**Iron rule**: hooks and yml config are additive — they never
block the main flow. Yml missing → silent skip; yml malformed →
warn + skip; hook failure → warn + continue to next hook;
`after` hooks fire even when the main command fails. All hook
output (success or failure) is echoed back to the chat so users
can see what actually ran.

Tests added in `internal/command/gtw/hooks_test.go` (32 cases:
Load semantics, RunHooks failure isolation, FormatResults
always-echo, ResolveAgent 3-tier + fallback + note-honesty,
withHooks factory wrapper including chat-order split).

### refactor(chatsession): extract AgentSession to internal/agentsession

`AgentSession` and its directly-coupled types (`Prompt`, `EnrichedEvent`,
`Spawner`, `Message`, `Status`, `agentSessionCounter`) moved from
`internal/chatsession/` to a new `internal/agentsession/` package.

Layering after this change:

```
internal/agent/         abstract: AgentSpec / Agent / Bridge / Mode
internal/agentsession/  runtime unit: AgentSession + per-AS state
internal/chatsession/   pool manager: ChatSession + persistence
```

Call-site impact is minimized via type aliases in `chatsession`
(`type AgentSession = agentsession.AgentSession`, etc.). New public
methods on `AgentSession`:

- `HandlePTYRestart(ctx, launcher)` — encapsulates the kill + readpump
  reset + respawn + `SessionID` clear lifecycle. `ChatSession` no
  longer reaches into AS internals (asMu, readpumpStarted, respawn).
- `InjectEvent(ev)` — test-only helper to push events directly into
  the dispatcher queue.
- Test-only setters: `SetHandleForTest`, `SetStatusForTest`,
  `SetPIDForTest`, `SetCurrentPromptForTest`, `SetIsReadyForTest`,
  `EndPromptForTest`. Production code MUST NOT use them.

Docs: `docs/SPEC.md` §1.1a (new) documents the package structure;
`docs/feat/F-32` / `F-34` / `F-54` updated to use `agentsession.*`
type names.

### test(agentsession): satisfy driver interface in fakeDrivers

`fakeDriver` and `callRecordingASDriver` in
`internal/agentsession/test_helpers_test.go` were missing the
`Abort` and `SetModel` methods added to `internal/agent.driver`
by #99 (opencode bridge). The interface assertion in
`agent.NewAgent` then panicked at test runtime once #102
extracted AgentSession into its own package and started
actually passing these fakes through `buildLive`. Fixed by
adding the two no-op methods; no test currently exercises
those paths so `nil` is correct. Out of scope for gtw hooks
but blocks CI on `feat-gtw-hooks` (which merged main), so
landed here to keep CI green.

### Codex app-server bridge (new agent)

The `codex` CLI is now a first-class bridge agent, joining
claude / pi / pty. The bridge speaks the codex **app-server** JSON-RPC
2.0 protocol directly over merged stdio pipes (no ACP middleware,
no PTY); see [`docs/bridge/codex.md`](./docs/bridge/codex.md) for the
full reference and [`wip/codex.md`](./wip/codex.md) for the design
notes.

- **Handshake**: `initialize` (with `optOutNotificationMethods` for
  all six text/reasoning delta streams) → `initialized`
  notification → `thread/start` (or `thread/resume` when a
  SessionID is supplied via `StartConfig.SessionID`).
- **Translator**: F-52 state machine flushes agentMessage text at
  tool boundaries and on turn completion; reasoning accumulates
  with `[思考] ` prefix; commandExecution / fileChange /
  webSearch / mcpToolCall / dynamicToolCall map to
  `EventAgentToolStart` / `EventAgentToolEnd` with stable per-item
  IDs.
- **Usage**: populated via the codex ≥0.125 `thread/tokenUsage/updated`
  notification (`last` preferred, `total` fallback). The per-turn
  usage rides on both `EventAgentResult.Usage` and
  `EventAgentDone.Usage`, including `ContextWindow` and the F-55
  `ContextWindowPct` formula.
- **Approvals**: `item/commandExecution/requestApproval`,
  `item/fileChange/requestApproval`, `item/toolCall/requestApproval`,
  and `item/requestUserInput` are dispatched to permission
  handlers; `respond`/`respondErr` write the JSON-RPC envelope
  directly. Five-minute default approval timeout; package var so
  tests can compress it.
- **Mode**: `ModeJSONIO` (same as claudecode / pi); the runtime
  does not branch on `Mode` for JSON-IO bridges.
- **Config**: the example agent name moves from `codex-acp` to
  `codex`; the `codex-acp` reference in docs / agents / tests is
  replaced with the new bridge.

### Lifecycle / close ordering fix

`codex.session.Close()` previously held `closeOnce.Do` while
waiting for `cmd.Wait()` to drain, which deadlocked against
`lifecycle()`’s own `closeOnce.Do(close(events))`. Split: lifecycle
owns `close(events)`; Close owns the shutdown-initiation once and
waits for `exitDone` outside of it. Caught by the real-CLI
e2e test (`TestE2E_FreshThread`).

### AgentEvent flattening + ResumeID→SessionID rename + F-49 compaction removal

Three related changes landed together as one runtime/schema
migration; see [`wip/agentevent.md`](./wip/agentevent.md) for the
full plan (13 sections, ~580 lines).

**`AgentEvent` payload flattening** — the long-standing tension
between "events" (actions, transient) and "context" (state, stable
for session lifetime) is resolved by giving every event a flat
top-level copy of the bridge-side session context:

- `AgentEvent` gains 5 flat `string` fields: `SessionID / Model /
  AgentName / Workspace / Branch`. Bridges stamp these on every
  delivered event (not just `EventAgentReady`); runtime reads them
  directly instead of mirroring into `AgentSession` state.
- `Err error` replaces `Error *AgentErrorEvent` (1-field wrapper)
  and absorbs the in-sub-type `Err` / `IsError` fields on
  `AgentToolEndEvent` / `AgentResultEvent`. Channels check
  `msg.Err != nil` instead of digging into sub-structs.
- Three single-purpose payload structs deleted entirely:
  `AgentErrorEvent`, `AgentReadyEvent`, `AgentCompactionEvent`.
  The corresponding EventKinds stay (`EventAgentError`,
  `EventAgentReady`) — only the wrappers go.
- `EventAgentCompaction` Kind **deleted entirely** — F-49
  compaction tracking was YAGNI: bridges were still digesting
  protocol differences, runtime `CompactionCount` was never
  observed in production, no consumer existed.

**`ResumeID` → `SessionID` rename (full chain)**:

- `registry.AgentSessionEntry.ResumeID` → `SessionID` (JSON key
  `resume_id` → `session_id`; `AgentSessionFileVersion` 2 → 3).
  Existing `agent_sessions.json` files with `resume_id` keys are
  silently ignored on load — `SessionID` defaults to `""`, the
  next spawn starts fresh.
- `AgentSession.resumeID` field → `sessionID`; `SetResumeID` →
  `SetSessionID`.
- `agent.StartConfig.ResumeID` → `SessionID`.

The rename unifies the two naming variants that were drifting
apart (the field captured "the agent's own session id" but the
method/persistence layer called it "ResumeID", which was the
wire-side semantics — confusing readers).

**Compaction chain removed (F-49 abandonment)**:

- `EventAgentCompaction` Kind, `AgentSession.CompactionCount`,
  `RecordCompaction()`, `registry.AgentSessionEntry.CompactionCount`
  JSON field, `gateway.SessionContext.CompactionCount`, feishu
  footer "· 🗜 N" segment — all deleted. 4 tests removed.
- [`docs/feat/F-49-compaction-counter.md`](./docs/feat/F-49-compaction-counter.md)
  marked OBSOLETE; design preserved as "why we didn't do this"
  reference.

**`OutboundMessage` symmetric flattening**: `Ready *AgentReadyEvent`
→ 5 flat `string` fields (`SessionID / Model / AgentName /
Workspace / Branch`) on `OutboundMessage`; `Err error` added so
channels can render error UI consistently.

**Breaking**:
- `agent.AgentEvent`: `Error *AgentErrorEvent` field removed
  (use `Err error`); `Ready *AgentReadyEvent` field removed
  (use top-level `SessionID / Model / AgentName / Workspace /
  Branch`); `Usage *AgentUsageEvent` field removed (always nil
  in old code); `IsError` field on `AgentToolEndEvent` and
  `AgentResultEvent` removed; `EventAgentCompaction` Kind
  removed; `Model` field on `AgentUsageEvent` removed (duplicate
  of `AgentEvent.Model`).
- `gateway.OutboundMessage`: `Ready *AgentReadyEvent` removed;
  `Err error` field added.
- `chatsession.AgentSession`: `resumeID` field renamed to
  `sessionID`; `SetResumeID` renamed to `SetSessionID`;
  `CompactionCount` field removed; `RecordCompaction` /
  `CompactionCount` methods removed.
- `registry.AgentSessionEntry`: `ResumeID` field renamed to
  `SessionID` (JSON key `resume_id` → `session_id`); `Model`
  field kept; `CompactionCount` field removed.

**Docs**: [`wip/agentevent.md`](./wip/agentevent.md) (canonical
plan), [`docs/feat/F-49-compaction-counter.md`](./docs/feat/F-49-compaction-counter.md)
(marked OBSOLETE), [`docs/SPEC.md` §0.14 changelog](./docs/SPEC.md)
(stale — needs follow-up edit; current entry still describes the
original F-49 increment).

### F-42: `/close` graceful shutdown + `/new` ResumeID clear + per-entry list reply

Three related fixes bundled:

- **`/close graceful shutdown: `ChatSession.CloseAll()` now actually signals each child process to exit via the bridge's own `Close()` path (stdin EOF + SIGINT + 2s grace + SIGKILL fallback). Old v1.2 implementation was a data-only operation that orphaned children. Disk entries are deleted *after* the process dies. `InputBuffer` is preserved (user messages are not /close's concern).

- **`/new` clears ResumeID on dead/detached entries**: previously `NewActiveAgentSessions` silently skipped dead entries, leaving a stale `ResumeID` that the next spawn would replay as `--resume <dead-id>` — defeating the user's `/new` intent. Dead entries now count as `matched=1`, `action=marked-fresh`, and their ResumeID is cleared in-memory + persisted.

- **Per-entry list reply** for both commands: instead of "Killed N" / "Reset X/Y", the user sees a per-agent status list with `✓` / `✗` / `•` markers, sorted alphabetically, capped at 20 lines + "...and N more" tail (Feishu 4KB safety).

**Docs**: `docs/feat/F-43-close-new-graceful-and-reset.md` (canonical — 14 sections, design + decision log + error matrix + test plan + risk).

**Background**: v1.2 §3.2 documented `/close` as "清空 pool" with no mention of process lifecycle. The bridge's graceful shutdown infrastructure (each bridge has its own 2s grace + SIGKILL watchdog) was already in place — but `KillAll` never called it. v1.3.x F-34 introduced `/new` with the "skip dead" optimization (Q-N4) which was correct for the no-spawn decision but didn't account for the stale ResumeID side effect.

**API changes (breaking)**:
- `CloseAll() error` → `[]CloseResult, error`
- `NewActiveAgentSessions(ctx, name) (matched, reset int, err error)` → `(matched, reset int, results []ResetResult, firstErr error)`

### F-41: Active Reconnect — 30s forced Stop+Start (no HTTP probe, no tier)

**Background**: F-40 added observability (`nightme health` + WSHealth struct + SDK lifecycle callbacks) so we can see when the Feishu WebSocket is down. F-41 closes the loop with **active recovery**: a 30s ticker that forces `ch.Stop() → 100ms → ch.Start()` whenever the SDK reports `OnDisconnected`, so the user-visible "no response" window drops from SDK's default 2min reconnectInterval to **30s**, and continues at 30s cadence for as long as the network stays down (no "give up after N tries" logic).

**Mechanism**: each prober tick kills the SDK's internal reconnect goroutine and starts a fresh `Start()` cycle. This effectively overrides the SDK's 2min default without changing its parameters. The prober stops on `OnReconnected` / `OnReady`; otherwise it ticks forever. No HTTP probe, no circuit breaker, no tier escalation, no watchdog.

**Files**: `internal/channel/feishu/reconnect.go` (NEW — prober struct, ticker, force-restart, snapshot), `internal/channel/feishu/reconnect_test.go` (NEW), `internal/channel/feishu/adapter.go` (wire SDK callbacks), `internal/channel/feishu/health.go` (add `Prober ProberSnapshot` to `WSHealthSnapshot`), `cmd/nightme/health.go` (new PROBER section).

**Docs**: `docs/feat/F-41-active-reconnect.md` (canonical), `docs/SPEC.md §0.9`, `docs/channel/feishu.md §13.18`.

**不变式**:
- `OutboundMessage` 契约不变 — prober 不影响 `channel.Send()`
- daemoncontrol RPC 协议向后兼容 — `health` JSON 多了 `prober` 字段,旧 client 忽略
- prober 永不主动退出(Connected 恢复或 daemon shutdown 除外)— 故意不引入"放弃重连"语义
- SDK 默认 `autoReconnect=true` 保留 — prober 跟 SDK reconnect timer **并行**,prober 抢先,SDK 兜底

### F-40: WS reconnect observability + `nightme health` command

**Background**: when a user reported "feishu消息nightme没收到" there was no signal we could read to distinguish WS down / SDK dead / reply path stuck. F-40 adds observability + a CLI command for first-stop diagnosis.

**Changes**:
- `internal/channel/feishu/health.go` (NEW): `WSHealth` struct + thread-safe ring buffers for `EventRing` (32 lifecycle events), `InboundRing` / `OutboundRing` (8 most recent successful samples). Updated by SDK `OnReady` / `OnError` / `OnDisconnected` / `OnReconnecting` / `OnReconnected` callbacks.
- `internal/daemoncontrol/`: new `health` RPC command + `HealthProvider` interface + `GetHealth` client function. `cmd/nightme/run.go` wires the post-`newChannel` adapter into the server's health provider.
- `cmd/nightme/health.go` (NEW): `nightme health [--json]` — human-readable or raw JSON status with `STATUS` / `LIVENESS` / `LAST ERROR` / `RECENT EVENTS` / `RECENT INBOUND` / `RECENT OUTBOUND` sections.

**Tests**: 8 `WSHealth` unit tests, existing `./...` tests still pass.

### F-39: `OutResult` → independent reply (reverse F-37 §13.3)

**Reverse-section proof**: Claude Code stream-json's `result.result` is byte-level equal to the last `assistant.event` content, so the previous dedup logic (`receipt_event.go:113-124`) silently swallowed the full final answer on any reply > 600 chars. F-39 reverses that path: `OutResult` no longer folds into the rolling-log receipt card, but is delivered as an **independent reply** anchored at `userMsgID` so the receipt card and the final answer become two separate surfaces.

**Three-stage dispatch** (ported from cc-connect `platform/feishu/feishu.go::buildReplyContent` + openclaw-lark `card/builder.ts::buildCompleteCard`):
- no markdown indicators → `MsgTypeText` (plain text bubble)
- markdown + tables > 5 → `MsgTypePost` + `tag:"md"` (GFM, no Card 2.0 5-table cap)
- default → `MsgTypeInteractive` (Card 2.0) with one or more `tag:"markdown"` divs, split by `splitMarkdownForDivs` at ≤ 1000 runes/div

**Markdown sanitize pipeline** (ported from cc-connect):
- `sanitizeMarkdownURLs` — non-HTTP(S) link → plain text (avoids 230001 invalid href)
- `preprocessFeishuMarkdown` — ensure ``` fence preceded by newline (lark_md renders as code block, not inline)
- `stripInvalidFeishuCardImages` — drop `![alt](not-img_xxx)`, keep Feishu image keys
- `optimizeFeishuCardMarkdown` — H1→H4, H2-H6→H5, code-block protect, newline compression

**Envelope defense**: 28 KB hard cap on the rendered card body; OutResult over the cap is truncated via `truncateRunes` and re-built. The 30 KB Feishu envelope is the ceiling; cap leaves 2 KB headroom.

**Files**: `internal/channel/feishu/{adapter.go::Send(OutResult),adapter.go::sendResultAsReceipt (new helper),card_sanitize.go (new),result_render.go (new),receipt_event.go (remove dedup + EventResult case)}`; tests `card_sanitize_test.go (new), result_render_test.go (new), adapter_test.go (TestSend_OutResult_*), receipt_event_test.go (TestEventToEntry_Result_Dropped)`.

**Docs**: `docs/feat/F-39-result-as-new-reply.md` (canonical design); `docs/SPEC.md` §0.8; `docs/channel/feishu.md` §13.16 + §13.17 + §12 渲染表 + §13.3 反转注 + §15.0 状态汇总.

**不变式**:
- `OutboundMessage` 契约不变(`Kind: OutResult`, `Result *agent.ResultEvent` typed field)
- Gateway 不动(`Translate` 仍产 OutboundMessage)
- ChatSession 不动(`currentTurnUserMsgID` 单数锚点保留)
- `ReplyTo = currentTurnUserMsgID` 不变(独立 reply 也锚同 userMsgID;Feishu 端视觉连接保留)
- 抽象归抽象 / 具体归具体(独立 reply target 是 Feishu 自治)
- §1.4 边界规范保留(OutResult 字段是 typed,Channel 自决 target)

### F-38: `/tools on|off` + per-tool thread-merge via PATCH

**Slash command**: `/tools on | /tools off` (also accepts `show`/`hide` aliases; `/tools` with no args reports current mode).

**State**: `ChatSession.ToolsMode` per-chat (`ToolsModeHide` default — opposite of `/think` which defaults to Show; rationale: tool spam is the loudest part of the agent stream and most users want it off by default), persisted as `ChatSessionEntry.ToolsMode` (JSON omitempty so old `chat_sessions.json` files decode to the safe Hide default).

**Gate**: `cmd/nightme/run.go::newEventHandler` drops `OutToolStart` and `OutToolEnd` after Translate + ReplyTo stamping when `cs.ToolsMode() == ToolsModeHide`. Other OutboundKinds (`OutText` / `OutResult` / `OutThinking` / `OutCompaction` / `OutInit` / `OutUsage`) are unaffected. Independent of the existing ThinkMode gate.

**Render upgrade** (Feishu only, `/tools on`): `internal/channel/feishu/tool_thread_merge.go` — each `OutToolStart` posts a fresh thread reply and remembers the Feishu message_id; the matching `OutToolEnd` PATCHes that same reply with the merged body (start body + newline + result body). Result: 10 tools in one agent turn = 10 thread replies (one per tool, call+result merged) instead of 20. Falls back to fresh reply on PATCH failure or orphan End (no silent data loss). Echo / other Channels unaffected — merge is Feishu-specific Channel rendering.

**Abstraction boundary preserved**: `OutboundMessage.Tool` is still a typed `ToolInfo` primitive; merging is a Channel-internal rendering decision (Feishu-specific, via `PUT /im/v1/messages/{id}` text PATCH). No new `OutboundKind`, no Gateway / ChatSession schema changes; `OutboundMessage` shape 100% unchanged from F-think.

**Files**: `internal/registry/tools_mode.go`, `internal/chatsession/toolsmode.go`, `internal/chatsession/chatsession.go`, `internal/chatsession/manager.go`, `internal/gateway/handlers_tools.go`, `internal/gateway/handlers_chatsession.go`, `cmd/nightme/run.go`, `internal/channel/feishu/tool_thread_merge.go`, `internal/channel/feishu/adapter.go`.

**Docs**: see `docs/SPEC.md` §0.7 + §3.1.3 + `docs/feat/F-38-tool-merge-and-toggle.md`.

### F-think: per-chat thinking-content visibility toggle + markdown rendering

**Slash command**: `/think on | /think off` (also accepts `show`/`hide` aliases; `/think` with no args reports current mode).

**State**: `ChatSession.ThinkMode` per-chat (`ThinkModeShow` default, `ThinkModeHide` opt-in), persisted as `ChatSessionEntry.ThinkMode` (JSON omitempty so old `chat_sessions.json` files decode to default).

**Gate**: `cmd/nightme/run.go::newEventHandler` drops `OutThinking` after Translate + ReplyTo stamping when `cs.ThinkMode() == ThinkModeHide`. Other OutboundKinds are unaffected.

**Render upgrade**: `internal/channel/feishu/thinking_card.go` — OutThinking now posts to the Feishu thread as a `Card 2.0` interactive card with `lark_md` content (via `postThreadMarkdownReply`). Long bodies split into multiple div elements via F-37 `splitMarkdownForDivs`, preserving code-block atomicity. Plain text `postThreadReply` is unchanged for OutToolStart / OutToolEnd / OutCompaction (those remain single-line summaries).

**Abstraction boundary preserved**: `OutboundMessage.Text` is still a primitive string; markdown rendering is a Channel-internal decision (Feishu-specific). No new `OutboundKind`, no Gateway / ChatSession schema changes.

**Files**: `internal/registry/think_mode.go`, `internal/chatsession/thinkmode.go`, `internal/chatsession/chatsession.go`, `internal/chatsession/manager.go`, `internal/gateway/handlers_think.go`, `internal/gateway/handlers_chatsession.go`, `cmd/nightme/run.go`, `internal/channel/feishu/thinking_card.go`, `internal/channel/feishu/adapter.go`.

**Docs**: see `docs/SPEC.md` §0.6 + §3.1.2.

### Architecture: ChatSession + AgentSession (replaces v1.x Session)

The v1.x model bound one chat to one CLI process. This snapshot
splits that into two layers:

- **`ChatSession`** (per-chat): persistent per-chat context.
  Bound 1:1 to an IM chat. Owns an `AgentSession` pool keyed by
  `(agent, cwd)`, the InputBuffer FSM, the readPump, and the
  `EventHandler` callback. See [`docs/feat/F-27-chatsession.md`](./docs/feat/F-27-chatsession.md).
- **`AgentSession`** (per-`(agent, cwd)`): the actual CLI process
  handle. `(ChatSessionID, agent, cwd)` is unique within a chat's
  pool. See [`docs/feat/F-29-agent-session-pool.md`](./docs/feat/F-29-agent-session-pool.md).

The runtime manages chats via `chatsession.Manager` (replaces
v1.x `session.MemoryManager`). See
[`docs/feat/F-27-chatsession.md` §3.4](./docs/feat/F-27-chatsession.md).

### Slash commands (replaces `/run`)

| Old (v1.x) | New (current dev) | Semantics |
|---|---|---|
| `/cwd <path>` | `/cwd <path>` | Set workspace for the chat. **Does not spawn.** |
| `/run <agent>` | (deleted) | Spawn was eager; now lazy. |
| (none) | `/use <agent>` | Switch active agent; **lazy spawn** — reuse pool if present, else spawn. |
| `/close` | `/close` | Clear the AgentSession pool (activeCwd/activeAgent survive). |

InputBuffer FSM moves to ChatSession level: queued messages flush
to whichever `AgentSession` is currently active. The buffer state
survives `/use` switches (it is keyed on the chat, not the agent).

### Config schema (breaking)

```yaml
# v1.x
agent:
  default: claude
  agents:
    claude:
      command: claude
      args: []
      env: {}

# current dev
primary: cc                    # top-level (was agent.default)
agents:                        # top-level list (was nested map)
  - name: cc
    bridge: claude
    command: "claude --dangerously-skip-permissions"
  - name: claude
    bridge: claude
    command: claude
```

`Command` is now a single string (binary + args) — split at spawn
time with `strings.Fields`. `Args` and `Env` fields are removed.
`Bridge` is a new per-entry field. See
[`configs/nightme.example.yaml`](./configs/nightme.example.yaml).

### Persistence (breaking)

| v1.x | current dev |
|---|---|
| `registry.json` (single file) | `chat_sessions.json` + `agent_sessions.json` |

The v1.x file is **not** transparently migrated to v1.2 entries —
v1.x did not persist `chat_id`, so the chat → session binding
cannot be reconstructed. On startup, v1.x's `registry.json` is
archived to `registry.json.v1.bak` and the runtime starts fresh.
See [`MIGRATION.md`](./MIGRATION.md).

### Interactive configuration

`nightme config` opens a two-level menu for setting up
agents. Currently only the `Agents` submenu exists: it merges
built-in agents (`agent.Builtins`) with user-configured entries
from `cfg.Agents`, lets the user pick which one to set as
`primary`, and saves back to `config.yaml`. Binary detection
is **not** performed — if you select a non-installed agent, the
spawn will fail at runtime. See
[`docs/feat/F-30-interactive-config.md`](./docs/feat/F-30-interactive-config.md).

### Runtime contracts (new abstractions)

- **`Spawner` interface** (`internal/chatsession/spawn.go`):
  ChatSession ↔ agent.Registry seam. The production
  `registrySpawner` wraps `agent.Registry.Get/Detect/Start`;
  tests substitute a `fakeSpawner` without forking. See
  [`docs/feat/F-27-chatsession.md`](./docs/feat/F-27-chatsession.md).
- **`EventHandler`** (`internal/chatsession/readpump.go`):
  per-ChatSession callback `func(chatID, *AgentSession,
  agent.AgentEvent)`. The runtime installs it once at startup;
  it persists across `/use` switches.
- **Default `FlushHook`**: every ChatSession has a built-in
  FlushHook that forwards queued user messages to the current
  active AgentSession's `SendBlocks`. Without this, queued
  messages would silently drop. The runtime may override via
  `SetFlushHook` (e.g., to add receipt-card side effects).

### Removed

- `cmd/nightme/run.go` (v1.x daemon): replaced by
  `cmd/nightme/run_v12.go`. The v1.x escape hatch (`--v12`
  flag, `runDeps`-based fallback) is gone — there is no flag.
- `internal/session/session.go::Session` (the per-chat single-CLI
  type): replaced by `internal/chatsession/AgentSession`.
- `internal/session/manager.go::MemoryManager`: replaced by
  `internal/chatsession/manager.go::Manager`.
- **ChatType field removed from data model** (F-33): ChatSession,
  BindingEntry, ChatSessionEntry, and `gateway.InboundMessage` no
  longer carry chat-type classification. The Gateway treats all
  chats as opaque string IDs; channel adapters classify chats
  internally for rendering decisions only. The `channel.ChatTypeThread`
  constant is dropped as well — Feishu `topic_group` (thread)
  messages flow through the same path as `p2p` / `group`. Pre-F-33
  `chat_sessions.json` files continue to load transparently (the
  `chatType` JSON field is silently ignored). The `/status`
  command no longer shows a DM/Group label. See
  [`docs/feat/F-33-simplify-chatid-data-model.md`](./docs/feat/F-33-simplify-chatid-data-model.md).
- **`InboundMessage.ReplyTo` was always empty** in pre-F-33 builds:
  F-33 wires the field from `event.Message.ParentId` (Feishu SDK's
  native parent_id). The thread-top-level `RootId` is intentionally
  not surfaced — nightme data model never carries a thread concept.
  The wire-up is currently metadata-only (no dispatch logic
  consumes `InboundMessage.ReplyTo` yet); future "reply context
  pull" features can rely on it. Outbound `ReplyTo` semantics are
  unchanged (still `currentTurnUserMsgID` per §13.10).

### Bug fixes

- **`/cwd` etc. silently failed** — `RegisterChatSessionCommands`
  registered command names with a leading slash (`"/cwd"`); the
  Gateway `ParseCommand` already strips the slash on lookup, so
  commands never matched and fell through to the fallback.
  Fixed: register as `"cwd"` / `"use"` / `"kill"`.
- **User messages silently dropped on Idle** —
  `InputBuffer.Add` calls the flush hook only when not nil;
  ChatSession constructed the buffer with `nil` hook, so
  Idle-flushed messages were no-op'd before reaching the agent.
  Fixed: `ensureBuffer` installs a default FlushHook that
  forwards to `cs.activeAS.SendBlocks`.
- **v12Fallback duplicated `errors.Is(ErrNoActiveCwd)` check** —
  cosmetic; the second branch was unreachable.

### Known gaps (deferred)

- **E2E Feishu DM round-trip test** — manual verification only;
  unit + integration tests cover F-27 / F-28 / F-29 / F-30.
- **`internal/session/` v1.x residue** — `MemoryManager` is still
  referenced by `internal/gateway/cmd/handlers.go` (v1.x binding
  helpers). Cleanup pending — tracked in
  (No separate tracking doc; see git history.)

### F-thread-route: OutThinking / OutToolStart / OutToolEnd → Feishu thread reply (2026-08-04)

反转 v1.3 §13.6 折叠方案(实机验证失败:30 panel 撞破 50 element 上限、视觉噪声大于折叠收益、最终回答被挤掉)。新方案:Channel 按 OutboundKind 自决 routing——thinking/tool/compaction 直接 POST 到 Feishu thread(rootID = userMsgID),receipt card 收窄到只承载最终答复(OutText / OutResult)+ 元数据(OutInit / OutUsage)。

**OutToolEnd 类型感知摘要**("decision 处理"):bridge 层把 `ToolEndEvent.Args` 填好;Channel 层 `summarizeToolEnd(name, args, output, err)` 按 tool name 生成单行摘要(`Read /foo.go → 1234 lines`),不 dump 原始 output 到 thread。Receipt card body 元素数从 ~30 降到 ≤5,50 element 上限永远不破。

**Bridge 层 contract 扩展**:`agent.ToolEndEvent.Args string` 字段;claudecode bridge 从同 message `tool_use` block 拿 args 填入。

**不变式**:OutboundMessage 不动(无新 Kind);Gateway 不动;ChatSession 不动;`currentTurnUserMsgID` 单数锚点保留;F-33 thread 概念不进 nightme 数据模型不变式保留;抽象归抽象 / 具体归具体 —— thread 路由是 Feishu 自治决定,Slack / Web 各自决定怎么渲染 thinking/tool。

详见 [`docs/SPEC.md` §0.3](./docs/SPEC.md) + [`docs/feat/F-37-tool-thread-routing.md`](./docs/feat/F-37-tool-thread-routing.md) + [`docs/channel/feishu.md` §13.12](./docs/channel/feishu.md) + [`docs/feat/F-25-rolling-log.md` §3.1.1](./docs/feat/F-25-rolling-log.md) + [`docs/feat/F-08-channel-abstraction.md` §4](./docs/feat/F-08-channel-abstraction.md)。

**飞书 3 种 reply 形态 (实机群 Frtpilot-Xiage 验证，2026-08-04 子决议，关闭 §13.10 P2)**:

> **作用域**：这三个名字（`ReplyInChat` / `ReplyInThreadAndChat` / `ReplyInThread`）是 **`channel/feishu` 自治**——不上升到 Gateway / OutboundMessage 抽象层。其他 channel（Web / Slack）应**各自**决定怎么渲染 OutThinking / OutTool*，不复制飞书的 thread 方案。

| 形态 | 飞书 `reply_in_thread` 字段 | main chat 显示 | thread panel 显示 | `thread_id` 响应 |
|---|---|---|---|---|
| **ReplyInChat** (顶级 Create) | n/a | 独立气泡 | 不在 thread | `""` |
| **ReplyInThreadAndChat** | **字段省略** (`omitempty` nil) | **正文内联** | 同一份正文 | `""` |
| **ReplyInThread** | `true` | **"X replies" 灰条** | **正文** | `omt_xxx` (首次分配，后续 reply-true 复用) |

`sendMessageFunc` / `sendContent` / `sendViaLarkReply` / `SendMessageText` / `SendCard` / `postThreadReply` 全链路加尾部 `replyInThread bool` 参数。`sendViaLarkReply` 内部 `larkim.NewReplyMessageReqBodyBuilder()` **仅在 `true` 时**调 `.ReplyInThread(true)` (false 路径靠 `omitempty` 字段省略保留 recorder log / idempotency cache 字节级兼容；**严禁**简化成 `.ReplyInThread(replyInThread)` 否则 false 路径多 28 字节破坏兼容性)。

按 OutboundKind 路径拆分（2026-08-04 ops 实机确认）：

- `OutThinking` / `OutToolStart` / `OutToolEnd` → **ReplyInThread** (agent 进度只进 thread panel,main chat 仅显示 "X replies" 指示器)
- `OutCompaction` / receipt 冷启动卡 / `OutCard` (permission) / `OutCommandReply` → **ReplyInThreadAndChat** (必须 main chat 可见)
- 顶级 Create (ReplyInChat) 形态 → nightme **不**走 (fallback 230011/231003 才退化)

> Kinds 命名 ops 用 past tense (`OutToolStarted/Ended/Think`)，但 nightme enum 实际是 present tense (`OutToolStart/OutToolEnd/OutThinking`)。**不**改 enum 名（会牵动 Gateway 抽象层多个包），只按 enum 行为归属。

测试：`TestSend_ThreadOnlyEvents_PassReplyInThreadTrue` (3 kinds × ReplyInThread: OutThinking/OutToolStart/OutToolEnd) + `TestSend_ChatVisibleEvents_PassReplyInThreadFalse` (4 paths × ReplyInThreadAndChat: ReceiptColdStart/OutCard/OutCommandReply/OutCompaction) + `cmd/_probe/send_one` 实机飞书群验证。详见 `docs/feat/F-37-tool-thread-routing.md` §7.5。

---

## Earlier snapshots (v1.x series, archived for reference)

These are preserved for diff archaeology. They predate the
current-dev model and are not compatible.

### v1.x — receipt card rolling-log

One user message → ONE Feishu reply card. The card body grows
over the agent's lifetime and FIFO-evicts from the front when
it overflows `replyMaxBytes`. Structured receipt footer
(`Agent: <name> | cwd: <path> | tokens: <in>K / <out>K`).

### v1.x — Gateway responsibility isolation

Channel ↔ Session ↔ Bridge three-layer separation. Binding table
(`chat_id → SessionID`) and per-userMessage receipt FSM owned by
Gateway; Channel renders transitions; Session is process-domain
only. See [`docs/feat/F-26-gateway-hub.md`](./docs/feat/F-26-gateway-hub.md).

### v1.x — Feishu one-click registration

QR-code onboarding via Feishu app credentials. See
[`docs/feat/F-22-feishu-onclick-registration.md`](./docs/feat/F-22-feishu-onclick-registration.md).

### v1.x — local bridge smoke test (`nightme test`)

PTY-byte-pipe smoke test for any CLI. `--cleanup` kills the child
on SIGINT/SIGTERM; default detaches. See
[`docs/feat/F-19-cli-bridge.md`](./docs/feat/F-19-cli-bridge.md).

### v1.x — structured logging + panic recovery + unified exit codes

slog + JSON output, secret redaction, `Recover()` middleware
maps panics → `CodeGenericError`, unified `ExitCode()` for CI
scripts.

---

## Architecture (single-line summary)

```
nightme (single binary on user's laptop)
├── channel/feishu/    IM transport (Feishu WebSocket)
├── gateway/           Slash router + binding + receipt FSM
├── chatsession/        Per-chat session context + AgentSession pool
│   ├── Manager         chat_id → ChatSession
│   ├── ChatSession     activeCwd / activeAgent / pool / InputBuffer / readPump
│   ├── AgentSession    bridge.AgentSession wrapper (per (agent, cwd))
│   ├── Spawner         Detect → Start via agent.Registry
│   └── InputBuffer     Idle/Busy FSM + FlushHook
├── agent/              Agent interface + Builtins + Event
├── bridge/             Bridge abstraction (PTY / ACP / SDK / JSON-IO)
├── registry/           chat_sessions.json + agent_sessions.json (0600)
└── config/             YAML + NIGHTME_* env overrides
```

See [`docs/SPEC.md` §1](./docs/SPEC.md) for the full
responsibility table.