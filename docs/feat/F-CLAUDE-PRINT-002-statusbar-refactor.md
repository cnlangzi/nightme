# F-CLAUDE-PRINT-002 — StatusBar 装配重构

> F-CLAUDE-PRINT-001 的后续。原本"在 runtime 集中装配 StatusBar"的设计
> 经过多轮 review 后被推翻,本文件记录推翻的理由 + 最终形态。

## 背景

F-CLAUDE-PRINT-001 把 `claudecode.Starter.RunOnce` 改成了 print-mode 实现,并把
`RunOnce` 的返回值从 `(string, error)` 改成了 `(RunResult, error)`,携带 Model /
Usage / DurationMs / Subtype。

跑完真机测试后,review 阶段发现 StatusBar 装配链路不顺,需要重做。
**本文件就是这次重做的设计记录**。

## 当前实现的问题

### OutboundMessage 已有字段

```go
// internal/messages/outbound.go
type OutboundMessage struct {
    SessionID string        // ← 已经存在
    Model     string        // ← 已经存在
    AgentName string        // ← 已经存在
    Workspace string        // ← 已经存在
    Branch    string        // ← 已经存在
    Usage     *agent.UsageInfo  // ← 已经存在
    Result    *agent.AgentResultEvent
    Text      string
    ...
    StatusBar *StatusBar   // ← wrapper
}
```

### 重复的 wrapper

```go
// internal/messages/session.go
type StatusBar struct {
    GitBar   *GitStatusBar    // ← 真的新增数据(Workspace / GitStatus / PR)
    AgentBar *AgentStatusBar  // ← 复制 out.{AgentName, Model, SessionID}
    UsageBar *UsageStatusBar  // ← 包装 out.Usage
}

type AgentStatusBar struct { Agent, Model, SessionID string }   // 纯 dupe
type UsageStatusBar struct { *UsageInfo }                       // 纯 dupe
```

`AgentStatusBar` 和 `UsageStatusBar` 完全是 `OutboundMessage` 已有字段的复制品。

### StatusBar 在 3 处被赋值

| 位置 | 调用方 | 行为 |
|---|---|---|
| `internal/statusbar/stamp.go:33` (`StampFromAS`) | `runtime/policy.go:100` + `eventbus.go:160` | 总是覆盖 |
| `internal/statusbar/stamp.go:78` (`AttachIfMissing`) | `gateway/outbound/outbound.go:98, 103` | 跳过 if pre-filled |
| (直接字段) | (none) | 没有直接 set 的地方 |

### StampFromAS 的乱越界

```go
// internal/statusbar/stamp.go
func StampFromAS(out *OutboundMessage, s *AgentSession, deps Deps) {
    out.StatusBar = Build(
        s.Agent,         // ← 来自 AgentSession
        s.Model(),       // ← 来自 AgentSession (内部 RLock)
        s.SessionID(),   // ← 来自 AgentSession (内部 RLock)
        s.Cwd,           // ← 来自 AgentSession ← 概念错位
        out.Usage,       // ← 来自 OutboundMessage
        deps,
    )
}
```

`AgentSession` 同时持有:
- `Agent` / `Model` / `SessionID` —— agent 身份(per-AS 粘性)
- `Cwd` —— workspace(chatsession 级别,不是 agent 级别)

**混在一起导致 chatsession 共享的 workspace state 被错误地放进了 per-agent 的对象**。

### OneShot 场景的 bug

```
[/gtw commit dispatcher]
   result := a.RunOnce(...)
       → result.Model = "haiku-4-5"   (one-shot 用的)
   reply(ctx, em, chatID, messageID, card)
       → OutboundMessage{Text: card}   无 StatusBar
   ↓
[em.Send] AttachIfMissing
   source(chatID) → chatsession 的 selected AS
   Build("claude", "opus-4-5", "chatsess-456", ...)   ← 用错
   ↑↑↑ 应该用 result.Model / result.SessionID,不是 chatsession 的
```

## 推翻前几轮的设计

### ❌ 加 `OutboundMessage.OneShotResult` 字段

第一版提议:加一个 `*RunResult` 字段,StampFromAS 看它存在就 override。

**问题**:既然字段是同一个 `RunResult` 形状,为什么要包一层?直接写
`out.Model` / `out.SessionID` 就行。

### ❌ 统一入口 `Stamp(agent, model, sessionID, cwd, deps)`

第二版提议:抽一个 4-string 的纯函数。

**问题**:仍然是"runtime 集中装配"的反模式。每条数据在产生它的层填
就够了,不需要 runtime 做第二次组装。

### ❌ `Build(agent, model, sessionID string, usage, deps)`

第三版提议:把 `*AgentSession` 拆成原语。

**问题**:同样的反模式——runtime 不该是数据填入点。

### ❌ `AttachGitStatus`

第四版提议:在 `em.Send` 处贴 `cs.GitStatus()` 到 `out.GitStatus`。

**问题**:chatsession 已经是数据的所有者,贴指针这一步也是多余的。直接在
chatsession 回调里 `out.GitStatus = cs.GitStatus()` 一行就完事。

### ❌ `AttachIfMissing` 兜底

第五版提议:保留 AttachIfMissing 作为"如果上游忘填就偷偷补"。

**问题**:**反模式**。如果消息到达 `em.Send` 但字段是 nil,说明 producer 有 bug。
runtime 偷偷补会掩盖这个 bug。应该让 producer 负责。

## 最终设计

### 核心原则

> **"在哪产生在哪填"** ——每条数据由产生它的层填到 `OutboundMessage`,runtime
> 不做任何字段填充。

### 数据流

```
[bridge event]
   AgentEvent{Result, Usage, Model, SessionID, ...}
   ↓
[chatsession.AgentEventBus 回调]    ← producer 1 + 2 一站式
   out := gateway.Translate(ev)     ← 填 per-event 字段
   out.GitStatus = cs.GitStatus()   ← chatsession 缓存
   ↓
[emitter.Send]                     ← 纯运输
   ↓
[Channel.Send]
   formatStatusBarLines(out)        ← 直接读字段
```

### OutboundMessage 形状(平铺)

```go
// internal/messages/outbound.go
type OutboundMessage struct {
    // Routing
    ChatID  string
    Kind    OutboundKind
    ReplyTo string

    // Body (per-event,by translate)
    Text         string
    Card         *Card
    Tool         *ToolInfo
    TaskList     *agent.AgentTaskListEvent
    Result       *agent.AgentResultEvent
    Usage        *agent.UsageInfo
    MessageState *MessageStatePayload

    // Per-event identity (by translate,from AgentEvent)
    AgentName  string
    Model      string
    SessionID  string

    // Per-event workspace (by translate,from AgentEvent)
    Workspace  string
    Branch     string

    // Per-chatsession workspace context (by chatsession 回调)
    GitStatus  *GitStatus

    // Error indicator
    Err        error
}
```

**没有 `StatusBar` wrapper**。`StatusBar` / `AgentStatusBar` / `UsageStatusBar`
三个 struct 全部删除。`GitStatusBar` 改名 `GitStatus` 并直接挂到 OutboundMessage。

### 删掉的 struct / 文件

| 删 | 原因 |
|---|---|
| `StatusBar` | wrapper,字段都已在 OutboundMessage |
| `AgentStatusBar` | 纯复制 out.{AgentName, Model, SessionID} |
| `UsageStatusBar` | 纯包装 out.Usage |
| `internal/statusbar/build.go` | 整个文件删 |
| `internal/statusbar/stamp.go` | 整个文件删 |
| `internal/statusbar/runtime.go` | 整个文件删 |
| `internal/statusbar/*_test.go` | 跟着删 |

**`internal/statusbar` 整个包消失**。

### 保留的

| 保留 | 原因 |
|---|---|
| `GitStatus` struct(`internal/messages/session.go`) | 真的携带 workspace 状态(GitStatusSnapshot + PR),不是 wrapper |
| `AgentEvent` 各种 event payload | bridge 产生,translate 翻译 |

### chatsession 改动

```go
// internal/chatsession/chatsession.go

type ChatSession struct {
    ...existing fields...

    // GitStatus 是 chatsession 的 workspace 状态快照。
    // RefreshGitStatus() 主动刷新(启动时、/gtw commit 前后等)。
    // 粘性缓存:普通 chat turn 期间不刷。
    gitStatus   *messages.GitStatus
    gitStatusMu sync.RWMutex
}

// GitStatus 读缓存。
func (cs *ChatSession) GitStatus() *messages.GitStatus {
    cs.gitStatusMu.RLock()
    defer cs.gitStatusMu.RUnlock()
    return cs.gitStatus
}

// RefreshGitStatus 主动跑 git 状态 + pr lookup,刷新缓存。
// 触发点:启动时、/gtw commit 前后、/gtw pr 前后。
func (cs *ChatSession) RefreshGitStatus(ctx context.Context, deps statusbar.Deps) error {
    cwd := cs.SelectedCwd()
    snap, _ := deps.CollectGit(ctx, cwd)
    var pr *messages.PR
    if deps.LookupPR != nil {
        pr = deps.LookupPR(cs.SelectedAgentSessionID())
    }
    cs.gitStatusMu.Lock()
    cs.gitStatus = &messages.GitStatus{
        Workspace:   cwd,
        Snapshot:    snap,
        PullRequest: pr,
    }
    cs.gitStatusMu.Unlock()
    return nil
}
```

### 各路径的填法

| 路径 | 谁填什么 |
|---|---|
| **Chat turn**(bridge event) | `gateway.Translate` 填 per-event 字段;chatsession 回调填 `out.GitStatus` |
| **Slash command reply**(经 `reply()` helper) | helper 内部读 `cs.GitStatus()` 填到 `out.GitStatus` |
| **One-shot dispatch**(`/gtw commit` / `/gtw pr`) | Dispatcher 直接构造 `OutboundMessage`,填 `out.{Model, SessionID, Usage, AgentName, Workspace, Branch, GitStatus}` |

### 删掉的函数

| 函数 | 状态 |
|---|---|
| `statusbar.Build` | 删(再没人调) |
| `statusbar.StampFromAS` | 删 |
| `statusbar.AttachIfMissing` | 删(反模式) |
| `statusbar.NewRuntimeSource` / `RuntimeSource` | 删 |
| `runtime.StatusBarStampPolicy` | 删 |

### chatsession 事件回调的实现

```go
// internal/chatsession/manager.go (or runtime/handler.go)

cs.AgentEventBus.Subscribe(func(env AgentEventEnvelope) bool {
    out := gateway.Translate(env.Event, env)  // 填 per-event 字段
    
    // Chatsession 上下文:workspace + git 状态
    out.GitStatus = cs.GitStatus()
    
    // Apply remaining policies(其他策略保留,不再有 StatusBarStampPolicy)
    for _, p := range policies {
        if p.Apply(&out, env) { return false }
    }
    
    if err := em.Send(ctx, out); err != nil { ... }
    return false
})
```

### One-shot dispatcher 的实现

```go
// internal/command/gtw/commit.go

result, err := a.RunOnce(ctx, cfg, blocks)
if err != nil { return agentName, fmt.Sprintf("❌ agent %s failed: %v", ...) }

// RunOnce 后:刷新 git 状态(commit 已经落地)
if err := cs.RefreshGitStatus(ctx, sbDeps); err != nil { ... }

out := messages.OutboundMessage{
    ChatID:    chatID,
    Kind:      messages.OutReply,
    ReplyTo:   messageID,
    Text:      card,
    Model:     result.Model,         // ← 来自 RunResult
    SessionID: result.SessionID,     // ← 来自 RunResult
    Usage:     result.Usage,         // ← 来自 RunResult
    AgentName: s.Agent,
    Workspace: s.Cwd,
    Branch:    s.Branch,
    GitStatus: cs.GitStatus(),      // ← 来自刚刷新的 cs
}
em.Send(ctx, out)
return agentName, ""
```

### Renderer 改写

```go
// internal/channel/feishu/usage_footer.go
func formatStatusBarLines(msg *messages.OutboundMessage) []string {
    var lines []string

    // Line 1: identity — 直接读 msg 字段
    if msg.AgentName != "" || msg.Model != "" || msg.SessionID != "" {
        idParts := []string{"🤖:"}
        if msg.AgentName != "" {
            idParts = append(idParts, msg.AgentName)
        }
        if msg.Model != "" {
            idParts = append(idParts, "·", msg.Model)
        }
        if msg.SessionID != "" {
            idParts = append(idParts, "·", msg.SessionID)
        }
        lines = append(lines, strings.Join(idParts, " "))
    }

    // Line 2: usage — 直接读 msg.Usage
    if msg.Usage != nil {
        lines = append(lines, formatUsageLine(msg.Usage))
    }

    // Line 3: git — 直接读 msg.GitStatus
    if msg.GitStatus != nil {
        lines = append(lines, formatGitLine(msg.GitStatus))
    }

    return lines
}
```

## 改动清单(按依赖顺序)

### 第 1 批:消息层 + chatsession

1. **`internal/messages/session.go`** — 删 `StatusBar` / `AgentStatusBar` / `UsageStatusBar`,
   `GitStatusBar` 改名 `GitStatus`(`GitStatus` 字段名 `Snapshot`)。**约 -60 / +5 行**
2. **`internal/messages/outbound.go`** — `OutboundMessage.StatusBar` 改名为 `GitStatus`,
   类型 `*GitStatus`。**约 -1 / +1 行**
3. **`internal/chatsession/chatsession.go`** — 加 `gitStatus` 字段 + `GitStatus()` /
   `RefreshGitStatus()` 方法。**约 +50 行**

### 第 2 批:chatsession 启动 + 事件回调

4. **`internal/chatsession/manager.go`** — 新建 chat session 时调
   `cs.RefreshGitStatus()` 一次。**约 +5 行**
5. **`internal/runtime/handler.go`** — 在 bridge 事件回调里,`translate` 之后
   加 `out.GitStatus = cs.GitStatus()`。**约 +1 行**

### 第 3 批:删除 statusbar 包

6. **`git rm internal/statusbar/`** — 整个目录删除
7. **`internal/runtime/policy.go`** — `DefaultPolicies` 不再含 `StatusBarStampPolicy`,
   删 `StatusBarStampPolicy` 函数。**约 -20 行**
8. **`internal/runtime/eventbus.go`** — `MessageStateBus` 改用 `chatsession.GitStatus()`
   填 `out.GitStatus`。**约 -10 / +5 行**
9. **`internal/gateway/outbound/outbound.go`** — 删 `AttachIfMissing` 调用,emitImpl
   只做"out → channel"纯运输。**约 -10 行**
10. **所有引用 `statusbar` 包的 caller** — 删 import,改用字段直接读

### 第 4 批:dispatcher 路径

11. **`internal/command/gtw/commit.go`** — 构造 `OutboundMessage` 自填字段,
    绕开 `reply()` helper。**约 +15 / -3 行**
12. **`internal/command/gtw/pr.go`** — 同上。**约 +15 / -3 行**

### 第 5 批:渲染 + 测试

13. **`internal/channel/feishu/usage_footer.go`** — `formatStatusBarLines` 直接读
    `out.{Model, SessionID, AgentName, Usage, GitStatus}`,无 StatusBar wrapper。
    **约 -30 / +30 行**
14. **测试更新** — 删 `StatusBar` 相关断言,改读 `out.GitStatus` / `out.Model` 等。
    **约 -100 / +50 行**

## 整体规模

| 方向 | 行数 |
|---|---|
| 删除 | ~250 行(整个 statusbar 包 + 多处 policy) |
| 新增 | ~180 行(chatsession GitStatus + 回调 + dispatcher + renderer 改写) |
| **净** | **-70 行** |

代码量**不增反减**——消除了"statusbar framework"这个抽象层。

## 不在本次范围

1. **`AgentSession.Cwd` 移出到 `ChatSession`** — 长期单独 PR
2. **GitStatus 缓存的 TTL / 失效策略** — 当前"启动刷一次 + dispatcher 主动刷"够用;
   如果后续 turn 内 git 状态变化频繁,加个短 TTL
3. **`internal/statusbar` 包可能其他 caller** — 已经全仓 grep 过,只 nightme 内部用
4. **one-shot 的 Model 覆盖是否需要"和 chatsession model 相同时跳过"** — 当前
   不做,让 renderer 自由处理(若同色显示反而冗余,后续可以加)

## 风险

| 风险 | 缓解 |
|---|---|
| `chatsession.GitStatus()` 在某些路径返回 nil | Channel 渲染 nil-safe,跳过 Line 3 |
| 多个 outbound event 在同一 turn,GitStatus 是缓存(stale) | 接受 stale(turn 内 git 通常不变);如需新鲜,dipatcher 自己 `RefreshGitStatus()` |
| `internal/statusbar` 包消失影响外部 import | 全仓 grep 过,只有 nightme 内部用,无外部依赖 |
| `AgentSession.Cwd` 还在(长期应该移出) | 这次**不动**;commit/pr 路径继续从 `s.Cwd` 读 |
| 现有 `*_realpi_test.go` 拿 `RunResult{}.Text` 会不会被 `out.Model = result.Model` 影响 | 不影响,Text 字段保留;只改 AgentBar / UsageBar 装配逻辑 |
| 测试覆盖度 | 第 5 批详细更新 mock tests + 加一个新的 end-to-end 测试覆盖 one-shot → out.GitStatus → Channel 渲染 |

## 决策记录

| 时间 | 提案 | 推翻原因 |
|---|---|---|
| Round 1 | 加 `OutboundMessage.OneShotResult *RunResult` 字段 | 既然字段是 RunResult 形状,直接写 msg 字段就行,不需要 wrapper |
| Round 2 | 统一入口 `Stamp(agent, model, sessionID, cwd, deps)` | 仍然是"runtime 集中装配"反模式 |
| Round 3 | `Build(agent, model, sessionID string, usage, deps)` 原语化 | 同上,Stamp 概念是多余的 |
| Round 4 | `AttachGitStatus` 在 runtime egress 贴指针 | chatsession 已经是数据的所有者,直接 chatsession 赋就行 |
| Round 5 | `AttachIfMissing` 兜底逻辑 | 反模式——掩盖 bug,应该让 producer 负责 |

## 工作量估计

**约 2-3 小时**:
- 1 小时:消息层 + chatsession(第 1 批)+ 事件回调(第 2 批)
- 30 分钟:删 statusbar 包(第 3 批)
- 30 分钟:dispatcher 路径(第 4 批)
- 30-60 分钟:renderer + 测试(第 5 批)

## 拍板

需确认:

1. **范围**:整个 `internal/statusbar` 包删除 + OutboundMessage 平铺 + 4 个路径都改?
2. **Cwd 移出 AS**:这次不做,长期单开?
3. **slash command reply 的 GitStatus 填法**:helper 内部直接读 cs,还是别的方案?
4. **测试策略**:先改 mock test 反映新数据流,真机测试在最后?

如果 4 点都确认,直接动手。
---

## 后续:去掉 CS 层 GitStatus 缓存 (`fix-gitstatus` 分支)

本设计第 1 批「chatsession 持有缓存」在落地后被进一步简化 —— 当前实现**没有** per-chat GitStatus 缓存层。具体差异:

| 项 | 本文档原设计 | 当前 (`fix-gitstatus` 分支) |
|---|---|---|
| ChatSession 上的 gitStatus 字段 | `*messages.GitStatus` 缓存 | **删除** |
| ChatSession 上的 gitStatusMu | `sync.RWMutex` 串行化 cache 读写 | **删除** |
| ChatSession.RefreshGitStatus | 公共写入接口 (SetSelectedCwd / /gtw commit / /gtw pr 主动刷) | **删除** |
| GitStatusDeps.RefreshPR | 触发后台 PR 刷新 | **删除**(无人调用,PR invalidation 走 prcache.Cache.Invalidate) |
| `ChatSession.GitStatus(ctx)` 行为 | RLock 读 cache,miss 时同步 3s refresh | **每次现采**(CollectGit + LookupPR 都是同步读,无状态) |
| 主动 refresh 入口 | SetSelectedCwd / ClearSelectedCwd / /gtw commit pre-post / /gtw pr | **全部移除**:不再需要 cache 失效 |

### 为什么去掉缓存

footer 反映的是用户工作区「刚改完的文件」,而 cs 层 cache 在两次 turn 之间给的是 stale 视图,与 footer 的语义错位。PR 仍然由 `prcache.Cache` 单独缓存(60s TTL + 后台 refresh),因为 `gh/glab` API round-trip 是贵的;本地 `git status` 不贵(~25-35ms),实时读即可。

### 性能权衡

每次 send 都跑一次 `git status --porcelain --branch --untracked-files=normal`。30-event turn 在 readpump 串行 goroutine 上触发 30 次 git 子进程,wall-time ≈ 30× 单次 git。对本仓库量级可忽略;大仓库若发现性能回退,后续可以:

- 把 `gtw.CollectReadiness` 改用 `--untracked-files=no`(砍 untracked 扫描,大仓库可省 60-90% 时间)
- 或在 cs 层加 inflight fan-in(只在「N 个 goroutine 同时 stamp 同 chat」时复用同一次 git 的 inflight 结果;串行 readpump 不受益)

本变更没引入 inflight;commit 时记录这个 follow-up。

---

## 后续:2026-08-22 plan-D 重生 `internal/statusbar` 包(telegram StatusBar 全量贴附)

本文档第 1-3 批描述的"**`internal/statusbar` 整个包消失**"仍然成立 —— 旧的 `stamp.go` / `build.go` / `runtime.go` 已经被删,`OutboundMessage` 直接承载 flat 字段(AgentName / Model / SessionID / Usage / GitStatus)。

但 2026-08-22 plan-D(telegram StatusBar 全量贴附,见 `docs/channel/telegram.md` §18)把 `formatStatusBarLines` 从 `internal/channel/feishu/usage_footer.go` 抽到**新**的 `internal/statusbar` 包 —— 仅作为**纯 renderer**,无 stamp / build / runtime 逻辑:

| 时期 | `internal/statusbar` 角色 | 文件 |
|---|---|---|
| F-CLAUDE-PRINT-002 之前 | 包存在:stamp / build / runtime helpers,负责"在 OutboundMessage 上 stamp StatusBar" | `stamp.go` / `build.go` / `runtime.go`(本文档第 1-3 批描述) |
| F-CLAUDE-PRINT-002 之后 | 包**删除**,flat fields 直接上 `OutboundMessage` | (空) |
| 2026-08-22 plan-D 之后 | 包**重生**:纯 renderer `StatusBarLines(msg) []string`,feishu + telegram adapter 都消费它 | `statusbar.go`(单文件,无 stamp / build / runtime 概念) |

**两个角色不冲突**:旧包是"stamping helper"(已经把责任推到 gateway),新包是"rendering helper"(channel 侧消费 OutboundMessage flat fields)。新包不持有任何 state,不修改 msg,纯函数 + 零依赖 `internal/messages`。

新包消费方:

```go
import "github.com/cnlangzi/nightme/internal/statusbar"

lines := statusbar.StatusBarLines(&msg) // 三行 footer,zero-omit
```

调用方:

- `internal/channel/feishu/adapter.go`(5 处 `formatStatusBarLines(&msg)` → `statusbar.StatusBarLines(&msg)`)
- `internal/channel/telegram/adapter.go`(v8: Send switch 的所有 text 出口 + OutHeartbeat 占位 PATCH,通过 `renderBodyWithStatusBar` helper 拼 trailer;`OutError` raw 拼接分支也直接调 `statusbar.RenderPanel`. v9 chain rewrite 2026-08-22 删除了 `renderBodyWithStatusBar`,footer 语义迁到 `chain.lastFooter` + `placeholder_chain_flush.go::renderActiveChunkBody`,8 个 Out* kind 改成走 `appendSegmentForKind` 路径. 见 docs/channel/telegram.md §11.12.6 + §11.12.7)

测试矩阵:`internal/statusbar/statusbar_test.go` 15 个 `Test*` 函数(从 feishu `usage_footer_test.go` 全量迁移)+ 1 个 8 行 table-driven `TestStatusBarLines_OmitsZeroSegments` 子用例。

---

## 后续:2026-08-29 减少 git status 撞 `.git/index.lock`(fix-git-lock-file)

**触发**:LLM(`claude` / `codex` / `pi`)在 worktree 里跑 `git add` / `git commit` 时反复撞到 `fatal: Unable to create '.git/index.lock': File exists`。LLM 看到 stderr 后**自行归纳**为"codegraph MCP 的子 git 进程在占着" — 这个归因是 hallucination:`pgrep -lf git` 当前查不到任何 git 进程,`codegraph` 的依赖里也没有任何 git 工具(`tree-sitter-wasms` / `web-tree-sitter` / `commander` / `sqlite` 等),其 watchdog 子进程(`PID 21396` 那条 `node -e <...>`)只 stat `.codegraph/*.db*`,**根本不接触 `.git/`**。

**真凶(本仓库内已确认 call path)**:`ChatSession.GitStatus(ctx)` 每条 outbound stamp 都跑一次 `git status --porcelain --branch --untracked-files=normal`,30-event turn 触发 30 次 git 子进程。`chatsession.go:754` 给 stamp 加了 `runCtx, _ := context.WithTimeout(ctx, 3*time.Second)`,git 未在 3s 内返回时 `exec.CommandContext` 直接 SIGKILL,**`.git/index.lock` 残留**。LLM 紧接着跑 `git add` 撞锁。

本文档前述"性能权衡"已记 `--untracked-files=no` 与 inflight fan-in 两个 follow-up。本次补刀两件事,**只这两件**:

1. **过滤**:Emitter 在 stamp git status 前按 `OutboundKind` 跳过 4 类(`OutToolStart` / `OutToolEnd` / `OutThinking` / `OutHeartbeat`)
2. **grace**:把 `gtw.runCmd` 出去的 git 子进程从 SIGKILL-on-cancel 改为 SIGTERM → 1s grace → SIGKILL,让 git 正常释放 `index.lock`

`OutHeartbeat` 也加入跳过清单(F-63 设计的每回合心跳事件,长 turn 会有 N 个心跳 = N 次额外 `git status`,纯内部状态传播,不展示给用户,跳过最划算)。

### 改动 1 — Emitter 守卫(`internal/gateway/outbound/outbound.go:131`)

```go
func (e *emitImpl) stampGitStatus(ctx context.Context, msg *messages.OutboundMessage) {
    if e.gitStatusLookup == nil ||
        msg.GitStatus != nil ||
        msg.ChatID == "" {
        return
    }
    // F-fix-git-lock: 跳过非 user-visible kind:
    //   - OutToolStart/End — Bash 可能正持有 .git/index.lock
    //   - OutThinking     — 用户看不到的 reasoning metadata
    //   - OutHeartbeat    — 每回合心跳的 progress tick(N/turn)
    switch msg.Kind {
    case messages.OutToolStart,
        messages.OutToolEnd,
        messages.OutThinking,
        messages.OutHeartbeat:
        return
    }
    msg.GitStatus = e.gitStatusLookup(ctx, msg.ChatID)
}
```

用 `OutboundKind` 而不是 `EventKind`,是因为 **`EventAgentThink` 不存在**(见 `internal/agent/agent.go:99-164` 枚举),thinking 数据是从 `EventAgentText` 下游切走的(见 `internal/agent/agent.go:82-83`)。`OutboundMessage.Kind` 是现成字段,Emitter 自己看到。

**不动**:`EventKind` enum、`OutboundMessage` 字段、AgentEventBus 路由。`EventAgentText → OutReply` 这条 user-visible 路径保留 stamp(用户实际看到的回复,必须带新鲜 footer)。

### 改动 2 — `proc.WithGrace`

新文件 platform-split,沿用现有 `exec_unix.go` / `exec_windows.go` 切分范式:

| 文件 | 平台 | 行为 |
|---|---|---|
| `internal/proc/grace_unix.go`(新)| !windows | 监听 ctx.Done → `syscall.Kill(-pid, SIGTERM)`(Setsid 启用 process group 广播)→ 1s grace 后 SIGKILL |
| `internal/proc/grace_windows.go`(新)| windows | no-op(`TerminateProcess` 是唯一机制) |

```go
// grace_unix.go 关键片段
const KillGrace = 1 * time.Second

func WithGrace(cmd *exec.Cmd, grace time.Duration) {
    if cmd == nil || grace <= 0 { return }
    ctx := cmd.Context()                  // exec.CommandContext 注入的 ctx,Go stdlib 公共 API
    if ctx == nil { return }
    var once sync.Once
    arm := func() {
        once.Do(func() {
            if cmd.Process == nil { return }
            if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
                if !errors.Is(err, syscall.ESRCH) {
                    _ = cmd.Process.Signal(syscall.SIGTERM)
                }
            }
            time.AfterFunc(grace, func() {
                if cmd.Process == nil { return }
                _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
                _ = cmd.Process.Kill()
            })
        })
    }
    go func() {
        <-ctx.Done()
        arm()
    }()
}
```

复用 `cmd/nightme/kill_unix.go:28-56` 的 SIGTERM → 轮询 → SIGKILL pattern,但**不共享常量** — `kill_unix.go:killGrace` 是 agent CLI flush `resume-id` 用的,语义不是 git lock release,两套各自持有。

### 改动 3 — `gtw.runCmd` 一行接线

`internal/command/gtw/exec.go:runCmd` 在 `cmd.Env` 赋值后、`.Run()` 之前加一行:

```go
cmd.Env = applyMSYSEnvNoPathConv(os.Environ())
proc.WithGrace(cmd, proc.KillGrace)   // ← 新增
var so, se bytes.Buffer
cmd.Stdout = &so
cmd.Stderr = &se
```

调用者签名 / 行为不变,`ExecGitRunner.Run` / `ExecCLIRunner.Run` / `/gtw commit` / `/gtw push` / `/gtw pr` / `prcache.Cache.refresh` / `agent.Agent.Review` 等所有夜码侧 git 调用自动继承 grace。

### 不在本 PR 范围

| 候选 | 为何不做 |
|---|---|
| cwd-scoped git 单飞(`internal/command/gtw/gate.go`) | 留 v3。当前放过 `prcache` 后台刷新 + stamp 之间的并发。grace 生效后压力应显著下降 |
| stale-lock 兜底自愈(`gtw.runCmd` 顶部 `os.Stat + os.Remove`) | 留 v3 作为最后防线。grace 起作用时根本不需要 |
| `--untracked-files=no`(`git_status.go:114`) | 已记在本文档"性能权衡"段,非本次主题 |
| 加 `EventAgentThink` | 它当前不存在;下游 routing 已通过 `OutboundKind.OutThinking` 解决 |

### 验证步骤

1. `go build ./...` — 无报错
2. `go test ./internal/gateway/outbound/...` — 8 个新 stamp-guard case 全过
3. `go test ./internal/proc/...` — 5 个新 grace case 全过
4. `go test ./internal/command/gtw/...` — 既有 case 全过(runCmd 行为对调用者透明)
5. 真实启动 daemon,跑一段"较长回合",观察 daemon log:
   - `git status` 调用频次明显下降(OutThinking + 工具期间全跳过)
   - 不再出现 `fatal: Unable to create '.git/index.lock'`
6. 极端测:对 sleep 大于 grace 的 git 子进程发 SIGTERM,观察 grace 内退出路径正确

### Reference

- 出处 commit:无(本 fix 还没合)
- 关联文件:
  - `internal/gateway/outbound/outbound.go:131`(改动 1)
  - `internal/proc/grace_unix.go`(改动 2,新)
  - `internal/proc/grace_windows.go`(改动 2,新)
  - `internal/proc/exec_unix.go:59-63` `proc.New`(改动 2 的 ctx 来源)
  - `internal/command/gtw/exec.go:54-81 runCmd`(改动 3 接线点)
  - `internal/messages/outbound.go:13-`OutboundKind 枚举(本次涉及 OutToolStart/OutToolEnd/OutThinking)
  - `internal/agent/agent.go:99-164` EventKind 枚举(本次**不**改,确认 EventAgentThink 不存在)
