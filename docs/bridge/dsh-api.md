# dsh — DeepSeek Harness HTTP/WS Wire Protocol Reference

> **Source of truth**: `@deepseek-ai/dsh-host-apiproxy` 0.1.0-rc.6 (TypeScript contracts) — vendored at `~/.nvm/.../@deepseek-ai/dsh/node_modules/@deepseek-ai/dsh-host-apiproxy/lib/types/`. All shapes below are mirrored verbatim from the `.d.ts` files there.
>
> **Bridge cross-check**: `internal/bridge/dsh/{protocol.go,http.go,ws.go,session.go,handle_mux.go,permissions.go,dispatch.go}` — Go mirrors of the TS contracts. Where the Go bridge and the TS contract diverge, the TS contract wins (this doc tracks the wire; a `❌ BRIDGE BUG` tag marks places where the bridge code does not yet match).
>
> **Runtime cross-check**: probed `dsh 0.1.0-rc.6 --profile web` on 2026-08-15 against the live server (`docs/probe/dsh-2026-08-15.sh` referenced in commit notes). Field-level probes verified `session.list.items`, `session.create.sessionId`, mux event envelope shape, mux `approval/requested.approvalId`.
>
> **Status**: ✅ fully reverse-engineered from authoritative source + 实机 verified.
>
> **Companion docs**:
>   - [docs/bridge/dsh.md](./dsh.md) — bridge design rationale, integration lifecycle, memory principles
>   - [docs/bridge/cli-transport.md](./cli-transport.md) — generic pipe / lifecycle rules

---

## 0. Topology at a Glance

```
nightme (Go bridge)                       dsh web (Node daemon, v0.1.0-rc.6)
┌──────────────────────────────┐         ┌────────────────────────────┐
│ cmd: dsh --profile web       │         │  HTTP :PORT (--port 0 →   │
│   --port 0                   │────────▶│   OS-assigned free port)  │
│   DSH_PERMISSION_MODE=...    │         │                            │
└──────────────────────────────┘         │  /api/{dotted.method}      │
            │                            │    POST  → RPC envelope    │
            │                            │    GET   → SSE stream OR   │
            │                            │           WS upgrade        │
            ▼                            │  /api/events.mux            │
   parseWebURL()                           │    GET → SSE / WS upgrade │
            │                              │  /api/events.host          │
            ▼                              │    GET → SSE / WS upgrade │
            │                              │  /api/respond              │
            ▼                              │    POST → client-response  │
   dials SSE (text/event-stream)            │            envelope        │
     OR WebSocket upgrade                  │  /api/session.export       │
     for both streams                       │    GET → ZIP download      │
            │                              └────────────────────────────┘
            ▼                                            ▲
                                HTTP RPC ─────────────────┘
                          (POST /api/session.create, session.prompt,
                           session.cancel, session.fork, session.list, …)
```

Four wire surfaces, all on the same port:

| Surface | Direction | Encoding | Purpose |
|---|---|---|---|
| **HTTP RPC** | client → server | `POST /api/{dotted.method}` JSON envelope | every business call (50+ methods across 11 domains) |
| **Mux stream** | server → client | `GET /api/events.mux` (SSE **or** WebSocket upgrade) | session/event, session/subscribed, session/projection, session/queue, session/jobs, approval/\*, question/\*, stream/error |
| **Host stream** | server → client | `GET /api/events.host` (SSE **or** WebSocket upgrade) | session lifecycle, workspace changes, archived-sessions, agent errors, forwarded remote events |
| **Client-response** | client → server | `POST /api/respond` JSON envelope (NOT a method) | answer server-initiated approval/question requests |
| **Downloads** | client → server | `GET /api/session.export?sessionId=...` (no envelope) | session log ZIP (host-only, absent from `IApiClient`) |

The contract has a **four-quadrant message model** (`rpc.d.ts`): physical carriers (HTTP / WebSocket / in-process SSE) are decoupled from logical messages (`ClientRequest` / `ServerResponse` / `ServerRequest` / `ClientResponse`). The browser `IApiClient` consumes the same domain methods regardless of carrier.

---

## 1. Envelope Conventions

dsh's wire uses **four** envelope shapes — one per message direction.

### 1.1 `ClientRequest` — every outbound RPC POST

```ts
interface ClientRequest {
    type: 'client-request';
    rpcId: RpcId;
    method: string;
    payload: unknown;       // second-level parse per method
}
```

| Field | Type | Required | Notes |
|---|---|---|---|
| `type` | `'client-request'` | ✅ | literal; server validates against `clientRequestSchema` |
| `rpcId` | branded string | ✅ | client-minted UUID v4 (see §1.5) |
| `method` | string | ✅ | dotted method name (e.g. `"session.prompt"`) — **also appears in the URL** as `POST /api/{method}` |
| `payload` | method-specific | ✅ | second parse dispatched by `method` |

**Real failure observed**: `POST /api/session.list` body `{"rpcId":"x","payload":{}}` → `result.ok=false, error.message="expected type: client-request"`. The `type` field is load-bearing.

### 1.2 `ServerResponse` — what `/api/{method}` returns

```ts
interface ServerResponse {
    type: 'server-response';
    rpcId: RpcId;
    result: RpcResult<unknown>;   // second-level parse per method
}

type RpcResult<T> = { ok: true; value: T } | { ok: false; error: RpcError };
```

| Field | Type | Required | Notes |
|---|---|---|---|
| `type` | `'server-response'` | ✅ | literal |
| `rpcId` | branded string | ✅ | echoes request's `rpcId` — server uses parallel id map; mismatch ⇒ stale/proxy corruption |
| `result.ok` | bool | ✅ | success/failure discriminator |
| `result.value` | method-specific | when `ok:true` | second parse per method |
| `result.error` | `RpcError` | when `ok:false` | discriminated union keyed on `code` (§7) |

**HTTP status codes describe only the carrier**, not the business: `200` is normal even on business error (errors ride inside `result.error`). Non-200 = transport / framework failure:
- `404` — unknown method path
- `415` — non-JSON content-type
- `400` — non-JSON body
- `500` — handler crash
- `403` — cross-origin write fence (CORS preflight failure)

### 1.3 `ServerRequest` — every server-pushed frame on mux / host streams

```ts
interface ServerRequest {
    type: 'server-request';
    rpcId: RpcId;
    method: string;        // mux/host frame discriminator
    payload: unknown;      // typed by method (MuxFrame | HostFrame)
}
```

Two flavours of server-request:

| Stable rpcId (answerable) | Fresh rpcId (pure push) |
|---|---|
| `approval/requested`, `question/requested` | `session/event`, `session/projection`, `session/subscribed`, `session/queue`, `session/jobs`, `approval/resolved`, `question/resolved`, host-stream frames |

**Answerable frames**: rpcId is stable across replay (refresh-recovery replays the same frame with the same rpcId). The client responds via `POST /api/respond` carrying a `ClientResponse` envelope that echoes this rpcId.

**Pure pushes**: rpcId identifies that one push; replay mints a new one.

### 1.4 `ClientResponse` — the answer to an answerable server-request

```ts
interface ClientResponse {
    type: 'client-response';
    rpcId: RpcId;                // echoes the server-request's rpcId; NEVER minted anew
    result: RpcResult<unknown>;  // { ok: true, value: ApprovalResponsePayload | QuestionResponsePayload }
}
```

Sent on `POST /api/respond` (NOT `POST /api/respond.something`). The `method` field is **absent** — `respond` is not a domain method, it's a special envelope path. See §3.7 for the actual payload shapes.

### 1.5 UUID generation

`newRPCID()` mints a v4 UUID via `crypto/rand` + RFC 4122 §4.4 bit twiddling. The first 16 bytes are random; the version (4) and variant (10xx) bits are forced. Fallback when `/dev/urandom` is unavailable: `fmt.Sprintf("%016x", time.Now().UnixNano())` — better than panicking mid-handshake.

---

## 2. HTTP RPC Endpoints

All unary RPCs share the envelope pair (§1.1 request / §1.2 response). URL convention: `POST {baseURL}/api/{method}` — the method appears **both** in the URL path and inside the envelope's `method` field; both must agree (server validates via `clientRequestSchema`).

The full RPC map (`rpc-map.d.ts`, 50+ methods across 11 domains):

| Domain | Methods |
|---|---|
| `sessions.*` | `list`, `search`, `create`, `history`, `models`, `selectModel`, `rename`, `fork`, `prompt`, `attachment`, `updateQueue`, `cancel` (12) |
| `subagents.*` | `list`, `history`, `prompt`, `interrupt` (4) |
| `host.*` | `describe`, `pickDirectory`, `listDirectory`, `createDirectory`, `openPath` (5) |
| `workspace.*` | `list`, `create`, `rename`, `delete`, `insertBefore`, `insertSessionBefore`, `archiveSession` (7) |
| `skills.*` | `list` (1) |
| `agentPresets.*` | `list`, `select`, `read`, `copy`, `openDocument`, `remove` (6) |
| `goals.*` | `create`, `edit`, `pause`, `resume`, `complete`, `clear` (6) |
| `settings.*` | `describe`, `openDocument`, `update`, `replace`, `mutate` (5) |
| `credentials.*` | `describe`, `set`, `unset` (3) |
| `llm.*` | `providers`, `models`, `discoverModels` (3) |

Every method has an optional trailing `AbortSignal` parameter on the wire carrier (never on the wire itself) — bounded calls merge it with the client timeout via `AbortSignal.any`; user-paced calls carry only the external signal.

### 2.1 `session.*` — session lifecycle

#### 2.1.1 `session.list` — list persisted sessions

```ts
list(request: RpcRequest<{ cursor?: string }>): Promise<RpcResponse<{ items: SessionSummary[] }>>;
```

**Request payload**: `{}` or `{ cursor }` (cursor is a reserved seat, **unimplemented in v1** — the server ignores it and returns everything).

**Success value**:

```ts
interface SessionSummary {
    sessionId: SessionId;
    updatedAt: number;                     // unix millis (later of creation + last human prompt)
    running: boolean;                      // false for cold (unattached) sessions
    blank: boolean;                        // no turn/start in checkpoint prefix
    parentSessionId?: SessionId;           // fork/spawn lineage
    origin?: 'subagent';                   // coarse durable origin
    cwd?: string;                          // session working directory
    agentPreset?: string;                  // composed agent preset
    projections?: SessionProjectionsBlock; // projection baseline (see §2.1.3)
}
```

**Wire field naming correction** (verified 2026-08-15 probe vs initial guess):

| Field | Initial guess | Actual |
|---|---|---|
| wrapper field | `sessions` | **`items`** |
| timestamp | `createdAt` | **`updatedAt`** (unix millis) |
| title | top-level | inside `projections.values.title` |

The runtime picker filters `Blank == false && Running == false`, sorts `UpdatedAt DESC`.

#### 2.1.2 `session.search` — search session content

```ts
search(request: RpcRequest<{ query: string }>, signal: AbortSignal): Promise<RpcResponse<{ items: SessionSearchItem[]; hasMore: boolean }>>;
```

Searches current user/assistant/steering message surface across sessions visible to `list`. Returns at most 20 sessions with no continuation cursor; `hasMore` asks the client to refine the query.

```ts
interface SessionSearchItem { sessionId: SessionId; snippet: string; }
```

#### 2.1.3 `session.create` — open a new session

```ts
create(request: RpcRequest<{
    workspaceId?: WorkspaceId;
    cwd?: string;
    sessionId?: SessionId;     // preallocate id; retry returns same session
    agentPreset?: string;
}>): Promise<RpcResponse<{ sessionId: SessionId; agentPreset?: string }>>;
```

| Field | Type | Notes |
|---|---|---|
| `workspaceId` | branded string | optional, **at most one of workspaceId / cwd** |
| `cwd` | string | optional, **at most one of workspaceId / cwd**; omitted uses host cwd |
| `sessionId` | branded string | optional — preallocate id (retries with same id+cwd return same session; different cwd fails with `session-conflict`) |
| `agentPreset` | string | optional — names composition; omitted uses effective default (user's stored choice, else deployment default) |

**Success value**: `{ sessionId, agentPreset? }`. `agentPreset` echoes the resolved preset id (so the caller can settle its UI).

**Errors**:

| `result.error.code` | When |
|---|---|
| `bad-request` | both `workspaceId` and `cwd` set, or neither |
| `session-conflict` | `sessionId` set with different cwd than existing session |
| `workspace-attach-failed` | workspace creation attached session after publication, but attach failed (details carry the published session id) |
| `agent-preset-not-found` | `agentPreset` id unknown (details carry the available list) |
| `agent-preset-invalid` | preset composition cannot be mounted (details carry reason) |

#### 2.1.4 `session.history` — read history pages

```ts
history(request: RpcRequest<{
    sessionId: SessionId;
    beforeSeq?: number;       // page backwards from window tail
    maxMessages?: number;
}>): Promise<RpcResponse<{
    events: HistoryEntry[];
    hasMore: boolean;
    projections?: SessionProjectionsBlock;  // tail page only
}>>;
```

Page boundaries align to **append-origin message boundaries** — one page = all raw events owned by a whole number of messages (including their chunk / tool events), never cut mid-message. Model-only replacement copies consume no `maxMessages`. The tail page (beforeSeq absent) additionally carries the in-flight partial chunk events already emitted for the last unfinalized message, plus `projections` when the deployment mounts the session-projection registry.

```ts
interface HistoryEntry {
    event: SessionEvent;
    view?: ToolEventView;   // host-computed render intent (same semantics as mux frame's `view`)
}

interface SessionProjectionsBlock {
    asOfSeq: number;        // seq of the last event the values reflect (-1 for empty log)
    values: Partial<SessionProjectionMap>;  // whole current value per registered projection key
}
```

#### 2.1.5 `session.models` — read session model directory

```ts
models(request: RpcRequest<{ sessionId: SessionId }>): Promise<RpcResponse<SessionModels>>;

interface SessionModels {
    current: ModelSelection;
    routable: boolean;        // adapter serves current.provider?
    groups: ModelProviderGroup[];
    failures: ModelCatalogFailure[];
}

interface ModelSelection {
    provider: string;          // registered route key
    model: string;             // provider-owned model id
    reasoningEffort?: string;  // adapter-owned (absent preserves default)
}

interface ModelProviderGroup {
    id: string;       // provider route id
    name: string;     // display name
    models: ModelCatalogModel[];  // provider-preferred order
}

interface ModelCatalogModel {
    id: string;
    name: string;
    description?: string;
    reasoning?: ModelReasoning;
}

interface ModelReasoning {
    efforts: ModelReasoningEffort[];
    defaultEffort?: string;
}

interface ModelReasoningEffort {
    id: string;
    name: string;
    description?: string;
}

interface ModelCatalogFailure {
    id: string;
    name: string;
    message: string;   // lookup failure diagnostic
}
```

**Important**: `routable` is NOT derivable from `groups`. Catalog membership is advisory — a route serving a model it stopped advertising is absent from `groups` yet perfectly usable, while a route whose adapter is gone can serve nothing. A surface that blocks input must read `routable`, not `groups`.

Bridge stamps `current.model` onto `EventAgentReady.Model` verbatim (no `provider:model` composite).

#### 2.1.6 `session.selectModel` — change the session's model selection

```ts
selectModel(request: RpcRequest<{
    sessionId: SessionId;
    provider: string;
    model: string;
    reasoningEffort?: string;
}>): Promise<RpcResponse<{ selected: ModelSelection }>>;
```

Exact model metadata validates an optional reasoning effort; catalog membership remains advisory. **Subagent sessions reject with `agent-busy`** — the parent session owns the model selection.

#### 2.1.7 `session.rename` — pin a session title

```ts
rename(request: RpcRequest<{
    sessionId: SessionId;
    title: string;
}>): Promise<RpcResponse<{ title: string; seq: number }>>;
```

Appends a `session/title` event with the `user` source, pinning the title against automatic regeneration. Returns the **normalized accepted title** and the title event's seq so the caller can settle its projection cell without waiting for the push frame. A title that normalizes to empty fails with `title-invalid`. Subagent sessions reject with `agent-busy`.

#### 2.1.8 `session.fork` — fork from a completed turn

```ts
fork(request: RpcRequest<{
    sessionId: SessionId;
    atSeq?: number;          // anchors the cut
}>): Promise<RpcResponse<{ sessionId: SessionId }>>;
```

| Param | Notes |
|---|---|
| `sessionId` | source session to fork from |
| `atSeq` | boundary is the first `turn/end` at or after this seq (a message's fork button passes the message seq, so the fork includes that whole turn). Past log end / omitted → falls back to source's last completed turn. **In-log anchor whose turn is still open fails with `fork-unavailable`** instead of clipping to an earlier turn. |

**Success value**: `{ sessionId }` — the NEW child id. The child inherits source's cwd, latest logged model target, and `parentSessionId` lineage; the seed prefix carries the source title.

**Why fork, not "resume in place"**: dsh routes all session writes through WS/SSE frames keyed on rpcId + sessionId — physically impossible for a new process to inherit an old server's stream.

**Errors**:

| `result.error.code` | When |
|---|---|
| `fork-unavailable` | source has no completed turn (blank) OR `atSeq` is mid-turn |
| `session-not-found` | sessionId does not exist |

**Bridge behaviour**: any failure → `ErrResumeUnhealthy` (strict refusal — see bridge docs §13). Runtime's `chatsession.go §1624` then clears the stale id and starts fresh on the user's next message.

#### 2.1.9 `session.prompt` — submit a turn

```ts
prompt(request: RpcRequest<{
    sessionId: SessionId;
    mode: 'queue' | 'steer';
    content: PromptContentPart[];
    clientTimeZone?: string;
}>): Promise<RpcResponse<{
    accepted: true;
    command?: { kind: 'success'; text?: string };
}>>;
```

| Field | Type | Notes |
|---|---|---|
| `sessionId` | string | the live session |
| `mode` | `'queue' \| 'steer'` | **REQUIRED** — discriminator. Missing → `bad-request: invalid input: expected "queue"`. `queue` appends after in-flight turn; `steer` preempts. |
| `content` | `PromptContentPart[]` | ordered list (see §2.1.9.1) |
| `clientTimeZone` | IANA zone string | optional; browser callers attach their current zone. Host validates + records on that user message. Omission remains valid for non-browser callers. |

**Special: slash-command routing**. A prompt whose content is exactly one text block starting with `/` is a slash command — the host executes it through the command registry (mode-agnostic) and it is never sent to the model. A successful command returns `ok` with the command slot. A usage/state error is `command-error`; an unrecognized name is `unknown-command`.

**Subagent sessions** reject with `agent-busy` and use `subagent.prompt` (§2.2) instead.

**`session.prompt` is inbox-only**: returns fast as soon as the block is enqueued; the model turn happens asynchronously and events arrive on `/api/events.mux`. The bridge awaits `turn/end` via the stream pump; it does not poll.

###### 2.1.9.1 ContentBlock shapes (`content[]`)

```ts
type PromptContentPart =
    | { type: 'text'; text: string }
    | { type: 'image'; mediaType: ImageMediaType; data: string; name?: string };
```

Probe-verified discriminators (docs/bridge/dsh.md §1.4):

| Shape | Result |
|---|---|
| `{type:"text", text:"..."}` | ✅ accepted |
| `{type:"image", mediaType:"image/png", data:"<base64>", name?}` | ✅ accepted (vision direct) |
| `{type:"resource_link", ...}` | ❌ `bad-request: No matching discriminator. Expected 'text'\|'image'` (despite TS union declaring it) |
| `{type:"document", ...}` | ❌ rejected at discriminator |
| empty `content[]` | ❌ rejected — must be non-empty |

**Bridge `contentBlocksToDTO` mapping** (`session.go:1024-1077`):

| `agent.ContentBlock.Type` | Wire shape |
|---|---|
| `ContentText` (non-empty) | `{type:"text", text:b.Text}` |
| `ContentImage` (`image/png`/`jpeg`/`gif`/`webp`) | `{type:"image", mediaType:b.MediaType, data:base64(file), name:basename}` |
| `ContentImage` (other MIME) | text annotation: `"[image: /path (image/heic) — unsupported mediaType, decode locally to view]"` |
| `ContentFile` (any) | text annotation: `"[file: /path]"` (dsh web doesn't accept file references on prompt) |

#### 2.1.10 `session.attachment` — read one durable image

```ts
attachment(request: RpcRequest<{
    sessionId: SessionId;
    attachmentId: AttachmentIdType;
}>): Promise<RpcResponse<{ attachment: ImageAttachmentRef; data: string }>>;
```

Reads one durable image after proving that this session's log references its id. The host promotes inline image bytes to durable references at prompt admission; `attachment` is how a later surface retrieves the canonical bytes.

#### 2.1.11 `session.updateQueue` — edit / remove / steer a queued item

```ts
updateQueue(request: RpcRequest<{
    sessionId: SessionId;
    itemId: MessageId;
    action: QueueAction;
}>): Promise<RpcResponse<{ accepted: true }>>;

type QueueAction =
    | { kind: 'edit'; content: ContentBlock[] }
    | { kind: 'remove' }
    | { kind: 'steer' };
```

Subagent sessions reject with `agent-busy`.

#### 2.1.12 `session.cancel` — cancel in-flight turn

```ts
cancel(request: RpcRequest<{ sessionId: SessionId }>): Promise<RpcResponse<{ accepted: true }>>;
```

Stops the active turn while preserving pending inbox work (resumes FIFO order after cancellation settles). The server emits `turn/end{stopReason: "abort"}` on mux. Subagent sessions reject with `agent-busy` and use `subagent.interrupt` (§2.2).

### 2.2 `subagent.*` — subagent lifecycle

```ts
interface SubagentAddress = {
    parentSessionId: SessionId;
    childSessionId: SessionId;
} & ({ mode: 'one-shot' } | { mode: 'continuable' });
```

#### 2.2.1 `subagent.list` — list direct children

```ts
list(request: RpcRequest<{ parentSessionId: SessionId }>, signal?: AbortSignal): Promise<RpcResponse<SubagentCatalog>>;

interface SubagentCatalog {
    entries: SubagentListEntry[];
    parentAvailable: boolean;
}

type SubagentListEntry =
    | { kind: 'child'; id: SessionId; activity: 'running' | 'inactive'; hasChildren: boolean }
        & ({ mode: 'one-shot'; label?: string } | { mode: 'continuable'; label: string })
    | { kind: 'diagnostic'; id: SessionId; reason: 'corrupt' | 'unsupported' | 'unavailable' };
```

#### 2.2.2 `subagent.history` — read child transcript

```ts
history(request: RpcRequest<SubagentAddress & { beforeSeq?: number; maxMessages?: number }>, signal?: AbortSignal): Promise<RpcResponse<{
    events: HistoryEntry[];
    hasMore: boolean;
    projections?: SessionProjectionsBlock;
}>>;
```

#### 2.2.3 `subagent.prompt` — deliver to continuable child

```ts
prompt(request: RpcRequest<Extract<SubagentAddress, { mode: 'continuable' }> & {
    content: ContentBlock[];
    clientTimeZone?: string;
}>, signal: AbortSignal): Promise<RpcResponse<SubagentPromptReceipt>>;

interface SubagentPromptReceipt { messageId: MessageId; }
```

Delivers human content through the exact live parent's continuation owner. `messageId` identifies the message accepted by the child's FIFO inbox; later execution is independent of this request. Optional `clientTimeZone` is validated and logged on that message.

#### 2.2.4 `subagent.interrupt` — interrupt live continuable child

```ts
interrupt(request: RpcRequest<Extract<SubagentAddress, { mode: 'continuable' }>>): Promise<RpcResponse<SubagentInterruptReceipt>>;

interface SubagentInterruptReceipt { accepted: true; }
```

Under the address's durable direct-parent authority — no live parent Agent required, no catalog consultation. **Fire-and-return**: `accepted` acknowledges the admitted cancel signal, not target quiescence (child may remain visibly running briefly). Unclaimed queued follow-ups are kept and parked; absent / idle / already-completed target likewise returns `accepted`.

### 2.3 `host.*` — host-level operations

#### 2.3.1 `host.describe` — host snapshot

```ts
describe(request: RpcRequest<{}>): Promise<RpcResponse<{
    version: string;
    cwd: string;
    provider?: string;       // defaults applied when new agent doesn't specify explicitly
    model?: string;          // absent when host configures no explicit default
    attachedSessions: number;
    canOpenPath: boolean;    // whether native desktop opener is available
}>>;
```

`version` = the host app's (apps/cli) package.json version. `cwd` = the host process working directory (root for session persistence and tool execution). `attachedSessions` = count of currently attached sessions (live agents).

#### 2.3.2 `host.pickDirectory` — native directory picker

```ts
pickDirectory(request: RpcRequest<{}>, signal: AbortSignal): Promise<RpcResponse<{ path: string | null }>>;
```

Opens the operating system's single-directory picker; cancellation returns `null`. **Only served under the `native` capability.**

#### 2.3.3 `host.listDirectory` — list one directory level

```ts
listDirectory(request: RpcRequest<{ path?: string }>, signal: AbortSignal): Promise<RpcResponse<DirectoryListing>>;

interface DirectoryListing {
    path: string;            // absolute path of listed directory
    home: string;            // host account's home
    crumbs: DirectoryEntry[]; // ancestor chain from fs root
    entries: DirectoryEntry[]; // direct child directories, name-sorted
    truncated: boolean;      // backend cut `entries` at its complete-result bound
}

interface DirectoryEntry {
    name: string;    // base name; root crumb carries its full path
    path: string;    // absolute host path — client never joins path segments itself
    hidden: boolean; // hidden by host platform convention
}
```

Absent `path` lists the host account's home directory. **Only served under the `browse` capability**; unreadable or missing targets fail with `directory-unreadable`.

#### 2.3.4 `host.createDirectory` — create child directory

```ts
createDirectory(request: RpcRequest<{ path: string; name: string }>): Promise<RpcResponse<{ path: string }>>;
```

Under an existing parent (the browser's "New folder"). Existing child → `directory-exists`; other fs failures → `directory-create-failed`.

#### 2.3.5 `host.openPath` — open path with OS default app

```ts
openPath(request: RpcRequest<{ path: string }>, signal: AbortSignal): Promise<RpcResponse<{ opened: true }>>;
```

Hands off to Finder / Explorer / xdg-open. The browser carrier's prefix-wide trust fence covers this like every other `/api` request.

### 2.4 `workspace.*` — workspace registry

A `Workspace` is a stable id over a directory path, a display title, and an ordered session account.

```ts
type WorkspaceId = Branded<'WorkspaceId'>;

interface WorkspaceView {
    workspaceId: WorkspaceId;
    path: string;            // canonical realpath
    title: string;           // display title (defaults to path basename at create)
    sessionIds: SessionId[]; // ordered (attach prepends, insertSessionBefore reorders; activity never)
    createdAt: string;       // ISO-8601
    updatedAt: string;       // ISO-8601
}
```

#### 2.4.1 `workspace.list`

```ts
list(request: RpcRequest<{}>): Promise<RpcResponse<{ items: WorkspaceView[]; archivedSessionIds: SessionId[] }>>;
```

All workspaces in durable display order, plus the registry-global archive set (the reconnect baseline of `host/archived-sessions-changed`).

#### 2.4.2 `workspace.create`

```ts
create(request: RpcRequest<{ path: string }>): Promise<RpcResponse<{ workspace: WorkspaceView; created: boolean }>>;
```

Idempotent over an EXISTING directory (no mkdir). Missing or non-directory → `workspace-invalid-path`. Already-owned path returns that workspace (`created: false`).

#### 2.4.3 `workspace.rename`

`title` trimmed, must be non-empty. Unknown id → `workspace-not-found`; title equal to another's → `workspace-name-conflict`. Renaming to current title is a no-op success.

#### 2.4.4 `workspace.delete`

Removes one workspace registration. Directory, every user file, every session log remain untouched; those Sessions become ungrouped. Unknown id → `workspace-not-found`.

#### 2.4.5 `workspace.insertBefore`

```ts
insertBefore(request: RpcRequest<{ workspaceId: WorkspaceId; beforeWorkspaceId?: WorkspaceId }>): Promise<RpcResponse<{ workspaceIds: WorkspaceId[] }>>;
```

DOM-insertBefore-like reordering. Omitted anchor appends to end.

#### 2.4.6 `workspace.insertSessionBefore`

```ts
insertSessionBefore(request: RpcRequest<{ workspaceId: WorkspaceId; sessionId: SessionId; beforeSessionId?: SessionId }>): Promise<RpcResponse<{ workspace: WorkspaceView }>>;
```

Same DOM-style ordering within the workspace's manual order. Unknown workspace → `workspace-not-found`; session/anchor not accounted → `workspace-move-invalid`.

#### 2.4.7 `workspace.archiveSession`

```ts
archiveSession(request: RpcRequest<{ sessionId: SessionId }>): Promise<RpcResponse<{ archivedSessionIds: SessionId[] }>>;
```

Adds one session to the registry-global archive set. Disappears from every grouping surface but keeps its session log and its workspace accounting slot. Idempotent for already-archived id. Session neither live nor in session persistence → `session-not-found`.

### 2.5 `skill.*` — read-only skill catalog

```ts
skill.list(request: RpcRequest<{ sessionId: SessionId }>): Promise<RpcResponse<{ skills: readonly SkillEntry[] }>>;

interface SkillEntry {
    readonly name: string;             // kebab-case `/name` reference
    readonly description: string;     // short routing description
    readonly whenToUse?: string;      // optional extra routing guidance
    readonly modelInvocable: boolean; // false marks user-only skill
}
```

Read-only listing by session cwd → canonical project root (host-side). **Invocation is NOT a dedicated RPC** — it's a plain `session.prompt` whose leading `/name` token the host recognizes at the pre-step boundary.

### 2.6 `agentPresets.*` — preset roster + authoring

```ts
interface AgentPresetEntry {
    readonly id: string;
    readonly trust: 'system' | 'user';   // 'user' = locally authored
    readonly isDefault: boolean;         // whether session-with-no-preset gets this one
    readonly name?: string;              // display name; fallback to id
    readonly description?: string;
    readonly broken?: string;            // why cannot compose, when applicable
}
```

| Method | Behaviour |
|---|---|
| `agentPreset.list(request: RpcRequest<{}>)` | All presets in root-precedence order. Returns `{ presets, authorable, hasDocument }`. |
| `agentPreset.select(request: RpcRequest<{sessionId; agentPreset}>)` | Recompose one session's agent from a different preset. **Only allowed while session is blank** — once a conversation starts, its history was produced under that preset's tools and the attempt returns `agent-preset-locked`. |
| `agentPreset.read(request: RpcRequest<{agentPreset}>)` | Read composition text (privileged — reconnaissance). |
| `agentPreset.copy(request: RpcRequest<{from; agentPreset; name?}>)` | Create locally authored preset by copying an existing one whole. **The only authoring write** — no composition text or path crosses the wire; `from`/`agentPreset` are ids the Host resolves against its own roots. |
| `agentPreset.openDocument(request: RpcRequest<{agentPreset}>, signal)` | Hand locally authored preset's directory to platform opener. Shipped presets refused. Where no native opener (`hasDocument: false`), reply carries the resolved directory path for the surface to show as text. |
| `agentPreset.remove(request: RpcRequest<{agentPreset}>)` | Delete locally authored preset. Shipped presets refused. |

### 2.7 `goals.*` — goal lifecycle (mutations only; reads are session projections)

```ts
type GoalId = Branded<'GoalId'>;
interface GoalRef { id: GoalId; revision: number; }
```

Every mutation resolves an ordinary session's Agent and applies one CAS-guarded verb. Subagent sessions reject with `agent-busy`.

| Method | Payload | Returns |
|---|---|---|
| `goal.create` | `{sessionId, objective, maxGoalRounds?}` | `{ref}` |
| `goal.edit` | `{sessionId, ref, objective?, maxGoalRounds?}` | `{ref}` |
| `goal.pause` | `{sessionId, ref}` | `{ref}` |
| `goal.resume` | `{sessionId, ref}` | `{ref}` |
| `goal.complete` | `{sessionId, ref}` | `{ref}` |
| `goal.clear` | `{sessionId, ref}` | `{cleared: true}` |

Read side = `goal` session projection (history tail-page `projections` block + `session/projection` frames). No wire `goal.get` / `goal.*` view.

### 2.8 `settings.*` — configuration namespaces (redacted)

Every payload leaving this domain is redacted by the seam: `role('secret')` fields never ride a response in any layer.

```ts
interface SettingsNamespaceView {
    ns: string;                  // namespace key (`llm-deepseek`, …)
    schema: unknown;             // schemastery schema envelope; rehydrate with `new Schema(json)`
    value: unknown;              // redacted resolved value (schema defaults → composition base → user layer)
    base?: unknown;              // redacted composition base layer
    user?: unknown;              // redacted raw user section (presence here marks user-overridden)
    applies: 'live' | 'restart'; // when owner applies changes
    secrets: SettingsSecretView[]; // schema-declared secret slots with `set` state
    revision: number;             // monotonic — pass back as `expectedRevision` on writes
}

interface SettingsSecretView {
    path: string[];   // path from section root to the removed field
    set: boolean;     // whether the slot currently holds a value (value itself never rides)
}

type SettingsPathOpView =
    | { op: 'set'; path: string[]; value: unknown }
    | { op: 'unset'; path: string[] };
```

| Method | Payload | Returns |
|---|---|---|
| `settings.describe` | `{}` | `{ writable, hasDocument, namespaces: SettingsNamespaceView[] }`. Loopback-only. |
| `settings.openDocument` | `{}` | `{ opened: true }` (or `{ opened: false, path }` if no native opener). Materializes configured local document when absent and hands to platform text-document opener. No path on the wire. |
| `settings.update` | `{ns, patch, expectedRevision?}` | `SettingsNamespaceView` — merge patch into user layer (validate → persist → commit). |
| `settings.replace` | `{ns, section, expectedRevision?}` | `SettingsNamespaceView` — wholesale replacement (reset path: `section: {}` resets to composition defaults). |
| `settings.mutate` | `{ns, ops: SettingsPathOpView[], expectedRevision?}` | `SettingsNamespaceView` — path-addressed edits resolved against STORED section (not caller-read view). |

Errors: `settings-rejected` (validation/storage), `settings-not-exposed` (outside configuration plane's model-provider boundary), `settings-conflict` (stale `expectedRevision`).

### 2.9 `credentials.*` — credential reference seam

```ts
interface CredentialView {
    configured: boolean;        // any layer currently supplies a non-empty value
    source?: string;            // winning layer (`env`, `file`, …) when configured
    writable: boolean;          // whether set/unset can affect this reference
}
```

| Method | Payload | Returns |
|---|---|---|
| `credentials.describe` | `{refs: string[]}` | `{credentials: Record<string, CredentialView>}` (batch). Invalid name → `bad-request`; unknown-but-valid one describes as unconfigured. |
| `credentials.set` | `{ref, value}` | `{}`. Rejected with `credential-rejected` when read-only shadowing layer (live env) shadows the reference. |
| `credentials.unset` | `{ref}` | `{}`. Same shadowing rejection. Idempotent for absent reference. |

Values ride the wire in exactly one direction: inside `credentials.set`. No enumeration method by design — clients learn which references exist from settings schemas/values (`apiKeyEnv` fields).

### 2.10 `llm.*` — host-scoped provider topology

```ts
interface ConfigurableProviderView {
    provider: string;          // route key (`deepseek-official`, `openai`, …)
    displayName: string;       // human-readable for config surfaces
    settingsNs: string;        // settings namespace configuring this provider
    settingsPath: string[];    // path from section root to provider profile (empty = whole section)
    active: boolean;           // whether route is currently registered (requestable)
    declared?: boolean;        // absent when adapter draws no such distinction
}

interface DiscoveredModelView {
    id: string;
    name?: string;
    contextWindow?: number;
    maxTokens?: number;
}
```

| Method | Returns |
|---|---|
| `llm.providers` | `{ providers: ConfigurableProviderView[] }` in directory declaration order |
| `llm.models` | `{ groups: ModelProviderGroup[], failures: ModelCatalogFailure[] }` — session-independent catalog (same as `session.models.groups` minus the per-session selection) |
| `llm.discoverModels({settingsNs, provider?, baseURL?, api?, apiKey?}, signal?)` | `{ models: DiscoveredModelView[] }`. Interrogates a draft endpoint, returns models to adopt. **`apiKey` is accepted but NEVER stored or returned**; a provider whose key is already stored omits it and the endpoint answers unauthenticated or refuses. |

Clients invalidate from forwarded `llm/adapters-updated` and `settings/document-updated` owner events.

### 2.11 `downloads.*` — host-only download surfaces (GET, no envelope)

```ts
interface DownloadsApi {
    sessionLog(request: { sessionId: SessionId; includeDescendants?: boolean }, signal: AbortSignal): Promise<Response>;
}
```

`GET /api/session.export?sessionId=...&includeDescendants=...` streams a session-log ZIP — root artifact verbatim plus each subagent descendant's — as an attachment response. The browser `IApiClient` never exposes this (host-only).

### 2.12 `respond` — answer a server-initiated approval/question

This is the **out-of-band** RPC used to reply to `approval/requested` or `question/requested` frames on `/api/events.mux`. It's **NOT a method** in the RPC map; the URL path is just `/api/respond` and the envelope is `type: "client-response"` (§1.4).

**Request envelope** (`POST /api/respond`):

```json
{
  "type": "client-response",
  "rpcId": "<echoes server-frame's rpcId>",
  "result": {
    "ok": true,
    "value": {
      "sessionId": "session-...",
      "approvalId": "<for approval>",
      "outcome": "allowed-once"
    }
  }
}
```

For questions, `value` is the `QuestionResponsePayload` shape:

```json
{
  "type": "client-response",
  "rpcId": "<echoes server-frame's rpcId>",
  "result": {
    "ok": true,
    "value": {
      "sessionId": "session-...",
      "answer": {
        "answers": [
          { "id": "q1", "selected": ["PostgreSQL"], "custom": null }
        ]
      }
    }
  }
}
```

**HTTP response body is an `RpcReceipt` carrier receipt** (NOT a `ServerResponse`):

```json
{ "accepted": true }
// or
{ "accepted": false, "reason": "not-pending" }      // stale/duplicate
{ "accepted": false, "reason": "bad-response" }    // payload parse failed
```

#### 2.12.1 Approval response value

```ts
interface ApprovalResponsePayload {
    sessionId: SessionId;
    approvalId: ApprovalRequestId;        // core audit correlation
    outcome: 'allowed-once' | 'rejected';  // ONLY these two are client-giveable
}
```

Closed `ApprovalOutcome` vocabulary (`dsh-user-approval/types.d.ts`):

```ts
type ApprovalOutcome = 'allowed-once' | 'rejected' | 'cancelled' | 'unavailable';
//                                                  ↑
//                                            these are HOST-side outcomes
```

#### 2.12.2 Question response value

```ts
interface QuestionResponsePayload {
    sessionId: SessionId;
    answer: AskUserQuestionAnswer;          // { answers: AskUserQuestionAnswerItem[] }
}

interface AskUserQuestionAnswer {
    answers: AskUserQuestionAnswerItem[];
}

interface AskUserQuestionAnswerItem {
    id: string;                  // answered question id (echoes AskUserQuestionItem.id)
    selected: string[];          // selected option labels
    custom?: string;             // optional "Other" free-text
}
```

#### 2.12.3 Routing invariants

- **Wire correlation = echoed `rpcId`**. The server uses the `rpcId` of the originating server-request frame as the pending-table key.
- **`approvalId` is audit-only** — it's the core correlation id used by the impl to reconcile `approval/asked`/`decided` events; the wire correlation is governed entirely by the echoed `rpcId`.
- **`sessionId` IS required in the response value** (despite not being on the wire for the request frame) — the server uses it as the second correlation key.

❌ **BRIDGE BUG** (`session.go:864-886`, `SendPermission`): the current Go bridge sends `type:"client-request", method:"respond", payload:{rpcId, payload:{outcome}}` instead of `type:"client-response", rpcId, result:{ok, value:{sessionId, approvalId, outcome}}`. The server accepts the legacy shape because dsh 0.1.0-rc.6 happens to recognize both forms, but the canonical wire (per `rpc.d.ts`) is `client-response`. Tracked for fix in next bridge revision.

---

## 3. Mux Stream (`/api/events.mux`)

Server pushes session-scoped frames. **Dual transport**: SSE or WebSocket upgrade — both work (verified 2026-08-15; `sseResponse(...)` in `handler.js` is the canonical impl, and `registerUpgrade(...)` in `webserver/index.js` is the WS path). The client chooses at dial time.

### 3.1 SSE form

```
GET /api/events.mux
Accept: text/event-stream

HTTP/1.1 200 OK
Content-Type: text/event-stream

: connected\n\n                                  ← SSE comment on open (probe / proxy liveness)
data: {"type":"server-request","rpcId":"...","method":"session/subscribed","payload":{...}}\n\n
data: {"type":"server-request","rpcId":"...","method":"session/event","payload":{...}}\n\n
...
```

Frame format: `data: {fullFrame}\n\n` where `fullFrame = serverFrame + method-discriminated payload`. Each `data:` line is one full server-request envelope (the format IS `ServerRequest`, not just the method-specific payload). Frames are NOT split across `data:` lines — the bridge decodes each line as one envelope.

A single `: connected\n\n` SSE comment is sent on stream open so proxies/clients see a live channel. The host stream has no baseline frames and would otherwise emit zero bytes while idle; the comment line is not a frame so client frame parsing skips it naturally.

### 3.2 WebSocket form

```
GET /api/events.mux
Connection: Upgrade
Upgrade: websocket
Sec-WebSocket-Version: 13
Sec-WebSocket-Key: <base64>

HTTP/1.1 101 Switching Protocols
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Accept: <base64>

<binary or text frames, each one full ServerRequest JSON envelope>
```

The bridge dials WS (`ws.go:dialWS`) because it was the path of least surprise for gorilla/websocket; SSE is equally valid and would let the bridge drop the gorilla/websocket dependency.

### 3.3 Frame envelope

Every pushed frame is a `ServerRequest` (§1.3):

```json
{
  "type":   "server-request",
  "rpcId":  "<uuid>",
  "method": "<mux method>",
  "payload": { ... method-specific ... }
}
```

### 3.4 `MuxFrame` types (events.d.ts, exhaustive union)

#### 3.4.1 `session/subscribed` — baseline frame on stream open

```json
{
  "type": "server-request",
  "rpcId": "<stable>",
  "method": "session/subscribed",
  "payload": { "sessionId": "session-...", "lastSeq": 0 }
}
```

First frame after stream open for **every attached session**. Replay replays each session's still-pending approval/question requested frames with rpcId reused verbatim — the refresh-recovery baseline. The bridge records `lastSeq` as the resume marker (used by `session.history?sinceSeq` on reconnect — `since` resume hook is **unimplemented in v1**; reconnection = reopen the stream + refetch history).

#### 3.4.2 `session/event` — the main event stream

```json
{
  "type": "server-request",
  "rpcId": "<fresh>",
  "method": "session/event",
  "payload": {
    "sessionId": "session-...",
    "event":     { "type": "...", "data": { ... } },
    "view":      { /* ToolEventView — optional, host-computed snapshot */ }
  }
}
```

`event` is a `SessionEvent` (merge-extensible; strict envelope `{type, seq, time}` + wide data). The 11 types the bridge currently decodes (registered in `dispatch.go:standardRegistry`):

| `event.type` | Bridge emits |
|---|---|
| `assistant/chunk` | (accumulate `textBuf[idx]` — streaming delta) |
| `assistant/message` | (flush `pendingText` → `EventAgentText`) |
| `tool/call` | `EventToolStart{ID, Name, Args}` + record `pendingTools[id]` |
| `tool/result` | `EventToolEnd{ID, Name, Args, Output, Err}` (back-fills Name/Args from pendingTools) |
| `turn/start` | (clear turnState; align with pi F-32). Does **not** emit OutTask*. Host also pushes `session/projection{key:"todos", value:null}` — see §3.4.3. |
| `turn/end` | `EventResult{Usage} + EventDone{Reason: "settled"}` |
| `compaction/end` | `EventCompaction` |
| `todo/write` | `EventAgentTaskCreate` (snapshot). Dashboard **To-dos / 任务** strip. Payload `{todos:[{content,status}]}`. |
| `todo/update` | `EventAgentTaskUpdate` (P3+; currently no-op) |
| `todo/delete` | (P3+; currently no-op) |
| `step/start` / `step/end` | (no AgentEvent). Inference-cycle bounds `{turn,step}` for `sessionStats` (TTFT / tok/s). **Not** the To-dos strip. |
| `approval/asked` | `EventAgentPermission` via `handleInlineApproval` (synthesised id `"evt-" + toolCallId`) |

`view` (optional) is the host-computed render intent:

```ts
type ToolEventView =
    | { for: 'call';   view: ToolCallView }
    | { for: 'result'; view: ToolResultView };
```

When present, `view` is the **authoritative** source for tool status (P3 contract).

#### 3.4.3 `session/projection` — host-computed derived state

```json
{
  "type": "server-request",
  "rpcId": "<fresh>",
  "method": "session/projection",
  "payload": {
    "sessionId": "session-...",
    "key":       "todos" | "title" | "sessionStats" | "goal" | ...,   // ⚠ NOT "projection"
    "value":     /* unit-specific — see table */,
    "seq":       42
  }
}
```

| Wire field | Notes |
|---|---|
| `key` | projection unit name. Canonical wire is `key` (not `projection`). Bridge `protocol.go:projectionEnvelope` uses `json:"key"` (fixed). |
| `value` for `key:"todos"` | `TodoItem[] \| null` — **the array itself**, not `{todos:[...]}`. Declared by `@deepseek-ai/dsh-tool-todo` `SessionProjectionMap.todos`. Captured: `internal/bridge/dsh/testdata/projections/todo_snapshot.json`. |
| `value` for other keys | unit-specific objects (`title` string, `sessionStats` counters, …). |

Live push state, never logged — replay recomputes on the host. Dashboard seeds from the history tail page's `projections` block (`{asOfSeq, values}`), then applies higher-seq `session/projection` frames.

**Dashboard To-dos / 任务** (`TodoDock` in `conversation.input.dock`; locale `todo.title` = "To-dos" / "任务"):

| Surface | Maps to | Does not map to |
|---|---|---|
| Input-dock plan strip (`TodoDock` → `useProjection("todos")`) | live `todo/write` `{todos:[{content,status}]}` (tool `todo_write`); host fold `backscanTodos` = latest write until a later `turn/start` (that emit is `value: null`, which hides the strip) | `step/start` / `step/end` |
| Chat tool row `todo_write(...)` | `tool/call` / `tool/result` | the dock list |
| TTFT / tok/s (`StatsLine`) | `step/start` / `step/end` `{turn,step}` folded into `sessionStats` | TodoPanel |

Bridge today: live `todo/write` → `EventAgentTaskCreate` (works). `step/*` is registered and emits nothing. `session/projection{key:"todos"}` is routed to `applyTodoProjectionLocked`, which still unmarshals an **object** `{todos\|items}`; a real array fails decode and is dropped. JSON `null` unmarshals as a zero struct and still emits `EventAgentTaskCreate{Items:[]}` (Feishu treats empty Items as “clear checklist”), which undoes `handleTurnStart`’s keep-last-snapshot comment. Attach/`session.history` does not apply the tail `projections` block (`observeHistory` only walks `events`).

#### 3.4.4 `session/queue` — input queue snapshot

```json
{
  "method": "session/queue",
  "payload": { "sessionId": "session-...", "items": [QueuedInboxItem, ...] }
}

interface QueuedInboxItem {
    id: MessageId;
    placement: 'queued' | 'steering' | 'context';
    message: Message;       // complete pending message
}
```

Authoritative snapshot after every enqueue, mutation, claim, or discard. Pending work is not model-visible and has no durable event.

#### 3.4.5 `session/jobs` — background jobs

```json
{
  "method": "session/jobs",
  "payload": { "sessionId": "session-...", "jobs": [JobView, ...] }
}

interface JobView {
    id: JobId;
    kind: string;            // open string — producer plugins extend
    label: string;           // producer-supplied one-line label
    status: 'running' | 'stopping' | 'completed' | 'killed' | 'failed';
    detail?: string;
    startedAt: number;       // epoch ms
    finishedAt?: number;     // absent while live
}
```

Sent as subscription baseline only for sessions with tasks. Empty `[]` still sent (absence cannot express the "transitioned to empty" event).

#### 3.4.6 `approval/requested` — server-pushed permission gate

```json
{
  "type": "server-request",
  "rpcId": "<stable>",
  "method": "approval/requested",
  "payload": {
    "sessionId":  "session-...",
    "approvalId": "<Branded<ApprovalRequestId>>",  // audit correlation only
    "toolName":   "Bash",
    "callId":     "toolu_xxx",                       // optional
    "reason":     "..."                              // optional
  }
}
```

Bridge emits `EventAgentPermission{Tool: toolName, Action: reason, Options: ["approve","decline"], ResponseCh: respCh}`. Response channel is registered in `pendingApprovals[frame.RPCID]`. Reply via `POST /api/respond` with `client-response` envelope (§2.12).

#### 3.4.7 `approval/resolved` — audit frame

```json
{
  "method": "approval/resolved",
  "payload": {
    "sessionId": "session-...",
    "approvalId": "<id>",
    "outcome": "allowed-once" | "rejected" | "cancelled' | 'unavailable'"
  }
}
```

Pure push; debug log only. Confirms our `respond` POST was received and acted on by the server.

#### 3.4.8 `approval/asked` — model-side approval (alt path)

Synthesised routing key: `"evt-" + toolCallID` (`permissions.go:112-130`). Same plumbing as `approval/requested`. Distinct from `approval/requested`: this one comes through the mux method-level dispatch rather than as a `session/event` envelope.

#### 3.4.9 `question/requested` — AskUserQuestion-style prompt

```json
{
  "type": "server-request",
  "rpcId": "<stable>",
  "method": "question/requested",
  "payload": {
    "sessionId": "session-...",
    "questions": [
      {
        "id":          "q1",
        "question":    "Which database?",
        "header":      "Setup",
        "detail":      "...",
        "options":     [                              // ⚠ array of OBJECTS, not strings
          { "label": "PostgreSQL", "description": "Production-ready" },
          { "label": "SQLite",     "description": "Dev / small project" }
        ],
        "multiSelect": false,
        "intent":      { "kind": "plan-review", "approve": "PostgreSQL" }
      }
    ]
  }
}
```

```ts
interface AskUserQuestionItem {
    id: string;             // stable caller-provided question id, echoed in the answer
    question: string;
    detail?: string;
    header?: string;
    options?: AskUserQuestionOption[];     // array of {label, description?}
    multiSelect?: boolean;
    intent?: AskUserQuestionIntent;        // presentation intent (plan-review etc.)
}
```

**Routing key** for `respond`: the frame's **rpcId** (NOT any session id concat). The question's logical id IS the rpcId minted when the host accepts `ask()`. `permissions.go:142-187` documents the trap.

Empty `questions` array is a known dsh quirk (placeholder frames); bridge logs and skips.

#### 3.4.10 `question/resolved` — audit frame

```json
{
  "method": "question/resolved",
  "payload": { "sessionId": "session-...", "questionRpcId": "...", "outcome": "answered" | "cancelled" }
}
```

Pure push; debug log only.

#### 3.4.11 `stream/error` — mid-stream error

```json
{
  "method": "stream/error",
  "payload": { "error": { "code": "internal", "message": "...", "details": {} } }
}
```

Emitted when the impl throws mid-stream; the stream then closes. The client must see the failure instead of a silent end.

### 3.5 Unknown mux methods

`handleMuxFrame`'s `default` branch (`handle_mux.go:145-156`):

1. Increments `wireState.unknownCount` (P4 observability)
2. Warns once per unknown: `"dsh: mux unknown method"`
3. Does NOT kill the session — lenient policy

When dsh adds a new mux method, the bridge surfaces it via this warn path until a handler is registered in `dispatch.go:standardRegistry`.

---

## 4. Host Stream (`/api/events.host`)

Same dual transport as mux (SSE or WS upgrade). Frames describe **host-container lifecycle** rather than session events.

### 4.1 Frame methods (events.d.ts `HostFrame` union — 9 types)

| `method` | Payload | Bridge action |
|---|---|---|
| `host/session-added` | `{sessionId, blank, parentSessionId?, origin?, cwd?, agentPreset?}` | debug log |
| `host/session-removed` | `{sessionId}` | debug log |
| `host/session-status` | `{sessionId, running}` | debug log |
| `host/agent-error` | `{sessionId, message}` | debug log |
| `host/workspace-changed` | `{workspace: WorkspaceView}` | debug log |
| `host/workspace-removed` | `{workspaceId}` | debug log |
| `host/workspace-order-changed` | `{workspaceIds: WorkspaceId[]}` | debug log |
| `host/archived-sessions-changed` | `{archivedSessionIds: SessionId[]}` | debug log |
| `host/remote-event` | `{event: string, args: JsonValue[]}` | debug log |
| `stream/error` | `{error: RpcError}` | (last-frame error path) |

`session-added` carries the lineage anchor, origin, cwd, and blank bit — the list-summary fields a client can't wait for a refresh to learn. Fires at `session/created`, so `blank` is constantly `true`; clients flip it on the session's first `host/session-status{running: true}` (a blank session never runs). Reconnecting clients take `session.list`'s `summary.blank` as authoritative.

`workspace-changed` / `archived-sessions-changed` push the full new snapshot after every durable mutation (same full-snapshot posture as `workspace.list`).

`remote-event` forwards allowlisted host cordis events verbatim (`@deepseek-ai/dsh-api-remotes` owns the allowlist). Delivery lands on `ctx.remote.$on`, not on a per-event frame variant.

`handleHostFrame` (`handle_mux.go:163-167`) currently logs every host frame without emitting `AgentEvent`. Future PR may surface `session/removed` / `agent-error` for runtime cleanup.

---

## 5. Downloads (`GET /api/session.export`)

No envelope. Query params: `sessionId`, `includeDescendants?`. Streams a session-log ZIP as attachment response. The browser `IApiClient` never exposes this method.

---

## 6. RpcError Code Reference

Full `RpcErrorDetailsMap` (`rpc.d.ts`) — closed set. Bridge behaviour grouped by category. Code values not in this table are unreachable.

| `code` | `details` shape | Where | Bridge behaviour |
|---|---|---|---|
| `bad-request` | `{issues: ZodIssue[]}` | every endpoint | surface on `EventAgentError` + payload decode details |
| `cancelled` | `{}` | server-initiated | not an error — normal cancellation path |
| `session-not-found` | `{sessionId}` | session-scoped methods | fork ⇒ `ErrResumeUnhealthy`; prompt ⇒ re-create + retry |
| `model-unavailable` | `{provider, model}` | session.* / llm.* | `EventAgentError` + prompt user `/use` |
| `session-conflict` | `{sessionId, requestedCwd, existingCwd?}` | session.create | `EventAgentError` (retry with consistent cwd) |
| `invalid-time-zone` | `{value}` | session.prompt (when zone invalid) | `EventAgentError` |
| `workspace-attach-failed` | `{sessionId, workspaceId}` | session.create | `EventAgentError` |
| `workspace-not-found` | `{workspaceId}` | workspace.* | `EventAgentError` |
| `workspace-invalid-path` | `{path}` | workspace.create | `EventAgentError` |
| `workspace-name-conflict` | `{name}` | workspace.rename | `EventAgentError` |
| `workspace-move-invalid` | `{workspaceId, sessionId, beforeSessionId?}` | workspace.insertSessionBefore | `EventAgentError` |
| `directory-unreadable` | `{path}` | host.listDirectory | `EventAgentError` |
| `directory-exists` | `{path}` | host.createDirectory | `EventAgentError` |
| `directory-create-failed` | `{path}` | host.createDirectory | `EventAgentError` |
| `directory-picker-unavailable` | `{capability}` | host.pickDirectory (capability missing) | `EventAgentError` |
| `agent-preset-read-only` | `{agentPreset, reason}` | agentPreset.* on read-only preset | `EventAgentError` |
| `agent-preset-locked` | `{sessionId, agentPreset}` | agentPreset.select after conversation started | `EventAgentError` |
| `agent-preset-conflict` | `{sessionId, requestedPreset, existingPreset?}` | agentPreset.select | `EventAgentError` |
| `agent-preset-not-found` | `{agentPreset, available[]}` | session.create / agentPreset.* | `EventAgentError` |
| `agent-preset-invalid` | `{agentPreset, reason}` | session.create | `EventAgentError` |
| `agent-busy` | `{reason}` | session.* / subagent.* on busy session | retry with backoff (codex `ErrTurnBusy` mirror) |
| `attachment-error` | `{reason}` | session.attachment | `EventAgentError` |
| `queue-item-not-found` | `{itemId}` | session.updateQueue | `EventAgentError` |
| `steer-unavailable` | `{itemId}` | session.updateQueue (steer not applicable) | `EventAgentError` |
| `command-error` | `{}` | session.prompt (slash command usage/state error) | `EventAgentError` |
| `unknown-command` | `{}` | session.prompt (unrecognized slash command name) | `EventAgentError` |
| `settings-rejected` | `{ns}` | settings.* | `EventAgentError` |
| `settings-not-exposed` | `{ns}` | settings.* (namespace outside config plane) | `EventAgentError` |
| `settings-conflict` | `{ns, expected, actual}` | settings.* (stale revision) | re-read and retry |
| `credential-rejected` | `{ref}` | credentials.set/unset (read-only shadow) | `EventAgentError` |
| `model-discovery-failed` | `{settingsNs, baseURL?}` | llm.discoverModels | `EventAgentError` (fallback to hand-entry) |
| `title-invalid` | `{sessionId}` | session.rename (empty after normalize) | `EventAgentError` |
| `fork-unavailable` | `{sessionId}` | session.fork (no completed turn / mid-turn anchor) | `ErrResumeUnhealthy` |
| `subagent-parent-unavailable` | `{parentSessionId}` | subagent.* (parent not available) | `EventAgentError` |
| `subagent-not-found` | `{parentSessionId, childSessionId}` | subagent.* | `EventAgentError` |
| `subagent-catalog-diagnostic` | `{parentSessionId, childSessionId, reason}` | subagent.list (diagnostic entries) | debug log |
| `subagent-not-resumable` | `{childSessionId}` | subagent.prompt (cannot resume) | `EventAgentError` |
| `subagent-unauthorized` | `{childSessionId}` | subagent.* (auth fail) | `EventAgentError` |
| `subagent-delivery-unavailable` | `{childSessionId}` | subagent.prompt | `EventAgentError` |
| `internal` | `{}` | transport catch-all | `EventAgentError` |

`details` is **required** for every error code (even `internal` uses `{}`). The schema enforces this. The bridge decodes via `json.RawMessage` and surfaces the raw `code:message:details` triple to the runtime.

---

## 7. Spawn and Lifecycle (out-of-band)

These are **not** wire — they're the spawn recipe and lifecycle that frame everything above.

### 7.1 Spawn recipe

```go
cmd := exec.CommandContext(ctx, "dsh", "--profile", "web", "--port", "0")
cmd.Dir = cfg.Workspace
cmd.Env = append(os.Environ(), "DSH_PERMISSION_MODE=danger-full-access")
```

| Arg / Env | Purpose | Why |
|---|---|---|
| `--profile web` | launch the web harness (vs `--profile headless` for one-shot print mode) | selects the HTTP+SSE/WS surface |
| `--port 0` | OS picks a free port | avoids collisions on shared hosts |
| `cmd.Dir` = workspace | dsh's session cwd | runtime context (no other model/provider injection) |
| `DSH_PERMISSION_MODE=danger-full-access` | bypass interactive permission prompts | per `[[agent-no-config-tampering]]` — only inject transport + permissions |

### 7.2 URL parse

dsh web prints its bound URL on the first stdout line:

```
dsh web: http://127.0.0.1:3080
```

The regex (`session.go:97`):

```go
var dshURLPattern = regexp.MustCompile(`dsh web:\s+http://([^:\s]+):(\d+)`)
```

`parseWebURL` reads stdout line-by-line in a goroutine + channel pattern (not `bufio.Scanner`) so ctx cancellation can fire mid-read. Bound by `webURLParseTimeout = 10 s`; cold start is ~1.5 s in practice.

### 7.3 Handshake

```
1. POST /api/session.fork  { sessionId: cfg.SessionID }   // if cfg.SessionID != ""
   └─ ok            → use returned sessionId; resumed=true
   └─ any failure   → return ErrResumeUnhealthy (strict refusal, no fallback)

2. POST /api/session.create { workspaceId|cwd, agentPreset? }   // if cfg.SessionID == ""
   └─ ok            → use returned sessionId; resumed=false
   └─ failure       → bridge error
```

Each call has its own 15 s `handshakeTimeout` ctx, derived from the spawn ctx. The fork/create are independent budgets.

`session.models` is **not** part of the handshake — the bridge queries it lazily after `EventAgentReady` to stamp `Model` on the event.

### 7.4 Stream pump topology

```
                       dsh web daemon
                            │
              ┌─────────────┼─────────────┐
              ▼             ▼             ▼
       /api/events.mux   /api/events.host   /api/{method}
              │             │             (HTTP)
              ▼             ▼
        readMuxPump    readHostPump
              │             │
              │  debug-only │
              ▼             ▼
        handleMuxFrame  handleHostFrame
              │
              ├─ session/event     → dispatcher.dispatch(env, view)
              ├─ session/projection → wireState.applyProjection (key field, see §3.4.3)
              ├─ approval/asked    → handleInlineApproval
              └─ approval/requested/question → permissions layer

        (parallel: drainStdout, drainStderr, lifecycle)
```

All four pumps + lifecycle are wired by `startPumps`. The `lifecycle` goroutine **exclusively owns `close(events)`**; `Close()` never closes the events channel itself.

### 7.5 Close (graceful → forced)

```go
func (d *driver) Close() error {
    d.closeOnce.Do(func() {
        close(d.closed)
        _ = d.muxWS.Close(); _ = d.hostWS.Close()
        if d.sessionID != "" {
            // POST /api/session.cancel, 3s timeout
            _, _ = d.http.Post(cancelCtx, "session.cancel", {"sessionId": d.sessionID})
        }
        _ = d.cmd.Process.Signal(os.Interrupt)
        select {
        case <-d.exitDone:
        case <-time.After(5 * time.Second):
            _ = d.cmd.Process.Kill()
            select {
            case <-d.exitDone:
            case <-time.After(5 * time.Second):
                err = errors.New("dsh: child did not exit within SIGKILL grace")
            }
        }
    })
    return err
}
```

Order: WS close → cancel in-flight turn → SIGINT → wait 5 s → SIGKILL → wait 5 s.

`closeOnce` guarantees a single teardown even if runtime calls `Close()` concurrently.

### 7.6 Lifecycle fail-path

When `cmd.Wait()` returns:

1. Wait for all pumps to drain (`pumpWG.Wait()`)
2. Send `"declined"` to every still-pending approval channel (so runtime handlers don't hang)
3. If `waitErr != nil`, emit `EventAgentError{Err: waitErr}`
4. Close `events` (sole owner)

---

## 8. Wire Limits and Operational Notes

| Limit | Value | Source |
|---|---|---|
| HTTP body read | 16 MiB | `http.go:103` |
| HTTP body error context | 1 KiB | `http.go:99` |
| WS per-frame read limit | 10 MiB | `ws.go` `wsFrameReadLimit` |
| HTTP client timeout (default) | 30 s | `httpClientTimeout` |
| Handshake per-call timeout | 15 s | `handshakeTimeout` |
| Web URL parse timeout | 10 s | `webURLParseTimeout` |
| Permission timeout | 5 min | `permissionTimeout` |
| Cancel RPC timeout | 3 s | `session.go:Close` |
| Lifecycle SIGINT grace | 5 s | `session.go:Close` |
| Lifecycle SIGKILL grace | 5 s | `session.go:Close` |
| Watchdog timeout | 5 min | `lifecycleWatchdogTimeout` |
| Events channel buffer | 131072 (~26 MiB at ~200 B/event) | `eventBufferSize` |
| stderr tail ring buffer | `StderrTailBytes` (4 KiB default) | `agent.StderrRingBuffer` |
| Wire frame ring buffer | 64 entries | `wireRingBuffer` (P4 observability) |
| CORS write fence | all `/api/*` POST blocked w/o preflight | `handler.js` cross-site write fence |

**Backpressure**: `deliver()` is non-blocking — drops on full buffer with a debug-log warning.

**Strict refusal on resume**: `session.fork` failure surfaces `ErrResumeUnhealthy` (not silent fallback). The runtime's chatsession.go §1624 path then clears the stale id and starts fresh on the user's next message — preventing "user lost their history forever" + "operator can't see why".

**FIFO routing for pending approvals**: `pendingOrder` slice tracks insertion order; `SendPermission` pops `pendingOrder[0]` so the runtime's answer goes to the oldest pending request.

---

## 9. End-to-End Example (single turn)

```
t0   shell> dsh --profile web --port 0
        stdout: "dsh web: http://127.0.0.1:51823"

t1   bridge → parseWebURL → baseURL = "http://127.0.0.1:51823"

t2   bridge → dial mux + host (SSE GET or WS upgrade, see §3) ────▶ streams open

t3   bridge → POST /api/session.create { cwd, title } ───────▶ 200 OK { sessionId: "session-aaa" }
        ◀────── { type: "server-response", rpcId, result: { ok: true, value: { sessionId, agentPreset? } } }

t4   bridge → EventAgentReady { SessionID, AgentName: "dsh", Workspace, Branch }

t5   mux stream → server-pushed "session/subscribed" { sessionId, lastSeq: 0 }

t6   user sends prompt: "describe this image"
      bridge → POST /api/session.prompt
                { sessionId: "session-aaa",
                  mode: "queue",
                  content: [
                    { type: "text", text: "describe this image" },
                    { type: "image", mediaType: "image/png", data: "<base64>", name: "cat.png" }
                  ],
                  clientTimeZone: "Asia/Shanghai" }
            ────────▶ 200 OK { result: { ok: true, value: { accepted: true } } }    (inbox enqueue, fast)

t7   mux stream push storm:
      data: { type: "server-request", rpcId: "fresh-1", method: "session/event",
              payload: { sessionId: "session-aaa", event: { type: "turn/start", data: { turn: 1 } } } }\n\n
      data: { type: "server-request", rpcId: "fresh-2", method: "session/event",
              payload: { sessionId: "session-aaa", event: { type: "assistant/chunk",
                data: { turn: 1, chunk: { type: "text-delta", index: 0, text: "The" } } } } }\n\n
      ...
      data: { type: "server-request", rpcId: "fresh-N", method: "session/event",
              payload: { sessionId: "session-aaa", event: { type: "assistant/message",
                data: { turn: 1, message: { role: "assistant", content: [{ type: "text", text: "The image shows..." }] } } } } }\n\n
      data: { type: "server-request", rpcId: "fresh-M", method: "session/event",
              payload: { sessionId: "session-aaa", event: { type: "turn/end", data: { turn: 1, stopReason: "stop" } } } }\n\n

      bridge accumulates text-deltas → flushes pendingText on assistant/message → EventAgentText
      turn/end → EventResult{Usage} + EventDone{Reason: "settled"}

t8   user sends next prompt → t6 repeats (same sessionId, same streams)

t9   bridge.Close()
      → muxWS.Close(); hostWS.Close()  (or cancel SSE generators via AbortSignal)
      → POST /api/session.cancel { sessionId }   (best-effort, 3s)
      → SIGINT to dsh process
      → wait ≤5s for exit, else SIGKILL, wait ≤5s more

t10  lifecycle goroutine: pumps drain → decline any pending approvals → close(events)
      AgentSession.SetExited(0)
```

---

## 10. Quick Wire Schema Index

For grep-ability, all schema types defined by the authoritative source (vendored at `@deepseek-ai/dsh-host-apiproxy/lib/types/api/*.d.ts`):

| Authoritative type | Wire location | Purpose |
|---|---|---|
| `ClientRequest` | HTTP POST body | outbound RPC envelope |
| `ServerResponse` | HTTP POST response | inbound RPC envelope |
| `ServerRequest` | both WS/SSE stream frames | every server-pushed frame |
| `ClientResponse` | `POST /api/respond` body | answer server-initiated approval/question |
| `RpcMessage` | union of all 4 above | wire full-form union |
| `RpcReceipt` | HTTP response of `/api/respond` | `{accepted: true}` or `{accepted: false, reason}` |
| `RpcResult<T>` | `rpcResponse.result` | success/failure discriminator |
| `RpcError` | `rpcResult.error` | discriminated by `code` |
| `RpcErrorDetailsMap` | per-error details shape | closed set of ~36 codes (§6) |
| `RpcId` | branded string | correlation id |
| `MuxFrame` | `events.d.ts` | discriminated union of mux frame types |
| `HostFrame` | `events.d.ts` | discriminated union of host frame types |
| `SessionEvent` | `session/event.event` | merge-extensible event union (42+ types) |
| `ToolEventView` | `session/event.view` | host-computed render intent |
| `QueuedInboxItem` | `session/queue.items[]` | pending inbox entry |
| `JobView` | `session/jobs.jobs[]` | background job wire view |
| `SessionsApi` | `rpc-map.d.ts` | 12 session.* methods |
| `SubagentsApi` | `rpc-map.d.ts` | 4 subagent.* methods |
| `HostApi` | `rpc-map.d.ts` | 5 host.* methods |
| `WorkspaceApi` | `rpc-map.d.ts` | 7 workspace.* methods |
| `SkillsApi` | `rpc-map.d.ts` | 1 skill.list method |
| `AgentPresetsApi` | `rpc-map.d.ts` | 6 agentPreset.* methods |
| `GoalsApi` | `rpc-map.d.ts` | 6 goal.* methods |
| `SettingsApi` | `rpc-map.d.ts` | 5 settings.* methods |
| `CredentialsApi` | `rpc-map.d.ts` | 3 credentials.* methods |
| `LlmApi` | `rpc-map.d.ts` | 3 llm.* methods |
| `DownloadsApi` | `rpc-map.d.ts` | 1 host-only download method |
| `SessionSummary` | `session.list.items[]` entry | session metadata |
| `SessionSearchItem` | `session.search.items[]` entry | search hit |
| `SessionModels` | `session.models` value | model directory snapshot |
| `ModelSelection` | `SessionModels.current` | one provider/model selection |
| `ModelProviderGroup` | `SessionModels.groups[]` | one provider's models |
| `ModelCatalogModel` | `ModelProviderGroup.models[]` | one model |
| `ModelReasoning` / `ModelReasoningEffort` | `ModelCatalogModel.reasoning` | exact-route reasoning metadata |
| `ModelCatalogFailure` | `SessionModels.failures[]` | provider-local lookup failure |
| `HistoryEntry` | `session.history.events[]` entry | `{event, view?}` |
| `SessionProjectionsBlock` | `session.history.projections` (tail) | `{asOfSeq, values}` |
| `PromptContentPart` | `session.prompt.content[]` element | `text` \| `image` |
| `QueueAction` | `session.updateQueue.action` | `edit` \| `remove` \| `steer` |
| `WorkspaceView` | `workspace.list.items[]` entry / `host/workspace-changed` | one workspace |
| `WorkspaceId` | branded string | workspace id |
| `SkillEntry` | `skill.list.skills[]` entry | one skill |
| `AgentPresetEntry` | `agentPreset.list.presets[]` entry | one preset |
| `GoalId` / `GoalRef` | branded string / `{id, revision}` | goal CAS identity |
| `SettingsNamespaceView` | `settings.describe.namespaces[]` / `update/replace/mutate` value | one namespace |
| `SettingsSecretView` | `SettingsNamespaceView.secrets[]` | one secret slot |
| `SettingsPathOpView` | `settings.mutate.ops[]` element | `set` \| `unset` |
| `CredentialView` | `credentials.describe.credentials[ref]` | one ref state |
| `ConfigurableProviderView` | `llm.providers.providers[]` entry | one provider config |
| `DiscoveredModelView` | `llm.discoverModels.models[]` entry | one interrogated model |
| `SubagentCatalog` / `SubagentListEntry` | `subagent.list` value / entries | direct children |
| `SubagentAddress` | `subagent.history/prompt/interrupt` discriminator | parent+child id |
| `SubagentPromptReceipt` / `SubagentInterruptReceipt` | subagent return values | `messageId` / `accepted` |
| `AskUserQuestionItem` | `question/requested.questions[]` | one question |
| `AskUserQuestionOption` | `AskUserQuestionItem.options[]` | `{label, description?}` |
| `AskUserQuestionIntent` | `AskUserQuestionItem.intent` | presentation intent (plan-review) |
| `AskUserQuestionAnswer` / `AskUserQuestionAnswerItem` | `question` response value | `{id, selected[], custom?}` |
| `ApprovalRequestId` | branded string | approval id (audit correlation) |
| `ApprovalOutcome` | closed enum | `allowed-once` \| `rejected` \| `cancelled` \| `unavailable` |
| `ApprovalResponsePayload` | `approval` response value | `{sessionId, approvalId, outcome}` |
| `QuestionResponsePayload` | `question` response value | `{sessionId, answer}` |
| `RpcMethodMap` | `rpc-map.d.ts` | method name → signature map (single source of truth) |
| `IApiClient` | `fetch/client.d.ts` | browser client consumption face |
| `ApiProxy` | `api/index.d.ts` | host-side impl face |

The `RpcMethodMap` is the single source of truth — adding a method = one entry in `rpc-map.ts` + one entry in the matching `*.d.ts` domain file. `RequestPayload<K>` and `ResponseValue<K>` are derived types that automatically propagate to `IApiClient`.

---

## 11. Bridge Bugs vs Authoritative Source (TRACKED)

The Go bridge was implemented before the TS source was accessible for verification; the following discrepancies exist:

| # | Bridge code | Authoritative wire | Status |
|---|---|---|---|
| 1 | `protocol.go:projectionEnvelope` uses `Key string \`json:"key"\`` | `session/projection` uses `key` | ✅ fixed |
| 1b | `applyTodoProjectionLocked` unmarshals `value` as `{todos\|items: TodoItem[]}` | `key:"todos"` value is `TodoItem[] \| null` (array or JSON null) | ❌ BUG — array frames drop; `null` still emits empty `EventAgentTaskCreate` (Feishu clears checklist). Live `todo/write` object shape is unaffected. History tail `projections.values.todos` is not applied on attach. |
| 2 | `session.go:SendPermission` sends `client-request{method:"respond", payload:{rpcId, payload:{outcome}}}` | `respond` is `client-response{envelope.rpcId echoes server-frame's rpcId, result.value:{sessionId, approvalId, outcome}}` | ❌ BUG — legacy form accidentally accepted by 0.1.0-rc.6; fix to canonical form |
| 3 | `session.go:SendPermission` outcome vocabulary: `"approved"` \| `"declined"` | `ApprovalOutcome`: `"allowed-once"` \| `"rejected"` (client-giveable subset) | ❌ BUG — wrong enum values |
| 4 | `protocol.go:questionPayload.Options` uses `[]string` | `AskUserQuestionItem.options` is `[]AskUserQuestionOption` (objects) | ❌ BUG — option shapes |
| 5 | `permissions.go:handleQuestionRequested` registers `pendingApprovals[frameRpcID]` and the `respond` payload uses `{rpcId: approvalID, outcome}` | Question response is `{sessionId, answer: {answers: [{id, selected, custom}]}}`; `rpcId` is the envelope-level echo of server-frame | ❌ BUG — wrong response payload shape |
| 6 | Bridge dials WebSocket (`ws.go:dialWS`) | Server supports BOTH SSE GET and WebSocket upgrade | ✅ OK (both valid); could simplify by switching to SSE and dropping gorilla/websocket |
| 7 | `sessions.d.ts` lists 50+ methods across 11 domains; bridge only calls ~8 | All 50+ available | ⚠ Bridge doesn't yet wire host/workspace/skills/agentPresets/goals/settings/credentials/llm; runtime doesn't ask for them yet |
| 8 | Bridge opens mux+host as 2 separate WS dials | `events.mux` aggregates ALL sessions on one stream (subscription baseline per attached session); host is single stream | ✅ OK semantically |

Items 1b and 2–5 are the active bridge bugs against the canonical wire. Item 1 (`key` vs `projection`) is fixed.

---

## 12. Reverse-Engineering Methodology Notes

For future maintainers picking up this protocol or extending it:

1. **The TS source under `dsh-host-apiproxy/lib/types/api/*.d.ts` IS the wire contract** — every method signature is annotated with `RpcRequest<P>` and `Promise<RpcResponse<T>>`, with the body of `T` documented inline. Doc comments give the precise semantics for each error code, capability gate, and edge case.

2. **`RpcMethodMap` is the index** — search `rpc-map.d.ts` for the method name you need. The payload/response types are derived via `RequestPayload<K>` / `ResponseValue<K>` from the matching domain file.

3. **Probe with curl + websocat / curl --no-buffer** — start `dsh --profile web --port 3080`, then:

   ```bash
   curl -s -X POST http://127.0.0.1:3080/api/session.list \
        -H 'Content-Type: application/json' \
        -d '{"type":"client-request","rpcId":"probe-1","method":"session.list","payload":{}}'
   curl -sN http://127.0.0.1:3080/api/events.mux    # SSE
   ```

4. **SSE frame format** — each `data:` line is ONE full `ServerRequest` envelope (including the `type:"server-request"` discriminator), terminated by `\n\n`. Comment lines (`: ...\n\n`) are not frames. Mid-stream impl failures emit one `stream/error` frame then close.

5. **Field naming traps** — verified during this audit:
   - `session.list.items` not `sessions`
   - `updatedAt` not `createdAt`
   - `session/projection.key` not `projection`
   - `session/projection` `key:"todos"` **value is `TodoItem[] \| null`** (array), while `todo/write` **data is `{todos: TodoItem[]}`** (object). Do not decode one shape as the other.
   - `ApprovalOutcome` enum: `allowed-once`/`rejected` (client-giveable) + `cancelled`/`unavailable` (host-side)
   - `question/requested.options[]` is objects (`{label, description?}`) not strings

6. **Strict refusal on resume** — `session.fork` failing should propagate `ErrResumeUnhealthy`, not silently fall through to `session.create`. The runtime handles `ErrResumeUnhealthy` uniformly across bridges (`chatsession.go §1624`); falling through would silently lose user history.

7. **Frame `rpcId` is the answer key** — the server correlates answers on the frame's `rpcId` (for both approval/requested and question/requested). We key `pendingApprovals[frame.RPCID]` and pass `frame.RPCID` as the **envelope-level** `rpcId` of `POST /api/respond`. Using the payload's `approvalId` (audit-only) as the correlation key would route answers to nothing.

8. **`type` field is load-bearing on every envelope** — outbound MUST carry `"type":"client-request"` (or `"client-response"` for `/api/respond`); inbound frames MUST be `"server-request"`. The server's schema rejects envelopes without it.

9. **`mode` is required on `session.prompt`** — sending `session.prompt` without `mode` returns `bad-request: invalid input: expected "queue"`.

10. **Slash commands go through `session.prompt` too** — content starting with `/` is intercepted by the host command registry (mode-agnostic); success returns `command: {kind:"success", text?}`.