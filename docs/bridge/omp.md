# omp Bridge — `internal/bridge/omp`

> **Scope**: `internal/bridge/omp/` — nightme-side adapter for the
> `can1357/oh-my-pi` CLI (binary name `omp`, docs at https://omp.sh).
>
> **Tier**: built-in bridge (issue #443).
>
> **Status**: implemented.
>
> **Related docs**:
>
> - [`pi.md`](./pi.md) — closest sibling bridge; omp's wire forks
>   from pi's but already diverged (`agent_end{isTerminal:true}`
>   replaces `agent_settled`; thinking arrives as a dedicated
>   content block rather than inline `<think>` tags). The two
>   packages are deliberately independent — see §1 below.
> - [`codex.md`](./codex.md) and [`opencode.md`](./opencode.md) —
>   print-mode spawn shape that omp's `RunOnce` mirrors.
> - [`claude.md`](./claude.md), [`acp.md`](./acp.md) — for the
>   `agent.AgentEvent` / `ModeJSONIO` shapes the bridge consumes.

## 0. Summary

`internal/bridge/omp` adds omp as nightme's eighth built-in agent.
omp exposes a long-lived `omp --mode rpc` transport (LF-delimited
JSON commands, responses, and events over real stdio pipes — NOT
PTY) and a one-shot `omp -p --mode json` print mode. The bridge
owns the protocol: spawn, handshake, request/response correlation,
event translation, and turn-end resolution.

Both spawn paths are wired into one `Starter`:
`Start` (chat sessions) → `omp --mode rpc --approval-mode yolo`;
`RunOnce` (one-shot calls) → `omp -p --mode json --approval-mode yolo`.
Both reuse the same `translator` (translate.go) because print-mode
emits the same NDJSON event shape as RPC mode — only the absence
of a `response` envelope and `get_state` handshake differs.

Control surface (R4/R14/R15): `prompt`, `abort`, `new_session`.
`set_model` / `set_thinking_level` / `steer` / `follow_up` /
`compact` / `switch_session` are out of scope (issue #443 §Scope).

## 1. Why a separate package (not shared with `internal/bridge/pi`)

omp forked from pi upstream but the wire has already diverged:

| Concern | pi (today) | omp (today) |
|---|---|---|
| Turn-end signal | `agent_settled` | `agent_end{isTerminal:true}` |
| Thinking on the wire | inline `<think>...</think>` text | dedicated `thinking_*` content block |
| Compaction events | `compaction_start` / `compaction_end` | documented as `auto_compaction_*` (untested) |
| Control vocabulary | `get_state` / `prompt` / `abort` / `new_session` | same vocabulary, different wire semantics |
| `--resume` flag | `--session-id <id>` (pi-ism) | `-r/--resume=<value>` (omp-native) |
| Print mode opening frame | `{"type":"session","id":...,"version":3,...}` | same |

A shared JSONL transport layer would be possible, but every
protocol change in either agent would touch the other bridge. Issue
#443 §Decisions resolves this as "two independent packages".

## 2. Wire shape

omp's wire (verified against the v18.x line at https://omp.sh):

- Strict LF framing; one JSON object per line, terminated by `\n`.
- `stdout` carries protocol frames only. `stderr` carries logs.
  Merging them (e.g. PTY transport) breaks omp — the bridge
  spawns the child with three separate pipes
  (`proc.New` → `child.StdinPipe` / `child.StdoutPipe` /
  `child.StderrPipe`).
- Commands: `{"id":<id>, "type":<name>, ...fields}`. The `id` is
  opaque; the bridge uses `req-NNNNNN` strings.
- Responses: `{"id":<id>, "type":"response", "command":<name>,
  "success":<bool>, "data":<obj|absent>, "error":<string|absent>}`.
- Events (async server pushes, no id):
  `{"type":<event name>, ...event-specific fields}`.
- Boot handshake in RPC mode: `{"type":"ready",
  "protocolVersion":1, ...}` (frame 1), then
  `extension_ui_request` (server→client UI dialog), then
  `advisor_cost_changed`, then `available_commands_update`. The
  bridge absorbs all of these without responding.
- Print mode opening frame: `{"type":"session",
  "version":3, "id":..., "timestamp":..., "cwd":...}` instead of
  `ready`. The id is the call's `SessionID`.

Frame size cap (`MaxFrameSize` in rpc.go): 4 MiB, matching
`internal/bridge/pi.MaxFrameSize`. Empirical 99th-percentile frames:

| Frame | Typical size |
|---|---|
| `get_state` response | ≈33 KiB (with full `systemPrompt` + `dumpTools`) |
| `available_commands_update` | ≈16.7 KiB |
| omp wire `maxFrameBytes` | 1 MiB |

## 3. Spawn recipe

```go
// internal/bridge/omp/agent.go
var DefaultArgs = []string{
    "--mode", "rpc",
    "--approval-mode", "yolo",
}
```

Per the agent-no-config-tampering principle, the bridge only
injects the transport/permission flags. Model / provider / API
key / MCP configuration all flow from the user's own `~/.omp/`.

### Resume

When `cfg.SessionID` is non-empty, `buildArgs` appends
`["--resume", "<id>"]` LAST so user-supplied session-selection
flags (`--resume` / `--continue` / `--no-session`) are
stripped via `filterSessionFlags` so the runtime identity always
wins.

omp's `Session "<id>" not found.` is the canonical resume-
rejection error text. omp exits 0 in this case, so the bridge
catches it via the stderr ring buffer
(`isResumeFailure` / `recordResumeFailure` in agent.go) and
surfaces `agent.ErrResumeUnhealthy` from `Start` so the chat
layer can clear the stale id and retry without `--resume`.

## 4. Translator (`translate.go`)

Translation rules match issue #443 §Spec §5/6/7/8. Highlights:

- `ready` is consumed by the boot handshake, not the translator
  (readPump discards it).
- `message_start` (any role): no event.
- `message_end{assistant}`: stamp `lastModel` and `lastUsage`
  from `message.usage` — usage is an overwriting snapshot, not a
  sum (each API call's `input` already contains the conversation
  history).
- `message_update` dispatches on the inner
  `assistantMessageEvent.type`:
  - `text_start` clears `textBuf[contentIndex]`.
  - `text_delta` accumulates (no event).
  - `text_end` moves the buffer into `pendingText`.
  - `thinking_*` is symmetric, flushing
    `EventAgentThinking` on `thinking_end`.
  - `start` / `done` / `error` are message-lifecycle markers,
    dropped.
- `tool_execution_start` flushes `pendingText` as one
  `EventAgentText` (F-52), then emits `EventAgentToolStart`
  with the title resolved as `intent ?? toolName`.
- `tool_execution_end` correlates on `toolCallId` so the end
  event carries the start-time args (omp's wire doesn't echo
  args on end).
- `agent_end` is the SOLE turn-end signal when `isTerminal !=
  false`. The translator emits one `EventAgentResult` (with the
  final text + usage) followed by one `EventAgentDone{
  Reason:"settled" }`.
- `agent_start` / `turn_*` / `agent_end{isTerminal:false}` are
  ignored; they are not terminal.
- `extension_ui_request` is ignored without response (omp does
  not block on no-response).
- `advisor_cost_changed` / `available_commands_update` /
  `prompt_result` / `session_settled` are housekeeping noise.
- Anything else: drop silently; the next text_delta's `textBuf`
  must remain intact (R5 acceptance: "未知事件插入在两个 text_delta
  之间时文本缓冲不被打散").

## 5. Modes

`Info().Mode == agent.ModeJSONIO` — `omp --mode rpc` is LF-delimited
JSON over real stdio pipes (no JSON-RPC envelope), matching
`ModeJSONIO`'s "NDJSON / JSON events over stdio" definition. pi
uses the same mode.

## 6. Print mode (`print.go`)

`RunOnce` calls `runPrintMode` which spawns `omp -p <prompt>
--mode json --approval-mode yolo`, drains the stdout NDJSON stream
through the same translator, and returns a `RunResult` carrying
text + `UsageInfo` + `SessionID` (peeked from the opening
`{"type":"session","id":..}` frame).

print-mode has no `ready` and no `get_state` — the translator's
default case drops `session`, and `peekPrintMeta` peeks the id
once per line.

If the process exits without `agent_end{isTerminal:true}`,
`parsePrintStream` returns an error whose message contains the
agent name (`omp: exit without agent_end isTerminal=true...`)
so /gtw commit's recovery logic can attribute the failure
correctly.

## 7. Out of scope

- `omp acp` (Agent Client Protocol) — separate wire; the
  `internal/bridge/acp` package currently merges stdout/stderr
  via PTY, which violates omp's protocol-only-on-stdout
  contract.
- `set_model` / `set_thinking_level` / `steer` / `follow_up` /
  `compact` / `switch_session` — control commands the issue
  defers to a separate issue.
- `rpc_chunk` / `negotiate_protocol` v2 large-frame reassembly.
- Image attachments via RPC `prompt` (omp's `input` payload
  shape is not verified to round-trip).
- v2 / chunked compaction events.

## 8. Tests

- `translate_test.go` — translation table coverage (R5 full
  table; R6 delta dispatch; R7 usage overwrite-not-sum; R8
  single-terminal invariant; unknown events do not break
  buffering; `agent_end{isTerminal:false}` is ignored;
  `extension_ui_request` does not break the turn).
- `rpc_test.go` — JSONL framing + request/response correlation
  (writeLine atomicity; round-trip; timeout; fail-pending;
  write-after-close; cancel-response; race-free registration;
  `MaxFrameSize >= 4 MiB` invariant).
- `starter_test.go` — Info / Detect / Init surfaces.
- `print_args_test.go` — argv layout per R12 (no `--model`,
  `--provider`, `--api-key`, `--mcp-config`); `--resume`
  position per R13.
- `probe_unix_test.go` (build tag `!windows`) — real-binary
  probe per R9. Skips when `omp` is not on PATH. On a
  developer machine with omp installed and the login flow
  complete, drives `omp --mode rpc` end-to-end (boot →
  get_state → prompt → agent_end{isTerminal:true}).

## 9. Configuration

`cfg.Agents` whitelist (`internal/config/config.go`) — `omp` is a
valid `name` for an entry whose `command` is the absolute path to
the omp binary. `cfg.Primary: omp` selects omp as the chat session
agent on first run (after `cfg.Agents` overrides are applied and
`Detect()` succeeds).

`NIGHTME_OMP_DEBUG` — set to `0` (or `false` / `no` / `off`) to
silence the bridge's `[omp]` breadcrumb trail. Default ON.

`internal/bridge/pty` is NOT a user-facing agent (see
docs/primary-agent-detection.md §"Why PTY is not in Builtins").
omp's transport is real stdio pipes, never PTY — `agent.go`
spawns through `proc.New` and bypasses the pty bridge.
