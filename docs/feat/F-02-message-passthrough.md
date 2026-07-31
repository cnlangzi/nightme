# F-02: Message Passthrough (Channel → PTY)

> **Status**: designed (v0.1)
> **Milestone**: M2
> **Depends on**: F-01 (Session), F-04 (PTY), F-08 (Channel)
> **Related docs**: [`F-19-cli-bridge.md`](./F-19-cli-bridge.md) §2.1, §4

## 1. Description

用户在 Chat 里的输入（IM 文本消息）透明转发到该 Chat 绑定 session 的 PTY stdin。nightme 不解析内容、不拆分、不重写，只做 byte pipe。

## 2. Interface

```go
// Channel adapter receives Message, hands to Router
func (s *Session) HandleInput(text string) error {
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
- `internal/session/session.go` — `Session.HandleInput()`
- `internal/channel/feishu/feishu.go` — `Incoming()` handler 路由到 session

**流程**：
```
飞书 WebSocket 收到消息事件
  ↓
feishuAdapter.handleEvent()
  → Channel.Message{ChatID, Text, SenderID, Time}
  ↓
Router.Lookup(chatID) → *Session
  ↓
session.HandleInput(text)
  → normalizeInput → bridge.Write
  ↓
PTY master fd → PTY slave → claude stdin
```

**异步模型**：
- Channel 的 `Incoming()` 是 buffered channel (size=128)
- 每个 session 一个 `writePump` goroutine，从 session 的 `inputStream` chan 读 → bridge.Write
- Channel handler 只负责 dispatch 到 session.inputStream，不阻塞

## 4. Edge cases

| 场景 | 处理 |
|------|------|
| 飞书富文本（@、emoji） | @ 前缀丢弃保留正文；emoji UTF-8 字节透传 |
| 多行粘贴 | 原样转发，PTY 自行处理（Claude Code 支持多行输入）|
| 用户发空消息 | 丢弃，不写入 PTY |
| PTY 已关闭 | bridge.Write 返回 error → session 进入 exited → 提示用户 "session ended" |
| 用户狂发消息（>10 QPS）| Channel adapter 内部 rate limit（飞书侧）|
| 含 ANSI 转义码的用户消息 | 原样转发（用户极少这么用）|
| 长消息（>4KB）| 飞书侧已限制发送大小，无需 nightme 处理 |

## 5. Test plan

**单元测试**：
- `normalizeInput("hello")` → `"hello\n"`
- `normalizeInput("hello\r\nworld")` → `"hello\nworld\n"`
- `normalizeInput("hello\n")` → `"hello\n"`（不去重已有 \n）

**集成测试**：
- mock channel → mock agent（cat）→ 输入 "hello" → 验证 cat 输出 "hello"

**E2E（M2）**：
- 飞书 DM 发消息 → claude 进程 stdin 收到（`strace -e trace=read` 或类似工具验证）

## 6. Open questions

- 是否需要支持 Ctrl+C / Ctrl+D 等控制字符？倾向：飞书用户极少需要，v0.1 不支持
- 是否需要支持 `/` 命令（如 `/kill`、`/clear`）？倾向：v0.1 不支持，v0.2 加 command router
