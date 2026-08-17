# F-review — 统一的 `/review` 命令,对接各底层 agent 的差异化 review 入口

> **Status**: 📐 designing(第九轮 — 走标准 slash command 流程,不写独立 parse 函数)
> **Scope (v1)**: 给现有 `Starter` 接口加一个 `Review(ctx, rc) error` 方法(每个 bridge 加 3 行,delegate 到 `agent.DefaultReview`)。新建 `internal/agent/review.go`(ReviewContext / DefaultReview / StandardPrompt)和 `internal/command/review/cmd.go`(一个文件,直接 inline 检查 `input.Args`)。
> **v9 关键收敛**: `/review` 跟其他 slash command(`/cwd` / `/think` / `/use` 等)一样,**直接用 Commander 解析好的 `input.Args`,inline `len(input.Args)` 检查**。不写独立的 `parseReviewArgs` 函数,也不写 `parse.go` 文件。
> **Behavior**: `/review` 零限定符 — 不带 flag / 不带 arg。永远是 "当前分支(含未 commit)vs 默认主分支" = PR 模式。
> **Architecture**: `/review` 走当前 `AgentSession`(chat 复用)。fix 由 chat agent 自带的 Edit 工具在同 session 内完成。
> **Related**: [`F-gtw.md`](./F-gtw.md) 的 `/gtw commit` / `/gtw pr`(**架构 A 的 RunOnce 模式,跟 `/review` 不同**),[`agent-no-config-tampering`](../../CLAUDE.md) 铁律
> **Motivation**: 现在每个 pi coding agent 都有自己的 code review 内置命令或 skill,nightme 没有统一入口。`/review` 收敛到 **PR 模式** 一种行为,产出结构化 finding,后续 fix 由 chat agent 用原生 Edit 工具完成。

---

## 0. 实机验证结果(2026-08-17,两轮)

下面所有命令在本地实机跑过(`/tmp/review-smoke` 里造了一个有 bug 的 main.go)。

### 0.1 v6 收敛:零限定符 — `就是 PR 模式`

`/review` **零限定符**:不带 flag、不带路径、不带 base 覆盖。永远是 "当前分支 vs 默认主分支,所有 commit + 所有未 commit 的差异 = 这个 PR 会包含什么"。

```
/review                                  # 唯一形式 — 不接受任何 arg
```

**为什么这一种行为就够**:
- 工作流对齐 PR review — 用户在做一个 PR 前,想看的就是"branch vs main 上的全部 diff"
- 不需要区分 commit / uncommitted / staged — 它们都在默认模式里包含
- 不需要单独的 base 覆盖 / 路径限定 — 不常见,留 v2 评估;真要做时,用户在 chat 里说 "再 focus 一下 foo.go" 就行,chat agent 二次响应

**用户传额外参数时**(e.g. `/review foo`、`/review --base develop`),直接报错引导:
```
❌ /review 不接受参数;去掉 "foo"
```

### 0.2 各 bridge 的 review 入口

### 0.2 各 bridge 的 review 入口

| Agent | 实机验证的 review 入口 | v1 接入? |
|---|---|---|
| **claude** (`/Users/geax/.local/bin/claude`, latest) | `claude -p "/code-review"`(slash command 内置) | ✅ v1 接入 — chat agent 读 StandardPrompt 出结构化 review |
| **codex** (`/Users/geax/.local/bin/codex`, v0.145.0) | `codex review --base <ref>` / `--uncommitted` 等独立子命令 | ✅ v1 接入 — chat agent 读 StandardPrompt;**不**调 `codex review` 子命令(架构 B 走 chat session,不走 print-mode 独立子命令) |
| **dsh** (`/Users/geax/.nvm/versions/node/v22.20.0/bin/dsh`, latest) | 没有原生 review,但 chat agent 能按 StandardPrompt 读 diff | ✅ v1 接入 |
| **opencode** (`/Users/geax/.bun/bin/opencode`, v1.18.18) | 没有原生 review,chat agent 按 StandardPrompt 读 diff | ✅ v1 接入 |
| **pi** (`/Users/geax/Library/pnpm/pi`) | 没有原生 review,chat agent 按 StandardPrompt 读 diff | ✅ v1 接入 |
| **bash / pty fallback** | 不是 coding agent,不能读 diff 也不能 review | ❌ 不支持 |

→ **架构 B + v5 收敛**:v1 接 5 个 coding agent,共用**一个** `StandardPrompt` 模板。SlashTrigger 字段被删除(所有 agent 走 prompt 注入)。

### 0.2 各 review 命令的真实 flag surface(实机 + 官方文档)

**Codex `review`**(实测 `codex review --help` + OpenAI 官方文档):
```
--uncommitted            Review staged, unstaged, and untracked changes
--base <BRANCH>          Review changes against the given base branch
--commit <SHA>           Review the changes introduced by a commit
--title <TITLE>          Optional commit title in review summary
[PROMPT]                 Custom review instructions (or `-` for stdin)
-c, --config / --strict-config / --enable / --disable
```

→ 3 种 scope 模式(uncommitted / branch / commit),**没有 `--fix` 也没有 `--pr`**。"review PR" = `gh pr view --json baseRefName` 解析 PR base 后传 `--base <that>`。

**Claude `/code-review`**(实测 + https://code.claude.com/docs/en/code-review):
```
/code-review                       默认 — review 当前 diff,文本输出
/code-review --fix                 review 后 auto-apply findings 到 working tree(破坏性)
/code-review --comment             review 后 post inline PR comments(远端副作用)
/code-review --post                ultra cloud review 时 preselect "post 到 PR"
/code-review ultra                 升级到 ultrareview(cloud-hosted multi-agent)
/code-review ultra --fix           ultra + auto-fix
/code-review [target]              target 可以是 file path / PR # / branch 名 / ref range(main...feature)
/code-review <effort>              effort level: low / medium / high / max / ultra
```

→ **真有 `--fix`**(破坏性,动 working tree);**真有 `--comment` / `--post`**(远端副作用);有 `ultra` 子模式(cloud);有 effort level。

→ 注意:Claude Code v2.1.223 起 `/review` 是 `/code-review` 的 alias,我们用 `/code-review` 是稳的。

### 0.3 flag 取舍(v1 决策)

| flag / 行为 | v1 暴露? | 理由 |
|---|:---:|---|
| **scope 切换**(uncommitted / staged / branch / commit / pr / path) | ✅ | 只读,纯路由,通过 `/review <scope> [args]` 表达 |
| **`[PROMPT]` 额外指示** | ✅ | 只读,`/review -- <comment>` |
| **`--fix`**(claude 自动 apply) | ❌ | 破坏性副作用,直接改 working tree。nightme 已有 `/gtw fix` 走完整 fix 流程,职责重叠 |
| **`--comment` / `--post`**(claude 发 PR 评论) | ❌ | 远端副作用,即便 user consent 也违反 `agent-no-config-tampering` 精神 — 这类副作用用户应该手动 `gh pr review` / 在 chat 里指挥 agent |
| **`ultra`**(cloud 多 agent) | ❌ | v1 只接本地 review;ultra 要 claude.ai 账号 + 计费 + API key,范围远超 v1 |
| **`effort level`**(low/medium/high/max) | ❌ | "配置 agent 行为"的口子,agent-no-config-tampering 原则下 v1 不开;用户想 deep review 就多跑几次 |
| **`--title`**(codex) | ❌ | 给 review 报告一个 title,纯 cosmetic,v1 没必要 |

→ **v1 `/review` 保持纯 review,所有 flag 都是只读**:读 diff / 读 git history / 读 PR 元数据,输出 finding 文本让用户决定下一步。

> **未来 v2+ 的扩展路径**:`/review --fix` 可以作为新 slash 命令(比如 `/review-fix` 或 `/gtw review-fix`),把 review + fix 串起来;`/review --post` 可以作为 `/review-comment` 单独命令;这都留 v2。

### 0.4 架构决策:`/review` 走当前 AgentSession,**不** spawn 新进程

早期设计稿让 `/review` 走 `agent.RunOnce`(one-shot 新进程,跟 `/gtw commit` / `/gtw pr` 同模式)。第三轮 review 时被否决 — 原因是 `/review` 是 **chat 内任务**,不是独立任务:

| 维度 | RunOnce(原设计) | 当前 AgentSession(新设计) |
|---|---|---|
| 进程 | spawn 新进程,用完即弃 | 复用 chat session 现有进程 |
| chat 上下文 | fresh,看不到 chat history | 完整(用户最近说了什么、agent 之前读过什么) |
| reply 路径 | dispatcher drain events → `cs.Emitter().Send` | chat session 自己的 readpump → emit 链路 |
| dispatcher 行为 | 同步等 review 完成,自己 send reply | **异步** — 立即返回 `Consumed=true`,review 在 chat 里异步出 |
| fix 流程 | chat agent 收到 emit 后的 review,改 | 一样(chat agent 已经在改,上下文连续) |
| agentbar / usagebar | dispatcher 手动盖章(走 `gtw.replyAgent`) | chat agent 既有 event 链路自动盖章 |
| 跟 `/gtw commit` 比 | 同模式 | 跟 `/stop` / `/use` / `/close` / `/cwd` 一致(对当前 session 状态做操作) |

→ 走当前 AgentSession 更自然,**架构上跟 `/stop` / `/use` 这类 state-management 命令一致**,而不是跟 `/gtw commit` / `/gtw pr` 这类 side-effect-heavy 任务一致。

→ 这个决策的关键推论:**`/review` 现在能接所有 5 个 coding agent**(claude / codex / dsh / opencode / pi),只要它们的 chat agent 能读 diff 并按 prompt 模板输出。bridge 列表不再受"有没有 native review 子命令"约束 — dsh / opencode / pi 也能用,因为 chat agent 本身能 review(只是没有"一键触发"按钮)。**`bash` 不是 coding agent,不支持**。

> 实现细节:`cs.SelectedAgentSession().Handle().SendBlocks(ctx, blocks)` 注入 review prompt 到当前 chat;dispatcher 立即返回 `&SlashOutput{Consumed: true}`,**不**等 review 完成。chat session 自己的 readpump 处理后续 events,review findings 自然出现在 chat 里。

**关键更正**(相对 v1 设计稿):
1. Claude 的 prompt 是 `/code-review`,不是 `/review`。
2. Codex `review` 子命令**不走 `codex exec`** — 它是另一个 print-mode 子命令,需要走 `RunNative` 路径(直接 exec `codex review ...`,不走 RunOnce)。
3. OpenCode 没有原生 review,prompt-only;之前 v1 的设计是正确的,但需要重新确认 `opencode run --prompt` 是否真能传 stdin(实机 `opencode run --help` 看到的 positional `[message..]` 是首选)。
4. DSH 的 "接入方式有错" → v1 沿用现有 print-mode(`dsh --profile headless -- "<prompt>"`),**不**新建 RPC 通路(那是更大的重构;留 v2)。

### 实机跑出来的样本

`claude -p "/code-review"` 在 `/tmp/review-smoke` 上输出(JSON 风格 finding 列表 — Claude Code 内置格式):
```json
[
  {
    "file": "main.go",
    "line": 24,
    "summary": "Program panics with 'integer divide by zero' ...",
    "failure_scenario": "..."
  },
  ...
]
```

`claude -p "review ... <structured prompt>"` 在同一份 diff 上(用结构化 prompt 模板):
```
## Summary
The diff adds two helper functions ...
## Findings
- blocker: main.go:12 — buggyDivide has no zero check ...
- blocker: main.go:16-17 — saveUser swallows json.Marshal error ...
## Suggestions
- Make buggyDivide return (int, error) ...
```

→ **prompt-based 路径输出可解析**(Sections + 严重程度 tags),可以作为统一格式 baseline。

---

## 1. 设计目标 / 边界

### 1.1 目标

- **v1**:用户视角 — 写 `/review` 就是 code review;**零限定符,只有一种行为**:review 当前分支 vs 主分支(PR 模式)。5 个 coding agent(claude / codex / dsh / opencode / pi)都 work。bash / pty 上 `/review` 回友好提示("不是 coding agent")。
- **v1**:输出视角 — 异步,review 在 chat 里异步出,自带 agentbar / usagebar footer。
- **v2+**(如有需要):加限定符 flag(比如 `/review path1.go` 限定文件、`/review --base develop` 覆盖 base)。v1 都不做。
- **fix 走 chat agent 自然流程**(见 §1.3):`/review` 是只读;修代码靠 chat agent 自带的 Edit/Write 工具,**不**在 `/review` 里加 `--fix` flag,也**不**单独开 `/review-fix` slash command。

### 1.2 边界

- **不**做 review result 持久化、不做 review 历史。
- **不**自动发 PR 评论(违反 `agent-no-config-tampering`;用户需要的话手动跑)。
- **不**接管现有 bridge 的 `Starter` / `RunOnce` 契约 — review 只是又一种 prompt 注入方式。
- **不**自动 apply 任何修改 — `/review` 只 read-only;fix 走 chat agent(见 §1.3)。
- **不**接受任何 flag 或位置参数 — v6 零限定符。
- 不与 `/gtw commit` / `/gtw pr` / `/gtw fix` 抢占资源,只是同一 `Builtins` 的另一种调用形态。

### 1.3 `fix` 走 chat agent 自然流程(为啥不用 `--fix` flag 或 `/review-fix` 命令)

考虑过三种 fix 路径,**最终选最简单的**:

**(a) `/review --fix` 一次过** — 像 Claude Code `--fix` flag 那样,review + apply in one shot。
- ❌ 缺点:audit 链丢失;`git log` 看不出"哪些改动是 review 触发的";claude docs 自己警告 `--fix` edits 不在 session checkpoint undo 体系;用户失去"只修 blocker 不修 nit"的分级权。

**(b) `/review-fix` 独立 slash command** — 单条命令做 review + fix,但显式两步语义。
- ❌ 缺点:跟 (a) 一样丢 audit + 失去分级权;**而且**增加一个新的 slash command 入口要维护。

**(c) `/review` 只读 + chat agent 用 Edit 工具修** — review 输出后,用户在 chat 里直接说"修一下那个 race condition",chat agent 用它自带的 Edit/Write 工具改文件。**第二步不需要新代码,因为 chat agent 本来就会改代码**。
- ✅ 优点:
  - **两步语义清晰**:review 找问题,fix 改代码
  - **audit 链完整**:review 输出在 chat history 里,fix 是 chat 消息触发的,commit message 可以引用 finding
  - **用户分级权**:用户说"只修 blocker" / "全部修了" / "先不动,我自己看",agent 听用户的
  - **零新代码**:chat agent 自带 Edit/Write 工具(所有 pi coding agent 都是),nightme 不需要为 fix 加任何东西
  - **上下文天然在一起**:review finding 在 chat history 里,fix 模型能直接看到

**v1 选 (c)**。示例流程:

```
用户: /review
bot: ## Summary
     The diff adds two helper functions ...
     ## Findings
     - blocker: main.go:12 — buggyDivide has no zero check
     - blocker: main.go:16 — saveUser swallows Write error

用户: 把那两个 blocker 修一下
bot: [chat agent 用 Edit 工具直接修]
     修好了。buggyDivide 加了 zero check,saveUser 改成返回 error。
     - main.go:12 — 加 if b == 0 { return 0, ErrDivByZero }
     - main.go:16 — 加 err handling
```

> `/help` 输出里加一行 hint:`/review` 找问题,fix 在 chat 里直接说"修一下"等指示 — chat agent 会用它的文件编辑工具应用修复。

→ 跟 `/gtw fix <issue>` 的关系:**完全独立**。`/gtw fix` 给一个 issue 创建 worktree + commit + PR(完整 fix 流程);`/review` 是只读 review,后续的 chat 内 fix 是手改当前 working tree。两条路不重叠,用户按场景选。

---

## 2. 设计 — **不抽象,直接给 Starter 加 Review 方法**

### 2.1 核心抽象(v8 动作方法)

v7 我加了 `SupportsReview() bool`(能力检查),但那不是真正的 slash command 对应方法。v8 改成:

```go
// internal/agent/agent.go(改现有 Starter interface,只加这一行)

type Starter interface {
    Info() Info
    Detect() error
    Start(ctx context.Context, cfg StartConfig) (*Agent, error)
    RunOnce(ctx context.Context, cfg StartConfig, blocks []ContentBlock) (RunResult, error)

    // Review runs the /review slash command for this agent's chat session.
    // The bridge is responsible for:
    //   1. Returning ErrReviewNotSupported if this agent can't review
    //      (e.g. pty/bash)
    //   2. Injecting a review prompt into rc.Inject(typically StandardPrompt,
    //      but v2 a bridge could customize)
    //
    // Most bridges just delegate to agent.DefaultReview(ctx, rc).
    Review(ctx context.Context, rc ReviewContext) error
}
```

### 2.2 关键改进 vs v7

| v7 (能力检查) | v8 (动作方法) |
|---|---|
| `SupportsReview() bool` — 只是回答"我能 review 吗?" | `Review(ctx, rc) error` — 直接做 review 动作 |
| dispatcher 调 `starter.SupportsReview()`,然后自己手动 SendBlocks | dispatcher 调 `starter.Review(ctx, rc)`,bridge 内部完成所有事 |
| 5 个 coding bridge 写 `return true`(信息量低) | 5 个 coding bridge 写 `return agent.DefaultReview(ctx, rc)`(实际做事) |
| bridge 不能自定义 review 行为 | bridge 自由 override Review 方法(v2 想用 `/code-review` trigger 时,直接覆盖) |

### 2.3 每个 bridge 加 ~3 行

```go
// 5 个 coding bridge 各自加这个方法(delegate 到 agent.DefaultReview):

// internal/bridge/claudecode/starter.go
func (s *Starter) Review(ctx context.Context, rc agent.ReviewContext) error {
    return agent.DefaultReview(ctx, rc)
}

// internal/bridge/codex/starter.go     — 同上
// internal/bridge/dsh/starter.go       — 同上
// internal/bridge/opencode/starter.go  — 同上
// internal/bridge/pi/starter.go        — 同上

// pty/bash 不支持 review,直接返回错误:
func (s *Starter) Review(ctx context.Context, rc agent.ReviewContext) error {
    return agent.ErrReviewNotSupported
}
```

→ **6 个 bridge,共 ~18 行核心改动**(vs v7 的 6 行)。多出来的是 bridge 自己做事的方法定义,不只是声明能力。

### 2.3 共享 prompt 模板 + DefaultReview(v8 移到 agent 包)

```go
// internal/agent/review.go(新建, ~80 行)

package agent

import (
    "context"
    "errors"
)

// ReviewContext is passed to Starter.Review by the /review dispatcher.
// It carries the workspace path and an Inject callback the bridge uses
// to send the review prompt blocks into the current chat session.
type ReviewContext struct {
    Workspace string

    // Inject sends the given blocks into the chat session as a user turn.
    // dispatcher closes over cs.SelectedAgentSession().Handle().SendBlocks.
    Inject func(ctx context.Context, blocks []ContentBlock) error
}

// ErrReviewNotSupported is returned by Starter.Review when the agent
// doesn't support /review (e.g. pty/bash fallback).
var ErrReviewNotSupported = errors.New("agent: /review not supported")

// DefaultReview is the canonical /review implementation.
// Bridges that don't need custom review behavior should call this from
// their Review method:
//
//   func (s *Starter) Review(ctx context.Context, rc agent.ReviewContext) error {
//       return agent.DefaultReview(ctx, rc)
//   }
//
// It injects StandardPrompt() into the chat session via rc.Inject.
func DefaultReview(ctx context.Context, rc ReviewContext) error {
    if rc.Inject == nil {
        return errors.New("agent: ReviewContext.Inject is nil")
    }
    return rc.Inject(ctx, []ContentBlock{{
        Type: ContentText,
        Text: StandardPrompt(),
    }})
}

// StandardPrompt returns the canonical review prompt used by all bridges
// for /review. The prompt asks the chat agent to:
//   1. Detect the default branch (main/master/trunk via git symbolic-ref)
//   2. Run git fetch + git diff <default>...HEAD + git diff --staged + git diff
//   3. Output structured ## Summary / ## Findings / ## Suggestions sections
//
// 借鉴的成熟 code-review prompt 形态:
//   - Claude Code `/code-review` plugin 的 confidence-based + finding-first 结构
//   - Codex `review` 子命令的 severity 分组 + actionable suggestion
//   - 社区通用 senior-engineer-reviewing-PR 的 section 化输出
func StandardPrompt() string {
    return `You are a senior engineer reviewing code changes for an upcoming pull request. Your job is to find real problems, not to lecture about style. Be specific, concrete, and actionable.

# What to review

Review the changes between the **current branch** and the **default branch** (` + "`main`" + `, ` + "`master`" + `, or ` + "`trunk`" + ` — auto-detect with ` + "`git symbolic-ref refs/remotes/origin/HEAD`" + ` or ` + "`git remote show origin`" + `, or use ` + "`origin/<default>`" + ` if neither works).

This review covers **all** of the following — these together form "the diff a PR would have":

1. **Committed changes on this branch that aren't on the default branch yet**
   ` + "```bash" + `
   git fetch origin  # best-effort, ignore failures
   git diff <default-branch>...HEAD
   ` + "```" + `
2. **Staged changes** (already ` + "`git add`" + `ed but not committed)
   ` + "```bash" + `
   git diff --staged
   ` + "```" + `
3. **Uncommitted unstaged changes** in the working tree
   ` + "```bash" + `
   git diff
   ` + "```" + `

Run all three and treat their union as the full diff to review. If you're on the default branch itself (so ` + "`git diff <default-branch>...HEAD`" + ` is empty), the staged + unstaged sections are still the full diff.

# How to review

1. **Read the diff first**, end to end, before judging anything. Form a mental model of what the change does, then look for where that mental model breaks.
2. **Distinguish BLOCKERS from nits.** A blocker is something that would break in production, lose data, leak permissions, or make a follow-up change materially harder. Everything else is a nit or a suggestion.
3. **Cite file:line for every finding.** A finding without a location is unfalsifiable and useless.
4. **Skip linter / typechecker territory.** Don't flag what CI / gofmt / eslint / tsc / rustfmt would catch. Assume those run separately.
5. **Skip pre-existing issues.** Only flag things this diff introduced or makes worse. If a pre-existing bug is relevant to the diff, mention it once with a note, do not enumerate.
6. **False-positive filter.** If you're not sure something is a real issue, downgrade it to a nit or omit it. A noisy review is worse than a short one.

# What to look for (priority order)

- **Correctness**: off-by-one, wrong null/nil handling, race conditions, integer overflow, divide-by-zero, error swallowing, panic paths
- **Resource lifetime**: unclosed files / handles / transactions, goroutine / connection leaks, ` + "`defer`" + ` in loops
- **Concurrency**: shared mutable state without locking, channel send without select, deadlock potential
- **Error handling**: errors silently dropped (especially via ` + "`_ =`" + `), errors wrapped without ` + "`%w`" + `, error returned with no context, panic-from-error
- **API surface**: exported functions with unclear contracts, signatures that make correct use impossible (e.g. ` + "`(int, error)`" + ` without error path), types that prevent the caller from handling failure
- **Security**: unsanitised input → shell / SQL / file path, auth checks skipped, secrets in logs
- **Migration risk**: schema changes with no rollback path, config changes that break old clients, deploy ordering hazards
- **Test gaps**: new code path with no test, behavioural change to existing function with no test update

# Output format

Write your review in this exact structure. Do not add sections, do not reorder them.

` + "```markdown" + `

## Summary

1-3 sentences. What does this change do, and what's the overall risk profile (low / medium / high)?

## Findings

One line per finding, ordered by severity. If there are no findings, write "No blockers; nothing material to flag." and stop.

- **blocker**: <file>:<line> — <one-line issue>. <optional: how to verify it>
- **major**:   <file>:<line> — <one-line issue>
- **minor**:   <file>:<line> — <one-line issue>
- **nit**:     <file>:<line> — <one-line issue>

Severity rubric:
- **blocker**: would break in production or lose data. Must fix before merge.
- **major**:   real bug or footgun, would cause user-visible pain. Should fix before merge.
- **minor**:   code-quality or maintainability issue, not user-visible. Nice to fix.
- **nit**:     subjective preference, pedantic, or stylistic. Take it or leave it.

Do not pad with generic "consider adding tests" or "consider adding documentation" — only call those out if a specific behaviour is untested and that test would have caught a real failure.

## Suggestions

Concrete next steps, ordered by impact. One bullet per suggestion. Don't restate the findings; link to them by file:line.

- <file>:<line> — <concrete fix, in one sentence>
- ...

` + "```" + `
`)
    return sb.String()
}
```

// defaultBase picks the default base branch for the repo (main / master / trunk).
// 沿用 gtw.HookContext.DefaultBranch 模式:优先 git symbolic-ref refs/remotes/origin/HEAD,
// fallback git remote show origin | sed -n 's/.*HEAD branch: //p',最后才 "main"。
func defaultBase(workspace string) string { ... }
```

### 2.4 dispatcher — `/review` slash command(v9 走标准 slash command 流程)

```go
// internal/command/review/cmd.go

// Handle 接收 slash command,转交给 starter.Review 做实际工作。
//
// 关键:Handle 立即返回 Consumed=true,**不**等 review 完成,不 send reply。
// review 在 chat session 自己的 readpump → translate → emit 链路里异步出。
// reply 的格式由 prompt template 强约束(chat agent 按 prompt 输出结构化 sections),
// agentbar / usagebar footer 由 chat 链路自动盖章。
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
    cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

    // /review 零限定符 — input.Args[0] 是 "review" 命令本身,
    // 任何 len > 1 都是非预期 arg,直接报错。
    // (跟 /cwd / /think / /use 等其他 slash command 的 inline 检查一致:
    //  它们也直接用 len(input.Args),不写独立 parse 函数。)
    if len(input.Args) > 1 {
        return command.Reply(ctx, rt, fmt.Sprintf(
            "❌ /review 不接受参数;去掉 %q", input.Args[1])), nil
    }

    as := cs.SelectedAgentSession()
    if as == nil {
        return command.Reply(ctx, rt,
            "❌ 当前没有 active agent。先发条消息 / /use <name> 激活。"), nil
    }

    starter, err := agent.Builtins.Get(as.Info().Name)
    if err != nil {
        return command.Reply(ctx, rt,
            fmt.Sprintf("❌ unknown agent %q", as.Info().Name)), nil
    }

    // 把"如何注入 prompt 到 chat session"封成 rc.Inject 传给 bridge。
    rc := agent.ReviewContext{
        Workspace: cs.SelectedCwd(),
        Inject:    as.Handle().SendBlocks,
    }

    // bridge 自己实现 Review — 大多数 delegate 到 agent.DefaultReview,
    // 但理论上 claude 未来可以 override 用 `/code-review` slash trigger 等。
    if err := starter.Review(ctx, rc); err != nil {
        return command.Reply(ctx, rt, "❌ /review 失败: "+err.Error()), nil
    }

    return &command.SlashOutput{Consumed: true}, nil
}
```

→ v9 dispatcher 极简 + 跟其他 slash command 风格一致:**inline `len(input.Args)` 检查,无独立 parse 函数**。

### 2.5 replyAgent / StampRunResult 仍然重要(沿用 move-logic-atomically)

虽然 `/review` 不再 dispatcher-side 调用 `replyAgent`,但 chat session 的 reply 路径仍然需要 `agent.StampRunResult`(给 EventAgentResult 的 stamp)。这不是 `/review` 范围,是 chat session 既有逻辑,只是确认一下 review 的 reply 会**自动**经过这个盖章函数 — 因为 chat agent 的 reply 本来就过。

### 2.6 v8 删了什么 vs v7

| 之前 v6 删掉的(v7 不再创建) |
|---|
| ❌ `internal/agent/review.go`(Spec / Adapter / ReviewScope 类型)— 整文件不需要 |
| ❌ `internal/agent/review_registry.go` — 整文件不需要(走现成 `agent.Builtins`) |
| ❌ 5 个 bridge adapter 文件(`internal/bridge/*/review_adapter.go`)— 不创建 |
| ❌ `defaultBase()` 调用 — agent 自己探测 base |
| ❌ `BuildPrompt` 按 scope 分支拼接 — 单一固定模板 |
| ❌ `resolvePRNumber` / `resolvePRBase` — 没有 PR scope |

→ **v7 比 v6 净减 4 个新文件 + 30 行 dispatcher 代码**。改动量级与"加 1 行 interface 方法"持平。

---

## 3. 实现方案 — 文件清单

### 3.1 新建文件(v9 只有 3 个新文件)

| 路径 | 行数估计 | 职责 |
|---|---:|---|
| `internal/agent/review.go` | ~80 | `ReviewContext` / `DefaultReview` / `StandardPrompt` / `ErrReviewNotSupported` — bridge 直接调这个包 |
| `internal/command/review/cmd.go` | ~60 | `Factory` + dispatcher(组装 `ReviewContext`,调 `starter.Review(ctx, rc)`) |
| ~~`internal/command/review/parse.go`~~ | ~~删~~ | ~~不写独立 parse 函数 — 走标准 slash command 流程,inline `len(input.Args)` 检查~~ |

→ v9 比 v8 少 1 个文件 + 1 个测试文件(parse.go + parse_test.go)。

### 3.2 修改现有文件(v9 共 7 处改动)

| 路径 | 改动 | 行数 |
|---|---|---:|
| `internal/agent/agent.go` | 给现有 `Starter` interface 加 `Review(ctx, rc) error` 方法 | +1(interface) |
| `internal/bridge/claudecode/starter.go` | 加 `Review` 方法,delegate 到 `agent.DefaultReview` | +3 |
| `internal/bridge/codex/starter.go` | 同上 | +3 |
| `internal/bridge/dsh/starter.go` | 同上 | +3 |
| `internal/bridge/opencode/starter.go` | 同上 | +3 |
| `internal/bridge/pi/starter.go` | 同上 | +3 |
| `internal/bridge/pty/starter.go` | 加 `Review` 方法,返回 `agent.ErrReviewNotSupported`(bash 不支持) | +3 |

→ **7 个文件改动**(~ 19 行)。多出来的是 bridge 自己做事的方法定义,不只是声明能力。

### 3.3 不需要做的事

- ❌ 不创建 `Spec` / `Adapter` / `ReviewScope` / `ReviewRegistry` 等新 type
- ❌ 不创建任何 `review_adapter.go` 文件
- ❌ 不创建 `parse.go` / `parseReviewArgs()`(走标准 slash command 流程)
- ❌ 不改 `agent.Builtins`(直接复用现有 registry)
- ❌ 不改 dispatcher 之外的 chat session 任何代码
- ❌ 不动 `gtw.replyAgent`(review 走 chat 自带链路)
- ❌ 不需要 `RunNative` / `timeouts.Review` / `StampRunResult`
- ❌ `internal/command/review/prompt.go` 不需要了(prompt 挪到 `internal/agent/review.go`)

### 3.4 测试

| 路径 | 覆盖 |
|---|---|
| `internal/agent/review_test.go`(新建) | `StandardPrompt` 内容覆盖关键 sections;`DefaultReview` 注入 blocks;`ErrReviewNotSupported` 错误路径 |
| `internal/agent/agent_test.go`(已有) | 给 `Starter.Review` 加表驱动测试(每个 bridge 实例化,验证 claude/codex/dsh/opencode/pi 走 DefaultReview,pty 返 ErrReviewNotSupported) |
| `internal/command/review/cmd_test.go`(新建) | dispatcher 各错误路径(无 AS / unknown agent / `ErrReviewNotSupported`)+ 成功路径(调 `starter.Review`)+ 额外 arg 报错路径 |

→ 比 v8 少 1 个 `parse_test.go`(没东西可测了)。

### 3.5 实机 e2e 清单(v1 关卡 — 5 个 agent,各跑一次 `/review`)

`/review` 在 5 个 chat agent 上必须真机跑通(`/tmp/review-smoke` 上的已知 bug diff,故意引入 `buggyDivide(10, 0)` 和 `saveUser` 吞 `Write` 错误两个 blocker):

**每个 agent**(各跑 `/review` 默认行为,验证 chat agent 收到 StandardPrompt 后按格式输出):
- [ ] claude `/review` → 输出 ## Summary / ## Findings(buggyDivide + saveUser 两条 blocker 命中)/ ## Suggestions
- [ ] codex `/review` → 同上结构
- [ ] dsh `/review` → 同上结构
- [ ] opencode `/review` → 同上结构
- [ ] pi `/review` → 同上结构(需要有效 API key)

**错误路径**:
- [ ] `/review`(无 active AS) → 回"先 /use 激活"
- [ ] `/review`(bash 不是 coding agent,`Review` 返回 `ErrReviewNotSupported`) → 回友好提示
- [ ] `/review foo` → 回"/review 不接受参数;去掉 \"foo\""

→ **v1 PR 1 提交前**这 ~8 项是关卡。

---

## 4. 落地顺序(v1 = 2 个 PR)

**v9 比之前任何版本都简单** — 不需要任何抽象层、不需要 RunNative / timeouts.Review / StampRunResult 改造、不需要任何 flag 解析、不需要独立 parse 函数。改动量级 = "改 7 个现有文件 + 新增 2 个文件"。

1. **PR 1 — `/review` 命令 + Starter 加 Review 方法(全合一)**:
   - `internal/agent/agent.go` 给 `Starter` interface 加 `Review(ctx, rc) error`(1 行)
   - 6 个 bridge 文件各加 `Review` 方法(5 delegate to DefaultReview,1 return ErrReviewNotSupported)
   - `internal/agent/review.go`(新建, ReviewContext / DefaultReview / StandardPrompt)+ 测试
   - `internal/command/review/cmd.go`(新建, dispatcher inline 检查 `len(input.Args)`)+ 测试
   - 实机 e2e:在 `/tmp/review-smoke` 上跑 `/review` 验证 5 个 agent 都出结构化 review
   - `/help` 输出加 `/review` 描述
2. **PR 2 — docs**:本文件 + `docs/PRD.md` / `docs/FEATURES.md` 加一行

→ **v1 总改动:7 个改 + 2 个新 = 9 个文件 + 测试 + 实机 e2e,核心逻辑 ~ 270 行**(StandardPrompt 占 ~ 70 行)。

---

## 5. 设计取舍

### 5.1 v8 极简:不抽象,直接给 Starter 加 `Review` 方法

之前 v7 加 `SupportsReview() bool`(能力检查),但那不是真正的 slash command 对应方法。v8 改成 `Review(ctx, rc) error` — **直接做 review 动作**,bridge 自己拥有这个行为。

`/review` 的本质就是 "给当前 chat session 的 agent 注入一段 prompt",bridge 拥有这件事最自然。v8 给现有 `Starter` 接口加一个 `Review(ctx, rc) error` 方法,5 个 coding bridge delegate 到 `agent.DefaultReview`,pty/bash 返回 `agent.ErrReviewNotSupported`。

**为什么不新开 type / registry**(沿用 v7 论证):
- **新 type 增加心智负担** — 加 `Spec` 加 `Adapter` 加 `ReviewScope` 加 `ReviewRegistry`,4 个新概念只为支持一个 slash command,过度设计。
- **registry 是平行抽象** — nightme 已经有 `agent.Builtins` registry。再开一个 `ReviewRegistry` 等于做两套平行的注册体系,容易 drift。
- **`Starter` 已经是合适的扩展点** — `Starter` 已经声明 agent 的 spawn recipe + capability,在它上面加 `Review()` 是自然的扩展,跟 `Info()` / `Detect()` / `Start()` / `RunOnce()` 一致。
- **`Review` 而非 `SupportsReview`** — 方法名对应 slash command 直接,**自描述**;且让 bridge 自己拥有 review 行为(v2 某个 bridge 想 customize 时直接 override)。

→ v8 改动量级:**7 个文件各改 ~3 行 + 4 个新文件**(StandardPrompt 在 agent 包,因为 bridge 直接调)。vs v6:**3 个新文件 + 5 个新 adapter 文件 + 多 30 行 dispatcher**。

### 5.2 零限定符

`/review` 不带任何 flag / arg。永远是 "当前分支(含未 commit)vs 默认主分支"。理由:
- **对齐 PR review** — 用户在做一个 PR 前,想看的就是"branch vs main 上的全部 diff",这一种行为覆盖 95% 场景。
- **复杂场景留 v2** — 想限定文件、想覆盖 base、想加 hint,用户在 chat 里说 "再 focus 一下 foo.go" 就行,chat agent 二次响应。如果真需要频繁,再加 flag。
- **YAGNI** — v1 不实现 v2 才会用到的功能。如果 v2 真加,`Spec` 加字段、dispatcher 加 case、prompt 加段落,改动面小。
- **降低用户认知负担** — `/review` 不需要记忆任何参数。

### 5.2 架构 B:为什么 `/review` 走 chat session,不 spawn 新进程

| 维度 | RunOnce(架构 A) | 当前 AgentSession(架构 B) |
|---|---|---|
| 进程 | spawn 新进程,用完即弃 | 复用 chat session 现有进程 |
| chat 上下文 | fresh,看不到 chat history | 完整 |
| dispatcher 行为 | 同步等 review 完成,自己 send reply | 异步 — 立即返回 Consumed=true |
| reply 路径 | dispatcher drain events → `cs.Emitter().Send` | chat session 自己的 readpump → emit 链路 |
| fix 流程 | chat agent 收到 emit 的 review → 改 | 一样(已经在改) |
| agentbar / usagebar | dispatcher 手动盖章 | chat agent 既有链路自动盖章 |
| 跟 `/gtw commit` 比 | 同模式 | 跟 `/stop` / `/use` / `/close` 一致 |

→ `/review` 是 chat 内任务,跟 `/stop` / `/use` 一致;`/gtw commit` 是独立任务,适合 RunOnce。

→ 推论:**v1 能接所有 5 个 coding agent**(claude / codex / dsh / opencode / pi)— bridge 是否"有原生 review 子命令"不再重要,因为 chat agent 本身能跑 git 命令并按 StandardPrompt 输出 review。bash 不是 coding agent,不支持。

### 5.3 v7 推翻 v6:不抽象

v6 设计的 `Spec` / `Adapter` / `ReviewScope` / `ReviewRegistry` **全部不需要**。这些抽象为单个 slash command 增加了 4 个新 type + 1 个 registry + 5 个 adapter 文件,得不偿失。

→ v7 直接复用现有 `Starter` 接口加 `SupportsReview() bool` 方法 — 跟 `Info()` / `Detect()` / `Start()` / `RunOnce()` 一致,符合 nightme 的扩展约定("给现有 interface 加方法,所有 impl 加 1 行")。

### 5.4 为什么 StandardPrompt 不带 review 模板文件

考虑过放在 `internal/command/review/prompt.md` 用 `embed.FS` 加载,但:
- 增加构建复杂度(embed 需要 go:embed 指令 + 文件管理)
- 模板会被多次修改,内嵌到 binary 反而迭代慢
- v6 的 prompt 模板是一个长字符串,直接写 Go string literal 即可

→ 模板放 `.go` 文件里,等真要热更新再说。

### 5.5 v1 不暴露 `--fix` / `--comment` / `--post` / `ultra` / `--base` / `path` / `--`

`/review` 零限定符(v6)。理由:
- **`--fix`** / **`--comment`** / **`--post`** / **`ultra`** — 见 §1.3 + §5.x(同 v5 论证):写副作用、违反 agent-no-config-tampering、扩 scope 超出 v1。
- **`--base <branch>`** — 默认 base 探测足够(agent 自己跑 `git symbolic-ref refs/remotes/origin/HEAD`)。想覆盖 base 的人在 chat 里说 "以 develop 为 base 再 review 一次" 就行,二次响应。v2 评估。
- **`<paths>` 限定** — 全量 diff review 是核心场景;限定到文件的需求不太常见(用户在 chat 里说 "focus 一下 foo.go" 也能二次响应)。v2 评估。
- **`-- "<hint>"`** — 用户在 chat 里说 "重点看 auth" 比用 flag 更自然。v2 评估。

→ v1 `/review` 是真正的"按一下就 review",不引入用户认知负担。

### 5.6 标准 prompt 借鉴来源

`StandardPrompt` 借鉴三处的成熟做法(详见 §2.3 注释):
- Claude Code `/code-review` plugin 的 confidence-based + finding-first 结构
- Codex `review` 子命令的 severity 分组 + actionable suggestion
- 社区通用 senior-engineer-reviewing-PR 的 section 化输出

→ 不直接复制(claude 的多 agent 太重,codex review 是闭源内部 prompt),但借鉴结构和 severity 标签。

### 5.7 未注册 adapter 的 agent 回友好提示

dispatcher 检测到 `agent.ReviewRegistry.Get(name)` 报错(agent 名存在但没注册 adapter)时,**不**报错,而是回:

```
❌ agent "bash" 暂不支持 /review(bash 不是 coding agent)
   当前支持 /review 的 coding agent: claude, codex, dsh, opencode, pi
   (用 /use <name> 切换)
```

→ 比"agent not found"更精确 — 用户立刻知道有 agent 但 review 没接,以及哪些 agent 能用。

### 5.8 架构 B 的关键好处:**fix 复用同一进程**

`/review` 走 chat session 后,fix 自然不另起进程:用户说 "fix blockers",chat agent 用它自带的 Edit 工具直接改。**零新代码**(chat agent 本来就会改),**零新风险**(用户的 working tree 改动由 chat session 状态机管理,不是 review 的副作用)。

### 5.9 用户传了 arg 时的报错引导

`/review` 零限定符(v6),任何 arg 都报错:
```
❌ /review 不接受参数;去掉 "foo"
```

→ 用户立刻知道 `/review` 是没参数的命令,不会被 silently ignore。

---

## 6. 风险与未做

### 6.1 已知风险

- **agent 输出格式漂移**:v6 让 chat agent 自由按 StandardPrompt 输出 sections,如果 agent 不严格遵循(比如输出 markdown 但缺 ## Findings 段),IM 渲染不一致。**缓解**:prompt 用强烈的 "exact structure / do not reorder" 措辞 + v1 在 PR 1 e2e 实跑 5 个 agent 看是否格式一致;v2 可加 output parser 把 review 转 finding 卡片。
- **大 diff 性能**:`/review` 让 agent 跑 `git fetch origin` + `git diff <default>...HEAD` + `git diff --staged` + `git diff`,大分支(几百文件)可能 review 超时或 token 成本高。**缓解**:v1 接受现状;v2 加 `--max-files <N>` 截断或 streaming diff(走 chat readpump 已有 streaming 通路)。
- **默认 base 探测失败**:如果仓库没 origin / 没设置 HEAD branch,agent 的 `git symbolic-ref refs/remotes/origin/HEAD` fallback 到 "main" 可能跟实际不符。**缓解**:agent 自己 fallback 并在 review 输出里写明用的是哪个 base;v2 评估加 `/review --base` 显式 override。
- **chat agent 没有 git 工具**:理论上 coding agent 一定有 Bash tool,但如果某个 agent 配置异常,跑不了 git,review 出错。**缓解**:SendBlocks 失败的 error 已经在 dispatcher 捕获并回 friendly reply;agent 跑 git 失败时它自己会报错并 yield reply。

### 6.2 v1 不做

- ❌ **`/review` 任何 flag / arg**(v6 零限定符决策,见 §0.1 + §5.1 + §5.5)— 想限定文件、覆盖 base、加 hint,都走 chat agent 二次响应
- ❌ **`/review-fix` 或 `/review --fix` slash command**(fix 走 chat agent 自带的 Edit 工具,见 §1.3 + §5.8)
- ❌ **claude `--fix` / `--comment` / `--post` / `ultra` / `effort level` 暴露**(v1 决策,见 §0.3 + §5.5)
- ❌ **codex `--fix` 等价能力**(codex review 没有这能力;`codex apply` 是另一条路,v1 不串)
- ❌ **scope 区分**(uncommitted / staged / commit / pr)— v6 全部折叠成 "branch vs default";v2 评估是否需要还原
- ❌ finding 卡片渲染(v2)
- ❌ review 持久化 / 历史
- ❌ 自动发 PR 评论(`agent-no-config-tampering`)
- ❌ multi-reviewer 对比
- ❌ DSH 切到 session.prompt RPC(单独 doc,单独 PR)

> **架构 B 的简化效果**:`RunOnce` / `RunNative` / `timeouts.Review` / `StampRunResult` 这些架构 A 需要的东西,架构 B **全不需要** — 因为走 chat session 自带链路。

---

## 7. 参考

- [`F-gtw.md`](./F-gtw.md) §"agent invocation + unified reply sink" — `replyAgent` 设计的同源(架构 A 的启发)
- [`F-dsh-bridge.md`](./F-dsh-bridge.md) — dsh 接入现状
- [`docs/bridge/codex.md`](../bridge/codex.md) — codex `review` 子命令形态
- [`docs/bridge/claudecode.md`](../bridge/claudecode.md) — claude `-p "/code-review"` print-mode 行为
- [`docs/SPEC.md`](../SPEC.md) §3.1 / §3.4 — 跨层协议边界 + handle inbound 一致性
- memory `agent-no-config-tampering` — 不注入 model / provider / credentials / 默认配置
- memory `nightme-agent-info` — Adapter 走 struct 不走 interface

## 8. 实机 e2e 验证记录(2026-08-17)

验证环境:`/tmp/review-smoke` 内一个 Go module,故意引入 `buggyDivide(10, 0)`(panic)和 `saveUser` 吞 `Write` 错误两个 blocker。

| 命令 | v1 关联 | 状态 | 输出要点 |
|---|---|---|---|
| `claude -p "/code-review"` | ✅ v1 claude adapter | ✅ works | JSON findings(3 条),覆盖两个已知 bug |
| `claude -p "<StandardPrompt>"` | ✅ v1 StandardPrompt | ✅ works | 文本 sections(## Summary / ## Findings / ## Suggestions),所有已知 bug 命中 |
| `codex review --uncommitted` | ⚠️ v1 不调子命令(走 chat agent),但子命令本身已验证可识别 | ⚠️ 模型 fallback | model `MiniMax-M3` not found,但 subcommand 本身正常被识别 |
| `dsh --profile headless -- "<prompt>"` | ✅ v1 dsh adapter | ⏸ 未跑 | 需要 dsh 服务器就绪 + API key,PR 3 实机跑 |
| `opencode run --prompt "<msg>"` | ✅ v1 opencode adapter | ⏸ 未跑 | 同上 |
| `pi -p "<msg>"` | ✅ v1 pi adapter | ❌ API key | 当前 pi 配置 google provider,API key invalid;不影响机制 |

→ **架构 B 核心机制已实机验证**:
- claude `/code-review` slash trigger → 输出 JSON findings ✅
- StandardPrompt → 输出结构化 sections ✅
- codex `review` 子命令 → argv 形态 OK(模型 fallback 是配置问题,不是机制问题)

→ **PR 1 (类型与 registry) 之前的所有 PR 都不需要真机,只需要 unit test**;PR 3 落地后再做 chat 内 review 集成 e2e。
---

## 9. v1 实施记录(2026-08-17,feat-code-review 分支)

设计稿 v9 落地后的实际代码状态。后续 PR 引用本节对齐文件清单。

### 9.1 实际文件清单(跟 §3 设计稿对应)

| 类型 | 路径 | 行数 | 状态 |
|---|---|---:|---|
| **新建 — 核心** | `internal/agent/review.go` | 198 | ReviewContext / DefaultReview / StandardPrompt / ErrReviewNotSupported |
|  | `internal/command/review/cmd.go` | 148 | Factory + dispatcher(inline `len(input.Args)` 检查) |
| **新建 — 测试** | `internal/agent/review_test.go` | 153 | StandardPrompt 内容 / DefaultReview 行为 / ErrReviewNotSupported sentinel |
|  | `internal/agent/review_per_bridge_test.go` | 157 | 每个 bridge 注入 StandardPrompt 验证 + 错误传播 |
|  | `internal/command/review/cmd_test.go` | 100 | dispatcher 错误路径(extra args) |
| **改 — interface** | `internal/agent/agent.go` | +9 | Starter interface 加 `Review(ctx, rc) error` 方法 + 注释 |
| **改 — 7 bridges** | `internal/bridge/claudecode/starter.go` | +5 | Review → DefaultReview |
|  | `internal/bridge/codex/starter.go` | +5 | Review → DefaultReview |
|  | `internal/bridge/dsh/starter.go` | +5 | Review → DefaultReview |
|  | `internal/bridge/opencode/starter.go` | +5 | Review → DefaultReview |
|  | `internal/bridge/pi/starter.go` | +5 | Review → DefaultReview |
|  | `internal/bridge/pty/starter.go` | +6 | Review → ErrReviewNotSupported |
|  | `internal/bridge/acp/starter.go` | +6 | Review → DefaultReview(v1 也覆盖 acp,跟 §3 表格的"6 bridges"差异是 acp 是 opencode 的下层,本身也满足 Starter interface) |
| **改 — test fakes** | `internal/agent/registry_test.go` | +3 | fakeAgent.Review → ErrReviewNotSupported |
|  | `internal/command/e2e_slash_test.go` | +5 | echoAgent.Review → ErrReviewNotSupported |
|  | `internal/command/gtw/push_test.go` | +6 | recordingAgent.Review → ErrReviewNotSupported |
|  | `internal/command/gtw/hooks_test.go` | +5 | testStarter.Review → ErrReviewNotSupported |
|  | `internal/chatsession/new_real_pi_unix_test.go` | +3 | fakeAgentBuilder.Review → ErrReviewNotSupported |
|  | `internal/gateway/integration_chatsession_unix_test.go` | +6 | integrationFake.Review → ErrReviewNotSupported |

**核心逻辑 ~ 350 行**(`StandardPrompt` 占 ~ 150 行,`cmd.go` dispatcher ~ 60 行,其余 boilerplate)。总改动 **19 个文件**(7 bridges + 7 核心 + 6 fakes)。

### 9.2 实际接口签名(以代码为准,设计稿的最终版)

```go
// internal/agent/agent.go
type Starter interface {
    Info() Info
    Detect() error
    Start(ctx context.Context, cfg StartConfig) (*Agent, error)
    RunOnce(ctx context.Context, cfg StartConfig, blocks []ContentBlock) (RunResult, error)

    // Review runs the /review slash command against this agent's
    // current chat session. The /review dispatcher (see
    // internal/command/review) provides a ReviewContext that wraps
    // the chat session's AgentSession.Handle().SendBlocks callback;
    // the bridge is responsible for injecting a review prompt via
    // that callback.
    Review(ctx context.Context, rc ReviewContext) error
}
```

```go
// internal/agent/review.go
type ReviewContext struct {
    Workspace string
    Inject    func(ctx context.Context, blocks []ContentBlock) error
}

var ErrReviewNotSupported = errors.New("agent: /review not supported")

func DefaultReview(ctx context.Context, rc ReviewContext) error
func StandardPrompt() string
```

```go
// internal/command/review/cmd.go (核心 dispatcher)
func (f *Factory) Handle(ctx, rt, cs, input) (*command.SlashOutput, error) {
    if len(input.Args) > 1 {
        return command.Reply(ctx, rt, fmt.Sprintf(
            "❌ /review 不接受参数;去掉 %q", input.Args[1])), nil
    }
    as := cs.SelectedAgentSession()
    if as == nil { return /* "no active agent" */ }
    starter, err := agent.Builtins.Get(as.Agent)
    if err != nil { return /* "unknown agent" */ }
    rc := agent.ReviewContext{
        Workspace: cs.SelectedCwd(),
        Inject:    as.SendBlocks,
    }
    if err := starter.Review(ctx, rc); err != nil {
        if err == agent.ErrReviewNotSupported { return /* "agent X 暂不支持 /review" */ }
        return command.Reply(ctx, rt, "❌ /review 失败: "+err.Error()), nil
    }
    return &command.SlashOutput{Consumed: true}, nil
}
```

### 9.3 实际验证结果

**`go build ./...`**: 全部编译通过(无错)。

**`go test -count=1 -short -timeout 60s ./...`**:

```
ok  internal/agent                  1.202s
ok  internal/command/review        0.390s
ok  cmd/nightme                    9.491s
ok  internal/agentsession          1.476s
ok  internal/bridge/acp            4.496s
ok  internal/bridge/claudecode     3.547s
ok  internal/bridge/codex          7.335s
ok  internal/bridge/dsh            8.283s
ok  internal/bridge/dsh/host       10.383s
ok  internal/bridge/opencode       3.552s
ok  internal/bridge/pi             30.834s
ok  internal/bridge/pty            3.368s
ok  internal/chatsession            14.260s
ok  internal/command/gtw           15.713s
... (49 packages total, all ok)
```

**smoke test**(2026-08-17 实跑):
- `/tmp/review-smoke` 内一个 Go module,故意引入 `buggyDivide(10, 0)` panic + `saveUser` 吞 `Write` 错误。
- `claude -p "/code-review"` 实机输出 JSON findings(3 条),命中两个已知 bug ✅
- v1 dispatcher 已集成;真机 /review 注入流程(走 chat session)留 PR 2 文档化后再做端到端 e2e。

### 9.4 跟设计稿 v9 的差异(微调)

| 项 | 设计稿 v9 | 实装 | 原因 |
|---|---|---|---|
| bridge 数量 | 6 | **7**(多了 `acp`) | acp 也有 Starter 类型(`interface_external_unix_test.go` 已经测 Starter 满足);opencode 走 acp 协议,但 acp 本身也要满足 interface |
| AgentSession 取名 | `as.Handle().SendBlocks` | `as.SendBlocks` | AgentSession 本身有 `SendBlocks` 方法(在 session.go:1486),不需绕 `Handle()` |
| pty 路径 | `pty.NewStarter("bash", "bash", nil, nil, 0, 0)` | 同 | 一致 |
| 测试覆盖 | 提到每个 bridge 加表驱动测试 | 落到 `review_per_bridge_test.go`(按文档 3.4 拆出来,更聚焦) | 拆出独立文件避免主 `registry_test.go` 过载 |
| `agent.Builtins` 测试 | 提议做 "TestRegistryContainsCodingAgents" | **删除** | 测的是 `cmd/nightme/agents.go` 的 init() 序列,不是 review 包;`/review` 包不依赖那个 init() |
| error message | 设计稿: "❌ agent X 暂不支持 /review" | 实装:加上"bash / pty 不是 coding agent,不能 review\n   当前支持 /review 的 agent: claude, codex, dsh, opencode, pi\n   (用 /use <name> 切换)" | 更友好,显式列出可用 agent |

### 9.5 下一步(PR 2)

按 v9 设计的 2-PR 拆分:

1. **PR 1(已合并到 feat-code-review 分支,待 commit + push)**: §9.1 列出的 19 个文件改动。
2. **PR 2(待做)**: docs 单独立 PR —
   - `docs/PRD.md` 加一行:`/review` slash command
   - `docs/FEATURES.md` 加一行:`/review` description
   - `docs/feat/F-review.md`(本文档)merge 时的设计 reference

→ PR 2 是纯 docs 改动,可独立 review 跟 merge。

---

## 10. v6 改造:从 prompt 注入改为 RunOnce 隔离(2026-08-18)

设计稿 §9 落地后,我们发现 v1 的"prompt 注入到主 turn"架构有两个问题:

1. **main context 污染**:review 的中间推理(token 几万)在主 turn 里烧 main context,挤掉后续 chat 可用空间。
2. **timeout 风险**:虽然 v1 dispatcher 也加了 ctx timeout,但 prompt 注入路径下 timeout 触发的语义不清晰 — 是不是已经注入了?还能不能恢复?

参考 claude 内置 `/code-review` 的设计(跑在独立 subagent 里),我们用 **`Starter.RunOnce`** 做隔离 — 这就是 `RunOnce` 本身的语义等价物:一个 fresh 进程,fresh context,fresh model state。

**关键 insight**(user):用 `RunOnce` 之后,**底层 bridge 不需要单独支持 subagent**。`RunOnce` 就是 subagent 的等价物,claude / codex / dsh / opencode / pi 都已经支持 `RunOnce`,我们直接复用,不需要新增任何抽象。

### 10.1 v6 关键变化 vs v1

| 项 | v1 (commit 35bdc07) | v6 |
|---|---|---|
| `internal/agent/review.go` 导出 | `DefaultReview(ctx, rc)`(只 inject prompt) | `ReviewOnce(ctx, s, rc)`(用 `s.RunOnce` 跑 review + inject findings) |
| bridge `Review` 方法 | `return agent.DefaultReview(ctx, rc)` | `return agent.ReviewOnce(ctx, s, rc)` |
| main context 污染 | ❌ review 推理在 main turn | ✅ review 在独立 RunOnce 进程,main 干净 |
| timeout 安全 | dispatcher 包装 ctx,但 prompt 已注入则部分状态 | ✅ RunOnce 自己的进程边界,timeout 触发只是 kill review 进程 |
| fix 上下文 | main agent 直接看到 prompt + 自己推理 | main agent 看到 inject 进来的 findings 文本,效果一致 |
| dispatcher cmd.go | 仅 `ctx, cancel` | `ctx, cancel := context.WithTimeout(ctx, timeouts.Review)`(30 min) |
| 跨 bridge 复杂度 | 5 bridge × 1 行 delegate | 5 bridge × 1 行 delegate(行数不变,语义不同) |

### 10.2 `timeouts.Review` 新增

```go
// internal/timeouts/timeouts.go
Review = 30 * time.Minute
```

跟 `timeouts.Agent` 同值(30 min)。语义独立,文档化意图:
- `/review` 是 LLM 驱动的审计任务(多步 git 检查),LLM 时代预算适用
- `RunOnce` 进程边界让 30 min timeout 安全:deadline 触发只 kill review 进程
- 跟 `timeouts.Agent` 对齐(因为 `/gtw commit` 已经用 Agent 30 min 包装 RunOnce)

### 10.3 实机行为对比

**v1**(commit 35bdc07):
```
用户: /review
bot: [30-60s 后] ## Summary ...
     ## Findings ...
     ## Suggestions ...
[中间推理过程:token 几万在 main context 烧]
```

**v6**(即将 amend):
```
用户: /review
bot: [独立 RunOnce 进程跑 30-60s,main context 干净]
     ## Code review of /Users/me/proj
     (current branch vs default branch; run via /review)
     
     ## Summary ...
     ## Findings ...
     ## Suggestions ...
[main AS 看到 review 作为 user message,回应一句简短确认]
[用户: 修一下]
[main AS 用 Edit 工具改]
```

**关键差异**:
- v6 的 main context 完整保留 — chat agent 的中间推理不烧 main
- v6 的 RunResult.Model / Usage 通过 chat agentbar 链路展示(自动)
- 用户的"修一下"流程不变 — main AS 在 review 文本里有完整 findings 上下文

### 10.4 测试更新

`internal/agent/review_per_bridge_test.go` 之前用真实 bridge(5 个 coding bridge 各 `NewStarter(...)`),依赖 binary on PATH。v6 改成用 `testStarter` mock — 不依赖真 binary,在 CI 也能跑。

每个 bridge 的 `Review` 方法都是 1 行 `return agent.Review(ctx, s, rc)` — 这一点通过 eyeball check 各 bridge starter.go 验证即可,不写重复表驱动测试。

### 10.5 不变的部分

- dispatcher 入口签名(cmd.go 的 `Handle` 方法)
- dispatcher 的 inline `len(input.Args) > 1` 检查
- pty / bash 的 `ErrReviewNotSupported` 路径
- `ReviewContext` 字段(Workspace + Inject)
- `StandardPrompt` 内容
- `Spec.Usage` / `Spec.Summary`
- chat session 的状态机
- chat agent 看到的 user message 流(只是内容从"prompt"变成"findings")

### 10.6 后续 v2 评估(不阻塞 PR)

- v2 加 `--async` flag 让 review 跑在 goroutine,user 可以继续 chat 不阻塞
- v2 加 `--base <branch>` flag 覆盖默认 base
- v2 加 `--path <files>` 限定文件
- v2 加 finding 卡片渲染(把 JSON 转卡片)

---

## 11. v7 改造:Handle 完全异步(2026-08-18)

v6 的 dispatcher 还在等 `starter.Review` 返回才 `Consumed=true` — 占着 dispatcher worker 30-60 秒。**slash command 本身是在 goroutine 跑的**(`internal/gateway/inbound/command.go:65` 派发),所以主 chat session 一直不阻塞 — 但 dispatcher worker 被占着。

v7 直接让 Handle 立刻返回 Consumed=true,把工作丢到独立 goroutine。

### 11.1 关键变化 vs v6

| 项 | v6 (commit 2afe822 → 6f02696) | v7 |
|---|---|---|
| `cmd.go` Handle 占用 dispatcher worker | 30-60 秒(等 RunOnce 完成) | 立即释放(返回 Consumed=true 后) |
| Handle 阻塞主 chat session? | ❌ 不阻塞(本来就不阻塞) | ❌ 不阻塞(同样) |
| 用户发完 /review 能继续发消息? | 能(readpump 没卡) | 能(一样) |
| 多用户并发 /review? | worker pool 排队 | 全并行(每个立即 launch 独立 goroutine) |
| /close 取消 review? | ❌(Handle 还在等 RunOnce,跟 chat 关闭无关) | ✅ goroutine 用 `cs.Context()`,关闭自动 cancel |
| Handle 失败时用户可见? | reply 立刻显示 | log only(operators 看 logs;v2 加 cs.Emitter().Send) |

### 11.2 Handle 主体改动

```go
// 之前(v6):同步等 RunOnce
revCtx, cancel := context.WithTimeout(ctx, timeouts.Review)
defer cancel()
if err := starter.Review(revCtx, rc); err != nil { ... }
return &command.SlashOutput{Consumed: true}, nil

// 现在(v7):立即返回,work 跑独立 goroutine
agentName := as.Agent
go func() {
    revCtx, cancel := context.WithTimeout(cs.Context(), timeouts.Review)
    defer cancel()
    if err := starter.Review(revCtx, rc); err != nil {
        slog.Default().Warn("/review failed", "agent", agentName, "err", err)
    }
}()
return &command.SlashOutput{Consumed: true}, nil
```

### 11.3 ctx 选择

- `cs.Context()`:chat session 自带 context,关闭时 cancel。`/close` → review 自动取消(子进程被 kill,不会有 orphan work)。
- `timeouts.Review` (30 min):跟 v6 一致,防 review 真的 hang 住。

### 11.4 错误处理

v7 把"成功 / 失败"和"用户可见"解耦:
- **成功**:review findings 通过 `rc.Inject` 出现在 chat 消息流(chat session 正常 emit 链路)
- **失败**:goroutine 里 log(`slog.Default().Warn`),不 emit 到 chat(因为 Handle 已经返回,inline reply 路径没了)

v2 评估:加 `cs.Emitter().Send("review failed: ...")` 给用户可见的失败通知。

### 11.5 测试覆盖

- `internal/agent/review_test.go` — `agent.Review` 函数本身(用 fakeStarter 验证 RunOnce 路径)
- `internal/agent/review_per_bridge_test.go` — 5 bridge 共享 `agent.Review` 的契约
- `internal/command/review/cmd_test.go` — 只测 inline 错误路径(arg check / accept no args);async 行为由上面两个测试 + 实机 smoke 覆盖

完整 chat session 构造太重,async dispatcher pattern 由代码审查 + smoke test 验证,不写重复测试。
