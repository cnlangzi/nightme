# F-07: Workspace Binding

> **Status**: implemented (v1.1 — Validate 调用方改为 Gateway)
> **Milestone**: M1 (used by M2)
> **Depends on**: F-01 (Session), F-04 (PTY)
> **Related docs**: [`SPEC.md`](../SPEC.md) v1.1 §1.1, §4; [`F-20-gateway.md`](./F-20-gateway.md) §4.1; [`F-01-session-create.md`](./F-01-session-create.md)

---

## 1. Description

CLI 进程启动时 `cwd = session.Workspace`。验证 workspace 路径存在 + 是目录 + 当前用户有可执行权限（确保 agent CLI 能跑）。

**v1.1 调用方变化**：

| 旧（v0.2）| 新（v1.1）|
|-----------|-----------|
| `session.Manager.CreateOrUpdate(chatID, ..., abs, ...)` 内部调 Validate | `gateway.handler.cwd` 内部调 Validate → `manager.Create(abs, ...)` |
| workspace.Validate 是 session 包的内部细节 | workspace.Validate 是 gateway.handler 调用的工具函数（仍是 `internal/workspace/` 包）|

Workspace 验证本身**没变**——还是 Resolve（`~` 展开 + 绝对路径）+ Validate（存在 + 目录 + 可执行）。变的只是"谁调用"。

---

## 2. Interface

```go
// internal/workspace/workspace.go

type Validator interface {
    Validate(path string) error
}

type PathResolver interface {
    Resolve(path string) (string, error)
}

// Validate returns nil if path is usable, error otherwise
func Validate(path string) error {
    info, err := os.Stat(path)
    if err != nil { return err }
    if !info.IsDir() { return ErrNotDirectory }
    // 检查可执行权限（用于后续 spawn 子进程）
    if info.Mode().Perm()&0o700 == 0 { return ErrNoExecute }
    return nil
}

func Resolve(path string) (string, error) {
    if strings.HasPrefix(path, "~") {
        home, _ := os.UserHomeDir()
        path = filepath.Join(home, path[1:])
    }
    return filepath.Abs(path)
}

var (
    ErrNotExist    = errors.New("workspace: not exist")
    ErrNotDirectory = errors.New("workspace: not a directory")
    ErrNoExecute   = errors.New("workspace: no execute permission")
)
```

---

## 3. Implementation

**文件**：
- `internal/workspace/workspace.go` — Validate + Resolve
- `internal/workspace/workspace_test.go` — 单测

**调用点（v1.1）**：
- `internal/gateway/cmd/handlers.go` 的 `handler.cwd` —— 在 `manager.Create` 之前调 `workspace.Validate(abs)`
- 不在 session 包内调（v1.1 强制隔离：session 不知道 workspace.Validate 的存在）

**为什么在 spawn 之前验证**：
- 避免 spawn 失败的副作用（PTY half-open 等）
- 给用户清晰的错误信息（path 不存在 / 不是目录 / 不可执行）

---

## 4. v1.1 调用顺序（`/cwd` handler）

```go
// internal/gateway/cmd/handlers.go
func (h *handlerContext) cwd(ctx context.Context, msg *gateway.InboundMessage, args []string) (*gateway.CommandResult, error) {
    if len(args) == 0 {
        existing, err := h.manager.Get(h.gateway.LookupByChat(msg.ChatID).SessionID)
        // ... 显示当前 workspace
    }

    path := args[0]
    if strings.HasPrefix(path, "~") {
        home, _ := os.UserHomeDir()
        path = filepath.Join(home, strings.TrimPrefix(path, "~"))
    } else if !filepath.IsAbs(path) {
        home, _ := os.UserHomeDir()
        path = filepath.Join(home, path)
    }
    abs, err := filepath.Abs(path)
    if err != nil { return reply(err) }
    info, err := os.Stat(abs)
    if err != nil { return reply(err) }
    if !info.IsDir() { return reply("not a directory") }

    // workspace.Validate 已通过（手写展开 + Stat）
    // 决定 agentName → manager.Create → gateway.Bind
    // ...
}
```

注：实际实现里，handlers.go 仍手写 `~` 展开 + `filepath.Abs` + `os.Stat` 而不是调 `workspace.Resolve` + `workspace.Validate`。这两套路径等价；v1.1 可以选择保留手写或迁到 workspace 包调用。两者都满足"session 不 import workspace 包"的约束。

---

## 5. Edge cases

| 场景 | 处理 |
|------|------|
| 路径不存在 | 返回 ErrNotExist → handler Reply "workspace not found: <path>" |
| 路径是文件不是目录 | 返回 ErrNotDirectory → handler Reply |
| 路径无执行权限（755 但用户无 x）| 返回 ErrNoExecute → handler Reply |
| 路径是 symlink | 跟随 symlink 验证 target（`os.Stat` 默认跟随）|
| 路径是相对路径 | Resolve 转为绝对路径（v1.1：用 `filepath.Join(home, path)` 兜底）|
| 路径含 `~` | Resolve 展开为 `$HOME` |
| 路径含空格 / 中文 / emoji | 原样保留（PTY 启动不 care）|
| 路径在 remote filesystem（NFS）| Validate 可能慢，加 timeout（v1.1 不加，假设本地 fs）|
| 用户取消（chat 走了 /kill）| 不影响验证（验证是无状态的）|

---

## 6. Test plan

**单元测试**：
- `workspace.Validate("/tmp")` → nil
- `workspace.Validate("/nonexistent")` → ErrNotExist
- `workspace.Validate("/etc/passwd")` → ErrNotDirectory
- `workspace.Resolve("~/code")` → `/home/devin/code`（mock UserHomeDir）
- `workspace.Resolve(".")` → `/cwd/absolute/path`

**集成测试**：
- `handler.cwd` 收到 `/cwd /nonexistent` → Reply error，不创建 binding
- `handler.cwd` 收到 `/cwd /etc/passwd` → Reply "not a directory"
- `handler.cwd` 收到 `/cwd /tmp/foo` → binding 创建 + Session spawn + registry 两表写入

---

## 7. Open questions

- 是否支持 workspace 是 git URL（如 `git@github.com:foo/bar` 自动 clone）？v1.1 不支持
- 是否在 session 跑的过程中检测 workspace 被删除？v1.1 不检测（F-06 cleanup 时才检查）
- 可执行权限检查在 macOS / Linux 上是否准确？`info.Mode().Perm()&0o700` 是 user bit，root 用户不需要检查
- workspace.Validate 是否应该返回 typed error 而不是 wrapped error？v1.1 当前用 fmt.Errorf("%w")，调用方 errors.Is 检查

---

## 8. Cross-references

- **/cwd handler 完整流程**：见 [`F-20-gateway.md`](./F-20-gateway.md) §4.1
- **Session 数据模型**：见 [`F-01-session-create.md`](./F-01-session-create.md) §2
- **完整 v1.1 架构**：见 [`F-26-gateway-hub.md`](./F-26-gateway-hub.md)

---

## 9. Change log

- **2026-08-02** — v1.1: workspace 验证调用方从 session.Manager.CreateOrUpdate 改为 gateway.handler.cwd。session 包不再 import workspace。Doc 重写。
- **2026-07-31** — v0.1: 原始 workspace 包设计。已被 v1.1 取代（接口未变，调用点变了）。