# B5: Remaining 7 Slash Commands → F-51 Architecture

Status: 📝 plan · Owner: TBD · Created: 2026-08-06

## 0. Context

F-51 (`docs/feat/F-51-slash-command-service-separation.md`) shipped the new
slash-command architecture: `command.Factory` interface, `command.Registry`,
`command.Commander` dispatcher, `command.RuntimeServices`, and the
`SessionService` / `Session` / `AgentSession` interfaces under
`internal/command/services/`. F-51 §4 called out that **only `gtw` was migrated
in that PR** — the remaining 7 commands (`/cwd /use /kill /new /watch /think
/tools`) still live in the legacy `internal/gateway/handlers_*.go` files and are
registered via `gateway.RegisterChatSessionCommands` (the old `gw.Register(Command)`
path).

This doc is the **migration plan** for the 7 remaining commands. It mirrors the
gtw pattern as the reference template and lists the gaps in the current
`command/` infrastructure that block a straight copy-paste migration.

Goal after B5 lands: `internal/gateway/handlers_*.go` is gone; every slash
command is a `command.SlashCommandFactory` registered in `cmd/nightme/run.go`
alongside `gtw.Factory`.

---

## 1. Inventory — 7 commands to migrate

| # | Command  | Current file                                 | Handler          | Spec name  | Aliases          |
|---|----------|----------------------------------------------|------------------|------------|------------------|
| 1 | `/cwd`   | `internal/gateway/handlers_chatsession.go`   | `handleCwd` L171 | `cwd`      | —                |
| 2 | `/use`   | `internal/gateway/handlers_chatsession.go`   | `handleUse` L266 | `use`      | —                |
| 3 | `/kill`  | `internal/gateway/handlers_chatsession.go`   | `handleKill` L316| `kill`     | —                |
| 4 | `/new`   | `internal/gateway/handlers_new.go`           | `handleNew` L32  | `new`      | —                |
| 5 | `/watch` | `internal/gateway/handlers_watch.go`         | `handleWatch` L47| `watch`    | —                |
| 6 | `/think` | `internal/gateway/handlers_think.go`         | `handleThink` L54| `think`    | —                |
| 7 | `/tools` | `internal/gateway/handlers_tools.go`         | `handleTools` L61| `tools`    | —                |

All 7 are registered together by `RegisterChatSessionCommands` (`internal/gateway/handlers_chatsession.go:38`),
which is called once from `cmd/nightme/run.go` during daemon startup. After B5
this function — and the `handlers_*.go` files it lives in — are deleted; the
registration moves to `cmd/nightme/run.go` next to the existing
`reg.Register(gtwFactory)` call.

---

## 2. Reference template — gtw.Factory

gtw is the canonical reference. The shape (lifted from
`internal/command/gtw/commands.go`) is:

```
internal/command/<name>/
  commands.go      ← Factory + Spec + Handle
  commands_test.go ← unit tests
  (state.go, types.go, manager.go etc. as needed — gtw has many because
   it carries draft state; cwd/use/kill/new/watch/think/tools are simpler)
```

The Factory shape every command implements:

```go
type Factory struct {
    // injected by NewFactory or SetHandlerDeps
    mgr  SomeManager
    deps SomeDeps
}

func NewFactory(mgr SomeManager) *Factory { ... }
func (f *Factory) SetHandlerDeps(deps SomeDeps) { ... }

func (f *Factory) Spec() command.Spec {
    return command.Spec{
        Name:    "<cmd>",
        Aliases: []string{...},
        Summary: "...",
        Usage:   "/<cmd> ...",
    }
}

func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
                          input command.SlashInput) (*command.SlashOutput, error) {
    // route on input.Args[1] (input.Args[0] is the bare command name)
    // or fall through to f.Spec().Usage
}
```

Wired in `cmd/nightme/run.go` next to the existing gtw registration:

```go
reg.Register(cwd.NewFactory(...))
reg.Register(use.NewFactory(...))
reg.Register(kill.NewFactory(...))
// ...
```

The runtime shim that translates `*gateway.InboundMessage` ↔ `command.SlashInput`
already exists (`cmd/nightme/run.go:383-406`); no runtime-side changes needed
for B5.

---

## 3. Per-command migration spec

Each subsection describes the target shape: package layout, dependencies,
handler logic relative to the legacy version, and test migration. Common to all:

- Replace `*chatsession.Manager` / `*chatsession.ChatSession` / `*chatsession.AgentSession`
  with `services.SessionService` / `services.Session` / `services.AgentSession`
  accessed via `rt.Session`.
- Replace `gateway.Channel` / `reply(ctx, channel, ...)` with
  `command.Reply(ctx, rt, text)` (defined in `internal/command/reply.go`).
- Use `command.RequireActiveCwd(rt.Session.Get(...))` for the
  "Send /cwd first" preflight where applicable.
- Handlers drop the `*InboundMessage`, `args []string`, and `globalPrimary` args
  — those are folded into `SlashInput`.

### 3.1 `/cwd` → `internal/command/cwd/commands.go`

- **Spec**: `Name: "cwd"`, no aliases; `Usage: "/cwd <absolute-path>"`.
- **Dependencies**: none beyond `rt.Session`. (No manager / state of its own.)
- **Handler logic** (lifted from `handleCwd`):
  1. Validate arg (`Usage: /cwd <path>` when missing/empty).
  2. `expandTilde` helper stays in this package (lives next to its caller; not
     a service-level helper).
  3. Relative-path resolution via `os.UserHomeDir` + `filepath.Join(home, raw)`.
  4. `os.Stat` + `IsDir` check (rejects non-existent / file paths early).
  5. `rt.Session.GetOrCreate(chatID, primaryAgent)`.
  6. `sess.SetActiveCwd(abs)`.
  7. Reply `"Workspace set to <abs>.\nSession is ready (active agent: <X>). ..."`.
- **Test migration**: lift the case matrix from `handlers_chatsession_test.go`
  for `/cwd` (~8 cases: missing arg / empty / ~ expansion / relative / absolute /
  nonexistent / file-not-dir / happy path). Use a fake `SessionService` that
  records `SetActiveCwd` calls. `primaryAgent` comes from
  `input.PrimaryAgent`-style extension or is passed via factory construction.
- **Note**: legacy `globalPrimary` is gone — `GetOrCreate` takes it from a
  factory-level default. Convention: factory stores `defaultPrimary` set by
  `cmd/nightme/run.go` via `NewFactoryWithDeps`.

### 3.2 `/use` → `internal/command/use/commands.go`

- **Spec**: `Name: "use"`, no aliases; `Usage: "/use <agent> [args...]"`.
- **Dependencies**: `rt.Session` + a `StartReadPump` hook (see §4 gap).
- **Handler logic** (lifted from `handleUse`):
  1. Validate arg.
  2. `rt.Session.GetOrCreate(chatID, primaryAgent)` then `RequireActiveCwd`.
  3. `sess.SetActiveAgent(name)`.
  4. `sess.LookupActiveAgentSession()` — may spawn.
  5. `sess.StartReadPump()` — start the per-ChatSession event pump for the
     newly-active AgentSession (commit 8c behavior; see §4).
  6. Reply `"Now using <name> (pid=..., cwd=..., source=spawn|resumed)"`.
- **Gap**: `services.Session` currently has **no `StartReadPump` method**. The
  legacy handler calls `cs.StartReadPump()` directly. §4 lists this as a gap
  to fill on the interface (and the chat-session adapter) before /use can be
  migrated.
- **Test migration**: lift /use case matrix (missing arg / empty / no cwd /
  unknown agent / resumed / spawned). Mock `LookupActiveAgentSession` and
  `StartReadPump`.

### 3.3 `/kill` → `internal/command/kill/commands.go`

- **Spec**: `Name: "kill"`, no aliases; `Usage: "/kill"`.
- **Dependencies**: `rt.Session`.
- **Handler logic** (lifted from `handleKill`):
  1. `rt.Session.Get(chatID)` — note `Get`, not `GetOrCreate`; `/kill` is a
     no-op when there's no session.
  2. `sess.KillAll()`.
  3. Reply with the formatted per-entry result list.
- **Test migration**: lift /kill cases (no session / happy path with N entries /
  KillAll error). Mock `KillAll` returning canned `services.KillResult` slices.
- **Helper relocation**: `chatsession.FormatKillResults` (currently in
  `internal/chatsession/chatsession.go:1015`) must move to
  `internal/command/kill/format.go` (or be inlined into the handler) — the
  legacy chatsession package no longer "owns" the kill-result shape after B5.

### 3.4 `/new` → `internal/command/new/commands.go`

- **Spec**: `Name: "new"`, no aliases; `Usage: "/new [<agent>]"`.
- **Dependencies**: `rt.Session` + a `PersistAgentSession` hook (see §4 gap).
- **Handler logic** (lifted from `handleNew`):
  1. `rt.Session.GetOrCreate(chatID, primaryAgent)` then `RequireActiveCwd`.
  2. Parse optional `<agent>` arg.
  3. `sess.NewActiveAgentSessions(ctx, agentName)` — returns matched count +
     `[]ResetResult`.
  4. For each row where `Session != nil && Error == nil`:
     - `r.Session.ResetCumulative()` (already on `services.AgentSession`).
     - `rt.Session.PersistAgentSession(r.Session)` — **gap, see §4**.
  5. Empty-pool reply variants (`"No agent session for ..."`).
  6. Reply with formatted reset summary + error tail.
- **Gap**: `services.Session` currently has **no `PersistAgentSession` method**.
  Two options:
  - (a) Add `SessionService.PersistAgentSession(as AgentSession) error`.
  - (b) Add `AgentSession.Persist() error`.
  Recommended: (b) — persistence is a property of the session itself, not of
  the chat-side state surface. The chat-session adapter already wraps
  `*chatsession.AgentSession`; adding `Persist()` there is one-line.
- **Helper relocation**: `chatsession.FormatResetResults`
  (`internal/chatsession/chatsession.go:1056`) moves to
  `internal/command/new/format.go`.

### 3.5 `/watch` → `internal/command/watch/commands.go`

- **Spec**: `Name: "watch"`, aliases `[]string{"w"}`; `Usage: "/watch on | /watch off"`.
- **Dependencies**: `rt.Session`.
- **Handler logic** (lifted from `handleWatch`):
  1. `rt.Session.GetOrCreate(chatID, primaryAgent)`.
  2. No-arg form: reply current `WatchMode()` + usage hint.
  3. Parse `services.WatchMode` via `chatsession.ParseWatchMode` (or move the
     parser under `command/services` — see §4).
  4. `sess.SetWatchMode(mode)`.
  5. Reply with state-dependent message ("watching all" / "watching mentions
     only" / DM no-op note).
- **Test migration**: lift 5 cases from `handlers_watch_test.go` (clean / no-arg
  / unknown mode / on / off / DM-noop). Mode parser is package-private — moves
  with the handler.

### 3.6 `/think` → `internal/command/think/commands.go`

- **Spec**: `Name: "think"`, no aliases; `Usage: "/think on | /think off"`.
- **Dependencies**: `rt.Session`.
- **Handler logic**: same shape as `/watch` with `ThinkMode` swap. Note: the
  default is **opposite** to `/tools` (`/think` defaults to `ThinkModeShow`,
  `/tools` defaults to `ToolsModeHide`) — preserve this.
- **Test migration**: lift 5 cases from `handlers_think_test.go`.

### 3.7 `/tools` → `internal/command/tools/commands.go`

- **Spec**: `Name: "tools"`, no aliases; `Usage: "/tools on | /tools off"`.
- **Dependencies**: `rt.Session`.
- **Handler logic**: same shape as `/think`. Parses `agent.ToolsMode` via
  `agent.ParseToolsMode`.
- **Test migration**: lift 5 cases from `handlers_tools_test.go` including
  the `TestHandleTools_DoesNotAffectWatchOrThink` cross-mode regression test.

---

## 4. Shared infrastructure gaps

Three blockers prevent a straight copy-paste migration. **None require new
packages**; all three are interface additions.

### 4.1 `services.AgentSession.Persist() error` — for /new

```go
// services/session.go
type AgentSession interface {
    // ... existing ...
    ResetCumulative()
    Persist() error  // NEW — write the entry to disk if dirty
}

// cmd/nightme/session_adapter.go — adapter impl
func (a *agentSessionAdapter) Persist() error {
    // walk up to *chatsession.AgentSession.PersistIfDirty() OR
    // accept a *chatsession.Manager reference for PersistAgentSession.
}
```

Recommended impl: adapter stores a `*chatsession.Manager` in addition to the
`*chatsession.AgentSession` and calls `mgr.PersistAgentSession(a.as)`. This
preserves the legacy semantics (full persist, not the cheaper
`PersistIfDirty`). One-line change at construction (the adapter is
single-purpose; the manager reference is already available at the
`SessionService` level — see next gap).

### 4.2 `SessionService.PersistAgentSession(as AgentSession) error` — alternative

If §4.1 is rejected on layering grounds (AgentSession should not know about
Manager), the alternative is to add it to `SessionService` instead:

```go
type SessionService interface {
    Get(chatID string) Session
    GetOrCreate(chatID, primaryAgent string) Session
    PersistAgentSession(as AgentSession) error  // NEW
}
```

This is more consistent with the current layering (Manager-level ops stay on
`SessionService`); **prefer this over §4.1**. Adapter: a new method on
`*sessionAdapter` that delegates to `a.mgr.PersistAgentSession(...)`.

### 4.3 `services.Session.StartReadPump() error` — for /use

```go
type Session interface {
    // ... existing ...
    StartReadPump() error  // NEW — install the per-session event pump
}
```

Adapter: `func (s *chatSessionAdapter) StartReadPump() error { return s.cs.StartReadPump() }`.

This is a single-line change on the interface + adapter. The concrete
`*chatsession.ChatSession.StartReadPump` already exists
(`internal/chatsession/readpump.go:86`); only the command-package projection
is missing.

### 4.4 Move mode parsers

`chatsession.ParseWatchMode` / `chatsession.ParseThinkMode` /
`agent.ParseToolsMode` should stay where they are (they don't import
`command/services`), but their consumers move to `command/watch`,
`command/think`, `command/tools`. No import cycle: parsers return canonical
types (`registry.WatchMode`, `chatsession.ThinkMode`, `agent.ToolsMode`), which
are aliased into `services.*` (already done for `WatchMode` / `ThinkMode` /
`ToolsMode`).

Optional cleanup: move `chatsession.ParseWatchMode` / `ParseThinkMode` to
`internal/registry/` (the canonical home for `WatchMode` / `ThinkMode`),
keeping `chatsession` as a re-export. Defer this to a separate PR — out of
scope for B5.

### 4.5 Move result formatters

`chatsession.FormatKillResults` / `FormatResetResults` move into the new
`command/kill` and `command/new` packages (one file each, ~40 lines).
`renderKillRow` / `renderResetRow` move with them. No callers outside the
legacy handlers + their tests, so deletion of the originals in
`internal/chatsession/chatsession.go` is safe.

### 4.6 Delete legacy handlers + RegisterChatSessionCommands

After all 7 commands migrate:

- Delete `internal/gateway/handlers_chatsession.go` (contains `RegisterChatSessionCommands`
  + the legacy `handleCwd` / `handleUse` / `handleKill` / `expandTilde` /
  `reply` helper + `RegisterChatSessionRuntime` if it's still around).
- Delete `internal/gateway/handlers_new.go` / `handlers_watch.go` /
  `handlers_think.go` / `handlers_tools.go`.
- Delete the corresponding `_test.go` files (`handlers_chatsession_test.go`,
  `handlers_new_test.go`, `handlers_watch_test.go`, `handlers_think_test.go`,
  `handlers_tools_test.go`) — the new tests under `command/<name>/commands_test.go`
  are their replacements.
- Delete `RegisterChatSessionCommands` call in `cmd/nightme/run.go`; add
  `reg.Register(cwd.NewFactory(...))` etc. next to the existing
  `reg.Register(gtwFactory)`.

---

## 5. Sequencing — proposed batches

The 7 commands split into 3 batches based on dependency surface:

### B5a — stateless toggles (`/watch /think /tools`)

- 3 small commands, identical shape, identical tests pattern.
- **No new interface methods** required (everything already on
  `services.Session`).
- Lowest risk; perfect first migration to shake out copy-paste ergonomics.
- **Files added**: `internal/command/{watch,think,tools}/commands.go`,
  `commands_test.go`.
- **Files deleted**: `internal/gateway/handlers_{watch,think,tools}.go` and
  their tests.
- **Effort**: ~half a day (3 × ~150 lines + tests).

### B5b — `/kill` and `/new`

- Pulls in the `PersistAgentSession` gap (§4.2) and the formatter relocations
  (§4.5).
- B5a lands first so the test pattern + the
  `cmd/nightme/run.go` registration block is already shaken out.
- **Files added**: `internal/command/{kill,new}/commands.go`,
  `commands_test.go`, plus `internal/command/{kill,new}/format.go` for the
  formatters.
- **Files deleted**: `handleKill` from `handlers_chatsession.go`;
  `internal/gateway/handlers_new.go` (and its test).
- **Effort**: ~1 day (interface change + 2 commands + tests + formatter
  moves).

### B5c — `/cwd` and `/use`

- `/use` pulls in the `StartReadPump` gap (§4.3).
- `/cwd` is the simplest of the seven but uses `expandTilde` + filesystem
  checks — those helpers move with it.
- Save for last because `/cwd` is a frequently-used command and `/use` adds
  a new interface method.
- **Files added**: `internal/command/{cwd,use}/commands.go`,
  `commands_test.go`.
- **Files deleted**: `handleCwd` / `handleUse` / `expandTilde` from
  `handlers_chatsession.go`; the file itself goes away if
  `RegisterChatSessionCommands` is the last remaining symbol in it.
- **Effort**: ~1 day.

After B5c lands: `internal/gateway/handlers_chatsession.go` is empty /
deleted; `gw.Register(Command)` is no longer used (gtw alone registers via the
commander); `RegisterChatSessionCommands` is gone. The legacy
`gateway.Command` / `gateway.CommandResult` types remain (they are still used
by the commander shim), but no new code goes through them.

---

## 6. Risks & verification

| Risk                                                    | Probability | Mitigation                                                                                                  |
|---------------------------------------------------------|-------------|-------------------------------------------------------------------------------------------------------------|
| Interface gap (PersistAgentSession / StartReadPump) missed | Medium   | Test each command's handler against a mock `Session` that records all calls; cross-check against legacy behavior in `handlers_*_test.go`. |
| `defaultPrimary` propagation broken — handlers get nil primary | Low  | Factory stores `defaultPrimary`; `run.go` wires it via `NewFactoryWithDeps(...)`. Add unit test asserting the value flows. |
| Mode-parser relocation breaks chat-session callers      | Low         | Defer the parser move (§4.4) — keep parsers in chatsession until a separate PR.                              |
| Formatter relocation loses a row formatting tweak       | Low         | Lift the test fixtures (table-driven `renderKillRow` cases) into the new `format_test.go`.                  |
| `/use` resume path (commit 8c) regresses                | Medium      | The `source=resumed|spawn` branch is hand-tested today; add an explicit `TestFactory_Use_ResumedVsSpawn` case. |
| Concurrent `reg.Register` ordering — /help list drifts  | Low         | `Registry.Specs()` is sorted; `/help` order is stable regardless of register order. Document registration order in `run.go` for code review only. |
| Existing user muscle-memory (`/h` alias?) breaks        | Low         | No aliases registered today. If users expect `/h`, add it to the relevant Factory's `Aliases` slice.        |

### Verification (per batch)

- `go test ./...` — full suite still passes (999 baseline + new tests).
- `go vet ./...` — 0 warnings.
- `golangci-lint run` — 0 issues (if CI enabled).
- Manual smoke (`make restart` + send each command in Feishu):
  - `/cwd <path>` — reply contains "Workspace set to ..."
  - `/use claude` — reply contains "(pid=..., source=spawn)" first time, "resumed" second time
  - `/kill` — reply is the per-entry formatted list
  - `/new` — reply is "Reset N/M agent session(s)."
  - `/watch on|off` / `/think on|off` / `/tools on|off` — reply contains "mode set to ..."
- `/help` lists all 7 commands alongside `/gtw`.

### Rollback

Each batch lands as a single squashed commit on `main`. Rollback = `git revert <merge-sha>`.
The legacy `handlers_*.go` files are deleted at the END of each batch (B5a/B5b/B5c),
not the beginning — intermediate commits keep the legacy path active so a partial
state can be released if a regression slips through review.

---

## 7. Open questions for review

1. **Where do the formatters live?** Plan puts them in `command/kill/format.go`
   and `command/new/format.go`. Alternative: a shared
   `internal/command/services/format.go` with `FormatKillResults([]KillResult)`
   / `FormatResetResults([]ResetResult)`. Pro: reusable for future commands.
   Con: introduces a leaf package for two ~40-line functions. **Default: keep
   local** until a third caller appears.
2. **`defaultPrimary` plumbing** — should it be:
   (a) A field on each `Factory`, set via `NewFactoryWithDeps` at startup
   (current plan, mirrors `gtw`).
   (b) A field on `RuntimeServices` so all commands read it uniformly.
   (c) A getter on `SessionService` (`PrimaryAgent()` already exists on
   `Session` but it's per-chat, not per-runtime). **Default: (a)** for
   consistency with `gtw`.
3. **Aliases** — none of the 7 commands have aliases today. Should any get
   them? `/h` → `/cwd` is sometimes a convention; `/k` → `/kill`; `/n` → `/new`.
   **Default: no aliases** — preserves wire-format stability. Add in a follow-up
   PR if requested.

---

## 8. Effort estimate (calendar)

| Batch | Files touched | Lines (est.) | Effort |
|-------|---------------|--------------|--------|
| B5a   | 6 new + 6 deleted | +500 / −700 | 0.5 day |
| B5b   | 4 new + 3 deleted + 1 interface | +600 / −800 | 1 day |
| B5c   | 4 new + 1 deleted + 1 interface | +500 / −600 | 1 day |
| Total |                 |              | ~2.5 days |

Plus ~half a day for review and PR turnaround per batch (3 PRs).