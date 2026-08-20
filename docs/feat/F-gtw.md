# /gtw 命令 (push / pr)

## A1. F-56: `/gtw push` 三分支流 + Agent 最小化 Prompt

> **Source**: `F-gtw.md`


> **Depends on**: F-104 (`/gtw push` 3 档 agent 优先级 + yml `~/.nightme/gtw.yml`)、F-120 (PR #120 `fix(gtw): harden push verification and PR cache correctness` — 本次新增的 HEAD-advance check)、`internal/command/gtw/push.go::headSHA`、`internal/command/gtw/push.go::countUnpushed`、`internal/command/gtw/push.go::listUncommittedFiles`

> **Related**: [`wip/gtw-hooks.md`](../wip/gtw-hooks.md)（agent 优先级 + 3 档 chain）、`internal/command/gtw/README.md`（gtw 回复排版规约）、PR #120 commit `77a8023`（HEAD-advance check 的来源）

---

## 0. TL;DR

把 `/gtw push` 拆成 **3 个互斥的分支**,由初始 `git status` + `countUnpushed` 决定走哪条:

1. **空操作分支**:worktree 干净且没有 unpushed commit → 提示用户,退出。
2. **agent commit 分支**:worktree dirty → 调 agent 跑 prompt(只 commit,不 push) → 校验 commit 完整 → 工程化 push。
3. **直推分支**:worktree 干净但有 unpushed → 工程化 push,agent 不参与。

Agent 的职责从"commit + push + 报告结果"收窄到 **"把当前 working tree 的改动按相关性拆成若干条 commit"**。Push 决策、push 执行、push 失败兜底、IM 卡片拼装,全部由 nightme 工程化代码根据 git 状态完成 —— 不再读 agent 的 prose 输出作为 ground truth。

Agent prompt 从 5 步 checklist 收窄到 **role + task + 3 条硬规则** (~130 tokens)。verification 不再做 SHA-parse 比对(原本就计划上,本设计后**取消** —— agent 不 claim SHA,不需要 parse)。

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

当前 prompt(`internal/command/gtw/commit_push.go::buildAgentPrompt`):

```
1. Run `git status` and `git diff` to inspect changes.
2. Write a Conventional Commits commit message.
   ...
3. `git add -A && git commit -m "<msg>"` (heredoc if multi-line).
4. `git push -u origin <branch>`.
5. Reply with: <commit_hash> <one-line summary>
```

5 步 + 5 类失败模式(LLM 都会遇到):

| 失败模式 | 谁的责任 |
|---|---|
| step 2 写错 message 格式 | LLM |
| step 3 commit hook 失败但 LLM 吞了 stderr | LLM |
| step 3 commit 失败 → LLM 跑 `git restore` 想"重置" | LLM(**静默吞用户改动**,本次 bug 高度疑似) |
| step 4 push 失败 → LLM 假装 push 成功 | LLM |
| step 5 LLM 把现有 SHA 当新 SHA 报 | LLM |

5 类失败中,**只有 1 类**(step 1-2,真正需要 LLM 判断的)对 prompt 有依赖。其余 4 类都是 LLM 在边界外的副作用 —— 最好的"修复"是 **不把那些事交给 LLM**。

### 1.3 三个根本性反思

设计本次重构时,经过 4 轮 prompt 迭代,逐步提炼出 3 条原则:

1. **Agent = "需要 judgment 的事",nightme = "infra 副作用的事"**
   - LLM 强项:看 diff、定 commit 边界、写 message
   - LLM 弱项:跑网络操作、判断 push 是否成功、稳定回报状态
   - 因此:agent 只 commit,nightme 自己 push
2. **Git = source of truth,不是 agent 的 prose**
   - LLM 可以 confabulate,但 `git log` 不会
   - 因此:nightme 从 `git log` 拿真实 commit 列表,agent 说什么无所谓
3. **Prompt = "如果 agent 是一个独立 CLI 工具,这个 prompt 就是它接收的命令"**
   - 不该让 agent 知道"谁在调用它"、调用方会怎么处理它的输出
   - 因此:prompt 里**没有** nightme 的 verification / push / 卡片 / "FAILED:" 输出格式

---

## 2. The 3 Branches

`dispatchPush` 启动时拍两张快照:

```go
isClean        := strings.TrimSpace(statusOut) == ""        // git status --porcelain
unpushedBefore := countUnpushed(worktree, branch)          // git rev-list @{u}..HEAD
```

| Branch | `isClean` | `unpushedBefore` | 动作 | Agent? |
|---|---|---|---|---|
| **Branch 1 (no-op)** | ✅ | 0 | 提示用户"无活儿干",退出 | ❌ |
| **Branch 2 (commit + push)** | ❌ | 任意 | agent commit → verify → nightme push | ✅ |
| **Branch 3 (push only)** | ✅ | >0 | nightme 直接 push | ❌ |

### 2.1 Branch 1: no-op

```go
if isClean && unpushedBefore == 0 {
    return reply(...,
        "ℹ️ nothing to push\n"+
        "  no uncommitted changes\n"+
        "  no unpushed commits on " + c.Branch), nil
}
```

不调 agent,不调 push。直接退出。这是**新增的 branch**,当前代码对这种情况返回的应该是 `pushClean` 路径(然后 push 0 commit 上去,IM 卡片错乱)。

### 2.2 Branch 2: dirty → agent → verify → push

```go
if !isClean {
    // (1) 调 agent 跑新 prompt
    name, agentErr := runAgentToCommit(ctx, cs, deps, c, args, ymlAgent,
        chatID, messageID)
    if agentErr != "" {
        return reply(..., agentErr), nil
    }
    agentName = name

    // (2) verify: HEAD 动了 + worktree 干净
    if msg := verifyAgentCommitted(ctx, deps, c, headBefore); msg != "" {
        return reply(..., msg), nil  // 失败,不 push
    }

    // (3) 重新数 unpushed(agent 也可能没 commit,看 verify 结果)
    unpushedBefore, _ = countUnpushed(ctx, c.Worktree, c.Branch, deps)
}
```

**`runAgentToCommit`**:单纯 spawn agent 跑 prompt(§3)。不读 agent 输出的 prose。

**`verifyAgentCommitted`**:纯 git 状态校验(§4.1)。HEAD 必须动 + worktree 必须 clean。

### 2.3 Branch 3 (或 Branch 2 续): worktree 干净 → push

```go
if unpushedBefore == 0 {
    return reply(...,
        "⚠️ worktree is clean but nothing to push.\n"+
        "hint: ..."), nil  // 防御:agent 跑了但没 produce commit
}

if err := programmaticPushWithRetry(ctx, deps, c); err != nil {
    commits := gitLogRange(ctx, c.Worktree, headBefore + "..HEAD", deps)
    return reply(...,
        "❌ push failed: " + err.Error() + "\n\n"+
        "commits (local, not on origin):\n" + formatCommits(commits)), nil
}

return replySuccessCard(ctx, deps, c, headBefore, agentName, isClean)
```

**`programmaticPushWithRetry`**:工程化 push,带 retry。封装现有 `verifyPushedAndRetry` 的 push 循环,从 commit_push.go 抽出来。

**`replySuccessCard`**:从 git log 拼 IM 卡片,跟 agent 输出完全无关(§5)。

### 2.4 状态机图

```
              ┌──────────────────────────┐
              │   dispatchPush entry     │
              │   snapshot headBefore,   │
              │   unpushedBefore, isClean│
              └────────────┬─────────────┘
                           │
        ┌──────────────────┼──────────────────┐
        │                  │                  │
        ▼                  ▼                  ▼
   Branch 1            Branch 2           Branch 3
   isClean &&          !isClean            isClean &&
   unpushed==0                             unpushed>0
        │                  │                  │
   ℹ️ no-op            agent commit         │
        │                  │                  │
        │                  ▼                  │
        │             verify:                 │
        │             HEAD advanced?          │
        │             worktree clean?         │
        │                  │                  │
        │            ┌─────┴─────┐            │
        │            │           │            │
        │          FAIL         OK            │
        │            │           │            │
        │       ⚠️ 错误     recount unpushed   │
        │       (no push)        │            │
        │                        ▼            │
        │                 ┌──────────────────┘
        │                 │
        │                 ▼
        │          programmaticPushWithRetry
        │                 │
        │           ┌─────┴─────┐
        │           │           │
        │         FAIL         OK
        │           │           │
        │      ❌ push 失败    replySuccessCard
        │      (列出本地)     (从 git log 拼)
        │           │           │
        └───────────┴───────────┘
                    │
                    ▼
              return reply(...), nil
```

---

## 3. Agent Prompt (Minimal)

### 3.1 完整 prompt 文本

```
You are a release engineer.

The user has uncommitted work on branch <BRANCH> in <WORKTREE>
[for issue #<N>]. They need it committed to local git.

Group the changes by relevance — different concerns go in
different commits, related changes go together. Use
Conventional Commits for each:

  <type>(<scope>): <subject>
  types: feat, fix, chore, refactor, docs, test, build,
         ci, perf, style, revert
  subject: ≤72 chars, imperative, no trailing period
  body: WHY, wrapped at 72  [<Issue: #N>] if applicable.

Rules:
- Do not push. Push is the user's decision, not yours;
  never run `git push`.
- Do not revert, restore, or stash the user's work.
- `git add <specific files>`, not `git add -A`.
```

**总长 ~130 tokens**(之前 ~250+ tokens)。三段结构:

| 段 | 作用 |
|---|---|
| Role | 你是 release engineer |
| Task | 拆 + commit 到本地 + Conventional Commits 格式 |
| Rules | 3 条硬规则:不 push / 不 restore / 不 `-A` |

### 3.2 这版 prompt 跟之前对比删了什么

| 之前 | 现在 | 为什么删 |
|---|---|---|
| "OUTPUT POLICY" 段(讲 agent 怎么不输出) | 删 | nightme 怎么用 agent 输出,不该 agent 知道 |
| "COMMIT FAILED: <reason>" 输出格式 | 删 | nightme 从 git 状态判断,不需要 agent 汇报 |
| "If a commit fails, stop and report" | 删 | agent 用工具反馈自然知道 commit 是否成;nightme 兜底 |
| Procedure 1-3 详细步骤 | 删 | 步骤拆太细鼓励"checklist 心态",反而不让 LLM 判断 |
| "Verify HEAD advanced" 步骤 | 删 | nightme 在外面 verify;内部 verify 跟外面打架 |
| "Why we hand this to an agent" 等元说明 | 删 | LLM 不需要 meta context |
| "Stop. Do not push. Do not run any other git command." 收尾 | 删 | 跟 Rules 段重复 |

### 3.3 为什么这 3 条 Rule 留着

1. **"Do not push"** —— 用户明确要求。LLM 真会想"commit 完顺手 push 一下",prompt 没说就会自己加(PR #120 当天 Pi 高度疑似这么干过)
2. **"Do not revert, restore, or stash"** —— 本次 bug 的根因。LLM 在 commit 失败时倾向用这些命令"清理"再重来,**会静默吞用户改动**。必须禁
3. **"`git add <files>`, not `-A`"** —— `-A` 会带进 untracked 临时文件(`.DS_Store` / `*.swp` / `*.log`),LLM 不会自己过滤

每条 Rule 都回答一个问题:"不告诉 agent 它就不会犯这个错吗?" —— 答案是**会**,所以留着。

### 3.4 通用 prompt 设计原则(从这次提炼)

1. **Agent 的 prompt 应该写成"如果它是一个独立 CLI 工具,这就是它接收的命令"**。可移植、不带调用方上下文
2. **Agent 不该知道"谁"在调用它**。verification / retry / 卡片渲染 是 nightme 自己的事
3. **每个约束都该回答"agent 不知道这条会犯什么错"**。否则就删

---

## 4. Verification Chain

### 4.1 `verifyAgentCommitted` (新)

替代当前 `verifyAgentPushedAndRecover` 的 "agent 是不是 commit 了" 部分(push 那部分由 `programmaticPushWithRetry` 接管)。

```go
func verifyAgentCommitted(ctx, deps, c, headBefore string) string {
    headAfter, err := headSHA(ctx, c.Worktree, deps)
    if err != nil {
        return "⚠️ agent finished but failed to read HEAD: " + err.Error()
    }

    // (a) HEAD 必须动
    if headAfter == headBefore {
        return fmt.Sprintf(
            "⚠️ agent finished but no new commit was created "+
            "(HEAD unchanged at %s).\n"+
            "hint: the worktree was dirty before the agent and may "+
            "not be clean now. Inspect manually.",
            shortSHA(headBefore))
    }

    // (b) worktree 必须干净
    uncommitted, err := listUncommittedFiles(ctx, c.Worktree, deps)
    if err != nil {
        return "⚠️ agent finished but failed to verify clean state: " + err.Error()
    }
    if len(uncommitted) > 0 {
        return fmt.Sprintf(
            "⚠️ agent finished but %d file(s) still uncommitted in %s:\n"+
            "  %s\n"+
            "hint: commit them manually, or re-run /gtw push to retry.",
            len(uncommitted), c.Worktree,
            strings.Join(uncommitted, "\n  "))
    }
    return ""
}
```

只校验"agent 真的 commit 了",**不**校验 push(push 是 nightme 自己的事)。

### 4.2 为什么不需要 SHA-parse 增强

之前的 F-56 设计草稿考虑过:解析 agent reply 里的 `<sha>` claim,跟 headAfter 比对。**这个增强在当前设计下取消**,原因:

- 设计:agent claim 3 条 SHA → 必须 parse 核对
- 新设计:agent **不 claim** 任何 SHA → `git log headBefore..HEAD` 是 ground truth → 没法不一致

LLM 没法靠少报 / 错报 SHA 来骗系统,因为 **agent 根本不被要求报 SHA**。

### 4.3 `programmaticPushWithRetry` (新,抽自 `verifyPushedAndRetry`)

```go
func programmaticPushWithRetry(ctx, deps, c) error {
    // 跟当前 verifyPushedAndRetry 一致:
    // 1. git push -u origin <branch>
    // 2. 失败再 git push -u origin HEAD
    // 3. countUnpushed 验真
    // 抽出来同时给 Branch 2 续 push 和 Branch 3 直推复用
}
```

---

## 5. IM Card 格式

### 5.1 Branch 1 — no-op

```
ℹ️ nothing to push
  no uncommitted changes
  no unpushed commits on docs-readme
```

emoji 用 `ℹ️`(信息,不是错误也不是警告)。这是**新增的 case**,当前代码对这种 case 的回复错乱(`pushClean` 跑完 0 commit push,IM 卡片会显示 "✅ pushed 0 commits")。

### 5.2 Branch 2 — agent commit + push

```
🤖 pi committed 2 change(s) and pushed to docs-readme:
  aabbcc1 feat(gtw): split verification into 3 branches
  ddeeff2 fix(gtw): cancel push on agent failure

```

emoji `🤖`(agent 主导)。commit 列表来自 `git log headBefore..origin/<branch>`,不是 agent prose。

### 5.3 Branch 3 — push only

```
✅ pushed 3 commit(s) to docs-readme:
  94c4a38 docs: add chat input routing and shell mode documentation to README
  77a8023 fix(gtw): harden push verification and PR cache correctness
  459b968 docs(README): clarify session management commands, drop stale gtw hook

```

emoji `✅`(单步完成)。commit 列表同样来自 git log。

### 5.4 Branch 2 失败(agent 没 commit 全)

```
⚠️ agent finished but 2 file(s) still uncommitted in <worktree>:
  new-secret.env
  scratch.txt
hint: commit them manually, or re-run /gtw push to retry.
```

不 push,直接退出。emoji `⚠️`(警告 / 部分成功)。这条跟当前 `verifyAgentPushedAndRecover` 的 uncommitted check 输出对齐。

### 5.5 Branch 2/3 失败(push 自己失败)

```
❌ push failed: <error>

commits (local, not on origin):
  aabbcc1 feat(gtw): split verification into 3 branches
  ddeeff2 fix(gtw): cancel push on agent failure
```

emoji `❌`(终态失败)。列出**本地但没推上去**的 commit,让用户知道哪些需要重试。

### 5.6 emoji 一致性

跟 `internal/command/gtw/README.md §1.1` 的 emoji 字典保持一致:

| emoji | 含义 | 适用 case |
|---|---|---|
| `ℹ️` | 无副作用的"现状" | Branch 1 |
| `🤖` | agent 主导 | Branch 2 |
| `✅` | 单步动作完成 | Branch 3 |
| `⚠️` | 警告 / 部分成功 | Branch 2 verify 失败 |
| `❌` | 终态失败 | push 失败 |

`ℹ️` 是新增(README §1.1 表里没有),**需要顺手在 README §1.1 字典里登记**。

---

## 6. 不变式 Checklist

- **Agent 不 push**。prompt 显式禁止;nightme 兜底 push 跟 agent 行为完全解耦
- **Agent 不汇报**。prompt 不要求"final reply"格式;agent 输出几乎完全被忽略(仅 agent 进程崩溃时做 stderr fallback)
- **Git 是 source of truth**。IM 卡片、commit 列表、push 状态全部从 `git log` / `git rev-parse` 拿,不读 agent prose
- **HEAD 校验必经**。Branch 2 必跑 `verifyAgentCommitted`,任一 case 失败 → 不 push
- **Push 校验必经**。`programmaticPushWithRetry` 内部 `countUnpushed` 兜底,失败 → IM 报错并列出本地未推 commit
- **Branch 1 不能误吞**。无活儿干时必须明确告诉用户,不能假装"推了 0 条 commit"
- **Conflict 永远先报**。`detectConflicts` 在 snapshot 之前跑,跟原 dispatchPush 一样

---

## 7. 错误处理矩阵

| 触发条件 | IM 输出 | 后续动作 |
|---|---|---|
| 初始 `git status` 出错 | `❌ git status: <err>` | 退出 |
| Conflict 存在 | `❌ worktree in conflicted state` | 退出 |
| Branch 2:无 agent 选中 | `❌ no agent selected. /use <name> first` | 退出(不 push) |
| Branch 2:agent 进程崩溃 / RunOnce 失败 | `❌ agent <name> failed: <err>` | 退出(不 push) |
| Branch 2:agent 跑完 HEAD 没动 | `⚠️ no new commit was created (HEAD unchanged at <sha>)` | 退出(不 push) |
| Branch 2:agent 跑完 worktree 仍 dirty | `⚠️ N file(s) still uncommitted: ...` | 退出(不 push) |
| Branch 2/3:`programmaticPushWithRetry` 失败 | `❌ push failed: <err>` + 列出本地 commit | 退出(commit 在本地) |
| Branch 1 | `ℹ️ nothing to push` | 退出 |
| Branch 2 成功 | `🤖 <agent> committed N change(s) and pushed to <branch>: ...` | 退出 |
| Branch 3 成功 | `✅ pushed N commit(s) to <branch>: ...` | 退出 |

---

## 8. 测试计划

### 8.1 单元测试(`commit_push_test.go`)

| 测试 | 覆盖 case |
|---|---|
| `TestDispatchPush_NoOp_BothClean` | isClean && unpushed==0 → Branch 1,IM 含 `ℹ️ nothing to push` |
| `TestDispatchPush_Dirty_AgentCommits` | isClean=false → Branch 2,agent 跑完 HEAD 动 + worktree 干净 → push 成功 → IM 含 `🤖 ... pushed` |
| `TestDispatchPush_Dirty_AgentNoCommit` | isClean=false → Branch 2,agent 跑完 HEAD 没动 → IM 含 `⚠️ no new commit`,**不**调 push |
| `TestDispatchPush_Dirty_AgentPartialCommit` | isClean=false → Branch 2,agent 跑完 worktree 仍 dirty → IM 含 `⚠️ N file(s) still uncommitted`,**不**调 push |
| `TestDispatchPush_Clean_WithUnpushed` | isClean && unpushed>0 → Branch 3,直推,IM 含 `✅ pushed N commit(s)` |
| `TestDispatchPush_Clean_NoUnpushed` | isClean && unpushed==0 → Branch 1(同 NoOp) |
| `TestDispatchPush_PushFails_ListsLocalCommits` | Branch 2 或 3,push 失败 → IM 含 `❌ push failed: ...` + 本地 commit 列表 |
| `TestDispatchPush_AgentCrash_NoPush` | Branch 2,agent 进程 RunOnce 返回 err → IM 含 `❌ agent failed`,**不**调 push |

### 8.2 关键不变式

```go
// pushDryBug: verifyAgentPushedAndRecover 在 agent 没真 commit 时
// 返回 "✅ pushed" 的 false-success。新设计应该返回 "⚠️ no new commit"
// + 不调 push。
func TestDispatchPush_NoCommitNoFalseSuccess(t *testing.T) { ... }

// prompt: 验证 buildAgentPrompt 不包含 "push" 命令字样
// (除了"Do not push"这一行)
func TestBuildAgentPrompt_NoPushCommand(t *testing.T) {
    prompt := buildAgentPrompt(ctx)
    if strings.Count(prompt, "git push") > 0 {
        // 只允许"Do not push"那一行
        // 实际检查:除"Do not push"段外,不含 "git push" 命令
    }
}

// prompt: 验证不含 nightme 实现细节的词
func TestBuildAgentPrompt_NoNightmeDetails(t *testing.T) {
    prompt := buildAgentPrompt(ctx)
    forbid := []string{"FAILED:", "verify", "git log", "origin/", "🤖"}
    for _, w := range forbid {
        if strings.Contains(prompt, w) {
            t.Errorf("prompt leaked nightme detail: %q", w)
        }
    }
}
```

### 8.3 E2E / manual smoke

- 真跑一次 `/gtw push` 在 docs-readme worktree(类似今天 case):验证 IM 卡片是从 git log 拼的(commit subject 跟 git log 完全一致),agent prose 不会出现在卡片里
- 用 `--agent` 强制指定 pi / claude / codex,确认 3 个 agent 都按 prompt 跑
- 故意在 agent prompt 注入 "fake commit successful" 的指令,看 verify 是否抓得到

---

## 9. 不在范围内(Out of Scope)

- **`/gtw fix` / `pr` / `close` / `sync` 的重构**:本次只动 `push`。`fix` 的 worktree 创建、`pr` 的 one-shot agent、`close` 的 worktree 销毁、`sync` 的 pull,各自有不同职责,不在本次改造范围
- **agent prompt 的"是否要在 commit 失败时输出 FAILED"**:本次设计选择**完全不让 agent 汇报**。如果将来调试需要,可以在 agent 跟 nightme 之间加一个 transcript 日志,但**不**让 FAILED 进 prompt
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
| `programmaticPushWithRetry` 抽出来 | Branch 2 续 push 跟 Branch 3 直推共用 | 复制 pushClean 逻辑 |
| Branch 1 新增 no-op IM | 当前 0 commit push 错误地返回 `✅ pushed 0 commits`,误导用户 | 静默 no-op(UX 差) |
| `ℹ️` 加入 gtw emoji 字典 | Branch 1 需要"信息但非错误"标志 | 用 `✨`(含义是"无副作用的现状",勉强但不准) |
| 删 `pushClean` 函数 | 被 Branch 3 收编,函数重命名 + 复用更清楚 | 保留 pushClean 给 Branch 3 用(代码重复) |

---

## 11. 实施清单

| Phase | 内容 | 涉及文件 |
|---|---|---|
| **Phase 0** | 本 F-56 文档 + `wip/gtw-push.md` 同步(可选) | `docs/feat/本文件`(本文件) |
| **Phase 1** | `buildAgentPrompt` 收窄到 130 tokens(只 role + task + 3 rules) | `internal/command/gtw/commit_push.go` |
| **Phase 2** | 抽 `programmaticPushWithRetry` 出来,从 `verifyPushedAndRetry` 复用 | `internal/command/gtw/push.go` |
| **Phase 3** | 重写 `dispatchPush` 为 3 branch(替换 `pushClean` / `pushDirty` 分裂) | `internal/command/gtw/commit_push.go` |
| **Phase 4** | 新增 `verifyAgentCommitted`(只校验 commit,不校验 push) | `internal/command/gtw/commit_push.go` |
| **Phase 5** | 新增 `replySuccessCard`(从 git log 拼 IM 卡片) | `internal/command/gtw/render.go` |
| **Phase 6** | 删 `verifyAgentPushedAndRecover` / `pushClean` / `pushDirty` | `internal/command/gtw/commit_push.go` |
| **Phase 7** | `commit_push_test.go` 重写为 8 个 branch 覆盖测试 | `internal/command/gtw/commit_push_test.go` |
| **Phase 8** | gtw README §1.1 emoji 字典加 `ℹ️` | `internal/command/gtw/README.md` |
| **Phase 9** | `docs/FEATURES.md` 加 F-56 索引行 | `docs/FEATURES.md` |
| **Phase 10** | `CHANGELOG.md` Unreleased 段加 F-56 entry | `CHANGELOG.md` |

**总 PR 建议:拆 2 个 PR**

- **PR 1 (本期)**:Phase 1-7(代码 + 测试)
- **PR 2 (后续)**:Phase 8-10(文档同步 + emoji 字典 + CHANGELOG)

PR 1 不依赖 PR 2,可以独立 merge。

---

## 12. 风险与回滚

### 12.1 风险

| 风险 | 缓解 |
|---|---|
| prompt 130 tokens;某些 edge case 缺少指令 | `commit_push_test.go` 8 个 branch case 覆盖;manual smoke 后再 merge |
| `programmaticPushWithRetry` 抽出来 pushClean 删了,测试可能 break | Phase 7 重写测试,pushClean 引用全清 |
| Branch 1 新增 no-op IM 卡片,旧用户期待"推 0 条"也回 `✅` | UX 是 bug 修复,不改;CHANGELOG 明示"no-op case 改为 `ℹ️`" |
| agent 在新 prompt 下不再 push,某些用户依赖"agent 自己推"的工作流 | prompt 改动是 additive breaking;CHANGELOG 明示"agent 不再 push,nightme 接管" |

### 12.2 回滚

`dispatchPush` 重写 + `buildAgentPrompt` 收窄是单一 PR 内的原子改动,git revert 即可完整回滚到 PR 前的 `pushClean` / `pushDirty` 模型。无需数据迁移、无需 schema 改动。

### 12.3 监控

merge 后 1 周内观察:
- `/gtw push` 的成功 / 失败 IM 卡片分布
- Branch 1 (no-op) 触发频率(预期不高,但应该被正确识别)
- Branch 2 中 `verifyAgentCommitted` 触发率(应该 < 5%,否则说明 prompt 不够好)
- push 失败频率(Branch 2/3 共用 `programmaticPushWithRetry`)

---

## 13. 后续 PR(不在本次)

| 任务 | 优先级 | 说明 |
|---|---|---|
| 把 `commit_push.go` 拆成 `commit_push.go`(commit 流程)+ `push.go`(push 流程),更符合 3 branch 模型 | P2 | 现在两个职责还在同一文件,Phase 3 后函数签名会变长,值得拆 |
| `replySuccessCard` 接入 `internal/channel/feishu/card` 的 format helper,跟 footer / checklist 风格统一 | P2 | 现在 replySuccessCard 自己拼字符串,Feishu adapter 那边有现成的 format helper 没用 |
| 把"agent transcript 日志"加进 daemond 控制台,方便调试 agent 为什么没 commit | P3 | 现在 agent 输出被忽略,出错时只能从 git log 倒推;transcript 日志能直接看 LLM 在干什么 |
| `/gtw pr` 的 one-shot agent 同步收窄(类似 F-56 的 prompt 重写) | P3 | 现在 `/gtw pr` 的 agent 也有"commit + push + 报告"老问题,但优先级低 |
| 整套 F-56 思路延展到 `/gtw close` / `sync`,让 agent 只做"judgment 部分" | P3 | `close` 是销毁 worktree(纯 git 命令,不需要 agent);`sync` 是 pull(也纯 git 命令)。这两条可能根本不需要 agent |

---

## 14. 参考

- [`wip/gtw-hooks.md`](../wip/gtw-hooks.md) — gtw hooks + agent 优先级 3 档 chain(F-56 复用)
- [`internal/command/gtw/README.md`](../../internal/command/gtw/README.md) — gtw IM 卡片排版规约(emoji 字典、Format 1/2/3)
- `internal/command/gtw/commit_push.go` — 当前实现,本次重构的 source of truth
- `internal/command/gtw/push.go::headSHA` / `countUnpushed` / `listUncommittedFiles` — git 状态查询原语(本次复用)
- `internal/command/gtw/push.go::verifyPushedAndRetry` — 现有 push retry 逻辑,Phase 2 抽到 `programmaticPushWithRetry`
- `internal/command/gtw/commit_push_test.go:524-580` — `TestRunPush_DirtyAgentClaimsDoneButNoCommit`,本次 bug 的 regression test 起点
- PR #120 commit `77a8023` — HEAD-advance check 引入位置
- 2026-08-11 docs-readme worktree 实测 case:agent 报 `94c4a38 docs: ...`,HEAD unchanged at `94c4a3866268` —— F-56 的 motivation 起点

---

## A2. F-57: `/gtw push` + `/gtw pr` 联动 Readiness Gate

> **Source**: `F-gtw.md`


> **Depends on**: F-56 (`/gtw push` 三分支流)、F-50 (GitHub/GitLab Provider)、F-45 (Session footer) — 后者已经在 SessionContext 里跑过 `CollectStatus`,本设计只是扩字段+补谓词

> **Related**: [`本文件 §push 三分支`](./F-gtw.md)（push 三分支的细节）、[`internal/command/gtw/git_status.go`](../../internal/command/gtw/git_status.go)（snapshot 解析层）、[`internal/messages/footer.go`](../../internal/messages/footer.go)（`GitStatusSnapshot` 类型定义）

---

## 0. TL;DR

把"本地分支已经 commit + push + 与 origin 对齐"这件事,从一个散落在 `dispatchPush` 和 `dispatchPR` 里的隐含约定,显式建模成一个**共享的 `Readiness` 概念**:

1. **单一数据源**:一次 `git status --porcelain --branch --untracked-files=normal` 调用 → `GitStatusSnapshot`,扩两个字段(`BehindRemote`、`HasConflicts`)。
2. **三个正交原子谓词**:`HasUpstreamBranch()`、`LocalIsAtUpstreamTip()`、`WorkingTreeIsClean()` —— push 和 pr 都基于这三个判定,语义不再各自为政。
3. **业务判定分离**:`/gtw push` 用 `HasNothingToPush()` + `PushBlockReason()`;/gtw pr 用 `IsReadyForPR()` + `PRBlockReason()`。两者基于同一份 snapshot,得到的是同一份事实的不同切片。
4. **连续性保证**:`/gtw push` 成功结束的工作区,**必**使 `/gtw pr` 的 readiness 通过(并发 race 除外)。两个命令之间不再需要用户在 IM 里手动确认"我到底有没有 push 过"。

今天撞到的 case (`Head sha can't be blank, Head ref must be a branch`) 是这一设计的直接触发:本地 `refactor-outbound` 没有 upstream(`git config branch.refactor-outbound.remote` 空),`dispatchPR` 复用 `countUnpushed` 把"无上游"误读为"零未推送",把分支从未 push 的状态当成"已对齐",直接调 `gh pr create`,被 GitHub 的 GraphQL 拒掉。新设计在 readiness gate 里把 `HasUpstream` 单独作为一条维度,这种 case 在 `/gtw pr` 入口就被显式拦截,提示用户 `/gtw push first to publish the branch`。

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
| `main..HEAD`(本地) | 17 commit | `countBaseAhead` 返回 17,预检通过 |
| `refactor-outbound@{u}..HEAD` | fatal: no upstream | `countUnpushed` 返回 `(0, nil)`(把"无上游"误读为"零未推送") |
| dispatchPR 的结论 | — | "通过,继续调 gh" |
| gh 的结论 | — | "Head ref must be a branch",GraphQL 拒绝 |

`countUnpushed` 的语义是**为 `/gtw push` 量身定制的**(`push.go:122-141` 注释里写明:"no upstream configured → our '0 unpushed' signal"),直接被 `dispatchPR` 复用是把两个命令的语义混在一起,边界破了。

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
- `dispatchPR` 同样用 `countUnpushed` + `countBaseAhead` 两段独立探测,但取了一个不一样的 `countUnpushed` 语义

两个命令各管各的 snapshot,IM 用户体验上会出现"push 提示我'已经干净',pr 提示我'未推送'"的诡异跳跃。

把 readiness gate 提到共享层,**两个命令读同一份 snapshot**,产品语义就有了结构上的保证。

---

## 2. Readiness Snapshot

### 2.1 扩展 `GitStatusSnapshot`

在 `internal/messages/footer.go::GitStatusSnapshot` 加两个字段(footer 渲染层会忽略新字段,不用动渲染代码):

```go
type GitStatusSnapshot struct {
    Branch        string
    Uncommitted   int
    Untracked     int
    AheadOfRemote int
    BehindRemote  int   // ← 新增:D4 维度
    HasUpstream   bool
    HasConflicts  bool  // ← 新增:D6 维度
}
```

`parseBranchHeader` 补 `behind M` 解析(`footer.go:31` 注释里早就预留了"如果以后要 surface 'behind',在这里加")。`parsePorcelainBranchStatus` 在扫剩余行时识别 unmerged(`U*` / `*U` / `A*` / `*A` / `D*` / `*D` 且非 `??`),置 `HasConflicts`。

### 2.2 单一数据源 `CollectReadiness`

复用现有 `internal/command/gtw/git_status.go::CollectStatus`(已经跑 `git status --porcelain --branch --untracked-files=normal` 并解析为 snapshot),改名 + 文档化为:

```go
// CollectReadiness is the single source of truth for "what is the
// worktree's senior-dev git state right now?". One `git status
// --porcelain --branch --untracked-files=normal` call, fully parsed.
//
// Both /gtw push and /gtw pr call it at entry — they MUST agree on
// the snapshot, or the push→pr continuity breaks.
func CollectReadiness(ctx context.Context, dir string, git GitRunner) (*GitStatusSnapshot, error) {
    return CollectStatus(ctx, dir, git)
}
```

不破坏 `CollectStatus` 现有签名(footer 渲染路径还在调它),只是补注释;旧名字继续可用。

### 2.3 6 维度 = 业务判定空间的完整展开

| 维度 | snapshot 字段 | 业务含义 |
|---|---|---|
| **D1 真分支** | `Branch != ""` | 不能在 detached HEAD 上 PR/push |
| **D2 上游存在** | `HasUpstream` | 分支必须在 origin 上存在 |
| **D3 已推送** | `AheadOfRemote == 0` | 本地所有 commit 都已 push |
| **D4 同步** | `BehindRemote == 0` | 本地拿到了 origin 上所有最新 commit |
| **D5 工作区干净** | `Uncommitted == 0 && Untracked == 0` | 没有需要 commit 的本地改动 |
| **D6 无冲突** | `!HasConflicts` | 没有未解决的 merge/rebase conflict |

外加一条**与上述 6 项正交**的判断:

| 维度 | 信号来源 | 业务含义 |
|---|---|---|
| **P1 有东西可 PR** | `rev-list --count <base>..HEAD` | 该分支在 base 上**有新增**(PR-worthiness,与卫生无关) |

### 2.4 三个正交原子谓词

业务判定(2.5 节)拆成 3 个**只依赖 snapshot、不掺命令意图**的谓词。这样 push 和 pr 的差异只在"如何组合这三个谓词",不再有各自独立的 git 调用:

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
    return s.Uncommitted == 0 && s.Untracked == 0 && !s.HasConflicts
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
        s.AheadOfRemote == 0                // 也没有 ahead
}
```

注意 **"无上游" 不进 `HasNothingToPush`** —— 没有 upstream 的干净工作区恰恰是要首次 push 的合法场景。

### 3.2 dispatchPush 改写骨架

```go
func dispatchPush(ctx, cs, deps, chatID, messageID, args, ymlAgent) (*Result, error) {
    c, res := loadDispatchContext(ctx, cs, deps, chatID, messageID)
    if res != nil { return res, nil }

    snap, err := CollectReadiness(ctx, c.Worktree, deps.Git)
    if err != nil { return reply(...❌...), nil }

    // 1. 硬拒冲突
    if reason := snap.PushBlockReason(); reason != "" {
        return reply(ctx, cs.Emitter(), chatID, messageID, reason), nil
    }

    // 2. 没活可干 → 提示用户
    if snap.HasNothingToPush() {
        return reply(ctx, cs.Emitter(), chatID, messageID,
            "ℹ️ nothing to push\n"+
                "  no uncommitted changes\n"+
                "  no unpushed commits on "+snap.Branch), nil
    }

    headBefore, err := headSHA(ctx, c.Worktree, deps)  // 保留: agent verify 用
    var agentName string

    // 3. 工作区脏 → 走 agent commit 路径(F-56 Branch 2)
    if !snap.WorkingTreeIsClean() {
        name, errMsg := runAgentToCommit(ctx, cs, c, args, ymlAgent)
        if errMsg != "" { return reply(...), nil }
        agentName = name

        if msg := verifyAgentCommitted(ctx, deps, c, headBefore); msg != "" {
            return reply(...), nil
        }

        // agent 跑完必须重新 snapshot:不能信任原 snap
        snap, err = CollectReadiness(ctx, c.Worktree, deps.Git)
        if err != nil { return reply(...), nil }
        if reason := snap.PushBlockReason(); reason != "" {
            // agent 引入冲突? 罕见但要兜住
            return reply(ctx, cs.Emitter(), chatID, messageID, reason), nil
        }
    }

    // 4. 兜底: agent 跑完但 re-snapshot 后发现没活可干(agent 没真 commit 等边界)
    if snap.HasNothingToPush() {
        return reply(ctx, cs.Emitter(), chatID, messageID,
            "⚠️ worktree is clean but nothing to push.\n"+
                "hint: inspect HEAD vs origin/"+snap.Branch+" manually."), nil
    }

    // 5. 真·push(带重试 + 验证 unpushed==0)
    if err := programmaticPushWithRetry(ctx, deps, c); err != nil {
        return reply(ctx, cs.Emitter(), chatID, messageID, err.Error()), nil
    }

    // 6. 成功卡 —— 此时 snapshot: AheadOfRemote=0, HasUpstream=true, WorkingTreeIsClean=true
    card, err := replySuccessCard(ctx, c, agentName,
        headBefore+"..origin/"+c.Branch, deps)
    if err != nil { return reply(...), nil }
    return reply(ctx, cs.Emitter(), chatID, messageID, card), nil
}
```

### 3.3 与 F-56 的对应关系

| F-56 分支 | F-57 改写后的路径 |
|---|---|
| Branch 1 (no-op): clean + unpushed==0 | `snap.HasNothingToPush()` 返回 true |
| Branch 2 (agent commit + push): dirty | `!snap.WorkingTreeIsClean()` 走 runAgentToCommit |
| Branch 3 (push only): clean + unpushed>0 | `snap.HasNothingToPush()` false + `snap.WorkingTreeIsClean()` true → 直推 |

新增的 case:**clean + !HasUpstream**(首次 push)—— `HasNothingToPush` 因为 `!HasUpstreamBranch()` 返回 false,所以进 §3.2 第 5 步的 push;`programmaticPush` 内部走 `git push -u origin <branch>`(已有逻辑,不动)。

### 3.4 `countUnpushed` 的新定位

`internal/command/gtw/push.go::countUnpushed` **不删**,但缩小职责范围:

- **保留**:在 `programmaticPushWithRetry` 内部做"push 后二次验证"(git 可以 exit 0 但提交没真上去,见 F-56 §4.3)
- **移除**:不再被 `dispatchPush` 直接调用做"是否有未推送"判定

`countUnpushed` 的"no upstream configured = 0 unpushed"语义对 push 后验证是对的(此时上游已被 `-u` 设置过,不会有"无上游"路径);`dispatchPush` 入口不再有路径走到这个 fallback。

---

## 4. `/gtw pr` Gate

pr 的语义 = "我准备好了,请把这个本地分支在 Web 平台上开放成 PR"。

### 4.1 业务判定

```go
// IsReadyForPR: senior-dev hygiene check for opening a PR.
// /gtw push's successful exit guarantees this, modulo race.
func (s *GitStatusSnapshot) IsReadyForPR() bool {
    return s.HasUpstreamBranch() &&
        s.LocalIsAtUpstreamTip() &&
        s.WorkingTreeIsClean()
}

// PRBlockReason: IsReadyForPR 为 false 时,按优先级返回一条 actionable 提示
// (硬拒在前,清理 nudge 在后,保证只回一条)
func (s *GitStatusSnapshot) PRBlockReason() string {
    switch {
    case s.Branch == "":
        return "❌ detached HEAD — checkout a named branch first"
    case s.HasConflicts:
        return "❌ worktree has unmerged paths (merge/rebase conflict)\n" +
            "hint: resolve conflicts and `git add`, then /gtw pr"
    case !s.HasUpstream:
        // ← 本次撞到的 case:"branch has no upstream" 显式拦在 pr 入口
        return "❌ branch has no upstream on origin\n" +
            "hint: /gtw push first to publish the branch to origin, then /gtw pr"
    case s.AheadOfRemote > 0:
        return fmt.Sprintf(
            "⚠️ %d commit(s) made locally but not pushed\n"+
                "hint: /gtw push first, then /gtw pr", s.AheadOfRemote)
    case s.BehindRemote > 0:
        return fmt.Sprintf(
            "⚠️ origin/%s is %d commit(s) ahead of your local branch\n"+
                "hint: `git pull --rebase`, then /gtw pr", s.Branch, s.BehindRemote)
    case s.Uncommitted > 0:
        return fmt.Sprintf(
            "⚠️ %d file(s) changed but not committed\n"+
                "hint: /gtw push first to commit + push, then /gtw pr", s.Uncommitted)
    case s.Untracked > 0:
        return fmt.Sprintf(
            "⚠️ %d new file(s) not added to git\n"+
                "hint: git add them, then /gtw push, then /gtw pr", s.Untracked)
    }
    return ""
}
```

### 4.2 dispatchPR 入口收紧

```go
func dispatchPR(ctx, cs, deps, chatID, messageID, args) (*Result, error) {
    c, res := loadDispatchContext(ctx, cs, deps, chatID, messageID)
    if res != nil { return res, nil }

    baseBranch, err := DefaultBranch(ctx, c.RepoRoot, deps.Git)
    if err != nil { return reply(...❌...), nil }

    // (NEW) 单一 readiness gate
    snap, err := CollectReadiness(ctx, c.Worktree, deps.Git)
    if err != nil { return reply(...❌...), nil }
    if reason := snap.PRBlockReason(); reason != "" {
        return reply(ctx, cs.Emitter(), chatID, messageID, reason), nil
    }

    // (KEPT) PR-worthiness check —— 与 readiness 正交
    ahead, err := countBaseAhead(ctx, c.Worktree, baseBranch, deps)
    if err != nil { return reply(...❌...), nil }
    if ahead == 0 {
        return reply(ctx, cs.Emitter(), chatID, messageID,
            fmt.Sprintf(
                "✅ branch %s is in sync with %s — nothing new to PR yet\n"+
                    "hint: make some changes, then /gtw push, then /gtw pr.",
                c.Branch, baseBranch)), nil
    }

    // agent + provider.CreatePR 完全保留
    ...
}
```

### 4.3 取消的旧逻辑

- **删除** `countUnpushed` 调用 + "⚠️ N unpushed" 提示(`PRBlockReason` 已覆盖)
- **删除** 旧的内嵌 uncommitted / untracked 嵌套 if(`PRBlockReason` 已统一处理)
- **保留** `countBaseAhead` 和"nothing new to PR yet"分支(语义独立,不在 readiness 维度)

---

## 5. Continuity Proof

把"push 成功, pr readiness 必通过"写成不变式:

设 `snap` 是某工作区在时刻 T 的 readiness 快照,`ProgrammaticPush` 表示成功执行 `git push -u origin <branch>` 后的瞬间状态。

```
T0:   用户敲 /gtw push
T1:   snap_push = CollectReadiness(T0)
T2:   若 snap_push.PushBlockReason() != "" → 拒绝(返回)
T3:   若 snap_push.HasNothingToPush() → 提示(返回)
T4:   若 !snap_push.WorkingTreeIsClean() → runAgentToCommit → re-snapshot
      snap_push = CollectReadiness(T_commit)
T5:   ProgrammaticPush 成功
      此时:snap_push 必有:
        - HasUpstreamBranch() == true  (T5 -u 设了)
        - LocalIsAtUpstreamTip() == true  (T5 推到对齐)
        - WorkingTreeIsClean() == true   (T4 commit 把脏收掉了)
T6:   用户敲 /gtw pr
T7:   snap_pr = CollectReadiness(T6)
T8:   若 T5 → T7 之间没有第三方动 origin/<branch> 或本地工作区:
        snap_pr == snap_push
        即 snap_pr.IsReadyForPR() == true ✓
```

唯一会让 pr 在 push 成功之后仍 readiness 失败的:**外部并发**修改了 `origin/<branch>` 或本地工作区。这种 race 本身就需要用户介入 —— 不可消除,但语义上"pr 仍能兜住"。

### 5.1 形式化对比

| 维度 | `/gtw push` 成功后 | `/gtw pr` readiness 要求 | 一致 |
|---|---|---|---|
| `HasUpstreamBranch()` | true(`-u` 已设) | true | ✓ |
| `LocalIsAtUpstreamTip()` | true(`AheadOfRemote`=0) | true | ✓ |
| `WorkingTreeIsClean()` | true(commit 把脏收掉) | true | ✓ |

三个维度一一对应。这就是"为什么两个命令读同一份 snapshot"的结构性保证 —— **不存在 push 干净而 pr 仍 dirty 的快照漂移**。

---

## 6. 不变式 Checklist

- **Snapshot 单一来源**。`/gtw push` 和 `/gtw pr` 入口都从 `CollectReadiness` 取,不再有 `git status --porcelain` 单独调用或 `countUnpushed` 直调
- **`countUnpushed` 退出 dispatch 顶层**。仅留在 `programmaticPushWithRetry` 内部做 push 后二次验证
- **Push 容忍 `!HasUpstreamBranch`**,pr 不容忍 —— 这是两个命令**唯一**相反的判定,刻意为之
- **Push 容忍 `BehindRemote > 0`**(因为 push 是"我有新东西要送上去",remote 是否比我新不影响 push 本身);**pr 不容忍** —— PR 的 head tip 必须等于 origin/branch tip
- **agent 跑完强制 re-snapshot**。`dispatchPush` 第 4 步的 `CollectReadiness` 重新调用不可省 —— agent 可能引入冲突或遗留 dirty 状态
- **PRBlockReason 按优先级返回单条**。硬拒 → 清理 nudge,只一条 actionable 提示,避免 checklist 轰炸用户
- **冲突永远先报**。`HasConflicts` 在 push 的 `PushBlockReason` 和 pr 的 `PRBlockReason` 里都是首条
- **底层 git 调用次数不增**。snapshot 解析仍是 `CollectStatus` 一次 `git status`;`countBaseAhead` 在 pr 里仍是 `rev-list`(per-snapshot 之外的"是否有 PR 内容"判断)

---

## 7. 错误处理矩阵

### 7.1 `/gtw push`

| 触发条件 | IM 输出 | 后续动作 |
|---|---|---|
| `CollectReadiness` 调用失败 | `❌ read worktree status: <err>` | 退出 |
| `snap.HasConflicts` | `❌ worktree has unmerged paths...` | 退出(**不调 agent 不调 push**) |
| `snap.HasNothingToPush()` | `ℹ️ nothing to push` | 退出 |
| `!snap.WorkingTreeIsClean()` + 无 agent | `❌ no agent selected. /use <name> first` | 退出(不 push) |
| agent 跑完 HEAD 没动 | `⚠️ no new commit was created (HEAD unchanged at <sha>)` | 退出(不 push) |
| agent 跑完 worktree 仍 dirty | `⚠️ N file(s) still uncommitted: ...` | 退出(不 push) |
| agent 跑完 re-snapshot 出冲突 | `❌ worktree has unmerged paths...` | 退出(不 push,agent 引入的冲突要用户处理) |
| `programmaticPushWithRetry` 失败 | `❌ push failed: <err>` + 列出本地 commit | 退出(commit 在本地) |
| 全部成功 | `🤖 <agent> committed N change(s) and pushed...` 或 `✅ pushed N commit(s)...` | 退出 |

### 7.2 `/gtw pr`

| 触发条件 | IM 输出 | 后续动作 |
|---|---|---|
| `CollectReadiness` 调用失败 | `❌ read worktree status: <err>` | 退出 |
| `Branch == ""` | `❌ detached HEAD — checkout a named branch first` | 退出 |
| `HasConflicts` | `❌ worktree has unmerged paths...` | 退出 |
| **`!HasUpstream`** | **`❌ branch has no upstream on origin\nhint: /gtw push first to publish the branch to origin, then /gtw pr`** | **退出(本次撞到的 case)** |
| `AheadOfRemote > 0` | `⚠️ N commit(s) made locally but not pushed\nhint: /gtw push first, then /gtw pr` | 退出 |
| `BehindRemote > 0` | `⚠️ origin/<branch> is N commit(s) ahead of your local branch\nhint: git pull --rebase, then /gtw pr` | 退出 |
| `Uncommitted > 0` | `⚠️ N file(s) changed but not committed\nhint: /gtw push first to commit + push, then /gtw pr` | 退出 |
| `Untracked > 0` | `⚠️ N new file(s) not added to git\nhint: git add them, then /gtw push, then /gtw pr` | 退出 |
| `countBaseAhead == 0`(ready 但没东西可 PR) | `✅ branch X is in sync with base — nothing new to PR yet` | 退出 |
| 全部通过 | 进入 agent + `gh/glab pr create` | 创建 PR |

### 7.3 emoji 一致性

沿用 `internal/command/gtw/README.md` 规约:

- ❌ 错误 / 硬拒(冲突、detached HEAD)
- ⚠️ 用户操作不当但有明确路径(未推送、未 commit、behind)
- ℹ️ 信息性提示(nothing to push / nothing to PR)
- ✅ 正面确认(branch in sync with base)

---

## 8. 测试计划

### 8.1 共享 fixture

把 `pr_test.go::setupPRGit(rig, branch, unpushed, ahead)` 升级为 `setupReadiness(rig, snap *messages.GitStatusSnapshot)`,让 push 测试和 pr 测试用同一份 mock 数据。push 测试额外 mock `programmaticPush` 调用,pr 测试额外 mock `countBaseAhead`。

### 8.2 `/gtw push` 用例(`commit_push_test.go`)

| 测试 | 覆盖 case |
|---|---|
| `TestDispatchPush_HardRefuse_Conflicts` | `HasConflicts=true` → 拒,**不调 agent 不调 push** |
| `TestDispatchPush_NothingToPush` | Clean + `HasUpstream` + `Ahead=0` → `ℹ️ nothing to push`,不调 agent 不调 push |
| `TestDispatchPush_DirtyNoUpstream` | Uncommitted=2 + `!HasUpstream` → agent → commit → push with `-u` |
| `TestDispatchPush_DirtyWithUpstream` | Uncommitted=2 + `Ahead=3` → agent → commit → push |
| `TestDispatchPush_NoUpstreamFreshBranch` | Clean + `!HasUpstream` → push with `-u`,**不调 agent** |
| `TestDispatchPush_AgentProducedNothing` | 脏 → agent → re-snapshot 仍 `HasNothingToPush` → 兜底提示 |
| `TestDispatchPush_AgentIntroducedConflicts` | 脏 → agent → re-snapshot `HasConflicts=true` → 拒绝 |
| `TestDispatchPush_UpstreamSet_NoUnpushedLeft` | Clean + `HasUpstream` + `Ahead=0` 仍是 `HasNothingToPush=true`(回归:`!HasUpstreamBranch` 才会强制 push)|

### 8.3 `/gtw pr` 用例(`pr_test.go`)

| 测试 | 覆盖 case |
|---|---|
| `TestDispatchPR_DetachedHead` | `Branch=""` → 拒(不动 agent 不动 gh)|
| `TestDispatchPR_NoUpstream` | `!HasUpstream` → **本次撞到的 case**,IM 含 "branch has no upstream on origin" + "**/gtw push first to publish the branch**",**不动 gh** |
| `TestDispatchPR_AheadOfUpstream` | `Ahead=3` → 拒,IM 含 "**/gtw push first**",不动 gh |
| `TestDispatchPR_BehindUpstream` | `Behind=2` → 拒,IM 含 "git pull --rebase, then /gtw pr" |
| `TestDispatchPR_Uncommitted` | `Uncommitted=2` → 拒,IM 含 "**/gtw push first** to commit + push" |
| `TestDispatchPR_Untracked` | `Untracked=2` → 拒,IM 含 "git add them, then **/gtw push**" |
| `TestDispatchPR_HasConflicts` | `HasConflicts=true` → 拒,IM 含 "resolve conflicts" |
| `TestDispatchPR_ReadyAheadOfBase` | Ready + `main..HEAD=5` → 进 agent + `gh pr create` 被调 |
| `TestDispatchPR_ReadyNothingNew` | Ready + `main..HEAD=0` → 提示 "nothing new to PR yet",不动 gh |

### 8.4 连续性用例(关键 — `*_test.go` 跨文件)

```go
// TestPushThenPR_ContinuousFlow:
// 1) mock 初始 snap = dirty + !HasUpstream
// 2) dispatchPush,验证 programmaticPushWithRetry 被调且 args 含 "-u origin <branch>"
// 3) re-mock 入口 snap 为 "push 完成态":
//      HasUpstream=true, Ahead=0, Behind=0,
//      Uncommitted=0, Untracked=0, HasConflicts=false
// 4) dispatchPR,验证:
//      - PRBlockReason 返回 ""
//      - countBaseAhead 调用一次(main..HEAD=5)
//      - agent 跑一次
//      - gh pr create 被调一次
```

```go
// TestPushFailThenPR_Refuses:
// 1) mock 初始 snap = dirty + HasUpstream+Ahead=3
// 2) dispatchPush,agent 跑完,programmaticPush 失败(mock stderr)
// 3) 验证 dispatchPush 返回 "❌ push failed" + commit 列表
// 4) snap 此刻仍 Ahead>0(dirty 已 commit 但未 push)
// 5) dispatchPR,验证 PRBlockReason 返回 "⚠️ 3 commit(s) made locally but not pushed"
//    + 不调 agent + 不调 gh
```

### 8.5 关键不变式(单测断言)

```go
// push.go::buildAgentPrompt 不包含 "push" 命令字样(F-56 §3 不变量)
// —— F-57 不动 prompt,但保留回归测试
func TestBuildAgentPrompt_NoPushCommand(t *testing.T) { ... }

// 验证 dispatchPR 永远先看 snapshot,不再调 countUnpushed
func TestDispatchPR_NoCountUnpushed(t *testing.T) {
    rig := newPRTestRig(t)
    // mock CollectReadiness 用的 git status,但 mock 不覆盖 rev-list @{u}..HEAD
    // 如果 dispatchPR 走了 countUnpushed,这条 mock 会返回 "no mock found" 错误
    rig.git.onArgs([]string{"status", "--porcelain", "--branch", "--untracked-files=normal"},
        "## branch...origin/branch\n", "", nil)
    ...
    // 期望:即使 unreadiness 被 mock,也不会因 countUnpushed mock miss 而报错
}

// 验证 dispatchPush 不再调用 countUnpushed 做主判定
func TestDispatchPush_NoCountUnpushedAtEntry(t *testing.T) {
    // mock 不覆盖 rev-list @{u}..HEAD,验证 dispatchPush 仍能基于 snapshot 走 Branch 2/3
}
```

---

## 9. 迁移步骤

按依赖顺序,每一步独立 commit + 测试通过:

1. **`internal/messages/footer.go`**:加 `BehindRemote`、`HasConflicts` 字段。snapshot consumer(footer 渲染)忽略新字段,无副作用。
2. **`internal/command/gtw/git_status.go`**:
   - `parseBranchHeader` 补 `behind M` 解析(去掉"intentionally not surfaced"那段注释)
   - `parsePorcelainBranchStatus` 在扫剩余行时计算 `HasConflicts`
   - `CollectStatus` 改名为 `CollectReadiness`(保留旧名 alias 或保留原函数也行,推荐直接改名 + 更新所有调用方——只有 footer 渲染层在用,grep `CollectStatus` 即可定位)
   - 加 `HasUpstreamBranch()` / `LocalIsAtUpstreamTip()` / `WorkingTreeIsClean()` / `HasNothingToPush()` / `PushBlockReason()` / `PRBlockReason()` / `IsReadyForPR()` 方法
3. **`internal/command/gtw/pr.go::dispatchPR`**:替换 `countUnpushed` + 内嵌 uncommitted/untracked 判断 → `CollectReadiness` + `PRBlockReason`。`countBaseAhead` 和 "nothing new to PR yet" 分支保留。
4. **`internal/command/gtw/commit_push.go::dispatchPush`**:把 `isClean` / `unpushedBefore` 两个变量换成 `snap` + 三个谓词;`countUnpushed` 入口直调移除(`programmaticPushWithRetry` 内部保留)。agent 跑完强制 re-snapshot。
5. **`internal/command/gtw/testharness_test.go`(或新文件)**:加 `setupReadiness(rig, snap)` 共享 fixture,统一 mock 入口。
6. **`internal/command/gtw/pr_test.go` + `commit_push_test.go`**:按 §8.2 / §8.3 / §8.4 列表补全测试。**不机械移植测试,重写时按 readiness 维度重新组织用例。
7. **`CHANGELOG.md`** [Unreleased] 加一条:"F-57: `/gtw push` + `/gtw pr` 统一使用 `Readiness` snapshot 做前置检测。本地分支未设 upstream / 未推送 / 与 origin 不一致 / 工作区不干净都会被显式拦截,IM 文案直接指向 `/gtw push`。"
8. **删除 `pr_test.go::TestDispatchPR_UnpushedCommits` 等基于 `countUnpushed` 数字 mock 的旧测试** —— 它们测的是未实现的代码路径。

---

## 10. 边界 / 未决问题

### 10.1 footer 渲染层是否消费新字段

当前 footer(`internal/command/gtw/render.go`)只展示 `Branch` / `Uncommitted` / `Untracked` / `AheadOfRemote`。本设计**不动** footer 渲染,只是把字段加进 struct(为后续 footer 增强留接口)。如果后续要 footer 显示 `behind N` 或 conflicts 警告,需要单独的 F-XX 提案 —— 本次只做 readiness 数据结构 + 入口判定。

### 10.2 race 兜底

`/gtw push` 成功 → `/gtw pr` 期间,第三方修改 `origin/<branch>`(其他 worktree 抢推 / GitHub 上直接 merge / admin 强推)。`CollectReadiness` 的 `BehindRemote > 0` 会捕获这种情况,提示 "git pull --rebase"。本设计**不**主动重试或自动 rebase,留给用户决策。

### 10.3 `programmaticPushWithRetry` 内的 `countUnpushed` 语义保留

`push.go:122-141` 的"no upstream configured → 0 unpushed"语义**继续有效**在 `programmaticPushWithRetry` 的 verify loop 内:此时 `-u` 已经设过了,理论上不会走到"无上游"分支;但若真走到(极端 race),返回 0 是合理的"push 没产生新东西"信号,不是错误。这部分语义与 `dispatchPush` 入口的 readiness 判定**无耦合**,互不污染。

### 10.4 `ModeLocal`(没有 yml 的 worktree)

`loadDispatchContext` 在无 yml 时调 `rev-parse --show-toplevel` + `rev-parse --abbrev-ref HEAD` 推导 `Branch` / `Worktree` / `RepoRoot`。这部分**不动** —— readiness gate 在入口之后,无论是否 ModeLocal 行为一致(只是少 `Repo` / `Provider` 信息,gh 调用时再 derive)。

### 10.5 "first-time push" UX

`HasUpstreamBranch()==false` 在 push 是合法路径(pr 是非法)。但 IM 上用户能否一眼看出"这是要首次 push"?当前 `programmaticPush` 内部调 `git push -u`,不 surface "first push" 这个概念。如果想区分"首次 push"与"已有 upstream 但 ahead",需要在 push 的 IM 卡片里加一行(`🌱 first push to origin/<branch>`),那是后续 UX 增强,不在本次 F-57 范围。

---

## 11. References

- [`本文件 §push 三分支`](./F-gtw.md):push 三分支的完整设计 —— 本 F-57 是其 readiness 维度上的扩展
- [`docs/feat/F-50-git-provider.md`](F-50-git-provider.md):GitHub/GitLab provider 抽象,gh/glab 调用的下游
- [`feishu-rendering.md §Footer`](../channel/feishu-rendering.md):footer 渲染层消费 `GitStatusSnapshot`,本次扩展字段不影响渲染
- [`internal/command/gtw/README.md`](../../internal/command/gtw/README.md):IM 卡片排版规约(emoji 一致性等)
- [`wip/gtw-hooks.md`](../wip/gtw-hooks.md):agent 优先级 + chain(本设计不动)

---


---

## A3. F-59: `/gtw fix` 自动 bootstrap `nightme/*` 标签

> **Source**: [`F-59-gtw-label-bootstrap.md`](./F-59-gtw-label-bootstrap.md)
>
> **Issue**: #235（`/gtw push` readiness bug —— 触发本次重构的载体
> case：第一次在 `cnlangzi/nightme` 跑 `/gtw fix 235` 撞到了
> `'nightme/wip' not found`）
>
> **改动摘要**：`GitProvider` 接口加 `CreateLabel` 方法；
> `runFixRemote` §5.2 在 `WorktreeAdd` 之后、`AddIssueLabel` 之前
> 串行 bootstrap 6 个 `nightme/*` 标签；AddIssueLabel / CreateLabel
> 失败的回滚逻辑抽成 `rollbackLabelStep` helper 集中维护。

详细设计、错误处理矩阵、决策记录、不在范围内的 followups
见 [`F-59-gtw-label-bootstrap.md`](./F-59-gtw-label-bootstrap.md)。

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
| 1. `origin/<branch>` ref 存在 | `RemoteBranchExists(ctx, dir, branch,)` → `git ls-remote --heads origin <branch>` 直接调 | `❌ origin/<branch> does not exist — /gtw push first` |
| 2. 没有已开 PR | `provider.FindOpenPRForBranch(owner, repo, head)` → `gh pr list --head ...` / `glab mr list --source-branch ...` | `❌ branch <branch> already has an open PR (#N): <url>` |

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
- `internal/command/gtw/provider.go::isExecutableNotFound` / `isStaleUpstreamGH` / `isStaleUpstreamGL`

### A4.3 A2 设计的哪些部分保留 / 哪些被替换

| A2 设计元素 | 状态 |
|---|---|
| `CollectReadinessForDispatch` / `verifyUpstreamOnOrigin`(A2 §A2.F-237)| **保留**给 `/gtw push` 和 `/gtw commit` 用 — 它们的 stale-cached-ref 探测仍是真 bug;**移除** PR 路径的调用 |
| `GitStatusSnapshot` 字段 / `parsePorcelainBranchStatus` / `parseBranchHeader` | **保留** — runtime footer 还在用 |
| `WorkingTreeIsClean` / `HasNothingToPush` / `PushBlockReason` / `HasUpstreamBranch` / `LocalIsAtUpstreamTip` / `UpstreamGone` 字段 | **保留** — push 路径的 `HasNothingToPush` 仍消费 `HasUpstreamBranch` + `UpstreamGone` |
| `PRBlockReason` / `IsReadyForPR` | **删除** — 0 caller |
| `dispatchPR` 的 6 维 `if reason := snap.PRBlockReason()` 分支 | **删除** |
| `countBaseAhead` + "nothing new to PR yet" 分支 | **删除** — ahead=0 也发,gh/glab 自己说 |
| `mapGhHeadMissingToHint` 字符串翻译 | **删除** — 替换为 sentinel-based flow + `provider.isStaleUpstream*` helper |

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

`internal/command/gtw/pr_test.go` 共 ~30 个测试,涵盖:
- 两道闸的 pass / fail 各路径
- 已知 stderr 翻译(auth / network / not-a-repo)
- 未知 stderr passthrough(关键不变量 — 不能匹配任何 known fragment)
- race-window 兜底(branch 被删 / PR 被人开了 / gh/glab 突然没装)
- dirty / ahead / behind / conflicts 不再 gate
- helper 单元测试(`wrapCreatePRError` / `wrapListPRError` /
  `isExecutableNotFound` / `isStaleUpstream*`)
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
