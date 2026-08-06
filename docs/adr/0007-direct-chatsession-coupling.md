# ADR 0007: Drop the SessionService abstraction layer

**Status**: Accepted · 2026-08-06
**Replaces**: F-51 §3.2 (the "command → services is the only allowed edge" invariant)
**Affects**: `internal/command/`, `cmd/nightme/`, all 8 slash commands

## Context

F-51 introduced `internal/command/services/` with a `SessionService` interface
and a `*chatsession.Manager` adapter in `cmd/nightme/session_adapter.go`. The
stated rationale was §3.2:

> command → services is the only allowed edge. services MUST NOT import
> chatsession; chatsession MUST NOT import command. This is a grep-able
> invariant that prevents the v1.3.x gtw ↔ chatsession cycle from coming back.

The cost: every command package that wants to read or mutate chat-session
state goes through:

```
command/<name>/*.go
  → services.Session / SessionService (interface)
  → cmd/nightme/session_adapter.go (concrete impl)
  → *chatsession.Manager / *chatsession.ChatSession
```

Every new chatsession method that a command wants to call requires three
edits: the interface, the adapter, and the command handler. For the 7
commands in B5 (the migration plan that prompted the discussion), this
ceremony surfaces concretely as:

- `/new` needs `PersistAgentSession` → not on the interface yet
- `/use` needs `StartReadPump` → not on the interface yet
- `/kill` and `/new` need `FormatKillResults` / `FormatResetResults` →
  live in `chatsession`, would have to be moved to a command package

The "future-proof" benefit of the indirection — the ability to swap
`*chatsession.Manager` for a different implementation — has no concrete
demand. The chatsession layer is stable, single-process, single-tenant, and
not on any near-term refactor roadmap.

## Decision

**`command/` packages may import `internal/chatsession` directly.** The
indirect layer (`services.SessionService`, `cmd/nightme/session_adapter.go`)
is removed. Each command package constructs a Factory that holds a
`*chatsession.Manager` directly:

```go
// internal/command/cwd/commands.go
type Factory struct {
    mgr            *chatsession.Manager
    defaultPrimary string
}

func NewFactory(mgr *chatsession.Manager, defaultPrimary string) *Factory {
    return &Factory{mgr: mgr, defaultPrimary: defaultPrimary}
}

func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
                          input command.SlashInput) (*command.SlashOutput, error) {
    cs := f.mgr.GetOrCreate(input.ChatID, f.defaultPrimary)
    // ... direct use of *chatsession.ChatSession ...
}
```

`command.RuntimeServices` loses its `Session` field. What remains:

- `Channel` — outbound message surface (`*gateway.Channel` projected as
  `command.Channel`). Kept: this is a real interface boundary and there
  are concrete plans for multi-channel deployments.
- `ReactionRouter` — inbound reaction dispatcher. Kept: the router is
  implemented by `services.reactionRouter` and consumed by multiple
  packages (gtw today; future reaction-driven commands tomorrow).

`internal/command/services/` shrinks to `services/reaction.go` only. The
session.go file (SessionService, Session, AgentSession, KillResult,
ResetResult, WatchMode/ThinkMode/ToolsMode aliases) is deleted.

`command.RequireActiveCwd` is replaced by a local helper in the commands
that need it (or by an inline check).

## Consequences

### Wins

- **B5 unblocks automatically.** `/new` calls `mgr.PersistAgentSession`
  directly. `/use` calls `cs.StartReadPump` directly. No new interface
  methods, no new adapter methods.
- **~340 lines deleted** (services/session.go + session_adapter.go).
- **Every Handle is shorter** by 1-2 lines of indirection (sess.X vs cs.X).
- **Refactoring chatsession is single-touch.** Renaming a chatsession method
  is one grep across `command/<name>/`, not two greps (interface + adapter).
- **The `command → chatsession` edge is honest.** Today's abstraction was
  hiding a real coupling that the type system would have made visible if
  we'd let it.

### Losses (accepted)

- **Test friction increases.** Today, tests mock `services.Session` (5
  methods). After this change, tests mock or construct `*chatsession.Manager`
  (~30 methods). Mitigation: use a real `*chatsession.Manager` with
  ephemeral state (`chatsession.NewManager().WithPersistence(nil)`) for
  most cases — it's already used this way in `internal/chatsession/*_test.go`
  and is fast.
- **The grep-able "command → services" invariant is gone.** Replaced by
  review-discipline: PRs touching `internal/command/` should not introduce
  *new* cyclic imports (e.g. chatsession importing command). This is what
  `go build ./...` already enforces at compile time.
- **Future work that wanted to swap chatsession for a different
  implementation now touches every command package.** This is a theoretical
  cost; there is no concrete plan to do so.
- **gtw loses a small asymmetry.** Today `gtw.Factory` goes through
  `services.SessionService` for its Sender factory. After this change,
  `gtw.Factory` directly holds `*chatsession.Manager` and the Sender
  adapter narrows to a local interface (or also holds `*chatsession.Manager`).

### What stays

- `internal/command/services/reaction.go` (the ReactionRouter interface +
  impl). This is a real interface boundary used by multiple packages.
- `command.RequireActiveCwd` → becomes a local helper. There is no reason
  for a global helper once the abstraction is gone; two-liner check in
  three command packages is fine.
- `command.Reply` (the single-reply helper). Real helper, used by all
  commands.
- The Commander + Registry + Factory interface. These are the core
  architecture.

## Implementation

This change ships together with the B5 migration plan
(`docs/feat/B5-slash-command-migration-plan.md`) — specifically B5a/B5b/B5c
combined into a single PR. The structural changes (RuntimeServices.Session
removal, session_adapter deletion, services/session.go deletion) ship first
in the same PR.

PR structure (single PR, three commits):

1. **refactor(command)**: drop SessionService abstraction. RuntimeServices
   loses Session field. session_adapter.go + services/session.go deleted.
   Build is broken (gtw + the 7 legacy handlers don't compile).
2. **refactor(command)**: migrate all 8 commands (gtw + cwd/use/kill/new/
   watch/think/tools) to direct *chatsession.Manager holders. Run.go
   rewires each Factory. Build now passes.
3. **test(command)**: port the 7 existing test suites from the legacy
   handler shape to the new Factory shape. `go test ./...` green.

Rollback: revert the PR. F-51 was shipped as a single squashed commit;
reverting this PR brings back the abstraction cleanly.

## What we are NOT doing

- We are NOT introducing a new abstract "SessionService2" interface.
  "Narrow consumer-defined interfaces" (Go idiom) are encouraged where they
  earn their keep — but the command packages will declare them locally
  inside their own package, not in a shared services package.
- We are NOT changing the Commander / Registry / Factory interface
  themselves. The dispatch surface is correct; the issue is only with the
  SessionService middleman.
- We are NOT moving FormatKillResults / FormatResetResults to the command
  packages yet. They live in chatsession today; after this change they can
  stay there and command/kill + command/new can import them directly. If
  we ever delete chatsession, they move then.

## Open follow-ups (out of scope)

- The `Outbound` slice on `SlashOutput` (multi-message reply support).
  Tracked in B5 plan §"已知架构缺口" #1.
- An outbox / reply-channel on `RuntimeServices` for async "late reply"
  scenarios. Tracked in B5 plan §"已知架构缺口" #2.
- Persistent cross-chat state on `RuntimeServices`. Tracked in B5 plan
  §"已知架构缺口" #3.

These are orthogonal to this ADR and can be addressed independently.