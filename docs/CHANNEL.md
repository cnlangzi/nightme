# Multi-Channel 架构

> **Status**: 设计定稿（v9 lazy 模式），待实施
> **Scope**: nightme 多 IM channel 并行接入与编排
> **读者**: 参与 runtime / gateway / chatsession / command / channel 任一层的工程师
> **Related docs**:
> - [SPEC.md](./SPEC.md) §1.1 七个逻辑组件、§1.3 不变式
> - [SPEC.md](./SPEC.md) §11.1 Daemon startup flow
> - [F-08-channel-abstraction.md](./feat/F-08-channel-abstraction.md) Channel 接口契约
> - [channel/feishu.md](./channel/feishu.md) 飞书实现细节
> - [channel/telegram.md](./channel/telegram.md) Telegram 实现细节

---

## 1. 设计目标

让 nightme 同时接入多个 IM channel（feishu + telegram + slack + 未来更多），**所有已 login 的 channel 全部自动启动**，每个 channel 拥有独立的 ChatSession 集合，**接入新 channel 满足 OCP（不改 runtime / gateway / chatsession / command）**。

### 1.1 核心不变量

| # | 不变量 | 违反后果 |
|---|--------|---------|
| 1 | 所有有凭据的 channel 自动启动，无需 `--channel` flag | 用户体验断（v0.1 现状：feishu 凭据缺失就 exit 7） |
| 2 | 每个 channel 一个 `chatsession.Manager`（chatID 由 channel 自己加前缀天然 namespaced：`tg_<digits>` / `oc_<hex>` / Slack `<vendor>_<id>`） | 跨 channel 撞 chatID |
| 3 | 每个 channel 一个 `outbound.Emitter` = 该 channel 自己 | 引入"multi emitter"伪概念 |
| 4 | ChatSession 不需要知道 channel（无 `channelName` 字段，持久化 schema 零变更） | 持久化耦合 |
| 9 | chatID 是 (channel, rawChatID, thread_id) 的纯函数,不带可变的 daemon state | 漂移 / 孤儿 entry |
| 5 | restore = 懒加载：首次 `GetOrCreate(chatID)` 时 `csFile.GetByChat(chatID)` 命中就 hydrate，miss 就新建 | 启动时 I/O + partition 难题 |
| 6 | 入站：gw pump goroutine 闭包 capture `(channel, mgr)`，dispatch chain 用 mgr per call | routing 概念泄漏 |
| 7 | 出站：`cs.emitter.Send(msg)` = `ch.Send(msg)`，无 routing 表 | 多余抽象 |
| 8 | OCP：接入新 channel = 1 个 adapter 文件 + 1 个 `init()` 注册调用 | — |

### 1.2 跟 SPEC 不变式对齐

| SPEC § | 不变式 | v9 是否满足 |
|---|---|---|
| §1.3 | Channel interface 不暴露 ChatSession / AgentSession / binding | ✅ 接口零修改 |
| §1.3 | ChatSession 不 import channel/feishu | ✅ 不变 |
| §1.3 | Channel ↔ Session 通信 = 单向经过 Gateway | ✅ gw 是 pump hub |
| §1.3 | outbound 路由唯一耦合点 = `out.ReplyTo = as.currentPrompt.LastMessageID` | ✅ 不改 |
| §1.3 | Channel 按 OutboundKind 自决渲染目标 | ✅ Channel 自治 |
| §1.1 | Gateway = 路由器（不假设 Channel 渲染细节） | ✅ gw 只持 `[]Pump` + 共享 Dispatcher |
| §11.1 | outbound.Emitter 是统一咽喉 | ✅ per-Manager.emitter 仍是单一对象 |

---

## 2. 架构图

```
                       nightme daemon
  ┌────────────────────────────────────────────────────────────┐
  │                                                            │
  │   ┌─────────────────── runtime ────────────────────────┐   │
  │   │  runDaemon (4 阶段 wire)                            │   │
  │   │   - 共享资源 (cfg / csFile / asFile / agents)      │   │
  │   │   - 共享编排 (gtw / reactionRouter / commander /   │   │
  │   │     shellDispatcher / inbound.Router)              │   │
  │   │   - 逐 channel buildStack → Manager + Emitter      │   │
  │   │   - runtime.allMgrs []*Manager  (包私有)           │   │
  │   │   - findChatSession(chatID) 跨 mgr 线性扫         │   │
  │   │   - gw.AttachPumps + gw.Start                     │   │
  │   └─────────────────────────────────────────────────────┘   │
  │                          │                                  │
  │   ┌─────────────────── gateway.Router ─────────────────┐   │
  │   │  dispatcher *inbound.Router (共享)                 │   │
  │   │  pumps []Pump                                      │   │
  │   │    Pump { Channel channel.Channel                  │   │
  │   │            Manager *chatsession.Manager }          │   │
  │   │                                                    │   │
  │   │  pumpOne goroutine 每 Pump 启一个                  │   │
  │   │  闭包 capture (channel, mgr)                       │   │
  │   └─────┬──────────────┬───────────────────────────────┘   │
  │         │              │                                   │
  │   ┌─────▼─────┐  ┌─────▼─────┐                            │
  │   │  feishu   │  │  telegram │   <- channel.Channel        │
  │   │  Adapter  │  │  Adapter  │      (实现 channel.Channel) │
  │   │  + state  │  │  + state  │                            │
  │   └─────┬─────┘  └─────┬─────┘                            │
  │         │              │                                   │
  │   ┌─────▼─────┐  ┌─────▼─────┐                            │
  │   │ Manager   │  │ Manager   │   <- chatsession.Manager    │
  │   │  sessions │  │  sessions │      (per-channel)          │
  │   │   [oc_xxx]│  │   [12345] │                              │
  │   │  emitter  │  │  emitter  │   <- outbound.Emitter       │
  │   │   =ch     │  │   =ch     │      (单 channel，无 router) │
  │   └───────────┘  └───────────┘                            │
  │                                                            │
  │   shared:                                                  │
  │   - csFile (in-memory, chatID-keyed)                       │
  │   - asFile (in-memory, chatSessionId-keyed)                │
  │   - command.Registry (factory pattern)                    │
  │   - gtw.Manager + ReactionRouter                           │
  └────────────────────────────────────────────────────────────┘

外部 IM 平台：
  ┌────────────┐                    ┌────────────┐
  │  Feishu WS │ ◄──────────────►   │  Telegram  │
  │            │                    │  Bot API   │
  └────────────┘                    └────────────┘
```

---

## 3. 数据流

### 3.1 启动流程

```
1. 共享资源就绪
   cfg := deps.LoadConfig()
   csFile, asFile := OpenChatSessions(cfg), OpenAgentSessions(cfg)
   agents := BuildAgents(cfg)
   prCache := &prcache.Registry{}
   gtwDeps := gtw.HandlerDeps{...}

2. 共享编排就绪
   gtwMgr := gtw.NewManager(); gtwMgr.SetHandlerDeps(gtwDeps)
   gtwMgr.SetGetChatSession(findChatSession)  // 跨 mgr 扫
   reactionRouter := commandServices.NewReactionRouter()
   reactionRouter.Register("*", gtwMgr.HandleReaction)
   gitStatusLookup := func(ctx, chatID) *GitStatus {
       cs := findChatSession(chatID); if cs == nil { return nil }
       return cs.GitStatus(ctx)
   }
   command.SetDeps(command.Deps{Primary: cfg.Primary, GTWExt: gtwDeps})
   commander := command.NewCommander(command.Default())
   shellDispatcher := shell.NewDispatcher()
   ir := inbound.New(commander, shellDispatcher, reactionRouter, cfg.Primary)

3. 启动 channels，逐个 buildStack
   chs, _ := deps.NewChannels(cfg)  // channel.BuildAll(cfg)
   var pumps []gateway.Pump
   for _, ch := range chs {
       if err := ch.Start(ctx); err != nil {
           logger.Error("channel start failed", "name", ch.Name(), "err", err)
           continue
       }
       ch.SetLogger(logger)
       fmt.Fprintf(out, "Channel %s connected\n", ch.Name())
       
       mgr := buildStack(ch, buildStackOpts{...})
       runtime.allMgrs = append(runtime.allMgrs, mgr)
       pumps = append(pumps, gateway.Pump{Channel: ch, Manager: mgr})
       
       if deps.RegisterHealth != nil {
           deps.RegisterHealth(ch.HealthSnapshot)
       }
   }

4. 启动 gw（不调 RestoreFromRegistry！）
   gw := gateway.New(ir)
   gw.AttachPumps(pumps...)
   gw.Start(ctx)
```

### 3.2 入站流程

```
ch[feishu].Incoming()
   │
   ▼
gw.pumpOne goroutine (闭包: mgr := allMgrs["feishu"])
   │
   ▼
inbound.Router.Dispatch(ctx, mgr, msg)
   │
   ├─ tryActionDispatch  (reactions)
   ├─ tryCommandDispatch (slash commands)
   ├─ tryShellDispatch   (! shell)
   └─ tryMessageDispatch
       │
       ▼
       mgr.HandleInbound(ctx, msg)
           │
           ▼
           mgr.GetOrCreate(chatID, primaryAgent)
               │
               ├─ m.sessions[chatID] hit → return
               │
               ├─ csFile.GetByChat(chatID) hit → hydrateFromEntry
               │   (恢复 selectedCwd/Agent/WatchMode/ThinkMode/ToolsMode
               │    + AgentSession 池 Detached + 清 selectedAS)
               │
               └─ miss → New(chatID, primaryAgent)
           
           ↓
       处理 msg (slash command / agent 调度 / etc.)
           ↓
       cs.emitter.Send(msg)  ──►  ch[feishu].Send(msg)
```

### 3.3 出站流程

```
cs.emitter.Send(msg)
   │
   ▼
ch[feishu].Send(msg)
   │
   ▼
Feishu API
```

无 routing 表。`cs.emitter` 是 `Manager.emitter`，是构造时绑定的该 channel 的 `outbound.Emitter`。

### 3.4 懒加载 restore 流程

```
首次 GetOrCreate(chatID)
   │
   ├─ m.sessions[chatID] hit → return (in-memory)
   │
   └─ miss
      │
      ├─ csFile.GetByChat(chatID) hit
      │   │
      │   ▼
      │   hydrateFromEntry(entry, ...)
      │   - New ChatSession with entry fields
      │   - cs.selectedCwd = entry.SelectedCwd
      │   - cs.selectedAgent = entry.SelectedAgent
      │   - cs.watchMode = WatchMode(entry.WatchMode)
      │   - cs.thinkMode = ThinkMode(entry.ThinkMode)
      │   - cs.toolsMode = ToolsMode(entry.ToolsMode)
      │   - cs.watcherHintEmitted = entry.WatcherHintEmitted
      │   - cs.lastInteractionAt = entry.LastInteractionAt
      │   - cs.selectedAS = nil  (handle 丢失，强制清空)
      │   - AgentSession 池 from asFile.ListByChatSessionID(entry.ID)
      │     全部 status=Detached
      │   - m.sessions[chatID] = cs
      │   - m.onCreate(cs)  (runtime hooks)
      │
      └─ miss → New(chatID, primaryAgent) (全新)
```

`csFile` 本身在 `OpenChatSessionFile` 时已全量加载到内存的 `entries map`（`registry/chat_session_file.go:31-61`）。`GetByChat(chatID)` 是 in-memory O(N) 查表。N 通常 10-100，无压力。

---

## 4. 模块详细设计

### 4.1 `channel.Registry` —— 接入点

**文件**：`internal/channel/registry.go`（新）

```go
type Builder func(*config.Config) (Channel, error)

var (
    mu  sync.RWMutex
    reg = map[string]Builder{}
)

func Register(name string, b Builder)
func Available() []string
func BuildAll(cfg *config.Config) ([]Channel, error) {
    // 遍历 reg，挨个调 Builder；builder 返回 missing-creds 错误时跳过
    // 全部失败才返回 error
    // 成功构造的收集到 []Channel
}
```

**使用方**：
- `internal/channel/feishu/init.go`：`channel.Register("feishu", NewAdapter)`
- `internal/channel/telegram/init.go`：`channel.Register("telegram", NewAdapter)`
- `cmd/nightme/...` 引入上述包触发 `init()`
- `echo` 不入 registry（保留 `Deps.NewChannels` 测试钩子）

**Channel 接口零修改**——v9 没有 `OwnsChatID`，没有 `channelName`，没有 `Channel()` 之外的任何方法。`channel.Channel` 的 9 个原有方法完全保持。

### 4.2 `chatsession.Manager` —— per-channel 实例

**文件**：`internal/chatsession/manager.go`（改）

`Manager` 结构体零字段变更。`GetOrCreate` 内部加 lazy hydrate 分支：

```go
func (m *Manager) GetOrCreate(chatID, primaryAgent string) (*ChatSession, error) {
    // Phase 1: in-memory
    m.mu.RLock()
    cs, ok := m.sessions[chatID]
    m.mu.RUnlock()
    if ok { return cs, nil }
    
    // Phase 2: lazy hydrate from csFile (NEW)
    var spawner Spawner
    var csFile *registry.ChatSessionFile
    var asFile *registry.AgentSessionFile
    var emitter outbound.Emitter
    m.mu.RLock()
    spawner, csFile, asFile, emitter = m.spawner, m.csFile, m.asFile, m.emitter
    m.mu.RUnlock()
    
    if csFile != nil {
        if entry, ok := csFile.GetByChat(chatID); ok {
            cs = m.hydrateFromEntry(entry, primaryAgent, spawner, csFile, asFile, emitter)
        }
    }
    if cs == nil {
        var err error
        cs, err = New(chatID, primaryAgent)
        if err != nil { return nil, err }
        cs.WithSpawner(spawner).WithPersistence(csFile, asFile)
        if emitter != nil { cs.WithEmitter(emitter) }
    }
    
    // Phase 3: insert + onCreate
    m.mu.Lock()
    defer m.mu.Unlock()
    if existing, ok := m.sessions[chatID]; ok { return existing, nil }
    m.sessions[chatID] = cs
    if m.onCreate != nil { m.onCreate(cs) }
    return cs, nil
}
```

`hydrateFromEntry` 把现有 `Manager.RestoreFromRegistry`（`manager.go:738-818`）里的 entry 恢复代码抽出为独立函数（selectedCwd/Agent/WatchMode/ThinkMode/ToolsMode/AgentSession 池）。

`Manager.RestoreFromRegistry` **方法保留**（21 个测试 caller 不破），但 `runtime.runDaemon` **不再调它**。行为等价于"首次 GetOrCreate 时 hydrate"。

### 4.3 `gateway.Router` —— 共享 Dispatcher + 多 Pump

**文件**：`internal/gateway/gateway.go`（大改）

```go
type Router struct {
    mu         sync.RWMutex
    dispatcher *inbound.Router  // 共享
    pumps      []Pump           // 替代原 channels + chatToChan
    stopCh     chan struct{}
    stopOnce   sync.Once
    wg         sync.WaitGroup
    bindings   map[string]*BindingEntry
    // 删：emitter / inbound / channels / chatToChan / defaultChannel / channelCh
}

type Pump struct {
    Channel channel.Channel
    Manager *chatsession.Manager
}

func New(dispatcher *inbound.Router) *Router
func (r *Router) AttachPumps(pumps ...Pump)
func (r *Router) Start(ctx context.Context) error
func (r *Router) Stop(ctx context.Context) error
func (r *Router) pumpOne(ctx context.Context, p Pump) {
    defer r.wg.Done()
    in := p.Channel.Incoming()
    for {
        select {
        case <-ctx.Done(): return
        case <-r.stopCh: return
        case msg, ok := <-in:
            if !ok { return }
            if _, err := r.dispatcher.Dispatch(ctx, p.Manager, &msg); err != nil {
                slog.Default().Warn("gateway: dispatch failed", ...)
            }
        }
    }
}
```

**删除的方法**：`AttachChannels` / `pumpInbound` / `dispatchLoop` / `resolveChannel` / `ResolveChannel`

**删除的字段**：`emitter` / `inbound` / `channels` / `chatToChan` / `defaultChannel` / `channelCh`

### 4.4 `inbound.Router` —— Dispatch 接受 mgr per call

**文件**：`internal/gateway/inbound/inbound.go`（改）

```go
type Router struct {
    // 删：csMgr MessageHandler
    commander CommandDispatcher
    shell     ShellDispatcher
    action    ReactionRouter
    primary   string
}

func New(commander CommandDispatcher, sh ShellDispatcher, action ReactionRouter, primaryAgent string) *Router

func (r *Router) Dispatch(ctx context.Context, mgr *chatsession.Manager, msg *messages.InboundMessage) (*CommandResult, error) {
    if msg == nil { return nil, nil }
    for _, try := range []func(context.Context, *chatsession.Manager, *messages.InboundMessage) (bool, *CommandResult, error){
        r.tryActionDispatch,
        r.tryCommandDispatch,
        r.tryShellDispatch,
        r.tryMessageDispatch,
    } {
        handled, result, err := try(ctx, mgr, msg)
        if err != nil { return nil, err }
        if handled { return result, nil }
    }
    return nil, nil
}

func (r *Router) tryMessageDispatch(ctx context.Context, mgr *chatsession.Manager, msg *messages.InboundMessage) (bool, *CommandResult, error) {
    return true, nil, mgr.HandleInbound(ctx, msg)
}
```

4 个 `tryXxx` 方法全部加 `mgr *chatsession.Manager` 参数。

### 4.5 `command.Factory` —— Handle 接受 mgr per call

**文件**：`internal/command/factory.go`（改）

```go
type Factory struct {
    // 删：mgr *chatsession.Manager
    // 删：registry *chatsession.ManagerRegistry
    primary string
    // 其他字段不动
}

func (f *Factory) Handle(ctx context.Context, mgr *chatsession.Manager, input *SlashInput) (*CommandResult, error) {
    cs, _ := mgr.GetOrCreate(input.ChatID, f.primary)
    // ... 后续逻辑不变（用 mgr 替代 f.mgr）
}
```

`command.Commander.Dispatch(ctx, mgr, input)` 签名改。

`command.Deps`：
```go
type Deps struct {
    // 删：Manager / Registry
    Primary string
    GTWExt  gtw.HandlerDeps
}
```

**21 个命令子包**（`internal/command/<name>/cmd.go`）：
- `Factory.Handle` 签名改 `(ctx, mgr, input)`
- `f.mgr.Get(...)` → `mgr.Get(...)`

### 4.6 `shell.Dispatcher` —— Handle 接受 mgr per call

**文件**：`internal/shell/dispatcher.go`（改）

```go
type Dispatcher struct {
    // 删：registry 字段
}

func NewDispatcher() *Dispatcher  // 无参

func (d *Dispatcher) Handle(ctx context.Context, mgr *chatsession.Manager, msg *messages.InboundMessage) error {
    // 内部用 mgr.Emitter().Send(msg) 做 outbound
}
```

### 4.7 `runtime.runDaemon` —— 4 阶段 wire

**文件**：`internal/runtime/runtime.go`（改）

```go
var allMgrs []*chatsession.Manager  // 包私有，gtw/gitStatus 用

func findChatSession(chatID string) *chatsession.ChatSession {
    for _, mgr := range allMgrs {
        if cs := mgr.Get(chatID); cs != nil { return cs }
    }
    return nil
}

func runDaemon(ctx, out, deps, sigCh, logger) error {
    // === 阶段 1: 共享资源 ===
    // 阶段 2: 共享编排
    // 阶段 3: 启动 channels，逐个 buildStack
    // 阶段 4: 启动 gw（不调 RestoreFromRegistry）
}

func buildStack(ch channel.Channel, opts buildStackOpts) *chatsession.Manager {
    spawner := chatsession.NewRegistrySpawner(opts.Agents)
    mgr := chatsession.NewManager().
        WithSpawner(spawner).
        WithPersistence(opts.CSFile, opts.ASFile)
    em := outbound.New(ch, outbound.Options{GitStatusLookup: opts.GitStatusLookup})
    mgr.WithEmitter(em).WithPrimaryAgent(opts.Primary)
    // wire runtime callbacks
    // ... WireRuntimeCallbacksAndRestore(mgr, ...)
    return mgr
}
```

**删除**：
- `runtime.go:183-185` feishu 凭据 prefix check
- `runDaemon` 里的 `Manager.RestoreFromRegistry` 调用
- 单 channel 假设的所有代码（`ch := deps.NewChannel(cfg)` 等）

### 4.8 `Deps` —— NewChannel 改 NewChannels

**文件**：`internal/runtime/deps.go`（改）

```go
type Deps struct {
    // 改：NewChannel func(*config.Config) (channel.Channel, error)
    //   → NewChannels func(*config.Config) ([]channel.Channel, error)
    // 删：SkipFeishuLogin bool
    // 删：WithChannel 函数
    NewChannels func(*config.Config) ([]channel.Channel, error)
}

func DefaultDeps() Deps {
    return Deps{
        // 改：NewChannel: feishu.NewAdapter
        //   → NewChannels: channel.BuildAll
        NewChannels: channel.BuildAll,
    }
}
```

### 4.9 `cmd/nightme/run.go` —— 删 `--channel` flag

```go
// 删：var channelName string
// 删：cmd.Flags().StringVar(&channelName, "channel", "feishu", ...)
// 删：runRun(cmd, channelName) 改 runRun(cmd)
```

### 4.10 `internal/login/login.go` —— LoginWith 按 provider 分派

```go
func LoginWith(ctx, provider, out, errOut) error {
    // ...
    cfg, _ := config.LoadDefault()
    
    switch provider.Name() {  // 替代原无脑 3 字段
    case "feishu":
        cfg.Feishu.AppID = creds.AppID
        cfg.Feishu.AppSecret = creds.AppSecret
    case "telegram":
        cfg.Telegram.BotToken = creds.BotToken
    default:
        return fmt.Errorf("login: unknown provider %q", provider.Name())
    }
    
    // ... SaveDefault + Greet
}
```

---

## 5. 关键不变量验证

### 5.1 持久化 schema 零变更

| Entry 字段 | 用途 | 来源 |
|---|---|---|
| `id` | ChatSession ID | 构造时生成 |
| `chatId` | **唯一 key** | Channel 消息自带（带 channel 前缀：`tg_<digits>` / `oc_<hex>` / `<vendor>_<id>`） |
| `selectedCwd` | /cwd 设 | user command |
| `selectedAgent` | /use 设 | user command |
| `primaryAgent` | 创建时 snapshot | cfg.Primary |
| `agentSessionIds` | pool 索引 | spawn 时维护 |
| `selectedAgentSessionId` | active AS | /use 切换 |
| `watchMode` | /watch on/off | user command |
| `thinkMode` | /think on/off | user command |
| `toolsMode` | /tools on/off | user command |
| `watcherHintEmitted` | hint tombstone | 一次性 |
| `lastInteractionAt` | LRU | 任何 inbound |
| `createdAt` | — | New() |
| `inFlightMessages` | AS-level 不在 CS 持久化 | — |

**v9 不加任何字段**。`channelName` 在 v8 提议过，v9 砍掉。

### 5.2 ChatSession 不知道 channel

`ChatSession` 结构体零字段变更。`Manager.emitter` 是构造时绑定的 channel 的 Emitter，ChatSession 继承之。出站走 `cs.emitter.Send(msg)` —— 无需 `cs.channel` 字段。

### 5.3 跨 channel 共享组件

| 共享组件 | 怎么拿 per-chat Manager | 备注 |
|---|---|---|
| `gtw.Manager` reaction handler | `findChatSession(chatID)` 跨 mgr 扫 | 共享 gtw 实例 |
| `gitStatusLookup` | `findChatSession(chatID)` 跨 mgr 扫 | 共享 closure |
| `command.Factory` | 链内 mgr 透传 | 共享 registry |
| `shell.Dispatcher` | Handle 接受 mgr per call | 单实例 |
| `commandServices.ReactionRouter` | 已共享 | 单例 |
| `inbound.Router` | Dispatch 接受 mgr per call | 单实例 |

`findChatSession` 是包私有函数，N=2-3 时 O(N) 线性扫无压力。

### 5.4 partition 隐式由 pump 闭包决定

```
pump goroutine (闭包 capture mgr)
   ↓
inbound.Router.Dispatch(ctx, mgr, msg)
   ↓
mgr.HandleInbound(ctx, msg)
   ↓
mgr.GetOrCreate(chatID, primaryAgent)
   ↓
新 ChatSession 落到 mgr.sessions
   ↓
cs.emitter = mgr.emitter = 该 mgr 对应的 channel 的 Emitter
```

**chatID 不会跨 channel 撞**（每 channel 用自己的 prefix 隔离：`tg_<digits>` / `oc_<hex>` / 各平台 namespace）。chatID 唯一性是隐式 partition 的基础。详见 §5.5。

### 5.5 chatID 命名空间规则

每条 channel 必须在 chatID 前加自己的 channel-specific 前缀，保证跨 channel 物理隔离：

| Channel | chatID 形式 | 来源 |
|---|---|---|
| Telegram | `tg_<chat.id>` 或 `tg_<chat.id>:<thread_id>` | telegram adapter 的 `sessionChatID` 加前缀（topic 实际存在时） |
| Feishu | `oc_<hex>` | Feishu API 自带 |
| 未来 Slack/Lark | `<vendor>_<id>` | 各 adapter 自己加 |

**稳定性约束**（关键）：

1. **chatID 必须是 update 内容的纯函数**,不依赖 daemon state / config
2. 同一 DM / 同一群 / 同一 topic 跨 daemon 重启 / 升级 / 状态文件丢失,chatID 永远一致
3. **不允许在 chatID 拼接中引用任何运行时状态**(如自动创建的 sentinel topic ID)
4. **不允许在 chatID 拼接中引用任何配置**(如 `topic_mode: separate` vs `shared`)

违反这些约束会导致 chatID 漂移,旧 entry 变孤儿——这正是 `feature/telegram` 分支 `/cwd` 后 `/gtw fix` 报 "No active workspace" 的根因。

反向 split (`splitSessionID`) 也必须是纯函数,确保 inbound 和 outbound 两侧始终看到同一 chatID。

---

## 6. OCP 验证

| 行为 | 改的位置 | 不改的位置 |
|---|---|---|
| 接入 feishu | `channel/feishu/init.go` 注册 | 全部其它代码 |
| 接入 telegram | `channel/telegram/init.go` 注册 | 全部其它代码 |
| 接入 slack | `channel/slack/adapter.go` + `init.go` 注册 | 全部其它代码 |
| 接入未来 webhook channel | `channel/webhook/init.go` + `cmd/nightme` 引入 | 全部其它代码 |
| 加 `/cwd` 之类命令 | `internal/command/<name>/cmd.go` + `reg.Register` | runtime / gateway / chatsession / channel 全不动 |
| 改 GitStatus stamping | runtime 注入的闭包 | gateway / outbound / chatsession 全不动 |
| 加新 OutboundKind | `messages.OutboundKind` + `gateway.Translate` + 各 channel 适配 | chatsession / runtime 全不动 |

完全 OCP。

---

## 7. 跟现状的对比

### 7.1 新增文件

| 文件 | 用途 |
|---|---|
| `internal/channel/registry.go` | channel 注册中心 |
| `internal/channel/feishu/init.go` | feishu 注册 |
| `internal/channel/telegram/init.go` | telegram 注册 |

### 7.2 改动文件

| 文件 | 改动 |
|---|---|
| `internal/login/login.go` | `LoginWith` 按 `provider.Name()` 分派写 cfg |
| `internal/chatsession/manager.go` | `GetOrCreate` 加 lazy hydrate + `hydrateFromEntry` 抽出 |
| `internal/command/factory.go` + 21 个命令包 | `Factory.Handle(ctx, mgr, input)` 改签名 |
| `internal/command/commander.go` | `Commander.Dispatch(ctx, mgr, input)` 改签名 |
| `internal/shell/dispatcher.go` | `NewDispatcher()` 无参；`Handle(ctx, mgr, msg)` |
| `internal/gateway/inbound/inbound.go` | `New` 删 csMgr 参；`Dispatch(ctx, mgr, msg)`；4 个 try 方法加 mgr |
| `internal/gateway/gateway.go` | 大改：删 5 字段 + 3 方法 + 加 `Pump`/`AttachPumps`/`pumpOne` |
| `internal/runtime/runtime.go` | 4 阶段 wire + `allMgrs` + `findChatSession` + `buildStack` |
| `internal/runtime/deps.go` | `NewChannel` → `NewChannels`；删 `WithChannel`/`SkipFeishuLogin` |
| `cmd/nightme/run.go` | 删 `--channel` flag |

### 7.3 不动的东西

- `chatsession.Manager` / `ChatSession` 字段（**零修改**）
- `messages.InboundMessage` / `OutboundMessage`（**零修改**）
- 持久化 schema（**零修改**）
- `outbound.New(ch, opts)`（**零修改**）
- `channel.Channel` 接口（**零修改**）
- `feishu.NewAdapter` / `telegram.NewAdapter` 内部协议
- `gtw.Manager` / `commandServices.ReactionRouter` 内部
- 现有 38 个 `outbound.New` caller
- 现有 21 个 `Manager.RestoreFromRegistry` caller

### 7.4 净增 / 净删

| 类型 | 数量 |
|---|---|
| 新增文件 | 3 |
| 新增私有变量 | 1（`runtime.allMgrs`） |
| 新增私有函数 | 1（`runtime.findChatSession`） |
| 新增字段 | **0** |
| 新增持久化字段 | **0** |
| 新增接口方法 | **0** |
| 签名变更 | 5（`inbound.Router.Dispatch` / `command.Factory.Handle` / `command.Commander.Dispatch` / `shell.Dispatcher.Handle` / `inbound.New` / `gateway.New` / `shell.NewDispatcher`） |
| 删除字段/方法 | 11（`csMgr` / `emitter` / `chatToChan` / `defaultChannel` / `channelCh` / `channels` / `pumpInbound` / `dispatchLoop` / `resolveChannel` / `WithChannel` / `SkipFeishuLogin`） |

---

## 8. 已知约束与边界

### 8.1 chatID 跨 channel 撞（理论可能）

- feishu `oc_*` / telegram 数字 / 各平台 namespace 互不撞
- 文档约束：channel 实现必须保证 chatID 在平台内有唯一性
- 实际风险：极低，chatID 命名空间天然分离

### 8.2 跨 mgr 的 ChatSession 查找

`findChatSession(chatID)` 是 O(N) 线性扫，N=2-3 个 channel 时无压力。

未来 N 增长（>10 个 channel）时考虑：channel-level 索引（每个 channel 维护自己的 chatID 集合）+ 跨 channel chatID→channel map。

### 8.3 懒加载的首次延迟

首次 `GetOrCreate(chatID)` 走 `csFile.GetByChat(chatID)` 是 in-memory O(N) 遍历 csFile.entries。N=100 时纳秒级。无 I/O。

### 8.4 错误恢复

如果 `csFile` 在 `OpenChatSessionFile` 加载失败（`registry/chat_session_file.go:31-61`）：
- 备份为 `.bak`，内存 store 初始化为空
- 懒加载时 `GetByChat` 永远返回 miss
- 行为：相当于"全新启动，所有 chat 走 `New(chatID, primary)` 分支"
- 这跟现状 `Manager.RestoreFromRegistry` 处理失败的方式一致

### 8.5 echo 烟雾测试

echo 不入 `channel.Registry`（`Deps.NewChannels` 仍可被测试覆盖）。生产 runtime 不会启 echo；测试代码自己注入。

---

## 9. 执行清单

按依赖顺序排列，每步独立编译通过 + 跑现有测试：

| # | 改动 | 风险 |
|---|------|------|
| 1 | `channel.Registry` + `BuildAll` + feishu/telegram `init.go` 注册 | 低 |
| 2 | `LoginWith` 按 `provider.Name()` 分派 | 低 |
| 3 | `Manager.GetOrCreate` 加 lazy hydrate + `hydrateFromEntry` | 中 |
| 4 | `command.Factory` 删 mgr/registry 字段 + `Handle(ctx, mgr, input)`；`command.SetDeps` 删 Manager/Registry；commander `Dispatch(ctx, mgr, input)`；21 个命令子包同步改 | 中 |
| 5 | `shell.Dispatcher` 删 registry + `NewDispatcher()` 无参 + `Handle(ctx, mgr, msg)` | 中 |
| 6 | `inbound.Router` 删 csMgr + `New` 删 csMgr/em 参 + `Dispatch(ctx, mgr, msg)` + 4 个 try 加 mgr | 中 |
| 7 | `gateway.Router` 大改：删 5 字段 + 3 方法 + 加 `Pump`/`AttachPumps`/`pumpOne` | 中 |
| 8 | `runtime.runDaemon` 4 阶段 wire + `allMgrs` + `findChatSession` + `buildStack` + 删 feishu prefix check + 删 RestoreFromRegistry 调用 | 中 |
| 9 | `runtime.Deps.NewChannels` + 删 `WithChannel`/`SkipFeishuLogin` | 中 |
| 10 | `cmd/nightme/run.go` 删 `--channel` flag | 低 |
| 11 | 测试改名 + 新增 lazy hydrate 测试 + 新增 LoginWith 互不干扰测试 | 低 |

---

## 10. 跟 v0.x → v1.x 演进历史

| 阶段 | 状态 | 关键约束 |
|---|---|---|
| v0.x | 单 channel（feishu） | gateway + manager + emitter 都绑死 feishu |
| v1.0 | 单 channel + 抽 channel interface | gateway 持 `channel.Channel` 抽象，但 runtime 仍硬编码 feishu |
| v1.1 | 加 echo 用于 smoke test | 通过 `Deps.NewChannel` 注入 echo |
| v1.2 | commit 8a-8b: ChatSession 池 + persistence | `Manager.RestoreFromRegistry` 引入 |
| **v1.3 (本设计)** | **多 channel 自动启动** | 1) 引入 `channel.Registry` 2) per-channel Manager/Emitter 3) 懒加载 restore 4) OCP 干净 |

---

## 11. 未来工作

### 11.1 Channel 间的 ChatSession 迁移

如果用户从 feishu 切到 telegram（同一 chat 内容），目前需要手动重发。future: chatID 映射 + 内容迁移工具。

### 11.2 Channel 健康检查暴露

`deps.RegisterHealth` 已为每个 channel 注册 `HealthSnapshot`。`nightme health` 命令可以枚举所有 channel 的健康状态。

### 11.3 Channel 动态启停

目前 channel 在 daemon 启动时全部 attach，不支持运行时增删。future: `nightme channel add` / `nightme channel remove` 子命令，热更新 gw.pumps + runtime.allMgrs。

### 11.4 Per-channel 配置覆盖

`channel.Registry.Builder` 目前只接 `*config.Config`。future: 接 `ChannelOptions` 允许 `cfg.Channels["feishu"]` 风格的 per-channel 覆盖（rate limit、polling timeout、mention policy 等）。
