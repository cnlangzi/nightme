# dsh — DeepSeek Harness Bridge

> **Status**: print-mode 已落地;chat session(web HTTP+WS)已设计,待实施
> **Scope**: `internal/bridge/dsh/` — nightme 侧的 DeepSeek Harness (`@deepseek-ai/dsh`) bridge
> **形态**: 一桥两端
>   - **Print-mode**(`Starter.RunOnce`):`dsh --profile headless -- "<prompt>"` CLI 调用,plain stdout
>   - **Chat session**(`Starter.Start`,待实施):`dsh --profile web --port 0` 长驻 + HTTP RPC + 双 WebSocket 下行
> **核心原则(用户 2026-08-14 锁定)**: 接入底层 AI agent,**不修改 agent 本地默认配置**;nightme 只管 transport + permissions(权限默认全开)。详见 [agent-no-config-tampering memory](../../.claude/projects/-Users-geax-code-geax-github-com-cnlangzi-nightme/memory/agent-no-config-tampering.md)
> **姊妹文档**:
> - [docs/bridge/claude.md](./claude.md) — stream-json transport,长生命周期
> - [docs/bridge/codex.md](./codex.md) — JSON-RPC over stdio + print-mode(本 bridge 设计模板)
> - [docs/bridge/pi.md](./pi.md) — JSONL RPC over stdio + print-mode
> - [docs/bridge/opencode.md](./opencode.md) — HTTP+SSE transport(本 chat session bridge 形态同构)
> - [docs/feat/F-dsh-bridge.md](../feat/F-dsh-bridge.md) — print-mode 阶段的设计稿(已 closed)
> - [docs/bridge/cli-transport.md](./cli-transport.md) — pipe / lifecycle 通用约束

---

## 1. 调研结论(2026-08-14 实机 + 源码双验)

### 1.1 dsh 是什么

[DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness) 是 DeepSeek AI 开源的 agent harness,核心口号 "everything is a plugin",由 Cordis 框架驱动。CLI 名 `dsh`,MIT 协议,主语言 TypeScript(23 MB TS + 168 KB Python)。

```
npx @deepseek-ai/dsh web                  # 默认 Web UI,127.0.0.1:3080
pnpm dsh --profile headless "<task>"      # 单 turn print-mode,plain stdout
```

### 1.2 实机三种可接入口

| 入口 | 实机状态 | 多 turn? | 备注 |
|------|---------|---------|------|
| `dsh --profile headless` (npm `@deepseek-ai/dsh`) | ✅ 实装 + 实测 `PONG` 通 | ❌ `headless --help` 无 `--resume`/`--session-id`,只暴露 `-h` | print-mode 唯一入口,每调用 = 新 session |
| `dsh --profile web` (HTTP :3080) | ✅ 实机启 + 验证 HTTP + WS + **image/text 混合投递** | ✅ server-side session 持久化 | **本 chat session bridge 走的路径**,HTTP + WS 双通道 |
| `dsh-jsonrpc-agent-pkg` (pip 单文件可执行) | ❌ PyPI wheel 是 placeholder(1.5 KB),**真 binary 未发布** | ✅ | **DEPRECATED 路径** — 详情见 §1.4 |

### 1.3 wire 协议(从 `packages/host/apiproxy/src/` 源码 + 实机双重验证)

#### 1.3.1 spawn 配方
```
dsh --profile web --port 0
```
- `--port 0`:OS 自动选空闲端口(避免冲突)
- stdout 第一行 `dsh web: http://127.0.0.1:3080`,正则提取 `<host>:<port>`

#### 1.3.2 HTTP RPC
```
POST http://127.0.0.1:3080/api/{method}
Content-Type: application/json

Request:
{
  "type": "client-request",
  "rpcId": "<uuid>",
  "method": "<api-method>",
  "payload": { ... }
}

Response:
{
  "type": "server-response",
  "rpcId": "<同>",
  "result": {
    "ok": true,
    "value": { ... }
  }
}
// 或:
{
  "type": "server-response",
  "rpcId": "<同>",
  "result": {
    "ok": false,
    "error": { "code": "bad-request", "message": "...", "details": {...} }
  }
}
```

**Method ↔ URL 映射**(完整列表见 `packages/host/apiproxy/src/api/rpc-map.ts`,关键方法):

| Method | Payload | 用途 |
|--------|---------|------|
| `session.list` | `{}` | 列所有 session |
| `session.create` | `{cwd, title}` | 新建 session,返 sessionId |
| `session.prompt` | `{sessionId, mode: "queue"\|"steer", content: [ContentBlock]}` | 提交 turn(`mode` **必填**;`content` 接受 `text` / `image` inline,见 §1.4 实机反查)|
| `session.cancel` | `{sessionId}` | 取消 in-flight turn |
| `session.fork` | `{sessionId}` | 从现有 session 开新(daemon 重启续接用)|
| `session.history` | `{sessionId, sinceSeq}` | 拉历史 event log |
| `session.models` | `{sessionId}` | 查可用 model |
| `host.describe` | `{}` | host metadata |
| `respond` | `{rpcId, payload}` | **服务端推送帧的回环**(approval / question answers) |

#### 1.3.3 WebSocket 下行(2 条独立流)

```
ws://127.0.0.1:3080/api/events.mux   # session/event 流(主事件)
ws://127.0.0.1:3080/api/events.host  # host lifecycle(创建/销毁/失败)
```

每条流由 server 单向推送,**客户端不可写**(下行-only)。Server frames 格式:

```json
{
  "type": "server-request",
  "rpcId": "<uuid>",
  "method": "<event-type>",
  "payload": { ... }
}
```

**MuxFrame 类型**(`packages/host/apiproxy/src/api/events.ts`,bridge 关心的子集):

| `method` | payload 关键字段 | 翻译目标 `agent.AgentEvent` |
|----------|------------------|------------------------------|
| `session/subscribed` | `{sessionId, lastSeq}` | 初始化基线;不发 event,仅记录 seq 起点 |
| `session/event` | `{sessionId, event: SessionEvent, view?: ToolEventView}` | translate 42 种 SessionEvent → AgentEvent |
| `session/projection` | `{sessionId, seq, projection, value}` | 投影帧(标题/任务列表),不直接发 event;runtime 更新内部状态 |
| `session/queue` | `{sessionId, items}` | 输入队列快照;F-38 后续若做 QueueDock UI 时用 |
| `session/jobs` | `{sessionId, jobs}` | 后台任务;本期不发 event |
| `approval/requested` | `{sessionId, approvalId, toolName, callId?, reason?}` | **`EventAgentPermission{ResponseCh}`** + 记 `approvalId` |
| `approval/resolved` | `{sessionId, approvalId, outcome}` | audit trail,debug log |
| `question/requested` | `{sessionId, questions: AskUserQuestionItem[]}` | **`EventAgentPermission`**(复用)|
| `question/resolved` | `{sessionId, questionRpcId, outcome}` | audit trail |
| 未知 / 其它 | — | debug log,不杀 session(宽松策略) |

**HostFrame 类型**(本期不主动消费,记 baseline):

```json
{
  "type": "session/created" | "session/destroyed" | "agent/status" | "agent/failure",
  "sessionId": "...",
  ...
}
```

#### 1.3.4 `respond` 回环(approval / question)

服务端发 `approval/requested` 时带 `approvalId`(或 `question/requested` 的 `rpcId`)。客户端回应走 HTTP POST `/api/respond`:

```json
POST /api/respond
{
  "type": "client-request",
  "rpcId": "<uuid>",
  "method": "respond",
  "payload": {
    "rpcId": "<server-frame 的 rpcId>",
    "outcome": { "kind": "approved" }  // 或 declined / answered-with-labels / ...
  }
}
```

**关键**:`approvalId` 在服务端是稳定的,`rpcId` 也是;bridge 维护 `pendingApprovals map[approvalId]chan ApprovalDecision`(`approvalId` 而非 `rpcId`,因服务端可能改 rpcId 但 approvalId 稳定)。

### 1.4 DEPRECATED 路径:dsh-jsonrpc-agent-pkg

| 项 | 值 |
|----|----|
| **wire** | newline-delimited JSON-RPC 2.0 over stdio |
| **3 方法** | `initialize` / `session/prompt` / `shutdown` |
| **4 通知** | `session.event` / `session.status` / `subagent.started` / `subagent.finished` |
| **session 持久化** | server-side JSONL(`~/.dsh/sessions/`)+ session-persistence-jsonl plugin |
| **多 turn** | `session/prompt` 复用 sessionId,server-side 持久 |
| **bundle size** | ~174 MB(pkg --sea 单文件可执行,Node 24 + 全部 bare plugin bundled)|

**DEPRECATED 原因**(2026-08-14 实机):
- PyPI `deepseek-harness-runtime-bin` wheel **是 1.5 KB placeholder**(`__init__.py` 自述「Placeholder package reserving the name」)
- 真 binary 只在 dsh 仓 `python/sdk-runtime/src/deepseek_harness_runtime/runtime/` 源码,需本地 `pnpm run build-exe` 构建
- 真 binary 未公开发布到 PyPI / GitHub Releases,只在 CI workflow artifact 留
- 用户实机无 deepseek-harness 仓 clone,无 build 产物

**结论**:走 `dsh --profile web` HTTP+WS 路径,该路径**用户机器上已装好**(@deepseek-ai/dsh npm),无需 pip / 无需 clone / 无需 build。

### 1.5 与现有 bridges 对照

| 维度 | claude | codex | pi | opencode | **dsh (print)** | **dsh (chat,待实施)** |
|------|--------|-------|-----|----------|-----------------|-----------------------|
| Transport | stream-json stdio | JSON-RPC stdio | JSONL RPC stdio | HTTP + SSE | **stdio (CLI)** | **HTTP + WS** |
| Spawn | `claude --print` / `--input-format stream-json` | `codex app-server --listen stdio://` | `pi --mode rpc` | `opencode serve --port 0` | `dsh --profile headless` | `dsh --profile web --port 0` |
| 长生命周期 | ✅ | ✅ | ✅ | ✅ | ❌ one-shot | ✅(设计) |
| 接收事件 | stdout stream-json | JSON-RPC notifications | JSONL RPC events | SSE | N/A | WebSocket |
| Approval | JSON-RPC request | JSON-RPC server request | (MVP auto cancel) | HTTP RPC | N/A | HTTP POST `/api/respond` |
| 多模态 | ✅(stream-json content array) | ✅(-i flag) | ✅(prompt.images) | ✅(attachments) | ⚠️ (text+file 注解) | ⚠️(baseline-only;text + resource_link)|
| 跨进程 resume | ✅ --resume | ✅ thread/resume | ✅ --session-id | ✅ sessionId | ❌(headless 不支持) | ✅ sessionId(server-persistence-jsonl)|
| 二进制自包含 | ❌ npm | ❌ npm | ❌ npm | ❌ npm | **✅ npm** | **✅ npm** |
| Print-mode(RunOnce)| ✅ `claude -p` | ✅ `codex exec` | ✅ `pi --print` | n/a | ✅ **`dsh --profile headless`** | n/a |

---

## 1.4 实机反查(2026-08-14 image / text 混合投递)

实机测试 `POST /api/session.prompt` 5 种 content block shape:

| Shape | dsh 响应 | 备注 |
|-------|----------|------|
| `[{type:"text", text:"..."}]` | ✅ OK | baseline text path |
| `[{type:"image", mediaType:"image/png", data:"<base64>"}]` | ✅ OK | **inline image,**vision 直通 |
| `[{type:"text",...},{type:"image",...}]` | ✅ OK | **混合 text+image,**同一个 payload |
| `[{type:"resource_link", name, uri}]` | ❌ `bad-request: No matching discriminator. Expected 'text'\|'image'` | dsh web prompt 不接受 resource_link(虽然 `PromptContentPart` TS union 声明了) |
| 缺 `mode` 字段 | ❌ `bad-request: invalid input: expected "queue"` | mode **必填**(`queue` \| `steer`) |

**关键修正**(对比 §1.3.2 早期假设):
- `session.prompt` payload **必须含 `mode` 字段** — 没填会被 schema validator 拒
- `resource_link` 不被 prompt 边界接受(尽管 TS 类型支持)— 实测 discriminator 拒绝
- ✅ `type:"image"` + base64 + `mediaType` **真支持**,与 web UI 一致(用户视角下,UI 上传图片走的也是这条 wire)

**bridge `contentBlocksToDTO` 改造**(2026-08-14 落地):
- `ContentText` → wire `text` verbatim
- `ContentImage` with `image/png` / `image/jpeg` / `image/gif` / `image/webp` → wire `image` + base64(`name` = basename)
- `ContentImage` 其他 MIME(`image/heic` / `image/svg`...)→ 退化为文本注解 `"\[image: <path> (<mime>) — unsupported mediaType, decode locally to view]"`
- `ContentFile` → 文本注解 `"\[file: <path>]"`(dsh web prompt 不接受 file 引用类型;模型可走 bash/fs 工具读)

**已知不支持**(对应 §11 deferred):
- dsh assistant 响应是 baseline-only,**不会回 image**(只回 text)。要拿 model 生成的图需 dsh 加 image capability。
- 多模态文件类型(image/heic, image/avif 等 dsh 没声明的)只能走 fallback 注解路径。

---

## 2. 设计基线

### 2.1 已锁定

1. **不 bundled 任何 dsh 配置文件** — 走 `~/.dsh/settings.yaml` + `~/.dsh/.credentials.yaml` 路径
2. **不注入 model / provider / credentials / API key** — 仅 `cmd.Dir`(workspace) + `DSH_PERMISSION_MODE`(permission)
3. **chat session bridge 走 `dsh --profile web` HTTP+WS** — 不依赖 pip / 不需要 clone dsh 仓
4. **print-mode 走 `dsh --profile headless` CLI** — 零新依赖,实机验证 PONG/ALPHA/BETA/YES/OK
5. **Start 报错清晰化**:`Starter.Start` 真实 spawn long-lived web process(非返 "not implemented");print-mode 仍是 `Starter.RunOnce` 的 headless 路径
6. **abort lifecycle 复用 codex 模式**:closeOnce.Do 守护 SIGINT/SIGTERM 兜底,channel 只在 lifecycle goroutine 关闭

### 2.2 已排除

1. **`dsh-jsonrpc-agent-pkg` JSON-RPC** — 见 §1.4,DEPRECATED
2. **bundled cordis.yml** — 不修改 dsh 本机配置(原则违反)
3. **TUI / acp / 自定义 profile** — `pnpm run demo:acp` 需 clone dsh 仓,`tui` 需 TTY,profile 需 `dsh plugin add` 安装;**不在实机**
4. **PTY fallback for headless** — `dsh --profile headless` 是一次性进程,PTY 接不到 stdin 续 turn,**结构上不可能多 turn**

---

## 3. 包结构(chat session 阶段,~1800 行 Go)

```
internal/bridge/dsh/
  ├── doc.go                 # package doc
  ├── starter.go             # Starter + Info(ModeJSONIO) + Detect + Start + RunOnce
  ├── print.go               # ✅ 已落地:RunOnce → dsh --profile headless
  ├── prompt.go              # ✅ 已落地:blocksToPrompt
  ├── starter_test.go        # ✅ 已落地:单测
  ├── print_real_unix_test.go # ✅ 已落地:e2e
  ├──
  ├── detect.go              # 新增:exec.LookPath("dsh") + `dsh web` smoke probe
  ├── session.go             # 新增:spawn `dsh web` + parse URL + lifecycle + closeOnce
  ├── http.go                # 新增:HTTP RPC client(POST /api/{method})
  ├── ws.go                  # 新增:WebSocket client(mux + host,下行 only)
  ├── translate.go           # 新增:MuxFrame/HostFrame → agent.AgentEvent(F-52 状态机)
  ├── permissions.go         # 新增:approval/requested → EventPermission + pending map
  ├── respond.go             # 新增:approval/question 答案走 /api/respond 回环
  ├──
  ├── session_test.go        # 新增:session lifecycle 单测
  ├── http_test.go           # 新增:HTTP envelope marshal/unmarshal 单测
  ├── translate_test.go      # 新增:MuxFrame 翻译表全覆盖(42 事件中 bridge 关心的 11 个)
  ├── permissions_test.go    # 新增:approval 流 + 5min timeout 压 200ms
  ├── session_real_unix_test.go # 新增:NIGHTME_REAL_DSH=1 e2e(完整 create → prompt → drain → close)
```

### 3.1 Mode 标记

```go
return agent.NewInfo(s.name, agent.ModeJSONIO, s.command, s.args, nil)
```

`ModeJSONIO` 是最接近的现有 mode(claudecode/codex/pi 都在用)。dsh 的事件流是结构化 JSON,不是 stream-json/JSON-RPC 协议,但 Mode 字段仅 metadata,不 gating 行为,放 JSONIO 是 OK 的折衷。

### 3.2 transport 选择 rationale

候选传输:

| 路径 | 选不选 | 理由 |
|------|--------|------|
| `dsh --profile headless` CLI | ✅ print-mode | 实机 0 新依赖,实机 PONG 通 |
| `dsh --profile web` HTTP + WS | ✅ chat session | npm 已装,实机多 turn 验证通,wire 文档化 |
| `dsh-jsonrpc-agent-pkg` | ❌ | 见 §1.4 — PyPI placeholder,真 binary 未公开发布 |
| `pnpm run demo:acp` | ❌ | 需 clone dsh 仓 + pnpm install,用户实机无 |
| `dsh --profile tui` | ❌ | 需 TTY,非 spawn 友好 |

---

## 4. 实施配方

### 4.1 spawn (`session.go`)

```go
// internal/bridge/dsh/session.go
func newDriver(ctx context.Context, s *Starter, cfg agent.StartConfig) (*driver, error) {
    if cfg.Workspace == "" {
        return nil, fmt.Errorf("dsh: workspace is required")
    }
    
    cmd := agent.NewCmd(ctx, "dsh", "--profile", "web", "--port", "0")
    cmd.Dir = cfg.Workspace
    cmd.Env = append(os.Environ(), "DSH_PERMISSION_MODE=danger-full-access")
    
    stdout, err := cmd.StdoutPipe()
    if err != nil { return nil, fmt.Errorf("dsh: stdout pipe: %w", err) }
    stderr, err := cmd.StderrPipe()
    if err != nil { _ = stdout.Close(); return nil, fmt.Errorf("dsh: stderr pipe: %w", err) }
    
    if err := cmd.Start(); err != nil { return nil, fmt.Errorf("dsh: start: %w", err) }
    
    // Parse "dsh web: http://127.0.0.1:PORT\n" from stdout
    url, err := parseWebURL(ctx, stdout, 5*time.Second)
    if err != nil {
        _ = cmd.Process.Kill()
        _ = cmd.Wait()
        return nil, fmt.Errorf("dsh: parse url: %w", err)
    }
    
    // Dial 2 WebSocket connections
    muxWS, err := dialWS(ctx, url+"/api/events.mux")
    if err != nil { return nil, fmt.Errorf("dsh: mux ws: %w", err) }
    hostWS, err := dialWS(ctx, url+"/api/events.host")
    if err != nil { muxWS.Close(); return nil, fmt.Errorf("dsh: host ws: %w", err) }
    
    d := &driver{
        cmd: cmd, stdout: stdout, stderr: stderr,
        muxWS: muxWS, hostWS: hostWS,
        http: newHTTPClient(url),
        events: make(chan agent.AgentEvent, eventBufferSize),
        pendingApprovals: map[string]chan ApprovalDecision{},
        pendingQuestions: map[string]chan QuestionDecision{},
        workspace: cfg.Workspace,
        agentName: s.name,
        closed: make(chan struct{}),
        exitDone: make(chan struct{}),
    }
    
    // session.create
    createResp, err := d.http.Post(ctx, "session.create", map[string]any{
        "cwd": cfg.Workspace,
        "title": filepath.Base(cfg.Workspace),
    })
    if err != nil {
        _ = d.Close()
        return nil, fmt.Errorf("dsh: session.create: %w", err)
    }
    if !createResp.OK() {
        _ = d.Close()
        return nil, fmt.Errorf("dsh: session.create rejected: %s", createResp.Error())
    }
    d.sessionID = createResp.Value["sessionId"].(string)
    
    // Start pumps in parallel with handshake
    d.pumpWG.Add(3)
    go func() { defer d.pumpWG.Done(); d.readPump(muxWS, "mux") }()
    go func() { defer d.pumpWG.Done(); d.readPump(hostWS, "host") }()
    go func() { defer d.pumpWG.Done(); d.drainStderr(stderr) }()
    go d.lifecycle()
    
    // Emit EventAgentReady
    d.deliver(agent.AgentEvent{
        Kind: agent.EventAgentReady,
        SessionID: d.sessionID,
        AgentName: "dsh",
        Workspace: cfg.Workspace,
        Branch: detectBranch(cfg.Workspace),
    })
    
    return d, nil
}
```

### 4.2 HTTP client (`http.go`)

```go
// internal/bridge/dsh/http.go
type httpClient struct {
    baseURL string
    http    *http.Client  // with 30s timeout
}

type rpcResponse struct {
    Type   string `json:"type"`
    RpcID  string `json:"rpcId"`
    Result struct {
        OK     bool            `json:"ok"`
        Value  json.RawMessage `json:"value,omitempty"`
        Error  *rpcError       `json:"error,omitempty"`
    } `json:"result"`
}

type rpcError struct {
    Code    string          `json:"code"`
    Message string          `json:"message"`
    Details json.RawMessage `json:"details,omitempty"`
}

func (h *httpClient) Post(ctx context.Context, method string, payload any) (*rpcResponse, error) {
    rpcID := newRPCID()
    body := map[string]any{
        "type": "client-request",
        "rpcId": rpcID,
        "method": method,
        "payload": payload,
    }
    bodyBytes, _ := json.Marshal(body)
    
    req, _ := http.NewRequestWithContext(ctx, "POST", h.baseURL+"/api/"+method, bytes.NewReader(bodyBytes))
    req.Header.Set("Content-Type", "application/json")
    
    resp, err := h.http.Do(req)
    if err != nil { return nil, err }
    defer resp.Body.Close()
    
    if resp.StatusCode != 200 {
        return nil, fmt.Errorf("dsh: http %d on %s", resp.StatusCode, method)
    }
    
    var out rpcResponse
    if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
        return nil, fmt.Errorf("dsh: decode: %w", err)
    }
    if out.RpcID != rpcID {
        return nil, fmt.Errorf("dsh: rpcId mismatch (sent %s, got %s)", rpcID, out.RpcID)
    }
    return &out, nil
}
```

### 4.3 WebSocket client (`ws.go`)

```go
// internal/bridge/dsh/ws.go
func dialWS(ctx context.Context, url string) (*websocket.Conn, error) {
    d := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
    conn, _, err := d.DialContext(ctx, url, nil)
    if err != nil { return nil, err }
    conn.SetReadLimit(10 * 1024 * 1024)  // 10 MiB frame cap
    return conn, nil
}

func (d *driver) readPump(conn *websocket.Conn, kind string) {
    defer conn.Close()
    for {
        _, raw, err := conn.ReadMessage()
        if err != nil {
            dLog("dsh: %s ws read err: %v", kind, err)
            return
        }
        var frame struct {
            Type    string          `json:"type"`
            RpcID   string          `json:"rpcId"`
            Method  string          `json:"method"`
            Payload json.RawMessage `json:"payload"`
        }
        if err := json.Unmarshal(raw, &frame); err != nil {
            dLog("dsh: %s ws frame decode: %v", kind, err)
            continue
        }
        d.handleFrame(kind, frame.RpcID, frame.Method, frame.Payload)
    }
}
```

### 4.4 translate 层 (`translate.go`)

```go
// internal/bridge/dsh/translate.go
type translator struct {
    workspace string
    agentName string
    
    // F-52 状态机:textBuf 按 contentIndex 分桶,thinking 累积,
    // tool 边界 flush pendingText。结构 mirror pi/translate.go。
    mu          sync.Mutex
    textBuf     map[int]*strings.Builder
    thinkBuf    strings.Builder
    pendingText string
    lastText    string
    lastUsage   *agent.UsageInfo
    
    // pendingTools 把 tool/call 的 Name + Args 存下来,
    // tool/result 时回填给 EventToolEnd
    pendingTools map[string]pendingTool
}

func (t *translator) handleSessionEvent(ev SessionEvent, deliver func(agent.AgentEvent)) {
    switch ev.Type {
    case "assistant/chunk":
        // 累加,no-op(对齐 F-52)
    case "assistant/message":
        // flush pendingText → EventText(F-52 粒度)
    case "tool/call":
        // EventToolStart,record Name+Args
    case "tool/result":
        // EventToolEnd,回填 Name+Args
    case "turn/start":
        // 清 turnState(对齐 pi F-32)
    case "turn/end":
        // EventResult(Usage) + EventDone{Reason:"settled"}
    case "compaction/end":
        // EventCompaction
    case "todo/write":
        // EventAgentTaskCreate/Update(snapshot)
    case "approval/asked":
        // EventAgentPermission(单独走 permissions.go)
    default:
        // debug log,不杀 session
    }
}
```

完整状态机 + 守卫(textDelivered / active / reset window)**镜像 pi/translate.go F-52 章节**,因为 dsh 的 `assistant/chunk → assistant/message → turn/end` 序列与 pi 同构。

### 4.5 permissions (`permissions.go`)

```go
// internal/bridge/dsh/permissions.go
type approvalCall struct {
    approvalID string
    toolName   string
    callID     string
    reason     string
    respCh     chan ApprovalDecision  // buffer 1, non-blocking send
}

// on approval/requested mux frame:
func (d *driver) handleApproval(payload ApprovalRequested) {
    respCh := make(chan ApprovalDecision, 1)
    d.pendingMu.Lock()
    d.pendingApprovals[payload.ApprovalID] = respCh
    d.pendingMu.Unlock()
    
    // Emit to runtime
    d.deliver(agent.AgentEvent{
        Kind: agent.EventAgentPermission,
        Permission: &agent.AgentPermissionRequest{
            Tool:    payload.ToolName,
            Action:  payload.Reason,
            Options: []string{"approve", "decline"},  // TBD: dsh 可能支持更多 outcome
            ResponseCh: respCh,
        },
    })
}

// on SendPermission(resp):
func (d *driver) SendPermission(resp string) error {
    d.pendingMu.Lock()
    var approvalID string
    for id := range d.pendingApprovals {
        approvalID = id
        break
    }
    delete(d.pendingApprovals, approvalID)
    d.pendingMu.Unlock()
    
    decision := map[string]any{"kind": resp}  // 或更复杂的 outcome 结构
    _, err := d.http.Post(context.Background(), "respond", map[string]any{
        "rpcId":   approvalID,
        "payload": decision,
    })
    return err
}
```

**关键修正**(对 codex 已知踩坑的预防):
- `SendPermission` 用 **approvalID**(服务端稳定字段)路由,**不**用 rpcId(可能变)
- 只有 1 个 pending approval 时,rpcId == approvalID;多 approval 并发时按 approvalID 路由,避免 codex §6.4 那样的乱答
- 5 min timeout(`permissionTimeout` 包级 var),过期 → decline,test 压 200ms

### 4.6 lifecycle (`session.go`)

```go
// lifecycle goroutine:关 events channel(独占权)
func (d *driver) lifecycle() {
    defer close(d.events)
    defer close(d.exitDone)
    
    waitErr := d.cmd.Wait()
    d.pumpWG.Wait()  // 等 mux/host/stderr drain 全退
    
    // 子进程退出后,所有 pending approval 解 "decline"(不能让 user 卡死)
    d.pendingMu.Lock()
    for _, ch := range d.pendingApprovals {
        select {
        case ch <- ApprovalDecision{Kind: "declined"}:
        default:
        }
    }
    d.pendingApprovals = nil
    d.pendingMu.Unlock()
    
    if waitErr != nil {
        d.deliver(agent.AgentEvent{Kind: agent.EventAgentError, Err: waitErr})
    }
}

// Close 由 Agent.Close 调,幂等
func (d *driver) Close() error {
    var err error
    d.closeOnce.Do(func() {
        close(d.closed)
        // 走协议 shutdown:发 session.cancel + 等待
        if d.sessionID != "" {
            _, _ = d.http.Post(context.Background(), "session.cancel", map[string]any{
                "sessionId": d.sessionID,
            })
        }
        // 关 WS
        _ = d.muxWS.Close()
        _ = d.hostWS.Close()
        // 关 stdin → 让 dsh 自然 exit
        _ = d.cmd.Process.Signal(os.Interrupt)
        // 等 lifecycle,5s 兜底 SIGKILL
        select {
        case <-d.exitDone:
        case <-time.After(5 * time.Second):
            _ = d.cmd.Process.Kill()
            <-d.exitDone
        }
    })
    return err
}
```

**关键不变量**(代码 codex.md §2.6 / §3.3 / §4.3 / §6.4 / §8.2):
- `close(events)` **只在 lifecycle goroutine** — `Close()` 不直接关 events
- `pendingTurnActive` 不存在(dsh `session.status: idle|running` 服务端管,我们只看 turn/end)
- `emitWireError` 四件套:`EventAgentError` + 解 pendingApprovals + 不解 pending RPC(本 bridge 无,HTTP 是 unary) + 关 stdin
- frame 10 MiB 上限

---

## 5. lifecycle 时间线(每 turn)

```
t0   cmd.Start ──→ spawn dsh --profile web --port 0
                ├── readPump goroutine (mux)
                ├── readPump goroutine (host)
                ├── drainStderr goroutine
                └── lifecycle goroutine (close events 独占)
t1   parse "dsh web: http://127.0.0.1:PORT" from stdout
t2   dial 2 WebSocket (mux + host)
t3   HTTP POST /api/session.create → sessionId
t4   EventAgentReady{ SessionID, Model="(blank, dsh 自己解析)", AgentName="dsh", Workspace, Branch }
t5   (events chan open, readPump 在跑)
t6   user 第一次发消息
        └── SendBlocks → HTTP POST /api/session.prompt {sessionId, content}
            └── prompt ack → wait turn/end
t7   readPump 收 turn/end → EventResult{Usage} + EventDone{Reason:"settled"}
t8   (events chan 仍 open,可继续发消息)
t9   next user 消息 → t6 重复
t10  Close() → session.cancel → 关 stdin → SIGINT 兜底
t11  child 退出 → lifecycle close(events)
t12  AgentSession.SetExited(0)
```

---

## 6. translate 表(MuxFrame → AgentEvent)

| `payload.method` | payload 关键字段 | 目标 `agent.AgentEvent` | 备注 |
|------------------|------------------|--------------------------|------|
| `session/subscribed` | `{sessionId, lastSeq}` | (debug log;不发 event) | 初始化 baseline,记 seq 起点 |
| `session/event` `assistant/chunk` | `{event:{messageId, contentIndex, delta}}` | (累加 `textBuf[contentIndex]`) | 流式 delta,不 emit |
| `session/event` `assistant/message` | `{event:{messageId, content:[...]}}` | **flush `pendingText` → `EventText`**(F-52 粒度)| 一个 message = 一个完整块 |
| `session/event` `tool/call` | `{event:{toolCallId, toolName, args, messageId}}` | `EventToolStart{ID, Name, Args}` | + 存 `pendingTools[id]` |
| `session/event` `tool/result` | `{event:{toolCallId, result, isError}}` | `EventToolEnd{ID, Name, Args, Output, Err}` | 从 pendingTools 回填 Name/Args |
| `session/event` `turn/start` | `{event:{turnId, messageIds?}}` | (清 turnState;对齐 pi F-32) | |
| `session/event` `turn/end` | `{event:{turnId, stopReason, usage?}}` | **`EventResult{Usage} → EventDone{Reason:"settled"}`** | F-52 终态 |
| `session/event` `compaction/end` | `{event:{reason, aborted}}` | `EventCompaction` | 一个周期 = 一次 emit |
| `session/event` `todo/write` | `{event:{items:[{content, status}]}}` | `EventAgentTaskCreate` / `EventAgentTaskUpdate`(snapshot)| 字段名对齐 F-38 |
| `session/event` `approval/asked` | `{event:{toolCallId, toolName, action, options}}` | (单独走 permissions.go) | **不**直接发 EventPermission 给 runtime,经 permissions 层 normalize |
| `approval/requested` | `{sessionId, approvalId, toolName, callId?, reason?}` | `EventAgentPermission{ResponseCh}` | 见 §4.5 |
| `question/requested` | `{sessionId, questions}` | `EventAgentPermission` (复用,多 question)| inline encode labels,见 codex §6.3 |
| `session/queue` | `{sessionId, items}` | (debug;F-38 后续) | |
| `session/jobs` | `{sessionId, jobs}` | (debug;本期不发) | |
| `session/projection` | `{sessionId, seq, projection, value}` | (runtime 更新内部 state;不发 event) | 投影帧,title/tasks 等 |
| `approval/resolved` | `{sessionId, approvalId, outcome}` | (audit log) | user 已答,记 trace |
| `question/resolved` | `{sessionId, questionRpcId, outcome}` | (audit log) | user 已答 |
| host `session/created` / `session/destroyed` / `agent/status` | — | (debug log) | host 生命周期,本期不渲染 |
| 未知 `method` | — | debug log + 继续 | 宽松策略,不杀 session |

---

## 7. RpcError 映射

`error.code` 值列表见 `packages/host/apiproxy/src/api/rpc.ts`,bridge 关心:

| `code` | 含义 | bridge 应对 |
|--------|------|-------------|
| `bad-request` | payload schema 失败 | `EventAgentError` + 错误细节 |
| `session-not-found` | sessionId 错 | 触发 re-create + retry |
| `agent-busy` | 上一 turn 还没完 | retry after backoff(对齐 codex `ErrTurnBusy`) |
| `model-unavailable` | 当前 model 不可用 | `EventAgentError` + 让 user `/use` 别的 |
| `cancelled` | 用户已取消 | 不报错,正常路径 |
| `attachment-error` | 上传失败 | `EventAgentError` |
| `agent-preset-conflict` / `agent-preset-locked` | preset 已锁 | **不该发生**(我们用 default model,不锁)|
| `*` 其它 | 业务错误 | `EventAgentError` + code 透明给 user |

---

## 8. 实机踩坑(2026-08-14 实测记录)

### 8.1 headless 不支持 `--resume`
**症状**:`dsh --profile headless -- --resume x "..."` → `error: unknown option '--resume'`
**结论**:headless profile 写死 "Answer one task and exit",每调用 = 新 session,**只能走 print-mode**
**影响**:print-mode 没 resume;chat session 必须走 web,不走 headless

### 8.2 PyPI `deepseek-harness-runtime-bin` 是 placeholder
**症状**:`pip install --user deepseek-harness-runtime-bin` → 1.5 KB wheel,`__init__.py` 自述 "Placeholder package"
**结论**:真 binary 不在 PyPI
**影响**:JSON-RPC 路径不可用,**改走 web HTTP+WS**

### 8.3 dsh npm CLI 已装好,`~/.dsh/settings.yaml` 配 minimax-cn
**实测**:`dsh --profile headless "Reply with PONG"` → "PONG",exit 0
**配置**:`agent-default-model: { provider: minimax-cn, model: MiniMax-M3 }`,API key 在 `.credentials.yaml`
**影响**:zero-config 接入,nightme 不注入任何 model/provider/credentials

### 8.4 dsh web spawn URL pattern
**实测**:`dsh --profile web --port 0` → stdout `dsh web: http://127.0.0.1:3080`,约 1.5s 启动
**影响**:用正则 `dsh web: http://([^:]+):(\d+)` 提取 host:port

### 8.5 WS 路径是 dot 不是 slash
**关键常量**(从 `packages/client/connection/src/api-path.ts`):
```ts
export const MUX_EVENTS_PATH = `${API_PATH}/events.mux`  // /api/events.mux
export const HOST_EVENTS_PATH = `${API_PATH}/events.host` // /api/events.host
```
**踩坑**:第一次试 `/api/events/mux`(slash)失败,**实机验证 dot 才对**

### 8.6 HTTP envelope 必须含 `type:"client-request"`
**实测**:`POST /api/session.list` body `{"rpcId":"x","payload":{}}` → 200 OK 但 result.ok=false,error="expected type: client-request"
**修法**:envelope 加 `"type":"client-request"` 和 `"method":"session.list"` 两个必填字段

### 8.7 session.prompt 失败需先选 model
**实测**:`session.create` 后直接 `session.prompt` → ok=False
**根因**:session 没 model 选(虽然 dsh `agent-default-model` 配了 minimax-cn,但 SDK 仍要求 session 显式 model)
**待研究**:session.create 是否可传 model;或 session.models 自动选 default — **chat session bridge 实施时验证**

---

## 9. 测试金字塔

### 9.1 单元(不需真 dsh)

| 文件 | 测试 | 锁 |
|------|------|----|
| `protocol_test.go` | `rpcResponse` / `ApprovalRequested` / `MuxFrame` JSON roundtrip | wire schema |
| `http_test.go` | HTTP envelope marshal + rpcId 校验 + 错误码 | transport |
| `translate_test.go` | MuxFrame 翻译表全覆盖(11 个事件)+ textDelivered / active / reset 守卫 | F-52 状态机 |
| `permissions_test.go` | approval/requested → EventPermission + 5min timeout 压 200ms | auth flow |
| `starter_test.go` | ✅ 已落地:Info / Detect / Start / RunOnce | metadata |

### 9.2 Real dsh e2e(`NIGHTME_REAL_DSH=1`)

| 测试 | 锁 |
|------|----|
| `TestE2E_PongSmoke` ✅ 已落地 | print-mode headless `dsh --profile headless "PONG"` → `Text=="PONG"` |
| `TestE2E_DSHPermissionEnvPropagates` ✅ 已落地 | mock script 验 `DSH_PERMISSION_MODE=danger-full-access` 透传 |
| `TestE2E_ArgsFromStarter` ✅ 已落地 | mock script 验 argv 链路 |
| `TestE2E_SpawnWeb_ParseURL` | `dsh --profile web` 启动,正则提取 host:port |
| `TestE2E_WebHandshake_FullFlow` | spawn → dial WS → session.create → EventAgentReady 在 10s 内发 |
| `TestE2E_WebPrompt_DrainsEvents` | prompt 后,5s 内从 WS mux 收 ≥3 个 session/event 帧 + 1 个 EventResult + 1 个 EventDone |
| `TestE2E_WebApprovalFlow` | 模型触发 approval → EventPermission → SendPermission → 后续 turn 正常 |
| `TestE2E_WebCancel` | session.cancel → 已 in-flight prompt response 返 cancelled stopReason |
| `TestE2E_WebClose_Reap5s` | `Close()` 后 `<-lifecycle` 5s 内返回,无 zombie |
| `TestE2E_WebPrintMode_StillWorks` | print-mode 不被 chat session 改动破坏 |

### 9.3 全库回归

```bash
go build ./... && go test ./internal/bridge/dsh/... -race
NIGHTME_REAL_DSH=1 go test ./internal/bridge/dsh/ -run Real -v
go test ./... -race

unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy FTP_PROXY ftp_proxy RSYNC_PROXY rsync_proxy
```

---

## 10. memory 原则对照

本 bridge 严格遵循 [[agent-no-config-tampering]]:
- ❌ 不 bundled cordis.yml(`cmd.Dir` 已满足 workspace 需求)
- ❌ 不读 `~/.dsh/settings.yaml`(dsh 启动时自己读)
- ❌ 不读 `~/.dsh/.credentials.yaml`(dsh 自己读)
- ❌ 不传 `DEEPSEEK_API_KEY` / `DSH_MODEL` / `DEEPSEEK_BASE_URL`
- ✅ `cmd.Dir = cfg.Workspace`(运行时上下文)
- ✅ `DSH_PERMISSION_MODE=danger-full-access`(权限放开,用户原话)
- ✅ `--profile web --port 0` / `--profile headless`(transport flag)

`Info().Args` 暴露的 argv 与实际 spawn 一致(避免 code-review §3 drift):
- print-mode:`["--profile", "headless"]`
- chat session:`["--profile", "web", "--port", "0"]`(两份 Info;`Starter.Info` 选 print-mode 默认,`Start` 用 web 配置)

---

## 11. 不在范围(deferred)

| 项 | 理由 | 何时做 |
|----|------|--------|
| `dsh-jsonrpc-agent-pkg` JSON-RPC bridge | 见 §1.4 — 真 binary 未发布 | PyPI 真 wheel 发布后另 PR |
| dsh multi-modal 图片 | baseline-only,fallback `[file:...]` | dsh 加 image capability 后 |
| session.resume 跨 daemon | ~~dsh session 持久化机制是 JSONL,但无显式 `--resume`~~ **已支持**:`POST /api/session.fork{sessionId}` + `POST /api/session.list`;见 §13 | — |
| subagent UI 渲染 | host frame `subagent.started/finished` 本期不消费 | 后续单独 PR |
| Windows 支持 | dsh 在 Windows 是 non-goal(`pkg --sea` 不打 Windows) | dsh Windows 支持后 |

---

## 12. 排错速查

| 症状 | 根因 | 修法 |
|------|------|------|
| `dsh: not found in PATH` | npm `@deepseek-ai/dsh` 未装 | `npm install -g @deepseek-ai/dsh` |
| `dsh web: parse url timeout` | spawn 慢 / stdout 格式变 | 调 `parseWebURL` timeout 到 30s |
| HTTP 404 `/api/{method}` | method 写错(拼写/路径 dot vs slash) | 对照 `rpc-map.ts` 锁 method 名 |
| `result.ok=false, error.code="bad-request"` | envelope 缺 `type` 或 `method` 字段 | 加 `"type":"client-request"` 和 `"method"` |
| WS upgrade 失败(curl 52 empty reply) | 路径错(用 `/api/events/mux` 不是 `/api/events/mux`)| 用 dot 分隔 |
| `result.error.code="agent-busy"` | 上一 turn 未完 | retry with backoff,模型 codex `ErrTurnBusy` |
| `session.prompt ok=False` | session 没 model | session.create 后调 session.models 自动选 default；要换 model 直接重启 session |
| `Close()` 卡 30s | 服务端不响应 / cancel | SIGKILL 兜底(reuse codex closeOnce 模式) |
| WS 断线 | server 端重载 / 网络 | reconnect with backoff(client/connection.ts `ConnectionController` 模式) |
| 测试 hang | 走了代理 | unset 代理变量 |

---

## 13. 时间线

| 阶段 | 内容 | 工作量 | 状态 |
|------|------|--------|------|
| 1 | print-mode print.go + prompt.go + starter.go | 0.5d | ✅ 已落地 |
| 2 | print-mode 单测 + e2e | 0.5d | ✅ 已落地 |
| 3 | print-mode cmd/nightme/agents.go 注册 | 0.25d | ✅ 已落地 |
| 4 | print-mode 5 个 review 修复 | 0.25d | ✅ 已落地 |
| 5 | print-mode code-review 5 个修复 | 0.25d | ✅ 已落地 |
| 6 | doc/bridge/dsh.md 设计稿 | 0.5d | ✅ 当前 |
| 7 | chat session detect.go + session.go(spawn + parse URL + lifecycle + closeOnce)| 1.5d | 🔜 |
| 8 | chat session http.go(HTTP RPC client)| 0.5d | 🔜 |
| 9 | chat session ws.go(2 条 WS 下行)| 0.5d | 🔜 |
| 10 | chat session translate.go(F-52 状态机,镜像 pi)| 2d | 🔜 |
| 11 | chat session permissions.go(approval/question)| 1d | 🔜 |
| 12 | chat session respond.go | 0.5d | 🔜 |
| 13 | chat session starter.go 改造(Start 真正 spawn,Info 双 mode)| 0.5d | 🔜 |
| 14 | chat session 单测 25+(protocol/http/translate/permissions)| 1d | 🔜 |
| 15 | chat session e2e 10+(NIGHTME_REAL_DSH=1)| 1d | 🔜 |
| 16 | chat session review + 修 | 0.5d | 🔜 |
| 17 | **chat session resume(fork + list)** | 0.5d | ✅ done |
| **总计** | | **~10.75d** | 5.75d done,5d remaining |

---

## 13. 跨 daemon resume (session.fork + session.list)

dsh web 没有显式的 `--resume <id>` 启动参数,但暴露 2 个 server-side RPC:

| Method | Payload | 用途 |
|--------|---------|------|
| `session.fork` | `{sessionId}` | 从现有 session 开新(返回新 sessionId,带父 history) |
| `session.list` | `{}` / `{limit}` | 列 daemon-wide 全部 session 元数据 |

bridge 在 `Start(cfg)` 路径透传 `cfg.SessionID`(strict-resume 策略):

```
newDriver
  └─ handshakeSession(ctx, cfg)         ← ctx 由调用方持有,每个 RPC 各自有 handshakeTimeout
       ├─ cfg.SessionID != "" → POST /api/session.fork {sessionId}
       │    ├─ ok            → 捕获新 sessionId,d.sessionID = 新 id,resumed=true,INFO 日志
       │    └─ 任何失败      → WARN 日志 + resumeUnhealthyError(满足 errors.Is(err, agent.ErrResumeUnhealthy))
       │                       runtime 收到后清掉 stale id,用户下一条消息自动 fresh start
       └─ cfg.SessionID == "" → 直接 POST /api/session.create
                                  (失败是 true bridge error,向上 surface)
```

**为什么 fork 而不是 create-in-place**:dsh web 设计上不允许"原地接管"一个 sessionId —— 所有 session 写操作都通过 mux WS 上的 server-frame 派发,server 用 rpcId + sessionId 双重 key 做请求关联。原地接管意味着新进程要继续消费旧 server 的 WS,但旧 server 早已 close;物理上不可能。`session.fork` 让 server 端在数据库里建一份新 session,内容复制父 history,旧 sessionId 仍可在 server 列表里查到(供 audit / 二次 fork)。

**为什么 fork 失败直接拒,而不是 fallthrough create**(strict resume,镜像 claudecode §87-103):
- 静默 fallback → stale sessionId 永远停在 `registry.AgentSessionEntry.SessionID`
- daemon 每次重启都重 fork 同一个死 id → 用户每次都失忆 → 永久丢历史,operator 看不见(全 Debug 日志)
- 改成 strict refusal + 满足 `agent.ErrResumeUnhealthy` → runtime 自动清理 stale id(参考 `chatsession.go §1624` 的 retry-without-resume-id 路径 + `agentsession.go §1095` 的 clear-and-persist 路径)
- 用户代价:多一次冷启动;用户收获:不永久丢历史,operator 看得见 WARN 日志

**实测 fork 失败 error code**(`dsh 0.1.0-rc.6`,2026-08-15 probe):

| Error code | 含义 | bridge 行为 |
|------------|------|--------------|
| `fork-unavailable` | session 无 completed turn | ErrResumeUnhealthy |
| `session-not-found` | sessionId 不存在 | ErrResumeUnhealthy |
| `bad-request` | payload 缺 `sessionId` | ErrResumeUnhealthy |
| transport EOF / connection refused | server 不可达 | ErrResumeUnhealthy |

所有 fork 失败统一映射到 `ErrResumeUnhealthy`(经 `resumeUnhealthyError.Is` 双 sentinel match),让 runtime 用统一路径处理。

**resume picker 流程**(`Starter.ListSessions`):
1. runtime 调用 `Starter.ListSessions(ctx, cfg)`
2. bridge `Start()` 起一个一次性 dsh web(~1.5s 冷启动)
3. driver 调 `session.list`,decode `[]Session`(wire 字段 `items`)
4. bridge `Close()` 关掉一次性 dsh web
5. runtime 按 `Session.Blank==false && Session.Running==false` 过滤,按 `Session.UpdatedAt DESC` 排序,渲染 picker
6. 用户选 id → runtime 调 `/use dsh <id>` → 进入下一轮 Start,cfg.SessionID = id

**wire 契约**(`internal/bridge/dsh/protocol.go`,2026-08-15 实机 probe 锁定):

```go
// Session 一条 = session.list items[] 一项
type Session struct {
    ID          string          `json:"sessionId"`     // "session-<uuid>" 格式
    UpdatedAt   int64           `json:"updatedAt"`     // unix millis(不是 createdAt!)
    Running     bool            `json:"running"`       // 是否有 in-flight turn
    Blank       bool            `json:"blank"`         // 是否有 completed turn(决定能否 fork)
    CWD         string          `json:"cwd,omitempty"`
    AgentPreset string          `json:"agentPreset,omitempty"` // e.g. "standard"
    Projections json.RawMessage `json:"projections,omitempty"` // 含 title/todo 等,目前不解
}

type sessionListValue struct {
    Items []Session `json:"items"`  // ⚠️ 是 items,不是 sessions(2026-08-15 probe 修正)
}

type sessionForkValue struct {
    SessionID string `json:"sessionId"`  // 与 sessionCreateValue 同 shape
}
```

**实测关键修正**(vs 初始猜测):

| 项 | 初始猜测 | 实测 | 影响 |
|----|---------|------|------|
| `session.list` 字段名 | `sessions` | `items` | 旧实现 decode 出 0 条,UI 全空 |
| `Session` `CreatedAt` | 有 | 没有,只有 `UpdatedAt` | 排序 key 错 |
| `Session` `Slug/Title` | 有 | 没有(top-level) | 旧 UI 字段全空,title 在 `projections.values.title` |
| `limit` 参数 | 透传 | dsh 完全忽略 | 浪费 wire 字节,功能无效 |
| `session.fork` 在 blank session | 未知 | 拒(`fork-unavailable`) | 大部分首次 fork 都会失败 |

**runtime 集成点**(已存在):
- `cfg.SessionID` 经 `agentregistry.Spawner` → `bridge.Start` 链路透传
- `EventAgentReady.SessionID` 经 `agentsession.SetSessionID` 持久化到 `registry.AgentSessionEntry.SessionID`
- `nightme list` 的 RESUME 列已读该字段,dsh 现在会显示非空

**已知未实装**:dsh picker UI 还没建(目前只 CLI 路径走 opencode);dsh picker UX 是后续单独 PR。本 PR 只补全 wire + bridge 端到端通路,IM 渲染是 follow-up。
