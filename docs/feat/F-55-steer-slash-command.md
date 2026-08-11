# F-55: `/steer` Slash Command

> **Status**: ✅ implemented on `feat-steer` branch.
> **Milestone**: v1.3
> **Depends on**: F-09 (Agent abstraction), F-19 (CLI Bridge), F-21 (Agent Modes), F-24 (Claude Code Bridge), F-27 (ChatSession), F-29 (AgentSession pool), F-43 (`/close` rename + AgentSession preservation), F-53 (Message/Prompt 抽象化), F-54 (event bus).
> **Related**: [`F-43-close-new-graceful-and-reset.md`](./F-43-close-new-graceful-and-reset.md), [`F-21-agent-modes.md`](./F-21-agent-modes.md).

## 0. 摘要

把 `/steer <message>` 接成 IM 入口。语义是 **"stop + 插队"** —— 试图终止当前 in-flight turn（通过 bridge 的 `Stop` 原语），然后把新消息插到输入队列头部。下次 turn 启动时，agent 第一眼看到的是这条 steered 消息，即使队列里已经有别的用户消息在等。

落地的两条改动：

1. `MessageQueue.PushFront(msg)` —— 队列的新原语，跟 `Push` 对称，把 `msg` 插到 pending 区域的头部。
2. `ChatSession.SteerUserMessage(msg)` —— runtime 入口，封装 `Stop` + `PushFront`。

不动 FSM（不加 `Steering` 态），不动 bridge wire（每个 bridge 的 `Stop` 已经存在），不动 IM adapter（`/steer` 走原生的 slash command 调度）。

## 1. 背景与决策

### 1.1 为什么"插到队首"

设计动机是解决一个用户故事：用户在某个 agent turn 跑着的时候意识到方向错了，发了一条"换个方向"的消息，然后立刻又发了几条后续追问。这时候：

- 当前 turn 还在跑，不能简单"丢掉当前 turn 然后发新消息" —— 模型已经在做事，丢掉它的进度是浪费。
- 但后续追问已经在队列里排着；如果只把新消息 Push 到队尾，会变成"先执行旧追问、再看到方向纠正"，体感很差。
- 用户的"最近意图"应当压过"历史意图"。

最朴素的实现：等当前 turn 自然结束，新消息走正常 flush 路径。但用户等不了那么久（特别是某些 bridge 的 `Stop` 可以毫秒级打断），所以增加加速路径。

最终语义："**插队**"。具体怎么做：

- 调 `as.Stop(ctx)` fire-and-forget。Stop 是异步的，可能走 RPC（`pi` 的 `{"type":"abort"}`）、HTTP（`opencode` 的 `/interrupt`）、SIGINT（`claudecode` / `codex`）、PTY interrupt（`acp`）、返回 `ErrNotSupported`（`pty`）等。**结果忽略**，只要它"有助于早一点让 busy guard 释放"就行；不阻塞 steer 消息入队。
- 把 steer 消息插到 `queue` 的 pending 头部。下次 `Peek` 时它会跟其他 pending 一起被打包，但保证排在第一位。

### 1.2 为什么不动 FSM

考虑过加一个 `Steering` 状态，记录"正在被 steer 中"的信息。但所有这些信息都可以从现有状态推导：

- in-flight？查 `cs.SelectedAgentSession().IsReady()`。
- 队列头是什么？看 `cs.queue.Peek()` 的下一个 item。
- 用户最近意图？消息内容已经在 steer 那条 Message 里。

加 FSM 只会引入 corner case（`Steering → Idle` 的迁移条件、`Steering` 期间能不能再 steer 一次等），不解决问题。runtime 把 Stop 和 PushFront 串起来就够了。

### 1.3 为什么不动 Feishu adapter

第一版只在 IM 卡片占位 + slash command 调度上动手。飞书卡片上目前没有"✋ 重定向"按钮，所以 steer 入口只能是用户主动输入 `/steer <msg>`。等 MVP 落地、看用户反馈后再考虑加按钮。

## 2. 设计

### 2.1 `MessageQueue.PushFront` —— 队列新原语

`internal/chatsession/message_queue.go` 在 `Push` 旁边新增 `PushFront`，签名对称（`func (q *MessageQueue) PushFront(msg Message) error`），容量检查 + 零值 ID no-op 一致。

实现层面的不变式（跟 `Push` 共享）：

- head..inFlightEnd (exclusive) = in-flight
- inFlightEnd..tail (inclusive) = pending

`PushFront` 把 `msg` 插到 pending 头部（即 inFlightEnd 位置），并把 inFlightEnd 移到 `msg` 自己，保持不变式。

边界情况：

| 入队前状态 | 行为 |
|---|---|
| 空队列 | head/tail/inFlightEnd 都指向 `msg`（跟 `Push` 在空队列上的形态一致） |
| 整个队列 in-flight | `msg` append 到 tail；inFlightEnd 移到 `msg`（跟 `Push` 的 all-in-flight 边界一致）—— 因为此时没有 pending item 可"插前面"，行为退化为 append，但语义正确：`msg` 是下一个 turn 的第一条 |
| 部分 in-flight + 部分 pending | `msg` 插到 inFlightEnd 之前；inFlightEnd 移到 `msg`（新 pending head） |

所有路径下 `PushFront` 的行为 = "下一个 `Peek` 返回的第一个 item 是 `msg`"，这是 steer 想要的保证。

### 2.2 `ChatSession.SteerUserMessage`

```go
func (cs *ChatSession) SteerUserMessage(msg Message) error {
    if msg.ID == "" {
        return nil
    }
    // Step 1: try to abort the current in-flight turn.
    if as := cs.SelectedAgentSession(); as != nil {
        if h := as.Handle(); h != nil {
            _ = h.Stop(cs.Context()) // fire-and-forget
        }
    }
    // Step 2: prepend msg to the pending region.
    return cs.queue.PushFront(msg)
}
```

跟 `QueueUserMessage` 的差别：

| | QueueUserMessage | SteerUserMessage |
|---|---|---|
| 触发 `Stop` | ❌ | ✅（fire-and-forget） |
| 队列位置 | 尾部 | 头部 |
| 调用 `TryFlush` | ✅（空队列时立即 flush） | ❌（让 FlushHook 在 idle 后自然 flush） |

`SteerUserMessage` **不主动调 `TryFlush`** 是有意为之：当前 turn 还在跑，flush 没意义。让 FlushHook 在 `Stop` 触发的 idle 事件到达后自然触发 —— 跟 `QueueUserMessage` 的"等 KindPromptEnded 再 flush"路径对齐。

### 2.3 `internal/command/steer/cmd.go` —— IM 入口

跟 `/close`、`/stop` 的形态对齐：

```go
type Factory struct{ mgr *chatsession.Manager }

func (f *Factory) Spec() command.Spec {
    return command.Spec{
        Name:     "steer",
        Summary:  "Stop the in-flight turn and prepend <message> to the queue.",
        Usage:    "/steer <message>",
        Category: "session",
    }
}

func (f *Factory) Handle(...) (*command.SlashOutput, error) {
    // 1. CS lookup + RequireActiveCwd preflight
    // 2. TrimPrefix("/steer") on input.Text → 保留多行 / 多个空格
    // 3. 空 body → "Usage: /steer <message>"
    // 4. Build Message{Kind: MessageKindNormal, Blocks: [text]}
    // 5. cs.EmitMessageState(MessageQueued) BEFORE SteerUserMessage
    //    （跟 QueueUserMessage 的 timing contract 一致）
    // 6. cs.SteerUserMessage(msg)
    // 7. Reply "🛑 Steering: <preview>"  ← 80 字符截断
}
```

注册到 `cmd/nightme/run.go`（在 `/stop` 后、`/new` 前）。

### 2.4 Per-bridge 行为对照

调 `cs.SteerUserMessage(msg)` 时，`Stop` 在每个 bridge 上的效果：

| Bridge | Stop 原语 | 触发后行为 | sessionID | Steer 后 idle 延迟 |
|---|---|---|---|---|
| `pi` | `{"type":"abort"}` JSON-RPC | `agent_settled` 事件 | 保留 | 几十 ms |
| `opencode` | `POST /api/session/{id}/abort` HTTP | `session.idle` | 保留 | 几十 ms |
| `claudecode` | SIGINT 到子进程 | 视实现 — 可能发 `result{is_error:true}` 也可能直接 exit | 保留（如果 exit，下次 spawn 用 `--resume <id>` 续上） | 几百 ms ~ 1s |
| `codex` | SIGINT 到 app-server | 同 claudecode | 保留 | 几百 ms ~ 1s |
| `acp` | PTY interrupt | 取决于具体 ACP agent | 视 agent | 不定 |
| `pty` | `ErrNotSupported` | 不动作 | 保留 | 等当前 turn 自然结束 |

注：上表"sessionID 保留"对所有 bridge 都成立 —— F-43 把 `/close` 的 AgentSession clearing 移除了，steer 的 `Stop` 不动 AgentSession，两条路径都不会触发 `DropAgentSession`。

### 2.5 用户体感时序

```
t0  用户发消息 M1
    → CS.QueueUserMessage(M1)
    → AS.IsReady()=true → TryFlush → AS.Submit → isReady=false
    → bridge 收到 M1，开始 turn

t1  用户发消息 M2（普通 follow-up）
    → CS.QueueUserMessage(M2)
    → AS.IsReady()=false → TryFlush 跳过 → M2 进队列尾部

t2  用户发 "/steer 改做另一件事"
    → CS.SteerUserMessage(M_steer)
        1) as.Stop(ctx) → 异步加速 idle
        2) queue.PushFront(M_steer)
    → 当前 turn 还在跑，steer 消息排在 M2 之前

t3  Stop 生效 → bridge 发 terminal 事件 → busy guard 释放
    → FlushHook → TryFlush
    → queue.Peek() → 返回 [M_steer, M2]（一个 Normal batch）
    → AS.Submit → 用户看到的下一条 prompt 是 steer 消息
    → 之后 M2 跟着被处理
```

## 3. 测试

### 3.1 `MessageQueue.PushFront` —— 6 个 case

`internal/chatsession/message_queue_test.go` 新增：

- `TestMessageQueue_PushFrontEmpty` —— 空队列上 PushFront
- `TestMessageQueue_PushFrontBeforePending` —— 队列只有 pending，PushFront 把 item 放到 head
- `TestMessageQueue_PushFrontDuringInFlight` —— 整个队列 in-flight，PushFront 走 append 路径
- `TestMessageQueue_PushFrontMixedInFlight` —— 部分 in-flight + 部分 pending，PushFront 插在中间
- `TestMessageQueue_PushFrontCapacity` —— 容量上限
- `TestMessageQueue_PushFrontZeroID` —— 零值 ID 是 no-op
- `TestMessageQueue_PushFrontBarrier` —— barrier 语义保留（Normal merge / Queue 自成 batch）

### 3.2 `/steer` factory —— 7 个 case

`internal/command/steer/cmd_test.go` 新增：

- `TestFactory_Spec` —— `Name="steer"` + Usage 提到 `<message>`
- `TestFactory_Handle_NoSession_RepliesNoActive`
- `TestFactory_Handle_NoActiveCwd_RepliesHint`
- `TestFactory_Handle_EmptyBody_RepliesUsage`
- `TestFactory_Handle_PrependsMessage` —— 单消息队列增长 1
- `TestFactory_Handle_MultiWordBody` —— 空格分词的 multi-token body 通过 Text 完整保留
- `TestFactory_Handle_QueueGrows` —— 已有 follow-up 时再 /steer，队列增长到 2

`MessageQueue.PushFront` 的"插到队首"语义通过 3.1 的 case 直接验证，factory 层不重复测。

## 4. 不需要做（明确划掉）

- ❌ 不加 per-bridge 的 native steer 原语（opencode `/interrupt + /message` 拼接、pi `steer` RPC、codex `turn/steer`）—— 这条路属于另一个 feature，不是 runtime steer 的替代
- ❌ 不改 FSM（不引入 `Steering` 态）—— Stop + FlushHook 已足够
- ❌ 不动 Feishu adapter —— 第一版只走 `/steer` slash command 输入
- ❌ 不改 `claudecode` 的 Stop 实现（继续 SIGINT，sessionID 保留靠 respawn + `--resume` 续上）
- ❌ 不改 `MessageQueue.Peek` / `Commit` / `Rewind`（PushFront 是新写一个对称 API）

## 5. 命令对照（v1.3 + F-43 + F-55）

| 命令 | 进程 | AS entry | sessionID | 队列位置 | 用例 |
|---|---|---|---|---|---|
| `/stop` | 可能死 | 保留 | 保留 | 不动 | "我改主意了，等下让 agent 看到 |
| `/close` | 死（graceful） | 保留 | 保留 | 不动 | "bridge 卡死了，重启 bridge" |
| `/new` | in-place reset | 保留 | 清空 | 不动 | "清掉对话历史，重新开始" |
| `/steer` | 可能中断 | 保留 | 保留 | **插到队首** | "停一下，按新方向继续" |

四者覆盖用户对 agent turn 的四种粒度控制。

## 6. 实施记录

2026-08-11 在 `feat-steer` 分支上完成：

- `MessageQueue.PushFront` 实现 + 7 个测试
- `ChatSession.SteerUserMessage` 实现
- `internal/command/steer/cmd.go` 工厂
- `cmd/nightme/run.go` 注册（`/stop` 后、`/new` 前）
- 本 doc 文件