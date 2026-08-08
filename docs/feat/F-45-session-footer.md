# F-45: Main-Chat 卡片 Footer (per-turn snapshot)

> **Status**: ✅ 已落地（2026-08-05）；§1.7 F-48 follow-up（2026-08-06）；§1.8 F-49 follow-up（2026-08-06）
> **F-46 增量**（2026-08-06）—decision cards 加 button + 原地 PATCH；详见 [`F-46-interactive-cards.md`](./F-46-interactive-cards.md)
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
> **Related**: [`SPEC.md`](../SPEC.md) §0.12（本文落地）/ §1.3 / §1.4 / §2.2；[`channel/feishu.md`](../channel/feishu.md) §12 / §13.22 / §13.24 (F-48 git branch) / §13.25 (F-49 compaction counter) / §18；[`F-44 §6.1`](./F-44-outreply-independent-and-task-receipt.md) 推迟项兑现；**[`F-46-interactive-cards.md`](./F-46-interactive-cards.md)** 决策卡 button + 原地 PATCH；§1.7 F-48 git branch follow-up；§1.8 F-49 compaction counter follow-up；详细 F-49 设计见 [`F-49`](./F-49-compaction-counter.md)

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
- `agent.AgentConnectedEvent.Model` 字段已存在（`internal/agent/agent.go:341`）
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
Usage *agent.UsageEvent  // bridge 报的本轮 usage — runtime 直接 out.Usage 透传
```

**问题**：Channel 拿到 3 个字段后要自己拼装 footer，metadata 关系散落 3 处，扩展新字段（如 `provider_url` / `agent_version`）要继续加字段。

#### 第二轮（采纳）：1 个 `SessionContext` typed snapshot

把所有 metadata 收拢到 1 个 typed struct：

```go
type SessionContext struct {
    Agent           string
    Model           string
    Usage *agent.UsageEvent
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
> 能。`AgentConnectedEvent.Model` 字段已经存在；runtime 在 EventAgentConnected 时 `s.SetModel(ev.Connected.Model)` 一次缓存，后续每次 outbound 直接读 `s.Model()`。避免每个 turn 都从 bridge event 流重新解析。

**Q：footer 格式偏好？**
> 极简箭头版：
> ```
> claude · opus-4-5 · ↓ 12.3k · ↻ 8.2k cached · ↑ 1.5k · Total 22.0k · $0.087
> ```
> `↓ / ↻ / ↑` 是 ASCII 箭头（不依赖 emoji 字体）；`·` 是 middle dot；`$0.087` 保留 3 位小数（与 Anthropic 账单精度对齐）。
>
> **F-52 update (2026-08)**：上方 C 版已废，footer 改为「`💰:「 in / out · X% · $cost 」`」格式——见 §1.6 最新规则。保留此行作为 F-45 原始 design record。

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

> **F-49 删除**：`OutCompaction` kind 整条 path 删除（runtime handler 不再产生 OutboundMessage；channel adapter 删除 `Send` case）。理由：用户明确不需要"压缩进行中"瞬时显示，compaction 只反映次数（Line 1 🗜 N）。详见 [`F-49 §1.9`](./F-49-compaction-counter.md)。

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
    // the runtime captures on first EventAgentConnected. Empty before
    // EventAgentConnected lands — footer helper omits the segment when "".
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
    Usage *agent.UsageEvent
    // F-49: cumulative count of completed context compactions on
    // this AgentSession. 0 when never compacted. Persists across
    // daemon restarts; cleared only by /new. Sourced from
    // AgentSession.CompactionCount at the same instant as
    // CumulativeUsage so Line 1 (🗜 N) and Line 2 (↓ ↻ ↑ total)
    // tell a coherent story.
    CompactionCount int
}
```

### 1.4 AgentSession 元数据（runtime 自管）

```go
// internal/chatsession/agentsession.go
type AgentSession struct {
    // ... 既有字段 ...
    Agent         string  // 已有，immutable
    
    // NEW: captured from EventAgentConnected on first observation.
    // Mutex-guarded because SetModel races with concurrent
    // reads (footer rendering). Empty before EventAgentConnected lands.
    modelMu       sync.RWMutex
    Model         string
    
    // NEW: per-AgentSession cumulative token / cost totals.
    // Persists across daemon restarts; cleared only by /new.
    // F-49: also guards compactionCount (RecordCompaction modifies
    // both atomically — see F-49 §1.4).
    cumulativeUsageMu sync.RWMutex
    cumulativeUsage   UsageInfo
    compactionCount   int       // F-49: 🗜 N 计数；同锁守护
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
func (as *AgentSession) ResetCumulative()                      // 清零 + dirty=true（仅 /new，包括 compactionCount）
func (as *AgentSession) CumulativeUsage() UsageInfo            // RLock 快照
// F-49:
func (as *AgentSession) RecordCompaction()                     // count++ + 4 token 字段归零 + CostUSD 保留 + dirty=true
func (as *AgentSession) CompactionCount() int                  // RLock 快照
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
- bridges 不动（仍是 EventAgentConnected / EventUsage 事件）
- runtime 唯一 owner
- Channel 读 `msg.SessionContext`，nil 时跳过 footer

### 1.6 Footer 渲染规则（formatSessionFooter）

**F-52 重构 (2026-08)**：F-45 原本把 `in / out / cache / total / $cost` 拆成多段（`↓ in · ↻ cached · ↑ out · Total · $cost`）。F-52 统一为更紧凑的「`💰:「 in / out · X% · $cost 」`」格式，理由：
- "in" 按 https://yb.tencent.com/s/3G6HphjOxM70 的口径合并三个 input-side 字段（`InputTokens + CacheCreationInputTokens + CacheReadInputTokens`），避免用户在 IM 里还要心算 cache_creation + cache_read。
- "X%" 是 per-turn context-window 使用率（`used / contextWindow * 100`，`contextWindow` 是 bridge-local 变量 — Claude Code 来自 `modelUsage[<model>].contextWindow`,Pi 来自 `get_state.data.model.contextWindow`,详见 [`F-54`](./F-54-pi-contextwindow-from-get-state.md)），让用户一眼看到距离 ceiling 还剩多少。
- "$cost" 直接透传 API 报的 `total_cost_usd`，客户端不计算（没有 rate table / 没有 per-model pricing）。

**新格式 (Format D, 「」 enclosed)**：

```
🤖 claude · opus-4-5
💰:「 20.5k / 1.5k · 10.5% · $0.087 」
📁 code/nightme · ⎇ main · ↑ 3 · ? 2 · ⇡ 2
```

**Line 2 segments**：

| 段 | 来源 | Omit 规则 | 渲染 |
|---|---|---|---|
| `in / out` | `in = InputTokens + CacheCreationInputTokens + CacheReadInputTokens`；`out = OutputTokens`。F-55.1 进一步把 `in` 拆 `new / cache / out`： | `in == 0 && out == 0` 时整段省略（无 usage） | F-55.1 render:`new / cache / out`,纯数字无 label；每段按 `> 0` 独立 omit。`cache == 0` 时退回 `new / out` 布局。`new = InputTokens + CacheCreationInputTokens`,`cache = CacheReadInputTokens`。 |

> F-55.1: Anthropic 三个 input-side 字段**互斥**(每个 token 恰好落一个桶),split 不引入重叠——`in == new + cache` 恒成立。Doc 1 pct 仍按 `(new + cache + out) / contextWindow` 计算。|
| `X% (window)` | `SessionContext.ContextWindowPct` + `SessionContext.Usage.ContextWindow`(F-55 透传:Claude Code `modelUsage[<model>].contextWindow`,Pi `get_state.data.model.contextWindow`) | `ContextWindowPct == 0` 时整段省略(`window == 0 && pct == 0` 也走 omit 路径) | `fmt.Sprintf("%.1f%% (%s)", pct, abbrevWindow(window))` — 一位小数;`99.6%` 不能四舍五入到 `100%`;`pct > 100%` **不 clamp 不告警**,让用户看到分母自行判断(`101.6% (200k)` 即是 MiniMax 兼容端把 1M 模型错报成 200K 的诊断信号) |
| `$cost` | `agent.UsageEvent.CostUSD`（F-52 透传 API 报的 `total_cost_usd`） | `== 0` 时省略（API 没报） | `fmt.Sprintf("$%.3f", cost)` — 三位小数，与 F-45 原约定一致 |

段之间用 ` · ` 分隔；`「」` 括号只在至少一个段非空时才包裹整行。Line 1 / Line 3 的 omit 规则、emoji 选择（🤖 / 🗜 / 📁）均不变。

**Why F-52 改这三件事**：
1. **in = uncached + cache_creation + cache_read**：Tencent YB 文档 + Claude Code `/cost` 统计口径一致。之前的 `↓ in · ↻ cached` 拆法让用户得自己加两个数才知道"in 总共多少"，违反 footer 一次成型的目的。
2. **加 `X%` 段**：F-52 引入的 `ContextWindowPct`（Doc 1 公式 = `used / contextWindow * 100`，`contextWindow` 是 bridge-local 变量，见 [`F-54`](./F-54-pi-contextwindow-from-get-state.md) §1.2）是 chat session 用户最关心的"距离 ceiling 还剩多少"指标，独立成段比塞进 `in / out` 自然。
3. **`$cost` 客户端不计算**：Anthropic API 的 `total_cost_usd` 已经把不同模型的差异化定价算好了，客户端维护 rate table 既过时又错。直接透传是唯一正解。

**实测样例 (F-52 后)**：

```
🤖 claude · opus-4-5
💰:「 20.5k / 1.5k · $0.087 」                                  # 无 ContextWindow 报回
🤖 claude · opus-4-5
💰:「 20.0k / 1.0k · 10.5% 」                                   # 有 X%，无 cost
🤖 claude · opus-4-5
💰:「 1.4M / 18.0k · 99.6% · $1.234 」                          # 大 turn，接近 ceiling
🤖 claude · opus-4-5
💰:「 $1.245 」                                                 # 只有 cost（极少见）
🤖 claude · opus-4-5
💰:「 100.0% 」                                                 # 满 context
```

**`abbrevTokens`**（未变）：

```go
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

**F-45 原 Format C 样例（保留以便 review 对照）**：

```
↓ 234 · ↻ 5.6k cached · ↑ 89 · Total 5.9k                    # 小 turn，无 agent/model
claude · opus-4-5 · ↓ 12.3k · ↻ 8.2k cached · ↑ 1.5k · Total 22.0k · $0.087   # 标准
claude · opus-4-5 · ↓ 156k · ↻ 1.2M cached · ↑ 18k · Total 1.37M · $1.245    # 大 turn
claude · ↓ 12.3k · ↑ 1.5k · Total 13.8k                                       # 无 model 无 cost 无 cache
claude · opus-4-5 · ↓ 12.3k · ↻ 8.2k cached · ↑ 1.5k · Total 22.0k          # 无 cost
```

> Format C 的对应代码已在 F-52 重构时删除，仅保留样例作为「之前怎么写」的考古参考。

### 1.7 Git Branch Tracking (F-48 follow-up)

**Why**：用户在 IM 里看不到当前的 workspace / branch / dirty 状态 — 每次要确认"我正在哪个 repo"都得跳到 terminal。Footer 加一行 git tracking 让 Feishu 卡片本身就是 ground truth：workspace 路径 + branch + 未提交 + 未跟踪 + 未推送。

**Format**（footer 第 3 行）：

```
📁 code/nightme · ⎇ main · ↑ 3 · ? 2 · ⇡ 2
```

| 段 | 来源 | Omit 规则 |
|---|---|---|
| 📁 `<workspace>` | `SessionContext.Workspace` = `s.Cwd` | 整段在 Workspace=="" 或 `GitStatus==nil` 时省略（review fix：non-git workspace 不显示误导性的 "⎇ ?"） |
| ⎇ `<branch>` | `GitStatus.Branch` | 永远显示（当行渲染时）；`Branch==""`（detached HEAD 在 git repo 内）→ 写 `?` |
| ↑ `<n>` | `GitStatus.Uncommitted` | `n==0` 省略 |
| ? `<n>` | `GitStatus.Untracked` | `n==0` 省略 |
| ⇡ `<n>` | `GitStatus.AheadOfRemote` | `HasUpstream==false \|\| n==0` 省略 |

**Workspace 显示规则**（简化版，Devin 拍板 2026-08-06）：
- **不加任何前缀**（既不 `~` 也不 `/`）—— 路径是什么就显示什么。理由：`~` 在 workspace 不在 HOME 下时会误导（不同 operator / 容器化 session / 非标准 HOME 布局）
- ≤ 2 个目录组件 → 完整显示：`/home/devin` → `home/devin`、`/tmp/foo` → `tmp/foo`、`/home/devin/code` → `devin/code`
- > 2 个目录组件 → 只显示最后两个：`/home/devin/code/nightme` → `code/nightme`、`/home/devin/code/nightme/internal` → `nightme/internal`、`/tmp/a/b/c` → `b/c`

**Arrow 选型**（与 F-45 约定一致）：
- `↑` / `⇡` / `?` — ASCII / Unicode 符号（非 emoji 字体），middle dot ` · ` 分隔
- `⎇` — Unicode Alternative Key Symbol (U+2387)，代表 branch
- `📁` — folder emoji，仅作 category header（与 F-45 line 1 🤖 / line 2 💰 风格一致）

**Stamping 规则**：
- 在 `cmd/nightme/run.go::newEventHandler` 的 4 个 main-chat kind 上 stamp
- 每次 stamp 都跑 `gtw.CollectStatus(s.Cwd, gtw.ExecGitRunner{})` —— **无缓存**，footer 永远反映当前 worktree
- Git 调用的 **3 秒 deadline**（review fix）：stalled NFS / broken .git/index 不能阻塞消息路径；超时返回 (nil, nil)，footer 静默省略 git 段
- `Workspace` = `s.Cwd`（immutable 字段，无锁读）
- `GitStatus` = parse 结果；非 git repo / git 失败 / git 超时 → `nil`（整段省略）
- stamp condition 扩展：`hasGit := gitSnap != nil && s.Cwd != ""`；其他 usage/model 条件不变

**Wire 形态**（`gateway.SessionContext`）：

```go
type SessionContext struct {
    Agent           string
    Model           string
    Usage *agent.UsageEvent
    Workspace       string                  // NEW (F-48)
    GitStatus       *gtw.GitStatusSnapshot  // NEW (F-48)
    CompactionCount int                     // NEW (F-49: 🗜 N 计数)
}

type GitStatusSnapshot struct {
    Branch        string  // empty when detached HEAD / not-a-repo
    Uncommitted   int     // M/A/D/R/C + 冲突 (UU/AA/DD/...)
    Untracked     int     // ??
    AheadOfRemote int     // 0 when no upstream
    HasUpstream   bool    // false for detached HEAD / new branch
}
```

**Render 路径**（`internal/channel/feishu/usage_footer.go::formatSessionFooterLines`）：
- 现有 line 1 / line 2 不变
- 新增 line 3：`formatGitLine(ctx)` 返回非空时 append
- `formatGitLine` 内部调 `formatWorkspacePath`（无 HOME 处理、≤ 2 组件完整、> 2 截尾）

**测试覆盖**：
- `internal/gtw/git_status_test.go` (NEW) — 12 case：clean / dirty / detached HEAD (3 sub) / no upstream / ahead+behind / conflicts / ignored / empty output / not-a-repo
- `internal/channel/feishu/usage_footer_test.go` — 新增 `TestFormatWorkspacePath`（17 case）+ `TestFormatGitLine_*`（8 case）+ `TestFormatSessionFooterLines_WithGitLine` / `_GitOnly`

**不变式**：
- `formatSessionFooterLines` 已存在测试全部通过（line 1/2 行为不变）
- `OutboundMessage.SessionContext` wire 兼容性保持：Channel 不读 `Workspace` / `GitStatus` 时零影响
- 不在 Channel 调 git（保持 F-08 "Channel is dumb" 边界）—— git CLI 只在 runtime stamp 时跑
- 无生产代码注入：测试用真实 mock git output + 直接构造 `SessionContext` 输入，不引入 test-only 变量

**F-48 PR scope**：
- `internal/gtw/git_status.go` (NEW)
- `internal/gtw/git_status_test.go` (NEW)
- `internal/gateway/messages.go` — `SessionContext` 加 2 个字段
- `cmd/nightme/run.go::newEventHandler` — stamp 时调用 `gtw.CollectStatus`
- `internal/channel/feishu/usage_footer.go` — line 3 渲染 + `formatWorkspacePath`
- `internal/channel/feishu/usage_footer_test.go` — 扩展测试

### 1.8 Compaction Counter (F-49 follow-up)

**Why**：F-45 的 Line 2 token 数字是 cumulative across the entire AgentSession。当 agent 执行 context compaction（截断/摘要化输入）后，cumulative 仍持续累加，导致用户看到的 total tokens 远超上下文窗口上限，**无法判断**是"真的用了那么多"还是"已压缩多次但 cumulative 不反映"。用户在 IM 视角下看到的是 mystery number。

F-49 给 Line 1 加 `· 🗜 N` 段（compaction 计数），同时把 Line 2 的 token 部分改成"since-last-compaction 归零"语义，而 `$cost` 保留为 lifetime spend——两个目的清晰分离。

**完整设计**：见 [`F-49-compaction-counter.md`](./F-49-compaction-counter.md)。本节只列与 F-45 footer 的交互点。

#### 1.8.1 Footer 渲染差异

**改前**（典型长 session，已 compact 3 次）：

```
🤖 claude · opus-4-5
� ↓ 156k · ↻ 1.2M cached · ↑ 18k · Total 1.37M · $1.245
📁 code/nightme · ⎇ main · ↑ 3 · ? 2 · ⇡ 2
```

**改后**（同 session，加 🗜 段 + token 归零）：

```
🤖 claude · opus-4-5 · 🗜 3
💰 ↓ 5k · ↻ 2k · ↑ 0.8k · 7.8k · $1.245
📁 code/nightme · ⎇ main · ↑ 3 · ? 2 · ⇡ 2
```

**关键变化**：
- **Line 1 末尾**追加 `· 🗜 N`（仅 N>0）
- **Line 2 token 部分**语义反转：从"lifetime"变成"since-last-compaction"——压缩后归零，从下一次 EventUsage 重新累加
- **Line 2 `$cost`** 保持累加：lifetime spend 跨压缩单调累加

**用户视角解读**：
- `🗜 3` + `↓ 5k · Total 7.8k` → "当前上下文用了 7.8k（压缩后），3 次压缩前实际用过 1.37M tokens"
- `$1.245` → "这个 Session 总共花了 $1.245，无论压缩几次都不会变"

#### 1.8.2 两个 token 目的 → 两个独立 metric

| 用户目的 | Footer 段 | 字段 | 压缩时行为 |
|---|---|---|---|
| **总耗费**（lifetime spend） | `$X.XXX` | `CostUSD` | **保留**，单调累加 |
| **当前 Session 上下文用量** | `↓ X · ↻ X · ↑ X · Total X` | 4 个 token 字段 | **归零**，重新累加 |

**为什么这样切**：
- `CostUSD` 是货币量——花了不会退，跨压缩累加
- 4 个 token 字段是**输入窗口**快照——压缩后输入窗口被截断，下一个 turn 的 input 自然变小；归零后从下一次 EventUsage 重新累加，**无需特殊处理**

#### 1.8.3 Bridge 抽象（与 F-45 的解耦点）

F-45 假设 bridge 只产生 `EventAgentConnected` / `EventUsage` / `EventResult` / `EventText` / `EventToolStart` / `EventToolEnd`。**F-49 新增一个 consumer**：`EventCompaction`。但 bridge 层有协议差异：

- **Pi**：`compaction_start` + `compaction_end` 两条
- **Claude Code**：result subtype `compact` / `compaction` 一条

**抽象归抽象 / 具体归具体**（SPEC §1.4）：bridge 自己消化协议差异，runtime 一视同仁。

```
                  ┌─────────────────────────────────────────────┐
  Pi 协议         │  compaction_start → [suppressed]            │
  compaction_start│  compaction_end   → EventCompaction × 1    │
  compaction_end  │                                             │
                  └─────────────────────────────────────────────┘
                                ↓
                  ┌─────────────────────────────────────────────┐
  Claude 协议     │  result.subtype == "compact" /              │
  result.subtype  │  "compaction" → EventCompaction × 1         │
                  └─────────────────────────────────────────────┘
                                ↓
                  ┌─────────────────────────────────────────────┐
  runtime handler │  case EventCompaction:                       │
  (协议无关)      │    as.RecordCompaction()                     │
                  │    // 不判断 Subtype，不产生 Outbound        │
                  └─────────────────────────────────────────────┘
```

详见 [`F-49 §1.3`](./F-49-compaction-counter.md) 与 [`F-32 §2.3`](./F-32-pi-rpc-bridge.md) 改动说明。

#### 1.8.4 F-45 routing 表更新（删除 OutCompaction）

F-45 §1.2 的 OutboundKind → Footer 路由表中 `OutCompaction` 一行删除：

```diff
  | OutboundKind | 表面 | Footer |
  |---|---|---|
  | `OutReply` | **ReplyInThreadAndChat**（每 chunk） | ✅ footer 在文末 |
  | `OutResult` | ReplyInThreadAndChat | ✅ footer 在文末 |
  | `OutTaskCreate` / `OutTaskUpdate` | **Rolling-log receipt card**（Tasks） | ✅ footer 在 checklist 末尾 |
  | `OutCard` (permission) | Top-level Create | ❌ 不带 footer（短状态消息） |
  | `OutCommandReply` | Top-level Create | ❌ 不带 footer |
- | `OutThinking` / `OutToolStart` / `OutToolEnd` | `ReplyInThread` | ❌ 不带 footer（thread 视觉独立） |
- | `OutCompaction` | `ReplyInBoth` | ❌ 不带 footer（短暂 marker） |
  | `OutMessageState` | AddReaction | ❌ 不带 footer |
  | `OutInit` / `OutUsage` | Silent drop（F-44 不变） | — |
```

**`OutCompaction` kind 整体删除**（不是"保留但 runtime 不发"）——runtime handler 不再产生 OutboundMessage；channel adapter 不再有 case 处理。理由见 [`F-49 §1.9`](./F-49-compaction-counter.md)。

#### 1.8.5 SessionContext 扩展

```go
type SessionContext struct {
    Agent           string
    Model           string
    Usage *agent.UsageEvent
    Workspace       string                  // F-48
    GitStatus       *gtw.GitStatusSnapshot  // F-48
    CompactionCount int                     // F-49: 🗜 N 计数
}
```

#### 1.8.6 与 F-45 §1.5 stamping 规则的交互

F-45 §1.5 在 4 个 main-chat Kind 上 stamp SessionContext。F-49 不改变这一规则——只在 stamp 时多填一个 `CompactionCount` 字段。Stamp condition 扩展（[`F-49 §1.8`](./F-49-compaction-counter.md)）：

```go
// 既有 condition（来自 F-45 §2.5 改动 C）
if snap.InputTokens != 0 || snap.OutputTokens != 0 ||
    snap.CacheCreationInputTokens != 0 ||
    snap.CacheReadInputTokens != 0 || snap.CostUSD != 0 ||
    s.Model() != "" || hasGit {
    out.SessionContext = &gateway.SessionContext{...}
}

// F-49 扩展：compactionCount > 0 也触发 stamp
if snap.InputTokens != 0 || ... || hasGit ||
    s.CompactionCount() > 0 {
    out.SessionContext = &gateway.SessionContext{
        // ...
        CompactionCount: s.CompactionCount(),
    }
}
```

实际上 `CompactionCount() > 0` 几乎不会单独触发 stamp（compaction 必然发生在至少 1 个 turn 之后，前几个 OR 条件已覆盖），但保持对称——理论上 `/new` 后立刻 compaction 也能显示 🗜 段。

#### 1.8.7 F-49 与 F-45 §1.8 (State 流转) 的交互

State 流转图（见下 §1.9）加一条 compaction 分支：

```
  SetModel(ev.Connected.Model)     ← EventAgentConnected 触发；idempotent
  AccumulateUsage(ev.Usage)   ← EventUsage 触发（每个 turn 一次）
  ...
+ RecordCompaction()          ← EventCompaction 触发；count++ + 4 token 字段归零
+                              CostUSD 保留；后续 EventUsage 从零重新累加 token
  ...
  /new → ResetCumulative      ← 用户主动重置（compactionCount + usage 全清零）
```

#### 1.8.8 F-49 不在 F-45 PR scope

F-49 是独立 PR（详见 §7 实施计划），不在 F-45 当初落地范围内。F-45 的 footer helper `formatSessionFooterLines` 在 F-49 PR 里加 `🗜 N` 段；其余文件（AgentSession / SessionContext / newEventHandler / bridges）都是 F-49 新增改动，与 F-45 已落地的代码解耦。

### 1.9 State 流转

```
AgentSession 生命周期:
  [spawn]
    ↓
  SetModel(ev.Connected.Model)     ← EventAgentConnected 触发；idempotent
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
- `ResumeID`：EventAgentConnected 时捕获，**永不重置**（除非 `/new` 通过 bridge `New()` 让 agent 重发 EventAgentConnected）
- `Model`：EventAgentConnected 时捕获，**永不重置**（同 ResumeID 语义）
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
    
    // F-45: model captured on first EventAgentConnected. Persists for
    // the lifetime of the AgentSession IDENTITY (until /new
    // re-emits EventAgentConnected with a new model — rare).
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

**改动 A**：`EventAgentConnected` 处理块里加 Model 捕获：

```go
// 既有：
if ev.Kind == agent.EventAgentConnected && ev.Connected != nil && ev.Connected.SessionID != "" {
    s.SetResumeID(ev.Connected.SessionID)
    if mgr != nil { _ = mgr.PersistAgentSession(s) }
}

// NEW:
if ev.Kind == agent.EventAgentConnected && ev.Connected != nil && ev.Connected.Model != "" {
    s.SetModel(ev.Connected.Model)
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

**改动 E**：（已废弃）早期的 OutResult 缓冲机制。后续 bridge 重构把 Usage 合并到 ResultEvent 之后不再需要 —— 见 §2.5 changelog 末尾的 "single-event design" 注释。

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

**bridge 协议**：零变化。bridges 仍发 `EventAgentConnected` / `EventUsage`，runtime 负责捕获并 stamp。

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
- **bridges 协议零变化**（仍发 EventAgentConnected / EventUsage，runtime 翻译）
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
