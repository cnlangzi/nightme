# Feishu Channel 可靠性 (限速 / 重试 / WS 重连)

## A1. F-35: Feishu Channel Rate Limit

> **Source**: `../channel/feishu-reliability.md`


---

## 0. 背景

nightme 的飞书 adapter (`internal/channel/feishu/adapter.go`) 在每个 agent turn 中会向飞书发起多次 API 调用：冷启动一张 card、event 触发多次 PATCH、AddReaction 状态 emoji 等。**hot path 完全同步**——readPump → `EventCallback` → `channel.Send` → SDK call，无队列、无 backpressure、无重试。

如果 daemon 同时跑多个 chat、或某个 receipt 的 PATCH storm 高频，飞书会触发 `230001` / `230020` 等限流错误码。这些错误**目前没有专属处理路径**——`sendContent` 只对 230011 / 231003（消息撤回/删除）做了 fallback，其他错误一律 log warn 返回 error，event 丢失。

**F-35 的解法**：在 feishu 包内加一个全局共享的 token bucket，所有出口 API 调用前都 `Wait()`。**预防**触发限流，而不是事后补救。

---

## 1. 设计原则

### 1.1 单桶覆盖所有 API

所有 5 类飞书出口 API（send / reply / patch / reaction / upload）的限速文档**完全相同**（1000 QPM / 50 QPS per app），且 nightme 热路径受 **per-user 5 QPS** 约束（向同一 user 发消息的硬限）——因此不需要 per-op-kind 分桶，**一个全局桶**足够且最简单。

### 1.2 不带 burst / 不留弹性

`maxTokens = 1`（最严格 token bucket）。连续两次调用至少间隔 200ms（5 QPS 决定）。**绝不让 nightme 突破飞书硬上限**——宁可慢一点，也不要触发 230001 后被飞书降级。

### 1.3 Lazy refill（不启后台 goroutine）

token refill 在 `Wait()` 调用时按 elapsed 时间计算，无 ticker、无 channel、无 goroutine 泄漏风险。`tokens` 字段 mutex 保护，并发安全。

### 1.4 与现有架构正交

- **不改 `Channel` 接口契约**（`internal/channel/channel.go::Send` 仍是 fire-and-ack）
- **不动 `sendContent` 包装层**（rootID fallback 路径仍生效）
- **不影响 PATCH 5 QPS / message_id**（由 `MessageReceipt.renderLocked` 单 mutex 天然满足）
- **不与 F-36 retry 耦合**（retry 在 sendContent 外层做，限速在最底层 SDK call 前做；二者职责严格分离）

### 1.5 PATCH 5 QPS per-message_id 为什么不在 limiter 里强制

理论上可以按 message_id 分桶，但**没必要**：

- `MessageReceipt.renderLocked` 是单 mutex 串行化 per-receipt 的 PATCH
- 即同一 receipt 内多次 PATCH 也是 mutex 顺序执行，不会并发
- 串行化天然保证 PATCH ≤ 1/200ms = 5 QPS per message_id

Limiter 只守住"app 级 + per-user + per-group"三个 hard limit。per-message_id 限速交给 `renderLocked` mutex。

### 1.6 UX 权衡（5 QPS 不是没有代价）

Receipt PATCH storm（一个 agent turn 内 receipt 被 PATCH 多次）受 F-35 limiter 串行化。**测算**：

- 每个 PATCH 过 `Wait()`，等 ~200ms（5 QPS）
- 10 events 的 PATCH storm 总耗时 ≈ 1.8s
- 用户视觉：receipt 卡片内容更新稍慢（动画可见），但绝不触顶飞书限流

**为什么不暴露 override**：用户主动调高会冒 230001 风险。改成 `feishu.rate_limit.rate_per_sec: 50`（贴 app 上限）后，单 chat 短时 PATCH 密集仍可能短暂触顶 per-user 限流。保守 5 QPS 是不留弹性的选择。

---

## 2. 实测飞书限频（数据来源 open.feishu.cn）

| API | App 级 QPS | App 级 QPM | Per-resource |
|---|---|---|---|
| Send / Reply message | **50 QPS** | 1000 QPM | **5 QPS per user** + **5 QPS per group**（群内机器人共享） |
| PATCH message | **50 QPS** | 1000 QPM | **5 QPS per message_id**（"单条消息更新频控为 5 QPS"） |
| Delete message | 50 QPS | 1000 QPM | — |
| AddReaction | 50 QPS | 1000 QPM | — |
| Upload image / file | 50 QPS | 1000 QPM | — |

**nightme 热路径分析**：
- 用户发消息 → bot reply → 同 user 的 5 QPS 限制是瓶颈
- Receipt PATCH storm（一个 turn 5-20 次 PATCH）→ per-message_id 5 QPS 由 `renderLocked` mutex 天然满足
- 多个 chat 并发 → App 级 50 QPS 是天花板

**结论**：把全局桶设为 **5 QPS / burst 1** 同时满足：
- ✅ ≤ 5 QPS per user（硬限）
- ✅ ≤ 5 QPS per group（硬限）
- ✅ ≤ 5 QPS per message_id（硬限）
- ✅ ≤ 50 QPS per app（硬限，留 90% 余裕）

---

## 3. 配置

### 3.1 Config schema

`internal/config/config.go::FeishuConfig` 新增 `RateLimit` 字段：

```go
type FeishuConfig struct {
    AppID             string `yaml:"app_id"`
    AppSecret         string `yaml:"app_secret"`
    VerificationToken string `yaml:"verification_token"`
    EncryptKey        string `yaml:"encrypt_key"`

    // Rate limit（保守默认，留空 = StrictDefault）
    RateLimit *FeishuRateLimitConfig `yaml:"rate_limit,omitempty"`
}

// FeishuRateLimitConfig 控制 feishu 包内全局 token bucket。
// 留空 = StrictDefault = RatePerSec=5, Burst=1（保守，零弹性）。
// 调高 = 冒触顶飞书限流错误码 230001/230020 风险。
type FeishuRateLimitConfig struct {
    RatePerSec float64 `yaml:"rate_per_sec"`  // 每秒补充令牌数
    Burst      int     `yaml:"burst"`         // 桶容量（最大突发）
}
```

### 3.2 默认值（`StrictDefault`）

```go
var StrictDefault = FeishuRateLimitConfig{
    RatePerSec: 5,  // per-user 硬限
    Burst:      1,  // 无突发
}
```

### 3.3 `config.yaml` 示例

```yaml
feishu:
  app_id: cli_xxx
  app_secret: xxx
  # 可选；不填 = StrictDefault（保守）
  rate_limit:
    rate_per_sec: 5
    burst: 1
```

**调高的代价**：rate_per_sec > 5 → 单 chat PATCH storm 可能短暂触顶 per-user 限流；burst > 1 → 启动期或空闲后突发达 N 个调用，可能触顶。

---

## 4. 代码契约

### 4.1 Limiter struct

```go
// internal/channel/feishu/ratelimit.go

type clock interface {
    Now() time.Time
}
type realClock struct{}
func (realClock) Now() time.Time { return time.Now() }

type Limiter struct {
    cfg    FeishuRateLimitConfig
    clock  clock
    logger *slog.Logger

    mu         sync.Mutex
    tokens     float64
    lastRefill time.Time
}

func NewLimiter(cfg *config.FeishuRateLimitConfig, logger *slog.Logger) *Limiter
func (l *Limiter) Wait(ctx context.Context) error
func (l *Limiter) SetClock(c clock)  // 仅测试
```

### 4.2 Wait 实现要点

```go
func (l *Limiter) Wait(ctx context.Context) error {
    for {
        l.mu.Lock()
        now := l.clock.Now()
        elapsed := now.Sub(l.lastRefill).Seconds()
        l.tokens = math.Min(float64(l.cfg.Burst), l.tokens + elapsed*l.cfg.RatePerSec)
        l.lastRefill = now

        if l.tokens >= 1.0 {
            l.tokens -= 1.0
            l.mu.Unlock()
            return nil
        }

        deficit := 1.0 - l.tokens
        waitSec := deficit / l.cfg.RatePerSec
        l.mu.Unlock()

        timer := time.NewTimer(time.Duration(waitSec * float64(time.Second)))
        select {
        case <-timer.C:
        case <-ctx.Done():
            timer.Stop()
            return ctx.Err()
        }
    }
}
```

### 4.3 clock 注入（testability）

```go
// 测试用 fake clock
type fakeClock struct {
    mu sync.Mutex
    t  time.Time
}
func (f *fakeClock) Now() time.Time {
    f.mu.Lock(); defer f.mu.Unlock()
    return f.t
}
func (f *fakeClock) Advance(d time.Duration) {
    f.mu.Lock(); defer f.mu.Unlock()
    f.t = f.t.Add(d)
}
```

### 4.4 监控埋点

Wait 阻塞 > 100ms 时记 debug log（避免 hot path 日志噪音）：

```go
if waitDur > 100*time.Millisecond {
    l.logger.Debug("feishu rate limit blocked",
        "wait_ms", waitDur.Milliseconds(),
        "tokens", l.tokens,
        "rate_per_sec", l.cfg.RatePerSec,
        "burst", l.cfg.Burst,
    )
}
```

---

## 5. 接入点

`internal/channel/feishu/adapter.go` 的 4 个底出口，**每个 SDK call 前都过 Wait**：

| 函数 | Wait 调用位置 |
|---|---|
| `sendViaLarkCreate` | `a.limiter.Wait(ctx)` 在 `client.Im.V1.Message.Create(...)` 之前 |
| `sendViaLarkReply` | 同上，在 `Message.Reply(...)` 之前 |
| `updateViaLark` | 在 `Message.Patch(...)` 之前 |
| `AddReaction` | 在 `MessageReaction.Create(...)` 之前 |

**`sendContent` 包装层不动**：rootID fallback 路径（230011 → top-level Create）第二次走 `sendViaLarkCreate` 仍会经 Wait，**单桶自动覆盖**。

**`GetBotIdentity` 不走 limiter**：启动期低频，且不走 IM 配额。

---

## 6. 测试

`internal/channel/feishu/ratelimit_test.go` 6 个单测：

| 用例 | 验证 |
|---|---|
| `TestNewLimiter_StrictDefault` | cfg=nil → RatePerSec=5, Burst=1 |
| `TestLimiter_ConfigOverride` | cfg={RatePerSec:10, Burst:2} → 应用生效 |
| `TestLimiter_InitialBurst` | 初始 tokens=1；连续 acquire 2 次：第一次立即成功，第二次 wait ≥ 200ms |
| `TestLimiter_Refill` | fakeClock.Advance(200ms) → tokens 重填，下次 acquire 立即成功 |
| `TestLimiter_ContextCancel` | Wait 阻塞中 ctx.Done() 立即返回 ctx.Err() |
| `TestLimiter_LongRunNoOvershoot` | fakeClock 跑 10s，acquire 51 次（5/s × 10 + initial burst），实际等待总和符合配置 |

集成测试（`adapter_test.go`）：

| 用例 | 验证 |
|---|---|
| `TestAdapter_RateLimit_PATCHStormThrottled` | 配 StrictDefault；mock sendFunc 记录 timestamp；触发 20 个连续 OutText → mock 收到的 timestamp 间隔 ≥ 200ms |

---

## 7. 与现有组件的关系

| 组件 | 交互 |
|---|---|
| `larkws.WebSocket`（SDK 内置重连） | 不变；limiter 只约束**出口** API call，不约束 inbound |
| `sendContent` rootID fallback（230011/231003） | 不变；fallback 第二次走 Create 仍经 Wait（单桶自动覆盖） |
| `MessageReceipt.renderLocked` mutex | 不变；5 QPS / message_id 由它天然满足，不需 limiter 介入 |
| Layer 1 `WithTransientRetry`（未来 PR） | 正交；retry 在 sendContent 外层，limiter 在 SDK call 内层，互不感知 |
| Echo channel（test only） | 不走 feishu，无影响 |

---

## 8. 不在本 PR scope

- ❌ per-chat 限速（不是飞书侧约束）
- ❌ 跨 daemon 实例全局令牌服务（不解决多实例共享 App ID）
- ❌ Prometheus metrics（仅预留 `OnWait` hook，不实现）
- ❌ 动态调整（启动读 config，运行时不变）
- ❌ Layer 1 transient retry（独立 PR，正交）

---

## 9. 实施步骤

1. 新增 `internal/channel/feishu/ratelimit.go`（Limiter + clock + Wait + StrictDefault）
2. 新增 `internal/channel/feishu/ratelimit_test.go`（6 个单测）
3. 改 `internal/config/config.go::FeishuConfig` 加 `RateLimit` 字段
4. 改 `internal/channel/feishu/adapter.go`：
   - `Adapter` 加 `limiter *Limiter` 字段
   - `NewAdapter` 末尾 `a.limiter = NewLimiter(cfg.Feishu.RateLimit, logger)`
   - 4 个底出口（`sendViaLarkCreate` / `sendViaLarkReply` / `updateViaLark` / `AddReaction`）首行加 `if err := a.limiter.Wait(ctx); err != nil { return ... }`
5. 验收：`go build ./... && go test ./internal/channel/feishu/... ./internal/config/... && go vet ./...`

---

---

## A2. F-36: Feishu Channel Transient Retry

> **Source**: `../channel/feishu-reliability.md`


---

## 0. 背景

F-35 限速器是"事前预防"：在 nightme 侧就阻断触达飞书 230001 的可能。但**瞬时网络抖动**（timeout / EOF / connection reset）不在限速控制范围内——这些是 TCP / SDK 层面的瞬态错误，不受 QPS 限制。

F-36 是"事后补救"：飞书 SDK call 返回 transient 错误时自动重试，避免事件丢失。

**两者正交**：
- F-35 limiter 在 SDK call **内层**，防 rate-limit（230001）
- F-36 retry 在 sendContent / updateViaLark / AddReaction **外层**，防 transient 网络抖动
- 触发路径：limiter.Wait → SDK call → 失败 → retry outer → 重新 limiter.Wait → SDK call → ...

---

## 1. 设计原则

### 1.1 只重试 transient 错误

错误分类（详见 `retry.go::IsTransient`）：

| 类别 | 例子 | 处理 |
|---|---|---|
| **Transient**（重试） | `net.Error.Timeout()` / `io.EOF` / `io.ErrUnexpectedEOF` / `syscall.ECONNRESET,EPIPE` / substring 匹配 ("connection reset" / "broken pipe" / "i/o timeout" / "TLS handshake timeout" / "connection refused" / "no such host") | 指数退避重试 |
| **Permanent**（不重试） | Feishu code 230011 / 231003（terminal，sendContent fallback 接管）/ 230001（rate-limit，limiter 应已防住）/ 其他 Feishu 永久错误码 / `context.Canceled` / `context.DeadlineExceeded` | 立即返回 |

**为什么不重试 Feishu 业务错误码**：
- **230011 / 231003**：message 已撤回/删除，重试无意义（root_id 永远不可用）。`sendContent` 的 rootID → top-level Create fallback 接管
- **230001**：限流。F-35 limiter 应已防住；如果还触发，说明配置需调整，**透传给上层比沉默吞掉更有价值**
- **其他永久错误码**（如 99991663 invalid token）：重试浪费 budget，立即 fail 暴露问题

### 1.2 不留 burst / 严格限速

`DefaultRetryConfig`：

```go
MaxAttempts:    3,                  // initial + 2 retries
InitialBackoff: 500 * time.Millisecond,
MaxBackoff:     5 * time.Second,
JitterPercent:  0.25,               // ±25% 防 thundering herd
```

3 次重试足够 cover 绝大多数抖动；超过则记降级日志并返回最后一次错误。

### 1.3 ctx 取消优先

`ctx.Done()` 在任何等待点（retry backoff / limiter wait）都立即返回 `ctx.Err()`：
- **daemon shutdown 期间调用 Send**，不等 retry 完
- **Chatsession 跨切换时**，旧的 AgentSession 关联的 ctx cancel，不污染新 session

降级日志会记录 cancellation 事件，便于事后分析 daemon 是否正常退出。

### 1.4 与现有架构正交

- **不改 `Channel` 接口契约**（`Send` 仍 fire-and-ack）
- **不与 F-35 limiter 耦合**（retry 在外层，limiter 在内层）
- **不动 sendContent 的 rootID fallback 语义**（fallback 也走 retry 救活瞬时失败）

---

## 2. 配置

`RetryConfig` 在 `retry.go` 定义：

```go
var DefaultRetryConfig = RetryConfig{
    MaxAttempts:    3,
    InitialBackoff: 500 * time.Millisecond,
    MaxBackoff:     5 * time.Second,
    JitterPercent:  0.25,
}
```

零 / 负值由 `normalize()` 静默回退到默认值（与 F-35 `NewLimiter` 行为一致）。

**不在 config.yaml 暴露**：retry 策略是 feishu 包自治参数，不属于用户配置面。调它需要改源码并重发版——刻意设置"留 90% 余地给 SDK 行为兜底"。

---

## 3. 接入点

`internal/channel/feishu/adapter.go` 的 4 个底出口，每个 SDK call 前都过 `WithTransientRetry`：

| 函数 | Retry 包裹 | Op 名 |
|---|---|---|
| `sendContent` → `sendViaLarkCreate` | ✅ | `"send"` / `"send_top_level"` |
| `sendContent` → `sendViaLarkReply` | ✅ | 同上 |
| `updateViaLark` | ✅ | `"patch_message"` |
| `AddReaction` | ✅ | `"add_reaction"` |

**`sendContent` 的 rootID fallback 也走 retry**：fallback 重新调 send 不带 rootID，整段包 retry。如果 fallback 自身也遇 transient，会再次退避重试；最终失败会再走 `logDegradation` 记录。

**不动 receipt / gateway / chatsession**：retry 是 feishu 包自治，外部仍 fire-and-ack。

---

## 4. 降级日志（post-analysis）

每次 retry 降级事件都 emit 一条 warn-level structured log，字段名稳定可 grep / 接入 dashboard：

| 字段 | 含义 |
|---|---|
| `degradation` | 事件类型：`retry_exhausted` / `ctx_cancel_during_wait` / `ctx_cancel_at_entry` / `fallback_to_top_level` / `limiter_wait_cancelled` |
| `op` | `"send"` / `"send_top_level"` / `"patch_message"` / `"add_reaction"` |
| `attempts` | 已尝试次数（ctx cancel at entry 时为 0） |
| `total_wait_ms` | retry 累计等待时长（毫秒） |
| `final_err` | 最终错误（transient 或 terminal） |
| `ctx_err` | ctx 取消时填 `context.Canceled` / `context.DeadlineExceeded` |
| `chat_id` | call site 提供 |
| `message_id` | call site 提供（userMsgID / receiptID / messageID） |
| `root_id` | call site 提供（仅 send / send_top_level） |
| `msg_type` | call site 提供（仅 send / send_top_level） |
| `reaction_type` | call site 提供（仅 add_reaction） |

**示例（grep 模式）**：

```bash
# 找出所有降级事件
grep 'feishu degradation' /var/log/nightme.log

# 找出 retry exhausted（最严重 — API 持续失败）
grep '"degradation":"retry_exhausted"' /var/log/nightme.log

# 找出 daemon shutdown 期间的降级（应均为 ctx cancel）
grep '"degradation":"ctx_cancel_during_wait"' /var/log/nightme.log

# 找出 fallback to top-level（user 撤回过原消息）
grep '"degradation":"fallback_to_top_level"' /var/log/nightme.log
```

**为什么是 warn 级**：降级不是 fatal——daemon 还在跑、用户感知是"消息慢一点 / 多一条独立消息"。但事件需要保留（不能被 default log level 过滤掉）以便事后分析。

---

## 5. 与 F-35 的边界

```
sendContent(ctx, chatID, msgType, content, rootID)
  └ WithTransientRetryMsg(RetryOpts{Op:"send", Cfg:DefaultRetryConfig, ...})
    └ func() (string, error) {
        return send(ctx, chatID, msgType, content, rootID)
      }
       └ sendViaLark (rootID != "" → sendViaLarkReply, else sendViaLarkCreate)
         └ limiter.Wait(ctx)       ← F-35 内层
         └ SDK call
```

每次 retry 都重新过 limiter：单次 retry 至少等 limiter 的 backoff (200ms @ 5 QPS)。3 次 retry 总耗时 ≈ 1.5s（500ms + 1s backoff + 200ms × 3 limiter wait）。可接受。

---

## 6. 测试

`internal/channel/feishu/retry_test.go` 14 个单测：

| 类别 | 用例 | 验证 |
|---|---|---|
| **IsTransient 分类** | TestIsTransient_NilAndCtxErrors | nil / context errors 都不是 transient |
| | TestIsTransient_NetTimeout | net.Error.Timeout() → true |
| | TestIsTransient_EOFAndSyscall | io.EOF / syscall.ECONNRESET,EPIPE → true |
| | TestIsTransient_SubstringFallback | 6 类 substring → true |
| | TestIsTransient_FeishuCodesNeverRetry | 230011/231003/230001 → false |
| | TestIsTransient_PermanentErrors | 其他飞书码 → false |
| **retry 行为** | TestWithTransientRetry_SuccessNoRetry | 一次成功，无 retry |
| | TestWithTransientRetry_RetriesOnTransientThenSuccess | 第 3 次成功 |
| | TestWithTransientRetry_NoRetryOnPermanent | 1 次失败立即返回 |
| | TestWithTransientRetry_ExhaustsAfterMaxAttempts | 3 次全失败，最后一次错误返回 |
| | TestWithTransientRetry_ContextCancelStopsRetry | ctx cancel 立即返回 ctx.Err() |
| | TestWithTransientRetry_CtxCancelledAtEntry | 入口时 ctx 已 cancel，fn 不被调用 |
| | TestWithTransientRetry_NormalizesZeroConfig | 零 / 负 cfg 自动 fallback |
| | TestWithTransientRetryMsg_RetainsMessageIDOnError | msg 变体保留 message_id |
| | TestWithTransientRetryMsg_SuccessNoRetry | msg 变体成功路径 |
| | TestWithTransientRetry_DegradationLogEmitted | 验证降级日志 schema 字段 |
| **jitter** | TestJitter_BoundsRespected | 1000 次采样全部落在 ±25% 区间 |

---

## 7. 不在本 PR scope

- ❌ per-op retry 策略（所有 op 用同一份 DefaultRetryConfig）
- ❌ 用户级配置（retry 策略不是用户面）
- ❌ 持久化重试历史（事件丢失 vs daemon crash 的取舍留给 Layer 2 outbox）
- ❌ Layer 2 outbox 化（独立 PR）

---

## 8. 实施步骤

1. 新增 `internal/channel/feishu/retry.go`（`RetryConfig` + `DefaultRetryConfig` + `normalize` + `IsTransient` + `logDegradation` + `RetryOpts` + `WithTransientRetry` + `WithTransientRetryMsg` + `jitter`）
2. 新增 `internal/channel/feishu/retry_test.go`（14 单测）
3. 改 `internal/channel/feishu/adapter.go`：
   - `sendContent` 把 `send(...)` 包进 `WithTransientRetryMsg`，fallback 也包
   - `updateViaLark` 把 SDK call 包进 `WithTransientRetry`
   - `AddReaction` 把 SDK call 包进 `WithTransientRetryMsg`
   - 引入 `addReactionOnce` / `patchMessageOnce` 拆 raw SDK call
4. 改 `internal/channel/feishu/ratelimit.go`：
   - `Wait()` 的 ctx cancel 分支加降级日志（`degradationLimiterWaitCancel`）
5. 验收：`go build ./... && go vet ./... && go test -race ./internal/channel/feishu/...`

---

---

## A3. F-41: Active Reconnect (30s Forced Stop+Start, No HTTP Probe)

> **Source**: `../channel/feishu-reliability.md`


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
- **daemoncontrol RPC 协议不变** ── health JSON 多了 `prober` 字段,client 解析忽略未知字段
- **prober 不抢 SDK 的重连** ── 我们只是周期性 kill + respawn SDK,SDK 内部机制不变
- **prober 永不主动退出(除非 Connected 或 daemon shutdown)** ── 这是有意的;不引入"放弃重连"这种语义
- **circuit breaker / tier escalation / watchdog 不存在** ── 简化掉;有需要可以后加

---

## 5. 落地顺序 (commit 切分)

```
commit A: docs(feishu): F-41 active reconnect design
         docs/SPEC.md (§1.4 changelog)
         docs/channel/feishu.md (§13.18 decision record)
         CHANGELOG.md (F-41 entry)
         docs/channel/feishu-reliability.md (NEW)

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

## 8. 不叫 的理由 (与 F-37 / F-39 一致) 不变式全部保留:
- OutboundMessage 契约不变
- Gateway 不动
- ChatSession 不动
- §1.3 不变式不动
- §1.4 边界规范不动

F-41 是 Channel 自治范围内的事(WS 连接管理 = 飞书实现细节),不影响 nightme 数据模型与 Gateway 契约。

---

