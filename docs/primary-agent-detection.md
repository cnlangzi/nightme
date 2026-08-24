# Primary-Agent Auto-Detection

> **Status**: implemented
> **Scope**: `cfg.Primary` resolution chain, `internal/agent/registry.go` insertion order, `internal/bridge/pty` exclusion from `agent.Builtins`
> **读者**: 参与 runtime / gateway / chatsession / agentregistry / config 的工程师;新增 builtin 的 bridge 维护者
> **Related docs**:
> - [`SPEC.md`](./SPEC.md) §1.1 Primary Agent、§1.3 不变式
> - [`feat/F-09-agent-abstraction.md`](./feat/F-09-agent-abstraction.md) 三层抽象(AgentSpec / Starter / Agent)
> - [`feat/F-chat-session.md`](./feat/F-chat-session.md) §3 ChatSession.PrimaryAgent 只读语义
> - [`CHATSTORE.md`](./CHATSTORE.md) §3 ChatSession bootstrap 失败兜底

---

## 1. TL;DR

启动期 `cfg.Primary` 不再硬编码 `"claude"`,而是在 **user config > `NIGHTME_PRIMARY` env > 注册顺序探测 > 空** 的解析链上自动决定。每个新 chat 仍把探测到的结果固化到磁盘 —— 下次启动不会重复探测,直到用户手动改 `primary:` 或删掉那一行。

> 这一改动的根因是:用户机器上**可能根本没装 Claude**。继续写死 `"claude"` 会让 `chatstore.Bootstrap` 在第一次 inbound 时报 `need primaryAgent to create`,而 daemon 自己却没法帮用户修。探测 + 固化把"用户没装 Claude"和"用户装了一堆别的 CLI"两种情况都覆盖了。

---

## 2. Resolution chain(`cfg.Primary` 怎么落地)

`LoadDefault`(`internal/config/config.go:212`)按以下顺序解析,后者胜:

| # | 来源 | 实现 | 备注 |
|---|------|------|------|
| 1 | **YAML 配置文件** `primary:` 行 | `Load(path)` 走 `yaml.Unmarshal` | 用户显式声明;空字符串也算"显式声明空" |
| 2 | **`NIGHTME_PRIMARY` 环境变量** | `applyEnvOverrides`(`config.go:340`) | CI / 容器场景常用;空字符串**不覆盖**(实现见 `config.go:322` 的 `if v := ...; v != ""` 守卫) |
| 3 | **`agent.Builtins.List()` 顺序探测** | `detectPrimaryFromBuiltins`(`config.go:262`) | 首个 `Detect()` 返回 `nil` 的 starter 胜出;命中即 `SaveDefault` 落盘 |
| 4 | **空** `""` | 不写盘,直接 return | 由下游 `chatstore.Bootstrap` 兜底报错 |

```text
$ NIGHTME_PRIMARY=codex nightme start
    ├─ yaml 缺 primary:        跳过 (1)
    ├─ env PRIMARY=codex:      cfg.Primary = "codex"  ✓ 不探测
    └─ SaveDefault?            否 (Primary 已经非空)
```

```text
$ # 全新安装,只装了 opencode
$ nightme start
    ├─ yaml 缺 primary:        跳过 (1)
    ├─ env 未设:               跳过 (2)
    ├─ Builtins.List() 探测:
    │     claude    → Detect() error  ✗
    │     codex     → Detect() error  ✗
    │     dsh       → Detect() error  ✗
    │     opencode  → Detect() nil     ✓ → cfg.Primary = "opencode"
    └─ SaveDefault:            写 ~/.nightme/config.yaml(仅 primary: opencode)
```

---

## 3. Registration order = priority chain

`agent.Registry`(`internal/agent/registry.go:33-37`)用 map + `order []string` 双结构:

```go
type Registry struct {
    mu      sync.RWMutex
    entries map[string]Starter
    order   []string  // 首次插入顺序,append-only
}
```

`Register`(`registry.go:60-71`)在 entry 首次出现时 `append` 到 `order`;re-registration **不移动位置** —— 这是契约:`TestList_PreservesInsertionOrder` (`registry_test.go:111-160`) 是这条契约的回归锁。

`List()`(`registry.go:84-95`)按 `order` 顺序遍历 `entries`。这意味着 **`cmd/nightme/agents.go` 的 `init()` 顺序就是探测优先级**:

```go
// cmd/nightme/agents.go:35-105
func init() {
    agent.Builtins.Register(claudecode.NewStarter("claude", "claude", nil))    // line 39  ← 优先级 1
    agent.Builtins.Register(codex.NewStarter("codex", "codex", nil))          // line 46  ← 优先级 2
    agent.Builtins.Register(dsh.NewStarter("dsh"))                            // line 65  ← 优先级 3
    agent.Builtins.Register(opencode.NewStarter("opencode", "opencode", ...)) // line 85  ← 优先级 4
    agent.Builtins.Register(cursor.NewStarter("cursor", "agent", ...))        // line 97  ← 优先级 5
    agent.Builtins.Register(pi.NewStarter("pi", "pi", nil))                    // line 105 ← 优先级 6
}
```

### 3.1 新增 builtin 的规则

`cmd/nightme/agents.go:7-11` 的 doc 注释明确:**append to the END**。理由:

- 把新 starter 插到中间会**永久**改变所有现有用户的探测优先级(如果他们两个都装了)。
- 现有的顺序是 hand-curated —— `claude` / `codex` 是 v0.1 MVP 已有的,后续加入的(`opencode` / `cursor` / `pi`)按"实验性 → 主流"自然下沉。
- 改探测优先级等同于改产品决策,不该是 side effect。

### 3.2 `cursor` 那行的 binary 是 `cursor-agent`,不是 `cursor`

`cursor.NewStarter("cursor", "cursor-agent", []string{"acp"})` 的第二个参数是 command(`exec.LookPath` 用的 binary 名),不是 name。Cursor CLI 安装后挂在 PATH 上的是 `cursor-agent` binary:bash installer (`curl https://cursor.com/install`) 在 `$HOME/.local/bin/cursor-agent` 创建 legacy symlink(主名 `agent`),PowerShell installer (`https://cursor.com/install?win32=true`) 在 `%LOCALAPPDATA%\cursor-agent\cursor-agent.cmd` 创建真实入口(并额外拷贝一份 `agent.cmd` 作为 alias)。Bridge 选 `cursor-agent` 是因为它是两个 installer 都创建的"真名字"",不依赖 installer 的 alias 创建逻辑。

---

## 4. Why PTY is not in Builtins

`internal/bridge/pty` 是 **shared infrastructure**,不是 user-facing agent。早期版本(`cmd/nightme/agents.go:107-116`,已删除)把它作为 `"bash"` 注册到 `agent.Builtins`,但用户心智模型里 "primary agent" 指的是 AI coding CLI,不是 shell fallback。

`internal/bridge/pty` 现在的角色:

1. **claudecode 的 PTY 兜底**:`agentregistry.Build`(`internal/agentregistry/agentregistry.go:64`)在为 `cfg.Agents` 条目构造 PTY starter 时直接 `pty.NewStarter(...)`,不经过 `agent.Builtins`。
2. **claudecode / opencode 等 bridge 内部**:各自的 driver 需要 PTY transport 时 `import "internal/bridge/pty"` 直接用,不通过 registry 间接。
3. **用户显式声明**:`cfg.Agents` 里写 `- name: bash / command: bash` 仍可用(走 agentregistry 那条 PTY 路径)。

把 pty 从 `agent.Builtins` 移走**不影响**以上任何一条 —— 它们本来就不依赖 `Builtins` 看到 `bash`。删掉之后:

- `nightme agents` 不再列 `bash`(符合预期)。
- 自动探测不会因为 `/bin/bash` 几乎总存在而错误地把 bash 当成 primary。
- `/use bash` 不再是合法命令(用户报错信息更明确:unknown agent)。

---

## 5. Detection failure semantics

`detectPrimaryFromBuiltins`(`internal/config/config.go:262-271`)全失败 → 返回 `""`,`LoadDefault` **不写盘**,`cfg.Primary == ""`。

后续路径:

1. **Daemon 启动**(`internal/runtime/runtime.go:429`):`buildStackOpts.primaryAgent = cfg.Primary` 是空字符串。
2. **第一次 inbound 进来**:`Manager.GetOrCreate`(`internal/chatsession/manager.go:225`)→ `constructChatSession` → `chatstore.Bootstrap(chatID, "")`。
3. `chatstore.Bootstrap`(`internal/chatstore/store.go:217-244`)在 entry 不存在且 `primaryAgent == ""` 时显式报错:

   ```
   chatstore: chatID not on disk; need primaryAgent to create
   ```

4. inbound dispatcher 把错误转发给 channel,用户看到一条提示。

我们**故意**不写空 `primary: ""` 到磁盘 —— 让用户能继续手工编辑文件 / 安装新 agent,下次启动再试一次。如果写了空串,反而需要新增"清空 Primary"的反向操作。

---

## 6. Code map(改动文件 + 行号)

| 文件 | 关键位置 | 改动 |
|------|---------|------|
| `internal/agent/registry.go` | `Registry` struct (33-37);`Register` (60-71);`List` (84-95) | 新增 `order []string`,`List` 按 order 返回 |
| `internal/config/config.go` | `LoadDefault` (212-241);`detectPrimaryFromBuiltins` (262-271);`applyDefaults` (289-...) | 删 `c.Primary = "claude"` 写死,改 LoadDefault 末尾探测 |
| `cmd/nightme/agents.go` | init (35-105);doc 注释 (1-32) | 删 `pty.NewStarter("bash", ...)` + `runtime` import;doc 注释加 "PTY 不是 builtin" 说明 |
| `cmd/nightme/agents_cmd.go` | `runAgents` (77-95);`printAgentsTable` (130-150);doc 注释 (1-29) | 删 `if defaultName == "" { defaultName = "claude" }` 兜底;footer 直接透传 `cfg.Primary` |
| `internal/agent/registry_test.go` | `TestList_PreservesInsertionOrder` (新增) | 锁住插入顺序契约 |
| `internal/config/config_test.go` | `TestDefaults` / `TestMissingFile` (改 expect);`TestLoadDefault_AutoDetectsPersistsAndIsIdempotent` (新增) | 验空 + 验持久化幂等 |

---

## 7. Cross-cutting impact check

`cfg.Primary` 的旧 default 行为变更影响到的所有代码路径:

| 调用方 | 影响 | 状态 |
|--------|------|------|
| `cmd/nightme/agents_cmd.go::runAgents` | footer 直接透传 `cfg.Primary`,空时省略 | 改 |
| `internal/runtime/runtime.go:429,581` | 透传给 `WithPrimaryAgent`;空时 `chatstore.Bootstrap` 报错 | 不改(行为已正确) |
| `internal/chatsession/manager.go::GetOrCreate` | `constructChatSession` 的 `primaryAgent` 参数空时 `Bootstrap` 报错 | 不改 |
| `internal/chatsession/chatsession.go::New` | `selectedAgent` / `primaryAgent` 都从构造参数取 | 不改 |
| `chatstore.Bootstrap` | 空 primary 报错 | 不改(就是兜底) |

所有路径都已被新行为覆盖,没有需要补的 failure mode。
