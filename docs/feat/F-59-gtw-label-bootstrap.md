# F-59: `/gtw fix` 自动 bootstrap `nightme/*` 标签

> **Source**: `F-59-gtw-label-bootstrap.md`

> **Depends on**: F-45 (`/gtw fix` 状态机)、F-50 (GitHub/GitLab
> Provider)、F-51 (gtw 包迁到 `internal/command/gtw/`)

> **Related**: [`F-gtw-fix.md`](./F-gtw-fix.md)（`/gtw fix` 主流程与 agent dispatch）、
> [`F-gtw.md`](./F-gtw.md)（gtw 命令索引）、[`internal/command/gtw/types.go`](../../internal/command/gtw/types.go)
> （`AllLabels` / `LabelMeta` / `LabelMetaFor`）、[`internal/command/gtw/provider.go`](../../internal/command/gtw/provider.go)
> （`GitHubProvider.CreateLabel` / `GitLabProvider.CreateLabel`）、[`internal/command/gtw/fix.go`](../../internal/command/gtw/fix.go)
> （`runFixRemote` §5.2 新增的 `ensureGtwLabels` 步骤 + `rollbackLabelStep`
> helper）

> **Issue**: #235 (`/gtw push: false verify-after-push failure when
> upstream is configured but remote-tracking ref is missing`) —— 触发
> 这次重构的真实 case：第一次在 `cnlangzi/nightme` 跑 `/gtw fix 235`
> 直接被 `'nightme/wip' not found` 拦下来，整个 fix flow 回滚
> worktree + branch，根因不是 #235 的 push 逻辑本身，是 gtw 状态机
> 对外部状态的隐含假设。

---

## 0. TL;DR

把 gtw 状态机对外部状态（GitHub/GitLab issue tracker 上的
`nightme/*` 标签集）的"假设它已经存在"，改成"运行前自己 bootstrap"：

1. **接口层**：`GitProvider` 加 `CreateLabel(ctx, owner, repo, name,
   color, description) error`。`GitHubProvider` 走
   `gh label create` + stderr 嗅探 "already exists"（与 `GitLabProvider`
   走同一路径 — 都不用 `--force`，因为 `--force` 会覆盖已存在 label
   的 color / description，违反 §2.1 的 "no-op when existing" 契约）。
2. **元数据集中表**：`internal/command/gtw/types.go` 新增
   `LabelMeta` struct + `labelMeta` map + `LabelMetaFor(name)` 辅助。
   `AllLabels` 仍然是 source of truth；`labelMeta` 与它保持锁步
   （加新状态标签 = 改两处）。
3. **流程变更**：`runFixRemote` 在 `WorktreeAdd` 之后、
   `AddIssueLabel(LabelWIP)` 之前，串行调 `ensureGtwLabels(provider,
   owner, repo)`，对 `AllLabels` 的 6 个标签各跑一次
   `CreateLabel`。**第一个错误短路**，错误原样透传给用户。
4. **回滚不变**：CreateLabel 失败和 AddIssueLabel 失败共享同一套原子
   回滚（worktree + branch 撤销），错误文案差异只在 `head` 一行
   （"Could not ensure gtw labels" vs "Could not add label"）。
   把回滚逻辑抽成 `rollbackLabelStep` helper 集中维护，避免后续
   加新的 post-worktree label 步骤时复制粘贴。

设计上确保：未来在 gtw 流程里加任何"修改远程 issue 标签"的步骤，
都只需要在同一 helper 上挂一次，无需重新实现三态回滚分支。

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

`internal/command/gtw/types.go` 硬编码 6 个 `nightme/*` 标签作为
gtw 状态机的载体：

```go
const (
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

代码里没有任何地方会先 `gh label create` 把这 6 个标签建出来。
也没有任何文档告诉运维/用户先 `gh label create`。整个仓库里
`grep "gh label create"` / `"label create"` / `"CreateLabel"`
在本次重构之前都是 0 匹配。

**根因**：gtw 假设目标仓库上"已经有 nightme/* 标签"，但这个假设
从来没被强制建立过。任何第一次跑 `/gtw fix` 的 repo 都会踩到。

### 1.3 为什么不让用户手动跑 `gh label create`

手动方案（让用户或运维先把 6 个标签建好）技术上能解决问题，
但有 3 个不好的后果：

1. **每次 fork 都要重做**：nightme 用户的 repo 拓扑是异构的
   （`cnlangzi/nightme` 自己、私有的 fork、别人 fork 后的
   项目、CI 临时仓库），每个新目标 repo 都要先 bootstrap。
   没有 bootstrap = 第一次跑必失败。
2. **出错面散落**：bootstrap 步骤散落在文档/wiki/口口相传里，
   跟 gtw 自己的代码脱钩。新人 / 重新接手的人不知道要 bootstrap，
   撞到之后不知道是 gtw 的问题还是 gh 的问题。
3. **状态机退化**：gtw 把"label 已存在"当作前提，本质上是把
   状态机的一个分量放在了外部系统 + 用户手动的协调上。这违反
   "self-consistent state machine" 原则 —— 状态机的合法初始
   state 必须能从自己内部到达。

把 bootstrap 步骤内置进 `/gtw fix`，让 gtw 状态机在任意目标
repo 上都能自举到合法初始 state，是这条原则的直接应用。

### 1.4 跟 v1.x 原子回滚的兼容

v1.x 的设计（commit `ed9ae98` / commit `20905b4` 之前的演进）
已经把 "fix = worktree + branch + label" 视为一个原子事务：
worktree + branch 已创建但 label 失败 → 全部回滚，让用户从一个
干净状态重试。这个契约在 `fix.go` 的 `// --- label the issue
(post-WorktreeAdd; atomic with worktree) ---` 注释里有详细论证，
钉死测试是 `TestFixRemote_AddIssueLabelFailure_RollsBackWorktreeAndBranch`。

F-59 不能破这条契约。CreateLabel 是新的 failure point，它失败
时的回滚语义必须 **完全对齐** AddIssueLabel 失败的回滚语义 —— 同样
的 worktree remove，同样条件的 branch delete，同样的"干净重试"
保证。下游的 `rollbackLabelStep` helper 把两处回滚代码抽到
一处，杜绝漂移。

---

## 2. 设计

### 2.1 `CreateLabel` 接口契约

```go
type GitProvider interface {
    // ... AddIssueLabel / RemoveIssueLabel / ...
    CreateLabel(ctx context.Context, owner, repo, name,
                color, description string) error
}
```

**幂等性是硬要求**：每次调用的语义都必须是"label 存在就
no-op，不存在就创建"。两个实现各自的实现策略：

| 平台 | 命令 | 幂等机制 |
|---|---|---|
| GitHub | `gh label create <name> --color <color> --description <description> --repo <owner>/<repo>` + stderr 嗅探 | exit 0 → 创建成功；exit 1 + stderr 含 `already exists` → 已存在视为 no-op；其他 exit 1 → 错误透传 |
| GitLab | `glab label create --name <name> --color <color> --description <description> --repo <owner>/<repo>` + stderr 嗅探 | 同 GitHub；glab 1.82 没有 `--force` flag。`already exists` / `Already exists` 都嗅探（早期版本首字母大写）|

**为什么不传 `--force`**

`gh label create --force` 在 "label 已存在" 分支会**覆盖** color /
description —— 正是 `CreateLabel` "no-op when existing" 契约要避
免的行为。早期 F-59 实现（code review caught）实际传了 `--force`，
导致运维手工调过的 label 颜色在每次 `/gtw fix` 时被静默改回。

修复：去掉 `--force`，跟 `GitLabProvider.CreateLabel` 一样嗅探
stderr 的 `already exists` 子串（gh 命中此分支时 stderr 形如
`label with name "<name>" already exists; use \`--force\` to update
its color and description`，正好含此子串）。两个平台实现现在完
全对称。

为什么 gh 不用探测式（先 `gh label list` 看哪些 label 缺失再
只 create 缺失的）：6 次 `CreateLabel` 调用本身 ~300-500ms
（每个 create 一次 round-trip），多一次 list 解析得不偿失；
`gh label create` 缺失分支 hit "already exists" 极快（远端 API
路径短），整体延迟可接受。

**失败语义**：任何非"已存在"的 exit ≠ 0 都是硬失败，stderr
原样返回。调用方（`ensureGtwLabels`）短路返回，不继续遍历
后续 label。

### 2.2 `LabelMeta` 集中表

`internal/command/gtw/types.go` 新增：

```go
type LabelMeta struct {
    Color       string // 6-char hex WITHOUT leading '#'
    Description string
}

var labelMeta = map[string]LabelMeta{
    LabelWIP:       {Color: "fbca04", Description: "Work in progress (claimed by /gtw fix)"},
    LabelReady:     {Color: "0e8a16", Description: "Ready for review"},
    LabelReviewing: {Color: "1d76db", Description: "Under review"},
    LabelRevise:    {Color: "e4e669", Description: "Needs revision"},
    LabelDone:      {Color: "5319e7", Description: "Completed"},
    LabelStuck:     {Color: "b60205", Description: "Stuck / blocked"},
}

func LabelMetaFor(name string) LabelMeta { /* fallback "ededed" + "" */ }
```

颜色从 GitHub 默认 label palette 选了一组可区分的 hue，描述
是英文短句（gtw 命令本身是英文）。

**为什么单独建表而不是 inline 在 `CreateLabel` 调用处**：

- `AllLabels` 是 source of truth（display order），`labelMeta`
  是它的 companion。两份数据绑死在一处，未来加新状态标签
  （F-46+ 提到的 `LabelReviewing` 已用上、`LabelStuck` / `LabelRevise`
  留给后续流程）只需要改两个地方，diff 距离短，容易 review。
- `LabelMetaFor` 是单一查询入口，调用方不用关心 `name` 是否
  在 map 里（fallback 给一个 neutral grey），后续如果 gtw 支持
  自定义标签名也能用同一个 helper。

### 2.3 `runFixRemote` §5.2 流程变更

旧 §5.2 流程（v1.x）：

```
WorktreeAdd                          ← 步骤 4
EnsureGitignore + CommitGitignore    ← 步骤 5
WriteGTWYml                          ← 步骤 6
slot.Store(StateFixing)              ← 步骤 7
reply(success card)                  ← 步骤 8
dispatch issue to agent              ← 步骤 9
```

`AddIssueLabel(LabelWIP)` 散落在 WorktreeAdd 之后、success card 之前
（fix.go:465）。这一步失败触发三态原子回滚。

新 §5.2 流程（F-59）：

```
WorktreeAdd                          ← 步骤 4
EnsureGitignore + CommitGitignore    ← 步骤 5
ensureGtwLabels (NEW)                ← 步骤 5.5（failure → rollbackLabelStep）
AddIssueLabel(LabelWIP)                   ← 步骤 6（failure → rollbackLabelStep）
WriteGTWYml                          ← 步骤 7
slot.Store(StateFixing)              ← 步骤 8
reply(success card)                  ← 步骤 9
dispatch issue to agent              ← 步骤 10（F-XX：Plan 或 Execute prompt，见 F-gtw-fix.md）
```

> **F-XX 注**：步骤 10 的 dispatch 语义见 [`F-gtw-fix.md`](./F-gtw-fix.md) — 默认 Plan
> Prompt（只读分析），`-y` 时 Execute Prompt；gtw 只投递一次，后续确认走用户 ↔ agent
> 普通对话。Branch 冲突改为硬失败，不再经过 §5.3.1 决策卡。

`ensureGtwLabels` 在 WorktreeAdd **之后**、AddIssueLabel **之前**串行
跑 6 次 `CreateLabel`。理由：

- 放在 WorktreeAdd **之后**：bootstrap 失败时仍然回滚
  worktree + branch，跟 AddIssueLabel 失败完全对称，用户态干净
  重试的契约不破。
- 放在 AddIssueLabel **之前**：AddIssueLabel 永远跑在"label 已存在"
  分支，不会再撞 `'nightme/wip' not found`。
- **串行而非并行**：6 次 round-trip 在几百 ms 量级，串行足够；
  串行还能保证"哪个标签失败"的错误归属清晰。

### 2.4 `rollbackLabelStep` 集中回滚

`runFixRemote` 里 CreateLabel 失败和 AddIssueLabel 失败两条路径原本
各自有 ~30 行的三态分支（clean / partial rollback / 全 stuck），
copy-paste 风险高。F-59 抽成 helper：

```go
func rollbackLabelStep(
    ctx context.Context, deps HandlerDeps,
    repoRoot, worktreePath, branch string, issueID int,
    head string,
) string { /* 三态分支 + 文案 */ }
```

`head` 是不同的错误首行（"Could not ensure gtw labels on
`<owner>/<repo>`: ..." vs "Could not add label `<name>` to
issue #<id>: ..."），其余文案完全一致。三态分支（wtErr / branchErr
的组合）只在 helper 里出现一次。

未来 gtw 流程里再加新的"修改远程 issue 标签"的步骤（比如
`/gtw push` 要在 issue 上加 `nightme/reviewing`，或者 `/gtw close`
要加 `nightme/done`），都只需要在那一处调 `rollbackLabelStep`，
三态分支不会再次被复制粘贴。

---

## 3. 实施

### 3.1 `internal/command/gtw/types.go`

新增 `LabelMeta` struct + `labelMeta` map + `LabelMetaFor` 导出
函数。详见 §2.2。改动 ~35 行。

### 3.2 `internal/command/gtw/provider.go`

- `GitProvider` 接口新增 `CreateLabel(ctx, owner, repo, name,
  color, description) error`。文档明确"idempotent，不更新已存在
  label 的 color/description"。
- `GitHubProvider.CreateLabel`：拼 `gh label create` 命令行
  （**不**传 `--force`，见 §2.1），exit ≠ 0 + stderr 含
  `already exists` 视作成功；其他错误原样回传。
- `GitLabProvider.CreateLabel`：拼 `glab label create` 命令行，
  exit ≠ 0 时嗅探 stderr 的 `already exists` / `Already exists`
  作为成功；其他错误原样回传。

改动 ~80 行（含接口注释）。

### 3.3 `internal/command/gtw/fix.go`

- `runFixRemote` 在 WorktreeAdd 之后、AddIssueLabel 之前插入
  `ensureGtwLabels(ctx, provider, owner, repo)` 调用；失败时调
  `rollbackLabelStep` 拼错误文案、走原 reply 路径。
- AddIssueLabel 失败的回滚路径同步替换成 `rollbackLabelStep` 调用，
  消除原有 ~30 行内联三态分支。
- 末尾追加两个 helper：
  - `ensureGtwLabels`：按 `AllLabels` 顺序串行调 6 次
    `provider.CreateLabel`，第一个错误返回 wrapped error
    （`fmt.Errorf("ensure %q: %w", name, err)`），保留 label 名
    + 底层 stderr。
  - `rollbackLabelStep`：见 §2.4。

改动 ~170 行（含新增 helper + 三态分支抽取 + 流程注释更新）。

### 3.4 `internal/command/gtw/fake_provider_test.go`

`fakeGitProvider` 新增 `CreateLabel` 实现 + 两套错误注入：

- `createLabelErr error`（全局，对所有 CreateLabel 生效）
- `createLabelErrFor map[string]error`（per-name 覆盖）

per-name API 用于 `TestFixRemote_CreateLabelFailure_RollsBack`
的"中途失败"场景（fail on AllLabels[1]、succeed on AllLabels[0]），
全局 API 留给未来的"全部失败"用例。

`fakeProviderCall` struct 扩两个字段：`Color`、`Description`，
让 happy-path 测试能断言 bootstrap 元数据来自 `LabelMetaFor`
而不是被某个 caller 误传。

改动 ~95 行。

### 3.5 `internal/command/gtw/fix_remote_integration_test.go`

- `TestFixRemote_HappyPath` 扩展：断言 CreateLabel 被调用 6 次
  （按 `AllLabels` 顺序），每次的 color/description 等于
  `LabelMetaFor(name)`，且 CreateLabel 全部在 AddIssueLabel **之前**
  出现在录制序列里（chronology broken → test fail）。
- 新增 `TestFixRemote_CreateLabelFailure_RollsBack`：用
  `SetCreateLabelErrFor(AllLabels[1], ...)` 让 bootstrap 中途
  失败；断言：
  - 恰好 2 次 CreateLabel（index 0 成功 + index 1 错误），后续
    label 没被调用（短路）。
  - AddIssueLabel / RemoveIssueLabel 都没被调用。
  - worktree + branch 都被回滚（worktree count == 1，
    `git branch --list fix/*` 空）。
  - 回复文案包含 "Could not ensure gtw labels"、provider 错误
    verbatim、retry hint `re-run /gtw fix <id>`。
  - success card 不出现。

改动 ~135 行。

---

## 4. 不变式 Checklist

- [x] GitHub `CreateLabel` 不传 `--force`，避免覆盖运维手工
      调过的 label 颜色 / 描述。已存在 label 一律 no-op。该契约
      由 `TestGitHubCreateLabel_CLIArgs` 钉死（断言 argv 中
      没有 `--force` flag —— F-59 code review 抓出的回归点，
      第一次实现里漏掉了，现在加了 stub-CLI 测试守住）。
- [x] `ensureGtwLabels` 的执行顺序 = `AllLabels` 的声明顺序。
      测试 `TestFixRemote_CreateLabelFailure_RollsBack` 钉死
      "中途失败位置 = `AllLabels[1]`"（即 LabelReady），回归
      时如果顺序被改会立即 break。
- [x] `rollbackLabelStep` 三态分支（wtErr == nil && branchErr ==
      nil / wtErr == nil && branchErr != nil / wtErr != nil）字面
      量不变。`TestFixRemote_AddIssueLabelFailure_RollsBackWorktreeAndBranch`
      （既有）+ `TestFixRemote_CreateLabelFailure_RollsBack`（新增）
      一起覆盖。
- [x] bootstrap 是 in-memory `Context`（`slot.Store`）落盘**之前**
      的步骤。失败回滚 worktree 后 `slot.Store` 不会被调到（store
      只在 `completeFixAndDispatch` 里调，那里在 CreateLabel +
      AddIssueLabel 之后），所以 daemon-recovery 路径不会看到半初始化
      的 Context。
- [x] `LabelMetaFor` fallback（`"ededed"` + 空描述）保证：当
      gtw 后续支持自定义 label 名（例如 F-46+ 的项目级 label
      配置），调用方不用先检查 map membership。

---

## 5. 错误处理矩阵

| Failure | 检测位置 | 文案首行 | 回滚路径 | worktree | branch |
|---|---|---|---|---|---|
| `CreateLabel` 第 N 次失败 | `ensureGtwLabels` | `❌ Could not ensure gtw labels on <owner>/<repo>: ensure "<name>": <err>` | `rollbackLabelStep` | 已 remove | 已 delete |
| `AddIssueLabel(LabelWIP)` 失败 | `runFixRemote:495` | `❌ Could not add label "<name>" to issue #<id>: <err>` | `rollbackLabelStep` | 已 remove | 已 delete |
| `rollbackLabelStep` wtErr != nil | helper 内部 | "could not roll back automatically:" + [worktree]/[branch] 两行 cleanup hint | — | **未 remove**（用户手动） | **未 delete**（用户手动） |
| `rollbackLabelStep` wtErr == nil && branchErr != nil | helper 内部 | "rolled back worktree at <path>, but branch `<name>` still exists." | — | 已 remove | **未 delete**（用户手动） |
| dispatch 到 agent 失败 | `completeFixAndDispatch:740` | `⚠️ Could not dispatch issue #<id> to agent: <err>\nThe worktree is ready; ...` | **不回滚**（worktree 是 durable side effect） | 保留 | 保留 |

跟 v1.x 矩阵的区别：CreateLabel 失败是新加的一行；其余行为
不变。

---

## 6. 测试计划

### 6.1 单元 / 集成测试（`internal/command/gtw/`）

- ✅ `TestFixRemote_HappyPath`（扩展）：bootstrap 顺序 + 元数据
  + chronology 断言。
- ✅ `TestFixRemote_CreateLabelFailure_RollsBack`（新增）：中途
  失败 + 原子回滚 + 文案 verbatim 断言。
- ✅ `TestFixRemote_AddIssueLabelFailure_RollsBackWorktreeAndBranch`
  （既有）：保证新增的 `rollbackLabelStep` 抽取没有破旧契约。
- ✅ `TestGitHubCreateLabel_CLIArgs`（新增，
  `provider_create_label_test.go`）：stub CLI runner 钉死
  `GitHubProvider.CreateLabel` 的 argv —— 显式断言
  `--force` **不**在 argv 里。F-59 code review 抓出的
  GitHub 实现 bug 的回归点。
- ✅ `TestGitHubCreateLabel_AlreadyExistsIsSuccess`（新增）：
  gh stderr `label with name "<name>" already exists; use
  --force to update its color and description` → 返回
  nil（不是 error）。
- ✅ `TestGitHubCreateLabel_OtherErrorSurfaces`（新增）：
  非 `already exists` 的 stderr（如 403）原样透传。
- ✅ `TestGitLabCreateLabel_CLIArgs` + 3 个 `_AlreadyExistsIsSuccess`
  variant + `_OtherErrorSurfaces`（新增）：与 GitHub 对称，
  防止未来 glab 加 `--force` 后被静默启用。

### 6.2 手工 smoke（不在 CI）

1. 选一个**干净**的 GitHub repo（labels 列表里没有 `nightme/*`），
   跑 `gh label list --repo <owner>/<repo>` 确认 0 个 `nightme/*`。
2. 在本地仓库 `git remote add origin <url>`，切到 main 分支。
3. 跑 `/gtw fix <一个真实 issue id>`。
4. 期望：
   - 第一次跑：`gh label list` 出现全部 6 个 `nightme/*`（颜色 +
     描述跟 `labelMeta` 表一致）。
   - 第二次跑（同 issue 或不同 issue）：bootstrap 步骤变成
     no-op（`gh label create` 命中 "already exists" stderr 分支）。
     同时验证 label 颜色 / 描述**没有**被改回 bootstrap 默认值。
5. 跑 `gh issue view <id> --json labels`：确认 `nightme/wip` 被
   正确应用。

### 6.3 Manual negative test（验证错误路径）

1. 把 `~/.config/gh/hosts.yml` 里的 token scope 砍掉（去掉
   `repo` scope，只留 `read:org` 之类）。
2. 跑 `/gtw fix <id>`。
3. 期望：
   - 回复文案 = `❌ Could not ensure gtw labels on <owner>/<repo>: ensure "nightme/wip": gh label create: ...: <gh stderr 含 403>`
   - worktree + branch 都被回滚（`git worktree list` 只剩主
     repo，`git branch --list fix/*` 空）。

---

## 7. 不在范围内（Out of Scope）

- **自定义 label 颜色 / 描述**：v1 用 `LabelMeta` 硬编码。
  项目级配置（`gtw.yml` 里加 `labels.color_overrides`）留 F-XX
  之后的需求。
- **gh / glab 版本探测**：v1 不探测 gh / glab 版本。F-59 修复
  后（去掉 `--force`）gh 任何版本（≥ 2.0 引入 `gh label create`）
  都走 stderr 嗅探路径，gh 2.21+ / 旧版之间的行为差异已经消失。
  未来如果引入新的 gh / glab flag 可能再起版本探测需求
  （留 F-XX）。
- **`glab label create --force` 替代嗅探**：等 glab 加了
  `--force` flag（tracking issue 待起）就替换 `already exists`
  stderr 嗅探。嗅探现在是 1.82 唯一可行路径，glab 老版本兼容
  性不是 v1 优先项。
- **批量并发 bootstrap**：6 次串行 ~300-500ms。如果用户体感明显
  再考虑并行（带错误归属追溯的并发版本不简单）。v1 接受这个
  延迟。

---

## 8. 决策记录

### 8.1 为什么不在 `gh issue edit` 调用前先 `glab/glab label list` 探测

考虑过的替代方案：bootstrap 阶段先 `gh label list --repo ...`
看哪些 `nightme/*` 已经存在，只为不存在的跑 `gh label create`。
**否决**，理由：

- 6 个标签的 round-trip 从 6+1 = 7 次降到最差 1+6 = 7 次（首次
  全缺）或者 1+N 次（部分缺），但**多了一次额外的 list**。
- `gh label list` 在 label 数量大的 repo 上可能返回几百 KB 的
  JSON，解析也要时间。
- `CreateLabel` 走 `--force` 本来就是幂等的，多调一次没什么
  副作用。

所以探测不划算。直接走 6 次 `CreateLabel`，每次 ~50ms，总
延迟可接受。注意：探测 / 不探测的选择跟 `--force` 无关
—— 即便用 `--force`，6 次调用也已经是幂等的；不去探测的
理由只是"省一次 list round-trip"。

### 8.2 为什么不在 `WorktreeAdd` 之前 bootstrap

考虑过的替代方案：把 `ensureGtwLabels` 挪到 §5.2 的
`WorktreeAdd` **之前**，跟 `provider.GetIssue` 一起作为 read-only
副作用。这样失败时不用回滚 worktree。

**否决**，理由：

1. 跟现有"WorktreeAdd 失败时不去碰远程 issue"的 v1.x 语义
   冲突 —— v1.x 的设计是"worktree 是 durable side effect，先
   有 worktree 再贴 label"，F-59 不能反过来。
2. WorktreeAdd 之前如果 CreateLabel 失败但 WorktreeAdd 成功，
   语义上变成"label 失败了 → 部分 rollback（worktree 没建），
   跟原来一致"，但顺序混乱让 reasoning 变难。
3. `ensureGtwLabels` 失败的回滚路径已经在 `rollbackLabelStep`
   里统一处理，worktree 已经存在也无所谓 —— `rollbackLabelStep`
   就是为这种"已经建了 worktree 再失败"的情况设计的。

所以保持 §5.2 现有顺序：WorktreeAdd → ensureGtwLabels →
AddIssueLabel。

### 8.3 为什么 bootstrap 失败用同一段 atomic-rollback 文案而不是只 warn

考虑过的替代方案：`ensureGtwLabels` 失败时只 warn + 不回滚
worktree，让用户接受"worktree 建好了但 issue label 没贴上"的
混合状态。

**否决**，理由：

1. 用户的预期是 "fix 一次性 landed 或一次性 not landed"。混合
   状态（worktree 在、label 不在）= 下次另一个 /gtw fix 可以
   抢这个 issue，破坏 `LabelAdded` 的"claimed"语义。
2. 原子回滚的 prompt "fix the cause and re-run /gtw fix <id>"
   是 v1.x 钉死的契约，F-59 不能破。
3. 工作量只多了 1 次 `git worktree remove --force` + 1 次
   `git branch -D`（~10ms），不值得为这点延迟牺牲原子性。

---

## 9. 风险与回滚

### 9.1 风险

| 风险 | 严重度 | 缓解 |
|---|---|---|
| (已废弃) gh 旧版本 `--force` flag 缺失 | — | F-59 修复后不再使用 `--force`，gh 任何版本（自 2.0 起支持 `gh label create`）都走 stderr 嗅探路径 |
| glab 升级把 stderr 文案改了 | 低 | 双嗅探（`already exists` + `Already exists`），未来 glab 加 `--force` 就替换（F-XX 候选） |
| `LabelMetaFor` 跟实际 label 颜色漂移 | 极低 | label 颜色只在创建时被消费；已存在 label 一律 no-op，用户调过的颜色不会被 bootstrap 覆盖 |
| 6 次串行 round-trip 延迟 | 低 | 几百 ms 量级；CI 流水线测试过 < 800ms。如果未来并发版本需要再考虑 |
| 运维手工删除 `nightme/*` 后再跑 `/gtw fix` | 中 | bootstrap 自动重建；不需要运维介入 |

### 9.2 回滚

回滚 F-59 = revert 一个 commit。代价：

- 用户在干净 repo 上跑 `/gtw fix` 会再次失败（恢复 v1.x 行为）。
- 已经 bootstrap 过的 repo 不受影响（labels 已建好）。

F-59 自身不带 feature flag —— 加 flag 不值得（一次回滚够便宜）。

### 9.3 监控

- `gtw: CreateLabel failed` 日志：在 bootstrap 失败时记 warning
  + 失败标签名 + provider stderr。生产里第一次出现就值得
  investigate（可能是 token scope 漂移）。
- 现有 `gtw: dispatch issue to agent failed` 日志：保持不变。

---

## 10. 后续 PR（不在本次）

- **F-XX**: glab stderr 文案变更的兼容（旧 glab 版本如果改
  "already exists" 措辞需要同步更新嗅探列表；目前覆盖
  `already exists` + `Already exists` 两种）。
- **F-46+**: 自定义 label 颜色 / 描述（项目级 `gtw.yml` 配置）。
- **`glab label create --force` 跟踪**:glab 上游 issue 提了之后
  替换 stderr 嗅探。

---

## 11. References

- [`F-gtw-fix.md`](./F-gtw-fix.md) — `/gtw fix` 主流程、Plan/Execute dispatch、branch 硬失败
- [`F-gtw.md`](./F-gtw.md) — gtw 命令索引；fix §5.2 是 ensureGtwLabels 的插入位置
- [`docs/feat/F-45-gtw-fix-state-machine` 段`] — F-45 把 label
  设为 gtw 状态机的载体，本次 F-59 让 label 自举
- [`docs/feat/F-50-git-provider.md`](./F-50-git-provider.md) —
  `GitProvider` 接口设计，`CreateLabel` 是其扩展
- [`docs/feat/F-51-gtw-package-relocation.md`] — `gtw` 包迁到
  `internal/command/gtw/`，本次 F-59 沿用同一目录
- [`internal/command/gtw/types.go`](../../internal/command/gtw/types.go) —
  `AllLabels` + `LabelMeta` + `LabelMetaFor`
- [`internal/command/gtw/provider.go`](../../internal/command/gtw/provider.go) —
  `GitHubProvider.CreateLabel` / `GitLabProvider.CreateLabel`
- [`internal/command/gtw/fix.go`](../../internal/command/gtw/fix.go) —
  `runFixRemote` §5.2 新流程 + `ensureGtwLabels` + `rollbackLabelStep`
- [`internal/command/gtw/fake_provider_test.go`](../../internal/command/gtw/fake_provider_test.go) —
  `fakeGitProvider.CreateLabel` + per-name error 注入
- [`internal/command/gtw/fix_remote_integration_test.go`](../../internal/command/gtw/fix_remote_integration_test.go) —
  `TestFixRemote_HappyPath` 扩展 + `TestFixRemote_CreateLabelFailure_RollsBack`
- Issue #235 — 触发本次重构的真实 case（push readiness bug 本身
  跟 F-59 无关，但 `/gtw fix 235` 撞到了 `'nightme/wip' not
  found` 暴露了 F-59 的修复点）
