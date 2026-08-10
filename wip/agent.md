# nightme Agent 三件套:Info / Starter / Agent

> **历史参考**:设计已落地,见 `git log --oneline 38d085f..ee0fdf1`。
> 实际状态以代码为准(`internal/agent/agent.go`、`internal/agent/registry.go`)。
> 实施过程中几个偏离原设计的地方见末尾"实施记录"。

---

## 背景

`internal/agent/agent.go` 当前把 Agent 切成两块:`AgentSpec` interface 和 `Agent` interface(后者嵌入前者)。`AgentSpec` 的 6 个方法里,5 个是**固定值访问器**(Name/Mode/Command/Args/Env),只有 1 个是真行为(Detect)。

Go 的 interface 是给多态行为用的——`io.Reader` 形态各异但"读"是一个动作。5 个返回固定字符串/切片的 getter 挂在 interface 上,**没有任何 polymorphic 价值**,每个实现里都长一样(`return a.name`)。这是 over-abstraction。

与此同时,真正的多态只发生在 Start:每桥的 fork+exec+handshake 流程完全不同,SendText/SendBlocks 的编码也不同,但当前把所有行为糅在一个大 interface 里,让 `Registry.Specs() []AgentSpec` 这种"窄接口"绕路显得必要。

## 核心洞察

- **spec 半是数据,不是契约**——不该是 interface
- **真正的多态只有 Start**——其他行为虽然编码不同,但调用形态一致
- **runtime 半是具体值,不是抽象**——PID、events、close 都是具体字段,可以收口共享

## 三件套

| 类型 | 形态 | 角色 |
|---|---|---|
| **Info** | struct | 静态元数据快照。值类型,可独立持有/序列化/比较。 |
| **Starter** | interface | 启动配方 + Start 行为。多态点。 |
| **Agent** | struct | 运行中句柄。具体类型。跨桥公共 runtime 字段在这里收口。 |

**Info 是从 Starter/Agent 上能"推断"出的元数据子集**——不是独立注册的实体,而是观察面。"Infer" 这个词描述读取方向:你拿一个句柄,能读到这些值。

**Starter 是唯一的 interface**——因为 Start 真的每个桥都不一样。Bridges 注册 starter 到 registry;Spawner 调 `starter.Start(...)` 拿回 `*Agent`。

**Agent 是具体 struct**——所有 runtime 方法(PID/Events/Send*/Close/New)形状一致,真正变化的是实现,实现藏在内部的 driver 里。`Agent` 收口的跨桥公共字段:pid、events、sessionID、closeOnce、closed。

## os/exec 三元对照

| os/exec | nightme | 对位诚实度 |
|---|---|---|
| `Cmd.Path/Args/Env` | `Info.Name/Mode/Command/Args/Env` | ✅ 完全对位 |
| `Cmd.Start()` | `Starter.Start(ctx,cfg) (*Agent, error)` | ⚠️ 多态所以是 interface,而非 struct |
| `Cmd.Process` | `*Agent`(返回值) | ✅ 对位 |
| `Process.Pid` | `Agent.Pid` | ✅ 字段 |
| `Process.Signal/Kill/Wait` | `Agent.SendText/SendBlocks/Close/New` | ⚠️ 名字不对位(Process 是 OS-level,Agent 是 protocol-level),但概念对位 |
| `ProcessState.ExitCode()` | ❌ 缺失(归位到 `AgentSession.exitCode`) | ⚠️ 故意不对位——见下 |

**两个故意不对位的点**:

1. `Cmd.Start()` mutate self vs `Starter.Start(...) (*Agent, error)` 返回新对象——因为 nightme 的 `Builtins` 需要保持模板干净,同一 starter 被多次 Start 出多个独立 session。
2. `ProcessState.ExitCode` 在 os/exec 归 Process,在 nightme 归 AgentSession——因为我们有 session 概念,退出码属于 session 生命周期而非 agent 本身,**归位更合理**。

类比对位 ~80%,不对位的两点都有合理理由,不靠歪曲事实凑类比。

## 信息流向

```
冷路径(列表/打印):
  Builtins.Get(name) → Starter → Info()

热路径(运行时):
  Spawner.Spawn(ctx, name, cfg)
    → Builtins.Get(name) → Starter
    → Starter.Detect()
    → Starter.Start(ctx, cfg) → *Agent
    → AgentSession.handle = live

持久化/恢复:
  *Agent.sessionID (跨桥统一字段)
  *Agent.pid (跨桥统一字段)
  → AgentSession.SetSessionID / PID 通过公共方法读取
```

## 语义化

**"Spec" 这个词在旧设计里是隐喻错了**——它暗示"spec 是和 live 平行存在的另一种东西"。新设计里没有"spec 实体"这个概念:Info 只是 Agent/Starter 上的一个观察面。读取方向叫"Infer"。

**"Starter"是接口,Agent 是 struct**——这是有意的分工。Start 形态多到无法收口成一个 struct;Agent 的 runtime 形态已经收敛,可以收口成共享 struct。

**driver 是隐藏的策略接口**——每个 bridge 实现自己的 driver,把 Send*/Close 的协议细节封装在内,不出 public API。Agent 的方法委托给 driver。

## 为什么这个方案契合 nightme

- **`Registry.Specs() []AgentSpec` 直接消失**——冷路径用 `starter.Info()` 拿值
- **`AgentSession.handle` 是具体 `*Agent`**——零接口 dispatch,直接调方法
- **跨桥重复的 runtime 字段收口**——`pid`/`events`/`closed`/`closeOnce`/`sessionID` 从 5 桥各写一遍变成 1 处
- **mock 测试样板减少**——fake starter 3 方法 + fake driver 5 方法,职责清晰
- **新增 bridge 模板化**——写一个 starter + 一个 driver,注册 starter,完成

## 不靠 os/exec 类比强撑的地方

- **Transport 层还在**——nightme 的 `bridge/pty/Transport` interface(`io.ReadWriteCloser + PID`)是 driver 之下的字节流抽象。driver 管协议编码,Transport 管字节 IO,两层职责分明。acp bridge 直接用 pty.Transport。
- **bridge 内部类型完全封装**——bridge 包外的代码拿不到 driver 指针,只能拿到 `*Agent`。任何 bridge 特有的方法(如 claudecode 的 stderr 流捕获)都是 driver 内部细节,不进 public API。
- **Agent 没有 exit code**——退出码归 AgentSession,因为 exit 是 session 结束事件,不是 agent 属性。

---

## 实施记录(跟设计文档的偏差)

实际落地过程中几个跟原计划不同的点:

### 1. 临时命名 LiveAgent

设计上叫 `Agent`,但旧 `Agent` interface 必须保留到 P4 才删(避免一次性爆炸半径)。为了同时存在,**临时**命名新 struct 为 `LiveAgent`,P4 完成重命名为 `Agent`。

过渡期 adapter 同样临时:
- `agent.LiveAgent` + `agent.NewLiveAgent` (P1-P3)
- → `agent.Agent` + `agent.NewAgent` (P4 后)

### 2. Spawner.Spawn 提前到 P3 后半就切到 *LiveAgent

计划说 P3 阶段切换。实际把签名改动放到 P3 末的 commit,配合 `WrapAsAgent` adapter 处理边界。P4 时 `WrapAsAgent` 删除,`Spawner.Spawn` 直接返回 `*Agent`。

### 3. driver interface 包私有,需要本地别名做编译期检查

`agent.driver` 是包私有 interface,bridge 包无法用 `var _ agent.driver = ...` 检查。解决方案:每个 bridge 文件加一个 `agentDriver` 本地别名 interface,做同样的 5 方法声明,编译期检查驱动 `var _ agentDriver = (*driver)(nil)`。

### 4. pty 的 Cols/Rows 移到了 Starter 构造参数

pty 桥原来 `Agent` struct 有 `Cols`/`Rows` 导出字段。新模型里 Starter 构造时直接接收 `cols, rows int`,Driver 不持有这两个字段(由 newDriver 局部变量用)。

### 5. NewLiveAgent 接受 `driver interface{}`

driver 包私有,外部包拿不到 `driver` 类型。`NewLiveAgent` 接收 `interface{}` 并在内部 `d.(driver)` 类型断言。这个模式跟 os/exec `Cmd` 用 `any` 接收任意 command 字段类似。

### 6. 双轨期实际没那么"双"

理论上 claudecode/pi/codex/acp/pty 5 桥全迁后,LlveAgent 路径完全取代旧 AgentSpec+Agent。但实际注册顺序里 claudecode 是最早迁的,迁移期间旧 Agent 还能通过 `LegacyRegister` 注册。P4 删除 `LegacyRegister` / `AsStarter` / `WrapAsAgent` 后,5 桥统一走 `Register(starter)`。

---

## 提交链总览

```
38d085f  refactor(agent): introduce Info/Starter/LiveAgent/driver triple
f89a433  refactor(claudecode): split into Starter + driver
6773ed4  refactor(pi): split into Starter + driver
82f253d  refactor(codex): split into Starter + driver
de7e55a  refactor(acp): split into Starter + driver
c04f980  refactor(chatsession): handle = *LiveAgent; Spawner returns *LiveAgent
2a4ac8e  test: align fakes with *LiveAgent refactor
d2842dd  refactor(pty): split into Starter + driver
f2093d0  refactor(agent): delete legacy AgentSpec + Agent interfaces
ee0fdf1  refactor(agent): rename LiveAgent → Agent
```