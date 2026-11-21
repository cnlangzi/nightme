# F-CHATSTORE-001: chat_sessions.json — `chatstore` 单一存储

> **Status**: 已落地
>
> **Depends on**: F-chat-session (per-chat session context)
>
> **Related docs**: [`SPEC.md`](../SPEC.md) §1.2, [`F-chat-session.md`](./F-chat-session.md) §6, [`CHATSTORE.md`](../CHATSTORE.md)

---

## 0. 摘要

`chat_sessions.json` 的全部读写集中在 `internal/chatstore.Store`。Store 持**一把** mutex，entry 的内存态与磁盘态在同一临界区内更新 —— 「改内存 + 落盘」是一个原子步骤。

| # | 决策 | 理由 |
|---|------|------|
| **D1** | `internal/chatstore.Store` 封装 chat_sessions.json 的所有 read/write | 持久化路径与 ChatSession 解耦 |
| **D2** | Store 单 mutex；字段 setter 在锁内完成「改内存 + 落盘」 | lost-update 窗口 = 0 |

不引入抽象层：没有 debounce / timer / FlushAll / mutator wrapper / SyncPool / Touch。每个 setter 自己判断「值变了才写盘」，同步落盘。

---

## 1. lost-update 的机制

setter 释放自己的锁之后才落盘时，落盘的 snapshot 可能已经陈旧：

```
T1: G1.Lock → 写 selectedCwd=A → G1.Unlock
T2: G2.Lock → 写 selectedCwd=B → G2.Unlock
T3: G2.RLock → snapshot{B} → 写盘 B
T4: G1.RLock → snapshot{A} → 写盘 A   ← 旧值覆盖新值
```

G1 的 snapshot 取自它自己 Unlock 的那一刻，但 G1 后到写盘的那一步，于是 G1 赢 —— 磁盘留下旧值。

同一个 chat 的并发来源不止一个：

| 来源 | 路径 |
|---|---|
| 命令路径 | `/cwd` / `/use` / `/watch` / `/think` / `/tools` |
| gtw 工作流 | `/gtw fix` / `/gtw close` |
| 命令 watcher hint | `MarkWatcherHintEmitted` 写 hint tombstone |
| shutdown | `runtime.persistChatStates` 在 daemon 退出时回写各 chat 的状态 |

任何两个 setter 并发写同一个 chatID 就能触发。

**修法**：改内存与落盘放进同一把锁的同一临界区。锁内先比对（值没变就短路，不写盘），变了就改内存并立刻落盘 —— 落盘期间没有别的 goroutine 能改内存，快照因此不可能陈旧。

---

## 2. Store 的组成

### 2.1 结构

```go
// internal/chatstore/store.go

type Store struct {
    path string

    mu      sync.Mutex
    entries map[string]*registry.ChatSessionEntry
    dropped map[string]json.RawMessage
}
```

- `entries` —— 已知 chatID 前缀的 entry，是权威状态。
- `dropped` —— 未知前缀的 entry，按**原始 JSON 字节**保留，`save()` 原样写回。
- 两张 map 合起来等于磁盘上的 `chatSessions` 对象。

### 2.2 构造与加载

```go
func New(path string) (*Store, error)
```

`New` 读 `path`：

| 情况 | 行为 |
|---|---|
| 文件不存在 | 空 store（正常启动路径） |
| 文件损坏（JSON 解析失败） | 原字节备份到 `<path>.bak`，store 重置为空 |
| 读取失败（非 NotExist） | 返回错误 |
| entry 的 map key ≠ `entry.ChatID` | 返回错误 —— 结构损坏，loader 无法代为修复 |
| entry 的 chatID 前缀未注册 | 原始字节进 `dropped`，warn 日志，`save()` 时原样保留 |
| 有 entry 但一个 channel 都没注册 | 返回错误 —— 否则所有 entry 都会被丢掉且没有有用的反馈 |

前缀集合来自 `channel.ChatIDPrefixes()`（每个 channel adapter 在 `init()` 里通过 `channel.Register` 声明）。空前缀被跳过，不会接受任意 key。新增 channel 只需在自己的 `init()` 里声明前缀，Store 的校验自动跟上，不需要改 `store.go`。

### 2.3 落盘

```go
func (s *Store) save() error
```

调用方必须持 `s.mu`。写入是原子的：临时文件 → `fsync` → `rename` → `chmod 0600`。`entries` 里的 entry 走 typed struct marshal，`dropped` 里的走 raw JSON 原样写回。

### 2.4 公开 API

| 方法 | 语义 |
|---|---|
| `New(path)` | 加载（或初始化）store |
| `Bootstrap(chatID, primaryAgent)` | chatID 不在盘上 → 建 entry（seed `SelectedAgent = primaryAgent`）并落盘；已在盘上 → 只 bump `LastInteractionAt` 再落盘。返回深拷贝 |
| `Get(chatID)` | 返回 entry 的**深拷贝**（指针字段也拷贝），未知 chatID 返回 `(nil, false)` |
| `List()` | 返回全部 entry 的深拷贝快照，顺序不定 |
| `Save(entry)` | 整表写一个 entry（map 写 + 落盘在同一把锁内） |
| `Path()` | store 打开时的文件路径 |
| `Dropped()` | 被原样保留的未知前缀 chatID 列表与数量 |

`Save` 的生产调用方为零 —— 生产路径走 `Bootstrap` 与字段 setter；`Save` 只在测试里给 store seed 数据。

### 2.5 字段 setter

```go
func (s *Store) SetSelectedCwd(chatID, cwd string) error
func (s *Store) SetSelectedAgent(chatID, agent string) error
func (s *Store) SetWatchMode(chatID string, mode int) error
func (s *Store) SetThinkMode(chatID string, mode int) error
func (s *Store) SetToolsMode(chatID string, mode int) error
func (s *Store) SetWatcherHintEmitted(chatID string, v bool) error
```

每个 setter 的契约一致：

1. 取 `s.mu`。
2. chatID 没有 entry → 报错 `entry not initialized; call Bootstrap first`（`SetSelectedAgent` 另外拒绝空 agent 名）。
3. 值与盘上相同 → 直接返回，不写盘。
4. 改内存字段 + bump `LastInteractionAt`（`SetWatcherHintEmitted` 除外：hint 是系统事件不是用户交互）→ `save()`。
5. 放锁。

`SetSelectedCwd` 接受空 cwd（清空语义）。`SetWatchMode` / `SetThinkMode` / `SetToolsMode` 的入参是裸 `int` —— enum 类型住在 chatsession 侧，registry / chatstore 层不依赖它们。

### 2.6 并发契约

- Store 只有一把 mutex，读（`Get` / `List`）也走它。
- 所有 setter 在同一临界区内完成「改内存 + 落盘」，所以并发读者看到的要么是改动前、要么是改动后的状态，不会看到半应用的状态。
- 跨 chatID 同样串行：这是刻意的 —— 落盘写的是整张表，两个 chat 的写盘在同一个临界区里排队，代价是一次 `git`-free 的小 JSON 序列化，换掉整表覆盖的 lost-update。
- `Get` / `List` 返回深拷贝，调用方拿到手改不会影响 store 内部状态。

---

## 3. chat_sessions.json 的磁盘格式

```json
{
  "version": 1,
  "chatSessions": {
    "oc_xxx": {
      "id": "cs_oc_xxx",
      "chatId": "oc_xxx",
      "selectedCwd": "/path/to/worktree",
      "selectedAgent": "claude",
      "primaryAgent": "claude",
      "agentSessionIds": [],
      "selectedAgentSessionId": "as_...",
      "createdAt": "2026-08-19T02:11:00Z",
      "lastInteractionAt": "2026-08-19T03:40:00Z",
      "watchMode": 0,
      "thinkMode": 0,
      "toolsMode": 0,
      "watcherHintEmitted": false
    }
  }
}
```

- `version` 取 `registry.ChatSessionFileVersion`（当前为 1）。
- `chatSessions` 的 key 必须是 entry 自己的 `chatId`。
- 旧字段名在读时透明迁移（`internal/registry/chat_session_entry.go` 的 `UnmarshalJSON`）：`defaultAgent` → `PrimaryAgent`、`activeCwd` → `SelectedCwd`、`activeAgent` → `SelectedAgent`、`activeAgentSessionId` → `SelectedAgentSessionID`。写时只出规范字段名。
- Go 的 JSON unmarshal 默认忽略未知字段，旧文件继续能读。

---

## 4. ChatSession / Manager / runtime 侧的接线

### 4.1 ChatSession

```go
// internal/chatsession/chatsession.go

type ChatSession struct {
    ...
    csStore *chatstore.Store
}

func (cs *ChatSession) WithPersistence(csFile *chatstore.Store, asFile *registry.AgentSessionFile) *ChatSession
```

ChatSession 保留自己的内存副本（`selectedCwd` / `selectedAgent` / `watchMode` / `thinkMode` / `toolsMode` / `watcherHintEmitted` / `lastInteractionAt` / `createdAt`），setter 的顺序固定：

1. `ensureStoreBootstrapped()` —— store 里还没有这个 chatID 时先 `Bootstrap`（生产路径由 `Manager.GetOrCreate` 保证已 Bootstrap，这一步是给没显式 Bootstrap 的调用方兜底）。
2. 调 store 的同名 setter 写盘。
3. 拿 `cs.mu` 更新内存副本。

`cs.mu` 在写盘期间**不**持有 —— 落盘发生在锁外，避免把 IO 拖进热路径的临界区。

`ClearSelectedCwd()` 走 `store.SetSelectedCwd(chatID, "")`（空值合法），并 detach 当前主动工作集、清队列。

`LastInteractionAt()` 优先从 store 读；store 没接上时才回落到内存字段。

### 4.2 Manager

```go
// internal/chatsession/manager.go

type Manager struct {
    ...
    csFile *chatstore.Store
    asFile *registry.AgentSessionFile
}

func (m *Manager) WithPersistence(csFile *chatstore.Store, asFile *registry.AgentSessionFile) *Manager
```

`Manager.GetOrCreate` → `constructChatSession` → `csFile.Bootstrap(chatID, primaryAgent)`（create-or-load），并以 entry 里的 `PrimaryAgent` / `SelectedAgent` 作为 seed。AgentSession 不在这里挂载 —— 那是 `LookupSelectedAgentSession` 的按需职责（见 [`CHATSTORE.md`](../CHATSTORE.md) §7.2）。

`Manager.RestoreFromRegistry()` 仍在：它从 store 的 entry 重建内存里的 ChatSession 表。

### 4.3 runtime

```go
// internal/runtime/deps.go
func defaultOpenChatSessions(cfg *config.Config) (*chatstore.Store, error) {
    ...
    return chatstore.New(path)
}
```

daemon 启动时打开一次 store，共享给所有 channel 的 Manager。shutdown 路径的 `persistChatStates(mgr, csFile)`（`internal/runtime/shutdown.go`）在退出时把各 chat 的状态回写 store。

`cmd/nightme list` / `cmd/nightme kill` 直接对同一个 JSON store 操作（`cmd/nightme/list.go` 的 `openV12Stores`）。

---

## 5. 仍存在的死代码

以下符号在代码里，但生产路径没有读者：

| 符号 | 位置 | 说明 |
|---|---|---|
| `entry.AgentSessionIDs` | `internal/registry/chat_session_entry.go` | 落盘但生产路径不读 |
| `entry.SelectedAgentSessionID` | `internal/registry/chat_session_entry.go` | 落盘但生产路径不读；`UnmarshalJSON` 的 `activeAgentSessionId` 兼容分支仍指向它 |
| `ChatSession.PrimaryAgent()` | `internal/chatsession/chatsession.go` | 无调用方 |
| `ChatSession.CreatedAt()` | `internal/chatsession/chatsession.go` | 无调用方 |
| `ChatSession.LastInteractionAt()` | `internal/chatsession/chatsession.go` | 无调用方 |
| `ChatSession.Entry()` | `internal/chatsession/chatsession.go` | 无调用方 |

`ChatSession.entryLocked()` 组装的 entry 也不写 `AgentSessionIDs` / `SelectedAgentSessionID` —— 那两个字段在磁盘上只可能来自旧文件或外部写入。

---

## 6. 防回归

`internal/chatstore/store_test.go`：

| 测试 | 守住的不变量 |
|---|---|
| `TestSetter_NoLostUpdate` | 并发 setter 之后，磁盘上的 entry 是某一次完整 setter 的结果，不是 torn / stale |
| `TestSetter_PersistsLastWriter` | 并发写 A / B，磁盘只能是 A 或 B |
| `TestSetter_NoChange` | 值没变不写盘 |
| `TestBootstrap_NewChat` / `_ExistingChat` / `_MissingFromDisk` | Bootstrap 的三条分支 |
| `TestGet_CopyReturned` / `TestGet_DeepCopyPointerField` | 读返回深拷贝 |
| `TestConcurrent_DifferentChats` | 跨 chat 并发写不互相破坏 |
| `TestNew_SkipsUnknownPrefix` / `TestNew_DroppedEntriesSurviveSave` | 未知前缀条目被原样保留 |
| `TestNew_RejectsLegacyIDKeyedFormat` | key ≠ ChatID 的旧格式被拒 |

CI 的 `go test -race ./...` 覆盖 `internal/chatstore/...` 与 `internal/chatsession/...`。

---

## 7. 后续

- `agent_sessions.json` 的持久化是另一个边界（`internal/registry/agent_session_file.go`），不在这套 Store 契约内。
- `cs.Pool()` 是进程内的 runtime state，Store 不持有，直接读 cs 即可。
- 批量落盘（每 N 秒 flush dirty）当前不做：同步写盘够用，debounce / timer 是 over-engineering。
