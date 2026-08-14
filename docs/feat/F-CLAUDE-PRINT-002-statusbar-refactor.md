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