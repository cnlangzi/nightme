# F-03: Output Push (AgentSession Events → Channel)

> **Status**: designed (v0.1)
> **Milestone**: M2
> **Depends on**: F-01 (Session), F-08 (Channel), F-20 (Gateway), F-21 (Agent Modes)
> **Related docs**: [`F-19-cli-bridge.md`](./F-19-cli-bridge.md) §2.2, §3, [`F-21-agent-modes.md`](./F-21-agent-modes.md), SPEC.md §4 (并发模型 + back-pressure)

## 1. Description

Session 的 `AgentSession.Events()` 事件流 → 聚合（PTY 模式 200ms / 4KB）→ 该 Chat。

**注意**：源是 `AgentSession.Events()`（不是 PTY 字节流）。AgentSession 内部产生的事件类型：
- PTY 模式：只有 `TextEvent`（字节流转文本）
- ACP 模式：`TextEvent` / `PermissionRequest` / `ToolStartEvent` / `ToolEndEvent`
- SDK 模式：同 ACP，可能更多 vendor-specific 事件

Channel adapter 决定怎么渲染（v0.1：Text → 普通消息，Permission → 飞书 card，ToolStart/End → emoji 消息）。

## 2. Interface

```go
// internal/pty/aggregator.go
type Aggregator struct {
    buf       []byte
    maxSize   int           // default 4096
    maxWait   time.Duration // default 200ms
    flushFn   func([]byte)  // callback (e.g. Channel.SendLongMessage)
    timer     *time.Timer
    mu        sync.Mutex
}

func NewAggregator(maxSize int, maxWait time.Duration, flushFn func([]byte)) *Aggregator
func (a *Aggregator) Write(p []byte)
func (a *Aggregator) Flush()  // 强制 flush
func (a *Aggregator) Close()  // flush + 停 timer
```

**触发 flush 的条件**（任一满足）：
1. buffer ≥ maxSize (4KB)
2. 距离上次 flush ≥ maxWait (200ms)
3. `Flush()` 手动调用
4. `Close()` 时

## 3. Implementation

**文件**：
- `internal/pty/aggregator.go` — Aggregator 实现
- `internal/session/session.go` — `readPump()` 集成 Aggregator
- `internal/channel/feishu/feishu.go` — `SendLongMessage()` 自动分段

**流程**：
```
claude stdout/stderr
  ↓
bridge.Read(buf 4KB)  // 阻塞直到有数据或 EOF
  ↓
session.aggregator.Write(buf[:n])
  ├─ buffer < 4KB 且 timer 未启动 → 启动 200ms timer
  ├─ buffer ≥ 4KB → 立即 flush
  └─ timer 到期 → flush
  ↓
flushFn(chunk)  // = Channel.SendLongMessage(chatID, chunk)
  ├─ chunk ≤ 4KB → 单条发送
  └─ chunk > 4KB → 切分（不在 ANSI escape sequence 中间断）
```

**关键决策**：
- Aggregator 输出 callback 是 `Channel.SendLongMessage`，不是 channel.sendQueue（避免桥接）
- 如果 Channel 发送慢（飞书限速），callback 同步等待 → 阻塞 readPump → 阻塞 PTY → **back-pressure 自然生效**（PTY buffer 满后 claude 阻塞）
- 极端情况 PTY buffer 也满 → claude 等于自身 back-pressure，整个链路自然限速

## 4. Edge cases

| 场景 | 处理 |
|------|------|
| chunk > 4KB | 自动切分；切分点选在 `\n` 处（避免破坏 ANSI escape）|
| PTY read 返回 0 字节 + nil error | 跳过，等待下次 read |
| PTY read 返回 EOF | session 退出，发送 "Session ended" 给用户 |
| Aggregator buffer 被多个 goroutine 写入 | mutex 保护 |
| Channel.SendLongMessage 失败 | log + 丢弃当前 chunk（不回退 PTY，避免死锁）|
| 用户在多个 Chat 看同一 session（v0.2 F-11 mirror）| flushFn 广播到多个 chat |
| Claude Code 输出二进制（如图片转义）| 原样发送，飞书会显示乱码但不崩溃 |

## 5. Test plan

**单元测试**：
- `Write(3KB)` 不 flush（200ms 内未到）
- `Write(5KB)` 立即 flush 一次 + buffer 留 1KB
- `Write(100B)`, 200ms 后自动 flush
- 并发 Write 100 个 goroutine 无 race

**集成测试**：
- spawn `yes "hello" | head -100` → Aggregator → mock Channel → 验证消息数量 + 顺序

**E2E（M2）**：
- claude 真实输出 → 飞书 DM 收到（消息数量 < 实际输出行数，因为有聚合）

## 6. Open questions

- 聚合窗口 200ms 是否合理？倾向：根据飞书 UX 测试调（手机推送频率）
- 是否检测 "PTY idle 500ms" 提前 flush？v0.1 不做（200ms 已经够短）
- 是否过滤 `\x1b[8m`（密码隐藏模式）？**不做**——密码走透传，详见 [PRD §4.1](../PRD.md#41-完全透传不解析)
