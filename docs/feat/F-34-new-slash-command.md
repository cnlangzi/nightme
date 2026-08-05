# F-34: `/new` Slash Command — Agent Conversation Reset

> **Status**: ✅ **已实现（Phase 3 review 完成，2026-08-04）**。**F-43 supersedes §6 Q-N4 for dead/detached entries**（dead entries 现 is `matched=1, action=marked-fresh`,不再 silently skip）。
> **Milestone**: v1.3.x
> **Depends on**: F-09 (Agent abstraction), F-19 (CLI Bridge), F-21 (Agent Modes), F-24 (Claude Code Bridge), F-27 (ChatSession), F-28 (`/use`), F-29 (AgentSession pool), F-32 (Pi RPC Bridge)
> **Related**: [`SPEC.md`](../SPEC.md) §3.2 状态转换触发器, [`F-28-use-command.md`](./F-28-use-command.md), [`F-29-agent-session-pool.md`](./F-29-agent-session-pool.md), [`F-32-pi-rpc-bridge.md`](./F-32-pi-rpc-bridge.md), [`F-43-kill-new-graceful-and-reset.md`](./F-43-kill-new-graceful-and-reset.md)

---

## 1. Description

`/new` slash command 让用户在不退出 nightme daemon、不杀任何 CLI 进程的前提下，**丢弃 agent 的当前对话上下文**（history / 累积 usage / conversation state），从干净状态重新开始。语义对齐 claudecode 的内置 `/clear` 命令。

```
/new                    → 重置当前 activeCwd 下 pool 里全部 AgentSession
/new <agent>            → 只重置当前 activeCwd 下名为 <agent> 的那一条 AgentSession
```

为什么需要它：

- **Claude Code**：跑久了 context 满、对话偏离、需要清理；`/clear` 是 claude 自身的命令，但用户要在外层触发（IM 里打字），不是再开 TUI 输一次。
- **Pi**：交互模式有 `/new` 内置命令（用户验证过），等价于开启一个全新 session；nightme 把它暴露到 IM 入口。
- **ACP**：`session/new` JSON-RPC 原生支持，无需重启 transport。

不变式（受 SPEC §1.3 约束）：

- **不杀进程**：PTY 模式下 claude 子进程不退出（claude 自己处理 `/clear`）；long-lived 模式下 transport 保持（pi RPC / acp session-over-transport）。
- **不动 `AgentSession` 池身份**：ID / `(agent, cwd)` / args / CreatedAt 全部保留 —— `/use`、`/cwd` 切回老槽位时仍是同一个 AgentSession，只是底层对话状态被 reset。
- **不动 `Events()` chan**：readPump 不需要重启，event 流继续。

---

## 2. Motivation & Problem

### 2.1 v1.3.x 现状

nightme 已有 `/kill`（清空 pool + 杀全部进程）和 `/use`（切 active）。但都"过重"：用户只是想 reset 对话上下文、不想丢 pool 槽位或重启进程。

**场景**：

| 用户场景 | 当前可用命令 | 问题 |
|---|---|---|
| Context 满 / 想重新开始 | `/kill` | 太重：杀掉所有进程 + 清空 pool；下次消息要重新 fork（~500ms-2s），所有挂起消息丢失 |
| 切到另一 agent | `/use` | 语义错：`/use` 是"换 agent"，不是"清上下文"。同 agent 没法 `/use cc`（noop）|
| 只想清 claudecode 的对话 | 无 | 必须 `/kill` 或手动 TUI 输 `/clear` |

### 2.2 设计目标

1. **轻量级 reset**：与 `/kill` 区分 —— 只丢对话，不丢进程 / pool 槽位 / args。
2. **跨 agent 协议统一**：每个 bridge 暴露 `AgentSession.New(ctx) error`，把"reset conversation"的语义收敛到一个方法。
3. **复用现有持久化链路**：bridge reset 后 emit `EventInit`（带新 SessionID）→ 现有 `cmd/nightme/run.go:467` 路径自动捕获 + 持久化。零新增 wiring。
4. **可选精修粒度**：`/new <agent>` 让用户只 reset 一个 agent 的对话，不动其他。

---

## 3. Concept

### 3.1 `AgentSession.New` 接口

新增方法到 `internal/agent/agent.go:494` 的 `AgentSession` 接口：

```go
type AgentSession interface {
    Events() <-chan AgentEvent
    PID() int
    SendText(text string) error
    SendBlocks(ctx context.Context, blocks []ContentBlock) error
    SendPermission(resp string) error
    Close() error

    // New resets the conversation context on the running session.
    // The underlying process (or transport, for long-lived bridges)
    // stays alive. Events() stays open. PID stays the same.
    // Subsequent SendText/SendBlocks operate on the fresh conversation.
    //
    // After New returns, the bridge MUST emit a new EventInit carrying
    // the new SessionID; the runtime's existing eventHandler captures
    // it via SetResumeID and persists. See cmd/nightme/run.go:467.
    //
    // Bridge-specific implementations:
    //   - claudecode: writeLine("/clear")       // stdin slash command
    //   - pi:         send {"type":"new_session"} RPC
    //   - acp:        send "session/new" JSON-RPC over the existing transport
    New(ctx context.Context) error
}
```

**错误契约**：bridge 实现保证 `New` 是 best-effort + 幂等。如果 reset 命令本身被 agent 拒绝（罕见；如 pi 还在处理上一 turn），`New` 返回非 nil error。调用方（`ChatSession.NewActiveAgentSessions`）继续清空其他 AS + InputBuffer，但 reply 里附上 error 信息。

### 3.2 三 bridge 实现

#### 3.2.1 claudecode — 发送 `{"type":"user",...,"content":"/clear"}` (in-process reset)

```go
// internal/bridge/claudecode/session.go
func (s *session) New(ctx context.Context) error {
    payload := []byte(`{"type":"user","message":{"role":"user","content":"/clear"}}`)
    return s.writeLine(payload)
}
```

**实测结论**（F-34 Phase 3 final，2026-08-04 实跑 `claude --print --input-format stream-json --output-format stream-json --verbose --dangerously-skip-permissions --model claude-haiku-4-5`）：

| 试探输入 | 结果 |
|---|---|
| `{"type":"user","message":{"role":"user","content":"Remember 77777"}}` | claude 答 "REMEMBERED" |
| recall | claude 答 "77777" |
| `{"type":"user","message":{"role":"user","content":"/clear"}}` | ✓ 触发 `SessionStart:clear` hook + **新 session_id** + 新 `system/init` |
| recall | claude 答 "NONE"（记忆被清空）|
| `{"type":"control","control":{"type":"clear"}}` | ✗ 静默忽略，session_id 不变 |
| `{"type":"control","control":{"type":"rewind"}}` | ✗ 静默忽略 |
| `{"type":"control","control":{"type":"interrupt"}}` | ✗ 静默忽略（capabilities 列表里有 `interrupt_receipt_v1` 但 stream-json stdin 路径没生效）|
| `{"type":"control","control":{"type":"compact"}}` | ✗ 静默忽略 |
| `{"type":"control","control":{"type":"reset"}}` | ✗ 静默忽略 |
| `{"type":"control","control":{"type":"new_session"}}` | ✗ 静默忽略 |
| `{"type":"control","control":{"type":"exit"}}` | ✗ 静默忽略 |

**关键发现**：
- claude-code 在 stream-json 模式下确实**接受 `/clear` 作为 user-typed slash command**（跟交互模式一样会触发内部 hook + 分配新 session）
- `{"type":"control","control":{"type":"..."}}` 各种 subtype 全部被静默丢弃 —— control 消息在 stdin 上**当前不工作**，尽管 `init.capabilities` 列了 `interrupt_receipt_v1`
- 因此 claudecode.New **不需要**杀进程走 fallback（用户初判是错的；实测证明 in-place reset 可用）
- bridge emit 新 `system/init{ session_id: <new> }` → runtime eventHandler 通过 `SetResumeID` + `PersistAgentSession` 持久化新 ResumeID

#### 3.2.2 pi — `{"type":"new_session"}` RPC

```go
// internal/bridge/pi/session.go
func (s *session) New(ctx context.Context) error {
    return s.rpc.requestAsync("new_session", nil)
}
```

**重要更正**（2026-08-04 经 pi-coding-agent 官方 `docs/rpc.md` 二次确认）：

> pi 在 `--mode rpc` 模式下，**内置 TUI 命令**（包括用户在交互模式看到的 `/new`）**不能通过 `prompt` 触发**。
>
> pi 官方文档明确："Built-in TUI commands (`/settings`, `/hotkeys`, etc.) are not included. They are handled only in interactive mode and would not execute if sent via `prompt`."
>
> 通过 `prompt` 发送的 `/xxx` 命令只包含：extension 注册的、prompt template、skill。

所以 pi 的 reset 必须走 **RPC command `new_session`**（F-32 §1.2 中原本 deferred 的命令）。这是用户看到的 `/new` 在交互模式下的等价物 —— pi 把交互模式的 `/new` 内部映射到同一个 `new_session` RPC。

**协议补充**（F-32 §2.2 表格加一行）：

| command | 方向 | 字段 | 用途 |
|---|---|---|---|
| `new_session` | C→S | `type` | 丢弃当前 session 对话上下文，server 端分配新 sessionId 并 emit `state_update` 等 init 类事件。**不**杀进程。 |

**Translator 补丁（F-34 Phase 3 发现）**：原 F-32 translator 没有处理 `state_update` 事件（落入 default 分支被 log debug 丢弃），runtime 拿不到新 EventInit → ResumeID 不会更新到 `agent_sessions.json`。Phase 3 在 `internal/bridge/pi/translate.go` 加了 case `"state_update"`：解析 `sessionId`（+ 可选 `modelId`/`modelName`/`sessionFile`），emit `EventInit{SessionID: <new>}`，**绕开 `initSent` 守卫**（每次 new_session 都要让 runtime 重新捕获）。runtime eventHandler（`cmd/nightme/run.go newEventHandler`）自身有 `if ev.Init.SessionID != ""` 守卫，重复 init 是幂等的。

**但**：实测 pi-coding-agent 官方 `docs/rpc.md` 二次确认 —— **`state_update` 不在官方事件列表**，`new_session` 响应也**不带新 sessionId**。唯一的获取方式是发完 `new_session` 后再调一次 `get_state`。

**修正实现**（F-34 Phase 3 final，`internal/bridge/pi/session.go::New`）：

```go
// 1. Send new_session, wait for response.
respEnv, err := s.rpc.request(ctx, "new_session", nil, "")
// 2. Inspect data.cancelled (extension may veto the reset).
// 3. Re-arm initSent under s.translatorMu + call get_state.
stateEnv, err := s.rpc.request(ctx, "get_state", map[string]any{}, "")
// 4. Decode get_state.data.sessionId → translator.emitInit → s.deliver
```

translator 里保留的 `case "state_update"` 是**防御性**的（若 pi 未来版本加了 state_update 事件，runtime 仍能拿到 sessionId），但**当前不依赖**。

#### 3.2.3 pty — `ErrRestartRequired` fallback (kill + respawn via Spawner)

PTY 是协议无关的字节管道，没有 "reset conversation context" 的概念（产品澄清 2026-08-04："pty 是删掉进程, 重启进程"）。`ptySession.New(ctx)` 返回 `agent.ErrRestartRequired`；wrapper 层（`chatsession.AgentSession.New`）捕获这个 sentinel 后走 fallback 路径：关掉旧 handle，调 `spawner.Spawn(ctx, agent, cwd, args, "")`（resumeID 为空），把新 handle 装回 `as.handle`，并 `SetResumeID("")` 清掉旧 id。下次 runtime 收到新 child 的 `EventInit` 时会重新捕获新 ResumeID。

```go
// internal/bridge/pty/session.go
func (s *ptySession) New(ctx context.Context) error {
    return agent.ErrRestartRequired
}

// internal/chatsession/agentsession.go (wrapper fallback)
if err := h.New(ctx); !errors.Is(err, agent.ErrRestartRequired) {
    return err  // nil or real error
}
if spawner == nil {
    return agent.ErrRestartRequired
}
_ = h.Close()
as.handle = nil
newHandle, err := spawner.Spawn(ctx, as.Agent, as.Cwd, as.args, "")
if err != nil { return err }
as.handle = newHandle
as.SetRunning(newHandle.PID())
as.SetResumeID("")
```

#### 3.2.4 acp — `session/new` JSON-RPC

```go
// internal/bridge/acp/session.go
func (s *acpSession) New(ctx context.Context) error {
    result, err := s.rpc.request(ctx, "session/new", newSessionParams{
        CWD:        s.workspace,
        MCPServers: []any{},
    })
    if err != nil {
        return err
    }
    // 用新 sessionID 替换；emit 新的 EventInit 让 runtime 持久化
    if err := s.setSessionID(result); err != nil {
        return err
    }
    return nil
}
```

ACP 的 `session/new` 本来就是创建新 session 的命令；当 transport 已存在时，它等价于"在现有 transport 上换 session"。复用现有 `setSessionID` + `emitInit` 路径，**无需拆 transport**（与 F-34 Phase 1 的设计一致；Phase 2 才考虑更激进的 transport 复用重构）。

### 3.3 `chatsession.AgentSession.New` wrapper + fallback

```go
// internal/chatsession/agentsession.go
//
// Signature is New(ctx, spawner Spawner). spawner is the chat's
// configured Spawner; nil-safe for bridges that handle reset in-place
// (pi / claudecode / acp); required for the pty fallback path.
func (as *AgentSession) New(ctx context.Context, spawner Spawner) error {
    as.handleMu.Lock()
    defer as.handleMu.Unlock()

    h := as.handle
    if h == nil {
        return ErrNotRunning   // Detached / Exited
    }
    if err := h.New(ctx); !errors.Is(err, agent.ErrRestartRequired) {
        return err  // nil (success) or real error (propagate)
    }
    if spawner == nil {
        return agent.ErrRestartRequired
    }
    _ = h.Close()
    as.handle = nil
    newHandle, err := spawner.Spawn(ctx, as.Agent, as.Cwd, as.args, "")
    if err != nil {
        return fmt.Errorf("chatsession: restart %s at %s: %w", as.Agent, as.Cwd, err)
    }
    as.handle = newHandle
    as.SetRunning(newHandle.PID())
    as.SetResumeID("")
    return nil
}
```

### 3.4 `ChatSession.NewActiveAgentSessions` 批量方法

```go
// internal/chatsession/chatsession.go
func (cs *ChatSession) NewActiveAgentSessions(ctx context.Context, agentName string) (matched, reset int, firstErr error) {
    cs.mu.RLock()
    cwd := cs.activeCwd
    if cwd == "" {
        cs.mu.RUnlock()
        return 0, 0, nil   // caller replies "send /cwd first"
    }
    cs.mu.RUnlock()

    // 1. 收集 RUNNING targets（filter by cwd + optional agent + Status）
    cs.mu.RLock()
    targets := make([]*AgentSession, 0)
    for _, as := range cs.pool {
        if as.Cwd != cwd { continue }
        if agentName != "" && as.Agent != agentName { continue }
        if as.Status() != StatusRunning { continue }   // 只看 Running
        targets = append(targets, as)
    }
    cs.mu.RUnlock()

    if len(targets) == 0 {
        return 0, 0, nil
    }

    // 2. 串行 reset (避免 stdin / RPC 交错)
    for _, as := range targets {
        matched++
        if err := as.New(ctx); err != nil {
            if firstErr == nil { firstErr = err }
            continue
        }
        reset++
    }

    // 3. 清空 InputBuffer queued messages
    if cs.inputBuffer != nil {
        cs.inputBuffer.Clear()
    }
    return matched, reset, firstErr
}
```

**行为锁**：
- 串行 reset：3 个 AS 排队发，~3× 单次延迟；total 通常 < 50ms。
- **Status == StatusRunning 是必备过滤**（产品澄清 2026-08-04）：未启动的 AS 没有对话上下文，不应被启动后再 reset；直接跳过、静默不计 matched。
- InputBuffer 清空**总是**触发（在 reset 失败/部分失败时也清）—— 用户决策 #3。
- **不动** `currentTurnUserMsgID`：下一条 user msg 自然开新 turn + 新 anchor + Channel 冷开新 receipt。

---

## 4. `/new` Slash Command

`internal/gateway/handlers_new.go`（对应 `handlers_watch.go` 风格）：

```go
func handleNew(ctx context.Context, mgr *chatsession.Manager, channel Channel,
               msg *InboundMessage, args []string, globalPrimary string) (*CommandResult, error) {

    cs := mgr.GetOrCreate(msg.ChatID, globalPrimary)
    if cs.ActiveCwd() == "" {
        return reply(ctx, channel, msg.ChatID, "No active workspace. Send /cwd <path> first."), nil
    }

    agentName := ""
    if len(args) > 0 {
        agentName = strings.TrimSpace(args[0])
        if agentName == "" {
            return reply(ctx, channel, msg.ChatID, "Usage: /new [<agent>]"), nil
        }
    }

    matched, reset, err := cs.NewActiveAgentSessions(ctx, agentName)

    if matched == 0 {
        if agentName != "" {
            return reply(ctx, channel, msg.ChatID,
                fmt.Sprintf("No agent session for %q in current workspace. Try /agents.", agentName)), nil
        }
        return reply(ctx, channel, msg.ChatID,
            "No agent session in current workspace to reset. Send a message to start one."), nil
    }

    text := fmt.Sprintf("Reset %d/%d agent session(s).", reset, matched)
    if err != nil {
        text += fmt.Sprintf(" (errors: %v)", err)
    }
    return reply(ctx, channel, msg.ChatID, text), nil
}
```

注册到 `internal/gateway/handlers_chatsession.go::RegisterChatSessionCommands`：

```go
gw.Register(gateway.Command{
    Name:        "new",
    Description: "Reset conversation context. /new for all sessions in current workspace, /new <agent> for one.",
    Handler: func(ctx context.Context, msg *InboundMessage, args []string) (*CommandResult, error) {
        return handleNew(ctx, mgr, channel, msg, args, globalPrimary)
    },
})
```

### 4.1 语义对照表

| 命令 | 过滤 | 清空的 AS | 清空 InputBuffer | 杀掉进程 |
|---|---|---|---|---|
| `/new`（默认）| `Cwd == activeCwd`（无 agent 过滤）| pool 中 activeCwd 下全部 | ✓ | ✗ |
| `/new <agent>` | `Cwd == activeCwd && Agent == <name>` | 至多 1 条 | ✓ | ✗ |
| `/kill` | 整个 pool | 全部 | ✓ | ✓ |
| `/use <agent>` | 无 | 无（只切 activeAS）| ✗ | ✗ |

### 4.2 `/new <agent>` 的 cwd 范围（决策锁）

**为什么限定 activeCwd**：

- 与 `/new`（无参）保持对称 —— 两者都在当前 workspace 作用域内 reset。
- 避免"在 /A reset /B 的 session"的反直觉行为 —— 用户已经在 /A 工作，不应该莫名其妙影响 /B 的 agent 对话。
- 如果用户想 reset 另一 cwd 的 AS，先 `/cwd` 切换，再 `/new <agent>` —— 显式胜过隐式。

### 4.3 持久化副作用

`/new` 后 bridge emit `EventInit{ SessionID: newID }` → 现有 `newEventHandler`（`cmd/nightme/run.go:467`）走：

```go
if ev.Kind == agent.EventInit && ev.Init != nil && ev.Init.SessionID != "" {
    s.SetResumeID(ev.Init.SessionID)
    if mgr != nil {
        if err := mgr.PersistAgentSession(s); err != nil && logger != nil {
            logger.Warn("persist agent session (init) failed", ...)
        }
    }
}
```

→ `s.SetResumeID(newID)` 覆盖旧的 `ResumeID` 字段  
→ `mgr.PersistAgentSession(s)` 把新 `ResumeID` 写盘到 `agent_sessions.json`

**零 schema 改动**。下次 daemon 重启时，spawn 携带新 `ResumeID`（claudecode: `--resume <newID>`，pi: 新 sessionId，acp: 新 sessionId），agent 看到 reset 后的上下文。

---

## 5. 不变式 checklist

| 不变式（来自 SPEC §1.3）| 影响 |
|---|---|
| `Chat ↔ ChatSession` 1:1 | `/new` 不动 binding；✓ |
| `(agent, cwd)` per ChatSession 1:1（Q13）| AgentSession.ID/Cwd 不动；✓ |
| `agentSession.Events()` 单消费者是 readPump（Q14）| Events() 不关，readPump 不动；✓ |
| `ChatSession` 不 import channel/ | 仍满足；✓ |
| `currentTurnUserMsgID` 单数 | 不清；新 turn 自然覆盖；✓ |
| OutboundMessage.ReplyTo = currentTurnUserMsgID | Channel 冷开新 receipt；✓ |
| Message.HasMention + WatchMode（F-watch）| 与 `/new` 无关；✓ |

**新增不变式**：

> **`AgentSession.New` 契约**：调用后 `Events()` 保持 open；`PID()` 不变；底层进程不退出；后续事件属于**新**对话。bridge 必须 emit 新 `EventInit{SessionID}`；runtime 自动持久化。

---

## 6. 错误处理矩阵

| 场景 | 行为 |
|---|---|
| `activeCwd == ""` | reply "No active workspace. Send /cwd <path> first." |
| `/new` 命中 0 条 AS | reply "No agent session in current workspace to reset." |
| `/new <agent>` 找不到 | reply "No agent session for <agent> in current workspace. Try /agents." |
| 单个 AS reset 失败（如 pi 还在处理 turn）| reset 计数 -1；InputBuffer **仍清空**；reply 附 "errors: <first err>" |
| 所有 AS reset 失败 | matched > 0, reset == 0；reply "Reset 0/N agent session(s). (errors: ...)" |
| pool 有 AS 但都未启动 (Status==Detached/Exited) | **F-43 ⚠ supersedes**: matched == 1, reset == 1, Action == `marked-fresh`; ResumeID cleared in-memory + persisted; reply 用 `FormatResetResults` per-entry list。原 F-34 "Q-N4 silently skip" 行为已被替换。 |
| pool 在 activeCwd 下完全为空 | matched == 0；reply 同上 |

---

## 7. 测试计划

| 文件 | 测试 |
|---|---|
| `internal/bridge/claudecode/claudecode_test.go` | `New()` 写 `/clear\n` 到 mock stdin；writeLine 锁不与 SendBlocks 竞争 |
| `internal/bridge/pi/session_test.go` | `New()` 发 `{"type":"new_session"}`；mock RPC server 返回新 sessionId；验证后续 EventInit 带新 SessionID |
| `internal/bridge/acp/acp_test.go` | `New()` 发 `session/new`；验证 `s.sessionID` 被替换 + EventInit emit |
| `internal/chatsession/new_test.go` | `ChatSession.NewActiveAgentSessions`：filter / 计数 / InputBuffer.Clear / Status 跳过 / firstErr 聚合 |
| `internal/chatsession/agentsession_test.go` | `AgentSession.New` delegate：handle=nil 返回 ErrNotRunning |
| `internal/gateway/handlers_new_test.go` | `handleNew` 命中 + 空 pool 报错 + `/new <agent>` 找不到 + 部分失败 reply |

---

## 8. 不在范围内（Out of Scope）

- **`/new` 跨 cwd reset**：`/new <agent>` 限定 activeCwd；reset 其他 cwd 的 AS 由用户 `/cwd` 切换触发（决策 §4.2）。
- **bridge 层的并发 reset 优化**：当前串行；如有性能诉求可在 Phase 3 加 per-bridge 并发（注意 stdin / RPC 各自串行约束）。
- **ACP transport 复用重构**：当前每次 `New` 都复用 transport 已有 session/new；不抽公共 transport（与 F-32 / F-21 Phase 2 一致）。
- **`/new` 清空用户消息历史**：仅清 agent 对话上下文 + InputBuffer queued；不删 Channel 端已发的 user message（Channel 自管 receipt，与 `/new` 正交）。
- **UI 反馈**：`/new` 只 reply 一行文本；不触发额外 OutboundMessage / reaction（与 `/watch`、`/kill` 一致）。
- **`/reset` 别名**：暂不提供；如用户想要，`/new` 已被锁定。

---

## 9. 决策记录

| # | 决策 | 结论 | 日期 |
|---|---|---|---|
| Q-N1 | `New` 放哪个接口 | `agent.AgentSession` 接口（不是 `agent.Agent`）| 2026-08-04 |
| Q-N2 | `/new` 无参的清空范围 | pool 中 `Cwd == activeCwd` 的全部 | 2026-08-04 |
| Q-N3 | `/new <agent>` 的清空范围 | pool 中 `Cwd == activeCwd && Agent == <name>`（限定 cwd，对称）| 2026-08-04 |
| Q-N4 | InputBuffer 处理 | **清空** queued（与 `/kill` 行为对齐）| 2026-08-04 |
| Q-N5 | claudecode reset 命令 | `writeLine({"type":"user","message":{...,"content":"/clear"}})` —— claude-code 在 stream-json 模式下接受 `/clear` 作为 user-typed slash command（实测 2026-08-04）；控制消息 `{"type":"control",...}` 各种 subtype 全部无效 | 2026-08-04 |
| Q-N6 | pi reset 协议 | **RPC command** `{"type":"new_session"}`（不是 prompt 文本 `/new`）| 2026-08-04 |
| Q-N7 | acp reset 协议 | JSON-RPC `session/new` over existing transport | 2026-08-04 |
| Q-N8 | `AgentSession` ID 是否换 | **不换** —— pool 槽位稳定 | 2026-08-04 |
| Q-N9 | ResumeID 更新时机 | bridge emit EventInit 后，runtime 走现有路径自动持久化 | 2026-08-04 |
| Q-N10 | handle / readPump 处理 | 不动；bridge 在原 transport 上发 reset，Events() 不关 | 2026-08-04 |

---

## 10. 实施清单（Phase 2）

| 文件 | 改动 | 行数估算 |
|---|---|---|
| `internal/agent/agent.go` | `AgentSession` 接口加 `New(ctx) error` | +10 |
| `internal/bridge/claudecode/session.go` | 实现 `New` | +6 |
| `internal/bridge/pi/session.go` | 实现 `New`（RPC requestAsync）| +12 |
| `internal/bridge/acp/session.go` | 实现 `New`（session/new + setSessionID）| +20 |
| `internal/chatsession/agentsession.go` | `New` delegate | +10 |
| `internal/chatsession/chatsession.go` | `NewActiveAgentSessions` | +30 |
| `internal/gateway/handlers_new.go` | 新文件 + `handleNew` | +60 |
| `internal/gateway/handlers_chatsession.go` | 注册 `/new` 命令 | +8 |
| 测试 6 文件 | bridge + chatsession + gateway | +200 |
| **合计** | | **~360** |

实施顺序：

1. 改 `agent.AgentSession` 接口（编译会断 3 个 bridge；先 stub）
2. 三 bridge 各自实现 `New`（claudecode 最简；acp 次之；pi 需要 RPC schema 验证）
3. chatsession 包装 + `NewActiveAgentSessions`
4. handlers_new.go + 注册
5. 测试