# F-DSH-CHAT-001 — dsh Chat-Session Event Translation 重构

> **Status**: 方案定稿,待前置 wire-probe 后开工
> **Scope**: `internal/bridge/dsh/` 内 `translate.go` / `protocol.go` / `session.go` 的事件翻译层重构
> **不动范围**: `internal/agent/` (AgentEvent 抽象)、`internal/channel/` (飞书渲染)、`internal/runtime/`、`internal/chatsession/` —— **零改动**
> **姊妹文档**:
> - [F-dsh-bridge.md](./F-dsh-bridge.md) — dsh print-mode RunOnce 设计(已落地)
> - [bridge/dsh.md](../bridge/dsh.md) — dsh wire / lifecycle 详细规范
> - [F-CLAUDE-PRINT-002-statusbar-refactor.md](./F-CLAUDE-PRINT-002-statusbar-refactor.md) — 跨包重构的失败案例,作为本方案的"避免踩坑"参考
>
> **2026-08-16 修订 (dashboard 对齐)**: dsh web 的 **To-dos / 任务** 条(`TodoDock`)对齐 `todo/write` `{todos:[{content,status}]}` + `session/projection` `key:"todos"`(`value` 是 `TodoItem[] | null` 数组,不是 `{todos:[...]}` 包装)。`step/start` / `step/end` 是 `sessionStats`(TTFT / tok/s),**不要**映射成 OutTask*。聊天里的 `todo_write(...)` 行是 `tool/call`,不是 dock 清单。权威表:[dsh-api.md §3.4.2–3.4.3](../bridge/dsh-api.md)。

---

## 0. 文档目的

2026-08-15 排查:dsh web 的 To-dos 面板上能看到任务清单(Read / Bash / 各步骤),但 `internal/bridge/dsh/translate.go` 在翻译 `session/event` 流时丢数据,导致 nightme 这边根本看不到 Tool 列表 / To-dos。

本文档记录:
- 根因(为什么丢)
- 治本方案(只在 dsh 包内改)
- 前置条件(wire-probe / 锁边界 / 跨桥调研)
- 分阶段落地路径

**核心原则(用户 2026-08-15 锁定)**: 平台抽象(`agent.AgentEvent`)稳定,所有 dsh 桥特有的智能化收敛在 `internal/bridge/dsh/` 包内,不污染上层抽象。

---

## 1. 症状(用户报告)

### 1.1 现象

dsh web UI(`127.0.0.1:3080`)的 To-dos 面板正常显示任务清单:

```
✅ Read docs/SPEC.md (architecture, invariants, lifecycle, persistence)
✅ Read docs/bridge/dsh.md (full design spec for dsh bridge)
🔄 Read docs/feat/F-dsh-bridge.md (print-mode phase design)
⚪ Read all bridge/dsh/*.go files (...)
⚪ Cross-check spec vs implementation: spawn, lifecycle, handshake, ...
⚪ Cross-check SPEC.md invariants: ChatSession/AgentSession imports, ...
⚪ Summarize: which features are fully implemented, partially, or missing
```

但同一会话在 nightme 这边(经 dsh bridge 翻译后)看不到任何 Tool 列表 / To-dos 事件。

### 1.2 影响

- **To-dos 面板在 channel(飞书)侧渲染为空**
- Tool 调用过程(`tool/call` → `tool/result`)正常,但 task list / project progress 信息完全丢失
- 用户无法在飞书侧看到 agent 正在做什么 / 进度如何

### 1.3 已排除的无关因素

- 不是 wire 协议解析错误(已确认 `assistant/chunk` / `tool/call` 等基础事件正常翻译)
- 不是 runtime / chatsession 层丢事件(其他 bridge 同样流程 OK)
- 不是 AgentEvent 通道阻塞(同会话的 `EventAgentText` / `EventAgentToolStart` 正常)

**问题域**: dsh bridge 的 **事件翻译层** 把 wire 事件压扁成 AgentEvent 时丢了 To-dos 维度。

---

## 2. 根因分析

### 2.1 dsh wire 的三路数据源(dsh web 的真实形态)

dsh web 同时下发三种并行数据源,共同描述 session 的"真值":

| 数据源 | 何时下发 | 内容 |
|---|---|---|
| `session/event` | 每条 SessionEvent 伴随下发 | 原始事件流(text delta / tool_call / tool_result / turn 边界 / `todo/write` 等 42 种 SessionEvent) |
| `session/projection` | host 在 `todo/write` / `turn/start` 等变化时下发 | 派生状态快照。To-dos 的 key 是 **`todos`**(复数);`value` 是 `TodoItem[] \| null` |
| `muxSessionEvent.View` | 每条 `session/event` 伴随下发 | host 已计算好的渲染视图(`ToolEventView`),含 tool 状态 + task 列表 + uuid |

详见 [bridge/dsh.md §1](../bridge/dsh.md) 的 wire 规范。

### 2.2 当前 `translate.go` 的封闭 switch(三个洞)

```go
// internal/bridge/dsh/translate.go - 当前实现
func (t *translator) handleSessionEvent(ev muxSessionEvent, deliver func(agent.AgentEvent)) {
    var env sessionEventEnvelope
    json.Unmarshal(ev.Event, &env)
    t.active = true
    switch env.Type {
    case "assistant/chunk":
        // 翻译 text delta
    case "tool/call":
        // 翻译 EventToolStart
    case "tool/result":
        // 翻译 EventToolEnd
    case "todo/write":
        // 翻译 EventAgentTaskCreate
        // ❌ Bug: todoItem struct 没有 ID / ActiveForm 字段
        //    AgentTaskItem{ID:"", Subject:..., Status:...}
        //    runtime 收到 ID 为空的整张 list,做合并/去重会失效
    case "approval/asked":
        // 走 permissions.go
    // ... 共 9 个 case
    default:
        dLog("dsh: session.event unknown type=%q", env.Type)
        // ❌ Bug: 未知 type 静默 dLog,生产环境 dsh 加新事件 bridge 静默不工作
    }
    // ❌ Bug: env.View (ToolEventView) 完全没被读取,直接丢弃
    //    host 算好的 task list / tool 状态在这里蒸发
    // ❌ Bug: session/projection 帧根本不走 handleSessionEvent,
    //    在 handleMuxFrame 里被默认分支消化
}
```

### 2.3 三个洞分别是什么

| # | 洞 | 后果 |
|---|---|---|
| **D1** | `todoItem` struct 缺 `ID` / `ActiveForm` 字段 | `EventAgentTaskCreate.Items[].ID == ""`,runtime 端按 ID 合并 / 去重 / 增量更新全部失效 |
| **D2** | `muxSessionEvent.View` 完全不读 | dsh host 已算好的 To-dos 列表(含 uuid / activeForm / 聚合状态)整段蒸发 |
| **D3** | `session/projection` 帧不在 dispatch 表里 | To-dos 完整快照 + Title 变更路径完全无路可走(文档说"不发 event",实际上没给翻译层机会) |
| **D4** | `switch` 封闭派发 + default 静默 dLog | dsh 加新 SessionEvent 时,bridge 静默不工作,生产环境无 actionable signal |

### 2.4 为什么这是结构性问题(不是 bug fix)

dsh wire 模型天生是 **三条数据流并行** 描述真值。任何"wire → AgentEvent 一对一开关"翻译模式都会丢数据,因为:

- raw event 是"动作流"(tool_call ↔ tool_result 配对必须靠 raw)
- View 是"渲染态"(host 已合并 / 推断,比 raw 更准)
- Projection 是"聚合快照"(title / tasks 等聚合维度)

三层各管一段,无法用单一 switch 表达。**治本 = 承认 dsh 是多数据源结构,而不是单事件流。**

---

## 3. 目标 & 非目标

### 3.1 目标

1. **D1/D2/D3 全消解**: dsh web UI 的 To-dos 面板 / Tool 列表 / Title 更新在 nightme 侧可见
2. **D4 治本**: dsh 加新 SessionEvent 类型时,bridge 通过"注册 handler"扩展,不需要改 switch
3. **平台抽象零改动**: `internal/agent/` / `internal/channel/` / `internal/runtime/` / `internal/chatsession/` 零改动
4. **回归测试可写**: 每个 handler 都有 fixture / 单测覆盖,wire 演化有迹可循

### 3.2 非目标(明确不做)

- ❌ 不新增 AgentEvent kind(不动 `internal/agent/agent.go`)
- ❌ 不改 channel 层(feishu / cli 不适配 dsh 特有 wire)
- ❌ 不改 runtime / chatsession 层
- ❌ 不做 wire-schema 自动生成(留给后续 P4 评估)
- ❌ 不改 dsh web 本地配置(继续遵循 `agent-no-config-tampering`)
- ❌ 不重写 print-mode RunOnce 路径(`print.go` 不动)

---

## 4. 设计:dsh 包内的三层组件

### 4.1 架构总览

```
┌────────────────────────────────────────────────────────┐
│ Layer 1 — Wire Ingest(解码 + 永不丢) │
│                                                        │
│   mux frame ──► dispatcher                              │
│     ├─ session/event   → dispatcher.Dispatch(env, view) │
│     ├─ session/projection → wireState.applyProjection() │
│     └─ unknown method  → dLog(dsh 包内,不上抛)          │
│                                                        │
└────────────────────────────────────────────────────────┘
                            │
                            ▼
┌────────────────────────────────────────────────────────┐
│ Layer 2 — wireState(dsh 内部真值镜像)                   │
│                                                        │
│   wireState{ Title, Tasks[by ID], Tools[by CallID] }    │
│   由 dispatch + applyProjection 两路喂数据             │
│   state 变更 → 算 delta → 转 AgentEvent 序列           │
│                                                        │
└────────────────────────────────────────────────────────┘
                            │
                            ▼
┌────────────────────────────────────────────────────────┐
│ Layer 3 — Adapter(AgentEvent 产出)                     │
│                                                        │
│   StateDelta → []agent.AgentEvent → deliver(...)        │
│     TaskAdded/Updated → EventAgentTaskCreate/Update    │
│     ToolStarted/Finished → EventAgentToolStart/End      │
│     TitleChanged → EventAgentTaskCreate(空更新触发)     │
│                                                        │
└────────────────────────────────────────────────────────┘
```

**所有组件都在 `internal/bridge/dsh/` 包内**,platform 抽象零改动。

### 4.2 文件分布

```
internal/bridge/dsh/
├── state.go             (新) wireState + applyProjection + diff
├── dispatch.go          (新) eventDispatcher + 注册表 + handler 签名
├── handle_mux.go        (新) 从 session.go 拆出 mux frame 分发
├── translate.go         (改) 9 个 case 拆成 9 个独立 handler,塞进注册表
├── protocol.go          (改) todoItem 加 ID / ActiveForm;新增 projectionEnvelope / ToolEventView
├── session.go           (改) driver 持有 wireState 实例
├── translate_regression_test.go  (改) textBuf 测试保留 + 加 dispatcher 派发测试
└── testdata/
    ├── envelopes/       (新) 各 event 类型的真实 raw payload
    ├── views/           (新) ToolEventView 的真实结构
    └── projections/     (新) session/projection 帧的真实结构
```

### 4.3 组件 1:`wireState`(dsh 内部真值)

```go
// internal/bridge/dsh/state.go
type wireState struct {
    mu       sync.Mutex
    title    string
    tasks    map[string]todoItem // by ID(从 View / Projection 拿)
    tools    map[string]toolItem // by CallID(从 View 拿)
    inflight map[string]bool     // tool_call 等待 result 配对
}

// 单事件流入口(raw event + 伴随 View)
func (s *wireState) applyEvent(env sessionEventEnvelope, view json.RawMessage, prev *snapshot) []agent.AgentEvent

// Projection 单独入口(host 周期性下发)
func (s *wireState) applyProjection(proj projectionEnvelope) []agent.AgentEvent
```

**关键设计**: `wireState` 是 dsh 包内私有,`applyEvent` / `applyProjection` 返回的是 `[]agent.AgentEvent` 序列(已经是平台层概念),由 dispatcher 调用 `deliver` 投出。

### 4.4 组件 2:`eventDispatcher`(干掉 switch 的根)

```go
// internal/bridge/dsh/dispatch.go
type eventHandler func(env sessionEventEnvelope, view json.RawMessage, st *wireState) []agent.AgentEvent

var eventRegistry = newRegistry(map[string]eventHandler{
    "assistant/chunk":   handleAssistantChunk,
    "assistant/message": handleAssistantMessage,
    "tool/call":         handleToolCall,
    "tool/result":       handleToolResult,
    "turn/start":        handleTurnStart,
    "turn/end":          handleTurnEnd,
    "compaction/end":    handleCompactionEnd,
    "todo/write":        handleTodoWrite,
    "todo/update":       handleTodoUpdate,    // 提前注册,handler 现在返回 nil
    "todo/delete":       handleTodoDelete,    // 同上(graceful)
    "approval/asked":    handleApprovalAsked,
})

func dispatch(env sessionEventEnvelope, view json.RawMessage, st *wireState, deliver func(agent.AgentEvent)) {
    h, ok := eventRegistry.lookup(env.Type)
    if !ok {
        dLog("dsh: unknown event type=%q", env.Type)  // dsh 包内 log
        return                                          // 不上抛平台
    }
    for _, e := range h(env, view, st) {
        deliver(e)
    }
}
```

**为什么是注册表不是 switch**:
- 加新事件 = 加一行 + 写 handler,**不动 switch 不动 default**
- 提前注册尚未实现的 type(如 `todo/update`),让 dsh web 加这种事件时 bridge 自动开始处理(即使 handler 返回 nil 也比静默 dLog 强)

### 4.5 组件 3:`handleMux.go`(拆 mux 分发)

```go
// internal/bridge/dsh/handle_mux.go (从 session.go 拆出来)
func (d *driver) handleMuxFrame(frame serverFrame) {
    switch frame.Method {
    case "session/event":
        var ev muxSessionEvent
        json.Unmarshal(frame.Payload, &ev)
        d.dispatch(ev.Event, ev.View, d.wireState, d.deliver)
    case "session/projection":
        var proj projectionEnvelope
        json.Unmarshal(frame.Payload, &proj)
        d.wireState.applyProjection(proj)
    case "session/subscribed":
        // baseline seq 记录(原逻辑)
    default:
        dLog("dsh: unknown mux method=%q", frame.Method)
    }
}
```

**关键改动**: `session/projection` 不再被 default 分支吞掉,走 `applyProjection` 单独通道。

### 4.6 View 字段的权威性边界(详细)

不是简单的"projection > view > raw event",而是按 **用途** 分:

| 数据 | 权威源 | 原因 |
|---|---|---|
| tool_call ↔ tool_result 配对 | raw event | 必须按顺序配对,View 是聚合后的最终态,配对已"过气" |
| tool 当前状态(running / completed) | View | host 已合并 tool_call + tool_result + 错误状态 |
| task list 当前完整快照 | Projection | host 周期性下发,最权威 |
| task 单条增量状态变更 | raw event `todo/update` + View | 双源叠加 |
| session title | Projection | 只有 projection 带 |
| text delta | raw event | View 不带 |

**结论**: 不写死的优先级,handler 内按字段决定读哪个源。

---

## 5. 前置条件(必须先做的 3 件事)

### 5.1 Wire Probe —— 跑真 dsh 拿真实 payload

**为什么必须**: 写 dispatcher handler 时,`json.Unmarshal` 用的字段名(`id` / `uuid` / `activeForm` / `tasks` vs `items`)是**猜的**,不是验证过的。猜错 = bug 制造器。

**做法** (半天工作量,不改代码):

1. 启一个真 dsh web 实例(`pnpm dsh --profile web`)
2. 用 Claude Code agent 跑一个会触发 TodoWrite 的 prompt(例如 "做 3 个步骤的任务,每步用 TodoWrite 标记")
3. 抓 mux WebSocket 流量(用 `mitmproxy` 或 chrome devtools),导出:
   - 一帧 `session/event` (带 `todo/write`)
   - 一帧 `session/event` (带 `tool/call`)
   - 一帧 `session/event` (View 字段存在的情况)
   - 一帧 `session/projection` (如果存在)
   - 一帧未识别的 envelope(看 default 分支触发什么)
4. 原始 JSON 存到 `internal/bridge/dsh/testdata/` 下:
   - `testdata/envelopes/todo_write.json`
   - `testdata/envelopes/tool_call.json`
   - `testdata/views/tool_call_view.json`(如果有 View)
   - `testdata/projections/todo_snapshot.json`(如果有 projection)

**不做这步,handler 写出来就是空架子**。

### 5.2 锁边界决策 —— wireState vs translator 的状态分界

**问题**: 当前 `translator` 已经有自己的锁 + per-turn 状态(`t.textBuf`, `t.pendingText`, `t.pendingTools`, `t.textDelivered`, `t.active`)。新 `wireState` 要存 tasks / tools / inflight。两套状态怎么分?

**待决策的 3 个选项**:

| 选项 | 描述 | 优点 | 缺点 |
|---|---|---|---|
| A | `pendingTools` 也搬进 `wireState` | 单源真值,tool_call/result/View 三路一致 | 改 translator 测试,风险高 |
| B | `wireState` 只管 tasks/tools/inflight,`pendingTools` 留 translator | 改动小,职责清晰 | 两个 map 同 key,需要明确不变量 |
| C | 把 `translator` 整个并入 `wireState` | 完全统一 | 等于重写 translator,风险最高 |

**默认建议: B**。在 P1 PR 描述里写一段"为什么 pendingTools 留在 translator"或"为什么搬进 wireState",写不出来说明设计还没收敛。

### 5.3 跨桥调研 —— 其他 bridge 是否有同样问题

**为什么**: 如果其他 bridge(claudecode / codex / opencode / pi)也有 raw event + projection + view 的多数据源问题,那 wireState 模式应该抽到 `internal/bridge/common/`(公共包)。如果不抽,以后每个 bridge 都要重写一遍。

**做法** (5 分钟 grep):
```bash
grep -l "projection\|view:\|host.*computed" internal/bridge/*/protocol.go
```

**决策**:
- 如果只有 dsh 有这问题 → wireState 留在 dsh 包(YAGNI)
- 如果 ≥2 个 bridge 有 → 抽到 `internal/bridge/common/state.go`,dsh 包用公共组件

---

## 6. 风险与缓解

### 6.1 Blocker(错了必须回滚)

| 风险 | 触发条件 | 缓解 |
|---|---|---|
| **B1**: wireState 与 translator 状态脱节 | `pendingTools` 在 translator,`tools` 在 wireState,同一个 callID 双源不一致 | 5.2 锁边界决策明确写文档,handler 内显式选边 |
| **B2**: `Projection` 语义猜错(full snapshot vs incremental) | `applyProjection` 误把 incremental 当 full → wireState 进入中间态 | 5.1 wire-probe 必须覆盖 projection 帧 |
| **B3**: `TodoItem.ID` 字段不存在 | `todoItem{ID:""}` → `st.tasks[it.ID] = it` 全撞同一 key | 5.1 wire-probe 看 View.Tasks 字段;若真没 ID,fallback 用 Content+Status hash |

### 6.2 Important(影响落地节奏)

| 风险 | 缓解 |
|---|---|
| **I1**: 锁嵌套死锁 | dispatcher 入口统一 acquire 所有锁,handler 内不许加锁;或单源真值 |
| **I2**: 测试 fixture 缺失 | 5.1 wire-probe 是 P1 编码前置条件 |
| **I3**: P1 完成后用户看不到收益 | P1+P2 合并为单 PR,Projection 通道先打通(让 To-dos 面板至少有 Projection 这条路渲染) |

### 6.3 Watch(值得监控)

| 风险 | 缓解 |
|---|---|
| **W1**: `unknown wire frame` 可观测性差 | P4 阶段加 dsh 包内 wireRingBuffer + debug dump |
| **W2**: handleMuxFrame 拆分合理性 | 看 session.go 实际行数再决定拆不拆 |
| **W3**: 默认开启 vs 默认关闭 | 默认**关闭**,build tag 控制,等 P1-P2-P3 全稳后 flip |

---

## 7. 分阶段落地

### 7.1 Phase 1+2 合并(单 PR) —— "Translation 表重构 + Projection 接通"

**目标**: To-dos 面板至少通过 Projection 路径能渲染;raw event 派发改成注册表;View 字段透传不消费。

**改动包**: `internal/bridge/dsh/` 单包

**包含**:
- 新增 `state.go` (wireState 骨架,只管 tasks map,其他字段后补)
- 新增 `dispatch.go` (注册表 + handler 签名)
- 新增 `handle_mux.go` (从 session.go 拆 mux 分发)
- `translate.go`: 9 个 case → 9 个 handler,`switch` 消失
- `protocol.go`: `todoItem` 加 `ID` / `ActiveForm` 字段;新增 `projectionEnvelope` struct
- `session.go`: driver 持有 `*wireState`,`handleSessionEvent` 改调用 `dispatch()`
- 新增 fixture: `testdata/envelopes/*.json` (从 5.1 wire-probe 拿)
- 新增测试:
  - `TestDispatcher_KnownTypesRoute` (9 个 case 全部命中)
  - `TestDispatcher_UnknownTypeLogsButDoesNotPanic`
  - `TestTodoWrite_PopulatesIDAndActiveForm`
  - `TestApplyProjection_UpdatesTasks`

**默认开关**: `bridge.dsh.dispatcher.v2 = false`(走旧 switch);flip 后走新 dispatcher

**工作量**: 1 PR / 2-3 天

### 7.2 Phase 3 —— "View 权威化"

**前置**: 5.1 wire-probe 拿到真实 ToolEventView 结构

**改动包**: `internal/bridge/dsh/` 单包

**包含**:
- `protocol.go`: 新增 `ToolEventView` struct(probe 出来的字段)
- `state.go`: `wireState.tools` map 接管 View 字段的 tool 状态
- `dispatch.go`: `tool/call` / `tool/result` handler 改读 View 优先
- 新增 fixture: `testdata/views/*.json`
- 新增测试: `TestView_AuthoritativeForToolStatus`

**工作量**: 1 PR / 2-3 天

### 7.3 Phase 4 —— "可观测性 + ring buffer"

**改动包**: `internal/bridge/dsh/` 单包

**包含**:
- `state.go`: 加 `wireRingBuffer` (最近 64 帧 raw frame)
- `state.go`: unknown event type 升级到 Warn 级别 + 计数
- 新增 `dump.go` + debug 命令: `nightme debug dsh dump-wire`
- `doc.go`: 加 ring buffer dump 说明

**工作量**: 1 PR / 1 天

### 7.4 Phase 5(可选 / 长期) —— "wire-schema 自描述"

**想法**: 启动时拉 dsh 的 schema 端点或读 TS 定义,自动生成 eventRegistry。运行时仍 fallback 到静态表。

**触发条件**: dsh wire schema 演化频繁(每 2 周一次),手工维护 handler 跟不上的情况下启动。

**工作量**: 评估 1 天 + 实现 3-5 天

---

## 8. 排错速查(部署后)

| 症状 | 根因 | 修法 |
|---|---|---|
| To-dos 面板仍为空 | 直播缺 `todo/write`;或只靠投影但 `value` 是数组而 decoder 当 object 解;或 resume 没读 history `projections` | 先确认 `todo/write` 字段是 `todos` 不是 `items`;投影形状见 [dsh-api.md §3.4.3](../bridge/dsh-api.md);`step/*` **不是**清单 |
| Tool 状态更新滞后 | View 没接进 wireState.tools | 确认 P3 已落地 |
| `unknown event type` 大量出现 | dsh 加了新 SessionEvent,注册表没追上 | `nightme debug dsh dump-wire` 看最近的 wire 帧,在 eventRegistry 加 handler |
| 锁死锁 | dispatcher handler 内部加锁 + 调用方已持锁 | 改用单源真值;或 dispatcher 入口统一 acquire |
| regression 测试通过但生产仍坏 | testdata fixture 与真实 wire 字段名不一致 | 重跑 wire-probe,更新 fixture |

---

## 9. 验收清单(Definition of Done)

按 Phase 拆分:

### Phase 1+2 DoD
- [ ] `state.go` + `dispatch.go` + `handle_mux.go` 三个新文件落地
- [ ] `translate.go` 的 `switch env.Type` 整段消失,9 个 case 变成 9 个独立 handler
- [ ] `protocol.go` 的 `todoItem` 含 `ID` / `ActiveForm` 字段
- [ ] `protocol.go` 含 `projectionEnvelope` struct
- [ ] `session/projection` 帧能触发 `EventAgentTaskCreate`
- [ ] 4 个新单测全绿
- [ ] e2e 测试: 真 dsh + Claude Code agent,跑 TodoWrite,飞书侧能看到 To-dos 面板
- [ ] 默认开关关闭,`bridge.dsh.dispatcher.v2 = true` 才走新路径
- [ ] 老 switch 路径仍可用,产出 AgentEvent 序列与新 dispatcher 一致(双跑对比)

### Phase 3 DoD
- [ ] `ToolEventView` struct 落地,字段匹配真实 wire
- [ ] `wireState.tools` 接管 View 字段
- [ ] `TestView_AuthoritativeForToolStatus` 通过
- [ ] e2e: Tool 调用过程中飞书侧能实时看到 status 更新

### Phase 4 DoD
- [ ] wireRingBuffer + dump 命令可用
- [ ] unknown event type 升级到 Warn
- [ ] 文档:`doc.go` 加 ring buffer 说明

---

## 10. 时间线(预估,单人)

| 阶段 | 内容 | 前置 | 工作量 |
|------|------|------|--------|
| **0** | Wire-probe + 跨桥调研 + 锁边界决策 | — | **0.5d**(必须先做) |
| **1+2** | state + dispatch + handle_mux + 9 个 handler + projection | Phase 0 | 2-3d |
| **3** | ToolEventView 接入 | Phase 1+2 | 2-3d |
| **4** | ring buffer + dump 命令 | Phase 3 | 1d |
| **5**(可选) | schema 自描述 | Phase 4 稳定 | 3-5d |
| **总计(不含 5)** | | | **6-7d** |

vs 之前的"全栈重构方案"(改 5+ 个包、加 4 个 AgentEvent kind):**节省 ~3 天 + 跨包协调成本**。

---

## 11. 已对齐 memory 原则

本方案严格遵循 [[agent-no-config-tampering]]:
- ❌ 不 bundled dsh cordis.yml
- ❌ 不读 `~/.dsh/settings.yaml`
- ❌ 不改 dsh 本机默认配置

本方案严格遵循 [[no-type-aliases]]:
- `wireState` / `dispatcher` / `eventHandler` 都用具体 struct + interface,不用 `type X = Y`

本方案严格遵循 [[move-logic-atomically]]:
- P1+2 合并 PR 里:加 `wireState` + 改 `translate.go` + 改 `session.go` 调用点 + 删除旧 switch 四件事必须**一次完成**,不留双份实现

---

## 12. 决策日志

- **2026-08-15**: 用户报告"dsh 桥看不到 Tool 列表"。调查后发现根因是 `translate.go` 封闭 switch 丢 DSH 多数据源数据
- **2026-08-15 v1 方案**: 全栈重构 + 加 4 个 AgentEvent kind
- **2026-08-15 v2 方案**: 改回 dsh 包内自洽,不污染平台抽象。用户认可方向
- **2026-08-15 v2 评审**: 发现 B1/B2/B3 三个 Blocker,加 Phase 0 (wire-probe) 作为前置
- **2026-08-15 v2 定稿**: 本文档落地,Phase 0 开工