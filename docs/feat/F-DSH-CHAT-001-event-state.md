# F-DSH-CHAT-001 — dsh Chat-Session Event Translation 重构

> **Status**: 已落地
> **Scope**: `internal/bridge/dsh/` 内 `translate.go` / `protocol.go` / `session.go` / `state.go` / `dispatch.go` / `handle_mux.go` 的事件翻译层
> **不动范围**: `internal/agent/` (AgentEvent 抽象)、`internal/channel/` (飞书渲染)、`internal/runtime/`、`internal/chatsession/` —— **零改动**
> **姊妹文档**:
> - [bridge/dsh-api.md](../bridge/dsh-api.md) — dsh HTTP / WS 协议契约
> - [bridge/dsh.md](../bridge/dsh.md) — dsh wire / lifecycle 详细规范
> - [F-CLAUDE-PRINT-002-statusbar-refactor.md](./F-CLAUDE-PRINT-002-statusbar-refactor.md) — 跨包重构的失败案例,作为本方案的"避免踩坑"参考
>
> **2026-08-16 修订 (dashboard 对齐)**: dsh web 的 **To-dos / 任务** 条(`TodoDock`)对齐 `todo/write` `{todos:[{content,status}]}` + `session/projection` `key:"todos"`(`value` 是 `TodoItem[] | null` 数组,不是 `{todos:[...]}` 包装)。`step/start` / `step/end` 是 `sessionStats`(TTFT / tok/s),**不要**映射成 OutTask*。聊天里的 `todo_write(...)` 行是 `tool/call`,不是 dock 清单。权威表:[dsh-api.md §3.4.2–3.4.3](../bridge/dsh-api.md)。

---

## 0. 文档目的

dsh web 的 To-dos 面板上能看到任务清单(Read / Bash / 各步骤),但 nightme 这边在翻译 `session/event` 流时丢数据,导致 channel 侧看不到 Tool 列表 / To-dos。

本文档记录 dsh 包内的三层结构:

- **Layer 1 — Wire Ingest**(解码 + 永不丢)
- **Layer 2 — wireState**(dsh 内部真值镜像)
- **Layer 3 — Adapter**(AgentEvent 产出)

**核心原则**: 平台抽象(`agent.AgentEvent`)稳定,所有 dsh 桥特有的智能化收敛在 `internal/bridge/dsh/` 包内,不污染上层抽象。

---

## 1. dsh wire 的三路数据源

dsh web 同时下发三种并行数据源,共同描述 session 的"真值":

| 数据源 | 何时下发 | 内容 |
|---|---|---|
| `session/event` | 每条 SessionEvent 伴随下发 | 原始事件流(text delta / tool_call / tool_result / turn 边界 / `todo/write` 等 SessionEvent) |
| `session/projection` | host 在 `todo/write` / `turn/start` 等变化时下发 | 派生状态快照。To-dos 的 key 是 **`todos`**(复数);`value` 是 `TodoItem[] \| null` |
| `muxSessionEvent.View` | 每条 `session/event` 伴随下发 | host 已计算好的渲染视图(`ToolEventView`),含 tool 状态 + task 列表 + uuid |

详见 [bridge/dsh.md §1](../bridge/dsh.md) 的 wire 规范。

dsh wire 模型天生是**三条数据流并行**描述真值。任何"wire → AgentEvent 一对一开关"翻译模式都会丢数据:

- raw event 是"动作流"(tool_call ↔ tool_result 配对必须靠 raw)
- View 是"渲染态"(host 已合并 / 推断,比 raw 更准)
- Projection 是"聚合快照"(title / tasks 等聚合维度)

三层各管一段,无法用单一 switch 表达 —— 所以翻译层承认 dsh 是多数据源结构。

---

## 2. 组件 1:`wireState`(`internal/bridge/dsh/state.go`)

### 2.1 结构

```go
type wireState struct {
    mu       sync.Mutex
    title    string
    tasks    map[string]todoItem // by ID;ID 为空时退化为 Content
    tools    map[string]toolItem // by CallID
    inflight map[string]bool     // tool_callIDs seen without matching result

    wireRing    *wireRingBuffer // 最近 64 帧 raw frame(可观测性)
    unknownCount uint64         // 未识别 envelope / mux method / todo drift 的累计计数
}
```

`wireState` 是 dsh 包内私有的 —— 它不进 `agent` / `channel` / `chatsession` 任何一层。

### 2.2 三个入口

```go
// 单事件流入口(raw event + 伴随 View)
func (s *wireState) applyEvent(env sessionEventEnvelope, view json.RawMessage) []agent.AgentEvent

// Projection 单独入口(host 周期性下发)
func (s *wireState) applyProjection(proj projectionEnvelope) []agent.AgentEvent

// View 单独入口(host 预计算的渲染视图)
func (s *wireState) applyView(viewBytes json.RawMessage) []agent.AgentEvent
```

每个入口都有一个 `_Locked` 变体(`applyEventLocked` / `applyProjectionLocked` / `applyViewLocked`),供 dispatcher 在已经持锁时调用。返回的是 `[]agent.AgentEvent` 序列 —— 已经是平台层概念,由 dispatcher 投出。

`applyView` 只更新 state,不产事件:事件由 raw envelope 的 handler 产出,View 负责把 host 算好的 tool 状态 / task 快照合并进 `wireState`。

### 2.3 task 的 key

`todoItem.ID` 为空时用 `Content` 当 key;`Content` 也为空(字段名漂移)时该条被丢弃、计数 +1,并记一条 Warn 日志 —— 静默丢弃是生产上最难查的失败模式。

### 2.4 可观测性

```go
func (s *wireState) DumpWireStats() (unknownCount uint64, frames []wireFrame)
```

`unknownCount` 有三个来源:

1. dispatcher 的注册表查找未命中(事件类型没注册)
2. `handleMuxFrame` 遇到未知 mux method
3. todo drift —— `todo/write` 或 `session/projection(todo)` 帧里的 item 双字段(`ID` 与 `Content`)都为空

`wireRing` 是定容 64 帧的环形缓冲,记录最近的 raw mux frame(method + envelope type + 字节数),供"bridge 没产出预期事件"时对照 wire 实际到达了什么。

---

## 3. 组件 2:`eventDispatcher`(`internal/bridge/dsh/dispatch.go`)

### 3.1 handler 签名

```go
type eventHandler func(env sessionEventEnvelope, view json.RawMessage, tr *translator, st *wireState, d *driver) []agent.AgentEvent
```

handler 是顶层函数(不闭包捕获 per-driver 状态)—— per-driver 的可变状态通过 `tr` / `st` / `d` 三个参数传进来。

### 3.2 注册表

```go
var standardRegistry = newRegistry(map[string]eventHandler{
    "assistant/chunk":   handleAssistantChunk,
    "assistant/message": handleAssistantMessage,
    "tool/call":         handleToolCall,
    "tool/result":       handleToolResult,
    "turn/start":        handleTurnStart,
    "turn/end":          handleTurnEnd,
    "compaction/end":    handleCompactionEnd,
    "todo/write":        handleTodoWrite,
    "todo/update":       handleTodoUpdate,   // 提前注册,handler 现在返回 nil
    "todo/delete":       handleTodoDelete,   // 同上(graceful)
    "approval/asked":    handleApprovalAsked,
    "approval/decided":  handleApprovalDecided,
    "permission/preset": handleDebugOnly,
    "sandbox/mode":      handleDebugOnly,
    "approval/policy":   handleDebugOnly,
    "agent/inbox/spliced": handleDebugOnly,
    "user/message":      handleUserMessageEcho,
    "request/header":    handleDebugOnly,
    "system/message":    handleDebugOnly,
    "request/context":   handleDebugOnly,
    "step/start":        handleStepBoundary,
    "step/end":          handleStepBoundary,
    "session/title":     handleSessionTitle,
    "session/title-llm-request": handleDebugOnly,
})
```

**为什么是注册表不是 switch**:

- 加新事件 = 加一行注册 + 写 handler,不动 default 分支
- 提前注册尚未实现的 type(如 `todo/update` / `todo/delete`),让 dsh web 加这种事件时 bridge 自动开始处理(即使 handler 返回 nil 也比静默 dLog 强)
- dsh 新增的 debug-only 事件类型也注册进去(`handleDebugOnly`):升级 dsh 不会把 unknown 计数打满

### 3.3 dispatch

```go
func (d *eventDispatcher) dispatch(env sessionEventEnvelope, view json.RawMessage)
```

流程:

1. 取 `translator.mu` → 取 `wireState.mu`(固定顺序:translator 在前)
2. 有 view 字节 → `wireState.applyViewLocked(view)`,把 host 预计算的状态先合并进去,再跑 handler
3. 查注册表:命中 → 跑 handler;未命中 → `unknownCount++`
4. 放锁(translator / wireState)
5. 放锁之后才 `deliver(event)`

**锁纪律**:handler 在两把锁都持有的情况下运行,**不许**再加锁(否则死锁);handler **不许**调 `deliver`(由 dispatcher 在放锁后统一投出)。

`deliver` 放在放锁之后是 C1 修复 —— 当下的 deliver 是非阻塞 select-default,这是正确性 / 清晰度的改进;将来若 deliver 变阻塞,锁窗口依然短。

---

## 4. 组件 3:mux 分发(`internal/bridge/dsh/handle_mux.go`)

```go
func (d *driver) handleMuxFrame(method, rpcID string, payload json.RawMessage)
```

顶层 method 分发:

| method | 去向 |
|---|---|
| `session/subscribed` | 记录 baseline `lastSeq`(重连时的 resume 标记) |
| `session/event` | 解出 envelope → `driver.dispatcher.dispatch(env, ev.View)` |
| `session/projection` | `driver.wireState.applyProjection(proj)` —— 单独通道,不产 AgentEvent |
| 其它 | debug log;计入 `unknownCount` |

`session/projection` 不再被 default 分支吞掉。

dsh 0.1.2-rc.1 把 per-event type 折进了 mux method 本身(method 直接是 `"assistant/chunk"`,payload 是 `event.data`)。`handleMuxFrame` 对这些 method 构造一个合成的 `sessionEventEnvelope` 再转发给 dispatcher;旧的 `session/event` 名字保留兼容读取,`session/subscribed` / `session/projection` 在新 wire 下名字不变。

`handleMuxFrame` 带 panic recover —— mux pump goroutine 里的 panic 会冒泡到 stream 的读循环并杀掉整个 mux 连接,每次重连都会再 panic 一次。

---

## 5. View 字段的权威性边界

不是简单的"projection > view > raw event",而是按**用途**分:

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

## 6. 配置

dsh bridge 没有专用配置段 —— `bridge.dsh.*` 这类 key 不存在。接入方式跟其它 bridge 一样,走 config yaml 的 `agents:` 列表:

```yaml
agents:
  - name: dsh
    bridge: dsh
    command: dsh
```

`command` 指向本机的 dsh 可执行文件(默认 `dsh`);共享的 `dsh --profile web` 守护进程由 `internal/bridge/dsh/host` 负责按需拉起或附着(canonical port 3080)。

dispatcher **不是** feature flag:注册表是 `session/event` 的唯一路由路径,没有"旧 switch / 新 dispatcher"两条路可选,也不需要开关切换。`session/projection` 走 `wireState.applyProjection` 单独通道,同样不是开关控制的。

nightme 不改 dsh 的本机配置(只注入 `cmd.Dir` 与 `DSH_PERMISSION_MODE=danger-full-access`),详见 [bridge/dsh.md](../bridge/dsh.md) §15。

---

## 7. 文件分布

```
internal/bridge/dsh/
├── state.go             wireState + applyEvent / applyProjection / applyView + wireRingBuffer
├── dispatch.go          eventDispatcher + 注册表 + 全部 handler
├── handle_mux.go        mux frame 顶层 method 分发
├── translate.go         translator(F-52 流式状态机)—— 不再有 env.Type 的 switch
├── protocol.go          sessionEventEnvelope / todoItem / ToolEventView / projectionEnvelope
├── session.go           driver 持有 translator / wireState / dispatcher
└── testdata/
    ├── envelopes/       todo_write.json
    └── projections/     todo_snapshot.json
```

View 的 fixture 不落盘 —— View 的字段结构由 `protocol.go` 的 `ToolEventView` struct 定义,测试直接构造 Go 值。

`translate.go` 里只剩 `translator`(F-52 流式缓冲:textBuf / reasoningBuf / pendingTools / pendingText / lastText / textDelivered / active)和几个纯转换 helper(`usageToAgent` / `stopReasonToSubtype` / `todoStatusToTaskStatus`)。`handleSessionEvent` 那个 200 行的 switch 已经不存在。

---

## 8. 跨桥:wireState 留在 dsh 包内

只有 dsh 有"raw event + projection + view"三路数据源的结构;claudecode / codex / pi / copilot / omp 都是单事件流。所以 wireState 没有抽到公共包 —— 全仓的 `wireState` / `applyProjection` 只在 `internal/bridge/dsh/` 下。

---

## 9. 测试锚点

| 文件 | 覆盖 |
|---|---|
| `internal/bridge/dsh/dispatch_test.go` | 注册表全类型路由、未知类型不 panic、未来类型预注册、`tool/call`↔`tool/result` 配对回填、未知类型计数与 Warn、View 附加不计数 |
| `internal/bridge/dsh/state_test.go` | `todo/write` 填 ID / ActiveForm、无 ID 时的 Content fallback、projection 更新 tasks、todo 字段漂移被计数、View 合并 tool / task 状态、ring buffer 时序与回绕 |
| `internal/bridge/dsh/view_test.go` | View 的 tool_call / task_list / 未知 kind |
| `internal/bridge/dsh/todo_realwire_test.go` | 真实 wire 形状的 `todo/write` 与 `session/projection`(todos / items 兼容) |
| `internal/bridge/dsh/todo_testdata_test.go` | 用 `testdata/` 里抓下来的 payload 跑 |
| `internal/bridge/dsh/translate_regression_test.go` | textBuf 的 grow-and-reuse 回归 |
| `internal/bridge/dsh/handle_mux_legacy_test.go` | 旧 wire method 的兼容读与丢弃 |

CI 的 `go test -race ./...` 覆盖本包 —— dispatcher 的锁纪律(两把锁的固定顺序 + 放锁后 deliver)靠 race detector 兜底。

---

## 10. 排错速查

| 症状 | 根因 | 修法 |
|---|---|---|
| To-dos 面板仍为空 | 直播缺 `todo/write`;或只靠投影但 `value` 是数组而 decoder 当 object 解;或 resume 没读 history `projections` | 先确认 `todo/write` 字段是 `todos` 不是 `items`;投影形状见 [dsh-api.md §3.4.3](../bridge/dsh-api.md);`step/*` **不是**清单 |
| Tool 状态更新滞后 | View 没接进 `wireState.tools` | 确认 dispatcher 的 view 分支在跑 |
| `unknown event type` 大量出现 | dsh 加了新 SessionEvent,注册表没追上 | `DumpWireStats` 看最近的 wire 帧,在 `standardRegistry` 加 handler |
| 锁死锁 | dispatcher handler 内部加锁 + 调用方已持锁 | handler 内不许再加锁;或走单源真值 |
| todo 全部为空 / 计数上涨 | wire 字段名与 `protocol.go` 的 `todoItem` json tag 不一致 | 对照 `internal/bridge/dsh/protocol.go` 的 tag 与 dsh wire |
| regression 测试通过但生产仍坏 | testdata fixture 与真实 wire 字段名不一致 | 重跑 wire-probe,更新 fixture |

---

## 11. 验收清单

- [x] `state.go` / `dispatch.go` / `handle_mux.go` 三个文件落地
- [x] `translate.go` 里的 `switch env.Type` 整段消失,事件类型全部走注册表
- [x] `protocol.go` 的 `todoItem` 含 `ID` / `ActiveForm` 字段
- [x] `protocol.go` 含 `projectionEnvelope` 与 `ToolEventView` struct
- [x] `session/projection` 帧能触发 `EventAgentTaskCreate`
- [x] unknown envelope / mux method / todo drift 三类都有计数与日志
- [x] wireRingBuffer + `DumpWireStats` 可用
