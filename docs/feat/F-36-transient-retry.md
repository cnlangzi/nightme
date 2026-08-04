# F-36: Feishu Channel Transient Retry

> **Status**: implemented (v1.3.x)
> **Scope**: `internal/channel/feishu/retry.go` — feishu 包内 transient 错误的指数退避重试
> **目的**: 飞书 SDK call 偶发网络抖动（timeout / EOF / connection reset）时自动重试，避免事件丢失
> **Related docs**:
> - [docs/feat/F-35-ratelimit.md](./F-35-ratelimit.md) — F-35 全局限速器；F-36 是其"事后补救"层
> - [docs/channel/feishu.md §17](../channel/feishu.md) — channel 实现细节、4 个出口的接入点、降级日志
> - [docs/SPEC.md §11](../SPEC.md) — backlog 引用
> **设计参考**：cc-connect `core/withTransientRetry` + `withFreshTenantAccessTokenRetry`；openclaw-lark 不显式 retry，靠 SDK 内部重试 + 兜底降级。

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

## 9. 变更日志

- **2026-08-04** — 初版。F-36 feishu 包内 transient retry（指数退避 + jitter + 降级日志）落地，覆盖 sendContent / updateViaLark / AddReaction 三个出口。