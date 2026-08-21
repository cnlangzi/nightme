# F-PATHUTIL-001: 统一跨平台路径处理包 (`internal/pathutil`)

> **Status**: implemented
> **Related docs**: [`../SPEC.md`](../SPEC.md) §13（跨平台类库使用规范）; [`../WINDOWS.md`](../WINDOWS.md)

---

## 1. 背景与动机

### 1.1 触发事件

在 Windows 下运行 `/gtw close` 时,`git worktree remove` 报错:

```
❌ git worktree remove failed: gtw: git worktree worktree_remove:
   exit status 255: error: failed to delete
   'F:/nightme.nightme/fix-windows-cli-style': Invalid argument
```

错误里的路径是 `F:/...`(前斜杠),而 git for Windows 在某些 argv / Win32 syscall 组合下会把这个带前斜杠的路径交给 `RemoveDirectoryW`,得到 `ERROR_INVALID_PARAMETER`("Invalid argument")。

### 1.2 路径流向分析

路径在系统中经过多个"产生方"和"消费方",每个交接点都可能引入平台相关的形态差异:

| 步骤 | 路径来源 | 形态 (Windows) |
|------|---------|---------------|
| 1. 用户 `/cwd F:\foo` | `cwd/cmd.go:Handle` | 经 `path_windows.go::resolvePath` → **`filepath.Clean` + `filepath.Abs`** → 反斜杠 |
| 2. `cs.SetSelectedCwd` 存入 | `chatsession/chatsession.go:468` | 原样存(已是反斜杠) |
| 3. `gtw/fix.go::RunFix` 调 `RepoRoot` | `worktree.go:48` | 跑 `git rev-parse --show-toplevel` → **git for Windows 原生返回 POSIX 风格(前斜杠)** |
| 4. `WorktreePath(repoRoot, slug)` | `slug.go:168` | `filepath.Clean` + `filepath.Join` → 反斜杠 |
| 5. `WriteGTWYml` 序列化 | `persist.go:212` | **原样写入,无规范化** |
| 6. yml 在磁盘上 | (文件) | 可能前斜杠 / 反斜杠(取决于 step 4 是否完全覆盖) |
| 7. `ReadGTWYml` 读回 | `persist.go:249` | **原样读回,无规范化** |
| 8. `RunClose` 传给 `WorktreeRemove` | `close.go:239` | 原样透传 |
| 9. `WorktreeRemove` 传给 git argv | `worktree.go:378` | 原样透传 |
| 10. git 内部 Win32 syscall | (git 内部) | **可能把 `F:/foo` 转成 `F:\foo` 时失败** |

**关键缺陷**:
- `/cwd` 命令入口已经按平台分流(`path_windows.go` / `path_unix.go`),但 `/gtw` 全程没有
- yml 是路径数据的"持久化层",在写 / 读两端都没有规范化
- 每个 caller 都得自己记得做平台相关处理(实际上没人记得)

### 1.3 设计目标

1. **入口规范化**:任何进入 nightme 的路径字符串(来自 yml / git / 用户 / 外部)在送进 OS 或子进程前,过一遍规范化
2. **集中一处**:路径相关的平台规则全部在 `internal/pathutil`,调用方不直接 import `path/filepath` 的平台差异部分
3. **可测**:平台分流测试(`path_windows_test.go` 风格)在 Windows CI 上可运行
4. **向后兼容**:不破坏现有 `cwd` 包语义,二者并存,`cwd` 逐步迁移或共存

---

## 2. 包结构

```
internal/pathutil/
├── normalize.go              # 平台无关:IME 规范化、纯字符串 Clean 的薄封装
├── path_unix.go              //go:build !windows
│                             # POSIX:IsAbs / Abs / Clean 直接转包
├── path_windows.go           //go:build windows
│                             # 驱动器、UNC、前斜杠规范化、isWindowsDriveRel
├── path_unix_test.go
└── path_windows_test.go
```

包结构**与 `internal/command/cwd/` 完全对称**(`cmd.go` + `path_unix.go` + `path_windows.go` + `normalize.go` + `path_windows_test.go`)—— 这是 [`../SPEC.md`](../SPEC.md) §13 定义的"跨平台行为集中在一个包"的范式。

---

## 3. 公开 API

### 3.1 NormalizeForOS

```go
// NormalizeForOS 把任意来源的路径字符串转换成当前 OS 的本地表示:
//
//   - Windows:
//       "F:/foo/bar"           → "F:\foo\bar"           (驱动器路径,前斜杠→反斜杠)
//       "/foo"                 → "<drv>:\foo"           (根相对,补当前驱动器)
//       "\\?\F:\foo"           → "\\?\F:\foo"           (长路径前缀保留)
//       "C:foo"                → error                  (驱动器相对歧义,显式拒绝)
//       "foo" / "./foo"        → 原样                   (相对路径不强制 abs)
//
//   - Unix:
//       原样返回(只做 filepath.Clean)
//
// 用于:在送进 OS 调用或子进程 argv 前统一一次。
// 调用方:`RunClose` 读 yml 后、`cs.SetSelectedCwd` 写入前、`WorktreeRemove` 内部。
func NormalizeForOS(p string) (string, error)
```

### 3.2 NormalizeForGit

```go
// NormalizeForGit 在 NormalizeForOS 的基础上,额外保证传给 git 的 argv 路径
// 是 git for Windows 不会二次"误转换"的形式:
//
//   - Windows: 强制反斜杠。即使 git for Windows 接受前斜杠,
//     但它在某些 argv / Win32 syscall 组合下会把 "F:/foo" 当成
//     "F:\foo" 时触发 ERROR_INVALID_PARAMETER("Invalid argument")。
//     我们这里强制为反斜杠,把"是否转换"的决定权拿回 Go 侧。
//   - Unix: 原样(本来就是 POSIX 风格)。
//
// 用于:每次 `git.Run(...)` 组装 argv 之前。
// 已知调用点:`internal/command/gtw/worktree.go::WorktreeRemove`、
//          `WorktreeAdd`、`BranchExists` 等所有走 git argv 的函数。
func NormalizeForGit(p string) (string, error)
```

### 3.3 Equal

```go
// Equal 是路径等价比较的单一入口:
//
//   - Windows:大小写不敏感(驱动器字母 / 文件名 / 扩展名),
//              分隔符统一(前斜杠 / 反斜杠等价),去除尾部分隔符
//   - Unix:   按字节比较(Windows 上的 Git Bash 不在考虑范围,
//             因为我们 spawn 的不是 bash 子进程)
//
// 用途:取代散落在代码里的 `filepath.Clean(a) == filepath.Clean(b)` 比较,
// 因为后者在 Windows 上大小写敏感,会把 `C:\Foo` 和 `c:\foo` 判成不等。
//
// 已知可立即受益的调用点:
//   - gtw/fix.go:344   `existingPath != "" && filepath.Clean(existingPath) == filepath.Clean(worktreePath)`
//   - gtw/fix.go:560   同上
//   - gtw/close.go     stat / slot 比对
func Equal(a, b string) bool
```

### 3.4 IsUnder

```go
// IsUnder 报告 child 是否在 parent 之下(同一盘符 / 前缀关系)。
//
// 语义:
//   - Windows: 大小写不敏感 + 规范化分隔符 + 去除尾部分隔符 + 必须同盘符
//   - Unix:    字节级前缀 + 不允许 `..` 越界
//
// 用途:校验 worktree 必须在 repoRoot 之下这一类"目录归属"判断。
// 已知可受益:preflight、gtw/close.go 的 stat 安全检查。
func IsUnder(child, parent string) bool
```

### 3.5 FromSlash / ToSlash (薄封装,可选导出)

```go
// FromSlash 在 Windows 上把路径里的 '/' 替换成 '\\'。
// Unix 上原样返回。是 filepath.FromSlash 的薄包装,仅作为:
//   - "跨平台类库使用规范"要求的统一入口(见 SPEC.md §13)
//   - 让 grep `pathutil.FromSlash` 能定位到所有调用点
//
// 大多数 caller 应该直接用 NormalizeForOS / NormalizeForGit,本函数只在
// caller 明确只想做"分隔符转换"这一件事时使用。
func FromSlash(p string) string

// ToSlash 反向。Windows '\\' → '/';Unix 原样。
func ToSlash(p string) string
```

---

## 4. 不变式 / 设计决策

### 4.1 单一入口原则

任何 caller 需要做以下事情时,**必须**走 `pathutil`:

| 场景 | 必须用 | 禁止 |
|------|--------|------|
| 判断绝对路径 | `pathutil.IsAbs` / `NormalizeForOS` | 直接 `filepath.IsAbs` |
| 拼接路径 | `pathutil.Join` | `filepath.Join` |
| 清理路径 | `pathutil.Clean` / `NormalizeForOS` | `filepath.Clean` |
| 比较路径 | `pathutil.Equal` | `==`、`strings.EqualFold`、手写 Clean+比 |
| 喂给 git argv | `pathutil.NormalizeForGit` | 直接传 |
| 喂给 OS 调用(`os.Stat` 等) | `pathutil.NormalizeForOS` | 直接传 |
| 转换分隔符 | `pathutil.FromSlash` / `pathutil.ToSlash` | `filepath.FromSlash` / `filepath.ToSlash` |

### 4.2 不引入新的错误

`NormalizeForOS` 只在以下场景返回错误:
- Windows 上的 `C:foo` 驱动器相对路径(歧义,显式拒绝——同 `cwd/path_windows.go::isWindowsDriveRel`)
- HOME unset 等系统级错误(Unix 上 `$HOME` 不可用时)

其它情况一律尽力转换,不抛错。

### 4.3 不做"silent join $HOME"

`NormalizeForOS` **不会**把 `foo` 静默拼上 `$HOME`——这是 `cwd::resolvePath` 的行为,不是 pathutil 的语义。pathutil 只做"形态转换",不改变路径所指的目录。

调用方如果想做 HOME-relative 解析,显式调 `cwd::resolvePath` 或新写一个 helper(不在 pathutil 范围内)。

### 4.4 不破坏现有 `cwd` 包

`internal/command/cwd/` 的 `path_windows.go` 已经实现了完整的 Windows 路径解析(驱动器 / UNC / 驱动器相对拒绝)。本包不与之合并,但 cwd 内部的某些 helper(如 `verifyDirectory` 的存在性检查语义)可以**逐步**迁移到 pathutil。

**短期共存**:cwd 包继续用 `filepath.*`,pathutil 独立存在。后续如果发现 cwd 包大量依赖 pathutil,再考虑把 cwd 包改为 thin wrapper。

---

## 5. 实施步骤 / 落地调用点

> **实施状态 (2026-08-21)**:全部三阶段已完成。

### 5.1 第一阶段:创建 pathutil 包 ✅

新增文件:

```
internal/pathutil/normalize.go       # NormalizeForOS / NormalizeForGit / Equal / IsUnder / Clean / Join / IsAbs / Base / Dir / NormalizeInput / NormalizeIMRichText
internal/pathutil/path_unix.go       //go:build !windows
internal/pathutil/path_windows.go    //go:build windows
internal/pathutil/path_unix_test.go  # 13 个测试
internal/pathutil/path_windows_test.go  # 24 个测试
```

测试结果:Windows 24/24 通过(关键回归 `TestNormalizeForOS_DriveRootedForward` 把 `F:/nightme.nightme/fix-windows-cli-style` 转成 `F:\nightme.nightme\fix-windows-cli-style`),`TestNormalizeForGit_ForcesBackslash` 保证喂给 git 的 argv 是纯反斜杠。

### 5.2 第二阶段:gtw 调用点切换 ✅

| 文件:行 | 当前 | 改后 |
|---------|------|------|
| `worktree.go:WorktreeRemove` | `args = append(args, path)` | `np, err := pathutil.NormalizeForGit(path); if err != nil { return WorktreeError }; path = np; args = append(args, path)` |
| `worktree.go:WorktreeAdd` | 同上 | 同上(同样有 NormalizeForGit 错误处理) |
| `worktree.go:RefreshDefaultBranch` | `filepath.Clean(filepath.Join(...))` | `pathutil.Clean(pathutil.Join(...))` |
| `worktree.go:PreflightWorktreeCreate` | `filepath.Dir` | `pathutil.Dir` |
| `worktree.go:PreflightWorktreeCreate` `EvalSymlinks` | **保留 `filepath.EvalSymlinks`** | 符号链接解析是 cwd/pathutil 的另一层语义,故意保留 |
| `persist.go:WriteGTWYml` | `doc.Worktree/RepoRoot` 原样序列化 | 写 yml 前 `NormalizeForOS` |
| `persist.go:ReadGTWYml` | 原样返回 Context | 读 yml 后同样 `NormalizeForOS`(防御手编 / 跨机器迁移的 yml) |
| `persist.go:gtwYmlPath` / `EnsureGitignore` / `MkdirAll` | `filepath.Join` / `filepath.Dir` | `pathutil.Join` / `pathutil.Dir` |
| `persist.go:ReadGTWYml` `IsAbs` 检查 | `filepath.IsAbs` | `pathutil.IsAbs` |
| `close.go:RunClose` step 0.5 stat | `statPath(selectedCwd)` | 先 `NormalizeForOS(selectedCwd)` 再 stat |
| `close.go:RunClose` step 0.5 slot 比对 | `filepath.Clean(cur.Worktree) == filepath.Clean(selectedCwd)` | `pathutil.Equal(cur.Worktree, selectedCwd)` |
| `close.go:assertWorktreeClean` | `filepath.Join(dir, "(worktree)")` | `pathutil.Join` |
| `fix.go:RunFix` existing-path 比较 (line 344 / 561) | `filepath.Clean(existingPath) == filepath.Clean(worktreePath)` | `pathutil.Equal(existingPath, worktreePath)` |
| `slug.go:WorktreePath` | `filepath.Clean` + `filepath.Dir` + `filepath.Base` + `filepath.Join` | `pathutil.NormalizeForOS` + `pathutil.Dir` + `pathutil.Base` + `pathutil.Join` |
| `preflight.go:preflightOrphanYml` | `filepath.Join` | `pathutil.Join` |
| `attachments.go::downloadAttachments` + `attachmentsDir` | `filepath.Base` / `filepath.Join` | `pathutil.Base` / `pathutil.Join` |
| `action.go::deriveRepoFromCwd` | `filepath.Dir` / `filepath.Base` | `pathutil.Dir` / `pathutil.Base` |
| `hooks.go::Load` 用户配置路径 | `filepath.Join(home, ".nightme", "gtw.yml")` | `pathutil.Join` |

### 5.3 第二阶段附加:`internal/command/cwd/` 迁移 ✅

cwd 包原本自己实现 `isWindowsDriveRel` / `htmlLinkTag` 正则 / IME 映射 / root-relative 驱动器补全,这些全部迁到 pathutil:

| 文件:行 | 当前 | 改后 |
|---------|------|------|
| `cwd/normalize.go::normalizePathInput` | 自有 IME 映射表 | `pathutil.NormalizeInput` 一行 delegate |
| `cwd/normalize.go::stripIMRichText` | 自有 `<a>` 标签正则 | 删除(pathutil `NormalizeIMRichText` 接管) |
| `cwd/normalize.go::htmlLinkTag` | 自有 regex 变量 | 删除 |
| `cwd/path_unix.go::resolvePath` | `filepath.IsAbs` + `filepath.Abs` + `filepath.Join` | `pathutil.NormalizeForOS` + `pathutil.IsAbs` + `pathutil.Join` |
| `cwd/path_windows.go::resolvePath` | 自有 drive-relative 检测 + 驱动器补全 + `filepath.Clean` + `filepath.Abs` | `pathutil.NormalizeForOS` 接管所有 shape 转换;只有 HOME 相对解析留在 cwd |
| `cwd/path_windows.go::isWindowsDriveRel` | 自有函数 | **删除**(pathutil 接管) |
| `cwd/cmd.go::expandTilde` | `filepath.Join(home, ...)` | `pathutil.Join` |
| `cwd/path_windows_test.go::TestIsWindowsDriveRel` | 复制粘贴的 table | **删除**(pathutil 的 `TestIsWindowsDriveRel` 覆盖同一张 table) |

迁移后 cwd 包生产代码 (cmd.go / path_unix.go / path_windows.go / normalize.go) **0 处 `path/filepath` import**,完全符合 SPEC §13.3.1 的"必须用 pathutil"铁律。

### 5.4 第三阶段:验证 ✅

```
go build ./...           ✅ 全项目编译通过
go vet ./...             ✅ 无任何 issue
go test ./internal/pathutil/          ✅ 24/24 Windows
go test ./internal/command/cwd/       ✅ 69/69 (含 10 subtest 的 IMRichText)
go test ./internal/command/gtw/       ✅ 302 PASS / 0 FAIL / 7 SKIP
```

其中 7 个 SKIP 是 Windows 上跳过的 POSIX chmod 测试(`TestPreflightWorktreeCreate_ParentUnwritable` 等),与本次改动无关。

### 5.5 测试覆盖缺口(下一轮建议补)

- gtw 层面 `TestRunClose_*` 没有显式覆盖 forward-slash 路径输入(目前依赖 pathutil 自身的 `TestNormalizeForOS_DriveRootedForward` 锁定行为)。如果以后想加端到端回归,建议写 `TestRunClose_WorktreeYmlWithForwardSlashes` —— 用 `WriteGTWYml` 写入一个 `worktree: "F:/foo/bar"` 的 yml,然后跑 `RunClose`,断言 `WorktreeRemove` 收到的 git argv 是 `["worktree", "remove", "F:\\foo\\bar"]`。这条测试需要 mock 掉 git CLI 才能纯 Go 测试;不阻塞本 spec 落地。

---

## 6. 测试矩阵

### 6.1 path_windows_test.go (必须)

```go
//go:build windows

// 验证 NormalizeForOS 的 Windows 形态
TestNormalizeForOS_DriveRootedBackslash   // "C:\foo" → "C:\foo"
TestNormalizeForOS_DriveRootedForward     // "C:/foo" → "C:\foo"  ← 关键回归测试
TestNormalizeForOS_RootRelative           // "/foo" → "<drv>:\foo"
TestNormalizeForOS_DriveRelative_Rejected // "C:foo" → error
TestNormalizeForOS_UNC                    // "\\?\F:\foo" → 原样
TestNormalizeForOS_Relative_Passthrough   // "foo" → "foo" (不静默拼 HOME)

// NormalizeForGit
TestNormalizeForGit_ForcesBackslash       // "F:/foo" → "F:\foo"

// Equal
TestEqual_CaseInsensitive                 // "C:\Foo" == "c:\foo"
TestEqual_SlashInsensitive                // "C:\Foo" == "C:/Foo"
TestEqual_TrailingSeparator               // "C:\foo" == "C:\foo\"

// IsUnder
TestIsUnder_SamePath                      // "C:\foo" Under "C:\foo" → true
TestIsUnder_ChildOf                       // "C:\foo\bar" Under "C:\foo" → true
TestIsUnder_ParentNotAncestor             // "C:\foo" Under "C:\bar" → false
TestIsUnder_DifferentDrive                // "D:\foo" Under "C:\foo" → false
```

### 6.2 path_unix_test.go

```go
//go:build !windows

TestNormalizeForOS_Passthrough            // "/foo" → "/foo", "foo" → "foo"
TestNormalizeForGit_Passthrough           // 同上
TestEqual_ByteExact                       // "/foo" == "/foo"
TestIsUnder_ChildOf                       // "/foo/bar" Under "/foo" → true
TestIsUnder_DotDotEscape                  // "/foo/../bar" Under "/foo" → false
```

### 6.3 gtw 集成测试(已存在,需在 Windows CI 通过)

- `internal/command/gtw/close_integration_test.go` 已覆盖 close 流程
- `internal/command/gtw/fix_remote_integration_test.go` 已覆盖 fix 流程
- 本 spec 的修复不需新增测试,但需要在 Windows runner 上跑通这些测试

---

## 7. 风险与回滚

### 7.1 风险

- **Risk 1**:`NormalizeForOS` 错误拒绝某些合法路径 → 用例覆盖(驱动器相对、UNC、相对路径)足够
- **Risk 2**:gtw 调用点切换后,Linux/macOS 上路径行为微妙改变(不应,因为 Unix 上是 passthrough)
- **Risk 3**:yml 读端做了 `NormalizeForOS`,而某个 caller 依赖"原样"形式(如显示给用户) → 路径规范化是幂等且无损的(只是分隔符转换),不应有 caller 依赖前斜杠形式

### 7.2 回滚策略

每阶段独立可回滚:
- 第一阶段(创建 pathutil)是纯新增,删除即可回滚
- 第二阶段(gtw 调用点切换)每个文件单独可改回,git history 清晰
- 第三阶段(验证)只决定是否合并,无状态变更

---

## 8. 与现有规范的关系

- **[`../SPEC.md`](../SPEC.md) §13(跨平台类库使用规范)**:本 spec 是该规范下的具体产物之一
- **[`../SPEC.md`](../SPEC.md) §1.4(抽象/具体 边界规范)**:pathutil 是"跨 OS 抽象"的范式
- **[`../WINDOWS.md`](../WINDOWS.md)**:本 spec 解决其中"gtw close on Windows fails"的具体问题
- **`internal/command/cwd/`**:是 pathutil 的"先驱",本包与之共存而非取代

---

## 9. 元信息

- **作者**: 🦞 虾哥 (PM/Architect) + Claude 协作
- **触发日期**: 2026-08-21
- **影响版本**: next minor (gtw 修复随版本)
- **状态**: implemented
- **相关 issue / PR**: 见 git log
