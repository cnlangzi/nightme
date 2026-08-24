# dsh — Shared Host Architecture (1:N multiplexing)

> **Status**: ✅ **已实现并落地**(2026-08-16,fix-dsh-tasks 分支)
> **Scope**: `internal/bridge/dsh/` 改为**单一全局 dsh web 实例**服务所有 ChatSession / AgentSession
> **实现位置**: `internal/bridge/dsh/host/{client.go, stream.go, router.go, lifecycle.go, recovery.go}`
> **验证**: 2026-08-16 本机 `dsh 0.1.0-rc.6` 实机,见 `/tmp/dsh-probe/`
> **姊妹文档**:
> - [./dsh.md](./dsh.md) — bridge 历史形态(per-ChatSession dsh,已 deprecated)
> - [./dsh-api.md](./dsh-api.md) — wire 协议权威参考(基于 TS contract + 实机)
> - [./cli-transport.md](./cli-transport.md) — pipe / lifecycle 通用约束

---

## 1. 目标与背景

### 1.1 现状(2026-08-16)

`internal/bridge/dsh/` 每个 ChatSession 启动一个独立 `dsh --profile web --port 0` 进程,各自监听 OS-assigned 端口,各自持有 mux+host 两条 WebSocket。N 个 session = N 个 dsh 子进程 + N×2 条 WS + N 套 `pendingApprovals` map。

### 1.2 问题

1. **资源浪费**: 每个 dsh 是 Node 进程(实测 RSS ~80 MiB)+ V8 warmup(~1.5 s)+ 完整 host cwd 重启。N 个 chat 同步跑 = N 倍开销
2. **协议能力浪费**: dsh 的 mux 协议**本身就是多路复用**(`session/subscribed` 为每个 attached session 发一帧,见 [dsh-api.md §3.4.1](./dsh-api.md)),我们却在 client 侧自己切碎了
3. **跨 session 视图缺失**: dsh 自带 `workspace.*` / `subagent.*` 协议,但当前实现完全用不上 — 因为每个 dsh 实例只有"自己"那一个 session

### 1.3 目标架构

```
                       ┌─────────────────────────────────────┐
                       │   dsh web (single instance)         │
                       │   --profile web --port 3080         │
                       │                                     │
                       │   /api/{method}    ── shared RPC     │
                       │   /api/events.mux ── ONE WS,N sessions│
                       │   /api/events.host ── ONE WS         │
                       └──────────────┬──────────────────────┘
                                      │ HTTP + WS
                                      ▼
              ┌───────────────────────────────────────────────────┐
              │     GlobalDshClient  (singleton)                  │
              │     ─────────────────                             │
              │     • http.Client (baseURL + RPC envelope)        │
              │     • muxPump: 单 WS + demuxRouter[sessionId]     │
              │     • hostPump: 单 WS + debug log                 │
              │     • pendingApprovals[sessionId][frame.rpcId]    │
              │     • RestartRecovery: session.list → re-attach   │
              └─────┬─────────────┬─────────────┬────────────────┘
                    │ demux       │ demux       │ demux
                    ▼             ▼             ▼
              ChatSession(A) ChatSession(B) ChatSession(C)
              │      │       │      │       │      │
              ▼      ▼       ▼      ▼       ▼      ▼
              AS-a  AS-a2   AS-b  AS-b2   AS-c  AS-c2
              (sessionId="session-aaa") (s-bbb)  (s-ccc)
```

---

## 2. 实机 Probe 结果(2026-08-16)

`/tmp/dsh-probe/` 保留所有探针脚本和输出,关键发现:

### 2.1 ✅ 多路复用确认(mux)

启 `dsh --profile web --port 3080`,**一条** WebSocket 连 `/api/events.mux`,**接收 6 个 `session/subscribed` 帧**(每个 sessionId 一帧,discriminator = `payload.sessionId`)。期间又 create 2 个新 session + 发 prompt,观察 65 帧跨 8 个 session 分布:

| sessionId | frames | methods |
|---|---|---|
| session-0e9a6466-... | 1 | subscribed |
| session-16d014db-...(新 prompt) | 52 | projection:25, event:24, subscribed:1, queue:2 |
| session-30a81a2b-...(mid-stream create) | 7 | projection:3, event:3, subscribed:1 |
| 其余 5 个历史 session | 各 1 | subscribed |

→ **动态新增 session 自动加入 mux 订阅**,demux 完全可靠。

### 2.2 ✅ session.cancel 不杀 dsh

```
session.cancel → {ok: true, value: {accepted: true}}
host.describe (post-cancel): attachedSessions 8→9,version unchanged ✓
```

`session.cancel` 只取消当前 turn,**dsh 进程和 host 状态不变**。

### 2.3 ✅ session.fork 在共享 host 上工作

```
session.fork({sessionId: "session-4a4196e5-..."}) 
→ {ok: true, value: {sessionId: "session-8137d00f-..."}} (新 id)
```

### 2.4 ⚠️ session.list 无分页

836 个 session 时,**`session.list` 耗时 23–50 秒,响应 376–397 KB**(probe: `list-post.json`)。dsh-api.md §2.1.1 标注 cursor "**unimplemented in v1**"。

**设计含义**:
- 不能每次 ChatSession 创建都调 `session.list`
- 必须**本地缓存 + 增量更新**(host/session-added/removed 已在 host 流)
- 启动时一次性扫一次,之后靠 host stream 增量

### 2.5 ✅ sessionId 跨 dsh 重启稳定

```
1. SIGTERM dsh web (PID 40682)
2. 重启 dsh web (PID 42335),绑同一端口
3. session.list → 836 items(数量一致)
4. 取 3 个 sessionId 验证 → 3/3 都存在(blank/running/cwd 一致) ✓
```

### 2.6 ✅ Re-attach via session.create({sessionId, cwd})

dsh 重启后,所有 session 都 `running=false`(detached)。重新挂载:

```
session.create({sessionId: "session-4a4196e5-...", cwd: "/tmp/dsh-probe"})
→ {ok: true, value: {sessionId: "session-4a4196e5-...", agentPreset: "standard"}}
   ^^^^^^^^ 同一 id,非新 id
```

re-attach 后:
- `session.list` 该 sessionId 仍在(没新 id 出现)
- mux stream 重连后,该 sessionId **自动出现在 `session/subscribed` 帧**
- `session.history` 返回历史事件,prompt 能续上 turn

→ **重启恢复路径完整**: list 持久化 + re-attach + 历史可拉 + mux 自动恢复订阅

### 2.7 ✅ 已发现 dsh-api.md 未列的新 event 类型

probe `session/event` 实际遇到的 type 集合(全部出现在 0.1.0-rc.6):

| 已有(bridge 已处理) | 新发现(待扩展) |
|---|---|
| `assistant/chunk` | `permission/preset` |
| `assistant/message` | `sandbox/mode` |
| `tool/call` | `approval/policy` |
| `tool/result` | `agent/inbox/spliced` |
| `turn/start` | `user/message` |
| `turn/end` | `request/header` |
| `compaction/end` | `request/context` |
| `todo/write/update/delete` | `session/title` |
| `approval/asked` | `session/title-llm-request` |
| `step/start` / `step/end` (registered, **不** emit AgentEvent) | |

`session/projection` keys (0.1.0-rc.6): `todos` (To-dos strip; `value` 是 `TodoItem[] | null`), `title`, `permissions`, `plan`, `goal`, `sessionStats` (由 `step/*` 折叠), `subagentTiming`, `sessionListMetadata`, `contextPressure`, `contextBreakdown`, `tokenUsage`, `imageLimits`.

`step/start` / `step/end` 是推理周期边界 `{turn, step}`,给 `sessionStats`(TTFT / tok/s)用,**不是** TodoPanel。清单只对齐 `todo/write` + `todos` 投影。详见 [dsh-api.md §3.4.3](./dsh-api.md)。

### 2.8 ⚠️ Bridge Bug 仍存在(dsh-api.md §11)

实机验证 §11 列的若干 ❌ BUG 部分已修:
1. ~~`protocol.go:projectionEnvelope` 用 `projection` 而非 `key`~~ ✅ 已改为 `json:"key"`。**仍开**:`todos` 投影 `value` 是数组/`null`,decoder 仍按 `{todos|items}` object 解 — 见 [dsh-api.md §11 item 1b](./dsh-api.md)。
2. ~~`SendPermission` 发 `client-request{method:"respond"}`~~ ✅ `client-response` + approval `QuestionResponse` / `ApprovalResponse`
3. ~~`ApprovalOutcome` 用 `"approved"/"declined"`~~ ✅ `"allowed-once"/"rejected"`
4. ~~`questionPayload.Options` 用 `[]string`~~ ✅ `[]AskUserQuestionOption`
5. ~~`handleQuestionRequested` response 形状错误~~ ✅ frame `rpcId` + `{sessionId, answer:{answers:[{id,selected,custom?}]}}`。飞书单题点选项即答;Type your answer / Skip 走 `nm-q:`;多题卡内向导,最后一步 `nm-q:` 整批 POST(host `matchesQuestions`)

剩余 active bug 是 item 1b。AskUserQuestion 的飞书 UX 见 [dsh-api.md §3.4.9](./dsh-api.md)；交互卡踩坑见 [feishu-cards.md](../channel/feishu-cards.md)。

---

## 3. 全局组件 API 设计

### 3.1 `internal/bridge/dsh/host/client.go` — RPC 客户端

```go
package host

// Client is the shared RPC + stream client for the global dsh web
// instance. Constructed once at nightme daemon startup; safe for
// concurrent use across all ChatSessions / AgentSessions.
type Client struct {
    baseURL   string
    httpClient *http.Client     // no Timeout; bounded per-call via ctx
    rpcTimeout time.Duration    // default 30s; overridable per call
}

// RPC sends one client-request envelope and returns the parsed
// server-response value (or RpcError). rpcId is auto-minted unless
// caller passes one.
func (c *Client) RPC(ctx context.Context, method string, payload any, out any) error

// High-level wrappers (built on RPC, keep dsh-api.md naming):
func (c *Client) SessionList(ctx context.Context, cursor string) ([]SessionSummary, error)
func (c *Client) SessionCreate(ctx context.Context, opts SessionCreateOpts) (SessionID, error)
func (c *Client) SessionPrompt(ctx context.Context, sessionID SessionID, mode string, blocks []ContentBlock, tz string) error
func (c *Client) SessionCancel(ctx context.Context, sessionID SessionID) error
func (c *Client) SessionFork(ctx context.Context, sessionID SessionID, atSeq *int) (SessionID, error)
func (c *Client) SessionHistory(ctx context.Context, sessionID SessionID, beforeSeq *int, maxMessages *int) (HistoryPage, error)
func (c *Client) SessionModels(ctx context.Context, sessionID SessionID) (SessionModels, error)
func (c *Client) HostDescribe(ctx context.Context) (HostInfo, error)
func (c *Client) WorkspaceList(ctx context.Context) ([]WorkspaceView, []SessionID, error)
func (c *Client) WorkspaceCreate(ctx context.Context, path string) (WorkspaceView, bool, error)
func (c *Client) SubagentList(ctx context.Context, parentSessionID SessionID) (SubagentCatalog, error)
func (c *Client) SubagentHistory(ctx context.Context, addr SubagentAddress, beforeSeq *int, maxMessages *int) (HistoryPage, error)
// ...

// Respond sends a client-response envelope (for approval/question answers).
// rpcId MUST echo the server-pushed frame's rpcId (per dsh-api.md §3.4.6).
func (c *Client) Respond(ctx context.Context, rpcID string, value any) (accepted bool, reason string, err error)
```

**关键设计**:
- `httpClient` 无 `Timeout`(同 [opencode bridge §3](./opencode.md) — 全局 Timeout 会误杀长连接)
- 每个 RPC 用 `context.WithTimeout` 单独绑定
- `Respond` 独立于 `RPC`,因为 envelope shape 是 `client-response` 不是 `client-request`

### 3.2 `internal/bridge/dsh/host/stream.go` — 单 WS mux/host pump

```go
package host

// StreamHub owns the single mux WS connection (and single host WS).
// Demuxes incoming frames by payload.sessionId into per-session
// subscriber channels.
type StreamHub struct {
    client *Client
    
    muxSubs   map[SessionID]chan<- ServerFrame   // N subscribers, fan-in
    muxSubMu  sync.RWMutex
    
    hostSubs  map[HostFrameMethod]chan<- HostFrame  // host-level subscribers
    
    reconnectCh chan struct{}    // signals reconnect
    backoff     BackoffStrategy
}

// ServerFrame is the generic envelope every mux frame rides.
type ServerFrame struct {
    RPCID   string
    Method  string          // "session/event", "session/projection", ...
    Payload json.RawMessage  // method-specific payload (decoded by receiver)
}

// Subscribe returns a per-session frame channel. Closes when the
// session is removed (host/session-removed) OR when the StreamHub
// shuts down.
func (h *StreamHub) Subscribe(ctx context.Context, sessionID SessionID) (<-chan ServerFrame, error)

// SubscribeHost returns a channel for one host method (e.g.
// "host/session-added"). One channel per method, N subscribers can
// read (broadcast).
func (h *StreamHub) SubscribeHost(ctx context.Context, method HostFrameMethod) (<-chan HostFrame, error)

// Start opens mux + host WS, runs pump goroutines, auto-reconnects
// with backoff on disconnect. Safe to call once per StreamHub lifetime.
func (h *StreamHub) Start(ctx context.Context) error

// Stop signals all subscribers and tears down WS. Idempotent.
func (h *StreamHub) Stop() error
```

**关键设计**:
- 单 WS 上跑 mux + 单 WS 上跑 host — 两者各自连接,各自重连
- mux pump 内按 `payload.sessionId` 二级分发: 每个 session 一个 buffered chan(默认 256)
- 慢消费者背压:**drop-on-full + warn**(同现有 dsh bridge `deliver()` 语义,见 session.go)
- host 流是 broadcast,每方法一个订阅者列表(registry / observability 等)

### 3.3 `internal/bridge/dsh/host/router.go` — Frame → AgentEvent

```go
package host

// Router turns server-side frames into agent.AgentEvent, partitioned
// by sessionId. Each ChatSession owns one Router binding; multiple
// AgentSessions under one ChatSession share the router (their frames
// differ by sessionId).
type Router struct {
    sessionID   SessionID
    eventSink   chan<- agent.AgentEvent    // back to ChatSession's event pump
    approvals   *PendingApprovalMap         // sessionId-keyed shared
    wireState   *WireState                  // per-session projection cache
}

// HandleFrame is called by StreamHub when a frame for this sessionId arrives.
func (r *Router) HandleFrame(ctx context.Context, f ServerFrame) error

// PendingApprovalMap is the GLOBAL (not per-driver) approval/question tracker.
// Keyed by (sessionId, frame.rpcId) — the rpcId is stable across replay.
type PendingApprovalMap struct {
    mu sync.Mutex
    m  map[pendingKey]chan RespondValue
}
type pendingKey struct {
    SessionID SessionID
    RPCID     string
}
```

**关键不变量**:
- `pendingApprovals` 从 per-driver map 改为全局 `(sessionId, rpcId)` 二级 map
- Frame 的 `rpcId` 是 stable 的(`dsh-api.md` §3.4.6 + §3.4.9),refresh-recovery 期间重复出现的帧用同一 rpcId — 直接覆盖 pending key 即可

### 3.4 `internal/bridge/dsh/host/recovery.go` — Restart Recovery

```go
package host

// RecoverResult summarizes what recovery found.
type RecoverResult struct {
    Reattached []SessionID  // successfully re-attached via session.create
    Orphaned   []SessionID  // sessionId persisted but session.create failed
    New        []SessionID  // found on dsh but not in our persistence (skip)
}

// RecoverSession re-attaches one sessionId to the live dsh host.
// Idempotent: calling twice with same id is no-op the second time.
func (c *Client) RecoverSession(ctx context.Context, sessionID SessionID, cwd string) error

// RecoverAll walks persisted sessionIds and re-attaches each.
// Caller passes a lookup function (sessionId → persisted metadata).
func (c *Client) RecoverAll(ctx context.Context,
    known func() []PersistedSession,
) (RecoverResult, error)

type PersistedSession struct {
    SessionID  SessionID
    CWD        string
    AgentPreset string
}
```

**调用时机**:
- nightme daemon 启动时,GlobalDshClient 启动后,异步执行 RecoverAll
- AgentSession 从磁盘恢复后,异步触发 RecoverSession(单 session)
- 失败(orphan)写日志 + 标记 `sessionId` 为 `nil`,提示用户"该 session 已丢失,开始新 session"

---

## 4. 生命周期

### 4.1 启动顺序

```
nightme daemon boot
  1. 构造 GlobalDshClient
     ├─ Start dsh: exec.CommandContext("dsh", "--profile", "web", "--port", "0")
     ├─ parseWebURL (10s timeout)
     ├─ RPC handshake: host.describe (sanity check)
     ├─ Start StreamHub: open mux + host WS, start pumps
     └─ Spawned PID 记录到 lifecycle manager
  2. 启动 RecoverAll:
     ├─ registry.LoadPersistedSessions() → []PersistedSession
     ├─ for each: session.create({sessionId, cwd})   (并发,但 N <= 8)
     ├─ subscribe mux per re-attached sessionId
     └─ 写 metrics: recover.{attached,orphaned}
  3. 注册 watchdog: 每 30s host.describe 健康检查
  4. nightme 进入正常服务循环
```

### 4.2 Watchdog

```
host.describe 失败 OR timeout > 10s
  → 标记 host unhealthy
  → 等 5s,重试 host.describe
  → 3 次连续失败:
     ├─ 关闭所有 WS (StreamHub.Stop)
     ├─ 关闭当前 dsh 进程 (SIGINT 5s → SIGKILL 5s)
     ├─ 重启 dsh (新 PID)
     ├─ StreamHub.Start (自动重连 mux+host)
     └─ RecoverAll (重新挂载所有 persisted sessionIds)
```

### 4.3 ChatSession 接入新 session

```
ChatSession(A) 处理首条消息
  1. LookupOrCreate AgentSession(as-a)
  2. as-a.sessionId == "" (新 session)
     ├─ session.create({cwd, agentPreset}) → sId
     ├─ as-a.sessionId = sId
     ├─ StreamHub.Subscribe(ctx, sId) → frameCh
     ├─ 启 per-session readpump: frameCh → eventHandler → AgentEvent → chat pump
     └─ 写 persist: as-a.sessionId = sId (registry)
  3. as-a.sessionId != "" (旧 session, daemon 重启恢复)
     ├─ 检查是否在 mux subscribed 列表
     │   ├─ 是: 直接用(可能 daemon 重启时已 re-attach)
     │   └─ 否: RecoverSession(sId, cwd) → mux auto-subscribe
     └─ 同样启 readpump
```

### 4.4 ChatSession Close / AgentSession Drop

```
cs.Close() / DropAgentSession(as-a)
  1. readpump cancel → 退出事件循环
  2. session.cancel(sId) (best-effort, 3s timeout)   // 取消 in-flight turn
  3. StreamHub.Unsubscribe(ctx, sId)               // 移除 mux 订阅
  4. dsh 进程**不杀**,session 留在 host 上(下次可被 RecoverSession 复活)
  5. Persist: as-a 标记 Exited,保留 sessionId
```

**关键转变**: AgentSession 不再"杀掉自己的 dsh",而是"在全局 dsh 上注销订阅"。dsh 永远在跑(直到 nightme 退出)。

---

## 5. Event 类型扩展(0.1.0-rc.6)

`internal/bridge/dsh/dispatch.go:standardRegistry` 当前处理 11 个 `session/event` type。需扩展以覆盖 probe 发现的全部:

| `event.type` | 当前 | 应 emit |
|---|---|---|
| `permission/preset` | ❌ | debug log(初始 sandbox 配置) |
| `sandbox/mode` | ❌ | debug log |
| `approval/policy` | ❌ | debug log(可能影响后续 approval gating) |
| `agent/inbox/spliced` | ❌ | debug log(queue 已被 server spliced) |
| `user/message` | ❌ | debug log(回显用户消息 — **不要** emit 到 chat,避免双气泡) |
| `request/header` / `request/context` | ❌ | debug log(LLM 请求元数据) |
| `step/start` / `step/end` | ✅ 已注册,不 emit | (no-op) inference cycle / `sessionStats`。**不是** TodoPanel(`todo/write` + `todos` 投影才是) |
| `session/title` | ❌ | `EventSessionTitle{Title}` |
| `session/title-llm-request` | ❌ | debug log(模型请求生成标题) |
| `assistant/chunk` 等已有 | ✅ | 保留 |

注意: `user/message` 是 **server 回显**的用户消息 — emit 会跟用户真实消息重复,必须 debug-only(同 [claudecode §1 `--replay-user-messages` 不要](./claude.md))。

---

## 6. 顺手修 Bridge BUG

`internal/bridge/dsh/` 现有若干 BUG(dsh-api.md §11),新架构落地一并修:

| # | 文件:行 | 当前 | 改为 |
|---|---|---|---|
| 1 | `protocol.go:projectionEnvelope` | ~~`"projection"`~~ | `"key"` ✅ |
| 1b | `state.go:applyTodoProjectionLocked` | `value` 当 `{todos\|items}` object 解 | `TodoItem[] \| null`(数组直出);`null` 不要发空 OutTask* |
| 2 | `session.go:SendPermission` envelope | ~~`client-request{method:"respond",payload:{...}}`~~ | `client-response{rpcId:echoed,result:{ok,value}}` ✅ |
| 3 | `SendPermission` outcome | ~~`"approved"/"declined"`~~ | `"allowed-once"/"rejected"` ✅ |
| 4 | `protocol.go:questionPayload.Options` | ~~`[]string`~~ | `[]AskUserQuestionOption{label,description?}` ✅ |
| 5 | `permissions.go:handleQuestionRequested` | ~~key by approvalID; approval outcome payload~~ | key by frame.rpcId; payload 用 `QuestionResponsePayload` shape ✅ |

加上: 新发现的 session/event 类型(§5)的 standardRegistry 注册。

---

## 7. 配置

**2026-08-16 修订**: 不引入 env flag 切换,不保留 per-ChatSession legacy 模式。新架构是**唯一**实现。

### 7.1 端口

- 默认尝试 bind `127.0.0.1:3080`(与浏览器 `dsh web` 默认一致)
- 端口已被占用 → fallback `--port 0`(OS 分配),从 dsh stdout 解析实际 URL
- 解析逻辑见 [dsh-api.md §7.2](./dsh-api.md)

### 7.2 Env

| env | 默认 | 含义 |
|---|---|---|
| `DSH_HOST_CMD` | `dsh` | dsh 可执行路径(debug 用,允许替换 mock) |
| `DSH_PERMISSION_MODE` | `danger-full-access` | 注入到 dsh 子进程 env(per [[agent-no-config-tampering]]) |

**不引入**: `DSH_GLOBAL_HOST` 切换 flag、`DSH_HOST_PORT` 显式覆盖(都用代码逻辑自动选)。

### 7.3 多 nightme 实例协调

每个 nightme daemon 独立启自己的 dsh(各自 OS-assigned 端口,或默认 3080 + 抢占失败 fallback)。不引入 IPC / 锁文件。如果两 nightme 都尝试 bind 同一显式端口,后到者 dsh 启动失败 → nightme 启动失败(由 systemd / launchd 重试)。

---

## 8. 风险与 Open Questions

### 8.1 已 probe 验证(低风险)

- ✅ mux 多路复用
- ✅ sessionId 跨重启稳定
- ✅ re-attach via session.create
- ✅ session.cancel 不杀 host
- ✅ session.fork 在共享 host 工作
- ✅ session.history 跨重启持久

### 8.2 仍需实测

- **approval/requested 真实路径**: 实机 prompt 没触发 approval(默认 sandbox 接受了 Bash),需要构造 permission-policy 拒绝的 prompt 验证 dsh-api.md §3.4.6 路径
- **session/event 新类型**: `step/start` / `step/end` 已对齐 dashboard — 推理周期 / `sessionStats`,不映射 OutTask*。`request/header` 仍是 debug-only
- **host/session-removed 语义**: host stream 上见到 `session-removed` 时,本地订阅如何 cancel(避免 send on closed chan) — 需在 Router 订阅上加 ctx cancel 联动

### 8.3 设计层面

- **session.list 无分页**: 启动时一次 list 836 session 耗时 30–50s。启动期可以接受,但 chat 创建时不能调 list — 严格走 host/session-added 流维护本地 cache
- **DSH_PERMISSION_MODE 全局**: 当前所有 session 共享一个 permission mode。未来需要 per-chat policy 时,dsh 协议层面是否支持?待 probe
- **多 chat 高并发**: 实机只验证了 8 个 attached session 并发。需做 50+ chat 压测,看 mux 帧速率和 approval 队列
- **resource leak**: ~~session.create 不再调用,chat 退出只 Unsubscribe。dsh 内部 session log 永远保留 — 需要定期 `session.archiveSession` 清理~~ **已修**(2026-08-24):`driver.Close` 现在对 driver-owned workspace 调 `workspace.delete`(见 [dsh.md §15](./dsh.md)),attachSession 路径跳过。同 cwd 多次 chat 仍会累积 workspace(未做 cwd dedupe — 见 §15.7 后续 PR 路线),但单次会话退出不再泄漏

---

## 9. 实现路线图

详见 [docs/feat/F-dsh-shared-host.md](../feat/F-dsh-shared-host.md)(3 阶段单线推进,**不留 compat flag**):

| Phase | 范围 | 估时 | 风险 |
|---|---|---|---|
| **1. Build & Replace** | `internal/bridge/dsh/host/` 5 文件 + 改写 `session.go` + 替换 `cmd/nightme/main.go` 启动路径 | 5d | 中(不可逆) |
| **2. Restart Recovery** | registry 持久化 sessionId + 启动时 RecoverAll + test-recovery.sh | 2d | 中 |
| **3. Bug fixes + Event types**(并行) | §5 + §6 一并修 | 2d | 低 |