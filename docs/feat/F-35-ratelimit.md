# F-35: Feishu Channel Rate Limit

> **Status**: implemented (v1.3.x)
> **Scope**: `internal/channel/feishu/ratelimit.go` — feishu 包自治的全局 token bucket 限速器
> **目的**: 在 nightme 侧就阻断触达飞书 230001 的可能，而不是事后 retry
> **Related docs**:
> - [docs/channel/feishu.md §16](../channel/feishu.md) — feishu adapter 内的接入细节、4 个出口的 Wait 调用点
> - [docs/SPEC.md §11](../SPEC.md) — backlog 引用
> **官方文档**（实测 2026-08-04）：
> - [Send message](https://open.feishu.cn/document/server-docs/im-v1/message/create) — 1000 QPM / 50 QPS per app + 5 QPS per user / 5 QPS per group
> - [PATCH message](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/patch) — 1000 QPM / 50 QPS per app + 5 QPS per message_id
> - [Delete message](https://open.feishu.cn/document/server-docs/im-v1/message/delete) — 1000 QPM / 50 QPS per app
> - [AddReaction](https://open.feishu.cn/document/server-docs/im-v1/message-reaction/create) — 1000 QPM / 50 QPS per app
> - [Upload image](https://open.feishu.cn/document/server-docs/im-v1/image/create) — 1000 QPM / 50 QPS per app

---

## 0. 背景

nightme 的飞书 adapter (`internal/channel/feishu/adapter.go`) 在每个 agent turn 中会向飞书发起多次 API 调用：冷启动一张 card、event 触发多次 PATCH、AddReaction 状态 emoji、heartbeat 周期 PATCH 等。**hot path 完全同步**——readPump → `EventCallback` → `channel.Send` → SDK call，无队列、无 backpressure、无重试。

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

**Heartbeat PATCH 也走 `updateViaLark`** → 自动经 Wait。30 min 一次，远低于 5 QPS，**不需特判**。

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
| Heartbeat goroutine（F-23） | 自动经 limiter；30 min 一次，无影响 |
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

## 10. 变更日志

- **2026-08-04** — 初版。F-35 feishu 全局限速器（5 QPS / burst 1 / lazy refill）落地。