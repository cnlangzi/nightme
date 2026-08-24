# /review 设计

> **Status**: 设计定稿(v11,含 ocr 委托模式三层分流 + 按 rule group 拆多 job 并发);**§2.5 多 job 并发机制已实现**(`internal/agent/aggregate_sink.go` + `delegate_review.go::delegateReviewMultiJob`,详见 §2.5.7 实现索引)
> **Scope**: `/review` slash 命令的架构、分流、数据流、生命周期与边界
> **读者**: 参与 command / agent / bridge / chatsession 任一层,或想理解 review 设计意图的工程师
> **Related docs**:
>
> - [feat/F-review.md](./feat/F-review.md) — v9 原始设计稿(native/delegate/不支持 pattern、flag 取舍、多 reviewer 并行风险评估)
> - [feat/F-review-ocr-fusion.md](./feat/F-review-ocr-fusion.md) — ocr 委托模式融合的可行性论证、落地计划、风险与待验证项
> - [flow/three-layer-sync.md](./flow/three-layer-sync.md) — ChatSession → AgentSession → Conversation 三层(`/review` 跑在这之上)

---

## 1. 设计目标

让用户在任意 IM chat 里发 `/review`,对当前 workspace 的代码变更做一次 code review,findings 回到**同一 chat**,主 agent 能据此 fix。review 在**隔离子进程**里跑,不污染主 chat 上下文;支持**多 reviewer 并行**(`--agent`),findings 汇聚到同一 chat。

### 1.1 核心不变量

| # | 不变量 | 违反后果 |
| --- | --- | --- |
| 1 | `/review` 全异步:Handle 立即返回,goroutine 跑 review | dispatch worker / readpump 阻塞,chat 卡死 |
| 2 | review 走 RunOnce 隔离子进程,独立 context 窗口 | 主 chat 上下文被 review 推理污染(可烧数万 token) |
| 3 | review 子进程 ctx 派生自 chat session ctx,`/close` 自动取消 | orphan review 子进程残留 |
| 4 | findings **双路分发**:注入 AS(当 user turn)+ 发 channel(直接可见) | 主 agent 看不到 findings,或用户要等下游回复 |
| 5 | `--agent` **不切 AS**(不同 reviewer,同一 chat) | 切 AS 副作用太大,破坏"多 reviewer 汇聚"工作流 |
| 6 | fix **对话式**:不自动 apply,用户说"fix blockers"主 agent 用原生 Edit | 违反 v1 "纯 review"边界 |
| 7 | **三层分流**:native / delegate-ocr / delegate-prompt,delegate 档 ocr 缺失自动降级 | 无 ocr 环境 review 退化为不可用,或 agent 漏文件 |
| 8 | ocr 始终是被调用的**外部工具**(类 git),不进 agent 注册表 | 把狭义 agent 当 bridge,扭曲 bridge 语义 |
| 9 | 大 changeset 按 ocr rule group 拆多 job,**每 job 独立 RunOnce / 独立 context**,不累积 | 单 context 塞全量 diff 爆窗口,或同进程多轮累积爆 |
| 10 | 多 job **自动并发**(sem 上限),merge 后一次返回 | 顺序跑 N job 慢;无上限并发爆 token / 撞 API rate |
| 11 | 多 job 时,**上层只看到一个 review lifecycle**(单 Ready / 单 Result) | chat channel 看到 N 个 ready / N 个 result,StatusBar 翻转混乱 |
| 12 | per-job 内的 ToolStart 必须**等配对 ToolEnd 才转发**,Start/End 在外层 wire 上**连续** | 半截 tool 调用外发,chat 渲染混乱 |
| 13 | 跨 job 的 EventAgentTaskCreate/Update **按 task ID 去重 merge**,同一 ID 多个 job 的最新版本只发一份 | chat checklist 看到 N 份重复 task 条目,数量 = N × tasks |

### 1.2 边界

- **是**:对"当前分支 vs 默认分支"(PR 模式)的代码变更做 review,产出 findings 文本回 chat,主 agent 可对话式 fix。
- **不是**:不开 `--fix` / `--post`(留 v2 的 `/review-fix` / `/review-comment`);不限定文件 / base(v1 除 `--agent` 外零限定符);不当 CI gating;不自动改代码。

---

## 2. 整体架构:三种 Review 方法

review 有三种 runner 实现。Agent.Review 接口(Starter.Review)由各 bridge 实现;bridge 决定调哪个 agent 包 runner。基础是 F-review.md §13 的三 pattern + v11 加的 simplify 并行 group。

```text
/review [--agent <name>]
   │  解析 --agent,选定 runner;runner ≠ 当前 AS 时,findings 仍回当前 AS
   ▼
Starter.Review(bridge 各自的实现)
   │
   ├─ Native bridge(claudecode / codex)
   │     → 桥自己调内置命令(`claude -p code-review` / `codex review --base`)
   │        〔各家最优形态,不画蛇添足;不走 agent 包 runner〕
   │
   ├─ Delegate bridge(dsh / pi / acp / opencode / cursor)
   │     → 桥检测 OcrAvailable()(SRP:路由选择放桥层,不放 agent 包内)
   │        ├─ YES → agent.ReviewWithOcr
   │        │           ├─ precomputeReview(ocr delegate preview + rule groups)
   │        │           ├─ groups = ocrGroups + simplifyGroup(reviewable)
   │        │           └─ delegateReviewMultiJob(sem cap 4,eventAggregator,mergeRunResults)
   │        └─ NO  → agent.ReviewWithPrompt
   │                    ├─ precomputeReview(Tier 3 fallback:collectReviewableFiles)
   │                    ├─ groups = builtinGroup(reviewable) + simplifyGroup(reviewable)
   │                    └─ delegateReviewMultiJob(同上)
   │
   └─ pty / bash → ErrReviewNotSupported → 友好提示("不是 coding agent")
```

simplify 作为并行 group 出现在 Tier 2 / Tier 3 两条路径上(`ReviewWithOcr` / `ReviewWithPrompt`),跟 ocr-sourced groups 或 builtinGroup 一起跑多 job fan-out。详见 §2.6。

### 两个 precompute 函数,产物 shape 一致

`precomputeReviewWithOcr` 和 `precomputeReviewWithBuiltin` 两条路径共用一个 `reviewContext` shape,fan-out machinery 不知道也不关心是哪条路径产出的:

```text
ReviewWithOcr                          ReviewWithPrompt
    ↓                                       ↓
precomputeReviewWithOcr                precomputeReviewWithBuiltin
    ├─ git: detectDefaultBranch           ├─ git: detectDefaultBranch
    ├─ git: merge-base                    ├─ git: merge-base
    ├─ ocr delegate preview               ├─ collectReviewableFiles (Go 端 git)
    │   → reviewable (ocr FileFilter)     │   → reviewable (Go isReviewablePath)
    │   → excluded (with reasons)         ├─ synthesize 1 个 builtin group
    ├─ ocr delegate rule                  │   → ocrGroups = [{patternBuiltin, BuiltinPrompt}]
    │   → ocrGroups (N groups, per-pattern)
    │   → ocrRules (markdown)
    └─ git: 3 diffs                        └─ git: 3 diffs
    ↓                                       ↓
    reviewContext (same shape)              reviewContext (same shape)
    ↓                                       ↓
    groups = pre.ocrGroups + simplifyGroup(pre.reviewable)
    ↓
    delegateReviewMultiJob → fan-out
```

**字段对照**:

| 字段 | ocr 路径 | Go 路径 |
|---|---|---|
| `reviewable` | ocr FileFilter(精度高) | `collectReviewableFiles` + `isReviewablePath`(启发式)|
| `excluded` | ocr 提供的排除原因列表 | nil(Go 端不跟踪 excluded)|
| `ocrGroups` | N 个(每 pattern 一组,Rule 是 ocr rule doc)| 1 个 builtin group(Pattern=patternBuiltin, Rule=BuiltinPrompt)|
| `ocrRules` | N 个 group 的 markdown 拼好 | ""(只有一个 group)|
| merge-base / 3 diffs | 都有(同)| 都有(同)|

`ReviewWithOcr` 调 `precomputeReviewWithOcr`,`ReviewWithPrompt` 调 `precomputeReviewWithBuiltin`。两个 Runner 函数返回值都是 `RunResult`,行为形状一致。Bridge dispatcher 仍按 `OcrAvailable()` 选择调用哪个 Runner —— 这是路由决策点(不属于任何 Runner 的内部职责)。



### 2.1 Tier 1 — native review(codex / claude)

有内置 review 子命令的 bridge **直接调内置命令**(codex 跑 `codex review --base <ref>`、claude 跑 `claude -p code-review`)。理由:这两家的内置 review 是各自**最优形态**(codex 的 severity 分组、claude 的多 agent + confidence 评分),通用 prompt 抢不过。符合 F-review.md §13"有原生就用原生"原则。**零改动**。

### 2.2 Tier 2 — ocr 委托模式(delegate + ocr 已装)

delegate 档 + ocr 已安装。用 alibaba open-code-review 的**委托模式**:ocr 只做确定性工程(文件选择 + 规则匹配),**LLM-free**;host agent(我们的 dsh / pi / …)用自己 LLM 跑 review。

ocr 两个子命令职责分明(**分组依据来自 rule,不是 preview**):

- `ocr delegate preview` → **扁平**文件清单 + merge_base(commit hash,可解析)+ 排除原因(**不分组**)
- `ocr delegate rule` → **groups**(按 rule content 分组,如 `**/*.go` 一组、`**/*.ts` 一组,每组带该组专属规则)

按 rule 的 groups 拆 jobs:每组 = 一组共享同一规则的文件 + 该组 per-file diff + 该组 rule。同组文件规则上下文一致,放一个 job review 最优。

**收益**:ocr 的精确文件选择 + 规则降噪 + 覆盖率约束,治"漏文件 / 偷懒 / 规则噪声";LLM 走现有 agent;ocr 不配 LLM、无双配置。

**边界**:委托模式下定位 / 反思由 host agent 自己做,**拿不到** ocr 的行级定位 / 反思精度(那是端到端 `ocr review` 的领域,不在 v1 默认路径)。

#### 2.2.1 ocr 上游源 / 本地镜像 / 规则集确认

ocr = alibaba 的 [open-code-review](https://github.com/alibaba/open-code-review)(Apache-2.0,NPM 包 `@alibaba-group/open-code-review`,委托模式 SKILL = `skills/open-code-review-delegate/SKILL.md`)。**外部 CLI**(类 `git`),不进 agent 注册表,不绑 LLM。

为了 (a) 离线审 SKILL / 看实现 / diff 版本,(b) 给 reviewer agent 提供可读的"我在跟哪个上游交互"锚点,本地维护一个浅 clone 镜像:

| 项 | 值 |
| --- | --- |
| 本地路径 | `~/code/geax/github.com/cnlangzi/open-code-review/`(与 `nightme.nightme/`、`seowatson/` 等外部 dep 镜像同款位置) |
| clone 方式 | `git clone --depth=1 https://github.com/alibaba/open-code-review.git`(浅 clone,只保留 HEAD,够查 SKILL / 命令实现 / 当前 tag) |
| 当前 pinned | `v1.9.10` / `66120291271b2e605e420e9f11fbd6448f06163f`(2026-08-24 确认;升级前先看 `cmd/opencodereview/delegate_cmd.go` 与 `skills/open-code-review-delegate/SKILL.md` 是否改 schema) |
| 委托入口(对应 nightme 引用) | `cmd/opencodereview/delegate_cmd.go`(nightme 的 `internal/agent/delegate_review.go` 注释路径直接对得上) |

**`ocr` 子命令全清单**(以本地镜像当前 HEAD 为准,完整列表见 README):

- `ocr config provider` / `ocr config model` —— 配 LLM(委托模式不需要)
- `ocr review [--from/--to/--commit/--resume]` —— 端到端 review(含 LLM,nightme 不走)
- `ocr scan [--path/--resume]` —— 全文件扫描(无 git 历史,nightme 不走)
- **`ocr delegate preview`** —— Tier 2 第一步:扁平文件清单 + merge_base + 排除原因
- **`ocr delegate rule <files...>`** —— Tier 2 第二步:按 rule content 分组的 rules(触发多 job 拆分的依据)
- `ocr session list` —— 会话管理(端到端路径用)

**规则集确认(2026-08-24)**:官方内置多语言规则集以 **NPE / 线程安全 / XSS / SQL 注入** 四类为锚点,**未提供 `simplify` / `simpily` 类规则**。`internal/config/rules/rule_docs/*.md` 全量 grep `simplify` 仅命中 `kotlin.md` 一处,且为英文单词用法(`Use = to simplify single-expression functions`,Kotlin 语法建议),非规则类别。结论:**如果 nightme 想要 simplify 行为,需要 host agent 自己出 prompt,不来自 ocr** —— 这是 v1 默认路径,符合 §2.2 "边界"。

### 2.3 Tier 3 — 优化版 StandardPrompt(delegate + ocr 未装)

delegate 档 + ocr 未装。走**参考 ocr 优化的增强 prompt**(Go 预算 + 内置 rubric)。**不分组,单 job 单 RunOnce**。原 `StandardPrompt()` 只在预计算全失败(非 git repo 等)时作极端 fallback。**零外部依赖,零回归**。

### 2.4 StandardPrompt 的优化(对 Tier 2 / Tier 3 通用)

参考 ocr 的"确定性工程"思想,把原本烤进静态 prompt 里的几项挪到 Go 侧**硬约束**(纯工程,不依赖 LLM):

1. **Go 侧预算 diff**(最高 ROI)—— 治 agent 偷懒 / 漏文件
2. **文件清单 + 排除**(generated / testdata / vendor)—— 治选择不全
3. **覆盖率硬约束**(每文件 reviewed 或 skipped-with-reason)—— 治选择性 review
4. **输出 schema**(path / content / start_line / end_line / category / severity)—— 结构化,便于 fix 定位
5. **规则匹配** —— Tier 2 用 `ocr delegate rule` 的 groups(跟随上游);Tier 3 用内置 / 本地 `review-rules.json`
6. **per-file 截断** —— 每文件 diff 独立阈值(如 300 行)截断,保证所有文件 diff 都在 prompt,单文件超阈才标"read directly"

**关键收敛**:1–4 项是纯 Go 工程,两档都做;第 5 项规则匹配是 Tier 2 / Tier 3 的差异点;第 6 项防单 job 的 prompt 膨胀。ocr 在与不在的区别收敛到"规则匹配"一项,优化惠及所有 delegate bridge,且不绑死 ocr。

---

## 2.5 多 job 并发机制(大 changeset)

Tier 2 按 ocr rule groups 拆多 job 时,核心是**每 job 独立 RunOnce / 独立 context + 自动并发 + merge**。

### 2.5.1 为什么拆 job(防大 changeset 爆)

单 RunOnce 塞全量 diff,大 PR(几十文件、几千行)会撞 host agent 的 context 窗口,review 静默降级(丢 findings / 丢文件)。按 rule group 拆:每 job 只含本组 diff,**独立 context**(fresh 子进程,不累积),从根上防爆。这是 ocr 端到端 smart bundling 的 **code-driven 等价**——用 ocr 的 rule groups 做分组边界,RunOnce 各自独立。

### 2.5.2 触发条件(自动)

| 情况 | 路径 |
| --- | --- |
| ocr 在 + 多 group | 拆多 job → **并发多 RunOnce** + merge |
| ocr 在 + 单 group(如全 Go 项目) | 单 job → 单 RunOnce(不并发) |
| ocr 不在(Tier 3) | 单 job → 单 RunOnce(增强 prompt,不分组) |
| 预计算全失败 | 单 RunOnce(原 StandardPrompt fallback) |

无需用户开关——多 group 自动并发,单 group 自动单 RunOnce。

### 2.5.3 并发控制

- **sem 上限**(如 4):防 N 个 agent 子进程同时跑爆 token / 撞 API rate。超 sem 的 job 排队等空位。
- **单 job 失败不阻塞**:某组 RunOnce 失败,其他组仍出结果;merge 时该组标"failed"。
- **ctx 派生**:所有 job 共用 DelegateReview 的 revCtx(chat session ctx + 30min 超时)。`/close` → 全部取消。

### 2.5.4 merge(一次返回)

各 job 输出**结构化 markdown**(`## Coverage` + `## Findings` schema)。LLM 自己解析——merge 阶段**不做**结构化解析、不按 severity 排序、不做 path 去重、不做 coverage 聚合。原因:

- 各 agent 输出的 schema 实现细节不一致(claude 的 confidence / codex 的 severity group / ocr 的 category enum 等),**结构不稳定**,统一解析器易碎
- 主 agent(消费侧)对 markdown 是 LLM,**自然语言理解足以**接住 findings;结构化字段只是 optional hint
- 降低 merge 路径的复杂度 = 降低 surface area = 减少回归风险

merge 步骤只有两步:

1. **按组标头拼接**:每组 `### Group: pattern X — files: A, B, C` 头 + 该组原文 markdown。失败组标 `### Group: pattern X — failed: <err>`。
2. **一次返回**:合成的 RunResult 经 `FormatReviewMessage` 注入 AS + 发 channel 一次。

partial failure 路径:**不**升级为 merge 整体错误,失败组以 inline marker 出现在合并文本里;all-failed 才返回 first error wrapped with agent name。

### 2.5.5 sink 契约:三相状态机 + per-job 配对

多 job 时,cmd.go 喂给 DelegateReview 的 outer sink(`outbound.StreamRunOnceToEmitter`)必须**只看到一个 review lifecycle**,不能感知到内部 N 个并发 job。eventAggregator 用三相状态机实现这一契约:

```text
┌─────────────────────────────────────────────────────────────────────┐
│   Phase 1 (buffering)                                                │
│   ─────────────────                                                   │
│   per-job 进来的所有事件进 perJob.initBuffer,**不外发**              │
│   readyCount++; doneCount++(per-job terminalSeen 守卫防重复)         │
│   当 readyCount == expected:                                          │
│     ┌── 锁内 ─────────────────────────────────────────────────────┐  │
│     │  phase = phaseStreaming                                       │  │
│     │  snapshot 各 perJob.initBuffer,清空                          │  │
│     │  构建 synthetic outer Ready(merged metadata,Source="")        │  │
│     └──────────────────────────────────────────────────────────────┘  │
│     ┌── 锁外 ─────────────────────────────────────────────────────┐  │
│     │  outer(synthetic outer Ready)                                 │  │
│     │  replay 各 perJob 的 snapshot → handleStreaming               │  │
│     │    (应用 ToolStart/End 配对、Task 合并 ID 去重等)             │  │
│     └──────────────────────────────────────────────────────────────┘  │
├─────────────────────────────────────────────────────────────────────┤
│   Phase 2 (streaming)                                                 │
│   ───────────────────                                                 │
│   live 事件到达 → handleStreaming 立刻处理:                          │
│     - ToolStart → pendingToolStarts[ID] = ev(**不转发**)            │
│     - ToolEnd   → 查 pendingToolStarts[ID],有则按 Start→End 连续    │
│                   转发两条事件;无则 forward 孤儿 End                  │
│     - TaskCreate/Update → 按 ID dedup 后 forward ONE merged snapshot │
│     - Text / Permission → forward as-is,Source="group-N"             │
│     - Result/Error → doneCount++(terminalSeen 守卫);不外发           │
│   当 doneCount == expected:                                           │
│     outer(synthetic outer Result,Source="") → Phase 3               │
├─────────────────────────────────────────────────────────────────────┤
│   Phase 3 (closed)                                                    │
│   ────────────────                                                    │
│   late events 忽略(已发出的 outer lifecycle 已闭合)                  │
└─────────────────────────────────────────────────────────────────────┘
```

**关键设计**:

- **#11 单 outer lifecycle**:上层只看到一次 `outer Ready` 和一次 `outer Result`。N 个 per-job Ready + N 个 per-job Result 在聚合器内消化掉,只在 all-wait 条件满足时各合成一个外发。
- **#12 per-job ToolStart/End 配对**:每个 per-job 一个 `pendingToolStarts map[string]AgentEvent`。ToolStart 进缓冲不外发,等配对 ToolEnd 到达后**Start → End 顺序连续**转发两条事件。保证 chat 渲染层看到的 tool 调用是完整块,不出现半截 open call。
- **#13 Task 跨 job 去重**:aggregator 维护 `tasks map[string]AgentTaskItem`,每个 TaskCreate/Update 进来按 ID 写入(latest wins),forward 一份合并 snapshot。避免 chat checklist 看到 N 份重复条目。
- **进程事件实时流**:Phase 2 期间 process 事件立即外发(Source="group-N" 让 chat 渲染层区分),review 期间用户能实时看到 "[group-1] tool: Read /home/repo/foo.go" 这种进度。
- **异序到达容错**:job-1 先报 Result、其他还没报 Ready 的情况:`doneCount > readyCount` 短暂成立,但**不**触发 outer Result(Phase 还在 buffering);等所有 Ready 后才触发 Phase 2,然后等所有 Done 才触发 outer Result。**Ready 优先于 Done 的合成外发**。
- **Replay 一致性**:Phase 1→2 转换时,旧 initBuffer 经同一个 `handleStreaming` 路径走完配对/合并逻辑,与 live 事件应用**同一套规则**,行为完全一致。

**为什么 outer sink 能安全接收**:outbound.StreamRunOnceToEmitter 底层是 chan-based,channel send 跨 goroutine 线程安全。aggregator 持锁区只做状态变更,`outer(ev)` 调用在锁外完成,避免持锁回调。

### 2.5.6 边界

- **超大单组**:某 group diff 仍超大(如 `**/*.go` 100 文件 5000 行)→ per-file 截断兜底(§2.4 第 6 项);极端时按目录二次拆(留后续)。
- **顺序 vs 并发**:默认并发(快);若并发有风险(如 agent 不支持并发 session),可降级顺序(loop 无 sem)——但已验证 delegate bridge 的 RunOnce 并发安全(dsh sessionId 多路复用 / 其他独立子进程),默认并发。
- **跨 job 事件流交错**:Phase 2 期间不同 job 的 process 事件在 outer 上**可能交错**(Source="group-N" 区分);**同一 job 内部** ToolStart→ToolEnd 严格配对连续(per-job 缓冲保证)。
- **Replay vs live 交错**:Phase 1→2 转换时,replay 处理 per-job 旧事件可能与同 job 新到的 live 事件在 outer 上交错。当前实现的已知 minor race:同 job 内 replay 事件 + live 事件少数情况下顺序非严格 chronological。后果仅为 chat 时间线略不规整,无功能影响。修复方案(若需要)是在转换时加 replay-in-progress barrier。
- **未配对 ToolStart**:job 在 ToolStart 后异常结束(无对应 ToolEnd),该 Start 进 buffer 后永远不被 forward——这是合理行为,chat 不会看到半截 call;该 job 的 done 仍会按 Result/Error 触发。
- **orphan ToolEnd**:配对 ToolStart 已先被 replay 转发过的罕见情况——orphan End 也 forward,chat 看到一条无 Start 的 End,可忽略。

### 2.5.7 实现索引(v12)

| 概念 | 实现位置 |
| --- | --- |
| 三层分流入口(Tier 2 多 job / Tier 2 单 job / Tier 3 分支) | `internal/agent/delegate_review.go::DelegateReview` |
| 多 job 并发编排(sem cap 4,N 个 goroutine,各自独立 ctx) | `internal/agent/delegate_review.go::delegateReviewMultiJob` |
| per-group 提示词(context / file list / diff / rule / how-to / schema) | `internal/agent/delegate_review.go::assembleGroupPrompt` |
| 按文件过滤 diff(`git diff -- <files...>`) | `internal/agent/delegate_review.go::groupFilteredDiff` |
| **三相状态机 + per-job 配对缓冲**(Phase 1 buffering → Phase 2 streaming → Phase 3 closed;pendingToolStarts map per-job;Task 跨 job ID 去重;异序到达容错) | `internal/agent/aggregate_sink.go::eventAggregator` |
| **多 job 结果合并**(纯自然语言拼接 + 部分失败 inline marker,无解析/排序/去重/coverage 聚合) | `internal/agent/delegate_review.go::mergeRunResults` |
| 单元测试(聚合器 / 合并 / 并发 / 配对) | `internal/agent/aggregate_sink_test.go`、`internal/agent/merge_results_test.go` |

**不变量与实现的对应**:

- **#1 全异步**:`Handle` 立即返回,goroutine 跑 review,revCtx 派生自 chat session ctx。
- **#2 / #9 RunOnce 隔离**:每个 per-job 是独立 `s.RunOnce`(独立子进程 + 独立 context),多 job 间不共享、不累积。
- **#3 ctx 派生 + /close 取消**:所有 job 共用 `revCtx`(chat session ctx + 30min 超时)。
- **#4 双路分发**:`FormatReviewMessage` 一次产出的合并文本 → 注入 AS(user turn)+ 发 channel 一次。
- **#10 自动并发 + sem + merge 一次返回**:`sem chan struct{}` cap 为 `maxConcurrentReviewJobs = 4`;`wg.Wait()` 同步;`mergeRunResults` 一次产出最终 RunResult。
- **#11 单 outer lifecycle**:eventAggregator 的三相状态机在 readyCount/doneCount all-wait 时各合成一次 outer Ready / outer Result,per-job lifecycle 不外泄。
- **#12 per-job ToolStart/End 配对**:eventAggregator 的 `perJob.pendingToolStarts` 按 ID 缓冲 ToolStart,等配对 ToolEnd 时按 Start→End 顺序连续转发。
- **#13 Task 跨 job 去重**:eventAggregator 的 `tasks map[string]AgentTaskItem`,latest-wins 写入,forward 合并 snapshot。

---

## 2.6 Simplify lens(nightme-owned,并行 group)

`ReviewWithOcr` 和 `ReviewWithPrompt` 都会追加一个 nightme-owned 的 **simplify** review group,作为独立的并行维度(独立 RunOnce)跟主 review(ocr groups 或 builtin rubric)一起跑。simplify 不是 prompt 末尾追加,而是一个完整的 reviewGroup,有自己的 Pattern sentinel (`_nightme_simplify`),所以它走跟 ocr/builtin groups 完全一样的 fan-out 路径。

### 来源

简化规则的 4 axes 借鉴自 Claude Code 的 `/simplify` skill(reuse / simplification / efficiency / altitude),但形态调整为 **review findings** 而非 **refactor apply** —— 它产出发现,不自动改代码。

### 实现

```go
// review_with_ocr.go
const (
    patternBuiltin  = "_nightme_builtin"   // ReviewWithPrompt 主 group
    patternSimplify = "_nightme_simplify"  // 永远追加
)

func simplifyGroup(files []string) reviewGroup {
    return reviewGroup{Pattern: patternSimplify, Files: files, Rule: SimplifyPrompt}
}

// ReviewWithOcr:precomputeReview + 追加 simplifyGroup → 风扇
groups := append(pre.ocrGroups, simplifyGroup(pre.reviewable))

// ReviewWithPrompt:builtinGroup + simplifyGroup → 风扇
groups := []reviewGroup{builtinGroup(pre.reviewable), simplifyGroup(pre.reviewable)}
```

simplify group 的 prompt 通过 `assembleGroupPrompt` 渲染:`switch g.Pattern` 命中 `patternSimplify` 分支,header 是 `# Simplify review lens (nightme-owned, complementary)`,rule 文本是 `SimplifyPrompt` const。

### Scope

| Runner | simplify group 出现? |
|---|---|
| `ReviewWithNative`(claudecode / codex) | ❌ 不出现(桥自己处理 prompt) |
| `ReviewWithOcr`(ocr 已装) | ✅ 始终追加 |
| `ReviewWithPrompt`(ocr 未装 / fallback) | ✅ 始终追加 |

### ocr 检测的位置(SRP)

`OcrAvailable()` 是 agent 包导出的函数。delegate-tier 桥的 `Starter.Review` 自己做 ocr 检测,然后决定调哪个 runner:

```go
// 5 个 delegate 桥的 Starter.Review(统一形态)
func (s *Starter) Review(ctx, cfg, opts...) (RunResult, error) {
    if agent.OcrAvailable() {
        return agent.ReviewWithOcr(ctx, s, cfg, opts...)
    }
    return agent.ReviewWithPrompt(ctx, s, cfg, opts...)
}
```

`ReviewWithOcr` 内部不再做 `OcrAvailable` 检查或 fallback —— 单一职责:假设 ocr 可用,跑 ocr 委托模式。如果调用方在 ocr 不可用时调它,那是调用方 bug,不该偷偷 fallback。

### 为什么不用 `WithSimplifyPrompt` option

早期 v3-v7 讨论过用 functional option `WithSimplifyPrompt()` 控制 simplify 启用。否决原因:
- simplify 是 simplify-as-group(独立 RunOnce),不是 prompt 末尾追加的 section —— 没法用 option 控制 prompt 字段
- 始终启用 simplify 比 opt-in/opt-out 简单,且 simplify 的 findings 是低风险补充(默认 severity = `low` / `style`)
- 未来若要 disable,只需把 simplifyGroup 从 group 列表里删,改 1 个 helper 函数



---

## 3. 数据流:从 `/review` 到 findings

1. 用户发 `/review [--agent <name>]`。
2. dispatcher 解析 args(`--agent` / `-a`),inline 校验未知 flag。
3. 解析 **inject 目标**:当前 chat 的 selected AS(lookup,daemon 重启后自动 spawn)。
4. 解析 **review runner**:`--agent` 覆盖则用其,否则用当前 AS 的 agent;查 agent 注册表拿 Starter。
5. 启动 goroutine(chat session ctx 派生 + 30min 超时):
   1. 接 sink,把 review 的中间事件(思考 / 工具调用)**流式**进 chat(观察用)。
   2. `Starter.Review` → **三层分流**:
      - Tier 1 native:调内置命令。
      - Tier 2/3 delegate:Go 预算 → (单 job 单 RunOnce / 多 job 并发多 RunOnce + merge)→ review 文本。
   3. `FormatReviewMessage` 包前缀(workspace + runner 标注,让主 agent 知道"这是谁跑的 review")。
   4. **双路分发**:注入 AS 当 user turn(主 agent 能"fix blockers")+ 发 channel(用户直接可见,不等下游回复)。
6. Handle 立即返回 `Consumed=true`,**无 inline reply**。readpump 继续,dispatch worker 释放,用户可继续发消息。findings 异步到达。

---

## 4. 隔离与生命周期

- **RunOnce 是隔离机制**:fresh 子进程,独立 context 窗口。review 推理(可烧数万 token)不污染主 chat。
- **多 job 各自独立 context**:大 changeset 按 rule group 拆的每个 job 都是独立 fresh RunOnce——不共享 context、不累积,防爆。区别于"同进程多轮 + /new reset"的弱隔离(依赖 agent reset 彻底),fresh spawn 是 OS 级强隔离,零信任。
- **超时**:Review 30min(同 `/gtw commit` 的 Agent 预算)。RunOnce 边界使超时安全:只杀 review 子进程,主 chat 不受影响。多 job 并发共用同一超时。
- **ctx 派生**:goroutine 用 chat session 的 ctx 当 parent。`/close` → ctx cancel → 所有 review 子进程 kill → goroutine 退出。无 orphan。
- **多 reviewer 并行**:多个 `/review --agent` 并发,各自独立 RunOnce,findings 全注入同一 AS。用户体验:多条 review 接连出现,然后一次"fix"。
- **sink vs deliverable**:sink 是观察用(流式中间事件),`FormatReviewMessage` 文本是交付物。两路独立,不冲突。

---

## 5. 设计原则

1. **agent-delegated** —— nightme 自己不调 LLM,review 推理交给现有 agent。ocr 委托模式符合这点(ocr LLM-free,工程产出喂 agent)。
2. **ocr 不是 bridge** —— ocr 是狭义 agent(只 review,不能 chat / Edit / `/use`)。塞进 agent 注册表扭曲 bridge 语义(bridge = 通用编码 agent)。ocr 是被调用的外部工具(类 git)。
3. **native 优先** —— 有内置 review 的 bridge 调内置,不跑通用 prompt。尊重各家最优形态,符合 F-review.md §13。
4. **优雅降级** —— delegate 档 ocr 缺失 → Tier 3 优化 prompt,不报错不阻塞。review 在任何环境可用。
5. **fix 对话式** —— review 只产出 findings,不自动改代码。主 agent 用原生 Edit 工具 fix。保持 v1 纯 review。
6. **`--agent` 不切 AS** —— runner 是一次性 spawn,findings 回当前 AS。不同 reviewer,同一 chat。
7. **独立 context 分 bundle(code-driven)** —— 大 changeset 按 ocr rule group 拆多 job,每 job 独立 fresh RunOnce(强隔离,不累积)。这是 ocr smart bundling 的 code-driven 等价——用 ocr 的 rule groups 做分组边界,RunOnce 各自独立,不是 ocr 端到端的重 multi-agent 机器。

---

## 6. 不做的事(边界)

- ❌ 把 ocr 当 bridge / 进 agent 注册表 —— 它是被调用的外部工具。
- ❌ 动 native review(codex / claude 内置)—— 有内置就调内置。
- ❌ 把 ocr 端到端(`ocr review`)设为默认 —— 需配 ocr LLM、双配置,留 opt-in(`--engine ocr`)。
- ❌ 引入 ocr 端到端的**重 multi-agent 机器**(ocr 自己的定位 / 反思 / 多 bundle 子 agent)作默认 —— 那是 ocr 端到端跑、依赖 ocr 自己的 multi-agent;我们的 code-driven 拆 job **不是**这个(用 ocr 的 rule groups 只取分组边界,RunOnce 各自独立 context,定位 / 反思仍由 host agent 自己做)。两者本质不同,不冲突。
- ❌ 同进程多轮 + `/new` reset 池化(跨调用复用)—— 破坏 RunOnce 强隔离不变量(#2),`/new` 是弱隔离(依赖 agent reset 彻底),且 review 非高频收益不抵。省 spawn 只在单次 /review 内(瞬态多轮),不跨调用池化。
- ❌ v1 开 `--fix` / `--post` flag —— 留 v2(`/review-fix` / `/review-comment`)。
- ❌ 破坏 `--agent` 不切 AS 的语义 —— ocr 委托模式 findings 仍注入回当前 AS,主 agent 仍能用原生 Edit 工具 fix。

---

## 7. 相关

- [feat/F-review.md](./feat/F-review.md) — v9 原始设计稿:三 pattern(native/delegate/不支持)、flag 取舍、多 reviewer 并行风险评估、`--agent` 不切 AS 的论证。
- [feat/F-review-ocr-fusion.md](./feat/F-review-ocr-fusion.md) — ocr 融合的可行性论证、三层分流的落地计划、风险与待验证项(双配置 / 跨平台 / prompt 膨胀等)。
- [flow/three-layer-sync.md](./flow/three-layer-sync.md) — ChatSession → AgentSession → Conversation 三层,`/review` 跑在这之上。
- [SPEC.md](./SPEC.md) §1 — 七个逻辑组件、不变式。
