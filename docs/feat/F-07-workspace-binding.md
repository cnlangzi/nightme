# F-07: Workspace Binding

> **Status**: designed (v0.1)
> **Milestone**: M1 (used by M2)
> **Depends on**: F-01 (Session), F-04 (PTY)
> **Related docs**: SPEC.md §1.1 (Session Manager 包含 workspace), §3 (lifecycle)

## 1. Description

CLI 进程启动时 `cwd = session.workspace`。验证 workspace 路径存在 + 是目录 + 当前用户有可执行权限（确保 claude 能跑）。

## 2. Interface

```go
// internal/workspace/workspace.go
type Validator interface {
    Validate(path string) error
}

type PathResolver interface {
    // Resolve handles ~ expansion
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
```

## 3. Implementation

**文件**：
- `internal/workspace/workspace.go` — Validate + Resolve
- `internal/workspace/workspace_test.go` — 单测

**调用点**：
- F-01 SessionManager.Create 中，agent.Start() 之前
- 解析用户 `/cwd <path>` slash command 后立即调用

**为什么在 spawn 之前验证**：
- 避免 spawn 失败的副作用（PTY half-open 等）
- 给用户清晰的错误信息

## 4. Edge cases

| 场景 | 处理 |
|------|------|
| 路径不存在 | 返回 ErrNotExist → 用户提示 |
| 路径是文件不是目录 | 返回 ErrNotDirectory → 用户提示 |
| 路径无执行权限（755 但用户无 x）| 返回 ErrNoExecute → 用户提示 |
| 路径是 symlink | 跟随 symlink 验证 target（`os.Stat` 默认跟随）|
| 路径是相对路径 | Resolve 转为绝对路径 |
| 路径含 `~` | Resolve 展开为 `$HOME` |
| 路径含空格 / 中文 / emoji | 原样保留（PTY 启动不 care）|
| 路径在 remote filesystem（NFS）| Validate 可能慢，加 timeout（v0.1 不加，假设本地 fs）|

## 5. Test plan

**单元测试**：
- `Validate("/tmp")` → nil
- `Validate("/nonexistent")` → ErrNotExist
- `Validate("/etc/passwd")` → ErrNotDirectory
- `Resolve("~/code")` → `/home/devin/code`（mock UserHomeDir）
- `Resolve(".")` → `/cwd/absolute/path`

**集成测试**：
- Create session with invalid workspace → 返回 error，registry 不写入

## 6. Open questions

- 是否支持 workspace 是 git URL（如 `git@github.com:foo/bar` 自动 clone）？v0.1 不支持
- 是否在 session 跑的过程中检测 workspace 被删除？v0.1 不检测（F-06 cleanup 时才检查）
- 可执行权限检查在 macOS / Linux 上是否准确？`info.Mode().Perm()&0o700` 是 user bit，root 用户不需要检查
