# dsh — DeepSeek Harness Bridge (shared-host only)

> **Status**: ✅ **统一架构 2026-08-22** — `Start` 与 `RunOnce` / `Review` **都走 `--profile web` shared host**;`dsh --profile headless` 路径已废弃
> **Wire 协议参考**: [./dsh-api.md](./dsh-api.md) — 权威 wire contract(TS source + 实机)
> **共享 host 设计 + 实现**: [./dsh-shared-host.md](./dsh-shared-host.md) — 1:N multiplexing、全局 watchdog + restart recovery、`workspace.archiveSession` 行为
> **Scope**: `internal/bridge/dsh/` — nightme 侧的 DeepSeek Harness (`@deepseek-ai/dsh`) bridge
> **形态**:**一桥一端** — 全部走 shared-host web
>   - **`Starter.Start`(chat session,long-lived)**:持有 `*Agent` 多轮
>   - **`Starter.RunOnce` / `Starter.Review`(一次性)**:与 `Start` 同一形态,但跑完 `defer a.Close()` 走 `workspace.archiveSession` 归档
>
> **本版与 2026-08-16 版的关键差异**:
> - `dsh/print.go` + `dsh/print_real_unix_test.go` **删除**(旧 headless 路径不再使用)
> - `dsh.RunOnce` 不再 `proc.New` 任何子进程;改为 `s.Start + drain + defer Close`,与 `acp.RunOnce` 同构
> - **R2 上下文隔离自动满足** —— 每次 RunOnce 在 dsh web 里 `session.create` 一个独立 sessionId,显式隔离;不再依赖 dsh CLI 的"是否读 ~/.dsh 共享状态"行为
> - **R4 归档复用现有 `driver.Close`** —— 已有 `workspace.archiveSession` 调用,`defer a.Close()` 自动触发,**无新代码**
> **核心原则(用户 2026-08-14 锁定)**: 接入底层 AI agent,**不修改 agent 本地默认配置**;nightme 只管 transport + permissions(权限默认全开)。详见 [agent-no-config-tampering memory](../../.claude/projects/-Users-geax-code-geax-github-com-cnlangzi-nightme/memory/agent-no-config-tampering.md)
> **姊妹文档**:
> - [docs/bridge/dsh-shared-host.md](./dsh-shared-host.md) — **新架构:全局单实例 dsh,1:N 多路复用**
> - [docs/bridge/dsh-api.md](./dsh-api.md) — wire 协议权威参考
> - [docs/bridge/claude.md](./claude.md) — stream-json transport,长生命周期
> - [docs/bridge/codex.md](./codex.md) — JSON-RPC over stdio + print-mode
> - [docs/bridge/pi.md](./pi.md) — JSONL RPC over stdio + print-mode
> - [docs/feat/F-dsh-bridge.md](../feat/F-dsh-bridge.md) — print-mode 阶段的设计稿(已 closed)
> - [docs/feat/F-dsh-shared-host.md](../feat/F-dsh-shared-host.md) — 共享 host 完整设计 + 实现记录
> - [docs/bridge/cli-transport.md](./cli-transport.md) — pipe / lifecycle 通用约束

---

## 1. 调研结论(2026-08-14 实机 + 源码双验)

### 1.1 dsh 是什么

[DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness) 是 DeepSeek AI 开源的 agent harness,核心口号 "everything is a plugin",由 Cordis 框架驱动。CLI 名 `dsh`,MIT 协议,主语言 TypeScript(23 MB TS + 168 KB Python)。

```
npx @deepseek-ai/dsh web                  # 默认 Web UI,127.0.0.1:3080
pnpm dsh --profile headless "<task>"      # 单 turn print-mode,plain stdout
```

### 1.2 实机三种可接入口(2026-08-14 实机 + 源码)

| 入口 | 实机状态 | nightme 实际使用? | 备注 |
|------|---------|-------------------|------|
| `dsh --profile web` (HTTP :3080) | ✅ 实机启 + 验证 HTTP + WS + **image/text 混合投递** | **✅ Start + RunOnce + Review 全部走这条** | server-side session 持久化;nightme 通过 `session.create` 开新 sessionId,R2 隔离自动满足 |
| `dsh --profile headless` (npm `@deepseek-ai/dsh`) | ⚠️ 实装 + 历史实测 `PONG` 通 | **❌ 2026-08-22 废弃** | `headless --help` 无 `--resume`/`--session-id`,只暴露 `-h`;print-mode 旧入口,每调用 = 新 session —— 但**没有显式 sessionId** 隔离,可能从共享状态读到主 chat 上下文 |
| `dsh-jsonrpc-agent-pkg` (pip 单文件可执行) | ❌ PyPI wheel 是 placeholder(1.5 KB),**真 binary 未发布** | ❌ | **DEPRECATED 路径** — 详情见 §1.4 |

### 1.3 wire 协议(从 `packages/host/apiproxy/src/` 源码 + 实机双重验证)

#### 1.3.1 spawn 配方
```
dsh --profile web
```
- **不带 `--port` flag** — 让 dsh 用自己的默认端口 **3080**(`host/discover.go:36` 的 `defaultDSHPort`)
- **绝对不使用** `--port 0`(OS 随机端口):实机验证 dsh 不接受 0 + 会随机到奇怪端口,与 "reuse-or-spawn" 契约冲突(详见 `host/lifecycle.go:246-278`);`lifecycle.go:262-278` 还会**硬断言**端口必须是 3080,否则 kill spawn 并报清晰错误
- stdout 第一行 `dsh web: http://127.0.0.1:3080`,正则提取 `<host>:<port>`;bridge 还要再校验端口 == 3080,不一致就 refuse

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
| `session/projection` | `{sessionId, seq, key, value}` | 投影帧。`key:"todos"` 的 `value` 是 `TodoItem[] \| null`(数组,不是 `{todos:[...]}` 包装)。Dashboard To-dos 条读这个 key;详见 §6 |
| `session/queue` | `{sessionId, items}` | 输入队列快照;F-38 后续若做 QueueDock UI 时用 |
| `session/jobs` | `{sessionId, jobs}` | 后台任务;本期不发 event |
| `approval/requested` | `{sessionId, approvalId, toolName, callId?, reason?}` | **`EventAgentPermission{ResponseCh}`** + 记 `approvalId` |
| `approval/resolved` | `{sessionId, approvalId, outcome}` | audit trail,debug log |
| `question/requested` | `{sessionId, questions: AskUserQuestionItem[]}` | **`EventAgentPermission`**(复用;`Questions` 整批 + `Options` 仅第一题)|
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

### 1.5 与现有 bridges 对照(2026-08-22 更新)

| 维度 | claude | codex | pi | opencode | **dsh** |
| |--------|-------|-----|----------|---------|
| Transport | stream-json stdio | JSON-RPC stdio | JSONL RPC stdio | HTTP + SSE | **HTTP + WS(shared host)** |
| Spawn | `claude -p`(RunOnce)/ stream-json(Start) | `codex exec`(RunOnce)/ app-server(Start) | `pi --mode json -p`(RunOnce)/ RPC(Start) | `opencode run`(RunOnce)/ `opencode acp`(Start) | **`dsh --profile web`(统一)** |
| 长生命周期 | ✅ | ✅ | ✅ | ✅ | **✅(同一进程,RunOnce 用临时 sessionId)** |
| 接收事件 | stdout stream-json | JSON-RPC notifications | JSONL RPC events | SSE | **WebSocket mux demux** |
| Approval | JSON-RPC request | JSON-RPC server request | (MVP auto cancel) | HTTP RPC | **HTTP POST `/api/respond`** |
| 多模态 | ✅(stream-json content array) | ✅(-i flag) | ✅(prompt.images) | ✅(attachments) | ✅(text + image inline)|
| 跨进程 resume | ✅ --resume | ✅ thread/resume | ✅ --session-id | ✅ sessionId | ✅ `session.fork`(`dsh-api.md` §2)|
| 二进制自包含 | ❌ npm | ❌ npm | ❌ npm | ❌ npm | ✅ npm |
| RunOnce 隔离 | `claude -p` 新进程 | `codex exec` 新进程 | `pi -p` 新进程 | `opencode run` 新进程 | **`session.create` 新 sessionId,共用 dsh web 进程** |
| RunOnce 收尾归档 | N/A(进程退即清) | N/A | N/A | N/A | **`defer a.Close()` → `workspace.archiveSession`** |

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
3. **chat session + RunOnce + Review 统一走 `dsh --profile web` HTTP+WS** — 不依赖 pip / 不需要 clone dsh 仓
4. ~~**print-mode 走 `dsh --profile headless` CLI** — 零新依赖,实机验证 PONG/ALPHA/BETA/YES/OK~~ — **2026-08-22 废弃**:headless 无显式 sessionId 隔离,可能从共享状态读到主 chat 上下文;RunOnce 改走 web,`dsh/print.go` 已删除
5. **Start 报错清晰化**:`Starter.Start` 真实连 shared host(非返 "not implemented");`RunOnce` 复用 `Start` 形态
6. **abort lifecycle 复用 codex 模式**:closeOnce.Do 守护 SIGINT/SIGTERM 兜底,channel 只在 lifecycle goroutine 关闭
7. **RunOnce/Review 收尾归档**:`defer a.Close()` 自动驱动 `driver.Close` → `Router.Unsubscribe` + `session.cancel` + `workspace.archiveSession`(`dsh/session.go:916-955`),**无新代码**
8. **RunOnce = `Start` + drain + `Close`** — 与 `acp.RunOnce` 同构;不引入新的子进程管理代码

### 2.2 已排除

1. **`dsh-jsonrpc-agent-pkg` JSON-RPC** — 见 §1.4,DEPRECATED
2. **bundled cordis.yml** — 不修改 dsh 本机配置(原则违反)
3. **TUI / acp / 自定义 profile** — `pnpm run demo:acp` 需 clone dsh 仓,`tui` 需 TTY,profile 需 `dsh plugin add` 安装;**不在实机**
4. ~~**PTY fallback for headless** — `dsh --profile headless` 是一次性进程,PTY 接不到 stdin 续 turn,**结构上不可能多 turn**~~ — 历史项;headless 路径整体废弃,不再讨论
5. **headless 作为 RunOnce 路径** — 2026-08-22 决策:即使实机跑通,因无显式 session 隔离,**禁止**再用于任何 nightme 内部调用

---

## 3. 包结构(unified shared-host 阶段,~1800 行 Go)

```
internal/bridge/dsh/
  ├── doc.go                 # package doc
  ├── starter.go             # Starter + Info(ModeJSONIO) + Detect + Start + RunOnce
  │                          # ★ RunOnce 走 Start + drain + defer Close
  ├── ~~print.go~~           # ❌ 2026-08-22 删除:RunOnce 不再走 headless subprocess
  ├── starter_test.go        # Starter-level tests(RunOnce / Review / drainForRunResult)
  ├── ~~print_real_unix_test.go~~ # ❌ 2026-08-22 删除:e2e 改走 session_real_unix_test.go
  ├──
  ├── detect.go              # exec.LookPath("dsh") + `dsh web` smoke probe
  ├── session.go             # host.EnsureSharedHost + parse URL + handshake + lifecycle + closeOnce
  │                          # ★ driver.Close 包含 workspace.archiveSession(RunOnce 收尾)
  │                          # ★ driver.Close 包含 workspace.archiveSession(RunOnce 收尾)
  ├── http.go                # HTTP RPC client(POST /api/{method})
  ├── ws.go                  # WebSocket client(mux + host,下行 only)
  ├── translate.go           # MuxFrame/HostFrame → agent.AgentEvent(F-52 状态机)
  ├── permissions.go         # approval/requested → EventPermission + pending map
  ├── respond.go             # approval/question 答案走 /api/respond 回环
  ├── host/                  # shared host 子包(lifecycle / ensure / client / stream / router / health / watchdog)
  ├──
  ├── session_test.go        # session lifecycle 单测
  ├── http_test.go           # HTTP envelope marshal/unmarshal 单测
  ├── translate_test.go      # MuxFrame 翻译表全覆盖(42 事件中 bridge 关心的 11 个)
  ├── permissions_test.go    # approval 流 + 5min timeout 压 200ms
  ├── session_real_unix_test.go # NIGHTME_REAL_DSH=1 e2e(完整 create → prompt → drain → close)
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
| `dsh --profile web` HTTP + WS | ✅ **Start + RunOnce + Review 全部走这条** | npm 已装,实机多 turn 验证通,wire 文档化;R2 隔离自动满足(sessionId 显式) |
| ~~`dsh --profile headless` CLI~~ | ~~✅ print-mode~~ | **2026-08-22 废弃** — 实机跑通但无显式 sessionId,可能从共享状态读到主 chat 上下文;RunOnce 改走 web |
| `dsh-jsonrpc-agent-pkg` | ❌ | 见 §1.4 — PyPI placeholder,真 binary 未公开发布 |
| `pnpm run demo:acp` | ❌ | 需 clone dsh 仓 + pnpm install,用户实机无 |
| `dsh --profile tui` | ❌ | 需 TTY,非 spawn 友好 |

---

## 4. 实施配方

### 4.1 spawn (`session.go`)

```go
// internal/bridge/dsh/host/lifecycle.go::spawnAndWire (注释化展示;
// 当前实现:EnsureSharedHost 走 host 包,dsh 包不再自己 spawn)
func newDriver(ctx context.Context, s *Starter, cfg agent.StartConfig) (*driver, error) {
    if cfg.Workspace == "" {
        return nil, fmt.Errorf("dsh: workspace is required")
    }
    
    // ★ 不传 --port:让 dsh 用自己的默认 3080;lifecycle.go:262-278
    //   会硬断言端口必须是 3080,否则 refuse spawn。
    cmd := proc.New(ctx, "dsh", "--profile", "web")
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
        // EventAgentTaskCreate(snapshot) — dashboard To-dos / 任务 strip
    case "step/start", "step/end":
        // 不 emit; sessionStats(TTFT / tok/s),不是 TodoPanel
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
- `SendPermission` 对 **approval** 用 `approvalId` 路由;对 **question** 用 **server-frame `rpcId`**(host pending 表的 key),不要用 `sessionId+":q"`
- 只有 1 个 pending approval 时,rpcId == approvalID;多 approval 并发时按 approvalID 路由,避免 codex §6.4 那样的乱答
- 5 min timeout(`permissionTimeout` 包级 var),过期 → decline,test 压 200ms
- **AskUserQuestion 批答**:host `matchesQuestions` 要求 `answers.length == questions.length` 且 `answer.id === question.id` 按序。飞书单题卡点选项即 `SendPermission(label)`;**Type your answer** Submit / **Skip this question** 走 `nm-q:`(`custom` 或空 `selected`)。多题卡在卡内翻页(`Step`/`Picks`),中间 click PATCH 并且在 `card.action.trigger` 回调里带回下一张卡(`card.type=raw`),最后一步才 inbound `nm-q:` JSON 批答。飞书交互卡设计与踩坑见 [feishu-cards.md](../channel/feishu-cards.md)。
- **Approval ≠ Question**:mux `approval/requested` 用 `ApprovalResponse{outcome:allowed-once|rejected}`(飞书 **Waiting for approval** / Allow once / Reject);`question/requested` 用 `QuestionResponse`。两种卡分类型。dashboard 点 Allow once 后 host 发 `approval/resolved`,bridge **继续收 mux 事件**(不卡在 Feishu Action Needed),并 PATCH 掉飞书按钮。
- 新 session / attach / `/new` 后发 `/permission danger-full-access`(host 拦截 slash,不开模型 turn),避免 workspace-write 下 git worktree lock 反复授权。

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
t0   cmd.Start ──→ spawn dsh --profile web(默认 port 3080,不带 --port flag)
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
| `session/event` `todo/write` | `{event:{todos:[{content, status}]}}` | `EventAgentTaskCreate`(snapshot)| dashboard **To-dos / 任务** strip; last-write-wins 整表。字段是 `todos` 不是 `items` |
| `session/event` `step/start` / `step/end` | `{event:{turn, step}}` | (不 emit)| 一次模型推理 + 其工具;折叠进 `sessionStats`(TTFT / tok/s)。**不是** TodoPanel |
| `session/projection` `key:"todos"` | `value: TodoItem[] \| null`(数组直出) | 应对齐 `EventAgentTaskCreate`| host fold:最新 `todo/write` 直到下一次 `turn/start`(`value:null` 退休 plan)。**与** `todo/write` 的 object `{todos:[...]}` **形状不同**。当前 `applyTodoProjectionLocked` 按 object 解,数组帧会丢;见 [dsh-api.md §3.4.3](./dsh-api.md) |
| `session/event` `approval/asked` | `{event:{toolCallId, toolName, action, options}}` | (单独走 permissions.go) | **不**直接发 EventPermission 给 runtime,经 permissions 层 normalize |
| `approval/requested` | `{sessionId, approvalId, toolName, callId?, reason?}` | `EventAgentPermission{ResponseCh}` | 见 §4.5 |
| `question/requested` | `{sessionId, questions}` | `EventAgentPermission` (复用,多 question)| `Questions` 整批保留;`Options` 仅第一题标签。飞书 `len>1` 走卡内向导,最后一步才 `POST /api/respond`(host `matchesQuestions` 要求 answers 与 questions 等长且 id 对齐)。单题 / 点选标签仍走 `questionAnswerFor` |
| `session/queue` | `{sessionId, items}` | (debug;F-38 后续) | |
| `session/jobs` | `{sessionId, jobs}` | (debug;本期不发) | |
| `session/projection` | `{sessionId, seq, key, value}` | (见上 `key:"todos"`;其余 title / sessionStats 等不发 chat event) | 字段名是 `key` 不是 `projection` |
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
**实测**:`dsh --profile web`(不带 `--port`,用 dsh 默认端口 3080) → stdout `dsh web: http://127.0.0.1:3080`,约 1.5s 启动
**影响**:用正则 `dsh web: http://([^:]+):(\d+)` 提取 host:port;**然后**硬断言端口必须是 3080(`host/lifecycle.go:262-278`),否则 refuse spawn
**关键决策**(2026-08):不带 `--port 0` 是故意的。`--port 0` 会让 dsh 随机选端口,与 "reuse-or-spawn" 契约(3080 或 fail loud)冲突 —— 如果 user 自己跑了一个 dsh 在 3080,nightme 用 `--port 0` 起在另一个端口,**sessions 会跨实例分裂**,无法 mux demux。`lifecycle.go:246-278` 显式 refuse fallback 到 `--port 0`

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
- ✅ `--profile web`(统一 transport flag;**不带 `--port`**,用 dsh 默认 3080;**2026-08-22 起** `--profile headless` 不再使用)

`Info().Args` 暴露的 argv 与实际 spawn 一致(避免 code-review §3 drift):
- **统一**:`["--profile", "web"]`(Start 与 RunOnce 同一份)
- ~~print-mode:`["--profile", "headless"]`~~ — **2026-08-22 删除**

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
| 1 | print-mode print.go + starter.go | 0.5d | ✅ 已落地(2026-08-22 之后**已删除**)|
| 2 | print-mode 单测 + e2e | 0.5d | ✅ 已落地(已删除)|
| 3 | print-mode cmd/nightme/agents.go 注册 | 0.25d | ✅ 已落地 |
| 4 | print-mode 5 个 review 修复 | 0.25d | ✅ 已落地 |
| 5 | print-mode code-review 5 个修复 | 0.25d | ✅ 已落地 |
| 6 | doc/bridge/dsh.md 设计稿 | 0.5d | ✅ 当前 |
| 7 | chat session detect.go + session.go(spawn + parse URL + lifecycle + closeOnce)| 1.5d | ✅ 已落地 |
| 8 | chat session http.go(HTTP RPC client)| 0.5d | ✅ 已落地 |
| 9 | chat session ws.go(2 条 WS 下行)| 0.5d | ✅ 已落地 |
| 10 | chat session translate.go(F-52 状态机,镜像 pi)| 2d | ✅ 已落地 |
| 11 | chat session permissions.go(approval/question)| 1d | ✅ 已落地 |
| 12 | chat session respond.go | 0.5d | ✅ 已落地 |
| 13 | chat session starter.go 改造(Start 真正连 host)| 0.5d | ✅ 已落地 |
| 14 | chat session 单测 25+(protocol/http/translate/permissions)| 1d | ✅ 已落地 |
| 15 | chat session e2e 10+(NIGHTME_REAL_DSH=1)| 1d | ✅ 已落地 |
| 16 | chat session review + 修 | 0.5d | ✅ 已落地 |
| 17 | chat session resume(fork + list) | 0.5d | ✅ done |
| 18 | dashboard parity(reasoning 独立 block,见 §14) | 1d | ✅ done(2026-08-16)|
| **19** | **RunOnce/Review 迁移到 shared host(本次)**:删 `dsh/print.go` + `print_real_unix_test.go`,改 `starter.go::RunOnce` 为 `Start + drain + defer Close`,补 4 个测试 + 文档更新 | **0.5d** | **🔜 本 PR** |
| **总计** | | **~12.25d** | 11.75d done,0.5d remaining(本 PR) |

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

---

## 14. dashboard parity 差距定位与最小 headless 复刻(2026-08-16)

> 触发问题:在 dashboard 上选中某个 session,能实时看到选中的 session 的 `tool / think / reply`,但 nightme 这边收到 `tool ✅ / think ❌ / reply ⚠️` 不全。本节定位并给出最小 headless 复刻方案。

### 14.1 现状对照表

| dsh wire frame | dashboard 行为 | nightme `dispatch.go` 现状 | 结果 |
|---|---|---|---|
| `assistant/chunk{type:"text-delta"}` | 实时拼接并显示 | `handleAssistantChunk` 把 `data.Chunk.Text` 写进 `tr.textBuf[idx]` 缓冲,**不发任何 AgentEvent** | **reply 不实时**,等 `assistant/message` 或 tool/call 边界才 flush;无 tool 的纯文本回答要等到 `turn/end` 的 `EventAgentResult.Text` 才一次性出现 |
| `assistant/chunk{type:"reasoning-delta"}` | "Show thinking" 折叠区 | `handleAssistantChunk` 无视 `data.Chunk.Type`,把 reasoning 的 text 也当 text 写入 `textBuf` | **think 串进 reply 内容**,且没有独立路径 |
| `assistant/chunk{type:"block-end", block:{type:"reasoning", text:"..."}}` | 一次性补齐整段 thinking | **完全丢弃**(`handleAssistantChunk` 的 switch 还没拆 `Chunk.Type`,该分支不存在) | think 进一步丢失 |
| `assistant/message.content[type:"reasoning"]` | 拆成 thinking 块独立展示 | `pickText(content)` 只挑 `Type=="text"`,**reasoning 整段被过滤掉** | think 丢光 |
| `assistant/message.content[type:"text"]` | 显示 | 累积进 `tr.pendingText`,等 tool/call / turn/end 边界 flush | reply 不实时 |
| `tool/call` | 工具卡片 | `handleToolCall` 发 `EventAgentToolStart` + 记 `pendingTools[CallID]` | ✅ 正常 |
| `tool/result` | 结果卡片 | `handleToolResult` 发 `EventAgentToolEnd` + 回填 Name/Args | ✅ 正常 |
| `user/message` | 用户消息回显 | `handleUserMessageEcho` **主动丢弃**(`return nil`) | OK,runtime 已经从 inbound 路径知道这条用户消息 |

### 14.2 与 dashboard demux 的对比(demux 的真相)

`@deepseek-ai/dsh-session/lib/types/types.d.ts:264-280`(`SessionEventMap`)和 `@deepseek-ai/dsh-llm/lib/types/types.d.ts:267-297`(`StreamChunk`)已经把 chunk 分类锁死:

```ts
type StreamChunk =
  | { type: 'block-start'; index; blockType: ContentBlockType }       // text | reasoning | tool-call | image | tool-result
  | { type: 'text-delta';      index; text }
  | { type: 'reasoning-delta'; index; text }                          // ← dashboard 的 "Show thinking"
  | { type: 'tool-call-delta'; index; id; name?; argumentsDelta }
  | { type: 'block-end';       index; block: ContentBlock }           // ← reasoning 整段落地在这里
  | { type: 'usage';           usage: TokenUsage }
  | { type: 'finish';          reason: FinishReason; replayState? }
```

dashboard 用 `@deepseek-ai/dsh-client-runtime/lib/types/client/sessions/partial.d.ts::PartialAccumulator.push(chunk)` 对每个 `StreamChunk` 按 `blockType` 分桶(`textBuf` / `reasoningBuf` / `tool-call-args`),`assistant/message.content[]` 再用 `toAssistantBlocks(content)` 把 `ContentBlockMap`(`text` / `reasoning` / `image` / `tool-call` / `tool-result`)转成 UI 关心的 `AssistantBlock`(text / reasoning / image / tool-call / other)。

dashboard 看到 think 的两条路径:
1. `assistant/chunk{type:"reasoning-delta"}` 实时拼到 reasoningBuf
2. `assistant/chunk{type:"block-end", block:{type:"reasoning", text:"…"}}` 整段定型

dashboard 看到 reply 的路径:`assistant/chunk{type:"text-delta"}` 实时拼到 textBuf,按 token 上屏。

### 14.3 nightme 这边对应的代码现状

`internal/bridge/dsh/dispatch.go:231-252`(`handleAssistantChunk`)目前只有一种处理方式,与 `data.Chunk.Type` 无关:

```go
// 现状:无视 Chunk.Type / BlockType,一律当 text 累积到 textBuf
b, ok := tr.textBuf[idx]
if !ok { b = &strings.Builder{}; tr.textBuf[idx] = b }
b.Grow(256)
b.WriteString(data.Chunk.Text)   // ❌ reasoning-delta 也走这里
return nil                       // ❌ 永远不发出 AgentEvent
```

`internal/bridge/dsh/translate.go:139-150`(`pickText`)的 content[] 分类也只看 `type=="text"`:

```go
for _, b := range content {
    if b.Type == "text" && b.Text != "" {   // ❌ reasoning 被过滤
        ...
    }
}
```

reply 的 flush 路径(目前只在三个边界触发):
- `handleToolCall` 到达时(`dispatch.go:281-292`)把 `tr.pendingText` 整体 emit `EventAgentText`
- `handleTurnEnd` 到达时(`dispatch.go:419-433`)把 `tr.lastText` 兜底成 `EventAgentResult.Text`
- `handleAssistantMessage` 不直接发文字,只把 `pickText(...)` 塞进 `tr.pendingText`

**结论**:一段无 tool 的纯文本回答,nightme 只在 `turn/end` 时通过 `EventAgentResult.Text` 一次性出现;含 reasoning 的回答,reasoning 文本会被串进 `textBuf` → 误显示为 reply 的一部分,thinking 内容等于丢光。

### 14.4 完美复刻方案(headless,严格走 `agent.AgentEvent` 路径)

按 dashboard 的 demux 行为对齐,**只动 `internal/bridge/dsh/` 包内的 handler 与 translator 状态**。nightme 的 `messages.OutThinking` 和 `internal/gateway/outbound/translate.go:31-63` 的 `[思考] ` 前缀约定已经存在(`gateway translate` 检测到该前缀就把事件转 `OutThinking`),直接复用这条已有管道,**不需要新 EventKind、不需要新 OutboundKind、不动上层**。

#### 14.4.1 `protocol.go` — `assistantChunk` 加 `Block` 解析字段,`contentBlockDTO` 已经够用

```go
type assistantChunk struct {
    Type           string            `json:"type"`               // text-delta | reasoning-delta | block-start | block-end | tool-call-delta | usage | finish
    Index          int               `json:"index,omitempty"`
    BlockType      string            `json:"blockType,omitempty"` // text | reasoning | tool-call | image | tool-result
    Text           string            `json:"text,omitempty"`
    ArgumentsDelta string            `json:"argumentsDelta,omitempty"`
    ID             string            `json:"id,omitempty"`
    Name           string            `json:"name,omitempty"`
    Block          json.RawMessage   `json:"block,omitempty"`    // ← block-end 的整块,需要解 type/text
    Usage          *usageInfo        `json:"usage,omitempty"`
    Reason         *chunkFinishReason `json:"reason,omitempty"`
}
```

`contentBlockDTO`(`protocol.go:244-254`)现有的字段已经覆盖 `text` / `reasoning` / `tool-call` / `tool-result` 的判别(`Type string` + `Text string`),不需要再扩。

#### 14.4.2 `translate.go` — 引入独立的 `reasoningBuf`,与 dashboard 的 PartialAccumulator 对齐

```go
type translator struct {
    // ... 既有字段 ...

    // F-DSH-DASHBOARD-PARITY: reasoning blocks get their OWN accumulator,
    // not the text one. Mixing them is the root bug — reasoning text
    // ends up in textBuf and surfaces as reply instead of thinking.
    reasoningBuf map[int]*strings.Builder  // blockIndex → builder

    // Track which blockIndexes we've emitted via reasoningBuf, so we
    // don't double-emit at block-end (reasoning-delta already queued
    // the text, block-end re-emits the assembled block — pick one).
    reasoningEmitted map[int]bool
}
```

`reasoningBuf` 与 `textBuf` 一样的指针-map 设计(避免 `strings.Builder` 的 noCopy 陷阱,见 `translate.go:43-61` 的同款注释)。

#### 14.4.3 `dispatch.go` — 改 `handleAssistantChunk` / `handleAssistantMessage` 两个 handler

`handleAssistantChunk` 按 `Chunk.Type` 分流,与 dashboard 的 `PartialAccumulator.push` 一一对应:

```go
func handleAssistantChunk(env sessionEventEnvelope, view json.RawMessage, tr *translator, st *wireState, d *driver) []agent.AgentEvent {
    var data assistantChunkData
    if err := json.Unmarshal(env.Data, &data); err != nil {
        dLog("dsh: handler data envelope decode: %v", err)
        return nil
    }
    tr.active = true

    switch data.Chunk.Type {
    case "text-delta":
        // 实时流式 reply —— 与 dashboard 行为一致
        // 但要走 F-52 颗粒度合约:不每 delta 发 EventAgentText(怕双发),
        // 而是把 text 累积进 pendingText,在 assistant/message / tool/call /
        // turn/end 三个边界 flush(沿用既有路径)。
        idx := data.Chunk.Index
        if idx < 0 || idx >= maxTextStreams { return nil }
        b, ok := tr.textBuf[idx]
        if !ok { b = &strings.Builder{}; tr.textBuf[idx] = b }
        b.Grow(256)
        b.WriteString(data.Chunk.Text)
        return nil

    case "reasoning-delta":
        // dashboard 的 thinking 路径:拼到 reasoningBuf,不进 textBuf
        idx := data.Chunk.Index
        if idx < 0 || idx >= maxTextStreams { return nil }
        b, ok := tr.reasoningBuf[idx]
        if !ok { b = &strings.Builder{}; tr.reasoningBuf[idx] = b }
        b.WriteString(data.Chunk.Text)
        return nil  // reasoning 实时逐 delta 发 OutThinking 会切碎视觉,
                    // 跟 dashboard 的 "折叠" 行为对齐,等 block-end 整段发

    case "block-end":
        var blk struct {
            Type string `json:"type"`
            Text string `json:"text,omitempty"`
        }
        _ = json.Unmarshal(data.Chunk.Block, &blk)
        if blk.Type == "reasoning" && blk.Text != "" {
            idx := data.Chunk.Index
            // 整段 thinking 上屏 —— 走已存在的 [思考] 前缀约定,
            // gateway/outbound/translate.go:58-63 自动转 OutThinking。
            return []agent.AgentEvent{{
                Kind: agent.EventAgentText,
                Text: "[思考] " + blk.Text,
            }}
        }
        // block-end for text: 已在 text-delta 累积,这里不动;
        // block-end for tool-call: 已在 tool/call 独立路径处理;
        // block-end for other: 忽略
        return nil

    case "block-start", "tool-call-delta", "usage", "finish":
        // dashboard 也只把它们喂给 PartialAccumulator,不直接渲染;
        // 我们同等丢弃(usage 已在 assistant/message 的 Usage 字段透传)
        return nil
    }
    return nil
}
```

`handleAssistantMessage` 把 `content[]` 按块类型拆开,与 dashboard 的 `toAssistantBlocks` 对齐:

```go
func handleAssistantMessage(env sessionEventEnvelope, view json.RawMessage, tr *translator, st *wireState, d *driver) []agent.AgentEvent {
    var data assistantMessageData
    if err := json.Unmarshal(env.Data, &data); err != nil {
        dLog("dsh: handler data envelope decode: %v", err)
        return nil
    }
    tr.active = true
    tr.lastUsage = data.Usage

    var evs []agent.AgentEvent
    for _, b := range data.Message.Content {
        switch b.Type {
        case "text":
            // dashboard: 把 finalized text 当作一次块 flush,走 reply
            if b.Text != "" {
                evs = append(evs, agent.AgentEvent{Kind: agent.EventAgentText, Text: b.Text})
            }
        case "reasoning":
            // dashboard: 整段折叠的 thinking,走 [思考] 约定
            if b.Text != "" {
                evs = append(evs, agent.AgentEvent{Kind: agent.EventAgentText, Text: "[思考] " + b.Text})
            }
        case "tool-call":
            // dashboard 在消息层就显示 tool-call,但我们已经有独立的 tool/call
            // 事件路径(handleToolCall),这里不再发,避免双发
        case "image", "tool-result":
            // 暂不渲染(image 由 dsh assistant 端 baseline-only 不产出;
            // tool-result 不应在 assistant message content[] 里出现,
            // 它走独立的 tool/result 路径)
        }
    }
    return evs
}
```

**取舍说明 — 关于 `text-delta` 不实时逐 delta 发 `EventAgentText`**:
- 选项 A(完全 dashboard parity):每个 text-delta 立刻发 `EventAgentText`,接受更多卡片更新。
- 选项 B(沿用 F-52 颗粒度):text-delta 仍走 `textBuf` 累积,在 `assistant/message` / `tool/call` / `turn/end` 三个边界 flush,只新增 thinking 路径。

**建议先做 B**,原因:
1. 用户的核心痛点是 **think 完全没分类**(被串进 reply),reply 文本会通过 `EventAgentResult.Text` 在 `turn/end` 时一次性出现,**不是丢了,是延迟了**;
2. 选项 B 改动最小,F-52 的 `textDelivered` / `active` / `pendingText` 守卫全部复用,不需要重新测 feishu 接收侧防双发;
3. 选项 A 要重测飞书接收侧(可能触发 reply 卡片更新风暴),单独 PR 更安全;
4. 若实测发现 reply 延迟感严重影响 UX,再开 A 方案的 PR。

**关键防双发不变量**:
- `reasoning-delta` 只写 `reasoningBuf`,不 emit
- `block-end{type:"reasoning"}` 整段 emit 一条 `[思考] ...`
- `assistant/message.content[type:"reasoning"]` 也 emit 一条 `[思考] ...`
- 这两条路径可能在同一个 block 上都触发(reasoning-delta 累积了 N 条,然后 block-end 再来一次,然后 assistant/message 的 content[] 又来一次)→ 可能看到三段 thinking

**对策**:用 `reasoningEmitted map[int]bool` 记录每个 blockIndex 是否已经发过;发过的就跳过下一次。或者更简单:`block-end{type:"reasoning"}` 触发时,把该 idx 在 `reasoningBuf` 里的内容 emit 完后清掉 `reasoningBuf[idx]` 并打标;`assistant/message.content[type:"reasoning"]` 路径只 emit 该 blockIndex 上 `reasoningEmitted[idx] == false` 的块。

#### 14.4.4 `state.go` / `handle_mux.go` — 不动

`applyProjection` / `applyTodoProjectionLocked` 与 mux stream 入口都不需要改——问题完全在 `session/event` 的 dispatcher 一层(`handleAssistantChunk` / `handleAssistantMessage`)。

#### 14.4.5 回归测试

`internal/bridge/dsh/dispatch_test.go` 已覆盖 11 种事件类型,新增三个 fixture 反向锁住 dashboard parity 合约:

| Fixture 文件 | 断言 |
|---|---|
| `testdata/envelopes/assistant_chunk_text_delta.json` | `Chunk.Type=="text-delta"`,`BlockType=="text"`,`Text=="hello"` → handler 写入 `textBuf[0]`,**不**写 `reasoningBuf`,**不** emit 任何 `AgentEvent` |
| `testdata/envelopes/assistant_chunk_reasoning_delta_then_block_end.json` | 先发 `reasoning-delta{Text:"让我想想"}`,再发 `block-end{Block:{type:"reasoning", text:"让我想想"}}` → 第一帧写入 `reasoningBuf[0]` 不 emit;第二帧 emit 一条 `EventAgentText{Text:"[思考] 让我想想"}`,**不**写 `textBuf` |
| `testdata/envelopes/assistant_message_mixed_content.json` | `content=[{type:"reasoning",text:"A"},{type:"text",text:"B"},{type:"tool-call",id:"x"}]` → emit 一条 `[思考] A` + 一条 `B`;tool-call 不发(走独立 tool/call 路径) |

### 14.5 验证清单(改完跑一遍)

```bash
# 1) 协议反向锁死
go test ./internal/bridge/dsh/ -run 'TestHandleAssistant' -count=1

# 2) 全库回归(确保没有破坏既有 11 种事件的处理)
go test ./internal/bridge/dsh/... -race -count=1

# 3) 实机对比验证:启动 dsh web,跑 dashboard,跑 nightme 同一个 sessionId,
#    在 dashboard 看得到 think/reply 的 turn,grep nightme logs:
bin/nightme logs --once -n 200 | grep -E "dsh: assistant/chunk|EventAgentText|OutThinking|OutReply"
# 期望:
#   - 文本 turn 多条 EventAgentText(对应 reply,经 OutReply 出)
#   - 任一含 reasoning 的 turn 至少一条 [思考] ... → OutThinking 出
#   - 任一含 tool 的 turn 多对 EventAgentToolStart/EventAgentToolEnd(对应 tool)
```

### 14.6 一句话总结

nightme 漏 think + reply 实时流的根因,完全集中在 `internal/bridge/dsh/dispatch.go::handleAssistantChunk` 与 `handleAssistantMessage` 两个 handler:`handleAssistantChunk` 把所有 `Chunk.Type` 一律当 text 写进 `textBuf` 而忽略 `BlockType="reasoning"`,`handleAssistantMessage` 用的 `pickText` 又只挑 `Type=="text"` 的 content block —— dashboard 看到的 thinking 与 streaming reply,在 nightme 这一层就被无声地吞掉或串进 reply。按 dashboard 的 `PartialAccumulator(blockType)` 拆成 `textBuf` + `reasoningBuf` 双缓冲,在 `block-end{type:"reasoning"}` 与 `assistant/message.content[type="reasoning"]` 两个边界把整段 thinking 用 nightme 已有的 `[思考] ` 前缀转 `EventAgentText` 就能开箱即用 —— `messages.OutThinking` 与 gateway translate 的转换管道都已经在那等着,无需改 EventKind 或上层。

---

## 15. RunOnce / Review 迁移到 shared host(2026-08-22)

### 15.1 迁移动机

2026-08-22 之前的架构:`Starter.RunOnce` / `Starter.Review` 用 `--profile headless` 子进程路径(`dsh/print.go` 的 `runPrintMode`),`Starter.Start` 走 `--profile web` shared host。两端分裂,且 headless 路径存在两个**结构性**问题:

1. **R2 上下文隔离不可控**:`dsh --profile headless` 没有 sessionId 概念 —— 它是"Answer one task and exit"的一次性进程,具体行为依赖 dsh CLI 实现是否从共享状态(`~/.dsh/` 任何文件)读取上下文。如果 dsh 默认从某处读上次 session 的 context,headless RunOnce / Review 就会**偷看到主 chat 的对话历史**,污染 `/gtw commit` 的 commit prompt、`/review` 的 review 输出。web 路径通过 `POST /api/session.create {cwd}` 显式开新 sessionId,**没有这种泄漏路径**。

2. **每次冷启动 ~1-3s**:`proc.New("dsh", "--profile", "headless", ...)` 每次 RunOnce 都要 Node.js 冷启动 + 配置重读 + 模型 client 重连。web 路径的 `session.create` 是 HTTP RPC 给已运行的 dsh web daemon,**单次成本 ~50-200ms**(`/gtw commit` 高频调用场景下省一个数量级)。

3. **headless 没有结构化事件**:`dsh/print.go:5-30` 明确说 "no structured events, no NDJSON"。headless 路径要支持 R3(实时 sink 投递)需单独写一份 wire parser;web 路径的 mux demux 已经吐结构化 JSON,**直接复用**(这是本次 PR 选 web 路径的第三个理由)。

### 15.2 迁移方案

`Starter.RunOnce` 不引入任何新的子进程管理代码,**严格等于**:

```
RunOnce := s.Start(ctx, cfg) + SendBlocks + drain → RunResult + defer a.Close()
```

`Review` 直接调 `RunOnce`,prompt 改成 `agent.StandardPrompt()`。

`defer a.Close()` 即 R4 归档 —— 现有 `driver.Close()`(`dsh/session.go:916-955`)已经按顺序做:

1. `Router.Unsubscribe(sessionId)` —— 共享 host mux 停止路由该 sessionId 的帧
2. `session.cancel` —— best-effort,benign if idle
3. `workspace.archiveSession` —— 把该 session 从 dsh web 的"left list"分组移除,**但保留 session log 和 workspace accounting slot**
4. close events chan

**关键推论**:本次改动**无新增归档代码**,`Close` 已经做完了。

### 15.3 实现要点

**`dsh/starter.go::RunOnce`**(新):
```go
func (s *Starter) RunOnce(ctx context.Context, cfg agent.StartConfig,
    blocks []agent.ContentBlock) (agent.RunResult, error) {
    a, err := s.Start(ctx, cfg)
    if err != nil {
        return agent.RunResult{}, fmt.Errorf("agent %s: spawn: %w", s.Info().Name, err)
    }
    defer a.Close() // ★ R4:driver.Close 已完成 workspace.archiveSession
    return drainForRunResult(ctx, a, blocks)
}
```

**`dsh/starter.go::Review`**(新):
```go
func (s *Starter) Review(ctx context.Context, cfg agent.StartConfig) (agent.RunResult, error) {
    return s.RunOnce(ctx, cfg, []agent.ContentBlock{{
        Type: agent.ContentText,
        Text: agent.StandardPrompt(),
    }})
}
```

**`drainForRunResult`**(`dsh/starter.go` 同文件):镜像 `acp/starter.go::collectResult` 的形态,从 `live.Events()` drain 出 `RunResult`,跟踪 `EventAgentReady.SessionID/Model`、`EventAgentResult.Text/Usage/DurationMs/Subtype`,遇到 `EventAgentDone` / `EventAgentError` 报错退出。

### 15.4 文件改动

| 文件 | 改动 | 净行数 |
|------|------|--------|
| `dsh/print.go` | **删除** | -227 |
| `dsh/print_real_unix_test.go` | **删除** | -76 |
| `dsh/starter.go` | `RunOnce` / `Review` 重写,新增 `drainForRunResult` / `auditFields` | +80 |
| `dsh/starter_test.go`(mock) | 新增 5 个测试:`TestStarter_Info_NoArgs` / `TestDrainForRunResult_EventAgentResult` / `TestDrainForRunResult_DoneWithoutResult` / `TestDrainForRunResult_ErrorEvent` / `TestRunOnce_StripsSessionID` / `TestRunOnce_ArchiveOnClose` | +200 |
| `dsh/runonce_real_unix_test.go`(真机 e2e) | 新增 2 个测试:`TestE2E_RunOnce_RealDSH` / `TestE2E_Review_RealDSH`(门控 `NIGHTME_REAL_DSH=1` + dsh on PATH) | +170 |
| `dsh/doc.go` | 删 "Print-mode" 段,改写包 doc;删 `Starter.ListSessions` 悬挂引用 | -15 |

**净:约 +12 行**(主要是测试代码)。

**测试覆盖分两层**:

- **Mock 层**(`starter_test.go`,无需真 dsh):覆盖 `Starter.Info` 契约、`drainForRunResult` 三个终态分支(`EventAgentResult` / `EventAgentDone` / `EventAgentError`)、`cfg.SessionID` 在 RunOnce 上被 strip、`defer a.Close()` 触发 `workspace.archiveSession`。
- **真机 e2e 层**(`runonce_real_unix_test.go`,需 `NIGHTME_REAL_DSH=1` + 本机装 dsh):覆盖端到端流程 —— 真 dsh web lazy spawn + 握手 + session.prompt + minimax-cn 模型响应 + session 归档。

`TestRunOnce_IsolatedSessions` 和 `TestReview_UsesStandardPrompt` 没在 mock 里:连续 `RunOnce` 调用在 mock 上有 state-pollution race(`session.create` 跟第一次 Close 之间的窗口),但真 dsh 进程独立 sessionId 互不干扰,所以 e2e 层覆盖了这两个语义。

### 15.5 R3 事件流化(sink) — ✅ 已落地

**接口扩展**:`agent.Starter.RunOnce / Review` 加 variadic `opts ...agent.RunOnceOption`(向后兼容,已有 caller 零修改)。目前只暴露一个选项:

```go
// internal/agent/agent.go
func WithEventSink(sink func(AgentEvent)) RunOnceOption
```

**Bridge 接入**:7 个 bridge(`claudecode / codex / pi / opencode / cursor / acp / pty / dsh`)签名都加 `opts ...RunOnceOption`,行为不变(print-mode bridge 忽略 opts,dsh 把 sink 解析出来传给 drain)。

**dsh drain 投递**:`drainForRunResult` 在事件循环开头调 `deliverToSink(ev)`,所以**sink 收到所有 event**(Ready / Text / ToolStart / ToolEnd / Permission / TaskCreate / TaskUpdate / Result / Done / Error)。但 **Done 不会到 sink** —— drain 在 Result 时已 return,Done 是 driver.Close 在 events chan 关闭后发的。

**Caller 接线**:`internal/gateway/outbound/emitter_sink.go::StreamRunOnceToEmitter` 是 canonical pattern —— buffered chan (cap=64) + drain goroutine,保证 bridge 不被 Feishu 等 channel 限速拖垮。`/gtw commit`、`/gtw pr`、`/review` 调用点都接上了 sink。

**Permission 语义**:one-shot / review 用 full-access 权限模式,**Permission.ResponseCh 不经 sink 路由** —— sink 是 observability,decision 走 runtime 已有路径。如果未来 Permission 真的在 one-shot 里 fire 了,sink 会看到但 bridge 不会卡住等响应。

**测试覆盖**:
- `TestDrainForRunResult_SinkReceivesEvents` (mock):sink 收齐 Text / ToolStart / ToolEnd / Result
- `TestDrainForRunResult_NilSinkSafe` (mock):nil sink 不 panic
- `TestE2E_RunOnce_Sink_RealDSH` (真机):真 dsh 上 sink 收到 Ready + Result
- gtw/review 调用点接 sink 由 compile-time 检查保证(类型签名强制)

### 15.6 验证清单

#### 自动化(必跑,本地 + CI)

- [ ] `go build ./...` 通过
- [ ] `go vet ./...` 通过
- [ ] `go test ./internal/bridge/dsh/ -count=1 -short` 全绿

#### Mock 测试清单(`starter_test.go`,无需真 dsh)

- [ ] `TestStarter_Info_NoArgs`:`Starter.Info().Args` 是 `nil`(Starter 不再直接 spawn)
- [ ] `TestDrainForRunResult_EventAgentResult`:`EventAgentResult` 触发 drain 返回,带 SessionID/Model/Text/Usage
- [ ] `TestDrainForRunResult_DoneWithoutResult`:`EventAgentDone` 无前导 Result → error with audit fields
- [ ] `TestDrainForRunResult_ErrorEvent`:`EventAgentError` 触发 drain 错误返回
- [ ] `TestRunOnce_StripsSessionID`:`cfg.SessionID` 在 RunOnce 路径被 strip,永远 fresh session
- [ ] `TestRunOnce_ArchiveOnClose`:`defer a.Close()` 触发 `workspace.archiveSession`(R4)
- [ ] `TestDrainForRunResult_SinkReceivesEvents`:sink 收到 Text / ToolStart / ToolEnd / Result(每种 kind 一次)
- [ ] `TestDrainForRunResult_NilSinkSafe`:nil sink 不 panic

#### 真机 e2e 测试清单(`runonce_real_unix_test.go`,需 `NIGHTME_REAL_DSH=1` + 本机装 dsh)

- [ ] `TestE2E_RunOnce_RealDSH`:真 dsh web lazy spawn + 握手 + session.prompt + minimax-cn 模型响应 + archive 全链路
- [ ] `TestE2E_Review_RealDSH`:`Starter.Review` 端到端(用 `agent.StandardPrompt()` 作 prompt)
- [ ] `TestE2E_RunOnce_Sink_RealDSH`:真 dsh 上 sink 收到 Ready + Result

#### 真机手动验证(可选,需要真实 feishu/telegram channel)

- [ ] `/gtw commit` 跑通:feishu chat **实时**看到 thinking / tool call / result(中间过程,不是只有最终 card),日志 `dsh: session archived`
- [ ] `/gtw pr` 跑通,同上
- [ ] `/review` 跑通,中间过程实时可见,review text 完整,日志同上
- [ ] 打开 dsh web dashboard,确认 left list **没有** RunOnce 跑完后的残留 session

### 15.7 行为变化(用户可见)

| 维度 | 之前(headless) | 现在(web shared host) |
|------|----------------|------------------------|
| 启动开销 | 每次 1-3s cold start | lazy 一次 + 后续 50-200ms/次 |
| 上下文隔离 | **不可控**(可能从共享状态读到主 chat 上下文) | **显式隔离**(`session.create` 新 sessionId) |
| 跨 session 串扰 | 风险:headless 偷看主 chat 上下文 | 隔离保证 |
| 归档 | 不需要(进程即清) | `workspace.archiveSession` |
| 多 session 并发 | N/A(进程独立) | ✅ 共享 dsh web,native 支持 n 个 session |
| 日志 | 进程级 stdout | 增加 `dsh: session archived` 行 |
| 测试模式 | `print_real_unix_test.go` 跑 mock script | `session_real_unix_test.go` 跑 mock dsh web |
| **用户自启 dsh web** | 不复用(headless 跑自己的) | **nightme 自动 attach**(`EnsureSharedHost` 的 reuse-or-spawn 命中 `DiscoverExisting`);用户原本开的 dashboard 与 nightme session 共享 mux 通道 — 可观测但不影响 nightme 行为 |

### 15.8 后续 PR 路线

| PR | 范围 | 改动 |
|----|------|------|
| **本 PR(2026-08-22)** | dsh RunOnce 路径切换 + R3 sink | 删 `print.go` + 改 `starter.go` + 加 `RunOnceOption` + sink 接线 + 测试 |
| (无) | — | R3 sink 已在本次 PR 完成,无需后续 |

每个 PR 独立可 ship、独立可回滚。**架构先行,事件流化随后**。
