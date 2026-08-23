# F-review-ocr — 将 alibaba/open-code-review 融入 nightme `/review` 的可行性报告

> 基于:`internal/command/review/cmd.go`(v9)、`internal/agent/review.go`(StandardPrompt)、`internal/bridge/{dsh,pi,acp,opencode,cursor,codex,claudecode}/starter.go`(Review 实现形态)、`docs/feat/F-review.md`(v9 设计稿)、`configs/nightme.example.yaml` + `internal/config`(nightme 无 LLM 层) 与 alibaba/open-code-review 的 **Delegation Mode**(`cmd/opencodereview/delegate_cmd.go` + `skills/open-code-review-delegate/SKILL.md`)。

---

## 0. 一句话结论

**三层分流,ocr 是被调用的外部工具而非 bridge。**

1. **有内置 review 的 agent(codex / claude)** → 继续调内置 review 命令,**不动**。
2. **无内置 review 的 agent(dsh / pi / acp / opencode / cursor)+ 装了 ocr** → 走 **ocr 委托模式**(`ocr delegate preview/rule`,LLM-free),工程产出拼进 prompt 喂给该 agent。
3. **无内置 review + 没装 ocr** → 走**优化版 `StandardPrompt`**(参考 ocr 做 Go 侧预算 + 覆盖率 + schema 增强)。

分流逻辑集中在一个新 helper `agent.DelegateReview`,5 个 delegate bridge 的 `Review` 各改一行替换过去;native bridge 和 pty(不支持)零改动。ocr 不进 `agent.Builtins`、不配 LLM、无双配置。

---

## 1. 系统对比

| 维度 | nightme `/review` | ocr 端到端(`ocr review`) | ocr **委托模式**(`ocr delegate`) |
| --- | --- | --- | --- |
| **谁来做 review** | 编码 agent 读 prompt | ocr 专用 Go agent + LLM 循环 | **host agent**(我们的 claude/codex/pi) |
| **LLM 来源** | 现有 agent 的 LLM | ocr 自配 LLM endpoint | **现有 agent 的 LLM**(ocr 侧 LLM-free) |
| **文件选择** | prompt 让 agent 自己跑 diff | 工程层精确选择 | **工程层精确选择**(JSON 输出) |
| **规则匹配** | 烤进 prompt | 按文件特征模板匹配 | **按文件特征模板匹配**(JSON 输出) |
| **定位 / 反思** | agent 自由报 file:line(有漂移) | ocr 独立定位 / 反思模块 | host agent 自己定位(无 ocr 反思模块) |
| **是否需配 LLM** | 否 | 是 | **否**(委托模式不调 LLM) |
| **接入形态** | 内置 slash command | 端到端 CLI / skill | 被调用的外部工具(类 git) |

**关键发现**:ocr 官方的委托模式把"deterministic engineering"解耦成 `ocr delegate preview/rule` 两个 **LLM-free** 子命令(JSON 输出),host agent 用自己 LLM 消费——完美契合 nightme 纯 agent-delegated、无 LLM 层的现状。

---

## 2. 契合点

`docs/feat/F-review.md` §12(line 1033、1051、1100):"**不同的 reviewer,同一个 chat**"——`/review --agent <name>` 不切 AS,findings 全注入回当前 AS。

三层分流不引入新 reviewer,而是**增强 delegate 档 reviewer 的输入**:`cmd.go` 的 goroutine、`timeouts.Review`(30 min)、`StreamRunOnceToEmitter`、`FormatReviewMessage`、双路分发(`as.SendBlocks` + `emitter.Send`)、对话式 fix——**全部原样复用**。改的只是 delegate 档的 prompt 构造方式。

---

## 2.5 定位判定:ocr 不是 bridge

### 用户洞察(成立)

ocr 是**狭义 agent**(只做 review,不能 chat / Edit / `/use`),塞进 `agent.Builtins` + `Starter` 当 bridge 会扭曲语义。**正确形态**:ocr 的确定性工程作为前置,喂给现有通用 agent,LLM 走那个 agent。委托模式下 ocr 是被调用的外部工具(类 git)。

### 为什么不能"让 ocr 直接复用现有 agent 的 LLM"(端到端路径)

nightme 是纯 agent-delegated(`configs/nightme.example.yaml` 只有 channel token,`internal/config` 无 LLM/Provider/Model 字段,bridge 只 spawn 成品 agent CLI)。所以不存在"我们 agent 的 LLM endpoint"可指给 ocr;claude/codex CLI 是成品 agent(带 system prompt + 工具 + guardrails),不是裸 LLM,当不了 ocr 的 `llm client`。**委托模式绕开这个死结**:ocr 根本不调 LLM,工程产出是 JSON,host agent 用自己的 LLM 消费。

### 委托模式拿不到什么

委托模式下定位 / 反思由 host agent 自己做(SKILL.md Step 4–6)。ocr 的专有定位 / 反思模块只在端到端 `ocr review` 里。委托模式拿到"文件选择 + 规则匹配 + 覆盖率硬约束"的确定性(治"漏文件 / 偷懒 / 规则噪声"),**拿不到** ocr 的行级定位 / 反思精度——那是后续可选的端到端路径。

---

## 3. 三层分流架构(核心设计)

### 3.1 分流决策

`Starter.Review` 被调起时,按 bridge 类型 + ocr 是否可用分流:

```text
Starter.Review(ctx, cfg, opts)
   │
   ├─ bridge 有 native review override?(codex/claudecode)
   │     YES → Tier 1: 调内置 review 命令(codex review / claude -p code-review)  【不动】
   │
   └─ bridge 是 delegate 档?(dsh/pi/acp/opencode/cursor)
         → 统一调 agent.DelegateReview(ctx, s, cfg, opts)
              │
              ├─ Go 侧通用预算(不管 ocr 在不在):默认分支 + merge_base + 3 条 diff + 文件清单 + 排除
              │
              ├─ exec.LookPath("ocr") 成功?
              │     YES → Tier 2: 跑 ocr delegate preview/rule,拿 ocr 的文件清单(带排除原因)+ 按文件匹配的规则 → 拼增强 prompt
              │     NO  → Tier 3: 用 Go 侧预算的文件清单 + 内置规则(或 .nightme/review-rules.json)→ 拼优化版 StandardPrompt
              │
              └─ s.RunOnce(ctx, cfg, [enhancedPrompt], opts)  【bridge 原有路径不变】
```

**决策表**:

| Agent | Review 入口现状 | 分流归属 | 改动 |
| --- | --- | --- | --- |
| **claudecode** | `runCodeReviewPrintMode`(native) | Tier 1 | 无 |
| **codex** | `runCodexReview`(native) | Tier 1 | 无 |
| **dsh / pi / acp / opencode / cursor** | `s.RunOnce(ctx, cfg, [StandardPrompt()], opts...)` | Tier 2 或 3(看 ocr 是否在) | `Review` 改一行,delegate 到 `agent.DelegateReview` |
| **pty / bash** | `ErrReviewNotSupported` | 不支持 | 无 |

### 3.2 Tier 1 — native review(codex / claude):不动

符合 F-review.md §13 现有 rule:有内置 review 子命令的 bridge **必须**直接调它,不跑我们的通用 prompt。codex 跑 `codex review --base <ref>`,claudecode 跑 `claude -p code-review`。这两家内置 review 是它们自己的最强形态(含各自的定位 / 多 agent 能力),不画蛇添足。

### 3.3 Tier 2 — ocr 委托模式(无 native + ocr 已装)

在 `agent.DelegateReview` 里,Go 侧通用预算之后,`exec.LookPath("ocr")` 成功则:

1. `ocr delegate preview --format json [--from <base> --to HEAD]` → JSON:`mode` / `from`/`to`/`commit`/`merge_base` / `reviewable_files`(path/status/insertions/deletions)/ `excluded_files`(含 reason)/ `background`。**ocr 的文件选择比我们 Go 侧自算更准**(它的 generated/testdata/vendor/exclude 规则更全),覆盖之。
2. `ocr delegate rule --format json <reviewable paths...>` → JSON:按 rule content 分组的 `groups`(group_id/pattern/files/rule)。**ocr 的按文件匹配规则**,比 prompt 内置规则降噪更彻底。
3. (可选)Go 侧按 ocr 给的 merge_base 预算每个文件 diff。
4. 拼装增强 prompt(文件清单 + 每文件规则 + 预算 diff + 覆盖率硬约束 + 输出 schema)→ `s.RunOnce`。

**ocr 侧 LLM-free**(SKILL 明说 "delegation mode never calls an LLM"),不配 ocr LLM,无双配置。用户只需 `npm i -g @alibaba-group/open-code-review`。

### 3.4 Tier 3 — 优化版 StandardPrompt(无 native + ocr 未装)

`exec.LookPath("ocr")` 失败时,走**参考 ocr 优化过的 StandardPrompt**(§3.5 的通用增强),不依赖 ocr。现状的静态 `StandardPrompt()` 升级为 Go 侧动态拼装。**零外部依赖,零回归**。

### 3.5 StandardPrompt 参考 ocr 的优化清单(对 Tier 2 / Tier 3 通用)

这些是 `agent.DelegateReview` 里的**通用前置**,不管 ocr 在不在都做——它们是纯 Go 工程,不依赖 ocr:

1. **(最高 ROI)Go 侧预算 diff** —— 现状 prompt 让 agent 自己跑 `git diff <base>...HEAD` / `--staged` / 工作区。这正是 ocr 诊断的头号痛点"agent 偷懒 / 漏文件"。改成 Go 侧跑这 3 条(+ untracked),把**已算好的 diff**喂进 prompt,agent 只负责判断。治"覆盖不全"。
2. **文件清单 + 排除** —— Go 侧算出变更文件清单,按 generated/testdata/vendor 过滤,显式交给 agent。对应 ocr 的 precise file selection。
3. **覆盖率硬约束** —— prompt 里强制"每个 reviewable 文件必须 reviewed 或 skipped-with-reason",输出 coverage 统计。治"选择性 review"。对应 SKILL Step 6 的 "Coverage is mandatory"。
4. **输出 schema** —— prompt 里规定输出 `path/content/start_line/end_line/category/severity` 结构化字段。对应 SKILL Step 5 的 schema。便于后续 fix 定位 + Phase C 的 schema 统一。
5. **规则**(分流点):
   - Tier 2(ocr 在):用 `ocr delegate rule` 的按文件匹配规则(更准,跟随 ocr 上游)。
   - Tier 3(ocr 不在):保持 prompt 内置规则;若 `<repo>/.nightme/review-rules.json` 存在,Go 侧按文件 glob 匹配注入(轻量复刻 ocr 的 rule.json)。

**关键**:1–4 项对 Tier 2 / Tier 3 都生效(纯 Go 工程);只有第 5 项规则匹配是 Tier 2 / Tier 3 的差异点。这样 ocr 在和不在的区别收敛到"规则匹配"一项,StandardPrompt 优化惠及所有 delegate bridge,且不绑死 ocr。

### 3.6 实现位置

**新增** `internal/agent/delegate_review.go`:

```go
// DelegateReview is the shared Review body for delegate-tier bridges
// (dsh/pi/acp/opencode/cursor). It does Go-side deterministic precompute
// (diff/file-list/coverage/schema) common to Tier 2 & 3, then branches on
// ocr availability for rule resolution, assembles an enhanced prompt, and
// drives s.RunOnce. Native bridges (codex/claudecode) bypass this entirely.
func DelegateReview(ctx context.Context, s Starter, cfg StartConfig, opts ...RunOnceOption) (RunResult, error) {
    precomputed := precomputeReviewContext(ctx, cfg)       // Tier2/3 通用:base/merge_base/diff/file-list
    prompt := assembleReviewPrompt(precomputed, ocrAvailable())  // Tier2: ocr delegate; Tier3: 内置规则
    return s.RunOnce(ctx, cfg, []ContentBlock{{Type: ContentText, Text: prompt}}, opts...)
}
```

**5 个 delegate bridge 各改一行**(`Review` 方法体替换为 delegate):

| Bridge | 现状 | 改为 |
| --- | --- | --- |
| dsh | `return s.RunOnce(ctx, cfg, [StandardPrompt()], opts...)` | `return agent.DelegateReview(ctx, s, cfg, opts...)` |
| pi / acp / opencode / cursor | 同上 + error wrap | `return agent.DelegateReview(ctx, s, cfg, opts...)`(wrap 统一进 helper) |

**native bridge(codex / claudecode)+ pty 零改动**。`StandardPrompt()` 保留(作为 Tier 3 fallback 的 prompt 骨架,由 `assembleReviewPrompt` 内部引用 + 增强)。

---

## 4. 后续可选(Phase B / C)

### Phase B — ocr 端到端作为 opt-in review 执行器,非 bridge(1–2 天)

委托模式(Tier 2)拿不到 ocr 的行级定位 / 反思精度。当用户要这精度时,ocr 作 opt-in 执行器端到端跑 `ocr review --audience agent --format json`,走 `/review --engine ocr`(非 `--agent`,非 bridge),findings 回 chat。需 `ocr config provider` 配 LLM(双配置根源,文档说清)。

### Phase C — 统一输出 schema + 引擎可插拔(1 周)

`Reviewer` 接口藏在 `Starter.Review` 后;统一 `ReviewResult` schema(ocr comment 字段 canonical);`/review --engine native|delegate|ocr|auto`。下游 fix / SARIF / gating 基于同一 schema。

---

## 5. 工作量

| 项 | 做什么 | 工作量 | 风险 | 回报 |
| --- | --- | --- | --- | --- |
| **Tier 2/3 helper** | `agent.DelegateReview` + Go 侧预算 + ocr delegate 调用 + prompt 拼装 | 2–3 天 | 中低(改 prompt 构造,可降级) | 治漏文件 / 降噪 / 覆盖率,LLM 走现有 agent |
| **5 bridge 替换** | dsh/pi/acp/opencode/cursor 的 `Review` 改一行 | 0.5 天 | 低 | 分流集中,后续维护一处 |
| **StandardPrompt 优化** | §3.5 的 1–4 项(通用前置) | 含上 | 低 | 全 delegate bridge 受益,不绑死 ocr |
| **Phase B(可选)** | ocr 端到端 `--engine ocr` | 1–2 天 | 低(纯增量) | 行级定位 / 反思精度 |
| **Phase C(可选)** | `Reviewer` 接口 + schema | ~1 周 | 中 | 多 engine 统一 |

**最小可行**:Tier 2/3 helper + 5 bridge 替换 + StandardPrompt 优化 = 一份完整可用的三层分流。native 不动,pty 不动,ocr 缺失优雅降级。

---

## 6. 不做的事(与 F-review.md 设计原则一致)

- ❌ **不把 ocr 当 bridge / agent** — 委托模式下 ocr 是被 `agent.DelegateReview` 调用的外部工具(类 git),不进 `agent.Builtins`。
- ❌ **不动 native review(codex / claude)** — 有内置 review 的 bridge 继续调内置命令,符合 F-review.md §13。
- ❌ **不把 ocr 端到端设为默认** — Tier 2 用的是 LLM-free 的 delegate(工程层),不是 `ocr review`(需配 ocr LLM,双配置)。端到端走 Phase B opt-in。
- ❌ **不在 v1 开 `--fix` / `--post` flag** — F-review.md §0.3 推到 v2;委托 SKILL Step 7 的 fix 也走对话式路径。
- ❌ **不破坏 `--agent` 不切 AS 的语义** — 三层分流都在 delegate 档内,findings 仍注入回当前 AS,主 agent 仍能用原生 Edit 工具 fix。

---

## 7. 风险与待验证项

1. **ocr 安装依赖** — Tier 2 需 `ocr` 二进制(`npm i -g`),但**不需配 LLM**(委托 LLM-free)。Tier 3 无任何外部依赖。对策:`DelegateReview` 里 `exec.LookPath("ocr")`,缺失降级 Tier 3。
2. **跨平台二进制** — ocr 经 npm + GitHub release,验 Windows/macOS/Linux。Tier 2 仅依赖 `ocr delegate`(纯 Git 工程,无 LLM),比端到端轻。
3. **超时** — `timeouts.Review = 30 min`;Tier 2 多跑 2 个 delegate 子命令(毫秒级),增量可忽略。
4. **输出截断** — 大 changeset 的 preview/rule JSON 可能大。Go 侧捕获 stdout,设够大 buffer / 流式解析 JSON。
5. **prompt 膨胀** — reviewable 清单 + 规则 + diff 全塞 prompt 可能很长。对策:按 ocr 的"按 rule group 分批"(SKILL Step 4 "bounded batches grouped by shared rules and diff size"),或 Go 侧分 bundle 多次 RunOnce(进 Phase C 的打包能力)。
6. **定位精度** — Tier 2/3 的行定位由 host agent 做,有漂移风险(等同现状 StandardPrompt 水平)。要行级精度走 Phase B 端到端。已知取舍,非 bug。
7. **native bridge 一致性** — codex/claude 的内置 review 输出格式可能与 Tier 2/3 的结构化 schema 不一致。Phase C 统一 schema 时再收敛;v1 不强求 native 与 delegate 输出对齐。
8. **license** — ocr Apache-2.0,作外部依赖(不 vendored 源码)无冲突。

---

## 8. 结论

nightme `/review` 现有架构(native / delegate / 不支持 三 pattern,F-review.md §13)+ 用户的 ocr 委托模式补充,收敛为**三层分流**:

- **Tier 1 native**(codex/claude):不动,继续调内置 review——这两家自己的最强形态。
- **Tier 2 ocr delegate**(无 native + ocr 在):`ocr delegate preview/rule` 的 LLM-free 工程产出拼进 prompt,喂给该 agent——ocr 的文件选择 + 规则匹配 + 覆盖率约束,LLM 走现有 agent。
- **Tier 3 优化版 StandardPrompt**(无 native + ocr 不在):参考 ocr 做 Go 侧预算 + 覆盖率 + schema 增强,零外部依赖。

分流集中在 `agent.DelegateReview` helper,5 个 delegate bridge 各改一行;native / pty 零改动;ocr 缺失自动降级 Tier 3。ocr 始终是被调用的外部工具(类 git),不当 bridge、不进 `agent.Builtins`、不配 LLM、无双配置。

**最低风险、最高 ROI、最符合 bridge 定位 + 用户诉求的落地点:Tier 2/3 helper + 5 bridge 替换 + StandardPrompt 优化**。之后按需推进 Phase B(行级精度补丁)与 Phase C(schema 统一)。
