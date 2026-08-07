# T-alive: claude 启动 / --resume / OutReply 卡死 — 调研交接 + 修复落地

> **状态**: 调研 + 修复落地。test19 hang 已根治,**resume 在每个 chat 生命周期内保留**。
> **日期**: 2026-08-07
> **前置文档**: [`wip.md`](./wip.md) L1/L2/L3 健康监控设计;本次 bug 与 L2 stall 探测同源。
> **代码分支**: `feat/alive`(未提交 — 见末尾 §7)

---

## 0. TL;DR — 根因 + 修复

**症状**:feishu 用户发消息后,只看到 placeholder 卡 + ⌨/🤖 reaction,永远等不到 OutReply/OutResult。

**根因(实测定位,2026-08-07)**:

feat/alive 分支相对 main 引入了 **两个独立 bug** 叠加,共同导致 hang:

1. **`--print` 被错误删除**(`permissions.go`):没有 `--print`,claude 跑 multi-turn interactive 模式,要等到 stdin 有 newline-terminated 数据后才 emit `system init`。ChatSession 在 SendBlocks 前没人喂 stdin,claude 永远等。
2. **`ObserveClose` 死代码 + KindLifecycle 未连 SetExited**(`agentsession.go` + `pump_events.go`):`--print` 单轮下 claude 处理完一条消息后保持 stdin 开着等下一条,但 AS 不会因此标记 Exited。下一次 LookupActiveAgentSession 看到 StatusRunning 复用死 handle,SendBlocks 写到坏 pipe 没反应。

任一 bug 修了都不够 — 两个叠加才是 hang。

**修复**(3 处):
1. `internal/bridge/claudecode/permissions.go`:恢复 `--print` 到 DefaultArgs(对齐 main)
2. `internal/chatsession/pump_events.go`:`KindLifecycle{StatusExited}` 分支调 `as.SetExited(0)`,让 chat-session 知道 AS 已死走 respawn 路径
3. `internal/bridge/claudecode/claudecode.go`:删除 silent fallback,改返 `ErrResumeUnhealthy`(防止 resume 真的失败时静默丢上下文)

---

## 1. 事件链 — feishu 到 claude 的出站路径

```
[feishu incoming]
  └→ mgr.GetOrCreate → cs := ChatSession
  └→ cs.EmitMessageState(MessageQueued)        → ⌨️ reaction
  └→ cs.LookupActiveAgentSession()
        └─ AS.Status == StatusRunning → reuse handle (multi-turn via stdin)
        └─ AS.Status == StatusExited  → reuse pool entry + Spawn 同 ResumeID
        └─ first time → Spawn new
              └─ claudecode.Agent.Start(ctx, cfg)
                    ├─ newSession → cmd.Start() → pumpStream goroutine
                    ├─ probeResume(ctx, sess)         ← 5s safetyCtx
                    ├─ unhealthy → ErrResumeUnhealthy (no fallback)
                    └─ return sess
              └─ set as.handle, pid, stat=Running, isReady=true
              └─ as.startReadPump() → 读 sess.Events() → push to as.eventQueue
              └─ emit KindLifecycle{Spawned}
  └→ cs.EmitMessageState(MessageSubmitted)     → 🔄 reaction
  └→ cs.QueueUserMessage(msg) → TryFlush → as.Submit
        └─ h.SendBlocks(ctx, p.Blocks) → writeLine to claude stdin
  └─ (claude 读 stdin → emit init/text/result → 等下一条 stdin)
  └─ (后续 turn:同一 bridge session,继续 SendBlocks)
  └─ (daemon 重启 / AS respawn:用同一 ResumeID 启新进程)
```

**关键依赖**:`OutboundMessage.ReplyTo` 必须等于 userMsgID,feishu 才能 PATCH 到正确的占位卡。这个值来自 `Prompt.LastMessageID`,由 `buildPromptLocked` 设置。

**`--print` + bridge-held-stdin 行为**(实测,2026-08-07):
- claude emit init 立即(不需要 stdin 数据)
- claude 处理一条消息,emit init/text/result
- **claude 不退出**(等下一条 stdin)
- 多次 SendBlocks 在同一 bridge session,同一 claude 进程
- claude 只在 stdin EOF 或 `/exit` 时退出
- `--resume <id>` 只在 daemon 重启 / AS respawn 时用 — claude 从磁盘恢复上下文,启动后继续 multi-turn

---

## 2. 修过的 Bug — 按发现顺序

### 2.1 `Prompt.LastMessageID` 未赋值(锚点丢失)

**症状**:feishu 永远看不到 OutReply,因为 receipt 卡找不到 userMsgID,新卡片永远 orphan。

**位置**:`internal/chatsession/chatsession.go::buildPromptLocked`

**修复**:在 `buildPromptLocked` 末尾加 `p.LastMessageID = ids[n-1]`。

### 2.2 `FromAgentSessionEntry` 不初始化 `isReady`(磁盘恢复后永久 SKIP)

**症状**:daemon 重启后,所有 chat 第一次发消息都 `TryFlush SKIP reason=as_not_ready`。

**位置**:`internal/chatsession/agentsession.go::Spawn`

**修复**:`as.isReady.Store(true)` 在 handle 设置后立刻调用,无论 AS 是新创建还是磁盘恢复。

### 2.3 Probe / AS readpump 抢 `sess.Events()` channel

**症状**:probe 跟 AS readpump 同时从 `sess.Events()` 读事件。Go channel 语义每条消息只被一个消费者收到。probe 经常抢到 init,导致 AS readpump 看不到 init。

**修复**:probe 改为**纯 stderr 监听**(不读 events)。

### 2.4 feishu adapter MessageQueued 时立刻建占位卡(UX)

**症状**:用户看不到任何视觉反馈直到 claude 真响应——但 claude 又又卡住,所以用户永远看不到。

**位置**:`internal/channel/feishu/adapter.go::Send` OutMessageState 分支

**修复**:`MessageQueued` 时立刻 `ensureReceiptForTyping`,placeholder + ⌨ 立即可见。

---

## 3. 真正的根因(test19 hang)+ 修复落地

### 3.1 feat/alive 引入的两个独立 bug(实测定位,2026-08-07)

**Bug A:`--print` 被删除**(`permissions.go`)

feat/alive 的 DefaultArgs:
```go
var DefaultArgs = []string{
    "--input-format", "stream-json",
    "--output-format", "stream-json",
    "--permission-mode", "bypassPermissions",
    "--verbose",
}
```

没有 `--print`。claude 跑 multi-turn interactive 模式:

> 实测:`claude --resume <id> --input-format stream-json --output-format stream-json --verbose`(空 stdin)
> 输出:hook_started → hook_response → **不 emit init** → 等 stdin

claude 在 multi-turn 模式下,要等 stdin 有 newline-terminated JSON 数据才 emit `system init`。生产流程:ChatSession.Spawn 后,SendBlocks 写 stdin 之前,claude 已经挂了。claude 等 stdin,init 不来,用户看不到 OutReply。

**对比 main**:`--print` 让 claude 是 single-turn per message 模式(init 立即 emit),但 stdin 仍由 bridge 持有不关,claude 处理完一条等下一条,实际是**单进程多轮**(per chat lifetime 一个 claude 进程)。

**Bug B:`ObserveClose` 死代码 + KindLifecycle 未连 SetExited**

main 分支的 `internal/chatsession/readpump.go` 在 handle.Events() close 时调 `as.SetExited(0)`。feat/alive 把 readpump 拆到 per-AS(`agentsession_readpump.go`),改成 emit `KindLifecycle{StatusExited}`,但 `pump_events.go` 只 log 不 SetExited。

如果只用 Bug A 修复(恢复 `--print`),每条用户消息 init 立即 emit,但 AS.Status 永远 Running。chat-session 不会 respawn。生产场景是 OK(同一进程多轮),但 daemon 重启或 AS 被 Close 后,LookupActiveAgentSession 复用死 handle,SendBlocks 写到坏 pipe 没响应 — 跟用户报的症状一模一样。

两个 bug 叠加:test19 既遇 stdin-gated init hang(因为没 `--print`),又遇死 handle(因为 KindLifecycle 未连)。修一个不够。

### 3.2 修复

**修复 A**:`internal/bridge/claudecode/permissions.go`
- 恢复 `--print` 到 DefaultArgs 头部,加 docstring 说明

**修复 B**:`internal/chatsession/pump_events.go`
- `routeEvent` 的 `KindLifecycle` 分支:检测 `ev.Lifecycle.Status == StatusExited` → 调 `as.SetExited(0)`
- 注释里说明 main 是在 readpump 直接 SetExited,feat/alive 拆分后必须在 consumer 侧连

**修复 C**:`internal/bridge/claudecode/claudecode.go`
- 删除 `resumeClearedConfig` + silent fallback
- 新增 `ErrResumeUnhealthy`
- `Start` unhealthy 时返 error 透给 runtime,不静默丢 resume
- `probeResume` 返回 reason 让 Start 透给用户
- `classifyStderrLineForResume` 删除 MCP 类别(MCP 失败是 informational,不应触发 unhealthy)
- 删除 dead code: `probeResumeHealthyEventsOnly`、`isHealthyResumePostInitEvent`

### 3.3 行为对比

| 场景 | 旧行为 | 新行为 |
|---|---|---|
| 多轮对话 test19 | placeholder + ⌨→🔄 → 无 OutReply(死锁) | placeholder + ⌨→🔄 → ~22s 内 OutReply;claude 不退出,等下一条 |
| invalid UUID + valid workspace | 静默 fallback,丢 resume 上下文 | `ErrResumeUnhealthy`,runtime 上报 "Failed to spawn agent: ... check workspace path and resume id" |
| valid UUID + wrong workspace | 静默 fallback,丢 resume | `ErrResumeUnhealthy`,同样上报 |
| MCP server failed (其他都正常) | probe unhealthy,fallback fresh,丢 resume | probe healthy,resume 保留,MCP 失败仅 warn log |
| 正确 workspace + valid id,首条 | resume + init.SessionID 匹配 | **完全一致**(实测 <2s init) |
| daemon 重启 / AS respawn | main 工作(ObserveClose 直接 SetExited) | feat/alive 修复(KindLifecycle 触发 SetExited) |

### 3.4 不在本次范围(留给后续 PR)

- **减 probe timeout**(5s → 2s):收益小,改动独立
- **`/exit` slash command 触发**:daemon Shutdown 时让 claude 优雅退出(目前靠进程 kill)

---

## 4. 测试基建 — 已落地的覆盖

| 测试 | 文件 | 验证什么 |
|---|---|---|
| `TestStart_ResumeMultiTurnRespawn` | `resume_multi_turn_test.go` | **核心**:同一 chat 多轮(phase 1 fresh → phase 2 --resume 新进程 → phase 3 同一 session 第二个 turn)— 完整 test19 复现 + 修复验证 |
| `TestStart_ResumeID_PreservedAcrossProbe` | `resume_fallback_test.go` | 用 `t.TempDir()` 做 create→resume,断言 `init.SessionID == resumeID` |
| `TestStart_ResumeRejectionSurfacesError` | `resume_fallback_test.go` | invalid UUID → 期望 `ErrResumeUnhealthy`,**不**返 fallback fresh |
| `TestClassifyStderrLineForResume_MCPNotTrigger` | `resume_fallback_test.go` | MCP 失败 stderr 不再被 classifier 标记 unhealthy |
| `TestClassifyStderrLineForResume_RejectionTriggers` | `resume_fallback_test.go` | session not found / resume requires 仍标记 unhealthy |
| `TestResumePaths_Table/resume_invalid_uuid` | `resume_paths_test.go` | invalid UUID → 期望 `ErrResumeUnhealthy`(已重写) |
| `TestResumePaths_Table/resume_user_workspace_known_id` | `resume_paths_test.go` | 用户的实际 session,init 在 <10s |
| `TestResumePaths_Table/resume_happy_path_replay` | `resume_paths_test.go` | fresh → capture → resume → 输出 |
| `TestIsResumeErrorMessage` | `resume_fallback_test.go` | 字符串匹配器正确性 |
| `TestFreshLiveness_PassesAnswer` | `fresh_liveness_test.go` | fresh session in clean workspace works |
| `TestFreshLiveness_LogsUserMCP` | `fresh_liveness_test.go` | fresh session in user workspace (with MCP config) works |

### 实测验证(2026-08-07)

```
NIGHTME_TALIVE_RESUME_USER=1 NIGHTME_TALIVE_RESUME_REPLAY=1 NIGHTME_TALIVE_RESUME_BAD=1 \
  go test -count=1 -run "TestStart_Resume|TestResumePaths_Table" -timeout 240s ./internal/bridge/claudecode/

TestStart_ResumeRejectionSurfacesError     PASS (2.35s)
TestStart_ResumeID_PreservedAcrossProbe    PASS (11.49s)
TestStart_ResumeMultiTurnRespawn           PASS (15.33s)   ← 核心
TestResumePaths_Table
  resume_happy_path_replay                 PASS (14.15s)
  resume_invalid_uuid                      PASS (3.14s)
  resume_user_workspace_known_id           PASS (7.95s)

internal/chatsession  PASS
internal/gateway      PASS
go build ./...        OK
```

---

## 5. 关键架构 — 接替者需要知道的

### 5.1 Claude 生命周期(--print 模式)

| 状态 | 触发 | 处理 |
|---|---|---|
| 启动 | AS.Spawn | cmd.Start,claude 进程就绪 |
| init emit | 启动后立即(<1s) | 通过 bridge events 通道流到 runtime |
| 多轮 stdin 处理 | 每条 SendBlocks | claude 读 stdin,emit text/result,继续等 |
| 退出 | stdin EOF 或 `/exit` | handle.Events() 关闭 |
| 重启 | daemon 重启 / AS respawn | 同 ResumeID → claude 从磁盘恢复上下文 |

### 5.2 `--resume` 的语义

- 每次 AS.Spawn 都会传 `--resume <id>`(如果 cfg.ResumeID 不空)
- claude 启动时从 `~/.claude/projects/<workspace>/<session_id>.jsonl` 加载历史
- 加载后,claude 仍按 multi-turn stdin 模式运行
- 同 chat 内的多轮,**不**需要 respawn — 同一 claude 进程持续处理

### 5.3 Resume 失败的语义

| stderr 信号 | 行为 |
|---|---|
| `No conversation found with session ID: <uuid>` | unhealthy → `ErrResumeUnhealthy` |
| `--resume requires a valid session ID...` | unhealthy → `ErrResumeUnhealthy` |
| `Failed to connect MCP server ...` | healthy(忽略),resume 保留 |
| 静默 | healthy |

### 5.4 Events channel 是 buffered,不是 pub/sub

`claudecode.session.events` 是 cap 64 的 buffered channel。**只能有一个消费者**。当前 AS readpump 是消费者;probe **已不再**读 events(改读 stderr)。**不要碰 events channel**。

---

## 6. 当前 uncommitted 改动

```
 cmd/nightme/run.go                              |  32 ++++
 internal/bridge/claudecode/claudecode.go        |  +252 / -85 (fallback removal + classifier cleanup)
 internal/bridge/claudecode/permissions.go       |  11 ±  (--print restored)
 internal/bridge/claudecode/session.go           |  123 ++++++++++++---
 internal/bridge/claudecode/resume_fallback_test.go | rewritten
 internal/bridge/claudecode/resume_multi_turn_test.go | NEW (TestStart_ResumeMultiTurnRespawn)
 internal/bridge/claudecode/resume_paths_test.go    | resumeInvalidUUID rewritten
 internal/channel/feishu/adapter.go              |  27 +++
 internal/chatsession/agentsession.go            |  21 +++
 internal/chatsession/pump_events.go             |  +15 (KindLifecycle → as.SetExited)
```

Untracked:
```
 internal/bridge/claudecode/fresh_liveness_test.go
 internal/bridge/claudecode/repro_real_test.go
 internal/bridge/claudecode/resume_paths_test.go
 internal/bridge/claudecode/resume_multi_turn_test.go (new)
 internal/gateway/integration_chatsession_test.go
```

### 6.1 主要修改文件

- **`internal/chatsession/chatsession.go`**: `buildPromptLocked` 加 `LastMessageID` 赋值
- **`internal/chatsession/agentsession.go`**: `Spawn` 末尾 `as.isReady.Store(true)`
- **`internal/chatsession/pump_events.go`**: `routeEvent` 的 `KindLifecycle` 分支调 `as.SetExited(0)`
- **`internal/channel/feishu/adapter.go`**: `OutMessageState` 在 `MessageQueued` 时调 `ensureReceiptForTyping`
- **`internal/bridge/claudecode/permissions.go`**: 恢复 `--print` 到 DefaultArgs 头部
- **`internal/bridge/claudecode/session.go`**: `session.stderrLines chan string`,`StderrLines()` 方法
- **`internal/bridge/claudecode/claudecode.go`**: 删除 fallback,新增 `ErrResumeUnhealthy`,`probeResume` 返回 reason,classifier 删除 MCP 类别

### 6.2 调试日志

跑 `bin/nightme logs --once -n 200 | grep -E "chatsession:|claudecode:"` 看 trace:
- `chatsession: PumpEvents started`
- `chatsession: Submit` / `Submit SendBlocks ok`
- `chatsession: TryFlush SKIP` (带 reason: queue_empty/activeAS_nil/as_not_ready)
- `chatsession: AS <id> marked Exited (claude process exited)` — respawn 触发器
- `claudecode: resume spawn unhealthy; refusing fallback to preserve resume context`(resume 失败时,**不会 fallback**)
- `claudecode stderr` (level Warn) — stderr 错误信号

---

## 7. 提交建议

1. **`fix(chatsession): set Prompt.LastMessageID at build time`** — fix #2.1
2. **`fix(chatsession): re-arm isReady on Spawn for restored AgentSessions`** — fix #2.2
3. **`feat(feishu): create placeholder receipt card at MessageQueued`** — fix #2.4
4. **`fix(claudecode): restore --print in DefaultArgs (multi-turn stdin-gated init hang)`** — **本次主修复之一**
5. **`feat(claudecode): expose stderr lines + add stderr-only resume probe`** — fix #2.3
6. **`fix(claudecode): refuse silent resume fallback — return ErrResumeUnhealthy`** — 辅助修复(防止 resume 失败静默丢上下文)
7. **`fix(chatsession): wire KindLifecycle{StatusExited} to AS.SetExited`** — **本次主修复之二**(让 daemon 重启 / AS respawn 路径生效)
8. **`test(claudecode): cover resume preservation + classifier + multi-turn`** — 4 个新 test(resume_fallback / resume_paths / resume_multi_turn)

---

## 8. 给接替者的清单

按顺序看:
1. `tasks/wip.md` 和 `docs/feat/message_lifecycle.md` — 理解 L1/L2/L3 健康监控
2. 本文档 §3.1-3.3 — 理解 `--print` + KindLifecycle 的双重根因
3. 跑测试:
   ```bash
   NIGHTME_TALIVE_RESUME_USER=1 go test -count=1 -run "TestStart_Resume|TestResumePaths" -timeout 240s ./internal/bridge/claudecode/
   ```
4. **手动验证多轮对话**:启 daemon,发消息,确认几秒内 OutReply + claude 进程保持存活(继续等下一条)

---

## 9. 一句话教训

> `--print` 不能随便删 — 它让 claude 在 stream-json 下立即 emit init,否则会等 stdin(bridge 还没写)就死锁。同时 per-AS readpump 拆分后必须在 consumer 侧(pump_events)重新连 `as.SetExited`,否则 daemon 重启或 AS 关闭后 chat-session 复用死 handle,SendBlocks 写到坏 pipe。改架构时,旧路径的 side effect 要在新路径里手动接回来 — 不能假设新代码自己会处理。