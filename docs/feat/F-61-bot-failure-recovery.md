# F-61: Bot Failure Recovery — Watchdog, Respawn, and Replay

> **Status**: Draft
> **Date**: 2026-08-12
> **Author**: 夜me
> **Branch**: `feat/F-61-bot-failure-recovery`(本仓在 worktree `docs-readme-install` 中开发)
> **触发**: 用户在飞书发送 `/close` + image 后,bot 静默无响应,体感"整个 nightme 卡住"。详见下文 §1.1 incident 还原。

## 1. 背景与动机

### 1.1 Incident 还原(2026-08-12)

用户在飞书向 LzBook(`oc_c8227dc7904fc9ec14a2d42f2b3c295f`,workspace `docs-readme-install`)连发三条:

```
20:44:53  text   "kill本地的所有Claude进程"
20:45:13  text   "/use pi"
20:45:35  text   "/close"
```

bot 在 20:45:36-37 正常返回 `Closed 1 bridge process(es) (sessions preserved)`,并加了 ✓ reaction。日志同步记录:

```
20:45:36  chatsession: AS marked Exited (claude process exited)
20:45:37  feishu: outgoing add_reaction  ✓
```

随后用户在 20:46:47 发了一条带 image 的消息(`om_x100b68f2fdbbd0a0b49120143ce241d`)。日志:

```
20:46:47  INFO   feishu: all attachments failed to download; dropping message
20:46:47  WARN   feishu degradation ctx_cancel_at_entry op=send attempts=0 ...
20:46:47  WARN   feishu: outgoing failed kind=send_text err="context canceled"
```

之后 28+ 分钟 daemon 零 inbound / outbound 事件,直到用户用 `claude --resume aa196df8...` 在 CLI 里手动查询 → 才触发"卡住"的反馈。

**根因(三处叠加)**:

1. **`adapter.go:3167` AllFailed 分支**:`SendMessage` 用同一个 inbound `ctx`,而 ctx 在 `downloadOneWithRetry` 期间已被 SDK 取消,`WithTransientRetry` 直接吃 `ctx.Canceled` 返回。降级提示未送达。
2. **`SetExited` 不写盘**:`agent_sessions.json` 中 `as_1786534213780918000_2` 仍是 `pid=16056, status=running`,而 PID 16056 已死。`nightme list` 显示的 PID/状态有滞后。
3. **缺 watchdog + 主动 probe**:bridge 在用户视线外死掉后,daemon 不主动 respawn,只能等下一条 inbound 触发 `LookupSelectedAgentSession`(而 AllFailed 让 inbound 不再到达)。
4. **`DownloadAttachments` 单次失败即放弃**:没外层 retry,网络抖动一次就 AllFailed。详见 §3.3 的 ladder 设计。

### 1.2 为什么"必须修"

- **bot 静默 = 比报错还糟**:用户没法判断是 IM 端坏、bot 端坏、还是 agent 端坏
- **恢复路径已经存在,只是没接上**:`Manager.RestoreFromRegistry` 在 daemon 重启时已经能重投 in-flight messages(`manager.go:540-575`),bridge 运行时死亡应该走同一条路
- **persistence 漏洞影响所有诊断面**:`nightme list` / `doctor` / 后续 prober 全部依赖 `agent_sessions.json` 的准确性

## 2. 目标 / 非目标

### 目标
- **优先静默恢复**:bridge 死了 → respawn + `--resume` 接住,用户只看到稍慢一点的正常回复
- **in-flight 重投**:bridge 死时正处理的消息,跟 daemon 重启走同一条 RestoreFromRegistry 重投路径
- **adapter 层的附件下载**:外层重试 ladder(2-3 次,backoff 5s/15s),都失败才走 text-only 降级
- **真恢复不了** → 才告知用户(明确文本 + 原因 + 建议)
- **`agent_sessions.json` 状态准确**:`SetExited` / `SetSuspect` 立即写盘

### 非目标
- 不做 endpoint health scoreboard / metrics dashboard
- 不改 feishu WS reconnect(F-41 已覆盖)
- 不动 `nightme doctor` 的现有展示,只在 `PROBER` 字段多加一行
- 不引入外部依赖(etcd / redis / etc.)

## 2. 目标 / 非目标

### 目标
- **优先静默恢复**:bridge 死了 → respawn + `--resume` 接住,用户只看到稍慢一点的正常回复
- **in-flight 重投**:bridge 死时正处理的消息,跟 daemon 重启走同一条 RestoreFromRegistry 重投路径
- **adapter 层的丢弃**(附件全失败等)→ 自动降级(text-only 入队),不静默
- **真恢复不了** → 才告知用户(明确文本 + 原因 + 建议)
- **`agent_sessions.json` 状态准确**:`SetExited` / `SetSuspect` 立即写盘

### 非目标
- 不做 endpoint health scoreboard / metrics dashboard
- 不改 feishu WS reconnect(F-41 已覆盖)
- 不动 `nightme doctor` 的现有展示,只在 `PROBER` 字段多加一行
- 不引入外部依赖(etcd / redis / etc.)

## 3. 设计

### 3.1 恢复优先级(决策树)

```
                   ┌─ 能否静默重投?──────────────────┐
                   │                                  │
                   │ 是                              │ 否
                   ▼                                  ▼
       respawn + --resume                  能否自动降级?
       (用户无感,可能稍慢)                    │
                                            ├─ 是: text-only 入队
                                            │     (用户无感,附件丢失)
                                            │
                                            └─ 否: 告知用户
                                                  ❌ + 原因 + 建议
```

### 3.2 In-flight 重投(对接 RestoreFromRegistry)

`Manager.RestoreFromRegistry`(`internal/chatsession/manager.go:540-575`)已有完整重投循环:
- 遍历每个 AS 的 `InFlightMessages`(持久化在 `agent_sessions.json`)
- 复制 blocks,推回 `cs.queue`
- 下次 `TryFlush` 时由新 spawn 的 bridge `--resume <sessionID>` 接住

**当前缺口**:仅 daemon 重启路径触发。`readpump` 检测到 process death 时,`endPrompt(PromptEndProcessDied)`(`internal/agentsession/readpump.go:101`)直接把 `as.inFlightMessages = nil`,没人把这批消息推到 queue。

**修复点**(`internal/agentsession/readpump.go:208-257`):

```go
func (as *AgentSession) endPrompt(reason PromptEndReason) {
    as.asMu.Lock()
    p := as.currentPrompt
    if p == nil {
        as.asMu.Unlock()
        return
    }

    // F-61: 死亡快照 — 在清空 currentPrompt 之前,把 in-flight
    // blocks 复制到一个 channel,让 routeEvent 推到 cs.queue。
    // 与 RestoreFromRegistry (manager.go:540) 走同一条重投路径。
    snapshot := append([]agent.ContentBlock(nil), p.Messages...)
    as.currentPrompt = nil
    as.inFlightMessages = nil
    as.isReady.Store(true)
    as.asMu.Unlock()

    if reason == PromptEndProcessDied {
        as.replayCh <- replayReq{promptID: p.ID, blocks: snapshot}
    }
    ...
}
```

接收端(`chatsession/pump_events.go`,在现有 `case KindPromptEnded:` 分支里):

```go
case KindPromptEnded:
    cs.writebackMessageState(as, ev.Prompt)
    if ev.Prompt.EndReason == agentsession.PromptEndProcessDied {
        // F-61: bridge 死亡场景的重投,与 RestoreFromRegistry 一致
        if err := cs.queue.Push(Message{
            ID:         ev.Prompt.LastMessageID,
            ChatID:     cs.S,
            Blocks:     ev.Prompt.Messages, // 已经防御性 copy
            ReceivedAt: ev.Prompt.StartedAt,
        }); err != nil {
            slog.Warn("chatsession: in-flight replay after bridge death failed",
                "chat_id", cs.S, "as_id", as.ID, "err", err)
        }
    }
    _ = cs.TryFlush()
```

注意:
- 必须在 `SetExited` 之前发生 — 否则 `TryFlush` 会因 `StatusExited` 直接 SKIP(见 `chatsession.go:917-925`)
- 实际上 `endPrompt` → emit KindPromptEnded → emitLifecycleLocked(Exited) 是顺序的,pump_events.go:117-127 的两个 case 是分开的,但路由由同一 pump 处理,需要确认顺序

### 3.3 附件下载重试 ladder + AllFailed 降级

`internal/channel/feishu/adapter.go:3165-3192` 当前是 `DownloadAttachments` 一次失败就声明 AllFailed,然后 `return nil`。两层问题:

1. **无外层 retry**:内层 `downloadOneWithRetry`(`attachment.go:397-422`)已有 per-attachment retry + backoff,但如果**所有**附件一起失败(网络抖动 / 飞书 API 临时不可用),外层没有任何补救,直接 AllFailed
2. **AllFailed 直接 `return nil`**:消息丢失,既不重投也不告知

**改为外层 ladder + 降级入队**:

```go
// adapter.go:3165 位置:抽出 downloadWithRetry 函数
func (a *Adapter) downloadWithRetry(ctx context.Context, messageID string, attachments []channel.Attachment, chatID string) (DownloadResult, error) {
    cfg := downloadRetryConfig{
        MaxAttempts: 3,         // 总共 3 次:立即 + 5s + 15s
        Backoffs:    []time.Duration{0, 5 * time.Second, 15 * time.Second},
    }
    var lastResult DownloadResult
    for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
        if cfg.Backoffs[attempt-1] > 0 {
            select {
            case <-time.After(cfg.Backoffs[attempt-1]):
            case <-ctx.Done():
                return lastResult, ctx.Err()
            }
            // 反馈用户:重试中
            if a.health != nil {
                a.health.recordRetryAttempt(chatID, messageID, attempt)
            }
        }
        result := a.DownloadAttachments(ctx, messageID, attachments, chatID)
        lastResult = result
        if !result.AllFailed {
            return result, nil
        }
        slog.Warn("feishu: attachment download attempt failed; will retry",
            "message_id", messageID,
            "attempt", attempt,
            "of", cfg.MaxAttempts,
        )
    }
    return lastResult, nil // AllFailed,让上层降级
}
```

调用点(`adapter.go:3165`):

```go
if len(attachments) > 0 {
    result, _ := a.downloadWithRetry(ctx, messageID, attachments, chatID)
    if result.AllFailed {
        slog.Warn("feishu: all attachment retries exhausted; degraded to text-only",
            "message_id", messageID,
            "attempts", downloadRetryConfig.MaxAttempts,
        )
        // 走 background ctx,绝不复用 inbound ctx(reuse retry ctx bug)
        ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        _ = a.SendMessage(ctx2, chatID,
            fmt.Sprintf("⚠️ %d attachment(s) failed to download after %d attempts; sending text only.",
                len(result.FailureKeys), downloadRetryConfig.MaxAttempts))

        attachments = nil
        blocks = resolveBlocks(blocks, nil)
        // 不 return,继续走 publish(text-only)
    } else if len(result.FailureKeys) > 0 {
        // 部分失败:警告 + 继续(现有逻辑保留)
        ...
    }
    attachments = result.Atts
}
```

**为什么用 background ctx 发告知**:复用 inbound ctx 正是 §1.1 incident 的根因之一(`WithTransientRetry` 看到 ctx 已 cancel 直接吃)。告知消息必须独立于 inbound ctx。

**为什么 ladder 上限 3 次**:外层 3 次 + 内层 `downloadOneWithRetry` 已有 per-attempt retry(详见 `attachment.go:397-422`),总失败概率被乘起来已经很低。再多就是浪费用户等待,真断网 1 分钟用户早就知道手动重发了。

**retry 期间的反应反馈**:复用现有 reaction FSM(见 `slash-command-reactions.md`)。可以加一个"⏳ retrying attachment (attempt 2/3)"的中间 reaction,放在 `health.recordRetryAttempt` 里(增量改造,不新造 reaction 路由)。

### 3.4 SetExited / SetSuspect 写盘

`internal/agentsession/session.go:761-768`:

```go
func (as *AgentSession) SetExited(code int) {
    as.asMu.Lock()
    as.pid = 0
    as.stat = StatusExited
    as.lastRunAt = time.Now()
    as.exitCode = &code
    persist := as.persist
    as.asMu.Unlock()

    // F-61: 写盘失败只 warn,不影响内存态正确性
    if persist != nil {
        if err := persist(as.Entry()); err != nil {
            slog.Warn("agentsession: persist after SetExited failed; JSON may be stale",
                "as_id", as.ID, "err", err)
        }
    }
}
```

对称增加 `SetSuspect(reason string)` / `ClearSuspect()`(只读锁改写锁 + 写盘)。

### 3.5 Watchdog(按需,主线)

**`internal/chatsession/watchdog.go`**:每个 ChatSession 一个,挂在 Manager 上。

两种 timer:

| Timer | 触发 | 期望 | 到期动作 |
|-------|------|------|----------|
| **FastAck**(10s) | `cs.inboundAccepted(msg)` 后 | 10s 内有 outbound(add_reaction / send_card) | `markSuspect("no_fast_ack")` → 主动 probe |
| **HungPrompt**(5min,可调) | `TryFlush` Submit 成功后 | `KindPromptEnded` 在 T 内回来 | `markSuspect("hung_prompt")` → 主动 probe |

**触发探针后,probe 阶梯**:

```
probe(as):
1. kill(as.pid, 0)
   ├─ ESRCH → 进程没了 → SetExited + persist + 触发 lazy respawn
   └─ 0     → 进程还在
       │
       2. (可选) SIGUSR1 + read 1 byte (claude/pi 协议支持的话)
          ├─ 有回应 → 标记 degraded,等下一个 HungPrompt 触发再处理
          └─ 无回应 / 协议不支持 → kill(SIGTERM) + 2s grace + SIGKILL + lazy respawn
```

**cooldown**:每个 AS 每 5 分钟最多 1 次主动 respawn(避免 spawn 抖动风暴)。

### 3.6 Blanket prober(兜底,镜像 F-41)

**`internal/chatsession/prober.go`**:模仿 `internal/channel/feishu/reconnect.go:50-230` 的 prober 模式。

- 30s ticker
- 遍历所有 `StatusRunning` 的 AS
- `kill(pid, 0)` — ESRCH → `SetExited` + 触发 lazy respawn
- **不做** HungPrompt 检测(那是 watchdog 的事)
- 暴露 `Snapshot()` 给 `nightme doctor`,沿用 F-41 输出格式:

```
PROBER (F-61 agent liveness)
  active:           yes
  interval:         30s
  scanned:          4 running ASes
  probes_run:       142
  respawns_triggered: 1
  last_respawn_at:  2m13s ago
```

## 4. 代码改动一览

| 文件 | 改动 |
|------|------|
| `internal/agentsession/readpump.go:208-257` | `endPrompt(PromptEndProcessDied)` 加 in-flight 快照 → replayCh |
| `internal/chatsession/pump_events.go:117-127` | `case KindPromptEnded:` 在 `SetExited` 之前把 in-flight 推回 queue |
| `internal/agentsession/session.go:761-768` | `SetExited` 加 `persist` 调用;新增 `SetSuspect` / `ClearSuspect` |
| `internal/channel/feishu/adapter.go:3165-3192` | 抽出 `downloadWithRetry`(3 次 ladder:0/5s/15s);AllFailed 走 background ctx + text-only publish,不再 return |
| `internal/chatsession/watchdog.go`(新) | FastAck / HungPrompt timer + probe ladder |
| `internal/chatsession/prober.go`(新) | blanket prober,镜像 F-41 |
| `cmd/nightme/doctor.go` | `PROBER` 字段加 `agent liveness` 行 |
| `internal/agentsession/session.go` | `AgentSession` struct 加 `SuspectReason string` / `SuspectSince time.Time` 字段 |

## 5. 用户可见行为(契约)

| 场景 | 用户看到 |
|------|----------|
| bridge 死了,watchdog respawn + 重投成功 | 略慢一点,正常收到 claude 回答 |
| bridge 死了,respawn 失败 N 次 | `❌ Agent crashed 3 times; last attempt failed. Send /restart or /new to retry.` |
| 附件 AllFailed | `⚠️ 1 attachment(s) failed to download; text-only message accepted.` 然后 bot 正常处理文字 |
| HungPrompt 5min 无响应 | reaction 🔄 → ⏳ → 🔄 ... 直到恢复或告知;不刷屏 |
| Watchdog + prober 都没救回 | `❌ Agent unreachable for 5m; check local claude/pi installation. Last message ID: <msg_id>.` |

**所有告知文案统一从 `internal/channel/feishu/downmsgs.go`(新增)导出**,跟 reaction 风格保持一致。

## 6. 可观测性

- **slog**:`agentsession: persist after SetExited failed` 等 WARN 行
- **doctor**:新增 `PROBER (F-61 agent liveness)` 块
- **metrics**(无,留给后续 PR):本次不引入 metrics 包,只暴露 snapshot

## 7. 测试策略

### 7.1 单元测试
- `endPrompt` 触发快照:注入 fake event 触发 channel close,断言 replayCh 收到
- `SetExited` 触发 persist:mock persist 函数,断言被调用 + error 路径不 panic
- `adapter.handleMessage` AllFailed:mock DownloadAttachments 返 AllFailed,断言 SendMessage 用 background ctx + publish 走后续路径

### 7.2 集成测试(`cmd/nightme/e2e_test/`)
- 场景 A:`/close` → image 消息 → bot 正常处理(text-only)
- 场景 B:bridge 进程 SIGKILL → watchdog 触发 respawn → 用户下一条消息得到正常回复,无错误文案
- 场景 C:连续 SIGKILL 5 次 → 第 5 次告知用户
- 场景 D:`nightme list` 在 bridge 死后立即反映新 pid(老 pid 消失,新 pid 出现)

### 7.3 回归
- `F-53` message-prompt-lifecycle 测试集(已有)— 重点关注 `as_status_exited` SKIP reason 仍正确
- `F-44` receipt 架构相关测试 — reaction 与 receipt 解耦不被打破

## 8. 迁移 / Rollout

- **磁盘 schema**:`AgentSession` 新增 `SuspectReason` / `SuspectSince` 字段(可选)。`agent_sessions.json` 缺失时按零值处理(`FromAgentSessionEntry` 已有兼容路径)
- **daemon 重启**:无新依赖;首次启动会自动建好 watchdog + prober
- **回滚**:本次改动涉及 3 个生产路径(readpump / adapter / persist),但每个改动都有测试覆盖;回滚 commit 即可
- **不发版公告**(项目目前无 release notes 流程)— 走 CHANGELOG.md `Unreleased` 段

## 9. 后续(Out of Scope)

- metrics 暴露(respawn count / HungPrompt rate)
- per-chat 用户可配置 HungPrompt timeout
- bridge 端 health endpoint(claude/pi 都还没)
- 跨 daemon 的 AS 共享(目前 daemon 是单进程,先不预判多副本)

## 10. 关联文档

- [`F-53-message-prompt-lifecycle`](./F-53-message-prompt-lifecycle.md) — PromptEndProcessDied 的语义源头
- [`F-runtime`](./F-runtime.md) — ChatSession / AgentSession 边界
- [`F-chat-session`](./F-chat-session.md) — InputBuffer FSM / TryFlush
- [`../channel/feishu.md`](../channel/feishu.md) — adapter.handleMessage 入口
- [`../channel/feishu-reliability.md`](../channel/feishu-reliability.md) — WithTransientRetry 行为
- §1.1 incident 还原 — 本文件 §1.1 段