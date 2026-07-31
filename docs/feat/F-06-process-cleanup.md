# F-06: Process Cleanup

> **Status**: designed (v0.1)
> **Milestone**: M3 (Hardening)
> **Depends on**: F-05 (Registry)
> **Related docs**: `architecture.md` §6.3

## 1. Description

nightme 关闭时（SIGTERM/SIGINT/crash）决定如何处理自己启动的 CLI 进程。**默认策略 = 不 kill，标记 detached**，让 CLI 继续在后台跑；用户下次启动 nightme 时自动 reattach。可选 `--cleanup` 标志位强制 kill。

## 2. Interface

```go
// internal/cleanup/cleanup.go
type Policy int

const (
    PolicyDetach Policy = iota  // default: 标记 detached，不杀
    PolicyKill                  // --cleanup: SIGTERM → 5s → SIGKILL
)

type Cleaner interface {
    OnShutdown(policy Policy) error
    OnStartup() ([]Session, error)  // reattach 检测
}

// CLI flag
var cleanup = flag.Bool("cleanup", false, "kill all running nightme sessions on shutdown")
```

**行为矩阵**：

| 触发 | 默认 (detach) | --cleanup |
|------|---------------|-----------|
| SIGTERM | 标记所有 session 为 detached | kill 所有 running session |
| SIGINT (Ctrl+C) | 同上 | 同上 |
| SIGKILL | 子进程变孤儿，OS 兜底 | 同上 |
| 启动时 | 检查 detached session，PID 活着则 reattach | kill 所有 detached session（孤儿清理）|

## 3. Implementation

**文件**：
- `internal/cleanup/cleanup.go` — Cleaner 实现
- `cmd/nightme/main.go` — `--cleanup` flag + signal handler

**流程（SIGTERM）**：
```
main 收到 SIGTERM
  ↓
signal.Notify 触发 cleanup handler
  ↓
cleaner.OnShutdown(policy)
  ├─ policy = PolicyDetach (default):
  │   for each session in registry:
  │     ├─ PID 活着 → registry.MarkDetached(sid)
  │     └─ PID 死了 → registry.Delete(sid)
  ├─ policy = PolicyKill:
  │   for each session in registry:
  │     ├─ kill(PID, SIGTERM)
  │     ├─ 等待 5s 或 process.Wait()
  │     └─ 还活着 → kill(PID, SIGKILL)
  └─ registry flush 到磁盘
  ↓
main 退出
```

**流程（启动 reattach）**：
```
main 启动
  ↓
cleaner.OnStartup()
  ├─ 读 registry
  ├─ for each session where status="detached":
  │   ├─ 检查 PID (kill PID, 0) — 不真发信号，只检测
  │   ├─ 活着 → 重新创建 session 对象 + readPump/writePump（reattach）
  │   └─ 死了 → registry.Delete
  └─ 返回 reattached sessions 列表
```

## 4. Edge cases

| 场景 | 处理 |
|------|------|
| 子进程在 SIGTERM 后 5s 内不退 | 发 SIGKILL 兜底 |
| 子进程已被用户手动 kill | Wait() 立即返回 → skip kill |
| 子进程变 zombie | registry.Delete，OS 自动 reap |
| nightme crash（无 SIGTERM）| 子进程变孤儿；下次启动时 registry.MarkDetached 检查 PID 失效 → Delete |
| 用户 `kill -9 nightme` | SIGTERM handler 不触发，子进程孤儿；启动时通过 PID 检测清理 |
| 多个 nightme 实例同时跑（用户误操作）| v0.1 不防，假设只有一个 nightme 进程 |
| 子进程的 stdin 已经断开但还在跑 | bridge.Close() 应发 SIGHUP（待 aymanbagabas 是否支持）|

## 5. Test plan

**单元测试**：
- mock Registry → Cleaner.OnShutdown(PolicyDetach) → 验证不调 kill
- Cleaner.OnShutdown(PolicyKill) → mock process → 验证 SIGTERM 发送
- mock process 不响应 SIGTERM → 5s 后 SIGKILL

**集成测试**：
- 启动 nightme + 创建 session + 发送 SIGTERM → 验证 session CLI 仍跑 + registry 标记 detached
- 启动 nightme + --cleanup + 发送 SIGTERM → 验证 session CLI 被 kill

**手动测试**：
- 启动 session → kill -TERM nightme → ps aux | grep claude → 仍在跑
- 重启 nightme → 飞书 DM 收到 "reattached to {workspace}" 提示

## 6. Open questions

- 是否需要 "soft kill"（先发退出命令，再 SIGTERM）？v0.1 不做
- 默认 detach 是否会让用户困惑（"为什么 nightme 退了但进程还在跑"）？
  - 缓解：README 明确说明 + Channel 推送 "nightme detached, session continues"
  - v0.2 可加 `--kill-on-exit` flag 翻转默认
- 是否检测 "用户通过其他 channel kill 了 session"？v0.1 不做
