# F-CHATSTORE-001: chat_sessions.json — Extract `chatstore`, Eliminate Lost-Update Race

> **Status**: designing → implementation (this doc is the landing page; tests + diff land in subsequent PRs)

> **Depends on**: F-chat-session (per-chat session context), F-27 (ChatSession persistence split), F-29 (AgentSession pool)
> **Related docs**: [`SPEC.md`](../SPEC.md) §1.2, [`F-chat-session.md`](./F-chat-session.md), [`F-runtime.md`](./F-runtime.md) §4

---

## 0. 摘要

chat_sessions.json 的当前写入路径(`ChatSession.persistChatEntry` 7 个 setter)有一个**lost-update race**:cs.mu 释放后才走 Upsert,中间窗口内另一个 goroutine 可以写新字段并抢先 Upsert,导致后到的旧 snapshot 把新值覆盖回磁盘。

修复分两层:

| # | 决策 | 理由 |
|---|------|------|
| **D1** | 抽出 `internal/chatstore.Store`,封装所有 chat_sessions.json 操作 | 持久化路径与 ChatSession 解耦,SetX 不再有"我自己写字段 + 自己落盘"的杂职责 |
| **D2** | 每个 setter 自己持 record.mu.Lock 全程到 Upsert 完成 | 消除 release-then-persist 窗口,修掉 lost-update |
| **D3** | 删除 entry 字段 `AgentSessionIDs` / `SelectedAgentSessionID`(只有写没有读) + `ChatSession` 的死 getter(`PrimaryAgent` / `LastInteractionAt` / `CreatedAt` / `Entry`) | 写但不被读 = 死代码,删干净 |

不引入任何抽象(没有 `mutate` / `structuralChange` / debounce / timer / FlushAll / SyncPool / Touch)。KISS。

---

## 1. Motivation

### 1.1 现场复现

某 chat session `cs_oc_09ef553...` 在 2026-08-19 凌晨到早上之间,反复跑 `/gtw fix -n ...`、`/cwd ...` 一连串命令。daemon 重启后,该 chat 的磁盘 `selectedCwd` 指向一个**已经被删除的工作树** `fix-app-icon`,但用户实际在 `fix-pi-stop` 工作树里跑 claude(UI 上显示 `30eaafa2-...` 的 fix-pi-stop AS session ID)。

**用户输入"继续"**,daemon 报错:

```
Failed to spawn agent: chatsession: spawn failed (selectedAgent="claude",
cwd="/Users/.../nightme.nightme/fix-app-icon"): chatsession: respawn claude at
/Users/.../fix-app-icon: claudecode: start: fork/exec
/Users/geax/.local/bin/claude: no such file or directory
```

错误信息归因到二进制不存在,但 `/Users/geax/.local/bin/claude` 实际是 symlink,daemon 直接 exec 时仍存在。fork/exec 失败的真因是 `child.Dir` 指向已删除的 fix-app-icon 目录(macOS + `SysProcAttr.Setsid: true` 下,chdir 失败被错误归到 binary 路径上 — 详见 daemon 日志 `fork/exec /Users/geax/.local/bin/claude` 段)。

不管 fork/exec 错误归因细节,**根因是 chat_sessions.json 上的 selectedCwd 是 fix-app-icon,跟用户实际工作的 fix-pi-stop 不一致**。

### 1.2 根因

`ChatSession.SetXxx` 系列方法(`SetSelectedCwd`、`SetSelectedAgent`、`SetWatchMode`、`SetThinkMode`、`SetToolsMode`、`ClearSelectedCwd`、`MarkWatcherHintEmitted`)统一模式:

```go
func (cs *ChatSession) SetSelectedCwd(cwd string) error {
    cs.mu.Lock()
    cs.selectedCwd = cwd
    cs.lastInteractionAt = time.Now()
    cs.mu.Unlock()        // ← 释放
    cs.persistChatEntry() // ← 然后 RLock + Upsert
    return nil
}

func (cs *ChatSession) persistChatEntry() {
    cs.mu.RLock()
    defer cs.mu.RUnlock()
    cs.persistChatEntryLocked()
}

func (cs *ChatSession) persistChatEntryLocked() {
    if cs.csFile == nil { return }
    _ = cs.csFile.Upsert(cs.entryLocked())  // ← entryLocked 读 cs 字段
}
```

`entryLocked()` 在 RLock 下读 `cs.pool` / `cs.selectedCwd` / `cs.selectedAS` 等构建 entry,snapshot 是**一致**的。但 `csFile.Upsert(snapshot)` 走 `csFile.mu.Lock()`,**跟 `cs.mu` 互相独立**。

### 1.3 Lost-update 交错(可重现)

时间线:

```
T1: G1.Lock → write selectedCwd=A → G1.Unlock
T2: G1.RLock → snapshot{A} → Upsert A  (拿 csFile.mu,写盘 A,放锁)
T3: G2.Lock → write selectedCwd=B → G2.Unlock (B 是更新的值)
T4: G2.RLock → snapshot{B} → Upsert B  (拿 csFile.mu,写盘 B) ✓
```

这是 happy path,B 赢。但交错反过来:

```
T1: G1.Lock → write selectedCwd=A → G1.Unlock
T2: G2.Lock → write selectedCwd=B → G2.Unlock
T3: G2.RLock → snapshot{B} → Upsert B  (写盘 B)
T4: G1.RLock → snapshot{A} → Upsert A  (写盘 A,覆盖 B) ✗ LOST UPDATE
```

G1 的 snapshot 是**陈旧**的(从它自己 Unlock 那一刻算起),但因为 G1 后到 csFile.mu,**G1 赢**。磁盘留下旧值。

### 1.4 真实触发场景

对单一 chat 并发来源不止一个:

| 来源 | 路径 |
|---|---|
| 命令路径 | /cwd / /use / /watch / /think / /tools |
| gtw 工作流 | /gtw fix -n / /gtw close |
| 命令 watcher hint | `Manager.maybeEmitWatcherHint` 写 `MarkWatcherHintEmitted` |
| shutdown | `runtime.persistChatStates` 在 daemon 退出时遍历所有 cs 强制 SetSelectedAgent 触发一次落盘 |

任何两个 setter 同 chatID 并发,**就可能**就触发 lost-update。

### 1.5 entry 字段审计(发现死代码)

为决策 D3 做证据收集:

| entry 字段 | 写入位置 | 生产读取者 |
|---|---|---|
| `SelectedCwd` | `entryLocked:2279` | `cs.SelectedCwd()`(多)|
| `SelectedAgent` | `entryLocked:2280` | `cs.SelectedAgent()`(多)|
| `PrimaryAgent` | `entryLocked:2281` | `cs.PrimaryAgent()`(**无**)|
| `AgentSessionIDs` | `entryLocked:2282` | **无** |
| `SelectedAgentSessionID` | `entryLocked:2283` | **无**(只有 `UnmarshalJSON` 兼容旧字段名)|
| `CreatedAt` | `entryLocked:2284` | `cs.CreatedAt()`(**无**)|
| `LastInteractionAt` | `entryLocked:2285` | `cs.LastInteractionAt()`(**无**)|
| `WatchMode` / `ThinkMode` / `ToolsMode` / `WatcherHintEmitted` | 各 setter | 各 getter(多)|

`AgentSessionIDs` 和 `SelectedAgentSessionID` **被持久化但永远不被读**。`PrimaryAgent` / `LastInteractionAt` / `CreatedAt` getter **没人调**。

---

## 2. 设计

### D1:抽出 `internal/chatstore`

职责单一:封装 chat_sessions.json 的所有 read/write。ChatSession 通过 `cs.Store()` 拿 store,所有 entry字段写入通过 `store.SetXxx(chatID, value)`。

```go
// internal/chatstore/store.go

type Store struct {
    file *registry.ChatSessionFile
    mu   sync.Mutex                // 保护 records map
    recs map[string]*record
}

type record struct {
    mu    sync.Mutex               // per-chat 写锁
    entry *registry.ChatSessionEntry
}

// ——构造 ——
func New(file *registry.ChatSessionFile) *Store

// —— Bootstrap ——
func (s *Store) Bootstrap(chatID, primaryAgent string) (*registry.ChatSessionEntry, error)
// - 内存有 → 返回
// - 内存没有 disk 有 → 从 disk 加载进内存,返回
// - 都没有 → 创建新 entry, sync Upsert 进 disk,返回

// —— 字段 setter ——
func (s *Store) SetSelectedCwd(chatID, cwd string) error        // cwd=="" 合法
func (s *Store) SetSelectedAgent(chatID, agent string) error    // agent=="" 报错
func (s *Store) SetWatchMode(chatID string, mode int) error
func (s *Store) SetThinkMode(chatID string, mode int) error
func (s *Store) SetToolsMode(chatID string, mode int) error
func (s *Store) SetWatcherHintEmitted(chatID string, v bool) error

// —— 读 ——
func (s *Store) Get(chatID string) (*registry.ChatSessionEntry, bool)
func (s *Store) List() []*registry.ChatSessionEntry
```

**9 个公开方法**,每个 setter 5-10 行。

**没有**:debounce / timer / FlushAll / mutator wrapper / structuralChange / SyncPool / Touch / Flush。每个 setter 自己决定 sync 写盘 + 自己 if-changed 短路。

### D2:每个 setter 自己持 record.mu.Lock 全程

```go
func (s *Store) SetSelectedCwd(chatID, cwd string) error {
    if chatID == "" { return errors.New("chatstore: empty chatID") }
    
    r, err := s.load(chatID)  // 内部:内存 → disk,都没有就报错(Caller 应调 Bootstrap)
    if err != nil { return err }
    r.mu.Lock()
    defer r.mu.Unlock()
    
    if r.entry.SelectedCwd == cwd { return nil }  // self-changed 短路,无 IO
    r.entry.SelectedCwd = cwd
    r.entry.LastInteractionAt = time.Now()
    return s.file.Upsert(r.entry)
}
```

**关键**:每个 setter 持 `record.mu.Lock` 全程,**包括 `s.file.Upsert`**。csFile 内部的 `csFile.mu.Lock()` 串行化跨 chat 写盘,**两层锁互不干涉**:

- record.mu(per-chat):序列化该 chat 的所有 setter
- csFile.mu(全局):序列化磁盘写操作

lost-update 窗口 = 0。

### D3:删除死代码

#### entry 字段

```diff
 type ChatSessionEntry struct {
     ID                     string
     ChatID                 string
     SelectedCwd            string
     SelectedAgent          string
     PrimaryAgent           string
-    AgentSessionIDs        []string
-    SelectedAgentSessionID *string
     CreatedAt              time.Time
     LastInteractionAt      time.Time
     WatchMode              int
     ThinkMode              int
     ToolsMode              int
     WatcherHintEmitted     bool
 }
```

`UnmarshalJSON` 里 `LegacyActiveAgentSessID` 兼容逻辑同步删(目标字段没了)。

#### ChatSession 字段

```diff
 type ChatSession struct {
     ID     string
     ChatID string

-    selectedCwd   string
-    selectedAgent string
-    primaryAgent  string
-    watchMode WatchMode
-    thinkMode ThinkMode
-    toolsMode ToolsMode
-    watcherHintEmitted bool
-    lastInteractionAt time.Time
-    createdAt time.Time
-    csFile *registry.ChatSessionFile
+    store *chatstore.Store

     // 保留:runtime state(pool / queue / buses / ctx / etc.)
 }
```

#### ChatSession 方法

```diff
-// —— entry 字段 setter(全部走 store)——
-func (cs *ChatSession) SetSelectedCwd(cwd string) error
-func (cs *ChatSession) ClearSelectedCwd()
-func (cs *ChatSession) SetSelectedAgent(agent string) error
-func (cs *ChatSession) SetWatchMode(mode WatchMode) error
-func (cs *ChatSession) SetThinkMode(mode ThinkMode) error
-func (cs *ChatSession) SetToolsMode(mode ToolsMode) error
-func (cs *ChatSession) MarkWatcherHintEmitted() error

-// —— 死 getter ——
-func (cs *ChatSession) PrimaryAgent() string
-func (cs *ChatSession) LastInteractionAt() time.Time
-func (cs *ChatSession) CreatedAt() time.Time
-func (cs *ChatSession) Entry() *registry.ChatSessionEntry

-// —— 内部 helper(chatstore 接管持久化)——
-func (cs *ChatSession) persistChatEntry()
-func (cs *ChatSession) persistChatEntryLocked()
-func (cs *ChatSession) entryLocked()
```

`SetToolsMode` 的 `*ToolsMode` 是 typed enum,getter 在 store 入口转 `int`:

```go
// caller 改造:
cs.Store().SetToolsMode(cs.ChatID, int(mode))
```

#### Manager

```diff
 type Manager struct {
     mu sync.RWMutex
     sessions map[string]*ChatSession
     spawner Spawner
-    csFile  *registry.ChatSessionFile
+    store   *chatstore.Store
     asFile  *registry.AgentSessionFile  // 不动
     ...
 }

-func (m *Manager) hydrateFromEntry(...)        // 删,并入 GetOrCreate
-func (m *Manager) RestoreFromRegistry()       // 删
-func persistChatStates(...)                  // 删
```

`Manager.GetOrCreate` 走 `store.Bootstrap(chatID, primaryAgent)`,自动处理内存 / disk / 新建三分支。`Manager.Store()` accessor 暴露给 runtime 在 shutdown 时用(实际上不需要 — 每次 SetX 已 sync Upsert,daemon 退出磁盘自然一致)。

#### runtime/shutdown.go

```diff
-func ShutdownRun(out, ch, mgr, csFile, asFile, ...) error
+func ShutdownRun(out, ch, mgr, asFile, ...) error

-func ShutdownRunMulti(out, chs, csFile, asFile, ...) error
+func ShutdownRunMulti(out, chs, asFile, ...) error

-func persistChatStates(mgr, csFile)   // 删
```

#### caller 迁移(grep + sed)

| 改前 | 改后 |
|---|---|
| `cs.SetSelectedCwd(x)` | `cs.Store().SetSelectedCwd(cs.ChatID, x)` |
| `cs.ClearSelectedCwd()` | `cs.Store().SetSelectedCwd(cs.ChatID, "")` |
| `cs.SetSelectedAgent(a)` | `cs.Store().SetSelectedAgent(cs.ChatID, a)` |
| `cs.SetWatchMode(m)` | `cs.Store().SetWatchMode(cs.ChatID, int(m))` |
| `cs.SetThinkMode(m)` | `cs.Store().SetThinkMode(cs.ChatID, int(m))` |
| `cs.SetToolsMode(m)` | `cs.Store().SetToolsMode(cs.ChatID, int(m))` |
| `cs.MarkWatcherHintEmitted()` | `cs.Store().SetWatcherHintEmitted(cs.ChatID, true)` |
| `cs.PrimaryAgent()` | `cs.Store().Get(cs.ChatID).PrimaryAgent` |
| `cs.LastInteractionAt()` | `cs.Store().Get(cs.ChatID).LastInteractionAt` |
| `cs.CreatedAt()` | `cs.Store().Get(cs.ChatID).CreatedAt` |
| `cs.Entry()` | `cs.Store().Get(cs.ChatID)` |

副作用编排(`oldAS.ClearInFlight()` 等)caller 自己做:

```go
// /cwd handler
oldAS, _ := cs.LookupInPool(cs.SelectedAgent(), cs.SelectedCwd())
if err := cs.Store().SetSelectedCwd(cs.ChatID, args[0]); err != nil {
    return err
}
if oldAS != nil { oldAS.ClearInFlight() }
return nil
```

---

## 3. chat_sessions.json schema 变化

**删除字段**(不再写入):

```diff
 {
   "id": "cs_...",
   "chatId": "...",
   "selectedCwd": "...",
   "selectedAgent": "...",
   "primaryAgent": "...",
-  "agentSessionIds": ["as_..."],
-  "selectedAgentSessionId": "as_...",
   "createdAt": "...",
   "lastInteractionAt": "...",
   "watchMode": 0,
   "thinkMode": 0,
   "toolsMode": 0,
   "watcherHintEmitted": false
 }
```

**向后兼容**:Go JSON unmarshal 默认忽略未知字段。旧文件继续能 unmarshal,只是这俩字段被丢弃。

---

## 4. 防回归

### 4.1 回归测试 `TestSetter_NoLostUpdate`

`internal/chatstore/store_test.go`:

```go
func TestSetter_NoLostUpdate(t *testing.T) {
    // N 个 goroutine 并发跑 SetXxx,断言 on-disk entry 是某个完整 setter 的结果
    // (不是 torn,不是 stale)。任何把 setter 改回 release-then-persist 的 PR 都让这个测试 fail
}

func TestSetter_PersistsLastWriter(t *testing.T) {
    // 直接复现 bug 场景:100 个 goroutine 并发写 SetSelectedCwd("A") vs "B"
    // 修完后断言 disk 一定是 "A" 或 "B",不可能是其他值(包括空)
}
```

### 4.2 chatstore.SetXxx 的 doc 注释

每个 setter 函数顶部加:

```go
// SetXxx mutates the entry for chatID and persists it. Holds record.mu.Lock
// through the Upsert to prevent the lost-update race — release-then-persist
// (Unlock before Upsert) lets a concurrent setter overwrite the in-memory
// state while a stale snapshot is still queued for disk, producing an
// older-value-wins disk state. See F-CHATSTORE-001 §1.3.
```

### 4.3 CI 跑 `go test -race`

`internal/chatsession/...` 和 `internal/chatstore/...` 都跑 -race,捕到任何同步原语写错。

---

## 5. 改动量

| 维度 | 数字 |
|---|---|
| 新增包 | 1 (`internal/chatstore`) |
| 新增公开方法 | 9(setter6 + Bootstrap + Get + List)|
| 删除公开方法 | `cs.SetSelectedCwd` / `ClearSelectedCwd` / `SetSelectedAgent` / `SetWatchMode` / `SetThinkMode` / `SetToolsMode` / `MarkWatcherHintEmitted` / `PrimaryAgent` / `LastInteractionAt` / `CreatedAt` / `Entry` + `Manager.hydrateFromEntry` / `RestoreFromRegistry` / `persistChatStates` |
| 删除字段 | `cs.{selectedCwd, selectedAgent, primaryAgent, watchMode, thinkMode, toolsMode, watcherHintEmitted, lastInteractionAt, createdAt, csFile}` + `entry.{AgentSessionIDs, SelectedAgentSessionID}` |
| caller 改动点 | ~10 处 grep + sed |
| 测试 | 1 个回归测试 + 1 个并发测试 + Bootstrap 三分支测试 + nop-change 短路测试 |

预计总 diff < 600 行(包含测试)。

---

## 6. 后续(本轮不修)

- `agent_sessions.json` 持久化也有类似 race 风险(`AgentSessionFile.Upsert` 同样内部 Lock,但多个 caller 通过不同路径并发写同 AS 的 entry),但 asFile 是另一个边界,本轮不动。
- `cs.Pool()` 是 in-memory runtime state,不被 `chatstore` 持有,直接读 cs 即可。
- 如果未来要批量落盘(每 N 秒 flush dirty),再加 chatstore 的 `FlushAll` — 当前 sync 写盘够用,debounce / timer 是 over-engineering。