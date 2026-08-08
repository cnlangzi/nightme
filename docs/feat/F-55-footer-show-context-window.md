# F-55: Footer 显示 `(<window>)` 让用户自己判断窗口值

> **Status**: 📋 设计落地,代码未改
> **Milestone**: v1.3.x
> **Depends**: [`F-45-session-footer.md`](./F-45-session-footer.md) (footer 第二行 X% 渲染), [`F-54-pi-contextwindow-from-get-state.md`](./F-54-pi-contextwindow-from-get-state.md) (bridge-local 取 `contextWindow`)
> **Related**: [`F-24-claudecode-bridge.md`](./F-24-claudecode-bridge.md) (ClaudeCode `decodeUsage` 行为), [`F-32-pi-rpc-bridge.md`](./F-32-pi-rpc-bridge.md) (Pi `get_state`)

## 0. 摘要

**一件事**:footer 第二行 `X%` 段后面追加 `(window)`,把分母一并展示,让用户自己判断 `pct > 100%` 时窗口值是不是上游兼容端(`MiniMax`、代理、Bedrock 转发等)报错了。

**为什么不做查表 / override / clamp**:nightme 不维护模型目录表,F-54 已经把窗口值定位成"CLI/Agent 上游报什么就是什么";再加 hybrid 信任分级会把架构复杂度推给用户,而多数用户其实只需要看到分母就能自行判断。

**具体格式**:

```text
💰:「 202.5k / 603 · 101.6% (200k) · $0.520 」
```

```text
💰:「 202.5k / 603 · 20.3% (1.0M) · $0.520 」
```

`X% (window)` 是一个语义单元,中间一个空格。

## 1. 背景与决策

### 1.1 现状

`internal/agent/agent.go:400` 上 `UsageEvent.ContextWindowPct` 是 bridge-local 算出来的:

```text
pct = (input + output + cache_creation + cache_read) / contextWindow * 100
```

`contextWindow` 来自子进程 wire 字段:

- ClaudeCode:`result.modelUsage[<model>].contextWindow` (`internal/bridge/claudecode/stream.go:685`)
- Pi:`get_state.data.model.contextWindow` (`internal/bridge/pi/protocol.go:111`)

F-54 §1.2 明确把 `agent.UsageEvent.ContextWindow` 字段删了,理由是"全 codebase 0 read / 0 write,bridge-local 算完即丢"。但 footer 渲染时用户其实**想知道**这个分母——`pct = 101.6%` 时,如果不显示分母,用户无法判断是模型真的吃满了 200K,还是 MiniMax 这种兼容端把 1M 模型错报成 200K。

### 1.2 决定

恢复 `agent.UsageEvent.ContextWindow int` 字段,语义保持 F-54 §1.2 的"bridge-local 透传"——nightme 自己**不**基于它做任何 decision(不重算 pct、不查表、不 clamp、不告警)。它跨过 bridge struct 边界,目的是让 footer 渲染时能给用户看到分母。

**nightme 不做的三件事**(明确否决):

1. ❌ 维护模型 → 窗口查表(避免引入 `anthropic-models-2026-06-24` 之类的 catalog,以及"Nightly 拉 `/v1/models` 校准"之类的运维负担)
2. ❌ 配置 override(避免 `~/.config/nightme/config.yaml` 里出现 `agents.contextWindow: 1000000` 之类的 hack)
3. ❌ `pct > 100%` 时 clamp 或告警(让用户看到原始事实;clamp 会把上游 bug 隐藏)

**一句话立场**:CLI Agent 报什么就显示什么,错了让用户自行计算。

### 1.3 受影响的显示格式

**当前 footer 第二行**(`internal/channel/feishu/usage_footer.go:183`):

```text
💰:「 in / out · X% · $cost 」
```

**改后**:

```text
💰:「 in / out · X% (window) · $cost 」
```

`(window)` 与 `X%` 中间一个空格,与现有 `· $cost` 一致用 `·` 分隔;括号包裹 `window` 是为了让用户一眼看出"这是分母,不是百分比继续累积"。

## 2. 设计

### 2.1 字段变更

```diff
 type UsageEvent struct {
     InputTokens              int
     OutputTokens             int
     CacheCreationInputTokens int
     CacheReadInputTokens     int
     CostUSD                  float64
+    ContextWindow            int
     ContextWindowPct         float64
 }
```

- `UsageInfo` 也同步加 `ContextWindow int`(二者字段表保持一致,见 F-52 §2.4)。
- 字段语义:**bridge-local 透传,仅供 footer 渲染**。runtime 不重算,不查表,不基于它做决策。
- 不引入 `ContextWindowSource string`("wire" / "catalog" / "override" 之类的来源标记)——本次刻意不增加信任分级架构。
- 不引入 `ContextWindowReliable bool`——同理由。

### 2.2 Bridge 改动

**`internal/bridge/claudecode/stream.go decodeUsage`**(`stream.go:643`):

当前把 `contextWindow` 当本地变量用完即丢:

```diff
-    if contextWindow > 0 {
-        used := out.InputTokens + out.OutputTokens +
-            out.CacheCreationInputTokens + out.CacheReadInputTokens
-        if used > 0 {
-            out.ContextWindowPct = float64(used) / float64(contextWindow) * 100
-        }
+    if contextWindow > 0 {
+        used := out.InputTokens + out.OutputTokens +
+            out.CacheCreationInputTokens + out.CacheReadInputTokens
+        if used > 0 {
+            out.ContextWindowPct = float64(used) / float64(contextWindow) * 100
+        }
+        // F-55: 透传 window 给 footer,仅渲染用途,nightme 不基于它重算/查表/clamp
+        out.ContextWindow = contextWindow
     }
```

**`internal/bridge/pi/translate.go decodeMessageUsage`**(`translate.go:837`):

```diff
-    if ctxWindow > 0 {
-        used := u.Input + u.Output + u.CacheRead + u.CacheWrite
-        if used > 0 {
-            out.ContextWindowPct = float64(used) / float64(ctxWindow) * 100
-        }
+    if ctxWindow > 0 {
+        used := u.Input + u.Output + u.CacheRead + u.CacheWrite
+        if used > 0 {
+            out.ContextWindowPct = float64(used) / float64(ctxWindow) * 100
+        }
+        // F-55: 透传 window 给 footer,仅渲染用途,nightme 不基于它重算/查表/clamp
+        out.ContextWindow = ctxWindow
     }
```

**emitConnected / modelUsage map iteration**:本次不动。`for _, v := range m` 的 map 随机遍历顺序在单模型场景下没有可见影响(只有一个 entry);多模型场景里的小毛病留待后续单独 PR。本次改动只透传 window,不动取值逻辑。

### 2.3 Footer 渲染

**`internal/channel/feishu/usage_footer.go formatSessionFooterLines`**(`usage_footer.go:183`):

当前:

```go
if u.ContextWindowPct > 0 {
    usageParts = append(usageParts, fmt.Sprintf("%.1f%%", u.ContextWindowPct))
}
```

改后:

```go
if u.ContextWindowPct > 0 {
    usageParts = append(usageParts, fmt.Sprintf("%.1f%% (%s)", u.ContextWindowPct, abbrevWindow(u.ContextWindow)))
}
```

**新 helper `abbrevWindow`**(同 `usage_footer.go:242 abbrevTokens` 口径):

```go
// abbrevWindow 格式化模型上下文窗口:
//   < 1000          -> 数字 (如 "999")
//   1000 <= n < 1M  -> K 缩写 (如 "1.0k", "200k", "999k")
//   n >= 1M         -> M 缩写 (如 "1.0M")
//
// 与 abbrevTokens 完全同款 (后者用于 in/out token 计数),
// 抽出来是为了 footer 渲染可读,不混淆"窗口大小"与"token 数"。
func abbrevWindow(n int) string {
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

**渲染规则**:

| `ContextWindowPct` | `ContextWindow` | 渲染 |
|---|---|---|
| `== 0` | `== 0` | 不显示 X% 段(沿用 F-45 §1.6 zero-omit) |
| `== 0` | `> 0` | 显示 `(200k)` 不带 X%(理论上不该发生:pct==0 时 window 也应==0;但保留兜底) |
| `> 0` | `== 0` | 只显示 `X%`(沿用 F-54,极少见,模型未识别窗口) |
| `> 0` | `> 0` | 显示 `X% (window)` |
| `> 100` | `> 0` | 显示 `X% (window)`,**不 clamp 不告警** |

### 2.4 不动的东西

- ❌ `UsageEvent` / `UsageInfo` 上**不**新增 `ContextWindowSource` / `ContextWindowReliable` 之类的 trust-tier 字段(见 §1.2)
- ❌ 不维护模型 → 窗口查表
- ❌ 不加 `/v1/models` 拉取逻辑
- ❌ 不加 config override
- ❌ 不改 `emitConnected` 的 window 缓存语义
- ❌ 不改 pct 公式
- ❌ 不改 `pct > 100%` 的现有零 clamp / 零告警行为
- ❌ 不动 `claudecode/stream.go decodeUsage` 的 `modelUsage` map iteration(留待单独 PR)

## 3. 影响

| 维度 | 估 |
|---|---|
| 字段变更 | +1 (`UsageEvent.ContextWindow`,`UsageInfo.ContextWindow` 同步) |
| 代码净增 | +15 / -2 (bridge 透传 + footer helper + 测试) |
| 涉及文件 | 5 (agent.go, claudecode/stream.go, pi/translate.go, feishu/usage_footer.go, 对应测试) |
| 测试改动 | claudecode / pi 各自加 ~1 case 验证 `out.ContextWindow` 透传;footer 加 ~3 case 覆盖三种渲染规则 |
| 持久化 schema | 不变(`ContextWindow` 不进 registry,只在 `SessionContext.Usage` 内存态) |
| Footer UX | 第二行 X% 段从 `20.3%` 变成 `20.3% (1.0M)`,用户可读分母;`pct > 100%` 时上下文一目了然(`101.6% (200k)` 让用户立刻看出窗口元数据可疑) |

## 4. 风险

- **括号里的 `200k` / `1.0M` 可能与上游实际能力不一致**:这是本次刻意保留的——让用户看到原始事实,自己判断。如果后续用户反馈"太长了看不清"再考虑 trust-tier(本次不做)。
- **测试 fixture 里 mock 桥发的 `modelUsage.contextWindow: 200000` 不变**:frozen snapshot,F-54 当时就这么写了;改后测试照旧。
- **`SessionContext.Usage` 多了一个 `ContextWindow` 字段**:runtime 通过 `OutboundMessage.Usage` 接收这条数据,文档已注明"runtime 不基于它重算"。runtime 唯一消费方是 footer 渲染。
- **delete 字段的反悔**:F-54 §1.2 删 `ContextWindow` 的理由是"全 codebase 0 read / 0 write"。本次恢复后字段有了**唯一**消费方(footer)。说明:`pct > 100%` 排查场景下,这就是用户需要的诊断信号;如果不显示分母,排查成本反而更高。

## 2.5 后续:把 `in` 拆成 `new` + `cache`(F-55.1, 2026-08)

**问题**: 用户反馈一张 footer 显示 `13.7M / 21.3k · 1374.5%`,13.7M input 远超模型窗口(1M),看起来像累计 bug。但官方文档明确三个 input-side 字段**互斥**(没有重复计数);实际 13.7M 全在 `cache_read_input_tokens` 里——上游兼容端把 cache hit 报成了 session 累计。

**做法**: Footer 第二行把 `in` 拆成两段:

```text
new   = input_tokens + cache_creation_input_tokens   // 本轮新增,不命中缓存
cache = cache_read_input_tokens                       // 命中缓存
out   = output_tokens
```

**渲染**:

```text
💰:「 new / cache / out · X% (window) · $cost 」
```

每段独立按 `> 0` omit(F-45 §1.6),纯数字,**无 label**。`cache == 0` 时退回原 `new / out` 布局。

**实例**(用户截图):

```text
💰:「 1.2k / 13.0M / 21.3k · 1374.5% (200.0k) · $24.422 」
```

中间段 `13.0M` 立刻告诉用户"这一轮 13M 是缓存命中,新内容只有 1.2k"。如果是 MiniMax 这种把 cache_read 报成累计的上游,这个数字会**非常醒目**——和 F-55 立场一致:CLI Agent 报什么显示什么,用户自行判断。

**不变**:
- pct 仍按 Doc 1 公式:`(new + cache + out) / contextWindow`(没改)
- `(window)` 仍按 F-55 显示
- `pct > 100%` 不 clamp 不告警
- nightme 不查表、不 override、不重算

**改动**:
- `internal/channel/feishu/usage_footer.go` `formatSessionFooterLines`: 行内 `in` 拆 `new` + `cache`
- `internal/channel/feishu/usage_footer_test.go`: `TestFormatSessionFooterLines_OmitsZeroSegments` 等几个 case 更新 + 加 2 个新 case(cache-only / cache+out)
- `docs/feat/F-45-session-footer.md` Line 2 表格: 把 `in / out` 一行拆 `new` + `cache` + `out`
- `docs/SPEC.md`: Line 2 段规则同步
