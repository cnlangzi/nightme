# F-review: 内置 `/review` 命令设计

> **Status**: design draft（讨论中，未实现）
> **Revision**:
>   - v1 (2026-01-XX, 初稿): adapter 主要为 prompt 路径，OpenCode 唯一原生
>   - v2 (2026-01-XX, 修正): 用户确认 Claude Code / Codex 也有原生 `/review`；adapter 设计反转 — **native 是主路径，prompt 是 fallback**；focus 处理分 inline (opencode) vs follow-up (claude/codex)
> **Author**: 调研稿 — 待与 F-team（PM/Architect + 虾哥）确认后落地
> **Related docs**:
>   - 项目总览 → [`../SPEC.md`](../SPEC.md)
>   - Slash command 引擎 → [`../PRD.md`](../PRD.md)、`internal/command/`
>   - 既有 prompt 模板（参考） → `internal/command/gtw/commit.go::buildAgentPrompt`、`internal/command/gtw/pr.go::buildPRPrompt`
>   - 既有社区 review prompt 范式（参考） → 社区常见的"代码 review / 安全 review / PR review" prompt 模板

---

## 1. 动机 / 问题陈述

nightme 现在已经能：

- 把消息送到 Claude Code / OpenCode / Codex / Pi / dsh / pty 任何一个底层 CLI（`internal/bridge/*`）
- 提供 `/gtw commit` / `/gtw pr`（`internal/command/gtw/`）这种**单轮 side-effect** 命令，跑一个
  `RunOnce` 调用把整个 prompt 写成一个 `ContentBlock` 喂给 agent
- 提供 `/steer`、`/new`、`/close`、`/stop` 这种**多轮 chat 内**对会话状态做切流（`internal/command/{steer,newcmd,close,stop}/`）

但缺一类命令：**"对当前工作区做一次代码 review"**。这个需求覆盖三个常见 case：

- **用户视角**：刚让 agent 改完一个 feature，想让它自己回头审一遍（"do a self-review"）
- **用户视角**：打开了一个新工作区 / 刚 clone 完仓库，想在改之前先让 agent 给一个
  "what to watch out for / what's risky here" 的报告（"cold review"）
- **团队视角**（gtw 现状）：`/gtw` 已经管"PR 怎么开"，但开 PR 之前是否要做一次 review、review
  结果怎么影响 PR body / label 流转，目前没有接口

所以本设计要补一个 **内置的 `/review [focus]`** slash 命令。

---

## 2. 设计约束（来自 SPEC.md + 现有架构）

### 2.1 不变式（强约束）

| # | 不变式 | 出处 |
|---|--------|------|
| I-1 | `command/` 包不 import `chatsession`/`gateway`/`channel` 等具体类 | SPEC.md §1.3 |
| I-2 | 每个 slash command 都通过 `SlashCommandFactory` 注册到 `Registry` | SPEC.md §1.3 |
| I-3 | 每条命令由 `internal/command/<name>/cmd.go` 实现，`init()` 里 `command.RegisterBuilder(...)` | `internal/command/{steer,think,...}/cmd.go` |
| I-4 | `Factory.Handle` 不直接产 `OutboundMessage`，仅返回 `*SlashOutput` + `command.Reply` helper | `internal/command/reply.go` |
| I-5 | 抽象层只见 `agent.ContentBlock` / `chatsession.Message` 这类 generic primitive，不引入 bridge-specific 字段 | SPEC.md §1.4 |
| I-6 | 不引入新 `OutboundKind` — `command` 层只是产生 `OutReply` 文本，让 `runtime/handler.go` 走正常事件流 | SPEC.md §2.4 |

### 2.2 现有可复用 API

| API | 用途 | 出处 |
|-----|------|------|
| `chatsession.QueueUserMessage(Message{...})` | 把一段 `ContentBlock` 投进 chat 的消息队列；会走 `TryFlush` → `AS.Submit` 把 blocks 发给 agent | `internal/chatsession/chatsession.go:974` |
| `agent.Starter.RunOnce(ctx, cfg, blocks)` | 单轮同步调用：spawn → send → drain → close | `internal/agent/agent.go:1011-1024`（gtw 的 commit / pr 路径） |
| `cs.SelectedAgent()` / `cs.SelectedCwd()` | 拿当前 chat 选定的 agent name 和 workspace | `internal/chatsession/chatsession.go:724,595` |
| `agent.Builtins.Get(name)` / `Detect()` / `RunOnce(...)` | gtw 用的解析路径 | `internal/command/gtw/agent_reply.go:102-117` |
| `timeouts.Agent` | 单 agent 调用的超时上限 | `internal/timeouts/timeouts.go` |
| `command.RuntimeServices.Config.Primary` | 默认 agent 名 | `internal/command/runtime.go:38-45` |

### 2.3 review 的两条自然路径（这是核心选择题）

| 路径 | 含义 | 类比 |
|------|------|------|
| **A. 多轮 chat 路径** | 把 review prompt 作为一条 Message 投到 chat 的 InputBuffer 里，agent 在它**自己的会话上下文**里接着 review（保留所有之前的对话历史） | `/steer` / 用户发一条普通文本 |
| **B. 单轮 one-shot 路径** | 用 `RunOnce` 起一个**独立**的 reviewer（不污染当前会话上下文），review 完即销毁 | `/gtw commit` / `/gtw pr` |

这两个路径在 SPEC.md §1.3 / §2.5 里都有清晰的不变式，先确定我们要走哪条，再讨论底层的转接。

---

## 3. 路径选择：A 还是 B？

### 3.1 用例区分

| 用例 | 推荐路径 | 理由 |
|------|----------|------|
| "刚让 agent 改了 X，回头审一遍 X" | **A** | 上下文（"刚才的意图"）必须有；否则 reviewer 不知道作者想达成什么 |
| "clone 完新仓库先扫一遍风险" | **B** | 当前 chat 没有上下文；甚至可以指定一个独立的 reviewer agent（更便宜 / 更专精） |
| "team work flow 里 PR 前的 gate" | **B** | 不能污染作者 chat；reviewer 必须是 orthogonal observer |
| "/review 上次结果"（纯历史 review） | **A** | 上下文就是上次结果本身 |

### 3.2 设计选择

**两条都做** — `/review [focus]` 默认走 A，flags 切 B：

```
/review                          → A: 在 chat 内发起一条 self-review message
/review --one-shot [focus]       → B: RunOnce 起独立 reviewer
/review --one-shot -a reviewer   → B + 指定 reviewer agent（覆盖 SelectedAgent）
```

参数表是 gtw commit / gtw pr 的延伸，结构上对齐：

- `Agent`（`-a <name>` / `--agent <name>`）：B 路径专用
- `OneShot`（`--one-shot`）：切到 B 路径
- `Focus`（位置参数）：review 主题（"安全" / "性能" / "可读性" / "API 一致性" 等）

---

## 4. 底层 CLI 的 review 转接设计

**核心问题**：用户可能用 Claude Code / Codex / OpenCode / Pi / dsh / pty 任何一个底层引擎。每个引擎的"内置 review"方式都不一样；我们要写一个 **agent-agnostic 的 adapter**，让 `/review` 命令在不同 agent 上都 work。

### 4.1 调研：各 AI Coding Agent 的内置 review 形态

下表是当前已知信息（截至调研时间；不熟悉的用 `?` 标注，待脚本实验确认）：

| Agent | 是否内置 `/review` 命令 | 调用方式 | 我们的转接策略 |
|-------|------------------------|----------|----------------|
| **Claude Code** (`internal/bridge/claudecode`) | **✅ 内置 `/review`**（Anthropic 在 2025 H2 加入的内置 slash command；GitHub PR review 场景，会读 `main`/base 的 diff 跑一次 PR-style review；通过 stdin 的 user message 触发，和 `/clear` 同形态）。参考 `/clear` 实现：`internal/bridge/claudecode/claudecode.go:652-666` | 走 stream-json stdin 的 user message：`{"type":"user","message":{"role":"user","content":"/review"}}` | **直接转发 + 加 focus**：adapter 判定"目标 agent 有原生 `/review`"时，把 `/review [focus]` 通过 `cs.QueueUserMessage` 直接喂给当前 chat 的 agent；不写自己的 prompt。Focus 作为 review 完成后**追加的一条追问 message**（不是 `/review` 本身带参数） |
| **OpenCode** (`internal/bridge/opencode`) | **✅ 有内置 `/review`**（OpenCode 1.x 官方文档） | HTTP `POST /api/session/:id/command` 或在 `available_commands_update` 事件里 advertise（`internal/bridge/opencode/translate.go:613-630`） | **直接转发**：检测到 `availableCommands` 里有 `review` 时，把 `/review` 原样通过 SSE/HTTP 转发 |
| **Codex CLI** (`internal/bridge/codex`) | **✅ 内置 `/review`**（OpenAI 在 2025-2026 期间给 Codex CLI 加了内置 `/review`，PR-style review；chat 模式走 JSON-RPC：`internal/bridge/codex/rpc.go`）。但 codex 还有独立 `codex review` 子命令（GitHub Action 用），那是**和 chat session 不同的进程模型**，不能直接复用 | Codex chat 模式走 JSON-RPC | **直接转发 + focus 处理**：检测 codex 的 RPC `available_commands`（如果它 advertise 的话）有 `review`，或直接走"user 消息内容是 `/review [focus]`"的形态；不写自己的 prompt（除非后续发现 codex 的 `/review` 在某些情境下不够好） |
| **Pi** (`internal/bridge/pi`) | **❓**（Pi 是比较新的 agent，内置命令少） | RPC 走 stdin/stdout | **Prompt 转写**（fallback 默认） |
| **dsh** (`internal/bridge/dsh`) | **❓** | HTTP | **Prompt 转写**（fallback 默认） |
| **pty** (`internal/bridge/pty`) | **❌**（pty 只是裸 stdin/stdout，没有任何 agent-side 解析） | 用户输什么就是什么 | **Prompt 转写**（fallback 默认） |

**结论（修正版）**：底层引擎**Claude Code / OpenCode / Codex 三个都有原生 `/review`**。只有 Pi / dsh / pty（以及未来未知的新 agent）需要 nightme 自己写 prompt。

设计含义：

1. **`Adapter.SupportsNative()` 不再是"几乎永远 false"** — 主流 agent 大多数时候都是 `true`，prompt adapter 退居 fallback
2. **Native 路径的 focus 处理**：原生 `/review` 通常**不接受参数**（只是触发 PR review）。我们要把 focus 拆出来，native 模式下"focus"作为 review 完成后**追加的一个追问 message**（不是 `/review` 本身带 focus）；prompt 模式下 focus 直接写到 prompt 模板里
3. **Prompt adapter 仍然重要**：它处理"未知 agent / 不原生支持 review 的 agent"，而且**也是我们 fallback 时的高质量模板来源**，所以 v1 模板还是要写

> **修订历史**：本文档初稿（2026-01）我把 Claude / Codex 标成"❓官方未公布"，是因为当时调研没找到官方 `/review` 文档，且项目内 `internal/bridge/claudecode/` 也没有任何 `/review` 的代码痕迹（只有 `/clear`）。后来这两个 CLI 都在 2025 H2 加了原生 `/review`。**设计上要重新调整**：直接转发的路径是主路径，prompt adapter 是 fallback。

### 4.2 Adapter 接口设计

**设计变化**：原设计（只有 OpenCode 有原生 review）→ 修正版（Claude / Codex / OpenCode 三个都有）。这意味着 `Adapter` 的语义要反过来 — **native 是主路径，prompt 是 fallback**。

```go
// internal/command/review/adapter.go (新增)
package review

// Adapter abstracts "how this backend invokes /review" so the
// /review command can pick a backend-specific strategy at runtime.
//
// Implementations are looked up via the SelectedAgent name
// (cs.SelectedAgent() → adapter for that agent). Unknown agents
// fall back to PromptAdapter (the nightme-authored prompt).
type Adapter interface {
    // Name returns the backend identifier this adapter targets
    // (e.g. "claude", "opencode", "codex"). Matches agent name in
    // Builtins registry.
    Name() string

    // SupportsNative reports whether the underlying CLI has a
    // built-in /review slash command we can forward to verbatim.
    //
    // 2025 H2 update: Claude Code / OpenCode / Codex all advertise
    // a native /review. So this returns true for the mainstream
    // agents; only Pi / dsh / pty / future unknowns return false.
    //
    // When true: BuildBlocks emits a /review message (and
    // optionally a focus-followup; see NativeReviewFollowup).
    // When false: BuildBlocks emits the full nightme-authored
    // review prompt.
    SupportsNative() bool

    // BuildBlocks returns the structured content the agent
    // will receive.
    //
    //   - Native-supporting adapter: returns ONE ContentBlock
    //     whose text is literally "/review" (or "/review <focus>"
    //     when the CLI supports parameterised review — opencode
    //     does, claude/codex historically didn't). Plus, when
    //     NativeReviewFollowup is non-empty, an OPTIONAL second
    //     ContentBlock carrying the focus as a follow-up question
    //     (the agent sees /review → outputs review → then sees
    //     the focus follow-up → narrows in on the user's subject).
    //
    //   - Prompt adapter: returns ONE ContentBlock carrying the
    //     full nightme-authored review prompt (focus is baked in).
    //
    // focus is the user-supplied subject ("security",
    // "performance", "api consistency", or empty for
    // default general review).
    //
    // cwd is the chat's SelectedCwd so the prompt adapter can
    // resolve repo context (branch, recent commits) and
    // include it in the prompt. Native adapters ignore cwd
    // (the CLI resolves it itself).
    BuildBlocks(focus, cwd string) ([]agent.ContentBlock, error)

    // NativeReviewFollowup returns the focus-specific follow-up
    // message to send AFTER the native /review completes (when
    // SupportsNative is true). Returns "" when no follow-up is
    // needed (either because focus is empty, or the CLI natively
    // takes the focus as a /review parameter and BuildBlocks
    // already inlined it).
    //
    // Default focus recipes:
    //   "security"  → "Now narrow your review to security issues: STRIDE,
    //                  injection / XSS / insecure deserialization / hardcoded
    //                  secrets. Cite CVE/CWE where applicable."
    //   "performance" → "Now narrow your review to performance: hot paths,
    //                  allocations, lock contention, N+1 queries."
    //   "api"       → "Now narrow your review to API consistency: naming,
    //                  error shapes, backwards-compat implications."
    //   "general"   (default, when empty) → ""
    //
    // Recipe table lives in focus_recipes.txt (embedded).
    NativeReviewFollowup(focus string) string
}
```

**实现矩阵**（修正版）：

| Adapter | SupportsNative | focus 处理 |
|---------|----------------|------------|
| `ClaudeCodeAdapter` | ✅ true | `BuildBlocks("/review")` + follow-up |
| `OpenCodeAdapter` | ✅ true（accepts `/review <focus>`） | `BuildBlocks("/review <focus>")`，无 follow-up |
| `CodexAdapter` | ✅ true | `BuildBlocks("/review")` + follow-up |
| `PromptAdapter`（**fallback / 默认**） | ❌ false | `BuildBlocks(<full prompt with focus baked in>)` |

**注册机制**：

```go
// init() in internal/command/review/adapter_claudecode.go
func init() { review.RegisterAdapter(claudeCodeAdapter{}) }
// init() in internal/command/review/adapter_opencode.go
func init() { review.RegisterAdapter(opencodeAdapter{}) }
// init() in internal/command/review/adapter_codex.go
func init() { review.RegisterAdapter(codexAdapter{}) }
// init() in internal/command/review/adapter_prompt.go
func init() { review.RegisterAdapter(promptAdapter{}) } // catch-all, registered LAST
```

`RegisterAdapter` 维护一个 `map[name]Adapter`，由 `ResolveAdapter(agentName string) Adapter` 查表；找不到时返回 `promptAdapter{}`（**默认走 prompt 转写**）。

**OpenCode 的特殊处理**：OpenCode 的 `/review` 接受参数（`/review security` 这种），所以 `opencodeAdapter.BuildBlocks` 会**把 focus 拼到 `/review` 后面**直接发出去，不走 follow-up。其他两个（Claude / Codex）原生 `/review` 不接受参数，所以 focus 走"review 完后追加一条 follow-up message"。

### 4.3 为什么不让 adapter 直接调用底层 CLI？

我们**不**走"nightme 自己在 host 上 fork 一个 `codex review`/`claude review` 进程"的路径，理由：

1. **不变式 I-5**（SPEC.md §1.4）：adapter 是 generic primitive 层，必须通过 `AgentSession` / `RunOnce` 走 runtime 抽象，不能让 command 包 fork exec 直接调底层 CLI
2. 绕过 AgentSession 等于绕过整个 chat session 上下文 / event 流 / message state / receipt card 渲染 — 用户看到的 card 就会"突然冒出来一条不属于当前 chat 的输出"
3. `RunOnce` 已经是抽象，且 gtw 在用、已经被验证为正确的"one-shot side-effect"路径

**所以 PromptAdapter 的实现方式 = 把 review prompt 写成 `[]ContentBlock`，走 A 或 B 路径。**

---

## 5. Prompt Adapter 的 prompt 模板（核心）

这是整个设计里**最需要打磨的部分**：community 里出名的 review prompt 范式已经被反复验证过，我们要借鉴但 nightme 化。

### 5.1 社区常见 review prompt 范式（参考）

调研下来，社区里"高引用 + 持续维护"的 review prompt 主要分这几类（**仅供借鉴结构，不是照抄**）：

1. **GitHub Copilot 的 PR Review 模板**（Microsoft 工程实践社区）— 强调"4 个维度"：
   correctness / security / performance / maintainability；每个维度有 rubric；
   要求 reviewer 必须 cite line number；要求 `Severity: blocker|major|minor|nit`
2. **Anthropic Cookbook / Claude Code 社区 "do a code review" 模板** —
   强调"读 diff 而不是猜"（`git diff` 必跑）、按"作者意图 → 设计 → 实现 → 测试"四阶读；
   输出"finding list"而不是"逐行注解"；每个 finding 必须有 "Why" + "Suggested fix"
3. **"Staff engineer reviewer" 模板**（Cursor / Cline 社区）— 给 reviewer 一个 role prompt：
   "你是 staff engineer，你要 reject 这个 PR 如果..."；强调拒绝 surface 而非妥协
4. **Security review 模板**（OWASP 社区 + Snyk 团队）—
   STRIDE 模型；注入 / XSS / 不安全 deserialization / 硬编码密钥等 checklist；
   要求 reviewer 标 CVE/CWE 编号
5. **"Review the last change" 模板**（Aider / Continue 社区）—
   "只 review 自上次 commit 以来的变更"，避免大仓库 context overflow；
   必须用 `git diff` 而非 read 全文件

### 5.2 nightme 的 `/review` prompt 模板（v1 设计）

借鉴上面的"4 维度 + finding list + role prompt + 只看 diff"四个范式，写一个 nightme 自己的模板。设计原则（参考 `buildPRPrompt` / `buildAgentPrompt` 的 v2 教学）：

- **Tool floor 是 hard requirement**：reviewer 必须先跑 `git status` / `git log` / `git diff` 再写 finding
- **Finding list 是契约**：每条 finding 都有 `Severity / File:Line / Why / Fix`
- **Default focus = general**，按社区范式覆盖 4 维度；用户可 override
- **Anti-pattern 显式 list**：禁止"looks good"、"maybe consider..."等空话
- **Output 在 fenced block 内**：方便 `runtime` 渲染（如果将来加 IM-friendly card）

```go
// internal/command/review/prompt.go (新增)

// promptReviewV1 is the nightme-authored review prompt. Mirrors the
// design rationale of buildPRPrompt (gtw/pr.go:258-342) and
// buildAgentPrompt (gtw/commit.go:327+):
//
//   - hard MUST tool floor (reviewer must run git status / log / diff
//     before writing any finding);
//   - explicit Severity rubric (blocker / major / minor / nit) so the
//     LLM has a decision rubric instead of vibes;
//   - fenced output contract so the daemon can parse findings;
//   - "Do NOT" anti-pattern list banning modal-pattern noise
//     ("looks good", "maybe consider...", "might want to...").
//
// Why split the constant into multiple chunks: Go's const rules forbid
// cross-line concatenation in a single const literal. We work around
// this by building the prompt with strings.Builder at package
// init() — see promptReviewV1Build below.
const (
    promptReviewV1Header = `You are a staff engineer performing a code review.
Your job: surface real, actionable findings about the current state
of the working directory. The user wants a structured finding list,
not encouragement.

## Working directory

cwd: %s
branch: %s
recent commits (for orientation only — do NOT review them):
%s

## Before you write — tool floor (MANDATORY)

You MUST run and read the output of these commands BEFORE composing
any finding. Do NOT review from file names alone.

- `git status` — what's dirty vs committed.
- `git log -10 --oneline` — recent commit subjects.
- `git diff HEAD` (or `git diff <base>` when reviewing a feature branch) — the actual change. **Read every line** before writing findings.
- For a feature-branch review: also `git diff <base>...HEAD --stat` to see file footprint.
- For files outside the diff, ONLY review if they're loaded by the changed code (e.g. shared util, types).

## Focus

%s

If "general" (default), cover all four: correctness, security,
performance, maintainability. If the user named a focus, weight
that dimension (still mention blocker-severity issues in the
other three if you spot them).

## Output format — fenced finding list

Reply with ONE fenced markdown code block (triple backtick).
Inside the fence, one finding per block, separated by ---.
NO prose outside the fence (the daemon parses the fence; anything
outside is dropped).

For each finding:

[triple backtick fence]
[severity] <blocker|major|minor|nit>
file: <path>:<line or line-range>
dimension: <correctness|security|performance|maintainability>
why: <one sentence, why this is a real problem, not stylistic>
fix: <concrete code change; not "consider refactoring">
[triple backtick fence]

Severity rubric:
- blocker: will break, lose data, expose a secret, or fail in prod
- major:   wrong behavior in a realistic path; or security gap
- minor:   suboptimal but works; or clarity gap that costs review time
- nit:     style / naming / docs; fix if convenient, ignore otherwise

## Do NOT

- Do NOT include prose outside the fence.
- Do NOT use "looks good", "maybe consider...", "might want to..." —
  these are noise. If you can't articulate a concrete Why, drop the finding.
- Do NOT review files outside the diff unless they directly affect the change.
- Do NOT recommend "extract a helper" / "add a comment" as a fix unless you
  show what the helper / comment would say.
- Do NOT suggest changes that would break public API without flagging it
  as blocker + naming the consumer(s).
- Do NOT repeat the diff back in prose; reviewers already have the diff.

## Self-check (apply before submitting)

- Did every finding come from "git diff" output (or a file the diff loads)?
  If not, drop it.
- Is every "why" a one-sentence problem statement, not a paraphrase of the diff?
- Is every "fix" a code change, not a vibe?
- Are severities honest? (Most findings should be minor/nit; reserve
  blocker/major for things that genuinely break.)
- Is the finding count in [0, 20]? More than 20 means you're producing noise.
`
)
```

注意：上面这个伪 Go 代码块**只是**给人类读的 prompt 草稿。落地的真实 Go 实现需要把
prompt 文本放到一个 `.txt` 文件里（`internal/command/review/prompt.txt`），运行时用
`embed.FS` / `os.ReadFile` 读出来。这样 prompt 维护不需要改 Go 源码；具体目录
布局见 §11。

模板的几个**取舍说明**（给后续 review 时参考）：

- **强制 fenced block**：和 `buildPRPrompt` 一致（`internal/command/gtw/pr.go:262-273`）— nightme 端
  解析时只读 fence 内容，外面的 prose 截断掉
- **Finding list 而不是 paragraph 评分**：和 `buildAgentPrompt` / `buildPRPrompt` 一样，遵守
  "modal pattern 显式 ban"的反 modal 哲学（`internal/command/gtw/commit.go:309-317`）
- **Tool floor 是 hard MUST**：和 `buildPRPrompt` "self-check: body should be longer than git log"
  一样的"长度自检"思路（`internal/command/gtw/pr.go:286`）
- **Severity rubric 是显式清单**：让 LLM 有决策 rubric 而不是凭感觉

### 5.3 Focus 参数的处理

```
/review                  → focus = "general"（覆盖 4 维度）
/review security         → focus = "STRIDE-style security review: ..."
/review perf             → focus = "performance: ..."
/review api              → focus = "API consistency: ..."
/review correctness      → focus = "correctness only: ..."
```

每个 focus 都是一段**追加说明文本**，在 prompt 里替换 `%s` 的 focus 段。建议在 `prompt.go` 里维护一个 `focusRecipes map[string]string`。

---

## 6. `/review` 命令的 Factory 实现

### 6.1 命令注册

```go
// internal/command/review/cmd.go (新增)

package review

import (
    "context"
    "fmt"
    "strings"

    "github.com/cnlangzi/nightme/internal/agent"
    "github.com/cnlangzi/nightme/internal/chatsession"
    "github.com/cnlangzi/nightme/internal/command"
    "github.com/cnlangzi/nightme/internal/timeouts"
)

type Factory struct {
    mgr *chatsession.Manager
}

func init() {
    command.RegisterBuilder(func(d command.Deps) command.SlashCommandFactory {
        return NewFactory(d.Manager)
    })
}

func NewFactory(mgr *chatsession.Manager) *Factory {
    return &Factory{mgr: mgr}
}

func (f *Factory) Spec() command.Spec {
    return command.Spec{
        Name:     "review",
        Summary:  "Run a code review on the current workspace",
        Usage:    "/review [focus] | /review --one-shot [-a agent] [focus]",
        Category: "session",
    }
}

func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
    cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

    // 1. Preflight
    if cs == nil {
        return command.Reply(ctx, rt, "No active chat session."), nil
    }
    cwd, failOut := command.RequireActiveCwd(cs); if failOut != nil {
        return failOut, nil
    }

    // 2. Parse args (mirror gtw commitArgs / prArgs shape)
    args, err := parseReviewArgs(input.Args[1:])
    if err != nil {
        return command.Reply(ctx, rt, err.Error()), nil
    }
    focus := args.Focus
    if focus == "" {
        focus = "general"
    }

    // 3. Resolve adapter for the SelectedAgent
    agentName := cs.SelectedAgent()
    if agentName == "" {
        agentName = rt.Config.Primary
    }
    adapter := ResolveAdapter(agentName)

    // 4. Build the structured blocks
    blocks, err := adapter.BuildBlocks(focus, cwd)
    if err != nil {
        return command.Reply(ctx, rt,
            fmt.Sprintf("❌ review adapter failed: %v", err)), nil
    }

    // 4b. Native-mode follow-up: when the adapter advertises native
    // /review but doesn't take focus as a parameter (claude / codex),
    // we still want to honor the user's focus. Strategy: queue the
    // /review as one MessageKindQueue batch, then queue the focus
    // follow-up as a SECOND MessageKindQueue batch. Both go in order;
    // the runtime's flush path treats them as two separate turns
    // (review → review narrows in on focus).
    //
    // opencode takes focus as /review <focus> directly, so its
    // adapter returns follow-up "" and we skip this.
    var followup string
    if adapter.SupportsNative() {
        followup = adapter.NativeReviewFollowup(focus)
    }

    // 5. Dispatch on path
    if args.OneShot {
        // Path B: RunOnce — independent reviewer, no chat pollution.
        // RunOnce only takes ONE blocks set; follow-up is not
        // supported in this path (review is a single turn). Native
        // adapters whose /review takes focus inline still work
        // because their BuildBlocks already inlined focus.
        return f.runOneShot(ctx, rt, cs, args.Agent, cwd, blocks)
    }
    // Path A: QueueUserMessage — chat内 review，保留上下文
    return f.runInChat(ctx, rt, cs, input.ChatID, input.MessageID, blocks, followup)
}

func (f *Factory) runInChat(ctx context.Context, rt command.RuntimeServices,
    cs *chatsession.ChatSession, chatID, messageID string, blocks []agent.ContentBlock, followup string) (*command.SlashOutput, error) {
    msg := chatsession.Message{
        ID:         messageID,
        ChatID:     chatID, // Mirror steer.go's pattern; cs doesn't expose ChatID().
        Blocks:     blocks,
        Kind:       chatsession.MessageKindQueue, // barrier: stand-alone batch
        // (review should not be merged with adjacent user messages;
        // it IS the prompt for the next turn)
    }
    if err := cs.QueueUserMessage(msg); err != nil {
        return command.Reply(ctx, rt,
            fmt.Sprintf("❌ review queue failed: %v", err)), nil
    }

    // Native-mode follow-up (claude / codex). PushFront so the
    // follow-up lands AFTER the review in the queue; both are
    // MessageKindQueue barriers so they run as two distinct turns.
    //
    // Why PushFront (not Push): we want the follow-up to be the
    // NEXT thing the agent sees after the review completes, not
    // mixed in with whatever else the user typed meanwhile.
    // PushFront on an empty post-review queue = same as Push, but
    // the semantics are clearer.
    if followup != "" {
        // Use a synthetic ID derived from the original review
        // messageID so receipt anchoring groups them visually as
        // "the review turn + its focus refinement".
        followupMsg := chatsession.Message{
            ID:     messageID + "+followup",
            ChatID: chatID,
            Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: followup}},
            Kind:   chatsession.MessageKindQueue,
        }
        if err := cs.SteerUserMessage(followupMsg); err != nil {
            // PushFront failed (queue full / etc.). The review itself
            // already ran — surface this as a soft warning, not a
            // fatal error. The user can re-send the focus manually.
            return command.Reply(ctx, rt,
                fmt.Sprintf("🔍 Review queued, but focus follow-up failed: %v. "+
                    "Send `/review %s` again to retry just the focus.", err, msg.Blocks[0].Text)), nil
        }
    }

    return command.Reply(ctx, rt, "🔍 Review queued. The agent will report back in this thread."), nil
}

func (f *Factory) runOneShot(ctx context.Context, rt command.RuntimeServices,
    cs *chatsession.ChatSession, overrideAgent, cwd string, blocks []agent.ContentBlock) (*command.SlashOutput, error) {
    // Resolve agent (override > yml > SelectedAgent)
    agentName := overrideAgent
    if agentName == "" {
        agentName = cs.SelectedAgent()
    }
    if agentName == "" {
        agentName = rt.Config.Primary
    }
    a, err := agent.Builtins.Get(agentName)
    if err != nil {
        return command.Reply(ctx, rt, fmt.Sprintf("❌ unknown agent %q", agentName)), nil
    }
    if err := a.Detect(); err != nil {
        return command.Reply(ctx, rt, fmt.Sprintf("❌ agent %s unavailable: %v", agentName, err)), nil
    }
    ctx, cancel := context.WithTimeout(ctx, timeouts.Agent)
    defer cancel()
    res, err := a.RunOnce(ctx, agent.StartConfig{Workspace: cwd}, blocks)
    if err != nil {
        return command.Reply(ctx, rt, fmt.Sprintf("❌ review failed: %v", err)), nil
    }
    // Forward RunResult through replyAgent-equivalent path so the
    // footer (agentbar / usagebar) renders — same pattern as
    // /gtw commit / /gtw pr.
    return replyReviewResult(ctx, cs, res, agentName)
}
```

### 6.2 `parseReviewArgs` 的 argv 解析

设计成和 `gtw/pr.go::parsePRArgs` 对齐的结构：

```go
type reviewArgs struct {
    OneShot bool   // --one-shot
    Agent   string // -a <name> / --agent <name>
    Focus   string // remaining positional args joined by space
}
```

支持 `/review` / `/review security` / `/review --one-shot -a claude security` / `/review -a opencode` 等。

### 6.3 Path A 的关键点：`MessageKindQueue` barrier + native follow-up 排队

为什么用 `MessageKindQueue` 而不是默认的 `MessageKindNormal`：

- review prompt 必须独立成一个 turn；它**不应该**和用户后续在 buffer 里的
  普通消息合并成一个 batch 推给 agent
- 否则场景：用户在 review 还没跑完时连发 "yes do it" 和 "also fix X"，两条会被合并，
  review prompt 就被稀释了
- 同样适用于用户先 `/review` 紧接着发普通消息的场景：review 应该先跑完

**Native mode 的 follow-up**：

claude / codex 的 `/review` 不接受参数，所以用户输入的 `focus`（比如 `security`）会通过
`adapter.NativeReviewFollowup(focus)` 翻译成一段**追问文本**，作为**第二条 MessageKindQueue
消息**排进队列（在 review 之后）。runtime 的 FlushHook 把 review 和 follow-up 当成两个
独立 turn 处理：

```
turn 1: /review                              → agent 跑 PR-style review
turn 2: "Now narrow your review to security" → agent 在已有 review 上细化
```

两条都标 `MessageKindQueue` barrier，所以：

1. 不会被用户后续消息合并稀释
2. review 没跑完时 follow-up 不会先跑
3. follow-up 跑完前用户消息不会插队

**Path B 不支持 follow-up**：`RunOnce` 是单轮 side-effect，没法排队第二条 message。
native + follow-up 的组合在 `--one-shot` 下退化为"用 opencode 风格的 inline focus"
（因为只有 opencode 的 `/review` 接受参数）；claude / codex + `--one-shot -a claude security`
的 follow-up 会**丢**，命令层面 reply 时加一句 hint："focus refinement only works in chat mode"。

### 6.4 Path B 的"输出回写"

### 6.4 Path B 的"输出回写"

Path B 用 `RunOnce`，review 结果是 `agent.RunResult{Text, Model, SessionID, Usage}`，需要走
gtw 的 `replyAgent` 同样路径把它打到 receipt card 上：

- 复用 `internal/command/gtw/agent_reply.go::replyAgent` 是最干净的选择
- 但这会引入 `command/review` → `command/gtw` 的循环依赖（gtw 已经 import command）
- **解决方案**：把 `replyAgent` 上提到 `internal/command/replyagent.go`（cmd 包的 internal helper），
  或者抽出一个 `command/cmdhelpers/reply` 子包；gtw 和 review 都 import 它

这是个**真正的小重构**，需要在 PR 描述里单独讨论（"抬 replyAgent 出 gtw"）。

---

## 7. 错误处理 / 边界

| 场景 | 行为 |
|------|------|
| ChatSession 不存在 | `"No active chat session."`（同其他命令） |
| SelectedCwd 空 | `"Send /cwd <path> first."` |
| Agent binary 缺失（Path B） | `"❌ agent <name> unavailable: <err>"` |
| 队列满（Path A） | 走 `cs.queue.Push` 的 `ErrQueueFull` 路径（不变式保留） |
| Adapter 自己 BuildBlocks 失败 | adapter 在生成 prompt 前要 read git 元信息；如果 git 读不出来，adapter 返回 err，命令回复 `❌ review adapter failed: ...` |
| Focus 不在 `focusRecipes` 里 | fallback 到 `"general"` 的 recipe |

---

## 8. 测试矩阵

| Case | 路径 | 期望 |
|------|------|------|
| `/review` (chat 无 AS) | A | spawn AS + push review message → AS 收到 prompt 块 |
| `/review security` | A | prompt 含 security focus 段落 |
| `/review --one-shot` | B | RunOnce 起独立 reviewer，输出走 receipt card |
| `/review --one-shot -a opencode` (假设 opencode 注册了 native adapter) | B | adapter 用 native path（不生成 prompt block） |
| `/review --one-shot -a claude` | B | prompt adapter 跑；reviewer 是 claude |
| Review 时用户连发 "yes" | A | review 单独跑成 1 turn；后续消息排队 |
| 队列满时 `/review` | A | ErrQueueFull → 命令层面 reply 错误 |
| `/review` 在没设 cwd 的 chat | A or B | "Send /cwd first" |

---

## 9. 落地分阶段

### Phase 1（最小可用 / native 路径全覆盖）

- [ ] `internal/command/review/` 包骨架：`cmd.go` + `args.go` + `adapter.go`
- [ ] **三个 native adapter**：`ClaudeCodeAdapter` / `OpenCodeAdapter` / `CodexAdapter`（覆盖主流 agent 的 `/review` 转发 + focus follow-up 排队）
- [ ] `PromptAdapter`（fallback；处理 Pi / dsh / pty / 未知 agent）
- [ ] `focus_recipes.txt`（`embed.FS`）
- [ ] Path A 完整：chat 内 review + native follow-up
- [ ] 测试：`cmd_test.go` + `adapter_test.go` + live test 用 claude / codex / opencode 各跑一次 `/review` 和 `/review security`

### Phase 2（one-shot 路径 + prompt adapter 完整模板）

- [ ] 抬 `replyAgent` 出 gtw 到 `internal/command/cmdhelpers/reply/`
- [ ] Path B 实现 + 测试
- [ ] `prompt.txt`（nightme 自带的高质量 review prompt 模板，§5.2 那段）
- [ ] `nightme.example.yaml` 加 `review:` 配置节（focus recipes / 默认 timeout）

### Phase 3（adapter 微调 / 体验改进）

- [ ] **检测 OpenCode 实际是否 advertise 了 `/review`**（opencode < 1.18 不 advertise，要 fallback；新版会自动启用 native）
- [ ] **检测 Claude Code / Codex 版本**：`/review` 是 2025 H2 之后才有的内置命令，老版本要 fallback 到 prompt adapter
  — `agent.Info()` / 系统 prompt 没暴露版本号；考虑从 `claude --version` / `codex --version` 启动时 parse 后存到 adapter factory
- [ ] 给 prompt adapter 加 `--strict` flag（强制 finding 必须在 fenced block 内，否则命令层面 retry 一次）

### Phase 4（gtw 集成）

- [ ] `/gtw fix` flow 在 PR 前自动跑一次 `/review --one-shot -a claude`（指定更便宜的 reviewer）
- [ ] review 结果写到 PR body 的 "Reviewer notes" 段（如果当前 PR 没 blocking finding）
- [ ] 如果 review 找到 blocking finding，gtw 把 issue label 从 `nightme/ready` 转回 `nightme/revise`

---

## 10. 风险 / 开放问题

1. **路径 A 的"破坏 chat 上下文"风险**：如果用户跑 `/review` 之后接着改 code，agent
   会把 review prompt 当成"系统 prompt" 一直 retain 着。要不要 review 完自动 `/new` 清一下？
   *建议：先不自动，文档说明 + `/new` 显式建议*。
2. **Path B 的 RunOnce 不走 receipt card 的 chat_id 路由**：review 输出的 receipt
   card 的 `ReplyTo` 该锚到哪条 user message？建议锚到 `/review` 这条 slash command
   的 `input.MessageID`（和 `/gtw commit` 锚到原 `/gtw commit` user msg 一致）。
3. **Prompt 模板的回归风险**：参考 `buildAgentPrompt` / `buildPRPrompt` 的 v2 修正（`commit.go:262-326`
   "F-56 §3"），review prompt 也会有 modal pattern 风险；需要 ship 之前用真实 agent
   （Claude / OpenCode / Codex 各一个）跑 5 个真实 case，看 finding list 是否真的可
   act upon 而不是 "looks good"。
4. **OpenCode native adapter 何时启用**：opencode 1.18+ 才 advertise available commands，
   1.18 之前要 fallback 到 prompt adapter。`ResolveAdapter` 要做 version check 吗？
   *建议：先不 check；命令 advertise 没出来就走 prompt adapter；之后 opencode 升级
   自动生效*。
5. **`/review` 和 `/gtw fix` 的交互**：gtw fix 创建 worktree 后要不要立刻 auto-review？
   *建议：Phase 4 才考虑；Phase 1~3 不耦合*。

---

## 11. 参考：现有文件 / 改动清单

| 路径 | 性质 |
|------|------|
| `internal/command/review/cmd.go` | 新增（Factory + Handle + runInChat + runOneShot） |
| `internal/command/review/args.go` | 新增（reviewArgs + parseReviewArgs） |
| `internal/command/review/adapter.go` | 新增（Adapter interface + RegisterAdapter + ResolveAdapter） |
| `internal/command/review/adapter_prompt.go` | 新增（fallback prompt adapter；走 embed 读 prompt.txt） |
| `internal/command/review/adapter_claudecode.go` | 新增（Phase 1；native `/review` 转发 + focus follow-up） |
| `internal/command/review/adapter_opencode.go` | 新增（Phase 1；native `/review [focus]` 转发） |
| `internal/command/review/adapter_codex.go` | 新增（Phase 1；native `/review` 转发 + focus follow-up） |
| `internal/command/review/prompt.txt` | 新增（Phase 2；prompt 模板本体；用 embed.FS 嵌入） |
| `internal/command/review/focus_recipes.txt` | 新增（Phase 1；focus → recipe 文本；embed.FS） |
| `internal/command/review/cmd_test.go` | 新增（argv 解析 + adapter 选择 + focus follow-up 排队） |
| `internal/command/review/adapter_test.go` | 新增（每个 adapter 的 BuildBlocks / NativeReviewFollowup 单测） |
| `internal/command/cmdhelpers/reply/reply.go` | 新增（Phase 2；抬出 gtw 的 replyAgent） |
| `internal/command/gtw/agent_reply.go` | 修改：Phase 2；删 replyAgent；改 import cmdhelpers/reply |
| `docs/feat/F-review-slash-command.md` | **本文档** |
| `configs/nightme.example.yaml` | 修改：加 `review:` 节（Phase 2） |

**为什么 prompt 用 `.txt` 而不是 Go const**：模板里有反引号、emoji、`---` 分隔符、markdown
等多类字符；写成 Go raw string literal 会让 diff 看起来像在改 Go 而不是改 prompt。
落到文件 + `embed.FS` 后维护者可以直接用 `vi` / GitHub web editor 改 prompt，diff
干净；`buildAgentPrompt` / `buildPRPrompt` 是历史包袱，等下次清理时也建议迁出来。

---

## 12. 一句话总结

> **`/review [focus]`：底层用 `Adapter` 把 review 按 agent 形态分发——Claude Code / OpenCode / Codex 三个都**有原生 `/review`**（OpenCode 还接受参数，Claude / Codex 不接受），native 是主路径；Pi / dsh / pty / 未知 agent fallback 到 nightme 自带的 prompt adapter。Focus 处理分两种：OpenCode 走 inline（`/review security`），Claude / Codex 走"review 完后追加追问 message"。执行路径默认 chat 内（保留上下文），`--one-shot` 走 `RunOnce`（独立 reviewer，但不支持 follow-up）。prompt adapter 的模板借鉴社区 4 范式（Copilot 4 维度 / "读 diff" / Staff Engineer role / STRIDE security），强制 tool floor + fenced finding list + 反 modal pattern。**