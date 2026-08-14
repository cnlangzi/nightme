# Codex App-Server Bridge — 集成方案

> **Status**: 已落地 M2 — 本文档是**活文档**,包含实测踩坑 + 实战经验
> **Scope**: `internal/bridge/codex/` — nightme 侧的 codex CLI 适配器
> **设计稿 / 调研**: [wip/codex.md](../../wip/codex.md)
> **姊妹文档**:
> - [docs/bridge/claude.md](./claude.md) — EventDone 不变量、生产踩坑
> - [docs/bridge/pi.md](./pi.md) — JSON-IO 模板 + F-52 粒度契约
> - [F-runtime §Agent 注册表](../feat/F-runtime.md) — 注册表
> - [F-32](../bridge/pi.md) — clone + 模板+live 模式
> - [F-52](../bridge/pi.md) — flush-at-tool-boundary
> - [F-34](F-chat-session.md) — `/new` 在长生命周期 bridge 上

---

## 1. 设计基线(已锁定,不再动摇)

### 1.1 传输选型

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

### 1.2 三不做

| 不做 | 理由 |
|---|---|
| **不走 ACP 中间层** | `@agentclientprotocol/codex-acp` 内部也是 spawn `codex app-server`,加一层 Node shim 只增加启动延迟 + 压平流式事件(`item/started` vs `item/completed` 边界、`turn/completed.usage` 被 ACP 抹掉)。 |
| **chat-session 单选 `app_server`(`exec` 不做 chat 后端)** | cc-connect 在 `codex.go::StartSession` L466–504 用了二选一,默认 `exec`。我们走单选 `app_server`,因为 `exec` 与 nightme 的多 turn + 流式事件 + 审批 IPC 三条核心契约全部不兼容。**`exec` 不是 chat 后端,但 `Starter.RunOnce` 的 one-shot 路径用它**(F-CODEX-PRINT-001,见 §1.4);chat 与 print 各自独立,不在运行时互斥。**用户临时降级 `codex` 命令 → 自动落 `ModePTY` 兜底**(`cmd/nightme/buildAgentRegistry`),不会失能。 |
| **不做模型别名表** | `-c model=…` 直接透传,由用户在 `StartConfig.Args` / chat args 写明。 |

### 1.3 与 cc-connect 的差异速查

| | cc-connect | nightme |
|---|---|---|
| 后端选择 | `exec` (默认) / `app_server` 二选一 | chat = 单选 `app_server`;print-mode = `exec`(见 §1.4)。两个用途独立,不运行时互斥 |
| provider config | `codex config.toml [model_providers.*]` + `auth.json` | 不做(本期直接 `-c model_provider=… -c openai_base_url=…` 透传) |
| `Mode` | `backend == "app_server" ? ModeJSONIO : ModePTY`(运行时分支) | `ModeJSONIO` 单一值,与 pi / claudecode 对齐 |
| 多模态图片路径 | `codex/images/img_<ns>_<pid>.<ext>` | 同上(在 `<workspace>/.nightme/codex/images/` 下,仅 app-server 路径用) |
| optOut 列表 | 6 条 | 同 6 条,**必须全发** —— 缺一条 double-consume |
| print-mode 入口 | 复用 `exec` 作为 chat 后端 | chat 用 app-server,print 用 `exec --json -o <tmpfile>`(F-CODEX-PRINT-001)|

---

### 1.4 Print Mode(一次性调用)

`Starter.RunOnce`(`/gtw commit` / `/gtw pr` / `buildAgentPrompt` 走这里)走 **`codex exec` 子命令**,不走 §1.1 的 app-server。`app-server` 是 chat session 的多 turn 路径;`exec` 是 codex CLI 自带的 one-shot 入口。两者不互斥——同一份 codex binary 装好就都可用。

#### 1.4.1 为什么不用 app-server 做一次性

旧实现是 `Start + defer Close + agent.RunOnceDrain`,跟 acp / opencode 对齐。但对一次性调用有两个浪费:
*(F-RUNONCEDRAIN-INTERNAL 后 `agent.RunOnceDrain` 已被删除,acp 的对应逻辑内联进 `(*acp.Starter).collectResult`,codex 走 print-mode 不再涉及)*

1. **5s `closeDrainTimeout` 上限**(`session.go:53`)。app-server 没有"做完一个 turn 就退出"的协议 flag,一次性场景等满 5s 是浪费。
2. **握手 + pump goroutine 开销**。`newSession` 起 4 个 goroutine(`readPump` + `stderrLoop` + `lifecycle` + `rpc`,见 §2)就跑一个 turn,全部拆掉——性价比为零。

claudecode/pi 已经先一步切到 `xxx -p` print-mode(F-CLAUDE-PRINT-001 / F-PI-PRINT-001),codex 这边走同一思路,详见 `docs/feat/F-CODEX-PRINT-001.md` / `internal/bridge/codex/print.go`。

#### 1.4.2 argv 布局

实测 `codex-cli 0.145.0` 后确定(`print_internal_test.go::TestBuildPrintArgs_*` 锁定):

```text
codex exec \
  --dangerously-bypass-approvals-and-sandbox \      # = app-server 的 approval_policy="never" + sandbox_mode="danger-full-access"
  -C <workspace> \                                    # = cmd.Dir / StartConfig.Workspace
  --skip-git-repo-check \                             # /gtw commit 可能在非 git 工作树跑
  [-i <img1>] [-i <img2>] ... \                       # ContentImage → -i flag(repeatable,实测有效)
  --json \                                            # NDJSON 事件流到 stdout
  -o <tmpfile> \                                      # final agent_message 写到文件(无 progress 噪声)
  -- \                                                # ← 必须:分隔 flag 与 positional prompt
  <prompt>                                            # ContentText + ContentFile 合成
```

**关键**:`--` 不可省。codex 0.145 在 `-i` 与 positional prompt 共存时若没有 `--` 会把 prompt 当 stdin 读(实测 bug,`print_internal_test.go::TestBuildPrintArgs_AllImagesFallsBackToSentinel` 间接验证)。

#### 1.4.3 RunResult 字段映射

| RunResult 字段 | 来源 | 备注 |
|---|---|---|
| `Text` | `-o <tmpfile>` 内容 | 干净;不含 user/codex 标记、不含 tool_call progress |
| `SessionID` | `thread.started.thread_id`(NDJSON 第一条) | 与 app-server 的 `threadId` 同源语义 |
| `Usage` | `turn.completed.usage{input_tokens, cached_input_tokens, output_tokens}` | 与 app-server 的 `appServerUsageToUsageInfo` 同源语义,wire 字段名不同 |
| `Subtype` | exit code:0 = `"completed"`,非零 = `"failed"` | 与 app-server 的 turn status 语义一致 |
| `DurationMs` | 我们 wall-clock 测 | cmd.Wait 立即返回,不像 app-server 要等 5s 上限 |
| `Model` | 空 | 当前 `StartConfig` 没有 `Model` 字段(app-server 也一样);后续若加可由 `-m` flag 注入 |

实测 `codex-cli 0.145.0` 上跑通:

```
INFO [codex] PrintMode Start command=codex workspace=/tmp/foo prompt_bytes=85 args_count=10 pid=52274
INFO [codex] PrintMode Exit  pid=52274 elapsed_ms=9828 wait_err=<nil>
                                  session_id=019fff55-11bc-7283-b757-b8be07822c6a
result: Text="PONG"  Subtype=completed  DurationMs=9828
        SessionID=019fff55-...     Usage=InputTokens:17869 OutputTokens:22 CacheReadInputTokens:128
```

#### 1.4.4 Block 编码

| `ContentBlock` | 处理 |
|---|---|
| `ContentText{Text}` | `\n` 拼到 prompt 末尾(空 `Text` 跳过) |
| `ContentImage{Path}` | 追加 `-i <Path>` flag(空 `Path` 跳过;多张图 repeatable) |
| `ContentFile{Path}` | 追加 `@<Path>` 到 prompt(app-server 没 file type;exec 没有 file flag,降级为文本注解) |
| **空 blocks / 全 image** | sentinel `"(see attached content)"` 注入到 prompt——避免 codex exec 走 stdin 回退(0.145 实测 bug) |

注意:`ContentImage` 走 `-i` flag 比 claudecode print-mode 的 `[image: ...]` 文本注解更强——exec 原生支持多模态附件,image 真的进模型视觉。

#### 1.4.5 文件位置

| 文件 | 角色 |
|---|---|
| `internal/bridge/codex/print.go` | 实现:`runPrintMode` + `buildPrintArgs` + `runNDJSON` |
| `internal/bridge/codex/print_internal_test.go` | 7 个 argv 构造单测(不需 codex binary,CI 必跑) |
| `internal/bridge/codex/print_real_unix_test.go` | 4 个 e2e(`NIGHTME_REAL_CODEX=1` 才跑真 binary) |
| `internal/bridge/codex/starter.go` | `Starter.RunOnce` 1 行委托到 `runPrintMode` |

关联:`F-CODEX-PRINT-001`(2026-08-14)。

---

## 2. 生命周期

### 2.1 进程级状态机

```
newSession() ──> initialize ──> initialized ──> thread/start ──> …turns… ──> cmd.Wait() ──> close(events)
```

### 2.2 进程内事件流(每 turn)

```
turn/start ──> item/started* ──> item/completed* ──> turn/completed ──> EventDone{Reason:"settled"}
```

### 2.3 ⛓️ 关键不变量:`EventAgentDone ≠ close events`

跨多 turn 的长生命周期 bridge,`ChatSession.runReadPump` 依赖 events channel 在 turn 之间持续打开。**只有进程退出或 `Close()` 才关闭 events**。

实现上 lifecycle goroutine 是 `close(s.events)` 的**唯一**持有者;`Close()` 只发起关闭(关 stdin、cancel ctx),通过 `<-s.exitDone` 等 reap,绝不直接 close events。

### 2.4 握手三步(必读)

JSON-RPC 2.0 握手是三步,**任何一步错位都会让 codex app-server 拒绝后续请求**:

1. `initialize {clientInfo, capabilities}` → 等待 response
2. **收到 response 后** → `initialized` notify(无 params、无 id)
3. `thread/start` 或 `thread/resume`

`newSession` 串行做这三步,任一失败立即 `Close()` 释放资源再返回错误。

### 2.5 超时表

| 阶段 | 超时 | 说明 |
|---|---|---|
| initialize 握手 | 10s(`handshakeTimeout`) | 冷启动含模型预热 |
| `Close()` 等 reap | 5s(`closeDrainTimeout`) | 超时也返回,绝不 wedge runtime |
| 审批等待 | 5min(`permissionTimeout`,包级 var) | 超时 → decline |
| turn RPC 请求 | 跟随 `ctx` 生命周期 | 跟随 `SendBlocks` 调用的 ctx |

`/new` → `ensureThread("")` 触发新的 `thread/start`(同 transport、不重启进程),后重新 emit `EventAgentReady`。

### 2.6 ⛓️ 踩坑教训:`closeOnce.Do` 的死锁

**问题**:最初 `Close()` 把 `close(s.events)` 也放进 `closeOnce.Do`,等待 `<-s.exitDone` 也在 Once 内。lifecycle goroutine 里 `defer close(s.exitDone)` 之前需要先 `s.pumpWG.Wait()`,而 pump 退出依赖 `lifecycle` 关闭 events — 形成**死锁**。

**修复**:`close(s.events)` **只在 lifecycle goroutine 里调用**(普通 `close()`,不是 `closeOnce.Do`);`Close()` 的 `<-s.exitDone` 等待**移到 `closeOnce.Do` 之外**,只保留 `close(s.closed) + s.cancel() + stdinW.Close()` 在 Once 内。

这条规则已经被 `TestE2E_FreshThread` + `TestE2E_ResumeThread` 守住:不开 e2e 实测,这个 bug 会藏到生产。

---

## 3. JSON-RPC 2.0 客户端(rpc.go)

### 3.1 写入路径

`rpcClient.request(ctx, method, params, &result)`:

1. 拿 `writeMu`(序列化所有写)
2. 分配 `id`(`atomic.Int64` 单调递增)
3. 注册到 `pending map[uint64]chan rpcResponse`
4. marshal + 写入 stdout pipe(末尾 `\n`)
5. `select { case <-ch: case <-ctx.Done(): case <-s.closed: }`

`respond()` / `respondErr()` 是 server-initiated request 的应答路径,只写不读。

### 3.2 读取路径(readPump)

`bufio.Scanner` 逐行读取,buffer 上限 10 MiB。每一行:

- 有 `id` + `result`/`error` → 查 `pending` map 派发
- 有 `method` + 无 `id` → 走 `translator.handleNotification`
- 有 `method` + 有 `id` → 走 `permissions.handleServerRequest`
- 都不是 / 字段缺失 → `emitWireError`(见 §8)

### 3.3 failPending 的两处调用点

| 触发点 | 错误 | 何时 |
|---|---|---|
| `readPump` 看到 EOF | `ErrSessionClosed` | 子进程关 stdout,正常退出 |
| `emitWireError` 之后 | `ErrSessionClosed` | wire 损坏,主动关 stdin |
| `lifecycle()` 之后 | `ErrSessionClosed` | 兜底双触发 |

**所有写请求都会在 `defer failPending(ErrSessionClosed)` 里被解**——见 §8。

### 3.4 ⛓️ 踩坑:`rpcClient.request` 的 ctx 派生

`request` 必须把传入 ctx 派生一个带 `s.ctx` 作为 parent 的 ctx,这样即便调用方 ctx 超时,session 关闭也能正确唤醒 caller。代码里:

```go
callCtx, cancel := context.WithCancel(ctx)
defer cancel()
go func() {
    select {
    case <-s.ctx.Done():
        cancel()  // session 关闭 → caller ctx 立即收到
    case <-callCtx.Done():
    }
}()
```

否则一旦父 ctx 取消,callCtx 会一直挂到子进程退出才醒。

---

## 4. 事件整合(F-52 镜像)

### 4.1 翻译表

| 通知 | 处理 | 产出 AgentEvent |
|---|---|---|
| `thread/started` | set threadId | — (`EventAgentReady` 已在 handshake 完成时合成) |
| `turn/started` | mark `t.turn.active=true` | — |
| `item/started` (agentMessage) | begin buffering | — |
| `item/completed` (agentMessage) | 缓存 pendingMsg | — |
| `item/agentMessage/delta` ❌ opt-out | — | — |
| `turn/completed` | flush + Result + Done + `onTurnEnd()` | `EventResult` + `EventDone{Reason:"settled"}` |
| `turn/failed` | flush + Err + Done + `onTurnEnd()` | `EventResult` + `EventAgentError` + `EventDone{Reason:"failed"}` |
| `thread/status/changed.idle` | idem­potent Done | `EventDone{Reason:"idle"}` + `onTurnEnd()`(无 turn/completed 时兜底) |
| `item/commandExecution/outputDelta` | append to tool block | `EventToolUpdate` |
| `item/commandExecution/completed` | 终态 + status | `EventToolEnd` |
| `item/fileChange/outputDelta` | patch 字符累积 | `EventToolUpdate` |
| `item/fileChange/completed` | 终态 + status | `EventToolEnd` |
| `item/reasoning/summaryTextDelta` / `textDelta` | 拼 `Reasoning` 块,带 `[思考]` 前缀 | `EventToolUpdate`(reasoning 工具) |
| `item/plan/delta` ❌ opt-out | — | — |
| `item/contextCompaction/completed` | 发 sentinel 文本 | `EventResult{Text:"…(context compacted)…"}` |
| `thread/tokenUsage/updated`(codex ≥ 0.125) | overwrite `t.turn.lastUsage` | — (下一 turn/completed 时取用) |
| `error` (顶层) | `EventAgentError` | `EventAgentError` |

### 4.2 6 条 optOut 必须全发

`initialize.capabilities.optOutNotificationMethods` 写死 6 条:

```text
command/exec/outputDelta
item/agentMessage/delta
item/plan/delta
item/fileChange/outputDelta
item/reasoning/summaryTextDelta
item/reasoning/textDelta
```

**缺一条 = 同一 agentMessage 收到 delta 增量 + completed 全量,bridge 重复消费。** 这条契约被 `initialize()` 单测守。

### 4.3 ⛓️ 踩坑:`thread/tokenUsage/updated`(codex ≥ 0.125)

codex < 0.125 的 usage 只通过 `turn/completed.params.usage` 报一次;**0.125 之后会单独 push `thread/tokenUsage/updated` 通知**。两条路径都必须接,否则 usage 永远是 0:

- **路径 A**(`turn/completed` 一次性):`params.usage != nil` → 直接用
- **路径 B**(`thread/tokenUsage/updated` 多次):overwrite `t.turn.lastUsage`(`Last` 优先,全 0 才退回 `Total`)
- **路径 C**(都空):`turn/completed` 来时 `params==nil` + `t.turn.lastUsage==nil` → 跳过 usage 字段(不要写 0 进去)

`TestTranslate_TokenUsageUpdatedStoresLastAsUsage` + `TestTranslate_CompleteTurnUsesTokenUsageWhenParamsNil` 守住两条路径。

---

## 5. 单线程单 turn(`ErrTurnBusy`)

`codex app-server` 一个 thread 同时只能跑一个 turn,这是协议层面的硬约束。

### 5.1 行为契约

- `SendBlocks` 收到请求时,如果 `pendingTurnActive==true`(上一 turn 还没收到终态事件)→ **直接返回 `ErrTurnBusy`**,不发任何 RPC。
- 终态事件触发(`turn/completed` / `turn/failed` / `thread/status/changed.idle`)→ 清除 `pendingTurnActive`。

### 5.2 ⛓️ 踩坑:`pendingTurnActive` 的 defer 重置

**问题**:最初用 `defer func(){ pendingTurnActive = false }()` 在 `SendBlocks` return 时清。结果是 `turn/start` 一返回(几十 ms)就清,真正的 turn 还在 codex 侧跑;紧接着第二次 `SendBlocks` 看 `pendingTurnActive==false` → 放行 → codex 侧拒绝 → `turn/start` 报错。

**修复**:`pendingTurnActive` 的释放下沉到 **translator 的 `onTurnEnd()` 回调**,由 `completeTurn` 在发送完 `Result` + `Done` 之后调用。`SendBlocks` 只设置 `pendingTurnActive=true`,**不再 defer 清**。

`onTurnEnd` 是 `translator` 构造时传入的闭包:

```go
live.session.translator = newTranslator(
    live.deliver, a.name, cfg.Workspace, s.branch, s.stderrTail,
    func() {
        a.pendingMu.Lock()
        a.pendingTurnActive = false
        a.pendingMu.Unlock()
    },
)
```

注意三处调用点都要触发 `onTurnEnd`:正常 `completeTurn`、`completeTurn` 早返(turn 为空)、`handleThreadStatusChanged`(idle 兜底路径)。

---

## 6. 审批流(server-initiated request)

### 6.1 4 类 server-request + 1 类工具

| method | Tool | 默认选项 |
|---|---|---|
| `item/commandExecution/requestApproval` | `Bash` | `["accept","decline"]` |
| `item/fileChange/requestApproval` | `Patch` | `["accept","decline"]` |
| `item/permissions/requestApproval` | `Permissions` | `["accept","decline"]` |
| `item/tool/requestUserInput` | `AskUserQuestion` | `["ok"]`(channel 回复 `<qid>:<labels>\|...`) |
| `item/tool/call`(动态工具) | (动态) | `success:false`,contentItems=`"tool not available in nightme bridge"` |
| (任何未知 method) | — | 回 `method not found` |

### 6.2 5 分钟超时 → decline

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

`permissionTimeout` 是包级 `var`(非 `const`),`permissions_test.go` 把它压成 200 ms。

### 6.3 多 question AskUserQuestion 编码

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

`parseRequestUserInputResponse` 解码 → `{Answers: {qid: {Answers: [labels]}}}`,缺失 qid 用该题第一项兜底,保证响应永远结构良好。

### 6.4 ⛓️ 踩坑:`SendPermission` 误广播到所有 pending approval

**问题**:最初实现是遍历 `pendingApprovals map` 全发。结果两个 bug:

1. Go map 迭代顺序随机,行为不可预测
2. 多 approval 并发场景(比如 `requestUserInput` 在飞 + 一个 `tool approval` 在飞)下,用户一个 `accept` 会同时解掉两个,语义错误(尤其 `requestUserInput` 的 `<qid>:labels` 是**按问题分**的,广播会让别的题答非所问)

**修复**:session 加 `lastPendingID string`,`spawnApproval` 时更新成最新 request id,`SendPermission` 只往 `lastPendingID` 这一个 channel 发。decision goroutine 消费后清掉 `lastPendingID`,`emitWireError` 兜底清掉所有 pending。

单 approval 场景下行为完全不变(因为只有一个 approval 时它就是 last);多 approval 场景下语义正确。

---

## 7. 多模态图片

`ContentImage` → stage 到 `<workspace>/.nightme/codex/images/img_<ns>_<pid>.<ext>`,`turn/start` 的 input 里追加 `localImage {path: "…"}`。

文件后缀从 MIME type 推导(`imageExtFromMediaType`);`os.CreateTemp` 拿不到时返回错误。

`ContentFile` 第一期走 `@<path>` 行追加 fallback,无 file type。

---

## 8. Wire 错误处理(`emitWireError`)

`rpcClient.readPump` 看到无法解析 / 超大帧 / schema 不符时,**不能**直接 panic,也不能只是记日志——runtime 等着 `EventAgentError` 知道要终止。

### 8.1 必须做四件事,顺序敏感

```go
func (s *session) emitWireError(err error) {
    cLog("wire error", "err", err.Error())
    s.deliver(agent.AgentEvent{Kind: agent.EventAgentError, Err: err})

    // 1. 解所有 pending approvals(否则 runtime 永远等用户回复)
    s.pendingMu.Lock()
    for id, ch := range s.pendingApprovals {
        select {
        case ch <- "decline":
        default:
        }
        delete(s.pendingApprovals, id)
    }
    s.lastPendingID = ""
    s.pendingMu.Unlock()

    // 2. 解所有 pending RPC requests(否则调用方永远挂)
    s.rpc.failPending(ErrSessionClosed)

    // 3. 关 stdin(让 lifecycle 通过 cmd.Wait 退出)
    select {
    case <-s.closed:
    default:
        _ = s.stdinW.Close()
    }
}
```

### 8.2 ⛓️ 踩坑:少做了 1 + 2 会让 runtime 卡死

**问题**:最初只发 `EventAgentError` + 关 stdin。后果:

- `turn/start` 之类的 pending RPC 永远挂到子进程退出才返回
- `permissionTimeout` 触底前,审批 goroutine 永远不退出(其实 user 也看不到审批,decision goroutine 在等 ch)
- runtime 上层一片死寂,daemon 必须 SIGKILL

**修复**:补上 `pendingApprovals` 遍历 + `rpc.failPending`。这条规则覆盖在 `TestSession_EmitWireError_UnblocksPendingRequests`(e2e 测试清单 §10)。

---

## 9. 实现文件 & 调用路径速查

```text
cmd/nightme/agents.go
  └─ agent.Builtins.Register(codex.New("codex", "codex", nil))

internal/bridge/codex/
  ├── protocol.go          wire 类型 + initialize/thread/turn/permission
  │                        + appServerTokenUsageNotification(tokenUsage/updated)
  ├── rpc.go               JSON-RPC 2.0 client(writeMu/pendingMu/failPending/readPump)
  │                        + ErrSessionClosed + 10 MiB frame cap
  ├── session.go           spawn + I/O + lifecycle(独占 close(events)) + ringBuffer
  │                        + detectBranch + handshakeTimeout + closeDrainTimeout
  │                        + eventBufferSize=40960 + permissionTimeout=5min
  │                        + emitWireError(§8 四件套)
  ├── agent.go             *Agent(模板+live)/Start/Events/Send*/SendPermission/New/Close
  │                        + ErrTurnBusy + pendingTurnActive + onTurnEnd 接线
  ├── translate.go         envelope → AgentEvent,F-52 状态机
  │                        + pendingMsgs + flush-at-tool-boundary
  │                        + 2 路 usage 兜底(§4.3)
  │                        + onTurnEnd 三处触发(completeTurn 正常 + 早返 + idle)
  ├── permissions.go       4 类 server-request + 5min 超时
  │                        + lastPendingID 写入与清除
  │                        + spawnApproval goroutine(s.ctx.Done 兜底)
  ├── agent_test.go        SendPermission 路由 + 单元测试
  ├── permissions_test.go  approval 流 + permissionTimeout 压缩到 200ms
  ├── protocol_test.go     initialize params / optOut 6 条覆盖
  ├── rpc_test.go          JSON-RPC client(11 个测试,含并发 / failPending)
  ├── session_test.go      detectBranch + argv + SessionConfig
  ├── translate_test.go    F-52 状态机 + 2 路 usage + CompleteTurn fallback
  ├── testhelpers_realcodex_test.go  skip 守卫(NIGHTME_CODEX_E2E_APPROVAL)
  └── session_real_test.go 真实 codex CLI 的 e2e(FreshThread / ResumeThread / ApprovalFlow)
```

### 9.1 关键不变量在文件中的位置

| 不变量 | 位置 |
|---|---|
| `close(events)` 只在 lifecycle | `session.go::lifecycle()` |
| `closeOnce.Do` 只包 close+cancel+stdinClose | `session.go::Close()` |
| `onTurnEnd` 接线 | `agent.go::Start`(`newTranslator` 参数) |
| `lastPendingID` 写入 | `permissions.go::spawnApproval` |
| `lastPendingID` 清除 | `permissions.go::decision goroutine` + `session.go::emitWireError` |
| `failPending(ErrSessionClosed)` 三处 | `rpc.go::readPump EOF` + `session.go::emitWireError` + `session.go::lifecycle` |
| 6 条 optOut | `session.go::initialize`(`OptOutNotificationMethods` 字面量) |

---

## 10. 验收:测试金字塔

### 10.1 测试分层

| 层 | 跑法 | 不依赖 |
|---|---|---|
| 单元(50+) | `go test ./internal/bridge/codex/` 默认即跑 | 不需要 codex CLI |
| Real-CLI e2e: FreshThread | 默认 skip,设 `NIGHTME_CODEX_REAL_CODEX=1` 跑 | 需要 `codex` 在 PATH |
| Real-CLI e2e: ResumeThread | 同上 | 同上 |
| Real-CLI e2e: ApprovalFlow | `NIGHTME_CODEX_E2E_APPROVAL=1` 才跑(默认 skip) | 同上 + 需交互 |

### 10.2 实测耗时(2026-08)

| 测试 | 耗时 |
|---|---|
| TestE2E_FreshThread | ~5–7s(冷启 codex + handshake + 1 turn) |
| TestE2E_ResumeThread | ~13–16s(冷启 2 次 + handshake + 2 turns) |

### 10.3 ⛓️ 坑:codex 走代理会让测试 hang

codex CLI 默认会读 `HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY` / `http_proxy` / `https_proxy` / `all_proxy` / `FTP_PROXY` / `RSYNC_PROXY` 等环境变量。如果设了,codex 会**绕过你预期的 `api.minimaxi.com`**,转而尝试走代理(代理常常不可达,握手超时挂死)。

**测试前必须**:

```bash
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy FTP_PROXY ftp_proxy RSYNC_PROXY rsync_proxy
go test ./internal/bridge/codex/ -count=1
```

或在 Makefile / CI step 里强制 unset。

---

## 11. 可观测性

### 11.1 调试日志

`codexDebug` 默认 ON;**`NIGHTME_CODEX_DEBUG=0` 关**(也接受 `false` / `no` / `off`,大小写无关)。

日志带 `component=codex`,关键节点:

- `[codex] session started` (pid/workspace/resume/model/effort)
- `[codex] initialize ok` (userAgent)
- `[codex] session handshake complete` (threadId/model)
- `[codex] SendBlocks enter` (blocks/threadID) — 上线后保留,排查 turn race 极有用
- `[codex] SendText enter` — 仅调试时开,SendText 即 SendBlocks 包装
- `[codex] server request: unknown method` (回 method not found)
- `[codex] item/tool/call: returning tool not available` (动态工具降级)
- `codex: approval timed out, defaulting to decline` (5min 触底)

### 11.2 events channel 满的反压兜底

`agent.deliver` 在 channel 满(cap=64)时:

1. 主动丢这一条事件
2. emit `EventAgentError{... dropped text}`

这是**最后兜底**,正常情况下不应该发生。如果发生,说明 runtime readpump 比 bridge produce 慢,需要查 `Chatsession.PumpEvents` 是否在读。

### 11.3 daemon / runtime 注意

- 重启 daemon:`make restart`(kill 旧 + 起新);**不会**自动 rebuild,要先 `make build`
- 日志:`~/.nightme/nightme.log`(JSON lines)
- 状态:`~/.nightme/agent_sessions.json` + `~/.nightme/chat_sessions.json`
- `/use codex` 切换之后,下一次 SendBlocks 才走 codex bridge,切换前的 chat session 继续跑原 agent

---

## 12. 已知的 codex 特性差异

| 项 | 状态 | 备注 |
|---|---|---|
| `ContextWindow` 报数 | ❌ 第一期不报 | footer `(window)` 分母缺,后续若 codex 加 `turnInfo.model.contextWindow` 字段再补 |
| task 子系统 | ❌ 不实现 | codex app-server 无 task 事件,`EventAgentTaskCreate/Update` 永不发 |
| 多模态图片 | ✅ | `ContentImage` → stage + `localImage` |
| 多模态文件 | ⚠️ fallback | `ContentFile` 走 `@<path>` 行追加(无 file type) |
| 第三方 provider | ❌ 不实现 | `codex config.toml [model_providers.*]` / `auth.json` 留给 F-XX;本期直接 `-c model_provider=… -c openai_base_url=…` 透传 |
| 动态工具 `item/tool/call` | ❌ 返回 not available | 跟 cc-connect MVP 一致 |
| `account/rateLimits/updated` UI | ❌ 不暴露 | nightme 无 quota UI |
| 跨 turn threadId 持久化 | ✅ `cfg.SessionID` ↔ `threadId` | `thread/resume {persistExtendedHistory:true}` |

---

## 13. 待定事项(本期不做)

| 项 | 理由 | 何时做 |
|---|---|---|
| ~~`codex exec` 后端(双选)~~ | ~~违反 F-32/F-52/F-34 多 turn 契约~~ | ~~永不~~ — F-CODEX-PRINT-001(2026-08-14)落地:`exec` 已用作 print-mode 入口(见 §1.4);`app_server` 仍是 chat-session 唯一后端,未引入双后端运行时分歧 |
| ACP 中间层 | 多一层 Node shim | 永不 |
| 模型别名表 | `-c model=…` 直传 | 永不 |
| 第三方 provider config.toml | 独立 PR | F-XX |
| `item/tool/call` 动态工具 | cc-connect 也只回 not available | 等 Codex 稳定再议 |
| `account/rateLimits/updated` UI | nightme 无 quota UI | 待有 quota UI 再说 |
| `internal/processkill` 抽提 | CC-connect proc_unix/proc_windows 端口 | 单独 PR |
| `internal/redactdiag` stderr 红线 | openclaw-lark 模式 | 单独 PR |

---

## 14. 回归测试清单(下一轮 review 时跑)

放在 `session_close_test.go`,被下面这些坑逼出来的:

- [ ] `TestSession_CloseAfterStart_ReturnsWithin5s` — 启动 + Close 必须在 `closeDrainTimeout` 内返回
- [ ] `TestSession_ConcurrentClose_OnlyOneNoHang` — 两次 Close 并发,都不死锁
- [ ] `TestAgent_PendingTurnActive_NotClearedUntilTurnEnd` — 模拟 turn/start 返回后立刻 SendBlocks,验 ErrTurnBusy;发 turn/completed 后 SendBlocks 成功
- [ ] `TestAgent_SendPermission_RoutesToMostRecentOnly` — 两个 pending approval,SendPermission("accept") 只解最近一个
- [ ] `TestSession_EmitWireError_UnblocksPendingRequests` — 注入 wire error,验 pending RPC 立即收到 ErrSessionClosed
- [x] `TestEventsBufferSize_PinnedAt40960` — events channel cap 锁定 40960(对齐 pi / claudecode / pty / acp)
- [x] `TestDeliver_BlocksWhenConsumerLags_NoDrop` — consumer lag 时不 instant drop
- [x] `TestDeliver_UnblocksOnClose` — Close() 解 blocking deliver()
- [x] `TestDeliver_UnblocksOnExitDone` — lifecycle 解 blocking deliver()
- [x] `TestAgent_PendingTurnActive_ReleasedOnImageStageError` — image stage 错误后 guard 释放
- [x] `TestAgent_PendingTurnActive_ReleasedOnEmptyInput` — 全空 input 后 guard 释放
- [x] `TestAgent_PendingTurnActive_ReleasedByOnTurnEndCallback` — 闭包捕获 live 而非模板
- [ ] `TestDetect_AcceptsExistingBinary`(已实现):用 `codex --version` 替换 `exec.LookPath`,坏 binary 立即失败

---

## 15. 排错速查

| 症状 | 根因 | 修法 |
|---|---|---|
| `codex: approval timed out` 频繁触发 | 用户没看到审批 UI | 查 `ChatSession.PumpEvents` 是否在读 events |
| `turn/start` 立刻报 `ErrSessionClosed` | 上一 turn 还没完又发新 turn | 检查 `pendingTurnActive` 释放路径(§5.2) |
| events channel 满 + drop | runtime 没在读 | 查 `internal/chatsession` 的 `PumpEvents` |
| usage 永远是 0 | codex 升级到 0.125+ | 检查是否注册了 `thread/tokenUsage/updated` 处理器(§4.3) |
| test hang | 走了代理 | unset 代理变量(§10.3) |
| 启动后立刻 `lifecycle exit` | stdin 漏接 / binary 错 | 看 `[codex] session started` 后有没有 `initialize ok`;有就 binary 错 |
| `method not found` 回包 | codex 推了未识别 method | 看 `[codex] server request: unknown method` 日志 |
| close 之后 runtime 卡住 | lifecycle 没拿到 exitDone | 看 §2.6 死锁模式 |

---

## 16. 版本与兼容性

- **最低 codex CLI 版本**:≥ 0.95(`thread/started` 通知 + `initialize` 三步握手 + 6 条 optOut 生效)
- **已知兼容**:codex 0.95–0.145(含 `thread/tokenUsage/updated` 0.125+)
- **不兼容**:`< 0.95` 的旧 CLI(没用 app-server 子命令)— 用户应 `codex login` 升级
