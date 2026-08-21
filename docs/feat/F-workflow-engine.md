# nightme Workflow 引擎（wfe 库 + bot subsystem host）

> **Source**: 新功能（2026-08-18），无历史 F-XX 来源
> **Depends on**: F-08 (Channel interface), F-09 (Agent abstraction), F-20 (Gateway), F-50 (GitProvider)
> **Related docs**: [`../WFE.md`](../WFE.md)（用户参考——YAML schema / 触发器 / step 形式）；[`../SPEC.md`](../SPEC.md) §2 数据流

本文件是 workflow 引擎的**实现设计**。YAML 怎么写、step 长啥样、`${{ }}` 怎么求值——这些都在 `WFE.md`。本文件回答的是：包怎么拆、接口怎么定、状态怎么存、测试怎么写、灰度怎么上。

---

## 1. 设计目标

构建一个 GitHub-Actions-风格的本地工作流引擎，跑在 nightme 自己的 runtime 上。核心约束：

- **不能绕过 Gateway 直接调 agent**。`prompt` 步骤必须走 `bot → gw → ChatSession → AgentSession`。
- **wfe 是纯库**。无 I/O、无 clock、无 secrets——所有外部能力由 host（bot）注入。
- **bot 是 host**。加载 workflow、装 trigger、驱动 Tick 循环、注入 Runtime、持久化状态。
- **可扩展**。`run` / `prompt` 是默认必带；`use` 是 bot 注入 action 的扩展点。

---

## 2. 包布局

```
internal/
├── wfe/                              # 纯库（无 I/O）
│   ├── types.go                      # Workflow / Step / Event / RunState / Spec 类型
│   ├── load.go                       # YAML 解析 + schema 校验
│   ├── match.go                      # Event -> *Workflow 匹配（纯函数）
│   ├── tick.go                       # 状态机：Tick(state, runtime) -> (state', error)
│   ├── expr.go                       # ${{ }} 求值
│   ├── runtime.go                    # Runtime interface（4 方法 = 4 channel 入口）
│   ├── errors.go                     # 错误类型（ValidationError / UnknownActionError / …）
│   └── internal_test.go              # 纯单元测试
│
└── channel/bot/                      # Channel 实现（跟 feishu/echo 平级，注册到 gateway）
    ├── bot.go                        # Channel interface 实现（Name/Start/Stop/Incoming/Send）
    ├── workflow.go                   # fireWorkflow + driveRun（per-run 编排）
    ├── runtime.go                    # botRuntime（implements wfe.Runtime, channel router）
    ├── trigger.go                    # TriggerManager（cron + git events, 3 阶段管线）
    ├── ws_map.go                     # WorkspaceRepoMap（repo URL → workspace path）
    ├── action.go                     # ActionRegistry + Action interface
    ├── action_builtin.go             # 内置 actions（notify / email / github_* / slack / webhook）
    ├── action_shell.go               # ShellAction（user-script channel）
    ├── state.go                      # StateStore（state dir 持久化）
    └── *                              # 集成测试
```

依赖方向严格单向：

```
internal/wfe ── (no deps on bot)
       ▲
       │ uses Runtime interface (4 methods, all channel entries)
       │
internal/channel/bot ──► internal/gateway (bot 注册到 AttachChannels)
                       └► internal/gitprovider (借, trigger events)
                       └► internal/registry (state dir)
                       └► internal/channels (借, notify action)
```

**关键**：bot **不依赖** `internal/chatsession` 和 `internal/agentsession`。这些由 nightme gateway 内部使用；bot 走 channel 协议，gateway 负责把这些都搞定。

wfe 不 import bot 的任何东西。bot import wfe + nightme 的 channel/gateway 体系。这是单测无敌干净的关键。

---

## 3. wfe 库（`internal/wfe/`）

### 3.1 核心类型（`types.go`）

```go
// Workflow 是解析+校验后的工作流定义。
type Workflow struct {
    Name       string         `yaml:"name" validate:"required"`
    Workspaces []string       `yaml:"workspaces" validate:"required,min=1"`
    Worker     int            `yaml:"worker" default:"1"`
    On         Trigger        `yaml:"on" validate:"required"`
    Jobs       map[string]Job `yaml:"jobs" validate:"required,min=1"`

    // 解析期填充，运行期不读
    _order     []string       // jobs 解析序
}

type Job struct {
    Needs  []string `yaml:"needs"`
    If     string   `yaml:"if"`
    Steps  []Step   `yaml:"steps" validate:"required,min=1"`
}

type Step struct {
    Name             string            `yaml:"name"`
    ID               string            `yaml:"id"`
    If               string            `yaml:"if"`
    Env              map[string]string `yaml:"env"`
    ContinueOnError  bool              `yaml:"continue-on-error"`

    // 互斥三选一
    Run     string            `yaml:"run"`
    Prompt  string            `yaml:"prompt"`
    Use     string            `yaml:"use"`
    With    map[string]any    `yaml:"with"`
    Agent   string            `yaml:"agent"`        // 仅 prompt
    Shell   string            `yaml:"shell"`        // 仅 run
}

type Trigger struct {
    Schedule    []ScheduleEntry  `yaml:"schedule"`
    PullRequest *PRTrigger       `yaml:"pull_request"`
    Branch      *BranchTrigger   `yaml:"branch"`
    Issue       *EventFilter     `yaml:"issue"`
    Mention     *MentionTrigger  `yaml:"mention"`
}

type PRTrigger struct {
    Branches []string `yaml:"branches"`
    Events   []string `yaml:"events"`
}

// ... 其余 Trigger 字段见 WFE.md §3

type Event struct {
    Kind string         // "schedule" | "pull_request" | "branch" | "issue" | "mention"
    Time time.Time      // 注入自 Runtime.Now()
    Data map[string]any // 各 trigger 原始 payload
}

// RunState 是 Tick 的输入+输出。Bot 持有并持久化。
type RunState struct {
    RunID         string                       `json:"run_id"`
    WorkflowName  string                       `json:"workflow"`
    Workspace     string                       `json:"workspace"`     // 当前 run 的 workspace path（从 wf.Workspaces 挑出）
    Status        Status                       `json:"status"`        // running | succeeded | failed | cancelled
    CurrentJob    string                       `json:"current_job"`
    CurrentStep   string                       `json:"current_step"`
    Env           map[string]string            `json:"env"`           // 含 bot 注入的 secrets
    StepOutputs   map[string]map[string]string `json:"step_outputs"`  // step_id -> outputs
    Event         Event                        `json:"event"`
    ChatID        string                       `json:"chat_id"`
    StartedAt     time.Time                    `json:"started_at"`
    UpdatedAt     time.Time                    `json:"updated_at"`
    Attempts      map[string]int               `json:"attempts"`      // step_id -> attempt count
}

type Status string

const (
    StatusRunning   Status = "running"
    StatusSucceeded Status = "succeeded"
    StatusFailed    Status = "failed"
    StatusCancelled Status = "cancelled"
)
```

### 3.2 Loader（`load.go`）

```go
func LoadDir(dir string) ([]*Workflow, error) {
    files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
    if err != nil { return nil, err }
    
    var out []*Workflow
    seen := map[string]string{} // name -> 文件名，用于重名检测
    for _, f := range files {
        wf, err := Load(f)
        if err != nil { return nil, fmt.Errorf("%s: %w", f, err) }
        if prev, ok := seen[wf.Name]; ok {
            return nil, fmt.Errorf("duplicate workflow name %q in %s and %s", wf.Name, prev, f)
        }
        seen[wf.Name] = f
        out = append(out, wf)
    }
    return out, nil
}

func Load(path string) (*Workflow, error) {
    raw, err := os.ReadFile(path)
    if err != nil { return nil, err }
    return Parse(raw)
}

func Parse(raw []byte) (*Workflow, error) {
    var wf Workflow
    if err := yaml.Unmarshal(raw, &wf); err != nil { return nil, err }
    if err := validate(&wf); err != nil { return nil, err }
    wf._order = topoSort(wf.Jobs)  // jobs 解析序（needs 反向拓扑）
    return &wf, nil
}
```

`validate()` 用 `go-playground/validator`（nightme 已用）做：
- 必填字段
- `step.run` / `step.prompt` / `step.use` 三选一（不能同时多个）
- `step.agent` 仅在 `prompt` 下出现
- `step.shell` 仅在 `run` 下出现
- `use` 名字以字母开头、只含 `[a-z0-9_-]`
- `cron` 5 字段合法
- `event` 名字在白名单内

### 3.3 Matcher（`match.go`）

纯函数：给定 `Event` 和 `*Workflow`，决定是否触发。

```go
func Match(wf *Workflow, ev Event) bool {
    switch ev.Kind {
    case "schedule":
        return wf.On.Schedule != nil  // cron 已经在 trigger 阶段评估
    case "pull_request":
        t := wf.On.PullRequest
        if t == nil { return false }
        return matchList(t.Branches, ev.Data["branch"]) &&
               matchList(t.Events,   ev.Data["action"])
    case "branch":
        t := wf.On.Branch
        if t == nil { return false }
        return matchGlobs(t.Patterns, ev.Data["name"]) &&
               matchList(t.Events,   ev.Data["action"])
    case "issue":
        if wf.On.Issue == nil { return false }
        return matchList(wf.On.Issue.Events, ev.Data["action"])
    case "mention":
        if wf.On.Mention == nil { return false }
        cmds := wf.On.Mention.Commands  // []string
        if len(cmds) == 0 { return true }   // 无白名单 = 任意
        return slices.Contains(cmds, ev.Data["command"])
    }
    return false
}
```

### 3.4 Tick 状态机（`tick.go`）

**这是 wfe 唯一会改变 state 的地方。** 每步：state in → state out。

```go
type TickResult int

const (
    TickAdvanced TickResult = iota
    TickDone
    TickWaiting  // prompt 步骤的 SendPrompt 在异步等回复（v0 不开，纯同步）
)

func Tick(ctx context.Context, state *RunState, wf *Workflow, rt Runtime) (*RunState, error) {
    state.UpdatedAt = rt.Now()
    
    // 1. 已失败 / 已完成 → no-op
    if state.Status != StatusRunning {
        return state, nil
    }
    
    // 2. 找下一个该跑的 step
    step, jobName, ok := nextStep(wf, state)
    if !ok {
        state.Status = StatusSucceeded
        return state, nil
    }
    
    state.CurrentJob = jobName
    
    // 3. 表达式求值上下文
    ec := ExprCtx{
        Event: state.Event.Data,
        Steps: state.StepOutputs,
        Needs: collectNeedsOutputs(wf, state),
        Env:   state.Env,
        Now:   rt.Now(),
    }
    
    // 4. if 条件
    if step.If != "" {
        ok, err := EvalCond(step.If, ec, rt)
        if err != nil { return state, wrapStepErr(step.ID, err) }
        if !ok {
            state.Advance(step.ID)  // 跳过
            return state, nil
        }
    }
    
    state.CurrentStep = step.ID
    
    // 5. step env 覆盖
    stepEnv := mergeEnv(state.Env, step.Env)
    
    // 6. 分发
    var (
        outputs map[string]string
        err     error
    )
    switch {
    case step.Run != "":
        cmd := EvalString(step.Run, ec, rt)
        var r *ShellResult
        r, err = rt.RunShell(ctx, ShellSpec{
            Cwd:     state.Env["WORKSPACE_CWD"],
            Command: cmd,
            Env:     stepEnv,
            Shell:   step.Shell,
        })
        if r != nil { outputs = r.Outputs }
        
    case step.Prompt != "":
        prompt := EvalString(step.Prompt, ec, rt)
        var r *Reply
        r, err = rt.SendPrompt(ctx, PromptSpec{
            ChatID:  state.ChatID,
            Agent:   pickAgent(step.Agent, state.Env),
            Prompt:  prompt,
            Env:     stepEnv,
        })
        if r != nil { outputs = r.Outputs }
        
    case step.Use != "":
        with := EvalMap(step.With, ec, rt)
        var r *ActionResult
        r, err = rt.RunAction(ctx, ActionSpec{
            Name: step.Use,
            With: with,
            Env:  stepEnv,
        })
        if r != nil { outputs = stringifyMap(r.Outputs) }
    }
    
    // 7. 处理结果
    if err != nil {
        if step.ContinueOnError {
            // 记录错误为输出，不 fail
            if outputs == nil { outputs = map[string]string{} }
            outputs["error"] = err.Error()
        } else {
            state.Status = StatusFailed
            state.StepOutputs[step.ID] = map[string]string{"error": err.Error()}
            return state, wrapStepErr(step.ID, err)
        }
    }
    
    if outputs != nil {
        if state.StepOutputs == nil { state.StepOutputs = map[string]map[string]string{} }
        state.StepOutputs[step.ID] = outputs
    }
    state.Attempts[step.ID]++
    
    return state, nil
}
```

`nextStep` 实现：按 job 顺序、同 job 内按 step 顺序，忽略 `if: false` 已跳过的、忽略 `needs` 未满足的。

### 3.5 表达式（`expr.go`）

```go
type ExprCtx struct {
    Event map[string]any
    Steps map[string]map[string]string
    Needs map[string]map[string]string
    Env   map[string]string
    Now   time.Time
}

type Runtime interface {
    // 表达式用到的两样外部能力
    Secret(ctx, key string) (string, error)  // 实际从 Env 读，不做 secret 区分
    Now() time.Time
}

func EvalString(s string, ec ExprCtx, rt Runtime) string { ... }
func EvalMap(m map[string]any, ec ExprCtx, rt Runtime) map[string]any { ... }  // 递归
func EvalCond(s string, ec ExprCtx, rt Runtime) (bool, error) { ... }
```

实现用 `text/template`（标准库），自定义 func：

| Func | 返回 |
|------|------|
| `event.X` | `ec.Event["X"]` |
| `steps.foo.outputs.bar` | `ec.Steps["foo"]["bar"]` |
| `needs.review.outputs.bar` | `ec.Needs["review"]["bar"]` |
| `env.GH_TOKEN` | `ec.Env["GH_TOKEN"]`（含 bot 注入的 secrets）|
| `success()` | 当前 job 的所有 needs 都成功 |
| `failure()` | 至少一个 need 失败 |
| `always()` | true |

wfe **不做 secret 区分**——`${{ env.GH_TOKEN }}` 和 `${{ env.WORKDIR }}` 走完全一样的 lookup。Secret 的 redaction 在 bot 的 logger 层做。

### 3.6 Runtime interface（`runtime.go`）—— resource channels

```go
package wfe

// Runtime 是 wfe 看外部世界的"channel 集合"。
// 每个方法是 bot 到一个具体资源的 channel：
//   RunShell   → shell channel   → os/exec
//   SendPrompt → nightme channel → bot.Incoming() (gateway does the rest)
//   RunAction  → action channels → ActionRegistry (按 name 解析)
//   Now        → clock           → time.Now (非 channel，仅解 clock 依赖)
type Runtime interface {
    RunShell(ctx context.Context, spec ShellSpec) (*ShellResult, error)
    SendPrompt(ctx context.Context, spec PromptSpec) (*Reply, error)
    RunAction(ctx context.Context, spec ActionSpec) (*ActionResult, error)
    Now() time.Time
}

type ShellSpec struct {
    Cwd     string
    Command string
    Env     map[string]string
    Shell   string            // "bash" / "sh" / "" (host default)
}

type PromptSpec struct {
    ChatID string
    Agent  string
    Prompt string
    Env    map[string]string
}

type ActionSpec struct {
    Name string
    With map[string]any
    Env  map[string]string
}

type ShellResult struct {
    ExitCode int
    Stdout   string
    Stderr   string
    Outputs  map[string]string  // 当前 v0：只支持从 stdout 解析 `key=value` 行
}

type Reply struct {
    Text    string
    Outputs map[string]string
}

type ActionResult struct {
    Outputs map[string]any
}
```

wfe 不知道这些 Spec 的实现——可能是 os/exec，可能是 nightme client，可能啥都没有（mock 测试）。这就是单测无敌干净的关键。

**为什么 nightme 是一条 channel 而不是 wfe 内部事**：nightme 只是 bot 用来执行 LLM 任务的一种资源。bot 还用 shell、github、slack 等其他资源。wfe 把这些一视同仁——都是通过 Runtime interface 注入的 channel。nightme 不比其它 channel 特殊，只是最重（跨进程、跑 LLM）。

---

## 4. bot channel（`internal/channel/bot/`）

> **🔒 Design invariant (locked)**: bot does **not** import `internal/chatsession`, `internal/agentsession`, `internal/gateway` (as a caller), or any other nightme internal package. bot's **only** nightme-facing surface is `channel.Channel` — specifically, pushing messages into `bot.Incoming()` and receiving them via `bot.Send()`. `/gtw fix`, `/cwd`, `/use agent` are all invoked by sending the corresponding slash-command messages; bot never calls them as Go functions.
>
> **Outgoing side-effects** (`notify`, `email`, `github_*`, `webhook`) are bot's **own** resource channels — they call external APIs directly. The constraint only covers bot → nightme runtime; bot → external is free.

**关键定位**：`bot` **实现 `channel.Channel` interface**，跟 `feishu` / `echo` 平级。bot 注册到 `gateway.AttachChannels`，通过完整的 channel 协议（Incoming → gateway → ChatSession → agent → reply → Emitter → Send）与 nightme 交互。bot **不直接接触** `chatsession.Manager` 或 `ChatSession`——这些都通过 channel 协议间接使用。

| nightme 子系统 | 类型 | 跟 gw 的关系 |
|---|---|---|
| `feishu` | `Channel` 实现 | gw 泵 Incoming → dispatch chain → 真人 feishu 用户 |
| `echo` | `Channel` 实现（test）| 同上 → 测试驱动 |
| `bot` | **`Channel` 实现** | 同上 → **workflow YAML 触发器** |
| `/gtw` | command subsystem | 持 `chatsession.Manager` 引用（`/gtw` 是 nightme 的内部 client，**跟 bot 不是一类**）|

**bot 跟 feishu / echo 走完全一样的协议**——gateway 不知道 feishu 和 bot 的区别，只知道"又有一个 channel 发消息过来了"。

### 4.1 Bot implements channel.Channel（`bot.go`）

```go
// Bot is a channel.Channel implementation. It feeds workflow
// trigger events into its own Incoming(); the gateway's
// dispatchLoop reads them and routes through inbound.Dispatch.
// Agent replies come back through bot.Send (called by the
// gateway's outbound.Emitter).
type Bot struct {
    workflows []*wfe.Workflow
    triggers  *TriggerManager        // cron + git events, all internal
    actions   *ActionRegistry
    stateStore *StateStore

    // per-run state, indexed by chatID for reply routing
    runsByChatID map[string]*botRun

    // channel protocol (we ARE a channel)
    incoming chan channel.Message    // bot pushes synthesized msgs here
    log      *slog.Logger

    // borrowed dependencies
    gitProvider GitProvider            // 4 个 git 触发器
    channels    *channel.Registry      // for notify action
}

// Compile-time check: Bot satisfies channel.Channel
var _ channel.Channel = (*Bot)(nil)

// --- channel.Channel interface ---

func (b *Bot) Name() string { return "bot" }

func (b *Bot) Start(ctx context.Context) error {
    // 1. load workflows
    wfs, err := wfe.LoadDir(b.cfg.WorkflowsDir)
    if err != nil { return err }
    b.workflows = wfs

    // 2. build workspace → repo map (for trigger filtering)
    b.wsMap = NewWorkspaceRepoMap()
    if err := b.wsMap.Build(wfs); err != nil { return err }

    // 3. register action channels
    b.actions.Register(builtin.NewNotify(b.channels))
    b.actions.Register(builtin.NewEmail(b.smtp))
    b.actions.Register(builtin.NewGitHub(b.gitProvider))
    // ... etc
    if err := b.actions.ScanUserScripts(filepath.Join(b.cfg.WorkflowsDir, "actions")); err != nil {
        return err
    }

    // 4. start trigger manager
    return b.triggers.Start(ctx)
}

func (b *Bot) Stop(ctx context.Context) error {
    return b.triggers.Stop(ctx)
}

// Incoming returns the channel from which the gateway's pumpInbound
// reads. bot pushes synthesized messages (workflow prompts, /cwd
// setups, /use agent calls, etc.) here. gateway treats them like
// any other channel's messages.
func (b *Bot) Incoming() <-chan channel.Message {
    return b.incoming
}

// Send is called by the gateway's outbound.Emitter when an agent
// reply is ready. bot looks up the botRun that owns chatID and
// delivers the reply to that run's reply channel.
func (b *Bot) Send(ctx context.Context, msg messages.OutboundMessage) error {
    r, ok := b.runsByChatID[msg.ChatID]
    if !ok {
        // Stale reply (run already finished). Drop.
        return nil
    }
    select {
    case r.reply <- msg.Text:
    default:
        // reply channel full; drop and log
        b.log.Warnf("bot: reply channel full for chatID=%s", msg.ChatID)
    }
    return nil
}
```

### 4.2 Per-run state（`workflow.go`）

```go
// botRun is the per-workflow-run state. Lives in bot.runsByChatID
// for the lifetime of one run.
type botRun struct {
    runID    string
    chatID   string
    workspace string              // local main directory this run operates in
    workflow  *wfe.Workflow
    env      map[string]string    // merged env (bot defaults + workflow env + step env)

    // reply channel: workflow run goroutine blocks on this;
    // bot.Send delivers the agent reply here.
    reply    chan string
}

func (b *Bot) fireWorkflow(ctx context.Context, wf *wfe.Workflow, ev wfe.Event, workspace string) {
    runID := makeRunID(wf.Name, ev, workspace)
    chatID := "bot:wf:" + runID                  // unique per run

    // worker pool
    if !b.workerPool.Acquire(wf.Name, runID) {
        b.log.Warnf("worker pool full for %s, dropping trigger %s", wf.Name, runID)
        return
    }

    // 1. 装 chat setup messages: /cwd + (optionally) /use agent + /gtw fix or first prompt
    setupMsgs := b.buildSetupMessages(wf, workspace, ev)
    // (see §4.5 for the details)

    r := &botRun{
        runID:     runID,
        chatID:    chatID,
        workspace: workspace,
        workflow:  wf,
        env:       mergeEnv(b.defaultEnv(), wf.Env),
        reply:     make(chan string, 1),
    }
    b.runsByChatID[chatID] = r
    defer func() {
        delete(b.runsByChatID, chatID)
        b.workerPool.Release(wf.Name, runID)
    }()

    // 2. push setup messages to bot.Incoming() — gateway will dispatch
    for _, m := range setupMsgs {
        b.incoming <- m
    }

    // 3. drive wfe.Tick
    b.driveRun(ctx, r)

    // 4. cleanup
    // Note: bot does NOT call /gtw close. The worktree is the
    // workflow's responsibility — the workflow can include a step
    // that sends "/gtw close <key>" as a prompt body, or the user
    // can manually run /gtw close after the workflow finishes. bot
    // has no opinion on this; its only job is to deliver messages
    // through the channel.
}
```

### 4.3 botRuntime（`runtime.go`）—— channel router

`botRuntime` 实现 `wfe.Runtime` 4 个方法，**每个方法都是 bot 端对应 channel 的入口**：

```go
type botRuntime struct {
    bot    *Bot
    run    *botRun   // current run, for reply channel + workspace lookup
}

func (r *botRuntime) Now() time.Time { return time.Now() }

func (r *botRuntime) RunShell(ctx context.Context, spec wfe.ShellSpec) (*wfe.ShellResult, error) {
    cwd := spec.Cwd
    if cwd == "" { cwd = r.run.workspace }

    shellBin, shellArg := pickShell(spec.Shell)
    cmd := exec.CommandContext(ctx, shellBin, shellArg, spec.Command)
    cmd.Dir = cwd
    cmd.Env = envToUnix(spec.Env)

    var stdout, stderr bytes.Buffer
    cmd.Stdout = &stdout
    cmd.Stderr = &stderr

    err := cmd.Run()
    res := &wfe.ShellResult{
        Stdout:  stdout.String(),
        Stderr:  stderr.String(),
        Outputs: parseKVOutput(stdout.String()),
    }
    if exitErr, ok := err.(*exec.ExitError); ok {
        res.ExitCode = exitErr.ExitCode()
        return res, err
    }
    res.ExitCode = 0
    return res, nil
}

// SendPrompt is the nightme channel entry: push the prompt into
// bot.Incoming(), then block on the per-run reply channel.
// When bot.Send delivers the agent's reply, this returns.
func (r *botRuntime) SendPrompt(ctx context.Context, spec wfe.PromptSpec) (*wfe.Reply, error) {
    // 1. push the prompt message into bot.Incoming()
    msg := channel.Message{
        ChatID: spec.ChatID,
        Body:   spec.Prompt,
    }
    select {
    case r.bot.incoming <- msg:
    case <-ctx.Done():
        return nil, ctx.Err()
    }

    // 2. block on the per-run reply channel (delivered by bot.Send)
    select {
    case text := <-r.run.reply:
        return &wfe.Reply{Text: text}, nil
    case <-ctx.Done():
        return nil, ctx.Err()
    case <-time.After(spec.Timeout):
        return nil, wfe.ErrPromptTimeout
    }
}

func (r *botRuntime) RunAction(ctx context.Context, spec wfe.ActionSpec) (*wfe.ActionResult, error) {
    return r.bot.actions.Run(ctx, spec)
}
```

`botRuntime` 是 wfe 的视角——它看到的是 4 个 channel 入口。**实际 channel 怎么实现（os/exec / nightme channel / action channels）由 bot 的字段决定**，不在 wfe 关心范围内。

### 4.4 Per-run drive loop（`workflow.go`）

```go
func (b *Bot) driveRun(ctx context.Context, r *botRun) {
    // Load or init state
    state, err := b.stateStore.Load(r.runID)
    if errors.Is(err, os.ErrNotExist) {
        state = wfe.NewRunState(r.runID, r.workflow.Name, r.runID, r.chatID, r.workspace, r.env, time.Now())
    } else if err != nil {
        b.log.Errorf("load state %s: %v", r.runID, err)
        return
    }

    // Tick loop: bot owns this, wfe only advances one step per call
    for state.Status == wfe.StatusRunning {
        newState, err := wfe.Tick(ctx, state, r.workflow, &botRuntime{bot: b, run: r})
        if err != nil {
            b.log.Errorf("tick %s: %v", r.runID, err)
        }
        state = newState
        if perr := b.stateStore.Save(state); perr != nil {
            b.log.Errorf("save state %s: %v", r.runID, perr)
        }
        if ctx.Err() != nil {
            state.Status = wfe.StatusCancelled
            b.stateStore.Save(state)
            return
        }
    }
}
```

### 4.5 Setup messages（`workflow.go`）

When a run starts, bot synthesizes a sequence of "messages" to send through `bot.Incoming()` to set up the chat:

```go
func (b *Bot) buildSetupMessages(wf *wfe.Workflow, workspace string, ev wfe.Event) []channel.Message {
    msgs := []channel.Message{
        // 1. /cwd sets the chat's CWD (first step of any chat session)
        {ChatID: chatID, Body: "/cwd " + workspace},
    }
    // 2. /use agent sets the agent (if workflow-level agent is configured)
    if wf.Agent != "" {
        msgs = append(msgs, channel.Message{
            ChatID: chatID, Body: "/use agent " + wf.Agent,
        })
    }
    return msgs
}
```

The first `prompt` step (or `/gtw fix`) of the workflow will then be pushed into `bot.Incoming()` by `SendPrompt` (or by the trigger pipeline if using `/gtw fix`).

### 4.6 TriggerManager（`trigger.go`）—— 3 阶段管线：receive → filter → trigger

**Important**: trigger detection is **not** nightme's responsibility. bot implements all trigger sources + filter logic itself. The code below lives entirely in `internal/channel/bot/trigger.go` and **does not** depend on nightme's event bus / channel interface / gateway.

#### 4.6.1 Stage 1 — Receive (passive)

```go
type TriggerManager struct {
    workflows []*wfe.Workflow
    cronSched *cron.Scheduler             // robfig/cron
    gitSub    GitSubscription             // single subscription to gitProvider
    wsMap     *WorkspaceRepoMap           // repo URL → workspace path
    bot       *Bot
}

func (t *TriggerManager) Start(ctx context.Context) error {
    // 1. cron: bot-internal timer
    for _, wf := range t.workflows {
        for _, s := range wf.On.Schedule {
            cronExpr := s.Cron
            t.cronSched.AddFunc(cronExpr, func() {
                t.onEvent(ctx, GitEvent{
                    Kind: "schedule",
                    Time: time.Now(),
                    Data: map[string]any{"cron": cronExpr},
                })
            })
        }
    }

    // 2. build workspace → repo map (startup, one-time)
    t.wsMap = NewWorkspaceRepoMap()
    if err := t.wsMap.Build(t.workflows); err != nil {
        return err
    }

    // 3. git events: single subscription, all event types
    if anyNeedsGitEvents(t.workflows) {
        t.gitSub = t.bot.gitProvider.Subscribe(t.onEvent)
    }

    t.cronSched.Start()
    return nil
}
```

#### 4.6.2 WorkspaceRepoMap（`ws_map.go`）

```go
// WorkspaceRepoMap is built at bot startup by reading
// `git -C <workspace> remote get-url origin` for every workspace.
type WorkspaceRepoMap struct {
    byRepo map[string]string  // "cnlangzi/nightme" → "~/work/nightme"
    byPath map[string]string  // "~/work/nightme" → "cnlangzi/nightme"
}

func (m *WorkspaceRepoMap) Build(workflows []*wfe.Workflow) error {
    seen := map[string]bool{}
    for _, wf := range workflows {
        for _, ws := range wf.Workspaces {
            if seen[ws] { continue }
            seen[ws] = true

            out, err := exec.Command("git", "-C", ws, "remote", "get-url", "origin").Output()
            if err != nil {
                return fmt.Errorf("workspace %s: %w", ws, err)
            }
            repo := canonicalRepoURL(strings.TrimSpace(string(out)))
            m.byRepo[repo] = ws
            m.byPath[ws] = repo
        }
    }
    return nil
}
```

#### 4.6.3 Stage 2 — Filter (by workflow.workspaces)

```go
func (t *TriggerManager) onEvent(ctx context.Context, ev GitEvent) {
    // 1. cron event: no event.repo; fires all workspaces
    if ev.Kind == "schedule" {
        for _, wf := range t.workflows {
            if !wfe.HasSchedule(wf, ev.Cron) { continue }
            for _, ws := range wf.Workspaces {
                t.bot.fireWorkflow(ctx, wf, ev, ws)
            }
        }
        return
    }

    // 2. git event: reverse-lookup repo → workspace
    workspace, ok := t.wsMap.byRepo[ev.Repo]
    if !ok {
        // event from a repo not in bot's workspaces; log warn and drop
        t.bot.log.Warnf("trigger event for unknown repo: %s", ev.Repo)
        return
    }

    // 3. match against workflows
    for _, wf := range t.workflows {
        if !contains(wf.Workspaces, workspace) { continue }
        if !wfe.Match(wf, convertToWfeEvent(ev)) { continue }
        t.bot.fireWorkflow(ctx, wf, ev, workspace)
    }
}
```

**Key behaviors**:

- bot startup builds `byRepo` once by reading `git remote get-url origin` for every workspace.
- git events use `event.repo` to reverse-lookup the workspace.
- **Cron triggers all workspaces** in the workflow (no event.repo → no filter).
- A single event can fire **multiple workflows** if multiple workflows include the workspace.
- A single event **does not** repeat for multiple workspace matches (event.repo → single workspace).
- bot is **client-side filtering** — no per-workflow gitProvider subscription, single subscription handles all.

### 4.7 ActionRegistry（`action.go`）

```go
type Action interface {
    Name() string
    Execute(ctx context.Context, args map[string]any, env map[string]string) (*wfe.ActionResult, error)
}

type ActionRegistry struct {
    mu      sync.RWMutex
    actions map[string]Action
}

func (r *ActionRegistry) Register(a Action) {
    r.mu.Lock()
    defer r.mu.Unlock()
    r.actions[a.Name()] = a
}

func (r *ActionRegistry) Run(ctx context.Context, spec wfe.ActionSpec) (*wfe.ActionResult, error) {
    r.mu.RLock()
    a, ok := r.actions[spec.Name]
    r.mu.RUnlock()
    if !ok {
        return nil, fmt.Errorf("unknown action %q — registered: %v", spec.Name, r.list())
    }
    return a.Execute(ctx, spec.With, spec.Env)
}

func (r *ActionRegistry) list() []string { ... }   // 给错误用
```

`ScanUserScripts` 扫目录：

```go
func (r *ActionRegistry) ScanUserScripts(dir string) error {
    files, err := filepath.Glob(filepath.Join(dir, "*"))
    if err != nil { return err }
    for _, f := range files {
        if info, err := os.Stat(f); err != nil || info.IsDir() { continue }
        if !isExecutable(f) { continue }
        ext := filepath.Ext(f)
        name := strings.TrimSuffix(filepath.Base(f), ext)
        r.Register(&ShellAction{ScriptPath: f, Name: name})
    }
    return nil
}
```

`ShellAction`：

```go
type ShellAction struct {
    ScriptPath string
    Name       string
}

func (a *ShellAction) Execute(ctx, args, env) (*wfe.ActionResult, error) {
    merged := mergeEnv(env, flattenArgs(args))
    argsFile, _ := os.CreateTemp("", "action-args-*.json")
    json.NewEncoder(argsFile).Encode(args)
    argsFile.Close()
    merged["ACTION_ARGS_FILE"] = argsFile.Name()
    defer os.Remove(argsFile.Name())

    cmd := exec.CommandContext(ctx, a.ScriptPath)
    cmd.Env = envToUnix(merged)
    var stdout, stderr bytes.Buffer
    cmd.Stdout = &stdout
    cmd.Stderr = &stderr
    err := cmd.Run()

    out := stdout.Bytes()
    var result wfe.ActionResult
    if json.Unmarshal(out, &result) == nil && result.Outputs != nil {
        return &result, nil
    }
    return &wfe.ActionResult{Outputs: map[string]any{
        "stdout": stdout.String(),
        "stderr": stderr.String(),
        "exit":   exitCode(err),
    }}, err
}
```

### 4.8 内置 action（`action_builtin.go`）

```go
// notify
type NotifyAction struct {
    channels *channel.Registry
}

func (a *NotifyAction) Name() string { return "notify" }

func (a *NotifyAction) Execute(ctx, args, env) (*wfe.ActionResult, error) {
    ch := cast.ToString(args["channel"])
    target := cast.ToString(args["target"])
    msg := cast.ToString(args["message"])
    if ch == "" || target == "" || msg == "" {
        return nil, errors.New("notify: channel, target, message are required")
    }
    if err := a.channels.Send(ctx, ch, target, msg); err != nil {
        return nil, err
    }
    return &wfe.ActionResult{Outputs: map[string]any{"sent": true}}, nil
}
```

`/gtw fix` is invoked through the channel mechanism (bot sends a `/gtw fix <key>` message). bot does not need a special `gtw_fix` action — the existing `/gtw fix` command handler in nightme processes the message.


## 5. 数据流走查

### 5.1 `prompt` 步骤（走 channel 协议）

```
[bot] trigger fires (e.g. mention in PR #42)
   ↓
[bot] TriggerManager.onEvent → match workflow + workspace
   ↓
[bot] fireWorkflow(wf, ev, workspace):
   │
   │  1. 派生 chatID = "bot:wf:" + runID
   │  2. 装 botRun (含 reply channel)
   │  3. setup messages 推入 bot.Incoming():
   │       "/cwd <workspace>"
   │       "/use agent <wf.agent>" (if set)
   │  4. 注册 runsByChatID[chatID] = botRun
   │  5. go driveRun(r)
   ↓
[bot.driveRun] load state, start Tick loop
   ↓
loop:
   ↓
   [bot] wfe.Tick(ctx, state, wf, rt=botRuntime{run: r})
                ↓
            [wfe] 找 step "ai" (prompt, agent=codex)
                ↓
            [wfe] ec = build ExprCtx(state)
                ↓
            [wfe] prompt = EvalString("Review ${{ event.title }}", ec, rt)
                ↓
            [wfe] rt.SendPrompt(ctx, PromptSpec{ChatID, Agent, Prompt, Env})
                ↓
            [bot] botRuntime.SendPrompt:
                  1. 合成 message, 推入 bot.Incoming()
                  2. 在 r.reply channel 上 block 等 reply
   ↓
   [gateway] pumpInbound 读 bot.Incoming() → channelCh
   ↓
   [gateway] dispatchLoop → inbound.Dispatch
   ↓
   [inbound] tryMessageDispatch (普通 prompt) → ChatSession(chatID)
   ↓
   [chatsession] (已经 /cwd 完, /use agent 完)
                → AgentSession (codex) → agent 跑
                → agent reply → AgentSession → outbound.Emitter
   ↓
   [gateway] outbound.Emitter → Channel.Send
   ↓
   [bot] bot.Send(ctx, msg):
         1. 查 runsByChatID[msg.ChatID] = r
         2. r.reply <- msg.Text
   ↓
   [bot] botRuntime.SendPrompt 收到 reply
                ↓
            [wfe] state.StepOutputs["ai"] = parse(reply)
                ↓
            [wfe] state.Advance("ai")
   ↓
   [bot] state = newState
   [bot] stateStore.Save(state)
   ↓
   loop (if state.Status == running)
   ↓
[bot] state.Status = succeeded → cleanup (delete runsByChatID entry, /gtw close if used)
```

**关键点**：

- `prompt` 步骤**走完整 channel 协议**：bot.Incoming() → gateway → ChatSession → agent → reply → outbound.Emitter → bot.Send
- 这条路跟 feishu 用户敲键盘走的是**完全相同**的 pipeline
- `botRuntime.SendPrompt` 是 bot goroutine 在 `r.reply` 上**同步阻塞**，直到 `bot.Send` 被 Emitter 调用投递 reply
- reply routing 是**隐式的**：通过 `runsByChatID[msg.ChatID]` 找到对应的 botRun
- 整个 run 生命周期内，bot 持有 goroutine，`runsByChatID` 是 in-process 状态

### 5.2 `run` 步骤（不走 channel）

```
[wfe] Tick: step "lint" (run)
   ↓
[wfe] cmd = EvalString("npm run lint", ec, rt)
   ↓
[wfe] rt.RunShell(ctx, ShellSpec{Cwd, Command, Env})
   ↓
[bot] botRuntime.RunShell:
      1. cwd = r.run.workspace (or spec.Cwd)
      2. exec.CommandContext("sh", "-c", cmd).Dir = cwd
      3. 收集 stdout/stderr/exit
   ↓
[wfe] state.StepOutputs["lint"] = {exit: "0", stdout: "..."}
```

完全本地，**不经 Gateway / Channel**。跟用户在 shell 里手敲 `make test` 等价。

### 5.3 `use` 步骤（不走 channel）

```
[wfe] Tick: step "notify-feishu" (use: notify)
   ↓
[wfe] with = EvalMap({channel, target, message}, ec, rt)
                ↓ 表达式求值
            message = "PR 42 done"
   ↓
[wfe] rt.RunAction(ctx, ActionSpec{Name: "notify", With, Env})
   ↓
[bot] botRuntime.RunAction → ActionRegistry.Run
   ↓
[bot] lookup "notify" → NotifyAction
   ↓
[bot] channels.Send("feishu", "oc_xxx", "PR 42 done")
   ↓
[wfe] state.StepOutputs["notify-feishu"] = {sent: "true"}
```

不经 Gateway / Channel，但经 channel adapter（bot 内部组件）。

### 5.4 `/gtw fix` 步骤（走 channel，作为 slash command）

```
[wfe] Tick: step "fix-it" (use ... 实际上 /gtw fix 是 prompt step, 发的消息是 /gtw fix)
   ↓
[bot] rt.SendPrompt → 推 "/gtw fix <key>" 消息到 bot.Incoming()
   ↓
[gateway] → inbound → tryCommandDispatch("/gtw fix ...")
   ↓
[chatsession] 命令处理: 调 /gtw fix 内部
   - git worktree add (worktree = workspace/.gtw/fix-<key>/)
   - 设 chat.cwd = worktree
   - 派发 issue 给 agent
   - agent 在 worktree 里跑
   ↓
[agent] → reply → outbound.Emitter → bot.Send
   ↓
[bot] botRun.reply 收到, wfe 推进
```

`/gtw fix` **不是 bot 自己调的**。bot 只是发了一条 `/gtw fix <key>` 消息。nightme 现有的 `/gtw fix` command handler 处理它。**bot 不知道也不关心 `/gtw fix` 内部**。

### 5.5 Workflow pattern for `/gtw fix` and `/gtw close`

Workflows use `/gtw fix` and `/gtw close` by **sending the corresponding slash-command messages** in `prompt` steps. The workflow body is the slash command; the existing nightme command handler does the work. bot has no special API for these commands.

**Example workflow that fixes an issue and posts a notification:**

```yaml
name: issue-fixer
workspaces: [~/work/nightme]
on:
  issue:
    events: [opened, labeled]

jobs:
  fix:
    steps:
      # Step 1: send "/gtw fix" to the chat. The command handler
      # creates a worktree and dispatches the issue to the agent.
      # The reply is the agent's fix summary.
      - id: fix-it
        prompt: "/gtw fix ${{ event.issue.number }}"
        agent: codex

      # Step 2: ask the agent to write a PR description for the fix.
      # (Same chat, same worktree, same agent session.)
      - id: write-pr-desc
        prompt: "Write a PR description for the fix you just made."
        if: ${{ steps.fix-it.outputs.text != '' }}
        agent: codex

      # Step 3: send "/gtw close" to clean up the worktree.
      - id: cleanup
        run: echo "/gtw close ${{ steps.fix-it.outputs.worktree }}"
        # Note: this is a shell echo, not a prompt. v0 doesn't
        # dispatch via SendPrompt; if you want it to run through
        # the channel, use a prompt step.
```

**Why this is the right pattern:**

- `/gtw fix` and `/gtw close` are **existing nightme slash commands** registered in nightme's command dispatcher.
- bot has **no special handling** for them. It pushes the message into `bot.Incoming()`, the gateway routes it via `tryCommandDispatch`, the existing handler runs.
- The workflow author decides when to invoke these commands by including a `prompt` (or `run`) step with the right message body.
- Cleanup (`/gtw close`) is **the workflow's responsibility**, not bot's. The workflow can include a cleanup step or rely on the user to run it manually.

**What bot knows vs. doesn't know:**

| Knows | Doesn't know |
|---|---|
| The chatID (run-scoped unique) | That `/gtw fix` was invoked |
| The workspace (from workflow) | What worktree path was created |
| The workflow's prompt body (sends it) | The agent's response content (only delivers to reply chan) |
| When agent replies come back | When the worktree is cleaned up |

bot is a transparent message pipe. The workflow + existing nightme slash commands + agent are the active participants.

---

## 6. Edge cases

### 6.1 workflow 重名

- 启动时 `LoadDir` 检测 `wf.Name` 重复 → fail fast。
- 重名时哪个文件赢不确定（glob 顺序），所以必须 fail。

### 6.2 同 trigger 短时间内多次触发

- `runID = workflow:trigger-key:started-at`（秒级粒度）。
- 同一 trigger-key 在 1 秒内多次触发 → runID 相同 → StateStore Load 拿已有 state → 续跑。
- 1 秒后的新触发 → 新 runID → 新 state。

### 6.3 daemon restart

- 启动时 `StateStore.List()` 拿到所有非终态的 run。
- 对每个：`onTrigger` 重入 → Load state → 调 `Tick` 续跑。
- 续跑时 `if:` 重新求值；幂等 step 重跑；非幂等 step（`prompt`）继续（如果 session 还活着）。

### 6.4 workflow YAML 语法错误

- Load 时 fail → 不启动 → 错误回显。
- 已启动后热改文件出错 → 不自动 reload（v0 行为），需 `nightme reload` 或重启。

### 6.5 mention 触发但 mention 不在白名单

- `Match` 返回 false → 不 dispatch → 静默忽略。
- 日志记一条 debug。

### 6.6 `use: <unknown>`

- `ActionRegistry.Run` 返回 `unknown action "foo" — registered: [notify, email, ...]`。
- Tick 包成 step error → 状态变 failed。

### 6.7 step `env:` 包含恶意值

- 用户的 YAML 是 trusted input（自己写的），不做隔离。
- 用户脚本是 trusted input（`~/.nightme/workflows/actions/*.sh`），不做隔离。
- bot 默认 env（含 nightme config secrets）跟用户 env **在同一 map 里**，不做命名空间区分。
- 日志 redaction 由 logger 名字匹配处理（已存在）。

### 6.8 prompt 超时

- `defaultPromptTimeout = 30min`（v0 写死，跟 agent session 默认 timeout 对齐）。
- 超时 → `ErrPromptTimeout` → step error → state failed。

### 6.9 run 的 working dir

- 来源：bot 在 `/gtw fix` 返回的 worktree 路径，写进 `state.Env["WORKSPACE_CWD"]`。
- 用户可在 workflow / job / step env 覆盖。
- v0 不做 path traversal 校验（trusted input）。
- **`workspaces:` 字段** 决定 run 的"哪个 workspace"被激活——trigger 命中后 bot 从 `workspaces` 列表里挑出对应的那个，写进 `state.Workspace`，再派生 worktree。

### 6.10 concurrent trigger 风暴

- `worker: N` 是每 workflow 上限，bot 用 semaphore 实现。
- 超出的 trigger 排队（FIFO）or 丢弃（v0 选哪个？——见 §9）。

### 6.11 mention 文本解析

- 来源是 gitProvider（PR/issue 评论），不是 feishu 聊天。bot 自己实现 parser。
- 形式 `@<owner> <command> [<args>...]`：第一个 token 是 `command`，剩余部分是 `args`。
- 取第一个 token 作为 `command`（`@owner review this PR` → `command: review`，`args: "this PR"`）。
- 边界条件：多空格、斜杠命令风格（`@owner/foo`）、单独 `@owner` 无 command 时的处理。

---

## 7. Test plan

### 7.1 wfe 单元测试（`internal/wfe/*_test.go`）

| Test | 覆盖 |
|------|------|
| `TestLoad_ValidYAML` | 正常 workflow 解析 |
| `TestLoad_InvalidYAML` | 缺字段 / 类型错 / 多选 step |
| `TestLoad_DuplicateName` | 跨文件重名 |
| `TestMatch_Schedule` | schedule trigger |
| `TestMatch_PullRequest` | branch + event 双过滤 |
| `TestMatch_Mention` | command 白名单 |
| `TestTick_RunStep` | RunShell 被调，outputs 落 state |
| `TestTick_PromptStep` | SendPrompt 被调，outputs 落 state |
| `TestTick_UseStep` | RunAction 被调，按 name 派发 |
| `TestTick_StepFailure` | err 流转，状态变 failed |
| `TestTick_ContinueOnError` | err 流转，状态仍 running，outputs 含 error |
| `TestTick_IfFalse` | 跳过 step |
| `TestTick_NeedsNotMet` | 跨 job 等待 |
| `TestEvalString_*` | ${{ }} 各场景 |
| `TestEvalCond_*` | success() / failure() / always() |
| `TestEvalMap_Recursive` | with: 嵌套结构求值 |

mock Runtime：

```go
type mockRT struct {
    shellCalls   []ShellSpec
    promptCalls  []PromptSpec
    actionCalls  []ActionSpec
    now          time.Time
    // 每个方法的预设返回值
}

func (m *mockRT) RunShell(ctx, s) (*ShellResult, error) { m.shellCalls = append(...); return m.shellRet, m.shellErr }
```

每个 Tick 测试在 30 行内，零 I/O、零 sleep、零依赖。

### 7.2 bot subsystem 集成测试

| Test | 覆盖 |
|------|------|
| `TestBot_Start_LoadsWorkflows` | 加载 + action 注册 |
| `TestBot_TriggerCron_Fires` | mock clock + cron |
| `TestBot_TriggerMention_ResolvesChat` | mention 路径 |
| `TestBot_TriggerPullRequest_ResolvesChat` | PR 路径 |
| `TestBot_WorkerPool_LimitsConcurrency` | 同一 workflow N+1 个 trigger |
| `TestBot_StateStore_RestartRecovery` | Save → kill → Load → Tick 续跑 |
| `TestBot_ActionRegistry_UserScript` | 写一个测试脚本到 tmp，verify 注册 + 执行 |
| `TestBot_ActionRegistry_UnknownAction` | error message 正确 |

### 7.3 E2E（v0 不强求）

需要真飞书 / 真 GitHub：

- 配置一个最小 workflow，cron 触发
- 验证 state file 出现，run 成功
- 验证 notify action 真的发到飞书

### 7.4 性能 / 并发

- 1000 个并行 run 的 StateStore 写盘（v0 用单 goroutine per run，瓶颈在 fs）
- ActionRegistry 的 RWMutex 竞争（read 远多于 write，问题不大）

---

## 8. 上线分阶段

### Phase 0: wfe 库 only（无 bot 集成）

- 写完 `internal/wfe/` 全套 + 单测
- 用 `go test ./internal/wfe/...` 100% 覆盖
- **没有** bot subsystem 集成——纯库
- 持续 1 周

### Phase 1: bot subsystem 骨架 + cron only

- 写完 `internal/channel/bot/` 但只接 cron trigger
- 不接 PR / issue / mention
- 不接 ActionRegistry 用户脚本（只内置）
- 一个 `notify` action
- 一个示例 workflow：每天 9 点发飞书 hello
- 持续 1 周

### Phase 2: 全 trigger

- 接入 git provider events（PR / issue / branch / mention 全部 4 个）
- 接入 action 用户脚本
- 跑几个真 workflow（PR review / issue triage / mention fix）
- 持续 2 周

### Phase 3: 稳定性 + v0 完成

- daemon restart 恢复测试
- 长跑 workflow（跨小时）测试
- 错误恢复（run state 损坏、action 脚本 fail、prompt 超时）
- 文档补全（CLI 命令、`nightme workflow list` / `logs` / `retry`）
- 持续 2 周

Phase 3 结束 → 内部 release。

---

## 9. Open questions

1. **trigger 排队 vs 丢弃**：`worker: N` 满了之后，新 trigger 排队（FIFO 等 worker）还是直接丢弃记 metric？倾向排队（不丢用户的请求），但要看存储成本。

2. **mention 文本解析的边界条件**：bot 自己的 parser 怎么处理 `@owner/foo`（斜杠命令风格）、`@owner  fix`（多个空格）、`@owner` 单独一个名字（没 command）？

3. **多 workflow 共享 chat**：两个 workflow 都 trigger 同一个 PR，是不是同一个 ChatSession？v0 倾向"按 workflow 拆 chat"（更隔离），但这意味着 PR 关联 chat 多份。

4. **action 脚本的执行用户**：跟 nightme daemon 同用户？要不要降权到 nobody？v0 假设同用户。

5. **workflow 文件热加载**：v0 不做（restart 才能改）。要不要在 Phase 3 加？

6. **action 脚本签名验证**：用户写的 `~/.nightme/workflows/actions/*.sh` 不签名就执行。如果用户改了脚本，bot 知道。v0 接受这个风险。

7. **state 清理策略**：state 文件会一直累积。要做 `nightme clean`（已有，参考 `nightme clean` 命令）覆盖？还是按 `mtime > 30d` 自动清？

8. **跨 daemon 共享 state**：现在 state 写到 `~/.nightme/`。多 daemon 部署（不常见但可能）会冲突。v0 假设单 daemon。

---

## 10. Cross-references

- **用户参考**：[`../WFE.md`](../WFE.md) §5 (steps) §6 (expressions) §7 (examples) §8 (runtime architecture overview)
- **Channel 抽象**：[`./F-08-channel-abstraction.md`](./F-08-channel-abstraction.md)
- **Agent 抽象**：[`./F-09-agent-abstraction.md`](./F-09-agent-abstraction.md)
- **Gateway 分发**：[`./F-gateway.md`](./F-gateway.md) §3 inbound dispatch
- **GitProvider 抽象**：[`./F-50-git-provider.md`](./F-50-git-provider.md)（用于 github_* actions + PR/issue events）
- **State 持久化风格**：[`./F-runtime.md`](./F-runtime.md) §A3（JSON 0600 原子写）

## 11. Change log

- 2026-08-18：初版。涵盖 wfe 库 + bot subsystem 两个包的设计。
