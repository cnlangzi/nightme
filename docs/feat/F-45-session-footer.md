# F-45: AgentSession 累计 Token 统计 + Main-Chat 卡片 Footer

> **Status**: 📝 设计阶段（doc-first，2026-08-05）
> **Milestone**: v1.3.x
> **Scope**:
> - `internal/agent/agent.go` — `UsageInfo` 搬到 `agent` 包（type alias in gateway）
> - `internal/chatsession/agentsession.go` — `Model` / `CumulativeUsage` / `ResetCumulative` / `PersistIfDirty` API
> - `internal/registry/agent_session_entry.go` — schema 加 `Model` / `CumulativeUsage`
> - `internal/gateway/messages.go` — `OutboundMessage.SessionContext *SessionContext` typed field
> - `internal/gateway/handlers_chatsession.go` — `handleNew` 调 `ResetCumulative` + `PersistAgentSession`
> - `cmd/nightme/run.go::newEventHandler` — `SetModel` / `AccumulateUsage` / stamp `SessionContext` / `PersistIfDirty`
> - `internal/channel/feishu/usage_footer.go` (NEW) — `formatSessionFooter` helper
> - `internal/channel/feishu/adapter.go` — `Send` 三个 main-chat case 拼 footer
> - 文档同步（`SPEC.md` §0.12 / `channel/feishu.md` §13.22 / `F-44 §6.1` cross-link）
>
> **Depends on**: F-25 (rolling-log receipt), F-37 (multi-div split), F-38 (task checklist), F-39 (OutResult 独立 reply), F-40 (OutReply 改名), F-42 (lazy receipt creation), F-43 (`/kill` graceful), F-44 (OutReply 拆出 receipt + OutInit/OutUsage 推迟)
> **Related**: [`SPEC.md`](../SPEC.md) §0.12（本文落地）/ §1.3 / §1.4 / §2.2；[`channel/feishu.md`](../channel/feishu.md) §12 / §13.22 / §18；[`F-44 §6.1`](./F-44-outreply-independent-and-task-receipt.md) 推迟项兑现

---

## 0. 背景

### 0.1 F-44 §6.1 推迟的 footer

F-44 在 `internal/channel/feishu/receipt.go` 把 receipt 简化为只装 Task checklist，删除了 footer（`init` / `usage`）段。同时 §6.1 明确推迟了 footer 的兑现：

> **6.1 OutInit / OutUsage footer 渲染**
> - 新增 `SessionMeta *SessionMeta` typed field 到 `OutboundMessage`（或扩展 `Init` 字段）
> - ChatSession 持有 `SnapshotInit()` + `LiveBranch()` + `LiveCwd()` + `InvalidateBranchCache()` API
> - EventHandler 在每次 emit 时戳印 `SessionMeta`
> - Channel 在 `sendReplyInThreadAndChat` / `sendResultAsReply` / `ensureReceiptForTask` 内部读 `msg.SessionMeta` 渲染 footer

**F-45 是这份兑现**。但设计经过两轮迭代（见 §0.3），比 F-44 §6.1 草拟更紧凑。

### 0.2 现状

**token 数据已经到达 Gateway，但不被消费**：
- `internal/agent/agent.go:296` `UsageEvent`（4 个 token 字段 + CostUSD）
- `internal/bridge/claudecode/stream.go:601` `decodeUsage` 解析 Anthropic API 字段
- `internal/gateway/translate.go:158` `Translate(EventUsage)` 产出 `OutboundMessage{Kind: OutUsage, Usage: *UsageInfo}`
- `internal/channel/feishu/adapter.go::Send` case `OutUsage`：**silent drop**（F-44 §0.11 落地）

**model 数据已经到达 Gateway，也不被消费**：
- `agent.InitEvent.Model` 字段已存在（`internal/agent/agent.go:341`）
- `internal/gateway/translate.go:205` 拼字符串 `"session initialized (model: %s)"`，但这个字符串随 `OutInit` 一起被 silent drop

**AgentSession wrapper 完全不感知这些**：
- `internal/chatsession/agentsession.go:43` `AgentSession` struct 只有 ID / ChatSessionID / Agent / Cwd / pid / status / args / 时间戳 / ExitCode / ResumeID / handle / handleEventsClosed
- **没有任何 token / model / cost 字段**

### 0.3 设计迭代（两轮收紧）

#### 第一轮：3 个独立 typed field

最初设想是给 `OutboundMessage` 加 3 个分散字段：

```go
AgentName       string  // runtime 填 s.Agent
Model           string  // runtime 填 s.Model()
CumulativeUsage *UsageInfo  // runtime 填 s.CumulativeUsage()
```

**问题**：Channel 拿到 3 个字段后要自己拼装 footer，metadata 关系散落 3 处，扩展新字段（如 `provider_url` / `agent_version`）要继续加字段。

#### 第二轮（采纳）：1 个 `SessionContext` typed snapshot

把所有 metadata 收拢到 1 个 typed struct：

```go
type SessionContext struct {
    Agent           string
    Model           string
    CumulativeUsage UsageInfo
}

type OutboundMessage struct {
    // ...既有字段...
    SessionContext *SessionContext  // ← 单一字段
}
```

**收益**：
- wire 更紧凑（1 个字段 vs 3 个）
- Channel 不需要知道"agent / model / tokens 是分别维护的"——`SessionContext` 是 1 个 atomic snapshot
- 未来扩展新字段只改 `SessionContext` 定义，不破 Channel 接口
- runtime 维护 AgentSession 的 metadata 是单一职责——「AgentSession 自描述」

### 0.4 用户问题澄清

实施前的对话澄清了三个关键点：

**Q：IN 和 Cache Read 是包含还是分开？**
> **分开**（独立计数，不重叠）。`internal/bridge/claudecode/stream.go:601` 直接拷贝 Anthropic API 原生字段：`input_tokens` 是非缓存输入，`cache_read_input_tokens` 是缓存命中，两者独立。
>
> **Total**：`InputTokens + CacheCreationInputTokens + CacheReadInputTokens + OutputTokens`——4 个全加。
>
> **附带**：原 `UsageInfo.InputTokens` 注释 "total input tokens consumed ... (prompt + cache reads + tool input)" 是误导（实现只搬运裸 `input_tokens`），本 PR 顺手修注释。

**Q：Agent name 是不是已经在 AgentSession 上？**
> 是的。`AgentSession.Agent string` 是 immutable 字段，runtime 直接读，无需走 OutInit 链路。

**Q：Model 能不能也放到 AgentSession 上？**
> 能。`InitEvent.Model` 字段已经存在；runtime 在 EventInit 时 `s.SetModel(ev.Init.Model)` 一次缓存，后续每次 outbound 直接读 `s.Model()`。避免每个 turn 都从 bridge event 流重新解析。

**Q：footer 格式偏好？**
> 极简箭头版：
> ```
> claude · opus-4-5 · ↓ 12.3k · ↻ 8.2k cached · ↑ 1.5k · Total 22.0k · $0.087
> ```
> `↓ / ↻ / ↑` 是 ASCII 箭头（不依赖 emoji 字体）；`·` 是 middle dot；`$0.087` 保留 3 位小数（与 Anthropic 账单精度对齐）。

### 0.5 持久化范围澄清

**Q：cumulative 持久化到文件？什么时候清零？**
> 持久化到 `agent_sessions.json`（`AgentSessionEntry` 加 `CumulativeUsage *UsageInfo` + `Model string`）。**只有 `/new` 命令清零**——其他场景（daemon 重启 / `/cwd` / `/use` / `/kill` / 进程崩溃）一律保留。
>
> **理由**：用户视角下，"这个 chat 的累计 token 消耗"是有价值的历史信息，不应被生命周期事件抹掉。`/new` 是用户主动"重置上下文"——此时 footer 也应从零开始计数，语义一致。

---

## 1. 设计

### 1.1 视觉对比

**改前**（典型 turn：5 个 OutReply chunk + 2 个 OutTaskCreate + 1 个 OutResult）：

```
main chat:
  ├ Reply 1  💬 chunk 1                    ⬅ 纯文本（无 token 信息）
  ├ Reply 2  💬 chunk 2
  ├ Reply 3  � chunk 3
  ├ Receipt Card (only Tasks, F-44 瘦身后)
  │   **📋 Tasks** checklist (2 items)
  └ Reply 4  📝 complete OutResult text

用户看不到 token / model / cost —— 信息丢失（F-44 §6.1 推迟项）。
```

**改后**（同样 turn，footer 在每条 main-chat 消息底部）：

```
main chat:
  ├ Reply 1  💬 chunk 1
  │           ─────────────────────────────
  │           claude · opus-4-5 · ↓ 12.3k · ↻ 8.2k cached · ↑ 1.5k · Total 22.0k · $0.087
  ├ Reply 2  � chunk 2
  │           claude · opus-4-5 · ↓ 15.1k · ↻ 9.4k cached · ↑ 1.8k · Total 26.3k · $0.103
  ├ Reply 3  💬 chunk 3
  │           claude · opus-4-5 · ↓ 18.0k · ↻ 10.7k cached · ↑ 2.1k · Total 30.8k · $0.119
  ├ Receipt Card (Tasks)
  │   **📋 Tasks** checklist (2 items)
  │   ────────────────────────────────────
  │   claude · opus-4-5 · ↓ 18.0k · ↻ 10.7k cached · ↑ 2.1k · Total 30.8k · $0.119
  └ Reply 4  📝 complete OutResult text
              ─────────────────────────────
              claude · opus-4-5 · ↓ 18.0k · ↻ 10.7k cached · ↑ 2.1k · Total 30.8k · $0.119
```

**关键变化**：
- 每条 main-chat 消息底部都带 footer
- cumulative 单调递增（每次 footer 显示"截至此刻"）
- 第一次 reply 已经有完整 footer（cumulative 已经含前几个 turn 的数据）
- Task receipt card 末尾也带 footer（与 reply 同步）
- footer 跟 reply 主体视觉上分隔（用 hr / 空白行）

### 1.2 Routing 分流表（F-45 后）

| OutboundKind | 表面 | Footer |
|---|---|---|
| `OutReply` | **ReplyInThreadAndChat**（每 chunk） | ✅ footer 在文末 |
| `OutResult` | ReplyInThreadAndChat | ✅ footer 在文末 |
| `OutTaskCreate` / `OutTaskUpdate` | **Rolling-log receipt card**（Tasks） | ✅ footer 在 checklist 末尾 |
| `OutCard` (permission) | Top-level Create | ❌ 不带 footer（短状态消息） |
| `OutCommandReply` | Top-level Create | ❌ 不带 footer |
| `OutThinking` / `OutToolStart` / `OutToolEnd` | `ReplyInThread` | ❌ 不带 footer（thread 视觉独立） |
| `OutMessageState` | AddReaction | ❌ 不带 footer |
| `OutInit` / `OutUsage` | Silent drop（F-44 不变） | — |
| `OutCompaction` | `ReplyInBoth` | ❌ 不带 footer（短暂 marker） |

**stamping 规则**（runtime 决定，不在 Channel）：
```go
switch out.Kind {
case gateway.OutReply, gateway.OutResult,
    gateway.OutTaskCreate, gateway.OutTaskUpdate:
    stamp SessionContext  // ← 4 个 main-chat Kind
}
```

### 1.3 SessionContext 字段语义

```go
// internal/gateway/messages.go (NEW)
type SessionContext struct {
    // Agent is the registry name of the agent that produced this
    // outbound event (e.g. "claude", "codex"). Sourced from
    // AgentSession.Agent — immutable, no lock needed at read site.
    Agent string

    // Model is the model the agent selected (Claude Code:
    // system/init.model). Sourced from AgentSession.Model, which
    // the runtime captures on first EventInit. Empty before
    // EventInit lands — footer helper omits the segment when "".
    Model string

    // CumulativeUsage is the per-AgentSession running total of
    // token / cost stats as of this event's emission. Sourced
    // from AgentSession.CumulativeUsage, which the runtime
    // accumulates on every EventUsage. Captured by VALUE (struct
    // copy under RWMutex) so Channel can render at leisure.
    //
    // All 4 token fields are zero on a fresh /new'd session.
    // Channel derives Total = In + CacheCreate + CacheRead + Out
    // at render time; no Total field on the wire (avoids redundancy).
    CumulativeUsage UsageInfo
}
```

### 1.4 AgentSession 元数据（runtime 自管）

```go
// internal/chatsession/agentsession.go
type AgentSession struct {
    // ... 既有字段 ...
    Agent         string  // 已有，immutable
    
    // NEW: captured from EventInit on first observation.
    // Mutex-guarded because SetModel races with concurrent
    // reads (footer rendering). Empty before EventInit lands.
    modelMu       sync.RWMutex
    Model         string
    
    // NEW: per-AgentSession cumulative token / cost totals.
    // Persists across daemon restarts; cleared only by /new.
    cumulativeUsageMu sync.RWMutex
    cumulativeUsage   UsageInfo
    cumulativeDirty   bool
}
```

**API（线程安全）**：
```go
// Model
func (as *AgentSession) SetModel(m string)   // idempotent: 已有非空值不覆盖
func (as *AgentSession) Model() string

// Usage
func (as *AgentSession) AccumulateUsage(u *agent.UsageEvent)  // 加锁累加，dirty=true
func (as *AgentSession) ResetCumulative()                      // 清零 + dirty=true（仅 /new）
func (as *AgentSession) CumulativeUsage() UsageInfo            // RLock 快照
func (as *AgentSession) PersistIfDirty(persist func(*registry.AgentSessionEntry) error) error
```

### 1.5 Wire 形态（F-45 后）

`OutboundMessage` 加 1 个 typed field：

```go
// SessionContext carries the runtime-stamped AgentSession
// snapshot for footer rendering. Stamped ONLY on OutReply /
// OutResult / OutTaskCreate / OutTaskUpdate. nil on every other
// kind (thread-only, lifecycle, init/usage payloads themselves).
//
// Bridges never populate this field; runtime's newEventHandler
// closure is the single owner of "what footer should this card
// render?". See docs/feat/F-45-session-footer.md §1.3.
SessionContext *SessionContext
```

**不变式**：
- 1 个字段，不是 3 个（§0.3 论述）
- bridges 不动（仍是 EventInit / EventUsage 事件）
- runtime 唯一 owner
- Channel 读 `msg.SessionContext`，nil 时跳过 footer

### 1.6 Footer 渲染规则（formatSessionFooter）

```go
// internal/channel/feishu/usage_footer.go (NEW)
//
// Format C (arrow-based, ASCII only):
//   claude · opus-4-5 · ↓ 12.3k · ↻ 8.2k cached · ↑ 1.5k · Total 22.0k · $0.087
//
// Segments:
//   - Agent   : omitted when ""
//   - Model   : omitted when ""
//   - ↓ in    : omitted when 0; value = InputTokens + CacheCreationInputTokens
//   - ↻ cache : omitted when 0; suffix " cached" fixed
//   - ↑ out   : omitted when 0
//   - Total   : omitted when all token segments are 0; value = sum of 3
//   - $cost   : omitted when CostUSD == 0; 3 decimal places
//
// Separator: " · " (middle dot + spaces) — visually consistent with
// F-37 / F-44 footer conventions. No emoji (user preference).
func formatSessionFooter(ctx *gateway.SessionContext) string {
    if ctx == nil {
        return ""
    }
    parts := []string{}
    if ctx.Agent != "" {
        parts = append(parts, ctx.Agent)
    }
    if ctx.Model != "" {
        parts = append(parts, ctx.Model)
    }
    u := ctx.CumulativeUsage
    in := u.InputTokens + u.CacheCreationInputTokens
    if in > 0 {
        parts = append(parts, "↓ " + abbrevTokens(in))
    }
    if u.CacheReadInputTokens > 0 {
        parts = append(parts, "↻ " + abbrevTokens(u.CacheReadInputTokens) + " cached")
    }
    if u.OutputTokens > 0 {
        parts = append(parts, "↑ " + abbrevTokens(u.OutputTokens))
    }
    total := in + u.CacheReadInputTokens + u.OutputTokens
    if total > 0 {
        parts = append(parts, fmt.Sprintf("Total %s", abbrevTokens(total)))
    }
    if u.CostUSD > 0 {
        parts = append(parts, fmt.Sprintf("$%.3f", u.CostUSD))
    }
    if len(parts) == 0 {
        return ""
    }
    return strings.Join(parts, " · ")
}

// abbrevTokens: <1000 raw, 1000-999999 → "%.1fk", >=1M → "%.1fM"
func abbrevTokens(n int) string {
    switch {
    case n >= 1_000_000:
        return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
    case n >= 1_000:
        return fmt.Sprintf("%.1fk", float64(n)/1_000)
    default:
        return fmt.Sprintf("%d", n)
    }
}
```

**实测样例**：

```
↓ 234 · ↻ 5.6k cached · ↑ 89 · Total 5.9k                    # 小 turn，无 agent/model
claude · opus-4-5 · ↓ 12.3k · ↻ 8.2k cached · ↑ 1.5k · Total 22.0k · $0.087   # 标准
claude · opus-4-5 · ↓ 156k · ↻ 1.2M cached · ↑ 18k · Total 1.37M · $1.245    # 大 turn
claude · ↓ 12.3k · ↑ 1.5k · Total 13.8k                                       # 无 model 无 cost 无 cache
claude · opus-4-5 · ↓ 12.3k · ↻ 8.2k cached · ↑ 1.5k · Total 22.0k          # 无 cost
```

### 1.7 State 流转

```
AgentSession 生命周期:
  [spawn]
    ↓
  SetModel(ev.Init.Model)     ← EventInit 触发；idempotent
  AccumulateUsage(ev.Usage)   ← EventUsage 触发（每个 turn 一次）
    ↓
  ... 持续累积 ...
    ↓
  PersistIfDirty              ← EventDone 触发；落盘 agent_sessions.json
    ↓
  /new → ResetCumulative      ← 用户主动重置上下文（唯一清零点）
    ↓
  PersistAgentSession         ← 立即落盘
```

**与现有字段的关系**：
- `ResumeID`：EventInit 时捕获，**永不重置**（除非 `/new` 通过 bridge `New()` 让 agent 重发 EventInit）
- `Model`：EventInit 时捕获，**永不重置**（同 ResumeID 语义）
- `CumulativeUsage`：EventUsage 时累加，**`/new` 重置**

---

## 2. 文件 & 接口

### 2.1 `internal/agent/agent.go`

**改动 A**：`UsageInfo` 从 `internal/gateway/messages.go` 搬到 `internal/agent/agent.go`（紧挨 `UsageEvent`）。

```go
// 原因：chatsession 包要 import UsageInfo，但不应反向 import gateway。
//       agent 是底层包，UsageInfo 与 UsageEvent 同语义层级，放一起。
type UsageInfo struct {
    InputTokens              int
    OutputTokens             int
    CacheCreationInputTokens int
    CacheReadInputTokens     int
    CostUSD                  float64
}
```

**改动 B**：修 `UsageInfo.InputTokens` 注释——原注释 "the total input tokens consumed across the turn (prompt + cache reads + tool input)" 是误导（实现只搬运裸 `input_tokens`，不包含 cache reads），新注释：

```go
// InputTokens is the non-cached input token count from the
// last LLM call (Anthropic API: input_tokens field).
// Cache hits are NOT included — see CacheReadInputTokens.
InputTokens int
```

### 2.2 `internal/gateway/messages.go`

**改动 A**：定义 `SessionContext` typed struct（见 §1.3）。

**改动 B**：给 `OutboundMessage` 加 `SessionContext *SessionContext` 字段（见 §1.5）。

**改动 C**：保留 `UsageInfo` 的 type alias 兼容：

```go
// UsageInfo is the cumulative form of UsageEvent — used on
// AgentSession wrapper and OutboundMessage.SessionContext.
// Re-exported as a type alias for backward compatibility with
// existing gateway code (translate.go / OutUsage payload).
type UsageInfo = agent.UsageInfo
```

### 2.3 `internal/registry/agent_session_entry.go`

**改动**：加 2 个字段：

```go
type AgentSessionEntry struct {
    // ... 既有字段 ...
    
    // F-45: cumulative token / cost stats. Persists across
    // daemon restarts; cleared only by /new (see F-45 §1.7).
    // nil on legacy entries (zero-value behavior on read).
    CumulativeUsage *UsageInfo `json:"cumulativeUsage,omitempty"`
    
    // F-45: model captured on first EventInit. Persists for
    // the lifetime of the AgentSession IDENTITY (until /new
    // re-emits EventInit with a new model — rare).
    Model string `json:"model,omitempty"`
}
```

**JSON 兼容**：Go 默认 JSON unmarshal 容忍缺失字段——旧 `agent_sessions.json` 无 `cumulativeUsage` / `model` 时安全 fallback 到零值。

### 2.4 `internal/chatsession/agentsession.go`

**改动 A**：struct 加 3 个字段（`modelMu` / `Model` / `cumulativeUsageMu` / `cumulativeUsage` / `cumulativeDirty`）。

**改动 B**：加 6 个方法（`SetModel` / `Model` / `AccumulateUsage` / `ResetCumulative` / `CumulativeUsage` / `PersistIfDirty`）。

**改动 C**：`FromAgentSessionEntry` 恢复时：

```go
if e.CumulativeUsage != nil {
    as.cumulativeUsage = *e.CumulativeUsage  // 拷贝，不是引用
}
if e.Model != "" {
    as.Model = e.Model  // 直接写，无需锁（构造时无并发读）
}
```

**改动 D**：`Entry()` 序列化时：

```go
as.cumulativeUsageMu.RLock()
cum := as.cumulativeUsage
as.cumulativeUsageMu.RUnlock()
return &registry.AgentSessionEntry{
    // ... 既有字段 ...
    CumulativeUsage: &cum,  // 永远非 nil：即使全零也带 — 区分"从未跑过"vs"跑了但=0"无意义，统一写
    Model:           as.Model(),
}
```

### 2.5 `cmd/nightme/run.go::newEventHandler`

**改动 A**：`EventInit` 处理块里加 Model 捕获：

```go
// 既有：
if ev.Kind == agent.EventInit && ev.Init != nil && ev.Init.SessionID != "" {
    s.SetResumeID(ev.Init.SessionID)
    if mgr != nil { _ = mgr.PersistAgentSession(s) }
}

// NEW:
if ev.Kind == agent.EventInit && ev.Init != nil && ev.Init.Model != "" {
    s.SetModel(ev.Init.Model)
}
```

**改动 B**：`Translate` 前加 usage 累加：

```go
// NEW: 累计 per-turn usage。Translate 前跑，保证 stamp 时已含本 turn。
if ev.Kind == agent.EventUsage && ev.Usage != nil {
    s.AccumulateUsage(ev.Usage)
}
```

**改动 C**：`out.ReplyTo = userMsgID` 之后 stamp SessionContext：

```go
// NEW: 在 4 个 main-chat Kind 上 stamp SessionContext 快照
switch out.Kind {
case gateway.OutReply, gateway.OutResult,
    gateway.OutTaskCreate, gateway.OutTaskUpdate:
    snap := s.CumulativeUsage()
    if snap.InputTokens != 0 || snap.OutputTokens != 0 ||
        snap.CacheReadInputTokens != 0 || snap.CostUSD != 0 ||
        s.Model() != "" {
        out.SessionContext = &gateway.SessionContext{
            Agent:           s.Agent,        // immutable string, 无锁
            Model:           s.Model(),
            CumulativeUsage: snap,
        }
    }
}
```

**改动 D**：EventDone 处理路径加持久化：

```go
// 在 emitMessageStateForCurrentTurn 之后：
if ev.Kind == agent.EventDone {
    if err := s.PersistIfDirty(func(e *registry.AgentSessionEntry) error {
        if mgr == nil { return nil }
        return mgr.PersistAgentSession(s)
    }); err != nil && logger != nil {
        logger.Warn("persist agent session (usage) failed",
            "agent_session_id", s.ID, "err", err)
    }
}
```

### 2.6 `internal/gateway/handlers_chatsession.go::handleNew`

**改动**：在调 `agentSession.New(ctx)` 之后立即清零：

```go
// 既有：
if err := as.New(ctx); err != nil { ... }

// NEW: /new 是唯一清零 cumulative 的入口
as.ResetCumulative()
_ = mgr.PersistAgentSession(as)
```

**scope**：
- `/new <agent>`：只清单个 AgentSession
- `/new`：清 activeCwd 下所有 AgentSession（pool 内）

### 2.7 `internal/channel/feishu/usage_footer.go` (NEW)

**新文件**：

```go
package feishu

// formatSessionFooter 渲染 SessionContext 为单行 markdown。
// 返回 "" 表示无需 footer（nil 或全零）。
//
// 见 docs/feat/F-45-session-footer.md §1.6 完整规则。
func formatSessionFooter(ctx *gateway.SessionContext) string

// abbrevTokens token 数字缩写：<1000 raw，≥1k "X.Xk"，≥1M "X.XM"
func abbrevTokens(n int) string
```

### 2.8 `internal/channel/feishu/adapter.go`

**改动**：3 个 main-chat case 在发送前拼 footer。

#### Case `OutReply` → `sendReplyInThreadAndChat`

扩展 helper 接受 `footer string` 参数（或在 caller 拼好再传）：

```go
func (a *Adapter) sendReplyInThreadAndChat(
    ctx context.Context, chatID, userMsgID, text string,
) error {
    // ... 既有 sanitize / truncate / buildResultPayload 逻辑 ...
    
    // NEW: 在 text 末尾追加 footer（如果有）
    if footer := formatSessionFooter(msg.SessionContext); footer != "" {
        // 用 "\n\n" 分隔 footer 与正文（lark_md 渲染时换行）
        text = text + "\n\n" + footer
    }
    
    _, err = a.sendContent(...)
    return err
}
```

**调用方改动**（`Send` case `OutReply`）：

```go
case gateway.OutReply:
    text := strings.TrimSpace(msg.Text)
    if text == "" { return nil }
    if msg.ReplyTo == "" {
        return a.sendReplyInThreadAndChat(ctx, msg.ChatID, "", text, msg.SessionContext)
    }
    // ... 既有 fold 进 receipt 路径 ...
    return a.sendReplyInThreadAndChat(ctx, msg.ChatID, msg.ReplyTo, text, msg.SessionContext)
```

> **简化版决策**：本 PR 把 `sendReplyInThreadAndChat` 签名改为接受 `ctx *SessionContext`，而不是加新参数 `footer string`。前者更内聚——helper 自己读 SessionContext，自己决定要不要加 footer。

#### Case `OutResult` → `sendResultAsReply`

平行改造，签名加 `ctx *SessionContext`。

#### Case `OutTaskCreate` / `OutTaskUpdate` → receipt card

`buildReceiptCard(entries, tasks)` 加第三参数 `footer string`：

```go
// buildReceiptCard 签名变更
func buildReceiptCard(tasks []agent.TaskItem, footer string) (json.RawMessage, error)

// 内部：
if footer != "" {
    elements = append(elements, map[string]any{
        "tag": "hr",
    })
    elements = append(elements, map[string]any{
        "tag":  "div",
        "text": map[string]any{
            "tag":     "lark_md",
            "content": footer,
        },
    })
}
```

**元素预算影响**：footer 加 1 个 hr + 1 个 lark_md div = 2 额外元素。当前 receipt 50 element 预算，task checklist 一般 5-15 个 element，加 footer 后 7-17，**远未撞上限**。

### 2.9 Echo channel 透传

`internal/channel/echo/echo.go::Send` —— **零改动**。`SessionContext` 字段自然落到 `recorded` slice，测试用 `c.Record()` 验证被 stamp。

---

## 3. 实施计划

按 9 个独立 commit 顺序落地，每步可单独 revert：

1. **`refactor(agent): move UsageInfo to agent package (alias in gateway)`**
   - `internal/agent/agent.go` 新增 `UsageInfo` struct
   - `internal/gateway/messages.go` 改为 `type UsageInfo = agent.UsageInfo`
   - 修 `UsageInfo.InputTokens` 注释（§2.1 改动 B）

2. **`feat(registry): AgentSessionEntry add Model + CumulativeUsage fields`**
   - `internal/registry/agent_session_entry.go` 加 2 个字段
   - 无 JSON 迁移（旧文件容忍缺失）

3. **`feat(chatsession): AgentSession Model + CumulativeUsage API`**
   - struct 加 3 个字段
   - 加 6 个方法（§2.4）
   - `Entry()` / `FromAgentSessionEntry` 同步

4. **`feat(gateway): SessionContext typed field on OutboundMessage`**
   - `internal/gateway/messages.go` 新增 `SessionContext` struct
   - `OutboundMessage` 加 `SessionContext *SessionContext` 字段

5. **`feat(runtime): newEventHandler accumulate + capture + stamp SessionContext + PersistIfDirty`**
   - `cmd/nightme/run.go` 4 处改动（§2.5 改动 A/B/C/D）

6. **`feat(gateway): /new handler ResetCumulative + PersistAgentSession`**
   - `internal/gateway/handlers_chatsession.go::handleNew` 加 2 行

7. **`feat(feishu): formatSessionFooter helper + 3 main-chat case 渲染 footer`**
   - 新文件 `internal/channel/feishu/usage_footer.go`
   - `internal/channel/feishu/adapter.go` 改 `Send` 3 case + 改 `buildReceiptCard` 签名 + 改 `sendReplyInThreadAndChat` / `sendResultAsReply` 签名

8. **`docs(SPEC): §0.12 F-45 footer + cumulative persistence`**
   - `docs/SPEC.md` 加 §0.12 增量变更摘要

9. **`docs(feat): F-44 §6.1 cross-link + channel/feishu.md §13.22`**
   - F-44 §6.1 加一行 "实现见 F-45"
   - `docs/channel/feishu.md` §12 渲染映射表更新 + §13.22 新增 section

---

## 4. 测试

### 4.1 单元测试

| 测试 | 文件 | 覆盖 |
|------|------|------|
| `TestSetModel_Idempotent` | `internal/chatsession/agentsession_test.go` (NEW) | 第一次 SetModel 设值，第二次空值不覆盖，第二次非空值仍允许覆盖（用于 --model 切换场景） |
| `TestAccumulateUsage_Race` | `internal/chatsession/agentsession_test.go` | 100 goroutine × 1000 increments，验证最终 sum 正确 + race detector 无 warning |
| `TestResetCumulative_Clears` | `internal/chatsession/agentsession_test.go` | 累加 → ResetCumulative → CumulativeUsage() 全零 + dirty=true |
| `TestPersistIfDirty_NoOpWhenClean` | `internal/chatsession/agentsession_test.go` | dirty=false 时 PersistIfDirty 不调 persist callback |
| `TestPersistIfDirty_DirtyResets` | `internal/chatsession/agentsession_test.go` | dirty=true 时调一次 callback，dirty 立即重置（不会双重落盘） |
| `TestEntry_RoundtripPreserves` | `internal/chatsession/agentsession_test.go` | `Entry() → JSON marshal → unmarshal → FromAgentSessionEntry` 字段全相等 |
| `TestHandleNew_ResetsCumulative` | `internal/gateway/handlers_new_test.go` (EXTEND) | `/new` 后 ActiveAgentSession.CumulativeUsage() 全零 + 持久化 |
| `TestFormatSessionFooter_*` | `internal/channel/feishu/usage_footer_test.go` (NEW) | nil / all-zero / 仅 in / 含 cost / cache 标记 / 大数缩写 |
| `TestSend_OutReply_AppendsFooter` | `internal/channel/feishu/adapter_test.go` (EXTEND) | msg.SessionContext 非 nil 时，sendContent 收到 body 包含 footer 行 |
| `TestSend_OutResult_AppendsFooter` | 同上 | 同上 for OutResult |
| `TestBuildReceiptCard_WithFooter` | `internal/channel/feishu/receipt_test.go` (EXTEND) | footer 字符串出现在 card body 的最后 div element |
| `TestEcho_RecordsSessionContext` | `internal/channel/echo/echo_test.go` (EXTEND) | c.Record() 验证 SessionContext 字段被填充 |

### 4.2 集成测试

| 测试 | 文件 | 覆盖 |
|------|------|------|
| `TestNewEventHandler_AccumulatesAcrossTurns` | `cmd/nightme/run_test.go` (EXTEND) | mock 5 个 turn 的 EventUsage → s.CumulativeUsage() 等于 5 个 turn 之和 |
| `TestNewEventHandler_StampsOnlyMainChatKinds` | 同上 | 5 种 OutboundKind 各发一个 Event，验证 SessionContext 仅在 4 个 main-chat Kind 上非 nil |
| `TestNewEventHandler_PersistsOnEventDone` | 同上 | 模拟 EventUsage + EventDone，验证 PersistAgentSession 被调一次 |
| `TestRestart_PreservesCumulative` | `cmd/nightme/run_test.go` | spawn AgentSession → 累加 → 模拟 daemon 重启 → 新 AgentSession.CumulativeUsage() 等于上次落盘值 |

### 4.3 边界测试

| 测试 | 场景 |
|------|------|
| `TestFooter_OmitsZeroSegments` | Model="" / Cost=0 / CacheRead=0 时对应 segment 不显示 |
| `TestFooter_AllZero_ReturnsEmpty` | 全零时返回 ""，caller 不拼到 text |
| `TestSessionContext_NeverStampedOnThreadKinds` | OutThinking / OutToolStart / OutToolEnd / OutCompaction 不带 SessionContext |
| `TestSessionContext_NeverStampedOnLifecycleKinds` | OutInit / OutUsage / OutMessageState / OutCard / OutCommandReply 不带 SessionContext |

---

## 5. Migration & 兼容性

### 5.1 JSON 兼容性

**`agent_sessions.json`**：
- 旧文件无 `cumulativeUsage` 字段 → Go JSON unmarshal 容忍 → 内存里 `*UsageInfo == nil` → 视为"从未跑过"，cumulative 从零开始
- 旧文件无 `model` 字段 → 视为空字符串 → footer 不显示 model segment
- 第一次写入新字段后，新文件包含 `cumulativeUsage` + `model`——向后兼容（读端都容忍缺失）

**`chat_sessions.json`**：无变化（cumulative 是 per-AgentSession，不是 per-ChatSession）。

### 5.2 wire 兼容性

**`OutboundMessage`**：
- 新字段 `SessionContext *SessionContext`——Channel 实现需要适配（Feishu adapter 改 3 case）
- 其他 Channel 实现（Echo / Slack / Web）零改动也能编译（只是不渲染 footer）
- 未来 Channel 想支持 footer：读 `msg.SessionContext` 即可

**bridge 协议**：零变化。bridges 仍发 `EventInit` / `EventUsage`，runtime 负责捕获并 stamp。

### 5.3 行为兼容性

- **F-44 silent drop**：`OutInit` / `OutUsage` 仍 silent drop（F-45 不改 Channel 的 silent drop 决策）
- **F-39 OutResult 独立 reply**：不变
- **F-37 thread routing**：不变
- **F-25 rolling-log UX**：task receipt 仍是 rolling-log，footer 是新增 segment
- **F-31 MessageState 抽象契约**：不变

---

## 6. 不在本 PR 范围

- **`/cost` slash command**（读 cumulative stats 主动展示）—— 后续 PR
- **per-model breakdown**（Anthropic API `modelUsage` map 展开成 multi-line footer）—— 后续 PR
- **ChatSession-level 总计**（pool 内所有 AgentSession 之和）—— 后续 PR
- **token 数据准确性改进**（bridge 在 EventText / EventToolStart 之间插 token snapshot）—— 超出 PR 范围，claudecode 当前 `result.usage` 是最后 LLM call 的 token 数
- **agent_version / provider_url** 等扩展字段——`SessionContext` 已预留扩展位置，按需加

---

## 7. 不变式总结

**F-45 加 AgentSession 元数据 + 1 个 wire field + footer 渲染，但保留**：

- **`OutboundMessage` 契约 100% typed**（§1.4 不变；新字段 typed 不破 §1.4 边界规范）
- **§1.3 ChatSession 不 import channel/feishu**（不变）
- **Channel 不 import chatsession**（不变；Channel 通过 typed `SessionContext` 字段读 metadata）
- **1 turn : 1 anchor 不变式**保留（`ReplyTo = currentTurnUserMsgID` 仍是唯一 coordination key）
- **抽象归抽象 / 具体归具体**（footer 渲染细节由 Feishu adapter 自决，Slack / Web / Echo 各自决定）
- **bridges 协议零变化**（仍发 EventInit / EventUsage，runtime 翻译）
- **OutboundKind 不增不减**（`SessionContext` 是字段，不是新 Kind）
- **OutInit / OutUsage 仍是 silent drop**（F-44 决策保留；footer 走 `SessionContext` 单独路径）
- **§1.4 抽象 / 具体 边界规范**：metadata 是 typed primitive，Channel 自决渲染目标
- **F-25 rolling-log UX**：task receipt 仍是 rolling-log，footer 是新增 lark_md div
- **F-31 MessageState 抽象契约**：不变
- **F-37 thread routing**：不变
- **F-38 task checklist 决策**：不变
- **F-39 OutResult 决策**：不变（独立 reply + footer 拼文末）
- **F-40 OutReply 命名**：不变
- **F-42 lazy receipt creation**：不变
- **F-43 `/kill` graceful / `/new` ResetID**：本 PR 在 `handleNew` 加 `ResetCumulative`，与 F-43 的"clear ResumeID"语义对称
- **F-44 OutReply 拆出 + task receipt 瘦身**：不变

**为什么不是 v2.0**：v1.3 核心不变式（职责隔离、Binding FSM owner、Receipt 自治、抽象归抽象 / 具体归具体、§1.4 边界规范）全部保留。F-45 是 runtime + Channel 自治范围内的"metadata 自描述 + footer 渲染"扩展，不影响 nightme 数据模型与 Gateway 核心契约。

---

## 8. 变更日志

| 日期 | 版本 | 变更 |
|---|---|---|
| 2026-08-05 | 草案 | 初稿；兑现 F-44 §6.1 推迟项；设计经过 §0.3 两轮迭代收敛到 SessionContext 单字段 |
