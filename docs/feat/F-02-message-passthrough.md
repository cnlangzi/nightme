# F-02: Message Passthrough (Channel → PTY)

> **Status**: designed (v0.1)
> **Milestone**: M2
> **Depends on**: F-01 (Session), F-04 (PTY), F-08 (Channel), F-20 (Gateway)
> **Related docs**: [`F-19-cli-bridge.md`](./F-19-cli-bridge.md) §2.1, §4, [`F-20-gateway.md`](./F-20-gateway.md)

## 1. Description

用户在 Chat 里的输入 → 经过 Gateway 路由判断 → 透传到该 Chat 绑定 session 的 PTY stdin。

**Gateway 之前**的职责：决定这条消息是 slash command 还是普通文本。
**本 feature 的职责**：仅处理"普通文本"分支——把消息原样写入 PTY stdin。

slash command 分支由 [F-20 Gateway](./F-20-gateway.md) 负责（包括 `/cwd`、`/kill`、`/help` 等）。

## 2. Interface

```go
// Session.WriteText 是 fallback 路径（普通文本走这里）
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
- `internal/gateway/gateway.go` — fallback handler（未命中命令时调 Session.WriteText）

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
  ├─ text 以 "/" 开头 → 走命令路由（[F-20](./F-20-gateway.md)）
  └─ text 不是 "/" 开头 → fallback:
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
| 用户发 slash command（以 `/` 开头）| Gateway 拦截，不透传（[F-20](./F-20-gateway.md)） |
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
- Gateway.Handle(普通文本) → fallback 触发 → bridge.Write 被调

**E2E（M2）**：
- 创建 session（/cwd）后，飞书 DM 发 "hello" → claude 收到 stdin
- 飞书 DM 发 `/help` → bot 回命令列表（不进 PTY）

## 6. Open questions

- 是否需要支持 Ctrl+C / Ctrl+D 等控制字符？倾向：飞书用户极少需要，v0.1 不支持
- session 未创建时的 fallback 行为：当前设计"提示用户"，但这等于 Gateway 在做 SessionManager 的事情。是否应让 SessionManager 报错？
