# F-review-ocr — 把 alibaba/open-code-review 融入 nightme `/review`

> **Status**: 已落地
> **Scope**: `internal/agent/review.go`(dispatcher)、`internal/agent/review_with_ocr.go`(ocr 路径)、`internal/agent/review_timeout.go`(per-group 静默阈值)
> **基于**: `internal/command/review/cmd.go`、`internal/agent/review.go`、`internal/bridge/{claudecode,codex,copilot,cursor,dsh,omp,opencode,pi}/starter.go`、`docs/REVIEW.md`、`configs/nightme.example.yaml` + `internal/config`(nightme 无 LLM 层) 与 alibaba/open-code-review 的 **Delegation Mode**(`open-code-review/cmd/opencodereview/delegate_cmd.go` + `skills/open-code-review-delegate/SKILL.md`)。

---

## 0. 一句话结论

**ocr 是被调用的外部工具,不是 bridge。** 所有 bridge 的 `Review` 走同一个 dispatcher:

```go
agent.ReviewDispatch(ctx, s, cfg, opts...)
```

dispatcher 只按一件事分流——`ocr` 在不在 PATH 上:

| 条件 | 路径 | 做什么 |
|---|---|---|
| `OcrAvailable()` 为 true | `ReviewWithOcr` | 调 `ocr delegate preview` / `ocr delegate rule` 拿文件清单 + 按文件匹配的规则,再按 rule group 拆多 job 并发 |
| `OcrAvailable()` 为 false | `ReviewWithPrompt` | Go 侧自己算 base / merge-base / diff / 文件清单,合成一个 builtin group |

ocr 不进 `agent.Builtins`、不配 LLM、无双配置。`ocr` 缺失时静默降级到 builtin 路径,用户不需要做任何配置。

---

## 1. 系统对比

| 维度 | nightme `/review` | ocr 端到端(`ocr review`) | ocr **委托模式**(`ocr delegate`) |
| --- | --- | --- | --- |
| **谁来做 review** | 编码 agent 读 prompt | ocr 专用 Go agent + LLM 循环 | **host agent**(claude / codex / pi / …) |
| **LLM 来源** | 现有 agent 的 LLM | ocr 自配 LLM endpoint | **现有 agent 的 LLM**(ocr 侧 LLM-free) |
| **文件选择** | prompt 让 agent 自己跑 diff | 工程层精确选择 | **工程层精确选择**(JSON 输出) |
| **规则匹配** | 烤进 prompt | 按文件特征模板匹配 | **按文件特征模板匹配**(JSON 输出) |
| **定位 / 反思** | agent 自由报 file:line(有漂移) | ocr 独立定位 / 反思模块 | host agent 自己定位(无 ocr 反思模块) |
| **是否需配 LLM** | 否 | 是 | **否**(委托模式不调 LLM) |
| **接入形态** | 内置 slash command | 端到端 CLI / skill | 被调用的外部工具(类 git) |

**关键发现**:ocr 官方的委托模式把 "deterministic engineering" 解耦成 `ocr delegate preview/rule` 两个 **LLM-free** 子命令(JSON 输出),host agent 用自己 LLM 消费——契合 nightme 纯 agent-delegated、无 LLM 层的现状。

---

## 2. 契合点

`docs/REVIEW.md` §2.5:"**不同的 reviewer,同一个 chat**"——`/review --agent <name>` 不切 AS,findings 全注入回当前 AS。

两条路径都不引入新 reviewer,而是**增强 reviewer 的输入**:`cmd.go` 的 goroutine、`internal/agent/review_timeout.go` 的 `DefaultReviewGroupTimeout`(15 分钟静默阈值)、session 路径(`internal/agent/run_session.go`)、双路分发(`as.SendBlocks` + `emitter.Send`)、对话式 fix——全部复用。改的只是 prompt 的构造方式。

---

## 2.5 定位判定:ocr 不是 bridge

### 为什么不能"让 ocr 直接复用现有 agent 的 LLM"(端到端路径)

nightme 是纯 agent-delegated(`configs/nightme.example.yaml` 只有 channel token,`internal/config` 无 LLM/Provider/Model 字段,bridge 只 spawn 成品 agent CLI)。所以不存在"我们 agent 的 LLM endpoint"可指给 ocr;claude/codex CLI 是成品 agent(带 system prompt + 工具 + guardrails),不是裸 LLM,当不了 ocr 的 `llm client`。**委托模式绕开这个死结**:ocr 根本不调 LLM,工程产出是 JSON,host agent 用自己的 LLM 消费。

### 委托模式拿不到什么

委托模式下定位 / 反思由 host agent 自己做(SKILL.md Step 4–6)。ocr 的专有定位 / 反思模块只在端到端 `ocr review` 里。委托模式拿到"文件选择 + 规则匹配 + 覆盖率硬约束"的确定性(治"漏文件 / 偷懒 / 规则噪声"),**拿不到** ocr 的行级定位 / 反思精度——那是后续可选的端到端路径。

---

## 3. 架构

### 3.1 分流决策

```text
Starter.Review(ctx, cfg, opts)
   │
   └─ agent.ReviewDispatch(ctx, s, cfg, opts)
         │
         ├─ OcrAvailable()   (exec.LookPath("ocr"))
         │     YES → ReviewWithOcr
         │            ├─ precomputeReviewWithOcr:ocr delegate preview / rule
         │            ├─ 按 rule group 拆 N 个 job
         │            └─ delegateReviewMultiJob:并发跑,各自独立 ctx
         │
         └─ NO  → ReviewWithPrompt
                  ├─ precomputeReviewWithBuiltin:Go 侧 base / merge-base / 3 条 diff / 文件枚举
                  ├─ 合成 1 个 builtin group(有代码文件时再加 1 个 simplify group)
                  └─ 同样走 delegateReviewMultiJob
```

**决策表**:

| Agent | Review 入口 | 归属 |
|---|---|---|
| claude / codex / dsh / pi / opencode / cursor / copilot / omp | `agent.ReviewDispatch` | ocr 在 → ocr 路径;不在 → builtin 路径 |
| acp(通用 ACP 桥) | `agent.ReviewDispatch` | 同上 |
| pty | `ErrReviewNotSupported` | 不支持 |

### 3.2 agent 自带的 review 子命令不走这条路径

claude 的 `-p code-review`、codex 的 `review`、cursor 的 `/review-bugbot` 是 agent CLI 自己的形态。`Starter.Review` 不为它们开特例——用户在 chat 里直接发 slash command,nightme 透传给底层 agent,走的是普通的 agent 调用路径。

### 3.3 ocr 委托路径

`ReviewWithOcr` 在 Go 侧通用预算之后跑两个 LLM-free 子命令:

1. `ocr delegate preview --format json [--from <base> --to HEAD]` → JSON:`mode` / `from`/`to`/`commit`/`merge_base` / `reviewable_files`(path/status/insertions/deletions)/ `excluded_files`(含 reason)/ `background`。
2. `ocr delegate rule --format json <reviewable paths...>` → JSON:按 rule content 分组的 `groups`(group_id/pattern/files/rule)。一次调用最多传 `ruleBatchSize`(50)个路径,避免 ARG_MAX 超限。
3. Go 侧按 ocr 给的 merge_base 预算每个文件 diff。
4. 按 group 拼 prompt → 并发跑每个 group。

**ocr 侧 LLM-free**(SKILL 明说 "delegation mode never calls an LLM"),不配 ocr LLM,无双配置。用户只需 `npm i -g @alibaba-group/open-code-review`。

### 3.4 builtin 路径

`ReviewWithPrompt` 不依赖 ocr:`precomputeReviewWithBuiltin` 用 3 个 git 来源 + `isReviewablePath` 启发式收集 reviewable,合成 1 个 `PatternBuiltin` 的 group;changeset 里有代码文件时再加 1 个 `PatternSimplify` group。**仍然 fan-out 出 builtin group**,跟 ocr 路径走同一条 `delegateReviewMultiJob` 路径,只是 file-list 精度低一档。

base 分支由 `internal/gitbase.Resolve` 判定,两条 precompute 路径共用。

### 3.5 两条路径共享的通用前置

1. **Go 侧预算 diff** —— `git diff <base>...HEAD` / `--staged` / 工作区(+ untracked),把已算好的 diff 喂进 prompt,agent 只负责判断。治"覆盖不全"。
2. **文件清单 + 排除** —— 按 generated/testdata/vendor 过滤,显式交给 agent。
3. **覆盖率硬约束** —— prompt 里强制"每个 reviewable 文件必须 reviewed 或 skipped-with-reason",输出 coverage 统计。
4. **输出 schema** —— `path/content/start_line/end_line/category/severity` 结构化字段,由 `internal/agent/review_findings.go` 解析。
5. **规则**(分流点):
   - ocr 路径:用 `ocr delegate rule` 的按文件匹配规则(更准,跟随 ocr 上游)。
   - builtin 路径:用 Go 侧内置规则。

1–4 项对两条路径都生效(纯 Go 工程);只有第 5 项规则匹配是差异点。这样 ocr 在和不在的区别收敛到"规则匹配"一项,且不绑死 ocr。

### 3.6 多 job 并发

ocr 路径按 rule group 拆 N 个 job,builtin 路径合成 1–2 个 group。两条路径都走 `delegateReviewMultiJob`:每个 job 一个独立 ctx,sem 上限 `maxConcurrentReviewJobs`(4),结果按组顺序合并。

### 3.7 实现位置

| 概念 | 实现位置 |
|---|---|
| 统一 dispatcher | `internal/agent/review.go::ReviewDispatch` |
| ocr Runner 入口 | `internal/agent/review_with_ocr.go::ReviewWithOcr` |
| builtin Runner 入口 | `internal/agent/review.go::ReviewWithPrompt` |
| ocr 路径 precompute | `internal/agent/review_with_ocr.go::precomputeReviewWithOcr` |
| builtin 路径 precompute | `internal/agent/review_with_ocr.go::precomputeReviewWithBuiltin` |
| ocr 可用性探测 | `internal/agent/review_with_ocr.go::OcrAvailable` |
| 多 job 并发编排 | `internal/agent/review_with_ocr.go::delegateReviewMultiJob` |
| 并发上限 | `internal/agent/aggregate_sink.go::maxConcurrentReviewJobs`(4) |
| per-group 静默阈值 | `internal/agent/review_timeout.go::DefaultReviewGroupTimeout`(15 分钟) |
| base 分支解析 | `internal/gitbase/gitbase.go::Resolve` |
| findings 解析 | `internal/agent/review_findings.go::ParseReviewFindings` |

---

## 4. 后续可选

### Phase B — ocr 端到端作为 opt-in review 执行器,非 bridge

委托模式拿不到 ocr 的行级定位 / 反思精度。当用户要这精度时,ocr 作 opt-in 执行器端到端跑 `ocr review --audience agent --format json`,走 `/review --engine ocr`(非 `--agent`,非 bridge),findings 回 chat。需 `ocr config provider` 配 LLM(双配置根源,文档说清)。

### Phase C — 统一输出 schema + 引擎可插拔

`Reviewer` 接口藏在 `Starter.Review` 后;统一 `ReviewResult` schema(ocr comment 字段 canonical);`/review --engine builtin|ocr|auto`。下游 fix / SARIF / gating 基于同一 schema。

---

## 5. 风险与待验证项

1. **ocr 安装依赖** — ocr 路径需 `ocr` 二进制(`npm i -g`),但**不需配 LLM**(委托 LLM-free)。builtin 路径无任何外部依赖。对策:`OcrAvailable()` 里 `exec.LookPath("ocr")`,缺失降级 builtin 路径。
2. **跨平台二进制** — ocr 经 npm + GitHub release,需验 Windows/macOS/Linux。ocr 路径只依赖 `ocr delegate`(纯 Git 工程,无 LLM),比端到端轻。
3. **超时** — per-group 静默阈值 15 分钟(`DefaultReviewGroupTimeout`);ocr 路径多跑 2 个 delegate 子命令(毫秒级),增量可忽略。
4. **输出截断** — 大 changeset 的 preview/rule JSON 可能大。Go 侧捕获 stdout,设够大 buffer / 流式解析 JSON。
5. **prompt 膨胀** — reviewable 清单 + 规则 + diff 全塞 prompt 可能很长。对策:按 ocr 的"按 rule group 分批"拆多 job(每个 job 独立 ctx)。
6. **定位精度** — 两条路径的行定位都由 host agent 做,有漂移风险。要行级精度走 Phase B 端到端。已知取舍,非 bug。
7. **license** — ocr Apache-2.0,作外部依赖(不 vendored 源码)无冲突。

---

## 6. 结论

nightme `/review` 收敛为**两条路径**:

- **ocr 路径**(`OcrAvailable()` 为 true):`ocr delegate preview/rule` 的 LLM-free 工程产出拼进 prompt,喂给该 agent——ocr 的文件选择 + 规则匹配 + 覆盖率约束,LLM 走现有 agent。
- **builtin 路径**(ocr 不在):Go 侧预算 + 覆盖率 + schema 增强,零外部依赖。

分流集中在 `agent.ReviewDispatch`;ocr 缺失自动降级。ocr 始终是被调用的外部工具(类 git),不当 bridge、不进 `agent.Builtins`、不配 LLM、无双配置。

**Phase B(行级精度)与 Phase C(schema 统一)是后续增量**,不阻塞当前形态。
