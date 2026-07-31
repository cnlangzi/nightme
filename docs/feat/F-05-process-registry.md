# F-05: Process Registry

> **Status**: designed (v0.1)
> **Milestone**: M1 (used by M2)
> **Depends on**: F-01 (Session)
> **Related docs**: SPEC.md §1.1 (Process Registry 组件), §3 (lifecycle)

## 1. Description

nightme 启动的所有 CLI 进程的元数据持久化到 JSON 文件，用于：
1. 重启后 reattach（F-15 部分功能）
2. `nightme list` 命令（F-10）查询
3. nightme 崩溃后知道哪些进程是自己的（F-6 清理）

## 2. Interface

```go
// internal/registry/registry.go
type Entry struct {
    SessionID  string    `json:"session_id"`
    ChatID     string    `json:"chat_id"`
    Workspace  string    `json:"workspace"`
    Agent      string    `json:"agent"`
    PID        int       `json:"pid"`
    PPID       int       `json:"ppid"`
    StartedAt  time.Time `json:"started_at"`
    Status     string    `json:"status"`  // "running" | "detached" | "exited"
    ExitCode   *int      `json:"exit_code,omitempty"`
}

type Registry interface {
    Upsert(entry Entry) error         // insert or update
    Get(sessionID string) (Entry, bool)
    Delete(sessionID string) error
    List() ([]Entry, error)
    MarkDetached(sessionID string) error
    MarkExited(sessionID string, code int) error
}
```

**文件位置**：`~/.local/share/nightme/registry.json`（可通过配置覆盖）
**文件权限**：`0600`
**持久化**：每次 Upsert/Delete 后立即 `fsync`

## 3. Implementation

**文件**：
- `internal/registry/registry.go` — Registry 接口 + JSON impl
- `internal/registry/registry_test.go` — 单测

**JSON schema**（v1）：
```json
{
  "version": 1,
  "sessions": {
    "s_01HF8XXXXX": {
      "session_id": "s_01HF8XXXXX",
      "chat_id": "oc_abc123",
      "workspace": "/home/devin/code/bailing",
      "agent": "claude",
      "pid": 12345,
      "ppid": 6789,
      "started_at": "2026-07-31T10:30:00+08:00",
      "status": "running"
    }
  }
}
```

**并发安全**：
- 全局 `sync.RWMutex` 保护内存 map
- 写操作：加 Lock → 改 map → 写文件 → Unlock
- 读操作：加 RLock → 读 map → Unlock（不读文件）

## 4. Edge cases

| 场景 | 处理 |
|------|------|
| 文件不存在 | 初始化为空 registry（version=1, sessions={}）|
| JSON 解析失败 | log error + 自动备份损坏文件为 `.bak` + 重置为空 |
| 文件权限 0600 被改成 0644 | 启动时检测 + warn（不强制修复）|
| 并发 Upsert 同一 session | mutex 串行化；冲突时后写覆盖前写 |
| registry 文件丢失 | 启动时检测 + warn，session 列表为空 |
| 磁盘满 | fsync 失败 → 返回 error → session 创建失败回滚 |
| 文件被外部修改 | 下次 Upsert 时覆盖（nightme 是 single owner）|
| Session.PID 在系统重启后失效 | `MarkDetached` → 启动时检查 PID 是否还活着（`os.FindProcess` + `signal 0`）|

## 5. Test plan

**单元测试**：
- Upsert + Get 一致性
- Delete 后 Get 返回 false
- List 排序（按 StartedAt）
- 并发 Upsert 无 race（`-race` flag）
- 文件损坏恢复（写入垃圾 → Load → 应 fallback 到空）

**集成测试**：
- Create session → 检查文件内容
- 重启 mock（重新 load Registry）→ session 列表一致
- 模拟文件损坏 → 自动备份 + 重置

## 6. Open questions

- 是否需要迁移到 SQLite？v0.1 不需要（< 100 sessions，JSON 够用）
- 是否记录 session 的 stdout 历史？v0.1 不记录（F-15 v0.2 再做）
- PID 在 macOS 上的 recycle 问题？v0.1 简化：session 创建时间 + agent 命令字符串联合判断
