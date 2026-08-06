# F-49: Context Compaction Counter + Footer Line 1 � N

> **Status**: 📝 设计阶段（doc-first，2026-08-06）
> **Milestone**: v1.3.x
> **Scope**:
> - `internal/agent/agent.go` — `CompactionEvent` 删除 `Subtype` 字段（空 struct）；从 `AgentEvent` 删除 `Compaction *CompactionEvent` 字段（runtime 不再 translate 出去）
> - `internal/bridge/pi/translate.go` — `compaction_start` 直接 `return nil, nil`（屏蔽瞬态信号）；`compaction_end` 仍 emit `EventCompaction`（不带 Subtype）
> - `internal/bridge/claudecode/stream.go` — `compact` / `compaction` subtype 不再赋值给 `CompactionEvent.Subtype`（已删除）
> - `internal/chatsession/agentsession.go` — `compactionCount int` 字段（由 `cumulativeUsageMu` 守护）；`RecordCompaction()`（count++ + 4 token 字段归零 + 保留 `CostUSD`）；`CompactionCount() int`；`ResetCumulative` / `Entry` / `PersistIfDirty` / `FromAgentSessionEntry` 同步
> - `internal/registry/agent_session_entry.go` — `AgentSessionEntry.CompactionCount int`；`AgentSessionFileVersion` +1
> - `internal/gateway/messages.go` — `SessionContext.CompactionCount int`；**`OutboundMessage.Kind = OutCompaction` 整条路径删除**（gateway.translate.go 不再产生）
> - `cmd/nightme/run.go::newEventHandler` — `case agent.EventCompaction: as.RecordCompaction()`（无 Subtype 判断、无 Outbound 产生）；`sessionContextInto` stamp `CompactionCount`
> - `internal/channel/feishu/usage_footer.go` — Line 1 末尾追加 `· 🗜 N`（仅 N>0）
> - `internal/channel/feishu/adapter.go` — 删除 `OutCompaction` case（不再有 thread reply "✶ Compacting…"）
> - 文档同步（[`F-45 §1.8`](./F-45-session-footer.md) follow-up；[`SPEC.md` §0.13](../SPEC.md) changelog；[`channel/feishu.md` §13.25](../channel/feishu.md) decision；[`F-32`](./F-32-pi-rpc-bridge.md) bridge 行为更新；[`F-37`](./F-37-tool-thread-routing.md) 移除 OutCompaction thread 路由；[`F-25`](./F-25-rolling-log.md) 移除 receipt entry 映射）
>
> **Depends on**: F-45 (SessionContext footer), F-32 (Pi bridge), F-37 (thread routing — 移除), F-25 (rolling-log — 移除)
> **Related**: [`SPEC.md`](../SPEC.md) §0.13（本文落地）；[`channel/feishu.md`](../channel/feishu.md) §13.25；[`F-45 §1.8`](./F-45-session-footer.md)；[`F-32 §2.3`](./F-32-pi-rpc-bridge.md)；[`F-37 §2.1`](./F-37-tool-thread-routing.md)（移除 OutCompaction 行）；[`F-25 §3.1.1`](./F-25-rolling-log.md)（移除 OutCompaction thread reply）

---

## 0. 背景

### 0.1 用户痛点

当前 footer 的 `💰 ↓ X · ↻ X · ↑ X · Total X` 是 **cumulative across the entire AgentSession**（F-45 决策）。当 agent 执行 context compaction 时（Claude Code 在接近上下文窗口上限时自动 compact；Pi 显式 emit `compaction_start/end`），本轮输入被截断/摘要化，但 cumulative 仍持续累加。结果：

```
💰 ↓ 156k · ↻ 1.2M cached · ↑ 18k · Total 1.37M · $1.245
```

用户看到 1.37M total **远超** 上下文窗口上限（如 200k），**无法判断**到底是"真的用了 1.37M 上下文"（不可能，会爆窗口）还是"agent 已 compact 多次但 cumulative 不反映"。从 IM 视角看是 mystery number。

**用户原话**（2026-08-06）：
> 不然现在Tokens有的统计量太大了，已经超过它的上下文上限了，这明显是它执行的压缩。但是我们却不是很知道，所以我们很难判断。

### 0.2 现状

**EventCompaction 已存在但 runtime 完全不消费**：
- `internal/agent/agent.go:109` `EventCompaction` Kind 定义存在
- `internal/agent/agent.go:374` `CompactionEvent{Subtype string}` payload
- `internal/bridge/pi/translate.go` emit `compaction_start` + `compaction_end` 各一条
- `internal/bridge/claudecode/stream.go` emit `compact` / `compaction` 各一条
- `internal/gateway/translate.go` 把 `EventCompaction` translate 成 `OutCompaction`
- `internal/channel/feishu/adapter.go` `Send` case `OutCompaction` → `ReplyInThreadAndChat` 发 `✶ Compacting conversation…`（F-37 决策）
- `internal/channel/feishu/receipt_event.go` `eventToEntry` 对 `EventCompaction` 返回 `(_, false)`（不进 receipt）
- **`runtime handler 完全不感知`**——既不累加也不持久化，只是把它当普通事件透传到 IM

**问题**：count 信息全丢失，footer 显示的 cumulative 数字对用户来说**没有 compaction 这个调节变量**。

### 0.3 用户澄清（2026-08-06 对话）

**Q：每个 agent 的协议差异谁负责消化？**
> **Bridge 层负责**。"每个 Agent 自己去桥接它的协议"——Pi 发 start + end 两条，runtime 看到的是**一次**事件；Claude Code 发一条，runtime 看到一次。runtime 一视同仁，**不基于 Subtype 字符串做 dispatch**。抽象归抽象 / 具体归具体不变式要求协议差异消化在 bridge。

**Q：emoji 选什么？**
> **🗜** (U+1F5DC, Unicode 正式名 "COMPRESSION")。语义最精确，配合数字 N 表达"被压缩了 N 次"。

**Q：压缩时 token 怎么处理？**
> **4 个 token 字段归零（InputTokens / CacheCreationInputTokens / CacheReadInputTokens / OutputTokens），`CostUSD` 保留**。理由：
> - Token 是"当前 Session 上下文用量"——压缩后归零重新计算，自然反映"since-last-compaction"
> - Cost 是"lifetime 耗费"——跨压缩单调累加，反映"这个 AgentSession 总共花了多少钱"
> - 两个目的各自清晰，不混淆
>
> **不需要做"压缩进行中"的瞬时显示**——runtime 不发 Outbound，用户看不到任何中间过程，只看到 count 累加 + footer 数字重置。

**Q：`CompactionEvent.Subtype` 字段还有用吗？**
> **没用，删掉**。runtime 不基于它 dispatch，bridge 也不再依赖它传递信息。字段空着就是死代码。未来要加字段时再加。

**Q：`OutCompaction` kind 还要保留吗？**
> **删掉**。runtime 不再产生 OutboundMessage 给 channel；channel adapter 也不再需要 case 处理。thread reply "✶ Compacting conversation…" 移除。

---

## 1. 设计

### 1.1 视觉对比

**改前**（典型长 session，已 compact 3 次）：

```
🤖 claude · opus-4-5
💰 ↓ 156k · ↻ 1.2M cached · ↑ 18k · Total 1.37M · $1.245
📁 code/nightme · ⎇ main · ↑ 3 · ? 2 · � 2
```

用户看到 1.37M total **不知所云**：超过上下文窗口 6 倍，怀疑数据错误或 agent bug。

**改后**（同样 session，加了 🗜 计数 + token 归零语义）：

```
🤖 claude · opus-4-5 · 🗜 3
💰 ↓ 5k · ↻ 2k · ↑ 0.8k · 7.8k · $1.245
📁 code/nightme · ⎇ main · ↑ 3 · ? 2 · ⇡ 2
```

**关键变化**：
- Line 1 末尾追加 `· � 3`——直接告诉用户"本 Session 已 compact 3 次"
- Line 2 的 token 部分从"累计 1.37M"变成"压缩后 7.8k"——直观显示**当前上下文用量**
- `· $1.245` **保持不变**——lifetime cost 跨压缩单调累加，与 token 数字形成对比

**用户视角解读**：
- `🗜 3` + `↓ 5k · Total 7.8k` = "当前会话上下文用了 7.8k，离 200k 上限还很远，但这是压缩后的数字，3 次压缩之前实际用过 1.37M tokens"
- `$1.245` = "这个 Session 总共花了 $1.245，无论压缩几次都不会变"

### 1.2 两个 token 目的 → 两个独立 metric

| 用户目的 | Footer 段 | 字段 | 压缩时行为 |
|---|---|---|---|
| **总耗费**（lifetime spend） | `$1.245` | `CostUSD` | **保留**，单调累加 |
| **当前 Session 上下文用量** | `↓ 5k · ↻ 2k · ↑ 0.8k · 7.8k` | 4 个 token 字段 | **归零**，重新累加 |

**为什么这样切**：
- `CostUSD` 是货币量，跟"花了多少"绑定，不会因为压缩退钱——跨压缩累加
- 4 个 token 字段是**输入窗口**的快照——压缩后输入窗口被截断/摘要，下一个 turn 的 input 自然变小，所以归零后**自然从下一次 EventUsage 重新累加**，无需特殊处理

### 1.3 Bridge 抽象（核心不变式）

```
                  ┌─────────────────────────────────────────────�
  Pi 协议         │  compaction_start → [suppressed]            │
  compaction_start│  compaction_end   → EventCompaction × 1    │
  compaction_end  │                                             │
                  └─────────────────────────────────────────────┘
                                ↓
                  ┌─────────────────────────────────────────────┐
  Claude 协议     │  result.subtype == "compact" /              │
  result.subtype  │  "compaction" → EventCompaction × 1         │
                  └─────────────────────────────────────────────�
                                ↓
                  ┌─────────────────────────────────────────────┐
  runtime handler │  case EventCompaction:                       │
  (协议无关)      │    as.RecordCompaction()                     │
                  │    // 不判断 Subtype，不产生 Outbound        │
                  └─────────────────────────────────────────────┘
                                ↓
                  ┌─────────────────────────────────────────────┐
  AgentSession    │  RecordCompaction:                          │
  (累计 + 归零)   │    compactionCount++                        │
                  │    cumulativeUsage.InputTokens        = 0   │
                  │    cumulativeUsage.CacheCreation...   = 0   │
                  │    cumulativeUsage.CacheRead...       = 0   │
                  │    cumulativeUsage.OutputTokens       = 0   │
                  │    // CostUSD 保留                          │
                  │    cumulativeDirty = true                   │
                  └─────────────────────────────────────────────┘
                                ↓
                  ┌─────────────────────────────────────────────┐
  Channel         │  Footer Line 1 末尾：                       │
  (渲染)          │    🤖 Agent · Model · 🗜 N                  │
                  └─────────────────────────────────────────────┘
```

**关键约束**：
- **runtime 不基于 Subtype 字符串 dispatch**——`CompactionEvent.Subtype` 字段**不存在**
- **runtime 不产生 OutboundMessage 给 channel**——`OutCompaction` kind **不存在**
- **bridge 各自负责把协议差异消化掉**——Pi 屏蔽 start；Claude Code 自然就是 1 条
- **新 agent 接入时**只需保证"一次压缩周期 = 一个 EventCompaction"——runtime 一视同仁

### 1.4 AgentSession 新增 API

```go
// internal/chatsession/agentsession.go

type AgentSession struct {
    // ... 既有字段 ...

    // F-49: cumulative compaction count + per-cycle token stats.
    // 由 cumulativeUsageMu 守护（与 cumulativeUsage 共用同一把锁，
    // 因为 RecordCompaction 原子修改两者）。Persists across daemon
    // restarts; cleared only by /new (see F-45 §1.5).
    cumulativeUsageMu sync.RWMutex
    cumulativeUsage   agent.UsageInfo
    compactionCount   int       // ← NEW
    cumulativeDirty   bool
}

// RecordCompaction atomically:
//   1. increments compactionCount;
//   2. zeroes the four token fields of cumulativeUsage
//      (InputTokens, CacheCreationInputTokens, CacheReadInputTokens,
//      OutputTokens), preserving CostUSD;
//   3. marks cumulativeDirty so the next PersistIfDirty flushes
//      both the new count and the post-reset token snapshot.
//
// The token reset makes Footer Line 2 (↓ ↻ ↑ total) reflect
// "since-last-compaction" — i.e. the agent's current context window
// usage, while $cost stays as lifetime spend. See F-49 §1.2.
func (as *AgentSession) RecordCompaction() {
    as.cumulativeUsageMu.Lock()
    defer as.cumulativeUsageMu.Unlock()
    as.compactionCount++
    as.cumulativeUsage.InputTokens = 0
    as.cumulativeUsage.CacheCreationInputTokens = 0
    as.cumulativeUsage.CacheReadInputTokens = 0
    as.cumulativeUsage.OutputTokens = 0
    // CostUSD deliberately preserved.
    as.cumulativeDirty = true
}

// CompactionCount returns the cumulative number of completed
// compaction cycles observed on this AgentSession. Snapshot under
// RLock; safe for concurrent read alongside RecordCompaction.
func (as *AgentSession) CompactionCount() int {
    as.cumulativeUsageMu.RLock()
    defer as.cumulativeUsageMu.RUnlock()
    return as.compactionCount
}
```

**ResetCumulative 同步修改**（`/new` 命令清零所有累计，包括 compactionCount）：

```go
func (as *AgentSession) ResetCumulative() {
    as.cumulativeUsageMu.Lock()
    as.cumulativeUsage = agent.UsageInfo{}
    as.compactionCount = 0  // ← NEW
    as.cumulativeDirty = true
    as.cumulativeUsageMu.Unlock()
}
```

**Entry() 序列化同步修改**：

```go
func (as *AgentSession) Entry() *registry.AgentSessionEntry {
    as.cumulativeUsageMu.RLock()
    cum := as.cumulativeUsage
    cc := as.compactionCount  // ← NEW
    as.cumulativeUsageMu.RUnlock()
    return &registry.AgentSessionEntry{
        // ... 既有字段 ...
        CumulativeUsage: &cum,
        CompactionCount: cc,  // ← NEW
        Model:           as.Model(),
    }
}
```

**FromAgentSessionEntry 还原同步修改**：

```go
if e.CumulativeUsage != nil {
    as.cumulativeUsage = *e.CumulativeUsage
}
as.compactionCount = e.CompactionCount  // ← NEW：JSON 默认零值兼容老数据
```

### 1.5 SessionContext 扩展

```go
// internal/gateway/messages.go
type SessionContext struct {
    // ... 既有字段（Agent / Model / CumulativeUsage / Workspace / GitStatus）...

    // F-49: cumulative count of completed context compactions on
    // this AgentSession. 0 when never compacted. Persists across
    // daemon restarts; cleared only by /new. Sourced from
    // AgentSession.CompactionCount at the same instant as
    // CumulativeUsage so the footer Line 1 (🗜 N) and Line 2 (↓ ↻
    // ↑ total) tell a coherent story: "lifetime cost grew by $X,
    // context window was reset and now totals Y since the last of
    // N compactions".
    CompactionCount int `json:"compactionCount,omitempty"`
}
```

### 1.6 Footer Line 1 渲染规则

```go
// internal/channel/feishu/usage_footer.go
// Line 1: identity (🤖 Agent · Model · 🗜 N).
idParts := []string{"🤖"}
if ctx.Agent != "" {
    idParts = append(idParts, ctx.Agent)
}
if ctx.Model != "" {
    idParts = append(idParts, "·", ctx.Model)
}
if ctx.CompactionCount > 0 {
    idParts = append(idParts, "·", "�", strconv.Itoa(ctx.CompactionCount))
}
if len(idParts) > 1 {
    lines = append(lines, strings.Join(idParts, " "))
}
```

**Segment 规则**：
| 段 | 来源 | Omit 规则 |
|---|---|---|
| 🤖 | literal | 永远显示 |
| `<Agent>` | `ctx.Agent` | `""` 时省略 |
| `· <Model>` | `ctx.Model` | `""` 时省略 |
| `· 🗜 <N>` | `ctx.CompactionCount` | **仅 N > 0 时显示**（沿用 F-45 §1.6 zero-omit 约定） |

**实测样例**：

```
🤖 claude                                         # 无 model 无 compaction
🤖 claude · opus-4-5                              # 标准
🤖 claude · opus-4-5 · � 3                       # 3 次压缩
🤖 claude · opus-4-5 · � 1                       # 1 次压缩
```

**Glyph 选型**：🗜 (U+1F5DC, Unicode 正式名 "COMPRESSION")——语义零歧义；与 F-45 line 1 🤖 / F-45 line 2 💰 / F-48 line 3 📁 emoji category header 风格一致。

### 1.7 Bridge 改动（细节）

**Pi bridge (`internal/bridge/pi/translate.go`)**：

```go
case "compaction_start":
    // F-49: 屏蔽瞬态信号。runtime handler 不区分 start / end ——
    // 任何 EventCompaction 都计为一次完成的压缩周期。如果 start
    // 不屏蔽，runtime 会被双数（Pi 一个压缩周期 = start + end 两条）。
    // 原因在 F-49 §1.3 "Bridge 抽象"。
    return nil, nil

case "compaction_end":
    // F-49: 仍 emit EventCompaction（不带 Subtype，因为字段已删除）。
    return []agent.AgentEvent{{
        Kind: agent.EventCompaction,
    }}, nil
```

**Claude Code bridge (`internal/bridge/claudecode/stream.go`)**：

```go
// F-49: 之前 emit EventCompaction{Subtype: subtype}（subtype="compact" / "compaction"）。
// 现在 Subtype 字段已删除，bridge 只 emit 一个 marker EventCompaction。
return agent.AgentEvent{
    Kind: agent.EventCompaction,
}, true
```

### 1.8 Runtime handler 改动

**`cmd/nightme/run.go::newEventHandler`**：

```go
// F-49: EventCompaction 不再产生 OutboundMessage（无 OutCompaction kind）。
// 也不再判断 Subtype（字段已删除）。runtime 一视同仁，任何 EventCompaction
// 都视为一次完成的压缩周期——bridge 层负责把协议差异消化掉。
case agent.EventCompaction:
    s.RecordCompaction()
    if logger != nil {
        logger.Debug("runtime: compaction observed",
            "agent", s.Agent,
            "count", s.CompactionCount())
    }
```

**`sessionContextInto` stamp 扩展**：

```go
out.SessionContext = &gateway.SessionContext{
    Agent:           s.Agent,
    Model:           s.Model(),
    CumulativeUsage: snap,
    CompactionCount: s.CompactionCount(),  // ← NEW
    Workspace:       s.Cwd,
    GitStatus:       gitSnap,
}
```

**stamp condition 扩展**：现有 condition 是 `usage 或 model 或 git 至少一个非空`。新增 `|| s.CompactionCount() > 0`——这样即使前 3 个都还没拿到，count 也能让 footer Line 1 显示出来。但实际上 compaction 必然发生在至少 1 个 turn 之后，所以这条 OR 几乎不会触发（除非 `/new` 后立刻发生 compaction，罕见）。

### 1.9 OutCompaction kind 删除

**为什么删**：
- runtime handler 不再产生 OutboundMessage for EventCompaction（§1.8）
- gateway.translate.go 不再有 `case agent.EventCompaction:` 分支
- 没有任何 producer，自然没有 consumer

**删什么**：
- `internal/gateway/messages.go` `OutboundKind` 常量删除 `OutCompaction`
- `internal/channel/feishu/adapter.go` `Send` case `OutCompaction` 删除
- `internal/channel/feishu/receipt_event.go` `eventToEntry` 对 EventCompaction 的 `(_, false)` 分支删除
- `internal/channel/feishu/receipt.go` `Append` 对 EventCompaction 的 silent PATCH 分支删除
- 文档删除所有 OutCompaction 引用（F-37 §2.1 表行；F-25 §3.1.1 thread reply 行；F-25 §2.4 silent PATCH 段）

**为什么不是保留 OutCompaction + runtime 不发**：
- 死代码——没有 producer 的 kind 就是死代码
- 删干净符合 "future 再说" 原则（要加字段 / 新行为时再加新 kind）

### 1.10 CompactionEvent / AgentEvent 字段删除

**`internal/agent/agent.go`**：

```go
// 改动前
type CompactionEvent struct {
    Subtype string
}

type AgentEvent struct {
    Kind EventKind
    // ...
    Compaction *CompactionEvent  // ← 删
    // ...
}

// 改动后
// CompactionEvent 是 EventCompaction 的 payload —— 当前没有字段，
// 纯粹作为 marker 存在。Bridge 各自负责把协议差异消化成"一个
// EventCompaction = 一次完成的压缩周期"。未来要加字段（如压缩后
// token 数）时在此扩展。
type CompactionEvent struct{}

type AgentEvent struct {
    Kind EventKind
    // ...
    // Compaction 字段删除 —— runtime 不再基于此指针判别类型，
    // Kind == EventCompaction 已是唯一判别依据。
    // ...
}
```

**为什么不留 `Compaction *CompactionEvent` 指针作为 marker**：
- 字段无值（空 struct 指针只是 `Kind` 的冗余表示）
- 删掉减少字段扫描，让 `AgentEvent` 更紧凑
- 未来真要加字段时一并加（避免现在留个空 marker 后续还要改）

**Bridges 同步修改**：
- Pi translate.go：`Kind: agent.EventCompaction`（不再附 `Compaction: &agent.CompactionEvent{}`）
- Claude Code stream.go：同上

---

## 2. 文件 & 接口

### 2.1 `internal/agent/agent.go`

**改动 A**：`CompactionEvent` 删除 `Subtype` 字段，变空 struct。
**改动 B**：`AgentEvent` 删除 `Compaction *CompactionEvent` 字段。
**改动 C**：`EventKind.String()` 对 `EventCompaction` 返回 `"compaction"` 不变（仅 debug）。

### 2.2 `internal/bridge/pi/translate.go`

**改动**：`compaction_start` case `return nil, nil`；`compaction_end` case 去掉 `Compaction:` 字段。

### 2.3 `internal/bridge/claudecode/stream.go`

**改动**：emit `EventCompaction` 时不再赋值 `Compaction: &CompactionEvent{Subtype: ...}`。

### 2.4 `internal/chatsession/agentsession.go`

**改动 A**：struct 加 `compactionCount int` 字段（由 `cumulativeUsageMu` 守护）。
**改动 B**：加 `RecordCompaction()` 和 `CompactionCount() int` 方法。
**改动 C**：`ResetCumulative` 同时清零 `compactionCount`。
**改动 D**：`Entry()` 拷出 `compactionCount`。
**改动 E**：`FromAgentSessionEntry` 还原 `compactionCount = e.CompactionCount`（默认 0 兼容老数据）。

### 2.5 `internal/registry/agent_session_entry.go`

**改动 A**：`AgentSessionEntry` 加 `CompactionCount int json:"compactionCount,omitempty"`。
**改动 B**：`AgentSessionFileVersion` +1。

### 2.6 `internal/gateway/messages.go`

**改动 A**：`SessionContext` 加 `CompactionCount int json:"compactionCount,omitempty"`。
**改动 B**：`OutboundKind` 删除 `OutCompaction` 常量。

### 2.7 `internal/gateway/translate.go`

**改动**：删除 `case agent.EventCompaction:` 分支（不再产生 OutboundMessage）。

### 2.8 `cmd/nightme/run.go::newEventHandler`

**改动 A**：`case agent.EventCompaction: s.RecordCompaction(); logger.Debug(...)`——**不**走 `gateway.Translate`，**不**产生 OutboundMessage。
**改动 B**：`sessionContextInto` stamp `CompactionCount`。
**改动 C**：stamp condition 加 `|| s.CompactionCount() > 0`（理论上不会触发但保持对称）。

### 2.9 `internal/channel/feishu/usage_footer.go`

**改动**：Line 1 末尾追加 `· 🗜 N`（仅 N>0）。需要 import `strconv`。

### 2.10 `internal/channel/feishu/adapter.go`

**改动**：删除 `Send` case `gateway.OutCompaction` 分支（不再有 OutCompaction kind）。

### 2.11 `internal/channel/feishu/receipt_event.go`

**改动**：删除 `eventToEntry` 对 `agent.EventCompaction` 返回 `(_, false)` 的分支。

### 2.12 `internal/channel/feishu/receipt.go`

**改动**：删除 `Append` 对 `agent.EventCompaction` 的 silent PATCH 分支（不 bump eventCount / lastEventAt）。

---

## 3. 测试

### 3.1 单元测试

| 测试 | 文件 | 覆盖 |
|------|------|------|
| `TestRecordCompaction_BumpsCount` | `internal/chatsession/agentsession_meta_test.go` (EXTEND) | 调一次 RecordCompaction → CompactionCount() == 1；调两次 → == 2 |
| `TestRecordCompaction_ResetsTokens` | 同上 | 先 AccumulateUsage(5k in + 2k cache + 1k out + $0.05) → RecordCompaction → CumulativeUsage().InputTokens=0, CacheCreation=0, CacheRead=0, Output=0, **CostUSD=$0.05** |
| `TestRecordCompaction_Race` | 同上 | 100 goroutine × 1000 RecordCompaction → 最终 CompactionCount() == 100000 + race detector clean |
| `TestResetCumulative_ClearsCount` | 同上 | 累加 + RecordCompaction → ResetCumulative → CompactionCount() == 0 + CumulativeUsage 全零 |
| `TestEntry_RoundtripPreservesCount` | 同上 | Entry() → JSON marshal → unmarshal → FromAgentSessionEntry → CompactionCount() == 原值 |
| `TestFormatSessionFooter_Line1_WithClamp` | `internal/channel/feishu/usage_footer_test.go` (EXTEND) | SessionContext{Agent:"claude", Model:"opus", CompactionCount:3} → line 1 == "🤖 claude · opus-4-5 · 🗜 3" |
| `TestFormatSessionFooter_Line1_NoClamp` | 同上 | CompactionCount:0 → line 1 == "🤖 claude · opus-4-5"（无 � 段） |
| `TestFormatSessionFooter_Line1_CostAfterReset` | 同上 | 模拟累积后压缩：footer line 2 = "💰 ↓ 5k · ↻ 2k · ↑ 0.8k · 7.8k · $1.245"（cost 保留，token 归零） |
| `TestPiBridge_SuppressesCompactionStart` | `internal/bridge/pi/translate_test.go` (EXTEND) | 输入 `compaction_start` event → 返回 `nil, nil`；输入 `compaction_end` → 返回 1 条 EventCompaction |
| `TestClaudeCodeBridge_EmitsEmptyCompaction` | `internal/bridge/claudecode/stream_test.go` (EXTEND) | result subtype "compact" → emit 1 条 EventCompaction（无 Subtype 字段可断言） |
| `TestTranslate_NoOutboundForCompaction` | `internal/gateway/translate_test.go` (EXTEND) | 调 Translate(EventCompaction) → 返回 `nil, nil`（无 Outbound 产生） |
| `TestFeishuAdapter_NoOutCompactionCase` | `internal/channel/feishu/adapter_test.go` (EXTEND) | 验证 `OutCompaction` case 已删除（grep 反向断言） |
| `TestNewEventHandler_BumpsOnCompaction` | `cmd/nightme/run_test.go` (EXTEND) | 注入 EventCompaction → s.CompactionCount() == 1 + 无 Outbound 发送到 channel |

### 3.2 集成测试

| 测试 | 文件 | 覆盖 |
|------|------|------|
| `TestPiBridge_OneCycle_OneCompactionCount` | `internal/bridge/pi/translate_test.go` | 完整模拟一次压缩周期（compaction_start + compaction_end）→ runtime 收到 1 条 EventCompaction → CompactionCount == 1 |
| `TestRuntime_FullCycle_BumpAndReset` | `cmd/nightme/run_test.go` | 累加 5 个 turn usage → EventCompaction → 下一个 EventUsage 只加新值（不叠加旧的） → footer Line 2 显示"压缩后"数字 |
| `TestRuntime_FullCycle_CostPreserved` | 同上 | 同上 + cost 跨压缩累加不变 |

### 3.3 边界测试

| 测试 | 场景 |
|------|------|
| `TestCompactionEvent_NoFields` | `agent.CompactionEvent{}` 空 struct 编译通过 + `agent.CompactionEvent{} == agent.CompactionEvent{}` |
| `TestAgentEvent_NoCompactionField` | `agent.AgentEvent{Kind: EventCompaction}` 编译通过（不依赖已删除字段） |
| `TestLegacyAgentSessionEntry_ZeroCount` | 老 JSON 文件无 `compactionCount` 字段 → FromAgentSessionEntry → CompactionCount() == 0 |
| `TestFooter_NoNegativeCount` | CompactionCount < 0 永不会出现（runtime 不产生负值；老数据 0；新累加 +1） |

---

## 4. Migration & 兼容性

### 4.1 JSON 兼容性

**`agent_sessions.json`**：
- 旧文件无 `compactionCount` 字段 → Go JSON unmarshal 容忍 → 内存里 `int == 0` → footer 不显示 🗜 段（符合零值约定）
- 第一次写入新字段后，新文件包含 `compactionCount`——向后兼容（读端都容忍缺失）
- `AgentSessionFileVersion` +1：标记 schema 升级，但**读端不强制版本检查**（沿用 F-45 的宽容策略）

### 4.2 Wire 兼容性

**`OutboundMessage.Kind`**：
- **删除 `OutCompaction` 常量**——这是破坏性变更。但 Channel 实现都是内部编译（Feishu adapter），无外部 wire consumer
- Echo channel 同样无 OutCompaction 处理代码（grep 确认）
- 编译期保证：删除 OutCompaction 后所有 switch case 都更新（`go build` 会报错）

**`SessionContext.CompactionCount`**：
- 新增字段（`omitempty`）——Channel 不读时零影响
- Feishu adapter 读 → 渲染 Line 1 🗜 段

**`AgentEvent.Compaction` 字段**：
- **删除**——任何引用此字段的代码编译失败。grep 全仓库确保只有 bridge / translate 引用
- bridges 同步删除 `Compaction: ...` 赋值

### 4.3 bridge 协议

- **Pi 行为变更**：`compaction_start` 不再产生 EventCompaction（之前产生 `EventCompaction{Subtype:"start:..."}`）
  - 兼容性：如果未来 Pi 改回只发 start 不发 end，runtime 会漏数；当前 Pi 协议保证两条都发，无问题
  - 文档更新：`docs/feat/F-32-pi-rpc-bridge.md §2.3` wire translation 表更新
- **Claude Code 行为变更**：emit EventCompaction 时不再带 Subtype 字段（字段已删除）
  - 兼容性：Subtype 仅做 debug，无功能性副作用

### 4.4 runtime handler 行为

- **新增副作用**：EventCompaction 会触发 AgentSession 累加（包括内存写 + dirty flag）+ PersistIfDirty 后续落盘
- **删除副作用**：EventCompaction 不再产生 OutboundMessage——channel 不再发任何 thread reply / receipt update / "Compacting…" marker
- 用户视角：从"看到 transient 提示"变成"footer Line 1 数字安静增长"

---

## 5. 不变式总结

**F-49 删字段 + 加 counter + 改 footer，但保留**：

### 5.1 抽象 / 具体边界（SPEC §1.4 强制）

- ✅ **bridge 消化协议差异**——Pi 屏蔽 start、Claude Code 自然 1 条、runtime 一视同仁
- ✅ **runtime 不基于 Subtype dispatch**——Subtype 字段不存在，runtime 不可能 string-sniff
- ✅ **Channel 不感知协议**——Feishu adapter 只读 SessionContext.CompactionCount，不知道它来自 Pi 还是 Claude Code

### 5.2 F-45 footer 不变式

- ✅ **Footer 仍是 3 行结构**：Line 1 identity + Line 2 tokens/cost + Line 3 git
- ✅ **Line 1 仍是 `🤖 <Agent> · <Model>` 起手**——🗜 段是 append，不是 replace
- ✅ **每段独立 omit**——零值不显示（🗜 在 count==0 时不显示）
- ✅ **`· ` middle-dot 分隔符**——与 F-37/F-44/F-48 一致

### 5.3 Channel / Runtime / Bridge 职责

- ✅ **Bridge 是 EventCompaction 的唯一 producer**——runtime 不造 EventCompaction
- ✅ **Runtime 是 AgentSession 元数据的唯一 owner**——RecordCompaction 只在 runtime handler 调
- ✅ **Channel 是 footer 渲染的唯一 owner**——runtime 只 stamp SessionContext，不直接拼字符串
- ✅ **Channel 不调 git、不算 token、不调 RecordCompaction**——保持 F-08 "Channel is dumb"

### 5.4 持久化

- ✅ **AgentSession 是 metadata 的唯一持久化载体**——compactionCount 跟 cumulativeUsage 同源（同一个 struct，同一把锁）
- ✅ **`/new` 是唯一清零入口**——compactionCount 与 cumulativeUsage 一同清零
- ✅ **daemon 重启不丢**——`FromAgentSessionEntry` 还原

### 5.5 1 turn : 1 anchor 不变式

- ✅ **OutCompaction 整条 path 删除**——之前 F-37 给 OutCompaction 发 thread reply，违反 1 turn : 1 anchor 不变式的精神（一个 turn 中间突然多一条 thread message）；删掉后更干净
- ✅ **EventCompaction 不再产生 thread reply / receipt entry / state reaction**

### 5.6 OutboundKind 集合

- ✅ **不增**——F-49 删除 OutCompaction，kind 集合净减少 1 个
- ✅ **未来要发 "压缩进行中" 提示时**——可以新增 OutCompaction 或别的 kind，但目前不需要（用户明确说"不用做进过程的显示"）

---

## 6. 不在本 PR 范围

- **`/cost` slash command**——展示 lifetime cost + since-last-compaction 拆分；后续 PR
- **per-model breakdown**——Anthropic API `modelUsage` map 展开成 multi-line footer；后续 PR
- **ChatSession-level 总计**——pool 内所有 AgentSession 之和；后续 PR
- **model → max_context 静态表**——可加 Line 2.5 显示"5k / 200k · 22%"，但需要查 model metadata；后续 PR 单独讨论
- **"压缩进行中"瞬时提示**——用户明确不需要；若未来需要，新增 OutboundKind 而不是复活 OutCompaction

---

## 7. 实施计划

按 8 个独立 commit 顺序落地，每步可单独 revert：

1. **`feat(agent): delete CompactionEvent.Subtype + AgentEvent.Compaction`**
   - `internal/agent/agent.go` 删 Subtype + Compaction 字段
   - bridges 同步删 `Compaction: &CompactionEvent{...}` 赋值
   - `internal/bridge/pi/translate.go` 屏蔽 compaction_start

2. **`feat(chatsession): AgentSession.CompactionCount + RecordCompaction`**
   - struct 加 `compactionCount int`
   - 加 `RecordCompaction()` / `CompactionCount()`
   - `ResetCumulative` / `Entry` / `FromAgentSessionEntry` 同步

3. **`feat(registry): AgentSessionEntry.CompactionCount + file version bump`**
   - 加字段
   - `AgentSessionFileVersion` +1

4. **`feat(gateway): SessionContext.CompactionCount + remove OutCompaction kind`**
   - `SessionContext` 加字段
   - `OutboundKind` 删除 `OutCompaction` 常量
   - `gateway.translate.go` 删除 `case agent.EventCompaction:`

5. **`feat(runtime): newEventHandler RecordCompaction + stamp CompactionCount`**
   - handler case EventCompaction → RecordCompaction
   - `sessionContextInto` stamp
   - stamp condition 扩展

6. **`feat(feishu): footer Line 1 🗜 N + remove OutCompaction adapter case + remove receipt entry`**
   - `usage_footer.go` Line 1 末尾追加
   - `adapter.go` Send 删 OutCompaction case
   - `receipt_event.go` 删 EventCompaction 分支
   - `receipt.go` 删 Append silent PATCH

7. **`docs(SPEC): §0.13 F-49 changelog`**
   - `docs/SPEC.md` 加 §0.13 增量变更摘要

8. **`docs(feat): F-45 §1.8 follow-up + F-32 bridge behavior + F-37 remove thread route + F-25 remove receipt entry`**
   - `docs/feat/F-45-session-footer.md` 加 §1.8
   - `docs/feat/F-32-pi-rpc-bridge.md` 更新 §2.3 wire translation 表
   - `docs/feat/F-37-tool-thread-routing.md` 删 §2.1 OutCompaction 行 + §3.4 silent PATCH 段
   - `docs/feat/F-25-rolling-log.md` 删 §3.1.1 OutCompaction thread reply 行 + §2.4 silent PATCH 段
   - `docs/channel/feishu.md` 加 §13.25 F-49 decision

---

## 8. 变更日志

| 日期 | 版本 | 变更 |
|---|---|---|
| 2026-08-06 | 草案 | 初稿；用户对话澄清 4 点（bridge 抽象、emoji = 🗜、token reset 语义、删 Subtype + OutCompaction）；设计经过 §1.3 抽象收敛到 runtime 协议无关 |
