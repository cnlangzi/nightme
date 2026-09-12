# dsh — DeepSeek Harness Bridge (shared-host only)

> **Status**: 统一架构 — `Start` 与 `RunOnce` / `Review` 都走 `dsh --profile web` shared host;`dsh --profile headless` 路径已废弃。
> **Wire 协议参考**: dsh 0.1.2-rc.1 本机已装 — 实机抓包 + 仓内 `@deepseek-ai/dsh-api-gateway` / `@deepseek-ai/dsh-api-session-controller` 源码双验。
> **共享 host 设计 + 实现**: 1:N multiplexing、全局 watchdog + restart recovery;`workspace.archiveSession` 行为(repo-scoped workspace 跨 session 持久)。
> **Scope**: `internal/bridge/dsh/`
> **形态**: 一桥一端 — 全部走 shared-host web
> - `Starter.Start`(chat session,long-lived): 持有 `*Agent` 多轮
> - `Starter.RunOnce` / `Starter.Review`(一次性): 与 `Start` 同一形态,但跑完 `defer a.Close()` 走 `workspace.archiveSession` 隐藏 session row(repo-scoped workspace 跨 session 持久)
> **核心原则**: 接入底层 AI agent,**不修改 agent 本地默认配置**;nightme 只管 transport + permissions(权限默认全开)。
> **姊妹文档**:
> - [docs/bridge/dsh-shared-host.md](./dsh-shared-host.md) — 全局单实例 dsh,1:N 多路复用
> - [docs/bridge/claude.md](./claude.md) — stream-json transport,长生命周期
> - [docs/bridge/codex.md](./codex.md) — JSON-RPC over stdio + print-mode
> - [docs/bridge/pi.md](./pi.md) — JSONL RPC over stdio + print-mode
> - [docs/bridge/cli-transport.md](./cli-transport.md) — pipe / lifecycle 通用约束

---

## 0. dsh 版本约束

**bridge 仅支持 `dsh@0.1.2-rc.1`**。dsh 的 wire 协议在不同 rc 之间存在 break change,nightme dsh bridge 在实机抓包 + 源码双验后只针对 0.1.2-rc.1 编写,其他版本不保证兼容。

### 0.1 兼容性矩阵

| dsh 版本 | bridge 状态 | 失败模式 |
|---|---|---|
| **`0.1.2-rc.1`** | ✅ 支持(唯一目标) | — |
| `0.1.0-rc.6` 及更早 | ❌ 不支持 | WS 走的是两个旧端点(`/api/events.mux` + `/api/events.host`),envelope 是嵌套 envelope;本 bridge 读 `/api/remote.mux` 单端点,直接 404 |
| `0.1.2-rc.2` 及之后(若已发布) | ❌ 不支持 | method 可能改名、envelope 字段可能增减、`session/follow` 流形状可能变;落进 unknown-method warn,events 全部丢 |
| `nightly` / `latest` track | ❌ 不支持 | bridge 不做版本探测 |

### 0.2 attach 路径 + runtime 多版本

**attach 路径**(新,2026-09-11 实测验证):dsh 0.1.2-rc.1 的 dsh-auth cookie 签名 secret 持久化在 `~/.dsh/.credentials.yaml`(见 §7.1)。nightme 读这个 secret 本地签 cookie,**跳过 launch token exchange 直接 attach 到同 secret 的 dsh**(详见 §7.2)。launch token 仅在 spawn 自己 dsh 时用 — 见 §2.1 / §3.1。

**runtime 多版本**:每个 nightme worktree 加载自己的 `dsh_api_gateway` / `dsh_session_controller` 源码,wire 必须对得上 dsh 0.1.2-rc.1。dsh 协议在不同 rc 之间有 break change,nightme 不能 runtime 兼容多版本 → **dsh 升级 = nightme 同步升级**(升级流程见 §0.3)。

### 0.3 改 dsh 版本时的流程

1. **实机抓包 + 源码对照**:目标版本的 `@deepseek-ai/dsh-api-gateway` + `@deepseek-ai/dsh-api-session-controller` 仓内 contract,本机 `dsh --profile web` 起服务,用 §3 的 wire 形状逐一核对
2. **更新本文件 §0.1 兼容性矩阵**:标记新版本为目标,移除旧版本(或写"已淘汰")
3. **同步更新 README 的 DSH 行 + Prerequisites**(end user 看的版本说明)
4. **CI 测试**:本仓的 `internal/bridge/dsh/host/*_test.go` 起 fake dsh subprocess,wire 形状对得上才能跑通;新协议下需要更新 mock
5. **如果 dsh 升级到非 `@latest-stable`(例如又发了 0.1.2-rc.2)**:在 PR 里同时改 bridge 代码 + 本文件 + README,不能分开发布

### 0.4 装错版本怎么排查

| 症状 | 原因 | 排查 |
|---|---|---|
| `dsh.host: token-exchange: HTTP 401` | 用的不是 nightme spawn 的 dsh(可能 PATH 里另有别的 dsh) | `which dsh` 确认,`dsh --version` 看版本 |
| WS upgrade 立刻断,log 报 `unexpected Sec-WebSocket-Protocol` 或 `HTTP 400` | dsh 比 0.1.2-rc.1 新,protocol negotiation 失败 | 装回 `npm i -g @deepseek-ai/dsh@0.1.2-rc.1` |
| events 全丢,log 里全是 `dsh: mux unknown method` | dsh 协议比 0.1.2-rc.1 新,envelope 形状变了 | 同上 |
| `mint dsh-auth cookie: no Set-Cookie in response` | dsh 协议比 0.1.2-rc.1 旧,token exchange 路径不存在 | 升级 dsh 到 0.1.2-rc.1 |

bridge 不做硬卡(故意留软,方便先排查别的问题),但症状都对得上"装错版本"。

---

## 1. dsh 是什么

[DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness) 是 DeepSeek AI 开源的 agent harness,核心口号 "everything is a plugin",由 Cordis 框架驱动。CLI 名 `dsh`,MIT 协议,主语言 TypeScript。

```bash
npx @deepseek-ai/dsh web              # 默认 Web UI,127.0.0.1:3080
pnpm dsh --profile headless "<task>"  # 单 turn print-mode,plain stdout(本 bridge 不用)
```

nightme 唯一接入入口是 `dsh --profile web`(HTTP+WS shared host)。每条 prompt 经 nightme 包装为一次 `session.create` + `session.prompt` + `session/follow` 订阅,dash 和 nightme session 在同一 dsh 进程内并行。

---

## 2. 入口与启动

### 2.1 spawn 配方

```bash
dsh --profile web --port 3080
```

| 旗 | 值 | 原因 |
|----|----|------|
| `--port` | 3080(显式锁) | 不依赖 dsh 默认值;3080 被占走 §2.2 fallback |
| `--port 0` | 禁止 | dsh 实机不接受,会随机到奇怪端口,与"nightme 拥有 dsh"契约冲突 |
| `--no-open` | nightme spawn 时**不**传 | 需要 dsh 把 launch URL 吐到 stdout 给 nightme 抓;开浏览器不是 nightme 的事 |

**attach-then-spawn 决策**: dsh 0.1.2-rc.1 用 `~/.dsh/.credentials.yaml` 里 `client-connection/browser-session.secret` 签 dsh-auth cookie(见 §3.1 + §7.1)。nightme 直接读这个 secret,本地 mint 出合法 cookie,跳过 launch token exchange。所以 **attach 优先**:StartSharedHost 第一步用我们 mint 的 cookie 探活 3080,验证通过就直接 attach(ownsProcess=false,不 kill 用户 dsh);不通过再扫 [3081, 3099];都失败才 spawn 自己的 dsh。

### 2.2 端口策略

```
首选 3080(显式 --port)
    ↓ TCP-dial reachable → 用 mint cookie 探 /api/session/list
    ↓    ↓ 200 OK → attach 此 dsh(共享同一个 ~/.dsh/ secret)
    ↓    ↓ 非 200 / 不可达 → fallback 扫 [3081, 3099]
    ↓ 全部被占或 cookie 拒绝
报错: "dsh.host: no free port in range 3080-3099"
```

`findFreePort(base, range)` 用 `net.Listen` 测试,扫不到 → fail loud。

**用户已启 dsh on 3080 的场景**: nightme attach 该 dsh,**复用其 session 历史**。session/cookie/WS 都由用户的 dsh 提供,nightme 不再 spawn 第二个 dsh。用户在 dashboard 看到的自己 session 与 nightme 跑的 session 走同一个 dsh 进程,**两者一致**。

**特例:用户 dsh 占 3080 但用的是别的 secret**(极少见 — 比如两个独立账户),cookie 验证失败 → nightme fallback spawn 自己的 dsh(独立 secret,独立 session 历史)。

### 2.3 Readiness

```
spawnAndWire (host/lifecycle.go):
  cmd.Start ──→ drainStdout (parse launch URL)
              ↓
              waitForListen(ctx, port)  # TCP-poll,每 50ms 拨号
              ↓
              mintAuthCookie(baseURL, token)
              ↓
              hub.Start(ctx)  # 启 WS pumps
```

**Readiness 判定用 TCP accept,不解析 stdout 文本**:
- stdout 第一行 `dsh web: http://127.0.0.1:PORT/?token=<launchToken>` 用正则 `dsh web:\s+(http://[^\s]+)` 捕获完整 URL → `url.Query().Get("token")` 提 token
- 但**不**用 stdout 文本判定端口(以防 dsh 漂移默认端口): `--port` 旗是 nightme 传的,自己知道
- TCP-poll 捕获 kernel-accept-queue vs app-Accept race:`waitForListen` 在 `dsh.bind` 系统调用返回但 `app.Accept` 还没拉起时就能 TCP-dial 通过

---

## 3. wire 协议(dsh 0.1.2-rc.1 实测)

### 3.1 Auth:launch token → dsh-auth cookie

dsh 0.1.2-rc.1 起**强制 per-process signed-cookie auth**:`/api/*` 和 `/api/remote.mux` 都用 `dsh-auth-<sha256(authority)>=<signed-payload>` cookie 校验,launch token 仅在初始 `GET /` 上有效。

```
GET http://127.0.0.1:3080/?token=<launchToken>
Cookie: (无)
→ 期望:
  HTTP/1.1 303 See Other
  location: /
  set-cookie: dsh-auth-<hash>=v1.eyJ2ZXJzaW9uIjoxLCJhdXRob3JpdHkiOiIxMjcuMC4wLjE6MzA4MCIs...;
             Max-Age=2592000; Path=/; HttpOnly; SameSite=Strict
```

**nightme mint 流程**(`host/lifecycle.go::mintAuthCookie`):

```
1. 新建 cookiejar.Jar
2. GET /?token=<launchToken>,CheckRedirect 限制一次跳(303 → / 后立即返回)
3. resp.Cookies() 拿到 dsh-auth-*
4. jar.SetCookies(u, cookies) 注入
5. NewRPCClientWithHTTP(baseURL, httpClientWithJar) ← RPC 用 jar
   NewStreamHubWithJar(baseURL, jar, log, ...) ← WS 用 jar
```

**为什么不 attach 到外部 dsh**: launch token 是 dsh 进程内私有,既不写文件也不暴露 API(grep `processLaunchToken` 全在内存对象上);`/proc/<pid>/environ` / `/proc/<pid>/fd` 在 Linux 上可读但 macOS 没 procfs,跨平台不可靠;即使能读到 token,外部 dsh 的 `secret` 也是 per-process 的,伪造不出 cookie。

### 3.2 HTTP RPC

```
POST /api/<namespace>/<method>          # SLASH,not dot
Content-Type: application/json
Cookie: dsh-auth-<hash>=<signed>

{
  "type":   "client-request",
  "rpcId":  "<uuid>",
  "method": "<namespace>/<method>",     # SLASH,匹配 URL path
  "payload": {                          # 强制 args 包装
    "args": { ...endpoint-specific... }
  }
}

→ 200 OK
{
  "type":   "server-response",
  "rpcId":  "<同 uuid>",
  "result": {
    "ok":    true,
    "value": {...}
  }
}
// 或:
{
  "result": { "ok": false, "error": { "code": "bad-request", "message": "...", "details": {...} } }
}
```

**关键不变量**:
- `method` 字段用 SLASH(`session/prompt`),不用 DOT(早期假设已废)
- URL path 也用 SLASH(`/api/session/prompt`),与 `method` 一致
- `payload` 必须包 `{args: ...}`,即 `args` 是 typert 的固定 entry key,不允许省略
- `rpcId` 必须 echo,dsh 用它关联请求-响应

**常用方法**(typert 强校验,任意字段缺失就拒):

| Method | payload.args | 备注 |
|--------|--------------|------|
| `session/create` | `{request: {workspaceId\|cwd, sessionId?, agentPreset?}}` | `workspaceId` 与 `cwd` 互斥(typert strict) |
| `session/list` | `{_request: {cursor?}}` | **wrapper key 是 `_request`**,不是 `request`(typert 独此一家) |
| `session/prompt` | `{request: {requestId, sessionId, mode, content, clientTimeZone?}}` | `requestId` 必填(client-minted UUID,server 用它去重);`mode` ∈ `queue`\|`steer` |
| `session/cancel` | `{request: {sessionId}}` | best-effort,idle 时返 `session-not-found` |
| `session/fork` | `{request: {sessionId, atSeq?}}` | daemon 重启续接用 |
| `session/models` | `{request: {sessionId}}` | 查可用 model |
| `session/selectModel` | `{request: {sessionId, provider, model, reasoningEffort?}}` | 切 model |
| `session/follow` (stream) | `{request: {address: {kind:"session", sessionId}, maxMessages?}}` | 见 §3.5 |
| `session/control` (stream) | `(无参)` | 基线 + jobs/queue 替换帧 |
| `workspace/create` | `{request: {path}}` | path 必须绝对;`created:bool` 指示是否新建 |
| `workspace/archiveSession` | `{request: {sessionId}}` | 隐藏 session row(workspace 保留) |
| `workspace/list` | `(无参)` | 列 workspace 视图 |
| `commands/execute` | `{agentId, line, images?}` | **flat-arg**,不走 `request` wrapper(typert 直接接 args) |
| `settings/describe` | `(无参)` | 读 settings 树 |
| `credentials/describe` | `(无参)` | 列 credential refs |
| `respond` | (二方 envelope,不走 client-request) | 服务端推送帧的应答回环,见 §3.7 |

**bridge 实现**: `host/client.go::RPCClient.Post(ctx, method, args)` 把 caller 传的 `args` 用 `wrapArgs` 包成 `{args: <args>}`,再 `methodDotsToSlashes(method)` 转 SLASH,最后 `json.Marshal` 进 `clientRequest` envelope。**caller 直接传 inner body**,不用关心 `args` 包装和 slash 转换。

### 3.3 WebSocket: `/api/remote.mux`

dsh 0.1.2-rc.1 把 `/api/events.mux` + `/api/events.host` 合并成单一 `/api/remote.mux`,wire 改成自定义 JSON 帧,**不再是 gorilla server-push**。

**升级握手 cookie quirk**: Go stdlib `cookiejar.Jar.Cookies(wsURL)` 对 `ws://` 永远返空(实现里 `if u.Scheme != "http" && u.Scheme != "https" return cookies`)。`host/stream.go::connectAndServe` 用 `websocket.NewClient` + 手工把 `Cookie: name=value` 头塞到 upgrade request 上。

**子协议 quirk**: dsh 用 `ws` 库严格校验 `Sec-WebSocket-Protocol`,只接受空或不认识的子协议;带自定义值返 `Invalid Sec-WebSocket-Protocol header` 400。**bridge 永远不设 `Sec-WebSocket-Protocol`**,让 `Dialer` 协商到 "no protocol"。

**帧类型**:

```
client → server:
  {type:"open",   streamId, endpoint, payload:{args:{...endpoint-specific...}}}
  {type:"cancel", streamId}

server → client:
  {type:"ready",  clientId, host:{home:"..."}}   # 仅 $events 第一帧
  {type:"item",   streamId, value}              # 业务帧
  {type:"end",    streamId}                     # 流结束
  {type:"error",  streamId, error:{code,message,details}}
```

**严格校验**: `parseRemoteStreamClientMessage` 对 `open` 强制 `exactKeys(type, streamId, endpoint, payload)` — 任何额外字段返 `invalid Remote stream request` 然后 1008 close。

**两类逻辑流共享一条物理连接**:

| streamId | endpoint | 用途 |
|----------|----------|------|
| `host-$events`(固定) | `$events` | daemon-global Host lifecycle 帧 |
| `sess-<N>`(bridge mint) | `session/follow` | 单 session event 流,每个 active session 一个 |

**$events stream** payload:
```
{args: {}}   # 严格空对象
```

**session/follow stream** payload(typert 强校验):
```
{
  "args": {
    "request": {                        # ★ 套 request wrapper(不是平铺 address)
      "address": {kind:"session", sessionId:"session-<uuid>"},
      "maxMessages": 50                # optional
    }
  }
}
```

### 3.4 $events stream items

dsh 0.1.2-rc.1 在 `openRemoteEvents` / `broadcastRemoteEvent` / `startRemoteEvent` 三处 push 不同 item shape:

```jsonc
// 首帧
{ "type": "ready",     "clientId": "<uuid>", "host": {"home": "/Users/geax"} }

// 广播 Cordis event(无 id,args 数组)
{ "type": "emit",      "event": "api-session/status",
  "args": ["session-xxx", true] }

// 作用域 event(待回应,有 eventId)
{ "type": "waterfall", "event": "approval/asked",
  "eventId": "<uuid>", "agentId": "<uuid>", "request": {...} }

// server-side cancel 一个 waterfall
{ "type": "cancel",    "eventId": "<uuid>" }
```

bridge 翻译(`host/stream.go::translateHostEvent`):
- `emit` → `method=event, rpcID=""`,payload 包 `{args:[...]}`
- `waterfall` → `method=event, rpcID=eventId`,payload 包 `{agentId, request}`
- `cancel` → `method="host/cancel", rpcID=eventId`,payload 包 `{eventId}`
- `ready` → 不翻译,dispatch 路径只 log(`dsh.host: mux ready clientId=...`)

### 3.5 session/follow stream items

typert `SessionFollowFrame` 一帧:

```jsonc
// 初始 snapshot
{
  "type": "snapshot",
  "header":     {version, id, createdAt, cwd?, parentSession?, ...},
  "cursor":     <lastSeq>,
  "records":    [{type:"event", event:{...}}, {type:"chunks", event:{...}}],
  "hasMore":    <bool>,
  "projections": {asOfSeq, values:{...}}
}

// 增量 event
{
  "type": "event",
  "event": {
    "type":   "<discriminator>",     // 见下表
    "seq":    <int64>,
    "time":   <int64 unix millis>,
    "data":   {...payload-specific...},
    "ignorable":     <bool?>,
    "sourceEventSeqs":<int[]?>,
    "surfaceOp":     "append"|{op:"replace",start,end}?
  }
}
```

**session event.type 判别子**(drain 实机抓到 14 种,`request/header` 是新增的未在 dsh.md 早期版本列出的):

| event.type | 含义 | bridge 处理 |
|------------|------|--------------|
| `agent/inbox/spliced` | turn inbox splice | log;不 emit event |
| `turn/start` | 一次 turn 开始 | dispatcher 清 turn 状态 |
| `turn/end` | 一次 turn 结束 | `EventResult` + `EventDone{Reason:"settled"}` |
| `step/start` | 模型推理+工具链一次 | 不 emit;统计 ttft/tok-s |
| `step/end` | 同上 | 不 emit;统计 |
| `user/message` | 用户消息回显 | 丢弃(runtime 已从 inbound 知道) |
| `session/title` | session 标题 | 静默 |
| `session/title-llm-request` | 标题 LLM 请求中 | 静默 |
| `request/header` | 推理 header | 静默 |
| `request/context` | 上下文注入 | 静默 |
| `usage` | token usage | 静默(汇总到 turn/end) |
| `assistant/chunk` | 流式 chunk | `translate.go::handleAssistantChunk` 按 `chunk.type` 分流 |
| `assistant/message` | 完整 message | `translate.go::handleAssistantMessage` |
| `compaction/end` | 上下文压缩完成 | 静默(本期不渲染) |
| `todo/write` | 任务条更新 | `applyTodoProjection` |
| `approval/asked` | approval 审计 echo(`approval/asked` + `approval/decided` 配对) | session/event 上的 audit echo;**不是** respondable gate — 真正的 replyable gate 在 host `$events` waterfall 上的 `approval/request`,见 §3.8 |

**`assistant/chunk` 内部 `chunk.type` 子分派**(dsh 0.1.2-rc.1 实机抓):

| chunk.type | 含义 | bridge 处理 |
|------------|------|--------------|
| `block-start` | ContentBlock 开始,`blockType` ∈ `text`/`tool-call`/`image`/`tool-result` | 累积,不发 event |
| `text-delta` | 文本流片段 | 累加 `textBuf[idx]`,**不**逐 delta emit(避免飞书卡刷屏) |
| `reasoning-delta` | 思考流片段 | 累加 `reasoningBuf[idx]`,**不** emit |
| `block-end` | ContentBlock 收尾,`block.text` 整段定型 | `block.type=="reasoning"` 时整段 emit 一条 `[思考] ...` |
| `tool-call-delta` | 工具参数 JSON 增量 | 累加 |
| `usage` | chunk 流结束时的 usage | 静默(汇总) |
| `finish` | 推理 finish | 静默 |

### 3.6 typert 严格校验

dsh 0.1.2-rc.1 的 `@deepseek-ai/dsh-api-gateway` 用 `assertExactArguments(args, descriptor, endpoint)` 校验:

```ts
const expected = new Set(descriptor.parameters.map(p => p.wire))
const extra    = Reflect.ownKeys(args).filter(k => !expected.has(k))
const missing  = [...expected].filter(k => !Object.hasOwn(args, k))
if (extra.length || missing.length) throw "gateway/arguments-invalid"
```

**对 bridge 的影响**:
- 任何 wire 字段漂移立即返 `bad-request: missing "X"; unexpected "Y"`,把字段对一遍 dsh-api-gateway/lib/types/* 即可
- `session/list` 的 wrapper key 是 `_request`(typert descriptor 用 `_request` 命名),不是其他 session.* 的 `request`
- `session/prompt` 缺 `requestId` 返 `gateway/input-invalid: wire field "request" failed boundary validation`
- `session.prompt mode=""` 返 `bad-request: invalid input: expected "queue"`
- extra 字段(typert 未声明的)严格拒 — `requestId` 之外的 `requestIdLike` 字段都算 extra

### 3.7 respond 回环

`/api/respond` **不走** `client-request` envelope,用**二方 envelope**:

```jsonc
POST /api/respond
{
  "type":   "client-response",
  "rpcId":  "<echoed server-frame rpcId>",   # ★ 不是 client-minted,必须 echo 服务端 rpcId
  "result": { "ok": true, "value": <ApprovalResponse | QuestionResponse> }
}
→ {accepted: true}  // 走 PostEnvelope 不走 Post
```

bridge `host/client.go::RPCClient.Respond(ctx, frameRpcID, value)` 手工 marshal 这条 envelope,不经 `wrapArgs`。

---

### 3.8 Host waterfall wire (dsh 0.1.2-rc.1)

dsh 0.1.2-rc.1 把 `approval` + `AskUserQuestion` 都搬到了 host `$events` waterfall 流上 — 不再走 mux 顶层 method。源码依据:

- `@deepseek-ai/dsh-user-approval/lib/index.js::ApprovalService.request` → `ctx.waterfall("approval/request", req, …)`
- `@deepseek-ai/dsh-user-questions/lib/index.js::UserQuestionService.ask` → `ctx.waterfall("user-questions/request", request, …)`
- dsh-api-gateway 把这些 waterfall 转发到 `/api/remote.mux` 上 host stream 的 items,形状 `{type:"waterfall", event, eventId, agentId, request}`

`host/stream.go::translateHostEvent` 把它翻译成 bridge envelope:

```
waterfall → (method=<event>, rpcID=eventId, payload={agentId, request})
```

`request` 的内容按 `event` 分:

| event | request 形状(对应 dsh 包) |
|---|---|
| `approval/request` | `{agent: Agent, toolName: string, callId?: ToolCallId, reason?: string, signal?: AbortSignal}`(`@deepseek-ai/dsh-user-approval/types.d.ts::ApprovalRequestEvent`) |
| `user-questions/request` | `{questions: AskUserQuestionItem[], agent?: Agent, signal?: AbortSignal}`(`@deepseek-ai/dsh-user-questions/types.d.ts::AskUserQuestionRequestEvent`) |

`Agent.id === SessionId`(见 `@deepseek-ai/dsh-agent/lib/types/types.d.ts`),所以 root session 的 `request.agent.id == sessionId` — 这就是 demux key。

**bridge 适配**(`internal/bridge/dsh/host_waterfall.go`):

- `installHostHandler(cli)` 在第一个 driver 构造时一次性装全局 `cli.SetHostHandler(hostWaterfallHandler)`(幂等)
- `hostWaterfallHandler` 按 `payload.agentId` 查 `hostWaterfallBySess map[sessionID]*driver` → `driver.handleHostFrame`
- `driver.handleHostFrame` 把 waterfall envelope 适配成 mux envelope,调用现有的 `handleApprovalRequested` / `handleQuestionRequested`(`internal/bridge/dsh/permissions.go`),后者用同一份 `pendingApprovals` / `pendingQuestions` FIFO,reply key 仍是 waterfall 的 `eventId`
- `/api/respond` 的 client-response envelope(§3.7)在 host waterfall 路径上不变 — `rpcId` 字段直接 echo `eventId`

**为什么 mux 顶层 method 不再发**:旧 wire `approval/requested` / `question/requested` 在 0.1.0-rc.6 时代是 mux frame,0.1.2-rc.1 改成 Cordis waterfall 后不再发。`handleMuxFrame` 的兜底分支对任何 straggler 仍会 `recordAndCountUnknown` + warn(`"dsh: mux legacy method dropped"`),不进 permission 路径。

---

## 4. bridge 架构

### 4.1 包结构

```
internal/bridge/dsh/
├── doc.go                 # package doc
├── starter.go             # Starter + Info(ModeJSONIO) + Detect + Start + RunOnce
├── detect.go              # exec.LookPath("dsh") + `dsh web` smoke probe
├── session.go             # host.EnsureSharedHost + handshake + closeOnce + 翻译层
├── host/                  # shared host 子包
│   ├── client.go          # HTTP RPC client(POST /api/{ns}/{method},args 包装,slash 转换)
│   ├── stream.go          # WebSocket client(单 /api/remote.mux,generation 跟踪,翻译)
│   ├── router.go          # per-session 路由 + pending approval/question 表
│   ├── lifecycle.go       # spawn + auth mint + watchdog + restart recovery
│   ├── ensure.go          # 一次性 Start 全局 host + 并发安全
│   ├── health.go          # 周期性 health check
│   └── watchdog.go        # subprocess exit detection + backoff respawn
├── host/host_test.go      # StreamHub + Router mock 测试
├── host/wire_e2e_test.go  # 真 dsh RPC e2e(门控 wire_e2e tag + NIGHTME_TEST_DSH_URL)
└── session_test.go ...    # 翻译层 + 状态机单测
```

### 4.2 HTTP layer(`host/client.go`)

```
caller                                  wire
session.Prompt(cwd)                      args
  ↓ cli.RPC.Post(ctx, "session.prompt", {request: {cwd, ...}})   ← 业务字段
RPCClient.Post
  ↓ wrapArgs  →  {args: {request: {cwd, ...}}}                    ← typert 包装
  ↓ methodDotsToSlashes → "session/prompt"                        ← slash
  ↓ Marshal(clientRequest{type, rpcId, method, payload})           ← envelope
  ↓ http.NewRequestWithContext("POST", baseURL+"/api/session/prompt", body)
  ↓ httpClient.Do(req)        ← jar 自动带 dsh-auth cookie
  → rpcResponse → Result.OK / Result.Error
```

`wrapArgs` 与 `methodDotsToSlashes` 是 wire-drift 的两个吸收点 — caller 不必关心。

### 4.3 WebSocket layer(`host/stream.go::StreamHub`)

单物理连接 + 多逻辑流。核心不变量:

| 不变量 | 怎么保证 |
|--------|----------|
| 单写者 | `writeLoop` goroutine 独占 `conn.WriteMessage`;其他 goroutine 走 `writeCh` 排队 |
| 多读 OK | 实际只一个 `readLoop` goroutine,`dispatch` 同步分派 |
| 拒绝并发 `WriteMessage` | `writeLoop` 是 `WriteMessage` 唯一调用方;gorilla 内部 state 不并发安全 |
| 重连重开所有 sub | `connectAndServe` 每次 dial 完扫 `h.sessions`,mint 新 streamId 重新发 `open` 帧 |
| 订阅热路径不被锁阻塞 | 翻译 + 调 FrameHandler 在读 loop 内同步跑(handler 自己保证 non-blocking) |

**Generation 跟踪**(解决 Subscribe-after-Connect 不发 open frame 的死锁):

```
connectAndServe:
  thisGen := h.currentGeneration.Add(1)   # 本次连接独占的代数
  for sub in h.sessions:
    if sub.generation == thisGen:
      continue                          # Subscribe 已经发了 open,跳过
    newID := mintStreamID("sess")
    sub.streamID = newID
    sub.generation = thisGen
    open{streamId:newID, endpoint:"session/follow", payload:...} → writeCh

Subscribe(sessionID, _):
  sub := {streamID:mint("sess"), generation:currentGeneration.Load()}
  h.sessions[sessionID] = sub
  h.byStreamID[sub.streamID] = sub
  if h.conn != nil && !h.closed:
    writeCh <- open{streamId:sub.streamID, ...}   # 立即发
  # 立即发的 sub.generation == thisGen
  # 下次 reconnect 时 toReopen 跳过它(避免 duplicate streamId → 1008 close)
```

**为什么需要这个**: 旧实现是 `connectAndServe` 单点建 `toReopen` 列表,Subscribe 不发任何东西。如果 `Start` 先连上 WS,再 `Subscribe(sid)`,这个 sid 永远进不了 `toReopen`,dsh 端从不知道 nightme 想要这个 session 的事件。修法: Subscribe 在 hub 已连接时立即发,sub 用 generation 标记自己"已发",reconnect 时不再 mint 新 streamId。

**dispatchWG 不再是 WaitGroup**: `sync.WaitGroup.Add(1)` 在并发 dispatch 热路径 + `Close` 的 `Wait()` 有 race(Go 文档: "Add(1) when counter is 0 must happen-before Wait")。`markDispatchStart` / `markDispatchDone` / `waitDispatchDrain` 改用 `sync.Mutex + counter + sync.Cond`:

```
markDispatchStart:  cond := sync.NewCond(&mu); count++
markDispatchDone:   count--; if count == 0: cond.Broadcast()
waitDispatchDrain: for count > 0: cond.Wait()
```

`cond.Locker` 由 `Cond.init` 时懒初始化,保持 struct literal 可读。

### 4.4 翻译层(`host/stream.go::translate*` + `session.go::handleMuxFrame`)

`session/follow` item 的 wire 形状 (`SessionFollowFrame`) 与 bridge 内部的 `{method, rpcID, payload}` envelope **不一样**。翻译层在 `StreamHub.dispatch` 完成:

| 物理 WS item.value | envelope `{method, rpcID, payload}` |
|--------------------|--------------------------------------|
| `{type:"snapshot", header, cursor, records, ...}` | `method="session/snapshot", payload=<raw snapshot>` → `handleMuxFrame::replaySnapshot` 解 records,逐个走 `dispatchEvent` |
| `{type:"event", event:{type, seq, time, data, ...}}` | `method=event.type`,`rpcID="seq-N"`,`payload=event.data + sessionId` |

`Router.DispatchMux(method, rpcID, payload)` 按 `payload.sessionId` 分派到 per-session handler → `handleMuxFrame(method, rpcID, payload)`。

`handleMuxFrame` switch:

- `session/snapshot` → `replaySnapshot` 解 records 逐个 `dispatchEvent` + `bumpLastSeq(cursor)`
- `session/subscribed` / `session/projection` → 已废,本版无 dsh 会发这些
- `approval/requested` / `approval/resolved` / `question/requested` / `question/resolved` → dsh 0.1.2-rc.1 不再发;若到则 `recordAndCountUnknown` + warn(`"dsh: mux legacy method dropped — dsh 0.1.2-rc.1 sends this as host waterfall"`),不 panic。真正的 respondable gate 在 host waterfall,见 §3.8。
- `assistant/chunk` / `turn/start` / `step/end` / `user/message` 等新 wire method → `isSessionEventType` 白名单 → 构造 `sessionEventEnvelope{Type:method, Seq:parseSeqFromRPCID(rpcID), Data:payload}` → `dispatchEvent` → `dispatcher.dispatch` → registry handler
- `host/cancel` → log
- 其它 → `recordAndCountUnknown` + warn log

`dispatcher.dispatch` 在 `eventRegistry` 里查 `env.Type` 命中即调 `handleAssistantChunk` / `handleAssistantMessage` / `handleTurnStart` / `handleTurnEnd` / `handleTodoWrite` 等,miss 走 `unknownCount++`。

`$events` item 的翻译在 `translateHostEvent`:

| 物理 item.value | envelope |
|------------------|----------|
| `{type:"emit", event, args:[...]}` | `method=event, rpcID=""`,payload=`{args:[...]}` |
| `{type:"waterfall", event, eventId, agentId, request}` | `method=event, rpcID=eventId`,payload=`{agentId, request}` |
| `{type:"cancel", eventId}` | `method="host/cancel", rpcID=eventId` |
| `{type:"ready", clientId, host:{home}}` | 不翻译,dispatch log 完事 |

### 4.5 Router / Dispatch

`host/router.go::Router` 维护 `map[sessionID]MuxFrameHandler` + `map[(sessionID, rpcID)]chan ApprovalDecision` + `map[(sessionID, rpcID)]chan QuestionDecision`。

`DispatchMux(method, rpcID, payload)`:

1. `extractSessionID(payload)` → sessionID
2. `subs[sessionID]` 命中 → `handler(method, rpcID, payload)`
3. miss → drop + `recordAndCountUnknown`

`handler` 是 `session.go::handleMuxFrame` 的方法值,继续走 dispatcher / approval / question 等分支。

approval 答案回环: `pendingApprovals[approvalId] <- decision`;`RPCClient.Respond` 发 `/api/respond` 用 `client-response` envelope(见 §3.7),`rpcId` 必须 echo server 推送的 `approval/requested.frameRpcID`(`approvalId` 是 audit-only,不是 answer key)。

**Host waterfall demux**(dsh 0.1.2-rc.1):host `$events` 上的 waterfall(`approval/request`、`user-questions/request`)不携带 mux 顶层 `sessionId`,但 `request.agent.id == sessionId`(root session)。`host_waterfall.go` 用包级 `hostWaterfallBySess map[sessionID]*driver` 维护 demux 表,`installHostHandler(cli)` 在第一个 driver 构造时把全局 `cli.SetHostHandler(hostWaterfallHandler)` 装好,后续 driver 只 register 自己;`registerDriverForWaterfall(d)` / `unregisterDriverForWaterfall(d)` 在 `newDriver` / `Reset` / `Close` 钩子上调。`hostWaterfallHandler` 按 `payload.agentId` 查表 → `driver.handleHostFrame`,后者把 waterfall envelope 转成 mux envelope 形状调用现有的 `handleApprovalRequested` / `handleQuestionRequested`,reply key 仍是 waterfall 的 `eventId`(`/api/respond` envelope 不变)。

---

## 5. lifecycle

### 5.1 一次性 spawn(daemon 启动)

```
EnsureSharedHost(cfg) (host/ensure.go)
  ↓ 一次性
StartSharedHost:
  1. findFreePort(3080, 3081..3099)         # 端口策略
  2. proc.New("dsh", "--profile", "web", "--port", P)
  3. cmd.Start
  4. drainStdout goroutine:
       parse "dsh web: http://.../?token=..." → token
       send to launchURLCh (buffered, non-blocking)
  5. waitForListen(ctx, P)                  # TCP-poll 50ms
  6. mintAuthCookie(baseURL, token)         # GET /?token=... → 303 + cookie
  7. jar → NewRPCClientWithHTTP
         → NewStreamHubWithJar
  8. hub.Start(ctx)                         # 起 mux pump
  9. return &SharedHost{cmd, RPC, Hub, Router, ...}
```

### 5.2 一次 session(Start 长生命周期)

```
client.Subscribe(sessionID, cwd, handler)         # 走 cli.Subscribe = Router.Subscribe + Hub.Subscribe
  ↓
Hub.Subscribe:
  mint streamId, generation = current
  if h.conn != nil && !closed: writeCh <- open{...}   # 立即发
  else: 等 connectAndServe 的 toReopen 兜底
  return unsubscribe()

client.SessionCreate(cwd) / SessionPrompt(...)
  ↓
RPCClient.Post → /api/session/prompt {args: {request: {requestId, sessionId, mode, content}}}

WS 帧回流:
  session/follow item.value
    → translateSessionEvent
    → (method=event.type, rpcID="seq-N", payload=event.data+sessionId)
    → Router.DispatchMux
    → handleMuxFrame
    → dispatchEvent → dispatcher.dispatch → handler (handleAssistantChunk / handleTurnStart / ...)
    → driver.deliver → session.Events chan → runtime.pumpOutbound → OutboundMessage → channel.Send

agent.Close():
  1. Router.Unsubscribe(sessionID)
  2. session.cancel (best-effort)
  3. workspace.archiveSession (★ 隐藏 row;workspace 保留跨 session)
  4. close(events chan); wait lifecycle
```

### 5.3 一次性 RunOnce / Review

```
RunOnce := Start(cfg) + SendBlocks + drain → RunResult + defer a.Close()
Review  := RunOnce(prompt=agent.StandardPrompt())
```

`defer a.Close()` 触发 R4 隐藏 session row — `workspace.archiveSession(sessionID)` 是关键,见 §4.5。

**为什么用 archiveSession 不用 workspace.delete**: workspace 是 repo-scoped(`git rev-parse --show-toplevel`),跨 driver 共享 — 同一个 git repo 的 chat session 与 `/review` run 共用 workspace;删掉会牵连其他 active session。`archiveSession` 只隐藏 session row,workspace 持久。

### 5.4 重启 / 崩溃恢复

```
watchdog 检测到 dsh 子进程 exit:
  attempt < maxRespawnAttempts(=5):
    backoff(attempt)
    fork/exec dsh again
    post-respawn recovery:
      ReattachSubscriptions:
        for active session in router:
          workspace.resume(session.workspaceID)
          session.fork(session.sessionID) → newSessionID
          client.Subscribe(newSessionID, ...)
  attempt >= maxRespawnAttempts:
    set h.dead = true; 错误返回 EnsureSharedHost
```

`workspace.fork` 不是"原地接管"(dsh web 设计上不允许),而是 server-side 复制 session 内容到新 sessionId,旧 sessionId 仍可查(供 audit / 二次 fork)。bridge `SessionList` + `session.fork` 是 daemon 重启续接的 wire 路径。

---

## 6. 关键设计决策

### 6.1 为什么单独 `host/` 子包,不分到 `internal/bridge/dsh/`

shared host 是**多 session 共享**的,任何 ChatSession / AgentSession 都从 `host.GetGlobal()` 拿同一个 `*Client`。`host/` 把"全局 dsh 进程 + RPC + WS + Router"封装成单例,`dsh/` 只管"单 session 生命周期 + 翻译层 + 与 agent 包的接口"。Phase 3 之前共存,Phase 3 时把 envelope 类型 / 翻译层 / session.go::handleMuxFrame 合并到 `host/` 后再删 `dsh/`。

### 6.2 为什么 nightme 现在可以 attach 用户自启的 dsh

**早期假设**(2026-09-11 之前):§3.1 论证 dsh 0.1.2-rc.1 的 launch token 是进程内私有,跨进程拿不到,所以 nightme 无法 attach 到别人跑的 dsh。

**实测推翻**(2026-09-11):`@deepseek-ai/dsh-client-connection/lib/index.js::BrowserAuth` 不只靠 launch token — 它用 `client-connection/browser-session.secret` 签 cookie。这个 secret 持久化在 `~/.dsh/.credentials.yaml`,**所有共享同一个 `~/.dsh/` 目录的 dsh 都加载同一份 secret**。

所以 nightme 可以本地读 secret + 签出 dsh-auth cookie,**跳过 launch token exchange 直接 attach**。`host/lifecycle.go::tryAttachExistingDSH` 在 `StartSharedHost` 第一步走这个路径:

1. mint cookie from `~/.dsh/.credentials.yaml` 的 secret
2. POST `/api/session/list` 用 minted cookie 验证
3. 200 OK → attach,ownsProcess=false(不 kill 用户的 dsh)
4. 非 200 → fallback sweep [3081, 3099]
5. 都失败 → spawn 自己的 dsh

**好处**:daemon 重启不再丢失 session 历史(用户 worktree 持续工作),也不再有"daemon 在 3081+ 留一堆孤儿 dsh"的 sprawl 模式。

### 6.3 为什么有 `host/stream.go::translateSessionEvent` 翻译层

物理 WS item 是 `SessionFollowFrame`(有 `type:event|snapshot` 二态),bridge 内部 envelope 是 `{method, rpcID, payload}`(Router 按 method 路由、handler 按 method 分支)。两者**不**是一回事。翻译层把 wire 上的 event.type 拆出来当 method,event.data 当 payload,event.seq 当 rpcID。这样:

- `handle_mux.go` switch 用业务概念 method (`assistant/chunk` / `turn/start` / `step/end`),与 dsh 早期 wire(`session/event` envelope 套 envelope)解耦
- `Router.DispatchMux` 不用关心 wire 细节,只按 `{method, rpcID, payload}` 路由
- `dispatcher.dispatch` 的 registry 按 env.Type 查 handler,与 wire 形状解耦

代价:每次 wire 大改要重写翻译层 + 测试 fixture。但 wire 改的频率远低于 bridge 逻辑改的频率,值。

### 6.4 为什么 `host/client.go` 自己做 `methodDotsToSlashes` 和 `wrapArgs`

caller 不必记:
- method 名用 SLASH 还是 DOT
- payload 要不要包 `args`
- `session.list` wrapper 是 `request` 还是 `_request`
- `commands/execute` 是 typed(`request:`)还是 flat-arg

`RPCClient.Post` 集中这些 wire 细节。如果 dsh 后续再改 wire,只动 `host/client.go` + `host/stream.go` 两个文件,caller 0 修改。

### 6.5 `dispatchWG` 不再用 `sync.WaitGroup`

Go 文档原文: "Note that calls with a positive delta that occur when the counter is zero must happen before a Wait." 我们的热路径 (`dispatch` 在 `readLoop` 里) 调 `Add(1)`,Close 调 `Wait()`,跨 goroutine + 没 happens-before 边 → race detector 偶尔会抓到 "Wait returned before Add(1)"。改用 `sync.Mutex + counter + sync.Cond`,所有 Add/Done/Wait 都过同一把锁,counter 是单一真相。代价:多一次 mutex acquire(单帧 ns 级 vs WS 帧 ms 级,忽略)。

### 6.6 generation 跟踪

§4.3 详细论证。核心:让 `Subscribe` 在 hub 已连接时立即发 open 帧,但用 `sub.generation == currentGeneration` 标记,让 `connectAndServe.toReopen` 跳过(否则会 mint 新 streamId + 重复 open → server 1008 close)。hub 重连时 `currentGeneration.Add(1)`,老 sub 的 `sub.generation < currentGeneration` 才重 mint,新的跳过。

### 6.7 `request/header` 等新 event.type 不在 dispatcher registry 里

discoverer 策略是 `recordAndCountUnknown` 计数 + warn log,而不是 fail-fast。好处: dsh 加新 event type 不会让 nightme 崩溃(graceful degradation),ops 通过 `DumpWireStats` 看 unknown count 决定是否升级 bridge。坏处: 新事件被无声丢弃,直到有人升级 dispatcher。trade-off 选前者(用户实机验证发现 `request/header` 是新增的,dump 可见,补 handler 是 PR 级别的工作)。

---

## 7. Operational lessons(实机 dsh 0.1.2-rc.1 接入经验,2026-09-11)

本节按"踩坑 → 根因 → 修复"格式记录,方便下次接入时直接复用。

### 7.1 dsh-auth cookie 是 HMAC-SHA256,不是 JWT

**踩坑**:早期假设 dsh-auth 走 launch token exchange(GET `/?token=...` 拿 303 + Set-Cookie),以为 cookie 是不可伪造的 — 所以 nightme 不能 attach 到用户自启的 dsh。

**实测推翻**:`@deepseek-ai/dsh-client-connection/lib/index.js:272-277` 的 `encodeCookie`:

```js
cookieName(authority) = "dsh-auth-" + base64url(sha256(authority))
cookieValue = "v1." + base64url(JSON payload) + "." + base64url(HMAC-SHA256(secret, body))
payload = {version:1, authority:"<host:port>", issuedAt:<ms>, expiresAt:<ms>}
```

secret 持久化在 `~/.dsh/.credentials.yaml` 的 `client-connection/browser-session.payload.secret`(32 字节,**URL-safe base64 不带 padding**)。所有共享这个 `~/.dsh/` 目录的 dsh 进程都加载同一份 secret。

**byte-for-byte 验证**(`TestMintDSHAuthCookieFromCredentials_MatchesRealCookie`):用 2026-09-11 实机 dsh 0.1.2-rc.1 在 `:3088` 捕获的真实 cookie 做 fixture,自己写 HMAC 重新签名,产出**完全一致**的 cookie。

**生产代码**:`host/lifecycle.go::mintDSHAuthCookieFromCredentials` + `loadBrowserSessionSecret` 用 `gopkg.in/yaml.v3` 解析 `.credentials.yaml`,按上面的算法签名。错误情形(secret 不在 / 文件不可读 / YAML 损坏)会 fail loud,不静默 fallback。

### 7.2 tryAttachExistingDSH 比 spawn 优先级高

**踩坑**:早期 `StartSharedHost` 看到 3080 被占就直接 fallback 到 3081,造成"daemon 重启一次就 spawn 一个新 dsh"的 sprawl,长期下来 3080-3099 散落 4-5 个 dsh 进程。

**修复**:`host/lifecycle.go::tryAttachExistingDSH` 用 mint 的 cookie 验证 3080 可用 → 试 3081-3099 → 都失败才 spawn:

```go
port := defaultDSHPort  // 3080
if attached, h := tryAttachExistingDSH(ctx, logger); attached {
    return h, nil  // 复用用户的 dsh
}
// 才 fallback
```

**注意**:`attachedSharedHost.ownsProcess=false`,watchdog 不能 kill 它。如果用户的 dsh 死了,nightme daemon 不知道 → 下次 `/new` 时再次 attach,会 retry;如果还是失败,fallback spawn。

### 7.3 `cli.Start(ctx)` 必须用长生命周期 ctx,不能复用探测 ctx

**踩坑**(2026-09-11 实测):`tryAttachExistingDSH` 里用 `probeCtx` (10s timeout) 调用 `cli.Start(probeCtx)`。attach 成功后,Hub 的 WS pump 收到 10s 超时 → exit → session events 停止回 → 1 分钟后 `agentsession: readpump stalled`。

**修复**(`lifecycle.go:383`):`cli.Start(ctx)` 用 `ctx`(StartSharedHost 的 caller ctx,daemon lifetime),**不用** `probeCtx`。`probeCtx` 只给 cookie 探测用,attach 成功后立刻 cancel。

```go
// WRONG:
if err := cli.Start(probeCtx); err != nil { ... }
// RIGHT:
if err := cli.Start(ctx); err != nil { ... }  // caller ctx, lifetime = daemon
```

**症状模式**:用户 log 出现 `agentsession: readpump stalled (no events in threshold while in-flight); marking suspect` + attach 之后看不到 `mux stream connected`(deferred open 没 fire)→ 怀疑 Hub pump 已死。

### 7.4 `session.list` 是 POST + slash,不是 GET + dot

**踩坑**:`TestTryAttachExistingDSH_ReusesRunningDSH` 第一版用 `client.Get(baseURL + "/api/session/list")` → **HTTP 404**。以为是 cookie 没签对,debug 了半天。

**根因**:`@deepseek-ai/dsh-api-session-controller/lib/typert.host.js` 把 `session.list` 标成 typed POST + args wrapper(`{_request:{}}`)。method 在 envelope 里是 `session/list`(slash),URL path 也是 `/api/session/list`。

**修复**:`attach probe` 用 `http.NewRequest("POST", baseURL+"/api/session/list", body)`,envelope 是 `{"type":"client-request", "method":"session/list", "payload":{"args":{"_request":{}}}}`。

这条经验也适用于所有 typed methods:`session.create`、`session.cancel`、`workspace.list`、`workspace.create` —— **全是 POST + envelope**。只有少数 fire-and-forget 的(`commands/execute`)可能走其他 shape。

### 7.5 dsh 3080 fallback 3081 才是设计错误

**踩坑**(长期):用户多个 worktree 各跑一个 nightme daemon + dsh。`fix-dsh-spawn` worktree 的 daemon 启动后 dsh 占 3080;`fix-dsh-ask-question` 的 daemon 启动,看到 3080 被占,fallback 3081 spawn 自己的 dsh。几次 `/new` 后,3080-3083 全被占用,用户切 worktree 找不到能 resume 的 dsh。

**修复**:本节 §7.2 描述的 tryAttachExistingDSH → 一个 fix 解决了"daemon 重启丢 session"和"orphan dsh sprawl"两个问题。

**遗留**:`session.list` 路径 404 那条 — 没在生产代码上修,只修了测试代码。生产代码用 `RPCClient.SessionList`(Post),应该没问题,但 `tryAttachExistingDSH` 用的 raw `client.Do` 路径要小心(目前已用 POST,以后改 typed method 时记得同步)。

### 7.6 daemon.log 里的 "host handler installed" 是好信号

attach 路径第一次触发时,`host_waterfall.go` 会 log:

```
INFO dsh: calling SetHostHandler
INFO dsh: installed host waterfall handler on client
```

如果没看到这两行,说明 `installHostHandler` 没跑(可能 `cli == nil` 或被另一个 test binary 抢先 install 了 — 重复 install 是 idem-potent 的,只是 log 会写两次)。

### 7.7 多 worktree 共享 ~/.dsh/.credentials.yaml

**潜在问题**:用户有几个 nightme worktree 都 import `internal/bridge/dsh`,它们的 `~/.dsh/.credentials.yaml` 是同一个文件。如果一个 daemon 用 secret A 创了一个 session,另一个 daemon 看到 secret A 也对,但 cookie 在 dsh 看来是用 secret A 签的 — **能 validate 吗?** 能,因为 dsh 自己也是用 secret A 签的。

**风险**:如果用户 `rm ~/.dsh/.credentials.yaml`(误操作),所有旧 dsh 实例会变得不响应 cookie 验证 → 新 daemon 会一直 fallback spawn。建议**永远不删除这个文件**,且 `~/.dsh/` 目录 mode 0700。

---

## 8. 测试金字塔

### 7.1 mock(无需真 dsh)

| 文件 | 锁 |
|------|------|
| `host/host_test.go` | StreamHub + Router + Subscribe + connectAndServe mock WS,5 个 TestClient_* |
| `session_test.go` | dispatcher + translator 状态机 |
| `dispatch_test.go` | 11 种 event.type handler fixture |
| `permissions_test.go` | approval 流 + 5min timeout |
| `handle_mux_test.go` | 新 wire method 路由 |

### 7.2 真 dsh e2e(门控 tag)

```
NIGHTME_TEST_DSH_URL='http://127.0.0.1:3082/?token=...' \
  go test -tags wire_e2e -run TestWireE2E -v ./internal/bridge/dsh/host/
```

| 测试 | 锁 |
|------|------|
| `TestWireE2E/workspace/create` | workspace.create wire |
| `TestWireE2E/session/create` | session.create wire |
| `TestWireE2E/session/prompt` | session.prompt + requestId |
| `TestWireE2E/session/list` | session.list + _request wrapper |
| `TestWireE2E/commands/execute` | flat-arg commands |
| `TestWireE2E/session/cancel` | session.cancel |
| `TestWireE2E/workspace/archiveSession` | archiveSession |
| `TestWireE2E/workspace/delete` | delete |

| 测试 | 锁 |
|------|------|
| `TestWireWS_E2E`(`wire_ws_e2e_test.go`) | 整条 WS 链路: auth mint → /api/remote.mux connect → Subscribe(generation 跟踪) → session.prompt → 收 `assistant/chunk` 流 + `assistant/message` + `turn/end`。验证 translateSessionEvent / translateHostEvent 的 wire 形状在真 dsh 上正确分发。 |

### 7.3 手工验证(本机真 dsh + 浏览器 dashboard)

```
1. bin/nightme _daemon  → spawn dsh on 3080
2. dsh --profile web --no-open --port 3081  → 另起一份做对照
3. 浏览器开 http://127.0.0.1:3080 → 跟 dashboard 一样的 session 跑一遍
4. grep "dsh: mux ready\|mux read\|mux dispatch\|EventAgentText\|turn/end" bin/nightme logs
5. 期望: 两边 UI 与 nightme event 流完全对齐
```

---

## 9. 与其他 bridges 对照

| 维度 | claude | codex | pi | opencode | **dsh** |
|------|--------|-------|----|----|---------|
| Transport | stream-json stdio | JSON-RPC stdio | JSONL RPC stdio | HTTP + SSE | **HTTP + WS(shared host)** |
| Spawn | `claude -p`(RunOnce)/ stream-json(Start) | `codex exec`(RunOnce)/ app-server(Start) | `pi --mode json -p`(RunOnce)/ RPC(Start) | `opencode run`(RunOnce)/ `opencode acp`(Start) | **`dsh --profile web`(统一)** |
| 长生命周期 | ✅ | ✅ | ✅ | ✅ | **✅(同一进程,RunOnce 用临时 sessionId)** |
| 接收事件 | stdout stream-json | JSON-RPC notifications | JSONL RPC events | SSE | **WebSocket mux demux** |
| Approval | JSON-RPC request | JSON-RPC server request | (MVP auto cancel) | HTTP RPC | **HTTP POST `/api/respond`** |
| 多模态 | ✅ stream-json content array | ✅ `-i` flag | ✅ `prompt.images` | ✅ attachments | ✅ text + image inline |
| 跨进程 resume | ✅ `--resume` | ✅ `thread/resume` | ✅ `--session-id` | ✅ sessionId | ✅ `session.fork` + `session.list` |
| 二进制自包含 | ❌ npm | ❌ npm | ❌ npm | ❌ npm | ✅ npm |
| RunOnce 隔离 | `claude -p` 新进程 | `codex exec` 新进程 | `pi -p` 新进程 | `opencode run` 新进程 | **`session.create` 新 sessionId,共用 dsh web 进程** |
| RunOnce 收尾隐藏 session | N/A(进程退即清) | N/A | N/A | N/A | **`defer a.Close()` → `workspace.archiveSession`** |

---

## 10. 不在范围(deferred)

| 项 | 理由 |
|----|------|
| `dsh --profile headless` JSON-RPC bridge | 见 dsh-api 早期讨论,headless 不发结构化事件,无 `--resume`,跨 session 上下文泄漏风险。统一走 web |
| `dsh-jsonrpc-agent-pkg` Python wheel | PyPI 是 1.5KB placeholder,真 binary 在仓内 `python/sdk-runtime/`,`pnpm run build-exe` 才能出。nightme 拒绝自建 dsh 二进制(违反 "不修改 agent 本地默认配置" 原则) |
| dsh 早期 wire(`session/event` envelope 套 envelope) | 0.1.2-rc.1 已废,翻译层已迁到新 wire 形状 |
| dsh 早期 mux 协议(`/api/events.mux` + `/api/events.host` 双 WS) | 同上,合并到 `/api/remote.mux` |
| 批量 session 端(`session.list` 之外的分页 API) | 当前 `session.list` 已满足 resume picker 需求;分页用 `cursor` 字段,无 wire 改动 |
| dsh image 端 | dsh 0.1.2-rc.1 assistant baseline-only 不产图;要等 dsh 加 image capability |
| 跨平台 `dsh` install | 用户自己 `npm install -g @deepseek-ai/dsh`,不写进 nightme 安装流程 |
| request/header 等新 event.type dispatcher handler | `recordAndCountUnknown` 计数 + warn log,等 dsh 稳定后单独 PR 补 |

---

## 11. 排错速查

| 症状 | 根因 | 修法 |
|------|------|------|
| `dsh: not found in PATH` | `@deepseek-ai/dsh` npm 未装 | `npm install -g @deepseek-ai/dsh` |
| `no free port in range 3080-3099` | 3080-3099 全被占 | `lsof -nP -iTCP:3080-3099 -sTCP:LISTEN` 看谁,清掉或换 `findFreePort` 范围 |
| `dsh.host: dsh not listening on port N: timeout ...` | spawn 慢 / bind 失败 / 端口被抢 | 看 stderr(已附在 error + Warn 字段);`lsof -nP -iTCP:127.0.0.1:N -sTCP:LISTEN` |
| `dsh.host: POST session/prompt: HTTP 404` | URL path 错(用了 `/api/session.prompt` 而不是 `/api/session/prompt`) | 确认 `methodDotsToSlashes` 调用链未断 |
| `result.ok=false, error.code="bad-request"` | envelope 缺 `type` 或 `method` 字段;`requestId` 缺失;`mode` 非 queue/steer | 看 stderr 上的 dsh 错误详情 |
| `result.ok=false, error.code="gateway/arguments-invalid"` | typert 严格校验失败:字段缺失或多余 | 对照 dsh-api-gateway/lib/types/* 的 `*Schema` 与 `expected = new Set(parameters.map(p => p.wire))` |
| `result.ok=false, missing "_request"; unexpected "request"` | `session.list` wrapper key 写错 | 用 `_request` 而不是 `request`(`RPCClient.SessionList` 已处理) |
| `WS upgrade 400 Invalid Sec-WebSocket-Protocol` | bridge 设了自定义 `Sec-WebSocket-Protocol` | 确认 `host/stream.go::connectAndServe` 不设这个头 |
| `WS 1008 close, invalid Remote stream request` | open 帧多/少字段(typert exactKeys) | 对照 §3.3 open 帧 shape |
| `dsh.host: item for unknown stream` | streamId 不在 `byStreamID` 里 | 通常是 reconnect 后 streamId 漂移;检查 `currentGeneration` 逻辑 |
| `dsh.host: session/archiveSession not found` | sessionId 已被归档或从未创建 | benign,通常来自 race |
| session 跑通但 `assistant/chunk` 不流 | 旧代码: dispatchWG race;新代码: 已修 | grep `dsh.host: dispatch handler panic\|mux unknown method` 找断点 |
| `dsh.host: WS cookie 401 Unauthorized` | jar 没注入 WS upgrade header | 确认 `connectAndServe` 用 `websocket.NewClient` + 手工塞 Cookie 头 |
| `panic: sync: negative WaitGroup counter` | 旧 dispatchWG race | 已修;若再现查 `markDispatchStart` / `markDispatchDone` 配对 |
| `Close()` 卡 30s | 服务端不响应 / cancel | SIGKILL 兜底(`driver.closeOnce` 模式) |
| 测试 hang | 走了代理 | `unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy FTP_PROXY ftp_proxy` |
| dsh 默认端口漂移 | dsh 升级 | 改 `findFreePort(3080, 3099)` 起点或重新 `pnpm install -g @deepseek-ai/dsh@<pinned>` |

---

## 12. 参考

- 本机 dsh 仓根:`/Users/geax/.nvm/versions/node/v22.20.0/lib/node_modules/@deepseek-ai/dsh/`
- dsh-api-gateway 源码:同仓下 `node_modules/@deepseek-ai/dsh-api-gateway/lib/types/index.js`(stream-protocol)、`lib/types/stream-protocol.d.ts`
- dsh-api-session-controller typert:同仓下 `node_modules/@deepseek-ai/dsh-api-session-controller/lib/typert.host.js`
- dsh-client-connection(浏览器 dashboard 客户端):同仓下 `node_modules/@deepseek-ai/dsh-client-connection/lib/client.js::createWebConnectionRpc`
- dsh-api-remotes 客户端:同仓下 `node_modules/@deepseek-ai/dsh-api-remotes/lib/client.js`
- 手工 e2e 探针:`internal/bridge/dsh/host/wire_ws_e2e_test.go`(`//go:build wire_e2e`,同 `NIGHTME_TEST_DSH_URL` 门控)— `go test -tags wire_e2e -run TestWireWS_E2E -v` 跑真 dsh 全流程,验 Subscribe-after-Connect 路径 + 翻译层在真 dsh 上行为
