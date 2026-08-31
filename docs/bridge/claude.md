# Claude Code Bridge

## A1. F-24: Claude Code Bridge (JSON-IO + AskUserQuestion)

> **Source**: `../bridge/claude.md`


> **Depends on**: F-09 (Agent abstraction), F-19 (CLI Bridge), F-21 (Agent Modes)
> **Related**: [../bridge/cli-transport.md](./../bridge/cli-transport.md), [../bridge/cli-transport.md](./../bridge/cli-transport.md), [F-gateway.md](./F-gateway.md) §2.3 (single-consumer)

## 0. 修订（bridge 与上层解耦）

**实现**：Claude Code bridge 通过 `Session.Events() <-chan AgentEvent` 把事件暴露给 session 内部 readPump + Gateway 外部 pumpOutbound。两个 reader 抢同一 chan 是 bug（见 [F-26 §2.3](./F-gateway.md)）。

**修订**：
- `bridge.Session.Events()` chan **只有 session.readPump 一个 consumer**（单消费者修复）
- Gateway 通过 `MemoryManager.EventCallback` 接收事件——bridge / agent 包不需要改 API；只是调用方用法变了
- Bridge / agent 包**完全不知道** receipt、chat、binding 的存在——它们只产 `agent.AgentEvent`，session 包只管 InputBuffer FSM，Gateway 负责 Translate + Send + receipt 翻转

**对 bridge 实现的影响**：无。`session.go: Events() <-chan agent.AgentEvent` 接口未变。Bridge 仍然写 `s.events chan`；session.readPump 仍然是唯一读者。

---

## 1. Description

Claude Code 是 Anthropic 的官方 AI coding CLI。当前 nightme 让 Claude Code 走 PTY mode（F-21 §5.3），但 PTY 有局限：

- 无结构化 event（只有 raw bytes + ANSI 垃圾）
- 权限确认靠用户手动输 `Y\n`（看到 "Allow? [Y/n]"）
- AskUserQuestion 工具的卡片渲染不可能（PTY 看不出 tool_use）

本 feature 设计一个**专用的 Claude Code bridge**，使用 Claude Code 官方的 `--input-format stream-json` + `--output-format stream-json` 模式：

- **结构化 event 流**：system / assistant / tool_use / tool_result / result
- **自动接受权限**：`--permission-mode bypassPermissions`
- **AskUserQuestion 双路兼容**：tool_use 拦截 + text fallback
- **PreToolUse hook**：可选 hook 拦截 AskUserQuestion 提供 headless answer

**核心决策**（per-agent bridge 架构）：每个 CLI 有自己的 `internal/bridge/<name>/` package（ClaudeCode、Codex、OpenCode 等）。AgentSession 接口统一，Bridge 实现按协议变化。

## 2. 触发条件：Claude Code 什么时候调用 AskUserQuestion？

基于 `Piebald-AI/claude-code-system-prompts` 仓库的 4 个 prompt 文件分析：

### 2.1 主触发条件（`tool-description-askuserquestion.md`）

四个关键词：
- genuinely the user's to make
- cannot resolve from the request
- cannot resolve from the code
- cannot resolve from sensible defaults

### 2.2 决策质量（`tool-description-askuserquestion-decision-guidance.md`）

排除项：
- 常规默认（agent 自己选）
- 代码里能查到的事实

### 2.3 调查优先（`system-prompt-clarifying-question-research-first.md`）

要点：
- 先做 1 分钟只读调查
- 问题要具体（已发现的候选项，不是开放式）

### 2.4 最小选项（`system-reminder-askuserquestion-minimum-options-validation.md`）

约束：
- ≥ 2 个不同选项

### 2.5 关键设计细节：Recommended 标记

→ nightme 飞书卡片**必须高亮第一项为 "Recommended"**（如果 LLM 推荐了某项）。

## 3. JSON-IO 模式

### 3.1 spawn 命令

```bash
claude \
  --print \
  --input-format stream-json \
  --output-format stream-json \
  --permission-mode bypassPermissions \
  --verbose \
  --cwd /workspace \
  [其他 args...]
```

**Flags 解释**：

| Flag | 作用 |
|------|------|
| `--print` | 非交互模式（无 TUI）|
| `--input-format stream-json` | stdin 接收 JSON（不是 raw text）|
| `--output-format stream-json` | stdout 输出 JSON events（不是 raw text）|
| `--permission-mode bypassPermissions` | 自动接受所有权限请求 |
| `--verbose` | 启用 stream-json 输出（必须）|

### 3.2 stdin 格式（user message）

**纯文本 turn**（最简形态）：

```json
{
  "type": "user",
  "message": {
    "role": "user",
    "content": "帮我优化这段代码"
  }
}
```

**含附件 turn**（F-14 落地后）：`content` 是 heterogeneous array，每个 element 单独表义，**顺序由调用方保证**（nightme 这边由 `[]agent.ContentBlock` slice 的下标 1:1 表达）：

```json
{
  "type": "user",
  "message": {
    "role": "user",
    "content": [
      {"type": "text",    "text": "看一下这只"},
      {"type": "image",   "source": {
        "type":       "base64",
        "media_type": "image/png",
        "data":       "<base64 cat>"
      }},
      {"type": "text",    "text": "跟这只"},
      {"type": "image",   "source": {
        "type":       "base64",
        "media_type": "image/jpeg",
        "data":       "<base64 dog>"
      }},
      {"type": "text",    "text": "的区别"}
    ]
  }
}
```

**PDF 文件**用 `type:"document"` 形态（Anthropic API 内联支持）：

```json
{"type": "document", "source": {
  "type":       "base64",
  "media_type": "application/pdf",
  "data":       "<base64 pdf>"
}}
```

**field 映射**（来自 `internal/bridge/claudecode/session.go::SendBlocks`,line 243-353）：

| `agent.ContentBlock.Type` | JSON element | 降级条件 |
|---------------------------|--------------|----------|
| `ContentText` | `{"type":"text","text": block.Text}` | `Text == ""` → skip |
| `ContentImage` | `{"type":"image","source":{"type":"base64","media_type": block.MediaType,"data": base64(file)}}` | size > 5 MiB → 文本注解；Path 为空 / os.Stat fail / base64 fail → skip |
| `ContentFile` (PDF) | `{"type":"document","source":{"type":"base64","media_type":"application/pdf","data": base64(file)}}` | 失败 → skip |
| `ContentFile` (其他 MIME) | `{"type":"text","text":"File: /path"}` | — |

**关键不变量**：
1. **`content[]` 数组顺序 = `[]ContentBlock` 顺序**（i → i，1:1 映射）
2. **空 slice → noop**：`SendBlocks([])` 立即返回，不写 envelope
3. **失败 block omit**：`content[]` 中不会插入 placeholder（避免 Claude 把"半截 array"误读为完整 turn）
4. **base64 转换只在 bridge 边界**：`agent.ContentBlock.Path` 永远持绝对路径，bridge 在 SendBlocks 内做 `os.ReadFile → base64.StdEncoding.EncodeToString`（`internal/bridge/claudecode/session.go:360`）

**为什么不用"text 里嵌 `[img:xxx]` 占位符"**:Anthropic API `content` 字段本身就是 heterogeneous array，placeholder 方案会引入解析歧义 + 类型丢失 + 协议弱化。slice 1:1 对应 wire 数组是天然选择。详见 `docs/channel/feishu-onboarding.md` §4.5。

多轮对话：每条 user message 单独一行 JSON，连续写入 stdin。

### 3.3 stdout 格式（event stream）

```json
// 1. session init
{
  "type": "system",
  "subtype": "init",
  "session_id": "uuid",
  "tools": ["Task", "Bash", "Edit", ...],
  "mcp_servers": [...],
  "model": "claude-sonnet-4-5",
  "permissionMode": "bypassPermissions"
}

// 2. assistant thinking
{
  "type": "assistant",
  "message": {
    "role": "assistant",
    "content": [{"type": "thinking", "thinking": "..."}]
  }
}

// 3. assistant text
{
  "type": "assistant",
  "message": {
    "role": "assistant",
    "content": [{"type": "text", "text": "让我看一下..."}]
  }
}

// 4. assistant tool_use
{
  "type": "assistant",
  "message": {
    "role": "assistant",
    "content": [{
      "type": "tool_use",
      "name": "Read",
      "input": {"file_path": "/path/to/file.py"}
    }]
  }
}

// 5. tool result
{
  "type": "user",
  "message": {
    "role": "user",
    "content": [{
      "type": "tool_result",
      "tool_use_id": "toolu_xxx",
      "content": "[file contents]"
    }]
  }
}

// 6. AskUserQuestion (this feature)
{
  "type": "assistant",
  "message": {
    "role": "assistant",
    "content": [{
      "type": "tool_use",
      "name": "AskUserQuestion",
      "input": {
        "questions": [{
          "question": "Which database?",
          "header": "Setup",
          "multiSelect": false,
          "options": [
            {"label": "PostgreSQL (Recommended)", "description": "Production-ready"},
            {"label": "SQLite", "description": "Dev / small project"},
            {"label": "MongoDB", "description": "Document-based"}
          ]
        }]
      }
    }]
  }
}

// 7. final result
{
  "type": "result",
  "subtype": "success",
  "session_id": "uuid",
  "duration_ms": 12345,
  "result": "[final text]",
  "usage": {"input_tokens": 100, "output_tokens": 200}
}
```

### 3.4 In-flight 取消：`/stop` 的真实协议

**2026-08-15 fix-stop**：用户报"第二次 /stop 后发送消息卡死在 Working…"。审计发现 claudecode bridge 用 `SignalProcessGroup(SIGINT)` 走 PTY，这跟 codex 老的 SIGINT 路径同款——子进程死了，聊天层 `--resume` 重启，撞上"幽灵 turn"。本文档记录**反向工程**过程、实际 wire 格式、以及最终方案。

#### 3.4.1 反向工程方法（不要信文档，要看 wire）

**为什么文档不够**：Anthropic `anthropics/claude-code` 的 `CHANGELOG.md` 只写"external SIGINT ... not running graceful shutdown"，读起来像"外部 SIGINT 是文档化的中断机制"。但 stream-json pipe 模式下实际行为完全不同——CLI **退出进程**而不是 cancel turn。文档没说 wire 协议，没说有没有 stdin 消息可以"取消 turn 但保留进程"。

**实验装置**（已落地到 `/tmp/claude-exp/test-interrupt.py`，可重跑）：

```bash
# 起一个真实 claude 子进程
ANTHROPIC_BASE_URL=... ANTHROPIC_DEFAULT_SONNET_MODEL=... \
  claude --print --output-format stream-json --input-format stream-json \
         --verbose --dangerously-skip-permissions \
         "Write a 200-line essay to /tmp/essay.txt. Take your time."
```

启动后：
1. 子进程发 init event 到 stdout——**关键发现**：`capabilities: ["interrupt_receipt_v1", "interrupt_cancel_queued_v1", "msg_lifecycle_v1"]`。`interrupt_receipt_v1` 这个 capability name 直接告诉我们"有一个 v1 版的结构化中断协议"，但 CHANGELOG 没提。
2. 通过 stdin 喂各种候选消息（详见 §3.4.3 的对照矩阵）。
3. 观察 stdout 的 `control_response` / `result` 事件判定哪种 payload 真正生效。

**判定信号**：
- CLI 发 `control_response{response.subtype: "success"}` → stdin payload 被接受
- CLI 发 `result{is_error: true, subtype: "error_during_execution"}` → turn 被取消
- `proc.poll() == nil` 12 秒后 → CLI **保持存活**（不是 SIGINT 那种退出）
- 后续再喂 `user` 消息 → CLI 在同一个 `session_id` 上继续 → turn-end → next turn 干净落地

#### 3.4.2 实际 wire 格式（反向工程结论）

**生效路径**（in-flight turn interrupt）：

```jsonc
// stdin
{"type":"control_request","request":{"subtype":"interrupt"}}

// stdout（紧跟其后）
{"type":"control_response","response":{
   "subtype": "success",
   "response": {"still_queued": []}
}}

// stdout（turn 终止事件）
{"is_error": true,
 "stop_reason": "tool_use",
 "subtype": "error_during_execution",
 "num_turns": 3, ...}
```

**关键语义**：

| 维度 | 行为 |
|---|---|
| CLI 进程 | **保持存活**，可接收下一条 user 消息 |
| session_id | **不变**——不需要 `--resume` |
| pendingTurnActive | turn 结束事件 `result{error_during_execution}` 触发现有的 translator 路径（`ev.IsError` 分支），发 `EventAgentResult` + `EventAgentDone{Reason:"settled"}`，`IsReady` 翻 true |
| 没有 in-flight turn 时 | `control_request{interrupt}` 是 clean no-op：发 `control_response{success, still_queued:[]}`，无 `result` 事件，CLI 不受影响 |
| 下一次 user prompt | 走同一个 `session_id`；`--resume` 不需要，幽灵 turn 不可能出现 |

#### 3.4.3 对照矩阵（哪些消息被接受）

| stdin payload | CLI 响应 | 备注 |
|---|---|---|
| `{"type":"control_request","request":{"subtype":"interrupt"}}` | `control_response{success}` + `result{error_during_execution}` + CLI 存活 | ✅ **正解** |
| `{"type":"interrupt"}` | 无响应，CLI 继续 turn | ❌ 被忽略 |
| `{"type":"control_request","request":{"subtype":"cancel_queued"}}` | 无响应，CLI 继续 turn | ❌ cancel_queued 是给"已排队但未开始"的消息用的，不是 in-flight turn |
| SIGINT (kill -INT) | 进程**退出**（exit code 0），chat 层走 `--resume` 路径 | ❌ pre-fix 实现；撞 codex 同款幽灵 turn |

#### 3.4.4 修复方案（落地的 `Stop()`）

**核心改动**（`internal/bridge/claudecode/claudecode.go::Stop`，line 498 起）：

```go
func (d *driver) Stop(ctx context.Context) error {
    if d.cmd == nil || d.cmd.Process == nil {
        return agent.ErrNotSupported
    }
    payload := []byte(`{"type":"control_request","request":{"subtype":"interrupt"}}`)
    if err := d.writeLine(payload); err == nil {
        return nil  // happy path: CLI stays alive, turn cancels in-band
    }
    // Fallback: stdin pipe broken (CLI exited). SIGINT triggers
    // the legacy graceful-shutdown path — only fires when the
    // happy path already failed, never on the happy path.
    return agent.SignalProcessGroup(d.cmd.Process, os.Interrupt)
}
```

**translator 不用改**——`internal/bridge/claudecode/stream.go::case "result"` 的 `ev.IsError` 分支已经覆盖 `subtype: "error_during_execution"`，发 `EventAgentResult` + `EventAgentDone`，`onTurnEnd` 释放 `pendingTurnActive`，`TryFlush` 取同 session 的下一条 prompt。整条链路天然走通。

**`stop.go` per-bridge 表格**：

```
claudecode — control_request{interrupt} stdin; CLI stays alive; turn ends cleanly; SessionID kept
```

#### 3.4.5 为什么 SIGINT 兜底仍然保留

`stdin pipe broken` 的边界情况：

1. CLI 已经在外面挂了（用户 Ctrl-C 终端、OOM kill、watchdog 终止）
2. CLI 版本老到不支持 control_request（虽然目前反向工程覆盖 2.1.220，但理论上更早版本可能没有）
3. pipe writer goroutine 异常

这三种情况下，`writeLine` 会返 `EPIPE` / `ErrClosed` 等错误，bridge 退到 SIGINT 走 legacy 路径。这跟 chat 层 `ErrResumeUnhealthy` 的双重防御结构是同源的——**重要路径永远不假设下层协议是最新的**。

#### 3.4.6 跟其他 bridge 的对比

| Bridge | stop 协议 | CLI 存活？ |
|---|---|---|
| **pi** | `abort` JSON-RPC | ✅ |
| **opencode** | `POST /api/session/{id}/interrupt` HTTP | ✅ |
| **codex** | `turn/interrupt` JSON-RPC | ✅ |
| **acp** | `session/cancel` JSON-RPC | ✅ |
| **claudecode**（fix-stop） | `control_request{interrupt}` stdin | ✅ |
| **pty** | `ErrNotSupported`（一次性 bridge，无 in-flight turn 概念） | — |

五个 bridge 现在统一在"结构化 in-band 取消 + 进程保持存活"这条契约上，跟 Anthropic / OpenAI / Zed 各自的官方协议语义对齐。SIGINT / SIGKILL 只在兜底路径用——保护老版本 CLI 和 edge cases。

#### 3.4.7 回归测试（`internal/bridge/claudecode/agent_interrupt_test.go`）

4 个测试，反向锁住"happy path 走 stdin 不走 SIGINT"这条契约：

| Test | 断言 |
|---|---|
| `TestStop_NoCmdReturnsErrNotSupported` | cmd 为空返 `ErrNotSupported`，不 panic |
| `TestStop_WritesControlRequestInterruptOnStdin` | 挂真 `sleep 30` + os.Pipe，调 Stop，**从 pipe 读回字节**，断言 == `{"type":"control_request","request":{"subtype":"interrupt"}}`（关键回归） |
| `TestStop_DoesNotSendSIGINT` | 同上装置，Stop 后 sleep **仍然存活 ≥1s**——SIGINT 会在 50ms 内干掉 sleep。**反向锁**：以后退回 SIGINT，测试立即挂掉 |
| `TestStop_FallsBackToSIGINTWhenStdinBroken` | 关闭 write 端造 EPIPE，Stop 应触发 SIGINT 兜底，sleep 在 5s 内退 |

`go test ./internal/bridge/claudecode/ -run 'TestStop_'` 全部 PASS。

## 4. 自动接受权限

### 4.1 模式选择

Claude Code 支持的 permission modes：

| Mode | 行为 |
|------|------|
| `acceptEdits` | 自动接受 Edit/Write，Bash 仍需确认 |
| `auto` | 类似 bypass，但保留一些安全检查 |
| `bypassPermissions` | 全部自动接受 |
| `manual` | 全部手动确认 |

**nightme 选择 `bypassPermissions`**：

- nightme 是自动化 daemon，不在终端前
- 用户通过 /close 控制中断
- 不需要交互式确认 UI

### 4.2 PreToolUse hook（可选）

CHANGELOG 显示 Claude Code 2.1.x 支持：

**这是 nightme 的关键 hook 点**：

```bash
# 注册 hook
claude --print \
  --permission-mode bypassPermissions \
  --hook 'PreToolUse'='/path/to/nightme-hook' \
  ...
```

```go
// nightme-hook: 拦截 AskUserQuestion tool_use
// 通过 stdout 返回 updatedInput 把答案注入
```

**计划**：

- 先实现 **tool_use 拦截 + text fallback**（不依赖 hook）
- 评估 PreToolUse hook（如果上面方案有边界问题）

## 5. AskUserQuestion 双路兼容

### 5.1 设计动机

CHANGELOG 显示 AskUserQuestion 在 Claude Code 2.1.220 仍存在，但某些情况下：
- 模型看不到工具（用 markdown 表格 fallback 到 text）
- 新版可能改名 / 整合 / 走 hook

→ nightme 必须**双路兼容**才能稳定。

### 5.2 路由 A: tool_use 拦截（首选）

```go
// internal/bridge/claudecode/session.go

func (s *ClaudeSession) handleAssistantMessage(msg AssistantMessage) {
    for _, block := range msg.Content {
        switch block.Type {
        case "tool_use":
            if block.Name == "AskUserQuestion" {
                questions := parseUserQuestions(block.Input)
                s.events <- AgentEvent{
                    Kind: EventPermissionRequest,
                    Permission: &PermissionRequest{
                        Tool:    "AskUserQuestion",
                        Action:  formatQuestions(questions),
                        Options: extractOptionLabels(questions),
                        Extra: map[string]any{
                            "questions": questions,
                        },
                        ResponseCh: make(chan string, 1),
                    },
                }
                return
            }
            // 其他 tool_use: EventToolStart
            s.events <- AgentEvent{Kind: EventToolStart, ...}
            
        case "text":
            s.events <- AgentEvent{Kind: EventText, Text: block.Text}
            
        case "thinking":
            // 可选：emit 或 ignore
        }
    }
}
```

### 5.3 路由 B: text fallback

检测 assistant 输出的 markdown 表格（如果 tool_use 路径未触发）：

```go
// 检测模式："| Option |" 风格的 markdown 表格 + "Pick one" 关键词
var askPattern = regexp.MustCompile(`(?s)\*\*\((\d+)\) ([^*]+)\*\*.*?\| Option \|`)

func (s *ClaudeSession) detectAskInText(text string) *PermissionRequest {
    if !askPattern.MatchString(text) {
        return nil
    }
    // 解析 markdown 表格 → questions 结构
    return parseMarkdownAsk(text)
}
```

### 5.4 优先级与共存

1. **tool_use 拦截**（结构化，携带 `tool_use_id`）：识别 `name=="AskUserQuestion"`
2. **text fallback**（cc 自己 fallback,无 `tool_use_id`）：解析 markdown 表格

如果两条路径都触发,**bridge 同时拦截**(都 emit `EventAgentPermission`),**应答分发按 origin 决定 wire 形状**(见 §5.5)。Channel 层不区分路径,渲染同一张 "Waiting for approval" 卡片。

### 5.5 Bridge 侧应答分发(2026-08 fix-claude-ask-your-question)

**问题**:2026-08 实测发现,某些 provider / 模型环境下,cc 自己 emit 文本 fallback("AskUserQuestion 不可用..."),同时也可能伴随真 `tool_use` 块。我们的两条拦截路径同时触发,但旧实现只在 tool_use 路径上把用户答案接回 `pendingAsk.ResponseCh`,text-fallback 路径上的 respCh 是孤儿 → 用户点按钮 → `driver.SendPermission` 返 `no pending AskUserQuestion` 错误 → 答案丢失。

**修复核心**:把"应答写入 stdin 的 JSON 形状"绑定到 `pendingAsk.TextFallback` 标志位上,而不是绑定到卡片 UI 上。

```
                    ┌──────────── (a) tool_use 路径 ────────────┐
                    │  armPendingAsk(block.ID, multi, false)    │
                    │  pendingAsk = {TF:false, ToolUseID:id,    │
                    │                 ResponseCh: respCh_a}     │
                    │  defaultAskHandler → EventAgentPermission │
                    └──────────────────────────────────────────┘
                                       │
                                       ▼  (用户点按钮 → respCh_a)
                                       ▼
                              driver.SendPermission(resp)
                                       │
                                ┌──────┴──────┐
                                │             │
                  TextFallback=false   TextFallback=true
                                │             │
                                ▼             ▼
                  encodeUserAnswer     writeUserText
                  (tool_result 形态)   (user-role text 形态)
                                │             │
                                ▼             ▼
                    writeLine(tool_result JSON)  writeLine(user text JSON)
                                │             │
                                └──────┬──────┘
                                       ▼
                                claude stdin

                    ┌─────── (b) text-fallback 路径 ───────────┐
                    │  armPendingAsk("", multi, true)           │
                    │  pendingAsk = {TF:true, ToolUseID:"",     │
                    │                 ResponseCh: respCh_b}     │
                    │  emitAskFromText → EventAgentPermission   │
                    └──────────────────────────────────────────┘
```

**两条路径的 wire 形状差异**:

| 路径 | 标志位 | stdin payload (`message.content[0]`) | cc 端行为 |
|------|--------|--------------------------------------|-----------|
| (a) tool_use | `TextFallback=false` | `{type:"tool_result", tool_use_id:"<id>", content:<resp>}` | 关联回对应 tool_use,模型按结构化答案处理 |
| (b) text-fallback | `TextFallback=true` | `{type:"text", text:<resp>}` | cc 当成下一轮 user 输入,模型继续推进 turn |

**`armPendingAsk` helper**(`internal/bridge/claudecode/claudecode.go`):

```go
// armPendingAsk 给 caller 提供 respCh 并把它挂进 pendingAsk。
// 返回的 armedEvents 必须喂给 emitAskFromText / defaultAskHandler:
// 流出的 EventAgentPermission.Permission.ResponseCh 会被拦截器
// 替换成我们持有的 respCh,保证 channel 层一定写到我们的 channel。
func (d *driver) armPendingAsk(blockID string, multi bool, textFallback bool) (
    respCh chan string, armedEvents chan<- agent.AgentEvent, done <-chan struct{},
)
```

**故意不做的**:

- 不做 pending 时把 typed text 路由到 SendPermission(留给用户决定)
- 不做 (a)→(b) 顺序触发时的 stale sentinel(让 last-write-wins,被覆盖的旧卡片按钮失效)
- 不清洗 label 里的 `(Recommended)` / `(保留 :8081)` 括号注释(模型自己接受括号字面)

详见 `claudecode.go::SendPermission`(`TextFallback` 分支)和 `ask.go::writeUserText`。

## 6. User Answer 格式

### 6.1 推断格式（基于 Anthropic API 约定 + cc-connect 代码）

```json
{
  "type": "user",
  "message": {
    "role": "user",
    "content": [{
      "type": "tool_result",
      "tool_use_id": "toolu_xxx",
      "content": "PostgreSQL"  // 选中的 label
    }]
  }
}
```

### 6.2 多格式兼容（CHANGELOG 验证）

CHANGELOG 显示 "Fixed AskUserQuestion discarding multi-select answers when supplied as an array"：

- **早期版本**：只接受 string（逗号分隔）
- **修复后**：array 也支持
- **nightme 必须同时支持**：

| multiSelect | content 类型 | 示例 |
|-------------|-------------|------|
| false | string | `"PostgreSQL"` |
| true | string | `"PostgreSQL,Auth"` |
| true | array | `["PostgreSQL", "Auth"]` |

### 6.3 "Other" 自定义输入

Claude Code 默认允许用户选 "Other" 自由输入。nightme 飞书卡片需要：
- 选项 button + "Other" 自由输入框
- "Other" 输入的纯文本作为 content

### 6.4 答案回写实现

```go
func (s *ClaudeSession) answerAsk(questions []Question, answers map[string]string) error {
    var contentBlocks []ContentBlock
    
    for i, q := range questions {
        answer := answers[q.Header]  // 用 header 索引（多问时区分）
        
        // 多选编码
        var content any
        if q.MultiSelect && strings.Contains(answer, ",") {
            content = strings.Split(answer, ",")
        } else if q.MultiSelect {
            content = []string{answer}
        } else {
            content = answer
        }
        
        contentBlocks = append(contentBlocks, ContentBlock{
            Type:      "tool_result",
            ToolUseID:  q.ToolUseID,
            Content:   content,
        })
    }
    
    msg := UserMessage{
        Type:    "user",
        Message: Message{Role: "user", Content: contentBlocks},
    }
    
    return s.writeJSON(msg)
}
```

### 6.5 text-fallback 路径的应答 wire 格式

(b) 路径没有对应 `tool_use_id`,所以不能写 `tool_result`。改为写一条 user-role 文本消息:

```json
{
  "type": "user",
  "message": {
    "role": "user",
    "content": [
      {"type": "text", "text": "<resp>"}
    ]
  }
}
```

实现(`claudecode.go::writeUserText`):

```go
func (d *driver) writeUserText(text string) error {
    payload := map[string]any{
        "type": "user",
        "message": map[string]any{
            "role": "user",
            "content": []map[string]any{
                {"type": "text", "text": text},
            },
        },
    }
    b, err := json.Marshal(payload)
    if err != nil {
        return err
    }
    return d.writeLine(b)
}
```

`SendPermission` 根据 `pendingAsk.TextFallback` 决定走 §6.1(tool_result)还是 §6.5(user text):

```go
func (d *driver) SendPermission(resp string) error {
    d.pendingMu.Lock()
    ask := d.pendingAsk
    d.pendingAsk = nil
    d.pendingMu.Unlock()

    if ask == nil {
        return fmt.Errorf("claudecode: no pending AskUserQuestion")
    }

    if ask.TextFallback {
        return d.writeUserText(resp)  // §6.5
    }
    // ... encodeUserAnswer + writeLine  // §6.1
}
```

### 6.6 ⚠️ 未实测部分

- **实际 wire format** 需要真 Claude 验证（本地测试环境强制 routing 到 MiniMax）
- **multiSelect array 行为** 在 2.1.220 是否完全稳定需要测
- **"Other" 路径**的具体格式（CHANGELOG 提到 multi-select "Other" 曾被 silently drop） release note 必须标"待真 Claude 验证"。
- **text-fallback 应答写 user-role text**（2026-08 fix）当前未在真 Claude 上跑过，wire 形状基于 cc 文档对 `stream-json` 的描述推断；建议在真机测试时打日志验证 cc 端能否正确把这条 user-role 消息解析为"对 fallback 问题的答案"

## 7. 飞书卡片渲染

### 7.1 卡片结构

```json
{
  "header": {
    "title": {"tag": "plain_text", "content": "Claude 提问"},
    "color": "blue"
  },
  "elements": [
    {
      "tag": "div",
      "text": {"tag": "lark_md", "content": "**Q1: Which database?**\n_Header: Setup_"}
    },
    {
      "tag": "div",
      "text": {"tag": "plain_text", "content": "PostgreSQL (Recommended) — Production-ready"}},
    {"tag": "div", "text": {"tag": "plain_text", "content": "SQLite — Dev / small project"}},
    {"tag": "div", "text": {"tag": "plain_text", "content": "MongoDB — Document-based"}},
    {"tag": "hr"},
    {
      "tag": "actions",
      "actions": [
        {"tag": "button", "text": {"tag": "plain_text", "content": "✅ PostgreSQL"}, "type": "primary", "value": "cmd:ask:1:PostgreSQL"},
        {"tag": "button", "text": {"tag": "plain_text", "content": "SQLite"}, "type": "default", "value": "cmd:ask:1:SQLite"},
        {"tag": "button", "text": {"tag": "plain_text", "content": "MongoDB"}, "type": "default", "value": "cmd:ask:1:MongoDB"}
      ]
    },
    {
      "tag": "input",
      "name": "other_answer",
      "placeholder": {"tag": "plain_text", "content": "或输入 Other..."}
    }
  ]
}
```

### 7.2 多选卡片（multiSelect: true）

用 select_static 元素：

```json
{
  "tag": "select_static",
  "placeholder": {"tag": "plain_text", "content": "选择（可多选）"},
  "options": [
    {"text": {"tag": "plain_text", "content": "PostgreSQL"}, "value": "PostgreSQL"},
    {"text": {"tag": "plain_text", "content": "Auth"}, "value": "Auth"},
    ...
  ],
  "multi_select": true
}
```

### 7.3 多问题（questions[] > 1）

每个 question 一张卡片，**连续发送**（不合并）。用户依次回答，最后一张答完才回写全部答案给 Claude。

或者**一张大卡片**，问题分块，用户一次提交全部答案（实现复杂但 UX 好）。

**选择**：**一张大卡片 + 一次提交**（避免多轮等待）。

## 8. Per-agent Bridge 架构

### 8.1 决策

每个 CLI 有自己的 `internal/bridge/<name>/` package：

```
internal/bridge/
├── pty/              # 兜底（任何 CLI）
├── acp/              # Codex / OpenCode
├── sdk/              # 占位（Anthropic 无 Go SDK）
└── claudecode/       # 新增：JSON-IO 专用
    ├── session.go
    ├── stream.go     # stream-json parse
    ├── permissions.go  # auto-accept
    ├── ask.go        # AskUserQuestion 拦截
    └── session_test.go
```

### 8.2 Agent 注册

```go
// internal/agent/registry.go

func init() {
    Register("claude", &claudecode.Agent{})  // ModeJSONIO（自定义）
    Register("codex", &acpagent.Agent{})      // ModeACP
    Register("opencode", &acpagent.Agent{})   // ModeACP
    Register("/path/to/cli", &ptyagent.Agent{})  // ModePTY fallback
}
```

### 8.3 Mode 扩展

F-21 §4 的 `Mode` enum 增加：

```go
const (
    ModeACP Mode = iota
    ModeSDK
    ModePTY
    ModeJSONIO  // 新增：专用于 Claude Code 的 JSON-IO 模式
)
```

## 9. 实现

### 9.1 文件结构

```
internal/bridge/claudecode/
├── claudecode.go       # Agent impl
├── session.go          # AgentSession impl
├── stream.go           # stream-json event parser
├── permissions.go      # --permission-mode 标志 + auto-accept
├── ask.go              # AskUserQuestion 拦截 + fallback
├── format.go           # content block formatting
├── claudecode_test.go  # unit tests
└── testdata/           # mock JSON event fixtures
    ├── init.json
    ├── text_chunk.json
    ├── tool_use.json
    ├── tool_result.json
    ├── ask_question.json
    └── result.json
```

### 9.2 关键代码

```go
// internal/bridge/claudecode/claudecode.go

type Agent struct{}

func (a *Agent) Name() string { return "claude" }
func (a *Agent) Mode() Mode { return ModeJSONIO }

func (a *Agent) Detect() error {
    // 检查 $PATH 里 claude binary 是否存在
    _, err := exec.LookPath("claude")
    return err
}

func (a *Agent) Start(ctx context.Context, cfg StartConfig) (AgentSession, error) {
    args := []string{
        "--print",
        "--input-format", "stream-json",
        "--output-format", "stream-json",
        "--permission-mode", "bypassPermissions",
        "--verbose",
    }
    args = append(args, cfg.Args...)
    
    cmd := exec.CommandContext(ctx, "claude", args...)
    cmd.Dir = cfg.Workspace
    cmd.Env = cfg.Env
    
    stdin, err := cmd.StdinPipe()
    if err != nil { return nil, err }
    
    stdout, err := cmd.StdoutPipe()
    if err != nil { return nil, err }
    
    stderr, err := cmd.StderrPipe()
    if err != nil { return nil, err }
    
    if err := cmd.Start(); err != nil { return nil, err }
    
    session := &claudeSession{
        cmd:     cmd,
        stdin:   stdin,
        stdout:  stdout,
        stderr:  stderr,
        events:  make(chan AgentEvent, 64),
        toolUseIDs: make(map[string]string),  // tool_use_id → question header
    }
    
    go session.pumpStdout()  // parse stream-json events
    go session.pumpStderr()  // log stderr
    
    return session, nil
}
```

```go
// internal/bridge/claudecode/stream.go

func (s *claudeSession) pumpStdout() {
    scanner := bufio.NewScanner(s.stdout)
    scanner.Buffer(make([]byte, 64*1024), 1024*1024)  // 1MB max line
    
    for scanner.Scan() {
        line := scanner.Bytes()
        if len(line) == 0 { continue }
        
        var event StreamEvent
        if err := json.Unmarshal(line, &event); err != nil {
            slog.Warn("claudecode: invalid json", "err", err, "line", string(line))
            continue
        }
        
        s.handleEvent(event)
    }
    
    if err := scanner.Err(); err != nil {
        s.events <- AgentEvent{Kind: EventError, Error: &ErrorEvent{Message: err.Error()}}
    }
    
    // stdout EOF → session end
    s.events <- AgentEvent{Kind: EventDone, Done: &DoneEvent{ExitCode: s.cmd.ProcessState.ExitCode()}}
    close(s.events)
}

func (s *claudeSession) handleEvent(ev StreamEvent) {
    switch ev.Type {
    case "system":
        if ev.Subtype == "init" {
            s.events <- AgentEvent{Kind: EventAgentConnected, Init: &AgentConnectedEvent{
                SessionID: ev.SessionID,
                Tools:     ev.Tools,
                Model:     ev.Model,
            }}
        }
        
    case "assistant":
        for _, block := range ev.Message.Content {
            switch block.Type {
            case "text":
                s.events <- AgentEvent{Kind: EventText, Text: block.Text}
            case "thinking":
                // 可选：emit thinking event 或 ignore
            case "tool_use":
                if block.Name == "AskUserQuestion" {
                    s.handleAskQuestion(block)
                } else {
                    s.events <- AgentEvent{Kind: EventToolStart, ToolStart: &ToolStartEvent{
                        Name:   block.Name,
                        Input:  block.Input,
                        ToolUseID: block.ID,
                    }}
                }
            }
        }
        
    case "user":
        // tool_result
        for _, block := range ev.Message.Content {
            if block.Type == "tool_result" {
                s.events <- AgentEvent{Kind: EventToolEnd, ToolEnd: &ToolEndEvent{
                    ToolUseID:  block.ToolUseID,
                    Content:    block.Content,
                    IsError:    block.IsError,
                }}
            }
        }
        
    case "result":
        s.events <- AgentEvent{Kind: EventDone, Done: &DoneEvent{
            ExitCode: 0,
            Result:   ev.Result,
            Usage:    ev.Usage,
        }}
    }
}
```

## 10. 测试用例

### 10.1 单元测试（用 mock JSON fixtures）

```go
// internal/bridge/claudecode/stream_test.go

func TestPumpStdout_EventAgentConnected(t *testing.T) {
    fixture := loadFixture("testdata/init.json")
    session := newTestSession(fixture)
    go session.pumpStdout()

    event := <-session.events
    if event.Kind != EventAgentConnected {
        t.Errorf("expected EventAgentConnected, got %v", event.Kind)
    }
    if event.Connected.Model != "claude-sonnet-4-5" {
        t.Errorf("expected model claude-sonnet-4-5, got %s", event.Connected.Model)
    }
}

func TestPumpStdout_AskUserQuestion(t *testing.T) {
    fixture := loadFixture("testdata/ask_question.json")
    session := newTestSession(fixture)
    go session.pumpStdout()
    
    // skip init event
    <-session.events
    
    event := <-session.events
    if event.Kind != EventPermissionRequest {
        t.Errorf("expected EventPermissionRequest, got %v", event.Kind)
    }
    if event.Permission.Tool != "AskUserQuestion" {
        t.Errorf("expected tool AskUserQuestion, got %s", event.Permission.Tool)
    }
    if len(event.Permission.Options) != 3 {
        t.Errorf("expected 3 options, got %d", len(event.Permission.Options))
    }
    // 检查第一项是 Recommended（来自 Claude Code 设计）
    if !strings.Contains(event.Permission.Options[0], "Recommended") {
        t.Errorf("expected first option to be Recommended")
    }
}

func TestAnswerAsk_SingleSelect(t *testing.T) {
    questions := []Question{{Header: "Setup", MultiSelect: false}}
    answers := map[string]string{"Setup": "PostgreSQL"}
    
    json, _ := session.answerAskJSON(questions, answers)
    
    // 验证 wire format
    var msg map[string]any
    json.Unmarshal([]byte(json), &msg)
    
    content := msg["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
    if content["type"] != "tool_result" {
        t.Errorf("expected tool_result, got %v", content["type"])
    }
    if content["content"] != "PostgreSQL" {
        t.Errorf("expected content 'PostgreSQL', got %v", content["content"])
    }
}

func TestAnswerAsk_MultiSelect_String(t *testing.T) {
    questions := []Question{{Header: "Stack", MultiSelect: true}}
    answers := map[string]string{"Stack": "PostgreSQL,Auth"}
    
    json, _ := session.answerAskJSON(questions, answers)
    // 验证 content 可以是 string 或 array
}

func TestAnswerAsk_MultiSelect_Array(t *testing.T) {
    questions := []Question{{Header: "Stack", MultiSelect: true}}
    answers := map[string]string{"Stack": "PostgreSQL,Auth"}
    
    // 强制 array 模式
    json, _ := session.answerAskJSONArray(questions, answers)
    // 验证 content 是 array
}
```

### 10.2 集成测试（mock Claude Code binary）

```go
// internal/bridge/claudecode/integration_test.go

func TestIntegration_MockClaude_RoundTrip(t *testing.T) {
    // mock claude binary 输出预定义 JSON events
    mock := newMockClaudeBinary(t, []string{
        `{"type":"system","subtype":"init",...}`,
        `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hello"}]}}`,
        `{"type":"result","subtype":"success",...}`,
    })
    defer mock.Close()
    
    session, err := mock.Start(ctx, StartConfig{Workspace: t.TempDir()})
    if err != nil { t.Fatal(err) }
    
    session.SendText("hello")
    
    events := collectEvents(session, 3)
    // 验证 init → text → result
}
```

### 10.3 真 Claude 测试 prompt（5 个确定触发 AskUserQuestion）

| # | Prompt | 触发原因 |
|---|--------|----------|
| 1 | "Before you write code, use AskUserQuestion to ask about: (1) auth method (JWT/OAuth/Session) (2) user store (PostgreSQL/SQLite/MongoDB). Each with 3 options." | 用户偏好决定 |
| 2 | "I see two patterns in this codebase: class-based service vs functional hook. Use AskUserQuestion to ask which style to use for the new feature." | 调查后问偏好 |
| 3 | "I'm about to set up CI/CD. Should I use GitHub Actions or Jenkins?" | 二选一，genuine 决定 |
| 4 | "I see two logging styles: console.log vs structured logging. Ask me which to use." | 调查后问 |
| 5 | "I have two migration strategies: blue-green vs rolling update. Use AskUserQuestion to ask which to use." | 复杂决定 |

**不触发的**：
- "Read file X" - 直接读
- "Fix the bug" - 单一答案
- "Use best practices" - 没具体选项

## 11. 配置

```yaml
# configs/nightme.example.yaml
agents:
  claude:
    bridge: claudecode
    args: []
    permission_mode: bypassPermissions  # bypassPermissions | auto | acceptEdits
    ask_user_question:
      dual_path: true                  # tool_use 拦截 + text fallback
      card_first_option_recommended: true  # 高亮第一项为 Recommended
      multi_select:
        prefer_array: true             # 多选用 array 编码（不是 string）
      # PreToolUse hook
      hook_enabled: false
      hook_path: ""

  codex:
    bridge: acp
    args: []
    
  opencode:
    bridge: acp
    args: [acp]
```

## 12. Edge cases

| 场景 | 处理 |
|------|------|
| Claude Code binary 不在 $PATH | Detect 返回 error，nightme 提示安装 |
| `--input-format stream-json` 被未来版本移除 | 退回到 PTY mode（F-21 fallback）|
| AskUserQuestion tool 不在 init event tools 列表 | text fallback 路径启用 |
| 用户答完一个问题就跑了 | 剩余问题作为下次 send 处理（不阻塞） |
| Claude Code 退出 | EventDone 正常发出，session 清理 |
| stream-json parse 错误 | log warning + 跳过该行（不阻塞 session） |
| 飞书卡片渲染 AskUserQuestion 失败 | 降级为文本："Q: ... Options: A, B, C（回复 'A' 选择）" |
| 多选答案 user 输入 free-form 文字 | nightme 当成单一选项处理，warn user "已识别为单选" |
| PreToolUse hook 拦截 + tool_use 拦截都触发 | tool_use 优先，hook 忽略（避免双重回答） |
| **(a) tool_use + (b) text fallback 同时触发**（实测常见，2026-08 fix） | bridge 同时 emit 两张 `EventAgentPermission`；`pendingAsk` 是单槽，**last-write-wins**；被覆盖的旧卡片按钮 → 孤儿 respCh，点击无效；用户需要点**最新的**那张卡片才能让答案回到 cc。被覆盖的 respCh 没人消费，无 stale 信号（**已知简化**，不做） |
| **(b) → (a) 顺序触发** | (a) 升级覆盖 (b)，旧 (b) 卡片 respCh 变孤儿；用户点 (a) 卡片即可，走 `tool_result` 路径 |
| **用户在 pending 时手打文本** | 不做劫持：typed text 走普通 `HandleInbound → SendBlocks` 路径，写 user-role text 到 stdin。cc 收到后按"下一轮 user 输入"处理——实测在 (b) 路径下用户手打"按'完全同步对齐'..."被模型正确解析为 option B |

## 13. Open questions

- **本地测试环境限制**：ANTHROPIC_BASE_URL 强制 routing 到 MiniMax，本地不能实测真 Claude 的 AskUserQuestion 行为 → release 后用户实测，发现 bug 即修
- **CHANGELOG 显示 AskUserQuestion 持续维护**：2.1.220 仍在改进（multi-select array support, "Other" free-text fix）→ 大方向稳定，但细节需实测
- **PreToolUse hook 是否启用**：不启用（保持简单），评估
- **Claude Code 2.1.220 的 AskUserQuestion 是否仍通过 tools API 暴露**：本地 init event 不显示，但 CHANGELOG 显示 active feature。可能**模型/account-specific**（真 Claude Pro 才暴露）→ 不能 100% 确定，需要真 Claude 验证

## 14. Change log

  - JSON-IO 模式 + auto-accept + AskUserQuestion 双路兼容
  - per-agent bridge 架构（`internal/bridge/claudecode/`）
  - 4 个触发条件 from Piebald-AI/claude-code-system-prompts
  - 用户答案格式兼容（string + array）
  - **2026-08 fix-claude-ask-your-question**:修复 text-fallback 路径的 `ResponseCh` 孤儿 bug。
    - `pendingAsk` 加 `TextFallback bool` 字段
    - 新增 `armPendingAsk` helper（共用给两条路径）
    - `SendPermission` 按 `TextFallback` 分支：(a) 走 `encodeUserAnswer` 写 `tool_result`；(b) 新增 `writeUserText` 写 user-role text message
    - `emitAskFromText` 不再自造 `ResponseCh`，由 `armPendingAsk` 注入
    - `pumpStream` 签名加 `armPendingAskFn` 参数
    - 故意不做：pending 时 typed text 劫持、(a)→(b) stale sentinel、label 括号清洗

---

## A2. Claude Code Bridge — 集成经验与规范

> **Source**: `claude.md`


本文记录 **对接真实 `claude` CLI 时必须遵守的约定**。F-24 讲「怎么设计」；本文讲「实测行为是什么、哪些做法会 silent hang、测试/日志怎么写才不炸 CI」。

---

## 1. 传输层

```
nightme ──stdio JSONL──> claude --print \
  --input-format stream-json \
  --output-format stream-json \
  --permission-mode bypassPermissions \
  --verbose \
  [--resume <session_id>]
```

| Flag | 作用 | 备注 |
|------|------|------|
| `--print` | 非交互（无 TUI） | **必须**。缺了会走 interactive，`system init` 被 stdin 门控，Spawn 后无 init → 首条消息 hang |
| `--input-format stream-json` | stdin = 一行一条 JSON user msg | |
| `--output-format stream-json` | stdout = 一行一条 JSON event | |
| `--permission-mode …` | 权限策略 | 默认 `bypassPermissions`；可由 `StartConfig.PermissionMode` 覆盖 |
| `--verbose` | 打开 stream-json 输出 | 官方要求；缺了无结构化事件 |
| `--resume <id>` | 从磁盘 session 恢复上下文 | 仅 daemon 重启 / AS respawn；**不是**每轮用户消息 |

**故意不传**：

- `--model` — 交给用户的 `~/.claude/settings.json` / 自定义 provider（MiniMax 等）
- `--replay-user-messages` — 会把 user 文本回显到 stdout，飞书侧会多出「你说了…」气泡；用 Reply 锚点代替

权威 argv 组装：`DefaultArgs`（`permissions.go`）+ `buildArgs`（`claudecode.go`）。

---

## 2. 进程生命周期（实测，2026-08-07）

> 旧注释曾写「`--print` = 每 Spawn 一轮，result 后 claude 退出」。**实测不成立。**

### 2.1 生产模型：单进程多轮

```
Spawn(claude --print …)          ← 一个 bridge session / 一个 OS 进程
  │
  ├─ emit system/init            ← 立即（不依赖先写 stdin）
  ├─ SendBlocks(turn1) → … → result
  ├─ SendBlocks(turn2) → … → result   ← 同一进程，stdin 一直开着
  ├─ …
  └─ Close() / 进程退出          ← 只有显式关闭或异常才结束
```

关键事实：

1. **`--print` + bridge 持有 stdin 不关** ⇒ claude 处理完一条 **不退出**，等下一条 newline-terminated JSON。
2. 同 chat 内多轮 **不需要** respawn，也不需要每次带 `--resume`。
3. `--resume <id>` 只在 **daemon 重启 / AS 变成 Detached 后重新 Spawn** 时使用，靠 claude 磁盘 session 恢复上下文。

### 2.2 为什么必须 `--print`

没有 `--print` 时 claude 走 interactive/TUI 风格：

- `system init` **门控在 stdin 有数据之后**
- bridge `Start` 返回时 events 里还没有 init
- 首条用户消息看起来「写进去了」但永远等不到 OutReply

因此：`DefaultArgs` **永远**带 `--print`。不要为了「多轮」去删它——多轮靠持有 stdin，不靠 interactive 模式。

### 2.3 与 pi 的对照

| | Claude | Pi |
|---|---|---|
| 传输 | stream-json stdout/stdin | `--mode rpc` JSONL |
| 进程/轮次 | 一进程多轮（held stdin） | 一进程多轮（RPC session） |
| 恢复 | `--resume <id>` | `--session-id` / `new_session` |
| 终态信号 | `result` → `EventResult`/`EventDone`；进程退出 → `KindLifecycle` | `agent_settled` → `EventResult`/`EventDone` |

两边共性：**`EventDone` ≠ 关 events channel**；只有进程退出或 `Close()` 才关。上层 PumpEvents 依赖这点跨 turn 持续读。

---

## 3. 事件通路（两级 channel，单消费者）

```
claude stdout
  → pumpStream → sess.events (cap 64)
       │              ↑ 唯一读者 = AS readpump
       │              ✗ 禁止第二读者（含 resume probe）
  → AS readpump enrich → as.eventQueue (cap 256)
       │                      ↑ 唯一读者 = cs.PumpEvents
  → routeEvent → eventHandler → feishu OutReply / OutResult
```

### 3.1 硬性规则

1. **`sess.Events()` 只有一个 consumer**（AS readpump）。任何「旁路偷看」都会抢走 init/result，表现为「进程活着但 nightme 收不到事件」。
2. **`as.eventQueue` 必须非 nil**。往 nil channel 发 = 永久阻塞 → `sess.events` 填满 → claude stdout pipe 背压 → 进程 0% CPU「假 hang」。
3. **构造路径必须合一**：所有运行时字段（channel / atomic / opCtx）只在 `newAgentSessionRuntime` 分配；`NewAgentSession` 与 `FromAgentSessionEntry` 都先调它。**禁止**在包内另起 `&AgentSession{}` 字面量。

### 3.2 生产事故：daemon 重启后 test20 hang

```
daemon restart
  → FromAgentSessionEntry 漏 make(eventQueue)   ← 旧 bug
  → Spawn → startReadPump → SendBlocks ok
  → claude emit init → pumpStream → sess.events
  → readpump: as.eventQueue <- …                ← send on nil = 死锁
  → 无 OutReply；claude 活着、0% CPU、pipe 仍连着 nightme
```

症状极易误判成「claude/`--resume` 坏了」。对照检查：

| 现象 | 含义 |
|------|------|
| 日志有 `Submit SendBlocks ok`，之后完全静默 | 卡在 **events 回流**，不是 stdin |
| 同机手动 `claude --print … --resume <id>` 正常 | CLI / session id / workspace 没问题 |
| main 正常、带 eventQueue 的分支必 hang | 构造路径漂移 |

回归：`TestFromAgentSessionEntry_InitializesEventQueue`（`internal/chatsession/restore_respawn_test.go`）。

教训（一句话）：

> `NewX` 分配的每个运行时字段（尤其是 channel），`FromPersistedX` 必须同样分配。漏一个 channel = send on nil = 永久阻塞，日志看起来像「一切正常只是没回包」。

---

## 4. `--resume` 语义与健康探测

### 4.1 何时传 ResumeID

| 场景 | 是否 `--resume` |
|------|-----------------|
| chat 内第 2、3、… 条用户消息（同 AS Running） | **否** — 复用 handle + SendBlocks |
| AS Exited / Detached 后同 chat 再发 | **是** — Spawn 带磁盘上的 ResumeID |
| daemon 重启后首次消息 | **是** — 同上 |
| `/new` 或明确开新会话 | **否** — 空 ResumeID，fresh session |

### 4.2 Probe：stderr-only

`probeResume` **只读 stderr**，窗口 `resumeFallbackTimeout = 5s`。

原因：早期版本同时读 `sess.Events()`，与 AS readpump 抢同一 buffered channel → probe 常抢走 init → 上层永远看不到会话就绪（手动 claude 17s 出结果，nightme 却 60s 超时）。

Probe **不**验证「是否在响应」——那是 readpump 的事。它只看：

- 已知的 resume 拒绝信号
- probe 窗口内进程是否干净退出（无拒绝信号 → 仍视为 healthy，交给上层）

### 4.3 拒绝静默 fallback

`--resume` 失败时返回 **`ErrResumeUnhealthy`**，**禁止**静默丢掉 ResumeID 开 fresh session。

| stderr / result 文本 | 行为 |
|----------------------|------|
| `No conversation found with session ID` | unhealthy → `ErrResumeUnhealthy` |
| `--resume requires a valid session ID…` | unhealthy → `ErrResumeUnhealthy` |
| `Failed to connect MCP server …` | **healthy（忽略）** — MCP 挂不影响 init / 处理消息 |

MCP 曾被误收进 classifier：配置坏掉的 MCP 会触发「假 unhealthy」+ 静默 fallback → 用户上下文无声丢失。分类器见 `classifyStderrLineForResume`。

### 4.4 Workspace 必须匹配

`--resume` 的 session 与 **cwd** 绑定。错误 workspace 会立刻：

```
No conversation found with session ID: <uuid>
```

生产 Spawn 必须用 AS 持久化的 `Cwd`，本地 repro 也必须对齐。

---

## 5. 上层对接约定（chatsession / feishu）

### 5.1 OutReply 锚点

`OutboundMessage.ReplyTo` **必须**等于用户消息 ID（`Prompt.LastMessageID`）。

`buildPromptLocked` 末尾赋值 `p.LastMessageID = ids[n-1]`。漏了 → receipt 找不到锚点 → 新卡片 orphan → 用户感觉「没回包」（其实事件到了）。

### 5.2 KindLifecycle → SetExited

per-AS readpump 拆分后，consumer 必须把 `KindLifecycle{StatusExited}` 接到 `as.SetExited(0)`。否则 AS 永远 `StatusRunning`，复用已死 handle。

### 5.3 isReady 与 TryFlush

磁盘恢复 / Spawn 成功后必须 `isReady.Store(true)`，否则 `TryFlush SKIP reason=as_not_ready` 永久跳过投递。该字段同样经 `newAgentSessionRuntime` 统一初始化。

### 5.4 启动审计日志

daemon 启动时应能看到 `runtime: handlers installed for chat`（证明该 chat 的 eventHandler / MessageState 等已挂上）。缺这条再查 WithOnCreate 路径。

---

## 6. 可观测性与日志级别

原则：**Info = 生命周期节点；Debug = 每条消息轨迹；Warn = 失败/拒绝。**

| 级别 | 保留的内容 | 示例 |
|------|------------|------|
| **Info** | 进程/AS 生命周期、handler 安装 | `AS marked Exited`、`handlers installed for chat`、`feishu: ws connected` |
| **Warn** | 投递失败、resume 拒绝、异常路径 | `Submit SendBlocks FAILED`、`--resume spawn unhealthy` |
| **Debug** | 热路径成功轨迹 | `Submit` / `Submit SendBlocks ok`、`TryFlush SKIP`、逐事件 / stderr 普通行 |

排查「发了消息没回包」时：

```bash
bin/nightme logs --once -n 200 | grep -E "chatsession:|claudecode:|runtime:|feishu: outgoing"
```

健康路径期望：

1. `feishu: incoming`
2. （Debug）`Submit` → `Submit SendBlocks ok`
3. 很快有 `feishu: outgoing`（`send_card` / reply）或至少 handler 侧痕迹
4. **不应**出现 SendBlocks ok 后长时间完全静默

若静默：先查 `eventQueue` 是否非 nil、readpump 是否在跑、是否有第二消费者抢 events——不要先怀疑模型 API。

---

## 7. 测试规范

### 7.1 真机 vs mock

| 类型 | 何时用 | PATH 守卫 |
|------|--------|-----------|
| **真机**（spawn 真实 `claude`） | `--resume`、stream-json、MCP、多轮 stdin | **必须** `requireRealClaude(t)` |
| **Mock**（`testdata/claude_mock.py` 等） | argv / 解析 / 协议单测 | **不要** skip — CI 必跑 |

共享 helper：`internal/bridge/claudecode/testhelpers_realclaude_test.go`：

```go
func requireRealClaude(t *testing.T) {
    t.Helper()
    if _, err := exec.LookPath("claude"); err != nil {
        t.Skipf("claude binary not on PATH: %v", err)
    }
}
```

约定：

- 真机测试 **第一行**调用 `requireRealClaude(t)`，禁止各文件复制 `LookPath`
- CI / 无 `claude` 的机器 → **SKIP**，不得 FAIL
- Mock / 纯单元测试不要调用该 helper

当前真机入口（均已守卫）：

- `fresh_liveness_test.go`
- `repro_real_test.go`
- `resume_fallback_test.go`
- `resume_multi_turn_test.go`
- `resume_paths_test.go`

### 7.2 本地环境必须完整

本地手动或真机测试 **必须**让 `claude` 加载完整 `~/.claude/settings.json`（模型、`env` 里的 proxy / API key 等）。

- 二进制会自己读用户配置；不要用空环境硬 spawn
- 自定义 provider（如 MiniMax）+ `HTTP(S)_PROXY` 缺失时，表现是「hang / 无 result」，易误判成 bridge bug
- 对照实验：与 nightme 相同的 argv + cwd + settings，用独立脚本跑一遍

### 7.3 常用命令

```bash
# 不依赖真机 claude（CI 安全）
go test ./internal/bridge/claudecode/ -count=1
go test ./internal/chatsession/ -count=1 \
  -run 'TestFromAgentSessionEntry_InitializesEventQueue|TestRestoreFromRegistry'

# 本机有 claude 时跑真机 resume / 多轮（无二进制则 Skip）
go test ./internal/bridge/claudecode/ -count=1 -timeout 240s \
  -run 'TestStart_Resume|TestResumePaths|TestFresh'
```

---

## 8. 调试 checklist（用户报「没 OutReply」）

按顺序排除，避免重复踩坑：

1. **Daemon 是否新二进制？** `make restart` / 确认日志里有 `handlers installed`。
2. **日志停在哪？**
   - 无 `Submit` → 卡在 Queue / TryFlush / isReady
   - 有 `SendBlocks ok` 无后续 → events 回流（eventQueue / 消费者抢占 / 背压）
   - 有事件无飞书卡片 → ReplyTo / LastMessageID / receipt
3. **AS 是否 Running 但 handle 已死？** KindLifecycle 是否调用了 `SetExited`。
4. **是否刚重启过 daemon？** 优先怀疑 restore 构造路径（`FromAgentSessionEntry`）。
5. **同 cwd 手动 `--resume` 是否成功？** 成功则 CLI/session 正常，问题在 nightme；失败则查 workspace / session id。
6. **settings / proxy / 模型** 是否与日常交互一致。
7. **残留 `claude --print` 孤儿进程** 是否占着之前的 pipe（daemon 杀掉后应清理）。

---

## 9. 代码锚点（改行为时先读）

| 主题 | 位置 |
|------|------|
| 默认 argv / `--print` | `internal/bridge/claudecode/permissions.go` |
| Start + resume probe + `ErrResumeUnhealthy` | `internal/bridge/claudecode/claudecode.go` |
| session / pumpStream / stderr | `internal/bridge/claudecode/session.go` |
| 事件解码 | `internal/bridge/claudecode/stream.go` |
| AskUserQuestion 拦截 + `pendingAsk` + `armPendingAsk` + `SendPermission` 分支 | `internal/bridge/claudecode/claudecode.go`、`ask.go` |
| `writeUserText`（text-fallback 应答回写） | `internal/bridge/claudecode/claudecode.go` |
| `emitAskFromText` + `detectAskInText` | `internal/bridge/claudecode/ask.go` |
| 运行时唯一构造器 | `internal/chatsession/agentsession.go` → `newAgentSessionRuntime` |
| restore | `FromAgentSessionEntry`（同上） |
| Exited 接线 | `internal/chatsession/pump_events.go` |
| 真机 skip helper | `testhelpers_realclaude_test.go` |
| restore 回归 | `restore_respawn_test.go` → `TestFromAgentSessionEntry_InitializesEventQueue` |
| AskUserQuestion 双路径回归 | `internal/bridge/claudecode/claudecode_test.go` → `TestPumpStream_AskUserQuestion` + `TestSendPermission_TextFallback_WritesUserText`（待补）|

---

---

