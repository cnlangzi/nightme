# me — 程序侧契约：settled 信号与 Outcome

> **Scope**: 每条 inbound 消息的 settled 信号、`Outcome` 词汇表与合并规则、各命令的 settle 时机
> **读者**: 编写驱动 nightme 的程序（`me`）的工程师
> **Related docs**:
> - [SPEC.md](./SPEC.md) 运行时与 gateway 架构
> - [feat/F-gateway.md](./feat/F-gateway.md) inbound 分发链
> - [CHANNEL.md](./CHANNEL.md) 多 channel 与消息渲染

---

## 0. 摘要

IM channel 上的反馈是给人看的自由文本卡片。程序驱动 nightme 时，卡片无法回答两个问题：

1. 这条消息**处理完了吗**？
2. 它**怎么结束的**？

settled 信号是与之并行的机器通道：每条 inbound 消息对应**恰好一条** `OutMessageSettled`，携带一个结构化的 `Outcome`。程序按 `Status` 分支，不解析文本。

settled 信号**不渲染**——所有 IM channel 静默丢弃它（见 §6）。它只在 channel 之外可见。

---

## 1. 消息形态

settled 是一条普通的 outbound message，`Kind` 为 `OutMessageSettled`（`String()` 为 `message_settled`）：

```go
type OutboundMessage struct {
	ChatID  string          // 消息所属的 chat
	Kind    OutboundKind    // OutMessageSettled
	ReplyTo string          // 这条 inbound 消息的 msgID
	Outcome *Outcome        // 结构化结论；其他 Kind 为 nil
}
```

`ReplyTo` 是 inbound 消息的 id，也就是程序自己发出去的那条消息的 id——程序据此把自己的请求和结论对上。

`Outcome`：

```go
type Outcome struct {
	Status  OutcomeStatus    // 机器可读的判定
	Summary string            // 一行人类可读的复述
	Data    map[string]string // 机器可读的事实（命令各自定义）
}
```

`Data` 的键由产生它的命令定义。当前在用的键：

| 键 | 含义 |
| --- | --- |
| `end_reason` | 回合结束原因（`clean` / `user-stopped` / `user-killed` / …） |
| `reason` | 结束原因分类（见 §7.1） |
| `agent` | 跑这件事的 agent 名 |
| `issue` | `/gtw fix` 处理的 issue 号（本地模式为 `-1`） |
| `branch` | 分支名 |
| `worktree` | worktree 路径 |
| `repo_root` | 主仓库根目录 |
| `mode` | `/gtw fix` 的模式（`remote` / `local`） |
| `topic` | `/gtw plan` 的主题 |
| `sha_range` | commit 区间（`<before>..HEAD`） |
| `commits` | commit 条数（`/gtw commit` 与 `/gtw push` 给出确切条数；`/gtw sync` 的卡片只列前 10 条，所以它不给这个键） |
| `remote_head` | push 之后 `origin/<branch>` 指向的 sha（读不到时这个键不出现） |
| `pr_number` / `pr_url` | PR 的编号 / 链接 |
| `groups` | review 跑了几组（`/review`） |
| `resumed_groups` | 续跑恢复了几组（`/review --continue` 只跑没完成的那几组） |
| `unfinished_groups` | 其中没跑完的组数 |
| `subcommand` | 被 `/gtw abort` 取消的子命令 |
| `elapsed` | 被取消的子命令已经跑了多久 |
| `repo` | 仓库（`owner/repo`） |
| `issues` | `/gtw new -y` 开出来的 issue 编号（逗号分隔） |
| `kept` | 还没开成 issue 的草稿数 |
| `lang` / `scope` | `/gtw config` 的语言与作用域（`global` / `repo` / `default`） |
| `unset` | `/gtw config --unset` 删掉了这个设置（值是 `true`） |
| `cwd` | `/cwd` 设好的工作区路径 |

`Data` 刻意是 `map[string]string` 而不是 struct：不同命令产出不同的事实，程序只读自己关心的键。

---

## 2. Outcome 词汇表

| Status | 含义 |
| --- | --- |
| `ok` | 按要求完成了 |
| `noop` | 没有可做的事——请求有效，但世界已经满足它 |
| `incomplete` | 做了一部分，剩下的没做完 |
| `failed` | 没做完 |
| `cancelled` | 被用户或操作者中止（`/stop` 停掉的回合、`/gtw abort` 取消的命令） |
| `timeout` | 跑超时了（**保留**，当前没有命令产出它） |
| `unknown` | 没有任何一方上报过结论 |

`unknown` 是零值语义：一条没人上报过的消息以此状态 settle；合并时也只有在**没有任何一方上报**的情况下才产生它。

`/gtw`、`/review`、`/cwd`、`/use`、`/stop` 的每一条退出路径都会上报一个非 `unknown` 的结论。`/close`、`/queue`、`/steer`、`/think`、`/tools`、`/watch`、`/new`、`/resume`、`/handoff`、`/wiki` 仍然 settle 成 `unknown`。

---

## 3. 合并规则

一条消息可能有多方参与（命令本身、它排入的 agent 回合），每方的结论合并成一条。

**严重度**（高的胜出）：

```
failed > cancelled > timeout > incomplete > ok > noop > unknown
```

**Status**：取更严重的一方；`unknown` 只在都没人上报时出现。

**Data**：逐键合并，后上报的覆盖先上报的；只有一方拥有的键保留。

**Summary**：取胜出方的；严重度相同时先上报的胜出（命令的专属结论不被后到的通用结论顶掉）；胜出方没有 Summary 时用另一方的。

**prompt-end 的兜底语义**：回合干净结束映射成 `ok`，它是**通用兜底**信号——命令已经上报过结论时，它不覆盖那个结论（命令报 `noop`，最终仍是 `noop`）。回合非正常结束（失败 / 进程死 / 被杀）永远覆盖：失败是新信息。

---

## 4. settle 的时机

settled 不是"命令返回时"发，是"**这条消息引发的工作全部结束**"时发。

### 4.1 hold 模型

每条消息有一个共享的 `Work`，参与方各自持一份 hold：

- `cs.Hold(msgID)` 取一份 hold；
- `Work.Report(outcome)` 把结论并进这条消息；
- `Work.Release()` 放掉一份 hold；
- **最后一份 hold 放掉时**，通过 `cs.Emitter()` 发出**一条** `OutMessageSettled`。

同一个 msgID 的多方拿到的是同一个 `Work`，各自加一份 hold——所以"命令 + 它排入的回合"两方都结束时才 settle。

### 4.2 等回合结束

`cs.HoldUntilPromptEnd(msgID)` 持一份 hold，直到承载这条消息的 agent 回合结束。回合结束原因映射成结论：

| 回合结束原因 | Status |
| --- | --- |
| 干净结束 | `ok` |
| 用户停止 / 用户杀死 | `cancelled` |
| 其他（错误、进程死、卡死被杀） | `failed`（带 `Data["end_reason"]`） |

自动持 hold 的入口：

- `cs.QueueUserMessage` —— 排入队列的消息；
- `cs.SteerUserMessage` —— 被 steer 的文本；
- `/review` 与 `/review --continue` —— review 本身，以及它把 findings 注入进去的那个回合。

### 4.3 命令自身的 hold

命令分发（`inbound.runCommand`）在派发期间持一份 hold，**在命令回复发出之后**才放掉——settled 永远排在命令回复之后。

`/review` 的 hold 在 `Handle` 里**同步**取（不能在 goroutine 里取：那会晚于分发器的 release，导致消息在 review 开始前就 settle 成 `unknown`），goroutine 退出时放掉。

---

## 5. 各命令的 settle 时机

| 命令 | agent 工作怎么跑 | settle 落在 |
| --- | --- | --- |
| 普通提问 | 会话自己的回合 | 回合结束 |
| `/queue` `/resume` | 排入队列 → 会话回合 | 回合结束 |
| `/gtw fix -y` `/gtw new` `/gtw plan` | 排入队列 → 会话回合 | 回合结束 |
| `/handoff` `/wiki init` | 排入队列 → 会话回合（命令自己等到回合结束） | 回合结束 |
| `/steer` | 文本进队首 → 下一个回合 | 承载该文本的回合结束 |
| `/gtw commit` `/gtw pr` | `agent.RunOnce` 同步跑在命令里 | 命令返回时（agent 已跑完） |
| `/gtw push` | 不跑回合（纯 git push） | 命令返回时 |
| `/gtw close` `/gtw back` `/gtw config` `/gtw sync` `/gtw abort` | 不跑回合 | 命令返回时 |
| `/cwd` `/use` `/think` `/tools` `/watch` `/format` | 不跑回合 | 命令返回时 |
| `/new` | 重置会话上下文（不跑回合） | 命令返回时 |
| `/stop` | 同步结束当前回合 | 被停止的**那条用户消息** settle 成 `cancelled`；`/stop` 自己结算它停掉的那次停（`ok` / `noop` / `failed`） |
| `/close` | 同步关闭会话（内部结束当前回合） | 被关闭的那条用户消息 settle 成 `cancelled`；`/close` 自己 settle 成 `unknown` |
| `/review` `/review --continue` | review goroutine + 注入回合 | 两者都结束 |

`/stop` 与 `/close` 结束的是**别的** inbound 消息的回合——那条消息拿到 `cancelled`，命令自己的消息独立结算。

---

## 6. 可见性

settled 信号**不渲染**。每个 IM channel 都静默丢弃 `OutMessageSettled`：它给程序看，不给人看。

人眼唯一能看到的地方是 daemon 日志里的一行：

```
chatsession: message settled chat_id=… message_id=… status=… summary=…
```

本机跑 daemon 时，这行的时间戳落在 agent 回合最后一个事件之后，就是"真的结束了"的证据。

---

## 7. 终局路径（不需要回合就 settle）

有些消息根本不会走到 agent 回合。它们各自直接结算：

| 情形 | Status | `Data["reason"]` |
| --- | --- | --- |
| 命令返回错误 | `failed` | — |
| 命令 panic | `failed` | `panic` |
| 队列拒绝这条消息 | `failed` | `queue_full` |
| 消息被丢弃（换 workspace 的边界清队列 / 队列重置） | `cancelled` | `dropped` |
| 消息没有 @ 到 bot（watch 模式丢弃） | `noop` | `watch_mode` |
| 没有 workspace / agent 起不来 | `failed` | `no_agent` |
| `/review` 没有可 review 的 diff | `noop` | `no_diff` |
| `/review` 的 agent 不支持 review | `failed` | `not_supported` |
| `/review` 的 findings 没能注入 agent | `incomplete` | `inject_failed` |

### 7.1 `reason` 词表

`reason` 是稳定的分类码，程序按它分支，不解析卡片文本。各命令用到的码：

| 码 | 含义 | 出现在 |
| --- | --- | --- |
| `usage` | 命令行不合法（未知 flag / 参数缺失 / 位置参数不对） | 全部 |
| `no_cwd` | 命令需要工作区，但没有 | 全部 |
| `wrong_cwd` | `/gtw back <worktree>` 要求当前在主仓库根目录 | `/gtw back` |
| `no_session` | 没有聊天会话 | `/stop` |
| `no_agent` | 没有选中的 agent | `/use` / `/review` / `/stop` |
| `unknown_agent` | agent 名没注册 | `/review` |
| `not_a_repo` | 工作区不在 git 仓库里 | `/gtw` |
| `no_remote` | 仓库没有 `origin` remote | `/gtw fix` |
| `provider_error` | git 平台解析 / 连接失败 | `/gtw fix` / `/gtw new` / `/gtw pr` |
| `invalid_issue` / `invalid_branch` | issue 号 / 分支名不合法 | `/gtw fix` |
| `issue_not_found` / `issue_claimed` | issue 不存在 / 已被别的 fix 占用 | `/gtw fix` |
| `branch_exists` | 分支已经存在 | `/gtw fix` |
| `already_active` | 当前目录已经在一个 `/gtw fix` 里 | `/gtw fix` |
| `label_error` | issue 标签没能加上 / 摘掉 | `/gtw fix` / `/gtw close` |
| `bad_state` | `.nightme/gtw.yml` 缺失 / 损坏 / 读不了 | `/gtw close` / `/gtw back` |
| `no_active_fix` | 这个聊天没有正在进行的 fix | `/gtw close` / `/gtw back` |
| `dirty` | 工作区有未提交的改动 | `/gtw push` / `/gtw close` |
| `detached` | HEAD 是游离的 | `/gtw push` / `/gtw commit` / `/gtw pr` |
| `conflict` | 有未解决的冲突 | `/gtw commit` |
| `rejected` | push 被拒绝 | `/gtw push` |
| `not_committed` | agent 跑完了但没有真的提交 | `/gtw commit` |
| `agent_error` | agent 没跑成 / 输出不可用 | `/gtw commit` / `/gtw pr` |
| `no_remote_branch` | 分支还没发布到 origin | `/gtw pr` |
| `no_commits` | 分支和 base 之间没有 commit | `/gtw pr` |
| `pr_exists` | 这个分支已经有 PR 了 | `/gtw pr` |
| `no_drafts` / `invalid_draft` | 没有草稿 / 草稿不可用 | `/gtw new -y` |
| `config_write_failed` | 全局 review agent 没能持久化 | `/review -g` |
| `no_review` / `already_running` | 没有可续的 review / 已有 review 在跑 | `/review -c` |
| `not_supported` | agent 不支持这个动作 | `/stop` |
| `aborted` | 被 `/gtw abort` 取消 | `/gtw` |
| `dispatch_failed` | worktree 建好了，但 issue 没送到 agent | `/gtw fix` |
| `sync_failed` | 主体动作完成，随后的 sync 失败 | `/gtw close` / `/gtw back` |
| `workspace_unreachable` | 工作区目录没了，只清了悬空状态 | `/gtw close` |
| `cwd_not_changed` / `cwd_not_restored` | 工作区切了但没切成 | `/gtw close` / `/gtw back` |
| `unknown_subcommand` | `/gtw` 的子命令不存在 | `/gtw` |
| `error` | 其它失败 | 全部 |

---

## 8. 边界

- settled 只在**工作真正结束**时发出。一轮很长的 agent 回合会把 settled 往后推——这是设计意图：settled 报的是结束，不是开始。
- `/steer` 的文本由下一个 prompt-end 或下一条入队消息冲刷队列，settled 跟着承载它的那个回合。
- 没有任何一方上报结论时，settled 是 `unknown`——它表示"没有结论"，不是"成功"。
- IM channel 丢弃 settled，所以从聊天窗口看不到它；要确认一条消息真的结束了，看 daemon 日志。
