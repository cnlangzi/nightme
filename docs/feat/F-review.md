# F-review — 统一的 `/review` 命令

> **Status**: implemented
> **Scope**: `internal/command/review/`(slash command)+ `internal/agent/review*.go`(runner)
> **Related docs**: [`REVIEW.md`](../REVIEW.md)(完整设计:多 job 并发、ocr 路径、findings 解析、渲染)
> **Architecture**: `/review` 跑在 agent 的 **session 路径** 上 —— 每个 rule group 一个独立 session(独立 context window,与主 chat 隔离)。findings 以 user message 的形式注入回主 chat session。
> **Behavior**: `/review` 只看当前分支(含未 commit)vs 默认主分支 = PR 模式。
> **Motivation**: 每个 AI coding agent 都有自己的 code review 内置命令或 skill,nightme 没有统一入口。`/review` 收敛到 PR 模式一种行为,产出结构化 finding,后续 fix 由 chat agent 用原生 Edit 工具完成。

---

## 0. 命令面

```
/review                  # 用当前选中的 agent 跑,findings 注入回同一个 chat
/review --agent codex    # 用 codex(或任何已注册 agent)跑,findings 仍回当前 chat
/review -a codex         # --agent 的短写
/review -a codex -g      # 顺便把 codex 存成全局 review agent
/review -g --unset       # 删掉持久化的全局 review agent
/review -c               # 续跑本 chat 上一次 review 里没跑完的 group
/review -c -a codex      # 续跑,未完成的 group 交给 codex
/review --abort          # 停掉本 chat 里正在跑的 review
/review --timeout 20m    # 单个 group 的静默上限
```

参数由共享 lexer(`command.ParseCmdArgs`)解析:未知 flag 与多余的位置参数都会被拒。

**runner 解析四层**:

1. 本次调用的 `-a / --agent`
2. 全局 `review.agent` 配置项(`~/.nightme/config.yaml`)
3. chat session 当前选中的 agent
4. 都没有 → "no active agent" 错误

`-g / --global` 把 runner 持久化成 `review.agent`;`--unset` 再删掉。

**超时**:`--timeout <dur>` 优先,其次配置里的 `review.timeout`,最后是内置默认 15 分钟
(`agent.DefaultReviewGroupTimeout`)。超时只针对"单个 group 静默"—— 到达 agent 的 group 会被关闭、
标记为 timed out、并且仍然可续跑。review 本身没有墙钟截止时间:它跑到 chat session 的 context 被取消
(daemon 退出)或者用户 `--abort` 为止。

---

## 1. 执行模型

### 1.1 全异步 dispatcher

`Handle` 起一个 goroutine 做 review 的活,立刻返回 `Consumed=true`。dispatch worker 在微秒级释放,
chat session 的 readpump 不会被阻塞,多个 `/review` 可以并行。

goroutine 以 `cs.Context()` 为 parent —— chat session 的 context 被取消时,review 的 session 一起关掉,
goroutine 干净退出,不留孤儿。

### 1.2 session 是隔离边界

review 跑在**新开的 session** 里,各自独立的 context window。主 chat 的 token 预算不会被 review 的
推理污染(那可能烧掉几万 token)。review 的产出经 `cs.Emitter()` 回到 chat。

### 1.3 fix 是对话式的

review 结果出现在 chat 里之后,用户说"把 blocker 修掉",主 agent 用自己的原生 Edit 工具改。
`/review` 不自动落盘、不自动改代码。

---

## 2. Runner:两条路径

所有 bridge 的 `Review` 走同一个 dispatcher:

```go
// internal/agent/agent.go
Review(ctx context.Context, cfg StartConfig, opts ...RunOnceOption) (RunResult, error)

// internal/agent/review.go
func ReviewDispatch(ctx context.Context, s Starter, cfg StartConfig, opts ...RunOnceOption) (RunResult, error) {
    if OcrAvailable() {
        return ReviewWithOcr(ctx, s, cfg, opts...)
    }
    return ReviewWithPrompt(ctx, s, cfg, opts...)
}
```

`OcrAvailable()` 就是 `exec.LookPath("ocr")`。两条路径的差别只在"文件清单 + 规则匹配"从哪来,
其余(Go 侧预算 diff、覆盖率约束、输出 schema、多 job 并发)完全一样。

| 路径 | 入口 | 文件清单 / 规则来源 |
|---|---|---|
| ocr 路径 | `internal/agent/review_with_ocr.go::ReviewWithOcr` | `ocr delegate preview` + `ocr delegate rule` |
| builtin 路径 | `internal/agent/review.go::ReviewWithPrompt` | Go 侧 3 个 git 来源 + `isReviewablePath` 启发式 + 内置规则 |

bridge 侧不需要知道自己走哪条 —— 它们只实现 `Starter.Review`,里面 delegate 给 `agent.ReviewDispatch`。
`pty` 返回 `agent.ErrReviewNotSupported`,由 dispatcher 通过 `cs.Emitter().Send()` 回一句友好的
"agent X 不支持 /review",而不是静默失败。

agent 自带的 review 子命令(`claude -p code-review` / `codex review` / `cursor-agent -p /review-bugbot`)
**不走** `Starter.Review` 这条路 —— 用户直接在 chat 里发就行,nightme 透传给底层 agent。

详见 [`REVIEW.md`](../REVIEW.md) §2。

---

## 3. 多 job 并发

ocr 路径按 rule group 拆 N 个 job,builtin 路径合成 1–2 个 group。两条路径都走
`delegateReviewMultiJob`:每个 job 一个独立 ctx,sem 上限 `maxConcurrentReviewJobs`(4),
结果按组顺序合并。

每个 group 有自己的静默阈值(watchdog)与自己的 agent session。超时的 group 被标记为失败并保持可续跑。

---

## 4. 产出

### 4.1 findings schema

agent 按固定 schema 输出:

```
path / content / start_line / end_line / category / severity
```

解析在 `internal/agent/review_findings.go::ParseReviewFindings`(宽松解析:块形态 / 单行 / JSON 三种都收)。
schema 读不出来时 `internal/agent/review_repair.go::reemitReviewFindings` 会重发一次 schema 提示。

### 4.2 渲染

`internal/command/review/render.go` 负责三块:

| 内容 | 实现 |
|---|---|
| plan 行 / verdict 行 | `renderPlanLine` / `reviewVerdict` |
| digest(按 severity 分组的 markdown 表) | `reviewDigest` / `digestTables` / `digestFindings` / `sanitizeCell` |
| 收尾提示(thread 版) | `reviewVerdictHint` / `sendVerdictHint` |
| 注入主 agent 的那一轮 | `reviewPromptFrame` |

plan 行和 verdict 行会带 group 清单 —— group 名字带 markdown 元字符时按 code span 渲染
(`internal/agent/review_label.go::CodeSpan`)。digest 接在 verdict 行后面,走同一条消息,
每条 finding 一行,只列位置与分类。

---

## 5. 续跑与中止

续跑状态按 chat id 存在 factory 的内存里(`internal/command/review/record.go`),**不落盘** ——
daemon 重启就没了。

`-c / --continue` 续跑上一次 review 里没跑完的 group:

- 已经到达 agent 的 group → 重新打开那个 agent session,发一句 "continue"
- 从没到达 agent 的 group → 用存下来的 prompt 重跑一遍

合并结果与一次正常 review 的投递方式完全一致。

`--abort` 取消本 chat 里正在跑的 review(或续跑)的 context,group 的 agent session 随之关闭。
被停掉的 group 仍然可续跑。

---

## 6. 实现位置

| 概念 | 实现位置 |
|---|---|
| slash command | `internal/command/review/cmd.go` |
| 续跑状态 | `internal/command/review/record.go` |
| 中止 | `internal/command/review/abort.go` |
| 渲染 | `internal/command/review/render.go` |
| 超时解析 | `internal/command/review/timeout.go` |
| 统一 dispatcher | `internal/agent/review.go::ReviewDispatch` |
| ocr 路径 | `internal/agent/review_with_ocr.go::ReviewWithOcr` |
| builtin 路径 | `internal/agent/review.go::ReviewWithPrompt` |
| 多 job 编排 | `internal/agent/review_with_ocr.go::delegateReviewMultiJob` |
| 并发上限 | `internal/agent/aggregate_sink.go::maxConcurrentReviewJobs`(4) |
| group 静默阈值 | `internal/agent/review_timeout.go::DefaultReviewGroupTimeout`(15 分钟) |
| findings 解析 | `internal/agent/review_findings.go::ParseReviewFindings` |
| 续跑内核 | `internal/agent/review_continue.go::ContinueReviewJobs` |
| base 分支解析 | `internal/gitbase/gitbase.go::Resolve` |
| 配置项 | `review.agent` / `review.timeout`(`internal/config/config.go`) |

---

## 7. 不做的事

- ❌ **不自动应用 fix** —— findings 注入 chat,由主 agent 用原生 Edit 工具改
- ❌ **不切 AgentSession** —— `/review --agent` 换的是"谁来做 review",findings 仍注入回当前 chat
- ❌ **不把 ocr 当 bridge** —— ocr 是被调用的外部工具(类 git),不进 `agent.Builtins`
- ❌ **不用 agent 自带的 review 子命令** —— 那条路是用户在 chat 里直接发 slash command
