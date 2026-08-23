# /review 设计

> **Status**: 设计定稿(v11,含 ocr 委托模式三层分流 + 按 rule group 拆多 job 并发),待实施
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

### 1.2 边界

- **是**:对"当前分支 vs 默认分支"(PR 模式)的代码变更做 review,产出 findings 文本回 chat,主 agent 可对话式 fix。
- **不是**:不开 `--fix` / `--post`(留 v2 的 `/review-fix` / `/review-comment`);不限定文件 / base(v1 除 `--agent` 外零限定符);不当 CI gating;不自动改代码。

---

## 2. 整体架构:三层分流

review 的执行不是一条路,而是按 **bridge 类型 + ocr 可用性**分流。基础是 F-review.md §13 的三 pattern:native override / delegate StandardPrompt / 不支持。在 delegate 档里再按 ocr 是否安装分两层,且 ocr 在时**按 rule group 拆多 job 并发**。

```text
/review [--agent <name>]
   │  解析 --agent,选定 runner;runner ≠ 当前 AS 时,findings 仍回当前 AS
   ▼
bridge 自报能力分流
   │
   ├─ 有内置 review?(codex / claude)
   │     → Tier 1: 调内置 review 命令
   │        〔各家自己的最强形态,含定位 / 多 agent,不画蛇添足〕
   │
   ├─ delegate 档?(dsh / pi / acp / opencode / cursor)
   │     → 统一分流入口(DelegateReview)
   │        ┌─ Go 通用预算(默认分支 / merge_base / diff / 文件清单 / 覆盖率 / schema)
   │        │   〔StandardPrompt 的 ocr 化优化,Tier 2 与 Tier 3 都做〕
   │        ├─ ocr 已安装?
   │        │    YES → Tier 2: ocr delegate preview(文件+merge_base+排除)
   │        │                 + ocr delegate rule → groups(按 rule content 分组)
   │        │                 → 按 groups 拆 jobs(每组: files + per-file diff + 该组 rule)
   │        │    NO  → Tier 3: 单 job(增强 prompt,Go 预算 + 内置 rubric,不分组)
   │        │
   │        ├─ 单 job?  → 单 RunOnce(单 prompt)
   │        └─ 多 job?  → 并发多 RunOnce(sem 上限,各自独立 context)
   │                     → merge: findings 合并 + severity 排序 + 跨组覆盖率汇总
   │
   └─ 不支持?(pty / bash)
         → ErrReviewNotSupported → 友好提示("不是 coding agent")
```

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

各 job 输出结构化 findings(path / content / start_line / end_line / category / severity),merge:

- findings 合并 + 按 severity 排序(critical > high > medium > low)
- 跨组 coverage_rate 汇总(total = Σ reviewable,reviewed = Σ reviewed,skipped = Σ skipped)
- 拼一份 markdown,**一次返回**给 cmd.go(FormatReviewMessage + 双路分发)

按 rule group 拆时,一文件只在一组,findings 不重复;merge 仍按 path 去重保险。

### 2.5.5 sink 复用

cmd.go 的 sink(中间事件流式进 chat)被多 job 并发复用——sink 是 chan-based(线程安全),多 job 并发调它安全。用户看到:各 job 的思考 / 工具调用实时流进 chat(review 在跑),最终合并 findings 一次发回。可选给事件加 `[group]` 标记区分来源。

### 2.5.6 边界

- **超大单组**:某 group diff 仍超大(如 `**/*.go` 100 文件 5000 行)→ per-file 截断兜底(§2.4 第 6 项);极端时按目录二次拆(留后续)。
- **顺序 vs 并发**:默认并发(快);若并发有风险(如 agent 不支持并发 session),可降级顺序(loop 无 sem)——但已验证 delegate bridge 的 RunOnce 并发安全(dsh sessionId 多路复用 / 其他独立子进程),默认并发。

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
