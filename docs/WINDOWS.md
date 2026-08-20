# Windows 对接规范与故障排查

> **目的**：nightme 的 bridge 层在 Windows 上有 4 个跨 CLI 的陷阱（env 格式、.cmd/.bat shim、argv quoting、信号语义），单独踩一次排查成本极高。本文把这些坑 + 修复 + 测试方法一次性沉淀下来。
> **关联文档**：
> - bridge 层协议细节 → [`bridge/`](./bridge/)
> - 单 CLI 实测约定 → [`bridge/CLAUDE.md`](./bridge/CLAUDE.md) §7（真机 vs mock）、§9（代码锚点）

## 0.0 跨平台经验规则（fix-stop 沉淀）

> 写跨平台 Go 代码时，**优先用跨平台兼容方案；不行则按平台分别实现**。

按经验重要度排：

| # | 规则 | 反例 | 正例 |
|---|------|------|------|
| 1 | **测试生产契约，别测子进程 stdout/stderr** | `assert stdout == "C:\\foo"`（MSYS 翻译后是 `/c/foo`） | 让 child 写 sentinel file，用 `os.Stat(sentinelPath)` 验证 cmd.Dir 是否生效（filesystem 操作不经过 MSYS libc） |
| 2 | **跨平台逻辑放 tagless 文件** | `cmd.Env = append(os.Environ(), "MSYS_NO_PATHCONV=1")` 放在 `exec.go`（所有平台编译） | `applyMSYSEnvNoPathConv` 在 `exec_windows.go`，no-op stub 在 `exec_unix.go` |
| 3 | **平台特定逻辑放 //go:build 文件** | `path_windows.go` 的 `isWindowsDriveRel` 提到 `path.go` 里加 `runtime.GOOS` 分支 | `path_unix.go`（//go:build !windows）只管 POSIX；`path_windows.go`（//go:build windows）只管 Win32 盘符、UNC、root-relative |
| 4 | **call site 不写 `runtime.GOOS`** | `if runtime.GOOS == "windows" { runMSYS() }` | 调用 `applyMSYSEnvNoPathConv(env)`（在 _windows.go / _unix.go 各有定义） |
| 5 | **path 验证用 `filepath.Clean` + 平台 separator** | `assert want == "/foo/.bar"` | `assert want == filepath.FromSlash("/foo/.bar")`（测试期望跟随平台） |
| 6 | **env vars 跨平台覆盖用 `os.Environ()` + append** | 自己造一份完整 env（漏 HOME / PATH） | `cmd.Env = append(os.Environ(), "KEY=VALUE")`（只追加需要的，不替换全部） |
| 7 | **MSYS env var 仅控制 argv 翻译，不控制 getcwd** | 设 `MSYS_NO_PATHCONV=1` 后断言 child 报告 exact path | 测 child 实际行为（写文件、读文件）——filesystem 不经过 MSYS libc |

## 0.1 当前 Windows 13 个测试失败的根因（不是 fix-stop 引入）

PR 186 引入 Windows CI runner 后，暴露了 13 个 pre-existing 平台问题。这些**与 fix-stop 无关**，已记在 `docs/branch/fix-stop-windows-followups.md`。按根因分类：

| # | 失败 | 根因 | 归类 |
|---|------|------|------|
| 1 | `TestSendBlocks_FileStillUsesFileURL` (opencode) | opencode 把 `C:\foo` 渲染成非 `file://` URL，跨平台差异 pre-existing | bridge 协议问题 |
| 2 | `TestDownloadInboxDir_CreatesPerSessionDir` (feishu) | `inbox dir mode = 0777, want 0700` — Windows 不支持 POSIX mode 位 | 平台权限 API |
| 3 | `TestFixRemote_HappyPath` (gtw) | `git worktree` 输出 MSYS 翻译路径（`/c/Users/...`） | 跨平台 git 行为 |
| 4 | `TestFormatResults_ShowsFailure` (gtw) | `cmd /c "echo oops 1>&2; exit 5"` 在 Windows 上 ExitCode 不为 5（sh 与 cmd 重定向语义差异） | dispatcher Windows shell |
| 5 | `TestRefreshDefaultBranch_RebaseConflict` (gtw) | `fatal: pathspec 'shared.txt' did not match any files` — git on Windows pathspec 行为 | 跨平台 git 行为 |
| 6 | `TestPreflightWorktreeCreate_ParentUnwritable` (gtw) | `os.Getenv("GOOS") == "windows"` 在 CI runner 上没正确返回 | 测试 guard 用错 API |
| 7 | `TestWindowsPipePingStatusStop` (daemoncontrol) | Windows named pipe 在 server 关闭前就报 "pipe has been ended" | Windows pipe 时序 |
| 8-12 | `TestLogger_*` (logging) 5 个 | (a) `mode = 666, want 0600` — Windows chmod no-op (b) `TempDir RemoveAll cleanup: file in use by another process` — Windows 文件锁延迟释放 | 跨平台文件权限 + cleanup 竞争 |
| 13 | `TestRenderANSI` (feishu/qrencode) | QR 渲染输出在 Windows console 宽度不同 | 跨平台终端宽度 |

**所有 13 个** = pre-existing 平台问题，**不在 fix-stop scope**。cmd.Dir 契约相关的 5 个测试（`TestRunCmd_DirBinding` 系列）已通过 **sentinel file 验证**修复（PR 186 内 commit `c2e3c2d`）。

---

## 0. 一句话速查

| 现象 | 根因 | 修复锚点 |
|------|------|---------|
| `fork/exec C:\WINDOWS\system32\cmd.exe: The parameter is incorrect.` | env 里有裸字符串（无 `KEY=VALUE` 格式），`CreateProcess` 拒绝 | 不要往 `cmd.Env` 追加裸字符串；详见 §2 |
| `fork/exec <path>.cmd: The parameter is incorrect.` | 直接用 `.cmd` 路径作为 `lpApplicationName` | 用 `proc.New`，自动包 `cmd.exe /d /c`；详见 §3 |
| `.exe` 装了但仍走 `cmd.exe /d /c` | LookPath 顺序把 `.cmd` 排在前面 | 当前行为可以接受；如要绕开，配置 `agent_cmd.exe` 显式路径；详见 §3.4 |
| Agent 启动后立刻退出 | `Setsid` 不可用 + 进程组信号被误用 | Windows 下 `agent.SignalProcessGroup` 已经退化为单 pid 信号；详见 §5 |
| child 的 `pwd` 报告 `/c/Users/...` 而不是 `C:\Users\...` | MSYS libc getcwd() 翻译（**`MSYS_NO_PATHCONV=1` 不影响 getcwd**） | 不要测 child stdout 验证 CWD；测 sentinel file（filesystem 操作不经过 MSYS libc） |

### §0.1.1 详细案例：cmd.Dir 契约测试为什么用 sentinel file 而不是 pwd

**问题**：`internal/command/gtw/exec_test.go` 之前用 `pwd` 验证 `runCmd` 设置的 `cmd.Dir`：

```go
stdout, _, _ := runCmd(ctx, real, "pwd")
assert stdout == real  // "C:\foo" expected
```

**Windows CI 上失败**：
```
runCmd: child CWD = "/c/Users/runneradmin/AppData/Local/Temp/TestRunCmd_DirBinding4047655615/001"
want = "C:\\Users\\runneradmin\\AppData\\Local\\Temp\\TestRunCmd_DirBinding4047655615\\001"
```

**根因**：`pwd` 是 MSYS-bash 工具，glibc 的 getcwd() 被 MSYS libc 拦截，**总是返回 MSYS 翻译后的路径**（`/c/...`）。这是 MSYS libc 层的硬编码行为，**`MSYS_NO_PATHCONV=1` 不影响**（该 env var 只控制 argv 翻译，不控制 getcwd）。

**真正修复**：测试**契约**而不是 child 的 cwd 报告。Child 在 cmd.Dir 写 sentinel file，test 在 EXACT path 用 `os.Stat` 验证文件存在：

```go
sentinelName := fmt.Sprintf("nightme-sentinel-%d", os.Getpid())
runCmd(ctx, real, "sh", "-c", "echo X > "+sentinelName)  // or cmd.exe on Windows
os.Stat(filepath.Join(real, sentinelName))  // verifies cmd.Dir was honored
```

`os.Stat` 直接走 kernel（不经过 MSYS libc）。如果 cmd.Dir 没被设到 `real`，sentinel 会写到别的目录，stat 会失败。

**关键点**：
- test **用 cmd.exe**（Windows-native）做写文件，不走 MSYS shell 层
- stat 验证用**真实 path**（不是 `filepath.FromSlash` 转换的）
- 契约：**"child 真的能在 cmd.Dir 操作"** — 这才是 nightme 的生产承诺

适用规则：#1（测试生产契约）+ #7（MSYS env 不影响 getcwd）。

---

## 1. 背景

nightme 是单进程 daemon，把 Claude Code / Codex / OpenCode / Pi 四种 CLI 桥接到 IM 通道。bridge 层（`internal/bridge/<name>/`）通过 spawn 子进程 + 接管 stdin/stdout/stderr/stderr pipe 驱动协议。

**Windows 的 4 个跨平台差异**会在这里集中暴露：

1. **`CreateProcess` 严格校验 `lpEnvironment`**——必须是 `KEY=VALUE` 数组，裸字符串直接拒绝。
2. **`.cmd` / `.bat` 不能作为 `lpApplicationName`**——必须显式包 `cmd.exe /d /c`。
3. **没有 `Setsid` 等价物**——Setsid 在 Linux/macOS 上用来脱离 controlling TTY，Windows 走 `STARTUPINFO` 句柄传 console handle，问题不一样。
4. **没有进程组 / `SIGINT` 到整个 pgroup**——Windows 只能 `Process.Signal(pid, sig)`，子进程的工具子进程会泄漏。

下面逐条展开。

---

## 2. ⚠️ Env 格式校验（最隐蔽的 bug）

### 2.1 现象

Windows 端启动 pi / claude 报：

```
[pi] Start cmd.Start failed err="fork/exec C:\\WINDOWS\\system32\\cmd.exe: The parameter is incorrect."
```

错误指向 `cmd.exe` 而非被启动的 `pi.cmd`——这是因为 **wrapper 已经触发**（把 pi.cmd 包给了 cmd.exe），但 cmd.exe 启动时被拒。

### 2.2 根因

bridge 在拼 env 时多写了一句：

```go
env := append([]string(nil), cfg.Env...)
env = append(env, s.command) // ← 把裸字符串 "pi" / "claude" 加进 env
```

Windows 的 `CreateProcess` 拿到 `lpEnvironment` 后，会逐条验证格式 `KEY=VALUE`。裸字符串 `"pi"` 不带 `=` → 返回 `ERROR_INVALID_PARAMETER (87)` → fork/exec 失败。

Unix 上的 `execve(2)` 没有这条校验，所以同样的代码在 Linux 跑没问题——这正是 PR #158 引入的 `.cmd` wrapping 在 Unix CI 上通过、移植到 Windows 才炸的根因。

### 2.3 修复

删掉裸追加。**只接受 `KEY=VALUE` 形式**：

```go
env := append([]string(nil), cfg.Env...)
// env 永远保持 []string{"KEY=VALUE", ...}
cmd.Env = append(os.Environ(), env...)
```

涉及位置（commit 修复）：

- `internal/bridge/claudecode/claudecode.go:165` — 删除 `env = append(env, s.command)`
- `internal/bridge/pi/agent.go:268` — 同上

codex / opencode 当时没有同款 bug，无需改动。

### 2.4 怎么测出来

> **永远在 Windows 上做 end-to-end 冒烟**，不要相信 Unix CI。

最小复现（已写在修复的 regression 思路里）：

```go
cmd := exec.CommandContext(ctx, `C:\WINDOWS\system32\cmd.exe`, "/d", "/c", `C:\path\pi.cmd`, "--mode", "rpc")
cmd.Dir = `D:\workspace`
cmd.Env = []string{"pi"} // ← 任何裸字符串都触发

stdin, _  := cmd.StdinPipe()
stdout, _ := cmd.StdoutPipe()
stderr, _ := cmd.StderrPipe()
err := cmd.Start() // ❌ The parameter is incorrect.
```

`os.Environ()` 默认是合规的——问题永远是 bridge 自己拼 env 时引入了裸项。

### 2.5 怎么防止回归

- bridge 包 `env` 构造时，**只接受 cfg.Env**（已通过配置加载，格式合规）。
- 不要为「防御性」往 env 加任何裸字符串——没有用，反而炸 Windows。
- 如果真要给子进程传 agent 名作为环境变量，用 `AGENT_COMMAND=pi` 形式（命名 + 显式 key），不要裸追加。

---

## 3. `.cmd` / `.bat` shim 启动矩阵

### 3.1 Windows 安装生态

nightme 支持的 4 个 CLI，在 Windows 上有以下分发形式（按 PATHEXT 顺序 `.COM;.EXE;.BAT;.CMD;.VBS;.JS;.WS;.MSC`）：

| CLI | 常见形式 | LookPath 解析结果 |
|-----|----------|-------------------|
| `claude` | npm 包内的 `@anthropic-ai/claude-code.cmd` + 真实 `.exe` | `claude.cmd`（先命中） |
| `codex` | npm 包内的 `codex.cmd` + `codex.exe` | `codex.cmd`（先命中） |
| `opencode` | npm 包内的 `opencode.cmd` + `opencode.exe`（实际就是 `node_modules\opencode-ai\bin\opencode.exe`） | `opencode.cmd`（先命中） |
| `pi` | pi-node 的 `pi.cmd` + `pi`（无扩展的 PE 二进制） | `pi.cmd`（先命中） |

**关键事实**：几乎所有分发都是「先放个 `.cmd` shim 在 PATH 顶层，shim 内部再去调 `.exe`」。所以 **LookPath 100% 返回 `.cmd` 路径**。

### 3.2 启动矩阵（`proc.New`）

`internal/proc/exec_windows.go` 的 `launchOnWindows` 是单一入口，按扩展名路由：

| 扩展名 | 启动方式 | 何时触发 |
|--------|----------|----------|
| `.exe` / `.com` / 无扩展 | `exec.CommandContext(resolved, args...)` 直调 | 用户显式配置绝对 `.exe` 路径时 |
| `.cmd` / `.bat` | `exec.CommandContext(cmd.exe, /d, /c, resolved, args...)` | 默认（npm shim） |
| `.ps1` | `exec.CommandContext(powershell.exe, -NoProfile -NonInteractive -ExecutionPolicy Bypass -File resolved, args...)` | 备用 |
| `.js` | `exec.CommandContext(node.exe, resolved, args...)` | 备用 |

**所有 4 个 bridge 都通过 `proc.New` 启动**——没有「各自实现一套」的地方。

```go
// claudecode.go / pi/agent.go / codex/session.go / opencode/server.go
cmd := proc.New(ctx, command, args...)  // ← 统一入口
cmd.Dir = cfg.Workspace
cmd.Env = append(os.Environ(), cfg.Env...)   // ← 注意 §2
```

### 3.3 为什么必须包 `cmd.exe /d /c`

`CreateProcess` 拒绝以 `.cmd` / `.bat` 路径作为 `lpApplicationName`，错误码 `ERROR_INVALID_PARAMETER (87)`。Microsoft 官方 workaround：

```
lpApplicationName = C:\Windows\System32\cmd.exe
lpCommandLine     = cmd /d /c "<resolved>" <args...>
```

- `/d` 跳过 AutoRun registry 命令，与 cmd.exe 交互模式一致
- `/c` 执行命令后退出

### 3.4 `.exe` 装了但仍走 wrapper

正常情况：`LookPath("pi")` → `C:\...\pi.cmd` → 命中 `.cmd` 分支 → cmd.exe 包一层 → cmd.exe 调 pi.cmd → pi.cmd 再调 `pi.exe`。两次间接，argv 已经正确传递，不会出错。

如果你想跳过 `.cmd` 这一层（理论上少一层 cmd.exe）：

- 在 `cmd/nightme/agents.go` 注册时改成绝对 `.exe` 路径：
  ```go
  agent.Builtins.Register(pi.NewStarter("pi", `C:\path\pi.exe`, nil))
  ```
- 或在用户配置里覆盖 `command` 字段。

**不要**在 bridge 包里做 LookPath 重定向——那是 `proc.New` 的活。

---

## 4. 测试方法

### 4.1 真机 / Mock 分层

| 类型 | 何时用 | PATH 守卫 |
|------|--------|-----------|
| **真机**（spawn 真实 cli） | stream-json、resume、多轮 stdin、env 行为 | **必须** `requireReal<Name>(t)` |
| **Mock**（`testdata/*.sh` / `.py`） | argv / 解析 / 协议单测 | **不要** skip — CI 必跑 |

参考 [`bridge/CLAUDE.md §7.1`](./bridge/CLAUDE.md) 的真机守卫 pattern。

### 4.2 Windows 上必须覆盖的真机场景

`go test` 在 Unix 上跑通 ≠ Windows 上没问题。**至少** 跑一次下列矩阵：

| 场景 | 验证目标 | 命令 |
|------|---------|------|
| pi `Start` 成功 + handshake 成功 | .cmd 包 + env 合规 | 见 §4.3 |
| claude `Start` 成功 | 同上 | 同上 |
| codex `Start` 成功 + initialize + thread/start | 同上 | 同上 |
| opencode `Start` 成功 + 解析 banner | 同上 + banner regex 兼容 | 同上 |
| 三 pipe（stdin/stdout/stderr）+ cmd 启动 | §2.4 的最小复现 | 见 §4.4 |
| `ComSpec` 自定义 | fallback 不写死 `C:\Windows\...` | 单测 |

### 4.3 一次性验证全部 4 个 agent

```go
// cmd/repro_test/main.go（修复期临时建；CI 不入仓）
package main

import (
    "context"
    "fmt"
    "os"
    "time"

    "github.com/cnlangzi/nightme/internal/agent"
    "github.com/cnlangzi/nightme/internal/bridge/claudecode"
    "github.com/cnlangzi/nightme/internal/bridge/codex"
    "github.com/cnlangzi/nightme/internal/bridge/opencode"
    "github.com/cnlangzi/nightme/internal/bridge/pi"
)

func main() {
    os.Setenv("PATH", `C:\Users\<you>\AppData\Local\pi-node\current;`+os.Getenv("PATH"))
    os.Setenv("NIGHTME_PI_DEBUG", "0")
    os.Setenv("NIGHTME_CODEX_DEBUG", "0")
    os.Setenv("NIGHTME_OPENCODE_DEBUG", "0")
    os.Setenv("NIGHTME_OPENCODE_HOME", `D:\andy`)

    agents := []struct {
        name    string
        starter interface {
            Start(ctx context.Context, cfg agent.StartConfig) (*agent.Agent, error)
        }
    }{
        {"pi",       pi.NewStarter("pi", "pi", nil)},
        {"claude",   claudecode.NewStarter("claude", "claude", nil)},
        {"codex",    codex.NewStarter("codex", "codex", nil)},
        {"opencode", opencode.NewStarter("opencode", "opencode", nil)},
    }
    for _, a := range agents {
        ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
        ag, err := a.starter.Start(ctx, agent.StartConfig{Workspace: `D:\andy`})
        cancel()
        if err != nil {
            fmt.Printf("%s Start error: %v\n", a.name, err)
            continue
        }
        fmt.Printf("%s Start ok, pid=%d\n", a.name, ag.PID())
        _ = ag.Close()
    }
}
```

期望：4 个 `Start ok, pid=...`，没有 `error`。Handshake / banner / 后续事件再单独断言。

### 4.4 最小 env 复现

任何 Windows 上「fork/exec 失败」的修复都要先跑通这段（**修复后**应 ok）：

```go
cmd := exec.CommandContext(ctx, `C:\WINDOWS\system32\cmd.exe`,
    "/d", "/c", `C:\path\pi.cmd`, "--mode", "rpc")
cmd.Env = []string{"FOO=BAR"} // 唯一一行 KEY=VALUE
cmd.StdinPipe(); cmd.StdoutPipe(); cmd.StderrPipe()
err := cmd.Start() // ✅ nil
```

如果 env 多写一行裸字符串（如 `"pi"`） → `err: The parameter is incorrect.`，对照实验成立。

### 4.5 CI 覆盖

`internal/proc/exec_windows_test.go` 已有 5 个回归测试（`TestNew_*`），任何 CI runner 上跑 `go test ./internal/proc/...` 都会覆盖 launch matrix。**不要**靠 Unix CI 替代 Windows 真机测。

---

## 5. 信号 / 进程组差异

### 5.1 `agent.SignalProcessGroup` 行为

| 平台 | 行为 |
|------|------|
| Unix | `syscall.SysProcAttr{Setsid: true}` 让子进程成为 pgroup leader；`kill(-pgid, SIGINT)` 广播到整个子树 |
| Windows | 没有 pgroup 等价物 → `p.Signal(os.Interrupt)` 退化为单 pid 信号 |

→ claude / pi 里 `Bash` 工具子进程在 Windows 上可能泄漏。

### 5.2 影响范围

- **功能**：不影响单次 prompt 完成（CLI 自己收到 SIGINT 退出）
- **资源**：Bash 子进程可能滞留 → 重启 daemon 时 `nightme kill --all` 应清理（详见 §6）
- **修复**：需要 Windows Job Object 跟踪子进程 → 未来 feature；当前已知 limitation

### 5.3 当前 workaround

- `Close()` 调 `Process.Kill()` 兜底：5s 内不退出就 SIGKILL（claudecode / pi 的 `shutdownGrace`）
- 退出时主动 `cmd.Wait()`，避免僵尸进程
- **不要**在 Windows 上依赖「Ctrl-C 传到子进程的工具子进程」

---

## 6. 调试 checklist（Windows）

> 复制 [`bridge/CLAUDE.md §8`](./bridge/CLAUDE.md) 之后追加。

按顺序排除：

1. **错误信息是「The parameter is incorrect」吗？**
   - 是 → 检查 `cmd.Env` 有没有裸字符串（§2）
   - 不是 → 转 §6.2
2. **错误信息是「not a valid Win32 application」？**
   - spawn 了一个非 PE 文件（如 `.sh` / `.js` 没包解释器）
   - 检查是否走的是测试 mock（`testdata/*.sh`）；真机用 `cmd/nightme` 跑
3. **错误信息是「executable file not found」或 PATH 找不到？**
   - `where <cli>` 验证 PATH
   - `os.Getenv("PATH")` 是否包含 `C:\Users\<you>\AppData\Local\pi-node\current`
   - 注意：用户用 git-bash 启动 nightme 时，PATH 是 git-bash 风格（`/c/...`），不是 Windows 风格——nightme 是 PE 二进制，直接看 `%PATH%`
4. **进程 spawn 成功但立刻退出？**
   - 看 stderr 是否有「permission denied」 / 「api key」 / 「model not configured」
   - 检查 `~/.claude/settings.json` / `~/.codex/config.toml` 等用户配置是否完整
5. **spawn 成功但 events / pipe 没有数据？**
   - 看 stdout / stderr 是不是空——可能是 CLI 在等 stdin（PTY vs JSON mode 不匹配）
   - 确认 `DefaultArgs` 里有 `--print` / `--mode rpc` 等「非交互」开关
6. **进程已退出但 nightme 不感知？**
   - 看 readpump / lifecycle goroutine 是否在跑
   - `cmd.Process.Wait()` 是否被调用（不调用 → `cmd.Wait` 永不返回）

---

## 7. 代码锚点（改 Windows 行为前先读）

| 主题 | 位置 |
|------|------|
| 启动矩阵（路由） | `internal/proc/exec_windows.go` → `launchOnWindows` |
| 启动矩阵（单元测试） | `internal/proc/exec_windows_test.go` → `TestNew_*` |
| Unix 端等价（Setsid） | `internal/proc/exec_unix.go` → `proc.New` |
| 信号 / pgroup 退路 | `internal/proc/exec_windows.go`（`hideWindow` / `applyHideWindow`） |
| pi bridge spawn 调用 | `internal/bridge/pi/agent.go` → `newDriver` |
| claude bridge spawn 调用 | `internal/bridge/claudecode/claudecode.go` → `newDriver` |
| codex bridge spawn 调用 | `internal/bridge/codex/session.go` → `newSession` |
| opencode bridge spawn 调用 | `internal/bridge/opencode/server.go` → `startServer` |
| Bridge 注册表 | `cmd/nightme/agents.go` |
| REPL console 门控（VT 检测） | `cmd/nightme/repl_console_windows.go` → `readlineUsable`；stub 在 `repl_console_unix.go` |
| REPL 三路分发 | `cmd/nightme/repl.go` → `runREPL`（tty+VT → `runREPLInteractive` / tty 无 VT → `runREPLScanner` / non-tty → `runREPLWith`） |
| REPL 版本检查共享 helper | `cmd/nightme/repl.go` → `runStartupUpdateCheck` + `scanRePLLoop` |

---

## 8. 已知 limitation / 未做

- **Bash 工具子进程泄漏**：Windows 上 SIGINT 不到 pgroup 下的 `Bash` 子进程；需要 Job Object 才能完整清理。
- **`.ps1` 桥**：当前实现走 `powershell.exe -File`，未实测；`Bypass -ExecutionPolicy` 在受限机器可能被组策略覆盖。
- **`.js` 直调**：当前实现走 `node.exe`，但 pi-node / npm shim 路径上几乎没有用户只装 `.js` 没装 `.cmd` 的场景——这是 defensive，未来按需启用。
- **`%ComSpec` 重定向**：用户自定义 `ComSpec` 时 `comspecOrDefault` 会尊重；测试覆盖。
- **`PATHEXT` 顺序**：当前 `.cmd` 永远赢过 `.exe`，因为 npm shim 把 `.cmd` 放在 PATH 顶层；如果用户只有 `.exe` 没 `.cmd`，自动命中 `.exe` 分支（无 wrapper），无需手工配置。
- **cmd.exe 行编辑 / 历史缺失**：经典 cmd.exe（输出 console 无 `ENABLE_VIRTUAL_TERMINAL_PROCESSING`）上 `reeflective/readline` 不可用——VT 关则 CSI 乱码（`nightme> [1 q[?25l[120D...`），VT 开则库启动时挂起（“Plan C” 回归，commit `6d29c03`）。`runREPL` 在此主机回退到 `runREPLScanner`（scanner + 版本更新 prompt），但**无行内编辑 / ↑/↓ 历史**。Windows Terminal / ConPTY 不受影响（走 `runREPLInteractive`，完整 readline）。恢复 cmd.exe 行编辑需 Win32-native 编辑器（`ReadConsoleInputW` + `SetConsoleCursorPosition`，不走 ANSI）——列为 follow-up。

---

## 9. 变更记录

- 2026-08-21：REPL Windows console 门控 + 版本更新 prompt 回到 cmd.exe
  - `runREPL` 改三路分发：tty+VT → `runREPLInteractive`(readline)；tty 无 VT（经典 cmd.exe）→ 新 `runREPLScanner`；non-tty → `runREPLWith`
  - 新增 `readlineUsable()`（`repl_console_windows.go` / `_unix.go`）：检测 stdout 的 `ENABLE_VIRTUAL_TERMINAL_PROCESSING`，**绝不 `SetConsoleMode` 开启**（开启会让 reeflective 启动挂起——Plan C 回归 `6d29c03`；关闭则 CSI 乱码）
  - 版本更新 prompt 抽成共享 helper `runStartupUpdateCheck`，`runREPLInteractive` 与 `runREPLScanner` 都跑；`runREPLWith`（测试 / non-tty）仍跳过以保 transcript 干净。scanner 循环抽成 `scanRePLLoop` 共享
  - cmd.exe 上行编辑 / ↑/↓ 历史仍缺（reeflective 在该 host 不可用），需 Win32-native 编辑器 follow-up
  - `paint()` 保持 identity（Windows `styleEnabled` 恒 false）

- 2026-08-15：fix-stop 沉淀跨平台经验规则
  - 新增 §0.0 七条跨平台经验规则（按重要度排序）
  - 新增 §0.1 当前 Windows 13 个测试失败的根因分类（pre-existing，不在 fix-stop scope）
  - 新增 §0.1.1 cmd.Dir 契约测试详细案例（为什么用 sentinel file 而不是 pwd）
  - §0 一句话速查新增第 5 行（MSYS getcwd 翻译）
  - 包含 fix-stop 的 cmd.Dir / MSYS / platform-split 修复

- 2026-08-13：建立本文档
  - §2 env 格式校验 bug（claudecode / pi 已修）
  - §3 启动矩阵统一进 `proc.New`
  - §4 真机测试方法 + 一次性 4-agent 复现脚本
  - §5-6 Windows 专属信号 / 调试 checklist
  - §7-8 代码锚点 + 已知 limitation
