# Changelog

All notable changes to nightme are documented here. nightme
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
as closely as a pre-1.0 project can.

## [Unreleased] — v0.3 architecture refactor (Stage 3 landed — v0.3 internally feature-complete)

- **CLI log surface (fix)**: the structured logger now tees every
  line to the persisted log file **plus stdout and stderr**, so
  the terminal shows the same trace the file captures and `nightme
  run > log.txt` / `2> log.txt` redirects both keep the trace.
  Companion to the Feishu adapter's `logInbound` call wiring —
  `handleMessage` now emits one `feishu: incoming` line per user
  message (the function was defined but never called since commit
  `0a0e021` left the call edge as a stale comment). Combined, the
  CLI now shows both halves of every conversation:
  - inbound: `feishu: incoming chat_id=… text=…`
  - outbound: `feishu: outgoing kind=… target=…`
  Existing tests for the file sink still pass; two new tests
  cover the stdout+stderr sinks and the inbound log emission.
- **Stage 3 (Feishu display strategy in Channel.Send)**: the
  rolling-log receipt logic that lived in
  `internal/channel/feishu/render.go` is now inside the Feishu
  adapter's `Channel.Send`. Each `OutboundKind` is dispatched:
  - `OutText` / `OutThinking` / `OutToolStart` / `OutToolEnd` →
    append to the active receipt's rolling log and edit the
    reply in place via `UpdateMessage` (FIFO eviction still
    applies). Cold-start (`receiptFor`) lazily creates a receipt
    when the gateway's outbound pump emits an OutText without a
    prior `SendUserMessage` (e.g. agent turn started from a
    startup, not a user message).
  - `OutReaction` / `OutReactionRemoved` → swap the reaction
    emoji on the user message (delegated to the existing
    AddReaction / DeleteReaction methods).
  - `OutCard` → `sendContent` with the v1 interactive envelope
    (request_id flows through the gateway's translate +
    OutboundMessage.Card.RequestID).
  - `OutTyping` → silently drop (Feishu has no bot-side typing
    API).
- The `feishu.Renderer` is GONE. The runtime's
  `channelResponder` type-asserts the channel to
  `*feishu.Adapter` and calls `adapter.SendUserMessage` /
  `adapter.MarkExecuting` directly. The sweeper in
  `cmd/nightme/run.go` no longer filters Feishu sessions —
  every channel goes through the gateway.
- New adapter methods: `SendUserMessage(ctx, chatID, userMsgID,
  content) (*MessageReceipt, error)` creates the receipt and
  posts the initial ⏳ reply; `MarkExecuting(ctx, userMsgID)
  error` flips ⏳ → 🔄; `receiptFor(ctx, chatID)` lazily creates
  a receipt for the gateway's outbound pump.
- New constructor: `NewMessageReceiptForReply(chatID,
  userMsgID, replyMsgID)` for cases where the caller already
  posted the reply (the adapter's own SendUserMessage path).
- Removed: `internal/channel/feishu/render.go` (was the
  Renderer struct + its sweep / pump / install helpers) and
  `render_test.go` (was 587 lines of Renderer-only coverage). The
  14+ unit tests that exercised RenderEvent-on-the-renderer are
  replaced by Channel.Send tests in `feishu/adapter_test.go` /
  a new `rolling_log_test.go` (rolled into the same package).
- Verified: `go test -race -count=1 ./...` green across all 17
  packages; `go build ./cmd/nightme` exit 0. Behaviour
  preserved end-to-end: Feishu messages still show the single
  rolling-log line per agent turn (the gateway's outbound
  pump now drives this; the renderer is gone). All previously
  gateway-test coverage (`TestRender_*`, `TestGateway_*`) is
  preserved as `TestAdapter_*` / `TestReceipt_*` tests.

### Architecture after Stage 3

The full message flow is now:

  user → Channel.Incoming() (Feishu WS)
       → Gateway.pumpInbound → Gateway.dispatchLoop
       → Gateway.Handle (slash command or fallback)
       → fallback: Channel.Send(OutboundMessage) — channel-
         specific display strategy. For Feishu, that means
         `SendUserMessage` creates a receipt, queues via the
         session's InputBuffer, and marks it Executing on
         dispatch.

  agent events → Session.Events()
              → Gateway.sweepSessions (5s tick) + per-session
                pumpOutbound
              → translate.AgentEvent → OutboundMessage
              → Channel.Send — Feishu's Send folds each
                event into the active receipt's rolling log
                (header + entries) via UpdateMessage; the
                receipt's reaction emoji is swapped on state
                transitions.

## [Unreleased] — v0.3 architecture refactor (Stage 1 landed)

- **Stage 1 (behaviour-preserving)**: abstract message types + thin
  Channel interface. See `docs/feat/F-26-gateway-hub.md` for the
  full design.
- NEW: `internal/gateway/messages.go` — `InboundMessage`,
  `OutboundMessage`, `Attachment`, `OutboundKind`, `ChatType`,
  `ActionPayload`, `Card`, `Reaction`, `AgentEventEnvelope`.
- NEW: `internal/gateway/gateway.go` — `Gateway` interface
  (`Register` / `Handle` / `ListCommands`), `Command`,
  `CommandResult`, `FallbackHandler`.
- NEW: `internal/gateway/cmd/` subpackage — `RegisterDefaultCommands`
  and the four slash-command handlers (`/cwd`, `/run`, `/kill`,
  `/help`, `/agents`). Moved out of the gateway package so
  handlers can import session without creating a cycle
  (session → channel/feishu → channel → gateway).
- CHANGED: `Channel` interface gains `Name() string` and
  `Send(ctx, OutboundMessage) error`; legacy `SendMessage` /
  `SendLongMessage` remain as helper methods. `channel.Message`
  and `channel.Attachment` are now type aliases for
  `gateway.InboundMessage` and `gateway.Attachment`.
- CHANGED: Feishu adapter implements `Channel.Send` by
  dispatching per `OutboundKind`. Rolling-log receipt path
  is NOT yet migrated (Stage 3); OutToolStart /
  OutToolEnd / OutThinking / OutTyping are dropped for now.
- Verified: `go test -race -count=1 ./...` green across all 17
  packages; `go build ./cmd/nightme` exit 0.

### Added
- `nightme agents [--json]`: list the registered agent set the daemon
- `nightme agents [--json]`: list the registered agent set the daemon
  would dispatch `/run` to, with name / command / args columns.
  Backed by the same `buildRunAgentRegistry` path used by `nightme test`
  and `nightme run`, so the names here are exactly what `/run <name>`
  accepts.
- `/agents` gateway slash command: same data, formatted for IM
  (bullet list per agent + a `/run` usage hint).
- `Agent` interface gains `Command()` and `Args()` getters so the
  registry exposes its spawn recipe without callers having to type-
  switch on the concrete agent packages.
- REPL mode: bare `nightme` (no args) now enters an interactive shell
  with a banner, `nightme> ` prompt, and dispatch of each line to the
  existing cobra command tree. Built on `bufio.Scanner` (zero new
  deps); reads history / tab completion are deferred to v0.3+ if a
  daily-driver REPL turns out to be needed. `exit` / `quit` / Ctrl-D
  leave the shell; an unknown command surfaces an inline error and
  the loop continues.
- REPL history: production path uses `chzyer/readline` so ↑/↓
  navigate previously-typed commands. History is held in memory
  only (per Devin: "in memory is enough") — no on-disk persistence,
  no history file. Ctrl-C at the prompt cancels the current line
  (no longer exits the shell). Tests stay on the scanner-based
  `runREPLWith` path so they don't need a TTY; the readline and
  scanner paths share `dispatchREPLLine`. New dep:
  `github.com/chzyer/readline`.
- REPL arrow-key fix: removed an overzealous `FuncFilterInputRune`
  that was dropping ESC (0x1b) bytes. Arrow keys arrive at the
  terminal as the escape sequence `\x1b[A` / `\x1b[B`; the filter
  silently ate the leading ESC so readline never saw the sequence.
  Removing the filter restores ↑/↓ navigation. The earlier
  TestFilterREPLInput_BlocksControlChars is removed with the
  filter.
- Single rolling-log receipt (F-25 §6 v0.3): every user message
  gets exactly ONE reply in chat that grows over the agent's
  lifetime. The reply starts as "⏳ 等待中", then appends every
  event (💭 thinking, 🔧 tool call, ✅ tool done, 💬 final reply)
  in order. When the message exceeds ~3.5 KiB, oldest entries are
  dropped from the front (FIFO) and a "…(前 N 条已省略)" marker is
  prepended. The reaction emoji (⏳ / 🔄 / ✅) on the user message
  is unchanged — the lifecycle signal stays at F-25's swap-on-
  state-change semantics, the reply message is the work log.
  EventText no longer triggers a separate Feishu message; the
  Renderer forwards every event into the receipt's `Append`,
  which updates the same reply message in place via UpdateMessage.
  Claudecode's stream-json now surfaces thinking blocks as
  EventText with a "[思考] " prefix so the user can see what the
  agent is doing; the Renderer renders thinking as 💭 and final
  replies as 💬 on the same prefix. EventDone / EventError
  transition the receipt to terminal state. `MessageReceipt.bot`
  is now an unexported `receiptBot` interface so unit tests can
  drive the receipt lifecycle without a real lark client.
- Tool-end output surfacing: `ToolEndEvent` gains an `Output`
  field. The claudecode bridge now stringifies the `tool_result`
  content from stream-json and populates `Output` (was previously
  dropped). The Feishu renderer shows the output as a short summary
  on the ✅ line (e.g. `✅ Read → 47 lines, handler at L42`)
  instead of the useless template `✅ Read done`. Bridges without
  `Output` still fall back to "name done" so existing agents stay
  working.
- Compat shim: `Renderer.MarkExecuting(ctx, userMsgID)` re-added
  so the pre-existing `cmd/nightme/run.go` wiring (added in
  `26611ec`, broken since the receipt refactor) compiles again.
  Looks up the receipt by userMsgID and delegates to
  `receipt.SetExecuting`. No new behaviour — just unblocks
  `make dev`.
- Renderer index refactor: `Renderer` now keeps two indexes in
  lockstep — `receipts` (chatID -> latest active receipt, used by
  the event pump) and `userMsgIndex` (userMsgID -> receipt, used
  by the legacy F-25 gateway path). Three helpers centralize the
  lock-and-lookup logic that was duplicated across
  `SendUserMessage` and `MarkExecuting`:
  `installReceipt(ctx, *MessageReceipt)` registers a new receipt
  atomically, evicts the prior chat's receipt (calling
  `SetCompleted` on it), and is idempotent on duplicate userMsgID;
  `lookupByUserMsgID(userMsgID)` and `lookupByChatID(chatID)`
  give O(1) receipt access with proper locking. `RenderEvent`
  uses `lookupByChatID`; `MarkExecuting` and `SendUserMessage`
  use `lookupByUserMsgID`. No public-API change; behaviour
  unchanged.
- Drop event-pump fallback: `cmd/nightme/run.go` no longer has a
  `switch ev.Kind { ... }` that fires when `s.renderer == nil`.
  The only path that set up a non-nil renderer was the Feishu
  channel (which is the only supported channel in v0.2.x), so the
  fallback was dead code. A nil renderer is now treated as a
  programmer error and the pump panics with the chat ID for
  context. This removes the misleading "Session ended (exit 0)"
  string that was being sent to users when the pump hit the
  fallback.
- Outgoing Feishu trace: `*Adapter` now logs every Send / Add /
  Delete / Update reaction call at info level via the same
  structured logger the run-time uses. `cmd/nightme/run.go` wires
  the adapter's logger right after `ch.Start`. Combined with the
  existing inbound "received: …" line in the channel pump, the
  CLI surface now shows both halves of every conversation — the
  user sees their own message AND the reply / reaction / card the
  bot sent back, instead of only the inbound side. Failed calls
  upgrade to warn-level and include the error.
- `nightme version` subcommand: REPL-friendly sibling of `--version`
  (Cobra only registers `--version` as a flag, not a verb).
- Cold-start fallback: `nightme run` and `nightme agents` now seed
  the agent registry with `claude` / `codex` / `opencode` defaults
  when the user's `config.yaml` ships an empty `agent.agents` map.
  Fixes the post-`nightme auth login feishu` flow where the config
  had Feishu credentials but no agents, surfacing as `/run claude`
  in Feishu returning "unknown agent: claude". Explicit user config
  is still respected as-is (no per-key merging).
- Agent registration pattern: replaced the name-based dispatch in
  `configuredAgent()` with a self-registration model. Each agent
  package's `init()` registers itself into `agent.Builtins`; the
  binary blank-imports the packages it ships. `cmd/nightme/main.go`
  blank-imports `internal/bridge/claudecode` (the only v0.2.x
  built-in). No defaults table, no name-based switch, no fallback
  for names that aren't registered — if `/run <name>` doesn't
  resolve, the user gets "unknown agent" instead of a misleading
  half-implementation. User config can still add new agents (always
  via ptyagent) or override the built-in `claude` to point at a
  custom binary (which drops the dedicated JSON-IO bridge).
- Package restructure: `internal/agent/` is now pure abstraction
  (`agent.go` + `registry.go` only); per-agent implementations live
  in their respective bridge packages (`internal/bridge/pty/`,
  `internal/bridge/acp/`). The standalone `internal/agent/ptyagent/`
  and `internal/agent/acpagent/` packages are gone — their code
  moved into the bridge packages alongside the protocol and the
  AgentSession implementation. `internal/bridge/sdk/` is removed
  (stub only, never wired). `cmd/nightme/agents.go` becomes the
  registration table: each line is `agent.Builtins.Register(<bridge
  package>.NewAgent(name, command, args, env))`. The `nightme
  agents` CLI subcommand moves to `cmd/nightme/agents_cmd.go` to
  avoid the file-name collision.
- Feishu scope + callback expansion: `DefaultAddons()` now asks
  for the full interaction set the bot will need:
  - Tenant scopes: `im:message:send_as_bot`, `im:message:update`,
    `im:message:receive_v1`, `im:message.reactions:write_only`,
    `im:message.reactions:read`, `im:message:readonly`,
    `im:message.group_at_msg:readonly`, `im:message.p2p_msg:readonly`,
    `im:message.pins:read`, `im:message.pins:write_only`,
    `im:message:recall`, `im:message:send_multi_users`,
    `im:message:send_sys_msg`, `im:resource`, `im:chat:read`,
    `im:chat:update`, `im:chat.members:bot_access`,
    `contact:contact.base:readonly`, `cardkit:card:write`,
    `cardkit:card:read`, `application:application:self_manage`.
    Mirrors `larksuite/openclaw-lark`'s `REQUIRED_APP_SCOPES`
    minus Docx/Base/Calendar/Task (nightme has no feature for
    those yet) so a fresh install can grow features without
    forcing a re-authorize round.
  - Events: `im.message.receive_v1`
  - Callbacks: `card.action.trigger` — required for interactive
    card button clicks (permission card Allow/Deny was previously
    inert — no handler was registered)
  - `OnP2CardActionTrigger(handler)` wired in the WebSocket
    dispatcher with a stub `handleCardAction` that returns an
    info toast; full click-value → permission-decision routing is
    a follow-up. The reaction event subscription is still absent
    (intentional per F-25 docs).
  - `TestDefaultAddons_ContainsRequiredScopes` extended to lock
    in every scope + the callback name.

## [0.2.0] - 2026-08-01

Second public release. Closes out the F-21 (JSON-IO + ACP/SDK/PTY per-agent bridges), F-23 (heartbeat), F-24 (Claude Code bridge), and F-25 (input buffer + dual-track receipts) milestones and ships a deterministic streaming status surface (event-driven, no clock-based guessing).

### Added
- F-23 Heartbeat & Streaming Status: event-driven tick + `ProcessProbe` (进程级 truth) + 用户主权 kill
- F-24 Claude Code Bridge: `--input-format stream-json` + `--output-format stream-json` + `--permission-mode bypassPermissions` + `AskUserQuestion` 双路兼容 (tool_use 拦截 + text fallback) + `PreToolUse` hook (可选 headless answer)
- F-25 Input Buffer: in-memory queue (50 msgs / 100KB, 3 states: Waiting / Active / Done); `flush` 把 buffer 合成单条 prompt 喂给 bridge
- F-25 MessageReceipt: 双轨状态 (Reaction emoji + Reply note), in-place swap 不堆叠; Feishu adapter exposes `AddReaction`
- F-21 ACP backend: `internal/bridge/acp` JSON-RPC client + session 实现
- F-21 SDK adapter fallback: `internal/bridge/sdk` Claude Code SDK adapter
- F-21 agent mode registry: `claude` / `codex` / `opencode`
- ChatType discriminator on incoming messages (DM vs group) + gateway wire (`run.go`)
- Renderer + F-25 receipts 接入 run daemon (text fallback + `updateMessage` + buffer integration)
- GitHub Actions CI workflow (`.github/workflows/ci.yml`, `go build` + `go vet` + `go test -race`)
- Release workflow builds + uploads binary on `v*` tag push

### Fixed
- channel/feishu: swap reactions in-place, no accumulation
- session test: drop unused `sync/atomic` import to satisfy `go vet`

## [0.1.0] - 2026-07-31

First public release. Closes out milestones M0 / M1 / M2 and
ships the M3 hardening pass (structured logging, panic recovery,
unified exit codes, `--cleanup`, CI).

### Added
- F-22 Feishu one-click app registration (QR 扫码授权)
- F-08 Feishu channel adapter (WebSocket + IM rendering)
- F-20 Gateway (slash command router with `/cwd /run /kill /help`)
- F-21 Agent Communication Modes (ACP / SDK / PTY stubs)
- F-19 PTY Mode byte pipe (`aymanbagabas/go-pty`)
- Session lifecycle (`/cwd` + `/run` two-step)
- Process Registry (JSON persistence, mode 0600, atomic rename)
- Local Bridge test mode (`nightme test`, `--cleanup` to kill on shutdown)
- Session list command (`nightme list`, `--json`)
- Structured logging (`internal/logging`, slog + JSON + secret redaction)
- Panic recovery middleware (`Recover()` → `CodeGenericError`)
- Unified error codes (`internal/errors`, `ExitCode()`)
- GitHub Actions CI (`go build` + `go vet` + `go test -race`)
- Release workflow (build + publish on `v*` tags)

### Configuration
- YAML config at `~/.config/nightme/config.yaml`
- `NIGHTME_<SECTION>_<KEY>` env-var overrides
- Logging defaults to `~/.local/share/nightme/nightme.log` (mode 0600)

## [0.0.0] - 2026-07-25 (M0)

Initial documentation-only milestone: PRD, SPEC, FEATURES index,
PLAN, and per-feature design docs (`docs/feat/F-01..F-21`).
