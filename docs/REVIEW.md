# /review 设计

> **Status**: 设计定稿(单一 dispatcher + 两种 Runner + ocr 路径按 rule group 拆多 job 并发 + simplify 作为并行 group + 每 group 走 agent session 路径)。所有 bridge 走 `agent.ReviewDispatch` 统一入口——`OcrAvailable() ? ReviewWithOcr : ReviewWithPrompt`。**Agent 自带 review 子命令(`claude -p code-review` / `codex review` / `cursor-agent -p /review-bugbot`)不再走 `Starter.Review` 特殊路径**,用户直接在 chat 发 slash command,nightme 透传给底层 agent。**§2.5 多 job 并发机制已实现**(`internal/agent/aggregate_sink.go` + `review_with_ocr.go::delegateReviewMultiJob`,详见 §2.5.7 实现索引)
> **Scope**: `/review` slash 命令的架构、分流、数据流、生命周期与边界
> **读者**: 参与 command / agent / bridge / chatsession 任一层,或想理解 review 设计意图的工程师
> **Related docs**:
>
> - [feat/F-review.md](./feat/F-review.md) — v9 原始设计稿(fallback pattern、flag 取舍、多 reviewer 并行风险评估)
> - [feat/F-review-ocr-fusion.md](./feat/F-review-ocr-fusion.md) — ocr 委托模式融合的可行性论证、落地计划、风险与待验证项
> - [flow/three-layer-sync.md](./flow/three-layer-sync.md) — ChatSession → AgentSession → Conversation 三层(`/review` 跑在这之上)

---

## 1. 设计目标

让用户在任意 IM chat 里发 `/review`,对当前 workspace 的代码变更做一次 code review,findings 回到**同一 chat**,主 agent 能据此 fix。review 在**隔离的 agent session**里跑(独立进程 / 独立 session,独立 context 窗口),不污染主 chat 上下文;支持**多 reviewer 并行**(`--agent`),findings 汇聚到同一 chat。

### 1.1 核心不变量

| # | 不变量 | 违反后果 |
| --- | --- | --- |
| 1 | `/review` 全异步:Handle 立即返回,goroutine 跑 review | dispatch worker / readpump 阻塞,chat 卡死 |
| 2 | review 走 agent session 路径(独立进程 / 独立 session),独立 context 窗口 | 主 chat 上下文被 review 推理污染(可烧数万 token) |
| 3 | review session 的 ctx 派生自 chat session ctx,`/close` 自动取消 | orphan review 进程 / session 残留 |
| 4 | findings **双路分发**:注入 AS(当 user turn)+ 发 channel(直接可见) | 主 agent 看不到 findings,或用户要等下游回复 |
| 5 | `--agent` **不切 AS**(不同 reviewer,同一 chat) | 切 AS 副作用太大,破坏"多 reviewer 汇聚"工作流 |
| 6 | fix **对话式**:不自动 apply,用户说"fix critical findings"(沿用 output schema 的 severity 词汇 critical/high/medium/low)主 agent 用原生 Edit | 违反 v1 "纯 review"边界 |
| 7 | **二选一**:`OcrAvailable() ? ReviewWithOcr : ReviewWithPrompt`,所有 bridge 走 `agent.ReviewDispatch` 同一入口 | review 行为跨 agent 不一致,或不同环境不可复现 |
| 8 | ocr 始终是被调用的**外部工具**(类 git),不进 agent 注册表 | 把狭义 agent 当 bridge,扭曲 bridge 语义 |
| 9 | 大 changeset 按 ocr rule group 拆多 job,**每 job 独立 session / 独立 context**,不累积 | 单 context 塞全量 diff 爆窗口,或同进程多轮累积爆 |
| 10 | 多 job **自动并发**(sem 上限),merge 后一次返回 | 顺序跑 N job 慢;无上限并发爆 token / 撞 API rate |
| 11 | 多 job 时,**上层只看到一个 review lifecycle**(单 Ready / 单 Result) | chat channel 看到 N 个 ready / N 个 result,StatusBar 翻转混乱 |
| 12 | per-job 内的 ToolStart 必须**等配对 ToolEnd 才转发**,Start/End 在外层 wire 上**连续** | 半截 tool 调用外发,chat 渲染混乱 |
| 13 | 跨 job 的 EventAgentTaskCreate/Update **按 task ID 去重 merge**,同一 ID 多个 job 的最新版本只发一份 | chat checklist 看到 N 份重复 task 条目,数量 = N × tasks |
| 14 | `--continue` **只续跑失败的 group**,已完成的 group 不再问一遍 | 重复的 findings 混进输出 |

### 1.2 边界

- **是**:对"当前分支 vs 默认分支"(PR 模式)的代码变更做 review,产出 findings 文本回 chat,主 agent 可对话式 fix。
- **不是**:不开 `--fix` / `--post`(留 v2 的 `/review-fix` / `/review-comment`);不限定文件 / base(只认 `--agent` / `--global` / `--unset` / `--continue` 四个 flag,短写 `-a` / `-g` / `-c`,不收位置参数);不当 CI gating;不自动改代码。

---

## 2. 整体架构:统一 dispatcher + 两种 Runner

review 由所有 bridge 统一走 `agent.ReviewDispatch`(单一入口):`OcrAvailable() ? ReviewWithOcr : ReviewWithPrompt`。两个 Runner 都 fan-out 出 ≥2 个 reviewGroup(ocr/builtin 主 group + simplify),走同一个 `delegateReviewMultiJob`。详见 §2.6。

agent 包不再区分 "native / mixed / delegate" 桥——每条桥的 `Starter.Review` 都是同一行 `return agent.ReviewDispatch(ctx, s, cfg, opts...)`。Agent 自带的 review 子命令(`claude -p code-review` / `codex review` / `cursor-agent -p /review-bugbot`)用户直接在 chat 发 slash command,nightme 透传给底层 agent——不再通过 `Starter.Review` 特殊路径触发。

```text
/review [-a|--agent <name>] [-g|--global] [--unset] [-c|--continue]
   │  解析 flags(共享 lexer),选定 runner;runner ≠ 当前 AS 时,findings 仍回当前 AS
   │  -g / --global:把 runner 落成全局 `review.agent`(~/.nightme/config.yaml)
   │  --unset:删掉全局 `review.agent`,不跑 review
   │  -c / --continue:续跑本 chat 上一次 review 未完成的 group(见 §2.7);
   │                  与 -a / -g / --unset 互斥
   ▼
Starter.Review(所有 bridge 都是同一行)
   │
   └─ agent.ReviewDispatch(ctx, s, cfg, opts...)
          │
          ├─ OcrAvailable() YES → ReviewWithOcr
          │     ├─ pre := precomputeReviewWithOcr(workspace)
          │     │     ocr delegate preview → reviewable + excluded + mergeBase
          │     │     ocr delegate rule    → ocrGroups (N per-pattern)
          │     │     git                 → base + merge-base
          │     ├─ groups = pre.ocrGroups + simplifyGroup(pre.reviewable)
          │     └─ delegateReviewMultiJob(sem cap 4,每 group 一个 session turn,eventAggregator,mergeRunResults)
          │
          └─ OcrAvailable() NO  → ReviewWithPrompt
                ├─ pre := precomputeReviewWithBuiltin(workspace)
                │     git → base + merge-base
                │     Go → 4 个 git 来源(inlined)+ 合成 1 个 builtin group
                ├─ groups = pre.ocrGroups(builtin) + simplifyGroup(pre.reviewable)
                └─ delegateReviewMultiJob(同上)

pty / bash → ErrReviewNotSupported → 友好提示("不是 coding agent")
```

### 两个 precompute 函数,产物 shape 一致

`precomputeReviewWithOcr` 和 `precomputeReviewWithBuiltin` 两条路径共用一个 `reviewContext` shape,fan-out machinery 不知道也不关心是哪条路径产出的:

```text
ReviewWithOcr                          ReviewWithPrompt
    ↓                                       ↓
precomputeReviewWithOcr                precomputeReviewWithBuiltin
    ├─ git: detectDefaultBranch           ├─ git: detectDefaultBranch
    ├─ git: merge-base                    ├─ git: merge-base
    ├─ ocr delegate preview               ├─ inlined: 4 个 git 来源 → reviewable + untracked
    │   → reviewable (ocr FileFilter)     │   → reviewable (Go isReviewablePath)
    │   → excluded (with reasons)         │   → untracked (新文件,无 diff)
    ├─ ocr delegate rule                  ├─ synthesize 1 个 builtin group
    │   → ocrGroups (N groups, per-pattern) │   → ocrGroups = [{patternBuiltin, BuiltinPrompt}]
    ↓                                       ↓
    reviewContext (same shape)              reviewContext (same shape)
    ↓                                       ↓
    groups = pre.ocrGroups + simplifyGroup(pre.reviewable)
    ↓
    delegateReviewMultiJob → fan-out
```

diff 不在 precompute 阶段算——每个 rule group 在组装 prompt 时自己切自己的 diff(`git diff -- <files...>`),那时才知道该 group 的字节预算怎么分(见 §2.4)。

**字段对照**:

| 字段 | ocr 路径 | Go 路径 |
|---|---|---|
| `reviewable` | ocr FileFilter(精度高) | Go 4 个 git 来源(committed/staged/unstaged)+ `isReviewablePath`(启发式) |
| `untracked`  | ocr 提供的未 tracked 文件列表 | Go 端 `git ls-files --others --exclude-standard`(独立于 reviewable,因为没有 diff 可渲染)|
| `excluded` | ocr 提供的排除原因列表 | nil(Go 端不跟踪 excluded)|
| `ocrGroups` | N 个(每 pattern 一组,Rule 是 ocr rule doc)| 1 个 builtin group(Pattern=patternBuiltin, Rule=BuiltinPrompt)|
| merge-base | 都有(同)| 都有(同)|

`ReviewWithOcr` 调 `precomputeReviewWithOcr`,`ReviewWithPrompt` 调 `precomputeReviewWithBuiltin`。两个 Runner 函数返回值都是 `RunResult`,行为形状一致。`agent.ReviewDispatch` 按 `OcrAvailable()` 选择调用哪个 Runner —— 路由决策集中在这一个 helper,不在任何 Runner 的内部职责里。




### 2.2 ocr 路径(ocr 已装,`ReviewWithOcr`)

ocr CLI 已装,`agent.ReviewDispatch` 走 `ReviewWithOcr`。用 alibaba open-code-review 的**委托模式**:ocr 只做确定性工程(文件选择 + 规则匹配),**LLM-free**;host agent(我们选定的 bridge)用自己 LLM 跑 review。

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
| 委托入口(对应 nightme 引用) | `cmd/opencodereview/delegate_cmd.go`(nightme 的 `internal/agent/review_with_ocr.go` 注释路径直接对得上) |

**`ocr` 子命令全清单**(以本地镜像当前 HEAD 为准,完整列表见 README):

- `ocr config provider` / `ocr config model` —— 配 LLM(委托模式不需要)
- `ocr review [--from/--to/--commit/--resume]` —— 端到端 review(含 LLM,nightme 不走)
- `ocr scan [--path/--resume]` —— 全文件扫描(无 git 历史,nightme 不走)
- **`ocr delegate preview`** —— ocr 路径第一步:扁平文件清单 + merge_base + 排除原因
- **`ocr delegate rule <files...>`** —— ocr 路径第二步:按 rule content 分组的 rules(触发多 job 拆分的依据)
- `ocr session list` —— 会话管理(端到端路径用)

**规则集确认(2026-08-24)**:官方内置多语言规则集以 **NPE / 线程安全 / XSS / SQL 注入** 四类为锚点,**未提供 `simplify` / `simpily` 类规则**。`internal/config/rules/rule_docs/*.md` 全量 grep `simplify` 仅命中 `kotlin.md` 一处,且为英文单词用法(`Use = to simplify single-expression functions`,Kotlin 语法建议),非规则类别。结论:**如果 nightme 想要 simplify 行为,需要 host agent 自己出 prompt,不来自 ocr** —— 这是 v1 默认路径,符合 §2.2 "边界"。

### 2.3 Go 复刻的 builtin 路径(ocr 未装,`ReviewWithPrompt`)

ocr CLI 未装,`agent.ReviewDispatch` 走 `ReviewWithPrompt`。`precomputeReviewWithBuiltin` 用 4 个 git 来源(inlined)+ `isReviewablePath` 启发式收集 reviewable / untracked,合成 1 个 `patternBuiltin` 的 ocrGroup(Rule = `BuiltinPrompt` const,跟 ocr 路径的 N 个 per-pattern ocrGroup 等价)。**仍然 fan-out 出 builtin + simplify 两个 group** —— 跟 ocr 路径走同一条 `delegateReviewMultiJob` 路径,只是 file-list 精度低一档。

workspace 为空 / precompute 全失败时,fan-out 退化为 `[simplifyGroup(nil)]` 单 goroutine,prompt 用 `BuiltinPrompt` 兜底。**零外部依赖,零回归**。

### 2.4 prompt 工程(对 ocr / builtin 路径通用)

参考 ocr 的"确定性工程"思想,把原本烤进静态 prompt 里的几项挪到 Go 侧**硬约束**(纯工程,不依赖 LLM):

1. **Go 侧预计算 diff** —— 治 agent 偷懒 / 漏文件
2. **文件清单 + 排除** —— `isReviewablePath` 启发式(generated / testdata / vendor 等)
3. **reviewable vs untracked 分离** —— 改了/未改的文件走 diff 通道;**新文件(untracked)走单独通道**,因为 `git diff <untracked>` 全空,把它们混进 reviewable 会让 LLM 看到文件列表却没有 diff 内容,违反 coverage mandate
4. **覆盖率硬约束** —— 每文件 reviewed 或 skipped-with-reason
5. **输出 schema** —— path / content / start_line / end_line / category(`bug|security|performance|maintainability|test|style|documentation|other`)/ severity(`critical|high|medium|low`),结构化便于 fix 定位
6. **规则匹配** —— ocr 路径用 `ocr delegate rule` 返回的 ocrGroups(每个 pattern 一组);builtin 路径用 `precomputeReviewWithBuiltin` 合成的一个 builtinGroup(全部文件 + BuiltinPrompt)。两者**形状一致**,fan-out 不区分
7. **per-group 字节预算** —— `groupDiffBudgetBytes = 1048576`(1 MiB)兜底,一个 rule group 的三段 diff 共享这个预算;超预算的段按行边界截断,并提示"read directly"

**diff 预算(per group)**:

- 一个 rule group 的 committed / staged / unstaged 三段 diff 共享 `groupDiffBudgetBytes = 1048576`(1 MiB)预算——约 30% 的 1M token context,余下留给系统 prompt、tool schema、文件清单、agent 输出,以及 agent 被截断后自己去读文件的开销。
- 预算按各段未截断的大小切分(fair-share):预算像水位一样上涨,每段取 `min(自身大小, 水位)`;比水位小的段保留完整内容,余量让给还差额的段——单段过大不会饿死小段。各段配额之和 ≤ 预算。
- 超配额的段按**行边界**截断(不切半行),段尾追加 `…(diff truncated: N bytes dropped; read the affected files directly for full context)…`。
- 放得下内容、放不下 marker 时,段内容退化为 marker 本身——agent 仍知道这段被截断、该去读哪些文件;配额小到连 marker 都放不下时,整段省略。
- 被截断的 group 记一条 Info 日志:group pattern / 预算 / 实际 payload 字节数 / 丢弃字节数。

**关键收敛**:1–5 项是纯 Go 工程,两档都做;第 6 项规则匹配是 ocr / builtin 路径的差异点(ocr 拿多 pattern groups,builtin 拿 Go 合成的一个 builtinGroup);第 7 项防单 group 的 prompt 膨胀。ocr 在与不在的区别收敛到"规则匹配"一项的精度(ocr FileFilter vs Go 启发式)。

`BuiltinPrompt` 本身不再携带 simplify 规则(原 `StandardPrompt` 里有这条 bullet,已删)—— simplify 作为独立并行 group 跑(`simplifyPrompt` const,小写未导出),不再在 BuiltinPrompt 里冗余出现。severity 词汇统一为 `critical/high/medium/low`,跟 `assembleGroupPrompt` 的 output schema 一致 —— 不再有 `blocker/major/minor/nit` 与 `critical/high/medium/low` 跨 group 冲突。

---

## 2.5 多 job 并发机制(大 changeset)

ocr 路径按 ocr rule groups 拆多 job 时,核心是**每 job 独立 agent session / 独立 context + 自动并发 + merge**。

**传输层:prompt 走 session pipe,不走 argv**

每个 rule group 的 prompt 通过 agent 的 **session 路径**投递(`agent.RunSession`:Start → SendBlocks → drain → Close)。prompt 写进 agent 的**协议管道**(stdio / JSON-RPC / ACP),管道没有长度上限,group prompt 多大都能送达 —— 如果 prompt 是 argv 元素,Linux 单个 argv 字符串上限 131072 字节(32 × PAGE_SIZE),Windows 整条命令行上限 32767 字符,超了直接 spawn 失败(`argument list too long`)。

所以 review 的隔离单位是**一个 group 一个 session**(独立进程 / 独立 session id),一个 group 跑**一轮**:prompt 一次性送达,agent 的回答即该 group 的终态事件。

session 路径的 turn 结束信号有两种,`agent.DrainSession` 两种都接(先到先算):

| 终态事件 | 走这条路的 bridge | 正文来源 |
| --- | --- | --- |
| `EventAgentResult` | claudecode / dsh / omp / pi | 累积的 `EventAgentText` + `Result.Text` |
| `EventAgentDone{Reason: "settled"}` | codex / acp 系(opencode / copilot / cursor) | 累积的 `EventAgentText` |

正文可能分两截到:bridge 在 turn 过程中以 `EventAgentText` 流出已说出的部分,结束时把还压着的尾巴放在终态事件上(claudecode / codex 的终态事件不带正文,正文全在 text 事件里)。drain 把两截按顺序拼成完整回答。

**可选能力 `agent.TurnText`**:bridge driver 如果实现了 `TurnText() string`,drain 优先取它给出的**原样正文**,只有它为空时才回退到上面那条"事件流拼装"。acp bridge 实现了这个能力 —— 它按句 flush 自己的正文缓冲,每个 flush 的块都被 trim,落在 flush 边界上的段落空行就没了;driver 另外维护一份 per-turn 的原文累加器,one-shot drain 取它,PR body 的空行因此不丢。其余 bridge 不实现:它们的流式拼装本来就无损。

下列情况算**失败**(该 group 标 failed,其余 group 的 findings 照常汇入):

- turn 结束但没有终态事件(事件流先关)。
- 终态事件不是上述两种(例如 `Done{Reason: "interrupted"}` 的中止 turn、或进程退出)——session 在 turn 之后仍然活着,继续等下去只会等到调用方的 deadline。
- turn 结束但没有正文(空回答)——drain 拒绝空回答。

review 路径没有交互用户,drain 遇到 `EventAgentPermission` 时用**第一个选项**自动放行("Allow once" 或问题首个标签),不等用户。

### 2.5.1 为什么拆 job(防大 changeset 爆)

单个 session turn 塞全量 diff,大 PR(几十文件、几千行)会撞 host agent 的 context 窗口,review 静默降级(丢 findings / 丢文件)。按 rule group 拆:每 job 只含本组 diff,**独立 context**(fresh session,不累积),从根上防爆。这是 ocr 端到端 smart bundling 的 **code-driven 等价**——用 ocr 的 rule groups 做分组边界,各 job 的 session 各自独立。

### 2.5.2 触发条件(自动)

两条 Runner 路径都**永远 fan-out**(v14 起,因为 simplify group 总是追加,groups 永远 ≥ 2)。具体 fan-out 数:

| 情况 | fan-out groups | goroutine 数 |
| --- | --- | --- |
| ocr 在 + N 个 ocr group | N + 1(N 个 ocrGroup + 1 个 simplifyGroup) | N + 1 |
| ocr 在 + ocrGroup 为空(ocr 失败) | 0 + 1(只剩 simplifyGroup) | 1 |
| ocr 不在 + 1 builtinGroup | 1 + 1(builtinGroup + simplifyGroup) | 2 |
| ocr 不在 + workspace 空 | 0(只有空 goroutine fallback 到 `BuiltinPrompt`)| 1 |

无需用户开关——`delegateReviewMultiJob` 自动按 groups 数 spawn goroutine,sem cap 4 限并发。

### 2.5.3 并发控制

- **sem 上限**(如 4):防 N 个 agent session 同时跑爆 token / 撞 API rate。超 sem 的 job 排队等空位。
- **单 job 失败不阻塞**:某组 session turn 失败,其他组仍出结果;merge 时该组标"failed"。
- **ctx 派生**:所有 job 共用 `ReviewWithOcr` / `ReviewWithPrompt` 的 revCtx(chat session ctx + 30min 超时)。`/close` → 全部取消。

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

多 job 时,cmd.go 喂给 `ReviewWithOcr` / `ReviewWithPrompt` 的 outer sink(`outbound.StreamRunOnceToEmitter`)必须**只看到一个 review lifecycle**,不能感知到内部 N 个并发 job。eventAggregator 用三相状态机实现这一契约:

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

- **超大单组**:某 group diff 仍超大(如 `**/*.go` 100 文件 5000 行)→ per-group 字节预算兜底(§2.4 第 7 项),超预算的段按行截断并标记丢弃字节数;极端时按目录二次拆(留后续)。
- **单组超模型 context 窗口**:`_nightme_simplify` group 永远带全部 reviewable 文件,大 changeset 下单个 group 的 prompt 仍可能超出模型的 context 窗口。传输层不受限(管道无长度上限),但模型侧会失败,该 group 标记 failed,其余 group 的 findings 照常汇入。
- **顺序 vs 并发**:默认并发(快);若并发有风险(如 agent 不支持并发 session),可降级顺序(loop 无 sem)——但已验证 delegate bridge 的 session 并发安全(dsh sessionId 多路复用 / 其他独立子进程),默认并发。
- **跨 job 事件流交错**:Phase 2 期间不同 job 的 process 事件在 outer 上**可能交错**(Source="group-N" 区分);**同一 job 内部** ToolStart→ToolEnd 严格配对连续(per-job 缓冲保证)。
- **Replay vs live 交错**:Phase 1→2 转换时,replay 处理 per-job 旧事件可能与同 job 新到的 live 事件在 outer 上交错。当前实现的已知 minor race:同 job 内 replay 事件 + live 事件少数情况下顺序非严格 chronological。后果仅为 chat 时间线略不规整,无功能影响。修复方案(若需要)是在转换时加 replay-in-progress barrier。
- **未配对 ToolStart**:job 在 ToolStart 后异常结束(无对应 ToolEnd),该 Start 进 buffer 后永远不被 forward——这是合理行为,chat 不会看到半截 call;该 job 的 done 仍会按 Result/Error 触发。
- **orphan ToolEnd**:配对 ToolStart 已先被 replay 转发过的罕见情况——orphan End 也 forward,chat 看到一条无 Start 的 End,可忽略。

### 2.5.7 实现索引

| 概念 | 实现位置 |
| --- | --- |
| 统一 dispatcher(所有 bridge 的 Starter.Review 都走它) | `internal/agent/review.go::ReviewDispatch` |
| ocr Runner 入口 | `internal/agent/review_with_ocr.go::ReviewWithOcr` |
| builtin Runner 入口(ocr 不在 / precompute 失败的 fallback) | `internal/agent/review.go::ReviewWithPrompt` |
| ocr 路径 precompute(ocr delegate preview + rule) | `internal/agent/review_with_ocr.go::precomputeReviewWithOcr` |
| Go 路径 precompute(detectDefaultBranch + merge-base + 4 git 来源 file enumeration(inlined) + builtin group 合成) | `internal/agent/review_with_ocr.go::precomputeReviewWithBuiltin` |
| `OcrAvailable` 导出函数(`ReviewDispatch` 用) | `internal/agent/review_with_ocr.go::OcrAvailable` |
| 多 job 并发编排(sem cap 4,N 个 goroutine,各自独立 ctx) | `internal/agent/review_with_ocr.go::delegateReviewMultiJob` |
| **session 路径的一次性调用**(Start → SendBlocks → drain → Close;两种终态事件都接;permission 自动放行) | `internal/agent/run_session.go::RunSession` / `::DrainSession` |
| per-group 提示词(context / file list / diff / rule / how-to / schema),按 `g.Pattern` 分 header(ocr / builtin / simplify) | `internal/agent/review_with_ocr.go::assembleGroupPrompt` |
| 按文件过滤 diff(`git diff -- <files...>`,未截断) | `internal/agent/review_with_ocr.go::groupFilteredDiff` |
| per-group diff 字节预算(1 MiB,三段共享 + 按行截断 + 截断日志) | `internal/agent/review_with_ocr.go::groupDiffBudgetBytes` / `::allocateDiffQuotas` / `::fitDiffBody` |
| `BuiltinPrompt` / `simplifyPrompt` 静态 prompt 模板 | `internal/agent/review.go` |
| **三相状态机 + per-job 配对缓冲**(Phase 1 buffering → Phase 2 streaming → Phase 3 closed;pendingToolStarts map per-job;Task 跨 job ID 去重;异序到达容错) | `internal/agent/aggregate_sink.go::eventAggregator` |
| **多 job 结果合并**(纯自然语言拼接 + 部分失败 inline marker,无解析/排序/去重/coverage 聚合) | `internal/agent/review_with_ocr.go::mergeRunResults` |
| **review / 续跑共用的 fan-out 内核**(goroutine 编排 + sem cap + eventAggregator + merge) | `internal/agent/review_with_ocr.go::runReviewGroups` |
| **续跑入口**(并发 resume N 个 session、发同一段文本、合并成同样的 group 格式) | `internal/agent/review_continue.go::ContinueReviewJobs` |
| **每个 group 的 outcome 观察者**(pattern / files / session id / error) | `internal/agent/agent.go::ReviewJob` / `::WithReviewJobObserver` |
| **失败 turn 保留 session id**(进程没了但 agent 侧会话还在,session id 是重新打开它的钥匙) | `internal/agent/run_session.go::drainSessionEvents` / `::ResumeSession` |
| **`/review` 的续跑状态**(按 chat id,只活在 daemon 进程内存里) | `internal/command/review/record.go` |
| 单元测试(聚合器 / 合并 / 并发 / 配对 / session drain) | `internal/agent/aggregate_sink_test.go`、`internal/agent/merge_results_test.go`、`internal/agent/fanout_test.go`、`internal/agent/run_session_test.go` |

**不变量与实现的对应**:

- **#1 全异步**:`Handle` 立即返回,goroutine 跑 review,revCtx 派生自 chat session ctx。
- **#2 / #9 session 隔离**:每个 per-job 是独立 `agent.RunSession`(独立进程 / 独立 session + 独立 context),多 job 间不共享、不累积。
- **#3 ctx 派生 + /close 取消**:所有 job 共用 `revCtx`(chat session ctx + 30min 超时)。
- **#4 双路分发**:`FormatReviewMessage` 一次产出的合并文本 → 注入 AS(user turn)+ 发 channel 一次。
- **#10 自动并发 + sem + merge 一次返回**:`sem chan struct{}` cap 为 `maxConcurrentReviewJobs = 4`;`wg.Wait()` 同步;`mergeRunResults` 一次产出最终 RunResult。
- **#11 单 outer lifecycle**:eventAggregator 的三相状态机在 readyCount/doneCount all-wait 时各合成一次 outer Ready / outer Result,per-job lifecycle 不外泄。
- **#12 per-job ToolStart/End 配对**:eventAggregator 的 `perJob.pendingToolStarts` 按 ID 缓冲 ToolStart,等配对 ToolEnd 时按 Start→End 顺序连续转发。
- **#13 Task 跨 job 去重**:eventAggregator 的 `tasks map[string]AgentTaskItem`,latest-wins 写入,forward 合并 snapshot。

---

## 2.6 Simplify lens(nightme-owned,并行 group)

`ReviewWithOcr` 和 `ReviewWithPrompt` 都会追加一个 nightme-owned 的 **simplify** review group,作为独立的并行维度(独立 session turn)跟主 review(ocr groups 或 builtin rubric)一起跑。simplify 不是 prompt 末尾追加,而是一个完整的 reviewGroup,有自己的 Pattern sentinel (`_nightme_simplify`),所以它走跟 ocr/builtin groups 完全一样的 fan-out 路径。

### 来源

简化规则的 4 axes 借鉴自 Claude Code 的 `/simplify` skill(reuse / simplification / efficiency / altitude),但形态调整为 **review findings** 而非 **refactor apply** —— 它产出发现,不自动改代码。

### 实现

```go
// review_with_ocr.go
const (
    patternBuiltin  = "_nightme_builtin"   // ReviewWithPrompt 的 builtinGroup
    patternSimplify = "_nightme_simplify"  // 永远追加
)

func simplifyGroup(files []string) reviewGroup {
    return reviewGroup{Pattern: patternSimplify, Files: files, Rule: simplifyPrompt}
}

// ReviewWithOcr:precomputeReviewWithOcr → pre.ocrGroups + simplifyGroup → 风扇
groups := append(pre.ocrGroups, simplifyGroup(pre.reviewable))

// ReviewWithPrompt:precomputeReviewWithBuiltin → 1 builtinGroup(已含在 pre.ocrGroups) + simplifyGroup → 风扇
// (precomputeReviewWithBuiltin 合成 ocrGroups = [{patternBuiltin, BuiltinPrompt}],所以同样 append simplifyGroup)
groups := append(pre.ocrGroups, simplifyGroup(pre.reviewable))
```

两个 Runner 函数**主体相同**:都 `pre := precomputeReview*(ctx, workspace); groups := append(pre.ocrGroups, simplifyGroup(pre.reviewable)); return delegateReviewMultiJob(...)`。区别只在调哪个 `precomputeReview*`。

simplify group 的 prompt 通过 `assembleGroupPrompt` 渲染:`switch g.Pattern` 命中 `patternSimplify` 分支,header 是 `# Simplify review lens (nightme-owned, complementary)`,rule 文本是 `simplifyPrompt` const。

### Scope

| Runner | simplify group 出现? |
|---|---|
| `ReviewWithOcr`(ocr 已装) | ✅ 始终追加 |
| `ReviewWithPrompt`(ocr 未装 / fallback) | ✅ 始终追加 |

### ocr 检测的位置(SRP)

`OcrAvailable()` 是 agent 包导出的纯存在性检查(`exec.LookPath("ocr")`)。`agent.ReviewDispatch` 是唯一的 dispatch 点 —— 所有桥的 `Starter.Review` 都是同一行 `return agent.ReviewDispatch(ctx, s, cfg, opts...)`,内部按 `OcrAvailable()` 选 `ReviewWithOcr` 或 `ReviewWithPrompt`:

```go
// 全部 8 个桥的 Starter.Review(单一形态)
func (s *Starter) Review(ctx context.Context, cfg agent.StartConfig, opts ...agent.RunOnceOption) (agent.RunResult, error) {
    return agent.ReviewDispatch(ctx, s, cfg, opts...)
}

// agent.ReviewDispatch
func ReviewDispatch(ctx context.Context, s Starter, cfg StartConfig, opts ...RunOnceOption) (RunResult, error) {
    if OcrAvailable() {
        return ReviewWithOcr(ctx, s, cfg, opts...)
    }
    return ReviewWithPrompt(ctx, s, cfg, opts...)
}
```

`ReviewWithOcr` 内部不再做 `OcrAvailable` 检查或 fallback —— 单一职责:假设 ocr 可用,跑 ocr 委托模式。如果调用方在 ocr 不可用时调它,那是调用方 bug,不该偷偷 fallback。

### 为什么不用 `WithSimplifyPrompt` option

早期 v3-v7 讨论过用 functional option `WithSimplifyPrompt()` 控制 simplify 启用。否决原因:
- simplify 是 simplify-as-group(独立 session turn),不是 prompt 末尾追加的 section —— 没法用 option 控制 prompt 字段
- 始终启用 simplify 比 opt-in/opt-out 简单,且 simplify 的 findings 是低风险补充(默认 severity = `low` / `style`)
- 未来若要 disable,只需把 simplifyGroup 从 group 列表里删,改 1 个 helper 函数



---

## 2.7 续跑未完成的 group(`/review --continue`)

review 是 fan-out:每个 rule group 一个独立 agent session(§2.5)。某个 group 的
session 挂了,其它 group 的 findings 照常汇入,合并文本里该 group 标成
`### Group: pattern <p> — failed: <err>`。`/review --continue` 把这次 review 捡
起来:**只**续跑那些失败的 group,给它们的 session 发一个字面量 `continue`,让
agent 接着上次的上下文把活干完;已经完成的 group 不再问一遍(否则 findings 会重复
出现在输出里)。

### 续跑需要的状态

| 状态 | 来源 | 说明 |
|---|---|---|
| runner | 上一次 review 跑的那个 agent | session 属于某个 agent,换不了,所以 `--continue` 不走四级 runner 解析 |
| workspace | 上一次 review 的 workspace | session 属于那个 workspace;换 cwd 会让 agent 看错文件 |
| 每个 group 的 session id | group 自己的 agent session | 失败的 group 也必须能捞回 session id,否则没有可续跑的 session |

第三项是唯一需要动底层的地方:`drainSessionEvents` 的失败路径原本返回空
`RunResult`,session id 只留在 error 的 audit 后缀里。失败路径现在返回带上
session id(和 model)的 result —— 进程没了,但 agent 侧的会话还在,session id
就是重新打开它的钥匙(`agent.ResumeSession` 把 `StartConfig.SessionID` 交给
bridge,bridge 用自己的方式 resume:`--resume <id>` / `resume_id` /
`--session-id <id>` / ACP 的 `session/resume` 或 `session/load` / dsh 的 session
握手)。bridge 拒绝 resume 时报 `agent.ErrResumeUnhealthy`,不会静默开一个新
session。

### fan-out 内核复用

fresh-session 的 review 和续跑共用同一套 fan-out 内核
(`internal/agent/review_with_ocr.go::runReviewGroups`):goroutine 编排、sem 上限
4、eventAggregator(单 outer lifecycle)、`mergeRunResults`(同样的
`### Group: pattern <p> — files: …` 段落 + inline `failed:` 标记)。两者的差别只有
"每个 group 跑什么":review 跑新 session + group prompt,续跑跑 resume 的 session +
`continue`。

`agent.ContinueReviewJobs` 是续跑的入口:传入若干 `ReviewJob`(pattern / files /
session id)和要发的文本,并发 resume,事件走同一个 aggregator,回复按同样的 group
格式合并。

### 续跑状态放在哪

`/review` 命令的 factory 按 chat id 记着上一次 review 的状态(runner /
workspace / 每个 group 的 outcome)。factory 每个 daemon 进程建一次,所以这份状态
只活在进程内,daemon 重启即丢 —— 重启后 `--continue` 报
`❌ no review to continue — run /review first`。落盘不在这条路径上:resume 状态
不需要持久化,一个 chat 也绝不续跑另一个 chat 的 review。

每个 group 的 outcome 由 fan-out 内核通过 `agent.WithReviewJobObserver` 回调交给
命令层:pattern、files、session id、error。失败 group 的 session id 一并保留 ——
那正是下一轮 `--continue` 要用的。

记录什么时候被替换:

- **跑出 group 的 review 结束时** —— 记录换成这次 review 每个 group 的 outcome。
  一个 group 都没跑出来的 review(没有 diff 可 review、bridge 不支持 review)不
  动记录:没有新的 resume 状态,上一次 review 没收尾的 group 依然没收尾。
- **续跑结束时** —— 记录换成这次续跑的 outcome。写回是**条件写**:只有当记录
  仍然是这次续跑读的那份时才写。续跑期间跑完的另一次 review 拥有这个 chat 的
  resume 状态,不能被更早开始、更晚结束的续跑覆盖。

### 边界

| 场景 | 行为 |
|---|---|
| 本 chat 没有 review 记录 | `❌ no review to continue — run /review first` |
| 本 chat 有 review 在跑 | `❌ a review is already running in this chat` |
| 上一次 review 每个 group 都完成了 | `ℹ️ the last review completed — nothing to continue` |
| 失败的 group 根本没到 agent(没有 session id) | `ℹ️ the unfinished group never reached the agent — nothing to resume` |
| 失败的 group 已经续跑过一次(还是失败) | `ℹ️ the unfinished group was already retried — nothing to resume` |
| 记录的 agent 已经不在注册表里 | `❌ unknown agent "<name>"` |
| bridge 拒绝 resume(`ErrResumeUnhealthy`) | 该 group 在合并文本里 inline 标 failed(所有 group 都失败时:只发 `❌ /review continue failed (agent=<name>): …`) |
| 续跑又失败 | 合并文本里 inline 标 failed;该 group 的 resume 机会已用完,再 `--continue` 无事可做 |
| 所有 group 都失败 | 整个续跑失败,只发失败回复,不注入 findings |

`--continue` 与 `--agent` / `--global` / `--unset` 互斥:那三个 flag 选的是"这次
用哪个 runner",而 `--continue` 续的是"上一次那个 runner"。

---

## 3. 数据流:从 `/review` 到 findings

1. 用户发 `/review [-a|--agent <name>] [-g|--global] [--unset] [-c|--continue]`。
2. dispatcher 用共享 lexer(`command.ParseCmdArgs`)解析 args,inline 校验未知 flag、位置参数与非法 flag 组合(`--unset` 需要 `--global`、`--unset` 与 `--agent` 互斥、`--global` 需要 `--agent`、`--continue` 与 `--agent` / `--global` / `--unset` 互斥)。
3. **落全局设置**:`-g` 把 runner 写进 `~/.nightme/config.yaml` 的 `review.agent`,`--unset` 删掉它(`--unset` 不跑 review)。写盘发生在查 chat session 之前——全局设置与本 chat 有没有 workspace 无关;写失败只告警(`⚠️ failed to save review.agent: … — reviewing with <name> anyway`),review 照跑。落盘走 node 级写入,config 文件里的注释 / 未建模的 key / 其它 section 原样保留。
4. 解析 **inject 目标**:当前 chat 的 selected AS(lookup,daemon 重启后自动 spawn)。
5. 解析 **review runner**(4 级):① `-a` / `--agent` → ② 全局 `review.agent` → ③ 当前 AS 的 agent → ④ 都没有 → `❌ no active agent configured. Run /use <agent> first.`。② 里的名字没在 agent 注册表里时降级到 ③ 并告警(`⚠️ review.agent "x" is not a registered agent — using <session agent>`),config 文件里的陈旧名字不会让 `/review` 变成死命令。查 agent 注册表拿 Starter。
6. 启动 goroutine(chat session ctx 派生 + 30min 超时):
   1. 接 sink,把 review 的中间事件(思考 / 工具调用)**流式**进 chat(观察用)。
   2. `Starter.Review` → **统一 dispatcher**:
      - 所有 bridge 调 `agent.ReviewDispatch` → `OcrAvailable() ? ReviewWithOcr : ReviewWithPrompt`。
      - Agent 自带的 review 子命令(`claude -p code-review` / `codex review` / `cursor-agent -p /review-bugbot`)不在 `Starter.Review` 路径里触发——用户直接在 chat 发 slash command,nightme 透传给底层 agent。
   3. `FormatReviewMessage` 包前缀(workspace + runner 标注,让主 agent 知道"这是谁跑的 review")。
   4. **双路分发**:注入 AS 当 user turn(主 agent 能"fix critical findings")+ 发 channel(用户直接可见,不等下游回复)。
7. Handle 立即返回 `Consumed=true`。readpump 继续,dispatch worker 释放,用户可继续发消息。findings 异步到达;全局设置的回执(写入确认 / 写失败告警 / 降级告警)随本条命令的 reply 一起发出,先于 findings 到达。

`--continue` 走另一条路:查本 chat 的 review 记录 → 本 chat 有 review 在跑就拒绝 →
挑出"失败且有 session id"的 group → 用记录里的 agent(不走四级 runner 解析)和记录里
的 workspace 跑 `agent.ContinueReviewJobs` → 合并文本照常包 `FormatReviewMessage`、
注入 AS + 发 channel(§2.7)。续跑结束后,记录换成这次续跑的结果,所以第二次
`--continue` 无事可做。

---

## 4. 隔离与生命周期

- **session 是隔离机制**:每 group 一个独立 agent session(独立进程 / 独立 session id),独立 context 窗口。review 推理(可烧数万 token)不污染主 chat。
- **多 job 各自独立 context**:大 changeset 按 rule group 拆的每个 job 都是独立 fresh session——不共享 context、不累积,防爆。区别于"同进程多轮 + /new reset"的弱隔离(依赖 agent reset 彻底),fresh session 是 OS 级强隔离,零信任。
- **prompt 走管道**:group prompt 经 session 的协议管道送达 agent,不占 argv(见 §2.5 传输层)。管道无长度上限,大 changeset 的 group prompt 不会撑爆 spawn 参数。
- **超时**:Review 30min(同 `/gtw commit` 的 Agent 预算)。session 边界使超时安全:只杀 review 进程,主 chat 不受影响。多 job 并发共用同一超时。
- **ctx 派生**:goroutine 用 chat session 的 ctx 当 parent。`/close` → ctx cancel → 所有 review session 关闭 → goroutine 退出。无 orphan。
- **多 reviewer 并行**:多个 `/review --agent` 并发,各自独立 session,findings 全注入同一 AS。用户体验:多条 review 接连出现,然后一次"fix"。
- **续跑同样是 session 隔离**:resume 打开的是 agent 侧同一个 session(新进程 + 老 session id),agent 的上下文延续,主 chat 上下文依旧不被污染。
- **sink vs deliverable**:sink 是观察用(流式中间事件),`FormatReviewMessage` 文本是交付物。两路独立,不冲突。

---

## 5. 设计原则

1. **agent-delegated** —— nightme 自己不调 LLM,review 推理交给现有 agent。ocr 委托模式符合这点(ocr LLM-free,工程产出喂 agent)。
2. **ocr 不是 bridge** —— ocr 是狭义 agent(只 review,不能 chat / Edit / `/use`)。塞进 agent 注册表扭曲 bridge 语义(bridge = 通用编码 agent)。ocr 是被调用的外部工具(类 git)。
3. **统一 dispatcher** —— 所有 bridge 走 `agent.ReviewDispatch`,二选一(`OcrAvailable() ? ReviewWithOcr : ReviewWithPrompt`),行为跨 agent 一致。Agent 自带 review 子命令(如 `claude -p code-review` / `codex review` / `cursor-agent -p /review-bugbot`)用户直接在 chat 发 slash command,nightme 透传给底层 agent——不再走 `Starter.Review` 特殊路径。
4. **优雅降级** —— ocr 缺失 → `ReviewWithPrompt` 走 Go 复刻路径(`precomputeReviewWithBuiltin` 内联 4 个 git 来源 + 合成 builtinGroup 模拟 ocr 的产出形状),不报错不阻塞。review 在任何环境可用,只是 file-list 精度低一档(Go 启发式 vs ocr FileFilter)。
5. **fix 对话式** —— review 只产出 findings,不自动改代码。主 agent 用原生 Edit 工具 fix。保持 v1 纯 review。
6. **`--agent` 不切 AS** —— runner 是一次性 spawn,findings 回当前 AS。不同 reviewer,同一 chat。
7. **独立 context 分 bundle(code-driven)** —— 大 changeset 按 ocr rule group 拆多 job,每 job 独立 fresh session(强隔离,不累积)。这是 ocr smart bundling 的 code-driven 等价——用 ocr 的 rule groups 做分组边界,各 job 的 session 各自独立,不是 ocr 端到端的重 multi-agent 机器。
8. **全局 runner 是机器级单键** —— `review.agent` 只有全局一档,没有 repo scope、没有环境变量 override:一个键只要一个 source of truth。落盘走 node 级写入,用户 config 里的注释 / 未建模的 key / 其它 section 原样保留;config 解析失败时不覆写文件。

---

## 6. 不做的事(边界)

- ❌ 把 ocr 当 bridge / 进 agent 注册表 —— 它是被调用的外部工具。
- ❌ 在 `Starter.Review` 里再分 native / non-native 分支 —— 所有 bridge 必须走 `agent.ReviewDispatch` 同一入口,行为一致。Agent 自带 review 子命令由 chat 层透传。
- ❌ 把 ocr 端到端(`ocr review`)设为默认 —— 需配 ocr LLM、双配置,留 opt-in(`--engine ocr`)。
- ❌ 引入 ocr 端到端的**重 multi-agent 机器**(ocr 自己的定位 / 反思 / 多 bundle 子 agent)作默认 —— 那是 ocr 端到端跑、依赖 ocr 自己的 multi-agent;我们的 code-driven 拆 job **不是**这个(用 ocr 的 rule groups 只取分组边界,各 job 的 session 各自独立 context,定位 / 反思仍由 host agent 自己做)。两者本质不同,不冲突。
- ❌ 同进程多轮 + `/new` reset 池化(跨调用复用)—— 破坏 session 强隔离不变量(#2),`/new` 是弱隔离(依赖 agent reset 彻底),且 review 非高频收益不抵。省 spawn 只在单次 /review 内(瞬态多轮),不跨调用池化。
- ❌ v1 开 `--fix` / `--post` flag —— 留 v2(`/review-fix` / `/review-comment`)。
- ❌ 把 review 的续跑状态落盘 —— 状态只活在 daemon 进程内存里,按 chat id 隔离(§2.7)。
- ❌ 破坏 `--agent` 不切 AS 的语义 —— ocr 委托模式 findings 仍注入回当前 AS,主 agent 仍能用原生 Edit 工具 fix。
- ❌ 给 review runner 加 repo 级 override / 环境变量 override —— 全局单键已经够用,多一档就多一个 source of truth。

---

## 7. 相关

- [feat/F-review.md](./feat/F-review.md) — v9 原始设计稿:flag 取舍、多 reviewer 并行风险评估、`--agent` 不切 AS 的论证。
- [feat/F-review-ocr-fusion.md](./feat/F-review-ocr-fusion.md) — ocr 融合的可行性论证、二选一机制的落地计划、风险与待验证项(双配置 / 跨平台 / prompt 膨胀等)。
- [flow/three-layer-sync.md](./flow/three-layer-sync.md) — ChatSession → AgentSession → Conversation 三层,`/review` 跑在这之上。
- [SPEC.md](./SPEC.md) §1 — 七个逻辑组件、不变式。
