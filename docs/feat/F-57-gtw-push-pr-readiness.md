# F-57: `/gtw push` + `/gtw pr` 联动 Readiness Gate

> **Status**: 📝 设计阶段（doc-first；2026-08-11）
> **Milestone**: v1.3.x
> **Depends on**: F-56 (`/gtw push` 三分支流)、F-50 (GitHub/GitLab Provider)、F-45 (Session footer) — 后者已经在 SessionContext 里跑过 `CollectStatus`,本设计只是扩字段+补谓词
> **Replaces**: `internal/command/gtw/pr.go::dispatchPR` 当前的两道独立 `countUnpushed` + `countBaseAhead` 预检;以及 `internal/command/gtw/commit_push.go::dispatchPush` 里 `isClean` + `countUnpushed` 双变量驱动的三分支判定
> **Related**: [`docs/feat/F-56-gtw-push-three-branch.md`](F-56-gtw-push-three-branch.md)（push 三分支的细节）、[`internal/command/gtw/git_status.go`](../../internal/command/gtw/git_status.go)（snapshot 解析层）、[`internal/messages/footer.go`](../../internal/messages/footer.go)（`GitStatusSnapshot` 类型定义）

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

| 状态维度 | 实际值 | 旧 `dispatchPR` 表现 |
|---|---|---|
| `refactor-outbound` 本地 HEAD | `fe6ed8d`(本地有 17 个 commit) | n/a |
| `origin/refactor-outbound` | **不存在**(`git ls-remote` 找不到) | n/a |
| `main..HEAD`(本地) | 17 commit | `countBaseAhead` 返回 17,预检通过 |
| `refactor-outbound@{u}..HEAD` | fatal: no upstream | `countUnpushed` 返回 `(0, nil)`(把"无上游"误读为"零未推送") |
| 旧 dispatchPR 的结论 | — | "通过,继续调 gh" |
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

> "我要 PR 了。`/gtw push` 应该把所有本地应该 commit 的东西 commit + push 上去,然后 `/gtw pr` 就是收尾。中间不该有'我到底有没有 push 过?'的二次确认。"

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

- **删除** 旧 `countUnpushed` 调用 + "⚠️ N unpushed" 提示(`PRBlockReason` 已覆盖)
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
6. **`internal/command/gtw/pr_test.go` + `commit_push_test.go`**:按 §8.2 / §8.3 / §8.4 列表补全测试。**不机械移植**旧测试,重写时按 readiness 维度重新组织用例。
7. **`CHANGELOG.md`** [Unreleased] 加一条:"F-57: `/gtw push` + `/gtw pr` 统一使用 `Readiness` snapshot 做前置检测。本地分支未设 upstream / 未推送 / 与 origin 不一致 / 工作区不干净都会被显式拦截,IM 文案直接指向 `/gtw push`。"
8. **删除 `pr_test.go::TestDispatchPR_UnpushedCommits` 等基于 `countUnpushed` 数字 mock 的旧测试** —— 它们测的是已废弃的代码路径。

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

- [`docs/feat/F-56-gtw-push-three-branch.md`](F-56-gtw-push-three-branch.md):push 三分支的完整设计 —— 本 F-57 是其 readiness 维度上的扩展
- [`docs/feat/F-50-git-provider.md`](F-50-git-provider.md):GitHub/GitLab provider 抽象,gh/glab 调用的下游
- [`docs/feat/F-45-session-footer.md`](F-45-session-footer.md):footer 渲染层消费 `GitStatusSnapshot`,本次扩展字段不影响渲染
- [`internal/command/gtw/README.md`](../../internal/command/gtw/README.md):IM 卡片排版规约(emoji 一致性等)
- [`wip/gtw-hooks.md`](../wip/gtw-hooks.md):agent 优先级 + chain(本设计不动)