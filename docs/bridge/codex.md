# Codex App-Server Bridge — 集成方案

> **Status**: 实现已落地(M2);活文档 — 实测 + 约定
> **Scope**: `internal/bridge/codexserver/*` — nightme 侧的 codex CLI 适配器
> **Related docs**:
> - [docs/bridge/claude.md](./claude.md) — 姊妹文档(EventDone 不变量、生产踩坑)
> - [docs/bridge/pi.md](./pi.md) — JSON-IO 模板 + F-52 粒度契约
> - [docs/feat/F-21-agent-modes.md §6](../feat/F-21-agent-modes.md) — 注册表(已对齐)
> - [docs/feat/F-32-pi-rpc-bridge.md](../feat/F-32-pi-rpc-bridge.md) — clone + 模板+live 模式
> - [docs/feat/F-52-pi-stream-aggregation.md](../feat/F-52-pi-stream-aggregation.md) — flush-at-tool-boundary
> - [docs/feat/F-34-new-slash-command.md](../feat/F-34-new-slash-command.md) — `/new` 在长生命周期 bridge 上
> - [docs/feat/F-55-footer-show-context-window.md](../feat/F-55-footer-show-context-window.md) — Usage.ContextWindow 语义
> **设计稿**: [wip/codex.md](../../wip/codex.md) — 调研 + 决策记录

---

## 1. 传输层选型

nightme 通过 **codex CLI 自带的 `app-server` 子命令**直连 JSON-RPC 2.0:

```text
nightme ──stdio JSON-RPC 2.0──> codex app-server --listen stdio:// [-c model=…] [-c model_reasoning_effort=…]
```

每行一条 JSON-RPC 消息(`\n` 分隔),最大帧 10 MiB。三种帧:

| 帧 | 形状 |
|---|---|
| request | `{"jsonrpc":"2.0","id":<n>,"method":<name>,"params":<obj>}` |
| response | `{"jsonrpc":"2.0","id":<n>,"result":<obj>}` 或 `"error":{...}` |
| notification | `{"jsonrpc":"2.0","method":<name>,"params":<obj>}`(无 id) |
| server request | `{"jsonrpc":"2.0","id":<n>,"method":<name>,"params":<obj>}`(需回包) |

### 1.1 为什么不走 ACP 中间层

历史上有 `@agentclientprotocol/codex-acp`(npm),它内部就是 spawn `codex app-server`。再套一层 Node shim 没有价值——既增加启动延迟,又让 codex 私有协议独有的能力(流式 item/started vs item/completed 边界、turn/completed usage)被 ACP 协议压扁。nightme 直连 app-server。

### 1.2 为什么不支持 `codex exec` 后端

cc-connect 在 `codex.go::StartSession`(L466–504)用 `backend == "app_server" ? newAppServerSession : newCodexSession` 二选一,**默认 `exec`**。nightme **不引入 backend 双选**:

| 维度 | nightme 立场 | `exec` 兼容性 | `app_server` 兼容性 |
|---|---|---|---|
| 进程模型 | 一进程多 turn(F-32 §2.3) | ❌ 每条 prompt 重启 | ✅ |
| 流式事件 | F-52 粒度契约 | ❌ exec 不发增量 | ✅ |
| 审批 | `EventAgentPermission` + 5min 超时 | ❌ exec 无审批 IPC | ✅ 4 类 server-request |
| `/new` | 原进程 reset(F-34 §3.2.1) | ❌ exec 只能 resume 重启 | ✅ `thread/start` 同 transport |
| 持久化 | `thread/resume {persistExtendedHistory:true}` | ❌ exec 无 thread 概念 | ✅ |
| Usage snapshot | `turn/completed.usage` 一次性填 | ⚠️ exec 不报 | ✅ |

保留 `exec` 等于让 nightme 核心契约在 codex 上消失。

**逃生路径**:用户临时降级 `codex` 命令 → 自动落 `ModePTY` 兜底(`cmd/nightme/buildAgentRegistry` 路径),不会失能。

### 1.3 与 cc-connect 的差异

| | cc-connect | nightme |
|---|---|---|
| 后端选择 | `exec` (默认) / `app_server` 二选一 | 单选 `app_server` |
| Provider config | `codex config.toml [model_providers.*]` + `auth.json` | 不做(本期) |
| optOut 列表 | 6 条 | 同 6 条(`initialize.capabilities.optOutNotificationMethods` 必须全发,缺一条 double-consume) |

---

## 2. 生命周期

```
进程: newSession() ──> initialize ──> initialized ──> thread/start ──> ... ──> cmd.Wait() ──> close(events)
轮次: turn/start ──> item/started* ──> item/completed* ──> turn/completed ──> EventDone{Reason:"settled"}
```

一个进程承载多个 turn。**`EventAgentDone` 不关闭 events channel**,只有进程退出或 `Close()` 才关——`ChatSession.runReadPump` 依赖这点跨 turn 持续读取。

| 阶段 | 超时 | 说明 |
|---|---|---|
| initialize 握手 | 10s | 冷启动含模型预热 |
| `Close()` SIGINT→SIGKILL | 2s | |
| Close 等待 reap | 5s | 超时也返回,避免 wedge 住 runtime |
| 审批等待 | 5min → decline | `permissionTimeout` 包级 var,测试可压缩 |

`/new` → `ensureThread("")` 触发新的 `thread/start`(同 transport,不重启进程),后重新 emit `EventAgentReady`。

### 2.1 必须先发送的 `initialized` notify

JSON-RPC 2.0 握手是三步:

1. `initialize {clientInfo, capabilities}` → 等待响应
2. **收到响应后** → `initialized` notify(无 params,无 id)
3. `thread/start` 或 `thread/resume`

第三步之前不发送 `initialized` 是 Codex app-server 拒绝后续请求的常见原因。nightme 的 `newSession` 串行做这三步。

---

## 3. 事件整合(F-52 镜像)

### 3.1 翻译表

| 通知 | 处理 | 产出 AgentEvent |
|---|---|---|
| `thread/started` | set threadId(model/threadId 已在 thread/start 响应里) | — (EventAgentReady 在 handshake 后早 emit) |
| `turn/started` | `turnState.reset()`,`active=true` 由首个 item handler 决定(避免空 turn 误 emit) | — |
| `item/started` `agentMessage` | 不缓存(避免重复) | — |
| `item/started` `reasoning` | thinkBuf 累加 | — |
| `item/started` tool | **先 flush `pendingMsgs` 为 Text**,再 `EventAgentToolStart` | N × Text + 1 × ToolStart |
| `item/completed` `agentMessage` | append `pendingMsgs` | — |
| `item/completed` `reasoning` | emit `EventAgentText{"[思考] " + thinkBuf}` | 1 × Text |
| `item/completed` tool | emit `EventAgentToolEnd{ID, Name, Output, Err (failed)}` | 1 × ToolEnd |
| `item/completed` `contextCompaction` | emit `EventAgentText{"[context 已压缩]"}`;**不清 pendingMsgs**(F-49 移除) | 1 × Text |
| `turn/completed` | flush pending → `EventAgentResult{Text, Usage}` + `EventAgentDone{Reason:"settled", Usage}` | N × Text + 1 × Result + 1 × Done |
| `turn/failed` | emit `EventAgentResult{Text, Err}` + `EventAgentDone{Reason:"failed"}` | 1 × Result + 1 × Done |
| `thread/status/changed.idle`(codex ≥0.125) | 幂等去重:仅当 `active && doneEmitted==false` 触发 turn-end | (条件 N × Text + Result + Done) |
| `account/rateLimits/updated` | debug 记录,不上抛(nightme 无 quota UI) | — |
| `thread/tokenUsage/updated` | debug 记录,真 usage 走 `turn/completed.usage` | — |
| `error` | `EventAgentError{Err}` + stderr tail(末 2KB) | 1 × Error |

### 3.2 五个必须保留的守卫(对应 pi §3.4)

1. `pendingMsgs` 只在 tool 边界或 turn/completed flush,token 级不刷。
2. `EventAgentResult.Text` 永不为空,空回退到 `"Done."`(同 pi `emptyReplyFallback`)。
3. `EventAgentDone` 不关闭 events channel,只有 `cmd.Wait()` / `Close()` 关。
4. Usage = **OVERWRITTEN**:`turnState.lastUsage = usage`(赋值非 +=)。
5. `ContextWindow` / `ContextWindowPct`:codex `turn/completed.usage` 不报,**第一期 footer `(window)` 分母缺**(同 claudecode v0.2 早期)。

### 3.3 thread/status/changed 双信号幂等

`codex ≥0.125` 同时发 `turn/completed` 和 `thread/status/changed {type:"idle"}` 两个 turn-end 信号。Bridge 在 `turnState.doneEmitted` 守卫下,只第一个信号走 `completeTurn`,后续一律 short-circuit。

### 3.4 optOut 列表(必须全发)

`initialize.capabilities.optOutNotificationMethods` 必须包含全部 6 条:

```
command/exec/outputDelta
item/agentMessage/delta
item/plan/delta
item/fileChange/outputDelta
item/reasoning/summaryTextDelta
item/reasoning/textDelta
```

缺一条 → 同一 agentMessage 会收到 `delta` 增量 + `completed` 全量,bridge 重复消费。`initialize()` 单测覆盖此契约。

---

## 4. 审批流(server-initiated request)

### 4.1 4 类 server-request + 1 类工具

| method | Tool | 默认选项 |
|---|---|---|
| `item/commandExecution/requestApproval` | `Bash` | `["accept","decline"]` |
| `item/fileChange/requestApproval` | `Patch` | `["accept","decline"]` |
| `item/permissions/requestApproval` | `Permissions` | `["accept","decline"]` |
| `item/tool/requestUserInput` | `AskUserQuestion` | `["ok"]`(channel 回复 `<qid>:<labels>\|...`) |
| `item/tool/call` | (动态工具) | 返回 `success:false`,contentItems=`"tool not available in nightme bridge"` |

### 4.2 5 分钟超时 → decline

```go
timer := time.NewTimer(permissionTimeout)  // 5 * time.Minute
select {
case resp = <-ch:
case <-s.ctx.Done():
    resp = "decline"
case <-timer.C:
    resp = "decline"
}
```

`permissionTimeout` 是包级 `var`(非 `const`),tests in `permissions_test.go` 压缩到 200ms。

### 4.3 多 question AskUserQuestion 编码

为保持 `AgentPermissionRequest.ResponseCh` 单一字符串 channel,bridge 用 inline 编码:

**Action** 字段(channel 渲染):

```
<qid1>[ (multi)]: <header1> — <question1> [<label1> | <label2>]
<qid2>[ (multi)]: <header2> — <question2> [<label1> | <label2>]
```

**SendPermission resp** 格式(单字符串):

```
<qid1>:<label>[,<label>...][|<qid2>:<label>[,<label>...]]
```

`parseRequestUserInputResponse` 解码 → `requestUserInputResponseResult{Answers: {qid: {Answers: [labels]}}}`,缺失 qid 用该题第一项兜底,保证响应永远结构良好。

---

## 5. 已知的 codex 特性差异

| 项 | 状态 | 备注 |
|---|---|---|
| `ContextWindow` 报数 | ❌ 第一期不报 | footer `(window)` 分母缺,后续若 codex 加 `turnInfo.model.contextWindow` 字段再补 |
| task 子系统 | ❌ 不实现 | codex app-server 无 task 事件,`EventAgentTaskCreate/Update` 永不发 |
| 多模态图片 | ✅ | `ContentImage` → stage 到 `<workspace>/.nightme/codexserver/images/img_<ns>_<pid>.<ext>` + `localImage` |
| 多模态文件 | ⚠️ fallback | `ContentFile` 走 `@<path>` 行追加(无 file type) |
| 第三方 provider | ❌ 不实现 | `codex config.toml [model_providers.*]` / `auth.json` 留给 F-XX;本期直接 `-c model_provider=... -c openai_base_url=...` 透传 |
| 动态工具 `item/tool/call` | ❌ 返回 not available | 跟 cc-connect MVP 一致 |
| `account/rateLimits/updated` UI | ❌ 不暴露 | nightme 无 quota UI |
| 跨 turn threadId 持久化 | ✅ `cfg.SessionID` ↔ `threadId` | 见 §2.1 `/new` |

---

## 6. 可观测性

codexserver 调试日志默认 ON,`NIGHTME_CODEX_DEBUG=0` 关,日志带 `component=codexserver`。关键节点:

- `[codexserver] session started` (pid/workspace/resume/model/effort)
- `[codexserver] initialize ok` (userAgent)
- `[codexserver] session handshake complete` (threadId/model)
- `[codexserver] server request: unknown method` (回 `method not found`)
- `[codexserver] item/tool/call: returning tool not available` (动态工具降级)
- `codexserver: approval timed out, defaulting to decline` (5min 触底)

事件 channel 满(> 64 buffered)→ bridge 主动丢一条 + emit `EventAgentError{... dropped text}`。这是最后的反压兜底,正常情况下不应该发生。

---

## 7. 待定事项(本期不做)

| 项 | 理由 | 何时做 |
|---|---|---|
| `codex exec` 后端(双选) | 违反 F-32/F-52/F-34 多 turn 契约 | 永不 |
| ACP 中间层 | 多一层 Node shim,且其内部也是 spawn `codex app-server` | 永不 |
| 模型别名表 | nightme 直接 `-c model=…` 透传 | 永不 |
| 第三方 provider config.toml | 独立 PR | F-XX |
| `item/tool/call` 动态工具 | cc-connect 也只回 not available | 等 Codex 稳定再议 |
| `account/rateLimits/updated` UI | nightme 无 quota UI | 待有 quota UI 再说 |
| `internal/processkill` 抽提 | CC-connect proc_unix/proc_windows 端口 | 单独 PR |
| `internal/redactdiag` stderr 红线 | openclaw-lark 模式 | 单独 PR |

---

## 8. 关联文件 & 调用路径速查

```text
cmd/nightme/agents.go
  └─ agent.Builtins.Register(codexserver.New("codex", "codex", nil))

internal/bridge/codexserver/
  ├── protocol.go   wire 类型(initialize/thread.start/turn.start/permission)
  ├── rpc.go        JSON-RPC 2.0 client(writeMu/pendingMu/failPending/readPump)
  ├── session.go    spawn + I/O + lifecycle + ringBuffer + detectBranch
  ├── agent.go      *Agent(模板+live)/Start/Events/Send*/SendPermission/New/Close
  ├── translate.go  envelope → AgentEvent,F-52 状态机(pendingMsgs + flush-at-tool-boundary)
  ├── permissions.go 4 类 server-request + 5min 超时
  ├── *_test.go     52 个单元测试(rpc/translate/permissions/session/agent)
  └── session_real_test.go  requireRealCodex e2e
```
