# F-59: `/gtw` 检查并补齐仓库的 `nightme/*` label catalog

> **Source**: `F-59-gtw-label-bootstrap.md`

> **Depends on**: F-45 (`/gtw fix` 状态机)、F-50 (GitHub/GitLab
> Provider)、F-51 (gtw 包迁到 `internal/command/gtw/`)

> **Related**: [`F-gtw-fix.md`](./F-gtw-fix.md)（`/gtw fix` 主流程与 agent dispatch）、
> [`F-gtw.md`](./F-gtw.md)（gtw 命令索引）、[`internal/command/gtw/types.go`](../../internal/command/gtw/types.go)
> （`AllLabels` / `LabelMeta` / `LabelMetaFor`）、[`internal/command/gtw/provider.go`](../../internal/command/gtw/provider.go)
> （`GitProvider.ListLabels` / `CreateLabel`）、[`internal/command/gtw/labels.go`](../../internal/command/gtw/labels.go)
> （`ensureRepoLabels` + `LabelCache`）

> **Issue**: #235 —— 触发这套机制的载体 case：第一次在
> `cnlangzi/nightme` 跑 `/gtw fix 235` 直接被 `'nightme/wip' not
> found` 拦下来，整个 fix flow 回滚 worktree + branch，根因不是
> #235 的 push 逻辑本身，是 gtw 状态机对外部状态的隐含假设。

---

## 0. TL;DR

gtw 的状态机住在 tracker 上的 `nightme/*` 标签里。一个仓库的
label catalog 里没有某个标签，那个标签就**挂不到任何 issue 上**
（平台报 "not found"，不会顺手建出来）。所以每一条跟 tracker
打交道的 gtw 命令都会先确认仓库的 catalog 里有 `AllLabels` 的
全部条目，把缺的建出来：

1. **接口层**：`GitProvider` 加 `ListLabels(ctx, owner, repo)
   ([]string, error)`（读仓库的 label catalog）和 `CreateLabel(ctx,
   owner, repo, name, color, description) error`（建一个标签）。
   `GitHubProvider` 走 `gh api repos/<owner>/<repo>/labels`，
   `GitLabProvider` 走 `glab api projects/<owner%2Frepo>/labels`；
   `CreateLabel` 两个平台都不传 `--force`。
2. **检查**：`ensureRepoLabels(ctx, deps, provider, owner, repo)
   (labelCheck, error)` —— 读 catalog，只把 `AllLabels` 里缺的那些
   建出来，返回建出来的标签名（以及这次是不是真的读了 catalog）。
3. **缓存**：`LabelCache` 按仓库记住「这个仓库的 catalog 齐全
   了」。只进内存，daemon 重启后重新检查；失败的检查不写缓存。
4. **跑在哪**：`/gtw fix` 在**队列挑选之前**跑，失败即致命（此时
   还没有 worktree，没有东西要回滚）；`/gtw pr` / `/gtw new -y` 跑在
   各自的 tracker 路径上，失败只给一行 `⚠️` 警告，命令照常跑完。
   `/gtw close` 跑检查 —— 它要往 issue 上贴 `nightme/done`，失败只给
   一行 `⚠️` 警告，命令照常跑完。provider 探测不跑检查。

设计上保证：任何一条会跟 tracker 说话的 gtw 命令，都不会因为
仓库少一个标签而卡住；队列为空的回复会说明队列为什么是空的。

---

## 1. Motivation

### 1.1 触发 case：`/gtw fix 235` 在 `cnlangzi/nightme` 第一次跑就翻车

实际跑出来的错误：

```
❌ Could not add label "nightme/wip" to issue #235: gh issue edit
   --add-label: exit status 1: failed to update
   https://github.com/cnlangzi/nightme/issues/235: 'nightme/wip' not
   found
worktree and branch have been rolled back.
fix the cause and re-run /gtw fix 235
```

用户体感：

- 已经拿到 fix 进度卡 → worktree 已经创建 + branch 已经创建 →
  切 cwd 已经完成 → `.gitignore` 已经提交 → agent 几乎要被
  dispatch → 在最后一步（"在 issue 上贴个标签表示 claimed"）整个
  flow 回滚。

回滚后的 state：worktree 没了，branch 没了，但用户已经为这次
fix 投入了 ~10s 的等待 + 一定的心理预期。

### 1.2 根因：gtw 状态机对外部世界有一个隐含但未文档化的前提

`internal/command/gtw/types.go` 硬编码 7 个 `nightme/*` 标签作为
gtw 状态机的载体：

```go
const (
    LabelAssigned  = "nightme/assigned"
    LabelWIP       = "nightme/wip"
    LabelReady     = "nightme/ready"
    LabelReviewing = "nightme/reviewing"
    LabelRevise    = "nightme/revise"
    LabelDone      = "nightme/done"
    LabelStuck     = "nightme/stuck"
)
```

`Provider.AddIssueLabel` 内部直接调 `gh issue edit <id> --add-label
<label> --repo ...`。`gh issue edit` 在 label 不存在时 **不自动
create，直接 exit 1 + stderr `'nightme/wip' not found`**。这是 gh
的设计选择，与 `gh label create` 的行为相反。

**根因**：gtw 假设目标仓库上"已经有 nightme/* 标签"，但这个假设
从来没被强制建立过。任何第一次跑 `/gtw fix` 的 repo 都会踩到。

### 1.3 为什么不让用户手动跑 `gh label create`

手动方案（让用户或运维先把标签建好）技术上能解决问题，
但有 3 个不好的后果：

1. **每次 fork 都要重做**：nightme 用户的 repo 拓扑是异构的
   （`cnlangzi/nightme` 自己、私有的 fork、别人 fork 后的
   项目、CI 临时仓库），每个新目标 repo 都要先建好。
2. **出错面散落**：这一步散落在文档/wiki/口口相传里，
   跟 gtw 自己的代码脱钩。新人 / 重新接手的人不知道要做，
   撞到之后不知道是 gtw 的问题还是 gh 的问题。
3. **状态机退化**：gtw 把"label 已存在"当作前提，本质上是把
   状态机的一个分量放在了外部系统 + 用户手动的协调上。这违反
   "self-consistent state machine" 原则 —— 状态机的合法初始
   state 必须能从自己内部到达。

把这一步内置进 gtw，让 gtw 状态机在任意目标 repo 上都能自举到
合法初始 state，是这条原则的直接应用。

### 1.4 更深的一层：catalog 里少一个标签，队列就永远是空的

`nightme/assigned` 是队列标记。`/gtw fix *` 从「开放 issue 里带
`nightme/assigned` 的那些」里挑下一个。而 catalog 里没有这个
标签的仓库**挂不上**它，于是：

- 用户贴不上 `nightme/assigned`（标签不存在）；
- gtw 也建不出这个标签（建标签的步骤在队列挑选**之后**，而队列
  为空时流程在那之前就返回了）；
- 队列永远空，`/gtw fix *` 永远报 "No eligible assigned issue"。

这是一个闭环死锁：唯一能修复它的路径（`/gtw fix <issue-id>`）
恰恰要求用户已经知道 issue id。所以检查必须跑在队列挑选
**之前**。

---

## 2. 设计

### 2.1 `GitProvider` 接口契约

```go
type GitProvider interface {
    // ListLabels returns the names of the labels the repository's
    // label catalog holds.
    ListLabels(ctx context.Context, owner, repo string) ([]string, error)

    // CreateLabel creates `label` in the repository's label catalog.
    CreateLabel(ctx context.Context, owner, repo, name,
                color, description string) error
}
```

**`ListLabels` 的实现**：

| 平台 | 命令 | 解析 |
|---|---|---|
| GitHub | `gh api "repos/<owner>/<repo>/labels?per_page=100"` | JSON 数组，取每项的 `name` |
| GitLab | `glab api "projects/<owner%2Frepo>/labels?per_page=100"` | JSON 数组，取每项的 `name` |

一页最多 100 个标签。catalog 比这更大的仓库可能把一个存在的
标签报成"缺失" —— 调用方会去建它，而 `CreateLabel` 把"已经存在"
当成功，所以 catalog 最后还是对的。

**`CreateLabel` 的幂等性是硬要求**：每次调用的语义都必须是"label
存在就 no-op，不存在就创建"。两个实现各自的实现策略：

| 平台 | 命令 | 幂等机制 |
|---|---|---|
| GitHub | `gh label create <name> --color <color> --description <description> --repo <owner>/<repo>` + stderr 嗅探 | exit 0 → 创建成功；exit 1 + stderr 含 `already exists` → 已存在视为 no-op；其他 exit 1 → 错误透传 |
| GitLab | `glab api --method POST projects/<owner%2Frepo>/labels -f name=... -f color=... -f description=...` + stderr 嗅探 | 同 GitHub（GitLab 的 409 报文是 `{"message":"Label already exists"}`）|

**为什么不传 `--force`**：`gh label create --force` 在 "label 已
存在" 分支会**覆盖** color / description —— 正是 `CreateLabel`
"no-op when existing" 契约要避免的行为。运维手工调过的 label 颜色
在每次 `/gtw fix` 时被静默改回，是比"标签重复创建"更难查的
问题。

**失败语义**：任何非"已存在"的 exit ≠ 0 都是硬失败，stderr
原样返回。

### 2.2 `LabelMeta` 集中表

`internal/command/gtw/types.go`：

```go
type LabelMeta struct {
    Color       string // 6-char hex WITH a leading '#'
    Description string
}

var labelMeta = map[string]LabelMeta{
    LabelAssigned:  {Color: "#c5def5", Description: "Assigned to NightMe — queued for /gtw fix *"},
    LabelWIP:       {Color: "#fbca04", Description: "Work in progress (claimed by /gtw fix)"},
    LabelReady:     {Color: "#0e8a16", Description: "Ready for review"},
    LabelReviewing: {Color: "#1d76db", Description: "Under review"},
    LabelRevise:    {Color: "#e4e669", Description: "Needs revision"},
    LabelDone:      {Color: "#5319e7", Description: "Completed"},
    LabelStuck:     {Color: "#b60205", Description: "Stuck / blocked"},
}

func LabelMetaFor(name string) LabelMeta { /* fallback "#ededed" + "" */ }
```

颜色从 GitHub 默认 label palette 选了一组可区分的 hue，描述
是英文短句（gtw 命令本身是英文）。

**为什么单独建表而不是 inline 在 `CreateLabel` 调用处**：

- `AllLabels` 是 source of truth（display order），`labelMeta`
  是它的 companion。两份数据绑死在一处，未来加新状态标签
  只需要改两个地方，diff 距离短，容易 review。
- `LabelMetaFor` 是单一查询入口，调用方不用关心 `name` 是否
  在 map 里（fallback 给一个 neutral grey）。

### 2.3 检查：`ensureRepoLabels`

```go
// ensureRepoLabels verifies that every label in AllLabels exists in
// the repository's label catalog and creates the ones that are
// missing.
//
// It returns what it found, so the caller can tell the user that the
// repository was repaired. An error means the catalog could not be
// verified — the caller decides whether that is fatal.
//
// A verified repository is answered from the cache without any
// remote call. A failed check records nothing, so the next command
// checks again.
//
// The comparison against the catalog is exact: a label name is
// matched as the platform reports it, without trimming or case
// folding.
func ensureRepoLabels(
    ctx context.Context,
    deps HandlerDeps,
    provider GitProvider,
    owner, repo string,
) (labelCheck, error)
```

返回值是 `labelCheck`（`internal/command/gtw/labels.go`）：

```go
type labelCheck struct {
    // Created lists the labels the check created.
    Created []string
    // Verified is true when the check read the catalog during this
    // call. It is false when the answer came from the cache — the
    // catalog was verified earlier in the session, and it may have
    // changed since.
    Verified bool
}
```

流程：

1. `deps.LabelCache` 里有这个仓库（平台 + `owner/repo`）这一条 →
   直接返回，**一次远程调用都不发**。
2. 否则 `provider.ListLabels` 读 catalog。
3. 算出 `AllLabels` 里 catalog 没有的条目（精确比对，标签名按平台
   报的样子比，不 trim、不折叠大小写）。
4. 缺的条目逐个 `provider.CreateLabel`，颜色 / 描述取自
   `LabelMetaFor(name)`。
5. 全部成功 → 把这个仓库记成"已验证"。

读失败或建失败 → 返回错误，**什么都不记**（下条命令会重试）。

### 2.4 缓存：`LabelCache`

```go
// LabelCache memoises, per repository, that every label in AllLabels
// exists in that repository's label catalog. Nothing is written to disk,
// so a restart re-checks.
type LabelCache struct {
    mu    sync.RWMutex
    repos map[string]struct{}
}
```

- **key 是 `kind + "/" + owner + "/" + repo`**。label catalog 是仓库
  的属性，不是 worktree 或 branch 的属性，所以同一仓库的每个
  worktree 共用一条记录。平台也在 key 里：GitHub 仓库和 GitLab
  项目可以同名同 owner，而两个平台的 catalog 是两套 —— 共用一条
  记录会让后一个平台的标签永远建不出来。host 不在 key 里：
  yml 缓存的 provider 路径不解析 host，同一个仓库会因为解析方式
  不同拿到两条记录。
- **条目永不失效**：仓库一旦验证过，这个结论就成立到 daemon
  重启为止。代价是可见的 —— 人在会话中间删掉一个标签，下一次
  `/gtw fix` 贴 label 时会失败，而那条回复会解释这一点（见 §5）。
- **nil 是合法的惰性缓存**：每次 lookup miss，每次 store 丢弃 ——
  这就是没有接缓存的 `HandlerDeps` 在检测结果被 memo 化之前的
  行为。

### 2.5 检查跑在哪

| 命令 | 什么时候 | 失败时 |
|---|---|---|
| `/gtw fix` | remote 路径，owner/repo 解析之后、**队列挑选之前** | 致命 —— 命令回显错误就停，此时还没有 worktree |
| `/gtw pr` | provider 解析之后、开 PR 之前 | 回复里给一行 `⚠️` 警告，命令继续 |
| `/gtw new -y` | 建 issue 的路径（provider 解析之后） | 回复里给一行 `⚠️` 警告，命令继续 |
| `/gtw close` | provider 解析之后、动 issue 标签之前 | 回复里给一行 `⚠️` 警告，命令继续 |

`/gtw close` 一样检查 —— 它要往 issue 上贴 `nightme/done`，贴不上时那一步
自己会报错，所以检查失败只给一行 `⚠️` 警告，close 照常跑完。

`/gtw new` 不带 `-y` 只把讨论变成草稿，不写 tracker，所以不检查。

**警告行出现在每一条回复的最前面** —— 无论命令是成功还是失败。

**检查不在 provider 探测里跑**：`detectProvider` /
`resolveProvider` 每次消息刷新都会被 workspace footer 的 PR 查询
走到，在那里检查会把一次读变成对用户仓库的写。

### 2.6 `/gtw fix` 的流程

```
resolve provider + owner/repo
  → ensureRepoLabels ── 失败 → ❌ Could not ensure gtw labels on <owner>/<repo>: <err>
       │                              （什么都没建，不需要回滚）
       ↓ 成功
  → 挑队列里最老的合格 issue
       ├─ 没有候选 → ❌ No eligible assigned issue + 队列为什么是空的
       └─ 挑到了
            → worktree + branch
            → AddIssueLabel(nightme/wip)   失败 → rollbackLabelStep
```

认领之后 issue 同时带 `nightme/assigned` 和 `nightme/wip` 两个
label —— 队列标记 gtw 从不摘。claim 本身只有在 `/gtw pr --ready`
把 issue 挪到 `nightme/ready` 时才摘。

label catalog 检查在 worktree 之前，所以它的失败不需要回滚 ——
`rollbackLabelStep` 只覆盖 `AddIssueLabel` 这一步。

### 2.7 回复文案

- **检查失败（`/gtw fix`）**：`❌ Could not ensure gtw labels on
  <owner>/<repo>: <err>`，`<err>` 是平台自己的错误文本，一字不改。
- **队列为空**：卡片保留 `❌ No eligible assigned issue` 抬头和队列
  那一行，并补一行说明队列为什么是空的 —— 标签刚被建出来（或者
  本来就在 catalog 里、或者这次是命中缓存、catalog 早先验证过），
  队列要等人工把它贴到某个 issue 上才有内容。这一行**只在窗口里
  没有任何 issue 带队列标记时出现**：队列里有候选但全被跳过时，
  卡片上写的是它们各自被跳过的原因，"贴标签"解决不了那些原因；
  队列里的 issue 全被占用（带着别的 `nightme/*` 状态 label）时，
  标签是在用的，卡片说的是「N 个被占用的 issue 不参与挑选」。
- **贴 label 失败**：`❌ Could not add label <name> to issue #<id>:
  <err>` 后面补一行，说明 catalog 可能是验证之后被人改过，办法是
  重启 daemon 重新验证，或者手工建这个标签。这一行是一个**可能**
  而不是诊断 —— 平台自己的错误文本说的是实际失败的原因。
- **检查失败（`/gtw pr` / `/gtw new -y`）**：
  `⚠️ could not ensure gtw labels on <owner>/<repo>: <err>`，
  放在回复正文的最前面。

---

## 3. 实施

### 3.1 `internal/command/gtw/provider.go`

- `GitProvider` 接口新增 `ListLabels` 与 `CreateLabel`。
- `GitHubProvider.ListLabels`：`gh api "repos/<owner>/<repo>/labels?per_page=100"`，
  解 JSON 数组取 `name`。
- `GitLabProvider.ListLabels`：`glab api "projects/<owner%2Frepo>/labels?per_page=100"`，
  解 JSON 数组取 `name`。
- 两个 `CreateLabel` 都**不**传 `--force`，exit ≠ 0 + stderr 含
  `already exists` 视作成功；其他错误原样回传。

### 3.2 `internal/command/gtw/labels.go`（新文件）

- `LabelCache`：`sync.RWMutex` + `map[string]struct{}`，`lookup` /
  `store` 两个 helper，nil receiver 惰性。
- `ensureRepoLabels`：读 catalog → 算缺哪些 → 建 → 记缓存。
- `labelCatalogNote` / `labelCheckWarning` / `staleCatalogNote`：
  三段回复文案的拼装。

### 3.3 `internal/command/gtw/fix.go`

- `HandlerDeps` 加 `LabelCache *LabelCache` 字段。
- `runFixRemote`：owner/repo 解析之后加检查，失败致命；删除
  `WorktreeAdd` 之后的 bootstrap 步骤和它的回滚分支。
- 贴 label 失败的回复加上 stale-catalog 那一行。

### 3.4 `internal/command/gtw/pr.go` / `new.go`

- 各自在 provider 解析之后跑检查，失败只产生一行 `⚠️` 警告，
  塞进回复正文的最前面。

### 3.5 `internal/runtime/runtime.go`

`gtw.HandlerDeps` 里接上 `LabelCache: &gtw.LabelCache{}` —— 跟
`ProviderCache` 一样由 runtime 拥有，进程活多久就在多久。

---

## 4. 不变式 Checklist

- [x] GitHub / GitLab `CreateLabel` 都不传 `--force`，避免覆盖运维
      手工调过的 label 颜色 / 描述。已存在 label 一律 no-op。
- [x] 检查只建 catalog 里缺的标签，不建全部 `AllLabels`。测试
      `TestEnsureRepoLabels_CreatesOnlyTheMissingLabels` 钉死
      "6 个里缺 1 个 → 只发 1 次创建"。
- [x] 验证过的仓库不再检查：第二条命令不发任何远程调用。测试
      `TestEnsureRepoLabels_CacheHitMakesNoRemoteCall`。
- [x] 缓存的 key 是仓库（在哪个平台上），不是 worktree。测试
      `TestEnsureRepoLabels_CacheIsKeyedByRepository`、
      `TestEnsureRepoLabels_SameOwnerRepoOnTwoPlatforms`。
- [x] 失败的检查不写缓存，下条命令重试。测试
      `TestEnsureRepoLabels_FailedCheckIsNotRecorded`。
- [x] nil 缓存是惰性的：每次都远程读，行为跟没有缓存一样。测试
      `TestEnsureRepoLabels_NilCacheAlwaysReadsRemotely`。
- [x] 检查跑在队列挑选之前 —— 一个 catalog 里没有
      `nightme/assigned` 的仓库因此能被修好。测试
      `TestFixAuto_CreatesMissingAssignedLabelBeforePick`。
- [x] 队列那一行只在队列一个候选都没有时出现；候选全被跳过时
      不出现（"贴标签"解决不了那些原因）。测试
      `TestFixAuto_SkippedCandidate_DropsTheQueueNote`。
- [x] 检查失败时 `/gtw fix` 致命且不留痕迹。测试
      `TestFixRemote_LabelCheckFailure_IsFatal`。
- [x] 检查失败时 `/gtw pr` / `/gtw new -y` 只警告，
      而且警告出现在每一条回复上（成功和失败都有）。测试
      `TestDispatchPR_LabelCheckFailure_WarnsAndContinues`、
      `TestDispatchPR_LabelCheckFailure_WarnsOnFailureToo`、
      `TestRunNewCreate_LabelCheckFailure_WarnsAndContinues`。
- [x] provider 探测不做检查。测试
      `TestCollectPR_DoesNotTouchTheLabelCatalog`。
- [x] `LabelMetaFor` fallback（`"#ededed"` + 空描述）保证调用方
      不用先检查 map membership。

---

## 5. 错误处理矩阵

| Failure | 检测位置 | 文案 | 回滚 | worktree | branch |
|---|---|---|---|---|---|
| catalog 读失败 | `ensureRepoLabels` | `/gtw fix`：`❌ Could not ensure gtw labels on <owner>/<repo>: read label catalog: <err>`<br/>其他命令：`⚠️ could not ensure gtw labels on <owner>/<repo>: <err>` | `/gtw fix` 不需要回滚（什么都没建）<br/>其他命令不回滚 | 未创建 | 未创建 |
| `CreateLabel` 失败 | `ensureRepoLabels` | 同上（`ensure "<name>": <err>`） | 同上 | 未创建 | 未创建 |
| `AddIssueLabel(LabelWIP)` 失败 | `runFixRemote` | `❌ Could not add label <name> to issue #<id>: <err>` + stale-catalog 那一行 | `rollbackLabelStep` | 已 remove | 已 delete |
| `rollbackLabelStep` wtErr != nil | helper 内部 | "could not roll back automatically:" + [worktree]/[branch] 两行 cleanup hint | — | **未 remove**（用户手动） | **未 delete**（用户手动） |
| `rollbackLabelStep` wtErr == nil && branchErr != nil | helper 内部 | "rolled back worktree at <path>, but branch `<name>` still exists." | — | 已 remove | **未 delete**（用户手动） |
| dispatch 到 agent 失败 | `completeFixAndDispatch` | `⚠️ Could not dispatch issue #<id> to agent: <err>` | **不回滚**（worktree 是 durable side effect） | 保留 | 保留 |

**stale-catalog 那一行**：catalog 在会话早期验证过，人可能在
验证之后删掉一个标签。贴 label 失败时，回复说明这一点，并给出两条
出路 —— 重启 daemon 重新验证，或者手工建这个标签。

---

## 6. 测试计划

### 6.1 单元 / 集成测试（`internal/command/gtw/`）

- ✅ `TestEnsureRepoLabels_CreatesOnlyTheMissingLabels` — 只建缺的。
- ✅ `TestEnsureRepoLabels_CompleteCatalogCreatesNothing` — 齐了就不建。
- ✅ `TestEnsureRepoLabels_CacheHitMakesNoRemoteCall` — 命中不发远程调用。
- ✅ `TestEnsureRepoLabels_CacheIsKeyedByRepository` — key 是仓库。
- ✅ `TestEnsureRepoLabels_SameOwnerRepoOnTwoPlatforms` — 平台也在
  key 里。
- ✅ `TestEnsureRepoLabels_FailedCheckIsNotRecorded` — 失败不写缓存。
- ✅ `TestEnsureRepoLabels_FailedCreationIsNotRecorded` — 建失败同样不写缓存。
- ✅ `TestEnsureRepoLabels_NilCacheAlwaysReadsRemotely` — nil 缓存惰性。
- ✅ `TestLabelCache_NilIsInert` / `TestLabelCache_StoreThenLookup` /
  `TestLabelCache_PlatformIsPartOfTheKey`。
- ✅ `TestLabelCatalogNote_SaysWhatTheQueueNeeds` / `TestPrependWarning`。
- ✅ `TestGitHubListLabels_ReturnsCatalogNames` / `_CLIArgs` /
  `_ErrorSurfaces`，`TestGitLabListLabels_ReturnsCatalogNames` /
  `_CLIArgs` — 钉死两个 provider 的 argv 和解析。
- ✅ `TestFixAuto_CreatesMissingAssignedLabelBeforePick` — 建标签 +
  挑选顺序 + 空队列回复。
- ✅ `TestFixAuto_SkippedCandidate_DropsTheQueueNote` — 候选全被跳过
  时不出现"贴标签"那一行。
- ✅ `TestFixRemote_LabelCheckFailure_IsFatal` — 致命 + 无残留。
- ✅ `TestFixRemote_AddIssueLabelFailure_NamesStaleCatalog` — stale
  catalog 那一行。
- ✅ `TestDispatchPR_LabelCheckFailure_WarnsAndContinues` /
  `TestDispatchPR_LabelCheckFailure_WarnsOnFailureToo` /
  `TestRunNewCreate_LabelCheckFailure_WarnsAndContinues` — 两个命令
  的警告路径（成功和失败都有）。
- ✅ `TestCollectPR_DoesNotTouchTheLabelCatalog` — 探测路径不检查。
- ✅ `TestFixRemote_CheckRunsOncePerRepository` — 每个仓库只查一次。
- ✅ `TestGitHubCreateLabel_CLIArgs` — 钉死 argv 里没有 `--force`。

### 6.2 手工 smoke（不在 CI）

1. 选一个**干净**的 GitHub repo（labels 列表里没有 `nightme/*`），
   跑 `gh label list --repo <owner>/<repo>` 确认 0 个 `nightme/*`。
2. 在本地仓库 `git remote add origin <url>`，切到 main 分支。
3. 跑 `/gtw fix *`。
4. 期望：
   - `gh label list` 出现全部 7 个 `nightme/*`（颜色 + 描述跟
     `labelMeta` 表一致）。
   - 回复说 `nightme/assigned` 刚被建出来，队列等人工贴标签。
   - 再跑一次 `/gtw fix *`：不再有任何 catalog 相关的远程调用
     （缓存命中）。

---

## 7. 不在范围内（Out of Scope）

- **自定义 label 颜色 / 描述**：用 `LabelMeta` 硬编码。项目级
  配置（`gtw.yml` 里加 `labels.color_overrides`）留待后续需求。
- **gh / glab 版本探测**：不探测版本。`gh label create` 自 gh 2.0
  起就有；stderr 嗅探路径覆盖了已存在的情况。
- **把 cache 落盘**：cache 只活在 daemon 进程里；重启后重新检查
  一次，代价是每个仓库一次 catalog 读。
- **给同一个 label 加 watch / 订阅**：平台侧 label 被删时主动
  失效缓存 —— 需要解析平台的错误文本，脆；当前靠 §5 的
  stale-catalog 那一行让用户知道。
- **`nightme/assigned` 由 gtw 自己贴**：队列由人工或 tracker 上的
  外部自动化填；gtw 只负责保证这个标签**存在**。

---

## 8. 决策记录

### 8.1 为什么先读 catalog 再只建缺的

考虑过的替代方案：每次 `/gtw fix` 把 `AllLabels` 全建一遍。
**否决**：7 次 round-trip 换 1 次 catalog 读，而且用户看不出这个
仓库到底缺了什么。读了 catalog 之后，检查既便宜又说得清"我修了
什么"。

### 8.2 为什么检查跑在队列挑选之前，而不是 worktree 之后

考虑过的替代方案：把检查放在 `WorktreeAdd` 之后（跟贴 label 相
邻），失败时沿用 `rollbackLabelStep` 回滚 worktree + branch。
**否决**：那是 §1.4 描述的死锁 —— 队列永远空，流程永远走不到
worktree 之后那一步。检查必须在挑选之前。

放在挑选之前也带来一个好处：检查失败时什么都没建，不需要回滚。

### 8.3 为什么只有 `/gtw fix` 把检查失败当致命

**否决**：到处都致命 —— token 管不了 label 的仓库会把每条 gtw
命令都打断。
**否决**：到处都只警告 —— `/gtw fix` 的贴 label 会在 worktree 建
好之后才失败。
**决定**：`/gtw fix` 致命（它马上就要贴 label，没有 label 就不可能
成功），其他三条只警告（它们不碰 label，不该因为 token 管不了
label 就失败）。

### 8.4 为什么缓存永不失效

考虑过的替代方案：贴 label 失败时把缓存条目删掉。
**否决**：要解析平台的错误文本才能判断"失败是不是因为标签不
在 catalog 里"，脆。缓存条目过期的症状是可见的（贴 label 失败
+ stale-catalog 那一行），而且后果轻 —— 用户重启一下 daemon 就
重新验证了。

考虑过的替代方案：按 worktree 或 branch 做 key。
**否决**：label catalog 是仓库的属性，按仓库做 key 才对。

---

## 9. 风险与回滚

### 9.1 风险

| 风险 | 严重度 | 缓解 |
|---|---|---|
| glab 升级把 stderr 文案改了 | 低 | 双嗅探（`already exists` + `Already exists`），未来 glab 加 `--force` 就替换 |
| `LabelMetaFor` 跟实际 label 颜色漂移 | 极低 | label 颜色只在创建时被消费；已存在 label 一律 no-op，用户调过的颜色不会被覆盖 |
| catalog 超过一页（> 100 个标签） | 极低 | 多余的创建调用是幂等的（"already exists" 当成功），catalog 结果不变 |
| 运维手工删除 `nightme/*` 后再跑 `/gtw fix` | 中 | 会话中间删除 → 贴 label 失败，回复说明并给出两条出路；daemon 重启后检查会重建 |
| 缓存跨主机同名仓库 | 低 | key 里有平台：GitHub 仓库和 GitLab 项目同名同 owner 时各查各的。两个同平台的自建实例（同一 kind、同一 owner/repo）仍共用一条记录 —— host 在 yml 缓存的 provider 路径上拿不到，放进 key 会让同一个仓库因解析方式不同拿到两条记录 |

### 9.2 回滚

回滚这套机制 = revert 一个 commit。代价：

- 用户在干净 repo 上跑 `/gtw fix` 会再次失败（恢复"label 必须
  手工建好"的行为）。
- 已经建好标签的 repo 不受影响。

它本身不带 feature flag —— 加 flag 不值得（一次回滚够便宜）。

### 9.3 监控

- 检查失败没有单独的日志行：错误文本直接进用户的回复。

---

## 10. 后续 PR（不在本次）

- **F-XX**: glab stderr 文案变更的兼容（旧 glab 版本如果改
  "already exists" 措辞需要同步更新嗅探列表；目前覆盖
  `already exists` + `Already exists` 两种）。
- **F-46+**: 自定义 label 颜色 / 描述（项目级 `gtw.yml` 配置）。
- **`glab label create --force` 跟踪**：glab 上游 issue 提了之后
  替换 stderr 嗅探。

---

## 11. References

- [`F-gtw-fix.md`](./F-gtw-fix.md) — `/gtw fix` 主流程、Assess/Execute dispatch、branch 硬失败
- [`F-gtw.md`](./F-gtw.md) — gtw 命令索引
- [`docs/feat/F-50-git-provider.md`](./F-50-git-provider.md) —
  `GitProvider` 接口设计，`ListLabels` / `CreateLabel` 是其扩展
- [`internal/command/gtw/types.go`](../../internal/command/gtw/types.go) —
  `AllLabels` + `LabelMeta` + `LabelMetaFor`
- [`internal/command/gtw/provider.go`](../../internal/command/gtw/provider.go) —
  `GitHubProvider.ListLabels` / `CreateLabel`、`GitLabProvider.ListLabels` / `CreateLabel`
- [`internal/command/gtw/labels.go`](../../internal/command/gtw/labels.go) —
  `ensureRepoLabels` + `LabelCache`
- [`internal/command/gtw/labels_test.go`](../../internal/command/gtw/labels_test.go) —
  检查与缓存的单元测试
- [`internal/command/gtw/labels_flow_test.go`](../../internal/command/gtw/labels_flow_test.go) —
  四条命令的检查路径测试
- Issue #235 — 触发这套机制的载体 case
