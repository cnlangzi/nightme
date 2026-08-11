# F-56: `/gtw push` 三分支流 + Agent 最小化 Prompt

> **Status**: 📝 设计阶段（doc-first；2026-08-11）
> **Milestone**: v1.3.x
> **Depends on**: F-104 (`/gtw push` 3 档 agent 优先级 + yml `~/.nightme/gtw.yml`)、F-120 (PR #120 `fix(gtw): harden push verification and PR cache correctness` — 本次新增的 HEAD-advance check)、`internal/command/gtw/push.go::headSHA`、`internal/command/gtw/push.go::countUnpushed`、`internal/command/gtw/push.go::listUncommittedFiles`
> **Replaces**: 当前 `internal/command/gtw/commit_push.go::dispatchPush` 的 2 分支设计（`pushClean` / `pushDirty`）+ `verifyAgentPushedAndRecover` 的"agent 自己 push 完 verifier 兜底"模型
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

- 旧设计:agent claim 3 条 SHA → 必须 parse 核对
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
> docs-readme
```

emoji `🤖`(agent 主导)。commit 列表来自 `git log headBefore..origin/<branch>`,不是 agent prose。

### 5.3 Branch 3 — push only

```
✅ pushed 3 commit(s) to docs-readme:
  94c4a38 docs: add chat input routing and shell mode documentation to README
  77a8023 fix(gtw): harden push verification and PR cache correctness
  459b968 docs(README): clarify session management commands, drop stale gtw hook
> docs-readme
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
// pushDryBug: 旧 verifyAgentPushedAndRecover 在 agent 没真 commit 时
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
| Agent 不 push | 网络副作用是 LLM 弱项;push 决策 / retry 是 infra 责任 | 让 agent push + nightme verify(被弃,旧设计) |
| Agent 不 claim SHA | git 是 source of truth,不需要 LLM 二次汇报 | 解析 agent reply 校验(被弃,F-56 早期版本) |
| `verifyAgentCommitted` 只看 git 状态 | 跟 nightme 其它 verification 一致,不读 agent prose | 解析 agent reply(被弃) |
| `programmaticPushWithRetry` 抽出来 | Branch 2 续 push 跟 Branch 3 直推共用 | 复制 pushClean 逻辑(被弃) |
| Branch 1 新增 no-op IM | 当前 0 commit push 错误地返回 `✅ pushed 0 commits`,误导用户 | 静默 no-op(被弃,UX 差) |
| `ℹ️` 加入 gtw emoji 字典 | Branch 1 需要"信息但非错误"标志 | 用 `✨`(含义是"无副作用的现状",勉强但不准) |
| 删 `pushClean` 函数 | 被 Branch 3 收编,函数重命名 + 复用更清楚 | 保留 pushClean 给 Branch 3 用(被弃,代码重复) |

---

## 11. 实施清单

| Phase | 内容 | 涉及文件 |
|---|---|---|
| **Phase 0** | 本 F-56 文档 + `wip/gtw-push.md` 同步(可选) | `docs/feat/F-56-*.md`(本文件) |
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
| 新 prompt 130 tokens 比旧版短,某些 edge case 缺少指令 | `commit_push_test.go` 8 个 branch case 覆盖;manual smoke 后再 merge |
| `programmaticPushWithRetry` 抽出来 pushClean 删了,旧测试可能 break | Phase 7 重写测试,旧 pushClean 引用全清 |
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
