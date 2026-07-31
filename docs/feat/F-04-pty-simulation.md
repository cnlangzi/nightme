# F-04: PTY Simulation (Bridge PTY Backend)

> **Status**: designed (v0.1)
> **Milestone**: M1 (used by M2)
> **Depends on**: (none — foundation)
> **Related docs**: SPEC.md §1.1 (Bridge 组件), [F-19-cli-bridge.md](./F-19-cli-bridge.md) (PTY byte pipe), [F-21-agent-modes.md](./F-21-agent-modes.md) (Bridge 三层模式), §4 (并发模型)

## 1. Description

在 pseudo-terminal (PTY) 中 spawn AI Coding CLI 进程，让 CLI 以为自己跑在真实终端里（颜色、进度条、交互 prompt 都正常）。nightme 通过 PTY master fd 与 CLI 通信。

## 2. Interface

```go
// internal/bridge/pty/pty.go
type Bridge interface {
    io.ReadWriteCloser
    PID() int
    Setsize(cols, rows int) error
}

func New(workspace string, command string, args []string, env []string, cols int, rows int) (Bridge, error)
```

**实现细节**（基于 `aymanbagabas/go-pty`）：

```go
import "github.com/aymanbagabas/go-pty"

type ptyBridge struct {
    ptmx pty.Pty   // master fd
    cmd  *exec.Cmd
}

func New(workspace, command string, args []string, env []string, cols, rows int) (Bridge, error) {
    cmd := exec.Command(command, args...)
    cmd.Dir = workspace
    cmd.Env = append(os.Environ(), env...)

    ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
        Rows: uint16(rows),
        Cols: uint16(cols),
    })
    if err != nil {
        return nil, err
    }
    return &ptyBridge{ptmx: ptmx, cmd: cmd}, nil
}

func (b *ptyBridge) Read(p []byte) (int, error)  { return b.ptmx.Read(p) }
func (b *ptyBridge) Write(p []byte) (int, error) { return b.ptmx.Write(p) }
func (b *ptyBridge) Close() error                 { return b.ptmx.Close() }
func (b *ptyBridge) PID() int                     { return b.cmd.Process.Pid }
func (b *ptyBridge) Setsize(cols, rows int) error {
    return b.ptmx.Setsize(&pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}
```

## 3. Implementation

**文件**：
- `internal/bridge/pty/pty.go` — Bridge 接口 + ptyBridge 实现
- `go.mod` — `github.com/aymanbagabas/go-pty`

**默认配置**（来自 `configs/nightme.example.yaml`）：
```yaml
session:
  default_pty_cols: 120
  default_pty_rows: 40
```

**为什么选 aymanbagabas/go-pty**：
- API 干净（一个 `Start` 一个 `Setsize`）
- macOS/Linux 跨平台处理已经做好
- resize 支持简单
- 与 creack/pty 接口差异小，备选切换成本 ~30 行

## 4. Edge cases

| 场景 | 处理 |
|------|------|
| command 不在 PATH | `pty.Start` 返回 error → 冒泡到 session manager |
| workspace 不存在 | `cmd.Start` 报错（在 aymanbagabas 内部处理）|
| command 启动后立刻 exit | `Read` 返回 EOF → session 立即进入 exited |
| 用户配置了超大的 cols/rows (e.g. 9999) | 限制上限（如 500x500），避免 PTY buffer 爆 |
| SIGWINCH resize (v0.2 F-13) | `Setsize()` 调用 |
| PTY 在 macOS 上的 fork+exec 问题 | aymanbagabas 已处理；如果遇到 fallback 到 creack/pty |
| cmd 已 Exit 但 ptmx 没 close | `Read` 返回 0 + nil error，跳过；`cmd.Wait()` 检测退出状态 |

## 5. Test plan

**单元测试**：
- `New(t.TempDir(), "/bin/echo", []string{"hello"}, ...)` → bridge.Read() 应返回 "hello\n"
- `Setsize(200, 50)` 不报错

**集成测试**：
- spawn `tty` 命令 → 验证 stdout 是 "not a tty" → 改为 PTY → 验证 stdout 是 "/dev/ttysXXX"
- spawn `stty size` → 验证 Setsize 生效

**手动测试**：
- spawn `/bin/zsh --interactive` → nightme 输入 `ls` → 看到颜色输出

## 6. Open questions

- 是否需要 PTY echo 关闭？倾向 v0.1 保持默认 echo on（Claude Code 不依赖）
- macOS ConPTY 是否要支持？v0.1 不支持（macOS 用 POSIX PTY）
- 是否需要记录 PTY 字节流到 log（debug 用）？v0.1 不记录，v0.2 加开关
