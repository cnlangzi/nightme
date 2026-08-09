# F-OPENCODE — 接 opencode 的方案调研与设计 v2

> **Status**: 已落地 M3 — stage 1/2/3/4 全部完成,实现文档在 [docs/bridge/opencode.md](../bridge/opencode.md)
> **Scope**: nightme 接入 [sst/opencode](https://github.com/sst/opencode) 的方案
> **Audit 时间**: 2026-08-09
> **前置阅读**: [F-21-agent-modes.md](./F-21-agent-modes.md) §5.3 + [docs/bridge/codex.md](../bridge/codex.md) + [docs/bridge/opencode.md](../bridge/opencode.md) (实现)

---

## 0. 一句话结论

**接 `opencode serve` HTTP server + 手写 Go HTTP client（约 350 行）。**

理由：
- opencode 已经把 server 端做完了，运行 `opencode serve` 启 HTTP server，openapi.json 1MB 写明了 100+ 端点
- TypeScript SDK 是个**薄封装**（就 wrap 一下 `fetch`），没真值钱逻辑。**直接对 HTTP 不亏**
- 不写 Node shim（codex 已经是反例，见 [docs/bridge/codex.md §1.2](../bridge/codex.md)）
- 不复用现有 ACP bridge —— PTY + JSON-RPC 信封 + 6 个协议面缺口，纯工成本
- 不复用现有 bridge 抽象 —— F-21 里写的"opencode -> ModeACP"是基于当时的"ACP 比 HTTP 简单"的误判。**HTTP 端点 9 个，端点格式明确，比 ACP 反而少**

---

## 1. 现状摸底

### 1.1 nightme 已有基础设施

| 组件 | 位置 | 状态 |
|------|------|------|
| `agent.Agent` 接口 + `agent.Builtins` 注册表 | `internal/agent/agent.go` + `registry.go` | 稳定 |
| `internal/bridge/acp/` | PTY + JSON-RPC | 已实现 ACP v1 核心，但 opencode 端没跑过 |
| `internal/bridge/codex/pi/claudecode/pty/` | 各自协议 | 稳定 |
| `cmd/nightme/agents.go` | 注册表 | **目前没注册 opencode** |
| `configs/nightme.example.yaml` | — | sample-only 写了 `opencode acp` |

### 1.2 opencode 提供的接口（按"对 nightme 已足够"的最小集列出）

| 形态 | 触发 | 路径 | 备注 |
|------|------|------|------|
| **HTTP server** | `opencode serve --port 4096` | OpenAPI 1MB | 100+ 端点，**我们的目标** |
| **stdio JSON-RPC (ACP)** | `opencode acp` | 私有 | 备用方案（v1 设计稿写了这个） |
| **TypeScript SDK** | `npm i @opencode-ai/sdk` | 薄封装 fetch | 仅 JS/TS，**没有 Go 版** |

### 1.3 SDK 实际内容（读了 `packages/sdk/js/src/`）

```ts
// src/server.ts: createOpencodeServer(options) -> { url, close() }
const args = [`serve`, `--hostname=${...}`, `--port=${...}`]
const proc = launch('opencode', args, { env: ... })
// 等 stdout "opencode server listening on http://..." 解析 URL

// src/client.ts: createOpencodeClient({ baseUrl }) -> OpencodeClient
// 返回的 client 就是 hey-api 生成的对 OpenAPI 的 fetch wrapper
const client = createClient({ baseUrl })
client.interceptors.request.use(...)  // 注入 x-opencode-directory header
```

**SDK 没有任何"业务逻辑"**。它就是：
1. spawn `serve` + 解析 URL
2. 对 OpenAPI 100+ 端点做 fetch wrapper

→ 我们 Go 端手写一套 ≈ 同样薄

### 1.4 实际需要的端点（最小集）

| 端点 | 用途 | 触发 |
|------|------|------|
| `GET /api/health` | server 起来了吗 | Start |
| `POST /api/session` | 新建 session | Start 无 SessionID |
| `GET /api/session/{id}` | 取 session 信息（resume / 状态） | Start 有 SessionID |
| `POST /api/session/{id}/prompt` | 发消息 | SendBlocks |
| `GET /api/session/{id}/event` | SSE 事件流（agent_message / tool / permission） | 整个会话期间 |
| `POST /api/session/{id}/interrupt` | abort 当前 turn | `/abort` |
| `POST /api/session/{id}/model` | 切换 provider/model | `/use` 派生 |
| `GET /api/session/{id}/permission/{reqID}/reply` | 回包权限 | SendPermission |
| `POST /api/session/{id}/init` | 初始化 session 项目上下文 | 启动时 |
| `GET /api/config` | 读 opencode 配置（模型、provider） | optional |

**= 9 个端点**。其中只有 1 个 SSE 长连接需要关心。

### 1.5 事件类型（从 OpenAPI 读出）

`GET /api/session/{id}/event` SSE 推送以下类型（`discriminator: type`）：

```ts
{ type: "message.updated", properties: { message } }      // 用户/助手消息开始
{ type: "message.removed", properties: { sessionID, messageID } }
{ type: "message.part.updated", properties: { part } }    // ⭐ 主事件
{ type: "message.part.removed", properties: { sessionID, messageID, partID } }
{ type: "session.compacted", properties: { sessionID } }
{ type: "session.error", properties: { sessionID, error } }
{ type: "session.idle", properties: { sessionID } }         // ⭐ turn 结束信号
{ type: "session.diff", properties: { sessionID, diff } }
{ type: "permission.asked", properties: { ... } }           // ⭐ 权限请求
{ type: "permission.replied", properties: { ... } }
{ type: "session.created", properties: { info } }
{ type: "session.updated", properties: { info } }
```

`part` 是 union，包含：
- `TextPart` `{ type: "text", text, time, ... }` ← agent 文本
- `ReasoningPart` `{ type: "reasoning", text, ... }` ← 思考
- `ToolPart` `{ type: "tool", tool, callID, state: {pending|running|completed|error}, ... }`
- `FilePart` / `AgentPart` / `StepStartPart` / `StepFinishPart` / `SnapshotPart` / `PatchPart` / `SubtaskPart` / `RetryPart` / `CompactionPart` — 内部用，bridge 忽略或打 log

`message.part.updated` 上动作 = 翻译 ：

| part.type | 翻译为 |
|-----------|--------|
| `text` | `EventAgentText`（**按 part 边界 flush** — F-52 友好） |
| `reasoning` | `EventText` 带 `[思考] ` 前缀（对齐 pi） |
| `tool` (state=pending) | `EventAgentToolStart` |
| `tool` (state=running → content delta) | `EventAgentToolUpdate` |
| `tool` (state=completed\|error) | `EventAgentToolEnd` |
| _其余_ | debug log |

**重点**：`text` part 已经是 part 边界，**直接 emit EventAgentText 不需要 buffer**（vs pi / codex 的 token 累积）。这是 opencode 天然友好的一个点。

`session.idle` ← → `EventAgentDone{Reason:"settled"}`（turn 结束）
`session.error` ← → `EventAgentError`
`session.compacted` ← → `EventCompaction` (新概念)

### 1.6 权限请求格式

`permission.asked` 事件 payload（来自 OpenAPI `PermissionRequest` + 实际样本）：

```json
{
  "sessionID": "ses_xxx",
  "permission": "bash",
  "patterns": ["rm", "kill"],
  "metadata": { "command": "rm -rf build", "description": "..." },
  "always": ["rm -rf build"],
  "tool": { "messageID": "...", "callID": "..." }
}
```

回复端点：

```json
POST /api/session/{id}/permission/{requestID}/reply
{
  "response": "once" | "always" | "reject"
}
```

**optionId 字符串直接拿来用**，与 ACP 模式对齐了。

---

## 2. 方案设计

### 2.1 进程层

```
nightme ──spawn──> opencode serve --port 4096 ──> HTTP on 127.0.0.1:4096
                                          |
                                          +-- /api/session
                                          +-- /api/session/{id}/prompt
                                          +-- /api/session/{id}/event (SSE)
                                          +-- ...
```

**关键决策**：

| 决策 | 取舍 | 理由 |
|------|------|------|
| 进程 1:1 vs 共享 | **1:1** | 每 AgentSession 启一个 `opencode serve`。比 codex/pi 略重，但 opencode 启动快（约 1-2s），且隔离干净 |
| 端口分配 | `--port 0`（让 server 选空闲端口）+ 解析 stdout | 与 SDK 实际做法一致 |
| Auth | `OPENCODE_SERVER_PASSWORD` 环境变量，nightme 启动 server 时设 | SDK 默认无密码时 console.log "Warning: server is unsecured" —— 我们强制设 |
| Workspace 路由 | `x-opencode-directory: <encoded>` header | opencode server 已支持 single-process 多目录（v2 SDK 的设计） |
| TLS | 默认跳过（127.0.0.1 局域网） | 同 server mode 设计 |

### 2.2 关键不变量（与现有 bridge 对齐）

| 不变量 | codex | pi | opencode (v2) |
|--------|-------|-----|---------------|
| Transport 1:1 | ✅ | ✅ | ✅ |
| `close(events)` 唯一所有者 | lifecycle goroutine | lifecycle | lifecycle |
| `deliver()` 永不超时/不 drop | ✅ | ✅ | ✅ |
| `EventAgentDone ≠ close events` | ✅ | ✅ | ✅ |
| EventText 粒度（F-52） | by content block | by text_end | **by part**（天然对齐） |
| `pendingTurnActive` | ✅ | ✅ | ✅（turn 边界 = `session.idle`） |
| Producer/consumer 单写单读 | ✅ | ✅ | ✅ |

### 2.3 与 acp / codex 形态对照

```
                          acp/opencode          codex            pi               opencode v2 (本设计)
                          ─────────────          ─────            ──               ───────────────
spawn argv                 opencode acp           codex app-server pi --mode rpc   opencode serve
                          + (--port 4096...)     --listen stdio://
transport                  stdio JSON-RPC 2.0     stdio JSON-RPC   stdio JSONL      HTTP (JSON over fetch)
transport byte framing     ndJsonStream           newline          newline          json/xml/multipart (标准)
multipart/multiline        PTY (newline + raw)    newline          newline          SSE (event: ...\ndata: ...)
event subscription model   poll/notification      poll/notify      poll/notify     server-sent events
process:turn ratio         1:N (long-lived)       1:N              1:N             1:N (long-lived)
resume semantics           session/load           thread/resume    --session-id     GET /session/{id}
multi-modal                image true             image true       image only       file part mime: url
```

**最大区别**：sees-and-types 是 HTTP REST + SSE 推送，**没有 NDJSON 帧切分、没有 JSON-RPC 信封**。代码量比 ACP/JSON-RPC 都少。

### 2.4 进程生命周期的"坑"预判（candidates）

opencode serve 是 node 进程，启动比 codex/pi 慢；其他四个 bridge 都是 go 二进制。**nightme 端要承担**：

1. **Subprocess 启动延迟**：`opencode serve` 冷启动 1-3s。**缓解**：每个 AgentSession 独立启一份 server，**不要全局共享**（共享会引入 "服务器不知道哪个 workspace 是谁的" 复杂度）
2. **stdout 解析 URL**：`opencode server listening on http://127.0.0.1:4096` 这行**必须**匹配上才能继续。**缓解**：解析失败 → 5s 超时 → 关闭 + 错误
3. **端口冲突**：用 `--port 0` + 解析，**避免**手动分配
4. **server 重启**：daemon 重启 / SessionID 还在 → `GET /session/{id}` 应该能查到（server 持久化）。**确认**：实测一下
5. **MCP 启动慢**：opencode server 启动会初始化 MCP servers —— `--no-mcp` 跳过？但要 user aware
6. **大模型输出 + SSE 缓冲**：Go `http` 默认不带 buffer，SSE 接 `Flush()` 后立刻发，OK

### 2.5 包结构

```
internal/bridge/opencode/
  ├── opencode.go         # 公共常量 + 包 doc + 协议版本
  ├── server.go           # 启停 opencode serve 子进程 + 解析 URL
  ├── client.go           # HTTP client wrapper (只 9 个端点)
  ├── transport.go        # ⭐ SSE 解析 → Event（删 PTY 包袱）
  ├── agent.go            # *Agent (template + live) + Spec/Live 实现
  ├── translate.go        # EventMessagePartUpdated / EventSessionIdle /
  │                       # EventPermissionAsked / EventSessionError / ...
  │                       # → agent.AgentEvent (F-52 粒度)
  ├── permissions.go      # /permission/{reqID}/reply 路由
  ├── agent_test.go       # mock HTTP server (net/http/httptest)
  ├── transport_test.go   # SSE 解析
  ├── translate_test.go   # part → AgentEvent 转换
  ├── session_real_test.go  # 真 opencode 二进制 e2e (defensive Skip)
  └── README.md
```

**vs 现有 acp bridge**：去 PTY，去 JSON-RPC 2.0 信封，去 permission_call 队列（permission 直接走 HTTP reply），**纯 HTTP + SSE**。

### 2.6 关键代码骨架

```go
// internal/bridge/opencode/server.go
package opencode

import (
    "bufio"
    "context"
    "fmt"
    "io"
    "os/exec"
    "regexp"
    "sync"
)

var serverURLRegex = regexp.MustCompile(`opencode server listening on (https?://\S+)`)

type serverProc struct {
    cmd     *exec.Cmd
    baseURL string
    pid     int
}

func startServer(ctx context.Context, workspace string, password string) (*serverProc, error) {
    args := []string{"serve", "--hostname=127.0.0.1", "--port=0"}
    cmd := exec.CommandContext(ctx, "opencode", args...)
    cmd.Dir = workspace
    if password != "" {
        cmd.Env = append(os.Environ(),
            "OPENCODE_SERVER_PASSWORD="+password)
    }
    stdout, err := cmd.StdoutPipe()
    if err != nil { return nil, err }
    if err := cmd.Start(); err != nil { return nil, err }

    // 解析 "opencode server listening on http://..."
    scanner := bufio.NewScanner(stdout)
    deadline := time.After(10 * time.Second)
    for {
        select {
        case <-deadline:
            cmd.Process.Kill()
            return nil, fmt.Errorf("opencode: server start timeout")
        default:
        }
        if !scanner.Scan() {
            return nil, fmt.Errorf("opencode: server exited: %v", scanner.Err())
        }
        line := scanner.Text()
        if m := serverURLRegex.FindStringSubmatch(line); m != nil {
            return &serverProc{cmd: cmd, baseURL: m[1], pid: cmd.Process.Pid}, nil
        }
    }
}
```

```go
// internal/bridge/opencode/client.go
package opencode

type Client struct {
    baseURL  string
    http     *http.Client
    password string
}

func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
    var r io.Reader
    if body != nil {
        b, err := json.Marshal(body)
        if err != nil { return nil, err }
        r = bytes.NewReader(b)
    }
    req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
    if err != nil { return nil, err }
    if r != nil { req.Header.Set("Content-Type", "application/json") }
    if c.password != "" {
        // basic auth: opencode / $password
        req.SetBasicAuth("opencode", c.password)
    }
    req.Header.Set("x-opencode-directory", url.QueryEscape(/* workspace */))
    return req, nil
}

// 9 个端点
func (c *Client) CreateSession(ctx context.Context, opts CreateSessionOpts) (*Session, error)
func (c *Client) GetSession(ctx context.Context, id string) (*Session, error)
func (c *Client) Prompt(ctx context.Context, sessionID string, parts []PartInput) (*PromptResult, error)
func (c *Client) Subscribe(ctx context.Context, sessionID string) (SSEEventReader, error)
func (c *Client) Interrupt(ctx context.Context, sessionID string) error
func (c *Client) ReplyPermission(ctx context.Context, sessionID, requestID, response string) error
func (c *Client) SetModel(ctx context.Context, sessionID, providerID, modelID string) error
```

```go
// internal/bridge/opencode/transport.go (SSE 解析)
package opencode

import (
    "bufio"
    "encoding/json"
)

type SSEEvent struct {
    Type       string          `json:"type"`
    Properties json.RawMessage `json:"properties"`
}

func decodeSSE(r io.Reader, onEvent func(SSEEvent) error) error {
    scanner := bufio.NewScanner(r)
    scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024) // 10 MiB, 跟 codex 对齐
    for scanner.Scan() {
        line := scanner.Text()
        if line == "" || !strings.HasPrefix(line, "data: ") {
            continue
        }
        payload := strings.TrimPrefix(line, "data: ")
        var ev SSEEvent
        if err := json.Unmarshal([]byte(payload), &ev); err != nil {
            // log + continue, 不要因为一个坏 event 杀掉整个流
            continue
        }
        if err := onEvent(ev); err != nil {
            return err
        }
    }
    return scanner.Err()
}
```

```go
// internal/bridge/opencode/agent.go (template + live)
package opencode

type Agent struct {
    name    string
    command string
    args    []string

    session *session
    closed  chan struct{}
    closeOnce sync.Once
}

func New(name, command string, args []string) *Agent { ... }

func (a *Agent) Name() string { return "opencode" }
func (a *Agent) Mode() agent.Mode { return agent.ModeJSONIO } // 复用 JSONIO 枚举 (不重要)
func (a *Agent) Command() string { return "opencode" }
func (a *Agent) Args() []string { return []string{"serve", "--hostname=127.0.0.1", "--port=0"} }

func (a *Agent) Start(ctx context.Context, cfg agent.StartConfig) (agent.Agent, error) {
    s, err := newSession(ctx, sessionConfig{
        workspace: cfg.Workspace,
        sessionID: cfg.SessionID,    // resume 路径
    })
    if err != nil { return nil, err }
    return &Agent{
        name: "opencode",
        session: s,
        closed: make(chan struct{}),
    }, nil
}

func (a *Agent) SendBlocks(ctx context.Context, blocks []agent.ContentBlock) error {
    parts := make([]PartInput, 0, len(blocks))
    for _, b := range blocks {
        switch b.Type {
        case agent.ContentText:
            if b.Text == "" { continue }
            parts = append(parts, TextPartInput{Type: "text", Text: b.Text})
        case agent.ContentImage, agent.ContentFile:
            if b.Path == "" { continue }
            // opencode FilePartInput: { type: "file", mime, url: file://... }
            // 绝对路径直接转 file:// URL（opencode 自己读）
            parts = append(parts, FilePartInput{
                Type: "file",
                MIME: b.MediaType,
                URL:  "file://" + b.Path,
            })
        }
    }
    return a.session.client.Prompt(ctx, a.session.id, parts)
}

func (a *Agent) SendPermission(resp string) error {
    return a.session.replyPermission(resp)
}

func (a *Agent) New(ctx context.Context) error {
    // opencode 没有"reset"概念, 通过 session/init + 删除旧的实现
    // 简化: New() = 同一 session 跑新 prompt (与 most 长期 bridge 行为一致)
    return nil
}
```

### 2.7 翻译器（translate.go）

```go
internal/bridge/opencode/translate.go

type translator struct {
    deliver func(agent.AgentEvent) agent.AgentEvent
    agentName, workspace, branch string

    // turn state
    turnActive    bool
    pendingTools  map[string]*agent.AgentToolStartEvent  // callID → start event
}

func (t *translator) handleEvent(ev SSEEvent) {
    switch ev.Type {
    case "message.part.updated":
        var p struct {
            Part Part `json:"part"`
        }
        json.Unmarshal(ev.Properties, &p)
        t.handlePart(p.Part)

    case "session.idle":
        t.deliver(agent.AgentEvent{
            Kind: agent.EventAgentDone,
            Done: &agent.AgentDoneEvent{Reason: "settled"},
        })

    case "session.error":
        var p struct { Error json.RawMessage `json:"error"` }
        json.Unmarshal(ev.Properties, &p)
        t.deliver(agent.AgentEvent{
            Kind: agent.EventAgentError,
            Err:  fmt.Errorf("opencode session error: %s", p.Error),
        })

    case "permission.asked":
        var p PermissionRequest
        json.Unmarshal(ev.Properties, &p)
        t.handlePermissionAsked(p)

    case "session.compacted":
        t.deliver(agent.AgentEvent{Kind: agent.EventCompaction})  // 新概念单独 PR
    }
}

func (t *translator) handlePart(p Part) {
    switch p.Type {
    case "text":
        if p.Synthetic || p.Ignored { return }  // 跳过 session 摘要等
        t.deliver(agent.AgentEvent{
            Kind: agent.EventAgentText,
            Text: p.Text,
        })
    case "reasoning":
        if p.Text == "" { return }
        t.deliver(agent.AgentEvent{
            Kind: agent.EventAgentText,
            Text: "[思考] " + p.Text,
        })
    case "tool":
        t.handleToolPart(p)
    }
}
```

---

## 3. 实施计划

### 阶段 1 — Hello world (1 天)

```text
internal/bridge/opencode/
  ├── opencode.go          # 包 doc
  ├── server.go            # startServer + parse URL
  ├── client.go            # 9 个端点 (stub 即可)
  ├── transport.go         # SSE 解析
  ├── agent.go             # *Agent + Start + SendBlocks (最小)
  └── translate.go         # message.part.updated → EventAgentText

cmd/nightme/agents.go     # + agent.Builtins.Register(opencode.New("opencode", "opencode", nil))
                          # 删 configs/nightme.example.yaml 里的 "opencode acp" 注释
```

**acceptance**:
- `nightme run opencode 'echo hi'` 能跑通
- 日志能看到 text 块 + 最终 result
- `cat cmd/nightme/agents.go` 看到 opencode 在 builtin list 里

### 阶段 2 — 协议完整 (2 天)

- 完整 translate：tool pending/running/completed 三阶段
- `session.idle` → EventAgentDone (Turn 结束)
- `session.error` → EventAgentError
- `permission.asked` → EventAgentPermission + SendPermission 路由
- `/interrupt` (/abort 命令)
- `/model` (/use 派生)

### 阶段 3 — Resume + Restart (1 天)

- `cfg.SessionID != ""` 路径 → `GET /session/{id}` 验证存在
- 测试：daemon 重启后 chat_Session 仍能 resume
- 加 `NIGHTME_OPENCODE_INITIAL_DELAY=0` 的真机测

### 阶段 4 — 测试 + 文档 (1 天)

- mock HTTP server 测试 translate
- 真机 e2e (require opencode binary on PATH)
- `docs/bridge/opencode.md` 写完
- `docs/feat/F-OPENCODE.md` 状态 → "已落地"
- `configs/nightme.example.yaml` opencode 块正式化

**总改动量**: ~1000-1500 行 Go 代码（含测试）—— 比 v1 (480 行) 多，但**这回可是真接上 full HTTP server**,不是猜

### 阶段 5 — 后续（可选）

- `session.fork` 实现（F-34 的 /checkpoint 概念）
- `session.compact` 主动触发（context full）
- 多模态图片 (opencode 实际只接 file URL)
- MCP / tool list 暴露

---

## 4. 为什么不用其他方案

| 方案 | 拒绝理由 |
|------|----------|
| **A. 复用 acp bridge** (v1 设计稿) | PTY + JSON-RPC 2.0 信封 + 6 个缺口 + permission_call 队列。 6 个缺口里至少 3 个要改协议层。**复杂度不亚于直接写 HTTP** |
| **B. 包 Node SDK** | Codex 共识明确反对 Node shim。TypeScript SDK 也没有任何业务逻辑，纯 fetch wrapper |
| **C. OpenAPI codegen** | 1MB spec → 生成的 Go 客户端 5k+ 行。10 个端点用 5k 行代码，**收益与代价严重不匹配** |
| **D. 重写 opencode 为 Go** | 50k+ 行 TS 翻译。**Phase 999+** |

---

## 5. 风险与对策

| 风险 | 概率 | 影响 | 对策 |
|------|------|------|------|
| opencode serve 启动慢（>5s） | 中 | Start 慢 | 阶段 1 实测；超时调到 15s |
| opencode 协议涨（API 变化） | 中 | bridge 失效 | 紧跟 openapi.json 变化; lockin opencode version in test |
| 同一个 agent 多 chat 共享 server vs 各起各 | 低 | 复杂度 | 1:1, 简化 |
| 持久化 session 跨 daemon restart | 中 | resume 失败 | 阶段 3 实测 |
| `EventAgentText` 粒度按 part 但 part 太大 | 低 | UX 略降 | 跟使用 codex / pi 一样：part 已经是协议边界 |
| `EventCompaction` 没有现有映射 | 低 | 丢事件 | 阶段 5 加；先打 log |

---

## 6. 与 F-21 现有规划的关系

`F-21-agent-modes.md` §5.3 + §6 写 `opencode -> ModeACP (opencode acp)` —— **本设计稿 v2 推翻这一行**。

影响：
- `ModeACP` 枚举保留，但 opencode 不走它
- `cmd/nightme/buildAgentRegistry` 里 `opencode` 名字映射改为 `ModeJSONIO` (借用枚举，含义不重要) 或新增 `ModeHTTP`
- `internal/bridge/acp/` 不删（其它 ACP 客户端仍可用）
- 删除 `configs/nightme.example.yaml` 里的 `opencode acp` 注释

需要 decide：**新增 `ModeHTTP` 枚举？** 还是借用 `ModeJSONIO`？倾向加 `ModeHTTP`，语义清晰。

---

## 7. 关键不变量复述

1. **生命周期一致**：1:1 子进程, lifecycle goroutine 拥有 close(events), producer 永不超时永不 drop
2. **EventText 粒度**: 按 part 边界, 粒度天然对齐 F-52
3. **多 turn**: `EventAgentDone{Reason:"settled"}` 不关 events channel
4. **resume**: `cfg.SessionID` → `GET /session/{id}` 验证
5. **丢弃 lo-fi**: `message.updated` / `message.removed` 等与 runtime 无关的事件一律不打

---

## 8. 验收清单

- [ ] `internal/bridge/opencode/` 全部文件存在
- [ ] `cmd/nightme/agents.go` 注册 `opencode`
- [ ] `nightme agents` 列出 opencode
- [ ] `nightme run opencode 'echo hi'` 跑通
- [ ] 真机 e2e 测试 require opencode on PATH
- [ ] mock HTTP server test 覆盖 translate / SSE / permission
- [ ] `docs/bridge/opencode.md` 写完
- [ ] `configs/nightme.example.yaml` opencode 块正式化

---

## 9. 参考链接

- [opencode SDK server.ts](https://github.com/anomalyco/opencode/blob/dev/packages/sdk/js/src/server.ts)
- [opencode SDK client.ts](https://github.com/anomalyco/opencode/blob/dev/packages/sdk/js/src/client.ts)
- [opencode SDK example.ts](https://github.com/anomalyco/opencode/blob/dev/packages/sdk/js/example/example.ts)
- [opencode 完整 OpenAPI 1MB spec](https://github.com/anomalyco/opencode/blob/dev/packages/sdk/openapi.json)
- [opencode serve docs](https://opencode.ai/docs/server/)
- [opencode SDK docs](https://opencode.ai/docs/sdk/)
- [opencode session test (v2)](https://github.com/anomalyco/opencode/blob/dev/packages/sdk/js/test/session-history.test.ts)
- [nightme F-21-agent-modes.md](F-21-agent-modes.md) — 需更新 §5.3
- [nightme bridge/codex.md](../bridge/codex.md) — 单一后端、生命周期踩坑

---

## 10. 变革总结 (vs v1)

| 维度 | v1 (复用 ACP) | v2 (本设计) |
|------|---------------|-------------|
| 入口 | `opencode acp` | `opencode serve` |
| 协议 | JSON-RPC 2.0 over PTY | HTTP + SSE |
| 行 framing | NDJSON | SSE |
| 授权 | ACP initialize 握手 | basic auth |
| 启动时间 | ~1s (一样) | ~1-3s |
| 代码改动量 | ~480 行 (acp bridge 加 6 method) | ~1000-1500 行 (新包) |
| 复用程度 | 90% 复用 | 0 复用 |
| 优点 | 衔接 ACP 协议; 不开新包 | 直接对 first-party HTTP; SSE 简单 |
| 缺点 | 6 个协议面要补, PTY 包袱 | 全新包; 1 个 SSE 解析器 |
| 失败点 | acp 协议表面涨 | opencode HTTP API 涨 |
| **怎么选** | 想少写新代码 | 想直接对接 first-party API |

**v2 推荐理由**: 用户明确说"不要复用现有实现, 以最方便的方式对接" → HTTP server 是 first-party 出口, 9 个端点明确, 字段清晰, 还自带 OpenAPI spec; 手写 Go client 比把 acp bridge 改 shape 简单。
