# F-23: Heartbeat & Streaming Status

> **Status**: implemented (v1.1 — heartbeat ticker 由 Channel 实现持有；InputBuffer / session 不再管 receipt 心跳)
> **Milestone**: v0.2 (设计), v0.3 (实现迁到 Channel)
> **Depends on**: F-03 (output push), F-08 (channel abstraction, receipt API), F-22 (feishu adapter)
> **Supersedes**: F-17 (v0.2 stub — heartbeat 设计收敛后并入本文)
> **Related**: [F-08-channel-abstraction.md](./F-08-channel-abstraction.md) §4 (receipt rendering); [F-25-rolling-log.md](./F-25-rolling-log.md) §6 (v1.1 修订)

## 0. v1.1 修订（heartbeat 归属变更）

**v0.2 设计**：InputBuffer 持有 receipts map，heartbeat 由 InputBuffer 内的 receipt.Heartbeat() 驱动（每收到一个 agent event 调一次）。

**v1.1 修订**：receipt 不属于 InputBuffer（v1.1 InputBuffer 不知道 receipt 存在——见 [F-25 §5](./F-25-rolling-log.md)）。heartbeat 现在由 **Channel 实现的 receipt 内部**驱动：

- Feishu adapter 的 `MessageReceipt` 持有独立 goroutine，按 heartbeat 节奏调 `bot.UpdateMessage(replyMsgID, "🔄 ⏳ N · HH:MM:SS")`
- ticker 只在 `currentEmoji == EmojiExecuting` 时跑；切到 Done/Error 时停
- 不需要 InputBuffer / session 配合；InputBuffer 完全不知道 heartbeat 存在

**对调用方的影响**：无。v0.2 调用方（InputBuffer）在 v1.1 不存在；Channel 实现自己启动/停止 ticker。

---

## 1. Description

Claude Code / 任何 AI agent 在长任务中可能数分钟无输出（LLM thinking、tool 长执行、子 agent 嵌套调用）。用户在飞书侧**必须能区分**：

- **进程还活着 + 在工作** → 等
- **进程已死 / 网络断** → 重连或重启

本 feature 设计一套**确定性的、不模糊的**心跳机制：tick 来源是 event（不是时间），DEAD 检测来自进程状态（不是时间阈值），kill 由用户主权触发（系统不自动）。

## 2. 核心原则

### 2.1 诚实

**不假装**知道 Claude 在做什么。"思考中" / "Bash 执行中" 这种文案是**推测**，nightme 看不到 LLM 内部状态，只能看到 stream-json event。

**正确做法**：文案永远是"⏳ N · HH:MM:SS"（客观事实），不假装具体动作。

### 2.2 用户主权

**进程还活着 = 没问题**。即使 stdout 长时间没流量，只要进程没退出、stdout pipe 没关闭，nightme **不报错**，不自动 kill。

**用户自己决定**是否 /kill。系统不替用户判断"超时"。

### 2.3 视觉变化 > 文案准确

用户能看到"还在响应" = **视觉上持续有变化**。单调递增的数字 + 触发时间是最佳方案：

```
⏳ 1 · 14:32:05
⏳ 2 · 14:32:10    ← 数字 +1，时间前进，用户直观看到在动
```

### 2.4 进程级 truth

DEAD 的**唯二**定义：

1. **进程退出**（`signal 0` 返回 `ESRCH`，或 `exit code != 0`）
2. **stdout pipe EOF**（Claude Code 异常退出但 OS 还没回收，或 stream pipe 被关闭）

**没有第三条**。时间阈值、心跳超时、飞书推送失败**都不是** DEAD 触发条件。

## 3. 心跳格式

### 3.1 活跃状态

```
⏳ N · HH:MM:SS
```

- `N` = 累计收到的 stream-json event 数（从 session 开始计）
- `HH:MM:SS` = 最近一个 event 的时间戳

### 3.2 空闲状态

当 N 不再增长（即 N 秒内没新 event），追加 idle 时长：

```
⏳ N · HH:MM:SS · idle Xs/Xm
```

Xm < 1 时显示 `Xs`，>= 1 时显示 `Xm`。

### 3.3 飞书 markdown 渲染

```markdown
**⏳ 47** · `14:32:05` · _idle 5m_
```

- 序号加粗（视觉锚点）
- 时间用 inline code（醒目）
- idle 用斜体（弱化，让用户知道"非主要信息"）

### 3.4 完整示例

```
T=0s    🤖 开始处理...
T=5s    ⏳ 1 · 14:32:05
T=10s   ⏳ 2 · 14:32:10
T=15s   ⏳ 3 · 14:32:15
T=20s   ⏳ 4 · 14:32:20
T=25s   ⏳ 5 · 14:32:25
T=30s   ⏳ 5 · 14:32:25 · idle 5s     ← 数字停了，5s 没新 event
T=60s   ⏳ 5 · 14:32:25 · idle 35s
T=120s  ⏳ 5 · 14:32:25 · idle 1m55s  ← 用户该警惕
T=300s  ⏳ 5 · 14:32:25 · idle 4m35s  ← 用户决定是否 /kill
```

## 4. Tick 机制（event-driven）

### 4.1 设计

```go
// internal/heartbeat/heartbeat.go

type Heartbeat struct {
    emoji     string          // "⏳"
    tickCount atomic.Int64    // 累计 event 数
    lastEvent atomic.Int64    // unix nanos
    cardRef   CardRef         // 飞书 card 引用
    process   ProcessProbe    // 进程探活接口
}

func (h *Heartbeat) OnEvent(ev AgentEvent) {
    now := time.Now().UnixNano()
    h.lastEvent.Store(now)
    n := h.tickCount.Add(1)
    h.updateCard(n, false /* not idle */)
}

func (h *Heartbeat) updateCard(n int64, idle bool) {
    ts := time.Unix(0, h.lastEvent.Load()).Format("15:04:05")
    var text string
    if !idle {
        text = fmt.Sprintf("%s %d · %s", h.emoji, n, ts)
    } else {
        age := time.Since(time.Unix(0, h.lastEvent.Load()))
        text = fmt.Sprintf("%s %d · %s · idle %s", h.emoji, n, ts, formatDuration(age))
    }
    h.cardRef.UpdateNote(text)  // 同一行 update，不 append
}
```

**关键**：

- **`OnEvent` 由 event pump 调用**，每个 stream-json event 都触发 +1
- **不基于时间定时器**——定时器只在 idle 检测时使用（见 §5）
- **Card 同一行 update**，不是 append（避免 card 过长）

### 4.2 不 Tick 的场景

| 场景 | tick 行为 |
|------|----------|
| LLM 持续输出 text chunks | 每个 chunk +1 |
| tool_use 已发，等 tool_result | tool_use +1，tool_result 再 +1 |
| idle（无 event）| 不 +1，显示 idle 时长 |
| Session 已结束 | 停止 tick |

### 4.3 idle 状态切换

```go
func (h *Heartbeat) Watch(ctx context.Context) {
    // 定时检查 idle 状态 + DEAD
    ticker := time.NewTicker(2 * time.Second)
    defer ticker.Stop()
    
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            h.refreshIdleStatus()
            // 顺便跑 DEAD 检测
            if dead, reason := h.checkDead(); dead {
                h.reportDead(reason)
                return
            }
        }
    }
}

func (h *Heartbeat) refreshIdleStatus() {
    n := h.tickCount.Load()
    lastEvent := h.lastEvent.Load()
    
    if lastEvent == 0 {
        return  // 还没收到任何 event
    }
    
    age := time.Since(time.Unix(0, lastEvent))
    if age < 30 * time.Second {
        // < 30s 内不显示 idle（避免太频繁）
        return
    }
    
    h.updateCard(n, true /* idle */)
}
```

## 5. DEAD 检测（无时间阈值）

### 5.1 双路检测

```go
type DeadReason int

const (
    DeadProcessExited DeadReason = iota  // 进程退出
    DeadStdoutEOF                         // stdout pipe 关闭
)

func (h *Heartbeat) checkDead() (bool, DeadReason) {
    // 1. 进程探活
    if err := h.process.Signal(syscall.Signal(0)); err != nil {
        // ESRCH = 进程不在；其他 error 也视为 DEAD
        return true, DeadProcessExited
    }
    
    // 2. stdout pipe EOF
    if h.process.StdoutEOF() {
        return true, DeadStdoutEOF
    }
    
    return false, 0
}
```

### 5.2 文案（确定性，不模糊）

```go
func (h *Heartbeat) reportDead(reason DeadReason) {
    var msg string
    switch reason {
    case DeadProcessExited:
        exitCode := h.process.ExitCode()
        msg = fmt.Sprintf("❌ Claude Code 已退出（exit code: %d）", exitCode)
    case DeadStdoutEOF:
        msg = "❌ Claude Code 输出流已关闭"
    }
    h.cardRef.UpdateNote(msg)
    h.Stop()
}
```

**关键**：
- ❌ 表示真问题
- exit code 暴露（用户可查）
- **不写"可能中断" / "响应超时"**——这种模糊文案禁止出现

### 5.3 ❌ 禁止的 DEAD 触发

| 触发条件 | 为什么不触发 |
|---------|-------------|
| 超过 X 分钟无 event | **禁止**——thinking / 长 tool 是合法的 |
| 飞书 API 调用失败 | 飞书重连就行，不影响 Claude 进程 |
| LLM API rate limit | 进程还活着，Claude 自己会 retry |
| 用户长时间没操作 | 用户可能离开了，进程不该死 |

## 6. /kill 用户主权

### 6.1 原则

**nightme 不自动 kill**。`/kill` 必须由用户**显式触发**：

```yaml
# configs/nightme.example.yaml
heartbeat:
  auto_kill_on_dead: false   # ❌ 永远 false
  auto_kill_on_timeout: false # ❌ 永远 false（即使设计上也禁用）
```

### 6.2 飞书卡片按钮

当数字停止增长 + idle 时长累积时，**主动给用户 /kill 按钮**：

```json
{
  "header": {"title": {"tag": "plain_text", "content": "Claude Code"}},
  "elements": [
    {"tag": "div", "text": {"tag": "lark_md", "content": "[累积 text 内容]"}},
    {"tag": "hr"},
    {"tag": "div", "text": {"tag": "plain_text", "content": "⏳ 47 · 14:32:25 · idle 5m"}},
    {"tag": "actions", "actions": [
      {"tag": "button", "text": {"tag": "plain_text", "content": "/kill 终止会话"}, "type": "danger", "value": "cmd:/kill"}
    ]}
  ]
}
```

**只在 idle > 30s 时显示按钮**（避免误触）。

## 7. 飞书集成

### 7.1 Typing Indicator（可选）

飞书平台支持"正在输入"指示器（通过 reaction 模拟）。**注意**：

- 不重复加同一个 emoji（飞书会显示 +N，但用户看不出变化）
- 只加 1 次，作为 turn 开始标记
- 不基于 tick 频率（避免 QPS）

```go
func (s *Session) startTypingIndicator(userMsgID string) {
    // 只加一次
    s.bot.ReactionAdd(userMsgID, "👀")
}
```

### 7.2 Card Note Update

同一行 update，不 append：

```go
type CardRef interface {
    UpdateNote(text string) error  // 飞书: im.message.update
}
```

**为什么同一行 update**：
- Append 会让 card 越来越长，视觉杂乱
- 同一行 update 让用户聚焦在"⏳ N · HH:MM:SS · idle Xm" 这一行
- 数字 +1 + 时间前进 = 视觉变化

### 7.3 失败处理

Card update 失败 → retry 3 次（指数退避）。最终失败：

- 不报错（避免假错）
- 下一轮 tick 重新尝试
- 用户看到的是"⏳ 47"卡在某数字上，但**不会误以为** DEAD（因为没 ❌）

## 8. 架构

```
internal/
├── heartbeat/
│   ├── heartbeat.go      # Heartbeat struct, OnEvent, Watch
│   ├── process.go        # ProcessProbe interface + impl
│   ├── format.go         # text/idle duration formatting
│   └── heartbeat_test.go
```

```go
// internal/heartbeat/process.go

type ProcessProbe interface {
    Signal(sig os.Signal) error
    StdoutEOF() bool
    ExitCode() int
}

type OSProcessProbe struct {
    cmd     *exec.Cmd
    stdout  io.Reader
    eofFlag atomic.Bool
}

func NewOSProcessProbe(cmd *exec.Cmd, stdout io.Reader) *OSProcessProbe {
    p := &OSProcessProbe{cmd: cmd, stdout: stdout}
    
    // 监听 stdout EOF
    go func() {
        io.Copy(io.Discard, stdout)  // 读到底
        p.eofFlag.Store(true)
    }()
    
    return p
}

func (p *OSProcessProbe) Signal(sig os.Signal) error {
    if p.cmd.Process == nil {
        return fmt.Errorf("process not started")
    }
    return p.cmd.Process.Signal(sig)
}

func (p *OSProcessProbe) StdoutEOF() bool {
    return p.eofFlag.Load()
}

func (p *OSProcessProbe) ExitCode() int {
    if p.cmd.ProcessState == nil {
        return -1
    }
    return p.cmd.ProcessState.ExitCode()
}
```

## 9. 配置

```yaml
# configs/nightme.example.yaml
heartbeat:
  enabled: true
  
  # 显示格式
  emoji: "⏳"
  markdown_format: true   # 用 **⏳ N** · `14:32:05` 富文本
  
  # idle 检测
  idle_check_interval: 2s   # 每 2s 检查一次 idle
  idle_display_threshold: 30s  # 多长时间没 event 开始显示 idle
  
  # DEAD 检测
  process_probe:
    enabled: true
    signal_zero: true       # signal 0 探活
    stdout_eof_check: true  # EOF 探活
    # 无时间阈值！永远不基于时间判断 DEAD
  
  # 用户主权
  user_kill:
    show_button_after_idle: 30s  # idle 30s 后显示 /kill 按钮
    button_label: "/kill 终止会话"
```

## 10. Anti-patterns（反例）

| ❌ 反例 | 为什么错 |
|---------|---------|
| 8 min 没输出 → 报 DEAD + kill | 拍脑袋阈值，误杀风险 |
| "🤔 思考中 (30s)" | 假装知道 Claude 在思考 |
| "🔧 Bash 执行中 (60s)" | 假装知道 Claude 跑什么 tool |
| "⚠️ 可能中断" | 模糊文案，用户不知道真假 |
| 系统自动 kill 真 idle session | 剥夺用户主权 |
| Append 多行 note 让 card 越来越长 | 视觉杂乱，应该同一行 update |
| 加多次相同 emoji（"👀👀👀"）| 飞书不显示多次，浪费 API |
| Card update 失败 → 报 DEAD | 飞书 API 失败 ≠ Claude 死了 |

## 11. Test plan

### 11.1 单元测试

| 测试 | 验证 |
|------|------|
| `TestOnEvent_IncrementsTickCount` | 每个 event +1 |
| `TestOnEvent_UpdatesLastEventTime` | lastEvent 同步更新 |
| `TestRefreshIdleStatus_After30s_ShowsIdle` | 30s 没 event 显示 idle |
| `TestRefreshIdleStatus_Under30s_NoIdle` | 30s 内不显示 idle |
| `TestFormatDuration_Xs_Xm` | `< 1m` 显示 `Xs`，`>= 1m` 显示 `Xm` |
| `TestCheckDead_ProcessExited_ReturnsDeadProcessExited` | 进程退出 → DEAD |
| `TestCheckDead_StdoutEOF_ReturnsDeadStdoutEOF` | pipe 关闭 → DEAD |
| `TestCheckDead_AliveNoOutput_ReturnsAlive` | 进程活 + 无输出 → ALIVE（**关键**）|
| `TestReportDead_ProcessExited_FormatsExitCode` | exit code 正确暴露 |

### 11.2 集成测试

| 测试 | 验证 |
|------|------|
| `TestIntegration_LongRunning_ShowsProgressiveIdle` | mock event → 1s 后等 → 验证 note 显示 "idle 30s+" |
| `TestIntegration_ProcessKilled_ReportsDEAD` | mock 进程被 SIGKILL → 验证 note 变成 "❌ 已退出" |
| `TestIntegration_UserClicksKillButton_TriggersKill` | mock 飞书 callback → 验证 /kill 被路由到 SessionManager |

### 11.3 Manual E2E

| 场景 | 步骤 | 期望 |
|------|------|------|
| LLM thinking 长 | `/run claude` + "think about X" | 飞书看到 ⏳ N · HH:MM:SS，N 持续增长，30s 后显示 idle |
| Bash 长执行 | `/run claude` + "compile my project" | 飞书看到 ⏳ N · HH:MM:SS · idle 5m+，无 DEAD 误报 |
| 进程被 kill | kill -9 claude process | 飞书看到 "❌ Claude Code 已退出" |
| 用户 /kill | 飞书点 /kill 按钮 | Session 终止，卡片显示最终状态 |

## 12. Open questions

- multi-tool 并发场景：同一个 turn 内多个 tool 并发跑，event 计数怎么处理？
  - 当前设计：每个 event 都 +1（包括 tool_use 和 tool_result），数字快速累加
  - 备选：去重（同 tool_use+tool_result 算 1 次），但实现复杂
- 飞书 reaction 数量上限：用户消息反应堆叠多少个会触顶？
  - 当前设计：typing indicator 只加 1 次（"👀"），不堆叠
  - 触顶不需要处理
- 飞书卡片 note 元素的硬上限？
  - 当前设计：note 是单个元素，每次 update 替换 text，不增加元素数
  - 没有触顶风险

## 13. Change log

- **2026-08-01** — 初版设计（取代 v0.2 stub 的 F-17）
  - tick 来源从"时间定时器"改为"每个 event +1"
  - 移除所有时间阈值 DEAD 触发
  - 用户主权：/kill 必须用户触发
  - 格式：⏳ N · HH:MM:SS · idle Xs/Xm
  - 文案原则：不假装知道 Claude 在做什么