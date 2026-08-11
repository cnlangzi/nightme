# 消息流转 (Inbound / Outbound)

## A1. F-02: Message Passthrough (Channel → AgentSession)

> **Source**: `F-message-flow.md`


> **Depends on**: F-01 (Session), F-08 (Channel), F-20 (Gateway), F-21 (Agent Modes)
> **Related docs**: [`../bridge/cli-transport.md`](./../bridge/cli-transport.md) §2.1, §4, [`F-gateway.md`](./F-gateway.md), [`../bridge/cli-transport.md`](./../bridge/cli-transport.md)

## 1. Description

用户在 Chat 里的输入 → 经过 Gateway 路由判断 → 透传到该 Chat 绑定 session 的 `AgentSession.SendText()`。

**注意**：发送目标是 `AgentSession`（不是 PTY stdin）。AgentSession 内部决定怎么写：
- PTY 模式：`bridge.Write(text)`
- ACP 模式：ACP `SendPrompt` 消息
- SDK 模式：SDK `Send` 调用

**Gateway 之前**的职责：决定这条消息是 slash command 还是普通文本。
**本 feature 的职责**：仅处理"普通文本"分支——把消息原样写入 PTY stdin。

slash command 分支由 [F-20 Gateway](./F-gateway.md) 负责（包括 `/cwd`、`/close`、`/help` 等）。

## 2. Interface

```go
// Session.WriteText 是 messageDispatcher 主路径（普通文本走这里）
func (s *Session) WriteText(text string) error {
    normalized := normalizeInput(text)
    _, err := s.bridge.Write([]byte(normalized))
    return err
}

// Normalize: \r -> \n, ensure trailing \n
func normalizeInput(text string) string {
    text = strings.ReplaceAll(text, "\r\n", "\n")
    text = strings.ReplaceAll(text, "\r", "\n")
    if !strings.HasSuffix(text, "\n") {
        text += "\n"
    }
    return text
}
```

## 3. Implementation

**文件**：
- `internal/session/session.go` — `Session.WriteText()` 方法
- `internal/channel/feishu/feishu.go` — Incoming handler（不含命令解析）
- `internal/gateway/gateway.go` — messageDispatcher（未命中 slash command 时调 ChatSession.QueueUserMessage）

**完整流程**：
```
飞书 WebSocket 收到消息事件
  ↓
feishuAdapter.handleEvent()
  → Channel.Message{ChatID, Text}
  ↓
Router.Lookup(chatID) → *Session
  ↓
session.Gateway.Handle(msg)
  ├─ text 以 "/" 开头 → 走命令路由（[F-20](./F-gateway.md)）
  └─ text 不是 "/" 开头 → messageDispatcher:
      session.WriteText(text) → normalizeInput → bridge.Write
                                          ↓
                                    PTY master → PTY slave → claude stdin
```

**异步模型**（不变）：
- Channel 的 `Incoming()` 是 buffered channel (size=128)
- 每个 session 一个 `writePump` goroutine
- Gateway.Handle 在 Channel handler goroutine 中调用

## 4. Edge cases

| 场景 | 处理 |
|------|------|
| 用户发 slash command（以 `/` 开头）| Gateway 拦截，不透传（[F-20](./F-gateway.md)） |
| 多行粘贴 | 原样转发，PTY 自行处理 |
| 用户发空消息 | 丢弃，不写入 PTY |
| PTY 已关闭 | bridge.Write 返回 error → session 进入 exited → 提示用户 |
| 用户狂发消息（>10 QPS）| Channel adapter 内部 rate limit（飞书侧）|
| 含 ANSI 转义码的用户消息 | 原样转发（用户极少这么用）|
| 长消息（>4KB）| 飞书侧已限制发送大小 |
| session 未创建时用户发普通文本 | Gateway 提示 "no active session, send /cwd first" |

## 5. Test plan

**单元测试**：
- `normalizeInput("hello")` → `"hello\n"`
- `normalizeInput("hello\r\nworld")` → `"hello\nworld\n"`
- `normalizeInput("hello\n")` → `"hello\n"`

**集成测试**：
- Gateway.DispatchInbound(普通文本) → messageDispatcher 触发 → bridge.Write 被调

**E2E（M2）**：
- 创建 session（/cwd）后，飞书 DM 发 "hello" → claude 收到 stdin
- 飞书 DM 发 `/help` → bot 回命令列表（不进 PTY）

## 6. Open questions

- 是否需要支持 Ctrl+C / Ctrl+D 等控制字符？倾向：飞书用户极少需要，不支持
- session 未创建时的 messageDispatcher 行为：当前设计"提示用户"，但这等于 Gateway 在做 SessionManager 的事情。是否应让 SessionManager 报错？

---

## A2. F-03: Output Push (AgentSession Events → Channel)

> **Source**: `F-message-flow.md`


> **Depends on**: F-01 (Session), F-08 (Channel), F-20 (Gateway), F-21 (Agent Modes)
> **Related docs**: [`../bridge/cli-transport.md`](./../bridge/cli-transport.md) §2.2, §3, [`../bridge/cli-transport.md`](./../bridge/cli-transport.md), SPEC.md §4 (并发模型 + back-pressure)

## 1. Description

Session 的 `AgentSession.Events()` 事件流 → 聚合（PTY 模式 200ms / 4KB）→ 该 Chat。

**注意**：源是 `AgentSession.Events()`（不是 PTY 字节流）。AgentSession 内部产生的事件类型：
- PTY 模式：只有 `TextEvent`（字节流转文本）
- ACP 模式：`TextEvent` / `PermissionRequest` / `ToolStartEvent` / `ToolEndEvent`
- SDK 模式：同 ACP，可能更多 vendor-specific 事件

Channel adapter 决定怎么渲染。

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
| 用户在多个 Chat 看同一 session| flushFn 广播到多个 chat |
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
- 是否检测 "PTY idle 500ms" 提前 flush？不做（200ms 已经够短）
- 是否过滤 `\x1b[8m`（密码隐藏模式）？**不做**——密码走透传，详见 [PRD §4.1](../PRD.md#41-完全透传不解析)

---

