# F-54: Pi Bridge 的 ContextWindow Lookup + 删除死字段

> **Status**: 📋 设计落地,代码未改
> **Milestone**: v1.3.x
> **Depends**: [`F-32-pi-rpc-bridge.md`](./F-32-pi-rpc-bridge.md) (get_state RPC), [`F-45-session-footer.md`](./F-45-session-footer.md) (X% 渲染), [`F-52-pi-stream-aggregation.md`](./F-52-pi-stream-aggregation.md) (decodeMessageUsage)
> **Related**: [`F-24-claudecode-bridge.md`](./F-24-claudecode-bridge.md) (claudecode 的 decodeUsage 行为作对照)

## 0. 摘要

**两件事**:

1. **删除死字段**: `agent.UsageEvent.ContextWindow int` —— 全 codebase 0 read / 0 write,claudecode `decodeUsage` 写入后只在同一函数内消费算 pct,从未穿出 struct 边界。footer / channel / runtime 全部读 `ContextWindowPct`,没人需要原始 `ContextWindow` 值。
2. **Pi 补全 X%**: Pi bridge 的 `decodeMessageUsage` 当前只填 `CostUSD`,**没填** `ContextWindowPct`(注释里自己承认 "omits the 'X%' segment for pi users until pi plumbing for context-window lookup lands")。从 `get_state.data.model.contextWindow` 拿,bridge-local 算 pct,直接填到 `UsageEvent.ContextWindowPct`。

**不动的契约**: `UsageInfo` / `UsageEvent` 字段表不变(只删 1 个死字段)。Footer 渲染不变。ClaudeCode bridge 行为不变(本来就报 X%)。

## 1. 背景与决策

### 1.1 现状

| Bridge | 报 X%? | 怎么报 |
|---|---|---|
| ClaudeCode | ✅ | `decodeUsage` 从 `modelUsage[<model>].contextWindow` 解出,同一函数算 pct 填 `out.ContextWindowPct` |
| Pi | ❌ | `decodeMessageUsage` 只填 `CostUSD`,pct 字段永远 0,footer 永远省略 X% 段 |

代码注释 `internal/bridge/pi/translate.go:809-811` 自己写明:

> "omits the 'X%' segment for pi users until pi plumbing for context-window lookup lands. Token / cost fields are unaffected. See docs/feat/F-45-session-footer.md §1.5."

——这是 F-52 重构时遗留的"等 pi plumbing"债务。本 PR 关闭它。

### 1.2 为什么删 `UsageEvent.ContextWindow` 字段

`tokensave_field_sites` 报告:整个 codebase 中

- **0 个 read site** (外部)
- **0 个 write site** (外部)

唯一一处写入它的代码是 `internal/bridge/claudecode/stream.go:642 decodeUsage`,但紧接的 6 行后它就被 `out.ContextWindowPct = float64(used) / float64(out.ContextWindow) * 100` 读完用了——**从没穿出 `decodeUsage` 函数边界**。它不是"传递中间值",而是"函数内用完即丢的临时存放"。

→ 完全改成本地变量。Struct 不需要这个字段。

### 1.3 为什么 Pi 能填 pct

Pi 的 `get_state` RPC 响应已经包含 `data.model.contextWindow`(详见 [`F-32-pi-rpc-bridge.md`](./F-32-pi-rpc-bridge.md) §2.4)。

F-32 实施时 `get_state` 响应只在 `session.New()` 调一次,目的是拿 `sessionId` 填 `ResumeID`。`data.model.contextWindow` 字段在 nightme 这边被静默丢弃。

**决策**:从 `get_state` 响应解 `data.model.contextWindow` → 存到 `translator.contextWindow`(bridge-local 状态)→ `decodeMessageUsage` 拿它算 pct。

### 1.4 为什么不需要 fallback 表

`contextWindow == 0`(pi get_state 失败 / 旧版 pi 没这个字段 / 模型未识别)→ `decodeMessageUsage` 看到 0 → `ContextWindowPct = 0` → footer 按既有约定 `== 0 时省略`(F-45 §1.6)。**与 ClaudeCode 当前未报 ContextWindow 时行为一致**,无新代码路径。

## 2. 设计

### 2.1 字段变更

```diff
 type UsageEvent struct {
     InputTokens              int
     OutputTokens             int
     CacheCreationInputTokens int
     CacheReadInputTokens     int
     CostUSD                  float64
-    ContextWindow            int
     ContextWindowPct         float64
 }
```

`UsageInfo` 不动(本来就没 `ContextWindow` 字段)。

### 2.2 Pi bridge 改动

**`internal/bridge/pi/protocol.go`** — 扩展现有 `getStateModel`,新增 `ContextWindow` 字段:

```go
type getStateModel struct {
    ID            string `json:"id"`
    Name          string `json:"name"`
    Provider      string `json:"provider"`
    ContextWindow int    `json:"contextWindow"`  // 新增 (F-54)
}
```

`maxTokens` / cost rate table / `id` 已有但 nightme 不消费(`id` / `Name` 已用于 EventAgentConnected.Model 显示)。

**`internal/bridge/pi/translate.go`** — 改 3 处:

```go
type translator struct {
    // ... 既有字段
    contextWindow int   // 新增: 由 emitConnected 填,bridge-local 状态
}

// emitConnected 缓存 contextWindow:
if result.Model.ContextWindow > 0 {
    t.contextWindow = result.Model.ContextWindow
}

// decodeMessageUsage 接收 ctxWindow 算 pct:
func decodeMessageUsage(u *messageUsage, ctxWindow int) *agent.UsageEvent {
    // ... 既有字段填充
    if ctxWindow > 0 {
        used := u.Input + u.Output + u.CacheRead + u.CacheWrite
        if used > 0 {
            out.ContextWindowPct = float64(used) / float64(ctxWindow) * 100
        }
    }
    return out
}
```

调用点变化:`recordAssistantMessageLocked` 把 `t.contextWindow` 作为参数传给 `decodeMessageUsage`。

### 2.3 ClaudeCode bridge 改动

**`internal/bridge/claudecode/stream.go decodeUsage`** — 把 `out.ContextWindow` 改成本地变量:

```go
- out := &agent.UsageEvent{...}
- // ... 解 rawModelUsage
- out.ContextWindow = v.ContextWindow
- // 算 pct
- out.ContextWindowPct = ...
+ out := &agent.UsageEvent{...}
+ contextWindow := 0
+ // ... 解 rawModelUsage
+ if v.ContextWindow > 0 {
+     contextWindow = v.ContextWindow
+ }
+ // 算 pct
+ if contextWindow > 0 {
+     used := ...
+     out.ContextWindowPct = float64(used) / float64(contextWindow) * 100
+ }
```

### 2.4 不动的东西

- ❌ `agent.UsageInfo`(本来就没 `ContextWindow`)
- ❌ `ContextWindowPct` 字段(已存在)
- ❌ `usage_footer.go`(它只读 pct,自动显示 X%)
- ❌ `gateway/` / `chatsession/` / `command/`
- ❌ `SessionContext` schema / 持久化

## 3. 影响

| 维度 | 估 |
|---|---|
| 字段变更 | -1 (`UsageEvent.ContextWindow`) |
| 新增 struct | `modelInfo`, `getStateData`(2 个小 struct,~6 行) |
| 代码净增 | +22 / -7 |
| 涉及文件 | 4 (agent.go, claudecode/stream.go, pi/protocol.go, pi/translate.go) |
| 测试改动 | claudecode `decodeUsage` 测试不 assert `out.ContextWindow`(改 assert `out.ContextWindowPct`);pi `translate_test.go` 加 ~3 用例(get_state 解析 + ctxWindow=0 + ctxWindow>0 算 pct) |
| 持久化 schema | 不变 |
| Footer UX | Pi 用户 footer 出现 `X%` 段(从无到有,claudecode 用户无变化) |

## 4. 风险

- **Pi 旧版本没有 `model.contextWindow`**: `get_state` 响应里 `data.model` 可能为 nil 或 `contextWindow` 为 0 → translator.contextWindow 留 0 → pct 不算 → footer 跳过 X%。**fallback 行为与现 ClaudeCode 未报场景一致。**
- **`/new` 后 contextWindow 失效?**: 不会(`/new` 是 reset conversation,模型没换)。当前没有 model-change 路径(pi F-32 MVP 不支持动态切模型),所以 `translator.contextWindow` 在 session 生命周期内有效。
- **删除字段是破坏性**: 仅删 `UsageEvent.ContextWindow`,**无外部 read site**(tokensave 验证),所以不会破坏调用方。