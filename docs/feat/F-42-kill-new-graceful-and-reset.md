# F-42: `/kill` Graceful Shutdown + `/new` ResumeID Clear + 列表式回复

> **Status**: 📝 设计落地（2026-08-05）
> **Milestone**: v1.3.x
> **Depends on**: F-34 (`/new` slash command), F-27 (ChatSession), F-29 (AgentSession pool), F-19 (CLI Bridge), F-24 (Claude Code Bridge), F-32 (Pi RPC Bridge), F-33 (ACP Bridge)
> **Related**: [`SPEC.md`](../SPEC.md) §3.2 状态转换触发器, [`F-06-process-cleanup.md`](./F-06-process-cleanup.md), [`F-27-chatsession.md`](./F-27-chatsession.md), [`F-29-agent-session-pool.md`](./F-29-agent-session-pool.md), [`F-34-new-slash-command.md`](./F-34-new-slash-command.md), [`F-39-result-as-new-reply.md`](./F-39-result-as-new-reply.md)

---

## 1. Description

三个独立但相互关联的修复,统一打包:

1. **`/kill` 改成 bridge graceful shutdown** —— 当前 `ChatSession.KillAll` 只清理 nightme 侧的内存和 disk,**不向 child CLI 发任何信号**,导致进程孤儿化继续运行;同时清掉 InputBuffer 等不属于该命令管理的 state。
2. **`/new` 对 dead/detached AgentSession 清 ResumeID** —— 当前 `NewActiveAgentSessions` 对 `StatusRunning` 之外的 entry **silently skip**,但 pool entry 里残留的旧 ResumeID 会在下次 spawn 时被透传,导致 `--resume <死 id>` 试图续接一个死 session(违背 `/new` 的语义)。
3. **两个命令的回复文案升级为 per-entry 列表** —— 用户从 "Killed 2 agents" 升级到 "✓ claude @ /A\n✓ codex @ /B",每条 entry 都有明确归属。

不变式（受 SPEC §1.3 约束）：

- **slash command 边界**:`/kill` 和 `/new` **只与 agent 进程交互**。`activeCwd` / `activeAgent` / `currentTurnUserMsgID` / `InputBuffer` 都不属于它们。
- **graceful 优先**:每个 bridge 的 `Close()` 已有 stdin EOF + SIGINT + 2s grace + SIGKILL 兜底的本地 watchdog（见 `claudecode` `session.go:466-498`）。nightme 端**不二次升级**信号。
- **不主动浪费资源**:`/new` **永不**为触发"reset"而 spawn 一个 dead agent。dead 状态 = 不动进程,只清 ResumeID。
- **state 同步**:disk 删除必须在进程死 *之后*,不能根据"还没发生的事"提前删。

---

## 2. Motivation & Problem

### 2.1 当前 `/kill` 的三个缺陷

`internal/chatsession/chatsession.go:769-796` 的 `KillAll` 注释自陈 "v1.2 commit 6: this is a data-only operation — no actual signal is sent (commit 7 will wire SIGTERM)",代码也确实如此:

```go
func (cs *ChatSession) KillAll() error {
    cs.StopReadPump()                  // ← 停 nightme 侧 read goroutine
    cs.mu.Lock()
    cs.pool = make(map[agentCwdKey]*AgentSession)  // ← 整张 map 重新分配
    cs.activeAS = nil
    cs.currentTurnUserMsgID = ""
    cs.mu.Unlock()
    if cs.inputBuffer != nil {
        cs.inputBuffer.Clear()         // ← 副作用:清掉用户排队的消息
    }
    if cs.asFile != nil {
        for _, e := range cs.asFile.GetByChatPool(cs.ID) {
            _ = cs.asFile.Delete(e.ID)   // ← 副作用:agent_sessions.json 删 entry
        }
    }
    cs.persistChatEntry()
    return nil
}
```

具体问题:

| 问题 | 后果 |
|------|------|
| **没真杀** | `cs.pool = make(...)` 抛弃 Go 指针,child CLI 进程孤儿化继续跑,占用 CPU/memory/PTY |
| **没走 bridge graceful** | `AgentSession.Close()`（`agentsession.go:449`）完全没被调;每个 bridge 自带的 graceful shutdown + SIGKILL 兜底路径浪费 |
| **副作用太多** | `InputBuffer.Clear()` 丢用户消息;`agent_sessions.json` entry 在进程 *未死* 时被删;`currentTurnUserMsgID` 清零 |
| **回复撒谎** | handler 报 "Killed 2 agent session(s)",但实际 child 没杀 |

### 2.2 当前 `/new` 的一个隐藏 bug

`internal/chatsession/chatsession.go:832-936` 的 `NewActiveAgentSessions` 对 dead/detached entry **silently skip**:

```go
if as.Status() != StatusRunning {
    // Not started → no conversation → skip silently.
    // Do NOT trigger a lazy spawn here (F-34 §6 Q-N4 / product
    // clarification 2026-08-04).
    continue
}
```

但是这个 entry 还在 pool 里,**它的 `ResumeID` 也没清**。下次消息触发 `LookupActiveAgentSession` → pool hit but Status=Exited → spawn with `as.ResumeID()` 的旧值 → Claude Code 桥拼 `--resume <旧id>` → **试图续接一个死 session**。这跟 `/new` 的"下一次 fresh"语义完全相反。

### 2.3 UX 不一致

旧 handler 只报 "Killed/Reset N":

```
Killed 2 agent session(s). Send a message to start fresh.
Reset 3 session(s). Send a message to start fresh.
```

用户无法知道:
- 哪些 agent 被处理了
- 哪些成功、哪些失败
- 死状态的 entry 是否清干净

### 2.4 设计目标

1. **`/kill` 真正 graceful kill**:每个 bridge 走自己的 Close() 路径,等进程真死,再清 disk。
2. **`/new` 对 dead state 也要有副作用**:清 ResumeID,让下次 spawn 必然 fresh。
3. **不动 slash command 边界外的 state**:InputBuffer、activeCwd、用户当前消息都不归 `/kill` 管。
4. **per-entry 列表回复**:每个 agent 一行,带 ✓ / ✗ / • 状态标记,失败带 error msg。

---

## 3. Concept

### 3.1 graceful shutdown 全貌

```
user /kill
  ↓
gateway.handleKill (handlers_chatsession.go)
  ↓
cs.KillAll() —— 改造后
  ├ snapshot pool(拷贝 Go 指针,不在原 map 上原地删)
  ├ 对每个 Running entry:
  │   as.Close()  ← 触发 bridge.Close()
  │     ├ claudecode: stdin EOF + SIGINT + 等 2s + SIGKILL 兜底
  │     ├ pi:         RPC shutdown + 等 2s + SIGKILL 兜底
  │     ├ acp:        session/close RPC(transport 不关)
  │     └ pty:        stdin EOF
  │   ObserveClose goroutine:events chan 关闭 → SetExited(0)
  ├ wg.Wait() ← 等所有 bridge 走完 graceful
  ├ 设 5s 整体 timeout —— bridge 内部 SIGKILL 兜底后这个 timeout 几乎不触发
  ├ activeAS = nil; currentTurnUserMsgID = ""
  ├ asFile.Delete(each entry) ← 进程死 *之后* 删 disk
  └ persistChatEntry()
```

### 3.2 `/new` 对 dead state 的"轻量 reset"

```
user /new
  ↓
gateway.handleNew (handlers_new.go)
  ↓
cs.NewActiveAgentSessions(ctx, agentName)
  ├ for each (cwd, [agent]) 匹配 entry:
  │   switch as.Status():
  │     case StatusRunning:
  │       pump 协调:StopReadPump → as.New(ctx, spawner) → StartReadPump
  │       matched++, reset++
  │
  │     case StatusDetached:
  │     case StatusExited:
  │       as.SetResumeID("")              ← 不 spawn!只清 ResumeID
  │       cs.asFile.Upsert(as.Entry())    ← 持久化清空的 ResumeID
  │       matched++, reset++
  │
  ├ InputBuffer.Clear()                  ← 不变:F-34 review #1
  └ return (matched, reset, results, firstErr)
```

### 3.3 列表式回复文案

详见 §5。

---

## 4. `/kill` Slash Command

### 4.1 不变量

| 触碰 | 不触碰 |
|------|--------|
| agent 进程(graceful shutdown) | `activeCwd` / `activeAgent` / `currentTurnUserMsgID` |
| pool entry 状态(Running → Exited) | InputBuffer 排队消息 |
| `agent_sessions.json` entry(**进程死 *之后***删除) | ChatSession 本身的 binding |
| `activeAS` 在 ChatSession 内的引用 | 其他 chat 状态 |

### 4.2 实现位置

`internal/chatsession/chatsession.go` —— `KillAll` 整体重写,新增 `KillResult` 类型 + `FormatKillResults` helper。

### 4.3 `KillResult` 类型

```go
// KillResult is one row of the /kill reply. It captures what
// happened to a single pool entry during KillAll so the handler
// can render a per-agent status instead of a bare count.
type KillResult struct {
    Agent       string  // "claude", "codex", ...
    Cwd         string  // "/code/A"
    BeforeState Status  // StatusRunning / Detached / Exited
    Action      string  // "killed" / "stale-cleared"
    Error       error   // nil for success
}
```

### 4.4 `KillAll` 新签名

```go
// Old: func (cs *ChatSession) KillAll() error
// New:
func (cs *ChatSession) KillAll() ([]KillResult, error)
```

返回 `[]KillResult` 让 handler 知道每条 entry 的命运。`error` 仍保留(整体性错误,比如 registry 损坏)。

### 4.5 实现

```go
// KillAll kicks every AgentSession in the pool out of the running
// state via each bridge's graceful Close() path. After all child
// processes have exited, their persistent entries are deleted from
// agent_sessions.json so the next spawn won't resume the dead
// sessions. The InputBuffer is left alone — the user's queued
// messages are not /kill's concern.
func (cs *ChatSession) KillAll() ([]KillResult, error) {
    // 1. snapshot pool under read lock; don't mutate pool until
    //    every bridge has confirmed shutdown.
    cs.mu.RLock()
    snapshot := make([]*AgentSession, 0, len(cs.pool))
    for _, as := range cs.pool {
        snapshot = append(snapshot, as)
    }
    cs.mu.RUnlock()

    results := make([]KillResult, 0, len(snapshot))

    // 2. fan out graceful shutdown. Each bridge drives its own
    //    shutdown sequence (stdin EOF + SIGINT, RPC close, etc.)
    //    with a SIGKILL fallback if the agent doesn't honor the
    //    graceful path within ~2s. We don't add a second
    //    escalation here — bridging the dial-in/wait dance would
    //    race with the bridge's local watchdog.
    var wg sync.WaitGroup
    for _, as := range snapshot {
        result := KillResult{
            Agent:       as.Agent,
            Cwd:         as.Cwd,
            BeforeState: as.Status(),
        }
        if as.Status() != StatusRunning {
            // Already dead or detached; no bridge to signal.
            result.Action = "stale-cleared"
        } else {
            result.Action = "killed"
            wg.Add(1)
            go func(as *AgentSession) {
                defer wg.Done()
                _ = as.Close()
            }(as)
        }
        results = append(results, result)
    }

    // 3. wait for all bridges to confirm exit. Bridge Close sets
    //    events chan closed; ObserveClose goroutine then flips
    //    as.Status to Exited (which wg.Wait correlates with
    //    by the bridge's own Wait).
    done := make(chan struct{})
    go func() { wg.Wait(); close(done) }()
    select {
    case <-done:
    case <-time.After(killGraceTotal):
        // Bridge's own watchdog should have SIGKILL'd by now.
        // If we still hit this, the child is wedged in a way
        // even SIGKILL can't fix (zombie / uninterruptible io).
        // Log and proceed — we still want to clean our state.
        log.Warn("killAll: graceful shutdown timeout", "limit", killGraceTotal)
    }

    // 4. wipe activeAS pointer BEFORE removing from disk so a
    //    follow-up message sees "no active" and goes through
    //    LookupActiveAgentSession -> spawn fresh.
    cs.mu.Lock()
    cs.activeAS = nil
    cs.currentTurnUserMsgID = ""
    cs.mu.Unlock()

    // 5. delete persistent entries. Now safe: child is dead (or
    //    was already dead), so any stale ResumeID would point to
    //    a corpse.
    if cs.asFile != nil {
        for _, as := range snapshot {
            _ = cs.asFile.Delete(as.ID)
        }
    }
    cs.persistChatEntry()
    return results, nil
}
```

`killGraceTotal` 建议 5s(比 bridge 内部 2s 多一倍,留 2 次 grace 重试 + SIGKILL 余量)。

### 4.6 与 `/kill` 旧版的差异总结

| 行为 | 旧 | 新 |
|------|----|----|
| 杀进程 | ❌ `cs.pool = make(...)` | ✅ `as.Close()` 走 bridge graceful |
| InputBuffer | ❌ `Clear()` | ✅ **保留** |
| `agent_sessions.json` | ❌ 进程死 *前* 删 | ✅ 进程死 *后* 删 |
| `currentTurnUserMsgID` | ❌ `= ""` | ✅ `= ""`(同) |
| `activeAS` | ❌ `nil` | ✅ `nil`(同) |
| 返回值 | `error` | `([]KillResult, error)` |
| 5s 整体 timeout | ❌ | ✅ 兜底 |

---

## 5. `/new` Slash Command

### 5.1 不变量

| 触碰 | 不触碰 |
|------|--------|
| pool entry.ResumeID(Running / Detached / Exited 都支持) | 进程本身 |
| `agent_sessions.json` entry.ResumeID 字段 | InputBuffer 排队消息 |
| matched / reset 计数 | 进程启动 |

### 5.2 `ResetResult` 类型

```go
type ResetResult struct {
    Agent       string
    Cwd         string
    BeforeState Status
    Action      string  // "in-place-reset" / "marked-fresh"
    Error       error
}
```

### 5.3 `NewActiveAgentSessions` 新签名

```go
// Old: func (cs *ChatSession) NewActiveAgentSessions(ctx context.Context, agentName string) (matched, reset int, firstErr error)
// New:
func (cs *ChatSession) NewActiveAgentSessions(ctx context.Context, agentName string) (matched, reset int, results []ResetResult, firstErr error)
```

`results` 包含所有 entry 的完整轨迹(`len(results) == matched`)。`matched` / `reset` 保留为简单 int,老调用点不依赖 `results` 也能用。

### 5.4 dead/detached 分支改造

`internal/chatsession/chatsession.go:850-855` 替换:

```go
// 改动前
if as.Status() != StatusRunning {
    // Not started → no conversation → skip silently.
    // Do NOT trigger a lazy spawn here (F-34 §6 Q-N4 / product
    // clarification 2026-08-04).
    continue
}
```

```go
// 改动后
if as.Status() != StatusRunning {
    // F-34 §6 Q-N4: do NOT trigger a lazy spawn for /new.
    // But the entry's stale ResumeID must not be replayed on the
    // next spawn — that would resurrect the dead session, defeating
    // the user's /new intent. Clear ResumeID in-memory + persist
    // so the next LookupActiveAgentSession spawns fresh.
    as.SetResumeID("")
    if cs.asFile != nil {
        _ = cs.asFile.Upsert(as.Entry())
    }
    matched++
    reset++
    continue
}
```

### 5.5 `/new` vs `/kill` 对比

| 维度 | `/kill` | `/new` |
|------|---------|--------|
| **目的** | 终止 agent 进程,清干净 | 重置对话上下文,下次 fresh |
| **进程状态** | `Running → Exited`(graceful) | `Running → Running`(in-place reset) / `Exited/Detached → Exited/Detached`(只清 ResumeID) |
| **是否 spawn** | ❌ 永不 spawn | ❌ 永不 spawn |
| **InputBuffer** | ✅ 保留(用户消息) | ❌ 清掉(旧对话的一部分) |
| `agent_sessions.json` | entry 删除(进程 dead 后) | entry.ResumeID 清空(entry 保留) |
| `currentTurnUserMsgID` | 清空 | 不动 |
| **下次 spawn** | fresh(无 ResumeID) | fresh(无 ResumeID) |
| **bridge 调用** | `as.Close()`(graceful) | `as.New(ctx, spawner)`(in-place);dead 分支 0 bridge 调用 |
| **强杀兜底** | bridge 内部 2s 后 SIGKILL | N/A |

---

## 6. 列表式回复文案

### 6.1 `/kill` 模板

**空 pool**:
```
No active agents to kill.
```

**全部成功**:
```
Stopped 2 agent session(s):
  ✓ claude @ /code/A
  ✓ codex @ /code/B
```

**部分失败**:
```
Stopped 1 agent session(s), 1 failed:
  ✓ claude @ /code/A
  ✗ pi @ /code/B — kill timeout after 5s
```

**所有 entry 已经死(state == Exited/Detached)**:
```
Cleared 2 stale agent session(s) (no live processes):
  • claude @ /code/A — already exited, entry cleaned
  • codex @ /code/B — already exited, entry cleaned
```

**混合 alive + 死**:
```
Stopped 1 agent session(s), 1 stale entry cleared:
  ✓ claude @ /code/A — killed
  • codex @ /code/B — already exited, entry cleaned
```

### 6.2 `/new` 模板

**全部 running + in-place reset OK**:
```
Reset 2 session(s):
  ✓ claude @ /code/A
  ✓ codex @ /code/A
```

**混合 running + dead**:
```
Reset 3 session(s):
  ✓ claude @ /code/A — reset in-place
  ✓ codex @ /code/B — already exited, marked fresh for next spawn
  ✓ pi @ /code/C — already exited, marked fresh for next spawn
```

**全部死**(纯标记 fresh):
```
Marked 2 session(s) fresh for next spawn:
  ✓ claude @ /code/A — already exited, ResumeID cleared
  ✓ codex @ /code/B — already exited, ResumeID cleared
```

**部分失败**(bridge.New 出错):
```
Reset 1 session(s), 1 failed:
  ✓ claude @ /code/A — reset in-place
  ✗ pi @ /code/B — bridge reset: <error>
```

### 6.3 图标选型

| 状态 | 图标 | 含义 |
|------|------|------|
| 成功 | `✓` | 真正发生了动作(killed / reset / cleared) |
| 失败 | `✗` | 出错,需要用户感知 |
| 跳过(已经是死状态) | `•` | 没有失败,但也没有"kill"这件事发生 —— 只是清理了 disk |

`✓` / `✗` 在 Feishu 普遍支持,`•` 是普通 bullet 不渲染问题。

### 6.4 排序

按 **"(成功 → 失败) → agent 名字 → cwd"** 排序:

```go
sort.SliceStable(results, func(i, j int) bool {
    // 失败组排后面
    if (results[i].Error != nil) != (results[j].Error != nil) {
        return results[j].Error != nil  // 无 err 的在前
    }
    if results[i].Agent != results[j].Agent {
        return results[i].Agent < results[j].Agent
    }
    return results[i].Cwd < results[j].Cwd
})
```

### 6.5 长度限制

Feishu 单条 4KB 限制。典型 pool 量(< 10)远远低于这个限制。**防御性截断**:

```go
const maxResultLines = 20
if len(lines) > maxResultLines {
    lines = lines[:maxResultLines]
    lines = append(lines, fmt.Sprintf("  ... and %d more", len(results)-maxResultLines))
}
```

### 6.6 `FormatKillResults` / `FormatResetResults` helper

放在 `internal/chatsession/chatsession.go` 末尾,handler 调用即可:

```go
// FormatKillResults produces a human-readable summary suitable for
// channel.Send. Caller passes the results slice from KillAll;
// FormatKillResults handles the per-state branching.
func FormatKillResults(results []KillResult) string {
    if len(results) == 0 {
        return "No active agents to kill."
    }

    var killed, stale, failed int
    lines := make([]string, 0, len(results))

    for _, r := range results {
        if r.Error != nil {
            failed++
            lines = append(lines, fmt.Sprintf("  ✗ %s @ %s — %s: %v",
                r.Agent, r.Cwd, humanAction(r.Action), r.Error))
            continue
        }
        switch r.Action {
        case "killed":
            killed++
            lines = append(lines, fmt.Sprintf("  ✓ %s @ %s", r.Agent, r.Cwd))
        case "stale-cleared":
            stale++
            lines = append(lines, fmt.Sprintf("  • %s @ %s — already exited, entry cleaned",
                r.Agent, r.Cwd))
        }
    }

    sort.Strings(lines)  // 简单按字符串 sort,保持稳定

    header := buildKillHeader(killed, stale, failed)
    return header + "\n" + strings.Join(lines, "\n")
}

func buildKillHeader(killed, stale, failed int) string {
    if failed == 0 && stale == 0 {
        return fmt.Sprintf("Stopped %d agent session(s):", killed)
    }
    if killed == 0 && stale > 0 && failed == 0 {
        return fmt.Sprintf("Cleared %d stale agent session(s) (no live processes):", stale)
    }
    parts := []string{}
    if killed > 0 {
        parts = append(parts, fmt.Sprintf("Stopped %d", killed))
    }
    if stale > 0 {
        parts = append(parts, fmt.Sprintf("%d stale entry cleared", stale))
    }
    if failed > 0 {
        parts = append(parts, fmt.Sprintf("%d failed", failed))
    }
    return strings.Join(parts, ", ") + ":"
}
```

`FormatResetResults` 同结构,差别只在 `Action` 字符串(`"in-place-reset"` / `"marked-fresh"`)和对应模板。

---

## 7. 不变式 checklist

- [ ] `/kill` **永不动** `activeCwd` / `activeAgent` / `currentTurnUserMsgID` / `InputBuffer`
- [ ] `/kill` 调 `as.Close()` 而不是 `cs.pool = make(...)` 直接丢指针
- [ ] `/kill` 删 `agent_sessions.json` entry 必须在 bridge 关闭 *之后*
- [ ] `/kill` 整体 timeout 5s 是兜底,bridge 内部 2s 已 SIGKILL,几乎不触发
- [ ] `/new` 对 dead/detached 也清 `ResumeID`,不 silently skip
- [ ] `/new` 不 spawn 任何 dead agent
- [ ] `/new` **不动** `currentTurnUserMsgID`(下条消息自然重新锚)
- [ ] `/new` 仍然 `InputBuffer.Clear()`(F-34 review #1 不变)
- [ ] handler 报 per-entry 列表,不是 `count`
- [ ] bridge 自治 graceful shutdown,nightme 不二次升级

---

## 8. 错误处理矩阵

### 8.1 `/kill`

| 场景 | 行为 |
|------|------|
| pool 空 | "No active agents to kill." |
| 所有 Running 的 entry 都 graceful OK | `Stopped N agent session(s):` + 每行 ✓ |
| bridge.Close 内部 SIGKILL 兜底触发 | `Stopped N...` (outcome 一样,error msg 由 bridge log) |
| bridge 整体 timeout 5s | `log.Warn` + 继续清 disk,返回 best-effort 结果 |
| 某 entry bridge.Close() 报错 | 单行 `✗ ... — error: <msg>`;其他 entry 继续 |
| registry 写失败 | `KillAll` 返回整体 error,handler `reply "Kill failed: ..."` |

### 8.2 `/new`

| 场景 | 行为 |
|------|------|
| pool 空 | `Matched 0 sessions.`(沿用 F-34) |
| 全部 Running,bridge.New OK | `Reset N session(s):` + 每行 `✓ reset in-place` |
| 全部 dead | `Marked N session(s) fresh for next spawn:` + 每行 `✓ already exited, ResumeID cleared` |
| 混合 | `Reset N session(s):` + mixed |
| bridge.New报错 | 单行 `✗ ... — bridge reset: <error>`;其他 entry 继续 |
| InputBuffer 已空 | `Clear()` no-op,handler 仍 assert |

---

## 9. 测试计划

### 9.1 `internal/chatsession/kill_test.go`(新建)

```
TestKillAll_GracefulShutdown
  - spawn fake agent
  - 调 KillAll
  - 断言:fake agent.Close() 被调
  - 断言:fake agent Events() 关闭后 SetExited(0) 触发
  - 断言:fake agent 没收到 SIGKILL(graceful 路径直接通过)

TestKillAll_GracefulTimesOut_BridgeEscalates
  - spawn fake agent,Close() hang > 2s 模拟 grace 失败
  - bridge 内部 watchdog 升级到 SIGKILL
  - 调 KillAll,设 killGraceTotal 略高于 2s
  - 断言:超时 5s 内 SIGKILL 已发出,nightme 端不二次升级

TestKillAll_InputBufferPreserved
  - 排队 3 条
  - 调 KillAll
  - 断言:inputBuffer.Len() == 3

TestKillAll_AgentSessionEntriesDeleted
  - spawn 2 个 agent → /cwd → spawn 2 个
  - 调 KillAll
  - 断言:agent_sessions.json 里这 4 个 entry 全删
  - 断言:pool entry 状态 Exited(ObserveClose 触发)

TestKillAll_ActiveASCleared
  - 调 KillAll
  - 断言:cs.ActiveAgentSession() == nil
  - 断言:cs.currentTurnUserMsgID == ""

TestKillAll_OnlyExitedEntries
  - mock pool 全是 Status=Exited(无进程)
  - 调 KillAll
  - 断言:每条 result.Action == "stale-cleared"
  - 断言:无 bridge.Close 调用
  - 断言:FormatKillResults 输出 "Cleared N stale agent session(s)"

TestKillAll_ResultsSortedStable
  - 3 个 entry,(success, failure, success) 顺序构造
  - 调 KillAll
  - 断言:results 已按 (失败在后) → agent → cwd 排序
```

### 9.2 `internal/chatsession/new_test.go`(扩展)

```
TestNewActiveAgentSessions_ClearsResumeIDForExited
  - pool 有 (claude, /A) Status=Exited,ResumeID="old-id"
  - 调 NewActiveAgentSessions
  - 断言:as.ResumeID() == ""
  - 断言:agent_sessions.json entry.ResumeID == ""
  - 断言:matched == 1, reset == 1
  - 断言:Handle() == nil(没动进程)

TestNewActiveAgentSessions_ClearsResumeIDForDetached
  - 同上,Status=Detached(daemon restart 后状态)

TestNewActiveAgentSessions_DoesNotSpawn
  - Spawner.Spawn 加 spy
  - 调 NewActiveAgentSessions 命中 Exited entry
  - 断言:Spawner.Spawn 未被调

TestNewActiveAgentSessions_Running_HitsInPlaceReset
  - spawn 1 个 Running
  - 调 NewActiveAgentSessions
  - 断言:as.Handle().New() 被调(bridge reset)
  - 断言:as.Status() == StatusRunning(原进程没死)

TestNewActiveAgentSessions_BufferClearedEvenIfMatched0
  - 排队 2 条
  - 调 NewActiveAgentSessions 没有 Running entry
  - 断言:inputBuffer.Len() == 0
```

### 9.3 `internal/chatsession/format_test.go`(新建)

```
TestFormatKillResults_Empty
  - 输入:[]KillResult{}
  - 输出:"No active agents to kill."

TestFormatKillResults_AllKilled
  - 2 个 KILLED
  - 输出:含 "Stopped 2" + 2 行 ✓

TestFormatKillResults_AllStale
  - 2 个 STALE-CLEARED
  - 输出:含 "Cleared 2 stale" + 2 行 •

TestFormatKillResults_Mixed
  - 1 killed + 1 stale + 1 failed
  - 输出:含 "Stopped 1... 1 stale entry cleared, 1 failed:" + 3 行

TestFormatKillResults_SortedSuccessFirst
  - 手动构造结果,failure 在 success 前
  - 输出:success 排在前面(✓ ... ✓ ... ✗ ...)

TestFormatKillResults_LongListTruncated
  - 25 个 entry
  - 输出:首 20 行 + "  ... and 5 more"

TestFormatResetResults_AllRunning
  - 2 个 in-place-reset
  - 输出:"Reset 2 session(s):" + 2 行 ✓

TestFormatResetResults_AllDead
  - 2 个 marked-fresh
  - 输出:"Marked 2 session(s) fresh for next spawn:" + 2 行 ✓
```

---

## 10. 不在范围内（Out of Scope）

- **🟢 改 bridge.Close() 实现**:本次只调用现有 Close(),不动 bridge 自己的 graceful shutdown 时序(2s grace + SIGKILL)。
- **🟢 改 `/cwd` / `/use` 语义**:已经是正确的不动 agent 进程,继续保留。
- **🟢 改 Feishu card 渲染**:先用 plain text,等用户反馈再升级 Card 2.0 富文本。
- **🟢 引入 `StatusDraining` 中间态**:当前依赖 `ObserveClose` 异步感知 entries 死亡,wg.Wait + bridge 内部 2s SIGKILL 兜底足够。
- **🟢 `/kill <agent>` 细粒度**:本次先做"全 pool kill",agent 子集过滤可以下一个 PR。

---

## 11. 决策记录

| # | 决策 | 结论 |
|---|------|------|
| D-1 | `/kill` 是否清 InputBuffer | **否**。用户消息不属于 `/kill` 的语义边界。 |
| D-2 | `/kill` 是否清 `currentTurnUserMsgID` | **是**。下一条消息该重新锚。 |
| D-3 | `/kill` 是否做 SIGKILL 兜底 | **不**。bridge 内部 2s 后已 SIGKILL,nightme 端做会和 bridge 抢信号。 |
| D-4 | `/kill` 整体 timeout | **5s**。比 bridge 内部 2s 多一倍,留 SIGKILL 余量。 |
| D-5 | `/new` 是否 spawn dead agent | **否**。只清 ResumeID。 |
| D-6 | `/new` 是否清 `currentTurnUserMsgID` | **否**。下条消息自然重新锚。 |
| D-7 | `/new` 是否清 InputBuffer | **是**。沿用 F-34 review #1。 |
| D-8 | `/new` 对 dead entry 是否静默 | **否**。返回 per-entry result,带 `marked-fresh` Action。 |
| D-9 | `/kill` / `/new` 报告形式 | **per-entry 列表**,带 `✓` / `✗` / `•` 标记。 |
| D-10 | 报告格式 | **plain text**(后续可升级 Card 2.0)。 |
| D-11 | 长度截断 | **20 行 + "... and N more"**。 |
| D-12 | 报告用词 | **"Stopped" / "Reset" / "Cleared" / "Marked fresh"**。"kill" 实现是 graceful,不用 "killed"。 |

---

## 12. 实施清单

### 12.1 `internal/chatsession/chatsession.go`

- [ ] 新增 `KillResult` struct(Agent, Cwd, BeforeState, Action, Error)
- [ ] 新增 `ResetResult` struct(Agent, Cwd, BeforeState, Action, Error)
- [ ] `KillAll` 改签名 `() ([]KillResult, error)`,实现 graceful + 5s timeout + 后置删 disk
- [ ] `NewActiveAgentSessions` 改签名 `(...) (matched, reset int, results []ResetResult, firstErr error)`,dead/detached 分支清 ResumeID + 持久化
- [ ] 新增 `FormatKillResults` helper
- [ ] 新增 `FormatResetResults` helper
- [ ] 新增 `killGraceTotal = 5 * time.Second` 常量

### 12.2 `internal/gateway/handlers_chatsession.go`

- [ ] `handleKill` 改用 `FormatKillResults`

### 12.3 `internal/gateway/handlers_new.go`

- [ ] `handleNew` 改用 `FormatResetResults`

### 12.4 测试

- [ ] `internal/chatsession/kill_test.go`(新建,7 个 case)
- [ ] `internal/chatsession/new_test.go`(扩展,5 个新 case)
- [ ] `internal/chatsession/format_test.go`(新建,8 个 case)

### 12.5 文档

- [ ] SPEC.md §3.2 状态转换触发器表更新(`/kill` 行注明走 graceful)
- [ ] F-34 §6 错误处理矩阵加 dead/detached 的 result 行
- [ ] F-34 README linking 加 F-42

### 12.6 估计工作量

| 类别 | 行数 |
|------|------|
| `chatsession.go` 改动 | ~120 |
| handler 改动 | ~10 |
| tests | ~330 |
| 文档 | ~80 |
| **合计** | ~540 |

---

## 13. 风险与回滚

| 风险 | 缓解 |
|------|------|
| Bridge `Close()` hang 超过 5s 导致 `/kill` UX 慢 | `killGraceTotal` 可调;bridge 内部 2s 已 SIGKILL |
| `SetResumeID("")` + 持久化 race(其它 goroutine 同时 `LookupActiveAgentSession`) | entry 复用已有 `resumeIDMu`,Upsert 是 atomic write |
| `/kill` 保留 InputBuffer 但用户期望清 | 设计决策(D-1);若用户反馈,加 `/clear-buffer` 单独命令 |
| 旧 `agent_sessions.json` 没 ResumeID 字段 | GO JSON 容忍,不破坏现有数据 |

**回滚方案**:改动都在 `internal/chatsession/` 内部,git revert 那个 commit 即可,不涉及 schema 或 wire format。

---

## 14. 后续 PR(不在本次)

- [ ] `/kill <agent>` agent 子集过滤
- [ ] 升级 Feishu Card 2.0 富文本渲染(目前 plain text)
- [ ] per-entry 操作耗时显示(`killed (1.2s)`)
- [ ] 长结果 list 折叠(accordion)
