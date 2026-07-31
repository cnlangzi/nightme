# nightme — CLI Bridge Protocol

> **状态**：v1.0
> **作者**：🦞 虾哥（PM/Architect）
> **日期**：2026-07-31
> **依赖**：`PRD.md`、`architecture.md`

本文档回答：**nightme 怎么把 Claude Code 的 TTY 字节流搬到飞书、再把飞书的字节流搬回 TTY**。

---

## 1. 核心约束

nightme 是 **byte pipe**，不是 **translator**：

| 方向 | 模式 |
|------|------|
| Channel → PTY stdin | **纯字节透传**。用户发什么，PTY 收什么（除 \r\n → \n 标准化） |
| PTY stdout/stderr → Channel | **保留 ANSI 转义码**，Channel adapter 决定怎么渲染（v0.1 飞书：保留为 code block 或纯文本） |

**绝对不做**：
- 不解析 CLI 输出识别 "success" / "failure"
- 不识别 prompt / 等待输入状态
- 不替换 emoji / 颜色 / 进度条
- 不主动补全 / 联想

如果用户想看效果，他应该在 macOS 上手动跑 Claude Code，看终端长什么样；飞书上能保留 80% 视觉一致性就够了。

---

## 2. PTY ↔ Channel 协议

### 2.1 Channel → PTY

```
┌─────────────────────────────────────────────┐
│ 飞书消息: "请帮我修复 login bug"            │
│     │                                       │
│     │ Channel adapter 提取 text              │
│     ▼                                       │
│ raw = "请帮我修复 login bug"                │
│     │                                       │
│     │ 标准化：去除 \r，统一 \n              │
│     ▼                                       │
│ normalized = "请帮我修复 login bug\n"        │
│     │                                       │
│     │ Bridge.Write(normalized)               │
│     ▼                                       │
│ PTY master fd → PTY slave → claude stdin   │
└─────────────────────────────────────────────┘
```

**关键点**：
- 飞书用户**手动**按回车发消息，所以用户消息自带 `\n` 或 `\r\n`
- 如果用户发的是单行文本（不带 `\n`），nightme 自动补 `\n`（避免 Claude Code 等不到 Enter）
- 飞书富文本（如 @、图片）v0.1 直接丢弃图片，只保留纯文本

### 2.2 PTY → Channel

```
┌─────────────────────────────────────────────┐
│ claude stdout:                              │
│   "\x1b[32m✓\x1b[0m Updated file login.ts\n"│
│   "Checking tests...\n"                      │
│   "\x1b[?25l"  ← 隐藏光标                   │
│     │                                       │
│     │ PTY master fd Read(buf 4KB)            │
│     ▼                                       │
│ raw bytes: b"\x1b[32m✓\x1b[0m Updated..."  │
│     │                                       │
│     │ 缓冲 + 聚合窗口（200ms 或 4KB）         │
│     ▼                                       │
│ chunk = "\x1b[32m✓\x1b[0m Updated file..." │
│     │                                       │
│     │ Channel adapter.SendLongMessage()      │
│     │ （>4KB 自动分段）                       │
│     ▼                                       │
│ 飞书消息 1 (4KB)                             │
│ 飞书消息 2 (剩余)                             │
└─────────────────────────────────────────────┘
```

**关键点**：
- 飞书单条消息上限 ~4KB（markdown text 实际 4096 字符）
- `SendLongMessage` 自动切分，保留 ANSI 码完整性（不在 escape sequence 中间断开）
- 200ms 聚合窗口：减少消息条数，避免飞书"刷屏"触犯 QPS 限制

---

## 3. 缓冲与聚合

### 3.1 为什么需要聚合？

Claude Code 一次操作可能产生 50+ 个 stdout write（如进度更新），如果不聚合：
- 飞书群发：~50 条消息
- 飞书 QPS：单聊 5 QPS，超限后消息会被吞
- 用户体验：手机通知刷屏

### 3.2 聚合策略

```go
type Aggregator struct {
    buf       []byte
    maxSize   int           // 4KB
    maxWait   time.Duration // 200ms
    flushCh   chan []byte
    timer     *time.Timer
    mu        sync.Mutex
}

func (a *Aggregator) Write(p []byte) {
    a.mu.Lock()
    a.buf = append(a.buf, p...)
    if len(a.buf) >= a.maxSize {
        a.flushLocked()
    } else if a.timer == nil {
        a.timer = time.AfterFunc(a.maxWait, a.flushFromTimer)
    }
    a.mu.Unlock()
}
```

**触发 flush 的条件**（任一满足）：
1. buffer ≥ 4KB（飞书单条上限）
2. 距离上次 flush 满 200ms
3. 检测到 PTY 空闲（无新数据 500ms）— v0.2 加
4. 检测到 prompt / 等待输入模式 — v0.2 不做（nightme 不识别 prompt）

### 3.3 ANSI 处理

**v0.1 策略**：保留 ANSI 转义码，发到飞书。
- 飞书 markdown 不渲染 ANSI（会显示成乱码 `^[[32m`）
- 但飞书**支持** `<text color="green">` 等富文本标签（card 模式）
- v0.1 选择最简单：**保留为原始字符串**，用户看到 ANSI 字面量

**v0.2 优化**：
- 检测 ANSI 颜色码，转换成飞书富文本标签
- 或：把 ANSI 编码后的内容用图片渲染（puppeteer 截图）→ 发图片
- v0.2 再讨论

**v0.1 接受 ANSI 显示乱码**的理由：
- 用户多数情况下看 Claude Code 是文字内容，ANSI 只是装饰
- 飞书支持"代码块"语法（```\`\`\` ```），可以把整段输出塞代码块，可读性还行
- 简单优于完美

---

## 4. 特殊字符 / 控制序列

### 4.1 必须正确处理的

| 序列 | 含义 | 处理 |
|------|------|------|
| `\n` / `\r\n` | 换行 | 透传 |
| `\r` | 回车 | 转 `\n`（飞书用 `\n` 换行） |
| `\x1b[2J` | 清屏 | 透传（飞书会显示为空白） |
| `\x1b[H` | 光标归零 | 透传 |
| `\x1b[?25l/h` | 隐藏/显示光标 | 透传（v0.1 不影响） |
| `\x1b[8m` / `\x1b[28m` | 隐藏/显示密码 | **过滤**（避免密码通过飞书泄露） |

### 4.2 输入方向（Channel → PTY）

| 输入 | 处理 |
|------|------|
| 普通文本 | 透传 + 自动补 `\n`（如果用户没发） |
| `\r` 或 `\r\n` | 转 `\n` |
| 粘贴多行 | 飞书粘贴板原样转发，PTY 自行处理 |
| 飞书 @机器人 | 丢弃 @ 前缀，保留正文 |
| 飞书 emoji | 转 UTF-8 字节透传（PTY 一般能显示） |
| 图片 / 文件 | v0.1 丢弃，提示 "v0.1 not supported" |

### 4.3 为什么不做 "认证模式" 过滤？

Claude Code / OpenCode 可能会让用户输入密码 / API key。如果用户直接在飞书发密码，密码**会出现在飞书聊天记录**——这是个安全问题。

**v0.1 缓解**：
- README 警告用户："不要在飞书里发密码 / API key"
- 默认在 daemon 日志里 redact 密码（但飞书记录没法删）

**v0.2 加**：
- 检测 CLI 进入 "hidden input" 模式（`\x1b[8m` 输出 + 等待输入）
- 自动通过飞书 card 弹出密码输入框（飞书 input 组件）
- 用户输入走加密通道，**不**进入飞书聊天记录

---

## 5. 错误恢复

### 5.1 PTY 异常关闭

```
Claude Code crash → PTY EOF → bridge.Read() returns io.EOF
  ↓
Session.readPump 退出
  ↓
SessionManager.MarkExited(session_id)
  ↓
Channel.SendMessage(chat_id, "Session ended (exit code: {code})")
  ↓
registry.Delete(session_id)
```

### 5.2 Channel 断连（飞书）

飞书 WebSocket 断连 → lark-oapi SDK 自动重连（指数退避 1s → 2s → 4s → ... → 60s）。
期间 PTY 输出会**丢失**（nightme 不知道发给谁）。

**v0.1 行为**：
- 断连期间 PTY 输出写入 "buffered" channel（无接收者即丢弃）
- 重连后 next message 处理正常
- **不补偿历史消息**（避免飞书刷屏）

**v0.2 改进**：
- 断连时 PTY 输出暂存到 session 内存 buffer
- 重连后发 "while you were offline: ..." 摘要

### 5.3 nightme 重启

```
nightme SIGTERM
  ↓
1. Channel.Stop() — 优雅停 WebSocket
2. SessionManager 遍历所有 session:
     - 不 kill PTY 子进程（默认策略）
     - 标记 session 为 "detached"
3. registry 持久化（包括 detached 状态）
4. 主进程退出

下次 nightme 启动:
  ↓
1. 加载 registry
2. 对每个 detached session:
     - 检查 PID 是否还活着
     - 活着 → 自动 reattach（重建 readPump/writePump）
     - 死了 → 删除 registry 记录
```

---

## 6. 性能预算

| 指标 | 预算 | 实测 |
|------|------|------|
| Channel → PTY 延迟 | < 50ms（P50）/ < 200ms（P99）| TBD |
| PTY → Channel 延迟 | < 500ms（聚合窗口上限）| TBD |
| PTY 读取吞吐 | 1MB/s 单 session | TBD |
| 并发 session 数 | 50（单 laptop 上限）| TBD |
| nightme 内存占用 | < 50MB（10 sessions idle）| TBD |
| nightme 启动时间 | < 2s | TBD |

**性能测试方法**（v0.1 完成时跑）：
- 启动 10 个 session，每个跑 `cat /dev/urandom` 灌数据
- 持续 5 分钟，观察 CPU / 内存 / 飞书 QPS

---

## 7. 实现注意（给开发者）

### 7.1 goroutine 生命周期

```go
// 每个 Session 两个 goroutine
func (s *Session) readPump(ctx context.Context) {
    defer s.cleanup()
    buf := make([]byte, 4096)
    for {
        n, err := s.bridge.Read(buf)
        if err != nil { return }  // PTY closed
        // 发送到 outputStream（buffered channel）
        select {
        case s.outputStream <- buf[:n]:
        case <-ctx.Done():
            return
        }
    }
}
```

**关键点**：
- `outputStream` 是 buffered channel，buffer = 16
- 如果 Channel adapter 处理慢（飞书 QPS 限），outputStream 满了怎么办？
  - v0.1：**丢弃**（PTY 不阻塞）
  - v0.2：**记录 metric + 断开 session**

### 7.2 Channel adapter 的发送去抖

```go
type feishuAdapter struct {
    sendQueue chan sendReq  // buffered 100
    workers   int           // 默认 2
}

func (a *feishuAdapter) sendLoop() {
    for req := range a.sendQueue {
        // 飞书 API 限速：单聊 5 QPS
        // 用 token bucket 控制
        if err := a.rateLimit.Wait(ctx); err != nil { continue }
        a.client.SendMessage(req.chatID, req.text)
    }
}
```

### 7.3 不在 Channel adapter 里做 ANSI 解析

ANSI 解析是**沉重**的活（状态机、各种 escape 变体）。v0.1 在 PTY bridge 直通字节流，ANSI 字符串原样塞飞书消息。

如果用户抱怨"飞书显示乱码"，v0.2 的方案是：
- **方案 A**：用 `mattn/go-runewidth` + `aymanbagabas/go-pty` 提供的 `render` 函数（如果支持）
- **方案 B**：用 `puppeteer/playwright` 渲染 ANSI 到 PNG，发图片
- **方案 C**：写一个最小 ANSI parser（~500 行 Go），提取颜色 + 文本，转飞书富文本

v0.1 不做决策，等用户反馈。

---

## 8. 测试场景

### 8.1 单元测试

```go
// internal/pty/bridge_test.go
func TestBridge_BasicEcho(t *testing.T) {
    b, _ := pty.New(t.TempDir(), "/bin/echo", []string{"hello"})
    defer b.Close()

    buf := make([]byte, 1024)
    n, _ := b.Read(buf)
    assert.Equal(t, "hello\n", string(buf[:n]))
}
```

### 8.2 集成测试：PTY → Aggregator → 飞书 mock

```go
func TestAggregator_FlushOnSize(t *testing.T) {
    mock := &mockChannel{}
    agg := NewAggregator(mock, 100, 0) // 100B 上限，0 等待

    agg.Write(bytes.Repeat([]byte("a"), 150))
    assert.Equal(t, 1, mock.callCount)  // 100B 一次 flush
    assert.Equal(t, 100, len(mock.lastMsg))
}
```

### 8.3 E2E（手动）

1. 启动 nightme，加载飞书 app config
2. 飞书 DM 机器人：`workspace: /tmp/test`
3. 验证：
   - 收到 "Session started in /tmp/test"
   - `/tmp/test` 下有 claude 进程在跑（`ps aux | grep claude`）
   - 在 DM 发 "hello"
   - 飞书收到 claude 输出
4. kill nightme，验证 claude 进程**仍在跑**（默认策略）
5. 重启 nightme，验证自动 reattach

---

## 9. 下一步

1. ✅ 本 cli-bridge.md 完成
2. ⏭ 出 **Implementation Brief**（milestone + 第一个 PR commit 计划）
3. ⏭ 动代码：`go mod init` + 目录骨架 + 第一段能跑的代码（local PTY echo）
