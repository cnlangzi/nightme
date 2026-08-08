# F-24: Claude Code Bridge (JSON-IO + AskUserQuestion)

> **Status**: implemented (v1.1 — bridge 接口未变；Bridge 不知道 receipt / chat / binding 存在)
> **Milestone**: v0.2 (设计 + 实现), v0.3 (event callback 路径)
> **Depends on**: F-09 (Agent abstraction), F-19 (CLI Bridge), F-21 (Agent Modes)
> **Related**: [F-21-agent-modes.md](./F-21-agent-modes.md), [F-19-cli-bridge.md](./F-19-cli-bridge.md), [F-26-gateway-hub.md](./F-26-gateway-hub.md) §2.3 (single-consumer)

## 0. v1.1 修订（bridge 与上层解耦）

**v0.2 实现**：Claude Code bridge 通过 `Session.Events() <-chan AgentEvent` 把事件暴露给 session 内部 readPump + Gateway 外部 pumpOutbound。两个 reader 抢同一 chan 是 bug（见 [F-26 §2.3](./F-26-gateway-hub.md)）。

**v1.1 修订**：
- `bridge.Session.Events()` chan **只有 session.readPump 一个 consumer**（单消费者修复）
- Gateway 通过 `MemoryManager.EventCallback` 接收事件——bridge / agent 包不需要改 API；只是调用方用法变了
- Bridge / agent 包**完全不知道** receipt、chat、binding 的存在——它们只产 `agent.AgentEvent`，session 包只管 InputBuffer FSM，Gateway 负责 Translate + Send + receipt 翻转

**对 bridge 实现的影响**：无。`session.go: Events() <-chan agent.AgentEvent` 接口未变。Bridge 仍然写 `s.events chan`；session.readPump 仍然是唯一读者。

---

## 1. Description

Claude Code 是 Anthropic 的官方 AI coding CLI。当前 nightme v0.1 让 Claude Code 走 PTY mode（F-21 §5.3），但 PTY 有局限：

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

> "Use this tool only when you are blocked on a decision that is **genuinely the user's to make**: one you **cannot resolve** from the request, the code, or sensible defaults."

四个关键词：
- genuinely the user's to make
- cannot resolve from the request
- cannot resolve from the code
- cannot resolve from sensible defaults

### 2.2 决策质量（`tool-description-askuserquestion-decision-guidance.md`）

> "Reserve this for decisions where the user's answer **changes what you do next** — not for choices with a conventional default or facts you can verify in the codebase yourself."

排除项：
- 常规默认（agent 自己选）
- 代码里能查到的事实

### 2.3 调查优先（`system-prompt-clarifying-question-research-first.md`）

> "Before asking, spend up to a minute on **read-only investigation** (grep the codebase, check docs, search memory) so your question is specific."

要点：
- 先做 1 分钟只读调查
- 问题要具体（已发现的候选项，不是开放式）

### 2.4 最小选项（`system-reminder-askuserquestion-minimum-options-validation.md`）

> "A question with **fewer than 2 options** has no decision in it."

约束：
- ≥ 2 个不同选项

### 2.5 关键设计细节：Recommended 标记

> "If you recommend a specific option, make that the **first option** in the list and add '**(Recommended)**' at the end of the label"

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

**含附件 turn**（F-14 v1.4 落地后）：`content` 是 heterogeneous array，每个 element 单独表义，**顺序由调用方保证**（nightme 这边由 `[]agent.ContentBlock` slice 的下标 1:1 表达）：

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

**为什么不用"text 里嵌 `[img:xxx]` 占位符"**:Anthropic API `content` 字段本身就是 heterogeneous array，placeholder 方案会引入解析歧义 + 类型丢失 + 协议弱化。slice 1:1 对应 wire 数组是天然选择。详见 `docs/feat/F-14-attachment-passthrough.md` §4.5。

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
- 用户通过 /kill 控制中断
- 不需要交互式确认 UI

### 4.2 PreToolUse hook（可选）

CHANGELOG 显示 Claude Code 2.1.x 支持：

> "PreToolUse hooks can now satisfy AskUserQuestion by returning updatedInput alongside permissionDecision: 'allow', enabling headless integrations that collect answers via their own UI"

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

**v0.2 计划**：

- 先实现 **tool_use 拦截 + text fallback**（不依赖 hook）
- v0.3 评估 PreToolUse hook（如果上面方案有边界问题）

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

### 5.4 优先级

1. **tool_use 拦截**（优先）：识别 `name=="AskUserQuestion"`
2. **text fallback**（fallback）：解析 markdown 表格

如果两条路径都触发，**tool_use 优先**（更结构化）。

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

### 6.5 ⚠️ 未实测部分

- **实际 wire format** 需要真 Claude 验证（本地测试环境强制 routing 到 MiniMax）
- **multiSelect array 行为** 在 2.1.220 是否完全稳定需要测
- **"Other" 路径**的具体格式（CHANGELOG 提到 multi-select "Other" 曾被 silently drop）

v0.2 release note 必须标"待真 Claude 验证"。

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

**v0.2 选择**：**一张大卡片 + 一次提交**（避免多轮等待）。

## 8. Per-agent Bridge 架构

### 8.1 决策

每个 CLI 有自己的 `internal/bridge/<name>/` package：

```
internal/bridge/
├── pty/              # v0.1 兜底（任何 CLI）
├── acp/              # Codex / OpenCode（v0.2）
├── sdk/              # 占位（Anthropic 无 Go SDK）
└── claudecode/       # v0.2 新增：JSON-IO 专用
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

> **本地测试环境**：MiniMax-M3 模型（不是真 Claude），不展示 AskUserQuestion tool。这些 prompt 需要**真 Claude Pro 账户**验证。

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
      # PreToolUse hook（v0.3 评估）
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

## 13. Open questions

- **本地测试环境限制**：ANTHROPIC_BASE_URL 强制 routing 到 MiniMax，本地不能实测真 Claude 的 AskUserQuestion 行为 → v0.2 release 后用户实测，发现 bug 即修
- **CHANGELOG 显示 AskUserQuestion 持续维护**：2.1.220 仍在改进（multi-select array support, "Other" free-text fix）→ 大方向稳定，但细节需实测
- **PreToolUse hook 是否启用**：v0.2 不启用（保持简单），v0.3 评估
- **Claude Code 2.1.220 的 AskUserQuestion 是否仍通过 tools API 暴露**：本地 init event 不显示，但 CHANGELOG 显示 active feature。可能**模型/account-specific**（真 Claude Pro 才暴露）→ 不能 100% 确定，需要真 Claude 验证

## 14. Change log

- **2026-08-01** — 初版设计
  - JSON-IO 模式 + auto-accept + AskUserQuestion 双路兼容
  - per-agent bridge 架构（`internal/bridge/claudecode/`）
  - 4 个触发条件 from Piebald-AI/claude-code-system-prompts
  - 用户答案格式兼容（string + array）