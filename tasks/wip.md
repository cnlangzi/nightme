# T-alive: Agent 探活 / Prompt 健康监控 —— 设计方案

> **Status**: design draft（讨论中，未拍板，未实现）
> **Date**: 2026-08-07
> **依赖**：[`docs/feat/message_lifecycle.md`](../docs/feat/message_lifecycle.md)（`Message`/`Prompt`
>   核心对象设计——**先读那份文档**，本文档假设 `Message.Stage`（`Queued`/`Submitted`/`Dropped`）和
>   `Prompt`（`Running`/`Done` + `EndReason`）已经存在，只讨论建立在它们之上的健康监控能力）
> **Scope**: `internal/agent`（`Pinger` 能力、`PromptEndReason` 的检测触发）+ `internal/bridge/pi`
>   （`Ping` 实现）+ `chatsession.AgentSession`（探活 goroutine 接线）+ `cmd/nightme/run.go`（收口
>   逻辑接线）
> **不含**：`Message`/`Prompt` 对象本身怎么改代码，见 [`tasks/wip-message-prompt.md`](./wip-message-prompt.md)
> **Out of scope（本方案不覆盖）**：wire 协议（`OutMessageState` 等）对外契约是否要跟着变

---

## 0. 背景与结论摘要

用户最初提出的三个问题：

1. 怎么判断一个 message 是否正式提交到 agent 里？——已由 `message_lifecycle.md` 的 `Message.Stage`
   回答（`Submitted` = 提交事务成功）。
2. 怎么判断 agent 是否执行完成？——`message_lifecycle.md` 的 `Prompt` 给出了"结束"这个事实
   （`Done` + `EndReason`），但**判定"结束"这件事本身**（尤其是进程中途崩溃、长时间无进展这两种
   情况）目前代码里是有缺口的，本文档负责补上。
3. 怎么判断 agent 是否还活着、继续执行？——本文档的核心内容：agent 存活探测（L1）+ Prompt 卡死
   检测（L2）。

本文档在 `Message`/`Prompt` 对象已经存在的前提下，建立 L1-L3 三层健康模型（agent 探活 / Prompt 探活
/ Prompt 结束的统一收口），并规划 `nightme health` 的可观测性扩展。

---

## 1. 现状缺口（代码证据）

### 缺口 A（最严重）—— agent 进程中途崩溃导致 ChatSession 永久死锁

```186:196:internal/chatsession/readpump.go
case ev, ok := <-evCh:
    if !ok {
        // Channel closed: process exited.
        as.SetExited(0)
        return
    }
```

`events` channel 意外关闭（进程被杀/崩溃，既没有 `EventDone` 也没有 `EventError`）时：

- 不发任何终态（对应 `Prompt` 永远停在 `Running`，用户的 🔄 永远不会变成 ✅/❌）
- 不 `SetIdle()`（`InputBuffer` FSM 永久卡在 `StateBusy`）
- 不触发 flush（之后新消息全部被静默 append 进队列，因为再也不会有 `EventDone` 来触发 flush ——
  **真死锁**）
- 不触发 `AgentExitObserver`（`SetAgentExitObserver`/`StartObserveClose` API 已存在但从未被 runtime
  注册）

项目自己在 backlog 里已经承认这一点：

```158:158:docs/FEATURES.md
| **Exit observer wiring** | `ChatSession.SetAgentExitObserver` + `StartObserveClose` exist; the runtime does not register an observer. readPump's natural exit is currently sufficient. | Reserved API for future work (respawn on death, /kill auto-reply, log user-visible "agent died" message). | `docs/feat/F-27-chatsession.md` §5.1.5 |
```

### 缺口 B —— 完全没有主动探活

- `pi`（长连接 RPC）的 `promptTimeout=90s`（`internal/bridge/pi/session.go:55`）只在"正在发一条消息"时
  才会暴露卡死；空闲期间进程被冻结（PID 还在但管道已堵）完全不可见。
- `agent.AgentSession` 接口（`internal/agent/agent.go:675`）没有 `Ping`/`HealthCheck` 方法——探活完全
  依赖"被动等 channel 关闭"。
- 对照组：`internal/channel/feishu` 已经为 WS 连接做了一套成熟方案（F-40/F-41：`health.go` 的
  `WSHealthSnapshot` + `reconnect.go` 的 30s ticker 主动探测 + `nightme health` 展示），但这套思路
  从未下沉到 agent 进程层。

> 之前草案里的"缺口 1"（`MessageForwarded` 触发过早）和"缺口 4"（`Prompt` 从未实体化）已经由
> `message_lifecycle.md` 的对象设计解决，具体迁移见 `tasks/wip-message-prompt.md`。

---

## 2. 分层健康模型（L1-L3，全部挂在 `Prompt` 对象上）

### L1 — Agent 探活（进程/传输层存活）

给 `agent.AgentSession` 加一个**可选能力接口**（不破坏 PTY/ACP 现有实现）：

```go
// internal/agent/agent.go
type Pinger interface {
    // Ping returns nil when the underlying process/transport is
    // responsive. MUST be safe to call while a turn may be in
    // flight (skip/no-op rather than contend with turn lock) and
    // MUST respect ctx's deadline.
    Ping(ctx context.Context) error
}
```

- **pi**：`Ping` = 短超时（3-5s）的 `get_state` RPC；若有 turn 在飞则直接返回 `nil`。
- **claudecode / PTY**：一次性进程，`Ping` = `cmd.Process.Signal(syscall.Signal(0))`，成本几乎为零。
- **acp**：如果协议有等价轻量 RPC 用它，否则退化为进程存活检查。

**探测触发**：仿照 F-41 的成熟模式（`internal/channel/feishu/reconnect.go` 的"30s ticker，
fire-and-forget"），在 `chatsession.AgentSession` 层挂一个 prober：仅在 `Status()==StatusRunning`
且没有 `Prompt` 在飞时才 tick；连续 N 次（默认 2）失败 → 主动 `Close()` + `SetExited(...)` + 触发
`AgentExitObserver`，直接走 L3 的 `PromptEndProcessDied`（如果当时有 Prompt 在飞）。

### L1.5 — AgentSession 状态队列（`/use` 切走后的离线 Prompt 追踪）【待实现功能，非本方案阻塞项】

**动机**：`promoteActiveLocked`/`/use` 只 `Background()`（cancel `opCtx`）旧 AS，**不杀进程**，旧 AS
可能仍在跑一个 Prompt；同时 `handleUse` 会 `StopReadPump()` 停掉旧 AS 的 pump——没有人再消费
`as.Events()`。今天这类"被弃置的 Prompt"完全不可观测（`docs/flow/three-layer-sync.md` §8 第 1/3
条已经记录为已知 UX 缺陷：receipt 永远停在 `Working...`、InputBuffer FSM 留在 Busy 不复位）。
`Prompt` 实体化之后，这个缺口会更显眼——`PromptEndReason` 五个值都覆盖不到"因为 `/use` 而失联"的
情况，`/health` 里的 `CurrentPrompt` 会一直显示一个假的"卡住"状态。

**核心思路（已达成一致）**：不要求无损——**假设旧 AS 上的 Prompt 还在正常跑**，当用户再次 `/use`
切回这个 AgentSession 时，由 AgentSession 自己保有的"状态队列"把期间发生的事情补投给我们，而不是
在切走的瞬间就武断地判它失败。

**前置依赖（必须先做，否则补投时对不上账）**：

- `currentPrompt` 必须挂在 **`AgentSession`** 上，不能像今天的 `currentTurnUserMsgID` 一样是
  `ChatSession` 的共享标量——`SetActiveAgent` 今天会清空它，这个清空动作以后必须**只清 ChatSession
  侧的 anchor 指针，不动旧 AS 自己的 `currentPrompt`**，否则旧 AS 的 Prompt 结束时找不到自己的
  `MessageIDs`。这一点已经作为决定写进 `tasks/wip-message-prompt.md` §5 的开放问题 4（建议直接在
  Phase 0 就把 `currentPrompt` 挂在 `AgentSession` 上，避免以后返工）。
- 每个 `AgentSession` 可以同时最多持有一个 `currentPrompt`（同一 AS 内 pi 是单 turn 串行；claudecode/PTY
  一次性进程同理），但 ChatSession 的 pool 里可以有多个 AS **各自**带着自己的 `currentPrompt` 并行跑
  （用户 `/use` 到别的 agent 不代表旧 agent 停了）。

**设计草图（两个子问题都要在实现前定，先记录不拍板）**：

1. **谁在旧 AS 被弃置期间持续消费 `as.Events()`？** 候选：给非 active 的 AS 也挂一个轻量后台
   goroutine（复用/扩展现有从未被 runtime 注册的 `StartObserveClose`），只更新
   `currentPrompt.LastProgressAt` / `EndReason`，**不**调用 `EventHandler`（不往 Channel
   发东西——旧 AS 不是当前会话的主角，此时给用户推送会很奇怪）。这样才能避免事件卡在有界 channel
   （F-29 的 cap 64）里被 `deliverEvent` 的"1s 内无人读则丢弃"逻辑吃掉，尤其是**终态事件本身被丢**
   这种最坏情况。
2. **补投的粒度：只给终态摘要，还是完整重放事件流？**
   - 方案 A（推荐起步）：**状态摘要**。后台 goroutine 只保留 `currentPrompt` 的结构化字段
     （`EndReason`/`EndedAt`/事件计数），不缓存每条 `AgentEvent` 的内容。用户切回来时直接用已经落定
     的终态一次性收口，Channel 侧渲染成类似"⏱ 你切走的 8 分钟里，这个任务已经完成/失败"的摘要，而
     不是逐条重放中间过程。
   - 方案 B（更完整但更贵）：**有界事件重放队列**。给非 active AS 一个独立于 `as.events`
     （cap 64，随时可能溢出）的、专门为"离线期"准备的、容量更大的重放缓冲，切回来时逐条推给
     Channel，体验上等价于"没切走过"。代价是要处理"缓冲区也满了怎么办"的降级路径，本质上只是把
     丢失窗口从 64 条撑大，没有从根上解决无损问题。
   - **先按方案 A 定义为可实现的最小闭环**；方案 B 留作方案 A 验证后如果产品觉得体验不够再升级。
3. **迟到的终态要不要还去 PATCH 原来的 receipt 卡片？** 如果用户切走了很久（几分钟到几小时），原有
   Feishu 卡片/线程是否还能/该不该被更新，这是产品侧的 UX 判断，不是技术问题——先列为开放问题。

**Non-goal（明确不做的事）**：不追求"`/use` 切走期间 100% 不丢事件"的强一致性；不要求旧 AS 在
非活跃期间的中间过程（工具调用、思考过程）也完整送达 Channel——只保证**终态**（成功/失败/进程死亡）
最终会被观察到，堵住"永远卡 🔄"这一类真死锁，而不是做完整的离线消息补推送体验。

### L2 — Prompt 探活（单轮执行的进度 / 卡死检测）

1. `Prompt.LastProgressAt`：`runReadPump` **每收到任意事件**（不限终态）就 touch 一次。
2. stall watchdog（tick 间隔 10-15s，阈值可配置，如 `PromptStallTimeout`=3-5 分钟）：超过阈值先借用
   L1 的 `Ping`：
   - Ping 成功 → 只是任务耗时长 → 发一个**非终态**信号（新枚举 `agent.PromptStalled`，供 Channel
     选择性渲染"⚠️ 仍在运行，已 N 分钟无新进展"，不打断执行）。
   - Ping 失败/超时 → 判定真死 → 强杀 → 走 L3 的 `PromptEndStalledKilled`。
3. `pi`（长连接、慢工具常见）和 `claudecode`（每轮独立进程）应该用不同默认阈值——见"开放问题"。

### L3 — Prompt 结束（统一终态契约）

所有终止路径统一收口到 `endPrompt(reason)`，避免像缺口 A 那样漏掉收口逻辑：

```go
func (cs *ChatSession) endPrompt(p *Prompt, reason PromptEndReason) {
    p.EndedAt = time.Now()
    p.EndReason = reason
    cs.SetIdle()
    _ = cs.flushPending() // 已排队的消息借此机会 flush 成新 Prompt，避免死锁
    if reason != PromptEndClean {
        cs.triggerExitObserver(cs.activeAS) // 接上 F-27 §5.1.5 预留但从未接线的 hook
    }
}
```

> 注：这段伪代码相对 `message_lifecycle.md` 定稿前的草案版本删掉了"对 `Prompt.MessageIDs` 做终态
> fan-out"那一步——按最终设计，`Message` 在 `Submitted` 时已经是终态，不需要等 `Prompt` 结束后再
> 广播给它合并进来的消息。

`runReadPump` 的三条路径（`EventDone`/`EventError`/`!ok`）以及 L1/L2 主动强杀路径，全部改成调用
`endPrompt(...)`，不再各写一遍 `SetIdle`+flush。这是修复缺口 A 的最小充分改动。

同时把 `SetAgentExitObserver` 真正接线到 `cmd/nightme/run.go`（目前从未注册）：默认实现 = 结构化
日志 + 可选给用户发一条 `OutReply`（"⚠️ Agent 进程意外退出，下一条消息会自动重启"）。

### L1-L3 流程图（纯文本）

```text
Prompt 诞生（Message.Submitted，见 message_lifecycle.md）→ Running
       │
【L2】stall watchdog 持续比对 LastProgressAt（借用 L1 的 Ping 二次确认）
       │
┌──────┴───────┬────────────────┬──────────────────┐
EventDone   EventError    !ok(进程崩溃)      stall + Ping 失败
   │            │               │                    │
endPrompt   endPrompt     【L3 修复】            【L2 新增】
(Clean)     (Error)   endPrompt(ProcessDied)  endPrompt(StalledKilled)
   │            │      （之前完全没有收口，死锁）        │
   └────────┬───┴────────────────┴────────────────────┘
            └─ SetIdle + flushPending + triggerExitObserver（非 Clean 时）
```

---

## 3. 可观测性：`nightme health` 扩展

复用已验证的 `WSHealthSnapshot`/`ProberSnapshot` 模式（`internal/channel/feishu/health.go`），在
agent 层加一份对称快照，喂给 `daemoncontrol` 的 `health` RPC：

```go
type AgentHealthSnapshot struct {
    Agent                   string
    Cwd                     string
    PID                     int
    Status                  string // running/detached/exited
    LastPingAt              time.Time
    LastPingOK              bool
    ConsecutivePingFailures int
    CurrentPrompt           *PromptHealthSnapshot // nil = idle
}

type PromptHealthSnapshot struct {
    PromptID       string
    MessageIDs     []string
    AckedAt        time.Time
    LastProgressAt time.Time
    Stalled        bool
}
```

`nightme health` 增加 `AGENTS` section，和现有 `PROBER` section 并列，向后兼容（协议加字段，旧
client 忽略）。

---

## 4. 分阶段落地计划

| Phase | 内容 | 性质 | 风险 |
|---|---|---|---|
| **Phase 1** | `!ok` 分支统一收口(`endPrompt`)堵死锁 + `SetAgentExitObserver` 接线 | Bug fix | 低——不改变对外 wire 契约 |
| **Phase 2** | `agent.Pinger` 接口 + pi/claudecode 实现 + AgentSession prober goroutine + 配置项 | 新能力 | 中——需要定阈值，要过 pi 的 turn 锁边界测试 |
| **Phase 3** | `Prompt.LastProgressAt` + stall watchdog + `PromptStalled` 广播 | 新能力，UX 向 | 中——涉及新 wire 事件，要过 Channel 渲染决策 |
| **Phase 4** | `nightme health` 扩展 + `docs/SPEC.md` 文档化 + 回归测试（崩溃死锁、ping 失败触发respawn、stall 强杀） | 收尾 | 低 |
| **Phase 5**（待实现功能，非阻塞） | L1.5 `AgentSession` 状态队列：`/use` 切走后离线 Prompt 的后台状态保鲜 + 切回时补投终态（方案 A，状态摘要粒度） | 新能力 | 中——依赖 `currentPrompt` 归属到 `AgentSession`（见 `tasks/wip-message-prompt.md`）；修 `three-layer-sync.md` §8 第 1/3 条已知 UX 缺陷 |

前置条件：`tasks/wip-message-prompt.md` 的 Phase 0（`Message`/`Prompt` 对象重构）是本文档所有 Phase
的地基，必须先落地。Phase 1 是纯 bug fix，建议紧随其后无条件做。Phase 2/3 涉及具体阈值和默认行为的
产品判断，需要产品侧确认后再排期。Phase 5 不阻塞 Phase 1-4。

---

## 5. 开放问题 / 待确认项

1. **L2 stall 阈值**：`pi`（长连接、慢工具常见）和 `claudecode`（每轮独立进程）要不要用不同默认
   阈值？初步倾向是——需要产品侧给出可接受的默认值再定。
2. **`/use` 切走后旧 AS 的 Prompt 怎么收口？** 已达成初步共识（见 §2 L1.5）：**不**在切走瞬间判定为
   失败/异常，而是假设它还在正常跑，靠 `AgentSession` 自己的状态队列在切回来时补投终态。由此推出
   `PromptEndReason` 暂不需要为这种情况新增一个"因为 /use 而失联"的值——只要 L1.5 的后台状态保鲜
   落地，这类 Prompt 最终还是会走到 `Clean`/`Error`/`ProcessDied` 之一，只是收口时间点延后到用户切
   回来（或后台 goroutine 独立观察到）为止。**待确认**：L1.5 落地之前的这段时间里，`endPrompt`/
   `PromptEndReason` 的实现要不要先预留一个"未决"状态（而不是假装它不存在）。
3. **`Ping` 是否应该在 Prompt 飞行中仍然探测？** L1 的 `Pinger` 草案里"若有 turn 在飞则直接返回
   `nil`"是保守简化——`pi/rpc.go` 的 `pending` 是按请求 id 关联的 map，`turnMu` 只 guard
   `SendBlocks` 自身的并发调用，架构上 `get_state` 理论上可以和一个在飞的 `prompt` RPC 并发，不
   一定要整体放弃 L1 探测退化成只靠 L2 stall watchdog。需要在实现阶段验证 pi CLI 自身是否真的能在
   处理 prompt 期间及时响应并发的 `get_state`，再决定要不要收紧这个简化。
4. **`Prompt` 健康快照要不要持久化？** 见 `tasks/wip-message-prompt.md` §5 开放问题 5——这里的
   `nightme health` 展示（§3）依赖那个决定。如果不持久化，daemon 重启后看不到"重启前最后一个 Prompt
   卡在哪一步"。

---

## 6. 参考

- [`docs/feat/message_lifecycle.md`](../docs/feat/message_lifecycle.md) — `Message`/`Prompt` 对象设计
  （本文档的前置依赖）
- [`tasks/wip-message-prompt.md`](./wip-message-prompt.md) — `Message`/`Prompt` 的开发任务拆解
- `docs/SPEC.md` §2.5 Message Lifecycle Tracking
- `docs/feat/F-29-agent-session-pool.md` §4.5 Death detection
- `docs/feat/F-31-message-state.md`
- `docs/feat/F-32-pi-rpc-bridge.md`
- `docs/feat/F-41-active-reconnect.md`（探活模式的成熟参照）
- `docs/FEATURES.md` §7 backlog "Exit observer wiring"
- `internal/channel/feishu/health.go` + `internal/channel/feishu/reconnect.go`（WSHealth/Prober 模式）
