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
> - [`codex.md`](./codex.md) — the app-server bridge shape that
>   omp's one-shot turn mirrors.
> - [`claude.md`](./claude.md), [`acp.md`](./acp.md) — for the
>   `agent.AgentEvent` / `ModeJSONIO` shapes the bridge consumes.

## 0. Summary

`internal/bridge/omp` adds omp as nightme's eighth built-in agent.
omp exposes a long-lived `omp --mode rpc` transport (LF-delimited
JSON commands, responses, and events over real stdio pipes — NOT
PTY). The bridge owns the protocol: spawn, handshake,
request/response correlation, event translation, and turn-end
resolution.

`Start` (chat sessions) and `RunOnce` (one-shot calls) both spawn
`omp --mode rpc --approval-mode yolo`. The one-shot turn runs the
same protocol over the same pipes — only the turn count and the
session lifetime differ — so the prompt is never bounded by the
platform's argv limit.

Control surface (R4/R14/R15): `prompt`, `abort`, `new_session`.
`set_model` / `set_thinking_level` / `steer` / `follow_up` /
`compact` / `switch_session` are out of scope (issue #443 §Scope).

The bridge's driver implements the package-private `agent.driver`
interface — §4 — and reports liveness through `Keepalive` —
§3.

## 1. Why a separate package (not shared with `internal/bridge/pi`)

omp forked from pi upstream but the wire has already diverged:

| Concern | pi (today) | omp (today) |
|---|---|---|
| Turn-end signal | `agent_settled` | `agent_end{isTerminal:true}` |
| Thinking on the wire | inline `<think>...</think>` text | dedicated `thinking_*` content block |
| Compaction events | `compaction_start` / `compaction_end` | documented as `auto_compaction_*` (untested) |
| Control vocabulary | `get_state` / `prompt` / `abort` / `new_session` | same vocabulary, different wire semantics |
| `--resume` flag | `--session-id <id>` (pi-ism) | `-r/--resume=<value>` (omp-native) |

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

### Telemetry

omp exposes no client-telemetry knob — no environment variable,
no `--disable-*-telemetry` flag, no settings key. The only
telemetry-shaped surface is the OpenTelemetry instrumentation
of the agent's own runs, which is not client analytics. The
bridge therefore adds nothing and the CLI keeps its defaults.

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

### Liveness

`driver.Keepalive` answers the chat layer's "is this bridge
usable right now?" question. It reports the bridge dead when
the child process is gone (`procutil.AlivePID`) or when the RPC
session has already been torn down (`rpcClient.closed`) while
the process lingers — the latter still has a live PID but can
no longer accept a prompt. On a dead bridge it invokes the
caller's recovery callback, which re-spawns omp with the saved
`--resume` session id.

The alive-check and the recovery are serialised inside the
bridge: the `agent.driver` contract says a burst of Submits must
not kick off parallel recoveries and that the caller does not
coordinate. A second caller waits for the first recovery, then
sees the fresh bridge and proceeds.

## 4. Driver contract

`internal/agent` keeps the per-bridge protocol interface
package-private. It has six methods:

| Method | Purpose |
|---|---|
| `SendBlocks` | deliver one user turn |
| `SendPermission` | answer a permission request |
| `Reset` | start a fresh session |
| `Close` | tear the session down |
| `Stop` | abort the in-flight turn |
| `Keepalive` | report liveness; recover when dead |

`agent.NewAgent` is generic over that interface:

```go
func NewAgent[D driver](info Info, pid int, events chan AgentEvent, d D) *Agent
```

so the compiler checks the bridge's driver at the call site:

```go
return agent.NewAgent(s.Info(), d.pid, d.events, d), nil
```

A driver that misses a method fails to build, with the error
pointing at the bridge:

```
internal/bridge/omp/starter.go:81:23: in call to agent.NewAgent,
D (type *driver) does not satisfy agent.driver (missing method Keepalive)
```

The check does not depend on the bridge remembering to assert
anything, and it does not depend on a hand-maintained copy of the
interface staying in sync — a copy is a second thing that can
drift, and a drifted copy silently stops checking the method it
omits. `agent.ImplementsDriver` is the runtime counterpart, for
asserting on a driver obtained by its dynamic type (e.g.
`Agent.Driver()`).

The bridge does not repeat any of this per-agent: the
registry-driven conformance suite in
`cmd/nightme/agentconformance_test.go` walks
`agent.Builtins.List()` and checks every registered agent's
metadata, `Detect()`, path override and spawn path. An agent
registered in `cmd/nightme/agents.go` is covered the moment it
is registered.

### Spawn-path coverage

Two different things are worth covering, and only the second
exercises the runtime contract:

- the wire — `rpc_test.go` and `translate_test.go` drive frames
  through a fake transport;
- the bridge entry point — `Starter.Start` spawns the child,
  runs the handshake, and hands the driver to
  `agent.NewAgent`.

The hand-off lives in `Starter.Start`, so a test that speaks
the protocol directly does not reach it. The bridge entry point
is covered against the real omp binary: the test confirms omp
is installed, skips when it is not, and then drives the path a
chat session takes.

## 5. Translator (`translate.go`)

Translation rules match issue #443 §Spec §5/6/7/8. Highlights:

- `ready` is consumed by the boot handshake, not the translator
  (readPump discards it).
- `message_start` (any role): no event.
- `message_end{assistant}`: stamp `lastModel` and `lastUsage`
  from `message.usage` — usage is an overwriting snapshot, not a
  sum (each API call's `input` already contains the conversation
  history). `stopReason` and `errorMessage` are stamped too —
  `stopReason` rides on every turn's `Result.Subtype`, and
  `errorMessage` only on a failed one (see "Provider errors"
  below).
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

### Provider errors

omp reports an upstream model failure on the assistant
`message_end`: `stopReason` is `"error"`, `errorMessage`
carries the provider's own text, `content` is empty, and the
usage block is zeroed. The translator surfaces that reason on
the turn's `EventAgentResult` (`Result.Subtype` carries the
`stopReason` on every turn, error or not — it is categorisation
only):

- `Result.Err` wraps `ErrModelError` and carries the
  provider's message.
- `Result.Text` falls back to the provider's message when the
  turn produced no assistant text — a body is what makes the
  reason visible in the chat.

A failed turn that already streamed assistant text keeps that
text as the result body; the error rides on `Err` alone.

## 6. Modes

`Info().Mode == agent.ModeJSONIO` — `omp --mode rpc` is LF-delimited
JSON over real stdio pipes (no JSON-RPC envelope), matching
`ModeJSONIO`'s "NDJSON / JSON events over stdio" definition. pi
uses the same mode.

## 7. One-shot turn

`RunOnce` delegates to the shared `agent.RunSession`
(`internal/agent/run_session.go`): `Start` → `SendBlocks` → drain
→ `Close`. The prompt travels over omp's JSONL pipe, so it is not
bounded by the platform's argv limit (Linux caps one argv string
at 131072 bytes, Windows caps the whole command line at 32767
characters).

The turn ends on the same terminator the chat path uses:
`agent_end{isTerminal:true}`.

If the process exits without `agent_end{isTerminal:true}`, the
error message contains the agent name
(`omp: exit without agent_end isTerminal=true...`) so /gtw
commit's recovery logic can attribute the failure correctly.

A provider failure surfaces the same way: the translator marks
the result event with `Err` (§5), and the drain reports that error
instead of handing the provider's message back as the run's text —
callers parse `RunResult.Text` as the agent's own output.

## 8. Out of scope

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

## 9. Tests

- `translate_test.go` — translation table coverage (R5 full
  table; R6 delta dispatch; R7 usage overwrite-not-sum; R8
  single-terminal invariant; unknown events do not break
  buffering; `agent_end{isTerminal:false}` is ignored;
  `extension_ui_request` does not break the turn; provider
  errors surface on the result's `Err` / `Subtype` / body).
- `rpc_test.go` — JSONL framing + request/response correlation
  (writeLine atomicity; round-trip; timeout; fail-pending;
  write-after-close; cancel-response; race-free registration;
  `MaxFrameSize >= 4 MiB` invariant).
- `starter_test.go` — Info / Detect / Init surfaces.
- `driver_test.go` — the driver satisfies the package-private
  `agent.driver` interface; `Keepalive` liveness / recovery
  paths (live process, dead process, torn-down RPC session,
  missing recovery callback).
- `cmd/nightme/agentconformance_test.go` — the registry-driven
  suite every registered agent is covered by (§4).
- `probe_unix_test.go` (build tag `!windows`) — the bridge
  entry point and the raw wire. Every case confirms omp is
  installed and skips when it is not.
  `TestProbe_StarterStartDrivesRealOmp` drives
  `Starter.Start` → handshake → `EventAgentReady`.
  `TestProbe_BridgeTurnRoundTrip` drives a full turn (boot →
  get_state → prompt → agent_end{isTerminal:true}) and checks
  the events the bridge produces.
  `TestProbe_RealOmpRoundTrip` drives the raw protocol
  end-to-end.

## 10. Configuration

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
