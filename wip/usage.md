# Usage 模型重构 — 把 cumulative 改成 per-event snapshot

> 状态:📋 调查完成,待评审开 PR
> 目标:Footer 渲染"当前这一次 turn 的消耗",无任何跨 turn 累加

---

## 0. 新契约(设计基线)

| 旧(删除) | 新 |
|---|---|
| `UsageInfo.CumulativeUsage` 字段 | `SessionContext.Usage *UsageInfo`(单事件) |
| `agent.UsageInfo` 是 "cumulative form of UsageEvent" | `agent.UsageInfo` 是 "per-turn snapshot"(注释改写) |
| `AS.cumulativeUsage` + `cumulativeDirty` + `cumulativeUsageMu` | **删除**(AS 不再管任何 usage) |
| `AS.AccumulateUsage(u)` | **删除** |
| `AS.ResetCumulative()` | **删除** |
| `AS.CumulativeUsage()` getter | **删除** |
| `AS.lastContextWindowPct` + `LastContextWindowPct()` getter | **删除**(pct 移到 `UsageEvent.ContextWindowPct`,bridge 填) |
| `AgentSessionEntry.CumulativeUsage` 持久化 | **删除字段** |
| Runtime 的 priority switch (Done.Usage > out.Usage) | **删除**;bridge 想填哪个就填,channel 直接渲染 |
| `/new` 调 `ResetCumulative` 清零 | **删除调用** |

**核心原则**:
- bridge 填什么,channel 渲染什么
- AS 不参与任何 usage / pct 累加或计算
- 无 dedup、无 priority、无状态机

---

## 1. 代码核心(必改,主重构)

### 1.1 `internal/chatsession/agentsession.go`

- [ ] 删除字段 `cumulativeUsage agent.UsageInfo`(~行 142)
- [ ] 删除字段 `cumulativeUsageMu sync.RWMutex`
- [ ] 删除字段 `cumulativeDirty bool`
- [ ] 删除字段 `lastContextWindow int`(若存在)
- [ ] 删除字段 `lastContextWindowPct float64`(~行 168)
- [ ] 删除方法 `AccumulateUsage(u *agent.UsageEvent)`(~行 646-670)
- [ ] 删除方法 `LastContextWindowPct() float64`(~行 687-691)
- [ ] 删除方法 `ResetCumulative()`(~行 715-722)
- [ ] `RecordCompaction()`:删除 `as.lastContextWindowPct = 0` 那一行(~行 751)
- [ ] `PersistIfDirty`:删除 cumulative 相关 dirty 逻辑
- [ ] `newAgentSessionRuntime` / `FromAgentSessionEntry`:删除 cumulativeUsage 相关初始化/反序列化代码
- [ ] AgentSession struct doc:删除 "cumulative usage" 章节,改为 "lifecycle management only"

### 1.2 `internal/agent/agent.go`

- [ ] `UsageInfo` struct doc 全面改写:从 "cumulative form of UsageEvent" → "per-turn snapshot; NOT cumulative; bridge 填一次,channel 渲染一次"
- [ ] `UsageInfo.CostUSD` 注释:删 "Cumulative state sums across turns",改为 "per-turn snapshot"
- [ ] `UsageEvent` struct doc:删除 "(in) AS computes lastContextWindowPct via ..." 的暗示
- [ ] `UsageEvent` 新增字段 `ContextWindowPct float64`(bridge 计算后填,AS 不参与)
  - doc 说明:bridge 按 Anthropic 公式 (input + cache_creation + cache_read + output) / ContextWindow * 100 算
  - 0 = 未填,channel footer 跳过 X% 段
- [ ] 删除 `agent.UsageInfo` 与 `agent.UsageEvent` 之间的"cumulative / per-turn"语义混用描述

### 1.3 `internal/gateway/messages.go`

- [ ] `SessionContext` struct:
  - [ ] 删除字段 `CumulativeUsage UsageInfo`
  - [ ] 新增字段 `Usage *agent.UsageInfo`(指针;OutReply 可能 nil)
  - [ ] 更新字段 doc
- [ ] `SessionContext` struct 顶部注释:删除 "Total = In + CacheCreate + CacheRead + Out is derived at render time; no Total field on the wire" 这种 total 概念
- [ ] `UsageInfo` type alias 注释:更新(语义已转交给 `agent.UsageInfo`)

### 1.4 `cmd/nightme/run.go`

- [ ] `newEventHandler`:
  - [ ] 删除 priority switch(~行 970-985):
    ```go
    switch {
    case ev.Kind == agent.EventDone && ev.Done != nil && ev.Done.Usage != nil:
        s.AccumulateUsage(ev.Done.Usage)
    case out.Usage != nil:
        s.AccumulateUsage(out.Usage)
    }
    ```
  - [ ] 删除 EventDone drop 分支里的 `s.AccumulateUsage(ev.Done.Usage)`
  - [ ] 删除 "Per-turn usage accumulation moved out..." 那段 30 行注释(已过时)
- [ ] `sessionContextInto`(~行 1094):
  - [ ] 删除 `snap := s.CumulativeUsage()`
  - [ ] 删除 `pct := s.LastContextWindowPct()`
  - [ ] 删除9 项 predicate 里关于 `snap.* != 0` / `pct > 0` 的5 个判断
  - [ ] 简化 predicate 为:`s.Agent != "" || s.Model() != "" || hasGit || s.CompactionCount() > 0 || out.Usage != nil`
  - [ ] 改 SessionContext 字段填充:`Usage: out.Usage`(直接引用)
- [ ] EventDone handler 简化:
  - [ ] 如果 `PersistIfDirty` 不再有 cumulativeDirty 触发条件,改写其 dirty 检测逻辑
  - [ ] 更新注释(去掉 "usage persistence on EventDone" 描述)

### 1.5 `internal/command/newcmd/cmd.go`

- [ ] `Factory.Handle`:
  - [ ] 删除 `r.Session.ResetCumulative()` 调用(~行 92)
  - [ ] 删除紧随其后的 `PersistAgentSession(r.Session)` 调用(cumulative 不再持久化,无需 flush)
  - [ ] 删除整段大注释 `// F-45 §1.8: /new is the ONLY event that clears cumulative ...`(~行 85-94)
- [ ] 文件顶部 package doc:删除 "/new resets cumulative token stats" 的措辞

### 1.6 `internal/registry/agent_session_entry.go`

- [ ] `AgentSessionEntry` struct:
  - [ ] 删除字段 `CumulativeUsage *agent.UsageInfo`
- [ ] 保留所有其他字段

### 1.7 `internal/channel/feishu/usage_footer.go`

- [ ] `formatSessionFooterLines`:
  - [ ] 把 `u := ctx.CumulativeUsage` 改为 `u := *ctx.Usage`(含 nil check:`if ctx.Usage == nil { 跳过 Line 2 }`)
  - [ ] 删除对 `ctx.ContextWindowPct` 的所有引用 — pct 已经在 `ctx.Usage.ContextWindowPct`
  - [ ] 整个 Line 2 渲染改为 "反映 ctx.Usage 当前事件的内容"
- [ ] package 顶部注释:
  - [ ] 删除 "cumulative across the entire AgentSession" 措辞
  - [ ] 重写 §Line 2 段说明

---

## 2. 桥代码:基本不动

- [ ] `internal/bridge/claudecode/stream.go`:`decodeUsage` 不动 — 它本来就是"从 result 事件解码一次"
- [ ] `internal/bridge/pi/translate.go`:`decodeMessageUsage` 不动
- [ ] 可选小修:bridge 注释里去掉 "for cumulative" 字样,改为 "per-turn snapshot"

---

## 3. 测试改动

### 3.1 `cmd/nightme/run_test.go`

- [ ] `TestEventHandler_OutResult_AccumulatesAcrossTurns`(行 ~324-362):
  - [ ] **重命名 + 改写**:改名为 `TestEventHandler_OutResult_UsageIsPerTurnNotCumulative`
  - [ ] 断言:turn 2 的 `SessionContext.Usage` 等于 turn 2 自己的 Usage,**不**等于 turn1+turn2 之和
- [ ] `TestEventHandler_OutResult_NilUsageLeavesEmptySessionContext`(行 ~369-402):
  - [ ] 更新断言从 `SessionContext.CumulativeUsage` → `SessionContext.Usage`
- [ ] `TestEventHandler_OutResult_FooterFirstTurnExact`(行 ~241):
  - [ ] 更新断言(用 `Usage` 而不是 `CumulativeUsage`)

### 3.2 `internal/chatsession/agentsession_meta_test.go`

- [ ] **删除** `TestAccumulateUsage_RaceFree`(行 ~48-78)
- [ ] **删除** `TestAccumulateUsage_NilSafe`(行 ~92-95)
- [ ] **删除** `TestResetCumulative_ClearsAndDirties`(行 ~100-115)
- [ ] **改写或删除** `TestEntry_RoundtripPreserves`(行 ~142-190):
  - [ ] 如果对象不再有 CumulativeUsage 字段,断言更新
- [ ] **删除** `TestEntry_LegacyFileWithMissingCumulativeUsage`(行 ~195-225):
  - [ ] legacy 文件不再有 cumulativeUsage 字段需要兼容

### 3.3 `internal/channel/feishu/usage_footer_test.go`

- [ ] 更新所有 `ctx.CumulativeUsage = ...` 断言 → `ctx.Usage = ...`
- [ ] `TestFormatSessionFooterLines_ContextWindowPct`(行 ~290):
  - [ ] 更新断言:`ctx.Usage.ContextWindowPct` 替代 `ctx.ContextWindowPct`
- [ ] 注释更新:删 "cumulative" 措辞,改为 "per-turn snapshot"

### 3.4 `internal/gateway/translate_test.go`

- [ ] ✅ 不动(Translate 测试本就测 "Result.Usage → out.Usage" 透传,与 cumulative 无关)

### 3.5 `internal/bridge/claudecode/claudecode_test.go`

- [ ] ✅ 不动(测试本就测 "decodeUsage 从 result 事件解码",与 cumulative 无关)

### 3.6 `internal/bridge/pi/translate_test.go`

- [ ] ✅ 不动

### 3.7 `internal/channel/feishu/adapter_test.go`

- [ ] 检查所有涉及 `SessionContext.CumulativeUsage` 的断言 → 改 `SessionContext.Usage`

---

## 4. 文档改动

### 4.1 `docs/feat/F-45-session-footer.md` ⭐ 大改

- [ ] 文件标题:"AgentSession **累计 Token 统计** + Main-Chat 卡片 Footer" → "Main-Chat 卡片 Footer(per-turn snapshot)"
- [ ] §0.5 "持久化范围澄清":删除 cumulative 讨论;改为 "footer 不持久化任何 usage,daemon 重启后下一轮自动恢复"
- [ ] §1.5 "SessionContext 字段":重写
- [ ] §1.6 "Footer 渲染规则"**:重写**:不再 "cumulative across AgentSession",改为 "渲染当前事件 Usage"
- [ ] §1.8 "/new 行为":删除 "清零 cumulative" 描述
- [ ] §2.5 "运行时实现":删除累积/dirty 触发 persist 描述
- [ ] §2.6 "`handleNew`":改写,/new 不再清零任何 AS 状态
- [ ] §2.x 所有 compatibility / roundtrip 测试描述:删 cumulative 字样
- [ ] §0.4 节 "formatSessionFooter":重写(整段)

### 4.2 `docs/feat/F-49-compaction-counter.md` ⭐ 大改

- [ ] 整个 doc 严重依赖 cumulative 概念 — compaction 后 footer 显示什么的策略需要重新讲
- [ ] 旧: "compaction 后 cumulativeUsage 归零,CacheCount 保留,显示 7.8k after 1.37M total"
- [ ] 新策略(待定,见 §6 决策点 1):
  - [ ] 策略 A 描述:bridge 在 compaction 事件上附带 post-compact usage(代表压缩后 context 大小)
  - [ ] 策略 B 描述:compaction 后 footer Line 2 暂时消失,等下一轮 result
  - [ ] 策略 C 描述:compaction 后只增加 count,token 不变

### 4.3 `docs/feat/F-52-pi-stream-aggregation.md` ⚠️ 小改

- [ ] 删除 "CumulativeUsage" 字样
- [ ] 删除 "DecodeMessageUsage 原来用聚合字段 totalTokens ..." 段(新设计下不是聚合)
- [ ] §6 "已知遗留"段:重写,不再说 "claudecode 也是取最后一次调用的快照"
- [ ] 整章 footer 描述:删 cumulative 措辞

### 4.4 `docs/SPEC.md` ⚠️ 中改

- [ ] §0.12 "F-45 footer + cumulative persistence":整个小节重写
- [ ] ~行 518:删除 `加 cumulativeUsage UsageInfo + mutex + cumulativeDirty bool:EventUsage 时 ...` 整段
- [ ] ~行 520:删除 `6 个新方法:SetModel / Model / AccumulateUsage / ResetCumulative / CumulativeUsage / LastContextWindowPct` 整段,改为新 API 列表
- [ ] ~行 523:`UsageInfo 从 internal/gateway/messages.go 搬到 internal/agent/agent.go` 保留,但加注 "语义从 cumulative 改为 per-turn"
- [ ] ~行 525-541:footer 渲染规则描述更新

### 4.5 `docs/FEATURES.md` ⚠️ 中改

- [ ] F-45 行的描述:"Main-Chat 卡片 Footer + AgentSession **累计 Token 持久化**" → "Main-Chat 卡片 Footer + 单次 Turn Usage 渲染"
- [ ] 整行的 footer 例子更新

### 4.6 `docs/channel/feishu.md` ⚠️ 小改

- [ ] ~行 1440 footer 例子注释:明确 "这是当前 prompt 的消耗,不是 session 累计"

### 4.7 `docs/channel/pi.md` ⚠️ 小改

- [ ] "已知遗留"段:删除 "AccumulateUsage 是跨轮累加" 描述,改为 "现在每个 turn 独立"
- [ ] ~行 144-148 "正确做法是覆盖"段:删除(没有"覆盖"概念了)

### 4.8 不动的文档

- [ ] `docs/flow/three-layer-sync.md` ✅ 不动(只是数据流向)
- [ ] `docs/feat/F-08-channel-abstraction.md` ✅ 不动

---

## 5. PR 拆分建议

### PR 1:协议字段扩展(纯增量,~30 行)
- [ ] `agent.UsageEvent` 加 `ContextWindowPct float64`
- [ ] `UsageInfo` / `UsageEvent` 文档全面重写(去掉 cumulative 措辞)
- [ ] `docs/SPEC.md` / `docs/FEATURES.md` 协议定义段同步更新
- [ ] 目的:协议层先扩展,后续 PR 不冲突

### PR 2:删除 AS 累积逻辑(主重构,~220 行净减)
- [ ] `agentsession.go` 删除 3 字段 + 3 方法
- [ ] `SessionContext.CumulativeUsage` → `Usage`
- [ ] `cmd/nightme/run.go` 删除 priority switch + 简化 sessionContextInto
- [ ] `internal/command/newcmd/cmd.go` 删除 ResetCumulative 调用
- [ ] `internal/registry/agent_session_entry.go` 删除 CumulativeUsage 字段
- [ ] `usage_footer.go` / `usage_footer_test.go` 适配
- [ ] `agentsession_meta_test.go` 累积相关测试删除
- [ ] `run_test.go` 的 `TestEventHandler_OutResult_AccumulatesAcrossTurns` 改写
- [ ] `docs/feat/F-45-session-footer.md` / `F-52` 小改
- [ ] 目的:主逻辑改完,大部分 AS 累加代码消失

### PR 3:F-49 compaction 策略(独立决策,~50 行)
- [ ] 取决于 §6 决策点 1 的选择
- [ ] 改 bridge 在 compaction 事件上的行为
- [ ] 改 footer 对 compaction 后的展示逻辑
- [ ] 改 `docs/feat/F-49-compaction-counter.md`(大改)
- [ ] 补 E2E 测试覆盖 compaction 后 footer

---

## 6. 待决策点(开 PR 前必须拍板)

- [ ] **决策 1:compaction 后 footer Line 2 行为**
  - A. bridge 在 EventCompaction 上附 post-compact usage,footer 显示压缩后的 context 大小
  - B. compaction 后 footer Line 2 暂时消失,等下一轮 result
  - C. compaction 后只增加 count(🗜 N+1),token 段不变
  - 影响:F-49 doc 大改 + 可能的 bridge 改动
- [ ] **决策 2:`SessionContext.CumulativeUsage` 字段重命名**
  - 推荐:`Usage`
  - 可选:`LastUsage` / `TurnUsage` / `CurrentUsage`
  - 影响:跨 4 个文件的字段名
- [ ] **决策 3:`agent.UsageInfo` 是否重命名**
  - 推荐:保留,只改注释
  - 可选:重命名为 `TurnUsage` / `SnapshotUsage`
  - 影响:跨 package 改动大
- [ ] **决策 4:`/new` 后是否需要"清空" footer / session 状态**
  - bridge 自己 New() 已经 reset context(发空 session)
  - runtime 不需要做任何事
  - 下一轮 result 时 footer 自然显示"新 session 的 usage"
  - 文档需要写清楚这一点

---

## 7. 影响面统计

| 类型 | 文件数 | 改动行(估) |
|---|---|---|
| 代码核心 | 4 | -150 / +30 |
| 命令 | 1 | -15 |
| 持久化 | 1 | -3 |
| Footer | 1 | -5 / +10 |
| 测试删除 | ~3 | -60 |
| 测试更新 | ~6 | -30 / +20 |
| 文档大改 | 3 | -200 / +150 |
| 文档小修 | 4 | -50 / +30 |
| **总计** | **~18** | **~-460 / +240** |

**净减 ~220 行**(主要是删除 AS 累积逻辑和它的测试)。

---

## 8. 风险点

- [ ] 删 `AgentSessionEntry.CumulativeUsage` 字段会改变 `agent_sessions.json` 持久化 schema
  - 旧文件有 `cumulativeUsage` 字段 → Go JSON unmarshal 容忍 → 运行时忽略 ✓ 向后兼容
  - 新文件不再写 `cumulativeUsage` 字段 ✓
- [ ] Footer 在 OutReply(无 usage)时 Line 2 不显示
  - UX 变化:用户在 streaming 期间看不到 token,等 OutResult 才出现
  - 影响可接受:footer 本来就是 "summary at prompt end"
- [ ] `$cost` 现在是当前 prompt 的 cost,不是 session total
  - 用户如果想看 session total → 需要单独 `/cost` 命令(独立特性,不在本次 PR)
- [ ] F-49 compaction 行为需要重新设计
  - 决策 1 待定,直接影响 PR 3 的范围

---

## 9. 不在本次范围(后续工作)

- [ ] `/cost` 命令(session lifetime total cost)— 等用户拍板后另起 PR
- [ ] F-49 compaction 后 footer 策略细化(决策 1)
- [ ] Pi bridge 翻译调整(若需要更精细的 usage 字段)— 当前不需要