# F-51: Slash Command / Service 分离重构（gtw 子集首落地）

> **Status**: 🛠 **设计 + 文档阶段**（v1.3.x；本 PR 仅交付 §0.16 B0：本文档 + SPEC §0.16 + 清「（待补）」标记。B1 / B2 / B3 落地是后续 PR 的工作；本文是设计目标 + 落地路径的契约，**不是**已完成状态的报告）
> **Milestone**: v1.3.x
> **Scope（本 PR）**:
> - 新建 `internal/command/` 包骨架：`commander.go` / `factory.go` / `runtime.go` / `services/{session,reaction}.go` / `event.go` / `preflight.go` / `reply.go`
> - 新建 `internal/command/gtw/`：gtw 全部代码（types / fix / action / worktree / provider / rebuild / render / slug / api / git_status / time / debug）从 `internal/gtw/` 迁入；6 个 gtw-only 类型**直接定义**（不再通过 5 个 type alias 借住 chatsession）
> - 拆 `internal/gateway/handlers_gtw.go` + `handlers_gtw_debug.go`：slash command 主入口 + `/gtw test` 调试进入 `internal/command/gtw/commands.go` + `debug.go`，实现 `SlashCommandFactory` 接口
> - `chatsession` 全面脱钩：删 `gtwContext` / `gtwDrafts` / `onReaction` 字段；删 `SetActionHandler` / `ActionHandler` / `HandleAction`；删 `ReactionEvent` 类型；删 `gtw_state.go` / `gtw_accessors.go` 整文件
> - reaction 分发切到 `command/services.ReactionRouter`：`cmd/nightme/run.go` 启动时 `router.Register("...", gtwMgr.HandleReaction)`；删 `gw.WithActionHandler` 的 gtw 路径
> - Gateway 把 `/gtw` 路由从直调 `handleGTW` 切到 `commander.Dispatch`；其余 7 个 slash command（`/cwd` `/use` `/kill` `/new` `/watch` `/think` `/tools`）**暂留** `internal/gateway/handlers_*.go` 旧路径，§0.16 后续 batch 迁移
> - 测试：`internal/chatsession/reaction_test.go` 整文件删除（被删 API）；`internal/gateway/dispatch_action_test.go` 改成测 `ReactionRouter`；`internal/gtw/*_test.go` 迁到 `internal/command/gtw/*_test.go` 并改成以 `gtw.Manager` / `gtw.HandlerDeps` 为 fixture
> - 文档：写本 doc + `SPEC.md` 末尾补 §0.16（4 批次迁移计划）；`docs/FEATURES.md` / `SPEC.md §0.15` 清除「（待补）」标记
>
> **Scope（本 PR 不做，留 §0.16 后续 batch）**:
> - 其余 7 个 slash command 搬到 `internal/command/<name>/`
> - `chat_sessions.json` / `agent_sessions.json` 持久化路径迁出 `gateway`（这是 command 化的副作用，不在本 PR）
> - `/gtw` 历史 on-disk 状态 schema 升级（F-51 不动 gtw 持久化——gtw 状态本就是 in-memory only，daemon 重启走 `RebuildContext`）
>
> **Depends on**: 无（纯重构；不依赖 F-45 / F-46 / F-49 / F-50）
> **Related**: [`SPEC.md`](../SPEC.md) §0.15 / §0.16 / §1.3 / §1.4 / §2.3 / §2.7 + [`F-46-interactive-cards.md`](./F-46-interactive-cards.md)（reaction 入口的 channel-side 归一化） + [`F-50-git-provider.md`](./F-50-git-provider.md)（gtw 内部 provider 抽象，已先于 F-51 落地）
>
> **Breaking changes**:
> - 删 `chatsession.{GTWState, GTWContext, GTWDraftKind, GTWFixDraftPayload, GTWDraft, CardChoice, ReactionEvent}` 7 个类型
> - 删 `chatsession.ChatSession.{SetActionHandler, ActionHandler, HandleAction}` 3 个方法
> - 删 `chatsession.ChatSession` 字段 `gtwContext` / `gtwDrafts` / `onReaction`
> - 任何 `import "internal/gtw"` 的代码须改 `import "internal/command/gtw"`
> - `gtw.HandlerDeps{NewPlatform: ...}` 删（F-50 已经删）；保留 `Prober` / `Detect` / `Send` / `SendCard` / `Git` / `Now` 字段

---

## 0. 背景

### 0.1 v1.3.x 现状：gtw 散在 3 个包，chatsession 被反向耦合

`/gtw` 子系统当前物理散落在 3 个包，违反 SPEC §1.3「chatsession 是中立 session 持有者」+ §1.4「抽象 / 具体边界规范」两条不变式：

| 包 | 文件 | 角色 |
|---|---|---|
| `internal/gtw/` | `types.go` / `fix.go` / `action.go` / `worktree.go` / `provider.go` / `rebuild.go` / `render.go` / `slug.go` / `api.go` / `git_status.go` / `time.go` + 3 test | gtw 业务实现 + 5 个 type alias 把 chatsession 的 gtw 类型"借出去" |
| `internal/chatsession/` | `gtw_state.go` / `gtw_accessors.go` | 6 个 gtw-only 类型 + 10 个 GTW accessor（注释自首"为避免 cycle 而住在 chatsession"） |
| `internal/gateway/` | `handlers_gtw.go` / `handlers_gtw_debug.go` | `/gtw` 与 `/gtw test` 的命令入口 + 适配器（`gtwContextSlot` / `gtwDraftsMap` / `csSender`） |

chatsession 因此**永久**持有 6 个 gtw-only 类型（`GTWState` / `GTWContext` / `GTWDraftKind` / `GTWFixDraftPayload` / `GTWDraft` / `CardChoice`，均住 `chatsession/gtw_state.go`）、10 个 `GTW*` 方法（住 `chatsession/gtw_accessors.go`）、2 个 gtw 字段（`gtwContext` / `gtwDrafts`）+ 1 个 reaction 字段（`onReaction`）、3 个 reaction 方法（`SetActionHandler` / `ActionHandler` / `HandleAction`）。这违反 §1.3 不变式——chatsession 不再"中立"。

`internal/gtw/types.go:34-46` 的 5 个 type alias（`State = chatsession.GTWState` 等）是这条不变式被破坏的最显眼证据。B5 个 const alias（`StateFixing` / `StatePushing` / `StateReady` / `StateCanceled` + `DraftFixBranchExists` / `DraftFixLabelTaken` / `DraftFixWorktreeFail`）同样问题。

### 0.2 反应分发的双向耦合

`/gtw` 决策卡（`branch-exists` / `worktree-fail`）被用户点 ✅ / 🆕 / 🔗 / ❌ / 🔄 / 🤝 后，要让 gtw 状态机接住这条 reaction，反过来 PATCH 原卡。整条链：

```
Channel.Incoming(reaction event)
  → gateway.dispatchAction → actionHandler closure
  → cs.HandleAction(ev)         ← chatsession 是分发者
  → onReaction(ev)              ← gtw 注册的 handler
  → gtw.HandleAction(...)
```

chatsession 充当 reaction 分发器，本身持有 1 个字段 + 3 个方法。F-51 §2.7 把这条分发链**从 chatsession 抽出**到 `command/services.ReactionRouter`（runtime 持单例 registry）。`chatsession.SetActionHandler` / `HandleAction` 整套 API 删除；gtw 在 runtime 启动时 `router.Register(gtwMgr.HandleReaction)` 注册自己。

### 0.3 历史"cycle 规避"是不必要的丑方案

`internal/chatsession/gtw_state.go:2-9` 注释说"types 住这里是为了 `ChatSession` 持有 `*GTWContext` 时不 import gtw，避免 `gtw → chatsession (for Sender) → gtw` 的 cycle"。

**这个 cycle 是真的，但规避方式太丑**——它把 6 个 gtw 类型永久钉在 chatsession 包，迫使 chatsession 永久包含 gtw 痕迹。F-51 的解法是「gtw 拥有自己的类型，跨包只通过接口」：

- gtw 包定义自己的 6 个类型**直接定义**（不是 alias）——`State` / `Context` / `DraftKind` / `Draft` / `FixDraftPayload` / `CardChoice`，从 `chatsession/gtw_state.go` 迁入后重命名（去掉 `GTW` 前缀）
- gtw 包删掉自己原有的 `ReactionEvent` struct（`internal/gtw/types.go:163`），统一使用 `command.ReactionEvent`（见 §1.2.6）
- gtw 包的 `Sender` 接口（`Send(ctx, OutMsg) error`）就定义在 gtw 包里，由 `cmd/nightme/` 装配时塞实现
- gtw 包**完全不 import chatsession**
- chatsession 删 `*GTWContext` 字段后，**也不 import gtw**

双向断开，cycle 不存在。`chatsession` 回到 §1.3 角色。

### 0.4 设计目标

1. **chatsession 回归 §1.3 中立 session 持有者**：删 6 个 gtw-only 类型 + 10 个 GTW accessor + 3 个 reaction 方法 + 2 个 gtw 字段 + 1 个 reaction 字段（`onReaction`）；不再 import 任何 `command/` / `internal/gtw` 包；不在 import 表里出现"GTW" / "command" / "gtw" 任何一个标识
2. **gtw 自管状态**：`gtw.Manager` 持 `map[chatID]*Context` / `map[chatID]*Draft` / `map[chatID]*Sender`，不再借用 `chatsession.ChatSession` 字段
3. **reaction 路由抽离**：`ChatSession.SetActionHandler` / `HandleAction` 删；runtime 持 `ReactionRouter` 单例，gtw 启动时 `Register("...", gtwMgr.HandleReaction)`
4. **slash command 路由抽象**：`internal/command/` 包提供 `Commander` / `SlashCommandFactory` / `RuntimeServices` 抽象；gtw 实现 `SlashCommandFactory`；Gateway 把 `/gtw` 路由切到 `commander.Dispatch`
5. **依赖单向**：concrete impl (chatsession / gtw / channel) ↑ implements service interface (command/services/*.go) ↑ implements command abstraction (command/<name>/commands.go) ↑ uses gateway+channel top layer。**下层 import 上层即破不变式**

---

## 1. 设计

### 1.1 包布局（本 PR 落地后）

```
internal/command/                              ← NEW · 抽象层
├── commander.go            Commander / Dispatch / Spec
├── factory.go              SlashCommandFactory / Registry
├── runtime.go              RuntimeServices 聚合
├── event.go                ReactionEvent    ← 从 chatsession 迁出
├── preflight.go            RequireActiveCwd 等通用 preflight
├── reply.go                Reply(ctx, rt, text) · 唯一 reply helper
├── services/
│   ├── session.go          SessionService 接口
│   └── reaction.go         ReactionRouter 接口 + runtime impl
└── gtw/                                       ← NEW · 从 internal/gtw/ 迁入
    ├── commands.go         /gtw SlashCommandFactory 主入口
    ├── debug.go            /gtw test 调试入口
    ├── manager.go          Manager: 持 states / drafts / senders (per-chatID)
    ├── types.go            State / Context / Draft / DraftKind /
    │                       FixDraftPayload / CardChoice / Card /
    │                       OutCardMsg / OutMsg / ReactionEvent /
    │                       ReactionKind · 直接定义
    ├── api.go              HandlerDeps
    ├── fix.go              RunFix
    ├── action.go           HandleAction
    ├── action_routing.go   ActionLookup / executeXxxAction 共享 helper
    ├── worktree.go         worktree 创建 / 探测
    ├── provider.go         GitProvider / GitHubProvider / GitLabProvider / Detect / HTTPProber
    ├── rebuild.go          RebuildContext
    ├── render.go           BranchExistsCard / WorktreeFailCard
    ├── slug.go             slug 生成
    ├── git_status.go       git status 探测
    ├── time.go             NowFunc / 默认 time.Now
    ├── *_test.go           随源代码迁
```

**未迁的旧包（后续 batch）**:
- `internal/gateway/handlers_chatsession.go`（`handleCwd` / `handleUse` / `handleKill`）
- `internal/gateway/handlers_new.go` / `handlers_watch.go` / `handlers_think.go` / `handlers_tools.go`
- `internal/gtw/`（本 PR 落地后**整包删除**）

### 1.2 接口契约

**关键设计决策（避免 import cycle）**：`internal/command/` **不 import** `internal/gateway` / `internal/channel`。Commander 的 input / output / Channel 接口都用 command 包**自己定义的最小类型**；gateway 在 boundary 做 `*gateway.InboundMessage` → `command.SlashInput` 翻译。详见 §1.2.7 跨包翻译规约。

#### 1.2.1 `internal/command/commander.go`

```go
// Commander is the slash command dispatch surface. Gateway
// dispatches inbound messages here; Commander routes by command
// name to a registered SlashCommandFactory.
//
// Thread-safety: Dispatch may be called from multiple goroutines
// (one per inbound). Implementations must be safe for concurrent
// use.
type Commander interface {
    // Dispatch runs the slash command implied by msg.Text. If the
    // message is not a slash command, returns Consumed=false and
    // a nil result so the caller can fall through to the agent
    // loop. Empty / unknown command → reply with usage hint.
    Dispatch(ctx context.Context, rt RuntimeServices, input SlashInput) (*SlashOutput, error)
}

// Spec describes one registered slash command. Used for help
// generation and dispatch routing.
type Spec struct {
    // Name is the bare command name without the leading slash,
    // e.g. "gtw" for /gtw.
    Name string
    // Aliases are alternative names that route to the same
    // factory (e.g. "h" → help).
    Aliases []string
    // Summary is a one-line help description, surfaced by /help.
    Summary string
    // Usage is a short usage hint surfaced when args are missing
    // or invalid. Free-form; may be a multi-line string.
    Usage string
}
```

#### 1.2.2 `internal/command/factory.go`

```go
// SlashCommandFactory builds the per-command implementation. The
// runtime calls New() at registration time, passing the assembled
// RuntimeServices. The returned SlashCommand's Spec() is used for
// dispatch routing and help generation; Handle() is called once
// per inbound that names this command.
type SlashCommandFactory interface {
    Spec() Spec
    // Handle dispatches one inbound that named this command.
    // Takes/returns command-package types (SlashInput /
    // SlashOutput) — NEVER gateway types.
    Handle(ctx context.Context, rt RuntimeServices, input SlashInput) (*SlashOutput, error)
}

// Registry holds the command dispatch table. Runtime owns one;
// the gateway only sees Commander.
type Registry struct { /* unexported */ }

func (r *Registry) Register(cmd SlashCommandFactory)
func (r *Registry) Specs() []Spec
func (r *Registry) FindByName(name string) SlashCommandFactory
```

#### 1.2.3 `internal/command/runtime.go`

```go
// RuntimeServices aggregates the dependencies a slash command
// receives at Handle() time. The runtime (cmd/nightme/run.go)
// builds this once at startup; the Commander passes it to every
// dispatched Handle() call.
//
// Commands must NEVER reach for *chatsession.Manager / *gtw /
// *gateway concrete types via this struct — only the interfaces
// below. RuntimeServices does NOT contain gateway.Channel or
// gateway.OutboundMessage; it carries command.Channel (this
// package) instead.
type RuntimeServices struct {
    Session        SessionService
    ReactionRouter ReactionRouter
    Channel        Channel
    // Reserved: future Logger / Metrics / Config once they
    // become part of the command contract.
}

// Channel is command-package's view of an outbound channel.
// *gateway.Channel satisfies this interface (compile-time
// asserted in cmd/nightme/run.go via
// `var _ command.Channel = (*gateway.Channel)(nil)`).
type Channel interface {
    Send(ctx context.Context, m Outbound) (msgID string, err error)
    SendCard(ctx context.Context, m Outbound) (msgID string, err error)
}
```

#### 1.2.4 `internal/command/services/session.go`

```go
// SessionService is the chat-side session surface commands
// depend on. The implementation lives in `cmd/nightme/`
// (`session_adapter.go`) as a `sessionAdapter` wrapping
// `*chatsession.Manager`. This package is **interfaces-only** —
// see §3.2 for why the adapter MUST NOT live here (otherwise
// this package would import chatsession, breaking the
// dependency arrow).
type SessionService interface {
    // Get returns the ChatSession for chatID, or nil if absent.
    // Commands that need a session (most) typically pair with
    // GetOrCreate via mgr.GetOrCreate(chatID, primary) — but
    // that requires primary agent name; for the common case
    // where the chat already has a session we use Get.
    Get(chatID string) Session

    // GetOrCreate returns the ChatSession for chatID, creating
    // it lazily with the given primary agent name. primary
    // typically comes from cfg.Primary; commands don't read
    // config directly.
    GetOrCreate(chatID, primaryAgent string) Session
}

// Session is the per-chat state surface that slash commands
// need. Internally wraps *chatsession.ChatSession but exposes
// only the methods commands actually call. Commands MUST go
// through this interface (NOT the concrete *ChatSession) so
// the command layer has zero chatsession dependency.
type Session interface {
    // Agent / cwd
    ActiveCwd() string
    SetActiveCwd(cwd string) error
    ActiveAgent() string
    SetActiveAgent(name string) error
    PrimaryAgent() string

    // Lifecycle
    LookupActiveAgentSession() (AgentSession, error)
    SetActiveAgentSession(as AgentSession)
    KillAll() ([]KillResult, error)
    NewActiveAgentSessions(ctx context.Context, agentName string) (matched int, sessions []AgentSession, results []NewResult, err error)

    // Per-chat toggle modes
    WatchMode() WatchMode
    SetWatchMode(WatchMode) error
    ThinkMode() ThinkMode
    SetThinkMode(ThinkMode) error
    ToolsMode() ToolsMode
    SetToolsMode(ToolsMode) error
}

// AgentSession / KillResult / NewResult / WatchMode / ThinkMode /
// ToolsMode are re-declared here as interfaces / types so the
// command layer doesn't import chatsession. The runtime adapter
// (`cmd/nightme/session_adapter.go`) wraps the chatsession
// concrete types.
```

#### 1.2.7 跨包翻译规约（避免 import cycle）

`internal/command/` 不 import `internal/gateway` / `internal/channel`。但 `gateway.WithCommander` 必须能调 `commander.Dispatch`，`channel` adapter 又要构造 `SlashInput.Reaction` 等 command 类型。解法是**单边翻译 + 装配点持有双引用**：

| 方向 | 实现 |
|---|---|
| `gateway` → `command` | `internal/channel/feishu/adapter.go` 等 channel 适配器构造 `command.ReactionEvent` 时直接 import `command/event.go`（channel → command 单向，OK） |
| `gateway` → `command` | `gateway.Gateway.WithCommander` 接 `DispatchFunc`（`func(ctx, *InboundMessage) (*CommandResult, error)`），**不**接 `command.Commander` interface——避免 gateway 依赖 `command.Commander` |
| `command` → `gateway` | **不存在**。command 永远不 import gateway 或 channel。`command.Channel` interface 由 runtime 装配时检查 `*gateway.Channel` 满足（`var _ command.Channel = (*gateway.Channel)(nil)`，写在 `cmd/nightme/run.go`） |
| `cmd/nightme/` → 两者 | `cmd/nightme/run.go` 是唯一同时 import 两边的装配者；提供 `DispatchFunc` 实现：`func(ctx, gwMsg) { input := command.SlashInput{...}; out, _ := commander.Dispatch(ctx, rt, input); return gwOutboundFromCommand(out) }` |
| `internal/channel/feishu/` → `command` | `adapter.go::handleActCardAction` 构造 `command.ReactionEvent{...}`（替换现有的 `gateway.ReactionEvent{...}`，因为 `gateway.ReactionEvent` 是 `chatsession.ReactionEvent` 的 type alias，跟着 chatsession 删） |

**总结**：command 是不被任何上层依赖的「最底层接口包」；gateway 通过 `DispatchFunc`（函数值）反向调 command，避开接口依赖；channel 通过 `command.ReactionEvent` 直接用 command 的 canonical 类型（channel 本身就被 gateway 依赖，channel → command 是新的 leaf edge）。

#### 1.2.8 `internal/command/event.go` 增补（与 §1.2.6 合并）

§1.2.6 给出 `ReactionEvent`；同一文件再加 `SlashInput` / `SlashOutput` / `Outbound` / `Card` / `CardChoice` 五个类型，作为 Commander / SlashCommandFactory 的输入输出载体：

```go
// SlashInput is command-package's view of one inbound message.
// gateway.WithCommander receives *gateway.InboundMessage and
// translates to this struct before calling Commander.Dispatch.
type SlashInput struct {
    ChatID     string         // IM-side chat id (D1 model; see SPEC §3.1)
    UserID     string         // sender
    Text       string         // full message text including any "/cmd args..."
    MessageID  string         // channel-native message id (used for ReplyTo)
    HasMention bool           // whether bot was @-mentioned (for WatchMode gate)
    Reaction   *ReactionEvent // non-nil for reaction / action events
    Args       []string       // pre-parsed args (gateway's parser fills this)
}

// SlashOutput is command-package's view of one command's result.
// gateway translates back to *gateway.CommandResult.
type SlashOutput struct {
    Reply    string    // human-readable reply text
    Consumed bool      // true = handled, gateway does NOT forward to agent loop
    Dropped  bool      // true = silently drop (e.g. /watch off + not @-mentioned)
    Outbound []Outbound // when set, gateway uses these instead of building from Reply
}

// Outbound is command-package's view of one outbound message.
type Outbound struct {
    ChatID  string
    Text    string
    ReplyTo string // empty = top-level
    Card    *Card  // non-nil = send as card
}

// Card is command-package's view of one interactive card.
// gtw.Card translates to this at the action boundary.
type Card struct {
    Kind        string
    Title       string
    Body        string
    Choices     []CardChoice
    RequestID   string
    Disabled    bool
    ChosenEmoji string
}

type CardChoice struct {
    Emoji  string
    Label  string
    Action string // "act:/xxx" form
}
```

#### 1.2.5 `internal/command/services/reaction.go`

```go
// ReactionRouter dispatches one reaction event to the right
// handler. The runtime holds one shared router (singleton,
// process-wide); slash command packages (gtw, future /follow)
// register themselves at startup via Register.
type ReactionRouter interface {
    // Register binds a handler to a chatID. chatID == "*" means
    // "all chats" (gtw is global). A second Register for the
    // same chatID overwrites.
    Register(chatID string, handler func(ctx context.Context, ev ReactionEvent) bool)
    // Handle dispatches one reaction. Returns true if any
    // registered handler consumed the event; false if no
    // handler matched (caller may log + drop). Handler errors
    // are logged and treated as "not consumed".
    Handle(ctx context.Context, chatID string, ev ReactionEvent) bool
}

// reactionRouter is the concrete runtime impl held in
// cmd/nightme/run.go. thread-safe; uses a sync.RWMutex around
// the handler map.
type reactionRouter struct { /* unexported */ }
func NewReactionRouter() ReactionRouter { return &reactionRouter{} }
```

#### 1.2.6 `internal/command/event.go`

```go
// ReactionEvent is the inbound reaction payload. F-51 把它从
// chatsession 迁到这里作为 canonical 类型。`chatsession.ReactionEvent`
// （F-45 / F-46 给 gtw 用的 bridge）和 `gtw.ReactionEvent`
// （internal/gtw/types.go:163，独立 struct，不是 alias）F-51 落地时
// **都删**——所有引用点统一改 import 到 `command.ReactionEvent`。
//
// channel/feishu/adapter.go::handleActCardAction 当前构造的是
// `gateway.ReactionEvent`，那是 `chatsession.ReactionEvent` 的
// type alias（gateway/messages.go:199），跟着一起删。
type ReactionEvent struct {
    TargetMsgID string
    Emoji       string
    UserID      string
    ChatID      string
}
```

### 1.3 gtw.Manager（state 自管）

```go
// internal/command/gtw/manager.go

// Manager owns the per-chat gtw state. Previously these fields
// lived on chatsession.ChatSession (gtwContext / gtwDrafts); the
// "type alias trick" forced chatsession to know about gtw. With
// F-51, Manager is the only place that knows what gtw state
// looks like; chatsession is unaware.
//
// The runtime instantiates one Manager per process and shares
// it across all chats. Per-chat substate (states / drafts /
// senders) is keyed by chatID and protected by a single
// sync.RWMutex.
type Manager struct {
    mu       sync.RWMutex
    states   map[string]*Context          // chatID → active /gtw fix snapshot
    drafts   map[string]map[string]*Draft // chatID → userMsgID → pending draft
    senders  map[string]Sender            // chatID → outbound sender (csSender adapter)
}

// Sender is what gtw uses to push messages back to the user.
// Production: cmd/nightme wires a closure that wraps
// chatsession.ChatSession's ActiveCwd/SetActiveCwd/Send to
// channel.Send — but Manager never sees chatsession directly.
type Sender interface {
    ActiveCwd() string
    SetActiveCwd(cwd string) error
    Send(ctx context.Context, m OutMsg) error
}

// HandleReaction is the callback gtw registers with
// ReactionRouter. Looks up the chatID's drafts map and
// dispatches to the per-draft action executor.
func (m *Manager) HandleReaction(ctx context.Context, ev ReactionEvent) bool
```

### 1.4 装配模式

```go
// cmd/nightme/run.go (pseudo-code; B1 末 / B2 头落地时的实际装配)

// 1. Build substate services
chatMgr := chatsession.NewManager().WithPersistence(...)
router  := command.NewReactionRouter()

// 2. Build gtw Manager; register its reaction handler.
gtwMgr := gtw.NewManager(...)
router.Register("*", gtwMgr.HandleReaction)

// 3. Build command registry; only gtw is in command/ for now.
reg := command.NewRegistry()
reg.Register(gtw.NewFactory(gtwMgr))

// 4. Build RuntimeServices; pass to Commander.
rt := command.RuntimeServices{
    Session:        sessionAdapter{chatMgr, cfg.Primary},
    ReactionRouter: router,
    Channel:        ch,
}
commander := command.NewCommander(reg)

// 5. Build gateway; wire commander as the slash dispatch path.
gw := gateway.New(...).WithCommander(commander, rt)
gw.WithActionHandler(func(ctx, msg) bool {
    if msg.Reaction == nil { return false }
    return rt.ReactionRouter.Handle(ctx, msg.ChatID, command.ReactionEvent{
        TargetMsgID: msg.Reaction.TargetMsgID,
        Emoji:       msg.Reaction.Emoji,
        UserID:      msg.Reaction.UserID,
        ChatID:      msg.ChatID,
    })
})
```

### 1.5 反应路由新链 vs 旧链

**旧链**（chatsession 是分发者）：
```
Channel.Incoming(reaction event)
  → gw.dispatchAction → gw.actionHandler closure
  → cs.HandleAction(ev)              ← chatsession 分发
  → onReaction(ev)                   ← gtw 注册的 handler
  → gtw.HandleAction(...)
```

**新链**（runtime router 是分发者）：
```
Channel.Incoming(reaction event)
  --> gw.dispatchAction --> gw.actionHandler closure
  --> rt.ReactionRouter.Handle(ctx, chatID, ev)   <-- runtime 单例
  --> router 查 map[chatID]handler
  --> gtwMgr.HandleReaction(ev)                   <-- gtw 自管状态,不再走 chatsession
       |__ lookup drafts[chatID][ev.TargetMsgID]
       |__ run ExecuteBranchExistsAction / ExecuteWorktreeFailAction
       |__ mutate gtw.Manager.states[chatID] / gtw.Manager.drafts[chatID]
       |__ emit Outbound via Sender adapter (g.Manager.senders[chatID])
```

chatsession 完全不见 reaction；gtw 也不见 chatsession。

---

## 2. 文件 & 接口

### 2.1 `internal/command/` 骨架（NEW）

#### 2.1.1 `commander.go`（约 80 行）
- `type Commander interface` + `type Spec struct`
- `NewCommander(reg *Registry) Commander` —— 简单实现，查 reg.FindByName(name)，调 Handle

#### 2.1.2 `factory.go`（约 100 行）
- `type SlashCommandFactory interface`
- `type Registry` + `NewRegistry()` + `Register` / `Specs` / `FindByName`

#### 2.1.3 `runtime.go`（约 30 行）
- `type RuntimeServices struct { Session SessionService; ReactionRouter ReactionRouter; Channel Channel }`

#### 2.1.4 `services/session.go`（约 150 行）
- `type SessionService interface`（Get / GetOrCreate）
- `type Session interface`（ActiveCwd / SetActiveCwd / ActiveAgent / SetActiveAgent / PrimaryAgent / LookupActiveAgentSession / SetActiveAgentSession / KillAll / NewActiveAgentSessions / WatchMode / SetWatchMode / ThinkMode / SetThinkMode / ToolsMode / SetToolsMode）
- `type AgentSession interface`（Forward declaration；runtime adapter 包 chatsession.AgentSession）
- `type KillResult struct` + `type NewResult struct`（重新声明字段，runtime 适配）
- `type WatchMode / ThinkMode / ToolsMode`（重新声明为独立类型，runtime 适配 chatsession.* 同名类型）
- `type sessionAdapter` —— **不**在 `command/services/`，搬到 `cmd/nightme/session_adapter.go`（unexported；实现 Session / SessionService 接口，包 *chatsession.Manager + *chatsession.ChatSession + *chatsession.AgentSession）。原因见 §1.2.5 + §3.2。

#### 2.1.5 `services/reaction.go`（约 120 行）
- `type ReactionRouter interface`
- `type reactionRouter struct`（unexported）+ `NewReactionRouter() ReactionRouter`
- `Register` / `Handle` 实现（map[chatID]handler；sync.RWMutex；handler 错误记 log + 视为未 consume）

#### 2.1.6 `event.go`（约 30 行）
- `type ReactionEvent struct`（从 chatsession 迁出）

#### 2.1.7 `preflight.go`（约 50 行）
- `func RequireActiveCwd(s Session) (string, *gateway.CommandResult)` —— 复用 /cwd /use 等命令的 preflight 提示

#### 2.1.8 `reply.go`（约 40 行）
- `func Reply(ctx, rt, text) *CommandResult` —— 唯一 reply helper；rt.Channel().Send(OutboundMessage{Kind: OutReply, Text: text})

### 2.2 `internal/command/gtw/`（NEW，从 `internal/gtw/` 整体迁入）

#### 2.2.1 文件级 move 列表

| 源 | 目标 | 改动 |
|---|---|---|
| `internal/gtw/types.go` | `internal/command/gtw/types.go` | 删 `type State = chatsession.GTWState` 等 5 个 alias；6 个 gtw 类型**直接定义**（`State` / `Context` / `DraftKind` / `Draft` / `FixDraftPayload` / `CardChoice`）；删 `chatsession.` 前缀的所有 const 别名（`StateFixing` 等 4 个 const + `DraftFixBranchExists` 等 3 个 const 直接定义）；删 `chatsession` import |
| `internal/gtw/api.go` | `internal/command/gtw/api.go` | `Sender` 接口从 chatsession 解放；只保留 gtw 自身的依赖 |
| `internal/gtw/fix.go` | `internal/command/gtw/fix.go` | 替换 `chatsession.GTWFixDraftPayload` 引用为 `gtw.FixDraftPayload`（同包内引用，零开销） |
| `internal/gtw/action.go` | `internal/command/gtw/action.go` | 删 `csSender` 适配器（搬到 `cmd/nightme/run.go` 装配时）；改用 `Manager.HandleReaction` 入口 |
| `internal/gtw/action_routing.go` | `internal/command/gtw/action_routing.go` | 同上 |
| `internal/gtw/worktree.go` | `internal/command/gtw/worktree.go` | 无 chatsession 引用，原样迁 |
| `internal/gtw/provider.go` | `internal/command/gtw/provider.go` | 无 chatsession 引用，原样迁（F-50 已先完成） |
| `internal/gtw/rebuild.go` | `internal/command/gtw/rebuild.go` | 改用 `Manager.Get(chatID)` 替代 `cs.GTWContext()` |
| `internal/gtw/render.go` | `internal/command/gtw/render.go` | 原样迁 |
| `internal/gtw/slug.go` | `internal/command/gtw/slug.go` | 原样迁 |
| `internal/gtw/git_status.go` | `internal/command/gtw/git_status.go` | 原样迁 |
| `internal/gtw/time.go` | `internal/command/gtw/time.go` | 原样迁 |
| `internal/gtw/gtw_test.go` | `internal/command/gtw/gtw_test.go` | **重写**：以 `gtw.Manager` + `gtw.HandlerDeps` 为 fixture，删 `*chatsession.ChatSession` 直接依赖；新增 `fakeSender` 替 `csSender` |
| `internal/gtw/git_status_test.go` | `internal/command/gtw/git_status_test.go` | 改 package 声明 `gtw` |
| `internal/gtw/action_routing_test.go` | `internal/command/gtw/action_routing_test.go` | 改 package 声明 `gtw` |
| — | `internal/command/gtw/manager.go`（NEW） | `Manager` struct + `NewManager` + `HandleReaction` + per-chat Get/Set/Store/Take/List/Count/Clear context+draft |
| — | `internal/command/gtw/commands.go`（NEW） | `Factory` 实现 `SlashCommandFactory`；`Spec()` 返回 `{Name: "gtw", Aliases: nil, Summary: "GTW: Git-driven team workflow", Usage: "..."}`；`Handle()` 解析 `/gtw fix|test|list|reset` 子命令 |
| — | `internal/command/gtw/debug.go`（NEW） | 从 `internal/gateway/handlers_gtw_debug.go` 迁入的 `/gtw test` 入口；改用 `Manager` 替代 `cs.ListGTWDrafts` 等 |

#### 2.2.2 删除项

迁完 + 验证后整包删除：
- `internal/gtw/` —— 全部 13 个 Go 文件 + 3 个 test
- `internal/gateway/handlers_gtw.go` / `handlers_gtw_debug.go` —— 2 个文件（已迁）
- `internal/gateway/handlers_gtw_test.go` —— 1 个文件（已迁；如还有未迁部分，删）

### 2.3 `internal/chatsession/` 收尾

| 改动 | 说明 |
|---|---|
| 删 `chatsession.go` 字段 `gtwContext *GTWContext` / `gtwDrafts map[string]*GTWDraft` / `onReaction func(...)` | `gtwContext` + `gtwDrafts` 在 `NewChatSession` 构造时初始化（共 2 处声明 + 2 处初始化）；`onReaction` 只在 struct 声明（func 类型，零值 nil，由 `SetActionHandler` 后赋） |
| 删 `chatsession.go` 方法 `SetActionHandler` / `ActionHandler` / `HandleAction` | 三处（l. 678 / 689 / 703） |
| 删 `chatsession.go` 调试 slog "F-46 debug: ChatSession.{SetActionHandler,HandleAction}" | 4 行（l. 679-681, 708-711, 714-717, 719-720） |
| 删 `chatsession/reaction_state.go` 整文件 | `ReactionEvent` 已迁到 `command/event.go` |
| 删 `chatsession/gtw_state.go` 整文件 | 6 个 gtw-only 类型已迁到 `command/gtw/types.go` |
| 删 `chatsession/gtw_accessors.go` 整文件 | 10 个 `GTW*` 方法已迁到 `command/gtw/manager.go` |
| 删 `chatsession/reaction_test.go` 整文件 | 4 个 TestHandleAction_* / TestSetActionHandler_*（NoHandler / DispatchesToHandler / HandlerFalse / NilClears）—— 被删 API 的测试 |
| 改 `chatsession_test.go` 任何 `cs.SetActionHandler` / `cs.HandleAction` 引用 | 0 命中（grep 验证） |
| 改 `manager_test.go` 任何 `SetActionHandler` 引用 | 0 命中 |

### 2.4 `internal/gateway/` 调整

| 文件 | 改动 |
|---|---|
| `gateway.go` | `gateway.Gateway` 接口加 `WithCommander(dispatch DispatchFunc) Gateway`（`DispatchFunc = func(ctx, *InboundMessage) (*CommandResult, error)`，**不**接 `command.Commander` interface——见 §1.2.7 翻译规约）；`dispatchInbound` 路径：若 `msg.Text` 以 `/gtw` 开头，调 `dispatch(ctx, msg)`（shim 由 `cmd/nightme/run.go` 提供）；`WithActionHandler` 行为不变（runtime 注入的 closure 调 `rt.ReactionRouter.Handle`，不再调 `cs.HandleAction`） |
| `dispatch_action_test.go` | 测 `ReactionRouter` 路径：从 `gw.WithActionHandler` 注 closure → closure 调 `rt.ReactionRouter.Handle` → router 派发到 `gtwMgr.HandleReaction`（fake） → 验证 return 值 |
| `handlers_chatsession.go` / `handlers_new.go` / `handlers_watch.go` / `handlers_think.go` / `handlers_tools.go` | **本 PR 不动**（留 §0.16 batch 5+） |

### 2.5 `internal/channel/feishu/adapter.go`

| 改动 | 说明 |
|---|---|
| `handleActCardAction` 第 3375 行的 `gateway.ReactionEvent{TargetMsgID, Emoji, UserID, ChatID}` 改为 `command.ReactionEvent{...}` | `gateway.ReactionEvent` 是 `chatsession.ReactionEvent` 的 type alias（`internal/gateway/messages.go:199`）；alias 跟 `chatsession.ReactionEvent` 一起删，统一用 `command.ReactionEvent` |
| `internal/channel/feishu/reaction_test.go` 的 `chatsession.ReactionEvent` 引用 | 0 命中（grep 验证） |

### 2.6 `cmd/nightme/` 装配

| 文件 | 改动 |
|---|---|
| `run.go` | (1) `gtwMgr := gtw.NewManager()` (2) `router := command.NewReactionRouter()` (3) `router.Register("*", gtwMgr.HandleReaction)` (4) `reg := command.NewRegistry(); reg.Register(gtw.NewFactory(gtwMgr))` (5) `commander := command.NewCommander(reg)` (6) `var _ command.Channel = (*gateway.Channel)(nil)` 编译期断言 (7) `rt := command.RuntimeServices{Session: sessionAdapter{chatMgr, cfg.Primary}, ReactionRouter: router, Channel: ch}` (8) `gw.WithCommander(slashDispatchFunc)` —— `slashDispatchFunc` 是 shim：`func(ctx, gwMsg) (*gwResult, error) { in := command.SlashInput{...}; out, _ := commander.Dispatch(ctx, rt, in); return gwFromCommand(out) }` (9) `gw.WithActionHandler` closure 调 `rt.ReactionRouter.Handle`（取代原 `cs.HandleAction`） |
| `debug.go` | 同 run.go 装配方式；`nightme gtw list` / `nightme gtw reset` 等 debug 子命令改用 `gtwMgr` 直接调 Manager（不再经 `cs.ListGTWDrafts`） |
| `run_test.go` | 任何 `WithActionHandler → cs.HandleAction` 测试改成 `→ rt.ReactionRouter.Handle → gtwMgr.HandleReaction` |
| `debug.go` 的 `gtwDrafts` / `gtwContext` 输出 | 改用 `gtwMgr` |

---

## 3. 不变式（§1.3 / §1.4 强化）

### 3.1 import 表残留（chatsession 视角）

F-51 落地后 `internal/chatsession/*.go` 的 import 表**只允许**出现：

```go
context
encoding/json
errors
fmt
log/slog
sort
strings
sync
sync/atomic
time
github.com/cnlangzi/nightme/internal/agent
github.com/cnlangzi/nightme/internal/registry
```

**禁止**出现的标识符（grep 验证）：
- `"github.com/cnlangzi/nightme/internal/command"` 或其子包路径
- `"github.com/cnlangzi/nightme/internal/gtw"`
- `"github.com/cnlangzi/nightme/internal/gateway"`
- 任何文件 / 类型 / 方法名含 `GTW` / `gtw` / `Command`（除非注释自指"F-51 删除项"）

### 3.2 跨层 import 箭头

```
Concrete impl (chatsession / gtw / channel)
       ↑ implements
Service interface (command/services/*.go)
       ↑ implements
Command abstraction (command/<name>/commands.go + Commander/SlashCommandFactory)
       ↑ uses
Gateway + Channel top layer
```

**单向**。下层 import 上层即破不变式。F-51 验证：
- `chatsession` 不 import `command/` / `internal/gtw` / `internal/gateway` ✓
- `internal/command/gtw` 不 import `chatsession` / `internal/gateway` ✓
- `internal/command/services` 不 import `chatsession` / `internal/command/gtw` / `internal/gateway` ✓（只依赖 stdlib + `command/event.go`）—— **adapter 搬到 `cmd/nightme/session_adapter.go`**（§1.2.5）
- `internal/command/gtw/commands.go` 不 import `chatsession`（用 `RuntimeServices.Session` 接口）✓
- `cmd/nightme/` 是唯一同时持 chatsession / gtw / channel 具体实现的装配者 ✓

### 3.3 reaction 路径

- `cs.HandleAction` / `cs.SetActionHandler` / `cs.ActionHandler` 整套 API **不存在**
- 任何代码出现这三个调用 → review 立即打回（grep 应 0 命中）
- reaction 一律走 `rt.ReactionRouter.Handle(ctx, chatID, ev)`

---

## 4. 4 批次迁移计划（详见 SPEC §0.16）

| Batch | 主题 | 状态 |
|---|---|---|
| B0 | 文档：写 feat doc + SPEC §0.16 + 清「（待补）」 | ✅ 本 PR 落地 |
| B1 | `internal/command/` 骨架（commander / factory / runtime / services / event / preflight / reply） | ✅ 本 PR 落地 |
| B2 | gtw 整体迁移：删 `internal/gtw/` + 拆 `internal/gateway/handlers_gtw*.go`，落到 `internal/command/gtw/`（含 Manager + commands + debug + 6 个原生类型） | ✅ 本 PR 落地 |
| B3 | chatsession 收尾：删 2 文件 + 7 类型 + 3 方法 + 2 字段 + 1 reaction 字段；Gateway `/gtw` 切到 Commander；装配切到 ReactionRouter | ✅ 本 PR 落地（+ B3+ 修 Sender 未注册 bug） |
| B5+ | 其余 7 个 slash command（`/cwd` `/use` `/kill` `/new` `/watch` `/think` `/tools`）搬到 `internal/command/<name>/` | 📝 后续 PR（§0.16 写明） |

---

## 5. 测试矩阵

### 5.1 单元测试（本 PR 必跑）

| 套件 | 验证 |
|---|---|
| `go test ./...` | 全 999 仍通过 + 新增 commander / factory / reaction router / gtw manager 测试通过 |
| `go vet ./...` | 0 warning |
| `golangci-lint run` | 0 issue（如果 CI 启用） |

### 5.2 灰度测试（本 PR 必做）

- `nightme gtw list` —— `gtwMgr.List(chatID)` 替代 `cs.ListGTWDrafts()`
- `nightme gtw reset` —— `gtwMgr.Reset(chatID)` 替代 `cs.ClearGTWDrafts() + cs.ClearGTWContext()`
- `nightme gtw test <scenario>` —— 端到端：seed draft → send card → 反应 PATCH

### 5.3 UAT（Feishu 真环境，不在本 PR review 范围）

- 决策卡点击 ✅ / 🆕 / 🔗 / ❌ / 🔄 / 🤝 → 路由走 `ReactionRouter` → `gtwMgr.HandleReaction` → PATCH 原卡
- 多 chat 并发：chatA 反应 → 只派发到 chatA 的 gtw 状态；不影响 chatB

---

## 6. 风险与回滚

| 风险 | 概率 | 缓解 |
|---|---|---|
| `gtw_test.go` 重写引入回归 | 中 | 保留核心场景（`TestHandleAction_*` / `TestRunFix_*`）的语义不变；原 39 KB 测试的子集先迁，剩余的 batch 5+ 补 |
| `cmd/nightme/run.go` 装配顺序写错导致 `router.Register` 在 `WithActionHandler` 之前被忘 | 低 | 启动日志在装配完成后打 `slog.Info("command: gtw registered", ...)`；缺这条日志即 panic |
| `chatsession.go` 删字段后某个测试或 hook 隐式引用 | 低 | `go build ./...` 编译期全捕；`go test ./...` 跑 999 验证 |
| 其余 7 个 slash command 暂时还能用旧路径 | 0 | 不在 B0-B3 范围，无回归；后续 batch 5+ 处理 |
| `gateway.ReactionEvent` type alias 删后，feishu adapter 漏改 | 低 | grep 验证 `gateway.ReactionEvent` 0 命中 |

**回滚**：本 PR 一个 commit（squash 4 个 batch）；回滚 = `git revert <merge-sha>`。`chatsession` 的旧字段在 revert 后会复活（因为旧 commit 也存在分支上）。

### B3+ 修复（P0 — gtw.Manager.SetSender 修）

**问题**：F-51 落地后运行时从未调用 `gtwMgr.SetSender(chatID, ...)`；`/gtw fix` 和 reaction 路径都依赖 `m.GetSender(chatID)` 拿 Sender 发消息，原设计假定 runtime 启动时主动注册每 chat 的 Sender，但实际没人调——`/gtw fix` 和决策卡点 ✅/🆕/🔗 都会 nil deref。

**修法**（已在当前代码中落地）：
- `Manager.SetSenderFactory(func(chatID) Sender)` 字段 + 懒构造：未注册 Sender 时调 factory，结果缓存
- `cmd/nightme/gtw_sender_adapter.go::chatSessionSender` 适配器：把 SessionService + Channel 转成 `gtw.Sender`
- `cmd/nightme/run.go` 启动时 `gtwMgr.SetSenderFactory(func(chatID) gtw.Sender { return newChatSessionSender(chatID, newSessionAdapter(mgr, cfg.Primary).GetOrCreate(chatID, cfg.Primary), newChannelAdapter(ch)) })`

**测试覆盖**：`Manager.SetSenderFactory` 4 case（lazy create / nil fallback / 未安装 / 并发安全）。`Factory` 8 case（Spec + 6 个 subcommand 路径）。**总计 25 passed in `internal/command/gtw/`**。

### B3+ 后续建议

- **P1 继续补测试**：`fix.go::RunFix` / `action.go::HandleDraftReaction` / `rebuild.go::RebuildContext` / `render.go::BranchExistsCard` 还没测试。这些要 mock HandlerDeps 全部字段（Send / SendCard / Git / Prober / Detect / Now），是 ~400-600 行测试代码，1 个集中 session 写完。
- **B5+（其余 7 个 slash command 迁移）**：照 `gtw.Factory` 模式搬到 `internal/command/cwd/commands.go` 等。Gateway 的 `WithCommander` 已经接好，新增的 command 直接 `reg.Register(...)` 即可。预计 1 个独立 PR，30-60 分钟。

---

## 7. 与既有 feat 的关系

| Feat | 关系 |
|---|---|
| F-45 (session footer) | F-45 §3.4 把 `gtwDrafts` payload 设计得「narrow」——F-51 搬到 `gtw.Manager` 后，payload 形状不变 |
| F-46 (interactive cards) | §2.6 决策卡 → reaction 的归一化不变；F-51 §2.7 接管 reaction 分发 |
| F-49 (compaction counter) | 无关 |
| F-50 (GitProvider) | F-50 先把 `internal/gtw/provider.go` 抽象做好；F-51 整体迁 `provider.go` 到 `command/gtw/provider.go`，无内容改动 |
| F-44 (independent reply + task receipt) | 无关 |
| F-26 (gateway hub) | Gateway 的 `dispatchInbound` 路径调整（slash 走 commander，reaction 走 router），`Gateway` 接口**新增** `WithCommander` 方法（接 `DispatchFunc` 而非 `command.Commander` interface，避开 gateway → command 的 import 依赖——见 §1.2.7） |
| F-20 (gateway) | §5.x 不变式"command 路由归 Gateway"放宽为"command 路由归 Commander（Gateway 持引用）"——这就是 F-51 的核心目的 |

---

## 8. 不在 F-51 范围（明确说"不做"）

1. `chat_sessions.json` / `agent_sessions.json` 持久化层迁出 `internal/registry` —— 后续 batch
2. `/gtw` 历史 on-disk 状态 schema 升级 —— gtw 状态本就是 in-memory only，daemon 重启走 `RebuildContext`；F-51 不动 schema
3. `cmd/nightme/gtw` 子命令（如 `nightme gtw list`）的命令名空间化 —— 维持现状
4. 其余 7 个 slash command 迁移 —— B5+ 后续 PR
5. `chatsession.AgentSession` 接口化 —— 仍以具体类型给 `command/gtw` 用，但通过 `command/services.Session.AgentSession` 接口 return
