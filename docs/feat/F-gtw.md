# /gtw 命令 (fix / push / pr)

> **子命令设计索引**：[`F-gtw-fix.md`](./F-gtw-fix.md)（`/gtw fix`）、下文 A1/A2（push/readiness）、A4（`/gtw pr`）

## A1. F-56: `/gtw push` 三分支流 + Agent 最小化 Prompt

> **Source**: `F-gtw.md`


> **Depends on**: F-104 (`/gtw push` 3 档 agent 优先级 + yml `~/.nightme/gtw.yml`)、F-120 (PR #120 `fix(gtw): harden push verification and PR cache correctness` — 本次新增的 HEAD-advance check)、`internal/command/gtw/push.go::headSHA`、`internal/command/gtw/push.go::countUnpushed`、`internal/command/gtw/push.go::listUncommittedFiles`

> **Related**: `internal/command/gtw/README.md`（gtw 回复排版规约）、[`internal/command/gtw/push.go`](../../internal/command/gtw/push.go)（agent 优先级 + 3 档 chain）、PR #120 commit `77a8023`（HEAD-advance check 的来源）

---

## 0. TL;DR

`/gtw push` 是 **push-only** 的 dispatcher:它不 spawn agent,由一份 readiness snapshot 决定"推"还是"没什么可推"。

1. **空操作分支**:worktree 干净 + 上游存在 + ahead=0 → 提示用户,退出。
2. **直推分支**:其余情况(首次 push / ahead>0)→ 工程化 push,agent 不参与。

原本落在 push 里的 "dirty → agent → commit → push" 那一段独立成了 `/gtw commit`:agent 只负责把 working tree 的改动拆成若干条 commit,nightme 校验 commit 落地,然后停在本地;push 由用户随后单独跑 `/gtw push`。push 撞到脏工作区直接拒掉并指向 `/gtw commit`。

Agent 的职责是 **"把当前 working tree 的改动按相关性拆成若干条 commit"**。Push 决策、push 执行、push 失败兜底、IM 卡片拼装,全部由 nightme 工程化代码根据 git 状态完成 —— 不读 agent 的 prose 输出作为 ground truth。

verification 不做 SHA-parse 比对:agent 不 claim SHA,commit 列表一律从 `git log` 拿。

---

## 1. Motivation

### 1.1 当前的 false-success bug

PR #120 (commit `77a8023`) 加的 HEAD-advance check 已经能兜底一种 false-success —— agent 没真 commit 但 worktree 变 clean 的场景。但 IM 卡片里仍然**包含 agent 的 prose**,这条 prose 在那天 case 里就是:

```
94c4a38 docs: add chat input routing and shell mode documentation to README
```

—— 这个 SHA 跟 HEAD before **字面上撞上同一前缀**(`94c4a38`)。说明 agent 把 "现有的 HEAD SHA" 包装成"我刚 commit 的"。HEAD-advance check 抓到了,但 UX 角度:

- agent 既然可以**撒谎报 SHA**,它也可以**撒谎报"已 push"**(今天 prompt 让它做这件事)
- prompt 里 step 5 的输出格式 "Reply with: <commit_hash> <one-line summary>" 给了 LLM 一个**显式撒谎的口子**

根因不是 HEAD-advance check 不够强,是 **prompt 设计给了 agent 太多它不该负责的事**(commit + push + 报告),且 **nightme 把 agent 的 prose 当成 source of truth**。

### 1.2 agent 职责过载的副作用

prompt(`internal/command/gtw/commit.go::buildAgentPrompt`)只让 agent 做两件事:读 diff、决定 commit 边界、写 Conventional Commits message。message 之外的所有事 —— push、PR、跑测试、改文件、回滚历史 —— 都由 nightme 工程化代码负责。

把副作用一起交给 agent 时,LLM 会在边界外产生副作用:

| 失败模式 | 谁的责任 | 现在的兜底 |
|---|---|---|
| commit message 格式写错 | LLM | nightme 不解析 message 格式,用户自己改 |
| commit hook 失败但 LLM 吞了 stderr | LLM | `verifyAgentCommitted` 兜底(HEAD 没动 → 拒) |
| commit 失败 → LLM 跑 `git restore` 想"重置" | LLM(**静默吞用户改动**) | prompt 的 denylist 禁 revert / restore / stash |
| push 失败 → LLM 假装 push 成功 | LLM | prompt 禁 `git push`;push 由 `/gtw push` 单独跑 |
| LLM 把现有 SHA 当新 SHA 报 | LLM | prompt 不要求报 SHA;卡片从 `git log` 拼 |

只有第 1 类真正需要 LLM 判断,其余几类都是 LLM 在边界外的副作用 —— 最好的"修复"是 **不把那些事交给 LLM**。

### 1.3 三个根本性反思

提炼出 3 条原则:

1. **Agent = "需要 judgment 的事",nightme = "infra 副作用的事"**
   - LLM 强项:看 diff、定 commit 边界、写 message
   - LLM 弱项:跑网络操作、判断 push 是否成功、稳定回报状态
   - 因此:agent 只 commit,nightme 自己 push
2. **Git = source of truth,不是 agent 的 prose**
   - LLM 可以 confabulate,但 `git log` 不会
   - 因此:nightme 从 `git log` 拿真实 commit 列表,agent 说什么无所谓
3. **Prompt = "如果 agent 是一个独立 CLI 工具,这个 prompt 就是它接收的命令"**
   - 不该让 agent 替调用方做副作用(push / PR / 跑测试 / 改文件 / 回滚历史)
   - 因此:prompt 里只有"读 diff → 定 commit 边界 → 写 message → commit",收尾只要求一行一 commit 的简短确认,成败判定全在 nightme 侧

---

## 2. The Branches

`dispatchPush`(`internal/command/gtw/push_dispatch.go`)启动时拍一张 readiness 快照:

```go
snap, err := CollectReadinessForDispatch(ctx, c.Worktree, deps.Git)
```

| 分支 | 条件 | 动作 | Agent? |
|---|---|---|---|
| **no-op** | `snap.HasNothingToPush()` | 提示用户"无活儿干",退出 | ❌ |
| **push** | 其余(首次 push / ahead>0) | `programmaticPushWithRetry` → 从 `git log` 拼卡片 | ❌ |

push 之前还有三道硬拒:

| 条件 | 回复 |
|---|---|
| `snap.PushBlockReason() != ""` | `❌ worktree has unmerged paths (merge/rebase conflict)` |
| `snap.Branch == ""` | `❌ detached HEAD — checkout a named branch first` |
| `!snap.WorkingTreeIsClean()` | `❌ worktree is dirty — /gtw push no longer auto-commits` |

### 2.1 no-op

```go
if snap.HasNothingToPush() {
    card, out := pushFallback(ctx, c, snap.Branch, deps)
    return replyOutcome(ctx, cs.Emitter(), chatID, messageID, card, out), nil
}
```

`pushFallback`(`internal/command/gtw/push.go`)是二次判定:远端已经握着本地全部 commit 时回 `✅ nothing to push — <branch> is already on the remote`;远端还缺 commit 时回 `❌ nothing to push`。

### 2.2 push

```go
originBefore, err := originBranchSHA(ctx, c.Worktree, c.Branch, deps)
if err != nil { ... }

if err := programmaticPushWithRetry(ctx, deps, c); err != nil {
    return replyOutcome(ctx, cs.Emitter(), chatID, messageID, err.Error(), ...), nil
}

revRange := originBefore + "..origin/" + c.Branch   // 首次 push 时走 firstPushRevRange
card, commitCount, err := replyPushCard(ctx, c, revRange, deps)
```

**`programmaticPushWithRetry`**:工程化 push,带 retry(`internal/command/gtw/push.go`)。内部用 `unpushedIsZero` 做 push 后二次验证。

**`replyPushCard`**:从 git log 拼 IM 卡片,跟 agent 输出完全无关(`internal/command/gtw/render.go`)。

**rev range 的坑**:左端必须用 push **之前**的 `origin/<branch>` tip(`originBranchSHA`),不能用本地 HEAD —— push 成功后 `origin/<branch> == 本地 tip`,`headBefore..origin/<branch>` 会塌成空区间,卡片就会谎报"pushed 0 commit(s)"。首次 push 时 `originBefore` 为空,改走 `firstPushRevRange`。

### 2.3 agent 那一条路在 `/gtw commit`

worktree 脏的时候 `/gtw push` 不再替用户 commit,而是拒掉并指向 `/gtw commit`:

```
❌ worktree is dirty — /gtw push no longer auto-commits
hint: run `/gtw commit` first, then `/gtw push`
```

`/gtw commit`(`internal/command/gtw/commit.go::dispatchCommit`)拥有原来的 "dirty → agent → commit" 那一段:跑 agent → `verifyAgentCommitted` → re-snapshot → 从 `git log headBefore..HEAD` 拼 commit 卡片,**不 push**。

### 2.4 状态机图

```
              ┌──────────────────────────┐
              │   dispatchPush entry     │
              │   snapshot (readiness)   │
              └────────────┬─────────────┘
                           │
              ┌────────────┼────────────┐
              │            │            │
              ▼            ▼            ▼
        PushBlockReason  Branch=""   !WorkingTree
        (conflict)      (detached)  IsClean (dirty)
              │            │            │
           ❌ 拒         ❌ 拒       ❌ 拒 + 指向
                                         /gtw commit
                           │
                           ▼
                  snap.HasNothingToPush()
                    │            │
                   true         false
                    │            │
                    ▼            ▼
               pushFallback   originBefore = originBranchSHA
                    │            │
                    ▼            ▼
                ✨ no-op    programmaticPushWithRetry
                                 │            │
                              FAIL         OK
                                 │            │
                                 ▼            ▼
                           ❌ push 失败   replyPushCard
                             (列出本地)    (从 git log 拼)
                                            │         │
                                         count==0   count>0
                                            │         │
                                            ▼         ▼
                                        pushFallback  ✅ 成功卡
```

---

## 3. Agent Prompt

`/gtw commit` 的 prompt 由 `internal/command/gtw/commit.go::buildAgentPrompt(c, lang)` 渲染。commit subject / body 用配置的输出语言写,Conventional Commits 的 type 关键词和 `Issue: #<id>` trailer 保持英文。

### 3.1 prompt 骨架

```
You are a release engineer. Stage the current local branch's uncommitted
changes into one or more local commits following Conventional Commits on
that branch. Push and PR creation are handled by separate steps; you
ONLY commit.

Branch: <branch>
Worktree: <worktree>
[Issue: #<N>]

## Allowed operations (strict allowlist)
## Workflow
## Commit message rules
## Final reply
```

### 3.2 allowlist:agent 只允许跑这 4 条 git 命令

| 命令 | 用途 |
|---|---|
| `git status` | 读 porcelain |
| `git diff` / `git diff --staged` / `git log` | 读内容 |
| `git add <specific files>` | **整文件** stage(不是 `git add -A`,不是 hunk 级) |
| `git commit -m "<subject>" [-m "<body>"]` | 建 commit |

Workflow 段只要求跑一次 `git status` + `git diff`(已经 staged 过就再补一次 `git diff --staged`),然后按逻辑意图定 commit 边界 —— **一个内聚的 commit 好过硬拆成两个**。

### 3.3 denylist:agent 明确不许做的 9 件事

- 改 / 建 / 删工作区里的任何文件
- `git apply`、`git apply --cached` 或任何 patch / hunk 级 surgery
- 跑测试、构建、linter、formatter 或任何其他工具
- 自查自己的产出(wrapper 在 agent 退出后跑 branch / clean / HEAD-advance 三道检查)
- `git push`
- amend / rebase / reset 或任何改历史、改 HEAD 的操作
- checkout 到别的分支
- revert / restore / stash 用户的工作
- `git add -A` / `git add .`

allowlist 和 denylist 两层都要:allowlist 单独留着有洞(比如"不跑测试"这条,LLM 仍能通过 Bash 工具绕),denylist 单独留着又给 LLM 留了"发明新绕法"的空间。

### 3.4 commit message 规则

- subject ≤72 chars、imperative mood、不带结尾句号
- `fix:` / `feat:` 必须带 1-3 段 body 说明 **WHY**;`chore:` body 可选
- subject-only 的 commit 只留给不重要的事
- 有 issue 时 body 末尾单独一行 `Issue: #<id>`

### 3.5 收尾契约

wrapper 从 `git log headBefore..HEAD` 渲染成功卡,agent 的文字回复只当简短确认:一行一个 commit(`<hash> <subject>`),没有 commit 就回 `(no commits)`。不允许 agent 复述自己的过程。

### 3.6 通用 prompt 设计原则

1. **Agent 的 prompt 应该写成"如果它是一个独立 CLI 工具,这就是它接收的命令"**。可移植、不带调用方上下文
2. **Agent 不该知道"谁"在调用它**。verification / retry / 卡片渲染 是 nightme 自己的事
3. **每个约束都该回答"agent 不知道这条会犯什么错"**。否则就删

---

## 4. Verification Chain

### 4.1 `verifyAgentCommitted`

`/gtw commit` 在 agent 跑完之后做三道只读校验(branch → worktree clean → HEAD advance),任何一道不过都直接拒,不 push。

```go
func verifyAgentCommitted(ctx, deps, c, headBefore string) string {
    // (1) branch:agent 必须还在 c.Branch 上
    branchAfter, err := currentBranch(ctx, c.Worktree, deps)
    if err != nil { return "⚠️ agent finished but failed to read current branch: ..." }
    if branchAfter != c.Branch {
        return "⚠️ agent finished but HEAD is on %q, expected %q.\n" + ...
    }

    // (2) worktree 必须干净
    uncommitted, err := listUncommittedFiles(ctx, c.Worktree, deps)
    if err != nil { return "⚠️ agent finished but failed to verify clean state: ..." }
    if len(uncommitted) > 0 {
        return "⚠️ agent finished but %d file(s) are still uncommitted in %s:\n" + ...
    }

    // (3) HEAD 必须动
    headAfter, err := headSHA(ctx, c.Worktree, deps)
    if err != nil { return "⚠️ agent finished but failed to read HEAD: ..." }
    if headAfter == headBefore {
        return "⚠️ agent finished but no new commit was created (HEAD unchanged at %s).\n" + ...
    }
    return ""
}
```

`headBefore` 是必填参数:拿不到就直接返回内部错误,而不是跳过 HEAD-advance 检查 —— 那正是 false-success 类要抓的东西,不能被静默关掉。

只校验"agent 真的 commit 了",**不**校验 push(push 是 `/gtw push` 自己的事)。

### 4.2 为什么不需要 SHA-parse 增强

设计草稿考虑过:解析 agent reply 里的 `<sha>` claim,跟 headAfter 比对。**这个增强在当前设计下取消**,原因:

- 设计:agent claim 3 条 SHA → 必须 parse 核对
- 新设计:agent **不 claim** 任何 SHA → `git log headBefore..HEAD` 是 ground truth → 没法不一致

LLM 没法靠少报 / 错报 SHA 来骗系统,因为 **agent 根本不被要求报 SHA**。

### 4.3 `programmaticPushWithRetry`

`/gtw push` 的唯一 push 入口(`internal/command/gtw/push.go`)。`git push -u origin <branch>` 最多重试一次:

```go
func programmaticPushWithRetry(ctx, deps, c) error {
    // 1. git push -u origin <branch>
    //    push 本身报错 → 直接把真实错误透传(不重试:auth / 保护规则 / 网络类错误重试不会自愈)
    // 2. unpushedIsZero 验真
    //    不为 0 → 重试一次
    // 3. 还不为 0 → 报错,附本地未推 commit 列表
}
```

判据是 `countUnpushed`(不是 push 命令的 exit code)—— git 可以 exit 0 但 commit 没真上去,也可以 exit non-zero 但分支本来就是 up to date。

---

## 5. IM Card 格式

### 5.1 no-op

远端已经握着本地全部 commit:

```
✅ nothing to push — docs-readme is already on the remote
```

远端还缺 commit:

```
❌ nothing to push
  docs-readme still carries commits the remote does not have
```

两条都出自 `pushFallback`(`internal/command/gtw/push.go`)。

### 5.2 push 成功

```
✅ pushed 3 commit(s) to docs-readme:
  94c4a38 docs: add chat input routing and shell mode documentation to README
  77a8023 fix(gtw): harden push verification and PR cache correctness
  459b968 docs(README): clarify session management commands, drop stale gtw hook
> docs-readme
```

emoji `✅`(单步完成)。commit 列表来自 `git log originBefore..origin/<branch>`,不是 agent prose。

### 5.3 commit 成功(`/gtw commit`)

```
🤖 pi committed 2 change(s) on docs-readme:
  aabbcc1 feat(gtw): split verification into 3 branches
  ddeeff2 fix(gtw): cancel push on agent failure
> docs-readme
```

emoji `🤖`(agent 主导)。commit 列表来自 `git log headBefore..HEAD`。

### 5.4 verify 失败(`/gtw commit`,agent 没 commit 全)

```
⚠️ agent finished but 2 file(s) are still uncommitted in <worktree>:
  new-secret.env
  scratch.txt

💡 hint: commit them manually, or re-run /gtw commit to retry.
```

不 push,直接退出。emoji `⚠️`(警告 / 部分成功)。

### 5.5 push 失败

```
❌ git push failed: <error>
hint: check auth (`gh auth status`), remote protection rules, or network.
raw output:
<git 的原始输出>
```

重试之后仍未推上去时,错误里还带 **本地但没推上去** 的 commit 列表。emoji `❌`(终态失败)。

### 5.6 emoji 一致性

gtw 的 emoji 来自 `internal/command/gtw/README.md §1.1` 的闭集:

| emoji | 含义 | 适用 case |
|---|---|---|
| `✅` | 单步动作完成 | push 成功 / 已经在远端 |
| `🤖` | agent 主导 | `/gtw commit` 成功 |
| `⚠️` | 警告 / 部分成功 | verify 失败 |
| `❌` | 终态失败 | push 失败 / nothing to push / nothing to commit |

---

## 6. 不变式 Checklist

- **Agent 不 push**。prompt 的 denylist 显式禁 `git push`;push 由 `/gtw push` 单独跑,跟 agent 行为完全解耦
- **Agent 不报成败**。prompt 不要求 agent 汇报 commit 成没成;agent 的输出几乎完全被忽略,成败由 git 状态判定
- **Git 是 source of truth**。IM 卡片、commit 列表、push 状态全部从 `git log` / `git rev-parse` 拿,不读 agent prose
- **HEAD 校验必经**。`/gtw commit` 必跑 `verifyAgentCommitted`,任一 case 失败 → 拒
- **Push 校验必经**。`programmaticPushWithRetry` 内部 `countUnpushed` 兜底,失败 → IM 报错并列出本地未推 commit
- **no-op 不能误吞**。无活儿干时必须明确告诉用户,不能假装"推了 0 条 commit"
- **Conflict 永远先报**。`snap.PushBlockReason()` 是三道硬拒里的第一道

---

## 7. 错误处理矩阵

### 7.1 `/gtw push`

| 触发条件 | IM 输出 | 后续动作 |
|---|---|---|
| readiness 快照读不出来 | `❌ read worktree status: <err>` | 退出 |
| 不是 git 仓库 / 空仓库 | `❌ cannot read worktree git status — refusing to push` | 退出 |
| Conflict 存在 | `❌ worktree has unmerged paths (merge/rebase conflict)` | 退出 |
| Detached HEAD | `❌ detached HEAD — checkout a named branch first` | 退出 |
| Worktree 脏 | `❌ worktree is dirty — /gtw push no longer auto-commits` + 指向 `/gtw commit` | 退出(不 push) |
| 无活儿干 | `✅ nothing to push — <branch> is already on the remote` / `❌ nothing to push` | 退出 |
| `programmaticPushWithRetry` 失败 | `❌ git push failed: <err>`(+ 本地未推 commit 列表) | 退出(commit 在本地) |
| push 落地但卡片渲染失败 | `❌ push succeeded but couldn't render card: <err>` | 退出 |
| 成功 | `✅ pushed N commit(s) to <branch>: ...` | 退出 |

### 7.2 `/gtw commit`

| 触发条件 | IM 输出 | 后续动作 |
|---|---|---|
| readiness 快照读不出来 | `❌ read worktree status: <err>` | 退出 |
| 不是 git 仓库 / 空仓库 | `❌ cannot read worktree git status — refusing to commit` | 退出 |
| Conflict 存在 | `❌ worktree has unmerged paths (merge/rebase conflict)` | 退出 |
| Detached HEAD | `❌ detached HEAD — checkout a named branch first` | 退出 |
| Worktree 已干净 | `✅ nothing to commit — the work was already committed` / `❌ nothing to commit` | 退出 |
| HEAD 读不出来 | `❌ read HEAD: <err>` | 退出 |
| 无 agent 选中 | `❌ no agent selected. Send \`/use <name>\` first or pass \`-a <name>\`.` | 退出(不 commit) |
| agent 未注册 | `❌ unknown agent "<name>" (check \`nightme agents\` or your config)` | 退出(不 commit) |
| agent 二进制不在 PATH | `❌ agent <name> not available: <err>` | 退出(不 commit) |
| agent 跑完换了分支 | `⚠️ agent finished but HEAD is on <b>, expected <b>` | 退出 |
| agent 跑完 worktree 仍 dirty | `⚠️ agent finished but N file(s) are still uncommitted in <worktree>: ...` | 退出 |
| agent 跑完 HEAD 没动 | `⚠️ agent finished but no new commit was created (HEAD unchanged at <sha>)` | 退出 |
| agent 跑完引入冲突 | `❌ worktree has unmerged paths (merge/rebase conflict)` | 退出 |
| 成功 | `🤖 <agent> committed N change(s) on <branch>: ...` | 退出(不 push) |

---

## 8. 测试计划

### 8.1 push 单测(`internal/command/gtw/push_test.go`)

| 测试 | 覆盖 case |
|---|---|
| `TestRunPush_CleanNoUnpushed` | 干净 + 无未推 commit → no-op 卡片 |
| `TestRunPush_CleanWithUnpushed` | 干净 + 有未推 commit → `✅ pushed N commit(s)` |
| `TestRunPush_FirstPush_NoUpstream` | 无上游 → `git push -u origin <branch>` |
| `TestRunPush_FirstPush_PushingDefaultBranch` | 推默认分支本身(首次)→ rev range 兜底 |
| `TestRunPush_FirstPush_NoOriginRemote` | 没有 origin remote |
| `TestRunPush_VerifyDetectsSilentPushFailure` | push exit 0 但 commit 没落地 → 验真抓到 |
| `TestRunPush_PushErrorSurfaced` | push 报错 → 真实错误透传,不报 "pushed 0 commit(s)" |
| `TestRunPush_DirtyRefused` | 工作区脏 → 拒 + 指向 `/gtw commit` |
| `TestRunPush_ConflictState` | 冲突 → 硬拒 |
| `TestRunPush_GoneUpstream_Pushes` | ghost upstream(`[gone]`)→ 仍然 push,不误判 nothing to push |
| `TestDispatchPush_HardRefuse_Conflicts` | `HasConflicts=true` → 拒,**不调 push** |
| `TestDispatchPush_NothingToPush` | `HasNothingToPush=true` → no-op 卡片 |
| `TestDispatchPush_NoUpstreamFreshBranch` | 干净 + 无上游 → push with `-u`,**不调 agent** |
| `TestHasNothingToPush_GoneUpstream` / `TestParseBranchHeader_GoneUpstream` | `[gone]` 解析 + 谓词短路 |

### 8.2 commit 单测(`internal/command/gtw/commit_test.go`)

| 测试 | 覆盖 case |
|---|---|
| `TestBuildAgentPrompt_*`(16 个) | allowlist / denylist 逐条断言:禁改文件、禁 hunk surgery、禁跑测试、禁自查、禁 push、禁改历史、禁换分支、禁 revert-restore-stash、禁 `git add -A`、Issue trailer、terse final reply、one coherent commit |
| `TestParseCommitArgs_*` | `-a` / `--agent` 解析,未知 flag / 位置参数拒绝 |
| `TestRunCommit_NoWorkToDo` | 干净 → nothing to commit |
| `TestRunCommit_DetachedHead` | detached → 拒 |
| `TestRunCommit_ConflictState` | 冲突 → 硬拒 |
| `TestRunCommit_HappyPath` | agent commit → `🤖 <agent> committed N change(s)` |
| `TestRunCommit_DirtyWithAgentFlag` | `-a <name>` 指定 runner |
| `TestRunCommit_DirtyAgentLeavesFiles` | agent 留了未提交文件 → `⚠️ ... still uncommitted` |
| `TestRunCommit_DirtyAgentClaimsDoneButNoCommit` | agent 声称完成但 HEAD 没动 → `⚠️ no new commit was created` |
| `TestRunCommit_DirtyAgentSwitchesBranch` | agent 换了分支 → 拒 |
| `TestRunCommit_AgentIntroducedConflicts` | agent 引入冲突 → 拒 |
| `TestRunCommit_NoAgentSelected` / `TestRunCommit_UnknownAgent` / `TestRunCommit_AgentBinaryMissing` / `TestRunCommit_AgentRunOnceError` | agent 侧的各类失败 |

### 8.3 共享 fixture

`internal/command/gtw/readiness_test.go` 提供 `setupReadiness(rig, branch, snap)` 与 `setupPushMocks(git, branch, snap)` —— push / commit / pr 三个 dispatcher 的测试用同一份 snapshot mock 数据。

### 8.4 E2E / manual smoke

- 真跑一次 `/gtw commit` 在 docs-readme worktree:验证 IM 卡片是从 git log 拼的(commit subject 跟 git log 完全一致),agent prose 不会出现在卡片里
- 用 `-a` 强制指定 pi / claude / codex,确认 3 个 agent 都按 prompt 跑
- 故意让 agent 只跑 `git restore` 不 commit,看 verify 是否抓得到

---

## 9. 不在范围内(Out of Scope)

- **`/gtw fix` 的 Assess-first dispatch + branch 硬失败**:见 [`F-gtw-fix.md`](./F-gtw-fix.md)
- **`/gtw fix` / `pr` / `close` / `sync` 的其他重构**:`fix` 的 worktree 创建、`pr` 的 one-shot agent、`close` 的 worktree 销毁、`sync` 的 pull,各自有不同职责
- **agent prompt 的"是否要在 commit 失败时输出 FAILED"**:设计选择**完全不让 agent 汇报**。如果将来调试需要,可以在 agent 跟 nightme 之间加一个 transcript 日志,但**不**让 FAILED 进 prompt
- **夜间 batch commit / scheduled push**:完全无关
- **跨 worktree 聚合 push**:每个 `/gtw push` 只管一个 worktree
- **commit message 自动 lint / 校验**:nightme 不解析 commit message 格式,LLM 写错就让它错(用户自己改)

---

## 10. 决策记录

| 决策 | 理由 | 备选 |
|---|---|---|
| Agent 不 push | 网络副作用是 LLM 弱项;push 决策 / retry 是 infra 责任 | 让 agent push + nightme verify |
| Agent 不 claim SHA | git 是 source of truth,不需要 LLM 二次汇报 | 解析 agent reply 校验 |
| `verifyAgentCommitted` 只看 git 状态 | 跟 nightme 其它 verification 一致,不读 agent prose | 解析 agent reply |
| push 与 commit 拆成两条命令 | agent 的失败模式(HEAD 没动 / worktree 还脏 / 引入冲突)属于 commit 阶段,混在 push 的 IM 回复里说不清 | push 内部跑 agent 再续推 |
| no-op 单独一张卡 | 推 0 条 commit 时静默返回会误导用户 | 静默 no-op(UX 差) |
| `programmaticPushWithRetry` 统一入口 | push 的成败判据只有一处,retry 语义一致 | 每个调用点各写一套 |

---

## 11. 实现落点

| 职责 | 文件 | 符号 |
|---|---|---|
| push dispatcher(2 branch + 3 道硬拒) | `internal/command/gtw/push_dispatch.go` | `dispatchPush` |
| commit dispatcher(agent → verify → 卡片) | `internal/command/gtw/commit.go` | `dispatchCommit` |
| agent prompt | `internal/command/gtw/commit.go` | `buildAgentPrompt` |
| commit 后校验 | `internal/command/gtw/commit.go` | `verifyAgentCommitted` |
| push + retry + git 原语 | `internal/command/gtw/push.go` | `programmaticPush` / `programmaticPushWithRetry` / `headSHA` / `countUnpushed` / `listUncommittedFiles` |
| IM 卡片 | `internal/command/gtw/render.go` | `replyPushCard` / `replyCommitSuccessCard` / `nothingToPushCard` / `alreadyPushedCard` |
| readiness 快照 | `internal/command/gtw/git_status.go` | `CollectReadiness` / `CollectReadinessForDispatch` |
| 快照类型 + 谓词 | `internal/messages/session.go` | `GitStatusSnapshot` / `HasUpstreamBranch` / `LocalIsAtUpstreamTip` / `WorkingTreeIsClean` / `HasNothingToPush` / `PushBlockReason` |
| 测试 | `internal/command/gtw/push_test.go` / `commit_test.go` / `readiness_test.go` | — |

---

## 12. 风险与回滚

### 12.1 风险

| 风险 | 缓解 |
|---|---|
| prompt 的 allowlist / denylist 覆盖不全,某些 edge case 缺指令 | `commit_test.go` 里 16 个 `TestBuildAgentPrompt_*` 逐条断言;manual smoke 后再 merge |
| push 判据从"exit code"换成"countUnpushed" | `push_test.go` 的 `TestRunPush_VerifyDetectsSilentPushFailure` 覆盖 git exit 0 但 commit 没落地 |
| push 拒绝脏工作区,依赖"agent 自己推"的工作流的用户会撞墙 | 回复里直接指向 `/gtw commit`,两步流程明确 |
| ghost upstream(`[gone]`)被误判成"nothing to push" | `HasNothingToPush` 的 `!s.UpstreamGone` 短路;`TestRunPush_GoneUpstream_Pushes` 覆盖 |

### 12.2 回滚

`dispatchPush` / `dispatchCommit` 的拆分是单一 PR 内的原子改动,git revert 即可回到"push 内跑 agent 再续推"的模型。无需数据迁移、无需 schema 改动。

---

## 13. 后续 PR(不在本次)

| 任务 | 优先级 | 说明 |
|---|---|---|
| `replyPushCard` / `replyCommitSuccessCard` 接入 `internal/channel/feishu/` 的 format helper,跟 footer / checklist 风格统一 | P2 | 现在卡片自己拼字符串,Feishu adapter 那边有现成的 format helper 没用 |
| 把"agent transcript 日志"加进 daemon 控制台,方便调试 agent 为什么没 commit | P3 | 现在 agent 输出被忽略,出错时只能从 git log 倒推;transcript 日志能直接看 LLM 在干什么 |
| `/gtw pr` 的 one-shot agent 同步收窄(类似 F-56 的 prompt 重写) | P3 | 现在 `/gtw pr` 的 agent 也有"commit + push + 报告"老问题,但优先级低 |
| 整套 F-56 思路延展到 `/gtw close` / `sync`,让 agent 只做"judgment 部分" | P3 | `close` 是销毁 worktree(纯 git 命令,不需要 agent);`sync` 是 pull(也纯 git 命令)。这两条可能根本不需要 agent |

---

## 14. 参考

- [`internal/command/gtw/push.go`](../../internal/command/gtw/push.go) — gtw hooks + agent 优先级 3 档 chain
- [`internal/command/gtw/README.md`](../../internal/command/gtw/README.md) — gtw IM 卡片排版规约(emoji 字典、Format 1/2/3)
- [`internal/command/gtw/commit.go`](../../internal/command/gtw/commit.go) — `/gtw commit` 的 dispatcher + agent prompt + commit 后校验
- [`internal/command/gtw/push_dispatch.go`](../../internal/command/gtw/push_dispatch.go) — `/gtw push` 的 dispatcher
- `internal/command/gtw/push.go::headSHA` / `countUnpushed` / `listUncommittedFiles` — git 状态查询原语
- `internal/command/gtw/push.go::programmaticPushWithRetry` — push 的唯一入口(带一次重试 + countUnpushed 验真)
- `internal/command/gtw/git_status.go::CollectReadinessForDispatch` — readiness 快照的采集入口
- `internal/messages/session.go::GitStatusSnapshot` — 快照类型 + 谓词(`HasUpstreamBranch` / `LocalIsAtUpstreamTip` / `WorkingTreeIsClean` / `HasNothingToPush` / `PushBlockReason`)
- `internal/command/gtw/commit_test.go::TestRunCommit_DirtyAgentClaimsDoneButNoCommit` — false-success 类的 regression test

---

## A2. F-57: `/gtw push` + `/gtw pr` 联动 Readiness Gate

> **Source**: `F-gtw.md`


> **Depends on**: F-56 (`/gtw push` 两分支流)、F-50 (GitHub/GitLab Provider)、F-45 (Session footer) — 后者已经在 SessionContext 里跑过 `CollectReadiness`,本设计只是扩字段+补谓词

> **Related**: [`本文件 §The Branches`](./F-gtw.md)（push 分支的细节）、[`internal/command/gtw/git_status.go`](../../internal/command/gtw/git_status.go)（snapshot 解析层）、[`internal/messages/session.go`](../../internal/messages/session.go)（`GitStatusSnapshot` 类型定义 + 谓词）

---

## 0. TL;DR

把"本地分支已经 commit + push + 与 origin 对齐"这件事,从一个散落在 `dispatchPush` 和 `dispatchPR` 里的隐含约定,显式建模成一个**共享的 `Readiness` 概念**:

1. **单一数据源**:一次 `git status --porcelain --branch` 调用 → `GitStatusSnapshot`。
2. **三个正交原子谓词**:`HasUpstreamBranch()`、`LocalIsAtUpstreamTip()`、`WorkingTreeIsClean()` —— push 和 commit 都基于这三个判定,语义不再各自为政。
3. **业务判定分离**:`/gtw push` 和 `/gtw commit` 用 `HasNothingToPush()` + `PushBlockReason()`;`/gtw pr` 不吃这份 snapshot —— 它走 A4 的两道闸(`origin/<branch>` 存在 + 没有已开 PR)。
4. **连续性保证**:`/gtw push` 成功结束的工作区,**必**使 `/gtw pr` 的第一道闸通过(并发 race 除外)。两个命令之间不再需要用户在 IM 里手动确认"我到底有没有 push 过"。

今天撞到的 case (`Head sha can't be blank, Head ref must be a branch`) 是这一设计的直接触发:本地 `refactor-outbound` 没有 upstream(`git config branch.refactor-outbound.remote` 空),`dispatchPR` 复用 `countUnpushed` 把"无上游"误读为"零未推送",把分支从未 push 的状态当成"已对齐",直接调 `gh pr create`,被 GitHub 的 GraphQL 拒掉。新设计把 `HasUpstream` 单独作为一条维度,这种 case 在 `/gtw pr` 入口就被显式拦截,提示用户 `/gtw push first to publish the branch`。

---

## 1. Motivation

### 1.1 撞到的具体 case

```
❌ create PR failed: gh pr create: exit status 1:
pull request create failed: GraphQL: Head sha can't be blank, Base sha can't be blank,
No commits between main and refactor-outbound, Head ref must be a branch (createPullRequest)
```

| 状态维度 | 实际值 | `dispatchPR` 表现 |
|---|---|---|
| `refactor-outbound` 本地 HEAD | `fe6ed8d`(本地有 17 个 commit) | n/a |
| `origin/refactor-outbound` | **不存在**(`git ls-remote` 找不到) | n/a |
| `main..HEAD`(本地) | 17 commit | rev-list 预检返回 17,预检通过 |
| `refactor-outbound@{u}..HEAD` | fatal: no upstream | `countUnpushed` 返回 `(0, nil)`(把"无上游"误读为"零未推送") |
| dispatchPR 的结论 | — | "通过,继续调 gh" |
| gh 的结论 | — | "Head ref must be a branch",GraphQL 拒绝 |

`countUnpushed`(`internal/command/gtw/push.go`)的语义是**为 `/gtw push` 量身定制的**(注释里写明:"no upstream configured → our '0 unpushed' signal"),直接被 `dispatchPR` 复用是把两个命令的语义混在一起,边界破了。

### 1.2 两个命令的语义本来就不一样

| 命令 | "无上游" 的语义 |
|---|---|
| `/gtw push` | **合法的工作场景**:这个分支首次 push,需要 `git push -u origin <branch>`。**不能**拒绝。 |
| `/gtw pr` | **非法的工作场景**:本地分支根本没在 origin 上,远端没有这个 ref,gh/glab 调不通。**必须**拒绝。 |

也就是说,"无上游" 在 push 是 happy path 之一,在 pr 是 hard block。同一个 git 事实,两个命令必须给出相反判定 —— 唯一的办法是**把它们各自的判定式分开**,共享的只有底层 snapshot。

### 1.3 "push → pr 连续体感"是产品语义

资深 dev 跑这两个命令的心智模型是:

现在的代码割裂在两处独立判定里:

- `dispatchPush` 用 `isClean` (string empty) + `countUnpushed`(rev-list) 两段独立探测,合起来才覆盖"脏 / ahead / nothing"
- `dispatchPR` 同样用 `countUnpushed` + 一条 rev-list 预检两段独立探测,但取了一个不一样的 `countUnpushed` 语义

两个命令各管各的 snapshot,IM 用户体验上会出现"push 提示我'已经干净',pr 提示我'未推送'"的诡异跳跃。

把 readiness gate 提到共享层,**两个命令读同一份 snapshot**,产品语义就有了结构上的保证。

---

## 2. Readiness Snapshot

### 2.1 `GitStatusSnapshot`

快照类型定义在 `internal/messages/session.go`(runtime footer 渲染层也用它):

```go
type GitStatusSnapshot struct {
    Branch        string
    Added         int
    Deleted       int
    Modified      int
    Untracked     int
    Conflicts     int
    AheadOfRemote int
    BehindRemote  int
    HasUpstream   bool
    UpstreamGone  bool
    HasConflicts  bool
}
```

`parseBranchHeader`(`internal/command/gtw/git_status.go`)解析 `behind M`,并在 `## b...origin/b [gone]` 时置 `UpstreamGone`。`parsePorcelainBranchStatus` 在扫剩余行时识别 unmerged(`U*` / `*U` / `A*` / `*A` / `D*` / `*D` 且非 `??`),置 `HasConflicts`。

### 2.2 单一数据源 `CollectReadiness`

`internal/command/gtw/git_status.go::CollectReadiness` 跑一次 `git status --porcelain --branch --untracked-files=normal` 并解析成 snapshot:

```go
func CollectReadiness(ctx context.Context, dir string, git GitRunner) (*GitStatusSnapshot, error)
```

不是 git 仓库 / 输出为空或畸形 / git 调用失败时返回 `(nil, nil)` —— footer 渲染路径把 `(nil, nil)` 当"没有 git 段落"。

dispatch 路径用的是 `CollectReadinessForDispatch`:它在 `CollectReadiness` 之上加一次 `git ls-remote --heads origin <branch>` 探测,用来抓 F-237 的 stale-cached-upstream(本地 config 认为有 upstream、远端其实已经删了分支)。探测失败时保留 porcelain 的判断,不把一个瞬时网络抖动翻转成 gate 失败。

### 2.3 6 维度 = 业务判定空间的完整展开

| 维度 | snapshot 字段 | 业务含义 |
|---|---|---|
| **D1 真分支** | `Branch != ""` | 不能在 detached HEAD 上 push |
| **D2 上游存在** | `HasUpstream` | 分支在 origin 上存在 |
| **D3 已推送** | `AheadOfRemote == 0` | 本地所有 commit 都已 push |
| **D4 同步** | `BehindRemote == 0` | 本地拿到了 origin 上所有最新 commit |
| **D5 工作区干净** | `Added == 0 && Deleted == 0 && Modified == 0 && Untracked == 0 && Conflicts == 0` | 没有需要 commit 的本地改动 |
| **D6 无冲突** | `!HasConflicts` | 没有未解决的 merge/rebase conflict |

### 2.4 三个正交原子谓词

业务判定(§3、§4)拆成 3 个**只依赖 snapshot、不掺命令意图**的谓词,定义在 `internal/messages/session.go`(gtw 层通过类型别名 `gtw.GitStatusSnapshot = messages.GitStatusSnapshot` 访问):

```go
// HasUpstreamBranch: 这个分支在 origin 上存在吗?
func (s *GitStatusSnapshot) HasUpstreamBranch() bool {
    return s.Branch != "" && s.HasUpstream
}

// LocalIsAtUpstreamTip: 本地分支 tip 跟 origin/<branch> tip 完全一致吗?
//   - HasUpstream 必须为 true,否则无对照基准
func (s *GitStatusSnapshot) LocalIsAtUpstreamTip() bool {
    return s.HasUpstreamBranch() && s.AheadOfRemote == 0 && s.BehindRemote == 0
}

// WorkingTreeIsClean: 工作区没有任何需要 commit 的东西?
func (s *GitStatusSnapshot) WorkingTreeIsClean() bool {
    return s.Added == 0 && s.Deleted == 0 &&
        s.Modified == 0 && s.Untracked == 0 &&
        s.Conflicts == 0
}
```

---

## 3. `/gtw push` Gate

push 的语义 = "把本地需要 commit 的东西 commit 掉,然后推到 origin"。

### 3.1 业务判定

```go
// PushBlockReason: 硬拒条件。返回非空字符串 → 拒绝 + 提示,不调 agent 不调 push。
// 当前唯一的硬拒是"merge conflict 在工作区" —— push 一个 unresolved 状态
// 等于把雷埋进团队历史。
func (s *GitStatusSnapshot) PushBlockReason() string {
    if s.HasConflicts {
        return "❌ worktree has unmerged paths (merge/rebase conflict)\n" +
            "hint: resolve conflicts and `git add`, OR `git rebase --abort` / `git merge --abort`"
    }
    return ""
}

// HasNothingToPush: 三种"无活可干"的合并判定
func (s *GitStatusSnapshot) HasNothingToPush() bool {
    return s.WorkingTreeIsClean() &&        // 啥都 commit 完了
        s.HasUpstreamBranch() &&            // 分支在 origin 存在
        s.AheadOfRemote == 0 &&             // 也没有 ahead
        !s.UpstreamGone                     // 上游不是 ghost
}
```

注意 **"无上游" 不进 `HasNothingToPush`** —— 没有 upstream 的干净工作区恰恰是要首次 push 的合法场景。

`UpstreamGone` 也要短路:`git status --porcelain --branch` 在 `## b...origin/b [gone]` 时仍然报 `HasUpstream=true, ahead=0`,但 `refs/remotes/origin/b` 其实已经不在了。这种状态下 `HasNothingToPush` 必须返回 false,让 push 继续走 `git push -u origin <branch>` 把分支重新发布出去。

### 3.2 dispatchPush 骨架

```go
func dispatchPush(ctx, cs, deps, chatID, messageID, args) (*Result, error) {
    c, res := loadDispatchContext(ctx, cs, deps, chatID, messageID)
    if res != nil { return res, nil }

    snap, err := CollectReadinessForDispatch(ctx, c.Worktree, deps.Git)
    if err != nil { return reply(...❌...), nil }
    if snap == nil { return reply(...❌ not a repo...), nil }

    // 1. 硬拒冲突
    if reason := snap.PushBlockReason(); reason != "" {
        return reply(ctx, cs.Emitter(), chatID, messageID, reason), nil
    }

    // 1b. 硬拒 detached HEAD
    if snap.Branch == "" {
        return reply(ctx, cs.Emitter(), chatID, messageID,
            "❌ detached HEAD — checkout a named branch first"), nil
    }

    // 1c. 硬拒脏工作区 —— commit 是 /gtw commit 的职责
    if !snap.WorkingTreeIsClean() {
        return reply(ctx, cs.Emitter(), chatID, messageID,
            "❌ worktree is dirty — /gtw push no longer auto-commits\n"+
                "hint: run `/gtw commit` first, then `/gtw push`"), nil
    }

    // 2. 没活儿干 → 提示用户
    if snap.HasNothingToPush() {
        card, out := pushFallback(ctx, c, snap.Branch, deps)
        return reply(ctx, cs.Emitter(), chatID, messageID, card), nil
    }

    // 3. push 前的 origin tip —— 成功卡片的 rev range 左端
    originBefore, err := originBranchSHA(ctx, c.Worktree, c.Branch, deps)
    if err != nil { return reply(...❌...), nil }

    // 4. 真·push(带重试 + 验证 unpushed==0)
    if err := programmaticPushWithRetry(ctx, deps, c); err != nil {
        return reply(ctx, cs.Emitter(), chatID, messageID, err.Error()), nil
    }

    // 5. 成功卡 —— 从 git log 拼,不用 agent prose
    revRange := originBefore + "..origin/" + c.Branch
    card, commitCount, err := replyPushCard(ctx, c, revRange, deps)
    if err != nil { return reply(...❌...), nil }
    return reply(ctx, cs.Emitter(), chatID, messageID, card), nil
}
```

`/gtw commit`(`internal/command/gtw/commit.go::dispatchCommit`)用同一份快照:冲突 → detached HEAD → 已干净 → 跑 agent → `verifyAgentCommitted` → re-snapshot → `replyCommitSuccessCard`。agent 跑完的 re-snapshot 不可省 —— agent 可能引入冲突或留下 dirty 状态。

### 3.3 与 F-56 的对应关系

| F-56 分支 | F-57 改写后的路径 |
|---|---|
| no-op:clean + 无未推 commit | `snap.HasNothingToPush()` 返回 true |
| push:clean + 有未推 commit | `snap.HasNothingToPush()` false → 直推 |
| (原 Branch 2)dirty | `/gtw push` 拒掉并指向 `/gtw commit`;commit 路径走 `runAgentFor` + `verifyAgentCommitted` |

**clean + !HasUpstream**(首次 push)—— `HasNothingToPush` 因为 `!HasUpstreamBranch()` 返回 false,所以进 push;`programmaticPush` 内部走 `git push -u origin <branch>`。

### 3.4 `countUnpushed` 的新定位

`internal/command/gtw/push.go::countUnpushed` **不删**,但缩小职责范围:

- **保留**:在 `programmaticPushWithRetry` 内部做"push 后二次验证"(git 可以 exit 0 但提交没真上去,见 F-56 §4.3)
- **移除**:不再被 `dispatchPush` 直接调用做"是否有未推送"判定

`countUnpushed` 的"no upstream configured = 0 unpushed"语义对 push 后验证是对的(此时上游已被 `-u` 设置过,不会有"无上游"路径);`dispatchPush` 入口不再有路径走到这个 fallback。

---

## 4. `/gtw pr` Gate

pr 的语义 = "我准备好了,请把这个本地分支在 Web 平台上开放成 PR"。

### 4.1 业务判定

`/gtw pr` **不吃** readiness snapshot。它的 readiness 收窄到两道闸(A4):

| 闸门 | 实现 | 失败提示 |
|---|---|---|
| 1. `origin/<branch>` ref 存在 | `RemoteBranchExists(ctx, dir, branch, git)` → `git ls-remote --heads origin <branch>` | `❌ origin/<branch> does not exist — /gtw push first to publish the branch to origin` |
| 2. 没有已开 PR | `provider.FindOpenPRForBranch(owner, repo, head)` → `gh pr list --head ...` / `glab mr list --source-branch ...` | `✅ branch <branch> already has an open PR (#N): <url>` |

ahead / behind / dirty / conflicts / detached / 无 tracking ref 都不是硬拒条件 —— 这些是 gh/glab 自己会处理的领域。

### 4.2 dispatchPR 入口

```go
func dispatchPR(ctx, cs, deps, chatID, messageID, args, cfg) (*Result, error) {
    c, res := loadDispatchContext(ctx, cs, deps, chatID, messageID)
    if res != nil { return res, nil }

    baseBranch, err := DefaultBranch(ctx, c.RepoRoot, deps.Git)
    if err != nil { return reply(...❌...), nil }

    // 闸 1: origin/<branch> ref 存在
    headExists, err := RemoteBranchExists(ctx, c.Worktree, c.Branch, deps.Git)
    if err != nil { return reply(...❌...), nil }
    if !headExists {
        return reply(ctx, cs.Emitter(), chatID, messageID,
            fmt.Sprintf(
                "❌ origin/%s does not exist — /gtw push first to publish the branch to origin",
                c.Branch)), nil
    }

    // provider 必须在闸 2 之前解析
    provider, owner, repo, err := resolveProvider(ctx, c, deps)
    if err != nil { return reply(...❌...), nil }

    // 闸 2: 没有已开的 PR
    existing, err := provider.FindOpenPRForBranch(ctx, owner, repo, c.Branch)
    if err != nil { return reply(...❌...), nil }
    if existing != nil {
        return reply(ctx, cs.Emitter(), chatID, messageID,
            fmt.Sprintf(
                "✅ branch %s already has an open PR (#%d): %s",
                c.Branch, existing.Number, existing.URL)), nil
    }

    // agent + provider.CreatePR
    ...
}
```

### 4.3 取消的旧逻辑

`/gtw pr` 的 readiness 收窄到两道闸,连同被取消的旧逻辑(`countUnpushed` 调用、"⚠️ N unpushed" 提示、内嵌的 uncommitted / untracked 判断、`countBaseAhead` + "nothing new to PR yet" 分支、`PRBlockReason` / `IsReadyForPR`),见 A4 §A4.3。

---

## 5. Continuity Proof

把"push 成功, pr 的第一道闸必通过"写成不变式:

设 `snap` 是某工作区在时刻 T 的 readiness 快照,`ProgrammaticPush` 表示成功执行 `git push -u origin <branch>` 后的瞬间状态。

```
T0:   用户敲 /gtw push
T1:   snap_push = CollectReadinessForDispatch(T0)
T2:   若 snap_push.PushBlockReason() != "" → 拒绝(返回)
T3:   若 !snap_push.WorkingTreeIsClean() → 拒绝,指向 /gtw commit(返回)
T4:   若 snap_push.HasNothingToPush() → no-op(返回)
T5:   ProgrammaticPush 成功
      此时:snap_push 必有:
        - HasUpstreamBranch() == true  (T5 -u 设了)
        - LocalIsAtUpstreamTip() == true  (T5 推到对齐)
        - WorkingTreeIsClean() == true   (T3 保证了工作区干净)
T6:   用户敲 /gtw pr
T7:   闸 1: RemoteBranchExists(origin/<branch>) == true  (T5 建立了这个 ref)
```

唯一会让 pr 在 push 成功之后仍被闸 1 挡住的是:**外部并发**动了 `origin/<branch>`(远端分支被删 / 别人抢推)。这种 race 本身就需要用户介入 —— 不可消除,但闸 1 仍能兜住。

### 5.1 形式化对比

| 维度 | `/gtw push` 成功后 | `/gtw pr` 闸 1 要求 | 一致 |
|---|---|---|---|
| `HasUpstreamBranch()` | true(`-u` 已设) | true(`RemoteBranchExists`) | ✓ |
| `LocalIsAtUpstreamTip()` | true(`AheadOfRemote`=0) | — | — |
| `WorkingTreeIsClean()` | true(push 硬拒脏工作区) | — | — |

---

## 6. 不变式 Checklist

- **Snapshot 单一来源**。`/gtw push` 和 `/gtw commit` 入口都从 `CollectReadinessForDispatch` 取,不再有 `git status --porcelain` 单独调用或 `countUnpushed` 直调
- **`countUnpushed` 退出 dispatch 顶层**。仅留在 `programmaticPushWithRetry` 内部做 push 后二次验证
- **Push 容忍 `!HasUpstreamBranch`** —— 没有上游的干净工作区正是首次 push 的合法场景;pr 的闸 1 则要求这个 ref 存在
- **agent 跑完强制 re-snapshot**。`dispatchCommit` 的 re-snapshot 不可省 —— agent 可能引入冲突或遗留 dirty 状态
- **冲突永远先报**。`HasConflicts` 在 push 和 commit 的 `PushBlockReason` 里都是首条
- **底层 git 调用次数不增**。snapshot 解析仍是 `CollectReadiness` 一次 `git status`

---

## 7. 错误处理矩阵

### 7.1 `/gtw push` 与 `/gtw commit`

两条命令的完整错误矩阵见 A1 段的 §7。

### 7.2 `/gtw pr`

| 触发条件 | IM 输出 | 后续动作 |
|---|---|---|
| 默认分支探测失败 | `❌ <err>` | 退出 |
| 闸 1:`origin/<branch>` 不存在 | `❌ origin/<branch> does not exist — /gtw push first to publish the branch to origin` | 退出(不动 agent 不动 gh) |
| provider 解析失败(没有 origin remote / URL 非法 / 平台不支持) | `❌ <err>` | 退出 |
| 闸 2:gh/glab 没装 | `❌ <install hint>` | 退出 |
| 闸 2:已开 PR | `✅ branch <branch> already has an open PR (#N): <url>` | 退出 |
| 闸 2:列 PR 失败 | `❌ check existing PR: <err>` | 退出 |
| 全部通过 | 进入 agent + `gh/glab pr create` | 创建 PR |

### 7.3 emoji 一致性

沿用 `internal/command/gtw/README.md §1.1` 的 emoji 闭集:

- ❌ 错误 / 硬拒(冲突、detached HEAD、push 失败)
- ⚠️ 警告 / 部分成功(verify 失败、hook 警告)
- ✅ 正面确认(push 成功、already on the remote、PR 已存在)
- 🤖 agent 主导(commit 成功)

---

## 8. 测试计划

### 8.1 共享 fixture

`internal/command/gtw/readiness_test.go` 提供 `setupReadiness(rig, branch, snap)` 与 `setupPushMocks(git, branch, snap)` —— push / commit / pr 三个 dispatcher 的测试用同一份 snapshot mock 数据,再加 `git ls-remote --heads origin <branch>` 的闸 1 mock。

### 8.2 `/gtw push` 与 `/gtw commit` 用例

见 A1 段 §8.1 / §8.2(`internal/command/gtw/push_test.go` / `commit_test.go`)。

### 8.3 `/gtw pr` 用例(`internal/command/gtw/pr_test.go`)

| 测试 | 覆盖 case |
|---|---|
| `TestDispatchPR_NoOriginBranch` | 闸 1:`origin/<branch>` 不存在 → 拒,IM 含 "**/gtw push first** to publish the branch",**不动 gh** |
| `TestDispatchPR_LSRemoteAuthError` / `_NetworkError` / `_NotARepository` | 已知 stderr → 友好 hint |
| `TestDispatchPR_LSRemoteUnknownErrorPassThrough` | 未知 stderr 原样透传 |
| `TestRemoteBranchExists_EmptyStderr` | ls-remote 返回空 → false |
| `TestDispatchPR_ExistingPR` | 闸 2:已开 PR → `✅ branch <b> already has an open PR (#N): <url>` |
| `TestDispatchPR_FindOpenPRErrCLINotInstalled` / `_UnknownErrorPassThrough` | gh/glab 没装 / 未知错误 |
| `TestDispatchPR_CreateStaleUpstreamRace` / `_GitLab` | race-window:head 在 origin 上消失 |
| `TestDispatchPR_AlreadyExistsRace` / `TestDispatchPR_CreatePRExists` | race-window:PR 被人开了 |
| `TestDispatchPR_CreatePR_CLINotInstalled` / `_UnknownErrorPassThrough` | CreatePR 的已知 / 未知错误分流 |
| `TestDispatchPR_DirtyNoLongerGate` | dirty / ahead / behind / conflicts 不再 gate |
| `TestDispatchPR_NoAgentSelected` / `TestDispatchPR_AgentRunOnceFails` / `TestDispatchPR_AgentOutputUnparsable` | agent 侧失败路径 |
| `TestDispatchPR_MissingTitleRefusesPR` / `TestDispatchPR_NonConventionalTitleStillOpens` / `TestDispatchPR_BranchNameTitleRefusesPR` | 第三道闸:输出格式 |
| `TestDispatchPR_RetryOpensPRFromSecondReply` / `TestDispatchPR_RetryStillFailsRefusesPR` | 格式不过时把原因回喂 agent 一次 |
| `TestDispatchPR_IssueFetchFails_StillOpens` / `TestDispatchPR_MissingDeviationSection_Warns` / `TestDispatchPR_WarningsMerged` | issue / label / deviation 警告 |
| `TestGitHubProvider_ClassifyCreatePRError_StaleUpstream` | provider 层的错误分类 |

`internal/command/gtw/pr_ready_test.go` 覆盖 `--ready` 的 label / issue 状态迁移。

### 8.4 连续性用例(关键 — `*_test.go` 跨文件)

```go
// TestPushThenPR_ContinuousFlow:
// 1) mock 初始 snap = clean + !HasUpstream
// 2) dispatchPush,验证 programmaticPushWithRetry 被调且 args 含 "-u origin <branch>"
// 3) re-mock 入口 snap 为 "push 完成态":
//      HasUpstream=true, Ahead=0, Behind=0,
//      Added=0, Deleted=0, Modified=0, Untracked=0, Conflicts=0,
//      HasConflicts=false
// 4) dispatchPR,验证:
//      - 闸 1 通过(ls-remote 返回该分支)
//      - agent 跑一次
//      - gh pr create 被调一次
```

```go
// TestPushFailThenPR_Refuses:
// 1) mock 初始 snap = clean + HasUpstream+Ahead=3
// 2) dispatchPush,programmaticPush 失败(mock stderr)
// 3) 验证 dispatchPush 返回 "❌ git push failed: ..."
// 4) snap 此刻仍 Ahead>0(commit 在本地但没推上去)
// 5) dispatchPR —— PR 路径不再 gate on ahead,
//    gh/glab 自己对"没有新 commit"给最终结论
```

### 8.5 关键不变式(单测断言)

```go
// commit.go::buildAgentPrompt 不包含 "git push" 命令(F-56 §3 不变量)
func TestBuildAgentPrompt_AntiPush(t *testing.T) { ... }

// 验证 dispatchPR 永远先看闸 1,不调 countUnpushed
func TestDispatchPR_NoCountUnpushed(t *testing.T) { ... }

// 验证 dispatchPush 不再调用 countUnpushed 做主判定
func TestDispatchPush_NoCountUnpushedAtEntry(t *testing.T) { ... }
```

---

## 9. 实现落点

| 职责 | 文件 | 符号 |
|---|---|---|
| 快照采集 + 解析 | `internal/command/gtw/git_status.go` | `CollectReadiness` / `CollectReadinessForDispatch` / `verifyUpstreamOnOrigin` / `parsePorcelainBranchStatus` / `parseBranchHeader` |
| 快照类型 + 谓词 | `internal/messages/session.go` | `GitStatusSnapshot` / `HasUpstreamBranch` / `LocalIsAtUpstreamTip` / `WorkingTreeIsClean` / `HasNothingToPush` / `PushBlockReason` |
| push 入口 | `internal/command/gtw/push_dispatch.go` | `dispatchPush` |
| commit 入口 | `internal/command/gtw/commit.go` | `dispatchCommit` |
| pr 入口(两道闸) | `internal/command/gtw/pr.go` | `dispatchPR` |
| 闸 1 | `internal/command/gtw/worktree.go` | `RemoteBranchExists` |
| provider 抽象 | `internal/command/gtw/provider.go` | `GitHubProvider` / `GitLabProvider` / `wrapCreatePRError` / `wrapListPRError` / `isExecutableNotFound` / `ErrCLINotInstalled` / `ErrStaleUpstream` / `ErrPRExists` |

---

## 10. 边界 / 未决问题

### 10.1 footer 渲染层是否消费新字段

runtime footer 渲染层消费 `GitStatusSnapshot`,但只展示它关心的字段;readiness 的新维度(`BehindRemote` / `HasConflicts` / `UpstreamGone`)不改变渲染。要让 footer 显示 `behind N` 或 conflicts 警告,需要单独的提案。

### 10.2 race 兜底

`/gtw push` 成功 → `/gtw pr` 期间,第三方修改 `origin/<branch>`(其他 worktree 抢推 / 远端分支被删)。闸 1 的 `ls-remote` 会捕获"ref 已经不在"这种情况。本设计**不**主动重试或自动 rebase,留给用户决策。

### 10.3 `programmaticPushWithRetry` 内的 `countUnpushed` 语义保留

"no upstream configured → 0 unpushed"语义**继续有效**在 `programmaticPushWithRetry` 的 verify loop 内:此时 `-u` 已经设过了,理论上不会走到"无上游"分支;但若真走到(极端 race),返回 0 是合理的"push 没产生新东西"信号,不是错误。这部分语义与 `dispatchPush` 入口的 readiness 判定**无耦合**,互不污染。

### 10.4 `ModeLocal`(没有 yml 的 worktree)

`loadDispatchContext` 在无 yml 时调 `rev-parse --show-toplevel` + `rev-parse --abbrev-ref HEAD` 推导 `Branch` / `Worktree` / `RepoRoot`。readiness gate 在入口之后,无论是否 ModeLocal 行为一致(只是少 `Repo` / `Provider` 信息,gh 调用时再 derive)。

### 10.5 "first-time push" UX

`HasUpstreamBranch()==false` 在 push 是合法路径。当前 `programmaticPush` 内部调 `git push -u`,不 surface "first push" 这个概念。如果想区分"首次 push"与"已有 upstream 但 ahead",需要在 push 的 IM 卡片里加一行。

---

## 11. References

- [`本文件 §The Branches`](./F-gtw.md):push 分支的完整设计 —— 本 F-57 是其 readiness 维度上的扩展
- [`internal/command/gtw/provider.go`](../../internal/command/gtw/provider.go):GitHub/GitLab provider 抽象,gh/glab 调用的下游
- [`internal/command/gtw/git_status.go`](../../internal/command/gtw/git_status.go):readiness 快照的采集与解析
- [`internal/messages/session.go`](../../internal/messages/session.go):`GitStatusSnapshot` 类型定义 + 谓词
- [`feishu-rendering.md §Footer`](../channel/feishu-rendering.md):footer 渲染层消费 `GitStatusSnapshot`,扩展字段不影响渲染
- [`internal/command/gtw/README.md`](../../internal/command/gtw/README.md):IM 卡片排版规约(emoji 一致性等)
- [`internal/command/gtw/push.go`](../../internal/command/gtw/push.go):agent 优先级 + chain

---


---

## A3. F-59: `/gtw` 检查并补齐仓库的 `nightme/*` label catalog

> **Source**: [`F-59-gtw-label-bootstrap.md`](./F-59-gtw-label-bootstrap.md)
>
> **Issue**: #235（`/gtw push` readiness bug —— 触发本次重构的载体
> case：第一次在 `cnlangzi/nightme` 跑 `/gtw fix 235` 撞到了
> `'nightme/wip' not found`）
>
> **改动摘要**：`GitProvider` 接口加 `CreateLabel` 与 `ListLabels`
> 方法；`ensureRepoLabels` 读仓库的 label catalog、只把 `AllLabels`
> 里缺的那些建出来，并按仓库把结果缓存进 `LabelCache`；检查在
> `/gtw fix` 的队列挑选**之前**跑（失败即致命，此时还没有
> worktree），在 `/gtw pr` / `/gtw new -y` 里只给
> 一行 `⚠️` 警告；provider 探测不做检查。

详细设计、错误处理矩阵、决策记录、不在范围内的 followups
见 [`F-59-gtw-label-bootstrap.md`](./F-59-gtw-label-bootstrap.md)。

---

## A3b. F-XX: `/gtw fix` Assess-first dispatch + 分支硬失败

> **Source**: [`F-gtw-fix.md`](./F-gtw-fix.md)
>
> **改动摘要**：
>
> - **默认** `/gtw fix <id>` dispatch **Assess Prompt**（只分析、出方案，禁止改文件）；用户
>   在 chat 里与 agent 确认后再让 agent 动手 —— gtw **不提供** `/gtw proceed`，也不做
>   第二次 dispatch。
> - **`-y` / `--yes`** dispatch **Execute Prompt**（直接实现修复）。
> - **删除 `--force` / `-f` 整个 flag**：branch-exists 硬失败之后，`-f` 仅剩的"路径
>   残留强制清理"用途变成纯破坏性 auto-recovery。残留路径让用户自己
>   `git worktree remove --force <path>` 或跑 `/gtw close` 即可。
> - **Branch 已存在** → 硬失败（废除 §5.3.1 branch-exists 决策卡、-v2 变体、daemon
>   recovery re-entry）。

完整流程、两套 prompt 正文、success card、测试要点与决策记录见
[`F-gtw-fix.md`](./F-gtw-fix.md)。

---

## A4. `/gtw pr` 收窄到 2 闸门(2-gate refactor)

> **Source**: branch `fix-gtw-pr-gitlab` — 把 `/gtw pr` 的 readiness
> 从 A2 设计的 6 维 snapshot gate 收窄到两道独立闸门。
>
> **改动动机**:产品决策 — `/gtw pr` 的语义 = "帮我尝试开 PR,前序
> 状态我自己负责"。`/gtw pr` 不应该再把 ahead / behind / dirty /
> conflicts / detached 当作硬拒条件 — 这些都是 gh/glab 自己会
> 处理的领域;我们卡在这条线上只会让用户在最后一步看到一堆 nudge,
> 跟"执行命令"的语义冲突。

### A4.1 新设计(2 闸门)

| 闸门 | 实现 | 失败提示 |
|---|---|---|
| 1. `origin/<branch>` ref 存在 | `RemoteBranchExists(ctx, dir, branch,)` → `git ls-remote --heads origin <branch>` 直接调 | `❌ origin/<branch> does not exist — /gtw push first to publish the branch to origin` |
| 2. 没有已开 PR | `provider.FindOpenPRForBranch(owner, repo, head)` → `gh pr list --head ...` / `glab mr list --source-branch ...` | `✅ branch <branch> already has an open PR (#N): <url>` |

两道闸都通过 → 直接 `CreatePR`,**不再**评估 ahead/behind/dirty/conflicts/detached/no-tracking-ref。

### A4.2 错误契约:known → 友好提示,unknown → 原文透传

每个底层 helper 自己判断 known / unknown,自己拼友好 hint(包带
 sentinel error)。`dispatchPR` 只用 `errors.Is` 分流:

| 错误源 | known stderr / 信号 | sentinel | unknown 行为 |
|---|---|---|---|
| `RemoteBranchExists` | `could not read Username` / `unable to access` / `does not appear to be a git repository` | (带 hint 字符串) | 原 stderr 透传,**绝不**二次翻译 |
| `provider.{FindOpenPRForBranch,CreatePR}` | `gh` / `glab` 可执行文件不存在 | `ErrCLINotInstalled` | 原 stderr 透传 |
| `provider.CreatePR` race-window | gh GraphQL 4 条 / glab 3 条 "head missing on origin" | `ErrStaleUpstream` | 原 stderr 透传 |
| `provider.CreatePR` race-window | `already exists` | `ErrPRExists`(已存在) | 原 stderr 透传 |

helper 实现位于:
- `internal/command/gtw/worktree.go::RemoteBranchExists`(lsRemoteKnownErrors 表)
- `internal/command/gtw/provider.go::wrapCreatePRError` / `wrapListPRError`
- `internal/command/gtw/provider.go::isExecutableNotFound`
- `internal/command/gtw/provider.go::classifyCreatePRError`(GitHub / GitLab 各一份)+ `ghStaleUpstreamSubstrings` / `glStaleUpstreamSubstrings`

### A4.3 A2 设计的哪些部分保留 / 哪些被替换

| A2 设计元素 | 状态 |
|---|---|
| `CollectReadinessForDispatch` / `verifyUpstreamOnOrigin`(A2 §A2.F-237)| **保留**给 `/gtw push` 和 `/gtw commit` 用 — 它们的 stale-cached-ref 探测仍是真 bug;**移除** PR 路径的调用 |
| `GitStatusSnapshot` 字段 / `parsePorcelainBranchStatus` / `parseBranchHeader` | **保留** — runtime footer 还在用 |
| `WorkingTreeIsClean` / `HasNothingToPush` / `PushBlockReason` / `HasUpstreamBranch` / `LocalIsAtUpstreamTip` / `UpstreamGone` 字段 | **保留** — push 路径的 `HasNothingToPush` 仍消费 `HasUpstreamBranch` + `UpstreamGone` |
| `PRBlockReason` / `IsReadyForPR` | **删除** — 0 caller |
| `dispatchPR` 的 6 维 `if reason := snap.PRBlockReason()` 分支 | **删除** |
| `countBaseAhead` + "nothing new to PR yet" 分支 | **删除** — ahead=0 也发,gh/glab 自己说 |
| `mapGhHeadMissingToHint` 字符串翻译 | **删除** — 替换为 sentinel-based flow + `provider.classifyCreatePRError` helper |

### A4.4 为什么 PR 路径独立,不复用 push 的 `verifyUpstreamOnOrigin`

PR 路径用的是**直接 ls-remote**,不 mutate snap,不 mutate snap 就
不需要 push 那条 "CollectReadiness + verifyUpstreamOnOrigin" 链。
两条路径语义不同:

- push: 需要 stale-cached-ref 检测(避免 push 把 stale `ahead=0`
  当成"nothing to push"漏推)
- pr: 只需要 ls-remote **单次**返回 — 没 ref 就拒绝,有就 pass

强行共用 `CollectReadinessForDispatch` 让 PR 路径付了它不需要的代价
(多一次 git status 调用,多一次 snap mutate),也得承担 push 的
graceful-fallback-on-network-error 语义(错误路径更曲折)。

### A4.5 测试覆盖

`internal/command/gtw/pr_test.go` 共 149 个测试,涵盖:
- 两道闸的 pass / fail 各路径
- 已知 stderr 翻译(auth / network / not-a-repo)
- 未知 stderr passthrough(关键不变量 — 不能匹配任何 known fragment)
- race-window 兜底(branch 被删 / PR 被人开了 / gh/glab 突然没装)
- dirty / ahead / behind / conflicts 不再 gate
- helper 单元测试(`wrapCreatePRError` / `wrapListPRError` /
  `isExecutableNotFound` / `classifyCreatePRError`)
- production `GitHubProvider.FindOpenPRForBranch` / `GitLabProvider.FindOpenPRForBranch` 直接单元测试(argv shape + JSON decode + empty list)

### A4.6 不在范围内

- `/gtw commit` / `/gtw push` 的 readiness 完全不动 — 它们的语义
  (本地状态决定能不能 push / commit)没变
- 不引入"如果 dispatchPR 失败,自动 revert gh 状态"的逻辑 —
  gh/glab 创建 PR 失败本来就只是拒绝,无需回滚
- 不改 `loadDispatchContext` 的 detached HEAD 检查 — yml 路径
  (`Branch == ""` reject)和非 worktree 路径(`branch == "HEAD"`
  reject)仍守住 dispatchPR 入口
- F-237 的 `verifyUpstreamOnOrigin` 不删 — push/commit 仍在用

---

## A5. `/gtw commit` · `/gtw pr` 的 `--continue` / `-g` / `--unset`

### A5.1 旗标面

```
/gtw commit [-a|--agent <name>] [-c|--continue] [-g|--global] [--unset]
/gtw pr    [-a|--agent <name>] [-c|--continue] [-g|--global] [--unset] [--ready]
```

三个新旗标照抄 `/review` 的那一套(`-a` 选 agent、`-g` 持久化、`--unset` 删掉、
`-c` 接着上次没做完的跑),组合校验也照 `/review` 的 `validateReviewFlags` 写,
只去掉 gtw 没有的两个概念(`--abort` / `--timeout`):

| 组合 | 报什么 |
|---|---|
| `--continue` + `--global` | `--continue and --global are mutually exclusive` |
| `--continue` + `--unset` | `--continue and --unset are mutually exclusive` |
| `--unset` + `--agent` | `--unset and --agent are mutually exclusive` |
| `--unset` 不带 `--global` | `--unset requires --global` |
| `--global` 不带 `--agent` | `--global requires --agent <name>` |
| `--continue` + `--agent` | 允许 —— 这就是接管 |
| `--continue` + `--ready` | 允许 —— `--ready` 正交 |

旗标的词法分析走共享的 `command.ParseCmdArgs`:未声明的旗标仍然是
`unknown flag`,位置参数仍然被拒。

### A5.2 记录(last run)

每个 **chat + 子命令**记一条"上次运行":

| 字段 | 是什么 |
|---|---|
| `agent` | 跑它的 agent 名(那次没有 agent 可跑时为空) |
| `ctx` | 失败那一刻冻结的工作区上下文(worktree / branch / repoRoot / issue) |
| `prompt` | 最后一次尝试的 prompt(自包含) |
| `sessionID` | 最后一次尝试的 session id(到过 agent 才有) |
| `failed` | 上一次是不是失败 |

存在 gtw 的 `Manager` 里(`recordStore`),**只在内存** —— daemon 重启即失,
和 `/review` 的续跑状态同款。`HandlerDeps` 把这个 store 递给 dispatcher,
所以 dispatcher 不需要 manager 本身。

什么时候写:agent 阶段失败时。commit 的两种是 `500`(agent 没跑起来)
与 `409`(agent 跑了但没提交);pr 的两种是 `500` 与格式门
失败(`500` + `format` / `attempts`)。git 层的拒绝(冲突、游离 HEAD、
工作树干净)不进记录 —— 那些没有"接着干"的活,重打命令就行。

记录里的 `agent` 可能是空的:那次失败就是"没有 agent 可跑"(用户还没 `/use`)。
续跑这时不拿空名字去查注册表(那只会回一句 `unknown agent ""`),而是按
正常的三档重新解析一次 —— 用户这中间可能已经选好了 agent。

什么时候清:该子命令这次跑成功时,记录换成一条"成功"的 —— `--continue`
因此能说出"上次已经跑完了",而不是"没有可续的"。

### A5.3 续跑

| 上一次的状态 | `--continue` 做什么 | 为什么 |
|---|---|---|
| 有 session id | `agent.ResumeSession` 重开那个 session,发一句 `continue` | session 那边还留着原始 prompt(含输出格式要求),agent 知道要交什么 |
| 没有 session id | 用存的 prompt 跑一次新 session | agent 没起来过,没有对话可续 |
| 配了 `-a <别的 agent>` | 在新 agent 上重放存的 prompt | session 不能换 agent,只能重放 prompt |

续跑用记录里冻结的 `ctx`,不要求 chat 的 cwd 还在那里 —— session 属于那个
工作区。续跑同步执行(gtw 的命令本来就是同步的),照常走
`commit.before` / `commit.after`(pr 同理)与超时(`timeouts.Command` +
`timeouts.Agent`),并走完 agent 阶段之后的全部流程:commit 照常验 HEAD、
出成功卡;pr 照常过格式门、真的开出 PR。

不搬 `/review` 的"只许续一次"(`spent`)规则:commit 的活还在工作树上,再续
一次合理。每次续跑更新记录,形成一条链。

### A5.4 全局档

`-g` 把这次的 agent 写进 `~/.nightme/gtw.yml` 的 `commit.agent` /
`pr.agent`(走 `writeUserConfig` + `yamlfile` 的节点编辑,hooks 与未知键不动);
`--unset` 删掉它(section 空了也删)。这两个键本来就在 `Config` 的 schema 里
(`CmdConfig.Agent`),只是没有命令能写。

`-g` 的名字必须是注册过的 agent(`agent.Builtins.Get`):写下一个没人认的名字,
之后每次 `/gtw commit` 都得靠警告兜底。名字不认识 → `unknown_agent`,
不写,命令不跑。yml 写不了 → 只出警告,命令照跑,结论是 `incomplete` +
`reason=config_write_failed`。

### A5.5 原因码

`no_continue`:没有可续的记录(失败 + `Data["reason"] == "no_continue"`)。
"上次跑完了,没东西可续"是 noop,不带原因码。

### A5.6 不在范围内

- `/review` 的 `--abort` / `--timeout` —— gtw 没有这两个概念
- `/gtw push` / `/gtw fix` 等其它 gtw 子命令的旗标
- gtw.yml 的 `lang` 与 `repos` 档位、`/gtw config` 的旗标面
- `/gtw pr view`(只读,不跑 agent)
