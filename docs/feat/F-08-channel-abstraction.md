# F-08: Channel Abstraction

> **Status**: designed (v0.1)
> **Milestone**: M2 (Feishu implementation)
> **Depends on**: (none — interface)
> **Related docs**: SPEC.md §1.1 (Channel Adapter 组件), §4 (并发模型)

## 1. Description

定义 `Channel` interface 让 nightme 支持多种 IM backend。MVP 仅实现飞书（lark-oapi-go），但 interface 设计保证后续接入 WhatsApp / Telegram / Slack / Web UI 无需改动核心逻辑。

## 2. Interface

```go
// internal/channel/channel.go
package channel

import (
    "context"
    "time"
)

type Message struct {
    ChatID   string    // IM 端唯一标识（飞书 open_chat_id）
    Text     string    // 已 strip 富文本后的纯文本
    SenderID string    // 用户 open_id（v0.2 用于多用户扩展）
    Time     time.Time
    // Raw     json.RawMessage  // 原始消息（v0.2 用于支持附件）
}

type Channel interface {
    // Start 启动长连接（飞书 WebSocket / WhatsApp webhook）
    // 收到消息推送到 Incoming channel
    Start(ctx context.Context) error

    // Stop 优雅停止（关闭长连接 + drain 队列）
    Stop(ctx context.Context) error

    // SendMessage 发送文本消息到指定 chat（<= 4KB）
    SendMessage(ctx context.Context, chatID string, text string) error

    // SendLongMessage 自动分段（>4KB 按 \n 或 ANSI 安全点切分）
    SendLongMessage(ctx context.Context, chatID string, text string) error

    // Incoming 用户消息通道（Channel adapter → Router）
    Incoming() <-chan Message
}

// Compile-time check
var _ Channel = (*feishu.Adapter)(nil)
```

## 3. Implementation

**文件**：
- `internal/channel/channel.go` — interface 定义 + Message struct
- `internal/channel/feishu/feishu.go` — 飞书 adapter 实现
- `internal/channel/mock/mock.go` — 测试用 mock

**架构**：
```
┌──────────────────────────────────────────┐
│ Channel interface (channel.go)           │
│   - Start(ctx) error                     │
│   - Stop(ctx) error                      │
│   - SendMessage / SendLongMessage        │
│   - Incoming() <-chan Message            │
└──────────────────────────────────────────┘
        ↑ implements
        │
┌──────────────────────────────────────────┐
│ feishu.Adapter (feishu.go)               │
│   - lark.NewClient(appID, appSecret)     │
│   - 长连接：larkws.NewClient              │
│   - Incoming: chan Message (buffered 128)│
│   - sendLoop: rate-limited + retry       │
└──────────────────────────────────────────┘
```

**SendLongMessage 切分策略**（`internal/channel/feishu/segment.go`）：
- 按 `\n` 切分（不在 ANSI escape sequence 中间断）
- 每段 ≤ 3.8KB（留余量）
- 段落间不插入分隔符，飞书会自动连续显示
- 如果单行 > 3.8KB（极少见），按字节硬切（接受 ANSI 截断风险）

## 4. Edge cases

| 场景 | 处理 |
|------|------|
| 飞书 WebSocket 断连 | lark-oapi SDK 自动重连（指数退避）；期间 PTY 输出 buffer，丢弃 |
| 飞书 QPS 限流（单聊 5 QPS）| 内部 token bucket；超限排队 |
| Channel.Incoming channel 满 | sendLoop 丢弃最早消息 + warn |
| 用户发图片 / 文件（v0.1 不支持）| Channel adapter 提取纯文本，丢弃附件；warn 日志 |
| 用户撤回消息 | v0.1 忽略（已转发给 PTY 无法撤回）|
| 飞书 appSecret 错误 | Start() 返回 error，nightme 启动失败 |
| 飞书权限被回收 | WebSocket 收到权限错误事件 → SendMessage 返回 error → 日志告警 |

## 5. Test plan

**单元测试**：
- mock Channel 实现满足 interface（compile-time check）
- SendLongMessage 切分逻辑：4KB 文本 → 2 段；500B 文本 → 1 段
- ANSI 安全切分：在 escape sequence 中间不切

**集成测试**：
- mock Channel → SessionManager → 验证消息路由正确
- 并发 SendMessage 100 个 → 验证顺序（FIFO）

**E2E（M2）**：
- 真飞书 app + nightme run → 飞书发消息 → claude 收到

## 6. Open questions

- v0.2 多 Channel mirror（F-11）时，Incoming 是合并的 chan 还是每个 Channel 独立？倾向：每个 Channel 独立 chan，由 Session 内部 fan-out
- 是否需要 Channel 抽象到 Process level（每个 Chat 一个 goroutine）？v0.1 不需要，飞书 SDK 已内部处理
- 是否支持群聊（group chat）？v0.1 仅 DM，群聊留 v0.2
- SenderID 在 v0.1 用不到，是否还要记录？倾向：是，v0.2 多用户鉴权需要
