# nightme Workflow 引擎（wfe 库 + personal agent host）

> **Source**: 无历史 F-XX 来源
> **Related docs**: [`../WFE.md`](../WFE.md)（用户参考——YAML schema / 触发器 / step 形式 / 运行时架构）；[`../me.md`](../me.md)（personal agent）；[`../SPEC.md`](../SPEC.md) §2 数据流

本文件是 workflow 引擎的**实现设计**。YAML 怎么写、step 长啥样、`${{ }}` 怎么求值——这些都在 `WFE.md`。本文件回答的是：包怎么拆、计划怎么算、状态怎么存、判断点怎么判、测试怎么写。

---

## 1. 设计目标

构建一个 GitHub-Actions-风格的本地工作流引擎，跑在 nightme 自己的 personal agent（`/me`）上。核心约束：

- **wfe 是纯库**。无 I/O、无 clock、无 secrets——所有外部能力由 host（`internal/me`）注入。
- **host 是 personal agent**。加载 workflow、持有 run 状态、驱动循环、注入 shell / action 能力、渲染 run 的卡片。
- **两层推进**。快路径：plan 能决定 → step 照跑，不问模型。判断点：plan 决定不了 → agent 决定。
- **可扩展**。`message` / `shell` 是默认必带；`use` 是 host 注入 action 的扩展点；`wait` 让 run 等一段时间。

---

## 2. 包布局

```
pkg/wfe/                          # 纯库（无 I/O）
├── types.go                      # Workflow / Job / Step / Trigger / Event / RunState / Outcome
├── load.go                       # bundle 发现 + YAML 解析 + 校验
├── validate.go                   # schema 校验 → []Diagnostic
├── match.go                      # Event -> *Workflow 匹配（纯函数）
├── engine.go                     # 计划引擎：Decide / ReadySet / EndRound
├── expr.go                       # ${{ }} 求值
├── policy.go                     # Policy：一次 run 能做多少事的边界
├── runtime.go                    # 外部能力的词汇（ShellSpec / ActionSpec / ExprCtx）
├── errors.go                     # 错误哨兵
└── diagnostics.go                # Diagnostic / ValidationError

internal/me/                      # host：personal agent
├── agent.go                      # Service：目录 / action registry / run 表 + 宿主接口
├── catalog.go                    # workflows 目录的视图（按文件指纹缓存）
├── agent_run.go                  # run 的状态、事件与工具调用
├── agent_loop.go                 # 驱动循环
├── agent_decide.go               # System 1：决策模型
├── agent_system2.go              # System 2：chat 模型
├── agent_tools.go                # 工具（resume / next / instruct / …）
├── agent_brain.go                # agent 工具：owner 的思考
├── agent_prompt.go               # system prompt
├── agent_report.go               # 情况报告
├── directive.go                  # 判断点的答复
├── step.go                       # 五种 step 的执行
├── shell.go                      # ExecShell：RunShell
├── action.go                     # ActionRegistry + 内置 action + 用户脚本
├── session.go                    # run 的会话
├── overview.go / summary.go / card.go / answer.go
└── inbound.go / commands.go / outcome.go / ratelimit.go / directive.go

cmd/nightme/workflow.go           # nightme workflow list / show / run / validate
internal/command/wf/              # /wf install / /wf uninstall
internal/llm/                     # 两种模型能力：Provider（chat）/ Decider（决策）
```

依赖方向严格单向：

```
pkg/wfe ◄── internal/me ◄── internal/llm
                      ◄── internal/channel（run 说话的通道）
                      ◄── internal/gateway/observe（daemon 回给 run 的旁路）
```

**关键**：`internal/me` **不依赖** `internal/chatsession` 和 `internal/agentsession`。run 说的每句话都是那个 chat 里的一条真消息——经 channel 协议进去，再经同一个 channel 回来。回执、表情和交互式提问因此像锚在一条真人的消息上一样锚在 run 的消息上。

wfe 不 import nightme 的任何东西。`internal/me` import wfe + nightme 的 channel / llm 体系。这是 wfe 单测干净的原因。

---

## 3. wfe 库（`pkg/wfe/`）

### 3.1 核心类型（`types.go`）

```go
// Workflow 是解析+校验后的工作流定义。
type Workflow struct {
    Title      string         `yaml:"title" json:"title"`
    Name       string         `yaml:"-" json:"name"`
    Args       []Arg          `yaml:"args" json:"args,omitempty"`
    Instruct   bool           `yaml:"instruct" json:"instruct"`
    Workspaces []string       `yaml:"workspaces" json:"workspaces,omitempty"`
    On         Trigger        `yaml:"on" json:"on"`
    Jobs       map[string]Job `yaml:"jobs" json:"jobs"`
}

type Job struct {
    Needs      []string `yaml:"needs" json:"needs,omitempty"`
    If         string   `yaml:"if" json:"if,omitempty"`
    Iterations *int     `yaml:"iterations,omitempty" json:"iterations,omitempty"`
    Sleep      string   `yaml:"sleep,omitempty" json:"sleep,omitempty"`
    Steps      []Step   `yaml:"steps" json:"steps"`
}

type Step struct {
    ID              string            `yaml:"id,omitempty" json:"id,omitempty"`
    If              string            `yaml:"if,omitempty" json:"if,omitempty"`
    Env             map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
    ContinueOnError bool              `yaml:"continue-on-error,omitempty" json:"continue-on-error,omitempty"`
    Note            string            `yaml:"note,omitempty" json:"note,omitempty"`

    // 四选一
    Message string         `yaml:"message,omitempty" json:"message,omitempty"`
    Shell   string         `yaml:"shell,omitempty" json:"shell,omitempty"`
    Use     string         `yaml:"use,omitempty" json:"use,omitempty"`
    With    map[string]any `yaml:"with,omitempty" json:"with,omitempty"`
    Wait    string         `yaml:"wait,omitempty" json:"wait,omitempty"`

    Agents []AgentChain `yaml:"agents,omitempty" json:"agents,omitempty"`
}
```

`Title` 说这个 workflow 是做什么的；`Name` 是它的身份——bundle 在 workflows 目录下的相对路径（`nightme/coder/workflow.yml` 就是 workflow `nightme/coder`），由 loader 填，文件里不写。旧文件里的 `goal` 键在反序列化时迁进 `Title`（`Workflow.UnmarshalJSON`）。

`Step` 有**五种** kind：`message` / `shell` / `use` / `wait` / `cmd`。`Step.Kind()`（`types.go`）先看 `Cmd` 是不是有值——有就是 `StepKindCmd`；否则按 Message → Shell → Use → Wait 的顺序判定，五种都没写就是 `StepKindNone`。

`Job.Iterations` 是这个 job 跑几轮：缺省 1，0 表示一直跑到 run 结束，N 就是 N 轮。`Job.Sleep` 是两轮之间的间隔。

`RunState` 是 run 的进度：

```go
type RunState struct {
    RunID        string                `json:"run_id"`
    WorkflowName string                `json:"workflow"`
    Workspace    string                `json:"workspace"`
    Status       Status                `json:"status"`
    Env          map[string]string     `json:"env,omitempty"`
    Rounds       map[string]int        `json:"rounds,omitempty"`
    EndedRounds  map[string]EndedRound `json:"ended_rounds,omitempty"`
    Steps        []StepState           `json:"steps"`
    StartedAt    time.Time             `json:"started_at"`
    UpdatedAt    time.Time             `json:"updated_at"`
}
```

`StepState` 记每一个跑过的 step：它的 `ID`、所属的 `Job`、第几 `Round`、`Outcome`，以及两个标志——`Skipped`（`if:` 不成立）、`Pending`（开始了但还没落定）。

`Outcome` 是 step 的裁决：`Status` 是 HTTP 状态码，`StatusText` 是一句话，`Data` 是事实。**plan 靠 `Status` 的类别区分"正常结果"和"异常"**：`2xx` 是正常结果，`4xx` 与 `5xx` 是异常。一个因为"有活儿要干"才跑起来的命令可能什么都没做就报了 `2xx`——是哪种情况由 `Data` 里的事实说明（没有 `issue` 键，或者 `commits=0`）。

### 3.2 Loader（`load.go`）

```go
func LoadDir(dir string) ([]*Workflow, []Failure)
func LoadFile(root, path string) (*Workflow, error)
func Parse(raw []byte) (*Workflow, error)
func Bind(wf *Workflow, args map[string]string) (*Workflow, error)
func WorkflowPaths(dir string) ([]string, error)
func WorkflowName(root, path string) string
```

一个 workflow 是一个 **bundle**：目录下一个 `workflow.yml`（`workflow.yaml` 也认）。bundle 在目录下的路径就是它的名字。`LoadDir` 返回加载成功的 workflow 和每个坏 bundle 的 `Failure`（`Name` / `Path` / `Err`）——一个坏文件不会把旁边的文件一起拖下去。两个文件认领同一个名字时，两个都不在这个名字下挂，loader 报一条 `duplicate workflow name` 的 `Failure`。

workflows 目录下的 `actions` 和 `runs` 是保留目录，loader 不往里走。`actions` 放用户的 action 脚本。

`Bind` 把 run 的实参绑到 workflow 上：没有默认值的 `args` 是必填的。

### 3.3 校验（`validate.go`）

```go
func Validate(raw []byte) []Diagnostic
func ValidateFile(path string) []Diagnostic
func ValidateDir(dir string) []Diagnostic
```

`Diagnostic` 带行号、列号和一句话说明。校验的内容：必填字段、step 的 kind 互斥、`if:` 表达式能解析、`args` 的形状、`agents` chain 的形状、trigger 的形状，以及**文件里出现但 schema 不认的键**（`checkRootKeys` / `checkJobKeys` / `checkAgentChainKeys` / `checkTriggerKeys`）。

`Parse` / `LoadFile` / `LoadDir` 返回的是 `*ValidationError`——它包着 `ErrValidationFailed`，并带着所有找到的问题。

### 3.4 Matcher（`match.go`）

纯函数：给定 `Event` 和 `*Workflow`，决定它是否该被这个 event 点着。

```go
func Match(wf *Workflow, ev Event) bool
```

按 `ev.Kind` 分派到 `matchSchedule` / `matchPR` / `matchBranch` / `matchIssue` / `matchMention`。workspace 的过滤不归它管——那是触发管线的事。

### 3.5 计划引擎（`engine.go`）

**这是 wfe 唯一会改变 state 的地方。**

```go
func Decide(state *RunState, wf *Workflow) (Decision, error)
func ReadySet(state *RunState, wf *Workflow) ([]Step, error)
func EndRound(state *RunState, wf *Workflow, job string) error
func RecordStep(state *RunState, job string, step Step, out Outcome, skipped bool)
func RecordPending(state *RunState, job string, step Step)
func IsPending(state *RunState, stepID string) bool
```

`Decide` 算出 run 下一步做什么，结论有两种：

| `Decision.Kind` | 什么时候 | 之后 |
|---|---|---|
| `DecisionRun` | ready set 只有一个 step，且上一个 outcome 是正常结果 | step 照跑（`Decision.Step` / `Decision.Job`） |
| `DecisionJudgment` | ready set 为空 / ready set 分叉 / 上一个 outcome 抛了异常 / step 还在跑 | agent 决定（`Decision.Reason` / `Decision.Subject`） |

还没跑过任何东西的 run 算"正常结果"，所以第一个 step 走快路径。

`Decision` 上还有三个标志：`Exhausted`（plan 已经没有东西可跑）、`Over`（workflow 结束——重复 job 的轮数用尽且最后一个 step 没坏，run 自己结束，不问模型）、`Gap`（上一轮结束到下一轮开始的间隔，也就是 `sleep:`）。

`ReadySet` 是 run 下一步可以跑的 step：job 的 `needs` 都满足、`if:` 成立、且还没跑过。同一个 job 里只有第一个这样的 step算数——job 的 step 是顺序的。`ReadySet` 不推进轮次，也不记录被跳过的 step；`Decide` 才做这两件事。

`EndRound` 是提前结束一轮的**唯一**入口：这一轮没跑的 step 记为 skipped，并把这一轮标记为已结束。plan 接着走下一轮。还在跑的 step 会让 `EndRound` 报错——一个没落定的命令下面不能收摊。

### 3.6 表达式（`expr_parse.go` / `expr_eval.go` / `expr.go`）

```go
type ExprCtx struct {
    Event map[string]any
    Steps map[string]map[string]string
    Needs map[string]map[string]string
    Env   map[string]string
    Args  map[string]string
    Now   time.Time
    Last  map[string]string
}

func EvalString(s string, ec ExprCtx) string
func EvalMap(m map[string]any, ec ExprCtx) map[string]any
func EvalCond(cond string, ec ExprCtx) (bool, error)
```

条件是一棵表达式树，不是插值出来的字符串：tokenizer 切词，递归下降的 parser 产出解析树，求值器走这棵树。这样条件才能比较——"上一轮是不是 429"这种问题，字符串插值答不了。

值只有四种：null、bool、number、string。路径查不到东西就是 null。

支持的写法：

| 写法 | 取值 |
|------|------|
| `event.X` | `ec.Event["X"]`（支持嵌套） |
| `steps.ID.K` / `steps.ID.outputs.K` | `ec.Steps["ID"]["K"]`（`outputs` 段可省） |
| `needs.JOB.K` | `ec.Needs["JOB"]["K"]` |
| `env.X` | `ec.Env["X"]` |
| `args.X` | `ec.Args["X"]` |
| `now` | `ec.Now` 的 RFC 3339 写法 |
| `success()` | 最后一个跑过的 step 的裁决；还没跑过 step 时为真 |
| `empty(X)` | `X` 是 null，或是去空白后为空的字符串 |

运算符从低到高：`||` → `&&` → `==` / `=` / `!=` → `<` / `<=` / `>` / `>=` → `!`。`!` 比比较紧，所以 `!a == b` 是 `(!a) == b`。

比较按自动转换做，运行期不报类型错误：两边都是字符串就比字符串，恰好一边是 bool 就比真值，一边是 null 就只有两边都是 null 才相等，其余比数字。`<` 这类比较：两边都读得出数字就比数字，否则两边都是字符串就比字典序，其余为假。

`$` 是分隔符的一部分（跟 GitHub Actions 一样）。引号里的 `}`、`}}`、`,`、`(`、`)` 都是普通字符——插值的扫描器跳过引号里的内容。

`EvalCond` 把求值结果当布尔读：null 是假，bool 是自身，number 不等于 0 是真，字符串空串 / `false` / `0` 是假；条件为空串时恒为真。

`if:` 既接受裸表达式，也接受整个值就是一个 `${{ ... }}`——两种写法等价。别的形态（两个 `${{ }}` 拼在一起、`${{ }}` 前后有别的字）是语法错误，加载文件时就报出来，诊断指到那个 `if:` 的位置。

wfe **不做 secret 区分**——`${{ env.GH_TOKEN }}` 和 `${{ env.WORKDIR }}` 走完全一样的 lookup。

### 3.7 外部能力的词汇（`runtime.go`）

```go
type ShellSpec struct { Cwd, Command string; Env map[string]string; Shell string }
type ShellResult struct { ExitCode int; Stdout, Stderr string; Outputs map[string]string }
type ActionSpec struct { Name string; With map[string]any; Env map[string]string }
type ActionResult struct { Outputs map[string]any }
```

wfe 不知道这些 Spec 的实现——可能是 os/exec，可能是 nightme client，可能是 mock。纯库单测就是这么干净的原因。

### 3.8 Policy（`policy.go`）

```go
func NewPolicy(wf *Workflow, history []ActionRecord) *Policy
```

Policy 是"一次 run 能做多少事"的边界，是 workflow 和已发生的动作的纯函数：

| 方法 | 报什么 |
|---|---|
| `CheckRun(command)` | 这个命令现在能不能跑、算在哪个 step 上 |
| `CheckStep(index)` | 这个 step 现在能不能动作（当前轮次的预算） |
| `CheckInstruct()` | 允不允许自由文本的指令 |
| `CheckFinish(status)` | 允不允许结束 run |
| `Remaining()` | 每个 step 还剩几次机会 |
| `Render()` | workflow 的紧凑文本块 |
| `StepCommands()` | policy 现在接受的 step 消息 |

预算按轮次计：step 自己的执行、agent 决定跳过它、发给它的 nightme 命令和指令、以及没产出工具调用的 chat 模型回合，都算在这个 step 上。每一轮重新开始算。

---

## 4. host（`internal/me/`）

### 4.1 Service（`agent.go`）

```go
type Service struct {
    cfg    Config
    logger *slog.Logger

    catalog *Catalog
    mu      sync.Mutex
    runs    map[string]*run
    active  map[string]*run
    started bool
    actions *ActionRegistry
    wg      sync.WaitGroup
}
```

`Service` 拥有 workflow 目录、action registry 和所有 run。**一个 chat 同时只有一个 run**：起 run 的那个 chat 就是 run 干活的 chat，也是 run 汇报的那个 chat。

宿主能力由 daemon 注入（`Config`）：`Operator`（在 chat 里说话）、`Shell`（跑 shell 命令）、`Git`（读仓库事实）、`Cwd`（chat 当前的工作目录）、`AgentOpener`（owner 的思考会话）、`AgentSwitcher`（临时换 agent）、`Provider`（chat 模型）、`Decider`（决策模型）。

`Start` 刷新 workflow 目录、注册内置 action、扫描用户脚本；`Shutdown` 停掉每个 run 并等它们结束。

### 4.2 Catalog（`catalog.go`）

目录是**用到时才读**的：改过的文件在下一次 `/me start` 时生效，中间没有 watcher。每个文件按指纹（大小 + 修改时间）记住，只重新解析变了的东西。`Lookup(name)` 给出 workflow，`Failures()` 给出每个用不了的名字的原因。

### 4.3 run 的状态（`agent_run.go`）

run 持有 `wfe.RunState`（引擎的账本）、动作历史（`[]wfe.ActionRecord`）、当前的 step、模型会话、以及它发出去的消息的 id。所有对 `RunState` 的写都经引擎——agent 自己不写状态。

### 4.4 驱动循环（`agent_loop.go`）

```go
func (s *Service) drive(ctx context.Context, r *run)
```

一轮一轮地推（`agent_loop.go` 的 `turn`）：

```
turn:
   1. 先执行上一轮留下的 directive（resume / next / instruct / …）
   2. decision := wfe.Decide(state, wf)
      - DecisionRun:
           查这个 step 的预算
           够   → 跑它（message / shell / use / wait），把 outcome 记进 state
           不够 → 降级成判断点
      - DecisionJudgment:
           exhausted 且每一步都以 ok 收尾 → run 自己结束（不问模型）
           Over（重复 job 的轮数用尽）   → workflow 结束，run 自己结束（不问模型）
           这个 step 的预算花完           → 暂停，等人（/me resume）
           否则 → System 1 决策模型 → 不确定就 System 2 chat 模型 → 还没有就问用户
   3. 刷新 run 的卡片
```

快路径上一次模型调用都没有：workflow 每一步都以 `ok` 收尾的 run，全流程零调用。

run 没有自己的时钟：每条命令带自己的预算，超时是 agent 的超时，不是 run 的。run 负责看着。

### 4.5 五种 step（`step.go`）

```go
func (s *Service) executeStep(ctx context.Context, r *run, job string, step wfe.Step) (stepVerdict, error)
```

按 `step.Kind()` 分派：

- **message** — 把文本发进 chat，等它的结果。step 可以带一条 `agents` chain：多个 agent 按顺序试，一个断了就交给下一个；第一个没断的就是这个 step 的裁决。chain 试完还断 → step 判 failed，plan 决定不了，交给 agent。
- **shell** — `cfg.Shell.RunShell` 在 chat 当前的工作目录里跑命令。daemon 没有 shell runner 时，这个 step 直接判 failed。
- **use** — `cfg.actions.Run` 按名字找注册的 action 跑。没有 shell runner 就没有 shell step；没有注册的 action 名字就是 `wfe.ErrUnknownAction`。
- **wait** — 等一段 Go duration。
- **cmd** — 跑引擎自己的 builtin command。step 可以带一条 `message:`：消息先发进 chat，它的裁决就是这个 step 的裁决；消息失败就不跑 command，交给 agent。command 跑在引擎里：`skip` 结束这一轮，并把这一轮记成被跳过的轮次。

每个 step 的结果都是一个 `wfe.Outcome`：`ok` / `failed` / …，外加 `Summary` 和事实。命令还在跑的时候 step 记为 pending，plan 把它当成判断点。

### 4.6 外部能力（`shell.go` / `action.go`）

`ExecShell`（`shell.go`）实现 `RunShell`：在 `Cwd` 里跑命令，收 stdout / stderr / exit code，并把 stdout 里的 `key=value` 行解析成 outputs。

`ActionRegistry`（`action.go`）是 `use:` step 的扩展点：

```go
type Action interface {
    Name() string
    Execute(ctx context.Context, args map[string]any, env map[string]string) (*wfe.ActionResult, error)
}
```

`ScanUserScripts(dir)` 把 `<workflows dir>/actions` 下每个可执行文件注册成一个 action（`ShellAction`）：`with:` 的参数写进一个临时 JSON 文件，通过 `ACTION_ARGS_FILE` 环境变量交给脚本；脚本 stdout 是 JSON 就用它当结果，否则包成 `stdout` / `stderr` / `exit` 三个事实。

内置 action 四个：`notify`、`email`、`github_pr_comment`、`github_issue`——都还是桩，返回 `stub: true`。

### 4.7 触发器（`types.go` / `match.go`）

`on:` 块被解析、被校验、也被显示，但**还没有任何东西点得着它**：没有 cron 调度器，没有 git provider 的订阅。`wfe.Match` 是匹配 `on:` 规格的纯函数，触发管线是留给它的接口。

---

## 5. 数据流走查

### 5.1 `message` 步骤

```
[me] plan: wfe.Decide(state, wf) → DecisionRun{Step: "ai"}
   ↓
[me] step.runAttempts: chain = step.Agents（没写就是一次）
   ↓
[me] AgentSwitcher.SwitchAgent(chatID, agent)（use 模式才切）
   ↓
[me] Operator.Speak(chatID, "/gtw fix 42")  ← 这就是 chat 里的一条真消息
   ↓
[channel] gateway → chatsession → agentsession → bridge → coding agent
   ↓
[agent] 干活 → 结果回到 daemon
   ↓
[me] runCommand 拿到结果 → wfe.RecordStep(state, job, step, outcome)
   ↓
[me] 下一次 Decide
```

关键点：

- `message` 步骤走的是**和真人打字完全一样的通道**——run 没有特权路径。
- run 的回复挂在它自己那条消息下（`session.go`）：回执、表情、交互式提问都锚在那条消息上。
- 命令还在跑 → step 记为 pending → plan 把它变成判断点，agent 决定是等、是跳过还是结束这一轮。

### 5.2 `shell` 步骤

```
[me] plan: DecisionRun{Step: "lint"}
   ↓
[me] EvalString("npm run lint", ec) → 命令
   ↓
[me] ExecShell.RunShell(ShellSpec{Cwd: chat 的当前目录, Command, Env})
   ↓
[me] wfe.RecordStep(..., Outcome{Status: ok, Data: {exit, stdout, stderr}})
```

完全本地，**不经 Gateway / Channel**。跟用户在 shell 里手敲 `make test` 等价。

### 5.3 `use` 步骤

```
[me] plan: DecisionRun{Step: "notify"}
   ↓
[me] EvalMap(step.With, ec) → with
   ↓
[me] ActionRegistry.Run(ActionSpec{Name, With, Env})
   ↓
[me] wfe.RecordStep(..., Outcome{Status: ok, Data: outputs})
```

不经 Gateway / Channel，但经 host 自己的 action registry。

### 5.4 判断点

```
[me] plan: wfe.Decide → DecisionJudgment{Reason, Subject}
   ↓
[me] System 1：决策模型（me.decider）
   │   问三件事：next（choice）/ needs_human（yes-no）/ step_succeeded（yes-no）
   │   置信度够、不是 escalate、policy 允许 → 直接照办
   ↓（不够确定）
[me] System 2：chat 模型（me.llm）带工具循环
   │   工具：resume / next / ntm_<command> / agent / instruct /
   │        answer_choice / ask_human / repo_status / wait / end_round / finish
   ↓（没有 chat 模型）
[me] 问用户（/me reply <text>）
```

答复由 `directive.go` 执行。`resume` 把出问题的 step 再跑一遍（先查它的预算），`next` 把它记为跳过并让 plan 继续，`finish(status, summary)` 结束 run。

---

## 6. Edge cases

### 6.1 workflow 名冲突

- 名字是 bundle 在目录下的路径，所以两个不同的 bundle 不会撞名。
- 同一个名字被两个文件认领 → 两个都不在这个名字下挂，报一条 `duplicate workflow name` 的 `Failure`。
- 直接躺在 workflows 目录里的 `*.yml` 不是 workflow；bundle 里 `workflow.yml` 旁边的文件也不是。

### 6.2 同一个 trigger 短时间内多次触发

- 触发器还没有接线，所以这一条目前不成立。
- 名字的粒度是 bundle 路径（`nightme/coder`），不是时间戳。

### 6.3 daemon 重启

- run 的状态在内存里，daemon 重启后不恢复。
- workflow 文件不用重读就能生效：目录是用到时才刷新的。

### 6.4 workflow YAML 语法错误

- 加载失败的 bundle 记成一条 `Failure`，不影响旁边的 bundle。
- daemon 启动时这些失败会记进日志；`nightme workflow list` 会把它们列出来。

### 6.5 mention 触发但 mention 不在白名单

- `Match` 返回 false → 不触发。
- 触发管线还没接线，所以这一条目前不成立。

### 6.6 `use: <unknown>`

- `ActionRegistry.Run` 返回包着 `wfe.ErrUnknownAction` 的错误，并列出已注册的名字。
- step 判 failed，plan 把它变成判断点。

### 6.7 step `env:` 包含敏感值

- 用户的 YAML 是 trusted input（自己写的），不做隔离。
- 用户脚本是 trusted input（`<workflows dir>/actions/*.sh`），不做隔离。
- host 的默认 env 跟用户 env 在同一个 map 里，不做命名空间区分。

### 6.8 命令超时

- 每条命令带自己的预算，超时是 agent 的超时，不是 run 的超时。
- 超时 → step 没有自己的裁决 → plan 把它变成判断点。

### 6.9 shell 步骤的工作目录

- 来源是 chat 当前的工作目录（`CwdProvider`）——chat 被 `/gtw fix` 挪进 worktree，shell 步骤就跟着挪。
- daemon 报不出 chat 的目录时，回落到 run 起手时的 workspace。

### 6.10 预算

- 每个 step、每一轮的预算是一个数，由 `me.max_judgments`（默认 50）定，workflow 文件里不写。
- 预算花完时 run 暂停，并在 chat 里说清楚；操作员用 `/me resume` 重跑，那一步的预算重新开始算。

### 6.11 两个模型都没有

- `me.llm` 和 `me.decider` 都没配 → personal agent 不可用，`/me start` 直接拒绝并说明原因。
- 只有 `me.decider` → 置信度够的决定直接做，其余问用户。
- 只有 `me.llm` → 每个判断点都走 chat 模型的工具循环。

---

## 7. Test plan

### 7.1 wfe 单元测试（`pkg/wfe/*_test.go`）

| 文件 | 覆盖 |
|------|------|
| `load_test.go` | bundle 发现、名字、重名、坏文件、实参绑定 |
| `validate_test.go` | 必填字段、kind 互斥、未知键、表达式、`args` |
| `match_test.go` | schedule / PR / branch / issue / mention 的过滤 |
| `engine_test.go` | 快路径、分叉、`EndRound`、plan 的各个出口 |
| `rounds_test.go` | 轮次与轮次之间的间隔 |
| `sleep_test.go` | `sleep:` 的解析、绑定、JSON 往返与 plan 里的 gap |
| `exception_test.go` | 异常与判断点 |
| `agents_test.go` | agent chain 的解析与绑定 |
| `expr_test.go` | `${{ }}` 的各种写法 |
| `policy_test.go` | 预算、检查、渲染 |

mock 很简单：wfe 不做 I/O，测试里只需要构造 `RunState` 和 `Workflow`。

### 7.2 host 集成测试（`internal/me/*_test.go`）

| 文件 | 覆盖 |
|------|------|
| `step_test.go` | 五种 step 的执行与裁决 |
| `action_test.go` | action registry、未知 action、用户脚本 |
| `agent_test.go` / `control_test.go` | run 的生命周期与控制 |
| `state_test.go` | run 状态的读写 |
| `agent_workflows_test.go` | workflow 相关的 agent 行为 |
| `agent_budget_test.go` | 预算的边界 |

### 7.3 CLI（`cmd/nightme/workflow_test.go`）

`nightme workflow list / show / run / validate` 的输出。

---

## 8. CLI 与安装面

| 入口 | 做什么 |
|---|---|
| `nightme workflow list` | 列出 workflows 目录里的 workflow，以及加载失败的 bundle（`--json` 给机器读） |
| `nightme workflow show <name>` | 展示一个 workflow 解析后的细节（`--json` 给机器读） |
| `nightme workflow run <name>` | dry-run：打印会执行什么，不真跑 |
| `nightme workflow validate [path...]` | 只做校验，报诊断 |
| `/wf install <owner>/<name>` | 从发布源装一个 workflow（`internal/command/wf`） |
| `/wf uninstall <owner>/<name>` | 卸掉它 |
| `/me start <workflow> [args...] [-i <rounds>]` | 在当前 chat 里跑一个 workflow |

workflows 目录由配置给出；action 脚本住在同一个目录下的 `actions/`。

---

## 9. Open questions

1. **触发器什么时候接线**：`on:` 的规格已经解析、校验、匹配，但还没有 cron 调度器和 git 事件源。接线时 workspace 的过滤、`runID` 的粒度和触发风暴的去重都要定。
2. **内置 action 什么时候不是桩**：`notify` / `email` / `github_*` 现在都返回 `stub: true`。真做的时候要定的是认证从哪来、失败怎么落成 outcome。
3. **run 状态要不要落盘**：现在 run 的状态只在内存里，daemon 重启就没了。落盘的话，跨 daemon 重启续跑需要解决工作目录和 chat 会话的对不上。
4. **用户 action 脚本的权限**：现在跟 daemon 同用户跑，没有降权。

---

## 10. Cross-references

- **用户参考**：[`../WFE.md`](../WFE.md) §5 (steps) §6 (expressions) §7 (examples) §8 (runtime architecture)
- **personal agent**：[`../me.md`](../me.md)
- **Channel 抽象**：[`./F-08-channel-abstraction.md`](./F-08-channel-abstraction.md)
- **Agent 抽象**：[`./F-09-agent-abstraction.md`](./F-09-agent-abstraction.md)
- **Gateway 分发**：[`../SPEC.md`](../SPEC.md) §2.1 inbound dispatch
- **状态持久化风格**：[`../CHATSTORE.md`](../CHATSTORE.md)
