# F-41: Active Reconnect (30s Forced Stop+Start, No HTTP Probe)

> **Status**: implemented (v1.3.x, 2026-08-05)
> **Scope**: `internal/channel/feishu/{reconnect.go,adapter.go,health.go}` + `cmd/nightme/health.go`
> **目的**: 把 Feishu WebSocket 从"断开到重连"的最大等待从 SDK 默认 2 分钟压到 30 秒 ── 用户发消息的"无响应窗口"从 2min 缩到 30s
> **Related docs**:
> - [docs/feat/F-40-ws-reconnect-observability.md](./F-40-ws-reconnect-observability.md) — health struct + callbacks; F-41 builds on the OnDisconnected / OnReconnected / OnReady hooks
> - [docs/feat/F-37-tool-thread-routing.md](./F-37-tool-thread-routing.md) — pattern of "what Feishu WS state nightme has visibility into"
> - [docs/SPEC.md §1.3](../SPEC.md) — adapter/state-machine invariants

---

## 0. 背景

### 0.1 SDK 默认行为(已读源码确认)

`larksuite/oapi-sdk-go/v3@v3.9.9/ws/client.go:186-190`:

```go
autoReconnect:     true,
reconnectNonce:    30,            // 首次重连 jitter 上限 30s
reconnectCount:    -1,            // 无限重试
reconnectInterval:  2 * time.Minute,
pingInterval:      2 * time.Minute,
```

reconnect 触发链(`client.go:519-528`):

```
receiveMessageLoop reads → 失败(err != nil) → defer → disconnect(ctx) + reconnect(ctx)
reconnect 循环:
  首次 jitter 0-30s
  然后每 reconnectInterval(2min) 一次
  reconnectCount = -1(无限)
```

### 0.2 实际用户体感

实测断开 30s-2min 之间,daemon 看起来**完全没反应**:
- ❌ 没有 inbound event
- ❌ 没有 log(因为 SDK 静默等待 2min)
- ❌ 用户发消息没回应

F-40 加了 health 命令(可以看到断线),但**没做主动恢复** ── 用户依然要等 SDK 自己的 2min timer。

### 0.3 候选方案(已比较过)

| 方案 | 描述 | 评价 |
|---|---|---|
| (a) 砍掉 SDK 默认 reconnectInterval,调到 30s | `WithReconnectInterval(30s)` | SDK 是构造期参数,运行时改不了 ── 不行 |
| (b) 三层(tier 0/1/2) | 简单阈值(30s probe / 5min watchdog) | 复杂,过设计 ── 砍 |
| (c) **30s ticker 无脑 Stop+Start** | 本方案:无 HTTP probe,无 tier,无 circuit breaker | **最简单** ── 选这个 |

**选 (c)** 的理由:
- 不探活 ── Stop+Start 失败会自然 reconnect(SDK 内部已经做了),我们不用关心网络状态
- 不分层 ── 断开期间唯一行为是"30s 一次强制重启 SDK 试试",直到成功
- 不退出 ── 没有"试 N 次后放弃"这种逻辑(网络断几天也无所谓,反正 daemon 进程在)

---

## 1. 设计

### 1.1 核心思路

不在 SDK 内部插手,而是**外层周期性地让 SDK 重新走 connect 流程**:

```
SDK 已建连,服务正常
   ↓ 网络断
OnDisconnected 触发 ── SDK 自己的 2min timer 启动(我们不管它)
   ↓
我们启动 prober goroutine ── 30s ticker
   ↓
每 30s:
  ch.Stop()   杀 SDK 内部 reconnect goroutine + 关闭 socket
  sleep 100ms
  ch.Start()  重新拉 SDK 走 fresh connect
   ↓
   ┌ 成功 → OnReconnected/OnReady → prober.Stop() 退出
   └ 失败 → SDK 内部重连循环又起(我们的 Stop 触发的关闭让 SDK 走 fresh reconnect)
              OnDisconnected 又触发 → prober 继续跑
```

**关键洞察**:我们跟 SDK 自己的 2min timer **并行运行**。每次我们 Stop+Start,SDK 内部 2min timer 就被取消,新的 timer 重新计时。这样我们实际强制 SDK "每 30s 试一次"。

### 1.2 为什么不需要 HTTP probe

直觉:先 HEAD 探活,通了再 Stop+Start。
- 优点:网络完全断的时候不浪费 SDK 重连尝试
- 缺点:多了一层网络调用,加了失败模式(我们 HEAD 的 endpoint 挂了 vs SDK reconnect 失败)

实测:**SDK 重连很快**(几十 ms 完成)。一次失败的 reconnect 代价小到可以忽略。30s 一次的频率下,即使 100% 失败也只占总时间的 0.X%。

**简化掉 HTTP probe** ── 让 SDK 内部去试,我们只负责"每 30s 杀一次重来"。

### 1.3 prober 状态机

prober 只有两个状态:

```
(inactive)  ──── Start() on OnDisconnected ────►  (active)
                                                  │
                                                  │ ticker fires:
                                                  │   Stop + 100ms + Start
                                                  │   check Connected:
                                                  │     yes → Stop() ──► (inactive)
                                                  │     no  → continue ticker
                                                  ▼
                                              永远 ticker,直到 Connected
```

没有失败状态、没有退出条件(除了 Connected 恢复)。这是**故意**的 ── 即使 SDK reconnect 一直失败,prober 继续跑,反正成本是 30s 一次的 SDK 重建尝试。

### 1.4 与 SDK 的交互

- `Stop()` 关闭 socket + 取消 SDK 内部 goroutine(`larkws/client.go:344-352`)
- `Start(ctx)` 重新拉 SDK connect 循环(`larkws/client.go:206-233`)
- SDK 的 autoReconnect=true 仍然有效 ── 我们 Stop 之后如果网络短暂恢复,Start 期间 SDK 也能用 default 2min 兜底
- 我们的 prober 跟 SDK reconnect timer **并行** ── 谁先连上谁赢

### 1.5 prober 终止

- `Connected=true` 时 (OnReconnected / OnReady 触发) ── prober.Stop()
- `daemon shutdown` 时 ── prober goroutine 跟 channel 一起退出
- 没有任何**主动放弃**逻辑

---

## 2. 文件 & 接口

### 2.1 新文件

**`internal/channel/feishu/reconnect.go`** ── prober 实现:

```go
type prober struct {
    adapter        *Adapter
    interval       time.Duration         // 默认 30s
    restarter      func() error           // 注入 ch.Stop() + ch.Start() 闭包
    stopCh         chan struct{}
    doneCh         chan struct{}
    startedAt      atomic.Pointer[time.Time]
    forceCount     atomic.Int64
    lastForceAt    atomic.Pointer[time.Time]
}

func newProber(adapter *Adapter, restarter func() error) *prober
func (p *prober) Start()  // 启动 goroutine
func (p *prober) Stop()   // 停止 goroutine (阻塞直到退出)
func (p *prober) Snapshot() ProberSnapshot

type ProberSnapshot struct {
    Active        bool          // 是否在跑
    Interval      time.Duration // 间隔
    ForceCount    int64         // 累计强制重启次数
    LastForceAt   time.Time     // 最近一次
    StartedAt     time.Time     // 当前 cycle 开始
}
```

### 2.2 改动文件

**`internal/channel/feishu/adapter.go`** ── wire 3 个 SDK callback 到 prober:

```go
// OnDisconnected: 启动 prober
// OnReconnected:   停止 prober
// OnReady:         停止 prober(首次连接也算恢复)
```

prober 字段加到 `Adapter` 结构体,跟 `health` 字段同级。

**`internal/channel/feishu/health.go`** ── WSHealthSnapshot 加 1 个字段:

```go
Prober ProberSnapshot `json:"prober"`
```

**`cmd/nightme/health.go`** ── 新 section `PROBER` 在 STATUS 和 LIVENESS 之间:

```
PROBER
  active:          yes
  interval:        30s
  force_attempts:  12         (累计)
  last_force_at:   5s ago
  started_at:      2m15s ago
```

### 2.3 保留不变

- `internal/daemoncontrol/{server,client,protocol}.go` ── 不改(health RPC 已经能传任意 JSON)
- `internal/channel/feishu/receipt.go` / `receipt_event.go` ── 不改
- `cmd/nightme/{run,daemon_lifecycle,root}.go` ── 不改
- SDK 的 `WithAutoReconnect` 等参数 ── 不改(SDK 默认行为保持,作为兜底)

---

## 3. 测试覆盖

### 3.1 单元测试(`reconnect_test.go`)

| 用例 | 断言 |
|---|---|
| `TestProber_StartStopHappy` | Start 后 Stop 在 < 100ms 内返回,`doneCh` 关闭 |
| `TestProber_TickerFires` | mock 30ms interval,2 次 ticker 内 restarter 至少被调 1 次 |
| `TestProber_StopOnConnect` | 调用 `recordForceResult(success=true)` → prober 自动 Stop |
| `TestProber_RetryOnFailure` | restarter 失败 → 继续 ticker,`forceCount` 累加 |
| `TestProber_ResetOnConnect` | 多次断开/重连 → prober 正确 start/stop 多次 |
| `TestProber_Snapshot` | Snapshot 字段反映最新状态 |

### 3.2 集成测试(`adapter_test.go`)

| 用例 | 断言 |
|---|---|
| `TestAdapter_OnDisconnected_StartsProber` | mock SDK 触发 OnDisconnected → prober Active=true,30s 后 mock restarter 被调 |
| `TestAdapter_OnReconnected_StopsProber` | OnReconnected → prober Active=false,ForceCount 累加 |

### 3.3 端到端

- `nightme health` 在 disconnect 期间显示 `active: yes`,connected: no
- 网络恢复后 → prober stop → `active: no`,connected: yes,force_count 计数

---

## 4. 不变式 (Invariants)

- **OutboundMessage 契约不变** ── prober 不影响 channel.Send()
- **daemoncontrol RPC 协议不变** ── health JSON 多了 `prober` 字段,旧 client 解析忽略未知字段
- **prober 不抢 SDK 的重连** ── 我们只是周期性 kill + respawn SDK,SDK 内部机制不变
- **prober 永不主动退出(除非 Connected 或 daemon shutdown)** ── 这是有意的;不引入"放弃重连"这种语义
- **circuit breaker / tier escalation / watchdog 不存在** ── 简化掉;有需要可以后加

---

## 5. 落地顺序 (commit 切分)

```
commit A: docs(feishu): F-41 active reconnect design
         docs/SPEC.md (§0.9 changelog)
         docs/channel/feishu.md (§13.18 decision record)
         CHANGELOG.md (F-41 entry)
         docs/feat/F-41-active-reconnect.md (NEW)

commit B: feat(feishu): F-41 30s forced reconnect prober
         internal/channel/feishu/reconnect.go (NEW)
         internal/channel/feishu/reconnect_test.go (NEW)
         internal/channel/feishu/adapter.go (wire SDK callbacks to prober)
         internal/channel/feishu/health.go (WSHealthSnapshot.Prober field)
         cmd/nightme/health.go (PROBER section)

2 commits, ~150 lines new code + ~150 lines tests.
```

---

## 6. 与 F-40 的关系

F-40 加了**观测**(health 命令 + struct log),用户能看到"WS 断了"。
F-41 加了**主动恢复**(prober 让 WS 30s 内尝试重连),用户能少等。

两者叠加 = 完整闭环:
1. F-40 让你**知道**断线了(看 health)
2. F-41 让断线**少发生** / 持续时间**短**(30s 一次强制重连)

---

## 7. 已知边界 (Out of Scope)

- **多 adapter 并发** ── 每个 adapter 一个 prober,prober 闭包 capture adapter 引用,无共享状态
- **持久化** ── prober 是 in-memory 状态,daemon 重启丢失(同 chat_session 生命周期)
- **配置 knob** ── 当前 interval 写死 30s;后续可加 `--ws-probe-interval=30s` CLI flag
- **metrics export** ── 不接 prometheus;future work

---

## 8. 不叫 v2.0 的理由 (与 F-37 / F-39 一致)

v1.3 不变式全部保留:
- OutboundMessage 契约不变
- Gateway 不动
- ChatSession 不动
- §1.3 不变式不动
- §1.4 边界规范不动

F-41 是 Channel 自治范围内的事(WS 连接管理 = 飞书实现细节),不影响 nightme 数据模型与 Gateway 契约。
