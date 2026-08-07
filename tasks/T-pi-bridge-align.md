# T-pi: Pi Bridge 对齐修正方案

> **Status**: ready to implement
> **Date**: 2026-08-06
> **Source**: 对照 `docs/feat/F-32-pi-rpc-bridge.md` + `docs/feat/F-24-claudecode-bridge.md`，以 **当前代码** 为准做 gap 审计（不盲信文档）
> **Scope**: `agent: pi`（`internal/bridge/pi/`）相对 `claudecode` / 现行 runtime contract 的缺口
> **Out of scope（本任务不实现）**: Extension UI → 飞书卡闭环、`/abort`、steer/follow_up、运行时切 model

---

## 0. 审计结论（代码事实）

F-32 RPC 核心 **已落地**：真 pipe spawn、`get_state` / `prompt` / 事件翻译、`agent_settled`→`EventDone{Reason:"settled"}`、`new_session`（F-34）、compaction（F-49）、Builtins 注册。

相对 `claudecode` + 现行 Channel 渲染契约，**仍有可验证缺口**：

| # | 缺口 | 严重度 | 代码证据 |
|---|---|---|---|
| P0 | `EventToolEnd` 不填 `Name` / `Args` | **bug** | `translate.go` `tool_execution_end` 只设 ID/Output/Err；`toolExecutionEnd` struct 无 `toolName` |
| P1 | Resume 对 pi 是空操作 | **产品缺口** | `pi.Agent.Start` 忽略 `cfg.ResumeID`；`StartConfig` 注释也写明 Pi ignore |
| P2 | Extension UI / AskUser 不对齐 claude | deferred（F-32 原定） | auto-`cancelled`；`SendPermission` 固定报错 |
| P3 | F-38 Task 清单 | N/A / 可选 | 仅 `claudecode/task.go` 有 `TaskCreate`/`TaskUpdate` |
| P4 | 测试 / 文档漂移 | 卫生 | 无 `New()` session 测；FEATURES/F-32 仍写 designing；Usage 文档与 `Result.Usage` 实现不一致 |

---

## 1. P0 — ToolEnd `Name` + `Args` 对齐

### 1.1 现象（用户可见）

- `OutToolStart`：pi 已填 Name/Args → Feishu call 行 `● bash(...)` **正常**
- `OutToolEnd`：Name 空 → `toolName()` fallback `"tool"` → result 行变成 `🔧 tool → N bytes`，**类型感知摘要失效**
- merge 成功时 call 行仍对，但 result 行坏；orphan End fallback 更糟

证据链：

```
pi/translate.go  tool_execution_end  → ToolEnd{ID, Output, Err}  // Name/Args 零值
gateway/translate.go                 → ToolInfo{Name:"", Args:""}
feishu/adapter.go toolName()         → "tool"
feishu/summarize_tool.go             → default 分支
```

对比 claude：`streamState.toolUseArgs` / `pendingTools` 在 tool_result 时回填 `Name`+`Args`。

官方 pi RPC（`tool_execution_end`）**自带 `toolName`**；args 在 `tool_execution_start`，end 不重复带 args → 必须做 start→end 关联。

### 1.2 方案

在 `translator` 上增加 per-session pending map（与 claude `pendingTools` 同构，更小）：

```go
// translator 新增字段
pendingTools map[string]pendingTool // toolCallId → {Name, Args}

type pendingTool struct {
    Name string
    Args string // raw JSON from tool_execution_start.args
}
```

**`tool_execution_start`**（改）：

1. 现有 emit `EventToolStart` 不变
2. `pendingTools[toolCallId] = {Name: toolName, Args: string(args)}`

**`tool_execution_end`**（改）：

1. `toolExecutionEnd` 增加 `ToolName string \`json:"toolName"\``（官方 wire 有）
2. 填：
   - `ID` = toolCallId
   - `Name` = pending.Name，若空则用 wire `toolName`
   - `Args` = pending.Args（无 pending 则 `""`）
   - `Output` / `Err` 不变
3. delete pending entry（防泄漏）

**`agent_settled`**（可选清理）：清空 `pendingTools`，避免 turn 间 orphan。

不改 Gateway / Feishu —— 它们已经按 `ToolEnd.Name`/`Args` 消费。

### 1.3 文件

| 文件 | 改动 |
|---|---|
| `internal/bridge/pi/protocol.go` | `toolExecutionEnd` 加 `ToolName` |
| `internal/bridge/pi/translate.go` | pending map + start 记录 + end 回填 + settled 清理 |
| `internal/bridge/pi/translate_test.go` | 断言 end.Name / end.Args；orphan end；settled 清 map |

### 1.4 验收

```bash
go test ./internal/bridge/pi -run 'TestTranslate_Tool' -count=1
```

- start→end：`ToolEnd.Name == "bash"`，`Args` 含 `command`
- end 带 wire `toolName`、无 prior start：Name 仍非空
- Feishu 侧不需改测；既有 `summarizeToolResult("bash", …)` 路径自动生效

### 1.5 非目标

- 不在 bridge 里做类型感知摘要（那是 Channel 自治）
- 不改 `OutboundMessage` / Meta（已删除）

---

## 2. P1 — Resume 策略（先决策，再实现）

### 2.1 代码事实

| 层 | 行为 |
|---|---|
| Runtime | `SetResumeID(ev.Init.SessionID)` + 持久化 `agent_sessions.json` |
| Spawn | `Spawn(..., resumeID)` → `StartConfig.ResumeID` |
| claude | `buildArgs` 追加 `--resume <id>` |
| **pi** | **`Start` 完全忽略 `cfg.ResumeID`** |

F-34 写「下次 spawn 带 sessionId」对 pi **不成立**。  
官方 pi 续会话是 `switch_session{sessionPath}`（**文件路径**），不是 sessionId；`get_state` 的 `sessionFile` 也未进入 `InitEvent`（结构体无该字段）。

### 2.2 决策选项（实现前必须拍板）

**Option A — 明确不 resume（最小改动）**

- 保持 `Start` ignore `ResumeID`
- 改文档：`F-34` / `FEATURES` / `StartConfig` 注释写清「pi 进程内多轮；跨进程不续」
- `/new` 仍走 in-process `new_session`（已实现）
- daemon 重启后 pi 永远 fresh（可接受则选 A）

**Option B — 用 sessionFile 续接（对齐 claude 产品语义）**

1. 扩展存储：`AgentSessionEntry` 增加 `ResumePath`（或复用/扩展 ResumeID 语义为 path —— **不推荐混用 id/path**）
2. handshake / `New()` 后的 `get_state`：把 `sessionFile` 写入并 persist
3. `Start` 成功后若 `cfg` 带 path：发 `switch_session` + 再 `get_state` + emit 新 `EventInit`
4. `/new` 对 dead entry 清 `ResumePath`（对称 F-43 清 ResumeID）

**推荐**：先落地 **P0**；P1 默认 **Option A 文档对齐**，除非产品明确要求跨重启续 pi 会话再开 Option B（单独 task / feat）。

### 2.3 Option A 文件（若选 A）

| 文件 | 改动 |
|---|---|
| `docs/feat/F-34-new-slash-command.md` | 删/改「pi: 新 sessionId」spawn 续接表述 |
| `docs/feat/F-32-pi-rpc-bridge.md` | §8/§11 标明跨进程不 resume |
| `docs/FEATURES.md` | F-32 状态 → ✅ 已实现（核心）+ 已知限制 |
| `internal/agent/agent.go` | `ResumeID` 注释保持 Pi ignore，可补一句「sessionFile/switch_session 未接线」 |

### 2.4 Option B 验收（若选 B）

- daemon 重启后同一 `(pi, cwd)` AgentSession 续到同一 sessionFile
- `/new` 后 ResumePath 更新；dead `/new` 清空后下次 fresh
- `go test ./internal/bridge/pi ./internal/chatsession` 覆盖 switch_session mock

---

## 3. P2 — Extension UI（本任务只记，不实现）

| claude | pi（现状） |
|---|---|
| `AskUserQuestion` → `EventPermission` → 飞书卡 → `SendPermission` | `extension_ui_request` → auto cancelled |

实现方向（未来 feat，勿塞进 P0 PR）：

1. bridge：`uiPending map[id]chan decision`
2. 映射 `select`/`confirm` → `EventPermission`
3. `SendPermission` → `extension_ui_response`
4. Gateway/Channel 已有 OutCard 路径可复用

依赖：chat FSM 对 permission 的 pending 列表是否完备（F-32 §11.1）。

---

## 4. P3 — Task 清单

仅当确认 pi 有等价于 `TaskCreate`/`TaskUpdate` 的 tool（或稳定 JSON 约定）再开。  
否则保持 N/A，避免为对齐而硬编码 Claude 工具名。

---

## 5. P4 — 测试与文档卫生（跟 P0 同 PR 或紧随）

### 5.1 测试补齐

| 测试 | 内容 |
|---|---|
| `TestTranslate_ToolExecutionEnd_FillsNameAndArgs` | start→end 关联 |
| `TestTranslate_ToolExecutionEnd_WireToolNameFallback` | 无 pending 时用 end.toolName |
| `TestTranslate_AgentSettled_ClearsPendingTools` | 防泄漏 |
| `TestSession_New_EmitsFreshEventInit` | mock：`new_session` + `get_state` → 新 SessionID（F-34 验收曾要求，当前 `session_test.go` 只有 FullRoundTrip / HandshakeTimeout） |

### 5.2 文档

- `docs/FEATURES.md` F-32：`📝 设计阶段` → `✅ 已实现`（附本 task 已知限制）
- `docs/feat/F-32-pi-rpc-bridge.md`：Status → implemented；§2.3 Usage 改为 `Result.Usage`（与 `translateAssistantMessage` + gateway 一致）；handshake 超时改为 10s（与代码 `handshakeTimeout` 一致）

---

## 6. 实施顺序

```
Phase 1 (本 task 必做)
  └─ P0 ToolEnd Name/Args + 单测
  └─ P4 文档状态修正（FEATURES + F-32 头/Usage/timeout）
  └─ P4 TestSession_New（若 mock 成本低；否则 follow-up）

Phase 2 (拍板后)
  └─ P1 Option A（文档）或 Option B（sessionFile + switch_session）

Phase 3 (独立 feat)
  └─ P2 Extension UI
  └─ P3 Task 清单（若有协议）
```

**禁止**：在 P0 PR 里顺手做 Extension UI / Resume Option B / 抽公共 JSONL transport。

---

## 7. Critical files

```
internal/bridge/pi/protocol.go      # P0
internal/bridge/pi/translate.go     # P0
internal/bridge/pi/translate_test.go
internal/bridge/pi/session.go       # P1-B / P4 New test 夹具
internal/bridge/pi/session_test.go
internal/bridge/pi/agent.go         # P1-B 才改 Start
internal/chatsession/agentsession.go  # P1-B ResumePath
docs/FEATURES.md
docs/feat/F-32-pi-rpc-bridge.md
docs/feat/F-34-new-slash-command.md   # P1-A
```

参考（只读）：

```
internal/bridge/claudecode/stream.go   # pendingTools / toolUseArgs 模式
internal/channel/feishu/summarize_tool.go
internal/gateway/translate.go          # ToolInfo 透传
```

---

## 8. Done 定义

- [ ] P0 合并：`ToolEnd.Name`/`Args` 有单测；`go test ./internal/bridge/pi` 绿
- [ ] P4：FEATURES + F-32 状态/Usage/timeout 与代码一致
- [ ] P1：书面决策 A 或 B；A 则文档改完；B 则另开 task 实现
- [ ] P2/P3：仍标记 deferred，不进本 PR

---

## 9. 参考链接

- [`docs/feat/F-32-pi-rpc-bridge.md`](../docs/feat/F-32-pi-rpc-bridge.md)
- [`docs/feat/F-24-claudecode-bridge.md`](../docs/feat/F-24-claudecode-bridge.md)
- [`docs/feat/F-37-tool-thread-routing.md`](../docs/feat/F-37-tool-thread-routing.md) §3.1（曾要求 pi 填 `ToolEndEvent.Args`）
- [`docs/feat/F-34-new-slash-command.md`](../docs/feat/F-34-new-slash-command.md) §3.2.2
- 官方 pi RPC：`packages/coding-agent/docs/rpc.md`（`tool_execution_end.toolName`、`switch_session.sessionPath`）
