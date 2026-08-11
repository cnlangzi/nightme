# F-06: Process Cleanup

> **Status**: implemented (v1.1 — detach/close 路径走 Manager.MarkDetached + Manager.Kill(binding.SessionID))
> **Milestone**: M3 (Hardening)
> **Depends on**: F-05 (Registry)
> **Related docs**: [`SPEC.md`](../SPEC.md) v1.1 §3.2; [`F-01-session-create.md`](./F-01-session-create.md); [`F-05-process-registry.md`](./F-05-process-registry.md); [`F-20-gateway.md`](./F-20-gateway.md) §4.3

---

## 1. Description

nightme 关闭时（SIGTERM/SIGINT/crash）决定如何处理自己启动的 CLI 进程。**默认策略 = 不 kill，标记 detached**，让 CLI 继续在后台跑；用户下次启动 nightme 时 binding + session 自动恢复。可选 `--cleanup` 标志位强制 kill。

**v1.1 路径变化**：

| 旧（v0.2）| 新（v1.1）|
|-----------|-----------|
| 关闭路径用 `session.ChatID` 索引 | 关闭路径用 `manager.List() → manager.MarkDetached(id)` / `manager.Kill(id)` |
| `cleanup.OnShutdown(policy)` 单独组件 | 直接在 `cmd/nightme/shutdownRun` 里实现（无独立 cleanup 包）|
| `--cleanup` flag 在 main.go 注册 | `--cleanup` flag 仍在 runCmd 注册 |

---

## 2. Interface

```go
// 在 session.Manager 上（v1.1 不变）
type Manager interface {
    // ... 其它 ...
    MarkDetached(sid string) error  // 释放 live handle，不杀进程
    Kill(sid string) error          // 终止进程，sess.Status → Exited
}

// CLI flag
var cleanup = flag.Bool("cleanup", false, "kill all running nightme sessions on shutdown")
```

**行为矩阵**：

| 触发 | 默认 (detach) | --cleanup |
|------|---------------|-----------|
| SIGTERM | 标记所有 sessions 为 detached | kill 所有 running sessions |
| SIGINT (Ctrl+C) | 同上 | 同上 |
| SIGKILL | 子进程变孤儿，OS 兜底 | 同上 |
| 启动时 | Restore sessions + bindings；StatusRunning/Detached 都映射成 StatusDetached | 同上；/run 时会 spawn 新 CLI（不试图 reattach 旧 PID）|

---

## 3. Implementation（v1.1）

**文件**：
- `cmd/nightme/run.go` — `runRun` + `shutdownRun`（不再有独立 `cleanup` 包）
- `internal/session/manager.go` — `MarkDetached` / `Kill` 实现

**流程（SIGTERM / SIGINT）**：
```
main 收到 SIGTERM (signal.Notify)
  ↓
runRun 退出 → defer shutdownRun()
  ↓
shutdownRun(ctx, ch, mgr, cleanup)
  ├ ch.Stop(ctx)                    // 关 Channel adapter
  ├ gw.Stop(ctx)                    // 关 gateway dispatch goroutines
  ├ gw.PersistBindings()            // flush binding 表到 registry
  ├ mgr.Persist()                   // flush session 表到 registry
  ├ if cleanup:
  │   └ for sess in mgr.List():
  │       └ if sess.Status == Running:
  │           └ mgr.Kill(sess.ID)   // SIGTERM → 5s → SIGKILL
  └ else:
      └ for sess in mgr.List():
          └ if sess.Status == Running:
              └ mgr.MarkDetached(sess.ID)   // 释放 handle，CLI 继续跑
  ↓
main 退出
```

**关键不变式（v1.1）**：
- shutdownRun 不知道 chat_id；只遍历 `manager.List()` 处理每个 session
- binding 由 Gateway 持久化（独立调用 `gw.PersistBindings()`）
- session 由 Manager 持久化（`mgr.Persist()`）
- 两次 registry 写都在 ch.Stop 之后，避免发消息时 registry 已经标 detached

**流程（启动 restore）**：
```
main 启动
  ↓
registry.Open(path) → 旧 schema 时 Migrate()
  ↓
agents := buildRegistry(cfg)
  ↓
mgr := session.NewMemoryManager(agents, reg, /* EventCallback */ gw.OnSessionEvent)
mgr.Restore(ctx)            // 从 registry.sessions 重建 in-memory 表
  ↓
gw := gateway.New(messageDispatcher)
gw.RestoreBindings(reg.ListBindings())  // 从 registry.bindings 重建 binding 表
  ↓
ch.Start(ctx)  // Feishu WS 连上
  ↓
... 处理消息 ...
```

**Status mapping on Restore**：
- registry.StatusRunning → MemoryManager.StatusDetached（PID 可能已死，下次 /run 重 spawn）
- registry.StatusDetached → MemoryManager.StatusDetached
- registry.StatusExited → MemoryManager.StatusExited

binding 重建：每个 `BindingEntry{ChatID, ChatType, SessionID, Workspace, Agent}` → `gw.bindings[ChatID] = BindingEntry{...}`。如 SessionEntry 不存在（脏数据），binding 保留但下次 `/cwd` 时覆盖。

---

## 4. Edge cases

| 场景 | 处理 |
|------|------|
| 子进程在 SIGTERM 后 5s 内不退 | bridge.Close() 发 SIGKILL 兜底 |
| 子进程已被用户手动 kill | Wait() 立即返回 → skip kill |
| 子进程变 zombie | registry.DeleteSession，OS 自动 reap |
| nightme crash（无 SIGTERM）| 子进程变孤儿；下次启动 Restore 标 Detached；/run 时 spawn 新 CLI |
| 用户 `kill -9 nightme` | SIGTERM handler 不触发，子进程孤儿；下次启动处理同上 |
| 多个 nightme 实例同时跑（用户误操作）| v1.1 不防，假设只有一个 nightme 进程 |
| 子进程的 stdin 已经断开但还在跑 | bridge.Close() 发 SIGHUP（待 aymanbagabas 是否支持）|
| Detach 时 binding 已不存在 | 不影响（shutdownRun 不依赖 binding）|
| Detach 时 session 已 Exited（race）| MarkDetached idempotent：sess.Status != Exited 才标 Detached |
| Restore 时 SessionEntry 缺失但 BindingEntry 在 | binding 保留；下次 /cwd 覆盖；log warn |
| Restore 时 BindingEntry 缺失但 SessionEntry 在 | session 留在 manager；binding 在下次 /cwd 时建立 |
| Restore 时两边都有但 SessionID 对不上 | 重建两表时按 ID 索引，不强一致；下次 /run 时 binding.SessionID 替换 |
| registry migration 失败（v0.2 → v1.1）| Open() 返回 error；nightme 启动失败 + 提示 |

---

## 5. Test plan

**单元测试**：
- `MemoryManager.MarkDetached` 释放 handle + 标 StatusDetached + upsert SessionEntry
- `MemoryManager.Kill` 终止 + 标 StatusExited + upsert SessionEntry
- `cmd/nightme/shutdownRun` mock Channel + manager → 验证 detach / kill 路径
- `registry.Migrate` v0.2 → v1.1 数据转换正确

**集成测试**：
- 启动 nightme + 创建 session + 发 SIGTERM → 验证 session CLI 仍跑 + registry 标 detached
- 启动 nightme + --cleanup + 发 SIGTERM → 验证 session CLI 被 kill
- 启动 nightme v0.2 写 registry → 升级到 v1.1 → 启动 → registry 自动 migrate

**手动测试**：
- 启动 session → `kill -TERM <nightme_pid>` → `ps aux | grep claude` → 仍在跑
- 重启 nightme → 飞书 DM 一切正常（binding + session 已恢复）
- 启动 session → `kill -9 <nightme_pid>` → 子进程孤儿 → 重启 nightme → /run → 新 CLI spawn

---

## 6. Open questions

- 是否需要 "soft kill"（先发退出命令，再 SIGTERM）？v1.1 不做
- 默认 detach 是否会让用户困惑？v1.1 决策：detach 是合理的（用户主动 /close 显式语义）
- 是否检测 "用户通过其他 channel kill 了 session"？v1.1 不做
- Restore 时是否检查 PID 还活着？v1.1 决策：**不检查**，下次 /run 一律 spawn 新 CLI 覆盖旧 PID。简化实现，避免 PID recycle 误判

---

## 7. Cross-references

- **Session Status 状态机**：见 [`F-01-session-create.md`](./F-01-session-create.md) §2
- **Registry schema + migration**：见 [`F-05-process-registry.md`](./F-05-process-registry.md) §3, §4
- **/close slash command**：见 [`F-20-gateway.md`](./F-20-gateway.md) §4.3
- **v1.1 完整架构**：见 [`F-26-gateway-hub.md`](./F-26-gateway-hub.md)

---

## 8. Change log

- **2026-08-02** — v1.1: shutdown 路径走 manager.MarkDetached / manager.Kill(binding.SessionID)，不再用 cleanup 包。Restore 拆为 sessions + bindings 两步。Restore 不检查 PID（下次 /run 重 spawn）。Doc 重写。
- **2026-07-31** — v0.1: 原始 cleanup 包设计。已被 v1.1 取代（cleanup 逻辑内联到 cmd/nightme/run.go）。