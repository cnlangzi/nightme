# F-05: Process Registry (Two-Table Persistence)

> **Status**: implemented (v1.1 — 两张表：SessionEntry + BindingEntry)
> **Milestone**: M1 (registry), v0.3 (binding table split)
> **Depends on**: F-01 (Session)
> **Related docs**: [`SPEC.md`](../SPEC.md) v1.1 §6; [`F-26-gateway-hub.md`](./F-26-gateway-hub.md) §6 commit 5; [`F-01-session-create.md`](./F-01-session-create.md) §3

---

## 1. Description

nightme 把所有 runtime state 持久化到 JSON 文件，用于：
1. 重启后恢复 binding + session（F-01 lifecycle）
2. `nightme list` 命令（F-10）查询
3. nightme 崩溃后知道哪些进程是自己的（F-06 cleanup）

**v1.1 核心变化**：registry 由**两张表**组成——`SessionEntry`（session 状态 + workspace + agent + PID）与 `BindingEntry`（chat_id ↔ session_id + chat_type）。v0.2.x 的单表（SessionEntry 带 ChatID）已废弃，registry 自动迁移。

---

## 2. Interface

```go
// internal/registry/registry.go

type Status string
const (
    StatusRunning  Status = "running"
    StatusDetached Status = "detached"
    StatusExited   Status = "exited"
)

// SessionEntry 是 session 状态持久化记录。v1.1 不含 ChatID。
type SessionEntry struct {
    SessionID  string    `json:"session_id"`
    Workspace  string    `json:"workspace"`
    Agent      string    `json:"agent"`
    Args       []string  `json:"args"`
    PID        int       `json:"pid"`
    StartedAt  time.Time `json:"started_at"`
    LastRunAt  time.Time `json:"last_run_at"`
    Status     Status    `json:"status"`
    ExitCode   *int      `json:"exit_code,omitempty"`
}

// BindingEntry 是 Gateway 维护的 chat ↔ session 映射。v1.1 新增。
type BindingEntry struct {
    ChatID    string `json:"chat_id"`
    ChatType  string `json:"chat_type"`
    SessionID string `json:"session_id"`
    Workspace string `json:"workspace"`  // denormalized for /cwd reply
    Agent     string `json:"agent"`      // denormalized for /run reply
}

// File 是 registry 的 JSON 持久化实现。
type File struct {
    mu       sync.RWMutex
    path     string
    sessions map[string]SessionEntry  // SessionID → entry
    bindings map[string]BindingEntry  // ChatID → entry
}

func Open(path string) (*File, error)
func (f *File) Close() error

// SessionEntry ops:
func (f *File) UpsertSession(e SessionEntry) error
func (f *File) GetSession(sid string) (SessionEntry, bool)
func (f *File) DeleteSession(sid string) error
func (f *File) ListSessions() []SessionEntry

// BindingEntry ops:
func (f *File) UpsertBinding(e BindingEntry) error
func (f *File) GetBinding(chatID string) (BindingEntry, bool)
func (f *File) DeleteBinding(chatID string) error
func (f *File) ListBindings() []BindingEntry

// Migration:
func (f *File) Migrate() error  // 旧 schema → 新 schema
```

**文件位置**：`~/.nightme/registry.json`（可通过配置覆盖）
**文件权限**：`0600`
**持久化**：每次 Upsert 后立即 `fsync`

---

## 3. JSON schema (v1.1, version 3)

```json
{
  "version": 3,
  "sessions": {
    "s_01HF8XXXXX": {
      "session_id": "s_01HF8XXXXX",
      "workspace": "/home/devin/code/bailing",
      "agent": "claude",
      "args": ["--model", "opus"],
      "pid": 12345,
      "started_at": "2026-07-31T10:30:00+08:00",
      "last_run_at": "2026-07-31T11:00:00+08:00",
      "status": "running"
    }
  },
  "bindings": {
    "oc_abc123": {
      "chat_id": "oc_abc123",
      "chat_type": "p2p",
      "session_id": "s_01HF8XXXXX",
      "workspace": "/home/devin/code/bailing",
      "agent": "claude"
    }
  }
}
```

**关键变化 vs v0.2.x**：
- 顶层 `version: 3`（v0.2.x 是 2）
- 新增 `bindings` 顶层 key
- `SessionEntry` 移除 `chat_id` 字段（不再属于 session 域）

---

## 4. Schema migration (v0.2.x → v1.1)

**触发条件**：`Open()` 读 registry.json 时检测 `version != 3`。

**v0.2.x schema (version 2)**：
```json
{
  "version": 2,
  "sessions": {
    "s_01HF8XXXXX": {
      "session_id": "s_01HF8XXXXX",
      "chat_id": "oc_abc123",
      "workspace": "/home/devin/code/bailing",
      "agent": "claude",
      "pid": 12345,
      "started_at": "...",
      "status": "running"
    }
  }
}
```

**Migrate() 步骤**：
1. 备份 `registry.json` → `registry.json.v2.bak`
2. 遍历 `sessions[*]`：
   - 提取 `chat_id` 字段
   - 创建 `BindingEntry{ChatID: chat_id, ChatType: "" /* unknown → "group" 安全侧 */, SessionID: session_id, Workspace, Agent}`
   - 写入 `bindings` 表
   - 从 `SessionEntry` 删除 `chat_id` 字段
3. 顶层 `version: 2` → `3`
4. 写回文件 + fsync

**迁移后 invariant**：
- 所有 v0.2.x sessions 都有对应的 binding（即使 ChatType 未知，按 group 处理）
- 旧 PID 字段保留（`MarkDetached` 后的 PID 检查仍可用）
- 如果同一个 chat_id 在 v0.2.x 有多个 sessions（不该发生，但防御）：保留最新启动的 binding，其他 binding.ChatID 不重复

**降级**（v1.1 binary 写出的 registry 被 v0.2.x binary 读）：v0.2.x 不识别 `bindings` key，会忽略；能继续工作但 binding 信息丢失。建议 v1.1 binary 启动时检测到 v0.2.x 时拒绝启动并提示升级。

---

## 5. Implementation

**文件**：
- `internal/registry/registry.go` — File struct + 两表 + Migrate
- `internal/registry/registry_test.go` — 单测 + migration 测试
- `internal/registry/migrate.go` — version 2 → version 3 转换

**持久化流程**：
```
session.Create / session.Kill → MemoryManager.upsertEntry → registry.UpsertSession
gateway.Bind / Rebind → registry.UpsertBinding
nightme shutdownRun → manager.Persist + registry.Persist (一次 fsync 两表)
nightme 启动 → registry.Open → Migrate if needed → MemoryManager.Restore + Gateway.RestoreBindings
```

**并发安全**：
- 全局 `sync.RWMutex` 保护两个 map
- 写操作：加 Lock → 改 map → 写文件 → Unlock
- 读操作：加 RLock → 读 map → Unlock（不读文件）
- UpsertSession 和 UpsertBinding **分别独立** lock（避免锁整个 registry）

---

## 6. Edge cases

| 场景 | 处理 |
|------|------|
| 文件不存在 | Open() 创建空 registry（version=3, sessions={}, bindings={}）|
| v0.2.x 文件（旧 schema）| Migrate() 自动转换 + 备份原文件 |
| v0.1.x 文件（更旧 schema）| Open() 返回 error + log "nightme requires migration from v0.1; please run nightme v0.2.x first" |
| JSON 解析失败 | log error + 自动备份损坏文件为 `.bak` + 重置为空 |
| 文件权限 0600 被改成 0644 | Open() 检测 + warn（不强制修复）|
| 并发 Upsert 同一 session | mutex 串行化；冲突时后写覆盖前写 |
| registry 文件丢失 | 启动时检测 + warn，binding + session 列表为空 |
| 磁盘满 | fsync 失败 → 返回 error → session 创建回滚 |
| 文件被外部修改 | 下次 Upsert 时覆盖（nightme 是 single owner）|
| Session.PID 在系统重启后失效 | `MarkDetached` + Restore 标 Detached；`/run` 时检测到 PID 失效直接 spawn 新 CLI |
| v0.2.x 升级时 binding.ChatType 未知 | 默认 "group"（安全侧）|
| 升级后 v1.1 binary 写出 → v0.2.x binary 读 | v0.2.x 忽略 bindings 字段；可能错过 binding 信息，提示用户升级 |

---

## 7. Test plan

**单元测试**：
- `Open` + `UpsertSession` + `GetSession` 一致性
- `UpsertBinding` + `GetBinding` 一致性
- `Delete` 后 `Get` 返回 false
- `ListSessions` / `ListBindings` 排序（按 StartedAt / ChatID）
- 并发 Upsert 无 race（`-race` flag）
- 文件损坏恢复（写入垃圾 → Open → 应 fallback 到空 + 备份）
- **v0.2 → v1.1 migration**：构造 v0.2 schema 文件 → Migrate → 验证两表内容正确 + 备份文件存在 + version=3

**集成测试**：
- Gateway + MemoryManager + registry: Create session → 验证 SessionEntry 写入
- Gateway Bind → 验证 BindingEntry 写入
- 重启 mock（重新 Open registry）→ MemoryManager.Restore + Gateway.RestoreBindings → 状态一致
- 模拟文件损坏 → 自动备份 + 重置
- **v0.2 → v1.1 upgrade path**：v0.2 binary 创建的 registry 用 v1.1 binary 启动 → Migrate 自动跑 → 后续行为正确

**手动 E2E**：
- nightme v0.2.x 跑 → 创建 sessions + 落盘 → 关闭
- 升级到 v1.1 binary → 启动 → log "migrated registry from v2 to v3" → 飞书 DM 一切正常

---

## 8. Open questions

- 是否需要迁移到 SQLite？v1.1 不需要（< 100 sessions + bindings，JSON 够用）
- 是否记录 session 的 stdout 历史？v1.1 不记录（F-15 v0.4 再做）
- PID 在 macOS 上的 recycle 问题？v1.1 简化：detach 后不检查 PID，下次 /run 直接 spawn 新 CLI 覆盖旧 PID
- BindingEntry.Workspace / Agent 冗余：denormalized 是为了 /cwd reply / /run reply 不需要再读 SessionEntry；trade-off 是 workspace 改了需要更新两个地方（已实现 Rebind 路径同步）

---

## 9. Cross-references

- **Session 数据模型**：见 [`F-01-session-create.md`](./F-01-session-create.md) §2, §3
- **Cleanup 行为**：见 [`F-06-process-cleanup.md`](./F-06-process-cleanup.md)
- **完整 v1.1 架构**：见 [`F-26-gateway-hub.md`](./F-26-gateway-hub.md) §6 commit 5

---

## 10. Change log

- **2026-08-02** — v1.1: 拆为两张表（SessionEntry + BindingEntry）。version 2 → 3。Migrate() 自动升级旧文件。Doc 重写。
- **2026-07-31** — v0.1: 原始单表 schema（SessionEntry 带 ChatID）。已被 v1.1 取代。